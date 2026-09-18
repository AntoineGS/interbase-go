#include "native.h"

#include <ibase.h>
#include <inttypes.h>
#include <limits.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define IB_STATUS_VECTOR_LENGTH 20
#define IB_SQL_MESSAGE_LENGTH 256

struct ib_service_manager {
	isc_svc_handle handle;
};

static char *copy_string(const char *value)
{
	size_t length;
	char *copy;

	if (value == NULL) {
		return NULL;
	}
	length = strlen(value);
	copy = (char *) malloc(length + 1U);
	if (copy == NULL) {
		return NULL;
	}
	memcpy(copy, value, length);
	copy[length] = '\0';
	return copy;
}

static char *plain_error(const char *message)
{
	return copy_string(message);
}

static char *join_errors(const char *first, const char *second)
{
	size_t first_length;
	size_t second_length;
	char *joined;

	if (first == NULL || second == NULL) {
		return NULL;
	}
	first_length = strlen(first);
	second_length = strlen(second);
	if (second_length > SIZE_MAX - 3U ||
		first_length > SIZE_MAX - second_length - 3U) {
		return NULL;
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

static void append_error(char **first_error, char *next_error)
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
	joined = join_errors(*first_error, next_error);
	if (joined != NULL) {
		free(*first_error);
		free(next_error);
		*first_error = joined;
		return;
	}
	/* Keep the first diagnostic when combining both messages runs out of
	 * memory. The native ownership result is reported independently. */
	free(next_error);
}

static void give_error(char **error, char *message)
{
	if (error != NULL) {
		*error = message;
	} else {
		free(message);
	}
}

static int primary_status(const ISC_STATUS *status, ISC_STATUS *value)
{
	size_t index;
	ISC_STATUS argument;

	if (status == NULL || value == NULL) {
		return 0;
	}
	for (index = 0U; index < IB_STATUS_VECTOR_LENGTH;) {
		argument = status[index];
		if (argument == isc_arg_end) {
			return 0;
		}
		if (index + 1U >= IB_STATUS_VECTOR_LENGTH) {
			return 0;
		}
		if (argument == isc_arg_gds || argument == isc_arg_warning) {
			*value = status[index + 1U];
			return 1;
		}
		if (argument == isc_arg_cstring) {
			if (index + 2U >= IB_STATUS_VECTOR_LENGTH) {
				return 0;
			}
			index += 3U;
		} else {
			index += 2U;
		}
	}
	return 0;
}

static int fail_status(char **error, const char *operation, ISC_STATUS *status)
{
	char sql_message[IB_SQL_MESSAGE_LENGTH];
	char message[1024];
	ISC_STATUS primary;
	ISC_LONG sqlcode;
	int has_primary;

	has_primary = primary_status(status, &primary);
	sqlcode = 0;
	sql_message[0] = '\0';
	if (has_primary) {
		sqlcode = isc_sqlcode(status);
		if (sqlcode >= (ISC_LONG) SHRT_MIN && sqlcode <= (ISC_LONG) SHRT_MAX) {
			isc_sql_interprete((short) sqlcode, sql_message,
				(short) sizeof(sql_message));
			sql_message[sizeof(sql_message) - 1U] = '\0';
		}
	}
	if (has_primary && sql_message[0] != '\0') {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE %" PRIdMAX ", native status %" PRIdPTR "): %s",
			operation, (intmax_t) sqlcode, (intptr_t) primary, sql_message);
	} else if (has_primary) {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE %" PRIdMAX ", native status %" PRIdPTR ")",
			operation, (intmax_t) sqlcode, (intptr_t) primary);
	} else {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE 0, native status unavailable)", operation);
	}
	give_error(error, plain_error(message));
	return -1;
}

ib_service_manager *ib_service_open(const char *name, size_t name_length,
	const unsigned char *spb, size_t spb_length, char **error, int *failed)
{
	ib_service_manager *manager;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	unsigned short name_size;
	unsigned short spb_size;
	char *first_error;
	int detach_result;

	if (error != NULL) {
		*error = NULL;
	}
	if (failed != NULL) {
		*failed = 0;
	}
	if (name == NULL || name_length == 0U) {
		if (error != NULL) {
			*error = plain_error("service attach requires a target");
		}
		return NULL;
	}
	if (name_length > (size_t) USHRT_MAX || spb_length > (size_t) USHRT_MAX) {
		if (error != NULL) {
			*error = plain_error("service attach parameter block exceeds native limit");
		}
		return NULL;
	}
	manager = (ib_service_manager *) calloc(1U, sizeof(*manager));
	if (manager == NULL) {
		if (error != NULL) {
			*error = plain_error("service attach allocation failed");
		}
		return NULL;
	}
	name_size = (unsigned short) name_length;
	spb_size = (unsigned short) spb_length;
	first_error = NULL;
	memset(status, 0, sizeof(status));
	if (isc_service_attach(status, name_size, (char *) name, &manager->handle,
		spb_size, (char *) spb) != 0) {
		if (failed != NULL) {
			*failed = 1;
		}
		(void) fail_status(&first_error, "service attach", status);
		if (manager->handle != NULL) {
			/* An attach failure is allowed to return a native handle. Detach it
			 * before deciding whether the wrapper can be released. The handle field
			 * is the ownership verdict: NULL means consumed, non-NULL means the
			 * caller must retain this wrapper for a retry. */
			memset(status, 0, sizeof(status));
			detach_result = isc_service_detach(status, &manager->handle);
			if (detach_result != 0) {
				char *cleanup_error = NULL;

				(void) fail_status(&cleanup_error,
					"service attach cleanup detach", status);
				append_error(&first_error, cleanup_error);
			} else if (manager->handle != NULL) {
				append_error(&first_error, plain_error(
					"service attach cleanup detach returned a live handle"));
			}
		}
		if (manager->handle != NULL) {
			/* Return the owner with its error so Go can quarantine and retry Close;
			 * never leave a live native handle behind an inaccessible allocation. */
			give_error(error, first_error);
			return manager;
		}
		free(manager);
		give_error(error, first_error);
		return NULL;
	}
	return manager;
}

int ib_service_start(ib_service_manager *manager, const unsigned char *request,
	size_t request_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	unsigned short request_size;

	if (error != NULL) {
		*error = NULL;
	}
	if (manager == NULL || manager->handle == NULL) {
		if (error != NULL) {
			*error = plain_error("service start on a closed manager");
		}
		return -1;
	}
	if (request == NULL || request_length == 0U || request_length > (size_t) USHRT_MAX) {
		if (error != NULL) {
			*error = plain_error("service start request is empty or exceeds native limit");
		}
		return -1;
	}
	request_size = (unsigned short) request_length;
	memset(status, 0, sizeof(status));
	if (isc_service_start(status, &manager->handle, NULL, request_size,
		(char *) request) != 0) {
		return fail_status(error, "service start", status);
	}
	return 0;
}

int ib_service_query(ib_service_manager *manager, unsigned char item,
	size_t capacity, unsigned char **result, size_t *result_length,
	int *truncated, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	unsigned char *buffer;
	unsigned char request[1];
	unsigned short result_size;
	size_t length;

	if (error != NULL) {
		*error = NULL;
	}
	if (result != NULL) {
		*result = NULL;
	}
	if (result_length != NULL) {
		*result_length = 0U;
	}
	if (truncated != NULL) {
		*truncated = 0;
	}
	if (manager == NULL || manager->handle == NULL) {
		if (error != NULL) {
			*error = plain_error("service query on a closed manager");
		}
		return -1;
	}
	if (capacity == 0U || capacity > (size_t) USHRT_MAX || result == NULL ||
		result_length == NULL || truncated == NULL) {
		if (error != NULL) {
			*error = plain_error("service query buffer is outside native bounds");
		}
		return -1;
	}
	buffer = (unsigned char *) calloc(1U, capacity);
	if (buffer == NULL) {
		if (error != NULL) {
			*error = plain_error("service query allocation failed");
		}
		return -1;
	}
	request[0] = item;
	result_size = (unsigned short) capacity;
	memset(status, 0, sizeof(status));
	if (isc_service_query(status, &manager->handle, NULL, 0, NULL, 1,
		(char *) request, result_size, (char *) buffer) != 0) {
		(void) fail_status(error, "service query", status);
		free(buffer);
		return -1;
	}
	*truncated = buffer[0] == isc_info_truncated ? 1 : 0;
	length = capacity;
	while (length > 0U && buffer[length - 1U] == 0U) {
		length--;
	}
	*result = buffer;
	*result_length = length;
	return 0;
}

int ib_service_close(ib_service_manager *manager, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	isc_svc_handle handle;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (manager == NULL) {
		return 0;
	}
	handle = manager->handle;
	if (handle == NULL) {
		free(manager);
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_service_detach(status, &handle);
	manager->handle = handle;
	if (result != 0) {
		return fail_status(error, "service detach", status);
	}
	if (handle != NULL) {
		if (error != NULL) {
			*error = plain_error("service detach returned a live handle");
		}
		return -1;
	}
	free(manager);
	return 0;
}

void ib_service_buffer_free(unsigned char *buffer)
{
	free(buffer);
}

void ib_service_error_free(char *error)
{
	free(error);
}
