// Package txcontext separates cancellable InterBase database/sql BeginTx
// admission from a transaction lifetime whose ending is owned by its caller.
// It has no native client or cgo dependency.
package txcontext

import "context"

type admissionKey struct{}

// WithExplicitEnd returns a transaction-lifetime context carrying attempt's
// values but not its cancellation/deadline. The InterBase driver still uses the
// original attempt for native BeginTx admission. This is an optional capability
// for database/sql callers that must synchronously own Commit/Rollback and its
// errors instead of database/sql's automatic cancellation rollback.
//
// The caller MUST end every successful transaction explicitly, including when
// attempt expires while native BeginTx is in flight, and use attempt for its
// subsequent queries. This does not stop an already-running native begin or
// introduce a new timeout. Pass a non-nil attempt, as with standard contexts.
func WithExplicitEnd(attempt context.Context) context.Context {
	return context.WithValue(context.WithoutCancel(attempt), admissionKey{}, attempt)
}

// BeginAdmission returns the original attempt carried by WithExplicitEnd.
// Otherwise it returns ctx unchanged, preserving ordinary BeginTx semantics.
// This function is for the driver's BeginTx admission path only, not queries or
// transaction cleanup. Context wrappers used by database/sql preserve the value.
func BeginAdmission(ctx context.Context) context.Context {
	if ctx == nil {
		return nil
	}
	if attempt, ok := ctx.Value(admissionKey{}).(context.Context); ok {
		return attempt
	}
	return ctx
}
