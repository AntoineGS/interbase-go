package interbase

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"interbase-go/psql"
)

// newDebugFakeRun builds a debug session over the existing line-profile fake
// transaction and starts it at the entry hook.
func newDebugFakeRun(t *testing.T, body string, rows func() (bool, error)) (*DebugSession, *lpRun, *lineProfileFakeCounts) {
	t.Helper()
	s, run, counts := newDebugFakeRunFromRun(t, func() (*lpRun, *lineProfileFakeCounts) {
		run, counts := newLineProfileFakeRun(t, context.Background(), body, rows, func(string) (int64, error) { return 0, nil })
		run.plan.entry.source = body
		return run, counts
	})
	return s, run, counts
}

func newDebugFakeStepRun(t *testing.T, body string, procs []lineProfileTestProc, rows func() (bool, error)) (*DebugSession, *lpRun) {
	t.Helper()
	var run *lpRun
	var savepoints *[]string
	run, savepoints = newLineProfileStepRunRows(t, context.Background(), body, procs, nil, rows)
	_ = savepoints
	run.plan.entry.source = body
	s, _, _ := newDebugFakeRunFromRun(t, func() (*lpRun, *lineProfileFakeCounts) { return run, nil })
	return s, run
}

func newDebugFakeRunFromRun(t *testing.T, makeRun func() (*lpRun, *lineProfileFakeCounts)) (*DebugSession, *lpRun, *lineProfileFakeCounts) {
	t.Helper()
	run, counts := makeRun()
	if run.plan.entry.source == "" {
		run.plan.entry.source = "BEGIN END"
	}
	markDebugFakeReadOnlyCallees(run.plan)
	analysis := debugAnalysisFromPlan(run.plan)
	initial := TransactionOptions{ReadOnly: !analysis.ReadWriteFromStart,
		Isolation: sql.LevelReadCommitted, NoWait: analysis.ReadWriteFromStart}
	if counts != nil {
		execute := func(query string, _ TransactionOptions) (int64, error) {
			if counts.execute == nil {
				return 0, nil
			}
			return counts.execute(query)
		}
		_, tx := beginDebugFakeTransaction(t, run, counts, initial, counts.next, execute)
		run.tx = tx
	}
	run.opts.ReadOnly = initial.ReadOnly
	s := newDebugSession(context.Background(), analysis, DebugOptions{KeepRows: 200, AllowWrites: true})
	run.opts.KeepRows = 200
	s.attachRun(run)
	go func() {
		outcome, txErr := run.runInTransaction()
		s.finishRun(run, outcome, txErr)
	}()
	return s, run, counts
}

func nextDebugStop(t *testing.T, s *DebugSession) DebugSnapshot {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case snap, ok := <-s.Snapshots():
			if !ok {
				t.Fatal("snapshot channel closed before an ended snapshot")
			}
			if snap.Status == DebugPaused || snap.Status == DebugEndedStatus {
				return snap
			}
		case <-deadline:
			t.Fatal("timed out waiting for a debug stop")
		}
	}
}

func sendDebugCommand(t *testing.T, s *DebugSession, cmd DebugCommand) DebugSnapshot {
	t.Helper()
	if err := s.Command(cmd); err != nil {
		t.Fatalf("Command(%+v): %v", cmd, err)
	}
	return nextDebugStop(t, s)
}

func stopDebugSession(t *testing.T, s *DebugSession) DebugSnapshot {
	t.Helper()
	s.Stop()
	return s.Latest()
}

func TestDebugStopsOnEntry(t *testing.T) {
	s, _, _ := newDebugFakeRun(t, "BEGIN\n  X = 1;\nEND", func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	snap := nextDebugStop(t, s)
	if snap.Status != DebugPaused || len(snap.Frames) != 1 || snap.Frames[0].Line != 2 {
		t.Fatalf("first snapshot = %+v, want paused at entry line 2", snap)
	}
}

func TestDebugAnalysisIncludesReachableStatements(t *testing.T) {
	body := "BEGIN\n  EXECUTE PROCEDURE P_CALLEE;\nEND"
	plan := newLineProfileFakePlan(t, body)
	plan.entry.source = body
	calleeBody := "DECLARE VARIABLE K INTEGER;\nBEGIN\n  K = 1;\nEND"
	callee, problems := psql.ParseRoutine(calleeBody, psql.ParseOptions{Dialect: 3})
	if len(problems) != 0 {
		t.Fatalf("callee parse problems: %+v", problems)
	}
	plan.procs["P_CALLEE"] = &lpRoutineMeta{name: "P_CALLEE", source: calleeBody, routine: callee}
	analysis := debugAnalysisFromPlan(plan)
	if len(analysis.Routines) != 2 || analysis.Routines[0].Name != "LP_FAKE" || analysis.Routines[1].Name != "P_CALLEE" {
		t.Fatalf("debug routines = %+v", analysis.Routines)
	}
	if !reflect.DeepEqual(analysis.Routines[0].StatementLines, []int{2}) || !reflect.DeepEqual(analysis.Routines[1].StatementLines, []int{3}) {
		t.Fatalf("statement lines = %+v", analysis.Routines)
	}
}

func TestDebugStepOverSkipsCalleeAndBlocks(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  IF (1 = 1) THEN\n  BEGIN\n    X = 1;\n    X = 2;\n  END\n" +
		"  X = 3;\nEND"
	s, _, _ := newDebugFakeRun(t, body, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	if got := nextDebugStop(t, s).Frames[0].Line; got != 3 {
		t.Fatalf("entry line %d, want 3", got)
	}
	for _, want := range []int{5, 6, 8} {
		snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
		if got := snap.Frames[0].Line; got != want {
			t.Fatalf("step-over line %d, want %d", got, want)
		}
	}

	proc := lineProfileTestProc{name: "P_CALLEE", body: "DECLARE VARIABLE K INTEGER;\nBEGIN\n  K = 1;\nEND"}
	s2, _ := newDebugFakeStepRun(t, "BEGIN\n  EXECUTE PROCEDURE P_CALLEE;\n  X = 2;\nEND", []lineProfileTestProc{proc}, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s2.Stop() })
	_ = nextDebugStop(t, s2)
	snap := sendDebugCommand(t, s2, DebugCommand{Kind: DebugStepOver})
	if len(snap.Frames) != 1 || snap.Frames[0].Routine != "LP_FAKE" || snap.Frames[0].Line != 3 {
		t.Fatalf("step-over from call = %+v", snap.Frames)
	}
}

func TestDebugStepIntoExecuteProcedure(t *testing.T) {
	body := "BEGIN\n  EXECUTE PROCEDURE P_CALLEE;\n  X = 1;\nEND"
	proc := lineProfileTestProc{name: "P_CALLEE", body: "DECLARE VARIABLE K INTEGER;\nBEGIN\n  K = 1;\nEND"}
	s, _ := newDebugFakeStepRun(t, body, []lineProfileTestProc{proc}, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	if got := nextDebugStop(t, s).Frames[0].Line; got != 2 {
		t.Fatalf("entry line %d, want 2", got)
	}
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepInto})
	if len(snap.Frames) != 2 || snap.Frames[0].Routine != "P_CALLEE" || snap.Frames[0].Line != 3 || snap.Frames[1].Line != 2 {
		t.Fatalf("step-into stack = %+v", snap.Frames)
	}
	snap = sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOut})
	if len(snap.Frames) != 1 || snap.Frames[0].Line != 3 {
		t.Fatalf("step-out stack = %+v, want caller line 3", snap.Frames)
	}
}

func TestDebugStepIntoSelectableCallee(t *testing.T) {
	body := "BEGIN\n  FOR SELECT I FROM LP_GEN(3) INTO :X DO\n    X = X + 1;\nEND"
	s, _ := newDebugFakeStepRun(t, body, []lineProfileTestProc{lpGen}, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	if got := nextDebugStop(t, s).Frames[0].Line; got != 2 {
		t.Fatalf("entry line %d, want 2", got)
	}
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepInto})
	if len(snap.Frames) != 2 || snap.Frames[0].Routine != "LP_GEN" || snap.Frames[0].Line != 2 || snap.Frames[1].Line != 2 {
		t.Fatalf("selectable step-into stack = %+v", snap.Frames)
	}
	snap = sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOut})
	if len(snap.Frames) != 1 || snap.Frames[0].Line != 3 {
		t.Fatalf("selectable step-out stack = %+v, want only caller at line 3", snap.Frames)
	}
}

func TestDebugBreakpointsAndContinue(t *testing.T) {
	s, _, _ := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 1;\n  X = 2;\n  X = 3;\nEND", func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	accepted := s.SetBreakpoints([]DebugBreakpoint{{Routine: "LP_FAKE", Line: 5}, {Routine: "LP_FAKE", Line: 99}})
	if !reflect.DeepEqual(accepted, []DebugBreakpoint{{Routine: "LP_FAKE", Line: 5}}) {
		t.Fatalf("accepted breakpoints = %+v", accepted)
	}
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if snap.Status != DebugPaused || snap.Frames[0].Line != 5 {
		t.Fatalf("continue stopped at %+v, want breakpoint line 5", snap)
	}
	snap = sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if snap.Status != DebugEndedStatus || snap.Outcome != DebugCompleted {
		t.Fatalf("continue to end = %+v", snap)
	}
}

func TestDebugRunTo(t *testing.T) {
	s, _, _ := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 1;\n  X = 2;\n  X = 3;\nEND", func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugRunTo, Routine: "LP_FAKE", Line: 5})
	if snap.Frames[0].Line != 5 {
		t.Fatalf("run-to line %d, want 5", snap.Frames[0].Line)
	}

	proc := lineProfileTestProc{name: "P_CALLEE", body: "DECLARE VARIABLE K INTEGER;\nBEGIN\n  K = 1;\nEND"}
	s2, _ := newDebugFakeStepRun(t, "BEGIN\n  EXECUTE PROCEDURE P_CALLEE;\n  X = 2;\nEND", []lineProfileTestProc{proc}, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s2.Stop() })
	_ = nextDebugStop(t, s2)
	snap = sendDebugCommand(t, s2, DebugCommand{Kind: DebugRunTo, Routine: "P_CALLEE", Line: 3})
	if len(snap.Frames) != 2 || snap.Frames[0].Routine != "P_CALLEE" || snap.Frames[0].Line != 3 {
		t.Fatalf("run-to callee = %+v", snap.Frames)
	}
}

func TestDebugWhileStopsPerIteration(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 0;\n  WHILE (X < 3) DO\n    X = X + 1;\nEND"
	s, _, _ := newDebugFakeRun(t, body, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	if got := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver}).Frames[0].Line; got != 4 {
		t.Fatalf("first WHILE stop line %d", got)
	}
	s.SetBreakpoints([]DebugBreakpoint{{Routine: "LP_FAKE", Line: 4}})
	for i := 0; i < 3; i++ {
		snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
		if snap.Status != DebugPaused || snap.Frames[0].Line != 4 {
			t.Fatalf("WHILE stop %d = %+v", i+1, snap)
		}
	}
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if snap.Status != DebugEndedStatus {
		t.Fatalf("session after failing WHILE condition = %+v", snap)
	}
}

func TestDebugVariablesChanged(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nDECLARE VARIABLE Y INTEGER;\nBEGIN\n" +
		"  X = 1;\n  Y = NULL;\n  X = X + 1;\n  Y = Y;\nEND"
	s, _, _ := newDebugFakeRun(t, body, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
	if got := snap.Frames[0].Locals; len(got) != 2 || got[0].Value != int64(1) || !got[0].Changed || got[1].Value != nil || got[1].Changed {
		t.Fatalf("values after first assignment = %+v", got)
	}
	snap = sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
	if got := snap.Frames[0].Locals; got[0].Value != int64(1) || got[0].Changed || got[1].Value != nil || got[1].Changed {
		t.Fatalf("values after NULL assignment = %+v", got)
	}
	snap = sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
	if got := snap.Frames[0].Locals; got[0].Value != int64(2) || !got[0].Changed || got[1].Value != nil || got[1].Changed {
		t.Fatalf("values after increment = %+v", got)
	}
}

func TestDebugCursorInSnapshot(t *testing.T) {
	rows := 0
	s, _, _ := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n"+
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND", func() (bool, error) {
		rows++
		return rows <= 2, nil
	})
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	s.SetBreakpoints([]DebugBreakpoint{{Routine: "LP_FAKE", Line: 4}})
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if snap.Status != DebugPaused || len(snap.Frames) == 0 {
		t.Fatalf("expected pause in cursor body, got %+v", snap)
	}
	if len(snap.Frames[0].Cursors) != 1 {
		t.Fatalf("cursors = %+v", snap.Frames[0].Cursors)
	}
	cursor := snap.Frames[0].Cursors[0]
	if cursor.Line != 3 || cursor.Fetched != 1 || !reflect.DeepEqual(cursor.Targets, []string{"X"}) {
		t.Fatalf("cursor = %+v", cursor)
	}
}

func TestDebugPauseWhileRunning(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	calls := 0
	rows := func() (bool, error) {
		calls++
		if calls == 1 {
			close(entered)
			<-release
		}
		return calls == 1, nil
	}
	s, _, _ := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n"+
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND", rows)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	if err := s.Command(DebugCommand{Kind: DebugContinue}); err != nil {
		t.Fatal(err)
	}
	if err := s.Command(DebugCommand{Kind: DebugStepOver}); !errors.Is(err, ErrDebugNotPaused) {
		t.Fatalf("second command during resume error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fake fetch did not start")
	}
	if err := s.Command(DebugCommand{Kind: DebugPause}); err != nil {
		t.Fatalf("Pause while running: %v", err)
	}
	close(release)
	snap := nextDebugStop(t, s)
	if snap.Status != DebugPaused || snap.Frames[0].Line != 4 {
		t.Fatalf("pause stop = %+v", snap)
	}
}

func TestDebugResumeSnapshotRetainsCumulativeState(t *testing.T) {
	entered := make(chan struct{})
	var run *lpRun
	rows := func() (bool, error) {
		close(entered)
		<-run.ctx.Done()
		return false, run.ctx.Err()
	}
	s, run, _ := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n"+
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND", rows)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	s.mu.Lock()
	s.debug.messages = append(s.debug.messages, DebugMessage{Kind: "info", Text: "cumulative message"})
	s.debug.statements = 9
	run.result.Columns = []string{"ID"}
	run.result.KeptRows = [][]Cell{{{Value: int64(11)}}}
	run.result.Rows = 12
	s.mu.Unlock()
	if err := s.Command(DebugCommand{Kind: DebugContinue}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fake fetch did not start")
	}

	var running DebugSnapshot
	select {
	case running = <-s.Snapshots():
		if running.Status != DebugRunning {
			t.Fatalf("snapshot after resume = %+v, want running", running)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for running snapshot")
	}
	latest := s.Latest()
	for _, snapshot := range []DebugSnapshot{running, latest} {
		if snapshot.Status != DebugRunning || len(snapshot.Frames) != 0 || snapshot.Outcome != "" || snapshot.Err != nil || snapshot.FailedAt != nil {
			t.Fatalf("running snapshot retained stop-specific state: %+v", snapshot)
		}
		if snapshot.Statements != 9 || snapshot.RowsTotal != 12 || !reflect.DeepEqual(snapshot.Columns, []string{"ID"}) || !reflect.DeepEqual(snapshot.Rows, [][]Cell{{{Value: int64(11)}}}) {
			t.Fatalf("running snapshot lost cumulative results: %+v", snapshot)
		}
		if len(snapshot.Messages) == 0 || snapshot.Messages[len(snapshot.Messages)-1].Text != "cumulative message" {
			t.Fatalf("running snapshot lost cumulative messages: %+v", snapshot.Messages)
		}
	}
}

func TestDebugStopDuringServerCall(t *testing.T) {
	entered := make(chan struct{})
	var run *lpRun
	rows := func() (bool, error) {
		close(entered)
		<-run.ctx.Done()
		return false, run.ctx.Err()
	}
	s, run, counts := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n"+
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND", rows)
	_ = nextDebugStop(t, s)
	if err := s.Command(DebugCommand{Kind: DebugContinue}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fake fetch did not start")
	}
	snap := stopDebugSession(t, s)
	if snap.Status != DebugEndedStatus || snap.Outcome != DebugStopped || counts.rolledBack || !debugFakeCommitted(counts) {
		t.Fatalf("stopped snapshot/result = %+v, rolledBack=%v counts=%+v", snap, run.result.RolledBack, counts)
	}
	if counts.closed != counts.prepared || counts.cursorsClosed != counts.queried ||
		len(run.statements) != 0 || len(run.tx.cursors) != 0 || len(run.tx.prepared) != 0 {
		t.Fatalf("stopped read-only debug run leaked resources: counts=%+v statements=%d cursors=%d prepared=%d",
			counts, len(run.statements), len(run.tx.cursors), len(run.tx.prepared))
	}
}

func TestDebugStopWithFullSnapshotBuffer(t *testing.T) {
	s, run, counts := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 1;\nEND", func() (bool, error) { return false, nil })
	_ = nextDebugStop(t, s)
	for len(s.snapshots) < cap(s.snapshots) {
		s.snapshots <- DebugSnapshot{Status: DebugRunning}
	}
	if err := s.Command(DebugCommand{Kind: DebugContinue}); err != nil {
		t.Fatal(err)
	}

	stopDone := make(chan struct{})
	go func() {
		s.Stop()
		close(stopDone)
	}()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		// Drain only after observing the deadlock, to unblock the old
		// unconditional intermediate-snapshot send.
		drained := make(chan struct{})
		go func() {
			for range s.Snapshots() {
			}
			close(drained)
		}()
		select {
		case <-stopDone:
			<-drained
		case <-time.After(3 * time.Second):
			t.Fatal("Stop remained blocked while draining the full snapshot buffer")
		}
		t.Fatal("Stop blocked with a full snapshot buffer and no reader")
	}
	snap := s.Latest()
	if snap.Status != DebugEndedStatus || snap.Outcome != DebugStopped || counts.rolledBack || !debugFakeCommitted(counts) {
		t.Fatalf("stopped snapshot/result = %+v, rolledBack=%v counts=%+v", snap, run.result.RolledBack, counts)
	}
	if len(run.statements) != 0 || len(run.tx.cursors) != 0 || len(run.tx.prepared) != 0 {
		t.Fatalf("stopped run retains statements %d, cursors %d, prepared %d", len(run.statements), len(run.tx.cursors), len(run.tx.prepared))
	}
	for range s.Snapshots() {
	}
}

func TestDebugCommandErrors(t *testing.T) {
	entered := make(chan struct{})
	var run *lpRun
	rows := func() (bool, error) {
		close(entered)
		<-run.ctx.Done()
		return false, run.ctx.Err()
	}
	s, run, _ := newDebugFakeRun(t, "DECLARE VARIABLE X INTEGER;\nBEGIN\n"+
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND", rows)
	_ = nextDebugStop(t, s)
	if err := s.Command(DebugCommand{Kind: DebugContinue}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("fake fetch did not start")
	}
	if err := s.Command(DebugCommand{Kind: DebugStepOver}); !errors.Is(err, ErrDebugNotPaused) {
		t.Fatalf("command while running error = %v", err)
	}
	s.Stop()
	<-s.Done()
	if err := s.Command(DebugCommand{Kind: DebugContinue}); !errors.Is(err, ErrDebugEnded) {
		t.Fatalf("command after end error = %v", err)
	}
}

func TestDebugEvaluateRejectsResumeAdmissionInProgress(t *testing.T) {
	s := newDebugSession(context.Background(), DebugAnalysis{}, DebugOptions{})
	s.mu.Lock()
	s.latest.Status = DebugPaused
	s.mu.Unlock()

	commandResult := make(chan error, 1)
	go func() {
		commandResult <- s.Command(DebugCommand{Kind: DebugContinue})
	}()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		commandSent := s.commandSent
		s.mu.Unlock()
		if commandSent {
			break
		}
		select {
		case <-deadline:
			s.cancel()
			t.Fatal("Continue did not enter command admission")
		case <-ticker.C:
		}
	}

	evalCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	_, evalErr := s.Evaluate(evalCtx, 0, []string{"1"})
	cancel()
	s.cancel()
	select {
	case err := <-commandResult:
		if !errors.Is(err, ErrDebugEnded) {
			t.Fatalf("concurrent Continue error = %v, want ended after cancellation", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Continue did not return after session cancellation")
	}
	if !errors.Is(evalErr, ErrDebugNotPaused) {
		t.Fatalf("Evaluate during Continue admission error = %v, want %v", evalErr, ErrDebugNotPaused)
	}
}

func TestDebugContinueRejectsEvaluationAdmissionInProgress(t *testing.T) {
	s := newDebugSession(context.Background(), DebugAnalysis{}, DebugOptions{})
	s.mu.Lock()
	s.latest.Status = DebugPaused
	s.mu.Unlock()
	evalCtx, cancelEval := context.WithCancel(context.Background())
	evalResult := make(chan error, 1)
	go func() {
		_, err := s.Evaluate(evalCtx, 0, []string{"1"})
		evalResult <- err
	}()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		s.mu.Lock()
		evaluationSent := s.evaluationSent
		s.mu.Unlock()
		if evaluationSent {
			break
		}
		select {
		case <-deadline:
			cancelEval()
			s.cancel()
			t.Fatal("Evaluate did not enter request admission")
		case <-ticker.C:
		}
	}

	commandResult := make(chan error, 1)
	go func() {
		commandResult <- s.Command(DebugCommand{Kind: DebugContinue})
	}()
	var commandErr error
	select {
	case commandErr = <-commandResult:
	case <-time.After(100 * time.Millisecond):
		cancelEval()
		s.cancel()
		<-commandResult
		<-evalResult
		t.Fatal("Continue was admitted while evaluation was already in progress")
	}
	cancelEval()
	s.cancel()
	if !errors.Is(commandErr, ErrDebugNotPaused) {
		t.Fatalf("Continue during Evaluate admission error = %v, want %v", commandErr, ErrDebugNotPaused)
	}
	if err := <-evalResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled Evaluate error = %v, want context canceled", err)
	}
}

func TestLineProfileUnaffectedByDebugHook(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  BEGIN\n    UPDATE LP_T SET A = 1;\n  WHEN ANY DO X = 0;\n  END\nEND"
	makeRun := func() (*lpRun, *[]string) {
		return newLineProfileHandlerRun(t, context.Background(), body)
	}
	first, firstSavepoints := makeRun()
	second, secondSavepoints := makeRun()
	if first.hook != nil || second.hook != nil || first.noMeasure || second.noMeasure {
		t.Fatal("ordinary line-profile runs must keep debug hooks and noMeasure disabled")
	}
	for i, run := range []*lpRun{first, second} {
		outcome, txErr := run.runInTransaction()
		if outcome != nil || txErr != nil {
			t.Fatalf("run %d: %v / %v", i, outcome, txErr)
		}
	}
	hits := func(nodes []LineProfileNode) []int64 {
		out := make([]int64, len(nodes))
		for i := range nodes {
			out[i] = nodes[i].Metrics.Hits
		}
		return out
	}
	if !reflect.DeepEqual(hits(first.result.Routines[0].Nodes), hits(second.result.Routines[0].Nodes)) || !reflect.DeepEqual(*firstSavepoints, *secondSavepoints) || len(*firstSavepoints) == 0 {
		t.Fatal("LineProfile node hits or savepoint sequence changed with a nil debug hook")
	}
}

func TestDebugHookSkipsBlocksAndEmptyStatements(t *testing.T) {
	s, _, _ := newDebugFakeRun(t, "BEGIN\n  -- comment\n  X = 1;\nEND", func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	snap := nextDebugStop(t, s)
	if snap.Frames[0].Kind == string(psql.NodeBlock) || snap.Frames[0].Kind == string(psql.NodeEmpty) || snap.Frames[0].Line != 3 {
		t.Fatalf("entry stop = %+v", snap.Frames[0])
	}
}

func TestDebugNativeStepIntoReportsMessage(t *testing.T) {
	proc := lineProfileTestProc{name: "P_NATIVE", body: "BEGIN\n  EXIT;\nEND"}
	s, run := newDebugFakeStepRun(t, "BEGIN\n  EXECUTE PROCEDURE P_NATIVE;\n  X = 1;\nEND", []lineProfileTestProc{proc}, func() (bool, error) { return false, nil })
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	delete(run.stepInto, "P_NATIVE")
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepInto})
	if len(snap.Frames) != 1 || snap.Frames[0].Line != 3 || len(snap.Messages) == 0 || !strings.Contains(snap.Messages[len(snap.Messages)-1].Text, "P_NATIVE ran on the server") {
		t.Fatalf("native step-into stop = %+v", snap)
	}
}
