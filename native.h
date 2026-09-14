#ifndef INTERBASE_GO_NATIVE_H
#define INTERBASE_GO_NATIVE_H

#include <stddef.h>
#include <stdint.h>

typedef struct ib_connection ib_connection;
typedef struct ib_cursor ib_cursor;
typedef struct ib_bindings ib_bindings;
typedef struct ib_statement ib_statement;

enum ib_value_kind {
	IB_VALUE_NULL = 0,
	IB_VALUE_STRING = 1,
	IB_VALUE_INT64 = 2,
	IB_VALUE_FLOAT64 = 3,
	IB_VALUE_BOOL = 4,
	IB_VALUE_TIMESTAMP = 5,
	IB_VALUE_SCALED_INT = 6,
	IB_VALUE_BYTES = 7
};

enum ib_metadata_type {
	IB_METADATA_UNKNOWN = 0,
	IB_METADATA_CHAR = 1,
	IB_METADATA_VARCHAR = 2,
	IB_METADATA_SMALLINT = 3,
	IB_METADATA_INTEGER = 4,
	IB_METADATA_BIGINT = 5,
	IB_METADATA_FLOAT = 6,
	IB_METADATA_DOUBLE = 7,
	IB_METADATA_TIMESTAMP = 8,
	IB_METADATA_DATE = 9,
	IB_METADATA_TIME = 10,
	IB_METADATA_BOOLEAN = 11,
	IB_METADATA_BLOB = 12
};

typedef struct ib_value_view {
	int kind;
	int64_t int64_value;
	double float64_value;
	int bool_value;
	int scale;
	int year;
	int month;
	int day;
	int hour;
	int minute;
	int second;
	int nanosecond;
	const char *bytes;
	size_t length;
} ib_value_view;

typedef struct ib_column_metadata {
	int sql_type;
	int sql_subtype;
	int sql_scale;
	int sql_precision;
	int length;
	int has_length;
	int nullable;
	int has_nullable;
	int precision;
	int scale;
	int has_precision_scale;
} ib_column_metadata;

ib_connection *ib_connection_open(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, const char *charset, size_t charset_length,
	int dialect, char **error);
int ib_connection_close(ib_connection *connection, char **error);
int ib_connection_is_broken(const ib_connection *connection);
int ib_connection_begin(ib_connection *connection, int read_only, char **error);
int ib_connection_commit(ib_connection *connection, char **error);
int ib_connection_rollback(ib_connection *connection, char **error);

ib_bindings *ib_bindings_new(size_t count, char **error);
void ib_bindings_free(ib_bindings *bindings);
int ib_bindings_set_null(ib_bindings *bindings, size_t index, char **error);
int ib_bindings_set_string(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error);
int ib_bindings_set_bytes(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error);
int ib_bindings_set_int64(ib_bindings *bindings, size_t index,
	int64_t value, char **error);
int ib_bindings_set_float64(ib_bindings *bindings, size_t index,
	double value, char **error);
int ib_bindings_set_bool(ib_bindings *bindings, size_t index, int value,
	char **error);
int ib_bindings_set_timestamp(ib_bindings *bindings, size_t index,
	int64_t year, int month, int day, int hour, int minute, int second,
	int nanosecond, char **error);

ib_cursor *ib_connection_query(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, char **error);
int ib_connection_exec(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, int64_t *rows_affected,
	char **error);
ib_statement *ib_statement_prepare(ib_connection *connection, const char *query,
	size_t query_length, char **error);
int ib_statement_num_input(const ib_statement *statement);
int ib_statement_exec(ib_statement *statement, const ib_bindings *bindings,
	int64_t *rows_affected, char **error);
ib_cursor *ib_statement_query(ib_statement *statement,
	const ib_bindings *bindings, char **error);
int ib_statement_close(ib_statement *statement, char **error);
int ib_cursor_next(ib_cursor *cursor, char **error);
int ib_cursor_close(ib_cursor *cursor, char **error);
int ib_cursor_abort(ib_cursor *cursor, char **error);
size_t ib_cursor_column_count(const ib_cursor *cursor);
const char *ib_cursor_column_name(const ib_cursor *cursor, size_t index,
	size_t *length);
int ib_cursor_column_metadata(const ib_cursor *cursor, size_t index,
	ib_column_metadata *metadata, char **error);
int ib_cursor_column(const ib_cursor *cursor, size_t index,
	ib_value_view *view, char **error);

void ib_error_free(char *error);

#endif
