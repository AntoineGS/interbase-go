//go:build integration

package integration_test

import (
	"bytes"
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

func TestDatabaseSQLBlobReadLimitIsQueryScoped(t *testing.T) {
	db := newDatabaseWithCharset(t, "UTF8")
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	ctx := readContextWithTimeout(t, longReadTestTimeout)
	if _, err := db.ExecContext(ctx, `CREATE TABLE GO_BLOB_PREVIEW (
		ID INTEGER NOT NULL PRIMARY KEY,
		TEXT_VALUE BLOB SUB_TYPE 1 CHARACTER SET UTF8,
		BINARY_VALUE BLOB SUB_TYPE 0)`); err != nil {
		t.Fatalf("create BLOB preview table: %v", err)
	}
	text := "a界z"
	binary := []byte{0x00, 0x01, 0x02, 0x03, 0x04}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_BLOB_PREVIEW (ID, TEXT_VALUE, BINARY_VALUE) VALUES (?, ?, ?)",
		int64(1), text, binary); err != nil {
		t.Fatalf("insert BLOB preview row: %v", err)
	}

	query := "SELECT TEXT_VALUE, BINARY_VALUE FROM GO_BLOB_PREVIEW WHERE ID = ?"
	bounded := interbase.WithBlobReadLimit(ctx, 2)
	rows, err := db.QueryContext(bounded, query, int64(1))
	if err != nil {
		t.Fatalf("bounded BLOB query: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("bounded BLOB query returned no row: %v", rows.Err())
	}
	var textValue, binaryValue any
	if err := rows.Scan(&textValue, &binaryValue); err != nil {
		t.Fatalf("scan bounded BLOB row: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close bounded BLOB rows: %v", err)
	}
	textPreview, ok := interbase.DecodeBlobPreview(textValue)
	if !ok {
		t.Fatalf("bounded text value type = %T, want a decoded BLOB preview", textValue)
	}
	if string(textPreview.Prefix) != "a" || textPreview.TotalLength != int64(len(text)) || !textPreview.Truncated || !textPreview.Text || !textPreview.UTF8 {
		t.Fatalf("bounded text preview = %+v, want safe prefix \"a\", total length %d, truncated UTF-8 text", textPreview, len(text))
	}
	binaryPreview, ok := interbase.DecodeBlobPreview(binaryValue)
	if !ok {
		t.Fatalf("bounded binary value type = %T, want a decoded BLOB preview", binaryValue)
	}
	if !bytes.Equal(binaryPreview.Prefix, binary[:2]) || binaryPreview.TotalLength != int64(len(binary)) || !binaryPreview.Truncated || binaryPreview.Text {
		t.Fatalf("bounded binary preview = %+v, want prefix % X, total length %d, truncated binary", binaryPreview, binary[:2], len(binary))
	}

	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		t.Fatalf("prepare BLOB query: %v", err)
	}
	defer stmt.Close()
	rows, err = stmt.QueryContext(bounded, int64(1))
	if err != nil {
		t.Fatalf("prepared bounded BLOB query: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("prepared bounded BLOB query returned no row: %v", rows.Err())
	}
	textValue = nil
	if err := rows.Scan(&textValue, new(any)); err != nil {
		t.Fatalf("scan prepared bounded BLOB row: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close prepared bounded BLOB rows: %v", err)
	}
	if preview, ok := interbase.DecodeBlobPreview(textValue); !ok || string(preview.Prefix) != "a" || preview.TotalLength != int64(len(text)) {
		t.Fatalf("prepared text preview = (%+v, %t), want the same bounded value", preview, ok)
	}

	var fullText string
	var fullBinary []byte
	if err := db.QueryRowContext(ctx, query, int64(1)).Scan(&fullText, &fullBinary); err != nil {
		t.Fatalf("unbounded BLOB query after bounded query: %v", err)
	}
	if fullText != text || !bytes.Equal(fullBinary, binary) {
		t.Fatalf("unbounded BLOB values = (%q, % X), want (%q, % X)", fullText, fullBinary, text, binary)
	}
}

func TestDatabaseSQLBoundedBlobPreviewPreservesNoneCharsetBytes(t *testing.T) {
	db := newDatabaseWithCharset(t, "NONE")
	ctx := readContextWithTimeout(t, longReadTestTimeout)
	if _, err := db.ExecContext(ctx, `CREATE TABLE GO_BLOB_NONE_PREVIEW (
		ID INTEGER NOT NULL PRIMARY KEY,
		TEXT_VALUE BLOB SUB_TYPE 1 CHARACTER SET NONE)`); err != nil {
		t.Fatalf("create NONE BLOB preview table: %v", err)
	}
	want := []byte{'C', 'a', 'f', 0xe9}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_BLOB_NONE_PREVIEW (ID, TEXT_VALUE) VALUES (?, ?)", int64(1), string(want)); err != nil {
		t.Fatalf("insert NONE BLOB preview value: %v", err)
	}
	rows, err := db.QueryContext(interbase.WithBlobReadLimit(ctx, len(want)),
		"SELECT TEXT_VALUE FROM GO_BLOB_NONE_PREVIEW WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("bounded NONE BLOB query: %v", err)
	}
	if !rows.Next() {
		t.Fatalf("bounded NONE BLOB query returned no row: %v", rows.Err())
	}
	var value any
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("scan bounded NONE BLOB row: %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close bounded NONE BLOB rows: %v", err)
	}
	preview, ok := interbase.DecodeBlobPreview(value)
	if !ok || !bytes.Equal(preview.Prefix, want) || preview.TotalLength != int64(len(want)) ||
		preview.Truncated || !preview.Text || preview.UTF8 {
		t.Fatalf("NONE BLOB preview = (%+v, %t), want exact CP1252 bytes and UTF8=false", preview, ok)
	}
}
