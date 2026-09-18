//go:build integration

package integration_test

import (
	"database/sql"
	"strings"
	"testing"

	interbase "interbase-go"
)

const (
	sqlIndicatorNull   uint16 = 1 << 15
	sqlIndicatorInsert uint16 = 1 << 0
	sqlIndicatorUpdate uint16 = 1 << 1
	sqlIndicatorDelete uint16 = 1 << 2
)

func TestDirectChangeViewIndicatorsIncludeNullInsertUpdateDelete(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	changeViewOptions := interbase.TransactionOptions{Isolation: sql.LevelSerializable}
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, changeViewOptions)
	ctx := readContext(t)

	execAndCommit := func(query string, args ...any) {
		t.Helper()
		tx, err := attachment.BeginTx(ctx, changeViewOptions)
		if err != nil {
			t.Fatalf("begin transaction for %q: %v", query, err)
		}
		if _, err := tx.Exec(ctx, query, args...); err != nil {
			_ = tx.Rollback()
			t.Fatalf("execute %q: %v", query, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("commit %q: %v", query, err)
		}
	}

	execAndCommit(`CREATE TABLE GO_CHANGE_VIEW (
		ID INTEGER NOT NULL PRIMARY KEY,
		VALUE_TEXT VARCHAR(40))`)
	execAndCommit("INSERT INTO GO_CHANGE_VIEW (ID, VALUE_TEXT) VALUES (?, ?)", int64(1), "first")
	execAndCommit("INSERT INTO GO_CHANGE_VIEW (ID, VALUE_TEXT) VALUES (?, ?)", int64(2), "second")
	execAndCommit(`CREATE SUBSCRIPTION GO_CHANGE_SUB ON GO_CHANGE_VIEW (ID, VALUE_TEXT)
		FOR ROW (INSERT, UPDATE, DELETE)`)
	execAndCommit("GRANT SUBSCRIBE ON SUBSCRIPTION GO_CHANGE_SUB TO " + cfg.User)
	execAndCommit("SET SUBSCRIPTION GO_CHANGE_SUB ACTIVE")

	execAndCommit("UPDATE GO_CHANGE_VIEW SET VALUE_TEXT = ? WHERE ID = ?", nil, int64(2))
	execAndCommit("INSERT INTO GO_CHANGE_VIEW (ID, VALUE_TEXT) VALUES (?, ?)", int64(3), "inserted")
	execAndCommit("DELETE FROM GO_CHANGE_VIEW WHERE ID = ?", int64(1))

	tx, err := attachment.BeginTx(ctx, changeViewOptions)
	if err != nil {
		t.Fatalf("begin change-view read transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(ctx, "SET SUBSCRIPTION GO_CHANGE_SUB ACTIVE"); err != nil {
		t.Fatalf("activate change-view subscription for read: %v", err)
	}
	cursor, err := tx.Query(ctx, "SELECT ID, VALUE_TEXT FROM GO_CHANGE_VIEW")
	if err != nil {
		t.Fatalf("query change view: %v", err)
	}

	rows := make(map[int64][]interbase.Cell)
	for {
		hasRow, nextErr := cursor.Next(ctx)
		if nextErr != nil {
			t.Fatalf("change-view Next(): %v", nextErr)
		}
		if !hasRow {
			break
		}
		row, rowErr := cursor.Row()
		if rowErr != nil {
			t.Fatalf("change-view Row(): %v", rowErr)
		}
		if len(row) != 2 {
			t.Fatalf("change-view row length = %d, want 2", len(row))
		}
		id, ok := row[0].Value.(int64)
		if !ok {
			t.Fatalf("change-view ID type = %T, want int64", row[0].Value)
		}
		if _, exists := rows[id]; exists {
			t.Fatalf("change-view returned duplicate ID %d", id)
		}
		rows[id] = row
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("close change-view cursor: %v", err)
	}
	if len(rows) != 3 {
		for id, row := range rows {
			t.Logf("change-view row id=%d value=%#v indicator=%#x", id, row[1].Value, row[1].Indicator)
		}
		t.Fatalf("change-view row count = %d, want 3", len(rows))
	}

	deleted := rows[1]
	if deleted == nil || deleted[1].Indicator&sqlIndicatorDelete == 0 {
		t.Fatalf("deleted row indicator = %#x, want DELETE bit", indicatorForRow(deleted))
	}
	if deleted[1].Value != "first" {
		t.Fatalf("deleted row value = %#v, want %q", deleted[1].Value, "first")
	}

	updated := rows[2]
	if updated == nil || updated[1].Indicator&(sqlIndicatorNull|sqlIndicatorUpdate) !=
		sqlIndicatorNull|sqlIndicatorUpdate {
		t.Fatalf("updated NULL row indicator = %#x, want NULL|UPDATE bits", indicatorForRow(updated))
	}
	if updated[1].Value != nil {
		t.Fatalf("updated NULL row value = %#v, want nil", updated[1].Value)
	}

	inserted := rows[3]
	if inserted == nil || inserted[1].Indicator&sqlIndicatorInsert == 0 {
		t.Fatalf("inserted row indicator = %#x, want INSERT bit", indicatorForRow(inserted))
	}
	if inserted[1].Value != "inserted" {
		t.Fatalf("inserted row value = %#v, want %q", inserted[1].Value, "inserted")
	}
}

func indicatorForRow(row []interbase.Cell) uint16 {
	if len(row) < 2 {
		return 0
	}
	return row[1].Indicator
}

func TestDatabaseSQLRejectsChangeViewActivationWithoutPoisoningPool(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "UTF8", 3,
		interbase.TransactionOptions{})
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, `CREATE TABLE GO_STANDARD_CHANGE_VIEW (
		ID INTEGER NOT NULL PRIMARY KEY,
		VALUE_TEXT VARCHAR(40))`); err != nil {
		t.Fatalf("create standard change-view table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_STANDARD_CHANGE_VIEW (ID, VALUE_TEXT) VALUES (?, ?)", int64(1), "ordinary"); err != nil {
		t.Fatalf("insert standard change-view row: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE SUBSCRIPTION GO_STANDARD_CHANGE_SUB
		ON GO_STANDARD_CHANGE_VIEW (ID, VALUE_TEXT) FOR ROW (INSERT, UPDATE, DELETE)`); err != nil {
		t.Fatalf("create standard change-view subscription: %v", err)
	}
	if _, err := db.ExecContext(ctx, "GRANT SUBSCRIBE ON SUBSCRIPTION GO_STANDARD_CHANGE_SUB TO "+cfg.User); err != nil {
		t.Fatalf("grant standard change-view subscription: %v", err)
	}

	if _, err := db.ExecContext(ctx, "SET SUBSCRIPTION GO_STANDARD_CHANGE_SUB ACTIVE"); err == nil {
		t.Fatal("database/sql accepted change-view activation")
	} else if !strings.Contains(err.Error(), "explicit direct API") {
		t.Fatalf("change-view activation error = %v, want direct API guidance", err)
	}
	stmt, err := db.PrepareContext(ctx, "SET SUBSCRIPTION GO_STANDARD_CHANGE_SUB ACTIVE")
	if err == nil {
		_ = stmt.Close()
		t.Fatal("database/sql prepared change-view activation")
	} else if !strings.Contains(err.Error(), "explicit direct API") {
		t.Fatalf("prepared change-view activation error = %v, want direct API guidance", err)
	}

	var id int64
	var value string
	if err := db.QueryRowContext(ctx,
		"SELECT ID, VALUE_TEXT FROM GO_STANDARD_CHANGE_VIEW").Scan(&id, &value); err != nil {
		t.Fatalf("ordinary query after rejected activation: %v", err)
	}
	if id != 1 || value != "ordinary" {
		t.Fatalf("ordinary row after rejected activation = (%d, %q), want (1, ordinary)", id, value)
	}
}
