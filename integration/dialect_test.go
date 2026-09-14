//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
	"time"
)

const dialectTypesSchema = `CREATE TABLE GO_DIALECT_TYPES (
    ID INTEGER NOT NULL PRIMARY KEY,
	BIG_VALUE NUMERIC(18,0),
	DISTINCT_VALUE NUMERIC(18,0),
    DATE_VALUE DATE,
    TIME_VALUE TIME,
    TIMESTAMP_VALUE TIMESTAMP,
    NUMERIC_VALUE NUMERIC(18,4),
    DECIMAL_VALUE DECIMAL(18,2)
)`

func createDialectTypes(t *testing.T, db *sql.DB, ctx context.Context) {
	t.Helper()
	if _, err := db.ExecContext(ctx, dialectTypesSchema); err != nil {
		t.Fatalf("create Dialect 3 type fixtures: %v", err)
	}
}

// TestDialectConfigurationAndQuotedIdentifiers proves the attachment and
// matching fixture honor the default and both supported explicit dialects.
func TestDialectConfigurationAndQuotedIdentifiers(t *testing.T) {
	dateTime := time.Date(2024, time.February, 29, 12, 34, 56, 0, time.UTC)
	tests := []struct {
		name                 string
		dialect              int
		useQuotedIdentifier  bool
		wantDatabaseTypeName string
		wantDate             time.Time
	}{
		{
			name:                 "zero uses dialect three",
			dialect:              0,
			useQuotedIdentifier:  true,
			wantDatabaseTypeName: "DATE",
			wantDate:             time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
		},
		{
			name:                 "explicit dialect one",
			dialect:              1,
			wantDatabaseTypeName: "TIMESTAMP",
			wantDate:             dateTime,
		},
		{
			name:                 "explicit dialect three",
			dialect:              3,
			useQuotedIdentifier:  true,
			wantDatabaseTypeName: "DATE",
			wantDate:             time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db := newDatabaseWithDialect(t, test.dialect)
			ctx := readContext(t)
			column := "DATE_VALUE"
			if test.useQuotedIdentifier {
				column = `"QuotedValue"`
			}
			if _, err := db.ExecContext(ctx,
				"CREATE TABLE GO_DIALECT_CONFIGURATION ("+column+" DATE)"); err != nil {
				t.Fatalf("create %s DATE table: %v", test.name, err)
			}
			if _, err := db.ExecContext(ctx,
				"INSERT INTO GO_DIALECT_CONFIGURATION ("+column+") VALUES (?)", dateTime); err != nil {
				t.Fatalf("insert %s DATE value: %v", test.name, err)
			}

			var value time.Time
			if err := db.QueryRowContext(ctx,
				"SELECT "+column+" FROM GO_DIALECT_CONFIGURATION").Scan(&value); err != nil {
				t.Fatalf("query %s DATE value: %v", test.name, err)
			}
			if !value.Equal(test.wantDate) {
				t.Fatalf("%s DATE value = %v, want %v", test.name, value, test.wantDate)
			}

			rows, err := db.QueryContext(ctx,
				"SELECT "+column+" FROM GO_DIALECT_CONFIGURATION WHERE 1 = 0")
			if err != nil {
				t.Fatalf("query %s DATE metadata: %v", test.name, err)
			}
			columns, err := rows.ColumnTypes()
			if err != nil {
				_ = rows.Close()
				t.Fatalf("read %s DATE metadata: %v", test.name, err)
			}
			if len(columns) != 1 || columns[0].DatabaseTypeName() != test.wantDatabaseTypeName {
				_ = rows.Close()
				t.Fatalf("%s DATE metadata = %#v, want type %q", test.name, columns, test.wantDatabaseTypeName)
			}
			finishReadRows(t, rows)

			if test.dialect == 1 {
				var literal string
				if err := db.QueryRowContext(ctx,
					`SELECT CAST("dialect one" AS VARCHAR(11)) FROM RDB$DATABASE`).Scan(&literal); err != nil {
					t.Fatalf("query Dialect 1 double-quoted string: %v", err)
				}
				if literal != "dialect one" {
					t.Fatalf("Dialect 1 double-quoted string = %q, want %q", literal, "dialect one")
				}
			}
		})
	}
}

func TestDialectOneNumericTextArgumentsKeepServerConversion(t *testing.T) {
	db := newDatabaseWithDialect(t, 1)
	ctx := readContext(t)
	const value = " 1.23 "

	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DATA (ID, FIXED_DECIMAL) VALUES (?, ?)", int64(10), value); err != nil {
		t.Fatalf("direct Dialect 1 numeric text insert: %v", err)
	}

	stmt, err := db.PrepareContext(ctx,
		"INSERT INTO GO_DATA (ID, FIXED_DECIMAL) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("prepare Dialect 1 numeric text insert: %v", err)
	}
	if _, err := stmt.ExecContext(ctx, int64(11), value); err != nil {
		_ = stmt.Close()
		t.Fatalf("prepared Dialect 1 numeric text insert: %v", err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close Dialect 1 numeric text insert: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
SELECT ID, FIXED_DECIMAL FROM GO_DATA WHERE ID IN (?, ?) ORDER BY ID`, int64(10), int64(11))
	if err != nil {
		t.Fatalf("query Dialect 1 numeric text rows: %v", err)
	}
	defer rows.Close()
	for wantID := int64(10); wantID <= 11; wantID++ {
		if !rows.Next() {
			t.Fatalf("Dialect 1 numeric text row %d missing: %v", wantID, rows.Err())
		}
		var id int64
		var got string
		if err := rows.Scan(&id, &got); err != nil {
			t.Fatalf("scan Dialect 1 numeric text row %d: %v", wantID, err)
		}
		if id != wantID || got != "1.23" {
			t.Fatalf("Dialect 1 numeric text row = (%d, %q), want (%d, %q)",
				id, got, wantID, "1.23")
		}
	}
	finishReadRows(t, rows)
}

func TestDialectDateTimeAndBigIntRoundTripDirectAndPrepared(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	createDialectTypes(t, db, ctx)

	date := time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC)
	boundary, err := time.Parse("15:04:05.9999", "23:59:59.9999")
	if err != nil {
		t.Fatalf("parse Dialect 3 boundary TIME: %v", err)
	}
	timestamp := time.Date(2024, time.February, 29, 12, 34, 56, 123400000, time.UTC)
	const bigValue int64 = 9007199254740993

	if _, err := db.ExecContext(ctx, `
INSERT INTO GO_DIALECT_TYPES
    (ID, BIG_VALUE, DISTINCT_VALUE, DATE_VALUE, TIME_VALUE, TIMESTAMP_VALUE)
VALUES (?, ?, ?, ?, ?, ?)`, int64(1), bigValue, bigValue, date, boundary, timestamp); err != nil {
		t.Fatalf("direct Dialect 3 temporal insert: %v", err)
	}

	stmt, err := db.PrepareContext(ctx, `
INSERT INTO GO_DIALECT_TYPES
    (ID, BIG_VALUE, DISTINCT_VALUE, DATE_VALUE, TIME_VALUE, TIMESTAMP_VALUE)
VALUES (?, ?, ?, ?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare Dialect 3 temporal insert: %v", err)
	}
	if _, err := stmt.ExecContext(ctx, int64(2), int64(-bigValue), int64(-bigValue),
		date, boundary,
		time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC)); err != nil {
		_ = stmt.Close()
		t.Fatalf("prepared Dialect 3 temporal insert: %v", err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close Dialect 3 temporal insert: %v", err)
	}

	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DIALECT_TYPES (ID) VALUES (?)", int64(3)); err != nil {
		t.Fatalf("insert Dialect 3 NULL temporal row: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
SELECT ID, BIG_VALUE, DISTINCT_VALUE, DATE_VALUE, TIME_VALUE, TIMESTAMP_VALUE
FROM GO_DIALECT_TYPES ORDER BY ID`)
	if err != nil {
		t.Fatalf("query Dialect 3 temporal rows: %v", err)
	}
	defer rows.Close()

	want := []struct {
		id        int64
		big       int64
		distinct  int64
		date      time.Time
		time      time.Time
		timestamp time.Time
	}{
		{
			id: 1, big: bigValue, distinct: bigValue,
			date:      date,
			time:      time.Date(1900, time.January, 1, 23, 59, 59, 999900000, time.UTC),
			timestamp: timestamp,
		},
		{
			id: 2, big: -bigValue, distinct: -bigValue,
			date: date, time: time.Date(1900, time.January, 1, 23, 59, 59, 999900000, time.UTC),
			timestamp: time.Date(2024, time.February, 29, 0, 0, 0, 0, time.UTC),
		},
	}
	for index := 0; index < len(want); index++ {
		if !rows.Next() {
			t.Fatalf("Dialect 3 temporal row %d missing: %v", index, rows.Err())
		}
		var got struct {
			id        int64
			big       int64
			distinct  int64
			date      time.Time
			time      time.Time
			timestamp time.Time
		}
		if err := rows.Scan(&got.id, &got.big, &got.distinct, &got.date,
			&got.time, &got.timestamp); err != nil {
			t.Fatalf("scan Dialect 3 temporal row %d: %v", index, err)
		}
		if got.id != want[index].id || got.big != want[index].big ||
			got.distinct != want[index].distinct || !got.date.Equal(want[index].date) ||
			!got.time.Equal(want[index].time) || !got.timestamp.Equal(want[index].timestamp) ||
			got.timestamp.Nanosecond() != want[index].timestamp.Nanosecond() ||
			got.time.Nanosecond() != want[index].time.Nanosecond() {
			t.Fatalf("Dialect 3 temporal row %d = %#v, want %#v", index, got, want[index])
		}
	}
	if !rows.Next() {
		t.Fatalf("Dialect 3 NULL row missing: %v", rows.Err())
	}
	var nullRow struct {
		id                    int64
		big, distinct         sql.NullInt64
		date, time, timestamp sql.NullTime
	}
	if err := rows.Scan(&nullRow.id, &nullRow.big, &nullRow.distinct,
		&nullRow.date, &nullRow.time, &nullRow.timestamp); err != nil {
		t.Fatalf("scan Dialect 3 NULL temporal row: %v", err)
	}
	if nullRow.id != 3 || nullRow.big.Valid || nullRow.distinct.Valid ||
		nullRow.date.Valid || nullRow.time.Valid || nullRow.timestamp.Valid {
		t.Fatalf("Dialect 3 NULL row = %#v, want all nullable values invalid", nullRow)
	}
	finishReadRows(t, rows)
}

func TestDialectTemporalYearValidationIsTargetAware(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	createDialectTypes(t, db, ctx)

	timeOnly, err := time.Parse("15:04:05.9999", "23:59:59.9999")
	if err != nil {
		t.Fatalf("parse Dialect 3 TIME: %v", err)
	}
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DIALECT_TYPES (ID, TIME_VALUE) VALUES (?, ?)", int64(1), timeOnly); err != nil {
		t.Fatalf("year-zero SQL TIME was rejected: %v", err)
	}

	invalidYear := time.Date(0, time.January, 1, 0, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		name   string
		column string
	}{
		{name: "DATE", column: "DATE_VALUE"},
		{name: "TIMESTAMP", column: "TIMESTAMP_VALUE"},
	} {
		query := "INSERT INTO GO_DIALECT_TYPES (ID, " + test.column + ") VALUES (?, ?)"
		if _, err := db.ExecContext(ctx, query, int64(2), invalidYear); err == nil {
			t.Fatalf("year-zero SQL %s was accepted", test.name)
		}
	}
}

func TestDialectExactNumericRoundTripAndMetadata(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	createDialectTypes(t, db, ctx)
	const numericValue = "12345678901234.5678"
	const decimalValue = "-1234567890123456.78"

	if _, err := db.ExecContext(ctx, `
INSERT INTO GO_DIALECT_TYPES (ID, NUMERIC_VALUE, DECIMAL_VALUE)
VALUES (?, ?, ?)`, int64(1), numericValue, decimalValue); err != nil {
		t.Fatalf("direct exact Dialect 3 numeric insert: %v", err)
	}

	stmt, err := db.PrepareContext(ctx, `
INSERT INTO GO_DIALECT_TYPES (ID, NUMERIC_VALUE, DECIMAL_VALUE)
VALUES (?, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare exact Dialect 3 numeric insert: %v", err)
	}
	if _, err := stmt.ExecContext(ctx, int64(2), "-12345678901234.5678", "1234567890123456.78"); err != nil {
		_ = stmt.Close()
		t.Fatalf("prepared exact Dialect 3 numeric insert: %v", err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close exact Dialect 3 numeric insert: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
SELECT NUMERIC_VALUE, DECIMAL_VALUE FROM GO_DIALECT_TYPES
WHERE ID IN (?, ?) ORDER BY ID`, int64(1), int64(2))
	if err != nil {
		t.Fatalf("query exact Dialect 3 numeric values: %v", err)
	}
	defer rows.Close()
	want := [][2]string{
		{numericValue, decimalValue},
		{"-12345678901234.5678", "1234567890123456.78"},
	}
	for index := range want {
		if !rows.Next() {
			t.Fatalf("exact Dialect 3 numeric row %d missing: %v", index, rows.Err())
		}
		var numeric, decimal string
		if err := rows.Scan(&numeric, &decimal); err != nil {
			t.Fatalf("scan exact Dialect 3 numeric row %d: %v", index, err)
		}
		if [2]string{numeric, decimal} != want[index] {
			t.Fatalf("exact Dialect 3 numeric row %d = (%q, %q), want (%q, %q)",
				index, numeric, decimal, want[index][0], want[index][1])
		}
	}
	finishReadRows(t, rows)
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_DIALECT_TYPES (ID) VALUES (?)", int64(3)); err != nil {
		t.Fatalf("insert exact Dialect 3 numeric NULL row: %v", err)
	}
	var nullNumeric, nullDecimal sql.NullString
	if err := db.QueryRowContext(ctx, `
SELECT NUMERIC_VALUE, DECIMAL_VALUE FROM GO_DIALECT_TYPES WHERE ID = ?`, int64(3)).
		Scan(&nullNumeric, &nullDecimal); err != nil {
		t.Fatalf("scan exact Dialect 3 numeric NULL row: %v", err)
	}
	if nullNumeric.Valid || nullDecimal.Valid {
		t.Fatalf("exact Dialect 3 numeric NULL row = (%#v, %#v), want both invalid",
			nullNumeric, nullDecimal)
	}

	metadataRows, err := db.QueryContext(ctx, `
SELECT NUMERIC_VALUE, DECIMAL_VALUE FROM GO_DIALECT_TYPES WHERE 1 = 0`)
	if err != nil {
		t.Fatalf("query exact Dialect 3 numeric metadata: %v", err)
	}
	columns, err := metadataRows.ColumnTypes()
	if err != nil {
		_ = metadataRows.Close()
		t.Fatalf("exact Dialect 3 numeric ColumnTypes: %v", err)
	}
	wantMetadata := []struct {
		name      string
		precision int64
		scale     int64
	}{
		{name: "NUMERIC", precision: 18, scale: 4},
		{name: "DECIMAL", precision: 18, scale: 2},
	}
	if len(columns) != len(wantMetadata) {
		_ = metadataRows.Close()
		t.Fatalf("exact Dialect 3 numeric metadata count = %d, want %d",
			len(columns), len(wantMetadata))
	}
	for index, want := range wantMetadata {
		if columns[index].DatabaseTypeName() != want.name {
			t.Errorf("Dialect 3 numeric column %d type = %q, want %q",
				index, columns[index].DatabaseTypeName(), want.name)
		}
		precision, scale, ok := columns[index].DecimalSize()
		if !ok || precision != want.precision || scale != want.scale {
			t.Errorf("Dialect 3 numeric column %d decimal size = (%d, %d, %t), want (%d, %d, true)",
				index, precision, scale, ok, want.precision, want.scale)
		}
		if columns[index].ScanType() != reflect.TypeOf("") {
			t.Errorf("Dialect 3 numeric column %d scan type = %v, want string",
				index, columns[index].ScanType())
		}
	}
	finishReadRows(t, metadataRows)
}

func TestDialectExactNumericErrorsRecover(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	createDialectTypes(t, db, ctx)
	connection, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("pin Dialect 3 connection: %v", err)
	}
	defer connection.Close()

	for _, value := range []string{
		"12345678901234.56789",     // nonzero fraction beyond NUMERIC(18,4).
		"99999999999999999.9999",   // too many declared digits.
		"9223372036854775808.0000", // outside signed native storage.
	} {
		if _, err := connection.ExecContext(ctx,
			"INSERT INTO GO_DIALECT_TYPES (ID, NUMERIC_VALUE) VALUES (?, ?)", int64(1), value); err == nil {
			t.Fatalf("invalid Dialect 3 numeric value %q was accepted", value)
		}
	}
	if _, err := connection.ExecContext(ctx,
		"INSERT INTO GO_DIALECT_TYPES (ID, NUMERIC_VALUE) VALUES (?, ?)", int64(2), "1.230000"); err != nil {
		t.Fatalf("direct exact trailing-zero Dialect 3 numeric value was rejected: %v", err)
	}

	stmt, err := connection.PrepareContext(ctx,
		"INSERT INTO GO_DIALECT_TYPES (ID, NUMERIC_VALUE) VALUES (?, ?)")
	if err != nil {
		t.Fatalf("prepare Dialect 3 numeric recovery statement: %v", err)
	}
	if _, err := stmt.ExecContext(ctx, int64(3), "12345678901234.56789"); err == nil {
		_ = stmt.Close()
		t.Fatal("prepared invalid Dialect 3 numeric value was accepted")
	}
	if _, err := stmt.ExecContext(ctx, int64(4), "1.230000"); err != nil {
		_ = stmt.Close()
		t.Fatalf("prepared exact trailing-zero Dialect 3 numeric value was rejected: %v", err)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close Dialect 3 numeric recovery statement: %v", err)
	}

	var badRows int64
	if err := connection.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM GO_DIALECT_TYPES WHERE ID NOT IN (?, ?)", int64(2), int64(4)).
		Scan(&badRows); err != nil {
		t.Fatalf("count invalid Dialect 3 numeric rows: %v", err)
	}
	if badRows != 0 {
		t.Fatalf("invalid Dialect 3 numeric rows = %d, want 0", badRows)
	}

	rows, err := connection.QueryContext(ctx,
		"SELECT ID, NUMERIC_VALUE FROM GO_DIALECT_TYPES ORDER BY ID")
	if err != nil {
		t.Fatalf("query recovered Dialect 3 numeric values: %v", err)
	}
	defer rows.Close()
	want := map[int64]string{2: "1.2300", 4: "1.2300"}
	count := 0
	for rows.Next() {
		var id int64
		var value string
		if err := rows.Scan(&id, &value); err != nil {
			t.Fatalf("scan recovered Dialect 3 numeric row: %v", err)
		}
		wantValue, ok := want[id]
		if !ok {
			t.Fatalf("unexpected recovered Dialect 3 numeric row ID %d", id)
		}
		if value != wantValue {
			t.Fatalf("recovered Dialect 3 numeric row %d = %q, want %q", id, value, wantValue)
		}
		count++
	}
	if count != len(want) {
		t.Fatalf("recovered Dialect 3 numeric row count = %d, want %d", count, len(want))
	}
	finishReadRows(t, rows)
}
