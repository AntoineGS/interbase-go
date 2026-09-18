package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"math"
	"strconv"
	"testing"
)

func TestAttachmentInvalidationClosesAllChildrenBeforeTransactionsAndParent(t *testing.T) {
	order := make([]string, 0, 10)
	connection := &conn{native: &nativeConnection{
		brokenOverride: func() bool { return true },
		closeOverride: func() error {
			order = append(order, "parent")
			return nil
		},
	}}
	attachment := &Attachment{conn: connection}
	makeTransaction := func(name string) *Transaction {
		native := &nativeTransaction{
			rollbackOverride: func() error {
				order = append(order, name+" rollback")
				return nil
			},
			freeOverride: func() { order = append(order, name+" free") },
		}
		transaction := &Transaction{
			attachment: attachment,
			native:     native,
			cursors:    make(map[*Cursor]struct{}),
			blobs:      make(map[*blobStream]struct{}),
		}
		cursor := &Cursor{
			tx:     transaction,
			native: &nativeCursor{closeOverride: func() error { order = append(order, name+" cursor"); return nil }},
		}
		stream := &blobStream{
			tx: transaction,
			closeNativeOverride: func(bool) (nativeBlobValue, error) {
				order = append(order, name+" blob")
				return nativeBlobValue{}, nil
			},
		}
		transaction.cursors[cursor] = struct{}{}
		transaction.blobs[stream] = struct{}{}
		attachment.transactions[transaction] = struct{}{}
		return transaction
	}
	attachment.transactions = make(map[*Transaction]struct{})
	first := makeTransaction("first")
	second := makeTransaction("second")
	attachment.directTx = first

	first.invalidateLocked(errors.New("native connection failed"))

	if !first.done || !second.done || first.native != nil || second.native != nil {
		t.Fatal("attachment invalidation did not terminally invalidate every transaction")
	}
	if len(attachment.transactions) != 0 || attachment.directTx != nil {
		t.Fatal("attachment invalidation left transaction registry entries")
	}
	if connection.native != nil || !connection.closed {
		t.Fatal("attachment invalidation did not close the parent connection")
	}
	parentIndex := indexOf(order, "parent")
	if parentIndex < 0 {
		t.Fatalf("cleanup order = %v, missing parent close", order)
	}
	for _, child := range []string{"first cursor", "first blob", "second cursor", "second blob", "first free", "second free"} {
		childIndex := indexOf(order, child)
		if childIndex < 0 || childIndex >= parentIndex {
			t.Fatalf("cleanup order = %v, %q was not completed before parent close", order, child)
		}
	}
}

func TestDirectBeginFailureDrainsSiblingResourcesBeforeParent(t *testing.T) {
	var order []string
	var beginCalls int
	broken := false
	beginErr := errors.New("begin failed")
	native := &nativeConnection{
		brokenOverride: func() bool { return broken },
		beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
			beginCalls++
			if beginCalls == 1 {
				return &nativeTransaction{
					rollbackOverride: func() error { order = append(order, "rollback"); return nil },
					freeOverride:     func() { order = append(order, "free") },
				}, nil
			}
			broken = true
			return nil, beginErr
		},
		closeOverride: func() error {
			order = append(order, "parent")
			return nil
		},
	}
	attachment := &Attachment{conn: &conn{native: native}}
	transaction, err := attachment.BeginTx(context.Background(), TransactionOptions{})
	if err != nil {
		t.Fatalf("first BeginTx() error = %v", err)
	}
	cursor := &Cursor{
		tx:     transaction,
		native: &nativeCursor{closeOverride: func() error { order = append(order, "cursor"); return nil }},
	}
	stream := &blobStream{
		tx: transaction,
		closeNativeOverride: func(bool) (nativeBlobValue, error) {
			order = append(order, "blob")
			return nativeBlobValue{}, nil
		},
	}
	transaction.cursors[cursor] = struct{}{}
	transaction.blobs[stream] = struct{}{}

	if _, err := attachment.BeginTx(context.Background(), TransactionOptions{}); !errors.Is(err, beginErr) {
		t.Fatalf("second BeginTx() error = %v, want begin error", err)
	}
	if !transaction.done || transaction.native != nil || len(attachment.transactions) != 0 {
		t.Fatal("begin failure left the sibling transaction owned")
	}
	if _, err := cursor.Next(context.Background()); !errors.Is(err, errDirectCursorClosed) {
		t.Fatalf("cursor.Next() after graph invalidation error = %v, want cursor-closed", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("cursor.Close() after graph invalidation error = %v", err)
	}
	if err := (&directBlobReader{stream: stream}).Close(); err != nil {
		t.Fatalf("blob Close() after graph invalidation error = %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() after graph invalidation error = %v", err)
	}
	parentIndex := indexOf(order, "parent")
	if parentIndex < 0 {
		t.Fatalf("cleanup order = %v, missing parent close", order)
	}
	for _, child := range []string{"cursor", "blob", "rollback", "free"} {
		childIndex := indexOf(order, child)
		if childIndex < 0 || childIndex >= parentIndex {
			t.Fatalf("cleanup order = %v, %q was not completed before parent close", order, child)
		}
	}
}

func TestDirectCompletionFailureInvalidatesSiblingGraph(t *testing.T) {
	for _, commit := range []bool{true, false} {
		t.Run(map[bool]string{true: "commit", false: "rollback"}[commit], func(t *testing.T) {
			broken := false
			var beginCalls int
			native := &nativeConnection{
				brokenOverride: func() bool { return broken },
				beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
					beginCalls++
					if beginCalls == 1 {
						return &nativeTransaction{
							commitOverride: func() error {
								broken = true
								return errors.New("commit failed")
							},
							rollbackOverride: func() error {
								if commit {
									return nil
								}
								broken = true
								return errors.New("rollback failed")
							},
						}, nil
					}
					return &nativeTransaction{
						rollbackOverride: func() error { return nil },
						freeOverride:     func() {},
					}, nil
				},
				closeOverride: func() error { return nil },
			}
			attachment := &Attachment{conn: &conn{native: native}}
			first, err := attachment.BeginTx(context.Background(), TransactionOptions{})
			if err != nil {
				t.Fatalf("first BeginTx() error = %v", err)
			}
			second, err := attachment.BeginTx(context.Background(), TransactionOptions{})
			if err != nil {
				t.Fatalf("second BeginTx() error = %v", err)
			}
			cursor := &Cursor{tx: second, native: &nativeCursor{}}
			second.cursors[cursor] = struct{}{}

			if commit {
				err = first.Commit()
			} else {
				err = first.Rollback()
			}
			if err == nil {
				t.Fatal("completion returned nil error")
			}
			if !first.done || !second.done || second.native != nil || len(attachment.transactions) != 0 {
				t.Fatal("completion failure did not invalidate the complete attachment graph")
			}
			if _, nextErr := cursor.Next(context.Background()); !errors.Is(nextErr, errDirectCursorClosed) {
				t.Fatalf("sibling cursor Next() error = %v, want cursor-closed", nextErr)
			}
			if closeErr := cursor.Close(); closeErr != nil {
				t.Fatalf("sibling cursor Close() error = %v", closeErr)
			}
			if closeErr := attachment.Close(); closeErr != nil {
				t.Fatalf("Attachment.Close() after completion failure = %v", closeErr)
			}
		})
	}
}

func TestDirectInvalidationDrainsEveryCursorBeforeRetainingParent(t *testing.T) {
	var order []string
	firstErr := errors.New("first cursor close failed")
	connection := &conn{native: &nativeConnection{
		brokenOverride: func() bool { return true },
		closeOverride: func() error {
			order = append(order, "parent")
			return nil
		},
	}}
	attachment := &Attachment{conn: connection, transactions: make(map[*Transaction]struct{})}
	transaction := &Transaction{
		attachment: attachment,
		native: &nativeTransaction{
			rollbackOverride: func() error { order = append(order, "rollback"); return nil },
			freeOverride:     func() { order = append(order, "free") },
		},
		cursors: make(map[*Cursor]struct{}),
		blobs:   make(map[*blobStream]struct{}),
	}
	first := &Cursor{tx: transaction, native: &nativeCursor{closeOverride: func() error {
		order = append(order, "first")
		return firstErr
	}}}
	second := &Cursor{tx: transaction, native: &nativeCursor{closeOverride: func() error {
		order = append(order, "second")
		return nil
	}}}
	transaction.cursors[first] = struct{}{}
	transaction.cursors[second] = struct{}{}
	attachment.transactions[transaction] = struct{}{}

	transaction.invalidateLocked(errors.New("connection failed"))

	if indexOf(order, "first") < 0 || indexOf(order, "second") < 0 {
		t.Fatalf("cleanup order = %v, did not attempt every cursor", order)
	}
	if indexOf(order, "parent") >= 0 {
		t.Fatalf("cleanup order = %v, parent was freed while a child close failed", order)
	}
	if transaction.native == nil {
		t.Fatal("transaction native owner was freed after incomplete child cleanup")
	}
	if _, ok := attachment.transactions[transaction]; !ok {
		t.Fatal("transaction ownership was not retained after incomplete child cleanup")
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() retry error = %v", err)
	}
	if indexOf(order, "parent") < 0 {
		t.Fatalf("cleanup order = %v, parent was not closed after retry", order)
	}
}

func TestDirectAttachmentCloseRetainsOwnerAfterChildCleanupFailure(t *testing.T) {
	firstErr := errors.New("first cursor close failed")
	var firstCursorCalls, secondCursorCalls int
	connection := &conn{native: &nativeConnection{
		closeOverride: func() error { return nil },
	}}
	attachment := &Attachment{conn: connection, transactions: make(map[*Transaction]struct{})}
	transaction := &Transaction{
		attachment: attachment,
		native: &nativeTransaction{
			rollbackOverride: func() error { return nil },
			freeOverride:     func() {},
		},
		cursors: make(map[*Cursor]struct{}),
		blobs:   make(map[*blobStream]struct{}),
	}
	transaction.cursors[&Cursor{tx: transaction, native: &nativeCursor{closeOverride: func() error {
		firstCursorCalls++
		return firstErr
	}}}] = struct{}{}
	transaction.cursors[&Cursor{tx: transaction, native: &nativeCursor{closeOverride: func() error {
		secondCursorCalls++
		return nil
	}}}] = struct{}{}
	attachment.transactions[transaction] = struct{}{}
	attachment.directTx = transaction

	if err := attachment.Close(); !errors.Is(err, firstErr) {
		t.Fatalf("Attachment.Close() error = %v, want child cleanup error", err)
	}
	if firstCursorCalls != 1 || secondCursorCalls != 1 {
		t.Fatalf("cursor cleanup calls = (%d, %d), want both cursors attempted", firstCursorCalls, secondCursorCalls)
	}
	if connection.closed || connection.native == nil || transaction.native == nil ||
		attachment.directTx != transaction {
		t.Fatal("Attachment.Close() released ownership after incomplete child cleanup")
	}

	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() retry error = %v", err)
	}
	if !connection.closed || connection.native != nil || len(attachment.transactions) != 0 {
		t.Fatal("Attachment.Close() retry did not release the retained owner")
	}
}

func TestDirectCompletionRetainsLiveNativeOwnerAfterCleanupFailure(t *testing.T) {
	completionErr := errors.New("commit outcome is unknown")
	cleanupErr := errors.New("rollback outcome is unknown")
	broken := false
	state := nativeHandleLive
	cleanupCalls := 0
	freeCalls := 0
	closeCalls := 0
	native := &nativeConnection{
		brokenOverride: func() bool { return broken },
		closeOverride: func() error {
			closeCalls++
			return nil
		},
	}
	attachment := &Attachment{conn: &conn{native: native}, transactions: make(map[*Transaction]struct{})}
	transaction := &Transaction{
		attachment: attachment,
		native: &nativeTransaction{
			commitOverride: func() error {
				broken = true
				return completionErr
			},
			handleStateOverride: func() nativeHandleState { return state },
			rollbackCleanupOverride: func() (error, nativeHandleState) {
				cleanupCalls++
				if cleanupCalls == 1 {
					return cleanupErr, nativeHandleLive
				}
				state = nativeHandleConsumed
				return nil, state
			},
			freeOverride: func() { freeCalls++ },
		},
		cursors: make(map[*Cursor]struct{}),
		blobs:   make(map[*blobStream]struct{}),
	}
	attachment.transactions[transaction] = struct{}{}
	attachment.directTx = transaction

	err := transaction.Commit()
	if !errors.Is(err, completionErr) || !errors.Is(err, cleanupErr) {
		t.Fatalf("Commit() error = %v, want completion and cleanup errors", err)
	}
	if !transaction.done || transaction.native == nil || len(attachment.transactions) != 1 {
		t.Fatal("failed completion did not retain the live native transaction owner")
	}
	if cleanupCalls != 1 || freeCalls != 0 || closeCalls != 0 {
		t.Fatalf("first cleanup = calls %d, frees %d, closes %d; want 1, 0, 0", cleanupCalls, freeCalls, closeCalls)
	}

	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() retry error = %v", err)
	}
	if cleanupCalls != 2 || freeCalls != 1 || closeCalls != 1 ||
		transaction.native != nil || len(attachment.transactions) != 0 {
		t.Fatalf("retry cleanup = calls %d, frees %d, closes %d, native=%p, transactions=%d; want 2, 1, 1, nil, 0",
			cleanupCalls, freeCalls, closeCalls, transaction.native, len(attachment.transactions))
	}
}

func TestDirectBeginCancellationRetainsLiveNativeOwnerAfterRollbackFailure(t *testing.T) {
	cleanupErr := errors.New("cancel rollback outcome is unknown")
	state := nativeHandleLive
	cleanupCalls := 0
	freeCalls := 0
	closeCalls := 0
	var cancel context.CancelFunc
	native := &nativeConnection{
		brokenOverride: func() bool { return state == nativeHandleLive && cleanupCalls > 0 },
		closeOverride: func() error {
			closeCalls++
			return nil
		},
		beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
			transaction := &nativeTransaction{
				handleStateOverride: func() nativeHandleState { return state },
				rollbackCleanupOverride: func() (error, nativeHandleState) {
					cleanupCalls++
					if cleanupCalls == 1 {
						return cleanupErr, nativeHandleLive
					}
					state = nativeHandleConsumed
					return nil, state
				},
				freeOverride: func() { freeCalls++ },
			}
			cancel()
			return transaction, nil
		},
	}
	attachment := &Attachment{conn: &conn{native: native}}
	ctx, cancelFunc := context.WithCancel(context.Background())
	cancel = cancelFunc

	transaction, err := attachment.BeginTx(ctx, TransactionOptions{})
	if transaction != nil || !errors.Is(err, context.Canceled) || !errors.Is(err, cleanupErr) {
		t.Fatalf("BeginTx() = (%p, %v), want nil with cancellation and cleanup errors", transaction, err)
	}
	if len(attachment.transactions) != 1 || attachment.directTx == nil ||
		attachment.directTx.native == nil || !attachment.directTx.done {
		t.Fatal("canceled BeginTx did not retain the live native transaction owner")
	}
	if cleanupCalls != 1 || freeCalls != 0 || closeCalls != 0 {
		t.Fatalf("first cancellation cleanup = calls %d, frees %d, closes %d; want 1, 0, 0", cleanupCalls, freeCalls, closeCalls)
	}

	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() after canceled BeginTx error = %v", err)
	}
	if cleanupCalls != 2 || freeCalls != 1 || closeCalls != 1 || len(attachment.transactions) != 0 {
		t.Fatalf("canceled BeginTx retry cleanup = calls %d, frees %d, closes %d, transactions=%d; want 2, 1, 1, 0",
			cleanupCalls, freeCalls, closeCalls, len(attachment.transactions))
	}
}

func TestDirectBeginFailureRetainsLiveNativeOwnerAfterRollbackFailure(t *testing.T) {
	beginErr := errors.New("start outcome is unknown")
	cleanupErr := errors.New("start rollback outcome is unknown")
	state := nativeHandleLive
	cleanupCalls := 0
	freeCalls := 0
	closeCalls := 0
	broken := false
	native := &nativeConnection{
		brokenOverride: func() bool { return broken },
		closeOverride: func() error {
			closeCalls++
			return nil
		},
		beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
			broken = true
			return &nativeTransaction{
				handleStateOverride: func() nativeHandleState { return state },
				rollbackCleanupOverride: func() (error, nativeHandleState) {
					cleanupCalls++
					if cleanupCalls == 1 {
						return cleanupErr, nativeHandleLive
					}
					state = nativeHandleConsumed
					return nil, state
				},
				freeOverride: func() { freeCalls++ },
			}, beginErr
		},
	}
	attachment := &Attachment{conn: &conn{native: native}}

	transaction, err := attachment.BeginTx(context.Background(), TransactionOptions{})
	if transaction != nil || !errors.Is(err, beginErr) {
		t.Fatalf("BeginTx() = (%p, %v), want nil with begin error", transaction, err)
	}
	if len(attachment.transactions) != 1 || attachment.directTx == nil ||
		attachment.directTx.native == nil || !attachment.directTx.done {
		t.Fatal("failed BeginTx did not retain the live native transaction owner")
	}

	if err := attachment.Close(); !errors.Is(err, cleanupErr) {
		t.Fatalf("Attachment.Close() after failed BeginTx error = %v, want cleanup error", err)
	}
	if cleanupCalls != 1 || freeCalls != 0 || closeCalls != 0 || len(attachment.transactions) != 1 {
		t.Fatalf("failed BeginTx first cleanup = calls %d, frees %d, closes %d, transactions=%d; want 1, 0, 0, 1",
			cleanupCalls, freeCalls, closeCalls, len(attachment.transactions))
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() retry after failed BeginTx error = %v", err)
	}
	if cleanupCalls != 2 || freeCalls != 1 || closeCalls != 1 || len(attachment.transactions) != 0 {
		t.Fatalf("failed BeginTx cleanup = calls %d, frees %d, closes %d, transactions=%d; want 2, 1, 1, 0",
			cleanupCalls, freeCalls, closeCalls, len(attachment.transactions))
	}
}

func TestDirectLifecycleAPISurface(t *testing.T) {
	var _ func(context.Context, Config, CreateOptions) (*Attachment, error) = CreateDatabase

	var _ interface {
		DropDatabase(context.Context) error
		Diagnostics(context.Context) (DatabaseDiagnostics, error)
	} = (*Attachment)(nil)
}

func TestCreateOptionsAcceptServerDefaultAndSupportedPageSizes(t *testing.T) {
	for _, pageSize := range []int{0, 1024, 2048, 4096, 8192, 16384, 32768} {
		t.Run(strconv.Itoa(pageSize), func(t *testing.T) {
			got, err := normalizeCreateOptions(CreateOptions{PageSize: pageSize})
			if err != nil {
				t.Fatalf("normalizeCreateOptions(%d) returned error: %v", pageSize, err)
			}
			if got.PageSize != pageSize {
				t.Fatalf("normalized page size = %d, want %d", got.PageSize, pageSize)
			}
		})
	}
}

func TestCreateOptionsRejectInvalidPageSizes(t *testing.T) {
	for _, pageSize := range []int{-1, 1, 512, 3072, 65536, math.MaxInt} {
		t.Run(strconv.Itoa(pageSize), func(t *testing.T) {
			if _, err := normalizeCreateOptions(CreateOptions{PageSize: pageSize}); err == nil {
				t.Fatalf("normalizeCreateOptions(%d) accepted an invalid page size", pageSize)
			}
		})
	}
}

func TestCreateDatabaseRejectsInvalidConfigBeforeNativeWork(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{name: "missing database", cfg: Config{User: "SYSDBA"}},
		{name: "missing user", cfg: Config{Database: "/tmp/new.ib"}},
		{name: "unsupported dialect", cfg: Config{Database: "/tmp/new.ib", User: "SYSDBA", Dialect: 2}},
		{name: "unsupported charset", cfg: Config{Database: "/tmp/new.ib", User: "SYSDBA", Charset: "not-a-charset"}},
		{name: "NUL database", cfg: Config{Database: "/tmp/new\x00.ib", User: "SYSDBA"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := CreateDatabase(context.Background(), tt.cfg, CreateOptions{})
			if err == nil {
				t.Fatal("CreateDatabase returned nil error")
			}
		})
	}
}

func TestInfoItemTypedValuesDecodeOfficialPayloads(t *testing.T) {
	pageSize, err := (InfoItem{
		Code: InfoDatabasePageSize,
		Data: []byte{0x00, 0x10, 0x00, 0x00},
	}).Uint64()
	if err != nil {
		t.Fatalf("Uint64() returned error: %v", err)
	}
	if pageSize != 4096 {
		t.Fatalf("page size = %d, want 4096", pageSize)
	}

	version, err := (InfoItem{
		Code: InfoDatabaseVersion,
		Data: []byte{1, 3, 'V', '1', '0'},
	}).Text()
	if err != nil {
		t.Fatalf("Text() returned error: %v", err)
	}
	if version != "V10" {
		t.Fatalf("version = %q, want V10", version)
	}

	readOnly, err := (InfoItem{
		Code: InfoDatabaseReadOnly,
		Data: []byte{1},
	}).Bool()
	if err != nil {
		t.Fatalf("Bool() returned error: %v", err)
	}
	if !readOnly {
		t.Fatal("read-only flag = false, want true")
	}

	for _, tc := range []struct {
		name string
		data []byte
		want int64
	}{
		{name: "negative one byte", data: []byte{0xff}, want: -1},
		{name: "negative two bytes", data: []byte{0xfe, 0xff}, want: -2},
		{name: "negative sixteen bit minimum", data: []byte{0x00, 0x80}, want: -32768},
		{name: "negative eight bytes", data: []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff, 0xff}, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := (InfoItem{Data: tc.data}).Int64()
			if err != nil {
				t.Fatalf("Int64() returned error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("Int64() = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestInfoItemTextDecodesCapturedDatabaseVersionFrame(t *testing.T) {
	const first = "LI-V15.1.0.49"
	const second = "LI-V15.1.0.49/tcp (17cfa1c08b2e)/P15"
	const third = "LI-V15.1.0.42/tcp (17cfa1c08b2e)/P15"

	payload := []byte{3, byte(len(first))}
	payload = append(payload, first...)
	payload = append(payload, byte(len(second)))
	payload = append(payload, second...)
	payload = append(payload, byte(len(third)))
	payload = append(payload, third...)
	response := []byte{InfoDatabaseVersion, byte(len(payload)), byte(len(payload) >> 8)}
	response = append(response, payload...)
	response = append(response, infoEnd)

	item, err := parseInfoItem(response, InfoDatabaseVersion)
	if err != nil {
		t.Fatalf("parseInfoItem() returned error: %v", err)
	}
	got, err := item.Text()
	if err != nil {
		t.Fatalf("Text() returned error: %v", err)
	}
	if got != first {
		t.Fatalf("Text() = %q, want %q", got, first)
	}
}

func TestInfoItemTextRejectsMalformedDatabaseVersionFrames(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "empty", data: nil},
		{name: "missing length", data: []byte{1}},
		{name: "truncated string", data: []byte{1, 3, 'V'}},
		{name: "truncated nested string", data: []byte{2, 1, 'V'}},
		{name: "trailing bytes", data: []byte{1, 1, 'V', 0}},
		{name: "invalid UTF-8", data: []byte{1, 1, 0xff}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := (InfoItem{Code: InfoDatabaseVersion, Data: tt.data}).Text(); err == nil {
				t.Fatal("Text() returned nil error")
			}
		})
	}
}

func TestInfoItemTypedValuesRejectMalformedPayloads(t *testing.T) {
	tests := []struct {
		name string
		call func(InfoItem) error
	}{
		{
			name: "empty integer",
			call: func(item InfoItem) error {
				_, err := item.Uint64()
				return err
			},
		},
		{
			name: "integer overflow",
			call: func(item InfoItem) error {
				_, err := item.Uint64()
				return err
			},
		},
		{
			name: "invalid UTF-8 text",
			call: func(item InfoItem) error {
				_, err := item.Text()
				return err
			},
		},
		{
			name: "invalid boolean",
			call: func(item InfoItem) error {
				_, err := item.Bool()
				return err
			},
		},
	}
	items := []InfoItem{
		{Code: InfoDatabasePageSize},
		{Code: InfoDatabasePageSize, Data: []byte{1, 2, 3, 4, 5, 6, 7, 8, 9}},
		{Code: InfoDatabaseVersion, Data: []byte{0xff}},
		{Code: InfoDatabaseReadOnly, Data: []byte{2}},
	}
	for index, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := tt.call(items[index]); err == nil {
				t.Fatal("typed decoder returned nil error")
			}
		})
	}
}

func TestInfoItemBytesReturnsOwnedCopy(t *testing.T) {
	item := InfoItem{Code: InfoDatabasePageSize, Data: []byte{1, 2, 3}}
	got := item.Bytes()
	got[0] = 9
	if item.Data[0] != 1 {
		t.Fatalf("Bytes() aliases InfoItem.Data: got %v", item.Data)
	}
}

func TestDropDatabaseRejectsActiveResourcesBeforeNativeCall(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*conn, *Attachment)
	}{
		{
			name: "direct transaction",
			setup: func(connection *conn, attachment *Attachment) {
				attachment.directTx = &Transaction{attachment: attachment, cursors: map[*Cursor]struct{}{}, blobs: map[*blobStream]struct{}{}}
			},
		},
		{
			name: "direct cursor",
			setup: func(connection *conn, attachment *Attachment) {
				tx := &Transaction{attachment: attachment, cursors: map[*Cursor]struct{}{}, blobs: map[*blobStream]struct{}{}}
				attachment.directTx = tx
				tx.cursors[&Cursor{tx: tx}] = struct{}{}
			},
		},
		{
			name: "direct blob",
			setup: func(connection *conn, attachment *Attachment) {
				tx := &Transaction{attachment: attachment, cursors: map[*Cursor]struct{}{}, blobs: map[*blobStream]struct{}{}}
				attachment.directTx = tx
				tx.blobs[&blobStream{tx: tx}] = struct{}{}
			},
		},
		{
			name: "stale completed transaction resources",
			setup: func(connection *conn, attachment *Attachment) {
				tx := &Transaction{attachment: attachment, done: true, cursors: map[*Cursor]struct{}{}, blobs: map[*blobStream]struct{}{}}
				attachment.directTx = tx
				tx.cursors[&Cursor{tx: tx}] = struct{}{}
				tx.blobs[&blobStream{tx: tx}] = struct{}{}
			},
		},
		{
			name: "prepared statement",
			setup: func(connection *conn, _ *Attachment) {
				connection.statements = map[*stmt]struct{}{&stmt{conn: connection}: {}}
			},
		},
		{
			name: "rows",
			setup: func(connection *conn, _ *Attachment) {
				connection.rows = map[*rows]struct{}{&rows{conn: connection}: {}}
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dropCalls := 0
			connection := &conn{native: &nativeConnection{
				brokenOverride: func() bool { return false },
				dropOverride: func() error {
					dropCalls++
					return nil
				},
			}}
			attachment := &Attachment{conn: connection}
			tc.setup(connection, attachment)
			if err := attachment.DropDatabase(context.Background()); !errors.Is(err, ErrAttachmentBusy) {
				t.Fatalf("DropDatabase() error = %v, want ErrAttachmentBusy", err)
			}
			if dropCalls != 0 || connection.closed || connection.native == nil {
				t.Fatalf("resource refusal changed ownership: calls=%d closed=%v native=%p", dropCalls, connection.closed, connection.native)
			}
		})
	}
}

func TestDropDatabasePreservesAttachmentAfterNativeFailure(t *testing.T) {
	nativeError := errors.New("drop failed")
	shouldFail := true
	dropCalls := 0
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		dropOverride: func() error {
			dropCalls++
			if shouldFail {
				return nativeError
			}
			return nil
		},
	}
	connection := &conn{native: native}
	attachment := &Attachment{conn: connection}

	err := attachment.DropDatabase(context.Background())
	if !errors.Is(err, nativeError) {
		t.Fatalf("failed DropDatabase() error = %v, want native error", err)
	}
	if connection.closed || connection.native != native || dropCalls != 1 {
		t.Fatalf("failed drop changed ownership: closed=%v native=%p calls=%d", connection.closed, connection.native, dropCalls)
	}

	shouldFail = false
	if err := attachment.DropDatabase(context.Background()); err != nil {
		t.Fatalf("retrying DropDatabase() = %v", err)
	}
	if !connection.closed || connection.native != nil || dropCalls != 2 {
		t.Fatalf("successful retry did not close ownership: closed=%v native=%p calls=%d", connection.closed, connection.native, dropCalls)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Close() after successful drop = %v", err)
	}
}

func TestDropDatabaseClosesAttachmentAfterConsumedNativeFailure(t *testing.T) {
	nativeError := errors.New("drop consumed the handle")
	dropCalls := 0
	native := &nativeConnection{
		brokenOverride:       func() bool { return false },
		dropConsumedOverride: true,
		dropOverride: func() error {
			dropCalls++
			return nativeError
		},
	}
	connection := &conn{native: native}
	attachment := &Attachment{conn: connection}

	if err := attachment.DropDatabase(context.Background()); !errors.Is(err, nativeError) {
		t.Fatalf("consumed DropDatabase() error = %v, want native error", err)
	}
	if !connection.closed || connection.native != nil || dropCalls != 1 {
		t.Fatalf("consumed drop left attachment retryable: closed=%v native=%p calls=%d",
			connection.closed, connection.native, dropCalls)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Close() after consumed drop = %v", err)
	}
	if err := attachment.DropDatabase(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("retry after consumed drop = %v, want driver.ErrBadConn", err)
	}
	if dropCalls != 1 {
		t.Fatalf("retry after consumed drop called native drop %d times, want 1", dropCalls)
	}
}

func TestDropDatabaseChecksContextBeforeNativeCall(t *testing.T) {
	dropCalls := 0
	connection := &conn{native: &nativeConnection{
		brokenOverride: func() bool { return false },
		dropOverride: func() error {
			dropCalls++
			return nil
		},
	}}
	attachment := &Attachment{conn: connection}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := attachment.DropDatabase(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("DropDatabase(cancelled context) = %v, want context.Canceled", err)
	}
	if dropCalls != 0 || connection.closed || connection.native == nil {
		t.Fatalf("cancelled drop changed ownership: calls=%d closed=%v native=%p", dropCalls, connection.closed, connection.native)
	}
}

func TestAttachmentDiagnosticsDecodesNativeInfoAndClientVersion(t *testing.T) {
	response := func(code byte, data []byte) []byte {
		result := make([]byte, 0, len(data)+4)
		result = append(result, code, byte(len(data)), byte(len(data)>>8))
		result = append(result, data...)
		return append(result, infoEnd)
	}
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoOverride: func(code byte) ([]byte, error) {
			switch code {
			case InfoDatabaseVersion:
				return response(code, []byte{1, 3, 'V', '1', '0'}), nil
			case InfoDatabaseODSVersion:
				return response(code, []byte{13, 0}), nil
			case InfoDatabaseODSMinorVersion:
				return response(code, []byte{1, 0}), nil
			case InfoDatabasePageSize:
				return response(code, []byte{0, 16, 0, 0}), nil
			case InfoDatabaseSQLDialect:
				return response(code, []byte{3}), nil
			case InfoDatabaseReadOnly:
				return response(code, []byte{0}), nil
			default:
				return nil, errors.New("unexpected info code")
			}
		},
		clientVersionOverride: func() (string, error) { return "client-v1", nil },
	}
	connection := &conn{native: native}
	attachment := &Attachment{conn: connection}
	got, err := attachment.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("Diagnostics() returned error: %v", err)
	}
	want := DatabaseDiagnostics{
		ClientVersion:   "client-v1",
		ServerVersion:   "V10",
		DatabaseVersion: "13.1",
		ODSVersion:      13,
		ODSMinorVersion: 1,
		PageSize:        4096,
		SQLDialect:      3,
		ReadOnly:        false,
	}
	if got != want {
		t.Fatalf("Diagnostics() = %#v, want %#v", got, want)
	}
}
