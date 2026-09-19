//go:build !integration

package interbase

// nativeDSQLCompletionHook is intentionally allocation-free in public builds.
func nativeDSQLCompletionHook() {}
