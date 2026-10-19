package interbase

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// lineProfileWarmUpFake is an attachment whose transactions are fake ones.
// events logs, in order, each begin ("begin ro" or "begin rw"), each
// rollback and the first info call (baseline probe or snapshot) after a
// rollback.
type lineProfileWarmUpFake struct {
	conn   *conn
	events []string
	counts []*lineProfileFakeCounts
	// warmUpRows is the number of rows the first transaction's cursors
	// return; fetches counts its fetches.
	warmUpRows, fetches int
	// warmUpNext, when set, replaces the first transaction's fetch.
	warmUpNext func() (bool, error)
	// warmUpCounts configures the first transaction's fake.
	warmUpCounts func(*lineProfileFakeCounts)
}

func newLineProfileWarmUpFake() *lineProfileWarmUpFake {
	f := &lineProfileWarmUpFake{warmUpRows: 3}
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		databaseInfoItemsOverride: func(codes []byte) ([]byte, error) {
			if n := len(f.events); n > 0 && f.events[n-1] != "info" {
				f.events = append(f.events, "info")
			}
			return lineProfileAllPagesInfo(codes)
		},
	}
	native.beginTransactionOverride = func(tpb []byte) (*nativeTransaction, error) {
		mode := "begin rw"
		if len(tpb) > 1 && tpb[1] == 8 {
			mode = "begin ro"
		}
		f.events = append(f.events, mode)
		counts := &lineProfileFakeCounts{}
		first := len(f.counts) == 0
		f.counts = append(f.counts, counts)
		// The measured run's only SELECTs are entry arguments.
		next := func() (bool, error) { return true, nil }
		if first {
			next = func() (bool, error) {
				if f.warmUpNext != nil {
					return f.warmUpNext()
				}
				f.fetches++
				return f.fetches <= f.warmUpRows, nil
			}
			if f.warmUpCounts != nil {
				f.warmUpCounts(counts)
			}
		}
		tx := newLineProfileFakeNative(counts, next, func(string) (int64, error) { return 1, nil })
		rollback := tx.rollbackOverride
		tx.rollbackOverride = func() error {
			f.events = append(f.events, "rollback")
			return rollback()
		}
		return tx, nil
	}
	f.conn = &conn{native: native}
	return f
}

func (f *lineProfileWarmUpFake) run(t *testing.T, ctx context.Context, plan *lpPlan, opts LineProfileOptions) (LineProfileResult, error, error) {
	t.Helper()
	if opts.KeepRows == 0 {
		opts.KeepRows = 10
	}
	result := LineProfileResult{ReadOnly: opts.ReadOnly, Routines: []LineProfileRoutine{newRoutineProfile(plan.entry)}}
	outcomeErr, err := profileOnConn(ctx, f.conn, plan, opts, &result, nil)
	return result, outcomeErr, err
}

const lineProfileWarmUpBody = "BEGIN\n  SUSPEND;\nEND"

func TestLineProfileWarmUpOffRunsNoExtraTransaction(t *testing.T) {
	f := newLineProfileWarmUpFake()
	result, outcomeErr, err := f.run(t, context.Background(), newLineProfileFakePlan(t, lineProfileWarmUpBody), LineProfileOptions{ReadOnly: true})
	if outcomeErr != nil || err != nil {
		t.Fatalf("run = %v, %v", outcomeErr, err)
	}
	if len(f.counts) != 1 || f.fetches != 0 {
		t.Fatalf("transactions %d, warm-up fetches %d; want the run's only (events %v)", len(f.counts), f.fetches, f.events)
	}
	if result.WarmUp != 0 || result.WarmUpErr != nil {
		t.Fatalf("WarmUp %v, WarmUpErr %v; want none", result.WarmUp, result.WarmUpErr)
	}
}

func TestLineProfileWarmUpRunsTheEntryOnceBeforeTheRun(t *testing.T) {
	for _, tt := range []struct {
		name       string
		selectable bool
		args       []string
		readOnly   bool
		want       string
		begin      string
	}{
		{"selectable read-only", true, nil, true, "SELECT * FROM LP_FAKE", "begin ro"},
		{"selectable read-write", true, []string{"1", "'x'"}, false, "SELECT * FROM LP_FAKE(1, 'x')", "begin rw"},
		{"executable", false, []string{"1"}, true, "EXECUTE PROCEDURE LP_FAKE(1)", "begin ro"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			const pause = 30 * time.Millisecond
			f := newLineProfileWarmUpFake()
			f.warmUpCounts = func(c *lineProfileFakeCounts) { c.onPrepare = func() { time.Sleep(pause) } }
			plan := newLineProfileFakePlan(t, lineProfileWarmUpBody)
			plan.analysis.Entry.Selectable = tt.selectable
			plan.analysis.Entry.Args = tt.args
			for i := range tt.args {
				plan.entry.inputs = append(plan.entry.inputs, lpParam{name: string(rune('A' + i)), typeText: "INTEGER"})
			}
			result, outcomeErr, err := f.run(t, context.Background(), plan, LineProfileOptions{ReadOnly: tt.readOnly, WarmUp: true})
			if outcomeErr != nil || err != nil {
				t.Fatalf("run = %v, %v", outcomeErr, err)
			}
			want := []string{tt.begin, "rollback", "info", tt.begin, "info", "rollback", "info"}
			if strings.Join(f.events, ",") != strings.Join(want, ",") {
				t.Fatalf("events %v, want %v", f.events, want)
			}
			warm := f.counts[0]
			if warm.prepared != 1 || warm.prepares[tt.want] != 1 {
				t.Fatalf("warm-up prepares %v, want one %q", warm.prepares, tt.want)
			}
			if warm.closedAtRollback != 1 || warm.cursorsClosedAtRollback != warm.queried {
				t.Fatalf("warm-up statements closed before rollback %d, cursors %d/%d", warm.closedAtRollback, warm.cursorsClosedAtRollback, warm.queried)
			}
			if tt.selectable && (warm.queried != 1 || f.fetches != f.warmUpRows+1) {
				t.Fatalf("warm-up queried %d, fetches %d; want every row fetched", warm.queried, f.fetches)
			}
			if !tt.selectable && warm.queried != 0 {
				t.Fatalf("executable warm-up queried %d", warm.queried)
			}
			if result.WarmUp < pause || result.Setup >= pause || result.Prepare >= pause {
				t.Fatalf("WarmUp %v, Setup %v, Prepare %v; want the warm-up in WarmUp only", result.WarmUp, result.Setup, result.Prepare)
			}
			if result.WarmUpErr != nil || !result.RolledBack {
				t.Fatalf("WarmUpErr %v, RolledBack %v", result.WarmUpErr, result.RolledBack)
			}
		})
	}
}

func TestLineProfileWarmUpQuotedEntryName(t *testing.T) {
	f := newLineProfileWarmUpFake()
	plan := newLineProfileFakePlan(t, lineProfileWarmUpBody)
	plan.analysis.Entry.Procedure, plan.analysis.Entry.Quoted = `lp "q"`, true
	if _, outcomeErr, err := f.run(t, context.Background(), plan, LineProfileOptions{WarmUp: true}); outcomeErr != nil || err != nil {
		t.Fatalf("run = %v, %v", outcomeErr, err)
	}
	if want := `SELECT * FROM "lp ""q"""`; f.counts[0].prepares[want] != 1 {
		t.Fatalf("warm-up prepares %v, want %q", f.counts[0].prepares, want)
	}
}

func TestLineProfileWarmUpFailureContinuesTheRun(t *testing.T) {
	f := newLineProfileWarmUpFake()
	f.warmUpNext = func() (bool, error) {
		return false, &Error{Operation: "fetch", SQLCode: -803, Message: "LP warm-up failure"}
	}
	result, outcomeErr, err := f.run(t, context.Background(), newLineProfileFakePlan(t, lineProfileWarmUpBody), LineProfileOptions{WarmUp: true})
	if outcomeErr != nil || err != nil {
		t.Fatalf("run = %v, %v; want the measured run to go on", outcomeErr, err)
	}
	var nativeErr *Error
	if !errors.As(result.WarmUpErr, &nativeErr) || nativeErr.SQLCode != -803 {
		t.Fatalf("WarmUpErr = %v", result.WarmUpErr)
	}
	if len(f.counts) != 2 || !f.counts[0].rolledBack || !f.counts[1].rolledBack || result.Rows != 1 {
		t.Fatalf("transactions %d, rows %d, events %v", len(f.counts), result.Rows, f.events)
	}
}

func TestLineProfileWarmUpCancelEndsTheCall(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := newLineProfileWarmUpFake()
	f.warmUpNext = func() (bool, error) {
		cancel()
		return true, nil
	}
	result, outcomeErr, err := f.run(t, ctx, newLineProfileFakePlan(t, lineProfileWarmUpBody), LineProfileOptions{WarmUp: true})
	if err != nil || !errors.Is(outcomeErr, context.Canceled) {
		t.Fatalf("run = %v, %v; want canceled", outcomeErr, err)
	}
	if len(f.counts) != 1 || !f.counts[0].rolledBack || f.counts[0].closedAtRollback != 1 {
		t.Fatalf("transactions %d, events %v; want the warm-up only, rolled back after its statement closed", len(f.counts), f.events)
	}
	if result.WarmUp <= 0 || result.WarmUpErr != nil || !result.RolledBack {
		t.Fatalf("WarmUp %v, WarmUpErr %v, RolledBack %v", result.WarmUp, result.WarmUpErr, result.RolledBack)
	}
	finishLineProfile(ctx, &result, outcomeErr, err)
	if result.Outcome != LineProfileCanceled {
		t.Fatalf("outcome %s, want canceled", result.Outcome)
	}
}
