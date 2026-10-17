//go:build integration

package integration_test

import (
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/planparse"
	"interbase-go/psql"
)

func TestRoutinePlansArePrepareOnly(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 2*time.Minute)

	for _, statement := range []string{
		"CREATE TABLE RP_T (ID INTEGER NOT NULL PRIMARY KEY, QTY INTEGER)",
		"CREATE TABLE RP_LOG (ID INTEGER, QTY INTEGER)",
		"CREATE TRIGGER RP_T_AU FOR RP_T AFTER UPDATE AS BEGIN INSERT INTO RP_LOG (ID, QTY) VALUES (NEW.ID, NEW.QTY); END",
		"CREATE PROCEDURE RP_SEL RETURNS (ID INTEGER) AS BEGIN FOR SELECT ID FROM RP_T WHERE QTY > 0 INTO :ID DO SUSPEND; END",
		"CREATE PROCEDURE RP_NOOP AS BEGIN EXIT; END",
		"CREATE PROCEDURE RP_EXEC AS DECLARE VARIABLE C INTEGER; BEGIN SELECT COUNT(*) FROM RP_T INTO :C; UPDATE RP_T SET QTY = QTY + 1 WHERE ID = :C; EXECUTE PROCEDURE RP_NOOP; EXECUTE STATEMENT 'SELECT 1 FROM RDB$DATABASE'; END",
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("execute fixture DDL %q: %v", statement, err)
		}
	}
	for _, args := range [][]any{{int64(1), int64(10)}, {int64(2), int64(20)}} {
		if _, err := db.ExecContext(ctx, "INSERT INTO RP_T (ID, QTY) VALUES (?, ?)", args...); err != nil {
			t.Fatalf("seed RP_T: %v", err)
		}
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	procedurePlan, err := interbase.PlanRoutine(ctx, conn,
		interbase.RoutineRef{Kind: "procedure", Name: "RP_EXEC"})
	if err != nil {
		t.Fatalf("PlanRoutine(RP_EXEC): %v", err)
	}
	if len(procedurePlan.Lanes) != 4 {
		t.Fatalf("RP_EXEC lanes = %d, want 4: %+v", len(procedurePlan.Lanes), procedurePlan.Lanes)
	}
	if procedurePlan.Truncated {
		t.Fatal("RP_EXEC unexpectedly truncated")
	}
	if got := procedurePlan.Lanes[0].Kind; got != "select" || !strings.Contains(procedurePlan.Lanes[0].Plan.Raw, "RP_T") {
		t.Fatalf("first lane = %+v, want SELECT plan using RP_T", procedurePlan.Lanes[0])
	}
	if got := procedurePlan.Lanes[1]; got.Kind != "update" || !hasRoutineRef(got.Refs, "trigger", "RP_T_AU", "RP_T") {
		t.Fatalf("update lane = %+v, want RP_T_AU trigger ref", got)
	}
	if got := procedurePlan.Lanes[2]; got.Kind != "executeProcedure" ||
		!hasRoutineRef(got.Refs, "procedure", "RP_NOOP", "") {
		t.Fatalf("execute-procedure lane = %+v, want RP_NOOP ref", got)
	}
	if got := procedurePlan.Lanes[3]; got.Kind != "executeStatement" || got.Message != "dynamic SQL" {
		t.Fatalf("execute-statement lane = %+v, want dynamic SQL message", got)
	}

	triggerPlan, err := interbase.PlanRoutine(ctx, conn,
		interbase.RoutineRef{Kind: "trigger", Name: "RP_T_AU", Relation: "RP_T"})
	if err != nil {
		t.Fatalf("PlanRoutine(RP_T_AU): %v", err)
	}
	if len(triggerPlan.Lanes) != 1 || triggerPlan.Lanes[0].Kind != "insert" ||
		psql.ExtractStatementRefs(triggerPlan.Lanes[0].SQL).Target != "RP_LOG" {
		t.Fatalf("RP_T_AU plan = %+v, want one INSERT lane targeting RP_LOG", triggerPlan.Lanes)
	}

	triggerRefs, err := interbase.StatementRoutines(ctx, conn, "UPDATE RP_T SET QTY = 1")
	if err != nil {
		t.Fatalf("StatementRoutines(UPDATE): %v", err)
	}
	if !reflect.DeepEqual(triggerRefs, []interbase.RoutineRef{{
		Kind: "trigger", Name: "RP_T_AU", Relation: "RP_T", Event: "after update",
	}}) {
		t.Fatalf("StatementRoutines(UPDATE) = %+v, want RP_T_AU", triggerRefs)
	}
	procedureRefs, err := interbase.StatementRoutines(ctx, conn, "SELECT * FROM RP_SEL")
	if err != nil {
		t.Fatalf("StatementRoutines(SELECT): %v", err)
	}
	if !reflect.DeepEqual(procedureRefs, []interbase.RoutineRef{{Kind: "procedure", Name: "RP_SEL"}}) {
		t.Fatalf("StatementRoutines(SELECT) = %+v, want RP_SEL", procedureRefs)
	}

	const query = "SELECT * FROM RP_T T WHERE ID = 1"
	raw, err := interbase.Plan(ctx, conn, query)
	if err != nil {
		t.Fatalf("Plan(RP_T): %v", err)
	}
	description, err := interbase.DescribePlan(ctx, conn, query, raw)
	if err != nil {
		t.Fatalf("DescribePlan(RP_T): %v", err)
	}
	if description.Tree == nil || description.Problem != "" {
		t.Fatalf("DescribePlan result = %+v, want a parsed plan", description)
	}
	if !treeHasRelation(description.Tree, "RP_T") {
		t.Fatalf("plan relation was not resolved to RP_T: %+v", description.Tree)
	}
	foundPrimary := false
	for _, index := range description.Indexes {
		if strings.HasPrefix(index.Name, "RDB$PRIMARY") && index.Relation == "RP_T" && index.Unique &&
			reflect.DeepEqual(index.Columns, []string{"ID"}) {
			foundPrimary = true
		}
	}
	if !foundPrimary {
		t.Fatalf("plan indexes = %+v, want unique RP_T primary index on ID", description.Indexes)
	}

	var count, logCount int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM RP_T").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM RP_LOG").Scan(&logCount); err != nil {
		t.Fatal(err)
	}
	if count != 2 || logCount != 0 {
		t.Fatalf("planning changed fixture rows: RP_T count=%d RP_LOG count=%d", count, logCount)
	}
	rows, err := conn.QueryContext(ctx, "SELECT ID, QTY FROM RP_T ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var got [][2]int64
	for rows.Next() {
		var row [2]int64
		if err := rows.Scan(&row[0], &row[1]); err != nil {
			t.Fatal(err)
		}
		got = append(got, row)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if want := [][2]int64{{1, 10}, {2, 20}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("RP_T rows after planning = %v, want %v", got, want)
	}
}

func hasRoutineRef(refs []interbase.RoutineRef, kind, name, relation string) bool {
	for _, ref := range refs {
		if ref.Kind == kind && ref.Name == name && ref.Relation == relation {
			return true
		}
	}
	return false
}

func treeHasRelation(tree *planparse.Tree, relation string) bool {
	if tree == nil {
		return false
	}
	var visit func(*planparse.Node) bool
	visit = func(node *planparse.Node) bool {
		if node == nil {
			return false
		}
		if node.Kind == "table" && node.Relation == relation {
			return true
		}
		for _, child := range node.Children {
			if visit(child) {
				return true
			}
		}
		return false
	}
	for _, root := range tree.Roots {
		if visit(root) {
			return true
		}
	}
	return false
}
