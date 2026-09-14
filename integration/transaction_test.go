//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
)

func registerTransactionRollback(t *testing.T, tx *sql.Tx) {
	t.Helper()
	t.Cleanup(func() {
		if err := tx.Rollback(); err != nil && !errors.Is(err, sql.ErrTxDone) {
			t.Errorf("rollback transaction cleanup: %v", err)
		}
	})
}

// Source mapping: TestTransaction.test_cursor and
// DatabaseAPI20Test.test_cursor_isolation.
func TestTransactionCommitVisibilityAndOwnWrites(t *testing.T) {
	db := newDatabase(t)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	ctx := readContext(t)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)

	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open separate reader: %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close separate reader: %v", err)
		}
	})

	result, err := tx.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(42), "committed")
	if err != nil {
		t.Fatalf("transaction insert: %v", err)
	}
	requireRowsAffected(t, result, 1)

	var ownValue int64
	var ownLabel string
	if err := tx.QueryRowContext(ctx,
		"SELECT WRITE_VALUE, LABEL FROM GO_WRITE WHERE ID = ?", int64(10)).
		Scan(&ownValue, &ownLabel); err != nil {
		t.Fatalf("read own uncommitted write: %v", err)
	}
	if ownValue != 42 || ownLabel != "committed" {
		t.Fatalf("own transaction row = (%d, %q), want (42, %q)",
			ownValue, ownLabel, "committed")
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit transaction: %v", err)
	}

	var visibleValue int64
	var visibleLabel string
	if err := reader.QueryRowContext(ctx,
		"SELECT WRITE_VALUE, LABEL FROM GO_WRITE WHERE ID = ?", int64(10)).
		Scan(&visibleValue, &visibleLabel); err != nil {
		t.Fatalf("read committed row from separate attachment: %v", err)
	}
	if visibleValue != 42 || visibleLabel != "committed" {
		t.Fatalf("committed row = (%d, %q), want (42, %q)",
			visibleValue, visibleLabel, "committed")
	}
}

// Source mapping: TestTransaction.test_context_manager and
// DatabaseAPI20Test.test_cursor_isolation.
func TestTransactionRollbackVisibilityAndOwnWrites(t *testing.T) {
	db := newDatabase(t)
	db.SetMaxOpenConns(2)
	db.SetMaxIdleConns(2)
	ctx := readContext(t)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)

	reader, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open separate reader: %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close separate reader: %v", err)
		}
	})

	result, err := tx.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(11), int64(7), "rolled back")
	if err != nil {
		t.Fatalf("transaction insert: %v", err)
	}
	requireRowsAffected(t, result, 1)

	var ownValue int64
	if err := tx.QueryRowContext(ctx,
		"SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = ?", int64(11)).Scan(&ownValue); err != nil {
		t.Fatalf("read own uncommitted write: %v", err)
	}
	if ownValue != 7 {
		t.Fatalf("own transaction value = %d, want 7", ownValue)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}

	var value int64
	err = reader.QueryRowContext(ctx,
		"SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = ?", int64(11)).Scan(&value)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rolled-back row query error = %v, want sql.ErrNoRows", err)
	}
}

// Source mapping: TestTransaction.test_savepoint.
func TestTransactionSavepointPartialRollback(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)

	for _, action := range []struct {
		query         string
		args          []any
		checkAffected bool
		wantAffected  int64
	}{
		{
			query:         "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			args:          []any{int64(10), int64(1), "before savepoint"},
			checkAffected: true,
			wantAffected:  1,
		},
		{query: "SAVEPOINT go_partial"},
		{
			query:         "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			args:          []any{int64(11), int64(2), "after savepoint"},
			checkAffected: true,
			wantAffected:  1,
		},
		{query: "ROLLBACK TO SAVEPOINT go_partial"},
	} {
		result, err := tx.ExecContext(ctx, action.query, action.args...)
		if err != nil {
			t.Fatalf("savepoint action %q: %v", action.query, err)
		}
		if action.checkAffected {
			requireRowsAffected(t, result, action.wantAffected)
		}
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after savepoint rollback: %v", err)
	}

	want := []writeRow{{id: 10, value: 1, label: "before savepoint"}}
	got := queryWriteRows(t, db, ctx)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after savepoint rollback = %#v, want %#v", got, want)
	}
}

// Source mapping: TestTransaction.test_tpb and
// DatabaseAPI20Test.test_cursor_isolation.
func TestTransactionReadOnlyOption(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	writable, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin writable transaction: %v", err)
	}
	registerTransactionRollback(t, writable)
	result, err := writable.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(1), "writable seed")
	if err != nil {
		t.Fatalf("writable transaction insert: %v", err)
	}
	requireRowsAffected(t, result, 1)
	if err := writable.Commit(); err != nil {
		t.Fatalf("commit writable transaction: %v", err)
	}
	wantSeed := []writeRow{{id: 10, value: 1, label: "writable seed"}}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, wantSeed) {
		t.Fatalf("rows after writable commit = %#v, want %#v", got, wantSeed)
	}

	readOnly, err := db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
		ReadOnly:  true,
	})
	if err != nil {
		t.Fatalf("begin read-only transaction: %v", err)
	}
	registerTransactionRollback(t, readOnly)

	var country string
	if err := readOnly.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("read in read-only transaction: %v", err)
	}
	if country != "USA" {
		t.Fatalf("read-only transaction country = %q, want USA", country)
	}

	if _, err := readOnly.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(11), int64(2), "not allowed"); err == nil {
		t.Fatal("write in a read-only transaction succeeded")
	}
	var rejectedValue int64
	err = readOnly.QueryRowContext(ctx,
		"SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = ?", int64(11)).Scan(&rejectedValue)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rejected read-only row query error = %v, want sql.ErrNoRows", err)
	}
	if err := readOnly.Rollback(); err != nil {
		t.Fatalf("rollback read-only transaction: %v", err)
	}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, wantSeed) {
		t.Fatalf("rows after read-only rollback = %#v, want %#v", got, wantSeed)
	}
}

func TestTransactionRejectsSQLControlAndRollbackUndoesInsert(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)

	if _, err := tx.ExecContext(ctx, "SAVEPOINT go_allowed"); err != nil {
		t.Fatalf("savepoint should remain executable: %v", err)
	}
	if result, err := tx.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(99), "must roll back"); err != nil {
		t.Fatalf("transaction insert: %v", err)
	} else {
		requireRowsAffected(t, result, 1)
	}

	for _, query := range []string{"COMMIT", "ROLLBACK", "START TRANSACTION"} {
		if _, err := tx.ExecContext(ctx, query); err == nil {
			t.Fatalf("transaction-control SQL %q unexpectedly succeeded", query)
		}
	}
	stmt, err := tx.PrepareContext(ctx, "COMMIT")
	if err != nil {
		t.Fatalf("prepare transaction-control SQL: %v", err)
	}
	if _, err := stmt.ExecContext(ctx); err == nil {
		t.Fatal("prepared transaction-control SQL unexpectedly succeeded")
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close transaction-control statement: %v", err)
	}

	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}
	if got := queryWriteRows(t, db, ctx); len(got) != 0 {
		t.Fatalf("rows after rejected SQL control and rollback = %#v, want no rows", got)
	}
}

// Source mapping: TestTransaction.test_cursor and
// TestPreparedStatement.test_execution.
func TestTransactionPrepareContextCommit(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)")
	if err != nil {
		t.Fatalf("prepare transaction INSERT: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })

	result, err := stmt.ExecContext(ctx, int64(10), int64(1), "prepared commit")
	if err != nil {
		t.Fatalf("execute transaction INSERT: %v", err)
	}
	requireRowsAffected(t, result, 1)
	var ownLabel string
	if err := tx.QueryRowContext(ctx,
		"SELECT LABEL FROM GO_WRITE WHERE ID = ?", int64(10)).Scan(&ownLabel); err != nil {
		t.Fatalf("read own prepared write: %v", err)
	}
	if ownLabel != "prepared commit" {
		t.Fatalf("own prepared write = %q, want %q", ownLabel, "prepared commit")
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close transaction statement: %v", err)
	}
	if _, err := stmt.ExecContext(ctx, int64(11), int64(2), "closed"); err == nil {
		t.Fatal("closed transaction statement executed successfully")
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit prepared transaction: %v", err)
	}

	want := []writeRow{{id: 10, value: 1, label: "prepared commit"}}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after prepared commit = %#v, want %#v", got, want)
	}
}

// Source mapping: TestTransaction.test_cursor and
// TestPreparedStatement.test_execution.
func TestTransactionStmtContextRollback(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	dbStmt, err := db.PrepareContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)")
	if err != nil {
		t.Fatalf("prepare DB INSERT before transaction: %v", err)
	}
	t.Cleanup(func() { _ = dbStmt.Close() })

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)
	txStmt := tx.StmtContext(ctx, dbStmt)
	t.Cleanup(func() { _ = txStmt.Close() })

	result, err := txStmt.ExecContext(ctx, int64(10), int64(1), "prepared rollback")
	if err != nil {
		t.Fatalf("execute transaction statement: %v", err)
	}
	requireRowsAffected(t, result, 1)
	var ownLabel string
	if err := tx.QueryRowContext(ctx,
		"SELECT LABEL FROM GO_WRITE WHERE ID = ?", int64(10)).Scan(&ownLabel); err != nil {
		t.Fatalf("read own transaction statement write: %v", err)
	}
	if ownLabel != "prepared rollback" {
		t.Fatalf("own transaction statement write = %q, want %q", ownLabel, "prepared rollback")
	}
	if err := txStmt.Close(); err != nil {
		t.Fatalf("close transaction statement: %v", err)
	}
	if _, err := txStmt.ExecContext(ctx, int64(11), int64(2), "closed"); err == nil {
		t.Fatal("closed transaction statement executed successfully")
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback prepared transaction: %v", err)
	}

	// Check the sole writer attachment before the external visibility helper
	// releases it; detach must not conceal an ineffective rollback.
	var localCount int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_WRITE").Scan(&localCount); err != nil {
		t.Fatalf("writer query after prepared rollback: %v", err)
	}
	if localCount != 0 {
		t.Fatalf("writer row count after prepared rollback = %d, want 0", localCount)
	}
	if got := queryWriteRows(t, db, ctx); len(got) != 0 {
		t.Fatalf("rows after prepared rollback = %#v, want no rows", got)
	}
}

func TestTransactionCompletionClosesPreparedCursorSafely(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)
	stmt, err := tx.PrepareContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("prepare transaction SELECT: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })
	rows, err := stmt.QueryContext(ctx, int64(1))
	if err != nil {
		t.Fatalf("open prepared cursor: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit with active prepared cursor: %v", err)
	}
	if rows.Next() {
		t.Fatal("prepared cursor returned a row after its transaction completed")
	}
	if err := rows.Err(); err != nil && !errors.Is(err, sql.ErrTxDone) &&
		!errors.Is(err, context.Canceled) {
		t.Fatalf("prepared cursor error after transaction completion: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close completed prepared cursor: %v", err)
	}
}

func TestTransactionPreparedQueryReusesCursorBeforeCompletion(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	registerTransactionRollback(t, tx)
	stmt, err := tx.PrepareContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("prepare transaction SELECT: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })

	for iteration := 0; iteration < 3; iteration++ {
		rows, err := stmt.QueryContext(ctx, int64(1))
		if err != nil {
			t.Fatalf("early-close query %d: %v", iteration, err)
		}
		if !rows.Next() {
			t.Fatalf("early-close query %d returned no row: %v", iteration, rows.Err())
		}
		var country string
		if err := rows.Scan(&country); err != nil {
			t.Fatalf("early-close scan %d: %v", iteration, err)
		}
		if country != "USA" {
			t.Fatalf("early-close country %d = %q, want %q", iteration, country, "USA")
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("early-close rows.Close %d: %v", iteration, err)
		}
	}

	rows, err := stmt.QueryContext(ctx, int64(1))
	if err != nil {
		t.Fatalf("EOF query: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("EOF query returned no row: %v", rows.Err())
	}
	var country string
	if err := rows.Scan(&country); err != nil {
		t.Fatalf("EOF scan: %v", err)
	}
	if country != "USA" {
		t.Fatalf("EOF country = %q, want %q", country, "USA")
	}
	if rows.Next() {
		t.Fatal("EOF query returned more than one row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("EOF query error: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("EOF rows.Close: %v", err)
	}

	rows, err = stmt.QueryContext(ctx, int64(1))
	if err != nil {
		t.Fatalf("post-EOF query before transaction completion: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("post-EOF query returned no row: %v", rows.Err())
	}
	if err := rows.Scan(&country); err != nil {
		t.Fatalf("post-EOF scan: %v", err)
	}
	if country != "USA" {
		t.Fatalf("post-EOF country = %q, want %q", country, "USA")
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("post-EOF rows.Close: %v", err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit after prepared cursor reuse: %v", err)
	}
}

// Source mapping: DatabaseAPI20Test.test_commit and
// DatabaseAPI20Test.test_rollback.
func TestTransactionErrTxDoneAfterCommitAndRollback(t *testing.T) {
	for _, committed := range []bool{true, false} {
		name := "rollback"
		if committed {
			name = "commit"
		}
		t.Run(name, func(t *testing.T) {
			db := newDatabase(t)
			ctx := readContext(t)
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin transaction: %v", err)
			}
			registerTransactionRollback(t, tx)

			if committed {
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit transaction: %v", err)
				}
			} else if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback transaction: %v", err)
			}

			if _, err := tx.ExecContext(ctx,
				"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
				int64(10), int64(1), "after done"); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("Exec after %s error = %v, want sql.ErrTxDone", name, err)
			}
			rows, err := tx.QueryContext(ctx, "SELECT ID FROM GO_WRITE")
			if rows != nil {
				_ = rows.Close()
			}
			if !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("Query after %s error = %v, want sql.ErrTxDone", name, err)
			}
			if err := tx.Commit(); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("second Commit after %s error = %v, want sql.ErrTxDone", name, err)
			}
			if err := tx.Rollback(); !errors.Is(err, sql.ErrTxDone) {
				t.Fatalf("second Rollback after %s error = %v, want sql.ErrTxDone", name, err)
			}
		})
	}
}
