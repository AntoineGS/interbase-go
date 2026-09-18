package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"sort"
	"sync"
	"sync/atomic"
)

// TxOptions is the transaction option set used when a direct transaction is
// enlisted in a distributed transaction.
type TxOptions = TransactionOptions

// Participant identifies one explicitly owned attachment and its transaction
// options for a distributed transaction.
type Participant struct {
	Attachment *Attachment
	Options    TxOptions
}

var (
	ErrDistributedNoParticipants       = errors.New("interbase: distributed transaction requires at least one participant")
	ErrDistributedParticipantNil       = errors.New("interbase: distributed transaction participant attachment is nil")
	ErrDistributedParticipantDuplicate = errors.New("interbase: distributed transaction participant attachment is duplicated")
	ErrDistributedParticipantBusy      = errors.New("interbase: distributed transaction participant has active resources")
	ErrDistributedNotPrepared          = errors.New("interbase: distributed transaction is not prepared")
	ErrDistributedRecoveryInfoMissing  = errors.New("interbase: distributed transaction recovery information is unavailable")
	// ErrDistributedNativeUnavailable indicates that the native coordinator
	// handle was consumed by an ambiguous completion. The copied recovery
	// identifiers remain available for external limbo resolution.
	ErrDistributedNativeUnavailable = errors.New("interbase: distributed transaction native coordinator is unavailable")
)

type distributedState uint32

const (
	distributedStateActive distributedState = iota + 1
	distributedStatePrepared
	distributedStateCommitted
	distributedStateRolledBack
	distributedStateUncertain
	distributedStateReleased
)

// DistributedParticipantInfo contains the native identifiers needed to locate
// one participant during limbo recovery. The payloads are copied and remain
// available after an ambiguous coordinator completion.
type DistributedParticipantInfo struct {
	DatabaseID    InfoItem
	TransactionID InfoItem
}

// DistributedTransaction coordinates one native transaction handle shared by
// all of its enlisted participant attachments. Participant transactions may
// execute work while the coordinator is active, but only the coordinator may
// prepare, commit, or roll back the native handle.
type DistributedTransaction struct {
	mu           sync.Mutex
	state        atomic.Uint32
	native       *nativeDistributedTransaction
	participants []*Transaction
	recoveryInfo []DistributedParticipantInfo
}

// BeginDistributed starts one native multi-attachment transaction. The native
// engine receives every attachment and TPB in a single isc_start_multiple call;
// this is not implemented as a sequence of independent local transactions.
func BeginDistributed(ctx context.Context, participants []Participant) (*DistributedTransaction, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if len(participants) == 0 {
		return nil, ErrDistributedNoParticipants
	}

	tpbs := make([][]byte, len(participants))
	connections := make([]*nativeConnection, len(participants))
	seenAttachments := make(map[*Attachment]struct{}, len(participants))
	seenConnections := make(map[*conn]struct{}, len(participants))
	for index, participant := range participants {
		attachment := participant.Attachment
		if attachment == nil {
			return nil, ErrDistributedParticipantNil
		}
		if _, exists := seenAttachments[attachment]; exists {
			return nil, ErrDistributedParticipantDuplicate
		}
		seenAttachments[attachment] = struct{}{}
		if attachment.conn == nil {
			return nil, ErrDistributedParticipantBusy
		}
		if _, exists := seenConnections[attachment.conn]; exists {
			return nil, ErrDistributedParticipantBusy
		}
		seenConnections[attachment.conn] = struct{}{}
		normalized, err := normalizeTransactionOptions(participant.Options)
		if err != nil {
			return nil, err
		}
		tpbs[index], err = buildTPB(
			driver.TxOptions{Isolation: driver.IsolationLevel(normalized.Isolation), ReadOnly: normalized.ReadOnly},
			normalized,
		)
		if err != nil {
			return nil, err
		}
	}

	locked := orderedAttachments(participants)
	for _, attachment := range locked {
		attachment.conn.lockDirect()
	}
	defer func() {
		for index := len(locked) - 1; index >= 0; index-- {
			locked[index].conn.mu.Unlock()
		}
	}()

	for _, attachment := range locked {
		connection := attachment.conn
		if attachment.distributed != nil || connection.distributed != nil {
			return nil, ErrDistributedParticipantManaged
		}
		if attachmentHasActiveResourcesLocked(attachment) ||
			len(connection.statements) != 0 || len(connection.rows) != 0 {
			return nil, ErrDistributedParticipantBusy
		}
		if connection.closed || connection.native == nil || connection.native.broken() {
			return nil, driver.ErrBadConn
		}
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	for index, participant := range participants {
		connections[index] = participant.Attachment.conn.native
	}

	native, err := connections[0].beginDistributed(connections, tpbs)
	if native == nil {
		if err != nil {
			return nil, participants[0].Attachment.conn.sanitizeError("begin distributed transaction", err)
		}
		return nil, errors.New("interbase: native distributed transaction is unavailable")
	}
	if len(native.participants) != len(participants) {
		native.free()
		return nil, errors.New("interbase: native distributed transaction returned invalid participants")
	}
	if err != nil && native.handleState() == nativeHandleConsumed {
		native.free()
		return nil, participants[0].Attachment.conn.sanitizeError("begin distributed transaction", err)
	}
	distributed := newDistributedTransaction(native, participants,
		map[bool]distributedState{true: distributedStateActive, false: distributedStateUncertain}[err == nil])
	if err != nil {
		return distributed, participants[0].Attachment.conn.sanitizeError("begin distributed transaction", err)
	}
	if err := contextError(ctx); err != nil {
		rollbackErr := native.rollback()
		handleState := native.handleState()
		if handleState == nativeHandleConsumed {
			distributed.finalizeLocal(distributedStateRolledBack)
			if rollbackErr != nil {
				return nil, errors.Join(err, participants[0].Attachment.conn.sanitizeError("rollback distributed transaction", rollbackErr))
			}
			return nil, err
		}
		if rollbackErr == nil {
			rollbackErr = nativeHandleStateError("rollback distributed transaction", handleState)
		}
		distributed.state.Store(uint32(distributedStateUncertain))
		return distributed, errors.Join(err, participants[0].Attachment.conn.sanitizeError("rollback distributed transaction", rollbackErr))
	}
	return distributed, nil
}

func newDistributedTransaction(native *nativeDistributedTransaction,
	participants []Participant, initialState distributedState) *DistributedTransaction {

	distributed := &DistributedTransaction{
		native:       native,
		participants: make([]*Transaction, len(participants)),
		recoveryInfo: make([]DistributedParticipantInfo, len(participants)),
	}
	distributed.state.Store(uint32(initialState))
	transactionInfos := distributedTransactionInfo(native, len(participants))
	for index, participant := range participants {
		attachment := participant.Attachment
		attachment.generation++
		transaction := &Transaction{
			attachment:       attachment,
			native:           native.participants[index],
			distributed:      distributed,
			distributedIndex: index,
			generation:       attachment.generation,
			readOnly:         participant.Options.ReadOnly,
			cursors:          make(map[*Cursor]struct{}),
			blobs:            make(map[*blobStream]struct{}),
		}
		if attachment.transactions == nil {
			attachment.transactions = make(map[*Transaction]struct{})
		}
		attachment.transactions[transaction] = struct{}{}
		if attachment.directTx == nil {
			attachment.directTx = transaction
		}
		attachment.distributed = distributed
		attachment.conn.distributed = distributed
		distributed.participants[index] = transaction
		if databaseID, infoErr := attachment.conn.native.databaseInfo(InfoDatabaseID); infoErr == nil {
			if item, parseErr := parseRecoveryDatabaseInfo(databaseID); parseErr == nil {
				distributed.recoveryInfo[index].DatabaseID = item
			}
		}
		if index < len(transactionInfos) {
			distributed.recoveryInfo[index].TransactionID = InfoItem{
				Code: transactionInfos[index].Code,
				Data: append([]byte(nil), transactionInfos[index].Data...),
			}
		}
	}
	return distributed
}

// distributedTransactionInfo snapshots the repeated isc_info_tra_id response
// from the shared coordinator handle. isc_start_multiple receives one entry
// per participant, and the native client returns one transaction-info item per
// entry in that same input order; it does not expose independent handles for
// the participant views. The integration recovery test verifies this contract
// against live Services limbo IDs with deliberately different counters.
func distributedTransactionInfo(native *nativeDistributedTransaction, count int) []InfoItem {
	if native == nil || len(native.participants) == 0 || count <= 0 {
		return nil
	}
	response, err := native.participants[0].info(InfoTransactionID)
	if err != nil {
		return nil
	}
	items, err := parseRecoveryTransactionInfo(response, count)
	if err != nil {
		return nil
	}
	result := make([]InfoItem, len(items))
	for index, item := range items {
		result[index] = InfoItem{
			Code: item.Code,
			Data: append([]byte(nil), item.Data...),
		}
	}
	return result
}

func attachmentHasActiveResourcesLocked(attachment *Attachment) bool {
	if attachment == nil {
		return true
	}
	for transaction := range attachment.transactions {
		if transaction != nil && (!transaction.done || len(transaction.cursors) != 0 || len(transaction.blobs) != 0) {
			return true
		}
	}
	if attachment.directTx != nil && (!attachment.directTx.done ||
		len(attachment.directTx.cursors) != 0 || len(attachment.directTx.blobs) != 0) {
		return true
	}
	return false
}

func orderedAttachments(participants []Participant) []*Attachment {
	result := make([]*Attachment, 0, len(participants))
	for _, participant := range participants {
		result = append(result, participant.Attachment)
	}
	sort.Slice(result, func(left, right int) bool {
		return reflect.ValueOf(result[left].conn).Pointer() < reflect.ValueOf(result[right].conn).Pointer()
	})
	return result
}

// Participant returns one participant transaction by its input order.
func (d *DistributedTransaction) Participant(index int) (*Transaction, error) {
	if d == nil {
		return nil, errTransactionDone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if index < 0 || index >= len(d.participants) {
		return nil, errors.New("interbase: distributed participant index is out of range")
	}
	return d.participants[index], nil
}

// Participants returns a copy of the participant transaction list.
func (d *DistributedTransaction) Participants() []*Transaction {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*Transaction(nil), d.participants...)
}

// RecoveryInfo returns copied database and transaction identifiers for every
// participant. It remains available while the coordinator is active, prepared,
// or uncertain, including after a native completion error.
func (d *DistributedTransaction) RecoveryInfo(ctx context.Context) ([]DistributedParticipantInfo, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if d == nil {
		return nil, errTransactionDone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state := distributedState(d.state.Load())
	if state != distributedStateActive && state != distributedStatePrepared &&
		state != distributedStateUncertain {
		return nil, errTransactionDone
	}
	for _, info := range d.recoveryInfo {
		if len(info.DatabaseID.Data) == 0 || len(info.TransactionID.Data) == 0 {
			return nil, ErrDistributedRecoveryInfoMissing
		}
	}
	result := make([]DistributedParticipantInfo, len(d.recoveryInfo))
	for index, info := range d.recoveryInfo {
		result[index] = DistributedParticipantInfo{
			DatabaseID: InfoItem{
				Code: info.DatabaseID.Code,
				Data: append([]byte(nil), info.DatabaseID.Data...),
			},
			TransactionID: InfoItem{
				Code: info.TransactionID.Code,
				Data: append([]byte(nil), info.TransactionID.Data...),
			},
		}
	}
	return result, nil
}

// Prepare durably prepares the native transaction. An optional recovery
// message is passed to isc_prepare_transaction2 for limbo recovery.
func (d *DistributedTransaction) Prepare(ctx context.Context, message ...[]byte) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if d == nil {
		return errTransactionDone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if distributedState(d.state.Load()) != distributedStateActive {
		return errTransactionDone
	}
	locked := d.lockAttachments()
	defer unlockAttachments(locked)
	for _, transaction := range d.participants {
		if transaction.done {
			return errTransactionDone
		}
		if transaction.attachment.conn.closed || transaction.attachment.conn.native == nil ||
			transaction.attachment.conn.native.broken() {
			return driver.ErrBadConn
		}
		if len(transaction.blobs) != 0 {
			return errDirectOpenBlobStreams
		}
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return err
	}
	defer releaseNative()
	// Cursor cleanup is a native side effect. Admit the operation first so a
	// canceled admission leaves caller-owned cursor state untouched; after
	// admission, cleanup remains uninterruptible under the lifecycle gate.
	for _, transaction := range d.participants {
		if err := transaction.closeCursorsLocked(); err != nil {
			return err
		}
	}
	var recoveryMessage []byte
	if len(message) != 0 {
		recoveryMessage = message[0]
	}
	if err := d.native.prepare(recoveryMessage); err != nil {
		d.state.Store(uint32(distributedStateUncertain))
		if d.native.handleState() == nativeHandleConsumed && d.nativeOwnerStateKnown() {
			d.discardNativeOwnerLocked()
		}
		return d.participants[0].attachment.conn.sanitizeError("prepare distributed transaction", err)
	}
	d.state.Store(uint32(distributedStatePrepared))
	return nil
}

// Commit commits a previously prepared distributed transaction. If native
// completion fails, the coordinator remains in an uncertain state so callers
// can make an explicit recovery decision rather than silently retrying.
func (d *DistributedTransaction) Commit(ctx context.Context) error {
	return d.finish(ctx, true)
}

// Rollback rolls back an active, prepared, or uncertain distributed
// transaction.
func (d *DistributedTransaction) Rollback(ctx context.Context) error {
	return d.finish(ctx, false)
}

// Close is an explicit rollback convenience for an unresolved coordinator.
func (d *DistributedTransaction) Close() error {
	return d.Rollback(context.Background())
}

// ReleaseAfterRecovery relinquishes local ownership after an ambiguous native
// outcome. It deliberately does not call commit or rollback: the caller is
// responsible for resolving limbo through the recovery service using the
// identifiers returned by RecoveryInfo.
func (d *DistributedTransaction) ReleaseAfterRecovery(ctx context.Context) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if d == nil {
		return errTransactionDone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state := distributedState(d.state.Load())
	if state == distributedStateReleased || state == distributedStateCommitted || state == distributedStateRolledBack {
		return errTransactionDone
	}
	if state != distributedStatePrepared && state != distributedStateUncertain {
		return ErrDistributedNotPrepared
	}
	return d.releaseLocked()
}

// Abandon releases local ownership without attempting to complete the native
// transaction. This is the explicit escape hatch for callers that have handed
// recovery to an external coordinator or recovery service.
func (d *DistributedTransaction) Abandon() error {
	if d == nil {
		return errTransactionDone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state := distributedState(d.state.Load())
	if state == distributedStateReleased || state == distributedStateCommitted || state == distributedStateRolledBack {
		return errTransactionDone
	}
	if state != distributedStatePrepared && state != distributedStateUncertain {
		return ErrDistributedNotPrepared
	}
	return d.releaseLocked()
}

func (d *DistributedTransaction) finish(ctx context.Context, commit bool) error {
	if err := contextError(ctx); err != nil {
		return err
	}
	if d == nil {
		return errTransactionDone
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	state := distributedState(d.state.Load())
	if state == distributedStateCommitted || state == distributedStateRolledBack ||
		state == distributedStateReleased {
		return errTransactionDone
	}
	if commit && state != distributedStatePrepared && state != distributedStateUncertain {
		return ErrDistributedNotPrepared
	}
	if d.native == nil {
		return ErrDistributedNativeUnavailable
	}
	locked := d.lockAttachments()
	defer unlockAttachments(locked)
	if err := contextError(ctx); err != nil {
		return err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return err
	}
	defer releaseNative()
	if cleanupErr := d.closeParticipantResourcesLocked(); cleanupErr != nil {
		d.state.Store(uint32(distributedStateUncertain))
		return cleanupErr
	}
	if commit {
		err = d.native.commit()
	} else {
		err = d.native.rollback()
	}
	handleState := d.native.handleState()
	if err == nil && handleState != nativeHandleConsumed && d.nativeOwnerStateKnown() {
		err = nativeHandleStateError(distributedOperation(commit), handleState)
	}
	if err != nil {
		d.state.Store(uint32(distributedStateUncertain))
		if handleState == nativeHandleConsumed && d.nativeOwnerStateKnown() {
			d.discardNativeOwnerLocked()
		}
		return d.participants[0].attachment.conn.sanitizeError(distributedOperation(commit), err)
	}
	if commit {
		d.finalizeLocal(distributedStateCommitted)
	} else {
		d.finalizeLocal(distributedStateRolledBack)
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	return nil
}

func (d *DistributedTransaction) nativeOwnerStateKnown() bool {
	if d == nil || d.native == nil {
		return true
	}
	return d.native.ptr != nil || d.native.handleStateOverride != nil
}

func (d *DistributedTransaction) discardNativeOwnerLocked() {
	if d == nil {
		return
	}
	for _, transaction := range d.participants {
		if transaction != nil {
			transaction.native = nil
		}
	}
	if d.native != nil {
		d.native.free()
		d.native = nil
	}
}

func (d *DistributedTransaction) finalizeLocal(state distributedState) {
	if d == nil {
		return
	}
	d.state.Store(uint32(state))
	for _, transaction := range d.participants {
		if transaction == nil {
			continue
		}
		transaction.done = true
		delete(transaction.attachment.transactions, transaction)
		if transaction.attachment.directTx == transaction {
			transaction.attachment.directTx = nil
		}
		transaction.native = nil
		if transaction.attachment.distributed == d {
			transaction.attachment.distributed = nil
		}
		if transaction.attachment.conn.distributed == d {
			transaction.attachment.conn.distributed = nil
		}
	}
	d.native.free()
	d.native = nil
}

func (d *DistributedTransaction) closeParticipantResourcesLocked() error {
	var firstErr error
	for _, transaction := range d.participants {
		if transaction == nil {
			continue
		}
		if err := transaction.closeCursorsLocked(); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
		if err := transaction.closeBlobsLocked(true); err != nil {
			firstErr = errors.Join(firstErr, err)
		}
	}
	return firstErr
}

func (d *DistributedTransaction) releaseLocked() error {
	locked := d.lockAttachments()
	defer unlockAttachments(locked)
	cleanupErr := d.closeParticipantResourcesLocked()
	if cleanupErr != nil {
		return cleanupErr
	}
	for _, transaction := range d.participants {
		if transaction == nil {
			continue
		}
		transaction.done = true
		delete(transaction.attachment.transactions, transaction)
		if transaction.attachment.directTx == transaction {
			transaction.attachment.directTx = nil
		}
		if transaction.attachment.distributed == d {
			transaction.attachment.distributed = nil
		}
		if transaction.attachment.conn.distributed == d {
			transaction.attachment.conn.distributed = nil
		}
		transaction.native = nil
	}
	if d.native != nil {
		d.native.free()
		d.native = nil
	}
	d.state.Store(uint32(distributedStateReleased))
	return cleanupErr
}

func distributedOperation(commit bool) string {
	if commit {
		return "commit distributed transaction"
	}
	return "rollback distributed transaction"
}

func (d *DistributedTransaction) lockAttachments() []*Attachment {
	attachments := make([]*Attachment, 0, len(d.participants))
	for _, transaction := range d.participants {
		attachments = append(attachments, transaction.attachment)
	}
	sort.Slice(attachments, func(left, right int) bool {
		return reflect.ValueOf(attachments[left].conn).Pointer() < reflect.ValueOf(attachments[right].conn).Pointer()
	})
	for _, attachment := range attachments {
		attachment.conn.lockDirect()
	}
	return attachments
}

func unlockAttachments(attachments []*Attachment) {
	for index := len(attachments) - 1; index >= 0; index-- {
		attachments[index].conn.mu.Unlock()
	}
}
