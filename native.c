#include "native.h"

#include <ibase.h>
#include <ctype.h>
#include <errno.h>
#include <inttypes.h>
#include <iconv.h>
#include <limits.h>
#include <math.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define IB_STATUS_VECTOR_LENGTH 20
#define IB_MAX_SQLDA_BUFFER (1024U * 1024U)
#define IB_SQL_MESSAGE_LENGTH 256U
#define IB_BLOB_SEGMENT_LENGTH 32767U
#define IB_BLOB_BPB_LENGTH 64U
#define IB_MAX_BLOB_BUFFER (64U * 1024U * 1024U)
#define IB_CHARSET_UTF8 59

enum ib_argument_kind {
	IB_ARGUMENT_NULL = 0,
	IB_ARGUMENT_STRING = 1,
	IB_ARGUMENT_INT64 = 2,
	IB_ARGUMENT_FLOAT64 = 3,
	IB_ARGUMENT_BOOL = 4,
	IB_ARGUMENT_TIMESTAMP = 5,
	IB_ARGUMENT_BYTES = 6
};

typedef struct ib_bind_value {
	int kind;
	char *bytes;
	size_t length;
	int64_t int64_value;
	double float64_value;
	int bool_value;
	int64_t year;
	int month;
	int day;
	int hour;
	int minute;
	int second;
	int nanosecond;
} ib_bind_value;

struct ib_bindings {
	size_t count;
	ib_bind_value *values;
};

struct ib_connection {
	isc_db_handle database;
	ib_cursor *active_cursor;
	ib_statement *statements;
	isc_tr_handle transaction;
	short charset;
	int dialect;
	int broken;
};

struct ib_statement {
	ib_connection *connection;
	isc_stmt_handle statement;
	XSQLDA *input;
	XSQLDA *output;
	int statement_type;
	ib_statement *next;
};

struct ib_cursor {
	ib_connection *connection;
	ib_statement *prepared;
	isc_tr_handle transaction;
	isc_stmt_handle statement;
	XSQLDA *input;
	XSQLDA *output;
	ib_column_metadata *metadata;
	char *converted_value;
	char *materialized_value;
	int owns_transaction;
	int server_cursor_open;
	int statement_type;
	int procedure;
	int output_pending;
	int fetched;
};

static unsigned short ib_connection_dialect(const ib_connection *connection)
{
	if (connection != NULL && connection->dialect == SQL_DIALECT_V5) {
		return SQL_DIALECT_V5;
	}
	return SQL_DIALECT_V6;
}

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

static int ib_fail(char **error, const char *message);

static const char *ib_charset_encoding(short charset)
{
	switch (charset) {
	case 2:
		return "ASCII";
	case 3:
		return "UTF-8";
	case 5:
		return "SHIFT-JIS";
	case 6:
		return "EUC-JP";
	case 8:
		return "UTF-16BE";
	case 10:
		return "CP437";
	case 11:
		return "CP850";
	case 12:
		return "CP865";
	case 13:
		return "CP860";
	case 14:
		return "CP863";
	case 21:
		return "ISO-8859-1";
	case 22:
		return "ISO-8859-2";
	case 39:
		return "ISO-8859-15";
	case 44:
		return "EUC-KR";
	case 45:
		return "CP852";
	case 46:
		return "CP857";
	case 47:
		return "CP861";
	case 51:
		return "CP1250";
	case 52:
		return "CP1251";
	case 53:
		return "CP1252";
	case 54:
		return "CP1253";
	case 55:
		return "CP1254";
	case 56:
		return "BIG5";
	case 57:
		return "GB2312";
	case 58:
		return "KOI8-R";
	case 59:
		return "UTF-8";
	case 64:
		return "UTF-16LE";
	default:
		return NULL;
	}
}

static int ib_charset_name_matches(const char *value, size_t length,
	const char *expected)
{
	size_t index;
	size_t expected_length;

	if (value == NULL || expected == NULL) {
		return 0;
	}
	expected_length = strlen(expected);
	if (length != expected_length) {
		return 0;
	}
	for (index = 0U; index < length; index++) {
		if (toupper((unsigned char) value[index]) !=
			toupper((unsigned char) expected[index])) {
			return 0;
		}
	}
	return 1;
}

static int ib_charset_id(const char *value, size_t length, short *charset)
{
	if (charset == NULL) {
		return -1;
	}
	if (length == 0U || ib_charset_name_matches(value, length, "UTF8")) {
		*charset = IB_CHARSET_UTF8;
		return 0;
	}
	if (ib_charset_name_matches(value, length, "WIN1250")) {
		*charset = 51;
		return 0;
	}
	return -1;
}

static const char *ib_charset_name(short charset)
{
	switch (charset) {
	case IB_CHARSET_UTF8:
		return "UTF8";
	case 51:
		return "WIN1250";
	default:
		return NULL;
	}
}

static char *ib_convert_charset(const char *value, size_t length,
	short source_charset, short target_charset, size_t *converted_length,
	char **error)
{
	const char *source_encoding;
	const char *target_encoding;
	iconv_t converter;
	char *converted;
	char *input;
	char *output;
	size_t input_length;
	size_t output_length;
	size_t capacity;
	size_t result;

	if (converted_length == NULL) {
		return NULL;
	}
	*converted_length = 0U;
	if (length == 0U || source_charset == target_charset ||
		source_charset == 0 || source_charset == 1 || target_charset == 0 ||
		target_charset == 1) {
		converted = ib_copy_string(value, length);
		if (converted == NULL) {
			(void) ib_fail(error, "out of memory copying string query argument");
			return NULL;
		}
		*converted_length = length;
		return converted;
	}
	source_encoding = ib_charset_encoding(source_charset);
	target_encoding = ib_charset_encoding(target_charset);
	if (source_encoding == NULL || target_encoding == NULL) {
		(void) ib_fail(error, "the character set conversion is unsupported");
		return NULL;
	}
	converter = iconv_open(target_encoding, source_encoding);
	if (converter == (iconv_t) -1) {
		(void) ib_fail(error, "the character set converter is unavailable");
		return NULL;
	}
	if (length > (SIZE_MAX - 1U) / 4U) {
		iconv_close(converter);
		(void) ib_fail(error, "the value is too large to convert");
		return NULL;
	}
	capacity = length * 4U + 1U;
	converted = (char *) malloc(capacity);
	if (converted == NULL) {
		iconv_close(converter);
		(void) ib_fail(error, "out of memory converting character data");
		return NULL;
	}
	input = (char *) value;
	input_length = length;
	output = converted;
	output_length = capacity - 1U;
	result = iconv(converter, &input, &input_length, &output, &output_length);
	if (result == (size_t) -1) {
		free(converted);
		iconv_close(converter);
		if (errno == EILSEQ || errno == EINVAL) {
			(void) ib_fail(error, "character data is not representable in the configured character set");
		} else {
			(void) ib_fail(error, "character data could not be converted");
		}
		return NULL;
	}
	*converted_length = (capacity - 1U) - output_length;
	*output = '\0';
	iconv_close(converter);
	return converted;
}

static char *ib_convert_utf8(const char *value, size_t length, short charset,
	size_t *converted_length, char **error)
{
	return ib_convert_charset(value, length, IB_CHARSET_UTF8, charset,
		converted_length, error);
}

static char *ib_convert_to_utf8(const char *value, size_t length, short charset,
	size_t *converted_length, char **error)
{
	return ib_convert_charset(value, length, charset, IB_CHARSET_UTF8,
		converted_length, error);
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

static void ib_append_error(char **first_error, char *next_error)
{
	char *joined;

	if (next_error == NULL) {
		return;
	}
	if (first_error == NULL) {
		free(next_error);
		return;
	}
	if (*first_error == NULL) {
		*first_error = next_error;
		return;
	}
	joined = ib_join_errors(*first_error, next_error);
	if (joined != NULL) {
		free(*first_error);
		free(next_error);
		*first_error = joined;
	} else {
		free(next_error);
	}
}

static int ib_start_transaction(ib_connection *connection,
	isc_tr_handle *transaction, int read_only, const char *operation,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	const char tpb[] = {
		isc_tpb_version3,
		isc_tpb_read_committed,
		isc_tpb_rec_version,
		isc_tpb_wait,
		read_only ? isc_tpb_read : isc_tpb_write
	};

	if (connection == NULL || connection->database == NULL ||
		connection->broken || transaction == NULL || *transaction != NULL) {
		return ib_fail(error, "connection or transaction is unavailable");
	}
	memset(status, 0, sizeof(status));
	result = isc_start_transaction(status, transaction, 1,
		&connection->database, (short) sizeof(tpb), (char *) tpb);
	if (result != 0) {
		connection->broken = 1;
		if (*transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];
			char *rollback_error = NULL;

			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed transaction",
					rollback_status);
			}
			*transaction = NULL;
			if (rollback_error != NULL) {
				char *primary_error = NULL;
				(void) ib_fail_status(&primary_error, operation, status);
				ib_append_error(&primary_error, rollback_error);
				ib_give_error(error, primary_error);
				return -1;
			}
		}
		return ib_fail_status(error, operation, status);
	}
	return 0;
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

static XSQLDA *ib_clone_sqlda(const XSQLDA *source, char **error)
{
	XSQLDA *clone;
	short index;

	if (source == NULL || source->sqln < 0 || source->sqld < 0 ||
		source->sqld > source->sqln) {
		(void) ib_fail(error, "the prepared SQLDA is invalid");
		return NULL;
	}
	clone = ib_alloc_sqlda(source->sqln);
	if (clone == NULL) {
		(void) ib_fail(error, "out of memory copying prepared SQLDA");
		return NULL;
	}
	clone->sqldabc = source->sqldabc;
	clone->sqld = source->sqld;
	for (index = 0; index < source->sqln; index++) {
		clone->sqlvar[index] = source->sqlvar[index];
		clone->sqlvar[index].sqldata = NULL;
		clone->sqlvar[index].sqlind = NULL;
	}
	return clone;
}

static int ib_sql_type(const XSQLVAR *variable)
{
	return ((int) variable->sqltype) & ~1;
}

static int ib_metadata_type_code(int type)
{
	switch (type) {
	case SQL_TEXT:
		return IB_METADATA_CHAR;
	case SQL_VARYING:
		return IB_METADATA_VARCHAR;
	case SQL_SHORT:
		return IB_METADATA_SMALLINT;
	case SQL_LONG:
		return IB_METADATA_INTEGER;
	case SQL_INT64:
		return IB_METADATA_BIGINT;
	case SQL_FLOAT:
		return IB_METADATA_FLOAT;
	case SQL_DOUBLE:
	case SQL_D_FLOAT:
		return IB_METADATA_DOUBLE;
	case SQL_TIMESTAMP:
		return IB_METADATA_TIMESTAMP;
	case SQL_TYPE_DATE:
		return IB_METADATA_DATE;
	case SQL_TYPE_TIME:
		return IB_METADATA_TIME;
	case SQL_BOOLEAN:
		return IB_METADATA_BOOLEAN;
	case SQL_BLOB:
		return IB_METADATA_BLOB;
	default:
		return IB_METADATA_UNKNOWN;
	}
}

static int ib_catalog_sql_type(int type)
{
	switch (type) {
	case 7:
		return SQL_SHORT;
	case 8:
		return SQL_LONG;
	case 10:
		return SQL_FLOAT;
	case 12:
		return SQL_TYPE_DATE;
	case 13:
		return SQL_TYPE_TIME;
	case 14:
		return SQL_TEXT;
	case 16:
		return SQL_INT64;
	case 17:
	case 23:
		return SQL_BOOLEAN;
	case 27:
		return SQL_DOUBLE;
	case 35:
		return SQL_TIMESTAMP;
	case 37:
		return SQL_VARYING;
	case 261:
		return SQL_BLOB;
	default:
		return 0;
	}
}

static short ib_text_charset(const XSQLVAR *variable)
{
	if (variable == NULL) {
		return 0;
	}
	/* The low byte is the charset; the high byte may carry collation. */
	return (short) ((unsigned short) variable->sqlsubtype & 0x00ffU);
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
	case SQL_BLOB:
		*size = sizeof(ISC_QUAD);
		return 0;
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
	case SQL_D_FLOAT:
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
		ib_connection_dialect(cursor->connection), sqlda);
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
			ib_connection_dialect(cursor->connection), sqlda);
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
	result = isc_dsql_describe(status, &cursor->statement,
		ib_connection_dialect(cursor->connection), sqlda);
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
		result = isc_dsql_describe(status, &cursor->statement,
			ib_connection_dialect(cursor->connection), sqlda);
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

static int ib_statement_type(ib_cursor *cursor, int *statement_type,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char request[1];
	char response[64];
	ISC_LONG response_length;
	ISC_LONG server_statement_type;

	if (cursor == NULL || statement_type == NULL || cursor->statement == NULL) {
		return ib_fail(error, "statement is unavailable");
	}

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
	server_statement_type = isc_vax_integer(response + 3,
		(short) response_length);
	*statement_type = (int) server_statement_type;
	cursor->statement_type = *statement_type;
	return 0;
}

static int ib_statement_is_select(ib_cursor *cursor, char **error)
{
	int statement_type;

	if (ib_statement_type(cursor, &statement_type, error) != 0) {
		return -1;
	}
	if (statement_type != isc_info_sql_stmt_select) {
		return ib_fail(error, "only SELECT statements are permitted");
	}
	return 0;
}

static int ib_statement_rejects_transaction_control_type(int statement_type,
	char **error)
{
	switch (statement_type) {
	case isc_info_sql_stmt_start_trans:
	case isc_info_sql_stmt_commit:
	case isc_info_sql_stmt_rollback:
		return ib_fail(error,
			"transaction-control SQL must use database/sql transaction methods");
	default:
		return 0;
	}
}

static int ib_statement_rejects_result(ib_cursor *cursor, char **error)
{
	int statement_type;

	if (ib_statement_type(cursor, &statement_type, error) != 0) {
		return -1;
	}
	if (statement_type == isc_info_sql_stmt_select ||
		statement_type == isc_info_sql_stmt_select_for_upd) {
		return ib_fail(error, "statement produces a result set; use Query instead");
	}
	if (statement_type == isc_info_sql_stmt_exec_procedure &&
		(cursor->output == NULL || cursor->output->sqld != 0)) {
		return ib_fail(error, "statement produces a result set; use Query instead");
	}
	return ib_statement_rejects_transaction_control_type(statement_type, error);
}

static int ib_statement_rows_affected(ib_cursor *cursor,
	int64_t *rows_affected, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	const char request[] = { isc_info_sql_records, isc_info_end };
	char response[64];
	ISC_LONG response_length;
	size_t offset;
	size_t end;
	int requested_type;

	if (cursor == NULL || rows_affected == NULL || cursor->statement == NULL) {
		return ib_fail(error, "statement result is unavailable");
	}
	*rows_affected = -1;
	switch (cursor->statement_type) {
	case isc_info_sql_stmt_insert:
		requested_type = isc_info_req_insert_count;
		break;
	case isc_info_sql_stmt_update:
		requested_type = isc_info_req_update_count;
		break;
	case isc_info_sql_stmt_delete:
		requested_type = isc_info_req_delete_count;
		break;
	default:
		return 0;
	}

	memset(response, 0, sizeof(response));
	memset(status, 0, sizeof(status));
	result = isc_dsql_sql_info(status, &cursor->statement,
		(short) sizeof(request), (char *) request, (short) sizeof(response), response);
	if (result != 0) {
		return ib_fail_status(error, "inspect affected rows", status);
	}
	if ((unsigned char) response[0] == isc_info_truncated) {
		return ib_fail(error, "InterBase returned a truncated affected-row count");
	}
	if ((unsigned char) response[0] != isc_info_sql_records) {
		return ib_fail(error, "InterBase did not return affected-row information");
	}
	response_length = isc_vax_integer(response + 1, 2);
	if (response_length < 1 || response_length > (ISC_LONG) sizeof(response) - 3) {
		return ib_fail(error, "InterBase returned an invalid affected-row response");
	}
	offset = 3U;
	end = offset + (size_t) response_length;
	while (offset < end) {
		unsigned char item;
		ISC_LONG item_length;
		ISC_LONG count;

		item = (unsigned char) response[offset++];
		if (item == isc_info_end) {
			break;
		}
		if (end - offset < 2U) {
			return ib_fail(error, "InterBase returned a malformed affected-row response");
		}
		item_length = isc_vax_integer(response + offset, 2);
		offset += 2U;
		if (item_length < 0 || (size_t) item_length > end - offset ||
			item_length > (ISC_LONG) sizeof(ISC_LONG)) {
			return ib_fail(error, "InterBase returned an invalid affected-row item");
		}
		if ((int) item == requested_type) {
			count = isc_vax_integer(response + offset, (short) item_length);
			if (count < 0) {
				return ib_fail(error, "InterBase returned a negative affected-row count");
			}
			*rows_affected = (int64_t) count;
		}
		offset += (size_t) item_length;
	}
	return 0;
}

static int ib_cursor_prepare_statement(ib_cursor *cursor, const char *query,
	size_t query_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		return ib_fail(error, "statement preparation is unavailable");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &cursor->connection->database,
		&cursor->statement);
	if (result != 0) {
		return ib_fail_status(error, "allocate statement", status);
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &cursor->transaction, &cursor->statement,
		(unsigned short) query_length, (char *) query,
		ib_connection_dialect(cursor->connection), NULL);
	if (result != 0) {
		return ib_fail_status(error, "prepare statement", status);
	}
	return 0;
}

static int ib_cursor_drop_statement(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	int failed;

	if (cursor == NULL) {
		return 0;
	}
	failed = 0;
	if (cursor->statement != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &cursor->statement, DSQL_drop);
		if (result == 0) {
			cursor->statement = NULL;
		} else {
			failed = 1;
			if (cursor->connection != NULL) {
				cursor->connection->broken = 1;
			}
			(void) ib_fail_status(error, "close statement", status);
		}
	}
	ib_free_sqlda(cursor->input);
	ib_free_sqlda(cursor->output);
	cursor->input = NULL;
	cursor->output = NULL;
	free(cursor->metadata);
	cursor->metadata = NULL;
	return failed ? -1 : 0;
}

static int ib_cursor_rollback_owned_transaction(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || !cursor->owns_transaction || cursor->transaction == NULL) {
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_rollback_transaction(status, &cursor->transaction);
	cursor->transaction = NULL;
	cursor->owns_transaction = 0;
	if (result != 0) {
		if (cursor->connection != NULL) {
			cursor->connection->broken = 1;
		}
		return ib_fail_status(error, "rollback transaction", status);
	}
	return 0;
}

static int ib_set_varying(XSQLVAR *variable, const char *value, size_t length,
	short connection_charset, int binary, short nullable, char **error)
{
	int described_type;
	int preserve_described;
	short target_charset;
	short described_length;
	int result_type;
	char *converted;
	size_t converted_length;
	size_t extra;
	size_t size;

	described_type = ib_sql_type(variable);
	described_length = variable->sqllen;
	preserve_described = (described_length > 0 &&
		(described_type == SQL_TEXT || described_type == SQL_VARYING));
	target_charset = binary ? 1 :
		(preserve_described ? ib_text_charset(variable) : connection_charset);
	converted = ib_convert_utf8(value, length, target_charset,
		&converted_length, error);
	if (converted == NULL) {
		return -1;
	}
	if (described_length < 0 || converted_length > (size_t) SHRT_MAX) {
		free(converted);
		return ib_fail(error, "string argument exceeds the described SQLDA length");
	}
	if (preserve_described) {
		extra = described_type == SQL_VARYING ? sizeof(unsigned short) : 0U;
		if (converted_length > (size_t) described_length) {
			free(converted);
			return ib_fail(error, "string argument exceeds the described SQLDA length");
		}
		if ((size_t) described_length > IB_MAX_SQLDA_BUFFER - extra) {
			free(converted);
			return ib_fail(error, "string argument exceeds the SQLDA storage limit");
		}
		size = (size_t) described_length + extra;
		if (size == 0U) {
			size = 1U;
		}
		variable->sqltype = (short) (described_type | nullable);
		variable->sqllen = described_length;
	} else {
		if (converted_length > IB_MAX_SQLDA_BUFFER) {
			free(converted);
			return ib_fail(error, "string argument exceeds the SQLDA storage limit");
		}
		result_type = described_type == SQL_VARYING ? SQL_VARYING : SQL_TEXT;
		extra = result_type == SQL_VARYING ? sizeof(unsigned short) : 0U;
		if (converted_length > IB_MAX_SQLDA_BUFFER - extra) {
			free(converted);
			return ib_fail(error, "string argument exceeds the SQLDA storage limit");
		}
		size = converted_length + extra;
		if (size == 0U) {
			size = 1U;
		}
		variable->sqltype = (short) (result_type | nullable);
		variable->sqlsubtype = binary ? 1 : connection_charset;
		variable->sqllen = (short) converted_length;
	}
	if (ib_allocate_variable(variable, size, error) != 0) {
		free(converted);
		return -1;
	}
	if (preserve_described && described_type == SQL_TEXT) {
		memset(variable->sqldata, ' ', size);
	}
	if (ib_sql_type(variable) == SQL_VARYING) {
		unsigned short varying_length = (unsigned short) converted_length;
		memcpy(variable->sqldata, &varying_length, sizeof(varying_length));
		if (converted_length != 0U) {
			memcpy(variable->sqldata + sizeof(varying_length), converted,
				converted_length);
		}
	} else if (converted_length != 0U) {
		memcpy(variable->sqldata, converted, converted_length);
	}
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	free(converted);
	return 0;
}

static int ib_is_scaled_integer(const XSQLVAR *variable)
{
	int type;

	if (variable == NULL) {
		return 0;
	}
	type = ib_sql_type(variable);
	return (type == SQL_SHORT || type == SQL_LONG || type == SQL_INT64) &&
		(variable->sqlscale != 0 || variable->sqlsubtype == 1 ||
			variable->sqlsubtype == 2 || variable->sqlprecision > 0);
}

static uint64_t ib_scaled_integer_limit(const XSQLVAR *variable, int negative)
{
	uint64_t limit;
	uint64_t power;
	int precision;
	int index;

	limit = negative ? (uint64_t) INT64_MAX + UINT64_C(1) :
		(uint64_t) INT64_MAX;
	precision = variable == NULL ? 0 : (int) variable->sqlprecision;
	if (precision <= 0) {
		return limit;
	}
	power = UINT64_C(1);
	for (index = 0; index < precision; index++) {
		if (power > limit / UINT64_C(10)) {
			return limit;
		}
		power *= UINT64_C(10);
	}
	if (power == 0U || power - UINT64_C(1) >= limit) {
		return limit;
	}
	return power - UINT64_C(1);
}

static int ib_parse_scaled_integer(const char *value, size_t length,
	const XSQLVAR *variable, int64_t *result, char **error)
{
	size_t offset;
	size_t start;
	size_t digit_count;
	size_t fractional_digits;
	size_t discarded_digits;
	size_t effective_digits;
	size_t padding;
	size_t digit_index;
	int negative;
	int decimal_point;
	int seen_digit;
	int scale;
	int target_fraction;
	uint64_t magnitude;
	uint64_t limit;

	if (value == NULL || length == 0U || variable == NULL || result == NULL) {
		return ib_fail(error, "decimal string is unavailable");
	}
	offset = 0U;
	negative = 0;
	if (value[offset] == '-' || value[offset] == '+') {
		negative = value[offset] == '-';
		offset++;
	}
	start = offset;
	digit_count = 0U;
	fractional_digits = 0U;
	decimal_point = 0;
	seen_digit = 0;
	for (; offset < length; offset++) {
		unsigned char character = (unsigned char) value[offset];

		if (character >= (unsigned char) '0' && character <= (unsigned char) '9') {
			seen_digit = 1;
			if (digit_count == SIZE_MAX) {
				return ib_fail(error, "decimal string is too long");
			}
			digit_count++;
			if (decimal_point) {
				if (fractional_digits == SIZE_MAX) {
					return ib_fail(error, "decimal string is too long");
				}
				fractional_digits++;
			}
		} else if (character == (unsigned char) '.' && !decimal_point) {
			decimal_point = 1;
		} else {
			return ib_fail(error,
				"decimal string contains unsupported syntax; use ordinary decimal notation");
		}
	}
	if (!seen_digit) {
		return ib_fail(error, "decimal string must contain a digit");
	}

	scale = (int) variable->sqlscale;
	target_fraction = scale < 0 ? -scale : 0;
	discarded_digits = 0U;
	if (fractional_digits > (size_t) target_fraction) {
		discarded_digits = fractional_digits - (size_t) target_fraction;
		for (offset = length; discarded_digits > 0U; offset--, discarded_digits--) {
			if (value[offset - 1U] != '0') {
				return ib_fail(error,
					"decimal string has a nonzero fraction beyond the described scale");
			}
		}
		discarded_digits = fractional_digits - (size_t) target_fraction;
	}
	effective_digits = digit_count - discarded_digits;
	padding = (size_t) target_fraction > fractional_digits ?
		(size_t) target_fraction - fractional_digits : 0U;
	if (scale > 0) {
		if (padding > SIZE_MAX - (size_t) scale) {
			return ib_fail(error, "decimal string is too large for the described scale");
		}
		padding += (size_t) scale;
	}
	limit = ib_scaled_integer_limit(variable, negative);
	magnitude = 0U;
	digit_index = 0U;
	for (offset = start; offset < length; offset++) {
		unsigned char character = (unsigned char) value[offset];
		uint64_t digit;

		if (character < (unsigned char) '0' || character > (unsigned char) '9') {
			continue;
		}
		if (digit_index < effective_digits) {
			digit = (uint64_t) (character - (unsigned char) '0');
			if (magnitude > (limit - digit) / UINT64_C(10)) {
				return ib_fail(error, "decimal string is outside the supported range");
			}
			magnitude = magnitude * UINT64_C(10) + digit;
		}
		digit_index++;
	}
	for (offset = 0U; offset < padding; offset++) {
		if (magnitude > limit / UINT64_C(10)) {
			return ib_fail(error, "decimal string is outside the supported range");
		}
		magnitude *= UINT64_C(10);
	}
	if (negative) {
		if (magnitude == (uint64_t) INT64_MAX + UINT64_C(1)) {
			*result = INT64_MIN;
		} else {
			*result = -(int64_t) magnitude;
		}
	} else {
		*result = (int64_t) magnitude;
	}
	return 0;
}

static int ib_set_scaled_integer(XSQLVAR *variable, int64_t value,
	short nullable, char **error)
{
	int type;
	short short_value = 0;
	ISC_LONG long_value = 0;
	ISC_INT64 int64_value = 0;
	size_t size;

	if (variable == NULL) {
		return ib_fail(error, "decimal parameter is unavailable");
	}
	type = ib_sql_type(variable);
	switch (type) {
	case SQL_SHORT:
		if (value < (int64_t) SHRT_MIN || value > (int64_t) SHRT_MAX) {
			return ib_fail(error, "decimal string is outside the parameter range");
		}
		short_value = (short) value;
		size = sizeof(short_value);
		break;
	case SQL_LONG:
		if (value < (int64_t) INT32_MIN || value > (int64_t) INT32_MAX) {
			return ib_fail(error, "decimal string is outside the parameter range");
		}
		long_value = (ISC_LONG) value;
		size = sizeof(long_value);
		break;
	case SQL_INT64:
		int64_value = (ISC_INT64) value;
		size = sizeof(int64_value);
		break;
	default:
		return ib_fail(error, "decimal parameter has an unsupported storage type");
	}
	variable->sqltype = (short) (type | nullable);
	variable->sqllen = (short) size;
	if (ib_allocate_variable(variable, size, error) != 0) {
		return -1;
	}
	switch (type) {
	case SQL_SHORT:
		memcpy(variable->sqldata, &short_value, sizeof(short_value));
		break;
	case SQL_LONG:
		memcpy(variable->sqldata, &long_value, sizeof(long_value));
		break;
	case SQL_INT64:
		memcpy(variable->sqldata, &int64_value, sizeof(int64_value));
		break;
	default:
		break;
	}
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	return 0;
}

static int ib_set_temporal(XSQLVAR *variable, const ib_bind_value *value,
	short nullable, char **error)
{
	int type;
	struct tm encoded;
	ISC_TIMESTAMP timestamp;
	ISC_DATE date;
	ISC_TIME time_value;
	void *data;
	size_t size;

	if (variable == NULL || value == NULL) {
		return ib_fail(error, "temporal parameter is unavailable");
	}
	type = ib_sql_type(variable);
	if (type == SQL_TYPE_DATE || type == SQL_TIMESTAMP) {
		if (value->year < INT64_C(1) || value->year > INT64_C(9999)) {
			return ib_fail(error, "temporal parameter year is outside the supported range");
		}
	}
	memset(&encoded, 0, sizeof(encoded));
	if (type == SQL_TYPE_TIME) {
		/* SQL TIME has no date component; use a stable valid date for the encoder. */
		encoded.tm_year = 0;
		encoded.tm_mon = 0;
		encoded.tm_mday = 1;
	} else {
		/* The range check above makes this conversion safe for struct tm. */
		encoded.tm_year = (int) (value->year - INT64_C(1900));
		encoded.tm_mon = value->month - 1;
		encoded.tm_mday = value->day;
	}
	encoded.tm_hour = value->hour;
	encoded.tm_min = value->minute;
	encoded.tm_sec = value->second;
	data = NULL;
	size = 0U;
	switch (type) {
	case SQL_TYPE_DATE:
		isc_encode_sql_date(&encoded, &date);
		data = &date;
		size = sizeof(date);
		break;
	case SQL_TYPE_TIME:
		isc_encode_sql_time(&encoded, &time_value);
		time_value += (ISC_TIME) (value->nanosecond / 100000);
		if (time_value >= (ISC_TIME) (24U * 60U * 60U * ISC_TIME_SECONDS_PRECISION)) {
			return ib_fail(error, "temporal parameter is outside the TIME range");
		}
		data = &time_value;
		size = sizeof(time_value);
		break;
	case SQL_TIMESTAMP:
		isc_encode_timestamp(&encoded, &timestamp);
		timestamp.timestamp_time += (ISC_TIME) (value->nanosecond / 100000);
		if (timestamp.timestamp_time >=
			(ISC_TIME) (24U * 60U * 60U * ISC_TIME_SECONDS_PRECISION)) {
			return ib_fail(error, "temporal parameter is outside the TIMESTAMP range");
		}
		data = &timestamp;
		size = sizeof(timestamp);
		break;
	default:
		return ib_fail(error, "temporal parameter has an unsupported storage type");
	}
	variable->sqltype = (short) (type | nullable);
	variable->sqllen = (short) size;
	if (ib_allocate_variable(variable, size, error) != 0) {
		return -1;
	}
	memcpy(variable->sqldata, data, size);
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	return 0;
}

static int ib_blob_cleanup(isc_blob_handle *blob, int cancel, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	int failed;

	/* A native cleanup failure is actionable even when its message cannot be allocated. */
	if (blob == NULL || *blob == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	memset(status, 0, sizeof(status));
	if (cancel) {
		result = isc_cancel_blob(status, blob);
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error, "cancel blob", status);
			memset(status, 0, sizeof(status));
			result = isc_close_blob(status, blob);
			if (result != 0) {
				char *close_error = NULL;

				failed = 1;
				(void) ib_fail_status(&close_error, "close blob", status);
				ib_append_error(&first_error, close_error);
			}
		}
	} else {
		result = isc_close_blob(status, blob);
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error, "close blob", status);
		}
	}
	*blob = NULL;
	ib_give_error(error, first_error);
	return failed != 0 ? -1 : 0;
}

static void ib_blob_descriptor(ISC_BLOB_DESC_V2 *descriptor, short subtype,
	short charset)
{
	memset(descriptor, 0, sizeof(*descriptor));
	descriptor->blob_desc_version = BLB_DESC_VERSION2;
	descriptor->blob_desc_subtype = subtype;
	descriptor->blob_desc_charset = charset;
	descriptor->blob_desc_segment_size = (short) IB_BLOB_SEGMENT_LENGTH;
}

static int ib_blob_generate_bpb(short source_charset, short target_charset,
	unsigned char *bpb, unsigned short *bpb_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_BLOB_DESC_V2 source;
	ISC_BLOB_DESC_V2 target;
	ISC_STATUS result;

	if (bpb == NULL || bpb_length == NULL) {
		return ib_fail(error, "BLOB parameter block storage is unavailable");
	}
	ib_blob_descriptor(&source, 1, source_charset);
	ib_blob_descriptor(&target, 1, target_charset);
	memset(status, 0, sizeof(status));
	/* isc_blob_gen_bpb2 takes the target descriptor before the source. */
	result = isc_blob_gen_bpb2(status, &target, &source, IB_BLOB_BPB_LENGTH, bpb,
		bpb_length);
	if (result != 0) {
		return ib_fail_status(error, "generate BLOB parameter block", status);
	}
	return 0;
}

static int ib_blob_lookup_descriptor(ib_cursor *cursor,
	const XSQLVAR *variable, ISC_BLOB_DESC_V2 *descriptor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	unsigned char field_name[METADATALENGTH + 1U];
	unsigned char relation_name[METADATALENGTH + 1U];

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		variable == NULL || descriptor == NULL || variable->sqlname_length <= 0 ||
		variable->relname_length <= 0) {
		return ib_fail(error, "BLOB column metadata is unavailable");
	}
	if ((size_t) variable->sqlname_length > METADATALENGTH ||
		(size_t) variable->relname_length > METADATALENGTH) {
		return ib_fail(error, "BLOB column metadata is too long");
	}
	memset(field_name, 0, sizeof(field_name));
	memset(relation_name, 0, sizeof(relation_name));
	memcpy(field_name, variable->sqlname, (size_t) variable->sqlname_length);
	memcpy(relation_name, variable->relname,
		(size_t) variable->relname_length);
	ib_blob_descriptor(descriptor, 1, 0);
	memset(status, 0, sizeof(status));
	result = isc_blob_lookup_desc2(status, &cursor->connection->database,
		&cursor->transaction, relation_name, field_name, descriptor, NULL);
	if (result != 0) {
		return ib_fail_status(error, "lookup BLOB column descriptor", status);
	}
	return 0;
}

static int ib_set_blob(ib_cursor *cursor, XSQLVAR *variable,
	const char *value, size_t length, short connection_charset, int text,
	short nullable, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	isc_blob_handle blob;
	ISC_QUAD blob_id;
	char *converted;
	char *first_error;
	const char *data;
	size_t data_length;
	size_t offset;
	unsigned char bpb[IB_BLOB_BPB_LENGTH];
	unsigned short bpb_length;
	int cleanup_failed;

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL) {
		return ib_fail(error, "BLOB parameter transaction is unavailable");
	}
	bpb_length = 0U;
	converted = NULL;
	data = value;
	data_length = length;
	if (text && connection_charset != 0 && connection_charset != 1 &&
		connection_charset != IB_CHARSET_UTF8) {
		converted = ib_convert_utf8(value, length, connection_charset,
			&data_length, error);
		if (converted == NULL) {
			return -1;
		}
		data = converted;
	}
	variable->sqltype = (short) (SQL_BLOB | nullable);
	variable->sqllen = (short) sizeof(ISC_QUAD);
	if (ib_allocate_variable(variable, sizeof(ISC_QUAD), error) != 0) {
		free(converted);
		return -1;
	}
	blob = NULL;
	memset(&blob_id, 0, sizeof(blob_id));
	memset(status, 0, sizeof(status));
	if (text && connection_charset != 0 && connection_charset != 1) {
		if (ib_blob_generate_bpb(connection_charset, connection_charset, bpb,
			&bpb_length, error) != 0) {
			free(variable->sqldata);
			free(variable->sqlind);
			variable->sqldata = NULL;
			variable->sqlind = NULL;
			free(converted);
			return -1;
		}
		result = isc_create_blob2(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id, (short) bpb_length,
			(char *) bpb);
	} else {
		result = isc_create_blob(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id);
	}
	if (result != 0) {
		first_error = NULL;
		(void) ib_fail_status(&first_error, "create input BLOB", status);
		{
			char *cleanup_error = NULL;

			cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
			if (cleanup_failed != 0) {
				cursor->connection->broken = 1;
			}
			ib_append_error(&first_error, cleanup_error);
		}
		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		free(converted);
		ib_give_error(error, first_error);
		return -1;
	}
	offset = 0U;
	while (offset < data_length) {
		size_t remaining = data_length - offset;
		size_t segment_size = remaining > IB_BLOB_SEGMENT_LENGTH
			? IB_BLOB_SEGMENT_LENGTH : remaining;
		unsigned short segment_length = (unsigned short) segment_size;
		memset(status, 0, sizeof(status));
		result = isc_put_segment(status, &blob, segment_length,
			(char *) data + offset);
		if (result != 0) {
			first_error = NULL;
			(void) ib_fail_status(&first_error, "write input BLOB segment", status);
			{
				char *cleanup_error = NULL;
				cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
				if (cleanup_failed != 0) {
					cursor->connection->broken = 1;
				}
				ib_append_error(&first_error, cleanup_error);
			}
			free(variable->sqldata);
			free(variable->sqlind);
			variable->sqldata = NULL;
			variable->sqlind = NULL;
			free(converted);
			ib_give_error(error, first_error);
			return -1;
		}
		offset += segment_size;
	}
	first_error = NULL;
	cleanup_failed = ib_blob_cleanup(&blob, 0, &first_error);
	if (cleanup_failed != 0) {
		cursor->connection->broken = 1;
	}
	if (cleanup_failed != 0 || first_error != NULL) {
		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		free(converted);
		ib_give_error(error, first_error);
		return -1;
	}
	memcpy(variable->sqldata, &blob_id, sizeof(blob_id));
	free(converted);
	return 0;
}

static int ib_bind_input(ib_cursor *cursor, const ib_bindings *bindings,
	char **error)
{
	short index;
	short connection_charset;

	if (bindings == NULL || cursor->input == NULL ||
		(bindings->count != 0U && bindings->values == NULL)) {
		return ib_fail(error, "missing input SQLDA");
	}
	connection_charset = IB_CHARSET_UTF8;
	if (cursor->connection != NULL && cursor->connection->charset != 0) {
		connection_charset = cursor->connection->charset;
	}
	if ((size_t) cursor->input->sqld != bindings->count) {
		return ib_fail(error, "query argument count does not match positional parameters");
	}
	for (index = 0; index < cursor->input->sqld; index++) {
		XSQLVAR *variable = &cursor->input->sqlvar[index];
		const ib_bind_value *value = &bindings->values[index];
		short nullable;
		size_t size;
		int described_type = ib_sql_type(variable);
		nullable = (short) (variable->sqltype & 1);

		if (described_type == SQL_ARRAY || described_type == SQL_QUAD) {
			return ib_fail(error, "array and quad parameters are unsupported");
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
		if (described_type == SQL_BLOB) {
			switch (value->kind) {
			case IB_ARGUMENT_STRING:
				if (ib_set_blob(cursor, variable, value->bytes, value->length,
					connection_charset, variable->sqlsubtype == 1, nullable, error) != 0) {
					return -1;
				}
				break;
			case IB_ARGUMENT_BYTES:
				if (ib_set_blob(cursor, variable, value->bytes, value->length,
					connection_charset, 0, nullable, error) != 0) {
					return -1;
				}
				break;
			default:
				return ib_fail(error, "only string and byte values can bind to a BLOB");
			}
			continue;
		}

		switch (value->kind) {
		case IB_ARGUMENT_STRING:
			if (ib_connection_dialect(cursor->connection) == SQL_DIALECT_V6 &&
				ib_is_scaled_integer(variable)) {
				int64_t scaled_value = 0;

				if (ib_parse_scaled_integer(value->bytes, value->length, variable,
					&scaled_value, error) != 0) {
					return -1;
				}
				if (ib_set_scaled_integer(variable, scaled_value, nullable, error) != 0) {
					return -1;
				}
			} else if (ib_set_varying(variable, value->bytes, value->length,
				connection_charset, 0, nullable, error) != 0) {
				return -1;
			}
			break;
		case IB_ARGUMENT_BYTES:
			if ((described_type != SQL_TEXT && described_type != SQL_VARYING) ||
				ib_text_charset(variable) != 1) {
				return ib_fail(error, "byte values require OCTETS parameters");
			}
			if (ib_set_varying(variable, value->bytes, value->length,
				connection_charset, 1, nullable, error) != 0) {
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
			if (!isfinite(value->float64_value)) {
				return ib_fail(error, "floating-point argument must be finite");
			}
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
		case IB_ARGUMENT_TIMESTAMP:
			if (described_type != SQL_TYPE_DATE && described_type != SQL_TYPE_TIME &&
				described_type != SQL_TIMESTAMP) {
				described_type = SQL_TIMESTAMP;
				variable->sqltype = (short) (described_type | nullable);
				variable->sqlscale = 0;
				variable->sqlsubtype = 0;
				variable->sqlprecision = 0;
			}
			if (ib_set_temporal(variable, value, nullable, error) != 0) {
				return -1;
			}
			break;
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
		case SQL_BLOB:
			break;
		case SQL_FLOAT:
		case SQL_DOUBLE:
		case SQL_D_FLOAT:
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
		if ((cursor->output->sqlvar[index].sqltype & 1) == 0) {
			free(cursor->output->sqlvar[index].sqlind);
			cursor->output->sqlvar[index].sqlind = NULL;
		}
		if (ib_sql_type(&cursor->output->sqlvar[index]) == SQL_TEXT) {
			memset(cursor->output->sqlvar[index].sqldata, ' ', size);
		}
	}
	return 0;
}

static void ib_cursor_free_parts(ib_cursor *cursor)
{
	if (cursor == NULL) {
		return;
	}
	free(cursor->converted_value);
	cursor->converted_value = NULL;
	free(cursor->materialized_value);
	cursor->materialized_value = NULL;
	free(cursor->metadata);
	cursor->metadata = NULL;
	ib_free_sqlda(cursor->input);
	ib_free_sqlda(cursor->output);
	cursor->input = NULL;
	cursor->output = NULL;
}

static int ib_cursor_close_internal(ib_cursor *cursor, char **error, int success)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	int failed;
	int commit_transaction;
	unsigned short free_option;

	if (cursor == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	commit_transaction = 0;
	if (cursor->statement != NULL &&
		(cursor->prepared == NULL || cursor->server_cursor_open)) {
		free_option = cursor->prepared != NULL && cursor->server_cursor_open
			? DSQL_close : DSQL_drop;
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &cursor->statement,
			free_option);
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error,
				free_option == DSQL_close ? "close cursor" : "close statement", status);
		}
	}
	cursor->statement = NULL;
	cursor->server_cursor_open = 0;
	if (cursor->owns_transaction && cursor->transaction != NULL) {
		memset(status, 0, sizeof(status));
		commit_transaction = success && !failed && cursor->procedure;
		if (commit_transaction) {
			result = isc_commit_transaction(status, &cursor->transaction);
			if (result != 0) {
				ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];
				char *rollback_error = NULL;

				failed = 1;
				(void) ib_fail_status(&first_error,
					"commit procedure transaction", status);
				if (cursor->transaction != NULL) {
					memset(rollback_status, 0, sizeof(rollback_status));
					if (isc_rollback_transaction(rollback_status,
						&cursor->transaction) != 0) {
						(void) ib_fail_status(&rollback_error,
							"rollback failed procedure transaction", rollback_status);
					}
					ib_append_error(&first_error, rollback_error);
				}
			}
		} else {
			result = isc_rollback_transaction(status, &cursor->transaction);
			if (result != 0) {
				failed = 1;
				if (first_error == NULL) {
					(void) ib_fail_status(&first_error, "rollback transaction", status);
				}
			}
		}
		cursor->transaction = NULL;
	}
	cursor->transaction = NULL;
	cursor->owns_transaction = 0;
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

static int ib_descriptor_name(const char *value, short value_length,
	char *buffer, size_t capacity, size_t *length, char **error)
{
	size_t name_length;

	if (value == NULL || buffer == NULL || length == NULL || capacity == 0U) {
		return ib_fail(error, "column name storage is unavailable");
	}
	if (value_length < 0 || (size_t) value_length >= capacity) {
		return ib_fail(error, "column name is too long");
	}
	name_length = (size_t) value_length;
	if (name_length != 0U) {
		memcpy(buffer, value, name_length);
	}
	while (name_length > 0U && buffer[name_length - 1U] == ' ') {
		name_length--;
	}
	buffer[name_length] = '\0';
	*length = name_length;
	return name_length != 0U;
}

static int ib_catalog_string(const XSQLVAR *variable, char *buffer,
	size_t capacity, size_t *length, char **error)
{
	int type;
	size_t value_length;
	unsigned short varying_length;

	if (variable == NULL || buffer == NULL || length == NULL || capacity == 0U) {
		return ib_fail(error, "catalog string storage is unavailable");
	}
	*length = 0U;
	if (variable->sqlind != NULL && *variable->sqlind < 0) {
		return 0;
	}
	if (variable->sqldata == NULL) {
		return ib_fail(error, "catalog string data is unavailable");
	}
	type = ib_sql_type(variable);
	if (type == SQL_TEXT) {
		if (variable->sqllen < 0) {
			return ib_fail(error, "catalog string length is invalid");
		}
		value_length = (size_t) variable->sqllen;
		if (value_length >= capacity) {
			return ib_fail(error, "catalog string is too long");
		}
		if (value_length != 0U) {
			memcpy(buffer, variable->sqldata, value_length);
		}
	} else if (type == SQL_VARYING) {
		if (variable->sqllen < 0) {
			return ib_fail(error, "catalog varying length is invalid");
		}
		memcpy(&varying_length, variable->sqldata, sizeof(varying_length));
		if (varying_length > (unsigned short) variable->sqllen) {
			return ib_fail(error, "catalog varying value length is invalid");
		}
		value_length = (size_t) varying_length;
		if (value_length >= capacity) {
			return ib_fail(error, "catalog string is too long");
		}
		if (value_length != 0U) {
			memcpy(buffer, variable->sqldata + sizeof(varying_length), value_length);
		}
	} else {
		return ib_fail(error, "catalog value is not textual");
	}
	while (value_length > 0U && buffer[value_length - 1U] == ' ') {
		value_length--;
	}
	buffer[value_length] = '\0';
	*length = value_length;
	return 1;
}

static int ib_catalog_integer(const XSQLVAR *variable, int *value,
	int *present, char **error)
{
	int type;
	short short_value;
	ISC_LONG long_value;
	ISC_INT64 int64_value;

	if (variable == NULL || value == NULL || present == NULL) {
		return ib_fail(error, "catalog integer storage is unavailable");
	}
	*value = 0;
	*present = 0;
	if (variable->sqlind != NULL && *variable->sqlind < 0) {
		return 0;
	}
	if (variable->sqldata == NULL) {
		return ib_fail(error, "catalog integer data is unavailable");
	}
	type = ib_sql_type(variable);
	switch (type) {
	case SQL_SHORT:
		memcpy(&short_value, variable->sqldata, sizeof(short_value));
		*value = (int) short_value;
		break;
	case SQL_LONG:
		memcpy(&long_value, variable->sqldata, sizeof(long_value));
		if (long_value < (ISC_LONG) INT_MIN || long_value > (ISC_LONG) INT_MAX) {
			return ib_fail(error, "catalog integer value is out of range");
		}
		*value = (int) long_value;
		break;
	case SQL_INT64:
		memcpy(&int64_value, variable->sqldata, sizeof(int64_value));
		if (int64_value < (ISC_INT64) INT_MIN || int64_value > (ISC_INT64) INT_MAX) {
			return ib_fail(error, "catalog integer value is out of range");
		}
		*value = (int) int64_value;
		break;
	default:
		return ib_fail(error, "catalog value is not an integer");
	}
	*present = 1;
	return 0;
}

static int ib_metadata_allocate(ib_cursor *cursor, char **error)
{
	short index;

	if (cursor == NULL || cursor->output == NULL || cursor->output->sqld < 0) {
		return ib_fail(error, "output metadata is unavailable");
	}
	if (cursor->output->sqld == 0) {
		return 0;
	}
	if ((size_t) cursor->output->sqld > SIZE_MAX / sizeof(*cursor->metadata)) {
		return ib_fail(error, "output metadata is too large");
	}
	cursor->metadata = (ib_column_metadata *) calloc((size_t) cursor->output->sqld,
		sizeof(*cursor->metadata));
	if (cursor->metadata == NULL) {
		return ib_fail(error, "out of memory allocating output metadata");
	}
	for (index = 0; index < cursor->output->sqld; index++) {
		XSQLVAR *variable = &cursor->output->sqlvar[index];
		int type = ib_sql_type(variable);

		cursor->metadata[index].sql_type = ib_metadata_type_code(type);
		cursor->metadata[index].sql_subtype = variable->sqlsubtype;
		if (type == SQL_TEXT || type == SQL_VARYING) {
			cursor->metadata[index].sql_subtype = variable->sqlsubtype & 0xFF;
		}
		cursor->metadata[index].sql_scale = variable->sqlscale;
		cursor->metadata[index].sql_precision = variable->sqlprecision;
		if (variable->relname_length > 0 && variable->sqlname_length > 0) {
			/* The live SQLDA nullable bit accounts for result-level nullability,
			 * including columns introduced by an outer join. */
			cursor->metadata[index].nullable = (variable->sqltype & 1) != 0;
			cursor->metadata[index].has_nullable = 1;
		}
	}
	return 0;
}

static int ib_catalog_relation_list(ib_cursor *cursor, char ***names,
	size_t **lengths, size_t *count, char **error)
{
	char **relation_names;
	size_t *relation_lengths;
	short index;
	size_t relation_count;
	size_t relation_index;

	if (cursor == NULL || cursor->output == NULL || names == NULL ||
		lengths == NULL || count == NULL) {
		return ib_fail(error, "catalog relation list is unavailable");
	}
	relation_names = NULL;
	relation_lengths = NULL;
	relation_count = 0U;
	for (index = 0; index < cursor->output->sqld; index++) {
		char relation[METADATALENGTH + 1U];
		size_t relation_length;
		short relation_name_length = cursor->output->sqlvar[index].relname_length;
		int name_result;
		char **resized_names;
		size_t *resized_lengths;

		name_result = ib_descriptor_name(cursor->output->sqlvar[index].relname,
			relation_name_length, relation, sizeof(relation), &relation_length, error);
		if (name_result < 0) {
			goto fail;
		}
		if (name_result == 0) {
			continue;
		}
		for (relation_index = 0U; relation_index < relation_count; relation_index++) {
			if (relation_lengths[relation_index] == relation_length &&
				memcmp(relation_names[relation_index], relation, relation_length) == 0) {
				break;
			}
		}
		if (relation_index != relation_count) {
			continue;
		}
		if (relation_count == SIZE_MAX / sizeof(*relation_names) ||
			relation_count == SIZE_MAX / sizeof(*relation_lengths)) {
			(void) ib_fail(error, "too many catalog relations");
			goto fail;
		}
		resized_names = (char **) malloc((relation_count + 1U) * sizeof(*relation_names));
		resized_lengths = (size_t *) malloc((relation_count + 1U) * sizeof(*relation_lengths));
		if (resized_names == NULL || resized_lengths == NULL) {
			free(resized_names);
			free(resized_lengths);
			(void) ib_fail(error, "out of memory collecting catalog relations");
			goto fail;
		}
		if (relation_count != 0U) {
			memcpy(resized_names, relation_names,
				relation_count * sizeof(*relation_names));
			memcpy(resized_lengths, relation_lengths,
				relation_count * sizeof(*relation_lengths));
		}
		free(relation_names);
		free(relation_lengths);
		relation_names = resized_names;
		relation_lengths = resized_lengths;
		relation_names[relation_count] = ib_copy_string(relation, relation_length);
		if (relation_names[relation_count] == NULL) {
			(void) ib_fail(error, "out of memory copying catalog relation");
			goto fail;
		}
		relation_lengths[relation_count] = relation_length;
		relation_count++;
	}
	*names = relation_names;
	*lengths = relation_lengths;
	*count = relation_count;
	return 0;

fail:
	if (relation_names != NULL) {
		for (relation_index = 0U; relation_index < relation_count; relation_index++) {
			free(relation_names[relation_index]);
		}
	}
	free(relation_names);
	free(relation_lengths);
	return -1;
}

static void ib_free_catalog_relations(char **names, size_t *lengths, size_t count)
{
	size_t index;

	(void) lengths;
	if (names == NULL) {
		free(lengths);
		return;
	}
	for (index = 0U; index < count; index++) {
		free(names[index]);
	}
	free(names);
	free(lengths);
}

static int ib_catalog_query_text(size_t relation_count, char **query,
	size_t *query_length, char **error)
{
	static const char prefix[] =
		"SELECT rf.RDB$RELATION_NAME, rf.RDB$FIELD_NAME, "
		"f.RDB$FIELD_TYPE, f.RDB$FIELD_SUB_TYPE, f.RDB$FIELD_LENGTH, "
		"f.RDB$FIELD_SCALE, f.RDB$FIELD_PRECISION, f.RDB$CHARACTER_LENGTH, "
		"f.RDB$CHARACTER_SET_ID, "
		"rf.RDB$NULL_FLAG "
		"FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f "
		"ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE "
		"WHERE rf.RDB$RELATION_NAME IN (";
	static const char suffix[] = ")";
	char *text;
	size_t prefix_length;
	size_t suffix_length;
	size_t placeholders;
	size_t total;
	size_t offset;
	size_t index;

	if (query == NULL || query_length == NULL || relation_count == 0U) {
		return ib_fail(error, "catalog query storage is unavailable");
	}
	prefix_length = sizeof(prefix) - 1U;
	suffix_length = sizeof(suffix) - 1U;
	if (relation_count > (SIZE_MAX - prefix_length - suffix_length) / 3U) {
		return ib_fail(error, "catalog query is too long");
	}
	placeholders = relation_count * 3U - 1U;
	total = prefix_length + placeholders + suffix_length;
	if (total > (size_t) USHRT_MAX) {
		return ib_fail(error, "catalog query is too long");
	}
	text = (char *) malloc(total + 1U);
	if (text == NULL) {
		return ib_fail(error, "out of memory building catalog query");
	}
	memcpy(text, prefix, prefix_length);
	offset = prefix_length;
	for (index = 0U; index < relation_count; index++) {
		if (index != 0U) {
			text[offset++] = ',';
			text[offset++] = ' ';
		}
		text[offset++] = '?';
	}
	memcpy(text + offset, suffix, suffix_length);
	offset += suffix_length;
	text[offset] = '\0';
	*query = text;
	*query_length = offset;
	return 0;
}

static int ib_catalog_apply_row(ib_cursor *cursor, const ib_cursor *catalog,
	char **error)
{
	char relation[METADATALENGTH + 1U];
	char field[METADATALENGTH + 1U];
	size_t relation_length;
	size_t field_length;
	int field_type;
	int sql_type;
	int field_subtype;
	int field_length_value;
	int field_scale;
	int field_precision;
	int character_length;
	int character_set_id;
	int null_flag;
	int present;
	short index;
	int relation_result;
	int field_result;

	if (cursor == NULL || catalog == NULL || catalog->output == NULL ||
		catalog->output->sqld < 10) {
		return ib_fail(error, "catalog output is incomplete");
	}
	relation_result = ib_catalog_string(&catalog->output->sqlvar[0], relation,
		sizeof(relation), &relation_length, error);
	field_result = ib_catalog_string(&catalog->output->sqlvar[1], field,
		sizeof(field), &field_length, error);
	if (relation_result < 0 || field_result < 0) {
		return -1;
	}
	if (relation_result == 0 || field_result == 0) {
		return 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[2], &field_type, &present, error) != 0 ||
		!present || ib_catalog_integer(&catalog->output->sqlvar[4], &field_length_value,
		&present, error) != 0 || !present) {
		return -1;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[3], &field_subtype, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_subtype = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[5], &field_scale, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_scale = 0;
	}
	sql_type = ib_catalog_sql_type(field_type);
	if (ib_catalog_integer(&catalog->output->sqlvar[6], &field_precision, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_precision = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[7], &character_length, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		character_length = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[8], &character_set_id, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		character_set_id = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[9], &null_flag, &present,
		error) != 0) {
		return -1;
	}
	for (index = 0; index < cursor->output->sqld; index++) {
		char source_relation[METADATALENGTH + 1U];
		char source_field[METADATALENGTH + 1U];
		size_t source_relation_length;
		size_t source_field_length;
		int source_relation_result;
		int source_field_result;
		XSQLVAR *variable = &cursor->output->sqlvar[index];

		source_relation_result = ib_descriptor_name(variable->relname,
			variable->relname_length, source_relation, sizeof(source_relation),
			&source_relation_length, error);
		source_field_result = ib_descriptor_name(variable->sqlname,
			variable->sqlname_length, source_field, sizeof(source_field),
			&source_field_length, error);
		if (source_relation_result < 0 || source_field_result < 0) {
			return -1;
		}
		if (source_relation_result == 0 || source_field_result == 0 ||
			source_relation_length != relation_length ||
			source_field_length != field_length ||
			memcmp(source_relation, relation, relation_length) != 0 ||
			memcmp(source_field, field, field_length) != 0) {
			continue;
		}
		cursor->metadata[index].sql_type = ib_metadata_type_code(sql_type);
		cursor->metadata[index].sql_subtype = field_subtype;
		if ((sql_type == SQL_TEXT || sql_type == SQL_VARYING) &&
			character_set_id > 0) {
			cursor->metadata[index].sql_subtype = character_set_id;
		}
		cursor->metadata[index].sql_scale = field_scale;
		cursor->metadata[index].sql_precision = field_precision;
		if ((sql_type == SQL_TEXT || sql_type == SQL_VARYING) &&
			character_length > 0) {
			cursor->metadata[index].length = character_length;
			cursor->metadata[index].has_length = 1;
		} else if ((sql_type == SQL_TEXT || sql_type == SQL_VARYING) &&
			field_length_value > 0 && field_subtype == 1) {
			cursor->metadata[index].length = field_length_value;
			cursor->metadata[index].has_length = 1;
		}
		if ((sql_type == SQL_SHORT || sql_type == SQL_LONG ||
			sql_type == SQL_INT64) && field_precision > 0) {
			cursor->metadata[index].precision = field_precision;
			cursor->metadata[index].scale = field_scale;
			cursor->metadata[index].has_precision_scale = 1;
		}
		cursor->metadata[index].nullable = cursor->metadata[index].nullable ||
			(present ? null_flag == 0 : 1);
		cursor->metadata[index].has_nullable = 1;
	}
	return 0;
}

static int ib_metadata_catalog(ib_cursor *cursor, char **error)
{
	char **relation_names;
	size_t *relation_lengths;
	size_t relation_count;
	char *catalog_query;
	size_t catalog_query_length;
	ib_bindings *bindings;
	ib_cursor *catalog_cursor;
	char *catalog_error;
	char *close_error;
	size_t index;
	int result;
	int close_result;

	relation_names = NULL;
	relation_lengths = NULL;
	relation_count = 0U;
	if (ib_catalog_relation_list(cursor, &relation_names, &relation_lengths,
		&relation_count, error) != 0) {
		return -1;
	}
	if (relation_count == 0U) {
		return 0;
	}
	catalog_query = NULL;
	catalog_query_length = 0U;
	if (ib_catalog_query_text(relation_count, &catalog_query, &catalog_query_length,
		error) != 0) {
		ib_free_catalog_relations(relation_names, relation_lengths, relation_count);
		return -1;
	}
	catalog_cursor = (ib_cursor *) calloc(1U, sizeof(*catalog_cursor));
	if (catalog_cursor == NULL) {
		free(catalog_query);
		ib_free_catalog_relations(relation_names, relation_lengths, relation_count);
		return ib_fail(error, "out of memory allocating catalog cursor");
	}
	catalog_cursor->connection = cursor->connection;
	catalog_cursor->transaction = cursor->transaction;
	bindings = NULL;
	catalog_error = NULL;
	close_error = NULL;
	result = 0;
	close_result = 0;
	if (ib_cursor_prepare_statement(catalog_cursor, catalog_query,
		catalog_query_length, &catalog_error) != 0 ||
		ib_statement_is_select(catalog_cursor, &catalog_error) != 0 ||
		ib_describe_bind(catalog_cursor, &catalog_error) != 0) {
		result = -1;
		goto catalog_done;
	}
	bindings = ib_bindings_new(relation_count, &catalog_error);
	if (bindings == NULL) {
		result = -1;
		goto catalog_done;
	}
	for (index = 0U; index < relation_count; index++) {
		if (ib_bindings_set_string(bindings, index, relation_names[index],
			relation_lengths[index], &catalog_error) != 0) {
			result = -1;
			goto catalog_done;
		}
	}
	if (ib_bind_input(catalog_cursor, bindings, &catalog_error) != 0 ||
		ib_describe_output(catalog_cursor, &catalog_error) != 0 ||
		ib_validate_output_types(catalog_cursor->output, &catalog_error) != 0 ||
		ib_allocate_output(catalog_cursor, &catalog_error) != 0) {
		result = -1;
		goto catalog_done;
	}
	{
		ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
		ISC_STATUS native_result;
		XSQLDA *input = catalog_cursor->input->sqld == 0 ? NULL : catalog_cursor->input;

		memset(status, 0, sizeof(status));
		native_result = isc_dsql_execute2(status, &catalog_cursor->transaction,
			&catalog_cursor->statement,
			ib_connection_dialect(catalog_cursor->connection), input, NULL);
		if (native_result != 0) {
			(void) ib_fail_status(&catalog_error, "execute catalog query", status);
			result = -1;
			goto catalog_done;
		}
	}
	catalog_cursor->server_cursor_open = 1;
	for (;;) {
		int has_row = ib_cursor_next(catalog_cursor, &catalog_error);

		if (has_row < 0) {
			result = -1;
			break;
		}
		if (has_row == 0) {
			break;
		}
		if (ib_catalog_apply_row(cursor, catalog_cursor, &catalog_error) != 0) {
			result = -1;
			break;
		}
	}

catalog_done:
	if (bindings != NULL) {
		ib_bindings_free(bindings);
	}
	if (catalog_cursor->statement != NULL || catalog_cursor->input != NULL ||
		catalog_cursor->output != NULL) {
		close_result = ib_cursor_close_internal(catalog_cursor, &close_error, 0);
		if (close_result != 0) {
			result = -1;
		}
	} else {
		free(catalog_cursor);
	}
	free(catalog_query);
	ib_free_catalog_relations(relation_names, relation_lengths, relation_count);
	if (close_result != 0) {
		ib_append_error(&catalog_error, close_error);
		close_error = NULL;
		ib_give_error(error, catalog_error);
		return -1;
	}
	if (result != 0) {
		/* Catalog information is an enhancement; an unavailable catalog must not
		 * turn an otherwise valid user query into a failure. */
		free(catalog_error);
		free(close_error);
		return 0;
	}
	free(catalog_error);
	free(close_error);
	return 0;
}

static int ib_cursor_describe_metadata(ib_cursor *cursor, char **error)
{
	if (ib_metadata_allocate(cursor, error) != 0) {
		return -1;
	}
	return ib_metadata_catalog(cursor, error);
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
	if (ib_cursor_close_internal(cursor, &cleanup_error, 0) != 0) {
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

static void ib_statement_unregister(ib_statement *statement)
{
	ib_statement **current;

	if (statement == NULL || statement->connection == NULL) {
		return;
	}
	current = &statement->connection->statements;
	while (*current != NULL) {
		if (*current == statement) {
			*current = statement->next;
			statement->next = NULL;
			return;
		}
		current = &(*current)->next;
	}
}

static int ib_statement_close_internal(ib_statement *statement, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;
	ib_connection *connection;
	int failed;

	if (statement == NULL) {
		return 0;
	}
	connection = statement->connection;
	first_error = NULL;
	failed = 0;
	if (connection != NULL && connection->active_cursor != NULL &&
		connection->active_cursor->prepared == statement) {
		cursor_error = NULL;
		if (ib_cursor_close_internal(connection->active_cursor, &cursor_error, 1) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	if (statement->statement != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &statement->statement, DSQL_drop);
		statement->statement = NULL;
		if (result != 0) {
			failed = 1;
			cursor_error = NULL;
			(void) ib_fail_status(&cursor_error, "close prepared statement", status);
			ib_append_error(&first_error, cursor_error);
		}
	}
	ib_free_sqlda(statement->input);
	ib_free_sqlda(statement->output);
	statement->input = NULL;
	statement->output = NULL;
	ib_statement_unregister(statement);
	statement->connection = NULL;
	free(statement);
	if (failed && connection != NULL) {
		connection->broken = 1;
	}
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

ib_connection *ib_connection_open(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, const char *charset, size_t charset_length,
	int dialect, char **error)
{
	unsigned char *dpb;
	size_t dpb_length;
	size_t offset;
	ib_connection *connection;
	isc_db_handle database_handle;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	short charset_id;
	const char *charset_name;
	size_t charset_name_length;
	unsigned char length;

	if (database == NULL || database_length == 0U || database_length > (size_t) SHRT_MAX) {
		ib_fail(error, "database name is empty or too long");
		return NULL;
	}
	if (dialect != SQL_DIALECT_V5 && dialect != SQL_DIALECT_V6) {
		ib_fail(error, "connection SQL dialect is unsupported");
		return NULL;
	}
	if (user == NULL || user_length == 0U || user_length > (size_t) UCHAR_MAX ||
		password == NULL || password_length > (size_t) UCHAR_MAX) {
		ib_fail(error, "connection credential length is invalid");
		return NULL;
	}
	if (ib_charset_id(charset, charset_length, &charset_id) != 0) {
		ib_fail(error, "connection character set is unsupported");
		return NULL;
	}
	charset_name = ib_charset_name(charset_id);
	if (charset_name == NULL) {
		ib_fail(error, "connection character set is unsupported");
		return NULL;
	}
	charset_name_length = strlen(charset_name);
	dpb_length = 1U + 2U + user_length + 2U + password_length +
		2U + charset_name_length + 2U + 1U;
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
	dpb[offset++] = isc_dpb_sql_dialect;
	dpb[offset++] = 1U;
	dpb[offset++] = (unsigned char) dialect;
	dpb[offset++] = isc_dpb_lc_ctype;
	dpb[offset++] = (unsigned char) charset_name_length;
	memcpy(dpb + offset, charset_name, charset_name_length);
	offset += charset_name_length;

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
	connection->charset = charset_id;
	connection->dialect = dialect;
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
		if (ib_cursor_close_internal(connection->active_cursor, &cursor_error, 1) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	while (connection->statements != NULL) {
		cursor_error = NULL;
		if (ib_statement_close_internal(connection->statements, &cursor_error) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	if (connection->transaction != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_rollback_transaction(status, &connection->transaction);
		connection->transaction = NULL;
		if (result != 0) {
			failed = 1;
			cursor_error = NULL;
			(void) ib_fail_status(&cursor_error, "rollback transaction", status);
			ib_append_error(&first_error, cursor_error);
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

int ib_connection_begin(ib_connection *connection, int read_only, char **error)
{
	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction != NULL) {
		return ib_fail(error, "a transaction is already active");
	}
	if (connection->active_cursor != NULL) {
		return ib_fail(error, "another result cursor is active on this connection");
	}
	return ib_start_transaction(connection, &connection->transaction,
		read_only != 0, "start transaction", error);
}

int ib_connection_commit(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	first_error = NULL;
	if (connection->active_cursor != NULL) {
		cursor_error = NULL;
		if (ib_cursor_close_internal(connection->active_cursor, &cursor_error, 1) != 0) {
			ib_append_error(&first_error, cursor_error);
			ib_give_error(error, first_error);
			return -1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	memset(status, 0, sizeof(status));
	result = isc_commit_transaction(status, &connection->transaction);
	if (result != 0) {
		connection->broken = 1;
		(void) ib_fail_status(&first_error, "commit transaction", status);
		ib_give_error(error, first_error);
		return -1;
	}
	connection->transaction = NULL;
	ib_give_error(error, first_error);
	return 0;
}

int ib_connection_rollback(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;
	int failed;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	first_error = NULL;
	failed = 0;
	if (connection->active_cursor != NULL) {
		cursor_error = NULL;
		if (ib_cursor_close_internal(connection->active_cursor, &cursor_error, 1) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	memset(status, 0, sizeof(status));
	result = isc_rollback_transaction(status, &connection->transaction);
	connection->transaction = NULL;
	if (result != 0) {
		failed = 1;
		connection->broken = 1;
		cursor_error = NULL;
		(void) ib_fail_status(&cursor_error, "rollback transaction", status);
		ib_append_error(&first_error, cursor_error);
	}
	if (failed) {
		ib_give_error(error, first_error);
		return -1;
	}
	return 0;
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

int ib_bindings_set_bytes(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error)
{
	char *copy;

	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	if (length != 0U && value == NULL) {
		return ib_fail(error, "byte query argument is missing data");
	}
	copy = ib_copy_string(value, length);
	if (copy == NULL) {
		return ib_fail(error, "out of memory copying byte query argument");
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_BYTES;
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

int ib_bindings_set_timestamp(ib_bindings *bindings, size_t index,
	int64_t year, int month, int day, int hour, int minute, int second,
	int nanosecond, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_TIMESTAMP;
	bindings->values[index].year = year;
	bindings->values[index].month = month;
	bindings->values[index].day = day;
	bindings->values[index].hour = hour;
	bindings->values[index].minute = minute;
	bindings->values[index].second = second;
	bindings->values[index].nanosecond = nanosecond;
	return 0;
}

ib_cursor *ib_connection_query(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	char *prepared_query;
	size_t prepared_query_length;
	int statement_type;
	int implicit;

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
	prepared_query = ib_convert_utf8(query, query_length, connection->charset,
		&prepared_query_length, error);
	if (prepared_query == NULL) {
		return NULL;
	}
	if (prepared_query_length > (size_t) USHRT_MAX) {
		free(prepared_query);
		ib_fail(error, "converted query is too long");
		return NULL;
	}

	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		free(prepared_query);
		ib_fail(error, "out of memory allocating cursor state");
		return NULL;
	}
	cursor->connection = connection;
	if (connection->transaction != NULL) {
		cursor->transaction = connection->transaction;
		cursor->owns_transaction = 0;
		implicit = 0;
	} else if (ib_start_transaction(connection, &cursor->transaction, 1,
		"start read transaction", error) != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	} else {
		cursor->owns_transaction = 1;
		implicit = 1;
	}
	if (ib_cursor_prepare_statement(cursor, prepared_query, prepared_query_length,
		error) != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (ib_statement_type(cursor, &statement_type, error) != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (implicit && statement_type == isc_info_sql_stmt_exec_procedure) {
		if (ib_cursor_drop_statement(cursor, error) != 0 ||
			ib_cursor_rollback_owned_transaction(cursor, error) != 0) {
			goto procedure_transition_failure;
		}
		if (ib_start_transaction(connection, &cursor->transaction, 0,
			"start write transaction", error) != 0) {
			goto procedure_transition_failure;
		}
		cursor->owns_transaction = 1;
		if (ib_cursor_prepare_statement(cursor, prepared_query, prepared_query_length,
			error) != 0) {
			goto procedure_transition_failure;
		}
		if (ib_statement_type(cursor, &statement_type, error) != 0) {
			goto procedure_transition_failure;
		}
	}
	if (statement_type != isc_info_sql_stmt_select &&
		statement_type != isc_info_sql_stmt_exec_procedure) {
		free(prepared_query);
		prepared_query = NULL;
		(void) ib_fail(error, "only SELECT and executable procedure statements are permitted");
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	free(prepared_query);
	prepared_query = NULL;
	if (ib_describe_bind(cursor, error) != 0 ||
		ib_bind_input(cursor, bindings, error) != 0 ||
		ib_describe_output(cursor, error) != 0 ||
		ib_cursor_describe_metadata(cursor, error) != 0 ||
		ib_validate_output_types(cursor->output, error) != 0 ||
		ib_allocate_output(cursor, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (statement_type == isc_info_sql_stmt_exec_procedure &&
		cursor->output->sqld == 0) {
		(void) ib_fail(error, "procedure has no output; use Exec instead");
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute2(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input,
			statement_type == isc_info_sql_stmt_exec_procedure ? cursor->output : NULL);
	}
	if (result != 0) {
		(void) ib_fail_status(error,
			statement_type == isc_info_sql_stmt_exec_procedure ?
			"execute procedure" : "execute SELECT", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	cursor->statement_type = statement_type;
	cursor->procedure = statement_type == isc_info_sql_stmt_exec_procedure;
	cursor->output_pending = cursor->procedure && cursor->output->sqld > 0;
	connection->active_cursor = cursor;
	return cursor;

procedure_transition_failure:
	free(prepared_query);
	prepared_query = NULL;
	ib_failed_query_cleanup(cursor, error);
	return NULL;
}

int ib_connection_exec(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, int64_t *rows_affected,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	char *prepared_query;
	char *cleanup_error;
	char *rollback_error;
	char *commit_error;
	isc_tr_handle implicit_transaction;
	size_t prepared_query_length;
	int close_result;
	int implicit;

	if (rows_affected == NULL) {
		return ib_fail(error, "affected-row destination is unavailable");
	}
	*rows_affected = -1;
	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		return ib_fail(error, "query is empty or too long");
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		return ib_fail(error, "query arguments are invalid");
	}
	if (connection->active_cursor != NULL) {
		return ib_fail(error, "another result cursor is active on this connection");
	}
	prepared_query = ib_convert_utf8(query, query_length, connection->charset,
		&prepared_query_length, error);
	if (prepared_query == NULL) {
		return -1;
	}
	if (prepared_query_length > (size_t) USHRT_MAX) {
		free(prepared_query);
		return ib_fail(error, "converted query is too long");
	}

	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		free(prepared_query);
		return ib_fail(error, "out of memory allocating cursor state");
	}
	cursor->connection = connection;
	implicit = 0;
	if (connection->transaction != NULL) {
		cursor->transaction = connection->transaction;
		cursor->owns_transaction = 0;
	} else {
		if (ib_start_transaction(connection, &cursor->transaction, 0,
			"start write transaction", error) != 0) {
			free(prepared_query);
			prepared_query = NULL;
			ib_failed_query_cleanup(cursor, error);
			return -1;
		}
		cursor->owns_transaction = 1;
		implicit = 1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &connection->database,
		&cursor->statement);
	if (result != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_fail_status(error, "allocate statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &cursor->transaction, &cursor->statement,
		(unsigned short) prepared_query_length, prepared_query,
		ib_connection_dialect(cursor->connection), NULL);
	free(prepared_query);
	prepared_query = NULL;
	if (result != 0) {
		ib_fail_status(error, "prepare statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_statement_type(cursor, &cursor->statement_type, error) != 0 ||
		(cursor->statement_type == isc_info_sql_stmt_exec_procedure &&
			ib_describe_output(cursor, error) != 0) ||
		ib_statement_rejects_result(cursor, error) != 0 ||
		ib_describe_bind(cursor, error) != 0 ||
		ib_bind_input(cursor, bindings, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input);
	}
	if (result != 0) {
		ib_fail_status(error, "execute statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_statement_rows_affected(cursor, rows_affected, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}

	implicit_transaction = NULL;
	if (implicit) {
		implicit_transaction = cursor->transaction;
		cursor->owns_transaction = 0;
	}
	cleanup_error = NULL;
	close_result = ib_cursor_close_internal(cursor, &cleanup_error, 0);
	cursor = NULL;
	if (close_result != 0) {
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed write", rollback_status);
			}
			ib_append_error(&cleanup_error, rollback_error);
		}
		ib_give_error(error, cleanup_error);
		return -1;
	}
	if (implicit_transaction == NULL) {
		ib_give_error(error, cleanup_error);
		return 0;
	}

	memset(status, 0, sizeof(status));
	result = isc_commit_transaction(status, &implicit_transaction);
	if (result != 0) {
		connection->broken = 1;
		commit_error = NULL;
		(void) ib_fail_status(&commit_error, "commit write transaction", status);
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed write", rollback_status);
			}
			ib_append_error(&commit_error, rollback_error);
		}
		ib_append_error(&commit_error, cleanup_error);
		ib_give_error(error, commit_error);
		return -1;
	}
	ib_give_error(error, cleanup_error);
	return 0;
}

ib_statement *ib_statement_prepare(ib_connection *connection, const char *query,
	size_t query_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_statement *statement;
	ib_cursor descriptor;
	char *prepared_query;
	char *failure_error;
	char *cleanup_error;
	char *rollback_error;
	isc_tr_handle prepare_transaction;
	size_t prepared_query_length;
	int owns_transaction;
	int statement_type;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		ib_fail(error, "connection is unavailable");
		return NULL;
	}
	if (query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		ib_fail(error, "query is empty or too long");
		return NULL;
	}
	if (connection->active_cursor != NULL) {
		ib_fail(error, "another result cursor is active on this connection");
		return NULL;
	}
	prepared_query = ib_convert_utf8(query, query_length, connection->charset,
		&prepared_query_length, error);
	if (prepared_query == NULL) {
		return NULL;
	}
	if (prepared_query_length > (size_t) USHRT_MAX) {
		free(prepared_query);
		ib_fail(error, "converted query is too long");
		return NULL;
	}
	statement = (ib_statement *) calloc(1U, sizeof(*statement));
	if (statement == NULL) {
		free(prepared_query);
		ib_fail(error, "out of memory allocating prepared statement");
		return NULL;
	}
	statement->connection = connection;
	prepare_transaction = NULL;
	owns_transaction = 0;
	failure_error = NULL;
	cleanup_error = NULL;
	rollback_error = NULL;
	memset(&descriptor, 0, sizeof(descriptor));
	if (connection->transaction != NULL) {
		prepare_transaction = connection->transaction;
	} else {
		if (ib_start_transaction(connection, &prepare_transaction, 1,
			"start prepare transaction", &failure_error) != 0) {
			goto fail;
		}
		owns_transaction = 1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &connection->database,
		&statement->statement);
	if (result != 0) {
		(void) ib_fail_status(&failure_error, "allocate statement", status);
		goto fail;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &prepare_transaction, &statement->statement,
		(unsigned short) prepared_query_length, prepared_query,
		ib_connection_dialect(connection), NULL);
	free(prepared_query);
	prepared_query = NULL;
	if (result != 0) {
		(void) ib_fail_status(&failure_error, "prepare statement", status);
		goto fail;
	}
	descriptor.connection = connection;
	descriptor.transaction = prepare_transaction;
	descriptor.statement = statement->statement;
	if (ib_describe_bind(&descriptor, &failure_error) != 0) {
		goto fail;
	}
	statement->input = descriptor.input;
	descriptor.input = NULL;
	if (ib_statement_type(&descriptor, &statement_type, &failure_error) != 0) {
		goto fail;
	}
	statement->statement_type = statement_type;
	if (statement_type == isc_info_sql_stmt_select ||
		statement_type == isc_info_sql_stmt_exec_procedure) {
		if (ib_describe_output(&descriptor, &failure_error) != 0 ||
			ib_validate_output_types(descriptor.output, &failure_error) != 0) {
			goto fail;
		}
		statement->output = descriptor.output;
		descriptor.output = NULL;
	}
	if (owns_transaction) {
		memset(status, 0, sizeof(status));
		result = isc_commit_transaction(status, &prepare_transaction);
		if (result != 0) {
			connection->broken = 1;
			(void) ib_fail_status(&failure_error, "commit prepare transaction", status);
			goto fail;
		}
	}
	statement->next = connection->statements;
	connection->statements = statement;
	ib_free_sqlda(descriptor.input);
	ib_free_sqlda(descriptor.output);
	return statement;

fail:
	free(prepared_query);
	ib_free_sqlda(descriptor.input);
	ib_free_sqlda(descriptor.output);
	if (statement != NULL) {
		if (ib_statement_close_internal(statement, &cleanup_error) != 0) {
			connection->broken = 1;
		}
	}
	if (owns_transaction && prepare_transaction != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_rollback_transaction(status, &prepare_transaction);
		if (result != 0) {
			connection->broken = 1;
			(void) ib_fail_status(&rollback_error, "rollback prepare transaction", status);
		}
	}
	ib_append_error(&failure_error, cleanup_error);
	ib_append_error(&failure_error, rollback_error);
	ib_give_error(error, failure_error);
	return NULL;
}

int ib_statement_num_input(const ib_statement *statement)
{
	if (statement == NULL || statement->input == NULL || statement->input->sqld < 0) {
		return 0;
	}
	return (int) statement->input->sqld;
}

int ib_statement_exec(ib_statement *statement, const ib_bindings *bindings,
	int64_t *rows_affected, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	char *cleanup_error;
	char *rollback_error;
	char *commit_error;
	isc_tr_handle implicit_transaction;
	int close_result;

	if (rows_affected == NULL) {
		return ib_fail(error, "affected-row destination is unavailable");
	}
	*rows_affected = -1;
	if (statement == NULL || statement->connection == NULL ||
		statement->statement == NULL || statement->connection->database == NULL ||
		statement->connection->broken) {
		return ib_fail(error, "prepared statement is unavailable");
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		return ib_fail(error, "query arguments are invalid");
	}
	if (statement->connection->active_cursor != NULL) {
		return ib_fail(error, "another result cursor is active on this connection");
	}
	if (statement->statement_type == isc_info_sql_stmt_select ||
		statement->statement_type == isc_info_sql_stmt_select_for_upd ||
		(statement->statement_type == isc_info_sql_stmt_exec_procedure &&
			(statement->output == NULL || statement->output->sqld != 0))) {
		return ib_fail(error, "statement produces a result set; use Query instead");
	}
	if (ib_statement_rejects_transaction_control_type(statement->statement_type,
		error) != 0) {
		return -1;
	}
	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		return ib_fail(error, "out of memory allocating prepared execution state");
	}
	cursor->connection = statement->connection;
	cursor->prepared = statement;
	cursor->statement = statement->statement;
	cursor->statement_type = statement->statement_type;
	if (statement->connection->transaction != NULL) {
		cursor->transaction = statement->connection->transaction;
		cursor->owns_transaction = 0;
	} else if (ib_start_transaction(statement->connection, &cursor->transaction, 0,
		"start write transaction", error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	} else {
		cursor->owns_transaction = 1;
	}
	cursor->input = ib_clone_sqlda(statement->input, error);
	if (cursor->input == NULL || ib_bind_input(cursor, bindings, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input);
	}
	if (result != 0) {
		(void) ib_fail_status(error, "execute prepared statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_statement_rows_affected(cursor, rows_affected, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	implicit_transaction = NULL;
	if (cursor->owns_transaction) {
		implicit_transaction = cursor->transaction;
		cursor->owns_transaction = 0;
	}
	cleanup_error = NULL;
	close_result = ib_cursor_close_internal(cursor, &cleanup_error, 0);
	cursor = NULL;
	if (close_result != 0) {
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed prepared write", rollback_status);
			}
			ib_append_error(&cleanup_error, rollback_error);
		}
		ib_give_error(error, cleanup_error);
		return -1;
	}
	if (implicit_transaction == NULL) {
		ib_give_error(error, cleanup_error);
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_commit_transaction(status, &implicit_transaction);
	if (result != 0) {
		commit_error = NULL;
		statement->connection->broken = 1;
		(void) ib_fail_status(&commit_error, "commit prepared write transaction", status);
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed prepared write", rollback_status);
			}
			ib_append_error(&commit_error, rollback_error);
		}
		ib_append_error(&commit_error, cleanup_error);
		ib_give_error(error, commit_error);
		return -1;
	}
	ib_give_error(error, cleanup_error);
	return 0;
}

ib_cursor *ib_statement_query(ib_statement *statement,
	const ib_bindings *bindings, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;

	if (statement == NULL || statement->connection == NULL ||
		statement->statement == NULL || statement->connection->database == NULL ||
		statement->connection->broken) {
		ib_fail(error, "prepared statement is unavailable");
		return NULL;
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		ib_fail(error, "query arguments are invalid");
		return NULL;
	}
	if (statement->connection->active_cursor != NULL) {
		ib_fail(error, "another result cursor is active on this connection");
		return NULL;
	}
	if (statement->statement_type != isc_info_sql_stmt_select &&
		statement->statement_type != isc_info_sql_stmt_exec_procedure) {
		ib_fail(error, "only SELECT and executable procedure statements are permitted");
		return NULL;
	}
	if (statement->statement_type == isc_info_sql_stmt_exec_procedure &&
		(statement->output == NULL || statement->output->sqld == 0)) {
		ib_fail(error, "procedure has no output; use Exec instead");
		return NULL;
	}
	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		ib_fail(error, "out of memory allocating prepared cursor state");
		return NULL;
	}
	cursor->connection = statement->connection;
	cursor->prepared = statement;
	cursor->statement = statement->statement;
	cursor->statement_type = statement->statement_type;
	if (statement->connection->transaction != NULL) {
		cursor->transaction = statement->connection->transaction;
		cursor->owns_transaction = 0;
	} else if (ib_start_transaction(statement->connection, &cursor->transaction,
		statement->statement_type == isc_info_sql_stmt_select ? 1 : 0,
		statement->statement_type == isc_info_sql_stmt_select ?
		"start read transaction" : "start write transaction", error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	} else {
		cursor->owns_transaction = 1;
	}
	cursor->input = ib_clone_sqlda(statement->input, error);
	if (cursor->input == NULL || ib_bind_input(cursor, bindings, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	cursor->output = ib_clone_sqlda(statement->output, error);
	if (cursor->output == NULL || ib_cursor_describe_metadata(cursor, error) != 0 ||
		ib_validate_output_types(cursor->output, error) != 0 ||
		ib_allocate_output(cursor, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute2(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input,
			statement->statement_type == isc_info_sql_stmt_exec_procedure ?
			cursor->output : NULL);
	}
	if (result != 0) {
		(void) ib_fail_status(error,
			statement->statement_type == isc_info_sql_stmt_exec_procedure ?
			"execute prepared procedure" : "execute prepared SELECT", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	cursor->procedure = statement->statement_type == isc_info_sql_stmt_exec_procedure;
	cursor->output_pending = cursor->procedure && cursor->output->sqld > 0;
	cursor->server_cursor_open = !cursor->procedure;
	statement->connection->active_cursor = cursor;
	return cursor;
}

int ib_statement_close(ib_statement *statement, char **error)
{
	return ib_statement_close_internal(statement, error);
}

int ib_cursor_next(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || cursor->statement == NULL || cursor->output == NULL) {
		return ib_fail(error, "cursor is unavailable");
	}
	if (cursor->procedure) {
		if (cursor->output_pending) {
			cursor->output_pending = 0;
			cursor->fetched = 1;
			return 1;
		}
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_fetch(status, &cursor->statement,
		ib_connection_dialect(cursor->connection),
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
	return ib_cursor_close_internal(cursor, error, 1);
}

int ib_cursor_abort(ib_cursor *cursor, char **error)
{
	return ib_cursor_close_internal(cursor, error, 0);
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

int ib_cursor_column_metadata(const ib_cursor *cursor, size_t index,
	ib_column_metadata *metadata, char **error)
{
	if (cursor == NULL || cursor->output == NULL || metadata == NULL ||
		index >= (size_t) cursor->output->sqld || cursor->metadata == NULL) {
		return ib_fail(error, "result column metadata is unavailable");
	}
	*metadata = cursor->metadata[index];
	return 0;
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

static size_t ib_utf8_prefix_length(const char *data, size_t length,
	size_t character_count)
{
	size_t offset;
	size_t count;

	offset = 0U;
	count = 0U;
	while (offset < length && count < character_count) {
		unsigned char first = (unsigned char) data[offset];
		size_t width = 1U;

		if (first < 0x80U) {
			width = 1U;
		} else if ((first & 0xe0U) == 0xc0U) {
			width = 2U;
		} else if ((first & 0xf0U) == 0xe0U) {
			width = 3U;
		} else if ((first & 0xf8U) == 0xf0U) {
			width = 4U;
		}
		if (width > length - offset) {
			width = 1U;
		} else if (width > 1U) {
			size_t continuation;
			for (continuation = 1U; continuation < width; continuation++) {
				if ((((unsigned char) data[offset + continuation]) & 0xc0U) != 0x80U) {
					width = 1U;
					break;
				}
			}
		}
		offset += width;
		count++;
	}
	return offset;
}

static size_t ib_text_character_count(const XSQLVAR *variable)
{
	size_t length;

	if (variable == NULL || variable->sqllen < 0) {
		return 0U;
	}
	length = (size_t) variable->sqllen;
	switch (variable->sqlsubtype) {
	case 3:  /* UNICODE_FSS */
		return length / 3U;
	case 5:  /* SJIS_0208 */
	case 6:  /* EUCJ_0208 */
	case 8:  /* UNICODE_BE */
	case 44: /* KSC_5601 */
	case 56: /* BIG_5 */
	case 57: /* GB_2312 */
	case 64: /* UNICODE_LE */
		return length / 2U;
	case IB_CHARSET_UTF8:
		return length / 4U;
	default:
		return length;
	}
}

static int ib_append_blob_data(char **data, size_t *length, size_t *capacity,
	const char *segment, size_t segment_length, char **error)
{
	char *resized;
	size_t needed;
	size_t new_capacity;

	if (data == NULL || length == NULL || capacity == NULL ||
		(segment_length != 0U && segment == NULL)) {
		return ib_fail(error, "BLOB result storage is unavailable");
	}
	if (*length > IB_MAX_BLOB_BUFFER ||
		segment_length > IB_MAX_BLOB_BUFFER - *length) {
		return ib_fail(error, "BLOB result exceeds the materialization limit");
	}
	needed = *length + segment_length;
	if (needed > *capacity) {
		new_capacity = *capacity == 0U ? IB_BLOB_SEGMENT_LENGTH : *capacity;
		while (new_capacity < needed) {
			if (new_capacity > IB_MAX_BLOB_BUFFER / 2U) {
				new_capacity = IB_MAX_BLOB_BUFFER;
				break;
			}
			new_capacity *= 2U;
		}
		if (new_capacity > IB_MAX_BLOB_BUFFER) {
			new_capacity = IB_MAX_BLOB_BUFFER;
		}
		resized = (char *) realloc(*data, new_capacity);
		if (resized == NULL) {
			return ib_fail(error, "out of memory materializing BLOB result");
		}
		*data = resized;
		*capacity = new_capacity;
	}
	if (segment_length != 0U) {
		memcpy(*data + *length, segment, segment_length);
		*length = needed;
	}
	return 0;
}

static char *ib_read_blob(ib_cursor *cursor, const XSQLVAR *variable,
	size_t *length, int *is_utf8, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ISC_QUAD blob_id;
	isc_blob_handle blob;
	ISC_BLOB_DESC_V2 source_descriptor;
	ISC_BLOB_DESC_V2 target_descriptor;
	unsigned char bpb[IB_BLOB_BPB_LENGTH];
	unsigned short bpb_length;
	char segment[IB_BLOB_SEGMENT_LENGTH];
	char *data;
	char *first_error;
	char *cleanup_error;
	size_t data_length;
	size_t capacity;
	int cleanup_failed;
	int use_bpb;

	if (length == NULL || cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		variable == NULL || variable->sqldata == NULL || is_utf8 == NULL) {
		(void) ib_fail(error, "BLOB result storage is unavailable");
		return NULL;
	}
	memcpy(&blob_id, variable->sqldata, sizeof(blob_id));
	*is_utf8 = 0;
	bpb_length = 0U;
	use_bpb = 0;
	if (variable->sqlsubtype == 1 && variable->sqlname_length > 0 &&
		variable->relname_length > 0) {
		if (ib_blob_lookup_descriptor(cursor, variable, &source_descriptor,
			error) != 0) {
			return NULL;
		}
		if (source_descriptor.blob_desc_charset != 0 &&
			source_descriptor.blob_desc_charset != 1) {
			ib_blob_descriptor(&target_descriptor, 1, IB_CHARSET_UTF8);
			memset(status, 0, sizeof(status));
			/* Read through the column's charset and expose UTF-8 to Go. */
			result = isc_blob_gen_bpb2(status, &target_descriptor,
				&source_descriptor, (unsigned short) sizeof(bpb), bpb,
				&bpb_length);
			if (result != 0) {
				(void) ib_fail_status(error, "generate output BLOB parameter block",
					status);
				return NULL;
			}
			use_bpb = 1;
			*is_utf8 = 1;
		}
	}
	blob = NULL;
	memset(status, 0, sizeof(status));
	if (use_bpb) {
		result = isc_open_blob2(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id, (short) bpb_length,
			(char *) bpb);
	} else {
		result = isc_open_blob(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id);
	}
	if (result != 0) {
		(void) ib_fail_status(error, "open output BLOB", status);
		return NULL;
	}
	data = NULL;
	data_length = 0U;
	capacity = 0U;
	for (;;) {
		unsigned short segment_length;

		segment_length = 0U;
		memset(status, 0, sizeof(status));
		result = isc_get_segment(status, &blob, &segment_length,
			(unsigned short) sizeof(segment), segment);
		if (result == isc_segstr_eof) {
			break;
		}
		if (result != 0 && result != isc_segment) {
			first_error = NULL;
			(void) ib_fail_status(&first_error, "read output BLOB segment", status);
			cleanup_error = NULL;
			cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
			if (cleanup_failed != 0) {
				cursor->connection->broken = 1;
			}
			ib_append_error(&first_error, cleanup_error);
			free(data);
			ib_give_error(error, first_error);
			return NULL;
		}
		first_error = NULL;
		if (ib_append_blob_data(&data, &data_length, &capacity, segment,
			(size_t) segment_length, &first_error) != 0) {
			cleanup_error = NULL;
			cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
			if (cleanup_failed != 0) {
				cursor->connection->broken = 1;
			}
			ib_append_error(&first_error, cleanup_error);
			free(data);
			ib_give_error(error, first_error);
			return NULL;
		}
	}
	cleanup_error = NULL;
	cleanup_failed = ib_blob_cleanup(&blob, 0, &cleanup_error);
	if (cleanup_failed != 0) {
		cursor->connection->broken = 1;
	}
	if (cleanup_failed != 0 || cleanup_error != NULL) {
		free(data);
		ib_give_error(error, cleanup_error);
		return NULL;
	}
	if (data == NULL) {
		data = (char *) malloc(1U);
		if (data == NULL) {
			(void) ib_fail(error, "out of memory materializing empty BLOB result");
			return NULL;
		}
		data[0] = '\0';
	}
	*length = data_length;
	return data;
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
	short connection_charset;
	short text_charset;
	size_t source_length;
	size_t character_count;
	size_t converted_length;
	char *converted;

	if (cursor == NULL || cursor->output == NULL || !cursor->fetched ||
		index >= (size_t) cursor->output->sqld || view == NULL) {
		return ib_fail(error, "result column is unavailable");
	}
	memset(view, 0, sizeof(*view));
	variable = &cursor->output->sqlvar[index];
	if (variable->sqldata == NULL ||
		((variable->sqltype & 1) != 0 && variable->sqlind == NULL)) {
		return ib_fail(error, "result column storage is unavailable");
	}
	free(((ib_cursor *) cursor)->converted_value);
	((ib_cursor *) cursor)->converted_value = NULL;
	free(((ib_cursor *) cursor)->materialized_value);
	((ib_cursor *) cursor)->materialized_value = NULL;
	connection_charset = IB_CHARSET_UTF8;
	if (cursor->connection != NULL && cursor->connection->charset != 0) {
		connection_charset = cursor->connection->charset;
	}
	if (variable->sqlind != NULL && *variable->sqlind < 0) {
		view->kind = IB_VALUE_NULL;
		return 0;
	}
	type = ib_sql_type(variable);
	text_charset = ib_text_charset(variable);
	switch (type) {
	case SQL_TEXT:
		source_length = (size_t) variable->sqllen;
		if (connection_charset == IB_CHARSET_UTF8) {
			character_count = ib_text_character_count(variable);
			source_length = ib_utf8_prefix_length(variable->sqldata,
				source_length, character_count);
		} else if (connection_charset != 0 && connection_charset != 1 &&
			text_charset != 1) {
			character_count = ib_text_character_count(variable);
			if (character_count < source_length) {
				source_length = character_count;
			}
		}
		if (connection_charset != 0 && connection_charset != IB_CHARSET_UTF8 &&
			text_charset != 1) {
			converted = ib_convert_to_utf8(variable->sqldata, source_length,
				connection_charset, &converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			((ib_cursor *) cursor)->converted_value = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = variable->sqldata;
			view->length = source_length;
		}
		view->kind = text_charset == 1 ? IB_VALUE_BYTES : IB_VALUE_STRING;
		return 0;
	case SQL_VARYING:
		memcpy(&varying_length, variable->sqldata, sizeof(varying_length));
		if (variable->sqllen < 0 || varying_length > (unsigned short) variable->sqllen) {
			return ib_fail(error, "InterBase returned an invalid varying value length");
		}
		source_length = (size_t) varying_length;
		if (connection_charset != 0 && connection_charset != IB_CHARSET_UTF8 &&
			text_charset != 1) {
			converted = ib_convert_to_utf8(variable->sqldata + sizeof(varying_length),
				source_length, connection_charset, &converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			((ib_cursor *) cursor)->converted_value = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = variable->sqldata + sizeof(varying_length);
			view->length = source_length;
		}
		view->kind = text_charset == 1 ? IB_VALUE_BYTES : IB_VALUE_STRING;
		return 0;
	case SQL_BLOB:
	{
		char *blob_data;
		size_t blob_length;
		int blob_is_utf8;

		blob_data = ib_read_blob((ib_cursor *) cursor, variable, &blob_length,
			&blob_is_utf8, error);
		if (blob_data == NULL) {
			return -1;
		}
		((ib_cursor *) cursor)->materialized_value = blob_data;
		if (!blob_is_utf8 && variable->sqlsubtype == 1 && connection_charset != 0 &&
			connection_charset != 1 && connection_charset != IB_CHARSET_UTF8) {
			converted = ib_convert_to_utf8(blob_data, blob_length,
				connection_charset, &converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			free(((ib_cursor *) cursor)->materialized_value);
			((ib_cursor *) cursor)->materialized_value = NULL;
			((ib_cursor *) cursor)->converted_value = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = ((ib_cursor *) cursor)->materialized_value;
			view->length = blob_length;
		}
		view->kind = variable->sqlsubtype == 1 ? IB_VALUE_STRING : IB_VALUE_BYTES;
		return 0;
	}
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
	case SQL_D_FLOAT:
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
