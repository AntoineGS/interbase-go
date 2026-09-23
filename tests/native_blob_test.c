#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

static int fail_message_allocation;
static int fail_close;
static int close_calls;
static int open_calls;
static int open_blob2_calls;
static int lookup_desc_calls;
static int get_segment_calls;
static int cancel_calls;
static const unsigned char *controlled_payload;
static size_t controlled_payload_length;
static int fail_segment;
static int fail_open;
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
#define isc_blob_lookup_desc2 test_blob_lookup_desc2
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
	if (fail_open) {
		return status_result(status, isc_network_error);
	}
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
	if (fail_open) {
		return status_result(status, isc_network_error);
	}
	*blob = &blob_token;
	return status_result(status, 0);
}

ISC_STATUS ISC_EXPORT test_get_segment(ISC_STATUS *status, isc_blob_handle *blob,
	unsigned short *length, unsigned short buffer_length, char *buffer)
{
	static const size_t total_length = 90000U;
	static unsigned char value;
	size_t source_length = controlled_payload == NULL ? total_length : controlled_payload_length;
	size_t remaining;
	size_t chunk;

	(void) blob;
	get_segment_calls++;
	if (fail_segment) {
		return status_result(status, isc_network_error);
	}
	if (blob_offset == source_length) {
		*length = 0;
		return status_result(status, isc_segstr_eof);
	}
	remaining = source_length - blob_offset;
	chunk = remaining > (size_t) buffer_length ? (size_t) buffer_length : remaining;
	if (controlled_payload != NULL) {
		memcpy(buffer, controlled_payload + blob_offset, chunk);
	} else {
		for (size_t index = 0U; index < chunk; index++) {
			buffer[index] = (char) (value + (unsigned char) ((blob_offset + index) & 0xffU));
		}
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
	cancel_calls++;
	return status_result(status, isc_network_error);
}

ISC_STATUS ISC_EXPORT test_blob_lookup_desc2(ISC_STATUS *status,
	isc_db_handle *database, isc_tr_handle *transaction, unsigned char *relation,
	unsigned char *field, ISC_BLOB_DESC_V2 *descriptor, unsigned char *global_field)
{
	(void) database;
	(void) transaction;
	(void) relation;
	(void) field;
	(void) global_field;
	lookup_desc_calls++;
	memset(descriptor, 0, sizeof(*descriptor));
	descriptor->blob_desc_subtype = 1;
	descriptor->blob_desc_charset = 3;
	return status_result(status, 0);
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

static void reset_controlled_blob(const unsigned char *payload, size_t length)
{
	controlled_payload = payload;
	controlled_payload_length = length;
	fail_segment = 0;
	fail_open = 0;
	blob_offset = 0U;
	open_calls = 0;
	open_blob2_calls = 0;
	close_calls = 0;
	lookup_desc_calls = 0;
	blob_gen_bpb2_calls = 0;
	get_segment_calls = 0;
	cancel_calls = 0;
}

static XSQLDA *make_catalog_output(ISC_QUAD *blob_id, short *indicator,
	short subtype, const char *relation, const char *field)
{
	XSQLDA *output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	if (output == NULL) {
		return NULL;
	}
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_BLOB | 1;
	output->sqlvar[0].sqlsubtype = subtype;
	output->sqlvar[0].sqldata = (char *) blob_id;
	output->sqlvar[0].sqlind = indicator;
	if (relation != NULL) {
		memcpy(output->sqlvar[0].relname, relation, strlen(relation));
		output->sqlvar[0].relname_length = (short) strlen(relation);
	}
	if (field != NULL) {
		memcpy(output->sqlvar[0].sqlname, field, strlen(field));
		output->sqlvar[0].sqlname_length = (short) strlen(field);
	}
	return output;
}

static void test_catalog_text_is_raw_read_and_locally_decoded(short attachment_charset,
	short catalog_charset, const unsigned char *payload, size_t payload_length,
	const unsigned char *expected, size_t expected_length, const char *relation,
	const char *field)
{
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = attachment_charset;
	connection.catalog_text_charset = catalog_charset;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	check(output != NULL, "catalog SQLDA allocation failed");
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_BLOB | 1;
	output->sqlvar[0].sqlsubtype = 1;
	output->sqlvar[0].sqldata = (char *) &blob_id;
	output->sqlvar[0].sqlind = &indicator;
	memcpy(output->sqlvar[0].relname, relation, strlen(relation));
	output->sqlvar[0].relname_length = (short) strlen(relation);
	memcpy(output->sqlvar[0].sqlname, field, strlen(field));
	output->sqlvar[0].sqlname_length = (short) strlen(field);
	memcpy(output->sqlvar[0].aliasname, "USER_ALIAS", sizeof("USER_ALIAS") - 1U);
	output->sqlvar[0].aliasname_length = (short) (sizeof("USER_ALIAS") - 1U);
	cursor.output = output;
	reset_controlled_blob(payload, payload_length);
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"catalog BLOB column conversion failed");
	check(view.kind == IB_VALUE_STRING, "catalog BLOB was not returned as a string");
	check(view.length == expected_length && memcmp(view.bytes, expected, expected_length) == 0,
		"catalog BLOB bytes did not match selected local charset");
	check(open_calls == 1 && open_blob2_calls == 0,
		"catalog override did not use one raw BLOB open");
	check(blob_gen_bpb2_calls == 0 && lookup_desc_calls == 0,
		"catalog override unexpectedly requested descriptor conversion");
	check(close_calls == 1, "catalog BLOB was not closed");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_text_uses_configured_source_not_attachment(void)
{
	static const unsigned char legacy[] = {
		'C', 'R', 0xc9, 'A', 'T', 'I', 'O', 'N', '\r', '\n'
	};
	static const unsigned char utf8[] = {
		'C', 'R', 0xc3, 0x89, 'A', 'T', 'I', 'O', 'N', '\r', '\n'
	};
	static const unsigned char win1250_aogonek[] = {0xc4, 0x84};
	static const unsigned char win1252_yen[] = {0xc2, 0xa5};
	static const unsigned char aogonek_raw[] = {0xa5};
	test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_UTF8,
		IB_CHARSET_WIN1250, legacy, sizeof(legacy), utf8, sizeof(utf8),
		"RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE");
	test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_WIN1252,
		IB_CHARSET_WIN1250, legacy, sizeof(legacy), utf8, sizeof(utf8),
		"RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE");
	test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_UTF8,
		IB_CHARSET_WIN1250, aogonek_raw, sizeof(aogonek_raw), win1250_aogonek,
		sizeof(win1250_aogonek), "RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE");
	test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_UTF8,
		IB_CHARSET_WIN1252, aogonek_raw, sizeof(aogonek_raw), win1252_yen,
		sizeof(win1252_yen), "RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE");
	test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_UTF8,
		IB_CHARSET_WIN1250, (const unsigned char *) "", 0U,
		(const unsigned char *) "", 0U, "RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE");
}

static void test_catalog_segmented_high_bytes_expand_exactly(void)
{
	const size_t input_length = 90000U;
	const size_t expected_length = input_length * 2U;
	unsigned char *payload = (unsigned char *) malloc(input_length);
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;
	check(payload != NULL, "segmented expansion input allocation failed");
	memset(payload, 0xa5, input_length);
	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 1, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "segmented expansion SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(payload, input_length);
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"segmented legacy catalog conversion failed");
	check(view.kind == IB_VALUE_STRING && view.length == expected_length,
		"segmented conversion returned incorrect expanded length");
	for (size_t index = 0U; index < input_length; index++) {
		check((unsigned char) view.bytes[index * 2U] == 0xc4U &&
			(unsigned char) view.bytes[index * 2U + 1U] == 0x84U,
			"segmented conversion changed expanded character bytes");
	}
	check(get_segment_calls > 3 && open_calls == 1 && close_calls == 1 &&
		open_blob2_calls == 0 && lookup_desc_calls == 0,
		"segmented conversion did not use the raw BLOB lifecycle");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	free(payload);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_conversion_expansion_is_bounded(void)
{
	const size_t input_length = 33U * 1024U * 1024U + 1U;
	unsigned char *payload = (unsigned char *) malloc(input_length);
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;
	check(payload != NULL, "expansion-bound input allocation failed");
	memset(payload, 0xa5, input_length);
	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 1, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "expansion-bound SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(payload, input_length);
	check(ib_cursor_column(&cursor, 0U, &view, &error) != 0 && error != NULL,
		"conversion exceeding 64 MiB unexpectedly succeeded");
	check(strstr(error, "conversion limit") != NULL &&
		strstr(error, "RDB$PROCEDURES.RDB$PROCEDURE_SOURCE") != NULL,
		"expansion-bound error omitted limit or catalog context");
	check(open_calls == 1 && close_calls == 1 && cancel_calls == 0 &&
		open_blob2_calls == 0 && lookup_desc_calls == 0,
		"expansion failure violated raw BLOB lifecycle");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	free(payload);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_byte_length_preserves_binary_boundaries(void)
{
	static const unsigned char payload[] = {'A', 0x00, 0xc9, '\r', '\n', ' ', ' '};
	static const unsigned char expected[] = {'A', 0x00, 0xc3, 0x89, '\r', '\n', ' ', ' '};
	test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_UTF8,
		IB_CHARSET_WIN1250, payload, sizeof(payload), expected, sizeof(expected),
		"RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE");
}

static void test_catalog_allowlist_pairs(void)
{
	static const unsigned char source[] = {0xc9};
	static const unsigned char converted[] = {0xc3, 0x89};
	static const struct {
		const char *relation;
		const char *field;
	} pairs[] = {
		{"RDB$PROCEDURES", "RDB$PROCEDURE_SOURCE"},
		{"RDB$PROCEDURES", "RDB$DESCRIPTION"},
		{"RDB$PROCEDURE_PARAMETERS", "RDB$DESCRIPTION"},
		{"RDB$TRIGGERS", "RDB$TRIGGER_SOURCE"},
		{"RDB$TRIGGERS", "RDB$DESCRIPTION"},
		{"RDB$RELATIONS", "RDB$VIEW_SOURCE"},
		{"RDB$RELATIONS", "RDB$DESCRIPTION"},
		{"RDB$RELATION_FIELDS", "RDB$DEFAULT_SOURCE"},
		{"RDB$RELATION_FIELDS", "RDB$DESCRIPTION"},
		{"RDB$FIELDS", "RDB$DEFAULT_SOURCE"},
		{"RDB$FIELDS", "RDB$COMPUTED_SOURCE"},
		{"RDB$FIELDS", "RDB$VALIDATION_SOURCE"},
		{"RDB$FIELDS", "RDB$DESCRIPTION"},
		{"RDB$INDICES", "RDB$EXPRESSION_SOURCE"},
		{"RDB$INDICES", "RDB$DESCRIPTION"},
		{"RDB$FUNCTIONS", "RDB$DESCRIPTION"},
	};

	for (size_t index = 0U; index < sizeof(pairs) / sizeof(pairs[0]); index++) {
		test_catalog_text_is_raw_read_and_locally_decoded(IB_CHARSET_UTF8,
			IB_CHARSET_WIN1250, source, sizeof(source), converted, sizeof(converted),
			pairs[index].relation, pairs[index].field);
	}
}

static void test_catalog_classifier_requires_exact_scope(void)
{
	ib_connection connection;
	ib_cursor cursor;
	XSQLVAR variable;
	static const char relation[] = "RDB$PROCEDURES";
	static const char field[] = "RDB$PROCEDURE_SOURCE";

	memset(&connection, 0, sizeof(connection));
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	memset(&variable, 0, sizeof(variable));
	variable.sqltype = SQL_BLOB | 1;
	variable.sqlsubtype = 1;
	memcpy(variable.relname, relation, sizeof(relation) - 1U);
	variable.relname_length = (short) (sizeof(relation) - 1U);
	memcpy(variable.sqlname, field, sizeof(field) - 1U);
	variable.sqlname_length = (short) (sizeof(field) - 1U);
	check(ib_catalog_text_override(&cursor, &variable) == IB_CHARSET_WIN1250,
		"exact catalog source pair was not classified");
	cursor.allow_arrays = 1;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"direct API route received the materialized override");
	cursor.allow_arrays = 0;
	variable.sqlsubtype = 0;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"binary BLOB received the catalog text override");
	variable.sqlsubtype = 1;
	variable.sqltype = SQL_LONG | 1;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"non-BLOB SQLDA type was accepted");
	variable.sqltype = SQL_BLOB | 1;
	variable.relname[0] = 'r';
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"case-folded catalog relation was accepted");
	variable.relname[0] = 'R';
	variable.relname_length--;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"catalog relation prefix was accepted");
	variable.relname_length = -1;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"negative relation length was accepted");
	variable.relname_length = METADATALENGTH + 1;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"oversized relation length was accepted");
	variable.relname_length = (short) (sizeof(relation) - 1U);
	variable.sqlname_length = METADATALENGTH + 1;
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"oversized field length was accepted");
	variable.sqlname_length = (short) (sizeof(field) - 1U);
	variable.relname[0] = 'U';
	memcpy(variable.relname, "USER_TABLE", sizeof("USER_TABLE") - 1U);
	variable.relname_length = (short) (sizeof("USER_TABLE") - 1U);
	check(ib_catalog_text_override(&cursor, &variable) == 0,
		"user-table field received catalog override");
}

static void test_catalog_conversion_error_has_context_and_closes_blob(void)
{
	static const unsigned char invalid_ascii[] = {0xc9};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_ASCII;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	check(output != NULL, "catalog error SQLDA allocation failed");
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_BLOB | 1;
	output->sqlvar[0].sqlsubtype = 1;
	output->sqlvar[0].sqldata = (char *) &blob_id;
	output->sqlvar[0].sqlind = &indicator;
	memcpy(output->sqlvar[0].relname, "RDB$PROCEDURES", sizeof("RDB$PROCEDURES") - 1U);
	output->sqlvar[0].relname_length = (short) (sizeof("RDB$PROCEDURES") - 1U);
	memcpy(output->sqlvar[0].sqlname, "RDB$PROCEDURE_SOURCE", sizeof("RDB$PROCEDURE_SOURCE") - 1U);
	output->sqlvar[0].sqlname_length = (short) (sizeof("RDB$PROCEDURE_SOURCE") - 1U);
	cursor.output = output;
	reset_controlled_blob(invalid_ascii, sizeof(invalid_ascii));
	check(ib_cursor_column(&cursor, 0U, &view, &error) != 0 && error != NULL,
		"invalid ASCII catalog bytes were silently converted");
	check(strstr(error, "RDB$PROCEDURES.RDB$PROCEDURE_SOURCE") != NULL &&
		strstr(error, "ASCII") != NULL,
		"catalog conversion error omitted field or charset context");
	check(strstr(error, "\xc9") == NULL,
		"catalog conversion error exposed source contents");
	check(close_calls == 1 && open_calls == 1,
		"conversion failure occurred before raw BLOB close");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_non_allowlisted_field_keeps_declared_path(
	const char *relation, const char *field, const char *alias)
{
	static const unsigned char payload[] = {'A'};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	check(output != NULL, "declared-path SQLDA allocation failed");
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_BLOB | 1;
	output->sqlvar[0].sqlsubtype = 1;
	output->sqlvar[0].sqldata = (char *) &blob_id;
	output->sqlvar[0].sqlind = &indicator;
	memcpy(output->sqlvar[0].relname, relation, strlen(relation));
	output->sqlvar[0].relname_length = (short) strlen(relation);
	memcpy(output->sqlvar[0].sqlname, field, strlen(field));
	output->sqlvar[0].sqlname_length = (short) strlen(field);
	memcpy(output->sqlvar[0].aliasname, alias, strlen(alias));
	output->sqlvar[0].aliasname_length = (short) strlen(alias);
	cursor.output = output;
	reset_controlled_blob(payload, sizeof(payload));
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"non-allowlisted declared-path BLOB read failed");
	check(view.kind == IB_VALUE_STRING && view.length == sizeof(payload) &&
		memcmp(view.bytes, payload, sizeof(payload)) == 0,
		"non-allowlisted BLOB output changed");
	check(lookup_desc_calls == 1 && blob_gen_bpb2_calls == 1 &&
		open_blob2_calls == 1 && open_calls == 0,
		"non-allowlisted BLOB bypassed declared-charset conversion");
	check(close_calls == 1, "non-allowlisted BLOB was not closed");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_alias_does_not_expand_scope(void)
{
	test_catalog_non_allowlisted_field_keeps_declared_path("USER_TABLE",
		"RDB$PROCEDURE_SOURCE", "RDB$PROCEDURE_SOURCE");
	test_catalog_non_allowlisted_field_keeps_declared_path("rdb$procedures",
		"RDB$PROCEDURE_SOURCE", "PROCEDURE_SOURCE");
	test_catalog_non_allowlisted_field_keeps_declared_path("RDB$PROCEDURES_EXTRA",
		"RDB$PROCEDURE_SOURCE", "PROCEDURE_SOURCE");
	test_catalog_non_allowlisted_field_keeps_declared_path("RDB$",
		"RDB$PROCEDURE_SOURCE", "PROCEDURE_SOURCE");
	test_catalog_non_allowlisted_field_keeps_declared_path("RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE_EXTRA", "RDB$PROCEDURE_SOURCE");
	test_catalog_non_allowlisted_field_keeps_declared_path("RDB$PROCEDURES",
		"RDB$PROCEDURE", "PROCEDURE_SOURCE");
	test_catalog_non_allowlisted_field_keeps_declared_path("RDB$PROCEDURES",
		"rdb$procedure_source", "PROCEDURE_SOURCE");
}

static void test_catalog_null_and_open_close_failures(void)
{
	static const unsigned char payload[] = {'A'};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 1, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "null/error SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(payload, sizeof(payload));
	indicator = -1;
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL &&
		view.kind == IB_VALUE_NULL,
		"NULL catalog BLOB did not remain NULL");
	check(open_calls == 0 && open_blob2_calls == 0 && close_calls == 0 &&
		cancel_calls == 0,
		"NULL catalog BLOB opened a native handle");

	indicator = 0;
	reset_controlled_blob(payload, sizeof(payload));
	fail_open = 1;
	check(ib_cursor_column(&cursor, 0U, &view, &error) != 0 && error != NULL,
		"injected raw BLOB open failure was hidden");
	check(ib_error_has_native_status(error, isc_network_error) &&
		strstr(error, "open output BLOB") != NULL,
		"raw-open failure did not retain native error context");
	check(open_calls == 1 && open_blob2_calls == 0 && close_calls == 0 &&
		cancel_calls == 0 && connection.broken == 0,
		"raw-open failure cleanup state changed");
	ib_error_free(error);
	error = NULL;
	fail_open = 0;

	reset_controlled_blob(payload, sizeof(payload));
	fail_close = 1;
	check(ib_cursor_column(&cursor, 0U, &view, &error) != 0 && error != NULL,
		"injected raw BLOB close failure was hidden");
	check(ib_error_has_native_status(error, isc_network_error) &&
		strstr(error, "close blob") != NULL,
		"raw-close failure did not retain native error context");
	check(open_calls == 1 && open_blob2_calls == 0 && close_calls == 1 &&
		cancel_calls == 0 && connection.broken != 0,
		"raw-close failure did not preserve cleanup/broken state");
	ib_error_free(error);
	fail_close = 0;
	free(output);
}

static void test_catalog_unicode_user_blob_uses_declared_charset(void)
{
	static const unsigned char unicode[] = {0xc4, 0x84};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;
	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 1, "USER_TABLE", "TEXT_DATA");
	check(output != NULL, "user Unicode SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(unicode, sizeof(unicode));
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"enabled-override user Unicode BLOB failed");
	check(view.kind == IB_VALUE_STRING && view.length == sizeof(unicode) &&
		memcmp(view.bytes, unicode, sizeof(unicode)) == 0,
		"user Unicode BLOB changed under catalog override");
	check(lookup_desc_calls == 1 && blob_gen_bpb2_calls == 1 &&
		blob_gen_source_charset == 3 && blob_gen_target_charset == IB_CHARSET_UTF8 &&
		open_blob2_calls == 1 && open_calls == 0,
		"user Unicode BLOB did not retain declared-charset conversion");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_disabled_override_uses_declared_charset(void)
{
	static const unsigned char payload[] = {0xc4, 0x84};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;
	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = 0;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 1, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "disabled-override SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(payload, sizeof(payload));
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"disabled catalog override declared path failed");
	check(view.kind == IB_VALUE_STRING && view.length == sizeof(payload) &&
		memcmp(view.bytes, payload, sizeof(payload)) == 0,
		"disabled catalog override changed declared UTF8 output");
	check(lookup_desc_calls == 1 && blob_gen_bpb2_calls == 1 &&
		blob_gen_source_charset == 3 && blob_gen_target_charset == IB_CHARSET_UTF8 &&
		open_blob2_calls == 1 && open_calls == 0,
		"disabled catalog override did not retain charset 3 to 59 BPB conversion");
	free(cursor.materialized_value);
	free(cursor.converted_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
}

static void test_catalog_binary_and_direct_blob_ref_are_unchanged(void)
{
	static const unsigned char binary[] = {0x00, 0xff, 0xc9, '\r', '\n'};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {17, 29U};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.charset = IB_CHARSET_UTF8;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 0, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "binary BLOB SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(binary, sizeof(binary));
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"catalog binary BLOB materialization failed");
	check(view.kind == IB_VALUE_BYTES && view.length == sizeof(binary) &&
		memcmp(view.bytes, binary, sizeof(binary)) == 0,
		"binary/BLR BLOB bytes were converted or changed");
	check(open_calls == 1 && open_blob2_calls == 0 && lookup_desc_calls == 0 &&
		blob_gen_bpb2_calls == 0 && close_calls == 1,
		"binary BLOB used text descriptor conversion");
	free(cursor.materialized_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;

	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = make_catalog_output(&blob_id, &indicator, 2, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "BLR BLOB SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(binary, sizeof(binary));
	error = NULL;
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"catalog BLR BLOB materialization failed");
	check(view.kind == IB_VALUE_BYTES && view.length == sizeof(binary) &&
		memcmp(view.bytes, binary, sizeof(binary)) == 0 && open_calls == 1 &&
		open_blob2_calls == 0 && lookup_desc_calls == 0 &&
		blob_gen_bpb2_calls == 0 && close_calls == 1,
		"BLR BLOB bytes changed or used text conversion");
	free(cursor.materialized_value);
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;

	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	cursor.allow_arrays = 1;
	output = make_catalog_output(&blob_id, &indicator, 1, "RDB$PROCEDURES",
		"RDB$PROCEDURE_SOURCE");
	check(output != NULL, "direct BlobRef SQLDA allocation failed");
	cursor.output = output;
	reset_controlled_blob(binary, sizeof(binary));
	error = NULL;
	check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
		"direct BlobRef result failed");
	check(view.kind == IB_VALUE_BLOB_REF && view.blob_high == 17 &&
		view.blob_low == 29U && view.blob_subtype == 1 && view.blob_charset == 3,
		"direct BlobRef semantics changed by materialized override");
	check(open_calls == 0 && open_blob2_calls == 0 && close_calls == 0 &&
		lookup_desc_calls == 1 && blob_gen_bpb2_calls == 0,
		"direct BlobRef was materialized or filtered");
	free(output);
	ib_error_free(error);
}

static void test_catalog_invalid_metadata_lengths_fall_back_without_overread(void)
{
	static const unsigned char payload[] = {0xa5};
	for (int variant = 0; variant < 2; variant++) {
		ib_connection connection;
		ib_cursor cursor;
		XSQLDA *output;
		ISC_QUAD blob_id = {0};
		short indicator = 0;
		ib_value_view view;
		char *error = NULL;
		memset(&connection, 0, sizeof(connection));
		connection.database = &handle_token;
		connection.charset = IB_CHARSET_UTF8;
		connection.catalog_text_charset = IB_CHARSET_WIN1250;
		memset(&cursor, 0, sizeof(cursor));
		cursor.connection = &connection;
		cursor.transaction = &handle_token;
		cursor.fetched = 1;
		output = make_catalog_output(&blob_id, &indicator, 1, "RDB$PROCEDURES",
			"RDB$PROCEDURE_SOURCE");
		check(output != NULL, "invalid-metadata SQLDA allocation failed");
		if (variant == 0) {
			output->sqlvar[0].sqlname_length = -1;
		} else {
			output->sqlvar[0].relname_length = 0;
		}
		cursor.output = output;
		reset_controlled_blob(payload, sizeof(payload));
		check(ib_cursor_column(&cursor, 0U, &view, &error) == 0 && error == NULL,
			"invalid metadata fallback BLOB read failed");
		check(view.kind == IB_VALUE_STRING && view.length == sizeof(payload) &&
			(unsigned char) view.bytes[0] == payload[0],
			"invalid metadata path changed payload bytes");
		check(open_calls == 1 && open_blob2_calls == 0 && close_calls == 1 &&
			lookup_desc_calls == 0 && blob_gen_bpb2_calls == 0,
			"invalid metadata triggered descriptor access or override");
		free(cursor.materialized_value);
		free(output);
		ib_error_free(error);
		controlled_payload = NULL;
		controlled_payload_length = 0U;
	}
}

static void test_catalog_read_failure_cancels_and_marks_connection_broken(void)
{
	static const unsigned char payload[] = {'A'};
	ib_connection connection;
	ib_cursor cursor;
	XSQLDA *output;
	ISC_QUAD blob_id = {0};
	short indicator = 0;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &handle_token;
	connection.catalog_text_charset = IB_CHARSET_WIN1250;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = &handle_token;
	cursor.fetched = 1;
	output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	check(output != NULL, "read-failure SQLDA allocation failed");
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_BLOB | 1;
	output->sqlvar[0].sqlsubtype = 1;
	output->sqlvar[0].sqldata = (char *) &blob_id;
	output->sqlvar[0].sqlind = &indicator;
	memcpy(output->sqlvar[0].relname, "RDB$PROCEDURES", sizeof("RDB$PROCEDURES") - 1U);
	output->sqlvar[0].relname_length = (short) (sizeof("RDB$PROCEDURES") - 1U);
	memcpy(output->sqlvar[0].sqlname, "RDB$PROCEDURE_SOURCE", sizeof("RDB$PROCEDURE_SOURCE") - 1U);
	output->sqlvar[0].sqlname_length = (short) (sizeof("RDB$PROCEDURE_SOURCE") - 1U);
	cursor.output = output;
	reset_controlled_blob(payload, sizeof(payload));
	fail_segment = 1;
	check(ib_cursor_column(&cursor, 0U, &view, &error) != 0 && error != NULL,
		"injected catalog segment failure was hidden");
	check(strstr(error, "read output BLOB segment") != NULL,
		"segment failure did not preserve its native context");
	check(cancel_calls == 1 && close_calls == 1 && connection.broken != 0,
		"segment failure did not cancel and poison the failed BLOB connection");
	free(output);
	ib_error_free(error);
	controlled_payload = NULL;
	controlled_payload_length = 0U;
	fail_segment = 0;
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
	test_catalog_text_uses_configured_source_not_attachment();
	test_catalog_segmented_high_bytes_expand_exactly();
	test_catalog_conversion_expansion_is_bounded();
	test_catalog_byte_length_preserves_binary_boundaries();
	test_catalog_allowlist_pairs();
	test_catalog_classifier_requires_exact_scope();
	test_catalog_conversion_error_has_context_and_closes_blob();
	test_catalog_alias_does_not_expand_scope();
	test_catalog_null_and_open_close_failures();
	test_catalog_unicode_user_blob_uses_declared_charset();
	test_catalog_disabled_override_uses_declared_charset();
	test_catalog_binary_and_direct_blob_ref_are_unchanged();
	test_catalog_invalid_metadata_lengths_fall_back_without_overread();
	test_catalog_read_failure_cancels_and_marks_connection_broken();
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
