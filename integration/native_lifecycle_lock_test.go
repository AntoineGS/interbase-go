//go:build integration

package integration_test

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/internal/nativegate"
)

const lifecycleLockProgressTimeout = 10 * time.Second

// isc_update_conflict from the InterBase client status definitions.
const iscUpdateConflict int64 = 335544451

func waitForNativeGateState(t *testing.T, predicate func(nativegate.State) bool) {
	t.Helper()
	deadline := time.Now().Add(lifecycleLockProgressTimeout)
	for {
		state := nativegate.Global.State()
		if predicate(state) {
			return
		}
		if !time.Now().Before(deadline) {
			t.Fatalf("native gate did not reach the expected state; last state = %+v", state)
		}
		runtime.Gosched()
	}
}

// TestNativeLifecycleWaitAllowsRowLockRelease proves the admission property
// that makes a quiescent lifecycle gate safe: a transaction waiting on a row
// lock remains active while lifecycle cleanup waits, but the transaction that
// owns that lock can still enter and release it.
func TestNativeLifecycleWaitAllowsRowLockRelease(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleLockProgressTimeout)
	defer cancel()

	holder := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})
	contender := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})
	lifecycle := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})

	holderTx, err := holder.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin lock-holder transaction: %v", err)
	}
	if _, err := holderTx.Exec(ctx, "UPDATE GO_WRITE SET WRITE_VALUE = WRITE_VALUE + 1 WHERE ID = 1"); err != nil {
		_ = holderTx.Rollback()
		t.Fatalf("acquire row lock: %v", err)
	}

	contenderTx, err := contender.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		_ = holderTx.Rollback()
		t.Fatalf("begin lock-contender transaction: %v", err)
	}
	contenderDone := make(chan error, 1)
	go func() {
		_, execErr := contenderTx.Exec(ctx, "UPDATE GO_WRITE SET WRITE_VALUE = WRITE_VALUE + 1 WHERE ID = 1")
		rollbackErr := contenderTx.Rollback()
		contenderDone <- errors.Join(execErr, rollbackErr)
	}()
	waitForNativeGateState(t, func(state nativegate.State) bool {
		return state.ActiveCalls != 0
	})

	lifecycleDone := make(chan error, 1)
	go func() {
		lifecycleDone <- lifecycle.Close()
	}()
	waitForNativeGateState(t, func(state nativegate.State) bool {
		return state.ExclusiveWaiters != 0 && state.ActiveCalls != 0
	})

	holderRollbackDone := make(chan error, 1)
	go func() {
		holderRollbackDone <- holderTx.Rollback()
	}()
	select {
	case err := <-holderRollbackDone:
		if err != nil {
			t.Fatalf("release row lock while lifecycle waits: %v", err)
		}
	case <-time.After(lifecycleLockProgressTimeout):
		t.Fatal("row-lock owner could not enter while lifecycle waited for quiescence")
	}

	select {
	case err := <-contenderDone:
		if err != nil {
			t.Fatalf("finish lock-contender transaction: %v", err)
		}
	case <-time.After(lifecycleLockProgressTimeout):
		t.Fatal("lock contender did not finish after its row lock was released")
	}
	select {
	case err := <-lifecycleDone:
		if err != nil {
			t.Fatalf("close lifecycle attachment: %v", err)
		}
	case <-time.After(lifecycleLockProgressTimeout):
		t.Fatal("lifecycle close did not finish after native calls drained")
	}
}

// TestNativeLifecycleWaitAllowsRowLockCommit proves that lifecycle cleanup
// does not strand an active conflicting update when its row-lock owner commits.
// A conflicting update can either apply after the committed version or report
// InterBase's documented update conflict; no other failure is acceptable.
func TestNativeLifecycleWaitAllowsRowLockCommit(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	ctx, cancel := context.WithTimeout(context.Background(), lifecycleLockProgressTimeout)
	defer cancel()

	owner := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})
	contender := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})
	lifecycle := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})
	verifier := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", "", 1, interbase.TransactionOptions{})

	seedTx, err := owner.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin seed transaction: %v", err)
	}
	if _, err := seedTx.Exec(ctx, "INSERT INTO GO_WRITE (ID, WRITE_VALUE) VALUES (1, 0)"); err != nil {
		_ = seedTx.Rollback()
		t.Fatalf("seed locked row: %v", err)
	}
	if err := seedTx.Commit(); err != nil {
		t.Fatalf("commit seed transaction: %v", err)
	}

	ownerTx, err := owner.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin row-lock owner transaction: %v", err)
	}
	if _, err := ownerTx.Exec(ctx, "UPDATE GO_WRITE SET WRITE_VALUE = 1 WHERE ID = 1"); err != nil {
		_ = ownerTx.Rollback()
		t.Fatalf("acquire row lock: %v", err)
	}

	contenderTx, err := contender.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		_ = ownerTx.Rollback()
		t.Fatalf("begin conflicting-update transaction: %v", err)
	}
	type contenderResult struct {
		execErr     error
		rollbackErr error
		commitErr   error
	}
	contenderDone := make(chan contenderResult, 1)
	go func() {
		_, execErr := contenderTx.Exec(ctx, "UPDATE GO_WRITE SET WRITE_VALUE = 2 WHERE ID = 1")
		if execErr != nil {
			contenderDone <- contenderResult{execErr: execErr, rollbackErr: contenderTx.Rollback()}
			return
		}
		contenderDone <- contenderResult{commitErr: contenderTx.Commit()}
	}()
	waitForNativeGateState(t, func(state nativegate.State) bool {
		return !state.Exclusive && state.ActiveCalls != 0
	})

	lifecycleDone := make(chan error, 1)
	go func() {
		lifecycleDone <- lifecycle.Close()
	}()
	waitForNativeGateState(t, func(state nativegate.State) bool {
		return !state.Exclusive && state.ExclusiveWaiters != 0 && state.ActiveCalls != 0
	})

	ownerCommitDone := make(chan error, 1)
	go func() {
		ownerCommitDone <- ownerTx.Commit()
	}()
	select {
	case err := <-ownerCommitDone:
		if err != nil {
			t.Fatalf("commit row-lock owner while lifecycle waits: %v", err)
		}
	case <-time.After(lifecycleLockProgressTimeout):
		t.Fatal("row-lock owner could not enter to commit while lifecycle waited for quiescence")
	}

	var wantValue int64 = 2
	select {
	case result := <-contenderDone:
		if result.execErr != nil {
			if result.rollbackErr != nil {
				t.Fatalf("rollback conflicting-update transaction: %v", result.rollbackErr)
			}
			nativeErr := requireNativeError(t, result.execErr)
			if nativeErr.NativeCode != iscUpdateConflict {
				t.Fatalf("conflicting update native status = %d, want isc_update_conflict (%d); SQLCODE=%d",
					nativeErr.NativeCode, iscUpdateConflict, nativeErr.SQLCode)
			}
			wantValue = 1
		} else if result.commitErr != nil {
			nativeErr := requireNativeError(t, result.commitErr)
			if nativeErr.NativeCode != iscUpdateConflict {
				t.Fatalf("conflicting update commit native status = %d, want isc_update_conflict (%d); SQLCODE=%d",
					nativeErr.NativeCode, iscUpdateConflict, nativeErr.SQLCode)
			}
			wantValue = 1
		}
	case <-time.After(lifecycleLockProgressTimeout):
		t.Fatal("conflicting update did not finish after the row-lock owner committed")
	}

	select {
	case err := <-lifecycleDone:
		if err != nil {
			t.Fatalf("close lifecycle attachment: %v", err)
		}
	case <-time.After(lifecycleLockProgressTimeout):
		t.Fatal("lifecycle close did not finish after native calls drained")
	}

	verifyTx, err := verifier.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin persisted-value transaction: %v", err)
	}
	cursor, err := verifyTx.Query(ctx, "SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = 1")
	if err != nil {
		_ = verifyTx.Rollback()
		t.Fatalf("query persisted row-lock value: %v", err)
	}
	hasRow, err := cursor.Next(ctx)
	if err != nil || !hasRow {
		_ = cursor.Close()
		_ = verifyTx.Rollback()
		t.Fatalf("read persisted row-lock value = (%t, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil {
		_ = cursor.Close()
		_ = verifyTx.Rollback()
		t.Fatalf("read persisted row-lock fields: %v", err)
	}
	if err := cursor.Close(); err != nil {
		_ = verifyTx.Rollback()
		t.Fatalf("close persisted-value cursor: %v", err)
	}
	if err := verifyTx.Rollback(); err != nil {
		t.Fatalf("rollback persisted-value transaction: %v", err)
	}
	if len(row) != 1 || row[0].Value != wantValue {
		t.Fatalf("persisted row-lock value = %#v, want %d", row, wantValue)
	}

	if state := nativegate.Global.State(); state != (nativegate.State{}) {
		t.Fatalf("native gate after lifecycle cleanup = %+v, want empty", state)
	}
}
