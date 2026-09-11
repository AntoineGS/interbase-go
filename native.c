#include "native.h"

#include <ibase.h>
#include <inttypes.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define IB_STATUS_VECTOR_LENGTH 20
#define IB_MAX_SQLDA_BUFFER (1024U * 1024U)
#define IB_SQL_MESSAGE_LENGTH 256U

enum ib_argument_kind {
	IB_ARGUMENT_NULL = 0,
	IB_ARGUMENT_STRING = 1,
	IB_ARGUMENT_INT64 = 2,
	IB_ARGUMENT_FLOAT64 = 3,
	IB_ARGUMENT_BOOL = 4
};

typedef struct ib_bind_value {
	int kind;
	char *bytes;
	size_t length;
	int64_t int64_value;
	double float64_value;
	int bool_value;
} ib_bind_value;

struct ib_bindings {
	size_t count;
	ib_bind_value *values;
};

struct ib_connection {
	isc_db_handle database;
	ib_cursor *active_cursor;
	int broken;
};

struct ib_cursor {
	ib_connection *connection;
	isc_tr_handle transaction;
	isc_stmt_handle statement;
	XSQLDA *input;
	XSQLDA *output;
	int fetched;
};

static char *ib_copy_string(const char *value, size_t length)
{
	char *copy;

	if (length == SIZE_MAX) {
		return NULL;
	}
	copy = (char *) malloc(length + 1U);
	if (copy == NULL) {
		return NULL;
	}
	if (length != 0U && value != NULL) {
		memcpy(copy, value, length);
	}
	copy[length] = '\0';
	return copy;
}

static void ib_give_error(char **error, char *message)
{
	if (error != NULL) {
		*error = message;
	} else {
		free(message);
	}
}

static int ib_fail(char **error, const char *message)
{
	char *copy;

	copy = ib_copy_string(message, strlen(message));
	ib_give_error(error, copy);
	return -1;
}

static char *ib_join_errors(const char *first, const char *second)
{
	static const char fallback[] = "native operation and cleanup both failed";
	size_t first_length;
	size_t second_length;
	char *joined;

	first_length = strlen(first);
	second_length = strlen(second);
	if (second_length > SIZE_MAX - 3U ||
		first_length > SIZE_MAX - second_length - 3U) {
		return ib_copy_string(fallback, sizeof(fallback) - 1U);
	}
	joined = (char *) malloc(first_length + second_length + 3U);
	if (joined == NULL) {
		return NULL;
	}
	memcpy(joined, first, first_length);
	memcpy(joined + first_length, "; ", 2U);
	memcpy(joined + first_length + 2U, second, second_length);
	joined[first_length + second_length + 2U] = '\0';
	return joined;
}

static int ib_primary_status(const ISC_STATUS *status, ISC_STATUS *value)
{
	size_t index;
	ISC_STATUS argument;

	if (status == NULL || value == NULL) {
		return 0;
	}
	for (index = 0U; index < IB_STATUS_VECTOR_LENGTH;) {
		argument = status[index];
		if (argument == isc_arg_end) {
			return 0;
		}
		if (index + 1U >= IB_STATUS_VECTOR_LENGTH) {
			return 0;
		}
		if (argument == isc_arg_gds || argument == isc_arg_warning) {
			*value = status[index + 1U];
			return 1;
		}
		if (argument == isc_arg_cstring) {
			if (index + 2U >= IB_STATUS_VECTOR_LENGTH) {
				return 0;
			}
			index += 3U;
		} else {
			index += 2U;
		}
	}
	return 0;
}

static int ib_fail_status(char **error, const char *operation,
	ISC_STATUS *status)
{
	char sql_message[IB_SQL_MESSAGE_LENGTH];
	char message[2560];
	ISC_STATUS primary_status;
	ISC_LONG sqlcode;
	int has_primary_status;

	has_primary_status = ib_primary_status(status, &primary_status);
	sqlcode = 0;
	sql_message[0] = '\0';
	if (has_primary_status) {
		sqlcode = isc_sqlcode(status);
		if (sqlcode >= (ISC_LONG) SHRT_MIN &&
			sqlcode <= (ISC_LONG) SHRT_MAX) {
			isc_sql_interprete((short) sqlcode, sql_message,
				(short) sizeof(sql_message));
			sql_message[sizeof(sql_message) - 1U] = '\0';
		}
	}
	if (has_primary_status && sql_message[0] != '\0') {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE %" PRIdMAX ", native status %" PRIdPTR "): %s",
			operation, (intmax_t) sqlcode, (intptr_t) primary_status,
			sql_message);
	} else if (has_primary_status) {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE %" PRIdMAX ", native status %" PRIdPTR ")",
			operation, (intmax_t) sqlcode, (intptr_t) primary_status);
	} else {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE 0, native status unavailable)", operation);
	}
	return ib_fail(error, message);
}

static void ib_free_sqlda(XSQLDA *sqlda)
{
	short index;

	if (sqlda == NULL) {
		return;
	}
	for (index = 0; index < sqlda->sqln; index++) {
		free(sqlda->sqlvar[index].sqldata);
		free(sqlda->sqlvar[index].sqlind);
		sqlda->sqlvar[index].sqldata = NULL;
		sqlda->sqlvar[index].sqlind = NULL;
	}
	free(sqlda);
}

static XSQLDA *ib_alloc_sqlda(short count)
{
	XSQLDA *sqlda;
	size_t size;

	if (count < 1) {
		count = 1;
	}
	size = sizeof(XSQLDA) + ((size_t) count - 1U) * sizeof(XSQLVAR);
	if (size > (size_t) INT_MAX) {
		return NULL;
	}
	sqlda = (XSQLDA *) calloc(1U, size);
	if (sqlda == NULL) {
		return NULL;
	}
	sqlda->version = SQLDA_VERSION2;
	sqlda->sqldabc = (ISC_LONG) size;
	sqlda->sqln = count;
	sqlda->sqld = 0;
	return sqlda;
}

static int ib_sql_type(const XSQLVAR *variable)
{
	return ((int) variable->sqltype) & ~1;
}

static int ib_validate_length(short length, size_t extra, size_t *size,
	char **error)
{
	size_t value;

	if (length < 0) {
		return ib_fail(error, "InterBase returned an invalid SQLDA length");
	}
	value = (size_t) length;
	if (value > IB_MAX_SQLDA_BUFFER || extra > IB_MAX_SQLDA_BUFFER - value) {
		return ib_fail(error, "InterBase returned an unsupported SQLDA length");
	}
	*size = value + extra;
	if (*size == 0U) {
		*size = 1U;
	}
	return 0;
}

static int ib_value_buffer_size(const XSQLVAR *variable, size_t *size,
	char **error)
{
	switch (ib_sql_type(variable)) {
	case SQL_TEXT:
		return ib_validate_length(variable->sqllen, 0U, size, error);
	case SQL_VARYING:
		return ib_validate_length(variable->sqllen, sizeof(unsigned short), size,
			error);
	case SQL_SHORT:
		*size = sizeof(short);
		return 0;
	case SQL_LONG:
		*size = sizeof(ISC_LONG);
		return 0;
	case SQL_INT64:
		*size = sizeof(ISC_INT64);
		return 0;
	case SQL_FLOAT:
		*size = sizeof(float);
		return 0;
	case SQL_DOUBLE:
		*size = sizeof(double);
		return 0;
	case SQL_TIMESTAMP:
		*size = sizeof(ISC_TIMESTAMP);
		return 0;
	case SQL_TYPE_DATE:
		*size = sizeof(ISC_DATE);
		return 0;
	case SQL_TYPE_TIME:
		*size = sizeof(ISC_TIME);
		return 0;
	case SQL_BOOLEAN:
		*size = sizeof(ISC_BOOLEAN);
		return 0;
	default:
		return ib_fail(error, "the parameter type is unsupported by the proof of concept");
	}
}

static int ib_allocate_variable(XSQLVAR *variable, size_t size, char **error)
{
	variable->sqldata = (char *) calloc(1U, size == 0U ? 1U : size);
	variable->sqlind = (short *) malloc(sizeof(short));
	if (variable->sqldata == NULL || variable->sqlind == NULL) {
		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		return ib_fail(error, "out of memory allocating SQLDA value storage");
	}
	*variable->sqlind = 0;
	return 0;
}

static int ib_describe_bind(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	XSQLDA *sqlda;
	short count;

	sqlda = ib_alloc_sqlda(1);
	if (sqlda == NULL) {
		return ib_fail(error, "out of memory allocating input SQLDA");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_describe_bind(status, &cursor->statement,
		SQL_DIALECT_V5, sqlda);
	if (result != 0) {
		ib_free_sqlda(sqlda);
		return ib_fail_status(error, "describe bind", status);
	}
	if (sqlda->sqld < 0) {
		ib_free_sqlda(sqlda);
		return ib_fail(error, "InterBase returned an invalid input SQLDA count");
	}
	if (sqlda->sqld > sqlda->sqln) {
		count = sqlda->sqld;
		ib_free_sqlda(sqlda);
		sqlda = ib_alloc_sqlda(count);
		if (sqlda == NULL) {
			return ib_fail(error, "out of memory resizing input SQLDA");
		}
		memset(status, 0, sizeof(status));
		result = isc_dsql_describe_bind(status, &cursor->statement,
			SQL_DIALECT_V5, sqlda);
		if (result != 0) {
			ib_free_sqlda(sqlda);
			return ib_fail_status(error, "describe bind", status);
		}
		if (sqlda->sqld < 0 || sqlda->sqld > sqlda->sqln) {
			ib_free_sqlda(sqlda);
			return ib_fail(error, "InterBase returned an invalid input SQLDA count");
		}
	}
	cursor->input = sqlda;
	return 0;
}

static int ib_describe_output(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	XSQLDA *sqlda;
	short count;

	sqlda = ib_alloc_sqlda(1);
	if (sqlda == NULL) {
		return ib_fail(error, "out of memory allocating output SQLDA");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_describe(status, &cursor->statement, SQL_DIALECT_V5, sqlda);
	if (result != 0) {
		ib_free_sqlda(sqlda);
		return ib_fail_status(error, "describe output", status);
	}
	if (sqlda->sqld < 0) {
		ib_free_sqlda(sqlda);
		return ib_fail(error, "InterBase returned an invalid output SQLDA count");
	}
	if (sqlda->sqld > sqlda->sqln) {
		count = sqlda->sqld;
		ib_free_sqlda(sqlda);
		sqlda = ib_alloc_sqlda(count);
		if (sqlda == NULL) {
			return ib_fail(error, "out of memory resizing output SQLDA");
		}
		memset(status, 0, sizeof(status));
		result = isc_dsql_describe(status, &cursor->statement, SQL_DIALECT_V5, sqlda);
		if (result != 0) {
			ib_free_sqlda(sqlda);
			return ib_fail_status(error, "describe output", status);
		}
		if (sqlda->sqld < 0 || sqlda->sqld > sqlda->sqln) {
			ib_free_sqlda(sqlda);
			return ib_fail(error, "InterBase returned an invalid output SQLDA count");
		}
	}
	cursor->output = sqlda;
	return 0;
}

static int ib_statement_is_select(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char request[1];
	char response[64];
	ISC_LONG response_length;
	ISC_LONG statement_type;

	request[0] = (char) isc_info_sql_stmt_type;
	memset(response, 0, sizeof(response));
	memset(status, 0, sizeof(status));
	result = isc_dsql_sql_info(status, &cursor->statement, (short) sizeof(request),
		request, (short) sizeof(response), response);
	if (result != 0) {
		return ib_fail_status(error, "inspect statement type", status);
	}
	if ((unsigned char) response[0] != isc_info_sql_stmt_type) {
		return ib_fail(error, "InterBase did not return a statement type");
	}
	response_length = isc_vax_integer(response + 1, 2);
	if (response_length <= 0 || response_length > (ISC_LONG) sizeof(response) - 3) {
		return ib_fail(error, "InterBase returned an invalid statement type");
	}
	statement_type = isc_vax_integer(response + 3, (short) response_length);
	if (statement_type != isc_info_sql_stmt_select) {
		return ib_fail(error, "only SELECT statements are permitted");
	}
	return 0;
}

static int ib_set_varying(XSQLVAR *variable, const char *value, size_t length,
	short nullable, char **error)
{
	unsigned short varying_length;
	size_t size;

	if (length > (size_t) SHRT_MAX) {
		return ib_fail(error, "string argument exceeds the SQLDA length limit");
	}
	variable->sqltype = (short) (SQL_VARYING | nullable);
	variable->sqlscale = 0;
	variable->sqlsubtype = 0;
	variable->sqlprecision = 0;
	variable->sqllen = (short) length;
	if (ib_validate_length(variable->sqllen, sizeof(unsigned short), &size, error) != 0 ||
		ib_allocate_variable(variable, size, error) != 0) {
		return -1;
	}
	varying_length = (unsigned short) length;
	memcpy(variable->sqldata, &varying_length, sizeof(varying_length));
	if (length != 0U) {
		memcpy(variable->sqldata + sizeof(varying_length), value, length);
	}
	return 0;
}

static int ib_bind_input(ib_cursor *cursor, const ib_bindings *bindings,
	char **error)
{
	short index;

	if (bindings == NULL || cursor->input == NULL ||
		(bindings->count != 0U && bindings->values == NULL)) {
		return ib_fail(error, "missing input SQLDA");
	}
	if ((size_t) cursor->input->sqld != bindings->count) {
		return ib_fail(error, "query argument count does not match positional parameters");
	}
	for (index = 0; index < cursor->input->sqld; index++) {
		XSQLVAR *variable = &cursor->input->sqlvar[index];
		const ib_bind_value *value = &bindings->values[index];
		short nullable = 1;
		size_t size;
		int described_type = ib_sql_type(variable);

		if (described_type == SQL_BLOB || described_type == SQL_ARRAY ||
			described_type == SQL_QUAD) {
			return ib_fail(error, "array, blob, and quad parameters are unsupported");
		}

		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		if (value->kind == IB_ARGUMENT_NULL) {
			variable->sqltype = (short) (variable->sqltype | 1);
			if (ib_value_buffer_size(variable, &size, error) != 0 ||
				ib_allocate_variable(variable, size, error) != 0) {
				return -1;
			}
			*variable->sqlind = -1;
			continue;
		}

		switch (value->kind) {
		case IB_ARGUMENT_STRING:
			if (ib_set_varying(variable, value->bytes, value->length, nullable, error) != 0) {
				return -1;
			}
			break;
		case IB_ARGUMENT_INT64:
			variable->sqltype = (short) (SQL_INT64 | nullable);
			variable->sqlscale = 0;
			variable->sqlsubtype = 0;
			variable->sqlprecision = 0;
			variable->sqllen = (short) sizeof(ISC_INT64);
			if (ib_allocate_variable(variable, sizeof(ISC_INT64), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &value->int64_value, sizeof(value->int64_value));
			break;
		case IB_ARGUMENT_FLOAT64:
			variable->sqltype = (short) (SQL_DOUBLE | nullable);
			variable->sqlscale = 0;
			variable->sqlsubtype = 0;
			variable->sqlprecision = 0;
			variable->sqllen = (short) sizeof(double);
			if (ib_allocate_variable(variable, sizeof(double), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &value->float64_value, sizeof(value->float64_value));
			break;
		case IB_ARGUMENT_BOOL:
		{
			ISC_BOOLEAN boolean_value = value->bool_value ? ISC_TRUE : ISC_FALSE;
			variable->sqltype = (short) (SQL_BOOLEAN | nullable);
			variable->sqlscale = 0;
			variable->sqlsubtype = 0;
			variable->sqlprecision = 0;
			variable->sqllen = (short) sizeof(boolean_value);
			if (ib_allocate_variable(variable, sizeof(boolean_value), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &boolean_value, sizeof(boolean_value));
			break;
		}
		default:
			return ib_fail(error, "the query argument kind is unsupported");
		}
	}
	return 0;
}

static int ib_validate_output_types(const XSQLDA *sqlda, char **error)
{
	short index;
	int type;

	if (sqlda == NULL) {
		return ib_fail(error, "missing output SQLDA");
	}
	for (index = 0; index < sqlda->sqld; index++) {
		type = ib_sql_type(&sqlda->sqlvar[index]);
		switch (type) {
		case SQL_TEXT:
		case SQL_VARYING:
		case SQL_SHORT:
		case SQL_LONG:
		case SQL_INT64:
		case SQL_TIMESTAMP:
		case SQL_TYPE_DATE:
		case SQL_TYPE_TIME:
		case SQL_BOOLEAN:
			break;
		case SQL_FLOAT:
		case SQL_DOUBLE:
			if (sqlda->sqlvar[index].sqlscale != 0) {
				return ib_fail(error, "scaled floating-point results are unsupported");
			}
			break;
		default:
			return ib_fail(error, "the result contains a type unsupported by the proof of concept");
		}
	}
	return 0;
}

static int ib_allocate_output(ib_cursor *cursor, char **error)
{
	short index;
	size_t size;

	for (index = 0; index < cursor->output->sqld; index++) {
		cursor->output->sqlvar[index].sqltype =
			(short) (cursor->output->sqlvar[index].sqltype | 1);
		if (ib_value_buffer_size(&cursor->output->sqlvar[index], &size, error) != 0 ||
			ib_allocate_variable(&cursor->output->sqlvar[index], size, error) != 0) {
			return -1;
		}
	}
	return 0;
}

static void ib_cursor_free_parts(ib_cursor *cursor)
{
	if (cursor == NULL) {
		return;
	}
	ib_free_sqlda(cursor->input);
	ib_free_sqlda(cursor->output);
	cursor->input = NULL;
	cursor->output = NULL;
}

static int ib_cursor_close_internal(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	int failed;

	if (cursor == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	if (cursor->statement != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &cursor->statement, DSQL_drop);
		cursor->statement = NULL;
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error, "close statement", status);
		}
	}
	if (cursor->transaction != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_rollback_transaction(status, &cursor->transaction);
		cursor->transaction = NULL;
		if (result != 0) {
			failed = 1;
			if (first_error == NULL) {
				(void) ib_fail_status(&first_error, "rollback transaction", status);
			}
		}
	}
	if (cursor->connection != NULL && cursor->connection->active_cursor == cursor) {
		cursor->connection->active_cursor = NULL;
	}
	if (failed && cursor->connection != NULL) {
		cursor->connection->broken = 1;
	}
	ib_cursor_free_parts(cursor);
	free(cursor);
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

static void ib_failed_query_cleanup(ib_cursor *cursor, char **error)
{
	char *cleanup_error;
	ib_connection *connection;

	if (cursor == NULL) {
		return;
	}
	connection = cursor->connection;
	cleanup_error = NULL;
	if (ib_cursor_close_internal(cursor, &cleanup_error) != 0) {
		if (connection != NULL) {
			connection->broken = 1;
		}
	}
	if (cleanup_error != NULL) {
		if (error == NULL || *error == NULL) {
			ib_give_error(error, cleanup_error);
		} else {
			char *joined = ib_join_errors(*error, cleanup_error);
			if (joined != NULL) {
				free(*error);
				free(cleanup_error);
				*error = joined;
			} else {
				free(*error);
				*error = cleanup_error;
			}
		}
	}
}

ib_connection *ib_connection_open(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, char **error)
{
	unsigned char *dpb;
	size_t dpb_length;
	size_t offset;
	ib_connection *connection;
	isc_db_handle database_handle;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	const char charset[] = "UTF8";
	unsigned char length;

	if (database == NULL || database_length == 0U || database_length > (size_t) SHRT_MAX) {
		ib_fail(error, "database name is empty or too long");
		return NULL;
	}
	if (user == NULL || user_length == 0U || user_length > (size_t) UCHAR_MAX ||
		password == NULL || password_length > (size_t) UCHAR_MAX) {
		ib_fail(error, "connection credential length is invalid");
		return NULL;
	}
	dpb_length = 1U + 2U + user_length + 2U + password_length +
		2U + sizeof(charset) - 1U + 2U + 1U;
	if (dpb_length > (size_t) SHRT_MAX) {
		ib_fail(error, "connection parameter block is too long");
		return NULL;
	}
	dpb = (unsigned char *) malloc(dpb_length);
	if (dpb == NULL) {
		ib_fail(error, "out of memory allocating connection parameters");
		return NULL;
	}
	offset = 0U;
	dpb[offset++] = isc_dpb_version1;
	dpb[offset++] = isc_dpb_user_name;
	length = (unsigned char) user_length;
	dpb[offset++] = length;
	memcpy(dpb + offset, user, user_length);
	offset += user_length;
	dpb[offset++] = isc_dpb_password;
	length = (unsigned char) password_length;
	dpb[offset++] = length;
	if (password_length != 0U) {
		memcpy(dpb + offset, password, password_length);
		offset += password_length;
	}
	dpb[offset++] = isc_dpb_lc_ctype;
	dpb[offset++] = (unsigned char) (sizeof(charset) - 1U);
	memcpy(dpb + offset, charset, sizeof(charset) - 1U);
	offset += sizeof(charset) - 1U;
	dpb[offset++] = isc_dpb_sql_dialect;
	dpb[offset++] = 1U;
	dpb[offset++] = SQL_DIALECT_V5;

	connection = (ib_connection *) calloc(1U, sizeof(*connection));
	if (connection == NULL) {
		free(dpb);
		ib_fail(error, "out of memory allocating connection state");
		return NULL;
	}
	database_handle = NULL;
	memset(status, 0, sizeof(status));
	result = isc_attach_database(status, (short) database_length,
		(char *) database, &database_handle, (short) offset, (char *) dpb);
	free(dpb);
	if (result != 0) {
		if (database_handle != NULL) {
			ISC_STATUS detach_status[IB_STATUS_VECTOR_LENGTH];
			memset(detach_status, 0, sizeof(detach_status));
			(void) isc_detach_database(detach_status, &database_handle);
		}
		ib_fail_status(error, "attach database", status);
		free(connection);
		return NULL;
	}
	connection->database = database_handle;
	return connection;
}

int ib_connection_close(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;
	int failed;

	if (connection == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	if (connection->active_cursor != NULL) {
		cursor_error = NULL;
		if (ib_cursor_close_internal(connection->active_cursor, &cursor_error) != 0) {
			failed = 1;
		}
		if (cursor_error != NULL) {
			first_error = cursor_error;
		}
	}
	if (connection->database != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_detach_database(status, &connection->database);
		connection->database = NULL;
		if (result != 0) {
			failed = 1;
			if (first_error == NULL) {
				(void) ib_fail_status(&first_error, "detach database", status);
			}
		}
	}
	free(connection);
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

int ib_connection_is_broken(const ib_connection *connection)
{
	return connection == NULL || connection->broken;
}

ib_bindings *ib_bindings_new(size_t count, char **error)
{
	ib_bindings *bindings;

	if (count > (size_t) SHRT_MAX) {
		ib_fail(error, "too many query arguments");
		return NULL;
	}
	bindings = (ib_bindings *) calloc(1U, sizeof(*bindings));
	if (bindings == NULL) {
		ib_fail(error, "out of memory allocating query arguments");
		return NULL;
	}
	bindings->count = count;
	if (count != 0U) {
		if (count > SIZE_MAX / sizeof(*bindings->values)) {
			free(bindings);
			ib_fail(error, "too many query arguments");
			return NULL;
		}
		bindings->values = (ib_bind_value *) calloc(count, sizeof(*bindings->values));
		if (bindings->values == NULL) {
			free(bindings);
			ib_fail(error, "out of memory allocating query arguments");
			return NULL;
		}
	}
	return bindings;
}

void ib_bindings_free(ib_bindings *bindings)
{
	size_t index;

	if (bindings == NULL) {
		return;
	}
	for (index = 0U; index < bindings->count; index++) {
		free(bindings->values[index].bytes);
	}
	free(bindings->values);
	free(bindings);
}

static int ib_binding_index(ib_bindings *bindings, size_t index, char **error)
{
	if (bindings == NULL || index >= bindings->count) {
		return ib_fail(error, "query argument index is out of bounds");
	}
	return 0;
}

int ib_bindings_set_null(ib_bindings *bindings, size_t index, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_NULL;
	return 0;
}

int ib_bindings_set_string(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error)
{
	char *copy;

	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	if (length != 0U && value == NULL) {
		return ib_fail(error, "string query argument is missing data");
	}
	copy = ib_copy_string(value, length);
	if (copy == NULL) {
		return ib_fail(error, "out of memory copying string query argument");
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_STRING;
	bindings->values[index].bytes = copy;
	bindings->values[index].length = length;
	return 0;
}

int ib_bindings_set_int64(ib_bindings *bindings, size_t index,
	int64_t value, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_INT64;
	bindings->values[index].int64_value = value;
	return 0;
}

int ib_bindings_set_float64(ib_bindings *bindings, size_t index,
	double value, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_FLOAT64;
	bindings->values[index].float64_value = value;
	return 0;
}

int ib_bindings_set_bool(ib_bindings *bindings, size_t index, int value,
	char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_BOOL;
	bindings->values[index].bool_value = value != 0;
	return 0;
}

ib_cursor *ib_connection_query(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	const char tpb[] = {
		isc_tpb_version3,
		isc_tpb_read_committed,
		isc_tpb_rec_version,
		isc_tpb_wait,
		isc_tpb_read
	};

	if (connection == NULL || connection->database == NULL || connection->broken) {
		ib_fail(error, "connection is unavailable");
		return NULL;
	}
	if (query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		ib_fail(error, "query is empty or too long");
		return NULL;
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		ib_fail(error, "query arguments are invalid");
		return NULL;
	}
	if (connection->active_cursor != NULL) {
		ib_fail(error, "another result cursor is active on this connection");
		return NULL;
	}

	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		ib_fail(error, "out of memory allocating cursor state");
		return NULL;
	}
	cursor->connection = connection;
	memset(status, 0, sizeof(status));
	result = isc_start_transaction(status, &cursor->transaction, 1,
		&connection->database, (short) sizeof(tpb), (char *) tpb);
	if (result != 0) {
		/* There may be no handle to clean up, so cleanup cannot detect a dead
		 * attachment. Conservatively retire it on every startup failure. */
		connection->broken = 1;
		ib_fail_status(error, "start read-only transaction", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &connection->database,
		&cursor->statement);
	if (result != 0) {
		ib_fail_status(error, "allocate statement", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &cursor->transaction, &cursor->statement,
		(unsigned short) query_length, (char *) query, SQL_DIALECT_V5, NULL);
	if (result != 0) {
		ib_fail_status(error, "prepare statement", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (ib_statement_is_select(cursor, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (ib_describe_bind(cursor, error) != 0 ||
		ib_bind_input(cursor, bindings, error) != 0 ||
		ib_describe_output(cursor, error) != 0 ||
		ib_validate_output_types(cursor->output, error) != 0 ||
		ib_allocate_output(cursor, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute(status, &cursor->transaction, &cursor->statement,
			SQL_DIALECT_V5, input);
	}
	if (result != 0) {
		ib_fail_status(error, "execute SELECT", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	connection->active_cursor = cursor;
	return cursor;
}

int ib_cursor_next(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || cursor->statement == NULL || cursor->output == NULL) {
		return ib_fail(error, "cursor is unavailable");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_fetch(status, &cursor->statement, SQL_DIALECT_V5,
		cursor->output);
	if (result == 100) {
		return 0;
	}
	if (result != 0) {
		return ib_fail_status(error, "fetch row", status);
	}
	cursor->fetched = 1;
	return 1;
}

int ib_cursor_close(ib_cursor *cursor, char **error)
{
	return ib_cursor_close_internal(cursor, error);
}

size_t ib_cursor_column_count(const ib_cursor *cursor)
{
	if (cursor == NULL || cursor->output == NULL || cursor->output->sqld < 0) {
		return 0U;
	}
	return (size_t) cursor->output->sqld;
}

const char *ib_cursor_column_name(const ib_cursor *cursor, size_t index,
	size_t *length)
{
	const XSQLVAR *variable;
	short name_length;

	if (length != NULL) {
		*length = 0U;
	}
	if (cursor == NULL || cursor->output == NULL || index >= (size_t) cursor->output->sqld) {
		return NULL;
	}
	variable = &cursor->output->sqlvar[index];
	if (variable->aliasname_length > 0) {
		name_length = variable->aliasname_length;
		if (name_length > METADATALENGTH) {
			name_length = METADATALENGTH;
		}
		if (length != NULL) {
			*length = (size_t) name_length;
		}
		return variable->aliasname;
	}
	name_length = variable->sqlname_length;
	if (name_length < 0 || name_length > METADATALENGTH) {
		return NULL;
	}
	if (length != NULL) {
		*length = (size_t) name_length;
	}
	return variable->sqlname;
}

static int ib_fill_timestamp(ib_value_view *view, int type, const char *data,
	char **error)
{
	struct tm decoded;
	ISC_TIMESTAMP timestamp;
	ISC_DATE date;
	ISC_TIME time_value;
	unsigned long ticks;

	memset(&decoded, 0, sizeof(decoded));
	if (type == SQL_TIMESTAMP) {
		memcpy(&timestamp, data, sizeof(timestamp));
		isc_decode_timestamp(&timestamp, &decoded);
		ticks = (unsigned long) timestamp.timestamp_time;
	} else if (type == SQL_TYPE_DATE) {
		memcpy(&date, data, sizeof(date));
		isc_decode_sql_date(&date, &decoded);
		ticks = 0UL;
		decoded.tm_hour = 0;
		decoded.tm_min = 0;
		decoded.tm_sec = 0;
	} else if (type == SQL_TYPE_TIME) {
		memcpy(&time_value, data, sizeof(time_value));
		isc_decode_sql_time(&time_value, &decoded);
		ticks = (unsigned long) time_value;
		decoded.tm_year = 0;
		decoded.tm_mon = 0;
		decoded.tm_mday = 1;
	} else {
		return ib_fail(error, "timestamp type is unsupported");
	}
	if (decoded.tm_year < -1900 || decoded.tm_mon < 0 || decoded.tm_mon > 11 ||
		decoded.tm_mday < 1 || decoded.tm_mday > 31 || decoded.tm_hour < 0 ||
		decoded.tm_hour > 23 || decoded.tm_min < 0 || decoded.tm_min > 59 ||
		decoded.tm_sec < 0 || decoded.tm_sec > 59 ||
		ticks >= 24UL * 60UL * 60UL * (unsigned long) ISC_TIME_SECONDS_PRECISION) {
		return ib_fail(error, "InterBase returned an invalid timestamp");
	}
	view->kind = IB_VALUE_TIMESTAMP;
	view->year = decoded.tm_year + 1900;
	view->month = decoded.tm_mon + 1;
	view->day = decoded.tm_mday;
	view->hour = decoded.tm_hour;
	view->minute = decoded.tm_min;
	view->second = decoded.tm_sec;
	view->nanosecond = (int) ((ticks % ISC_TIME_SECONDS_PRECISION) * 100000UL);
	return 0;
}

int ib_cursor_column(const ib_cursor *cursor, size_t index,
	ib_value_view *view, char **error)
{
	const XSQLVAR *variable;
	int type;
	short short_value;
	ISC_LONG long_value;
	ISC_INT64 int64_value;
	float float_value;
	double double_value;
	ISC_BOOLEAN boolean_value;
	unsigned short varying_length;

	if (cursor == NULL || cursor->output == NULL || !cursor->fetched ||
		index >= (size_t) cursor->output->sqld || view == NULL) {
		return ib_fail(error, "result column is unavailable");
	}
	memset(view, 0, sizeof(*view));
	variable = &cursor->output->sqlvar[index];
	if (variable->sqlind == NULL || variable->sqldata == NULL) {
		return ib_fail(error, "result column storage is unavailable");
	}
	if (*variable->sqlind < 0) {
		view->kind = IB_VALUE_NULL;
		return 0;
	}
	type = ib_sql_type(variable);
	switch (type) {
	case SQL_TEXT:
		view->kind = IB_VALUE_STRING;
		view->bytes = variable->sqldata;
		view->length = (size_t) variable->sqllen;
		return 0;
	case SQL_VARYING:
		memcpy(&varying_length, variable->sqldata, sizeof(varying_length));
		if (variable->sqllen < 0 || varying_length > (unsigned short) variable->sqllen) {
			return ib_fail(error, "InterBase returned an invalid varying value length");
		}
		view->kind = IB_VALUE_STRING;
		view->bytes = variable->sqldata + sizeof(varying_length);
		view->length = (size_t) varying_length;
		return 0;
	case SQL_SHORT:
		memcpy(&short_value, variable->sqldata, sizeof(short_value));
		view->int64_value = (int64_t) short_value;
		if (variable->sqlscale != 0) {
			view->kind = IB_VALUE_SCALED_INT;
			view->scale = variable->sqlscale;
		} else {
			view->kind = IB_VALUE_INT64;
		}
		return 0;
	case SQL_LONG:
		memcpy(&long_value, variable->sqldata, sizeof(long_value));
		view->int64_value = (int64_t) long_value;
		if (variable->sqlscale != 0) {
			view->kind = IB_VALUE_SCALED_INT;
			view->scale = variable->sqlscale;
		} else {
			view->kind = IB_VALUE_INT64;
		}
		return 0;
	case SQL_INT64:
		memcpy(&int64_value, variable->sqldata, sizeof(int64_value));
		view->int64_value = (int64_t) int64_value;
		if (variable->sqlscale != 0) {
			view->kind = IB_VALUE_SCALED_INT;
			view->scale = variable->sqlscale;
		} else {
			view->kind = IB_VALUE_INT64;
		}
		return 0;
	case SQL_FLOAT:
		memcpy(&float_value, variable->sqldata, sizeof(float_value));
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = (double) float_value;
		return 0;
	case SQL_DOUBLE:
		memcpy(&double_value, variable->sqldata, sizeof(double_value));
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = double_value;
		return 0;
	case SQL_TIMESTAMP:
	case SQL_TYPE_DATE:
	case SQL_TYPE_TIME:
		return ib_fill_timestamp(view, type, variable->sqldata, error);
	case SQL_BOOLEAN:
		memcpy(&boolean_value, variable->sqldata, sizeof(boolean_value));
		view->kind = IB_VALUE_BOOL;
		view->bool_value = boolean_value != 0;
		return 0;
	default:
		return ib_fail(error, "the result type is unsupported by the proof of concept");
	}
}

void ib_error_free(char *error)
{
	free(error);
}
