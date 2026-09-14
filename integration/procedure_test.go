//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

var wantBudgetOutput = [4]string{"3800000.00", "760000.00", "500000.00", "1500000.00"}

func requireBudgetProcedureRows(t *testing.T, rows *sql.Rows) {
	t.Helper()

	if !rows.Next() {
		t.Fatalf("procedure returned no row: %v", rows.Err())
	}
	var got [4]string
	if err := rows.Scan(&got[0], &got[1], &got[2], &got[3]); err != nil {
		t.Fatalf("scan procedure output: %v", err)
	}
	if !reflect.DeepEqual(got, wantBudgetOutput) {
		t.Fatalf("procedure output = %#v, want %#v", got, wantBudgetOutput)
	}
	if rows.Next() {
		t.Fatal("procedure returned more than one output row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("procedure EOF error: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close procedure rows: %v", err)
	}
}

// Source mapping: TestStoredProc.test_callproc.
func TestProcedureDirectQueryReturnsOneOutputRowAndEOF(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	for _, input := range []any{"100", int64(100)} {
		rows, err := db.QueryContext(ctx,
			"EXECUTE PROCEDURE GO_SUB_TOT_BUDGET(?)", input)
		if err != nil {
			t.Fatalf("direct procedure query for %T input: %v", input, err)
		}
		requireBudgetProcedureRows(t, rows)
	}
}

// Source mapping: TestStoredProc.test_callproc and TestPreparedStatement.test_execution.
func TestProcedurePreparedQueryReusesHandleForStringAndIntegerInputs(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	stmt, err := db.PrepareContext(ctx,
		"EXECUTE PROCEDURE GO_SUB_TOT_BUDGET(?)")
	if err != nil {
		t.Fatalf("prepare executable procedure: %v", err)
	}
	t.Cleanup(func() {
		if err := stmt.Close(); err != nil {
			t.Errorf("close executable procedure: %v", err)
		}
	})

	for _, input := range []any{"100", int64(100), "100"} {
		rows, err := stmt.QueryContext(ctx, input)
		if err != nil {
			t.Fatalf("prepared procedure query for %T input: %v", input, err)
		}
		requireBudgetProcedureRows(t, rows)
	}
}

func createProcedureWriteFixtures(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()

	const noOutputProcedure = `
CREATE PROCEDURE GO_WRITE_PROC (P_ID INTEGER, P_VALUE INTEGER)
AS
BEGIN
  INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL)
  VALUES (:P_ID, :P_VALUE, 'procedure');
END`
	if _, err := db.ExecContext(ctx, noOutputProcedure); err != nil {
		t.Fatalf("create no-output procedure: %v", err)
	}

	const outputProcedure = `
CREATE PROCEDURE GO_WRITE_RETURN (P_ID INTEGER, P_VALUE INTEGER)
RETURNS (OUT_VALUE INTEGER)
AS
BEGIN
  INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL)
  VALUES (:P_ID, :P_VALUE, 'procedure');
  OUT_VALUE = :P_VALUE;
  SUSPEND;
END`
	if _, err := db.ExecContext(ctx, outputProcedure); err != nil {
		t.Fatalf("create output procedure: %v", err)
	}
}

func requireProcedureOutputValue(t *testing.T, rows *sql.Rows, want int64) {
	t.Helper()

	if !rows.Next() {
		t.Fatalf("write procedure returned no row: %v", rows.Err())
	}
	var got int64
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("scan write procedure output: %v", err)
	}
	if got != want {
		t.Fatalf("write procedure output = %d, want %d", got, want)
	}
	if rows.Next() {
		t.Fatal("write procedure returned more than one row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("write procedure EOF error: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close write procedure rows: %v", err)
	}
}

func TestProcedureExecRunsZeroOutputProcedureAndPersistsSideEffect(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)

	result, err := db.ExecContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_PROC(?, ?)", int64(10), int64(42))
	if err != nil {
		t.Fatalf("execute no-output procedure: %v", err)
	}
	if result == nil {
		t.Fatal("no-output procedure returned a nil result")
	}

	want := []writeRow{{id: 10, value: 42, label: "procedure"}}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("no-output procedure side effect = %#v, want %#v", got, want)
	}
}

func TestProcedureExecRejectsOutputWithoutExecutingIt(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx,
		"EXECUTE PROCEDURE GO_SUB_TOT_BUDGET(?)", "100"); err == nil {
		t.Fatal("Exec accepted a procedure whose output would be discarded")
	}
}

func TestProcedureQueryCommitsImplicitWriteOnEOFAndEarlyClose(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)

	rows, err := db.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(10), int64(42))
	if err != nil {
		t.Fatalf("implicit procedure query: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("early-close implicit procedure rows: %v", err)
	}

	rows, err = db.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(11), int64(7))
	if err != nil {
		t.Fatalf("second implicit procedure query: %v", err)
	}
	requireProcedureOutputValue(t, rows, 7)

	want := []writeRow{
		{id: 10, value: 42, label: "procedure"},
		{id: 11, value: 7, label: "procedure"},
	}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("implicit procedure side effects = %#v, want %#v", got, want)
	}
}

func TestProcedureQueryContextCancellationBeforeRowsCloseRollsBack(t *testing.T) {
	for _, prepared := range []bool{false, true} {
		name := "direct"
		if prepared {
			name = "prepared"
		}
		t.Run(name, func(t *testing.T) {
			db := newDatabase(t)
			setupContext := readContext(t)
			createProcedureWriteFixtures(t, db, setupContext)

			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var (
				rows *sql.Rows
				stmt *sql.Stmt
				err  error
			)
			if prepared {
				stmt, err = db.PrepareContext(ctx,
					"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)")
				if err != nil {
					t.Fatalf("prepare canceled procedure: %v", err)
				}
				defer stmt.Close()
				rows, err = stmt.QueryContext(ctx, int64(10), int64(42))
			} else {
				rows, err = db.QueryContext(ctx,
					"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(10), int64(42))
			}
			if err != nil {
				t.Fatalf("query cancelable procedure: %v", err)
			}

			cancel()
			if err := rows.Close(); err != nil {
				t.Fatalf("close canceled procedure rows: %v", err)
			}
			if got := queryWriteRows(t, db, setupContext); len(got) != 0 {
				t.Fatalf("%s canceled procedure side effects = %#v, want none", name, got)
			}
		})
	}
}

func TestProcedureQueryUsesCallerOwnedExplicitTransaction(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)

	commitTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin commit transaction: %v", err)
	}
	rows, err := commitTx.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(10), int64(42))
	if err != nil {
		_ = commitTx.Rollback()
		t.Fatalf("explicit commit procedure query: %v", err)
	}
	requireProcedureOutputValue(t, rows, 42)
	if err := commitTx.Commit(); err != nil {
		t.Fatalf("commit explicit procedure transaction: %v", err)
	}

	rollbackTx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin rollback transaction: %v", err)
	}
	rows, err = rollbackTx.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(11), int64(7))
	if err != nil {
		_ = rollbackTx.Rollback()
		t.Fatalf("explicit rollback procedure query: %v", err)
	}
	requireProcedureOutputValue(t, rows, 7)
	if err := rollbackTx.Rollback(); err != nil {
		t.Fatalf("rollback explicit procedure transaction: %v", err)
	}

	want := []writeRow{{id: 10, value: 42, label: "procedure"}}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("explicit transaction procedure side effects = %#v, want %#v", got, want)
	}
}

func TestProcedureQueryFailureRollsBackAndConnectionRecovers(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)

	result, err := db.ExecContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_PROC(?, ?)", int64(10), int64(1))
	if err != nil {
		t.Fatalf("seed procedure side effect: %v", err)
	}
	if result == nil {
		t.Fatal("seed procedure returned a nil result")
	}

	if rows, err := db.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(10), int64(99)); err == nil {
		if rows != nil {
			_ = rows.Close()
		}
		t.Fatal("duplicate-key procedure unexpectedly succeeded")
	}

	rows, err := db.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(11), int64(7))
	if err != nil {
		t.Fatalf("procedure query after execution failure: %v", err)
	}
	requireProcedureOutputValue(t, rows, 7)

	want := []writeRow{
		{id: 10, value: 1, label: "procedure"},
		{id: 11, value: 7, label: "procedure"},
	}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("side effects after failed procedure recovery = %#v, want %#v", got, want)
	}
}

func TestProcedurePreparedFailureDoesNotReplayExecution(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)

	if _, err := db.ExecContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_PROC(?, ?)", int64(10), int64(1)); err != nil {
		t.Fatalf("seed procedure side effect: %v", err)
	}
	stmt, err := db.PrepareContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)")
	if err != nil {
		t.Fatalf("prepare write procedure: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })

	if rows, err := stmt.QueryContext(ctx, int64(10), int64(99)); err == nil {
		if rows != nil {
			_ = rows.Close()
		}
		t.Fatal("duplicate-key prepared procedure unexpectedly succeeded")
	}

	rows, err := stmt.QueryContext(ctx, int64(11), int64(7))
	if err != nil {
		t.Fatalf("prepared procedure after failure: %v", err)
	}
	requireProcedureOutputValue(t, rows, 7)

	want := []writeRow{
		{id: 10, value: 1, label: "procedure"},
		{id: 11, value: 7, label: "procedure"},
	}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("prepared procedure side effects after failure = %#v, want %#v", got, want)
	}
}

func TestProcedureExplicitTransactionErrorRemainsCallerOwned(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createProcedureWriteFixtures(t, db, ctx)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin explicit procedure error transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_PROC(?, ?)", int64(10), int64(1)); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed explicit transaction side effect: %v", err)
	}
	if _, err := tx.QueryContext(ctx,
		"EXECUTE PROCEDURE GO_WRITE_RETURN(?, ?)", int64(10), int64(99)); err == nil {
		t.Fatalf("duplicate-key explicit procedure unexpectedly succeeded")
	}

	var value int64
	if err := tx.QueryRowContext(ctx,
		"SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = ?", int64(10)).Scan(&value); err != nil {
		_ = tx.Rollback()
		t.Fatalf("read explicit transaction after procedure error: %v", err)
	}
	if value != 1 {
		t.Fatalf("explicit transaction value after procedure error = %d, want 1", value)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback explicit procedure error transaction: %v", err)
	}

	if got := queryWriteRows(t, db, ctx); len(got) != 0 {
		t.Fatalf("explicit procedure error transaction rows = %#v, want no rows", got)
	}
}
