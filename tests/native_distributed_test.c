#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <stdint.h>

#include <ibase.h>

static unsigned int fake_start_calls;
static unsigned int fake_prepare_calls;
static unsigned int fake_commit_calls;
static unsigned int fake_rollback_calls;
static isc_tr_handle fake_active_handle;
static int fail_start_multiple;
static int start_multiple_returns_handle_on_failure;
static int fail_prepare;
static int prepare_consumes_handle_on_error;
static int fail_commit;
static int commit_consumes_handle_on_error;
static int fail_rollback;
static int rollback_consumes_handle_on_error;
static int fail_cursor_close;
static int cursor_close_calls;
static int fail_message_allocation;

static void *test_malloc(size_t size)
{
	if (fail_message_allocation) {
		return NULL;
	}
	return malloc(size);
}

static void fake_status_success(ISC_STATUS *status)
{
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
}

static void fake_status_failure(ISC_STATUS *status)
{
	status[0] = isc_arg_gds;
	status[1] = 1;
	status[2] = isc_arg_end;
}

static isc_tr_handle fake_new_handle(void)
{
	fake_active_handle = (isc_tr_handle) (uintptr_t) (0x1000U + fake_start_calls);
	return fake_active_handle;
}

ISC_STATUS ISC_EXPORT test_start_multiple(ISC_STATUS *status,
	isc_tr_handle *transaction, short count, void *vector)
{
	(void) vector;
	if (transaction == NULL || count != 2) {
		fake_status_failure(status);
		return 1;
	}
	fake_start_calls++;
	*transaction = fake_new_handle();
	if (fail_start_multiple) {
		if (!start_multiple_returns_handle_on_failure) {
			*transaction = NULL;
		}
		fake_status_failure(status);
		return 1;
	}
	fake_status_success(status);
	return 0;
}

ISC_STATUS ISC_EXPORT test_prepare_transaction2(ISC_STATUS *status,
	isc_tr_handle *transaction, short message_length, char *message)
{
	(void) message_length;
	(void) message;
	if (transaction == NULL || *transaction != fake_active_handle) {
		fake_status_failure(status);
		return 1;
	}
	fake_prepare_calls++;
	if (fail_prepare) {
		if (prepare_consumes_handle_on_error) {
			*transaction = NULL;
		}
		fake_status_failure(status);
		return 1;
	}
	fake_status_success(status);
	return 0;
}

ISC_STATUS ISC_EXPORT test_commit_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	if (transaction == NULL || *transaction != fake_active_handle) {
		fake_status_failure(status);
		return 1;
	}
	fake_commit_calls++;
	if (fail_commit) {
		if (commit_consumes_handle_on_error) {
			*transaction = NULL;
		}
		fake_status_failure(status);
		return 1;
	}
	*transaction = NULL;
	fake_status_success(status);
	return 0;
}

ISC_STATUS ISC_EXPORT test_rollback_transaction(ISC_STATUS *status,
	isc_tr_handle *transaction)
{
	if (transaction == NULL || *transaction != fake_active_handle) {
		fake_status_failure(status);
		return 1;
	}
	fake_rollback_calls++;
	if (fail_rollback) {
		if (rollback_consumes_handle_on_error) {
			*transaction = NULL;
		}
		fake_status_failure(status);
		return 1;
	}
	*transaction = NULL;
	fake_status_success(status);
	return 0;
}

ISC_STATUS ISC_EXPORT test_transaction_info(ISC_STATUS *status,
	isc_tr_handle *transaction, short item_length, char *items,
	short buffer_length, char *buffer)
{
	(void) transaction;
	(void) item_length;
	(void) items;
	if (buffer_length < 15) {
		status[0] = isc_arg_gds;
		status[1] = 1;
		status[2] = isc_arg_end;
		return 1;
	}
	memset(buffer, 0, (size_t) buffer_length);
	buffer[0] = (char) isc_info_tra_id;
	buffer[1] = 4;
	buffer[2] = 0;
	buffer[3] = 1;
	buffer[7] = (char) isc_info_tra_id;
	buffer[8] = 4;
	buffer[9] = 0;
	buffer[10] = 2;
	buffer[14] = (char) isc_info_end;
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

ISC_STATUS ISC_EXPORT test_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option)
{
	(void) option;
	cursor_close_calls++;
	if (!fail_cursor_close) {
		*statement = NULL;
		fake_status_success(status);
		return 0;
	}
	fake_status_failure(status);
	return 1;
}

#define malloc test_malloc
#define isc_dsql_free_statement test_free_statement
#define isc_transaction_info test_transaction_info
#define isc_start_multiple test_start_multiple
#define isc_prepare_transaction2 test_prepare_transaction2
#define isc_commit_transaction test_commit_transaction
#define isc_rollback_transaction test_rollback_transaction
#include "../native.c"
#undef malloc
#undef isc_dsql_free_statement
#undef isc_transaction_info
#undef isc_start_multiple
#undef isc_prepare_transaction2
#undef isc_commit_transaction
#undef isc_rollback_transaction

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native distributed test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void test_participant_error_does_not_break_parent_attachment(void)
{
	ib_connection parent;
	ib_distributed_transaction distributed;
	ib_transaction participant;

	memset(&parent, 0, sizeof(parent));
	memset(&distributed, 0, sizeof(distributed));
	memset(&participant, 0, sizeof(participant));
	parent.broken = 0;
	participant.parent = &parent;
	participant.distributed = &distributed;
	participant.view.broken = 1;

	ib_transaction_sync(&participant);
	check(parent.broken == 0,
		"participant operation error poisoned the parent attachment");
}

static void test_distributed_error_breaks_all_participant_views(void)
{
	ib_connection first;
	ib_connection second;
	ib_connection *parents[2];
	ib_transaction participants[2];
	ib_distributed_transaction distributed;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	memset(participants, 0, sizeof(participants));
	memset(&distributed, 0, sizeof(distributed));
	parents[0] = &first;
	parents[1] = &second;
	distributed.parents = parents;
	distributed.participants = participants;
	distributed.count = 2U;

	ib_distributed_mark_broken(&distributed);
	check(first.broken != 0 && second.broken != 0,
		"distributed error did not break every parent attachment");
	check(participants[0].view.broken != 0 && participants[1].view.broken != 0,
		"distributed error did not break every participant view");
}

static void test_transaction_info_preserves_every_participant_record(void)
{
	ib_connection connection;
	char database_token;
	char transaction_token;
	char *response = NULL;
	size_t response_length = 0U;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.transaction = &transaction_token;
	check(ib_connection_transaction_info(&connection, (unsigned char) isc_info_tra_id,
		&response, &response_length, &error) == 0,
		"repeated transaction info records were rejected");
	check(response_length == 15U && (unsigned char) response[0] == isc_info_tra_id &&
		(unsigned char) response[3] == 1U && (unsigned char) response[7] == isc_info_tra_id &&
		(unsigned char) response[10] == 2U && (unsigned char) response[14] == isc_info_end,
		"transaction info did not preserve every participant record");
	ib_error_free(error);
	ib_error_free(response);
}

static void test_distributed_native_group_lifecycle(void)
{
	ib_connection first;
	ib_connection second;
	ib_connection *connections[2];
	const char *tpbs[2] = {"read", "write"};
	const size_t tpb_lengths[2] = {4U, 5U};
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	ib_transaction *first_participant;
	ib_transaction *second_participant;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	connections[0] = &first;
	connections[1] = &second;
	fake_start_calls = 0U;
	fake_prepare_calls = 0U;
	fake_commit_calls = 0U;
	fake_rollback_calls = 0U;
	fake_active_handle = NULL;

	distributed = ib_distributed_begin(connections, tpbs, tpb_lengths, 2U, &error);
	check(distributed != NULL && error == NULL,
		"native distributed begin did not use the fake start_multiple handle");
	check(fake_start_calls == 1U && distributed->handle == fake_active_handle,
		"native distributed begin did not retain the fake transaction identity");
	first_participant = ib_distributed_participant(distributed, 0U);
	second_participant = ib_distributed_participant(distributed, 1U);
	check(first_participant != NULL && second_participant != NULL &&
		first_participant->view.transaction == fake_active_handle &&
		second_participant->view.transaction == fake_active_handle,
		"distributed participants did not share the fake transaction identity");
	check(ib_distributed_prepare(distributed, "recovery", 8U, &error) == 0 &&
		error == NULL && fake_prepare_calls == 1U,
		"native distributed prepare did not reach the fake prepare_transaction2");
	check(ib_distributed_commit(distributed, &error) == 0 && error == NULL &&
		fake_commit_calls == 1U && distributed->handle == NULL,
		"native distributed commit did not consume the fake transaction handle");
	ib_distributed_free(distributed);

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	fake_active_handle = NULL;
	error = NULL;
	distributed = ib_distributed_begin(connections, tpbs, tpb_lengths, 2U, &error);
	check(distributed != NULL && error == NULL,
		"native distributed rollback setup failed");
	check(ib_distributed_rollback(distributed, &error) == 0 && error == NULL &&
		fake_rollback_calls == 1U && distributed->handle == NULL,
		"native distributed rollback did not consume the fake transaction handle");
	ib_distributed_free(distributed);
}

static void reset_failure_flags(void)
{
	fail_start_multiple = 0;
	start_multiple_returns_handle_on_failure = 0;
	fail_prepare = 0;
	prepare_consumes_handle_on_error = 0;
	fail_commit = 0;
	commit_consumes_handle_on_error = 0;
	fail_rollback = 0;
	rollback_consumes_handle_on_error = 0;
	fail_cursor_close = 0;
	cursor_close_calls = 0;
	fail_message_allocation = 0;
}

static ib_distributed_transaction *new_distributed_for_failure_test(
	ib_connection *first, ib_connection *second, char **error)
{
	ib_connection *connections[2] = {first, second};
	const char *tpbs[2] = {"read", "write"};
	const size_t tpb_lengths[2] = {4U, 5U};

	fake_active_handle = NULL;
	return ib_distributed_begin(connections, tpbs, tpb_lengths, 2U, error);
}

static void add_transaction_cursor(ib_transaction *participant)
{
	static char statement_token;
	ib_cursor *cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));

	check(cursor != NULL, "distributed cursor allocation failed");
	cursor->connection = &participant->view;
	cursor->statement = &statement_token;
	cursor->transaction = participant->view.transaction;
	cursor->owns_transaction = 0;
	ib_cursor_register(cursor);
}

static void test_distributed_rollback_merges_cursor_and_rollback_errors(void)
{
	ib_connection first;
	ib_connection second;
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL,
		"rollback error merge setup failed");
	add_transaction_cursor(ib_distributed_participant(distributed, 0U));
	fail_cursor_close = 1;
	fail_rollback = 1;
	check(ib_distributed_rollback(distributed, &error) != 0 && error != NULL,
		"cursor and rollback failures did not fail the coordinator");
	check(cursor_close_calls == 1 &&
		strstr(error, "close statement") != NULL &&
		strstr(error, "rollback distributed transaction") != NULL,
		"cursor and rollback diagnostics were not merged");
	check(ib_distributed_handle_state(distributed) == IB_HANDLE_LIVE,
		"failed rollback unexpectedly consumed its live handle");
	ib_error_free(error);
	error = NULL;
	reset_failure_flags();
	check(ib_distributed_rollback(distributed, &error) == 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"merged rollback failure could not be resolved on retry");
	ib_distributed_free(distributed);
}

static void test_distributed_rollback_cleanup_failure_survives_diagnostic_oom(void)
{
	ib_connection first;
	ib_connection second;
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL,
		"cleanup OOM rollback setup failed");
	add_transaction_cursor(ib_distributed_participant(distributed, 0U));
	fail_cursor_close = 1;
	fail_message_allocation = 1;
	check(ib_distributed_rollback(distributed, &error) != 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"cursor cleanup failure was hidden by diagnostic allocation failure");
	ib_distributed_free(distributed);
}

static void test_distributed_rollback_consumed_handle_failure_survives_diagnostic_oom(void)
{
	ib_connection first;
	ib_connection second;
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL,
		"consumed rollback OOM setup failed");
	fail_rollback = 1;
	rollback_consumes_handle_on_error = 1;
	fail_message_allocation = 1;
	check(ib_distributed_rollback(distributed, &error) != 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"consumed rollback failure was reported as success after diagnostic OOM");
	ib_distributed_free(distributed);
}

static void test_distributed_failures_preserve_live_handle(void)
{
	ib_connection first;
	ib_connection second;
	ib_connection *connections[2] = {&first, &second};
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	ib_transaction *first_participant;
	ib_transaction *second_participant;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL, "failure test setup failed");
	first_participant = ib_distributed_participant(distributed, 0U);
	second_participant = ib_distributed_participant(distributed, 1U);
	check(first_participant != NULL && second_participant != NULL,
		"failure test participants are unavailable");

	fail_prepare = 1;
	check(ib_distributed_prepare(distributed, "recovery", 8U, &error) != 0 &&
		error != NULL && ib_distributed_handle_state(distributed) == IB_HANDLE_LIVE &&
		first_participant->view.transaction == fake_active_handle &&
		second_participant->view.transaction == fake_active_handle,
		"prepare failure did not preserve the distributed native handle");
	ib_error_free(error);
	error = NULL;
	fail_prepare = 0;
	check(ib_distributed_rollback(distributed, &error) == 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED &&
		first_participant->view.transaction == NULL &&
		second_participant->view.transaction == NULL,
		"prepare failure could not be resolved by rollback retry");
	ib_error_free(error);
	ib_distributed_free(distributed);

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL, "commit failure setup failed");
	fail_prepare = 0;
	check(ib_distributed_prepare(distributed, NULL, 0U, &error) == 0 && error == NULL,
		"commit failure setup did not prepare");
	fail_commit = 1;
	check(ib_distributed_commit(distributed, &error) != 0 && error != NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_LIVE,
		"commit failure did not preserve the distributed native handle");
	ib_error_free(error);
	error = NULL;
	fail_commit = 0;
	check(ib_distributed_rollback(distributed, &error) == 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"commit failure could not be resolved by rollback retry");
	ib_error_free(error);
	ib_distributed_free(distributed);

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL, "rollback failure setup failed");
	fail_rollback = 1;
	check(ib_distributed_rollback(distributed, &error) != 0 && error != NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_LIVE,
		"rollback failure did not preserve the distributed native handle");
	ib_error_free(error);
	error = NULL;
	fail_rollback = 0;
	check(ib_distributed_rollback(distributed, &error) == 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"rollback failure could not be resolved by rollback retry");
	ib_error_free(error);
	ib_distributed_free(distributed);
	(void) connections;
}

static void test_distributed_failures_report_consumed_handle(void)
{
	ib_connection first;
	ib_connection second;
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL, "consumed prepare setup failed");
	fail_prepare = 1;
	prepare_consumes_handle_on_error = 1;
	check(ib_distributed_prepare(distributed, NULL, 0U, &error) != 0 && error != NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"prepare error did not report a consumed handle");
	ib_error_free(error);
	error = NULL;
	ib_distributed_free(distributed);

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL, "consumed commit setup failed");
	check(ib_distributed_prepare(distributed, NULL, 0U, &error) == 0 && error == NULL,
		"consumed commit setup did not prepare");
	fail_commit = 1;
	commit_consumes_handle_on_error = 1;
	check(ib_distributed_commit(distributed, &error) != 0 && error != NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"commit error did not report a consumed handle");
	ib_error_free(error);
	error = NULL;
	ib_distributed_free(distributed);

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error == NULL, "consumed rollback setup failed");
	fail_rollback = 1;
	rollback_consumes_handle_on_error = 1;
	check(ib_distributed_rollback(distributed, &error) != 0 && error != NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"rollback error did not report a consumed handle");
	ib_error_free(error);
	ib_distributed_free(distributed);
}

static void test_distributed_start_failure_retains_handle(void)
{
	ib_connection first;
	ib_connection second;
	char first_database;
	char second_database;
	ib_distributed_transaction *distributed;
	char *error = NULL;

	memset(&first, 0, sizeof(first));
	memset(&second, 0, sizeof(second));
	first.database = &first_database;
	second.database = &second_database;
	reset_failure_flags();
	fail_start_multiple = 1;
	start_multiple_returns_handle_on_failure = 1;
	fail_rollback = 1;
	distributed = new_distributed_for_failure_test(&first, &second, &error);
	check(distributed != NULL && error != NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_LIVE &&
		ib_distributed_participant(distributed, 0U)->view.transaction == fake_active_handle &&
		ib_distributed_participant(distributed, 1U)->view.transaction == fake_active_handle,
		"distributed start failure lost the live handle");
	ib_error_free(error);
	error = NULL;
	fail_start_multiple = 0;
	fail_rollback = 0;
	check(ib_distributed_rollback(distributed, &error) == 0 && error == NULL &&
		ib_distributed_handle_state(distributed) == IB_HANDLE_CONSUMED,
		"distributed start failure could not be cleaned up on retry");
	ib_error_free(error);
	ib_distributed_free(distributed);
}

int main(void)
{
	test_participant_error_does_not_break_parent_attachment();
	test_distributed_error_breaks_all_participant_views();
	test_transaction_info_preserves_every_participant_record();
	test_distributed_native_group_lifecycle();
	test_distributed_failures_preserve_live_handle();
	test_distributed_failures_report_consumed_handle();
	test_distributed_start_failure_retains_handle();
	test_distributed_rollback_merges_cursor_and_rollback_errors();
	test_distributed_rollback_cleanup_failure_survives_diagnostic_oom();
	test_distributed_rollback_consumed_handle_failure_survives_diagnostic_oom();
	(void) puts("native distributed tests passed");
	return EXIT_SUCCESS;
}
