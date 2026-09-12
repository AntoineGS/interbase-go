//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"context"
	"database/sql"
	"math"
	"reflect"
	"testing"
)

func requireRowsAffected(t *testing.T, result sql.Result, want int64) {
	t.Helper()
	if result == nil {
		t.Fatal("Exec returned a nil result")
	}
	got, err := result.RowsAffected()
	if err != nil {
		t.Fatalf("RowsAffected: %v", err)
	}
	if got != want {
		t.Fatalf("RowsAffected = %d, want %d", got, want)
	}
}

type writeRow struct {
	id    int64
	value int64
	label string
}

// queryWriteRows reads from a second physical attachment. Holding the only
// MaxOpenConns(1) attachment while raising the limit makes database/sql open a
// distinct reader instead of reusing an implicit writer transaction.
func queryWriteRows(t *testing.T, db *sql.DB, ctx context.Context) []writeRow {
	t.Helper()
	db.SetMaxOpenConns(1)
	writer, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin writer attachment: %v", err)
	}

	db.SetMaxOpenConns(2)
	reader, err := db.Conn(ctx)
	if err != nil {
		if closeErr := writer.Close(); closeErr != nil {
			t.Errorf("close writer attachment after reader failure: %v", closeErr)
		}
		db.SetMaxOpenConns(1)
		t.Fatalf("pin reader attachment: %v", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close reader attachment: %v", err)
		}
		if err := writer.Close(); err != nil {
			t.Errorf("close writer attachment: %v", err)
		}
		db.SetMaxOpenConns(1)
	}()

	stats := db.Stats()
	if stats.OpenConnections != 2 || stats.InUse != 2 || stats.Idle != 0 {
		t.Fatalf("writer and reader did not occupy distinct attachments: %+v", stats)
	}

	rows, err := reader.QueryContext(ctx,
		"SELECT ID, WRITE_VALUE, LABEL FROM GO_WRITE ORDER BY ID")
	if err != nil {
		t.Fatalf("committed GO_WRITE query: %v", err)
	}
	defer rows.Close()
	got := make([]writeRow, 0)
	for rows.Next() {
		var row writeRow
		if err := rows.Scan(&row.id, &row.value, &row.label); err != nil {
			t.Fatalf("committed GO_WRITE scan: %v", err)
		}
		got = append(got, row)
	}
	finishReadRows(t, rows)
	return got
}

// Source mapping: DatabaseAPI20Test.test_rowcount and
// TestInsertData.test_insert_integers.
func TestWriteDMLRowsAffected(t *testing.T) {
	tests := []struct {
		name  string
		setup bool
		query string
		args  []any
		want  int64
		rows  []writeRow
	}{
		{
			name:  "insert one",
			query: "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			args:  []any{int64(10), int64(42), "inserted"},
			want:  1,
			rows:  []writeRow{{id: 10, value: 42, label: "inserted"}},
		},
		{
			name:  "update one",
			setup: true,
			query: "UPDATE GO_WRITE SET WRITE_VALUE = ?, LABEL = ? WHERE ID = ?",
			args:  []any{int64(43), "updated", int64(10)},
			want:  1,
			rows:  []writeRow{{id: 10, value: 43, label: "updated"}},
		},
		{
			name:  "update zero",
			setup: true,
			query: "UPDATE GO_WRITE SET WRITE_VALUE = ?, LABEL = ? WHERE ID = ?",
			args:  []any{int64(43), "updated", int64(99)},
			want:  0,
			rows:  []writeRow{{id: 10, value: 42, label: "initial"}},
		},
		{
			name:  "delete one",
			setup: true,
			query: "DELETE FROM GO_WRITE WHERE ID = ?",
			args:  []any{int64(10)},
			want:  1,
			rows:  []writeRow{},
		},
		{
			name:  "delete zero",
			setup: true,
			query: "DELETE FROM GO_WRITE WHERE ID = ?",
			args:  []any{int64(99)},
			want:  0,
			rows:  []writeRow{{id: 10, value: 42, label: "initial"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newDatabase(t)
			ctx := readContext(t)
			if test.setup {
				result, err := db.ExecContext(ctx,
					"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
					int64(10), int64(42), "initial")
				if err != nil {
					t.Fatalf("setup insert: %v", err)
				}
				requireRowsAffected(t, result, 1)
				seed := []writeRow{{id: 10, value: 42, label: "initial"}}
				if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, seed) {
					t.Fatalf("seed rows before %s = %#v, want %#v", test.name, got, seed)
				}
			}

			result, err := db.ExecContext(ctx, test.query, test.args...)
			if err != nil {
				t.Fatalf("DML: %v", err)
			}
			requireRowsAffected(t, result, test.want)
			if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, test.rows) {
				t.Fatalf("rows after %s = %#v, want %#v", test.name, got, test.rows)
			}
		})
	}
}

// Source mapping: DatabaseAPI20Test.test_execute and
// TestInsertData.test_insert_integers.
func TestWriteDMLRoundTrip(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	actions := []struct {
		query string
		args  []any
		want  int64
		rows  []writeRow
	}{
		{
			query: "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			args:  []any{int64(10), int64(42), "first"},
			want:  1,
			rows:  []writeRow{{id: 10, value: 42, label: "first"}},
		},
		{
			query: "INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			args:  []any{int64(11), int64(7), "second"},
			want:  1,
			rows: []writeRow{
				{id: 10, value: 42, label: "first"},
				{id: 11, value: 7, label: "second"},
			},
		},
		{
			query: "UPDATE GO_WRITE SET WRITE_VALUE = ?, LABEL = ? WHERE ID = ?",
			args:  []any{int64(43), "changed", int64(10)},
			want:  1,
			rows: []writeRow{
				{id: 10, value: 43, label: "changed"},
				{id: 11, value: 7, label: "second"},
			},
		},
		{
			query: "DELETE FROM GO_WRITE WHERE ID = ?",
			args:  []any{int64(11)},
			want:  1,
			rows:  []writeRow{{id: 10, value: 43, label: "changed"}},
		},
	}
	for _, action := range actions {
		result, err := db.ExecContext(ctx, action.query, action.args...)
		if err != nil {
			t.Fatalf("DML %q: %v", action.query, err)
		}
		requireRowsAffected(t, result, action.want)
		if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, action.rows) {
			t.Fatalf("round-trip rows after %q = %#v, want %#v", action.query, got, action.rows)
		}
	}
}

// Source mapping: DatabaseAPI20Test.test_execute and
// TestInsertData.test_insert_integers.
func TestWriteDuplicateKeyRecovery(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	result, err := db.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(42), "original")
	if err != nil {
		t.Fatalf("initial insert: %v", err)
	}
	requireRowsAffected(t, result, 1)
	initial := []writeRow{{id: 10, value: 42, label: "original"}}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, initial) {
		t.Fatalf("rows after initial insert = %#v, want %#v", got, initial)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(99), "duplicate"); err == nil {
		t.Fatal("duplicate primary key insert succeeded")
	}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, initial) {
		t.Fatalf("rows after duplicate insert = %#v, want %#v", got, initial)
	}

	result, err = db.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(11), int64(7), "recovered")
	if err != nil {
		t.Fatalf("insert after duplicate error: %v", err)
	}
	requireRowsAffected(t, result, 1)

	want := []writeRow{
		{id: 10, value: 42, label: "original"},
		{id: 11, value: 7, label: "recovered"},
	}
	if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, want) {
		t.Fatalf("rows after duplicate recovery = %#v, want %#v", got, want)
	}
}

// Source mapping: TestPreparedStatement.test_execution and
// DatabaseAPI20Test.test_execute.
func TestWritePreparedSelectReuseAndArgumentCounts(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	stmt, err := db.PrepareContext(ctx,
		"SELECT COUNTRY, CURRENCY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("prepare SELECT: %v", err)
	}
	t.Cleanup(func() {
		if err := stmt.Close(); err != nil {
			t.Errorf("close prepared SELECT: %v", err)
		}
	})

	for _, want := range []struct {
		id       int64
		country  string
		currency string
	}{
		{id: 1, country: "USA", currency: "Dollar"},
		{id: 2, country: "England", currency: "Pound"},
	} {
		rows, err := stmt.QueryContext(ctx, want.id)
		if err != nil {
			t.Fatalf("prepared SELECT for ID %d: %v", want.id, err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("prepared SELECT for ID %d returned no row: %v", want.id, rows.Err())
		}
		var country, currency string
		if err := rows.Scan(&country, &currency); err != nil {
			t.Fatalf("prepared SELECT scan for ID %d: %v", want.id, err)
		}
		if country != want.country || currency != want.currency {
			t.Fatalf("prepared SELECT row for ID %d = (%q, %q), want (%q, %q)",
				want.id, country, currency, want.country, want.currency)
		}
		finishReadRows(t, rows)
	}

	for _, args := range [][]any{nil, {int64(1), int64(2)}} {
		rows, err := stmt.QueryContext(ctx, args...)
		if rows != nil {
			_ = rows.Close()
		}
		if err == nil {
			t.Fatalf("prepared SELECT with %d arguments succeeded", len(args))
		}
	}

	rows, err := stmt.QueryContext(ctx, int64(3))
	if err != nil {
		t.Fatalf("prepared SELECT reuse after argument errors: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("prepared SELECT reuse returned no row: %v", rows.Err())
	}
	var country, currency string
	if err := rows.Scan(&country, &currency); err != nil {
		t.Fatalf("prepared SELECT reuse scan: %v", err)
	}
	if country != "Japan" || currency != "Yen" {
		t.Fatalf("prepared SELECT reuse row = (%q, %q), want (Japan, Yen)", country, currency)
	}
	finishReadRows(t, rows)
}

// Source mapping: TestPreparedStatement.test_execution and
// DatabaseAPI20Test.test_close.
func TestWritePreparedCloseRejectsUse(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	stmt, err := db.PrepareContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?")
	if err != nil {
		t.Fatalf("prepare SELECT: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })
	rows, err := stmt.QueryContext(ctx, int64(1))
	if err != nil {
		t.Fatalf("execute prepared SELECT before close: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("prepared SELECT before close returned no row: %v", rows.Err())
	}
	var country string
	if err := rows.Scan(&country); err != nil {
		t.Fatalf("scan prepared SELECT before close: %v", err)
	}
	if country != "USA" {
		t.Fatalf("prepared SELECT before close = %q, want USA", country)
	}
	finishReadRows(t, rows)
	if err := stmt.Close(); err != nil {
		t.Fatalf("close prepared SELECT: %v", err)
	}
	if rows, err := stmt.QueryContext(ctx, int64(1)); err == nil {
		if rows != nil {
			_ = rows.Close()
		}
		t.Fatal("query through a closed prepared statement succeeded")
	}
}

// Source mapping: TestPreparedStatement.test_execution and
// DatabaseAPI20Test.test_rowcount.
func TestWritePreparedExecRepeated(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	stmt, err := db.PrepareContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)")
	if err != nil {
		t.Fatalf("prepare INSERT: %v", err)
	}
	t.Cleanup(func() {
		if err := stmt.Close(); err != nil {
			t.Errorf("close prepared INSERT: %v", err)
		}
	})

	for _, test := range []struct {
		args [3]any
		want []writeRow
	}{
		{
			args: [3]any{int64(10), int64(42), "first"},
			want: []writeRow{{id: 10, value: 42, label: "first"}},
		},
		{
			args: [3]any{int64(11), int64(7), "second"},
			want: []writeRow{
				{id: 10, value: 42, label: "first"},
				{id: 11, value: 7, label: "second"},
			},
		},
	} {
		result, err := stmt.ExecContext(ctx, test.args[0], test.args[1], test.args[2])
		if err != nil {
			t.Fatalf("prepared INSERT: %v", err)
		}
		requireRowsAffected(t, result, 1)
		if got := queryWriteRows(t, db, ctx); !reflect.DeepEqual(got, test.want) {
			t.Fatalf("prepared INSERT rows = %#v, want %#v", got, test.want)
		}
	}
}

// Source mapping: TestInsertData.test_insert_integers and
// TestInsertData.test_insert_float_double.
func TestWriteNumericParameterConversions(t *testing.T) {
	tests := []struct {
		name string
		args []any
		want int64
	}{
		{name: "int", args: []any{int(10), int(42)}, want: 42},
		{name: "int32", args: []any{int32(10), int32(-42)}, want: -42},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newDatabase(t)
			ctx := readContext(t)
			result, err := db.ExecContext(ctx,
				"INSERT INTO GO_WRITE (ID, WRITE_VALUE) VALUES (?, ?)", test.args...)
			if err != nil {
				t.Fatalf("%s parameter insert: %v", test.name, err)
			}
			requireRowsAffected(t, result, 1)

			var value int64
			if err := db.QueryRowContext(ctx,
				"SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = ?", int64(10)).Scan(&value); err != nil {
				t.Fatalf("%s parameter query: %v", test.name, err)
			}
			if value != test.want {
				t.Fatalf("%s parameter value = %d, want %d", test.name, value, test.want)
			}
		})
	}

	t.Run("float32", func(t *testing.T) {
		db := newDatabase(t)
		ctx := readContext(t)
		result, err := db.ExecContext(ctx, `
			INSERT INTO GO_DATA (ID, FLOAT_VALUE, DOUBLE_VALUE)
			VALUES (?, ?, ?)`, int64(10), float32(1.25), float32(-2.5))
		if err != nil {
			t.Fatalf("float32 parameter insert: %v", err)
		}
		requireRowsAffected(t, result, 1)

		var floatValue, doubleValue float64
		if err := db.QueryRowContext(ctx,
			"SELECT FLOAT_VALUE, DOUBLE_VALUE FROM GO_DATA WHERE ID = ?", int64(10)).
			Scan(&floatValue, &doubleValue); err != nil {
			t.Fatalf("float32 parameter query: %v", err)
		}
		if math.IsNaN(floatValue) || math.IsNaN(doubleValue) ||
			math.Abs(floatValue-1.25) > 1e-5 || math.Abs(doubleValue+2.5) > 1e-5 {
			t.Fatalf("float32 parameter values = (%g, %g), want approximately (1.25, -2.5)",
				floatValue, doubleValue)
		}
	})
}
