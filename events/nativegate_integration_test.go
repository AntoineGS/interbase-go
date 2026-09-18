package events

import (
	"context"
	"errors"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

func TestNativeEventStopUsesLifecycleGate(t *testing.T) {
	testSubscription := newTestNativeSubscription(t, "event_a")
	defer destroyTestNativeSubscription(t, testSubscription)
	native := &nativeSubscription{pointer: testSubscription.pointer}

	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	stopCalled := make(chan struct{})
	stopDone := make(chan error, 1)
	go func() {
		close(stopCalled)
		stopDone <- native.stop()
	}()
	<-stopCalled

	select {
	case err := <-stopDone:
		releaseLifecycle()
		t.Fatalf("event stop entered while lifecycle gate was held: %v", err)
	case <-time.After(20 * time.Millisecond):
	}

	releaseLifecycle()
	select {
	case err := <-stopDone:
		if err != nil {
			t.Fatalf("event stop: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("event stop did not resume after lifecycle gate release")
	}
}

func TestNativeEventCallbackWaitsForOrdinaryGateWithoutBlockingClose(t *testing.T) {
	testSubscription := newTestNativeSubscription(t, "event_a")
	defer destroyTestNativeSubscription(t, testSubscription)

	if err := testSubscription.holdCallback(); err != nil {
		t.Fatal(err)
	}
	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	emitDone := make(chan error, 1)
	go func() {
		emitDone <- testSubscription.emitCounts(0, []uint64{0})
	}()
	if entered, err := testSubscription.waitCallback(time.Second); err != nil {
		t.Fatalf("wait for callback entry: %v", err)
	} else if !entered {
		t.Fatal("callback did not reach the deterministic hold")
	}
	if err := testSubscription.releaseCallback(); err != nil {
		t.Fatalf("release callback hold: %v", err)
	}

	select {
	case err := <-emitDone:
		releaseLifecycle()
		t.Fatalf("callback bypassed ordinary native gate: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	releaseLifecycle()
	select {
	case err := <-emitDone:
		if err != nil {
			t.Fatalf("callback after ordinary gate release: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("callback did not resume after ordinary gate release")
	}
}

func TestNativeEventContextAdmissionCanCancelBeforeNativeWait(t *testing.T) {
	testSubscription := newTestNativeSubscription(t, "event_a")
	defer destroyTestNativeSubscription(t, testSubscription)
	native := &nativeSubscription{pointer: testSubscription.pointer}

	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := native.wait(ctx, time.Second); err == nil {
		releaseLifecycle()
		t.Fatal("native wait entered while its context was canceled")
	} else if !errors.Is(err, context.DeadlineExceeded) {
		releaseLifecycle()
		t.Fatalf("native wait error = %v, want context deadline", err)
	}
	releaseLifecycle()
}

func TestNativeSubscribeContextCanCancelBeforeOpenAdmission(t *testing.T) {
	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := openNative(ctx, Config{
		Database: "not-used-by-canceled-open",
		User:     "SYSDBA",
	}, "not-used-by-canceled-open", "UTF8", 3, []string{"event_a"})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("openNative() error = %v, want context deadline", err)
	}
}

func TestPublicSubscribePreservesContextAdmissionError(t *testing.T) {
	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err := Subscribe(ctx, Config{
		Database: "not-used-by-canceled-subscribe",
		User:     "SYSDBA",
		Password: "masterkey",
	}, "event_a")
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Subscribe() error = %v, want context deadline identity", err)
	}
}

func TestPublicNextWaitAdmissionErrorPreservesSubscription(t *testing.T) {
	subscription, testSubscription, native := newPublicNativeSubscription(t)
	defer cleanupPublicNativeSubscription(t, subscription, testSubscription, native)

	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	nextDone := make(chan error, 1)
	go func() {
		_, err := subscription.Next(ctx)
		nextDone <- err
	}()

	select {
	case err := <-nextDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Next() wait admission error = %v, want context deadline identity", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Next() waited for cleanup behind the held lifecycle gate")
	}
	if subscription.isClosed() {
		t.Fatal("context admission error closed the subscription")
	}
	releaseLifecycle()

	if err := testSubscription.emitCounts(0, []uint64{1}); err != nil {
		t.Fatalf("emit notification after canceled Next: %v", err)
	}
	counts, err := subscription.Next(context.Background())
	if err != nil {
		t.Fatalf("Next() after canceled wait: %v", err)
	}
	if counts["event_a"] != 1 {
		t.Fatalf("Next() after canceled wait = %#v, want event_a=1", counts)
	}
}

func TestPublicNextTakeAdmissionErrorPreservesSubscription(t *testing.T) {
	testSubscription := newTestNativeSubscription(t, "event_a")
	native := &nativeSubscription{pointer: testSubscription.pointer}
	backend := &contextTakeBackend{native: native}
	subscription := newSubscription(Config{}, []string{"event_a"}, backend)
	defer cleanupPublicNativeSubscription(t, subscription, testSubscription, native)

	releaseLifecycle := nativegate.Global.EnterExclusive()
	defer releaseLifecycle()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := subscription.Next(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Next() take admission error = %v, want context deadline identity", err)
	}
	if subscription.isClosed() {
		t.Fatal("context take admission error closed the subscription")
	}
	releaseLifecycle()

	counts, err := subscription.Next(context.Background())
	if err != nil {
		t.Fatalf("Next() after canceled take: %v", err)
	}
	if counts["event_a"] != 1 {
		t.Fatalf("Next() after canceled take = %#v, want event_a=1", counts)
	}
}

func newPublicNativeSubscription(t *testing.T) (*Subscription,
	*nativeTestSubscription, *nativeSubscription) {
	t.Helper()
	testSubscription := newTestNativeSubscription(t, "event_a")
	native := &nativeSubscription{pointer: testSubscription.pointer}
	if err := testSubscription.emitCounts(0, []uint64{0}); err != nil {
		destroyTestNativeSubscription(t, testSubscription)
		t.Fatalf("establish native event baseline: %v", err)
	}
	return newSubscription(Config{}, []string{"event_a"}, native), testSubscription, native
}

func cleanupPublicNativeSubscription(t *testing.T, subscription *Subscription,
	testSubscription *nativeTestSubscription, native *nativeSubscription) {
	t.Helper()
	if subscription != nil {
		if err := subscription.Close(); err != nil {
			t.Errorf("close public native test subscription: %v", err)
		}
	}
	if native != nil && native.pointer == nil && testSubscription != nil {
		/* Close already destroyed the aliased native pointer. */
		testSubscription.pointer = nil
	}
	destroyTestNativeSubscription(t, testSubscription)
}

type contextTakeBackend struct {
	native *nativeSubscription
}

func (b *contextTakeBackend) wait(context.Context, time.Duration) (bool, error) {
	return true, nil
}

func (b *contextTakeBackend) ready(context.Context, time.Duration) (bool, error) {
	return true, nil
}

func (b *contextTakeBackend) take(ctx context.Context, count int) ([]uint64, bool, error) {
	counts, hasCounts, err := b.native.take(ctx, count)
	if err != nil {
		return nil, false, err
	}
	if !hasCounts {
		return []uint64{1}, true, nil
	}
	return counts, true, nil
}

func (b *contextTakeBackend) stop() error    { return nil }
func (b *contextTakeBackend) destroy() error { return nil }
