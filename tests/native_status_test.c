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

static void test_long_interpreted_status_is_bounded(void)
{
	enum { LONG_MESSAGE_LENGTH = 8192 };
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
	require_condition(strstr(error, "LLLLLLLL") != NULL,
		"long interpreted status text is missing");
	require_condition(strstr(error, interpreted) == NULL,
		"long interpreted status was rendered unbounded");
	require_condition(strstr(error, "...") != NULL,
		"truncated status text has no marker");
	require_condition(strlen(error) < IB_STATUS_TEXT_LIMIT + 256U,
		"status error exceeds its bound");
	(void) snprintf(pointer_text, sizeof(pointer_text), "%" PRIxPTR,
		(uintptr_t) interpreted);
	require_condition(strstr(error, pointer_text) == NULL,
		"interpreted status pointer was rendered");
	ib_error_free(error);
}

static void test_status_vector_text_names_the_failure(void)
{
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	int result;

	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_dsql_error;
	status[2] = isc_arg_gds;
	status[3] = (ISC_STATUS) isc_random;
	status[4] = isc_arg_string;
	status[5] = (ISC_STATUS) (uintptr_t) "NO_SUCH_COLUMN";
	status[6] = isc_arg_end;
	error = NULL;

	result = ib_fail_status(&error, "prepare statement", status);
	require_condition(result == -1, "DSQL status did not fail");
	require_condition(error != NULL, "DSQL status produced no error");
	require_condition(strstr(error, "SQLCODE") != NULL,
		"SQLCODE envelope is missing");
	require_condition(strstr(error, "\nNO_SUCH_COLUMN") != NULL,
		"status-vector string argument is missing from its own line");
	ib_error_free(error);
}

static void test_long_string_argument_is_bounded(void)
{
	enum { LONG_ARGUMENT_LENGTH = 16384 };
	char *argument;
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];

	argument = malloc(LONG_ARGUMENT_LENGTH + 1U);
	require_condition(argument != NULL, "argument allocation failed");
	memset(argument, 'A', LONG_ARGUMENT_LENGTH);
	argument[LONG_ARGUMENT_LENGTH] = '\0';
	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_random;
	status[2] = isc_arg_string;
	status[3] = (ISC_STATUS) (uintptr_t) argument;
	status[4] = isc_arg_gds;
	status[5] = (ISC_STATUS) isc_random;
	status[6] = isc_arg_cstring;
	status[7] = (ISC_STATUS) 4;
	status[8] = (ISC_STATUS) (uintptr_t) "TAILxxxxxxxx";
	status[9] = isc_arg_end;
	error = NULL;

	(void) ib_fail_status(&error, "synthetic long argument", status);
	require_condition(error != NULL, "long argument produced no error");
	require_condition(strstr(error, "AAAAAAAA") != NULL,
		"long string argument text is missing");
	require_condition(strlen(error) < IB_STATUS_TEXT_LIMIT + 256U,
		"long argument error exceeds its bound");
	require_condition(strstr(error, "AAA...\n") != NULL,
		"clamped argument has no marker");
	require_condition(strstr(error, "\nTAIL") != NULL &&
		strstr(error, "TAILx") == NULL,
		"counted string argument was not honoured");
	ib_error_free(error);
	free(argument);
}

static void test_many_long_arguments_share_one_budget(void)
{
	enum { LONG_ARGUMENT_LENGTH = 4096 };
	char *argument;
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	size_t index;

	argument = malloc(LONG_ARGUMENT_LENGTH + 1U);
	require_condition(argument != NULL, "argument allocation failed");
	memset(argument, 'B', LONG_ARGUMENT_LENGTH);
	argument[LONG_ARGUMENT_LENGTH] = '\0';
	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_random;
	for (index = 2U; index + 2U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_string;
		status[index + 1U] = (ISC_STATUS) (uintptr_t) argument;
	}
	status[index] = isc_arg_end;
	error = NULL;

	(void) ib_fail_status(&error, "synthetic many arguments", status);
	require_condition(error != NULL && strstr(error, "BBB...") != NULL,
		"clamped arguments were not rendered");
	require_condition(strlen(error) < IB_STATUS_TEXT_LIMIT + 256U,
		"many-argument error exceeds its bound");
	ib_error_free(error);
	free(argument);
}

static void test_missing_template_arguments_are_padded(void)
{
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];

	memset(status, 0x5a, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_network_error;
	status[2] = isc_arg_gds;
	status[3] = (ISC_STATUS) isc_random;
	status[4] = isc_arg_end;
	error = NULL;

	(void) ib_fail_status(&error, "synthetic missing arguments", status);
	require_condition(error != NULL && strstr(error, "SQLCODE") != NULL,
		"vector without template arguments produced no error");
	ib_error_free(error);
}

static void test_malformed_arguments_are_safe(void)
{
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	ISC_STATUS copy[IB_STATUS_COPY_LENGTH];
	char storage[IB_STATUS_ARGUMENTS_LIMIT + IB_STATUS_VECTOR_LENGTH];
	char *error;
	size_t index;

	/* Ten argument-less codes expand the copy to its full length. */
	memset(status, 0, sizeof(status));
	for (index = 0U; index + 2U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_gds;
		status[index + 1U] = (ISC_STATUS) isc_random;
	}
	status[index] = isc_arg_end;
	require_condition(ib_status_clamped_copy(status, copy, storage),
		"padded vector was rejected");
	error = NULL;
	(void) ib_fail_status(&error, "synthetic padded", status);
	require_condition(error != NULL, "padded vector produced no error");
	ib_error_free(error);

	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_random;
	status[2] = isc_arg_cstring;
	status[3] = (ISC_STATUS) -1;
	status[4] = (ISC_STATUS) (uintptr_t) "x";
	status[5] = isc_arg_end;
	require_condition(!ib_status_clamped_copy(status, copy, storage),
		"negative counted length was accepted");

	status[2] = isc_arg_string;
	status[3] = (ISC_STATUS) (uintptr_t) NULL;
	status[4] = isc_arg_end;
	error = NULL;
	(void) ib_fail_status(&error, "synthetic null argument", status);
	require_condition(error != NULL, "NULL string argument produced no error");
	ib_error_free(error);
}

static void test_unterminated_status_vector_is_not_interpreted(void)
{
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	size_t index;

	for (index = 0U; index + 1U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_gds;
		status[index + 1U] = (ISC_STATUS) isc_dsql_error;
	}
	{
		ISC_STATUS copy[IB_STATUS_COPY_LENGTH];
		char storage[IB_STATUS_ARGUMENTS_LIMIT + IB_STATUS_VECTOR_LENGTH];

		require_condition(!ib_status_clamped_copy(status, copy, storage),
			"unterminated vector was accepted for interpretation");
	}
	error = NULL;
	(void) ib_fail_status(&error, "synthetic unterminated", status);
	require_condition(error != NULL && strstr(error, "SQLCODE") != NULL,
		"unterminated vector lost its numeric context");
	{
		char generic_message[IB_SQL_MESSAGE_LENGTH];
		ISC_LONG sqlcode = isc_sqlcode(status);

		generic_message[0] = '\0';
		isc_sql_interprete((short) sqlcode, generic_message,
			(short) sizeof(generic_message));
		generic_message[sizeof(generic_message) - 1U] = '\0';
		require_condition(generic_message[0] == '\0' ||
			strstr(error, generic_message) != NULL,
			"generic SQLCODE fallback message is missing");
	}
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
	require_condition(strstr(error, "): ") != NULL &&
		strstr(error, "): ")[3] != '\0',
		"interpreted status text is missing");
	(void) generic_message;
	ib_error_free(error);
}

static void test_status_codes_report_gds_and_exception_number(void)
{
	char *error;
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];

	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_except;
	status[2] = isc_arg_number;
	status[3] = (ISC_STATUS) 42;
	status[4] = isc_arg_gds;
	status[5] = (ISC_STATUS) isc_random;
	status[6] = isc_arg_string;
	status[7] = (ISC_STATUS) (uintptr_t) "LP_EX";
	status[8] = isc_arg_end;
	error = NULL;

	require_condition(ib_fail_status(&error, "synthetic exception", status) == -1,
		"user exception status did not fail");
	require_condition(error != NULL, "user exception status produced no error");
	require_condition(strstr(error, "native status 335544517, gds 335544517 335544382, exception 42)") != NULL,
		"GDS codes and exception number are missing from the error envelope");
	ib_error_free(error);

	memset(status, 0, sizeof(status));
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_dsql_error;
	status[2] = isc_arg_end;
	error = NULL;
	require_condition(ib_fail_status(&error, "synthetic sql error", status) == -1,
		"plain status did not fail");
	require_condition(error != NULL && strstr(error, "gds 335544569)") != NULL,
		"single GDS code is missing");
	require_condition(strstr(error, "exception") == NULL,
		"plain status reported an exception number");
	ib_error_free(error);
}

static void test_status_codes_render_a_full_vector(void)
{
	char codes[IB_STATUS_CODES_LENGTH];
	char want[IB_STATUS_CODES_LENGTH * 2U];
	ISC_STATUS status[IB_STATUS_VECTOR_LENGTH];
	size_t index;
	size_t used;

	/* The most codes the loop reads: an unterminated vector of pairs, every
	 * code at its widest. */
	used = (size_t) snprintf(want, sizeof(want), ", gds");
	for (index = 0U; index + 1U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_gds;
		status[index + 1U] = (ISC_STATUS) INTPTR_MIN;
		used += (size_t) snprintf(want + used, sizeof(want) - used, " %" PRIdPTR,
			(intptr_t) INTPTR_MIN);
	}
	require_condition(index / 2U == IB_STATUS_VECTOR_LENGTH / 2U,
		"the full vector does not hold the maximum code count");
	require_condition(used < sizeof(codes),
		"the widest code rendering does not fit IB_STATUS_CODES_LENGTH");
	ib_status_codes(status, codes, sizeof(codes));
	require_condition(strcmp(codes, want) == 0,
		"a full status vector did not render every GDS code");

	/* The widest form with an exception: isc_except and its number take the
	 * first four slots, then every remaining pair is a widest code, and the
	 * exception number is widest too. */
	status[0] = isc_arg_gds;
	status[1] = (ISC_STATUS) isc_except;
	status[2] = isc_arg_number;
	status[3] = (ISC_STATUS) INTPTR_MIN;
	used = (size_t) snprintf(want, sizeof(want), ", gds %" PRIdPTR, (intptr_t) isc_except);
	for (index = 4U; index + 1U < IB_STATUS_VECTOR_LENGTH; index += 2U) {
		status[index] = isc_arg_gds;
		status[index + 1U] = (ISC_STATUS) INTPTR_MIN;
		used += (size_t) snprintf(want + used, sizeof(want) - used, " %" PRIdPTR,
			(intptr_t) INTPTR_MIN);
	}
	used += (size_t) snprintf(want + used, sizeof(want) - used, ", exception %" PRIdPTR,
		(intptr_t) INTPTR_MIN);
	require_condition(used < sizeof(codes),
		"the widest exception rendering does not fit IB_STATUS_CODES_LENGTH");
	ib_status_codes(status, codes, sizeof(codes));
	require_condition(strcmp(codes, want) == 0,
		"a full status vector lost its codes or exception number");
}

static void test_dialect_is_encoded_in_dpb(void)
{
	ib_connection *connection;
	char *error = NULL;
	size_t offset;
	int dialect_seen;
	int role_seen;
	int encrypted_seen;
	int system_encryption_seen;
	int timeout_seen;

	memset(captured_dpb, 0, sizeof(captured_dpb));
	captured_dpb_length = 0;
	connection = ib_connection_open("database", strlen("database"),
		"SYSDBA", strlen("SYSDBA"), "masterkey", strlen("masterkey"),
		"APP_ROLE", strlen("APP_ROLE"), "encrypted", strlen("encrypted"),
		"system-encryption", strlen("system-encryption"), "UTF8", strlen("UTF8"),
		SQL_DIALECT_V6, 0U, &error);
	require_condition(connection != NULL && error == NULL,
		"Dialect 3 connection open failed");
	if (connection == NULL) {
		ib_error_free(error);
		return;
	}
	require_condition(connection->dialect == SQL_DIALECT_V6,
		"Dialect 3 was not retained in native connection state");
	dialect_seen = 0;
	role_seen = 0;
	encrypted_seen = 0;
	system_encryption_seen = 0;
	timeout_seen = 0;
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
		if (tag == isc_dpb_sql_role_name) {
			role_seen++;
			require_condition(length == strlen("APP_ROLE") &&
				memcmp(captured_dpb + offset, "APP_ROLE", length) == 0,
				"captured DPB selected the wrong SQL role");
		}
		if (tag == isc_dpb_password_enc) {
			encrypted_seen++;
			require_condition(length == strlen("encrypted") &&
				memcmp(captured_dpb + offset, "encrypted", length) == 0,
				"captured DPB selected the wrong encrypted password");
		}
		if (tag == isc_dpb_sys_encrypt_password) {
			system_encryption_seen++;
			require_condition(length == strlen("system-encryption") &&
				memcmp(captured_dpb + offset, "system-encryption", length) == 0,
				"captured DPB selected the wrong system encryption password");
		}
		if (tag == isc_dpb_connect_timeout) {
			timeout_seen++;
		}
		offset += length;
	}
	require_condition(dialect_seen == 1, "captured DPB has no unique SQL dialect item");
	require_condition(role_seen == 1, "captured DPB has no unique SQL role item");
	require_condition(encrypted_seen == 1,
		"captured DPB has no unique encrypted password item");
	require_condition(system_encryption_seen == 1,
		"captured DPB has no unique system encryption password item");
	require_condition(timeout_seen == 0,
		"zero connect timeout unexpectedly changed the native default");
	connection->database = NULL;
	free(connection);
}

static void test_connect_timeout_is_encoded_as_little_endian_integer(void)
{
	const unsigned char expected[] = {0x04, 0x03, 0x02, 0x01};
	ib_connection *connection;
	char *error = NULL;
	size_t offset;
	int timeout_seen = 0;

	memset(captured_dpb, 0, sizeof(captured_dpb));
	captured_dpb_length = 0;
	connection = ib_connection_open("database", strlen("database"),
		"SYSDBA", strlen("SYSDBA"), "masterkey", strlen("masterkey"),
		NULL, 0U, NULL, 0U, NULL, 0U, "UTF8", strlen("UTF8"),
		SQL_DIALECT_V6, 0x01020304U, &error);
	require_condition(connection != NULL && error == NULL,
		"connection with connect timeout failed");
	if (connection == NULL) {
		ib_error_free(error);
		return;
	}
	offset = 1U;
	while (offset < (size_t) captured_dpb_length) {
		unsigned char tag;
		unsigned char length;

		require_condition((size_t) captured_dpb_length - offset >= 2U,
			"timeout DPB item header is truncated");
		if ((size_t) captured_dpb_length - offset < 2U) {
			break;
		}
		tag = captured_dpb[offset++];
		length = captured_dpb[offset++];
		require_condition((size_t) captured_dpb_length - offset >= length,
			"timeout DPB item is truncated");
		if ((size_t) captured_dpb_length - offset < length) {
			break;
		}
		if (tag == isc_dpb_connect_timeout) {
			timeout_seen++;
			require_condition(length == sizeof(expected) &&
				memcmp(captured_dpb + offset, expected, sizeof(expected)) == 0,
				"connect timeout DPB value is not little endian");
		}
		offset += length;
	}
	require_condition(timeout_seen == 1, "connect timeout DPB item is missing");
	connection->database = NULL;
	free(connection);
	ib_error_free(error);
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
	test_long_interpreted_status_is_bounded();
	test_status_vector_text_names_the_failure();
	test_long_string_argument_is_bounded();
	test_many_long_arguments_share_one_budget();
	test_missing_template_arguments_are_padded();
	test_malformed_arguments_are_safe();
	test_unterminated_status_vector_is_not_interpreted();
	test_sqlcode_status_keeps_numeric_context();
	test_status_codes_report_gds_and_exception_number();
	test_status_codes_render_a_full_vector();
	test_unset_connection_defaults_to_dialect_three();
	test_dialect_is_encoded_in_dpb();
	test_connect_timeout_is_encoded_as_little_endian_integer();
	(void) puts("native status tests passed");
	return EXIT_SUCCESS;
}
