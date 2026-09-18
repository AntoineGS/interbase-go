#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <ibase.h>

static int set_cursor_name_calls;
static unsigned short set_cursor_name_reserved;
static char set_cursor_name_value[128];

ISC_STATUS ISC_EXPORT test_dsql_set_cursor_name(ISC_STATUS *status,
	isc_stmt_handle *statement, char *name, unsigned short reserved)
{
	(void) statement;
	set_cursor_name_calls++;
	set_cursor_name_reserved = reserved;
	if (name != NULL) {
		(void) snprintf(set_cursor_name_value, sizeof(set_cursor_name_value), "%s", name);
	} else {
		set_cursor_name_value[0] = '\0';
	}
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

#define isc_dsql_set_cursor_name test_dsql_set_cursor_name
#include "../native.c"
#undef isc_dsql_set_cursor_name

static void check(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native direct test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void test_indicator_flags(void)
{
	ib_cursor cursor;
	XSQLDA *output;
	short indicator;
	uint16_t got;
	char *error = NULL;

	memset(&cursor, 0, sizeof(cursor));
	output = (XSQLDA *) calloc(1U, XSQLDA_LENGTH(1));
	check(output != NULL, "output allocation failed");
	output->sqln = 1;
	output->sqld = 1;
	output->sqlvar[0].sqlind = &indicator;
	cursor.output = output;

	indicator = (short) -1;
	check(ib_cursor_column_indicator(&cursor, 0U, &got, &error) == 0,
		"plain NULL indicator read failed");
	check(got == (uint16_t) SQLIND_NULL, "plain NULL indicator was not normalized");
	ib_error_free(error);
	error = NULL;

	indicator = (short) (SQLIND_NULL | SQLIND_UPDATE);
	check(ib_cursor_column_indicator(&cursor, 0U, &got, &error) == 0,
		"NULL/update indicator read failed");
	check(got == ((uint16_t) SQLIND_NULL | (uint16_t) SQLIND_UPDATE),
		"NULL/update indicator lost its change flag");
	ib_error_free(error);
	error = NULL;

	indicator = (short) (SQLIND_INSERT | SQLIND_UPDATE);
	check(ib_cursor_column_indicator(&cursor, 0U, &got, &error) == 0,
		"change indicator read failed");
	check(got == (uint16_t) (SQLIND_INSERT | SQLIND_UPDATE),
		"change indicator bits were not preserved");
	ib_error_free(error);
	free(output);
}

static void test_cursor_name_reserved_argument(void)
{
	ib_connection connection;
	ib_cursor cursor;
	char database_token;
	char statement_token;
	char *error = NULL;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.charset = IB_CHARSET_UTF8;
	memset(&cursor, 0, sizeof(cursor));
	cursor.connection = &connection;
	cursor.statement = &statement_token;
	set_cursor_name_calls = 0;
	set_cursor_name_reserved = 99U;
	set_cursor_name_value[0] = '\0';

	check(ib_cursor_set_name(&cursor, "direct_cursor", strlen("direct_cursor"), &error) == 0,
		"cursor name call failed");
	check(error == NULL, "cursor name call returned an unexpected error");
	check(set_cursor_name_calls == 1, "cursor name native call count was wrong");
	check(set_cursor_name_reserved == 0U, "cursor name reserved argument was not zero");
	check(strcmp(set_cursor_name_value, "direct_cursor") == 0,
		"cursor name was not passed to the native call");
}

int main(void)
{
	test_indicator_flags();
	test_cursor_name_reserved_argument();
	(void) puts("native direct tests passed");
	return EXIT_SUCCESS;
}
