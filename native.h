#ifndef INTERBASE_GO_NATIVE_H
#define INTERBASE_GO_NATIVE_H

#include <stddef.h>
#include <stdint.h>

typedef struct ib_connection ib_connection;
typedef struct ib_cursor ib_cursor;
typedef struct ib_bindings ib_bindings;

enum ib_value_kind {
	IB_VALUE_NULL = 0,
	IB_VALUE_STRING = 1,
	IB_VALUE_INT64 = 2,
	IB_VALUE_FLOAT64 = 3,
	IB_VALUE_BOOL = 4,
	IB_VALUE_TIMESTAMP = 5,
	IB_VALUE_SCALED_INT = 6
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

ib_connection *ib_connection_open(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, char **error);
int ib_connection_close(ib_connection *connection, char **error);
int ib_connection_is_broken(const ib_connection *connection);

ib_bindings *ib_bindings_new(size_t count, char **error);
void ib_bindings_free(ib_bindings *bindings);
int ib_bindings_set_null(ib_bindings *bindings, size_t index, char **error);
int ib_bindings_set_string(ib_bindings *bindings, size_t index,
	const char *value, size_t length, char **error);
int ib_bindings_set_int64(ib_bindings *bindings, size_t index,
	int64_t value, char **error);
int ib_bindings_set_float64(ib_bindings *bindings, size_t index,
	double value, char **error);
int ib_bindings_set_bool(ib_bindings *bindings, size_t index, int value,
	char **error);

ib_cursor *ib_connection_query(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, char **error);
int ib_cursor_next(ib_cursor *cursor, char **error);
int ib_cursor_close(ib_cursor *cursor, char **error);
size_t ib_cursor_column_count(const ib_cursor *cursor);
const char *ib_cursor_column_name(const ib_cursor *cursor, size_t index,
	size_t *length);
int ib_cursor_column(const ib_cursor *cursor, size_t index,
	ib_value_view *view, char **error);

void ib_error_free(char *error);

#endif
