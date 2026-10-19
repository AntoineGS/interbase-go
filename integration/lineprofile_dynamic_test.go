//go:build integration

package integration_test

import (
	"fmt"
	"strings"
	"testing"

	interbase "interbase-go"
)

// lpDynamicDDL creates the EXECUTE STATEMENT fixtures (ibg-5d6.1). Every
// form runs in both dialects; texts select INTEGER or CHARACTER SET NONE
// columns into targets of the same storage, which InterBase copies exactly.
func lpDynamicDDL() []string {
	return []string{
		"CREATE TABLE LP_ES_T (ID INTEGER, V VARCHAR(10) CHARACTER SET NONE)",
		"INSERT INTO LP_ES_T VALUES (1, '5')",
		"INSERT INTO LP_ES_T VALUES (2, 'x')",
		"INSERT INTO LP_ES_T VALUES (3, '7')",
		`CREATE PROCEDURE LP_ES_SING (K INTEGER) RETURNS (N VARCHAR(20) CHARACTER SET NONE, C INTEGER, E INTEGER) AS
BEGIN
  N = 'none';
  EXECUTE STATEMENT 'SELECT NAME FROM LP_ITEMS WHERE ID = ' || K INTO :N;
  EXECUTE STATEMENT ('SELECT ID FROM LP_ITEMS ORDER BY ID DESC') INTO C;
  EXECUTE STATEMENT 'SELECT ID FROM LP_ITEMS WHERE ID > 100' INTO :E;
  EXECUTE STATEMENT 'SELECT ID FROM LP_ITEMS';
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_FOR RETURNS (I INTEGER, N VARCHAR(20) CHARACTER SET NONE) AS
BEGIN
  FOR EXECUTE STATEMENT 'SELECT ID, NAME FROM LP_ITEMS ORDER BY ID' INTO :I, :N DO
    SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_FORPROC RETURNS (I INTEGER, C INTEGER) AS
BEGIN
  FOR EXECUTE STATEMENT 'SELECT I FROM LP_MK_GEN(5)' INTO :I DO
  BEGIN
    SELECT COUNT(*) FROM LP_MK_LOG INTO :C;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_ES_DML (K INTEGER) RETURNS (C INTEGER, M VARCHAR(40)) AS
BEGIN
  EXECUTE STATEMENT 'UPDATE LP_ITEMS SET CODE = ''es'' WHERE ID <= ' || K;
  SELECT COUNT(*) FROM LP_ITEMS WHERE CODE = 'es' INTO :C;
  EXECUTE STATEMENT 'INSERT INTO LP_LOG (ID, MSG) VALUES (' || K || ', ''dyn'')';
  SELECT MSG FROM LP_LOG WHERE ID = :K INTO :M;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_LOOP (N INTEGER) RETURNS (T INTEGER) AS
DECLARE VARIABLE I INTEGER;
DECLARE VARIABLE X INTEGER;
BEGIN
  I = 0;
  T = 0;
  WHILE (I < N) DO
  BEGIN
    I = I + 1;
    EXECUTE STATEMENT 'SELECT ID + ' || I || ' FROM LP_ES_T WHERE ID = 1' INTO :X;
    T = T + X;
  END
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_ERR (K INTEGER) RETURNS (N INTEGER) AS
DECLARE VARIABLE S VARCHAR(40);
BEGIN
  N = 1;
  SUSPEND;
  IF (K = 1) THEN EXECUTE STATEMENT 'SELEC 1 FROM RDB$DATABASE';
  IF (K = 2) THEN EXECUTE STATEMENT 'UPDATE LP_NOPE SET X = 1';
  IF (K = 3) THEN EXECUTE STATEMENT 'INSERT INTO LP_LOG (ID) VALUES (''x'')';
  IF (K = 4) THEN EXECUTE STATEMENT 'EXECUTE PROCEDURE LP_RET 1';
  IF (K = 5) THEN EXECUTE STATEMENT 'COMMIT';
  IF (K = 6) THEN EXECUTE STATEMENT S;
  IF (K = 7) THEN EXECUTE STATEMENT 'SELECT ID FROM LP_ITEMS WHERE ID = ?' INTO :N;
  N = 2;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_WHEN RETURNS (N INTEGER, C INTEGER) AS
BEGIN
  N = 0;
  BEGIN
    INSERT INTO LP_LOG (ID, MSG) VALUES (77, 'w');
    EXECUTE STATEMENT 'UPDATE LP_NOPE SET X = 1';
  WHEN SQLCODE -204 DO N = 204;
  WHEN ANY DO N = N + 1;
  END
  SELECT COUNT(*) FROM LP_LOG WHERE ID = 77 INTO :C;
  BEGIN
    EXECUTE STATEMENT 'SELEC 1';
  WHEN GDSCODE exec_stmt2_execute DO C = C + 853;
  END
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_FETCH RETURNS (X INTEGER, K INTEGER) AS
BEGIN
  X = 99;
  K = 0;
  EXECUTE STATEMENT 'SELECT CAST(V AS INTEGER) FROM LP_ES_T ORDER BY ID' INTO :X;
  FOR EXECUTE STATEMENT 'SELECT CAST(V AS INTEGER) FROM LP_ES_T ORDER BY ID' INTO :X DO
    K = K + X;
  X = -X;
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_DDL RETURNS (N INTEGER) AS
BEGIN
  N = 0;
  BEGIN
    EXECUTE STATEMENT 'CREATE TABLE LP_ES_X (ID INTEGER)';
  WHEN ANY DO N = 1;
  END
  SUSPEND;
END`,
		`CREATE PROCEDURE LP_ES_TYPE RETURNS (D DOUBLE PRECISION) AS
BEGIN
  EXECUTE STATEMENT 'SELECT ID FROM LP_ES_T WHERE ID = 1' INTO :D;
  SUSPEND;
END`,
	}
}

type lpDynamicCase struct {
	name, sql, failAt string
	check             func(t *testing.T, result interbase.LineProfileResult)
}

// lpDynamicCases are the EXECUTE STATEMENT differential cases.
func lpDynamicCases() []lpDynamicCase {
	cases := []lpDynamicCase{
		{name: "EXECUTE STATEMENT INTO", sql: "select * from LP_ES_SING(2)"},
		{name: "EXECUTE STATEMENT INTO without a row", sql: "select * from LP_ES_SING(9)"},
		{name: "FOR EXECUTE STATEMENT over a table", sql: "select * from LP_ES_FOR"},
		{name: "FOR EXECUTE STATEMENT over a procedure", sql: "select * from LP_ES_FORPROC",
			check: func(t *testing.T, result interbase.LineProfileResult) {
				routine := result.Routines[0]
				loop := routine.Nodes[lpNodeAt(t, routine, "FOR EXECUTE STATEMENT")]
				if !strings.HasSuffix(loop.SQL, " FOR UPDATE") || loop.DynamicSQL != "SELECT I FROM LP_MK_GEN(5)" || loop.DynamicTexts != 1 {
					t.Fatalf("FOR EXECUTE STATEMENT node %+v", loop)
				}
			}},
		{name: "dynamic DML", sql: "select * from LP_ES_DML(2)",
			check: func(t *testing.T, result interbase.LineProfileResult) {
				routine := result.Routines[0]
				update := routine.Nodes[lpNodeAt(t, routine, "'UPDATE LP_ITEMS")]
				if update.Metrics.Rows != 2 || update.DynamicSQL != "UPDATE LP_ITEMS SET CODE = 'es' WHERE ID <= 2" {
					t.Fatalf("UPDATE node %+v, want 2 rows affected", update)
				}
			}},
		{name: "distinct texts", sql: "select * from LP_ES_LOOP(3)", check: lpCheckDynamicTexts(3)},
		{name: "distinct texts beyond the cache", sql: "select * from LP_ES_LOOP(40)", check: lpCheckDynamicTexts(40)},
		{name: "errors caught", sql: "select * from LP_ES_WHEN"},
		{name: "fetch errors ignored", sql: "select * from LP_ES_FETCH"},
	}
	for k, marker := range map[int]string{
		1: "'SELEC 1", 2: "'UPDATE LP_NOPE", 3: "'INSERT INTO LP_LOG (ID) VALUES (''x'')'", 4: "'EXECUTE PROCEDURE LP_RET 1'",
		5: "'COMMIT'", 6: "EXECUTE STATEMENT S;", 7: "WHERE ID = ?",
	} {
		cases = append(cases, lpDynamicCase{name: fmt.Sprintf("error %d", k), sql: fmt.Sprintf("select * from LP_ES_ERR(%d)", k), failAt: marker})
	}
	return cases
}

func lpCheckDynamicTexts(want int) func(t *testing.T, result interbase.LineProfileResult) {
	return func(t *testing.T, result interbase.LineProfileResult) {
		t.Helper()
		routine := result.Routines[0]
		node := routine.Nodes[lpNodeAt(t, routine, "EXECUTE STATEMENT")]
		last := fmt.Sprintf("SELECT ID + %d FROM LP_ES_T WHERE ID = 1", want)
		if node.DynamicTexts != want || node.DynamicSQL != last || node.SQL != last || node.Metrics.Hits != int64(want) {
			t.Fatalf("EXECUTE STATEMENT node %+v, want %d texts", node, want)
		}
		if result.Prepare <= 0 {
			t.Fatal("no prepare overhead")
		}
	}
}

// TestLineProfileExecuteStatementCopy checks that a column InterBase would
// copy into a target of another storage fails the run rather than yielding
// a value native PSQL would not.
func TestLineProfileExecuteStatementCopy(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpFixture(t, dialect)
			const query = "select * from LP_ES_TYPE"
			analysis, err := interbase.AnalyzeLineProfile(ctx, conn, query, dialect, nil)
			if err != nil || len(analysis.Problems) != 0 || !analysis.MayWrite {
				t.Fatalf("analysis %+v, %v; want no problem and MayWrite", analysis, err)
			}
			result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: query, Dialect: dialect})
			if err != nil {
				t.Fatalf("LineProfile(): %v", err)
			}
			if result.Outcome != interbase.LineProfileFailed || result.Err == nil ||
				!strings.Contains(result.Err.Error(), "without conversion") || result.FailedAt == nil ||
				result.FailedAt.Line != lpLine(t, result.Routines[0].Source, "EXECUTE STATEMENT") {
				t.Fatalf("outcome %s at %+v: %v", result.Outcome, result.FailedAt, result.Err)
			}
		})
	}
}

// TestLineProfileExecuteStatementDDL checks Ruling R26: a DDL text is never
// run; the run fails at its line, and WHEN handlers do not catch it.
func TestLineProfileExecuteStatementDDL(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpFixture(t, dialect)
			result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{SQL: "select * from LP_ES_DDL", Dialect: dialect})
			if err != nil {
				t.Fatalf("LineProfile(): %v", err)
			}
			if result.Outcome != interbase.LineProfileFailed || result.Err == nil || !result.RolledBack ||
				!strings.Contains(result.Err.Error(), "does not run this kind of statement") || result.FailedAt == nil ||
				result.FailedAt.Line != lpLine(t, result.Routines[0].Source, "EXECUTE STATEMENT") {
				t.Fatalf("outcome %s at %+v: %v", result.Outcome, result.FailedAt, result.Err)
			}
			var exists int
			if err := conn.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = 'LP_ES_X'").Scan(&exists); err != nil || exists != 0 {
				t.Fatalf("LP_ES_X exists %d, %v", exists, err)
			}
		})
	}
}
