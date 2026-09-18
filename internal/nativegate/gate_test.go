package nativegate

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"
)

func waitForExclusiveWaiter(t *testing.T, gate *Gate) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		waiting := gate.exclusiveWaiters
		gate.mu.Unlock()
		if waiting != 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("exclusive entry did not reach its wait state")
}

func waitForOrdinaryWaiter(t *testing.T, gate *Gate) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		gate.mu.Lock()
		waiting := gate.ordinaryWaiters
		gate.mu.Unlock()
		if waiting != 0 {
			return
		}
		runtime.Gosched()
	}
	t.Fatal("ordinary entry did not reach its wait state")
}

func TestGateEnterContextStopsWaitingWhenContextIsCanceled(t *testing.T) {
	gate := new(Gate)
	releaseExclusive := gate.EnterExclusive()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		release, err := gate.EnterContext(ctx)
		if release != nil {
			release()
		}
		result <- err
	}()
	waitForOrdinaryWaiter(t, gate)

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnterContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnterContext() did not stop waiting after cancellation")
	}

	if state := gate.State(); state.ActiveCalls != 0 || state.ExclusiveWaiters != 0 {
		t.Fatalf("gate state after canceled ordinary entry = %+v", state)
	}
	releaseExclusive()
}

func TestGateEnterExclusiveContextStopsWaitingWhenContextIsCanceled(t *testing.T) {
	gate := new(Gate)
	releaseCall := gate.Enter()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		release, err := gate.EnterExclusiveContext(ctx)
		if release != nil {
			release()
		}
		result <- err
	}()
	waitForExclusiveWaiter(t, gate)

	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("EnterExclusiveContext() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("EnterExclusiveContext() did not stop waiting after cancellation")
	}

	releaseCall()
	if state := gate.State(); state.ActiveCalls != 0 || state.Exclusive || state.ExclusiveWaiters != 0 {
		t.Fatalf("gate state after canceled exclusive entry = %+v", state)
	}
}

func TestGateEnterContextAllowsReadersWhileExclusiveWaits(t *testing.T) {
	gate := new(Gate)
	firstRelease := gate.Enter()

	exclusiveAcquired := make(chan func(), 1)
	go func() {
		release, err := gate.EnterExclusiveContext(context.Background())
		if err != nil {
			t.Errorf("EnterExclusiveContext() error = %v", err)
			return
		}
		exclusiveAcquired <- release
	}()
	waitForExclusiveWaiter(t, gate)

	readerAcquired := make(chan func(), 1)
	go func() {
		release, err := gate.EnterContext(context.Background())
		if err != nil {
			t.Errorf("EnterContext() error = %v", err)
			return
		}
		readerAcquired <- release
	}()
	select {
	case release := <-readerAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("reader was blocked behind a waiting exclusive caller")
	}

	select {
	case <-exclusiveAcquired:
		t.Fatal("exclusive caller acquired before the existing reader drained")
	default:
	}
	firstRelease()
	select {
	case release := <-exclusiveAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("exclusive caller did not acquire after the existing reader drained")
	}
}

func TestGateAllowsNewCallsWhileLifecycleWaitsForQuiescence(t *testing.T) {
	gate := new(Gate)
	firstRelease := gate.Enter()

	exclusiveAcquired := make(chan func(), 1)
	go func() {
		exclusiveAcquired <- gate.EnterExclusive()
	}()
	waitForExclusiveWaiter(t, gate)

	readerAcquired := make(chan func(), 1)
	go func() {
		readerAcquired <- gate.Enter()
	}()
	select {
	case release := <-readerAcquired:
		release()
	case <-time.After(time.Second):
		t.Fatal("new native call was blocked behind a waiting lifecycle call")
	}

	select {
	case <-exclusiveAcquired:
		t.Fatal("lifecycle call acquired before the existing native call drained")
	default:
	}
	firstRelease()

	var exclusiveRelease func()
	select {
	case exclusiveRelease = <-exclusiveAcquired:
	case <-time.After(time.Second):
		t.Fatal("lifecycle call did not acquire after native calls drained")
	}

	blockedReader := make(chan func(), 1)
	go func() {
		blockedReader <- gate.Enter()
	}()
	select {
	case <-blockedReader:
		t.Fatal("native call entered while lifecycle call was active")
	case <-time.After(20 * time.Millisecond):
	}
	exclusiveRelease()
	select {
	case release := <-blockedReader:
		release()
	case <-time.After(time.Second):
		t.Fatal("native call did not resume after lifecycle call completed")
	}
}

func TestGateAllowsLockReleaseCallWhileLifecycleWaits(t *testing.T) {
	gate := new(Gate)
	lockedCallEntered := make(chan struct{})
	lockRelease := make(chan struct{})
	lockedCallDone := make(chan struct{})
	go func() {
		release := gate.Enter()
		close(lockedCallEntered)
		<-lockRelease
		release()
		close(lockedCallDone)
	}()
	<-lockedCallEntered

	exclusiveAcquired := make(chan func(), 1)
	go func() {
		exclusiveAcquired <- gate.EnterExclusive()
	}()
	waitForExclusiveWaiter(t, gate)

	releaseCallDone := make(chan struct{})
	go func() {
		release := gate.Enter()
		close(lockRelease)
		release()
		close(releaseCallDone)
	}()
	select {
	case <-releaseCallDone:
	case <-time.After(time.Second):
		t.Fatal("lock-release native call was blocked behind a waiting lifecycle call")
	}
	select {
	case <-lockedCallDone:
	case <-time.After(time.Second):
		t.Fatal("blocked native call did not finish after lock-release call")
	}

	var exclusiveRelease func()
	select {
	case exclusiveRelease = <-exclusiveAcquired:
	case <-time.After(time.Second):
		t.Fatal("lifecycle call did not acquire after the blocked native call drained")
	}
	exclusiveRelease()
}

func TestGateStateReportsAdmission(t *testing.T) {
	gate := new(Gate)
	if state := gate.State(); state.ActiveCalls != 0 || state.Exclusive || state.ExclusiveWaiters != 0 {
		t.Fatalf("initial gate state = %+v, want empty", state)
	}

	release := gate.Enter()
	defer release()
	state := gate.State()
	if state.ActiveCalls != 1 || state.Exclusive || state.ExclusiveWaiters != 0 {
		t.Fatalf("active gate state = %+v, want one ordinary call", state)
	}
}
