package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
	"interbase-go/txcontext"
)

// Mirrors the watched-reader's explicit-end context. The initial RED used
// context.WithoutCancel(attempt), reproducing its lost admission cancellation.
func watchedBeginContext(attempt context.Context) context.Context {
	return txcontext.WithExplicitEnd(attempt)
}

func TestWatchedBeginAdmissionCancellation(t *testing.T) {
	for _, mode := range []string{"cancel", "deadline"} {
		t.Run(mode, func(t *testing.T) {
			release := nativegate.Global.EnterExclusive()
			defer release()
			attempt, cancel := context.WithCancel(context.Background())
			if mode == "deadline" {
				attempt, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
			}
			defer cancel()
			validated := make(chan struct{})
			c := &conn{native: &nativeConnection{brokenOverride: func() bool { close(validated); return false }}}
			type outcome struct {
				tx  driver.Tx
				err error
			}
			done := make(chan outcome, 1)
			go func() {
				tx, err := c.BeginTx(watchedBeginContext(attempt), driver.TxOptions{ReadOnly: true, Isolation: driver.IsolationLevel(sql.LevelReadCommitted)})
				done <- outcome{tx, err}
			}()
			<-validated
			select {
			case result := <-done:
				t.Fatalf("begin did not wait at held native gate: %v", result.err)
			case <-time.After(10 * time.Millisecond):
			}
			if mode == "cancel" {
				cancel()
			}
			<-attempt.Done()
			select {
			case result := <-done:
				if result.tx != nil || !errors.Is(result.err, attempt.Err()) {
					t.Fatalf("tx=%v err=%v want=%v", result.tx, result.err, attempt.Err())
				}
				if state := nativegate.Global.State(); !state.Exclusive || state.ActiveCalls != 0 {
					t.Fatalf("cancelled admission leaked gate: %+v", state)
				}
			case <-time.After(100 * time.Millisecond):
				// Release only to drain the failed reproduction; no native server or
				// attachment exists. It then attempts begin against an unavailable native
				// connection instead of observing the cancelled attempt.
				release()
				result := <-done
				t.Fatalf("Begin admission ignored %v while actual gate held; after release tx=%v err=%v", attempt.Err(), result.tx, result.err)
			}
			release()
			admitted, err := nativegate.Global.EnterContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			admitted()
			if state := nativegate.Global.State(); state.Exclusive || state.ActiveCalls != 0 {
				t.Fatalf("gate not reusable: %+v", state)
			}
		})
	}
}

func TestBeginAdmissionOrdinaryDefaultCancellation(t *testing.T) {
	release := nativegate.Global.EnterExclusive()
	defer release()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	validated := make(chan struct{})
	c := &conn{native: &nativeConnection{brokenOverride: func() bool { close(validated); return false }}}
	done := make(chan error, 1)
	go func() {
		tx, err := c.BeginTx(ctx, driver.TxOptions{})
		if tx != nil {
			done <- errors.New("ordinary cancelled begin retained tx")
		} else {
			done <- err
		}
	}()
	<-validated
	select {
	case err := <-done:
		t.Fatalf("ordinary begin did not wait at native gate: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		release()
		<-done
		t.Fatal("ordinary BeginTx cancellation changed")
	}
	if state := nativegate.Global.State(); !state.Exclusive || state.ActiveCalls != 0 {
		t.Fatalf("ordinary cancellation leaked native admission: %+v", state)
	}
}

func TestBeginAdmissionNilContextPreservesValidation(t *testing.T) {
	if tx, err := (&conn{}).BeginTx(nil, driver.TxOptions{}); tx != nil || err == nil {
		t.Fatal("ordinary nil-context validation changed")
	}
}
