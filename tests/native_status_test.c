#include <limits.h>
#include <inttypes.h>
#include <stdint.h>
#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#include <ibase.h>

static char attach_database_token;
static unsigned char captured_dpb[256];
static unsigned short captured_dpb_length;

static ISC_STATUS test_attach_database(ISC_STATUS *status, short database_length,
	char *database, isc_db_handle *database_handle, short dpb_length,
	char *dpb)
{
	(void) database_length;
	(void) database;
	if (dpb_length < 0 || (size_t) dpb_length > sizeof(captured_dpb)) {
		status[0] = isc_arg_gds;
		status[1] = isc_network_error;
		status[2] = isc_arg_end;
		return status[1];
	}
	captured_dpb_length = (unsigned short) dpb_length;
	memcpy(captured_dpb, dpb, (size_t) dpb_length);
	*database_handle = &attach_database_token;
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

#define isc_attach_database test_attach_database
#include "../native.c"
#undef isc_attach_database

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

static void test_dialect_is_encoded_in_dpb(void)
{
	ib_connection *connection;
	char *error = NULL;
	size_t offset;
	int dialect_seen;

	memset(captured_dpb, 0, sizeof(captured_dpb));
	captured_dpb_length = 0;
	connection = ib_connection_open("database", strlen("database"),
		"SYSDBA", strlen("SYSDBA"), "masterkey", strlen("masterkey"),
		"UTF8", strlen("UTF8"), SQL_DIALECT_V6, &error);
	require_condition(connection != NULL && error == NULL,
		"Dialect 3 connection open failed");
	if (connection == NULL) {
		ib_error_free(error);
		return;
	}
	require_condition(connection->dialect == SQL_DIALECT_V6,
		"Dialect 3 was not retained in native connection state");
	dialect_seen = 0;
	require_condition(captured_dpb_length > 0U &&
		captured_dpb[0] == isc_dpb_version1,
		"captured DPB has the wrong version marker");
	offset = 1U;
	while (offset < (size_t) captured_dpb_length) {
		unsigned char tag;
		unsigned char length;

		require_condition((size_t) captured_dpb_length - offset >= 2U,
			"captured DPB item header is truncated");
		if ((size_t) captured_dpb_length - offset < 2U) {
			break;
		}
		tag = captured_dpb[offset++];
		length = captured_dpb[offset++];
		require_condition((size_t) captured_dpb_length - offset >= length,
			"captured DPB item is truncated");
		if ((size_t) captured_dpb_length - offset < length) {
			break;
		}
		if (tag == isc_dpb_sql_dialect && length == 1U) {
			dialect_seen++;
			require_condition(captured_dpb[offset] == SQL_DIALECT_V6,
				"captured DPB selected the wrong SQL dialect");
		}
		offset += length;
	}
	require_condition(dialect_seen == 1, "captured DPB has no unique SQL dialect item");
	connection->database = NULL;
	free(connection);
}

static void test_unset_connection_defaults_to_dialect_three(void)
{
	ib_connection connection = {0};

	require_condition(ib_connection_dialect(&connection) == SQL_DIALECT_V6,
		"unset native connection did not use the Dialect 3 default");
	require_condition(ib_connection_dialect(NULL) == SQL_DIALECT_V6,
		"missing native connection did not use the Dialect 3 default");
}

int main(void)
{
	test_long_interpreted_status_is_not_rendered();
	test_sqlcode_status_keeps_numeric_context();
	test_unset_connection_defaults_to_dialect_three();
	test_dialect_is_encoded_in_dpb();
	(void) puts("native status tests passed");
	return EXIT_SUCCESS;
}
