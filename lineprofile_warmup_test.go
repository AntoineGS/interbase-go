package interbase

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// lineProfileWarmUpFake is an attachment whose transactions are fake ones:
// each cursor of a transaction returns rows rows. events logs, in order,
// each begin ("begin ro" or "begin rw"), each rollback and the first info
// call (baseline probe or snapshot) after another event.
type lineProfileWarmUpFake struct {
	conn      *conn
	events    []string
	counts    []*lineProfileFakeCounts
	execs     [][]string // statements executed per transaction
	rows      int
	infoCalls int
	beginErr  error // fails the first begin
	// warmUpNext, when set, replaces the first transaction's fetch;
	// warmUpExec its exec.
	warmUpNext func() (bool, error)
	warmUpExec func(query string) (int64, error)
}

func newLineProfileWarmUpFake() *lineProfileWarmUpFake {
	f := &lineProfileWarmUpFake{rows: 2}
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		// The page counters grow with every info call, so a delta of the
		// warm-up pass leaking into the measured result would show.
		databaseInfoItemsOverride: func(codes []byte) ([]byte, error) {
			if n := len(f.events); n > 0 && f.events[n-1] != "info" {
				f.events = append(f.events, "info")
			}
			f.infoCalls++
			var items [][]byte
			for _, code := range codes {
				if code < 5 || code > 8 {
					continue
				}
				items = append(items, infoItemRecord(code, byte(f.infoCalls), byte(f.infoCalls>>8), 0, 0))
			}
			return infoItemsFrame(items...), nil
		},
	}
	native.beginTransactionOverride = func(tpb []byte) (*nativeTransaction, error) {
		mode := "begin rw"
		if len(tpb) > 1 && tpb[1] == 8 {
			mode = "begin ro"
		}
		if f.beginErr != nil && len(f.counts) == 0 {
			err := f.beginErr
			f.beginErr = nil
			return nil, err
		}
		f.events = append(f.events, mode)
		index := len(f.counts)
		counts := &lineProfileFakeCounts{}
		f.counts = append(f.counts, counts)
		f.execs = append(f.execs, nil)
		fetched := 0
		next := func() (bool, error) {
			fetched++
			return fetched <= f.rows, nil
		}
		exec := func(query string) (int64, error) {
			f.execs[index] = append(f.execs[index], query)
			return 1, nil
		}
		if index == 0 {
			if f.warmUpNext != nil {
				next = f.warmUpNext
			}
			if f.warmUpExec != nil {
				exec = func(query string) (int64, error) {
					f.execs[index] = append(f.execs[index], query)
					return f.warmUpExec(query)
				}
			}
		}
		tx := newLineProfileFakeNative(counts, next, exec)
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

func (f *lineProfileWarmUpFake) run(t *testing.T, ctx context.Context, opts LineProfileOptions) (LineProfileResult, error, error) {
	t.Helper()
	opts.KeepRows = 10
	plan := newLineProfileFakePlan(t, lineProfileWarmUpBody)
	result := LineProfileResult{ReadOnly: opts.ReadOnly, Routines: []LineProfileRoutine{newRoutineProfile(plan.entry)}}
	outcomeErr, err := profileOnConn(ctx, f.conn, plan, opts, &result, nil)
	return result, outcomeErr, err
}

const lineProfileWarmUpBody = "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
	"  FOR SELECT ID FROM LP_T INTO :X DO\n" +
	"  BEGIN\n" +
	"    UPDATE LP_T SET A = :X;\n" +
	"    SUSPEND;\n" +
	"  END\n" +
	"END"

// lineProfileWarmUpShape is the timing-free part of a result.
type lineProfileWarmUpShape struct {
	Outcome     error
	Rows        int64
	ServerCalls int64
	Nodes       []LineProfileNode
	Overhead    ActivityDelta
}

func lineProfileShape(result LineProfileResult, outcomeErr error) lineProfileWarmUpShape {
	shape := lineProfileWarmUpShape{Outcome: outcomeErr, Rows: result.Rows, ServerCalls: result.ServerCalls, Overhead: result.OverheadActivity}
	for _, node := range result.Routines[0].Nodes {
		m := node.Metrics
		node.Metrics = LineMetrics{Hits: m.Hits, ServerCalls: m.ServerCalls, Rows: m.Rows, Errors: m.Errors, Pages: m.Pages, Tables: m.Tables}
		shape.Nodes = append(shape.Nodes, node)
	}
	return shape
}

func TestLineProfileWarmUpOffRunsNoExtraTransaction(t *testing.T) {
	f := newLineProfileWarmUpFake()
	result, outcomeErr, err := f.run(t, context.Background(), LineProfileOptions{ReadOnly: true})
	if outcomeErr != nil || err != nil {
		t.Fatalf("run = %v, %v", outcomeErr, err)
	}
	if len(f.counts) != 1 {
		t.Fatalf("transactions %d; want the run's only (events %v)", len(f.counts), f.events)
	}
	if result.WarmUp != 0 || result.WarmUpErr != nil {
		t.Fatalf("WarmUp %v, WarmUpErr %v; want none", result.WarmUp, result.WarmUpErr)
	}
}

// TestLineProfileWarmUpIsADiscardedPass checks Ruling W1: the warm-up runs
// the same emulation once in its own rolled-back transaction before the
// run's baselines, and the measured run starts fresh with nothing of it.
func TestLineProfileWarmUpIsADiscardedPass(t *testing.T) {
	for _, readOnly := range []bool{true, false} {
		cold := newLineProfileWarmUpFake()
		coldResult, coldOutcome, coldErr := cold.run(t, context.Background(), LineProfileOptions{ReadOnly: readOnly})
		f := newLineProfileWarmUpFake()
		result, outcomeErr, err := f.run(t, context.Background(), LineProfileOptions{ReadOnly: readOnly, WarmUp: true})
		if outcomeErr != nil || err != nil || coldOutcome != nil || coldErr != nil {
			t.Fatalf("read-only %v: run = %v, %v; cold %v, %v", readOnly, outcomeErr, err, coldOutcome, coldErr)
		}
		begin := "begin rw"
		if readOnly {
			begin = "begin ro"
		}
		want := []string{begin, "info", "rollback", "info", begin, "info", "rollback", "info"}
		if strings.Join(f.events, ",") != strings.Join(want, ",") {
			t.Fatalf("read-only %v: events %v, want %v", readOnly, f.events, want)
		}
		warm, measured := f.counts[0], f.counts[1]
		if !reflect.DeepEqual(warm.prepares, measured.prepares) || !reflect.DeepEqual(f.execs[0], f.execs[1]) ||
			warm.queried != measured.queried || len(f.execs[1]) != cold.rows {
			t.Fatalf("warm-up prepares %v execs %v, run prepares %v execs %v; want the same node sequence",
				warm.prepares, f.execs[0], measured.prepares, f.execs[1])
		}
		if !warm.rolledBack || warm.closedAtRollback != warm.prepared || warm.cursorsClosedAtRollback != warm.queried {
			t.Fatalf("warm-up rolled back %v, statements closed before %d/%d, cursors %d/%d",
				warm.rolledBack, warm.closedAtRollback, warm.prepared, warm.cursorsClosedAtRollback, warm.queried)
		}
		if got, want := lineProfileShape(result, outcomeErr), lineProfileShape(coldResult, coldOutcome); !reflect.DeepEqual(got, want) {
			t.Fatalf("measured run %+v, want as without warm-up %+v", got, want)
		}
		if len(result.KeptRows) != len(coldResult.KeptRows) || result.WarmUp <= 0 || result.WarmUpErr != nil || !result.RolledBack {
			t.Fatalf("kept %d/%d, WarmUp %v, WarmUpErr %v, RolledBack %v",
				len(result.KeptRows), len(coldResult.KeptRows), result.WarmUp, result.WarmUpErr, result.RolledBack)
		}
	}
}

func TestLineProfileWarmUpFailureContinuesTheRun(t *testing.T) {
	f := newLineProfileWarmUpFake()
	f.warmUpExec = func(string) (int64, error) {
		return 0, &Error{Operation: "exec", SQLCode: -803, Message: "LP warm-up failure"}
	}
	result, outcomeErr, err := f.run(t, context.Background(), LineProfileOptions{WarmUp: true})
	if outcomeErr != nil || err != nil {
		t.Fatalf("run = %v, %v; want the measured run to go on", outcomeErr, err)
	}
	var nativeErr *Error
	var located *lpNodeError
	if !errors.As(result.WarmUpErr, &nativeErr) || nativeErr.SQLCode != -803 || errors.As(result.WarmUpErr, &located) {
		t.Fatalf("WarmUpErr = %#v; want the driver error", result.WarmUpErr)
	}
	if len(f.counts) != 2 || !f.counts[0].rolledBack || !f.counts[1].rolledBack || result.Rows != 2 {
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
	result, outcomeErr, err := f.run(t, ctx, LineProfileOptions{WarmUp: true})
	if err != nil || !errors.Is(outcomeErr, context.Canceled) {
		t.Fatalf("run = %v, %v; want canceled", outcomeErr, err)
	}
	warm := f.counts[0]
	if len(f.counts) != 1 || !warm.rolledBack || warm.closedAtRollback != warm.prepared {
		t.Fatalf("transactions %d, events %v; want the warm-up only, rolled back after its statements closed", len(f.counts), f.events)
	}
	if result.WarmUp <= 0 || result.WarmUpErr != nil || !result.RolledBack || result.ServerCalls != 0 || result.Before.Samples != 0 {
		t.Fatalf("WarmUp %v, WarmUpErr %v, RolledBack %v, calls %d, baseline %+v",
			result.WarmUp, result.WarmUpErr, result.RolledBack, result.ServerCalls, result.Before)
	}
	finishLineProfile(ctx, &result, outcomeErr, err)
	if result.Outcome != LineProfileCanceled {
		t.Fatalf("outcome %s, want canceled", result.Outcome)
	}
	for _, node := range result.Routines[0].Nodes {
		if node.Metrics.Hits != 0 {
			t.Fatalf("node %d keeps warm-up hits %d", node.ID, node.Metrics.Hits)
		}
	}
}

func TestLineProfileWarmUpBeginFailureContinuesTheRun(t *testing.T) {
	f := newLineProfileWarmUpFake()
	f.beginErr = errors.New("LP warm-up begin failure")
	result, outcomeErr, err := f.run(t, context.Background(), LineProfileOptions{WarmUp: true})
	if outcomeErr != nil || err != nil {
		t.Fatalf("run = %v, %v; want the measured run to go on", outcomeErr, err)
	}
	if result.WarmUpErr == nil || !strings.Contains(result.WarmUpErr.Error(), "LP warm-up begin failure") {
		t.Fatalf("WarmUpErr = %v", result.WarmUpErr)
	}
	if len(f.counts) != 1 || !f.counts[0].rolledBack || result.Rows != 2 || !result.RolledBack {
		t.Fatalf("transactions %d, rows %d, rolled back %v", len(f.counts), result.Rows, result.RolledBack)
	}
}
