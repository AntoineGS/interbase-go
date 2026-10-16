#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <ibase.h>

static int detach_calls;
static int fail_detach;
static int consume_on_failure;
static int attach_calls;
static int fail_attach;
static int attach_returns_handle_on_failure;
static void *manager_pointer;
static int manager_free_calls;
static char service_handle_token;
static void *(*system_malloc)(size_t) = malloc;
static void *(*system_calloc)(size_t, size_t) = calloc;
static void (*system_free)(void *) = free;
static int fail_message_allocation;

static void *test_malloc(size_t size)
{
	if (fail_message_allocation) {
		return NULL;
	}
	return system_malloc(size);
}

static void *test_calloc(size_t count, size_t size)
{
	void *pointer = system_calloc(count, size);

	if (pointer != NULL && manager_pointer == NULL) {
		manager_pointer = pointer;
	}
	return pointer;
}

static void test_free(void *pointer)
{
	if (pointer == manager_pointer) {
		manager_free_calls++;
		return;
	}
	system_free(pointer);
}

ISC_STATUS ISC_EXPORT test_service_attach(ISC_STATUS *status,
	unsigned short name_length, char *name, isc_svc_handle *handle,
	unsigned short spb_length, char *spb);

ISC_STATUS ISC_EXPORT test_service_detach(ISC_STATUS *status,
	isc_svc_handle *handle);

#define malloc test_malloc
#define calloc test_calloc
#define isc_service_attach test_service_attach
#define isc_service_detach test_service_detach
#define free test_free
#include "../services/native.c"
#undef malloc
#undef calloc
#undef isc_service_attach
#undef isc_service_detach
#undef free

static void check(int condition, const char *message)
{
	if (!condition) {
		fprintf(stderr, "native services test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void set_status(ISC_STATUS *status)
{
	status[0] = isc_arg_gds;
	status[1] = fail_detach ? isc_network_error : 0;
	status[2] = isc_arg_end;
}

ISC_STATUS ISC_EXPORT test_service_attach(ISC_STATUS *status,
	unsigned short name_length, char *name, isc_svc_handle *handle,
	unsigned short spb_length, char *spb)
{
	(void) name_length;
	(void) name;
	(void) spb_length;
	(void) spb;
	attach_calls++;
	if (attach_returns_handle_on_failure || !fail_attach) {
		*handle = &service_handle_token;
	} else {
		*handle = NULL;
	}
	if (fail_attach) {
		set_status(status);
		return -1;
	}
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

ISC_STATUS ISC_EXPORT test_service_detach(ISC_STATUS *status,
	isc_svc_handle *handle)
{
	detach_calls++;
	if (fail_detach) {
		if (consume_on_failure) {
			*handle = NULL;
		}
		set_status(status);
		return -1;
	}
	*handle = NULL;
	set_status(status);
	return 0;
}

static ib_service_manager *new_manager(void)
{
	ib_service_manager *manager = (ib_service_manager *) calloc(1U,
		sizeof(*manager));
	check(manager != NULL, "manager allocation failed");
	manager->handle = &service_handle_token;
	return manager;
}

static void release_captured_manager(void)
{
	if (manager_pointer != NULL) {
		system_free(manager_pointer);
		manager_pointer = NULL;
	}
}

static void reset_open_flags(void)
{
	attach_calls = 0;
	fail_attach = 0;
	attach_returns_handle_on_failure = 0;
	detach_calls = 0;
	fail_detach = 0;
	consume_on_failure = 0;
	fail_message_allocation = 0;
	manager_pointer = NULL;
	manager_free_calls = 0;
}

static void test_attach_failure_cleanup_success_releases_wrapper(void)
{
	char *error = NULL;
	ib_service_manager *manager;
	int failed = 0;

	reset_open_flags();
	fail_attach = 1;
	attach_returns_handle_on_failure = 1;
	manager = ib_service_open("service_mgr", 11U, NULL, 0U, &error, &failed);
	check(manager == NULL && error != NULL && failed != 0,
		"successful attach cleanup did not preserve the attach error");
	check(attach_calls == 1 && detach_calls == 1 && manager_free_calls == 1,
		"successful attach cleanup did not detach and release its wrapper");
	ib_service_error_free(error);
	release_captured_manager();
}

static void test_attach_failure_cleanup_live_handle_is_recoverable(void)
{
	char *error = NULL;
	ib_service_manager *manager;
	int failed = 0;

	reset_open_flags();
	fail_attach = 1;
	attach_returns_handle_on_failure = 1;
	fail_detach = 1;
	manager = ib_service_open("service_mgr", 11U, NULL, 0U, &error, &failed);
	check(manager != NULL && error != NULL && failed != 0 &&
		manager->handle == &service_handle_token,
		"live failed attach did not return its recoverable native owner");
	check(attach_calls == 1 && detach_calls == 1 && manager_free_calls == 0,
		"live failed attach lost or freed its native owner");
	check(strstr(error, "service attach") != NULL &&
		strstr(error, "cleanup detach") != NULL,
		"live failed attach did not preserve attach and cleanup diagnostics");
	ib_service_error_free(error);
	fail_detach = 0;
	error = NULL;
	check(ib_service_close(manager, &error) == 0 && error == NULL,
		"recoverable failed attach could not be closed on retry");
	check(detach_calls == 2 && manager_free_calls == 1,
		"recoverable failed attach retry did not consume its owner");
	release_captured_manager();
}

static void test_attach_failure_cleanup_consumed_releases_wrapper(void)
{
	char *error = NULL;
	ib_service_manager *manager;
	int failed = 0;

	reset_open_flags();
	fail_attach = 1;
	attach_returns_handle_on_failure = 1;
	fail_detach = 1;
	consume_on_failure = 1;
	manager = ib_service_open("service_mgr", 11U, NULL, 0U, &error, &failed);
	check(manager == NULL && error != NULL && failed != 0,
		"consumed failed attach returned an inaccessible owner");
	check(attach_calls == 1 && detach_calls == 1 && manager_free_calls == 1,
		"consumed failed attach did not release its wrapper");
	check(strstr(error, "service attach") != NULL &&
		strstr(error, "cleanup detach") != NULL,
		"consumed failed attach did not preserve cleanup diagnostics");
	ib_service_error_free(error);
	release_captured_manager();
}

static void test_attach_failure_diagnostic_oom_preserves_live_identity(void)
{
	char *error = NULL;
	ib_service_manager *manager;
	int failed = 0;

	reset_open_flags();
	fail_attach = 1;
	attach_returns_handle_on_failure = 1;
	fail_detach = 1;
	/* Both the primary and cleanup diagnostics may be unavailable. The owner
	 * must still be returned when the fake detach leaves the handle live. */
	fail_message_allocation = 1;
	manager = ib_service_open("service_mgr", 11U, NULL, 0U, &error, &failed);
	check(manager != NULL && failed != 0 &&
		manager->handle == &service_handle_token,
		"diagnostic OOM lost the live failed-attach identity");
	check(error == NULL && manager_free_calls == 0 && detach_calls == 1,
		"diagnostic OOM did not retain the recoverable owner");
	fail_message_allocation = 0;
	fail_detach = 0;
	error = NULL;
	check(ib_service_close(manager, &error) == 0 && error == NULL,
		"live failed-attach owner was not closable after diagnostic OOM");
	check(manager_free_calls == 1 && detach_calls == 2,
		"diagnostic OOM owner retry did not release the wrapper");
	release_captured_manager();
}

static void test_live_failure_is_retryable(void)
{
	char *error = NULL;
	ib_service_manager *manager = new_manager();
	manager_pointer = manager;
	manager_free_calls = 0;
	detach_calls = 0;
	fail_detach = 1;
	consume_on_failure = 0;

	check(ib_service_close(manager, &error) != 0 && error != NULL,
		"live detach failure returned success");
	check(detach_calls == 1 && manager_free_calls == 0 &&
		manager->handle == &service_handle_token,
		"live detach failure did not preserve the service manager");
	ib_service_error_free(error);

	error = NULL;
	fail_detach = 0;
	check(ib_service_close(manager, &error) == 0 && error == NULL,
		"retry after live detach failure failed");
	check(detach_calls == 2 && manager_free_calls == 1,
		"successful detach retry did not consume the service manager");
	system_free(manager);
	manager_pointer = NULL;
}

static void test_consumed_failure_is_releasable(void)
{
	char *error = NULL;
	ib_service_manager *manager = new_manager();
	manager_pointer = manager;
	manager_free_calls = 0;
	detach_calls = 0;
	fail_detach = 1;
	consume_on_failure = 1;

	check(ib_service_close(manager, &error) != 0 && error != NULL,
		"consumed detach failure returned success");
	check(detach_calls == 1 && manager_free_calls == 0 && manager->handle == NULL,
		"consumed detach failure freed the wrapper before the error was observed");
	ib_service_error_free(error);

	error = NULL;
	check(ib_service_close(manager, &error) == 0 && error == NULL,
		"closing a consumed service wrapper was not idempotent");
	check(detach_calls == 1 && manager_free_calls == 1,
		"consumed service wrapper was detached again or not released");
	system_free(manager);
	manager_pointer = NULL;
}

static void test_status_text_names_the_failure(void)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	char *error = NULL;

	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = isc_random;
	status[2] = isc_arg_string;
	status[3] = (ISC_STATUS) (uintptr_t) "backup file busy";
	status[4] = isc_arg_end;
	check(fail_status(&error, "start backup", status) == -1 && error != NULL,
		"status text failure produced no error");
	check(strstr(error, "SQLCODE") != NULL && strstr(error, "): backup file busy") != NULL,
		"status-vector string argument is missing");
	free(error);
}

static void test_status_text_is_bounded(void)
{
	enum { LONG_LENGTH = 16384 };
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	char *argument;
	char *error = NULL;
	size_t index;

	argument = system_malloc(LONG_LENGTH + 1U);
	check(argument != NULL, "argument allocation failed");
	memset(argument, 'A', LONG_LENGTH);
	argument[LONG_LENGTH] = '\0';
	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = isc_random;
	for (index = 2U; index + 2U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_string;
		status[index + 1U] = (ISC_STATUS) (uintptr_t) argument;
	}
	status[index] = isc_arg_end;
	(void) fail_status(&error, "synthetic long argument", status);
	check(error != NULL && strstr(error, "AAA...") != NULL,
		"long argument was not clamped with a marker");
	check(strlen(error) < IB_STATUS_TEXT_LIMIT + 256U, "status error exceeds its bound");
	free(error);
	system_free(argument);

	/* A code without the arguments its template needs is padded, not read past. */
	memset(status, 0x5a, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = isc_network_error;
	status[2] = isc_arg_end;
	error = NULL;
	(void) fail_status(&error, "synthetic missing arguments", status);
	check(error != NULL && strstr(error, "SQLCODE") != NULL,
		"vector without template arguments produced no error");
	free(error);
}

static void test_unterminated_status_falls_back(void)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS copy[IB_STATUS_COPY_LENGTH];
	char storage[IB_STATUS_ARGUMENTS_LIMIT + IB_STATUS_VECTOR_LENGTH];
	char *error = NULL;
	size_t index;

	for (index = 0U; index + 1U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_gds;
		status[index + 1U] = isc_dsql_error;
	}
	check(!clamped_status_copy(status, copy, storage),
		"unterminated vector was accepted for interpretation");
	(void) fail_status(&error, "synthetic unterminated", status);
	check(error != NULL && strstr(error, "SQLCODE") != NULL,
		"unterminated vector lost its numeric context");
	free(error);
}

int main(void)
{
	test_status_text_names_the_failure();
	test_status_text_is_bounded();
	test_unterminated_status_falls_back();
	test_attach_failure_cleanup_success_releases_wrapper();
	test_attach_failure_cleanup_live_handle_is_recoverable();
	test_attach_failure_cleanup_consumed_releases_wrapper();
	test_attach_failure_diagnostic_oom_preserves_live_identity();
	test_live_failure_is_retryable();
	test_consumed_failure_is_releasable();
	puts("native services tests passed");
	return EXIT_SUCCESS;
}
