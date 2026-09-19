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
	cancelAttempted   bool
	cancelOverlapped  bool
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
	return nativeCancelResult{
		nativeCode: nativeCode,
		attempted:  s.cancelAttempted,
		overlapped: s.cancelOverlapped,
	}, cancelErr
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

func TestClassifyNativeWriteOutcomeRequiresKnownCleanup(t *testing.T) {
	native := &NativeError{
		Operation:  "execute statement",
		NativeCode: nativeCancelledCode,
		Message:    "isc_cancelled",
	}

	tests := []struct {
		name          string
		state         nativeWriteOutcomeState
		wantUncertain bool
	}{
		{
			name:          "confirmed implicit rollback",
			state:         nativeWriteOutcomeRollbackConfirmed,
			wantUncertain: false,
		},
		{
			name:          "usable explicit transaction",
			state:         nativeWriteOutcomeExplicitUsable,
			wantUncertain: false,
		},
		{
			name:          "unknown cleanup state",
			state:         nativeWriteOutcomeUnknown,
			wantUncertain: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyNativeWriteOutcome("execute statement", true,
				context.Canceled, native, nil, test.state)
			if !errors.Is(got, context.Canceled) {
				t.Fatalf("classification = %v, want context.Canceled", got)
			}
			var uncertain *UncertainOutcomeError
			if errors.As(got, &uncertain) != test.wantUncertain {
				t.Fatalf("uncertain outcome = %v, want %v", uncertain != nil, test.wantUncertain)
			}
			if errors.Is(got, driver.ErrBadConn) {
				t.Fatal("cancellation outcome must not be driver.ErrBadConn")
			}
		})
	}
}

func TestNativeWriteOutcomeStateParsingIsConservative(t *testing.T) {
	for _, test := range []struct {
		name  string
		value int
		want  nativeWriteOutcomeState
	}{
		{name: "unknown", value: 0, want: nativeWriteOutcomeUnknown},
		{name: "rollback confirmed", value: 1, want: nativeWriteOutcomeRollbackConfirmed},
		{name: "explicit usable", value: 2, want: nativeWriteOutcomeExplicitUsable},
		{name: "unrecognized", value: 99, want: nativeWriteOutcomeUnknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := nativeWriteOutcomeStateFromValue(test.value); got != test.want {
				t.Fatalf("nativeWriteOutcomeStateFromValue(%d) = %d, want %d",
					test.value, got, test.want)
			}
		})
	}
}

func TestNativeExecutionErrorPreservesPrimaryAndCleanupDiagnostics(t *testing.T) {
	primary := &NativeError{Operation: "execute statement", NativeCode: nativeCancelledCode}
	cleanup := &NativeError{Operation: "rollback transaction", NativeCode: 335545000}
	original := &nativeExecutionError{primary: primary, cleanup: cleanup}

	gotPrimary, gotCleanup := splitNativeExecutionError(original)
	if !errors.Is(gotPrimary, primary) || !errors.Is(gotCleanup, cleanup) {
		t.Fatalf("splitNativeExecutionError() = (%v, %v), want primary and cleanup diagnostics",
			gotPrimary, gotCleanup)
	}
	if !errors.Is(original, primary) || !errors.Is(original, cleanup) {
		t.Fatal("native execution error did not retain both diagnostics in its error tree")
	}
}

func TestClassifyNativeWriteOutcomeUsesOverlappingCancellationForLostResponse(t *testing.T) {
	const primarySecret = "transport /private/db.ib password=primary"
	const cleanupSecret = "rollback /private/db.ib password=cleanup"
	primary := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544366,
		Message:    primarySecret,
	}
	cleanup := &NativeError{
		Operation:  "rollback transaction",
		NativeCode: 335545000,
		Message:    cleanupSecret,
	}

	got := classifyNativeWriteOutcome("execute statement", true,
		context.Canceled, primary, cleanup, nativeWriteOutcomeUnknown,
		nativeCancellationEvidence{attempted: true, overlapped: true})
	if !errors.Is(got, context.Canceled) {
		t.Fatalf("classification = %v, want context.Canceled", got)
	}
	var uncertain *UncertainOutcomeError
	if !errors.As(got, &uncertain) {
		t.Fatalf("classification = %v, want UncertainOutcomeError", got)
	}
	var cancellation *CancellationError
	if !errors.As(got, &cancellation) {
		t.Fatalf("classification = %v, want CancellationError cause", got)
	}
	if !errors.Is(got, primary) || !errors.Is(got, cleanup) {
		t.Fatalf("classification lost primary or cleanup diagnostics: %v", got)
	}
	if errors.Is(got, driver.ErrBadConn) {
		t.Fatal("uncertain cancellation must not match driver.ErrBadConn")
	}
	confirmed := classifyNativeWriteOutcome("execute statement", true,
		context.Canceled, primary, nil, nativeWriteOutcomeRollbackConfirmed,
		nativeCancellationEvidence{attempted: true, overlapped: true})
	var confirmedUncertain *UncertainOutcomeError
	if errors.As(confirmed, &confirmedUncertain) {
		t.Fatalf("confirmed rollback outcome became uncertain: %v", confirmed)
	}
	var confirmedCancellation *CancellationError
	if !errors.As(confirmed, &confirmedCancellation) ||
		!errors.Is(confirmed, context.Canceled) {
		t.Fatalf("confirmed rollback outcome = %v, want CancellationError matching context", confirmed)
	}
}

func TestClassifyNativeWriteOutcomeDoesNotInferCancellationFromExpiredContext(t *testing.T) {
	primary := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544366,
		Message:    "ordinary native failure",
	}

	got := classifyNativeWriteOutcome("execute statement", true,
		context.DeadlineExceeded, primary, nil, nativeWriteOutcomeUnknown,
		nativeCancellationEvidence{})
	if !errors.Is(got, primary) {
		t.Fatalf("classification = %v, lost ordinary native error", got)
	}
	if errors.Is(got, context.DeadlineExceeded) {
		t.Fatalf("ordinary native failure was relabeled as deadline cancellation: %v", got)
	}
	var uncertain *UncertainOutcomeError
	if errors.As(got, &uncertain) {
		t.Fatalf("ordinary native failure became uncertain without cancellation evidence: %v", got)
	}
}

func TestExecContextOverlappingLostResponsePreservesFaultTreeAndRedacts(t *testing.T) {
	const secret = "password=primary-secret attachment=/private/primary.ib"
	const cleanupSecret = "password=cleanup-secret attachment=/private/cleanup.ib"
	const requestSecret = "password=request-secret attachment=/private/request.ib"
	primary := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544366,
		Message:    secret,
	}
	cleanup := &NativeError{
		Operation:  "rollback transaction",
		NativeCode: 335545000,
		Message:    cleanupSecret,
	}
	requestErr := &NativeError{
		Operation:  "cancel statement",
		NativeCode: 335545001,
		Message:    requestSecret,
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted:    make(chan struct{}),
		cancelErr:        requestErr,
		cancelAttempted:  true,
		cancelOverlapped: true,
	}
	native := &nativeConnection{
		brokenOverride:     func() bool { return false },
		cancelSlotOverride: slot,
		writeOutcomeStateOverride: func() nativeWriteOutcomeState {
			return nativeWriteOutcomeUnknown
		},
		execContextOverride: func(_ string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
			close(entered)
			<-release
			return 0, &nativeExecutionError{primary: primary, cleanup: cleanup}
		},
	}
	connection := &conn{
		native:           native,
		redactionSecrets: []string{"primary-secret", "cleanup-secret", "request-secret", "/private/primary.ib", "/private/cleanup.ib", "/private/request.ib"},
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
		result <- err
	}()
	waitForTestSignal(t, entered, "execution did not enter native override")
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "cancellation request did not overlap execution")
	close(release)

	err := <-result
	var uncertain *UncertainOutcomeError
	if !errors.As(err, &uncertain) {
		t.Fatalf("ExecContext() error = %v, want UncertainOutcomeError", err)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, primary) ||
		!errors.Is(err, cleanup) || !errors.Is(err, requestErr) {
		t.Fatalf("ExecContext() error lost context/fault diagnostics: %v", err)
	}
	if !errors.Is(uncertain.Cleanup, cleanup) {
		t.Fatalf("UncertainOutcomeError cleanup = %v, lost rollback diagnostic", uncertain.Cleanup)
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("ExecContext() error = %v, must not match driver.ErrBadConn", err)
	}
	for _, secretValue := range []string{secret, cleanupSecret, requestSecret} {
		if strings.Contains(err.Error(), secretValue) {
			t.Fatalf("ExecContext() rendered unsanitized secret %q: %v", secretValue, err)
		}
	}
}

func TestExecContextCancellationRequestFailureWithoutOverlapStaysNative(t *testing.T) {
	primary := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544366,
		Message:    "ordinary native failure",
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted: make(chan struct{}),
		cancelErr: &NativeError{
			Operation:  "cancel statement",
			NativeCode: nativeCancelledCode,
			Message:    "cancel request completed after execution ended",
		},
	}
	native := &nativeConnection{
		brokenOverride:     func() bool { return false },
		cancelSlotOverride: slot,
		writeOutcomeStateOverride: func() nativeWriteOutcomeState {
			return nativeWriteOutcomeUnknown
		},
		execContextOverride: func(_ string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
			close(entered)
			<-release
			return 0, primary
		},
	}
	connection := &conn{native: native}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := connection.ExecContext(ctx, "UPDATE example SET value = 1", nil)
		result <- err
	}()
	waitForTestSignal(t, entered, "execution did not enter native override")
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "cancellation request did not run")
	close(release)

	err := <-result
	if !errors.Is(err, primary) {
		t.Fatalf("ExecContext() error = %v, lost ordinary native error", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("ordinary native error was relabeled as context cancellation: %v", err)
	}
	var uncertain *UncertainOutcomeError
	if errors.As(err, &uncertain) {
		t.Fatalf("ordinary native error became uncertain without overlap evidence: %v", err)
	}
}

func TestExecContextSlotAllocationFailureDoesNotEnterExecution(t *testing.T) {
	allocationErr := errors.New("injected cancellation slot allocation failure")
	originalFactory := nativeCancelSlotFactory
	nativeCancelSlotFactory = func() (nativeCancelSlotBackend, error) {
		return nil, allocationErr
	}
	defer func() { nativeCancelSlotFactory = originalFactory }()

	called := false
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		execContextOverride: func(_ string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
			called = true
			return 1, nil
		},
	}
	connection := &conn{native: native}
	got, err := connection.ExecContext(context.Background(),
		"UPDATE example SET value = 1", nil)
	if got != nil {
		t.Fatalf("ExecContext() result = %v, want nil", got)
	}
	if !errors.Is(err, allocationErr) {
		t.Fatalf("ExecContext() error = %v, want allocation error", err)
	}
	if called {
		t.Fatal("execution entered native override after cancellation slot allocation failed")
	}
}

func TestExecContextSuccessfulCompletionWinsCancellationRaceExactlyOnce(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted:    make(chan struct{}),
		cancelAttempted:  true,
		cancelOverlapped: true,
	}
	attempts := 0
	native := &nativeConnection{
		brokenOverride:     func() bool { return false },
		cancelSlotOverride: slot,
		execContextOverride: func(_ string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
			attempts++
			close(entered)
			<-release
			return 1, nil
		},
	}
	connection := &conn{native: native}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan struct {
		affected driver.Result
		err      error
	}, 1)
	go func() {
		affected, err := connection.ExecContext(ctx, "UPDATE example SET value = value + 1", nil)
		result <- struct {
			affected driver.Result
			err      error
		}{affected: affected, err: err}
	}()
	waitForTestSignal(t, entered, "execution did not enter native override")
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "cancellation request did not overlap execution")
	close(release)

	completed := <-result
	if completed.err != nil {
		t.Fatalf("successful completion returned error after cancellation race: %v", completed.err)
	}
	if completed.affected == nil {
		t.Fatal("successful completion returned nil result")
	}
	rowsAffected, err := completed.affected.RowsAffected()
	if err != nil || rowsAffected != 1 {
		t.Fatalf("successful completion rows affected = %d, error = %v; want 1, nil",
			rowsAffected, err)
	}
	if attempts != 1 {
		t.Fatalf("non-idempotent execution attempts = %d, want exactly one", attempts)
	}
}

func TestSQLDoesNotReplayUncertainLostResponseAfterConnectionLoss(t *testing.T) {
	const primarySecret = "password=lost-response attachment=/private/lost.ib"
	primary := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544366,
		Message:    primarySecret,
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted:    make(chan struct{}),
		cancelAttempted:  true,
		cancelOverlapped: true,
	}
	attempts := 0
	native := &nativeConnection{
		brokenOverride:     func() bool { return true },
		cancelSlotOverride: slot,
		execContextOverride: func(_ string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
			attempts++
			close(entered)
			<-release
			return 0, primary
		},
	}
	connection := &conn{
		native:           native,
		redactionSecrets: []string{"lost-response", "/private/lost.ib"},
	}
	db := openDatabaseSQLTestDB(t, connection)
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, "UPDATE example SET value = value + 1")
		result <- err
	}()
	waitForTestSignal(t, entered, "execution did not enter native override")
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "cancellation request did not overlap execution")
	close(release)

	err := <-result
	var uncertain *UncertainOutcomeError
	if !errors.As(err, &uncertain) {
		t.Fatalf("database/sql error = %v, want UncertainOutcomeError", err)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, primary) {
		t.Fatalf("database/sql error lost cancellation/native identity: %v", err)
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("database/sql error = %v, must not match driver.ErrBadConn", err)
	}
	if attempts != 1 {
		t.Fatalf("uncertain lost-response execution attempts = %d, want exactly one", attempts)
	}
	if strings.Contains(err.Error(), primarySecret) {
		t.Fatalf("database/sql error rendered unsanitized native secret: %v", err)
	}
}

func TestExecContextConnectionLossLeavesOutcomeUnknown(t *testing.T) {
	primary := &NativeError{
		Operation:  "execute statement",
		NativeCode: 335544366,
		Message:    "connection lost after cancellation overlap",
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	slot := &fakeCancelSlot{
		cancelStarted:    make(chan struct{}),
		cancelAttempted:  true,
		cancelOverlapped: true,
	}
	native := &nativeConnection{
		brokenOverride:     func() bool { return true },
		cancelSlotOverride: slot,
		execContextOverride: func(_ string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
			close(entered)
			<-release
			return 0, primary
		},
	}
	connection := &conn{native: native}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := connection.ExecContext(ctx, "UPDATE example SET value = value + 1", nil)
		result <- err
	}()
	waitForTestSignal(t, entered, "execution did not enter native override")
	cancel()
	waitForTestSignal(t, slot.cancelStarted, "cancellation request did not overlap execution")
	close(release)

	err := <-result
	var uncertain *UncertainOutcomeError
	if !errors.As(err, &uncertain) {
		t.Fatalf("ExecContext() error = %v, want UncertainOutcomeError", err)
	}
	if !errors.Is(err, context.Canceled) || !errors.Is(err, primary) {
		t.Fatalf("ExecContext() error lost cancellation/native identity: %v", err)
	}
	if connection.native != nil {
		t.Fatal("connection-loss path retained a broken native connection")
	}
}
