#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int fail_message_allocation;
static int fail_close;
static int close_calls;
static int open_calls;
static size_t blob_offset;
static char blob_token;
static char handle_token;

static void *test_malloc(size_t size)
{
	return fail_message_allocation ? NULL : malloc(size);
}

#define malloc test_malloc
#define isc_open_blob test_open_blob
#define isc_get_segment test_get_segment
#define isc_close_blob test_close_blob
#define isc_cancel_blob test_cancel_blob
#include "../native.c"
#undef malloc

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native BLOB test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static ISC_STATUS status_result(ISC_STATUS *status, ISC_STATUS code)
{
	status[0] = isc_arg_gds;
	status[1] = code;
	status[2] = isc_arg_end;
	return code;
}

ISC_STATUS ISC_EXPORT test_open_blob(ISC_STATUS *status, isc_db_handle *database,
	isc_tr_handle *transaction, isc_blob_handle *blob, ISC_QUAD *blob_id)
{
	(void) database;
	(void) transaction;
	(void) blob_id;
	open_calls++;
	*blob = &blob_token;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_get_segment(ISC_STATUS *status, isc_blob_handle *blob,
	unsigned short *length, unsigned short buffer_length, char *buffer)
{
	static const size_t total_length = 90000U;
	static unsigned char value;
	size_t remaining;
	size_t chunk;

	(void) blob;
	if (blob_offset == total_length) {
		*length = 0;
		return status_result(status, isc_segstr_eof);
	}
	remaining = total_length - blob_offset;
	chunk = remaining > (size_t) buffer_length ? (size_t) buffer_length : remaining;
	for (size_t index = 0U; index < chunk; index++) {
		buffer[index] = (char) (value + (unsigned char) ((blob_offset + index) & 0xffU));
	}
	*length = (unsigned short) chunk;
	blob_offset += chunk;
	if (chunk < remaining) {
		return status_result(status, isc_segment);
	}
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_close_blob(ISC_STATUS *status, isc_blob_handle *blob)
{
	close_calls++;
	if (fail_close) {
		return status_result(status, isc_network_error);
	}
	*blob = NULL;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_cancel_blob(ISC_STATUS *status, isc_blob_handle *blob)
{
	(void) blob;
	return status_result(status, isc_network_error);
}

static void test_cleanup_failure_without_message(void)
{
	isc_blob_handle blob = &blob_token;
	char *error = NULL;
	int result;

	fail_close = 1;
	fail_message_allocation = 1;
	close_calls = 0;
	result = ib_blob_cleanup(&blob, 0, &error);
	fail_message_allocation = 0;
	fail_close = 0;
	check(result == -1, "cleanup failure was hidden by error allocation failure");
	check(blob == NULL, "cleanup left a failed blob handle live");
	check(close_calls == 1, "cleanup did not attempt close");
	check(error == NULL, "injected allocation unexpectedly returned an error");
	ib_error_free(error);
}

static void test_large_segment_materialization(void)
{
	ib_connection connection;
	ib_cursor cursor;
	XSQLVAR variable;
	ISC_QUAD blob_id = {0};
	char *data;
	char *error = NULL;
	size_t length = 0U;
	int is_utf8 = 0;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.dialect = SQL_DIALECT_V5;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	memset(&variable, 0, sizeof(variable));
	variable.sqltype = SQL_BLOB | 1;
	variable.sqlsubtype = 1;
	variable.sqldata = (char *) &blob_id;
	blob_offset = 0U;
	open_calls = 0;
	close_calls = 0;
	data = ib_read_blob(&cursor, &variable, &length, &is_utf8, &error);
	check(data != NULL && error == NULL, "large segmented BLOB read failed");
	check(open_calls == 1 && close_calls == 1, "large segmented BLOB handle lifecycle is wrong");
	check(length == 90000U, "large segmented BLOB length changed");
	for (size_t index = 0U; index < length; index++) {
		check((unsigned char) data[index] == (unsigned char) (index & 0xffU),
			"large segmented BLOB data changed");
	}
	free(data);
}

int main(void)
{
	test_cleanup_failure_without_message();
	test_large_segment_materialization();
	puts("native BLOB tests passed");
	return EXIT_SUCCESS;
}
