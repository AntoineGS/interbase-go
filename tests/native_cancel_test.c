#include <assert.h>
#include <stdint.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <ibase.h>

static pthread_mutex_t test_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t test_condition = PTHREAD_COND_INITIALIZER;
static int cancel_entered;
static int cancel_release;
static int cancel_calls;
static unsigned short cancel_option;
static ISC_STATUS *cancel_status_storage;
static isc_stmt_handle cancel_statement;
static ISC_STATUS cancel_result;
static int execute_entered;
static int execute_release;
static int completion_started;
static int drop_calls;
static ISC_STATUS execute_status_storage[20];
static int fail_next_malloc;

static void *test_malloc(size_t size)
{
	if (fail_next_malloc) {
		fail_next_malloc = 0;
		return NULL;
	}
	return malloc(size);
}

static ISC_STATUS test_isc_dsql_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option)
{
	int wait_result;

	(void) pthread_mutex_lock(&test_mutex);
	cancel_calls++;
	cancel_option = option;
	cancel_status_storage = status;
	cancel_statement = statement == NULL ? NULL : *statement;
	cancel_entered = 1;
	(void) pthread_cond_broadcast(&test_condition);
	while (!cancel_release) {
		wait_result = pthread_cond_wait(&test_condition, &test_mutex);
		if (wait_result != 0) {
			(void) pthread_mutex_unlock(&test_mutex);
			return (ISC_STATUS) wait_result;
		}
	}
	status[0] = isc_arg_gds;
	status[1] = cancel_result;
	status[2] = isc_arg_end;
	(void) pthread_mutex_unlock(&test_mutex);
	return cancel_result;
}

#define isc_dsql_free_statement test_isc_dsql_free_statement
#define malloc test_malloc
#include "../native.c"
#undef malloc
#undef isc_dsql_free_statement

static void require_condition(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native cancellation test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void require_pthread(int result, const char *operation)
{
	if (result != 0) {
		(void) fprintf(stderr, "native cancellation test failed: %s returned %d\n",
			operation, result);
		exit(EXIT_FAILURE);
	}
}

static void reset_cancel_stub(ISC_STATUS result)
{
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	cancel_entered = 0;
	cancel_release = 0;
	cancel_calls = 0;
	cancel_option = 0;
	cancel_status_storage = NULL;
	cancel_statement = NULL;
	cancel_result = result;
	execute_entered = 0;
	execute_release = 0;
	completion_started = 0;
	drop_calls = 0;
	(void) memset(execute_status_storage, 0, sizeof(execute_status_storage));
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
}

static void wait_for_cancel_entry(void)
{
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	while (!cancel_entered) {
		require_pthread(pthread_cond_wait(&test_condition, &test_mutex),
			"pthread_cond_wait");
	}
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
}

static void release_cancel_stub(void)
{
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	cancel_release = 1;
	require_pthread(pthread_cond_broadcast(&test_condition),
		"pthread_cond_broadcast");
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
}

static void wait_for_execute_entry(void)
{
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	while (!execute_entered) {
		require_pthread(pthread_cond_wait(&test_condition, &test_mutex),
			"pthread_cond_wait");
	}
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
}

static void release_execute_call(void)
{
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	execute_release = 1;
	require_pthread(pthread_cond_broadcast(&test_condition),
		"pthread_cond_broadcast");
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
}

struct cancel_call {
	ib_cancel_slot *slot;
	uint64_t generation;
	int request_status;
	int64_t native_code;
	char *error;
};

static void *cancel_worker(void *argument)
{
	struct cancel_call *call = (struct cancel_call *) argument;

	call->request_status = ib_cancel_slot_cancel(call->slot, call->generation,
		&call->native_code, &call->error);
	return NULL;
}

struct completion_call {
	ib_cancel_slot *slot;
	uint64_t generation;
};

static void *execute_worker(void *argument)
{
	struct completion_call *call = (struct completion_call *) argument;

	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	execute_entered = 1;
	require_pthread(pthread_cond_broadcast(&test_condition),
		"pthread_cond_broadcast");
	while (!execute_release) {
		require_pthread(pthread_cond_wait(&test_condition, &test_mutex),
			"pthread_cond_wait");
	}
	completion_started = 1;
	require_pthread(pthread_cond_broadcast(&test_condition),
		"pthread_cond_broadcast");
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");

	ib_cancel_slot_complete(call->slot, call->generation);

	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	drop_calls++;
	require_pthread(pthread_cond_broadcast(&test_condition),
		"pthread_cond_broadcast");
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
	return NULL;
}

static void wait_for_completion_start(void)
{
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	while (!completion_started) {
		require_pthread(pthread_cond_wait(&test_condition, &test_mutex),
			"pthread_cond_wait");
	}
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");
}

static ib_cancel_slot *new_slot(void)
{
	ib_cancel_slot *slot;
	char *error = NULL;

	slot = ib_cancel_slot_new(&error);
	require_condition(slot != NULL, "slot allocation failed");
	require_condition(error == NULL, "slot allocation returned an error");
	return slot;
}

static uint64_t begin_slot(ib_cancel_slot *slot)
{
	char *error = NULL;
	uint64_t generation;

	generation = ib_cancel_slot_begin(slot, &error);
	require_condition(generation != 0U, "slot begin did not return a generation");
	require_condition(error == NULL, "slot begin returned an error");
	return generation;
}

static void publish_slot(ib_cancel_slot *slot, uint64_t generation,
	isc_stmt_handle *statement)
{
	char *error = NULL;

	require_condition(ib_cancel_slot_publish(slot, generation, statement, &error) == 0,
		"slot publication failed");
	require_condition(error == NULL, "slot publication returned an error");
}

static void complete_slot(ib_cancel_slot *slot, uint64_t generation)
{
	ib_cancel_slot_complete(slot, generation);
}

static void test_active_cancel_joins_completion(void)
{
	char statement_token;
	isc_stmt_handle statement = &statement_token;
	ib_cancel_slot *slot;
	uint64_t generation;
	struct cancel_call cancel_call_data;
	struct completion_call completion_call_data;
	pthread_t cancel_thread;
	pthread_t completion_thread;

	reset_cancel_stub(0);
	slot = new_slot();
	generation = begin_slot(slot);
	publish_slot(slot, generation, &statement);

	cancel_call_data.slot = slot;
	cancel_call_data.generation = generation;
	cancel_call_data.request_status = -2;
	cancel_call_data.native_code = -1;
	cancel_call_data.error = NULL;
	completion_call_data.slot = slot;
	completion_call_data.generation = generation;
	require_pthread(pthread_create(&completion_thread, NULL, execute_worker,
		&completion_call_data), "pthread_create");
	wait_for_execute_entry();

	require_pthread(pthread_create(&cancel_thread, NULL, cancel_worker,
		&cancel_call_data), "pthread_create");
	wait_for_cancel_entry();
	release_execute_call();
	wait_for_completion_start();
	require_pthread(pthread_mutex_lock(&test_mutex), "pthread_mutex_lock");
	require_condition(drop_calls == 0,
		"completion reached simulated drop before cancel returned");
	require_pthread(pthread_mutex_unlock(&test_mutex), "pthread_mutex_unlock");

	release_cancel_stub();
	require_pthread(pthread_join(cancel_thread, NULL), "pthread_join");
	require_pthread(pthread_join(completion_thread, NULL), "pthread_join");

	require_condition(cancel_call_data.request_status == 0,
		"active cancellation request failed");
	require_condition(cancel_call_data.error == NULL,
		"active cancellation returned an unexpected error");
	require_condition(cancel_option == DSQL_cancel,
		"active cancellation used the wrong option");
	require_condition(cancel_status_storage != execute_status_storage,
		"cancellation reused the executing status storage");
	require_condition(cancel_call_data.native_code == 0,
		"active cancellation returned the wrong native status");
	require_condition(cancel_calls == 1, "active cancellation called native cancel twice");
	require_condition(cancel_statement == statement,
		"active cancellation used the wrong statement handle");
	require_condition(drop_calls == 1, "completion did not reach simulated drop");

	ib_cancel_slot_free(slot);
}

static void test_cancel_before_publication_is_noop_after_completion(void)
{
	ib_cancel_slot *slot;
	uint64_t generation;
	struct cancel_call cancel_call_data;
	pthread_t cancel_thread;

	reset_cancel_stub(0);
	slot = new_slot();
	generation = begin_slot(slot);
	cancel_call_data.slot = slot;
	cancel_call_data.generation = generation;
	cancel_call_data.request_status = -2;
	cancel_call_data.native_code = -1;
	cancel_call_data.error = NULL;
	require_pthread(pthread_create(&cancel_thread, NULL, cancel_worker,
		&cancel_call_data), "pthread_create");

	complete_slot(slot, generation);
	require_pthread(pthread_join(cancel_thread, NULL), "pthread_join");
	require_condition(cancel_call_data.request_status == 0,
		"pre-publication cancellation request failed");
	require_condition(cancel_call_data.native_code == 0,
		"pre-publication cancellation returned a native status");
	require_condition(cancel_calls == 0,
		"pre-publication cancellation called the native client");
	ib_cancel_slot_free(slot);
}

static void test_completion_before_cancel_is_noop(void)
{
	char statement_token;
	isc_stmt_handle statement = &statement_token;
	ib_cancel_slot *slot;
	uint64_t generation;
	char *error = NULL;
	int64_t native_code = -1;

	reset_cancel_stub(0);
	slot = new_slot();
	generation = begin_slot(slot);
	publish_slot(slot, generation, &statement);
	complete_slot(slot, generation);
	require_condition(ib_cancel_slot_cancel(slot, generation, &native_code, &error) == 0,
		"post-completion cancellation request failed");
	require_condition(error == NULL, "post-completion cancellation returned an error");
	require_condition(native_code == 0,
		"post-completion cancellation returned a native status");
	require_condition(cancel_calls == 0,
		"post-completion cancellation called the native client");
	ib_cancel_slot_free(slot);
}

static void test_idle_cancel_is_noop(void)
{
	ib_cancel_slot *slot;
	char *error = NULL;
	int64_t native_code = -1;

	reset_cancel_stub(0);
	slot = new_slot();
	require_condition(ib_cancel_slot_cancel(slot, 1U, &native_code, &error) == 0,
		"idle cancellation request failed");
	require_condition(error == NULL, "idle cancellation returned an error");
	require_condition(native_code == 0, "idle cancellation returned a native status");
	require_condition(cancel_calls == 0, "idle cancellation called the native client");
	ib_cancel_slot_free(slot);
}

static void test_stale_generation_cannot_cancel_next_operation(void)
{
	char first_statement_token;
	char second_statement_token;
	isc_stmt_handle first_statement = &first_statement_token;
	isc_stmt_handle second_statement = &second_statement_token;
	ib_cancel_slot *slot;
	uint64_t first_generation;
	uint64_t second_generation;
	char *error = NULL;
	int64_t native_code = -1;

	reset_cancel_stub(0);
	slot = new_slot();
	first_generation = begin_slot(slot);
	publish_slot(slot, first_generation, &first_statement);
	complete_slot(slot, first_generation);
	second_generation = begin_slot(slot);
	require_condition(second_generation != first_generation,
		"slot generation was reused");
	publish_slot(slot, second_generation, &second_statement);
	require_condition(ib_cancel_slot_cancel(slot, first_generation, &native_code, &error) == 0,
		"stale cancellation request failed");
	require_condition(error == NULL, "stale cancellation returned an error");
	require_condition(native_code == 0, "stale cancellation returned a native status");
	require_condition(cancel_calls == 0,
		"stale cancellation affected the next generation");
	complete_slot(slot, second_generation);
	ib_cancel_slot_free(slot);
}

static void test_cancel_failure_is_reported_separately(void)
{
	char statement_token;
	isc_stmt_handle statement = &statement_token;
	ib_cancel_slot *slot;
	uint64_t generation;
	struct cancel_call cancel_call_data;
	pthread_t cancel_thread;

	reset_cancel_stub((ISC_STATUS) 37);
	slot = new_slot();
	generation = begin_slot(slot);
	publish_slot(slot, generation, &statement);
	cancel_call_data.slot = slot;
	cancel_call_data.generation = generation;
	cancel_call_data.request_status = -2;
	cancel_call_data.native_code = -1;
	cancel_call_data.error = NULL;
	require_pthread(pthread_create(&cancel_thread, NULL, cancel_worker,
		&cancel_call_data), "pthread_create");
	wait_for_cancel_entry();
	release_cancel_stub();
	require_pthread(pthread_join(cancel_thread, NULL), "pthread_join");
	require_condition(cancel_call_data.request_status == 0,
		"failed cancellation request returned a request error");
	require_condition(cancel_call_data.error == NULL,
		"failed cancellation returned a request error message");
	require_condition(cancel_call_data.native_code == 37,
		"failed cancellation did not preserve native status");
	require_condition(cancel_calls == 1, "failed cancellation was not attempted once");
	complete_slot(slot, generation);
	ib_cancel_slot_free(slot);
}

static void test_allocation_failure_is_reported(void)
{
	ib_cancel_slot *slot;
	char *error = NULL;

	reset_cancel_stub(0);
	fail_next_malloc = 1;
	slot = ib_cancel_slot_new(&error);
	require_condition(slot == NULL, "allocation failure unexpectedly created a slot");
	require_condition(error != NULL, "allocation failure returned no error");
	ib_error_free(error);
}

static void test_invalid_arguments_are_rejected(void)
{
	char *error = NULL;
	int64_t native_code = -1;

	require_condition(ib_cancel_slot_begin(NULL, &error) == 0,
		"null slot begin unexpectedly succeeded");
	require_condition(error != NULL, "null slot begin returned no error");
	ib_error_free(error);
	error = NULL;
	require_condition(ib_cancel_slot_cancel(NULL, 1U, &native_code, &error) == -1,
		"null slot cancel unexpectedly succeeded");
	require_condition(error != NULL, "null slot cancel returned no error");
	ib_error_free(error);
}

int main(void)
{
	test_active_cancel_joins_completion();
	test_cancel_before_publication_is_noop_after_completion();
	test_completion_before_cancel_is_noop();
	test_idle_cancel_is_noop();
	test_stale_generation_cannot_cancel_next_operation();
	test_cancel_failure_is_reported_separately();
	test_allocation_failure_is_reported();
	test_invalid_arguments_are_rejected();
	(void) puts("native cancellation tests passed");
	return EXIT_SUCCESS;
}
