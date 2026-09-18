#include "../events/native.h"

#include <ibase.h>
#include <stdint.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int failures;

/*
 * The production event implementation calls these hooks when it crosses the
 * shared native gate.  The standalone C test has no Go gate, so provide the
 * same reader/writer semantics here rather than weakening the production
 * code behind a test-only lock.  In particular, readers remain admissible
 * while a writer is waiting, but not while it owns the gate.
 */
static pthread_mutex_t bridge_mutex = PTHREAD_MUTEX_INITIALIZER;
static pthread_cond_t bridge_condition = PTHREAD_COND_INITIALIZER;
static unsigned int bridge_active_readers;
static unsigned int bridge_exclusive_waiters;
static int bridge_exclusive;
static uintptr_t bridge_next_token = 1U;
static unsigned int bridge_ordinary_entries;
static unsigned int bridge_exclusive_entries;

uintptr_t ib_event_gate_enter(void)
{

	uintptr_t token;
	(void) pthread_mutex_lock(&bridge_mutex);
	while (bridge_exclusive) {
		(void) pthread_cond_wait(&bridge_condition, &bridge_mutex);
	}
	bridge_active_readers++;
	bridge_ordinary_entries++;
	token = bridge_next_token++;
	if (token == 0U) {
		token = bridge_next_token++;
	}
	(void) pthread_mutex_unlock(&bridge_mutex);
	return token;
}

void ib_event_gate_leave(uintptr_t token)
{

	if (token == 0U) {
		return;
	}
	(void) pthread_mutex_lock(&bridge_mutex);
	if (bridge_active_readers != 0U) {
		bridge_active_readers--;
	}
	if (bridge_active_readers == 0U) {
		(void) pthread_cond_broadcast(&bridge_condition);
	}
	(void) pthread_mutex_unlock(&bridge_mutex);
}

uintptr_t ib_event_gate_enter_exclusive(void)
{

	uintptr_t token;
	(void) pthread_mutex_lock(&bridge_mutex);
	bridge_exclusive_waiters++;
	while (bridge_active_readers != 0U || bridge_exclusive) {
		(void) pthread_cond_wait(&bridge_condition, &bridge_mutex);
	}
	bridge_exclusive_waiters--;
	bridge_exclusive = 1;
	bridge_exclusive_entries++;
	token = bridge_next_token++;
	if (token == 0U) {
		token = bridge_next_token++;
	}
	(void) pthread_mutex_unlock(&bridge_mutex);
	return token;
}

void ib_event_gate_leave_exclusive(uintptr_t token)
{

	if (token == 0U) {
		return;
	}
	(void) pthread_mutex_lock(&bridge_mutex);
	bridge_exclusive = 0;
	(void) pthread_cond_broadcast(&bridge_condition);
	(void) pthread_mutex_unlock(&bridge_mutex);
}

static void check(int condition, const char *message)
{
	if (!condition) {
		fprintf(stderr, "native events test failed: %s\n", message);
		failures++;
	}
}

static void check_success(int result, char **error, const char *operation)
{
	if (result == 0 && (error == NULL || *error == NULL)) {
		return;
	}
	fprintf(stderr, "native events test failed: %s: %s\n", operation,
		(error != NULL && *error != NULL) ? *error : "unknown error");
	if (error != NULL && *error != NULL) {
		ib_event_error_free(*error);
		*error = NULL;
	}
	failures++;
}

static void check_failure(int result, char **error, const char *operation,
	const char *message)
{
	if (result != 0 && error != NULL && *error != NULL) {
		ib_event_error_free(*error);
		*error = NULL;
		return;
	}
	fprintf(stderr, "native events test failed: %s: %s\n", operation, message);
	if (error != NULL && *error != NULL) {
		ib_event_error_free(*error);
		*error = NULL;
	}
	failures++;
}

static ib_event_subscription *new_single_test_subscription(void)
{
	const char *name = "native_event_lifecycle";
	char *error = NULL;
	ib_event_subscription *subscription;

	subscription = ib_event_test_new(&name, 1U, &error);
	if (subscription == NULL || error != NULL) {
		fprintf(stderr, "native events test failed: create lifecycle subscription: %s\n",
			error != NULL ? error : "unknown error");
		if (error != NULL) {
			ib_event_error_free(error);
		}
		failures++;
		return NULL;
	}
	return subscription;
}

static void test_initial_baseline_and_wrap(void)
{
	const char *name = "native_event_baseline";
	const uint64_t initial[1] = {7U};
	const uint64_t next[1] = {8U};
	const uint64_t wrap_initial[1] = {UINT32_MAX - 1U};
	const uint64_t wrap_next[1] = {1U};
	uint64_t counts[1] = {0U};
	int has_counts = 0;
	char *error = NULL;
	ib_event_subscription *subscription;

	subscription = ib_event_test_new(&name, 1U, &error);
	check(subscription != NULL && error == NULL,
		"create baseline test subscription");
	if (subscription == NULL) {
		if (error != NULL) {
			ib_event_error_free(error);
		}
		return;
	}
	check_success(ib_event_test_emit_counts(subscription, 0U, initial, 1U,
		&error), &error, "emit nonzero initial baseline");
	check_success(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "take initial baseline");
	check(!has_counts && counts[0] == 0U, "initial baseline was reported");
	check_success(ib_event_test_emit_counts(subscription, 0U, next, 1U, &error),
		&error, "emit event after initial baseline");
	check_success(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "take event after initial baseline");
	check(has_counts && counts[0] == 1U,
		"initial baseline was not installed before the first delta");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy baseline test subscription");

	subscription = ib_event_test_new(&name, 1U, &error);
	check(subscription != NULL && error == NULL,
		"create wrap test subscription");
	if (subscription == NULL) {
		if (error != NULL) {
			ib_event_error_free(error);
		}
		return;
	}
	check_success(ib_event_test_emit_counts(subscription, 0U, wrap_initial, 1U,
		&error), &error, "emit wrapping baseline");
	check_success(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "take wrapping baseline");
	check(!has_counts, "wrapping baseline was reported");
	check_success(ib_event_test_emit_counts(subscription, 0U, wrap_next, 1U,
		&error), &error, "emit wrapped event");
	check_success(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "take wrapped event");
	check(has_counts && counts[0] == 3U,
		"native event counter did not wrap at uint32 width");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy wrap test subscription");
}

static void test_native_name_limit(void)
{
	char accepted[128];
	char rejected[129];
	const char *accepted_names[] = {accepted};
	const char *rejected_names[] = {rejected};
	char *error = NULL;
	ib_event_subscription *subscription;

	memset(accepted, 'a', sizeof(accepted) - 1U);
	accepted[sizeof(accepted) - 1U] = '\0';
	subscription = ib_event_test_new(accepted_names, 1U, &error);
	check(subscription != NULL && error == NULL,
		"native accepted event name boundary");
	if (error != NULL) {
		ib_event_error_free(error);
		error = NULL;
	}
	if (subscription != NULL) {
		check_success(ib_event_destroy(subscription, &error), &error,
			"destroy native accepted event name boundary");
	}

	memset(rejected, 'r', sizeof(rejected) - 1U);
	rejected[sizeof(rejected) - 1U] = '\0';
	subscription = ib_event_test_new(rejected_names, 1U, &error);
	check(subscription == NULL && error != NULL,
		"native rejected event name boundary");
	if (error != NULL) {
		ib_event_error_free(error);
	}
}

static void test_connect_timeout_is_encoded_as_little_endian_integer(void)
{
	const unsigned char expected[] = {0x04, 0x03, 0x02, 0x01};
	unsigned char dpb[256];
	size_t dpb_length;
	size_t offset;
	int timeout_seen;
	char *error = NULL;

	dpb_length = 0U;
	check_success(ib_event_test_build_dpb(0x01020304U, dpb, sizeof(dpb),
		&dpb_length, &error), &error, "encode event connect timeout");
	timeout_seen = 0;
	offset = 1U;
	while (offset < dpb_length) {
		unsigned char tag;
		unsigned char length;

		check(dpb_length - offset >= 2U, "event timeout DPB item header is truncated");
		if (dpb_length - offset < 2U) {
			break;
		}
		tag = dpb[offset++];
		length = dpb[offset++];
		check(dpb_length - offset >= length, "event timeout DPB item is truncated");
		if (dpb_length - offset < length) {
			break;
		}
		if (tag == isc_dpb_connect_timeout) {
			timeout_seen++;
			check(length == sizeof(expected) &&
				memcmp(dpb + offset, expected, sizeof(expected)) == 0,
				"event connect timeout DPB value is not little endian");
		}
		offset += length;
	}
	check(timeout_seen == 1, "event connect timeout DPB item is missing");

	dpb_length = 0U;
	check_success(ib_event_test_build_dpb(0U, dpb, sizeof(dpb), &dpb_length,
		&error), &error, "encode default event connect timeout");
	timeout_seen = 0;
	offset = 1U;
	while (offset < dpb_length) {
		unsigned char tag;
		unsigned char length;

		check(dpb_length - offset >= 2U, "default event timeout DPB item header is truncated");
		if (dpb_length - offset < 2U) {
			break;
		}
		tag = dpb[offset++];
		length = dpb[offset++];
		check(dpb_length - offset >= length, "default event timeout DPB item is truncated");
		if (dpb_length - offset < length) {
			break;
		}
		if (tag == isc_dpb_connect_timeout) {
			timeout_seen++;
		}
		offset += length;
	}
	check(timeout_seen == 0, "zero event connect timeout changed the native default");
}

static void test_cleanup_failures_are_retriable(void)
{
	char *error = NULL;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_fail_next_cancel(subscription, &error), &error,
		"inject cancel failure");
	check_failure(ib_event_stop(subscription, &error), &error,
		"stop after cancel failure", "cancel failure was not retained");
	check_success(ib_event_stop(subscription, &error), &error,
		"retry stop after cancel failure");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy after retried cancel");

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_fail_next_detach(subscription, &error), &error,
		"inject detach failure");
	check_failure(ib_event_destroy(subscription, &error), &error,
		"destroy after detach failure", "detach failure was not retained");
	check_success(ib_event_destroy(subscription, &error), &error,
		"retry destroy after detach failure");
}

static void test_rearm_failure_and_counter_overflow(void)
{
	const char *name = "native_event_rearm";
	const uint64_t zero[1] = {0U};
	const uint64_t one[1] = {1U};
	char *error = NULL;
	uint64_t counts[1] = {0U};
	int has_counts = 0;
	ib_event_subscription *subscription;

	subscription = ib_event_test_new(&name, 1U, &error);
	check(subscription != NULL && error == NULL, "create rearm test subscription");
	if (subscription == NULL) {
		if (error != NULL) {
			ib_event_error_free(error);
		}
		return;
	}
	check_success(ib_event_test_enable_rearm(subscription, &error), &error,
		"enable test rearm");
	check_success(ib_event_test_sync_next_queue(subscription, &error), &error,
		"schedule synchronous rearm callback");
	check_success(ib_event_test_emit_counts(subscription, 0U, zero, 1U, &error),
		&error, "exercise synchronous rearm callback");
	check_success(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "take synchronous rearm callback");
	check(!has_counts, "synchronous rearm callback produced a phantom event");
	check_success(ib_event_test_fail_next_rearm(subscription, &error), &error,
		"inject rearm failure");
	check_success(ib_event_test_emit_counts(subscription, 0U, zero, 1U, &error),
		&error, "establish rearm baseline");
	check_success(ib_event_test_emit_counts(subscription, 0U, one, 1U, &error),
		&error, "emit event with rearm failure");
	check_failure(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "observe rearm failure", "rearm failure was not surfaced");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy after rearm failure");

	subscription = ib_event_test_new(&name, 1U, &error);
	check(subscription != NULL && error == NULL,
		"create counter overflow test subscription");
	if (subscription == NULL) {
		if (error != NULL) {
			ib_event_error_free(error);
		}
		return;
	}
	check_success(ib_event_test_emit_counts(subscription, 0U, zero, 1U, &error),
		&error, "establish overflow baseline");
	check_success(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "take overflow baseline");
	check_success(ib_event_test_set_pending_count(subscription, 0U, 0U,
		UINT64_MAX, &error), &error, "set pending counter near overflow");
	check_success(ib_event_test_emit_counts(subscription, 0U, one, 1U, &error),
		&error, "emit counter overflow");
	check_failure(ib_event_take(subscription, counts, 1U, &has_counts, &error),
		&error, "observe counter overflow", "counter overflow was not surfaced");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy after counter overflow");
}

static void test_shared_gate_callback_and_lifecycle_admission(void)
{
	const uint64_t zero[1] = {0U};
	char *error = NULL;
	unsigned int ordinary_before;
	unsigned int exclusive_before;
	unsigned int queue_count_before;
	unsigned int queue_count_after;
	unsigned int outstanding_count;
	uint64_t callback_token;
	uint64_t event_id;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	ordinary_before = bridge_ordinary_entries;
	check_success(ib_event_test_emit_counts(subscription, 0U, zero, 1U, &error),
		&error, "run callback through shared gate");
	check(bridge_ordinary_entries > ordinary_before,
		"event callback bypassed ordinary native gate admission");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy callback gate subscription");

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	exclusive_before = bridge_exclusive_entries;
	check_success(ib_event_stop(subscription, &error), &error,
		"stop gate admission subscription");
	check(bridge_exclusive_entries > exclusive_before,
		"event cancellation bypassed exclusive native gate admission");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy gate admission subscription");

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_enable_rearm(subscription, &error), &error,
		"enable synchronous gate rearm");
	check_success(ib_event_test_sync_next_queue(subscription, &error), &error,
		"schedule synchronous gate callback");
	ordinary_before = bridge_ordinary_entries;
	check_success(ib_event_test_emit_counts(subscription, 0U, zero, 1U, &error),
		&error, "run synchronous callback rearm through shared gate");
	check(bridge_ordinary_entries - ordinary_before >= 2U,
		"synchronous callback rearm did not admit both native callbacks");
	check_success(ib_event_test_queue_state(subscription, 0U, &queue_count_before,
		&outstanding_count, &callback_token, &event_id, &error), &error,
		"inspect queued request before close");
	check_success(ib_event_stop(subscription, &error), &error,
		"stop synchronous gate rearm subscription");
	check_success(ib_event_test_emit_counts(subscription, 0U, zero, 1U, &error),
		&error, "invoke callback after close");
	check_success(ib_event_test_queue_state(subscription, 0U, &queue_count_after,
		&outstanding_count, &callback_token, &event_id, &error), &error,
		"inspect queued request after close");
	check(queue_count_after == queue_count_before,
		"callback after close queued a new native request");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy synchronous gate rearm subscription");
}

static void test_allocation_failures_are_safe(void)
{
	const char *name = "native_event_allocation";
	const int failpoints[] = {
		IB_EVENT_TEST_ALLOC_SUBSCRIPTION,
		IB_EVENT_TEST_ALLOC_NAMES,
		IB_EVENT_TEST_ALLOC_NAME,
		IB_EVENT_TEST_ALLOC_BLOCKS,
		IB_EVENT_TEST_ALLOC_PENDING,
		IB_EVENT_TEST_ALLOC_NATIVE_COUNTS
	};
	char *error = NULL;
	ib_event_subscription *subscription;
	size_t index;

	for (index = 0U; index < sizeof(failpoints) / sizeof(failpoints[0]); index++) {
		check_success(ib_event_test_fail_next_allocation(failpoints[index], &error),
			&error, "inject event allocation failure");
		subscription = ib_event_test_new(&name, 1U, &error);
		check(subscription == NULL && error != NULL,
			"event allocation failure did not fail subscription creation");
		if (error != NULL) {
			ib_event_error_free(error);
			error = NULL;
		}
		if (subscription != NULL) {
			(void) ib_event_destroy(subscription, &error);
			if (error != NULL) {
				ib_event_error_free(error);
				error = NULL;
			}
		}
	}

	check_success(ib_event_test_fail_next_allocation(IB_EVENT_TEST_ALLOC_DPB, &error),
		&error, "inject event DPB allocation failure");
	subscription = ib_event_open("/tmp/nonexistent-event-database",
		sizeof("/tmp/nonexistent-event-database") - 1U,
		"SYSDBA", 6U, "", 0U, NULL, 0U, NULL, 0U, NULL, 0U,
		"UTF8", 5U, 3, 0U, &name, 1U, &error);
	check(subscription == NULL && error != NULL,
		"event DPB allocation failure did not fail subscription creation");
	if (error != NULL) {
		ib_event_error_free(error);
		error = NULL;
	}

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_fail_next_allocation(IB_EVENT_TEST_ALLOC_EVENT_RESULT,
		&error), &error, "inject event result allocation failure");
	check_failure(ib_event_test_emit_counts(subscription, 0U,
		(const uint64_t[]){0U}, 1U, &error), &error,
		"emit after event result allocation failure",
		"event result allocation failure was not surfaced");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy after event result allocation failure");
}

typedef struct emit_thread_args {
	ib_event_subscription *subscription;
	int result;
	char *error;
} emit_thread_args;

static void *emit_thread(void *argument)
{
	emit_thread_args *args = (emit_thread_args *) argument;

	args->result = ib_event_test_emit(args->subscription, NULL, 0, &args->error);
	return NULL;
}

typedef struct destroy_thread_args {
	ib_event_subscription *subscription;
	int result;
	char *error;
} destroy_thread_args;

static void *destroy_thread(void *argument)
{
	destroy_thread_args *args = (destroy_thread_args *) argument;

	args->result = ib_event_destroy(args->subscription, &args->error);
	return NULL;
}

typedef struct queue_thread_args {
	ib_event_subscription *subscription;
	int result;
	char *error;
} queue_thread_args;

static void *queue_thread(void *argument)
{
	queue_thread_args *args = (queue_thread_args *) argument;

	args->result = ib_event_test_queue(args->subscription, &args->error);
	return NULL;
}

static void test_destroy_waits_for_paused_callback_release(void)
{
	pthread_t emit;
	pthread_t destroy;
	emit_thread_args emit_args = {0};
	destroy_thread_args destroy_args = {0};
	char *error = NULL;
	int entered = 0;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_pause_callback_release(subscription, &error),
		&error, "pause callback release");
	emit_args.subscription = subscription;
	check(pthread_create(&emit, NULL, emit_thread, &emit_args) == 0,
		"start paused callback release");
	if (failures != 0) {
		(void) ib_event_test_release_callback_release(&error);
		(void) pthread_join(emit, NULL);
		(void) ib_event_destroy(subscription, &error);
		return;
	}
	check_success(ib_event_test_wait_callback_release(1000, &entered, &error),
		&error, "wait for paused callback release");
	check(entered, "callback release did not reach its gate");

	destroy_args.subscription = subscription;
	check(pthread_create(&destroy, NULL, destroy_thread, &destroy_args) == 0,
		"start concurrent destroy");
	if (failures != 0) {
		(void) ib_event_test_release_callback_release(&error);
		(void) pthread_join(emit, NULL);
		(void) pthread_join(destroy, NULL);
		return;
	}
	check_success(ib_event_test_wait_stop(1000, &entered, &error), &error,
		"wait for concurrent stop");
	check(entered, "concurrent destroy did not enter stop");
	check_success(ib_event_test_wait_stop_done(100, &entered, &error), &error,
		"check concurrent destroy completion while callback is paused");
	check(!entered, "destroy completed before callback release was unblocked");
	check_success(ib_event_test_release_callback_release(&error), &error,
		"release paused callback release");
	check_success(ib_event_test_wait_stop_done(1000, &entered, &error), &error,
		"wait for concurrent destroy completion");
	check(entered, "concurrent destroy did not complete after callback release");
	check(pthread_join(emit, NULL) == 0,
		"join callback after concurrent destroy");
	check(pthread_join(destroy, NULL) == 0,
		"join destroy after callback release");
	check(emit_args.result == 0 && emit_args.error == NULL,
		"paused callback release returned an unexpected error");
	check(destroy_args.result == 0 && destroy_args.error == NULL,
		"concurrent destroy returned an unexpected error");
	if (emit_args.error != NULL) {
		ib_event_error_free(emit_args.error);
	}
	if (destroy_args.error != NULL) {
		ib_event_error_free(destroy_args.error);
	}
}

static void test_async_baseline_rearm_handoff_has_one_outstanding_request(void)
{
	pthread_t initial_queue;
	queue_thread_args queue_args = {0};
	char *error = NULL;
	int entered = 0;
	unsigned int queue_count = 0U;
	unsigned int outstanding_count = 0U;
	uint64_t callback_token = 0U;
	uint64_t event_id = 0U;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_enable_rearm(subscription, &error), &error,
		"enable asynchronous rearm");
	check_success(ib_event_test_pause_callback_rearm(subscription, &error),
		&error, "pause asynchronous callback rearm");
	check_success(ib_event_test_hold_queue(subscription, &error), &error,
		"hold initial queue owner");
	check_success(ib_event_test_start_async_baseline(subscription, &error),
		&error, "start asynchronous baseline callback");
	check_success(ib_event_test_wait_callback_rearm(1000, &entered, &error),
		&error, "wait for asynchronous baseline callback");
	check(entered, "asynchronous baseline callback did not reach rearm handoff");

	queue_args.subscription = subscription;
	check(pthread_create(&initial_queue, NULL, queue_thread, &queue_args) == 0,
		"start competing initial queue caller");
	if (failures != 0) {
		(void) ib_event_test_release_callback_rearm(&error);
		(void) ib_event_test_release_queue(&error);
		(void) ib_event_test_join_async_baseline(&error);
		(void) pthread_join(initial_queue, NULL);
		(void) ib_event_destroy(subscription, &error);
		return;
	}
	check_success(ib_event_test_wait_queue(1000, &entered, &error), &error,
		"wait for initial queue owner");
	check(entered, "initial queue caller did not become queue owner");
	check_success(ib_event_test_release_callback_rearm(&error), &error,
		"release asynchronous callback rearm");
	check_success(ib_event_test_join_async_baseline(&error), &error,
		"join asynchronous baseline callback");
	check_success(ib_event_test_release_queue(&error), &error,
		"release initial queue owner");
	check(pthread_join(initial_queue, NULL) == 0,
		"join initial queue caller");
	check(queue_args.result == 0 && queue_args.error == NULL,
		"initial queue caller returned an unexpected error");
	check_success(ib_event_test_queue_state(subscription, 0U, &queue_count,
		&outstanding_count, &callback_token, &event_id, &error), &error,
		"inspect asynchronous queue state");
	check(queue_count == 2U,
		"asynchronous rearm handoff issued a duplicate queue request");
	check(outstanding_count == 1U && callback_token != 0U && event_id != 0U,
		"asynchronous rearm handoff did not retain one request and event ID");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy asynchronous rearm subscription");
	if (queue_args.error != NULL) {
		ib_event_error_free(queue_args.error);
	}
}

static void test_cleanup_retains_subscription_when_error_allocation_fails(void)
{
	char *error = NULL;
	int result;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_fail_next_detach(subscription, &error), &error,
		"inject detach failure for error allocation test");
	check_success(ib_event_test_fail_next_allocation(IB_EVENT_TEST_ALLOC_ERROR,
		&error), &error, "inject event error allocation failure");
	result = ib_event_test_cleanup_unattached(subscription, &error);
	check(result != 0, "cleanup ignored detach failure with missing diagnostic");
	check(error == NULL, "error allocation failure unexpectedly produced a diagnostic");
	if (result == 0) {
		return;
	}
	check_success(ib_event_destroy(subscription, &error), &error,
		"retry cleanup after missing detach diagnostic");
}

static void test_multithreaded_callback_lifecycle(void)
{
	pthread_t thread;
	emit_thread_args args = {0};
	char *error = NULL;
	int entered = 0;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_hold_callback(subscription, &error), &error,
		"hold multithreaded callback");
	args.subscription = subscription;
	check(pthread_create(&thread, NULL, emit_thread, &args) == 0,
		"start multithreaded callback");
	if (failures != 0) {
		(void) ib_event_test_release_callback(subscription, &error);
		(void) pthread_join(thread, NULL);
		(void) ib_event_destroy(subscription, &error);
		return;
	}
	check_success(ib_event_test_wait_callback(subscription, 1000, &entered, &error),
		&error, "wait for multithreaded callback");
	check(entered, "multithreaded callback did not enter");
	check_success(ib_event_stop(subscription, &error), &error,
		"stop while callback is active");
	check_success(ib_event_test_release_callback(subscription, &error), &error,
		"release multithreaded callback");
	check(pthread_join(thread, NULL) == 0, "join multithreaded callback");
	check(args.result == 0 && args.error == NULL,
		"multithreaded callback returned an unexpected error");
	if (args.error != NULL) {
		ib_event_error_free(args.error);
	}
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy after active callback drained");
}

static void test_callback_pre_entry_after_destroy(void)
{
	char *error = NULL;
	int entered = 0;
	ib_event_subscription *subscription;

	subscription = new_single_test_subscription();
	if (subscription == NULL) {
		return;
	}
	check_success(ib_event_test_start_pre_entry_callback(subscription, &error), &error,
		"start pre-entry callback");
	check_success(ib_event_test_wait_pre_entry_callback(1000, &entered, &error), &error,
		"wait for pre-entry callback");
	check(entered, "pre-entry callback did not reach its gate");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy with callback before registry entry");
	check_success(ib_event_test_release_pre_entry_callback(&error), &error,
		"release pre-entry callback");
	check_success(ib_event_test_join_pre_entry_callback(&error), &error,
		"join pre-entry callback");
}

static void test_token_overflow(void)
{
	const char *names[16] = {
		"token_event_00", "token_event_01", "token_event_02",
		"token_event_03", "token_event_04", "token_event_05",
		"token_event_06", "token_event_07", "token_event_08",
		"token_event_09", "token_event_10", "token_event_11",
		"token_event_12", "token_event_13", "token_event_14",
		"token_event_15"
	};
	char *error = NULL;
	ib_event_subscription *subscription;

	check_success(ib_event_test_set_next_token(UINTPTR_MAX, &error), &error,
		"set token counter to maximum");
	subscription = ib_event_test_new(names, 16U, &error);
	check(subscription == NULL && error != NULL,
		"token counter overflow was not rejected");
	if (error != NULL) {
		check(strstr(error, "token") != NULL,
			"token counter overflow error did not identify tokens");
		ib_event_error_free(error);
	}
}

int main(void)
{
	const char *names[16] = {
		"native_event_00", "native_event_01", "native_event_02",
		"native_event_03", "native_event_04", "native_event_05",
		"native_event_06", "native_event_07", "native_event_08",
		"native_event_09", "native_event_10", "native_event_11",
		"native_event_12", "native_event_13", "native_event_14",
		"native_event_15"
	};
	const uint64_t baseline[15] = {0};
	const uint64_t first_block[15] = {
		1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1, 1
	};
	const uint64_t second_block[15] = {
		2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2, 2
	};
	const uint64_t last_baseline[1] = {0};
	const uint64_t last_first[1] = {3};
	const uint64_t last_second[1] = {4};
	uint64_t counts[16] = {0};
	int has_counts = 0;
	int ready;
	char *error = NULL;
	ib_event_subscription *subscription;
	size_t index;

	subscription = ib_event_test_new(names, 16U, &error);
	check(subscription != NULL && error == NULL,
		"create multi-block test subscription");
	if (subscription == NULL) {
		if (error != NULL) {
			ib_event_error_free(error);
		}
		return 1;
	}

	test_native_name_limit();
	test_connect_timeout_is_encoded_as_little_endian_integer();
	ready = 1;
	check_success(ib_event_ready(subscription, 0, &ready, &error), &error,
		"check event readiness before initial callbacks");
	check(ready == 0, "event subscription reported ready before baselines");
	check_success(ib_event_test_emit_counts(subscription, 0U, baseline, 15U,
		&error), &error, "establish first block baseline");
	check_success(ib_event_test_emit_counts(subscription, 1U, last_baseline, 1U,
		&error), &error, "establish second block baseline");
	ready = 0;
	check_success(ib_event_ready(subscription, 0, &ready, &error), &error,
		"check event readiness after initial callbacks");
	check(ready != 0, "event subscription did not report ready after baselines");
	check_success(ib_event_test_emit_counts(subscription, 0U, first_block, 15U,
		&error), &error, "emit first block counts");
	check_success(ib_event_test_emit_counts(subscription, 1U, last_first, 1U,
		&error), &error, "emit second block counts");
	check_success(ib_event_test_emit_counts(subscription, 0U, second_block, 15U,
		&error), &error, "emit accumulated first block counts");
	check_success(ib_event_test_emit_counts(subscription, 1U, last_second, 1U,
		&error), &error, "emit accumulated second block counts");

	ready = 0;
	check_success(ib_event_wait(subscription, 1000, &ready, &error), &error,
		"wait for accumulated event counts");
	check(ready != 0, "event notification pipe was not signaled");
	check_success(ib_event_take(subscription, counts, 16U, &has_counts, &error),
		&error, "take accumulated event counts");
	check(has_counts != 0, "accumulated event counts were not reported");
	for (index = 0U; index < 15U; index++) {
		check(counts[index] == 2U, "first block count did not accumulate");
	}
	check(counts[15] == 4U, "second block count did not accumulate");

	check_success(ib_event_stop(subscription, &error), &error,
		"stop multi-block test subscription");
	check_success(ib_event_destroy(subscription, &error), &error,
		"destroy multi-block test subscription");

	test_initial_baseline_and_wrap();
	test_cleanup_failures_are_retriable();
	test_rearm_failure_and_counter_overflow();
	test_shared_gate_callback_and_lifecycle_admission();
	test_allocation_failures_are_safe();
	test_multithreaded_callback_lifecycle();
	test_destroy_waits_for_paused_callback_release();
	test_async_baseline_rearm_handoff_has_one_outstanding_request();
	test_cleanup_retains_subscription_when_error_allocation_fails();
	test_callback_pre_entry_after_destroy();
	test_token_overflow();

	return failures == 0 ? EXIT_SUCCESS : EXIT_FAILURE;
}
