package txcontext

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestExplicitEndSeparatesAdmissionAndLifetime(t *testing.T) {
	type valueKey struct{}
	attempt, cancel := context.WithDeadline(context.WithValue(context.Background(), valueKey{}, "request-value"), time.Now().Add(time.Hour))
	defer cancel()
	lifetime := WithExplicitEnd(attempt)
	admission := BeginAdmission(lifetime)
	if admission != attempt {
		t.Fatal("begin admission must be original attempt, not a fresh budget")
	}
	if lifetime.Done() != nil || lifetime.Err() != nil {
		t.Fatal("explicit transaction end must not auto-cancel")
	}
	if _, ok := lifetime.Deadline(); ok {
		t.Fatal("lifetime retained deadline")
	}
	if got, ok := admission.Deadline(); !ok {
		t.Fatal("admission lost deadline")
	} else {
		want, _ := attempt.Deadline()
		if got != want {
			t.Fatal("admission changed deadline")
		}
	}
	for _, ctx := range []context.Context{lifetime, admission} {
		if ctx.Value(valueKey{}) != "request-value" {
			t.Fatal("context values lost")
		}
	}
	cancel()
	if !errors.Is(admission.Err(), context.Canceled) || lifetime.Err() != nil {
		t.Fatal("cancellation not confined to admission")
	}
}

func TestBeginAdmissionDefaultIsUnchanged(t *testing.T) {
	if BeginAdmission(nil) != nil {
		t.Fatal("nil context must reach driver's ordinary validation")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if BeginAdmission(ctx) != ctx {
		t.Fatal("ordinary context changed")
	}
	cancel()
	if !errors.Is(BeginAdmission(ctx).Err(), context.Canceled) {
		t.Fatal("ordinary cancellation lost")
	}
}

func TestExplicitEndSurvivesDatabaseSQLContextWrapping(t *testing.T) {
	attempt, cancel := context.WithCancel(context.Background())
	defer cancel()
	lifetime := WithExplicitEnd(attempt)
	wrapped, stop := context.WithCancel(lifetime)
	defer stop()
	cancel()
	if BeginAdmission(wrapped) != attempt || wrapped.Err() != nil {
		t.Fatal("database/sql wrapper lost begin admission/lifetime separation")
	}
	stop()
	if !errors.Is(wrapped.Err(), context.Canceled) {
		t.Fatal("explicit lifecycle owner cannot end context")
	}
}
