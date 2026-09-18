#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <ibase.h>
#include "../native.h"

static void check(int condition, const char *message);

ISC_STATUS ISC_EXPORT test_dsql_allocate_statement(ISC_STATUS *status,
	isc_db_handle *database, isc_stmt_handle *statement);
ISC_STATUS ISC_EXPORT test_dsql_prepare(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short query_length, const char *query, unsigned short dialect,
	XSQLDA *output);
ISC_STATUS ISC_EXPORT test_dsql_describe_bind(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input);
ISC_STATUS ISC_EXPORT test_dsql_describe(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output);
ISC_STATUS ISC_EXPORT test_dsql_sql_info(ISC_STATUS *status,
	isc_stmt_handle *statement, short request_length, char *request,
	short response_length, char *response);
ISC_STATUS ISC_EXPORT test_dsql_execute(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input);
ISC_STATUS ISC_EXPORT test_dsql_execute2(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input, XSQLDA *output);
ISC_STATUS ISC_EXPORT test_dsql_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option);
ISC_STATUS ISC_EXPORT test_dsql_set_cursor_name(ISC_STATUS *status,
	isc_stmt_handle *statement, char *name, unsigned short reserved);

#define isc_dsql_allocate_statement test_dsql_allocate_statement
#define isc_dsql_prepare test_dsql_prepare
#define isc_dsql_describe_bind test_dsql_describe_bind
#define isc_dsql_describe test_dsql_describe
#define isc_dsql_sql_info test_dsql_sql_info
#define isc_dsql_execute test_dsql_execute
#define isc_dsql_execute2 test_dsql_execute2
#define isc_dsql_free_statement test_dsql_free_statement
#define isc_dsql_set_cursor_name test_dsql_set_cursor_name
#include "../native.c"
#undef isc_dsql_allocate_statement
#undef isc_dsql_prepare
#undef isc_dsql_describe_bind
#undef isc_dsql_describe
#undef isc_dsql_sql_info
#undef isc_dsql_execute
#undef isc_dsql_execute2
#undef isc_dsql_free_statement
#undef isc_dsql_set_cursor_name

static int set_cursor_name_calls;
static unsigned short set_cursor_name_reserved;
static char set_cursor_name_value[128];
static char statement_token;
static char transaction_token;
static char database_token;
static ib_cancel_slot *expected_cancel_slot;
static isc_stmt_handle expected_cancel_statement;
static unsigned char expected_statement_type;

ISC_STATUS ISC_EXPORT test_dsql_set_cursor_name(ISC_STATUS *status,
	isc_stmt_handle *statement, char *name, unsigned short reserved)
{
	(void) statement;
	set_cursor_name_calls++;
	set_cursor_name_reserved = reserved;
	if (name != NULL) {
		(void) snprintf(set_cursor_name_value, sizeof(set_cursor_name_value), "%s", name);
	} else {
		set_cursor_name_value[0] = '\0';
	}
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native direct test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static ISC_STATUS status_result(ISC_STATUS *status, ISC_STATUS result)
{
	status[0] = isc_arg_gds;
	status[1] = result;
	status[2] = isc_arg_end;
	return result;
}

static void check_cancel_publication(isc_stmt_handle *statement, const char *message)
{
	check(expected_cancel_slot != NULL && statement != NULL &&
		expected_cancel_statement != NULL && *statement == expected_cancel_statement &&
		expected_cancel_slot->active && expected_cancel_slot->published &&
		expected_cancel_slot->statement == expected_cancel_statement,
		message);
}

static void check_cancel_complete(const char *message)
{
	check(expected_cancel_slot == NULL ||
		(!expected_cancel_slot->active && !expected_cancel_slot->published &&
		expected_cancel_slot->operation_complete &&
		expected_cancel_slot->cancel_users == 0U &&
		expected_cancel_slot->cancel_callers == 0U), message);
}

ISC_STATUS ISC_EXPORT test_dsql_allocate_statement(ISC_STATUS *status,
	isc_db_handle *database, isc_stmt_handle *statement)
{
	check(database != NULL && *database != NULL && statement != NULL,
		"transient statement allocation received invalid handles");
	*statement = (isc_stmt_handle) &statement_token;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_prepare(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short query_length, const char *query, unsigned short dialect,
	XSQLDA *output)
{
	(void) transaction;
	(void) query_length;
	(void) query;
	(void) dialect;
	(void) output;
	check_cancel_publication(statement,
		"transient prepare did not publish its actual statement handle");
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_describe_bind(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input)
{
	(void) statement;
	(void) dialect;
	input->sqld = 0;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_describe(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output)
{
	(void) statement;
	(void) dialect;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_LONG;
	output->sqlvar[0].sqllen = (short) sizeof(ISC_LONG);
	output->sqlvar[0].sqlname_length = 1;
	output->sqlvar[0].sqlname[0] = 'X';
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_sql_info(ISC_STATUS *status,
	isc_stmt_handle *statement, short request_length, char *request,
	short response_length, char *response)
{
	(void) statement;
	(void) response_length;
	memset(response, 0, (size_t) response_length);
	if (request_length == 1 && request[0] == isc_info_sql_stmt_type) {
		response[0] = isc_info_sql_stmt_type;
		response[1] = 4;
		response[3] = (char) expected_statement_type;
		response[7] = isc_info_end;
		return status_result(status, 0);
	}
	if (request_length == 2 && request[0] == isc_info_sql_records) {
		response[0] = isc_info_sql_records;
		response[1] = 8;
		response[3] = isc_info_req_insert_count;
		response[4] = 4;
		response[6] = 1;
		response[10] = isc_info_end;
		return status_result(status, 0);
	}
	return status_result(status, isc_bad_stmt_handle);
}

ISC_STATUS ISC_EXPORT test_dsql_execute(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input)
{
	(void) transaction;
	(void) dialect;
	(void) input;
	check_cancel_publication(statement,
		"transient execute did not publish its actual statement handle");
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_execute2(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input, XSQLDA *output)
{
	(void) transaction;
	(void) dialect;
	(void) input;
	(void) output;
	check_cancel_publication(statement,
		"transient execute2 did not publish its actual statement handle");
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option)
{
	check(option != DSQL_cancel, "transient cleanup unexpectedly requested cancellation");
	check_cancel_complete("transient cleanup began before cancellation completed");
	if (statement != NULL) {
		*statement = NULL;
	}
	return status_result(status, 0);
}

static void test_indicator_flags(void)
{
	ib_cursor cursor;
	XSQLDA *output;
	short indicator;
	uint16_t got;
	char *error = NULL;

	memset(&cursor, 0, sizeof(cursor));
	output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	check(output != NULL, "output allocation failed");
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqlind = &indicator;
	cursor.output = output;

	indicator = (short) -1;
	check(ib_cursor_column_indicator(&cursor, 0U, &got, &error) == 0,
		"plain NULL indicator read failed");
	check(got == (uint16_t) SQLIND_NULL, "plain NULL indicator was not normalized");
	ib_error_free(error);
	error = NULL;

	indicator = (short) (SQLIND_NULL | SQLIND_UPDATE);
	check(ib_cursor_column_indicator(&cursor, 0U, &got, &error) == 0,
		"NULL/update indicator read failed");
	check(got == ((uint16_t) SQLIND_NULL | (uint16_t) SQLIND_UPDATE),
		"NULL/update indicator lost its change flag");
	ib_error_free(error);
	error = NULL;

	indicator = (short) (SQLIND_INSERT | SQLIND_UPDATE);
	check(ib_cursor_column_indicator(&cursor, 0U, &got, &error) == 0,
		"change indicator read failed");
	check(got == (uint16_t) (SQLIND_INSERT | SQLIND_UPDATE),
		"change indicator bits were not preserved");
	ib_error_free(error);
	free(output);
}

static void test_cursor_name_reserved_argument(void)
{
	ib_connection connection;
	ib_cursor cursor;
	char database_token;
	char statement_token;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.charset = IB_CHARSET_UTF8;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.statement = &statement_token;
	set_cursor_name_calls = 0;
	set_cursor_name_reserved = 99U;
	set_cursor_name_value[0] = '\0';

	check(ib_cursor_set_name(&cursor, "direct_cursor", strlen("direct_cursor"), &error) == 0,
		"cursor name call failed");
	check(error == NULL, "cursor name call returned an unexpected error");
	check(set_cursor_name_calls == 1, "cursor name native call count was wrong");
	check(set_cursor_name_reserved == 0U, "cursor name reserved argument was not zero");
	check(strcmp(set_cursor_name_value, "direct_cursor") == 0,
		"cursor name was not passed to the native call");
}

static ib_cancel_slot *new_cancel_slot(uint64_t *generation)
{
	char *error = NULL;
	ib_cancel_slot *slot = ib_cancel_slot_new(&error);

	check(slot != NULL && error == NULL, "transient cancellation slot allocation failed");
	*generation = ib_cancel_slot_begin(slot, &error);
	check(*generation != 0U && error == NULL,
		"transient cancellation slot begin failed");
	ib_error_free(error);
	return slot;
}

static void test_transient_prepare_publishes_actual_handle(void)
{
	const char query[] = "INSERT INTO T (ID) VALUES (1)";
	ib_connection connection;
	ib_statement *statement;
	ib_cancel_slot *slot;
	uint64_t generation;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.transaction = &transaction_token;
	connection.dialect = SQL_DIALECT_V6;
	expected_statement_type = isc_info_sql_stmt_insert;
	slot = new_cancel_slot(&generation);
	expected_cancel_slot = slot;
	expected_cancel_statement = (isc_stmt_handle) &statement_token;
	statement = ib_statement_prepare(&connection, query, sizeof(query) - 1U,
		slot, generation, &error);
	check(statement != NULL && error == NULL,
		"transient prepare did not return a statement");
	check_cancel_complete("transient prepare returned with an active cancellation slot");
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"transient prepared statement cleanup failed");
	ib_error_free(error);
	expected_cancel_slot = NULL;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
}

static void test_transient_exec_publishes_actual_handle(void)
{
	const char query[] = "INSERT INTO T (ID) VALUES (1)";
	ib_connection connection;
	ib_bindings bindings;
	ib_cancel_slot *slot;
	uint64_t generation;
	int64_t affected = 0;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.transaction = &transaction_token;
	connection.dialect = SQL_DIALECT_V6;
	memset(&bindings, 0, sizeof(bindings));
	expected_statement_type = isc_info_sql_stmt_insert;
	slot = new_cancel_slot(&generation);
	expected_cancel_slot = slot;
	expected_cancel_statement = (isc_stmt_handle) &statement_token;
	check(ib_connection_exec(&connection, query, sizeof(query) - 1U,
		&bindings, &affected, 0, slot, generation, &error) == 0 &&
		error == NULL && affected == 1,
		"transient execute did not return its affected-row result");
	check_cancel_complete("transient execute returned with an active cancellation slot");
	ib_error_free(error);
	expected_cancel_slot = NULL;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
}

static void test_transient_query_publishes_execute2_handle(void)
{
	const char query[] = "SELECT 1 FROM RDB$DATABASE";
	ib_connection connection;
	ib_bindings bindings;
	ib_cursor *cursor;
	ib_cancel_slot *slot;
	uint64_t generation;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.transaction = &transaction_token;
	connection.dialect = SQL_DIALECT_V6;
	memset(&bindings, 0, sizeof(bindings));
	expected_statement_type = isc_info_sql_stmt_select;
	slot = new_cancel_slot(&generation);
	expected_cancel_slot = slot;
	expected_cancel_statement = (isc_stmt_handle) &statement_token;
	cursor = ib_connection_query(&connection, query, sizeof(query) - 1U,
		&bindings, 0, slot, generation, &error);
	check(cursor != NULL && error == NULL,
		"transient query did not return a cursor");
	check_cancel_complete("transient query returned with an active cancellation slot");
	check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
		"transient query cursor cleanup failed");
	ib_error_free(error);
	expected_cancel_slot = NULL;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
}

int main(void)
{
	test_indicator_flags();
	test_cursor_name_reserved_argument();
	test_transient_prepare_publishes_actual_handle();
	test_transient_exec_publishes_actual_handle();
	test_transient_query_publishes_execute2_handle();
	(void) puts("native direct tests passed");
	return EXIT_SUCCESS;
}
