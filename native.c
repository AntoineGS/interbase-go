#include "native.h"

#include <ibase.h>
#include <ctype.h>
#include <errno.h>
#include <inttypes.h>
#include <iconv.h>
#include <limits.h>
#include <math.h>
#include <pthread.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#define IB_STATUS_VECTOR_LENGTH 20
#define IB_MAX_SQLDA_BUFFER (1024U * 1024U)
#define IB_SQL_MESSAGE_LENGTH 256U
#define IB_BLOB_SEGMENT_LENGTH 32767U
#define IB_BLOB_BPB_LENGTH 64U
#define IB_MAX_BLOB_BUFFER (64U * 1024U * 1024U)
#define IB_MAX_ARRAY_BUFFER (64U * 1024U * 1024U)
#define IB_CHARSET_CONVERSION_CHUNK 32768U
#define IB_ICONV_PENDING_LENGTH 16U
#define IB_CHARSET_ASCII 2
#define IB_CHARSET_ISO8859_1 21
#define IB_CHARSET_WIN1250 51
#define IB_CHARSET_WIN1252 53
#define IB_CHARSET_UTF8 59

#if defined(__GNUC__) || defined(__clang__)
#define IB_MAYBE_UNUSED __attribute__((__unused__))
#else
#define IB_MAYBE_UNUSED
#endif

struct ib_cancel_slot {
	pthread_mutex_t mutex;
	pthread_cond_t condition;
	uint64_t generation;
	int active;
	int published;
	isc_stmt_handle statement;
	unsigned int cancel_users;
	unsigned int cancel_callers;
	int operation_complete;
	int completing;
	ISC_STATUS cancel_status[IB_STATUS_VECTOR_LENGTH];
	int64_t cancel_native_code;
};

enum ib_argument_kind {
	IB_ARGUMENT_NULL = 0,
	IB_ARGUMENT_STRING = 1,
	IB_ARGUMENT_INT64 = 2,
	IB_ARGUMENT_FLOAT64 = 3,
	IB_ARGUMENT_BOOL = 4,
	IB_ARGUMENT_TIMESTAMP = 5,
	IB_ARGUMENT_BYTES = 6,
	IB_ARGUMENT_ARRAY = 7,
	IB_ARGUMENT_BLOB_REF = 8
};

typedef struct ib_bind_value {
	int kind;
	char *bytes;
	size_t length;
	short source_charset;
	int64_t int64_value;
	double float64_value;
	int bool_value;
	int64_t year;
	int month;
	int day;
	int hour;
	int minute;
	int second;
	int nanosecond;
	int32_t blob_high;
	uint32_t blob_low;
	int blob_subtype;
	int blob_charset;
	struct ib_array_value *array;
} ib_bind_value;

typedef struct ib_array_value {
	size_t dimensions;
	ib_array_bound *bounds;
	size_t element_count;
	ib_array_element *elements;
} ib_array_value;

struct ib_blob_reader {
	ib_connection *connection;
	isc_blob_handle blob;
	char *segment;
	size_t segment_length;
	size_t segment_offset;
	int eof;
};

struct ib_blob_writer {
	ib_connection *connection;
	isc_blob_handle blob;
	ISC_QUAD blob_id;
	short subtype;
	short charset;
};

struct ib_bindings {
	size_t count;
	ib_bind_value *values;
};

struct ib_connection {
	isc_db_handle database;
	ib_connection *parent;
	ib_cursor *cursors;
	ib_statement *statements;
	isc_tr_handle transaction;
	short charset;
	int dialect;
	int transaction_read_only;
	int broken;
	char *read_tpb;
	size_t read_tpb_length;
	char *write_tpb;
	size_t write_tpb_length;
};

struct ib_transaction {
	ib_connection *parent;
	ib_connection view;
	ib_distributed_transaction *distributed;
};

typedef struct ib_transaction_entry {
	isc_db_handle *database;
	short tpb_length;
	char *tpb;
} ib_transaction_entry;

struct ib_distributed_transaction {
	isc_tr_handle handle;
	ib_connection **parents;
	ib_transaction *participants;
	size_t count;
	int prepared;
};

static void ib_mark_broken(ib_connection *connection)
{
	if (connection == NULL) {
		return;
	}
	connection->broken = 1;
	if (connection->parent != NULL) {
		connection->parent->broken = 1;
	}
}

struct ib_statement {
	ib_connection *connection;
	isc_stmt_handle statement;
	XSQLDA *input;
	XSQLDA *output;
	char *query;
	size_t query_length;
	int statement_type;
	ib_cursor *active_cursor;
	ib_statement *next;
};

struct ib_cursor {
	ib_connection *connection;
	ib_statement *prepared;
	isc_tr_handle transaction;
	isc_stmt_handle statement;
	XSQLDA *input;
	XSQLDA *output;
	ib_column_metadata *metadata;
	short *blob_charsets;
	unsigned char *blob_charset_known;
	char **column_names;
	size_t *column_name_lengths;
	char *converted_value;
	char *materialized_value;
	size_t *fixed_text_lengths;
	int owns_transaction;
	int server_cursor_open;
	int statement_type;
	int procedure;
	int output_pending;
	int fetched;
	int allow_arrays;
	char *query;
	size_t query_length;
	char *array_data;
	ib_array_bound *array_bounds;
	ib_value_view *array_elements;
	char **array_element_data;
	size_t array_dimensions;
	size_t array_element_count;
	ib_array_view array_view;
	ib_cursor *next;
};

static void ib_cursor_register(ib_cursor *cursor)
{
	if (cursor == NULL || cursor->connection == NULL) {
		return;
	}
	cursor->next = cursor->connection->cursors;
	cursor->connection->cursors = cursor;
}

static void ib_cursor_unregister(ib_cursor *cursor)
{
	ib_cursor **current;

	if (cursor == NULL || cursor->connection == NULL) {
		return;
	}
	current = &cursor->connection->cursors;
	while (*current != NULL) {
		if (*current == cursor) {
			*current = cursor->next;
			cursor->next = NULL;
			return;
		}
		current = &(*current)->next;
	}
}

static unsigned short ib_connection_dialect(const ib_connection *connection)
{
	if (connection != NULL && connection->dialect == SQL_DIALECT_V5) {
		return SQL_DIALECT_V5;
	}
	return SQL_DIALECT_V6;
}

static short ib_connection_charset(const ib_connection *connection)
{
	if (connection != NULL && connection->charset != 0) {
		return connection->charset;
	}
	return IB_CHARSET_UTF8;
}

static char *ib_copy_string(const char *value, size_t length)
{
	char *copy;

	if (length == SIZE_MAX) {
		return NULL;
	}
	copy = (char *) malloc(length + 1U);
	if (copy == NULL) {
		return NULL;
	}
	if (length != 0U && value != NULL) {
		memcpy(copy, value, length);
	}
	copy[length] = '\0';
	return copy;
}

static char *ib_copy_buffer(const char *value, size_t length)
{
	char *copy;

	if (value == NULL || length == 0U) {
		return NULL;
	}
	copy = (char *) malloc(length);
	if (copy == NULL) {
		return NULL;
	}
	memcpy(copy, value, length);
	return copy;
}

static int ib_fail(char **error, const char *message);
static int ib_array_shape_valid(const ib_array_bound *bounds, size_t dimensions,
	size_t element_count, char **error);
static size_t ib_text_character_count_for_charset(size_t length, short charset);
static int ib_bindings_set_encoded_string(ib_bindings *bindings, size_t index,
	const char *value, size_t length, short source_charset, char **error);

static const char *ib_charset_encoding(short charset)
{
	switch (charset) {
	case 2:
		return "ASCII";
	case 3:
		return "UTF-8";
	case 5:
		return "SHIFT-JIS";
	case 6:
		return "EUC-JP";
	case 8:
		return "UTF-16BE";
	case 10:
		return "CP437";
	case 11:
		return "CP850";
	case 12:
		return "CP865";
	case 13:
		return "CP860";
	case 14:
		return "CP863";
	case 21:
		return "ISO-8859-1";
	case 22:
		return "ISO-8859-2";
	case 39:
		return "ISO-8859-15";
	case 44:
		return "EUC-KR";
	case 45:
		return "CP852";
	case 46:
		return "CP857";
	case 47:
		return "CP861";
	case 51:
		return "CP1250";
	case 52:
		return "CP1251";
	case 53:
		return "CP1252";
	case 54:
		return "CP1253";
	case 55:
		return "CP1254";
	case 56:
		return "BIG5";
	case 57:
		return "GB2312";
	case 58:
		return "KOI8-R";
	case 59:
		return "UTF-8";
	case 64:
		return "UTF-16LE";
	default:
		return NULL;
	}
}

static int ib_charset_name_matches(const char *value, size_t length,
	const char *expected)
{
	size_t index;
	size_t expected_length;

	if (value == NULL || expected == NULL) {
		return 0;
	}
	expected_length = strlen(expected);
	if (length != expected_length) {
		return 0;
	}
	for (index = 0U; index < length; index++) {
		if (toupper((unsigned char) value[index]) !=
			toupper((unsigned char) expected[index])) {
			return 0;
		}
	}
	return 1;
}

static int ib_charset_id(const char *value, size_t length, short *charset)
{
	if (charset == NULL) {
		return -1;
	}
	if (length == 0U || ib_charset_name_matches(value, length, "UTF8")) {
		*charset = IB_CHARSET_UTF8;
		return 0;
	}
	if (ib_charset_name_matches(value, length, "WIN1250")) {
		*charset = IB_CHARSET_WIN1250;
		return 0;
	}
	if (ib_charset_name_matches(value, length, "WIN1252")) {
		*charset = IB_CHARSET_WIN1252;
		return 0;
	}
	if (ib_charset_name_matches(value, length, "ISO8859_1")) {
		*charset = IB_CHARSET_ISO8859_1;
		return 0;
	}
	if (ib_charset_name_matches(value, length, "ASCII")) {
		*charset = IB_CHARSET_ASCII;
		return 0;
	}
	return -1;
}

static const char *ib_charset_name(short charset)
{
	switch (charset) {
	case IB_CHARSET_UTF8:
		return "UTF8";
	case IB_CHARSET_WIN1250:
		return "WIN1250";
	case IB_CHARSET_WIN1252:
		return "WIN1252";
	case IB_CHARSET_ISO8859_1:
		return "ISO8859_1";
	case IB_CHARSET_ASCII:
		return "ASCII";
	default:
		return NULL;
	}
}

static int ib_append_conversion_data(char **data, size_t *length,
	size_t *capacity, const char *source, size_t source_length, char **error);

static char *ib_convert_charset(const char *value, size_t length,
	short source_charset, short target_charset, size_t *converted_length,
	char **error)
{
	char *converted;
	const char *source_encoding;
	const char *target_encoding;
	iconv_t converter;
	char pending[IB_ICONV_PENDING_LENGTH];
	char *input_cursor;
	char *output_cursor;
	char input[IB_CHARSET_CONVERSION_CHUNK + IB_ICONV_PENDING_LENGTH];
	char output[IB_CHARSET_CONVERSION_CHUNK];
	size_t capacity;
	size_t input_length;
	size_t input_left;
	size_t offset;
	size_t pending_length;
	size_t output_length;
	size_t output_left;
	size_t copied;
	size_t result;
	int conversion_error;

	if (converted_length == NULL) {
		return NULL;
	}
	*converted_length = 0U;
	if (length != 0U && value == NULL) {
		(void) ib_fail(error, "character data is unavailable");
		return NULL;
	}
	if (length > (size_t) IB_MAX_BLOB_BUFFER) {
		(void) ib_fail(error, "character data exceeds the conversion limit");
		return NULL;
	}
	if (length == 0U || source_charset == target_charset ||
		source_charset == 0 || source_charset == 1 || target_charset == 0 ||
		target_charset == 1) {
		converted = ib_copy_string(value, length);
		if (converted == NULL) {
			(void) ib_fail(error, "out of memory copying string query argument");
			return NULL;
		}
		*converted_length = length;
		return converted;
	}
	source_encoding = ib_charset_encoding(source_charset);
	target_encoding = ib_charset_encoding(target_charset);
	if (source_encoding == NULL || target_encoding == NULL) {
		(void) ib_fail(error, "the character set conversion is unsupported");
		return NULL;
	}
	converter = iconv_open(target_encoding, source_encoding);
	if (converter == (iconv_t) -1) {
		(void) ib_fail(error, "the character set converter is unavailable");
		return NULL;
	}
	converted = NULL;
	capacity = 0U;
	output_length = 0U;
	pending_length = 0U;
	offset = 0U;
	conversion_error = 0;
	while (offset < length || pending_length != 0U) {
		input_length = pending_length;
		if (pending_length != 0U) {
			memcpy(input, pending, pending_length);
		}
		copied = length - offset;
		if (copied > (size_t) IB_CHARSET_CONVERSION_CHUNK) {
			copied = (size_t) IB_CHARSET_CONVERSION_CHUNK;
		}
		if (copied != 0U) {
			memcpy(input + input_length, value + offset, copied);
			input_length += copied;
			offset += copied;
		}
		pending_length = 0U;
		input_cursor = input;
		input_left = input_length;
		while (input_left != 0U) {
			size_t input_before = input_left;

			output_cursor = output;
			output_left = sizeof(output);
			errno = 0;
			result = iconv(converter, &input_cursor, &input_left,
				&output_cursor, &output_left);
			if (ib_append_conversion_data(&converted, &output_length, &capacity,
				output, sizeof(output) - output_left, error) != 0) {
				conversion_error = 1;
				break;
			}
			if (result != (size_t) -1) {
				if (input_left == input_before && output_left == sizeof(output)) {
					(void) ib_fail(error, "character set conversion made no progress");
					conversion_error = 1;
					break;
				}
				continue;
			}
			if (errno == E2BIG) {
				continue;
			}
			if (errno == EINVAL && input_left != 0U && offset < length) {
				if (input_left > sizeof(pending)) {
					(void) ib_fail(error, "incomplete character sequence is too long");
					conversion_error = 1;
					break;
				}
				memcpy(pending, input_cursor, input_left);
				pending_length = input_left;
				input_left = 0U;
				break;
			}
			if (errno == EILSEQ || errno == EINVAL) {
				(void) ib_fail(error, "character data could not be converted");
			} else {
				(void) ib_fail(error, "character set conversion failed");
			}
			conversion_error = 1;
			break;
		}
		if (conversion_error != 0) {
			break;
		}
	}
	if (conversion_error == 0) {
		for (;;) {
			output_cursor = output;
			output_left = sizeof(output);
			errno = 0;
			result = iconv(converter, NULL, NULL, &output_cursor, &output_left);
			if (ib_append_conversion_data(&converted, &output_length, &capacity,
				output, sizeof(output) - output_left, error) != 0) {
				conversion_error = 1;
				break;
			}
			if (result != (size_t) -1) {
				break;
			}
			if (errno == E2BIG) {
				continue;
			}
			(void) ib_fail(error, errno == EILSEQ || errno == EINVAL ?
				"character data could not be converted" :
				"character set conversion failed");
			conversion_error = 1;
			break;
		}
	}
	iconv_close(converter);
	if (conversion_error != 0) {
		free(converted);
		return NULL;
	}
	if (converted == NULL) {
		converted = ib_copy_string(NULL, 0U);
		if (converted == NULL) {
			(void) ib_fail(error, "out of memory converting character data");
			return NULL;
		}
	}
	*converted_length = output_length;
	return converted;
}

static int ib_append_conversion_data(char **data, size_t *length,
	size_t *capacity, const char *source, size_t source_length, char **error)
{
	char *resized;
	size_t needed;
	size_t new_capacity;

	if (data == NULL || length == NULL || capacity == NULL ||
		(source_length != 0U && source == NULL)) {
		return ib_fail(error, "converted character data storage is unavailable");
	}
	if (source_length == 0U) {
		return 0;
	}
	if (*length > (size_t) IB_MAX_BLOB_BUFFER ||
		source_length > (size_t) IB_MAX_BLOB_BUFFER - *length) {
		return ib_fail(error, "converted character data exceeds the conversion limit");
	}
	needed = *length + source_length;
	if (needed > *capacity) {
		new_capacity = *capacity == 0U ? (size_t) IB_CHARSET_CONVERSION_CHUNK : *capacity;
		while (new_capacity < needed) {
			if (new_capacity > (size_t) IB_MAX_BLOB_BUFFER / 2U) {
				new_capacity = (size_t) IB_MAX_BLOB_BUFFER;
				break;
			}
			new_capacity *= 2U;
		}
		if (new_capacity > (size_t) IB_MAX_BLOB_BUFFER) {
			new_capacity = (size_t) IB_MAX_BLOB_BUFFER;
		}
		resized = (char *) realloc(*data, new_capacity + 1U);
		if (resized == NULL) {
			return ib_fail(error, "out of memory converting character data");
		}
		*data = resized;
		*capacity = new_capacity;
	}
	memcpy(*data + *length, source, source_length);
	*length = needed;
	(*data)[*length] = '\0';
	return 0;
}

static char *ib_convert_utf8(const char *value, size_t length, short charset,
	size_t *converted_length, char **error)
{
	return ib_convert_charset(value, length, IB_CHARSET_UTF8, charset,
		converted_length, error);
}

static char *ib_convert_to_utf8(const char *value, size_t length, short charset,
	size_t *converted_length, char **error)
{
	return ib_convert_charset(value, length, charset, IB_CHARSET_UTF8,
		converted_length, error);
}

static void ib_give_error(char **error, char *message)
{
	if (error != NULL) {
		*error = message;
	} else {
		free(message);
	}
}

static int ib_fail(char **error, const char *message)
{
	char *copy;

	copy = ib_copy_string(message, strlen(message));
	ib_give_error(error, copy);
	return -1;
}

static int ib_pthread_fail(char **error, const char *operation, int result)
{
	char message[256];
	const char *detail;

	detail = strerror(result);
	if (detail == NULL) {
		detail = "unknown synchronization error";
	}
	(void) snprintf(message, sizeof(message), "%s failed: %s", operation, detail);
	return ib_fail(error, message);
}

static void ib_cancel_slot_unlock_or_abort(ib_cancel_slot *slot)
{
	if (pthread_mutex_unlock(&slot->mutex) != 0) {
		abort();
	}
}

static void ib_cancel_slot_cancel_caller_done(ib_cancel_slot *slot)
{
	if (slot->cancel_callers == 0U) {
		abort();
	}
	slot->cancel_callers--;
#ifdef IB_CANCEL_SLOT_TEST_CALLER_RELEASE_HOOK
	IB_CANCEL_SLOT_TEST_CALLER_RELEASE_HOOK(slot);
#endif
	if (pthread_cond_broadcast(&slot->condition) != 0) {
		abort();
	}
}

ib_cancel_slot *ib_cancel_slot_new(char **error)
{
	ib_cancel_slot *slot;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	slot = (ib_cancel_slot *) malloc(sizeof(*slot));
	if (slot == NULL) {
		(void) ib_fail(error, "out of memory allocating cancellation slot");
		return NULL;
	}
	(void) memset(slot, 0, sizeof(*slot));
	slot->operation_complete = 1;
	result = pthread_mutex_init(&slot->mutex, NULL);
	if (result != 0) {
		free(slot);
		(void) ib_pthread_fail(error, "pthread_mutex_init", result);
		return NULL;
	}
	result = pthread_cond_init(&slot->condition, NULL);
	if (result != 0) {
		if (pthread_mutex_destroy(&slot->mutex) != 0) {
			abort();
		}
		free(slot);
		(void) ib_pthread_fail(error, "pthread_cond_init", result);
		return NULL;
	}
	return slot;
}

uint64_t ib_cancel_slot_begin(ib_cancel_slot *slot, char **error)
{
	uint64_t generation;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (slot == NULL) {
		(void) ib_fail(error, "cancellation slot is unavailable");
		return 0U;
	}
	result = pthread_mutex_lock(&slot->mutex);
	if (result != 0) {
		(void) ib_pthread_fail(error, "pthread_mutex_lock", result);
		return 0U;
	}
	if (slot->active || slot->completing || slot->cancel_users != 0U ||
		slot->cancel_callers != 0U) {
		ib_cancel_slot_unlock_or_abort(slot);
		(void) ib_fail(error, "cancellation slot operation is already active");
		return 0U;
	}
	if (slot->generation == UINT64_MAX) {
		ib_cancel_slot_unlock_or_abort(slot);
		(void) ib_fail(error, "cancellation slot generation exhausted");
		return 0U;
	}
	generation = slot->generation + 1U;
	slot->generation = generation;
	slot->active = 1;
	slot->published = 0;
	slot->statement = NULL;
	slot->operation_complete = 0;
	slot->cancel_native_code = 0;
	(void) memset(slot->cancel_status, 0, sizeof(slot->cancel_status));
	result = pthread_cond_broadcast(&slot->condition);
	if (result != 0) {
		slot->active = 0;
		slot->operation_complete = 1;
		ib_cancel_slot_unlock_or_abort(slot);
		(void) ib_pthread_fail(error, "pthread_cond_broadcast", result);
		return 0U;
	}
	result = pthread_mutex_unlock(&slot->mutex);
	if (result != 0) {
		abort();
	}
	return generation;
}

static int IB_MAYBE_UNUSED ib_cancel_slot_publish(ib_cancel_slot *slot,
	uint64_t generation,
	isc_stmt_handle *statement, char **error)
{
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (slot == NULL || statement == NULL || generation == 0U) {
		return ib_fail(error, "cancellation slot publication arguments are invalid");
	}
	result = pthread_mutex_lock(&slot->mutex);
	if (result != 0) {
		return ib_pthread_fail(error, "pthread_mutex_lock", result);
	}
	if (slot->generation != generation || !slot->active ||
		slot->operation_complete || slot->completing || slot->published) {
		ib_cancel_slot_unlock_or_abort(slot);
		return ib_fail(error, "cancellation slot generation cannot be published");
	}
	slot->statement = *statement;
	slot->published = 1;
	result = pthread_cond_broadcast(&slot->condition);
	if (result != 0) {
		slot->statement = NULL;
		slot->published = 0;
		ib_cancel_slot_unlock_or_abort(slot);
		return ib_pthread_fail(error, "pthread_cond_broadcast", result);
	}
	result = pthread_mutex_unlock(&slot->mutex);
	if (result != 0) {
		abort();
	}
	return 0;
}

static int ib_cancel_slot_publish_if_active(ib_cancel_slot *slot,
	uint64_t generation, isc_stmt_handle *statement, char **error)
{
	if (slot == NULL && generation == 0U) {
		if (error != NULL) {
			*error = NULL;
		}
		return 0;
	}
	return ib_cancel_slot_publish(slot, generation, statement, error);
}

static void IB_MAYBE_UNUSED ib_cancel_slot_complete(ib_cancel_slot *slot,
	uint64_t generation)
{
	int result;

	if (slot == NULL || generation == 0U) {
		return;
	}
	result = pthread_mutex_lock(&slot->mutex);
	if (result != 0) {
		abort();
	}
	if (slot->generation != generation || slot->operation_complete ||
		slot->completing) {
		if (pthread_mutex_unlock(&slot->mutex) != 0) {
			abort();
		}
		return;
	}
	slot->active = 0;
	slot->published = 0;
	slot->statement = NULL;
	slot->operation_complete = 1;
	slot->completing = 1;
	result = pthread_cond_broadcast(&slot->condition);
	if (result != 0) {
		ib_cancel_slot_unlock_or_abort(slot);
		abort();
	}
	while (slot->cancel_users != 0U) {
#ifdef IB_CANCEL_SLOT_TEST_COMPLETION_WAIT_HOOK
		IB_CANCEL_SLOT_TEST_COMPLETION_WAIT_HOOK(slot);
#endif
		result = pthread_cond_wait(&slot->condition, &slot->mutex);
		if (result != 0) {
			ib_cancel_slot_unlock_or_abort(slot);
			abort();
		}
	}
	slot->completing = 0;
	result = pthread_cond_broadcast(&slot->condition);
	if (result != 0) {
		ib_cancel_slot_unlock_or_abort(slot);
		abort();
	}
	result = pthread_mutex_unlock(&slot->mutex);
	if (result != 0) {
		abort();
	}
}

int ib_cancel_slot_cancel(ib_cancel_slot *slot, uint64_t generation,
	int64_t *native_code, char **error)
{
	isc_stmt_handle statement;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result_status;
	int result;

	if (error != NULL) {
		*error = NULL;
	}
	if (native_code == NULL) {
		return ib_fail(error, "cancellation native status output is unavailable");
	}
	*native_code = 0;
	if (slot == NULL) {
		return ib_fail(error, "cancellation slot is unavailable");
	}
	statement = NULL;
	result = pthread_mutex_lock(&slot->mutex);
	if (result != 0) {
		return ib_pthread_fail(error, "pthread_mutex_lock", result);
	}
	if (slot->cancel_callers == UINT_MAX) {
		ib_cancel_slot_unlock_or_abort(slot);
		return ib_fail(error, "cancellation slot caller count exhausted");
	}
	slot->cancel_callers++;
	result = pthread_cond_broadcast(&slot->condition);
	if (result != 0) {
		abort();
	}
	for (;;) {
		if (slot->generation != generation || slot->operation_complete ||
			!slot->active || slot->completing) {
			ib_cancel_slot_cancel_caller_done(slot);
			ib_cancel_slot_unlock_or_abort(slot);
			return 0;
		}
		if (slot->published) {
			if (slot->cancel_users == UINT_MAX) {
				ib_cancel_slot_cancel_caller_done(slot);
				ib_cancel_slot_unlock_or_abort(slot);
				return ib_fail(error, "cancellation slot user count exhausted");
			}
			statement = slot->statement;
			slot->cancel_users++;
			break;
		}
		result = pthread_cond_wait(&slot->condition, &slot->mutex);
		if (result != 0) {
			ib_cancel_slot_cancel_caller_done(slot);
			ib_cancel_slot_unlock_or_abort(slot);
			return ib_pthread_fail(error, "pthread_cond_wait", result);
		}
	}
	result = pthread_mutex_unlock(&slot->mutex);
	if (result != 0) {
		abort();
	}

	(void) memset(status, 0, sizeof(status));
	result_status = isc_dsql_free_statement(status, &statement, DSQL_cancel);
	result = pthread_mutex_lock(&slot->mutex);
	if (result != 0) {
		abort();
	}
	(void) memcpy(slot->cancel_status, status, sizeof(slot->cancel_status));
	slot->cancel_native_code = (int64_t) result_status;
	slot->cancel_users--;
	ib_cancel_slot_cancel_caller_done(slot);
	ib_cancel_slot_unlock_or_abort(slot);
	*native_code = (int64_t) result_status;
	return 0;
}

void ib_cancel_slot_free(ib_cancel_slot *slot)
{
	int result;

	if (slot == NULL) {
		return;
	}
	result = pthread_mutex_lock(&slot->mutex);
	if (result != 0) {
		abort();
	}
	while (slot->active || slot->published || slot->completing ||
		slot->cancel_users != 0U || slot->cancel_callers != 0U) {
		result = pthread_cond_wait(&slot->condition, &slot->mutex);
		if (result != 0) {
			(void) pthread_mutex_unlock(&slot->mutex);
			abort();
		}
	}
	result = pthread_mutex_unlock(&slot->mutex);
	if (result != 0) {
		abort();
	}
#ifdef IB_CANCEL_SLOT_TEST_FREE_READY_HOOK
	IB_CANCEL_SLOT_TEST_FREE_READY_HOOK(slot);
#endif
	result = pthread_cond_destroy(&slot->condition);
	if (result != 0) {
		abort();
	}
	result = pthread_mutex_destroy(&slot->mutex);
	if (result != 0) {
		abort();
	}
	free(slot);
}

static char *ib_join_errors(const char *first, const char *second)
{
	static const char fallback[] = "native operation and cleanup both failed";
	size_t first_length;
	size_t second_length;
	char *joined;

	first_length = strlen(first);
	second_length = strlen(second);
	if (second_length > SIZE_MAX - 3U ||
		first_length > SIZE_MAX - second_length - 3U) {
		return ib_copy_string(fallback, sizeof(fallback) - 1U);
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

static int ib_primary_status(const ISC_STATUS *status, ISC_STATUS *value)
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

static int ib_fail_status(char **error, const char *operation,
	ISC_STATUS *status)
{
	char sql_message[IB_SQL_MESSAGE_LENGTH];
	char message[2560];
	ISC_STATUS primary_status;
	ISC_LONG sqlcode;
	int has_primary_status;

	has_primary_status = ib_primary_status(status, &primary_status);
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
	return ib_fail(error, message);
}

static void ib_append_error(char **first_error, char *next_error)
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
	joined = ib_join_errors(*first_error, next_error);
	if (joined != NULL) {
		free(*first_error);
		free(next_error);
		*first_error = joined;
	} else {
		free(next_error);
	}
}

static int ib_start_transaction_tpb(ib_connection *connection,
	isc_tr_handle *transaction, const char *tpb, size_t tpb_length,
	const char *operation, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (connection == NULL || connection->database == NULL || connection->broken ||
		transaction == NULL || *transaction != NULL) {
		return ib_fail(error, "connection or transaction is unavailable");
	}
	if (tpb == NULL || tpb_length == 0U || tpb_length > (size_t) SHRT_MAX) {
		return ib_fail(error, "transaction parameter block is invalid");
	}
	memset(status, 0, sizeof(status));
	result = isc_start_transaction(status, transaction, 1,
		&connection->database, (short) tpb_length, (char *) tpb);
	if (result != 0) {
		ib_mark_broken(connection);
		if (*transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];
			char *rollback_error = NULL;

			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed transaction",
					rollback_status);
			}
			if (rollback_error != NULL) {
				char *primary_error = NULL;
				(void) ib_fail_status(&primary_error, operation, status);
				ib_append_error(&primary_error, rollback_error);
				ib_give_error(error, primary_error);
				return -1;
			}
		}
		return ib_fail_status(error, operation, status);
	}
	return 0;
}

static int ib_start_transaction(ib_connection *connection,
	isc_tr_handle *transaction, int read_only, const char *operation,
	char **error)
{
	const char fallback_tpb[] = {
		isc_tpb_version3,
		read_only ? isc_tpb_read : isc_tpb_write,
		isc_tpb_wait,
		isc_tpb_read_committed,
		isc_tpb_rec_version
	};
	const char *tpb = fallback_tpb;
	size_t tpb_length = sizeof(fallback_tpb);

	if (connection != NULL) {
		if (read_only && connection->read_tpb != NULL) {
			tpb = connection->read_tpb;
			tpb_length = connection->read_tpb_length;
		} else if (!read_only && connection->write_tpb != NULL) {
			tpb = connection->write_tpb;
			tpb_length = connection->write_tpb_length;
		}
	}

	return ib_start_transaction_tpb(connection, transaction, tpb, tpb_length,
		operation, error);
}

static int ib_tpb_is_read_only(const char *tpb, size_t tpb_length)
{
	size_t index;

	if (tpb == NULL) {
		return 0;
	}
	for (index = 0U; index < tpb_length; index++) {
		if ((unsigned char) tpb[index] == (unsigned char) isc_tpb_write) {
			return 0;
		}
		if ((unsigned char) tpb[index] == (unsigned char) isc_tpb_read) {
			return 1;
		}
	}
	return 0;
}

static void ib_free_sqlda(XSQLDA *sqlda)
{
	short index;

	if (sqlda == NULL) {
		return;
	}
	for (index = 0; index < sqlda->sqln; index++) {
		free(sqlda->sqlvar[index].sqldata);
		free(sqlda->sqlvar[index].sqlind);
		sqlda->sqlvar[index].sqldata = NULL;
		sqlda->sqlvar[index].sqlind = NULL;
	}
	free(sqlda);
}

static XSQLDA *ib_alloc_sqlda(short count)
{
	XSQLDA *sqlda;
	size_t size = 0U;

	if (count < 1) {
		count = 1;
	}
	size = sizeof(XSQLDA) + ((size_t) count - 1U) * sizeof(XSQLVAR);
	if (size > (size_t) INT_MAX) {
		return NULL;
	}
	sqlda = (XSQLDA *) calloc(1U, size);
	if (sqlda == NULL) {
		return NULL;
	}
	sqlda->version = SQLDA_VERSION2;
	sqlda->sqldabc = (ISC_LONG) size;
	sqlda->sqln = count;
	sqlda->sqld = 0;
	return sqlda;
}

static XSQLDA *ib_clone_sqlda(const XSQLDA *source, char **error)
{
	XSQLDA *clone;
	short index;

	if (source == NULL || source->sqln < 0 || source->sqld < 0 ||
		source->sqld > source->sqln) {
		(void) ib_fail(error, "the prepared SQLDA is invalid");
		return NULL;
	}
	clone = ib_alloc_sqlda(source->sqln);
	if (clone == NULL) {
		(void) ib_fail(error, "out of memory copying prepared SQLDA");
		return NULL;
	}
	clone->sqldabc = source->sqldabc;
	clone->sqld = source->sqld;
	for (index = 0; index < source->sqln; index++) {
		clone->sqlvar[index] = source->sqlvar[index];
		clone->sqlvar[index].sqldata = NULL;
		clone->sqlvar[index].sqlind = NULL;
	}
	return clone;
}

static int ib_sql_type(const XSQLVAR *variable)
{
	return ((int) variable->sqltype) & ~1;
}

static int ib_metadata_type_code(int type)
{
	switch (type) {
	case SQL_TEXT:
		return IB_METADATA_CHAR;
	case SQL_VARYING:
		return IB_METADATA_VARCHAR;
	case SQL_SHORT:
		return IB_METADATA_SMALLINT;
	case SQL_LONG:
		return IB_METADATA_INTEGER;
	case SQL_INT64:
		return IB_METADATA_BIGINT;
	case SQL_FLOAT:
		return IB_METADATA_FLOAT;
	case SQL_DOUBLE:
	case SQL_D_FLOAT:
		return IB_METADATA_DOUBLE;
	case SQL_TIMESTAMP:
		return IB_METADATA_TIMESTAMP;
	case SQL_TYPE_DATE:
		return IB_METADATA_DATE;
	case SQL_TYPE_TIME:
		return IB_METADATA_TIME;
	case SQL_BOOLEAN:
		return IB_METADATA_BOOLEAN;
	case SQL_BLOB:
		return IB_METADATA_BLOB;
	case SQL_ARRAY:
		return IB_METADATA_ARRAY;
	default:
		return IB_METADATA_UNKNOWN;
	}
}

static int ib_catalog_sql_type(int type)
{
	switch (type) {
	case 7:
		return SQL_SHORT;
	case 8:
		return SQL_LONG;
	case 10:
		return SQL_FLOAT;
	case 12:
		return SQL_TYPE_DATE;
	case 13:
		return SQL_TYPE_TIME;
	case 14:
		return SQL_TEXT;
	case 16:
		return SQL_INT64;
	case 17:
	case 23:
		return SQL_BOOLEAN;
	case 27:
		return SQL_DOUBLE;
	case 35:
		return SQL_TIMESTAMP;
	case 37:
		return SQL_VARYING;
	case 261:
		return SQL_BLOB;
	default:
		return 0;
	}
}

static short ib_text_charset(const XSQLVAR *variable)
{
	if (variable == NULL) {
		return 0;
	}
	/* The low byte is the charset; the high byte may carry collation. */
	return (short) ((unsigned short) variable->sqlsubtype & 0x00ffU);
}

static int ib_validate_length(short length, size_t extra, size_t *size,
	char **error)
{
	size_t value;

	if (length < 0) {
		return ib_fail(error, "InterBase returned an invalid SQLDA length");
	}
	value = (size_t) length;
	if (value > IB_MAX_SQLDA_BUFFER || extra > IB_MAX_SQLDA_BUFFER - value) {
		return ib_fail(error, "InterBase returned an unsupported SQLDA length");
	}
	*size = value + extra;
	if (*size == 0U) {
		*size = 1U;
	}
	return 0;
}

static int ib_value_buffer_size(const XSQLVAR *variable, size_t *size,
	char **error)
{
	switch (ib_sql_type(variable)) {
	case SQL_TEXT:
		return ib_validate_length(variable->sqllen, 0U, size, error);
	case SQL_VARYING:
		return ib_validate_length(variable->sqllen, sizeof(unsigned short), size,
			error);
	case SQL_BLOB:
		*size = sizeof(ISC_QUAD);
		return 0;
	case SQL_ARRAY:
		*size = sizeof(ISC_QUAD);
		return 0;
	case SQL_SHORT:
		*size = sizeof(short);
		return 0;
	case SQL_LONG:
		*size = sizeof(ISC_LONG);
		return 0;
	case SQL_INT64:
		*size = sizeof(ISC_INT64);
		return 0;
	case SQL_FLOAT:
		*size = sizeof(float);
		return 0;
	case SQL_DOUBLE:
		*size = sizeof(double);
		return 0;
	case SQL_D_FLOAT:
		*size = sizeof(double);
		return 0;
	case SQL_TIMESTAMP:
		*size = sizeof(ISC_TIMESTAMP);
		return 0;
	case SQL_TYPE_DATE:
		*size = sizeof(ISC_DATE);
		return 0;
	case SQL_TYPE_TIME:
		*size = sizeof(ISC_TIME);
		return 0;
	case SQL_BOOLEAN:
		*size = sizeof(ISC_BOOLEAN);
		return 0;
	default:
		return ib_fail(error, "the parameter type is unsupported by the proof of concept");
	}
}

static int ib_allocate_variable(XSQLVAR *variable, size_t size, char **error)
{
	variable->sqldata = (char *) calloc(1U, size == 0U ? 1U : size);
	variable->sqlind = (short *) malloc(sizeof(short));
	if (variable->sqldata == NULL || variable->sqlind == NULL) {
		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		return ib_fail(error, "out of memory allocating SQLDA value storage");
	}
	*variable->sqlind = 0;
	return 0;
}

static int ib_describe_bind(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	XSQLDA *sqlda;
	short count;

	sqlda = ib_alloc_sqlda(1);
	if (sqlda == NULL) {
		return ib_fail(error, "out of memory allocating input SQLDA");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_describe_bind(status, &cursor->statement,
		ib_connection_dialect(cursor->connection), sqlda);
	if (result != 0) {
		ib_free_sqlda(sqlda);
		return ib_fail_status(error, "describe bind", status);
	}
	if (sqlda->sqld < 0) {
		ib_free_sqlda(sqlda);
		return ib_fail(error, "InterBase returned an invalid input SQLDA count");
	}
	if (sqlda->sqld > sqlda->sqln) {
		count = sqlda->sqld;
		ib_free_sqlda(sqlda);
		sqlda = ib_alloc_sqlda(count);
		if (sqlda == NULL) {
			return ib_fail(error, "out of memory resizing input SQLDA");
		}
		memset(status, 0, sizeof(status));
		result = isc_dsql_describe_bind(status, &cursor->statement,
			ib_connection_dialect(cursor->connection), sqlda);
		if (result != 0) {
			ib_free_sqlda(sqlda);
			return ib_fail_status(error, "describe bind", status);
		}
		if (sqlda->sqld < 0 || sqlda->sqld > sqlda->sqln) {
			ib_free_sqlda(sqlda);
			return ib_fail(error, "InterBase returned an invalid input SQLDA count");
		}
	}
	cursor->input = sqlda;
	return 0;
}

static int ib_describe_output(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	XSQLDA *sqlda;
	short count;

	sqlda = ib_alloc_sqlda(1);
	if (sqlda == NULL) {
		return ib_fail(error, "out of memory allocating output SQLDA");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_describe(status, &cursor->statement,
		ib_connection_dialect(cursor->connection), sqlda);
	if (result != 0) {
		ib_free_sqlda(sqlda);
		return ib_fail_status(error, "describe output", status);
	}
	if (sqlda->sqld < 0) {
		ib_free_sqlda(sqlda);
		return ib_fail(error, "InterBase returned an invalid output SQLDA count");
	}
	if (sqlda->sqld > sqlda->sqln) {
		count = sqlda->sqld;
		ib_free_sqlda(sqlda);
		sqlda = ib_alloc_sqlda(count);
		if (sqlda == NULL) {
			return ib_fail(error, "out of memory resizing output SQLDA");
		}
		memset(status, 0, sizeof(status));
		result = isc_dsql_describe(status, &cursor->statement,
			ib_connection_dialect(cursor->connection), sqlda);
		if (result != 0) {
			ib_free_sqlda(sqlda);
			return ib_fail_status(error, "describe output", status);
		}
		if (sqlda->sqld < 0 || sqlda->sqld > sqlda->sqln) {
			ib_free_sqlda(sqlda);
			return ib_fail(error, "InterBase returned an invalid output SQLDA count");
		}
	}
	cursor->output = sqlda;
	return 0;
}

static int ib_statement_type(ib_cursor *cursor, int *statement_type,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char request[1];
	char response[64];
	ISC_LONG response_length;
	ISC_LONG server_statement_type;

	if (cursor == NULL || statement_type == NULL || cursor->statement == NULL) {
		return ib_fail(error, "statement is unavailable");
	}

	request[0] = (char) isc_info_sql_stmt_type;
	memset(response, 0, sizeof(response));
	memset(status, 0, sizeof(status));
	result = isc_dsql_sql_info(status, &cursor->statement, (short) sizeof(request),
		request, (short) sizeof(response), response);
	if (result != 0) {
		return ib_fail_status(error, "inspect statement type", status);
	}
	if ((unsigned char) response[0] != isc_info_sql_stmt_type) {
		return ib_fail(error, "InterBase did not return a statement type");
	}
	response_length = isc_vax_integer(response + 1, 2);
	if (response_length <= 0 || response_length > (ISC_LONG) sizeof(response) - 3) {
		return ib_fail(error, "InterBase returned an invalid statement type");
	}
	server_statement_type = isc_vax_integer(response + 3,
		(short) response_length);
	*statement_type = (int) server_statement_type;
	cursor->statement_type = *statement_type;
	return 0;
}

static int ib_statement_is_select(ib_cursor *cursor, char **error)
{
	int statement_type;

	if (ib_statement_type(cursor, &statement_type, error) != 0) {
		return -1;
	}
	if (statement_type != isc_info_sql_stmt_select &&
		statement_type != isc_info_sql_stmt_select_for_upd) {
		return ib_fail(error, "only SELECT statements are permitted");
	}
	return 0;
}

static int ib_statement_rejects_transaction_control_type(int statement_type,
	char **error)
{
	switch (statement_type) {
	case isc_info_sql_stmt_start_trans:
	case isc_info_sql_stmt_commit:
	case isc_info_sql_stmt_rollback:
		return ib_fail(error,
			"transaction-control SQL must use database/sql transaction methods");
	default:
		return 0;
	}
}

static int ib_statement_rejects_result(ib_cursor *cursor, char **error)
{
	int statement_type;

	if (ib_statement_type(cursor, &statement_type, error) != 0) {
		return -1;
	}
	if (statement_type == isc_info_sql_stmt_select ||
		statement_type == isc_info_sql_stmt_select_for_upd) {
		return ib_fail(error, "statement produces a result set; use Query instead");
	}
	if (statement_type == isc_info_sql_stmt_exec_procedure &&
		(cursor->output == NULL || cursor->output->sqld != 0)) {
		return ib_fail(error, "statement produces a result set; use Query instead");
	}
	return ib_statement_rejects_transaction_control_type(statement_type, error);
}

static int ib_statement_rows_affected(ib_cursor *cursor,
	int64_t *rows_affected, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	const char request[] = { isc_info_sql_records, isc_info_end };
	char response[64];
	ISC_LONG response_length;
	size_t offset;
	size_t end;
	int requested_type;

	if (cursor == NULL || rows_affected == NULL || cursor->statement == NULL) {
		return ib_fail(error, "statement result is unavailable");
	}
	*rows_affected = -1;
	switch (cursor->statement_type) {
	case isc_info_sql_stmt_insert:
		requested_type = isc_info_req_insert_count;
		break;
	case isc_info_sql_stmt_update:
		requested_type = isc_info_req_update_count;
		break;
	case isc_info_sql_stmt_delete:
		requested_type = isc_info_req_delete_count;
		break;
	default:
		return 0;
	}

	memset(response, 0, sizeof(response));
	memset(status, 0, sizeof(status));
	result = isc_dsql_sql_info(status, &cursor->statement,
		(short) sizeof(request), (char *) request, (short) sizeof(response), response);
	if (result != 0) {
		return ib_fail_status(error, "inspect affected rows", status);
	}
	if ((unsigned char) response[0] == isc_info_truncated) {
		return ib_fail(error, "InterBase returned a truncated affected-row count");
	}
	if ((unsigned char) response[0] != isc_info_sql_records) {
		return ib_fail(error, "InterBase did not return affected-row information");
	}
	response_length = isc_vax_integer(response + 1, 2);
	if (response_length < 1 || response_length > (ISC_LONG) sizeof(response) - 3) {
		return ib_fail(error, "InterBase returned an invalid affected-row response");
	}
	offset = 3U;
	end = offset + (size_t) response_length;
	while (offset < end) {
		unsigned char item;
		ISC_LONG item_length;
		ISC_LONG count;

		item = (unsigned char) response[offset++];
		if (item == isc_info_end) {
			break;
		}
		if (end - offset < 2U) {
			return ib_fail(error, "InterBase returned a malformed affected-row response");
		}
		item_length = isc_vax_integer(response + offset, 2);
		offset += 2U;
		if (item_length < 0 || (size_t) item_length > end - offset ||
			item_length > (ISC_LONG) sizeof(ISC_LONG)) {
			return ib_fail(error, "InterBase returned an invalid affected-row item");
		}
		if ((int) item == requested_type) {
			count = isc_vax_integer(response + offset, (short) item_length);
			if (count < 0) {
				return ib_fail(error, "InterBase returned a negative affected-row count");
			}
			*rows_affected = (int64_t) count;
		}
		offset += (size_t) item_length;
	}
	return 0;
}

static int ib_cursor_prepare_statement(ib_cursor *cursor, const char *query,
	size_t query_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		return ib_fail(error, "statement preparation is unavailable");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &cursor->connection->database,
		&cursor->statement);
	if (result != 0) {
		return ib_fail_status(error, "allocate statement", status);
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &cursor->transaction, &cursor->statement,
		(unsigned short) query_length, (char *) query,
		ib_connection_dialect(cursor->connection), NULL);
	if (result != 0) {
		return ib_fail_status(error, "prepare statement", status);
	}
	return 0;
}

static int ib_cursor_drop_statement(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	int failed;

	if (cursor == NULL) {
		return 0;
	}
	failed = 0;
	if (cursor->statement != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &cursor->statement, DSQL_drop);
		if (result == 0) {
			cursor->statement = NULL;
		} else {
			failed = 1;
			if (cursor->connection != NULL) {
				ib_mark_broken(cursor->connection);
			}
			(void) ib_fail_status(error, "close statement", status);
		}
	}
	ib_free_sqlda(cursor->input);
	ib_free_sqlda(cursor->output);
	cursor->input = NULL;
	cursor->output = NULL;
	free(cursor->metadata);
	cursor->metadata = NULL;
	free(cursor->blob_charsets);
	cursor->blob_charsets = NULL;
	free(cursor->blob_charset_known);
	cursor->blob_charset_known = NULL;
	return failed ? -1 : 0;
}

static int ib_cursor_rollback_owned_transaction(ib_cursor *cursor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || !cursor->owns_transaction || cursor->transaction == NULL) {
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_rollback_transaction(status, &cursor->transaction);
	cursor->transaction = NULL;
	cursor->owns_transaction = 0;
	if (result != 0) {
		if (cursor->connection != NULL) {
			ib_mark_broken(cursor->connection);
		}
		return ib_fail_status(error, "rollback transaction", status);
	}
	return 0;
}

static int ib_set_varying(XSQLVAR *variable, const char *value, size_t length,
	short source_charset, short connection_charset, int binary, short nullable,
	char **error)
{
	int described_type;
	int preserve_described;
	short target_charset;
	short described_length;
	int result_type;
	char *converted;
	size_t converted_length;
	size_t extra;
	size_t size;

	described_type = ib_sql_type(variable);
	described_length = variable->sqllen;
	preserve_described = (described_length > 0 &&
		(described_type == SQL_TEXT || described_type == SQL_VARYING));
	target_charset = binary ? 1 :
		(preserve_described ? ib_text_charset(variable) : connection_charset);
	converted = ib_convert_charset(value, length, source_charset, target_charset,
		&converted_length, error);
	if (converted == NULL) {
		return -1;
	}
	if (described_length < 0 || converted_length > (size_t) SHRT_MAX) {
		free(converted);
		return ib_fail(error, "string argument exceeds the described SQLDA length");
	}
	if (preserve_described) {
		extra = described_type == SQL_VARYING ? sizeof(unsigned short) : 0U;
		if (converted_length > (size_t) described_length) {
			free(converted);
			return ib_fail(error, "string argument exceeds the described SQLDA length");
		}
		if ((size_t) described_length > IB_MAX_SQLDA_BUFFER - extra) {
			free(converted);
			return ib_fail(error, "string argument exceeds the SQLDA storage limit");
		}
		size = (size_t) described_length + extra;
		if (size == 0U) {
			size = 1U;
		}
		variable->sqltype = (short) (described_type | nullable);
		variable->sqllen = described_length;
	} else {
		if (converted_length > IB_MAX_SQLDA_BUFFER) {
			free(converted);
			return ib_fail(error, "string argument exceeds the SQLDA storage limit");
		}
		result_type = described_type == SQL_VARYING ? SQL_VARYING : SQL_TEXT;
		extra = result_type == SQL_VARYING ? sizeof(unsigned short) : 0U;
		if (converted_length > IB_MAX_SQLDA_BUFFER - extra) {
			free(converted);
			return ib_fail(error, "string argument exceeds the SQLDA storage limit");
		}
		size = converted_length + extra;
		if (size == 0U) {
			size = 1U;
		}
		variable->sqltype = (short) (result_type | nullable);
		variable->sqlsubtype = binary ? 1 : connection_charset;
		variable->sqllen = (short) converted_length;
	}
	if (ib_allocate_variable(variable, size, error) != 0) {
		free(converted);
		return -1;
	}
	if (preserve_described && described_type == SQL_TEXT) {
		memset(variable->sqldata, ' ', size);
	}
	if (ib_sql_type(variable) == SQL_VARYING) {
		unsigned short varying_length = (unsigned short) converted_length;
		memcpy(variable->sqldata, &varying_length, sizeof(varying_length));
		if (converted_length != 0U) {
			memcpy(variable->sqldata + sizeof(varying_length), converted,
				converted_length);
		}
	} else if (converted_length != 0U) {
		memcpy(variable->sqldata, converted, converted_length);
	}
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	free(converted);
	return 0;
}

static int ib_is_scaled_integer(const XSQLVAR *variable)
{
	int type;

	if (variable == NULL) {
		return 0;
	}
	type = ib_sql_type(variable);
	return (type == SQL_SHORT || type == SQL_LONG || type == SQL_INT64) &&
		(variable->sqlscale != 0 || variable->sqlsubtype == 1 ||
			variable->sqlsubtype == 2 || variable->sqlprecision > 0);
}

static uint64_t ib_scaled_integer_limit(const XSQLVAR *variable, int negative)
{
	uint64_t limit;
	uint64_t power;
	int precision;
	int index;

	limit = negative ? (uint64_t) INT64_MAX + UINT64_C(1) :
		(uint64_t) INT64_MAX;
	precision = variable == NULL ? 0 : (int) variable->sqlprecision;
	if (precision <= 0) {
		return limit;
	}
	power = UINT64_C(1);
	for (index = 0; index < precision; index++) {
		if (power > limit / UINT64_C(10)) {
			return limit;
		}
		power *= UINT64_C(10);
	}
	if (power == 0U || power - UINT64_C(1) >= limit) {
		return limit;
	}
	return power - UINT64_C(1);
}

static int ib_parse_scaled_integer(const char *value, size_t length,
	const XSQLVAR *variable, int64_t *result, char **error)
{
	size_t offset;
	size_t start;
	size_t digit_count;
	size_t fractional_digits;
	size_t discarded_digits;
	size_t effective_digits;
	size_t padding;
	size_t digit_index;
	int negative;
	int decimal_point;
	int seen_digit;
	int scale;
	int target_fraction;
	uint64_t magnitude;
	uint64_t limit;

	if (value == NULL || length == 0U || variable == NULL || result == NULL) {
		return ib_fail(error, "decimal string is unavailable");
	}
	offset = 0U;
	negative = 0;
	if (value[offset] == '-' || value[offset] == '+') {
		negative = value[offset] == '-';
		offset++;
	}
	start = offset;
	digit_count = 0U;
	fractional_digits = 0U;
	decimal_point = 0;
	seen_digit = 0;
	for (; offset < length; offset++) {
		unsigned char character = (unsigned char) value[offset];

		if (character >= (unsigned char) '0' && character <= (unsigned char) '9') {
			seen_digit = 1;
			if (digit_count == SIZE_MAX) {
				return ib_fail(error, "decimal string is too long");
			}
			digit_count++;
			if (decimal_point) {
				if (fractional_digits == SIZE_MAX) {
					return ib_fail(error, "decimal string is too long");
				}
				fractional_digits++;
			}
		} else if (character == (unsigned char) '.' && !decimal_point) {
			decimal_point = 1;
		} else {
			return ib_fail(error,
				"decimal string contains unsupported syntax; use ordinary decimal notation");
		}
	}
	if (!seen_digit) {
		return ib_fail(error, "decimal string must contain a digit");
	}

	scale = (int) variable->sqlscale;
	target_fraction = scale < 0 ? -scale : 0;
	discarded_digits = 0U;
	if (fractional_digits > (size_t) target_fraction) {
		discarded_digits = fractional_digits - (size_t) target_fraction;
		for (offset = length; discarded_digits > 0U; offset--, discarded_digits--) {
			if (value[offset - 1U] != '0') {
				return ib_fail(error,
					"decimal string has a nonzero fraction beyond the described scale");
			}
		}
		discarded_digits = fractional_digits - (size_t) target_fraction;
	}
	effective_digits = digit_count - discarded_digits;
	padding = (size_t) target_fraction > fractional_digits ?
		(size_t) target_fraction - fractional_digits : 0U;
	if (scale > 0) {
		if (padding > SIZE_MAX - (size_t) scale) {
			return ib_fail(error, "decimal string is too large for the described scale");
		}
		padding += (size_t) scale;
	}
	limit = ib_scaled_integer_limit(variable, negative);
	magnitude = 0U;
	digit_index = 0U;
	for (offset = start; offset < length; offset++) {
		unsigned char character = (unsigned char) value[offset];
		uint64_t digit;

		if (character < (unsigned char) '0' || character > (unsigned char) '9') {
			continue;
		}
		if (digit_index < effective_digits) {
			digit = (uint64_t) (character - (unsigned char) '0');
			if (magnitude > (limit - digit) / UINT64_C(10)) {
				return ib_fail(error, "decimal string is outside the supported range");
			}
			magnitude = magnitude * UINT64_C(10) + digit;
		}
		digit_index++;
	}
	for (offset = 0U; offset < padding; offset++) {
		if (magnitude > limit / UINT64_C(10)) {
			return ib_fail(error, "decimal string is outside the supported range");
		}
		magnitude *= UINT64_C(10);
	}
	if (negative) {
		if (magnitude == (uint64_t) INT64_MAX + UINT64_C(1)) {
			*result = INT64_MIN;
		} else {
			*result = -(int64_t) magnitude;
		}
	} else {
		*result = (int64_t) magnitude;
	}
	return 0;
}

static int ib_set_scaled_integer(XSQLVAR *variable, int64_t value,
	short nullable, char **error)
{
	int type;
	short short_value = 0;
	ISC_LONG long_value = 0;
	ISC_INT64 int64_value = 0;
	size_t size;

	if (variable == NULL) {
		return ib_fail(error, "decimal parameter is unavailable");
	}
	type = ib_sql_type(variable);
	switch (type) {
	case SQL_SHORT:
		if (value < (int64_t) SHRT_MIN || value > (int64_t) SHRT_MAX) {
			return ib_fail(error, "decimal string is outside the parameter range");
		}
		short_value = (short) value;
		size = sizeof(short_value);
		break;
	case SQL_LONG:
		if (value < (int64_t) INT32_MIN || value > (int64_t) INT32_MAX) {
			return ib_fail(error, "decimal string is outside the parameter range");
		}
		long_value = (ISC_LONG) value;
		size = sizeof(long_value);
		break;
	case SQL_INT64:
		int64_value = (ISC_INT64) value;
		size = sizeof(int64_value);
		break;
	default:
		return ib_fail(error, "decimal parameter has an unsupported storage type");
	}
	variable->sqltype = (short) (type | nullable);
	variable->sqllen = (short) size;
	if (ib_allocate_variable(variable, size, error) != 0) {
		return -1;
	}
	switch (type) {
	case SQL_SHORT:
		memcpy(variable->sqldata, &short_value, sizeof(short_value));
		break;
	case SQL_LONG:
		memcpy(variable->sqldata, &long_value, sizeof(long_value));
		break;
	case SQL_INT64:
		memcpy(variable->sqldata, &int64_value, sizeof(int64_value));
		break;
	default:
		break;
	}
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	return 0;
}

static int ib_set_temporal(XSQLVAR *variable, const ib_bind_value *value,
	short nullable, char **error)
{
	int type;
	struct tm encoded;
	ISC_TIMESTAMP timestamp;
	ISC_DATE date;
	ISC_TIME time_value;
	void *data;
	size_t size;

	if (variable == NULL || value == NULL) {
		return ib_fail(error, "temporal parameter is unavailable");
	}
	type = ib_sql_type(variable);
	if (type == SQL_TYPE_DATE || type == SQL_TIMESTAMP) {
		if (value->year < INT64_C(1) || value->year > INT64_C(9999)) {
			return ib_fail(error, "temporal parameter year is outside the supported range");
		}
	}
	memset(&encoded, 0, sizeof(encoded));
	if (type == SQL_TYPE_TIME) {
		/* SQL TIME has no date component; use a stable valid date for the encoder. */
		encoded.tm_year = 0;
		encoded.tm_mon = 0;
		encoded.tm_mday = 1;
	} else {
		/* The range check above makes this conversion safe for struct tm. */
		encoded.tm_year = (int) (value->year - INT64_C(1900));
		encoded.tm_mon = value->month - 1;
		encoded.tm_mday = value->day;
	}
	encoded.tm_hour = value->hour;
	encoded.tm_min = value->minute;
	encoded.tm_sec = value->second;
	data = NULL;
	size = 0U;
	switch (type) {
	case SQL_TYPE_DATE:
		isc_encode_sql_date(&encoded, &date);
		data = &date;
		size = sizeof(date);
		break;
	case SQL_TYPE_TIME:
		isc_encode_sql_time(&encoded, &time_value);
		time_value += (ISC_TIME) (value->nanosecond / 100000);
		if (time_value >= (ISC_TIME) (24U * 60U * 60U * ISC_TIME_SECONDS_PRECISION)) {
			return ib_fail(error, "temporal parameter is outside the TIME range");
		}
		data = &time_value;
		size = sizeof(time_value);
		break;
	case SQL_TIMESTAMP:
		isc_encode_timestamp(&encoded, &timestamp);
		timestamp.timestamp_time += (ISC_TIME) (value->nanosecond / 100000);
		if (timestamp.timestamp_time >=
			(ISC_TIME) (24U * 60U * 60U * ISC_TIME_SECONDS_PRECISION)) {
			return ib_fail(error, "temporal parameter is outside the TIMESTAMP range");
		}
		data = &timestamp;
		size = sizeof(timestamp);
		break;
	default:
		return ib_fail(error, "temporal parameter has an unsupported storage type");
	}
	variable->sqltype = (short) (type | nullable);
	variable->sqllen = (short) size;
	if (ib_allocate_variable(variable, size, error) != 0) {
		return -1;
	}
	memcpy(variable->sqldata, data, size);
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	return 0;
}

static int ib_blob_cleanup(isc_blob_handle *blob, int cancel, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	int failed;

	/* A native cleanup failure is actionable even when its message cannot be allocated. */
	if (blob == NULL || *blob == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	memset(status, 0, sizeof(status));
	if (cancel) {
		result = isc_cancel_blob(status, blob);
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error, "cancel blob", status);
			memset(status, 0, sizeof(status));
			result = isc_close_blob(status, blob);
			if (result != 0) {
				char *close_error = NULL;

				failed = 1;
				(void) ib_fail_status(&close_error, "close blob", status);
				ib_append_error(&first_error, close_error);
			}
		}
	} else {
		result = isc_close_blob(status, blob);
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error, "close blob", status);
		}
	}
	*blob = NULL;
	ib_give_error(error, first_error);
	return failed != 0 ? -1 : 0;
}

static void ib_blob_descriptor(ISC_BLOB_DESC_V2 *descriptor, short subtype,
	short charset)
{
	memset(descriptor, 0, sizeof(*descriptor));
	descriptor->blob_desc_version = BLB_DESC_VERSION2;
	descriptor->blob_desc_subtype = subtype;
	descriptor->blob_desc_charset = charset;
	descriptor->blob_desc_segment_size = (short) IB_BLOB_SEGMENT_LENGTH;
}

static int ib_blob_generate_bpb(short source_charset, short target_charset,
	unsigned char *bpb, unsigned short *bpb_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_BLOB_DESC_V2 source;
	ISC_BLOB_DESC_V2 target;
	ISC_STATUS result;

	if (bpb == NULL || bpb_length == NULL) {
		return ib_fail(error, "BLOB parameter block storage is unavailable");
	}
	ib_blob_descriptor(&source, 1, source_charset);
	ib_blob_descriptor(&target, 1, target_charset);
	memset(status, 0, sizeof(status));
	/* isc_blob_gen_bpb2 takes the target descriptor before the source. */
	result = isc_blob_gen_bpb2(status, &target, &source, IB_BLOB_BPB_LENGTH, bpb,
		bpb_length);
	if (result != 0) {
		return ib_fail_status(error, "generate BLOB parameter block", status);
	}
	return 0;
}

static int ib_blob_generate_bpb_options(short subtype, short charset,
	unsigned char *bpb, unsigned short *bpb_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_BLOB_DESC_V2 source;
	ISC_BLOB_DESC_V2 target;
	ISC_STATUS result;
	short source_charset;

	if (bpb == NULL || bpb_length == NULL) {
		return ib_fail(error, "BLOB parameter block storage is unavailable");
	}
	if (subtype != 1) {
		charset = 0;
	}
	source_charset = subtype == 1 ? IB_CHARSET_UTF8 : 0;
	ib_blob_descriptor(&source, subtype, source_charset);
	ib_blob_descriptor(&target, subtype, charset);
	memset(status, 0, sizeof(status));
	result = isc_blob_gen_bpb2(status, &target, &source, IB_BLOB_BPB_LENGTH,
		bpb, bpb_length);
	if (result != 0) {
		return ib_fail_status(error, "generate BLOB parameter block", status);
	}
	return 0;
}

static int ib_blob_lookup_descriptor(ib_cursor *cursor,
	const XSQLVAR *variable, ISC_BLOB_DESC_V2 *descriptor, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	unsigned char field_name[METADATALENGTH + 1U];
	unsigned char relation_name[METADATALENGTH + 1U];

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		variable == NULL || descriptor == NULL || variable->sqlname_length <= 0 ||
		variable->relname_length <= 0) {
		return ib_fail(error, "BLOB column metadata is unavailable");
	}
	if ((size_t) variable->sqlname_length > METADATALENGTH ||
		(size_t) variable->relname_length > METADATALENGTH) {
		return ib_fail(error, "BLOB column metadata is too long");
	}
	memset(field_name, 0, sizeof(field_name));
	memset(relation_name, 0, sizeof(relation_name));
	memcpy(field_name, variable->sqlname, (size_t) variable->sqlname_length);
	memcpy(relation_name, variable->relname,
		(size_t) variable->relname_length);
	ib_blob_descriptor(descriptor, 1, 0);
	memset(status, 0, sizeof(status));
	result = isc_blob_lookup_desc2(status, &cursor->connection->database,
		&cursor->transaction, relation_name, field_name, descriptor, NULL);
	if (result != 0) {
		return ib_fail_status(error, "lookup BLOB column descriptor", status);
	}
	return 0;
}

enum ib_sql_token_kind {
	IB_SQL_TOKEN_WORD = 1,
	IB_SQL_TOKEN_PARAMETER = 2,
	IB_SQL_TOKEN_OPEN = 3,
	IB_SQL_TOKEN_CLOSE = 4,
	IB_SQL_TOKEN_COMMA = 5,
	IB_SQL_TOKEN_DOT = 6,
	IB_SQL_TOKEN_EQUAL = 7,
	IB_SQL_TOKEN_OTHER = 8,
	IB_SQL_TOKEN_LITERAL = 9
};

typedef struct ib_sql_token {
	const char *start;
	size_t length;
	int kind;
	int quoted;
} ib_sql_token;

static int ib_sql_token_is_word(const ib_sql_token *token, const char *word)
{
	size_t index;
	size_t length;

	if (token == NULL || word == NULL || token->kind != IB_SQL_TOKEN_WORD ||
		token->quoted) {
		return 0;
	}
	length = strlen(word);
	if (token->length != length) {
		return 0;
	}
	for (index = 0U; index < length; index++) {
		if (tolower((unsigned char) token->start[index]) !=
			tolower((unsigned char) word[index])) {
			return 0;
		}
	}
	return 1;
}

static int ib_sql_tokenize(const char *query, size_t query_length,
	ib_sql_token **tokens, size_t *token_count, char **error)
{
	ib_sql_token *result;
	size_t capacity;
	size_t count;
	size_t offset;

	if (query == NULL || tokens == NULL || token_count == NULL) {
		return ib_fail(error, "SQL token storage is unavailable");
	}
	if (query_length > SIZE_MAX / sizeof(*result) - 1U) {
		return ib_fail(error, "SQL query token storage is too large");
	}
	capacity = query_length + 1U;
	result = (ib_sql_token *) calloc(capacity, sizeof(*result));
	if (result == NULL) {
		return ib_fail(error, "out of memory tokenizing SQL query");
	}
	count = 0U;
	offset = 0U;
	while (offset < query_length) {
		unsigned char value;
		size_t start;

		while (offset < query_length) {
			value = (unsigned char) query[offset];
			if (isspace(value)) {
				offset++;
				continue;
			}
			if (value == '-' && offset + 1U < query_length &&
				query[offset + 1U] == '-') {
				offset += 2U;
				while (offset < query_length && query[offset] != '\n') {
					offset++;
				}
				continue;
			}
			if (value == '/' && offset + 1U < query_length &&
				query[offset + 1U] == '*') {
				offset += 2U;
				while (offset + 1U < query_length &&
					!(query[offset] == '*' && query[offset + 1U] == '/')) {
					offset++;
				}
				if (offset + 1U >= query_length) {
					free(result);
					return ib_fail(error, "SQL query contains an unterminated comment");
				}
				offset += 2U;
				continue;
			}
			break;
		}
		if (offset >= query_length) {
			break;
		}
		if (count >= capacity - 1U) {
			free(result);
			return ib_fail(error, "SQL query has too many tokens");
		}
		value = (unsigned char) query[offset];
		if (value == '\'') {
			start = ++offset;
			while (offset < query_length) {
				if (query[offset] == '\'') {
					if (offset + 1U < query_length && query[offset + 1U] == '\'') {
						offset += 2U;
						continue;
					}
					result[count++] = (ib_sql_token) {
						query + start, offset - start, IB_SQL_TOKEN_LITERAL, 0};
					offset++;
					break;
				}
				offset++;
			}
			if (offset > query_length ||
				(offset == query_length && query[offset - 1U] != '\'')) {
				free(result);
				return ib_fail(error, "SQL query contains an unterminated string");
			}
			continue;
		}
		if (value == '"') {
			start = ++offset;
			while (offset < query_length) {
				if (query[offset] == '"') {
					if (offset + 1U < query_length && query[offset + 1U] == '"') {
						offset += 2U;
						continue;
					}
					result[count++] = (ib_sql_token) {
						query + start, offset - start, IB_SQL_TOKEN_WORD, 1};
					offset++;
					break;
				}
				offset++;
			}
			if (offset > query_length ||
				(offset == query_length && query[offset - 1U] != '"')) {
				free(result);
				return ib_fail(error, "SQL query contains an unterminated identifier");
			}
			continue;
		}
		if (value == '?') {
			result[count++] = (ib_sql_token) {query + offset, 1U,
				IB_SQL_TOKEN_PARAMETER, 0};
			offset++;
			continue;
		}
		if (value == '(' || value == ')' || value == ',' || value == '.' ||
			value == '=' || value == ';') {
			int kind = value == '(' ? IB_SQL_TOKEN_OPEN :
				value == ')' ? IB_SQL_TOKEN_CLOSE :
				value == ',' ? IB_SQL_TOKEN_COMMA :
				value == '.' ? IB_SQL_TOKEN_DOT :
				value == '=' ? IB_SQL_TOKEN_EQUAL : IB_SQL_TOKEN_OTHER;
			result[count++] = (ib_sql_token) {query + offset, 1U, kind, 0};
			offset++;
			continue;
		}
		start = offset;
		while (offset < query_length) {
			value = (unsigned char) query[offset];
			if (isspace(value) || value == '\'' || value == '"' || value == '?' ||
				value == '(' || value == ')' || value == ',' || value == '.' ||
				value == '=' || value == ';' || value == ':' || value == '+' ||
				value == '-' || value == '*' || value == '/' || value == '<' ||
				value == '>') {
				break;
			}
			offset++;
		}
		if (offset == start) {
			result[count++] = (ib_sql_token) {query + offset, 1U,
				IB_SQL_TOKEN_OTHER, 0};
			offset++;
		} else {
			result[count++] = (ib_sql_token) {query + start, offset - start,
				IB_SQL_TOKEN_WORD, 0};
		}
	}
	*tokens = result;
	*token_count = count;
	return 0;
}

static int ib_sql_select_expression_end(const ib_sql_token *tokens,
	size_t token_count, size_t start, size_t *end)
{
	size_t position;
	size_t depth;

	if (tokens == NULL || end == NULL || start >= token_count) {
		return -1;
	}
	position = start;
	depth = 0U;
	while (position < token_count) {
		const ib_sql_token *token = &tokens[position];

		if (depth == 0U && (token->kind == IB_SQL_TOKEN_COMMA ||
			(token->kind == IB_SQL_TOKEN_OTHER && token->length == 1U &&
				token->start[0] == ';') ||
			ib_sql_token_is_word(token, "FROM"))) {
			break;
		}
		if (token->kind == IB_SQL_TOKEN_OPEN) {
			depth++;
		} else if (token->kind == IB_SQL_TOKEN_CLOSE) {
			if (depth == 0U) {
				return -1;
			}
			depth--;
		}
		position++;
	}
	if (depth != 0U) {
		return -1;
	}
	*end = position;
	return 0;
}

static int ib_sql_identifier_is_valid(const ib_sql_token *token)
{
	size_t index;
	unsigned char value;

	if (token == NULL || token->kind != IB_SQL_TOKEN_WORD || token->length == 0U) {
		return 0;
	}
	if (token->quoted) {
		for (index = 0U; index < token->length; index++) {
			value = (unsigned char) token->start[index];
			if (value == '"') {
				if (index + 1U >= token->length || token->start[index + 1U] != '"') {
					return 0;
				}
				index++;
			}
		}
		return 1;
	}
	value = (unsigned char) token->start[0];
	if (!(isalpha(value) || value == '_' || value == '$')) {
		return 0;
	}
	for (index = 1U; index < token->length; index++) {
		value = (unsigned char) token->start[index];
		if (!(isalnum(value) || value == '_' || value == '$')) {
			return 0;
		}
	}
	return 1;
}

static int ib_sql_positive_integer(const ib_sql_token *token, size_t *value)
{
	size_t index;
	size_t parsed;
	unsigned char digit;

	if (token == NULL || value == NULL || token->kind != IB_SQL_TOKEN_WORD ||
		token->length == 0U) {
		return 0;
	}
	parsed = 0U;
	for (index = 0U; index < token->length; index++) {
		digit = (unsigned char) token->start[index];
		if (digit < '0' || digit > '9' ||
			parsed > (size_t) INT_MAX / 10U ||
			(parsed == (size_t) INT_MAX / 10U &&
				(size_t) (digit - '0') > (size_t) INT_MAX % 10U)) {
			return 0;
		}
		parsed = parsed * 10U + (size_t) (digit - '0');
	}
	if (parsed == 0U) {
		return 0;
	}
	*value = parsed;
	return 1;
}

static int ib_sql_matching_close(const ib_sql_token *tokens, size_t limit,
	size_t open, size_t *close)
{
	size_t depth;
	size_t position;

	if (tokens == NULL || close == NULL || open >= limit ||
		tokens[open].kind != IB_SQL_TOKEN_OPEN) {
		return 0;
	}
	depth = 0U;
	for (position = open; position < limit; position++) {
		if (tokens[position].kind == IB_SQL_TOKEN_OPEN) {
			depth++;
		} else if (tokens[position].kind == IB_SQL_TOKEN_CLOSE) {
			if (depth == 0U) {
				return 0;
			}
			depth--;
			if (depth == 0U) {
				*close = position;
				return 1;
			}
		}
	}
	return 0;
}

static int ib_sql_fixed_char_cast(const ib_sql_token *tokens,
	size_t start, size_t end, size_t *declared_length)
{
	size_t argument_close;
	size_t as_position;
	size_t close;
	size_t depth;
	size_t position;
	size_t type;
	size_t value;

	if (tokens == NULL || declared_length == NULL || start >= end) {
		return 0;
	}
	*declared_length = 0U;
	if (end - start >= 2U && ib_sql_token_is_word(&tokens[end - 2U], "AS")) {
		if (!ib_sql_identifier_is_valid(&tokens[end - 1U])) {
			return 0;
		}
		end -= 2U;
	} else if (end - start >= 2U &&
		tokens[end - 1U].kind == IB_SQL_TOKEN_WORD &&
		tokens[end - 2U].kind == IB_SQL_TOKEN_CLOSE &&
		ib_sql_identifier_is_valid(&tokens[end - 1U])) {
		/* InterBase accepts a bare output alias as well as AS alias. */
		end--;
	}
	if (start >= end) {
		return 0;
	}
	while (tokens[start].kind == IB_SQL_TOKEN_OPEN) {
		if (!ib_sql_matching_close(tokens, end, start, &close)) {
			return 0;
		}
		if (close != end - 1U) {
			break;
		}
		start++;
		end = close;
		if (start >= end) {
			return 0;
		}
	}
	if (!ib_sql_token_is_word(&tokens[start], "CAST") ||
		start + 1U >= end || tokens[start + 1U].kind != IB_SQL_TOKEN_OPEN ||
		!ib_sql_matching_close(tokens, end, start + 1U, &argument_close) ||
		argument_close != end - 1U) {
		return 0;
	}
	as_position = SIZE_MAX;
	depth = 1U;
	for (position = start + 2U; position < argument_close; position++) {
		if (tokens[position].kind == IB_SQL_TOKEN_OPEN) {
			depth++;
		} else if (tokens[position].kind == IB_SQL_TOKEN_CLOSE) {
			if (depth <= 1U) {
				return 0;
			}
			depth--;
		} else if (depth == 1U && ib_sql_token_is_word(&tokens[position], "AS")) {
			if (as_position != SIZE_MAX) {
				return 0;
			}
			as_position = position;
		}
	}
	if (depth != 1U || as_position == SIZE_MAX || as_position == start + 2U) {
		return 0;
	}
	type = as_position + 1U;
	if (type >= argument_close ||
		(!ib_sql_token_is_word(&tokens[type], "CHAR") &&
			!ib_sql_token_is_word(&tokens[type], "CHARACTER"))) {
		return 0;
	}
	type++;
	if (type < argument_close && ib_sql_token_is_word(&tokens[type], "VARYING")) {
		return 0;
	}
	if (type >= argument_close || tokens[type].kind != IB_SQL_TOKEN_OPEN ||
		!ib_sql_matching_close(tokens, argument_close, type, &close) ||
		close != type + 2U ||
		!ib_sql_positive_integer(&tokens[type + 1U], &value)) {
		return 0;
	}
	type = close + 1U;
	if (type < argument_close) {
		if (type + 2U >= argument_close ||
			!ib_sql_token_is_word(&tokens[type], "CHARACTER") ||
			!ib_sql_token_is_word(&tokens[type + 1U], "SET") ||
			!ib_sql_identifier_is_valid(&tokens[type + 2U])) {
			return 0;
		}
		type += 3U;
	}
	if (type != argument_close) {
		return 0;
	}
	*declared_length = value;
	return 1;
}

static int ib_sql_select_expression_has_star(const ib_sql_token *tokens,
	size_t start, size_t end)
{
	size_t position;

	if (tokens == NULL) {
		return 0;
	}
	for (position = start; position < end; position++) {
		/* Treat every unquoted star in the SELECT list as potentially expanded.
		 * This is deliberately conservative: an arithmetic star or COUNT(*) can
		 * forfeit an optimization, but must never make column ordinals unsafe. */
		if (tokens[position].kind == IB_SQL_TOKEN_OTHER &&
			tokens[position].length == 1U && tokens[position].start[0] == '*') {
			return 1;
		}
	}
	return 0;
}

static size_t ib_sql_top_level_select(const ib_sql_token *tokens,
	size_t token_count)
{
	size_t position;
	size_t depth;

	if (tokens == NULL) {
		return SIZE_MAX;
	}
	depth = 0U;
	for (position = 0U; position < token_count; position++) {
		const ib_sql_token *token = &tokens[position];

		if (token->kind == IB_SQL_TOKEN_OPEN) {
			depth++;
			continue;
		}
		if (token->kind == IB_SQL_TOKEN_CLOSE) {
			if (depth == 0U) {
				return SIZE_MAX;
			}
			depth--;
			continue;
		}
		if (depth == 0U && ib_sql_token_is_word(token, "SELECT")) {
			return position;
		}
	}
	return SIZE_MAX;
}

static void ib_sql_describe_fixed_text_columns(const char *query,
	size_t query_length, size_t *fixed_text_lengths, size_t column_count)
{
	ib_sql_token *tokens;
	size_t token_count;
	size_t position;
	size_t column;
	size_t select_position;
	size_t expression_end;
	size_t declared_length;
	char *parse_error;
	int valid;

	if (query == NULL || query_length == 0U || fixed_text_lengths == NULL ||
		column_count == 0U) {
		return;
	}
	if (column_count > SIZE_MAX / sizeof(*fixed_text_lengths)) {
		return;
	}
	memset(fixed_text_lengths, 0, column_count * sizeof(*fixed_text_lengths));
	tokens = NULL;
	token_count = 0U;
	parse_error = NULL;
	if (ib_sql_tokenize(query, query_length, &tokens, &token_count,
		&parse_error) != 0) {
		free(parse_error);
		return;
	}
	select_position = ib_sql_top_level_select(tokens, token_count);
	valid = select_position != SIZE_MAX;
	for (position = 0U; valid && position < token_count; position++) {
		if (ib_sql_token_is_word(&tokens[position], "SELECT") &&
			position != select_position) {
			/* CTEs, derived tables, and scalar subqueries make the simple ordinal
			 * proof ambiguous. Unknown metadata is safer than a wrong truncation. */
			valid = 0;
		} else if (ib_sql_token_is_word(&tokens[position], "UNION") ||
			ib_sql_token_is_word(&tokens[position], "INTERSECT") ||
			ib_sql_token_is_word(&tokens[position], "EXCEPT") ||
			ib_sql_token_is_word(&tokens[position], "MINUS")) {
			valid = 0;
		}
	}
	if (!valid) {
		free(tokens);
		return;
	}
	position = select_position + 1U;
	while (position < token_count &&
		(ib_sql_token_is_word(&tokens[position], "DISTINCT") ||
			ib_sql_token_is_word(&tokens[position], "ALL"))) {
		position++;
	}
	column = 0U;
	while (valid && position < token_count) {
		if (column >= column_count ||
			ib_sql_select_expression_end(tokens, token_count, position,
				&expression_end) != 0 || expression_end == position ||
			ib_sql_select_expression_has_star(tokens, position, expression_end)) {
			valid = 0;
			break;
		}
		declared_length = 0U;
		if (ib_sql_fixed_char_cast(tokens, position, expression_end,
			&declared_length)) {
			fixed_text_lengths[column] = declared_length;
		}
		column++;
		position = expression_end;
		if (position >= token_count ||
			(tokens[position].kind == IB_SQL_TOKEN_OTHER &&
				tokens[position].length == 1U && tokens[position].start[0] == ';') ||
			ib_sql_token_is_word(&tokens[position], "FROM")) {
			break;
		}
		if (tokens[position].kind != IB_SQL_TOKEN_COMMA) {
			valid = 0;
			break;
		}
		position++;
	}
	if (!valid || column != column_count) {
		memset(fixed_text_lengths, 0, column_count * sizeof(*fixed_text_lengths));
	}
	free(tokens);
}

static int ib_sql_fixed_text_descriptor_compatible(const XSQLVAR *variable,
	size_t declared_length)
{
	size_t descriptor_length;

	if (variable == NULL || declared_length == 0U ||
		declared_length > (size_t) INT_MAX || ib_sql_type(variable) != SQL_TEXT ||
		variable->sqllen <= 0) {
		return 0;
	}
	descriptor_length = (size_t) variable->sqllen;
	return ib_text_character_count_for_charset(descriptor_length,
		ib_text_charset(variable)) >= declared_length;
}

static int ib_sql_copy_identifier(const ib_sql_token *token,
	char *destination, size_t capacity, size_t *length, char **error)
{
	size_t index;
	size_t used;

	if (token == NULL || destination == NULL || length == NULL ||
		token->kind != IB_SQL_TOKEN_WORD || token->length == 0U) {
		return ib_fail(error, "SQL identifier is unavailable");
	}
	used = 0U;
	for (index = 0U; index < token->length; index++) {
		unsigned char value = (unsigned char) token->start[index];

		if (token->quoted && value == '"' && index + 1U < token->length &&
			token->start[index + 1U] == '"') {
			index++;
		}
		if (used + 1U >= capacity) {
			return ib_fail(error, "SQL identifier is too long");
		}
		destination[used++] = (char) (token->quoted ? value : toupper(value));
	}
	destination[used] = '\0';
	*length = used;
	return 0;
}

static int ib_sql_copy_relation(const ib_sql_token *tokens, size_t token_count,
	size_t *position, char *relation, size_t relation_capacity, char **error)
{
	ib_sql_token token;
	size_t relation_length;

	if (position == NULL || *position >= token_count || relation == NULL) {
		return ib_fail(error, "BLOB destination relation is unavailable");
	}
	token = tokens[(*position)++];
	if (ib_sql_copy_identifier(&token, relation, relation_capacity,
		&relation_length, error) != 0) {
		return -1;
	}
	if (*position < token_count && tokens[*position].kind == IB_SQL_TOKEN_DOT) {
		(*position)++;
		if (*position >= token_count ||
			ib_sql_copy_identifier(&tokens[(*position)++], relation,
				relation_capacity, &relation_length, error) != 0) {
			return -1;
		}
		if (*position < token_count && tokens[*position].kind == IB_SQL_TOKEN_DOT) {
			return ib_fail(error, "BLOB destination relation is too deeply qualified");
		}
	}
	return 0;
}

static int ib_sql_expression_end(const ib_sql_token *tokens, size_t token_count,
	size_t start, int update, size_t *end, char **error)
{
	size_t position;
	size_t depth;

	if (tokens == NULL || end == NULL || start >= token_count) {
		return ib_fail(error, "BLOB parameter expression metadata is unavailable");
	}
	position = start;
	depth = 0U;
	while (position < token_count) {
		const ib_sql_token *token = &tokens[position];

		if (depth == 0U && (token->kind == IB_SQL_TOKEN_COMMA ||
			token->kind == IB_SQL_TOKEN_CLOSE ||
			(update && (ib_sql_token_is_word(token, "WHERE") ||
				ib_sql_token_is_word(token, "RETURNING") ||
				(token->kind == IB_SQL_TOKEN_OTHER && token->length == 1U &&
					token->start[0] == ';'))))) {
			break;
		}
		if (token->kind == IB_SQL_TOKEN_OPEN) {
			depth++;
		} else if (token->kind == IB_SQL_TOKEN_CLOSE) {
			if (depth == 0U) {
				break;
			}
			depth--;
		}
		position++;
	}
	if (depth != 0U) {
		return ib_fail(error, "BLOB parameter expression metadata is incomplete");
	}
	*end = position;
	return 0;
}

static int ib_sql_scan_parameter_expression(const ib_sql_token *tokens,
	size_t start, size_t end, size_t parameter_index, size_t *parameter_count,
	int *matched, char **error)
{
	size_t position;
	int found;

	if (tokens == NULL || parameter_count == NULL || matched == NULL ||
		start >= end) {
		return ib_fail(error, "BLOB parameter expression metadata is unavailable");
	}
	found = 0;
	for (position = start; position < end; position++) {
		if (tokens[position].kind != IB_SQL_TOKEN_PARAMETER) {
			continue;
		}
		if (*parameter_count == parameter_index) {
			found = 1;
		}
		if (*parameter_count == SIZE_MAX) {
			return ib_fail(error, "BLOB parameter index is too large");
		}
		(*parameter_count)++;
	}
	if (found && (end != start + 1U ||
		tokens[start].kind != IB_SQL_TOKEN_PARAMETER)) {
		return ib_fail(error,
			"BLOB parameter expression metadata is unavailable");
	}
	*matched = found;
	return 0;
}

static int ib_blob_destination_from_insert(const ib_sql_token *tokens,
	size_t token_count, size_t parameter_index, char *relation,
	char *field, char **error)
{
	ib_sql_token *columns;
	size_t column_count;
	size_t position;
	size_t value_parameter;

	if (token_count < 1U || !ib_sql_token_is_word(&tokens[0], "INSERT")) {
		return ib_fail(error, "BLOB INSERT destination metadata is unavailable");
	}
	position = 1U;
	if (position >= token_count || !ib_sql_token_is_word(&tokens[position], "INTO")) {
		return ib_fail(error, "BLOB INSERT destination metadata is unavailable");
	}
	position++;
	if (ib_sql_copy_relation(tokens, token_count, &position, relation,
		METADATALENGTH + 1U, error) != 0) {
		return -1;
	}
	if (position < token_count && ib_sql_token_is_word(&tokens[position], "AS")) {
		position += 2U;
	} else if (position + 1U < token_count &&
		tokens[position].kind == IB_SQL_TOKEN_WORD &&
		tokens[position + 1U].kind == IB_SQL_TOKEN_OPEN) {
		position++;
	}
	if (position >= token_count || tokens[position].kind != IB_SQL_TOKEN_OPEN) {
		return ib_fail(error, "BLOB INSERT column metadata is unavailable");
	}
	position++;
	columns = (ib_sql_token *) calloc(token_count + 1U, sizeof(*columns));
	if (columns == NULL) {
		return ib_fail(error, "out of memory storing BLOB INSERT columns");
	}
	column_count = 0U;
	for (;;) {
		if (position >= token_count || tokens[position].kind != IB_SQL_TOKEN_WORD ||
			column_count >= token_count) {
			free(columns);
			return ib_fail(error, "BLOB INSERT column metadata is invalid");
		}
		columns[column_count++] = tokens[position++];
		if (position >= token_count) {
			free(columns);
			return ib_fail(error, "BLOB INSERT column metadata is incomplete");
		}
		if (tokens[position].kind == IB_SQL_TOKEN_CLOSE) {
			position++;
			break;
		}
		if (tokens[position].kind != IB_SQL_TOKEN_COMMA) {
			free(columns);
			return ib_fail(error, "BLOB INSERT column metadata is invalid");
		}
		position++;
	}
	if (position >= token_count || !ib_sql_token_is_word(&tokens[position], "VALUES")) {
		free(columns);
		return ib_fail(error, "BLOB INSERT values metadata is unavailable");
	}
	position++;
	value_parameter = 0U;
	for (;;) {
		size_t column = 0U;

		if (position >= token_count || tokens[position].kind != IB_SQL_TOKEN_OPEN) {
			free(columns);
			return ib_fail(error, "BLOB INSERT values metadata is invalid");
		}
		position++;
		for (;;) {
			size_t expression_start = position;
			size_t expression_end;
			int matched;

			if (ib_sql_expression_end(tokens, token_count, expression_start, 0,
				&expression_end, error) != 0) {
				free(columns);
				return -1;
			}
			if (expression_end == expression_start ||
				ib_sql_scan_parameter_expression(tokens, expression_start, expression_end,
					parameter_index, &value_parameter, &matched, error) != 0) {
				free(columns);
				return -1;
			}
			if (matched) {
				int result;

				if (column >= column_count) {
					free(columns);
					return ib_fail(error, "BLOB INSERT parameter has no destination column");
				}
				result = ib_sql_copy_identifier(&columns[column], field,
					METADATALENGTH + 1U, &column, error);
				free(columns);
				return result;
			}
			position = expression_end;
			if (position >= token_count) {
				free(columns);
				return ib_fail(error, "BLOB INSERT values metadata is incomplete");
			}
			if (tokens[position].kind == IB_SQL_TOKEN_COMMA) {
				position++;
				column++;
				continue;
			}
			if (tokens[position].kind == IB_SQL_TOKEN_CLOSE) {
				position++;
				break;
			}
			free(columns);
			return ib_fail(error, "BLOB INSERT values metadata is invalid");
		}
		if (position < token_count && tokens[position].kind == IB_SQL_TOKEN_COMMA &&
			position + 1U < token_count &&
			tokens[position + 1U].kind == IB_SQL_TOKEN_OPEN) {
			position++;
			continue;
		}
		break;
	}
	free(columns);
	return ib_fail(error, "BLOB INSERT parameter destination metadata is unavailable");
}

static int ib_blob_destination_from_update(const ib_sql_token *tokens,
	size_t token_count, size_t parameter_index, char *relation,
	char *field, char **error)
{
	size_t position;
	size_t value_parameter;

	if (token_count < 1U || !ib_sql_token_is_word(&tokens[0], "UPDATE")) {
		return ib_fail(error, "BLOB UPDATE destination metadata is unavailable");
	}
	position = 1U;
	if (ib_sql_copy_relation(tokens, token_count, &position, relation,
		METADATALENGTH + 1U, error) != 0) {
		return -1;
	}
	while (position < token_count && !ib_sql_token_is_word(&tokens[position], "SET")) {
		position++;
	}
	if (position >= token_count) {
		return ib_fail(error, "BLOB UPDATE assignment metadata is unavailable");
	}
	position++;
	value_parameter = 0U;
	while (position < token_count) {
		ib_sql_token field_token;

		if (ib_sql_token_is_word(&tokens[position], "WHERE") ||
			ib_sql_token_is_word(&tokens[position], "RETURNING")) {
			break;
		}
		if (tokens[position].kind != IB_SQL_TOKEN_WORD) {
			return ib_fail(error, "BLOB UPDATE assignment metadata is invalid");
		}
		field_token = tokens[position++];
		if (position + 1U < token_count && tokens[position].kind == IB_SQL_TOKEN_DOT) {
			position++;
			if (tokens[position].kind != IB_SQL_TOKEN_WORD) {
				return ib_fail(error, "BLOB UPDATE assignment metadata is invalid");
			}
			field_token = tokens[position++];
		}
		if (position >= token_count || tokens[position].kind != IB_SQL_TOKEN_EQUAL) {
			return ib_fail(error, "BLOB UPDATE assignment metadata is invalid");
		}
		position++;
		{
			size_t expression_start = position;
			size_t expression_end;
			int matched;
			size_t ignored_length;

			if (ib_sql_expression_end(tokens, token_count, expression_start, 1,
				&expression_end, error) != 0) {
				return -1;
			}
			if (expression_end == expression_start ||
				ib_sql_scan_parameter_expression(tokens, expression_start, expression_end,
					parameter_index, &value_parameter, &matched, error) != 0) {
				return -1;
			}
			if (matched) {
				if (ib_sql_copy_identifier(&field_token, field,
					METADATALENGTH + 1U, &ignored_length, error) != 0) {
					return -1;
				}
				return 0;
			}
			position = expression_end;
		}
		if (position < token_count && tokens[position].kind == IB_SQL_TOKEN_COMMA) {
			position++;
			continue;
		}
		break;
	}
	return ib_fail(error, "BLOB UPDATE parameter destination metadata is unavailable");
}

static int ib_blob_destination_names(const ib_cursor *cursor,
	size_t parameter_index, char *relation, char *field, char **error)
{
	ib_sql_token *tokens;
	size_t token_count;
	int result;

	if (cursor == NULL || cursor->query == NULL || cursor->query_length == 0U) {
		return ib_fail(error, "BLOB parameter destination metadata is unavailable");
	}
	tokens = NULL;
	token_count = 0U;
	if (ib_sql_tokenize(cursor->query, cursor->query_length, &tokens,
		&token_count, error) != 0) {
		return -1;
	}
	if (token_count == 0U) {
		free(tokens);
		return ib_fail(error, "BLOB parameter destination metadata is unavailable");
	}
	if (ib_sql_token_is_word(&tokens[0], "INSERT")) {
		result = ib_blob_destination_from_insert(tokens, token_count,
			parameter_index, relation, field, error);
	} else if (ib_sql_token_is_word(&tokens[0], "UPDATE")) {
		result = ib_blob_destination_from_update(tokens, token_count,
			parameter_index, relation, field, error);
	} else {
		result = ib_fail(error,
			"BLOB parameter destination metadata is unavailable for this SQL statement");
	}
	free(tokens);
	return result;
}

static int ib_blob_lookup_bind_descriptor(ib_cursor *cursor, size_t parameter_index,
	const XSQLVAR *variable, ISC_BLOB_DESC_V2 *descriptor, char **error)
{
	XSQLVAR destination;
	char relation[METADATALENGTH + 1U];
	char field[METADATALENGTH + 1U];
	size_t relation_length;
	size_t field_length;

	if (variable != NULL && variable->sqlname_length > 0 &&
		variable->relname_length > 0) {
		return ib_blob_lookup_descriptor(cursor, variable, descriptor, error);
	}
	if (ib_blob_destination_names(cursor, parameter_index, relation, field, error) != 0) {
		return -1;
	}
	relation_length = strlen(relation);
	field_length = strlen(field);
	if (relation_length > METADATALENGTH || field_length > METADATALENGTH) {
		return ib_fail(error, "BLOB parameter destination metadata is too long");
	}
	memset(&destination, 0, sizeof(destination));
	destination.relname_length = (short) relation_length;
	destination.sqlname_length = (short) field_length;
	memcpy(destination.relname, relation, relation_length);
	memcpy(destination.sqlname, field, field_length);
	return ib_blob_lookup_descriptor(cursor, &destination, descriptor, error);
}

static int ib_cursor_blob_ref(ib_cursor *cursor, const XSQLVAR *variable,
	ib_value_view *view, char **error)
{
	ISC_BLOB_DESC_V2 descriptor;
	ISC_QUAD blob_id;
	short blob_charset;
	short index;

	if (variable == NULL || variable->sqldata == NULL || view == NULL) {
		return ib_fail(error, "BLOB result storage is unavailable");
	}
	/* Keep the lookup separate from the SQLDA value: the descriptor gives the
	 * caller the subtype and character set needed to interpret the stream. */
	if (cursor != NULL && cursor->procedure) {
		if (variable->sqlsubtype == 1) {
			if (cursor->output == NULL || cursor->blob_charsets == NULL ||
				cursor->blob_charset_known == NULL) {
				return ib_fail(error, "text BLOB procedure metadata is unavailable");
			}
			index = -1;
			for (short output_index = 0; output_index < cursor->output->sqld;
				output_index++) {
				if (&cursor->output->sqlvar[output_index] == variable) {
					index = output_index;
					break;
				}
			}
			if (index < 0 || cursor->blob_charset_known[index] == 0) {
				return ib_fail(error, "text BLOB procedure character set is unavailable");
			}
			blob_charset = cursor->blob_charsets[index];
			ib_blob_descriptor(&descriptor, 1, blob_charset);
		} else {
			ib_blob_descriptor(&descriptor, variable->sqlsubtype, 0);
		}
	} else if (variable->sqlname_length > 0 && variable->relname_length > 0) {
		if (ib_blob_lookup_descriptor(cursor, variable, &descriptor, error) != 0) {
			return -1;
		}
	} else {
		if (cursor == NULL || cursor->connection == NULL) {
			return ib_fail(error, "BLOB expression metadata is unavailable");
		}
		/* Expressions have no relation/field pair for isc_blob_lookup_desc2. A
		 * binary BLOB is self-describing, while a text BLOB must not guess its
		 * character set from the attachment. */
		if (variable->sqlsubtype == 1) {
			return ib_fail(error, "text BLOB expression character set is unavailable");
		}
		ib_blob_descriptor(&descriptor, variable->sqlsubtype, 0);
	}
	memcpy(&blob_id, variable->sqldata, sizeof(blob_id));
	view->kind = IB_VALUE_BLOB_REF;
	view->blob_high = (int32_t) blob_id.isc_quad_high;
	view->blob_low = (uint32_t) blob_id.isc_quad_low;
	view->blob_subtype = descriptor.blob_desc_subtype;
	view->blob_charset = descriptor.blob_desc_charset;
	return 0;
}

static int ib_set_blob(ib_cursor *cursor, XSQLVAR *variable,
	const char *value, size_t length, short connection_charset, int text,
	short nullable, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	isc_blob_handle blob;
	ISC_QUAD blob_id;
	char *converted;
	char *first_error;
	const char *data;
	size_t data_length;
	size_t offset;
	unsigned char bpb[IB_BLOB_BPB_LENGTH];
	unsigned short bpb_length;
	int cleanup_failed;

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL) {
		return ib_fail(error, "BLOB parameter transaction is unavailable");
	}
	bpb_length = 0U;
	converted = NULL;
	data = value;
	data_length = length;
	if (text && connection_charset != 0 && connection_charset != 1 &&
		connection_charset != IB_CHARSET_UTF8) {
		converted = ib_convert_utf8(value, length, connection_charset,
			&data_length, error);
		if (converted == NULL) {
			return -1;
		}
		data = converted;
	}
	variable->sqltype = (short) (SQL_BLOB | nullable);
	variable->sqllen = (short) sizeof(ISC_QUAD);
	if (ib_allocate_variable(variable, sizeof(ISC_QUAD), error) != 0) {
		free(converted);
		return -1;
	}
	blob = NULL;
	memset(&blob_id, 0, sizeof(blob_id));
	memset(status, 0, sizeof(status));
	if (text && connection_charset != 0 && connection_charset != 1) {
		if (ib_blob_generate_bpb(connection_charset, connection_charset, bpb,
			&bpb_length, error) != 0) {
			free(variable->sqldata);
			free(variable->sqlind);
			variable->sqldata = NULL;
			variable->sqlind = NULL;
			free(converted);
			return -1;
		}
		result = isc_create_blob2(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id, (short) bpb_length,
			(char *) bpb);
	} else {
		result = isc_create_blob(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id);
	}
	if (result != 0) {
		first_error = NULL;
		(void) ib_fail_status(&first_error, "create input BLOB", status);
		{
			char *cleanup_error = NULL;

			cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
			if (cleanup_failed != 0) {
				ib_mark_broken(cursor->connection);
			}
			ib_append_error(&first_error, cleanup_error);
		}
		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		free(converted);
		ib_give_error(error, first_error);
		return -1;
	}
	offset = 0U;
	while (offset < data_length) {
		size_t remaining = data_length - offset;
		size_t segment_size = remaining > IB_BLOB_SEGMENT_LENGTH
			? IB_BLOB_SEGMENT_LENGTH : remaining;
		unsigned short segment_length = (unsigned short) segment_size;
		memset(status, 0, sizeof(status));
		result = isc_put_segment(status, &blob, segment_length,
			(char *) data + offset);
		if (result != 0) {
			first_error = NULL;
			(void) ib_fail_status(&first_error, "write input BLOB segment", status);
			{
				char *cleanup_error = NULL;
				cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
				if (cleanup_failed != 0) {
					ib_mark_broken(cursor->connection);
				}
				ib_append_error(&first_error, cleanup_error);
			}
			free(variable->sqldata);
			free(variable->sqlind);
			variable->sqldata = NULL;
			variable->sqlind = NULL;
			free(converted);
			ib_give_error(error, first_error);
			return -1;
		}
		offset += segment_size;
	}
	first_error = NULL;
	cleanup_failed = ib_blob_cleanup(&blob, 0, &first_error);
	if (cleanup_failed != 0) {
		ib_mark_broken(cursor->connection);
	}
	if (cleanup_failed != 0 || first_error != NULL) {
		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		free(converted);
		ib_give_error(error, first_error);
		return -1;
	}
	memcpy(variable->sqldata, &blob_id, sizeof(blob_id));
	free(converted);
	return 0;
}

ib_blob_reader *ib_blob_open2(ib_connection *connection, int32_t high,
	uint32_t low, int subtype, int charset, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ISC_QUAD blob_id;
	ib_blob_reader *reader;
	unsigned char bpb[IB_BLOB_BPB_LENGTH];
	unsigned short bpb_length;
	int use_bpb;

	if (connection == NULL || connection->database == NULL ||
		connection->transaction == NULL || connection->broken) {
		ib_fail(error, "BLOB transaction is unavailable");
		return NULL;
	}
	if (subtype < SHRT_MIN || subtype > SHRT_MAX || charset < SHRT_MIN ||
		charset > SHRT_MAX) {
		ib_fail(error, "BLOB descriptor is outside the native range");
		return NULL;
	}
	bpb_length = 0U;
	use_bpb = subtype == 1 && charset != 0 && charset != 1 &&
		charset != IB_CHARSET_UTF8;
	if (use_bpb && ib_blob_generate_bpb((short) charset, IB_CHARSET_UTF8,
		bpb, &bpb_length, error) != 0) {
		return NULL;
	}
	reader = (ib_blob_reader *) calloc(1U, sizeof(*reader));
	if (reader == NULL) {
		ib_fail(error, "out of memory allocating BLOB reader");
		return NULL;
	}
	reader->segment = (char *) malloc(IB_BLOB_SEGMENT_LENGTH);
	if (reader->segment == NULL) {
		free(reader);
		ib_fail(error, "out of memory allocating BLOB reader buffer");
		return NULL;
	}
	reader->connection = connection;
	reader->blob = NULL;
	blob_id.isc_quad_high = (ISC_LONG) high;
	blob_id.isc_quad_low = (unsigned ISC_LONG) low;
	memset(status, 0, sizeof(status));
	if (use_bpb) {
		result = isc_open_blob2(status, &connection->database,
			&connection->transaction, &reader->blob, &blob_id,
			(short) bpb_length, (char *) bpb);
	} else {
		result = isc_open_blob(status, &connection->database,
			&connection->transaction, &reader->blob, &blob_id);
	}
	if (result != 0) {
		free(reader->segment);
		free(reader);
		ib_fail_status(error, "open BLOB", status);
		return NULL;
	}
	return reader;
}

ib_blob_reader *ib_blob_open(ib_connection *connection, int32_t high,
	uint32_t low, char **error)
{
	return ib_blob_open2(connection, high, low, 0, 0, error);
}

int ib_blob_reader_read(ib_blob_reader *reader, char *buffer, size_t capacity,
	size_t *length, int *eof, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	size_t copied;

	if (length == NULL || eof == NULL) {
		return ib_fail(error, "BLOB read result storage is unavailable");
	}
	*length = 0U;
	*eof = 0;
	if (reader == NULL || reader->blob == NULL || reader->segment == NULL) {
		return ib_fail(error, "BLOB reader is unavailable");
	}
	if (capacity != 0U && buffer == NULL) {
		return ib_fail(error, "BLOB read buffer is unavailable");
	}
	copied = 0U;
	while (copied < capacity) {
		if (reader->segment_offset < reader->segment_length) {
			size_t available = reader->segment_length - reader->segment_offset;
			size_t requested = capacity - copied;
			size_t amount = available < requested ? available : requested;

			memcpy(buffer + copied, reader->segment + reader->segment_offset, amount);
			reader->segment_offset += amount;
			copied += amount;
			continue;
		}
		if (reader->eof) {
			break;
		}
		reader->segment_length = 0U;
		reader->segment_offset = 0U;
		{
			unsigned short segment_length = 0U;

			memset(status, 0, sizeof(status));
			result = isc_get_segment(status, &reader->blob, &segment_length,
				(unsigned short) IB_BLOB_SEGMENT_LENGTH, reader->segment);
			if (result == isc_segstr_eof) {
				reader->eof = 1;
				break;
			}
			if (result != 0 && result != isc_segment) {
				return ib_fail_status(error, "read BLOB segment", status);
			}
			reader->segment_length = (size_t) segment_length;
			if (reader->segment_length == 0U && result == isc_segment) {
				continue;
			}
		}
	}
	*length = copied;
	*eof = reader->eof && reader->segment_offset >= reader->segment_length;
	return 0;
}

int ib_blob_reader_close(ib_blob_reader *reader, int cancel, char **error)
{
	char *first_error;
	int result;

	if (reader == NULL) {
		return 0;
	}
	first_error = NULL;
	result = ib_blob_cleanup(&reader->blob, cancel != 0, &first_error);
	if (result != 0 && reader->connection != NULL) {
		ib_mark_broken(reader->connection);
	}
	free(reader->segment);
	free(reader);
	if (result != 0) {
		ib_give_error(error, first_error);
		return -1;
	}
	free(first_error);
	return 0;
}

ib_blob_writer *ib_blob_create(ib_connection *connection, int subtype,
	int charset, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	unsigned char bpb[IB_BLOB_BPB_LENGTH];
	unsigned short bpb_length;
	ib_blob_writer *writer;

	if (connection == NULL || connection->database == NULL ||
		connection->transaction == NULL || connection->broken) {
		ib_fail(error, "BLOB transaction is unavailable");
		return NULL;
	}
	if (subtype < SHRT_MIN || subtype > SHRT_MAX || charset < SHRT_MIN ||
		charset > SHRT_MAX) {
		ib_fail(error, "BLOB options are outside the native range");
		return NULL;
	}
	writer = (ib_blob_writer *) calloc(1U, sizeof(*writer));
	if (writer == NULL) {
		ib_fail(error, "out of memory allocating BLOB writer");
		return NULL;
	}
	writer->connection = connection;
	writer->subtype = (short) subtype;
	writer->charset = (short) (subtype == 1 ? charset : 0);
	bpb_length = 0U;
	if (ib_blob_generate_bpb_options(writer->subtype, writer->charset, bpb,
		&bpb_length, error) != 0) {
		free(writer);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	result = isc_create_blob2(status, &connection->database, &connection->transaction,
		&writer->blob, &writer->blob_id, (short) bpb_length, (char *) bpb);
	if (result != 0) {
		free(writer);
		ib_fail_status(error, "create BLOB", status);
		return NULL;
	}
	return writer;
}

int ib_blob_writer_write(ib_blob_writer *writer, const char *data,
	size_t length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	size_t offset;

	if (writer == NULL || writer->blob == NULL) {
		return ib_fail(error, "BLOB writer is unavailable");
	}
	if (length != 0U && data == NULL) {
		return ib_fail(error, "BLOB write data is unavailable");
	}
	offset = 0U;
	while (offset < length) {
		size_t remaining = length - offset;
		size_t segment_size = remaining > IB_BLOB_SEGMENT_LENGTH ?
			IB_BLOB_SEGMENT_LENGTH : remaining;

		memset(status, 0, sizeof(status));
		result = isc_put_segment(status, &writer->blob, (unsigned short) segment_size,
			(char *) data + offset);
		if (result != 0) {
			return ib_fail_status(error, "write BLOB segment", status);
		}
		offset += segment_size;
	}
	return 0;
}

int ib_blob_writer_close(ib_blob_writer *writer, int cancel, int32_t *high,
	uint32_t *low, int *subtype, int *charset, char **error)
{
	char *first_error;
	int result;

	if (writer == NULL) {
		return ib_fail(error, "BLOB writer is unavailable");
	}
	if (!cancel && (high == NULL || low == NULL || subtype == NULL || charset == NULL)) {
		return ib_fail(error, "BLOB reference result storage is unavailable");
	}
	first_error = NULL;
	result = ib_blob_cleanup(&writer->blob, cancel != 0, &first_error);
	if (result != 0 && writer->connection != NULL) {
		ib_mark_broken(writer->connection);
	}
	if (result == 0 && !cancel) {
		*high = (int32_t) writer->blob_id.isc_quad_high;
		*low = (uint32_t) writer->blob_id.isc_quad_low;
		*subtype = (int) writer->subtype;
		*charset = (int) writer->charset;
	}
	free(writer);
	if (result != 0) {
		ib_give_error(error, first_error);
		return -1;
	}
	free(first_error);
	return 0;
}

static int ib_array_field_metadata(ib_cursor *cursor, const XSQLVAR *variable,
	int *precision, int *charset, char **error);
static size_t ib_utf8_prefix_length(const char *data, size_t length,
	size_t character_count);
static size_t ib_array_character_count(const ISC_ARRAY_DESC_V2 *descriptor,
	int charset);

static int ib_array_lookup_descriptor(ib_cursor *cursor,
	const XSQLVAR *variable, ISC_ARRAY_DESC_V2 *descriptor, int *precision,
	int *charset, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char field_name[METADATALENGTH + 1U];
	char relation_name[METADATALENGTH + 1U];
	int field_precision;
	int field_charset;
	int character_array;
	int numeric_array;

	if (cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		variable == NULL || descriptor == NULL || charset == NULL ||
		variable->sqlname_length <= 0 ||
		variable->relname_length <= 0) {
		return ib_fail(error, "array column metadata is unavailable");
	}
	if ((size_t) variable->sqlname_length > METADATALENGTH ||
		(size_t) variable->relname_length > METADATALENGTH) {
		return ib_fail(error, "array column metadata is too long");
	}
	memset(field_name, 0, sizeof(field_name));
	memset(relation_name, 0, sizeof(relation_name));
	memcpy(field_name, variable->sqlname, (size_t) variable->sqlname_length);
	memcpy(relation_name, variable->relname,
		(size_t) variable->relname_length);
	memset(descriptor, 0, sizeof(*descriptor));
	memset(status, 0, sizeof(status));
	result = isc_array_lookup_desc2(status, &cursor->connection->database,
		&cursor->transaction, relation_name, field_name, descriptor);
	if (result != 0) {
		return ib_fail_status(error, "lookup array descriptor", status);
	}
	memset(status, 0, sizeof(status));
	result = isc_array_lookup_bounds2(status, &cursor->connection->database,
		&cursor->transaction, relation_name, field_name, descriptor);
	if (result != 0) {
		return ib_fail_status(error, "lookup array bounds", status);
	}
	character_array = descriptor->array_desc_dtype == blr_text ||
		descriptor->array_desc_dtype == blr_text2 ||
		descriptor->array_desc_dtype == blr_varying ||
		descriptor->array_desc_dtype == blr_varying2;
	numeric_array = descriptor->array_desc_subtype == 1 ||
		descriptor->array_desc_subtype == 2 || descriptor->array_desc_scale != 0;
	field_precision = 0;
	field_charset = 0;
	*charset = -1;
	if (precision != NULL) {
		*precision = 0;
	}
	if (character_array || (precision != NULL && numeric_array)) {
		if (ib_array_field_metadata(cursor, variable, &field_precision,
			&field_charset, error) != 0) {
			return -1;
		}
		if (character_array) {
			if (field_charset < 0 || field_charset > SHRT_MAX) {
				return ib_fail(error, "array field character set is out of range");
			}
			*charset = field_charset;
		}
		if (precision != NULL && numeric_array) {
			if (field_precision <= 0 || field_precision > SHRT_MAX) {
				return ib_fail(error, "scaled array field precision is unavailable");
			}
			*precision = field_precision;
		}
	}
	return 0;
}

static int ib_array_element_size(const ISC_ARRAY_DESC_V2 *descriptor,
	size_t *element_size, char **error)
{
	size_t size;

	if (descriptor == NULL || element_size == NULL ||
		descriptor->array_desc_version != ARR_DESC_VERSION2) {
		return ib_fail(error, "array descriptor storage is unavailable");
	}
	size = (size_t) descriptor->array_desc_length;
	switch (descriptor->array_desc_dtype) {
	case blr_text:
	case blr_text2:
		if (size == 0U) {
			return ib_fail(error, "array descriptor has no element storage");
		}
		break;
	case blr_varying:
	case blr_varying2:
		if (size == 0U || size > SIZE_MAX - sizeof(unsigned short)) {
			return ib_fail(error, "array varying element size is invalid");
		}
		size += sizeof(unsigned short);
		break;
	case blr_short:
		if (size != sizeof(short)) {
			return ib_fail(error, "SMALLINT array descriptor length is invalid");
		}
		break;
	case blr_long:
		if (size != sizeof(ISC_LONG)) {
			return ib_fail(error, "INTEGER array descriptor length is invalid");
		}
		break;
	case blr_int64:
		if (size != sizeof(ISC_INT64)) {
			return ib_fail(error, "BIGINT array descriptor length is invalid");
		}
		break;
	case blr_float:
		if (size != sizeof(float)) {
			return ib_fail(error, "FLOAT array descriptor length is invalid");
		}
		break;
	case blr_double:
	case blr_d_float:
		if (size != sizeof(double)) {
			return ib_fail(error, "DOUBLE array descriptor length is invalid");
		}
		break;
	case blr_timestamp:
		if (size != sizeof(ISC_TIMESTAMP)) {
			return ib_fail(error, "TIMESTAMP array descriptor length is invalid");
		}
		break;
	case blr_sql_date:
		if (size != sizeof(ISC_DATE)) {
			return ib_fail(error, "DATE array descriptor length is invalid");
		}
		break;
	case blr_sql_time:
		if (size != sizeof(ISC_TIME)) {
			return ib_fail(error, "TIME array descriptor length is invalid");
		}
		break;
	case blr_boolean_dtype:
		if (size != sizeof(ISC_BOOLEAN)) {
			return ib_fail(error, "BOOLEAN array descriptor length is invalid");
		}
		break;
	default:
		return ib_fail(error, "array descriptor type is unsupported");
	}
	*element_size = size;
	return 0;
}

static int ib_array_slice_layout(const ISC_ARRAY_DESC_V2 *descriptor,
	size_t *element_size, size_t *element_count, size_t *slice_size,
	char **error)
{
	size_t index;
	size_t size = 0U;
	size_t count;
	uint64_t width;

	if (descriptor == NULL || element_size == NULL || element_count == NULL ||
		slice_size == NULL || descriptor->array_desc_dimensions <= 0 ||
		descriptor->array_desc_dimensions > 16 ||
		descriptor->array_desc_version != ARR_DESC_VERSION2) {
		return ib_fail(error, "array descriptor dimensions are invalid");
	}
	if (ib_array_element_size(descriptor, &size, error) != 0) {
		return -1;
	}
	count = 1U;
	for (index = 0U; index < (size_t) descriptor->array_desc_dimensions; index++) {
		short lower = descriptor->array_desc_bounds[index].array_bound_lower;
		short upper = descriptor->array_desc_bounds[index].array_bound_upper;

		if (lower > upper) {
			return ib_fail(error, "array descriptor bounds are invalid");
		}
		width = (uint64_t) ((int64_t) upper - (int64_t) lower) + UINT64_C(1);
		if (width > (uint64_t) SIZE_MAX / count) {
			return ib_fail(error, "array element count is too large");
		}
		count *= (size_t) width;
	}
	if (count > (size_t) IB_MAX_ARRAY_BUFFER / size) {
		return ib_fail(error, "array slice is too large");
	}
	*element_size = size;
	*element_count = count;
	*slice_size = count * size;
	return 0;
}

static int ib_array_descriptor_matches(const ISC_ARRAY_DESC_V2 *descriptor,
	const ib_array_value *array, char **error)
{
	size_t index;

	if (descriptor == NULL || array == NULL ||
		array->dimensions != (size_t) descriptor->array_desc_dimensions) {
		return ib_fail(error, "array bounds do not match the column descriptor");
	}
	for (index = 0U; index < array->dimensions; index++) {
		const ISC_ARRAY_BOUND *native_bound = &descriptor->array_desc_bounds[index];
		if (array->bounds[index].lower != (int32_t) native_bound->array_bound_lower ||
			array->bounds[index].upper != (int32_t) native_bound->array_bound_upper) {
			return ib_fail(error, "array bounds do not match the column descriptor");
		}
	}
	return 0;
}

static int ib_array_store_bytes(char *destination, size_t capacity,
	const char *source, size_t length, int varying, int binary, char **error)
{
	size_t payload_capacity = capacity;

	if (varying && capacity < sizeof(unsigned short)) {
		return ib_fail(error, "array varying element storage is invalid");
	}
	if (varying) {
		payload_capacity -= sizeof(unsigned short);
	}
	if (length != 0U && source == NULL) {
		return ib_fail(error, "array character element data is unavailable");
	}
	if (varying && length != 0U && memchr(source, '\0', length) != NULL) {
		return ib_fail(error, binary ?
			"varying OCTETS array elements cannot contain NUL" :
			"varying text array elements cannot contain NUL");
	}
	if (length > payload_capacity || length > (size_t) USHRT_MAX) {
		return ib_fail(error, binary ?
			"array byte element is too large" : "array string element is too large");
	}
	if (payload_capacity != 0U) {
		memset(destination, binary || varying ? 0 : ' ', payload_capacity);
	}
	if (length != 0U) {
		memcpy(destination, source, length);
	}
	return 0;
}

static int ib_scale_array_integer(const ISC_ARRAY_DESC_V2 *descriptor,
	int64_t value, int precision, int64_t *result, char **error)
{
	char text[32];
	XSQLVAR variable;
	int length;

	if (descriptor == NULL || result == NULL) {
		return ib_fail(error, "scaled array integer storage is unavailable");
	}
	length = snprintf(text, sizeof(text), "%" PRId64, value);
	if (length < 0 || (size_t) length >= sizeof(text)) {
		return ib_fail(error, "scaled array integer is unavailable");
	}
	memset(&variable, 0, sizeof(variable));
	variable.sqltype = SQL_INT64;
	variable.sqlscale = descriptor->array_desc_scale;
	variable.sqlsubtype = descriptor->array_desc_subtype;
	if (precision < 0 || precision > SHRT_MAX) {
		return ib_fail(error, "scaled array integer precision is invalid");
	}
	variable.sqlprecision = (short) precision;
	return ib_parse_scaled_integer(text, (size_t) length, &variable, result, error);
}

static int ib_array_encode_element(const ISC_ARRAY_DESC_V2 *descriptor,
	int charset, const ib_array_element *element, char *destination,
	size_t element_size, int precision, char **error)
{
	int dtype;
	int numeric_scaled;
	size_t expected_size;

	if (descriptor == NULL || element == NULL || destination == NULL) {
		return ib_fail(error, "array element storage is unavailable");
	}
	if (ib_array_element_size(descriptor, &expected_size, error) != 0) {
		return -1;
	}
	if (element_size != expected_size) {
		return ib_fail(error, "array element storage does not match its descriptor");
	}
	dtype = descriptor->array_desc_dtype;
	numeric_scaled = descriptor->array_desc_subtype == 1 ||
		descriptor->array_desc_subtype == 2 || descriptor->array_desc_scale != 0;
	memset(destination, 0, element_size);
	switch (dtype) {
	case blr_text:
	case blr_text2:
	case blr_varying:
	case blr_varying2:
	{
		char *converted;
		size_t converted_length;
		int varying = dtype == blr_varying || dtype == blr_varying2;

		if (charset < 0 || charset > SHRT_MAX) {
			return ib_fail(error, "array character set is unavailable");
		}
		if (element->kind == IB_ARRAY_ELEMENT_BYTES) {
			if (charset != 1) {
				return ib_fail(error, "byte array elements require an OCTETS array");
			}
			return ib_array_store_bytes(destination, element_size, element->bytes,
				element->length, varying, 1, error);
		}
		if (element->kind != IB_ARRAY_ELEMENT_STRING) {
			return ib_fail(error, "array character elements require string or byte values");
		}
		if (charset == 1) {
			return ib_fail(error, "string array elements cannot bind to OCTETS");
		}
		converted = ib_convert_utf8(element->bytes, element->length,
			(short) charset, &converted_length, error);
		if (converted == NULL) {
			return -1;
		}
		if (charset == IB_CHARSET_UTF8 &&
			ib_utf8_prefix_length(converted, converted_length,
				ib_array_character_count(descriptor, charset)) != converted_length) {
			free(converted);
			return ib_fail(error, "array string element exceeds its character capacity");
		}
		if (ib_array_store_bytes(destination, element_size, converted,
			converted_length, varying, 0, error) != 0) {
			free(converted);
			return -1;
		}
		free(converted);
		return 0;
	}
	case blr_short:
	{
		short value;
		XSQLVAR variable;
		int64_t integer_value;

		if (element->kind == IB_ARRAY_ELEMENT_STRING && numeric_scaled) {
			memset(&variable, 0, sizeof(variable));
			variable.sqltype = SQL_SHORT;
			variable.sqlscale = descriptor->array_desc_scale;
			variable.sqlsubtype = descriptor->array_desc_subtype;
			variable.sqlprecision = (short) precision;
			if (ib_parse_scaled_integer(element->bytes, element->length, &variable,
				&integer_value, error) != 0) {
				return -1;
			}
		} else if (element->kind == IB_ARRAY_ELEMENT_INT64) {
			if (numeric_scaled) {
				if (ib_scale_array_integer(descriptor, element->int64_value, precision,
					&integer_value, error) != 0) {
					return -1;
				}
			} else {
				integer_value = element->int64_value;
			}
		} else {
			return ib_fail(error, "SMALLINT array elements require integer values");
		}
		if (integer_value < (int64_t) SHRT_MIN || integer_value > (int64_t) SHRT_MAX) {
			return ib_fail(error, "SMALLINT array element is outside its range");
		}
		value = (short) integer_value;
		memcpy(destination, &value, sizeof(value));
		return 0;
	}
	case blr_long:
	{
		ISC_LONG value;
		XSQLVAR variable;
		int64_t integer_value;

		if (element->kind == IB_ARRAY_ELEMENT_STRING && numeric_scaled) {
			memset(&variable, 0, sizeof(variable));
			variable.sqltype = SQL_LONG;
			variable.sqlscale = descriptor->array_desc_scale;
			variable.sqlsubtype = descriptor->array_desc_subtype;
			variable.sqlprecision = (short) precision;
			if (ib_parse_scaled_integer(element->bytes, element->length, &variable,
				&integer_value, error) != 0) {
				return -1;
			}
		} else if (element->kind == IB_ARRAY_ELEMENT_INT64) {
			if (numeric_scaled) {
				if (ib_scale_array_integer(descriptor, element->int64_value, precision,
					&integer_value, error) != 0) {
					return -1;
				}
			} else {
				integer_value = element->int64_value;
			}
		} else {
			return ib_fail(error, "INTEGER array elements require integer values");
		}
		if (integer_value < (int64_t) INT32_MIN || integer_value > (int64_t) INT32_MAX) {
			return ib_fail(error, "INTEGER array element is outside its range");
		}
		value = (ISC_LONG) integer_value;
		memcpy(destination, &value, sizeof(value));
		return 0;
	}
	case blr_int64:
	{
		int64_t value;
		XSQLVAR variable;

		if (element->kind == IB_ARRAY_ELEMENT_STRING && numeric_scaled) {
			memset(&variable, 0, sizeof(variable));
			variable.sqltype = SQL_INT64;
			variable.sqlscale = descriptor->array_desc_scale;
			variable.sqlsubtype = descriptor->array_desc_subtype;
			variable.sqlprecision = (short) precision;
			if (ib_parse_scaled_integer(element->bytes, element->length, &variable,
				&value, error) != 0) {
				return -1;
			}
		} else if (element->kind == IB_ARRAY_ELEMENT_INT64) {
			if (numeric_scaled) {
				if (ib_scale_array_integer(descriptor, element->int64_value, precision,
					&value, error) != 0) {
					return -1;
				}
			} else {
				value = element->int64_value;
			}
		} else {
			return ib_fail(error, "BIGINT array elements require integer values");
		}
		memcpy(destination, &value, sizeof(value));
		return 0;
	}
	case blr_float:
	{
		float value;

		if (element->kind != IB_ARRAY_ELEMENT_FLOAT64 ||
			!isfinite(element->float64_value)) {
			return ib_fail(error, "FLOAT array elements require finite floating-point values");
		}
		value = (float) element->float64_value;
		if (!isfinite(value)) {
			return ib_fail(error, "FLOAT array element is outside its range");
		}
		memcpy(destination, &value, sizeof(value));
		return 0;
	}
	case blr_double:
	case blr_d_float:
	{
		if (element->kind != IB_ARRAY_ELEMENT_FLOAT64 ||
			!isfinite(element->float64_value)) {
			return ib_fail(error, "DOUBLE array elements require finite floating-point values");
		}
		memcpy(destination, &element->float64_value, sizeof(element->float64_value));
		return 0;
	}
	case blr_timestamp:
	case blr_sql_date:
	case blr_sql_time:
	{
		ib_bind_value temporal;
		XSQLVAR variable;

		if (element->kind != IB_ARRAY_ELEMENT_TIMESTAMP) {
			return ib_fail(error, "date/time array elements require time values");
		}
		memset(&temporal, 0, sizeof(temporal));
		temporal.kind = IB_ARGUMENT_TIMESTAMP;
		temporal.year = element->year;
		temporal.month = element->month;
		temporal.day = element->day;
		temporal.hour = element->hour;
		temporal.minute = element->minute;
		temporal.second = element->second;
		temporal.nanosecond = element->nanosecond;
		memset(&variable, 0, sizeof(variable));
		variable.sqltype = dtype == blr_timestamp ? SQL_TIMESTAMP :
			dtype == blr_sql_date ? SQL_TYPE_DATE : SQL_TYPE_TIME;
		variable.sqllen = descriptor->array_desc_length;
		if (ib_set_temporal(&variable, &temporal, 0, error) != 0) {
			return -1;
		}
		if ((size_t) variable.sqllen != element_size || variable.sqldata == NULL) {
			free(variable.sqldata);
			free(variable.sqlind);
			return ib_fail(error, "date/time array element storage is invalid");
		}
		memcpy(destination, variable.sqldata, element_size);
		free(variable.sqldata);
		free(variable.sqlind);
		return 0;
	}
	case blr_boolean_dtype:
	{
		ISC_BOOLEAN value;

		if (element->kind != IB_ARRAY_ELEMENT_BOOL) {
			return ib_fail(error, "BOOLEAN array elements require boolean values");
		}
		value = element->bool_value ? ISC_TRUE : ISC_FALSE;
		memcpy(destination, &value, sizeof(value));
		return 0;
	}
	default:
		return ib_fail(error, "array element type is unsupported");
	}
}

static int ib_set_array(ib_cursor *cursor, XSQLVAR *variable,
	const ib_array_value *array, short nullable, char **error)
{
	ISC_ARRAY_DESC_V2 descriptor;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ISC_QUAD array_id;
	char *slice;
	ISC_LONG slice_length;
	size_t element_size;
	size_t element_count;
	size_t slice_size;
	int precision;
	int charset;
	size_t index;

	if (ib_array_lookup_descriptor(cursor, variable, &descriptor, &precision,
		&charset, error) != 0 ||
		ib_array_descriptor_matches(&descriptor, array, error) != 0 ||
		ib_array_slice_layout(&descriptor, &element_size, &element_count, &slice_size,
			error) != 0) {
		return -1;
	}
	if (element_count != array->element_count) {
		return ib_fail(error, "array element count does not match the column descriptor");
	}
	slice = (char *) calloc(slice_size == 0U ? 1U : slice_size, 1U);
	if (slice == NULL) {
		return ib_fail(error, "out of memory allocating array slice");
	}
	for (index = 0U; index < element_count; index++) {
		if (ib_array_encode_element(&descriptor, charset, &array->elements[index],
			slice + index * element_size, element_size, precision, error) != 0) {
			free(slice);
			return -1;
		}
	}
	memset(&array_id, 0, sizeof(array_id));
	slice_length = (ISC_LONG) slice_size;
	memset(status, 0, sizeof(status));
	result = isc_array_put_slice2(status, &cursor->connection->database,
		&cursor->transaction, &array_id, &descriptor, slice,
		&slice_length);
	if (result != 0) {
		free(slice);
		return ib_fail_status(error, "write array slice", status);
	}
	variable->sqltype = (short) (SQL_ARRAY | nullable);
	variable->sqllen = (short) sizeof(ISC_QUAD);
	if (ib_allocate_variable(variable, sizeof(array_id), error) != 0) {
		free(slice);
		return -1;
	}
	memcpy(variable->sqldata, &array_id, sizeof(array_id));
	if (!nullable) {
		free(variable->sqlind);
		variable->sqlind = NULL;
	}
	free(slice);
	return 0;
}

static int ib_bind_input_mode(ib_cursor *cursor, const ib_bindings *bindings,
	int allow_arrays, char **error)
{
	short index;
	short connection_charset;

	if (bindings == NULL || cursor->input == NULL ||
		(bindings->count != 0U && bindings->values == NULL)) {
		return ib_fail(error, "missing input SQLDA");
	}
	connection_charset = ib_connection_charset(cursor->connection);
	if ((size_t) cursor->input->sqld != bindings->count) {
		return ib_fail(error, "query argument count does not match positional parameters");
	}
	for (index = 0; index < cursor->input->sqld; index++) {
		XSQLVAR *variable = &cursor->input->sqlvar[index];
		const ib_bind_value *value = &bindings->values[index];
		short nullable;
		size_t size;
		int described_type = ib_sql_type(variable);
		nullable = (short) (variable->sqltype & 1);

		free(variable->sqldata);
		free(variable->sqlind);
		variable->sqldata = NULL;
		variable->sqlind = NULL;
		if (value->kind == IB_ARGUMENT_NULL && described_type == SQL_ARRAY &&
			!allow_arrays) {
			return ib_fail(error, "array parameters are unsupported by database/sql");
		}
		if (value->kind == IB_ARGUMENT_NULL) {
			variable->sqltype = (short) (variable->sqltype | 1);
			if (ib_value_buffer_size(variable, &size, error) != 0 ||
				ib_allocate_variable(variable, size, error) != 0) {
				return -1;
			}
			*variable->sqlind = -1;
			continue;
		}
		if (value->kind == IB_ARGUMENT_BLOB_REF) {
			ISC_QUAD blob_id;
			ISC_BLOB_DESC_V2 destination;

			if (!allow_arrays) {
				return ib_fail(error, "BLOB references are unsupported by database/sql");
			}
			if (described_type != SQL_BLOB) {
				if ((variable->sqlname_length <= 0 || variable->relname_length <= 0) &&
					cursor->query != NULL && cursor->query_length != 0U) {
					char relation[METADATALENGTH + 1U];
					char field[METADATALENGTH + 1U];
					char *metadata_error = NULL;

					if (ib_blob_destination_names(cursor, (size_t) index, relation, field,
						&metadata_error) != 0) {
						ib_give_error(error, metadata_error);
						return -1;
					}
					free(metadata_error);
				}
				return ib_fail(error, "BLOB references require a BLOB parameter");
			}
			if (ib_blob_lookup_bind_descriptor(cursor, (size_t) index, variable,
				&destination, error) != 0) {
				return -1;
			}
			if (destination.blob_desc_subtype != value->blob_subtype) {
				return ib_fail(error, "BLOB reference subtype does not match BLOB parameter");
			}
			if (value->blob_subtype == 1 &&
				destination.blob_desc_charset != value->blob_charset) {
				return ib_fail(error, "BLOB reference character set does not match BLOB parameter");
			}
			blob_id.isc_quad_high = (ISC_LONG) value->blob_high;
			blob_id.isc_quad_low = (unsigned ISC_LONG) value->blob_low;
			variable->sqltype = (short) (SQL_BLOB | nullable);
			variable->sqllen = (short) sizeof(blob_id);
			if (ib_allocate_variable(variable, sizeof(blob_id), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &blob_id, sizeof(blob_id));
			if (!nullable) {
				free(variable->sqlind);
				variable->sqlind = NULL;
			}
			continue;
		}
		if (described_type == SQL_ARRAY) {
			if (!allow_arrays) {
				return ib_fail(error, "array parameters are unsupported by database/sql");
			}
			if (value->kind != IB_ARGUMENT_ARRAY || value->array == NULL) {
				return ib_fail(error, "array parameters require an Array value");
			}
			if (ib_set_array(cursor, variable, value->array, nullable, error) != 0) {
				return -1;
			}
			continue;
		}
		if (value->kind == IB_ARGUMENT_ARRAY) {
			return ib_fail(error, "Array values require an array parameter");
		}
		if (described_type == SQL_BLOB) {
			switch (value->kind) {
			case IB_ARGUMENT_STRING:
				if (ib_set_blob(cursor, variable, value->bytes, value->length,
					connection_charset, variable->sqlsubtype == 1, nullable, error) != 0) {
					return -1;
				}
				break;
			case IB_ARGUMENT_BYTES:
				if (ib_set_blob(cursor, variable, value->bytes, value->length,
					connection_charset, 0, nullable, error) != 0) {
					return -1;
				}
				break;
			default:
				return ib_fail(error, "only string and byte values can bind to a BLOB");
			}
			continue;
		}

		switch (value->kind) {
		case IB_ARGUMENT_STRING:
			if (ib_connection_dialect(cursor->connection) == SQL_DIALECT_V6 &&
				ib_is_scaled_integer(variable)) {
				int64_t scaled_value = 0;

				if (ib_parse_scaled_integer(value->bytes, value->length, variable,
					&scaled_value, error) != 0) {
					return -1;
				}
				if (ib_set_scaled_integer(variable, scaled_value, nullable, error) != 0) {
					return -1;
				}
			} else if (ib_set_varying(variable, value->bytes, value->length,
				value->source_charset, connection_charset, 0, nullable, error) != 0) {
				return -1;
			}
			break;
		case IB_ARGUMENT_BYTES:
			if ((described_type != SQL_TEXT && described_type != SQL_VARYING) ||
				ib_text_charset(variable) != 1) {
				return ib_fail(error, "byte values require OCTETS parameters");
			}
			if (ib_set_varying(variable, value->bytes, value->length,
				connection_charset, connection_charset, 1, nullable, error) != 0) {
				return -1;
			}
			break;
		case IB_ARGUMENT_INT64:
			variable->sqltype = (short) (SQL_INT64 | nullable);
			variable->sqlscale = 0;
			variable->sqlsubtype = 0;
			variable->sqlprecision = 0;
			variable->sqllen = (short) sizeof(ISC_INT64);
			if (ib_allocate_variable(variable, sizeof(ISC_INT64), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &value->int64_value, sizeof(value->int64_value));
			break;
		case IB_ARGUMENT_FLOAT64:
			if (!isfinite(value->float64_value)) {
				return ib_fail(error, "floating-point argument must be finite");
			}
			variable->sqltype = (short) (SQL_DOUBLE | nullable);
			variable->sqlscale = 0;
			variable->sqlsubtype = 0;
			variable->sqlprecision = 0;
			variable->sqllen = (short) sizeof(double);
			if (ib_allocate_variable(variable, sizeof(double), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &value->float64_value, sizeof(value->float64_value));
			break;
		case IB_ARGUMENT_BOOL:
		{
			ISC_BOOLEAN boolean_value = value->bool_value ? ISC_TRUE : ISC_FALSE;
			variable->sqltype = (short) (SQL_BOOLEAN | nullable);
			variable->sqlscale = 0;
			variable->sqlsubtype = 0;
			variable->sqlprecision = 0;
			variable->sqllen = (short) sizeof(boolean_value);
			if (ib_allocate_variable(variable, sizeof(boolean_value), error) != 0) {
				return -1;
			}
			memcpy(variable->sqldata, &boolean_value, sizeof(boolean_value));
			break;
		}
		case IB_ARGUMENT_TIMESTAMP:
			if (described_type != SQL_TYPE_DATE && described_type != SQL_TYPE_TIME &&
				described_type != SQL_TIMESTAMP) {
				described_type = SQL_TIMESTAMP;
				variable->sqltype = (short) (described_type | nullable);
				variable->sqlscale = 0;
				variable->sqlsubtype = 0;
				variable->sqlprecision = 0;
			}
			if (ib_set_temporal(variable, value, nullable, error) != 0) {
				return -1;
			}
			break;
		default:
			return ib_fail(error, "the query argument kind is unsupported");
		}
	}
	return 0;
}

static int ib_bind_input(ib_cursor *cursor, const ib_bindings *bindings,
	char **error)
{
	return ib_bind_input_mode(cursor, bindings, 0, error);
}

static int ib_validate_output_types_mode(const XSQLDA *sqlda, int allow_arrays,
	char **error)
{
	short index;
	int type;

	if (sqlda == NULL) {
		return ib_fail(error, "missing output SQLDA");
	}
	for (index = 0; index < sqlda->sqld; index++) {
		type = ib_sql_type(&sqlda->sqlvar[index]);
		switch (type) {
		case SQL_TEXT:
		case SQL_VARYING:
		case SQL_SHORT:
		case SQL_LONG:
		case SQL_INT64:
		case SQL_TIMESTAMP:
		case SQL_TYPE_DATE:
		case SQL_TYPE_TIME:
		case SQL_BOOLEAN:
		case SQL_BLOB:
			break;
		case SQL_ARRAY:
			if (!allow_arrays) {
				return ib_fail(error, "array results are unsupported by database/sql");
			}
			break;
		case SQL_FLOAT:
		case SQL_DOUBLE:
		case SQL_D_FLOAT:
			break;
		default:
			return ib_fail(error, "the result contains a type unsupported by the proof of concept");
		}
	}
	return 0;
}

static int ib_validate_output_types(const XSQLDA *sqlda, char **error)
{
	return ib_validate_output_types_mode(sqlda, 0, error);
}

static int ib_allocate_output(ib_cursor *cursor, char **error)
{
	short index;
	size_t size;

	for (index = 0; index < cursor->output->sqld; index++) {
		cursor->output->sqlvar[index].sqltype =
			(short) (cursor->output->sqlvar[index].sqltype | 1);
		if (ib_value_buffer_size(&cursor->output->sqlvar[index], &size, error) != 0 ||
			ib_allocate_variable(&cursor->output->sqlvar[index], size, error) != 0) {
			return -1;
		}
		if ((cursor->output->sqlvar[index].sqltype & 1) == 0) {
			free(cursor->output->sqlvar[index].sqlind);
			cursor->output->sqlvar[index].sqlind = NULL;
		}
		if (ib_sql_type(&cursor->output->sqlvar[index]) == SQL_TEXT) {
			memset(cursor->output->sqlvar[index].sqldata, ' ', size);
		}
	}
	return 0;
}

static void ib_cursor_free_array(ib_cursor *cursor)
{
	size_t index;

	if (cursor == NULL) {
		return;
	}
	if (cursor->array_element_data != NULL) {
		for (index = 0U; index < cursor->array_element_count; index++) {
			free(cursor->array_element_data[index]);
		}
	}
	free(cursor->array_element_data);
	free(cursor->array_elements);
	free(cursor->array_bounds);
	free(cursor->array_data);
	cursor->array_element_data = NULL;
	cursor->array_elements = NULL;
	cursor->array_bounds = NULL;
	cursor->array_data = NULL;
	cursor->array_dimensions = 0U;
	cursor->array_element_count = 0U;
}

static void ib_cursor_free_parts(ib_cursor *cursor)
{
	if (cursor == NULL) {
		return;
	}
	free(cursor->converted_value);
	cursor->converted_value = NULL;
	free(cursor->materialized_value);
	cursor->materialized_value = NULL;
	ib_cursor_free_array(cursor);
	free(cursor->metadata);
	cursor->metadata = NULL;
	free(cursor->blob_charsets);
	cursor->blob_charsets = NULL;
	free(cursor->blob_charset_known);
	cursor->blob_charset_known = NULL;
	free(cursor->fixed_text_lengths);
	cursor->fixed_text_lengths = NULL;
	if (cursor->column_names != NULL) {
		short index;

		for (index = 0; cursor->output != NULL && index < cursor->output->sqld; index++) {
			free(cursor->column_names[index]);
		}
	}
	free(cursor->column_names);
	cursor->column_names = NULL;
	free(cursor->column_name_lengths);
	cursor->column_name_lengths = NULL;
	free(cursor->query);
	cursor->query = NULL;
	cursor->query_length = 0U;
	ib_free_sqlda(cursor->input);
	ib_free_sqlda(cursor->output);
	cursor->input = NULL;
	cursor->output = NULL;
}

static int ib_cursor_close_internal(ib_cursor *cursor, char **error, int success)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	int failed;
	int commit_transaction;
	unsigned short free_option;

	if (cursor == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	commit_transaction = 0;
	if (cursor->statement != NULL &&
		(cursor->prepared == NULL || cursor->server_cursor_open)) {
		free_option = cursor->prepared != NULL && cursor->server_cursor_open
			? DSQL_close : DSQL_drop;
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &cursor->statement,
			free_option);
		if (result != 0) {
			failed = 1;
			(void) ib_fail_status(&first_error,
				free_option == DSQL_close ? "close cursor" : "close statement", status);
		}
	}
	cursor->statement = NULL;
	cursor->server_cursor_open = 0;
	if (cursor->owns_transaction && cursor->transaction != NULL) {
		memset(status, 0, sizeof(status));
		commit_transaction = success && !failed && cursor->procedure;
		if (commit_transaction) {
			result = isc_commit_transaction(status, &cursor->transaction);
			if (result != 0) {
				ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];
				char *rollback_error = NULL;

				failed = 1;
				(void) ib_fail_status(&first_error,
					"commit procedure transaction", status);
				if (cursor->transaction != NULL) {
					memset(rollback_status, 0, sizeof(rollback_status));
					if (isc_rollback_transaction(rollback_status,
						&cursor->transaction) != 0) {
						(void) ib_fail_status(&rollback_error,
							"rollback failed procedure transaction", rollback_status);
					}
					ib_append_error(&first_error, rollback_error);
				}
			}
		} else {
			result = isc_rollback_transaction(status, &cursor->transaction);
			if (result != 0) {
				failed = 1;
				if (first_error == NULL) {
					(void) ib_fail_status(&first_error, "rollback transaction", status);
				}
			}
		}
		cursor->transaction = NULL;
	}
	cursor->transaction = NULL;
	cursor->owns_transaction = 0;
	if (cursor->prepared != NULL && cursor->prepared->active_cursor == cursor) {
		cursor->prepared->active_cursor = NULL;
	}
	ib_cursor_unregister(cursor);
	if (failed && cursor->connection != NULL) {
		ib_mark_broken(cursor->connection);
	}
	ib_cursor_free_parts(cursor);
	free(cursor);
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

static ib_cursor *ib_find_transaction_cursor(ib_connection *connection,
	isc_tr_handle transaction)
{
	ib_cursor *cursor;

	if (connection == NULL || transaction == NULL) {
		return NULL;
	}
	for (cursor = connection->cursors; cursor != NULL; cursor = cursor->next) {
		if (!cursor->owns_transaction && cursor->transaction == transaction) {
			return cursor;
		}
	}
	return NULL;
}

static int ib_close_transaction_cursors(ib_connection *connection,
	isc_tr_handle transaction, char **error)
{
	char *first_error;
	char *cursor_error;
	ib_cursor *cursor;
	int failed;

	first_error = NULL;
	failed = 0;
	while ((cursor = ib_find_transaction_cursor(connection, transaction)) != NULL) {
		cursor_error = NULL;
		if (ib_cursor_close_internal(cursor, &cursor_error, 1) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

static int ib_close_all_cursors(ib_connection *connection, char **error,
	int success)
{
	char *first_error;
	char *cursor_error;
	ib_cursor *cursor;
	int failed;

	first_error = NULL;
	failed = 0;
	while (connection != NULL && connection->cursors != NULL) {
		cursor = connection->cursors;
		cursor_error = NULL;
		if (ib_cursor_close_internal(cursor, &cursor_error,
			success && connection->broken == 0) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

static int ib_descriptor_name(const char *value, short value_length,
	char *buffer, size_t capacity, size_t *length, char **error)
{
	size_t name_length;

	if (value == NULL || buffer == NULL || length == NULL || capacity == 0U) {
		return ib_fail(error, "column name storage is unavailable");
	}
	if (value_length < 0 || (size_t) value_length >= capacity) {
		return ib_fail(error, "column name is too long");
	}
	name_length = (size_t) value_length;
	if (name_length != 0U) {
		memcpy(buffer, value, name_length);
	}
	while (name_length > 0U && buffer[name_length - 1U] == ' ') {
		name_length--;
	}
	buffer[name_length] = '\0';
	*length = name_length;
	return name_length != 0U;
}

static int ib_catalog_string(const XSQLVAR *variable, char *buffer,
	size_t capacity, size_t *length, char **error)
{
	int type;
	size_t value_length;
	unsigned short varying_length;

	if (variable == NULL || buffer == NULL || length == NULL || capacity == 0U) {
		return ib_fail(error, "catalog string storage is unavailable");
	}
	*length = 0U;
	if (variable->sqlind != NULL && *variable->sqlind < 0) {
		return 0;
	}
	if (variable->sqldata == NULL) {
		return ib_fail(error, "catalog string data is unavailable");
	}
	type = ib_sql_type(variable);
	if (type == SQL_TEXT) {
		if (variable->sqllen < 0) {
			return ib_fail(error, "catalog string length is invalid");
		}
		value_length = (size_t) variable->sqllen;
		if (value_length >= capacity) {
			return ib_fail(error, "catalog string is too long");
		}
		if (value_length != 0U) {
			memcpy(buffer, variable->sqldata, value_length);
		}
	} else if (type == SQL_VARYING) {
		if (variable->sqllen < 0) {
			return ib_fail(error, "catalog varying length is invalid");
		}
		memcpy(&varying_length, variable->sqldata, sizeof(varying_length));
		if (varying_length > (unsigned short) variable->sqllen) {
			return ib_fail(error, "catalog varying value length is invalid");
		}
		value_length = (size_t) varying_length;
		if (value_length >= capacity) {
			return ib_fail(error, "catalog string is too long");
		}
		if (value_length != 0U) {
			memcpy(buffer, variable->sqldata + sizeof(varying_length), value_length);
		}
	} else {
		return ib_fail(error, "catalog value is not textual");
	}
	while (value_length > 0U && buffer[value_length - 1U] == ' ') {
		value_length--;
	}
	buffer[value_length] = '\0';
	*length = value_length;
	return 1;
}

static int ib_catalog_integer(const XSQLVAR *variable, int *value,
	int *present, char **error)
{
	int type;
	short short_value;
	ISC_LONG long_value;
	ISC_INT64 int64_value;

	if (variable == NULL || value == NULL || present == NULL) {
		return ib_fail(error, "catalog integer storage is unavailable");
	}
	*value = 0;
	*present = 0;
	if (variable->sqlind != NULL && *variable->sqlind < 0) {
		return 0;
	}
	if (variable->sqldata == NULL) {
		return ib_fail(error, "catalog integer data is unavailable");
	}
	type = ib_sql_type(variable);
	switch (type) {
	case SQL_SHORT:
		memcpy(&short_value, variable->sqldata, sizeof(short_value));
		*value = (int) short_value;
		break;
	case SQL_LONG:
		memcpy(&long_value, variable->sqldata, sizeof(long_value));
		if (long_value < (ISC_LONG) INT_MIN || long_value > (ISC_LONG) INT_MAX) {
			return ib_fail(error, "catalog integer value is out of range");
		}
		*value = (int) long_value;
		break;
	case SQL_INT64:
		memcpy(&int64_value, variable->sqldata, sizeof(int64_value));
		if (int64_value < (ISC_INT64) INT_MIN || int64_value > (ISC_INT64) INT_MAX) {
			return ib_fail(error, "catalog integer value is out of range");
		}
		*value = (int) int64_value;
		break;
	default:
		return ib_fail(error, "catalog value is not an integer");
	}
	*present = 1;
	return 0;
}

static int ib_array_field_metadata(ib_cursor *cursor, const XSQLVAR *variable,
	int *precision, int *charset, char **error)
{
	static const char query[] =
		"SELECT f.RDB$FIELD_PRECISION, f.RDB$CHARACTER_SET_ID "
		"FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f "
		"ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE "
		"WHERE rf.RDB$RELATION_NAME = ? "
		"AND rf.RDB$FIELD_NAME = ?";
	char relation_name[METADATALENGTH + 1U];
	char field_name[METADATALENGTH + 1U];
	size_t relation_length;
	size_t field_length;
	int relation_result;
	int field_result;
	ib_cursor *catalog_cursor;
	ib_bindings *bindings;
	char *catalog_error;
	char *close_error;
	int result;
	int close_result;

	if (cursor == NULL || variable == NULL || precision == NULL || charset == NULL) {
		return ib_fail(error, "array field metadata storage is unavailable");
	}
	*precision = 0;
	*charset = -1;
	relation_result = ib_descriptor_name(variable->relname,
		variable->relname_length, relation_name, sizeof(relation_name),
		&relation_length, error);
	field_result = ib_descriptor_name(variable->sqlname,
		variable->sqlname_length, field_name, sizeof(field_name), &field_length,
		error);
	if (relation_result < 0 || field_result < 0) {
		return -1;
	}
	if (relation_result == 0 || field_result == 0) {
		return ib_fail(error, "array field metadata is unavailable");
	}
	catalog_cursor = (ib_cursor *) calloc(1U, sizeof(*catalog_cursor));
	if (catalog_cursor == NULL) {
		return ib_fail(error, "out of memory allocating array metadata cursor");
	}
	catalog_cursor->connection = cursor->connection;
	catalog_cursor->transaction = cursor->transaction;
	bindings = NULL;
	catalog_error = NULL;
	close_error = NULL;
	result = 0;
	close_result = 0;
	if (ib_cursor_prepare_statement(catalog_cursor, query, sizeof(query) - 1U,
		&catalog_error) != 0 ||
		ib_statement_is_select(catalog_cursor, &catalog_error) != 0 ||
		ib_describe_bind(catalog_cursor, &catalog_error) != 0) {
		result = -1;
		goto array_metadata_done;
	}
	bindings = ib_bindings_new(2U, &catalog_error);
	if (bindings == NULL || ib_bindings_set_encoded_string(bindings, 0U,
		relation_name, relation_length,
		ib_connection_charset(cursor->connection), &catalog_error) != 0 ||
		ib_bindings_set_encoded_string(bindings, 1U, field_name, field_length,
		ib_connection_charset(cursor->connection), &catalog_error) != 0 ||
		ib_bind_input(catalog_cursor, bindings, &catalog_error) != 0 ||
		ib_describe_output(catalog_cursor, &catalog_error) != 0 ||
		ib_validate_output_types(catalog_cursor->output, &catalog_error) != 0 ||
		ib_allocate_output(catalog_cursor, &catalog_error) != 0) {
		result = -1;
		goto array_metadata_done;
	}
	{
		ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
		ISC_STATUS native_result;
		XSQLDA *input = catalog_cursor->input->sqld == 0 ? NULL :
			catalog_cursor->input;

		memset(status, 0, sizeof(status));
		native_result = isc_dsql_execute2(status, &catalog_cursor->transaction,
			&catalog_cursor->statement,
			ib_connection_dialect(catalog_cursor->connection), input, NULL);
		if (native_result != 0) {
			(void) ib_fail_status(&catalog_error, "execute array metadata query", status);
			result = -1;
			goto array_metadata_done;
		}
	}
	catalog_cursor->server_cursor_open = 1;
	for (;;) {
		int has_row = ib_cursor_next(catalog_cursor, NULL, 0U, &catalog_error);
		int precision_present;
		int charset_present;
		int value;

		if (has_row < 0) {
			result = -1;
			break;
		}
		if (has_row == 0) {
			break;
		}
		if (catalog_cursor->output == NULL || catalog_cursor->output->sqld < 2 ||
			ib_catalog_integer(&catalog_cursor->output->sqlvar[0], &value,
			&precision_present, &catalog_error) != 0) {
			result = -1;
			break;
		}
		if (precision_present) {
			if (value <= 0 || value > SHRT_MAX) {
				(void) ib_fail(&catalog_error,
					"array field precision is out of range");
				result = -1;
				break;
			}
			*precision = value;
		}
		if (ib_catalog_integer(&catalog_cursor->output->sqlvar[1], &value,
			&charset_present, &catalog_error) != 0) {
			result = -1;
			break;
		}
		if (charset_present) {
			*charset = value;
		}
	}

array_metadata_done:
	if (bindings != NULL) {
		ib_bindings_free(bindings);
	}
	if (catalog_cursor->statement != NULL || catalog_cursor->input != NULL ||
		catalog_cursor->output != NULL) {
		close_result = ib_cursor_close_internal(catalog_cursor, &close_error, 0);
		if (close_result != 0) {
			result = -1;
		}
	} else {
		free(catalog_cursor);
	}
	if (close_result != 0) {
		ib_append_error(&catalog_error, close_error);
		close_error = NULL;
	}
	if (result != 0) {
		ib_give_error(error, catalog_error);
		free(close_error);
		return -1;
	}
	free(catalog_error);
	free(close_error);
	return 0;
}

static int ib_metadata_allocate(ib_cursor *cursor, char **error)
{
	short index;

	if (cursor == NULL || cursor->output == NULL || cursor->output->sqld < 0) {
		return ib_fail(error, "output metadata is unavailable");
	}
	if (cursor->output->sqld == 0) {
		return 0;
	}
	if ((size_t) cursor->output->sqld > SIZE_MAX / sizeof(*cursor->metadata)) {
		return ib_fail(error, "output metadata is too large");
	}
	cursor->metadata = (ib_column_metadata *) calloc((size_t) cursor->output->sqld,
		sizeof(*cursor->metadata));
	if (cursor->metadata == NULL) {
		return ib_fail(error, "out of memory allocating output metadata");
	}
	cursor->blob_charsets = (short *) calloc((size_t) cursor->output->sqld,
		sizeof(*cursor->blob_charsets));
	cursor->blob_charset_known = (unsigned char *) calloc((size_t) cursor->output->sqld,
		sizeof(*cursor->blob_charset_known));
	cursor->fixed_text_lengths = (size_t *) calloc((size_t) cursor->output->sqld,
		sizeof(*cursor->fixed_text_lengths));
	if (cursor->blob_charsets == NULL || cursor->blob_charset_known == NULL ||
		cursor->fixed_text_lengths == NULL) {
		return ib_fail(error, "out of memory allocating BLOB metadata");
	}
	{
		const char *query = cursor->query;
		size_t query_length = cursor->query_length;

		if (query == NULL && cursor->prepared != NULL) {
			query = cursor->prepared->query;
			query_length = cursor->prepared->query_length;
		}
		ib_sql_describe_fixed_text_columns(query, query_length,
			cursor->fixed_text_lengths, (size_t) cursor->output->sqld);
	}
	for (index = 0; index < cursor->output->sqld; index++) {
		XSQLVAR *variable = &cursor->output->sqlvar[index];
		int type = ib_sql_type(variable);

		cursor->metadata[index].sql_type = ib_metadata_type_code(type);
		cursor->metadata[index].sql_subtype = variable->sqlsubtype;
		if (type == SQL_TEXT || type == SQL_VARYING) {
			cursor->metadata[index].sql_subtype = variable->sqlsubtype & 0xFF;
		}
		if (cursor->fixed_text_lengths[index] != 0U) {
			if (ib_sql_fixed_text_descriptor_compatible(variable,
				cursor->fixed_text_lengths[index])) {
				cursor->metadata[index].length =
					(int) cursor->fixed_text_lengths[index];
				cursor->metadata[index].has_length = 1;
			} else {
				/* Do not let an SQL parser guess override an incompatible SQLDA. */
				cursor->fixed_text_lengths[index] = 0U;
			}
		}
		cursor->metadata[index].sql_scale = variable->sqlscale;
		cursor->metadata[index].sql_precision = variable->sqlprecision;
		if (variable->relname_length > 0 && variable->sqlname_length > 0) {
			/* The live SQLDA nullable bit accounts for result-level nullability,
			 * including columns introduced by an outer join. */
			cursor->metadata[index].nullable = (variable->sqltype & 1) != 0;
			cursor->metadata[index].has_nullable = 1;
		}
	}
	return 0;
}

static int ib_cursor_describe_column_names(ib_cursor *cursor, char **error)
{
	short index;
	size_t count;

	if (cursor == NULL || cursor->output == NULL || cursor->output->sqld < 0) {
		return ib_fail(error, "output column names are unavailable");
	}
	count = (size_t) cursor->output->sqld;
	if (count == 0U) {
		return 0;
	}
	if (count > SIZE_MAX / sizeof(*cursor->column_names) ||
		count > SIZE_MAX / sizeof(*cursor->column_name_lengths)) {
		return ib_fail(error, "output column names are too large");
	}
	cursor->column_names = (char **) calloc(count, sizeof(*cursor->column_names));
	cursor->column_name_lengths = (size_t *) calloc(count,
		sizeof(*cursor->column_name_lengths));
	if (cursor->column_names == NULL || cursor->column_name_lengths == NULL) {
		return ib_fail(error, "out of memory allocating output column names");
	}
	for (index = 0; index < cursor->output->sqld; index++) {
		XSQLVAR *variable = &cursor->output->sqlvar[index];
		const char *name;
		short name_length;

		if (variable->aliasname_length > 0) {
			name = variable->aliasname;
			name_length = variable->aliasname_length;
		} else {
			name = variable->sqlname;
			name_length = variable->sqlname_length;
		}
		if (name_length < 0 || name_length > METADATALENGTH) {
			return ib_fail(error, "result column name is too long");
		}
		if (name_length != 0 && name == NULL) {
			return ib_fail(error, "result column name is unavailable");
		}
		cursor->column_names[index] = ib_convert_to_utf8(name,
			(size_t) name_length, cursor->connection->charset,
			&cursor->column_name_lengths[index], error);
		if (cursor->column_names[index] == NULL) {
			return -1;
		}
	}
	return 0;
}

static int ib_catalog_relation_list(ib_cursor *cursor, char ***names,
	size_t **lengths, size_t *count, char **error)
{
	char **relation_names;
	size_t *relation_lengths;
	short index;
	size_t relation_count;
	size_t relation_index;

	if (cursor == NULL || cursor->output == NULL || names == NULL ||
		lengths == NULL || count == NULL) {
		return ib_fail(error, "catalog relation list is unavailable");
	}
	relation_names = NULL;
	relation_lengths = NULL;
	relation_count = 0U;
	for (index = 0; index < cursor->output->sqld; index++) {
		char relation[METADATALENGTH + 1U];
		size_t relation_length;
		short relation_name_length = cursor->output->sqlvar[index].relname_length;
		int name_result;
		char **resized_names;
		size_t *resized_lengths;

		name_result = ib_descriptor_name(cursor->output->sqlvar[index].relname,
			relation_name_length, relation, sizeof(relation), &relation_length, error);
		if (name_result < 0) {
			goto fail;
		}
		if (name_result == 0) {
			continue;
		}
		for (relation_index = 0U; relation_index < relation_count; relation_index++) {
			if (relation_lengths[relation_index] == relation_length &&
				memcmp(relation_names[relation_index], relation, relation_length) == 0) {
				break;
			}
		}
		if (relation_index != relation_count) {
			continue;
		}
		if (relation_count == SIZE_MAX / sizeof(*relation_names) ||
			relation_count == SIZE_MAX / sizeof(*relation_lengths)) {
			(void) ib_fail(error, "too many catalog relations");
			goto fail;
		}
		resized_names = (char **) malloc((relation_count + 1U) * sizeof(*relation_names));
		resized_lengths = (size_t *) malloc((relation_count + 1U) * sizeof(*relation_lengths));
		if (resized_names == NULL || resized_lengths == NULL) {
			free(resized_names);
			free(resized_lengths);
			(void) ib_fail(error, "out of memory collecting catalog relations");
			goto fail;
		}
		if (relation_count != 0U) {
			memcpy(resized_names, relation_names,
				relation_count * sizeof(*relation_names));
			memcpy(resized_lengths, relation_lengths,
				relation_count * sizeof(*relation_lengths));
		}
		free(relation_names);
		free(relation_lengths);
		relation_names = resized_names;
		relation_lengths = resized_lengths;
		relation_names[relation_count] = ib_copy_string(relation, relation_length);
		if (relation_names[relation_count] == NULL) {
			(void) ib_fail(error, "out of memory copying catalog relation");
			goto fail;
		}
		relation_lengths[relation_count] = relation_length;
		relation_count++;
	}
	*names = relation_names;
	*lengths = relation_lengths;
	*count = relation_count;
	return 0;

fail:
	if (relation_names != NULL) {
		for (relation_index = 0U; relation_index < relation_count; relation_index++) {
			free(relation_names[relation_index]);
		}
	}
	free(relation_names);
	free(relation_lengths);
	return -1;
}

static void ib_free_catalog_relations(char **names, size_t *lengths, size_t count)
{
	size_t index;

	(void) lengths;
	if (names == NULL) {
		free(lengths);
		return;
	}
	for (index = 0U; index < count; index++) {
		free(names[index]);
	}
	free(names);
	free(lengths);
}

static int ib_catalog_query_text(size_t relation_count, char **query,
	size_t *query_length, char **error)
{
	static const char prefix[] =
		"SELECT rf.RDB$RELATION_NAME, rf.RDB$FIELD_NAME, "
		"f.RDB$FIELD_TYPE, f.RDB$FIELD_SUB_TYPE, f.RDB$FIELD_LENGTH, "
		"f.RDB$FIELD_SCALE, f.RDB$FIELD_PRECISION, f.RDB$CHARACTER_LENGTH, "
		"f.RDB$CHARACTER_SET_ID, "
		"rf.RDB$NULL_FLAG "
		"FROM RDB$RELATION_FIELDS rf JOIN RDB$FIELDS f "
		"ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE "
		"WHERE rf.RDB$RELATION_NAME IN (";
	static const char suffix[] = ")";
	char *text;
	size_t prefix_length;
	size_t suffix_length;
	size_t placeholders;
	size_t total;
	size_t offset;
	size_t index;

	if (query == NULL || query_length == NULL || relation_count == 0U) {
		return ib_fail(error, "catalog query storage is unavailable");
	}
	prefix_length = sizeof(prefix) - 1U;
	suffix_length = sizeof(suffix) - 1U;
	if (relation_count > (SIZE_MAX - prefix_length - suffix_length) / 3U) {
		return ib_fail(error, "catalog query is too long");
	}
	placeholders = relation_count * 3U - 1U;
	total = prefix_length + placeholders + suffix_length;
	if (total > (size_t) USHRT_MAX) {
		return ib_fail(error, "catalog query is too long");
	}
	text = (char *) malloc(total + 1U);
	if (text == NULL) {
		return ib_fail(error, "out of memory building catalog query");
	}
	memcpy(text, prefix, prefix_length);
	offset = prefix_length;
	for (index = 0U; index < relation_count; index++) {
		if (index != 0U) {
			text[offset++] = ',';
			text[offset++] = ' ';
		}
		text[offset++] = '?';
	}
	memcpy(text + offset, suffix, suffix_length);
	offset += suffix_length;
	text[offset] = '\0';
	*query = text;
	*query_length = offset;
	return 0;
}

static int ib_procedure_catalog_query_text(char **query, size_t *query_length,
	char **error)
{
	static const char text[] =
		"SELECT pp.RDB$PROCEDURE_NAME, pp.RDB$PARAMETER_NAME, "
		"f.RDB$FIELD_TYPE, f.RDB$FIELD_SUB_TYPE, f.RDB$FIELD_SCALE, "
		"f.RDB$FIELD_PRECISION, f.RDB$CHARACTER_SET_ID "
		"FROM RDB$PROCEDURE_PARAMETERS pp JOIN RDB$FIELDS f "
		"ON f.RDB$FIELD_NAME = pp.RDB$FIELD_SOURCE "
		"WHERE pp.RDB$PROCEDURE_NAME = ? "
		"AND pp.RDB$PARAMETER_NAME = ? "
		"AND pp.RDB$PARAMETER_TYPE = 1";
	size_t length;

	if (query == NULL || query_length == NULL) {
		return ib_fail(error, "procedure catalog query storage is unavailable");
	}
	length = sizeof(text) - 1U;
	if (length > (size_t) USHRT_MAX) {
		return ib_fail(error, "procedure catalog query is too long");
	}
	*query = ib_copy_string(text, length);
	if (*query == NULL) {
		return ib_fail(error, "out of memory building procedure catalog query");
	}
	*query_length = length;
	return 0;
}

static int ib_procedure_catalog_apply_row(ib_cursor *cursor,
	const ib_cursor *catalog, char **error)
{
	char procedure_name[METADATALENGTH + 1U];
	char parameter_name[METADATALENGTH + 1U];
	size_t procedure_name_length;
	size_t parameter_name_length;
	int procedure_result;
	int parameter_result;
	int field_type;
	int field_subtype;
	int field_scale;
	int field_precision;
	int field_charset;
	int present;
	int charset_present;
	int sql_type;
	short index;

	if (cursor == NULL || catalog == NULL || catalog->output == NULL ||
		catalog->output->sqld < 7) {
		return ib_fail(error, "procedure catalog output is incomplete");
	}
	procedure_result = ib_catalog_string(&catalog->output->sqlvar[0],
		procedure_name, sizeof(procedure_name), &procedure_name_length, error);
	parameter_result = ib_catalog_string(&catalog->output->sqlvar[1],
		parameter_name, sizeof(parameter_name), &parameter_name_length, error);
	if (procedure_result < 0 || parameter_result < 0) {
		return -1;
	}
	if (procedure_result == 0 || parameter_result == 0) {
		return 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[2], &field_type, &present,
		error) != 0 || !present || ib_catalog_integer(&catalog->output->sqlvar[3],
		&field_subtype, &present, error) != 0) {
		return -1;
	}
	if (!present) {
		field_subtype = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[4], &field_scale, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_scale = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[5], &field_precision,
		&present, error) != 0) {
		return -1;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[6], &field_charset,
		&charset_present, error) != 0) {
		return -1;
	}
	if (!charset_present) {
		field_charset = 0;
	}
	sql_type = ib_catalog_sql_type(field_type);
	for (index = 0; index < cursor->output->sqld; index++) {
		char source_procedure[METADATALENGTH + 1U];
		char source_parameter[METADATALENGTH + 1U];
		size_t source_procedure_length;
		size_t source_parameter_length;
		int source_procedure_result;
		int source_parameter_result;
		XSQLVAR *variable = &cursor->output->sqlvar[index];

		source_procedure_result = ib_descriptor_name(variable->relname,
			variable->relname_length, source_procedure, sizeof(source_procedure),
			&source_procedure_length, error);
		source_parameter_result = ib_descriptor_name(variable->sqlname,
			variable->sqlname_length, source_parameter, sizeof(source_parameter),
			&source_parameter_length, error);
		if (source_procedure_result < 0 || source_parameter_result < 0) {
			return -1;
		}
		if (source_procedure_result == 0 || source_parameter_result == 0 ||
			source_procedure_length != procedure_name_length ||
			source_parameter_length != parameter_name_length ||
			memcmp(source_procedure, procedure_name, procedure_name_length) != 0 ||
			memcmp(source_parameter, parameter_name, parameter_name_length) != 0) {
			continue;
		}
		if (sql_type == SQL_BLOB) {
			if (field_subtype == 1 && charset_present &&
				cursor->blob_charsets != NULL && cursor->blob_charset_known != NULL) {
				cursor->blob_charsets[index] = (short) field_charset;
				cursor->blob_charset_known[index] = 1;
			}
			continue;
		}
		if (!present || field_precision <= 0 ||
			(sql_type != SQL_SHORT && sql_type != SQL_LONG && sql_type != SQL_INT64)) {
			continue;
		}
		cursor->metadata[index].sql_type = ib_metadata_type_code(
			sql_type);
		cursor->metadata[index].sql_subtype = field_subtype;
		cursor->metadata[index].sql_scale = field_scale;
		cursor->metadata[index].sql_precision = field_precision;
		cursor->metadata[index].precision = field_precision;
		cursor->metadata[index].scale = field_scale;
		cursor->metadata[index].has_precision_scale = 1;
	}
	return 0;
}

static int ib_catalog_apply_row(ib_cursor *cursor, const ib_cursor *catalog,
	char **error)
{
	char relation[METADATALENGTH + 1U];
	char field[METADATALENGTH + 1U];
	size_t relation_length;
	size_t field_length;
	int field_type;
	int sql_type;
	int field_subtype;
	int field_length_value;
	int field_scale;
	int field_precision;
	int character_length;
	int character_set_id;
	int null_flag;
	int present;
	short index;
	int relation_result;
	int field_result;

	if (cursor == NULL || catalog == NULL || catalog->output == NULL ||
		catalog->output->sqld < 10) {
		return ib_fail(error, "catalog output is incomplete");
	}
	relation_result = ib_catalog_string(&catalog->output->sqlvar[0], relation,
		sizeof(relation), &relation_length, error);
	field_result = ib_catalog_string(&catalog->output->sqlvar[1], field,
		sizeof(field), &field_length, error);
	if (relation_result < 0 || field_result < 0) {
		return -1;
	}
	if (relation_result == 0 || field_result == 0) {
		return 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[2], &field_type, &present, error) != 0 ||
		!present || ib_catalog_integer(&catalog->output->sqlvar[4], &field_length_value,
		&present, error) != 0 || !present) {
		return -1;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[3], &field_subtype, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_subtype = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[5], &field_scale, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_scale = 0;
	}
	sql_type = ib_catalog_sql_type(field_type);
	if (ib_catalog_integer(&catalog->output->sqlvar[6], &field_precision, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		field_precision = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[7], &character_length, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		character_length = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[8], &character_set_id, &present,
		error) != 0) {
		return -1;
	}
	if (!present) {
		character_set_id = 0;
	}
	if (ib_catalog_integer(&catalog->output->sqlvar[9], &null_flag, &present,
		error) != 0) {
		return -1;
	}
	for (index = 0; index < cursor->output->sqld; index++) {
		char source_relation[METADATALENGTH + 1U];
		char source_field[METADATALENGTH + 1U];
		size_t source_relation_length;
		size_t source_field_length;
		int source_relation_result;
		int source_field_result;
		XSQLVAR *variable = &cursor->output->sqlvar[index];

		source_relation_result = ib_descriptor_name(variable->relname,
			variable->relname_length, source_relation, sizeof(source_relation),
			&source_relation_length, error);
		source_field_result = ib_descriptor_name(variable->sqlname,
			variable->sqlname_length, source_field, sizeof(source_field),
			&source_field_length, error);
		if (source_relation_result < 0 || source_field_result < 0) {
			return -1;
		}
		if (source_relation_result == 0 || source_field_result == 0 ||
			source_relation_length != relation_length ||
			source_field_length != field_length ||
			memcmp(source_relation, relation, relation_length) != 0 ||
			memcmp(source_field, field, field_length) != 0) {
			continue;
		}
		cursor->metadata[index].sql_type = ib_metadata_type_code(sql_type);
		cursor->metadata[index].sql_subtype = field_subtype;
		if ((sql_type == SQL_TEXT || sql_type == SQL_VARYING) &&
			character_set_id > 0) {
			cursor->metadata[index].sql_subtype = character_set_id;
		}
		cursor->metadata[index].sql_scale = field_scale;
		cursor->metadata[index].sql_precision = field_precision;
		if ((sql_type == SQL_TEXT || sql_type == SQL_VARYING) &&
			character_length > 0) {
			cursor->metadata[index].length = character_length;
			cursor->metadata[index].has_length = 1;
		} else if ((sql_type == SQL_TEXT || sql_type == SQL_VARYING) &&
			field_length_value > 0 && field_subtype == 1) {
			cursor->metadata[index].length = field_length_value;
			cursor->metadata[index].has_length = 1;
		}
		if ((sql_type == SQL_SHORT || sql_type == SQL_LONG ||
			sql_type == SQL_INT64) && field_precision > 0) {
			cursor->metadata[index].precision = field_precision;
			cursor->metadata[index].scale = field_scale;
			cursor->metadata[index].has_precision_scale = 1;
		}
		cursor->metadata[index].nullable = cursor->metadata[index].nullable ||
			(present ? null_flag == 0 : 1);
		cursor->metadata[index].has_nullable = 1;
	}
	return 0;
}

static int ib_procedure_metadata_catalog(ib_cursor *cursor, char **error)
{
	char *catalog_query;
	size_t catalog_query_length;
	short index;

	if (cursor == NULL || cursor->output == NULL) {
		return ib_fail(error, "procedure metadata is unavailable");
	}
	if (ib_procedure_catalog_query_text(&catalog_query, &catalog_query_length,
		error) != 0) {
		return -1;
	}
	for (index = 0; index < cursor->output->sqld; index++) {
		char procedure_name[METADATALENGTH + 1U];
		char parameter_name[METADATALENGTH + 1U];
		size_t procedure_name_length;
		size_t parameter_name_length;
		int procedure_result;
		int parameter_result;
		ib_cursor *catalog_cursor;
		ib_bindings *bindings;
		char *catalog_error;
		char *close_error;
		int result;
		int close_result;
		int has_row;
		XSQLVAR *variable = &cursor->output->sqlvar[index];

		procedure_result = ib_descriptor_name(variable->relname,
			variable->relname_length, procedure_name, sizeof(procedure_name),
			&procedure_name_length, error);
		parameter_result = ib_descriptor_name(variable->sqlname,
			variable->sqlname_length, parameter_name, sizeof(parameter_name),
			&parameter_name_length, error);
		if (procedure_result < 0 || parameter_result < 0) {
			free(catalog_query);
			return -1;
		}
		if (procedure_result == 0 || parameter_result == 0) {
			continue;
		}
		catalog_cursor = (ib_cursor *) calloc(1U, sizeof(*catalog_cursor));
		if (catalog_cursor == NULL) {
			free(catalog_query);
			return ib_fail(error, "out of memory allocating procedure catalog cursor");
		}
		catalog_cursor->connection = cursor->connection;
		catalog_cursor->transaction = cursor->transaction;
		bindings = NULL;
		catalog_error = NULL;
		close_error = NULL;
		result = 0;
		close_result = 0;
		if (ib_cursor_prepare_statement(catalog_cursor, catalog_query,
			catalog_query_length, &catalog_error) != 0 ||
			ib_statement_is_select(catalog_cursor, &catalog_error) != 0 ||
			ib_describe_bind(catalog_cursor, &catalog_error) != 0) {
			result = -1;
			goto procedure_catalog_done;
		}
		bindings = ib_bindings_new(2U, &catalog_error);
		if (bindings == NULL || ib_bindings_set_encoded_string(bindings, 0U,
			procedure_name, procedure_name_length,
			ib_connection_charset(cursor->connection), &catalog_error) != 0 ||
			ib_bindings_set_encoded_string(bindings, 1U, parameter_name,
			parameter_name_length,
			ib_connection_charset(cursor->connection), &catalog_error) != 0 ||
			ib_bind_input(catalog_cursor, bindings, &catalog_error) != 0 ||
			ib_describe_output(catalog_cursor, &catalog_error) != 0 ||
			ib_validate_output_types(catalog_cursor->output, &catalog_error) != 0 ||
			ib_allocate_output(catalog_cursor, &catalog_error) != 0) {
			result = -1;
			goto procedure_catalog_done;
		}
		{
			ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
			ISC_STATUS native_result;
			XSQLDA *input = catalog_cursor->input->sqld == 0 ? NULL :
				catalog_cursor->input;

			memset(status, 0, sizeof(status));
			native_result = isc_dsql_execute2(status, &catalog_cursor->transaction,
				&catalog_cursor->statement,
				ib_connection_dialect(catalog_cursor->connection), input, NULL);
			if (native_result != 0) {
				(void) ib_fail_status(&catalog_error, "execute procedure catalog query",
					status);
				result = -1;
				goto procedure_catalog_done;
			}
		}
		catalog_cursor->server_cursor_open = 1;
		for (;;) {
			has_row = ib_cursor_next(catalog_cursor, NULL, 0U, &catalog_error);
			if (has_row < 0) {
				result = -1;
				break;
			}
			if (has_row == 0) {
				break;
			}
			if (ib_procedure_catalog_apply_row(cursor, catalog_cursor,
				&catalog_error) != 0) {
				result = -1;
				break;
			}
		}

procedure_catalog_done:
		if (bindings != NULL) {
			ib_bindings_free(bindings);
		}
		if (catalog_cursor->statement != NULL || catalog_cursor->input != NULL ||
			catalog_cursor->output != NULL) {
			close_result = ib_cursor_close_internal(catalog_cursor, &close_error, 0);
			if (close_result != 0) {
				result = -1;
			}
		} else {
			free(catalog_cursor);
		}
		if (close_result != 0) {
			ib_append_error(&catalog_error, close_error);
			close_error = NULL;
			free(catalog_query);
			ib_give_error(error, catalog_error);
			return -1;
		}
		free(catalog_error);
		free(close_error);
		if (result != 0) {
			/* Procedure catalog metadata is an enhancement; preserve the query
			 * result when the catalog is unavailable. */
			continue;
		}
	}
	free(catalog_query);
	return 0;
}

static int ib_metadata_catalog(ib_cursor *cursor, char **error)
{
	char **relation_names;
	size_t *relation_lengths;
	size_t relation_count;
	char *catalog_query;
	size_t catalog_query_length;
	ib_bindings *bindings;
	ib_cursor *catalog_cursor;
	char *catalog_error;
	char *close_error;
	size_t index;
	int result;
	int close_result;

	relation_names = NULL;
	relation_lengths = NULL;
	relation_count = 0U;
	if (ib_catalog_relation_list(cursor, &relation_names, &relation_lengths,
		&relation_count, error) != 0) {
		return -1;
	}
	if (relation_count == 0U) {
		return 0;
	}
	catalog_query = NULL;
	catalog_query_length = 0U;
	if (ib_catalog_query_text(relation_count, &catalog_query, &catalog_query_length,
		error) != 0) {
		ib_free_catalog_relations(relation_names, relation_lengths, relation_count);
		return -1;
	}
	catalog_cursor = (ib_cursor *) calloc(1U, sizeof(*catalog_cursor));
	if (catalog_cursor == NULL) {
		free(catalog_query);
		ib_free_catalog_relations(relation_names, relation_lengths, relation_count);
		return ib_fail(error, "out of memory allocating catalog cursor");
	}
	catalog_cursor->connection = cursor->connection;
	catalog_cursor->transaction = cursor->transaction;
	bindings = NULL;
	catalog_error = NULL;
	close_error = NULL;
	result = 0;
	close_result = 0;
	if (ib_cursor_prepare_statement(catalog_cursor, catalog_query,
		catalog_query_length, &catalog_error) != 0 ||
		ib_statement_is_select(catalog_cursor, &catalog_error) != 0 ||
		ib_describe_bind(catalog_cursor, &catalog_error) != 0) {
		result = -1;
		goto catalog_done;
	}
	bindings = ib_bindings_new(relation_count, &catalog_error);
	if (bindings == NULL) {
		result = -1;
		goto catalog_done;
	}
	for (index = 0U; index < relation_count; index++) {
		if (ib_bindings_set_encoded_string(bindings, index, relation_names[index],
			relation_lengths[index], ib_connection_charset(cursor->connection),
			&catalog_error) != 0) {
			result = -1;
			goto catalog_done;
		}
	}
	if (ib_bind_input(catalog_cursor, bindings, &catalog_error) != 0 ||
		ib_describe_output(catalog_cursor, &catalog_error) != 0 ||
		ib_validate_output_types(catalog_cursor->output, &catalog_error) != 0 ||
		ib_allocate_output(catalog_cursor, &catalog_error) != 0) {
		result = -1;
		goto catalog_done;
	}
	{
		ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
		ISC_STATUS native_result;
		XSQLDA *input = catalog_cursor->input->sqld == 0 ? NULL : catalog_cursor->input;

		memset(status, 0, sizeof(status));
		native_result = isc_dsql_execute2(status, &catalog_cursor->transaction,
			&catalog_cursor->statement,
			ib_connection_dialect(catalog_cursor->connection), input, NULL);
		if (native_result != 0) {
			(void) ib_fail_status(&catalog_error, "execute catalog query", status);
			result = -1;
			goto catalog_done;
		}
	}
	catalog_cursor->server_cursor_open = 1;
	for (;;) {
		int has_row = ib_cursor_next(catalog_cursor, NULL, 0U, &catalog_error);

		if (has_row < 0) {
			result = -1;
			break;
		}
		if (has_row == 0) {
			break;
		}
		if (ib_catalog_apply_row(cursor, catalog_cursor, &catalog_error) != 0) {
			result = -1;
			break;
		}
	}

catalog_done:
	if (bindings != NULL) {
		ib_bindings_free(bindings);
	}
	if (catalog_cursor->statement != NULL || catalog_cursor->input != NULL ||
		catalog_cursor->output != NULL) {
		close_result = ib_cursor_close_internal(catalog_cursor, &close_error, 0);
		if (close_result != 0) {
			result = -1;
		}
	} else {
		free(catalog_cursor);
	}
	free(catalog_query);
	ib_free_catalog_relations(relation_names, relation_lengths, relation_count);
	if (close_result != 0) {
		ib_append_error(&catalog_error, close_error);
		close_error = NULL;
		ib_give_error(error, catalog_error);
		return -1;
	}
	if (result != 0) {
		/* Catalog information is an enhancement; an unavailable catalog must not
		 * turn an otherwise valid user query into a failure. */
		free(catalog_error);
		free(close_error);
		return 0;
	}
	free(catalog_error);
	free(close_error);
	return 0;
}

static int ib_cursor_describe_metadata(ib_cursor *cursor, char **error)
{
	if (ib_metadata_allocate(cursor, error) != 0) {
		return -1;
	}
	if (ib_cursor_describe_column_names(cursor, error) != 0) {
		return -1;
	}
	if (cursor->statement_type == isc_info_sql_stmt_exec_procedure) {
		return ib_procedure_metadata_catalog(cursor, error);
	}
	return ib_metadata_catalog(cursor, error);
}

static void ib_failed_query_cleanup(ib_cursor *cursor, char **error)
{
	char *cleanup_error;
	ib_connection *connection;

	if (cursor == NULL) {
		return;
	}
	connection = cursor->connection;
	cleanup_error = NULL;
	if (ib_cursor_close_internal(cursor, &cleanup_error, 0) != 0) {
		if (connection != NULL) {
			ib_mark_broken(connection);
		}
	}
	if (cleanup_error != NULL) {
		if (error == NULL || *error == NULL) {
			ib_give_error(error, cleanup_error);
		} else {
			char *joined = ib_join_errors(*error, cleanup_error);
			if (joined != NULL) {
				free(*error);
				free(cleanup_error);
				*error = joined;
			} else {
				free(*error);
				*error = cleanup_error;
			}
		}
	}
}

static void ib_statement_unregister(ib_statement *statement)
{
	ib_statement **current;

	if (statement == NULL || statement->connection == NULL) {
		return;
	}
	current = &statement->connection->statements;
	while (*current != NULL) {
		if (*current == statement) {
			*current = statement->next;
			statement->next = NULL;
			return;
		}
		current = &(*current)->next;
	}
}

static int ib_statement_close_internal(ib_statement *statement, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;
	ib_connection *connection;
	int failed;

	if (statement == NULL) {
		return 0;
	}
	connection = statement->connection;
	first_error = NULL;
	failed = 0;
	if (statement->active_cursor != NULL) {
		cursor_error = NULL;
		if (ib_cursor_close_internal(statement->active_cursor, &cursor_error, 1) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	if (statement->statement != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_dsql_free_statement(status, &statement->statement, DSQL_drop);
		statement->statement = NULL;
		if (result != 0) {
			failed = 1;
			cursor_error = NULL;
			(void) ib_fail_status(&cursor_error, "close prepared statement", status);
			ib_append_error(&first_error, cursor_error);
		}
	}
	ib_free_sqlda(statement->input);
	ib_free_sqlda(statement->output);
	statement->input = NULL;
	statement->output = NULL;
	free(statement->query);
	statement->query = NULL;
	statement->query_length = 0U;
	ib_statement_unregister(statement);
	statement->connection = NULL;
	free(statement);
	if (failed && connection != NULL) {
		ib_mark_broken(connection);
	}
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

static void ib_dpb_add_string(unsigned char *dpb, size_t *offset,
	unsigned char code, const char *value, size_t length)
{
	dpb[(*offset)++] = code;
	dpb[(*offset)++] = (unsigned char) length;
	if (length != 0U) {
		memcpy(dpb + *offset, value, length);
		*offset += length;
	}
}

static void ib_dpb_add_byte(unsigned char *dpb, size_t *offset,
	unsigned char code, unsigned char value)
{
	dpb[(*offset)++] = code;
	dpb[(*offset)++] = 1U;
	dpb[(*offset)++] = value;
}

static void ib_dpb_add_integer(unsigned char *dpb, size_t *offset,
	unsigned char code, uint32_t value)
{
	dpb[(*offset)++] = code;
	dpb[(*offset)++] = 4U;
	dpb[(*offset)++] = (unsigned char) (value & 0xffU);
	dpb[(*offset)++] = (unsigned char) ((value >> 8U) & 0xffU);
	dpb[(*offset)++] = (unsigned char) ((value >> 16U) & 0xffU);
	dpb[(*offset)++] = (unsigned char) ((value >> 24U) & 0xffU);
}

static int ib_build_connection_dpb(const char *user, size_t user_length,
	const char *password, size_t password_length, const char *role,
	size_t role_length, const char *encrypted_password,
	size_t encrypted_password_length, const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, uint32_t connect_timeout, int page_size, int create,
	unsigned char **dpb_result, size_t *dpb_length_result, short *charset_result,
	char **error)
{
	unsigned char *dpb;
	size_t dpb_length;
	size_t offset;
	short charset_id = 0;
	const char *charset_name;
	size_t charset_name_length;

	if (dpb_result == NULL || dpb_length_result == NULL || charset_result == NULL) {
		return ib_fail(error, "connection parameter storage is unavailable");
	}
	*dpb_result = NULL;
	*dpb_length_result = 0U;
	if (dialect != SQL_DIALECT_V5 && dialect != SQL_DIALECT_V6) {
		return ib_fail(error, "connection SQL dialect is unsupported");
	}
	if (user == NULL || user_length == 0U || user_length > (size_t) UCHAR_MAX ||
		password == NULL || password_length > (size_t) UCHAR_MAX ||
		(role_length != 0U && role == NULL) || role_length > (size_t) UCHAR_MAX ||
		(encrypted_password_length != 0U && encrypted_password == NULL) ||
		encrypted_password_length > (size_t) UCHAR_MAX ||
		(system_encryption_password_length != 0U &&
			system_encryption_password == NULL) ||
		system_encryption_password_length > (size_t) UCHAR_MAX) {
		return ib_fail(error, "connection credential length is invalid");
	}
	if (create && page_size != 0 && (page_size < 1024 || page_size > 32768 ||
		((unsigned int) page_size & ((unsigned int) page_size - 1U)) != 0U)) {
		return ib_fail(error, "database page size is invalid");
	}
	if (ib_charset_id(charset, charset_length, &charset_id) != 0) {
		return ib_fail(error, "connection character set is unsupported");
	}
	charset_name = ib_charset_name(charset_id);
	if (charset_name == NULL) {
		return ib_fail(error, "connection character set is unsupported");
	}
	charset_name_length = strlen(charset_name);
	dpb_length = 1U + 2U + user_length + 2U + password_length + 3U +
		2U + charset_name_length;
	if (role_length != 0U) {
		dpb_length += 2U + role_length;
	}
	if (encrypted_password_length != 0U) {
		dpb_length += 2U + encrypted_password_length;
	}
	if (system_encryption_password_length != 0U) {
		dpb_length += 2U + system_encryption_password_length;
	}
	if (connect_timeout != 0U) {
		dpb_length += 2U + 4U;
	}
	if (create) {
		dpb_length += 3U + 3U;
		if (page_size != 0) {
			dpb_length += 2U + 4U;
		}
	}
	if (dpb_length > (size_t) SHRT_MAX) {
		return ib_fail(error, "connection parameter block is too long");
	}
	dpb = (unsigned char *) malloc(dpb_length);
	if (dpb == NULL) {
		return ib_fail(error, "out of memory allocating connection parameters");
	}
	offset = 0U;
	dpb[offset++] = isc_dpb_version1;
	ib_dpb_add_string(dpb, &offset, isc_dpb_user_name, user, user_length);
	ib_dpb_add_string(dpb, &offset, isc_dpb_password, password, password_length);
	if (role_length != 0U) {
		ib_dpb_add_string(dpb, &offset, isc_dpb_sql_role_name, role, role_length);
	}
	ib_dpb_add_byte(dpb, &offset, isc_dpb_sql_dialect, (unsigned char) dialect);
	ib_dpb_add_string(dpb, &offset, isc_dpb_lc_ctype, charset_name,
		charset_name_length);
	if (encrypted_password_length != 0U) {
		ib_dpb_add_string(dpb, &offset, isc_dpb_password_enc,
			encrypted_password, encrypted_password_length);
	}
	if (system_encryption_password_length != 0U) {
		ib_dpb_add_string(dpb, &offset, isc_dpb_sys_encrypt_password,
			system_encryption_password, system_encryption_password_length);
	}
	if (connect_timeout != 0U) {
		ib_dpb_add_integer(dpb, &offset, isc_dpb_connect_timeout, connect_timeout);
	}
	if (create) {
		if (page_size != 0) {
			ib_dpb_add_integer(dpb, &offset, isc_dpb_page_size, (uint32_t) page_size);
		}
		ib_dpb_add_byte(dpb, &offset, isc_dpb_overwrite, 0U);
		ib_dpb_add_byte(dpb, &offset, isc_dpb_admin_option, 1U);
	}
	*dpb_result = dpb;
	*dpb_length_result = offset;
	*charset_result = charset_id;
	return 0;
}

static ib_connection *ib_connection_from_dpb(const char *database,
	size_t database_length, unsigned char *dpb, size_t dpb_length,
	short charset_id, int dialect, int create, char **error)
{
	ib_connection *connection;
	isc_db_handle database_handle;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	connection = (ib_connection *) calloc(1U, sizeof(*connection));
	if (connection == NULL) {
		free(dpb);
		ib_fail(error, "out of memory allocating connection state");
		return NULL;
	}
	database_handle = NULL;
	memset(status, 0, sizeof(status));
	if (create) {
		result = isc_create_database(status, (short) database_length,
			(char *) database, &database_handle, (short) dpb_length,
			(char *) dpb, 0);
	} else {
		result = isc_attach_database(status, (short) database_length,
			(char *) database, &database_handle, (short) dpb_length,
			(char *) dpb);
	}
	free(dpb);
	if (result != 0) {
		if (database_handle != NULL) {
			ISC_STATUS detach_status[IB_STATUS_VECTOR_LENGTH];
			memset(detach_status, 0, sizeof(detach_status));
			(void) isc_detach_database(detach_status, &database_handle);
		}
		ib_fail_status(error, create ? "create database" : "attach database", status);
		free(connection);
		return NULL;
	}
	if (database_handle == NULL) {
		free(connection);
		ib_fail(error, create ? "create database returned no handle" :
			"attach database returned no handle");
		return NULL;
	}
	connection->database = database_handle;
	connection->charset = charset_id;
	connection->dialect = dialect;
	return connection;
}

static ib_connection *ib_connection_open_or_create(const char *database,
	size_t database_length, const char *user, size_t user_length,
	const char *password, size_t password_length, const char *role,
	size_t role_length, const char *encrypted_password,
	size_t encrypted_password_length, const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, uint32_t connect_timeout, int page_size,
	int create, char **error)
{
	unsigned char *dpb;
	size_t dpb_length;
	short charset_id = 0;

	if (database == NULL || database_length == 0U || database_length > (size_t) SHRT_MAX) {
		ib_fail(error, "database name is empty or too long");
		return NULL;
	}
	if (ib_build_connection_dpb(user, user_length, password, password_length,
		role, role_length, encrypted_password, encrypted_password_length,
		system_encryption_password, system_encryption_password_length, charset,
		charset_length, dialect, connect_timeout, page_size, create, &dpb, &dpb_length,
		&charset_id, error) != 0) {
		return NULL;
	}
	return ib_connection_from_dpb(database, database_length, dpb, dpb_length,
		charset_id, dialect, create, error);
}

ib_connection *ib_connection_open(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, const char *role, size_t role_length,
	const char *encrypted_password, size_t encrypted_password_length,
	const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, uint32_t connect_timeout, char **error)
{
	return ib_connection_open_or_create(database, database_length, user, user_length,
		password, password_length, role, role_length, encrypted_password,
		encrypted_password_length, system_encryption_password,
		system_encryption_password_length, charset, charset_length, dialect,
		connect_timeout, 0, 0, error);
}

ib_connection *ib_connection_create(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, const char *role, size_t role_length,
	const char *encrypted_password, size_t encrypted_password_length,
	const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, int page_size, uint32_t connect_timeout,
	char **error)
{
	return ib_connection_open_or_create(database, database_length, user, user_length,
		password, password_length, role, role_length, encrypted_password,
		encrypted_password_length, system_encryption_password,
		system_encryption_password_length, charset, charset_length, dialect,
		connect_timeout, page_size, 1, error);
}

int ib_connection_set_default_tpbs(ib_connection *connection,
	const char *read_tpb, size_t read_tpb_length, const char *write_tpb,
	size_t write_tpb_length, char **error)
{
	char *read_copy;
	char *write_copy;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (read_tpb == NULL || read_tpb_length == 0U ||
		write_tpb == NULL || write_tpb_length == 0U ||
		read_tpb_length > (size_t) SHRT_MAX ||
		write_tpb_length > (size_t) SHRT_MAX) {
		return ib_fail(error, "default transaction parameter block is invalid");
	}
	read_copy = ib_copy_buffer(read_tpb, read_tpb_length);
	if (read_copy == NULL) {
		return ib_fail(error, "out of memory copying read-only transaction parameter block");
	}
	write_copy = ib_copy_buffer(write_tpb, write_tpb_length);
	if (write_copy == NULL) {
		free(read_copy);
		return ib_fail(error, "out of memory copying writable transaction parameter block");
	}
	free(connection->read_tpb);
	free(connection->write_tpb);
	connection->read_tpb = read_copy;
	connection->read_tpb_length = read_tpb_length;
	connection->write_tpb = write_copy;
	connection->write_tpb_length = write_tpb_length;
	return 0;
}

static void ib_connection_free_state(ib_connection *connection)
{
	if (connection == NULL) {
		return;
	}
	free(connection->read_tpb);
	free(connection->write_tpb);
	free(connection);
}

int ib_connection_close(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;
	int failed;

	if (connection == NULL) {
		return 0;
	}
	first_error = NULL;
	failed = 0;
	cursor_error = NULL;
	if (ib_close_all_cursors(connection, &cursor_error, connection->broken == 0) != 0) {
			failed = 1;
	}
	ib_append_error(&first_error, cursor_error);
	while (connection->statements != NULL) {
		cursor_error = NULL;
		if (ib_statement_close_internal(connection->statements, &cursor_error) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	if (connection->transaction != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_rollback_transaction(status, &connection->transaction);
		connection->transaction = NULL;
		if (result != 0) {
			failed = 1;
			cursor_error = NULL;
			(void) ib_fail_status(&cursor_error, "rollback transaction", status);
			ib_append_error(&first_error, cursor_error);
		}
	}
	if (connection->database != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_detach_database(status, &connection->database);
		connection->database = NULL;
		if (result != 0) {
			failed = 1;
			if (first_error == NULL) {
				(void) ib_fail_status(&first_error, "detach database", status);
			}
		}
	}
	ib_connection_free_state(connection);
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

int ib_connection_drop(ib_connection *connection, int *consumed, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (consumed == NULL) {
		return ib_fail(error, "drop ownership storage is unavailable");
	}
	*consumed = 0;
	if (connection == NULL) {
		return 0;
	}
	if (connection->database == NULL) {
		*consumed = 1;
		ib_connection_free_state(connection);
		return 0;
	}
	if (connection->transaction != NULL || connection->cursors != NULL ||
		connection->statements != NULL) {
		return ib_fail(error, "cannot drop database with active native resources");
	}

	memset(status, 0, sizeof(status));
	result = isc_drop_database(status, &connection->database);
	if (connection->database == NULL) {
		*consumed = 1;
		ib_connection_free_state(connection);
		if (result != 0) {
			return ib_fail_status(error, "drop database", status);
		}
		return 0;
	}
	if (result != 0) {
		return ib_fail_status(error, "drop database", status);
	}
	return ib_fail(error, "drop database returned a live handle");
}

int ib_client_version(char **version, char **error)
{
	char buffer[256];
	char *copy;
	size_t length;

	if (version == NULL) {
		return ib_fail(error, "client version storage is unavailable");
	}
	*version = NULL;
	memset(buffer, 0, sizeof(buffer));
	isc_get_client_version(buffer);
	buffer[sizeof(buffer) - 1U] = '\0';
	length = 0U;
	while (length < sizeof(buffer) && buffer[length] != '\0') {
		length++;
	}
	if (length == sizeof(buffer)) {
		return ib_fail(error, "client version is not NUL terminated");
	}
	copy = ib_copy_string(buffer, length);
	if (copy == NULL) {
		return ib_fail(error, "out of memory copying client version");
	}
	*version = copy;
	return 0;
}

int ib_connection_is_broken(const ib_connection *connection)
{
	return connection == NULL || connection->broken;
}

int ib_connection_begin(ib_connection *connection, int read_only, char **error)
{
	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction != NULL) {
		return ib_fail(error, "a transaction is already active");
	}
	if (ib_start_transaction(connection, &connection->transaction,
		read_only != 0, "start transaction", error) != 0) {
		return -1;
	}
	connection->transaction_read_only = read_only != 0;
	return 0;
}

int ib_connection_begin_tpb(ib_connection *connection, const char *tpb,
	size_t tpb_length, char **error)
{
	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction != NULL) {
		return ib_fail(error, "a transaction is already active");
	}
	if (ib_start_transaction_tpb(connection, &connection->transaction, tpb,
		tpb_length, "start transaction", error) != 0) {
		return -1;
	}
	connection->transaction_read_only = ib_tpb_is_read_only(tpb, tpb_length);
	return 0;
}

int ib_connection_commit(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	first_error = NULL;
	cursor_error = NULL;
	if (ib_close_transaction_cursors(connection, connection->transaction,
		&cursor_error) != 0) {
			ib_append_error(&first_error, cursor_error);
			ib_give_error(error, first_error);
			return -1;
	}
	ib_append_error(&first_error, cursor_error);
	memset(status, 0, sizeof(status));
	result = isc_commit_transaction(status, &connection->transaction);
	if (result != 0) {
		ib_mark_broken(connection);
		(void) ib_fail_status(&first_error, "commit transaction", status);
		ib_give_error(error, first_error);
		return -1;
	}
	connection->transaction = NULL;
	connection->transaction_read_only = 0;
	ib_give_error(error, first_error);
	return 0;
}

static int ib_connection_rollback_internal(ib_connection *connection,
	char **error, int cleanup)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *first_error;
	char *cursor_error;
	int failed;

	if (connection == NULL || connection->database == NULL ||
		(!cleanup && connection->broken)) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	first_error = NULL;
	failed = 0;
	cursor_error = NULL;
	if (ib_close_transaction_cursors(connection, connection->transaction,
		&cursor_error) != 0) {
		failed = 1;
	}
	ib_append_error(&first_error, cursor_error);
	memset(status, 0, sizeof(status));
	result = isc_rollback_transaction(status, &connection->transaction);
	if (connection->transaction == NULL) {
		connection->transaction_read_only = 0;
	}
	if (result != 0) {
		failed = 1;
		ib_mark_broken(connection);
		cursor_error = NULL;
		(void) ib_fail_status(&cursor_error, "rollback transaction", status);
		ib_append_error(&first_error, cursor_error);
	}
	if (failed) {
		ib_give_error(error, first_error);
		return -1;
	}
	return 0;
}

int ib_connection_rollback(ib_connection *connection, char **error)
{
	return ib_connection_rollback_internal(connection, error, 0);
}

int ib_connection_rollback_cleanup(ib_connection *connection, char **error)
{
	return ib_connection_rollback_internal(connection, error, 1);
}

int ib_connection_transaction_state(const ib_connection *connection)
{
	if (connection == NULL) {
		return IB_HANDLE_UNKNOWN;
	}
	return connection->transaction == NULL ? IB_HANDLE_CONSUMED : IB_HANDLE_LIVE;
}

int ib_connection_commit_retaining(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	memset(status, 0, sizeof(status));
	result = isc_commit_retaining(status, &connection->transaction);
	if (result != 0) {
		ib_mark_broken(connection);
		return ib_fail_status(error, "commit retaining transaction", status);
	}
	return 0;
}

int ib_connection_rollback_retaining(ib_connection *connection, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	memset(status, 0, sizeof(status));
	result = isc_rollback_retaining(status, &connection->transaction);
	if (result != 0) {
		ib_mark_broken(connection);
		return ib_fail_status(error, "rollback retaining transaction", status);
	}
	return 0;
}

static int ib_information_response(ib_connection *connection, unsigned char item,
	int transaction, char **response, size_t *response_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char request[1];
	char *buffer;
	size_t capacity;
	size_t payload_length;
	size_t used;
	size_t cursor;
	size_t next_payload_length;
	int retry;

	if (response == NULL || response_length == NULL) {
		return ib_fail(error, "information response storage is unavailable");
	}
	*response = NULL;
	*response_length = 0U;
	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (transaction && connection->transaction == NULL) {
		return ib_fail(error, "no active transaction");
	}
	request[0] = (char) item;
	capacity = 256U;
	for (;;) {
		buffer = (char *) calloc(capacity, 1U);
		if (buffer == NULL) {
			return ib_fail(error, "out of memory allocating information response");
		}
		memset(status, 0, sizeof(status));
		if (transaction) {
			result = isc_transaction_info(status, &connection->transaction,
				1, request, (short) capacity, buffer);
		} else {
			result = isc_database_info(status, &connection->database,
				1, request, (short) capacity, buffer);
		}
		if (result != 0) {
			free(buffer);
			return ib_fail_status(error, transaction ?
				"transaction information" : "database information", status);
		}
		if ((unsigned char) buffer[0] == isc_info_truncated || capacity < 4U) {
			free(buffer);
			if (capacity >= (size_t) SHRT_MAX) {
				return ib_fail(error, "information response exceeds the native limit");
			}
			capacity *= 2U;
			if (capacity > (size_t) SHRT_MAX) {
				capacity = (size_t) SHRT_MAX;
			}
			continue;
		}
		payload_length = (size_t) (unsigned char) buffer[1] |
			((size_t) (unsigned char) buffer[2] << 8U);
		if (payload_length > SIZE_MAX - 4U) {
			free(buffer);
			return ib_fail(error, "information response length overflowed");
		}
		used = 3U + payload_length;
		if (used >= capacity) {
			free(buffer);
			if (capacity < (size_t) SHRT_MAX) {
				capacity *= 2U;
				if (capacity > (size_t) SHRT_MAX) {
					capacity = (size_t) SHRT_MAX;
				}
				continue;
			}
			return ib_fail(error, "malformed information response");
		}
		if (!transaction) {
			if ((unsigned char) buffer[used] != isc_info_end) {
				free(buffer);
				return ib_fail(error, "malformed information response");
			}
			*response = buffer;
			*response_length = used + 1U;
			return 0;
		}
		/* A transaction-info response contains one item record per participant.
		 * Keep the complete ordered sequence. The caller that needs one value
		 * may deliberately select a record, but this layer must not discard the
		 * remaining participant identifiers. */
		cursor = used;
		retry = 0;
		for (;;) {
			if (cursor >= capacity) {
				retry = 1;
				break;
			}
			if ((unsigned char) buffer[cursor] == isc_info_end) {
				break;
			}
			if ((unsigned char) buffer[cursor] != (unsigned char) item ||
				capacity - cursor < 3U) {
				if (capacity - cursor < 3U) {
					retry = 1;
					break;
				}
				free(buffer);
				return ib_fail(error, "malformed transaction information response");
			}
			next_payload_length = (size_t) (unsigned char) buffer[cursor + 1U] |
				((size_t) (unsigned char) buffer[cursor + 2U] << 8U);
			if (next_payload_length > capacity - cursor - 3U) {
				retry = 1;
				break;
			}
			cursor += 3U + next_payload_length;
		}
		if (retry) {
			free(buffer);
			if (capacity < (size_t) SHRT_MAX) {
				capacity *= 2U;
				if (capacity > (size_t) SHRT_MAX) {
					capacity = (size_t) SHRT_MAX;
				}
				continue;
			}
			return ib_fail(error, "malformed information response");
		}
		*response = buffer;
		*response_length = cursor + 1U;
		return 0;
	}
}

int ib_connection_database_info(ib_connection *connection, unsigned char item,
	char **response, size_t *response_length, char **error)
{
	return ib_information_response(connection, item, 0, response,
		response_length, error);
}

int ib_connection_transaction_info(ib_connection *connection, unsigned char item,
	char **response, size_t *response_length, char **error)
{
	return ib_information_response(connection, item, 1, response,
		response_length, error);
}

ib_bindings *ib_bindings_new(size_t count, char **error)
{
	ib_bindings *bindings;

	if (count > (size_t) SHRT_MAX) {
		ib_fail(error, "too many query arguments");
		return NULL;
	}
	bindings = (ib_bindings *) calloc(1U, sizeof(*bindings));
	if (bindings == NULL) {
		ib_fail(error, "out of memory allocating query arguments");
		return NULL;
	}
	bindings->count = count;
	if (count != 0U) {
		if (count > SIZE_MAX / sizeof(*bindings->values)) {
			free(bindings);
			ib_fail(error, "too many query arguments");
			return NULL;
		}
		bindings->values = (ib_bind_value *) calloc(count, sizeof(*bindings->values));
		if (bindings->values == NULL) {
			free(bindings);
			ib_fail(error, "out of memory allocating query arguments");
			return NULL;
		}
	}
	return bindings;
}

static void ib_array_value_free(ib_array_value *array)
{
	size_t index;

	if (array == NULL) {
		return;
	}
	if (array->elements != NULL) {
		for (index = 0U; index < array->element_count; index++) {
			free((void *) array->elements[index].bytes);
		}
	}
	free(array->elements);
	free(array->bounds);
	free(array);
}

static void ib_bind_value_clear(ib_bind_value *value)
{
	if (value == NULL) {
		return;
	}
	free(value->bytes);
	ib_array_value_free(value->array);
	memset(value, 0, sizeof(*value));
}

void ib_bindings_free(ib_bindings *bindings)
{
	size_t index;

	if (bindings == NULL) {
		return;
	}
	for (index = 0U; index < bindings->count; index++) {
		ib_bind_value_clear(&bindings->values[index]);
	}
	free(bindings->values);
	free(bindings);
}

static int ib_binding_index(ib_bindings *bindings, size_t index, char **error)
{
	if (bindings == NULL || index >= bindings->count) {
		return ib_fail(error, "query argument index is out of bounds");
	}
	return 0;
}

static int ib_array_element_kind_valid(int kind)
{
	switch (kind) {
	case IB_ARRAY_ELEMENT_STRING:
	case IB_ARRAY_ELEMENT_INT64:
	case IB_ARRAY_ELEMENT_FLOAT64:
	case IB_ARRAY_ELEMENT_BOOL:
	case IB_ARRAY_ELEMENT_TIMESTAMP:
	case IB_ARRAY_ELEMENT_BYTES:
		return 1;
	default:
		return 0;
	}
}

static int ib_array_shape_valid(const ib_array_bound *bounds, size_t dimensions,
	size_t element_count, char **error)
{
	size_t index;
	size_t expected;
	uint64_t width;

	if (bounds == NULL || dimensions == 0U || dimensions > 16U) {
		return ib_fail(error, "array dimensions are invalid");
	}
	expected = 1U;
	for (index = 0U; index < dimensions; index++) {
		if (bounds[index].lower < INT32_C(SHRT_MIN) ||
			bounds[index].lower > INT32_C(SHRT_MAX) ||
			bounds[index].upper < INT32_C(SHRT_MIN) ||
			bounds[index].upper > INT32_C(SHRT_MAX) ||
			bounds[index].lower > bounds[index].upper) {
			return ib_fail(error, "array bounds are invalid");
		}
		width = (uint64_t) ((int64_t) bounds[index].upper -
			(int64_t) bounds[index].lower) + UINT64_C(1);
		if (width > (uint64_t) SIZE_MAX / expected) {
			return ib_fail(error, "array element count is too large");
		}
		expected *= (size_t) width;
	}
	if (expected != element_count) {
		return ib_fail(error, "array element count does not match its bounds");
	}
	return 0;
}

int ib_bindings_set_array(ib_bindings *bindings, size_t index,
	const ib_array_bound *bounds, size_t dimensions,
	const ib_array_element *elements, size_t element_count, char **error)
{
	ib_array_value *array;
	size_t element_index;

	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	if (ib_array_shape_valid(bounds, dimensions, element_count, error) != 0) {
		return -1;
	}
	if (element_count != 0U && elements == NULL) {
		return ib_fail(error, "array elements are unavailable");
	}
	array = (ib_array_value *) calloc(1U, sizeof(*array));
	if (array == NULL) {
		return ib_fail(error, "out of memory allocating array value");
	}
	array->dimensions = dimensions;
	array->element_count = element_count;
	array->bounds = (ib_array_bound *) malloc(dimensions * sizeof(*array->bounds));
	if (array->bounds == NULL) {
		ib_array_value_free(array);
		return ib_fail(error, "out of memory copying array bounds");
	}
	memcpy(array->bounds, bounds, dimensions * sizeof(*array->bounds));
	if (element_count != 0U) {
		array->elements = (ib_array_element *) calloc(element_count,
			sizeof(*array->elements));
		if (array->elements == NULL) {
			ib_array_value_free(array);
			return ib_fail(error, "out of memory copying array elements");
		}
	}
	for (element_index = 0U; element_index < element_count; element_index++) {
		const ib_array_element *source = &elements[element_index];
		ib_array_element *target = &array->elements[element_index];

		if (!ib_array_element_kind_valid(source->kind)) {
			ib_array_value_free(array);
			return ib_fail(error, "array element kind is unsupported");
		}
		if ((source->kind == IB_ARRAY_ELEMENT_STRING ||
			source->kind == IB_ARRAY_ELEMENT_BYTES) &&
			source->length != 0U && source->bytes == NULL) {
			ib_array_value_free(array);
			return ib_fail(error, "array element data is unavailable");
		}
		*target = *source;
		target->bytes = NULL;
		if (source->length != 0U) {
			target->bytes = ib_copy_buffer(source->bytes, source->length);
			if (target->bytes == NULL) {
				ib_array_value_free(array);
				return ib_fail(error, "out of memory copying array element");
			}
		}
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_ARRAY;
	bindings->values[index].array = array;
	return 0;
}

int ib_bindings_set_blob_ref(ib_bindings *bindings, size_t index,
	int32_t high, uint32_t low, int subtype, int charset, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	if (high == 0 && low == 0U) {
		return ib_fail(error, "BLOB reference is empty");
	}
	if (subtype < SHRT_MIN || subtype > SHRT_MAX ||
		charset < SHRT_MIN || charset > SHRT_MAX) {
		return ib_fail(error, "BLOB reference descriptor is outside the native range");
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_BLOB_REF;
	bindings->values[index].blob_high = high;
	bindings->values[index].blob_low = low;
	bindings->values[index].blob_subtype = subtype;
	bindings->values[index].blob_charset = charset;
	return 0;
}

int ib_bindings_set_null(ib_bindings *bindings, size_t index, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_NULL;
	return 0;
}

static int ib_bindings_set_encoded_string(ib_bindings *bindings, size_t index,
	const char *value, size_t length, short source_charset, char **error)
{
	char *copy;

	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	if (length != 0U && value == NULL) {
		return ib_fail(error, "string query argument is missing data");
	}
	copy = ib_copy_string(value, length);
	if (copy == NULL) {
		return ib_fail(error, "out of memory copying string query argument");
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_STRING;
	bindings->values[index].bytes = copy;
	bindings->values[index].length = length;
	bindings->values[index].source_charset = source_charset;
	return 0;
}

int ib_bindings_set_string(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error)
{
	return ib_bindings_set_encoded_string(bindings, index, value, length,
		IB_CHARSET_UTF8, error);
}

int ib_bindings_set_bytes(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error)
{
	char *copy;

	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	if (length != 0U && value == NULL) {
		return ib_fail(error, "byte query argument is missing data");
	}
	copy = ib_copy_string(value, length);
	if (copy == NULL) {
		return ib_fail(error, "out of memory copying byte query argument");
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_BYTES;
	bindings->values[index].bytes = copy;
	bindings->values[index].length = length;
	return 0;
}

int ib_bindings_set_int64(ib_bindings *bindings, size_t index,
	int64_t value, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_INT64;
	bindings->values[index].int64_value = value;
	return 0;
}

int ib_bindings_set_float64(ib_bindings *bindings, size_t index,
	double value, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_FLOAT64;
	bindings->values[index].float64_value = value;
	return 0;
}

int ib_bindings_set_bool(ib_bindings *bindings, size_t index, int value,
	char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	ib_bind_value_clear(&bindings->values[index]);
	bindings->values[index].kind = IB_ARGUMENT_BOOL;
	bindings->values[index].bool_value = value != 0;
	return 0;
}

int ib_bindings_set_timestamp(ib_bindings *bindings, size_t index,
	int64_t year, int month, int day, int hour, int minute, int second,
	int nanosecond, char **error)
{
	if (ib_binding_index(bindings, index, error) != 0) {
		return -1;
	}
	free(bindings->values[index].bytes);
	memset(&bindings->values[index], 0, sizeof(bindings->values[index]));
	bindings->values[index].kind = IB_ARGUMENT_TIMESTAMP;
	bindings->values[index].year = year;
	bindings->values[index].month = month;
	bindings->values[index].day = day;
	bindings->values[index].hour = hour;
	bindings->values[index].minute = minute;
	bindings->values[index].second = second;
	bindings->values[index].nanosecond = nanosecond;
	return 0;
}

ib_cursor *ib_connection_query(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, int allow_arrays,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	char *prepared_query;
	size_t prepared_query_length;
	int statement_type;
	int implicit;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		ib_fail(error, "connection is unavailable");
		return NULL;
	}
	if (query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		ib_fail(error, "query is empty or too long");
		return NULL;
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		ib_fail(error, "query arguments are invalid");
		return NULL;
	}
	prepared_query = ib_convert_utf8(query, query_length, connection->charset,
		&prepared_query_length, error);
	if (prepared_query == NULL) {
		return NULL;
	}
	if (prepared_query_length > (size_t) USHRT_MAX) {
		free(prepared_query);
		ib_fail(error, "converted query is too long");
		return NULL;
	}

	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		free(prepared_query);
		ib_fail(error, "out of memory allocating cursor state");
		return NULL;
	}
	cursor->connection = connection;
	cursor->allow_arrays = allow_arrays != 0;
	cursor->query = prepared_query;
	cursor->query_length = prepared_query_length;
	prepared_query = NULL;
	ib_cursor_register(cursor);
	if (connection->transaction != NULL) {
		cursor->transaction = connection->transaction;
		cursor->owns_transaction = 0;
		implicit = 0;
	} else if (ib_start_transaction(connection, &cursor->transaction, 1,
		"start read transaction", error) != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	} else {
		cursor->owns_transaction = 1;
		implicit = 1;
	}
	if (ib_cursor_prepare_statement(cursor, cursor->query, cursor->query_length,
		error) != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (ib_statement_type(cursor, &statement_type, error) != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (implicit && statement_type == isc_info_sql_stmt_exec_procedure) {
		if (ib_cursor_drop_statement(cursor, error) != 0 ||
			ib_cursor_rollback_owned_transaction(cursor, error) != 0) {
			goto procedure_transition_failure;
		}
		if (ib_start_transaction(connection, &cursor->transaction, 0,
			"start write transaction", error) != 0) {
			goto procedure_transition_failure;
		}
		cursor->owns_transaction = 1;
		if (ib_cursor_prepare_statement(cursor, cursor->query, cursor->query_length,
			error) != 0) {
			goto procedure_transition_failure;
		}
		if (ib_statement_type(cursor, &statement_type, error) != 0) {
			goto procedure_transition_failure;
		}
	}
	if (statement_type == isc_info_sql_stmt_select_for_upd &&
		(implicit || connection->transaction_read_only)) {
		free(prepared_query);
		prepared_query = NULL;
		(void) ib_fail(error,
			"SELECT FOR UPDATE requires an explicit writable transaction");
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (statement_type != isc_info_sql_stmt_select &&
		statement_type != isc_info_sql_stmt_select_for_upd &&
		statement_type != isc_info_sql_stmt_exec_procedure) {
		free(prepared_query);
		prepared_query = NULL;
		(void) ib_fail(error,
			"only SELECT, SELECT FOR UPDATE, and executable procedure statements are permitted");
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	free(prepared_query);
	prepared_query = NULL;
	cursor->statement_type = statement_type;
	if (ib_describe_bind(cursor, error) != 0 ||
		ib_bind_input_mode(cursor, bindings, cursor->allow_arrays, error) != 0 ||
		ib_describe_output(cursor, error) != 0 ||
		ib_cursor_describe_metadata(cursor, error) != 0 ||
		ib_validate_output_types_mode(cursor->output, cursor->allow_arrays, error) != 0 ||
		ib_allocate_output(cursor, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (statement_type == isc_info_sql_stmt_exec_procedure &&
		cursor->output->sqld == 0) {
		(void) ib_fail(error, "procedure has no output; use Exec instead");
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute2(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input,
			statement_type == isc_info_sql_stmt_exec_procedure ? cursor->output : NULL);
	}
	if (result != 0) {
		(void) ib_fail_status(error,
			statement_type == isc_info_sql_stmt_exec_procedure ?
			"execute procedure" : "execute SELECT", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	cursor->statement_type = statement_type;
	cursor->procedure = statement_type == isc_info_sql_stmt_exec_procedure;
	cursor->output_pending = cursor->procedure && cursor->output->sqld > 0;
	return cursor;

procedure_transition_failure:
	free(prepared_query);
	prepared_query = NULL;
	ib_failed_query_cleanup(cursor, error);
	return NULL;
}

int ib_connection_exec(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, int64_t *rows_affected,
	int allow_arrays, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	char *prepared_query;
	char *cleanup_error;
	char *rollback_error;
	char *commit_error;
	isc_tr_handle implicit_transaction;
	size_t prepared_query_length;
	int close_result;
	int implicit;

	if (rows_affected == NULL) {
		return ib_fail(error, "affected-row destination is unavailable");
	}
	*rows_affected = -1;
	if (connection == NULL || connection->database == NULL || connection->broken) {
		return ib_fail(error, "connection is unavailable");
	}
	if (query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		return ib_fail(error, "query is empty or too long");
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		return ib_fail(error, "query arguments are invalid");
	}
	prepared_query = ib_convert_utf8(query, query_length, connection->charset,
		&prepared_query_length, error);
	if (prepared_query == NULL) {
		return -1;
	}
	if (prepared_query_length > (size_t) USHRT_MAX) {
		free(prepared_query);
		return ib_fail(error, "converted query is too long");
	}

	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		free(prepared_query);
		return ib_fail(error, "out of memory allocating cursor state");
	}
	cursor->connection = connection;
	cursor->allow_arrays = allow_arrays != 0;
	cursor->query = prepared_query;
	cursor->query_length = prepared_query_length;
	prepared_query = NULL;
	ib_cursor_register(cursor);
	implicit = 0;
	if (connection->transaction != NULL) {
		cursor->transaction = connection->transaction;
		cursor->owns_transaction = 0;
	} else {
		if (ib_start_transaction(connection, &cursor->transaction, 0,
			"start write transaction", error) != 0) {
			free(prepared_query);
			prepared_query = NULL;
			ib_failed_query_cleanup(cursor, error);
			return -1;
		}
		cursor->owns_transaction = 1;
		implicit = 1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &connection->database,
		&cursor->statement);
	if (result != 0) {
		free(prepared_query);
		prepared_query = NULL;
		ib_fail_status(error, "allocate statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &cursor->transaction, &cursor->statement,
		(unsigned short) cursor->query_length, cursor->query,
		ib_connection_dialect(cursor->connection), NULL);
	free(prepared_query);
	prepared_query = NULL;
	if (result != 0) {
		ib_fail_status(error, "prepare statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_statement_type(cursor, &cursor->statement_type, error) != 0 ||
		(cursor->statement_type == isc_info_sql_stmt_exec_procedure &&
			ib_describe_output(cursor, error) != 0) ||
		ib_statement_rejects_result(cursor, error) != 0 ||
		ib_describe_bind(cursor, error) != 0 ||
		ib_bind_input_mode(cursor, bindings, cursor->allow_arrays, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input);
	}
	if (result != 0) {
		ib_fail_status(error, "execute statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_statement_rows_affected(cursor, rows_affected, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}

	implicit_transaction = NULL;
	if (implicit) {
		implicit_transaction = cursor->transaction;
		cursor->owns_transaction = 0;
	}
	cleanup_error = NULL;
	close_result = ib_cursor_close_internal(cursor, &cleanup_error, 0);
	cursor = NULL;
	if (close_result != 0) {
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed write", rollback_status);
			}
			ib_append_error(&cleanup_error, rollback_error);
		}
		ib_give_error(error, cleanup_error);
		return -1;
	}
	if (implicit_transaction == NULL) {
		ib_give_error(error, cleanup_error);
		return 0;
	}

	memset(status, 0, sizeof(status));
	result = isc_commit_transaction(status, &implicit_transaction);
	if (result != 0) {
		ib_mark_broken(connection);
		commit_error = NULL;
		(void) ib_fail_status(&commit_error, "commit write transaction", status);
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed write", rollback_status);
			}
			ib_append_error(&commit_error, rollback_error);
		}
		ib_append_error(&commit_error, cleanup_error);
		ib_give_error(error, commit_error);
		return -1;
	}
	ib_give_error(error, cleanup_error);
	return 0;
}

static ib_statement *ib_statement_prepare_mode(ib_connection *connection,
	const char *query,
	size_t query_length, int allow_arrays, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_statement *statement;
	ib_cursor descriptor;
	char *prepared_query;
	char *failure_error;
	char *cleanup_error;
	char *rollback_error;
	isc_tr_handle prepare_transaction;
	size_t prepared_query_length;
	int owns_transaction;
	int statement_type;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		ib_fail(error, "connection is unavailable");
		return NULL;
	}
	if (query == NULL || query_length == 0U || query_length > (size_t) USHRT_MAX) {
		ib_fail(error, "query is empty or too long");
		return NULL;
	}
	prepared_query = ib_convert_utf8(query, query_length, connection->charset,
		&prepared_query_length, error);
	if (prepared_query == NULL) {
		return NULL;
	}
	if (prepared_query_length > (size_t) USHRT_MAX) {
		free(prepared_query);
		ib_fail(error, "converted query is too long");
		return NULL;
	}
	statement = (ib_statement *) calloc(1U, sizeof(*statement));
	if (statement == NULL) {
		free(prepared_query);
		ib_fail(error, "out of memory allocating prepared statement");
		return NULL;
	}
	statement->connection = connection;
	statement->query = prepared_query;
	statement->query_length = prepared_query_length;
	prepared_query = NULL;
	prepare_transaction = NULL;
	owns_transaction = 0;
	failure_error = NULL;
	cleanup_error = NULL;
	rollback_error = NULL;
	memset(&descriptor, 0, sizeof(descriptor));
	if (connection->transaction != NULL) {
		prepare_transaction = connection->transaction;
	} else {
		if (ib_start_transaction(connection, &prepare_transaction, 1,
			"start prepare transaction", &failure_error) != 0) {
			goto fail;
		}
		owns_transaction = 1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_allocate_statement(status, &connection->database,
		&statement->statement);
	if (result != 0) {
		(void) ib_fail_status(&failure_error, "allocate statement", status);
		goto fail;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_prepare(status, &prepare_transaction, &statement->statement,
		(unsigned short) statement->query_length, statement->query,
		ib_connection_dialect(connection), NULL);
	if (result != 0) {
		(void) ib_fail_status(&failure_error, "prepare statement", status);
		goto fail;
	}
	descriptor.connection = connection;
	descriptor.transaction = prepare_transaction;
	descriptor.statement = statement->statement;
	if (ib_describe_bind(&descriptor, &failure_error) != 0) {
		goto fail;
	}
	statement->input = descriptor.input;
	descriptor.input = NULL;
	if (ib_statement_type(&descriptor, &statement_type, &failure_error) != 0) {
		goto fail;
	}
	statement->statement_type = statement_type;
	if (statement_type == isc_info_sql_stmt_select ||
		statement_type == isc_info_sql_stmt_select_for_upd ||
		statement_type == isc_info_sql_stmt_exec_procedure) {
		if (ib_describe_output(&descriptor, &failure_error) != 0 ||
			ib_validate_output_types_mode(descriptor.output, allow_arrays,
				&failure_error) != 0) {
			goto fail;
		}
		statement->output = descriptor.output;
		descriptor.output = NULL;
	}
	if (owns_transaction) {
		memset(status, 0, sizeof(status));
		result = isc_commit_transaction(status, &prepare_transaction);
		if (result != 0) {
			ib_mark_broken(connection);
			(void) ib_fail_status(&failure_error, "commit prepare transaction", status);
			goto fail;
		}
	}
	statement->next = connection->statements;
	connection->statements = statement;
	ib_free_sqlda(descriptor.input);
	ib_free_sqlda(descriptor.output);
	return statement;

fail:
	free(prepared_query);
	ib_free_sqlda(descriptor.input);
	ib_free_sqlda(descriptor.output);
	if (statement != NULL) {
		if (ib_statement_close_internal(statement, &cleanup_error) != 0) {
			ib_mark_broken(connection);
		}
	}
	if (owns_transaction && prepare_transaction != NULL) {
		memset(status, 0, sizeof(status));
		result = isc_rollback_transaction(status, &prepare_transaction);
		if (result != 0) {
			ib_mark_broken(connection);
			(void) ib_fail_status(&rollback_error, "rollback prepare transaction", status);
		}
	}
	ib_append_error(&failure_error, cleanup_error);
	ib_append_error(&failure_error, rollback_error);
	ib_give_error(error, failure_error);
	return NULL;
}

ib_statement *ib_statement_prepare(ib_connection *connection, const char *query,
	size_t query_length, char **error)
{
	return ib_statement_prepare_mode(connection, query, query_length, 0, error);
}

int ib_statement_num_input(const ib_statement *statement)
{
	if (statement == NULL || statement->input == NULL || statement->input->sqld < 0) {
		return 0;
	}
	return (int) statement->input->sqld;
}

int ib_statement_exec(ib_statement *statement, const ib_bindings *bindings,
	ib_cancel_slot *cancel, uint64_t generation, int64_t *rows_affected,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;
	char *cleanup_error;
	char *rollback_error;
	char *commit_error;
	isc_tr_handle implicit_transaction;
	int close_result;

	if (rows_affected == NULL) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "affected-row destination is unavailable");
	}
	*rows_affected = -1;
	if (statement == NULL || statement->connection == NULL ||
		statement->statement == NULL || statement->connection->database == NULL ||
		statement->connection->broken) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "prepared statement is unavailable");
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "query arguments are invalid");
	}
	if (statement->active_cursor != NULL) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "prepared statement already has an active execution");
	}
	if (statement->statement_type == isc_info_sql_stmt_select ||
		statement->statement_type == isc_info_sql_stmt_select_for_upd ||
		(statement->statement_type == isc_info_sql_stmt_exec_procedure &&
			(statement->output == NULL || statement->output->sqld != 0))) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "statement produces a result set; use Query instead");
	}
	if (ib_statement_rejects_transaction_control_type(statement->statement_type,
		error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		return -1;
	}
	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "out of memory allocating prepared execution state");
	}
	cursor->connection = statement->connection;
	cursor->prepared = statement;
	cursor->statement = statement->statement;
	cursor->statement_type = statement->statement_type;
	statement->active_cursor = cursor;
	ib_cursor_register(cursor);
	if (statement->connection->transaction != NULL) {
		cursor->transaction = statement->connection->transaction;
		cursor->owns_transaction = 0;
	} else if (ib_start_transaction(statement->connection, &cursor->transaction, 0,
		"start write transaction", error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	} else {
		cursor->owns_transaction = 1;
	}
	cursor->input = ib_clone_sqlda(statement->input, error);
	if (cursor->input == NULL || ib_bind_input(cursor, bindings, error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_cancel_slot_publish_if_active(cancel, generation, &cursor->statement,
		error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input);
	}
	ib_cancel_slot_complete(cancel, generation);
	if (result != 0) {
		(void) ib_fail_status(error, "execute prepared statement", status);
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	if (ib_statement_rows_affected(cursor, rows_affected, error) != 0) {
		ib_failed_query_cleanup(cursor, error);
		return -1;
	}
	implicit_transaction = NULL;
	if (cursor->owns_transaction) {
		implicit_transaction = cursor->transaction;
		cursor->owns_transaction = 0;
	}
	cleanup_error = NULL;
	close_result = ib_cursor_close_internal(cursor, &cleanup_error, 0);
	cursor = NULL;
	if (close_result != 0) {
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed prepared write", rollback_status);
			}
			ib_append_error(&cleanup_error, rollback_error);
		}
		ib_give_error(error, cleanup_error);
		return -1;
	}
	if (implicit_transaction == NULL) {
		ib_give_error(error, cleanup_error);
		return 0;
	}
	memset(status, 0, sizeof(status));
	result = isc_commit_transaction(status, &implicit_transaction);
	if (result != 0) {
		commit_error = NULL;
			ib_mark_broken(statement->connection);
		(void) ib_fail_status(&commit_error, "commit prepared write transaction", status);
		if (implicit_transaction != NULL) {
			ISC_STATUS rollback_status[IB_STATUS_VECTOR_LENGTH];

			rollback_error = NULL;
			memset(rollback_status, 0, sizeof(rollback_status));
			if (isc_rollback_transaction(rollback_status, &implicit_transaction) != 0) {
				(void) ib_fail_status(&rollback_error, "rollback failed prepared write", rollback_status);
			}
			ib_append_error(&commit_error, rollback_error);
		}
		ib_append_error(&commit_error, cleanup_error);
		ib_give_error(error, commit_error);
		return -1;
	}
	ib_give_error(error, cleanup_error);
	return 0;
}

ib_cursor *ib_statement_query(ib_statement *statement,
	const ib_bindings *bindings, ib_cancel_slot *cancel, uint64_t generation,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ib_cursor *cursor;

	if (statement == NULL || statement->connection == NULL ||
		statement->statement == NULL || statement->connection->database == NULL ||
		statement->connection->broken) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error, "prepared statement is unavailable");
		return NULL;
	}
	if (bindings == NULL || bindings->count > (size_t) SHRT_MAX) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error, "query arguments are invalid");
		return NULL;
	}
	if (statement->active_cursor != NULL) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error, "prepared statement already has an active execution");
		return NULL;
	}
	if (statement->statement_type != isc_info_sql_stmt_select &&
		statement->statement_type != isc_info_sql_stmt_select_for_upd &&
		statement->statement_type != isc_info_sql_stmt_exec_procedure) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error,
			"only SELECT, SELECT FOR UPDATE, and executable procedure statements are permitted");
		return NULL;
	}
	if (statement->statement_type == isc_info_sql_stmt_select_for_upd &&
		(statement->connection->transaction == NULL ||
			statement->connection->transaction_read_only)) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error, "SELECT FOR UPDATE requires an explicit writable transaction");
		return NULL;
	}
	if (statement->statement_type == isc_info_sql_stmt_exec_procedure &&
		(statement->output == NULL || statement->output->sqld == 0)) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error, "procedure has no output; use Exec instead");
		return NULL;
	}
	cursor = (ib_cursor *) calloc(1U, sizeof(*cursor));
	if (cursor == NULL) {
		ib_cancel_slot_complete(cancel, generation);
		ib_fail(error, "out of memory allocating prepared cursor state");
		return NULL;
	}
	cursor->connection = statement->connection;
	cursor->prepared = statement;
	cursor->statement = statement->statement;
	cursor->statement_type = statement->statement_type;
	statement->active_cursor = cursor;
	ib_cursor_register(cursor);
	if (statement->connection->transaction != NULL) {
		cursor->transaction = statement->connection->transaction;
		cursor->owns_transaction = 0;
	} else if (ib_start_transaction(statement->connection, &cursor->transaction,
		statement->statement_type == isc_info_sql_stmt_exec_procedure ? 0 : 1,
		statement->statement_type == isc_info_sql_stmt_exec_procedure ?
		"start write transaction" :
		"start read transaction", error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	} else {
		cursor->owns_transaction = 1;
	}
	cursor->input = ib_clone_sqlda(statement->input, error);
	if (cursor->input == NULL || ib_bind_input(cursor, bindings, error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	cursor->output = ib_clone_sqlda(statement->output, error);
	if (cursor->output == NULL || ib_cursor_describe_metadata(cursor, error) != 0 ||
		ib_validate_output_types(cursor->output, error) != 0 ||
		ib_allocate_output(cursor, error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	if (ib_cancel_slot_publish_if_active(cancel, generation, &cursor->statement,
		error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	memset(status, 0, sizeof(status));
	{
		XSQLDA *input = cursor->input->sqld == 0 ? NULL : cursor->input;
		result = isc_dsql_execute2(status, &cursor->transaction, &cursor->statement,
			ib_connection_dialect(cursor->connection), input,
			statement->statement_type == isc_info_sql_stmt_exec_procedure ?
			cursor->output : NULL);
	}
	ib_cancel_slot_complete(cancel, generation);
	if (result != 0) {
		(void) ib_fail_status(error,
			statement->statement_type == isc_info_sql_stmt_exec_procedure ?
			"execute prepared procedure" : "execute prepared SELECT", status);
		ib_failed_query_cleanup(cursor, error);
		return NULL;
	}
	cursor->procedure = statement->statement_type == isc_info_sql_stmt_exec_procedure;
	cursor->output_pending = cursor->procedure && cursor->output->sqld > 0;
	cursor->server_cursor_open = !cursor->procedure;
	return cursor;
}

int ib_statement_close(ib_statement *statement, char **error)
{
	return ib_statement_close_internal(statement, error);
}

int ib_statement_plan(ib_statement *statement, char **plan,
	size_t *plan_length, char **error)
{
	static const char request[] = {isc_info_sql_get_plan, isc_info_end};
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *response;
	char *converted;
	size_t capacity;
	size_t payload_length;
	size_t used;
	size_t converted_length;

	if (plan == NULL || plan_length == NULL) {
		return ib_fail(error, "plan response storage is unavailable");
	}
	*plan = NULL;
	*plan_length = 0U;
	if (statement == NULL || statement->connection == NULL ||
		statement->statement == NULL || statement->connection->database == NULL ||
		statement->connection->broken) {
		return ib_fail(error, "prepared statement is unavailable");
	}
	capacity = 256U;
	for (;;) {
		response = (char *) calloc(capacity, 1U);
		if (response == NULL) {
			return ib_fail(error, "out of memory allocating plan response");
		}
		memset(status, 0, sizeof(status));
		result = isc_dsql_sql_info(status, &statement->statement,
			(short) sizeof(request), (char *) request, (short) capacity, response);
		if (result != 0) {
			free(response);
			return ib_fail_status(error, "request statement plan", status);
		}
		if ((unsigned char) response[0] == isc_info_truncated || capacity < 4U) {
			free(response);
			if (capacity >= (size_t) SHRT_MAX) {
				return ib_fail(error, "plan response exceeds the native limit");
			}
			capacity *= 2U;
			if (capacity > (size_t) SHRT_MAX) {
				capacity = (size_t) SHRT_MAX;
			}
			continue;
		}
		if ((unsigned char) response[0] == isc_info_end) {
			if ((unsigned char) response[1] != 0U ||
				(unsigned char) response[2] != 0U) {
				free(response);
				return ib_fail(error, "malformed empty plan response");
			}
			converted = ib_copy_string(NULL, 0U);
			free(response);
			if (converted == NULL) {
				return ib_fail(error, "out of memory allocating empty statement plan");
			}
			*plan = converted;
			*plan_length = 0U;
			return 0;
		}
		if ((unsigned char) response[0] != isc_info_sql_get_plan) {
			free(response);
			return ib_fail(error, "InterBase returned an invalid plan response");
		}
		payload_length = (size_t) (unsigned char) response[1] |
			((size_t) (unsigned char) response[2] << 8U);
		used = 3U + payload_length;
		if (used >= capacity || (unsigned char) response[used] != isc_info_end) {
			free(response);
			if (used >= capacity && capacity < (size_t) SHRT_MAX) {
				capacity *= 2U;
				if (capacity > (size_t) SHRT_MAX) {
					capacity = (size_t) SHRT_MAX;
				}
				continue;
			}
			return ib_fail(error, "malformed plan response");
		}
		converted = ib_convert_to_utf8(response + 3U, payload_length,
			statement->connection->charset, &converted_length, error);
		free(response);
		if (converted == NULL) {
			return -1;
		}
		*plan = converted;
		*plan_length = converted_length;
		return 0;
	}
}

int ib_cursor_next(ib_cursor *cursor, ib_cancel_slot *cancel,
	uint64_t generation, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;

	if (cursor == NULL || cursor->statement == NULL || cursor->output == NULL) {
		ib_cancel_slot_complete(cancel, generation);
		return ib_fail(error, "cursor is unavailable");
	}
	if (cursor->procedure) {
		if (cursor->output_pending) {
			ib_cancel_slot_complete(cancel, generation);
			cursor->output_pending = 0;
			cursor->fetched = 1;
			return 1;
		}
		ib_cancel_slot_complete(cancel, generation);
		return 0;
	}
	if (ib_cancel_slot_publish_if_active(cancel, generation, &cursor->statement,
		error) != 0) {
		ib_cancel_slot_complete(cancel, generation);
		return -1;
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_fetch(status, &cursor->statement,
		ib_connection_dialect(cursor->connection),
		cursor->output);
	ib_cancel_slot_complete(cancel, generation);
	if (result == 100) {
		return 0;
	}
	if (result != 0) {
		return ib_fail_status(error, "fetch row", status);
	}
	cursor->fetched = 1;
	return 1;
}

int ib_cursor_close(ib_cursor *cursor, char **error)
{
	return ib_cursor_close_internal(cursor, error, 1);
}

int ib_cursor_abort(ib_cursor *cursor, char **error)
{
	return ib_cursor_close_internal(cursor, error, 0);
}

int ib_cursor_set_name(ib_cursor *cursor, const char *name, size_t name_length,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	char *encoded;
	size_t encoded_length;

	if (cursor == NULL || cursor->connection == NULL || cursor->statement == NULL ||
		cursor->connection->database == NULL || cursor->connection->broken) {
		return ib_fail(error, "cursor is unavailable");
	}
	if (name == NULL || name_length == 0U || name_length > (size_t) USHRT_MAX) {
		return ib_fail(error, "cursor name is empty or too long");
	}
	encoded = ib_convert_utf8(name, name_length, cursor->connection->charset,
		&encoded_length, error);
	if (encoded == NULL) {
		return -1;
	}
	if (encoded_length > (size_t) USHRT_MAX) {
		free(encoded);
		return ib_fail(error, "encoded cursor name is too long");
	}
	memset(status, 0, sizeof(status));
	result = isc_dsql_set_cursor_name(status, &cursor->statement, encoded,
		0);
	free(encoded);
	if (result != 0) {
		return ib_fail_status(error, "set cursor name", status);
	}
	return 0;
}

int ib_cursor_uses_connection_transaction(const ib_cursor *cursor)
{
	return cursor != NULL && cursor->connection != NULL &&
		!cursor->owns_transaction && cursor->connection->transaction != NULL &&
		cursor->transaction == cursor->connection->transaction;
}

size_t ib_cursor_column_count(const ib_cursor *cursor)
{
	if (cursor == NULL || cursor->output == NULL || cursor->output->sqld < 0) {
		return 0U;
	}
	return (size_t) cursor->output->sqld;
}

const char *ib_cursor_column_name(const ib_cursor *cursor, size_t index,
	size_t *length)
{
	const XSQLVAR *variable;
	short name_length;

	if (length != NULL) {
		*length = 0U;
	}
	if (cursor == NULL || cursor->output == NULL || index >= (size_t) cursor->output->sqld) {
		return NULL;
	}
	if (cursor->column_names != NULL && cursor->column_name_lengths != NULL) {
		if (length != NULL) {
			*length = cursor->column_name_lengths[index];
		}
		return cursor->column_names[index];
	}
	variable = &cursor->output->sqlvar[index];
	if (variable->aliasname_length > 0) {
		name_length = variable->aliasname_length;
		if (name_length > METADATALENGTH) {
			name_length = METADATALENGTH;
		}
		if (length != NULL) {
			*length = (size_t) name_length;
		}
		return variable->aliasname;
	}
	name_length = variable->sqlname_length;
	if (name_length < 0 || name_length > METADATALENGTH) {
		return NULL;
	}
	if (length != NULL) {
		*length = (size_t) name_length;
	}
	return variable->sqlname;
}

int ib_cursor_column_metadata(const ib_cursor *cursor, size_t index,
	ib_column_metadata *metadata, char **error)
{
	if (cursor == NULL || cursor->output == NULL || metadata == NULL ||
		index >= (size_t) cursor->output->sqld || cursor->metadata == NULL) {
		return ib_fail(error, "result column metadata is unavailable");
	}
	*metadata = cursor->metadata[index];
	return 0;
}

int ib_cursor_column_indicator(const ib_cursor *cursor, size_t index,
	uint16_t *indicator, char **error)
{
	const XSQLVAR *variable;

	if (indicator == NULL) {
		return ib_fail(error, "result indicator storage is unavailable");
	}
	*indicator = 0U;
	if (cursor == NULL || cursor->output == NULL ||
		index >= (size_t) cursor->output->sqld) {
		return ib_fail(error, "result column indicator is unavailable");
	}
	variable = &cursor->output->sqlvar[index];
	if (variable->sqlind != NULL) {
		/* The legacy -1 sentinel is plain NULL. Other bit patterns, including
		 * NULL combined with a change flag, must remain intact. */
		*indicator = *variable->sqlind == (short) -1
			? (uint16_t) SQLIND_NULL
			: (uint16_t) *variable->sqlind;
	}
	return 0;
}

static int ib_fill_timestamp(ib_value_view *view, int type, const char *data,
	char **error)
{
	struct tm decoded;
	ISC_TIMESTAMP timestamp;
	ISC_DATE date;
	ISC_TIME time_value;
	unsigned long ticks;

	memset(&decoded, 0, sizeof(decoded));
	if (type == SQL_TIMESTAMP) {
		memcpy(&timestamp, data, sizeof(timestamp));
		isc_decode_timestamp(&timestamp, &decoded);
		ticks = (unsigned long) timestamp.timestamp_time;
	} else if (type == SQL_TYPE_DATE) {
		memcpy(&date, data, sizeof(date));
		isc_decode_sql_date(&date, &decoded);
		ticks = 0UL;
		decoded.tm_hour = 0;
		decoded.tm_min = 0;
		decoded.tm_sec = 0;
	} else if (type == SQL_TYPE_TIME) {
		memcpy(&time_value, data, sizeof(time_value));
		isc_decode_sql_time(&time_value, &decoded);
		ticks = (unsigned long) time_value;
		decoded.tm_year = 0;
		decoded.tm_mon = 0;
		decoded.tm_mday = 1;
	} else {
		return ib_fail(error, "timestamp type is unsupported");
	}
	if (decoded.tm_year < -1900 || decoded.tm_mon < 0 || decoded.tm_mon > 11 ||
		decoded.tm_mday < 1 || decoded.tm_mday > 31 || decoded.tm_hour < 0 ||
		decoded.tm_hour > 23 || decoded.tm_min < 0 || decoded.tm_min > 59 ||
		decoded.tm_sec < 0 || decoded.tm_sec > 59 ||
		ticks >= 24UL * 60UL * 60UL * (unsigned long) ISC_TIME_SECONDS_PRECISION) {
		return ib_fail(error, "InterBase returned an invalid timestamp");
	}
	view->kind = IB_VALUE_TIMESTAMP;
	view->year = decoded.tm_year + 1900;
	view->month = decoded.tm_mon + 1;
	view->day = decoded.tm_mday;
	view->hour = decoded.tm_hour;
	view->minute = decoded.tm_min;
	view->second = decoded.tm_sec;
	view->nanosecond = (int) ((ticks % ISC_TIME_SECONDS_PRECISION) * 100000UL);
	return 0;
}

static size_t ib_utf8_prefix_length(const char *data, size_t length,
	size_t character_count)
{
	size_t offset;
	size_t count;

	offset = 0U;
	count = 0U;
	while (offset < length && count < character_count) {
		unsigned char first = (unsigned char) data[offset];
		size_t width = 1U;

		if (first < 0x80U) {
			width = 1U;
		} else if ((first & 0xe0U) == 0xc0U) {
			width = 2U;
		} else if ((first & 0xf0U) == 0xe0U) {
			width = 3U;
		} else if ((first & 0xf8U) == 0xf0U) {
			width = 4U;
		}
		if (width > length - offset) {
			width = 1U;
		} else if (width > 1U) {
			size_t continuation;
			for (continuation = 1U; continuation < width; continuation++) {
				if ((((unsigned char) data[offset + continuation]) & 0xc0U) != 0x80U) {
					width = 1U;
					break;
				}
			}
		}
		offset += width;
		count++;
	}
	return offset;
}

static size_t ib_text_character_count_for_charset(size_t length, short charset)
{
	switch (charset) {
	case 3:  /* UNICODE_FSS */
		return length / 3U;
	case 5:  /* SJIS_0208 */
	case 6:  /* EUCJ_0208 */
	case 8:  /* UNICODE_BE */
	case 44: /* KSC_5601 */
	case 56: /* BIG_5 */
	case 57: /* GB_2312 */
	case 64: /* UNICODE_LE */
		return length / 2U;
	case IB_CHARSET_UTF8:
		return length / 4U;
	default:
		return length;
	}
}

static size_t ib_text_character_count(const XSQLVAR *variable)
{
	if (variable == NULL || variable->sqllen < 0) {
		return 0U;
	}
	return ib_text_character_count_for_charset((size_t) variable->sqllen,
		ib_text_charset(variable));
}

static size_t ib_utf8_text_length(const ib_cursor *cursor, const XSQLVAR *variable,
	size_t index)
{
	size_t length;
	size_t character_count;

	if (variable == NULL || variable->sqllen < 0 || variable->sqldata == NULL) {
		return 0U;
	}
	length = (size_t) variable->sqllen;
	if (variable->relname_length <= 0 || variable->sqlname_length <= 0) {
		if (cursor != NULL && cursor->fixed_text_lengths != NULL &&
			cursor->output != NULL && index < (size_t) cursor->output->sqld &&
			cursor->fixed_text_lengths[index] != 0U) {
			return ib_utf8_prefix_length(variable->sqldata, length,
				cursor->fixed_text_lengths[index]);
		}
		/* SQL_TEXT descriptors for expressions do not carry a source relation and
		 * field. Unless the SQL expression is known to be a fixed-width CHAR cast,
		 * its descriptor length is the exact expression result length. Never infer
		 * padding by inspecting content: a literal may legitimately end in spaces. */
		return length;
	}
	character_count = ib_text_character_count(variable);
	return ib_utf8_prefix_length(variable->sqldata, length, character_count);
}

static size_t ib_array_character_count(const ISC_ARRAY_DESC_V2 *descriptor,
	int charset)
{
	if (descriptor == NULL) {
		return 0U;
	}
	return ib_text_character_count_for_charset(
		(size_t) descriptor->array_desc_length,
		(short) charset);
}

static int ib_array_decode_element(const ISC_ARRAY_DESC_V2 *descriptor,
	int charset, const char *source, size_t element_size,
	ib_value_view *view, char **owned, char **error)
{
	int dtype;
	int numeric_scaled;
	size_t expected_size;

	if (descriptor == NULL || source == NULL || view == NULL || owned == NULL) {
		return ib_fail(error, "array result element storage is unavailable");
	}
	if (ib_array_element_size(descriptor, &expected_size, error) != 0) {
		return -1;
	}
	if (element_size != expected_size) {
		return ib_fail(error, "array result element storage does not match its descriptor");
	}
	memset(view, 0, sizeof(*view));
	*owned = NULL;
	dtype = descriptor->array_desc_dtype;
	numeric_scaled = descriptor->array_desc_subtype == 1 ||
		descriptor->array_desc_subtype == 2 || descriptor->array_desc_scale != 0;
	switch (dtype) {
	case blr_text:
	case blr_text2:
	case blr_varying:
	case blr_varying2:
	{
		int varying = dtype == blr_varying || dtype == blr_varying2;
		const char *data = source;
		size_t length = (size_t) descriptor->array_desc_length;
		size_t converted_length;
		char *converted;

		if (varying) {
			const char *terminator = (const char *) memchr(source, '\0', length);
			if (terminator != NULL) {
				length = (size_t) (terminator - source);
			}
		} else if (charset == IB_CHARSET_UTF8) {
			length = ib_utf8_prefix_length(data, length,
				ib_array_character_count(descriptor, charset));
		}
		if (charset == 1) {
			view->kind = IB_VALUE_BYTES;
			view->bytes = data;
			view->length = length;
			return 0;
		}
		if (charset != 0 && charset != IB_CHARSET_UTF8) {
			converted = ib_convert_to_utf8(data, length, charset,
				&converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			*owned = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = data;
			view->length = length;
		}
		view->kind = IB_VALUE_STRING;
		return 0;
	}
	case blr_short:
	{
		short value;

		if (element_size < sizeof(value)) {
			return ib_fail(error, "SMALLINT array result storage is invalid");
		}
		memcpy(&value, source, sizeof(value));
		view->kind = numeric_scaled ? IB_VALUE_SCALED_INT : IB_VALUE_INT64;
		view->scale = descriptor->array_desc_scale;
		view->int64_value = (int64_t) value;
		return 0;
	}
	case blr_long:
	{
		ISC_LONG value;

		if (element_size < sizeof(value)) {
			return ib_fail(error, "INTEGER array result storage is invalid");
		}
		memcpy(&value, source, sizeof(value));
		view->kind = numeric_scaled ? IB_VALUE_SCALED_INT : IB_VALUE_INT64;
		view->scale = descriptor->array_desc_scale;
		view->int64_value = (int64_t) value;
		return 0;
	}
	case blr_int64:
	{
		int64_t value;

		if (element_size < sizeof(value)) {
			return ib_fail(error, "BIGINT array result storage is invalid");
		}
		memcpy(&value, source, sizeof(value));
		view->kind = numeric_scaled ? IB_VALUE_SCALED_INT : IB_VALUE_INT64;
		view->scale = descriptor->array_desc_scale;
		view->int64_value = value;
		return 0;
	}
	case blr_float:
	{
		float value;

		if (element_size < sizeof(value)) {
			return ib_fail(error, "FLOAT array result storage is invalid");
		}
		memcpy(&value, source, sizeof(value));
		if (!isfinite(value)) {
			return ib_fail(error, "InterBase returned a non-finite FLOAT array element");
		}
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = (double) value;
		return 0;
	}
	case blr_double:
	case blr_d_float:
	{
		double value;

		if (element_size < sizeof(value)) {
			return ib_fail(error, "DOUBLE array result storage is invalid");
		}
		memcpy(&value, source, sizeof(value));
		if (!isfinite(value)) {
			return ib_fail(error, "InterBase returned a non-finite DOUBLE array element");
		}
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = value;
		return 0;
	}
	case blr_timestamp:
	case blr_sql_date:
	case blr_sql_time:
	{
		int type = dtype == blr_timestamp ? SQL_TIMESTAMP :
			dtype == blr_sql_date ? SQL_TYPE_DATE : SQL_TYPE_TIME;
		return ib_fill_timestamp(view, type, source, error);
	}
	case blr_boolean_dtype:
	{
		ISC_BOOLEAN value;

		if (element_size < sizeof(value)) {
			return ib_fail(error, "BOOLEAN array result storage is invalid");
		}
		memcpy(&value, source, sizeof(value));
		view->kind = IB_VALUE_BOOL;
		view->bool_value = value != 0;
		return 0;
	}
	default:
		return ib_fail(error, "array result element type is unsupported");
	}
}

static int ib_cursor_read_array(ib_cursor *cursor, const XSQLVAR *variable,
	ib_value_view *view, char **error)
{
	ISC_ARRAY_DESC_V2 descriptor;
	ISC_ARRAY_BOUND *native_bounds;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ISC_QUAD array_id;
	char *data;
	ISC_LONG slice_length;
	size_t element_size;
	size_t element_count;
	size_t slice_size;
	size_t index;
	int charset;

	if (ib_array_lookup_descriptor(cursor, variable, &descriptor, NULL, &charset,
		error) != 0 ||
		ib_array_slice_layout(&descriptor, &element_size, &element_count, &slice_size,
			error) != 0) {
		return -1;
	}
	data = (char *) malloc(slice_size == 0U ? 1U : slice_size);
	native_bounds = NULL;
	if (data == NULL) {
		return ib_fail(error, "out of memory allocating array result slice");
	}
	if (element_count != 0U) {
		cursor->array_elements = (ib_value_view *) calloc(element_count,
			sizeof(*cursor->array_elements));
		cursor->array_element_data = (char **) calloc(element_count,
			sizeof(*cursor->array_element_data));
	}
	native_bounds = (ISC_ARRAY_BOUND *) calloc((size_t) descriptor.array_desc_dimensions,
		sizeof(*native_bounds));
	if ((element_count != 0U && (cursor->array_elements == NULL ||
		cursor->array_element_data == NULL)) || native_bounds == NULL) {
		free(data);
		free(native_bounds);
		free(cursor->array_elements);
		free(cursor->array_element_data);
		cursor->array_elements = NULL;
		cursor->array_element_data = NULL;
		return ib_fail(error, "out of memory allocating array result metadata");
	}
	memcpy(&array_id, variable->sqldata, sizeof(array_id));
	slice_length = (ISC_LONG) slice_size;
	memset(status, 0, sizeof(status));
	result = isc_array_get_slice2(status, &cursor->connection->database,
		&cursor->transaction, &array_id, &descriptor, data, &slice_length);
	if (result != 0 || slice_length < 0 || (size_t) slice_length != slice_size) {
		free(data);
		free(native_bounds);
		free(cursor->array_elements);
		free(cursor->array_element_data);
		cursor->array_elements = NULL;
		cursor->array_element_data = NULL;
		if (result != 0) {
			return ib_fail_status(error, "read array slice", status);
		}
		return ib_fail(error, "InterBase returned an invalid array slice length");
	}
	for (index = 0U; index < (size_t) descriptor.array_desc_dimensions; index++) {
		native_bounds[index].array_bound_lower = descriptor.array_desc_bounds[index].array_bound_lower;
		native_bounds[index].array_bound_upper = descriptor.array_desc_bounds[index].array_bound_upper;
	}
	cursor->array_data = data;
	cursor->array_bounds = (ib_array_bound *) calloc(
		(size_t) descriptor.array_desc_dimensions, sizeof(*cursor->array_bounds));
	if (cursor->array_bounds == NULL) {
		free(native_bounds);
		ib_cursor_free_array(cursor);
		return ib_fail(error, "out of memory copying array result bounds");
	}
	for (index = 0U; index < (size_t) descriptor.array_desc_dimensions; index++) {
		cursor->array_bounds[index].lower =
			(int32_t) native_bounds[index].array_bound_lower;
		cursor->array_bounds[index].upper =
			(int32_t) native_bounds[index].array_bound_upper;
	}
	free(native_bounds);
	cursor->array_dimensions = (size_t) descriptor.array_desc_dimensions;
	cursor->array_element_count = element_count;
	for (index = 0U; index < element_count; index++) {
		if (ib_array_decode_element(&descriptor,
			charset, data + index * element_size, element_size,
			&cursor->array_elements[index], &cursor->array_element_data[index],
			error) != 0) {
			ib_cursor_free_array(cursor);
			return -1;
		}
	}
	cursor->array_view.dimensions = (int) cursor->array_dimensions;
	cursor->array_view.bounds = cursor->array_bounds;
	cursor->array_view.element_count = cursor->array_element_count;
	cursor->array_view.elements = cursor->array_elements;
	view->kind = IB_VALUE_ARRAY;
	view->array = &cursor->array_view;
	return 0;
}

static int ib_append_blob_data(char **data, size_t *length, size_t *capacity,
	const char *segment, size_t segment_length, char **error)
{
	char *resized;
	size_t needed;
	size_t new_capacity;

	if (data == NULL || length == NULL || capacity == NULL ||
		(segment_length != 0U && segment == NULL)) {
		return ib_fail(error, "BLOB result storage is unavailable");
	}
	if (*length > IB_MAX_BLOB_BUFFER ||
		segment_length > IB_MAX_BLOB_BUFFER - *length) {
		return ib_fail(error, "BLOB result exceeds the materialization limit");
	}
	needed = *length + segment_length;
	if (needed > *capacity) {
		new_capacity = *capacity == 0U ? IB_BLOB_SEGMENT_LENGTH : *capacity;
		while (new_capacity < needed) {
			if (new_capacity > IB_MAX_BLOB_BUFFER / 2U) {
				new_capacity = IB_MAX_BLOB_BUFFER;
				break;
			}
			new_capacity *= 2U;
		}
		if (new_capacity > IB_MAX_BLOB_BUFFER) {
			new_capacity = IB_MAX_BLOB_BUFFER;
		}
		resized = (char *) realloc(*data, new_capacity);
		if (resized == NULL) {
			return ib_fail(error, "out of memory materializing BLOB result");
		}
		*data = resized;
		*capacity = new_capacity;
	}
	if (segment_length != 0U) {
		memcpy(*data + *length, segment, segment_length);
		*length = needed;
	}
	return 0;
}

static char *ib_read_blob(ib_cursor *cursor, const XSQLVAR *variable,
	size_t *length, int *is_utf8, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS result;
	ISC_QUAD blob_id;
	isc_blob_handle blob;
	ISC_BLOB_DESC_V2 source_descriptor;
	ISC_BLOB_DESC_V2 target_descriptor;
	unsigned char bpb[IB_BLOB_BPB_LENGTH];
	unsigned short bpb_length;
	char segment[IB_BLOB_SEGMENT_LENGTH];
	char *data;
	char *first_error;
	char *cleanup_error;
	size_t data_length;
	size_t capacity;
	int cleanup_failed;
	int use_bpb;

	if (length == NULL || cursor == NULL || cursor->connection == NULL ||
		cursor->connection->database == NULL || cursor->transaction == NULL ||
		variable == NULL || variable->sqldata == NULL || is_utf8 == NULL) {
		(void) ib_fail(error, "BLOB result storage is unavailable");
		return NULL;
	}
	memcpy(&blob_id, variable->sqldata, sizeof(blob_id));
	*is_utf8 = 0;
	bpb_length = 0U;
	use_bpb = 0;
	if (variable->sqlsubtype == 1 && variable->sqlname_length > 0 &&
		variable->relname_length > 0) {
		if (ib_blob_lookup_descriptor(cursor, variable, &source_descriptor,
			error) != 0) {
			return NULL;
		}
		if (source_descriptor.blob_desc_charset != 0 &&
			source_descriptor.blob_desc_charset != 1) {
			ib_blob_descriptor(&target_descriptor, 1, IB_CHARSET_UTF8);
			memset(status, 0, sizeof(status));
			/* Read through the column's charset and expose UTF-8 to Go. */
			result = isc_blob_gen_bpb2(status, &target_descriptor,
				&source_descriptor, (unsigned short) sizeof(bpb), bpb,
				&bpb_length);
			if (result != 0) {
				(void) ib_fail_status(error, "generate output BLOB parameter block",
					status);
				return NULL;
			}
			use_bpb = 1;
			*is_utf8 = 1;
		}
	}
	blob = NULL;
	memset(status, 0, sizeof(status));
	if (use_bpb) {
		result = isc_open_blob2(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id, (short) bpb_length,
			(char *) bpb);
	} else {
		result = isc_open_blob(status, &cursor->connection->database,
			&cursor->transaction, &blob, &blob_id);
	}
	if (result != 0) {
		(void) ib_fail_status(error, "open output BLOB", status);
		return NULL;
	}
	data = NULL;
	data_length = 0U;
	capacity = 0U;
	for (;;) {
		unsigned short segment_length;

		segment_length = 0U;
		memset(status, 0, sizeof(status));
		result = isc_get_segment(status, &blob, &segment_length,
			(unsigned short) sizeof(segment), segment);
		if (result == isc_segstr_eof) {
			break;
		}
		if (result != 0 && result != isc_segment) {
			first_error = NULL;
			(void) ib_fail_status(&first_error, "read output BLOB segment", status);
			cleanup_error = NULL;
			cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
			if (cleanup_failed != 0) {
				ib_mark_broken(cursor->connection);
			}
			ib_append_error(&first_error, cleanup_error);
			free(data);
			ib_give_error(error, first_error);
			return NULL;
		}
		first_error = NULL;
		if (ib_append_blob_data(&data, &data_length, &capacity, segment,
			(size_t) segment_length, &first_error) != 0) {
			cleanup_error = NULL;
			cleanup_failed = ib_blob_cleanup(&blob, 1, &cleanup_error);
			if (cleanup_failed != 0) {
				ib_mark_broken(cursor->connection);
			}
			ib_append_error(&first_error, cleanup_error);
			free(data);
			ib_give_error(error, first_error);
			return NULL;
		}
	}
	cleanup_error = NULL;
	cleanup_failed = ib_blob_cleanup(&blob, 0, &cleanup_error);
	if (cleanup_failed != 0) {
		ib_mark_broken(cursor->connection);
	}
	if (cleanup_failed != 0 || cleanup_error != NULL) {
		free(data);
		ib_give_error(error, cleanup_error);
		return NULL;
	}
	if (data == NULL) {
		data = (char *) malloc(1U);
		if (data == NULL) {
			(void) ib_fail(error, "out of memory materializing empty BLOB result");
			return NULL;
		}
		data[0] = '\0';
	}
	*length = data_length;
	return data;
}

int ib_cursor_column(const ib_cursor *cursor, size_t index,
	ib_value_view *view, char **error)
{
	const XSQLVAR *variable;
	int type;
	short short_value;
	ISC_LONG long_value;
	ISC_INT64 int64_value;
	float float_value;
	double double_value;
	ISC_BOOLEAN boolean_value;
	unsigned short varying_length;
	short connection_charset;
	short text_charset;
	size_t source_length;
	size_t character_count;
	size_t converted_length;
	char *converted;

	if (cursor == NULL || cursor->output == NULL || !cursor->fetched ||
		index >= (size_t) cursor->output->sqld || view == NULL) {
		return ib_fail(error, "result column is unavailable");
	}
	memset(view, 0, sizeof(*view));
	variable = &cursor->output->sqlvar[index];
	if (variable->sqldata == NULL ||
		((variable->sqltype & 1) != 0 && variable->sqlind == NULL)) {
		return ib_fail(error, "result column storage is unavailable");
	}
	free(((ib_cursor *) cursor)->converted_value);
	((ib_cursor *) cursor)->converted_value = NULL;
	free(((ib_cursor *) cursor)->materialized_value);
	((ib_cursor *) cursor)->materialized_value = NULL;
	ib_cursor_free_array((ib_cursor *) cursor);
	connection_charset = IB_CHARSET_UTF8;
	if (cursor->connection != NULL && cursor->connection->charset != 0) {
		connection_charset = cursor->connection->charset;
	}
	if (variable->sqlind != NULL && *variable->sqlind < 0) {
		view->kind = IB_VALUE_NULL;
		return 0;
	}
	type = ib_sql_type(variable);
	text_charset = ib_text_charset(variable);
	switch (type) {
	case SQL_TEXT:
		source_length = (size_t) variable->sqllen;
		if (connection_charset == IB_CHARSET_UTF8) {
			source_length = ib_utf8_text_length(cursor, variable, index);
		} else if (connection_charset != 0 && connection_charset != 1 &&
			text_charset != 1) {
			character_count = ib_text_character_count(variable);
			if (character_count < source_length) {
				source_length = character_count;
			}
		}
		if (connection_charset != 0 && connection_charset != IB_CHARSET_UTF8 &&
			text_charset != 1) {
			converted = ib_convert_to_utf8(variable->sqldata, source_length,
				connection_charset, &converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			((ib_cursor *) cursor)->converted_value = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = variable->sqldata;
			view->length = source_length;
		}
		view->kind = text_charset == 1 ? IB_VALUE_BYTES : IB_VALUE_STRING;
		return 0;
	case SQL_VARYING:
		memcpy(&varying_length, variable->sqldata, sizeof(varying_length));
		if (variable->sqllen < 0 || varying_length > (unsigned short) variable->sqllen) {
			return ib_fail(error, "InterBase returned an invalid varying value length");
		}
		source_length = (size_t) varying_length;
		if (connection_charset != 0 && connection_charset != IB_CHARSET_UTF8 &&
			text_charset != 1) {
			converted = ib_convert_to_utf8(variable->sqldata + sizeof(varying_length),
				source_length, connection_charset, &converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			((ib_cursor *) cursor)->converted_value = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = variable->sqldata + sizeof(varying_length);
			view->length = source_length;
		}
		view->kind = text_charset == 1 ? IB_VALUE_BYTES : IB_VALUE_STRING;
		return 0;
	case SQL_BLOB:
	{
		char *blob_data;
		size_t blob_length;
		int blob_is_utf8;

		if (cursor->allow_arrays) {
			return ib_cursor_blob_ref((ib_cursor *) cursor, variable, view, error);
		}

		blob_data = ib_read_blob((ib_cursor *) cursor, variable, &blob_length,
			&blob_is_utf8, error);
		if (blob_data == NULL) {
			return -1;
		}
		((ib_cursor *) cursor)->materialized_value = blob_data;
		if (!blob_is_utf8 && variable->sqlsubtype == 1 && connection_charset != 0 &&
			connection_charset != 1 && connection_charset != IB_CHARSET_UTF8) {
			converted = ib_convert_to_utf8(blob_data, blob_length,
				connection_charset, &converted_length, error);
			if (converted == NULL) {
				return -1;
			}
			free(((ib_cursor *) cursor)->materialized_value);
			((ib_cursor *) cursor)->materialized_value = NULL;
			((ib_cursor *) cursor)->converted_value = converted;
			view->bytes = converted;
			view->length = converted_length;
		} else {
			view->bytes = ((ib_cursor *) cursor)->materialized_value;
			view->length = blob_length;
		}
		view->kind = variable->sqlsubtype == 1 ? IB_VALUE_STRING : IB_VALUE_BYTES;
		return 0;
	}
	case SQL_ARRAY:
		if (!cursor->allow_arrays) {
			return ib_fail(error, "array results are unsupported by database/sql");
		}
		return ib_cursor_read_array((ib_cursor *) cursor, variable, view, error);
	case SQL_SHORT:
		memcpy(&short_value, variable->sqldata, sizeof(short_value));
		view->int64_value = (int64_t) short_value;
		if (variable->sqlscale != 0) {
			view->kind = IB_VALUE_SCALED_INT;
			view->scale = variable->sqlscale;
		} else {
			view->kind = IB_VALUE_INT64;
		}
		return 0;
	case SQL_LONG:
		memcpy(&long_value, variable->sqldata, sizeof(long_value));
		view->int64_value = (int64_t) long_value;
		if (variable->sqlscale != 0) {
			view->kind = IB_VALUE_SCALED_INT;
			view->scale = variable->sqlscale;
		} else {
			view->kind = IB_VALUE_INT64;
		}
		return 0;
	case SQL_INT64:
		memcpy(&int64_value, variable->sqldata, sizeof(int64_value));
		view->int64_value = (int64_t) int64_value;
		if (variable->sqlscale != 0) {
			view->kind = IB_VALUE_SCALED_INT;
			view->scale = variable->sqlscale;
		} else {
			view->kind = IB_VALUE_INT64;
		}
		return 0;
	case SQL_FLOAT:
		memcpy(&float_value, variable->sqldata, sizeof(float_value));
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = (double) float_value;
		return 0;
	case SQL_DOUBLE:
		memcpy(&double_value, variable->sqldata, sizeof(double_value));
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = double_value;
		return 0;
	case SQL_D_FLOAT:
		memcpy(&double_value, variable->sqldata, sizeof(double_value));
		view->kind = IB_VALUE_FLOAT64;
		view->float64_value = double_value;
		return 0;
	case SQL_TIMESTAMP:
	case SQL_TYPE_DATE:
	case SQL_TYPE_TIME:
		return ib_fill_timestamp(view, type, variable->sqldata, error);
	case SQL_BOOLEAN:
		memcpy(&boolean_value, variable->sqldata, sizeof(boolean_value));
		view->kind = IB_VALUE_BOOL;
		view->bool_value = boolean_value != 0;
		return 0;
	default:
		return ib_fail(error, "the result type is unsupported by the proof of concept");
	}
}

static int ib_transaction_available(ib_transaction *transaction,
	char **error)
{
	if (transaction == NULL || transaction->view.database == NULL ||
		(transaction->distributed != NULL &&
			transaction->distributed->handle == NULL) ||
		(transaction->distributed == NULL && transaction->view.transaction == NULL) ||
		transaction->view.broken ||
		(transaction->parent != NULL && transaction->parent->broken)) {
		return ib_fail(error, "transaction is unavailable");
	}
	if (transaction->distributed != NULL) {
		transaction->view.transaction = transaction->distributed->handle;
	}
	return 0;
}

static void ib_transaction_sync(ib_transaction *transaction)
{
	/* A failed participant operation must leave the distributed handle
	 * available for coordinator-driven recovery. Group-level failures are
	 * propagated by ib_distributed_mark_broken instead. */
	if (transaction != NULL && transaction->parent != NULL &&
		transaction->distributed == NULL && transaction->view.broken) {
		ib_mark_broken(transaction->parent);
	}
}

ib_transaction *ib_transaction_begin(ib_connection *connection,
	const char *tpb, size_t tpb_length, char **error)
{
	ib_transaction *transaction;
	isc_tr_handle handle;

	if (connection == NULL || connection->database == NULL || connection->broken) {
		ib_fail(error, "connection is unavailable");
		return NULL;
	}
	transaction = (ib_transaction *) calloc(1U, sizeof(*transaction));
	if (transaction == NULL) {
		ib_fail(error, "out of memory allocating transaction state");
		return NULL;
	}
	handle = NULL;
	if (ib_start_transaction_tpb(connection, &handle, tpb, tpb_length,
		"start direct transaction", error) != 0) {
		/* A failed start can leave a live handle when the compensating
		 * rollback also fails.  Return the wrapper with the error so its
		 * owner can retry cleanup instead of losing the native handle. */
		transaction->parent = connection;
		transaction->view = *connection;
		transaction->view.parent = connection;
		transaction->view.cursors = NULL;
		transaction->view.statements = NULL;
		transaction->view.transaction = handle;
		transaction->view.transaction_read_only = ib_tpb_is_read_only(tpb, tpb_length);
		transaction->view.broken = 0;
		return transaction;
	}
	transaction->parent = connection;
	transaction->view = *connection;
	transaction->view.parent = connection;
	transaction->view.cursors = NULL;
	transaction->view.statements = NULL;
	transaction->view.transaction = handle;
	transaction->view.transaction_read_only = ib_tpb_is_read_only(tpb, tpb_length);
	transaction->view.broken = 0;
	return transaction;
}

int ib_transaction_commit(ib_transaction *transaction, char **error)
{
	int result;

	if (ib_transaction_available(transaction, error) != 0) {
		return -1;
	}
	result = ib_connection_commit(&transaction->view, error);
	ib_transaction_sync(transaction);
	return result;
}

int ib_transaction_rollback(ib_transaction *transaction, char **error)
{
	int result;

	if (ib_transaction_available(transaction, error) != 0) {
		return -1;
	}
	result = ib_connection_rollback(&transaction->view, error);
	ib_transaction_sync(transaction);
	return result;
}

int ib_transaction_rollback_cleanup(ib_transaction *transaction, char **error)
{
	int result;

	if (transaction == NULL || transaction->distributed != NULL) {
		return ib_fail(error, "transaction is unavailable");
	}
	result = ib_connection_rollback_internal(&transaction->view, error, 1);
	ib_transaction_sync(transaction);
	return result;
}

int ib_transaction_handle_state(const ib_transaction *transaction)
{
	if (transaction == NULL) {
		return IB_HANDLE_UNKNOWN;
	}
	return transaction->view.transaction == NULL ?
		IB_HANDLE_CONSUMED : IB_HANDLE_LIVE;
}

int ib_transaction_commit_retaining(ib_transaction *transaction, char **error)
{
	int result;

	if (ib_transaction_available(transaction, error) != 0) {
		return -1;
	}
	result = ib_connection_commit_retaining(&transaction->view, error);
	ib_transaction_sync(transaction);
	return result;
}

int ib_transaction_rollback_retaining(ib_transaction *transaction, char **error)
{
	int result;

	if (ib_transaction_available(transaction, error) != 0) {
		return -1;
	}
	result = ib_connection_rollback_retaining(&transaction->view, error);
	ib_transaction_sync(transaction);
	return result;
}

int ib_transaction_info(ib_transaction *transaction, uint8_t item,
	char **response, size_t *response_length, char **error)
{
	int result;

	if (ib_transaction_available(transaction, error) != 0) {
		return -1;
	}
	result = ib_connection_transaction_info(&transaction->view, item,
		response, response_length, error);
	ib_transaction_sync(transaction);
	return result;
}

ib_cursor *ib_transaction_query(ib_transaction *transaction, const char *query,
	size_t query_length, const ib_bindings *bindings, int allow_arrays,
	char **error)
{
	ib_cursor *cursor;

	if (ib_transaction_available(transaction, error) != 0) {
		return NULL;
	}
	cursor = ib_connection_query(&transaction->view, query, query_length,
		bindings, allow_arrays, error);
	ib_transaction_sync(transaction);
	return cursor;
}

int ib_transaction_exec(ib_transaction *transaction, const char *query,
	size_t query_length, const ib_bindings *bindings, int64_t *rows_affected,
	int allow_arrays, char **error)
{
	int result;

	if (ib_transaction_available(transaction, error) != 0) {
		return -1;
	}
	result = ib_connection_exec(&transaction->view, query, query_length,
		bindings, rows_affected, allow_arrays, error);
	ib_transaction_sync(transaction);
	return result;
}

ib_statement *ib_transaction_prepare(ib_transaction *transaction,
	const char *query, size_t query_length, int allow_arrays, char **error)
{
	ib_statement *statement;

	if (ib_transaction_available(transaction, error) != 0) {
		return NULL;
	}
	statement = ib_statement_prepare_mode(&transaction->view, query, query_length,
		allow_arrays, error);
	ib_transaction_sync(transaction);
	return statement;
}

ib_blob_reader *ib_transaction_blob_open2(ib_transaction *transaction,
	int32_t high, uint32_t low, int subtype, int charset, char **error)
{
	ib_blob_reader *reader;

	if (ib_transaction_available(transaction, error) != 0) {
		return NULL;
	}
	reader = ib_blob_open2(&transaction->view, high, low, subtype, charset, error);
	ib_transaction_sync(transaction);
	return reader;
}

ib_blob_writer *ib_transaction_blob_create(ib_transaction *transaction,
	int subtype, int charset, char **error)
{
	ib_blob_writer *writer;

	if (ib_transaction_available(transaction, error) != 0) {
		return NULL;
	}
	writer = ib_blob_create(&transaction->view, subtype, charset, error);
	ib_transaction_sync(transaction);
	return writer;
}

void ib_transaction_free(ib_transaction *transaction)
{
	if (transaction == NULL || transaction->distributed != NULL) {
		return;
	}
	free(transaction);
}

static void ib_distributed_mark_broken(ib_distributed_transaction *transaction)
{
	size_t index;

	if (transaction == NULL) {
		return;
	}
	for (index = 0U; index < transaction->count; index++) {
		if (transaction->parents[index] != NULL) {
			ib_mark_broken(transaction->parents[index]);
		}
		transaction->participants[index].view.broken = 1;
	}
}

static void ib_distributed_sync_handles(ib_distributed_transaction *transaction)
{
	size_t index;

	if (transaction == NULL || transaction->participants == NULL) {
		return;
	}
	for (index = 0U; index < transaction->count; index++) {
		transaction->participants[index].view.transaction = transaction->handle;
	}
}

static void ib_distributed_init_participants(ib_distributed_transaction *transaction,
	const char *const *tpbs, const size_t *tpb_lengths)
{
	size_t index;

	if (transaction == NULL || transaction->participants == NULL ||
		tpbs == NULL || tpb_lengths == NULL) {
		return;
	}
	for (index = 0U; index < transaction->count; index++) {
		ib_transaction *participant = &transaction->participants[index];
		participant->parent = transaction->parents[index];
		participant->distributed = transaction;
		participant->view = *participant->parent;
		participant->view.parent = participant->parent;
		participant->view.cursors = NULL;
		participant->view.statements = NULL;
		participant->view.transaction = transaction->handle;
		participant->view.transaction_read_only =
			ib_tpb_is_read_only(tpbs[index], tpb_lengths[index]);
		participant->view.broken = 0;
	}
}

static void ib_distributed_free_parts(ib_distributed_transaction *transaction)
{
	if (transaction == NULL) {
		return;
	}
	free(transaction->participants);
	transaction->participants = NULL;
	free(transaction->parents);
	transaction->parents = NULL;
	transaction->count = 0U;
}

ib_distributed_transaction *ib_distributed_begin(ib_connection **connections,
	const char *const *tpbs, const size_t *tpb_lengths, size_t count,
	char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ib_transaction_entry *entries;
	ib_distributed_transaction *transaction;
	size_t index;
	char *rollback_error;

	if (connections == NULL || tpbs == NULL || tpb_lengths == NULL ||
		count == 0U || count > (size_t) SHRT_MAX) {
		ib_fail(error, "distributed transaction participants are invalid");
		return NULL;
	}
	transaction = (ib_distributed_transaction *) calloc(1U, sizeof(*transaction));
	if (transaction == NULL) {
		ib_fail(error, "out of memory allocating distributed transaction state");
		return NULL;
	}
	transaction->parents = (ib_connection **) calloc(count, sizeof(*transaction->parents));
	transaction->participants = (ib_transaction *) calloc(count, sizeof(*transaction->participants));
	entries = (ib_transaction_entry *) calloc(count, sizeof(*entries));
	if (transaction->parents == NULL || transaction->participants == NULL || entries == NULL) {
		free(entries);
		ib_distributed_free_parts(transaction);
		free(transaction);
		ib_fail(error, "out of memory allocating distributed transaction participants");
		return NULL;
	}
	transaction->count = count;
	for (index = 0U; index < count; index++) {
		if (connections[index] == NULL || connections[index]->database == NULL ||
			connections[index]->broken || connections[index]->transaction != NULL ||
			tpbs[index] == NULL || tpb_lengths[index] == 0U ||
			tpb_lengths[index] > (size_t) SHRT_MAX) {
			free(entries);
			ib_distributed_free_parts(transaction);
			free(transaction);
			ib_fail(error, "distributed transaction participant is unavailable");
			return NULL;
		}
		transaction->parents[index] = connections[index];
		entries[index].database = &connections[index]->database;
		entries[index].tpb_length = (short) tpb_lengths[index];
		entries[index].tpb = (char *) tpbs[index];
	}
	memset(status, 0, sizeof(status));
	if (isc_start_multiple(status, &transaction->handle, (short) count,
		(void *) entries) != 0) {
		char *first_error = NULL;
		(void) ib_fail_status(&first_error, "start distributed transaction", status);
		ib_distributed_init_participants(transaction, tpbs, tpb_lengths);
		if (transaction->handle != NULL) {
			memset(status, 0, sizeof(status));
			if (isc_rollback_transaction(status, &transaction->handle) != 0) {
				rollback_error = NULL;
				(void) ib_fail_status(&rollback_error, "rollback failed distributed transaction", status);
				ib_append_error(&first_error, rollback_error);
			}
		}
		ib_distributed_mark_broken(transaction);
		free(entries);
		if (transaction->handle != NULL) {
			ib_distributed_sync_handles(transaction);
			ib_give_error(error, first_error);
			return transaction;
		}
		ib_distributed_free_parts(transaction);
		free(transaction);
		ib_give_error(error, first_error);
		return NULL;
	}
	free(entries);
	ib_distributed_init_participants(transaction, tpbs, tpb_lengths);
	return transaction;
}

ib_transaction *ib_distributed_participant(ib_distributed_transaction *transaction,
	size_t index)
{
	if (transaction == NULL || index >= transaction->count ||
		transaction->participants == NULL) {
		return NULL;
	}
	return &transaction->participants[index];
}

static int ib_distributed_close_cursors(ib_distributed_transaction *transaction,
	char **error)
{
	char *first_error = NULL;
	char *cursor_error;
	size_t index;
	int failed = 0;

	if (transaction == NULL || transaction->handle == NULL) {
		return ib_fail(error, "distributed transaction is unavailable");
	}
	for (index = 0U; index < transaction->count; index++) {
		cursor_error = NULL;
		if (ib_close_transaction_cursors(&transaction->participants[index].view,
			transaction->handle, &cursor_error) != 0) {
			failed = 1;
		}
		ib_append_error(&first_error, cursor_error);
	}
	if (failed) {
		ib_distributed_mark_broken(transaction);
	}
	ib_give_error(error, first_error);
	return failed ? -1 : 0;
}

int ib_distributed_prepare(ib_distributed_transaction *transaction,
	const char *message, size_t message_length, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];

	if (transaction == NULL || transaction->handle == NULL || transaction->prepared) {
		return ib_fail(error, "distributed transaction is not active or is already prepared");
	}
	if (message_length > (size_t) SHRT_MAX ||
		(message_length != 0U && message == NULL)) {
		return ib_fail(error, "distributed transaction recovery message is invalid");
	}
	if (message == NULL) {
		message = "";
	}
	if (ib_distributed_close_cursors(transaction, error) != 0) {
		return -1;
	}
	memset(status, 0, sizeof(status));
	if (isc_prepare_transaction2(status, &transaction->handle, (short) message_length,
		(char *) message) != 0) {
		ib_distributed_sync_handles(transaction);
		ib_distributed_mark_broken(transaction);
		return ib_fail_status(error, "prepare distributed transaction", status);
	}
	ib_distributed_sync_handles(transaction);
	transaction->prepared = 1;
	return 0;
}

int ib_distributed_commit(ib_distributed_transaction *transaction, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];

	if (transaction == NULL || transaction->handle == NULL || !transaction->prepared) {
		return ib_fail(error, "distributed transaction is not prepared");
	}
	memset(status, 0, sizeof(status));
	if (isc_commit_transaction(status, &transaction->handle) != 0) {
		ib_distributed_sync_handles(transaction);
		ib_distributed_mark_broken(transaction);
		return ib_fail_status(error, "commit distributed transaction", status);
	}
	ib_distributed_sync_handles(transaction);
	return 0;
}

int ib_distributed_rollback(ib_distributed_transaction *transaction, char **error)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	char *first_error = NULL;
	char *rollback_error = NULL;
	int failed = 0;

	if (transaction == NULL || transaction->handle == NULL) {
		return ib_fail(error, "distributed transaction is not active");
	}
	if (ib_distributed_close_cursors(transaction, &first_error) != 0) {
		failed = 1;
	}
	memset(status, 0, sizeof(status));
	if (isc_rollback_transaction(status, &transaction->handle) != 0) {
		failed = 1;
		ib_distributed_sync_handles(transaction);
		ib_distributed_mark_broken(transaction);
		(void) ib_fail_status(&rollback_error,
			"rollback distributed transaction", status);
		ib_append_error(&first_error, rollback_error);
	} else {
		ib_distributed_sync_handles(transaction);
	}
	if (failed) {
		ib_give_error(error, first_error);
		return -1;
	}
	ib_give_error(error, first_error);
	return 0;
}

int ib_distributed_handle_state(const ib_distributed_transaction *transaction)
{
	if (transaction == NULL) {
		return IB_HANDLE_UNKNOWN;
	}
	return transaction->handle == NULL ? IB_HANDLE_CONSUMED : IB_HANDLE_LIVE;
}

void ib_distributed_free(ib_distributed_transaction *transaction)
{
	if (transaction == NULL) {
		return;
	}
	ib_distributed_free_parts(transaction);
	free(transaction);
}

int ib_array_view_dimensions(const ib_array_view *array)
{
	if (array == NULL || array->dimensions < 0) {
		return 0;
	}
	return array->dimensions;
}

size_t ib_array_view_element_count(const ib_array_view *array)
{
	if (array == NULL) {
		return 0U;
	}
	return array->element_count;
}

int ib_array_view_bound(const ib_array_view *array, size_t index,
	ib_array_bound *bound, char **error)
{
	if (array == NULL || bound == NULL || index >= (size_t) array->dimensions ||
		array->bounds == NULL) {
		return ib_fail(error, "array result bound is unavailable");
	}
	*bound = array->bounds[index];
	return 0;
}

int ib_array_view_element(const ib_array_view *array, size_t index,
	ib_value_view *view, char **error)
{
	if (array == NULL || view == NULL || index >= array->element_count ||
		array->elements == NULL) {
		return ib_fail(error, "array result element is unavailable");
	}
	*view = array->elements[index];
	return 0;
}

void ib_error_free(char *error)
{
	free(error);
}
