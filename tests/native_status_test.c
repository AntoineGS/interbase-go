#include <limits.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include "../native.c"

static void require_condition(int condition, const char *message)
{
	if (!condition) {
		(void) fprintf(stderr, "native status test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static void test_long_interpreted_status_is_not_rendered(void)
{
	enum { LONG_MESSAGE_LENGTH = 1024 };
	char interpreted[LONG_MESSAGE_LENGTH + 1U];
	char pointer_text[64];
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	int result;

	memset(interpreted, 'L', sizeof(interpreted) - 1U);
	interpreted[sizeof(interpreted) - 1U] = '\0';
	memset(status, 0, sizeof(status));
	status[0] = isc_arg_interpreted;
	status[1] = (ISC_STATUS) (uintptr_t) interpreted;
	status[2] = isc_arg_end;
	error = NULL;

	result = ib_fail_status(&error, "synthetic long operation", status);
	require_condition(result == -1, "long interpreted status did not fail");
	require_condition(error != NULL, "long interpreted status produced no error");
	require_condition(strstr(error, "synthetic long operation failed") != NULL,
		"operation name is missing from long-status error");
	require_condition(strstr(error, interpreted) == NULL,
		"long interpreted status was rendered");
	(void) snprintf(pointer_text, sizeof(pointer_text), "%" PRIxPTR,
		(uintptr_t) interpreted);
	require_condition(strstr(error, pointer_text) == NULL,
		"interpreted status pointer was rendered");
	ib_error_free(error);
}

static void test_sqlcode_status_keeps_numeric_context(void)
{
	char *error;
	char generic_message[256];
	char expected_sqlcode[64];
	char expected_status[64];
	ISC_LONG sqlcode;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	int result;

	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_dsql_error;
	status[2] = isc_arg_end;
	sqlcode = isc_sqlcode(status);
	require_condition(sqlcode >= (ISC_LONG) SHRT_MIN &&
		sqlcode <= (ISC_LONG) SHRT_MAX,
		"synthetic SQLCODE does not fit the SDK formatter");
	memset(generic_message, 0, sizeof(generic_message));
	isc_sql_interprete((short) sqlcode, generic_message,
		(short) sizeof(generic_message));
	generic_message[sizeof(generic_message) - 1U] = '\0';

	error = NULL;
	result = ib_fail_status(&error, "synthetic SQL operation", status);
	require_condition(result == -1, "SQLCODE status did not fail");
	require_condition(error != NULL, "SQLCODE status produced no error");
	(void) snprintf(expected_sqlcode, sizeof(expected_sqlcode),
		"SQLCODE %" PRIdMAX, (intmax_t) sqlcode);
	(void) snprintf(expected_status, sizeof(expected_status),
		"status %" PRIdPTR, (intptr_t) status[1]);
	require_condition(strstr(error, expected_sqlcode) != NULL,
		"SQLCODE is missing from SQLCODE status error");
	require_condition(strstr(error, expected_status) != NULL,
		"native primary status is missing from SQLCODE status error");
	if (generic_message[0] != '\0') {
		require_condition(strstr(error, generic_message) != NULL,
			"bounded generic SQLCODE message is missing");
	}
	ib_error_free(error);
}

int main(void)
{
	test_long_interpreted_status_is_not_rendered();
	test_sqlcode_status_keeps_numeric_context();
	(void) puts("native status tests passed");
	return EXIT_SUCCESS;
}
