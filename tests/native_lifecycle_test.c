#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int fail_message_allocation;

static void *test_malloc(size_t size)
{
	return fail_message_allocation ? NULL : malloc(size);
}

/* Import the real SDK declarations under the stub names to check their ABI.
 * Only the exercised client entry points are replaced. No attachment is opened.
 */
#define malloc test_malloc
#define isc_start_transaction test_start_transaction
#define isc_rollback_transaction test_rollback_transaction
#define isc_dsql_free_statement test_free_statement
#define isc_detach_database test_detach_database
#define isc_dsql_sql_info test_sql_info
#include "../native.c"
#undef malloc

static int failures;
static int rollback_calls;
static int detach_calls;
static int fail_rollback;
static int fail_drop;
static int fail_detach;
static unsigned char statement_type;
static char handle_token;

static void check(int condition, const char *message)
{
	if (!condition) {
		fprintf(stderr, "native lifecycle test failed: %s\n", message);
		failures++;
	}
}

static ISC_STATUS status_result(ISC_STATUS *status, int failed)
{
	status[0] = isc_arg_gds;
	status[1] = failed ? isc_network_error : 0;
	status[2] = isc_arg_end;
	return status[1];
}

ISC_STATUS ISC_EXPORT_VARARG test_start_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction, short count, ...)
{
	const char expected[] = { isc_tpb_version3, isc_tpb_read_committed,
		isc_tpb_rec_version, isc_tpb_wait, isc_tpb_read };
	va_list args;
	isc_db_handle *database;
	int length;
	char *tpb;

	va_start(args, count);
	database = va_arg(args, isc_db_handle *);
	length = va_arg(args, int); /* short is promoted in the SDK varargs API. */
	tpb = va_arg(args, char *);
	va_end(args);
	check(count == 1 && database != NULL && *database != NULL,
		"transaction must use one attachment");
	check(length == (int) sizeof(expected) &&
		memcmp(tpb, expected, sizeof(expected)) == 0,
		"query did not request the read-only read-committed TPB");
	*transaction = NULL;
	return status_result(status, 1);
}

ISC_STATUS ISC_EXPORT test_rollback_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	rollback_calls++;
	if (!fail_rollback) {
		*transaction = NULL;
	}
	return status_result(status, fail_rollback);
}

ISC_STATUS ISC_EXPORT test_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option)
{
	check(option == DSQL_drop, "cursor cleanup must drop its statement");
	if (!fail_drop) {
		*statement = NULL;
	}
	return status_result(status, fail_drop);
}

ISC_STATUS ISC_EXPORT test_detach_database(ISC_STATUS *status,
	isc_db_handle *database)
{
	detach_calls++;
	if (!fail_detach) {
		*database = NULL;
	}
	return status_result(status, fail_detach);
}

ISC_STATUS ISC_EXPORT test_sql_info(ISC_STATUS *status,
	isc_stmt_handle *statement, short request_length, char *request,
	short response_length, char *response)
{
	(void) statement;
	check(request_length == 1 && request[0] == isc_info_sql_stmt_type,
		"gate must inspect the server statement type");
	if (response_length < 8) {
		return status_result(status, 1);
	}
	memset(response, 0, (size_t) response_length);
	response[0] = isc_info_sql_stmt_type;
	response[1] = 4;
	response[3] = (char) statement_type;
	response[7] = isc_info_end;
	return status_result(status, 0);
}

static ib_cursor *new_cursor(ib_connection *connection)
{
	ib_cursor *cursor = calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		abort();
	}
	cursor->connection = connection;
	cursor->transaction = &handle_token;
	cursor->owns_transaction = 1;
	connection->active_cursor = cursor;
	return cursor;
}

static void test_start_failure(void)
{
	ib_connection connection = {0};
	ib_bindings bindings = {0};
	const char query[] = "SELECT 1 FROM RDB$DATABASE";
	char *error = NULL;
	connection.dialect = SQL_DIALECT_V5;
	connection.database = &handle_token;
	rollback_calls = 0;
	check(ib_connection_query(&connection, query, sizeof(query) - 1U,
		&bindings, &error) == NULL, "start failure returned a cursor");
	check(error != NULL, "start failure lost its error");
	check(ib_connection_is_broken(&connection), "start failure did not invalidate attachment");
	check(connection.active_cursor == NULL && rollback_calls == 0,
		"start failure without a handle attempted rollback or retained a cursor");
	ib_error_free(error);
}

static void test_cursor_cleanup(int drop_failure)
{
	ib_connection connection = {0};
	ib_cursor *cursor;
	char *error = NULL;
	int result;
	connection.dialect = SQL_DIALECT_V5;
	cursor = new_cursor(&connection);
	cursor->statement = &handle_token;
	fail_drop = drop_failure;
	fail_rollback = !drop_failure;
	rollback_calls = 0;
	fail_message_allocation = 1;
	result = ib_cursor_close(cursor, &error);
	fail_message_allocation = 0;
	check(result == -1, "cursor cleanup failure with no message returned success");
	check(error == NULL, "error-message allocation injection was ineffective");
	check(ib_connection_is_broken(&connection), "message-less cleanup did not invalidate attachment");
	check(connection.active_cursor == NULL && rollback_calls == 1,
		"cursor cleanup did not release ownership and attempt rollback");
	ib_error_free(error);
	fail_drop = fail_rollback = 0;
}

static void test_failed_query_cleanup(int preserve_primary)
{
	ib_connection connection = {0};
	ib_cursor *cursor;
	char *error = NULL;
	connection.dialect = SQL_DIALECT_V5;
	cursor = new_cursor(&connection);
	if (preserve_primary) {
		(void) ib_fail(&error, "original query failure");
		if (error == NULL) {
			abort();
		}
	}
	fail_rollback = 1;
	fail_message_allocation = 1;
	ib_failed_query_cleanup(cursor, &error);
	fail_message_allocation = 0;
	check(ib_connection_is_broken(&connection),
		"failed-query cleanup lost broken state when its error message was NULL");
	check(preserve_primary ? error != NULL && strcmp(error, "original query failure") == 0 : error == NULL,
		"message-less cleanup changed the original query error");
	ib_error_free(error);
	connection.broken = 0;
	cursor = new_cursor(&connection);
	fail_message_allocation = 1;
	ib_failed_query_cleanup(cursor, NULL);
	fail_message_allocation = 0;
	check(ib_connection_is_broken(&connection),
		"cleanup without an error destination lost broken state");
	fail_rollback = 0;
}

static void test_connection_cleanup(int rollback_failure, int detach_failure)
{
	ib_connection *connection = calloc(1U, sizeof(*connection));
	char *error = NULL;
	int result;
	if (connection == NULL) {
		abort();
	}
	connection->dialect = SQL_DIALECT_V5;
	connection->database = &handle_token;
	if (rollback_failure) {
		(void) new_cursor(connection);
	}
	fail_rollback = rollback_failure;
	fail_detach = detach_failure;
	detach_calls = 0;
	fail_message_allocation = 1;
	result = ib_connection_close(connection, &error);
	fail_message_allocation = 0;
	check(result == ((rollback_failure || detach_failure) ? -1 : 0),
		"connection cleanup result depends on error-message allocation");
	check(error == NULL && detach_calls == 1,
		"connection cleanup must attempt detach even after message-less cursor failure");
	ib_error_free(error);
	fail_rollback = fail_detach = 0;
}

static void test_explicit_rollback_reports_failure_without_diagnostic(void)
{
	ib_connection connection = {0};
	char *error = NULL;
	int result;

	connection.dialect = SQL_DIALECT_V5;
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	fail_rollback = 1;
	fail_message_allocation = 1;
	result = ib_connection_rollback(&connection, &error);
	fail_message_allocation = 0;
	check(result == -1, "rollback failure was hidden by diagnostic allocation failure");
	check(connection.broken, "rollback failure did not invalidate the connection");
	check(connection.transaction == NULL, "rollback failure retained a stale transaction handle");
	check(error == NULL, "diagnostic allocation unexpectedly succeeded during rollback failure");
	ib_error_free(error);
	fail_rollback = 0;
}

static void test_select_gate(void)
{
	const unsigned char types[] = { isc_info_sql_stmt_select,
		isc_info_sql_stmt_insert, isc_info_sql_stmt_update,
		isc_info_sql_stmt_delete, isc_info_sql_stmt_ddl,
		isc_info_sql_stmt_exec_procedure, isc_info_sql_stmt_select_for_upd };
	ib_cursor cursor = {0};
	size_t i;
	cursor.statement = &handle_token;
	for (i = 0; i < sizeof(types) / sizeof(types[0]); i++) {
		char *error = NULL;
		int result;
		statement_type = types[i];
		result = ib_statement_is_select(&cursor, &error);
		check(result == (i == 0 ? 0 : -1), "statement gate accepted a non-SELECT or rejected SELECT");
		check((error == NULL) == (i == 0), "statement gate returned the wrong error state");
		ib_error_free(error);
	}
}

int main(void)
{
	test_start_failure();
	test_cursor_cleanup(0);
	test_cursor_cleanup(1);
	test_failed_query_cleanup(0);
	test_failed_query_cleanup(1);
	test_connection_cleanup(0, 1);
	test_connection_cleanup(1, 0);
	test_connection_cleanup(1, 1);
	test_connection_cleanup(0, 0);
	test_explicit_rollback_reports_failure_without_diagnostic();
	test_select_gate();
	if (failures != 0) {
		return EXIT_FAILURE;
	}
	puts("native lifecycle tests passed");
	return EXIT_SUCCESS;
}
