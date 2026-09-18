#ifndef INTERBASE_GO_EVENTS_NATIVE_H
#define INTERBASE_GO_EVENTS_NATIVE_H

#include <stddef.h>
#include <stdint.h>

typedef struct ib_event_subscription ib_event_subscription;

/* Shared process-wide native gate bridge.  Each successful enter returns an
 * opaque token that must be released exactly once. Go provides these symbols
 * for the package build; the standalone native event harness supplies
 * equivalent reader/writer stubs. */
uintptr_t ib_event_gate_enter(void);
void ib_event_gate_leave(uintptr_t token);
uintptr_t ib_event_gate_enter_exclusive(void);
void ib_event_gate_leave_exclusive(uintptr_t token);

#define IB_EVENT_TEST_ALLOC_SUBSCRIPTION 1
#define IB_EVENT_TEST_ALLOC_NAMES 2
#define IB_EVENT_TEST_ALLOC_NAME 3
#define IB_EVENT_TEST_ALLOC_DPB 4
#define IB_EVENT_TEST_ALLOC_BLOCKS 5
#define IB_EVENT_TEST_ALLOC_PENDING 6
#define IB_EVENT_TEST_ALLOC_NATIVE_COUNTS 7
#define IB_EVENT_TEST_ALLOC_EVENT_RESULT 8
#define IB_EVENT_TEST_ALLOC_ERROR 9

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
	char **error);

int ib_event_wait(ib_event_subscription *subscription, int timeout_ms,
	int *ready, char **error);
int ib_event_ready(ib_event_subscription *subscription, int timeout_ms,
	int *ready, char **error);
int ib_event_take(ib_event_subscription *subscription, uint64_t *counts,
	size_t count, int *has_counts, char **error);
int ib_event_stop(ib_event_subscription *subscription, char **error);
int ib_event_destroy(ib_event_subscription *subscription, char **error);
int ib_event_quarantine(ib_event_subscription *subscription, char **error);
void ib_event_error_free(char *error);

int ib_event_test_build_dpb(uint32_t connect_timeout, unsigned char *buffer,
	size_t buffer_length, size_t *encoded_length, char **error);

/* Deterministic callback harness used by the native lifecycle tests. */
ib_event_subscription *ib_event_test_new(const char *const *event_names,
	size_t event_name_count, char **error);
int ib_event_test_emit(ib_event_subscription *subscription,
	const char *updated, short length, char **error);
int ib_event_test_emit_counts(ib_event_subscription *subscription,
	size_t block_index, const uint64_t *counts, size_t count, char **error);
int ib_event_test_hold_callback(ib_event_subscription *subscription,
	char **error);
int ib_event_test_wait_callback(ib_event_subscription *subscription,
	int timeout_ms, int *entered, char **error);
int ib_event_test_release_callback(ib_event_subscription *subscription,
	char **error);
int ib_event_test_pause_callback_release(ib_event_subscription *subscription,
	char **error);
int ib_event_test_wait_callback_release(int timeout_ms, int *entered,
	char **error);
int ib_event_test_release_callback_release(char **error);
int ib_event_test_wait_stop(int timeout_ms, int *entered, char **error);
int ib_event_test_wait_stop_done(int timeout_ms, int *entered, char **error);
int ib_event_test_pause_callback_rearm(ib_event_subscription *subscription,
	char **error);
int ib_event_test_wait_callback_rearm(int timeout_ms, int *entered,
	char **error);
int ib_event_test_release_callback_rearm(char **error);
int ib_event_test_hold_queue(ib_event_subscription *subscription,
	char **error);
int ib_event_test_wait_queue(int timeout_ms, int *entered, char **error);
int ib_event_test_release_queue(char **error);
int ib_event_test_start_async_baseline(ib_event_subscription *subscription,
	char **error);
int ib_event_test_join_async_baseline(char **error);
int ib_event_test_queue(ib_event_subscription *subscription, char **error);
int ib_event_test_queue_state(ib_event_subscription *subscription,
	size_t block_index, unsigned int *queue_count,
	unsigned int *outstanding_count, uint64_t *callback_token,
	uint64_t *event_id, char **error);
int ib_event_test_cleanup_unattached(ib_event_subscription *subscription,
	char **error);
int ib_event_test_enable_rearm(ib_event_subscription *subscription,
	char **error);
int ib_event_test_sync_next_queue(ib_event_subscription *subscription,
	char **error);
int ib_event_test_fail_next_cancel(ib_event_subscription *subscription,
	char **error);
int ib_event_test_fail_next_detach(ib_event_subscription *subscription,
	char **error);
int ib_event_test_fail_next_rearm(ib_event_subscription *subscription,
	char **error);
int ib_event_test_fail_next_allocation(int failpoint, char **error);
int ib_event_test_set_pending_count(ib_event_subscription *subscription,
	size_t block_index, size_t count_index, uint64_t value, char **error);
int ib_event_test_start_pre_entry_callback(ib_event_subscription *subscription,
	char **error);
int ib_event_test_wait_pre_entry_callback(int timeout_ms, int *entered,
	char **error);
int ib_event_test_release_pre_entry_callback(char **error);
int ib_event_test_join_pre_entry_callback(char **error);
int ib_event_test_set_next_token(uintptr_t next_token, char **error);

#endif
