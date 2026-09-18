package interbase

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"strings"
	"testing"
)

func TestDirectAPISurface(t *testing.T) {
	var _ func(context.Context, Config) (*Attachment, error) = Open

	var _ interface {
		Close() error
		BeginTx(context.Context, TransactionOptions) (*Transaction, error)
		DatabaseInfo(context.Context, byte) (InfoItem, error)
	} = (*Attachment)(nil)

	var _ interface {
		Query(context.Context, string, ...any) (*Cursor, error)
		Exec(context.Context, string, ...any) (int64, error)
		Commit() error
		Rollback() error
		CommitRetaining() error
		RollbackRetaining() error
		Plan(context.Context, string) (string, error)
		Info(context.Context, byte) (InfoItem, error)
	} = (*Transaction)(nil)

	var _ interface {
		Next(context.Context) (bool, error)
		Row() ([]Cell, error)
		Close() error
		SetName(string) error
	} = (*Cursor)(nil)

	_ = TransactionOptions{
		Isolation: sql.LevelSnapshot,
		ReadOnly:  true,
	}
}

func TestOpenRejectsNilAndCancelledContextsBeforeConnecting(t *testing.T) {
	if _, err := Open(nil, Config{}); err == nil {
		t.Fatal("Open(nil, ...) returned nil error")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Open(ctx, Config{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("Open(cancelled context, ...) error = %v, want context.Canceled", err)
	}
}

func TestDirectCursorCloseFailureInvalidatesAllWrappers(t *testing.T) {
	connection := &conn{}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
	}
	attachment.directTx = tx
	first := &Cursor{tx: tx, generation: 1, native: &nativeCursor{}}
	sibling := &Cursor{tx: tx, generation: 1, native: &nativeCursor{}}
	tx.cursors[first] = struct{}{}
	tx.cursors[sibling] = struct{}{}
	injected := errors.New("injected direct cursor close failure")
	first.closeNative = func(abort bool) error {
		if !abort {
			t.Fatal("close failure test did not use abort cleanup")
		}
		return injected
	}

	connection.mu.Lock()
	err := first.closeLocked(true, nil)
	connection.mu.Unlock()
	if !errors.Is(err, injected) {
		t.Fatalf("closeLocked() error = %v, want injected close failure", err)
	}
	if !tx.done || attachment.directTx != nil {
		t.Fatalf("failed cursor close left transaction active: done=%v directTx=%p", tx.done, attachment.directTx)
	}
	if !first.closed || first.native != nil || !sibling.closed || sibling.native != nil {
		t.Fatalf("failed cursor close left wrappers live: first=%#v sibling=%#v", first, sibling)
	}
	if len(tx.cursors) != 0 {
		t.Fatalf("failed cursor close left %d registered cursors", len(tx.cursors))
	}

	if err := first.Close(); err != nil {
		t.Fatalf("repeat first.Close() = %v", err)
	}
	if err := sibling.Close(); err != nil {
		t.Fatalf("repeat sibling.Close() = %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("repeat attachment.Close() = %v", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("second attachment.Close() = %v", err)
	}
	if err := tx.Rollback(); !errors.Is(err, errTransactionDone) {
		t.Fatalf("repeat tx.Rollback() = %v, want transaction-done", err)
	}
}

func TestDirectRetainingRejectsOpenBlobStreams(t *testing.T) {
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			commitRetainingOverride: func() error {
				t.Fatal("CommitRetaining reached native code with an open BLOB")
				return nil
			},
		},
	}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
		blobs:      make(map[*blobStream]struct{}),
	}
	attachment.directTx = tx
	tx.blobs[&blobStream{tx: tx, generation: 1}] = struct{}{}

	err := tx.CommitRetaining()
	if !errors.Is(err, errDirectOpenBlobStreams) {
		t.Fatalf("CommitRetaining() error = %v, want open-BLOB-stream rejection", err)
	}
	if tx.generation != 1 || tx.done || attachment.directTx != tx {
		t.Fatalf("rejected retaining changed transaction state: generation=%d done=%v directTx=%p",
			tx.generation, tx.done, attachment.directTx)
	}
}

func TestDirectRetainingInvalidatesBlobReferencesAndRefreshesCursors(t *testing.T) {
	native := &nativeConnection{
		brokenOverride:          func() bool { return false },
		commitRetainingOverride: func() error { return nil },
	}
	connection := &conn{native: native}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
		blobs:      make(map[*blobStream]struct{}),
	}
	attachment.directTx = tx
	cursor := &Cursor{tx: tx, generation: 1, native: &nativeCursor{}}
	tx.cursors[cursor] = struct{}{}
	oldRef := BlobRef{attachment: attachment, tx: tx, generation: 1, high: 1, low: 2}

	if err := tx.CommitRetaining(); err != nil {
		t.Fatalf("CommitRetaining() error = %v", err)
	}
	if tx.generation != 2 || cursor.generation != 2 || tx.done || attachment.directTx != tx {
		t.Fatalf("retaining state = generation %d, cursor generation %d, done=%v, directTx=%p",
			tx.generation, cursor.generation, tx.done, attachment.directTx)
	}
	if _, err := tx.OpenBlob(context.Background(), oldRef); !errors.Is(err, errDirectBlobRefInvalid) {
		t.Fatalf("OpenBlob(oldRef) error = %v, want stale-reference error", err)
	}
}

func TestDirectBlobCleanupFailureRetainsAttachmentUntilRetry(t *testing.T) {
	connection := &conn{
		native: &nativeConnection{brokenOverride: func() bool { return false }},
	}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
		blobs:      make(map[*blobStream]struct{}),
	}
	attachment.directTx = tx
	injected := errors.New("injected direct BLOB cleanup failure")
	stream := &blobStream{
		tx:         tx,
		generation: 1,
		closeNativeOverride: func(cancel bool) (nativeBlobValue, error) {
			if !cancel {
				t.Fatal("transaction cleanup did not cancel the BLOB")
			}
			return nativeBlobValue{}, injected
		},
	}
	tx.blobs[stream] = struct{}{}

	err := tx.Rollback()
	if !errors.Is(err, injected) {
		t.Fatalf("Rollback() error = %v, want BLOB cleanup failure", err)
	}
	if connection.closed || connection.native == nil || !tx.done || attachment.directTx != tx {
		t.Fatalf("BLOB cleanup failure did not retain ownership: conn=%#v tx=%#v attachment=%#v",
			connection, tx, attachment)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Attachment.Close() retry error = %v", err)
	}
	if !connection.closed || connection.native != nil || len(attachment.transactions) != 0 {
		t.Fatalf("Attachment.Close() retry did not finish cleanup: conn=%#v attachment=%#v",
			connection, attachment)
	}
}

func TestDirectBlobReaderCloseReleasesNativeAfterConnectionInvalidation(t *testing.T) {
	connection := &conn{
		native: &nativeConnection{brokenOverride: func() bool { return true }},
	}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
		blobs:      make(map[*blobStream]struct{}),
	}
	attachment.directTx = tx
	closeCalls := 0
	stream := &blobStream{
		tx:         tx,
		generation: 1,
		closeNativeOverride: func(cancel bool) (nativeBlobValue, error) {
			if !cancel {
				t.Fatal("invalidated reader was not cancelled")
			}
			closeCalls++
			return nativeBlobValue{}, nil
		},
	}
	tx.blobs[stream] = struct{}{}

	if err := (&directBlobReader{stream: stream}).Close(); err != nil {
		t.Fatalf("Close() after connection invalidation = %v, want nil", err)
	}
	if closeCalls != 1 {
		t.Fatalf("native reader cleanup calls = %d, want 1", closeCalls)
	}
	if !stream.closed || len(tx.blobs) != 0 {
		t.Fatalf("reader cleanup left stream registered: closed=%v blobs=%d", stream.closed, len(tx.blobs))
	}
}

func TestDirectBlobWriterAbortFailureInvalidatesWholeWrapperGraph(t *testing.T) {
	connection := &conn{
		native: &nativeConnection{brokenOverride: func() bool { return false }},
	}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
		blobs:      make(map[*blobStream]struct{}),
	}
	attachment.directTx = tx
	injected := errors.New("injected direct BLOB cancel failure")
	writer := &blobStream{
		tx:         tx,
		generation: 1,
		closeNativeOverride: func(cancel bool) (nativeBlobValue, error) {
			if !cancel {
				t.Fatal("writer abort did not cancel the BLOB")
			}
			return nativeBlobValue{}, injected
		},
	}
	siblingCloseCalls := 0
	sibling := &blobStream{
		tx:         tx,
		generation: 1,
		closeNativeOverride: func(cancel bool) (nativeBlobValue, error) {
			if !cancel {
				t.Fatal("sibling cleanup did not cancel the BLOB")
			}
			siblingCloseCalls++
			return nativeBlobValue{}, nil
		},
	}
	tx.blobs[writer] = struct{}{}
	tx.blobs[sibling] = struct{}{}

	cause := errors.New("reader failed")
	err := tx.abortBlobWriter(writer, cause)
	if !errors.Is(err, cause) || !errors.Is(err, injected) {
		t.Fatalf("abortBlobWriter() error = %v, want cause and cancel errors", err)
	}
	if siblingCloseCalls != 1 {
		t.Fatalf("sibling native cleanup calls = %d, want 1", siblingCloseCalls)
	}
	if !tx.done || attachment.directTx != nil || !connection.closed || connection.native != nil {
		t.Fatalf("cancel failure left wrapper graph usable: tx=%#v attachment=%#v conn=%#v",
			tx, attachment, connection)
	}
	if !writer.closed || !sibling.closed || len(tx.blobs) != 0 {
		t.Fatalf("cancel failure left BLOB wrappers live: writer=%#v sibling=%#v blobs=%d",
			writer, sibling, len(tx.blobs))
	}
}

func TestValidateCursorNameUsesEncodedNativeLimit(t *testing.T) {
	tests := []struct {
		name    string
		wantErr bool
	}{
		{name: "", wantErr: true},
		{name: "cursor_name"},
		{name: strings.Repeat("x", math.MaxUint16), wantErr: false},
		{name: strings.Repeat("x", math.MaxUint16+1), wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name[:min(len(tt.name), 24)], func(t *testing.T) {
			err := validateCursorName(tt.name)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateCursorName(%q) error = %v, wantErr=%v", tt.name, err, tt.wantErr)
			}
		})
	}
}

func TestParseInfoItemCopiesPayload(t *testing.T) {
	const code byte = 12
	response := []byte{code, 3, 0, 'a', 'b', 'c', 1}

	item, err := parseInfoItem(response, code)
	if err != nil {
		t.Fatalf("parseInfoItem() error = %v", err)
	}
	if item.Code != code || string(item.Data) != "abc" {
		t.Fatalf("item = %#v, want code %d and payload abc", item, code)
	}

	response[3] = 'x'
	item.Data[0] = 'y'
	if string(item.Data) != "ybc" {
		t.Fatalf("item payload unexpectedly changed after its own mutation: %q", item.Data)
	}
	if response[3] != 'x' {
		t.Fatalf("item payload aliases native response: response = %q", response)
	}
}

func TestParseInfoItemRejectsMalformedResponses(t *testing.T) {
	const code byte = 12
	tests := []struct {
		name     string
		response []byte
	}{
		{name: "empty", response: nil},
		{name: "truncated", response: []byte{infoTruncated}},
		{name: "error", response: []byte{infoError}},
		{name: "wrong code", response: []byte{13, 0, 0, infoEnd}},
		{name: "short header", response: []byte{code}},
		{name: "short payload", response: []byte{code, 2, 0, 'a', infoEnd}},
		{name: "missing terminator", response: []byte{code, 1, 0, 'a', 0}},
		{name: "duplicate item", response: []byte{code, 0, 0, infoEnd, code, 0, 0, infoEnd}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseInfoItem(tt.response, code); err == nil {
				t.Fatalf("parseInfoItem(%#v) returned nil error", tt.response)
			}
		})
	}
}

func TestParseInfoItemsOrPayloadRequiresStrictFrames(t *testing.T) {
	unterminated := infoItemFrame(InfoTransactionID, []byte{1, 2, 3, 4})
	unterminated = unterminated[:len(unterminated)-1]
	tests := []struct {
		name     string
		response []byte
		wantErr  bool
	}{
		{name: "wrong item code", response: infoItemFrame(InfoDatabaseVersion, []byte{1, 2, 3, 4}), wantErr: true},
		{name: "information error", response: []byte{infoError}, wantErr: true},
		{name: "truncated response", response: []byte{infoTruncated}, wantErr: true},
		{name: "unterminated response", response: unterminated, wantErr: true},
		{name: "nonempty unframed garbage", response: []byte("garbage"), wantErr: true},
		{name: "valid repeated records", response: transactionInfoFrame(InfoTransactionID, 303, 404)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			items, err := parseInfoItemsOrPayload(tt.response, InfoTransactionID)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("parseInfoItemsOrPayload() = %#v, want error", items)
				}
				return
			}
			if err != nil || len(items) != 2 {
				t.Fatalf("parseInfoItemsOrPayload() = (%#v, %v), want two framed records", items, err)
			}
		})
	}
}

func TestSnapshotDirectValueCopiesBytes(t *testing.T) {
	source := []byte("before")
	got, err := snapshotDirectValue(source)
	if err != nil {
		t.Fatalf("snapshotDirectValue() error = %v", err)
	}
	source[0] = 'a'
	if string(got.([]byte)) != "before" {
		t.Fatalf("snapshot = %q, want before", got)
	}
}

func TestBlobCharsetIDRecognizesSupportedCharacterSets(t *testing.T) {
	tests := []struct {
		name    string
		charset string
		want    int16
	}{
		{name: "UTF8", charset: "UTF8", want: 59},
		{name: "WIN1250", charset: "WIN1250", want: 51},
		{name: "WIN1252", charset: "WIN1252", want: 53},
		{name: "ISO8859_1", charset: "ISO8859_1", want: 21},
		{name: "ASCII", charset: "ASCII", want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := blobCharsetID(tc.charset)
			if err != nil {
				t.Fatalf("blobCharsetID(%q) returned error: %v", tc.charset, err)
			}
			if got != tc.want {
				t.Fatalf("blobCharsetID(%q) = %d, want %d", tc.charset, got, tc.want)
			}
		})
	}

	if _, err := blobCharsetID("NOPE"); err == nil {
		t.Fatal("blobCharsetID accepted an unsupported character set")
	}
}

func TestValidateSQLRowIndicator(t *testing.T) {
	tests := []struct {
		name      string
		indicator uint16
		wantErr   bool
	}{
		{name: "ordinary value", indicator: 0},
		{name: "null", indicator: 1 << 15},
		{name: "null update", indicator: (1 << 15) | 1<<1, wantErr: true},
		{name: "null all change flags", indicator: (1 << 15) | 0x3f, wantErr: true},
		{name: "insert", indicator: 1, wantErr: true},
		{name: "update", indicator: 1 << 1, wantErr: true},
		{name: "all change flags", indicator: 0x3f, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateSQLRowIndicator(tt.indicator)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validateSQLRowIndicator(%#x) error = %v, wantErr=%v", tt.indicator, err, tt.wantErr)
			}
		})
	}
}
