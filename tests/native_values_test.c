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

static void test_text_binding_converts_to_described_charset(void)
{
	ib_cursor cursor;
	ib_connection connection;
	ib_bindings *bindings;
	char *error;
	static const char win1250[] = "\xec\x9a\xe8";

	memset(&connection, 0, sizeof(connection));
	connection.charset = IB_CHARSET_UTF8;
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

int main(void)
{
	test_positional_bind_values();
	test_column_decoding();
	test_utf8_fixed_text_and_scaled_float_decoding();
	test_fixed_text_output_is_space_initialized();
	test_text_binding_converts_to_described_charset();
	test_binding_converts_to_described_charset_capacity();
	test_configured_charset_conversion();
	test_output_nullability();
	test_timestamp_and_rejections();
	(void) puts("native values tests passed");
	return EXIT_SUCCESS;
}
