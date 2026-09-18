package interbase

import (
	"context"
	"errors"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

func TestDistributedPrepareCancellationBeforeCursorCleanupPreservesOwnership(t *testing.T) {
	checked := make(chan struct{}, 1)
	closed := make(chan struct{}, 1)
	a := &Attachment{conn: &conn{native: &nativeConnection{brokenOverride: func() bool {
		checked <- struct{}{}
		return false
	}}}}
	d := &DistributedTransaction{}
	d.state.Store(uint32(distributedStateActive))
	tx := &Transaction{attachment: a, distributed: d, cursors: map[*Cursor]struct{}{}}
	cur := &Cursor{tx: tx, native: &nativeCursor{closeOverride: func() error {
		closed <- struct{}{}
		return nil
	}}}
	tx.cursors[cur] = struct{}{}
	d.participants = []*Transaction{tx}
	release := nativegate.Global.EnterExclusive()
	gateReleased := false
	defer func() {
		if !gateReleased {
			release()
		}
	}()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- d.Prepare(ctx) }()
	select {
	case <-checked:
	case <-time.After(time.Second):
		t.Fatal("Prepare did not validate the participant")
	}
	cancel()
	var err error
	select {
	case err = <-done:
	case <-time.After(100 * time.Millisecond):
		release()
		gateReleased = true
		select {
		case err = <-done:
		case <-time.After(time.Second):
			t.Fatal("Prepare did not resume after releasing the lifecycle gate")
		}
		t.Fatalf("Prepare remained blocked on cursor cleanup after cancellation: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Prepare() error = %v, want context.Canceled", err)
	}
	if _, ok := tx.cursors[cur]; !ok {
		t.Fatal("canceled Prepare removed the participant cursor from ownership")
	}
	if cur.closed {
		t.Fatal("canceled Prepare marked the participant cursor closed")
	}
	if cur.native == nil {
		t.Fatal("canceled Prepare cleared the participant cursor native handle")
	}
	release()
	gateReleased = true
	select {
	case <-closed:
		t.Error("canceled Prepare entered native cursor close after lifecycle release")
	default:
	}
}
