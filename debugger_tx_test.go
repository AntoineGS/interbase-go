package interbase

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"interbase-go/psql"
)

type debugFakeTx struct {
	options   TransactionOptions
	commits   int
	rollbacks int
}

type debugTxEvent struct {
	tx     int
	action string
	query  string
}

type debugTxRecorder struct {
	mu     sync.Mutex
	txs    []debugFakeTx
	events []debugTxEvent
}

func (r *debugTxRecorder) begin(options TransactionOptions) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := len(r.txs)
	r.txs = append(r.txs, debugFakeTx{options: options})
	r.events = append(r.events, debugTxEvent{tx: id, action: "begin"})
	return id
}

func (r *debugTxRecorder) record(event debugTxEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	if event.action == "commit" {
		r.txs[event.tx].commits++
	}
	if event.action == "rollback" {
		r.txs[event.tx].rollbacks++
	}
}

func (r *debugTxRecorder) snapshot() ([]debugFakeTx, []debugTxEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]debugFakeTx(nil), r.txs...), append([]debugTxEvent(nil), r.events...)
}

func debugFakeOptionsFromTPB(tpb []byte) TransactionOptions {
	options := TransactionOptions{Isolation: sql.LevelReadCommitted}
	if len(tpb) > 1 {
		options.ReadOnly = tpb[1] == 8
	}
	for _, item := range tpb[2:] {
		if item == 7 {
			options.NoWait = true
		}
	}
	return options
}

func beginDebugFakeTransaction(t *testing.T, run *lpRun, counts *lineProfileFakeCounts,
	options TransactionOptions, rows func() (bool, error), execute func(string, TransactionOptions) (int64, error)) (*debugTxRecorder, *Transaction) {
	t.Helper()
	recorder := &debugTxRecorder{}
	attachment := run.tx.attachment
	attachment.directTx = nil // Replace the construction-only fake with a real direct-API transaction.
	run.conn.native.beginTransactionOverride = func(tpb []byte) (*nativeTransaction, error) {
		actual := debugFakeOptionsFromTPB(tpb)
		txID := recorder.begin(actual)
		counts.onQuery = func(query string) { recorder.record(debugTxEvent{tx: txID, action: "query", query: query}) }
		exec := func(query string) (int64, error) {
			recorder.record(debugTxEvent{tx: txID, action: "exec", query: query})
			if execute != nil {
				return execute(query, actual)
			}
			return 0, nil
		}
		native := newLineProfileFakeNative(counts, rows, exec)
		// Direct Transaction.Exec (used for savepoints) bypasses prepared
		// statement fakes and calls nativeTransaction.exec.
		native.execOverride = func(query string, _ []argument, _ bool) (int64, error) {
			return exec(query)
		}
		commit := native.commitOverride
		native.commitOverride = func() error {
			recorder.record(debugTxEvent{tx: txID, action: "commit"})
			return commit()
		}
		rollback := native.rollbackOverride
		native.rollbackOverride = func() error {
			recorder.record(debugTxEvent{tx: txID, action: "rollback"})
			return rollback()
		}
		return native, nil
	}
	tx, err := attachment.BeginTx(context.Background(), options)
	if err != nil {
		t.Fatalf("BeginTx(%+v): %v", options, err)
	}
	return recorder, tx
}

func debugFakeCommitted(counts *lineProfileFakeCounts) bool { return counts.committed }

func markDebugFakeReadOnlyCallees(plan *lpPlan) {
	for name, meta := range plan.procs {
		if name != plan.entry.name && meta.routine != nil && !meta.routine.MayWrite() {
			meta.readsOnly, meta.readsChecked = true, true
		}
	}
}

type debugTxProcedure struct {
	name      string
	body      string
	inputs    []lpParam
	outputs   []lpParam
	readsOnly bool
}

func newDebugTxRun(t *testing.T, body string, procedures []debugTxProcedure,
	rows func() (bool, error), execute func(string, TransactionOptions) (int64, error), forceLazy bool,
	queryErr ...func(string) error) (*DebugSession, *lpRun, *debugTxRecorder, *lineProfileFakeCounts) {
	t.Helper()
	run, counts := newLineProfileFakeRun(t, context.Background(), body, rows, func(string) (int64, error) { return 0, nil })
	if len(queryErr) > 0 {
		counts.queryErr = queryErr[0]
	}
	run.plan.entry.source = body
	for _, procedure := range procedures {
		routine, problems := psql.ParseRoutine(procedure.body, psql.ParseOptions{Dialect: 3})
		if len(problems) != 0 {
			t.Fatalf("%s parse problems: %+v", procedure.name, problems)
		}
		meta := &lpRoutineMeta{name: procedure.name, source: procedure.body, routine: routine,
			inputs: procedure.inputs, outputs: procedure.outputs, readsOnly: procedure.readsOnly, readsChecked: procedure.readsOnly}
		run.plan.procs[procedure.name] = meta
	}
	markDebugFakeReadOnlyCallees(run.plan)
	run.plan.analysis.MayWrite = run.plan.entry.routine.MayWrite()
	analysis := debugAnalysisFromPlan(run.plan)
	initial := TransactionOptions{ReadOnly: !analysis.ReadWriteFromStart,
		Isolation: sql.LevelReadCommitted, NoWait: analysis.ReadWriteFromStart}
	if forceLazy {
		initial.ReadOnly, initial.NoWait = true, false
	}
	recorder, tx := beginDebugFakeTransaction(t, run, counts, initial, rows, execute)
	run.tx, run.opts.ReadOnly = tx, initial.ReadOnly
	s := newDebugSession(context.Background(), analysis, DebugOptions{KeepRows: 200, AllowWrites: true})
	if forceLazy {
		s.debug.debugForceLazy = true
		s.debug.phase = DebugReadOnly
	}
	s.attachRun(run)
	go func() {
		outcome, txErr := run.runInTransaction()
		s.finishRun(run, outcome, txErr)
	}()
	return s, run, recorder, counts
}

func debugTxHasEvent(events []debugTxEvent, tx int, action, queryPrefix string) bool {
	for _, event := range events {
		if event.tx == tx && event.action == action && strings.HasPrefix(event.query, queryPrefix) {
			return true
		}
	}
	return false
}

func debugTxEventIndex(events []debugTxEvent, tx int, action, queryPrefix string) int {
	for i, event := range events {
		if event.tx == tx && event.action == action && strings.HasPrefix(event.query, queryPrefix) {
			return i
		}
	}
	return -1
}

func TestDebugStartsReadOnlyAndSwitchesBeforeFirstWrite(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = 1;\n" +
		"  SELECT 5 FROM RDB$DATABASE INTO :X;\n  UPDATE LP_T SET A = :X;\nEND"
	s, _, recorder, _ := newDebugTxRun(t, body, nil, func() (bool, error) { return false, nil }, nil, false)
	t.Cleanup(func() { s.Stop() })
	if snap := nextDebugStop(t, s); snap.Phase != DebugReadOnly {
		t.Fatalf("entry phase = %s, want read-only", snap.Phase)
	}
	if snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver}); snap.Frames[0].Line != 4 || snap.Phase != DebugReadOnly {
		t.Fatalf("stop before SELECT = %+v", snap)
	}
	if snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver}); snap.Frames[0].Line != 5 || snap.Phase != DebugReadOnly {
		t.Fatalf("stop before UPDATE = %+v", snap)
	}
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
	if ended.Status != DebugEndedStatus || ended.Phase != DebugWritten {
		t.Fatalf("after UPDATE = %+v, want ended written", ended)
	}
	txs, events := recorder.snapshot()
	if len(txs) != 2 || !txs[0].options.ReadOnly || txs[0].commits != 1 || txs[0].rollbacks != 0 ||
		txs[1].options.ReadOnly || !txs[1].options.NoWait || txs[1].rollbacks != 1 || txs[1].commits != 0 {
		t.Fatalf("transaction lifecycle = %+v", txs)
	}
	if !debugTxHasEvent(events, 0, "query", "SELECT 5") || !debugTxHasEvent(events, 1, "exec", "UPDATE LP_T") {
		t.Fatalf("statement phases = %+v", events)
	}
}

func TestDebugWriteInsideLoopStartsReadWrite(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    UPDATE LP_T SET A = :X;\nEND"
	s, _, recorder, _ := newDebugTxRun(t, body, nil, func() (bool, error) { return false, nil }, nil, false)
	t.Cleanup(func() { s.Stop() })
	if !s.Analysis().ReadWriteFromStart || !strings.Contains(s.Analysis().ReadWriteReason, "line 3 of LP_FAKE") {
		t.Fatalf("start analysis = %+v, want write-in-loop reason", s.Analysis())
	}
	if snap := nextDebugStop(t, s); snap.Phase != DebugReadWrite {
		t.Fatalf("entry phase = %s, want read-write", snap.Phase)
	}
	txs, _ := recorder.snapshot()
	if len(txs) != 1 || txs[0].options.ReadOnly || !txs[0].options.NoWait {
		t.Fatalf("initial transaction options = %+v, want read-write NO WAIT", txs)
	}
}

func TestDebugCallInsideLoopCountsWholeCallee(t *testing.T) {
	body := "BEGIN\n  FOR SELECT ID FROM LP_T INTO :X DO\n    EXECUTE PROCEDURE P_CALLEE;\nEND"
	callee := debugTxProcedure{name: "P_CALLEE", body: "BEGIN\n  UPDATE LP_T SET A = 1;\nEND"}
	s, _, recorder, _ := newDebugTxRun(t, body, []debugTxProcedure{callee}, func() (bool, error) { return false, nil }, nil, false)
	t.Cleanup(func() { s.Stop() })
	if !s.Analysis().ReadWriteFromStart || !strings.Contains(s.Analysis().ReadWriteReason, "line 2 of LP_FAKE") {
		t.Fatalf("start analysis = %+v, want callee write in loop", s.Analysis())
	}
	if snap := nextDebugStop(t, s); snap.Phase != DebugReadWrite {
		t.Fatalf("entry phase = %s, want read-write", snap.Phase)
	}
	txs, _ := recorder.snapshot()
	if len(txs) != 1 || txs[0].options.ReadOnly || !txs[0].options.NoWait {
		t.Fatalf("initial transaction options = %+v, want read-write NO WAIT", txs)
	}
}

func TestDebugReadOnlyCalleeStaysLazy(t *testing.T) {
	body := "BEGIN\n  FOR SELECT ID FROM LP_T INTO :X DO\n    EXECUTE PROCEDURE P_READ;\nEND"
	callee := debugTxProcedure{name: "P_READ", body: "BEGIN\n  EXIT;\nEND", readsOnly: true}
	s, _, recorder, _ := newDebugTxRun(t, body, []debugTxProcedure{callee}, func() (bool, error) { return false, nil }, nil, false)
	t.Cleanup(func() { s.Stop() })
	if s.Analysis().ReadWriteFromStart {
		t.Fatalf("read-only callee forced read-write start: %+v", s.Analysis())
	}
	if snap := nextDebugStop(t, s); snap.Phase != DebugReadOnly {
		t.Fatalf("entry phase = %s, want read-only", snap.Phase)
	}
	txs, _ := recorder.snapshot()
	if len(txs) != 1 || !txs[0].options.ReadOnly {
		t.Fatalf("initial transaction options = %+v, want read-only", txs)
	}
}

func TestDebugLazySwitchRecreatesSavepoints(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  BEGIN\n    X = 1;\n" +
		"    UPDATE LP_T SET A = 1;\n  WHEN ANY DO\n    X = 2;\n  END\nEND"
	failed := false
	execute := func(query string, _ TransactionOptions) (int64, error) {
		if strings.HasPrefix(strings.ToUpper(query), "UPDATE") && !failed {
			failed = true
			return 0, &Error{Operation: "fake update", SQLCode: -803, NativeCode: 335544345,
				Message: "synthetic conflict", gdsCodes: []int64{335544345}}
		}
		return 1, nil
	}
	s, run, recorder, _ := newDebugTxRun(t, body, nil, func() (bool, error) { return false, nil }, execute, false)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugCompleted {
		t.Fatalf("guarded block ended = %+v", ended)
	}
	txs, events := recorder.snapshot()
	if len(txs) != 2 {
		t.Fatalf("transactions = %+v", txs)
	}
	for _, event := range events {
		if event.tx == 0 && event.action == "exec" && strings.HasPrefix(event.query, "SAVEPOINT") {
			t.Fatalf("pending savepoint reached read-only transaction: %+v", events)
		}
	}
	create := debugTxEventIndex(events, 1, "exec", "SAVEPOINT LP$SP_0_1")
	update := debugTxEventIndex(events, 1, "exec", "UPDATE LP_T")
	// A caught direct DML error keeps earlier block work under InterBase's
	// verified semantics, so the handler releases the recreated savepoint.
	release := debugTxEventIndex(events, 1, "exec", "RELEASE SAVEPOINT LP$SP_0_1")
	if create < 0 || update <= create || release <= update {
		t.Fatalf("re-created savepoint sequence = %+v", events)
	}
	if lineProfileRoutineHits(run.result.Routines[0], 7) == 0 {
		t.Fatalf("WHEN ANY handler did not run; node hits = %+v", run.result.Routines[0].Nodes)
	}
}

func TestDebugPendingRollbackDropsNestedSavepoints(t *testing.T) {
	s := newDebugSession(context.Background(), DebugAnalysis{}, DebugOptions{})
	run := &lpRun{debug: s.debug}
	for _, name := range []string{"LP$SP_0_1", "LP$SP_1_1", "LP$SP_0_2"} {
		s.debug.addPendingSavepoint(name)
	}
	if err := run.savepoint("ROLLBACK TO SAVEPOINT LP$SP_0_1"); err != nil {
		t.Fatalf("rollback pending savepoint: %v", err)
	}
	s.mu.Lock()
	got := append([]string(nil), s.debug.pending...)
	s.mu.Unlock()
	want := []string{"LP$SP_0_1"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("pending savepoints after rollback = %v, want %v", got, want)
	}
}

func TestDebugSafetyNetMixedTransactions(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    UPDATE LP_T SET A = :X;\nEND"
	rows := 0
	s, _, recorder, _ := newDebugTxRun(t, body, nil, func() (bool, error) {
		rows++
		return rows <= 2, nil
	}, nil, true)
	t.Cleanup(func() { s.Stop() })
	if snap := nextDebugStop(t, s); snap.Phase != DebugReadOnly {
		t.Fatalf("forced-lazy entry phase = %s", snap.Phase)
	}
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Mixed == "" ||
		!strings.Contains(ended.Mixed, "line 4") || !strings.Contains(ended.Mixed, "FOR SELECT at line 3") {
		t.Fatalf("mixed transaction ended snapshot = %+v", ended)
	}
	if !hasDebugMessage(ended.Messages, "Mixed transactions from line 4:") {
		t.Fatalf("missing persistent mixed warning: %+v", ended.Messages)
	}
	txs, events := recorder.snapshot()
	if len(txs) != 2 || !txs[0].options.ReadOnly || txs[0].commits != 1 ||
		txs[1].options.ReadOnly || !txs[1].options.NoWait || txs[1].rollbacks != 1 {
		t.Fatalf("mixed transaction lifecycle = %+v", txs)
	}
	commitRO := debugTxEventIndex(events, 0, "commit", "")
	rollbackRW := debugTxEventIndex(events, 1, "rollback", "")
	if commitRO < 0 || rollbackRW <= commitRO {
		t.Fatalf("mixed transaction completion order = %+v", events)
	}
}

func hasDebugMessage(messages []DebugMessage, prefix string) bool {
	for _, message := range messages {
		if strings.HasPrefix(message.Text, prefix) {
			return true
		}
	}
	return false
}

func TestDebugReadOnlyErrorRetriesOnce(t *testing.T) {
	body := "BEGIN\n  EXECUTE PROCEDURE P_READ;\nEND"
	callee := debugTxProcedure{name: "P_READ", body: "BEGIN\n  EXIT;\nEND", readsOnly: true}
	attempts := 0
	execute := func(query string, _ TransactionOptions) (int64, error) {
		if strings.HasPrefix(strings.ToUpper(query), "EXECUTE PROCEDURE P_READ") && attempts == 0 {
			attempts++
			return 0, &Error{Operation: "fake execute", SQLCode: -817, NativeCode: 335544361,
				Message: "read-only transaction", gdsCodes: []int64{335544361}}
		}
		if strings.HasPrefix(strings.ToUpper(query), "EXECUTE PROCEDURE P_READ") {
			attempts++
		}
		return 0, nil
	}
	s, _, recorder, _ := newDebugTxRun(t, body, []debugTxProcedure{callee}, func() (bool, error) { return false, nil }, execute, false)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugCompleted || attempts != 2 {
		t.Fatalf("read-only retry result = %+v, attempts=%d", ended, attempts)
	}
	txs, events := recorder.snapshot()
	if len(txs) != 2 || !txs[0].options.ReadOnly || txs[0].commits != 1 ||
		txs[1].options.ReadOnly || !txs[1].options.NoWait || txs[1].rollbacks != 1 {
		t.Fatalf("retry transaction lifecycle = %+v", txs)
	}
	if debugTxEventIndex(events, 0, "exec", "EXECUTE PROCEDURE P_READ") < 0 ||
		debugTxEventIndex(events, 1, "exec", "EXECUTE PROCEDURE P_READ") < 0 {
		t.Fatalf("native retry phases = %+v", events)
	}
}

func TestDebugReadOnlyForSelectOpenErrorRetriesBeforeRows(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND"
	queryAttempts := 0
	queryErr := func(query string) error {
		if strings.HasPrefix(strings.ToUpper(query), "SELECT ID FROM LP_T") && queryAttempts == 0 {
			queryAttempts++
			return &Error{Operation: "fake query", SQLCode: -817, NativeCode: 335544361,
				Message: "read-only transaction", gdsCodes: []int64{335544361}}
		}
		return nil
	}
	s, _, recorder, _ := newDebugTxRun(t, body, nil, func() (bool, error) { return false, nil }, nil, false, queryErr)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugCompleted || queryAttempts != 1 {
		t.Fatalf("read-only FOR SELECT open retry = %+v, failed query attempts=%d", ended, queryAttempts)
	}
	txs, events := recorder.snapshot()
	if len(txs) != 2 || !txs[0].options.ReadOnly || txs[0].commits != 1 ||
		txs[1].options.ReadOnly || !txs[1].options.NoWait || txs[1].rollbacks != 1 {
		t.Fatalf("FOR SELECT retry transaction lifecycle = %+v", txs)
	}
	if debugTxEventIndex(events, 0, "query", "SELECT ID FROM LP_T") < 0 ||
		debugTxEventIndex(events, 1, "query", "SELECT ID FROM LP_T") < 0 {
		t.Fatalf("FOR SELECT retry query phases = %+v", events)
	}
}

func TestDebugNoWaitConflictIsCatchable(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  BEGIN\n" +
		"    UPDATE LP_T SET A = 1;\n  WHEN GDSCODE lock_conflict DO\n    X = 2;\n  END\nEND"
	execute := func(query string, _ TransactionOptions) (int64, error) {
		if strings.HasPrefix(strings.ToUpper(query), "UPDATE") {
			return 0, &Error{Operation: "fake update", SQLCode: -913, NativeCode: 335544345,
				Message: "synthetic lock conflict", gdsCodes: []int64{335544345}}
		}
		return 1, nil
	}
	s, run, _, _ := newDebugTxRun(t, body, nil, func() (bool, error) { return false, nil }, execute, false)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugCompleted || ended.Phase != DebugReadWrite {
		t.Fatalf("catchable NO WAIT conflict = %+v", ended)
	}
	if lineProfileRoutineHits(run.result.Routines[0], 6) == 0 {
		t.Fatalf("GDSCODE handler did not run; node hits = %+v", run.result.Routines[0].Nodes)
	}
}

func TestDebugReadOnlyErrorOutsideRetryableNodeFails(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n    X = X + 1;\nEND"
	rows := 0
	next := func() (bool, error) {
		rows++
		if rows == 1 {
			return true, nil
		}
		return false, &Error{Operation: "fake fetch", SQLCode: -817, NativeCode: 335544361,
			Message: "read-only transaction", gdsCodes: []int64{335544361}}
	}
	s, _, _, _ := newDebugTxRun(t, body, nil, next, nil, true)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugFailed ||
		ended.Err == nil || !strings.Contains(ended.Err.Error(), "This statement needs a read-write transaction; restart the debug session.") {
		t.Fatalf("read-only failure after loop body started = %+v", ended)
	}
}

func TestDebugReadOnlyRetryErrorIsFatalAndNotCaught(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  BEGIN\n" +
		"    EXECUTE PROCEDURE P_READ;\n  WHEN ANY DO\n    X = 1;\n  END\nEND"
	attempts := 0
	execute := func(query string, _ TransactionOptions) (int64, error) {
		if strings.HasPrefix(strings.ToUpper(query), "EXECUTE PROCEDURE P_READ") {
			attempts++
			return 0, &Error{Operation: "fake execute", SQLCode: -817, NativeCode: 335544361,
				Message: "synthetic read-only transaction", gdsCodes: []int64{335544361}}
		}
		return 1, nil
	}
	callee := debugTxProcedure{name: "P_READ", body: "BEGIN\n  EXIT;\nEND", readsOnly: true}
	s, run, _, _ := newDebugTxRun(t, body, []debugTxProcedure{callee},
		func() (bool, error) { return false, nil }, execute, false)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugFailed || attempts != 2 || ended.Err == nil ||
		!strings.Contains(ended.Err.Error(), "This statement needs a read-write transaction; restart the debug session.") {
		t.Fatalf("repeated read-only error = %+v, attempts=%d", ended, attempts)
	}
	if lineProfileRoutineHits(run.result.Routines[0], 6) != 0 {
		t.Fatalf("WHEN ANY handler caught a repeated read-only transaction error; nodes = %+v", run.result.Routines[0].Nodes)
	}
}

func TestDebugReadOnlyErrorInReadWritePhaseIsFatalAndNotCaught(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  BEGIN\n" +
		"    UPDATE LP_T SET A = 1;\n  WHEN ANY DO\n    X = 1;\n  END\nEND"
	attempts := 0
	execute := func(query string, _ TransactionOptions) (int64, error) {
		if strings.HasPrefix(strings.ToUpper(query), "UPDATE LP_T") {
			attempts++
			return 0, &Error{Operation: "fake update", SQLCode: -817, NativeCode: 335544361,
				Message: "synthetic read-only transaction", gdsCodes: []int64{335544361}}
		}
		return 1, nil
	}
	s, run, _, _ := newDebugTxRun(t, body, nil, func() (bool, error) { return false, nil }, execute, false)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	ended := sendDebugCommand(t, s, DebugCommand{Kind: DebugContinue})
	if ended.Status != DebugEndedStatus || ended.Outcome != DebugFailed || attempts != 1 || ended.Err == nil ||
		!strings.Contains(ended.Err.Error(), "This statement needs a read-write transaction; restart the debug session.") {
		t.Fatalf("read-only error in read-write phase = %+v, attempts=%d", ended, attempts)
	}
	if lineProfileRoutineHits(run.result.Routines[0], 6) != 0 {
		t.Fatalf("WHEN ANY handler caught a read-only transaction error in read-write phase; nodes = %+v", run.result.Routines[0].Nodes)
	}
}

func TestDebugNativeSelectableProcedureCallsMarkWritten(t *testing.T) {
	procedure := debugTxProcedure{
		name:    "P_WRITE",
		body:    "BEGIN\n  UPDATE LP_T SET A = 1;\n  SUSPEND;\nEND",
		inputs:  []lpParam{{name: "N", typeText: "INTEGER"}},
		outputs: []lpParam{{name: "ID", typeText: "INTEGER"}},
	}
	tests := []struct {
		name, body string
		startPhase DebugPhase
		pauseLine  int
	}{
		{
			name:       "select into",
			body:       "DECLARE VARIABLE X INTEGER;\nBEGIN\n  SELECT ID FROM P_WRITE(1) INTO :X;\n  X = X + 1;\nEND",
			startPhase: DebugReadOnly, pauseLine: 4,
		},
		{
			name:       "for select",
			body:       "DECLARE VARIABLE X INTEGER;\nBEGIN\n  FOR SELECT ID FROM P_WRITE(1) INTO :X DO\n    X = X + 1;\n  X = 3;\nEND",
			startPhase: DebugReadWrite, pauseLine: 4,
		},
		{
			name:       "expression subquery",
			body:       "DECLARE VARIABLE X INTEGER;\nBEGIN\n  X = (SELECT ID FROM P_WRITE(1));\n  X = X + 1;\nEND",
			startPhase: DebugReadOnly, pauseLine: 4,
		},
		{
			name:       "condition subquery",
			body:       "DECLARE VARIABLE X INTEGER;\nBEGIN\n  IF ((SELECT ID FROM P_WRITE(1)) = 1) THEN X = 2;\nEND",
			startPhase: DebugReadOnly, pauseLine: 3,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows := 0
			s, _, _, _ := newDebugTxRun(t, tt.body, []debugTxProcedure{procedure}, func() (bool, error) {
				rows++
				return rows == 1, nil
			}, nil, false)
			t.Cleanup(func() { s.Stop() })
			if snap := nextDebugStop(t, s); snap.Phase != tt.startPhase {
				t.Fatalf("entry phase = %s, want %s", snap.Phase, tt.startPhase)
			}
			snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
			if snap.Status != DebugPaused || snap.Frames[0].Line != tt.pauseLine || snap.Phase != DebugWritten {
				t.Fatalf("after native selectable procedure = %+v, want pause at line %d with written phase", snap, tt.pauseLine)
			}
		})
	}
}

func TestDebugEmulatedSelectableProcedureIsNotMarkedNative(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM P_WRITE(1) INTO :X DO\n    X = X + 1;\nEND"
	procedure := debugTxProcedure{
		name:    "P_WRITE",
		body:    "BEGIN\n  UPDATE LP_T SET A = 1;\n  SUSPEND;\nEND",
		inputs:  []lpParam{{name: "N", typeText: "INTEGER"}},
		outputs: []lpParam{{name: "ID", typeText: "INTEGER"}},
	}
	rows := 0
	s, _, _, _ := newDebugTxRun(t, body, []debugTxProcedure{procedure}, func() (bool, error) {
		rows++
		return rows == 1, nil
	}, nil, false)
	t.Cleanup(func() { s.Stop() })
	_ = nextDebugStop(t, s)
	snap := sendDebugCommand(t, s, DebugCommand{Kind: DebugStepInto})
	if snap.Status != DebugPaused || snap.Frames[0].Routine != "P_WRITE" || snap.Phase != DebugReadWrite {
		t.Fatalf("paused in emulated selectable procedure before its update = %+v", snap)
	}
	snap = sendDebugCommand(t, s, DebugCommand{Kind: DebugStepOver})
	if snap.Status != DebugPaused || snap.Phase != DebugWritten {
		t.Fatalf("emulated procedure update phase = %+v, want written after its update", snap)
	}
}

func TestDebugReadOnlyTransactionErrorName(t *testing.T) {
	if code, ok := GDSCodeByName("read_only_trans"); !ok || code != 335544361 {
		t.Fatalf("read_only_trans = %d, %v; want 335544361", code, ok)
	}
}

func TestDebugReadOnlyRetryErrorIsIdentified(t *testing.T) {
	err := &Error{Operation: "fake", NativeCode: 335544361, gdsCodes: []int64{335544361}}
	if !isDebugReadOnlyTransactionError(errors.Join(errors.New("wrapped"), err)) {
		t.Fatal("wrapped isc_read_only_trans was not identified")
	}
}
