//go:build integration

package integration_test

import "testing"

func TestReadRootCHARExpressionsPreserveLogicalCharacterLength(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	tests := []struct {
		name  string
		query string
		want  string
	}{
		{
			name:  "short ASCII",
			query: "SELECT CAST('x' AS CHAR(3)) FROM RDB$DATABASE",
			want:  "x  ",
		},
		{
			name:  "trailing spaces",
			query: "SELECT CAST('x  ' AS CHAR(5)) FROM RDB$DATABASE",
			want:  "x    ",
		},
		{
			name:  "all spaces",
			query: "SELECT CAST('    ' AS CHAR(5)) FROM RDB$DATABASE",
			want:  "     ",
		},
		{
			name:  "multibyte UTF8",
			query: "SELECT CAST('ě' AS CHAR(5) CHARACTER SET UTF8) FROM RDB$DATABASE",
			want:  "ě    ",
		},
		{
			name:  "character type",
			query: "SELECT CAST('x' AS CHARACTER(3)) FROM RDB$DATABASE",
			want:  "x  ",
		},
		{
			name:  "parenthesized cast",
			query: "SELECT (CAST('x' AS CHAR(3))) FROM RDB$DATABASE",
			want:  "x  ",
		},
		{
			name:  "aliased cast",
			query: "SELECT CAST('x' AS CHAR(3)) AS RESULT FROM RDB$DATABASE",
			want:  "x  ",
		},
		{
			name:  "untyped trailing spaces",
			query: "SELECT 'x  ' FROM RDB$DATABASE",
			want:  "x  ",
		},
		{
			name:  "untyped literal",
			query: "SELECT 'x' FROM RDB$DATABASE",
			want:  "x",
		},
		{
			name:  "four-byte untyped literal",
			query: "SELECT 'test' FROM RDB$DATABASE",
			want:  "test",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var got string
			if err := db.QueryRowContext(ctx, test.query).Scan(&got); err != nil {
				t.Fatalf("query root CHAR expression: %v", err)
			}
			if got != test.want {
				t.Fatalf("root CHAR result = %q (length %d), want %q (length %d)",
					got, len(got), test.want, len(test.want))
			}
		})
	}
}

func TestReadPreparedRootCHARExpressionPreservesLogicalCharacterLength(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	stmt, err := db.PrepareContext(ctx,
		"SELECT CAST(? AS CHAR(3)) FROM RDB$DATABASE")
	if err != nil {
		t.Fatalf("prepare root CHAR expression: %v", err)
	}
	defer stmt.Close()

	var got string
	if err := stmt.QueryRowContext(ctx, "x").Scan(&got); err != nil {
		t.Fatalf("query prepared root CHAR expression: %v", err)
	}
	if got != "x  " {
		t.Fatalf("prepared root CHAR result = %q (length %d), want %q (length 3)",
			got, len(got), "x  ")
	}
}

func TestReadRootCHARExpressionLengthsAreTrackedPerColumn(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)

	var fixed, literal string
	if err := db.QueryRowContext(ctx,
		"SELECT CAST('x' AS CHAR(3)), 'y  ' FROM RDB$DATABASE").Scan(&fixed, &literal); err != nil {
		t.Fatalf("query mixed root text expressions: %v", err)
	}
	if fixed != "x  " || literal != "y  " {
		t.Fatalf("mixed root text expressions = (%q, %q), want (%q, %q)",
			fixed, literal, "x  ", "y  ")
	}
}

func TestReadRootCHARExpressionsDoNotTruncateAmbiguousResults(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)

	tests := []struct {
		name  string
		query string
		want  [][]string
	}{
		{
			name:  "concatenation",
			query: "SELECT CAST('x' AS CHAR(3)) || 'abcdefghijkl' FROM RDB$DATABASE",
			want:  [][]string{{"x  abcdefghijkl"}},
		},
		{
			name:  "nested function inside cast",
			query: "SELECT CAST(UPPER('x') AS CHAR(3)) || 'abcdefghijkl' FROM RDB$DATABASE",
			want:  [][]string{{"X  abcdefghijkl"}},
		},
		{
			name:  "comments",
			query: "SELECT CAST('x' AS CHAR(3)) /* not the end */ || 'abcdefghijkl' FROM RDB$DATABASE",
			want:  [][]string{{"x  abcdefghijkl"}},
		},
		{
			name: "cte",
			query: `WITH CTE AS (
				SELECT 'abcdefghijkl' AS LONG_TEXT FROM RDB$DATABASE
			) SELECT CAST('x' AS CHAR(3)) || CTE.LONG_TEXT FROM CTE`,
			want: [][]string{{"x  abcdefghijkl"}},
		},
		{
			name: "derived star",
			query: `SELECT CAST('x' AS CHAR(3)) || 'abcdefghijkl', D.*
			FROM (SELECT 'mnopqrst' AS DERIVED_TEXT, 'uvwxyz' AS SECOND_TEXT
			      FROM RDB$DATABASE) D`,
			want: [][]string{{"x  abcdefghijkl", "mnopqrst", "uvwxyz"}},
		},
		{
			name: "union arms",
			query: `SELECT CAST((CAST('x' AS CHAR(3)) || 'abcdefghijkl') AS VARCHAR(20))
			FROM RDB$DATABASE
			UNION ALL
			SELECT CAST('mnopqrstuv' AS VARCHAR(20)) FROM RDB$DATABASE`,
			want: [][]string{{"x  abcdefghijkl"}, {"mnopqrstuv"}},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			rows, err := db.QueryContext(ctx, test.query)
			if err != nil {
				t.Fatalf("query ambiguous root CHAR expression: %v", err)
			}
			defer rows.Close()
			for rowIndex, expected := range test.want {
				if !rows.Next() {
					t.Fatalf("row %d missing: %v", rowIndex, rows.Err())
				}
				values := make([]any, len(expected))
				pointers := make([]any, len(expected))
				for index := range pointers {
					pointers[index] = &values[index]
				}
				if err := rows.Scan(pointers...); err != nil {
					t.Fatalf("scan row %d: %v", rowIndex, err)
				}
				for index, expectedValue := range expected {
					got, ok := values[index].(string)
					if !ok || got != expectedValue {
						t.Fatalf("row %d column %d = %#v, want %q", rowIndex, index,
							values[index], expectedValue)
					}
				}
			}
			if rows.Next() {
				t.Fatal("query returned more rows than expected")
			}
			if err := rows.Err(); err != nil {
				t.Fatalf("iterate rows: %v", err)
			}
		})
	}
}

func TestReadRootCHARExpressionWithQuotedAliasKeepsCompleteValue(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)

	var got string
	if err := db.QueryRowContext(ctx,
		`SELECT /* leading */ CAST('x' AS CHAR(3)) /* alias follows */ AS "R""ESULT"
		 FROM "RDB$DATABASE"`).Scan(&got); err != nil {
		t.Fatalf("query quoted root CHAR alias: %v", err)
	}
	if got != "x  " {
		t.Fatalf("quoted alias root CHAR result = %q (length %d), want %q (length 3)",
			got, len(got), "x  ")
	}
}
