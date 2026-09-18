//go:build integration

package integration_test

import (
	"database/sql"
	"testing"
)

func TestConnectionAllowsMultipleImplicitCursorsAndExecWhileRowsAreActive(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open pinned connection: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close pinned connection: %v", err)
		}
	})

	rows1, err := conn.QueryContext(ctx,
		"SELECT ID, COUNTRY FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		t.Fatalf("open first implicit cursor: %v", err)
	}
	t.Cleanup(func() { _ = rows1.Close() })

	rows2, err := conn.QueryContext(ctx,
		"SELECT ID, COUNTRY FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		t.Fatalf("open second implicit cursor while first is active: %v", err)
	}
	t.Cleanup(func() { _ = rows2.Close() })

	result, err := conn.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(1), "active cursors")
	if err != nil {
		t.Fatalf("execute while implicit cursors are active: %v", err)
	}
	requireRowsAffected(t, result, 1)

	assertCountryRow := func(t *testing.T, rows *sql.Rows, wantID int64, wantCountry string) {
		t.Helper()
		if !rows.Next() {
			t.Fatalf("cursor returned no row: %v", rows.Err())
		}
		var id int64
		var country string
		if err := rows.Scan(&id, &country); err != nil {
			t.Fatalf("scan cursor row: %v", err)
		}
		if id != wantID || country != wantCountry {
			t.Fatalf("cursor row = (%d, %q), want (%d, %q)",
				id, country, wantID, wantCountry)
		}
	}
	assertCountryRow(t, rows1, 1, "USA")
	assertCountryRow(t, rows2, 1, "USA")
	assertCountryRow(t, rows1, 2, "England")
	assertCountryRow(t, rows2, 2, "England")

	if err := rows1.Close(); err != nil {
		t.Fatalf("close first implicit cursor: %v", err)
	}
	if err := rows2.Close(); err != nil {
		t.Fatalf("close second implicit cursor: %v", err)
	}
}

func TestExplicitTransactionCompletionDoesNotCloseImplicitCursors(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("open pinned connection: %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close pinned connection: %v", err)
		}
	})

	implicitRows, err := conn.QueryContext(ctx,
		"SELECT ID, COUNTRY FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		t.Fatalf("open implicit cursor: %v", err)
	}
	t.Cleanup(func() { _ = implicitRows.Close() })

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin explicit transaction beside implicit cursor: %v", err)
	}
	explicitRows, err := tx.QueryContext(ctx,
		"SELECT ID, COUNTRY FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("open explicit transaction cursor: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit explicit transaction: %v", err)
	}
	if explicitRows.Next() {
		t.Fatal("explicit transaction cursor remained usable after commit")
	}
	if err := explicitRows.Close(); err != nil {
		t.Fatalf("close committed transaction cursor: %v", err)
	}

	var id int64
	var country string
	if !implicitRows.Next() {
		t.Fatalf("implicit cursor was closed by explicit transaction completion: %v", implicitRows.Err())
	}
	if err := implicitRows.Scan(&id, &country); err != nil {
		t.Fatalf("scan surviving implicit cursor: %v", err)
	}
	if id != 1 || country != "USA" {
		t.Fatalf("surviving implicit cursor row = (%d, %q), want (1, %q)", id, country, "USA")
	}
	if err := implicitRows.Close(); err != nil {
		t.Fatalf("close surviving implicit cursor: %v", err)
	}
}
