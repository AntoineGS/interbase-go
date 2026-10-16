#include "native.h"

#include <ibase.h>
#include <inttypes.h>
#include <limits.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#define IB_STATUS_VECTOR_LENGTH 20
#define IB_SQL_MESSAGE_LENGTH 256
#define IB_STATUS_TEXT_LIMIT 2048U
#define IB_STATUS_ARGUMENT_LIMIT 255U
#define IB_STATUS_ARGUMENTS_LIMIT 768U
/* gds__msg_format substitutes at most five arguments into a template. */
#define IB_STATUS_TEMPLATE_ARGUMENTS 5U
#define IB_STATUS_COPY_LENGTH \
	(IB_STATUS_VECTOR_LENGTH * (1U + IB_STATUS_TEMPLATE_ARGUMENTS) + 1U)
/* One interpreted line: a template, or the message-file-missing fallback and
 * its path, plus the clamped arguments. */
#define IB_STATUS_LINE_LENGTH (1024U + 4096U + IB_STATUS_ARGUMENTS_LIMIT)

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

/*
 * isc_interprete has no output length, and the client's own formatter
 * overflows its internal buffer for long string arguments and reads
 * uninitialized slots for arguments a code's template needs but the vector
 * lacks. It therefore only ever sees a private copy of the vector in which
 * string arguments are clamped, each to IB_STATUS_ARGUMENT_LIMIT bytes and all
 * to IB_STATUS_ARGUMENTS_LIMIT (a clamped argument ends in "..."), and every
 * message code carries at least IB_STATUS_TEMPLATE_ARGUMENTS arguments, padded
 * with empty strings. Returns 0 for a vector without an end marker within its
 * fixed length. copy holds IB_STATUS_COPY_LENGTH entries.
 */
static int clamped_status_copy(const ISC_STATUS *status, ISC_STATUS *copy,
	char *storage)
{
	static const char marker[] = "...";
	static const char empty[] = "";
	size_t index;
	size_t out;
	size_t used;
	size_t length;
	size_t allowed;
	size_t arguments;
	int in_message;
	ISC_STATUS argument;
	const char *text;

	if (status == NULL) {
		return 0;
	}
	out = 0U;
	used = 0U;
	arguments = 0U;
	in_message = 0;
	for (index = 0U; index < IB_STATUS_VECTOR_LENGTH;) {
		argument = status[index];
		if (in_message && argument != isc_arg_string &&
			argument != isc_arg_cstring && argument != isc_arg_number) {
			for (; arguments < IB_STATUS_TEMPLATE_ARGUMENTS; ++arguments) {
				copy[out++] = isc_arg_string;
				copy[out++] = (ISC_STATUS) (uintptr_t) empty;
			}
			in_message = 0;
		}
		if (argument == isc_arg_end) {
			copy[out] = isc_arg_end;
			return 1;
		}
		if (index + 1U >= IB_STATUS_VECTOR_LENGTH) {
			return 0;
		}
		if (argument == isc_arg_gds || argument == isc_arg_warning) {
			in_message = 1;
			arguments = 0U;
		} else if (argument == isc_arg_number) {
			++arguments;
		}
		if (argument != isc_arg_string && argument != isc_arg_cstring &&
			argument != isc_arg_interpreted) {
			copy[out++] = argument;
			copy[out++] = status[index + 1U];
			index += 2U;
			continue;
		}
		if (argument == isc_arg_cstring) {
			if (index + 2U >= IB_STATUS_VECTOR_LENGTH ||
				status[index + 1U] < 0) {
				return 0;
			}
			text = (const char *) (uintptr_t) status[index + 2U];
			length = (size_t) status[index + 1U];
		} else {
			text = (const char *) (uintptr_t) status[index + 1U];
			/* Only whether it exceeds the clamp matters past this length. */
			for (length = 0U; text != NULL &&
				length <= IB_STATUS_ARGUMENT_LIMIT && text[length] != '\0'; ++length) {
			}
		}
		if (text == NULL) {
			length = 0U;
		}
		allowed = IB_STATUS_ARGUMENTS_LIMIT - used;
		if (allowed > IB_STATUS_ARGUMENT_LIMIT) {
			allowed = IB_STATUS_ARGUMENT_LIMIT;
		}
		if (length > allowed && allowed < sizeof(marker) - 1U) {
			length = 0U;
		} else if (length > allowed) {
			length = allowed - (sizeof(marker) - 1U);
			memcpy(storage, text, length);
			memcpy(storage + length, marker, sizeof(marker) - 1U);
			length = allowed;
		} else if (length > 0U) {
			memcpy(storage, text, length);
		}
		storage[length] = '\0';
		/* A counted string becomes an ordinary terminated one. */
		copy[out++] = argument == isc_arg_interpreted ? isc_arg_interpreted : isc_arg_string;
		copy[out++] = (ISC_STATUS) (uintptr_t) storage;
		index += argument == isc_arg_cstring ? 3U : 2U;
		if (argument != isc_arg_interpreted) {
			++arguments;
		}
		storage += length + 1U;
		used += length;
	}
	return 0;
}

/*
 * Render the status vector as isc_interprete lines joined by '\n', at most
 * IB_STATUS_TEXT_LIMIT bytes; longer text ends with "...". Leaves text empty
 * when the vector has no end marker or yields nothing.
 */
static void status_text(const ISC_STATUS *status, char *text)
{
	static const char marker[] = "...";
	const size_t room = IB_STATUS_TEXT_LIMIT - (sizeof(marker) - 1U);
	ISC_STATUS copy[IB_STATUS_COPY_LENGTH];
	char storage[IB_STATUS_ARGUMENTS_LIMIT + IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS *cursor;
	size_t used;
	size_t length;
	size_t calls;
	int truncated;
	char *line;

	text[0] = '\0';
	if (!clamped_status_copy(status, copy, storage)) {
		return;
	}
	line = malloc(IB_STATUS_LINE_LENGTH);
	if (line == NULL) {
		return;
	}
	cursor = copy;
	used = 0U;
	truncated = 0;
	for (calls = 0U; calls < IB_STATUS_VECTOR_LENGTH; ++calls) {
		line[0] = '\0';
		if (isc_interprete(line, &cursor) <= 0) {
			break;
		}
		line[IB_STATUS_LINE_LENGTH - 1U] = '\0';
		length = strlen(line);
		if (length == 0U) {
			continue;
		}
		if (used > 0U) {
			if (used + 1U > room) {
				truncated = 1;
				break;
			}
			text[used++] = '\n';
		}
		if (length > room - used) {
			memcpy(text + used, line, room - used);
			used = room;
			truncated = 1;
			break;
		}
		memcpy(text + used, line, length);
		used += length;
	}
	free(line);
	if (truncated) {
		memcpy(text + used, marker, sizeof(marker) - 1U);
		used += sizeof(marker) - 1U;
	}
	text[used] = '\0';
}

static int fail_status(char **error, const char *operation, ISC_STATUS *status)
{
	char sql_message[IB_SQL_MESSAGE_LENGTH];
	char text[IB_STATUS_TEXT_LIMIT + 1U];
	char message[IB_STATUS_TEXT_LIMIT + 512U];
	ISC_STATUS primary;
	ISC_LONG sqlcode;
	int has_primary;

	has_primary = primary_status(status, &primary);
	sqlcode = 0;
	sql_message[0] = '\0';
	status_text(status, text);
	if (text[0] != '\0' && has_primary) {
		sqlcode = isc_sqlcode(status);
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE %" PRIdMAX ", native status %" PRIdPTR "): %s",
			operation, (intmax_t) sqlcode, (intptr_t) primary, text);
		give_error(error, plain_error(message));
		return -1;
	}
	if (text[0] != '\0') {
		(void) snprintf(message, sizeof(message),
			"%s failed (SQLCODE 0, native status unavailable): %s", operation, text);
		give_error(error, plain_error(message));
		return -1;
	}
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
