package interbase

import (
	"context"
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"interbase-go/psql"
)

// lineProfileTestProc is a procedure of a fake step-in run.
type lineProfileTestProc struct {
	name            string
	inputs, outputs []lpParam
	body            string
}

// lpGen yields I = 1 … N.
var lpGen = lineProfileTestProc{name: "LP_GEN",
	inputs: []lpParam{{"N", "INTEGER"}}, outputs: []lpParam{{"I", "INTEGER"}},
	body: "BEGIN\n  I = 0;\n  WHILE (I < N) DO\n  BEGIN\n    I = I + 1;\n    SUSPEND;\n  END\nEND"}

// lpTwice returns B = A + A.
var lpTwice = lineProfileTestProc{name: "LP_TWICE",
	inputs: []lpParam{{"A", "INTEGER"}}, outputs: []lpParam{{"B", "INTEGER"}},
	body: "BEGIN\n  B = A + A;\nEND"}

// lpBad yields one row, then raises E_ONE.
var lpBad = lineProfileTestProc{name: "LP_BAD",
	inputs: []lpParam{{"K", "INTEGER"}}, outputs: []lpParam{{"I", "INTEGER"}},
	body: "BEGIN\n  I = K;\n  SUSPEND;\n  EXCEPTION E_ONE;\nEND"}

// lpRec recurses N levels deep and returns D = N + 1.
var lpRec = lineProfileTestProc{name: "LP_REC",
	inputs: []lpParam{{"N", "INTEGER"}}, outputs: []lpParam{{"D", "INTEGER"}},
	body: "BEGIN\n  D = 0;\n  IF (N > 0) THEN\n    EXECUTE PROCEDURE LP_REC N - 1 RETURNING_VALUES :D;\n  D = D + 1;\nEND"}

var lpNative = lineProfileTestProc{name: "LP_NATIVE", body: "BEGIN\n  EXIT;\nEND"}

// newLineProfileStepRun is a handler fake run (newLineProfileHandlerRun)
// whose entry routine LP_FAKE has the output X (kept as the result row) and
// calls the given procedures, stepping into those named in stepInto.
func newLineProfileStepRun(t *testing.T, ctx context.Context, body string, procs []lineProfileTestProc, stepInto []string, fail ...error) (*lpRun, *[]string) {
	t.Helper()
	return newLineProfileStepRunRows(t, ctx, body, procs, stepInto, func() (bool, error) { return false, nil }, fail...)
}

// newLineProfileStepRunRows is newLineProfileStepRun whose SELECT
// statements return rows from next.
func newLineProfileStepRunRows(t *testing.T, ctx context.Context, body string, procs []lineProfileTestProc, stepInto []string,
	next func() (bool, error), fail ...error) (*lpRun, *[]string) {
	t.Helper()
	run, savepoints := newLineProfileHandlerRunRows(t, ctx, "BEGIN\nEND", next, fail...)
	routine, problems := psql.ParseRoutine(body, psql.ParseOptions{Dialect: 3, Params: []string{"X"}})
	if len(problems) != 0 {
		t.Fatalf("entry parse problems: %+v", problems)
	}
	entry := &lpRoutineMeta{name: "LP_FAKE", routine: routine, outputs: []lpParam{{"X", "INTEGER"}}}
	run.plan.entry = entry
	run.plan.procs = map[string]*lpRoutineMeta{"LP_FAKE": entry}
	run.plan.analysis.Entry = psql.EntryCall{Procedure: "LP_FAKE"}
	for _, p := range procs {
		var names []string
		for _, param := range append(append([]lpParam(nil), p.inputs...), p.outputs...) {
			names = append(names, param.name)
		}
		r, problems := psql.ParseRoutine(p.body, psql.ParseOptions{Dialect: 3, Params: names})
		if len(problems) != 0 {
			t.Fatalf("%s parse problems: %+v", p.name, problems)
		}
		run.plan.procs[p.name] = &lpRoutineMeta{name: p.name, source: p.body, routine: r, inputs: p.inputs, outputs: p.outputs}
	}
	run.stepInto = make(map[string]bool)
	for _, name := range stepInto {
		run.stepInto[name] = true
	}
	run.result.Routines = []LineProfileRoutine{newRoutineProfile(entry)}
	run.result.Columns = []string{"X"}
	return run, savepoints
}

// waitGoroutines waits for the goroutine count to fall back to want.
func waitGoroutines(t *testing.T, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > want {
		if time.Now().After(deadline) {
			t.Fatalf("goroutines %d, want %d: a stepped-into routine leaked", runtime.NumGoroutine(), want)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func lineProfileRoutineNames(routines []LineProfileRoutine) []string {
	var names []string
	for _, r := range routines {
		names = append(names, r.Name)
	}
	return names
}

func lineProfileRoutineHits(routine LineProfileRoutine, line int) int64 {
	var hits int64
	for _, node := range routine.Nodes {
		if node.Line == line && node.Kind != "block" {
			hits += node.Metrics.Hits
		}
	}
	return hits
}

func TestLineProfileStepIntoShapes(t *testing.T) {
	goroutines := runtime.NumGoroutine()
	body := "BEGIN\n" +
		"  FOR SELECT I FROM LP_GEN(3) INTO :X DO\n" +
		"    UPDATE LP_T SET A = :X;\n" +
		"  SELECT I FROM LP_GEN(1) INTO :X;\n" +
		"  EXECUTE PROCEDURE LP_TWICE :X RETURNING_VALUES :X;\n" +
		"  EXECUTE PROCEDURE LP_TWICE :X RETURNING_VALUES :X;\n" +
		"END"
	run, _ := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{lpGen, lpTwice}, []string{"LP_GEN", "LP_TWICE"})
	outcome, txErr := run.runInTransaction()
	if outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	waitGoroutines(t, goroutines)
	if len(run.result.KeptRows) != 1 || run.result.KeptRows[0][0].Value != int64(4) {
		t.Fatalf("kept rows %#v, want X = 4", run.result.KeptRows)
	}
	routines := run.result.Routines
	if got := lineProfileRoutineNames(routines); !reflect.DeepEqual(got, []string{"LP_FAKE", "LP_GEN", "LP_GEN", "LP_TWICE", "LP_TWICE"}) {
		t.Fatalf("routines %v", got)
	}
	for i, want := range []struct {
		parent, line int
		calls        int64
	}{{-1, 0, 1}, {0, 2, 1}, {0, 4, 1}, {0, 5, 1}, {0, 6, 1}} {
		if r := routines[i]; r.Parent != want.parent || r.CallLine != want.line || r.Calls != want.calls {
			t.Fatalf("routine %d %s parent %d line %d calls %d; want %+v", i, r.Name, r.Parent, r.CallLine, r.Calls, want)
		}
	}
	if hits := lineProfileRoutineHits(routines[1], 3); hits != 4 {
		t.Fatalf("FOR SELECT callee WHILE hits %d, want 4", hits)
	}
	if hits := lineProfileRoutineHits(routines[0], 3); hits != 3 {
		t.Fatalf("loop body hits %d, want 3", hits)
	}
	if rows := routines[0].Nodes[1].Metrics.Rows; rows != 3 {
		t.Fatalf("FOR SELECT rows %d, want 3", rows)
	}
	if hits := lineProfileRoutineHits(routines[2], 3); hits != 2 {
		t.Fatalf("singleton callee WHILE hits %d, want 2 (runs to its end for the second fetch)", hits)
	}
}

func TestLineProfileStepIntoAggregatesCalls(t *testing.T) {
	body := "DECLARE VARIABLE K INTEGER;\nBEGIN\n" +
		"  X = 1;\n" +
		"  K = 0;\n" +
		"  WHILE (K < 3) DO\n" +
		"  BEGIN\n" +
		"    K = K + 1;\n" +
		"    EXECUTE PROCEDURE LP_TWICE :X RETURNING_VALUES :X;\n" +
		"  END\n" +
		"END"
	run, _ := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{lpTwice}, []string{"LP_TWICE"})
	if outcome, _ := run.runInTransaction(); outcome != nil {
		t.Fatalf("outcome %v", outcome)
	}
	if len(run.result.Routines) != 2 || run.result.Routines[1].Calls != 3 || run.result.KeptRows[0][0].Value != int64(8) {
		t.Fatalf("routines %v calls %d rows %#v", lineProfileRoutineNames(run.result.Routines), run.result.Routines[1].Calls, run.result.KeptRows)
	}
}

func TestLineProfileStepIntoAbandonsCallee(t *testing.T) {
	goroutines := runtime.NumGoroutine()
	body := "BEGIN\n" +
		"  FOR SELECT I FROM LP_GEN(5) INTO :X DO\n" +
		"    IF (X = 2) THEN EXIT;\n" +
		"END"
	run, _ := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{lpGen}, []string{"LP_GEN"})
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	waitGoroutines(t, goroutines)
	if hits := lineProfileRoutineHits(run.result.Routines[1], 3); hits != 2 {
		t.Fatalf("callee WHILE hits %d, want 2: the callee stops at its second SUSPEND", hits)
	}

	// A closed callee keeps its work: its open savepoints are released.
	writer := lineProfileTestProc{name: "LP_GENW", inputs: lpGen.inputs, outputs: lpGen.outputs,
		body: "BEGIN\n  I = 0;\n  WHILE (I < N) DO\n  BEGIN\n    I = I + 1;\n    UPDATE LP_T SET A = :I;\n    SUSPEND;\n  END\nEND"}
	body = "BEGIN\n  BEGIN\n    FOR SELECT I FROM LP_GENW(5) INTO :X DO\n      IF (X = 2) THEN EXIT;\n  WHEN ANY DO X = 0;\n  END\nEND"
	run, savepoints := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{writer}, []string{"LP_GENW"})
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	want := []string{"SAVEPOINT LP$SP_0_1", "SAVEPOINT LP$SP_1_1", "SAVEPOINT LP$SP_1_2", "RELEASE SAVEPOINT LP$SP_1_2",
		"SAVEPOINT LP$SP_1_3", "RELEASE SAVEPOINT LP$SP_1_3", "RELEASE SAVEPOINT LP$SP_1_1", "RELEASE SAVEPOINT LP$SP_0_1"}
	if !reflect.DeepEqual(*savepoints, want) {
		t.Fatalf("savepoints %q, want %q", *savepoints, want)
	}

	// A callee closed while it is suspended in its own FOR SELECT loop
	// closes that cursor and ends cleanly.
	scan := lineProfileTestProc{name: "LP_SCAN", inputs: lpGen.inputs, outputs: lpGen.outputs,
		body: "BEGIN\n  FOR SELECT ID FROM LP_T INTO :I DO\n    SUSPEND;\nEND"}
	run, _ = newLineProfileStepRunRows(t, context.Background(), "BEGIN\n  FOR SELECT I FROM LP_SCAN(1) INTO :X DO\n    EXIT;\nEND",
		[]lineProfileTestProc{scan}, []string{"LP_SCAN"}, func() (bool, error) { return true, nil })
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("callee closed in its FOR SELECT: %v, %v", outcome, txErr)
	}
	if run.result.KeptRows[0][0].Value != int64(1) || run.result.Routines[1].Nodes[1].Metrics.Rows != 1 {
		t.Fatalf("kept rows %#v, callee rows %d", run.result.KeptRows, run.result.Routines[1].Nodes[1].Metrics.Rows)
	}
	waitGoroutines(t, goroutines)
}

func TestLineProfileStepIntoSingletonError(t *testing.T) {
	goroutines := runtime.NumGoroutine()
	run, _ := newLineProfileStepRun(t, context.Background(), "BEGIN\n  SELECT I FROM LP_GEN(3) INTO :X;\nEND",
		[]lineProfileTestProc{lpGen}, []string{"LP_GEN"})
	outcome, _ := run.runInTransaction()
	var nativeErr *Error
	var located *lpNodeError
	if !errors.As(outcome, &nativeErr) || nativeErr.SQLCode != -811 || !errors.As(outcome, &located) || located.line != 2 || located.routine != "LP_FAKE" {
		t.Fatalf("outcome %v (%+v)", outcome, located)
	}
	waitGoroutines(t, goroutines)
	if hits := lineProfileRoutineHits(run.result.Routines[1], 3); hits != 2 {
		t.Fatalf("callee WHILE hits %d, want 2", hits)
	}
}

// TestLineProfileCallErrorUndoesHandlingBlock checks the native rule: an
// error that a procedure call (native or stepped into) raises undoes the
// handling block's work before its handler runs, unlike a DML or EXCEPTION
// statement's error (R19).
func TestLineProfileCallErrorUndoesHandlingBlock(t *testing.T) {
	failed := &Error{Operation: "exec", SQLCode: -836, NativeCode: 335544517,
		gdsCodes: []int64{335544517, 335544382}, exceptionNumber: 7, hasException: true}
	for _, tt := range []struct {
		name     string
		call     string
		stepInto []string
		fail     []error
	}{
		{"native call", "EXECUTE PROCEDURE LP_NATIVE;", nil, []error{nil, failed}},
		{"stepped selectable callee", "FOR SELECT I FROM LP_BAD(1) INTO :X DO UPDATE LP_T SET A = :X;", []string{"LP_BAD"}, nil},
	} {
		t.Run(tt.name, func(t *testing.T) {
			goroutines := runtime.NumGoroutine()
			body := "BEGIN\n  BEGIN\n    UPDATE LP_T SET A = 1;\n    " + tt.call + "\n  WHEN ANY DO X = 7;\n  END\nEND"
			run, savepoints := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{lpNative, lpBad}, tt.stepInto, tt.fail...)
			if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
				t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
			}
			waitGoroutines(t, goroutines)
			want := []string{"SAVEPOINT LP$SP_0_1", "ROLLBACK TO SAVEPOINT LP$SP_0_1", "SAVEPOINT LP$SP_0_2", "RELEASE SAVEPOINT LP$SP_0_2"}
			if !reflect.DeepEqual(*savepoints, want) {
				t.Fatalf("savepoints %q, want %q", *savepoints, want)
			}
			if run.result.KeptRows[0][0].Value != int64(7) {
				t.Fatalf("handler did not run: %#v", run.result.KeptRows)
			}
		})
	}
}

func TestLineProfileStepIntoDepthLimit(t *testing.T) {
	goroutines := runtime.NumGoroutine()
	run, _ := newLineProfileStepRun(t, context.Background(), "BEGIN\n  EXECUTE PROCEDURE LP_REC 31 RETURNING_VALUES :X;\nEND",
		[]lineProfileTestProc{lpRec}, []string{"LP_REC"})
	if outcome, _ := run.runInTransaction(); outcome != nil || run.result.KeptRows[0][0].Value != int64(32) {
		t.Fatalf("depth 32: outcome %v rows %#v", outcome, run.result.KeptRows)
	}
	if n := len(run.result.Routines); n != 33 || run.result.Routines[32].Parent != 31 {
		t.Fatalf("routines %d, want the entry and 32 frames each the child of the previous", n)
	}
	run, _ = newLineProfileStepRun(t, context.Background(), "BEGIN\n  BEGIN\n    EXECUTE PROCEDURE LP_REC 32 RETURNING_VALUES :X;\n  WHEN ANY DO X = 1;\n  END\nEND",
		[]lineProfileTestProc{lpRec}, []string{"LP_REC"})
	outcome, _ := run.runInTransaction()
	var fatalErr *lpFatalError
	if outcome == nil || !errors.As(outcome, &fatalErr) || !strings.Contains(outcome.Error(), "32") {
		t.Fatalf("depth 33: outcome %v, want a depth limit error no handler catches", outcome)
	}
	waitGoroutines(t, goroutines)
}

func TestLineProfileCoroutine(t *testing.T) {
	goroutines := runtime.NumGoroutine()
	generate := func(n int, fail error) func(suspend func([]any) error) error {
		return func(suspend func([]any) error) error {
			for i := 1; i <= n; i++ {
				if err := suspend([]any{int64(i)}); err != nil {
					return err
				}
			}
			return fail
		}
	}
	cleaned := 0
	cleanup := func() { cleaned++ }

	co := startCoroutine(generate(2, nil), cleanup)
	var got []any
	for {
		row, ok, err := co.next()
		if err != nil {
			t.Fatal(err)
		}
		if !ok {
			break
		}
		got = append(got, row[0])
	}
	if !reflect.DeepEqual(got, []any{int64(1), int64(2)}) || co.close() != nil {
		t.Fatalf("rows %v", got)
	}

	co = startCoroutine(generate(5, nil), cleanup)
	if _, ok, err := co.next(); !ok || err != nil {
		t.Fatalf("first row %v %v", ok, err)
	}
	if err := co.close(); err != nil {
		t.Fatalf("abandon: %v", err)
	}

	boom := errors.New("boom")
	co = startCoroutine(generate(1, boom), cleanup)
	co.next()
	if _, ok, err := co.next(); ok || !errors.Is(err, boom) {
		t.Fatalf("error after a row = %v, %v", ok, err)
	}
	if err := co.close(); err != nil {
		t.Fatalf("close after the error: %v", err)
	}

	co = startCoroutine(func(func([]any) error) error { panic("bad") }, cleanup)
	if _, _, err := co.next(); err == nil || !strings.Contains(err.Error(), "bad") {
		t.Fatalf("panic = %v", err)
	}

	co = startCoroutine(generate(1, nil), cleanup)
	if err := co.close(); err != nil {
		t.Fatalf("close before the first row: %v", err)
	}
	if cleaned != 5 {
		t.Fatalf("cleanups %d, want 5", cleaned)
	}
	waitGoroutines(t, goroutines)
}

func TestLineProfileInclusiveMetrics(t *testing.T) {
	node := func(raw time.Duration, hits int64) []LineProfileNode {
		return []LineProfileNode{{Line: 1, Metrics: LineMetrics{Hits: hits, Raw: raw, Adjusted: raw / 2, Tables: map[int]TableCounters{1: {SeqReads: hits}}}}}
	}
	routines := []LineProfileRoutine{
		{Name: "A", Parent: -1, Nodes: node(10, 1)},
		{Name: "B", Parent: 0, Nodes: node(20, 2)},
		{Name: "C", Parent: 1, Nodes: node(40, 4)},
		{Name: "D", Parent: 0, Nodes: node(80, 8)},
	}
	aggregateRoutines(routines)
	for i, want := range []int64{15, 6, 4, 8} {
		r := routines[i]
		if r.Inclusive.Hits != want || r.Exclusive.Hits != r.Nodes[0].Metrics.Hits || r.Inclusive.Tables[1].SeqReads != want {
			t.Fatalf("%s inclusive %+v exclusive %+v; want inclusive hits %d", r.Name, r.Inclusive, r.Exclusive, want)
		}
	}
	if routines[0].Inclusive.Raw != 150 || routines[0].Inclusive.Adjusted != 75 || routines[1].Inclusive.Raw != 60 {
		t.Fatalf("inclusive times %v/%v, %v", routines[0].Inclusive.Raw, routines[0].Inclusive.Adjusted, routines[1].Inclusive.Raw)
	}
}

func TestLineProfileNativeCallSites(t *testing.T) {
	body := "BEGIN\n" +
		"  FOR SELECT I FROM LP_GEN(2) INTO :X DO\n" +
		"    X = X;\n" +
		"  SELECT MAX(I) FROM LP_GEN(3) INTO :X;\n" +
		"  SELECT COUNT(*) FROM LP_TWICE(1) INTO :X;\n" +
		"  EXECUTE PROCEDURE LP_TWICE 1 RETURNING_VALUES :X;\n" +
		"END"
	run, _ := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{lpGen, lpTwice}, []string{"LP_GEN"})
	got := nativeCallSites(run.plan, run.stepInto)
	if len(got) != 1 || got[0].Routine != "LP_FAKE" || got[0].Line != 4 || got[0].Procedure != "LP_GEN" || got[0].Reason == "" {
		t.Fatalf("native call sites %+v", got)
	}
}

func TestLineProfileAssignTextToBlob(t *testing.T) {
	run := &lpRun{plan: &lpPlan{dialect: 3, charsets: map[int]lpCharset{0: {"NONE", 1}}}, result: &LineProfileResult{},
		statements: make(map[string]cachedStatement)}
	f := &lpFrame{vars: map[string]*lpVar{"B": {typ: psql.ParseTypeText("BLOB SUB_TYPE 1")}}}
	node := &LineProfileNode{}
	for _, tt := range []struct {
		value  any
		source OutputDescriptor
	}{
		{"text", OutputDescriptor{Kind: "VARCHAR", Length: 20}},
		{"ab   ", OutputDescriptor{Kind: "CHAR", Length: 5}},
	} {
		if err := run.assign(f, node, "B", tt.value, tt.source, false); err != nil || f.vars["B"].value != tt.value {
			t.Fatalf("assign(%q) = %v, held %#v", tt.value, err, f.vars["B"].value)
		}
	}
	if node.Metrics.ServerCalls != 0 || len(run.statements) != 0 {
		t.Fatalf("text into a BLOB used the server: %+v", node.Metrics)
	}
	run.plan.charsets[2] = lpCharset{"WIN1252", 1}
	if err := run.assign(f, node, "B", "w", OutputDescriptor{Kind: "VARCHAR", Length: 5, SubType: 2}, false); err != nil || f.vars["B"].value != "w" {
		t.Fatalf("WIN1252 text into a BLOB = %v, held %#v", err, f.vars["B"].value)
	}
}

// TestLineProfileBlobValue checks the native results of assigning a value
// of each type to a BLOB variable.
func TestLineProfileBlobValue(t *testing.T) {
	run := &lpRun{plan: &lpPlan{dialect: 3}}
	for _, tt := range []struct {
		value   any
		source  string
		want    any
		sqlCode int
		fatal   bool
	}{
		{nil, "INTEGER", nil, 0, false},
		{"t", "VARCHAR(5) CHARACTER SET UTF8", "t", 0, false},
		{"ab   ", "CHAR(5)", "ab   ", 0, false},
		{[]byte("x"), "BLOB SUB_TYPE 0", []byte("x"), 0, false},
		{int64(0), "INTEGER", "", 0, false},
		{int64(42), "SMALLINT", lpBadBlob{}, 0, false},
		{int64(42), "NUMERIC(18,0)", lpBadBlob{}, 0, false},
		{"12.50", "NUMERIC(15,2)", nil, -902, false},
		{12.5, "DOUBLE PRECISION", nil, -902, false},
		{time.Date(2020, 1, 2, 0, 0, 0, 0, time.UTC), "DATE", nil, -413, false},
		{time.Date(2020, 1, 2, 10, 0, 0, 0, time.UTC), "TIMESTAMP", nil, -413, false},
		{true, "BOOLEAN", nil, 0, true},
	} {
		got, err := run.blobValue(tt.value, tt.source)
		var nativeErr *Error
		var fatalErr *lpFatalError
		switch {
		case tt.fatal:
			if !errors.As(err, &fatalErr) {
				t.Fatalf("%s: %v, want an unsupported (fatal) error", tt.source, err)
			}
		case tt.sqlCode != 0:
			if errors.As(err, &fatalErr) || !errors.As(err, &nativeErr) || nativeErr.SQLCode != tt.sqlCode {
				t.Fatalf("%s: %v, want a catchable SQLCODE %d", tt.source, err, tt.sqlCode)
			}
		default:
			if err != nil || !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("%s %#v = %#v, %v; want %#v", tt.source, tt.value, got, err, tt.want)
			}
		}
	}
	if err := checkBlobArgs([]any{int64(1), lpBadBlob{}}); lpTestSQLCode(err) != -901 {
		t.Fatalf("bad BLOB parameter = %v, want SQLCODE -901", err)
	}
}

func lpTestSQLCode(err error) int {
	var nativeErr *Error
	if errors.As(err, &nativeErr) {
		return nativeErr.SQLCode
	}
	return 0
}

func TestLineProfileSplitAliasOperators(t *testing.T) {
	for _, item := range []string{
		"A IS NULL", "A IS NOT NULL", "A NOT LIKE B", "A LIKE B", "A CONTAINING B", "A STARTING WITH B",
		"A BETWEEN 1 AND B", "A = 1 OR B", "NOT B", "A IN B",
	} {
		if _, alias, ok := splitAlias(item); ok {
			t.Fatalf("splitAlias(%q) = alias %q, ok; want the item skipped", item, alias)
		}
	}
	if expr, alias, ok := splitAlias("NULL"); !ok || expr != "NULL" || alias != "" {
		t.Fatalf("bare NULL = %q %q %v", expr, alias, ok)
	}
	if expr, alias, ok := splitAlias("A || B C"); !ok || expr != "A || B" || alias != " C" {
		t.Fatalf("plain alias = %q %q %v", expr, alias, ok)
	}
	if got, cast := castSelectItems("SELECT A IS NULL, B FROM T", []string{"INTEGER", "INTEGER"}); got != "SELECT A IS NULL, CAST(B AS INTEGER) FROM T" || !reflect.DeepEqual(cast, []bool{false, true}) {
		t.Fatalf("castSelectItems() = %q %v", got, cast)
	}
}

func TestLineProfileStepIntoNamesAreNormalized(t *testing.T) {
	routine, _ := psql.ParseRoutine("BEGIN\nEND", psql.ParseOptions{Dialect: 3})
	plan := &lpPlan{procs: map[string]*lpRoutineMeta{
		"LP_GEN": {name: "LP_GEN", routine: routine},
		"Mixed":  {name: "Mixed", routine: routine},
		"MIXED":  {name: "MIXED", routine: routine},
	}}
	plan.analysis.Callees = []LineProfileCallee{{Name: "LP_GEN", Parses: true}, {Name: "Mixed", Parses: true}, {Name: "MIXED", Parses: true}}
	got := stepIntoSet(plan, []string{"lp_gen", `"Mixed"`})
	if want := map[string]bool{"LP_GEN": true, "Mixed": true}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stepIntoSet() = %v, want %v", got, want)
	}
}

// TestLineProfileNativeProcedureCursorRowByRow checks ibg-mkx: a native FOR
// SELECT over a procedure fetches one row at a time (FOR UPDATE) when the
// procedure or the loop body may write, so the procedure cannot run ahead of
// the body; when FOR UPDATE does not prepare, the call site is labeled.
func TestLineProfileNativeProcedureCursorRowByRow(t *testing.T) {
	const reader = "BEGIN\n  FOR SELECT ID FROM LP_T INTO :X DO SUSPEND;\nEND"
	const writer = "BEGIN\n  INSERT INTO LP_T (ID) VALUES (1);\n  SUSPEND;\nEND"
	readBody := "DECLARE VARIABLE Y INTEGER;\nBEGIN\n  FOR SELECT X FROM LP_SRC(1) INTO :Y DO\n    Y = Y + 1;\nEND"
	writeBody := "DECLARE VARIABLE Y INTEGER;\nBEGIN\n  FOR SELECT X FROM LP_SRC(1) INTO :Y DO\n    UPDATE LP_T SET A = :Y;\nEND"
	joinBody := "DECLARE VARIABLE Y INTEGER;\nBEGIN\n  FOR SELECT T.ID FROM LP_T T JOIN LP_SRC(1) S ON S.X = T.ID INTO :Y DO\n    Y = Y + 1;\nEND"
	tableBody := "DECLARE VARIABLE Y INTEGER;\nBEGIN\n  FOR SELECT ID FROM LP_T INTO :Y DO\n    UPDATE LP_T SET A = :Y;\nEND"
	for _, tt := range []struct {
		name, body, callee string
		known, readOnly    bool
		refuse             bool
		wantRowByRow       bool
		wantSite           bool
	}{
		{name: "callee writes", body: readBody, callee: writer, known: true, wantRowByRow: true},
		{name: "callee reads, body reads", body: readBody, callee: reader, known: true},
		{name: "callee reads, body writes", body: writeBody, callee: reader, known: true, wantRowByRow: true},
		{name: "callee unknown", body: readBody, wantRowByRow: true},
		{name: "join with a writing callee", body: joinBody, callee: writer, known: true, wantRowByRow: true},
		{name: "table only", body: tableBody, callee: writer, known: true},
		{name: "read-only run", body: readBody, callee: writer, known: true, readOnly: true},
		{name: "FOR UPDATE refused", body: readBody, callee: writer, known: true, refuse: true, wantSite: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var procs []lineProfileTestProc
			if tt.known {
				procs = []lineProfileTestProc{{name: "LP_SRC", body: tt.callee,
					inputs: []lpParam{{"P", "INTEGER"}}, outputs: []lpParam{{"X", "INTEGER"}}}}
			}
			rows := 2
			run, _ := newLineProfileStepRunRows(t, context.Background(), tt.body, procs, nil,
				func() (bool, error) { rows--; return rows >= 0, nil })
			if tt.known {
				meta := run.plan.procs["LP_SRC"]
				meta.readsOnly = tt.callee == reader
			}
			run.opts.ReadOnly = tt.readOnly
			var prepared []string
			prepare := run.tx.native.prepareContextOverride
			run.tx.native.prepareContextOverride = func(query string, arrays bool, op *nativeCancelOperation) (*nativeStatement, error) {
				prepared = append(prepared, query)
				if tt.refuse && strings.HasSuffix(query, " FOR UPDATE") {
					return nil, &Error{Operation: "prepare", SQLCode: -104, Message: "token unknown"}
				}
				return prepare(query, arrays, op)
			}
			if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
				t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
			}
			loop := run.result.Routines[0].Nodes[1]
			rowByRow := strings.HasSuffix(loop.SQL, " FOR UPDATE")
			if rowByRow != tt.wantRowByRow || loop.Metrics.Rows != 2 || loop.Metrics.Errors != 0 {
				t.Fatalf("FOR SELECT sent %q (rows %d, errors %d), want row by row %v; prepared %q",
					loop.SQL, loop.Metrics.Rows, loop.Metrics.Errors, tt.wantRowByRow, prepared)
			}
			var sites []LineProfileCallSite
			for _, site := range run.result.NativeCallSites {
				if site.Reason == lpReasonRunsAhead {
					sites = append(sites, site)
				}
			}
			if tt.wantSite != (len(sites) == 1) || tt.wantSite &&
				(sites[0] != LineProfileCallSite{Routine: "LP_FAKE", Line: 3, Procedure: "LP_SRC", Reason: lpReasonRunsAhead}) {
				t.Fatalf("run-ahead call sites %+v, want one: %v", sites, tt.wantSite)
			}
		})
	}
}

func TestLineProfileSourceProceduresResolvedOncePerRoutine(t *testing.T) {
	routine, problems := psql.ParseRoutine("DECLARE VARIABLE X INTEGER;\nBEGIN\n"+
		"  SELECT A FROM LP_SRC INTO :X;\n"+
		"  SELECT A FROM LP_TAB INTO :X;\n"+
		"END", psql.ParseOptions{Dialect: 3})
	if len(problems) != 0 {
		t.Fatalf("parse problems: %+v", problems)
	}
	meta := &lpRoutineMeta{name: "LP_CALLER", routine: routine}
	lookups := 0
	plan := &lpPlan{dialect: 3, procs: map[string]*lpRoutineMeta{"LP_CALLER": meta}}
	plan.readProcedureRefs = func(_ context.Context, _ *Transaction, names []lookupName) ([]RoutineRef, error) {
		lookups++
		if len(names) != 2 {
			t.Fatalf("lookup of %+v, want both source names", names)
		}
		return nil, nil
	}
	if _, mayWrite, err := plan.callees(context.Background(), nil, meta, 1); err != nil || mayWrite {
		t.Fatalf("callees() mayWrite %v, err %v", mayWrite, err)
	}
	if err := plan.checkReadsOnly(context.Background(), nil, meta); err != nil || !meta.readsOnly {
		t.Fatalf("checkReadsOnly() readsOnly %v, err %v", meta.readsOnly, err)
	}
	if lookups != 1 {
		t.Fatalf("source names looked up %d times, want once per routine", lookups)
	}
	procedures, err := plan.sourceProcedures(context.Background(), nil, meta)
	if err != nil || len(procedures) != 0 || lookups != 1 {
		t.Fatalf("sourceProcedures() = %v, %v after %d lookups", procedures, err, lookups)
	}
}

// TestLineProfileNativeCallSitesOnlyInSteppedRoutines checks that only
// routines the run can step into are scanned: LP_OUTER2 is called in a
// shape that runs natively, so its own calls are never emulated.
func TestLineProfileNativeCallSitesOnlyInSteppedRoutines(t *testing.T) {
	outer := lineProfileTestProc{name: "LP_OUTER2", inputs: []lpParam{{"N", "INTEGER"}}, outputs: []lpParam{{"I", "INTEGER"}},
		body: "BEGIN\n  SELECT MAX(I) FROM LP_GEN(2) INTO :I;\n  SUSPEND;\nEND"}
	body := "BEGIN\n" +
		"  SELECT MAX(I) FROM LP_OUTER2(1) INTO :X;\n" +
		"  EXECUTE PROCEDURE LP_TWICE 1 RETURNING_VALUES :X;\n" +
		"END"
	run, _ := newLineProfileStepRun(t, context.Background(), body, []lineProfileTestProc{lpGen, lpTwice, outer},
		[]string{"LP_GEN", "LP_TWICE", "LP_OUTER2"})
	want := []LineProfileCallSite{{Routine: "LP_FAKE", Line: 2, Procedure: "LP_OUTER2", Reason: lpReasonShape}}
	if got := nativeCallSites(run.plan, run.stepInto); !reflect.DeepEqual(got, want) {
		t.Fatalf("native call sites %+v, want %+v", got, want)
	}
}
