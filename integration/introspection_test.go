//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"

	interbase "interbase-go"
)

func TestIntrospectionDiagnosticsReportsDialect(t *testing.T) {
	for _, dialect := range []int{1, 3} {
		t.Run(fmt.Sprintf("dialect%d", dialect), func(t *testing.T) {
			db := newDatabaseWithDialect(t, dialect)
			ctx := readContext(t)
			pooled, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("DB.Conn(): %v", err)
			}
			defer pooled.Close()

			diagnostics, err := interbase.Diagnostics(ctx, pooled)
			if err != nil {
				t.Fatalf("pooled Diagnostics(): %v", err)
			}
			if diagnostics.SQLDialect != int64(dialect) {
				t.Fatalf("SQLDialect = %d, want %d", diagnostics.SQLDialect, dialect)
			}
			if diagnostics.ClientVersion == "" || diagnostics.ServerVersion == "" ||
				diagnostics.PageSize <= 0 {
				t.Fatalf("Diagnostics = %#v, missing expected native values", diagnostics)
			}
		})
	}
}

func TestIntrospectionPlanReturnsSelectPlan(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	plan, err := interbase.Plan(ctx, pooled, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("pooled Plan(): %v", err)
	}
	if plan == "" {
		t.Fatal("pooled Plan() returned an empty plan for a SELECT")
	}
}

func TestIntrospectionDescribeInputsReportsTypesWithoutExecutingDML(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	descriptors, err := interbase.DescribeInputs(ctx, pooled,
		"SELECT ID FROM GO_COUNTRY WHERE ID = ? AND COUNTRY = ?")
	if err != nil {
		t.Fatalf("pooled DescribeInputs(): %v", err)
	}
	if len(descriptors) != 2 || descriptors[0].Kind != "INTEGER" || descriptors[1].Kind != "VARCHAR" {
		t.Fatalf("input descriptors = %+v, want INTEGER then VARCHAR", descriptors)
	}

	const probeID = 9199
	var before int64
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_WRITE WHERE ID = ?", probeID).Scan(&before); err != nil {
		t.Fatalf("count probe rows before DML prepare: %v", err)
	}
	if before != 0 {
		t.Fatalf("probe rows before DML prepare = %d, want 0", before)
	}
	if _, err := interbase.DescribeInputs(ctx, pooled,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (9199, 91, 'describe only')"); err != nil {
		t.Fatalf("DescribeInputs() for read-only DML prepare: %v", err)
	}
	var after int64
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_WRITE WHERE ID = ?", probeID).Scan(&after); err != nil {
		t.Fatalf("count probe rows after DML prepare: %v", err)
	}
	if after != before {
		t.Fatalf("probe rows after DML prepare = %d, want unchanged count %d", after, before)
	}
}

// TestIntrospectionPlanForDMLDoesNotExecuteImplicitly establishes only that the
// read-only default path is safe. With no explicit transaction the native
// prepare transaction is read-only, so a hypothetical execute would be refused
// by the engine and these assertions would pass for the wrong reason.
// TestIntrospectionPlanInsideWritableTransaction removes that escape.
func TestIntrospectionPlanForDMLDoesNotExecuteImplicitly(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	first := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	second := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	pooled, err := first.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	var sentinel string
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&sentinel); err != nil {
		t.Fatalf("read sentinel row: %v", err)
	}
	if sentinel != "USA" {
		t.Fatalf("sentinel COUNTRY = %q, want USA", sentinel)
	}

	for _, statement := range []string{
		"UPDATE GO_COUNTRY SET COUNTRY = 'changed' WHERE ID = 1",
		"DELETE FROM GO_COUNTRY WHERE ID = 1",
	} {
		plan, err := interbase.Plan(ctx, pooled, statement)
		if err != nil {
			t.Fatalf("pooled Plan(%q): %v", statement, err)
		}
		// An empty DML plan is permitted, so the plan string is evidence of
		// nothing and is only logged.
		t.Logf("plan for %q: %q", statement, plan)
	}

	var country string
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("re-read sentinel row: %v", err)
	}
	if country != "USA" {
		t.Fatalf("COUNTRY after planning DML = %q, want USA", country)
	}
	var rowCount int64
	if err := pooled.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&rowCount); err != nil {
		t.Fatalf("count sentinel row: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("row count for ID = 1 after planning DML = %d, want 1", rowCount)
	}

	var separateCountry string
	if err := second.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&separateCountry); err != nil {
		t.Fatalf("read sentinel row from the second attachment: %v", err)
	}
	if separateCountry != "USA" {
		t.Fatalf("COUNTRY from the second attachment = %q, want USA", separateCountry)
	}
}

func TestIntrospectionPlanLeavesNoOpenTransaction(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	first := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	second := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	pooled, err := first.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	if _, err := interbase.Plan(ctx, pooled, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?"); err != nil {
		t.Fatalf("pooled Plan(): %v", err)
	}
	if _, err := pooled.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(9101), int64(91), "after plan"); err != nil {
		t.Fatalf("insert after pooled Plan(): %v", err)
	}

	var label string
	if err := second.QueryRowContext(ctx,
		"SELECT LABEL FROM GO_WRITE WHERE ID = ?", int64(9101)).Scan(&label); err != nil {
		t.Fatalf("read the committed row from the second attachment: %v", err)
	}
	if label != "after plan" {
		t.Fatalf("LABEL = %q, want \"after plan\"", label)
	}
}

func TestIntrospectionOnClosedConn(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	if err := pooled.Close(); err != nil {
		t.Fatalf("sql.Conn.Close(): %v", err)
	}
	if _, err := interbase.Diagnostics(ctx, pooled); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("Diagnostics() on a closed conn = %v, want sql.ErrConnDone", err)
	}
	if _, err := interbase.Plan(ctx, pooled,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?"); !errors.Is(err, sql.ErrConnDone) {
		t.Fatalf("Plan() on a closed conn = %v, want sql.ErrConnDone", err)
	}
}

// TestIntrospectionPlanInsideWritableTransaction carries both the
// transaction-selection check and the load-bearing safety check, because both
// need the same writable explicit transaction.
func TestIntrospectionPlanInsideWritableTransaction(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	first := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	second := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	pooled, err := first.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn(): %v", err)
	}
	defer pooled.Close()

	tx, err := pooled.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("sql.Conn.BeginTx(): %v", err)
	}
	rolledBack := false
	defer func() {
		if !rolledBack {
			_ = tx.Rollback()
		}
	}()

	if _, err := tx.ExecContext(ctx,
		"CREATE TABLE GO_INTROSPECTION_TXPROBE (ID INTEGER NOT NULL PRIMARY KEY, TEXT_VALUE VARCHAR(32))"); err != nil {
		t.Fatalf("create probe table: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO GO_INTROSPECTION_TXPROBE (ID, TEXT_VALUE) VALUES (1, 'sentinel')"); err != nil {
		t.Fatalf("insert probe sentinel: %v", err)
	}

	// Transaction-selection proof: the table and row are still uncommitted, so
	// a Plan that started its own implicit transaction could not see the
	// metadata and prepare would fail with an unknown-table error.
	plan, err := interbase.Plan(ctx, pooled,
		"SELECT TEXT_VALUE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1")
	if err != nil {
		t.Fatalf("pooled Plan() inside the writable transaction: %v", err)
	}
	if plan == "" {
		t.Fatal("pooled Plan() returned an empty plan for a SELECT on the uncommitted probe table")
	}

	// Safety proof: the transaction the DML would have run in is writable and
	// reads its own uncommitted work, so an execute would be both permitted
	// and visible. The reads go through tx because the probe table is not
	// visible outside this transaction.
	mutations := []string{
		"UPDATE GO_INTROSPECTION_TXPROBE SET TEXT_VALUE = 'changed' WHERE ID = 1",
		"DELETE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1",
		"INSERT INTO GO_INTROSPECTION_TXPROBE VALUES (2, 'inserted')",
	}
	for _, statement := range mutations {
		mutationPlan, planErr := interbase.Plan(ctx, pooled, statement)
		if planErr != nil {
			t.Fatalf("pooled Plan(%q): %v", statement, planErr)
		}
		t.Logf("plan for %q: %q", statement, mutationPlan)
		assertTxProbeUnchanged(t, ctx, tx, fmt.Sprintf("after planning %q", statement))
	}

	// The procedure form is called out separately because the execution path
	// does treat procedures specially: ib_connection_query re-prepares an
	// implicit procedure under a write transaction (native.c:7486), while
	// ib_statement_prepare_mode has no such branch.
	if _, err := tx.ExecContext(ctx, `CREATE PROCEDURE GO_INTROSPECTION_TXPROC AS
BEGIN
  INSERT INTO GO_INTROSPECTION_TXPROBE (ID, TEXT_VALUE) VALUES (3, 'procedure');
END`); err != nil {
		t.Fatalf("create probe procedure: %v", err)
	}
	procedurePlan, err := interbase.Plan(ctx, pooled, "EXECUTE PROCEDURE GO_INTROSPECTION_TXPROC")
	if err != nil {
		t.Fatalf("pooled Plan(EXECUTE PROCEDURE): %v", err)
	}
	t.Logf("plan for EXECUTE PROCEDURE GO_INTROSPECTION_TXPROC: %q", procedurePlan)
	assertTxProbeUnchanged(t, ctx, tx, "after planning EXECUTE PROCEDURE")

	// A sql.ErrTxDone here would mean Plan had already completed the caller's
	// transaction, which would make every assertion above vacuous.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("sql.Tx.Rollback() = %v, want nil; pooled Plan completed the caller's transaction", err)
	}
	rolledBack = true

	var relationCount int64
	if err := second.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?",
		"GO_INTROSPECTION_TXPROBE").Scan(&relationCount); err != nil {
		t.Fatalf("look up the probe table from the second attachment: %v", err)
	}
	if relationCount != 0 {
		t.Fatalf("GO_INTROSPECTION_TXPROBE relation count after rollback = %d, want 0", relationCount)
	}
}

func assertTxProbeUnchanged(t *testing.T, ctx context.Context, tx *sql.Tx, stage string) {
	t.Helper()
	var sentinel string
	if err := tx.QueryRowContext(ctx,
		"SELECT TEXT_VALUE FROM GO_INTROSPECTION_TXPROBE WHERE ID = 1").Scan(&sentinel); err != nil {
		t.Fatalf("%s: read probe sentinel: %v", stage, err)
	}
	if sentinel != "sentinel" {
		t.Fatalf("%s: TEXT_VALUE for ID = 1 = %q, want \"sentinel\"", stage, sentinel)
	}
	var rowCount int64
	if err := tx.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_INTROSPECTION_TXPROBE").Scan(&rowCount); err != nil {
		t.Fatalf("%s: count probe rows: %v", stage, err)
	}
	if rowCount != 1 {
		t.Fatalf("%s: probe row count = %d, want 1", stage, rowCount)
	}
}
