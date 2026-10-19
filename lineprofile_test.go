package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"io"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"interbase-go/psql"
)

func testLocalScope(literalStrings bool) localScope {
	vars := map[string]localValue{
		"N":    {kind: localNullKind},
		"I":    {kind: localIntKind, i: 7},
		"BIG":  {kind: localIntKind, i: maxLocalInteger},
		"S":    {kind: localStringKind, s: []byte("ab  ")},
		"CAFE": {kind: localStringKind, s: []byte("Caf\xe9")},
	}
	return localScope{
		lookup: func(name string) (localValue, bool) {
			v, ok := vars[name]
			return v, ok
		},
		literalStrings: literalStrings,
	}
}

func testExpr(t *testing.T, text string) psql.LocalNode {
	t.Helper()
	names := map[string]bool{"N": true, "I": true, "BIG": true, "S": true, "CAFE": true, "OTHER": true}
	e := psql.AnalyzeExpr(text, 1, 1, names, 3)
	return e.Local
}

func TestLineProfileLocalConditionTruthTable(t *testing.T) {
	tests := []struct {
		cond string
		want truth
	}{
		{"N IS NULL", truthTrue},
		{"N IS NOT NULL", truthFalse},
		{"I IS NULL", truthFalse},
		{"N = 1", truthUnknown},
		{"N = 1 AND 1 = 0", truthFalse},
		{"N = 1 AND 1 = 1", truthUnknown},
		{"N = 1 OR 1 = 1", truthTrue},
		{"N = 1 OR 1 = 0", truthUnknown},
		{"NOT (N = 1)", truthUnknown},
		{"NOT (I = 7)", truthFalse},
		{"I <> 7 OR I >= 7", truthTrue},
		{"I + 1 = 8 AND I - 8 < 0", truthTrue},
		{"I != 6", truthTrue},
		{"S = 'ab'", truthTrue},
		{"S = 'ab   '", truthTrue},
		{"'a' > 'a\x01'", truthTrue},
		{"CAFE > 'Cafe'", truthTrue},
		{"CAFE = 'Caf\xe9'", truthTrue},
		{"S < 'b'", truthTrue},
	}
	scope := testLocalScope(true)
	for _, tt := range tests {
		t.Run(tt.cond, func(t *testing.T) {
			node := testExpr(t, tt.cond)
			if node == nil {
				t.Fatalf("AnalyzeExpr(%q).Local = nil", tt.cond)
			}
			got, ok := evalLocalCondition(node, scope)
			if !ok {
				t.Fatalf("evalLocalCondition(%q) not local", tt.cond)
			}
			if got != tt.want {
				t.Fatalf("evalLocalCondition(%q) = %v, want %v", tt.cond, got, tt.want)
			}
		})
	}
}

func TestLineProfileLocalEvaluationRefusesDoubt(t *testing.T) {
	scope := testLocalScope(true)
	for _, cond := range []string{
		"BIG + 1 = 0",         // INTEGER overflow
		"-2147483647 - 2 = 0", // INTEGER underflow
		"'1' + 1 = 2",         // mixed families
		"I = 'a'",             // integer vs string
		"S = 1",               // string vs integer
		"OTHER = 1",           // variable not local-eligible
		"I",                   // a value is not a condition
	} {
		node := testExpr(t, cond)
		if node == nil {
			t.Fatalf("AnalyzeExpr(%q).Local = nil; the case must reach the evaluator", cond)
		}
		if got, ok := evalLocalCondition(node, scope); ok {
			t.Fatalf("evalLocalCondition(%q) = %v, want not local", cond, got)
		}
	}
	if _, ok := evalLocalCondition(testExpr(t, "S = 'ab'"), testLocalScope(false)); ok {
		t.Fatal("string literal compared locally when literal strings are not local")
	}
	if _, ok := evalLocalValue(testExpr(t, "I = 7"), scope); ok {
		t.Fatal("condition evaluated as a value")
	}
	if v, ok := evalLocalValue(testExpr(t, "I + 3 - 1"), scope); !ok || v.kind != localIntKind || v.i != 9 {
		t.Fatalf("evalLocalValue(I + 3 - 1) = %+v, %v", v, ok)
	}
	if _, ok := evalLocalValue(testExpr(t, "BIG + 1"), scope); ok {
		t.Fatal("overflowing addition evaluated locally")
	}
	if v, ok := evalLocalValue(testExpr(t, "N"), scope); !ok || v.kind != localNullKind {
		t.Fatalf("evalLocalValue(N) = %+v, %v", v, ok)
	}
	if v, ok := evalLocalValue(testExpr(t, "I + N"), scope); !ok || v.kind != localNullKind {
		t.Fatalf("evalLocalValue(I + N) = %+v, %v", v, ok)
	}
}

func TestLineProfileLocalConversion(t *testing.T) {
	smallint := psql.ParseTypeText("SMALLINT")
	integer := psql.ParseTypeText("INTEGER")
	char5 := psql.ParseTypeText("CHAR(5)")
	varchar2 := psql.ParseTypeText("VARCHAR(2)")
	numeric := psql.ParseTypeText("NUMERIC(15,2)")
	tests := []struct {
		name    string
		value   localValue
		typ     psql.TypeInfo
		strings bool
		want    any
		ok      bool
	}{
		{"null to numeric", localValue{kind: localNullKind}, numeric, false, nil, true},
		{"int to integer", localValue{kind: localIntKind, i: -5}, integer, false, int64(-5), true},
		{"int to smallint in range", localValue{kind: localIntKind, i: 32767}, smallint, false, int64(32767), true},
		{"int to smallint overflow", localValue{kind: localIntKind, i: 32768}, smallint, false, nil, false},
		{"int to numeric", localValue{kind: localIntKind, i: 1}, numeric, false, nil, false},
		{"string to char pads", localValue{kind: localStringKind, s: []byte("ab")}, char5, true, "ab   ", true},
		{"string to varchar fits", localValue{kind: localStringKind, s: []byte("ab")}, varchar2, true, "ab", true},
		{"string to varchar too long", localValue{kind: localStringKind, s: []byte("abc")}, varchar2, true, nil, false},
		{"padded string to varchar drops blanks", localValue{kind: localStringKind, s: []byte("ab ")}, varchar2, true, "ab", true},
		{"padded string keeps blanks within length", localValue{kind: localStringKind, s: []byte("a  ")}, varchar2, true, "a ", true},
		{"blank string to varchar", localValue{kind: localStringKind, s: []byte("     ")}, varchar2, true, "  ", true},
		{"non-blank beyond length", localValue{kind: localStringKind, s: []byte("a b")}, varchar2, true, nil, false},
		{"padded string to char", localValue{kind: localStringKind, s: []byte("abc      ")}, char5, true, "abc  ", true},
		{"string target not local", localValue{kind: localStringKind, s: []byte("ab")}, varchar2, false, nil, false},
		{"string to integer", localValue{kind: localStringKind, s: []byte("1")}, integer, true, nil, false},
		{"int to varchar", localValue{kind: localIntKind, i: 1}, varchar2, true, nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := convertLocal(tt.value, tt.typ, tt.strings)
			if ok != tt.ok || ok && !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("convertLocal() = %#v, %v; want %#v, %v", got, ok, tt.want, tt.ok)
			}
		})
	}
}

func TestLineProfileRoundTripClassification(t *testing.T) {
	before := RoundTripBaseline{Min: 10 * time.Millisecond, Median: 12 * time.Millisecond, Samples: 15}
	tests := []struct {
		d         time.Duration
		roundTrip bool
		adjusted  time.Duration
	}{
		{4 * time.Millisecond, false, 4 * time.Millisecond},
		{5 * time.Millisecond, true, 0},
		{20 * time.Millisecond, true, 8 * time.Millisecond},
		{30 * time.Microsecond, false, 30 * time.Microsecond},
	}
	for _, tt := range tests {
		roundTrip, adjusted := classifyServerCall(before, tt.d)
		if roundTrip != tt.roundTrip || adjusted != tt.adjusted {
			t.Fatalf("classifyServerCall(%v) = %v, %v; want %v, %v", tt.d, roundTrip, adjusted, tt.roundTrip, tt.adjusted)
		}
	}
}

func TestLineProfileBaseline(t *testing.T) {
	samples := []time.Duration{9, 3, 7, 5, 1}
	if got := baselineOf(samples); got != (RoundTripBaseline{Min: 1, Median: 5, Samples: 5}) {
		t.Fatalf("baselineOf(odd) = %+v", got)
	}
	if got := baselineOf([]time.Duration{4, 2, 8, 6}); got != (RoundTripBaseline{Min: 2, Median: 5, Samples: 4}) {
		t.Fatalf("baselineOf(even) = %+v", got)
	}
	if got := baselineOf(nil); got != (RoundTripBaseline{}) {
		t.Fatalf("baselineOf(nil) = %+v", got)
	}
}

func TestLineProfileLineAggregation(t *testing.T) {
	routine := LineProfileRoutine{
		Nodes: []LineProfileNode{
			{ID: 0, Kind: "block", Line: 1, Metrics: LineMetrics{Hits: 1}},
			{ID: 1, Kind: "assign", Line: 3, Metrics: LineMetrics{Hits: 2, Raw: 5, Adjusted: 3, ServerCalls: 1,
				RoundTrips: 1, Pages: &PageCounters{Reads: 1}, Tables: map[int]TableCounters{128: {SeqReads: 2}}}},
			{ID: 2, Kind: "if", Line: 3, Metrics: LineMetrics{Hits: 1, Raw: 7, Adjusted: 7, ServerCalls: 2, Rows: 4,
				Pages: &PageCounters{Reads: 2, Fetches: 3}, Tables: map[int]TableCounters{128: {SeqReads: 1}, 129: {Inserts: 1}}}},
			{ID: 3, Kind: "assign", Line: 5},
		},
	}
	aggregateRoutine(&routine)
	if len(routine.Lines) != 2 {
		t.Fatalf("Lines keys = %v, want lines 1 and 3 only", routine.Lines)
	}
	line := routine.Lines[3]
	want := LineMetrics{Hits: 3, Raw: 12, Adjusted: 10, ServerCalls: 3, RoundTrips: 1, Rows: 4,
		Pages:  &PageCounters{Reads: 3, Fetches: 3},
		Tables: map[int]TableCounters{128: {SeqReads: 3}, 129: {Inserts: 1}}}
	if !reflect.DeepEqual(line, want) {
		t.Fatalf("Lines[3] = %+v, want %+v", line, want)
	}
	if routine.Exclusive.Hits != 4 || routine.Exclusive.Raw != 12 || !reflect.DeepEqual(routine.Inclusive, routine.Exclusive) {
		t.Fatalf("Exclusive/Inclusive = %+v / %+v", routine.Exclusive, routine.Inclusive)
	}
	// Aggregation must not alias node maps.
	routine.Lines[3].Tables[128] = TableCounters{}
	if routine.Nodes[1].Metrics.Tables[128].SeqReads != 2 {
		t.Fatal("line aggregation aliases node table counters")
	}
}

func TestLineProfileFirstHitAggregation(t *testing.T) {
	routines := []LineProfileRoutine{{
		Parent: -1,
		Nodes: []LineProfileNode{
			{ID: 0, Kind: "assign", Line: 3, Metrics: LineMetrics{Hits: 2, Raw: 5, Adjusted: 3, FirstRaw: 4, FirstAdjusted: 2}},
			{ID: 1, Kind: "if", Line: 3, Metrics: LineMetrics{Hits: 1, Raw: 7, Adjusted: 6, FirstRaw: 7, FirstAdjusted: 6}},
		},
	}, {
		Parent: 0,
		Nodes:  []LineProfileNode{{ID: 0, Kind: "assign", Line: 2, Metrics: LineMetrics{Hits: 1, Raw: 1, Adjusted: 1, FirstRaw: 1, FirstAdjusted: 1}}},
	}}
	aggregateRoutines(routines)
	if line := routines[0].Lines[3]; line.FirstRaw != 11 || line.FirstAdjusted != 8 {
		t.Fatalf("Lines[3] first hit = %v/%v, want the sum of its nodes' first hits 11/8", line.FirstRaw, line.FirstAdjusted)
	}
	for i, r := range routines {
		for name, m := range map[string]LineMetrics{"exclusive": r.Exclusive, "inclusive": r.Inclusive} {
			if m.FirstRaw != 0 || m.FirstAdjusted != 0 {
				t.Fatalf("routine %d %s first hit = %v/%v, want zero", i, name, m.FirstRaw, m.FirstAdjusted)
			}
		}
	}
	if routines[0].Inclusive.Raw != 13 {
		t.Fatalf("inclusive raw = %v", routines[0].Inclusive.Raw)
	}
}

func TestLineProfileFirstHitMetrics(t *testing.T) {
	const slow = 30 * time.Millisecond
	rows, updates := 3, 0
	run, _ := newLineProfileFakeRun(t, context.Background(), lineProfileFakeBody,
		func() (bool, error) { rows--; return rows >= 0, nil },
		func(string) (int64, error) {
			if updates++; updates == 1 {
				time.Sleep(slow)
			}
			return 1, nil
		})
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	update := run.result.Routines[0].Nodes[2].Metrics
	if update.Hits != 3 || update.FirstRaw < slow || update.FirstAdjusted < slow || update.Raw-update.FirstRaw >= slow {
		t.Fatalf("UPDATE hits %d raw %v first raw %v first adjusted %v; want the first hit's time only",
			update.Hits, update.Raw, update.FirstRaw, update.FirstAdjusted)
	}
	loop := run.result.Routines[0].Nodes[1].Metrics
	if loop.Hits != 1 || loop.FirstRaw != loop.Raw || loop.FirstAdjusted != loop.Adjusted {
		t.Fatalf("FOR SELECT hit once: first %v/%v, total %v/%v", loop.FirstRaw, loop.FirstAdjusted, loop.Raw, loop.Adjusted)
	}
}

func TestLineProfileSQLTextRewrites(t *testing.T) {
	text := "SELECT 1 FROM T WHERE A = ? AND B = '?' AND /* ? */ C = ? AND \"?\" = 1"
	got := castParams(text, []string{"VARCHAR(10)", "INTEGER"})
	want := "SELECT 1 FROM T WHERE A = CAST(? AS VARCHAR(10)) AND B = '?' AND /* ? */ C = CAST(? AS INTEGER) AND \"?\" = 1"
	if got != want {
		t.Fatalf("castParams() = %q, want %q", got, want)
	}
	if got := castParams("A = ? AND B = ?", []string{"", "INTEGER"}); got != "A = ? AND B = CAST(? AS INTEGER)" {
		t.Fatalf("castParams(skip) = %q", got)
	}

	if got := unknownColonNames("A = :X AND ':Y' = :Z", map[string]bool{"X": true}); !reflect.DeepEqual(got, []string{"Z"}) {
		t.Fatalf("unknownColonNames() = %v", got)
	}
}

func TestLineProfileParameterTypeText(t *testing.T) {
	i := func(v int64) any { return v }
	tests := []struct {
		name    string
		field   lpField
		dialect int
		want    string
	}{
		{"integer", lpField{typ: 8}, 3, "INTEGER"},
		{"smallint", lpField{typ: 7}, 1, "SMALLINT"},
		{"numeric int", lpField{typ: 8, subType: i(1), scale: i(-2), precision: i(9)}, 3, "NUMERIC(9,2)"},
		{"bigint", lpField{typ: 16}, 3, "NUMERIC(18,0)"},
		{"dialect 1 numeric", lpField{typ: 27, scale: i(-2), precision: i(15)}, 1, "NUMERIC(15,2)"},
		{"double", lpField{typ: 27}, 3, "DOUBLE PRECISION"},
		{"dialect 1 date", lpField{typ: 35}, 1, "DATE"},
		{"timestamp", lpField{typ: 35}, 3, "TIMESTAMP"},
		{"date", lpField{typ: 12}, 3, "DATE"},
		{"char none", lpField{typ: 14, length: 3, charLength: i(3), charset: "NONE"}, 3, "CHAR(3) CHARACTER SET NONE"},
		{"varchar no charset", lpField{typ: 37, length: 20}, 3, "VARCHAR(20)"},
		{"blob", lpField{typ: 261, subType: i(1)}, 3, "BLOB SUB_TYPE 1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.field.typeText(tt.dialect)
			if err != nil || got != tt.want {
				t.Fatalf("typeText() = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
	if _, err := (lpField{typ: 9}).typeText(3); err == nil {
		t.Fatal("QUAD accepted")
	}
}

func TestLineProfileStringsLocal(t *testing.T) {
	if !stringsLocal("VARCHAR(3)", true) || stringsLocal("VARCHAR(3)", false) {
		t.Fatal("default charset rule")
	}
	if !stringsLocal("varchar(3)  character   set none", false) {
		t.Fatal("explicit NONE")
	}
	if stringsLocal("VARCHAR(3) CHARACTER SET WIN1252", true) || stringsLocal("INTEGER", true) {
		t.Fatal("non-NONE or non-string accepted")
	}
}

func TestLineProfileCheckRoutine(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  X = 1;\n" +
		"  SELECT A FROM T WHERE B = :NOPE INTO :X;\n" +
		"  IF (SQLCODE = 0) THEN X = 2;\n" +
		"  Y = 3;\n" +
		"  EXCEPTION E_ONE;\n" +
		"  BEGIN X = 4; WHEN ANY DO X = 5; END\n" +
		"  EXECUTE PROCEDURE P(:MISSING) RETURNING_VALUES :Z;\n" +
		"  EXCEPTION E_KNOWN;\n" +
		"  BEGIN X = 6;\n" +
		"  WHEN EXCEPTION E_KNOWN, SQLCODE -803, GDSCODE isc_no_dup DO X = 7;\n" +
		"  WHEN EXCEPTION E_TWO DO X = 8;\n" +
		"  WHEN GDSCODE not_a_code DO X = 9;\n" +
		"  END\n" +
		"  EXCEPTION E_KNOWN 'boom';\n" +
		"END"
	routine, problems := psql.ParseRoutine(body, psql.ParseOptions{Dialect: 3, Params: []string{"P_IN"}})
	if len(problems) != 0 {
		t.Fatalf("parse problems: %+v", problems)
	}
	meta := &lpRoutineMeta{name: "LP_CHECK", routine: routine, inputs: []lpParam{{name: "P_IN", typeText: "INTEGER"}}}
	got := map[int][]string{}
	for _, p := range checkRoutine(meta, map[string]lpException{"E_KNOWN": {number: 1}}) {
		got[p.Line] = append(got[p.Line], p.Message)
	}
	want := map[int][]string{
		4:  {"unknown variable :NOPE"},
		6:  {"unknown target Y"},
		7:  {"unknown exception E_ONE"},
		9:  {"unknown target Z", "unknown variable :MISSING"},
		13: {"unknown exception E_TWO"},
		14: {"unsupported GDSCODE not_a_code"},
		16: {"EXCEPTION with a message is not supported (InterBase refuses it)"},
	}
	for line := range want {
		sortStrings(got[line])
		sortStrings(want[line])
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("checkRoutine() = %v, want %v", got, want)
	}
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

type lineProfileFakeCounts struct {
	prepared, closed, queried, cursorsClosed  int
	closedAtRollback, cursorsClosedAtRollback int
	rolledBack                                bool
	onPrepare, onCursorClose                  func()       // optional server-side effects of a prepare and of a cursor close
	rowValue                                  driver.Value // fetched column value; nil means int64(1)
	// Optional per-statement behavior: valueFor overrides the fetched value,
	// outputsFor the output descriptors and prepareErr fails the prepare;
	// prepares counts prepares by statement text.
	valueFor   func(query string) driver.Value
	outputsFor func(query string) []OutputDescriptor
	prepareErr func(query string) error
	onClose    func(query string) error // result of closing a statement
	prepares   map[string]int
}

// lineProfileFakeNull, returned by valueFor, makes the fake fetch a NULL.
type lineProfileFakeNull struct{}

func lineProfileAllPagesInfo([]byte) ([]byte, error) {
	return infoItemsFrame(infoItemRecord(5, 0, 0, 0, 0), infoItemRecord(6, 0, 0, 0, 0),
		infoItemRecord(7, 0, 0, 0, 0), infoItemRecord(8, 0, 0, 0, 0)), nil
}

// newLineProfileFakeRun builds a run over fake native handles: SELECT
// statements return rows from next, other statements run exec.
func newLineProfileFakeRun(t *testing.T, ctx context.Context, body string,
	next func() (bool, error), exec func(query string) (int64, error)) (*lpRun, *lineProfileFakeCounts) {
	t.Helper()
	counts := &lineProfileFakeCounts{}
	native := &nativeTransaction{
		cancelSlotOverride: &fakeCancelSlot{},
		commitOverride:     func() error { return nil },
		rollbackOverride: func() error {
			counts.rolledBack = true
			counts.closedAtRollback, counts.cursorsClosedAtRollback = counts.closed, counts.cursorsClosed
			return nil
		},
		handleStateOverride: func() nativeHandleState { return nativeHandleConsumed },
		freeOverride:        func() {},
		prepareContextOverride: func(query string, _ bool, _ *nativeCancelOperation) (*nativeStatement, error) {
			if counts.prepareErr != nil {
				if err := counts.prepareErr(query); err != nil {
					return nil, err
				}
			}
			counts.prepared++
			if counts.prepares == nil {
				counts.prepares = make(map[string]int)
			}
			counts.prepares[query]++
			if counts.onPrepare != nil {
				counts.onPrepare()
			}
			isSelect := strings.HasPrefix(strings.ToUpper(query), "SELECT")
			inputs := strings.Count(query, "?")
			statement := &nativeStatement{
				descriptionOverride: func() (StatementDescriptor, error) {
					kind := "update"
					if isSelect {
						kind = "select"
					} else if strings.HasPrefix(strings.ToUpper(query), "EXECUTE PROCEDURE") {
						kind = "procedure"
					} else if strings.HasPrefix(strings.ToUpper(query), "CREATE ") {
						kind = "ddl"
					} else if upper := strings.ToUpper(query); strings.HasPrefix(upper, "COMMIT") ||
						strings.HasPrefix(upper, "ROLLBACK") || strings.HasPrefix(upper, "SET ") {
						kind = "unsupported"
					}
					return StatementDescriptor{Kind: kind, ReturnsRows: isSelect, InputCount: inputs}, nil
				},
				numInputOverride:      func() int { return inputs },
				inputMetadataOverride: func(int) (nativeInputMetadata, error) { return nativeInputMetadata{}, nil },
				outputsOverride: func() ([]OutputDescriptor, error) {
					if !isSelect {
						return nil, nil
					}
					if counts.outputsFor != nil {
						if outputs := counts.outputsFor(query); outputs != nil {
							return outputs, nil
						}
					}
					if _, blob := counts.rowValue.(BlobRef); blob {
						return []OutputDescriptor{{Name: "ID", Kind: "BLOB", SubType: 1, Nullable: true}}, nil
					}
					return []OutputDescriptor{{Name: "ID", Kind: "INTEGER", Nullable: true}}, nil
				},
				queryOverride: func([]argument) (*nativeCursor, []string, error) {
					counts.queried++
					closeCursor := func() error {
						counts.cursorsClosed++
						if counts.onCursorClose != nil {
							counts.onCursorClose()
						}
						return nil
					}
					// valueFor is asked once per fetched row.
					var current driver.Value
					return &nativeCursor{
						nextOverride: func() (bool, error) {
							hasRow, err := next()
							if current = nil; hasRow && counts.valueFor != nil {
								current = counts.valueFor(query)
							}
							return hasRow, err
						},
						indicatorOverride: func(int) (uint16, error) {
							if _, null := current.(lineProfileFakeNull); null {
								return nullIndicator, nil
							}
							return 0, nil
						},
						valueOverride: func(int) (driver.Value, error) {
							if _, null := current.(lineProfileFakeNull); null {
								return nil, nil
							}
							if current != nil {
								return current, nil
							}
							if counts.rowValue != nil {
								return counts.rowValue, nil
							}
							return int64(1), nil
						},
						closeOverride: closeCursor,
						abortOverride: closeCursor,
					}, []string{"ID"}, nil
				},
				execOverride: func([]argument) (int64, error) { return exec(query) },
				closeOverride: func() error {
					counts.closed++
					if counts.onClose != nil {
						return counts.onClose(query)
					}
					return nil
				},
			}
			return statement, nil
		},
	}
	connection := &conn{native: &nativeConnection{
		brokenOverride:            func() bool { return false },
		databaseInfoItemsOverride: lineProfileAllPagesInfo,
	}}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{attachment: attachment, native: native, generation: 1, cursors: make(map[*Cursor]struct{})}
	attachment.directTx = tx

	routine, problems := psql.ParseRoutine(body, psql.ParseOptions{Dialect: 3})
	if len(problems) != 0 {
		t.Fatalf("parse problems: %+v", problems)
	}
	meta := &lpRoutineMeta{name: "LP_FAKE", routine: routine}
	plan := &lpPlan{dialect: 3, entry: meta, procs: map[string]*lpRoutineMeta{"LP_FAKE": meta}}
	plan.analysis.Entry = psql.EntryCall{Procedure: "LP_FAKE", Selectable: true}
	result := &LineProfileResult{Routines: []LineProfileRoutine{newRoutineProfile(meta)}}
	return &lpRun{
		ctx: ctx, conn: connection, tx: tx, plan: plan, opts: LineProfileOptions{KeepRows: 10},
		result: result, statements: make(map[string]cachedStatement),
	}, counts
}

const lineProfileFakeBody = "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
	"  FOR SELECT ID FROM LP_T INTO :X DO\n" +
	"    UPDATE LP_T SET A = :X;\n" +
	"END"

func assertLineProfileFakeClosed(t *testing.T, run *lpRun, counts *lineProfileFakeCounts) {
	t.Helper()
	if !counts.rolledBack || !run.result.RolledBack {
		t.Fatal("transaction was not rolled back")
	}
	if counts.prepared == 0 || counts.closedAtRollback != counts.prepared || counts.closed != counts.prepared {
		t.Fatalf("statements prepared %d, closed before rollback %d, closed %d", counts.prepared, counts.closedAtRollback, counts.closed)
	}
	if counts.queried == 0 || counts.cursorsClosedAtRollback != counts.queried {
		t.Fatalf("cursors opened %d, closed before rollback %d", counts.queried, counts.cursorsClosedAtRollback)
	}
	if len(run.statements) != 0 || len(run.tx.cursors) != 0 || len(run.tx.prepared) != 0 {
		t.Fatalf("run keeps statements %d, cursors %d, prepared %d", len(run.statements), len(run.tx.cursors), len(run.tx.prepared))
	}
}

func TestLineProfileFailedRunClosesStatementsAndCursors(t *testing.T) {
	calls := 0
	run, counts := newLineProfileFakeRun(t, context.Background(), lineProfileFakeBody,
		func() (bool, error) { return true, nil },
		func(string) (int64, error) {
			if calls++; calls == 2 {
				return 0, &Error{Operation: "exec", SQLCode: -803, Message: "duplicate"}
			}
			return 1, nil
		})
	outcome, txErr := run.runInTransaction()
	var nativeErr *Error
	if txErr != nil || !errors.As(outcome, &nativeErr) || nativeErr.SQLCode != -803 {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	var located *lpNodeError
	if !errors.As(outcome, &located) || located.line != 4 {
		t.Fatalf("failure location = %+v", located)
	}
	assertLineProfileFakeClosed(t, run, counts)
}

func TestLineProfileCanceledRunClosesStatementsAndCursors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	run, counts := newLineProfileFakeRun(t, ctx, lineProfileFakeBody,
		func() (bool, error) { return true, nil },
		func(string) (int64, error) {
			if calls++; calls == 3 {
				cancel()
			}
			return 1, nil
		})
	outcome, txErr := run.runInTransaction()
	if txErr != nil || !errors.Is(outcome, context.Canceled) {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	if hits := run.result.Routines[0].Nodes[2].Metrics.Hits; hits != 3 {
		t.Fatalf("UPDATE hits = %d, want 3 (partial metrics kept)", hits)
	}
	assertLineProfileFakeClosed(t, run, counts)
}

func TestLineProfileCountersUnavailableContinue(t *testing.T) {
	for name, info := range map[string]func([]byte) ([]byte, error){
		"error item": func([]byte) ([]byte, error) {
			return infoItemsFrame(infoItemRecord(infoError, 1, 0)), nil
		},
		"absent page items": func([]byte) ([]byte, error) {
			return infoItemsFrame(infoItemRecord(23, 17, 0, 1, 0, 0, 0)), nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			run, counts := newLineProfileFakeRun(t, context.Background(), lineProfileFakeBody,
				func() (bool, error) { return false, nil },
				func(string) (int64, error) { return 1, nil })
			run.conn.native.databaseInfoItemsOverride = info
			outcome, txErr := run.runInTransaction()
			if outcome != nil || txErr != nil {
				t.Fatalf("runInTransaction() = %v, %v; want the run to continue", outcome, txErr)
			}
			if !run.countersUnavailable {
				t.Fatal("counters not marked unavailable")
			}
			for _, node := range run.result.Routines[0].Nodes {
				if node.Metrics.Pages != nil || node.Metrics.Tables != nil {
					t.Fatalf("node %d keeps counters %+v", node.ID, node.Metrics)
				}
			}
			if forSelect := run.result.Routines[0].Nodes[1].Metrics; forSelect.ServerCalls != 2 {
				t.Fatalf("FOR SELECT server calls = %d, want execute and fetch", forSelect.ServerCalls)
			}
			assertLineProfileFakeClosedWithoutCursorCheck(t, counts)
		})
	}

	run, _ := newLineProfileFakeRun(t, context.Background(), lineProfileFakeBody,
		func() (bool, error) { return false, nil },
		func(string) (int64, error) { return 1, nil })
	run.conn.native.databaseInfoItemsOverride = func([]byte) ([]byte, error) { return nil, errors.New("link down") }
	if outcome, _ := run.runInTransaction(); outcome == nil || run.countersUnavailable {
		t.Fatalf("snapshot failure outcome = %v, unavailable %v; want the run to end", outcome, run.countersUnavailable)
	}
}

func assertLineProfileFakeClosedWithoutCursorCheck(t *testing.T, counts *lineProfileFakeCounts) {
	t.Helper()
	if counts.closed != counts.prepared || counts.cursorsClosed != counts.queried {
		t.Fatalf("statements %d/%d, cursors %d/%d closed", counts.closed, counts.prepared, counts.cursorsClosed, counts.queried)
	}
}

func TestLineProfileNullKeepsDeclaredFamily(t *testing.T) {
	for _, tt := range []struct {
		typeText string
		fam      localFamily
	}{
		{"INTEGER", famInt},
		{"VARCHAR(3) CHARACTER SET NONE", famString},
		{"VARCHAR(3)", famOther},
		{"NUMERIC(15,2)", famOther},
	} {
		typ := psql.ParseTypeText(tt.typeText)
		v := &lpVar{typ: typ, stringsLocal: isStringFamily(typ) && stringsLocal(tt.typeText, false)}
		got, ok := localValueOf(v)
		if !ok || got.kind != localNullKind || got.fam != tt.fam {
			t.Fatalf("localValueOf(NULL %s) = %+v, %v; want family %d", tt.typeText, got, ok, tt.fam)
		}
	}
	scope := localScope{lookup: func(string) (localValue, bool) {
		return localValue{kind: localNullKind, fam: famInt}, true
	}}
	names := map[string]bool{"NI": true}
	if _, ok := evalLocalCondition(psql.AnalyzeExpr("NI = 'a'", 1, 1, names, 3).Local, localScope{lookup: scope.lookup, literalStrings: true}); ok {
		t.Fatal("NULL integer compared with a string locally")
	}
	if got, ok := evalLocalCondition(psql.AnalyzeExpr("NI IS NULL", 1, 1, names, 3).Local, scope); !ok || got != truthTrue {
		t.Fatalf("NI IS NULL = %v, %v", got, ok)
	}
}

func TestLineProfileTypeNormalization(t *testing.T) {
	if normalizeType("numeric( 15 , 2 )") != "NUMERIC(15,2)" || normalizeType("VARCHAR(3)  character set NONE collate X") != "VARCHAR(3) CHARACTER SET NONE" {
		t.Fatal("normalizeType mismatch")
	}
	run := &lpRun{plan: &lpPlan{dialect: 1, dbCharset: "UTF8", charsets: map[int]lpCharset{0: {"NONE", 1}, 4: {"UTF8", 4}}}}
	for _, tt := range []struct {
		d    OutputDescriptor
		want string
	}{
		{OutputDescriptor{Kind: "VARCHAR", Length: 80, SubType: 4}, "VARCHAR(20) CHARACTER SET UTF8"},
		{OutputDescriptor{Kind: "CHAR", Length: 3}, "CHAR(3) CHARACTER SET NONE"},
		{OutputDescriptor{Kind: "INTEGER", Scale: -2}, "NUMERIC(9,2)"},
		{OutputDescriptor{Kind: "SMALLINT"}, "SMALLINT"},
		{OutputDescriptor{Kind: "DOUBLE PRECISION", Scale: -2}, "DOUBLE PRECISION"},
		{OutputDescriptor{Kind: "TIMESTAMP"}, "DATE"},
		{OutputDescriptor{Kind: "ARRAY"}, ""},
	} {
		if got := run.descriptorType(tt.d); got != tt.want {
			t.Fatalf("descriptorType(%+v) = %q, want %q", tt.d, got, tt.want)
		}
	}
	if got := run.declaredType("varchar(20)"); got != "VARCHAR(20) CHARACTER SET UTF8" {
		t.Fatalf("declaredType() = %q", got)
	}
}

// newLineProfileHandlerRun is a fake run whose UPDATE statements fail with
// the errors of fail in turn (nil succeeds); savepoint statements are
// recorded in savepoints.
func newLineProfileHandlerRun(t *testing.T, ctx context.Context, body string, fail ...error) (*lpRun, *[]string) {
	t.Helper()
	return newLineProfileHandlerRunRows(t, ctx, body, func() (bool, error) { return false, nil }, fail...)
}

// newLineProfileHandlerRunRows is newLineProfileHandlerRun whose SELECT
// statements return rows from next.
func newLineProfileHandlerRunRows(t *testing.T, ctx context.Context, body string, next func() (bool, error), fail ...error) (*lpRun, *[]string) {
	t.Helper()
	calls := 0
	run, _ := newLineProfileFakeRun(t, ctx, body, next,
		func(string) (int64, error) {
			calls++
			if calls <= len(fail) && fail[calls-1] != nil {
				return 0, fail[calls-1]
			}
			return 1, nil
		})
	var savepoints []string
	run.tx.native.execOverride = func(query string, _ []argument, _ bool) (int64, error) {
		savepoints = append(savepoints, query)
		return 0, nil
	}
	run.plan.exceptions = map[string]lpException{"E_ONE": {number: 7, message: "boom"}, "E_TWO": {number: 8, message: "two"}}
	run.plan.exceptionNames = map[int32]string{7: "E_ONE", 8: "E_TWO"}
	return run, &savepoints
}

func lineProfileNodeHits(run *lpRun, line int) int64 {
	var hits int64
	for _, node := range run.result.Routines[0].Nodes {
		if node.Line == line && node.Kind != "block" {
			hits += node.Metrics.Hits
		}
	}
	return hits
}

func TestLineProfileWhenHandlers(t *testing.T) {
	duplicate := &Error{Operation: "exec", SQLCode: -803, NativeCode: 335544665, gdsCodes: []int64{335544665}}
	userException := &Error{Operation: "exec", SQLCode: -836, NativeCode: 335544517,
		gdsCodes: []int64{335544517, 335544382}, exceptionNumber: 8, hasException: true}
	guarded := func(when string) string {
		return "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
			"  BEGIN\n" +
			"    UPDATE LP_T SET A = 1;\n" +
			"    X = 5;\n" +
			"  " + when + " DO X = 2;\n" +
			"  END\n" +
			"  UPDATE LP_T SET A = :X;\n" +
			"END"
	}
	for _, tt := range []struct {
		name       string
		when       string
		fail       error
		caught     bool
		savepoints []string
	}{
		{"no error releases", "WHEN ANY", nil, false, []string{"SAVEPOINT LP$SP_0_1", "RELEASE SAVEPOINT LP$SP_0_1"}},
		{"SQLCODE", "WHEN SQLCODE -803", duplicate, true,
			[]string{"SAVEPOINT LP$SP_0_1", "RELEASE SAVEPOINT LP$SP_0_1"}},
		{"GDSCODE", "WHEN SQLCODE -100, GDSCODE unique_key_violation", duplicate, true,
			[]string{"SAVEPOINT LP$SP_0_1", "RELEASE SAVEPOINT LP$SP_0_1"}},
		{"user exception from a callee", "WHEN EXCEPTION E_TWO", userException, true,
			[]string{"SAVEPOINT LP$SP_0_1", "RELEASE SAVEPOINT LP$SP_0_1"}},
		{"other user exception", "WHEN EXCEPTION E_ONE", userException, false, []string{"SAVEPOINT LP$SP_0_1"}},
		{"unmatched", "WHEN SQLCODE -100", duplicate, false, []string{"SAVEPOINT LP$SP_0_1"}},
		{"internal errors are not caught", "WHEN ANY", errors.New("not a server error"), false, []string{"SAVEPOINT LP$SP_0_1"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run, savepoints := newLineProfileHandlerRun(t, context.Background(), guarded(tt.when), tt.fail)
			outcome, txErr := run.runInTransaction()
			if txErr != nil {
				t.Fatalf("transaction error %v", txErr)
			}
			if (outcome == nil) != (tt.caught || tt.fail == nil) {
				t.Fatalf("outcome %v, caught %v", outcome, tt.caught)
			}
			if tt.fail != nil && !tt.caught {
				var located *lpNodeError
				if !errors.As(outcome, &located) || located.line != 4 || !errors.Is(outcome, tt.fail) {
					t.Fatalf("outcome %v located %+v", outcome, located)
				}
			}
			if !reflect.DeepEqual(*savepoints, tt.savepoints) {
				t.Fatalf("savepoints %q, want %q", *savepoints, tt.savepoints)
			}
			if got := lineProfileNodeHits(run, 6); (got == 1) != tt.caught {
				t.Fatalf("handler hits %d, caught %v", got, tt.caught)
			}
			if got := lineProfileNodeHits(run, 5); (got == 1) != (tt.fail == nil) {
				t.Fatalf("statement after the failure hits %d", got)
			}
			if run.result.Savepoints <= 0 {
				t.Fatal("savepoint time not recorded as overhead")
			}
		})
	}
}

func TestLineProfileCanceledErrorIsNotHandled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body := "BEGIN\n  BEGIN\n    UPDATE LP_T SET A = 1;\n  WHEN ANY DO EXIT;\n  END\nEND"
	run, _ := newLineProfileFakeRun(t, ctx, body, func() (bool, error) { return false, nil },
		func(string) (int64, error) {
			cancel()
			return 0, &Error{Operation: "exec", SQLCode: -901, NativeCode: 335544794, gdsCodes: []int64{335544794}}
		})
	run.tx.native.execOverride = func(string, []argument, bool) (int64, error) { return 0, nil }
	outcome, _ := run.runInTransaction()
	if outcome == nil || lineProfileNodeHits(run, 4) != 0 {
		t.Fatalf("outcome %v, handler hits %d; a canceled run must not be handled", outcome, lineProfileNodeHits(run, 4))
	}
	for _, node := range run.result.Routines[0].Nodes {
		if node.Line == 3 && node.Kind != "block" && (node.Metrics.ServerCalls != 1 || node.Metrics.Errors != 0) {
			t.Fatalf("canceled UPDATE metrics %+v; a canceled call is not a node error", node.Metrics)
		}
	}
}

func TestLineProfileExceptionStatement(t *testing.T) {
	run, _ := newLineProfileHandlerRun(t, context.Background(), "BEGIN\n  EXCEPTION E_ONE;\nEND")
	outcome, _ := run.runInTransaction()
	var nativeErr *Error
	var located *lpNodeError
	if !errors.As(outcome, &nativeErr) || !errors.As(outcome, &located) || located.line != 2 {
		t.Fatalf("outcome %v", outcome)
	}
	if number, ok := nativeErr.ExceptionNumber(); !ok || number != 7 || nativeErr.SQLCode != -836 ||
		nativeErr.GDSCode() != 335544517 || nativeErr.Message != "exception 7\nboom" || !strings.Contains(nativeErr.Error(), "E_ONE") {
		t.Fatalf("exception error %#v (%v)", nativeErr, nativeErr)
	}

	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n  BEGIN\n    EXCEPTION E_ONE;\n  WHEN EXCEPTION E_TWO DO X = 1;\n  WHEN EXCEPTION E_ONE DO X = 2;\n  END\nEND"
	run, savepoints := newLineProfileHandlerRun(t, context.Background(), body)
	if outcome, _ := run.runInTransaction(); outcome != nil || lineProfileNodeHits(run, 6) != 1 || lineProfileNodeHits(run, 5) != 0 {
		t.Fatalf("outcome %v, handler hits %d/%d", outcome, lineProfileNodeHits(run, 5), lineProfileNodeHits(run, 6))
	}
	if len(*savepoints) != 0 {
		t.Fatalf("savepoints %q for a block that cannot write", *savepoints)
	}
}

func TestLineProfileSavepointNamesAreUnique(t *testing.T) {
	body := "DECLARE VARIABLE I INTEGER;\nBEGIN\n  I = 0;\n  WHILE (I < 2) DO\n  BEGIN\n    I = I + 1;\n" +
		"    BEGIN\n      UPDATE LP_T SET A = 1;\n    WHEN ANY DO EXIT;\n    END\n  END\nEND"
	run, savepoints := newLineProfileHandlerRun(t, context.Background(), body)
	if outcome, _ := run.runInTransaction(); outcome != nil {
		t.Fatalf("outcome %v", outcome)
	}
	want := []string{"SAVEPOINT LP$SP_0_1", "RELEASE SAVEPOINT LP$SP_0_1", "SAVEPOINT LP$SP_0_2", "RELEASE SAVEPOINT LP$SP_0_2"}
	if !reflect.DeepEqual(*savepoints, want) {
		t.Fatalf("savepoints %q, want %q", *savepoints, want)
	}
}

// TestLineProfileBlockUndo checks InterBase's savepoint scopes: a nested
// block that an error leaves is undone, the handled block's own work is kept.
func TestLineProfileBlockUndo(t *testing.T) {
	body := "BEGIN\n" +
		"  BEGIN\n" +
		"    UPDATE LP_T SET A = 1;\n" +
		"    BEGIN\n" +
		"      UPDATE LP_T SET A = 2;\n" +
		"      EXCEPTION E_ONE;\n" +
		"    END\n" +
		"    BEGIN\n" +
		"      EXIT;\n" +
		"    END\n" +
		"  WHEN ANY DO EXIT;\n" +
		"  END\n" +
		"END"
	run, savepoints := newLineProfileHandlerRun(t, context.Background(), body)
	if outcome, _ := run.runInTransaction(); outcome != nil || lineProfileNodeHits(run, 11) != 1 {
		t.Fatalf("outcome %v, handler hits %d", outcome, lineProfileNodeHits(run, 11))
	}
	want := []string{"SAVEPOINT LP$SP_0_1", "SAVEPOINT LP$SP_0_2", "ROLLBACK TO SAVEPOINT LP$SP_0_2", "RELEASE SAVEPOINT LP$SP_0_1"}
	if !reflect.DeepEqual(*savepoints, want) {
		t.Fatalf("savepoints %q, want %q", *savepoints, want)
	}

	// An error raised by a handler leaves the block: it is undone when an
	// enclosing handler can catch the error.
	body = "BEGIN\n" +
		"  BEGIN\n" +
		"    BEGIN\n" +
		"      UPDATE LP_T SET A = 1;\n" +
		"      EXCEPTION E_ONE;\n" +
		"    WHEN ANY DO EXCEPTION E_TWO;\n" +
		"    END\n" +
		"  WHEN EXCEPTION E_TWO DO EXIT;\n" +
		"  END\n" +
		"END"
	run, savepoints = newLineProfileHandlerRun(t, context.Background(), body)
	if outcome, _ := run.runInTransaction(); outcome != nil || lineProfileNodeHits(run, 8) != 1 {
		t.Fatalf("outcome %v, outer handler hits %d", outcome, lineProfileNodeHits(run, 8))
	}
	want = []string{"SAVEPOINT LP$SP_0_1", "SAVEPOINT LP$SP_0_2", "ROLLBACK TO SAVEPOINT LP$SP_0_2", "RELEASE SAVEPOINT LP$SP_0_1"}
	if !reflect.DeepEqual(*savepoints, want) {
		t.Fatalf("savepoints %q, want %q", *savepoints, want)
	}
}

// TestLineProfileHandlerOrder checks InterBase's handler selection: the
// first matching handler, then every later ANY handler; the last handler's
// signal wins.
func TestLineProfileHandlerOrder(t *testing.T) {
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  BEGIN\n" +
		"    EXCEPTION E_ONE;\n" +
		"  WHEN EXCEPTION E_TWO DO X = 1;\n" +
		"  WHEN SQLCODE -836 DO EXIT;\n" +
		"  WHEN EXCEPTION E_ONE DO X = 3;\n" +
		"  WHEN GDSCODE except, ANY DO X = 4;\n" +
		"  END\n" +
		"  X = 5;\n" +
		"END"
	run, _ := newLineProfileHandlerRun(t, context.Background(), body)
	if outcome, _ := run.runInTransaction(); outcome != nil {
		t.Fatalf("outcome %v", outcome)
	}
	for line, want := range map[int]int64{5: 0, 6: 1, 7: 0, 8: 1, 10: 1} {
		if got := lineProfileNodeHits(run, line); got != want {
			t.Fatalf("line %d hits %d, want %d", line, got, want)
		}
	}
}

func TestLineProfileSelectItemCasts(t *testing.T) {
	for _, tt := range []struct {
		text  string
		types []string
		want  string // "" when not rewritten
		cast  []bool
	}{
		{"SELECT T40 FROM LP_TEXTS WHERE ID <= ? ORDER BY ID", []string{"VARCHAR(8)"},
			"SELECT CAST(T40 AS VARCHAR(8)) FROM LP_TEXTS WHERE ID <= ? ORDER BY ID", []bool{true}},
		{"select a.P4, S, sum(x * (y + 1)) from T a", []string{"NUMERIC(15,2)", "", "NUMERIC(15,2)"},
			"select CAST(a.P4 AS NUMERIC(15,2)), S, CAST(sum(x * (y + 1)) AS NUMERIC(15,2)) from T a", []bool{true, false, true}},
		{"SELECT 'a,b', (SELECT MAX(ID) FROM T2 GROUP BY ID) FROM T", []string{"CHAR(3)", "INTEGER"},
			"SELECT CAST('a,b' AS CHAR(3)), CAST((SELECT MAX(ID) FROM T2 GROUP BY ID) AS INTEGER) FROM T", []bool{true, true}},
		{"SELECT ALL N FROM T", []string{"INTEGER"}, "SELECT ALL CAST(N AS INTEGER) FROM T", []bool{true}},
		{"SELECT N AS M, N M2, N FROM T", []string{"INTEGER", "INTEGER", "INTEGER"},
			"SELECT CAST(N AS INTEGER) AS M, CAST(N AS INTEGER) M2, CAST(N AS INTEGER) FROM T", []bool{true, true, true}},
		{"SELECT N AS M, N  M2, F(N) \"Q\", N AS \"R S\", T.\"N\", \"N\", 'a' \"B\" FROM T",
			[]string{"INTEGER", "INTEGER", "INTEGER", "INTEGER", "INTEGER", "INTEGER", "CHAR(1)"},
			"SELECT CAST(N AS INTEGER) AS M, CAST(N AS INTEGER)  M2, CAST(F(N) AS INTEGER) \"Q\", CAST(N AS INTEGER) AS \"R S\", " +
				"CAST(T.\"N\" AS INTEGER), CAST(\"N\" AS INTEGER), CAST('a' AS CHAR(1)) \"B\" FROM T",
			[]bool{true, true, true, true, true, true, true}},
		{"SELECT CASE WHEN A > 1 THEN 'x' ELSE 'y' END, CASE A WHEN 1 THEN 'x' END C, UPPER(B) FROM T",
			[]string{"VARCHAR(2)", "VARCHAR(2)", "VARCHAR(6)"},
			"SELECT CAST(CASE WHEN A > 1 THEN 'x' ELSE 'y' END AS VARCHAR(2)), CAST(CASE A WHEN 1 THEN 'x' END AS VARCHAR(2)) C, CAST(UPPER(B) AS VARCHAR(6)) FROM T",
			[]bool{true, true, true}},
		{"SELECT B COLLATE PXW_INTL, B FROM T", []string{"VARCHAR(2)", "VARCHAR(2)"},
			"SELECT B COLLATE PXW_INTL, CAST(B AS VARCHAR(2)) FROM T", []bool{false, true}},
		{"SELECT FIRST 1 N FROM T", []string{"INTEGER"}, "SELECT FIRST 1 CAST(N AS INTEGER) FROM T", []bool{true}},
		{"select first 2 skip ? N, M from T", []string{"INTEGER", ""}, "select first 2 skip ? CAST(N AS INTEGER), M from T", []bool{true, false}},
		{"SELECT SKIP 3 ALL N FROM T", []string{"INTEGER"}, "SELECT SKIP 3 ALL CAST(N AS INTEGER) FROM T", []bool{true}},
		{"SELECT N FROM T ORDER BY N ROWS 1 TO 5", []string{"INTEGER"}, "SELECT CAST(N AS INTEGER) FROM T ORDER BY N ROWS 1 TO 5", []bool{true}},
		{"SELECT FIRST (1 + 1) N FROM T", []string{"INTEGER"}, "", nil},
		{"SELECT FIRST 1 DISTINCT N FROM T", []string{"INTEGER"}, "", nil},
		{"SELECT DISTINCT N FROM T", []string{"INTEGER"}, "", nil},
		{"SELECT N FROM T UNION SELECT N FROM U", []string{"INTEGER"}, "", nil},
		{"SELECT N, COUNT(*) FROM T GROUP BY N", []string{"INTEGER", "INTEGER"}, "", nil},
		{"SELECT N FROM T HAVING N > 1", []string{"INTEGER"}, "", nil},
		{"SELECT N FROM T ORDER BY 1", []string{"INTEGER"}, "", nil},
		{"SELECT N FROM T ORDER BY M, 1", []string{"INTEGER"}, "", nil},
		{"SELECT * FROM T", []string{"INTEGER"}, "", nil},
		{"SELECT T.* FROM T", []string{"INTEGER"}, "", nil},
		{"SELECT N FROM T", []string{""}, "", nil},
		{"SELECT N, M FROM T", []string{"INTEGER"}, "", nil},
	} {
		got, cast := castSelectItems(tt.text, tt.types)
		if got != tt.want || !reflect.DeepEqual(cast, tt.cast) {
			t.Fatalf("castSelectItems(%q) = %q %v, want %q %v", tt.text, got, cast, tt.want, tt.cast)
		}
	}
}

func TestLineProfileDeclaredTypeDefaultCharset(t *testing.T) {
	run := &lpRun{plan: &lpPlan{dialect: 3}}
	if a, b := run.declaredType("VARCHAR(10)"), run.declaredType("VARCHAR(10) CHARACTER SET NONE"); a != b {
		t.Fatalf("NONE database: %q != %q", a, b)
	}
	run.plan.dbCharset = "WIN1252"
	if a, b := run.declaredType("varchar(10)"), run.declaredType("VARCHAR(10) CHARACTER SET WIN1252"); a != b {
		t.Fatalf("WIN1252 database: %q != %q", a, b)
	}
	if run.declaredType("INTEGER") != "INTEGER" {
		t.Fatal("non-text type changed")
	}
}

// lineProfileFakeActivity is a fake attachment whose relation index-read
// counters the test bumps to simulate server work.
type lineProfileFakeActivity struct{ idxReads map[uint16]uint32 }

func (a *lineProfileFakeActivity) info([]byte) ([]byte, error) {
	var relations []byte
	for id, count := range a.idxReads {
		relations = append(relations, byte(id), byte(id>>8), byte(count), byte(count>>8), byte(count>>16), byte(count>>24))
	}
	records := [][]byte{infoItemRecord(5, 0, 0, 0, 0), infoItemRecord(6, 0, 0, 0, 0),
		infoItemRecord(7, 0, 0, 0, 0), infoItemRecord(8, 0, 0, 0, 0)}
	if len(relations) > 0 {
		records = append(records, infoItemRecord(24, relations...))
	}
	return infoItemsFrame(records...), nil
}

func TestLineProfilePrepareActivityIsOverhead(t *testing.T) {
	const metadata, user = 50, 128
	activity := &lineProfileFakeActivity{idxReads: map[uint16]uint32{}}
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  UPDATE LP_T SET A = 1;\n" +
		"  UPDATE LP_T SET A = 2;\n" +
		"  UPDATE LP_T SET A = 1;\n" +
		"END"
	run, counts := newLineProfileFakeRun(t, context.Background(), body,
		func() (bool, error) { return false, nil },
		func(string) (int64, error) { activity.idxReads[user]++; return 1, nil })
	counts.onPrepare = func() { activity.idxReads[metadata] += 810 }
	run.conn.native.databaseInfoItemsOverride = activity.info
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	for _, node := range run.result.Routines[0].Nodes {
		if node.Kind != "update" {
			continue
		}
		want := map[int]TableCounters{user: {IdxReads: 1}}
		if !reflect.DeepEqual(node.Metrics.Tables, want) {
			t.Fatalf("UPDATE line %d tables = %+v, want only the statement's own read %+v", node.Line, node.Metrics.Tables, want)
		}
	}
	if counts.prepared != 3 {
		t.Fatalf("prepared %d statements, want 3 (one per node)", counts.prepared)
	}
	if got := run.result.OverheadActivity.Tables[metadata].IdxReads; got != 3*810 {
		t.Fatalf("OverheadActivity metadata idx reads = %d, want %d (%+v)", got, 3*810, run.result.OverheadActivity)
	}
	if _, ok := run.result.OverheadActivity.Tables[user]; ok {
		t.Fatalf("OverheadActivity holds node work: %+v", run.result.OverheadActivity)
	}
}

func TestLineProfileEarlyCursorCloseIsNotTheNextNodes(t *testing.T) {
	const closing, user = 60, 128
	activity := &lineProfileFakeActivity{idxReads: map[uint16]uint32{}}
	body := "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n" +
		"    BREAK;\n" +
		"  UPDATE LP_T SET A = 1;\n" +
		"END"
	run, counts := newLineProfileFakeRun(t, context.Background(), body,
		func() (bool, error) { return true, nil },
		func(string) (int64, error) { activity.idxReads[user]++; return 1, nil })
	counts.onCursorClose = func() { activity.idxReads[closing]++ }
	run.conn.native.databaseInfoItemsOverride = activity.info
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	for _, node := range run.result.Routines[0].Nodes {
		if node.Kind == "update" && !reflect.DeepEqual(node.Metrics.Tables, map[int]TableCounters{user: {IdxReads: 1}}) {
			t.Fatalf("UPDATE tables = %+v; the FOR SELECT cursor close leaked into it", node.Metrics.Tables)
		}
	}
	if got := run.result.OverheadActivity.Tables[closing].IdxReads; got != 1 {
		t.Fatalf("OverheadActivity cursor-close reads = %d, want 1", got)
	}
}

func TestLineProfileFailedSavepointActivityIsOverhead(t *testing.T) {
	const backouts = 70
	activity := &lineProfileFakeActivity{idxReads: map[uint16]uint32{}}
	run, _ := newLineProfileFakeRun(t, context.Background(), lineProfileFakeBody,
		func() (bool, error) { return false, nil },
		func(string) (int64, error) { return 1, nil })
	run.conn.native.databaseInfoItemsOverride = activity.info
	run.tx.native.execOverride = func(query string, _ []argument, _ bool) (int64, error) {
		activity.idxReads[backouts] += 4
		return 0, &Error{Operation: "exec", SQLCode: -901, Message: query + " failed"}
	}
	if err := run.snapshot(nil); err != nil {
		t.Fatalf("baseline snapshot: %v", err)
	}
	err := run.savepoint("ROLLBACK TO SAVEPOINT LP_SP")
	var fatalErr *lpFatalError
	if !errors.As(err, &fatalErr) {
		t.Fatalf("savepoint() error = %v, want a fatal run error", err)
	}
	if got := run.result.OverheadActivity.Tables[backouts].IdxReads; got != 4 {
		t.Fatalf("OverheadActivity = %+v; the failed savepoint's counters were not snapshotted", run.result.OverheadActivity)
	}
	if run.activity.Tables[backouts].IdxReads != 4 {
		t.Fatalf("snapshot baseline not advanced past the failed savepoint: %+v", run.activity.Tables)
	}
}

type lineProfileFakeBlobReader struct {
	*strings.Reader
	onRead func()
}

func (r lineProfileFakeBlobReader) Read(p []byte) (int, error) {
	r.onRead()
	return r.Reader.Read(p)
}

func (lineProfileFakeBlobReader) Close() error { return nil }

func TestLineProfileBlobReadsAreOverhead(t *testing.T) {
	const blobPages, user = 90, 128
	activity := &lineProfileFakeActivity{idxReads: map[uint16]uint32{}}
	body := "DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;\nBEGIN\n" +
		"  FOR SELECT ID FROM LP_T INTO :X DO\n" +
		"    UPDATE LP_T SET A = :X;\n" +
		"END"
	rows := 2
	run, counts := newLineProfileFakeRun(t, context.Background(), body,
		func() (bool, error) { rows--; activity.idxReads[user]++; return rows >= 0, nil },
		func(string) (int64, error) { activity.idxReads[user]++; return 1, nil })
	run.conn.native.databaseInfoItemsOverride = activity.info
	counts.rowValue = BlobRef{attachment: run.tx.attachment, tx: run.tx, generation: run.tx.generation,
		high: 1, low: 2, subtype: 1}
	var bound []any
	run.tx.native.prepareContextOverride = wrapFakePrepare(run.tx.native.prepareContextOverride, &bound)
	opened := 0
	run.openBlob = func(BlobRef) (io.ReadCloser, error) {
		opened++
		return lineProfileFakeBlobReader{Reader: strings.NewReader("hello blob"),
			onRead: func() { activity.idxReads[blobPages]++ }}, nil
	}
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	if opened != 2 {
		t.Fatalf("BLOB opened %d times, want once per fetched row", opened)
	}
	for _, node := range run.result.Routines[0].Nodes {
		if _, leaked := node.Metrics.Tables[blobPages]; leaked {
			t.Fatalf("%s node counts the BLOB read: %+v", node.Kind, node.Metrics.Tables)
		}
	}
	if run.result.OverheadActivity.Tables[blobPages].IdxReads == 0 {
		t.Fatalf("OverheadActivity = %+v; the BLOB reads are missing", run.result.OverheadActivity)
	}
	for _, arg := range bound {
		if _, isRef := arg.(BlobRef); isRef {
			t.Fatal("a BLOB reference was bound; variables must hold the BLOB's bytes")
		}
	}
	if len(bound) != 2 || bound[0] != "hello blob" {
		t.Fatalf("UPDATE bound %#v, want the BLOB text per row", bound)
	}
}

func TestLineProfileBlobOverCapFailsRun(t *testing.T) {
	body := "DECLARE VARIABLE X BLOB SUB_TYPE TEXT CHARACTER SET NONE;\nBEGIN\n" +
		"  BEGIN\n" +
		"    FOR SELECT ID FROM LP_T INTO :X DO\n" +
		"      UPDATE LP_T SET A = :X;\n" +
		"  WHEN ANY DO X = NULL;\n" +
		"  END\n" +
		"END"
	for _, tt := range []struct {
		name    string
		max     int64
		content string
		fails   bool
	}{
		{"at cap", 10, "0123456789", false},
		{"over cap", 10, "0123456789A", true},
		{"default cap", 0, "small", false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rows := 1
			run, counts := newLineProfileFakeRun(t, context.Background(), body,
				func() (bool, error) { rows--; return rows >= 0, nil },
				func(string) (int64, error) { return 1, nil })
			run.tx.native.execOverride = func(string, []argument, bool) (int64, error) { return 0, nil }
			run.opts.MaxBlobBytes = tt.max
			counts.rowValue = BlobRef{attachment: run.tx.attachment, tx: run.tx, generation: run.tx.generation,
				high: 1, low: 2, subtype: 1}
			run.openBlob = func(BlobRef) (io.ReadCloser, error) {
				return lineProfileFakeBlobReader{Reader: strings.NewReader(tt.content), onRead: func() {}}, nil
			}
			outcome, txErr := run.runInTransaction()
			if txErr != nil {
				t.Fatalf("runInTransaction() tx error = %v", txErr)
			}
			if !tt.fails {
				if outcome != nil {
					t.Fatalf("outcome = %v, want completed", outcome)
				}
				return
			}
			var result LineProfileResult
			finishLineProfile(context.Background(), &result, outcome, nil)
			if result.Outcome != LineProfileFailed || result.Err == nil ||
				result.Err.Error() != "interbase: line profile: BLOB too large to emulate (more than 10 bytes)" {
				t.Fatalf("outcome %s err %v; want failed with the BLOB-too-large error", result.Outcome, result.Err)
			}
			if result.FailedAt == nil || result.FailedAt.Line != 4 {
				t.Fatalf("FailedAt = %+v, want line 4", result.FailedAt)
			}
			if hits := lineProfileNodeHits(run, 6); hits != 0 {
				t.Fatalf("WHEN ANY handler ran %d times; the over-cap error must not be catchable", hits)
			}
		})
	}
}

func TestLineProfileBlobReadIsBounded(t *testing.T) {
	run := &lpRun{result: &LineProfileResult{}, countersUnavailable: true, opts: LineProfileOptions{MaxBlobBytes: 100}}
	source := &lineProfileCountingReader{remaining: 1 << 20}
	run.openBlob = func(BlobRef) (io.ReadCloser, error) { return source, nil }
	_, err := run.holdValue(BlobRef{})
	if err == nil || !strings.Contains(err.Error(), "BLOB too large to emulate (more than 100 bytes)") {
		t.Fatalf("holdBlobCapped() error = %v", err)
	}
	var fatalErr *lpFatalError
	if !errors.As(err, &fatalErr) {
		t.Fatalf("error %v is not fatal", err)
	}
	if source.read > 101 {
		t.Fatalf("read %d bytes for a 100-byte cap; the read must stop after cap+1", source.read)
	}
	if !source.closed {
		t.Fatal("BLOB reader not closed")
	}
}

func TestLineProfileMaxBlobBytesLimits(t *testing.T) {
	for _, tt := range []struct {
		max, want int64
	}{
		{0, 16 << 20},
		{-5, DefaultLineProfileMaxBlobBytes},
		{100, 100},
		{math.MaxInt64, math.MaxInt64 - 1},
	} {
		run := &lpRun{result: &LineProfileResult{}, countersUnavailable: true, opts: LineProfileOptions{MaxBlobBytes: tt.max}}
		if got := run.maxBlobBytes(); got != tt.want {
			t.Fatalf("maxBlobBytes() with MaxBlobBytes %d = %d, want %d", tt.max, got, tt.want)
		}
		run.openBlob = func(BlobRef) (io.ReadCloser, error) {
			return lineProfileFakeBlobReader{Reader: strings.NewReader("hello blob"), onRead: func() {}}, nil
		}
		if got, err := run.holdValue(BlobRef{subtype: 1}); err != nil || got != "hello blob" {
			t.Fatalf("holdValue() with MaxBlobBytes %d = %q, %v; want the whole BLOB", tt.max, got, err)
		}
	}
}

type lineProfileCountingReader struct {
	remaining, read int
	closed          bool
}

func (r *lineProfileCountingReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := min(len(p), r.remaining)
	r.remaining -= n
	r.read += n
	return n, nil
}

func (r *lineProfileCountingReader) Close() error { r.closed = true; return nil }

// wrapFakePrepare records the arguments every executed statement binds.
func wrapFakePrepare(prepare func(string, bool, *nativeCancelOperation) (*nativeStatement, error),
	bound *[]any) func(string, bool, *nativeCancelOperation) (*nativeStatement, error) {
	return func(query string, arrays bool, op *nativeCancelOperation) (*nativeStatement, error) {
		statement, err := prepare(query, arrays, op)
		if err != nil || statement.execOverride == nil {
			return statement, err
		}
		exec := statement.execOverride
		statement.execOverride = func(args []argument) (int64, error) {
			for _, arg := range args {
				switch arg.kind {
				case argumentString:
					*bound = append(*bound, arg.stringValue)
				case argumentBlobRef:
					*bound = append(*bound, arg.blobRef)
				default:
					*bound = append(*bound, arg.kind)
				}
			}
			return exec(args)
		}
		return statement, nil
	}
}

func TestLineProfileKeptBlobDisplayIsCapped(t *testing.T) {
	long := strings.Repeat("x", keptBlobLimit+10)
	if got := truncateKeptBlob(long).(string); len(got) != keptBlobLimit+len(keptBlobTruncated) || !strings.HasSuffix(got, keptBlobTruncated) {
		t.Fatalf("truncateKeptBlob(string) length %d", len(got))
	}
	if got := truncateKeptBlob([]byte(long)).([]byte); !strings.HasSuffix(string(got), keptBlobTruncated) {
		t.Fatal("truncateKeptBlob([]byte) not marked")
	}
	if got := truncateKeptBlob("short"); got != "short" {
		t.Fatalf("short value changed: %v", got)
	}
}

func TestLineProfileFinishUnwrapsWrappedFatalError(t *testing.T) {
	base := errors.New("link down")
	var result LineProfileResult
	outcome := &lpNodeError{routine: "LP_FAKE", line: 3, err: errors.Join(fatal(base))}
	finishLineProfile(context.Background(), &result, outcome, nil)
	var fatalErr *lpFatalError
	if result.Outcome != LineProfileFailed || errors.As(result.Err, &fatalErr) || !errors.Is(result.Err, base) {
		t.Fatalf("outcome %s err %#v; want the wrapped fatal error unwrapped", result.Outcome, result.Err)
	}
	if want := (&LineProfileLocation{Routine: "LP_FAKE", Line: 3}); !reflect.DeepEqual(result.FailedAt, want) {
		t.Fatalf("FailedAt = %+v", result.FailedAt)
	}
}

func TestLineProfileRelationIDsIncludeOverhead(t *testing.T) {
	result := LineProfileResult{
		Routines:         []LineProfileRoutine{{Nodes: []LineProfileNode{{Metrics: LineMetrics{Tables: map[int]TableCounters{5: {}}}}}}},
		OverheadActivity: ActivityDelta{Tables: map[int]TableCounters{5: {}, 9: {}}},
	}
	got := relationIDs(&result)
	if !reflect.DeepEqual(got, []int{5, 9}) {
		t.Fatalf("relationIDs() = %v, want [5 9]", got)
	}
}

func TestLineProfileExceedsPrecision(t *testing.T) {
	for _, tt := range []struct {
		value, typeText string
		want            bool
	}{
		{"92233720368547758.07", "NUMERIC(18,2)", true},
		{"-92233720368547758.08", "NUMERIC(18, 2)", true},
		{"9223372036854775.80", "NUMERIC(18,2)", false},
		{"0.07", "NUMERIC(18,2)", false},
		{"21474836.47", "DECIMAL(9,2)", true},
		{"1234567.89", "NUMERIC(9,2)", false},
		{"12345678901", "INTEGER", false},
		{"abc", "VARCHAR(5)", false},
	} {
		if got := exceedsPrecision(tt.value, tt.typeText); got != tt.want {
			t.Errorf("exceedsPrecision(%q, %q) = %v, want %v", tt.value, tt.typeText, got, tt.want)
		}
	}
}
