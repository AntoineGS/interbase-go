#include <stdarg.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int fail_message_allocation;
static int fail_allocation_after = -1;

static void *test_malloc(size_t size)
{
	if (fail_message_allocation) {
		return NULL;
	}
	if (fail_allocation_after == 0) {
		fail_allocation_after = -1;
		return NULL;
	}
	if (fail_allocation_after > 0) {
		fail_allocation_after--;
	}
	return malloc(size);
}

/* Import the real SDK declarations under the stub names to check their ABI.
 * Only the exercised client entry points are replaced. No attachment is opened.
 */
#define malloc test_malloc
#define isc_start_transaction test_start_transaction
#define isc_commit_transaction test_commit_transaction
#define isc_rollback_transaction test_rollback_transaction
#define isc_dsql_free_statement test_free_statement
#define isc_detach_database test_detach_database
#define isc_create_database test_create_database
#define isc_drop_database test_drop_database
#define isc_dsql_sql_info test_sql_info
#include "../native.c"
#undef malloc

static int failures;
static int rollback_calls;
static int detach_calls;
static int fail_rollback;
static int fail_commit;
static int fail_drop;
static int fail_detach;
static int fail_start;
static int start_returns_handle_on_failure;
static int start_returns_handle_on_success;
static int create_calls;
static int drop_calls;
static int fail_create;
static int fail_drop_database;
static int consume_drop_handle_on_error;
static int create_returns_handle;
static unsigned char observed_dpb[1024];
static size_t observed_dpb_length;
static short observed_database_length;
static short observed_database_type;
static char observed_database[256];
static const char *expected_tpb;
static size_t expected_tpb_length;
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
	const char fallback[] = { isc_tpb_version3, isc_tpb_read, isc_tpb_wait,
		isc_tpb_read_committed, isc_tpb_rec_version };
	const char *wanted = expected_tpb == NULL ? fallback : expected_tpb;
	size_t wanted_length = expected_tpb == NULL ? sizeof(fallback) : expected_tpb_length;
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
	check(length == (int) wanted_length &&
		memcmp(tpb, wanted, wanted_length) == 0,
		"transaction did not request the expected TPB");
	if ((fail_start && start_returns_handle_on_failure) ||
		(!fail_start && start_returns_handle_on_success)) {
		*transaction = &handle_token;
	} else {
		*transaction = NULL;
	}
	return status_result(status, fail_start);
}

ISC_STATUS ISC_EXPORT test_commit_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	if (!fail_commit) {
		*transaction = NULL;
	}
	return status_result(status, fail_commit);
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

ISC_STATUS ISC_EXPORT test_create_database(ISC_STATUS *status,
	short database_length, char *database, isc_db_handle *db_handle,
	short dpb_length, char *dpb, short database_type)
{
	create_calls++;
	observed_database_length = database_length;
	observed_database_type = database_type;
	if ((size_t) database_length < sizeof(observed_database)) {
		memcpy(observed_database, database, (size_t) database_length);
		observed_database[database_length] = '\0';
	}
	if ((size_t) dpb_length <= sizeof(observed_dpb)) {
		memcpy(observed_dpb, dpb, (size_t) dpb_length);
		observed_dpb_length = (size_t) dpb_length;
	}
	if (!fail_create || create_returns_handle) {
		*db_handle = &handle_token;
	}
	return status_result(status, fail_create);
}

ISC_STATUS ISC_EXPORT test_drop_database(ISC_STATUS *status,
	isc_db_handle *database)
{
	drop_calls++;
	if (!fail_drop_database || consume_drop_handle_on_error) {
		*database = NULL;
	}
	return status_result(status, fail_drop_database);
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
	ib_cursor_register(cursor);
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
	fail_start = 1;
	check(ib_connection_query(&connection, query, sizeof(query) - 1U,
		&bindings, 0, &error) == NULL, "start failure returned a cursor");
	check(error != NULL, "start failure lost its error");
	check(ib_connection_is_broken(&connection), "start failure did not invalidate attachment");
	check(connection.cursors == NULL && rollback_calls == 0,
		"start failure without a handle attempted rollback or retained a cursor");
	ib_error_free(error);
	fail_start = 0;
}

static void test_direct_transaction_start_failure_retains_handle(void)
{
	const char tpb[] = {isc_tpb_version3, isc_tpb_write, isc_tpb_wait};
	ib_connection connection;
	ib_transaction *transaction;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	fail_start = 1;
	start_returns_handle_on_failure = 1;
	fail_rollback = 1;
	expected_tpb = tpb;
	expected_tpb_length = sizeof(tpb);
	transaction = ib_transaction_begin(&connection, tpb, sizeof(tpb), &error);
	check(transaction != NULL && error != NULL,
		"direct transaction start failure lost a returned native wrapper");
	check(connection.broken && transaction->view.transaction == &handle_token &&
		ib_transaction_handle_state(transaction) == IB_HANDLE_LIVE,
		"direct transaction start failure did not preserve the live handle");
	ib_error_free(error);
	error = NULL;
	check(ib_transaction_rollback_cleanup(transaction, &error) != 0 && error != NULL &&
		ib_transaction_handle_state(transaction) == IB_HANDLE_LIVE,
		"cleanup rollback failure did not retain the live direct transaction handle");
	ib_error_free(error);
	error = NULL;
	fail_rollback = 0;
	check(ib_transaction_rollback_cleanup(transaction, &error) == 0 && error == NULL &&
		ib_transaction_handle_state(transaction) == IB_HANDLE_CONSUMED,
		"cleanup rollback retry did not consume the direct transaction handle");
	ib_error_free(error);
	ib_transaction_free(transaction);
	fail_start = 0;
	start_returns_handle_on_failure = 0;
	expected_tpb = NULL;
	expected_tpb_length = 0U;
}

static void test_direct_transaction_completion_failure_retains_handle(void)
{
	const char tpb[] = {isc_tpb_version3, isc_tpb_write, isc_tpb_wait};
	ib_connection connection;
	ib_transaction *transaction;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	start_returns_handle_on_success = 1;
	expected_tpb = tpb;
	expected_tpb_length = sizeof(tpb);
	transaction = ib_transaction_begin(&connection, tpb, sizeof(tpb), &error);
	check(transaction != NULL && error == NULL,
		"direct transaction completion setup failed");
	ib_error_free(error);
	error = NULL;
	fail_commit = 1;
	check(ib_transaction_commit(transaction, &error) != 0 && error != NULL &&
		ib_transaction_handle_state(transaction) == IB_HANDLE_LIVE,
		"commit failure did not preserve the live direct transaction handle");
	ib_error_free(error);
	error = NULL;
	fail_commit = 0;
	check(ib_transaction_rollback_cleanup(transaction, &error) == 0 && error == NULL &&
		ib_transaction_handle_state(transaction) == IB_HANDLE_CONSUMED,
		"commit failure could not be resolved by cleanup rollback");
	ib_error_free(error);
	ib_transaction_free(transaction);
	start_returns_handle_on_success = 0;
	expected_tpb = NULL;
	expected_tpb_length = 0U;
}

static void test_configured_and_explicit_tpb(void)
{
	const char configured_read[] = { isc_tpb_version3, isc_tpb_read,
		isc_tpb_nowait, isc_tpb_concurrency };
	const char configured[] = { isc_tpb_version3, isc_tpb_write,
		isc_tpb_nowait, isc_tpb_concurrency };
	const char explicit_tpb[] = { isc_tpb_version3, isc_tpb_read,
		isc_tpb_wait, isc_tpb_consistency };
	ib_connection connection = {0};
	char *error = NULL;

	connection.database = &handle_token;
	expected_tpb = configured_read;
	expected_tpb_length = sizeof(configured_read);
	check(ib_connection_set_default_tpbs(&connection, configured_read,
		sizeof(configured_read), configured, sizeof(configured), &error) == 0 &&
		error == NULL, "default TPB configuration failed");
	check(ib_start_transaction(&connection, &connection.transaction, 1,
		"test read-only transaction", &error) == 0 && error == NULL,
		"configured read-only TPB was not used");
	ib_error_free(error);
	error = NULL;

	expected_tpb = configured;
	expected_tpb_length = sizeof(configured);
	check(ib_connection_begin(&connection, 0, &error) == 0 && error == NULL,
		"configured default TPB was not used");
	ib_error_free(error);
	error = NULL;

	expected_tpb = explicit_tpb;
	expected_tpb_length = sizeof(explicit_tpb);
	check(ib_connection_begin_tpb(&connection, explicit_tpb,
		sizeof(explicit_tpb), &error) == 0 && error == NULL,
		"explicit TPB was not used");
	ib_error_free(error);
	free(connection.read_tpb);
	free(connection.write_tpb);
	connection.read_tpb = NULL;
	connection.write_tpb = NULL;
	expected_tpb = NULL;
	expected_tpb_length = 0U;
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
	check(connection.cursors == NULL && rollback_calls == 1,
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
	connection.transaction_read_only = 1;
	fail_rollback = 1;
	fail_message_allocation = 1;
	result = ib_connection_rollback(&connection, &error);
	fail_message_allocation = 0;
	check(result == -1, "rollback failure was hidden by diagnostic allocation failure");
	check(connection.broken, "rollback failure did not invalidate the connection");
	check(connection.transaction == &handle_token && connection.transaction_read_only == 1,
		"rollback failure did not retain the live transaction state");
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
		check(result == (i == 0 || i == sizeof(types) / sizeof(types[0]) - 1U ? 0 : -1),
			"statement gate accepted a non-SELECT or rejected SELECT");
		check((error == NULL) == (i == 0 || i == sizeof(types) / sizeof(types[0]) - 1U),
			"statement gate returned the wrong error state");
		ib_error_free(error);
	}
}

static int dpb_has(unsigned char code, const unsigned char *value, size_t value_length)
{
	size_t offset = 1U;
	while (offset + 2U <= observed_dpb_length) {
		unsigned char item_code = observed_dpb[offset++];
		size_t length = observed_dpb[offset++];
		if (offset + length > observed_dpb_length) {
			return 0;
		}
		if (item_code == code && length == value_length &&
			memcmp(observed_dpb + offset, value, value_length) == 0) {
			return 1;
		}
		offset += length;
	}
	return 0;
}

static void reset_create_stubs(void)
{
	create_calls = 0;
	drop_calls = 0;
	fail_create = 0;
	fail_drop_database = 0;
	consume_drop_handle_on_error = 0;
	create_returns_handle = 0;
	observed_dpb_length = 0U;
	observed_database_length = 0;
	observed_database_type = -1;
	memset(observed_database, 0, sizeof(observed_database));
	memset(observed_dpb, 0, sizeof(observed_dpb));
	detach_calls = 0;
	fail_detach = 0;
	fail_allocation_after = -1;
}

static void test_create_database_dpb_and_drop_ownership(void)
{
	const unsigned char page_size[] = {0, 16, 0, 0};
	const unsigned char overwrite_disabled[] = {0};
	const unsigned char enabled[] = {1};
	const unsigned char dialect[] = {3};
	const unsigned char connect_timeout[] = {0x04, 0x03, 0x02, 0x01};
	ib_connection *connection;
	char *error = NULL;
	const char database[] = "/tmp/interbase-go-'owned'.ib";

	reset_create_stubs();
	connection = ib_connection_create(database, sizeof(database) - 1U,
		"SYSDBA", 6U, "master'key", 10U, "R\"OLE", 5U,
		"encrypted", 9U, "system-secret", 13U, "UTF8", 4U, 3, 4096,
		0x01020304U, &error);
	check(connection != NULL && error == NULL, "native create did not return a connection");
	check(create_calls == 1 && observed_database_length == (short) (sizeof(database) - 1U) &&
		strcmp(observed_database, database) == 0 && observed_database_type == 0,
		"native create changed the counted database path or type");
	check(observed_dpb_length > 0U && observed_dpb[0] == isc_dpb_version1,
		"native create did not send a versioned DPB");
	check(dpb_has(isc_dpb_user_name, (const unsigned char *) "SYSDBA", 6U),
		"native create omitted the user DPB item");
	check(dpb_has(isc_dpb_password, (const unsigned char *) "master'key", 10U),
		"native create did not preserve the password bytes");
	check(dpb_has(isc_dpb_sql_role_name, (const unsigned char *) "R\"OLE", 5U),
		"native create omitted the role DPB item");
	check(dpb_has(isc_dpb_lc_ctype, (const unsigned char *) "UTF8", 4U),
		"native create omitted the charset DPB item");
	check(dpb_has(isc_dpb_sql_dialect, dialect, sizeof(dialect)),
		"native create omitted the SQL dialect DPB item");
	check(dpb_has(isc_dpb_connect_timeout, connect_timeout, sizeof(connect_timeout)),
		"native create omitted the connect-timeout DPB item");
	check(dpb_has(isc_dpb_page_size, page_size, sizeof(page_size)),
		"native create omitted the page-size DPB item");
	check(dpb_has(isc_dpb_overwrite, overwrite_disabled, sizeof(overwrite_disabled)),
		"native create did not explicitly disable overwrite");
	check(dpb_has(isc_dpb_admin_option, enabled, sizeof(enabled)),
		"native create omitted the admin-option DPB item");
	ib_error_free(error);
	error = NULL;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) == 0 &&
			error == NULL && consumed, "successful native drop returned an error");
	}
	check(drop_calls == 1 && detach_calls == 0,
		"successful native drop detached or ran the wrong number of calls");
	ib_error_free(error);
	error = NULL;
}

static void test_create_failure_cleans_returned_handle(void)
{
	char *error = NULL;
	reset_create_stubs();
	fail_create = 1;
	create_returns_handle = 1;
	check(ib_connection_create("new.ib", 6U, "SYSDBA", 6U, "password", 8U,
		NULL, 0U, NULL, 0U, NULL, 0U, "UTF8", 4U, 3, 0, 0U, &error) == NULL,
		"failed native create returned a connection");
	check(error != NULL && detach_calls == 1,
		"failed native create did not detach a returned handle");
	ib_error_free(error);
	error = NULL;
}

static void test_default_tpb_failure_preserves_created_connection(void)
{
	const char read_tpb[] = {isc_tpb_version3, isc_tpb_read, isc_tpb_wait};
	const char write_tpb[] = {isc_tpb_version3, isc_tpb_write, isc_tpb_wait};
	char *error = NULL;
	ib_connection *connection;
	int consumed = 0;

	reset_create_stubs();
	connection = ib_connection_create("new.ib", 6U, "SYSDBA", 6U,
		"password", 8U, NULL, 0U, NULL, 0U, NULL, 0U, "UTF8", 4U, 3, 0, 0U,
		&error);
	check(connection != NULL && error == NULL,
		"default-TPB failure setup did not create a connection");
	if (connection == NULL) {
		ib_error_free(error);
		return;
	}
	ib_error_free(error);
	error = NULL;

	/* Let the read TPB copy succeed and fail the write TPB copy. */
	fail_allocation_after = 1;
	check(ib_connection_set_default_tpbs(connection, read_tpb, sizeof(read_tpb),
		write_tpb, sizeof(write_tpb), &error) != 0 && error != NULL,
		"default-TPB allocation failure returned success");
	fail_allocation_after = -1;
	check(connection->database == &handle_token && connection->read_tpb == NULL &&
		connection->write_tpb == NULL,
		"default-TPB allocation failure lost the created connection");
	ib_error_free(error);
	error = NULL;

	check(ib_connection_drop(connection, &consumed, &error) == 0 &&
		error == NULL && consumed,
		"created connection was not recoverable after default-TPB failure");
	ib_error_free(error);
}

static void test_failed_drop_preserves_connection(void)
{
	char *error = NULL;
	ib_connection *connection;
	reset_create_stubs();
	connection = ib_connection_create("new.ib", 6U, "SYSDBA", 6U, "password", 8U,
		NULL, 0U, NULL, 0U, NULL, 0U, "UTF8", 4U, 3, 0, 0U, &error);
	check(connection != NULL && error == NULL, "drop-preservation setup failed");
	ib_error_free(error);
	error = NULL;
	fail_drop_database = 1;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) != 0 &&
			error != NULL && !consumed, "failed native drop returned success");
	}
	check(drop_calls == 1 && connection->database == &handle_token && !connection->broken,
		"failed native drop did not preserve the native connection");
	ib_error_free(error);
	error = NULL;
	fail_drop_database = 0;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) == 0 &&
			error == NULL && consumed, "retrying native drop failed after a preserved failure");
	}
	check(drop_calls == 2 && detach_calls == 0,
		"retrying native drop used detach or the wrong number of calls");
	ib_error_free(error);
}

static void test_failed_drop_consumes_handle(void)
{
	char *error = NULL;
	int consumed = 0;
	ib_connection *connection;

	reset_create_stubs();
	connection = ib_connection_create("new.ib", 6U, "SYSDBA", 6U, "password", 8U,
		NULL, 0U, NULL, 0U, NULL, 0U, "UTF8", 4U, 3, 0, 0U, &error);
	check(connection != NULL && error == NULL, "consumed-drop setup failed");
	ib_error_free(error);
	error = NULL;
	fail_drop_database = 1;
	consume_drop_handle_on_error = 1;
	check(ib_connection_drop(connection, &consumed, &error) != 0 &&
		error != NULL && consumed,
		"drop error with a consumed handle did not report consumption");
	check(drop_calls == 1 && detach_calls == 0,
		"consumed drop error attempted detach or the wrong number of calls");
	ib_error_free(error);
	consume_drop_handle_on_error = 0;
}

static void test_drop_rejects_active_native_resources(void)
{
	char *error = NULL;
	ib_connection *connection;
	reset_create_stubs();
	connection = ib_connection_create("new.ib", 6U, "SYSDBA", 6U, "password", 8U,
		NULL, 0U, NULL, 0U, NULL, 0U, "UTF8", 4U, 3, 0, 0U, &error);
	check(connection != NULL && error == NULL, "resource-guard setup failed");
	ib_error_free(error);
	error = NULL;
	connection->transaction = &handle_token;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) != 0 && error != NULL && !consumed,
		"drop accepted an active transaction");
	}
	check(drop_calls == 0 && connection->database == &handle_token,
		"transaction guard changed native ownership");
	ib_error_free(error);
	error = NULL;
	connection->transaction = NULL;
	connection->cursors = (ib_cursor *) &handle_token;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) != 0 && error != NULL && !consumed,
		"drop accepted an active cursor");
	}
	check(drop_calls == 0 && connection->database == &handle_token,
		"cursor guard changed native ownership");
	ib_error_free(error);
	error = NULL;
	connection->cursors = NULL;
	connection->statements = (ib_statement *) &handle_token;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) != 0 && error != NULL && !consumed,
		"drop accepted an active statement");
	}
	check(drop_calls == 0 && connection->database == &handle_token,
		"statement guard changed native ownership");
	ib_error_free(error);
	error = NULL;
	connection->statements = NULL;
	{
		int consumed = 0;
		check(ib_connection_drop(connection, &consumed, &error) == 0 && error == NULL && consumed,
		"drop failed after native resources were released");
	}
	ib_error_free(error);
	error = NULL;
}

int main(void)
{
	test_start_failure();
	test_direct_transaction_start_failure_retains_handle();
	test_direct_transaction_completion_failure_retains_handle();
	test_configured_and_explicit_tpb();
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
	test_create_database_dpb_and_drop_ownership();
	test_create_failure_cleans_returned_handle();
	test_default_tpb_failure_preserves_created_connection();
	test_failed_drop_preserves_connection();
	test_failed_drop_consumes_handle();
	test_drop_rejects_active_native_resources();
	if (failures != 0) {
		return EXIT_FAILURE;
	}
	puts("native lifecycle tests passed");
	return EXIT_SUCCESS;
}
