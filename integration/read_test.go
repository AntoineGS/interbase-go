//go:build integration

// These tests adapt selected InterBasePython contracts. See
// integration/UPSTREAM_LICENSE.txt for attribution, licensing, and the source
// commit.
package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	readTestTimeout      = 30 * time.Second
	longReadTestTimeout  = 2 * time.Minute
	earlyCloseIterations = 20
)

func readContext(t *testing.T) context.Context {
	t.Helper()
	return readContextWithTimeout(t, readTestTimeout)
}

func readContextWithTimeout(t *testing.T, timeout time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	t.Cleanup(cancel)
	return ctx
}

func finishReadRows(t *testing.T, rows *sql.Rows) {
	t.Helper()
	if rows.Next() {
		t.Fatal("rows returned more rows than expected")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows.Err() = %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("rows.Close() = %v", err)
	}
}

// Source mapping: DatabaseAPI20Test.test_connect, DatabaseAPI20Test.test_close,
// and TestCursor.test_exec_after_close.
func TestReadConnectionLifecycle(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("initial PingContext: %v", err)
	}

	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("DB.Conn: %v", err)
	}
	defer connection.Close()
	if err := connection.PingContext(ctx); err != nil {
		_ = connection.Close()
		t.Fatalf("connection PingContext: %v", err)
	}
	if err := connection.Close(); err != nil {
		t.Fatalf("connection.Close: %v", err)
	}

	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("PingContext after connection close: %v", err)
	}
	var country string
	if err := db.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("query after connection reuse: %v", err)
	}
	if country != "USA" {
		t.Fatalf("country after connection reuse = %q, want %q", country, "USA")
	}

	stats := db.Stats()
	if stats.MaxOpenConnections != 1 || stats.OpenConnections != 1 || stats.InUse != 0 || stats.Idle != 1 {
		t.Fatalf("unexpected connection pool stats after reuse: %+v", stats)
	}
}

// Source mapping: TestCursor.test_iteration, TestCursor.test_description,
// and DatabaseAPI20Test.test_fetchall.
func TestReadRowsColumnsAndEOF(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT COUNTRY, CURRENCY FROM GO_COUNTRY ORDER BY ID")
	if err != nil {
		t.Fatalf("country query: %v", err)
	}
	defer rows.Close()

	wantColumns := []string{"COUNTRY", "CURRENCY"}
	columns, err := rows.Columns()
	if err != nil {
		t.Fatalf("columns: %v", err)
	}
	if !reflect.DeepEqual(columns, wantColumns) {
		t.Fatalf("columns = %#v, want %#v", columns, wantColumns)
	}

	wantRows := [][2]string{
		{"USA", "Dollar"},
		{"England", "Pound"},
		{"Japan", "Yen"},
	}
	gotRows := make([][2]string, 0, len(wantRows))
	for rows.Next() {
		var row [2]string
		if err := rows.Scan(&row[0], &row[1]); err != nil {
			t.Fatalf("scan country row: %v", err)
		}
		gotRows = append(gotRows, row)
	}
	if !reflect.DeepEqual(gotRows, wantRows) {
		t.Fatalf("country rows = %#v, want %#v", gotRows, wantRows)
	}
	if len(gotRows) != 3 {
		t.Fatalf("country row count = %d, want 3", len(gotRows))
	}
	finishReadRows(t, rows)

	var country string
	if err := db.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(2)).Scan(&country); err != nil {
		t.Fatalf("query after EOF and close: %v", err)
	}
	if country != "England" {
		t.Fatalf("country after EOF and close = %q, want %q", country, "England")
	}
}

// Source mapping: DatabaseAPI20Test.test_fetchone and
// DatabaseAPI20Test.test_fetchmany.
func TestReadEmptyRowsAndErrNoRows(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(99))
	if err != nil {
		t.Fatalf("empty query: %v", err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("empty query returned a row")
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("empty query Rows.Err() = %v", err)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("empty query Rows.Close() = %v", err)
	}

	var id int64
	err = db.QueryRowContext(ctx,
		"SELECT ID FROM GO_COUNTRY WHERE ID = ?", int64(99)).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty QueryRow Scan error = %v, want sql.ErrNoRows", err)
	}
}

// Source mapping: DatabaseAPI20Test.test_None.
func TestReadNullAndEmptyString(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT ID, TEXT_VALUE FROM GO_DATA WHERE ID IN (2, 3) ORDER BY ID")
	if err != nil {
		t.Fatalf("NULL and empty query: %v", err)
	}
	defer rows.Close()

	wantIDs := []int64{2, 3}
	count := 0
	for rows.Next() {
		var id int64
		var value sql.NullString
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan NULL and empty row: %v", err)
		}
		if count >= len(wantIDs) {
			t.Fatalf("unexpected extra NULL and empty row with ID %d", id)
		}
		if id != wantIDs[count] {
			t.Fatalf("row %d ID = %d, want %d", count, id, wantIDs[count])
		}
		switch id {
		case 2:
			if !value.Valid || value.String != "" {
				t.Fatalf("empty string scan = %#v, want valid empty string", value)
			}
		case 3:
			if value.Valid {
				t.Fatalf("NULL scan = %#v, want invalid NullString", value)
			}
		}
		count++
	}
	if count != len(wantIDs) {
		t.Fatalf("NULL and empty row count = %d, want %d", count, len(wantIDs))
	}
	finishReadRows(t, rows)
}

// Source mapping: DatabaseAPI20Test.test_execute and TestInsertData.test_insert_float_double.
func TestReadPositionalCasts(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	special := "quote ' and \" with spaces %s :may ca%(u)se? troub:1e"
	tests := []struct {
		name  string
		query string
		arg   any
		want  any
	}{
		{
			name:  "special text",
			query: "SELECT CAST(? AS VARCHAR(80)) FROM RDB$DATABASE",
			arg:   special,
			want:  special,
		},
		{
			name:  "UTF8 text",
			query: "SELECT CAST(? AS VARCHAR(40) CHARACTER SET UTF8) FROM RDB$DATABASE",
			arg:   "caf\u00e9",
			want:  "caf\u00e9",
		},
		{
			name:  "empty text",
			query: "SELECT CAST(? AS VARCHAR(40)) FROM RDB$DATABASE",
			arg:   "",
			want:  "",
		},
		{
			name:  "int64",
			query: "SELECT CAST(? AS INTEGER) FROM RDB$DATABASE",
			arg:   int64(-42),
			want:  int64(-42),
		},
		{
			name:  "float64",
			query: "SELECT CAST(? AS DOUBLE PRECISION) FROM RDB$DATABASE",
			arg:   float64(1.25),
			want:  float64(1.25),
		},
		{
			name:  "boolean",
			query: "SELECT CAST(? AS BOOLEAN) FROM RDB$DATABASE",
			arg:   true,
			want:  true,
		},
		{
			name:  "NULL",
			query: "SELECT CAST(? AS VARCHAR(40)) FROM RDB$DATABASE",
			arg:   nil,
			want:  nil,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, test.query, test.arg)
			if err != nil {
				t.Fatalf("positional cast query: %v", err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("positional cast returned no row: %v", rows.Err())
			}
			var got any
			if err := rows.Scan(&got); err != nil {
				t.Fatalf("positional cast scan: %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("positional cast = %#v, want %#v", got, test.want)
			}
			finishReadRows(t, rows)
		})
	}
}

// Source mapping: TestInsertData.test_insert_integers,
// TestInsertData.test_insert_float_double, and TestInsertData.test_insert_boolean.
func TestReadPositionalFilters(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	tests := []struct {
		name  string
		query string
		arg   any
		want  []int64
	}{
		{
			name:  "boolean",
			query: "SELECT ID FROM GO_DATA WHERE ACTIVE_VALUE = ? ORDER BY ID",
			arg:   true,
			want:  []int64{2},
		},
		{
			name:  "int64",
			query: "SELECT ID FROM GO_DATA WHERE INT_VALUE = ? ORDER BY ID",
			arg:   int64(2147483647),
			want:  []int64{2},
		},
		{
			name:  "float64",
			query: "SELECT ID FROM GO_DATA WHERE DOUBLE_VALUE = ? ORDER BY ID",
			arg:   float64(-2.5),
			want:  []int64{2},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, test.query, test.arg)
			if err != nil {
				t.Fatalf("positional filter: %v", err)
			}
			defer rows.Close()

			got := make([]int64, 0, len(test.want))
			for rows.Next() {
				var id int64
				if err := rows.Scan(&id); err != nil {
					t.Fatalf("scan positional filter: %v", err)
				}
				got = append(got, id)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("positional filter IDs = %#v, want %#v", got, test.want)
			}
			if len(got) != len(test.want) {
				t.Fatalf("positional filter row count = %d, want %d", len(got), len(test.want))
			}
			finishReadRows(t, rows)
		})
	}
}

// Source mapping: TestCursor.test_iteration and the cursor-reuse regression
// represented by TestBugs.test_pyib_34.
func TestReadRepeatedEarlyClose(t *testing.T) {
	db := newDatabase(t)
	ctx := readContextWithTimeout(t, longReadTestTimeout)

	for iteration := 0; iteration < earlyCloseIterations; iteration++ {
		rows, err := db.QueryContext(ctx,
			"SELECT ID, COUNTRY FROM GO_COUNTRY ORDER BY ID")
		if err != nil {
			t.Fatalf("early-close query %d: %v", iteration, err)
		}
		defer rows.Close()
		if !rows.Next() {
			t.Fatalf("early-close query %d returned no first row: %v", iteration, rows.Err())
		}
		var id int64
		var country string
		if err := rows.Scan(&id, &country); err != nil {
			t.Fatalf("early-close scan %d: %v", iteration, err)
		}
		if id != 1 || country != "USA" {
			t.Fatalf("early-close first row %d = (%d, %q), want (1, %q)",
				iteration, id, country, "USA")
		}
		if err := rows.Close(); err != nil {
			t.Fatalf("early-close rows.Close() %d: %v", iteration, err)
		}
		if rows.Next() {
			t.Fatalf("closed rows %d returned another row", iteration)
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("closed rows.Err() %d: %v", iteration, err)
		}
	}

	var country string
	if err := db.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(3)).Scan(&country); err != nil {
		t.Fatalf("query after repeated early close: %v", err)
	}
	if country != "Japan" {
		t.Fatalf("country after repeated early close = %q, want %q", country, "Japan")
	}
	stats := db.Stats()
	if stats.OpenConnections != 1 || stats.InUse != 0 || stats.Idle != 1 {
		t.Fatalf("unexpected pool stats after early close: %+v", stats)
	}
}

// Source mapping: TestBugs.test_pyib_35 and DatabaseAPI20Test.test_fetchone.
func TestReadRecoveryAfterInvalidQuery(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx, "SELECT FROM GO_COUNTRY")
	if rows != nil {
		_ = rows.Close()
		t.Fatal("invalid query returned rows")
	}
	if err == nil {
		t.Fatal("invalid query returned nil error")
	}
	if err := db.PingContext(ctx); err != nil {
		t.Fatalf("PingContext after invalid query: %v", err)
	}

	var country string
	if err := db.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("query after invalid query: %v", err)
	}
	if country != "USA" {
		t.Fatalf("country after invalid query = %q, want %q", country, "USA")
	}
}

// Source mapping: DatabaseAPI20Test.test_execute and TestBugs.test_pyib_35.
func TestReadRecoveryAfterArgumentCountError(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	cases := []struct {
		name  string
		query string
		args  []any
	}{
		{
			name:  "missing argument",
			query: "SELECT CAST(? AS INTEGER) FROM RDB$DATABASE",
		},
		{
			name:  "extra argument",
			query: "SELECT 1 FROM RDB$DATABASE",
			args:  []any{int64(1)},
		},
	}

	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, test.query, test.args...)
			if rows != nil {
				_ = rows.Close()
				t.Fatal("argument-count query returned rows")
			}
			if err == nil {
				t.Fatal("argument-count query returned nil error")
			}
			if err := db.PingContext(ctx); err != nil {
				t.Fatalf("PingContext after argument-count error: %v", err)
			}
		})
	}

	var country string
	if err := db.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(2)).Scan(&country); err != nil {
		t.Fatalf("query after argument-count errors: %v", err)
	}
	if country != "England" {
		t.Fatalf("country after argument-count errors = %q, want %q", country, "England")
	}
}

// Source mapping: TestBugs.test_pyib_35; context cancellation is the
// database/sql adaptation of the original cursor-lifecycle error boundary.
func TestReadPreCancelledQuery(t *testing.T) {
	db := newDatabase(t)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	rows, err := db.QueryContext(ctx, "SELECT COUNTRY FROM GO_COUNTRY ORDER BY ID")
	if rows != nil {
		_ = rows.Close()
		t.Fatal("pre-cancelled query returned rows")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancelled query error = %v, want context.Canceled", err)
	}

	freshContext := readContext(t)
	var country string
	if err := db.QueryRowContext(freshContext,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("query after pre-cancelled query: %v", err)
	}
	if country != "USA" {
		t.Fatalf("country after pre-cancelled query = %q, want %q", country, "USA")
	}
}

// Source mapping: TestInsertData.test_insert_char_varchar.
func TestReadCHARPadding(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT ID, FIXED_VALUE FROM GO_DATA WHERE ID IN (1, 2) ORDER BY ID")
	if err != nil {
		t.Fatalf("CHAR query: %v", err)
	}
	defer rows.Close()

	want := []struct {
		id    int64
		value string
	}{
		{1, "AA   "},
		{2, "ZZ   "},
	}
	count := 0
	for rows.Next() {
		var id int64
		var value string
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan CHAR row: %v", err)
		}
		if count >= len(want) {
			t.Fatalf("unexpected extra CHAR row = (%d, %q)", id, value)
		}
		if id != want[count].id || value != want[count].value {
			t.Fatalf("CHAR row %d = (%d, %q), want (%d, %q)",
				count, id, value, want[count].id, want[count].value)
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("CHAR row count = %d, want %d", count, len(want))
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_integers.
func TestReadIntegerBoundaries(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT ID, SMALL_VALUE, INT_VALUE FROM GO_DATA WHERE ID IN (1, 2) ORDER BY ID")
	if err != nil {
		t.Fatalf("integer boundary query: %v", err)
	}
	defer rows.Close()

	want := [][3]int64{
		{1, -32768, -2147483648},
		{2, 32767, 2147483647},
	}
	got := make([][3]int64, 0, len(want))
	for rows.Next() {
		var row [3]int64
		if err := rows.Scan(&row[0], &row[1], &row[2]); err != nil {
			t.Fatalf("scan integer boundary row: %v", err)
		}
		got = append(got, row)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("integer boundary rows = %#v, want %#v", got, want)
	}
	if len(got) != 2 {
		t.Fatalf("integer boundary row count = %d, want 2", len(got))
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_datetime.
func TestReadTimestampFraction(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	var got time.Time
	if err := db.QueryRowContext(ctx,
		"SELECT MOMENT FROM GO_DATA WHERE ID = ?", int64(1)).Scan(&got); err != nil {
		t.Fatalf("timestamp query: %v", err)
	}
	want := time.Date(2011, time.November, 13, 15, 0, 1, 200000, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("timestamp = %v, want %v", got, want)
	}
	if got.Nanosecond() != want.Nanosecond() {
		t.Fatalf("timestamp nanoseconds = %d, want %d", got.Nanosecond(), want.Nanosecond())
	}
}

// Source mapping: TestInsertData.test_insert_numeric_decimal.
func TestReadNumeric9ExactStrings(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT ID, FIXED_DECIMAL FROM GO_DATA WHERE ID IN (1, 2) ORDER BY ID")
	if err != nil {
		t.Fatalf("NUMERIC(9,2) query: %v", err)
	}
	defer rows.Close()

	want := []struct {
		id    int64
		value string
	}{
		{1, "100.11"},
		{2, "-1.10"},
	}
	count := 0
	for rows.Next() {
		var id int64
		var value string
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan NUMERIC(9,2) row: %v", err)
		}
		if count >= len(want) {
			t.Fatalf("unexpected extra NUMERIC(9,2) row = (%d, %q)", id, value)
		}
		if id != want[count].id || value != want[count].value {
			t.Fatalf("NUMERIC(9,2) row %d = (%d, %q), want (%d, %q)",
				count, id, value, want[count].id, want[count].value)
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("NUMERIC(9,2) row count = %d, want %d", count, len(want))
	}
	finishReadRows(t, rows)
}

// Source mapping: TestInsertData.test_insert_numeric_decimal. Dialect 1 may
// describe NUMERIC(12,2) as a scaled floating result; this is intentionally a
// separate red contract rather than an expected-unsupported assertion.
func TestReadWideDecimalDialectOneFloatTolerance(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx,
		"SELECT ID, WIDE_DECIMAL FROM GO_DATA WHERE ID IN (1, 2) ORDER BY ID")
	if err != nil {
		t.Fatalf("NUMERIC(12,2) query: %v", err)
	}
	defer rows.Close()

	want := []struct {
		id    int64
		value float64
	}{
		{1, 100.11},
		{2, -1.10},
	}
	count := 0
	for rows.Next() {
		var id int64
		var value float64
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan NUMERIC(12,2) row: %v", err)
		}
		if count >= len(want) {
			t.Fatalf("unexpected extra NUMERIC(12,2) row = (%d, %.17g)", id, value)
		}
		if id != want[count].id {
			t.Fatalf("NUMERIC(12,2) row ID = %d, want %d", id, want[count].id)
		}
		if math.IsNaN(value) || math.Abs(value-want[count].value) > 1e-9 {
			t.Fatalf("NUMERIC(12,2) value = %.17g, want approximately %.17g", value, want[count].value)
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("NUMERIC(12,2) row count = %d, want %d", count, len(want))
	}
	finishReadRows(t, rows)
}

// Source mapping: TestStoredProc.test_callproc.
func TestReadSelectableBudgetProcedure(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx, `
		SELECT TOT_BUDGET, AVG_BUDGET, MIN_BUDGET, MAX_BUDGET
		FROM GO_SUB_TOT_BUDGET('100')`)
	if err != nil {
		t.Fatalf("selectable procedure query: %v", err)
	}
	defer rows.Close()

	want := [4]string{"3800000.00", "760000.00", "500000.00", "1500000.00"}
	count := 0
	for rows.Next() {
		var got [4]string
		if err := rows.Scan(&got[0], &got[1], &got[2], &got[3]); err != nil {
			t.Fatalf("scan selectable procedure row: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("selectable procedure row = %#v, want %#v", got, want)
		}
		count++
	}
	if count != 1 {
		t.Fatalf("selectable procedure row count = %d, want 1", count)
	}
	finishReadRows(t, rows)
}

// Source mapping: TestCharsetConversion.test_utf82win1250.
func TestReadWIN1250UTF8RoundTrip(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	want := "\u011b\u0161\u010d\u0159\u017e\u00fd\u00e1\u00ed\u00e9\u00fa\u016f\u010f\u0165\u0148\u00f3\u011a\u0160\u010c\u0158\u017d\u00dd\u00c1\u00cd\u00c9\u00da\u016e\u010e\u0164\u0147\u00d3"
	rows, err := db.QueryContext(ctx, `
		SELECT CAST(
			CAST(? AS VARCHAR(40) CHARACTER SET WIN1250)
			AS VARCHAR(40) CHARACTER SET UTF8
		) FROM RDB$DATABASE`, want)
	if err != nil {
		t.Fatalf("WIN1250/UTF8 cast query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("WIN1250/UTF8 cast returned no row: %v", rows.Err())
	}
	var got string
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("WIN1250/UTF8 cast scan: %v", err)
	}
	if got != want {
		t.Fatalf("WIN1250/UTF8 round trip = %q, want %q", got, want)
	}
	finishReadRows(t, rows)
}

// Source mapping: TestBugs.test_pyib_25. This is a read-side cast contract;
// the write round trip belongs to the later write-contract task.
func TestReadVarchar5000Cast(t *testing.T) {
	db := newDatabase(t)
	ctx := readContextWithTimeout(t, longReadTestTimeout)

	want := strings.Repeat("1234567890", 500)
	rows, err := db.QueryContext(ctx,
		"SELECT CAST(? AS VARCHAR(5000)) FROM RDB$DATABASE", want)
	if err != nil {
		t.Fatalf("VARCHAR(5000) cast query: %v", err)
	}
	defer rows.Close()
	if !rows.Next() {
		t.Fatalf("VARCHAR(5000) cast returned no row: %v", rows.Err())
	}
	var got string
	if err := rows.Scan(&got); err != nil {
		t.Fatalf("VARCHAR(5000) cast scan: %v", err)
	}
	if len(got) != 5000 || got != want {
		t.Fatalf("VARCHAR(5000) result length/value = (%d, %q), want length 5000 and the input", len(got), got)
	}
	finishReadRows(t, rows)
}

// Source mapping: TestBugs.test_pyib_22. This is a read-side cast contract;
// the write round trip belongs to the later write-contract task.
func TestReadVarcharParameterLengths(t *testing.T) {
	db := newDatabase(t)
	for length := 0; length <= 254; length++ {
		t.Run("length-"+strconv.Itoa(length), func(t *testing.T) {
			ctx := readContext(t)
			seed := strings.Repeat("0123456789", (length+9)/10)
			want := seed[:length]
			rows, err := db.QueryContext(ctx,
				"SELECT CAST(? AS VARCHAR(255)) FROM RDB$DATABASE", want)
			if err != nil {
				t.Fatalf("VARCHAR length %d query: %v", length, err)
			}
			defer rows.Close()
			if !rows.Next() {
				t.Fatalf("VARCHAR length %d returned no row: %v", length, rows.Err())
			}
			var got string
			if err := rows.Scan(&got); err != nil {
				t.Fatalf("scan VARCHAR length %d: %v", length, err)
			}
			if len(got) != length || got != want {
				t.Fatalf("VARCHAR length %d result = (%d, %q), want (%d, %q)",
					length, len(got), got, length, want)
			}
			finishReadRows(t, rows)
		})
	}
}
