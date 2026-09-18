package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

func TestExecContextCancellationWhileNativeGateWaitsDoesNotEnterNativeCall(t *testing.T) {
	called := make(chan struct{})
	connection := &conn{
		native: &nativeConnection{
			execOverride: func(string, []argument, bool) (int64, error) {
				close(called)
				return 1, nil
			},
		},
	}
	releaseGate := nativegate.Global.EnterExclusive()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
		result <- err
	}()

	select {
	case <-called:
		t.Fatal("ExecContext entered the native operation while the gate was held")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	releaseGate()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ExecContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ExecContext() did not return after gate cancellation")
	}
	select {
	case <-called:
		t.Fatal("canceled ExecContext entered the native operation after gate release")
	default:
	}
}

func TestRootExecContextCancellationWhileOwningConnectionLock(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	nativeErr := &Error{
		Operation:  "execute statement",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	connection := &conn{
		native: &nativeConnection{
			brokenOverride: func() bool { return false },
			execOverride: func(string, []argument, bool) (int64, error) {
				close(entered)
				<-release
				return 0, nativeErr
			},
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
		result <- err
	}()
	<-entered

	lockAcquired := make(chan struct{})
	go func() {
		connection.mu.Lock()
		close(lockAcquired)
		connection.mu.Unlock()
	}()
	cancel()
	select {
	case <-lockAcquired:
		t.Fatal("root operation released its connection lock before native completion")
	case <-time.After(20 * time.Millisecond):
	}
	close(release)

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("root ExecContext() error = %v, want context.Canceled", err)
		}
		if errors.Is(err, driver.ErrBadConn) {
			t.Fatal("root execution cancellation was classified as driver.ErrBadConn")
		}
	case <-time.After(time.Second):
		t.Fatal("root ExecContext() did not return after native completion")
	}
	select {
	case <-lockAcquired:
	case <-time.After(time.Second):
		t.Fatal("root operation did not release its connection lock")
	}
}

func TestNativeConnectionCloseUsesLifecycleGate(t *testing.T) {
	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()

	closeCalled := make(chan struct{})
	connection := &nativeConnection{
		closeOverride: func() error {
			close(closeCalled)
			return nil
		},
	}
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- connection.close()
	}()

	select {
	case <-closeCalled:
		t.Fatal("native connection close entered while lifecycle gate was held")
	case <-time.After(20 * time.Millisecond):
	}

	releaseLifecycle()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("native connection close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("native connection close did not resume after lifecycle gate release")
	}
}

func TestNativeConnectionCallWaitsForActiveLifecycle(t *testing.T) {
	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()

	callCalled := make(chan struct{})
	connection := &nativeConnection{
		execOverride: func(string, []argument, bool) (int64, error) {
			close(callCalled)
			return 0, nil
		},
	}
	callDone := make(chan error, 1)
	go func() {
		_, err := connection.exec(context.Background(), "SELECT 1", nil, false)
		callDone <- err
	}()

	select {
	case <-callCalled:
		releaseLifecycle()
		t.Fatal("native call entered while lifecycle gate was held")
	case <-time.After(20 * time.Millisecond):
	}

	releaseLifecycle()
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("native call: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("native call did not resume after lifecycle gate release")
	}
}
