package services

import (
	"context"
	"errors"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

func TestManagerStartCancellationWhileNativeGateWaitsDoesNotStartAction(t *testing.T) {
	backend := &gateStartBackend{started: make(chan struct{})}
	manager := newTestManager(backend)
	releaseGate := nativegate.Global.EnterExclusive()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.Start(ctx, LogRequest{})
		result <- err
	}()
	select {
	case <-backend.started:
		t.Fatal("Manager.Start() entered native start while lifecycle gate was held")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	releaseGate()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Manager.Start() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Manager.Start() did not return after gate cancellation")
	}
	select {
	case <-backend.started:
		t.Fatal("canceled Manager.Start() invoked native start")
	default:
	}
}

func TestServiceInfoCancellationWhileNativeGateWaitsDoesNotQuery(t *testing.T) {
	backend := &gateQueryBackend{queried: make(chan struct{})}
	manager := newTestManager(backend)
	releaseGate := nativegate.Global.EnterExclusive()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := manager.ServiceManagerVersion(ctx)
		result <- err
	}()
	select {
	case <-backend.queried:
		t.Fatal("service information query entered while lifecycle gate was held")
	case <-time.After(20 * time.Millisecond):
	}
	cancel()
	releaseGate()

	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("ServiceManagerVersion() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("ServiceManagerVersion() did not return after gate cancellation")
	}
	select {
	case <-backend.queried:
		t.Fatal("canceled service information query entered native query")
	default:
	}
}

type gateStartBackend struct {
	started chan struct{}
}

func (b *gateStartBackend) start([]byte) error {
	close(b.started)
	return nil
}

func (b *gateStartBackend) query(byte, int) ([]byte, bool, error) {
	return []byte{infoEnd}, false, nil
}

func (b *gateStartBackend) close() error { return nil }

type gateQueryBackend struct {
	queried chan struct{}
}

func (b *gateQueryBackend) start([]byte) error { return nil }

func (b *gateQueryBackend) query(byte, int) ([]byte, bool, error) {
	close(b.queried)
	return []byte{infoVersion, 2, 0, 1, 0, infoEnd}, false, nil
}

func (b *gateQueryBackend) close() error { return nil }

func TestNativeServiceCloseUsesLifecycleGate(t *testing.T) {
	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	closeCalled := make(chan struct{})
	service := &nativeService{
		closeOverride: func() error {
			close(closeCalled)
			return nil
		},
	}
	closeDone := make(chan error, 1)
	go func() {
		closeDone <- service.close()
	}()

	select {
	case <-closeCalled:
		releaseLifecycle()
		t.Fatal("service close entered while lifecycle gate was held")
	case <-time.After(20 * time.Millisecond):
	}

	releaseLifecycle()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("service close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("service close did not resume after lifecycle gate release")
	}
}
