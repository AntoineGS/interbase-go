#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int fail_message_allocation;
static int fail_close;
static int close_calls;
static int open_calls;
static int open_blob2_calls;
static size_t blob_offset;
static char blob_token;
static char handle_token;
static size_t writer_offset;
static int writer_bytes_valid;
static int blob_gen_bpb2_calls;
static short blob_gen_target_subtype;
static short blob_gen_target_charset;
static short blob_gen_source_subtype;
static short blob_gen_source_charset;

static void *test_malloc(size_t size)
{
	return fail_message_allocation ? NULL : malloc(size);
}

#define malloc test_malloc
#define isc_open_blob test_open_blob
#define isc_open_blob2 test_open_blob2
#define isc_get_segment test_get_segment
#define isc_close_blob test_close_blob
#define isc_cancel_blob test_cancel_blob
#define isc_blob_gen_bpb2 test_blob_gen_bpb2
#define isc_create_blob2 test_create_blob2
#define isc_put_segment test_put_segment
#include "../native.c"
#undef malloc

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native BLOB test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void check_case(int condition, const char *name, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native BLOB test failed (%s): %s\n", name, message);
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

ISC_STATUS ISC_EXPORT test_open_blob2(ISC_STATUS *status, isc_db_handle *database,
	isc_tr_handle *transaction, isc_blob_handle *blob, ISC_QUAD *blob_id,
	short bpb_length, char *bpb)
{
	(void) database;
	(void) transaction;
	(void) blob_id;
	(void) bpb_length;
	(void) bpb;
	open_blob2_calls++;
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

ISC_STATUS ISC_EXPORT test_blob_gen_bpb2(ISC_STATUS *status,
	ISC_BLOB_DESC_V2 *target, ISC_BLOB_DESC_V2 *source,
	unsigned short bpb_buffer_length, unsigned char *bpb,
	unsigned short *bpb_length)
{
	(void) bpb_buffer_length;
	(void) bpb;
	blob_gen_bpb2_calls++;
	blob_gen_target_subtype = target->blob_desc_subtype;
	blob_gen_target_charset = target->blob_desc_charset;
	blob_gen_source_subtype = source->blob_desc_subtype;
	blob_gen_source_charset = source->blob_desc_charset;
	*bpb_length = 0U;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_create_blob2(ISC_STATUS *status,
	isc_db_handle *database, isc_tr_handle *transaction, isc_blob_handle *blob,
	ISC_QUAD *blob_id, short bpb_length, char *bpb)
{
	(void) database;
	(void) transaction;
	(void) bpb_length;
	(void) bpb;
	blob_id->isc_quad_high = 17;
	blob_id->isc_quad_low = 29U;
	*blob = &blob_token;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_put_segment(ISC_STATUS *status,
	isc_blob_handle *blob, unsigned short length, char *data)
{
	(void) blob;
	for (unsigned short index = 0U; index < length; index++) {
		if ((unsigned char) data[index] != (unsigned char) ((writer_offset + index) & 0xffU)) {
			writer_bytes_valid = 0;
		}
	}
	writer_offset += length;
	return status_result(status, 0);
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

static void test_stream_reader(void)
{
	ib_connection connection;
	ib_blob_reader *reader;
	char buffer[137];
	char *error = NULL;
	size_t length;
	size_t total;
	int eof;
	int result;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	blob_offset = 0U;
	open_calls = 0;
	close_calls = 0;
	reader = ib_blob_open(&connection, 1, 2U, &error);
	check(reader != NULL && error == NULL, "stream BLOB open failed");
	total = 0U;
	for (;;) {
		length = 0U;
		eof = 0;
		error = NULL;
		result = ib_blob_reader_read(reader, buffer, sizeof(buffer), &length, &eof,
			&error);
		check(result == 0 && error == NULL, "stream BLOB read failed");
		for (size_t index = 0U; index < length; index++) {
			check((unsigned char) buffer[index] == (unsigned char) ((total + index) & 0xffU),
				"stream BLOB bytes changed");
		}
		total += length;
		if (eof) {
			break;
		}
		check(length != 0U, "stream BLOB reader made no progress");
	}
	check(total == 90000U, "stream BLOB length changed");
	error = NULL;
	check(ib_blob_reader_close(reader, 0, &error) == 0 && error == NULL,
		"stream BLOB close failed");
	check(open_calls == 1 && close_calls == 1,
		"stream BLOB handle lifecycle is wrong");
}

static void test_stream_reader_uses_text_blob_descriptor(void)
{
	ib_connection connection;
	ib_blob_reader *reader;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	blob_offset = 0U;
	open_calls = 0;
	open_blob2_calls = 0;
	reader = ib_blob_open2(&connection, 1, 2U, 1, 51, &error);
	check(reader != NULL && error == NULL, "charset-aware stream BLOB open failed");
	check(open_calls == 0 && open_blob2_calls == 1,
		"text BLOB stream did not use the charset-aware open path");
	error = NULL;
	check(ib_blob_reader_close(reader, 0, &error) == 0 && error == NULL,
		"charset-aware stream BLOB close failed");
}

static void test_stream_writer(void)
{
	ib_connection connection;
	ib_blob_writer *writer;
	char payload[90000];
	char *error = NULL;
	int32_t high = 0;
	uint32_t low = 0U;
	int subtype = 0;
	int charset = 0;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	writer_offset = 0U;
	writer_bytes_valid = 1;
	open_calls = 0;
	close_calls = 0;
	writer = ib_blob_create(&connection, 1, 59, &error);
	check(writer != NULL && error == NULL, "stream BLOB create failed");
	for (size_t index = 0U; index < sizeof(payload); index++) {
		payload[index] = (char) (index & 0xffU);
	}
	error = NULL;
	check(ib_blob_writer_write(writer, payload, sizeof(payload), &error) == 0 &&
		error == NULL, "stream BLOB write failed");
	error = NULL;
	check(ib_blob_writer_close(writer, 0, &high, &low, &subtype, &charset,
		&error) == 0 && error == NULL, "stream BLOB writer close failed");
	check(writer_offset == sizeof(payload) && writer_bytes_valid,
		"stream BLOB writer changed bytes");
	check(high == 17 && low == 29U && subtype == 1 && charset == 59,
		"stream BLOB reference changed");
	check(close_calls == 1, "stream BLOB writer did not close its handle");
}

static void test_stream_writer_uses_utf8_source_for_text_target(void)
{
	ib_connection connection;
	ib_blob_writer *writer;
	char *error = NULL;
	int32_t high = 0;
	uint32_t low = 0U;
	int subtype = 0;
	int charset = 0;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	blob_gen_bpb2_calls = 0;
	writer = ib_blob_create(&connection, 1, 51, &error);
	check(writer != NULL && error == NULL,
		"charset-aware stream BLOB create failed");
	check(blob_gen_bpb2_calls == 1 && blob_gen_target_subtype == 1 &&
		blob_gen_target_charset == 51 && blob_gen_source_subtype == 1 &&
		blob_gen_source_charset == IB_CHARSET_UTF8,
		"charset-aware stream BLOB did not describe UTF8 input and target charset");
	check(ib_blob_writer_close(writer, 0, &high, &low, &subtype, &charset,
		&error) == 0 && error == NULL,
		"charset-aware stream BLOB close failed");
}

static void test_stream_writer_ignores_charset_for_binary_target(void)
{
	ib_connection connection;
	ib_blob_writer *writer;
	char *error = NULL;
	int32_t high = 0;
	uint32_t low = 0U;
	int subtype = -1;
	int charset = -1;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	blob_gen_bpb2_calls = 0;
	writer = ib_blob_create(&connection, 0, 59, &error);
	check(writer != NULL && error == NULL,
		"binary stream BLOB create failed");
	check(blob_gen_bpb2_calls == 1 && blob_gen_target_subtype == 0 &&
		blob_gen_target_charset == 0 && blob_gen_source_subtype == 0 &&
		blob_gen_source_charset == 0,
		"binary stream BLOB did not normalize its character set");
	check(ib_blob_writer_close(writer, 0, &high, &low, &subtype, &charset,
		&error) == 0 && error == NULL,
		"binary stream BLOB close failed");
	check(subtype == 0 && charset == 0,
		"binary stream BLOB reference retained a character set");
}

static void test_reader_cleanup_failure_marks_connection_broken(void)
{
	ib_connection connection;
	ib_blob_reader *reader;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	blob_offset = 0U;
	reader = ib_blob_open(&connection, 1, 2U, &error);
	check(reader != NULL && error == NULL, "failure-injection reader open failed");
	fail_close = 1;
	fail_message_allocation = 1;
	check(ib_blob_reader_close(reader, 0, &error) == -1,
		"reader cleanup failure was hidden");
	check(connection.broken != 0,
		"reader cleanup failure did not poison the connection");
	check(error == NULL,
		"reader cleanup unexpectedly allocated an error diagnostic");
	ib_error_free(error);
	fail_message_allocation = 0;
	fail_close = 0;
}

static void test_writer_cleanup_failure_marks_connection_broken(void)
{
	ib_connection connection;
	ib_blob_writer *writer;
	char *error = NULL;
	int32_t high = 0;
	uint32_t low = 0U;
	int subtype = 0;
	int charset = 0;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.transaction = &handle_token;
	writer = ib_blob_create(&connection, 1, 59, &error);
	check(writer != NULL && error == NULL, "failure-injection writer open failed");
	fail_close = 1;
	fail_message_allocation = 1;
	check(ib_blob_writer_close(writer, 0, &high, &low, &subtype, &charset, &error) == -1,
		"writer cleanup failure was hidden");
	check(connection.broken != 0,
		"writer cleanup failure did not poison the connection");
	check(error == NULL,
		"writer cleanup unexpectedly allocated an error diagnostic");
	ib_error_free(error);
	fail_message_allocation = 0;
	fail_close = 0;
}

static void test_blob_destination_requires_direct_parameter_expression(void)
{
	static const struct {
		const char *name;
		const char *query;
		size_t parameter_index;
		int want_success;
	} tests[] = {
		{
			.name = "insert direct bare parameter",
			.query = "INSERT INTO GO_BLOB (ID, DATA_VALUE) VALUES (?, /* before */ ? /* after */)",
			.parameter_index = 1U,
			.want_success = 1,
		},
		{
			.name = "insert cast parameter",
			.query = "INSERT INTO GO_BLOB (ID, DATA_VALUE) VALUES (?, CAST(? AS BLOB SUB_TYPE 1 CHARACTER SET UTF8))",
			.parameter_index = 1U,
			.want_success = 0,
		},
		{
			.name = "insert function parameter",
			.query = "INSERT INTO GO_BLOB (ID, DATA_VALUE) VALUES (?, COALESCE(?, ?))",
			.parameter_index = 1U,
			.want_success = 0,
		},
		{
			.name = "insert subquery parameter",
			.query = "INSERT INTO GO_BLOB (ID, DATA_VALUE) VALUES (?, (SELECT ? FROM RDB$DATABASE))",
			.parameter_index = 1U,
			.want_success = 0,
		},
		{
			.name = "update direct bare parameter with comments",
			.query = "UPDATE GO_BLOB SET DATA_VALUE = /* before */ ? /* after */ WHERE ID = ?",
			.parameter_index = 0U,
			.want_success = 1,
		},
		{
			.name = "update cast parameter",
			.query = "UPDATE GO_BLOB SET DATA_VALUE = CAST(? AS BLOB SUB_TYPE 1 CHARACTER SET UTF8) WHERE ID = ?",
			.parameter_index = 0U,
			.want_success = 0,
		},
	};

	for (size_t index = 0U; index < sizeof(tests) / sizeof(tests[0]); index++) {
		ib_cursor cursor;
		char relation[METADATALENGTH + 1U];
		char field[METADATALENGTH + 1U];
		char *error = NULL;
		int result;

		memset(&cursor, 0, sizeof(cursor));
		cursor.query = (char *) tests[index].query;
		cursor.query_length = strlen(tests[index].query);
		memset(relation, 0, sizeof(relation));
		memset(field, 0, sizeof(field));
		result = ib_blob_destination_names(&cursor, tests[index].parameter_index,
			relation, field, &error);
		if (tests[index].want_success) {
			check_case(result == 0 && error == NULL, tests[index].name,
				"direct BLOB parameter expression was rejected");
			check_case(strcmp(relation, "GO_BLOB") == 0 && strcmp(field, "DATA_VALUE") == 0,
				tests[index].name,
				"direct BLOB parameter destination changed");
		} else {
			check_case(result != 0 && error != NULL &&
				strstr(error, "metadata is unavailable") != NULL,
				tests[index].name,
				"nested BLOB parameter expression was attributed to its outer field");
		}
		ib_error_free(error);
	}
}

int main(void)
{
	test_cleanup_failure_without_message();
	test_large_segment_materialization();
	test_stream_reader();
	test_stream_reader_uses_text_blob_descriptor();
	test_stream_writer();
	test_stream_writer_uses_utf8_source_for_text_target();
	test_stream_writer_ignores_charset_for_binary_target();
	test_reader_cleanup_failure_marks_connection_broken();
	test_writer_cleanup_failure_marks_connection_broken();
	test_blob_destination_requires_direct_parameter_expression();
	puts("native BLOB tests passed");
	return EXIT_SUCCESS;
}
