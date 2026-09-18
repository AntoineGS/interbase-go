#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <time.h>

#include "../native.c"

static void require_condition(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native values test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void require_success(int result, char *error, const char *message)
{
	require_condition(result == 0, message);
	require_condition(error == NULL, message);
}

static void test_positional_bind_values(void)
{
	static const char text[] = { 'A', '\0', (char) 0xc3, (char) 0xa9 };
	ib_cursor cursor;
	ib_bindings *bindings;
	ISC_INT64 integer;
	double floating;
	ISC_BOOLEAN boolean;
	unsigned short length;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.input = ib_alloc_sqlda(5);
	require_condition(cursor.input != NULL, "input SQLDA allocation failed");
	require_condition(cursor.input->sqldabc ==
		(ISC_LONG) (sizeof(XSQLDA) + 4U * sizeof(XSQLVAR)),
		"input SQLDA byte capacity is wrong");
	cursor.input->sqld = 5;
	cursor.input->sqlvar[0].sqltype = SQL_VARYING | 1;
	cursor.input->sqlvar[1].sqltype = SQL_INT64 | 1;
	cursor.input->sqlvar[2].sqltype = SQL_DOUBLE | 1;
	cursor.input->sqlvar[3].sqltype = SQL_BOOLEAN | 1;
	cursor.input->sqlvar[4].sqltype = SQL_LONG | 1;
	error = NULL;
	bindings = ib_bindings_new(5, &error);
	require_condition(bindings != NULL && error == NULL, "binding allocation failed");
	require_success(ib_bindings_set_string(bindings, 0, text, sizeof(text), &error),
		error, "string binding setup failed");
	require_success(ib_bindings_set_int64(bindings, 1, INT64_C(-9007199254740991), &error),
		error, "int64 binding setup failed");
	require_success(ib_bindings_set_float64(bindings, 2, 3.25, &error),
		error, "double binding setup failed");
	require_success(ib_bindings_set_bool(bindings, 3, 7, &error),
		error, "boolean binding setup failed");
	require_success(ib_bindings_set_null(bindings, 4, &error), error,
		"null binding setup failed");
	require_success(ib_bind_input(&cursor, bindings, &error), error,
		"positional binding failed");
	require_condition(cursor.input->sqlvar[0].sqltype == (SQL_VARYING | 1) &&
		cursor.input->sqlvar[0].sqllen == (short) sizeof(text),
		"string binding description is wrong");
	memcpy(&length, cursor.input->sqlvar[0].sqldata, sizeof(length));
	require_condition(length == sizeof(text) &&
		memcmp(cursor.input->sqlvar[0].sqldata + sizeof(length), text, sizeof(text)) == 0 &&
		*cursor.input->sqlvar[0].sqlind == 0, "string binding buffer is wrong");
	memcpy(&integer, cursor.input->sqlvar[1].sqldata, sizeof(integer));
	require_condition(integer == INT64_C(-9007199254740991) &&
		cursor.input->sqlvar[1].sqllen == sizeof(integer), "int64 binding buffer is wrong");
	memcpy(&floating, cursor.input->sqlvar[2].sqldata, sizeof(floating));
	require_condition(floating == 3.25 && cursor.input->sqlvar[2].sqlscale == 0,
		"double binding buffer is wrong");
	memcpy(&boolean, cursor.input->sqlvar[3].sqldata, sizeof(boolean));
	require_condition(boolean == ISC_TRUE && *cursor.input->sqlvar[3].sqlind == 0,
		"boolean binding buffer is wrong");
	require_condition(*cursor.input->sqlvar[4].sqlind == -1 &&
		(cursor.input->sqlvar[4].sqltype & 1) != 0, "null binding is wrong");
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
}

static void test_blob_reference_binding(void)
{
	ib_bindings *bindings;
	char *error;

	error = NULL;
	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"BLOB reference binding allocation failed");
	require_success(ib_bindings_set_blob_ref(bindings, 0, 17, 29U, 1,
		IB_CHARSET_UTF8, &error),
		error, "BLOB reference binding setup failed");
	require_condition(bindings->values[0].blob_high == 17 &&
		bindings->values[0].blob_low == 29U &&
		bindings->values[0].blob_subtype == 1 &&
		bindings->values[0].blob_charset == IB_CHARSET_UTF8,
		"BLOB reference binding changed its ID or descriptor");
	ib_bindings_free(bindings);
}

static void test_text_blob_reference_without_relation_is_rejected(void)
{
	ib_connection connection;
	ib_cursor cursor;
	XSQLVAR variable;
	ISC_QUAD blob_id;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.charset = IB_CHARSET_UTF8;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = (isc_tr_handle) (uintptr_t) 1U;
	memset(&variable, 0, sizeof(variable));
	variable.sqltype = SQL_BLOB;
	variable.sqlsubtype = 1;
	blob_id.isc_quad_high = 7;
	blob_id.isc_quad_low = 11U;
	variable.sqldata = (char *) &blob_id;
	require_condition(ib_cursor_blob_ref(&cursor, &variable, &view, &error) != 0 &&
		error != NULL, "unattributed text BLOB expression was accepted");
	ib_error_free(error);
}

static void test_binary_blob_reference_without_relation_is_preserved(void)
{
	ib_connection connection;
	ib_cursor cursor;
	XSQLVAR variable;
	ISC_QUAD blob_id;
	ib_value_view view;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.charset = IB_CHARSET_UTF8;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.transaction = (isc_tr_handle) (uintptr_t) 1U;
	memset(&variable, 0, sizeof(variable));
	variable.sqltype = SQL_BLOB;
	variable.sqlsubtype = 0;
	blob_id.isc_quad_high = 7;
	blob_id.isc_quad_low = 11U;
	variable.sqldata = (char *) &blob_id;
	require_success(ib_cursor_blob_ref(&cursor, &variable, &view, &error),
		error, "binary BLOB expression metadata was rejected");
	require_condition(view.kind == IB_VALUE_BLOB_REF && view.blob_high == 7 &&
		view.blob_low == 11U && view.blob_subtype == 0 && view.blob_charset == 0,
		"binary BLOB expression metadata changed");
}

static void test_column_decoding(void)
{
	static const char fixed_text[] = { 'A', ' ', ' ', ' ', ' ' };
	ib_cursor cursor;
	ib_value_view view;
	ISC_INT64 scaled = INT64_C(-12345);
	ISC_BOOLEAN boolean = ISC_TRUE;
	unsigned short varying_length = 4;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.output = ib_alloc_sqlda(5);
	require_condition(cursor.output != NULL, "output SQLDA allocation failed");
	cursor.output->sqld = 5;
	cursor.output->sqlvar[0].sqltype = SQL_TEXT | 1;
	cursor.output->sqlvar[0].sqllen = 5;
	cursor.output->sqlvar[1].sqltype = SQL_VARYING | 1;
	cursor.output->sqlvar[1].sqllen = 6;
	cursor.output->sqlvar[2].sqltype = SQL_INT64 | 1;
	cursor.output->sqlvar[2].sqlscale = -2;
	cursor.output->sqlvar[3].sqltype = SQL_BOOLEAN | 1;
	cursor.output->sqlvar[4].sqltype = SQL_LONG | 1;
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error,
		"output storage allocation failed");
	memcpy(cursor.output->sqlvar[0].sqldata, fixed_text, sizeof(fixed_text));
	memcpy(cursor.output->sqlvar[1].sqldata, &varying_length, sizeof(varying_length));
	memcpy(cursor.output->sqlvar[1].sqldata + sizeof(varying_length),
		"x\0\xc3\xa9", varying_length);
	memcpy(cursor.output->sqlvar[2].sqldata, &scaled, sizeof(scaled));
	memcpy(cursor.output->sqlvar[3].sqldata, &boolean, sizeof(boolean));
	*cursor.output->sqlvar[4].sqlind = -1;
	cursor.fetched = 1;
	require_success(ib_cursor_column(&cursor, 0, &view, &error), error, "CHAR decode failed");
	require_condition(view.kind == IB_VALUE_STRING && view.length == 5 &&
		memcmp(view.bytes, fixed_text, sizeof(fixed_text)) == 0, "CHAR trailing spaces changed");
	require_success(ib_cursor_column(&cursor, 1, &view, &error), error, "VARCHAR decode failed");
	require_condition(view.kind == IB_VALUE_STRING && view.length == 4 &&
		memcmp(view.bytes, "x\0\xc3\xa9", 4) == 0, "VARCHAR prefix or bytes changed");
	require_success(ib_cursor_column(&cursor, 2, &view, &error), error, "scaled integer decode failed");
	require_condition(view.kind == IB_VALUE_SCALED_INT && view.int64_value == scaled &&
		view.scale == -2, "scaled integer kind or scale is wrong");
	require_success(ib_cursor_column(&cursor, 3, &view, &error), error, "boolean decode failed");
	require_condition(view.kind == IB_VALUE_BOOL && view.bool_value, "boolean decode is wrong");
	require_success(ib_cursor_column(&cursor, 4, &view, &error), error, "null decode failed");
	require_condition(view.kind == IB_VALUE_NULL, "null column is not null");
	ib_free_sqlda(cursor.output);
}

static void test_utf8_fixed_text_and_scaled_float_decoding(void)
{
	static const char padded_text[] = "AA                  ";
	ib_cursor cursor;
	ib_value_view view;
	double floating = 100.11;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.output = ib_alloc_sqlda(2);
	require_condition(cursor.output != NULL, "UTF8 output SQLDA allocation failed");
	cursor.output->sqld = 2;
	cursor.output->sqlvar[0].sqltype = SQL_TEXT | 1;
	cursor.output->sqlvar[0].sqlsubtype = 59; /* InterBase UTF8 charset. */
	cursor.output->sqlvar[0].sqllen = (short) (sizeof(padded_text) - 1U);
	cursor.output->sqlvar[0].relname[0] = 'T';
	cursor.output->sqlvar[0].relname_length = 1;
	cursor.output->sqlvar[0].sqlname[0] = 'C';
	cursor.output->sqlvar[0].sqlname_length = 1;
	cursor.output->sqlvar[1].sqltype = SQL_DOUBLE | 1;
	cursor.output->sqlvar[1].sqlscale = -2;
	error = NULL;
	require_success(ib_validate_output_types(cursor.output, &error), error,
		"Dialect 1 scaled floating output was rejected");
	require_success(ib_allocate_output(&cursor, &error), error,
		"UTF8/scaled floating output storage failed");
	memcpy(cursor.output->sqlvar[0].sqldata, padded_text, sizeof(padded_text) - 1U);
	memcpy(cursor.output->sqlvar[1].sqldata, &floating, sizeof(floating));
	cursor.fetched = 1;

	require_success(ib_cursor_column(&cursor, 0, &view, &error), error,
		"UTF8 CHAR decode failed");
	require_condition(view.kind == IB_VALUE_STRING && view.length == 5U &&
		memcmp(view.bytes, "AA   ", 5U) == 0,
		"UTF8 CHAR padding was measured in bytes instead of characters");
	require_success(ib_cursor_column(&cursor, 1, &view, &error), error,
		"scaled floating decode failed");
	require_condition(view.kind == IB_VALUE_FLOAT64 && view.float64_value == floating,
		"scaled floating value was not decoded as a float");
	ib_free_sqlda(cursor.output);
}

static void test_unattributed_utf8_text_preserves_literal_spaces(void)
{
	static const char trailing_spaces[] = {'a', ' ', ' ', ' '};
	static const char unicode_trailing_spaces[] = {(char) 0xc3, (char) 0xa9, ' ', ' '};
	ib_cursor cursor;
	ib_value_view view;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "unattributed UTF8 output SQLDA allocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_TEXT | 1;
	cursor.output->sqlvar[0].sqlsubtype = IB_CHARSET_UTF8;
	cursor.output->sqlvar[0].sqllen = (short) sizeof(trailing_spaces);
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error,
		"unattributed UTF8 output storage failed");
	cursor.fetched = 1;

	memcpy(cursor.output->sqlvar[0].sqldata, trailing_spaces, sizeof(trailing_spaces));
	require_success(ib_cursor_column(&cursor, 0, &view, &error), error,
		"ASCII literal UTF8 decode failed");
	require_condition(view.length == sizeof(trailing_spaces) &&
		memcmp(view.bytes, trailing_spaces, sizeof(trailing_spaces)) == 0,
		"unattributed ASCII literal lost trailing spaces");

	memcpy(cursor.output->sqlvar[0].sqldata, unicode_trailing_spaces,
		sizeof(unicode_trailing_spaces));
	require_success(ib_cursor_column(&cursor, 0, &view, &error), error,
		"non-ASCII literal UTF8 decode failed");
	require_condition(view.length == sizeof(unicode_trailing_spaces) &&
		memcmp(view.bytes, unicode_trailing_spaces, sizeof(unicode_trailing_spaces)) == 0,
		"unattributed non-ASCII literal lost trailing spaces");
	ib_free_sqlda(cursor.output);
}

static void test_fixed_text_output_is_space_initialized(void)
{
	ib_cursor cursor;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "fixed text SQLDA allocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_TEXT | 1;
	cursor.output->sqlvar[0].sqllen = 5;
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error,
		"fixed text output storage failed");
	require_condition(memcmp(cursor.output->sqlvar[0].sqldata, "     ", 5U) == 0,
		"fixed text output storage was not space initialized");
	ib_free_sqlda(cursor.output);
}

static void test_octets_bind_and_decode_as_bytes(void)
{
	static const char payload[] = { 0, 1, 127, (char) 0x80, (char) 0xff };
	ib_connection connection;
	ib_cursor cursor;
	ib_bindings *bindings;
	ib_value_view view;
	unsigned short varying_length;
	char *error;

	memset(&connection, 0, sizeof(connection));
	connection.charset = IB_CHARSET_UTF8;
	connection.dialect = SQL_DIALECT_V5;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.input = ib_alloc_sqlda(2);
	require_condition(cursor.input != NULL, "OCTETS input SQLDA allocation failed");
	cursor.input->sqld = 2;
	cursor.input->sqlvar[0].sqltype = SQL_TEXT | 1;
	cursor.input->sqlvar[0].sqlsubtype = 1; /* InterBase OCTETS charset. */
	cursor.input->sqlvar[0].sqllen = sizeof(payload);
	cursor.input->sqlvar[1].sqltype = SQL_VARYING | 1;
	cursor.input->sqlvar[1].sqlsubtype = 1;
	cursor.input->sqlvar[1].sqllen = sizeof(payload);
	error = NULL;
	bindings = ib_bindings_new(2, &error);
	require_condition(bindings != NULL && error == NULL,
		"OCTETS binding allocation failed");
	require_success(ib_bindings_set_bytes(bindings, 0, payload, sizeof(payload), &error),
		error, "OCTETS fixed binding setup failed");
	require_success(ib_bindings_set_bytes(bindings, 1, payload, sizeof(payload), &error),
		error, "OCTETS varying binding setup failed");
	require_success(ib_bind_input(&cursor, bindings, &error), error,
		"OCTETS byte binding failed");
	require_condition(*cursor.input->sqlvar[0].sqlind == 0 &&
		memcmp(cursor.input->sqlvar[0].sqldata, payload, sizeof(payload)) == 0,
		"OCTETS fixed binding changed bytes");
	memcpy(&varying_length, cursor.input->sqlvar[1].sqldata, sizeof(varying_length));
	require_condition(varying_length == sizeof(payload) &&
		memcmp(cursor.input->sqlvar[1].sqldata + sizeof(varying_length), payload,
			sizeof(payload)) == 0, "OCTETS varying binding changed bytes");
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
	cursor.input = NULL;

	cursor.output = ib_alloc_sqlda(2);
	require_condition(cursor.output != NULL, "OCTETS output SQLDA allocation failed");
	cursor.output->sqld = 2;
	cursor.output->sqlvar[0].sqltype = SQL_TEXT | 1;
	cursor.output->sqlvar[0].sqlsubtype = 1;
	cursor.output->sqlvar[0].sqllen = sizeof(payload);
	cursor.output->sqlvar[1].sqltype = SQL_VARYING | 1;
	cursor.output->sqlvar[1].sqlsubtype = 1;
	cursor.output->sqlvar[1].sqllen = sizeof(payload);
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error,
		"OCTETS output storage allocation failed");
	memcpy(cursor.output->sqlvar[0].sqldata, payload, sizeof(payload));
	varying_length = sizeof(payload);
	memcpy(cursor.output->sqlvar[1].sqldata, &varying_length, sizeof(varying_length));
	memcpy(cursor.output->sqlvar[1].sqldata + sizeof(varying_length), payload,
		sizeof(payload));
	cursor.fetched = 1;
	error = NULL;
	require_success(ib_cursor_column(&cursor, 0, &view, &error), error,
		"OCTETS fixed decode failed");
	require_condition(view.kind == IB_VALUE_BYTES && view.length == sizeof(payload) &&
		memcmp(view.bytes, payload, sizeof(payload)) == 0,
		"OCTETS fixed value was not returned as unchanged bytes");
	error = NULL;
	require_success(ib_cursor_column(&cursor, 1, &view, &error), error,
		"OCTETS varying decode failed");
	require_condition(view.kind == IB_VALUE_BYTES && view.length == sizeof(payload) &&
		memcmp(view.bytes, payload, sizeof(payload)) == 0,
		"OCTETS varying value was not returned as unchanged bytes");
	ib_free_sqlda(cursor.output);
}

static int bind_scaled_string(const char *text, short scale, short precision,
	int64_t *raw, char **error)
{
	ib_connection connection;
	ib_cursor cursor;
	ib_bindings *bindings;
	int result;

	memset(&connection, 0, sizeof(connection));
	connection.dialect = SQL_DIALECT_V6;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.input = ib_alloc_sqlda(1);
	if (cursor.input == NULL) {
		return -1;
	}
	cursor.input->sqld = 1;
	cursor.input->sqlvar[0].sqltype = SQL_INT64 | 1;
	cursor.input->sqlvar[0].sqlsubtype = 1;
	cursor.input->sqlvar[0].sqlscale = scale;
	cursor.input->sqlvar[0].sqlprecision = precision;
	cursor.input->sqlvar[0].sqllen = (short) sizeof(ISC_INT64);
	bindings = ib_bindings_new(1, error);
	if (bindings == NULL) {
		ib_free_sqlda(cursor.input);
		return -1;
	}
	result = ib_bindings_set_string(bindings, 0, text, strlen(text), error);
	if (result == 0) {
		result = ib_bind_input(&cursor, bindings, error);
	}
	if (result == 0 && raw != NULL) {
		memcpy(raw, cursor.input->sqlvar[0].sqldata, sizeof(*raw));
	}
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
	return result;
}

static void test_exact_scaled_integer_binding(void)
{
	static const int64_t wide_raw = INT64_C(123456789012345678);
	static const struct {
		const char *name;
		const char *text;
		short scale;
		short precision;
		int64_t want;
	} accepted[] = {
		{"wide positive", "12345678901234.5678", -4, 18, wide_raw},
		{"wide negative", "-12345678901234.5678", -4, 18, -wide_raw},
		{"trailing zeroes", "1.230000", -2, 18, 123},
		{"precision maximum", "999999999999999999", 0, 18,
			INT64_C(999999999999999999)},
		{"precision negative maximum", "-999999999999999999", 0, 18,
			-INT64_C(999999999999999999)},
		{"int64 maximum", "9223372036854775807", 0, 0, INT64_MAX},
		{"int64 minimum", "-9223372036854775808", 0, 0, INT64_MIN},
		{"positive scaled maximum", "922337203685477580.7", -1, 0, INT64_MAX},
		{"negative scaled minimum", "-922337203685477580.8", -1, 0, INT64_MIN},
		{"positive scaled final digit", "922337203685477580.6", -1, 0,
			INT64_MAX - INT64_C(1)},
		{"negative scaled final digit", "-922337203685477580.7", -1, 0,
			INT64_MIN + INT64_C(1)}
	};
	static const struct {
		const char *name;
		const char *text;
		short scale;
		short precision;
	} rejected[] = {
		{"positive int64 overflow", "9223372036854775808", 0, 0},
		{"negative int64 overflow", "-9223372036854775809", 0, 0},
		{"positive scaled final digit overflow", "922337203685477580.8", -1, 0},
		{"negative scaled final digit overflow", "-922337203685477580.9", -1, 0},
		{"positive scale padding overflow", "9223372036854775807", -1, 0},
		{"negative scale padding overflow", "-9223372036854775808", -1, 0},
		{"positive precision overflow", "1000000000000000000", 0, 18},
		{"negative precision overflow", "-1000000000000000000", 0, 18},
		{"nonzero excess fraction", "1.001", -2, 18},
		{"scientific notation", "1e2", 0, 0}
	};
	size_t index;

	for (index = 0U; index < sizeof(accepted) / sizeof(accepted[0]); index++) {
		char *error = NULL;
		int64_t raw = 0;
		int result = bind_scaled_string(accepted[index].text, accepted[index].scale,
			accepted[index].precision, &raw, &error);

		require_condition(result == 0 && error == NULL && raw == accepted[index].want,
			accepted[index].name);
		ib_error_free(error);
	}
	for (index = 0U; index < sizeof(rejected) / sizeof(rejected[0]); index++) {
		char *error = NULL;
		int64_t raw = 0;
		int result = bind_scaled_string(rejected[index].text, rejected[index].scale,
			rejected[index].precision, &raw, &error);

		require_condition(result != 0 && error != NULL, rejected[index].name);
		ib_error_free(error);
	}
}

static void test_text_binding_converts_to_described_charset(void)
{
	ib_cursor cursor;
	ib_connection connection;
	ib_bindings *bindings;
	char *error;
	static const char win1250[] = "\xec\x9a\xe8";

	memset(&connection, 0, sizeof(connection));
	connection.charset = IB_CHARSET_UTF8;
	connection.dialect = SQL_DIALECT_V5;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.input = ib_alloc_sqlda(1);
	require_condition(cursor.input != NULL, "metadata input SQLDA allocation failed");
	cursor.input->sqld = 1;
	cursor.input->sqlvar[0].sqltype = SQL_VARYING | 1;
	cursor.input->sqlvar[0].sqlsubtype = 51; /* InterBase WIN1250 charset. */
	cursor.input->sqlvar[0].sqllen = 20;
	error = NULL;
	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"metadata binding allocation failed");
	require_success(ib_bindings_set_string(bindings, 0, "\xc4\x9b\xc5\xa1\xc4\x8d", 6U, &error),
		error, "metadata string binding setup failed");
	require_success(ib_bind_input(&cursor, bindings, &error), error,
		"metadata string binding failed");
	require_condition(cursor.input->sqlvar[0].sqltype == (SQL_VARYING | 1) &&
		cursor.input->sqlvar[0].sqlsubtype == 51 &&
		cursor.input->sqlvar[0].sqllen == 20,
		"text binding discarded described charset/type metadata or capacity");
	{
		unsigned short varying_length;
		memcpy(&varying_length, cursor.input->sqlvar[0].sqldata,
			sizeof(varying_length));
		require_condition(varying_length == sizeof(win1250) - 1U &&
			memcmp(cursor.input->sqlvar[0].sqldata + sizeof(varying_length),
				win1250, sizeof(win1250) - 1U) == 0,
			"text binding was not converted to the described charset");
	}
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
}

static void test_binding_converts_to_described_charset_capacity(void)
{
	static const char utf8[] =
		"ěščřžýáíéúůďťňóĚŠČŘŽÝÁÍÉÚŮĎŤŇÓ";
	static const char win1250[] =
		"\xec\x9a\xe8\xf8\x9e\xfd\xe1\xed\xe9\xfa\xf9\xef\x9d\xf2\xf3\xcc\x8a\xc8\xd8\x8e\xdd\xc1\xcd\xc9\xda\xd9\xcf\x8d\xd2\xd3";
	ib_cursor cursor;
	ib_connection connection;
	ib_bindings *bindings;
	unsigned short varying_length;
	char *error;

	memset(&connection, 0, sizeof(connection));
	connection.charset = IB_CHARSET_UTF8;
	connection.dialect = SQL_DIALECT_V5;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.input = ib_alloc_sqlda(1);
	require_condition(cursor.input != NULL, "UTF8 capacity input SQLDA allocation failed");
	cursor.input->sqld = 1;
	cursor.input->sqlvar[0].sqltype = SQL_VARYING | 1;
	cursor.input->sqlvar[0].sqlsubtype = 51; /* Server-described WIN1250 parameter. */
	cursor.input->sqlvar[0].sqllen = 40; /* Described WIN1250 character capacity. */
	error = NULL;
	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"UTF8 capacity binding allocation failed");
	require_success(ib_bindings_set_string(bindings, 0, utf8, sizeof(utf8) - 1U,
		&error), error, "UTF8 capacity string setup failed");
	require_success(ib_bind_input(&cursor, bindings, &error), error,
		"UTF8 capacity string binding failed");
	require_condition(cursor.input->sqlvar[0].sqltype == (SQL_VARYING | 1) &&
		cursor.input->sqlvar[0].sqlsubtype == 51 &&
		cursor.input->sqlvar[0].sqllen == 40,
		"UTF8 binding did not preserve the described byte capacity");
	memcpy(&varying_length, cursor.input->sqlvar[0].sqldata,
		sizeof(varying_length));
	require_condition(varying_length == sizeof(win1250) - 1U &&
		memcmp(cursor.input->sqlvar[0].sqldata + sizeof(varying_length), win1250,
			sizeof(win1250) - 1U) == 0,
		"UTF8 varying buffer is not in the described charset");
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
}

static void test_charset_conversion_handles_a_multibyte_chunk_boundary(void)
{
	const size_t split = 32768U;
	const size_t source_length = split + 2U;
	char *source;
	char *converted;
	size_t converted_length;
	char *error = NULL;

	source = (char *) malloc(source_length);
	require_condition(source != NULL, "chunked conversion input allocation failed");
	memset(source, 'a', source_length);
	source[split - 1U] = (char) 0xc4;
	source[split] = (char) 0x9b; /* U+011B, one WIN1250 byte. */
	source[split + 1U] = 'z';
	converted = ib_convert_utf8(source, source_length, 51, &converted_length, &error);
	require_condition(converted != NULL && error == NULL,
		"chunked UTF8/WIN1250 conversion failed at a multibyte boundary");
	require_condition(converted_length == split + 1U &&
		(unsigned char) converted[split - 1U] == 0xec && converted[split] == 'z',
		"chunked UTF8/WIN1250 conversion changed boundary data");
	free(converted);
	free(source);
	error = NULL;
	converted = ib_convert_utf8("x", (size_t) IB_MAX_BLOB_BUFFER + 1U, 51,
		&converted_length, &error);
	require_condition(converted == NULL && error != NULL,
		"oversized charset conversion was not rejected before allocation");
	ib_error_free(error);
}

static void test_configured_charset_conversion(void)
{
	static const char utf8[] = "\xc4\x9b\xc5\xa1\xc4\x8d";
	static const char win1250[] = "\xec\x9a\xe8";
	ib_connection connection;
	ib_cursor cursor;
	ib_bindings *bindings;
	ib_value_view view;
	unsigned short varying_length;
	char *error;

	memset(&connection, 0, sizeof(connection));
	connection.charset = 51; /* InterBase WIN1250. */
	connection.dialect = SQL_DIALECT_V5;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.input = ib_alloc_sqlda(1);
	require_condition(cursor.input != NULL, "charset input SQLDA allocation failed");
	cursor.input->sqld = 1;
	cursor.input->sqlvar[0].sqltype = SQL_VARYING | 1;
	cursor.input->sqlvar[0].sqlsubtype = 51;
	cursor.input->sqlvar[0].sqllen = 40;
	error = NULL;
	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"charset binding allocation failed");
	require_success(ib_bindings_set_string(bindings, 0, utf8, sizeof(utf8) - 1U,
		&error), error, "charset string setup failed");
	require_success(ib_bind_input(&cursor, bindings, &error), error,
		"charset input conversion failed");
	require_condition(cursor.input->sqlvar[0].sqltype == (SQL_VARYING | 1) &&
		cursor.input->sqlvar[0].sqlsubtype == 51 &&
		cursor.input->sqlvar[0].sqllen == 40,
		"charset input conversion changed the value or described capacity");
	{
		unsigned short varying_length;
		memcpy(&varying_length, cursor.input->sqlvar[0].sqldata,
			sizeof(varying_length));
		require_condition(varying_length == sizeof(win1250) - 1U &&
			memcmp(cursor.input->sqlvar[0].sqldata + sizeof(varying_length),
				win1250, sizeof(win1250) - 1U) == 0,
			"charset input conversion changed the value");
	}
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
	cursor.input = NULL;

	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "charset output SQLDA allocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_VARYING | 1;
	cursor.output->sqlvar[0].sqlsubtype = 51;
	cursor.output->sqlvar[0].sqllen = 40;
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error,
		"charset output allocation failed");
	varying_length = (unsigned short) (sizeof(win1250) - 1U);
	memcpy(cursor.output->sqlvar[0].sqldata, &varying_length,
		sizeof(varying_length));
	memcpy(cursor.output->sqlvar[0].sqldata + sizeof(varying_length),
		win1250, sizeof(win1250) - 1U);
	cursor.fetched = 1;
	error = NULL;
	require_success(ib_cursor_column(&cursor, 0, &view, &error), error,
		"charset output conversion failed");
	require_condition(view.kind == IB_VALUE_STRING && view.length == sizeof(utf8) - 1U &&
		memcmp(view.bytes, utf8, sizeof(utf8) - 1U) == 0,
		"charset output conversion changed the value");
	ib_cursor_free_parts(&cursor);
}

static void test_supported_charset_names(void)
{
	static const struct {
		const char *name;
		short id;
	} cases[] = {
		{"UTF8", 59},
		{"WIN1250", 51},
		{"WIN1252", 53},
		{"ISO8859_1", 21},
		{"ASCII", 2}
	};
	short charset;
	const char *name;
	char *error = NULL;
	 size_t index;

	for (index = 0U; index < sizeof(cases) / sizeof(cases[0]); index++) {
		charset = 0;
		require_condition(ib_charset_id(cases[index].name,
			strlen(cases[index].name), &charset) == 0 && charset == cases[index].id,
			"supported character set name was not mapped to its InterBase ID");
		name = ib_charset_name(charset);
		require_condition(name != NULL && strcmp(name, cases[index].name) == 0,
			"InterBase character set ID did not return its canonical name");
	}

	charset = 0;
	require_condition(ib_charset_id("NOPE", 4U, &charset) != 0,
		"unsupported character set name was accepted");
	ib_error_free(error);
}

static void test_output_nullability(void)
{
	ib_cursor cursor;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "nullable output SQLDA allocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_VARYING;
	cursor.output->sqlvar[0].sqllen = 40;
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error,
		"nullable output storage allocation failed");
	require_condition((cursor.output->sqlvar[0].sqltype & 1) != 0,
		"output SQLDA nullability was not enabled");
	require_condition(cursor.output->sqlvar[0].sqlind != NULL &&
		*cursor.output->sqlvar[0].sqlind == 0,
		"nullable output indicator was not initialized");
	ib_free_sqlda(cursor.output);
}

static void test_timestamp_and_rejections(void)
{
	ib_cursor cursor;
	ib_value_view view;
	struct tm encoded;
	ISC_TIMESTAMP timestamp;
	unsigned short invalid_length = 25;
	char *error;

	memset(&cursor, 0, sizeof(cursor));
	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "timestamp SQLDA allocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_TIMESTAMP | 1;
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error, "timestamp storage allocation failed");
	memset(&encoded, 0, sizeof(encoded));
	encoded.tm_year = 124;
	encoded.tm_mon = 6;
	encoded.tm_mday = 8;
	encoded.tm_hour = 9;
	encoded.tm_min = 10;
	encoded.tm_sec = 11;
	isc_encode_timestamp(&encoded, &timestamp);
	timestamp.timestamp_time += 1234;
	memcpy(cursor.output->sqlvar[0].sqldata, &timestamp, sizeof(timestamp));
	cursor.fetched = 1;
	require_success(ib_cursor_column(&cursor, 0, &view, &error), error, "timestamp decode failed");
	require_condition(view.kind == IB_VALUE_TIMESTAMP && view.year == 2024 && view.month == 7 &&
		view.day == 8 && view.hour == 9 && view.minute == 10 && view.second == 11 &&
		view.nanosecond == 123400000, "timestamp fractional ticks changed");
	ib_free_sqlda(cursor.output);

	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "rejection SQLDA allocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_BLOB | 1;
	error = NULL;
	require_success(ib_validate_output_types(cursor.output, &error), error,
		"BLOB output was rejected");
	ib_free_sqlda(cursor.output);
	cursor.output = ib_alloc_sqlda(1);
	require_condition(cursor.output != NULL, "rejection SQLDA reallocation failed");
	cursor.output->sqld = 1;
	cursor.output->sqlvar[0].sqltype = SQL_DOUBLE | 1;
	cursor.output->sqlvar[0].sqlscale = -1;
	error = NULL;
	require_success(ib_validate_output_types(cursor.output, &error), error,
		"scaled double output was rejected");
	cursor.output->sqlvar[0].sqltype = SQL_VARYING | 1;
	cursor.output->sqlvar[0].sqlscale = 0;
	cursor.output->sqlvar[0].sqllen = 6;
	error = NULL;
	require_success(ib_allocate_output(&cursor, &error), error, "varying storage allocation failed");
	memcpy(cursor.output->sqlvar[0].sqldata, &invalid_length, sizeof(invalid_length));
	cursor.fetched = 1;
	require_condition(ib_cursor_column(&cursor, 0, &view, &error) == -1 && error != NULL,
		"invalid VARCHAR prefix was accepted");
	ib_error_free(error);
	ib_free_sqlda(cursor.output);
}

static void test_array_binding_copies_shape_and_elements(void)
{
	static const char text[] = "array";
	ib_array_bound bounds[] = {
		{-1, 0},
		{2, 3}
	};
	ib_array_element elements[] = {
		{IB_ARRAY_ELEMENT_INT64, .int64_value = 10},
		{IB_ARRAY_ELEMENT_INT64, .int64_value = 11},
		{IB_ARRAY_ELEMENT_STRING, .bytes = text, .length = sizeof(text) - 1U},
		{IB_ARRAY_ELEMENT_BOOL, .bool_value = 1}
	};
	ib_bindings *bindings;
	char *error = NULL;

	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"array binding allocation failed");
	require_success(ib_bindings_set_array(bindings, 0, bounds, 2, elements, 4,
		&error), error, "array binding setup failed");
	require_condition(bindings->values[0].kind == IB_ARGUMENT_ARRAY &&
		bindings->values[0].array != NULL,
		"array binding did not store an array value");
	require_condition(bindings->values[0].array->dimensions == 2 &&
		bindings->values[0].array->element_count == 4,
		"array binding shape is wrong");
	require_condition(bindings->values[0].array->bounds[0].lower == -1 &&
		bindings->values[0].array->bounds[1].upper == 3,
		"array binding bounds are wrong");
	require_condition(bindings->values[0].array->elements[0].int64_value == 10 &&
		bindings->values[0].array->elements[3].bool_value != 0,
		"array binding scalar elements are wrong");
	require_condition(bindings->values[0].array->elements[2].length == sizeof(text) - 1U &&
		memcmp(bindings->values[0].array->elements[2].bytes, text, sizeof(text) - 1U) == 0,
		"array binding string element is wrong");

	bounds[0].lower = 99;
	elements[2].bytes = "changed";
	require_condition(bindings->values[0].array->bounds[0].lower == -1 &&
		memcmp(bindings->values[0].array->elements[2].bytes, text, sizeof(text) - 1U) == 0,
		"array binding aliases caller storage");
	ib_bindings_free(bindings);
}

static void test_null_array_is_rejected_without_array_support(void)
{
	ib_cursor cursor;
	ib_bindings *bindings;
	char *error = NULL;

	memset(&cursor, 0, sizeof(cursor));
	cursor.input = ib_alloc_sqlda(1);
	require_condition(cursor.input != NULL,
		"NULL array input SQLDA allocation failed");
	cursor.input->sqld = 1;
	cursor.input->sqlvar[0].sqltype = SQL_ARRAY;
	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"NULL array binding allocation failed");
	require_success(ib_bindings_set_null(bindings, 0, &error), error,
		"NULL array binding setup failed");
	error = NULL;
	require_condition(ib_bind_input_mode(&cursor, bindings, 0, &error) != 0 &&
		error != NULL, "database/sql accepted a NULL array parameter");
	require_condition(cursor.input->sqlvar[0].sqldata == NULL &&
		cursor.input->sqlvar[0].sqlind == NULL,
		"NULL array rejection allocated native binding storage");
	ib_error_free(error);
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
}

static void test_array_descriptor_lengths_are_validated_before_allocation(void)
{
	static const struct {
		unsigned char dtype;
		unsigned short length;
		const char *message;
	} invalid[] = {
		{blr_short, 1, "SMALLINT array descriptor accepted a short element"},
		{blr_long, 8, "INTEGER array descriptor accepted a wide element"},
		{blr_int64, 4, "BIGINT array descriptor accepted a short element"},
		{blr_float, 8, "FLOAT array descriptor accepted a wide element"},
		{blr_double, 4, "DOUBLE array descriptor accepted a short element"},
		{blr_timestamp, 4, "TIMESTAMP array descriptor accepted a short element"},
		{blr_sql_date, 8, "DATE array descriptor accepted a wide element"},
		{blr_sql_time, 8, "TIME array descriptor accepted a wide element"},
		{blr_boolean_dtype, 1, "BOOLEAN array descriptor accepted a short element"},
		{blr_text, 0, "CHAR array descriptor accepted zero storage"},
		{blr_varying, 0, "VARCHAR array descriptor accepted zero storage"},
		{255, sizeof(ISC_QUAD), "unsupported array descriptor type was accepted"},
	};
	ISC_ARRAY_DESC_V2 descriptor;
	size_t element_size;
	size_t element_count;
	size_t slice_size;
	char *error;
	size_t index;

	for (index = 0U; index < sizeof(invalid) / sizeof(invalid[0]); index++) {
		memset(&descriptor, 0, sizeof(descriptor));
		descriptor.array_desc_version = ARR_DESC_VERSION2;
		descriptor.array_desc_dtype = invalid[index].dtype;
		descriptor.array_desc_length = invalid[index].length;
		descriptor.array_desc_dimensions = 1;
		descriptor.array_desc_bounds[0].array_bound_lower = 0;
		descriptor.array_desc_bounds[0].array_bound_upper = 0;
		error = NULL;
		require_condition(ib_array_slice_layout(&descriptor, &element_size,
			&element_count, &slice_size, &error) != 0 && error != NULL,
			invalid[index].message);
		ib_error_free(error);
	}

	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_short;
	descriptor.array_desc_length = sizeof(short);
	descriptor.array_desc_dimensions = 1;
	descriptor.array_desc_bounds[0].array_bound_lower = -1;
	descriptor.array_desc_bounds[0].array_bound_upper = 1;
	error = NULL;
	require_success(ib_array_slice_layout(&descriptor, &element_size,
		&element_count, &slice_size, &error), error,
		"valid SMALLINT array descriptor was rejected");
	require_condition(element_size == sizeof(short) && element_count == 3U &&
		slice_size == 3U * sizeof(short), "valid array layout is wrong");

	{
		char source[4] = {'a', 'b', 'c', 'd'};
		char destination[sizeof(int64_t)];
		ib_array_element element;
		ib_value_view view;
		char *owned = NULL;

		memset(&descriptor, 0, sizeof(descriptor));
		descriptor.array_desc_version = ARR_DESC_VERSION2;
		descriptor.array_desc_dtype = blr_text;
		descriptor.array_desc_length = sizeof(source);
		error = NULL;
		require_condition(ib_array_decode_element(&descriptor, 0, source, 2U,
			&view, &owned, &error) != 0 && error != NULL,
			"array decoder accepted undersized character storage");
		ib_error_free(error);
		free(owned);

		memset(&descriptor, 0, sizeof(descriptor));
		descriptor.array_desc_version = ARR_DESC_VERSION2;
		descriptor.array_desc_dtype = blr_varying;
		descriptor.array_desc_length = sizeof(source);
		memset(&element, 0, sizeof(element));
		element.kind = IB_ARRAY_ELEMENT_BYTES;
		element.bytes = source;
		element.length = sizeof(source);
		error = NULL;
		require_success(ib_array_encode_element(&descriptor, 1, &element, destination,
			sizeof(source) + sizeof(unsigned short), 0, &error), error,
			"OCTETS varying array element was rejected");
		require_condition(memcmp(destination, "abcd\0\0", sizeof(source) + sizeof(unsigned short)) == 0,
			"OCTETS varying array element used a length prefix");
		memset(&element, 0, sizeof(element));
		element.kind = IB_ARRAY_ELEMENT_BYTES;
		element.bytes = "abcde";
		element.length = 5U;
		error = NULL;
		require_condition(ib_array_encode_element(&descriptor, 1, &element, destination,
			sizeof(source) + sizeof(unsigned short), 0, &error) != 0 && error != NULL,
			"OCTETS varying array element exceeded its declared payload capacity");
		ib_error_free(error);

		memset(&element, 0, sizeof(element));
		element.kind = IB_ARRAY_ELEMENT_STRING;
		element.bytes = "a";
		element.length = 1U;
		error = NULL;
		require_success(ib_array_encode_element(&descriptor, IB_CHARSET_UTF8, &element,
			destination, sizeof(source) + sizeof(unsigned short), 0, &error), error,
			"UTF8 varying array string was rejected");
		require_condition(memcmp(destination, "a\0\0\0\0\0", sizeof(source) + sizeof(unsigned short)) == 0,
			"UTF8 varying array string was padded with spaces");

		memcpy(destination, "abcd\0\0", sizeof(source) + sizeof(unsigned short));
		error = NULL;
		require_success(ib_array_decode_element(&descriptor, 1, destination,
			sizeof(source) + sizeof(unsigned short), &view, &owned, &error), error,
			"OCTETS varying array result was rejected");
		require_condition(view.kind == IB_VALUE_BYTES && view.length == sizeof(source) &&
			memcmp(view.bytes, source, sizeof(source)) == 0,
			"OCTETS varying array result changed its raw payload");
		free(owned);

		memset(&element, 0, sizeof(element));
		element.kind = IB_ARRAY_ELEMENT_INT64;
		element.int64_value = 1;
		descriptor.array_desc_dtype = blr_long;
		descriptor.array_desc_length = sizeof(ISC_LONG);
		error = NULL;
		require_condition(ib_array_encode_element(&descriptor, 0, &element, destination,
			2U, 0, &error) != 0 && error != NULL,
			"array encoder accepted undersized integer storage");
		ib_error_free(error);
	}
}

static void test_varying_array_rejects_embedded_nul_and_preserves_empty(void)
{
	ISC_ARRAY_DESC_V2 descriptor;
	ib_array_element element;
	ib_value_view view;
	char destination[sizeof("abcd\0\0") - 1U];
	const char embedded[] = {'a', '\0', 'b'};
	char *owned = NULL;
	char *error;

	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_varying;
	descriptor.array_desc_length = 4;

	memset(&element, 0, sizeof(element));
	element.kind = IB_ARRAY_ELEMENT_BYTES;
	element.bytes = embedded;
	element.length = sizeof(embedded);
	error = NULL;
	require_condition(ib_array_encode_element(&descriptor, 1, &element,
		destination, sizeof(destination), 0, &error) != 0 && error != NULL,
		"varying OCTETS array accepted an embedded NUL");
	ib_error_free(error);

	memset(&element, 0, sizeof(element));
	element.kind = IB_ARRAY_ELEMENT_STRING;
	element.bytes = embedded;
	element.length = sizeof(embedded);
	error = NULL;
	require_condition(ib_array_encode_element(&descriptor, IB_CHARSET_UTF8, &element,
		destination, sizeof(destination), 0, &error) != 0 && error != NULL,
		"varying text array accepted an embedded NUL");
	ib_error_free(error);

	memset(&element, 0, sizeof(element));
	element.kind = IB_ARRAY_ELEMENT_BYTES;
	element.bytes = NULL;
	element.length = 0;
	error = NULL;
	require_success(ib_array_encode_element(&descriptor, 1, &element,
		destination, sizeof(destination), 0, &error), error,
		"empty varying OCTETS array element was rejected");
	require_condition(memcmp(destination, "\0\0\0\0\0\0", sizeof(destination)) == 0,
		"empty varying OCTETS array element was not zero-filled");
	error = NULL;
	require_success(ib_array_decode_element(&descriptor, 1, destination,
		sizeof(destination), &view, &owned, &error), error,
		"empty varying OCTETS array result was rejected");
	require_condition(view.kind == IB_VALUE_BYTES && view.length == 0,
		"empty varying OCTETS array result was not preserved");
	free(owned);

	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_text;
	descriptor.array_desc_length = 4;
	element.kind = IB_ARRAY_ELEMENT_BYTES;
	element.bytes = embedded;
	element.length = sizeof(embedded);
	error = NULL;
	require_success(ib_array_encode_element(&descriptor, 1, &element,
		destination, 4U, 0, &error), error,
		"fixed-width OCTETS array element with an embedded NUL was rejected");
	require_condition(memcmp(destination, "a\0b\0", 4U) == 0,
		"fixed-width OCTETS array element changed its zero bytes");
}

static void test_utf8_array_character_capacity_preserves_non_ascii_values(void)
{
	ISC_ARRAY_DESC_V2 descriptor;
	char *data;
	ib_value_view view;
	char *owned = NULL;
	char *error = NULL;
	const size_t length = 32768U;

	data = (char *) malloc(length);
	require_condition(data != NULL, "UTF8 array result allocation failed");
	memset(data, ' ', length);
	data[0] = (char) 0xc3;
	data[1] = (char) 0xa9;
	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_text;
	descriptor.array_desc_subtype = IB_CHARSET_UTF8;
	descriptor.array_desc_length = (unsigned short) length;
	require_success(ib_array_decode_element(&descriptor, IB_CHARSET_UTF8, data, length, &view,
		&owned, &error), error,
		"UTF8 array character capacity rejected a non-ASCII value");
	require_condition(view.kind == IB_VALUE_STRING && view.length == 8193U &&
		memcmp(view.bytes, "\xc3\xa9", 2U) == 0 && view.bytes[2] == ' ',
		"UTF8 array character capacity truncated a non-ASCII value");
	free(owned);
	free(data);
}

static void test_scaled_array_elements_keep_exact_integer_precision(void)
{
	static const struct {
		unsigned char dtype;
		unsigned short length;
		char scale;
		const char *value;
		int64_t want;
		int precision;
		const char *message;
	} accepted[] = {
		{blr_int64, sizeof(int64_t), -4, "12345678901234.5678",
			INT64_C(123456789012345678), 18, "wide scaled array value changed"},
		{blr_int64, sizeof(int64_t), -1, "-922337203685477580.8",
			INT64_MIN, 19, "minimum scaled array value changed"},
		{blr_long, sizeof(ISC_LONG), -2, "21474836.47",
			INT32_MAX, 10, "INTEGER scaled array value changed"},
	};
	static const char *const rejected[] = {
		"922337203685477580.9",
		"922337203685477580.8",
		"1.001",
	};
	ISC_ARRAY_DESC_V2 descriptor;
	ib_array_element element;
	char destination[sizeof(int64_t)];
	int64_t value;
	char *error;
	size_t index;

	for (index = 0U; index < sizeof(accepted) / sizeof(accepted[0]); index++) {
		memset(&descriptor, 0, sizeof(descriptor));
		descriptor.array_desc_version = ARR_DESC_VERSION2;
		descriptor.array_desc_dtype = accepted[index].dtype;
		descriptor.array_desc_length = accepted[index].length;
		descriptor.array_desc_scale = accepted[index].scale;
		descriptor.array_desc_subtype = 2;
		memset(&element, 0, sizeof(element));
		element.kind = IB_ARRAY_ELEMENT_STRING;
		element.bytes = accepted[index].value;
		element.length = strlen(accepted[index].value);
		error = NULL;
		require_success(ib_array_encode_element(&descriptor, 0, &element, destination,
			accepted[index].length, accepted[index].precision, &error), error,
			accepted[index].message);
		value = 0;
		memcpy(&value, destination, accepted[index].length);
		require_condition(value == accepted[index].want, accepted[index].message);
	}

	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_int64;
	descriptor.array_desc_length = sizeof(int64_t);
		descriptor.array_desc_scale = -1;
		descriptor.array_desc_subtype = 2;
	for (index = 0U; index < sizeof(rejected) / sizeof(rejected[0]); index++) {
		memset(&element, 0, sizeof(element));
		element.kind = IB_ARRAY_ELEMENT_STRING;
		element.bytes = rejected[index];
		element.length = strlen(rejected[index]);
		error = NULL;
		require_condition(ib_array_encode_element(&descriptor, 0, &element, destination,
			sizeof(int64_t), 18, &error) != 0 && error != NULL,
		"out-of-range scaled array value was accepted");
		ib_error_free(error);
	}
}

static void test_scaled_array_integer_elements_apply_scale_and_precision(void)
{
	ISC_ARRAY_DESC_V2 descriptor;
	ib_array_element element;
	char destination[sizeof(int64_t)];
	int64_t value;
	char *error = NULL;

	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_int64;
	descriptor.array_desc_length = sizeof(int64_t);
	descriptor.array_desc_scale = -2;
	descriptor.array_desc_subtype = 1;
	memset(&element, 0, sizeof(element));
	element.kind = IB_ARRAY_ELEMENT_INT64;
	element.int64_value = 12;
	require_success(ib_array_encode_element(&descriptor, 0, &element, destination,
		sizeof(destination), 4, &error), error,
		"scaled integer array element was rejected");
	memcpy(&value, destination, sizeof(value));
	require_condition(value == 1200,
		"scaled integer array element did not apply its decimal scale");

	element.int64_value = 100;
	error = NULL;
	require_condition(ib_array_encode_element(&descriptor, 0, &element, destination,
		sizeof(destination), 4, &error) != 0 && error != NULL,
		"scaled integer array element exceeded declared precision");
	ib_error_free(error);
}

static void test_utf8_array_encoding_enforces_character_capacity(void)
{
	static const char *const overlong[] = {"12345", "ééééé"};
	ISC_ARRAY_DESC_V2 descriptor;
	ib_array_element element;
	char destination[16];
	char *error;
	size_t index;

	memset(&descriptor, 0, sizeof(descriptor));
	descriptor.array_desc_version = ARR_DESC_VERSION2;
	descriptor.array_desc_dtype = blr_text;
	descriptor.array_desc_subtype = IB_CHARSET_UTF8;
	descriptor.array_desc_length = sizeof(destination);
	memset(&element, 0, sizeof(element));
	element.kind = IB_ARRAY_ELEMENT_STRING;
	for (index = 0U; index < sizeof(overlong) / sizeof(overlong[0]); index++) {
		element.bytes = overlong[index];
		element.length = strlen(overlong[index]);
		error = NULL;
		require_condition(ib_array_encode_element(&descriptor, IB_CHARSET_UTF8, &element, destination,
			sizeof(destination), 0, &error) != 0 && error != NULL,
			"UTF8 array encoder accepted a value beyond character capacity");
		ib_error_free(error);
	}

	element.bytes = "éééé";
	element.length = strlen(element.bytes);
	error = NULL;
	require_success(ib_array_encode_element(&descriptor, IB_CHARSET_UTF8, &element, destination,
		sizeof(destination), 0, &error), error,
		"UTF8 array encoder rejected a value within character capacity");
}

static int bind_temporal_for_type(short type, int64_t year, char **error)
{
	ib_cursor cursor;
	ib_bindings *bindings;
	int result;

	memset(&cursor, 0, sizeof(cursor));
	cursor.input = ib_alloc_sqlda(1);
	if (cursor.input == NULL) {
		return -1;
	}
	cursor.input->sqld = 1;
	cursor.input->sqlvar[0].sqltype = (short) (type | 1);
	bindings = ib_bindings_new(1, error);
	if (bindings == NULL) {
		ib_free_sqlda(cursor.input);
		return -1;
	}
	result = ib_bindings_set_timestamp(bindings, 0, year, 1, 1, 23, 59, 59, 0,
		error);
	if (result == 0) {
		result = ib_bind_input(&cursor, bindings, error);
	}
	ib_bindings_free(bindings);
	ib_free_sqlda(cursor.input);
	return result;
}

static void test_temporal_binding_target_validation(void)
{
	ib_bindings *bindings;
	char *error = NULL;
	int result;

	result = bind_temporal_for_type(SQL_TYPE_TIME, 0, &error);
	require_condition(result == 0 && error == NULL,
		"year-zero TIME binding was rejected");
	ib_error_free(error);

	error = NULL;
	result = bind_temporal_for_type(SQL_TYPE_TIME, INT64_C(3000000000), &error);
	require_condition(result == 0 && error == NULL,
		"large-year TIME binding was rejected or truncated");
	ib_error_free(error);

	error = NULL;
	bindings = ib_bindings_new(1, &error);
	require_condition(bindings != NULL && error == NULL,
		"large-year timestamp binding allocation failed");
	if (bindings != NULL) {
		result = ib_bindings_set_timestamp(bindings, 0, INT64_C(3000000000),
			1, 1, 23, 59, 59, 0, &error);
		require_condition(result == 0 && error == NULL,
			"large-year timestamp binding setup failed");
		require_condition(bindings->values[0].year == INT64_C(3000000000),
			"large-year timestamp binding was narrowed before native validation");
	}
	ib_error_free(error);
	ib_bindings_free(bindings);

	error = NULL;
	result = bind_temporal_for_type(SQL_TYPE_DATE, 0, &error);
	require_condition(result != 0 && error != NULL,
		"year-zero DATE binding was accepted");
	ib_error_free(error);

	error = NULL;
	result = bind_temporal_for_type(SQL_TIMESTAMP, 0, &error);
	require_condition(result != 0 && error != NULL,
		"year-zero TIMESTAMP binding was accepted");
	ib_error_free(error);
}

static void test_fixed_char_expression_attribution_is_conservative(void)
{
	const char *partial_expression =
		"SELECT CAST('x' AS CHAR(3)) || 'abcdefghijkl' FROM RDB$DATABASE";
	const char *union_expression =
		"SELECT CAST('x' AS CHAR(3)) FROM RDB$DATABASE "
		"UNION SELECT CAST('y' AS CHAR(3)) FROM RDB$DATABASE";
	const char *star_expression =
		"SELECT CAST('x' AS CHAR(3)), D.* FROM T D";
	const char *valid_expression =
		"SELECT CAST('x' AS CHAR(3)) FROM RDB$DATABASE";
	const char *quoted_alias_expression =
		"SELECT CAST('x' AS CHAR(3)) AS \"R\"\"ESULT\" FROM RDB$DATABASE";
	const char *invalid_length_expression =
		"SELECT CAST('x' AS CHAR(0)) FROM RDB$DATABASE";
	XSQLVAR variable;
	size_t fixed[1];

	memset(fixed, 0, sizeof(fixed));
	ib_sql_describe_fixed_text_columns(partial_expression,
		strlen(partial_expression), fixed, 1U);
	require_condition(fixed[0] == 0,
		"partial fixed CHAR expression was attributed");

	memset(fixed, 0, sizeof(fixed));
	ib_sql_describe_fixed_text_columns(union_expression,
		strlen(union_expression), fixed, 1U);
	require_condition(fixed[0] == 0,
		"UNION fixed CHAR expression was attributed");

	memset(fixed, 0, sizeof(fixed));
	ib_sql_describe_fixed_text_columns(star_expression,
		strlen(star_expression), fixed, 1U);
	require_condition(fixed[0] == 0,
		"star-expanded SELECT list attributed a fixed CHAR expression");

	memset(fixed, 0, sizeof(fixed));
	ib_sql_describe_fixed_text_columns(valid_expression,
		strlen(valid_expression), fixed, 1U);
	require_condition(fixed[0] == 3U,
		"valid fixed CHAR expression did not retain its declared length");

	memset(fixed, 0, sizeof(fixed));
	ib_sql_describe_fixed_text_columns(quoted_alias_expression,
		strlen(quoted_alias_expression), fixed, 1U);
	require_condition(fixed[0] == 3U,
		"quoted alias prevented fixed CHAR attribution");

	memset(fixed, 0, sizeof(fixed));
	ib_sql_describe_fixed_text_columns(invalid_length_expression,
		strlen(invalid_length_expression), fixed, 1U);
	require_condition(fixed[0] == 0U,
		"invalid fixed CHAR declaration was attributed");

	memset(&variable, 0, sizeof(variable));
	variable.sqltype = SQL_TEXT;
	variable.sqlsubtype = IB_CHARSET_UTF8;
	variable.sqllen = 12;
	require_condition(ib_sql_fixed_text_descriptor_compatible(&variable, 3U),
		"compatible UTF8 fixed CHAR descriptor was rejected");
	variable.sqllen = 8;
	require_condition(!ib_sql_fixed_text_descriptor_compatible(&variable, 3U),
		"short UTF8 fixed CHAR descriptor was accepted");
	variable.sqltype = SQL_VARYING;
	require_condition(!ib_sql_fixed_text_descriptor_compatible(&variable, 3U),
		"varying descriptor was accepted as fixed CHAR");
}

int main(void)
{
	test_positional_bind_values();
	test_blob_reference_binding();
	test_text_blob_reference_without_relation_is_rejected();
	test_binary_blob_reference_without_relation_is_preserved();
	test_column_decoding();
	test_utf8_fixed_text_and_scaled_float_decoding();
	test_unattributed_utf8_text_preserves_literal_spaces();
	test_fixed_text_output_is_space_initialized();
	test_octets_bind_and_decode_as_bytes();
	test_exact_scaled_integer_binding();
	test_temporal_binding_target_validation();
	test_text_binding_converts_to_described_charset();
	test_binding_converts_to_described_charset_capacity();
	test_charset_conversion_handles_a_multibyte_chunk_boundary();
	test_configured_charset_conversion();
	test_supported_charset_names();
	test_output_nullability();
	test_timestamp_and_rejections();
	test_array_binding_copies_shape_and_elements();
	test_null_array_is_rejected_without_array_support();
	test_array_descriptor_lengths_are_validated_before_allocation();
	test_varying_array_rejects_embedded_nul_and_preserves_empty();
	test_utf8_array_character_capacity_preserves_non_ascii_values();
	test_scaled_array_elements_keep_exact_integer_precision();
	test_scaled_array_integer_elements_apply_scale_and_precision();
	test_utf8_array_encoding_enforces_character_capacity();
	test_fixed_char_expression_attribution_is_conservative();
	(void) puts("native values tests passed");
	return EXIT_SUCCESS;
}
