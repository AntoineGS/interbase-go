package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
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

func TestNativeCancelOperationWaitsForCancellationRequestBeforeClosingSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancelRelease := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted: make(chan struct{}),
		cancelRelease: cancelRelease,
	}
	op := &nativeCancelOperation{slot: slot}
	if err := op.begin(ctx); err != nil {
		t.Fatalf("begin() error = %v", err)
	}
	slot.mu.Lock()
	slot.watcherDone = op.watcherDone
	slot.mu.Unlock()

	cancel()
	waitForTestSignal(t, slot.cancelStarted, "watcher did not request native cancellation")
	finished := make(chan struct{})
	go func() {
		op.finish()
		close(finished)
	}()
	select {
	case <-finished:
		t.Fatal("finish returned before cancellation request completed")
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

func TestNativeCancelOperationPassesOneGenerationAndJoinsDelayedWatcher(t *testing.T) {
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
		t.Fatal("delayed first watcher targeted the second generation")
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
