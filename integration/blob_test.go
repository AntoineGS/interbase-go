//go:build integration

package integration_test

import (
	"database/sql"
	"testing"

	interbase "interbase-go"
)

func TestBlobTextColumnCharsetAcrossAttachments(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	utf8DB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "UTF8", 1,
		interbase.TransactionOptions{})
	win1250DB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "WIN1250", 1,
		interbase.TransactionOptions{})

	ctx := readContextWithTimeout(t, longReadTestTimeout)
	if _, err := utf8DB.ExecContext(ctx, `
		CREATE TABLE GO_BLOB_CHARSETS (
			ID INTEGER NOT NULL PRIMARY KEY,
			UTF8_BLOB BLOB SUB_TYPE 1 CHARACTER SET UTF8,
			WIN1250_BLOB BLOB SUB_TYPE 1 CHARACTER SET WIN1250)`); err != nil {
		t.Fatalf("create BLOB charset table: %v", err)
	}

	want := "ěščřžýáíéúůďťňó"
	if _, err := utf8DB.ExecContext(ctx,
		"INSERT INTO GO_BLOB_CHARSETS (ID, UTF8_BLOB) VALUES (?, ?)", 1, want); err != nil {
		t.Fatalf("UTF8 attachment write to UTF8 BLOB: %v", err)
	}
	if _, err := win1250DB.ExecContext(ctx,
		"INSERT INTO GO_BLOB_CHARSETS (ID, WIN1250_BLOB) VALUES (?, ?)", 2, want); err != nil {
		t.Fatalf("WIN1250 attachment write to WIN1250 BLOB: %v", err)
	}

	for _, test := range []struct {
		name   string
		column string
		id     int
	}{
		{name: "UTF8 read through WIN1250", column: "UTF8_BLOB", id: 1},
		{name: "WIN1250 read through UTF8", column: "WIN1250_BLOB", id: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			for _, db := range []*sql.DB{utf8DB, win1250DB} {
				var got string
				query := "SELECT " + test.column + " FROM GO_BLOB_CHARSETS WHERE ID = ?"
				if err := db.QueryRowContext(ctx, query, test.id).Scan(&got); err != nil {
					t.Fatalf("read %s BLOB: %v", test.column, err)
				}
				if got != want {
					t.Fatalf("%s BLOB = %q, want %q", test.column, got, want)
				}
			}
		})
	}
}
