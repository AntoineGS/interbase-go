#define _POSIX_C_SOURCE 200809L

#include "native.h"

#include <ibase.h>
#include <errno.h>
#include <fcntl.h>
#include <inttypes.h>
#include <limits.h>
#include <poll.h>
#include <pthread.h>
#include <stdatomic.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <unistd.h>

#define IB_EVENT_STATUS_VECTOR_LENGTH 20
#define IB_EVENT_SQL_MESSAGE_LENGTH 256U
#define IB_EVENT_ERROR_MESSAGE_LENGTH 2560U
#define IB_EVENT_MAX_NAME_LENGTH 127U
#define IB_EVENT_MAX_BUFFER_LENGTH ((size_t) SHRT_MAX)

typedef struct ib_event_block ib_event_block;

struct ib_event_block {
	struct ib_event_subscription *subscription;
	char *event_buffer;
	char *result_buffer;
	ISC_USHORT buffer_length;
	ISC_LONG event_id;
	ISC_ULONG *native_counts;
	size_t event_count;
	size_t name_offset;
	int first_callback;
	int queued;
	int queue_in_progress;
	int callback_during_queue;
	unsigned int test_queue_count;
	unsigned int test_outstanding;
	int cancel_pending;
	ISC_LONG cancel_event_id;
	uintptr_t callback_token;
	int registered;
	struct ib_event_block *registry_next;
};

struct ib_event_subscription {
	isc_db_handle database;
	ib_event_block *blocks;
	size_t block_count;
	char **event_names;
	size_t event_name_count;
	uint64_t *pending_counts;
	int notify_read;
	int notify_write;
	pthread_mutex_t mutex;
	pthread_cond_t callback_done;
	_Atomic unsigned int callbacks;
	_Atomic int closing;
	int stop_done;
	int database_attached;
	size_t initial_callbacks;
	int test_mode;
	int test_rearm;
	int hold_callback;
	int callback_entered;
	_Atomic unsigned int test_cancel_failures;
	_Atomic unsigned int test_detach_failures;
	_Atomic unsigned int test_rearm_failures;
	_Atomic unsigned int test_sync_queue_callbacks;
	_Atomic int test_pause_callback_release;
	_Atomic int test_pause_callback_rearm;
	char *callback_error;
	struct ib_event_subscription *quarantine_next;
	int quarantined;
};

static pthread_mutex_t event_registry_mutex = PTHREAD_MUTEX_INITIALIZER;
static ib_event_block *event_registry_head;
static uintptr_t event_next_token = (uintptr_t) 1U;
static uintptr_t event_last_token;
static int event_token_exhausted;
static ib_event_subscription *event_quarantine_head;
static _Atomic int event_test_allocation_failpoint;

typedef struct event_test_pre_entry {
	pthread_mutex_t mutex;
	pthread_cond_t condition;
	pthread_t thread;
	uintptr_t token;
	int active;
	int entered;
	int release;
} event_test_pre_entry;

static event_test_pre_entry event_pre_entry = {
	.mutex = PTHREAD_MUTEX_INITIALIZER,
	.condition = PTHREAD_COND_INITIALIZER
};

typedef struct event_test_callback_release {
	pthread_mutex_t mutex;
	pthread_cond_t condition;
	ib_event_subscription *subscription;
	int active;
	int entered;
	int release;
	int stop_entered;
	int stop_done;
} event_test_callback_release;

static event_test_callback_release event_callback_release_test = {
	.mutex = PTHREAD_MUTEX_INITIALIZER,
	.condition = PTHREAD_COND_INITIALIZER
};

typedef struct event_test_gate {
	pthread_mutex_t mutex;
	pthread_cond_t condition;
	ib_event_subscription *subscription;
	int active;
	int entered;
	int release;
} event_test_gate;

static event_test_gate event_callback_rearm_test = {
	.mutex = PTHREAD_MUTEX_INITIALIZER,
	.condition = PTHREAD_COND_INITIALIZER
};

static event_test_gate event_queue_test = {
	.mutex = PTHREAD_MUTEX_INITIALIZER,
	.condition = PTHREAD_COND_INITIALIZER
};

typedef struct event_test_async_callback {
	pthread_mutex_t mutex;
	pthread_t thread;
	ib_event_subscription *subscription;
	uintptr_t token;
	char *updated;
	short length;
	int active;
} event_test_async_callback;

static event_test_async_callback event_async_callback_test = {
	.mutex = PTHREAD_MUTEX_INITIALIZER
};

static int event_test_should_fail_allocation(int failpoint)
{
	int expected;

	if (failpoint == 0) {
		return 0;
	}
	expected = failpoint;
	return atomic_compare_exchange_strong_explicit(
		&event_test_allocation_failpoint, &expected, 0,
		memory_order_acq_rel, memory_order_acquire);
}

static void *event_malloc(size_t size, int failpoint)
{
	if (event_test_should_fail_allocation(failpoint)) {
		return NULL;
	}
	return malloc(size);
}

static void *event_calloc(size_t count, size_t size, int failpoint)
{
	if (event_test_should_fail_allocation(failpoint)) {
		return NULL;
	}
	return calloc(count, size);
}

static char *event_copy_string(const char *value, size_t length)
{
	char *copy;

	if (length == SIZE_MAX) {
		return NULL;
	}
	copy = (char *) event_malloc(length + 1U, 0);
	if (copy == NULL) {
		return NULL;
	}
	if (length != 0U && value != NULL) {
		memcpy(copy, value, length);
	}
	copy[length] = '\0';
	return copy;
}

static char *event_copy_error(const char *value, size_t length)
{
	char *copy;

	if (length == SIZE_MAX) {
		return NULL;
	}
	copy = (char *) event_malloc(length + 1U, IB_EVENT_TEST_ALLOC_ERROR);
	if (copy == NULL) {
		return NULL;
	}
	if (length != 0U && value != NULL) {
		memcpy(copy, value, length);
	}
	copy[length] = '\0';
	return copy;
}

static char *event_copy_name(const char *value, size_t length)
{
	char *copy;

	if (length == SIZE_MAX) {
		return NULL;
	}
	copy = (char *) event_malloc(length + 1U, IB_EVENT_TEST_ALLOC_NAME);
	if (copy == NULL) {
		return NULL;
	}
	if (length != 0U && value != NULL) {
		memcpy(copy, value, length);
	}
	copy[length] = '\0';
	return copy;
}

static void event_give_error(char **error, char *message)
{
	if (error != NULL) {
		*error = message;
	} else {
		free(message);
	}
}

static int event_fail(char **error, const char *message)
{
	char *copy;

	copy = event_copy_error(message, strlen(message));
	event_give_error(error, copy);
	return -1;
}

static char *event_join_errors(const char *first, const char *second)
{
	static const char fallback[] = "event operation and cleanup both failed";
	size_t first_length;
	size_t second_length;
	char *joined;

	first_length = strlen(first);
	second_length = strlen(second);
	if (second_length > SIZE_MAX - 3U ||
		first_length > SIZE_MAX - second_length - 3U) {
		return event_copy_string(fallback, sizeof(fallback) - 1U);
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

static void event_append_error(char **first_error, char *next_error)
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
	joined = event_join_errors(*first_error, next_error);
	if (joined != NULL) {
		free(*first_error);
		free(next_error);
		*first_error = joined;
	} else {
		free(next_error);
	}
}

static int event_primary_status(const ISC_STATUS *status, ISC_STATUS *value)
{
	size_t index;
	ISC_STATUS argument;

	if (status == NULL || value == NULL) {
		return 0;
	}
	for (index = 0U; index < IB_EVENT_STATUS_VECTOR_LENGTH;) {
		argument = status[index];
		if (argument == isc_arg_end) {
			return 0;
		}
		if (index + 1U >= IB_EVENT_STATUS_VECTOR_LENGTH) {
			return 0;
		}
		if (argument == isc_arg_gds || argument == isc_arg_warning) {
			*value = status[index + 1U];
			return 1;
		}
		if (argument == isc_arg_cstring) {
			if (index + 2U >= IB_EVENT_STATUS_VECTOR_LENGTH) {
				return 0;
			}
			index += 3U;
		} else {
			index += 2U;
		}
	}
	return 0;
}

static int event_fail_status(char **error, const char *operation,
	ISC_STATUS *status)
{
	char sql_message[IB_EVENT_SQL_MESSAGE_LENGTH];
	char message[IB_EVENT_ERROR_MESSAGE_LENGTH];
	ISC_STATUS primary_status;
	ISC_LONG sqlcode;
	int has_primary_status;

	has_primary_status = event_primary_status(status, &primary_status);
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
	return event_fail(error, message);
}

static void event_close_fd(int *descriptor)
{
	if (descriptor != NULL && *descriptor >= 0) {
		(void) close(*descriptor);
		*descriptor = -1;
	}
}

static int event_set_nonblocking(int descriptor)
{
	int flags;

	flags = fcntl(descriptor, F_GETFL, 0);
	if (flags < 0 || fcntl(descriptor, F_SETFL, flags | O_NONBLOCK) != 0) {
		return -1;
	}
	return 0;
}

static int event_signal(ib_event_subscription *subscription)
{
	const unsigned char value = 1U;
	ssize_t result;

	if (subscription == NULL || subscription->notify_write < 0) {
		return -1;
	}
	result = write(subscription->notify_write, &value, sizeof(value));
	if (result == (ssize_t) sizeof(value) || (result < 0 && errno == EAGAIN)) {
		return 0;
	}
	return -1;
}

static void event_set_callback_error_locked(ib_event_subscription *subscription,
	const char *message)
{
	if (subscription->callback_error == NULL) {
		subscription->callback_error = event_copy_string(message, strlen(message));
	}
	atomic_store_explicit(&subscription->closing, 1, memory_order_release);
	(void) event_signal(subscription);
}

static void event_set_callback_error_owned_locked(
	ib_event_subscription *subscription, char *message)
{
	if (message == NULL) {
		message = event_copy_string("event callback failed", sizeof("event callback failed") - 1U);
	}
	if (subscription->callback_error == NULL) {
		subscription->callback_error = message;
	} else {
		free(message);
	}
	atomic_store_explicit(&subscription->closing, 1, memory_order_release);
	(void) event_signal(subscription);
}

static ib_event_block *event_registry_find_locked(uintptr_t token)
{
	ib_event_block *block;

	for (block = event_registry_head; block != NULL; block = block->registry_next) {
		if (block->registered && block->callback_token == token) {
			return block;
		}
	}
	return NULL;
}

static int event_registry_register_block(ib_event_block *block, char **error)
{
	uintptr_t token;

	if (block == NULL) {
		return event_fail(error, "event callback block is unavailable");
	}
	(void) pthread_mutex_lock(&event_registry_mutex);
	if (event_token_exhausted || event_next_token == 0U) {
		(void) pthread_mutex_unlock(&event_registry_mutex);
		return event_fail(error, "event callback token counter overflow");
	}
	token = event_next_token;
	if (token <= event_last_token) {
		(void) pthread_mutex_unlock(&event_registry_mutex);
		return event_fail(error, "event callback token would be reused");
	}
	event_last_token = token;
	if (token == UINTPTR_MAX) {
		event_token_exhausted = 1;
		event_next_token = 0U;
	} else {
		event_next_token = token + (uintptr_t) 1U;
	}
	block->callback_token = token;
	block->registered = 1;
	block->registry_next = event_registry_head;
	event_registry_head = block;
	(void) pthread_mutex_unlock(&event_registry_mutex);
	return 0;
}

static void event_registry_unregister_subscription(ib_event_subscription *subscription)
{
	ib_event_block **link;
	ib_event_block *block;

	if (subscription == NULL) {
		return;
	}
	(void) pthread_mutex_lock(&event_registry_mutex);
	link = &event_registry_head;
	while (*link != NULL) {
		block = *link;
		if (block->subscription != subscription) {
			link = &block->registry_next;
			continue;
		}
		*link = block->registry_next;
		block->registry_next = NULL;
		block->registered = 0;
	}
	(void) pthread_mutex_unlock(&event_registry_mutex);
}

static ib_event_block *event_callback_acquire(uintptr_t token,
	ib_event_subscription **subscription)
{
	ib_event_block *block;

	if (token == 0U || subscription == NULL) {
		return NULL;
	}
	(void) pthread_mutex_lock(&event_registry_mutex);
	block = event_registry_find_locked(token);
	if (block == NULL || block->subscription == NULL) {
		(void) pthread_mutex_unlock(&event_registry_mutex);
		return NULL;
	}
	*subscription = block->subscription;
	(void) atomic_fetch_add_explicit(&(*subscription)->callbacks, 1U,
		memory_order_acq_rel);
	(void) pthread_mutex_unlock(&event_registry_mutex);
	return block;
}

static int event_test_consume_failure(_Atomic unsigned int *failures)
{
	unsigned int expected;

	expected = atomic_load_explicit(failures, memory_order_acquire);
	while (expected != 0U) {
		if (atomic_compare_exchange_weak_explicit(failures, &expected,
			expected - 1U, memory_order_acq_rel, memory_order_acquire)) {
			return 1;
		}
	}
	return 0;
}

static void event_test_note_stop(ib_event_subscription *subscription)
{
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	if (event_callback_release_test.active &&
		event_callback_release_test.subscription == subscription) {
		event_callback_release_test.stop_entered = 1;
		(void) pthread_cond_broadcast(&event_callback_release_test.condition);
	}
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
}

static void event_test_note_stop_done(ib_event_subscription *subscription)
{
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	if (event_callback_release_test.active &&
		event_callback_release_test.subscription == subscription) {
		event_callback_release_test.stop_done = 1;
		(void) pthread_cond_broadcast(&event_callback_release_test.condition);
	}
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
}

static int event_test_wait_release_gate(int timeout_ms, int *entered,
	int wait_for_stop, char **error)
{
	struct timespec deadline;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (entered == NULL || timeout_ms < 0) {
		return event_fail(error, "event test callback release wait arguments are invalid");
	}
	if (clock_gettime(CLOCK_REALTIME, &deadline) != 0) {
		return event_fail(error, "get event test callback release clock failed");
	}
	deadline.tv_sec += timeout_ms / 1000;
	deadline.tv_nsec += (long) (timeout_ms % 1000) * 1000000L;
	if (deadline.tv_nsec >= 1000000000L) {
		deadline.tv_sec++;
		deadline.tv_nsec -= 1000000000L;
	}
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	if (!event_callback_release_test.active) {
		(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
		return event_fail(error, "event test callback release is inactive");
	}
	while ((wait_for_stop == 0 && !event_callback_release_test.entered) ||
		(wait_for_stop == 1 && !event_callback_release_test.stop_entered) ||
		(wait_for_stop == 2 && !event_callback_release_test.stop_done)) {
		result = pthread_cond_timedwait(&event_callback_release_test.condition,
			&event_callback_release_test.mutex, &deadline);
		if (result == ETIMEDOUT) {
			*entered = 0;
			(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
			return 0;
		}
		if (result != 0) {
			(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
			return event_fail(error, "wait for event test callback release failed");
		}
	}
	*entered = 1;
	if (wait_for_stop == 2 && event_callback_release_test.release) {
		event_callback_release_test.active = 0;
		event_callback_release_test.subscription = NULL;
	}
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
	return 0;
}

static void event_test_pause_release_gate(ib_event_subscription *subscription)
{
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	event_callback_release_test.subscription = subscription;
	event_callback_release_test.active = 1;
	event_callback_release_test.entered = 0;
	event_callback_release_test.release = 0;
	event_callback_release_test.stop_entered = 0;
	event_callback_release_test.stop_done = 0;
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
	atomic_store_explicit(&subscription->test_pause_callback_release, 1,
		memory_order_release);
}

static void event_test_release_release_gate(void)
{
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	event_callback_release_test.release = 1;
	(void) pthread_cond_broadcast(&event_callback_release_test.condition);
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
}

static int event_test_activate_gate(event_test_gate *gate,
	ib_event_subscription *subscription, char **error, const char *message)
{
	(void) pthread_mutex_lock(&gate->mutex);
	if (gate->active) {
		(void) pthread_mutex_unlock(&gate->mutex);
		return event_fail(error, message);
	}
	gate->subscription = subscription;
	gate->active = 1;
	gate->entered = 0;
	gate->release = 0;
	(void) pthread_mutex_unlock(&gate->mutex);
	return 0;
}

static int event_test_wait_gate(event_test_gate *gate, int timeout_ms,
	int *entered, char **error, const char *inactive_message,
	const char *clock_message, const char *wait_message)
{
	struct timespec deadline;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (entered == NULL || timeout_ms < 0) {
		return event_fail(error, "event test gate wait arguments are invalid");
	}
	if (clock_gettime(CLOCK_REALTIME, &deadline) != 0) {
		return event_fail(error, clock_message);
	}
	deadline.tv_sec += timeout_ms / 1000;
	deadline.tv_nsec += (long) (timeout_ms % 1000) * 1000000L;
	if (deadline.tv_nsec >= 1000000000L) {
		deadline.tv_sec++;
		deadline.tv_nsec -= 1000000000L;
	}
	(void) pthread_mutex_lock(&gate->mutex);
	if (!gate->active) {
		(void) pthread_mutex_unlock(&gate->mutex);
		return event_fail(error, inactive_message);
	}
	while (!gate->entered) {
		result = pthread_cond_timedwait(&gate->condition, &gate->mutex,
			&deadline);
		if (result == ETIMEDOUT) {
			*entered = 0;
			(void) pthread_mutex_unlock(&gate->mutex);
			return 0;
		}
		if (result != 0) {
			(void) pthread_mutex_unlock(&gate->mutex);
			return event_fail(error, wait_message);
		}
	}
	*entered = 1;
	(void) pthread_mutex_unlock(&gate->mutex);
	return 0;
}

static int event_test_release_gate(event_test_gate *gate, char **error,
	const char *inactive_message)
{
	if (error != NULL) {
		*error = NULL;
	}
	(void) pthread_mutex_lock(&gate->mutex);
	if (!gate->active) {
		(void) pthread_mutex_unlock(&gate->mutex);
		return event_fail(error, inactive_message);
	}
	gate->release = 1;
	(void) pthread_cond_broadcast(&gate->condition);
	(void) pthread_mutex_unlock(&gate->mutex);
	return 0;
}

static void event_test_wait_callback_rearm_gate(ib_event_subscription *subscription)
{
	(void) pthread_mutex_lock(&event_callback_rearm_test.mutex);
	if (event_callback_rearm_test.active &&
		event_callback_rearm_test.subscription == subscription) {
		event_callback_rearm_test.entered = 1;
		(void) pthread_cond_broadcast(&event_callback_rearm_test.condition);
		while (!event_callback_rearm_test.release) {
			(void) pthread_cond_wait(&event_callback_rearm_test.condition,
				&event_callback_rearm_test.mutex);
		}
		event_callback_rearm_test.active = 0;
		event_callback_rearm_test.subscription = NULL;
	}
	(void) pthread_mutex_unlock(&event_callback_rearm_test.mutex);
}

static void event_test_wait_queue_gate(ib_event_subscription *subscription)
{
	(void) pthread_mutex_lock(&event_queue_test.mutex);
	if (event_queue_test.active && event_queue_test.subscription == subscription) {
		event_queue_test.entered = 1;
		(void) pthread_cond_broadcast(&event_queue_test.condition);
		while (!event_queue_test.release) {
			(void) pthread_cond_wait(&event_queue_test.condition,
				&event_queue_test.mutex);
		}
		event_queue_test.active = 0;
		event_queue_test.subscription = NULL;
	}
	(void) pthread_mutex_unlock(&event_queue_test.mutex);
}

static void event_callback_release(ib_event_subscription *subscription)
{
	(void) pthread_mutex_lock(&subscription->mutex);
	if (atomic_exchange_explicit(&subscription->test_pause_callback_release, 0,
		memory_order_acq_rel) != 0) {
		(void) pthread_mutex_lock(&event_callback_release_test.mutex);
		event_callback_release_test.entered = 1;
		(void) pthread_cond_broadcast(&event_callback_release_test.condition);
		while (!event_callback_release_test.release) {
			(void) pthread_cond_wait(&event_callback_release_test.condition,
				&event_callback_release_test.mutex);
		}
		(void) pthread_cond_broadcast(&event_callback_release_test.condition);
		(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
	}
	(void) atomic_fetch_sub_explicit(&subscription->callbacks, 1U,
		memory_order_acq_rel);
	(void) pthread_cond_broadcast(&subscription->callback_done);
	(void) pthread_mutex_unlock(&subscription->mutex);
}

static void event_callback(void *argument, short length, char *updated);
static int event_queue_block(ib_event_subscription *subscription,
	ib_event_block *block, char **error);

static int event_gate_enter(uintptr_t *token, char **error)
{
	if (token == NULL) {
		return event_fail(error, "event native gate token storage is unavailable");
	}
	*token = ib_event_gate_enter();
	if (*token == 0U) {
		return event_fail(error, "event native gate admission failed");
	}
	return 0;
}

static void event_gate_leave(uintptr_t *token)
{
	if (token == NULL || *token == 0U) {
		return;
	}
	ib_event_gate_leave(*token);
	*token = 0U;
}

static int event_gate_enter_exclusive(uintptr_t *token, char **error)
{
	if (token == NULL) {
		return event_fail(error,
			"event exclusive native gate token storage is unavailable");
	}
	*token = ib_event_gate_enter_exclusive();
	if (*token == 0U) {
		return event_fail(error, "event exclusive native gate admission failed");
	}
	return 0;
}

static void event_gate_leave_exclusive(uintptr_t *token)
{
	if (token == NULL || *token == 0U) {
		return;
	}
	ib_event_gate_leave_exclusive(*token);
	*token = 0U;
}

static void *event_test_async_baseline_runner(void *argument)
{
	uintptr_t token;
	char *updated;
	short length;

	(void) argument;
	(void) pthread_mutex_lock(&event_async_callback_test.mutex);
	token = event_async_callback_test.token;
	updated = event_async_callback_test.updated;
	length = event_async_callback_test.length;
	(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
	event_callback((void *) token, length, updated);
	return NULL;
}

static isc_callback event_callback_pointer(void)
{
	union {
		void (*typed)(void *, short, char *);
		isc_callback sdk;
	} callback;

	/* The official SDK exposes isc_callback without a parameter prototype. */
	callback.typed = event_callback;
	return callback.sdk;
}

static void event_callback(void *argument, short length, char *updated)
{
	ib_event_block *block;
	ib_event_subscription *subscription;
	uintptr_t token;
	uintptr_t gate_token = 0U;
	size_t index;
	int should_queue;
	char *queue_error = NULL;

	/* argument is an opaque token. It is never dereferenced before lookup. */
	token = (uintptr_t) argument;
	block = event_callback_acquire(token, &subscription);
	if (block == NULL) {
		return;
	}

	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->test_mode && block->test_outstanding != 0U) {
		block->test_outstanding--;
	}
	if (block->queue_in_progress) {
		block->callback_during_queue = 1;
	} else {
		block->queued = 0;
	}
	if (subscription->test_mode && subscription->hold_callback) {
		subscription->callback_entered = 1;
		(void) pthread_cond_broadcast(&subscription->callback_done);
		while (subscription->hold_callback &&
			atomic_load_explicit(&subscription->closing, memory_order_acquire) == 0) {
			(void) pthread_cond_wait(&subscription->callback_done, &subscription->mutex);
		}
	}
	if (atomic_load_explicit(&subscription->closing, memory_order_acquire) != 0) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		event_callback_release(subscription);
		return;
	}
	if (updated == NULL || length < 0 ||
		(size_t) length != (size_t) block->buffer_length) {
		event_set_callback_error_locked(subscription,
			"event callback returned an invalid result buffer");
		(void) pthread_mutex_unlock(&subscription->mutex);
		event_callback_release(subscription);
		return;
	}
	/*
	 * Never wait for the process-wide gate while holding the subscription
	 * mutex.  The callback count and queue state are protected by that mutex;
	 * native counter processing takes the ordinary gate first and then the
	 * mutex, while stop waits for callbacks without holding the gate.
	 */
	(void) pthread_mutex_unlock(&subscription->mutex);
	if (event_gate_enter(&gate_token, &queue_error) != 0) {
		(void) pthread_mutex_lock(&subscription->mutex);
		event_set_callback_error_owned_locked(subscription, queue_error);
		(void) pthread_mutex_unlock(&subscription->mutex);
		event_callback_release(subscription);
		return;
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (atomic_load_explicit(&subscription->closing, memory_order_acquire) != 0) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		event_gate_leave(&gate_token);
		event_callback_release(subscription);
		return;
	}
	memmove(block->result_buffer, updated, (size_t) length);
	isc_event_counts(block->native_counts, (short) block->buffer_length,
		block->event_buffer, block->result_buffer);
	if (block->first_callback) {
		block->first_callback = 0;
		subscription->initial_callbacks++;
		(void) pthread_cond_broadcast(&subscription->callback_done);
	} else {
		int overflow = 0;

		for (index = 0U; index < block->event_count; index++) {
			uint64_t current = subscription->pending_counts[block->name_offset + index];
			uint64_t increment = (uint64_t) block->native_counts[index];

			if (UINT64_MAX - current < increment) {
				overflow = 1;
				break;
			}
		}
		if (overflow) {
			event_set_callback_error_locked(subscription,
				"event occurrence count overflow");
		} else {
			for (index = 0U; index < block->event_count; index++) {
				uint64_t current = subscription->pending_counts[block->name_offset + index];
				uint64_t increment = (uint64_t) block->native_counts[index];

				subscription->pending_counts[block->name_offset + index] = current + increment;
			}
			if (event_signal(subscription) != 0) {
				event_set_callback_error_locked(subscription,
					"event notification signal failed");
			}
		}
	}

	should_queue = (subscription->test_mode == 0 || subscription->test_rearm) &&
		subscription->callback_error == NULL &&
		atomic_load_explicit(&subscription->closing, memory_order_acquire) == 0;
	(void) pthread_mutex_unlock(&subscription->mutex);
	event_gate_leave(&gate_token);
	if (should_queue && atomic_exchange_explicit(
		&subscription->test_pause_callback_rearm, 0, memory_order_acq_rel) != 0) {
		event_test_wait_callback_rearm_gate(subscription);
	}
	if (should_queue && event_queue_block(subscription, block, &queue_error) != 0) {
		(void) pthread_mutex_lock(&subscription->mutex);
		event_set_callback_error_owned_locked(subscription, queue_error);
		(void) pthread_mutex_unlock(&subscription->mutex);
	}
	event_callback_release(subscription);
	return;
}

static int event_queue_native(ib_event_subscription *subscription,
	ib_event_block *block, char **error)
{
	ISC_STATUS status[IB_EVENT_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	uintptr_t gate_token = 0U;

	if (subscription->test_mode) {
		if (subscription->test_rearm &&
			event_test_consume_failure(&subscription->test_rearm_failures)) {
			return event_fail(error, "queue test event request failed");
		}
		event_test_wait_queue_gate(subscription);
		(void) pthread_mutex_lock(&subscription->mutex);
		block->test_queue_count++;
		block->test_outstanding++;
		block->event_id = (ISC_LONG) block->test_queue_count;
		(void) pthread_mutex_unlock(&subscription->mutex);
		if (event_test_consume_failure(&subscription->test_sync_queue_callbacks)) {
			event_callback((void *) block->callback_token,
				(short) block->buffer_length, block->result_buffer);
		}
		return 0;
	}
	if (event_gate_enter(&gate_token, error) != 0) {
		return -1;
	}
	memset(status, 0, sizeof(status));
	result = isc_que_events(status, &subscription->database,
		&block->event_id, (short) block->buffer_length,
		block->event_buffer, event_callback_pointer(),
		(void *) block->callback_token);
	event_gate_leave(&gate_token);
	if (result != 0) {
		return event_fail_status(error, "queue database events", status);
	}
	return 0;
}

static int event_queue_block(ib_event_subscription *subscription,
	ib_event_block *block, char **error)
{
	int callback_during_queue;
	int should_requeue;
	char *queue_error = NULL;

	if (subscription == NULL || block == NULL || block->subscription != subscription) {
		return event_fail(error, "event queue state is invalid");
	}
	for (;;) {
		(void) pthread_mutex_lock(&subscription->mutex);
		if (atomic_load_explicit(&subscription->closing, memory_order_acquire) != 0) {
			(void) pthread_mutex_unlock(&subscription->mutex);
			return 0;
		}
		if (block->queue_in_progress) {
			(void) pthread_mutex_unlock(&subscription->mutex);
			return 0;
		}
		if (block->queued) {
			(void) pthread_mutex_unlock(&subscription->mutex);
			return 0;
		}
		block->queue_in_progress = 1;
		block->callback_during_queue = 0;
		(void) pthread_mutex_unlock(&subscription->mutex);

		if (event_queue_native(subscription, block, &queue_error) != 0) {
			(void) pthread_mutex_lock(&subscription->mutex);
			block->queue_in_progress = 0;
			block->queued = 0;
			(void) pthread_cond_broadcast(&subscription->callback_done);
			(void) pthread_mutex_unlock(&subscription->mutex);
			event_append_error(error, queue_error);
			return -1;
		}

		(void) pthread_mutex_lock(&subscription->mutex);
		block->queue_in_progress = 0;
		callback_during_queue = block->callback_during_queue;
		should_requeue = callback_during_queue &&
			atomic_load_explicit(&subscription->closing, memory_order_acquire) == 0;
		block->queued = callback_during_queue ? 0 : 1;
		(void) pthread_cond_broadcast(&subscription->callback_done);
		(void) pthread_mutex_unlock(&subscription->mutex);
		if (!should_requeue) {
			return 0;
		}
	}
}

static void event_free_client_buffer(char **buffer)
{
	if (buffer == NULL || *buffer == NULL) {
		return;
	}
	(void) isc_free(*buffer);
	*buffer = NULL;
}

static void event_release_block(ib_event_subscription *subscription)
{
	size_t index;

	if (subscription == NULL) {
		return;
	}
	if (subscription->blocks != NULL) {
		for (index = 0U; index < subscription->block_count; index++) {
			event_free_client_buffer(&subscription->blocks[index].event_buffer);
			event_free_client_buffer(&subscription->blocks[index].result_buffer);
			free(subscription->blocks[index].native_counts);
			subscription->blocks[index].native_counts = NULL;
		}
	}
	free(subscription->blocks);
	subscription->blocks = NULL;
	subscription->block_count = 0U;
	free(subscription->pending_counts);
	subscription->pending_counts = NULL;
}

static void event_release_names(ib_event_subscription *subscription)
{
	size_t index;

	if (subscription == NULL || subscription->event_names == NULL) {
		return;
	}
	for (index = 0U; index < subscription->event_name_count; index++) {
		free(subscription->event_names[index]);
	}
	free(subscription->event_names);
	subscription->event_names = NULL;
	subscription->event_name_count = 0U;
}

static int event_add_size(size_t *total, size_t value)
{
	if (total == NULL || value > SIZE_MAX - *total) {
		return -1;
	}
	*total += value;
	return 0;
}

static void event_dpb_add_string(unsigned char *dpb, size_t *offset,
	unsigned char code, const char *value, size_t length)
{
	dpb[(*offset)++] = code;
	dpb[(*offset)++] = (unsigned char) length;
	if (length != 0U) {
		memcpy(dpb + *offset, value, length);
		*offset += length;
	}
}

static void event_dpb_add_byte(unsigned char *dpb, size_t *offset,
	unsigned char code, unsigned char value)
{
	dpb[(*offset)++] = code;
	dpb[(*offset)++] = 1U;
	dpb[(*offset)++] = value;
}

static void event_dpb_add_integer(unsigned char *dpb, size_t *offset,
	unsigned char code, uint32_t value)
{
	dpb[(*offset)++] = code;
	dpb[(*offset)++] = 4U;
	dpb[(*offset)++] = (unsigned char) (value & 0xffU);
	dpb[(*offset)++] = (unsigned char) ((value >> 8U) & 0xffU);
	dpb[(*offset)++] = (unsigned char) ((value >> 16U) & 0xffU);
	dpb[(*offset)++] = (unsigned char) ((value >> 24U) & 0xffU);
}

static int event_build_dpb(const char *user, size_t user_length,
	const char *password, size_t password_length, const char *role,
	size_t role_length, const char *encrypted_password,
	size_t encrypted_password_length, const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, uint32_t connect_timeout,
	unsigned char **dpb_result, size_t *dpb_length_result, char **error)
{
	unsigned char *dpb;
	size_t dpb_length;
	size_t offset;

	if (dpb_result == NULL || dpb_length_result == NULL) {
		return event_fail(error, "event connection parameter storage is unavailable");
	}
	*dpb_result = NULL;
	*dpb_length_result = 0U;
	dpb_length = 1U;
	if (event_add_size(&dpb_length, 2U + user_length) != 0 ||
		event_add_size(&dpb_length, 2U + password_length) != 0 ||
		event_add_size(&dpb_length, 3U) != 0 ||
		event_add_size(&dpb_length, 2U + charset_length) != 0 ||
		(role_length != 0U && event_add_size(&dpb_length, 2U + role_length) != 0) ||
		(encrypted_password_length != 0U &&
			event_add_size(&dpb_length, 2U + encrypted_password_length) != 0) ||
		(system_encryption_password_length != 0U &&
			event_add_size(&dpb_length, 2U + system_encryption_password_length) != 0) ||
		(connect_timeout != 0U && event_add_size(&dpb_length, 2U + 4U) != 0) ||
		dpb_length > (size_t) SHRT_MAX) {
		return event_fail(error, "event connection parameter block is too long");
	}

	dpb = (unsigned char *) event_malloc(dpb_length, IB_EVENT_TEST_ALLOC_DPB);
	if (dpb == NULL) {
		return event_fail(error, "out of memory allocating event connection parameters");
	}
	offset = 0U;
	dpb[offset++] = isc_dpb_version1;
	event_dpb_add_string(dpb, &offset, isc_dpb_user_name, user, user_length);
	event_dpb_add_string(dpb, &offset, isc_dpb_password, password, password_length);
	if (role_length != 0U) {
		event_dpb_add_string(dpb, &offset, isc_dpb_sql_role_name, role, role_length);
	}
	event_dpb_add_byte(dpb, &offset, isc_dpb_sql_dialect, (unsigned char) dialect);
	event_dpb_add_string(dpb, &offset, isc_dpb_lc_ctype, charset, charset_length);
	if (encrypted_password_length != 0U) {
		event_dpb_add_string(dpb, &offset, isc_dpb_password_enc,
			encrypted_password, encrypted_password_length);
	}
	if (system_encryption_password_length != 0U) {
		event_dpb_add_string(dpb, &offset, isc_dpb_sys_encrypt_password,
			system_encryption_password, system_encryption_password_length);
	}
	if (connect_timeout != 0U) {
		event_dpb_add_integer(dpb, &offset, isc_dpb_connect_timeout, connect_timeout);
	}
	*dpb_result = dpb;
	*dpb_length_result = offset;
	return 0;
}

static ISC_LONG event_block_for_names(char **event_buffer, char **result_buffer,
	char **names, size_t count)
{
	switch (count) {
	case 1U:
		return isc_event_block(event_buffer, result_buffer, 1U, names[0]);
	case 2U:
		return isc_event_block(event_buffer, result_buffer, 2U,
			names[0], names[1]);
	case 3U:
		return isc_event_block(event_buffer, result_buffer, 3U,
			names[0], names[1], names[2]);
	case 4U:
		return isc_event_block(event_buffer, result_buffer, 4U,
			names[0], names[1], names[2], names[3]);
	case 5U:
		return isc_event_block(event_buffer, result_buffer, 5U,
			names[0], names[1], names[2], names[3], names[4]);
	case 6U:
		return isc_event_block(event_buffer, result_buffer, 6U,
			names[0], names[1], names[2], names[3], names[4], names[5]);
	case 7U:
		return isc_event_block(event_buffer, result_buffer, 7U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6]);
	case 8U:
		return isc_event_block(event_buffer, result_buffer, 8U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7]);
	case 9U:
		return isc_event_block(event_buffer, result_buffer, 9U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8]);
	case 10U:
		return isc_event_block(event_buffer, result_buffer, 10U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8], names[9]);
	case 11U:
		return isc_event_block(event_buffer, result_buffer, 11U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8], names[9], names[10]);
	case 12U:
		return isc_event_block(event_buffer, result_buffer, 12U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8], names[9], names[10], names[11]);
	case 13U:
		return isc_event_block(event_buffer, result_buffer, 13U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8], names[9], names[10], names[11],
			names[12]);
	case 14U:
		return isc_event_block(event_buffer, result_buffer, 14U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8], names[9], names[10], names[11],
			names[12], names[13]);
	case 15U:
		return isc_event_block(event_buffer, result_buffer, 15U,
			names[0], names[1], names[2], names[3], names[4], names[5],
			names[6], names[7], names[8], names[9], names[10], names[11],
			names[12], names[13], names[14]);
	default:
		return 0;
	}
}

static int event_initialize_blocks(ib_event_subscription *subscription,
	char **error)
{
	size_t block_index;
	size_t name_offset;
	size_t block_event_count;
	size_t block_count;
	ISC_LONG buffer_length;
	uintptr_t gate_token = 0U;

	if (subscription == NULL || subscription->event_name_count == 0U) {
		return event_fail(error, "event block configuration is invalid");
	}
	block_count = (subscription->event_name_count + 14U) / 15U;
	subscription->blocks = (ib_event_block *) event_calloc(block_count,
		sizeof(*subscription->blocks), IB_EVENT_TEST_ALLOC_BLOCKS);
	if (subscription->blocks == NULL) {
		return event_fail(error, "out of memory allocating event blocks");
	}
	subscription->block_count = block_count;
	subscription->pending_counts = (uint64_t *) event_calloc(
		subscription->event_name_count, sizeof(*subscription->pending_counts),
		IB_EVENT_TEST_ALLOC_PENDING);
	if (subscription->pending_counts == NULL) {
		return event_fail(error, "out of memory allocating pending event counts");
	}
	for (block_index = 0U; block_index < subscription->block_count; block_index++) {
		name_offset = block_index * 15U;
		block_event_count = subscription->event_name_count - name_offset;
		if (block_event_count > 15U) {
			block_event_count = 15U;
		}
		subscription->blocks[block_index].subscription = subscription;
		subscription->blocks[block_index].event_count = block_event_count;
		subscription->blocks[block_index].name_offset = name_offset;
		subscription->blocks[block_index].first_callback = 1;
		subscription->blocks[block_index].native_counts =
			(ISC_ULONG *) event_calloc(block_event_count,
				sizeof(*subscription->blocks[block_index].native_counts),
				IB_EVENT_TEST_ALLOC_NATIVE_COUNTS);
		if (subscription->blocks[block_index].native_counts == NULL) {
			return event_fail(error, "out of memory allocating native event counters");
		}
		if (event_gate_enter(&gate_token, error) != 0) {
			return -1;
		}
		buffer_length = event_block_for_names(
			&subscription->blocks[block_index].event_buffer,
			&subscription->blocks[block_index].result_buffer,
			subscription->event_names + name_offset, block_event_count);
		event_gate_leave(&gate_token);
		if (buffer_length <= 0 ||
			(size_t) buffer_length > IB_EVENT_MAX_BUFFER_LENGTH ||
			(size_t) buffer_length > (size_t) USHRT_MAX) {
			return event_fail(error, "event block is empty or too large");
		}
		subscription->blocks[block_index].buffer_length =
			(ISC_USHORT) buffer_length;
	}
	return 0;
}

static int event_register_blocks(ib_event_subscription *subscription, char **error)
{
	size_t index;

	if (subscription == NULL || subscription->blocks == NULL ||
		subscription->block_count == 0U) {
		return event_fail(error, "event callback blocks are unavailable");
	}
	for (index = 0U; index < subscription->block_count; index++) {
		if (event_registry_register_block(&subscription->blocks[index], error) != 0) {
			event_registry_unregister_subscription(subscription);
			return -1;
		}
	}
	return 0;
}

static int event_open_pipe(ib_event_subscription *subscription, char **error)
{
	int descriptors[2];

	if (pipe(descriptors) != 0) {
		return event_fail(error, "create event notification pipe failed");
	}
	if (event_set_nonblocking(descriptors[0]) != 0 ||
		event_set_nonblocking(descriptors[1]) != 0) {
		event_close_fd(&descriptors[0]);
		event_close_fd(&descriptors[1]);
		return event_fail(error, "configure event notification pipe failed");
	}
	(void) fcntl(descriptors[0], F_SETFD, FD_CLOEXEC);
	(void) fcntl(descriptors[1], F_SETFD, FD_CLOEXEC);
	subscription->notify_read = descriptors[0];
	subscription->notify_write = descriptors[1];
	return 0;
}

static void event_quarantine_subscription(ib_event_subscription *subscription)
{
	if (subscription == NULL) {
		return;
	}
	(void) pthread_mutex_lock(&event_registry_mutex);
	if (!subscription->quarantined) {
		subscription->quarantine_next = event_quarantine_head;
		event_quarantine_head = subscription;
		subscription->quarantined = 1;
	}
	(void) pthread_mutex_unlock(&event_registry_mutex);
}

static int event_detach_database(ib_event_subscription *subscription, char **error)
{
	ISC_STATUS status[IB_EVENT_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	uintptr_t gate_token = 0U;

	if (subscription == NULL || !subscription->database_attached) {
		return 0;
	}
	if (event_gate_enter_exclusive(&gate_token, error) != 0) {
		return -1;
	}
	if (subscription->test_mode) {
		if (event_test_consume_failure(&subscription->test_detach_failures)) {
			event_gate_leave_exclusive(&gate_token);
			return event_fail(error, "detach test event database failed");
		}
		subscription->database = NULL;
		subscription->database_attached = 0;
		event_gate_leave_exclusive(&gate_token);
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_detach_database(status, &subscription->database);
	event_gate_leave_exclusive(&gate_token);
	if (result != 0) {
		return event_fail_status(error, "detach event database", status);
	}
	subscription->database_attached = 0;
	return 0;
}

static void event_release_subscription(ib_event_subscription *subscription)
{
	if (subscription == NULL) {
		return;
	}
	event_registry_unregister_subscription(subscription);
	event_release_block(subscription);
	event_release_names(subscription);
	event_close_fd(&subscription->notify_read);
	event_close_fd(&subscription->notify_write);
	(void) pthread_cond_destroy(&subscription->callback_done);
	(void) pthread_mutex_destroy(&subscription->mutex);
	free(subscription);
}

static int event_cleanup_unattached(ib_event_subscription *subscription,
	char **error)
{
	char *first_error = NULL;
	char *stop_error = NULL;
	char *detach_error = NULL;
	int cleanup_failed = 0;

	if (subscription == NULL) {
		return 0;
	}
	if (ib_event_stop(subscription, &stop_error) != 0) {
		cleanup_failed = 1;
		event_append_error(&first_error, stop_error);
	} else if (event_detach_database(subscription, &detach_error) != 0) {
		cleanup_failed = 1;
		event_append_error(&first_error, detach_error);
	}
	if (cleanup_failed) {
		/* Preserve every still-owned native handle for an explicit retry. */
		event_quarantine_subscription(subscription);
		event_append_error(error, first_error);
		return -1;
	}
	event_release_subscription(subscription);
	return 0;
}

ib_event_subscription *ib_event_open(
	const char *database, size_t database_length,
	const char *user, size_t user_length,
	const char *password, size_t password_length,
	const char *role, size_t role_length,
	const char *encrypted_password, size_t encrypted_password_length,
	const char *system_encryption_password,
	size_t system_encryption_password_length,
	const char *charset, size_t charset_length, int dialect,
	uint32_t connect_timeout,
	const char *const *event_names, size_t event_name_count,
	char **error)
{
	ib_event_subscription *subscription;
	unsigned char *dpb;
	size_t dpb_length;
	size_t index;
	const char *current_name;
	size_t name_length;
	ISC_STATUS status[IB_EVENT_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	uintptr_t gate_token = 0U;

	if (error != NULL) {
		*error = NULL;
	}
	if (database == NULL || database_length == 0U ||
		database_length > (size_t) SHRT_MAX) {
		event_fail(error, "event database name is empty or too long");
		return NULL;
	}
	if (user == NULL || user_length == 0U || user_length > (size_t) UCHAR_MAX ||
		password == NULL || password_length > (size_t) UCHAR_MAX ||
		(role_length != 0U && role == NULL) || role_length > (size_t) UCHAR_MAX ||
		(encrypted_password_length != 0U && encrypted_password == NULL) ||
		encrypted_password_length > (size_t) UCHAR_MAX ||
		(system_encryption_password_length != 0U &&
			system_encryption_password == NULL) ||
		system_encryption_password_length > (size_t) UCHAR_MAX ||
		charset == NULL || charset_length == 0U ||
		charset_length > (size_t) UCHAR_MAX) {
		event_fail(error, "event connection credential length is invalid");
		return NULL;
	}
	if (dialect != SQL_DIALECT_V5 && dialect != SQL_DIALECT_V6) {
		event_fail(error, "event connection SQL dialect is unsupported");
		return NULL;
	}
	if (event_names == NULL || event_name_count == 0U ||
		event_name_count > (size_t) USHRT_MAX) {
		event_fail(error, "event name count is invalid");
		return NULL;
	}
	for (index = 0U; index < event_name_count; index++) {
		current_name = event_names[index];
		if (current_name == NULL) {
			event_fail(error, "event name is unavailable");
			return NULL;
		}
		name_length = strnlen(current_name, IB_EVENT_MAX_NAME_LENGTH + 1U);
		if (name_length == 0U || name_length > IB_EVENT_MAX_NAME_LENGTH) {
			event_fail(error, "event name is empty or too long");
			return NULL;
		}
	}

	subscription = (ib_event_subscription *) event_calloc(1U,
		sizeof(*subscription), IB_EVENT_TEST_ALLOC_SUBSCRIPTION);
	if (subscription == NULL) {
		event_fail(error, "out of memory allocating event subscription");
		return NULL;
	}
	subscription->notify_read = -1;
	subscription->notify_write = -1;
	if (pthread_mutex_init(&subscription->mutex, NULL) != 0) {
		event_fail(error, "initialize event synchronization failed");
		free(subscription);
		return NULL;
	}
	if (pthread_cond_init(&subscription->callback_done, NULL) != 0) {
		event_fail(error, "initialize event synchronization failed");
		(void) pthread_mutex_destroy(&subscription->mutex);
		free(subscription);
		return NULL;
	}
	atomic_init(&subscription->callbacks, 0U);
	atomic_init(&subscription->closing, 0);
	atomic_init(&subscription->test_pause_callback_release, 0);
	atomic_init(&subscription->test_pause_callback_rearm, 0);
	if (event_open_pipe(subscription, error) != 0) {
		event_cleanup_unattached(subscription, NULL);
		return NULL;
	}

	subscription->event_names = (char **) event_calloc(event_name_count,
		sizeof(*subscription->event_names), IB_EVENT_TEST_ALLOC_NAMES);
	if (subscription->event_names == NULL) {
		event_fail(error, "out of memory allocating event names");
		event_cleanup_unattached(subscription, NULL);
		return NULL;
	}
	subscription->event_name_count = event_name_count;
	for (index = 0U; index < event_name_count; index++) {
		name_length = strlen(event_names[index]);
		subscription->event_names[index] = event_copy_name(event_names[index], name_length);
		if (subscription->event_names[index] == NULL) {
			event_fail(error, "out of memory copying event name");
			event_cleanup_unattached(subscription, NULL);
			return NULL;
		}
	}

	if (event_build_dpb(user, user_length, password, password_length, role,
		role_length, encrypted_password, encrypted_password_length,
		system_encryption_password, system_encryption_password_length, charset,
		charset_length, dialect, connect_timeout, &dpb, &dpb_length, error) != 0) {
		event_cleanup_unattached(subscription, NULL);
		return NULL;
	}
	if (event_gate_enter(&gate_token, error) != 0) {
		free(dpb);
		event_cleanup_unattached(subscription, NULL);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	result = isc_attach_database(status, (short) database_length, (char *) database,
		&subscription->database, (short) dpb_length, (char *) dpb);
	event_gate_leave(&gate_token);
	free(dpb);
	if (result != 0) {
		if (subscription->database != NULL) {
			subscription->database_attached = 1;
		}
		event_fail_status(error, "attach event database", status);
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	subscription->database_attached = 1;

	if (event_initialize_blocks(subscription, error) != 0) {
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	if (event_register_blocks(subscription, error) != 0) {
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	for (index = 0U; index < subscription->block_count; index++) {
		if (event_queue_block(subscription, &subscription->blocks[index], error) != 0) {
			event_cleanup_unattached(subscription, error);
			return NULL;
		}
	}
	return subscription;
}

int ib_event_wait(ib_event_subscription *subscription, int timeout_ms,
	int *ready, char **error)
{
	struct pollfd descriptor;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || ready == NULL || timeout_ms < 0) {
		return event_fail(error, "event wait arguments are invalid");
	}
	*ready = 0;
	if (atomic_load_explicit(&subscription->closing, memory_order_acquire) != 0) {
		*ready = 1;
		return 0;
	}
	descriptor.fd = subscription->notify_read;
	descriptor.events = POLLIN;
	descriptor.revents = 0;
	do {
		result = poll(&descriptor, 1U, timeout_ms);
	} while (result < 0 && errno == EINTR);
	if (result < 0) {
		return event_fail(error, "wait for event notification failed");
	}
	if (result > 0 && (descriptor.revents & (POLLIN | POLLERR | POLLHUP)) != 0) {
		*ready = 1;
	}
	return 0;
}

int ib_event_ready(ib_event_subscription *subscription, int timeout_ms,
	int *ready, char **error)
{
	struct timespec deadline;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || ready == NULL || timeout_ms < 0) {
		return event_fail(error, "event readiness arguments are invalid");
	}
	*ready = 0;
	if (clock_gettime(CLOCK_REALTIME, &deadline) != 0) {
		return event_fail(error, "get event readiness clock failed");
	}
	deadline.tv_sec += timeout_ms / 1000;
	deadline.tv_nsec += (long) (timeout_ms % 1000) * 1000000L;
	if (deadline.tv_nsec >= 1000000000L) {
		deadline.tv_sec++;
		deadline.tv_nsec -= 1000000000L;
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	while (subscription->initial_callbacks < subscription->block_count &&
		subscription->callback_error == NULL &&
		atomic_load_explicit(&subscription->closing, memory_order_acquire) == 0) {
		result = pthread_cond_timedwait(&subscription->callback_done,
			&subscription->mutex, &deadline);
		if (result == ETIMEDOUT) {
			break;
		}
		if (result != 0) {
			(void) pthread_mutex_unlock(&subscription->mutex);
			return event_fail(error, "wait for event readiness failed");
		}
	}
	if (subscription->callback_error != NULL) {
		char *callback_error = event_copy_string(subscription->callback_error,
			strlen(subscription->callback_error));
		(void) pthread_mutex_unlock(&subscription->mutex);
		if (callback_error == NULL) {
			return event_fail(error, "event readiness callback error unavailable");
		}
		event_give_error(error, callback_error);
		return -1;
	}
	if (subscription->initial_callbacks >= subscription->block_count) {
		*ready = 1;
	}
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_take(ib_event_subscription *subscription, uint64_t *counts,
	size_t count, int *has_counts, char **error)
{
	unsigned char buffer[128];
	ssize_t result;
	size_t index;
	char *callback_error;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || counts == NULL || has_counts == NULL ||
		count != subscription->event_name_count) {
		return event_fail(error, "event counter arguments are invalid");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	callback_error = subscription->callback_error;
	subscription->callback_error = NULL;
	if (callback_error != NULL) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		event_give_error(error, callback_error);
		return -1;
	}
	*has_counts = 0;
	for (index = 0U; index < count; index++) {
		counts[index] = subscription->pending_counts[index];
		if (counts[index] != 0U) {
			*has_counts = 1;
		}
		subscription->pending_counts[index] = 0U;
	}
	do {
		result = read(subscription->notify_read, buffer, sizeof(buffer));
	} while (result > 0);
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

static int event_cancel_database(ib_event_subscription *subscription,
	ISC_LONG event_id, char **error)
{
	ISC_STATUS status[IB_EVENT_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	uintptr_t gate_token = 0U;

	if (!subscription->test_mode &&
		(!subscription->database_attached || subscription->database == NULL)) {
		return event_fail(error, "event database handle ownership is unavailable");
	}
	if (event_gate_enter_exclusive(&gate_token, error) != 0) {
		return -1;
	}
	if (subscription->test_mode) {
		if (event_test_consume_failure(&subscription->test_cancel_failures)) {
			event_gate_leave_exclusive(&gate_token);
			return event_fail(error, "cancel test event request failed");
		}
		event_gate_leave_exclusive(&gate_token);
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_cancel_events(status, &subscription->database, &event_id);
	event_gate_leave_exclusive(&gate_token);
	if (result != 0) {
		return event_fail_status(error, "cancel database events", status);
	}
	return 0;
}

static int event_has_queue_in_progress_locked(ib_event_subscription *subscription)
{
	size_t index;

	if (subscription == NULL || subscription->blocks == NULL) {
		return 0;
	}
	for (index = 0U; index < subscription->block_count; index++) {
		if (subscription->blocks[index].queue_in_progress) {
			return 1;
		}
	}
	return 0;
}

static int event_has_unresolved_requests_locked(ib_event_subscription *subscription)
{
	size_t index;

	if (subscription == NULL || subscription->blocks == NULL) {
		return 0;
	}
	for (index = 0U; index < subscription->block_count; index++) {
		if (subscription->blocks[index].queued ||
			subscription->blocks[index].cancel_pending ||
			subscription->blocks[index].queue_in_progress) {
			return 1;
		}
	}
	return 0;
}

int ib_event_stop(ib_event_subscription *subscription, char **error)
{
	char *first_error = NULL;
	char *cancel_error;
	size_t index;
	ISC_LONG event_id;
	int unresolved;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL) {
		return 0;
	}
	atomic_store_explicit(&subscription->closing, 1, memory_order_release);
	if (subscription->test_mode) {
		event_test_note_stop(subscription);
	}
	/*
	 * Stop admission first.  A callback that already acquired the registry
	 * reference is counted and can finish; a callback arriving after this
	 * point cannot find a registered block.  Do not acquire the process-wide
	 * exclusive gate while waiting: callbacks may need the ordinary gate to
	 * finish their counter processing or re-arm handoff.
	 */
	event_registry_unregister_subscription(subscription);
	(void) event_signal(subscription);
	(void) pthread_cond_broadcast(&subscription->callback_done);
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->stop_done) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		if (subscription->test_mode) {
			event_test_note_stop_done(subscription);
		}
		return 0;
	}
	while (atomic_load_explicit(&subscription->callbacks, memory_order_acquire) != 0U ||
		event_has_queue_in_progress_locked(subscription)) {
		(void) pthread_cond_wait(&subscription->callback_done, &subscription->mutex);
	}
	(void) pthread_mutex_unlock(&subscription->mutex);
	for (index = 0U; index < subscription->block_count; index++) {
		(void) pthread_mutex_lock(&subscription->mutex);
		if (subscription->blocks[index].cancel_pending) {
			event_id = subscription->blocks[index].cancel_event_id;
		} else if (subscription->blocks[index].queued) {
			event_id = subscription->blocks[index].event_id;
			subscription->blocks[index].cancel_pending = 1;
			subscription->blocks[index].cancel_event_id = event_id;
		} else {
			(void) pthread_mutex_unlock(&subscription->mutex);
			continue;
		}
		(void) pthread_mutex_unlock(&subscription->mutex);
		cancel_error = NULL;
		if (event_cancel_database(subscription, event_id, &cancel_error) != 0) {
			(void) pthread_mutex_lock(&subscription->mutex);
			/* A failed native call does not prove that the request is gone. */
			subscription->blocks[index].queued = 1;
			(void) pthread_cond_broadcast(&subscription->callback_done);
			(void) pthread_mutex_unlock(&subscription->mutex);
			event_append_error(&first_error, cancel_error);
			continue;
		}
		(void) pthread_mutex_lock(&subscription->mutex);
		subscription->blocks[index].queued = 0;
		subscription->blocks[index].cancel_pending = 0;
		(void) pthread_cond_broadcast(&subscription->callback_done);
		(void) pthread_mutex_unlock(&subscription->mutex);
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	while (atomic_load_explicit(&subscription->callbacks, memory_order_acquire) != 0U) {
		(void) pthread_cond_wait(&subscription->callback_done, &subscription->mutex);
	}
	while (event_has_queue_in_progress_locked(subscription)) {
		(void) pthread_cond_wait(&subscription->callback_done, &subscription->mutex);
	}
	unresolved = event_has_unresolved_requests_locked(subscription);
	subscription->stop_done = !unresolved && first_error == NULL;
	if (subscription->test_mode) {
		event_test_note_stop_done(subscription);
	}
	(void) pthread_mutex_unlock(&subscription->mutex);
	if (first_error != NULL || unresolved) {
		if (first_error == NULL) {
			first_error = event_copy_string(
				"event request ownership could not be established",
				sizeof("event request ownership could not be established") - 1U);
		}
		event_give_error(error, first_error);
		return -1;
	}
	return 0;
}

int ib_event_destroy(ib_event_subscription *subscription, char **error)
{
	char *stop_error = NULL;
	char *detach_error = NULL;
	char *callback_error;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL) {
		return 0;
	}
	if (ib_event_stop(subscription, &stop_error) != 0) {
		event_give_error(error, stop_error);
		return -1;
	}
	if (event_detach_database(subscription, &detach_error) != 0) {
		event_give_error(error, detach_error);
		return -1;
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	callback_error = subscription->callback_error;
	subscription->callback_error = NULL;
	(void) pthread_mutex_unlock(&subscription->mutex);
	free(callback_error);
	event_release_subscription(subscription);
	return 0;
}

void ib_event_error_free(char *error)
{
	free(error);
}

int ib_event_test_build_dpb(uint32_t connect_timeout, unsigned char *buffer,
	size_t buffer_length, size_t *encoded_length, char **error)
{
	unsigned char *dpb = NULL;
	size_t dpb_length = 0U;

	if (error != NULL) {
		*error = NULL;
	}
	if (buffer == NULL || encoded_length == NULL) {
		return event_fail(error, "event test DPB output storage is unavailable");
	}
	*encoded_length = 0U;
	if (event_build_dpb("SYSDBA", 6U, "", 0U, NULL, 0U, NULL, 0U,
		NULL, 0U, "UTF8", 5U, SQL_DIALECT_V6, connect_timeout,
		&dpb, &dpb_length, error) != 0) {
		return -1;
	}
	if (dpb_length > buffer_length) {
		free(dpb);
		return event_fail(error, "event test DPB output buffer is too small");
	}
	memcpy(buffer, dpb, dpb_length);
	*encoded_length = dpb_length;
	free(dpb);
	return 0;
}

ib_event_subscription *ib_event_test_new(const char *const *event_names,
	size_t event_name_count, char **error)
{
	ib_event_subscription *subscription;
	size_t index;
	size_t name_length;

	if (error != NULL) {
		*error = NULL;
	}
	if (event_names == NULL || event_name_count == 0U ||
		event_name_count > (size_t) USHRT_MAX) {
		(void) event_fail(error, "event test name count is invalid");
		return NULL;
	}
	subscription = (ib_event_subscription *) event_calloc(1U,
		sizeof(*subscription), IB_EVENT_TEST_ALLOC_SUBSCRIPTION);
	if (subscription == NULL) {
		(void) event_fail(error, "out of memory allocating event test subscription");
		return NULL;
	}
	subscription->notify_read = -1;
	subscription->notify_write = -1;
	if (pthread_mutex_init(&subscription->mutex, NULL) != 0) {
		(void) event_fail(error, "initialize event test synchronization failed");
		free(subscription);
		return NULL;
	}
	if (pthread_cond_init(&subscription->callback_done, NULL) != 0) {
		(void) event_fail(error, "initialize event test synchronization failed");
		(void) pthread_mutex_destroy(&subscription->mutex);
		free(subscription);
		return NULL;
	}
	atomic_init(&subscription->callbacks, 0U);
	atomic_init(&subscription->closing, 0);
	atomic_init(&subscription->test_cancel_failures, 0U);
	atomic_init(&subscription->test_detach_failures, 0U);
	atomic_init(&subscription->test_rearm_failures, 0U);
	atomic_init(&subscription->test_sync_queue_callbacks, 0U);
	atomic_init(&subscription->test_pause_callback_release, 0);
	atomic_init(&subscription->test_pause_callback_rearm, 0);
	subscription->test_mode = 1;
	subscription->database = (isc_db_handle) (uintptr_t) 1U;
	subscription->database_attached = 1;
	if (event_open_pipe(subscription, error) != 0) {
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	subscription->event_names = (char **) event_calloc(event_name_count,
		sizeof(*subscription->event_names), IB_EVENT_TEST_ALLOC_NAMES);
	if (subscription->event_names == NULL) {
		(void) event_fail(error, "out of memory allocating event test names");
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	subscription->event_name_count = event_name_count;
	for (index = 0U; index < event_name_count; index++) {
		if (event_names[index] == NULL) {
			(void) event_fail(error, "event test name is unavailable");
			event_cleanup_unattached(subscription, error);
			return NULL;
		}
		name_length = strnlen(event_names[index], IB_EVENT_MAX_NAME_LENGTH + 1U);
		if (name_length == 0U || name_length > IB_EVENT_MAX_NAME_LENGTH) {
			(void) event_fail(error, "event test name is empty or too long");
			event_cleanup_unattached(subscription, error);
			return NULL;
		}
		subscription->event_names[index] = event_copy_name(event_names[index], name_length);
		if (subscription->event_names[index] == NULL) {
			(void) event_fail(error, "out of memory copying event test name");
			event_cleanup_unattached(subscription, error);
			return NULL;
		}
	}
	if (event_initialize_blocks(subscription, error) != 0) {
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	if (event_register_blocks(subscription, error) != 0) {
		event_cleanup_unattached(subscription, error);
		return NULL;
	}
	for (index = 0U; index < subscription->block_count; index++) {
		if (event_queue_block(subscription, &subscription->blocks[index], error) != 0) {
			event_cleanup_unattached(subscription, error);
			return NULL;
		}
	}
	return subscription;
}

int ib_event_test_emit(ib_event_subscription *subscription,
	const char *updated, short length, char **error)
{
	uintptr_t token;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->block_count == 0U) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test block is unavailable");
	}
	token = subscription->blocks[0].callback_token;
	(void) pthread_mutex_unlock(&subscription->mutex);
	event_callback((void *) token, length, (char *) updated);
	return 0;
}

int ib_event_test_emit_counts(ib_event_subscription *subscription,
	size_t block_index, const uint64_t *counts, size_t count, char **error)
{
	ib_event_block *block;
	unsigned char *updated;
	size_t position;
	size_t index;
	size_t name_length;
	size_t count_offset;
	size_t byte;
	uint64_t value;
	uintptr_t token;
	short callback_length;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0 ||
		block_index >= subscription->block_count) {
		return event_fail(error, "event test block is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	block = &subscription->blocks[block_index];
	if (counts == NULL || count != block->event_count) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test counter arguments are invalid");
	}
	token = block->callback_token;
	callback_length = (short) block->buffer_length;
	updated = (unsigned char *) event_malloc((size_t) block->buffer_length,
		IB_EVENT_TEST_ALLOC_EVENT_RESULT);
	if (updated == NULL) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "out of memory allocating event test result");
	}
	memcpy(updated, block->event_buffer, (size_t) block->buffer_length);
	position = 1U;
	for (index = 0U; index < count; index++) {
		if (position >= (size_t) block->buffer_length) {
			free(updated);
			(void) pthread_mutex_unlock(&subscription->mutex);
			return event_fail(error, "event test block layout is invalid");
		}
		name_length = (size_t) updated[position];
		if (position > (size_t) block->buffer_length ||
			(size_t) block->buffer_length - position < 5U ||
			name_length > (size_t) block->buffer_length - position - 5U) {
			free(updated);
			(void) pthread_mutex_unlock(&subscription->mutex);
			return event_fail(error, "event test block layout is invalid");
		}
		count_offset = position + 1U + name_length;
		value = counts[index];
		if (value > UINT32_MAX) {
			free(updated);
			(void) pthread_mutex_unlock(&subscription->mutex);
			return event_fail(error, "event test counter is too large");
		}
		for (byte = 0U; byte < 4U; byte++) {
			updated[count_offset + byte] =
				(unsigned char) ((value >> (8U * byte)) & 0xffU);
		}
		position = count_offset + 4U;
	}
	(void) pthread_mutex_unlock(&subscription->mutex);
	event_callback((void *) token, callback_length, (char *) updated);
	free(updated);
	return 0;
}

int ib_event_test_hold_callback(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	subscription->hold_callback = 1;
	subscription->callback_entered = 0;
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_test_wait_callback(ib_event_subscription *subscription,
	int timeout_ms, int *entered, char **error)
{
	struct timespec deadline;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || entered == NULL || timeout_ms < 0 ||
		subscription->test_mode == 0) {
		return event_fail(error, "event test callback wait arguments are invalid");
	}
	if (clock_gettime(CLOCK_REALTIME, &deadline) != 0) {
		return event_fail(error, "get event test callback clock failed");
	}
	deadline.tv_sec += timeout_ms / 1000;
	deadline.tv_nsec += (long) (timeout_ms % 1000) * 1000000L;
	if (deadline.tv_nsec >= 1000000000L) {
		deadline.tv_sec++;
		deadline.tv_nsec -= 1000000000L;
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	while (!subscription->callback_entered) {
		result = pthread_cond_timedwait(&subscription->callback_done,
			&subscription->mutex, &deadline);
		if (result == ETIMEDOUT) {
			*entered = 0;
			(void) pthread_mutex_unlock(&subscription->mutex);
			return 0;
		}
		if (result != 0) {
			(void) pthread_mutex_unlock(&subscription->mutex);
			return event_fail(error, "wait for event test callback failed");
		}
	}
	*entered = 1;
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_test_release_callback(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	subscription->hold_callback = 0;
	(void) pthread_cond_broadcast(&subscription->callback_done);
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_test_pause_callback_rearm(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	if (event_test_activate_gate(&event_callback_rearm_test, subscription, error,
		"event test callback rearm is already active") != 0) {
		return -1;
	}
	atomic_store_explicit(&subscription->test_pause_callback_rearm, 1,
		memory_order_release);
	return 0;
}

int ib_event_test_wait_callback_rearm(int timeout_ms, int *entered,
	char **error)
{
	return event_test_wait_gate(&event_callback_rearm_test, timeout_ms, entered,
		error, "event test callback rearm is inactive",
		"get event test callback rearm clock failed",
		"wait for event test callback rearm failed");
}

int ib_event_test_release_callback_rearm(char **error)
{
	return event_test_release_gate(&event_callback_rearm_test, error,
		"event test callback rearm is inactive");
}

int ib_event_test_hold_queue(ib_event_subscription *subscription, char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	return event_test_activate_gate(&event_queue_test, subscription, error,
		"event test queue gate is already active");
}

int ib_event_test_wait_queue(int timeout_ms, int *entered, char **error)
{
	return event_test_wait_gate(&event_queue_test, timeout_ms, entered, error,
		"event test queue gate is inactive", "get event test queue clock failed",
		"wait for event test queue failed");
}

int ib_event_test_release_queue(char **error)
{
	return event_test_release_gate(&event_queue_test, error,
		"event test queue gate is inactive");
}

int ib_event_test_start_async_baseline(ib_event_subscription *subscription,
	char **error)
{
	ib_event_block *block;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->blocks == NULL || subscription->block_count == 0U) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test callback block is unavailable");
	}
	block = &subscription->blocks[0];
	(void) pthread_mutex_lock(&event_async_callback_test.mutex);
	if (event_async_callback_test.active) {
		(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test asynchronous callback is already active");
	}
	event_async_callback_test.subscription = subscription;
	event_async_callback_test.token = block->callback_token;
	event_async_callback_test.updated = block->result_buffer;
	event_async_callback_test.length = (short) block->buffer_length;
	event_async_callback_test.active = 1;
	result = pthread_create(&event_async_callback_test.thread, NULL,
		event_test_async_baseline_runner, NULL);
	if (result != 0) {
		event_async_callback_test.active = 0;
		event_async_callback_test.subscription = NULL;
		event_async_callback_test.updated = NULL;
		(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "start event test asynchronous callback failed");
	}
	(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_test_join_async_baseline(char **error)
{
	pthread_t thread;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	(void) pthread_mutex_lock(&event_async_callback_test.mutex);
	if (!event_async_callback_test.active) {
		(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
		return event_fail(error, "event test asynchronous callback is inactive");
	}
	thread = event_async_callback_test.thread;
	(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
	result = pthread_join(thread, NULL);
	if (result != 0) {
		return event_fail(error, "join event test asynchronous callback failed");
	}
	(void) pthread_mutex_lock(&event_async_callback_test.mutex);
	event_async_callback_test.active = 0;
	event_async_callback_test.subscription = NULL;
	event_async_callback_test.updated = NULL;
	(void) pthread_mutex_unlock(&event_async_callback_test.mutex);
	return 0;
}

int ib_event_test_queue(ib_event_subscription *subscription, char **error)
{
	ib_event_block *block;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->blocks == NULL || subscription->block_count == 0U) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test queue block is unavailable");
	}
	block = &subscription->blocks[0];
	(void) pthread_mutex_unlock(&subscription->mutex);
	return event_queue_block(subscription, block, error);
}

int ib_event_test_queue_state(ib_event_subscription *subscription,
	size_t block_index, unsigned int *queue_count,
	unsigned int *outstanding_count, uint64_t *callback_token,
	uint64_t *event_id, char **error)
{
	ib_event_block *block;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0 ||
		queue_count == NULL || outstanding_count == NULL ||
		callback_token == NULL || event_id == NULL) {
		return event_fail(error, "event test queue state arguments are invalid");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->blocks == NULL || block_index >= subscription->block_count) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test queue block is unavailable");
	}
	block = &subscription->blocks[block_index];
	*queue_count = block->test_queue_count;
	*outstanding_count = block->test_outstanding;
	*callback_token = (uint64_t) block->callback_token;
	*event_id = (uint64_t) block->event_id;
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_test_cleanup_unattached(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	return event_cleanup_unattached(subscription, error);
}

int ib_event_test_pause_callback_release(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	if (event_callback_release_test.active) {
		(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
		return event_fail(error, "event test callback release is already active");
	}
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
	event_test_pause_release_gate(subscription);
	return 0;
}

int ib_event_test_wait_callback_release(int timeout_ms, int *entered,
	char **error)
{
	return event_test_wait_release_gate(timeout_ms, entered, 0, error);
}

int ib_event_test_wait_stop(int timeout_ms, int *entered, char **error)
{
	return event_test_wait_release_gate(timeout_ms, entered, 1, error);
}

int ib_event_test_wait_stop_done(int timeout_ms, int *entered, char **error)
{
	return event_test_wait_release_gate(timeout_ms, entered, 2, error);
}

int ib_event_test_release_callback_release(char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	(void) pthread_mutex_lock(&event_callback_release_test.mutex);
	if (!event_callback_release_test.active) {
		(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
		return event_fail(error, "event test callback release is inactive");
	}
	(void) pthread_mutex_unlock(&event_callback_release_test.mutex);
	event_test_release_release_gate();
	return 0;
}

int ib_event_test_enable_rearm(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	subscription->test_rearm = 1;
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

int ib_event_test_sync_next_queue(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) atomic_fetch_add_explicit(&subscription->test_sync_queue_callbacks, 1U,
		memory_order_acq_rel);
	return 0;
}

int ib_event_test_fail_next_cancel(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) atomic_fetch_add_explicit(&subscription->test_cancel_failures, 1U,
		memory_order_acq_rel);
	return 0;
}

int ib_event_test_fail_next_detach(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) atomic_fetch_add_explicit(&subscription->test_detach_failures, 1U,
		memory_order_acq_rel);
	return 0;
}

int ib_event_test_fail_next_rearm(ib_event_subscription *subscription,
	char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) atomic_fetch_add_explicit(&subscription->test_rearm_failures, 1U,
		memory_order_acq_rel);
	return 0;
}

int ib_event_test_fail_next_allocation(int failpoint, char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (failpoint < IB_EVENT_TEST_ALLOC_SUBSCRIPTION ||
		failpoint > IB_EVENT_TEST_ALLOC_ERROR) {
		return event_fail(error, "event allocation failpoint is invalid");
	}
	atomic_store_explicit(&event_test_allocation_failpoint, failpoint,
		memory_order_release);
	return 0;
}

int ib_event_test_set_pending_count(ib_event_subscription *subscription,
	size_t block_index, size_t count_index, uint64_t value, char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->blocks == NULL || block_index >= subscription->block_count ||
		count_index >= subscription->blocks[block_index].event_count) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test counter index is invalid");
	}
	subscription->pending_counts[
		subscription->blocks[block_index].name_offset + count_index] = value;
	(void) pthread_mutex_unlock(&subscription->mutex);
	return 0;
}

static void *event_test_pre_entry_runner(void *argument)
{
	uintptr_t token;

	(void) argument;
	(void) pthread_mutex_lock(&event_pre_entry.mutex);
	event_pre_entry.entered = 1;
	(void) pthread_cond_broadcast(&event_pre_entry.condition);
	while (!event_pre_entry.release) {
		(void) pthread_cond_wait(&event_pre_entry.condition, &event_pre_entry.mutex);
	}
	token = event_pre_entry.token;
	(void) pthread_mutex_unlock(&event_pre_entry.mutex);
	/* Only the opaque token crosses the pre-entry lifetime boundary. */
	event_callback((void *) token, 0, NULL);
	return NULL;
}

int ib_event_test_start_pre_entry_callback(ib_event_subscription *subscription,
	char **error)
{
	uintptr_t token;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL || subscription->test_mode == 0) {
		return event_fail(error, "event test subscription is unavailable");
	}
	(void) pthread_mutex_lock(&subscription->mutex);
	if (subscription->blocks == NULL || subscription->block_count == 0U) {
		(void) pthread_mutex_unlock(&subscription->mutex);
		return event_fail(error, "event test callback block is unavailable");
	}
	token = subscription->blocks[0].callback_token;
	(void) pthread_mutex_unlock(&subscription->mutex);

	(void) pthread_mutex_lock(&event_pre_entry.mutex);
	if (event_pre_entry.active) {
		(void) pthread_mutex_unlock(&event_pre_entry.mutex);
		return event_fail(error, "event test pre-entry callback is already active");
	}
	event_pre_entry.token = token;
	event_pre_entry.entered = 0;
	event_pre_entry.release = 0;
	result = pthread_create(&event_pre_entry.thread, NULL,
		event_test_pre_entry_runner, NULL);
	if (result != 0) {
		(void) pthread_mutex_unlock(&event_pre_entry.mutex);
		return event_fail(error, "start event test pre-entry callback failed");
	}
	event_pre_entry.active = 1;
	(void) pthread_mutex_unlock(&event_pre_entry.mutex);
	return 0;
}

int ib_event_test_wait_pre_entry_callback(int timeout_ms, int *entered,
	char **error)
{
	struct timespec deadline;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (entered == NULL || timeout_ms < 0) {
		return event_fail(error, "event test pre-entry wait arguments are invalid");
	}
	if (clock_gettime(CLOCK_REALTIME, &deadline) != 0) {
		return event_fail(error, "get event test pre-entry clock failed");
	}
	deadline.tv_sec += timeout_ms / 1000;
	deadline.tv_nsec += (long) (timeout_ms % 1000) * 1000000L;
	if (deadline.tv_nsec >= 1000000000L) {
		deadline.tv_sec++;
		deadline.tv_nsec -= 1000000000L;
	}
	(void) pthread_mutex_lock(&event_pre_entry.mutex);
	while (!event_pre_entry.entered) {
		result = pthread_cond_timedwait(&event_pre_entry.condition,
			&event_pre_entry.mutex, &deadline);
		if (result == ETIMEDOUT) {
			*entered = 0;
			(void) pthread_mutex_unlock(&event_pre_entry.mutex);
			return 0;
		}
		if (result != 0) {
			(void) pthread_mutex_unlock(&event_pre_entry.mutex);
			return event_fail(error, "wait for event test pre-entry callback failed");
		}
	}
	*entered = 1;
	(void) pthread_mutex_unlock(&event_pre_entry.mutex);
	return 0;
}

int ib_event_test_release_pre_entry_callback(char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	(void) pthread_mutex_lock(&event_pre_entry.mutex);
	if (!event_pre_entry.active) {
		(void) pthread_mutex_unlock(&event_pre_entry.mutex);
		return event_fail(error, "event test pre-entry callback is inactive");
	}
	event_pre_entry.release = 1;
	(void) pthread_cond_broadcast(&event_pre_entry.condition);
	(void) pthread_mutex_unlock(&event_pre_entry.mutex);
	return 0;
}

int ib_event_test_join_pre_entry_callback(char **error)
{
	pthread_t thread;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	(void) pthread_mutex_lock(&event_pre_entry.mutex);
	if (!event_pre_entry.active) {
		(void) pthread_mutex_unlock(&event_pre_entry.mutex);
		return event_fail(error, "event test pre-entry callback is inactive");
	}
	thread = event_pre_entry.thread;
	(void) pthread_mutex_unlock(&event_pre_entry.mutex);
	result = pthread_join(thread, NULL);
	if (result != 0) {
		return event_fail(error, "join event test pre-entry callback failed");
	}
	(void) pthread_mutex_lock(&event_pre_entry.mutex);
	event_pre_entry.active = 0;
	(void) pthread_mutex_unlock(&event_pre_entry.mutex);
	return 0;
}

int ib_event_test_set_next_token(uintptr_t next_token, char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (next_token == 0U) {
		return event_fail(error, "event callback token is zero");
	}
	(void) pthread_mutex_lock(&event_registry_mutex);
	if (event_registry_head != NULL || next_token <= event_last_token) {
		(void) pthread_mutex_unlock(&event_registry_mutex);
		return event_fail(error, "event callback token counter cannot move backward");
	}
	event_next_token = next_token;
	event_token_exhausted = 0;
	(void) pthread_mutex_unlock(&event_registry_mutex);
	return 0;
}

int ib_event_quarantine(ib_event_subscription *subscription, char **error)
{
	if (error != NULL) {
		*error = NULL;
	}
	if (subscription == NULL) {
		return event_fail(error, "event subscription is unavailable");
	}
	event_quarantine_subscription(subscription);
	return 0;
}
