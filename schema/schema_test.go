package schema

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"sync"
	"testing"
)

var (
	_ Queryer = (*sql.DB)(nil)
	_ Queryer = (*sql.Conn)(nil)
	_ Queryer = (*sql.Tx)(nil)
)

func TestTablesLoadOrderedColumnsAndDomainMetadata(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()

	catalog := New(db)
	got, err := catalog.Tables(context.Background(), "ORDERS")
	if err != nil {
		t.Fatalf("Tables returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("Tables returned %d relations, want 1", len(got))
	}
	if got[0].Name != "ORDERS" {
		t.Fatalf("table name = %q, want ORDERS", got[0].Name)
	}
	if got[0].Kind != RelationTable {
		t.Fatalf("table kind = %q, want %q", got[0].Kind, RelationTable)
	}
	if len(got[0].Columns) != 2 {
		t.Fatalf("column count = %d, want 2", len(got[0].Columns))
	}
	if names := []string{got[0].Columns[0].Name, got[0].Columns[1].Name}; !reflect.DeepEqual(names, []string{"ID", "TOTAL"}) {
		t.Fatalf("column names = %#v, want [ID TOTAL]", names)
	}
	if got[0].Columns[1].DefaultSource.String != "DEFAULT  42  " || !got[0].Columns[1].DefaultSource.Valid {
		t.Fatalf("default source = %#v, want valid untrimmed source", got[0].Columns[1].DefaultSource)
	}
	if got[0].Columns[1].Domain == nil {
		t.Fatal("column domain is nil")
	}
	if got[0].Columns[1].Domain.Name != "ORDER_TOTAL" {
		t.Fatalf("domain name = %q, want ORDER_TOTAL", got[0].Columns[1].Domain.Name)
	}
	if got[0].Columns[1].Domain.FieldType.Int64 != 8 || !got[0].Columns[1].Domain.FieldType.Valid {
		t.Fatalf("domain field type = %#v, want valid INTEGER code", got[0].Columns[1].Domain.FieldType)
	}
	if got[0].Columns[0].Nullable.Bool || !got[0].Columns[0].Nullable.Valid {
		t.Fatalf("NOT NULL column nullable = %#v, want valid false", got[0].Columns[0].Nullable)
	}
	if got[0].Columns[0].Description.Valid {
		t.Fatal("NULL column description unexpectedly valid")
	}
}

func TestRelationsAndViewsPreserveMetadataAndReturnCatalogOrder(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	catalog := New(db)

	relations, err := catalog.Relations(context.Background(), "")
	if err != nil {
		t.Fatalf("Relations returned error: %v", err)
	}
	if names := relationNames(relations); !reflect.DeepEqual(names, []string{"AUDIT", "CUSTOMER_VIEW", "ORDERS", "OTHER_VIEW"}) {
		t.Fatalf("relation names = %#v, want catalog order", names)
	}
	if relations[1].Kind != RelationView {
		t.Fatalf("second relation kind = %q, want %q", relations[1].Kind, RelationView)
	}
	if !relations[0].RelationType.Valid || relations[0].RelationType.String != "PERSISTENT" ||
		!relations[1].RelationType.Valid || relations[1].RelationType.String != "VIEW" {
		t.Fatalf("relation types = (%#v, %#v), want padded catalog text", relations[0].RelationType, relations[1].RelationType)
	}

	tables, err := catalog.Tables(context.Background(), "")
	if err != nil {
		t.Fatalf("Tables returned error: %v", err)
	}
	if names := relationNames(tables); !reflect.DeepEqual(names, []string{"AUDIT", "ORDERS"}) {
		t.Fatalf("table names = %#v, want user tables only", names)
	}

	allViews, err := catalog.Views(context.Background(), "")
	if err != nil {
		t.Fatalf("Views returned error: %v", err)
	}
	if names := relationNames(allViews); !reflect.DeepEqual(names, []string{"CUSTOMER_VIEW", "OTHER_VIEW"}) {
		t.Fatalf("view names = %#v, want user views only", names)
	}

	views, err := catalog.Views(context.Background(), "CUSTOMER_VIEW")
	if err != nil {
		t.Fatalf("Views returned error: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("Views returned %d relations, want 1", len(views))
	}
	view := views[0]
	if view.Name != "CUSTOMER_VIEW" || !view.ViewSource.Valid || view.ViewSource.String != "SELECT  ID  FROM ORDERS\n" {
		t.Fatalf("view metadata = (%q, %#v), want padded-name decoding and exact source", view.Name, view.ViewSource)
	}
	if len(view.Columns) != 1 {
		t.Fatalf("view column count = %d, want 1", len(view.Columns))
	}
	column := view.Columns[0]
	if !column.BaseField.Valid || column.BaseField.String != "ID" || !column.BaseRelation.Valid || column.BaseRelation.String != "ORDERS" {
		t.Fatalf("view base column = (%#v, %#v), want trimmed identifiers", column.BaseField, column.BaseRelation)
	}
	if column.Domain == nil || !column.Domain.ComputedSource.Valid || column.Domain.ComputedSource.String != "COMPUTED BY  ID  " {
		t.Fatalf("view domain computed source = %#v, want exact source", column.Domain)
	}
	if column.BaseRelation.String != "ORDERS" {
		t.Fatalf("view base relation = %#v, want relation constrained by view name and context", column.BaseRelation)
	}

	for _, call := range state.callsSnapshot() {
		if fixtureQueryKind(call.query) == "columns" {
			upper := strings.ToUpper(call.query)
			if !strings.Contains(upper, "V.RDB$VIEW_NAME = RF.RDB$RELATION_NAME") ||
				!strings.Contains(upper, "V.RDB$VIEW_CONTEXT = RF.RDB$VIEW_CONTEXT") {
				t.Fatalf("view column query lacks name/context join constraint: %s", call.query)
			}
		}
	}
}

func TestDomainEnumerationExcludesImplicitRDBPrefixButInternalLookupResolves(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)
	ctx := context.Background()

	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		t.Fatalf("Domains returned error: %v", err)
	}
	for _, domain := range domains {
		if strings.HasPrefix(domain.Name, "RDB$") {
			t.Fatalf("user domain enumeration returned implicit domain %q", domain.Name)
		}
	}
	if domains, err := catalog.Domains(ctx, "RDB$IMPLICIT"); err != nil {
		t.Fatalf("implicit domain lookup returned error: %v", err)
	} else if len(domains) != 0 {
		t.Fatalf("implicit domain lookup = %#v, want no user domain", domains)
	}

	domain, err := catalog.domain(ctx, "RDB$IMPLICIT")
	if err != nil {
		t.Fatalf("internal domain lookup returned error: %v", err)
	}
	if domain == nil || domain.Name != "RDB$IMPLICIT" {
		t.Fatalf("internal domain lookup = %#v, want implicit catalog domain", domain)
	}
}

func TestProceduresSeparateOrderedInputAndOutputParameters(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)

	procedures, err := catalog.Procedures(context.Background(), "PROC_TWO")
	if err != nil {
		t.Fatalf("Procedures returned error: %v", err)
	}
	if len(procedures) != 1 {
		t.Fatalf("Procedures returned %d procedures, want 1", len(procedures))
	}
	procedure := procedures[0]
	if procedure.Name != "PROC_TWO" || !procedure.Source.Valid || procedure.Source.String != "BEGIN\n  X =  1;\nEND" {
		t.Fatalf("procedure metadata = (%q, %#v), want exact source", procedure.Name, procedure.Source)
	}
	if len(procedure.InputParameters) != 2 || len(procedure.OutputParameters) != 1 {
		t.Fatalf("procedure parameter counts = (%d, %d), want (2, 1)", len(procedure.InputParameters), len(procedure.OutputParameters))
	}
	if names := parameterNames(procedure.InputParameters); !reflect.DeepEqual(names, []string{"IN_NAME", "IN_COUNT"}) {
		t.Fatalf("input parameter names = %#v, want ordered inputs", names)
	}
	input := procedure.InputParameters[0]
	if input.Direction != ParameterInput || !input.FieldSource.Valid || input.FieldSource.String != "IN_NAME" {
		t.Fatalf("input parameter metadata = %#v, want input and reference-compatible field source", input)
	}
	if input.Domain == nil || input.Domain.Name != "IN_NAME" || input.Domain.FieldType.Int64 != 37 {
		t.Fatalf("input parameter domain = %#v, want VARCHAR domain metadata", input.Domain)
	}
	if input.Nullable.Valid {
		t.Fatalf("nullable input parameter = %#v, want unknown declaration nullability", input.Nullable)
	}
	output := procedure.OutputParameters[0]
	if output.Direction != ParameterOutput || output.Name != "OUT_TOTAL" || output.Domain == nil ||
		!output.Nullable.Valid || output.Nullable.Bool {
		t.Fatalf("output parameter metadata = %#v, want output NOT NULL metadata", output)
	}
	if output.Domain.Nullable.Valid && output.Domain.Nullable.Bool {
		t.Fatalf("output parameter domain nullable = %#v, want non-null domain", output.Domain.Nullable)
	}
}

func TestCatalogProjectionAndOrderingContracts(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	catalog := New(db)

	if _, err := catalog.Tables(context.Background(), "ORDERS"); err != nil {
		t.Fatalf("Tables returned error: %v", err)
	}
	if _, err := catalog.Columns(context.Background(), "ORDERS"); err != nil {
		t.Fatalf("Columns returned error: %v", err)
	}
	if _, err := catalog.Domains(context.Background(), "IN_NAME"); err != nil {
		t.Fatalf("Domains returned error: %v", err)
	}
	if _, err := catalog.Procedures(context.Background(), "PROC_TWO"); err != nil {
		t.Fatalf("Procedures returned error: %v", err)
	}

	calls := state.callsSnapshot()
	relationQuery := fixtureCallQuery(t, calls, "relations")
	relationUpper := strings.ToUpper(relationQuery)
	for _, projection := range []string{
		"CAST(R.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME",
		"CAST(R.RDB$SECURITY_CLASS AS VARCHAR(31)) AS RDB$SECURITY_CLASS",
		"CAST(R.RDB$OWNER_NAME AS VARCHAR(31)) AS RDB$OWNER_NAME",
		"CAST(R.RDB$DEFAULT_CLASS AS VARCHAR(31)) AS RDB$DEFAULT_CLASS",
	} {
		if !strings.Contains(relationUpper, projection) {
			t.Fatalf("relation query = %q, want identifier projection %q", relationQuery, projection)
		}
	}
	if !strings.Contains(relationUpper, "AND R.RDB$RELATION_NAME = ?") || !strings.Contains(relationUpper, "ORDER BY R.RDB$RELATION_NAME") {
		t.Fatalf("relation query ordering = %q, want relation-name ordering", relationQuery)
	}
	if !strings.Contains(relationUpper, "R.RDB$VIEW_BLR IS NULL") {
		t.Fatalf("table query filter = %q, want view-BLR table filter", relationQuery)
	}

	columnQuery := fixtureCallQuery(t, calls, "columns")
	columnUpper := strings.ToUpper(columnQuery)
	for _, projection := range []string{
		"CAST(RF.RDB$FIELD_NAME AS VARCHAR(31)) AS RDB$FIELD_NAME",
		"CAST(RF.RDB$RELATION_NAME AS VARCHAR(31)) AS RDB$RELATION_NAME",
		"CAST(RF.RDB$FIELD_SOURCE AS VARCHAR(31)) AS RDB$FIELD_SOURCE",
		"CAST(RF.RDB$SECURITY_CLASS AS VARCHAR(31)) AS RDB$SECURITY_CLASS",
		"CAST(RF.RDB$BASE_FIELD AS VARCHAR(31)) AS RDB$BASE_FIELD",
		"CAST(V.RDB$RELATION_NAME AS VARCHAR(67)) AS BASE_RELATION",
		"CAST(F.RDB$FIELD_NAME AS VARCHAR(31)) AS DOMAIN_NAME",
		"CAST(CS.RDB$CHARACTER_SET_NAME AS VARCHAR(31)) AS RDB$CHARACTER_SET_NAME",
		"CAST(CO.RDB$COLLATION_NAME AS VARCHAR(31)) AS RDB$COLLATION_NAME",
		"CAST(RCO.RDB$COLLATION_NAME AS VARCHAR(31)) AS COLUMN_COLLATION_NAME",
	} {
		if !strings.Contains(columnUpper, projection) {
			t.Fatalf("column query = %q, want identifier projection %q", columnQuery, projection)
		}
	}
	for _, clause := range []string{
		"V.RDB$VIEW_NAME = RF.RDB$RELATION_NAME",
		"V.RDB$VIEW_CONTEXT = RF.RDB$VIEW_CONTEXT",
		"WHERE RF.RDB$RELATION_NAME = ?",
		"ORDER BY RF.RDB$FIELD_POSITION",
	} {
		if !strings.Contains(columnUpper, clause) {
			t.Fatalf("column query = %q, want clause %q", columnQuery, clause)
		}
	}
	parameterDomainQuery := ""
	for _, call := range calls {
		upper := strings.ToUpper(call.query)
		if fixtureQueryKind(call.query) == "domains" && !strings.Contains(upper, "NOT STARTING WITH 'RDB$'") {
			parameterDomainQuery = call.query
			break
		}
	}
	if parameterDomainQuery == "" || !strings.Contains(strings.ToUpper(parameterDomainQuery), "CAST(F.RDB$FIELD_NAME AS VARCHAR(31)) AS RDB$FIELD_NAME") {
		t.Fatalf("procedure parameter domain query = %q, want cast domain identity", parameterDomainQuery)
	}

	procedureQuery := fixtureCallQuery(t, calls, "procedures")
	procedureUpper := strings.ToUpper(procedureQuery)
	if strings.Contains(procedureUpper, "RDB$RUNTIME") {
		t.Fatalf("procedure query selects unsupported internal runtime field: %q", procedureQuery)
	}
	wantProcedureQuery := normalizeFixtureSQL(`
SELECT CAST(p.RDB$PROCEDURE_NAME AS VARCHAR(31)) AS RDB$PROCEDURE_NAME, p.RDB$PROCEDURE_ID,
       p.RDB$PROCEDURE_INPUTS, p.RDB$PROCEDURE_OUTPUTS,
       p.RDB$DESCRIPTION, p.RDB$PROCEDURE_SOURCE,
       CAST(p.RDB$SECURITY_CLASS AS VARCHAR(31)) AS RDB$SECURITY_CLASS,
       CAST(p.RDB$OWNER_NAME AS VARCHAR(31)) AS RDB$OWNER_NAME, p.RDB$SYSTEM_FLAG
FROM RDB$PROCEDURES p
WHERE COALESCE(p.RDB$SYSTEM_FLAG, 0) = 0
  AND p.RDB$PROCEDURE_NAME = ?
ORDER BY p.RDB$PROCEDURE_NAME`)
	if got := normalizeFixtureSQL(procedureQuery); got != wantProcedureQuery {
		t.Fatalf("procedure query = %q, want %q", got, wantProcedureQuery)
	}

	domainQuery := fixtureCallQuery(t, calls, "domains")
	domainUpper := normalizeFixtureSQL(domainQuery)
	for _, projection := range []string{
		"CAST(F.RDB$FIELD_NAME AS VARCHAR(31)) AS RDB$FIELD_NAME",
		"CAST(CS.RDB$CHARACTER_SET_NAME AS VARCHAR(31)) AS RDB$CHARACTER_SET_NAME",
		"CAST(CO.RDB$COLLATION_NAME AS VARCHAR(31)) AS RDB$COLLATION_NAME",
	} {
		if !strings.Contains(domainUpper, projection) {
			t.Fatalf("domain query = %q, want identifier projection %q", domainQuery, projection)
		}
	}
	for _, clause := range []string{
		"LEFT JOIN RDB$CHARACTER_SETS CS ON CS.RDB$CHARACTER_SET_ID = F.RDB$CHARACTER_SET_ID",
		"LEFT JOIN RDB$COLLATIONS CO ON CO.RDB$CHARACTER_SET_ID = F.RDB$CHARACTER_SET_ID",
		"AND F.RDB$FIELD_NAME = ?",
		"ORDER BY F.RDB$FIELD_NAME",
	} {
		if !strings.Contains(domainUpper, clause) {
			t.Fatalf("domain query = %q, want original clause %q", domainQuery, clause)
		}
	}

	parameterQuery := fixtureCallQuery(t, calls, "parameters")
	parameterUpper := strings.ToUpper(parameterQuery)
	for _, forbidden := range []string{
		"RDB$NULL_FLAG", "RDB$DEFAULT_SOURCE", "RDB$COLLATION_ID",
		"RDB$PARAMETER_MECHANISM", "RDB$RELATION_NAME", "RDB$FIELD_NAME",
		"RDB$FIELDS",
	} {
		if strings.Contains(parameterUpper, forbidden) {
			t.Fatalf("parameter query selects unsupported field %q: %q", forbidden, parameterQuery)
		}
	}
	wantParameterQuery := normalizeFixtureSQL(`
SELECT CAST(pp.RDB$PARAMETER_NAME AS VARCHAR(31)) AS RDB$PARAMETER_NAME,
       CAST(pp.RDB$PROCEDURE_NAME AS VARCHAR(31)) AS RDB$PROCEDURE_NAME,
       pp.RDB$PARAMETER_NUMBER, pp.RDB$PARAMETER_TYPE,
       CAST(pp.RDB$FIELD_SOURCE AS VARCHAR(31)) AS RDB$FIELD_SOURCE,
       pp.RDB$DESCRIPTION, pp.RDB$SYSTEM_FLAG
FROM RDB$PROCEDURE_PARAMETERS pp
WHERE pp.RDB$PROCEDURE_NAME = ?
ORDER BY pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER`)
	if got := normalizeFixtureSQL(parameterQuery); got != wantParameterQuery {
		t.Fatalf("parameter query = %q, want %q", got, wantParameterQuery)
	}
}

func TestCatalogFullIdentifiers(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	catalog := New(db)
	ctx := context.Background()

	for _, test := range []struct {
		tableName  string
		columnName string
	}{
		{tableName: "IMPORT_ORDER_LINE_ITEMS", columnName: "ITEMS_ONLY_COLUMN"},
		{tableName: "IMPORT_ORDER_LINE_ITEMX", columnName: "ITEMX_ONLY_COLUMN"},
	} {
		table, err := catalog.Table(ctx, test.tableName)
		if err != nil || table == nil || table.Name != test.tableName || len(table.Columns) != 1 || table.Columns[0].Name != test.columnName {
			t.Fatalf("Table(%q) = %#v, %v; want only column %q", test.tableName, table, err, test.columnName)
		}
	}

	// InterBase stores quoted mixed-case names without the quote delimiters.
	const mixedCaseTable = "MiXeD_Table"
	table, err := catalog.Table(ctx, mixedCaseTable)
	if err != nil || table == nil || table.Name != mixedCaseTable || len(table.Columns) != 1 || table.Columns[0].Name != "MiXeD_Column" {
		t.Fatalf("Table(%q) = %#v, %v; want exact mixed-case identity", mixedCaseTable, table, err)
	}
	miss, err := catalog.Table(ctx, "mixed_table")
	if err != nil || miss != nil {
		t.Fatalf("Table(case-mismatched name) = %#v, %v; want nil without error", miss, err)
	}

	calls := state.callsSnapshot()
	relationQuery := fixtureCallQuery(t, calls, "relations")
	relationUpper := strings.ToUpper(relationQuery)
	if !strings.Contains(relationUpper, "CAST(R.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME") {
		t.Fatalf("relation query = %q, want cast full relation identity", relationQuery)
	}
	if !strings.Contains(relationUpper, "AND R.RDB$RELATION_NAME = ?") || strings.Contains(relationUpper, "AND CAST(") {
		t.Fatalf("relation query filter = %q, want original exact bound filter", relationQuery)
	}
	columnQuery := fixtureCallQuery(t, calls, "columns")
	columnUpper := strings.ToUpper(columnQuery)
	if !strings.Contains(columnUpper, "CAST(RF.RDB$FIELD_NAME AS VARCHAR(31)) AS RDB$FIELD_NAME") ||
		!strings.Contains(columnUpper, "WHERE RF.RDB$RELATION_NAME = ?") ||
		!strings.Contains(columnUpper, "ORDER BY RF.RDB$FIELD_POSITION") {
		t.Fatalf("column query = %q, want cast field identities and original filter/order", columnQuery)
	}
	if !strings.Contains(columnUpper, "V.RDB$VIEW_NAME = RF.RDB$RELATION_NAME") ||
		!strings.Contains(columnUpper, "V.RDB$VIEW_CONTEXT = RF.RDB$VIEW_CONTEXT") {
		t.Fatalf("column query = %q, want original view joins", columnQuery)
	}
}

func TestDomainEnumerationUsesExplicitImplicitNameFilter(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()

	if _, err := New(db).Domains(context.Background(), ""); err != nil {
		t.Fatalf("Domains returned error: %v", err)
	}
	query := fixtureCallQuery(t, state.callsSnapshot(), "domains")
	if !strings.Contains(strings.ToUpper(query), "RDB$FIELD_NAME NOT STARTING WITH 'RDB$'") {
		t.Fatalf("domain query = %q, want implicit RDB$ name filter", query)
	}
}

func TestProcedureParameterNullabilityIsConservative(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		wantValid     bool
		wantNullable  bool
		wantDomainNil bool
	}{
		{name: "non-null domain guarantees non-null parameter", mode: "not_null", wantValid: true, wantNullable: false},
		{name: "nullable domain does not prove declaration", mode: "nullable", wantValid: false},
		{name: "NULL field source", mode: "null_field_source", wantValid: false, wantDomainNil: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, _ := openFixtureDBWithOptions(t, fixtureOptions{parameterNullabilityMode: test.mode})
			defer db.Close()

			procedure, err := New(db).Procedure(context.Background(), "PROC_TWO")
			if err != nil {
				t.Fatalf("Procedure returned error: %v", err)
			}
			if procedure == nil || len(procedure.InputParameters) == 0 {
				t.Fatal("Procedure returned no input parameter")
			}
			parameter := procedure.InputParameters[0]
			if parameter.Nullable.Valid != test.wantValid || (parameter.Nullable.Valid && parameter.Nullable.Bool != test.wantNullable) {
				t.Fatalf("parameter nullable = %#v, want valid=%t nullable=%t", parameter.Nullable, test.wantValid, test.wantNullable)
			}
			if test.wantDomainNil && parameter.Domain != nil {
				t.Fatalf("parameter domain = %#v, want nil for NULL field source", parameter.Domain)
			}
		})
	}
}

func TestUnknownObjectsReturnEmptyResults(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)

	tables, err := catalog.Tables(context.Background(), "DOES_NOT_EXIST")
	if err != nil {
		t.Fatalf("unknown Tables returned error: %v", err)
	}
	if len(tables) != 0 {
		t.Fatalf("unknown Tables returned %d objects, want 0", len(tables))
	}
	procedure, err := catalog.Procedure(context.Background(), "DOES_NOT_EXIST")
	if err != nil {
		t.Fatalf("unknown Procedure returned error: %v", err)
	}
	if procedure != nil {
		t.Fatalf("unknown Procedure = %#v, want nil", procedure)
	}
}

func TestSingularLookupsRejectEmptyNames(t *testing.T) {
	catalog := New(openFixtureDB(t))
	ctx := context.Background()
	if _, err := catalog.Table(ctx, ""); err == nil {
		t.Fatal("Table accepted an empty name")
	}
	if _, err := catalog.View(ctx, ""); err == nil {
		t.Fatal("View accepted an empty name")
	}
	if _, err := catalog.Procedure(ctx, ""); err == nil {
		t.Fatal("Procedure accepted an empty name")
	}
}

func TestNameFiltersAreBoundAndRowsDoNotOverlapFollowUpQueries(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	const maliciousName = "ORDERS' OR 1=1 --"

	if _, err := New(db).Tables(context.Background(), maliciousName); err != nil {
		t.Fatalf("malicious table filter returned error: %v", err)
	}
	calls := state.callsSnapshot()
	if len(calls) != 4 {
		t.Fatalf("query count = %d, want 3 identifier-width queries and 1 relation query", len(calls))
	}
	relationCall := fixtureCallForKind(t, calls, "relations")
	if strings.Contains(relationCall.query, maliciousName) {
		t.Fatalf("filter was interpolated into query: %q", relationCall.query)
	}
	if len(relationCall.args) != 1 || relationCall.args[0].Value != maliciousName {
		t.Fatalf("bound filter args = %#v, want malicious value as one argument", relationCall.args)
	}

	if _, err := New(db).Tables(context.Background(), "ORDERS"); err != nil {
		t.Fatalf("normal table filter returned error: %v", err)
	}
	if state.maxActiveRows() != 1 || state.activeRows() != 0 {
		t.Fatalf("row lifetime = (max %d, active %d), want no overlapping cursors", state.maxActiveRows(), state.activeRows())
	}
}

func TestCatalogRejectsNilQueryerAndRequiredEmptyColumnName(t *testing.T) {
	if _, err := (*Catalog)(nil).Tables(context.Background(), ""); !errors.Is(err, ErrNilQueryer) {
		t.Fatalf("nil Catalog error = %v, want ErrNilQueryer", err)
	}
	if _, err := New(nil).Columns(context.Background(), ""); err == nil {
		t.Fatal("empty column relation name was accepted")
	}
}

func TestCatalogPreservesMixedCaseLeadingWhitespaceAndNullFieldSource(t *testing.T) {
	t.Run("identifiers", func(t *testing.T) {
		db, _ := openFixtureDBWithOptions(t, fixtureOptions{identifierVariantFor: "columns"})
		defer db.Close()

		columns, err := New(db).Columns(context.Background(), "ORDERS")
		if err != nil {
			t.Fatalf("Columns returned error: %v", err)
		}
		if len(columns) == 0 {
			t.Fatal("Columns returned no columns")
		}
		column := columns[0]
		if column.Name != "  MiXeD_ID" || column.RelationName != "  MiXeD_REL" {
			t.Fatalf("identifier values = (%q, %q), want leading whitespace/case preserved and trailing padding removed", column.Name, column.RelationName)
		}
		if !column.FieldSource.Valid || column.FieldSource.String != "  MiXeD_DOMAIN" {
			t.Fatalf("field source = %#v, want valid padded identifier with only trailing padding removed", column.FieldSource)
		}
	})

	t.Run("null field source", func(t *testing.T) {
		db, _ := openFixtureDBWithOptions(t, fixtureOptions{nullFieldSourceFor: "columns"})
		defer db.Close()

		columns, err := New(db).Columns(context.Background(), "ORDERS")
		if err != nil {
			t.Fatalf("Columns returned error: %v", err)
		}
		if len(columns) == 0 {
			t.Fatal("Columns returned no columns")
		}
		if columns[0].FieldSource.Valid {
			t.Fatalf("NULL field source = %#v, want invalid NullString", columns[0].FieldSource)
		}
		if columns[0].Domain != nil {
			t.Fatalf("NULL field source domain = %#v, want nil", columns[0].Domain)
		}
	})

	t.Run("procedure parameter identifiers", func(t *testing.T) {
		db, _ := openFixtureDBWithOptions(t, fixtureOptions{identifierVariantFor: "parameters"})
		defer db.Close()

		procedure, err := New(db).Procedure(context.Background(), "PROC_TWO")
		if err != nil {
			t.Fatalf("Procedure returned error: %v", err)
		}
		if procedure == nil || len(procedure.InputParameters) == 0 {
			t.Fatal("Procedure returned no input parameter")
		}
		parameter := procedure.InputParameters[0]
		if parameter.Name != "  MiXeD_PARAM" || parameter.ProcedureName != "  MiXeD_PROC" ||
			!parameter.FieldSource.Valid || parameter.FieldSource.String != "  MiXeD_DOMAIN" {
			t.Fatalf("parameter identifiers = (%q, %q, %#v), want leading whitespace/case preserved", parameter.Name, parameter.ProcedureName, parameter.FieldSource)
		}
	})
}

func TestCatalogPreservesQueryErrorsAndClosesRows(t *testing.T) {
	wantErr := errors.New("fixture query failed")
	db, state := openFixtureDBWithOptions(t, fixtureOptions{queryErrorFor: "relations", queryError: wantErr})
	defer db.Close()

	_, err := New(db).Tables(context.Background(), "ORDERS")
	if !errors.Is(err, wantErr) {
		t.Fatalf("query error = %v, want wrapped fixture error", err)
	}
	if state.activeRows() != 0 {
		t.Fatalf("active rows after query error = %d, want 0", state.activeRows())
	}
}

func TestCatalogPreservesScanAndIterationErrorsAndClosesRows(t *testing.T) {
	tests := []struct {
		name       string
		options    fixtureOptions
		want       error
		wantSubstr string
	}{
		{name: "scan", options: fixtureOptions{scanErrorFor: "relations"}, wantSubstr: "not an integer"},
		{name: "iteration", options: fixtureOptions{iterationErrorFor: "relations", iterationError: errors.New("fixture iteration failed")}, wantSubstr: "fixture iteration failed"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, state := openFixtureDBWithOptions(t, test.options)
			defer db.Close()

			_, err := New(db).Tables(context.Background(), "ORDERS")
			if test.want != nil && !errors.Is(err, test.want) {
				t.Fatalf("%s error = %v, want wrapped fixture error", test.name, err)
			}
			if test.wantSubstr != "" && (err == nil || !strings.Contains(err.Error(), test.wantSubstr)) {
				t.Fatalf("%s error = %v, want substring %q", test.name, err, test.wantSubstr)
			}
			if state.activeRows() != 0 || state.closedRows() == 0 {
				t.Fatalf("%s row lifetime = (active %d, closed %d), want closed rows", test.name, state.activeRows(), state.closedRows())
			}
		})
	}
}

func TestCatalogHonorsCanceledContextsBeforeAndBetweenQueries(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(db).Tables(ctx, "ORDERS"); !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-canceled context error = %v, want context.Canceled", err)
	}
	if len(state.callsSnapshot()) != 0 {
		t.Fatalf("pre-canceled query calls = %d, want 0", len(state.callsSnapshot()))
	}

	db, state = openFixtureDBWithOptions(t, fixtureOptions{cancelOnCloseFor: "relations"})
	defer db.Close()
	ctx, cancel = context.WithCancel(context.Background())
	state.cancel = cancel
	_, err := New(db).Tables(ctx, "ORDERS")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("between-query cancellation error = %v, want context.Canceled", err)
	}
	if len(state.callsSnapshot()) != 4 {
		t.Fatalf("between-query cancellation calls = %d, want identifier-width queries followed by relation query", len(state.callsSnapshot()))
	}
}

func TestCatalogClosesRowsWhenParameterDirectionIsInvalid(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{invalidParameterTypeFor: "parameters"})
	defer db.Close()

	_, err := New(db).Procedures(context.Background(), "PROC_TWO")
	if err == nil || !strings.Contains(err.Error(), "invalid parameter type") {
		t.Fatalf("invalid parameter type error = %v, want validation error", err)
	}
	if state.activeRows() != 0 || state.closedRows() != 5 {
		t.Fatalf("invalid parameter row lifetime = (active %d, closed %d), want identifier-width and catalog cursors closed", state.activeRows(), state.closedRows())
	}
}

func TestCatalogChildQueryAndIterationFailuresCloseEveryCursor(t *testing.T) {
	tests := []struct {
		name       string
		options    fixtureOptions
		operation  func(*Catalog) error
		want       error
		wantSubstr string
	}{
		{
			name:    "columns query",
			options: fixtureOptions{queryErrorFor: "columns", queryError: errors.New("columns query failed")},
			operation: func(catalog *Catalog) error {
				_, err := catalog.Columns(context.Background(), "ORDERS")
				return err
			},
			want: errors.New("columns query failed"),
		},
		{
			name:    "procedures query",
			options: fixtureOptions{queryErrorFor: "procedures", queryError: errors.New("procedures query failed")},
			operation: func(catalog *Catalog) error {
				_, err := catalog.Procedures(context.Background(), "PROC_TWO")
				return err
			},
			want: errors.New("procedures query failed"),
		},
		{
			name:    "parameters query",
			options: fixtureOptions{queryErrorFor: "parameters", queryError: errors.New("parameters query failed")},
			operation: func(catalog *Catalog) error {
				_, err := catalog.Procedures(context.Background(), "PROC_TWO")
				return err
			},
			want: errors.New("parameters query failed"),
		},
		{
			name:       "columns scan",
			options:    fixtureOptions{scanErrorFor: "columns"},
			operation:  func(catalog *Catalog) error { _, err := catalog.Columns(context.Background(), "ORDERS"); return err },
			wantSubstr: "not an integer",
		},
		{
			name:    "parameters iteration",
			options: fixtureOptions{iterationErrorFor: "parameters", iterationError: errors.New("parameters iteration failed")},
			operation: func(catalog *Catalog) error {
				_, err := catalog.Procedures(context.Background(), "PROC_TWO")
				return err
			},
			wantSubstr: "parameters iteration failed",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, state := openFixtureDBWithOptions(t, test.options)
			defer db.Close()

			err := test.operation(New(db))
			if test.want != nil && !strings.Contains(errString(err), test.want.Error()) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
			if test.wantSubstr != "" && (err == nil || !strings.Contains(err.Error(), test.wantSubstr)) {
				t.Fatalf("error = %v, want substring %q", err, test.wantSubstr)
			}
			if err == nil {
				t.Fatal("operation returned nil error")
			}
			if state.activeRows() != 0 {
				t.Fatalf("active rows after failure = %d, want 0", state.activeRows())
			}
		})
	}
}

func TestCatalogPreservesChildErrorsForRelationsAndProcedures(t *testing.T) {
	tests := []struct {
		name      string
		kind      string
		operation func(*Catalog) error
	}{
		{
			name: "relation columns",
			kind: "columns",
			operation: func(catalog *Catalog) error {
				_, err := catalog.Tables(context.Background(), "ORDERS")
				return err
			},
		},
		{
			name: "procedure parameters",
			kind: "parameters",
			operation: func(catalog *Catalog) error {
				_, err := catalog.Procedures(context.Background(), "PROC_TWO")
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			wantErr := errors.New("child query failed")
			db, _ := openFixtureDBWithOptions(t, fixtureOptions{queryErrorFor: test.kind, queryError: wantErr})
			defer db.Close()

			if err := test.operation(New(db)); !errors.Is(err, wantErr) {
				t.Fatalf("child error = %v, want errors.Is(..., %v)", err, wantErr)
			}
		})
	}
}

func TestCatalogJoinsResultCloseErrorWithPrimaryFailure(t *testing.T) {
	closeErr := errors.New("columns close failed")
	db, state := openFixtureDBWithOptions(t, fixtureOptions{
		scanErrorFor: "columns", closeErrorFor: "columns", closeError: closeErr,
	})
	defer db.Close()

	_, err := New(db).Columns(context.Background(), "ORDERS")
	if err == nil || !strings.Contains(err.Error(), "not an integer") {
		t.Fatalf("primary error = %v, want scan failure", err)
	}
	if !errors.Is(err, closeErr) {
		t.Fatalf("combined error = %v, want close failure", err)
	}
	if state.activeRows() != 0 {
		t.Fatalf("active rows after close failure = %d, want 0", state.activeRows())
	}
}

func TestCatalogReportsUnknownNullabilityWhenCatalogFlagIsNull(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{nullabilityUnknownFor: "columns"})
	defer db.Close()

	columns, err := New(db).Columns(context.Background(), "ORDERS")
	if err != nil {
		t.Fatalf("Columns returned error: %v", err)
	}
	if len(columns) == 0 {
		t.Fatal("Columns returned no columns")
	}
	if columns[0].NullFlag.Valid || columns[0].Nullable.Valid {
		t.Fatalf("unknown column nullability = (%#v, %#v), want both invalid", columns[0].NullFlag, columns[0].Nullable)
	}
	if state.activeRows() != 0 {
		t.Fatalf("active rows after nullability read = %d, want 0", state.activeRows())
	}
}

func TestCatalogCombinesColumnAndDomainNullabilityConservatively(t *testing.T) {
	tests := []struct {
		name          string
		mode          string
		wantValid     bool
		wantNullable  bool
		wantDomainNil bool
	}{
		{name: "both nullable", mode: "both_nullable", wantValid: true, wantNullable: true},
		{name: "domain not null", mode: "domain_not_null", wantValid: true, wantNullable: false},
		{name: "local not null over nullable domain", mode: "column_not_null_domain_nullable", wantValid: true, wantNullable: false},
		{name: "column flag unknown", mode: "column_unknown", wantValid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, _ := openFixtureDBWithOptions(t, fixtureOptions{columnNullabilityMode: test.mode})
			defer db.Close()

			columns, err := New(db).Columns(context.Background(), "ORDERS")
			if err != nil {
				t.Fatalf("Columns returned error: %v", err)
			}
			if len(columns) == 0 || columns[0].Domain == nil {
				t.Fatal("expected first column and its domain")
			}
			got := columns[0].Nullable
			if got.Valid != test.wantValid || (got.Valid && got.Bool != test.wantNullable) {
				t.Fatalf("effective nullable = %#v, want valid=%t nullable=%t", got, test.wantValid, test.wantNullable)
			}
		})
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func relationNames(relations []Relation) []string {
	names := make([]string, 0, len(relations))
	for _, relation := range relations {
		names = append(names, relation.Name)
	}
	return names
}

func parameterNames(parameters []ProcedureParameter) []string {
	names := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		names = append(names, parameter.Name)
	}
	return names
}

func fixtureCallQuery(t *testing.T, calls []fixtureCall, kind string) string {
	t.Helper()
	for _, call := range calls {
		if fixtureQueryKind(call.query) == kind {
			return call.query
		}
	}
	t.Fatalf("no %s query recorded", kind)
	return ""
}

func fixtureCallForKind(t *testing.T, calls []fixtureCall, kind string) fixtureCall {
	t.Helper()
	for _, call := range calls {
		if fixtureQueryKind(call.query) == kind {
			return call
		}
	}
	t.Fatalf("no %s query recorded", kind)
	return fixtureCall{}
}

func normalizeFixtureSQL(query string) string {
	return strings.Join(strings.Fields(strings.ToUpper(query)), " ")
}

func openFixtureDB(t *testing.T) *sql.DB {
	t.Helper()
	db, _ := openFixtureDBWithOptions(t, fixtureOptions{})
	return db
}

func openFixtureDBWithOptions(t *testing.T, options fixtureOptions) (*sql.DB, *fixtureState) {
	t.Helper()
	state := &fixtureState{options: options}
	db := sql.OpenDB(fixtureConnector{state: state})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return db, state
}

type fixtureOptions struct {
	queryErrorFor            string
	queryError               error
	scanErrorFor             string
	iterationErrorFor        string
	iterationError           error
	invalidParameterTypeFor  string
	nullabilityUnknownFor    string
	columnNullabilityMode    string
	parameterNullabilityMode string
	identifierVariantFor     string
	nullFieldSourceFor       string
	closeErrorFor            string
	closeError               error
	cancelOnCloseFor         string
	identifierWidthMode      string
}

type fixtureCall struct {
	query string
	args  []driver.NamedValue
}

type fixtureState struct {
	mu      sync.Mutex
	options fixtureOptions
	calls   []fixtureCall
	active  int
	max     int
	closed  int
	cancel  context.CancelFunc
}

func (s *fixtureState) recordCall(query string, args []driver.NamedValue) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cloned := append([]driver.NamedValue(nil), args...)
	s.calls = append(s.calls, fixtureCall{query: query, args: cloned})
}

func (s *fixtureState) beginRows() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != 0 {
		return errors.New("fixture: a previous result is still active")
	}
	s.active++
	if s.active > s.max {
		s.max = s.active
	}
	return nil
}

func (s *fixtureState) endRows() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active > 0 {
		s.active--
	}
	s.closed++
}

func (s *fixtureState) callsSnapshot() []fixtureCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	result := make([]fixtureCall, len(s.calls))
	for i, call := range s.calls {
		result[i] = fixtureCall{query: call.query, args: append([]driver.NamedValue(nil), call.args...)}
	}
	return result
}

func (s *fixtureState) activeRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.active
}

func (s *fixtureState) maxActiveRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.max
}

func (s *fixtureState) closedRows() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

type fixtureConnector struct {
	state *fixtureState
}

func (c fixtureConnector) Connect(ctx context.Context) (driver.Conn, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return &fixtureConn{state: c.state}, nil
}

func (c fixtureConnector) Driver() driver.Driver { return fixtureDriver{} }

type fixtureDriver struct{}

func (fixtureDriver) Open(string) (driver.Conn, error) {
	return &fixtureConn{state: &fixtureState{}}, nil
}

type fixtureConn struct {
	state *fixtureState
}

func (*fixtureConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (*fixtureConn) Close() error                        { return nil }
func (*fixtureConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c *fixtureConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	kind := fixtureQueryKind(query)
	c.state.recordCall(query, args)
	if err := c.state.beginRows(); err != nil {
		return nil, err
	}
	columns, values, err := fixtureResult(kind, query, args)
	if err != nil {
		c.state.endRows()
		return nil, err
	}
	c.state.mu.Lock()
	options := c.state.options
	c.state.mu.Unlock()
	if options.queryErrorFor == kind {
		c.state.endRows()
		if options.queryError != nil {
			return nil, options.queryError
		}
		return nil, errors.New("fixture: query failed")
	}
	if options.scanErrorFor == kind {
		values = fixtureScanFailureValues(kind, values)
	}
	if options.invalidParameterTypeFor == kind && len(values) != 0 {
		values = append([][]driver.Value(nil), values...)
		values[0] = append([]driver.Value(nil), values[0]...)
		values[0][3] = int64(99)
	}
	if options.nullabilityUnknownFor == kind && len(values) != 0 {
		values = append([][]driver.Value(nil), values...)
		values[0] = append([]driver.Value(nil), values[0]...)
		switch kind {
		case "columns":
			values[0][9] = nil
			values[0][29] = nil
		}
	}
	values = applyFixtureOptions(kind, values, options)
	rows := &fixtureRows{
		state:    c.state,
		kind:     kind,
		columns:  columns,
		values:   values,
		closeErr: fixtureErrorFor(kind, options.closeErrorFor, options.closeError),
	}
	if options.iterationErrorFor == kind {
		rows.nextErrAt = 1
		rows.nextErr = options.iterationError
		if rows.nextErr == nil {
			rows.nextErr = errors.New("fixture: iteration failed")
		}
	}
	return rows, nil
}

func applyFixtureOptions(kind string, values [][]driver.Value, options fixtureOptions) [][]driver.Value {
	if len(values) == 0 {
		return values
	}
	cloneFirstRow := func() {
		values = append([][]driver.Value(nil), values...)
		values[0] = append([]driver.Value(nil), values[0]...)
	}
	if options.identifierWidthMode == "unsupported" && kind == "identifier_width_bootstrap" {
		cloneFirstRow()
		values[0][0] = int64(32767)
	}
	if kind == "identifier_widths" && (options.identifierWidthMode == "zero" || options.identifierWidthMode == "null") {
		for row := range values {
			relation, relationOK := values[row][0].(string)
			field, fieldOK := values[row][1].(string)
			if relationOK && fieldOK && strings.TrimRight(relation, " ") == "RDB$RELATION_CONSTRAINTS" && strings.TrimRight(field, " ") == "RDB$CONSTRAINT_NAME" {
				values = append([][]driver.Value(nil), values...)
				values[row] = append([]driver.Value(nil), values[row]...)
				if options.identifierWidthMode == "zero" {
					values[row][2] = int64(0)
				} else {
					values[row][2] = nil
				}
				break
			}
		}
	}
	if options.columnNullabilityMode != "" && kind == "columns" {
		cloneFirstRow()
		switch options.columnNullabilityMode {
		case "both_nullable":
			values[0][9] = int64(0)
			values[0][29] = int64(0)
		case "domain_not_null":
			values[0][9] = int64(0)
			values[0][29] = int64(1)
		case "column_not_null_domain_nullable":
			values[0][9] = int64(1)
			values[0][29] = int64(0)
		case "column_unknown":
			values[0][9] = nil
			values[0][29] = int64(0)
		}
	}
	if options.parameterNullabilityMode != "" && kind == "domains" {
		cloneFirstRow()
		switch options.parameterNullabilityMode {
		case "not_null":
			values[0][15] = int64(1)
		case "nullable":
			values[0][15] = int64(0)
		}
	}
	if options.parameterNullabilityMode == "null_field_source" && kind == "parameters" {
		cloneFirstRow()
		values[0][4] = nil
	}
	if options.identifierVariantFor == kind {
		cloneFirstRow()
		switch kind {
		case "columns":
			values[0][0] = "  MiXeD_ID  "
			values[0][1] = "  MiXeD_REL  "
			values[0][2] = "  MiXeD_DOMAIN  "
		case "parameters":
			values[0][0] = "  MiXeD_PARAM  "
			values[0][1] = "  MiXeD_PROC  "
			values[0][4] = "  MiXeD_DOMAIN  "
		}
	}
	if options.nullFieldSourceFor == kind {
		cloneFirstRow()
		switch kind {
		case "columns":
			values[0][2] = nil
		case "parameters":
			values[0][4] = nil
		}
	}
	return values
}

func fixtureErrorFor(kind, configuredKind string, configured error) error {
	if kind != configuredKind {
		return nil
	}
	if configured != nil {
		return configured
	}
	return errors.New("fixture: close failed")
}

func fixtureQueryKind(query string) string {
	upper := strings.ToUpper(query)
	normalized := normalizeFixtureSQL(query)
	switch {
	case strings.HasPrefix(normalized, "SELECT CAST(RF.RDB$RELATION_NAME AS VARCHAR("):
		return "identifier_widths"
	case strings.Contains(upper, "FROM RDB$RELATION_FIELDS RF") && strings.Contains(upper, "SELECT F.RDB$FIELD_LENGTH"):
		return "identifier_width_bootstrap"
	case strings.Contains(upper, "FROM RDB$FIELDS"):
		return "domains"
	case strings.Contains(upper, "FROM RDB$GENERATORS"):
		return "sequences"
	case strings.Contains(upper, "FROM RDB$INDEX_SEGMENTS"):
		return "index_segments"
	case strings.Contains(upper, "FROM RDB$RELATION_CONSTRAINTS"):
		return "constraints"
	case strings.Contains(upper, "FROM RDB$INDICES"):
		return "indexes"
	case strings.Contains(upper, "FROM RDB$TRIGGERS"):
		return "triggers"
	case strings.Contains(upper, "FROM RDB$ROLES"):
		return "roles"
	case strings.Contains(upper, "FROM RDB$DEPENDENCIES"):
		return "dependencies"
	case strings.Contains(upper, "FROM RDB$FUNCTION_ARGUMENTS"):
		return "function_arguments"
	case strings.Contains(upper, "FROM RDB$FUNCTIONS"):
		return "functions"
	case strings.Contains(upper, "FROM RDB$USER_PRIVILEGES"):
		return "privileges"
	case strings.Contains(upper, "FROM RDB$FILES") && strings.Contains(upper, "RDB$SHADOW_NUMBER > 0"):
		return "shadows"
	case strings.Contains(upper, "FROM RDB$FILES") && strings.Contains(upper, "RDB$SHADOW_NUMBER = ?"):
		return "shadow_files"
	case strings.Contains(upper, "FROM RDB$FILES"):
		return "database_files"
	case strings.Contains(upper, "FROM RDB$RELATION_FIELDS"):
		return "columns"
	case strings.Contains(upper, "FROM RDB$PROCEDURE_PARAMETERS"):
		return "parameters"
	case strings.Contains(upper, "FROM RDB$PROCEDURES"):
		return "procedures"
	case strings.Contains(upper, "FROM RDB$RELATIONS"):
		return "relations"
	default:
		return "unknown"
	}
}

func fixtureResult(kind, query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
	if kind == "identifier_width_bootstrap" {
		return fixtureIdentifierWidthBootstrapResult(query, args)
	}
	if kind == "identifier_widths" {
		return fixtureIdentifierWidthsResult(query, args)
	}

	filter, err := fixtureFilter(args)
	if err != nil {
		return nil, nil, err
	}
	switch kind {
	case "relations":
		upper := strings.ToUpper(query)
		wantKind := RelationKind("")
		if strings.Contains(upper, "AND R.RDB$VIEW_BLR IS NULL") {
			wantKind = RelationTable
		} else if strings.Contains(upper, "WHERE R.RDB$VIEW_BLR IS NOT NULL") {
			wantKind = RelationView
		}
		values := make([][]driver.Value, 0)
		relations := fixtureRelations()
		if strings.HasPrefix(filter, "IMPORT_ORDER_LINE_ITEM") || filter == "MiXeD_Table" {
			relations = fixtureFullIdentifierRelations()
		}
		for _, relation := range relations {
			if wantKind != "" && relation.kind != wantKind {
				continue
			}
			if filter != "" && relation.name != filter {
				continue
			}
			relationValues := append([]driver.Value(nil), relation.values...)
			if strings.HasPrefix(relation.name, "IMPORT_ORDER_LINE_ITEM") && !strings.Contains(upper, "CAST(R.RDB$RELATION_NAME") {
				name := strings.TrimRight(relationValues[0].(string), " ")
				relationValues[0] = name[:22]
			}
			values = append(values, relationValues)
		}
		return relationResultColumns, values, nil
	case "columns":
		if filter == "" {
			return nil, nil, errors.New("fixture: missing relation filter")
		}
		relations := fixtureRelations()
		if strings.HasPrefix(filter, "IMPORT_ORDER_LINE_ITEM") || filter == "MiXeD_Table" {
			relations = fixtureFullIdentifierRelations()
		}
		for _, relation := range relations {
			if relation.name == filter {
				values := relation.columns
				for row := range values {
					for len(values[row]) < len(relationColumnsResult) {
						values[row] = append(values[row], nil)
					}
				}
				if relation.kind == RelationView && !strings.Contains(strings.ToUpper(query), "V.RDB$VIEW_NAME = RF.RDB$RELATION_NAME") {
					for _, other := range fixtureRelations() {
						if other.kind == RelationView && other.name != filter {
							values = append(append([][]driver.Value(nil), values...), other.columns...)
							break
						}
					}
				}
				return relationColumnsResult, values, nil
			}
		}
		return relationColumnsResult, nil, nil
	case "domains":
		values := make([][]driver.Value, 0)
		excludeImplicitNames := strings.Contains(strings.ToUpper(query), "RDB$FIELD_NAME NOT STARTING WITH 'RDB$'")
		for _, domain := range fixtureDomains() {
			if excludeImplicitNames && strings.HasPrefix(domain.name, "RDB$") {
				continue
			}
			if filter == "" || domain.name == filter {
				values = append(values, domain.values)
				if filter != "" {
					break
				}
			}
		}
		return domainResultColumns, values, nil
	case "sequences":
		values := make([][]driver.Value, 0)
		for _, sequence := range fixtureSequences() {
			if filter == "" || sequence.name == filter {
				values = append(values, sequence.values)
				if filter != "" {
					break
				}
			}
		}
		return sequenceResultColumns, values, nil
	case "indexes":
		values := make([][]driver.Value, 0)
		relationScoped := strings.Contains(strings.ToUpper(query), "RDB$RELATION_NAME = ?")
		for _, index := range fixtureIndexes() {
			matches := filter == "" || index.name == filter
			if relationScoped {
				matches = filter == "" || index.relationName == filter
			}
			if matches {
				values = append(values, index.values)
				if filter != "" && !relationScoped {
					break
				}
			}
		}
		return indexResultColumns, values, nil
	case "index_segments":
		if filter == "" {
			return indexSegmentResultColumns, nil, nil
		}
		for _, index := range fixtureIndexes() {
			if index.name == filter {
				return indexSegmentResultColumns, index.segments, nil
			}
		}
		return indexSegmentResultColumns, nil, nil
	case "constraints":
		values := make([][]driver.Value, 0)
		relationScoped := strings.Contains(strings.ToUpper(query), "RDB$RELATION_NAME = ?")
		for _, constraint := range fixtureConstraints() {
			matches := filter == "" || constraint.name == filter
			if relationScoped {
				matches = filter == "" || constraint.relationName == filter
			}
			if matches {
				values = append(values, constraint.values)
				if filter != "" && !relationScoped {
					break
				}
			}
		}
		return constraintResultColumns, values, nil
	case "triggers":
		values := make([][]driver.Value, 0)
		for _, trigger := range fixtureTriggers() {
			matches := filter == "" || trigger.name == filter
			if strings.Contains(strings.ToUpper(query), "RDB$RELATION_NAME = ?") {
				matches = filter == "" || trigger.relationName == filter
			}
			if matches {
				values = append(values, trigger.values)
				if filter != "" {
					break
				}
			}
		}
		return triggerResultColumns, values, nil
	case "roles":
		values := make([][]driver.Value, 0)
		for _, role := range fixtureRoles() {
			if filter == "" || role.name == filter {
				values = append(values, role.values)
				if filter != "" {
					break
				}
			}
		}
		return roleResultColumns, values, nil
	case "dependencies":
		values := make([][]driver.Value, 0)
		for _, dependency := range fixtureDependencies() {
			if filter == "" || dependency.name == filter {
				values = append(values, dependency.values)
				if filter != "" {
					break
				}
			}
		}
		return dependencyResultColumns, values, nil
	case "functions":
		values := make([][]driver.Value, 0)
		for _, function := range fixtureFunctions() {
			if filter == "" || function.name == filter {
				values = append(values, function.values)
				if filter != "" {
					break
				}
			}
		}
		return functionResultColumns, values, nil
	case "function_arguments":
		if filter == "" {
			return functionArgumentResultColumns, nil, nil
		}
		for _, function := range fixtureFunctions() {
			if function.name == filter {
				return functionArgumentResultColumns, function.arguments, nil
			}
		}
		return functionArgumentResultColumns, nil, nil
	case "database_files":
		return databaseFileResultColumns, fixtureDatabaseFiles(0), nil
	case "shadow_files":
		shadowNumber, err := fixtureNumericFilter(args)
		if err != nil {
			return nil, nil, err
		}
		return databaseFileResultColumns, fixtureDatabaseFiles(shadowNumber), nil
	case "shadows":
		return shadowResultColumns, fixtureShadows(), nil
	case "privileges":
		values := make([][]driver.Value, 0)
		for _, privilege := range fixturePrivileges() {
			if filter == "" || privilege.name == filter {
				values = append(values, privilege.values)
				if filter != "" {
					break
				}
			}
		}
		return privilegeResultColumns, values, nil
	case "procedures":
		values := make([][]driver.Value, 0)
		for _, procedure := range fixtureProcedures() {
			if filter == "" || procedure.name == filter {
				values = append(values, procedure.values)
			}
		}
		return procedureResultColumns, values, nil
	case "parameters":
		if filter == "" {
			return nil, nil, errors.New("fixture: missing procedure filter")
		}
		for _, procedure := range fixtureProcedures() {
			if procedure.name == filter {
				return parameterResultColumns, procedure.parameters, nil
			}
		}
		return parameterResultColumns, nil, nil
	default:
		return nil, nil, errors.New("fixture: unexpected query")
	}
}

func fixtureIdentifierWidthBootstrapResult(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
	wantQuery := normalizeFixtureSQL(`
SELECT f.RDB$FIELD_LENGTH
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE rf.RDB$RELATION_NAME = ?
  AND rf.RDB$FIELD_NAME = ?`)
	if got := normalizeFixtureSQL(query); got != wantQuery {
		return nil, nil, errors.New("fixture: unexpected identifier width bootstrap query shape: " + query)
	}
	if len(args) != 2 {
		return nil, nil, errors.New("fixture: identifier width bootstrap requires two bound filters")
	}
	relation, relationOK := args[0].Value.(string)
	field, fieldOK := args[1].Value.(string)
	if !relationOK || !fieldOK {
		return nil, nil, errors.New("fixture: identifier width bootstrap filters are not strings")
	}
	switch {
	case relation == "RDB$RELATIONS" && field == "RDB$RELATION_NAME":
		return []string{"FIELD_LENGTH"}, [][]driver.Value{{int64(67)}}, nil
	case relation == "RDB$RELATION_FIELDS" && field == "RDB$RELATION_NAME":
		return []string{"FIELD_LENGTH"}, [][]driver.Value{{int64(31)}}, nil
	case relation == "RDB$RELATION_FIELDS" && field == "RDB$FIELD_NAME":
		return []string{"FIELD_LENGTH"}, [][]driver.Value{{int64(31)}}, nil
	default:
		return nil, nil, errors.New("fixture: unexpected identifier width bootstrap filter")
	}
}

func fixtureIdentifierWidthsResult(query string, args []driver.NamedValue) ([]string, [][]driver.Value, error) {
	if strings.Contains(strings.ToUpper(query), "VARCHAR(32767)") {
		return nil, nil, errors.New("fixture: database rejected VARCHAR width 32767")
	}
	if len(args) != 0 {
		return nil, nil, errors.New("fixture: identifier widths query does not accept arguments")
	}
	wantQuery := normalizeFixtureSQL(`
SELECT CAST(rf.RDB$RELATION_NAME AS VARCHAR(31)),
       CAST(rf.RDB$FIELD_NAME AS VARCHAR(31)),
       f.RDB$FIELD_LENGTH
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON rf.RDB$FIELD_SOURCE = f.RDB$FIELD_NAME`)
	if got := normalizeFixtureSQL(query); got != wantQuery {
		return nil, nil, errors.New("fixture: unexpected identifier widths query shape: " + query)
	}
	return []string{"RELATION_NAME", "FIELD_NAME", "FIELD_LENGTH"}, [][]driver.Value{
		{"RDB$RELATIONS      ", "RDB$RELATION_NAME     ", int64(67)},
		{"RDB$RELATIONS      ", "RDB$SECURITY_CLASS    ", int64(31)},
		{"RDB$RELATIONS      ", "RDB$OWNER_NAME        ", int64(31)},
		{"RDB$RELATIONS      ", "RDB$DEFAULT_CLASS     ", int64(31)},
		{"RDB$RELATION_FIELDS      ", "RDB$RELATION_NAME     ", int64(31)},
		{"RDB$RELATION_FIELDS      ", "RDB$FIELD_NAME      ", int64(31)},
		{"RDB$RELATION_FIELDS      ", "RDB$FIELD_SOURCE     ", int64(31)},
		{"RDB$RELATION_FIELDS      ", "RDB$SECURITY_CLASS    ", int64(31)},
		{"RDB$RELATION_FIELDS      ", "RDB$BASE_FIELD        ", int64(31)},
		{"RDB$VIEW_RELATIONS      ", "RDB$RELATION_NAME     ", int64(67)},
		{"RDB$FIELDS      ", "RDB$FIELD_NAME     ", int64(31)},
		{"RDB$CHARACTER_SETS      ", "RDB$CHARACTER_SET_NAME     ", int64(31)},
		{"RDB$COLLATIONS      ", "RDB$COLLATION_NAME     ", int64(31)},
		{"RDB$PROCEDURES      ", "RDB$PROCEDURE_NAME     ", int64(31)},
		{"RDB$PROCEDURES      ", "RDB$SECURITY_CLASS    ", int64(31)},
		{"RDB$PROCEDURES      ", "RDB$OWNER_NAME        ", int64(31)},
		{"RDB$PROCEDURE_PARAMETERS      ", "RDB$PARAMETER_NAME     ", int64(31)},
		{"RDB$PROCEDURE_PARAMETERS      ", "RDB$PROCEDURE_NAME     ", int64(31)},
		{"RDB$PROCEDURE_PARAMETERS      ", "RDB$FIELD_SOURCE     ", int64(31)},
		{"RDB$GENERATORS      ", "RDB$GENERATOR_NAME     ", int64(67)},
		{"RDB$INDICES      ", "RDB$INDEX_NAME     ", int64(67)},
		{"RDB$INDICES      ", "RDB$RELATION_NAME     ", int64(67)},
		{"RDB$INDICES      ", "RDB$FOREIGN_KEY     ", int64(67)},
		{"RDB$INDEX_SEGMENTS      ", "RDB$INDEX_NAME     ", int64(67)},
		{"RDB$INDEX_SEGMENTS      ", "RDB$FIELD_NAME     ", int64(67)},
		{"RDB$RELATION_CONSTRAINTS      ", "RDB$CONSTRAINT_NAME     ", int64(31)},
		{"RDB$RELATION_CONSTRAINTS      ", "RDB$RELATION_NAME     ", int64(67)},
		{"RDB$RELATION_CONSTRAINTS      ", "RDB$INDEX_NAME     ", int64(67)},
		{"RDB$REF_CONSTRAINTS      ", "RDB$CONST_NAME_UQ     ", int64(67)},
		{"RDB$CHECK_CONSTRAINTS      ", "RDB$TRIGGER_NAME     ", int64(67)},
		{"RDB$TRIGGERS      ", "RDB$TRIGGER_NAME     ", int64(67)},
		{"RDB$TRIGGERS      ", "RDB$RELATION_NAME     ", int64(67)},
		{"RDB$ROLES      ", "RDB$ROLE_NAME     ", int64(67)},
		{"RDB$DEPENDENCIES      ", "RDB$DEPENDENT_NAME     ", int64(67)},
		{"RDB$DEPENDENCIES      ", "RDB$FIELD_NAME     ", int64(67)},
		{"RDB$DEPENDENCIES      ", "RDB$DEPENDED_ON_NAME     ", int64(67)},
		{"RDB$FUNCTIONS      ", "RDB$FUNCTION_NAME     ", int64(67)},
		{"RDB$FUNCTION_ARGUMENTS      ", "RDB$FUNCTION_NAME     ", int64(67)},
		{"RDB$USER_PRIVILEGES      ", "RDB$RELATION_NAME     ", int64(67)},
		{"RDB$USER_PRIVILEGES      ", "RDB$FIELD_NAME     ", int64(67)},
	}, nil
}

func fixtureFilter(args []driver.NamedValue) (string, error) {
	if len(args) == 0 {
		return "", nil
	}
	if len(args) != 1 {
		return "", errors.New("fixture: unexpected argument count")
	}
	value, ok := args[0].Value.(string)
	if !ok {
		return "", errors.New("fixture: filter is not a string")
	}
	return value, nil
}

func fixtureNumericFilter(args []driver.NamedValue) (int64, error) {
	if len(args) != 1 {
		return 0, errors.New("fixture: unexpected numeric argument count")
	}
	value, ok := args[0].Value.(int64)
	if !ok {
		return 0, errors.New("fixture: numeric filter is not int64")
	}
	return value, nil
}

func fixtureScanFailureValues(kind string, values [][]driver.Value) [][]driver.Value {
	if len(values) == 0 {
		return values
	}
	result := make([][]driver.Value, len(values))
	for i := range values {
		result[i] = append([]driver.Value(nil), values[i]...)
	}
	switch kind {
	case "relations", "procedures":
		result[0][1] = "not an integer"
	case "columns":
		result[0][3] = "not an integer"
	case "parameters":
		result[0][2] = "not an integer"
	}
	return result
}

type fixtureRows struct {
	mu        sync.Mutex
	state     *fixtureState
	kind      string
	columns   []string
	values    [][]driver.Value
	index     int
	nextErrAt int
	nextErr   error
	closeErr  error
	closed    bool
}

func (r *fixtureRows) Columns() []string { return r.columns }

func (r *fixtureRows) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil
	}
	r.closed = true
	r.state.endRows()
	r.state.mu.Lock()
	cancel := r.state.cancel
	shouldCancel := r.state.options.cancelOnCloseFor == r.kind
	r.state.mu.Unlock()
	if shouldCancel && cancel != nil {
		cancel()
	}
	return r.closeErr
}

func (r *fixtureRows) Next(dest []driver.Value) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return io.EOF
	}
	if r.nextErr != nil && r.index >= r.nextErrAt {
		return r.nextErr
	}
	if r.index == len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

type fixtureRelation struct {
	name    string
	kind    RelationKind
	values  []driver.Value
	columns [][]driver.Value
}

type fixtureProcedure struct {
	name       string
	values     []driver.Value
	parameters [][]driver.Value
}

type fixtureDomain struct {
	name   string
	values []driver.Value
}

type fixtureSequence struct {
	name   string
	values []driver.Value
}

type fixtureIndex struct {
	name         string
	relationName string
	values       []driver.Value
	segments     [][]driver.Value
}

type fixtureConstraint struct {
	name         string
	relationName string
	values       []driver.Value
}

type fixtureTrigger struct {
	name         string
	relationName string
	values       []driver.Value
}

type fixtureRole struct {
	name   string
	values []driver.Value
}

type fixtureDependency struct {
	name   string
	values []driver.Value
}

type fixtureFunction struct {
	name      string
	values    []driver.Value
	arguments [][]driver.Value
}

var relationResultColumns = []string{
	"RELATION_NAME", "RELATION_ID", "VIEW_SOURCE", "DESCRIPTION", "SECURITY_CLASS", "OWNER_NAME", "DEFAULT_CLASS", "DBKEY_LENGTH", "FORMAT", "EXTERNAL_FILE", "FLAGS", "RELATION_TYPE", "SYSTEM_FLAG", "RELATION_KIND",
}

var relationColumnsResult = []string{
	"FIELD_NAME", "RELATION_NAME", "FIELD_SOURCE", "FIELD_POSITION", "UPDATE_FLAG", "FIELD_ID", "DESCRIPTION", "SYSTEM_FLAG", "SECURITY_CLASS", "NULL_FLAG", "DEFAULT_SOURCE", "COLLATION_ID", "BASE_FIELD", "BASE_RELATION",
	"DOMAIN_NAME", "VALIDATION_SOURCE", "COMPUTED_SOURCE", "DOMAIN_DEFAULT_SOURCE", "FIELD_LENGTH", "FIELD_SCALE", "FIELD_TYPE", "FIELD_SUB_TYPE", "DOMAIN_DESCRIPTION", "DOMAIN_SYSTEM_FLAG", "SEGMENT_LENGTH", "EXTERNAL_LENGTH", "EXTERNAL_SCALE", "EXTERNAL_TYPE", "DIMENSIONS", "DOMAIN_NULL_FLAG", "CHARACTER_LENGTH", "DOMAIN_COLLATION_ID", "CHARACTER_SET_ID", "FIELD_PRECISION", "CHARACTER_SET_NAME", "COLLATION_NAME", "COLUMN_COLLATION_NAME",
}

var procedureResultColumns = []string{
	"PROCEDURE_NAME", "PROCEDURE_ID", "PROCEDURE_INPUTS", "PROCEDURE_OUTPUTS", "DESCRIPTION", "SOURCE", "SECURITY_CLASS", "OWNER_NAME", "SYSTEM_FLAG",
}

var parameterResultColumns = []string{
	"PARAMETER_NAME", "PROCEDURE_NAME", "PARAMETER_NUMBER", "PARAMETER_TYPE", "FIELD_SOURCE", "DESCRIPTION", "SYSTEM_FLAG",
}

var domainResultColumns = []string{
	"FIELD_NAME", "VALIDATION_SOURCE", "COMPUTED_SOURCE", "DEFAULT_SOURCE", "FIELD_LENGTH", "FIELD_SCALE", "FIELD_TYPE", "FIELD_SUB_TYPE", "DESCRIPTION", "SYSTEM_FLAG",
	"SEGMENT_LENGTH", "EXTERNAL_LENGTH", "EXTERNAL_SCALE", "EXTERNAL_TYPE", "DIMENSIONS", "NULL_FLAG", "CHARACTER_LENGTH", "COLLATION_ID", "CHARACTER_SET_ID", "FIELD_PRECISION", "CHARACTER_SET_NAME", "COLLATION_NAME",
}

var sequenceResultColumns = []string{"GENERATOR_NAME", "GENERATOR_ID", "SYSTEM_FLAG"}

var indexResultColumns = []string{
	"INDEX_NAME", "RELATION_NAME", "INDEX_ID", "UNIQUE_FLAG", "DESCRIPTION", "SEGMENT_COUNT", "INDEX_INACTIVE", "INDEX_TYPE", "FOREIGN_KEY", "SYSTEM_FLAG", "EXPRESSION_SOURCE", "STATISTICS", "CONSTRAINT_NAME",
}

var indexSegmentResultColumns = []string{"INDEX_NAME", "FIELD_NAME", "FIELD_POSITION", "STATISTICS"}

var constraintResultColumns = []string{
	"CONSTRAINT_NAME", "CONSTRAINT_TYPE", "RELATION_NAME", "DEFERRABLE", "INITIALLY_DEFERRED", "INDEX_NAME", "TRIGGER_NAME", "CONST_NAME_UQ", "MATCH_OPTION", "UPDATE_RULE", "DELETE_RULE", "CHECK_TRIGGER_NAME", "CHECK_SOURCE", "REFERENCED_RELATION_NAME", "REFERENCED_INDEX_NAME",
}

var triggerResultColumns = []string{"TRIGGER_NAME", "RELATION_NAME", "TRIGGER_SEQUENCE", "TRIGGER_TYPE", "TRIGGER_SOURCE", "DESCRIPTION", "TRIGGER_INACTIVE", "SYSTEM_FLAG", "FLAGS"}

var roleResultColumns = []string{"ROLE_NAME", "OWNER_NAME"}

var dependencyResultColumns = []string{"DEPENDENT_NAME", "DEPENDENT_TYPE", "FIELD_NAME", "DEPENDED_ON_NAME", "DEPENDED_ON_TYPE"}

var functionResultColumns = []string{"FUNCTION_NAME", "FUNCTION_TYPE", "DESCRIPTION", "MODULE_NAME", "ENTRYPOINT", "RETURN_ARGUMENT", "SYSTEM_FLAG"}

var functionArgumentResultColumns = []string{"FUNCTION_NAME", "ARGUMENT_POSITION", "MECHANISM", "FIELD_LENGTH", "FIELD_SCALE", "FIELD_TYPE", "FIELD_SUB_TYPE", "CHARACTER_SET_ID", "FIELD_PRECISION", "CHARACTER_LENGTH"}

var databaseFileResultColumns = []string{"FILE_NAME", "FILE_SEQUENCE", "FILE_START", "FILE_LENGTH", "SHADOW_NUMBER"}

var shadowResultColumns = []string{"SHADOW_NUMBER", "FILE_FLAGS"}

var privilegeResultColumns = []string{"USER", "GRANTOR", "PRIVILEGE", "GRANT_OPTION", "RELATION_NAME", "FIELD_NAME", "USER_TYPE", "OBJECT_TYPE"}

func fixtureDomains() []fixtureDomain {
	return []fixtureDomain{
		{
			name: "IN_NAME",
			values: []driver.Value{"IN_NAME       ", nil, nil, nil, int64(20), int64(0), int64(37), int64(0), nil, int64(0),
				int64(0), int64(0), int64(0), int64(0), nil, int64(0), int64(20), nil, int64(4), nil, "UTF8       ", nil},
		},
		{
			name: "IN_COUNT",
			values: []driver.Value{"IN_COUNT      ", nil, nil, nil, int64(4), int64(0), int64(8), int64(0), nil, int64(0),
				int64(0), int64(0), int64(0), int64(0), nil, int64(0), nil, nil, nil, int64(10), nil, nil},
		},
		{
			name: "OUT_TOTAL",
			values: []driver.Value{"OUT_TOTAL     ", nil, nil, nil, int64(8), int64(0), int64(8), int64(0), nil, int64(0),
				int64(0), int64(0), int64(0), int64(0), nil, int64(1), nil, nil, nil, int64(10), nil, nil},
		},
		{
			name: "RDB$IMPLICIT",
			values: []driver.Value{"RDB$IMPLICIT  ", nil, nil, nil, int64(4), int64(0), int64(8), int64(0), nil, int64(0),
				int64(0), int64(0), int64(0), int64(0), nil, int64(0), nil, nil, nil, int64(10), nil, nil},
		},
	}
}

func fixtureSequences() []fixtureSequence {
	return []fixtureSequence{{
		name:   "GO_SEQUENCE",
		values: []driver.Value{"GO_SEQUENCE       ", int64(7), int64(0)},
	}}
}

func fixtureIndexes() []fixtureIndex {
	return []fixtureIndex{
		{
			name: "GO_CHILD_UQ", relationName: "GO_CHILD",
			values:   []driver.Value{"GO_CHILD_UQ       ", "GO_CHILD       ", int64(1), int64(1), nil, int64(1), int64(0), int64(0), nil, int64(0), nil, float64(0.5), "GO_CHILD_UQ_CON       "},
			segments: [][]driver.Value{{"GO_CHILD_UQ       ", "PARENT_ID       ", int64(0), float64(0.5)}},
		},
		{
			name: "GO_CHILD_FK_IDX", relationName: "GO_CHILD",
			values:   []driver.Value{"GO_CHILD_FK_IDX   ", "GO_CHILD       ", int64(2), int64(0), nil, int64(1), int64(0), int64(0), "GO_PARENT_PK_IDX   ", int64(0), nil, float64(0.5), "GO_CHILD_FK       "},
			segments: [][]driver.Value{{"GO_CHILD_FK_IDX   ", "PARENT_ID       ", int64(0), float64(0.5)}},
		},
		{
			name: "GO_PARENT_PK_IDX", relationName: "GO_PARENT",
			values:   []driver.Value{"GO_PARENT_PK_IDX  ", "GO_PARENT       ", int64(3), int64(1), nil, int64(1), int64(0), int64(0), nil, int64(0), nil, float64(0.5), "GO_PARENT_PK       "},
			segments: [][]driver.Value{{"GO_PARENT_PK_IDX  ", "ID              ", int64(0), float64(0.5)}},
		},
	}
}

func fixtureConstraints() []fixtureConstraint {
	return []fixtureConstraint{
		{
			name: "GO_CHILD_CHECK", relationName: "GO_CHILD",
			values: []driver.Value{
				"GO_CHILD_CHECK   ", "CHECK             ", "GO_CHILD       ", nil, nil, nil, nil,
				nil, nil, nil, nil, "GO_CHILD_CHECK_TRG ", "CHECK (ID > 0)  ", nil, nil,
			},
		},
		{
			name: "GO_CHILD_CHECK", relationName: "GO_CHILD",
			values: []driver.Value{
				"GO_CHILD_CHECK   ", "CHECK             ", "GO_CHILD       ", nil, nil, nil, nil,
				nil, nil, nil, nil, "GO_CHILD_CHECK_TRG ", "CHECK (ID > 0)  ", nil, nil,
			},
		},
		{
			name: "GO_CHILD_FK", relationName: "GO_CHILD",
			values: []driver.Value{
				"GO_CHILD_FK       ", "FOREIGN KEY       ", "GO_CHILD       ", nil, nil,
				"GO_CHILD_FK_IDX   ", nil, "GO_PARENT_PK      ", "FULL             ", "CASCADE          ", "CASCADE          ", nil, nil,
				"GO_PARENT       ", "GO_PARENT_PK_IDX  ",
			},
		},
		{
			name: "ORDERS_TOTAL_NN", relationName: "ORDERS",
			values: []driver.Value{
				"ORDERS_TOTAL_NN  ", "NOT NULL          ", "ORDERS          ", nil, nil,
				nil, "TOTAL           ", nil, nil, nil, nil, "UNRELATED_TRIGGER ", "CHECK (SHOULD_NOT_LEAK) ", nil, nil,
			},
		},
	}
}

func fixtureTriggers() []fixtureTrigger {
	return []fixtureTrigger{{
		name: "GO_CHILD_BI", relationName: "GO_CHILD",
		values: []driver.Value{"GO_CHILD_BI      ", "GO_CHILD       ", int64(0), int64(1), "AS\nBEGIN\n  NEW.ID = GEN_ID(GO_SEQUENCE, 1);\nEND", nil, int64(0), int64(0), int64(0)},
	}}
}

func fixtureRoles() []fixtureRole {
	return []fixtureRole{{name: "GO_SCHEMA_ROLE", values: []driver.Value{"GO_SCHEMA_ROLE    ", "SYSDBA          "}}}
}

func fixtureDependencies() []fixtureDependency {
	return []fixtureDependency{{name: "GO_CHILD", values: []driver.Value{"GO_CHILD       ", int64(0), nil, "GO_PARENT       ", int64(0)}}}
}

func fixtureFunctions() []fixtureFunction {
	return []fixtureFunction{
		{
			name:      "GO_EXTERNAL",
			values:    []driver.Value{"GO_EXTERNAL      ", int64(0), nil, "go_udf.so       ", "go_external     ", int64(0), int64(0)},
			arguments: [][]driver.Value{{"GO_EXTERNAL      ", int64(0), int64(0), int64(4), int64(0), int64(8), int64(0), nil, int64(10), nil}},
		},
		{
			// A CSTRING(80) return argument with RDB$CHARACTER_LENGTH NULL and
			// RDB$FIELD_LENGTH 80, matching every measured catalog row.
			name:      "GO_EXTERNAL_TEXT",
			values:    []driver.Value{"GO_EXTERNAL_TEXT ", int64(0), nil, "go_udf.so       ", "go_external_text", int64(0), int64(0)},
			arguments: [][]driver.Value{{"GO_EXTERNAL_TEXT ", int64(0), int64(1), int64(80), int64(0), int64(40), nil, nil, nil, nil}},
		},
	}
}

func fixtureDatabaseFiles(shadowNumber int64) [][]driver.Value {
	return [][]driver.Value{{"/tmp/go-schema.fdb", int64(0), int64(0), int64(1024), shadowNumber}}
}

func fixtureShadows() [][]driver.Value { return nil }

func fixturePrivileges() []fixtureDependency {
	return []fixtureDependency{{
		name:   "GO_SCHEMA_ROLE",
		values: []driver.Value{"GO_SCHEMA_ROLE    ", "SYSDBA          ", "S", int64(1), "GO_CHILD       ", nil, int64(13), int64(0)},
	}}
}

func fixtureRelations() []fixtureRelation {
	return []fixtureRelation{
		{
			name: "AUDIT", kind: RelationTable,
			values: []driver.Value{"AUDIT       ", int64(2), nil, nil, nil, "SYSDBA", nil, int64(8), int64(1), nil, int64(0), "PERSISTENT                     ", int64(0), int64(0)},
		},
		{
			name: "CUSTOMER_VIEW", kind: RelationView,
			values:  []driver.Value{"CUSTOMER_VIEW    ", int64(9), "SELECT  ID  FROM ORDERS\n", "view description", "SQL$VIEW", "SYSDBA", nil, int64(8), int64(4), nil, int64(0), "VIEW                          ", int64(0), int64(1)},
			columns: [][]driver.Value{fixtureViewColumn()},
		},
		{
			name: "ORDERS", kind: RelationTable,
			values:  []driver.Value{"ORDERS       ", int64(7), nil, "table description", "SQL$SEC", "SYSDBA", nil, int64(8), int64(3), nil, int64(0), "PERSISTENT                     ", int64(0), int64(0)},
			columns: [][]driver.Value{fixtureOrderIDColumn(), fixtureOrderTotalColumn()},
		},
		{
			name: "OTHER_VIEW", kind: RelationView,
			values:  []driver.Value{"OTHER_VIEW    ", int64(10), "SELECT  OTHER_ID  FROM SECOND_ORDERS\n", nil, "SQL$VIEW", "SYSDBA", nil, int64(8), int64(5), nil, int64(0), "VIEW                          ", int64(0), int64(1)},
			columns: [][]driver.Value{fixtureOtherViewColumn()},
		},
	}
}

func fixtureFullIdentifierRelations() []fixtureRelation {
	return []fixtureRelation{
		fixtureFullIdentifierRelation("IMPORT_ORDER_LINE_ITEMS", "ITEMS_ONLY_COLUMN", 20),
		fixtureFullIdentifierRelation("IMPORT_ORDER_LINE_ITEMX", "ITEMX_ONLY_COLUMN", 21),
		fixtureFullIdentifierRelation("MiXeD_Table", "MiXeD_Column", 22),
	}
}

func fixtureFullIdentifierRelation(name, columnName string, id int64) fixtureRelation {
	nameValue := name + strings.Repeat(" ", 67-len(name))
	column := append([]driver.Value(nil), fixtureOrderIDColumn()...)
	column[0] = columnName + strings.Repeat(" ", 31-len(columnName))
	column[1] = nameValue
	column[2] = "DOMAIN_" + columnName
	column[14] = "DOMAIN_" + columnName
	return fixtureRelation{
		name: name,
		kind: RelationTable,
		values: []driver.Value{
			nameValue, id, nil, nil, nil, "SYSDBA", nil, int64(8), int64(1), nil,
			int64(0), "PERSISTENT                     ", int64(0), int64(0),
		},
		columns: [][]driver.Value{column},
	}
}

func fixtureProcedures() []fixtureProcedure {
	return []fixtureProcedure{{
		name:       "PROC_TWO",
		values:     []driver.Value{"PROC_TWO      ", int64(5), int64(2), int64(1), "procedure description", "BEGIN\n  X =  1;\nEND", "SQL$PROC", "SYSDBA", int64(0)},
		parameters: [][]driver.Value{fixtureInNameParameter(), fixtureInCountParameter(), fixtureOutTotalParameter()},
	}}
}

func fixtureOrderIDColumn() []driver.Value {
	return []driver.Value{"ID       ", "ORDERS       ", "RDB$ORDER_ID       ", int64(0), int64(1), int64(0), nil, int64(0), nil, int64(1), nil, nil, nil, nil,
		"RDB$ORDER_ID       ", nil, nil, nil, int64(4), int64(0), int64(8), int64(0), nil, int64(1), int64(0), int64(0), int64(0), int64(0), nil, int64(1), nil, nil, nil, int64(10), "UTF8       ", nil, nil}
}

func fixtureOrderTotalColumn() []driver.Value {
	return []driver.Value{"TOTAL  ", "ORDERS       ", "ORDER_TOTAL       ", int64(1), int64(1), int64(1), nil, int64(0), nil, int64(0), "DEFAULT  42  ", nil, nil, nil,
		"ORDER_TOTAL       ", nil, "COMPUTED BY  A + B  ", nil, int64(8), int64(0), int64(8), int64(0), nil, int64(0), int64(0), int64(0), int64(0), int64(0), nil, int64(0), nil, nil, nil, int64(10), nil, nil, nil}
}

func fixtureViewColumn() []driver.Value {
	return []driver.Value{"ID  ", "CUSTOMER_VIEW    ", "RDB$VIEW_ID       ", int64(0), int64(1), int64(0), nil, int64(0), nil, int64(0), nil, nil, "ID   ", "ORDERS       ",
		"VIEW_ID       ", nil, "COMPUTED BY  ID  ", nil, int64(4), int64(0), int64(8), int64(0), nil, int64(0), int64(0), int64(0), int64(0), int64(0), nil, int64(0), nil, nil, nil, int64(10), nil, nil}
}

func fixtureOtherViewColumn() []driver.Value {
	return []driver.Value{"OTHER_ID  ", "OTHER_VIEW    ", "RDB$OTHER_VIEW_ID       ", int64(0), int64(1), int64(0), nil, int64(0), nil, int64(0), nil, nil, "OTHER_ID   ", "SECOND_ORDERS       ",
		"OTHER_VIEW_ID       ", nil, "COMPUTED BY  OTHER_ID  ", nil, int64(4), int64(0), int64(8), int64(0), nil, int64(0), int64(0), int64(0), int64(0), int64(0), nil, int64(0), nil, nil, nil, int64(10), nil, nil}
}

func fixtureInNameParameter() []driver.Value {
	return []driver.Value{"IN_NAME   ", "PROC_TWO      ", int64(0), int64(0), "IN_NAME       ", nil, int64(0)}
}

func fixtureInCountParameter() []driver.Value {
	return []driver.Value{"IN_COUNT  ", "PROC_TWO      ", int64(1), int64(0), "IN_COUNT      ", nil, int64(0)}
}

func fixtureOutTotalParameter() []driver.Value {
	return []driver.Value{"OUT_TOTAL ", "PROC_TWO      ", int64(0), int64(1), "OUT_TOTAL     ", nil, int64(0)}
}
