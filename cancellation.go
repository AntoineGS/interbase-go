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

// nativeWriteOutcomeState records what the native execution wrapper proved
// after a mutating statement returned isc_cancelled.  A successful implicit
// rollback and a still-live caller-owned transaction are both safe to report
// as cancellation.  Every other state is deliberately conservative: the
// caller must reconcile the write before retrying it.
type nativeWriteOutcomeState uint8

const (
	nativeWriteOutcomeUnknown nativeWriteOutcomeState = iota
	nativeWriteOutcomeRollbackConfirmed
	nativeWriteOutcomeExplicitUsable
)

var errWriteOutcomeUnknown = errors.New("interbase: canceled write outcome could not be established")

// nativeCancellationEvidence records what the cancellation backend observed,
// independently of the executing call's result.  attempted is true when the
// watcher entered the cancellation backend; overlapped is true only when the
// backend observed a published, still-active native statement and invoked the
// native cancellation operation against it.
type nativeCancellationEvidence struct {
	known             bool
	attempted         bool
	overlapped        bool
	executingCanceled bool
}

// nativeExecutionError keeps an execution failure separate from cleanup
// diagnostics returned by the native wrapper.  The public classifier needs to
// distinguish a failed implicit rollback from the authoritative DSQL result,
// while the error tree must retain both diagnostics for callers.
type nativeExecutionError struct {
	primary       error
	cleanup       error
	request       error
	cancellation  nativeCancellationEvidence
	mutating      bool
	mutatingKnown bool
}

func (e *nativeExecutionError) Error() string {
	if e == nil || e.primary == nil {
		return "interbase: native execution failed"
	}
	return e.primary.Error()
}

func (e *nativeExecutionError) Unwrap() []error {
	if e == nil {
		return nil
	}
	return joinErrorValues(e.primary, e.cleanup, e.request)
}

func splitNativeExecutionError(err error) (primary, cleanup error) {
	if err == nil {
		return nil, nil
	}
	var executionErr *nativeExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil {
		return splitNativeCleanupDiagnostic(err)
	}
	return executionErr.primary,
		joinPrimaryCleanup(executionErr.cleanup, executionErr.request)
}

func nativeCancellationEvidenceOf(err error) nativeCancellationEvidence {
	if err == nil {
		return nativeCancellationEvidence{}
	}
	var executionErr *nativeExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil {
		return nativeCancellationEvidence{}
	}
	evidence := executionErr.cancellation
	evidence.known = true
	return evidence
}

// splitNativeCleanupDiagnostic decodes the legacy C error representation used
// by wrappers that predate nativeExecutionError.  The C bridge preserves both
// messages as "primary; cleanup" in one status string; split it before Go
// sanitization so the public uncertain error retains a distinct cleanup
// NativeError without requiring a second borrowed C allocation.
func splitNativeCleanupDiagnostic(err error) (primary, cleanup error) {
	var nativeErr *NativeError
	if err == nil || !errors.As(err, &nativeErr) || nativeErr == nil {
		return err, nil
	}
	const separator = "; "
	separatorIndex := strings.Index(nativeErr.Message, separator)
	if separatorIndex < 0 {
		return err, nil
	}
	cleanupMessage := strings.TrimSpace(nativeErr.Message[separatorIndex+len(separator):])
	if !strings.Contains(cleanupMessage, " failed (SQLCODE ") {
		return err, nil
	}
	primaryCopy := *nativeErr
	primaryCopy.Message = nativeErr.Message[:separatorIndex]
	return &primaryCopy, parseNativeError(cleanupMessage)
}

func wrapNativeExecutionErrorWithMetadata(primary, cleanup, request error,
	evidence nativeCancellationEvidence, mutating ...bool) error {
	mutatingKnown := len(mutating) != 0
	mutates := mutatingKnown && mutating[0]
	if primary == nil && cleanup == nil && request == nil {
		return nil
	}
	if primary == nil {
		if cleanup != nil && request == nil && !evidence.known &&
			!evidence.attempted && !evidence.overlapped && !evidence.executingCanceled &&
			!mutatingKnown {
			return cleanup
		}
	}
	if cleanup == nil && request == nil && !evidence.known &&
		!evidence.attempted && !evidence.overlapped && !evidence.executingCanceled &&
		!mutatingKnown {
		return primary
	}
	evidence.known = true
	evidence.executingCanceled = isNativeCancellation(primary)
	return &nativeExecutionError{
		primary:       primary,
		cleanup:       cleanup,
		request:       request,
		cancellation:  evidence,
		mutating:      mutates,
		mutatingKnown: mutatingKnown,
	}
}

func (o *nativeCancelOperation) wrapExecutionError(primary, cleanup error) error {
	return o.wrapExecutionErrorMutating(primary, cleanup, false, false)
}

func (o *nativeCancelOperation) wrapQueryExecutionError(primary, cleanup error,
	mutating bool) error {
	return o.wrapExecutionErrorMutating(primary, cleanup, true, mutating)
}

func (o *nativeCancelOperation) wrapExecutionErrorMutating(primary, cleanup error,
	mutatingKnown, mutating bool) error {
	var nested *nativeExecutionError
	if errors.As(primary, &nested) && nested != nil {
		var nestedCleanup error
		primary, nestedCleanup = splitNativeExecutionError(primary)
		cleanup = joinPrimaryCleanup(nestedCleanup, cleanup)
		if !mutatingKnown && nested.mutatingKnown {
			mutatingKnown = true
			mutating = nested.mutating
		}
	}
	if mutatingKnown {
		return wrapNativeExecutionErrorWithMetadata(primary, cleanup,
			o.requestError(), o.cancellationEvidence(), mutating)
	}
	return wrapNativeExecutionErrorWithMetadata(primary, cleanup,
		o.requestError(), o.cancellationEvidence())
}

func wrapNativeQueryMutability(err error, mutating bool) error {
	if err == nil {
		return nil
	}
	primary, cleanup := splitNativeCleanupDiagnostic(err)
	return wrapNativeExecutionErrorWithMetadata(primary, cleanup, nil,
		nativeCancellationEvidence{}, mutating)
}

func nativeQueryMutatingOf(err error) bool {
	if err == nil {
		return false
	}
	var executionErr *nativeExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil ||
		!executionErr.mutatingKnown {
		return false
	}
	return executionErr.mutating
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
	// Optional deterministic test barriers; nil in production.
	finishWaitStarted chan struct{}
	closeWaitStarted  chan struct{}
	finishWaitOnce    sync.Once
	closeWaitOnce     sync.Once

	started  atomic.Bool
	finished atomic.Bool
	closed   atomic.Bool

	diagnosticsMu     sync.Mutex
	contextErr        error
	requestErr        error
	nativeCode        int64
	requestAttempted  bool
	requestOverlapped bool
}

// beginNativeCancelOperation allocates a slot, starts its generation, and
// launches exactly one context watcher. A context already carrying an error is
// rejected before any slot is allocated or generation is begun.
func beginNativeCancelOperation(ctx context.Context) (*nativeCancelOperation, error) {
	return newNativeCancelOperationWithSlot(ctx, nil)
}

// newNativeCancelOperation is kept as the descriptive constructor used by
// native call sites that do not need to distinguish allocation from begin.
func newNativeCancelOperation(ctx context.Context) (*nativeCancelOperation, error) {
	return newNativeCancelOperationWithSlot(ctx, nil)
}

func newNativeCancelOperationWithSlot(ctx context.Context,
	slot nativeCancelSlotBackend) (*nativeCancelOperation, error) {
	op := &nativeCancelOperation{slot: slot}
	if err := op.begin(ctx); err != nil {
		return nil, err
	}
	return op, nil
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
		slot, err = nativeCancelSlotFactory()
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
	o.requestAttempted = result.attempted
	o.requestOverlapped = result.overlapped
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
	if o.finished.CompareAndSwap(false, true) {
		close(o.done)
	}
	o.waitForWatcher(o.finishWaitStarted, &o.finishWaitOnce)
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
	o.waitForWatcher(o.closeWaitStarted, &o.closeWaitOnce)
}

func (o *nativeCancelOperation) waitForWatcher(waitStarted chan struct{}, once *sync.Once) {
	if waitStarted != nil {
		once.Do(func() { close(waitStarted) })
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

func (o *nativeCancelOperation) cancellationEvidence() nativeCancellationEvidence {
	if o == nil {
		return nativeCancellationEvidence{}
	}
	o.diagnosticsMu.Lock()
	defer o.diagnosticsMu.Unlock()
	return nativeCancellationEvidence{
		known:      true,
		attempted:  o.requestAttempted,
		overlapped: o.requestOverlapped,
	}
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

// isNativeUnknownResponse reports failures for which an overlapped cancel can
// explain why the executing response was not authoritative. A definitive
// server status must remain authoritative even when cancellation overlapped it.
func isNativeUnknownResponse(err error) bool {
	var nativeErr *NativeError
	if !errors.As(err, &nativeErr) || nativeErr == nil {
		return true
	}
	if nativeErr.NativeCode == 0 {
		return true
	}
	switch nativeErr.NativeCode {
	case 335544721, // isc_network_error
		335544722, // isc_net_connect_err
		335544723, // isc_net_connect_listen_err
		335544724, // isc_net_event_connect_err
		335544725, // isc_net_event_listen_err
		335544726, // isc_net_read_err
		335544727, // isc_net_write_err
		335544741, // isc_lost_db_connection
		335544751: // isc_bad_protocol
		return true
	default:
		return false
	}
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
	nativeErr, cleanupErr error, evidence ...nativeCancellationEvidence) error {
	return classifyNativeWriteOutcome(operation, mutating, contextErr, nativeErr,
		cleanupErr, nativeWriteOutcomeExplicitUsable, evidence...)
}

// classifyNativeWriteOutcome applies the authoritative executing-result rule
// and additionally requires a known post-cancellation state for mutating
// operations.  Unknown state is an uncertainty even when the native wrapper
// could not return a separate cleanup diagnostic (for example, after a broken
// connection consumed the response).
func classifyNativeWriteOutcome(operation string, mutating bool, contextErr error,
	nativeErr, cleanupErr error, outcomeState nativeWriteOutcomeState,
	evidence ...nativeCancellationEvidence) error {
	if nativeErr == nil {
		if cleanupErr != nil {
			return cleanupErr
		}
		return nil
	}
	cancellationEvidence := nativeCancellationEvidence{}
	if len(evidence) != 0 {
		cancellationEvidence = evidence[0]
	}
	executingCanceled := isNativeCancellation(nativeErr)
	if cancellationEvidence.known {
		executingCanceled = cancellationEvidence.executingCanceled
	}
	overlappedCancellation := cancellationEvidence.attempted &&
		cancellationEvidence.overlapped && isNativeUnknownResponse(nativeErr)
	if (!executingCanceled && !overlappedCancellation) || contextErr == nil {
		return joinPrimaryCleanup(nativeErr, cleanupErr)
	}

	canceled := &CancellationError{
		Operation: operation,
		Mutating:  mutating,
		Context:   contextErr,
		Native:    nativeErr,
	}
	if mutating && (cleanupErr != nil || outcomeState == nativeWriteOutcomeUnknown) {
		if cleanupErr == nil {
			cleanupErr = errWriteOutcomeUnknown
		}
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
