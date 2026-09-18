//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
)

func assertDirectNoRows(t *testing.T, tx *interbase.Transaction, ctx context.Context, id int64) {
	t.Helper()
	cursor, err := tx.Query(ctx, "SELECT ID FROM GO_WRITE WHERE ID = ?", id)
	if err != nil {
		t.Fatalf("query direct row %d: %v", id, err)
	}
	hasRow, nextErr := cursor.Next(ctx)
	closeErr := cursor.Close()
	if nextErr != nil {
		t.Fatalf("query direct row %d Next(): %v", id, nextErr)
	}
	if closeErr != nil {
		t.Fatalf("query direct row %d Close(): %v", id, closeErr)
	}
	if hasRow {
		t.Fatalf("direct transaction unexpectedly contains row %d", id)
	}
}

func TestDirectReadsScalarCellsAndPreservesNullIndicator(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		t.Fatalf("begin direct transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	cursor, err := tx.Query(ctx,
		"SELECT ID, TEXT_VALUE FROM GO_DATA WHERE ID IN (?, ?) ORDER BY ID",
		int64(1), int64(3))
	if err != nil {
		t.Fatalf("direct query: %v", err)
	}
	t.Cleanup(func() { _ = cursor.Close() })

	hasRow, err := cursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("first direct Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil {
		t.Fatalf("first direct Row(): %v", err)
	}
	if len(row) != 2 || row[0].Value != int64(1) || row[1].Value != "plain" {
		t.Fatalf("first direct row = %#v, want id 1 and plain", row)
	}
	if row[0].Indicator != 0 || row[1].Indicator != 0 {
		t.Fatalf("first direct indicators = %#v, want zero indicators", row)
	}

	hasRow, err = cursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("second direct Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	row, err = cursor.Row()
	if err != nil {
		t.Fatalf("second direct Row(): %v", err)
	}
	if len(row) != 2 || row[0].Value != int64(3) || row[1].Value != nil {
		t.Fatalf("second direct row = %#v, want id 3 and NULL text", row)
	}
	if row[1].Indicator != uint16(1<<15) {
		t.Fatalf("NULL indicator = %#x, want %#x", row[1].Indicator, uint16(1<<15))
	}

	hasRow, err = cursor.Next(ctx)
	if err != nil || hasRow {
		t.Fatalf("exhausted direct Next() = (%v, %v), want (false, nil)", hasRow, err)
	}
}

func TestDirectArrayRoundTripAndDatabaseSQLRejection(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_ARRAY (
			ID INTEGER NOT NULL PRIMARY KEY,
			VALUES_ARRAY INTEGER[-1:0, 2:3])`); err != nil {
		t.Fatalf("create direct array table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_SCALED_ARRAY (
			ID INTEGER NOT NULL PRIMARY KEY,
			VALUES_ARRAY NUMERIC(18, 4)[1:3])`); err != nil {
		t.Fatalf("create direct scaled array table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_OCTETS_ARRAY (
			ID INTEGER NOT NULL PRIMARY KEY,
			VALUES_ARRAY VARCHAR(4)[1:2] CHARACTER SET OCTETS)`); err != nil {
		t.Fatalf("create direct OCTETS array table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_UTF8_ARRAY (
			ID INTEGER NOT NULL PRIMARY KEY,
			VALUES_ARRAY CHAR(4)[1:2] CHARACTER SET UTF8)`); err != nil {
		t.Fatalf("create direct UTF8 array table: %v", err)
	}
	want := interbase.Array{
		Bounds: []interbase.ArrayBound{{Lower: -1, Upper: 0}, {Lower: 2, Upper: 3}},
		Elements: []any{
			int64(10), int64(11),
			int64(12), int64(13),
		},
	}
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct array transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	if plan, err := tx.Plan(ctx,
		"SELECT VALUES_ARRAY FROM GO_DIRECT_ARRAY WHERE ID = ?"); err != nil {
		t.Fatalf("direct array Plan(): %v", err)
	} else if plan == "" {
		t.Fatal("direct array Plan() returned an empty plan")
	}

	affected, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)", int64(1), want)
	if err != nil {
		t.Fatalf("direct array insert: %v", err)
	}
	if affected != 1 {
		t.Fatalf("direct array insert affected = %d, want 1", affected)
	}

	cursor, err := tx.Query(ctx,
		"SELECT VALUES_ARRAY FROM GO_DIRECT_ARRAY WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("direct array query: %v", err)
	}
	hasRow, err := cursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("direct array Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil {
		t.Fatalf("direct array Row(): %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("close direct array cursor: %v", err)
	}
	if len(row) != 1 {
		t.Fatalf("direct array row length = %d, want 1", len(row))
	}
	got, ok := row[0].Value.(interbase.Array)
	if !ok {
		t.Fatalf("direct array value type = %T, want interbase.Array", row[0].Value)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("direct array value = %#v, want %#v", got, want)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DIRECT_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)", int64(2), want); err == nil {
		t.Fatal("database/sql accepted a direct Array value")
	}

	scaledWant := interbase.Array{
		Bounds: []interbase.ArrayBound{{Lower: 1, Upper: 3}},
		Elements: []any{
			"12345678901234.5678",
			"-12345678901234.5678",
			"0.0000",
		},
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_SCALED_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
		int64(1), scaledWant); err != nil {
		t.Fatalf("direct scaled array insert: %v", err)
	}
	scaledCursor, err := tx.Query(ctx,
		"SELECT VALUES_ARRAY FROM GO_DIRECT_SCALED_ARRAY WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("direct scaled array query: %v", err)
	}
	hasRow, err = scaledCursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("direct scaled array Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	scaledRow, err := scaledCursor.Row()
	if err != nil {
		t.Fatalf("direct scaled array Row(): %v", err)
	}
	if err := scaledCursor.Close(); err != nil {
		t.Fatalf("close direct scaled array cursor: %v", err)
	}
	if len(scaledRow) != 1 {
		t.Fatalf("direct scaled array row length = %d, want 1", len(scaledRow))
	}
	scaledGot, ok := scaledRow[0].Value.(interbase.Array)
	if !ok || !reflect.DeepEqual(scaledGot, scaledWant) {
		t.Fatalf("direct scaled array value = %#v (ok=%v), want %#v", scaledGot, ok, scaledWant)
	}

	scaledOverflow := interbase.Array{
		Bounds:   []interbase.ArrayBound{{Lower: 1, Upper: 3}},
		Elements: []any{"123456789012345.6789", "0.0000", "0.0000"},
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_SCALED_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
		int64(2), scaledOverflow); err == nil {
		t.Fatal("direct scaled array accepted a value beyond NUMERIC(18,4) precision")
	}

	integerWant := interbase.Array{
		Bounds:   []interbase.ArrayBound{{Lower: 1, Upper: 3}},
		Elements: []any{int64(12), int64(-12), int64(0)},
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_SCALED_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
		int64(3), integerWant); err != nil {
		t.Fatalf("direct scaled integer array insert: %v", err)
	}
	integerCursor, err := tx.Query(ctx,
		"SELECT VALUES_ARRAY FROM GO_DIRECT_SCALED_ARRAY WHERE ID = ?", int64(3))
	if err != nil {
		t.Fatalf("direct scaled integer array query: %v", err)
	}
	if hasRow, nextErr := integerCursor.Next(ctx); nextErr != nil || !hasRow {
		t.Fatalf("direct scaled integer array Next() = (%v, %v), want (true, nil)", hasRow, nextErr)
	}
	integerRow, err := integerCursor.Row()
	if err != nil {
		t.Fatalf("direct scaled integer array Row(): %v", err)
	}
	if err := integerCursor.Close(); err != nil {
		t.Fatalf("close direct scaled integer array cursor: %v", err)
	}
	integerGot, ok := integerRow[0].Value.(interbase.Array)
	integerExpected := interbase.Array{
		Bounds: integerWant.Bounds,
		// NUMERIC(18,4) results use the descriptor's four fractional digits,
		// regardless of whether the input element was an integer.
		Elements: []any{"12.0000", "-12.0000", "0.0000"},
	}
	if !ok || !reflect.DeepEqual(integerGot, integerExpected) {
		t.Fatalf("direct scaled integer array value = %#v (ok=%v), want %#v",
			integerGot, ok, integerExpected)
	}

	integerOverflow := interbase.Array{
		Bounds:   []interbase.ArrayBound{{Lower: 1, Upper: 3}},
		Elements: []any{int64(100000000000000), int64(0), int64(0)},
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_SCALED_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
		int64(4), integerOverflow); err == nil {
		t.Fatal("direct scaled integer array accepted a value beyond NUMERIC(18,4) precision")
	}

	octetsWant := interbase.Array{
		Bounds: []interbase.ArrayBound{{Lower: 1, Upper: 2}},
		Elements: []any{
			[]byte("abcd"),
			[]byte("EFGH"),
		},
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_OCTETS_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
		int64(1), octetsWant); err != nil {
		t.Fatalf("direct OCTETS array insert: %v", err)
	}
	octetsCursor, err := tx.Query(ctx,
		"SELECT VALUES_ARRAY FROM GO_DIRECT_OCTETS_ARRAY WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("direct OCTETS array query: %v", err)
	}
	if hasRow, nextErr := octetsCursor.Next(ctx); nextErr != nil || !hasRow {
		t.Fatalf("direct OCTETS array Next() = (%v, %v), want (true, nil)", hasRow, nextErr)
	}
	octetsRow, err := octetsCursor.Row()
	if err != nil {
		t.Fatalf("direct OCTETS array Row(): %v", err)
	}
	if err := octetsCursor.Close(); err != nil {
		t.Fatalf("close direct OCTETS array cursor: %v", err)
	}
	octetsGot, ok := octetsRow[0].Value.(interbase.Array)
	if !ok || !reflect.DeepEqual(octetsGot, octetsWant) {
		t.Fatalf("direct OCTETS array value = %#v (ok=%v), want %#v", octetsGot, ok, octetsWant)
	}

	utf8Want := interbase.Array{
		Bounds:   []interbase.ArrayBound{{Lower: 1, Upper: 2}},
		Elements: []any{"éééé", "abcd"},
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_UTF8_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
		int64(1), utf8Want); err != nil {
		t.Fatalf("direct UTF8 array insert: %v", err)
	}
	for _, value := range []string{"12345", "ééééé"} {
		overlong := interbase.Array{
			Bounds:   []interbase.ArrayBound{{Lower: 1, Upper: 2}},
			Elements: []any{value, "ok"},
		}
		if _, err := tx.Exec(ctx,
			"INSERT INTO GO_DIRECT_UTF8_ARRAY (ID, VALUES_ARRAY) VALUES (?, ?)",
			int64(2), overlong); err == nil {
			t.Fatalf("direct UTF8 array accepted %d-character value %q", len([]rune(value)), value)
		}
	}
}

func TestDirectBlobCreateOpenAndCursorReference(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_BLOB (
			ID INTEGER NOT NULL PRIMARY KEY,
			DATA_VALUE BLOB SUB_TYPE 1 CHARACTER SET UTF8)`); err != nil {
		t.Fatalf("create direct BLOB table: %v", err)
	}

	payload := bytes.Repeat([]byte("0123456789abcdef"), 5625)
	if len(payload) != 90000 {
		t.Fatalf("payload length = %d, want 90000", len(payload))
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DIRECT_BLOB (ID, DATA_VALUE) VALUES (?, ?)", int64(1), payload); err != nil {
		t.Fatalf("insert direct BLOB row: %v", err)
	}

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct BLOB transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	cursor, err := tx.Query(ctx, "SELECT DATA_VALUE FROM GO_DIRECT_BLOB WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("query direct BLOB: %v", err)
	}
	hasRow, err := cursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("direct BLOB Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil {
		t.Fatalf("direct BLOB Row(): %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("close direct BLOB cursor: %v", err)
	}
	if len(row) != 1 {
		t.Fatalf("direct BLOB row length = %d, want 1", len(row))
	}
	ref, ok := row[0].Value.(interbase.BlobRef)
	if !ok {
		t.Fatalf("direct BLOB value type = %T, want interbase.BlobRef", row[0].Value)
	}

	readBlob := func(name string, reference interbase.BlobRef) {
		t.Helper()
		reader, openErr := tx.OpenBlob(ctx, reference)
		if openErr != nil {
			t.Fatalf("open %s BLOB: %v", name, openErr)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			t.Fatalf("read %s BLOB: %v", name, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close %s BLOB reader: %v", name, closeErr)
		}
		if !bytes.Equal(got, payload) {
			t.Fatalf("%s BLOB length/content changed: got %d bytes", name, len(got))
		}
	}
	readBlob("cursor", ref)

	created, err := tx.CreateBlob(ctx, interbase.BlobOptions{Subtype: 1, Charset: "UTF8"},
		bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("create direct BLOB: %v", err)
	}
	readBlob("created", created)

	affected, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB (ID, DATA_VALUE) VALUES (?, ?)", int64(2), created)
	if err != nil {
		t.Fatalf("bind created BLOB reference: %v", err)
	}
	if affected != 1 {
		t.Fatalf("BLOB reference insert affected = %d, want 1", affected)
	}
	boundCursor, err := tx.Query(ctx, "SELECT DATA_VALUE FROM GO_DIRECT_BLOB WHERE ID = ?", int64(2))
	if err != nil {
		t.Fatalf("query bound BLOB: %v", err)
	}
	hasRow, err = boundCursor.Next(ctx)
	if err != nil || !hasRow {
		t.Fatalf("bound BLOB Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	boundRow, err := boundCursor.Row()
	if err != nil {
		t.Fatalf("bound BLOB Row(): %v", err)
	}
	if err := boundCursor.Close(); err != nil {
		t.Fatalf("close bound BLOB cursor: %v", err)
	}
	boundRef, ok := boundRow[0].Value.(interbase.BlobRef)
	if !ok {
		t.Fatalf("bound BLOB value type = %T, want interbase.BlobRef", boundRow[0].Value)
	}
	readBlob("bound", boundRef)

	if err := tx.Commit(); err != nil {
		t.Fatalf("commit direct BLOB transaction: %v", err)
	}
	if _, err := tx.OpenBlob(ctx, created); err == nil {
		t.Fatal("OpenBlob after transaction commit returned nil error")
	}
}

func TestDirectBlobReferenceBindingValidatesMetadata(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_BLOB_BIND_UTF8 (
			ID INTEGER NOT NULL PRIMARY KEY,
			DATA_VALUE BLOB SUB_TYPE 1 CHARACTER SET UTF8)`); err != nil {
		t.Fatalf("create UTF8 BLOB binding table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_BLOB_BIND_WIN1250 (
			ID INTEGER NOT NULL PRIMARY KEY,
			DATA_VALUE BLOB SUB_TYPE 1 CHARACTER SET WIN1250)`); err != nil {
		t.Fatalf("create WIN1250 BLOB binding table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_BLOB_BIND_BINARY (
			ID INTEGER NOT NULL PRIMARY KEY,
			DATA_VALUE BLOB SUB_TYPE 0)`); err != nil {
		t.Fatalf("create binary BLOB binding table: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_BINARY (ID, DATA_VALUE) VALUES (?, ?)",
		int64(1), []byte("binary source")); err != nil {
		t.Fatalf("insert binary BLOB binding source: %v", err)
	}

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin BLOB binding transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	utf8Payload := []byte("ě Ž")
	utf8Ref, err := tx.CreateBlob(ctx,
		interbase.BlobOptions{Subtype: 1, Charset: "UTF8"}, bytes.NewReader(utf8Payload))
	if err != nil {
		t.Fatalf("create UTF8 BLOB reference: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_WIN1250 (ID, DATA_VALUE) VALUES (?, ?)",
		int64(1), utf8Ref); err == nil || !strings.Contains(strings.ToLower(err.Error()), "character set") {
		t.Fatalf("UTF8 BLOB reference into WIN1250 destination error = %v, want character-set rejection", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_UTF8 (ID, DATA_VALUE) VALUES (?, ?)",
		int64(1), utf8Ref); err != nil {
		t.Fatalf("matching UTF8 BLOB reference binding: %v", err)
	}

	binaryCursor, err := tx.Query(ctx,
		"SELECT DATA_VALUE FROM GO_DIRECT_BLOB_BIND_BINARY WHERE ID = ?", int64(1))
	if err != nil {
		t.Fatalf("query binary BLOB binding source: %v", err)
	}
	if hasRow, nextErr := binaryCursor.Next(ctx); nextErr != nil || !hasRow {
		t.Fatalf("binary BLOB binding source Next() = (%v, %v), want (true, nil)", hasRow, nextErr)
	}
	binaryRow, err := binaryCursor.Row()
	if err != nil {
		t.Fatalf("read binary BLOB binding source: %v", err)
	}
	if err := binaryCursor.Close(); err != nil {
		t.Fatalf("close binary BLOB binding source: %v", err)
	}
	binaryRef, ok := binaryRow[0].Value.(interbase.BlobRef)
	if !ok {
		t.Fatalf("binary BLOB binding source value type = %T, want interbase.BlobRef", binaryRow[0].Value)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_UTF8 (ID, DATA_VALUE) VALUES (?, ?)",
		int64(2), binaryRef); err == nil || !strings.Contains(strings.ToLower(err.Error()), "subtype") {
		t.Fatalf("binary BLOB reference into text destination error = %v, want subtype rejection", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_BINARY (ID, DATA_VALUE) VALUES (?, ?)",
		int64(2), binaryRef); err != nil {
		t.Fatalf("matching binary BLOB reference binding: %v", err)
	}

	binaryPayload := []byte{'b', 0, 'i', 'n', 0, 'a', 'r', 'y'}
	createdBinaryRef, err := tx.CreateBlob(ctx,
		interbase.BlobOptions{Subtype: 0, Charset: "UTF8"}, bytes.NewReader(binaryPayload))
	if err != nil {
		t.Fatalf("create binary BLOB reference: %v", err)
	}
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_BINARY (ID, DATA_VALUE) VALUES (?, ?)",
		int64(3), createdBinaryRef); err != nil {
		t.Fatalf("matching created binary BLOB reference binding: %v", err)
	}

	_, nestedCastErr := tx.Exec(ctx,
		"INSERT INTO GO_DIRECT_BLOB_BIND_UTF8 (ID, DATA_VALUE) VALUES (?, CAST(? AS VARCHAR(10)))",
		int64(3), utf8Ref)
	if nestedCastErr == nil || !strings.Contains(strings.ToLower(nestedCastErr.Error()), "metadata") ||
		!strings.Contains(strings.ToLower(nestedCastErr.Error()), "unavailable") {
		t.Fatalf("incompatible nested BLOB cast error = %v, want explicit metadata-unavailable error", nestedCastErr)
	}

	readReference := func(query string, id int64, want []byte) {
		t.Helper()
		cursor, queryErr := tx.Query(ctx, query, id)
		if queryErr != nil {
			t.Fatalf("query bound BLOB %d: %v", id, queryErr)
		}
		if hasRow, nextErr := cursor.Next(ctx); nextErr != nil || !hasRow {
			t.Fatalf("bound BLOB %d Next() = (%v, %v), want (true, nil)", id, hasRow, nextErr)
		}
		row, rowErr := cursor.Row()
		if rowErr != nil {
			t.Fatalf("read bound BLOB %d: %v", id, rowErr)
		}
		if closeErr := cursor.Close(); closeErr != nil {
			t.Fatalf("close bound BLOB %d: %v", id, closeErr)
		}
		reference, ok := row[0].Value.(interbase.BlobRef)
		if !ok {
			t.Fatalf("bound BLOB %d value type = %T, want interbase.BlobRef", id, row[0].Value)
		}
		reader, openErr := tx.OpenBlob(ctx, reference)
		if openErr != nil {
			t.Fatalf("open bound BLOB %d: %v", id, openErr)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil || closeErr != nil {
			t.Fatalf("read/close bound BLOB %d: read=%v close=%v", id, readErr, closeErr)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("bound BLOB %d = %x, want %x", id, got, want)
		}
	}
	readReference("SELECT DATA_VALUE FROM GO_DIRECT_BLOB_BIND_UTF8 WHERE ID = ?", 1, utf8Payload)
	readReference("SELECT DATA_VALUE FROM GO_DIRECT_BLOB_BIND_BINARY WHERE ID = ?", 2, []byte("binary source"))
	readReference("SELECT DATA_VALUE FROM GO_DIRECT_BLOB_BIND_BINARY WHERE ID = ?", 3, binaryPayload)
}

func TestDirectTextBlobStreamUsesReferenceCharset(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct text BLOB transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	// CreateBlob receives UTF-8 at the public boundary. The reader deliberately
	// splits both multibyte codepoints across calls; the stored target is WIN1250.
	created, err := tx.CreateBlob(ctx,
		interbase.BlobOptions{Subtype: 1, Charset: "WIN1250"},
		&directChunkReader{data: []byte("ě Ž"), chunkSize: 1})
	if err != nil {
		t.Fatalf("create WIN1250 direct BLOB: %v", err)
	}
	reader, err := tx.OpenBlob(ctx, created)
	if err != nil {
		t.Fatalf("open WIN1250 direct BLOB: %v", err)
	}
	got, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("read WIN1250 direct BLOB: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close WIN1250 direct BLOB: %v", closeErr)
	}
	want := []byte("ě Ž")
	if !bytes.Equal(got, want) {
		t.Fatalf("WIN1250 direct BLOB = %x, want UTF-8 %x", got, want)
	}
}

func TestDirectTextBlobStreamUsesAdditionalCharsets(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct text BLOB transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	for _, test := range []struct {
		charset string
		value   string
	}{
		{charset: "WIN1252", value: "caf\u00e9 \u20ac"},
		{charset: "ISO8859_1", value: "caf\u00e9"},
		{charset: "ASCII", value: "plain ASCII  "},
	} {
		t.Run(test.charset, func(t *testing.T) {
			created, createErr := tx.CreateBlob(ctx,
				interbase.BlobOptions{Subtype: 1, Charset: test.charset},
				&directChunkReader{data: []byte(test.value), chunkSize: 1})
			if createErr != nil {
				t.Fatalf("create %s direct BLOB: %v", test.charset, createErr)
			}
			reader, openErr := tx.OpenBlob(ctx, created)
			if openErr != nil {
				t.Fatalf("open %s direct BLOB: %v", test.charset, openErr)
			}
			got, readErr := io.ReadAll(reader)
			closeErr := reader.Close()
			if readErr != nil || closeErr != nil {
				t.Fatalf("read/close %s direct BLOB: read=%v close=%v", test.charset, readErr, closeErr)
			}
			if string(got) != test.value {
				t.Fatalf("%s direct BLOB = %q, want %q", test.charset, got, test.value)
			}
		})
	}
}

func TestDirectBlobCharsetMetadataForExpressionsAndProcedures(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	// InterBase stores procedure BLOB assignment bytes as supplied by the
	// connection, even when the declared output BLOB has another charset. Use a
	// WIN1250 attachment to define the WIN1250 procedure so its returned bytes
	// match the catalog descriptor that the direct API uses for streaming.
	procedureDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"WIN1250", 3, interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	procedures := []struct {
		db         *sql.DB
		definition string
	}{
		{db: db, definition: `CREATE PROCEDURE GO_DIRECT_BLOB_PROC_UTF8
RETURNS (OUT_VALUE BLOB SUB_TYPE 1 CHARACTER SET UTF8)
AS
BEGIN
  OUT_VALUE = 'ě Ž';
  SUSPEND;
END`},
		{db: procedureDB, definition: `CREATE PROCEDURE GO_DIRECT_BLOB_PROC_WIN1250
RETURNS (OUT_VALUE BLOB SUB_TYPE 1 CHARACTER SET WIN1250)
AS
BEGIN
  OUT_VALUE = 'ě Ž';
  SUSPEND;
END`},
	}
	for _, procedure := range procedures {
		if _, err := procedure.db.ExecContext(ctx, procedure.definition); err != nil {
			t.Fatalf("create direct BLOB procedure: %v", err)
		}
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_DIRECT_BLOB_EXPRESSION (
			UTF8_VALUE BLOB SUB_TYPE 1 CHARACTER SET UTF8,
			WIN1250_VALUE BLOB SUB_TYPE 1 CHARACTER SET WIN1250,
			BINARY_VALUE BLOB SUB_TYPE 0)`); err != nil {
		t.Fatalf("create direct BLOB expression table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO GO_DIRECT_BLOB_EXPRESSION
			(UTF8_VALUE, WIN1250_VALUE, BINARY_VALUE)
		VALUES (?, ?, ?)`, "ě Ž", "ě Ž", []byte("raw bytes")); err != nil {
		t.Fatalf("insert direct BLOB expression values: %v", err)
	}

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct BLOB metadata transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	readReference := func(name string, ref interbase.BlobRef) {
		t.Helper()
		reader, openErr := tx.OpenBlob(ctx, ref)
		if openErr != nil {
			t.Fatalf("open %s BLOB: %v", name, openErr)
		}
		got, readErr := io.ReadAll(reader)
		closeErr := reader.Close()
		if readErr != nil {
			t.Fatalf("read %s BLOB: %v", name, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close %s BLOB: %v", name, closeErr)
		}
		if !bytes.Equal(got, []byte("ě Ž")) {
			t.Fatalf("%s BLOB = %x, want UTF-8 %x", name, got, []byte("ě Ž"))
		}
	}

	for _, procedure := range []string{"GO_DIRECT_BLOB_PROC_UTF8", "GO_DIRECT_BLOB_PROC_WIN1250"} {
		cursor, queryErr := tx.Query(ctx, "EXECUTE PROCEDURE "+procedure)
		if queryErr != nil {
			t.Fatalf("query %s: %v", procedure, queryErr)
		}
		if hasRow, nextErr := cursor.Next(ctx); nextErr != nil || !hasRow {
			t.Fatalf("%s Next() = (%v, %v), want (true, nil)", procedure, hasRow, nextErr)
		}
		row, rowErr := cursor.Row()
		if rowErr != nil {
			t.Fatalf("%s Row(): %v", procedure, rowErr)
		}
		if closeErr := cursor.Close(); closeErr != nil {
			t.Fatalf("close %s cursor: %v", procedure, closeErr)
		}
		if len(row) != 1 {
			t.Fatalf("%s row length = %d, want 1", procedure, len(row))
		}
		ref, ok := row[0].Value.(interbase.BlobRef)
		if !ok {
			t.Fatalf("%s value type = %T, want interbase.BlobRef", procedure, row[0].Value)
		}
		readReference(procedure, ref)
	}

	for _, expression := range []string{
		"SELECT (SELECT UTF8_VALUE FROM GO_DIRECT_BLOB_EXPRESSION) FROM RDB$DATABASE",
		"SELECT (SELECT WIN1250_VALUE FROM GO_DIRECT_BLOB_EXPRESSION) FROM RDB$DATABASE",
	} {
		cursor, queryErr := tx.Query(ctx, expression)
		if queryErr != nil {
			t.Fatalf("query text BLOB expression %q: %v", expression, queryErr)
		}
		if hasRow, nextErr := cursor.Next(ctx); nextErr != nil || !hasRow {
			t.Fatalf("text BLOB expression Next() = (%v, %v), want (true, nil)", hasRow, nextErr)
		}
		if _, rowErr := cursor.Row(); rowErr == nil || !strings.Contains(strings.ToLower(rowErr.Error()), "character set") {
			t.Fatalf("text BLOB expression Row() = %v, want an explicit character-set error", rowErr)
		}
		if closeErr := cursor.Close(); closeErr != nil {
			t.Fatalf("close text BLOB expression cursor: %v", closeErr)
		}
	}

	binaryCursor, err := tx.Query(ctx,
		"SELECT (SELECT BINARY_VALUE FROM GO_DIRECT_BLOB_EXPRESSION) FROM RDB$DATABASE")
	if err != nil {
		t.Fatalf("query binary BLOB expression: %v", err)
	}
	if hasRow, nextErr := binaryCursor.Next(ctx); nextErr != nil || !hasRow {
		t.Fatalf("binary BLOB expression Next() = (%v, %v), want (true, nil)", hasRow, nextErr)
	}
	binaryRow, err := binaryCursor.Row()
	if err != nil {
		t.Fatalf("binary BLOB expression Row(): %v", err)
	}
	if closeErr := binaryCursor.Close(); closeErr != nil {
		t.Fatalf("close binary BLOB expression cursor: %v", closeErr)
	}
	binaryRef, ok := binaryRow[0].Value.(interbase.BlobRef)
	if !ok {
		t.Fatalf("binary BLOB expression value type = %T, want interbase.BlobRef", binaryRow[0].Value)
	}
	binaryReader, err := tx.OpenBlob(ctx, binaryRef)
	if err != nil {
		t.Fatalf("open binary BLOB expression: %v", err)
	}
	binaryValue, readErr := io.ReadAll(binaryReader)
	closeErr := binaryReader.Close()
	if readErr != nil || closeErr != nil {
		t.Fatalf("read/close binary BLOB expression: read=%v close=%v", readErr, closeErr)
	}
	if string(binaryValue) != "raw bytes" {
		t.Fatalf("binary BLOB expression = %q, want raw bytes", binaryValue)
	}
}

type directChunkReader struct {
	data      []byte
	offset    int
	chunkSize int
}

func (r *directChunkReader) Read(buffer []byte) (int, error) {
	if r.offset == len(r.data) {
		return 0, io.EOF
	}
	count := len(buffer)
	if count > r.chunkSize {
		count = r.chunkSize
	}
	if count > len(r.data)-r.offset {
		count = len(r.data) - r.offset
	}
	copy(buffer, r.data[r.offset:r.offset+count])
	r.offset += count
	return count, nil
}

type directPatternReader struct {
	remaining uint64
	offset    uint64
}

func (r *directPatternReader) Read(buffer []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	length := uint64(len(buffer))
	if length > r.remaining {
		length = r.remaining
	}
	for index := uint64(0); index < length; index++ {
		buffer[index] = byte((r.offset + index) & 0xff)
	}
	r.offset += length
	r.remaining -= length
	return int(length), nil
}

type directPatternVerifier struct {
	offset uint64
	valid  bool
}

func (w *directPatternVerifier) Write(data []byte) (int, error) {
	for index, value := range data {
		if value != byte((w.offset+uint64(index))&0xff) {
			w.valid = false
		}
	}
	w.offset += uint64(len(data))
	return len(data), nil
}

func TestDirectBlobStreamExceedsMaterializationLimit(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContextWithTimeout(t, longReadTestTimeout)
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin large direct BLOB transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	const size = uint64(64*1024*1024 + 12345)
	created, err := tx.CreateBlob(ctx,
		interbase.BlobOptions{Subtype: 0, Charset: "UTF8"},
		&directPatternReader{remaining: size})
	if err != nil {
		t.Fatalf("create large direct BLOB: %v", err)
	}
	reader, err := tx.OpenBlob(ctx, created)
	if err != nil {
		t.Fatalf("open large direct BLOB: %v", err)
	}
	verifier := &directPatternVerifier{valid: true}
	read, readErr := io.Copy(verifier, reader)
	closeErr := reader.Close()
	if readErr != nil {
		t.Fatalf("read large direct BLOB: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close large direct BLOB: %v", closeErr)
	}
	if uint64(read) != size || verifier.offset != size || !verifier.valid {
		t.Fatalf("large direct BLOB stream changed: read %d bytes, verified %d, want %d",
			read, verifier.offset, size)
	}
}

func TestDirectExecPlanInfoAndNormalCompletion(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin direct transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	plan, err := tx.Plan(ctx, "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("direct Plan(): %v", err)
	}
	if plan == "" {
		t.Fatal("direct Plan() returned an empty plan")
	}
	const plannedID int64 = 7103
	_, err = tx.Plan(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (7103, 71, 'planned DML')")
	if err != nil {
		t.Fatalf("direct DML Plan() returned error: %v", err)
	}
	assertDirectNoRows(t, tx, ctx, plannedID)

	databaseVersion, err := attachment.DatabaseInfo(ctx, interbase.InfoDatabaseVersion)
	if err != nil {
		t.Fatalf("direct DatabaseInfo(): %v", err)
	}
	if databaseVersion.Code != interbase.InfoDatabaseVersion || len(databaseVersion.Data) == 0 {
		t.Fatalf("database version info = %#v, want non-empty code %d", databaseVersion, interbase.InfoDatabaseVersion)
	}
	version, err := databaseVersion.Text()
	if err != nil {
		t.Fatalf("direct DatabaseInfo().Text(): %v", err)
	}
	if version == "" || version == string(databaseVersion.Data) || version[0] < 0x20 ||
		strings.ContainsAny(version, "\x00\x01\x02\x03") {
		t.Fatalf("direct database version = %q, retained protocol framing from %#v", version, databaseVersion.Data)
	}
	transactionID, err := tx.Info(ctx, interbase.InfoTransactionID)
	if err != nil {
		t.Fatalf("direct Transaction.Info(): %v", err)
	}
	if transactionID.Code != interbase.InfoTransactionID || len(transactionID.Data) == 0 {
		t.Fatalf("transaction info = %#v, want non-empty code %d", transactionID, interbase.InfoTransactionID)
	}

	affected, err := tx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(701), int64(7), "direct exec")
	if err != nil {
		t.Fatalf("direct Exec(): %v", err)
	}
	if affected != 1 {
		t.Fatalf("direct Exec() affected = %d, want 1", affected)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("direct Commit(): %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(702), int64(7), "must fail"); err == nil {
		t.Fatal("direct Exec() after Commit() returned nil error")
	}

	var label string
	if err := db.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = ?", int64(701)).Scan(&label); err != nil {
		t.Fatalf("verify direct commit: %v", err)
	}
	if label != "direct exec" {
		t.Fatalf("committed label = %q, want direct exec", label)
	}
}

func TestDirectRetainingKeepsMultipleCursorsAndVisibility(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		t.Fatalf("begin direct transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	idCursor, err := tx.Query(ctx, "SELECT ID FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		t.Fatalf("open retaining ID cursor: %v", err)
	}
	t.Cleanup(func() { _ = idCursor.Close() })
	countryCursor, err := tx.Query(ctx, "SELECT COUNTRY FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		t.Fatalf("open retaining country cursor: %v", err)
	}
	t.Cleanup(func() { _ = countryCursor.Close() })
	if hasRow, err := idCursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("first retaining ID Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	if hasRow, err := countryCursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("first retaining country Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	firstID, err := idCursor.Row()
	if err != nil || firstID[0].Value != int64(1) {
		t.Fatalf("first retaining ID row = %#v, err=%v, want id 1", firstID, err)
	}
	if _, err := countryCursor.Row(); err != nil {
		t.Fatalf("first retaining country Row(): %v", err)
	}

	const committedID int64 = 7101
	const rolledBackID int64 = 7102
	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		committedID, int64(71), "retained commit"); err != nil {
		t.Fatalf("insert before CommitRetaining(): %v", err)
	}
	var label string
	err = db.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = ?", committedID).Scan(&label)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("uncommitted row visibility error = %v, want sql.ErrNoRows", err)
	}

	if err := tx.CommitRetaining(); err != nil {
		t.Fatalf("direct CommitRetaining(): %v", err)
	}
	if err := db.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = ?", committedID).Scan(&label); err != nil {
		t.Fatalf("committed retaining row visibility: %v", err)
	}
	if label != "retained commit" {
		t.Fatalf("committed retaining label = %q, want retained commit", label)
	}
	if hasRow, err := idCursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("post-commit retaining ID Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	if hasRow, err := countryCursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("post-commit retaining country Next() = (%v, %v), want (true, nil)", hasRow, err)
	}

	if _, err := tx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		rolledBackID, int64(72), "retained rollback"); err != nil {
		t.Fatalf("insert before RollbackRetaining(): %v", err)
	}
	if err := tx.RollbackRetaining(); err != nil {
		t.Fatalf("direct RollbackRetaining(): %v", err)
	}
	assertDirectNoRows(t, tx, ctx, rolledBackID)
	if err := db.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = ?", committedID).Scan(&label); err != nil {
		t.Fatalf("committed row after RollbackRetaining(): %v", err)
	}
	if hasRow, err := idCursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("post-rollback retaining ID Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	if hasRow, err := countryCursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("post-rollback retaining country Next() = (%v, %v), want (true, nil)", hasRow, err)
	}

	if err := tx.Commit(); err != nil {
		t.Fatalf("normal direct Commit(): %v", err)
	}
	var rolledBackLabel string
	err = db.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = ?", rolledBackID).Scan(&rolledBackLabel)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("rolled-back row survived final commit: query error = %v", err)
	}
	if _, err := idCursor.Next(ctx); err == nil {
		t.Fatal("ID cursor remained usable after normal transaction completion")
	}
	if _, err := countryCursor.Next(ctx); err == nil {
		t.Fatal("country cursor remained usable after normal transaction completion")
	}
}

func TestDirectCursorNameSupportsPositionedUpdate(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if _, err := db.ExecContext(ctx, "UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "USA", int64(1)); err != nil {
			t.Errorf("restore positioned-update fixture row: %v", err)
		}
	})

	ctx := readContext(t)
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin writable direct transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	cursor, err := tx.Query(ctx, "SELECT ID, COUNTRY FROM GO_COUNTRY WHERE ID = ? FOR UPDATE", int64(1))
	if err != nil {
		t.Fatalf("open positioned-update cursor: %v", err)
	}
	if err := cursor.SetName("go_direct_cursor"); err != nil {
		t.Fatalf("set direct cursor name: %v", err)
	}
	if hasRow, err := cursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("positioned cursor Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	if _, err := cursor.Row(); err != nil {
		t.Fatalf("read positioned cursor row: %v", err)
	}
	affected, err := tx.Exec(ctx,
		"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE CURRENT OF go_direct_cursor", "direct")
	if err != nil {
		t.Fatalf("positioned update: %v", err)
	}
	if affected != 1 {
		t.Fatalf("positioned update affected = %d, want 1", affected)
	}
	if err := cursor.Close(); err != nil {
		t.Fatalf("close positioned-update cursor: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit positioned update: %v", err)
	}
}
