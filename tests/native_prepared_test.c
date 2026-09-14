#include <stdarg.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <ibase.h>

static int fail_message_allocation;

static void *test_malloc(size_t size)
{
	return fail_message_allocation ? NULL : malloc(size);
}

ISC_STATUS test_start_transaction(ISC_STATUS *, isc_tr_handle *, short, ...);
ISC_STATUS test_commit_transaction(ISC_STATUS *, isc_tr_handle *);
ISC_STATUS test_rollback_transaction(ISC_STATUS *, isc_tr_handle *);
ISC_STATUS test_dsql_allocate_statement(ISC_STATUS *, isc_db_handle *, isc_stmt_handle *);
ISC_STATUS test_dsql_describe_bind(ISC_STATUS *, isc_stmt_handle *, unsigned short, XSQLDA *);
ISC_STATUS test_dsql_describe(ISC_STATUS *, isc_stmt_handle *, unsigned short, XSQLDA *);
ISC_STATUS test_dsql_execute(ISC_STATUS *, isc_tr_handle *, isc_stmt_handle *, unsigned short, XSQLDA *);
ISC_STATUS test_dsql_execute2(ISC_STATUS *, isc_tr_handle *, isc_stmt_handle *, unsigned short, XSQLDA *, XSQLDA *);
ISC_STATUS test_dsql_fetch(ISC_STATUS *, isc_stmt_handle *, unsigned short, XSQLDA *);
ISC_STATUS test_dsql_free_statement(ISC_STATUS *, isc_stmt_handle *, unsigned short);
ISC_STATUS test_dsql_prepare(ISC_STATUS *, isc_tr_handle *, isc_stmt_handle *, unsigned short, char *, unsigned short, XSQLDA *);
ISC_STATUS test_dsql_sql_info(ISC_STATUS *, isc_stmt_handle *, short, char *, short, char *);
ISC_STATUS test_detach_database(ISC_STATUS *, isc_db_handle *);

#define isc_start_transaction test_start_transaction
#define isc_commit_transaction test_commit_transaction
#define isc_rollback_transaction test_rollback_transaction
#define isc_dsql_allocate_statement test_dsql_allocate_statement
#define isc_dsql_describe_bind test_dsql_describe_bind
#define isc_dsql_describe test_dsql_describe
#define isc_dsql_execute test_dsql_execute
#define isc_dsql_execute2 test_dsql_execute2
#define isc_dsql_fetch test_dsql_fetch
#define isc_dsql_free_statement test_dsql_free_statement
#define isc_dsql_prepare test_dsql_prepare
#define isc_dsql_sql_info test_dsql_sql_info
#define isc_detach_database test_detach_database
#define malloc test_malloc
#include "../native.c"
#undef isc_start_transaction
#undef isc_commit_transaction
#undef isc_rollback_transaction
#undef isc_dsql_allocate_statement
#undef isc_dsql_describe_bind
#undef isc_dsql_describe
#undef isc_dsql_execute
#undef isc_dsql_execute2
#undef isc_dsql_fetch
#undef isc_dsql_free_statement
#undef isc_dsql_prepare
#undef isc_dsql_sql_info
#undef isc_detach_database
#undef malloc

struct ib_statement;
struct ib_statement *ib_statement_prepare(ib_connection *, const char *, size_t, char **);
int ib_statement_num_input(const struct ib_statement *);
int ib_statement_exec(struct ib_statement *, const ib_bindings *, int64_t *, char **);
ib_cursor *ib_statement_query(struct ib_statement *, const ib_bindings *, char **);
int ib_statement_close(struct ib_statement *, char **);

static int failures;
static int start_calls;
static int commit_calls;
static int rollback_calls;
static int allocate_calls;
static int prepare_calls;
static int describe_bind_calls;
static int describe_calls;
static int execute_calls;
static int execute2_calls;
static int execute2_open;
static int fetch_rows_remaining;
static int fetch_calls;
static int free_statement_calls;
static int close_calls;
static int detach_calls;
static int fail_execute_once;
static int fail_prepare;
static int fail_rollback;
static int statement_type = isc_info_sql_stmt_insert;
static int64_t last_execute_value;
static char database_token;
static char transaction_tokens[32];
static char statement_tokens[32];

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native prepared test failed: %s\n", message);
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

static void reset_mocks(void)
{
	start_calls = 0;
	commit_calls = 0;
	rollback_calls = 0;
	allocate_calls = 0;
	prepare_calls = 0;
	describe_bind_calls = 0;
	describe_calls = 0;
	execute_calls = 0;
	execute2_calls = 0;
	execute2_open = 0;
	fetch_rows_remaining = 0;
	fetch_calls = 0;
	free_statement_calls = 0;
	close_calls = 0;
	detach_calls = 0;
	fail_execute_once = 0;
	fail_prepare = 0;
	fail_rollback = 0;
	fail_message_allocation = 0;
	last_execute_value = 0;
	statement_type = isc_info_sql_stmt_insert;
}

static ib_connection *new_connection(void)
{
	ib_connection *connection = (ib_connection *) calloc(1U, sizeof(*connection));
	if (connection == NULL) {
		abort();
	}
	connection->database = &database_token;
	return connection;
}

static ib_bindings *new_integer_binding(int64_t value)
{
	char *error = NULL;
	ib_bindings *bindings = ib_bindings_new(1U, &error);
	check(bindings != NULL && error == NULL, "integer binding allocation failed");
	if (bindings == NULL) {
		ib_error_free(error);
		return NULL;
	}
	check(ib_bindings_set_int64(bindings, 0U, value, &error) == 0 && error == NULL,
		"integer binding setup failed");
	ib_error_free(error);
	return bindings;
}

static struct ib_statement *prepare_statement(ib_connection *connection, int type)
{
	static const char query[] = "INSERT INTO T (ID) VALUES (?)";
	char *error = NULL;
	struct ib_statement *statement;

	statement_type = type;
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL, "prepared statement allocation failed");
	ib_error_free(error);
	return statement;
}

ISC_STATUS ISC_EXPORT_VARARG test_start_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction, short count, ...)
{
	(void) count;
	start_calls++;
	*transaction = &transaction_tokens[start_calls % sizeof(transaction_tokens)];
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_commit_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	commit_calls++;
	*transaction = NULL;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_rollback_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	rollback_calls++;
	*transaction = NULL;
	return status_result(status, fail_rollback);
}

ISC_STATUS ISC_EXPORT test_dsql_allocate_statement(ISC_STATUS *status,
	isc_db_handle *database, isc_stmt_handle *statement)
{
	(void) database;
	allocate_calls++;
	*statement = &statement_tokens[allocate_calls % sizeof(statement_tokens)];
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_prepare(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short query_length, char *query, unsigned short dialect,
	XSQLDA *output)
{
	(void) transaction;
	(void) statement;
	(void) query_length;
	(void) query;
	(void) dialect;
	(void) output;
	prepare_calls++;
	if (fail_prepare && fail_rollback) {
		fail_message_allocation = 1;
	}
	return status_result(status, fail_prepare);
}

ISC_STATUS ISC_EXPORT test_dsql_describe_bind(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input)
{
	(void) statement;
	(void) dialect;
	describe_bind_calls++;
	input->sqld = 1;
	input->sqlvar[0].sqltype = SQL_INT64 | 1;
	input->sqlvar[0].sqlscale = 0;
	input->sqlvar[0].sqlsubtype = 0;
	input->sqlvar[0].sqlprecision = 0;
	input->sqlvar[0].sqllen = (short) sizeof(ISC_INT64);
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_describe(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output)
{
	(void) statement;
	(void) dialect;
	describe_calls++;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_INT64 | 1;
	output->sqlvar[0].sqlscale = 0;
	output->sqlvar[0].sqlsubtype = 0;
	output->sqlvar[0].sqlprecision = 0;
	output->sqlvar[0].sqllen = (short) sizeof(ISC_INT64);
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_sql_info(ISC_STATUS *status,
	isc_stmt_handle *statement, short request_length, char *request,
	short response_length, char *response)
{
	(void) statement;
	if (request_length == 1 && request[0] == isc_info_sql_stmt_type) {
		check(response_length >= 8, "statement-type response buffer is too short");
		if (response_length >= 8) {
			memset(response, 0, (size_t) response_length);
			response[0] = isc_info_sql_stmt_type;
			response[1] = 4;
			response[3] = (char) statement_type;
			response[7] = isc_info_end;
		}
		return status_result(status, 0);
	}
	check(request_length == 2 && request[0] == isc_info_sql_records,
		"unexpected statement info request");
	if (response_length >= 10) {
		memset(response, 0, (size_t) response_length);
		response[0] = isc_info_sql_records;
		response[1] = 7;
		response[3] = isc_info_req_insert_count;
		response[4] = 4;
		response[6] = 1;
	}
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_execute(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input)
{
	(void) transaction;
	(void) statement;
	(void) dialect;
	execute_calls++;
	if (input != NULL && input->sqld == 1 && input->sqlvar[0].sqldata != NULL) {
		memcpy(&last_execute_value, input->sqlvar[0].sqldata,
			sizeof(last_execute_value));
	}
	return status_result(status, fail_execute_once-- > 0);
}

ISC_STATUS ISC_EXPORT test_dsql_execute2(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input, XSQLDA *output)
{
	(void) transaction;
	(void) statement;
	(void) dialect;
	(void) input;
	(void) output;
	execute2_calls++;
	if (execute2_open) {
		return status_result(status, 1);
	}
	execute2_open = 1;
	fetch_rows_remaining = 1;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_fetch(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output)
{
	(void) statement;
	(void) dialect;
	(void) output;
	if (!execute2_open) {
		return status_result(status, 1);
	}
	fetch_calls++;
	if (fetch_rows_remaining > 0) {
		fetch_rows_remaining--;
		return status_result(status, 0);
	}
	return status_result(status, 0) == 0 ? 100 : -1;
}

ISC_STATUS ISC_EXPORT test_dsql_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option)
{
	if (option == DSQL_close) {
		check(execute2_open, "cursor cleanup must close an open prepared SELECT");
		close_calls++;
		execute2_open = 0;
	} else {
		check(option == DSQL_drop, "statement cleanup must drop the prepared handle");
		check(!execute2_open, "statement cleanup dropped an open prepared SELECT");
		free_statement_calls++;
		*statement = NULL;
	}
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_detach_database(ISC_STATUS *status,
	isc_db_handle *database)
{
	detach_calls++;
	*database = NULL;
	return status_result(status, 0);
}

static void test_prepare_once_execute_repeated(void)
{
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	char *error = NULL;
	int64_t affected;

	reset_mocks();
	connection = new_connection();
	statement = prepare_statement(connection, isc_info_sql_stmt_insert);
	check(statement != NULL && ib_statement_num_input(statement) == 1,
		"prepared statement did not retain its input count");
	check(prepare_calls == 1 && describe_bind_calls == 1,
		"prepare did not describe its handle exactly once");
	if (statement == NULL) {
		free(connection);
		return;
	}

	bindings = new_integer_binding(10);
	check(ib_statement_exec(statement, bindings, &affected, &error) == 0 &&
		error == NULL && affected == 1 && last_execute_value == 10,
		"first prepared execution failed or bound the wrong value");
	ib_error_free(error);
	error = NULL;
	ib_bindings_free(bindings);

	bindings = new_integer_binding(20);
	check(ib_statement_exec(statement, bindings, &affected, &error) == 0 &&
		error == NULL && affected == 1 && last_execute_value == 20,
		"second prepared execution failed or reused stale bindings");
	ib_error_free(error);
	ib_bindings_free(bindings);
	check(prepare_calls == 1 && execute_calls == 2,
		"repeated prepared execution reparsed or reallocated the statement");
	check(commit_calls == 3 && rollback_calls == 0,
		"implicit prepared writes did not commit exactly once per execution");

	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"prepared statement close failed");
	ib_error_free(error);
	check(free_statement_calls == 1, "prepared handle was not freed exactly once");
	free(connection);
}

static void test_failed_implicit_execution_keeps_handle(void)
{
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	char *error = NULL;
	int64_t affected;

	reset_mocks();
	connection = new_connection();
	statement = prepare_statement(connection, isc_info_sql_stmt_insert);
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(30);
	fail_execute_once = 1;
	check(ib_statement_exec(statement, bindings, &affected, &error) != 0 &&
		error != NULL, "failed prepared execution unexpectedly succeeded");
	ib_error_free(error);
	ib_bindings_free(bindings);
	check(rollback_calls == 1 && free_statement_calls == 0,
		"failed implicit execution leaked or committed its transaction");

	error = NULL;
	bindings = new_integer_binding(40);
	check(ib_statement_exec(statement, bindings, &affected, &error) == 0 &&
		error == NULL && last_execute_value == 40,
		"prepared handle could not be reused after an execution failure");
	ib_error_free(error);
	ib_bindings_free(bindings);
	check(prepare_calls == 1 && execute_calls == 2,
		"execution failure caused a second native prepare");
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"failed-execution statement close failed");
	ib_error_free(error);
	free(connection);
}

static void test_prepare_failure_releases_handle_and_transaction(void)
{
	static const char query[] = "INSERT INTO T (ID) VALUES (?)";
	ib_connection *connection;
	ib_statement *statement;
	char *error = NULL;

	reset_mocks();
	connection = new_connection();
	fail_prepare = 1;
	statement_type = isc_info_sql_stmt_insert;
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement == NULL && error != NULL,
		"failed native prepare unexpectedly returned a statement");
	ib_error_free(error);
	check(prepare_calls == 1 && free_statement_calls == 1 && rollback_calls == 1,
		"failed prepare leaked its native handle or temporary transaction");

	error = NULL;
	fail_prepare = 0;
	statement = prepare_statement(connection, isc_info_sql_stmt_insert);
	check(statement != NULL && error == NULL && prepare_calls == 2,
		"connection could not prepare again after a failed prepare");
	ib_error_free(error);
	if (statement != NULL) {
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
	}
	check(free_statement_calls == 2, "recovered prepare left a native handle behind");
	free(connection);
}

static void test_failed_prepare_rollback_marks_connection_broken(void)
{
	static const char query[] = "INSERT INTO T (ID) VALUES (?)";
	ib_connection *connection;
	ib_statement *statement;
	char *error = NULL;

	reset_mocks();
	connection = new_connection();
	fail_prepare = 1;
	fail_rollback = 1;
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement == NULL && connection->broken && connection->transaction == NULL,
		"failed prepare rollback did not retire the connection after native failure");
	check(error == NULL, "failed prepare rollback unexpectedly allocated diagnostics");
	ib_error_free(error);
	fail_message_allocation = 0;
	free(connection);
}

static void test_query_cursor_does_not_drop_statement(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL, "SELECT statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(1);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL, "prepared SELECT query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	check(cursor != NULL && execute2_calls == 1 && free_statement_calls == 0,
		"prepared query dropped its reusable handle before cursor close");
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
			"prepared SELECT cursor close failed");
		ib_error_free(error);
	}
	check(close_calls == 1 && free_statement_calls == 0 && rollback_calls == 1,
		"prepared cursor cleanup did not close the server cursor or missed implicit rollback");

	bindings = new_integer_binding(2);
	error = NULL;
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL && execute2_calls == 2 &&
		prepare_calls == 1, "prepared SELECT did not reuse its handle");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_next(cursor, &error) == 1 && error == NULL,
			"prepared SELECT did not return its first row");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_next(cursor, &error) == 0 && error == NULL,
			"prepared SELECT did not report EOF");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
			"prepared SELECT EOF cursor close failed");
		ib_error_free(error);
	}
	check(close_calls == 2 && free_statement_calls == 0 && rollback_calls == 2,
		"prepared SELECT EOF cleanup did not close the server cursor");
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"SELECT statement close failed");
	ib_error_free(error);
	check(free_statement_calls == 1, "SELECT statement handle was not released at close");
	free(connection);
}

static void test_explicit_transaction_is_not_cursor_owned(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL && start_calls == 0 && commit_calls == 0,
		"prepare unexpectedly took ownership of the explicit transaction");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(3);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL, "explicit-transaction prepared query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		(void) ib_cursor_close(cursor, &error);
		ib_error_free(error);
	}
	check(rollback_calls == 0, "closing a borrowed cursor rolled back the explicit transaction");
	error = NULL;
	check(ib_connection_rollback(connection, &error) == 0 && error == NULL,
		"explicit transaction rollback failed");
	ib_error_free(error);
	check(rollback_calls == 1, "connection did not retain explicit transaction ownership");
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"explicit transaction statement close failed");
	ib_error_free(error);
	free(connection);
}

static void test_transaction_control_is_rejected_before_execution(void)
{
	static const int control_types[] = {
		isc_info_sql_stmt_start_trans,
		isc_info_sql_stmt_commit,
		isc_info_sql_stmt_rollback
	};
	size_t index;

	for (index = 0U; index < sizeof(control_types) / sizeof(control_types[0]); index++) {
		ib_connection *connection;
		struct ib_statement *statement;
		ib_bindings *bindings;
		char *error = NULL;
		int64_t affected;

		reset_mocks();
		connection = new_connection();
		statement = prepare_statement(connection, control_types[index]);
		check(statement != NULL, "transaction-control statement preparation failed");
		if (statement == NULL) {
			free(connection);
			continue;
		}
		bindings = new_integer_binding(1);
		error = NULL;
		check(ib_statement_exec(statement, bindings, &affected, &error) != 0 &&
			error != NULL && execute_calls == 0 && start_calls == 1 &&
			commit_calls == 1 && rollback_calls == 0,
			"prepared transaction control reached execution or changed transaction ownership");
		ib_error_free(error);
		ib_bindings_free(bindings);
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
		free(connection);
	}
}

static void test_savepoint_remains_executable(void)
{
	static const char query[] = "SAVEPOINT go_allowed";
	ib_connection *connection;
	ib_bindings *bindings;
	int64_t affected;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_ddl;
	connection = new_connection();
	bindings = new_integer_binding(1);
	check(ib_connection_exec(connection, query, sizeof(query) - 1U, bindings,
		&affected, &error) == 0 && error == NULL && execute_calls == 1 &&
		start_calls == 1 && commit_calls == 1,
		"SAVEPOINT was incorrectly classified as transaction control");
	ib_error_free(error);
	ib_bindings_free(bindings);
	free(connection);
}

static void test_transaction_completion_closes_active_prepared_cursor(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL, "active-cursor statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(5);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL && connection->active_cursor == cursor,
		"active prepared cursor was not registered on the connection");
	ib_error_free(error);
	ib_bindings_free(bindings);
	error = NULL;
	check(ib_connection_commit(connection, &error) == 0 && error == NULL,
		"transaction completion with active prepared cursor failed");
	ib_error_free(error);
	check(connection->active_cursor == NULL && free_statement_calls == 0 &&
		rollback_calls == 0 && commit_calls == 1,
		"transaction completion dropped the statement or rolled back a borrowed transaction");
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"active-cursor statement close after transaction completion failed");
	ib_error_free(error);
	check(free_statement_calls == 1, "active-cursor statement was not released at close");
	free(connection);
}

static void test_connection_close_drains_prepared_statements(void)
{
	ib_connection *connection;
	struct ib_statement *first;
	struct ib_statement *second;
	char *error = NULL;

	reset_mocks();
	connection = new_connection();
	first = prepare_statement(connection, isc_info_sql_stmt_insert);
	second = prepare_statement(connection, isc_info_sql_stmt_insert);
	check(first != NULL && second != NULL, "connection-drain statement preparation failed");
	error = NULL;
	check(ib_connection_close(connection, &error) == 0 && error == NULL,
		"connection close with prepared statements failed");
	ib_error_free(error);
	check(free_statement_calls == 2 && detach_calls == 1,
		"connection close did not drain all prepared handles before detach");
}

int main(void)
{
	test_prepare_once_execute_repeated();
	test_failed_implicit_execution_keeps_handle();
	test_prepare_failure_releases_handle_and_transaction();
	test_failed_prepare_rollback_marks_connection_broken();
	test_query_cursor_does_not_drop_statement();
	test_explicit_transaction_is_not_cursor_owned();
	test_transaction_control_is_rejected_before_execution();
	test_savepoint_remains_executable();
	test_transaction_completion_closes_active_prepared_cursor();
	test_connection_close_drains_prepared_statements();
	if (failures != 0) {
		return EXIT_FAILURE;
	}
	(void) puts("native prepared tests passed");
	return EXIT_SUCCESS;
}
