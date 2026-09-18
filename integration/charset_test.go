//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"database/sql"
	"testing"

	interbase "interbase-go"
)

// Source mapping: TestCharsetConversion.test_utf82win1250. Unlike the
// nested-cast regression, this preserves the upstream persisted-row contract.
func TestCharsetUTF8InsertReadAcrossAttachments(t *testing.T) {
	// Only the five columns used by the upstream test are needed here; their
	// names, lengths, and character sets match the original T4 schema.
	fixture, cfg, cleanup := createFixture(t, 1, `
		CREATE TABLE T4 (
			C1 INTEGER,
			C_WIN1250 CHAR(5) CHARACTER SET WIN1250,
			V_WIN1250 VARCHAR(30) CHARACTER SET WIN1250,
			C_UTF8 CHAR(5) CHARACTER SET UTF8,
			V_UTF8 VARCHAR(30) CHARACTER SET UTF8);
	`)
	var databases [2]*sql.DB
	for i, charset := range []string{"UTF8", "WIN1250"} {
		databases[i] = openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, charset, 1,
			interbase.TransactionOptions{})
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

func TestCharsetAdditionalAttachmentsRoundTrip(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3, `
		CREATE TABLE GO_CHARSET_MATRIX (
			ID INTEGER NOT NULL PRIMARY KEY,
			WIN1252_VALUE VARCHAR(40) CHARACTER SET WIN1252,
			ISO8859_1_VALUE VARCHAR(40) CHARACTER SET ISO8859_1,
			ASCII_VALUE VARCHAR(40) CHARACTER SET ASCII);`)

	attachments := make(map[string]*sql.DB, 4)
	for _, charset := range []string{"UTF8", "WIN1252", "ISO8859_1", "ASCII"} {
		attachments[charset] = openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
			charset, 3, interbase.TransactionOptions{})
	}

	ctx := readContext(t)
	cases := []struct {
		id      int64
		charset string
		column  string
		value   string
	}{
		{id: 1, charset: "WIN1252", column: "WIN1252_VALUE", value: "caf\u00e9 \u20ac"},
		{id: 2, charset: "ISO8859_1", column: "ISO8859_1_VALUE", value: "caf\u00e9"},
		{id: 3, charset: "ASCII", column: "ASCII_VALUE", value: "plain ASCII  "},
	}
	for _, test := range cases {
		t.Run(test.charset+" persisted value", func(t *testing.T) {
			db := attachments[test.charset]
			if db == nil {
				t.Fatalf("missing %s attachment", test.charset)
			}
			if _, err := db.ExecContext(ctx,
				"INSERT INTO GO_CHARSET_MATRIX (ID, "+test.column+") VALUES (?, ?)",
				test.id, test.value); err != nil {
				t.Fatalf("insert through %s attachment: %v", test.charset, err)
			}
		})
	}

	for _, test := range cases {
		for _, readerCharset := range []string{"UTF8", test.charset} {
			readerCharset := readerCharset
			t.Run(test.charset+" read through "+readerCharset, func(t *testing.T) {
				attachment := attachments[readerCharset]
				var got string
				query := "SELECT " + test.column + " FROM GO_CHARSET_MATRIX WHERE ID = ?"
				if err := attachment.QueryRowContext(ctx, query, test.id).Scan(&got); err != nil {
					t.Fatalf("read %s through %s attachment: %v", test.charset, readerCharset, err)
				}
				if got != test.value {
					t.Fatalf("%s through %s = %q, want %q", test.charset, readerCharset, got, test.value)
				}
			})
		}
	}
}

func TestCharsetAdditionalParameterRoundTrips(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	ctx := readContext(t)
	tests := []struct {
		charset string
		value   string
	}{
		{charset: "WIN1252", value: "caf\u00e9 \u20ac"},
		{charset: "ISO8859_1", value: "caf\u00e9"},
		{charset: "ASCII", value: "plain ASCII  "},
	}
	for _, test := range tests {
		t.Run(test.charset, func(t *testing.T) {
			db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
				test.charset, 3, interbase.TransactionOptions{})
			var got string
			query := "SELECT CAST(? AS VARCHAR(40) CHARACTER SET " + test.charset + ") FROM RDB$DATABASE"
			if err := db.QueryRowContext(ctx, query, test.value).Scan(&got); err != nil {
				t.Fatalf("%s parameter query: %v", test.charset, err)
			}
			if got != test.value {
				t.Fatalf("%s parameter = %q, want %q", test.charset, got, test.value)
			}
		})
	}
}

func TestCharsetASCIIRejectsNonRepresentableParameter(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"ASCII", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	var got string
	err := db.QueryRowContext(ctx,
		"SELECT CAST(? AS VARCHAR(40) CHARACTER SET ASCII) FROM RDB$DATABASE",
		"caf\u00e9").Scan(&got)
	if err == nil {
		t.Fatalf("ASCII accepted non-representable parameter as %q", got)
	}
}

func TestCharsetASCIIRejectsNonRepresentableStoredValue(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3, `
CREATE TABLE GO_ASCII_NEGATIVE (
    ID INTEGER NOT NULL PRIMARY KEY,
    VALUE_TEXT VARCHAR(40) CHARACTER SET ASCII);`)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"ASCII", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_ASCII_NEGATIVE (ID, VALUE_TEXT) VALUES (?, ?)", 1, "caf\u00e9"); err == nil {
		t.Fatal("ASCII accepted non-representable stored value")
	}
}
