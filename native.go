package interbase

/*
#cgo linux,amd64 CFLAGS: -I/opt/interbase/include
#cgo linux,amd64 LDFLAGS: -L/opt/interbase/lib -Wl,-rpath,/opt/interbase/lib -lgds
#include <stdlib.h>
#include "native.h"
*/
import "C"

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"math"
	"reflect"
	"time"
	"unsafe"

	"interbase-go/internal/nativegate"
)

// enterNativeContext admits an ordinary native call before the caller performs
// any client-library side effect. The second context check closes the small
// race between admission and the caller's first native operation; callers
// still decide separately whether a post-operation cancellation is a known
// success or an operation error.
func enterNativeContext(ctx context.Context) (func(), error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	release, err := nativegate.Global.EnterContext(ctx)
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		release()
		return nil, err
	}
	return release, nil
}

const (
	nativeValueNull      = 0
	nativeValueString    = 1
	nativeValueInt64     = 2
	nativeValueFloat64   = 3
	nativeValueBool      = 4
	nativeValueTimestamp = 5
	nativeValueScaledInt = 6
	nativeValueBytes     = 7
	nativeValueArray     = 8
	nativeValueBlobRef   = 9
)

type nativeHandleState uint8

const (
	nativeHandleUnknown  nativeHandleState = nativeHandleState(C.IB_HANDLE_UNKNOWN)
	nativeHandleLive     nativeHandleState = nativeHandleState(C.IB_HANDLE_LIVE)
	nativeHandleConsumed nativeHandleState = nativeHandleState(C.IB_HANDLE_CONSUMED)
)

type nativeConnection struct {
	ptr                       *C.ib_connection
	dialect                   int
	brokenOverride            func() bool
	closeOverride             func() error
	beginTransactionOverride  func([]byte) (*nativeTransaction, error)
	beginDistributedOverride  func([]*nativeConnection, [][]byte) (*nativeDistributedTransaction, error)
	dropOverride              func() error
	dropConsumedOverride      bool
	execOverride              func(string, []argument, bool) (int64, error)
	prepareOverride           func(string) (*nativeStatement, error)
	databaseInfoOverride      func(byte) ([]byte, error)
	clientVersionOverride     func() (string, error)
	commitRetainingOverride   func() error
	rollbackRetainingOverride func() error
}

type nativeTransaction struct {
	ptr                     *C.ib_transaction
	freeOverride            func()
	commitOverride          func() error
	rollbackOverride        func() error
	rollbackCleanupOverride func() (error, nativeHandleState)
	handleStateOverride     func() nativeHandleState
	commitRetainOverride    func() error
	rollbackRetainOverride  func() error
	infoOverride            func(byte) ([]byte, error)
	openBlobOverride        func(int32, uint32, int16, int16) (*blobStream, error)
}

type nativeDistributedTransaction struct {
	ptr                 *C.ib_distributed_transaction
	participants        []*nativeTransaction
	prepareOverride     func([]byte) error
	commitOverride      func() error
	rollbackOverride    func() error
	handleStateOverride func() nativeHandleState
	freeOverride        func()
}

func (c *nativeConnection) beginTransaction(tpb []byte) (*nativeTransaction, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.beginTransactionOverride != nil {
		return c.beginTransactionOverride(append([]byte(nil), tpb...))
	}
	if c == nil || c.ptr == nil {
		return nil, errors.New("interbase: native connection is unavailable")
	}
	if len(tpb) == 0 {
		return nil, errors.New("interbase: transaction parameter block is empty")
	}
	var errorPointer *C.char
	transaction := C.ib_transaction_begin(c.ptr,
		(*C.char)(unsafe.Pointer(&tpb[0])), C.size_t(len(tpb)), &errorPointer)
	if transaction == nil {
		return nil, takeNativeError(errorPointer)
	}
	native := &nativeTransaction{ptr: transaction}
	if errorPointer != nil {
		return native, takeNativeError(errorPointer)
	}
	return native, nil
}

func (c *nativeConnection) beginDistributed(connections []*nativeConnection,
	tpbs [][]byte) (*nativeDistributedTransaction, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.beginDistributedOverride != nil {
		return c.beginDistributedOverride(connections, tpbs)
	}
	if len(connections) == 0 || len(connections) != len(tpbs) {
		return nil, errors.New("interbase: distributed transaction participants are invalid")
	}
	const maxArrayElements = 1 << 20
	if len(connections) > maxArrayElements {
		return nil, errors.New("interbase: too many distributed transaction participants")
	}
	connectionMemory := C.calloc(C.size_t(len(connections)), C.size_t(unsafe.Sizeof(uintptr(0))))
	tpbMemory := C.calloc(C.size_t(len(tpbs)), C.size_t(unsafe.Sizeof(uintptr(0))))
	lengthMemory := C.calloc(C.size_t(len(tpbs)), C.size_t(unsafe.Sizeof(C.size_t(0))))
	if connectionMemory == nil || tpbMemory == nil || lengthMemory == nil {
		C.free(connectionMemory)
		C.free(tpbMemory)
		C.free(lengthMemory)
		return nil, errors.New("interbase: out of memory allocating distributed transaction inputs")
	}
	defer C.free(connectionMemory)
	defer C.free(tpbMemory)
	defer C.free(lengthMemory)
	connectionArray := (*[maxArrayElements]*C.ib_connection)(connectionMemory)[:len(connections):len(connections)]
	tpbArray := (*[maxArrayElements]*C.char)(tpbMemory)[:len(tpbs):len(tpbs)]
	lengthArray := (*[maxArrayElements]C.size_t)(lengthMemory)[:len(tpbs):len(tpbs)]
	defer func() {
		for _, pointer := range tpbArray {
			C.free(unsafe.Pointer(pointer))
		}
	}()
	for index, connection := range connections {
		if connection == nil || connection.ptr == nil {
			return nil, errors.New("interbase: native distributed participant is unavailable")
		}
		if len(tpbs[index]) == 0 {
			return nil, errors.New("interbase: transaction parameter block is empty")
		}
		tpbMemory := C.CBytes(tpbs[index])
		if tpbMemory == nil {
			return nil, errors.New("interbase: out of memory copying transaction parameter block")
		}
		connectionArray[index] = connection.ptr
		tpbArray[index] = (*C.char)(tpbMemory)
		lengthArray[index] = C.size_t(len(tpbs[index]))
	}
	var errorPointer *C.char
	transaction := C.ib_distributed_begin((**C.ib_connection)(connectionMemory),
		(**C.char)(tpbMemory), (*C.size_t)(lengthMemory), C.size_t(len(connections)),
		&errorPointer)
	if transaction == nil {
		return nil, takeNativeError(errorPointer)
	}
	native := &nativeDistributedTransaction{ptr: transaction,
		participants: make([]*nativeTransaction, len(connections))}
	for index := range native.participants {
		participant := C.ib_distributed_participant(transaction, C.size_t(index))
		if participant == nil {
			native.free()
			return nil, errors.New("interbase: native distributed participant is unavailable")
		}
		native.participants[index] = &nativeTransaction{ptr: participant}
	}
	if errorPointer != nil {
		return native, takeNativeError(errorPointer)
	}
	return native, nil
}

func (t *nativeDistributedTransaction) prepare(message []byte) error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.prepareOverride != nil {
		return t.prepareOverride(append([]byte(nil), message...))
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native distributed transaction is unavailable")
	}
	var messagePointer *C.char
	if len(message) != 0 {
		messagePointer = (*C.char)(C.CBytes(message))
		if messagePointer == nil {
			return errors.New("interbase: out of memory copying distributed recovery message")
		}
		defer C.free(unsafe.Pointer(messagePointer))
	}
	var errorPointer *C.char
	if result := C.ib_distributed_prepare(t.ptr, messagePointer, C.size_t(len(message)), &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeDistributedTransaction) commit() error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.commitOverride != nil {
		return t.commitOverride()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native distributed transaction is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_distributed_commit(t.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeDistributedTransaction) rollback() error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.rollbackOverride != nil {
		return t.rollbackOverride()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native distributed transaction is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_distributed_rollback(t.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeDistributedTransaction) handleState() nativeHandleState {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.handleStateOverride != nil {
		return t.handleStateOverride()
	}
	if t == nil || t.ptr == nil {
		if t != nil && (t.prepareOverride != nil || t.commitOverride != nil ||
			t.rollbackOverride != nil) {
			return nativeHandleLive
		}
		return nativeHandleConsumed
	}
	return nativeHandleState(C.ib_distributed_handle_state(t.ptr))
}

func (t *nativeDistributedTransaction) free() {
	if t == nil {
		return
	}
	release := nativegate.Global.Enter()
	defer release()
	if t.freeOverride != nil {
		t.freeOverride()
		t.ptr = nil
		t.participants = nil
		return
	}
	if t.ptr != nil {
		C.ib_distributed_free(t.ptr)
		t.ptr = nil
	}
	t.participants = nil
}

func (t *nativeTransaction) commit() error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.commitOverride != nil {
		return t.commitOverride()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native transaction is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_transaction_commit(t.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeTransaction) rollback() error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.rollbackOverride != nil {
		return t.rollbackOverride()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native transaction is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_transaction_rollback(t.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeTransaction) rollbackCleanup() (error, nativeHandleState) {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.rollbackCleanupOverride != nil {
		return t.rollbackCleanupOverride()
	}
	if t != nil && t.rollbackOverride != nil {
		err := t.rollbackOverride()
		return err, t.handleState()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native transaction is unavailable"), nativeHandleConsumed
	}
	var errorPointer *C.char
	result := C.ib_transaction_rollback_cleanup(t.ptr, &errorPointer)
	if result != 0 {
		return takeNativeError(errorPointer), t.handleState()
	}
	return nil, t.handleState()
}

func (t *nativeTransaction) handleState() nativeHandleState {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.handleStateOverride != nil {
		return t.handleStateOverride()
	}
	if t == nil || t.ptr == nil {
		return nativeHandleConsumed
	}
	return nativeHandleState(C.ib_transaction_handle_state(t.ptr))
}

func (t *nativeTransaction) commitRetaining() error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.commitRetainOverride != nil {
		return t.commitRetainOverride()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native transaction is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_transaction_commit_retaining(t.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeTransaction) rollbackRetaining() error {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.rollbackRetainOverride != nil {
		return t.rollbackRetainOverride()
	}
	if t == nil || t.ptr == nil {
		return errors.New("interbase: native transaction is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_transaction_rollback_retaining(t.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (t *nativeTransaction) info(item byte) ([]byte, error) {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.infoOverride != nil {
		response, err := t.infoOverride(item)
		return append([]byte(nil), response...), err
	}
	if t == nil || t.ptr == nil {
		return nil, errors.New("interbase: native transaction is unavailable")
	}
	var responsePointer *C.char
	var responseLength C.size_t
	var errorPointer *C.char
	if result := C.ib_transaction_info(t.ptr, C.uint8_t(item), &responsePointer,
		&responseLength, &errorPointer); result != 0 {
		return nil, takeNativeError(errorPointer)
	}
	if responsePointer == nil {
		return nil, errors.New("native information response is unavailable")
	}
	defer C.free(unsafe.Pointer(responsePointer))
	if uint64(responseLength) > uint64(math.MaxInt32) {
		return nil, errors.New("native information response is too long")
	}
	return C.GoBytes(unsafe.Pointer(responsePointer), C.int(responseLength)), nil
}

func (t *nativeTransaction) transactionInfo(item byte) ([]byte, error) {
	return t.info(item)
}

func (t *nativeTransaction) query(query string, args []argument, allowArrays bool) (*nativeCursor, []string, error) {
	release := nativegate.Global.Enter()
	defer release()
	if t == nil || t.ptr == nil {
		return nil, nil, errors.New("interbase: native transaction is unavailable")
	}
	bindings, err := newNativeBindings(args)
	if err != nil {
		return nil, nil, err
	}
	defer C.ib_bindings_free(bindings)
	queryPointer := C.CString(query)
	defer C.free(unsafe.Pointer(queryPointer))
	var errorPointer *C.char
	cursor := C.ib_transaction_query(t.ptr, queryPointer, C.size_t(len(query)),
		bindings, C.int(boolToInt(allowArrays)), &errorPointer)
	if cursor == nil {
		return nil, nil, takeNativeError(errorPointer)
	}
	return nativeCursorFromPointer(cursor, nil)
}

func (t *nativeTransaction) exec(query string, args []argument, allowArrays bool) (int64, error) {
	release := nativegate.Global.Enter()
	defer release()
	if t == nil || t.ptr == nil {
		return 0, errors.New("interbase: native transaction is unavailable")
	}
	bindings, err := newNativeBindings(args)
	if err != nil {
		return 0, err
	}
	defer C.ib_bindings_free(bindings)
	queryPointer := C.CString(query)
	defer C.free(unsafe.Pointer(queryPointer))
	var affected C.int64_t
	var errorPointer *C.char
	if result := C.ib_transaction_exec(t.ptr, queryPointer, C.size_t(len(query)),
		bindings, &affected, C.int(boolToInt(allowArrays)), &errorPointer); result != 0 {
		return 0, takeNativeError(errorPointer)
	}
	return int64(affected), nil
}

func (t *nativeTransaction) prepare(query string, allowArrays bool) (*nativeStatement, error) {
	release := nativegate.Global.Enter()
	defer release()
	if t == nil || t.ptr == nil {
		return nil, errors.New("interbase: native transaction is unavailable")
	}
	queryPointer := C.CString(query)
	defer C.free(unsafe.Pointer(queryPointer))
	var errorPointer *C.char
	statement := C.ib_transaction_prepare(t.ptr, queryPointer, C.size_t(len(query)),
		C.int(boolToInt(allowArrays)), &errorPointer)
	if statement == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &nativeStatement{ptr: statement}, nil
}

func (t *nativeTransaction) openBlob(high int32, low uint32, subtype, charset int16) (*blobStream, error) {
	release := nativegate.Global.Enter()
	defer release()
	if t != nil && t.openBlobOverride != nil {
		return t.openBlobOverride(high, low, subtype, charset)
	}
	if t == nil || t.ptr == nil {
		return nil, errors.New("interbase: native transaction is unavailable")
	}
	var errorPointer *C.char
	reader := C.ib_transaction_blob_open2(t.ptr, C.int32_t(high), C.uint32_t(low),
		C.int(subtype), C.int(charset), &errorPointer)
	if reader == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &blobStream{reader: reader}, nil
}

func (t *nativeTransaction) createBlob(subtype, charset int16) (*blobStream, error) {
	release := nativegate.Global.Enter()
	defer release()
	if t == nil || t.ptr == nil {
		return nil, errors.New("interbase: native transaction is unavailable")
	}
	var errorPointer *C.char
	writer := C.ib_transaction_blob_create(t.ptr, C.int(subtype), C.int(charset), &errorPointer)
	if writer == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &blobStream{writer: writer}, nil
}

func (t *nativeTransaction) free() {
	if t == nil {
		return
	}
	release := nativegate.Global.Enter()
	defer release()
	if t.freeOverride != nil {
		free := t.freeOverride
		t.freeOverride = nil
		free()
		t.ptr = nil
		return
	}
	if t.ptr == nil {
		return
	}
	C.ib_transaction_free(t.ptr)
	t.ptr = nil
}

type nativeStatement struct {
	ptr              *C.ib_statement
	execOverride     func([]argument) (int64, error)
	closeOverride    func() error
	numInputOverride func() int
}

type nativeCursor struct {
	ptr                 *C.ib_cursor
	closeOverride       func() error
	abortOverride       func() error
	statement           *nativeStatement
	explicitTransaction bool
	directTransaction   *Transaction
	metadata            []columnMetadata
}

type nativeBlobValue struct {
	high    int32
	low     uint32
	subtype int16
	charset int16
}

type blobStream struct {
	tx                  *Transaction
	generation          uint64
	reader              *C.ib_blob_reader
	writer              *C.ib_blob_writer
	closed              bool
	closeNativeOverride func(bool) (nativeBlobValue, error)
}

func (c *nativeConnection) openBlob(high int32, low uint32, subtype int16,
	charset int16) (*blobStream, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return nil, errors.New("interbase: native connection is unavailable")
	}
	var errorPointer *C.char
	reader := C.ib_blob_open2(c.ptr, C.int32_t(high), C.uint32_t(low),
		C.int(subtype), C.int(charset), &errorPointer)
	if reader == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &blobStream{reader: reader}, nil
}

func (c *nativeConnection) createBlob(subtype, charset int16) (*blobStream, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return nil, errors.New("interbase: native connection is unavailable")
	}
	var errorPointer *C.char
	writer := C.ib_blob_create(c.ptr, C.int(subtype), C.int(charset), &errorPointer)
	if writer == nil {
		return nil, takeNativeError(errorPointer)
	}
	return &blobStream{writer: writer}, nil
}

type columnMetadata struct {
	databaseTypeName string
	scanType         reflect.Type
	length           int64
	hasLength        bool
	nullable         bool
	hasNullable      bool
	precision        int64
	scale            int64
	hasPrecision     bool
}

func openNative(cfg Config) (*nativeConnection, error) {
	return openNativeContext(context.Background(), cfg)
}

func openNativeContext(ctx context.Context, cfg Config) (*nativeConnection, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	dialect, err := normalizeDialect(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	connectTimeout, err := normalizeConnectTimeout(cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	attachment, err := buildAttachment(cfg)
	if err != nil {
		return nil, err
	}
	database := C.CString(attachment)
	defer C.free(unsafe.Pointer(database))
	user := C.CString(cfg.User)
	defer C.free(unsafe.Pointer(user))
	password := C.CString(cfg.Password)
	defer C.free(unsafe.Pointer(password))
	role := C.CString(cfg.Role)
	defer C.free(unsafe.Pointer(role))
	encryptedPassword := C.CString(cfg.EncryptedPassword)
	defer C.free(unsafe.Pointer(encryptedPassword))
	systemEncryptionPassword := C.CString(cfg.SystemEncryptionPassword)
	defer C.free(unsafe.Pointer(systemEncryptionPassword))
	charset, err := normalizeCharset(cfg.Charset)
	if err != nil {
		return nil, err
	}
	readOnlyTPB, err := buildTPB(driver.TxOptions{ReadOnly: true}, cfg.TransactionOptions)
	if err != nil {
		return nil, err
	}
	writeTPB, err := buildTPB(driver.TxOptions{}, cfg.TransactionOptions)
	if err != nil {
		return nil, err
	}
	charsetPointer := C.CString(charset)
	defer C.free(unsafe.Pointer(charsetPointer))

	release, err := nativegate.Global.EnterExclusiveContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var errorPointer *C.char
	connection := C.ib_connection_open(
		database,
		C.size_t(len(attachment)),
		user,
		C.size_t(len(cfg.User)),
		password,
		C.size_t(len(cfg.Password)),
		role,
		C.size_t(len(cfg.Role)),
		encryptedPassword,
		C.size_t(len(cfg.EncryptedPassword)),
		systemEncryptionPassword,
		C.size_t(len(cfg.SystemEncryptionPassword)),
		charsetPointer,
		C.size_t(len(charset)),
		C.int(dialect),
		C.uint32_t(uint64(connectTimeout/time.Second)),
		&errorPointer,
	)
	if connection == nil {
		return nil, takeNativeError(errorPointer)
	}
	var defaultsError *C.char
	if result := C.ib_connection_set_default_tpbs(connection,
		(*C.char)(unsafe.Pointer(&readOnlyTPB[0])), C.size_t(len(readOnlyTPB)),
		(*C.char)(unsafe.Pointer(&writeTPB[0])), C.size_t(len(writeTPB)),
		&defaultsError); result != 0 {
		firstErr := takeNativeError(defaultsError)
		var closeError *C.char
		if closeResult := C.ib_connection_close(connection, &closeError); closeResult != 0 {
			firstErr = errors.Join(firstErr, takeNativeError(closeError))
		}
		return nil, firstErr
	}
	return &nativeConnection{ptr: connection, dialect: dialect}, nil
}

func createNative(cfg Config, pageSize int) (*nativeConnection, error) {
	return createNativeContext(context.Background(), cfg, pageSize)
}

func createNativeContext(ctx context.Context, cfg Config, pageSize int) (*nativeConnection, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	dialect, err := normalizeDialect(cfg.Dialect)
	if err != nil {
		return nil, err
	}
	connectTimeout, err := normalizeConnectTimeout(cfg.ConnectTimeout)
	if err != nil {
		return nil, err
	}
	attachment, err := buildAttachment(cfg)
	if err != nil {
		return nil, err
	}
	charset, err := normalizeCharset(cfg.Charset)
	if err != nil {
		return nil, err
	}
	readOnlyTPB, err := buildTPB(driver.TxOptions{ReadOnly: true}, cfg.TransactionOptions)
	if err != nil {
		return nil, err
	}
	writeTPB, err := buildTPB(driver.TxOptions{}, cfg.TransactionOptions)
	if err != nil {
		return nil, err
	}
	database := C.CString(attachment)
	defer C.free(unsafe.Pointer(database))
	user := C.CString(cfg.User)
	defer C.free(unsafe.Pointer(user))
	password := C.CString(cfg.Password)
	defer C.free(unsafe.Pointer(password))
	role := C.CString(cfg.Role)
	defer C.free(unsafe.Pointer(role))
	encryptedPassword := C.CString(cfg.EncryptedPassword)
	defer C.free(unsafe.Pointer(encryptedPassword))
	systemEncryptionPassword := C.CString(cfg.SystemEncryptionPassword)
	defer C.free(unsafe.Pointer(systemEncryptionPassword))
	charsetPointer := C.CString(charset)
	defer C.free(unsafe.Pointer(charsetPointer))

	release, err := nativegate.Global.EnterExclusiveContext(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	var errorPointer *C.char
	connection := C.ib_connection_create(
		database,
		C.size_t(len(attachment)),
		user,
		C.size_t(len(cfg.User)),
		password,
		C.size_t(len(cfg.Password)),
		role,
		C.size_t(len(cfg.Role)),
		encryptedPassword,
		C.size_t(len(cfg.EncryptedPassword)),
		systemEncryptionPassword,
		C.size_t(len(cfg.SystemEncryptionPassword)),
		charsetPointer,
		C.size_t(len(charset)),
		C.int(dialect),
		C.int(pageSize),
		C.uint32_t(uint64(connectTimeout/time.Second)),
		&errorPointer,
	)
	if connection == nil {
		return nil, takeNativeError(errorPointer)
	}
	native := &nativeConnection{ptr: connection, dialect: dialect}
	var defaultsError *C.char
	if result := C.ib_connection_set_default_tpbs(connection,
		(*C.char)(unsafe.Pointer(&readOnlyTPB[0])), C.size_t(len(readOnlyTPB)),
		(*C.char)(unsafe.Pointer(&writeTPB[0])), C.size_t(len(writeTPB)),
		&defaultsError); result != 0 {
		firstErr := takeNativeError(defaultsError)
		return native, firstErr
	}
	return native, nil
}

func (c *nativeConnection) close() error {
	if c == nil {
		return nil
	}
	release := nativegate.Global.EnterExclusive()
	defer release()
	if c.closeOverride != nil {
		close := c.closeOverride
		c.closeOverride = nil
		c.ptr = nil
		return close()
	}
	if c.ptr == nil {
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

func (c *nativeConnection) drop() (error, bool) {
	return c.dropContext(context.Background())
}

func (c *nativeConnection) dropContext(ctx context.Context) (error, bool) {
	if c == nil {
		return errors.New("native connection is unavailable"), false
	}
	release, err := nativegate.Global.EnterExclusiveContext(ctx)
	if err != nil {
		return err, false
	}
	defer release()
	if err := contextError(ctx); err != nil {
		return err, false
	}
	if c != nil && c.dropOverride != nil {
		return c.dropOverride(), c.dropConsumedOverride
	}
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable"), false
	}
	var errorPointer *C.char
	var consumed C.int
	if result := C.ib_connection_drop(c.ptr, &consumed, &errorPointer); result != 0 {
		if consumed != 0 {
			c.ptr = nil
		}
		return takeNativeError(errorPointer), consumed != 0
	}
	c.ptr = nil
	return nil, consumed != 0
}

func (c *nativeConnection) clientVersion() (string, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.clientVersionOverride != nil {
		return c.clientVersionOverride()
	}
	var versionPointer *C.char
	var errorPointer *C.char
	if result := C.ib_client_version(&versionPointer, &errorPointer); result != 0 {
		return "", takeNativeError(errorPointer)
	}
	if versionPointer == nil {
		return "", errors.New("native client version is unavailable")
	}
	defer C.free(unsafe.Pointer(versionPointer))
	return C.GoString(versionPointer), nil
}

func (c *nativeConnection) broken() bool {
	if c != nil && c.brokenOverride != nil {
		return c.brokenOverride()
	}
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
	release := nativegate.Global.Enter()
	defer release()
	if s != nil && s.numInputOverride != nil {
		return s.numInputOverride()
	}
	if s == nil || s.ptr == nil {
		return 0
	}
	return int(C.ib_statement_num_input(s.ptr))
}

func (s *nativeStatement) exec(args []argument) (int64, error) {
	release := nativegate.Global.Enter()
	defer release()
	if s != nil && s.execOverride != nil {
		return s.execOverride(append([]argument(nil), args...))
	}
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
	release := nativegate.Global.Enter()
	defer release()
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
	nativeRows := &nativeCursor{
		ptr:                 cursor,
		statement:           s,
		explicitTransaction: C.ib_cursor_uses_connection_transaction(cursor) != 0,
	}
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
	if _, err := nativeRows.columnMetadata(); err != nil {
		return nil, nil, nativeCursorQueryError(nativeRows, err)
	}
	return nativeRows, columns, nil
}

func nativeCursorFromPointer(cursor *C.ib_cursor, statement *nativeStatement) (*nativeCursor, []string, error) {
	if cursor == nil {
		return nil, nil, errors.New("native cursor is unavailable")
	}
	nativeRows := &nativeCursor{
		ptr:                 cursor,
		statement:           statement,
		explicitTransaction: C.ib_cursor_uses_connection_transaction(cursor) != 0,
	}
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
	if _, err := nativeRows.columnMetadata(); err != nil {
		return nil, nil, nativeCursorQueryError(nativeRows, err)
	}
	return nativeRows, columns, nil
}

func scaledIntegerDatabaseTypeName(subtype int) string {
	if subtype == 2 {
		return "DECIMAL"
	}
	return "NUMERIC"
}

func columnMetadataFromNative(value C.ib_column_metadata) columnMetadata {
	metadata := columnMetadata{
		scanType:     reflect.TypeOf((*any)(nil)).Elem(),
		length:       int64(value.length),
		precision:    int64(value.precision),
		scale:        int64(value.scale),
		hasLength:    value.has_length != 0,
		hasNullable:  value.has_nullable != 0,
		nullable:     value.nullable != 0,
		hasPrecision: value.has_precision_scale != 0,
	}
	if metadata.scale < 0 {
		metadata.scale = -metadata.scale
	}

	switch int(value.sql_type) {
	case int(C.IB_METADATA_CHAR):
		metadata.databaseTypeName = "CHAR"
		if int(value.sql_subtype) == 1 {
			metadata.scanType = reflect.TypeOf([]byte(nil))
		} else {
			metadata.scanType = reflect.TypeOf("")
		}
	case int(C.IB_METADATA_VARCHAR):
		metadata.databaseTypeName = "VARCHAR"
		if int(value.sql_subtype) == 1 {
			metadata.scanType = reflect.TypeOf([]byte(nil))
		} else {
			metadata.scanType = reflect.TypeOf("")
		}
	case int(C.IB_METADATA_SMALLINT):
		metadata.databaseTypeName = "SMALLINT"
		if value.sql_scale != 0 || value.sql_subtype == 1 || value.sql_subtype == 2 || metadata.hasPrecision {
			metadata.databaseTypeName = scaledIntegerDatabaseTypeName(int(value.sql_subtype))
		}
		if value.sql_scale != 0 {
			metadata.scanType = reflect.TypeOf("")
		} else {
			metadata.scanType = reflect.TypeOf(int64(0))
		}
	case int(C.IB_METADATA_INTEGER):
		metadata.databaseTypeName = "INTEGER"
		if value.sql_scale != 0 || value.sql_subtype == 1 || value.sql_subtype == 2 || metadata.hasPrecision {
			metadata.databaseTypeName = scaledIntegerDatabaseTypeName(int(value.sql_subtype))
		}
		if value.sql_scale != 0 {
			metadata.scanType = reflect.TypeOf("")
		} else {
			metadata.scanType = reflect.TypeOf(int64(0))
		}
	case int(C.IB_METADATA_BIGINT):
		metadata.databaseTypeName = "BIGINT"
		if value.sql_scale != 0 || value.sql_subtype == 1 || value.sql_subtype == 2 || metadata.hasPrecision {
			metadata.databaseTypeName = scaledIntegerDatabaseTypeName(int(value.sql_subtype))
		}
		if value.sql_scale != 0 {
			metadata.scanType = reflect.TypeOf("")
		} else {
			metadata.scanType = reflect.TypeOf(int64(0))
		}
	case int(C.IB_METADATA_FLOAT):
		metadata.databaseTypeName = "FLOAT"
		metadata.scanType = reflect.TypeOf(float64(0))
	case int(C.IB_METADATA_DOUBLE):
		metadata.databaseTypeName = "DOUBLE PRECISION"
		metadata.scanType = reflect.TypeOf(float64(0))
	case int(C.IB_METADATA_TIMESTAMP):
		metadata.databaseTypeName = "TIMESTAMP"
		metadata.scanType = reflect.TypeOf(time.Time{})
	case int(C.IB_METADATA_DATE):
		metadata.databaseTypeName = "DATE"
		metadata.scanType = reflect.TypeOf(time.Time{})
	case int(C.IB_METADATA_TIME):
		metadata.databaseTypeName = "TIME"
		metadata.scanType = reflect.TypeOf(time.Time{})
	case int(C.IB_METADATA_BOOLEAN):
		metadata.databaseTypeName = "BOOLEAN"
		metadata.scanType = reflect.TypeOf(false)
	case int(C.IB_METADATA_BLOB):
		metadata.databaseTypeName = "BLOB"
		if int(value.sql_subtype) == 1 {
			metadata.scanType = reflect.TypeOf("")
		} else {
			metadata.scanType = reflect.TypeOf([]byte(nil))
		}
	case int(C.IB_METADATA_ARRAY):
		metadata.databaseTypeName = "ARRAY"
		metadata.scanType = reflect.TypeOf(Array{})
	}
	return metadata
}

func (c *nativeCursor) columnMetadata() ([]columnMetadata, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return nil, errors.New("native cursor is unavailable")
	}
	count := C.ib_cursor_column_count(c.ptr)
	if uint64(count) > uint64(maxInt()) {
		return nil, errors.New("result has too many columns")
	}
	metadata := make([]columnMetadata, int(count))
	for index := range metadata {
		var nativeMetadata C.ib_column_metadata
		var errorPointer *C.char
		if result := C.ib_cursor_column_metadata(c.ptr, C.size_t(index),
			&nativeMetadata, &errorPointer); result != 0 {
			return nil, takeNativeError(errorPointer)
		}
		metadata[index] = columnMetadataFromNative(nativeMetadata)
	}
	c.metadata = metadata
	return metadata, nil
}

func (s *nativeStatement) close() error {
	if s != nil && s.closeOverride != nil {
		release := nativegate.Global.Enter()
		defer release()
		close := s.closeOverride
		s.closeOverride = nil
		s.ptr = nil
		return close()
	}
	if s == nil || s.ptr == nil {
		return nil
	}
	release := nativegate.Global.Enter()
	defer release()
	statement := s.ptr
	s.ptr = nil
	var errorPointer *C.char
	if result := C.ib_statement_close(statement, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (s *nativeStatement) plan() (string, error) {
	release := nativegate.Global.Enter()
	defer release()
	if s == nil || s.ptr == nil {
		return "", errors.New("native statement is unavailable")
	}
	var planPointer *C.char
	var planLength C.size_t
	var errorPointer *C.char
	if result := C.ib_statement_plan(s.ptr, &planPointer, &planLength, &errorPointer); result != 0 {
		return "", takeNativeError(errorPointer)
	}
	if planPointer == nil {
		return "", errors.New("native statement plan is unavailable")
	}
	defer C.free(unsafe.Pointer(planPointer))
	if uint64(planLength) > uint64(math.MaxInt32) {
		return "", errors.New("native statement plan is too long")
	}
	return string(C.GoBytes(unsafe.Pointer(planPointer), C.int(planLength))), nil
}

func (c *nativeConnection) query(query string, args []argument, allowArrays bool) (*nativeCursor, []string, error) {
	release := nativegate.Global.Enter()
	defer release()
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
	cursor := C.ib_connection_query(c.ptr, queryPointer, C.size_t(len(query)),
		bindings, C.int(boolToInt(allowArrays)), &cursorError)
	if cursor == nil {
		return nil, nil, takeNativeError(cursorError)
	}
	nativeRows := &nativeCursor{
		ptr:                 cursor,
		explicitTransaction: C.ib_cursor_uses_connection_transaction(cursor) != 0,
	}

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
	if _, err := nativeRows.columnMetadata(); err != nil {
		return nil, nil, nativeCursorQueryError(nativeRows, err)
	}
	return nativeRows, columns, nil
}

func (c *nativeConnection) exec(query string, args []argument, allowArrays bool) (int64, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.execOverride != nil {
		return c.execOverride(query, append([]argument(nil), args...), allowArrays)
	}
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
		bindings, &affected, C.int(boolToInt(allowArrays)), &execError); result != 0 {
		return 0, takeNativeError(execError)
	}
	return int64(affected), nil
}

func (c *nativeConnection) begin(tpb []byte) error {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	if len(tpb) == 0 {
		return errors.New("transaction parameter block is empty")
	}
	var errorPointer *C.char
	if result := C.ib_connection_begin_tpb(c.ptr,
		(*C.char)(unsafe.Pointer(&tpb[0])), C.size_t(len(tpb)), &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) commit() error {
	release := nativegate.Global.Enter()
	defer release()
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
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_connection_rollback(c.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) rollbackCleanup() (error, nativeHandleState) {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable"), nativeHandleConsumed
	}
	var errorPointer *C.char
	result := C.ib_connection_rollback_cleanup(c.ptr, &errorPointer)
	if result != 0 {
		return takeNativeError(errorPointer), c.transactionState()
	}
	return nil, c.transactionState()
}

func (c *nativeConnection) transactionState() nativeHandleState {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return nativeHandleConsumed
	}
	return nativeHandleState(C.ib_connection_transaction_state(c.ptr))
}

func (c *nativeConnection) commitRetaining() error {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.commitRetainingOverride != nil {
		return c.commitRetainingOverride()
	}
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_connection_commit_retaining(c.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) rollbackRetaining() error {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.rollbackRetainingOverride != nil {
		return c.rollbackRetainingOverride()
	}
	if c == nil || c.ptr == nil {
		return errors.New("native connection is unavailable")
	}
	var errorPointer *C.char
	if result := C.ib_connection_rollback_retaining(c.ptr, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeConnection) databaseInfo(item byte) ([]byte, error) {
	return c.info(item, false)
}

func (c *nativeConnection) transactionInfo(item byte) ([]byte, error) {
	return c.info(item, true)
}

func (c *nativeConnection) info(item byte, transaction bool) ([]byte, error) {
	release := nativegate.Global.Enter()
	defer release()
	if !transaction && c != nil && c.databaseInfoOverride != nil {
		response, err := c.databaseInfoOverride(item)
		return append([]byte(nil), response...), err
	}
	if c == nil || c.ptr == nil {
		return nil, errors.New("native connection is unavailable")
	}
	var responsePointer *C.char
	var responseLength C.size_t
	var errorPointer *C.char
	var result C.int
	if transaction {
		result = C.ib_connection_transaction_info(c.ptr, C.uint8_t(item),
			&responsePointer, &responseLength, &errorPointer)
	} else {
		result = C.ib_connection_database_info(c.ptr, C.uint8_t(item),
			&responsePointer, &responseLength, &errorPointer)
	}
	if result != 0 {
		return nil, takeNativeError(errorPointer)
	}
	if responsePointer == nil {
		return nil, errors.New("native information response is unavailable")
	}
	defer C.free(unsafe.Pointer(responsePointer))
	if uint64(responseLength) > uint64(math.MaxInt32) {
		return nil, errors.New("native information response is too long")
	}
	return C.GoBytes(unsafe.Pointer(responsePointer), C.int(responseLength)), nil
}

func (c *nativeConnection) prepare(query string) (*nativeStatement, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c != nil && c.prepareOverride != nil {
		return c.prepareOverride(query)
	}
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
			C.int64_t(year), C.int(month), C.int(day),
			C.int(hour), C.int(minute), C.int(second),
			C.int(arg.timeValue.Nanosecond()),
			&errorPointer,
		)
	case argumentArray:
		return setNativeArrayBinding(bindings, index, arg.array)
	case argumentBlobRef:
		result = C.ib_bindings_set_blob_ref(bindings, C.size_t(index),
			C.int32_t(arg.blobRef.high), C.uint32_t(arg.blobRef.low),
			C.int(arg.blobRef.subtype), C.int(arg.blobRef.charset), &errorPointer)
	default:
		return errors.New("unsupported native query argument")
	}
	if result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func setNativeArrayBinding(bindings *C.ib_bindings, index int, array *Array) error {
	if array == nil {
		return errors.New("interbase: nil native array")
	}
	boundPointer := (*C.ib_array_bound)(C.malloc(
		C.size_t(len(array.Bounds)) * C.size_t(unsafe.Sizeof(C.ib_array_bound{}))))
	if boundPointer == nil {
		return errors.New("interbase: out of memory copying native array bounds")
	}
	defer C.free(unsafe.Pointer(boundPointer))
	bounds := unsafe.Slice(boundPointer, len(array.Bounds))
	for index, bound := range array.Bounds {
		bounds[index].lower = C.int32_t(bound.Lower)
		bounds[index].upper = C.int32_t(bound.Upper)
	}

	elementPointer := (*C.ib_array_element)(C.calloc(
		C.size_t(len(array.Elements)), C.size_t(unsafe.Sizeof(C.ib_array_element{}))))
	if elementPointer == nil {
		return errors.New("interbase: out of memory copying native array elements")
	}
	defer C.free(unsafe.Pointer(elementPointer))
	elements := unsafe.Slice(elementPointer, len(array.Elements))
	bytePointers := make([]unsafe.Pointer, 0, len(array.Elements))
	defer func() {
		for _, pointer := range bytePointers {
			C.free(pointer)
		}
	}()
	for elementIndex, value := range array.Elements {
		native := &elements[elementIndex]
		switch typed := value.(type) {
		case string:
			native.kind = C.int(C.IB_ARRAY_ELEMENT_STRING)
			if len(typed) != 0 {
				pointer := C.CBytes([]byte(typed))
				if pointer == nil {
					return errors.New("interbase: out of memory copying native array string")
				}
				bytePointers = append(bytePointers, pointer)
				native.bytes = (*C.char)(pointer)
			}
			native.length = C.size_t(len(typed))
		case []byte:
			native.kind = C.int(C.IB_ARRAY_ELEMENT_BYTES)
			if len(typed) != 0 {
				pointer := C.CBytes(typed)
				if pointer == nil {
					return errors.New("interbase: out of memory copying native array bytes")
				}
				bytePointers = append(bytePointers, pointer)
				native.bytes = (*C.char)(pointer)
			}
			native.length = C.size_t(len(typed))
		case int64:
			native.kind = C.int(C.IB_ARRAY_ELEMENT_INT64)
			native.int64_value = C.int64_t(typed)
		case float64:
			native.kind = C.int(C.IB_ARRAY_ELEMENT_FLOAT64)
			native.float64_value = C.double(typed)
		case bool:
			native.kind = C.int(C.IB_ARRAY_ELEMENT_BOOL)
			if typed {
				native.bool_value = C.int(1)
			}
		case time.Time:
			native.kind = C.int(C.IB_ARRAY_ELEMENT_TIMESTAMP)
			native.year = C.int64_t(typed.Year())
			native.month = C.int(typed.Month())
			native.day = C.int(typed.Day())
			native.hour = C.int(typed.Hour())
			native.minute = C.int(typed.Minute())
			native.second = C.int(typed.Second())
			native.nanosecond = C.int(typed.Nanosecond())
		default:
			return fmt.Errorf("interbase: unsupported native array element %T", value)
		}
	}
	var errorPointer *C.char
	if result := C.ib_bindings_set_array(bindings, C.size_t(index), boundPointer,
		C.size_t(len(array.Bounds)), elementPointer, C.size_t(len(array.Elements)),
		&errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (b *blobStream) readLocked(buffer []byte) (int, bool, error) {
	release := nativegate.Global.Enter()
	defer release()
	if b == nil || b.reader == nil || b.closed {
		return 0, false, errors.New("interbase: BLOB reader is closed")
	}
	if len(buffer) == 0 {
		return 0, false, nil
	}
	var length C.size_t
	var eof C.int
	var errorPointer *C.char
	if result := C.ib_blob_reader_read(b.reader, (*C.char)(unsafe.Pointer(&buffer[0])),
		C.size_t(len(buffer)), &length, &eof, &errorPointer); result != 0 {
		return 0, false, takeNativeError(errorPointer)
	}
	if uint64(length) > uint64(len(buffer)) {
		return 0, false, errors.New("interbase: native BLOB reader returned too much data")
	}
	return int(length), eof != 0, nil
}

func (b *blobStream) writeLocked(data []byte) error {
	release := nativegate.Global.Enter()
	defer release()
	if b == nil || b.writer == nil || b.closed {
		return errors.New("interbase: BLOB writer is closed")
	}
	if len(data) == 0 {
		return nil
	}
	var errorPointer *C.char
	if result := C.ib_blob_writer_write(b.writer, (*C.char)(unsafe.Pointer(&data[0])),
		C.size_t(len(data)), &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (b *blobStream) closeNativeLocked(cancel bool) (nativeBlobValue, error) {
	release := nativegate.Global.Enter()
	defer release()
	if b == nil {
		return nativeBlobValue{}, errors.New("interbase: BLOB stream is unavailable")
	}
	if b.closed {
		return nativeBlobValue{}, nil
	}
	b.closed = true
	if b.closeNativeOverride != nil {
		return b.closeNativeOverride(cancel)
	}
	if b.reader != nil {
		reader := b.reader
		b.reader = nil
		var errorPointer *C.char
		if result := C.ib_blob_reader_close(reader, C.int(boolToInt(cancel)), &errorPointer); result != 0 {
			return nativeBlobValue{}, takeNativeError(errorPointer)
		}
		return nativeBlobValue{}, nil
	}
	if b.writer != nil {
		writer := b.writer
		b.writer = nil
		var high C.int32_t
		var low C.uint32_t
		var subtype C.int
		var charset C.int
		var errorPointer *C.char
		if result := C.ib_blob_writer_close(writer, C.int(boolToInt(cancel)),
			&high, &low, &subtype, &charset, &errorPointer); result != 0 {
			return nativeBlobValue{}, takeNativeError(errorPointer)
		}
		return nativeBlobValue{
			high:    int32(high),
			low:     uint32(low),
			subtype: int16(subtype),
			charset: int16(charset),
		}, nil
	}
	return nativeBlobValue{}, nil
}

func nativeCursorQueryError(cursor *nativeCursor, primary error) error {
	abortErr := cursor.abortUnlocked()
	if abortErr != nil {
		return errors.Join(primary, abortErr)
	}
	return primary
}

func (c *nativeCursor) next() (bool, error) {
	release := nativegate.Global.Enter()
	defer release()
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
	if c == nil {
		return nil
	}
	if c.closeOverride != nil {
		release := nativegate.Global.Enter()
		defer release()
		close := c.closeOverride
		c.closeOverride = nil
		c.ptr = nil
		return close()
	}
	if c.ptr == nil {
		return nil
	}
	release := nativegate.Global.Enter()
	defer release()
	cursor := c.ptr
	c.ptr = nil
	var errorPointer *C.char
	if result := C.ib_cursor_close(cursor, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeCursor) abort() error {
	if c == nil || c.abortOverride != nil || c.ptr == nil {
		return c.abortUnlocked()
	}
	return c.abortUnlocked()
}

func (c *nativeCursor) abortUnlocked() error {
	if c == nil {
		return nil
	}
	if c.abortOverride != nil {
		release := nativegate.Global.Enter()
		defer release()
		abort := c.abortOverride
		c.abortOverride = nil
		c.ptr = nil
		return abort()
	}
	if c.ptr == nil {
		return nil
	}
	release := nativegate.Global.Enter()
	defer release()
	cursor := c.ptr
	c.ptr = nil
	var errorPointer *C.char
	if result := C.ib_cursor_abort(cursor, &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeCursor) setName(name string) error {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return errors.New("native cursor is unavailable")
	}
	namePointer := C.CBytes([]byte(name))
	if namePointer == nil {
		return errors.New("out of memory copying native cursor name")
	}
	defer C.free(namePointer)
	var errorPointer *C.char
	if result := C.ib_cursor_set_name(c.ptr, (*C.char)(namePointer),
		C.size_t(len(name)), &errorPointer); result != 0 {
		return takeNativeError(errorPointer)
	}
	return nil
}

func (c *nativeCursor) indicator(index int) (uint16, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return 0, errors.New("native cursor is unavailable")
	}
	var indicator C.uint16_t
	var errorPointer *C.char
	if result := C.ib_cursor_column_indicator(c.ptr, C.size_t(index),
		&indicator, &errorPointer); result != 0 {
		return 0, takeNativeError(errorPointer)
	}
	return uint16(indicator), nil
}

func (c *nativeCursor) value(index int) (driver.Value, error) {
	release := nativegate.Global.Enter()
	defer release()
	if c == nil || c.ptr == nil {
		return nil, errors.New("native cursor is unavailable")
	}
	var view C.ib_value_view
	var errorPointer *C.char
	if result := C.ib_cursor_column(c.ptr, C.size_t(index), &view, &errorPointer); result != 0 {
		return nil, takeNativeError(errorPointer)
	}

	value, err := nativeValueView(&view)
	if err != nil {
		return nil, err
	}
	if blob, ok := value.(nativeBlobValue); ok {
		if c.directTransaction == nil || c.directTransaction.attachment == nil {
			return nil, errors.New("native BLOB reference has no direct transaction")
		}
		return BlobRef{
			attachment: c.directTransaction.attachment,
			tx:         c.directTransaction,
			generation: c.directTransaction.generation,
			high:       blob.high,
			low:        blob.low,
			subtype:    blob.subtype,
			charset:    blob.charset,
		}, nil
	}
	return value, nil
}

func nativeValueView(view *C.ib_value_view) (driver.Value, error) {
	if view == nil {
		return nil, errors.New("native result value is unavailable")
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
	case nativeValueArray:
		return nativeArrayValue(view.array)
	case nativeValueBlobRef:
		return nativeBlobValue{
			high:    int32(view.blob_high),
			low:     uint32(view.blob_low),
			subtype: int16(view.blob_subtype),
			charset: int16(view.blob_charset),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported native result kind %d", int(view.kind))
	}
}

func nativeArrayValue(array *C.ib_array_view) (Array, error) {
	if array == nil {
		return Array{}, errors.New("native array result is unavailable")
	}
	dimensionCount := int(C.ib_array_view_dimensions(array))
	if dimensionCount <= 0 || dimensionCount > maxDirectArrayDimensions {
		return Array{}, fmt.Errorf("native array result has invalid dimension count %d", dimensionCount)
	}
	result := Array{Bounds: make([]ArrayBound, dimensionCount)}
	for index := range result.Bounds {
		var bound C.ib_array_bound
		var errorPointer *C.char
		if resultCode := C.ib_array_view_bound(array, C.size_t(index), &bound,
			&errorPointer); resultCode != 0 {
			return Array{}, takeNativeError(errorPointer)
		}
		result.Bounds[index] = ArrayBound{
			Lower: int32(bound.lower),
			Upper: int32(bound.upper),
		}
	}
	elementCount := C.ib_array_view_element_count(array)
	if uint64(elementCount) > uint64(maxInt()) {
		return Array{}, errors.New("native array result has too many elements")
	}
	result.Elements = make([]any, int(elementCount))
	for index := range result.Elements {
		var view C.ib_value_view
		var errorPointer *C.char
		if resultCode := C.ib_array_view_element(array, C.size_t(index), &view,
			&errorPointer); resultCode != 0 {
			return Array{}, takeNativeError(errorPointer)
		}
		value, err := nativeValueView(&view)
		if err != nil {
			return Array{}, fmt.Errorf("native array element %d: %w", index, err)
		}
		result.Elements[index] = value
	}
	return result, nil
}

func takeNativeError(errorPointer *C.char) error {
	if errorPointer == nil {
		return &Error{Message: "InterBase native operation failed"}
	}
	message := C.GoString(errorPointer)
	C.ib_error_free(errorPointer)
	if message == "" {
		return &Error{Message: "InterBase native operation failed"}
	}
	return parseNativeError(message)
}

func maxInt() int {
	return int(^uint(0) >> 1)
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
