package interbase

import (
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type synchronizedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
	changed chan struct{}
}

func (b *synchronizedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	n, err := b.Buffer.Write(value)
	b.mu.Unlock()
	select {
	case b.changed <- struct{}{}:
	default:
	}
	return n, err
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

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

func TestResetSessionPanicReturnsBadConn(t *testing.T) {
	native := &nativeConnection{brokenOverride: func() bool { panic("injected reset validation panic") }}
	connection := panicBoundaryConn(native)
	err := connection.ResetSession(context.Background())
	if !errors.Is(err, driver.ErrBadConn) || errors.Is(err, ErrDriverPanic) {
		t.Fatalf("ResetSession panic error=%v; want driver.ErrBadConn only", err)
	}
	if connection.IsValid() {
		t.Fatal("ResetSession panic left the connection valid")
	}
}

func TestNewConnectorCopiesDriverPanicObserver(t *testing.T) {
	var originalCalls, replacementCalls atomic.Int32
	observer := &DriverPanicObserver{Notify: func(error) { originalCalls.Add(1) }}
	configured, err := NewConnector(Config{Database: "test.fdb", User: "SYSDBA", DriverPanicObserver: observer})
	if err != nil {
		t.Fatal(err)
	}
	observer.Notify = func(error) { replacementCalls.Add(1) }
	stored := configured.(*connector).cfg.DriverPanicObserver
	if stored == observer {
		t.Fatal("connector retained the caller-owned panic observer wrapper")
	}
	stored.Notify(ErrDriverPanic)
	if originalCalls.Load() != 1 || replacementCalls.Load() != 0 {
		t.Fatalf("observer calls original=%d replacement=%d; want 1 and 0", originalCalls.Load(), replacementCalls.Load())
	}
}

func TestDatabaseSQLResetSessionPanicNotifiesAndRetriesOnFreshConnection(t *testing.T) {
	observed := make(chan error, 1)
	var resetValidationCalls atomic.Int32
	connector := &panicBoundaryConnector{connect: func(opened int) *conn {
		native := &nativeConnection{execOverride: func(string, []argument, bool) (int64, error) { return 1, nil }}
		if opened == 1 {
			native.brokenOverride = func() bool {
				if resetValidationCalls.Add(1) == 2 {
					panic("injected reset validation panic")
				}
				return false
			}
		}
		connection := panicBoundaryConn(native)
		connection.resetPanicObserver = func(err error) { observed <- err }
		return connection
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.ExecContext(context.Background(), "UPDATE T SET V = 1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.ExecContext(context.Background(), "UPDATE T SET V = 2"); err != nil {
		t.Fatalf("database/sql did not safely retry on a fresh connection: %v", err)
	}
	if opened := connector.Opened(); opened != 2 {
		t.Fatalf("connections opened=%d, want one fresh connection after reset panic", opened)
	}
	select {
	case err := <-observed:
		if !errors.Is(err, ErrDriverPanic) {
			t.Fatalf("reset panic diagnostic=%v; want ErrDriverPanic", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ResetSession panic was not reported to the owning application session")
	}
}

func TestDatabaseSQLPreparePanicIsNonRetryable(t *testing.T) {
	connector := &panicBoundaryConnector{connect: func(int) *conn {
		native := &nativeConnection{}
		native.prepareOverride = func(string) (*nativeStatement, error) {
			panic("injected prepare panic")
		}
		return panicBoundaryConn(native)
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	statement, err := db.PrepareContext(context.Background(), "UPDATE T SET V = 1")
	if statement != nil {
		_ = statement.Close()
	}
	if !errors.Is(err, ErrDriverPanic) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("prepare panic error=%v; want non-retryable ErrDriverPanic", err)
	}
	if opened := connector.Opened(); opened != 1 {
		t.Fatalf("prepare panic opened %d connections; want no retry", opened)
	}
}

func TestPinnedDatabaseSQLConnReportsDriverPanicAndLogsCleanupPanic(t *testing.T) {
	diagnostics := synchronizedBuffer{changed: make(chan struct{}, 1)}
	previousOutput, previousFlags := log.Writer(), log.Flags()
	log.SetOutput(&diagnostics)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(previousOutput)
		log.SetFlags(previousFlags)
	}()

	connector := &panicBoundaryConnector{connect: func(int) *conn {
		native := &nativeConnection{
			execOverride: func(string, []argument, bool) (int64, error) {
				panic("sensitive native panic detail")
			},
			closeOverride: func() error { panic("injected native cleanup panic") },
		}
		return panicBoundaryConn(native)
	}}
	db := sql.OpenDB(connector)
	defer db.Close()
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	pinned, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	_, err = pinned.ExecContext(context.Background(), "UPDATE T SET V = 1")
	if !errors.Is(err, ErrDriverPanic) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("pinned execution error=%v; want non-retryable ErrDriverPanic", err)
	}
	if strings.Contains(err.Error(), "sensitive native panic detail") {
		t.Fatalf("returned driver error exposed panic detail: %v", err)
	}
	if opened := connector.Opened(); opened != 1 {
		t.Fatalf("pinned execution opened %d connections; want exactly one", opened)
	}
	if _, nextErr := pinned.ExecContext(context.Background(), "UPDATE T SET V = 2"); !errors.Is(nextErr, ErrDriverPanic) || errors.Is(nextErr, driver.ErrBadConn) {
		t.Fatalf("next pinned operation error=%v; want the recorded ErrDriverPanic", nextErr)
	}
	if err := pinned.Close(); err != nil {
		t.Fatalf("close pinned connection: %v", err)
	}

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	for {
		logs := diagnostics.String()
		if strings.Contains(logs, "sensitive native panic detail") &&
			strings.Contains(logs, "injected native cleanup panic") &&
			strings.Contains(logs, "goroutine") {
			return
		}
		select {
		case <-diagnostics.changed:
		case <-deadline.C:
			t.Fatalf("driver panic and cleanup diagnostics were not both logged:\n%s", diagnostics.String())
		}
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
