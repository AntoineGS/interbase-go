package interbase

import (
	"context"
	"errors"
	"testing"
)

func introspectionInfoPayload(code byte, data []byte) []byte {
	result := make([]byte, 0, len(data)+4)
	result = append(result, code, byte(len(data)), byte(len(data)>>8))
	result = append(result, data...)
	return append(result, infoEnd)
}

// introspectionInfoResponder reproduces the native information framing used by
// TestAttachmentDiagnosticsDecodesNativeInfoAndClientVersion
// (direct_lifecycle_test.go:869), with a selectable SQL dialect payload.
func introspectionInfoResponder(dialect byte) func(byte) ([]byte, error) {
	return func(code byte) ([]byte, error) {
		switch code {
		case InfoDatabaseVersion:
			return introspectionInfoPayload(code, []byte{1, 3, 'V', '1', '0'}), nil
		case InfoDatabaseODSVersion:
			return introspectionInfoPayload(code, []byte{13, 0}), nil
		case InfoDatabaseODSMinorVersion:
			return introspectionInfoPayload(code, []byte{1, 0}), nil
		case InfoDatabasePageSize:
			return introspectionInfoPayload(code, []byte{0, 16, 0, 0}), nil
		case InfoDatabaseSQLDialect:
			return introspectionInfoPayload(code, []byte{dialect}), nil
		case InfoDatabaseReadOnly:
			return introspectionInfoPayload(code, []byte{0}), nil
		default:
			return nil, errors.New("unexpected info code")
		}
	}
}

func introspectionTestNative(dialect byte) *nativeConnection {
	return &nativeConnection{
		brokenOverride:        func() bool { return false },
		databaseInfoOverride:  introspectionInfoResponder(dialect),
		clientVersionOverride: func() (string, error) { return "client-v1", nil },
		closeOverride:         func() error { return nil },
	}
}

func introspectionWantDiagnostics(dialect int64) DatabaseDiagnostics {
	return DatabaseDiagnostics{
		ClientVersion:   "client-v1",
		ServerVersion:   "V10",
		DatabaseVersion: "13.1",
		ODSVersion:      13,
		ODSMinorVersion: 1,
		PageSize:        4096,
		SQLDialect:      dialect,
		ReadOnly:        false,
	}
}

func TestConnDiagnosticsDecodesEveryFieldForEachDialect(t *testing.T) {
	for _, dialect := range []byte{1, 3} {
		connection := &conn{native: introspectionTestNative(dialect)}
		got, err := connection.Diagnostics(context.Background())
		if err != nil {
			t.Fatalf("dialect %d: (*conn).Diagnostics() error = %v", dialect, err)
		}
		want := introspectionWantDiagnostics(int64(dialect))
		if got != want {
			t.Fatalf("dialect %d: (*conn).Diagnostics() = %#v, want %#v", dialect, got, want)
		}
	}
}

func TestAttachmentDiagnosticsAndConnDiagnosticsAgree(t *testing.T) {
	attachmentConn := &conn{native: introspectionTestNative(3)}
	attachment := &Attachment{conn: attachmentConn}
	fromAttachment, err := attachment.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("Attachment.Diagnostics() error = %v", err)
	}
	pooledConn := &conn{native: introspectionTestNative(3)}
	fromConn, err := pooledConn.Diagnostics(context.Background())
	if err != nil {
		t.Fatalf("(*conn).Diagnostics() error = %v", err)
	}
	if fromAttachment != fromConn {
		t.Fatalf("Attachment.Diagnostics() = %#v, (*conn).Diagnostics() = %#v; the shared implementation drifted",
			fromAttachment, fromConn)
	}
}
