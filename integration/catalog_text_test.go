//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"strings"
	"testing"
	"unicode/utf8"

	interbase "interbase-go"
	catalogschema "interbase-go/schema"
)

const legacyProcedureSource = "/* CRÉATION */\r\nBEGIN RESULT = 1; SUSPEND; END"

func TestCatalogTextCharsetLegacyProcedure(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	setupDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "UTF8", 1,
		interbase.TransactionOptions{})
	ctx := readContextWithTimeout(t, longReadTestTimeout)
	if _, err := setupDB.ExecContext(ctx, `CREATE PROCEDURE GO_LEGACY_SOURCE RETURNS (RESULT INTEGER)
AS BEGIN RESULT = 1; SUSPEND; END`); err != nil {
		t.Fatalf("create legacy source procedure: %v", err)
	}
	raw := append([]byte("/* CR"), 0xc9)
	raw = append(raw, []byte("ATION */\r\nBEGIN RESULT = 1; SUSPEND; END")...)
	result, err := setupDB.ExecContext(ctx,
		"UPDATE RDB$PROCEDURES SET RDB$PROCEDURE_SOURCE = ? WHERE RDB$PROCEDURE_NAME = ?",
		raw, "GO_LEGACY_SOURCE")
	if err != nil {
		t.Fatalf("replace disposable fixture procedure source with legacy bytes: %v", err)
	}
	if affected, err := result.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("legacy source update affected (%d, %v), want (1, nil)", affected, err)
	}

	strictDB := openCatalogTextDB(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "")
	var strictSource string
	strictErr := strictDB.QueryRowContext(ctx,
		"SELECT RDB$PROCEDURE_SOURCE FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?",
		"GO_LEGACY_SOURCE").Scan(&strictSource)
	if strictErr == nil {
		t.Fatalf("strict catalog source read unexpectedly succeeded with %q", strictSource)
	}
	var conversionErr *interbase.Error
	if !errors.As(strictErr, &conversionErr) {
		t.Fatalf("strict catalog source error type = %T, want InterBase transliteration error SQLCODE -314", strictErr)
	}
	if conversionErr.SQLCode != -314 {
		t.Fatalf("strict catalog source SQLCODE = %d (operation %q), want transliteration SQLCODE -314", conversionErr.SQLCode, conversionErr.Operation)
	}
	legacyDB := openCatalogTextDB(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "WIN1250")
	legacyDB.SetMaxOpenConns(2)
	legacyDB.SetMaxIdleConns(2)
	assertCatalogSource := func(label string, queryer interface {
		QueryRowContext(context.Context, string, ...any) *sql.Row
	}, query string, args ...any) {
		t.Helper()
		var got string
		if err := queryer.QueryRowContext(ctx, query, args...).Scan(&got); err != nil {
			t.Fatalf("%s read: %v", label, err)
		}
		if !utf8.ValidString(got) || got != legacyProcedureSource {
			t.Fatalf("%s source = %q (valid UTF-8=%t), want exact %q", label, got, utf8.ValidString(got), legacyProcedureSource)
		}
	}
	query := "SELECT RDB$PROCEDURE_SOURCE FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?"
	assertCatalogSource("ordinary query", legacyDB, query, "GO_LEGACY_SOURCE")

	statement, err := legacyDB.PrepareContext(ctx, query)
	if err != nil {
		t.Fatalf("prepare catalog source query: %v", err)
	}
	statementClosed := false
	defer func() {
		if !statementClosed {
			_ = statement.Close()
		}
	}()
	var preparedSource string
	if err := statement.QueryRowContext(ctx, "GO_LEGACY_SOURCE").Scan(&preparedSource); err != nil {
		t.Fatalf("prepared catalog source read: %v", err)
	}
	if !utf8.ValidString(preparedSource) || preparedSource != legacyProcedureSource {
		t.Fatalf("prepared source = %q (valid UTF-8=%t), want exact %q", preparedSource, utf8.ValidString(preparedSource), legacyProcedureSource)
	}
	if err := statement.Close(); err != nil {
		t.Fatalf("close prepared catalog query: %v", err)
	}
	statementClosed = true

	tx, err := legacyDB.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("begin read-only transaction: %v", err)
	}
	txFinished := false
	defer func() {
		if !txFinished {
			_ = tx.Rollback()
		}
	}()
	assertCatalogSource("read-only transaction", tx, query, "GO_LEGACY_SOURCE")
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback read-only transaction: %v", err)
	}
	txFinished = true

	first, err := legacyDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire first pooled connection: %v", err)
	}
	firstClosed := false
	defer func() {
		if !firstClosed {
			_ = first.Close()
		}
	}()
	second, err := legacyDB.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire second pooled connection while first is held: %v", err)
	}
	secondClosed := false
	defer func() {
		if !secondClosed {
			_ = second.Close()
		}
	}()
	assertCatalogSource("first held pooled connection", first, query, "GO_LEGACY_SOURCE")
	assertCatalogSource("second held pooled connection", second, query, "GO_LEGACY_SOURCE")
	if err := second.Close(); err != nil {
		t.Fatalf("release second pooled connection: %v", err)
	}
	secondClosed = true
	if err := first.Close(); err != nil {
		t.Fatalf("release first pooled connection: %v", err)
	}
	firstClosed = true

	// These projections exercise the server SQLDA's source metadata under a
	// table alias, an additional catalog join, and a result-column alias. The
	// original catalog relation/field must remain available to the classifier.
	assertCatalogSource("aliased direct projection", legacyDB,
		"SELECT p.RDB$PROCEDURE_SOURCE AS SOURCE_ALIAS FROM RDB$PROCEDURES p WHERE p.RDB$PROCEDURE_NAME = ?",
		"GO_LEGACY_SOURCE")
	assertCatalogSource("joined projection", legacyDB,
		`SELECT p.RDB$PROCEDURE_SOURCE
FROM RDB$PROCEDURES p LEFT JOIN RDB$PROCEDURE_PARAMETERS pp
  ON pp.RDB$PROCEDURE_NAME = p.RDB$PROCEDURE_NAME
WHERE p.RDB$PROCEDURE_NAME = ?`, "GO_LEGACY_SOURCE")

	var charset sql.NullInt64
	if err := legacyDB.QueryRowContext(ctx, `
SELECT f.RDB$CHARACTER_SET_ID
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE rf.RDB$RELATION_NAME = 'RDB$PROCEDURES'
  AND rf.RDB$FIELD_NAME = 'RDB$PROCEDURE_SOURCE'`).Scan(&charset); err != nil {
		t.Fatalf("read declared procedure source charset: %v", err)
	}
	if !charset.Valid || charset.Int64 != 3 {
		t.Fatalf("procedure source declared charset = %#v, want charset ID 3 (UNICODE_FSS)", charset)
	}

	procedures, err := catalogschema.New(legacyDB).Procedures(ctx, "GO_LEGACY_SOURCE")
	if err != nil {
		t.Fatalf("schema Procedures: %v", err)
	}
	if len(procedures) != 1 || !procedures[0].Source.Valid || procedures[0].Source.String != legacyProcedureSource || !utf8.ValidString(procedures[0].Source.String) {
		t.Fatalf("schema Procedures = %#v, want one exact UTF-8 legacy-source procedure", procedures)
	}

	// C9 maps to É in both WIN1250 and WIN1252. A5 differentiates them while
	// retaining the source payload above exactly as required by the regression.
	if _, err := setupDB.ExecContext(ctx, `CREATE PROCEDURE GO_LEGACY_DISTINGUISH RETURNS (RESULT INTEGER)
AS BEGIN RESULT = 1; SUSPEND; END`); err != nil {
		t.Fatalf("create charset distinction procedure: %v", err)
	}
	distinguishing := append([]byte("/* "), 0xa5)
	distinguishing = append(distinguishing, []byte(" */ BEGIN RESULT = 1; SUSPEND; END")...)
	distinctionResult, err := setupDB.ExecContext(ctx,
		"UPDATE RDB$PROCEDURES SET RDB$PROCEDURE_SOURCE = ? WHERE RDB$PROCEDURE_NAME = ?",
		distinguishing, "GO_LEGACY_DISTINGUISH")
	if err != nil {
		t.Fatalf("set charset distinction source in disposable fixture: %v", err)
	}
	if affected, err := distinctionResult.RowsAffected(); err != nil || affected != 1 {
		t.Fatalf("charset distinction update affected (%d, %v), want (1, nil)", affected, err)
	}
	var win1250Source string
	if err := legacyDB.QueryRowContext(ctx,
		"SELECT RDB$PROCEDURE_SOURCE FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?",
		"GO_LEGACY_DISTINGUISH").Scan(&win1250Source); err != nil {
		t.Fatalf("WIN1250 distinction read: %v", err)
	}
	if !strings.Contains(win1250Source, "Ą") || strings.Contains(win1250Source, "¥") {
		t.Fatalf("WIN1250 A5 decoding = %q, want A-ogonek and not yen sign", win1250Source)
	}
	win1252DB := openCatalogTextDB(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "WIN1252")
	var win1252Source string
	if err := win1252DB.QueryRowContext(ctx,
		"SELECT RDB$PROCEDURE_SOURCE FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?",
		"GO_LEGACY_DISTINGUISH").Scan(&win1252Source); err != nil {
		t.Fatalf("WIN1252 distinction read: %v", err)
	}
	if !strings.Contains(win1252Source, "¥") || strings.Contains(win1252Source, "Ą") {
		t.Fatalf("WIN1252 A5 decoding = %q, want yen sign and not WIN1250 A-ogonek", win1252Source)
	}
}

func TestCatalogTextCharsetScope(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "UTF8", 1,
		interbase.TransactionOptions{})
	ctx := readContextWithTimeout(t, longReadTestTimeout)
	if _, err := db.ExecContext(ctx, `CREATE PROCEDURE GO_UTF8_SOURCE RETURNS (RESULT INTEGER)
AS BEGIN /* ěšč */ RESULT = 1; SUSPEND; END`); err != nil {
		t.Fatalf("create Unicode fixture procedure: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE GO_SCOPE_BLOB (
		ID INTEGER NOT NULL PRIMARY KEY,
		DATA_VALUE BLOB SUB_TYPE 1 CHARACTER SET UTF8)`); err != nil {
		t.Fatalf("create UTF8 text BLOB table: %v", err)
	}
	const unicodeValue = "ěšč 😀"
	if _, err := db.ExecContext(ctx, "INSERT INTO GO_SCOPE_BLOB (ID, DATA_VALUE) VALUES (?, ?)", 1, unicodeValue); err != nil {
		t.Fatalf("insert UTF8 text BLOB: %v", err)
	}
	defaultDB := openCatalogTextDB(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "")
	var procedureSource string
	if err := defaultDB.QueryRowContext(ctx,
		"SELECT RDB$PROCEDURE_SOURCE FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?",
		"GO_UTF8_SOURCE").Scan(&procedureSource); err != nil {
		t.Fatalf("default Unicode procedure source read: %v", err)
	}
	const wantUnicodeProcedureSource = " BEGIN /* ěšč */ RESULT = 1; SUSPEND; END"
	if procedureSource != wantUnicodeProcedureSource || !utf8.ValidString(procedureSource) {
		t.Fatalf("default Unicode procedure source = %q (valid UTF-8=%t), want exact %q", procedureSource, utf8.ValidString(procedureSource), wantUnicodeProcedureSource)
	}

	legacyEnabledDB := openCatalogTextDB(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "WIN1250")
	for _, query := range []string{
		"SELECT DATA_VALUE FROM GO_SCOPE_BLOB WHERE ID = ?",
		"SELECT DATA_VALUE AS RDB$PROCEDURE_SOURCE FROM GO_SCOPE_BLOB WHERE ID = ?",
	} {
		var got string
		if err := legacyEnabledDB.QueryRowContext(ctx, query, 1).Scan(&got); err != nil {
			t.Fatalf("read unrelated UTF8 text BLOB with query %q: %v", query, err)
		}
		if got != unicodeValue {
			t.Fatalf("unrelated UTF8 BLOB via %q = %q, want %q", query, got, unicodeValue)
		}
	}

	attachment, err := interbase.Open(ctx, interbase.Config{
		Database: fixture.ConnectionString(), User: cfg.User, Password: cfg.Password,
		Dialect: 1, Charset: "UTF8", CatalogTextCharset: "WIN1250",
	})
	if err != nil {
		t.Fatalf("open direct attachment with catalog override: %v", err)
	}
	cleanup.addAttachment(attachment)
	directTx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct text BLOB transaction: %v", err)
	}
	t.Cleanup(func() { _ = directTx.Rollback() })
	cursor, err := directTx.Query(ctx, "SELECT DATA_VALUE FROM GO_SCOPE_BLOB WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("query direct user text BLOB: %v", err)
	}
	hasRow, err := cursor.Next(ctx)
	if err != nil || !hasRow {
		_ = cursor.Close()
		t.Fatalf("direct user text BLOB Next() = (%t, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	closeErr := cursor.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("read/close direct user text BLOB row: row=%v close=%v", err, closeErr)
	}
	reference, ok := row[0].Value.(interbase.BlobRef)
	if !ok {
		t.Fatalf("direct user BLOB value type = %T, want BlobRef", row[0].Value)
	}
	reader, err := directTx.OpenBlob(ctx, reference)
	if err != nil {
		t.Fatalf("open direct user BLOB: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr = reader.Close()
	if readErr != nil || closeErr != nil || string(got) != unicodeValue {
		t.Fatalf("direct UTF8 BLOB with catalog override = %q, read=%v close=%v; want %q", got, readErr, closeErr, unicodeValue)
	}
}

func openCatalogTextDB(t *testing.T, cleanup *fixtureCleanup, database, user, password, catalogCharset string) *sql.DB {
	t.Helper()
	connector, err := interbase.NewConnector(interbase.Config{
		Database: database, User: user, Password: password,
		Dialect: 1, Charset: "UTF8", CatalogTextCharset: catalogCharset,
	})
	if err != nil {
		t.Fatalf("catalog-text connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cleanup.addAttachment(db)
	if err := pingDatabase(readContext(t), db); err != nil {
		t.Fatalf("catalog-text fixture pool ping: %v", err)
	}
	return db
}
