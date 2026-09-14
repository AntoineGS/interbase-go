//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	interbase "interbase-go"
	"interbase-go/internal/testfixture"
)

// Source mapping: TestCharsetConversion.test_utf82win1250. Unlike the
// nested-cast regression, this preserves the upstream persisted-row contract.
func TestCharsetUTF8InsertReadAcrossAttachments(t *testing.T) {
	cfg, err := testfixture.FromEnv()
	if err != nil {
		t.Fatalf("fixture configuration: %v", err)
	}
	setupContext, cancel := context.WithTimeout(context.Background(), fixtureSetupTimeout)
	defer cancel()
	// Only the five columns used by the upstream test are needed here; their
	// names, lengths, and character sets match the original T4 schema.
	fixture, createErr := testfixture.Create(setupContext, cfg, `
		CREATE TABLE T4 (
			C1 INTEGER,
			C_WIN1250 CHAR(5) CHARACTER SET WIN1250,
			V_WIN1250 VARCHAR(30) CHARACTER SET WIN1250,
			C_UTF8 CHAR(5) CHARACTER SET UTF8,
			V_UTF8 VARCHAR(30) CHARACTER SET UTF8);
	`)
	var databases [2]*sql.DB
	if fixture != nil {
		t.Cleanup(func() {
			closeDatabases := func() error {
				var closeErr error
				for _, db := range databases {
					if db != nil {
						closeErr = errors.Join(closeErr, db.Close())
					}
				}
				return closeErr
			}
			if err := cleanupDatabaseAndFixture(closeDatabases, fixture.Close, fixture.Path); err != nil {
				t.Errorf("charset fixture cleanup: %v", err)
			}
		})
	}
	if createErr != nil {
		t.Fatalf("create charset fixture: %v", createErr)
	}
	for i, charset := range []string{"UTF8", "WIN1250"} {
		connector, err := interbase.NewConnector(interbase.Config{
			Database: fixture.Path,
			User:     cfg.User,
			Password: cfg.Password,
			Charset:  charset,
		})
		if err != nil {
			t.Fatalf("%s connector: %v", charset, err)
		}
		databases[i] = sql.OpenDB(connector)
		databases[i].SetMaxOpenConns(1)
		databases[i].SetMaxIdleConns(1)
		if err := pingDatabase(setupContext, databases[i]); err != nil {
			t.Fatalf("%s fixture ping: %v", charset, err)
		}
	}

	want := struct {
		id                     int64
		fixedWIN, varyingWIN   string
		fixedUTF8, varyingUTF8 string
	}{
		id:          1,
		fixedWIN:    "abcde",
		varyingWIN:  "012345678901234567890123456789",
		fixedUTF8:   "\u011b\u0161\u010d\u0159\u017e",
		varyingUTF8: "\u011b\u0161\u010d\u0159\u017e\u00fd\u00e1\u00ed\u00e9\u00fa\u016f\u010f\u0165\u0148\u00f3\u011a\u0160\u010c\u0158\u017d\u00dd\u00c1\u00cd\u00c9\u00da\u016e\u010e\u0164\u0147\u00d3",
	}
	ctx := readContext(t)
	tx, err := databases[0].BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin UTF8 insert: %v", err)
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx,
		"INSERT INTO T4 (C1, C_WIN1250, V_WIN1250, C_UTF8, V_UTF8) VALUES (?,?,?,?,?)",
		want.id, want.fixedWIN, want.varyingWIN, want.fixedUTF8, want.varyingUTF8)
	if err != nil {
		t.Fatalf("insert through UTF8 attachment: %v", err)
	}
	requireRowsAffected(t, result, 1)
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit UTF8 insert: %v", err)
	}

	for _, test := range []struct {
		charset string
		db      *sql.DB
	}{
		{charset: "WIN1250", db: databases[1]},
		{charset: "UTF8", db: databases[0]},
	} {
		t.Run(test.charset, func(t *testing.T) {
			rows, err := test.db.QueryContext(ctx,
				"SELECT C1, C_WIN1250, V_WIN1250, C_UTF8, V_UTF8 FROM T4 WHERE C1 = 1")
			if err != nil {
				t.Fatalf("query through %s attachment: %v", test.charset, err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("%s attachment returned no row: %v", test.charset, rows.Err())
			}
			var id int64
			var fixedWIN, varyingWIN, fixedUTF8, varyingUTF8 string
			if err := rows.Scan(&id, &fixedWIN, &varyingWIN, &fixedUTF8, &varyingUTF8); err != nil {
				t.Fatalf("scan through %s attachment: %v", test.charset, err)
			}
			if id != want.id || fixedWIN != want.fixedWIN || varyingWIN != want.varyingWIN ||
				fixedUTF8 != want.fixedUTF8 || varyingUTF8 != want.varyingUTF8 {
				t.Fatalf("%s row = (%d, %q, %q, %q, %q), want %#v",
					test.charset, id, fixedWIN, varyingWIN, fixedUTF8, varyingUTF8, want)
			}
			finishReadRows(t, rows)
		})
	}
}
