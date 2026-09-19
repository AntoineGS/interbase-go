package interbase

import (
	"context"
	"database/sql/driver"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"
	"unicode/utf8"
)

// ArrayBound describes one inclusive InterBase array dimension.
//
// InterBase stores array bounds as 16-bit signed values. The direct API keeps
// the public representation at int32 so callers can construct values without
// silently truncating a bound; validation rejects values that cannot be sent
// to the native client.
type ArrayBound struct {
	Lower int32
	Upper int32
}

// Array is a flat InterBase array value. Elements are ordered with the
// rightmost dimension varying fastest, and Bounds are inclusive.
type Array struct {
	Bounds   []ArrayBound
	Elements []any
}

// BlobOptions controls the subtype and character set used by CreateBlob.
type BlobOptions struct {
	Subtype int16
	Charset string
}

// BlobRef is an opaque, transaction-generation-bound InterBase BLOB ID.
// Values are only usable with the transaction that produced them and before
// that transaction completes.
type BlobRef struct {
	attachment *Attachment
	tx         *Transaction
	generation uint64
	high       int32
	low        uint32
	subtype    int16
	charset    int16
}

var (
	errDirectAttachmentClosed = errors.New("interbase: direct attachment is closed")
	ErrAttachmentBusy         = errors.New("interbase: direct attachment has active resources")
	// ErrDistributedParticipantManaged indicates that a participant transaction
	// can execute work but its completion is owned by its coordinator.
	ErrDistributedParticipantManaged = errors.New("interbase: distributed transaction participant is coordinator-managed")
	errDirectCursorClosed            = errors.New("interbase: direct cursor is closed")
	errDirectCursorNotReady          = errors.New("interbase: direct cursor has no current row")
	errDirectCursorNamed             = errors.New("interbase: direct cursor name is already set")
	errDirectBlobRefInvalid          = errors.New("interbase: direct BLOB reference is invalid")
	errDirectOpenBlobStreams         = errors.New("interbase: retaining completion requires all BLOB streams to be closed")
)

func nativeHandleStateError(operation string, state nativeHandleState) error {
	if state == nativeHandleLive {
		return fmt.Errorf("interbase: %s left a live native transaction handle", operation)
	}
	return fmt.Errorf("interbase: %s left native transaction ownership unresolved", operation)
}

const (
	maxDirectArrayDimensions = 16
	maxDirectArrayBound      = int32(math.MaxInt16)
	minDirectArrayBound      = int32(math.MinInt16)
	directBlobSegmentSize    = 32767

	infoEnd       byte = 1
	infoTruncated byte = 2
	infoError     byte = 3

	recoveryTransactionIDWidth = 4

	// InfoDatabaseVersion requests the server version string from an
	// Attachment.DatabaseInfo call.
	InfoDatabaseVersion byte = 12
	// InfoDatabaseServerVersion is an explicit alias for InfoDatabaseVersion.
	InfoDatabaseServerVersion byte = InfoDatabaseVersion
	// InfoDatabaseID requests the database identity payload.
	InfoDatabaseID byte = 4
	// InfoDatabasePageSize requests the database page size.
	InfoDatabasePageSize byte = 14
	// InfoDatabaseAttachmentID requests the attachment identifier.
	InfoDatabaseAttachmentID byte = 22
	// InfoDatabaseODSVersion requests the database ODS major version.
	InfoDatabaseODSVersion byte = 32
	// InfoDatabaseODSMinorVersion requests the database ODS minor version.
	InfoDatabaseODSMinorVersion byte = 33
	// InfoDatabaseSQLDialect requests the database SQL dialect.
	InfoDatabaseSQLDialect byte = 62
	// InfoDatabaseReadOnly requests the database read-only flag.
	InfoDatabaseReadOnly byte = 63
	// InfoTransactionID requests the active transaction identifier.
	InfoTransactionID byte = 4
)

// InfoItem is one item returned by an InterBase database or transaction
// information request. Data is copied from the native response and is owned
// by the caller.
type InfoItem struct {
	Code byte
	Data []byte
}

// CreateOptions controls native database creation. A zero PageSize asks the
// server to use its default page size.
type CreateOptions struct {
	PageSize int
}

// DatabaseDiagnostics contains stable attachment and client diagnostics.
// Version fields are server/client-provided strings and may vary by engine
// release; numeric fields are decoded from the native information API.
type DatabaseDiagnostics struct {
	ClientVersion   string
	ServerVersion   string
	DatabaseVersion string
	ODSVersion      int64
	ODSMinorVersion int64
	PageSize        int64
	SQLDialect      int64
	ReadOnly        bool
}

// Bytes returns an owned copy of the raw information payload.
func (i InfoItem) Bytes() []byte {
	return append([]byte(nil), i.Data...)
}

// Uint64 decodes an InterBase little-endian unsigned integer payload.
func (i InfoItem) Uint64() (uint64, error) {
	if len(i.Data) == 0 {
		return 0, errors.New("interbase: information integer payload is empty")
	}
	if len(i.Data) > 8 {
		return 0, errors.New("interbase: information integer payload overflows uint64")
	}
	var value uint64
	for index, data := range i.Data {
		value |= uint64(data) << (8 * index)
	}
	return value, nil
}

// Int64 decodes an InterBase little-endian signed integer payload.
func (i InfoItem) Int64() (int64, error) {
	if len(i.Data) == 0 {
		return 0, errors.New("interbase: information integer payload is empty")
	}
	if len(i.Data) > 8 {
		return 0, errors.New("interbase: information integer payload overflows int64")
	}
	var value uint64
	for index, data := range i.Data {
		value |= uint64(data) << (8 * index)
	}
	if len(i.Data) < 8 && i.Data[len(i.Data)-1]&0x80 != 0 {
		value |= ^uint64(0) << (8 * len(i.Data))
	}
	return int64(value), nil
}

// Bool decodes a zero-or-one InterBase integer payload.
func (i InfoItem) Bool() (bool, error) {
	value, err := i.Uint64()
	if err != nil {
		return false, err
	}
	if value > 1 {
		return false, errors.New("interbase: information boolean payload is not zero or one")
	}
	return value == 1, nil
}

// Text decodes a UTF-8 information payload. The outer information envelope has
// already been removed by the native response parser.
func (i InfoItem) Text() (string, error) {
	data := i.Data
	if i.Code == InfoDatabaseVersion {
		var err error
		data, err = decodeDatabaseVersion(data)
		if err != nil {
			return "", err
		}
	}
	if !utf8.Valid(data) {
		return "", errors.New("interbase: information text payload is not valid UTF-8")
	}
	return string(data), nil
}

func decodeDatabaseVersion(data []byte) ([]byte, error) {
	if len(data) == 0 {
		return nil, errors.New("interbase: database version payload is empty")
	}
	count := int(data[0])
	if count == 0 {
		return nil, errors.New("interbase: database version payload has no versions")
	}
	offset := 1
	var first []byte
	for index := 0; index < count; index++ {
		if offset >= len(data) {
			return nil, errors.New("interbase: database version payload is missing a length")
		}
		length := int(data[offset])
		offset++
		if length > len(data)-offset {
			return nil, errors.New("interbase: database version payload is truncated")
		}
		if index == 0 {
			first = data[offset : offset+length]
		}
		offset += length
	}
	if offset != len(data) {
		return nil, errors.New("interbase: database version payload contains trailing bytes")
	}
	return first, nil
}

// Cell is one direct cursor value and its Python-compatible SQLDA indicator.
// Indicator uses the SQLIND_NULL high bit for NULL and preserves positive
// change-indicator flags.
type Cell struct {
	Value     any
	Indicator uint16
}

// Attachment is an explicitly owned InterBase attachment for the direct API.
// It is not a database/sql pool. Close releases all direct transactions and
// cursors before detaching the underlying native attachment.
type Attachment struct {
	conn *conn
	// directTx is retained as a compatibility seam for existing package tests;
	// transactions is the authoritative registry for direct ownership.
	directTx     *Transaction
	transactions map[*Transaction]struct{}
	distributed  *DistributedTransaction
	generation   uint64
}

// Transaction is an explicitly owned direct transaction.
type Transaction struct {
	attachment       *Attachment
	native           *nativeTransaction
	distributed      *DistributedTransaction
	distributedIndex int
	generation       uint64
	readOnly         bool
	done             bool
	cursors          map[*Cursor]struct{}
	blobs            map[*blobStream]struct{}
}

// Cursor is an explicitly owned direct result cursor.
type Cursor struct {
	tx          *Transaction
	generation  uint64
	native      *nativeCursor
	columns     []string
	ctx         context.Context
	fetched     bool
	named       bool
	closed      bool
	closeNative func(abort bool) error
}

// Open opens one explicitly owned direct attachment using the same connector,
// native engine, and scalar conversion path as database/sql.
func Open(ctx context.Context, cfg Config) (*Attachment, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	connectorValue, err := NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	connector, ok := connectorValue.(*connector)
	if !ok {
		return nil, errors.New("interbase: connector has an unexpected implementation")
	}
	nativeConn, err := connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	connection, ok := nativeConn.(*conn)
	if !ok {
		_ = nativeConn.Close()
		return nil, errors.New("interbase: connector returned an unexpected connection")
	}
	return &Attachment{conn: connection}, nil
}

const (
	minCreatePageSize = 1024
	maxCreatePageSize = 32768
)

func normalizeCreateOptions(options CreateOptions) (CreateOptions, error) {
	if options.PageSize == 0 {
		return options, nil
	}
	if options.PageSize < minCreatePageSize || options.PageSize > maxCreatePageSize ||
		options.PageSize&(options.PageSize-1) != 0 {
		return CreateOptions{}, fmt.Errorf("interbase: unsupported database page size %d", options.PageSize)
	}
	return options, nil
}

// CreateDatabase creates a new database using the native InterBase create API.
// The returned attachment owns the newly created database until DropDatabase
// is called explicitly. A native create can succeed before default transaction
// setup or context validation fails; in that case the function returns both a
// non-nil attachment and an error, and the caller must clean up that attachment.
func CreateDatabase(ctx context.Context, cfg Config, options CreateOptions) (*Attachment, error) {
	return createDatabaseWithNativeContext(ctx, cfg, options,
		func(ctx context.Context, cfg Config, pageSize int) (*nativeConnection, error) {
			return createNativeContext(ctx, cfg, pageSize)
		})
}

func createDatabaseWithNative(ctx context.Context, cfg Config, options CreateOptions,
	create func(Config, int) (*nativeConnection, error)) (*Attachment, error) {
	return createDatabaseWithNativeContext(ctx, cfg, options,
		func(_ context.Context, normalizedCfg Config, pageSize int) (*nativeConnection, error) {
			return create(normalizedCfg, pageSize)
		})
}

func createDatabaseWithNativeContext(ctx context.Context, cfg Config, options CreateOptions,
	create func(context.Context, Config, int) (*nativeConnection, error)) (*Attachment, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	normalizedOptions, err := normalizeCreateOptions(options)
	if err != nil {
		return nil, err
	}
	connectorValue, err := NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	connector, ok := connectorValue.(*connector)
	if !ok {
		return nil, errors.New("interbase: connector has an unexpected implementation")
	}
	if create == nil {
		return nil, errors.New("interbase: native database creator is unavailable")
	}
	native, createErr := create(ctx, connector.cfg, normalizedOptions.PageSize)
	if native == nil {
		if createErr == nil {
			createErr = errors.New("native database creator returned no connection")
		}
		return nil, sanitizeError("create database", createErr, configSecrets(connector.cfg)...)
	}
	connection := &conn{
		native:             native,
		transactionOptions: connector.cfg.TransactionOptions,
		redactionSecrets:   configSecrets(connector.cfg),
	}
	attachment := &Attachment{conn: connection}
	if createErr != nil {
		return attachment, sanitizeError("create database", createErr, configSecrets(connector.cfg)...)
	}
	if err := contextError(ctx); err != nil {
		return attachment, err
	}
	return attachment, nil
}

// DropDatabase permanently removes the database associated with this dedicated
// attachment. It is a destructive caller-authorized operation; do not use it
// for application databases or database/sql pools. If the native client
// consumes the database handle while reporting an error, the attachment is
// closed and cannot be retried; otherwise a failed drop keeps the attachment
// available for retry.
func (a *Attachment) DropDatabase(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if a == nil || a.conn == nil {
		return errDirectAttachmentClosed
	}
	a.conn.lockDirect()
	defer a.conn.mu.Unlock()
	if a.conn.closed || a.conn.native == nil || a.conn.native.broken() {
		return driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	for tx := range a.transactions {
		if !tx.done || len(tx.cursors) != 0 || len(tx.blobs) != 0 {
			return ErrAttachmentBusy
		}
		delete(a.transactions, tx)
	}
	if a.directTx != nil {
		if !a.directTx.done || len(a.directTx.cursors) != 0 || len(a.directTx.blobs) != 0 {
			return ErrAttachmentBusy
		}
		a.directTx = nil
	}
	if len(a.conn.statements) != 0 || len(a.conn.rows) != 0 {
		return ErrAttachmentBusy
	}
	native := a.conn.native
	dropErr, consumed := native.dropContext(ctx)
	if dropErr != nil {
		if consumed {
			a.conn.closed = true
			a.conn.native = nil
		}
		return a.conn.sanitizeError("drop database", dropErr)
	}
	a.conn.closed = true
	a.conn.native = nil
	if err := contextError(ctx); err != nil {
		return err
	}
	return nil
}

// Diagnostics returns server, database, and linked client diagnostics for the
// attachment.
func (a *Attachment) Diagnostics(ctx context.Context) (DatabaseDiagnostics, error) {
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	if a == nil || a.conn == nil {
		return DatabaseDiagnostics{}, errDirectAttachmentClosed
	}
	a.conn.lockDirect()
	defer a.conn.mu.Unlock()
	if a.conn.closed || a.conn.native == nil || a.conn.native.broken() {
		return DatabaseDiagnostics{}, driver.ErrBadConn
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	defer releaseNative()
	readInfo := func(code byte) (InfoItem, error) {
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		response, err := a.conn.native.databaseInfo(code)
		if err != nil {
			return InfoItem{}, a.conn.sanitizeError("database diagnostics", err)
		}
		item, err := parseInfoItem(response, code)
		if err != nil {
			return InfoItem{}, err
		}
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		return item, nil
	}
	serverVersionItem, err := readInfo(InfoDatabaseVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	serverVersion, err := serverVersionItem.Text()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsItem, err := readInfo(InfoDatabaseODSVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsVersion, err := odsItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsMinorItem, err := readInfo(InfoDatabaseODSMinorVersion)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	odsMinorVersion, err := odsMinorItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	pageSizeItem, err := readInfo(InfoDatabasePageSize)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	pageSize, err := pageSizeItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	dialectItem, err := readInfo(InfoDatabaseSQLDialect)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	dialect, err := dialectItem.Int64()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	readOnlyItem, err := readInfo(InfoDatabaseReadOnly)
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	readOnly, err := readOnlyItem.Bool()
	if err != nil {
		return DatabaseDiagnostics{}, err
	}
	if err := contextError(ctx); err != nil {
		return DatabaseDiagnostics{}, err
	}
	clientVersion, err := a.conn.native.clientVersion()
	if err != nil {
		return DatabaseDiagnostics{}, a.conn.sanitizeError("client diagnostics", err)
	}
	return DatabaseDiagnostics{
		ClientVersion:   clientVersion,
		ServerVersion:   serverVersion,
		DatabaseVersion: fmt.Sprintf("%d.%d", odsVersion, odsMinorVersion),
		ODSVersion:      odsVersion,
		ODSMinorVersion: odsMinorVersion,
		PageSize:        pageSize,
		SQLDialect:      dialect,
		ReadOnly:        readOnly,
	}, nil
}

// Close rolls back every active direct transaction as part of native attachment
// teardown. It is idempotent after the attachment has been closed.
func (a *Attachment) Close() error {
	if a == nil || a.conn == nil {
		return nil
	}
	a.conn.mu.Lock()
	defer a.conn.mu.Unlock()

	for transaction := range a.transactions {
		if transaction != nil && transaction.distributed != nil {
			state := distributedState(transaction.distributed.state.Load())
			if state != distributedStateCommitted && state != distributedStateRolledBack {
				return ErrAttachmentBusy
			}
		}
	}
	if a.directTx != nil && a.directTx.distributed != nil {
		state := distributedState(a.directTx.distributed.state.Load())
		if state != distributedStateCommitted && state != distributedStateRolledBack {
			return ErrAttachmentBusy
		}
	}

	var firstErr error
	transactions := make(map[*Transaction]struct{}, len(a.transactions)+1)
	for tx := range a.transactions {
		transactions[tx] = struct{}{}
	}
	if a.directTx != nil {
		transactions[a.directTx] = struct{}{}
	}
	for tx := range transactions {
		if tx == nil {
			continue
		}
		tx.done = true
		a.removeTransactionLocked(tx)
		var cleanupErr error
		if err := tx.closeCursorsLocked(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if err := tx.closeBlobsLocked(true); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
		if cleanupErr != nil {
			a.retainTransactionLocked(tx)
			firstErr = errors.Join(firstErr, cleanupErr)
			continue
		}
		if tx.native != nil {
			rollbackErr, handleState := tx.native.rollbackCleanup()
			if rollbackErr != nil {
				firstErr = errors.Join(firstErr,
					a.conn.sanitizeError("rollback direct transaction", rollbackErr))
			}
			if handleState != nativeHandleConsumed {
				if rollbackErr == nil {
					firstErr = errors.Join(firstErr,
						a.conn.sanitizeError("rollback direct transaction",
							nativeHandleStateError("rollback direct transaction", handleState)))
				}
				a.retainTransactionLocked(tx)
				continue
			}
			tx.native.free()
			tx.native = nil
		}
	}
	if len(a.transactions) != 0 {
		return firstErr
	}
	a.transactions = nil
	a.directTx = nil
	if err := a.conn.closeLocked(); err != nil {
		firstErr = errors.Join(firstErr, a.conn.sanitizeError("close attachment", err))
	}
	return firstErr
}

// BeginTx starts one explicit direct transaction on the attachment. Multiple
// direct transactions may be active concurrently; each owns an independent
// native transaction handle.
func (a *Attachment) BeginTx(ctx context.Context, options TransactionOptions) (*Transaction, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	normalized, err := normalizeTransactionOptions(options)
	if err != nil {
		return nil, err
	}
	if err := validateTxOptions(driver.TxOptions{
		Isolation: driver.IsolationLevel(normalized.Isolation),
		ReadOnly:  normalized.ReadOnly,
	}); err != nil {
		return nil, err
	}

	if a == nil || a.conn == nil {
		return nil, errDirectAttachmentClosed
	}
	a.conn.mu.Lock()
	defer a.conn.mu.Unlock()
	if a.distributed != nil || a.conn.distributed != nil {
		return nil, ErrDistributedParticipantManaged
	}
	if a.conn.closed || a.conn.native == nil || a.conn.native.broken() {
		return nil, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	tpb, err := buildTPB(
		driver.TxOptions{Isolation: driver.IsolationLevel(normalized.Isolation), ReadOnly: normalized.ReadOnly},
		normalized,
	)
	if err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	nativeTransaction, err := a.conn.native.beginTransaction(tpb)
	releaseNative()
	if err != nil {
		operationErr := a.conn.sanitizeError("begin direct transaction", err)
		if nativeTransaction != nil {
			a.generation++
			transaction := &Transaction{
				attachment: a,
				native:     nativeTransaction,
				generation: a.generation,
				done:       true,
				cursors:    make(map[*Cursor]struct{}),
				blobs:      make(map[*blobStream]struct{}),
			}
			a.retainTransactionLocked(transaction)
			if nativeTransaction.handleState() == nativeHandleConsumed {
				nativeTransaction.free()
				transaction.native = nil
				a.removeTransactionLocked(transaction)
			}
			operationErr = errors.Join(operationErr,
				a.invalidateExceptLocked(operationErr, transaction))
			return nil, operationErr
		}
		if a.conn.native.broken() {
			operationErr = errors.Join(operationErr, a.invalidateLocked(operationErr))
		}
		return nil, operationErr
	}
	if err := contextError(ctx); err != nil {
		a.generation++
		transaction := &Transaction{
			attachment: a,
			native:     nativeTransaction,
			generation: a.generation,
			done:       true,
			cursors:    make(map[*Cursor]struct{}),
			blobs:      make(map[*blobStream]struct{}),
		}
		a.retainTransactionLocked(transaction)
		rollbackErr, handleState := nativeTransaction.rollbackCleanup()
		if handleState == nativeHandleConsumed {
			nativeTransaction.free()
			transaction.native = nil
			a.removeTransactionLocked(transaction)
		} else if rollbackErr == nil {
			rollbackErr = nativeHandleStateError("rollback direct transaction", handleState)
		}
		if rollbackErr != nil {
			operationErr := a.conn.sanitizeError("rollback direct transaction", rollbackErr)
			operationErr = errors.Join(operationErr,
				a.invalidateExceptLocked(operationErr, transaction))
			return nil, errors.Join(err, operationErr)
		}
		return nil, err
	}

	a.generation++
	tx := &Transaction{
		attachment: a,
		native:     nativeTransaction,
		generation: a.generation,
		readOnly:   normalized.ReadOnly,
		cursors:    make(map[*Cursor]struct{}),
		blobs:      make(map[*blobStream]struct{}),
	}
	if a.transactions == nil {
		a.transactions = make(map[*Transaction]struct{})
	}
	a.transactions[tx] = struct{}{}
	if a.directTx == nil {
		a.directTx = tx
	}
	return tx, nil
}

func (t *Transaction) checkLocked() (*Attachment, error) {
	if t == nil || t.attachment == nil || t.attachment.conn == nil || t.done {
		return nil, errTransactionDone
	}
	a := t.attachment
	if t.distributed != nil && distributedState(t.distributed.state.Load()) != distributedStateActive {
		return nil, ErrDistributedParticipantManaged
	}
	if _, ok := a.transactions[t]; !ok && a.directTx != t {
		return nil, errTransactionDone
	}
	if a.conn.closed || a.conn.native == nil || a.conn.native.broken() {
		return nil, driver.ErrBadConn
	}
	return a, nil
}

// Query executes a SELECT-like statement in the direct transaction.
func (t *Transaction) Query(ctx context.Context, query string, args ...any) (*Cursor, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateQueryText(query); err != nil {
		return nil, err
	}
	converted, err := convertDirectArguments(args)
	if err != nil {
		return nil, err
	}

	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return nil, errTransactionDone
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateDirectArguments(converted, t); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	nativeCursor, columns, err := t.native.query(ctx, query, converted, true)
	releaseNative()
	if err != nil {
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := a.conn.sanitizeError("direct query", parts.primary)
		cleanupErr := a.conn.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeWriteOutcome("direct query",
			nativeQueryMutatingOf(err), contextCancellation(ctx), operationErr,
			cleanupErr, t.native.writeOutcomeState(), evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			a.conn.sanitizeError("", parts.request))
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	cursor := &Cursor{
		tx:         t,
		generation: t.generation,
		native:     nativeCursor,
		columns:    append([]string(nil), columns...),
		ctx:        ctx,
	}
	nativeCursor.directTransaction = t
	t.cursors[cursor] = struct{}{}
	if err := contextError(ctx); err != nil {
		closeErr := cursor.closeLocked(true, err)
		return nil, errors.Join(err, closeErr)
	}
	return cursor, nil
}

// Exec executes a non-result statement in the direct transaction.
func (t *Transaction) Exec(ctx context.Context, query string, args ...any) (int64, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if err := validateQueryText(query); err != nil {
		return 0, err
	}
	converted, err := convertDirectArguments(args)
	if err != nil {
		return 0, err
	}

	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return 0, errTransactionDone
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return 0, err
	}
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if err := validateDirectArguments(converted, t); err != nil {
		return 0, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return 0, err
	}
	defer releaseNative()
	affected, err := t.native.exec(ctx, query, converted, true)
	releaseNative()
	if err != nil {
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := a.conn.sanitizeError("direct exec", parts.primary)
		cleanupErr := a.conn.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeWriteOutcome("direct exec", true,
			contextCancellation(ctx), operationErr, cleanupErr, t.native.writeOutcomeState(), evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			a.conn.sanitizeError("", parts.request))
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return 0, operationErr
	}
	if err := contextError(ctx); err != nil {
		// Native execution completed successfully before the context was
		// observed. Returning the known result avoids an unsafe retry.
		return affected, nil
	}
	return affected, nil
}

// Commit completes the direct transaction and invalidates its cursors.
func (t *Transaction) Commit() error {
	return t.finish(true)
}

// Rollback completes the direct transaction and invalidates its cursors.
func (t *Transaction) Rollback() error {
	return t.finish(false)
}

func (t *Transaction) finish(commit bool) error {
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return errTransactionDone
	}
	if t.distributed != nil {
		return ErrDistributedParticipantManaged
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return err
	}
	t.done = true
	delete(a.transactions, t)
	if a.directTx == t {
		a.directTx = nil
	}
	var closeErr error
	if err := t.closeCursorsLocked(); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	if err := t.closeBlobsLocked(true); err != nil {
		closeErr = errors.Join(closeErr, err)
	}
	if closeErr != nil {
		a.retainTransactionLocked(t)
		return errors.Join(closeErr, a.invalidateExceptLocked(closeErr, t))
	}
	var handleState nativeHandleState
	if t.native == nil {
		if commit {
			err = a.conn.native.commit()
		} else {
			err = a.conn.native.rollback()
		}
		handleState = a.conn.native.transactionState()
	} else if commit {
		err = t.native.commit()
		handleState = t.native.handleState()
	} else {
		err = t.native.rollback()
		handleState = t.native.handleState()
	}
	if err == nil && handleState != nativeHandleConsumed {
		err = nativeHandleStateError("complete direct transaction", handleState)
	}
	if err == nil {
		if t.native != nil {
			t.native.free()
			t.native = nil
		}
		return nil
	}
	action := "rollback direct transaction"
	if commit {
		action = "commit direct transaction"
	}
	operationErr := a.conn.sanitizeError(action, err)
	cleanupErr := t.invalidateOwnedLocked(operationErr)
	graphErr := a.invalidateExceptLocked(operationErr, t)
	return errors.Join(operationErr, cleanupErr, graphErr)
}

// CommitRetaining commits the current work while retaining the transaction and
// its direct cursors open.
func (t *Transaction) CommitRetaining() error {
	return t.retain(true)
}

// RollbackRetaining rolls back the current work while retaining the
// transaction and its direct cursors open.
func (t *Transaction) RollbackRetaining() error {
	return t.retain(false)
}

func (t *Transaction) retain(commit bool) error {
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return errTransactionDone
	}
	if t.distributed != nil {
		return ErrDistributedParticipantManaged
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return err
	}
	if len(t.blobs) != 0 {
		return errDirectOpenBlobStreams
	}
	if t.native == nil {
		if commit {
			err = a.conn.native.commitRetaining()
		} else {
			err = a.conn.native.rollbackRetaining()
		}
	} else if commit {
		err = t.native.commitRetaining()
	} else {
		err = t.native.rollbackRetaining()
	}
	if err != nil {
		action := "rollback retaining direct transaction"
		if commit {
			action = "commit retaining direct transaction"
		}
		operationErr := a.conn.sanitizeError(action, err)
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return operationErr
	}
	t.generation++
	if a.generation < t.generation {
		a.generation = t.generation
	}
	for cursor := range t.cursors {
		cursor.generation = t.generation
	}
	return nil
}

// Plan prepares query and returns its server-generated plan without executing
// it. The prepared statement is always closed before returning.
func (t *Transaction) Plan(ctx context.Context, query string) (string, error) {
	if err := contextError(ctx); err != nil {
		return "", err
	}
	if err := validateQueryText(query); err != nil {
		return "", err
	}
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return "", errTransactionDone
	}
	t.attachment.conn.lockDirect()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return "", err
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return "", err
	}
	defer releaseNative()
	statement, err := t.native.prepare(ctx, query, true)
	if err != nil {
		releaseNative()
	}
	if err != nil {
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := a.conn.sanitizeError("prepare direct plan", parts.primary)
		cleanupErr := a.conn.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeOutcome("prepare direct plan", false,
			contextCancellation(ctx), operationErr, cleanupErr, evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			a.conn.sanitizeError("", parts.request))
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return "", operationErr
	}
	if err := contextError(ctx); err != nil {
		return "", errors.Join(err, statement.close())
	}
	plan, planErr := statement.plan()
	closeErr := statement.close()
	releaseNative()
	if planErr != nil || closeErr != nil {
		operationErr := errors.Join(planErr, closeErr)
		if operationErr != nil {
			operationErr = a.conn.sanitizeError("direct plan", operationErr)
		}
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return "", operationErr
	}
	if err := contextError(ctx); err != nil {
		return "", err
	}
	return plan, nil
}

// Info requests one raw transaction information item.
func (t *Transaction) Info(ctx context.Context, code byte) (InfoItem, error) {
	if err := contextError(ctx); err != nil {
		return InfoItem{}, err
	}
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return InfoItem{}, errTransactionDone
	}
	t.attachment.conn.lockDirect()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkInfoLocked()
	if err != nil {
		return InfoItem{}, err
	}
	if err := contextError(ctx); err != nil {
		return InfoItem{}, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return InfoItem{}, err
	}
	defer releaseNative()
	response, err := t.native.transactionInfo(code)
	releaseNative()
	if err != nil {
		operationErr := a.conn.sanitizeError("transaction info", err)
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return InfoItem{}, operationErr
	}
	if t.distributed != nil {
		items, parseErr := parseInfoItems(response, code)
		if parseErr != nil {
			return InfoItem{}, parseErr
		}
		if t.distributedIndex >= len(items) {
			return InfoItem{}, ErrDistributedRecoveryInfoMissing
		}
		item := items[t.distributedIndex]
		if err := contextError(ctx); err != nil {
			return InfoItem{}, err
		}
		return InfoItem{Code: item.Code, Data: append([]byte(nil), item.Data...)}, nil
	}
	item, err := parseInfoItem(response, code)
	if err != nil {
		return InfoItem{}, err
	}
	if err := contextError(ctx); err != nil {
		return InfoItem{}, err
	}
	return item, nil
}

func (t *Transaction) checkInfoLocked() (*Attachment, error) {
	if t == nil || t.attachment == nil || t.attachment.conn == nil || t.done {
		return nil, errTransactionDone
	}
	a := t.attachment
	if t.distributed != nil {
		state := distributedState(t.distributed.state.Load())
		if state != distributedStateActive && state != distributedStatePrepared &&
			state != distributedStateUncertain {
			return nil, errTransactionDone
		}
	}
	if _, ok := a.transactions[t]; !ok && a.directTx != t {
		return nil, errTransactionDone
	}
	if a.conn.closed || a.conn.native == nil || a.conn.native.broken() {
		return nil, driver.ErrBadConn
	}
	return a, nil
}

// OpenBlob opens a BLOB reference for bounded streaming reads.
func (t *Transaction) OpenBlob(ctx context.Context, ref BlobRef) (io.ReadCloser, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if t == nil || ref.tx == nil || ref.attachment == nil || ref.tx != t ||
		ref.attachment != t.attachment || ref.generation == 0 {
		return nil, errDirectBlobRefInvalid
	}
	if t.attachment == nil || t.attachment.conn == nil {
		return nil, errTransactionDone
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if ref.generation != t.generation {
		return nil, errDirectBlobRefInvalid
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	stream, err := t.native.openBlob(ref.high, ref.low, ref.subtype, ref.charset)
	releaseNative()
	if err != nil {
		operationErr := a.conn.sanitizeError("open direct BLOB", err)
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}
	stream.tx = t
	stream.generation = t.generation
	if err := contextError(ctx); err != nil {
		_, closeErr := stream.closeNativeLocked(true)
		return nil, errors.Join(err, closeErr)
	}
	t.blobs[stream] = struct{}{}
	return &directBlobReader{stream: stream}, nil
}

// CreateBlob streams data from reader into a new transaction-owned BLOB.
func (t *Transaction) CreateBlob(ctx context.Context, options BlobOptions, reader io.Reader) (BlobRef, error) {
	if err := contextError(ctx); err != nil {
		return BlobRef{}, err
	}
	if reader == nil {
		return BlobRef{}, errors.New("interbase: nil BLOB reader")
	}
	charsetName, err := normalizeCharset(options.Charset)
	if err != nil {
		return BlobRef{}, err
	}
	charset, err := blobCharsetID(charsetName)
	if err != nil {
		return BlobRef{}, err
	}
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return BlobRef{}, errTransactionDone
	}
	t.attachment.conn.mu.Lock()
	a, err := t.checkLocked()
	if err != nil {
		t.attachment.conn.mu.Unlock()
		return BlobRef{}, err
	}
	if err := contextError(ctx); err != nil {
		t.attachment.conn.mu.Unlock()
		return BlobRef{}, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		t.attachment.conn.mu.Unlock()
		return BlobRef{}, err
	}
	stream, err := t.native.createBlob(options.Subtype, charset)
	releaseNative()
	if err != nil {
		operationErr := a.conn.sanitizeError("create direct BLOB", err)
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		t.attachment.conn.mu.Unlock()
		return BlobRef{}, operationErr
	}
	stream.tx = t
	stream.generation = t.generation
	t.blobs[stream] = struct{}{}
	t.attachment.conn.mu.Unlock()

	buffer := make([]byte, 32*1024)
	pendingText := make([]byte, 0, utf8.UTFMax)
	noProgress := 0
	for {
		if err := contextError(ctx); err != nil {
			return BlobRef{}, t.abortBlobWriter(stream, err)
		}
		count, readErr := reader.Read(buffer)
		if count < 0 || count > len(buffer) {
			return BlobRef{}, t.abortBlobWriter(stream,
				errors.New("interbase: BLOB reader returned an invalid byte count"))
		}
		if count != 0 {
			noProgress = 0
			if err := contextError(ctx); err != nil {
				return BlobRef{}, t.abortBlobWriter(stream, err)
			}
			if options.Subtype == 1 {
				pendingText = append(pendingText, buffer[:count]...)
				for len(pendingText) != 0 {
					limit := len(pendingText)
					if limit > directBlobSegmentSize {
						limit = directBlobSegmentSize
					}
					prefix := utf8CompletePrefix(pendingText[:limit])
					if prefix == 0 {
						break
					}
					if err := t.writeBlobChunk(ctx, stream, pendingText[:prefix]); err != nil {
						return BlobRef{}, t.abortBlobWriter(stream, err)
					}
					pendingText = pendingText[prefix:]
				}
			} else if err := t.writeBlobChunk(ctx, stream, buffer[:count]); err != nil {
				return BlobRef{}, t.abortBlobWriter(stream, err)
			}
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				break
			}
			return BlobRef{}, t.abortBlobWriter(stream, readErr)
		}
		if count == 0 {
			noProgress++
			if noProgress >= 100 {
				return BlobRef{}, t.abortBlobWriter(stream, io.ErrNoProgress)
			}
		}
	}
	if len(pendingText) != 0 {
		return BlobRef{}, t.abortBlobWriter(stream,
			errors.New("interbase: text BLOB input ends with an incomplete UTF-8 sequence"))
	}

	if err := contextError(ctx); err != nil {
		return BlobRef{}, t.abortBlobWriter(stream, err)
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	if stream.closed || stream.writer == nil {
		return BlobRef{}, errTransactionDone
	}
	if _, err := t.checkLocked(); err != nil {
		return BlobRef{}, errors.Join(err, t.abortBlobWriterLocked(stream))
	}
	if err := contextError(ctx); err != nil {
		return BlobRef{}, errors.Join(err, t.abortBlobWriterLocked(stream))
	}
	releaseClose, gateErr := enterNativeContext(ctx)
	if gateErr != nil {
		return BlobRef{}, errors.Join(gateErr, t.abortBlobWriterLocked(stream))
	}
	nativeRef, closeErr := stream.closeNativeLocked(false)
	releaseClose()
	delete(t.blobs, stream)
	if closeErr != nil {
		operationErr := a.conn.sanitizeError("close direct BLOB", closeErr)
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return BlobRef{}, operationErr
	}
	return BlobRef{
		attachment: a,
		tx:         t,
		generation: t.generation,
		high:       nativeRef.high,
		low:        nativeRef.low,
		subtype:    nativeRef.subtype,
		charset:    nativeRef.charset,
	}, nil
}

func (t *Transaction) writeBlobChunk(ctx context.Context, stream *blobStream, data []byte) error {
	if len(data) == 0 {
		return nil
	}
	t.attachment.conn.mu.Lock()
	if stream.closed || stream.writer == nil {
		t.attachment.conn.mu.Unlock()
		return errTransactionDone
	}
	if _, err := t.checkLocked(); err != nil {
		t.attachment.conn.mu.Unlock()
		return err
	}
	if err := contextError(ctx); err != nil {
		t.attachment.conn.mu.Unlock()
		return err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		t.attachment.conn.mu.Unlock()
		return err
	}
	writeErr := stream.writeLocked(data)
	releaseNative()
	t.attachment.conn.mu.Unlock()
	if writeErr != nil {
		return t.attachment.conn.sanitizeError("write direct BLOB", writeErr)
	}
	return nil
}

func utf8CompletePrefix(data []byte) int {
	for offset := 0; offset < len(data); {
		if !utf8.FullRune(data[offset:]) {
			return offset
		}
		_, size := utf8.DecodeRune(data[offset:])
		if size == 0 {
			return offset
		}
		offset += size
	}
	return len(data)
}

type directBlobReader struct {
	stream *blobStream
}

func (r *directBlobReader) Read(buffer []byte) (int, error) {
	if r == nil || r.stream == nil || r.stream.tx == nil ||
		r.stream.tx.attachment == nil || r.stream.tx.attachment.conn == nil {
		return 0, io.ErrClosedPipe
	}
	t := r.stream.tx
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	if r.stream.closed || r.stream.reader == nil {
		return 0, io.ErrClosedPipe
	}
	if _, err := t.checkLocked(); err != nil {
		return 0, err
	}
	if r.stream.generation != t.generation {
		return 0, errDirectBlobRefInvalid
	}
	count, eof, err := r.stream.readLocked(buffer)
	if err != nil {
		operationErr := t.attachment.conn.sanitizeError("read direct BLOB", err)
		_, closeErr := r.stream.closeNativeLocked(true)
		delete(t.blobs, r.stream)
		if closeErr != nil {
			operationErr = errors.Join(operationErr,
				t.attachment.conn.sanitizeError("close direct BLOB", closeErr))
		}
		if t.attachment.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return 0, operationErr
	}
	if count != 0 {
		return count, nil
	}
	if eof {
		return 0, io.EOF
	}
	return 0, nil
}

func (r *directBlobReader) Close() error {
	if r == nil || r.stream == nil || r.stream.tx == nil ||
		r.stream.tx.attachment == nil || r.stream.tx.attachment.conn == nil {
		return nil
	}
	t := r.stream.tx
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	if r.stream.closed {
		delete(t.blobs, r.stream)
		return nil
	}
	if _, err := t.checkLocked(); err != nil {
		if errors.Is(err, errTransactionDone) || errors.Is(err, driver.ErrBadConn) {
			_, closeErr := r.stream.closeNativeLocked(true)
			r.stream.closed = true
			delete(t.blobs, r.stream)
			if closeErr != nil {
				return t.attachment.conn.sanitizeError("close direct BLOB", closeErr)
			}
			return nil
		}
		return err
	}
	_, closeErr := r.stream.closeNativeLocked(true)
	delete(t.blobs, r.stream)
	if closeErr != nil {
		operationErr := t.attachment.conn.sanitizeError("close direct BLOB", closeErr)
		if t.attachment.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return operationErr
	}
	return nil
}

func (t *Transaction) abortBlobWriter(stream *blobStream, cause error) error {
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return cause
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	return errors.Join(cause, t.abortBlobWriterLocked(stream))
}

func (t *Transaction) abortBlobWriterLocked(stream *blobStream) error {
	if stream == nil {
		return nil
	}
	_, closeErr := stream.closeNativeLocked(true)
	delete(t.blobs, stream)
	if closeErr == nil {
		return nil
	}
	operationErr := t.attachment.conn.sanitizeError("cancel direct BLOB", closeErr)
	t.invalidateLocked(operationErr)
	return operationErr
}

func blobCharsetID(charset string) (int16, error) {
	switch strings.ToUpper(charset) {
	case "UTF8":
		return 59, nil
	case "WIN1250":
		return 51, nil
	case "WIN1252":
		return 53, nil
	case "ISO8859_1":
		return 21, nil
	case "ASCII":
		return 2, nil
	default:
		return 0, fmt.Errorf("interbase: unsupported BLOB character set %q", charset)
	}
}

// DatabaseInfo requests one raw attachment information item.
func (a *Attachment) DatabaseInfo(ctx context.Context, code byte) (InfoItem, error) {
	if err := contextError(ctx); err != nil {
		return InfoItem{}, err
	}
	if a == nil || a.conn == nil {
		return InfoItem{}, errDirectAttachmentClosed
	}
	a.conn.lockDirect()
	defer a.conn.mu.Unlock()
	if a.conn.closed || a.conn.native == nil || a.conn.native.broken() {
		return InfoItem{}, driver.ErrBadConn
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return InfoItem{}, err
	}
	defer releaseNative()
	if err := contextError(ctx); err != nil {
		return InfoItem{}, err
	}
	response, err := a.conn.native.databaseInfo(code)
	releaseNative()
	if err != nil {
		operationErr := a.conn.sanitizeError("database info", err)
		if a.conn.native.broken() {
			a.invalidateLocked(operationErr)
		}
		return InfoItem{}, operationErr
	}
	item, err := parseInfoItem(response, code)
	if err != nil {
		return InfoItem{}, err
	}
	if err := contextError(ctx); err != nil {
		return InfoItem{}, err
	}
	return item, nil
}

func (t *Transaction) invalidateLocked(cause error) {
	if t == nil {
		return
	}
	if t.distributed != nil {
		t.invalidateDistributedLocked()
		return
	}
	if t.attachment != nil {
		_ = t.attachment.invalidateLocked(cause)
		return
	}
	_ = t.invalidateOwnedLocked(cause)
}

func (t *Transaction) invalidateDistributedLocked() {
	if t == nil || t.distributed == nil {
		return
	}
	t.distributed.state.Store(uint32(distributedStateUncertain))
	for stream := range t.blobs {
		delete(t.blobs, stream)
		_, _ = stream.closeNativeLocked(true)
	}
	for cursor := range t.cursors {
		cursor.closed = true
		native := cursor.native
		cursor.native = nil
		delete(t.cursors, cursor)
		if native != nil {
			_ = native.close()
		}
	}
}

func (t *Transaction) invalidateOwnedLocked(cause error) error {
	if t == nil {
		return nil
	}
	t.done = true
	a := t.attachment
	if a != nil {
		a.removeTransactionLocked(t)
	}
	var cleanupErr error
	if err := t.closeCursorsLocked(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := t.closeBlobsLocked(true); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	retainOwner := cleanupErr != nil && t.native != nil
	if cleanupErr == nil && t.native != nil {
		rollbackErr, handleState := t.native.rollbackCleanup()
		if rollbackErr != nil {
			if a != nil && a.conn != nil {
				cleanupErr = errors.Join(cleanupErr,
					a.conn.sanitizeError("rollback direct transaction", rollbackErr))
			} else {
				cleanupErr = errors.Join(cleanupErr, rollbackErr)
			}
		}
		if handleState == nativeHandleConsumed {
			t.native.free()
			t.native = nil
		} else {
			retainOwner = true
			if rollbackErr == nil {
				cleanupErr = errors.Join(cleanupErr,
					nativeHandleStateError("rollback direct transaction", handleState))
			}
		}
	}
	if retainOwner && a != nil {
		a.retainTransactionLocked(t)
	}
	_ = cause
	return cleanupErr
}

func (a *Attachment) invalidateLocked(cause error) error {
	return a.invalidateGraphLocked(cause, nil)
}

func (a *Attachment) invalidateExceptLocked(cause error, excluded *Transaction) error {
	return a.invalidateGraphLocked(cause, excluded)
}

func (a *Attachment) invalidateGraphLocked(cause error, excluded *Transaction) error {
	if a == nil || a.conn == nil {
		return nil
	}
	transactions := make(map[*Transaction]struct{}, len(a.transactions)+1)
	for transaction := range a.transactions {
		transactions[transaction] = struct{}{}
	}
	if a.directTx != nil {
		transactions[a.directTx] = struct{}{}
	}
	hasDistributed := false
	var firstErr error
	for transaction := range transactions {
		if transaction != nil {
			if transaction == excluded {
				continue
			}
			if transaction.distributed != nil {
				hasDistributed = true
				transaction.invalidateDistributedLocked()
				continue
			}
			if err := transaction.invalidateOwnedLocked(cause); err != nil {
				firstErr = errors.Join(firstErr, err)
			}
		}
	}
	if hasDistributed || len(a.transactions) != 0 {
		return firstErr
	}
	a.transactions = nil
	a.directTx = nil
	a.conn.invalidateLocked(cause)
	return firstErr
}

func (a *Attachment) removeTransactionLocked(transaction *Transaction) {
	if a == nil || transaction == nil {
		return
	}
	delete(a.transactions, transaction)
	if a.directTx == transaction {
		a.directTx = nil
	}
}

func (a *Attachment) retainTransactionLocked(transaction *Transaction) {
	if a == nil || transaction == nil {
		return
	}
	if a.transactions == nil {
		a.transactions = make(map[*Transaction]struct{})
	}
	a.transactions[transaction] = struct{}{}
	if a.directTx == nil {
		a.directTx = transaction
	}
}

func (t *Transaction) closeCursorsLocked() error {
	if t == nil || len(t.cursors) == 0 {
		return nil
	}
	nativeCursors := make([]*nativeCursor, 0, len(t.cursors))
	for cursor := range t.cursors {
		cursor.closed = true
		native := cursor.native
		cursor.native = nil
		delete(t.cursors, cursor)
		if native != nil {
			nativeCursors = append(nativeCursors, native)
		}
	}
	var firstErr error
	for _, native := range nativeCursors {
		if err := native.close(); err != nil {
			firstErr = errors.Join(firstErr, t.attachment.conn.sanitizeError("close direct cursor", err))
		}
	}
	return firstErr
}

func (t *Transaction) closeBlobsLocked(cancel bool) error {
	if t == nil || len(t.blobs) == 0 {
		return nil
	}
	streams := make([]*blobStream, 0, len(t.blobs))
	for stream := range t.blobs {
		delete(t.blobs, stream)
		streams = append(streams, stream)
	}
	var firstErr error
	for _, stream := range streams {
		if _, err := stream.closeNativeLocked(cancel); err != nil {
			firstErr = errors.Join(firstErr,
				t.attachment.conn.sanitizeError("close direct BLOB", err))
		}
	}
	return firstErr
}

// Next advances the cursor and reports whether a row is available.
func (c *Cursor) Next(ctx context.Context) (bool, error) {
	if err := contextError(ctx); err != nil {
		return false, err
	}
	if c == nil || c.tx == nil || c.tx.attachment == nil || c.tx.attachment.conn == nil {
		return false, errDirectCursorClosed
	}
	c.tx.attachment.conn.mu.Lock()
	defer c.tx.attachment.conn.mu.Unlock()
	if c.closed || c.native == nil {
		return false, errDirectCursorClosed
	}
	if _, err := c.tx.checkLocked(); err != nil {
		return false, err
	}
	if err := contextError(ctx); err != nil {
		return false, errors.Join(err, c.closeLocked(true, err))
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return false, errors.Join(err, c.closeLocked(true, err))
	}
	defer releaseNative()
	hasRow, err := c.native.next(ctx)
	releaseNative()
	if err != nil {
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := c.tx.attachment.conn.sanitizeError("direct fetch", parts.primary)
		cleanupErr := c.tx.attachment.conn.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeOutcome("direct fetch", false,
			contextCancellation(ctx), operationErr, cleanupErr, evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			c.tx.attachment.conn.sanitizeError("", parts.request))
		closeErr := c.closeLocked(true, operationErr)
		return false, errors.Join(operationErr, closeErr)
	}
	if !hasRow {
		return false, c.closeLocked(false, nil)
	}
	c.fetched = true
	return true, nil
}

// Row returns a Go-owned snapshot of the current row.
func (c *Cursor) Row() ([]Cell, error) {
	if c == nil || c.tx == nil || c.tx.attachment == nil || c.tx.attachment.conn == nil {
		return nil, errDirectCursorClosed
	}
	c.tx.attachment.conn.mu.Lock()
	defer c.tx.attachment.conn.mu.Unlock()
	if c.closed || c.native == nil {
		return nil, errDirectCursorClosed
	}
	if _, err := c.tx.checkLocked(); err != nil {
		return nil, err
	}
	if !c.fetched {
		return nil, errDirectCursorNotReady
	}
	result := make([]Cell, len(c.columns))
	for index := range result {
		indicator, err := c.native.indicator(index)
		if err != nil {
			operationErr := c.tx.attachment.conn.sanitizeError("read direct indicator", err)
			return nil, errors.Join(operationErr, c.closeLocked(true, operationErr))
		}
		value, err := c.native.value(index)
		if err != nil {
			operationErr := c.tx.attachment.conn.sanitizeError("decode direct result", err)
			return nil, errors.Join(operationErr, c.closeLocked(true, operationErr))
		}
		value, err = snapshotDirectValue(value)
		if err != nil {
			return nil, errors.Join(err, c.closeLocked(true, err))
		}
		result[index] = Cell{Value: value, Indicator: indicator}
	}
	return result, nil
}

// Close closes the cursor. It is idempotent after owner completion or
// attachment teardown.
func (c *Cursor) Close() error {
	if c == nil || c.tx == nil || c.tx.attachment == nil || c.tx.attachment.conn == nil {
		return nil
	}
	c.tx.attachment.conn.mu.Lock()
	defer c.tx.attachment.conn.mu.Unlock()
	if c.closed || c.native == nil {
		return nil
	}
	if _, err := c.tx.checkLocked(); err != nil {
		if errors.Is(err, errTransactionDone) || errors.Is(err, driver.ErrBadConn) {
			c.closed = true
			c.native = nil
			delete(c.tx.cursors, c)
			return nil
		}
		return err
	}
	return c.closeLocked(false, nil)
}

// SetName gives the active server cursor a name for positioned SQL such as
// UPDATE ... WHERE CURRENT OF. The operation requires a writable direct
// transaction and may only be performed once for this execution.
func (c *Cursor) SetName(name string) error {
	if err := validateCursorName(name); err != nil {
		return err
	}
	if c == nil || c.tx == nil || c.tx.attachment == nil || c.tx.attachment.conn == nil {
		return errDirectCursorClosed
	}
	c.tx.attachment.conn.mu.Lock()
	defer c.tx.attachment.conn.mu.Unlock()
	if c.closed || c.native == nil {
		return errDirectCursorClosed
	}
	if _, err := c.tx.checkLocked(); err != nil {
		return err
	}
	if c.tx.readOnly {
		return errors.New("interbase: cursor names require a writable direct transaction")
	}
	if c.named {
		return errDirectCursorNamed
	}
	if err := c.native.setName(name); err != nil {
		operationErr := c.tx.attachment.conn.sanitizeError("set direct cursor name", err)
		if c.tx.attachment.conn.native.broken() {
			c.tx.invalidateLocked(operationErr)
		}
		return operationErr
	}
	c.named = true
	return nil
}

func (c *Cursor) closeLocked(abort bool, cause error) error {
	if c.closed {
		delete(c.tx.cursors, c)
		return nil
	}
	c.closed = true
	delete(c.tx.cursors, c)
	native := c.native
	c.native = nil
	if native == nil {
		return nil
	}
	var closeErr error
	if c.closeNative != nil {
		closeErr = c.closeNative(abort)
	} else if abort {
		closeErr = native.abort()
	} else {
		closeErr = native.close()
	}
	if closeErr != nil {
		operationErr := c.tx.attachment.conn.sanitizeError("close direct cursor", closeErr)
		if cause != nil {
			operationErr = errors.Join(cause, operationErr)
		}
		c.tx.invalidateLocked(operationErr)
		return operationErr
	}
	return nil
}

func convertDirectArguments(values []any) ([]argument, error) {
	if len(values) > math.MaxInt16 {
		return nil, errors.New("interbase: too many query arguments")
	}
	args := make([]argument, len(values))
	for index, value := range values {
		arg, err := convertArgument(value)
		if err != nil {
			return nil, fmt.Errorf("interbase: argument %d: %w", index+1, err)
		}
		args[index] = arg
	}
	return args, nil
}

func validateDirectArguments(args []argument, transaction *Transaction) error {
	for index, arg := range args {
		if arg.kind != argumentBlobRef {
			continue
		}
		if transaction == nil || transaction.attachment == nil ||
			arg.blobRef.tx != transaction || arg.blobRef.attachment != transaction.attachment ||
			arg.blobRef.generation != transaction.generation ||
			(arg.blobRef.high == 0 && arg.blobRef.low == 0) {
			return fmt.Errorf("interbase: argument %d: %w", index+1, errDirectBlobRefInvalid)
		}
	}
	return nil
}

func validateCursorName(name string) error {
	if name == "" {
		return errors.New("interbase: cursor name is empty")
	}
	if strings.IndexByte(name, 0) >= 0 {
		return errors.New("interbase: cursor name contains a NUL byte")
	}
	if len([]byte(name)) > math.MaxUint16 {
		return errors.New("interbase: cursor name is too long")
	}
	return nil
}

func validateSQLRowIndicator(indicator uint16) error {
	const sqlIndicatorChange uint16 = 0x3f
	if indicator&sqlIndicatorChange != 0 {
		return errors.New("interbase: database/sql cannot represent result change indicators; use the direct API")
	}
	return nil
}

func parseInfoItem(response []byte, requested byte) (InfoItem, error) {
	if len(response) == 0 {
		return InfoItem{}, errors.New("interbase: empty information response")
	}
	if response[0] == infoTruncated {
		return InfoItem{}, errors.New("interbase: information response is truncated")
	}
	if response[0] == infoError {
		return InfoItem{}, errors.New("interbase: information request returned an error item")
	}
	if response[0] != requested {
		return InfoItem{}, fmt.Errorf("interbase: information response code %d does not match request %d", response[0], requested)
	}
	if len(response) < 4 {
		return InfoItem{}, errors.New("interbase: malformed information response header")
	}
	length := int(binary.LittleEndian.Uint16(response[1:3]))
	end := 3 + length
	if end > len(response) {
		return InfoItem{}, errors.New("interbase: malformed information response length")
	}
	if end >= len(response) || response[end] != infoEnd {
		return InfoItem{}, errors.New("interbase: information response has no terminator")
	}
	if end+1 != len(response) {
		return InfoItem{}, errors.New("interbase: information response contains duplicate items")
	}
	return InfoItem{Code: response[0], Data: append([]byte(nil), response[3:end]...)}, nil
}

// parseInfoItems parses the complete repeated-item response returned by the
// native information API. A transaction-info request can contain one item per
// participant; callers must not silently discard all but the first item.
func parseInfoItems(response []byte, requested byte) ([]InfoItem, error) {
	if len(response) == 0 {
		return nil, errors.New("interbase: empty information response")
	}
	if response[0] == infoTruncated {
		return nil, errors.New("interbase: information response is truncated")
	}
	if response[0] == infoError {
		return nil, errors.New("interbase: information request returned an error item")
	}
	items := make([]InfoItem, 0, 1)
	position := 0
	for position < len(response) && response[position] != infoEnd {
		if response[position] != requested {
			return nil, fmt.Errorf("interbase: information response code %d does not match request %d", response[position], requested)
		}
		if len(response)-position < 3 {
			return nil, errors.New("interbase: malformed information response header")
		}
		length := int(binary.LittleEndian.Uint16(response[position+1 : position+3]))
		start := position + 3
		end := start + length
		if end > len(response) {
			return nil, errors.New("interbase: malformed information response length")
		}
		items = append(items, InfoItem{
			Code: response[position],
			Data: append([]byte(nil), response[start:end]...),
		})
		position = end
	}
	if len(items) == 0 || position >= len(response) || response[position] != infoEnd {
		return nil, errors.New("interbase: information response has no terminator")
	}
	if position+1 != len(response) {
		return nil, errors.New("interbase: information response contains trailing data")
	}
	return items, nil
}

// parseInfoItemsOrPayload retains its historical package-local name but only
// accepts complete native information frames. Payload-only fallback is unsafe
// for recovery identifiers and is intentionally not supported.
func parseInfoItemsOrPayload(response []byte, requested byte) ([]InfoItem, error) {
	return parseInfoItems(response, requested)
}

func parseRecoveryDatabaseInfo(response []byte) (InfoItem, error) {
	items, err := parseInfoItemsOrPayload(response, InfoDatabaseID)
	if err != nil {
		return InfoItem{}, err
	}
	if len(items) != 1 {
		return InfoItem{}, fmt.Errorf("interbase: database information returned %d records, want 1", len(items))
	}
	if len(items[0].Data) == 0 {
		return InfoItem{}, errors.New("interbase: database information returned an empty database identity")
	}
	return items[0], nil
}

// parseRecoveryTransactionInfo validates the complete repeated transaction ID
// response used by Services ResolveLimbo. Native transaction IDs are unsigned
// four-byte values, and zero is not a resolvable limbo transaction ID.
func parseRecoveryTransactionInfo(response []byte, expectedCount int) ([]InfoItem, error) {
	if expectedCount <= 0 {
		return nil, errors.New("interbase: distributed recovery requires a positive participant count")
	}
	items, err := parseInfoItemsOrPayload(response, InfoTransactionID)
	if err != nil {
		return nil, err
	}
	if len(items) != expectedCount {
		return nil, fmt.Errorf("interbase: transaction information returned %d records, want %d", len(items), expectedCount)
	}
	for index, item := range items {
		if len(item.Data) != recoveryTransactionIDWidth {
			return nil, fmt.Errorf("interbase: transaction information record %d has width %d, want native width %d", index, len(item.Data), recoveryTransactionIDWidth)
		}
		transactionID := int64(binary.LittleEndian.Uint32(item.Data))
		if transactionID <= 0 || transactionID > math.MaxUint32 {
			return nil, fmt.Errorf("interbase: transaction information record %d has ID %d outside the Services ResolveLimbo range", index, transactionID)
		}
	}
	return items, nil
}

func snapshotDirectValue(value driver.Value) (any, error) {
	switch typed := value.(type) {
	case nil, string, int64, float64, bool, time.Time:
		return typed, nil
	case []byte:
		return append([]byte(nil), typed...), nil
	case Array:
		return cloneDirectArray(typed)
	case BlobRef:
		return typed, nil
	default:
		return nil, fmt.Errorf("interbase: unsupported direct result type %T", value)
	}
}

func validateDirectArray(value Array) error {
	if len(value.Bounds) == 0 {
		return errors.New("interbase: direct array requires at least one dimension")
	}
	if len(value.Bounds) > maxDirectArrayDimensions {
		return fmt.Errorf("interbase: direct array has %d dimensions; maximum is %d",
			len(value.Bounds), maxDirectArrayDimensions)
	}

	elementCount := uint64(1)
	for index, bound := range value.Bounds {
		if bound.Lower < minDirectArrayBound || bound.Lower > maxDirectArrayBound ||
			bound.Upper < minDirectArrayBound || bound.Upper > maxDirectArrayBound {
			return fmt.Errorf("interbase: direct array bound %d is outside InterBase array bound range [%d, %d]",
				index, minDirectArrayBound, maxDirectArrayBound)
		}
		if bound.Lower > bound.Upper {
			return fmt.Errorf("interbase: direct array bound %d has lower bound %d greater than upper bound %d",
				index, bound.Lower, bound.Upper)
		}
		width := uint64(int64(bound.Upper)-int64(bound.Lower)) + 1
		if elementCount > uint64(maxInt())/width {
			return errors.New("interbase: direct array element count overflows the Go address space")
		}
		elementCount *= width
	}
	if elementCount != uint64(len(value.Elements)) {
		return fmt.Errorf("interbase: direct array element count is %d, want %d",
			len(value.Elements), elementCount)
	}
	for index, element := range value.Elements {
		if element == nil {
			return fmt.Errorf("interbase: direct array element %d is nil; array elements cannot be NULL", index)
		}
		if _, err := normalizeArrayElement(element); err != nil {
			return fmt.Errorf("interbase: direct array element %d: %w", index, err)
		}
	}
	return nil
}

func normalizeDirectArray(value Array) (*Array, error) {
	if err := validateDirectArray(value); err != nil {
		return nil, err
	}
	result := &Array{
		Bounds:   append([]ArrayBound(nil), value.Bounds...),
		Elements: make([]any, len(value.Elements)),
	}
	for index, element := range value.Elements {
		normalized, err := normalizeArrayElement(element)
		if err != nil {
			return nil, fmt.Errorf("interbase: direct array element %d: %w", index, err)
		}
		result.Elements[index] = normalized
	}
	return result, nil
}

func normalizeArrayElement(value any) (any, error) {
	if _, ok := value.(Array); ok {
		return nil, errors.New("nested arrays are unsupported; use flat Elements with Bounds")
	}
	if _, ok := value.(*Array); ok {
		return nil, errors.New("nested arrays are unsupported; use flat Elements with Bounds")
	}
	converted, err := normalizeArgumentValue(value)
	if err != nil {
		return nil, err
	}
	if converted == nil {
		return nil, errors.New("array elements cannot be NULL")
	}
	switch converted.(type) {
	case string, []byte, int64, float64, bool, time.Time:
		return cloneDirectScalar(converted), nil
	default:
		return nil, fmt.Errorf("unsupported element type %T", converted)
	}
}

func cloneDirectArray(value Array) (Array, error) {
	if err := validateDirectArray(value); err != nil {
		return Array{}, err
	}
	result := Array{
		Bounds:   append([]ArrayBound(nil), value.Bounds...),
		Elements: make([]any, len(value.Elements)),
	}
	for index, element := range value.Elements {
		result.Elements[index] = cloneDirectScalar(element)
	}
	return result, nil
}

func cloneDirectScalar(value any) any {
	if bytes, ok := value.([]byte); ok {
		return append([]byte(nil), bytes...)
	}
	return value
}
