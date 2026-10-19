package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// newLineProfileDynamicRun is a fake run over a NONE database whose output
// columns are listed in columns; savepoint statements are recorded.
func newLineProfileDynamicRun(t *testing.T, body string, columns []string, next func() (bool, error),
	exec func(query string) (int64, error)) (*lpRun, *lineProfileFakeCounts, *[]string) {
	t.Helper()
	if exec == nil {
		exec = func(string) (int64, error) { return 1, nil }
	}
	run, counts := newLineProfileFakeRun(t, context.Background(), body, next, exec)
	run.dbNone, run.plan.dbNone = true, true
	run.result.Columns = columns
	var savepoints []string
	run.tx.native.execOverride = func(query string, _ []argument, _ bool) (int64, error) {
		savepoints = append(savepoints, query)
		return 0, nil
	}
	return run, counts, &savepoints
}

func lineProfileNodeOn(t *testing.T, run *lpRun, line int, kind string) LineProfileNode {
	t.Helper()
	for _, node := range run.result.Routines[0].Nodes {
		if node.Line == line && node.Kind == kind {
			return node
		}
	}
	t.Fatalf("no %s node on line %d", kind, line)
	return LineProfileNode{}
}

func lineProfileKept(run *lpRun) [][]any {
	var rows [][]any
	for _, row := range run.result.KeptRows {
		values := make([]any, len(row))
		for i, cell := range row {
			values[i] = cellValue(cell)
		}
		rows = append(rows, values)
	}
	return rows
}

func countDynamicStatements(run *lpRun) int {
	n := 0
	for key := range run.statements {
		if strings.HasPrefix(key, lpDynamicKeyPrefix) {
			n++
		}
	}
	return n
}

func TestLineProfileExecuteStatementCache(t *testing.T) {
	body := "DECLARE VARIABLE I INTEGER;\nBEGIN\n" +
		"  I = 0;\n" +
		"  WHILE (I < 41) DO\n" +
		"  BEGIN\n" +
		"    I = I + 1;\n" +
		"    EXECUTE STATEMENT 'UPDATE LP_T SET A = ' || I;\n" +
		"    EXECUTE STATEMENT 'DELETE FROM LP_T';\n" +
		"  END\n" +
		"END"
	updates, maxOpen := 0, 0
	var run *lpRun
	run, counts, _ := newLineProfileDynamicRun(t, body, nil, func() (bool, error) { return true, nil },
		func(query string) (int64, error) {
			maxOpen = max(maxOpen, countDynamicStatements(run))
			if strings.HasPrefix(query, "UPDATE") {
				updates++
			}
			return 2, nil
		})
	// The texts are UPDATE … = 1 to 40, then 1 again.
	counts.valueFor = func(query string) driver.Value {
		if !strings.HasPrefix(query, "SELECT CAST((") {
			return nil
		}
		return fmt.Sprintf("UPDATE LP_T SET A = %d", updates%40+1)
	}
	outcome, txErr := run.runInTransaction()
	if outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	update := lineProfileNodeOn(t, run, 7, "executeStatement")
	if update.DynamicTexts != 40 || update.DynamicSQL != "UPDATE LP_T SET A = 1" || update.SQL != update.DynamicSQL ||
		update.Metrics.Hits != 41 || update.Metrics.Rows != 82 {
		t.Fatalf("UPDATE node %+v", update)
	}
	if counts.prepares["UPDATE LP_T SET A = 1"] != 2 || counts.prepares["UPDATE LP_T SET A = 2"] != 1 ||
		counts.prepares["UPDATE LP_T SET A = 40"] != 1 {
		t.Fatalf("prepares %v; want one per text, the evicted oldest text prepared again", counts.prepares)
	}
	deleteNode := lineProfileNodeOn(t, run, 8, "executeStatement")
	if deleteNode.DynamicTexts != 1 || deleteNode.DynamicSQL != "DELETE FROM LP_T" || counts.prepares["DELETE FROM LP_T"] != 1 {
		t.Fatalf("DELETE node %+v, prepares %d", deleteNode, counts.prepares["DELETE FROM LP_T"])
	}
	// The UPDATE node keeps lpDynamicLimit texts prepared, the DELETE node
	// its one text.
	if maxOpen != lpDynamicLimit+1 {
		t.Fatalf("at most %d dynamic statements open across both nodes, want %d (%d for the UPDATE node, 1 for the DELETE node)",
			maxOpen, lpDynamicLimit+1, lpDynamicLimit)
	}
	if run.result.Prepare <= 0 || counts.closed != counts.prepared || len(run.statements) != 0 {
		t.Fatalf("prepare %v, statements %d prepared %d closed, %d kept", run.result.Prepare, counts.prepared, counts.closed, len(run.statements))
	}
}

func TestLineProfileExecuteStatementInto(t *testing.T) {
	body := func(into string) string {
		return "DECLARE VARIABLE X INTEGER;\nDECLARE VARIABLE Y INTEGER;\nBEGIN\n" +
			"  X = 5;\n" +
			"  BEGIN\n" +
			"    EXECUTE STATEMENT 'SELECT ID FROM LP_T' INTO " + into + ";\n" +
			"  WHEN ANY DO Y = 9;\n" +
			"  END\n" +
			"  SUSPEND;\n" +
			"END"
	}
	arithmetic := &Error{Operation: "fetch", SQLCode: -802, NativeCode: 335544321, gdsCodes: []int64{335544321}}
	// fetches returns a row for each nil result, then the end of rows for
	// lineProfileFakeEOF or the error; rows without end after the results.
	fetches := func(results ...error) func() (bool, error) {
		i := 0
		return func() (bool, error) {
			if i >= len(results) {
				return true, nil
			}
			err := results[i]
			i++
			if err == lineProfileFakeEOF {
				return false, nil
			}
			return err == nil, err
		}
	}
	varchar := []OutputDescriptor{{Name: "ID", Kind: "VARCHAR", Length: 10, Nullable: true}}
	bigint := []OutputDescriptor{{Name: "ID", Kind: "BIGINT", Nullable: true}}
	for _, tt := range []struct {
		name    string
		into    string
		next    func() (bool, error)
		outputs []OutputDescriptor
		value   driver.Value
		want    []any  // X, Y
		rows    int64  // rows fetched
		fatal   string // the run fails with this message, uncaught
	}{
		{name: "first of several rows", into: ":X", next: fetches(), want: []any{int64(1), nil}, rows: 2},
		{name: "single row", into: "X", next: fetches(nil, lineProfileFakeEOF), want: []any{int64(1), nil}, rows: 1},
		{name: "no row keeps the target", into: ":X", next: fetches(lineProfileFakeEOF), want: []any{int64(5), nil}},
		{name: "first fetch error is ignored", into: ":X", next: fetches(arithmetic), want: []any{int64(5), nil}},
		{name: "second fetch error is ignored", into: ":X", next: fetches(nil, arithmetic), want: []any{int64(1), nil}, rows: 1},
		{name: "NULL of another type", into: ":X", next: fetches(), outputs: varchar, value: lineProfileFakeNull{}, want: []any{nil, nil}, rows: 2},
		{name: "column of another type", into: ":X", next: fetches(), outputs: varchar, value: "abc", fatal: "without conversion"},
		{name: "wider integer that fits", into: ":X", next: fetches(), outputs: bigint, value: int64(-7), want: []any{int64(-7), nil}, rows: 2},
		{name: "wider integer that does not fit", into: ":X", next: fetches(), outputs: bigint, value: int64(1) << 40, fatal: "without conversion"},
		{name: "more targets than columns", into: ":X, :Y", next: fetches(), fatal: "1 columns for 2 targets"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run, counts, _ := newLineProfileDynamicRun(t, body(tt.into), []string{"X", "Y"}, tt.next, nil)
			counts.outputsFor = func(query string) []OutputDescriptor {
				if query == "SELECT ID FROM LP_T" {
					return tt.outputs
				}
				return nil
			}
			counts.valueFor = func(query string) driver.Value {
				if query == "SELECT ID FROM LP_T" {
					return tt.value
				}
				return nil
			}
			outcome, txErr := run.runInTransaction()
			if txErr != nil {
				t.Fatalf("transaction error %v", txErr)
			}
			if tt.fatal != "" {
				var fatalErr *lpFatalError
				if !errors.As(outcome, &fatalErr) || !strings.Contains(outcome.Error(), tt.fatal) || lineProfileNodeHits(run, 7) != 0 {
					t.Fatalf("outcome %v, handler hits %d; want an uncaught failure mentioning %q", outcome, lineProfileNodeHits(run, 7), tt.fatal)
				}
				return
			}
			if outcome != nil {
				t.Fatalf("outcome %v", outcome)
			}
			if got := lineProfileKept(run); len(got) != 1 || fmt.Sprint(got[0]) != fmt.Sprint(tt.want) {
				t.Fatalf("X, Y = %v, want %v", got, tt.want)
			}
			node := lineProfileNodeOn(t, run, 6, "executeStatement")
			if node.Metrics.Rows != tt.rows || node.DynamicSQL != "SELECT ID FROM LP_T" || node.DynamicTexts != 1 {
				t.Fatalf("node %+v, want %d rows", node, tt.rows)
			}
			assertLineProfileFakeClosedWithoutCursorCheck(t, counts)
		})
	}
}

// TestLineProfileExecuteStatementIntoDescribesDeclaredType checks that the
// target of an INTO copy is described with its declared type, the database
// default character set added to a text type declared without one.
func TestLineProfileExecuteStatementIntoDescribesDeclaredType(t *testing.T) {
	body := "DECLARE VARIABLE X VARCHAR(10);\nBEGIN\n" +
		"  EXECUTE STATEMENT 'SELECT V FROM LP_T' INTO :X;\n" +
		"  SUSPEND;\n" +
		"END"
	const describe = "SELECT CAST(NULL AS VARCHAR(10) CHARACTER SET WIN1252) FROM RDB$DATABASE"
	run, counts, _ := newLineProfileDynamicRun(t, body, []string{"X"}, func() (bool, error) { return true, nil }, nil)
	run.plan.dbCharset = "win1252"
	varchar := []OutputDescriptor{{Name: "V", Kind: "VARCHAR", SubType: 53, Length: 10, Nullable: true}}
	counts.outputsFor = func(query string) []OutputDescriptor {
		if query == "SELECT V FROM LP_T" || query == describe {
			return varchar
		}
		return nil
	}
	counts.valueFor = func(query string) driver.Value {
		if query == "SELECT V FROM LP_T" {
			return "abc"
		}
		return nil
	}
	if outcome, txErr := run.runInTransaction(); outcome != nil || txErr != nil {
		t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
	}
	if counts.prepares[describe] != 1 {
		t.Fatalf("prepares %v; want the target described as %q", counts.prepares, describe)
	}
}

// lineProfileFakeEOF marks the end of rows in a fake result.
var lineProfileFakeEOF = errors.New("fake end of rows")

func TestLineProfileExecuteStatementErrors(t *testing.T) {
	body := func(text, when string) string {
		return "DECLARE VARIABLE X INTEGER;\nBEGIN\n" +
			"  X = 0;\n" +
			"  BEGIN\n" +
			"    UPDATE LP_T SET A = 1;\n" +
			"    EXECUTE STATEMENT " + text + ";\n" +
			"  " + when + " DO X = 1;\n" +
			"  END\n" +
			"  SUSPEND;\n" +
			"END"
	}
	unknownTable := &Error{Operation: "prepare statement", SQLCode: -204, NativeCode: 335544569,
		Message: "Dynamic SQL Error\nTable unknown", gdsCodes: []int64{335544569, 335544580}}
	exec := func(query string) (int64, error) {
		if strings.HasPrefix(query, "INSERT INTO LP_T VALUES") {
			return 0, &Error{Operation: "exec", SQLCode: -413, NativeCode: 335544334, gdsCodes: []int64{335544334}}
		}
		return 1, nil
	}
	for _, tt := range []struct {
		name, text, when string
		sqlCode          int
		gdsCode          int64
		fatal            bool
	}{
		{name: "NULL text", text: "NULL", when: "WHEN SQLCODE -104", sqlCode: -104, gdsCode: 335544853},
		{name: "blank text", text: "'  '", when: "WHEN SQLCODE -104", sqlCode: -104, gdsCode: 335544853},
		{name: "prepare error", text: "'UPDATE LP_NOPE SET B = 1'", when: "WHEN GDSCODE exec_stmt2_execute", sqlCode: -204, gdsCode: 335544853},
		{name: "prepare error SQLCODE", text: "'UPDATE LP_NOPE SET B = 1'", when: "WHEN SQLCODE -204", sqlCode: -204, gdsCode: 335544853},
		{name: "execution error", text: "'INSERT INTO LP_T VALUES (''x'')'", when: "WHEN SQLCODE -204", sqlCode: -204, gdsCode: 335544853},
		{name: "parameter marker", text: "'UPDATE LP_T SET A = ?'", when: "WHEN SQLCODE -804", sqlCode: -804, gdsCode: 335544853},
		{name: "EXECUTE PROCEDURE", text: "'EXECUTE PROCEDURE LP_P'", when: "WHEN SQLCODE -204", sqlCode: -204, gdsCode: 335544853},
		{name: "COMMIT", text: "'COMMIT'", when: "WHEN GDSCODE exec_stmt_disallow", sqlCode: -204, gdsCode: 335544857},
		{name: "ROLLBACK WORK", text: "'ROLLBACK WORK'", when: "WHEN GDSCODE exec_stmt_disallow", sqlCode: -204, gdsCode: 335544857},
		{name: "SET TRANSACTION", text: "'SET TRANSACTION'", when: "WHEN SQLCODE -204", sqlCode: -204, gdsCode: 335544853},
		{name: "unsupported statement", text: "'SET GENERATOR LP_G TO 1'", when: "WHEN ANY", fatal: true},
		{name: "SAVEPOINT", text: "'SAVEPOINT LP_S'", when: "WHEN ANY", fatal: true},
		{name: "RELEASE SAVEPOINT", text: "'RELEASE SAVEPOINT LP_S'", when: "WHEN ANY", fatal: true},
		{name: "ROLLBACK TO SAVEPOINT", text: "'ROLLBACK TO SAVEPOINT LP_S'", when: "WHEN ANY", fatal: true},
		{name: "ROLLBACK WORK TO SAVEPOINT", text: "'ROLLBACK WORK TO SAVEPOINT LP_S'", when: "WHEN ANY", fatal: true},
		{name: "DDL", text: "'CREATE TABLE LP_X (ID INTEGER)'", when: "WHEN ANY", fatal: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run, counts, savepoints := newLineProfileDynamicRun(t, body(tt.text, tt.when), []string{"X"},
				func() (bool, error) { return false, nil }, exec)
			counts.prepareErr = func(query string) error {
				if strings.Contains(query, "LP_NOPE") {
					return unknownTable
				}
				return nil
			}
			outcome, txErr := run.runInTransaction()
			if txErr != nil {
				t.Fatalf("transaction error %v", txErr)
			}
			if tt.fatal {
				var fatalErr *lpFatalError
				var located *lpNodeError
				if !errors.As(outcome, &fatalErr) || !errors.As(outcome, &located) || located.line != 6 || lineProfileNodeHits(run, 7) != 0 ||
					!strings.Contains(outcome.Error(), "does not run this kind of statement") {
					t.Fatalf("outcome %v; want an uncaught refusal at line 6", outcome)
				}
				if counts.closed != counts.prepared || counts.queried != 0 {
					t.Fatalf("statements %d prepared, %d closed, %d queried; a refused text is closed, never run", counts.prepared, counts.closed, counts.queried)
				}
				return
			}
			if outcome != nil || fmt.Sprint(lineProfileKept(run)) != "[[1]]" {
				t.Fatalf("outcome %v, rows %v; want the handler to catch the error", outcome, lineProfileKept(run))
			}
			if got := strings.Join(*savepoints, ";"); !strings.Contains(got, "ROLLBACK TO SAVEPOINT") {
				t.Fatalf("savepoints %q; an EXECUTE STATEMENT error undoes the handling block", got)
			}
			// The same error, uncaught, is the run's outcome.
			run, counts, _ = newLineProfileDynamicRun(t, body(tt.text, "WHEN SQLCODE 1"), []string{"X"},
				func() (bool, error) { return false, nil }, exec)
			counts.prepareErr = func(query string) error {
				if strings.Contains(query, "LP_NOPE") {
					return unknownTable
				}
				return nil
			}
			outcome, _ = run.runInTransaction()
			var nativeErr *Error
			var located *lpNodeError
			if !errors.As(outcome, &nativeErr) || !errors.As(outcome, &located) || located.line != 6 ||
				nativeErr.SQLCode != tt.sqlCode || nativeErr.GDSCode() != tt.gdsCode ||
				!strings.Contains(nativeErr.Message, "EXECUTE STATEMENT") && !strings.Contains(nativeErr.Message, "Could not execute statement") {
				t.Fatalf("uncaught outcome %#v located %+v", nativeErr, located)
			}
			if node := lineProfileNodeOn(t, run, 6, "executeStatement"); node.Metrics.Errors != 1 {
				t.Fatalf("node errors %d, want 1", node.Metrics.Errors)
			}
		})
	}
}

// TestLineProfileUnsupportedDynamicWords checks how the first words of a
// text the driver does not prepare choose its outcome.
func TestLineProfileUnsupportedDynamicWords(t *testing.T) {
	for _, tt := range []struct {
		text    string
		gdsCode int64 // 0: refused, not run
	}{
		{"COMMIT RETAIN", gdsExecStmtDisallow},
		{"commit work", gdsExecStmtDisallow},
		{"ROLLBACK RETAIN", gdsExecStmtDisallow},
		{"ROLLBACK WORK", gdsExecStmtDisallow},
		{"/* undo */ ROLLBACK", gdsExecStmtDisallow},
		{"-- undo\nrollback work", gdsExecStmtDisallow},
		{"SET TRANSACTION READ ONLY", gdsExecStmtExecute},
		{"/* t */ SET TRANSACTION", gdsExecStmtExecute},
		{"ROLLBACK TO SAVEPOINT LP_S", 0},
		{"ROLLBACK WORK TO LP_S", 0},
		{"/* sp */ ROLLBACK TO LP_S", 0},
		{"SET GENERATOR LP_G TO 1", 0},
		{"SAVEPOINT LP_S", 0},
		{"RELEASE SAVEPOINT LP_S", 0},
	} {
		t.Run(tt.text, func(t *testing.T) {
			err := unsupportedDynamic(tt.text)
			var nativeErr *Error
			if tt.gdsCode == 0 {
				if !errors.As(err, new(*lpFatalError)) || !strings.Contains(err.Error(), "does not run this kind of statement") {
					t.Fatalf("unsupportedDynamic() = %v, want a refusal", err)
				}
				return
			}
			if !errors.As(err, &nativeErr) || errors.As(err, new(*lpFatalError)) || nativeErr.GDSCode() != tt.gdsCode || nativeErr.SQLCode != -204 {
				t.Fatalf("unsupportedDynamic() = %#v, want catchable GDSCODE %d, SQLCODE -204", err, tt.gdsCode)
			}
		})
	}
}

func TestLineProfileForExecuteStatement(t *testing.T) {
	body := func(text, loop string) string {
		return "DECLARE VARIABLE X INTEGER;\nDECLARE VARIABLE K INTEGER;\nBEGIN\n" +
			"  K = 0;\n" +
			"  FOR EXECUTE STATEMENT " + text + " INTO :X DO\n" +
			"    " + loop + "\n" +
			"  X = 7;\n" +
			"  SUSPEND;\n" +
			"END"
	}
	arithmetic := &Error{Operation: "fetch", SQLCode: -802, NativeCode: 335544321, gdsCodes: []int64{335544321}}
	rows := func(n int, end error) func() (bool, error) {
		i := 0
		return func() (bool, error) {
			if i++; i <= n {
				return true, nil
			}
			return false, end
		}
	}
	for _, tt := range []struct {
		name, text, loop string
		next             func() (bool, error)
		readOnly         bool
		failForUpdate    bool
		want             string // K, X
		forUpdate        bool
		runsAhead        bool
	}{
		{name: "all rows", text: "'SELECT ID FROM LP_T'", loop: "K = K + 1;", next: rows(3, nil), want: "[[3 7]]"},
		{name: "fetch error ends the loop", text: "'SELECT ID FROM LP_T'", loop: "K = K + 1;", next: rows(2, arithmetic), want: "[[2 7]]"},
		{name: "BREAK", text: "'SELECT ID FROM LP_T'", loop: "BEGIN K = K + 1; BREAK; END", next: rows(3, nil), want: "[[1 7]]"},
		{name: "procedure row by row", text: "'SELECT I FROM LP_GEN(3)'", loop: "K = K + 1;", next: rows(3, nil), want: "[[3 7]]", forUpdate: true},
		{name: "procedure in a read-only run", text: "'SELECT I FROM LP_GEN(3)'", loop: "K = K + 1;", next: rows(3, nil), readOnly: true, want: "[[3 7]]"},
		{name: "procedure that cannot be fetched row by row", text: "'SELECT I FROM LP_GEN(3)'", loop: "K = K + 1;", next: rows(3, nil),
			failForUpdate: true, want: "[[3 7]]", runsAhead: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			run, counts, _ := newLineProfileDynamicRun(t, body(tt.text, tt.loop), []string{"K", "X"}, tt.next, nil)
			run.opts.ReadOnly = tt.readOnly
			run.procedureNames = map[string]bool{"LP_T": false}
			counts.prepareErr = func(query string) error {
				if tt.failForUpdate && strings.HasSuffix(query, " FOR UPDATE") {
					return &Error{Operation: "prepare statement", SQLCode: -104, NativeCode: 335544569, gdsCodes: []int64{335544569}}
				}
				return nil
			}
			outcome, txErr := run.runInTransaction()
			if outcome != nil || txErr != nil {
				t.Fatalf("runInTransaction() = %v, %v", outcome, txErr)
			}
			if got := fmt.Sprint(lineProfileKept(run)); got != tt.want {
				t.Fatalf("K, X = %s, want %s", got, tt.want)
			}
			node := lineProfileNodeOn(t, run, 5, "forExecuteStatement")
			text := strings.Trim(tt.text, "'")
			if node.DynamicSQL != text || strings.HasSuffix(node.SQL, " FOR UPDATE") != tt.forUpdate {
				t.Fatalf("node SQL %q dynamic %q, want FOR UPDATE %v", node.SQL, node.DynamicSQL, tt.forUpdate)
			}
			if got := len(run.result.NativeCallSites) == 1 && run.result.NativeCallSites[0].Procedure == "LP_GEN" &&
				run.result.NativeCallSites[0].Line == 5; got != tt.runsAhead {
				t.Fatalf("native call sites %+v, want a runs-ahead site %v", run.result.NativeCallSites, tt.runsAhead)
			}
			assertLineProfileFakeClosedWithoutCursorCheck(t, counts)
		})
	}
}

// TestLineProfileDynamicCloseFailureKeepsStatement checks that an evicted
// or refused dynamic statement leaves the run's statements only once it is
// closed, so that closeStatements still closes it when its close fails.
func TestLineProfileDynamicCloseFailureKeepsStatement(t *testing.T) {
	closeFailure := errors.New("close failed")
	for _, tt := range []struct {
		name, body, closing string
		before              int // dynamic statements kept when it closes
	}{
		{name: "eviction", closing: "UPDATE LP_T SET A = 1", before: lpDynamicLimit + 1,
			body: "DECLARE VARIABLE I INTEGER;\nBEGIN\n  I = 0;\n  WHILE (I < 33) DO\n  BEGIN\n    I = I + 1;\n" +
				"    EXECUTE STATEMENT 'UPDATE LP_T SET A = ' || I;\n  END\nEND"},
		{name: "refusal", closing: "EXECUTE PROCEDURE LP_P", before: 1,
			body: "BEGIN\n  EXECUTE STATEMENT 'EXECUTE PROCEDURE LP_P';\nEND"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			updates := 0
			var run *lpRun
			run, counts, _ := newLineProfileDynamicRun(t, tt.body, nil, func() (bool, error) { return true, nil },
				func(query string) (int64, error) {
					updates++
					return 1, nil
				})
			counts.valueFor = func(query string) driver.Value {
				if strings.HasPrefix(query, "SELECT CAST((") {
					return fmt.Sprintf("UPDATE LP_T SET A = %d", updates+1)
				}
				return nil
			}
			kept := -1
			counts.onClose = func(query string) error {
				if query == tt.closing && kept < 0 {
					kept = countDynamicStatements(run)
					return closeFailure
				}
				return nil
			}
			outcome, _ := run.runInTransaction()
			if !errors.Is(outcome, closeFailure) {
				t.Fatalf("outcome %v, want the close failure", outcome)
			}
			if kept != tt.before {
				t.Fatalf("%d dynamic statements kept while closing %q, want %d (deleted only after a successful close)", kept, tt.closing, tt.before)
			}
			if len(run.statements) != 0 {
				t.Fatalf("statements left after the run: %d", len(run.statements))
			}
		})
	}
}
