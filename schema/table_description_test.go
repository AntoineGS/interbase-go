package schema

import (
	"database/sql"
	"errors"
	"strings"
	"testing"
)

func TestRelationDescribeCatalog(t *testing.T) {
	legacyDomain := &Domain{
		Name:         "RDB$16082",
		FieldType:    sql.NullInt64{Int64: fieldTypeDouble, Valid: true},
		FieldScale:   sql.NullInt64{Int64: -2, Valid: true},
		FieldSubType: sql.NullInt64{Int64: 0, Valid: true},
	}
	actualNumericDomain := &Domain{
		Name:           "RECORDED_NUMERIC",
		FieldType:      sql.NullInt64{Int64: fieldTypeDouble, Valid: true},
		FieldScale:     sql.NullInt64{Int64: -2, Valid: true},
		FieldSubType:   sql.NullInt64{Int64: 1, Valid: true},
		FieldPrecision: sql.NullInt64{Int64: 15, Valid: true},
	}
	unknownDomain := &Domain{
		Name:      "FUTURE_TYPE",
		FieldType: sql.NullInt64{Int64: 999, Valid: true},
	}
	relation := Relation{
		Name: "IMPORT_ORDER_PAYMENT",
		Kind: RelationTable,
		Columns: []Column{
			{Name: "AMOUNT", Domain: legacyDomain},
			{Name: "RECORDED_AMOUNT", Domain: actualNumericDomain},
			{Name: "FUTURE_VALUE", Domain: unknownDomain},
		},
	}

	got, err := relation.DescribeCatalog()
	if err != nil {
		t.Fatalf("DescribeCatalog returned error: %v", err)
	}
	if got.Body[got.Table.Start:got.Table.End] != `"IMPORT_ORDER_PAYMENT"` {
		t.Fatalf("table span = %q, want exact quoted table name", got.Body[got.Table.Start:got.Table.End])
	}
	if len(got.Columns) != len(relation.Columns) {
		t.Fatalf("description has %d columns, want %d", len(got.Columns), len(relation.Columns))
	}
	for index, want := range []string{`"AMOUNT"`, `"RECORDED_AMOUNT"`, `"FUTURE_VALUE"`} {
		column := got.Columns[index]
		if got.Body[column.Span.Start:column.Span.End] != want {
			t.Errorf("column %d span = %q, want %q", index, got.Body[column.Span.Start:column.Span.End], want)
		}
	}
	for _, want := range []string{
		"CREATE TABLE \"IMPORT_ORDER_PAYMENT\"",
		"normalized legacy scaled DOUBLE catalog type",
		"NUMERIC(15, 2)",
		"type=27, scale=-2, subtype=0, precision=NULL",
		"unknown field type 999",
		"type=999, scale=NULL, subtype=NULL, precision=NULL",
		"constraints unknown",
	} {
		if !strings.Contains(got.Body, want) {
			t.Errorf("description body does not contain %q:\n%s", want, got.Body)
		}
	}
	unknownDescription := got.Body[got.Columns[2].Span.End:]
	if strings.Contains(unknownDescription, "--   Type label: ") {
		t.Errorf("unknown field type was rendered as a SQL type:\n%s", unknownDescription)
	}

	label, normalized := legacyDomain.CatalogTypeLabel()
	if label != "NUMERIC(15, 2)" || !normalized {
		t.Errorf("legacy CatalogTypeLabel() = (%q, %t), want (%q, true)", label, normalized, "NUMERIC(15, 2)")
	}
	label, normalized = actualNumericDomain.CatalogTypeLabel()
	if label != "NUMERIC(15, 2)" || normalized {
		t.Errorf("recorded CatalogTypeLabel() = (%q, %t), want (%q, false)", label, normalized, "NUMERIC(15, 2)")
	}
	label, normalized = unknownDomain.CatalogTypeLabel()
	if label != "" || normalized {
		t.Errorf("unknown CatalogTypeLabel() = (%q, %t), want empty label and false", label, normalized)
	}

	relation.ConstraintsLoaded = true
	if _, err := relation.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Errorf("legacy relation GenerateDDL error = %v, want ErrUnsupportedDDL", err)
	}
}

func TestRelationDescribeCatalogRejectsIncompleteOrAmbiguousIdentity(t *testing.T) {
	for _, test := range []struct {
		name     string
		relation Relation
	}{
		{
			name: "empty table name",
			relation: Relation{
				Columns: []Column{{Name: "VALUE"}},
			},
		},
		{
			name:     "missing columns",
			relation: Relation{Name: "NO_COLUMNS"},
		},
		{
			name: "duplicate exact column identity",
			relation: Relation{
				Name: "DUPLICATE_COLUMNS",
				Columns: []Column{
					{Name: "VALUE"},
					{Name: "VALUE"},
				},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.relation.DescribeCatalog(); err == nil {
				t.Fatal("DescribeCatalog returned no error for invalid relation identity")
			}
		})
	}
}

func TestRelationDescribeCatalogSpansUseByteOffsets(t *testing.T) {
	relation := Relation{
		Name: "TABLE_é",
		Columns: []Column{
			{Name: "列"},
		},
	}

	got, err := relation.DescribeCatalog()
	if err != nil {
		t.Fatalf("DescribeCatalog returned error: %v", err)
	}
	if value := got.Body[got.Table.Start:got.Table.End]; value != `"TABLE_é"` {
		t.Fatalf("multibyte table span = %q, want exact quoted table name", value)
	}
	if value := got.Body[got.Columns[0].Span.Start:got.Columns[0].Span.End]; value != `"列"` {
		t.Fatalf("multibyte column span = %q, want exact quoted column name", value)
	}
}

func TestRelationDescribeCatalogUsesOptionsAndKeepsSQLShapedKnownFacets(t *testing.T) {
	relation := Relation{Name: "TABLE_NAME", Kind: RelationTable, Columns: []Column{
		{Name: "AMOUNT", Domain: &Domain{FieldType: sql.NullInt64{Int64: fieldTypeDouble, Valid: true}, FieldScale: sql.NullInt64{Int64: -2, Valid: true}}, DefaultSource: sql.NullString{String: "DEFAULT  1.25", Valid: true}, Nullable: sql.NullBool{Bool: false, Valid: true}},
	}}
	got, err := relation.DescribeCatalogWithOptions(DDLOptions{Dialect: Dialect1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Body[got.Table.Start:got.Table.End] != "TABLE_NAME" {
		t.Fatalf("Dialect 1 table span=%q", got.Body[got.Table.Start:got.Table.End])
	}
	if got.Body[got.Columns[0].Span.Start:got.Columns[0].Span.End] != "AMOUNT" {
		t.Fatalf("Dialect 1 column span=%q", got.Body[got.Columns[0].Span.Start:got.Columns[0].Span.End])
	}
	for _, fragment := range []string{"CREATE TABLE TABLE_NAME", "AMOUNT NUMERIC(15, 2)", "DEFAULT  1.25", "NOT NULL", "legacy scaled DOUBLE", "constraints unknown"} {
		if !strings.Contains(got.Body, fragment) {
			t.Errorf("description missing %q:\n%s", fragment, got.Body)
		}
	}
	if strings.Contains(got.Body, `"TABLE_NAME"`) {
		t.Fatalf("Dialect 1 description quoted identifier:\n%s", got.Body)
	}
	relation.ConstraintsLoaded = false
	if _, err := relation.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("incomplete table executable DDL error=%v; strict completeness must remain", err)
	}
}

func TestRelationDescribeCatalogKeepsNamedDomainsChecksOverridesAndRelationKinds(t *testing.T) {
	domain := &Domain{Name: "GO_TEXT_DOMAIN", SystemFlag: sql.NullInt64{Int64: 0, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeVarchar, Valid: true}, CharacterLength: sql.NullInt64{Int64: 20, Valid: true}, CharacterSetName: sql.NullString{String: "UTF8", Valid: true}, CollationID: sql.NullInt64{Int64: 1, Valid: true}, CollationName: sql.NullString{String: "UTF8", Valid: true}, DefaultSource: sql.NullString{String: "DEFAULT 'domain default'", Valid: true}, ValidationSource: sql.NullString{String: "CHECK (VALUE <> '')", Valid: true}}
	relation := Relation{Name: "GO_TEMP_TABLE", Kind: RelationTable, RelationType: sql.NullString{String: "GLOBAL_TEMPORARY_PRESERVE_ROWS", Valid: true}, ConstraintsLoaded: true, Columns: []Column{
		{Name: "TEXT_VALUE", FieldSource: sql.NullString{String: "GO_TEXT_DOMAIN", Valid: true}, Domain: domain, DefaultSource: sql.NullString{String: "DEFAULT 'column default'", Valid: true}, CollationID: sql.NullInt64{Int64: 2, Valid: true}, CollationName: sql.NullString{String: "UNICODE_CI", Valid: true}},
		{Name: "INHERITED_VALUE", FieldSource: sql.NullString{String: "GO_TEXT_DOMAIN", Valid: true}, Domain: domain},
	}, Constraints: []Constraint{{Name: "GO_TEMP_VALUE_NN", ConstraintType: string(ConstraintNotNull), ColumnName: sql.NullString{String: "TEXT_VALUE", Valid: true}}}}
	got, err := relation.DescribeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"CREATE GLOBAL TEMPORARY TABLE", `"TEXT_VALUE" "GO_TEXT_DOMAIN"`, "DEFAULT 'column default'", "CONSTRAINT \"GO_TEMP_VALUE_NN\" NOT NULL", "COLLATE \"UNICODE_CI\"", `/* inherited domain check: CHECK (VALUE <> '') */`, `"INHERITED_VALUE" "GO_TEXT_DOMAIN" /* inherited domain check: CHECK (VALUE <> '') */`, "/* inherited domain default: DEFAULT 'domain default' */", "ON COMMIT PRESERVE ROWS"} {
		if !strings.Contains(got.Body, fragment) {
			t.Errorf("description missing known facet %q:\n%s", fragment, got.Body)
		}
	}
	for _, name := range []string{"TEXT_VALUE", "INHERITED_VALUE"} {
		line := descriptionLineForColumn(t, got.Body, `"`+name+`"`)
		sqlPart := line
		if commentStart := strings.Index(sqlPart, "/*"); commentStart >= 0 {
			sqlPart = sqlPart[:commentStart]
		}
		if strings.Contains(sqlPart, "CHECK (VALUE") {
			t.Errorf("named domain CHECK escaped informational comment for %s: %s", name, line)
		}
	}
	if strings.Contains(got.Body, `"TEXT_VALUE" "GO_TEXT_DOMAIN" DEFAULT 'column default' /* inherited domain`) {
		t.Errorf("column default did not override the referenced domain default:\n%s", got.Body)
	}

	external := Relation{Name: "GO_EXTERNAL_TABLE", Kind: RelationTable, RelationType: sql.NullString{String: "EXTERNAL", Valid: true}, ExternalFile: sql.NullString{String: "orders.dat", Valid: true}, Columns: []Column{{Name: "ID", Domain: &Domain{FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}, ExternalLength: sql.NullInt64{Int64: 0, Valid: true}, ExternalScale: sql.NullInt64{Int64: 0, Valid: true}, ExternalType: sql.NullInt64{Int64: 0, Valid: true}}}}}
	externalDescription, err := external.DescribeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"EXTERNAL FILE 'orders.dat'", `"ID" INTEGER`} {
		if !strings.Contains(externalDescription.Body, fragment) {
			t.Errorf("external description missing %q:\n%s", fragment, externalDescription.Body)
		}
	}

	view := Relation{Name: "GO_VIEW", Kind: RelationView, ViewSource: sql.NullString{String: "SELECT 1 AS VALUE FROM RDB$DATABASE", Valid: true}, Columns: []Column{{Name: "VALUE"}}}
	viewDescription, err := view.DescribeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(viewDescription.Body, "CREATE VIEW \"GO_VIEW\"") || !strings.HasSuffix(viewDescription.Body, view.ViewSource.String) {
		t.Fatalf("view description lost relation kind/source:\n%s", viewDescription.Body)
	}
}

func TestRelationDescribeCatalogAnnotatesUnrenderableInheritedClauses(t *testing.T) {
	validNamedDomain := &Domain{Name: "GO_VALID_DOMAIN", SystemFlag: sql.NullInt64{Int64: 0, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}, DefaultSource: sql.NullString{Valid: true}, ValidationSource: sql.NullString{Valid: true}}
	fallbackNamedDomain := &Domain{Name: "GO_FALLBACK_DOMAIN", SystemFlag: sql.NullInt64{Int64: 0, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeVarchar, Valid: true}, CharacterLength: sql.NullInt64{Int64: 12, Valid: true}, DefaultSource: sql.NullString{Valid: true}, ValidationSource: sql.NullString{Valid: true}}
	relation := Relation{Name: "GO_BAD_INHERITED_CLAUSES", Columns: []Column{
		{Name: "VALID_PATH", FieldSource: sql.NullString{String: "GO_VALID_DOMAIN", Valid: true}, Domain: validNamedDomain},
		{Name: "FALLBACK_PATH", FieldSource: sql.NullString{String: "GO_FALLBACK_DOMAIN", Valid: true}, Domain: fallbackNamedDomain, CollationID: sql.NullInt64{Int64: 1, Valid: true}},
	}}
	got, err := relation.DescribeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"VALID_PATH", "FALLBACK_PATH"} {
		line := descriptionLineForColumn(t, got.Body, `"`+column+`"`)
		for _, facet := range []string{"inherited domain default unknown:", "inherited domain check unknown:"} {
			if !strings.Contains(line, facet) {
				t.Errorf("column %s missing %q:\n%s", column, facet, line)
			}
		}
	}
}

func TestRelationDescribeCatalogEscapesInheritedSourceComments(t *testing.T) {
	domain := &Domain{Name: "GO_COMMENT_DOMAIN", SystemFlag: sql.NullInt64{Int64: 0, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}, DefaultSource: sql.NullString{String: "DEFAULT 'default */\n continued'", Valid: true}, ValidationSource: sql.NullString{String: "CHECK (VALUE <> '*/\n continued')", Valid: true}}
	relation := Relation{Name: "GO_COMMENT_TABLE", Columns: []Column{{Name: "VALUE", FieldSource: sql.NullString{String: domain.Name, Valid: true}, Domain: domain}}}
	got, err := relation.DescribeCatalog()
	if err != nil {
		t.Fatal(err)
	}
	line := descriptionLineForColumn(t, got.Body, `"VALUE"`)
	if !strings.Contains(line, `DEFAULT 'default * /  continued'`) || !strings.Contains(line, `CHECK (VALUE <> '* /  continued')`) {
		t.Fatalf("inherited source comments were not escaped/single-line:\n%s", line)
	}
	if strings.Count(line, "/*") != strings.Count(line, "*/") {
		t.Fatalf("unterminated comment due to catalog source text:\n%s", line)
	}
}

func descriptionLineForColumn(t *testing.T, body, column string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if strings.Contains(line, column) {
			return line
		}
	}
	t.Fatalf("column %s not found in description:\n%s", column, body)
	return ""
}
