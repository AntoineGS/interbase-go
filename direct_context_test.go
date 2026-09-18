package interbase

import (
	"context"
	"errors"
	"testing"
)

func TestCreateDatabaseReturnsCreatedAttachmentAfterPostCreateCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	native := &nativeConnection{brokenOverride: func() bool { return false }}
	dropCalls := 0
	native.dropOverride = func() error {
		dropCalls++
		return nil
	}
	attachment, err := createDatabaseWithNative(ctx, Config{
		Database: "/tmp/direct-context.ib",
		User:     "SYSDBA",
	}, CreateOptions{}, func(Config, int) (*nativeConnection, error) {
		cancel()
		return native, nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("createDatabaseWithNative() error = %v, want context.Canceled", err)
	}
	if attachment == nil || attachment.conn == nil || attachment.conn.native != native || attachment.conn.closed {
		t.Fatalf("post-create cancellation lost ownership: attachment=%p conn=%p native=%p closed=%v", attachment, attachment.conn, attachment.conn.native, attachment.conn.closed)
	}
	if dropCalls != 0 {
		t.Fatalf("post-create cancellation dropped the created database: calls=%d", dropCalls)
	}
	if err := attachment.DropDatabase(context.Background()); err != nil {
		t.Fatalf("recovering created attachment with DropDatabase: %v", err)
	}
	if dropCalls != 1 || !attachment.conn.closed || attachment.conn.native != nil {
		t.Fatalf("recovered attachment state: calls=%d closed=%v native=%p", dropCalls, attachment.conn.closed, attachment.conn.native)
	}
}

func TestCreateDatabasePreservesAttachmentWhenNativeSetupFails(t *testing.T) {
	setupError := errors.New("default transaction setup failed")
	dropCalls := 0
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		dropOverride: func() error {
			dropCalls++
			return nil
		},
	}
	attachment, err := createDatabaseWithNative(context.Background(), Config{
		Database: "/tmp/direct-setup-failure.ib",
		User:     "SYSDBA",
	}, CreateOptions{}, func(Config, int) (*nativeConnection, error) {
		return native, setupError
	})
	if !errors.Is(err, setupError) {
		t.Fatalf("createDatabaseWithNative() error = %v, want setup error", err)
	}
	if attachment == nil {
		t.Fatal("setup failure lost created attachment")
	}
	if attachment.conn == nil || attachment.conn.native != native || attachment.conn.closed {
		var gotNative *nativeConnection
		if attachment.conn != nil {
			gotNative = attachment.conn.native
		}
		t.Fatalf("setup failure returned unusable attachment: conn=%p native=%p closed=%v",
			attachment.conn, gotNative, attachment.conn != nil && attachment.conn.closed)
	}
	if err := attachment.DropDatabase(context.Background()); err != nil {
		t.Fatalf("recovering setup-failed attachment with DropDatabase: %v", err)
	}
	if dropCalls != 1 || !attachment.conn.closed || attachment.conn.native != nil {
		t.Fatalf("recovered setup-failed attachment: calls=%d closed=%v native=%p",
			dropCalls, attachment.conn.closed, attachment.conn.native)
	}
}

func newDirectContextTestTransaction(t *testing.T) (*conn, *Attachment, *Transaction) {
	t.Helper()
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
		},
	}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
	}
	attachment.directTx = tx
	return connection, attachment, tx
}

func TestDirectOperationsRecheckContextAfterWaitingForConnection(t *testing.T) {
	tests := []struct {
		name string
		call func(context.Context, *Attachment, *Transaction) error
	}{
		{
			name: "plan",
			call: func(ctx context.Context, _ *Attachment, tx *Transaction) error {
				_, err := tx.Plan(ctx, "SELECT 1 FROM RDB$DATABASE")
				return err
			},
		},
		{
			name: "transaction info",
			call: func(ctx context.Context, _ *Attachment, tx *Transaction) error {
				_, err := tx.Info(ctx, InfoTransactionID)
				return err
			},
		},
		{
			name: "database info",
			call: func(ctx context.Context, attachment *Attachment, _ *Transaction) error {
				_, err := attachment.DatabaseInfo(ctx, InfoDatabaseVersion)
				return err
			},
		},
		{
			name: "drop database",
			call: func(ctx context.Context, attachment *Attachment, _ *Transaction) error {
				return attachment.DropDatabase(ctx)
			},
		},
		{
			name: "diagnostics",
			call: func(ctx context.Context, attachment *Attachment, _ *Transaction) error {
				_, err := attachment.Diagnostics(ctx)
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			connection, attachment, tx := newDirectContextTestTransaction(t)
			beforeLock := make(chan struct{})
			connection.directBeforeLock = func() { close(beforeLock) }
			connection.mu.Lock()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- tt.call(ctx, attachment, tx) }()
			<-beforeLock
			cancel()
			connection.mu.Unlock()

			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatalf("operation error = %v, want context.Canceled", err)
			}
		})
	}
}

func TestOpenBlobRechecksContextAfterLockedValidation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	connection, attachment, transaction := newDirectContextTestTransaction(t)
	transaction.native = &nativeTransaction{}
	connection.native.brokenOverride = func() bool {
		cancel()
		return false
	}
	ref := BlobRef{
		attachment: attachment,
		tx:         transaction,
		generation: transaction.generation,
		high:       1,
		low:        2,
	}

	_, err := transaction.OpenBlob(ctx, ref)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenBlob() error = %v, want context.Canceled", err)
	}
}

func TestOpenBlobClosesNativeStreamAfterPostOpenCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, attachment, transaction := newDirectContextTestTransaction(t)
	var stream *blobStream
	closeCalls := 0
	transaction.native = &nativeTransaction{
		openBlobOverride: func(int32, uint32, int16, int16) (*blobStream, error) {
			stream = &blobStream{
				closeNativeOverride: func(cancelled bool) (nativeBlobValue, error) {
					closeCalls++
					if !cancelled {
						t.Errorf("blob close cancellation = %v, want true", cancelled)
					}
					return nativeBlobValue{}, nil
				},
			}
			cancel()
			return stream, nil
		},
	}
	ref := BlobRef{
		attachment: attachment,
		tx:         transaction,
		generation: transaction.generation,
		high:       1,
		low:        2,
	}

	reader, err := transaction.OpenBlob(ctx, ref)
	if reader != nil {
		t.Fatalf("OpenBlob() returned reader %T after cancellation", reader)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("OpenBlob() error = %v, want context.Canceled", err)
	}
	if closeCalls != 1 || stream == nil || !stream.closed {
		t.Fatalf("post-open cleanup = calls %d, stream=%p closed=%v; want one cancelled close", closeCalls, stream, stream != nil && stream.closed)
	}
	if len(transaction.blobs) != 0 {
		t.Fatalf("transaction retained cancelled BLOB stream: %d", len(transaction.blobs))
	}
}
