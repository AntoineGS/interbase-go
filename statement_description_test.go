package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
)

func TestDescribeStatementDoesNotExecute(t *testing.T) {
	prepare, close, execute := 0, 0, 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		prepare++
		return &nativeStatement{
			descriptionOverride: func() (StatementDescriptor, error) {
				return StatementDescriptor{Kind: "update", Mutating: true, InputCount: 1}, nil
			},
			execOverride:  func([]argument) (int64, error) { execute++; return 0, nil },
			closeOverride: func() error { close++; return nil },
		}, nil
	}
	got, err := (&conn{native: native}).DescribeStatement(context.Background(), "UPDATE T SET X=?")
	if err != nil || got.Kind != "update" || got.ReturnsRows || !got.Mutating || got.InputCount != 1 {
		t.Fatalf("description = %+v, %v", got, err)
	}
	if prepare != 1 || close != 1 || execute != 0 {
		t.Fatalf("prepare/close/execute = %d/%d/%d, want 1/1/0", prepare, close, execute)
	}
}

func TestDescribeStatementJoinsInspectionAndCloseFailures(t *testing.T) {
	inspectErr := errors.New("inspect failed")
	closeErr := errors.New("close failed")
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		return &nativeStatement{
			descriptionOverride: func() (StatementDescriptor, error) { return StatementDescriptor{}, inspectErr },
			closeOverride:       func() error { return closeErr },
		}, nil
	}
	if _, err := (&conn{native: native}).DescribeStatement(context.Background(), "SELECT 1 FROM RDB$DATABASE"); !errors.Is(err, inspectErr) || !errors.Is(err, closeErr) {
		t.Fatalf("error = %v, want inspect and close failures", err)
	}
}

func TestDescribeStatementRejectsClosedAndCanceledBeforePrepare(t *testing.T) {
	called := 0
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) {
		called++
		return nil, errors.New("unexpected prepare")
	}
	c := &conn{native: native}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.DescribeStatement(ctx, "SELECT 1 FROM RDB$DATABASE"); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.DescribeStatement(context.Background(), "SELECT 1 FROM RDB$DATABASE"); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("closed: %v", err)
	}
	if called != 0 {
		t.Fatalf("prepared %d times", called)
	}
}

func TestDescribeStatementPropagatesPrepareError(t *testing.T) {
	want := errors.New("prepare failed")
	native := introspectionTestNative(3)
	native.prepareOverride = func(string) (*nativeStatement, error) { return nil, want }
	if _, err := (&conn{native: native}).DescribeStatement(context.Background(), "SELECT 1 FROM RDB$DATABASE"); !errors.Is(err, want) {
		t.Fatalf("prepare: %v", err)
	}
}

func TestDescribeStatementRawRejectsForeignNilAndClosed(t *testing.T) {
	if _, err := DescribeStatement(context.Background(), nil, "SELECT 1"); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("nil conn: %v", err)
	}
	foreignDB := openDatabaseSQLTestDB(t, notInterBaseTestConn{})
	foreign, err := foreignDB.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer foreign.Close()
	if _, err := DescribeStatement(context.Background(), foreign, "SELECT 1"); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("foreign conn: %v", err)
	}
	db := openDatabaseSQLTestDB(t, &conn{native: introspectionTestNative(3)})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := pooled.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := DescribeStatement(context.Background(), pooled, "SELECT 1"); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("closed pooled conn: %v", err)
	}
}
