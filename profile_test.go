package interbase

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

func TestNormalizeProfileOptionsDefaultsMaxRows(t *testing.T) {
	got, err := normalizeProfileOptions(ProfileOptions{})
	if err != nil {
		t.Fatalf("normalizeProfileOptions(): %v", err)
	}
	if got.MaxRows != 256 {
		t.Fatalf("MaxRows = %d, want 256", got.MaxRows)
	}
}

func TestNormalizeProfileOptionsRejectsNegativeMaxRows(t *testing.T) {
	if _, err := normalizeProfileOptions(ProfileOptions{MaxRows: -1}); err == nil {
		t.Fatal("normalizeProfileOptions() succeeded with a negative MaxRows")
	}
}

func TestFinishProfileSetsActivityDelta(t *testing.T) {
	before := Activity{Tables: map[int]TableCounters{7: {SeqReads: 10}}}
	after := Activity{Tables: map[int]TableCounters{7: {SeqReads: 15, Updates: 2}}}

	got, err := finishProfile(ProfileResult{}, before, after)
	if err != nil {
		t.Fatalf("finishProfile(): %v", err)
	}
	if want := (TableCounters{SeqReads: 5, Updates: 2}); got.Activity.Tables[7] != want {
		t.Fatalf("Activity.Tables[7] = %+v, want %+v", got.Activity.Tables[7], want)
	}
}

func TestFinishProfilePropagatesActivityCounterReset(t *testing.T) {
	before := Activity{Tables: map[int]TableCounters{7: {SeqReads: 10}}}
	after := Activity{Tables: map[int]TableCounters{7: {SeqReads: 4}}}

	_, err := finishProfile(ProfileResult{}, before, after)
	if !errors.Is(err, ErrActivityCounterReset) {
		t.Fatalf("finishProfile() error = %v, want ErrActivityCounterReset", err)
	}
}

func TestRunProfileTransactionPanicRetainsRollbackFailure(t *testing.T) {
	profilePanic := errors.New("profile operation panicked")
	rollbackErr := errors.New("native rollback failed")
	rolledBack := false

	panicValue := captureProfilePanic(func() {
		_ = runProfileTransaction(func() error { return rollbackErr }, func() error {
			panic(profilePanic)
		}, &rolledBack)
	})
	got, ok := panicValue.(error)
	if !ok {
		t.Fatalf("panic value type = %T, want error carrying panic and rollback errors", panicValue)
	}
	if !errors.Is(got, profilePanic) {
		t.Fatalf("panic error = %v, want original panic %v", got, profilePanic)
	}
	if !errors.Is(got, ErrProfileRollback) || !errors.Is(got, rollbackErr) {
		t.Fatalf("panic error = %v, want ErrProfileRollback and %v", got, rollbackErr)
	}
	if rolledBack {
		t.Fatal("rolledBack = true after rollback failure")
	}
}

func TestRunProfileTransactionPreservesPanicWhenRollbackSucceeds(t *testing.T) {
	profilePanic := &struct{ message string }{message: "profile panic"}
	rolledBack := false
	panicValue := captureProfilePanic(func() {
		_ = runProfileTransaction(func() error { return nil }, func() error {
			panic(profilePanic)
		}, &rolledBack)
	})
	if panicValue != profilePanic {
		t.Fatalf("panic value = %#v, want original value %#v", panicValue, profilePanic)
	}
	if !rolledBack {
		t.Fatal("rolledBack = false after successful rollback")
	}
}

func TestProfilePanicReleasesNativeGate(t *testing.T) {
	const childEnvironment = "INTERBASE_PROFILE_PANIC_GATE_CHILD"
	if os.Getenv(childEnvironment) == "1" {
		testProfilePanicReleasesNativeGateInChild(t)
		return
	}

	command := exec.Command(os.Args[0], "-test.run=^TestProfilePanicReleasesNativeGate$")
	command.Env = append(os.Environ(), childEnvironment+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("isolated gate-release test failed: %v\n%s", err, output)
	}
}

func testProfilePanicReleasesNativeGateInChild(t *testing.T) {
	native := introspectionTestNative(3)
	native.databaseInfoItemsOverride = perCodeInfoItems(func(code byte) ([]byte, error) {
		if (code >= 5 && code <= 8) || (code >= 23 && code <= 30) {
			return introspectionInfoPayload(code, nil), nil
		}
		return nil, errors.New("unexpected database information code")
	})
	native.beginTransactionOverride = func([]byte) (*nativeTransaction, error) {
		return &nativeTransaction{
			prepareContextOverride: func(string, bool, *nativeCancelOperation) (*nativeStatement, error) {
				panic("injected prepare panic")
			},
			rollbackOverride:    func() error { return nil },
			handleStateOverride: func() nativeHandleState { return nativeHandleConsumed },
			freeOverride:        func() {},
		}, nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	if got := captureProfilePanic(func() {
		_, _ = Profile(context.Background(), pooled, "SELECT 1 FROM RDB$DATABASE", ProfileOptions{})
	}); got != "injected prepare panic" {
		t.Fatalf("Profile panic = %#v, want injected prepare panic", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	release, err := nativegate.Global.EnterExclusiveContext(ctx)
	if err != nil {
		t.Fatalf("exclusive native gate admission after panic: %v", err)
	}
	release()
}

func captureProfilePanic(fn func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	fn()
	return nil
}
