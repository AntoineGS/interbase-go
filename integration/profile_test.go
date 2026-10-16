//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
)

func TestProfileRunsAndAlwaysRollsBack(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3, "")
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 2*time.Minute)
	if _, err := db.ExecContext(ctx, "CREATE TABLE PF_T (ID INTEGER NOT NULL PRIMARY KEY, V INTEGER NOT NULL)"); err != nil {
		t.Fatalf("create PF_T: %v", err)
	}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("begin PF_T insert transaction: %v", err)
	}
	statement, err := tx.PrepareContext(ctx, "INSERT INTO PF_T (ID, V) VALUES (?, ?)")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare PF_T insert: %v", err)
	}
	for id := int64(1); id <= 1000; id++ {
		if _, err := statement.ExecContext(ctx, id, id*10); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert PF_T row %d: %v", id, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close PF_T insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit PF_T inserts: %v", err)
	}

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("db.Conn(): %v", err)
	}
	defer conn.Close()
	var relationID int
	if err := conn.QueryRowContext(ctx,
		"SELECT RDB$RELATION_ID FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = 'PF_T'").Scan(&relationID); err != nil {
		t.Fatalf("read PF_T relation ID: %v", err)
	}

	t.Run("fetch all and measure activity", func(t *testing.T) {
		result, err := interbase.Profile(ctx, conn, "SELECT * FROM PF_T", interbase.ProfileOptions{
			ReadOnly: true, FetchAll: true,
		})
		if err != nil {
			t.Fatalf("Profile(): %v", err)
		}
		if result.Rows != 1000 || !result.Complete {
			t.Fatalf("rows/complete = %d/%t, want 1000/true", result.Rows, result.Complete)
		}
		if got := result.Activity.Tables[relationID].SeqReads; got != 1000 {
			t.Fatalf("PF_T SeqReads = %d, want 1000 (delta: %+v)", got, result.Activity.Tables[relationID])
		}
		if result.RelationNames[relationID] != "PF_T" {
			t.Fatalf("PF_T relation name = %q, want PF_T", result.RelationNames[relationID])
		}
		if !result.RolledBack || result.Prepare <= 0 || !result.ReturnsRows || !strings.Contains(result.RawPlan, "PF_T") {
			t.Fatalf("profile result missing plan/timing/rollback data: %+v", result)
		}
	})

	t.Run("default fetch cap", func(t *testing.T) {
		result, err := interbase.Profile(ctx, conn, "SELECT * FROM PF_T", interbase.ProfileOptions{ReadOnly: true})
		if err != nil {
			t.Fatalf("Profile(): %v", err)
		}
		if result.Rows != 256 || result.Complete {
			t.Fatalf("rows/complete = %d/%t, want 256/false", result.Rows, result.Complete)
		}
		if !result.RolledBack {
			t.Fatal("Profile() did not confirm rollback")
		}
	})

	t.Run("delete is rolled back", func(t *testing.T) {
		result, err := interbase.Profile(ctx, conn, "DELETE FROM PF_T", interbase.ProfileOptions{})
		if err != nil {
			t.Fatalf("Profile(DELETE): %v", err)
		}
		if got := result.Activity.Tables[relationID].Deletes; got != 1000 {
			t.Fatalf("PF_T Deletes = %d, want 1000", got)
		}
		if !result.RolledBack {
			t.Fatal("Profile(DELETE) did not confirm rollback")
		}
		var count int64
		if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM PF_T").Scan(&count); err != nil {
			t.Fatalf("count PF_T after Profile(DELETE): %v", err)
		}
		if count != 1000 {
			t.Fatalf("PF_T row count after Profile(DELETE) = %d, want 1000", count)
		}
	})

	t.Run("update is rolled back", func(t *testing.T) {
		result, err := interbase.Profile(ctx, conn, "UPDATE PF_T SET V = V + 1", interbase.ProfileOptions{})
		if err != nil {
			t.Fatalf("Profile(UPDATE): %v", err)
		}
		if !result.RolledBack {
			t.Fatal("Profile(UPDATE) did not confirm rollback")
		}
		var value int64
		if err := conn.QueryRowContext(ctx, "SELECT V FROM PF_T WHERE ID = 1").Scan(&value); err != nil {
			t.Fatalf("read PF_T value after Profile(UPDATE): %v", err)
		}
		if value != 10 {
			t.Fatalf("PF_T.V after Profile(UPDATE) = %d, want 10", value)
		}
	})

	t.Run("cancellation rolls back", func(t *testing.T) {
		cancelContext, cancel := context.WithCancel(ctx)
		timer := time.AfterFunc(50*time.Millisecond, cancel)
		result, err := interbase.Profile(cancelContext, conn,
			"SELECT a.ID, b.ID FROM PF_T a, PF_T b", interbase.ProfileOptions{FetchAll: true})
		timer.Stop()
		cancel()
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Profile(canceled query) error = %v, want context.Canceled", err)
		}
		if result.Rows == 0 {
			t.Fatalf("Profile was canceled before fetching any row: %+v", result)
		}
		if !result.RolledBack {
			t.Fatalf("canceled Profile did not confirm rollback: %+v", result)
		}
	})

	t.Run("caller transaction and cursor are untouched", func(t *testing.T) {
		callerTx, err := conn.BeginTx(ctx, &sql.TxOptions{ReadOnly: true, Isolation: sql.LevelReadCommitted})
		if err != nil {
			t.Fatalf("caller BeginTx(): %v", err)
		}
		rows, err := callerTx.QueryContext(ctx, "SELECT ID FROM PF_T ORDER BY ID")
		if err != nil {
			_ = callerTx.Rollback()
			t.Fatalf("caller QueryContext(): %v", err)
		}
		defer rows.Close()
		if !rows.Next() {
			_ = callerTx.Rollback()
			t.Fatalf("caller first Next(): %v", rows.Err())
		}
		var first int64
		if err := rows.Scan(&first); err != nil || first != 1 {
			_ = callerTx.Rollback()
			t.Fatalf("caller first row = %d, %v; want 1", first, err)
		}

		result, err := interbase.Profile(ctx, conn, "SELECT * FROM PF_T", interbase.ProfileOptions{ReadOnly: true})
		if err != nil {
			_ = callerTx.Rollback()
			t.Fatalf("Profile() beside caller cursor: %v", err)
		}
		if !result.RolledBack {
			_ = callerTx.Rollback()
			t.Fatal("Profile() beside caller cursor did not confirm rollback")
		}
		if !rows.Next() {
			_ = callerTx.Rollback()
			t.Fatalf("caller second Next() after Profile(): %v", rows.Err())
		}
		var second int64
		if err := rows.Scan(&second); err != nil || second != 2 {
			_ = callerTx.Rollback()
			t.Fatalf("caller second row = %d, %v; want 2", second, err)
		}
		_ = rows.Close()
		if err := callerTx.Rollback(); err != nil {
			t.Fatalf("caller Rollback(): %v", err)
		}
	})
}
