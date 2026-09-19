//go:build integration

package interbase

import "sync"

var nativeDSQLCompletionTestHook struct {
	sync.Mutex
	entered chan<- struct{}
	release <-chan struct{}
}

// SetNativeDSQLCompletionTestHook installs an integration-build-only boundary
// hook after C DSQL completion and before Go result classification.
func SetNativeDSQLCompletionTestHook(entered chan<- struct{}, release <-chan struct{}) func() {
	nativeDSQLCompletionTestHook.Lock()
	nativeDSQLCompletionTestHook.entered = entered
	nativeDSQLCompletionTestHook.release = release
	nativeDSQLCompletionTestHook.Unlock()
	return func() {
		nativeDSQLCompletionTestHook.Lock()
		nativeDSQLCompletionTestHook.entered = nil
		nativeDSQLCompletionTestHook.release = nil
		nativeDSQLCompletionTestHook.Unlock()
	}
}

func nativeDSQLCompletionHook() {
	nativeDSQLCompletionTestHook.Lock()
	entered, release := nativeDSQLCompletionTestHook.entered, nativeDSQLCompletionTestHook.release
	nativeDSQLCompletionTestHook.Unlock()
	if entered != nil {
		entered <- struct{}{}
		<-release
	}
}
