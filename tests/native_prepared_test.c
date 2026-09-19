#include <stdarg.h>
#include <pthread.h>
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
ISC_STATUS test_prepare_transaction2(ISC_STATUS *, isc_tr_handle *, unsigned short, char *);
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
struct ib_cancel_slot;
static void overlap_completion_wait_hook(struct ib_cancel_slot *slot);
static void overlap_publication_wait_hook(struct ib_cancel_slot *slot);

#define isc_start_transaction test_start_transaction
#define isc_commit_transaction test_commit_transaction
#define isc_rollback_transaction test_rollback_transaction
#define isc_prepare_transaction2 test_prepare_transaction2
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
#define IB_CANCEL_SLOT_TEST_COMPLETION_WAIT_HOOK(slot) overlap_completion_wait_hook(slot)
#define IB_CANCEL_SLOT_TEST_PUBLICATION_WAIT_HOOK(slot) overlap_publication_wait_hook(slot)
#include "../native.c"
#undef IB_CANCEL_SLOT_TEST_COMPLETION_WAIT_HOOK
#undef IB_CANCEL_SLOT_TEST_PUBLICATION_WAIT_HOOK
#undef isc_start_transaction
#undef isc_commit_transaction
#undef isc_rollback_transaction
#undef isc_prepare_transaction2
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
struct ib_statement *ib_statement_prepare(ib_connection *, const char *, size_t,
	ib_cancel_slot *, uint64_t, char **);
int ib_statement_num_input(const struct ib_statement *);
int ib_statement_exec(struct ib_statement *, const ib_bindings *, ib_cancel_slot *,
	uint64_t, int64_t *, char **);
ib_cursor *ib_statement_query(struct ib_statement *, const ib_bindings *, ib_cancel_slot *,
	uint64_t, char **);
int ib_statement_close(struct ib_statement *, char **);
int ib_cursor_next(ib_cursor *, ib_cancel_slot *, uint64_t, char **);

/* Existing prepared coverage uses the contextless native seam.  Keep those
 * calls explicit about an idle slot while the publication tests below invoke
 * the full cancellation-aware signatures without this compatibility shim. */
#define ib_statement_exec(statement, bindings, rows, error) \
	(ib_statement_exec)(statement, bindings, NULL, 0U, rows, error)
#define ib_statement_query(statement, bindings, error) \
	(ib_statement_query)(statement, bindings, NULL, 0U, error)
#define ib_statement_prepare(connection, query, length, error) \
	(ib_statement_prepare)(connection, query, length, NULL, 0U, error)
#define ib_connection_query(connection, query, length, bindings, arrays, error) \
	(ib_connection_query)(connection, query, length, bindings, arrays, NULL, 0U, error)
#define ib_connection_exec(connection, query, length, bindings, rows, arrays, error) \
	(ib_connection_exec)(connection, query, length, bindings, rows, arrays, NULL, 0U, error)
#define ib_cursor_describe_metadata(cursor, error) \
	(ib_cursor_describe_metadata)(cursor, NULL, 0U, error)
#define ib_cursor_next(cursor, error) \
	(ib_cursor_next)(cursor, NULL, 0U, error)

static int failures;
static int start_calls;
static int commit_calls;
static int rollback_calls;
static int prepare_transaction_calls;
static int allocate_calls;
static int prepare_calls;
static int describe_bind_calls;
static int describe_calls;
static int execute_calls;
static int execute2_calls;
static int execute2_open;
static int open_selects;
static int fetch_rows_remaining;
static int fetch_calls;
static int free_statement_calls;
static int close_calls;
static int detach_calls;
static int fail_execute_once;
static int fail_execute2_once;
static int fail_commit;
static int fail_start_on_call;
static int fail_prepare;
static int fail_prepare_on_call;
static ISC_STATUS prepare_failure_code;
static int fail_rollback;
static int fail_drop;
static int fail_sql_info_on_call;
static int sql_info_calls;
static int statement_type = isc_info_sql_stmt_insert;
static int procedure_output_count = 1;
static int rollback_saw_live_transaction;
static int catalog_close_calls;
static int catalog_drop_calls;
static int catalog_fetch_rows_remaining;
static int catalog_statement_open;
static int fail_catalog_drop;
static int catalog_drop_left_handle;
static int failed_drop_left_handle;
static int describe_user_column_relation;
static int describe_user_charset_identifier;
static int describe_user_column_alias;
static int describe_procedure_decimal;
static int describe_procedure_source;
static int complete_during_prepare;
static int user_execute2_calls;
static int64_t last_execute_value;
static int64_t persisted_execute_value;
static ib_cancel_slot *expected_cancel_slot;
static uint64_t expected_cancel_generation;
static isc_stmt_handle expected_cancel_statement;
static int execute_publication_calls;
static int execute2_publication_calls;
static int fetch_publication_calls;
enum overlap_kind {
	OVERLAP_NONE = 0,
	OVERLAP_EXECUTE = 1,
	OVERLAP_EXECUTE2 = 2,
	OVERLAP_FETCH = 3,
	OVERLAP_PREPARE = 4,
	OVERLAP_TRANSIENT_EXECUTE = 5,
	OVERLAP_TRANSIENT_EXECUTE2 = 6,
	OVERLAP_CATALOG_EXECUTE2 = 7,
	OVERLAP_DISTRIBUTED_EXECUTE = 8
};
static pthread_mutex_t overlap_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t overlap_condition = PTHREAD_COND_INITIALIZER;
static int overlap_mode;
static int overlap_native_entered;
static int overlap_native_returned;
static int overlap_native_release;
static int overlap_native_failed;
static int overlap_cancel_entered;
static int overlap_cancel_release;
static int overlap_cancel_failed;
static int overlap_cancel_in_call;
static int overlap_cancel_requested;
static int overlap_completion_waiting;
static int overlap_publication_waiting;
static int overlap_operation_done;
static int overlap_cleanup_calls;
static int overlap_cleanup_violations;
static isc_stmt_handle overlap_cancel_statement;
static char database_token;
static char transaction_tokens[32];
static char statement_tokens[32];
static isc_stmt_handle catalog_statement;
static int procedure_catalog_statement;
static int procedure_catalog_precision;
static int procedure_catalog_scale;
static int catalog_charset_identifier;
static int catalog_lookup_bytes_ok;
static unsigned short expected_dialect;

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native prepared test failed: %s\n", message);
		failures++;
	}
}

static void check_cancel_publication(isc_stmt_handle *statement, int *seen,
	const char *message)
{
	ib_cancel_slot *slot;
	uint64_t generation;
	isc_stmt_handle expected_statement;
	int published;

	if (expected_cancel_slot == NULL || statement == NULL ||
		expected_cancel_statement == NULL || *statement != expected_cancel_statement) {
		return;
	}
	slot = expected_cancel_slot;
	generation = expected_cancel_generation;
	expected_statement = expected_cancel_statement;
	(*seen)++;
	if (pthread_mutex_lock(&slot->mutex) != 0) {
		abort();
	}
	published = slot->generation == generation && slot->active &&
		slot->published && slot->statement == expected_statement &&
		slot->statement == *statement;
	if (pthread_mutex_unlock(&slot->mutex) != 0) {
		abort();
	}
	check(published,
		message);
}

static void check_cancel_complete(const char *message)
{
	ib_cancel_slot *slot;
	int complete;

	if (expected_cancel_slot == NULL) {
		return;
	}
	slot = expected_cancel_slot;
	if (pthread_mutex_lock(&slot->mutex) != 0) {
		abort();
	}
	complete = !slot->active && !slot->published && slot->operation_complete &&
		slot->cancel_users == 0U && slot->cancel_callers == 0U;
	if (pthread_mutex_unlock(&slot->mutex) != 0) {
		abort();
	}
	check(complete, message);
}

static int is_catalog_statement(const isc_stmt_handle *statement)
{
	return statement != NULL && *statement != NULL && *statement == catalog_statement;
}

static void set_catalog_text(XSQLVAR *variable, const char *value)
{
	size_t length = strlen(value);

	memset(variable->sqldata, ' ', (size_t) variable->sqllen);
	if (length > (size_t) variable->sqllen) {
		length = (size_t) variable->sqllen;
	}
	memcpy(variable->sqldata, value, length);
	*variable->sqlind = 0;
}

static void set_catalog_bytes(XSQLVAR *variable, const unsigned char *value,
	size_t length)
{
	memset(variable->sqldata, ' ', (size_t) variable->sqllen);
	if (length > (size_t) variable->sqllen) {
		length = (size_t) variable->sqllen;
	}
	memcpy(variable->sqldata, value, length);
	*variable->sqlind = 0;
}

static void set_catalog_integer(XSQLVAR *variable, ISC_LONG value)
{
	memcpy(variable->sqldata, &value, sizeof(value));
	*variable->sqlind = 0;
}

static void set_catalog_null(XSQLVAR *variable)
{
	*variable->sqlind = -1;
}

static ISC_STATUS status_result(ISC_STATUS *status, int failed)
{
	status[0] = isc_arg_gds;
	status[1] = failed ? isc_network_error : 0;
	status[2] = isc_arg_end;
	return status[1];
}

static ISC_STATUS status_code(ISC_STATUS *status, ISC_STATUS code)
{
	status[0] = isc_arg_gds;
	status[1] = code;
	status[2] = isc_arg_end;
	return code;
}

static void overlap_lock(void)
{
	if (pthread_mutex_lock(&overlap_mutex) != 0) {
		abort();
	}
}

static void overlap_unlock(void)
{
	if (pthread_mutex_unlock(&overlap_mutex) != 0) {
		abort();
	}
}

static void overlap_broadcast(void)
{
	if (pthread_cond_broadcast(&overlap_condition) != 0) {
		abort();
	}
}

static void overlap_wait(int *condition)
{
	while (!*condition) {
		if (pthread_cond_wait(&overlap_condition, &overlap_mutex) != 0) {
			abort();
		}
	}
}

static void overlap_reset(int mode, int native_failed, int cancel_failed)
{
	overlap_lock();
	overlap_mode = mode;
	overlap_native_entered = 0;
	overlap_native_returned = 0;
	overlap_native_release = 0;
	overlap_native_failed = native_failed;
	overlap_cancel_entered = 0;
	overlap_cancel_release = 0;
	overlap_cancel_failed = cancel_failed;
	overlap_cancel_in_call = 0;
	overlap_cancel_requested = 0;
	overlap_completion_waiting = 0;
	overlap_publication_waiting = 0;
	overlap_operation_done = 0;
	overlap_cleanup_calls = 0;
	overlap_cleanup_violations = 0;
	overlap_cancel_statement = NULL;
	overlap_unlock();
}

static int overlap_native_call(enum overlap_kind kind)
{
	int failed;

	overlap_lock();
	if (overlap_mode != (int) kind) {
		overlap_unlock();
		return 0;
	}
	overlap_native_entered = 1;
	overlap_broadcast();
	overlap_wait(&overlap_native_release);
	failed = overlap_native_failed;
	overlap_native_returned = 1;
	overlap_broadcast();
	overlap_unlock();
	return failed;
}

static ISC_STATUS overlap_cancel_call(ISC_STATUS *status)
{
	int failed;

	overlap_lock();
	overlap_cancel_entered = 1;
	overlap_cancel_in_call = 1;
	overlap_cancel_requested = 1;
	overlap_broadcast();
	overlap_wait(&overlap_cancel_release);
	failed = overlap_cancel_failed;
	overlap_cancel_in_call = 0;
	overlap_broadcast();
	overlap_unlock();
	return status_result(status, failed);
}

static void overlap_cleanup_event(void)
{
	overlap_lock();
	overlap_cleanup_calls++;
	if (overlap_cancel_in_call) {
		overlap_cleanup_violations++;
	}
	overlap_broadcast();
	overlap_unlock();
}

static void overlap_completion_wait_hook(struct ib_cancel_slot *slot)
{
	(void) slot;
	overlap_lock();
	overlap_completion_waiting = 1;
	overlap_broadcast();
	overlap_unlock();
}

static void overlap_publication_wait_hook(struct ib_cancel_slot *slot)
{
	(void) slot;
	overlap_lock();
	overlap_publication_waiting = 1;
	overlap_broadcast();
	overlap_unlock();
}

static void overlap_wait_native_entry(void)
{
	overlap_lock();
	overlap_wait(&overlap_native_entered);
	overlap_unlock();
}

static void overlap_wait_cancel_entry(void)
{
	overlap_lock();
	overlap_wait(&overlap_cancel_entered);
	overlap_unlock();
}

static void overlap_wait_native_return(void)
{
	overlap_lock();
	overlap_wait(&overlap_native_returned);
	overlap_unlock();
}

static void overlap_wait_completion(void)
{
	overlap_lock();
	overlap_wait(&overlap_completion_waiting);
	overlap_unlock();
}

static void overlap_wait_publication(void)
{
	overlap_lock();
	overlap_wait(&overlap_publication_waiting);
	overlap_unlock();
}

static void overlap_release_native(void)
{
	overlap_lock();
	overlap_native_release = 1;
	overlap_broadcast();
	overlap_unlock();
}

static void overlap_release_cancel(void)
{
	overlap_lock();
	overlap_cancel_release = 1;
	overlap_broadcast();
	overlap_unlock();
}

static int overlap_operation_is_done(void)
{
	int done;

	overlap_lock();
	done = overlap_operation_done;
	overlap_unlock();
	return done;
}

static int overlap_cleanup_violation_count(void)
{
	int violations;

	overlap_lock();
	violations = overlap_cleanup_violations;
	overlap_unlock();
	return violations;
}

static int overlap_cleanup_count(void)
{
	int count;

	overlap_lock();
	count = overlap_cleanup_calls;
	overlap_unlock();
	return count;
}

static int overlap_is_enabled(void)
{
	int enabled;

	overlap_lock();
	enabled = overlap_mode != OVERLAP_NONE;
	overlap_unlock();
	return enabled;
}

static void reset_mocks(void)
{
	overlap_reset(OVERLAP_NONE, 0, 0);
	start_calls = 0;
	commit_calls = 0;
	rollback_calls = 0;
	prepare_transaction_calls = 0;
	allocate_calls = 0;
	prepare_calls = 0;
	describe_bind_calls = 0;
	describe_calls = 0;
	execute_calls = 0;
	execute2_calls = 0;
	execute2_open = 0;
	open_selects = 0;
	fetch_rows_remaining = 0;
	fetch_calls = 0;
	free_statement_calls = 0;
	close_calls = 0;
	detach_calls = 0;
	fail_execute_once = 0;
	fail_execute2_once = 0;
	fail_commit = 0;
	fail_start_on_call = 0;
	fail_prepare = 0;
	fail_prepare_on_call = 0;
	prepare_failure_code = 0;
	fail_rollback = 0;
	fail_drop = 0;
	fail_sql_info_on_call = 0;
	sql_info_calls = 0;
	fail_message_allocation = 0;
	rollback_saw_live_transaction = 0;
	catalog_close_calls = 0;
	catalog_drop_calls = 0;
	catalog_fetch_rows_remaining = 0;
	catalog_statement_open = 0;
	fail_catalog_drop = 0;
	catalog_drop_left_handle = 0;
	failed_drop_left_handle = 0;
	describe_user_column_relation = 0;
	describe_user_charset_identifier = 0;
	describe_user_column_alias = 0;
	describe_procedure_decimal = 0;
	describe_procedure_source = 0;
	complete_during_prepare = 0;
	user_execute2_calls = 0;
	catalog_statement = NULL;
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	execute_publication_calls = 0;
	execute2_publication_calls = 0;
	fetch_publication_calls = 0;
	procedure_catalog_statement = 0;
	procedure_catalog_precision = 0;
	procedure_catalog_scale = 0;
	catalog_charset_identifier = 0;
	catalog_lookup_bytes_ok = 0;
	expected_dialect = SQL_DIALECT_V5;
	last_execute_value = 0;
	persisted_execute_value = 0;
	statement_type = isc_info_sql_stmt_insert;
	procedure_output_count = 1;
}

static ib_connection *new_connection(void)
{
	ib_connection *connection = (ib_connection *) calloc(1U, sizeof(*connection));
	if (connection == NULL) {
		abort();
	}
	connection->database = &database_token;
	connection->dialect = SQL_DIALECT_V5;
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

struct overlap_operation_call {
	int kind;
	ib_connection *connection;
	ib_transaction *transaction;
	const char *query;
	size_t query_length;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	ib_cancel_slot *slot;
	uint64_t generation;
	int result;
	int64_t rows_affected;
	int has_row;
	char *error;
	int abort_result;
	char *abort_error;
};

struct overlap_cancel_call {
	ib_cancel_slot *slot;
	uint64_t generation;
	int request_status;
	int64_t native_code;
	char *error;
};

static void *overlap_operation_worker(void *argument)
{
	struct overlap_operation_call *call =
		(struct overlap_operation_call *) argument;

	if (call->kind == OVERLAP_EXECUTE) {
		call->result = (ib_statement_exec)(call->statement, call->bindings,
			call->slot, call->generation, &call->rows_affected, &call->error);
	} else if (call->kind == OVERLAP_PREPARE) {
		call->statement = (ib_statement_prepare)(call->connection, call->query,
			call->query_length, call->slot, call->generation, &call->error);
	} else if (call->kind == OVERLAP_TRANSIENT_EXECUTE) {
		call->result = (ib_connection_exec)(call->connection, call->query,
			call->query_length, call->bindings, &call->rows_affected, 0,
			call->slot, call->generation, &call->error);
	} else if (call->kind == OVERLAP_DISTRIBUTED_EXECUTE) {
		call->result = (ib_transaction_exec)(call->transaction, call->query,
			call->query_length, call->bindings, &call->rows_affected, 0,
			call->slot, call->generation, &call->error);
	} else if (call->kind == OVERLAP_TRANSIENT_EXECUTE2) {
		call->cursor = (ib_connection_query)(call->connection, call->query,
			call->query_length, call->bindings, 0, call->slot, call->generation,
			&call->error);
	} else if (call->kind == OVERLAP_EXECUTE2) {
		call->cursor = (ib_statement_query)(call->statement, call->bindings,
			call->slot, call->generation, &call->error);
	} else {
		call->has_row = (ib_cursor_next)(call->cursor, call->slot,
			call->generation, &call->error);
		call->abort_result = ib_cursor_abort(call->cursor, &call->abort_error);
	}
	overlap_lock();
	overlap_operation_done = 1;
	overlap_broadcast();
	overlap_unlock();
	return NULL;
}

static void *overlap_cancel_worker(void *argument)
{
	struct overlap_cancel_call *call =
		(struct overlap_cancel_call *) argument;

	call->request_status = ib_cancel_slot_cancel(call->slot, call->generation,
		&call->native_code, &call->error);
	return NULL;
}

static void run_delayed_cancel(struct overlap_operation_call *operation,
	struct overlap_cancel_call *cancel, int cancel_failed,
	const char *operation_name)
{
	pthread_t operation_thread;
	pthread_t cancel_thread;

	if (pthread_create(&operation_thread, NULL, overlap_operation_worker,
		operation) != 0) {
		abort();
	}
	overlap_wait_native_entry();
	if (pthread_create(&cancel_thread, NULL, overlap_cancel_worker, cancel) != 0) {
		abort();
	}
	overlap_wait_cancel_entry();
	overlap_release_native();
	overlap_wait_completion();
	check(!overlap_operation_is_done(), operation_name);
	check(overlap_cleanup_count() == 0,
		"prepared cleanup started before delayed cancellation returned");
	check(overlap_cleanup_violation_count() == 0,
		"prepared cleanup overlapped delayed cancellation");
	overlap_release_cancel();
	if (pthread_join(cancel_thread, NULL) != 0 ||
		pthread_join(operation_thread, NULL) != 0) {
		abort();
	}
	check(overlap_operation_is_done(), "prepared operation worker did not finish");
	check(cancel->request_status == 0 && cancel->error == NULL,
		"prepared cancellation request returned an unexpected request error");
	if (cancel_failed) {
		check(cancel->native_code != 0,
			"prepared cancellation failure did not preserve native request status");
	} else {
		check(cancel->native_code == 0,
			"prepared cancellation unexpectedly returned a native request failure");
	}
	check(overlap_cleanup_count() > 0,
		"prepared operation did not perform cleanup after cancellation returned");
}

static void run_delayed_cancel_transient(struct overlap_operation_call *operation,
	struct overlap_cancel_call *cancel, int cancel_failed,
	const char *operation_name)
{
	pthread_t operation_thread;
	pthread_t cancel_thread;

	if (pthread_create(&operation_thread, NULL, overlap_operation_worker,
		operation) != 0) {
		abort();
	}
	overlap_wait_native_entry();
	if (pthread_create(&cancel_thread, NULL, overlap_cancel_worker, cancel) != 0) {
		abort();
	}
	overlap_wait_cancel_entry();
	overlap_release_native();
	overlap_wait_native_return();
	overlap_wait_publication();
	check(!overlap_operation_is_done(), operation_name);
	check(overlap_cleanup_count() == 0,
		"transient cleanup started before delayed cancellation returned");
	check(overlap_cleanup_violation_count() == 0,
		"transient cleanup overlapped delayed cancellation");
	overlap_release_cancel();
	if (pthread_join(cancel_thread, NULL) != 0 ||
		pthread_join(operation_thread, NULL) != 0) {
		abort();
	}
	check(overlap_operation_is_done(), "transient operation worker did not finish");
	check(cancel->request_status == 0 && cancel->error == NULL,
		"transient cancellation request returned an unexpected request error");
	if (cancel_failed) {
		check(cancel->native_code != 0,
			"transient cancellation failure did not preserve native request status");
	} else {
		check(cancel->native_code == 0,
			"transient cancellation unexpectedly returned a native request failure");
	}
	check(overlap_cleanup_count() > 0,
		"transient operation did not perform cleanup after cancellation returned");
}

static void test_dialect_arguments_are_propagated(void)
{
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;
	int64_t rows_affected;

	reset_mocks();
	expected_dialect = SQL_DIALECT_V6;
	connection = new_connection();
	connection->dialect = SQL_DIALECT_V6;
	statement_type = isc_info_sql_stmt_select;
	statement = prepare_statement(connection, statement_type);
	check(statement != NULL, "Dialect 3 prepared statement was not created");
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(7);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL, "Dialect 3 prepared query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_next(cursor, &error) == 1 && error == NULL,
			"Dialect 3 prepared query did not fetch a row");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
			"Dialect 3 prepared query close failed");
		ib_error_free(error);
	}
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"Dialect 3 prepared query statement close failed");
	ib_error_free(error);

	statement_type = isc_info_sql_stmt_insert;
	statement = prepare_statement(connection, statement_type);
	check(statement != NULL, "Dialect 3 prepared execute statement was not created");
	if (statement != NULL) {
		bindings = new_integer_binding(8);
		error = NULL;
		rows_affected = -1;
		check(ib_statement_exec(statement, bindings, &rows_affected, &error) == 0 &&
			error == NULL, "Dialect 3 prepared execute failed");
		ib_error_free(error);
		ib_bindings_free(bindings);
		error = NULL;
		check(ib_statement_close(statement, &error) == 0 && error == NULL,
			"Dialect 3 prepared execute statement close failed");
		ib_error_free(error);
	}
	free(connection);
}

ISC_STATUS ISC_EXPORT_VARARG test_start_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction, short count, ...)
{
	(void) count;
	start_calls++;
	*transaction = &transaction_tokens[start_calls % sizeof(transaction_tokens)];
	return status_result(status, start_calls == fail_start_on_call);
}

ISC_STATUS ISC_EXPORT test_commit_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	overlap_cleanup_event();
	check_cancel_complete("prepared cancellation slot was not complete before commit");
	commit_calls++;
	if (!fail_commit) {
		*transaction = NULL;
	}
	return status_result(status, fail_commit);
}

ISC_STATUS ISC_EXPORT test_prepare_transaction2(ISC_STATUS *status,
	isc_tr_handle *transaction, unsigned short message_length, char *message)
{
	(void) message_length;
	(void) message;
	check(transaction != NULL && *transaction != NULL,
		"distributed prepare did not receive a live transaction");
	prepare_transaction_calls++;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_rollback_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	overlap_cleanup_event();
	check_cancel_complete("prepared cancellation slot was not complete before rollback");
	rollback_calls++;
	if (*transaction != NULL) {
		rollback_saw_live_transaction = 1;
	}
	if (!fail_rollback) {
		*transaction = NULL;
	}
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
	(void) output;
	check(dialect == expected_dialect, "prepare used the wrong SQL dialect");
	prepare_calls++;
	if (complete_during_prepare && expected_cancel_slot != NULL &&
		expected_cancel_generation != 0U) {
		ib_cancel_slot_complete(expected_cancel_slot, expected_cancel_generation);
	}
	if (query != NULL && strstr(query, "RDB$RELATION_FIELDS") != NULL) {
		catalog_statement = *statement;
		procedure_catalog_statement = 0;
	} else if (query != NULL && strstr(query, "RDB$PROCEDURE_PARAMETERS") != NULL) {
		check(strstr(query, "RDB$PARAMETER_TYPE = 1") != NULL,
			"procedure catalog query did not restrict output parameters");
		catalog_statement = *statement;
		procedure_catalog_statement = 1;
	}
	if ((fail_prepare || (fail_prepare_on_call != 0 &&
		prepare_calls == fail_prepare_on_call)) && fail_rollback) {
		fail_message_allocation = 1;
	}
	if (overlap_native_call(OVERLAP_PREPARE)) {
		return status_code(status, isc_cancelled);
	}
	if (fail_prepare || (fail_prepare_on_call != 0 &&
		prepare_calls == fail_prepare_on_call)) {
		return prepare_failure_code != 0 ?
			status_code(status, prepare_failure_code) : status_result(status, 1);
	}
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_describe_bind(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input)
{
	(void) statement;
	check(dialect == expected_dialect, "describe bind used the wrong SQL dialect");
	describe_bind_calls++;
	input->sqld = procedure_catalog_statement ? 2 : 1;
	for (short index = 0; index < input->sqln && index < input->sqld; index++) {
		input->sqlvar[index].sqltype = SQL_INT64 | 1;
		input->sqlvar[index].sqlscale = 0;
		input->sqlvar[index].sqlsubtype = 0;
		input->sqlvar[index].sqlprecision = 0;
		input->sqlvar[index].sqllen = (short) sizeof(ISC_INT64);
	}
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_describe(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output)
{
	check(dialect == expected_dialect, "describe used the wrong SQL dialect");
	describe_calls++;
	if (is_catalog_statement(statement)) {
		short index;

		output->sqld = procedure_catalog_statement ? 7 : 10;
		for (index = 0; index < output->sqln && index < output->sqld; index++) {
			if (index < 2) {
				output->sqlvar[index].sqltype = SQL_TEXT | 1;
				output->sqlvar[index].sqllen = 32;
			} else {
				output->sqlvar[index].sqltype = SQL_LONG | 1;
				output->sqlvar[index].sqllen = (short) sizeof(ISC_LONG);
			}
		}
		return status_result(status, 0);
	}
	output->sqld = statement_type == isc_info_sql_stmt_exec_procedure
		? (short) procedure_output_count : 1;
	if (output->sqld == 0) {
		return status_result(status, 0);
	}
	output->sqlvar[0].sqltype = SQL_INT64 | 1;
	output->sqlvar[0].sqlscale = 0;
	output->sqlvar[0].sqlsubtype = 0;
	output->sqlvar[0].sqlprecision = 0;
	output->sqlvar[0].sqllen = (short) sizeof(ISC_INT64);
	if (describe_procedure_decimal && statement_type == isc_info_sql_stmt_exec_procedure) {
		output->sqlvar[0].sqlsubtype = 1;
		output->sqlvar[0].sqlscale = -2;
		output->sqlvar[0].sqlprecision = 18;
	}
	if (describe_user_column_relation && statement_type == isc_info_sql_stmt_select) {
		output->sqlvar[0].relname_length = 1;
		output->sqlvar[0].relname[0] = 'T';
		output->sqlvar[0].sqlname_length = 2;
		output->sqlvar[0].sqlname[0] = 'I';
		output->sqlvar[0].sqlname[1] = 'D';
	}
	if (describe_user_charset_identifier && statement_type == isc_info_sql_stmt_select) {
		static const unsigned char relation[] = {0x8e};
		static const unsigned char field[] = {0x8a};
		memcpy(output->sqlvar[0].relname, relation, sizeof(relation));
		output->sqlvar[0].relname_length = (short) sizeof(relation);
		memcpy(output->sqlvar[0].sqlname, field, sizeof(field));
		output->sqlvar[0].sqlname_length = (short) sizeof(field);
	}
	if (describe_user_column_alias && statement_type == isc_info_sql_stmt_select) {
		static const unsigned char alias[] = {'P', 0xf8, 0xed, 'l', 'i', 0x9a};
		memcpy(output->sqlvar[0].aliasname, alias, sizeof(alias));
		output->sqlvar[0].aliasname_length = (short) sizeof(alias);
	}
	if (describe_procedure_source && statement_type == isc_info_sql_stmt_exec_procedure) {
		static const char procedure_name[] = "P";
		static const char parameter_name[] = "OUT";
		memcpy(output->sqlvar[0].relname, procedure_name, sizeof(procedure_name) - 1U);
		output->sqlvar[0].relname_length = (short) (sizeof(procedure_name) - 1U);
		memcpy(output->sqlvar[0].sqlname, parameter_name, sizeof(parameter_name) - 1U);
		output->sqlvar[0].sqlname_length = (short) (sizeof(parameter_name) - 1U);
	}
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_dsql_sql_info(ISC_STATUS *status,
	isc_stmt_handle *statement, short request_length, char *request,
	short response_length, char *response)
{
	sql_info_calls++;
	if (fail_sql_info_on_call != 0 && sql_info_calls == fail_sql_info_on_call) {
		return status_result(status, 1);
	}
	if (request_length == 1 && request[0] == isc_info_sql_stmt_type) {
		check(response_length >= 8, "statement-type response buffer is too short");
		if (response_length >= 8) {
			memset(response, 0, (size_t) response_length);
			response[0] = isc_info_sql_stmt_type;
			response[1] = 4;
			response[3] = (char) (is_catalog_statement(statement) ?
				isc_info_sql_stmt_select : statement_type);
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
	int failed;

	(void) transaction;
	check(dialect == expected_dialect, "execute used the wrong SQL dialect");
	check_cancel_publication(statement, &execute_publication_calls,
		"prepared execute did not publish its statement handle");
	if (overlap_native_call(OVERLAP_EXECUTE) ||
		overlap_native_call(OVERLAP_TRANSIENT_EXECUTE) ||
		overlap_native_call(OVERLAP_DISTRIBUTED_EXECUTE)) {
		return status_code(status, isc_cancelled);
	}
	execute_calls++;
	if (input != NULL && input->sqld == 1 && input->sqlvar[0].sqldata != NULL) {
		memcpy(&last_execute_value, input->sqlvar[0].sqldata,
			sizeof(last_execute_value));
	}
	failed = fail_execute_once-- > 0;
	if (!failed) {
		persisted_execute_value = last_execute_value;
	}
	return status_result(status, failed);
}

ISC_STATUS ISC_EXPORT test_dsql_execute2(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short dialect, XSQLDA *input, XSQLDA *output)
{
	(void) transaction;
	check(dialect == expected_dialect, "execute2 used the wrong SQL dialect");
	check_cancel_publication(statement, &execute2_publication_calls,
		"prepared execute2 did not publish its statement handle");
	(void) input;
	(void) output;
	execute2_calls++;
	if (is_catalog_statement(statement)) {
		if (overlap_native_call(OVERLAP_CATALOG_EXECUTE2)) {
			return status_code(status, isc_cancelled);
		}
		if (catalog_charset_identifier && input != NULL && input->sqld == 1 &&
			input->sqlvar[0].sqldata != NULL &&
			ib_sql_type(&input->sqlvar[0]) == SQL_TEXT &&
			input->sqlvar[0].sqllen == 1 &&
			(unsigned char) input->sqlvar[0].sqldata[0] == 0x8e &&
			ib_text_charset(&input->sqlvar[0]) == 51) {
			catalog_lookup_bytes_ok = 1;
		}
		catalog_statement_open = 1;
		catalog_fetch_rows_remaining = 1;
		return status_result(status, 0);
	}
	if (overlap_native_call(OVERLAP_EXECUTE2) ||
		overlap_native_call(OVERLAP_TRANSIENT_EXECUTE2)) {
		return status_code(status, isc_cancelled);
	}
	user_execute2_calls++;
	if (fail_execute2_once-- > 0) {
		return status_result(status, 1);
	}
	if (statement_type == isc_info_sql_stmt_exec_procedure && output != NULL &&
		output->sqld == 1 && output->sqlvar[0].sqldata != NULL) {
		int64_t value = 123;
		memcpy(output->sqlvar[0].sqldata, &value, sizeof(value));
	}
	if (statement_type == isc_info_sql_stmt_exec_procedure) {
		return status_result(status, 0);
	}
	open_selects++;
	execute2_open = 1;
	fetch_rows_remaining = 1;
	return status_result(status, 0);
}

static void test_executable_procedure_output_and_reuse(void)
{
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	ib_value_view view;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 1;
	connection = new_connection();
	statement = prepare_statement(connection, statement_type);
	check(statement != NULL && statement->output != NULL && statement->output->sqld == 1,
		"executable procedure output descriptor was not retained");
	if (statement == NULL) {
		free(connection);
		return;
	}

	bindings = new_integer_binding(10);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL && execute2_calls == 1,
		"executable procedure query did not execute through execute2");
	check(fetch_calls == 0, "executable procedure query unexpectedly fetched a row");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_next(cursor, &error) == 1 && error == NULL,
			"executable procedure did not expose its output row");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_column(cursor, 0, &view, &error) == 0 && error == NULL &&
			view.kind == IB_VALUE_INT64 && view.int64_value == 123,
			"executable procedure output row was not populated");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_next(cursor, &error) == 0 && error == NULL,
			"executable procedure returned more than one output row");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
			"executable procedure cursor close failed");
		ib_error_free(error);
	}
	check(commit_calls == 2 && rollback_calls == 0,
		"implicit executable procedure query did not commit its transaction");

	bindings = new_integer_binding(20);
	error = NULL;
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL && execute2_calls == 2 && fetch_calls == 0,
		"prepared executable procedure handle was not reusable");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
			"early-close executable procedure cursor failed");
		ib_error_free(error);
	}
	check(commit_calls == 3 && rollback_calls == 0,
		"early-close executable procedure did not commit its transaction");

	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"executable procedure statement close failed");
	ib_error_free(error);
	free(connection);
}

static void test_executable_procedure_exec_contracts(void)
{
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	char *error = NULL;
	int64_t affected;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 1;
	connection = new_connection();
	statement = prepare_statement(connection, statement_type);
	check(statement != NULL, "output procedure statement preparation failed");
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(1);
	check(ib_statement_exec(statement, bindings, &affected, &error) != 0 &&
		error != NULL && execute_calls == 0,
		"Exec discarded executable procedure output or executed it");
	ib_error_free(error);
	ib_bindings_free(bindings);
	error = NULL;
	(void) ib_statement_close(statement, &error);
	ib_error_free(error);
	free(connection);

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 0;
	connection = new_connection();
	statement = prepare_statement(connection, statement_type);
	check(statement != NULL && statement->output != NULL && statement->output->sqld == 0,
		"zero-output procedure descriptor was not retained");
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(2);
	error = NULL;
	check(ib_statement_exec(statement, bindings, &affected, &error) == 0 &&
		error == NULL && execute_calls == 1 && commit_calls == 2,
		"zero-output procedure was not executable through Exec");
	ib_error_free(error);
	ib_bindings_free(bindings);
	error = NULL;
	(void) ib_statement_close(statement, &error);
	ib_error_free(error);
	free(connection);
}

static void test_executable_procedure_failure_and_abort_rollback(void)
{
	ib_connection *connection;
	struct ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 1;
	connection = new_connection();
	statement = prepare_statement(connection, statement_type);
	check(statement != NULL, "failure procedure statement preparation failed");
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(3);
	fail_execute2_once = 1;
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor == NULL && error != NULL && rollback_calls == 1 && execute2_calls == 1,
		"failed executable procedure did not roll back without replaying execution");
	ib_error_free(error);
	ib_bindings_free(bindings);

	bindings = new_integer_binding(4);
	error = NULL;
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL && execute2_calls == 2,
		"executable procedure handle did not recover after execution failure");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_abort(cursor, &error) == 0 && error == NULL,
			"aborting executable procedure output did not clean up");
		ib_error_free(error);
	}
	check(rollback_calls == 2 && commit_calls == 1,
		"executable procedure output/decode abort did not roll back its transaction");

	error = NULL;
	(void) ib_statement_close(statement, &error);
	ib_error_free(error);
	free(connection);
}

static void test_catalog_metadata_cleanup_drops_owned_statement(void)
{
	ib_connection *connection;
	ib_cursor cursor = {0};
	char *error = NULL;

	reset_mocks();
	connection = new_connection();
	cursor.connection = connection;
	cursor.transaction = &transaction_tokens[0];
	cursor.output = ib_alloc_sqlda(1);
	check(cursor.output != NULL, "catalog metadata test output allocation failed");
	if (cursor.output == NULL) {
		free(connection);
		return;
	}
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_LONG | 1;
	cursor.output->sqlvar[0].sqllen = (short) sizeof(ISC_LONG);
	cursor.output->sqlvar[0].relname_length = 1;
	cursor.output->sqlvar[0].relname[0] = 'T';
	cursor.output->sqlvar[0].sqlname_length = 2;
	cursor.output->sqlvar[0].sqlname[0] = 'I';
	cursor.output->sqlvar[0].sqlname[1] = 'D';

	check(ib_cursor_describe_metadata(&cursor, &error) == 0 && error == NULL,
		"catalog metadata lookup failed");
	check(cursor.metadata != NULL && cursor.metadata[0].sql_type == IB_METADATA_INTEGER,
		"catalog metadata lookup did not apply the matching field");
	check(catalog_drop_calls == 1 && catalog_close_calls == 0,
		"catalog-owned statement was closed instead of dropped");
	ib_error_free(error);
	ib_cursor_free_parts(&cursor);
	free(connection);
}

static void test_catalog_metadata_respects_connection_charset(void)
{
	static const unsigned char relation[] = {0x8e};
	static const unsigned char field[] = {0x8a};
	ib_connection *connection;
	ib_cursor cursor = {0};
	char *error = NULL;

	reset_mocks();
	describe_user_charset_identifier = 1;
	catalog_charset_identifier = 1;
	connection = new_connection();
	connection->charset = 51;
	cursor.connection = connection;
	cursor.transaction = &transaction_tokens[0];
	cursor.output = ib_alloc_sqlda(1);
	check(cursor.output != NULL, "charset catalog metadata output allocation failed");
	if (cursor.output == NULL) {
		free(connection);
		return;
	}
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_LONG | 1;
	cursor.output->sqlvar[0].sqllen = (short) sizeof(ISC_LONG);
	memcpy(cursor.output->sqlvar[0].relname, relation, sizeof(relation));
	cursor.output->sqlvar[0].relname_length = (short) sizeof(relation);
	memcpy(cursor.output->sqlvar[0].sqlname, field, sizeof(field));
	cursor.output->sqlvar[0].sqlname_length = (short) sizeof(field);

	check(ib_cursor_describe_metadata(&cursor, &error) == 0 && error == NULL,
		"charset catalog metadata lookup failed");
	check(cursor.column_names != NULL &&
		strcmp(cursor.column_names[0], "\xc5\xa0") == 0,
		"catalog metadata did not decode the connection-charset column name");
	check(catalog_lookup_bytes_ok,
		"catalog metadata did not retain raw connection-charset lookup bytes");
	check(cursor.metadata != NULL && cursor.metadata[0].sql_precision == 9,
		"charset catalog metadata lookup did not apply the matching field");
	ib_error_free(error);
	ib_cursor_free_parts(&cursor);
	free(connection);
}

static void test_procedure_commit_failure_rolls_back_live_handle(void)
{
	ib_connection *connection;
	ib_cursor *cursor;
	char *error = NULL;
	int result;

	reset_mocks();
	connection = new_connection();
	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	check(cursor != NULL, "commit failure cursor allocation failed");
	if (cursor == NULL) {
		free(connection);
		return;
	}
	cursor->connection = connection;
	cursor->transaction = &transaction_tokens[0];
	cursor->owns_transaction = 1;
	cursor->procedure = 1;
	ib_cursor_register(cursor);
	fail_commit = 1;
	fail_rollback = 1;
	result = ib_cursor_close(cursor, &error);
	check(result == -1 && commit_calls == 1 && rollback_calls == 1 &&
		rollback_saw_live_transaction, "procedure commit failure did not attempt rollback");
	check(connection->broken, "procedure commit/rollback failure did not invalidate connection");
	check(error != NULL && strstr(error, "commit procedure transaction") != NULL &&
		strstr(error, "rollback failed procedure transaction") != NULL,
		"procedure commit failure did not preserve cleanup diagnostics");
	ib_error_free(error);
	free(connection);
}

static void test_direct_procedure_transition_failures_free_query(void)
{
	static const char query[] = "EXECUTE PROCEDURE P(?)";
	struct {
		const char *name;
		int start_failure;
		int prepare_failure;
		int rollback_failure;
		int sql_info_failure;
	} cases[] = {
		{"rollback", 0, 0, 1, 0},
		{"write start", 2, 0, 0, 0},
		{"second prepare", 0, 2, 0, 0},
		{"second type lookup", 0, 0, 0, 2}
	};
	size_t index;

	for (index = 0U; index < sizeof(cases) / sizeof(cases[0]); index++) {
		ib_connection *connection;
		ib_bindings *bindings;
		ib_cursor *cursor;
		char *error = NULL;

		reset_mocks();
		statement_type = isc_info_sql_stmt_exec_procedure;
		fail_start_on_call = cases[index].start_failure;
		fail_prepare_on_call = cases[index].prepare_failure;
		fail_rollback = cases[index].rollback_failure;
		fail_sql_info_on_call = cases[index].sql_info_failure;
		connection = new_connection();
		bindings = new_integer_binding((int64_t) index + 1);
		cursor = ib_connection_query(connection, query, sizeof(query) - 1U,
			bindings, 0, &error);
		check(cursor == NULL && error != NULL && connection->cursors == NULL,
			cases[index].name);
		ib_error_free(error);
		ib_bindings_free(bindings);
		free(connection);
	}
}

static void test_procedure_transition_drop_failure_retires_connection(void)
{
	static const char query[] = "EXECUTE PROCEDURE P(?)";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;
	int prepare_count;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	fail_drop = 1;
	connection = new_connection();
	bindings = new_integer_binding(9);
	cursor = ib_connection_query(connection, query, sizeof(query) - 1U,
		bindings, 0, &error);
	check(cursor == NULL && error != NULL && rollback_calls == 1 &&
		failed_drop_left_handle && connection->broken,
		"procedure transition drop failure did not retire the connection");
	check(error != NULL && strstr(error, "close statement") != NULL,
		"procedure transition drop failure lost cleanup diagnostics");
	ib_error_free(error);
	ib_bindings_free(bindings);
	prepare_count = prepare_calls;

	error = NULL;
	bindings = new_integer_binding(10);
	cursor = ib_connection_query(connection, query, sizeof(query) - 1U,
		bindings, 0, &error);
	check(cursor == NULL && error != NULL && prepare_calls == prepare_count &&
		execute2_calls == 0,
		"broken connection reused a live procedure transition handle");
	ib_error_free(error);
	ib_bindings_free(bindings);
	free(connection);
}

static void test_catalog_drop_failure_stops_user_execution(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	describe_user_column_relation = 1;
	fail_catalog_drop = 1;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	bindings = new_integer_binding(1);
	check(bindings != NULL && error == NULL, "catalog drop failure bindings allocation failed");
	if (bindings == NULL) {
		ib_error_free(error);
		free(connection);
		return;
	}
	cursor = ib_connection_query(connection, query, sizeof(query) - 1U,
		bindings, 0, &error);
	check(catalog_drop_calls == 1 && catalog_drop_left_handle,
		"catalog drop fault injection did not leave its handle live");
	check(cursor == NULL, "catalog drop failure returned a user cursor");
	check(connection->broken, "catalog drop failure did not retire the connection");
	check(user_execute2_calls == 0,
		"catalog drop failure executed the user statement");
	check(connection->transaction == &transaction_tokens[0],
		"catalog drop failure lost the explicit transaction");
	check(error != NULL && strstr(error, "close statement") != NULL,
		"catalog drop failure lost its cleanup diagnostic");
	ib_error_free(error);
	ib_bindings_free(bindings);
	free(connection);
}

ISC_STATUS ISC_EXPORT test_dsql_fetch(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output)
{
	check(dialect == expected_dialect, "fetch used the wrong SQL dialect");
	check_cancel_publication(statement, &fetch_publication_calls,
		"prepared fetch did not publish its statement handle");
	if (is_catalog_statement(statement)) {
		fetch_calls++;
		if (!catalog_statement_open) {
			return status_result(status, 1);
		}
		if (catalog_fetch_rows_remaining > 0) {
			catalog_fetch_rows_remaining--;
			if (procedure_catalog_statement) {
				set_catalog_text(&output->sqlvar[0], "P");
				set_catalog_text(&output->sqlvar[1], "OUT");
				set_catalog_integer(&output->sqlvar[2], 16);
				set_catalog_integer(&output->sqlvar[3], 1);
				set_catalog_integer(&output->sqlvar[4], procedure_catalog_scale);
				set_catalog_integer(&output->sqlvar[5], procedure_catalog_precision);
				set_catalog_null(&output->sqlvar[6]);
			} else {
				if (catalog_charset_identifier) {
					static const unsigned char relation[] = {0x8e};
					static const unsigned char field[] = {0x8a};
					set_catalog_bytes(&output->sqlvar[0], relation, sizeof(relation));
					set_catalog_bytes(&output->sqlvar[1], field, sizeof(field));
				} else {
					set_catalog_text(&output->sqlvar[0], "T");
					set_catalog_text(&output->sqlvar[1], "ID");
				}
				set_catalog_integer(&output->sqlvar[2], 8);
				set_catalog_integer(&output->sqlvar[3], 0);
				set_catalog_integer(&output->sqlvar[4], (ISC_LONG) sizeof(ISC_LONG));
				set_catalog_integer(&output->sqlvar[5], 0);
				if (catalog_charset_identifier) {
					set_catalog_integer(&output->sqlvar[6], 9);
				} else {
					set_catalog_null(&output->sqlvar[6]);
				}
				set_catalog_null(&output->sqlvar[7]);
				set_catalog_null(&output->sqlvar[8]);
				set_catalog_integer(&output->sqlvar[9], 1);
			}
			return status_result(status, 0);
		}
		return status_result(status, 0) == 0 ? 100 : -1;
	}
	(void) output;
	if (overlap_native_call(OVERLAP_FETCH)) {
		return status_code(status, isc_cancelled);
	}
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
	if (option == DSQL_cancel && overlap_is_enabled()) {
		overlap_cancel_statement = statement == NULL ? NULL : *statement;
		return overlap_cancel_call(status);
	}
	if (option != DSQL_cancel && overlap_is_enabled()) {
		overlap_cleanup_event();
	}
	if (expected_cancel_slot != NULL && statement != NULL &&
		*statement == expected_cancel_statement && option != DSQL_cancel) {
		check_cancel_complete("prepared cancellation slot was not complete before statement cleanup");
	}
	if (*statement == catalog_statement) {
		if (option == DSQL_close) {
			catalog_close_calls++;
		} else {
			check(option == DSQL_drop, "catalog cleanup used an unexpected option");
			catalog_drop_calls++;
			if (fail_catalog_drop) {
				catalog_drop_left_handle = *statement != NULL;
				return status_result(status, 1);
			}
			*statement = NULL;
		}
		catalog_statement_open = 0;
		return status_result(status, 0);
	}
	if (option == DSQL_close) {
		check(execute2_open, "cursor cleanup must close an open prepared SELECT");
		close_calls++;
		if (open_selects > 0) {
			open_selects--;
		}
		execute2_open = open_selects != 0;
	} else {
		check(option == DSQL_drop, "statement cleanup must drop the prepared handle");
		free_statement_calls++;
		if (fail_drop) {
			failed_drop_left_handle = *statement != NULL;
			return status_result(status, 1);
		}
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

static void test_descriptor_aliases_are_exposed_as_utf8(void)
{
	static const char query[] = "SELECT ID AS ALIAS FROM T";
	static const char want[] = "P\xc5\x99\xc3\xadli\xc5\xa1";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	const char *name;
	size_t name_length;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	describe_user_column_alias = 1;
	connection = new_connection();
	connection->charset = 51;
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL, "WIN1250 alias statement preparation failed");
	ib_error_free(error);
	error = NULL;
	if (statement == NULL) {
		free(connection);
		return;
	}

	bindings = new_integer_binding(1);
	check(bindings != NULL && error == NULL, "WIN1250 alias bindings allocation failed");
	ib_error_free(error);
	error = NULL;
	cursor = bindings == NULL ? NULL : ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL, "WIN1250 alias query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		name = ib_cursor_column_name(cursor, 0U, &name_length);
		check(name != NULL && name_length == sizeof(want) - 1U &&
			memcmp(name, want, name_length) == 0,
			"WIN1250 descriptor alias was not converted to UTF-8");
		error = NULL;
		(void) ib_cursor_close(cursor, &error);
		ib_error_free(error);
	}
	error = NULL;
	(void) ib_statement_close(statement, &error);
	ib_error_free(error);
	free(connection);
}

static void test_procedure_descriptor_without_source_remains_unknown(void)
{
	static const char query[] = "EXECUTE PROCEDURE P(?)";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	ib_column_metadata metadata;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	describe_procedure_decimal = 1;
	connection = new_connection();
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"procedure precision statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	error = NULL;
	bindings = new_integer_binding(1);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL, "procedure precision query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_column_metadata(cursor, 0U, &metadata, &error) == 0 &&
			error == NULL && metadata.sql_type == IB_METADATA_BIGINT &&
			metadata.sql_subtype == 1 && metadata.sql_scale == -2 &&
			metadata.sql_precision == 18 && metadata.precision == 0 &&
			metadata.scale == 0 && metadata.has_precision_scale == 0,
			"procedure descriptor without a source was treated as declared metadata");
		ib_error_free(error);
		error = NULL;
		(void) ib_cursor_close(cursor, &error);
		ib_error_free(error);
	}
	error = NULL;
	(void) ib_statement_close(statement, &error);
	ib_error_free(error);
	free(connection);
}

static void test_broken_close_aborts_pending_procedure_sibling(void)
{
	static const char procedure_query[] = "EXECUTE PROCEDURE P(?)";
	static const char select_query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;
	int commit_before_failure;
	int rollback_before_failure;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 1;
	connection = new_connection();
	statement = ib_statement_prepare(connection, procedure_query,
		sizeof(procedure_query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"pending procedure statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}

	bindings = new_integer_binding(1);
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL,
		"pending procedure query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor == NULL) {
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}
	commit_before_failure = commit_calls;
	rollback_before_failure = rollback_calls;

	statement_type = isc_info_sql_stmt_select;
	fail_start_on_call = start_calls + 1;
	bindings = new_integer_binding(2);
	error = NULL;
	check(ib_connection_query(connection, select_query, sizeof(select_query) - 1U,
			bindings, 0, &error) == NULL && error != NULL && connection->broken,
		"sibling transaction-start failure did not invalidate the connection");
	ib_error_free(error);
	ib_bindings_free(bindings);

	error = NULL;
	check(ib_connection_close(connection, &error) == 0 && error == NULL,
		"broken connection cleanup failed");
	check(commit_calls == commit_before_failure &&
		rollback_calls == rollback_before_failure + 2,
		"broken connection cleanup committed a pending procedure sibling");
	ib_error_free(error);
}

static void test_cursor_close_failure_aborts_later_procedure_sibling(void)
{
	static const char procedure_query[] = "EXECUTE PROCEDURE P(?)";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *procedure_cursor;
	ib_cursor *failing_cursor;
	char *error = NULL;
	int commit_before_failure;
	int rollback_before_failure;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 1;
	connection = new_connection();
	statement = ib_statement_prepare(connection, procedure_query,
		sizeof(procedure_query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"cursor-close sibling procedure preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}

	bindings = new_integer_binding(1);
	procedure_cursor = ib_statement_query(statement, bindings, &error);
	check(procedure_cursor != NULL && error == NULL,
		"cursor-close sibling procedure query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (procedure_cursor == NULL) {
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}

	/* Register a cursor that fails first. Cleanup marks the connection broken;
	 * the later procedure must then roll back rather than commit. */
	failing_cursor = (ib_cursor *) calloc(1U, sizeof(*failing_cursor));
	check(failing_cursor != NULL, "cursor-close sibling failure cursor allocation failed");
	if (failing_cursor == NULL) {
		error = NULL;
		(void) ib_cursor_close(procedure_cursor, &error);
		ib_error_free(error);
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}
	failing_cursor->connection = connection;
	failing_cursor->transaction = &transaction_tokens[5];
	failing_cursor->owns_transaction = 1;
	ib_cursor_register(failing_cursor);

	commit_before_failure = commit_calls;
	rollback_before_failure = rollback_calls;
	fail_rollback = 1;
	error = NULL;
	check(ib_close_all_cursors(connection, &error, 1) != 0 &&
		connection->broken,
		"cursor-close sibling failure did not invalidate the connection");
	ib_error_free(error);
	check(commit_calls == commit_before_failure &&
		rollback_calls == rollback_before_failure + 2,
		"cursor-close failure committed a later procedure sibling");

	fail_rollback = 0;
	error = NULL;
	check(ib_connection_close(connection, &error) == 0 && error == NULL,
		"cursor-close sibling failure cleanup failed");
	ib_error_free(error);
}

static void test_procedure_catalog_precision_overrides_sqlda(void)
{
	static const char query[] = "EXECUTE PROCEDURE P(?)";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	ib_column_metadata metadata;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	describe_procedure_decimal = 1;
	describe_procedure_source = 1;
	procedure_catalog_precision = 4;
	procedure_catalog_scale = -2;
	connection = new_connection();
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"procedure catalog precision statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}

	bindings = new_integer_binding(1);
	error = NULL;
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL, "procedure catalog precision query failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		check(ib_cursor_column_metadata(cursor, 0U, &metadata, &error) == 0 &&
			error == NULL && metadata.sql_precision == 4 && metadata.precision == 4 &&
			metadata.sql_scale == -2 && metadata.scale == -2 &&
			metadata.has_precision_scale != 0,
			"procedure catalog metadata did not override the reserved SQLDA precision");
		ib_error_free(error);
		error = NULL;
		(void) ib_cursor_close(cursor, &error);
		ib_error_free(error);
	}
	error = NULL;
	(void) ib_statement_close(statement, &error);
	ib_error_free(error);
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

static void test_cursors_do_not_use_a_connection_global_lock(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	static const char write_query[] = "INSERT INTO T (ID) VALUES (?)";
	ib_connection *connection;
	ib_statement *first_statement;
	ib_statement *second_statement;
	ib_statement *write_statement;
	ib_bindings *bindings;
	ib_cursor *first_cursor;
	ib_cursor *second_cursor;
	ib_cursor *rejected_cursor;
	char *error = NULL;
	int64_t affected;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	first_statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(first_statement != NULL && error == NULL,
		"first cursor-lock statement preparation failed");
	ib_error_free(error);
	if (first_statement == NULL) {
		free(connection);
		return;
	}
	error = NULL;
	second_statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(second_statement != NULL && error == NULL,
		"second cursor-lock statement preparation failed");
	ib_error_free(error);
	if (second_statement == NULL) {
		error = NULL;
		(void) ib_statement_close(first_statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}

	bindings = new_integer_binding(1);
	first_cursor = ib_statement_query(first_statement, bindings, &error);
	check(first_cursor != NULL && error == NULL,
		"first prepared cursor failed");
	ib_error_free(error);
	error = NULL;
	ib_bindings_free(bindings);
	if (first_cursor == NULL) {
		error = NULL;
		(void) ib_statement_close(second_statement, &error);
		ib_error_free(error);
		error = NULL;
		(void) ib_statement_close(first_statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}

	bindings = new_integer_binding(2);
	second_cursor = ib_statement_query(second_statement, bindings, &error);
	check(second_cursor != NULL && error == NULL && open_selects == 2,
		"second prepared cursor was blocked by another cursor");
	ib_error_free(error);
	error = NULL;
	ib_bindings_free(bindings);

	bindings = new_integer_binding(3);
	error = NULL;
	rejected_cursor = ib_statement_query(first_statement, bindings, &error);
	check(rejected_cursor == NULL && error != NULL,
		"one prepared statement allowed two active executions");
	ib_error_free(error);
	error = NULL;
	ib_bindings_free(bindings);

	statement_type = isc_info_sql_stmt_insert;
	write_statement = ib_statement_prepare(connection, write_query,
		sizeof(write_query) - 1U, &error);
	check(write_statement != NULL && error == NULL,
		"write statement preparation with active cursors failed");
	ib_error_free(error);
	error = NULL;
	if (write_statement != NULL) {
		bindings = new_integer_binding(4);
		error = NULL;
		check(ib_statement_exec(write_statement, bindings, &affected, &error) == 0 &&
			error == NULL && affected == 1,
			"write execution was blocked by active cursors");
		ib_error_free(error);
		ib_bindings_free(bindings);
		error = NULL;
		(void) ib_statement_close(write_statement, &error);
		ib_error_free(error);
	}

	if (first_cursor != NULL) {
		error = NULL;
		check(ib_cursor_close(first_cursor, &error) == 0 && error == NULL,
			"first prepared cursor close failed");
		ib_error_free(error);
	}
	if (second_cursor != NULL) {
		error = NULL;
		check(ib_cursor_next(second_cursor, &error) == 1 && error == NULL,
			"second prepared cursor was invalidated by first cursor close");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_close(second_cursor, &error) == 0 && error == NULL,
			"second prepared cursor close failed");
		ib_error_free(error);
	}
	check(open_selects == 0, "prepared cursor close leaked an open server cursor");
	error = NULL;
	(void) ib_statement_close(second_statement, &error);
	ib_error_free(error);
	error = NULL;
	(void) ib_statement_close(first_statement, &error);
	ib_error_free(error);
	free(connection);
}

static void test_select_for_update_requires_explicit_writable_transaction(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ? WITH LOCK";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select_for_upd;
	connection = new_connection();
	bindings = new_integer_binding(1);
	cursor = ib_connection_query(connection, query, sizeof(query) - 1U,
		bindings, 0, &error);
	check(cursor == NULL && error != NULL && execute2_calls == 0 &&
		start_calls == 1 && rollback_calls == 1 && connection->cursors == NULL,
		"direct SELECT FOR UPDATE was not rejected without an explicit writable transaction");
	ib_error_free(error);
	error = NULL;
	ib_bindings_free(bindings);
	free(connection);

	reset_mocks();
	statement_type = isc_info_sql_stmt_select_for_upd;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	connection->transaction_read_only = 1;
	bindings = new_integer_binding(2);
	cursor = ib_connection_query(connection, query, sizeof(query) - 1U,
		bindings, 0, &error);
	check(cursor == NULL && error != NULL && execute2_calls == 0 &&
		start_calls == 0 && connection->cursors == NULL,
		"direct SELECT FOR UPDATE was not rejected in a read-only transaction");
	ib_error_free(error);
	error = NULL;
	ib_bindings_free(bindings);
	free(connection);

	reset_mocks();
	statement_type = isc_info_sql_stmt_select_for_upd;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"SELECT FOR UPDATE prepared statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	bindings = new_integer_binding(3);
	error = NULL;
	cursor = ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL && execute2_calls == 1,
		"SELECT FOR UPDATE was rejected in an explicit writable transaction");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		error = NULL;
		(void) ib_cursor_close(cursor, &error);
		ib_error_free(error);
	}
	error = NULL;
	(void) ib_statement_close(statement, &error);
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
		&affected, 0, &error) == 0 && error == NULL && execute_calls == 1 &&
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
	check(cursor != NULL && error == NULL && statement->active_cursor == cursor,
		"active prepared cursor was not registered on the connection");
	ib_error_free(error);
	ib_bindings_free(bindings);
	error = NULL;
	check(ib_connection_commit(connection, &error) == 0 && error == NULL,
		"transaction completion with active prepared cursor failed");
	ib_error_free(error);
	check(connection->cursors == NULL && statement->active_cursor == NULL &&
		free_statement_calls == 0 &&
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

static void test_cancel_overlap_execute(void)
{
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	connection = new_connection();
	statement = prepare_statement(connection, isc_info_sql_stmt_insert);
	bindings = new_integer_binding(51);
	slot = ib_cancel_slot_new(&error);
	check(statement != NULL && bindings != NULL && slot != NULL && error == NULL,
		"delayed execute setup failed");
	ib_error_free(error);
	if (statement == NULL || bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "delayed execute slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = statement->statement;
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_EXECUTE;
	operation.statement = statement;
	operation.bindings = bindings;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_EXECUTE, 1, 0);
	run_delayed_cancel(&operation, &cancel, 0,
		"prepared execute completed before delayed cancellation");
	check(operation.result != 0 && operation.error != NULL,
		"delayed prepared execute did not return its native cancellation error");
	check(operation.abort_result == 0 && operation.abort_error == NULL,
		"delayed prepared execute cleanup failed");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"prepared execute could not reuse its statement after cancellation cleanup");
	ib_error_free(error);
	free(connection);
}

static void test_cancel_overlap_execute2(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"delayed execute2 statement preparation failed");
	ib_error_free(error);
	bindings = new_integer_binding(52);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"delayed execute2 setup failed");
	ib_error_free(error);
	if (statement == NULL || bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "delayed execute2 slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = statement->statement;
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_EXECUTE2;
	operation.statement = statement;
	operation.bindings = bindings;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_EXECUTE2, 1, 0);
	run_delayed_cancel(&operation, &cancel, 0,
		"prepared execute2 completed before delayed cancellation");
	check(operation.cursor == NULL && operation.error != NULL,
		"delayed prepared execute2 returned a cursor after native cancellation");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"prepared execute2 statement close after cancellation failed");
	ib_error_free(error);
	free(connection);
}

static void test_cancel_overlap_fetch_and_reuse(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_statement *statement;
	ib_statement *other_statement;
	ib_bindings *bindings;
	ib_bindings *reuse_bindings;
	ib_bindings *other_bindings;
	ib_cancel_slot *slot;
	ib_cursor *cursor;
	ib_cursor *reuse_cursor;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	int64_t rows_affected;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	statement = ib_statement_prepare(connection, query, sizeof(query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"delayed fetch statement preparation failed");
	ib_error_free(error);
	bindings = new_integer_binding(53);
	cursor = bindings == NULL ? NULL :
		ib_statement_query(statement, bindings, &error);
	check(cursor != NULL && error == NULL,
		"delayed fetch cursor setup failed");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (statement == NULL || cursor == NULL) {
		free(connection);
		return;
	}
	slot = ib_cancel_slot_new(&error);
	check(slot != NULL && error == NULL, "delayed fetch slot allocation failed");
	ib_error_free(error);
	if (slot == NULL) {
		error = NULL;
		(void) ib_cursor_abort(cursor, &error);
		ib_error_free(error);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "delayed fetch slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = statement->statement;
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_FETCH;
	operation.cursor = cursor;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_FETCH, 1, 1);
	run_delayed_cancel(&operation, &cancel, 1,
		"prepared fetch completed before delayed cancellation");
	check(operation.has_row < 0 && operation.error != NULL,
		"delayed prepared fetch did not return its native cancellation error");
	check(operation.abort_result == 0 && operation.abort_error == NULL,
		"delayed prepared fetch cleanup failed");
	check(cancel.native_code != 0,
		"delayed prepared fetch did not expose cancellation-call failure");
	ib_error_free(operation.error);
	ib_error_free(operation.abort_error);
	ib_error_free(cancel.error);
	check(connection->transaction == &transaction_tokens[0],
		"canceled fetch cleanup lost the explicit transaction");
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);

	reuse_bindings = new_integer_binding(54);
	error = NULL;
	reuse_cursor = reuse_bindings == NULL ? NULL :
		ib_statement_query(statement, reuse_bindings, &error);
	check(reuse_cursor != NULL && error == NULL,
		"prepared statement was not reusable after canceled fetch cleanup");
	ib_error_free(error);
	ib_bindings_free(reuse_bindings);
	if (reuse_cursor != NULL) {
		error = NULL;
		check(ib_cursor_next(reuse_cursor, &error) == 1 && error == NULL,
			"reused prepared statement did not fetch after cancellation cleanup");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_close(reuse_cursor, &error) == 0 && error == NULL,
			"reused prepared cursor close failed");
		ib_error_free(error);
	}
	check(connection->transaction == &transaction_tokens[0],
		"prepared fetch reuse changed the explicit transaction");

	statement_type = isc_info_sql_stmt_insert;
	other_statement = prepare_statement(connection, isc_info_sql_stmt_insert);
	other_bindings = new_integer_binding(55);
	rows_affected = -1;
	error = NULL;
	check(other_statement != NULL && other_bindings != NULL &&
		(ib_statement_exec)(other_statement, other_bindings, NULL, 0U,
			&rows_affected, &error) == 0 && error == NULL,
		"another prepared statement did not execute after canceled fetch cleanup");
	ib_error_free(error);
	ib_bindings_free(other_bindings);
	if (other_statement != NULL) {
		error = NULL;
		check(ib_statement_close(other_statement, &error) == 0 && error == NULL,
			"follow-up prepared statement close failed");
		ib_error_free(error);
	}
	check(connection->transaction == &transaction_tokens[0],
		"follow-up prepared execution changed the explicit transaction");
	error = NULL;
	check(ib_connection_rollback(connection, &error) == 0 && error == NULL,
		"explicit transaction rollback after prepared cancellation reuse failed");
	ib_error_free(error);
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"reused prepared statement close failed");
	ib_error_free(error);
	free(connection);
}

static void test_cancel_publication_and_cleanup_order(void)
{
	static const char execute_query[] = "INSERT INTO T (ID) VALUES (?)";
	static const char query_text[] = "SELECT ID FROM T WHERE ID = ?";
	ib_cancel_slot *slot;
	ib_connection *connection;
	ib_statement *statement;
	ib_bindings *bindings;
	ib_cursor *cursor;
	char *error = NULL;
	int64_t affected;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_insert;
	connection = new_connection();
	statement = ib_statement_prepare(connection, execute_query,
		sizeof(execute_query) - 1U, &error);
	check(statement != NULL && error == NULL,
		"cancel publication execute statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	slot = ib_cancel_slot_new(&error);
	check(slot != NULL && error == NULL, "cancel publication execute slot allocation failed");
	ib_error_free(error);
	if (slot == NULL) {
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "cancel publication execute slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = statement->statement;
	bindings = new_integer_binding(41);
	error = NULL;
	check((ib_statement_exec)(statement, bindings, slot, generation,
		&affected, &error) == 0 && error == NULL,
		"cancel publication prepared execute failed");
	check(execute_publication_calls == 1,
		"prepared execute publication was not observed exactly once");
	check_cancel_complete("prepared execute did not complete its cancellation slot");
	ib_error_free(error);
	ib_bindings_free(bindings);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"cancel publication execute statement close failed");
	ib_error_free(error);
	free(connection);

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	connection = new_connection();
	statement = ib_statement_prepare(connection, query_text, sizeof(query_text) - 1U, &error);
	check(statement != NULL && error == NULL,
		"cancel publication query statement preparation failed");
	ib_error_free(error);
	if (statement == NULL) {
		free(connection);
		return;
	}
	slot = ib_cancel_slot_new(&error);
	check(slot != NULL && error == NULL, "cancel publication query slot allocation failed");
	ib_error_free(error);
	if (slot == NULL) {
		error = NULL;
		(void) ib_statement_close(statement, &error);
		ib_error_free(error);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "cancel publication query slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = statement->statement;
	bindings = new_integer_binding(42);
	error = NULL;
	cursor = (ib_statement_query)(statement, bindings, slot, generation, &error);
	check(cursor != NULL && error == NULL,
		"cancel publication prepared query failed");
	check(execute2_publication_calls == 1,
		"prepared execute2 publication was not observed exactly once");
	check_cancel_complete("prepared execute2 did not complete its cancellation slot");
	ib_error_free(error);
	ib_bindings_free(bindings);
	if (cursor != NULL) {
		generation = ib_cancel_slot_begin(slot, &error);
		check(generation != 0U && error == NULL,
			"cancel publication fetch slot begin failed");
		ib_error_free(error);
		expected_cancel_generation = generation;
		error = NULL;
		check((ib_cursor_next)(cursor, slot, generation, &error) == 1 && error == NULL,
			"cancel publication prepared fetch failed");
		check(fetch_publication_calls == 1,
			"prepared fetch publication was not observed exactly once");
		check_cancel_complete("prepared fetch did not complete its cancellation slot");
		ib_error_free(error);
		error = NULL;
		check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
			"cancel publication cursor close failed");
		ib_error_free(error);
	}
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
	error = NULL;
	check(ib_statement_close(statement, &error) == 0 && error == NULL,
		"cancel publication query statement close failed");
	ib_error_free(error);
	free(connection);
}

static void test_transient_catalog_fallback_and_cancelled_propagation(void)
{
	static const char select_query[] = "SELECT ID FROM T WHERE ID = ?";
	static const char procedure_query[] = "EXECUTE PROCEDURE P(?)";
	struct catalog_case {
		const char *query;
		int statement_kind;
		int procedure_source;
	};
	static const struct catalog_case cases[] = {
		{select_query, isc_info_sql_stmt_select, 0},
		{procedure_query, isc_info_sql_stmt_exec_procedure, 1}
	};
	size_t index;

	for (index = 0U; index < sizeof(cases) / sizeof(cases[0]); index++) {
		ib_connection *connection;
		ib_bindings *bindings;
		ib_cursor *cursor;
		ib_cancel_slot *slot;
		uint64_t generation;
		char *error = NULL;

		reset_mocks();
		statement_type = cases[index].statement_kind;
		describe_user_column_relation = cases[index].statement_kind ==
			isc_info_sql_stmt_select;
		describe_procedure_source = cases[index].procedure_source;
		fail_prepare_on_call = 2;
		prepare_failure_code = isc_network_error;
		connection = new_connection();
		connection->transaction = &transaction_tokens[0];
		bindings = new_integer_binding((int64_t) index + 1);
		slot = ib_cancel_slot_new(&error);
		check(bindings != NULL && slot != NULL && error == NULL,
			"ordinary catalog fallback setup failed");
		ib_error_free(error);
		if (bindings == NULL || slot == NULL) {
			ib_bindings_free(bindings);
			ib_cancel_slot_free(slot);
			free(connection);
			continue;
		}
		generation = ib_cancel_slot_begin(slot, &error);
		check(generation != 0U && error == NULL,
			"ordinary catalog fallback slot begin failed");
		ib_error_free(error);
		expected_cancel_slot = slot;
		expected_cancel_generation = generation;
		expected_cancel_statement = NULL;
		cursor = (ib_connection_query)(connection, cases[index].query,
			strlen(cases[index].query), bindings, 0, slot, generation, &error);
		check(cursor != NULL && error == NULL,
			"ordinary catalog failure did not fall back with an active slot");
		check_cancel_complete("ordinary catalog fallback left its slot active");
		ib_error_free(error);
		if (cursor != NULL) {
			error = NULL;
			check(ib_cursor_close(cursor, &error) == 0 && error == NULL,
				"ordinary catalog fallback cursor cleanup failed");
			ib_error_free(error);
		}
		expected_cancel_slot = NULL;
		expected_cancel_generation = 0U;
		expected_cancel_statement = NULL;
		ib_cancel_slot_free(slot);
		ib_bindings_free(bindings);
		free(connection);
	}

	for (index = 0U; index < sizeof(cases) / sizeof(cases[0]); index++) {
		ib_connection *connection;
		ib_bindings *bindings;
		ib_cursor *cursor;
		ib_cancel_slot *slot;
		uint64_t generation;
		char *error = NULL;

		reset_mocks();
		statement_type = cases[index].statement_kind;
		describe_user_column_relation = cases[index].statement_kind ==
			isc_info_sql_stmt_select;
		describe_procedure_source = cases[index].procedure_source;
		fail_prepare_on_call = 2;
		prepare_failure_code = isc_cancelled;
		connection = new_connection();
		connection->transaction = &transaction_tokens[0];
		bindings = new_integer_binding((int64_t) index + 11);
		slot = ib_cancel_slot_new(&error);
		check(bindings != NULL && slot != NULL && error == NULL,
			"cancelled catalog setup failed");
		ib_error_free(error);
		if (bindings == NULL || slot == NULL) {
			ib_bindings_free(bindings);
			ib_cancel_slot_free(slot);
			free(connection);
			continue;
		}
		generation = ib_cancel_slot_begin(slot, &error);
		check(generation != 0U && error == NULL,
			"cancelled catalog slot begin failed");
		ib_error_free(error);
		expected_cancel_slot = slot;
		expected_cancel_generation = generation;
		expected_cancel_statement = NULL;
		cursor = (ib_connection_query)(connection, cases[index].query,
			strlen(cases[index].query), bindings, 0, slot, generation, &error);
		check(cursor == NULL && error != NULL,
			"isc_cancelled catalog failure was swallowed");
		check(error == NULL || strstr(error, "335544794") != NULL,
			"isc_cancelled catalog failure lost its native diagnostic");
		check_cancel_complete("isc_cancelled catalog failure left its slot active");
		check(user_execute2_calls == 0,
			"isc_cancelled catalog failure reached user execution");
		ib_error_free(error);
		expected_cancel_slot = NULL;
		expected_cancel_generation = 0U;
		expected_cancel_statement = NULL;
		ib_cancel_slot_free(slot);
		ib_bindings_free(bindings);
		free(connection);
	}
}

static void test_cancel_overlap_transient_prepare(void)
{
	static const char query[] = "INSERT INTO T (ID) VALUES (?)";
	ib_connection *connection;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	slot = ib_cancel_slot_new(&error);
	check(slot != NULL && error == NULL, "transient prepare overlap slot allocation failed");
	ib_error_free(error);
	if (slot == NULL) {
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "transient prepare overlap slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_PREPARE;
	operation.connection = connection;
	operation.query = query;
	operation.query_length = sizeof(query) - 1U;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_PREPARE, 1, 0);
	run_delayed_cancel_transient(&operation, &cancel, 0,
		"transient prepare completed before delayed cancellation");
	check(operation.statement == NULL && operation.error != NULL,
		"transient prepare returned a statement after native cancellation");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);
	free(connection);
}

static void test_unpublish_failure_aborts_transient_prepare(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	ib_cursor *cursor;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	complete_during_prepare = 1;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	bindings = new_integer_binding(60);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"transient prepare unpublication setup failed");
	ib_error_free(error);
	if (bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL,
		"transient prepare unpublication slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	cursor = (ib_connection_query)(connection, query, sizeof(query) - 1U,
		bindings, 0, slot, generation, &error);
	check(cursor == NULL && error != NULL &&
		strstr(error, "cannot be unpublished") != NULL,
		"successful prepare unpublication failure was not returned");
	check(sql_info_calls == 0 && describe_calls == 0 && user_execute2_calls == 0,
		"transient prepare unpublication failure continued into describe or execute");
	check_cancel_complete("transient prepare unpublication failure left its slot active");
	ib_error_free(error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	free(connection);
}

static void test_transient_catalog_cleanup_failure_propagates(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	ib_cursor *cursor;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	describe_user_column_relation = 1;
	fail_catalog_drop = 1;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	bindings = new_integer_binding(65);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"transient catalog cleanup setup failed");
	ib_error_free(error);
	if (bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL,
		"transient catalog cleanup slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	cursor = (ib_connection_query)(connection, query, sizeof(query) - 1U,
		bindings, 0, slot, generation, &error);
	check(cursor == NULL && error != NULL && catalog_drop_calls == 1 &&
		strstr(error, "close statement") != NULL,
		"transient catalog cleanup failure was swallowed");
	check(user_execute2_calls == 0 && connection->broken,
		"transient catalog cleanup failure executed or preserved a broken query");
	check_cancel_complete("transient catalog cleanup failure left its slot active");
	ib_error_free(error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	free(connection);
}

static void test_cancel_overlap_transient_execute(void)
{
	static const char query[] = "INSERT INTO T (ID) VALUES (?)";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	bindings = new_integer_binding(61);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"transient execute overlap setup failed");
	ib_error_free(error);
	if (bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL, "transient execute overlap slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_TRANSIENT_EXECUTE;
	operation.connection = connection;
	operation.query = query;
	operation.query_length = sizeof(query) - 1U;
	operation.bindings = bindings;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_TRANSIENT_EXECUTE, 1, 0);
	run_delayed_cancel_transient(&operation, &cancel, 0,
		"transient execute completed before delayed cancellation");
	check(operation.result != 0 && operation.error != NULL,
		"transient execute did not return its native cancellation error");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	free(connection);
}

static void test_cancel_distributed_participant_execute_and_recover(void)
{
	static const char query[] = "INSERT INTO T (ID) VALUES (?)";
	static const char recovery_query[] = "INSERT INTO T (ID) VALUES (?)";
	ib_connection parent;
	ib_connection *parents[1];
	ib_distributed_transaction distributed;
	ib_transaction participant;
	ib_bindings *bindings;
	ib_bindings *recovery_bindings;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char database;
	char *error = NULL;
	uint64_t generation;
	int64_t rows_affected;

	reset_mocks();
	memset(&parent, 0, sizeof(parent));
	memset(&distributed, 0, sizeof(distributed));
	memset(&participant, 0, sizeof(participant));
	parent.database = &database;
	parent.dialect = SQL_DIALECT_V5;
	parents[0] = &parent;
	distributed.handle = &transaction_tokens[0];
	distributed.parents = parents;
	distributed.participants = &participant;
	distributed.count = 1U;
	participant.parent = &parent;
	participant.distributed = &distributed;
	participant.view = parent;
	participant.view.parent = &parent;
	participant.view.transaction = distributed.handle;
	bindings = new_integer_binding(64);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"distributed participant overlap setup failed");
	ib_error_free(error);
	if (bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL,
		"distributed participant overlap slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_DISTRIBUTED_EXECUTE;
	operation.transaction = &participant;
	operation.query = query;
	operation.query_length = sizeof(query) - 1U;
	operation.bindings = bindings;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_DISTRIBUTED_EXECUTE, 1, 0);
	run_delayed_cancel_transient(&operation, &cancel, 0,
		"distributed participant completed before delayed cancellation");
	check(operation.result != 0 && operation.error != NULL,
		"distributed participant execution did not return cancellation");
	check(parent.broken == 0 && participant.view.broken == 0,
		"distributed participant cancellation poisoned its parent attachment");
	check(persisted_execute_value == 0 && distributed.handle == &transaction_tokens[0] &&
		participant.view.transaction == distributed.handle,
		"distributed participant cancellation changed persisted state or consumed the coordinator");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	recovery_bindings = new_integer_binding(640);
	rows_affected = -1;
	error = NULL;
	check(recovery_bindings != NULL &&
		ib_transaction_exec(&participant, recovery_query, sizeof(recovery_query) - 1U,
			recovery_bindings, &rows_affected, 0, NULL, 0U, &error) == 0 &&
		error == NULL && rows_affected == 1 && execute_calls == 1 &&
		persisted_execute_value == 640 && distributed.handle == &transaction_tokens[0] &&
		participant.view.transaction == distributed.handle,
		"distributed participant did not execute the exact recovery write after cancellation");
	ib_error_free(error);
	ib_bindings_free(recovery_bindings);
	error = NULL;
	check(ib_distributed_prepare(&distributed, "round-2", sizeof("round-2") - 1U,
		&error) == 0 && error == NULL && prepare_transaction_calls == 1 &&
		distributed.prepared,
		"distributed coordinator was not usable for prepare after participant cancellation");
	ib_error_free(error);
	error = NULL;
	check(ib_distributed_commit(&distributed, &error) == 0 && error == NULL &&
		commit_calls == 1 && distributed.handle == NULL &&
		participant.view.transaction == NULL && parent.broken == 0 &&
		participant.view.broken == 0,
		"distributed coordinator did not commit the recovered exact state");
	ib_error_free(error);
	ib_bindings_free(bindings);
	ib_cancel_slot_free(slot);
}

static void test_cancel_overlap_transient_procedure_execute2(void)
{
	static const char query[] = "EXECUTE PROCEDURE P(?)";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_exec_procedure;
	procedure_output_count = 1;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	bindings = new_integer_binding(62);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"transient procedure overlap setup failed");
	ib_error_free(error);
	if (bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL,
		"transient procedure overlap slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_TRANSIENT_EXECUTE2;
	operation.connection = connection;
	operation.query = query;
	operation.query_length = sizeof(query) - 1U;
	operation.bindings = bindings;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_TRANSIENT_EXECUTE2, 1, 0);
	run_delayed_cancel_transient(&operation, &cancel, 0,
		"transient procedure execute2 completed before delayed cancellation");
	check(operation.cursor == NULL && operation.error != NULL,
		"transient procedure execute2 returned a cursor after native cancellation");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	free(connection);
}

static void test_cancel_overlap_transient_catalog_execute2(void)
{
	static const char query[] = "SELECT ID FROM T WHERE ID = ?";
	ib_connection *connection;
	ib_bindings *bindings;
	ib_cancel_slot *slot;
	struct overlap_operation_call operation;
	struct overlap_cancel_call cancel;
	char *error = NULL;
	uint64_t generation;

	reset_mocks();
	statement_type = isc_info_sql_stmt_select;
	describe_user_column_relation = 1;
	connection = new_connection();
	connection->transaction = &transaction_tokens[0];
	bindings = new_integer_binding(63);
	slot = ib_cancel_slot_new(&error);
	check(bindings != NULL && slot != NULL && error == NULL,
		"transient catalog overlap setup failed");
	ib_error_free(error);
	if (bindings == NULL || slot == NULL) {
		ib_bindings_free(bindings);
		ib_cancel_slot_free(slot);
		free(connection);
		return;
	}
	generation = ib_cancel_slot_begin(slot, &error);
	check(generation != 0U && error == NULL,
		"transient catalog overlap slot begin failed");
	ib_error_free(error);
	expected_cancel_slot = slot;
	expected_cancel_generation = generation;
	expected_cancel_statement = (isc_stmt_handle) &statement_tokens[1];
	memset(&operation, 0, sizeof(operation));
	operation.kind = OVERLAP_TRANSIENT_EXECUTE2;
	operation.connection = connection;
	operation.query = query;
	operation.query_length = sizeof(query) - 1U;
	operation.bindings = bindings;
	operation.slot = slot;
	operation.generation = generation;
	memset(&cancel, 0, sizeof(cancel));
	cancel.slot = slot;
	cancel.generation = generation;
	overlap_reset(OVERLAP_CATALOG_EXECUTE2, 1, 0);
	run_delayed_cancel_transient(&operation, &cancel, 0,
		"transient catalog execute2 completed before delayed cancellation");
	check(operation.cursor == NULL && operation.error != NULL,
		"transient catalog cancellation was swallowed");
	check(overlap_cancel_statement == catalog_statement,
		"transient catalog cancellation did not target the catalog handle");
	ib_error_free(operation.error);
	ib_error_free(cancel.error);
	expected_cancel_slot = NULL;
	expected_cancel_generation = 0U;
	expected_cancel_statement = NULL;
	overlap_reset(OVERLAP_NONE, 0, 0);
	ib_cancel_slot_free(slot);
	ib_bindings_free(bindings);
	free(connection);
}

int main(void)
{
	test_dialect_arguments_are_propagated();
	test_executable_procedure_output_and_reuse();
	test_executable_procedure_exec_contracts();
	test_executable_procedure_failure_and_abort_rollback();
	test_catalog_metadata_cleanup_drops_owned_statement();
	test_catalog_metadata_respects_connection_charset();
	test_procedure_commit_failure_rolls_back_live_handle();
	test_direct_procedure_transition_failures_free_query();
	test_procedure_transition_drop_failure_retires_connection();
	test_catalog_drop_failure_stops_user_execution();
	test_prepare_once_execute_repeated();
	test_failed_implicit_execution_keeps_handle();
	test_prepare_failure_releases_handle_and_transaction();
	test_failed_prepare_rollback_marks_connection_broken();
	test_query_cursor_does_not_drop_statement();
	test_descriptor_aliases_are_exposed_as_utf8();
	test_procedure_descriptor_without_source_remains_unknown();
	test_broken_close_aborts_pending_procedure_sibling();
	test_cursor_close_failure_aborts_later_procedure_sibling();
	test_procedure_catalog_precision_overrides_sqlda();
	test_explicit_transaction_is_not_cursor_owned();
	test_cursors_do_not_use_a_connection_global_lock();
	test_select_for_update_requires_explicit_writable_transaction();
	test_transaction_control_is_rejected_before_execution();
	test_savepoint_remains_executable();
	test_transaction_completion_closes_active_prepared_cursor();
	test_connection_close_drains_prepared_statements();
	test_cancel_overlap_execute();
	test_cancel_overlap_execute2();
	test_cancel_overlap_fetch_and_reuse();
	test_cancel_publication_and_cleanup_order();
	test_transient_catalog_fallback_and_cancelled_propagation();
	test_cancel_overlap_transient_prepare();
	test_unpublish_failure_aborts_transient_prepare();
	test_transient_catalog_cleanup_failure_propagates();
	test_cancel_overlap_transient_execute();
	test_cancel_distributed_participant_execute_and_recover();
	test_cancel_overlap_transient_procedure_execute2();
	test_cancel_overlap_transient_catalog_execute2();
	if (failures != 0) {
		return EXIT_FAILURE;
	}
	(void) puts("native prepared tests passed");
	return EXIT_SUCCESS;
}
