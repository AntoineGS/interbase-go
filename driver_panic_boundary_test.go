package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
)

type panicBoundaryConnector struct {
	mu      sync.Mutex
	opened  int
	connect func(int) *conn
	last    *conn
}

func (c *panicBoundaryConnector) Connect(context.Context) (driver.Conn, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.opened++
	c.last = c.connect(c.opened)
	return c.last, nil
}

func (*panicBoundaryConnector) Driver() driver.Driver { return driverInstance }

func (c *panicBoundaryConnector) Opened() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.opened
}

func panicBoundaryConn(native *nativeConnection) *conn {
	if native.brokenOverride == nil {
		native.brokenOverride = func() bool { return false }
	}
	if native.closeOverride == nil {
		native.closeOverride = func() error { return nil }
	}
	return &conn{native: native}
}

func TestDatabaseSQLDriverPanicFailsWithoutRetryAndReconnectsNextCall(t *testing.T) {
	connector := &panicBoundaryConnector{connect: func(opened int) *conn {
		native := &nativeConnection{}
		if opened == 1 {
			native.execOverride = func(string, []argument, bool) (int64, error) {
				panic("injected statement execution panic")
			}
		} else {
			native.execOverride = func(string, []argument, bool) (int64, error) { return 1, nil }
		}
		return panicBoundaryConn(native)
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if _, err := db.ExecContext(context.Background(), "UPDATE T SET V = 1"); err == nil || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("panic execution error=%v; want a non-retryable error", err)
	}
	if opened := connector.Opened(); opened != 1 {
		t.Fatalf("first execution opened %d connections; unsafe statement was retried", opened)
	}
	connector.mu.Lock()
	invalid := connector.last
	connector.mu.Unlock()
	if invalid.IsValid() {
		t.Fatal("panicking connection still reports valid")
	}
	resetter, ok := any(invalid).(driver.SessionResetter)
	if !ok {
		t.Fatal("connection does not implement SessionResetter")
	}
	if err := resetter.ResetSession(context.Background()); !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ResetSession error=%v, want driver.ErrBadConn", err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE T SET V = 2"); err != nil {
		t.Fatalf("next execution did not reconnect: %v", err)
	}
	if opened := connector.Opened(); opened != 2 {
		t.Fatalf("connections opened=%d, want 2 after lazy reconnect", opened)
	}
}

func TestDatabaseSQLPreparePanicMayRetryOnFreshConnection(t *testing.T) {
	connector := &panicBoundaryConnector{connect: func(opened int) *conn {
		native := &nativeConnection{}
		if opened == 1 {
			native.prepareOverride = func(string) (*nativeStatement, error) {
				panic("injected prepare panic")
			}
		} else {
			native.prepareOverride = func(string) (*nativeStatement, error) {
				return &nativeStatement{numInputOverride: func() int { return 0 }}, nil
			}
		}
		return panicBoundaryConn(native)
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	statement, err := db.PrepareContext(context.Background(), "UPDATE T SET V = 1")
	if err != nil {
		t.Fatalf("safe prepare retry failed: %v", err)
	}
	defer statement.Close()
	if opened := connector.Opened(); opened != 2 {
		t.Fatalf("connections opened=%d, want retry on a fresh connection", opened)
	}
}

func TestDatabaseSQLStatementRowsAndTransactionPanicsReturnErrors(t *testing.T) {
	t.Run("prepared statement", func(t *testing.T) {
		connector := &panicBoundaryConnector{connect: func(int) *conn {
			native := &nativeConnection{prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return 0 },
					execOverride:     func([]argument) (int64, error) { panic("injected prepared exec panic") },
				}, nil
			}}
			return panicBoundaryConn(native)
		}}
		db := sql.OpenDB(connector)
		defer db.Close()
		statement, err := db.PrepareContext(context.Background(), "UPDATE T SET V = 1")
		if err != nil {
			t.Fatal(err)
		}
		defer statement.Close()
		if _, err := statement.ExecContext(context.Background()); err == nil || errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("prepared execution error=%v; want non-retryable error", err)
		}
	})

	t.Run("rows Next", func(t *testing.T) {
		connector := &panicBoundaryConnector{connect: func(int) *conn {
			native := &nativeConnection{prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return 0 },
					queryOverride: func([]argument) (*nativeCursor, []string, error) {
						return &nativeCursor{nextOverride: func() (bool, error) { panic("injected cursor panic") }}, []string{"V"}, nil
					},
				}, nil
			}}
			return panicBoundaryConn(native)
		}}
		db := sql.OpenDB(connector)
		defer db.Close()
		statement, err := db.PrepareContext(context.Background(), "SELECT V FROM T")
		if err != nil {
			t.Fatal(err)
		}
		defer statement.Close()
		rows, err := statement.QueryContext(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		if rows.Next() || rows.Err() == nil {
			t.Fatalf("rows after panic: Next=%v Err=%v", rows.Next(), rows.Err())
		}
	})

	t.Run("transaction commit", func(t *testing.T) {
		connector := &panicBoundaryConnector{connect: func(int) *conn {
			native := &nativeConnection{
				beginOverride:  func([]byte) error { return nil },
				commitOverride: func() error { panic("injected commit panic") },
			}
			return panicBoundaryConn(native)
		}}
		db := sql.OpenDB(connector)
		defer db.Close()
		tx, err := db.BeginTx(context.Background(), nil)
		if err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err == nil || errors.Is(err, driver.ErrBadConn) {
			t.Fatalf("commit error=%v; want non-retryable error", err)
		}
	})
}
