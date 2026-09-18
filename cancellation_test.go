package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

type fakeCancelSlot struct {
	mu                sync.Mutex
	beginCalls        int
	generation        uint64
	cancelCalls       int
	cancelGenerations []uint64
	cancelStarted     chan struct{}
	cancelStartedOnce sync.Once
	publication       <-chan struct{}
	publicationPassed chan struct{}
	publicationOnce   sync.Once
	cancelRelease     <-chan struct{}
	cancelErr         error
	nativeCode        int64
	closeCalls        int
	closeBeforeWatch  bool
	watcherDone       <-chan struct{}
}

func (s *fakeCancelSlot) begin() (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beginCalls++
	s.generation++
	return s.generation, nil
}

func (s *fakeCancelSlot) cancel(generation uint64) (nativeCancelResult, error) {
	s.mu.Lock()
	s.cancelCalls++
	s.cancelGenerations = append(s.cancelGenerations, generation)
	started := s.cancelStarted
	release := s.cancelRelease
	cancelErr := s.cancelErr
	nativeCode := s.nativeCode
	s.mu.Unlock()
	if started != nil {
		s.cancelStartedOnce.Do(func() { close(started) })
	}
	if s.publication != nil {
		<-s.publication
		if s.publicationPassed != nil {
			s.publicationOnce.Do(func() { close(s.publicationPassed) })
		}
	}
	if release != nil {
		<-release
	}
	return nativeCancelResult{nativeCode: nativeCode}, cancelErr
}

func (s *fakeCancelSlot) close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closeCalls++
	if s.watcherDone != nil {
		select {
		case <-s.watcherDone:
		default:
			s.closeBeforeWatch = true
		}
	}
}

func (s *fakeCancelSlot) snapshot() (begin, cancel, close int, generations []uint64, beforeWatch bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.beginCalls, s.cancelCalls, s.closeCalls,
		append([]uint64(nil), s.cancelGenerations...), s.closeBeforeWatch
}

func waitForTestSignal(t *testing.T, signal <-chan struct{}, message string) {
	t.Helper()
	select {
	case <-signal:
	case <-time.After(time.Second):
		t.Fatal(message)
	}
}

func TestNativeCancelOperationRejectsPreCanceledContextBeforeBegin(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	slot := &fakeCancelSlot{}
	op := &nativeCancelOperation{slot: slot}

	if err := op.begin(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("begin() error = %v, want context.Canceled", err)
	}
	beginCalls, cancelCalls, closeCalls, _, _ := slot.snapshot()
	if beginCalls != 0 || cancelCalls != 0 || closeCalls != 0 {
		t.Fatalf("pre-canceled begin touched slot: begin=%d cancel=%d close=%d",
			beginCalls, cancelCalls, closeCalls)
	}
}

func TestNativeCancelOperationWaitsForPublicationAndCancellationBeforeClosingSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	publication := make(chan struct{})
	publicationPassed := make(chan struct{})
	cancelRelease := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted:     make(chan struct{}),
		publication:       publication,
		publicationPassed: publicationPassed,
		cancelRelease:     cancelRelease,
	}
	finishWaitStarted := make(chan struct{})
	op := &nativeCancelOperation{slot: slot, finishWaitStarted: finishWaitStarted}
	if err := op.begin(ctx); err != nil {
		t.Fatalf("begin() error = %v", err)
	}
	slot.mu.Lock()
	slot.watcherDone = op.watcherDone
	slot.mu.Unlock()

	cancel()
	waitForTestSignal(t, slot.cancelStarted, "watcher did not wait for publication")
	finished := make(chan struct{})
	go func() {
		op.finish()
		close(finished)
	}()
	waitForTestSignal(t, finishWaitStarted, "finish did not enter watcher wait")
	select {
	case <-finished:
		t.Fatal("finish returned before cancellation request completed")
	default:
	}

	close(publication)
	waitForTestSignal(t, publicationPassed, "cancellation did not observe publication")
	select {
	case <-finished:
		t.Fatal("finish returned before the published cancellation request completed")
	default:
	}
	close(cancelRelease)
	waitForTestSignal(t, finished, "finish did not join the cancellation watcher")
	_, cancelCalls, closeCalls, _, beforeWatch := slot.snapshot()
	if cancelCalls != 1 || closeCalls != 1 {
		t.Fatalf("slot lifecycle = cancel %d, close %d; want one each", cancelCalls, closeCalls)
	}
	if beforeWatch {
		t.Fatal("slot closed before watcher joined")
	}
}

func TestNativeCancelOperationOverlappingFinishAndCloseWaitsForWatcher(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelRelease := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted: make(chan struct{}),
		cancelRelease: cancelRelease,
	}
	finishWaitStarted := make(chan struct{})
	closeWaitStarted := make(chan struct{})
	op := &nativeCancelOperation{
		slot:              slot,
		finishWaitStarted: finishWaitStarted,
		closeWaitStarted:  closeWaitStarted,
	}
	if err := op.begin(ctx); err != nil {
		t.Fatalf("begin() error = %v", err)
	}
	slot.mu.Lock()
	slot.watcherDone = op.watcherDone
	slot.mu.Unlock()

	cancel()
	waitForTestSignal(t, slot.cancelStarted, "watcher did not enter cancellation request")
	finishDone := make(chan struct{})
	go func() {
		op.finish()
		close(finishDone)
	}()
	waitForTestSignal(t, finishWaitStarted, "finish did not enter watcher wait")

	closeAttempted := make(chan struct{})
	closeDone := make(chan struct{})
	go func() {
		close(closeAttempted)
		op.close()
		close(closeDone)
	}()
	waitForTestSignal(t, closeAttempted, "close did not overlap finish")
	waitForTestSignal(t, closeWaitStarted, "close did not enter watcher wait")
	select {
	case <-closeDone:
		t.Fatal("close destroyed the slot before watcher completion")
	default:
	}

	close(cancelRelease)
	waitForTestSignal(t, finishDone, "finish did not complete after cancellation request")
	waitForTestSignal(t, closeDone, "close did not complete after watcher join")
	_, _, closeCalls, _, beforeWatch := slot.snapshot()
	if closeCalls != 1 || beforeWatch {
		t.Fatalf("slot close lifecycle = calls %d beforeWatch=%v", closeCalls, beforeWatch)
	}
	if err := classifyNativeOutcome("execute statement", true, ctx.Err(), nil, nil); err != nil {
		t.Fatalf("successful completion was replaced by cancellation: %v", err)
	}
}

func TestNativeCancelOperationCompletionWinsWithoutCancellationRequest(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	slot := &fakeCancelSlot{}
	op := &nativeCancelOperation{slot: slot}
	if err := op.begin(ctx); err != nil {
		t.Fatalf("begin() error = %v", err)
	}
	slot.mu.Lock()
	slot.watcherDone = op.watcherDone
	slot.mu.Unlock()

	op.finish()
	cancel()
	_, cancelCalls, closeCalls, _, beforeWatch := slot.snapshot()
	if cancelCalls != 0 {
		t.Fatalf("completion raced cancellation with %d cancellation calls", cancelCalls)
	}
	if closeCalls != 1 || beforeWatch {
		t.Fatalf("slot lifecycle = close %d beforeWatch=%v", closeCalls, beforeWatch)
	}
}

func TestNativeCancelOperationUsesOrdinaryNativeGateAdmission(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	slot := &fakeCancelSlot{cancelStarted: make(chan struct{})}
	op := &nativeCancelOperation{slot: slot, watchStarted: make(chan struct{})}
	releaseExclusive := nativegate.Global.EnterExclusive()
	if err := op.begin(ctx); err != nil {
		releaseExclusive()
		t.Fatalf("begin() error = %v", err)
	}
	waitForTestSignal(t, op.watchStarted, "watcher did not start")
	cancel()
	select {
	case <-slot.cancelStarted:
		t.Fatal("cancellation entered the native gate while it was exclusive")
	default:
	}
	releaseExclusive()
	waitForTestSignal(t, slot.cancelStarted, "cancellation did not enter ordinary native gate")
	op.finish()
}

func TestNativeCancelOperationReportsCancellationRequestFailureWithoutChangingResult(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	requestErr := errors.New("cancel request failed")
	slot := &fakeCancelSlot{
		cancelStarted: make(chan struct{}),
		cancelRelease: release,
		cancelErr:     requestErr,
	}
	op := &nativeCancelOperation{slot: slot}
	if err := op.begin(ctx); err != nil {
		t.Fatalf("begin() error = %v", err)
	}
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "watcher did not request cancellation")
	close(release)
	op.finish()
	if !errors.Is(op.requestError(), requestErr) {
		t.Fatalf("requestError() = %v, want %v", op.requestError(), requestErr)
	}
	if err := classifyNativeOutcome("execute statement", true, ctx.Err(), nil, nil); err != nil {
		t.Fatalf("successful operation was replaced by cancellation request failure: %v", err)
	}
}

// Task 1's native harness proves that a stale generation cannot cancel the
// next operation. This Go test only proves that the first watcher is joined
// before a backend can be reused; its fake backend forwards generations and
// cannot prove the native slot's rejection rule.
func TestNativeCancelOperationJoinsWatcherBeforeSlotReuse(t *testing.T) {
	firstContext, cancelFirst := context.WithCancel(context.Background())
	slot := &fakeCancelSlot{cancelStarted: make(chan struct{})}
	first := &nativeCancelOperation{slot: slot}
	if err := first.begin(firstContext); err != nil {
		t.Fatalf("begin() error = %v", err)
	}
	first.finish()

	secondContext, cancelSecond := context.WithCancel(context.Background())
	second := &nativeCancelOperation{slot: slot}
	if err := second.begin(secondContext); err != nil {
		t.Fatalf("second begin() error = %v", err)
	}
	cancelFirst()
	select {
	case <-slot.cancelStarted:
		t.Fatal("delayed first watcher survived slot reuse")
	default:
	}
	cancelSecond()
	waitForTestSignal(t, slot.cancelStarted, "second watcher did not request cancellation")
	second.finish()

	beginCalls, cancelCalls, closeCalls, generations, beforeWatch := slot.snapshot()
	if beginCalls != 2 || cancelCalls != 1 || closeCalls != 2 || beforeWatch {
		t.Fatalf("operation lifecycle = begin %d cancel %d close %d beforeWatch=%v",
			beginCalls, cancelCalls, closeCalls, beforeWatch)
	}
	if len(generations) != 1 || generations[0] != 2 {
		t.Fatalf("cancellation generations = %v, want [2]", generations)
	}
}

type cleanupDiagnostic struct {
	message string
}

func (e *cleanupDiagnostic) Error() string { return e.message }

func TestClassifyReadOnlyCancellationRedactsCleanupMessageAndPreservesIdentity(t *testing.T) {
	const secret = "UPDATE accounts SET password='secret' attachment=/private/db.ib"
	native := &NativeError{
		Operation:  "fetch rows",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}
	cleanup := &cleanupDiagnostic{message: secret}

	err := classifyNativeOutcome("fetch rows", false, context.DeadlineExceeded, native, cleanup)
	if err == nil {
		t.Fatal("read-only cancellation with cleanup returned nil")
	}
	var cancellationErr *CancellationError
	if !errors.As(err, &cancellationErr) {
		t.Fatal("read-only cleanup lost CancellationError identity")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("read-only cleanup lost context identity")
	}
	var nativeErr *NativeError
	if !errors.As(err, &nativeErr) || !errors.Is(err, native) {
		t.Fatal("read-only cleanup lost native cancellation identity")
	}
	var cleanupErr *cleanupDiagnostic
	if !errors.As(err, &cleanupErr) || !errors.Is(err, cleanup) {
		t.Fatal("read-only cleanup lost cleanup identity")
	}
	var uncertainErr *UncertainOutcomeError
	if errors.As(err, &uncertainErr) {
		t.Fatal("read-only cleanup was classified as uncertain write")
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("read-only cancellation rendered cleanup secret: %v", err)
	}
}

func TestClassifyNativeOutcomeKeepsExecutingResultAuthoritative(t *testing.T) {
	deadline := context.DeadlineExceeded
	canceledNative := &NativeError{
		Operation:  "execute statement",
		NativeCode: nativeCancelledCode,
		Message:    "native cancellation",
	}
	otherNative := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544999,
		Message:    "different native failure",
	}
	cleanup := &NativeError{
		Operation:  "rollback transaction",
		NativeCode: 335545000,
		Message:    "rollback failure",
	}

	tests := []struct {
		name          string
		mutating      bool
		contextErr    error
		nativeErr     error
		cleanupErr    error
		wantCancel    bool
		wantUncertain bool
		wantNative    error
	}{
		{
			name:       "successful operation wins context race",
			mutating:   true,
			contextErr: deadline,
			wantNative: nil,
		},
		{
			name:       "isc cancelled becomes cancellation error",
			mutating:   false,
			contextErr: deadline,
			nativeErr:  fmt.Errorf("native wrapper: %w", canceledNative),
			wantCancel: true,
			wantNative: canceledNative,
		},
		{
			name:       "different native error remains authoritative",
			mutating:   true,
			contextErr: deadline,
			nativeErr:  otherNative,
			wantNative: otherNative,
		},
		{
			name:          "canceled mutating operation with cleanup failure is uncertain",
			mutating:      true,
			contextErr:    deadline,
			nativeErr:     canceledNative,
			cleanupErr:    cleanup,
			wantCancel:    true,
			wantUncertain: true,
			wantNative:    canceledNative,
		},
		{
			name:       "native cancellation without context stays native",
			mutating:   false,
			nativeErr:  canceledNative,
			wantNative: canceledNative,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyNativeOutcome("execute statement", test.mutating,
				test.contextErr, test.nativeErr, test.cleanupErr)
			if test.wantNative == nil {
				if got != nil {
					t.Fatalf("classification = %v, want success", got)
				}
				return
			}
			if got == nil {
				t.Fatal("classification returned success for native failure")
			}
			var nativeErr *NativeError
			if !errors.As(got, &nativeErr) || !errors.Is(got, test.wantNative) {
				t.Fatalf("classification lost native error: %v", got)
			}
			if errors.Is(got, driver.ErrBadConn) {
				t.Fatal("cancellation classification must not be driver.ErrBadConn")
			}
			var cancellationErr *CancellationError
			if errors.As(got, &cancellationErr) != test.wantCancel {
				t.Fatalf("CancellationError presence = %v, want %v", cancellationErr != nil, test.wantCancel)
			}
			var uncertainErr *UncertainOutcomeError
			if errors.As(got, &uncertainErr) != test.wantUncertain {
				t.Fatalf("UncertainOutcomeError presence = %v, want %v", uncertainErr != nil, test.wantUncertain)
			}
		})
	}
}
