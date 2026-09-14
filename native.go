package interbase

/*
#cgo linux,amd64 CFLAGS: -I/opt/interbase/include
#cgo linux,amd64 LDFLAGS: -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds
#include <stdlib.h>
#include "native.h"
*/
import "C"

import (
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"time"
	"unsafe"
)

const (
	nativeValueNull      = 0
	nativeValueString    = 1
	nativeValueInt64     = 2
	nativeValueFloat64   = 3
	nativeValueBool      = 4
	nativeValueTimestamp = 5
	nativeValueScaledInt = 6
	nativeValueBytes     = 7
)

type nativeConnection struct {
	ptr *C.ib_connection
}

type nativeStatement struct {
	ptr *C.ib_statement
}

type nativeCursor struct {
	ptr       *C.ib_cursor
	statement *nativeStatement
}

func openNative(cfg Config) (*nativeConnection, error) {
	database := C.CString(cfg.Database)
	defer C.free(unsafe.Pointer(database))
	user := C.CString(cfg.User)
	defer C.free(unsafe.Pointer(user))
	password := C.CString(cfg.Password)
	defer C.free(unsafe.Pointer(password))
	charset, err := normalizeCharset(cfg.Charset)
	if err != nil {
		return nil, err
	}
	charsetPointer := C.CString(charset)
	defer C.free(unsafe.Pointer(charsetPointer))

	var errorPointer *C.char
	connection := C.ib_connection_open(
		database,
		C.size_t(len(cfg.Database)),
		user,
		C.size_t(len(cfg.User)),
		password,
		C.size_t(len(cfg.Password)),
		charsetPointer,
		C.size_t(len(charset)),
		&errorPointer,
	)
	if connection == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &nativeConnection{ptr: connection}, nil
}

func (c *nativeConnection) close() error {
	if c == nil || c.ptr == nil {
		return nil
	}
	connection := c.ptr
	c.ptr = nil
	var errorPointer *C.char
	if result := C.ib_connection_close(connection, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) broken() bool {
	return c == nil || c.ptr == nil || C.ib_connection_is_broken(c.ptr) != 0
}

func newNativeBindings(args []argument) (*C.ib_bindings, error) {
	var errorPointer *C.char
	bindings := C.ib_bindings_new(C.size_t(len(args)), &errorPointer)
	if bindings == nil {
		return nil, takeNativeError(errorPointer)
	}
	for index, arg := range args {
		if err := setNativeBinding(bindings, index, arg); err != nil {
			C.ib_bindings_free(bindings)
			return nil, err
		}
	}
	return bindings, nil
}

func (s *nativeStatement) numInput() int {
	if s == nil || s.ptr == nil {
		return 0
	}
	return int(C.ib_statement_num_input(s.ptr))
}

func (s *nativeStatement) exec(args []argument) (int64, error) {
	if s == nil || s.ptr == nil {
		return 0, errors.New("native statement is unavailable")
	}
	bindings, err := newNativeBindings(args)
	if err != nil {
		return 0, err
	}
	defer C.ib_bindings_free(bindings)

	var affected C.int64_t
	var errorPointer *C.char
	if result := C.ib_statement_exec(s.ptr, bindings, &affected, &errorPointer); result != 0 {
		return 0, takeNativeError(errorPointer)
	}
	return int64(affected), nil
}

func (s *nativeStatement) query(args []argument) (*nativeCursor, []string, error) {
	if s == nil || s.ptr == nil {
		return nil, nil, errors.New("native statement is unavailable")
	}
	bindings, err := newNativeBindings(args)
	if err != nil {
		return nil, nil, err
	}
	defer C.ib_bindings_free(bindings)

	var errorPointer *C.char
	cursor := C.ib_statement_query(s.ptr, bindings, &errorPointer)
	if cursor == nil {
		return nil, nil, takeNativeError(errorPointer)
	}
	nativeRows := &nativeCursor{ptr: cursor, statement: s}
	count := C.ib_cursor_column_count(cursor)
	if uint64(count) > uint64(maxInt()) {
		return nil, nil, nativeCursorQueryError(nativeRows, errors.New("result has too many columns"))
	}
	columns := make([]string, int(count))
	for index := range columns {
		var nameLength C.size_t
		name := C.ib_cursor_column_name(cursor, C.size_t(index), &nameLength)
		if name == nil {
			return nil, nil, nativeCursorQueryError(nativeRows, errors.New("result column name is unavailable"))
		}
		if uint64(nameLength) > uint64(math.MaxInt32) {
			return nil, nil, nativeCursorQueryError(nativeRows, errors.New("result column name is too long"))
		}
		columns[index] = string(C.GoBytes(unsafe.Pointer(name), C.int(nameLength)))
	}
	return nativeRows, columns, nil
}

func (s *nativeStatement) close() error {
	if s == nil || s.ptr == nil {
		return nil
	}
	statement := s.ptr
	s.ptr = nil
	var errorPointer *C.char
	if result := C.ib_statement_close(statement, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) query(query string, args []argument) (*nativeCursor, []string, error) {
	if c == nil || c.ptr == nil {
		return nil, nil, errors.New("native connection is unavailable")
	}

	var errorPointer *C.char
	bindings := C.ib_bindings_new(C.size_t(len(args)), &errorPointer)
	if bindings == nil {
		return nil, nil, takeNativeError(errorPointer)
	}
	defer C.ib_bindings_free(bindings)

	for index, arg := range args {
		if err := setNativeBinding(bindings, index, arg); err != nil {
			return nil, nil, err
		}
	}

	queryPointer := C.CString(query)
	defer C.free(unsafe.Pointer(queryPointer))
	var cursorError *C.char
	cursor := C.ib_connection_query(c.ptr, queryPointer, C.size_t(len(query)), bindings, &cursorError)
	if cursor == nil {
		return nil, nil, takeNativeError(cursorError)
	}
	nativeRows := &nativeCursor{ptr: cursor}

	count := C.ib_cursor_column_count(cursor)
	if uint64(count) > uint64(maxInt()) {
		return nil, nil, nativeCursorQueryError(nativeRows, errors.New("result has too many columns"))
	}
	columns := make([]string, int(count))
	for index := range columns {
		var nameLength C.size_t
		name := C.ib_cursor_column_name(cursor, C.size_t(index), &nameLength)
		if name == nil {
			return nil, nil, nativeCursorQueryError(nativeRows, errors.New("result column name is unavailable"))
		}
		if uint64(nameLength) > uint64(math.MaxInt32) {
			return nil, nil, nativeCursorQueryError(nativeRows, errors.New("result column name is too long"))
		}
		columns[index] = string(C.GoBytes(unsafe.Pointer(name), C.int(nameLength)))
	}
	return nativeRows, columns, nil
}

func (c *nativeConnection) exec(query string, args []argument) (int64, error) {
	if c == nil || c.ptr == nil {
		return 0, errors.New("native connection is unavailable")
	}

	var errorPointer *C.char
	bindings := C.ib_bindings_new(C.size_t(len(args)), &errorPointer)
	if bindings == nil {
		return 0, takeNativeError(errorPointer)
	}
	defer C.ib_bindings_free(bindings)

	for index, arg := range args {
		if err := setNativeBinding(bindings, index, arg); err != nil {
			return 0, err
		}
	}

	queryPointer := C.CString(query)
	defer C.free(unsafe.Pointer(queryPointer))
	var affected C.int64_t
	var execError *C.char
	if result := C.ib_connection_exec(c.ptr, queryPointer, C.size_t(len(query)),
		bindings, &affected, &execError); result != 0 {
		return 0, takeNativeError(execError)
	}
	return int64(affected), nil
}

func (c *nativeConnection) begin(readOnly bool) error {
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	readOnlyValue := C.int(0)
	if readOnly {
		readOnlyValue = 1
	}
	var errorPointer *C.char
	if result := C.ib_connection_begin(c.ptr, readOnlyValue, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) commit() error {
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_connection_commit(c.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) rollback() error {
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_connection_rollback(c.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) prepare(query string) (*nativeStatement, error) {
	if c == nil || c.ptr == nil {
		return nil, errors.New("native connection is unavailable")
	}
	queryPointer := C.CString(query)
	defer C.free(unsafe.Pointer(queryPointer))
	var errorPointer *C.char
	statement := C.ib_statement_prepare(c.ptr, queryPointer, C.size_t(len(query)), &errorPointer)
	if statement == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &nativeStatement{ptr: statement}, nil
}

func setNativeBinding(bindings *C.ib_bindings, index int, arg argument) error {
	var errorPointer *C.char
	var result C.int
	switch arg.kind {
	case argumentNull:
		result = C.ib_bindings_set_null(bindings, C.size_t(index), &errorPointer)
	case argumentString:
		var valuePointer unsafe.Pointer
		if len(arg.stringValue) != 0 {
			valuePointer = C.CBytes([]byte(arg.stringValue))
		}
		result = C.ib_bindings_set_string(
			bindings,
			C.size_t(index),
			(*C.char)(valuePointer),
			C.size_t(len(arg.stringValue)),
			&errorPointer,
		)
		C.free(valuePointer)
	case argumentBytes:
		var valuePointer unsafe.Pointer
		if len(arg.bytesValue) != 0 {
			valuePointer = C.CBytes(arg.bytesValue)
		}
		result = C.ib_bindings_set_bytes(
			bindings,
			C.size_t(index),
			(*C.char)(valuePointer),
			C.size_t(len(arg.bytesValue)),
			&errorPointer,
		)
		C.free(valuePointer)
	case argumentInt64:
		result = C.ib_bindings_set_int64(bindings, C.size_t(index), C.int64_t(arg.int64Value), &errorPointer)
	case argumentFloat64:
		result = C.ib_bindings_set_float64(bindings, C.size_t(index), C.double(arg.float64Value), &errorPointer)
	case argumentBool:
		value := C.int(0)
		if arg.boolValue {
			value = 1
		}
		result = C.ib_bindings_set_bool(bindings, C.size_t(index), value, &errorPointer)
	case argumentTimestamp:
		year, month, day := arg.timeValue.Date()
		hour, minute, second := arg.timeValue.Clock()
		result = C.ib_bindings_set_timestamp(
			bindings,
			C.size_t(index),
			C.int(year), C.int(month), C.int(day),
			C.int(hour), C.int(minute), C.int(second),
			C.int(arg.timeValue.Nanosecond()),
			&errorPointer,
		)
	default:
		return errors.New("unsupported native query argument")
	}
	if result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func nativeCursorQueryError(cursor *nativeCursor, primary error) error {
	closeErr := cursor.close()
	if closeErr != nil {
		return errors.Join(primary, closeErr)
	}
	return primary
}

func (c *nativeCursor) next() (bool, error) {
	if c == nil || c.ptr == nil {
		return false, errors.New("native cursor is unavailable")
	}
	var errorPointer *C.char
	result := C.ib_cursor_next(c.ptr, &errorPointer)
	switch result {
	case 0:
		return false, nil
	case 1:
		return true, nil
	default:
		return false, takeNativeError(errorPointer)
	}
}

func (c *nativeCursor) close() error {
	if c == nil || c.ptr == nil {
		return nil
	}
	cursor := c.ptr
	c.ptr = nil
	var errorPointer *C.char
	if result := C.ib_cursor_close(cursor, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeCursor) value(index int) (driver.Value, error) {
	if c == nil || c.ptr == nil {
		return nil, errors.New("native cursor is unavailable")
	}
	var view C.ib_value_view
	var errorPointer *C.char
	if result := C.ib_cursor_column(c.ptr, C.size_t(index), &view, &errorPointer); result != 0 {
		return nil, takeNativeError(errorPointer)
	}

	switch int(view.kind) {
	case nativeValueNull:
		return nil, nil
	case nativeValueString:
		if uint64(view.length) > uint64(math.MaxInt32) {
			return nil, errors.New("result string is too long")
		}
		return string(C.GoBytes(unsafe.Pointer(view.bytes), C.int(view.length))), nil
	case nativeValueBytes:
		if uint64(view.length) > uint64(math.MaxInt32) {
			return nil, errors.New("result bytes are too long")
		}
		return C.GoBytes(unsafe.Pointer(view.bytes), C.int(view.length)), nil
	case nativeValueInt64:
		return int64(view.int64_value), nil
	case nativeValueFloat64:
		return float64(view.float64_value), nil
	case nativeValueBool:
		return int(view.bool_value) != 0, nil
	case nativeValueTimestamp:
		return time.Date(
			int(view.year),
			time.Month(view.month),
			int(view.day),
			int(view.hour),
			int(view.minute),
			int(view.second),
			int(view.nanosecond),
			time.UTC,
		), nil
	case nativeValueScaledInt:
		return formatScaledInteger(int64(view.int64_value), int16(view.scale))
	default:
		return nil, fmt.Errorf("unsupported native result kind %d", int(view.kind))
	}
}

func takeNativeError(errorPointer *C.char) error {
	if errorPointer == nil {
		return errors.New("InterBase native operation failed")
	}
	message := C.GoString(errorPointer)
	C.ib_error_free(errorPointer)
	if message == "" {
		return errors.New("InterBase native operation failed")
	}
	return errors.New(message)
}

func maxInt() int {
	return int(^uint(0) >> 1)
}
