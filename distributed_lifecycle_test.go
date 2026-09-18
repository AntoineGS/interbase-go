package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
)

func transactionInfoFrame(code byte, values ...uint32) []byte {
	response := make([]byte, 0, len(values)*7+4)
	for _, value := range values {
		response = append(response, code, 4, 0,
			byte(value), byte(value>>8), byte(value>>16), byte(value>>24))
	}
	response = append(response, infoEnd)
	return response
}

func infoItemFrame(code byte, payload []byte) []byte {
	response := make([]byte, 0, len(payload)+4)
	response = append(response, code, byte(len(payload)), byte(len(payload)>>8))
	response = append(response, payload...)
	return append(response, infoEnd)
}

func databaseInfoFrame(code byte, value uint32) []byte {
	return transactionInfoFrame(code, value)
}

func TestDistributedTransactionUsesOneCoordinatorAndFinalizesParticipants(t *testing.T) {
	var prepareCalls, commitCalls, freeCalls int
	nativeGroup := func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{
				{},
				{},
			},
			prepareOverride: func([]byte) error {
				prepareCalls++
				return nil
			},
			commitOverride: func() error {
				commitCalls++
				return nil
			},
			freeOverride: func() { freeCalls++ },
		}, nil
	}
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride:           func() bool { return false },
		beginDistributedOverride: nativeGroup,
	}}}
	second := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
	}}}

	distributed, err := BeginDistributed(context.Background(), []Participant{
		{Attachment: first},
		{Attachment: second},
	})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	participants := distributed.Participants()
	if len(participants) != 2 || participants[0] == nil || participants[1] == nil {
		t.Fatalf("Participants() = %#v, want two transactions", participants)
	}
	if participants[0].native == participants[1].native {
		t.Fatal("distributed participants unexpectedly share a Go native wrapper")
	}
	if err := distributed.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := distributed.Commit(context.Background()); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if prepareCalls != 1 || commitCalls != 1 || freeCalls != 1 {
		t.Fatalf("coordinator calls = prepare %d, commit %d, free %d; want 1 each", prepareCalls, commitCalls, freeCalls)
	}
	for index, participant := range participants {
		if !participant.done {
			t.Errorf("participant %d remains active", index)
		}
	}
	if len(first.transactions) != 0 || len(second.transactions) != 0 {
		t.Fatalf("participant registries not finalized: first=%d second=%d", len(first.transactions), len(second.transactions))
	}
}

func TestDistributedRollbackCanResolveBeforePrepare(t *testing.T) {
	var rollbackCalls int
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		beginDistributedOverride: func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
			return &nativeDistributedTransaction{
				participants: []*nativeTransaction{{}},
				rollbackOverride: func() error {
					rollbackCalls++
					return nil
				},
			}, nil
		},
	}}}
	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if rollbackCalls != 1 {
		t.Fatalf("rollback calls = %d, want 1", rollbackCalls)
	}
	if err := distributed.Rollback(context.Background()); !errors.Is(err, errTransactionDone) {
		t.Fatalf("repeat Rollback() error = %v, want transaction-done", err)
	}
}

func TestDistributedBeginFailureRetainsLiveNativeOwner(t *testing.T) {
	beginErr := errors.New("distributed start outcome is unknown")
	rollbackErr := errors.New("distributed rollback outcome is unknown")
	state := nativeHandleLive
	rollbackCalls := 0
	freeCalls := 0
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		beginDistributedOverride: func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
			return &nativeDistributedTransaction{
				participants: []*nativeTransaction{{}},
				rollbackOverride: func() error {
					rollbackCalls++
					if rollbackCalls == 1 {
						return rollbackErr
					}
					state = nativeHandleConsumed
					return nil
				},
				handleStateOverride: func() nativeHandleState { return state },
				freeOverride:        func() { freeCalls++ },
			}, beginErr
		},
	}}}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if distributed == nil || !errors.Is(err, beginErr) {
		t.Fatalf("BeginDistributed() = (%p, %v), want retained coordinator and begin error", distributed, err)
	}
	if len(first.transactions) != 1 || first.distributed != distributed || distributed.native == nil {
		t.Fatal("failed distributed begin did not retain coordinator ownership")
	}
	if err := distributed.Rollback(context.Background()); !errors.Is(err, rollbackErr) {
		t.Fatalf("first distributed rollback error = %v, want cleanup error", err)
	}
	if distributed.native == nil || freeCalls != 0 || rollbackCalls != 1 {
		t.Fatalf("first distributed rollback freed or lost owner: native=%p frees=%d calls=%d",
			distributed.native, freeCalls, rollbackCalls)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("distributed rollback retry error = %v", err)
	}
	if distributed.native != nil || freeCalls != 1 || len(first.transactions) != 0 {
		t.Fatalf("distributed rollback retry did not release owner: native=%p frees=%d transactions=%d",
			distributed.native, freeCalls, len(first.transactions))
	}
}

func TestDistributedParticipantCannotRetainOrComplete(t *testing.T) {
	first := &Attachment{
		conn: &conn{native: &nativeConnection{brokenOverride: func() bool { return false }}},
	}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{}},
			rollbackOverride: func() error {
				return nil
			},
			freeOverride: func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	participant, err := distributed.Participant(0)
	if err != nil {
		t.Fatalf("Participant(0) error = %v", err)
	}
	if err := participant.CommitRetaining(); !errors.Is(err, ErrDistributedParticipantManaged) {
		t.Fatalf("participant.CommitRetaining() error = %v, want ErrDistributedParticipantManaged", err)
	}
	if err := participant.RollbackRetaining(); !errors.Is(err, ErrDistributedParticipantManaged) {
		t.Fatalf("participant.RollbackRetaining() error = %v, want ErrDistributedParticipantManaged", err)
	}
	if err := participant.Commit(); !errors.Is(err, ErrDistributedParticipantManaged) {
		t.Fatalf("participant.Commit() error = %v, want ErrDistributedParticipantManaged", err)
	}
	if err := participant.Rollback(); !errors.Is(err, ErrDistributedParticipantManaged) {
		t.Fatalf("participant.Rollback() error = %v, want ErrDistributedParticipantManaged", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("distributed.Rollback() error = %v", err)
	}
}

func TestDistributedParticipantInvalidationPreservesCoordinatorForRecovery(t *testing.T) {
	first := &Attachment{
		conn: &conn{native: &nativeConnection{brokenOverride: func() bool { return false }}},
	}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{}},
			rollbackOverride: func() error {
				return nil
			},
			freeOverride: func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	participant := distributed.participants[0]
	participant.invalidateLocked(errors.New("participant operation failed"))
	if participant.done {
		t.Fatal("participant invalidation marked the coordinator-owned transaction done")
	}
	if _, ok := first.transactions[participant]; !ok {
		t.Fatal("participant invalidation removed the transaction from the attachment registry")
	}
	if got := distributedState(distributed.state.Load()); got != distributedStateUncertain {
		t.Fatalf("distributed state = %d, want uncertain", got)
	}
	if err := first.Close(); !errors.Is(err, ErrAttachmentBusy) {
		t.Fatalf("Attachment.Close() error = %v, want ErrAttachmentBusy", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("distributed.Rollback() error = %v", err)
	}
}

func TestDistributedRecoveryInfoIsCopiedAndSurvivesUncertainCompletion(t *testing.T) {
	prepareErr := errors.New("prepare outcome is unknown")
	first := &Attachment{
		conn: &conn{native: &nativeConnection{
			brokenOverride: func() bool { return false },
			databaseInfoOverride: func(byte) ([]byte, error) {
				return infoItemFrame(InfoDatabaseID, []byte{1, 2, 3, 4}), nil
			},
		}},
	}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{
				infoOverride: func(byte) ([]byte, error) {
					return transactionInfoFrame(InfoTransactionID, 0x06050403), nil
				},
			}},
			prepareOverride:  func([]byte) error { return prepareErr },
			rollbackOverride: func() error { return nil },
			freeOverride:     func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	info, err := distributed.RecoveryInfo(context.Background())
	if err != nil {
		t.Fatalf("RecoveryInfo() error = %v", err)
	}
	if len(info) != 1 || string(info[0].DatabaseID.Data) != string([]byte{1, 2, 3, 4}) ||
		string(info[0].TransactionID.Data) != string([]byte{3, 4, 5, 6}) {
		t.Fatalf("RecoveryInfo() = %#v, want copied database and transaction IDs", info)
	}
	info[0].DatabaseID.Data[0] = 99
	info[0].TransactionID.Data[0] = 99
	if err := distributed.Prepare(context.Background()); !errors.Is(err, prepareErr) {
		t.Fatalf("Prepare() error = %v, want prepare error", err)
	}
	info, err = distributed.RecoveryInfo(context.Background())
	if err != nil {
		t.Fatalf("RecoveryInfo() after uncertain prepare error = %v", err)
	}
	if string(info[0].DatabaseID.Data) != string([]byte{1, 2, 3, 4}) ||
		string(info[0].TransactionID.Data) != string([]byte{3, 4, 5, 6}) {
		t.Fatalf("RecoveryInfo() after mutation = %#v, want original IDs", info)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("distributed.Rollback() error = %v", err)
	}
}

func TestDistributedCompletionAfterConsumedNativeOwnerReturnsRecoveryError(t *testing.T) {
	completionErr := errors.New("distributed completion consumed the native owner")
	freeCalls := 0
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(byte) ([]byte, error) {
			return infoItemFrame(InfoDatabaseID, []byte{9, 8, 7, 6}), nil
		},
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{
				infoOverride: func(byte) ([]byte, error) {
					return transactionInfoFrame(InfoTransactionID, 707), nil
				},
			}},
			prepareOverride: func([]byte) error { return completionErr },
			handleStateOverride: func() nativeHandleState {
				return nativeHandleConsumed
			},
			freeOverride: func() { freeCalls++ },
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Prepare(context.Background()); !errors.Is(err, completionErr) {
		t.Fatalf("Prepare() error = %v, want completion error", err)
	}
	if distributed.native != nil {
		t.Fatal("consumed native owner was not discarded")
	}
	if _, err := distributed.RecoveryInfo(context.Background()); err != nil {
		t.Fatalf("RecoveryInfo() after consumed native owner = %v", err)
	}
	if err := distributed.Commit(context.Background()); !errors.Is(err, ErrDistributedNativeUnavailable) {
		t.Fatalf("Commit() after consumed native owner = %v, want ErrDistributedNativeUnavailable", err)
	}
	if err := distributed.Rollback(context.Background()); !errors.Is(err, ErrDistributedNativeUnavailable) {
		t.Fatalf("Rollback() after consumed native owner = %v, want ErrDistributedNativeUnavailable", err)
	}
	if got := distributedState(distributed.state.Load()); got != distributedStateUncertain {
		t.Fatalf("distributed state = %d, want uncertain", got)
	}
	if err := distributed.ReleaseAfterRecovery(context.Background()); err != nil {
		t.Fatalf("ReleaseAfterRecovery() after consumed native owner = %v", err)
	}
	if freeCalls != 1 {
		t.Fatalf("native free calls = %d, want 1", freeCalls)
	}
}

func TestDistributedRecoveryInfoParsesOrderedRepeatedTransactionRecords(t *testing.T) {
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 101), nil
		},
	}}}
	second := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 202), nil
		},
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{
				{infoOverride: func(code byte) ([]byte, error) {
					return transactionInfoFrame(code, 303, 404), nil
				}},
				{},
			},
			rollbackOverride: func() error { return nil },
			freeOverride:     func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{
		{Attachment: first},
		{Attachment: second},
	})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}

	info := distributed.recoveryInfo
	if len(info) != 2 {
		t.Fatalf("recovery info count = %d, want 2", len(info))
	}
	for index, want := range []struct {
		database    uint64
		transaction uint64
	}{
		{database: 101, transaction: 303},
		{database: 202, transaction: 404},
	} {
		gotDatabase, err := info[index].DatabaseID.Uint64()
		if err != nil {
			t.Fatalf("recovery info[%d] database ID: %v", index, err)
		}
		gotTransaction, err := info[index].TransactionID.Uint64()
		if err != nil {
			t.Fatalf("recovery info[%d] transaction ID: %v", index, err)
		}
		if gotDatabase != want.database || gotTransaction != want.transaction {
			t.Fatalf("recovery info[%d] = (%d, %d), want (%d, %d)", index,
				gotDatabase, gotTransaction, want.database, want.transaction)
		}
	}
}

func TestDistributedRecoveryInfoRejectsUnexpectedTransactionRecordCount(t *testing.T) {
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 111), nil
		},
	}}}
	second := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 222), nil
		},
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{
				{infoOverride: func(code byte) ([]byte, error) {
					return transactionInfoFrame(code, 333, 444, 555), nil
				}},
				{},
			},
			rollbackOverride: func() error { return nil },
			freeOverride:     func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{
		{Attachment: first},
		{Attachment: second},
	})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if _, err := distributed.RecoveryInfo(context.Background()); !errors.Is(err, ErrDistributedRecoveryInfoMissing) {
		t.Fatalf("RecoveryInfo() error = %v, want missing info for unexpected record count", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestDistributedTransactionInfoRejectsMalformedRecoveryFrames(t *testing.T) {
	unterminated := infoItemFrame(InfoTransactionID, []byte{1, 2, 3, 4})
	unterminated = unterminated[:len(unterminated)-1]
	tests := []struct {
		name     string
		response []byte
		count    int
		wantNil  bool
	}{
		{name: "wrong item code", response: infoItemFrame(InfoDatabaseVersion, []byte{1, 2, 3, 4}), count: 1, wantNil: true},
		{name: "information error", response: []byte{infoError}, count: 1, wantNil: true},
		{name: "truncated response", response: []byte{infoTruncated}, count: 1, wantNil: true},
		{name: "unterminated response", response: unterminated, count: 1, wantNil: true},
		{name: "invalid transaction width", response: infoItemFrame(InfoTransactionID, []byte{1, 2, 3}), count: 1, wantNil: true},
		{name: "zero transaction ID", response: transactionInfoFrame(InfoTransactionID, 0), count: 1, wantNil: true},
		{name: "nonempty unframed garbage", response: []byte("garbage"), count: 1, wantNil: true},
		{name: "too few repeated records", response: transactionInfoFrame(InfoTransactionID, 303), count: 2, wantNil: true},
		{name: "too many repeated records", response: transactionInfoFrame(InfoTransactionID, 303, 404, 505), count: 2, wantNil: true},
		{name: "valid repeated records", response: transactionInfoFrame(InfoTransactionID, 303, 404), count: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			native := &nativeDistributedTransaction{
				participants: []*nativeTransaction{{
					infoOverride: func(byte) ([]byte, error) { return tt.response, nil },
				}},
			}
			got := distributedTransactionInfo(native, tt.count)
			if tt.wantNil {
				if got != nil {
					t.Fatalf("distributedTransactionInfo() = %#v, want unavailable recovery info", got)
				}
				return
			}
			if len(got) != 2 {
				t.Fatalf("distributedTransactionInfo() returned %d records, want 2", len(got))
			}
			for index, want := range []uint32{303, 404} {
				value, err := got[index].Uint64()
				if err != nil || value != uint64(want) {
					t.Fatalf("record %d = (%d, %v), want %d", index, value, err, want)
				}
			}
		})
	}
}

func TestDistributedRecoveryInfoRejectsInvalidCapturedIdentifiers(t *testing.T) {
	tests := []struct {
		name                 string
		databaseInfoResponse []byte
		transactionResponse  []byte
	}{
		{
			name:                 "database nonempty garbage",
			databaseInfoResponse: []byte("garbage"),
			transactionResponse:  transactionInfoFrame(InfoTransactionID, 606),
		},
		{
			name:                 "transaction zero",
			databaseInfoResponse: infoItemFrame(InfoDatabaseID, []byte{1, 2, 3, 4}),
			transactionResponse:  transactionInfoFrame(InfoTransactionID, 0),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := &Attachment{conn: &conn{native: &nativeConnection{
				brokenOverride: func() bool { return false },
				databaseInfoOverride: func(byte) ([]byte, error) {
					return tt.databaseInfoResponse, nil
				},
			}}}
			first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
				return &nativeDistributedTransaction{
					participants: []*nativeTransaction{{
						infoOverride: func(byte) ([]byte, error) { return tt.transactionResponse, nil },
					}},
					rollbackOverride: func() error { return nil },
					freeOverride:     func() {},
				}, nil
			}

			distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
			if err != nil {
				t.Fatalf("BeginDistributed() error = %v", err)
			}
			if _, err := distributed.RecoveryInfo(context.Background()); !errors.Is(err, ErrDistributedRecoveryInfoMissing) {
				t.Fatalf("RecoveryInfo() error = %v, want unavailable recovery info", err)
			}
			if err := distributed.Rollback(context.Background()); err != nil {
				t.Fatalf("Rollback() error = %v", err)
			}
		})
	}
}

func TestDistributedRollbackClosesParticipantResourcesBeforeNativeCompletion(t *testing.T) {
	order := make([]string, 0, 4)
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{}},
			rollbackOverride: func() error {
				order = append(order, "rollback")
				return nil
			},
			freeOverride: func() { order = append(order, "free") },
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	participant := distributed.participants[0]
	participant.cursors[&Cursor{
		tx:     participant,
		native: &nativeCursor{closeOverride: func() error { order = append(order, "cursor"); return nil }},
	}] = struct{}{}
	participant.blobs[&blobStream{
		tx: participant,
		closeNativeOverride: func(bool) (nativeBlobValue, error) {
			order = append(order, "blob")
			return nativeBlobValue{}, nil
		},
	}] = struct{}{}

	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	if len(order) != 4 {
		t.Fatalf("cleanup order = %v, want cursor/blob/rollback/free", order)
	}
	rollbackIndex := indexOf(order, "rollback")
	freeIndex := indexOf(order, "free")
	if rollbackIndex < 2 || freeIndex <= rollbackIndex {
		t.Fatalf("cleanup order = %v, resources were not closed before completion", order)
	}
	if !participant.done || len(participant.cursors) != 0 || len(participant.blobs) != 0 || len(first.transactions) != 0 {
		t.Fatal("distributed rollback left participant resources or registry entries live")
	}
}

func TestDistributedParticipantCannotStartAnotherTransaction(t *testing.T) {
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
			return &nativeTransaction{}, nil
		},
	}}}
	second := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants:     []*nativeTransaction{{}, {}},
			rollbackOverride: func() error { return nil },
			freeOverride:     func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}, {Attachment: second}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if _, err := first.BeginTx(context.Background(), TransactionOptions{}); !errors.Is(err, ErrDistributedParticipantManaged) {
		t.Fatalf("Attachment.BeginTx() error = %v, want ErrDistributedParticipantManaged", err)
	}
	if _, err := first.conn.BeginTx(context.Background(), driver.TxOptions{}); !errors.Is(err, ErrDistributedParticipantManaged) {
		t.Fatalf("database/sql BeginTx() error = %v, want ErrDistributedParticipantManaged", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestBeginDistributedUsesNativeConnectionsAfterLocking(t *testing.T) {
	oldNative := &nativeConnection{brokenOverride: func() bool { return false }}
	newNative := &nativeConnection{
		brokenOverride: func() bool { return false },
		beginDistributedOverride: func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
			return &nativeDistributedTransaction{
				participants:     []*nativeTransaction{{}},
				rollbackOverride: func() error { return nil },
				freeOverride:     func() {},
			}, nil
		},
	}
	connection := &conn{native: oldNative}
	connection.directBeforeLock = func() {
		connection.native = newNative
	}
	attachment := &Attachment{conn: connection}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: attachment}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
}

func TestBeginDistributedDoesNotStartAfterPostLockCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	beginCalls := 0
	native := &nativeConnection{brokenOverride: func() bool { return false }}
	native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		beginCalls++
		return &nativeDistributedTransaction{
			participants:     []*nativeTransaction{{}},
			rollbackOverride: func() error { return nil },
			freeOverride:     func() {},
		}, nil
	}
	connection := &conn{native: native}
	connection.directBeforeLock = cancel
	attachment := &Attachment{conn: connection}

	distributed, err := BeginDistributed(ctx, []Participant{{Attachment: attachment}})
	if distributed != nil {
		t.Fatalf("BeginDistributed() returned coordinator %p after cancellation", distributed)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("BeginDistributed() error = %v, want context.Canceled", err)
	}
	if beginCalls != 0 {
		t.Fatalf("native distributed begin calls = %d, want 0 after post-lock cancellation", beginCalls)
	}
}

func TestDistributedReleaseAfterRecoveryDoesNotCompleteNativeTransaction(t *testing.T) {
	var rollbackCalls, commitCalls, freeCalls int
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 505), nil
		},
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{infoOverride: func(code byte) ([]byte, error) {
				return transactionInfoFrame(code, 606), nil
			}}},
			prepareOverride: func([]byte) error { return errors.New("prepare outcome unknown") },
			rollbackOverride: func() error {
				rollbackCalls++
				return nil
			},
			commitOverride: func() error {
				commitCalls++
				return nil
			},
			freeOverride: func() { freeCalls++ },
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare() returned nil error for unknown outcome")
	}
	if err := distributed.ReleaseAfterRecovery(context.Background()); err != nil {
		t.Fatalf("ReleaseAfterRecovery() error = %v", err)
	}
	if rollbackCalls != 0 || commitCalls != 0 || freeCalls != 1 {
		t.Fatalf("native completion calls = rollback %d, commit %d, free %d; want 0, 0, 1",
			rollbackCalls, commitCalls, freeCalls)
	}
	if got := distributedState(distributed.state.Load()); got != distributedStateReleased {
		t.Fatalf("distributed state = %d, want released", got)
	}
	if len(first.transactions) != 0 || first.distributed != nil || first.conn.distributed != nil {
		t.Fatal("ReleaseAfterRecovery() left attachment ownership live")
	}
}

func TestDistributedAbandonDoesNotImplicitlyRollbackOrCommit(t *testing.T) {
	var rollbackCalls, commitCalls, freeCalls int
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 707), nil
		},
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{infoOverride: func(code byte) ([]byte, error) {
				return transactionInfoFrame(code, 808), nil
			}}},
			prepareOverride:  func([]byte) error { return errors.New("prepare outcome unknown") },
			rollbackOverride: func() error { rollbackCalls++; return nil },
			commitOverride:   func() error { commitCalls++; return nil },
			freeOverride:     func() { freeCalls++ },
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare() returned nil error for unknown outcome")
	}
	if err := distributed.Abandon(); err != nil {
		t.Fatalf("Abandon() error = %v", err)
	}
	if rollbackCalls != 0 || commitCalls != 0 || freeCalls != 1 {
		t.Fatalf("native completion calls = rollback %d, commit %d, free %d; want 0, 0, 1",
			rollbackCalls, commitCalls, freeCalls)
	}
	if got := distributedState(distributed.state.Load()); got != distributedStateReleased {
		t.Fatalf("distributed state = %d, want released", got)
	}
}

func TestDistributedAbandonRejectsActiveTransaction(t *testing.T) {
	var rollbackCalls, commitCalls, freeCalls int
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants:     []*nativeTransaction{{}},
			rollbackOverride: func() error { rollbackCalls++; return nil },
			commitOverride:   func() error { commitCalls++; return nil },
			freeOverride:     func() { freeCalls++ },
		}, nil
	}
	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Abandon(); !errors.Is(err, ErrDistributedNotPrepared) {
		t.Fatalf("Abandon() error = %v, want ErrDistributedNotPrepared", err)
	}
	if rollbackCalls != 0 || commitCalls != 0 || freeCalls != 0 {
		t.Fatalf("active Abandon() caused native side effects: rollback=%d commit=%d free=%d",
			rollbackCalls, commitCalls, freeCalls)
	}
	if got := distributedState(distributed.state.Load()); got != distributedStateActive {
		t.Fatalf("distributed state = %d, want active", got)
	}
	if err := distributed.Rollback(context.Background()); err != nil {
		t.Fatalf("Rollback() cleanup error = %v", err)
	}
}

func TestDistributedReleaseRetainsOwnerAfterChildCleanupFailure(t *testing.T) {
	firstCloseErr := errors.New("first cursor close failed")
	var firstCursorCalls, secondCursorCalls, freeCalls int
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			return databaseInfoFrame(code, 909), nil
		},
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants: []*nativeTransaction{{infoOverride: func(code byte) ([]byte, error) {
				return transactionInfoFrame(code, 910), nil
			}}},
			prepareOverride: func([]byte) error { return errors.New("prepare outcome unknown") },
			freeOverride:    func() { freeCalls++ },
		}, nil
	}
	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Prepare(context.Background()); err == nil {
		t.Fatal("Prepare() returned nil error for unknown outcome")
	}
	participant := distributed.participants[0]
	participant.cursors[&Cursor{tx: participant, native: &nativeCursor{closeOverride: func() error {
		firstCursorCalls++
		return firstCloseErr
	}}}] = struct{}{}
	participant.cursors[&Cursor{tx: participant, native: &nativeCursor{closeOverride: func() error {
		secondCursorCalls++
		return nil
	}}}] = struct{}{}

	if err := distributed.ReleaseAfterRecovery(context.Background()); !errors.Is(err, firstCloseErr) {
		t.Fatalf("ReleaseAfterRecovery() error = %v, want child cleanup error", err)
	}
	if firstCursorCalls != 1 || secondCursorCalls != 1 {
		t.Fatalf("cursor cleanup calls = (%d, %d), want both cursors attempted", firstCursorCalls, secondCursorCalls)
	}
	if freeCalls != 0 || distributed.native == nil || distributedState(distributed.state.Load()) == distributedStateReleased {
		t.Fatal("ReleaseAfterRecovery() freed or released the coordinator after incomplete cleanup")
	}
	if len(first.transactions) != 1 || first.distributed != distributed || first.conn.distributed != distributed {
		t.Fatal("distributed ownership was not retained after incomplete cleanup")
	}

	if err := distributed.ReleaseAfterRecovery(context.Background()); err != nil {
		t.Fatalf("ReleaseAfterRecovery() retry error = %v", err)
	}
	if freeCalls != 1 || distributed.native != nil || distributedState(distributed.state.Load()) != distributedStateReleased {
		t.Fatal("ReleaseAfterRecovery() retry did not release the coordinator")
	}
}

func TestDistributedFinishAfterReleaseReturnsTransactionDone(t *testing.T) {
	first := &Attachment{conn: &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
	}}}
	first.conn.native.beginDistributedOverride = func(_ []*nativeConnection, _ [][]byte) (*nativeDistributedTransaction, error) {
		return &nativeDistributedTransaction{
			participants:    []*nativeTransaction{{}},
			prepareOverride: func([]byte) error { return nil },
			freeOverride:    func() {},
		}, nil
	}

	distributed, err := BeginDistributed(context.Background(), []Participant{{Attachment: first}})
	if err != nil {
		t.Fatalf("BeginDistributed() error = %v", err)
	}
	if err := distributed.Prepare(context.Background()); err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if err := distributed.Abandon(); err != nil {
		t.Fatalf("Abandon() error = %v", err)
	}
	if err := distributed.Commit(context.Background()); !errors.Is(err, errTransactionDone) {
		t.Fatalf("Commit() after Abandon() error = %v, want transaction-done", err)
	}
	if err := distributed.Rollback(context.Background()); !errors.Is(err, errTransactionDone) {
		t.Fatalf("Rollback() after Abandon() error = %v, want transaction-done", err)
	}
}

func indexOf(values []string, target string) int {
	for index, value := range values {
		if value == target {
			return index
		}
	}
	return -1
}
