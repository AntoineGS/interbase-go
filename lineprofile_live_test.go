package interbase_test

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
)

// Environment for the live line profiler checks (all skip when
// INTERBASE_DATABASE is unset):
//
//	INTERBASE_DATABASE, INTERBASE_USER, INTERBASE_PASSWORD  connection
//	INTERBASE_CHARSET          optional connection character set (for example NONE)
//	INTERBASE_DIALECT          1 (default) or 3
//	INTERBASE_LP_CORPUS=1      TestLiveLineProfileCorpus: analyze every procedure
//	INTERBASE_LP_CORPUS_STRICT=1  fail the corpus test on any analysis failure
//	INTERBASE_LP_SQL           TestLiveLineProfileStatement: entry statement
//	INTERBASE_LP_STEP_INTO     comma-separated procedures to step into
//	INTERBASE_LP_IGNORE_COLUMNS  comma-separated output columns excluded from
//	                           the native comparison (columns fed by generators,
//	                           whose increments are not rolled back)
//	INTERBASE_LP_WARMUP=1      run the statement test with LineProfileOptions.WarmUp

func lpLiveDB(t *testing.T) (*sql.DB, int) {
	t.Helper()
	database := os.Getenv("INTERBASE_DATABASE")
	if database == "" {
		t.Skip("set INTERBASE_DATABASE, INTERBASE_USER, INTERBASE_PASSWORD to run live line profiler tests")
	}
	password, ok := os.LookupEnv("INTERBASE_PASSWORD")
	if !ok || os.Getenv("INTERBASE_USER") == "" {
		t.Fatal("live connection settings are incomplete")
	}
	dialect := 1
	if v := os.Getenv("INTERBASE_DIALECT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || (n != 1 && n != 3) {
			t.Fatalf("INTERBASE_DIALECT must be 1 or 3, got %q", v)
		}
		dialect = n
	}
	connector, err := interbase.NewConnector(interbase.Config{
		Database: database, User: os.Getenv("INTERBASE_USER"), Password: password,
		Dialect: dialect, Charset: os.Getenv("INTERBASE_CHARSET"),
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Errorf("close database: %v", err)
		}
	})
	return db, dialect
}

func lpList(name string) []string {
	var out []string
	for _, part := range strings.Split(os.Getenv(name), ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

type lpCorpusProc struct {
	name           string
	inputs, output int
}

func lpCorpusProcedures(ctx context.Context, conn *sql.Conn) ([]lpCorpusProc, error) {
	rows, err := conn.QueryContext(ctx, `SELECT P.RDB$PROCEDURE_NAME,
  (SELECT COUNT(*) FROM RDB$PROCEDURE_PARAMETERS A WHERE A.RDB$PROCEDURE_NAME = P.RDB$PROCEDURE_NAME AND A.RDB$PARAMETER_TYPE = 0),
  (SELECT COUNT(*) FROM RDB$PROCEDURE_PARAMETERS A WHERE A.RDB$PROCEDURE_NAME = P.RDB$PROCEDURE_NAME AND A.RDB$PARAMETER_TYPE = 1)
FROM RDB$PROCEDURES P WHERE COALESCE(P.RDB$SYSTEM_FLAG, 0) = 0 ORDER BY 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var procs []lpCorpusProc
	for rows.Next() {
		var p lpCorpusProc
		if err := rows.Scan(&p.name, &p.inputs, &p.output); err != nil {
			return nil, err
		}
		p.name = strings.TrimRight(p.name, " ")
		procs = append(procs, p)
	}
	return procs, rows.Err()
}

// TestLiveLineProfileCorpus parses every user procedure as a line profile
// entry. Analysis only: no routine code is ever executed. Failures are
// reported in the test output; they fail the test only when
// INTERBASE_LP_CORPUS_STRICT=1.
func TestLiveLineProfileCorpus(t *testing.T) {
	db, dialect := lpLiveDB(t)
	if os.Getenv("INTERBASE_LP_CORPUS") != "1" {
		t.Skip("set INTERBASE_LP_CORPUS=1 to analyze every procedure")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	procs, err := lpCorpusProcedures(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	var parsed, refused, errored, unnamed int
	var failures []string
	for _, p := range procs {
		entry, ok := lpCorpusEntry(p, dialect)
		if !ok {
			unnamed++
			failures = append(failures, fmt.Sprintf("%s: cannot be named in dialect %d", p.name, dialect))
			continue
		}
		analysis, err := interbase.AnalyzeLineProfile(ctx, conn, entry, dialect, nil)
		switch {
		case err != nil:
			errored++
			failures = append(failures, fmt.Sprintf("%s: analysis error: %v", p.name, err))
		case len(analysis.Problems) > 0:
			refused++
			for _, pr := range analysis.Problems {
				failures = append(failures, fmt.Sprintf("%s: %s %d:%d %s", p.name, pr.Routine, pr.Line, pr.Column, pr.Message))
			}
		default:
			parsed++
		}
	}
	t.Logf("procedures=%d parsed=%d refused=%d analysis_errors=%d unnamed=%d", len(procs), parsed, refused, errored, unnamed)
	for _, f := range failures {
		t.Logf("FAILURE %s", f)
	}
	if os.Getenv("INTERBASE_LP_CORPUS_STRICT") == "1" && refused+errored > 0 {
		t.Fatalf("%d procedures could not be analyzed", refused+errored)
	}
}

// lpCorpusEntry returns the entry statement of a corpus procedure, with
// NULL arguments. Dialect 3 quotes the name; dialect 1 has no quoted names,
// so a name that is not a plain upper-case identifier cannot be written
// there (ok is false).
func lpCorpusEntry(p lpCorpusProc, dialect int) (entry string, ok bool) {
	name := p.name
	if dialect == 3 {
		name = `"` + strings.ReplaceAll(p.name, `"`, `""`) + `"`
	} else {
		for i, c := range p.name {
			if !(c >= 'A' && c <= 'Z' || i > 0 && (c >= '0' && c <= '9' || c == '_' || c == '$')) {
				return "", false
			}
		}
	}
	args := ""
	if p.inputs > 0 {
		args = "(" + strings.TrimSuffix(strings.Repeat("NULL,", p.inputs), ",") + ")"
	}
	if p.output > 0 {
		return "SELECT * FROM " + name + args, true
	}
	return "EXECUTE PROCEDURE " + name + args, true
}

func lpCellText(v any) string {
	switch x := v.(type) {
	case nil:
		return "<NULL>"
	case []byte:
		return string(x)
	case time.Time:
		return x.Format(time.RFC3339Nano)
	default:
		return fmt.Sprint(x)
	}
}

// TestLiveLineProfileStatement profiles INTERBASE_LP_SQL (always rolled
// back) and compares its first kept row with a native execution that is also
// rolled back.
func TestLiveLineProfileStatement(t *testing.T) {
	db, dialect := lpLiveDB(t)
	entry := os.Getenv("INTERBASE_LP_SQL")
	if entry == "" {
		t.Skip("set INTERBASE_LP_SQL to profile a statement")
	}
	ignore := map[string]bool{}
	for _, c := range lpList("INTERBASE_LP_IGNORE_COLUMNS") {
		ignore[strings.ToUpper(c)] = true
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	stepInto := lpList("INTERBASE_LP_STEP_INTO")
	analysis, err := interbase.AnalyzeLineProfile(ctx, conn, entry, dialect, stepInto)
	if err != nil {
		conn.Close()
		t.Fatal(err)
	}
	if len(analysis.Problems) > 0 {
		for _, p := range analysis.Problems {
			t.Logf("problem: %s %d:%d %s", p.Routine, p.Line, p.Column, p.Message)
		}
		conn.Close()
		t.Fatalf("analysis refused the statement (%d problems)", len(analysis.Problems))
	}
	t.Logf("routine=%s mayWrite=%v callees=%d", analysis.Routine, analysis.MayWrite, len(analysis.Callees))
	result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
		SQL: entry, Dialect: dialect, ReadOnly: !analysis.MayWrite, StepInto: stepInto, KeepRows: 10,
		WarmUp: os.Getenv("INTERBASE_LP_WARMUP") == "1",
	})
	if cerr := conn.Close(); cerr != nil {
		t.Errorf("close connection: %v", cerr)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("outcome=%s err=%v rolledBack=%v readOnly=%v", result.Outcome, result.Err, result.RolledBack, result.ReadOnly)
	t.Logf("warmUp=%v warmUpErr=%v", result.WarmUp, result.WarmUpErr)
	if result.FailedAt != nil {
		t.Logf("failedAt=%s:%d", result.FailedAt.Routine, result.FailedAt.Line)
	}
	t.Logf("rows=%d columns=%v raw=%v adjusted=%v serverCalls=%d roundTrips=%d", result.Rows, result.Columns, result.Raw, result.Adjusted, result.ServerCalls, result.RoundTrips)
	t.Logf("overhead prepare=%v savepoints=%v snapshots=%v setup=%v blobs=%v baseline before=%v after=%v",
		result.Prepare, result.Savepoints, result.Snapshots, result.Setup, result.Blobs, result.Before.Median, result.After.Median)
	for _, site := range result.NativeCallSites {
		t.Logf("native call site: %s:%d %s (%s)", site.Routine, site.Line, site.Procedure, site.Reason)
	}
	if !result.RolledBack {
		t.Error("line profile was not rolled back")
	}

	type topLine struct {
		routine string
		line    int
		m       interbase.LineMetrics
	}
	var lines []topLine
	for _, r := range result.Routines {
		for n, m := range r.Lines {
			lines = append(lines, topLine{r.Name, n, m})
		}
	}
	sort.Slice(lines, func(i, j int) bool { return lines[i].m.Adjusted > lines[j].m.Adjusted })
	if len(lines) > 15 {
		lines = lines[:15]
	}
	for _, l := range lines {
		t.Logf("top line %s:%d hits=%d adjusted=%v firstAdjusted=%v raw=%v serverCalls=%d rows=%d", l.routine, l.line, l.m.Hits, l.m.Adjusted, l.m.FirstAdjusted, l.m.Raw, l.m.ServerCalls, l.m.Rows)
	}
	if result.Outcome != interbase.LineProfileCompleted {
		t.Fatalf("line profile did not complete: %s: %v", result.Outcome, result.Err)
	}

	// Native comparison in a separate, rolled-back transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, entry)
	if err != nil {
		t.Fatalf("native execution: %v", err)
	}
	defer rows.Close()
	native := rows.Next()
	if !native {
		if err := rows.Err(); err != nil {
			t.Fatalf("native fetch: %v", err)
		}
		if len(result.KeptRows) != 0 {
			t.Fatalf("native returned no rows, emulation kept %d", len(result.KeptRows))
		}
		return
	}
	cols, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	values := make([]any, len(cols))
	targets := make([]any, len(cols))
	for i := range values {
		targets[i] = &values[i]
	}
	if err := rows.Scan(targets...); err != nil {
		t.Fatal(err)
	}
	if len(result.KeptRows) == 0 {
		t.Fatal("native returned a row, emulation kept none")
	}
	first := result.KeptRows[0]
	if len(first) != len(cols) {
		t.Fatalf("column count: emulated %d, native %d", len(first), len(cols))
	}
	for i, name := range cols {
		emulated, nativeText := lpCellText(first[i].Value), lpCellText(values[i])
		if ignore[strings.ToUpper(name)] {
			t.Logf("column %s ignored (emulated=%q native=%q)", name, emulated, nativeText)
			continue
		}
		if emulated != nativeText {
			t.Errorf("column %s differs: emulated=%q native=%q", name, emulated, nativeText)
		}
	}
}

func TestLineProfileCorpusEntry(t *testing.T) {
	for _, tt := range []struct {
		proc    lpCorpusProc
		dialect int
		want    string
		ok      bool
	}{
		{lpCorpusProc{"LP_A", 2, 1}, 1, "SELECT * FROM LP_A(NULL,NULL)", true},
		{lpCorpusProc{"LP_A$1", 0, 0}, 1, "EXECUTE PROCEDURE LP_A$1", true},
		{lpCorpusProc{"lp_lower", 0, 1}, 1, "", false},
		{lpCorpusProc{"LP A", 1, 0}, 1, "", false},
		{lpCorpusProc{"1LP", 0, 0}, 1, "", false},
		{lpCorpusProc{`lp "q"`, 1, 0}, 3, `EXECUTE PROCEDURE "lp ""q"""(NULL)`, true},
	} {
		if got, ok := lpCorpusEntry(tt.proc, tt.dialect); got != tt.want || ok != tt.ok {
			t.Errorf("lpCorpusEntry(%+v, %d) = %q, %v; want %q, %v", tt.proc, tt.dialect, got, ok, tt.want, tt.ok)
		}
	}
}
