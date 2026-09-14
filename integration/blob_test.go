//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"testing"

	interbase "interbase-go"
	"interbase-go/internal/testfixture"
)

func TestBlobTextColumnCharsetAcrossAttachments(t *testing.T) {
	cfg, err := testfixture.FromEnv()
	if err != nil {
		t.Fatalf("fixture configuration: %v", err)
	}
	cfg.Dialect = 1

	setupContext, cancel := context.WithTimeout(context.Background(), fixtureSetupTimeout)
	defer cancel()
	fixture, createErr := testfixture.Create(setupContext, cfg, fixtureSchema)
	var utf8DB, win1250DB *sql.DB
	t.Cleanup(func() {
		if utf8DB != nil {
			if err := utf8DB.Close(); err != nil {
				t.Errorf("close UTF8 database: %v", err)
			}
		}
		if win1250DB != nil {
			if err := win1250DB.Close(); err != nil {
				t.Errorf("close WIN1250 database: %v", err)
			}
		}
		if fixture != nil {
			if err := fixture.Close(); err != nil {
				t.Errorf("close BLOB fixture: %v", err)
			}
		}
	})
	if createErr != nil {
		t.Fatalf("create BLOB fixture: %v", createErr)
	}

	open := func(charset string) *sql.DB {
		t.Helper()
		connector, err := interbase.NewConnector(interbase.Config{
			Database: fixture.Path,
			User:     cfg.User,
			Password: cfg.Password,
			Charset:  charset,
			Dialect:  1,
		})
		if err != nil {
			t.Fatalf("%s connector: %v", charset, err)
		}
		db := sql.OpenDB(connector)
		db.SetMaxOpenConns(1)
		db.SetMaxIdleConns(1)
		if err := pingDatabase(setupContext, db); err != nil {
			db.Close()
			t.Fatalf("%s fixture ping: %v", charset, err)
		}
		return db
	}
	utf8DB = open("UTF8")
	win1250DB = open("WIN1250")

	ctx := readContextWithTimeout(t, longReadTestTimeout)
	_, err = utf8DB.ExecContext(ctx, `
		CREATE TABLE GO_BLOB_CHARSETS (
			ID INTEGER NOT NULL PRIMARY KEY,
			UTF8_BLOB BLOB SUB_TYPE 1 CHARACTER SET UTF8,
			WIN1250_BLOB BLOB SUB_TYPE 1 CHARACTER SET WIN1250)`)
	if err != nil {
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
