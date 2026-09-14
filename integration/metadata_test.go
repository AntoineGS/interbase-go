//go:build integration

package integration_test

import (
	"database/sql"
	"reflect"
	"testing"
	"time"
)

type expectedColumnMetadata struct {
	name         string
	databaseType string
	scanType     reflect.Type
	length       int64
	hasLength    bool
	nullable     bool
	hasNullable  bool
	precision    int64
	scale        int64
	hasPrecision bool
}

func requireColumnMetadata(t *testing.T, columns []*sql.ColumnType,
	want []expectedColumnMetadata) {
	t.Helper()
	if len(columns) != len(want) {
		t.Fatalf("column count = %d, want %d", len(columns), len(want))
	}
	for index, expected := range want {
		column := columns[index]
		if got := column.Name(); got != expected.name {
			t.Errorf("column %d name = %q, want %q", index, got, expected.name)
		}
		if got := column.DatabaseTypeName(); got != expected.databaseType {
			t.Errorf("column %d database type = %q, want %q", index, got, expected.databaseType)
		}
		if got := column.ScanType(); got != expected.scanType {
			t.Errorf("column %d scan type = %v, want %v", index, got, expected.scanType)
		}
		if got, ok := column.Length(); got != expected.length || ok != expected.hasLength {
			t.Errorf("column %d length = (%d, %t), want (%d, %t)",
				index, got, ok, expected.length, expected.hasLength)
		}
		if got, ok := column.Nullable(); got != expected.nullable || ok != expected.hasNullable {
			t.Errorf("column %d nullable = (%t, %t), want (%t, %t)",
				index, got, ok, expected.nullable, expected.hasNullable)
		}
		if gotPrecision, gotScale, ok := column.DecimalSize(); gotPrecision != expected.precision || gotScale != expected.scale || ok != expected.hasPrecision {
			t.Errorf("column %d decimal size = (%d, %d, %t), want (%d, %d, %t)",
				index, gotPrecision, gotScale, ok,
				expected.precision, expected.scale, expected.hasPrecision)
		}
	}
}

func tableColumnMetadata() []expectedColumnMetadata {
	return []expectedColumnMetadata{
		{
			name: "ID", databaseType: "INTEGER", scanType: reflect.TypeOf(int64(0)),
			nullable: false, hasNullable: true,
		},
		{
			name: "FIXED_VALUE", databaseType: "CHAR", scanType: reflect.TypeOf(""),
			length: 5, hasLength: true, nullable: true, hasNullable: true,
		},
		{
			name: "TEXT_VALUE", databaseType: "VARCHAR", scanType: reflect.TypeOf(""),
			length: 100, hasLength: true, nullable: true, hasNullable: true,
		},
		{
			name: "FIXED_DECIMAL", databaseType: "NUMERIC", scanType: reflect.TypeOf(""),
			nullable: true, hasNullable: true, precision: 9, scale: 2, hasPrecision: true,
		},
		{
			name: "ACTIVE_VALUE", databaseType: "BOOLEAN", scanType: reflect.TypeOf(false),
			nullable: true, hasNullable: true,
		},
		{
			name: "MOMENT", databaseType: "TIMESTAMP", scanType: reflect.TypeOf(time.Time{}),
			nullable: true, hasNullable: true,
		},
		{
			name: "TEXT_BLOB", databaseType: "BLOB", scanType: reflect.TypeOf(""),
			nullable: true, hasNullable: true,
		},
		{
			name: "BINARY_BLOB", databaseType: "BLOB", scanType: reflect.TypeOf([]byte(nil)),
			nullable: true, hasNullable: true,
		},
	}
}

func TestMetadataDirectRowsDescribeTableColumnsBeforeRows(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx, `
SELECT ID, FIXED_VALUE, TEXT_VALUE, FIXED_DECIMAL,
       ACTIVE_VALUE, MOMENT, TEXT_BLOB, BINARY_BLOB
FROM GO_DATA WHERE ID = ?`, int64(1))
	if err != nil {
		t.Fatalf("metadata query: %v", err)
	}
	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("direct ColumnTypes: %v", err)
	}
	requireColumnMetadata(t, columns, tableColumnMetadata())
	if !rows.Next() {
		t.Fatalf("metadata query returned no row: %v", rows.Err())
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("close direct metadata rows: %v", err)
	}
}

func TestMetadataPreparedRowsPreserveAliasesAndDeclaredProperties(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	stmt, err := db.PrepareContext(ctx, `
SELECT ID AS IDENTIFIER, FIXED_VALUE AS FIXED_ALIAS,
       TEXT_VALUE AS TEXT_ALIAS, FIXED_DECIMAL AS DECIMAL_ALIAS
FROM GO_DATA WHERE ID = ?`)
	if err != nil {
		t.Fatalf("prepare metadata query: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })

	rows, err := stmt.QueryContext(ctx, int64(1))
	if err != nil {
		t.Fatalf("prepared metadata query: %v", err)
	}
	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("prepared ColumnTypes: %v", err)
	}
	want := tableColumnMetadata()[:4]
	want[0].name = "IDENTIFIER"
	want[1].name = "FIXED_ALIAS"
	want[2].name = "TEXT_ALIAS"
	want[3].name = "DECIMAL_ALIAS"
	requireColumnMetadata(t, columns, want)
	if !rows.Next() {
		t.Fatalf("prepared metadata query returned no row: %v", rows.Err())
	}
	finishReadRows(t, rows)
}

func TestMetadataEmptyDirectAndPreparedResultsRemainDescribed(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	query := `
SELECT ID, TEXT_VALUE, FIXED_DECIMAL, ACTIVE_VALUE, MOMENT
FROM GO_DATA WHERE 1 = 0`

	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("empty direct metadata query: %v", err)
	}
	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("empty direct ColumnTypes: %v", err)
	}
	want := tableColumnMetadata()
	requireColumnMetadata(t, columns, []expectedColumnMetadata{want[0], want[2], want[3], want[4], want[5]})
	finishReadRows(t, rows)

	stmt, err := db.PrepareContext(ctx, query)
	if err != nil {
		t.Fatalf("prepare empty metadata query: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })
	rows, err = stmt.QueryContext(ctx)
	if err != nil {
		t.Fatalf("empty prepared metadata query: %v", err)
	}
	columns, err = rows.ColumnTypes()
	if err != nil {
		t.Fatalf("empty prepared ColumnTypes: %v", err)
	}
	requireColumnMetadata(t, columns, []expectedColumnMetadata{want[0], want[2], want[3], want[4], want[5]})
	finishReadRows(t, rows)
}

func TestMetadataExpressionsReportUnknownPrecisionAndNullability(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx, `
SELECT ID + 1 AS NEXT_ID, TEXT_VALUE || 'x' AS NEXT_TEXT
FROM GO_DATA WHERE 1 = 0`)
	if err != nil {
		t.Fatalf("expression metadata query: %v", err)
	}
	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("expression ColumnTypes: %v", err)
	}
	if len(columns) != 2 {
		t.Fatalf("expression column count = %d, want 2", len(columns))
	}
	if columns[0].Name() != "NEXT_ID" || columns[1].Name() != "NEXT_TEXT" {
		t.Fatalf("expression names = (%q, %q), want (NEXT_ID, NEXT_TEXT)",
			columns[0].Name(), columns[1].Name())
	}
	for index, column := range columns {
		if _, _, ok := column.DecimalSize(); ok {
			t.Errorf("expression column %d fabricated decimal precision", index)
		}
		if _, ok := column.Nullable(); ok {
			t.Errorf("expression column %d fabricated nullability", index)
		}
	}
	finishReadRows(t, rows)
}

func TestMetadataDoesNotPromiseNotNullForOuterJoinResults(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	rows, err := db.QueryContext(ctx, `
SELECT country.ID AS COUNTRY_ID
FROM GO_DATA data
LEFT JOIN GO_COUNTRY country ON country.ID = 999
WHERE data.ID = 1`)
	if err != nil {
		t.Fatalf("outer-join metadata query: %v", err)
	}
	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("outer-join ColumnTypes: %v", err)
	}
	if len(columns) != 1 {
		t.Fatalf("outer-join column count = %d, want 1", len(columns))
	}
	if nullable, known := columns[0].Nullable(); known && !nullable {
		t.Fatalf("outer-join nullable = (%t, %t), must not promise NOT NULL", nullable, known)
	}
	if !rows.Next() {
		t.Fatalf("outer-join query returned no row: %v", rows.Err())
	}
	var value sql.NullInt64
	if err := rows.Scan(&value); err != nil {
		t.Fatalf("scan outer-join NULL: %v", err)
	}
	if value.Valid {
		t.Fatalf("outer-join value = %v, want NULL", value.Int64)
	}
	finishReadRows(t, rows)
}

func TestMetadataDistinguishesNumericAndDecimalSubtypes(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	if _, err := db.ExecContext(ctx, `
CREATE TABLE GO_NUMERIC_KINDS (
    NUMERIC_VALUE NUMERIC(9,2),
    DECIMAL_VALUE DECIMAL(9,2))`); err != nil {
		t.Fatalf("create numeric subtype table: %v", err)
	}

	rows, err := db.QueryContext(ctx, `
SELECT NUMERIC_VALUE, DECIMAL_VALUE
FROM GO_NUMERIC_KINDS WHERE 1 = 0`)
	if err != nil {
		t.Fatalf("numeric subtype metadata query: %v", err)
	}
	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("numeric subtype ColumnTypes: %v", err)
	}
	requireColumnMetadata(t, columns, []expectedColumnMetadata{
		{
			name: "NUMERIC_VALUE", databaseType: "NUMERIC", scanType: reflect.TypeOf(""),
			nullable: true, hasNullable: true, precision: 9, scale: 2, hasPrecision: true,
		},
		{
			name: "DECIMAL_VALUE", databaseType: "DECIMAL", scanType: reflect.TypeOf(""),
			nullable: true, hasNullable: true, precision: 9, scale: 2, hasPrecision: true,
		},
	})
	finishReadRows(t, rows)
}

func requireZeroScaleNumericMetadata(t *testing.T, rows *sql.Rows) {
	t.Helper()

	columns, err := rows.ColumnTypes()
	if err != nil {
		t.Fatalf("zero-scale numeric ColumnTypes: %v", err)
	}
	wantTypes := []string{"NUMERIC", "DECIMAL"}
	if len(columns) != len(wantTypes) {
		t.Fatalf("zero-scale numeric column count = %d, want %d", len(columns), len(wantTypes))
	}
	for index, wantType := range wantTypes {
		if columns[index].DatabaseTypeName() != wantType {
			t.Errorf("zero-scale numeric column %d type = %q, want %q",
				index, columns[index].DatabaseTypeName(), wantType)
		}
		if columns[index].ScanType() != reflect.TypeOf(int64(0)) {
			t.Errorf("zero-scale numeric column %d scan type = %v, want int64",
				index, columns[index].ScanType())
		}
		if _, _, ok := columns[index].DecimalSize(); ok {
			t.Errorf("zero-scale numeric column %d fabricated precision", index)
		}
	}
	if !rows.Next() {
		t.Fatalf("zero-scale numeric row missing: %v", rows.Err())
	}
	var numeric, decimal int64
	if err := rows.Scan(&numeric, &decimal); err != nil {
		t.Fatalf("scan zero-scale numeric row: %v", err)
	}
	if numeric != 1 || decimal != 2 {
		t.Fatalf("zero-scale numeric row = (%d, %d), want (1, 2)", numeric, decimal)
	}
	finishReadRows(t, rows)
}

func TestMetadataZeroScaleNumericSubtypesForExpressionsAndProcedures(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	const expression = `
SELECT CAST(1 AS NUMERIC(18,0)) AS NUMERIC_ZERO,
       CAST(2 AS DECIMAL(18,0)) AS DECIMAL_ZERO
FROM RDB$DATABASE`

	rows, err := db.QueryContext(ctx, expression)
	if err != nil {
		t.Fatalf("direct zero-scale numeric expression: %v", err)
	}
	requireZeroScaleNumericMetadata(t, rows)

	stmt, err := db.PrepareContext(ctx, expression)
	if err != nil {
		t.Fatalf("prepare zero-scale numeric expression: %v", err)
	}
	t.Cleanup(func() { _ = stmt.Close() })
	rows, err = stmt.QueryContext(ctx)
	if err != nil {
		t.Fatalf("prepared zero-scale numeric expression: %v", err)
	}
	requireZeroScaleNumericMetadata(t, rows)

	if _, err := db.ExecContext(ctx, `
CREATE PROCEDURE GO_ZERO_SCALE_NUMERIC
RETURNS (NUMERIC_ZERO NUMERIC(18,0), DECIMAL_ZERO DECIMAL(18,0))
AS
BEGIN
  NUMERIC_ZERO = 1;
  DECIMAL_ZERO = 2;
  SUSPEND;
END`); err != nil {
		t.Fatalf("create zero-scale numeric procedure: %v", err)
	}

	rows, err = db.QueryContext(ctx, "EXECUTE PROCEDURE GO_ZERO_SCALE_NUMERIC")
	if err != nil {
		t.Fatalf("direct zero-scale numeric procedure: %v", err)
	}
	requireZeroScaleNumericMetadata(t, rows)

	procedureStmt, err := db.PrepareContext(ctx, "EXECUTE PROCEDURE GO_ZERO_SCALE_NUMERIC")
	if err != nil {
		t.Fatalf("prepare zero-scale numeric procedure: %v", err)
	}
	t.Cleanup(func() { _ = procedureStmt.Close() })
	rows, err = procedureStmt.QueryContext(ctx)
	if err != nil {
		t.Fatalf("prepared zero-scale numeric procedure: %v", err)
	}
	requireZeroScaleNumericMetadata(t, rows)
}
