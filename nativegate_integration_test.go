package interbase

import (
	"context"
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
		_, err := connection.exec("SELECT 1", nil, false)
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
