//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"

	interbase "interbase-go"
)

// pendingStatementIDs lists, from another attachment, the monitored
// statements whose text contains marker. The pattern is concatenated so the
// monitoring statement does not match itself.
func pendingStatementIDs(t *testing.T, ctx context.Context, monitor *sql.Conn, marker string) []int64 {
	t.Helper()
	rows, err := monitor.QueryContext(ctx,
		"SELECT TMP$STATEMENT_ID FROM TMP$STATEMENTS WHERE TMP$SQL CONTAINING ? || ?",
		marker[:3], marker[3:])
	if err != nil {
		t.Fatalf("read monitored statements: %v", err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return ids
}

// pendingConns opens a pinned connection and, on a second attachment to the
// same fixture, a monitoring connection.
func pendingConns(t *testing.T, ctx context.Context) (*sql.Conn, *sql.Conn) {
	t.Helper()
	fixture, cfg, cleanup := createFixture(t, 3)
	open := func() *sql.Conn {
		db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
			interbase.TransactionOptions{})
		connection, err := db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = connection.Close() })
		return connection
	}
	return open(), open()
}

func TestPendingStatementQueryRunsThePreparedStatementInTheTransaction(t *testing.T) {
	ctx := readContext(t)
	pinned, monitor := pendingConns(t, ctx)
	const marker = "pending-query-marker"
	query := "SELECT COUNTRY FROM GO_COUNTRY /* " + marker + " */ WHERE ID = 1"

	pending, err := interbase.PrepareNext(ctx, pinned, query)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	if got := pending.Descriptor(); got.Kind != "select" || !got.ReturnsRows || got.InputCount != 0 {
		t.Fatalf("descriptor = %+v", got)
	}
	prepared := pendingStatementIDs(t, ctx, monitor, marker)
	if len(prepared) != 1 {
		t.Fatalf("monitored statements after PrepareNext = %v, want one", prepared)
	}

	tx, err := pinned.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted, ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatalf("no row: %v", rows.Err())
	}
	var country string
	if err := rows.Scan(&country); err != nil {
		t.Fatal(err)
	}
	if country != "USA" {
		t.Fatalf("country = %q", country)
	}
	running := pendingStatementIDs(t, ctx, monitor, marker)
	if len(running) != 1 || running[0] != prepared[0] {
		t.Fatalf("monitored statements while running = %v, want the prepared %v", running, prepared)
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if left := pendingStatementIDs(t, ctx, monitor, marker); len(left) != 0 {
		t.Fatalf("statement survived its rows: %v", left)
	}
}

func TestPendingStatementExecWritesInTheExplicitTransaction(t *testing.T) {
	ctx := readContext(t)
	pinned, _ := pendingConns(t, ctx)
	const insert = "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (9311, 1, 'pending exec')"

	pending, err := interbase.PrepareNext(ctx, pinned, insert)
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	if got := pending.Descriptor(); got.Kind != "insert" || got.ReturnsRows || !got.Mutating {
		t.Fatalf("descriptor = %+v", got)
	}
	tx, err := pinned.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	result, err := tx.ExecContext(ctx, insert)
	if err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		_ = tx.Rollback()
		t.Fatalf("affected = %d, %v", affected, err)
	}
	var inside int
	if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_WRITE WHERE ID = 9311").Scan(&inside); err != nil {
		_ = tx.Rollback()
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	var after int
	if err := pinned.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_WRITE WHERE ID = 9311").Scan(&after); err != nil {
		t.Fatal(err)
	}
	if inside != 1 || after != 0 {
		t.Fatalf("row inside/after rollback = %d/%d, want 1/0", inside, after)
	}
}

func TestPendingStatementCloseReleasesTheObjectsForDDL(t *testing.T) {
	ctx := readContext(t)
	pinned, monitor := pendingConns(t, ctx)
	if _, err := monitor.ExecContext(ctx, "CREATE TABLE GO_PENDING_DDL (ID INTEGER)"); err != nil {
		t.Fatal(err)
	}
	const marker = "pending-ddl-marker"
	pending, err := interbase.PrepareNext(ctx, pinned, "SELECT ID FROM GO_PENDING_DDL /* "+marker+" */")
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	if left := pendingStatementIDs(t, ctx, monitor, marker); len(left) != 0 {
		t.Fatalf("Close left the statement prepared: %v", left)
	}
	if _, err := monitor.ExecContext(ctx, "DROP TABLE GO_PENDING_DDL"); err != nil {
		t.Fatalf("DDL after Close: %v", err)
	}
}

func TestPendingStatementOtherTextUsesAFreshPrepare(t *testing.T) {
	ctx := readContext(t)
	pinned, monitor := pendingConns(t, ctx)
	const marker = "pending-other-marker"
	pending, err := interbase.PrepareNext(ctx, pinned, "SELECT ID FROM GO_COUNTRY /* "+marker+" */")
	if err != nil {
		t.Fatal(err)
	}
	defer pending.Close()
	var count int
	if err := pinned.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_COUNTRY").Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count == 0 {
		t.Fatal("fixture has no countries")
	}
	if left := pendingStatementIDs(t, ctx, monitor, marker); len(left) != 0 {
		t.Fatalf("another statement left the pending one prepared: %v", left)
	}
}
