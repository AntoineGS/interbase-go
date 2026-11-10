package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func newDebugEvalFakeRun(t *testing.T, body string, rows func() (bool, error)) (*DebugSession, *lpRun, *lineProfileFakeCounts) {
	t.Helper()
	return newDebugFakeRunFromRun(t, func() (*lpRun, *lineProfileFakeCounts) {
		run, counts := newLineProfileFakeRun(t, context.Background(), body, rows, func(string) (int64, error) { return 0, nil })
		run.plan.entry.source = body
		run.dbNone = true
		return run, counts
	})
}

func TestDebugEvaluateLocalAndServer(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nDECLARE VARIABLE S VARCHAR(20);\nBEGIN\n" +
		"  X = 4;\n  S = 'alpha';\n  X = X + 1;\nEND"
	s, _, counts := newDebugEvalFakeRun(t, body, func() (bool, error) { return true, nil })
	t.Cleanup(func() { s.Stop() })

	if got := nextDebugStop(t, s).Frames[0].Line; got != 4 {
		t.Fatalf("entry line = %d, want 4", got)
	}
	if got := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver}).Frames[0].Line; got != 5 {
		t.Fatalf("second stop line = %d, want 5", got)
	}
	if got := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver}).Frames[0].Line; got != 6 {
		t.Fatalf("third stop line = %d, want 6", got)
	}

	var query string
	var args []argument
	counts.onQueryArgs = func(text string, got []argument) {
		if strings.Contains(text, "UPPER") {
			query = text
			args = append([]argument(nil), got...)
		}
	}
	counts.valueFor = func(text string) driver.Value {
		if strings.Contains(text, "UPPER") {
			return "ALPHA"
		}
		return int64(1)
	}
	counts.outputsFor = func(text string) []OutputDescriptor {
		if strings.Contains(text, "UPPER") {
			return []OutputDescriptor{{Name: "RESULT", Kind: "VARCHAR", Length: 20, Nullable: true}}
		}
		return nil
	}
	preparedBefore := counts.prepared
	evaluations, err := s.Evaluate(context.Background(), 0, []string{" X + 1 ", "S", "UPPER(:S)"})
	if err != nil {
		t.Fatalf("Evaluate() error = %v", err)
	}
	if len(evaluations) != 3 {
		t.Fatalf("Evaluate() returned %d results, want 3", len(evaluations))
	}
	if got := evaluations[0]; got.Err != nil || got.Expression != "X + 1" || got.Type != "INTEGER" || got.Value != int64(5) {
		t.Fatalf("local evaluation = %+v, want X + 1 = INTEGER 5", got)
	}
	if got := evaluations[1]; got.Err != nil || got.Expression != "S" || got.Type != "VARCHAR" || got.Value != "alpha" {
		t.Fatalf("local string evaluation = %+v, want S = VARCHAR alpha", got)
	}
	if got := evaluations[2]; got.Err != nil || got.Expression != "UPPER(:S)" || got.Type != "VARCHAR(20)" || got.Value != "ALPHA" {
		t.Fatalf("server evaluation = %+v, want UPPER(:S) = VARCHAR(20) ALPHA", got)
	}
	if counts.prepared != preparedBefore+1 {
		t.Fatalf("Evaluate() prepared %d statements, want only the one server expression", counts.prepared-preparedBefore)
	}
	if query != "SELECT (UPPER(?)) FROM RDB$DATABASE" {
		t.Fatalf("server evaluation query = %q", query)
	}
	if len(args) != 1 || args[0].kind != argumentString || args[0].stringValue != "alpha" {
		t.Fatalf("server evaluation arguments = %+v, want S bound to alpha", args)
	}
	if got := s.Latest().Status; got != DebugPaused {
		t.Fatalf("session status after evaluation = %q, want paused", got)
	}
}

func TestDebugEvaluateRefusals(t *testing.T) {
	s, _, counts := newDebugEvalFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 1;\nEND", func() (bool, error) {
		return true, nil
	})
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)

	expressions := []string{"", "   ", "1; SELECT 2", "X + :Z"}
	got, err := s.Evaluate(context.Background(), 0, expressions)
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want per-expression errors", err)
	}
	if len(got) != len(expressions) {
		t.Fatalf("Evaluate() returned %d results, want %d", len(got), len(expressions))
	}
	for i, result := range got {
		if result.Err == nil || result.Type != "" || result.Value != nil {
			t.Errorf("evaluation %q = %+v, want a refusal error", expressions[i], result)
		}
	}
	if counts.prepared != 0 {
		t.Fatalf("refused expressions prepared %d statements, want none", counts.prepared)
	}
	if got := s.Latest().Status; got != DebugPaused {
		t.Fatalf("session status after refusals = %q, want paused", got)
	}
}

func TestDebugEvaluateSelectedFrame(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 9;\n" +
		"  EXECUTE PROCEDURE P_CALLEE;\n  X = 10;\nEND"
	callee := lineProfileTestProc{name: "P_CALLEE", body: "DECLARE VARIABLE K INTEGER;\nBEGIN\n  K = 1;\nEND"}
	s, _ := newDebugFakeStepRun(t, body, []lineProfileTestProc{callee}, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	if got := nextDebugStop(t, s).Frames[0].Line; got != 3 {
		t.Fatalf("entry line = %d, want 3", got)
	}
	if got := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver}).Frames[0].Line; got != 4 {
		t.Fatalf("call line = %d, want 4", got)
	}
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepInto})
	if len(snap.Frames) != 2 || snap.Frames[0].Routine != "P_CALLEE" || snap.Frames[1].Routine != "LP_FAKE" {
		t.Fatalf("step-into frames = %+v, want callee and caller", snap.Frames)
	}
	got, err := s.Evaluate(context.Background(), 1, []string{"X + 1"})
	if err != nil {
		t.Fatalf("Evaluate(caller frame) error = %v", err)
	}
	if len(got) != 1 || got[0].Err != nil || got[0].Type != "INTEGER" || got[0].Value != int64(10) {
		t.Fatalf("caller-frame evaluation = %+v, want INTEGER 10", got)
	}
}

func TestDebugEvaluateNotPaused(t *testing.T) {
	s := newDebugSession(context.Background(), DebugAnalysis{}, DebugOptions{})
	defer s.cancel()
	if _, err := s.Evaluate(context.Background(), 0, []string{"1"}); !errors.Is(err, ErrDebugNotPaused) {
		t.Fatalf("Evaluate() error = %v, want %v", err, ErrDebugNotPaused)
	}
}

func TestDebugEvaluateReadOnlyErrorIsPerExpression(t *testing.T) {
	body := "DECLARE VARIABLE S VARCHAR(20);\nBEGIN\n  S = 'alpha';\n  S = 'beta';\nEND"
	s, _, counts := newDebugEvalFakeRun(t, body, func() (bool, error) { return true, nil })
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	_ = sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
	counts.queryErr = func(query string) error {
		if strings.Contains(query, "UPPER") {
			return &Error{Operation: "fake evaluation", SQLCode: -817, NativeCode: debugReadOnlyTransactionGDSCode,
				Message: "synthetic read-only transaction", gdsCodes: []int64{debugReadOnlyTransactionGDSCode}}
		}
		return nil
	}
	got, err := s.Evaluate(context.Background(), 0, []string{"UPPER(:S)"})
	if err != nil {
		t.Fatalf("Evaluate() error = %v, want per-expression error", err)
	}
	if len(got) != 1 || got[0].Err == nil || !isDebugReadOnlyTransactionError(got[0].Err) {
		t.Fatalf("read-only evaluation = %+v, want isc_read_only_trans as the expression error", got)
	}
	snap := s.Latest()
	if snap.Status != DebugPaused || snap.Phase != DebugReadOnly {
		t.Fatalf("session after read-only evaluation error = status %q phase %q, want paused/read-only", snap.Status, snap.Phase)
	}
}

func TestAnalyzeDebugStatementLines(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  IF (X = 0) THEN\n  BEGIN\n    X = 1;\n  END\n" +
		"  WHILE (X < 3) DO\n  BEGIN\n    X = X + 1;\n  END\n  X = 4;\nEND"
	plan := newLineProfileFakePlan(t, body)
	plan.entry.source = body
	analysis := debugAnalysisFromPlan(plan)
	if len(analysis.Routines) != 1 || analysis.Routines[0].Name != "LP_FAKE" {
		t.Fatalf("debug routines = %+v, want entry routine only", analysis.Routines)
	}
	if analysis.Routines[0].Source != body || analysis.ReadWriteFromStart || analysis.ReadWriteReason != "" {
		t.Fatalf("debug analysis entry/source/write analysis = %+v / %v / %q", analysis.Routines[0], analysis.ReadWriteFromStart, analysis.ReadWriteReason)
	}
	want := []int{3, 5, 7, 9, 11}
	if !reflect.DeepEqual(analysis.Routines[0].StatementLines, want) {
		t.Fatalf("statement lines = %v, want %v", analysis.Routines[0].StatementLines, want)
	}

	writeBody := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    UPDATE LP_T SET ID = X;\nEND"
	writePlan := newLineProfileFakePlan(t, writeBody)
	writePlan.entry.source = writeBody
	writeAnalysis := debugAnalysisFromPlan(writePlan)
	if !writeAnalysis.ReadWriteFromStart || !strings.Contains(writeAnalysis.ReadWriteReason, "line 3 of LP_FAKE") {
		t.Fatalf("debug write analysis = %v / %q, want read-write from start at line 3", writeAnalysis.ReadWriteFromStart, writeAnalysis.ReadWriteReason)
	}
}

func TestDebugAnalysisIncludesEntryWithoutParsedBody(t *testing.T) {
	meta := &lpRoutineMeta{name: "LP_FAKE", source: "EXTERNAL BODY"}
	plan := &lpPlan{entry: meta, procs: map[string]*lpRoutineMeta{meta.name: meta}}
	analysis := debugAnalysisFromPlan(plan)
	if len(analysis.Routines) != 1 || analysis.Routines[0].Name != meta.name || analysis.Routines[0].Source != meta.source {
		t.Fatalf("debug routines = %+v, want entry source even without parsed body", analysis.Routines)
	}
	if len(analysis.Routines[0].StatementLines) != 0 {
		t.Fatalf("unparsed entry statement lines = %v, want none", analysis.Routines[0].StatementLines)
	}
}
