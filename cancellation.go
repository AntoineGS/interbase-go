package interbase

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"

	"interbase-go/internal/nativegate"
)

// NativeError is the public name for an error returned by the InterBase
// client. Error remains the historical name and is an alias-compatible type.
type NativeError = Error

// CancellationError reports a DSQL operation that the native client stopped
// after its context was canceled. The native operation result, rather than the
// cancellation request, determines whether this error is returned.
//
// Context and Native are retained in the error tree so callers can use
// errors.Is and errors.As without parsing a rendered error message.
type CancellationError struct {
	Operation string
	Mutating  bool
	Context   error
	Native    error
}

func (e *CancellationError) Error() string {
	if e == nil {
		return "interbase: operation canceled"
	}
	operation := safeOperationName(e.Operation, "operation")
	switch {
	case errors.Is(e.Context, context.DeadlineExceeded):
		return fmt.Sprintf("interbase: %s canceled (deadline exceeded)", operation)
	case errors.Is(e.Context, context.Canceled):
		return fmt.Sprintf("interbase: %s canceled", operation)
	default:
		return fmt.Sprintf("interbase: %s canceled", operation)
	}
}

// Unwrap retains both the originating context error and native diagnostics.
func (e *CancellationError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return joinErrorValues(e.Context, e.Native)
}

// UncertainOutcomeError reports a mutating operation whose final effect could
// not be established. Cause normally contains a CancellationError and Cleanup
// contains the failed rollback or other cleanup diagnostic.
type UncertainOutcomeError struct {
	Operation string
	Mutating  bool
	Cause     error
	Cleanup   error
}

// cancellationCleanupError preserves a non-mutating cancellation's cleanup
// diagnostics without rendering the cleanup error. It is intentionally not an
// UncertainOutcomeError: cleanup failure makes a canceled write uncertain, but
// does not establish an uncertain write outcome for a read-only operation.
type cancellationCleanupError struct {
	Operation string
	Cause     error
	Cleanup   error
}

func (e *cancellationCleanupError) Error() string {
	if e == nil {
		return "interbase: canceled operation cleanup failed"
	}
	return fmt.Sprintf("interbase: %s canceled; cleanup failed",
		safeOperationName(e.Operation, "operation"))
}

func (e *cancellationCleanupError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return joinErrorValues(e.Cause, e.Cleanup)
}

// nativeCancelOperation owns one cancellation slot generation and its single
// context watcher. The executing native call owns the slot while it is active;
// finish joins the watcher before close can destroy the slot.
type nativeCancelOperation struct {
	ctx          context.Context
	slot         nativeCancelSlotBackend
	generation   uint64
	done         chan struct{}
	watcherDone  chan struct{}
	watchStarted chan struct{}

	started  atomic.Bool
	finished atomic.Bool
	closed   atomic.Bool

	diagnosticsMu sync.Mutex
	contextErr    error
	requestErr    error
	nativeCode    int64
}

// beginNativeCancelOperation allocates a slot, starts its generation, and
// launches exactly one context watcher. A context already carrying an error is
// rejected before any slot is allocated or generation is begun.
func beginNativeCancelOperation(ctx context.Context) (*nativeCancelOperation, error) {
	op := new(nativeCancelOperation)
	if err := op.begin(ctx); err != nil {
		return nil, err
	}
	return op, nil
}

// newNativeCancelOperation is kept as the descriptive constructor used by
// native call sites that do not need to distinguish allocation from begin.
func newNativeCancelOperation(ctx context.Context) (*nativeCancelOperation, error) {
	return beginNativeCancelOperation(ctx)
}

// begin initializes the operation. Tests and native wrappers may provide a
// slot backend before calling begin; production callers leave slot nil and the
// C-owned backend is allocated here.
func (o *nativeCancelOperation) begin(ctx context.Context) error {
	if o == nil {
		return errors.New("interbase: nil native cancellation operation")
	}
	if err := contextError(ctx); err != nil {
		return err
	}
	if !o.started.CompareAndSwap(false, true) {
		return errors.New("interbase: native cancellation operation already begun")
	}

	slot := o.slot
	if slot == nil {
		var err error
		slot, err = newNativeCancelSlot()
		if err != nil {
			o.started.Store(false)
			return err
		}
	}
	generation, err := slot.begin()
	if err != nil {
		slot.close()
		o.started.Store(false)
		return err
	}

	o.ctx = ctx
	o.slot = slot
	o.generation = generation
	o.done = make(chan struct{})
	o.watcherDone = make(chan struct{})
	go o.watch()
	return nil
}

// watch waits for context cancellation and requests native cancellation after
// ordinary native-gate admission. It never acquires a connection, transaction,
// statement, or cursor mutex and never owns native cleanup.
func (o *nativeCancelOperation) watch() {
	defer close(o.watcherDone)
	if o.watchStarted != nil {
		close(o.watchStarted)
	}
	select {
	case <-o.done:
		return
	case <-o.ctx.Done():
	}
	if o.finished.Load() {
		return
	}

	release := nativegate.Global.Enter()
	if o.finished.Load() {
		release()
		return
	}
	result, requestErr := o.slot.cancel(o.generation)
	release()

	contextErr := o.ctx.Err()
	if contextErr == nil {
		contextErr = context.Canceled
	}
	o.diagnosticsMu.Lock()
	o.contextErr = contextErr
	o.requestErr = requestErr
	o.nativeCode = result.nativeCode
	if requestErr == nil && result.nativeCode != 0 {
		o.requestErr = &NativeError{
			Operation:  "cancel statement",
			NativeCode: result.nativeCode,
		}
	}
	o.diagnosticsMu.Unlock()
}

// finish publishes operation completion to the watcher, joins it, and only
// then destroys the C slot. The native wrapper must call its C completion
// protocol before invoking finish so ib_cancel_slot_free cannot observe an
// active generation.
func (o *nativeCancelOperation) finish() {
	if o == nil || !o.started.Load() {
		return
	}
	o.close()
}

// close is idempotent and retains the ownership boundary in one place. Every
// destruction path waits for watcherDone, including a close concurrent with a
// finish that has already published the finished signal.
func (o *nativeCancelOperation) close() {
	if o == nil || !o.started.Load() {
		return
	}
	if o.finished.CompareAndSwap(false, true) {
		close(o.done)
	}
	<-o.watcherDone
	if o.closed.CompareAndSwap(false, true) {
		o.slot.close()
	}
}

func (o *nativeCancelOperation) requestError() error {
	if o == nil {
		return nil
	}
	o.diagnosticsMu.Lock()
	defer o.diagnosticsMu.Unlock()
	return o.requestErr
}

func (o *nativeCancelOperation) requestNativeCode() int64 {
	if o == nil {
		return 0
	}
	o.diagnosticsMu.Lock()
	defer o.diagnosticsMu.Unlock()
	return o.nativeCode
}

func (o *nativeCancelOperation) cancellationContext() error {
	if o == nil {
		return nil
	}
	o.diagnosticsMu.Lock()
	defer o.diagnosticsMu.Unlock()
	return o.contextErr
}

func (e *UncertainOutcomeError) Error() string {
	if e == nil {
		return "interbase: operation outcome is uncertain"
	}
	return fmt.Sprintf("interbase: %s outcome is uncertain", safeOperationName(e.Operation, "operation"))
}

// Unwrap retains the operation and cleanup error trees without rendering their
// potentially sensitive native messages in Error.
func (e *UncertainOutcomeError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return joinErrorValues(e.Cause, e.Cleanup)
}

func joinErrorValues(values ...error) []error {
	joined := make([]error, 0, len(values))
	for _, value := range values {
		if value != nil {
			joined = append(joined, value)
		}
	}
	return joined
}

func safeOperationName(operation, fallback string) string {
	operation = strings.TrimSpace(operation)
	if operation == "" {
		return fallback
	}
	return operation
}

// isNativeCancellation reports whether an executing native operation returned
// InterBase's isc_cancelled status. It deliberately does not inspect the
// context: a native cancellation without a canceled context remains a native
// error and must not be relabeled as a context cancellation.
func isNativeCancellation(err error) bool {
	var nativeErr *NativeError
	return errors.As(err, &nativeErr) && nativeErr != nil &&
		nativeErr.NativeCode == nativeCancelledCode
}

// contextCancellation returns the context error that should be retained when
// an executing operation reports isc_cancelled. A nil context is treated as a
// canceled operation only when the caller already supplied a native
// cancellation; normal native methods reject nil contexts before admission.
func contextCancellation(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// classifyNativeOutcome applies the authoritative executing-result rule shared
// by the later database/sql and direct-operation integrations. cleanupErr is
// diagnostic only for a successful operation, but a failed cleanup makes a
// canceled mutating operation uncertain.
func classifyNativeOutcome(operation string, mutating bool, contextErr error,
	nativeErr, cleanupErr error) error {
	if nativeErr == nil {
		if cleanupErr != nil {
			return cleanupErr
		}
		return nil
	}
	if !isNativeCancellation(nativeErr) || contextErr == nil {
		return joinPrimaryCleanup(nativeErr, cleanupErr)
	}

	canceled := &CancellationError{
		Operation: operation,
		Mutating:  mutating,
		Context:   contextErr,
		Native:    nativeErr,
	}
	if mutating && cleanupErr != nil {
		return &UncertainOutcomeError{
			Operation: operation,
			Mutating:  true,
			Cause:     canceled,
			Cleanup:   cleanupErr,
		}
	}
	if cleanupErr != nil {
		return &cancellationCleanupError{
			Operation: operation,
			Cause:     canceled,
			Cleanup:   cleanupErr,
		}
	}
	return canceled
}

func joinPrimaryCleanup(primary, cleanup error) error {
	if primary == nil {
		return cleanup
	}
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, cleanup)
}
