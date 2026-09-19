#ifndef INTERBASE_GO_NATIVE_H
#define INTERBASE_GO_NATIVE_H

#include <stddef.h>
#include <stdint.h>

typedef struct ib_connection ib_connection;
typedef struct ib_transaction ib_transaction;
typedef struct ib_distributed_transaction ib_distributed_transaction;
typedef struct ib_cursor ib_cursor;
typedef struct ib_bindings ib_bindings;
typedef struct ib_statement ib_statement;
typedef struct ib_blob_reader ib_blob_reader;
typedef struct ib_blob_writer ib_blob_writer;
typedef struct ib_cancel_slot ib_cancel_slot;

enum ib_value_kind {
	IB_VALUE_NULL = 0,
	IB_VALUE_STRING = 1,
	IB_VALUE_INT64 = 2,
	IB_VALUE_FLOAT64 = 3,
	IB_VALUE_BOOL = 4,
	IB_VALUE_TIMESTAMP = 5,
	IB_VALUE_SCALED_INT = 6,
	IB_VALUE_BYTES = 7,
	IB_VALUE_ARRAY = 8,
	IB_VALUE_BLOB_REF = 9
};

ib_cancel_slot *ib_cancel_slot_new(char **error);
uint64_t ib_cancel_slot_begin(ib_cancel_slot *slot, char **error);
int ib_cancel_slot_cancel(ib_cancel_slot *slot, uint64_t generation,
	int64_t *native_code, char **error);
/* The owner must not start a new cancellation call after free begins.  Calls
 * that have entered the slot are included in free's lifetime wait. */
void ib_cancel_slot_free(ib_cancel_slot *slot);

/* The status of an SDK handle after an operation reports an error.  A live
 * handle remains owned by its wrapper and may be retried through an explicit
 * cleanup path; a consumed handle no longer names a native resource. */
enum ib_handle_state {
	IB_HANDLE_UNKNOWN = 0,
	IB_HANDLE_LIVE = 1,
	IB_HANDLE_CONSUMED = 2
};

/* Post-cancellation state for a mutating DSQL operation.  Unknown is
 * intentionally conservative: callers must not replay a write unless the
 * wrapper proved either a successful implicit rollback or a usable explicit
 * transaction. */
enum ib_write_outcome_state {
	IB_WRITE_OUTCOME_UNKNOWN = 0,
	IB_WRITE_OUTCOME_ROLLBACK_CONFIRMED = 1,
	IB_WRITE_OUTCOME_EXPLICIT_USABLE = 2
};

enum ib_array_element_kind {
	IB_ARRAY_ELEMENT_STRING = 1,
	IB_ARRAY_ELEMENT_INT64 = 2,
	IB_ARRAY_ELEMENT_FLOAT64 = 3,
	IB_ARRAY_ELEMENT_BOOL = 4,
	IB_ARRAY_ELEMENT_TIMESTAMP = 5,
	IB_ARRAY_ELEMENT_BYTES = 6
};

typedef struct ib_array_bound {
	int32_t lower;
	int32_t upper;
} ib_array_bound;

typedef struct ib_array_element {
	int kind;
	int64_t int64_value;
	double float64_value;
	int bool_value;
	int64_t year;
	int month;
	int day;
	int hour;
	int minute;
	int second;
	int nanosecond;
	const char *bytes;
	size_t length;
} ib_array_element;

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
	IB_METADATA_BLOB = 12,
	IB_METADATA_ARRAY = 13
};

typedef struct ib_array_view ib_array_view;

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
	const ib_array_view *array;
	int32_t blob_high;
	uint32_t blob_low;
	int blob_subtype;
	int blob_charset;
} ib_value_view;

struct ib_array_view {
	int dimensions;
	const ib_array_bound *bounds;
	size_t element_count;
	const ib_value_view *elements;
};

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
	size_t password_length, const char *role, size_t role_length,
	const char *encrypted_password, size_t encrypted_password_length,
	const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, uint32_t connect_timeout, char **error);
ib_connection *ib_connection_create(const char *database, size_t database_length,
	const char *user, size_t user_length, const char *password,
	size_t password_length, const char *role, size_t role_length,
	const char *encrypted_password, size_t encrypted_password_length,
	const char *system_encryption_password,
	size_t system_encryption_password_length, const char *charset,
	size_t charset_length, int dialect, int page_size, uint32_t connect_timeout,
	char **error);
int ib_connection_set_default_tpbs(ib_connection *connection,
	const char *read_tpb, size_t read_tpb_length, const char *write_tpb,
	size_t write_tpb_length, char **error);
int ib_connection_close(ib_connection *connection, char **error);
int ib_connection_drop(ib_connection *connection, int *consumed, char **error);
int ib_connection_is_broken(const ib_connection *connection);
int ib_client_version(char **version, char **error);
int ib_connection_begin(ib_connection *connection, int read_only, char **error);
int ib_connection_begin_tpb(ib_connection *connection, const char *tpb,
	size_t tpb_length, char **error);
int ib_connection_commit(ib_connection *connection, char **error);
int ib_connection_rollback(ib_connection *connection, char **error);
int ib_connection_rollback_cleanup(ib_connection *connection, char **error);
int ib_connection_transaction_state(const ib_connection *connection);
int ib_connection_write_outcome_state(const ib_connection *connection);
int ib_connection_commit_retaining(ib_connection *connection, char **error);
int ib_connection_rollback_retaining(ib_connection *connection, char **error);
int ib_connection_database_info(ib_connection *connection, uint8_t item,
	char **response, size_t *response_length, char **error);
int ib_connection_transaction_info(ib_connection *connection, uint8_t item,
	char **response, size_t *response_length, char **error);

ib_transaction *ib_transaction_begin(ib_connection *connection,
	const char *tpb, size_t tpb_length, char **error);
int ib_transaction_commit(ib_transaction *transaction, char **error);
int ib_transaction_rollback(ib_transaction *transaction, char **error);
int ib_transaction_rollback_cleanup(ib_transaction *transaction, char **error);
int ib_transaction_handle_state(const ib_transaction *transaction);
int ib_transaction_write_outcome_state(const ib_transaction *transaction);
int ib_transaction_commit_retaining(ib_transaction *transaction, char **error);
int ib_transaction_rollback_retaining(ib_transaction *transaction, char **error);
int ib_transaction_info(ib_transaction *transaction, uint8_t item,
	char **response, size_t *response_length, char **error);
ib_cursor *ib_transaction_query(ib_transaction *transaction, const char *query,
	size_t query_length, const ib_bindings *bindings, int allow_arrays,
	ib_cancel_slot *cancel, uint64_t generation, char **error);
int ib_transaction_exec(ib_transaction *transaction, const char *query,
	size_t query_length, const ib_bindings *bindings, int64_t *rows_affected,
	int allow_arrays, ib_cancel_slot *cancel, uint64_t generation,
	char **error);
ib_statement *ib_transaction_prepare(ib_transaction *transaction,
	const char *query, size_t query_length, int allow_arrays,
	ib_cancel_slot *cancel, uint64_t generation, char **error);
ib_blob_reader *ib_transaction_blob_open2(ib_transaction *transaction,
	int32_t high, uint32_t low, int subtype, int charset, char **error);
ib_blob_writer *ib_transaction_blob_create(ib_transaction *transaction,
	int subtype, int charset, char **error);
void ib_transaction_free(ib_transaction *transaction);

ib_distributed_transaction *ib_distributed_begin(ib_connection **connections,
	const char *const *tpbs, const size_t *tpb_lengths, size_t count,
	char **error);
ib_transaction *ib_distributed_participant(ib_distributed_transaction *transaction,
	size_t index);
int ib_distributed_prepare(ib_distributed_transaction *transaction,
	const char *message, size_t message_length, char **error);
int ib_distributed_commit(ib_distributed_transaction *transaction, char **error);
int ib_distributed_rollback(ib_distributed_transaction *transaction, char **error);
int ib_distributed_handle_state(const ib_distributed_transaction *transaction);
void ib_distributed_free(ib_distributed_transaction *transaction);

ib_bindings *ib_bindings_new(size_t count, char **error);
void ib_bindings_free(ib_bindings *bindings);
int ib_bindings_set_array(ib_bindings *bindings, size_t index,
	const ib_array_bound *bounds, size_t dimensions,
	const ib_array_element *elements, size_t element_count, char **error);
int ib_bindings_set_blob_ref(ib_bindings *bindings, size_t index,
	int32_t high, uint32_t low, int subtype, int charset, char **error);
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

ib_blob_reader *ib_blob_open(ib_connection *connection, int32_t high,
	uint32_t low, char **error);
ib_blob_reader *ib_blob_open2(ib_connection *connection, int32_t high,
	uint32_t low, int subtype, int charset, char **error);
int ib_blob_reader_read(ib_blob_reader *reader, char *buffer, size_t capacity,
	size_t *length, int *eof, char **error);
int ib_blob_reader_close(ib_blob_reader *reader, int cancel, char **error);
ib_blob_writer *ib_blob_create(ib_connection *connection, int subtype,
	int charset, char **error);
int ib_blob_writer_write(ib_blob_writer *writer, const char *data,
	size_t length, char **error);
int ib_blob_writer_close(ib_blob_writer *writer, int cancel, int32_t *high,
	uint32_t *low, int *subtype, int *charset, char **error);

ib_cursor *ib_connection_query(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, int allow_arrays,
	ib_cancel_slot *cancel, uint64_t generation, char **error);
int ib_connection_exec(ib_connection *connection, const char *query,
	size_t query_length, const ib_bindings *bindings, int64_t *rows_affected,
	int allow_arrays, ib_cancel_slot *cancel, uint64_t generation,
	char **error);
ib_statement *ib_statement_prepare(ib_connection *connection, const char *query,
	size_t query_length, ib_cancel_slot *cancel, uint64_t generation,
	char **error);
int ib_statement_num_input(const ib_statement *statement);
int ib_statement_exec(ib_statement *statement, const ib_bindings *bindings,
	ib_cancel_slot *cancel, uint64_t generation, int64_t *rows_affected,
	char **error);
int ib_statement_write_outcome_state(const ib_statement *statement);
ib_cursor *ib_statement_query(ib_statement *statement,
	const ib_bindings *bindings, ib_cancel_slot *cancel, uint64_t generation,
	char **error);
int ib_statement_close(ib_statement *statement, char **error);
int ib_statement_plan(ib_statement *statement, char **plan,
	size_t *plan_length, char **error);
int ib_cursor_next(ib_cursor *cursor, ib_cancel_slot *cancel,
	uint64_t generation, char **error);
int ib_cursor_close(ib_cursor *cursor, char **error);
int ib_cursor_abort(ib_cursor *cursor, char **error);
int ib_cursor_set_name(ib_cursor *cursor, const char *name, size_t name_length,
	char **error);
int ib_cursor_uses_connection_transaction(const ib_cursor *cursor);
size_t ib_cursor_column_count(const ib_cursor *cursor);
const char *ib_cursor_column_name(const ib_cursor *cursor, size_t index,
	size_t *length);
int ib_cursor_column_metadata(const ib_cursor *cursor, size_t index,
	ib_column_metadata *metadata, char **error);
int ib_cursor_column_indicator(const ib_cursor *cursor, size_t index,
	uint16_t *indicator, char **error);
int ib_cursor_column(const ib_cursor *cursor, size_t index,
	ib_value_view *view, char **error);
int ib_array_view_dimensions(const ib_array_view *array);
size_t ib_array_view_element_count(const ib_array_view *array);
int ib_array_view_bound(const ib_array_view *array, size_t index,
	ib_array_bound *bound, char **error);
int ib_array_view_element(const ib_array_view *array, size_t index,
	ib_value_view *view, char **error);

void ib_error_free(char *error);

#endif
