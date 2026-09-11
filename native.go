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
)

type nativeConnection struct {
	ptr *C.ib_connection
}

type nativeCursor struct {
	ptr *C.ib_cursor
}

func openNative(cfg Config) (*nativeConnection, error) {
	database := C.CString(cfg.Database)
	defer C.free(unsafe.Pointer(database))
	user := C.CString(cfg.User)
	defer C.free(unsafe.Pointer(user))
	password := C.CString(cfg.Password)
	defer C.free(unsafe.Pointer(password))

	var errorPointer *C.char
	connection := C.ib_connection_open(
		database,
		C.size_t(len(cfg.Database)),
		user,
		C.size_t(len(cfg.User)),
		password,
		C.size_t(len(cfg.Password)),
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
