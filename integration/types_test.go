//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"bytes"
	"database/sql"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Source mapping: TestInsertData.test_insert_integers.
func TestTypesIntegerBounds(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	values := [][3]int64{
		{10, -32768, -2147483648},
		{11, 32767, 2147483647},
	}
	for _, value := range values {
		result, err := db.ExecContext(ctx, `
			INSERT INTO GO_DATA (ID, SMALL_VALUE, INT_VALUE)
			VALUES (?, ?, ?)`, value[0], value[1], value[2])
		if err != nil {
			t.Fatalf("insert integer boundary %d: %v", value[0], err)
		}
		requireRowsAffected(t, result, 1)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ID, SMALL_VALUE, INT_VALUE FROM GO_DATA WHERE ID >= 10 ORDER BY ID")
	if err != nil {
		t.Fatalf("integer boundary query: %v", err)
	}
	defer rows.Close()
	got := make([][3]int64, 0, len(values))
	for rows.Next() {
		var row [3]int64
		if err := rows.Scan(&row[0], &row[1], &row[2]); err != nil {
			t.Fatalf("integer boundary scan: %v", err)
		}
		got = append(got, row)
	}
	if !reflect.DeepEqual(got, values) {
		t.Fatalf("integer boundary rows = %#v, want %#v", got, values)
	}
	if len(got) != len(values) {
		t.Fatalf("integer boundary row count = %d, want %d", len(got), len(values))
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_char_varchar and
// TestCharsetConversion.testCharVarchar.
func TestTypesCharVarcharUTF8RoundTrip(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	result, err := db.ExecContext(ctx, `
		INSERT INTO GO_DATA (ID, FIXED_VALUE, TEXT_VALUE)
		VALUES (?, ?, ?)`, int64(10), "AA", "Introdu\u00e7\u00e3o")
	if err != nil {
		t.Fatalf("insert CHAR/VARCHAR UTF8 value: %v", err)
	}
	requireRowsAffected(t, result, 1)

	var fixed, text string
	if err := db.QueryRowContext(ctx,
		"SELECT FIXED_VALUE, TEXT_VALUE FROM GO_DATA WHERE ID = ?", int64(10)).
		Scan(&fixed, &text); err != nil {
		t.Fatalf("CHAR/VARCHAR UTF8 query: %v", err)
	}
	if fixed != "AA   " {
		t.Fatalf("CHAR value = %q, want %q", fixed, "AA   ")
	}
	if text != "Introdu\u00e7\u00e3o" {
		t.Fatalf("VARCHAR UTF8 value = %q, want %q", text, "Introdu\u00e7\u00e3o")
	}
}

// Source mapping: TestInsertData.test_insert_char_varchar.
func TestTypesUTF8OverlengthRecovery(t *testing.T) {
	snake := "\U0001f40d"
	tests := []struct {
		name         string
		insert       string
		selectStmt   string
		success      string
		tooLong      string
		recovery     string
		wantSuccess  string
		wantRecovery string
	}{
		{
			name:         "CHAR5",
			insert:       "INSERT INTO GO_DATA (ID, FIXED_VALUE) VALUES (?, ?)",
			selectStmt:   "SELECT ID, FIXED_VALUE FROM GO_DATA WHERE ID >= 10 ORDER BY ID",
			success:      strings.Repeat(snake, 5),
			tooLong:      strings.Repeat(snake, 6),
			recovery:     "ok",
			wantSuccess:  strings.Repeat(snake, 5),
			wantRecovery: "ok   ",
		},
		{
			name:         "VARCHAR100",
			insert:       "INSERT INTO GO_DATA (ID, TEXT_VALUE) VALUES (?, ?)",
			selectStmt:   "SELECT ID, TEXT_VALUE FROM GO_DATA WHERE ID >= 10 ORDER BY ID",
			success:      strings.Repeat(snake, 100),
			tooLong:      strings.Repeat(snake, 101),
			recovery:     "recovered",
			wantSuccess:  strings.Repeat(snake, 100),
			wantRecovery: "recovered",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newDatabase(t)
			ctx := readContext(t)
			result, err := db.ExecContext(ctx, test.insert, int64(10), test.success)
			if err != nil {
				t.Fatalf("valid %s boundary value: %v", test.name, err)
			}
			requireRowsAffected(t, result, 1)
			if _, err := db.ExecContext(ctx, test.insert, int64(11), test.tooLong); err == nil {
				t.Fatalf("overlength %s value succeeded", test.name)
			}

			result, err = db.ExecContext(ctx, test.insert, int64(11), test.recovery)
			if err != nil {
				t.Fatalf("valid value after overlength error: %v", err)
			}
			requireRowsAffected(t, result, 1)

			rows, err := db.QueryContext(ctx, test.selectStmt)
			if err != nil {
				t.Fatalf("%s recovery query: %v", test.name, err)
			}
			defer rows.Close()
			want := []struct {
				id    int64
				value string
			}{
				{id: 10, value: test.wantSuccess},
				{id: 11, value: test.wantRecovery},
			}
			count := 0
			for rows.Next() {
				var id int64
				var value string
				if err := rows.Scan(&id, &value); err != nil {
					t.Fatalf("%s recovery scan: %v", test.name, err)
				}
				if count >= len(want) {
					t.Fatalf("%s returned an unexpected extra row (%d, %q)", test.name, id, value)
				}
				if id != want[count].id || value != want[count].value {
					t.Fatalf("%s row %d = (%d, %q), want (%d, %q)",
						test.name, count, id, value, want[count].id, want[count].value)
				}
				count++
			}
			if count != len(want) {
				t.Fatalf("%s row count = %d, want %d", test.name, count, len(want))
			}
			finishReadRows(t, rows)
		})
	}
}

// Source mapping: TestInsertData.test_insert_float_double.
func TestTypesFloatTolerance(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	result, err := db.ExecContext(ctx, `
		INSERT INTO GO_DATA (ID, FLOAT_VALUE, DOUBLE_VALUE)
		VALUES (?, ?, ?)`, int64(10), float64(1.25), float64(-2.5))
	if err != nil {
		t.Fatalf("insert floating-point values: %v", err)
	}
	requireRowsAffected(t, result, 1)

	var floatValue, doubleValue float64
	if err := db.QueryRowContext(ctx,
		"SELECT FLOAT_VALUE, DOUBLE_VALUE FROM GO_DATA WHERE ID = ?", int64(10)).
		Scan(&floatValue, &doubleValue); err != nil {
		t.Fatalf("floating-point query: %v", err)
	}
	if math.IsNaN(floatValue) || math.IsNaN(doubleValue) ||
		math.Abs(floatValue-1.25) > 1e-6 || math.Abs(doubleValue+2.5) > 1e-9 {
		t.Fatalf("floating-point values = (%g, %g), want approximately (1.25, -2.5)",
			floatValue, doubleValue)
	}
}

// Source mapping: TestInsertData.test_insert_numeric_decimal.
func TestTypesNumericDecimalBindings(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	values := []struct {
		id        int64
		fixed     any
		wide      any
		wantFixed string
		wantWide  float64
	}{
		{id: 10, fixed: "100.11", wide: "100.11", wantFixed: "100.11", wantWide: 100.11},
		{id: 11, fixed: float64(-1.10), wide: float64(-1.10), wantFixed: "-1.10", wantWide: -1.10},
	}
	for _, value := range values {
		result, err := db.ExecContext(ctx, `
			INSERT INTO GO_DATA (ID, FIXED_DECIMAL, WIDE_DECIMAL)
			VALUES (?, ?, ?)`, value.id, value.fixed, value.wide)
		if err != nil {
			t.Fatalf("insert decimal row %d: %v", value.id, err)
		}
		requireRowsAffected(t, result, 1)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT ID, FIXED_DECIMAL, WIDE_DECIMAL
		FROM GO_DATA WHERE ID >= 10 ORDER BY ID`)
	if err != nil {
		t.Fatalf("decimal query: %v", err)
	}
	defer rows.Close()
	got := make([]struct {
		id    int64
		fixed string
		wide  float64
	}, 0, len(values))
	for rows.Next() {
		var row struct {
			id    int64
			fixed string
			wide  float64
		}
		if err := rows.Scan(&row.id, &row.fixed, &row.wide); err != nil {
			t.Fatalf("decimal scan: %v", err)
		}
		got = append(got, row)
	}
	if len(got) != len(values) {
		t.Fatalf("decimal row count = %d, want %d", len(got), len(values))
	}
	for i, row := range got {
		if row.id != values[i].id || row.fixed != values[i].wantFixed ||
			math.IsNaN(row.wide) || math.Abs(row.wide-values[i].wantWide) > 1e-9 {
			t.Fatalf("decimal row %d = (%d, %q, %.17g), want (%d, %q, approximately %.17g)",
				i, row.id, row.fixed, row.wide, values[i].id, values[i].wantFixed, values[i].wantWide)
		}
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_datetime and TestBugs.test_pyib_44.
func TestTypesTimestampPrecisionAndMidnight(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	want := []time.Time{
		time.Date(2011, time.November, 13, 15, 0, 1, 200000, time.UTC),
		time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
	}
	for i, moment := range want {
		result, err := db.ExecContext(ctx,
			"INSERT INTO GO_DATA (ID, MOMENT) VALUES (?, ?)", int64(10+i), moment)
		if err != nil {
			t.Fatalf("insert timestamp %d: %v", i, err)
		}
		requireRowsAffected(t, result, 1)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ID, MOMENT FROM GO_DATA WHERE ID >= 10 ORDER BY ID")
	if err != nil {
		t.Fatalf("timestamp query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int64
		var got time.Time
		if err := rows.Scan(&id, &got); err != nil {
			t.Fatalf("timestamp scan: %v", err)
		}
		if count >= len(want) {
			t.Fatalf("unexpected timestamp row ID %d", id)
		}
		if id != int64(10+count) || !got.Equal(want[count]) ||
			got.Nanosecond() != want[count].Nanosecond() {
			t.Fatalf("timestamp row %d = (%d, %v), want (%d, %v)",
				count, id, got, 10+count, want[count])
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("timestamp row count = %d, want %d", count, len(want))
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_boolean.
func TestTypesBooleanRoundTrip(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	for i, active := range []bool{false, true} {
		result, err := db.ExecContext(ctx,
			"INSERT INTO GO_DATA (ID, ACTIVE_VALUE) VALUES (?, ?)", int64(10+i), active)
		if err != nil {
			t.Fatalf("insert boolean %t: %v", active, err)
		}
		requireRowsAffected(t, result, 1)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ID, ACTIVE_VALUE FROM GO_DATA WHERE ID >= 10 ORDER BY ID")
	if err != nil {
		t.Fatalf("boolean query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int64
		var active bool
		if err := rows.Scan(&id, &active); err != nil {
			t.Fatalf("boolean scan: %v", err)
		}
		if count >= 2 || id != int64(10+count) || active != (count == 1) {
			t.Fatalf("boolean row %d = (%d, %t), want (%d, %t)",
				count, id, active, 10+count, count == 1)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("boolean row count = %d, want 2", count)
	}
	finishReadRows(t, rows)
}

// Source mapping: DatabaseAPI20Test.test_None and TestCharsetConversion.testCharVarchar.
func TestTypesNullAndEmptyText(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	for i, value := range []any{"", nil} {
		result, err := db.ExecContext(ctx,
			"INSERT INTO GO_DATA (ID, TEXT_VALUE) VALUES (?, ?)", int64(10+i), value)
		if err != nil {
			t.Fatalf("insert text value %d: %v", i, err)
		}
		requireRowsAffected(t, result, 1)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ID, TEXT_VALUE FROM GO_DATA WHERE ID >= 10 ORDER BY ID")
	if err != nil {
		t.Fatalf("NULL and empty text query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int64
		var value sql.NullString
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("NULL and empty text scan: %v", err)
		}
		if count >= 2 || id != int64(10+count) {
			t.Fatalf("NULL and empty text row %d = ID %d", count, id)
		}
		if count == 0 {
			if !value.Valid || value.String != "" {
				t.Fatalf("empty text scan = %#v, want valid empty text", value)
			}
		} else if value.Valid {
			t.Fatalf("NULL text scan = %#v, want invalid NullString", value)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("NULL and empty text row count = %d, want 2", count)
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_blob, TestCharsetConversion.testBlob,
// and TestBugs.test_pyib_30.
func TestTypesTextAndBinaryBlobRoundTrips(t *testing.T) {
	smallText := "BLOB \u00e9 \u4e2d \U0001f40d\x00tail"
	largeText := strings.Repeat("1234567890", 9000)
	smallBinary := []byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10}
	largeBinary := make([]byte, 90000)
	for i := range largeBinary {
		largeBinary[i] = byte(i)
	}

	for _, test := range []struct {
		name       string
		id         int64
		insert     string
		selectStmt string
		value      any
		text       bool
		wantText   string
		wantBinary []byte
	}{
		{
			name:       "smalltext",
			id:         10,
			insert:     "INSERT INTO GO_DATA (ID, TEXT_BLOB) VALUES (?, ?)",
			selectStmt: "SELECT ID, TEXT_BLOB FROM GO_DATA WHERE ID = ? ORDER BY ID",
			value:      smallText,
			text:       true,
			wantText:   smallText,
		},
		{
			name:       "large90000text",
			id:         10,
			insert:     "INSERT INTO GO_DATA (ID, TEXT_BLOB) VALUES (?, ?)",
			selectStmt: "SELECT ID, TEXT_BLOB FROM GO_DATA WHERE ID = ? ORDER BY ID",
			value:      largeText,
			text:       true,
			wantText:   largeText,
		},
		{
			name:       "binary0..10",
			id:         10,
			insert:     "INSERT INTO GO_DATA (ID, BINARY_BLOB) VALUES (?, ?)",
			selectStmt: "SELECT ID, BINARY_BLOB FROM GO_DATA WHERE ID = ? ORDER BY ID",
			value:      smallBinary,
			wantBinary: smallBinary,
		},
		{
			name:       "large90000binary",
			id:         10,
			insert:     "INSERT INTO GO_DATA (ID, BINARY_BLOB) VALUES (?, ?)",
			selectStmt: "SELECT ID, BINARY_BLOB FROM GO_DATA WHERE ID = ? ORDER BY ID",
			value:      largeBinary,
			wantBinary: largeBinary,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			db := newDatabase(t)
			ctx := readContextWithTimeout(t, longReadTestTimeout)
			result, err := db.ExecContext(ctx, test.insert, test.id, test.value)
			if err != nil {
				t.Fatalf("insert BLOB row %d: %v", test.id, err)
			}
			requireRowsAffected(t, result, 1)

			rows, err := db.QueryContext(ctx, test.selectStmt, test.id)
			if err != nil {
				t.Fatalf("BLOB query: %v", err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("BLOB query returned no row: %v", rows.Err())
			}
			var id int64
			if test.text {
				var got string
				if err := rows.Scan(&id, &got); err != nil {
					t.Fatalf("text BLOB scan: %v", err)
				}
				if id != test.id || got != test.wantText {
					t.Fatalf("text BLOB row = (%d, length %d), want (%d, length %d)",
						id, len(got), test.id, len(test.wantText))
				}
			} else {
				var got []byte
				if err := rows.Scan(&id, &got); err != nil {
					t.Fatalf("binary BLOB scan: %v", err)
				}
				if id != test.id || !bytes.Equal(got, test.wantBinary) {
					t.Fatalf("binary BLOB row = (%d, length %d), want (%d, length %d)",
						id, len(got), test.id, len(test.wantBinary))
				}
			}
			finishReadRows(t, rows)
		})
	}
}

// Source mapping: DatabaseAPI20Test.test_Binary and
// DatabaseAPI20Test.test_None.
func TestTypesEmptyBytesDistinguishNull(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	for i, value := range []any{[]byte{}, nil} {
		result, err := db.ExecContext(ctx,
			"INSERT INTO GO_DATA (ID, BINARY_BLOB) VALUES (?, ?)", int64(10+i), value)
		if err != nil {
			t.Fatalf("insert binary value %d: %v", i, err)
		}
		requireRowsAffected(t, result, 1)
	}

	rows, err := db.QueryContext(ctx,
		"SELECT ID, BINARY_BLOB FROM GO_DATA WHERE ID >= 10 ORDER BY ID")
	if err != nil {
		t.Fatalf("empty bytes and NULL query: %v", err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var id int64
		var value []byte
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("empty bytes and NULL scan: %v", err)
		}
		if count >= 2 || id != int64(10+count) {
			t.Fatalf("empty bytes and NULL row %d = ID %d", count, id)
		}
		if count == 0 {
			if value == nil || len(value) != 0 {
				t.Fatalf("empty []byte scan = %#v, want non-nil empty bytes", value)
			}
		} else if value != nil {
			t.Fatalf("NULL []byte scan = %#v, want nil", value)
		}
		count++
	}
	if count != 2 {
		t.Fatalf("empty bytes and NULL row count = %d, want 2", count)
	}
	finishReadRows(t, rows)
}

// Source mapping: TestBugs.test_pyib_25.
func TestTypesVarchar5000Insert(t *testing.T) {
	for _, length := range []int{0, 1, 5000} {
		t.Run("length-"+strconv.Itoa(length), func(t *testing.T) {
			db := newDatabase(t)
			ctx := readContextWithTimeout(t, longReadTestTimeout)
			if _, err := db.ExecContext(ctx,
				"CREATE TABLE GO_LONG (ID INTEGER NOT NULL PRIMARY KEY, TEXT_VALUE VARCHAR(5000))"); err != nil {
				t.Fatalf("create GO_LONG: %v", err)
			}

			seed := "0123456789"
			value := strings.Repeat(seed, length/len(seed)) + seed[:length%len(seed)]
			result, err := db.ExecContext(ctx,
				"INSERT INTO GO_LONG (ID, TEXT_VALUE) VALUES (?, ?)", int64(1), value)
			if err != nil {
				t.Fatalf("insert VARCHAR(%d): %v", length, err)
			}
			requireRowsAffected(t, result, 1)

			rows, err := db.QueryContext(ctx,
				"SELECT ID, TEXT_VALUE FROM GO_LONG ORDER BY ID")
			if err != nil {
				t.Fatalf("query VARCHAR(%d): %v", length, err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("VARCHAR(%d) returned no row: %v", length, rows.Err())
			}
			var id int64
			var got string
			if err := rows.Scan(&id, &got); err != nil {
				t.Fatalf("scan VARCHAR(%d): %v", length, err)
			}
			if id != 1 || len(got) != length || got != value {
				t.Fatalf("VARCHAR(%d) row = (%d, length %d), want (1, length %d, input)",
					length, id, len(got), length)
			}
			finishReadRows(t, rows)
		})
	}
}
