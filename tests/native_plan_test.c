#include <stdio.h>
#include <stdlib.h>
#include <string.h>
#include <ibase.h>

static int sql_info_calls;
static int execute_calls;
static int response_mode;
static char database_token;
static char transaction_token;
static char statement_token;

ISC_STATUS ISC_EXPORT test_dsql_allocate_statement(ISC_STATUS *status,
	isc_db_handle *database, isc_stmt_handle *statement);
ISC_STATUS ISC_EXPORT test_dsql_describe_bind(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input);
ISC_STATUS ISC_EXPORT test_dsql_describe(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output);
ISC_STATUS ISC_EXPORT test_dsql_prepare(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short query_length, char *query, unsigned short dialect,
	XSQLDA *output);
ISC_STATUS ISC_EXPORT test_dsql_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option);

ISC_STATUS ISC_EXPORT test_execute(ISC_STATUS *status, isc_tr_handle *transaction,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input);
ISC_STATUS ISC_EXPORT test_execute2(ISC_STATUS *status, isc_tr_handle *transaction,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input, XSQLDA *output);
ISC_STATUS ISC_EXPORT test_sql_info(ISC_STATUS *status, isc_stmt_handle *statement,
	short request_length, char *request, short response_length, char *response);

#define isc_dsql_sql_info test_sql_info
#define isc_dsql_execute test_execute
#define isc_dsql_execute2 test_execute2
#define isc_dsql_allocate_statement test_dsql_allocate_statement
#define isc_dsql_describe_bind test_dsql_describe_bind
#define isc_dsql_describe test_dsql_describe
#define isc_dsql_prepare test_dsql_prepare
#define isc_dsql_free_statement test_dsql_free_statement
#include "../native.c"

static void check(int condition, const char *message)
{
	if (!condition) {
		fprintf(stderr, "native plan test failed: %s\n", message);
		exit(EXIT_FAILURE);
	}
}

static ISC_STATUS test_execute_failure(ISC_STATUS *status)
{
	status[0] = isc_arg_gds;
	status[1] = isc_network_error;
	status[2] = isc_arg_end;
	return status[1];
}

ISC_STATUS ISC_EXPORT test_execute(ISC_STATUS *status, isc_tr_handle *transaction,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input)
{
	(void) transaction;
	(void) statement;
	(void) dialect;
	(void) input;
	execute_calls++;
	return test_execute_failure(status);
}

ISC_STATUS ISC_EXPORT test_execute2(ISC_STATUS *status, isc_tr_handle *transaction,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input, XSQLDA *output)
{
	(void) transaction;
	(void) statement;
	(void) dialect;
	(void) input;
	(void) output;
	execute_calls++;
	return test_execute_failure(status);
}

ISC_STATUS ISC_EXPORT test_sql_info(ISC_STATUS *status,
	isc_stmt_handle *statement, short request_length, char *request,
	short response_length, char *response)
{
	(void) statement;
	(void) request_length;
	(void) request;
	sql_info_calls++;
	memset(response, 0, (size_t) response_length);
	if (request_length == 1 && request[0] == isc_info_sql_stmt_type) {
		check(response_length >= 8, "statement type response buffer is too short");
		response[0] = isc_info_sql_stmt_type;
		response[1] = 4;
		response[3] = isc_info_sql_stmt_select;
		response[7] = isc_info_end;
		return 0;
	}
	switch (response_mode) {
	case 0:
		response[0] = isc_info_sql_get_plan;
		response[1] = 0;
		response[2] = 0;
		response[3] = isc_info_end;
		break;
	case 1:
		response[0] = isc_info_end;
		break;
	case 2:
		response[0] = isc_info_sql_get_plan;
		response[1] = 5;
		response[2] = 0;
		memcpy(response + 3, "short", 5U);
		break;
	default:
		return 1;
	}
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

static void test_plan_response(int mode, int want_error, const char *want)
{
	char handle_token;
	ib_connection connection = {0};
	ib_statement statement = {0};
	char *plan = NULL;
	char *error = NULL;

	connection.database = &handle_token;
	statement.connection = &connection;
	statement.statement = &handle_token;
	response_mode = mode;
	sql_info_calls = 0;
	execute_calls = 0;

	{
		size_t plan_length = 0U;
		plan = NULL;
		error = NULL;
		int result = ib_statement_plan(&statement, &plan, &plan_length, &error);
		check((result != 0) == want_error, "plan result had the wrong error state");
		if (!want_error) {
			check(plan != NULL && plan_length == strlen(want) &&
				memcmp(plan, want, plan_length) == 0,
				"empty/no-plan response was not returned as the expected string");
		}
		check(execute_calls == 0, "plan request executed the statement");
		check(sql_info_calls == 1, "plan request made an unexpected number of info calls");
		free(plan);
		ib_error_free(error);
	}
}

ISC_STATUS ISC_EXPORT test_dsql_allocate_statement(ISC_STATUS *status,
	isc_db_handle *database, isc_stmt_handle *statement)
{
	(void) database;
	*statement = &statement_token;
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

ISC_STATUS ISC_EXPORT test_dsql_describe_bind(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *input)
{
	(void) statement;
	(void) dialect;
	input->sqld = 0;
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

ISC_STATUS ISC_EXPORT test_dsql_describe(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short dialect, XSQLDA *output)
{
	(void) statement;
	(void) dialect;
	output->sqld = 1;
	output->sqlvar[0].sqltype = SQL_ARRAY | 1;
	output->sqlvar[0].sqllen = (short) sizeof(ISC_QUAD);
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

ISC_STATUS ISC_EXPORT test_dsql_prepare(ISC_STATUS *status,
	isc_tr_handle *transaction, isc_stmt_handle *statement,
	unsigned short query_length, char *query, unsigned short dialect,
	XSQLDA *output)
{
	(void) transaction;
	(void) statement;
	(void) query_length;
	(void) query;
	(void) dialect;
	(void) output;
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

ISC_STATUS ISC_EXPORT test_dsql_free_statement(ISC_STATUS *status,
	isc_stmt_handle *statement, unsigned short option)
{
	(void) option;
	*statement = NULL;
	status[0] = isc_arg_gds;
	status[1] = 0;
	status[2] = isc_arg_end;
	return 0;
}

static void test_transaction_prepare_array_mode(void)
{
	static const char query[] = "SELECT VALUES_ARRAY FROM T";
	ib_connection connection;
	ib_transaction transaction;
	char *error = NULL;
	ib_statement *statement;

	memset(&connection, 0, sizeof(connection));
	connection.database = &database_token;
	connection.transaction = &transaction_token;
	connection.dialect = SQL_DIALECT_V6;
	memset(&transaction, 0, sizeof(transaction));
	transaction.parent = &connection;
	transaction.view = connection;

	statement = ib_transaction_prepare(&transaction, query, sizeof(query) - 1U,
		1, &error);
	check(statement != NULL && error == NULL,
		"array-capable transaction preparation rejected an array result");
	ib_error_free(error);
	if (statement != NULL) {
		error = NULL;
		check(ib_statement_close(statement, &error) == 0 && error == NULL,
			"array-capable prepared statement close failed");
		ib_error_free(error);
	}

	error = NULL;
	statement = ib_transaction_prepare(&transaction, query, sizeof(query) - 1U,
		0, &error);
	check(statement == NULL && error != NULL,
		"ordinary transaction preparation accepted an array result");
	ib_error_free(error);
}

int main(void)
{
	test_transaction_prepare_array_mode();
	test_plan_response(0, 0, "");
	test_plan_response(1, 0, "");
	test_plan_response(2, 1, NULL);
	puts("native plan tests passed");
	return EXIT_SUCCESS;
}
