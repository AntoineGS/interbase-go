package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"net"
	"sync"
	"testing"
	"time"
)

type databaseSQLTestConnector struct {
	connection driver.Conn
}

func (c databaseSQLTestConnector) Connect(context.Context) (driver.Conn, error) {
	return c.connection, nil
}

func (databaseSQLTestConnector) Driver() driver.Driver {
	return databaseSQLTestDriver{}
}

type databaseSQLTestDriver struct{}

func (databaseSQLTestDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("database/sql test driver does not support DSN opening")
}

func openDatabaseSQLTestDB(t *testing.T, connection driver.Conn) *sql.DB {
	t.Helper()
	db := sql.OpenDB(databaseSQLTestConnector{connection: connection})
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing database/sql test DB: %v", err)
		}
	})
	return db
}

// databaseSQLTestFactoryConnector mints a fresh driver.Conn per Connect call,
// unlike databaseSQLTestConnector, which returns one fixed connection. Tests
// that must observe the pool discarding a connection need a distinguishable
// replacement.
type databaseSQLTestFactoryConnector struct {
	mu      sync.Mutex
	factory func() driver.Conn
	minted  []driver.Conn
}

func (c *databaseSQLTestFactoryConnector) Connect(context.Context) (driver.Conn, error) {
	connection := c.factory()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.minted = append(c.minted, connection)
	return connection, nil
}

func (*databaseSQLTestFactoryConnector) Driver() driver.Driver {
	return databaseSQLTestDriver{}
}

func (c *databaseSQLTestFactoryConnector) mintedConnections() []driver.Conn {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]driver.Conn(nil), c.minted...)
}

func openDatabaseSQLTestFactoryDB(t *testing.T, factory func() driver.Conn) (*sql.DB,
	*databaseSQLTestFactoryConnector) {
	t.Helper()
	connector := &databaseSQLTestFactoryConnector{factory: factory}
	db := sql.OpenDB(connector)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("closing database/sql test DB: %v", err)
		}
	})
	return db, connector
}

func TestConfigConnectTimeoutRoundsUpToWholeNativeSeconds(t *testing.T) {
	base := Config{
		Database: "/tmp/example.ib",
		User:     "alice",
	}
	tests := []struct {
		name  string
		input time.Duration
		want  time.Duration
	}{
		{name: "zero preserves native default", input: 0, want: 0},
		{name: "exact second", input: 3 * time.Second, want: 3 * time.Second},
		{name: "fraction rounds upward", input: 1500 * time.Millisecond, want: 2 * time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.ConnectTimeout = test.input
			opened, err := NewConnector(cfg)
			if err != nil {
				t.Fatalf("NewConnector returned error: %v", err)
			}
			got := opened.(*connector).cfg.ConnectTimeout
			if got != test.want {
				t.Fatalf("normalized ConnectTimeout = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNewConnectorRejectsInvalidConnectTimeout(t *testing.T) {
	maxSeconds := uint64(^uint32(0))
	maxTimeout := time.Duration(maxSeconds) * time.Second
	base := Config{
		Database: "/tmp/example.ib",
		User:     "alice",
	}
	for _, test := range []struct {
		name  string
		input time.Duration
	}{
		{name: "negative", input: -time.Nanosecond},
		{name: "exceeds native uint32 seconds", input: maxTimeout + time.Nanosecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base
			cfg.ConnectTimeout = test.input
			opened, err := NewConnector(cfg)
			if opened != nil || err == nil {
				t.Fatalf("NewConnector(%v) = (%v, %v), want validation error", test.input, opened, err)
			}
		})
	}
}

func TestNativeConnectTimeoutBoundsUnresponsiveAttachment(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	accepted := make(chan net.Conn, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr == nil {
			accepted <- connection
		}
	}()
	host, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatalf("net.SplitHostPort() error = %v", err)
	}

	started := time.Now()
	_, openErr := openNative(Config{
		Database:       "/tmp/interbase-go-connect-timeout.ib",
		Host:           host + "/" + port,
		User:           "SYSDBA",
		Password:       "masterkey",
		ConnectTimeout: time.Second,
	})
	elapsed := time.Since(started)
	select {
	case connection := <-accepted:
		_ = connection.Close()
	default:
		t.Fatal("native attach did not reach the test listener")
	}
	if openErr == nil {
		t.Fatal("openNative() unexpectedly succeeded against an unresponsive listener")
	}
	if elapsed < 750*time.Millisecond || elapsed > 5*time.Second {
		t.Fatalf("native attach elapsed %v, want approximately the configured 1s timeout", elapsed)
	}
}

func TestCreateDatabaseNormalizesConnectTimeoutForDirectAttachment(t *testing.T) {
	var received Config
	native := &nativeConnection{brokenOverride: func() bool { return false }}
	attachment, err := createDatabaseWithNative(context.Background(), Config{
		Database:       "/tmp/example.ib",
		User:           "alice",
		ConnectTimeout: 1500 * time.Millisecond,
	}, CreateOptions{}, func(cfg Config, _ int) (*nativeConnection, error) {
		received = cfg
		return native, nil
	})
	if err != nil {
		t.Fatalf("createDatabaseWithNative() returned error: %v", err)
	}
	if attachment == nil {
		t.Fatal("createDatabaseWithNative() returned no attachment")
	}
	defer attachment.Close()
	if received.ConnectTimeout != 2*time.Second {
		t.Fatalf("direct attachment ConnectTimeout = %v, want 2s", received.ConnectTimeout)
	}
}

func TestExecContextPreCancellationDoesNotEnterNativeCall(t *testing.T) {
	called := false
	connection := &conn{
		native: &nativeConnection{
			execOverride: func(string, []argument, bool) (int64, error) {
				called = true
				return 1, nil
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	result, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
	if result != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled ExecContext() = (%v, %v), want nil and context.Canceled", result, err)
	}
	if called {
		t.Fatal("pre-canceled ExecContext entered the native operation")
	}
}

func TestExecContextReturnsKnownSuccessAfterPostOperationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			execOverride: func(string, []argument, bool) (int64, error) {
				cancel()
				return 7, nil
			},
		},
	}

	result, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
	if err != nil {
		t.Fatalf("post-operation cancellation error = %v, want nil after known native success", err)
	}
	if result == nil {
		t.Fatal("post-operation cancellation discarded the known driver result")
	}
	affected, rowsErr := result.RowsAffected()
	if rowsErr != nil || affected != 7 {
		t.Fatalf("known rows affected = (%d, %v), want (7, nil)", affected, rowsErr)
	}
	if errors.Is(err, driver.ErrBadConn) || connection.closed {
		t.Fatalf("post-operation cancellation was classified as a bad connection: err=%v closed=%v", err, connection.closed)
	}
}

func TestDatabaseSQLExecContextReturnsKnownSuccessAfterNativeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			execOverride: func(string, []argument, bool) (int64, error) {
				cancel()
				return 7, nil
			},
		},
	}
	db := openDatabaseSQLTestDB(t, connection)

	result, err := db.ExecContext(ctx, "UPDATE example SET value = 1")
	if err != nil {
		t.Fatalf("DB.ExecContext() error = %v, want nil after known native success", err)
	}
	if result == nil {
		t.Fatal("DB.ExecContext() returned a nil result after known native success")
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 7 {
		t.Fatalf("DB.ExecContext() rows affected = (%d, %v), want (7, nil)", affected, err)
	}
}

func TestDatabaseSQLPreparedExecContextReturnsKnownSuccessAfterNativeCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			prepareOverride: func(string) (*nativeStatement, error) {
				return &nativeStatement{
					numInputOverride: func() int { return -1 },
					execOverride: func([]argument) (int64, error) {
						cancel()
						return 11, nil
					},
				}, nil
			},
		},
	}
	db := openDatabaseSQLTestDB(t, connection)

	statement, err := db.PrepareContext(context.Background(), "UPDATE example SET value = 1")
	if err != nil {
		t.Fatalf("DB.PrepareContext() error = %v", err)
	}
	defer statement.Close()

	result, err := statement.ExecContext(ctx)
	if err != nil {
		t.Fatalf("Stmt.ExecContext() error = %v, want nil after known native success", err)
	}
	if result == nil {
		t.Fatal("Stmt.ExecContext() returned a nil result after known native success")
	}
	affected, err := result.RowsAffected()
	if err != nil || affected != 11 {
		t.Fatalf("Stmt.ExecContext() rows affected = (%d, %v), want (11, nil)", affected, err)
	}
}

func TestExecContextPreservesNativeErrorWhenContextCancelsAfterFailure(t *testing.T) {
	nativeErr := &Error{Operation: "execute", NativeCode: 335544366, Message: "statement failed"}
	ctx, cancel := context.WithCancel(context.Background())
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			execOverride: func(string, []argument, bool) (int64, error) {
				cancel()
				return 0, nativeErr
			},
		},
	}

	result, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
	if result != nil {
		t.Fatalf("native failure returned a driver result: %v", result)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("native failure was replaced with cancellation or bad-connection classification: %v", err)
	}
	var gotNative *Error
	if !errors.As(err, &gotNative) || gotNative.NativeCode != nativeErr.NativeCode {
		t.Fatalf("native error identity = %v, want native status %d", err, nativeErr.NativeCode)
	}
	if connection.closed {
		t.Fatal("known native failure unexpectedly invalidated a live connection")
	}
}

func TestDatabaseSQLSessionControlIsRejectedBeforePoolUse(t *testing.T) {
	const wantError = "interbase: subscription session state requires the explicit direct API"
	queries := []string{
		"SET SUBSCRIPTION SUB_CUSTOMER_CHANGE ACTIVE",
		"-- comment\n SET /* comment */ SUBSCRIPTION SUB_CUSTOMER_CHANGE INACTIVE",
	}
	for _, query := range queries {
		t.Run(query, func(t *testing.T) {
			connection := &conn{}
			if _, err := connection.ExecContext(context.Background(), query, nil); err == nil || err.Error() != wantError {
				t.Fatalf("ExecContext() error = %v, want %q", err, wantError)
			}
			if _, err := connection.QueryContext(context.Background(), query, nil); err == nil || err.Error() != wantError {
				t.Fatalf("QueryContext() error = %v, want %q", err, wantError)
			}
			if _, err := connection.PrepareContext(context.Background(), query); err == nil || err.Error() != wantError {
				t.Fatalf("PrepareContext() error = %v, want %q", err, wantError)
			}
			if connection.closed {
				t.Fatal("session-state admission failure poisoned the connection")
			}
		})
	}
}
