//go:build integration

package integration_test

import (
	"testing"

	interbase "interbase-go"
)

func TestStatementDescriptionClassifiesWithoutExecuting(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()

	cases := []struct {
		query, kind    string
		rows, mutating bool
		inputs         int
	}{
		{"/* leading */ SELECT ID FROM GO_COUNTRY WHERE ID = ?", "select", true, false, 1},
		{"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (9199, 91, 'describe only')", "insert", false, true, 0},
		{"UPDATE GO_COUNTRY SET COUNTRY = 'changed' WHERE ID = 1", "update", false, true, 0},
		{"DELETE FROM GO_COUNTRY WHERE ID = 1", "delete", false, true, 0},
		{"CREATE TABLE GO_DESCRIPTION_ONLY (ID INTEGER)", "ddl", false, true, 0},
		{"EXECUTE PROCEDURE GO_WRITE_RETURN(9199, 42)", "procedure", true, true, 0},
		{"EXECUTE PROCEDURE GO_WRITE_PROC(9199, 42)", "procedure", false, true, 0},
	}
	for _, tc := range cases {
		got, err := interbase.DescribeStatement(ctx, pooled, tc.query)
		if err != nil {
			t.Fatalf("DescribeStatement(%q): %v", tc.query, err)
		}
		if got.Kind != tc.kind || got.ReturnsRows != tc.rows || got.Mutating != tc.mutating || got.InputCount != tc.inputs {
			t.Fatalf("DescribeStatement(%q) = %+v, want %s rows=%v mutating=%v inputs=%d",
				tc.query, got, tc.kind, tc.rows, tc.mutating, tc.inputs)
		}
	}
	// This SELECT is optional server grammar, not a classification fallback.
	cte, err := interbase.DescribeStatement(ctx, pooled,
		"WITH source AS (SELECT ID FROM GO_COUNTRY) SELECT ID FROM source")
	if err != nil {
		t.Logf("fixture does not support CTE preparation: %v", err)
	} else if cte.Kind != "select" || !cte.ReturnsRows {
		t.Fatalf("CTE descriptor = %+v", cte)
	}
	var count int
	if err := pooled.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_WRITE WHERE ID = 9199").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("description executed DML/procedure; marker count = %d", count)
	}
	var name string
	if err := pooled.QueryRowContext(ctx, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = 1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "USA" {
		t.Fatalf("description modified sentinel: %q", name)
	}
}

func TestStatementDescriptionRetainsCallerTransaction(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()
	tx, err := pooled.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := interbase.DescribeStatement(ctx, pooled,
		"UPDATE GO_COUNTRY SET COUNTRY = 'unwanted' WHERE ID = 1")
	if err != nil || got.Kind != "update" || !got.Mutating {
		t.Fatalf("transaction description = %+v, %v", got, err)
	}
	var name string
	if err := tx.QueryRowContext(ctx, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = 1").Scan(&name); err != nil {
		t.Fatal(err)
	}
	if name != "USA" {
		t.Fatalf("description changed a row in caller transaction: %q", name)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("description closed caller transaction: %v", err)
	}
}
