//go:build integration

package integration_test

import (
	"database/sql"
	"testing"

	interbase "interbase-go"
)

// TestWithTransactionWritesBesideOpenReadOnlyCursor proves decision-4's
// premise: a short write commits on the same pooled connection while a
// read-only cursor stays open and keeps fetching.
func TestWithTransactionWritesBesideOpenReadOnlyCursor(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	if _, err := db.ExecContext(ctx, "DELETE FROM GO_WRITE WHERE ID BETWEEN 9300 AND 9302"); err != nil {
		t.Fatal(err)
	}
	for id := 9300; id <= 9302; id++ {
		if _, err := db.ExecContext(ctx, "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, 0, 'before')", id); err != nil {
			t.Fatal(err)
		}
	}
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()
	read, err := pooled.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer read.Rollback()
	rows, err := read.QueryContext(ctx, "SELECT ID, LABEL FROM GO_WRITE WHERE ID BETWEEN 9300 AND 9302 ORDER BY ID")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatal("no first row")
	}
	err = interbase.WithTransaction(ctx, pooled, interbase.TransactionOptions{Isolation: sql.LevelReadCommitted, NoWait: true},
		func(tx *interbase.Transaction) error {
			affected, err := tx.Exec(ctx, "UPDATE GO_WRITE SET LABEL = ? WHERE ID = ?", "after", int64(9302))
			if err == nil && affected != 1 {
				t.Errorf("affected = %d", affected)
			}
			return err
		})
	if err != nil {
		t.Fatalf("write beside cursor: %v", err)
	}
	var labels []string
	for rows.Next() {
		var id int64
		var label string
		if err := rows.Scan(&id, &label); err != nil {
			t.Fatal(err)
		}
		labels = append(labels, label)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("cursor after write: %v", err)
	}
	// The open SELECT keeps its result values while later reads can observe the
	// committed write. Keep fetching the existing cursor to prove it remains valid.
	if len(labels) != 2 {
		t.Fatalf("remaining cursor rows = %v, want two rows", labels)
	}
	if err := read.Commit(); err != nil {
		t.Fatal(err)
	}
	var label string
	if err := pooled.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = 9302").Scan(&label); err != nil || label != "after" {
		t.Fatalf("committed label = %q, %v", label, err)
	}
}

func TestWithTransactionRollsBackOnError(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()
	if _, err := pooled.ExecContext(ctx, "DELETE FROM GO_WRITE WHERE ID = 9310"); err != nil {
		t.Fatal(err)
	}
	err = interbase.WithTransaction(ctx, pooled, interbase.TransactionOptions{NoWait: true}, func(tx *interbase.Transaction) error {
		if _, err := tx.Exec(ctx, "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (9310, 1, 'x')"); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (9310, 1, 'duplicate')")
		return err
	})
	if err == nil {
		t.Fatal("duplicate key did not fail")
	}
	var count int
	if err := pooled.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_WRITE WHERE ID = 9310").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rolled-back rows = %d, %v", count, err)
	}
}
