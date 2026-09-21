package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
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

func TestNativeStatementPlanUsesOverrideWithoutNativePointer(t *testing.T) {
	statement := &nativeStatement{
		planOverride: func() (string, error) { return "PLAN (GO_COUNTRY INDEX (PK_GO_COUNTRY))", nil },
	}
	plan, err := statement.plan()
	if err != nil {
		t.Fatalf("(*nativeStatement).plan() error = %v", err)
	}
	if plan != "PLAN (GO_COUNTRY INDEX (PK_GO_COUNTRY))" {
		t.Fatalf("(*nativeStatement).plan() = %q, want the override value", plan)
	}
}

func TestNativeStatementPlanReturnsOverrideError(t *testing.T) {
	planErr := errors.New("injected plan failure")
	statement := &nativeStatement{
		planOverride: func() (string, error) { return "", planErr },
	}
	if _, err := statement.plan(); !errors.Is(err, planErr) {
		t.Fatalf("(*nativeStatement).plan() error = %v, want the override error", err)
	}
}

func TestConnPlanReturnsPlanAndClosesTheStatementExactlyOnce(t *testing.T) {
	closeCalls := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			planOverride:  func() (string, error) { return "PLAN (GO_COUNTRY NATURAL)", nil },
			closeOverride: func() error { closeCalls++; return nil },
			// The live suite is the real prepare-only proof, but it needs a
			// server. This makes a regression fail fast in `make test`: if a
			// later edit to (*conn).Plan ever reaches an execute path, this
			// fires immediately instead of waiting for the Docker fixture.
			execOverride: func([]argument) (int64, error) {
				t.Fatal("(*conn).Plan executed the statement; it must only prepare")
				return 0, nil
			},
		}, nil
	}
	connection := &conn{native: native}
	plan, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY")
	if err != nil {
		t.Fatalf("(*conn).Plan() error = %v", err)
	}
	if plan != "PLAN (GO_COUNTRY NATURAL)" {
		t.Fatalf("(*conn).Plan() = %q, want the native plan", plan)
	}
	if closeCalls != 1 {
		t.Fatalf("statement close calls = %d, want 1", closeCalls)
	}
}

func TestConnPlanClosesTheStatementAndJoinsBothFailures(t *testing.T) {
	planErr := errors.New("injected plan failure")
	closeErr := errors.New("injected close failure")
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			planOverride:  func() (string, error) { return "", planErr },
			closeOverride: func() error { return closeErr },
		}, nil
	}
	connection := &conn{native: native}
	plan, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY")
	if plan != "" {
		t.Fatalf("(*conn).Plan() = %q, want an empty plan on failure", plan)
	}
	if !errors.Is(err, planErr) || !errors.Is(err, closeErr) {
		t.Fatalf("(*conn).Plan() error = %v, want both the plan and close failures", err)
	}
}

func TestConnPlanRejectsInvalidQueriesBeforeNativePrepare(t *testing.T) {
	tests := []struct {
		name  string
		query string
	}{
		{name: "NUL byte", query: "SELECT 1 FROM RDB$DATABASE\x00"},
		{name: "oversized", query: strings.Repeat("a", math.MaxUint16+1)},
		{name: "subscription session control", query: "SET SUBSCRIPTION MY_SUB ACTIVE"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			prepareCalls := 0
			native := introspectionTestNative(3)
			native.prepareOverride = func(string) (*nativeStatement, error) {
				prepareCalls++
				return nil, errors.New("native prepare must not be reached")
			}
			connection := &conn{native: native}
			if _, err := connection.Plan(context.Background(), test.query); err == nil {
				t.Fatal("(*conn).Plan() accepted a query the pooled validator rejects")
			}
			if prepareCalls != 0 {
				t.Fatalf("native prepare calls = %d, want 0", prepareCalls)
			}
		})
	}
}

func TestConnPlanInvalidatesTheConnectionWhenNativePrepareBreaksIt(t *testing.T) {
	prepareErr := &NativeError{
		Operation:  "prepare",
		NativeCode: 335544721,
		Message:    "connection lost",
	}
	broken := false
	native := introspectionTestNative(3)
	native.brokenOverride = func() bool { return broken }
	native.prepareOverride = func(string) (*nativeStatement, error) {
		broken = true
		return nil, prepareErr
	}
	connection := &conn{native: native}
	if _, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY"); !errors.Is(err, prepareErr) {
		t.Fatalf("(*conn).Plan() error = %v, want the native prepare failure", err)
	}
	if connection.IsValid() {
		t.Fatal("(*conn).IsValid() is true after a native failure that broke the attachment")
	}
}

func TestConnPlanOnClosedConnectionReturnsErrBadConn(t *testing.T) {
	prepareCalls := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		prepareCalls++
		return nil, errors.New("native prepare must not be reached")
	}
	connection := &conn{native: native}
	if err := connection.Close(); err != nil {
		t.Fatalf("(*conn).Close() error = %v", err)
	}
	if _, err := connection.Plan(context.Background(), "SELECT COUNTRY FROM GO_COUNTRY"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("(*conn).Plan() error = %v, want driver.ErrBadConn", err)
	}
	if prepareCalls != 0 {
		t.Fatalf("native prepare calls = %d, want 0", prepareCalls)
	}
}
