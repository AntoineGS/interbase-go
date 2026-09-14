//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"bytes"
	"strings"
	"testing"
)

// Source mapping: InterBasePython test_bugs.py::test_pyib_17.
func TestParityTriggerDefault(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_PARITY_TRIGGER (
			ID INTEGER NOT NULL PRIMARY KEY,
			C1 INTEGER NOT NULL
		)`); err != nil {
		t.Fatalf("create trigger table: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		CREATE TRIGGER GO_PARITY_TRIGGER_BI FOR GO_PARITY_TRIGGER
		ACTIVE BEFORE INSERT POSITION 0
		AS
		BEGIN
			IF (NEW.C1 IS NULL) THEN
				NEW.C1 = 1;
		END`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin trigger transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO GO_PARITY_TRIGGER (ID, C1) VALUES (?, ?)", int64(1), nil); err != nil {
		t.Fatalf("insert NULL through trigger: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit trigger insert: %v", err)
	}

	var got int64
	if err := db.QueryRowContext(ctx,
		"SELECT C1 FROM GO_PARITY_TRIGGER WHERE ID = ?", int64(1)).Scan(&got); err != nil {
		t.Fatalf("read trigger default: %v", err)
	}
	if got != 1 {
		t.Fatalf("trigger default = %d, want 1", got)
	}
}

// Source mapping: InterBasePython test_bugs.py::test_pyib_22.
func TestParityVarcharInsertLengths(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_PARITY_VARCHAR (
			ID INTEGER NOT NULL PRIMARY KEY,
			VALUE_TEXT VARCHAR(255)
		)`); err != nil {
		t.Fatalf("create VARCHAR(255) table: %v", err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin VARCHAR transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO GO_PARITY_VARCHAR (ID, VALUE_TEXT) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("prepare VARCHAR insert: %v", err)
	}
	for length := 0; length < 255; length++ {
		value := strings.Repeat("x", length)
		if _, err := stmt.ExecContext(ctx, int64(length), value); err != nil {
			_ = stmt.Close()
			t.Fatalf("insert VARCHAR length %d: %v", length, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close VARCHAR insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit VARCHAR inserts: %v", err)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ID, VALUE_TEXT FROM GO_PARITY_VARCHAR ORDER BY ID")
	if err != nil {
		t.Fatalf("query VARCHAR values: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int64
		var value string
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan VARCHAR length %d: %v", count, err)
		}
		if id != int64(count) {
			t.Fatalf("VARCHAR row ID = %d, want %d", id, count)
		}
		if len(value) != count || value != strings.Repeat("x", count) {
			t.Fatalf("VARCHAR row %d = %q (length %d), want %q (length %d)",
				count, value, len(value), strings.Repeat("x", count), count)
		}
		count++
	}
	if count != 255 {
		t.Fatalf("VARCHAR row count = %d, want 255", count)
	}
	finishReadRows(t, rows)
}

type parityScanner interface {
	Scan(dest ...any) error
}

type parityOctetsCase struct {
	id          int64
	input       any
	wantFixed   []byte
	wantVarying []byte
	wantNull    bool
}

func requireParityOctets(t *testing.T, scanner parityScanner, want parityOctetsCase) {
	t.Helper()
	var fixed, varying any
	if err := scanner.Scan(&fixed, &varying); err != nil {
		t.Fatalf("scan OCTETS row %d: %v", want.id, err)
	}
	requireParityOctetsValues(t, want, fixed, varying)
}

func requireParityOctetsValues(t *testing.T, want parityOctetsCase, fixed, varying any) {
	t.Helper()
	if want.wantNull {
		if fixed != nil || varying != nil {
			t.Fatalf("OCTETS row %d = (%#v, %#v), want (NULL, NULL)",
				want.id, fixed, varying)
		}
		return
	}
	requireParityBytes(t, want.id, "CHAR(5)", fixed, want.wantFixed)
	requireParityBytes(t, want.id, "VARCHAR(5)", varying, want.wantVarying)
}

func requireParityBytes(t *testing.T, id int64, column string, value any, want []byte) {
	t.Helper()
	got, ok := value.([]byte)
	if !ok {
		t.Fatalf("OCTETS row %d %s type = %T, want []byte", id, column, value)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("OCTETS row %d %s = %v, want %v", id, column, got, want)
	}
	if want != nil && got == nil {
		t.Fatalf("OCTETS row %d %s is nil, want an empty/non-nil byte slice", id, column)
	}
}

// Source mapping: InterBasePython test_charset_conversion.py::test_octets.
func TestParityOctets(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, `
		CREATE TABLE GO_PARITY_OCTETS (
			ID INTEGER NOT NULL PRIMARY KEY,
			FIXED_VALUE CHAR(5) CHARACTER SET OCTETS,
			VARYING_VALUE VARCHAR(5) CHARACTER SET OCTETS
		)`); err != nil {
		t.Fatalf("create OCTETS table: %v", err)
	}

	full := []byte{0, 1, 127, 128, 255}
	short := []byte{0, 128}
	upstream := []byte{1, 2, 3, 4, 5}
	fixedShort := []byte{0, 128, ' ', ' ', ' '}
	fixedEmpty := []byte{' ', ' ', ' ', ' ', ' '}
	cases := []parityOctetsCase{
		{id: 1, input: full, wantFixed: full, wantVarying: full},
		{id: 2, input: short, wantFixed: fixedShort, wantVarying: short},
		{id: 3, input: []byte{}, wantFixed: fixedEmpty, wantVarying: []byte{}},
		{id: 4, input: nil, wantNull: true},
		{id: 5, input: upstream, wantFixed: upstream, wantVarying: upstream},
	}

	for _, want := range cases {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO GO_PARITY_OCTETS (ID, FIXED_VALUE, VARYING_VALUE) VALUES (?, ?, ?)",
			want.id, want.input, want.input); err != nil {
			t.Fatalf("direct OCTETS insert %d: %v", want.id, err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin prepared OCTETS transaction: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx,
		"INSERT INTO GO_PARITY_OCTETS (ID, FIXED_VALUE, VARYING_VALUE) VALUES (?, ?, ?)")
	if err != nil {
		t.Fatalf("prepare OCTETS insert: %v", err)
	}
	for _, want := range cases {
		if _, err := stmt.ExecContext(ctx, want.id+10, want.input, want.input); err != nil {
			_ = stmt.Close()
			t.Fatalf("prepared OCTETS insert %d: %v", want.id, err)
		}
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close prepared OCTETS insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit prepared OCTETS inserts: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT ID, FIXED_VALUE, VARYING_VALUE
		FROM GO_PARITY_OCTETS ORDER BY ID`)
	if err != nil {
		t.Fatalf("direct OCTETS query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		if count >= len(cases)*2 {
			t.Fatal("direct OCTETS query returned an unexpected extra row")
		}
		var id int64
		var fixed, varying any
		if err := rows.Scan(&id, &fixed, &varying); err != nil {
			t.Fatalf("scan direct OCTETS row %d: %v", count, err)
		}
		want := cases[count%len(cases)]
		want.id += int64(count / len(cases) * 10)
		if id != want.id {
			t.Fatalf("direct OCTETS row %d ID = %d, want %d", count, id, want.id)
		}
		requireParityOctetsValues(t, want, fixed, varying)
		count++
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("direct OCTETS rows.Err() = %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close direct OCTETS rows: %v", err)
	}
	if count != len(cases)*2 {
		t.Fatalf("direct OCTETS row count = %d, want %d", count, len(cases)*2)
	}

	query, err := db.PrepareContext(ctx, `
		SELECT FIXED_VALUE, VARYING_VALUE
		FROM GO_PARITY_OCTETS WHERE ID = ?`)
	if err != nil {
		t.Fatalf("prepare OCTETS query: %v", err)
	}
	defer query.Close()
	for _, want := range cases {
		for _, id := range []int64{want.id, want.id + 10} {
			row := query.QueryRowContext(ctx, id)
			preparedWant := want
			preparedWant.id = id
			requireParityOctets(t, row, preparedWant)
		}
	}
}
