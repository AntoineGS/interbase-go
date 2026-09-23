package schema

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestExtendedCatalogFullNames(t *testing.T) {
	ctx := context.Background()
	catalog, fixture := openExtendedFullNameFixture(t)

	constraints, err := catalog.Constraints(ctx, "")
	if err != nil {
		t.Fatalf("Constraints returned error: %v", err)
	}
	if len(constraints) != 3 {
		t.Fatalf("Constraints returned %d rows, want the two colliding names and a nullable reference", len(constraints))
	}
	if constraints[0].Name != extendedConstraintAlpha || constraints[1].Name != extendedConstraintBravo {
		t.Fatalf("constraint names = %#v, want distinct full names sharing the 22-character prefix", []string{constraints[0].Name, constraints[1].Name})
	}
	if constraints[0].RelationName != extendedRelationName || !constraints[0].IndexName.Valid || constraints[0].IndexName.String != extendedIndexAlpha ||
		constraints[0].Index == nil || constraints[0].Index.Name != extendedIndexAlpha || constraints[0].Index.RelationName != extendedRelationName ||
		!constraints[0].Index.ConstraintName.Valid || constraints[0].Index.ConstraintName.String != extendedConstraintAlpha ||
		!constraints[0].Index.ForeignKey.Valid || constraints[0].Index.ForeignKey.String != extendedConstraintAlpha ||
		!reflect.DeepEqual(constraints[0].Columns, []string{"ALPHA_SEGMENT_FIELD"}) || len(constraints[0].Index.Segments) != 1 ||
		constraints[0].Index.Segments[0].IndexName != extendedIndexAlpha {
		t.Fatalf("alpha constraint index = %#v, want its own full index and segment", constraints[0])
	}
	if constraints[1].RelationName != extendedRelationName || !constraints[1].IndexName.Valid || constraints[1].IndexName.String != extendedIndexBravo ||
		constraints[1].Index == nil || constraints[1].Index.Name != extendedIndexBravo || constraints[1].Index.RelationName != extendedRelationName ||
		!constraints[1].Index.ConstraintName.Valid || constraints[1].Index.ConstraintName.String != extendedConstraintBravo ||
		!constraints[1].Index.ForeignKey.Valid || constraints[1].Index.ForeignKey.String != extendedConstraintBravo ||
		!reflect.DeepEqual(constraints[1].Columns, []string{"BRAVO_SEGMENT_FIELD"}) || len(constraints[1].Index.Segments) != 1 ||
		constraints[1].Index.Segments[0].IndexName != extendedIndexBravo {
		t.Fatalf("bravo constraint index = %#v, want its own full index and segment", constraints[1])
	}
	if constraints[2].ReferencedConstraintName.String != "MISSING_PARENT_CONSTRAINT" || constraints[2].ReferencedRelationName != "" ||
		constraints[2].ReferencedIndexName.Valid || constraints[2].PartnerConstraint != nil {
		t.Fatalf("nullable parent reference = %#v, want the absent LEFT JOIN row to remain absent", constraints[2])
	}

	if got, err := catalog.Constraint(ctx, extendedConstraintAlpha); err != nil || got == nil || got.Name != extendedConstraintAlpha {
		t.Fatalf("exact long constraint lookup = (%#v, %v), want the exact full constraint", got, err)
	}

	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		t.Fatalf("Domains returned error: %v", err)
	}
	if len(domains) != 1 || domains[0].Name != "LONG_DOMAIN_PREFIX_NAME" || domains[0].CharacterSetName.String != extendedCharacterSetName || domains[0].CollationName.String != extendedCollationName {
		t.Fatalf("domain character set/collation = %#v, want complete long names", domains)
	}

	sequences, err := catalog.Sequences(ctx, "")
	if err != nil || len(sequences) != 1 || sequences[0].Name != extendedSequenceName {
		t.Fatalf("sequence names = (%#v, %v), want the complete sequence identifier", sequences, err)
	}
	indexes, err := catalog.Indexes(ctx, "")
	if err != nil || len(indexes) != 2 || indexes[0].Name != extendedIndexAlpha || indexes[1].Name != extendedIndexBravo {
		t.Fatalf("index names = (%#v, %v), want distinct complete names", indexes, err)
	}
	triggers, err := catalog.Triggers(ctx, "")
	if err != nil || len(triggers) != 1 || triggers[0].Name != extendedTriggerName || !triggers[0].RelationName.Valid || triggers[0].RelationName.String != extendedRelationName {
		t.Fatalf("trigger identifiers = (%#v, %v), want complete trigger and relation names", triggers, err)
	}
	roles, err := catalog.Roles(ctx, "")
	if err != nil || len(roles) != 1 || roles[0].Name != extendedRoleName {
		t.Fatalf("role names = (%#v, %v), want the complete role identifier", roles, err)
	}
	dependencies, err := catalog.Dependencies(ctx, "")
	if err != nil || len(dependencies) != 1 || dependencies[0].DependentName != extendedDependentName || !dependencies[0].FieldName.Valid || dependencies[0].FieldName.String != extendedFieldName || dependencies[0].DependedOnName != extendedDependedOnName {
		t.Fatalf("dependency identifiers = (%#v, %v), want complete object and field names", dependencies, err)
	}
	functions, err := catalog.Functions(ctx, "")
	if err != nil || len(functions) != 1 || functions[0].Name != extendedFunctionName || len(functions[0].Arguments) != 1 || functions[0].Arguments[0].FunctionName != extendedFunctionName {
		t.Fatalf("function identifiers = (%#v, %v), want the full function name on the argument row", functions, err)
	}
	privileges, err := catalog.Privileges(ctx, "SYSDBA")
	if err != nil || len(privileges) != 1 || privileges[0].SubjectName != extendedRelationName || !privileges[0].FieldName.Valid || privileges[0].FieldName.String != extendedFieldName {
		t.Fatalf("privilege object identifiers = (%#v, %v), want complete subject and field names", privileges, err)
	}

	for _, expected := range []string{
		"CAST(F.RDB$FIELD_NAME AS VARCHAR(67)) AS RDB$FIELD_NAME",
		"CAST(CS.RDB$CHARACTER_SET_NAME AS VARCHAR(67)) AS RDB$CHARACTER_SET_NAME",
		"CAST(CO.RDB$COLLATION_NAME AS VARCHAR(67)) AS RDB$COLLATION_NAME",
		"CAST(G.RDB$GENERATOR_NAME AS VARCHAR(67)) AS RDB$GENERATOR_NAME",
		"CAST(I.RDB$INDEX_NAME AS VARCHAR(67)) AS RDB$INDEX_NAME",
		"CAST(I.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME",
		"CAST(I.RDB$FOREIGN_KEY AS VARCHAR(67)) AS RDB$FOREIGN_KEY",
		"CAST(RC.RDB$CONSTRAINT_NAME AS VARCHAR(31)) AS RDB$CONSTRAINT_NAME",
		"CAST(S.RDB$INDEX_NAME AS VARCHAR(67)) AS RDB$INDEX_NAME",
		"CAST(S.RDB$FIELD_NAME AS VARCHAR(67)) AS RDB$FIELD_NAME",
		"CAST(C.RDB$CONSTRAINT_NAME AS VARCHAR(31)) AS RDB$CONSTRAINT_NAME",
		"CAST(C.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME",
		"CAST(C.RDB$INDEX_NAME AS VARCHAR(67)) AS RDB$INDEX_NAME",
		"CAST(K.RDB$TRIGGER_NAME AS VARCHAR(67)) AS RDB$TRIGGER_NAME",
		"CAST(R.RDB$CONST_NAME_UQ AS VARCHAR(67)) AS RDB$CONST_NAME_UQ",
		"CAST(PC.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME",
		"CAST(PC.RDB$INDEX_NAME AS VARCHAR(67)) AS RDB$INDEX_NAME",
		"CAST(T.RDB$TRIGGER_NAME AS VARCHAR(67)) AS RDB$TRIGGER_NAME",
		"CAST(T.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME",
		"CAST(RDB$ROLE_NAME AS VARCHAR(67)) AS RDB$ROLE_NAME",
		"CAST(D.RDB$DEPENDENT_NAME AS VARCHAR(67)) AS RDB$DEPENDENT_NAME",
		"CAST(D.RDB$FIELD_NAME AS VARCHAR(67)) AS RDB$FIELD_NAME",
		"CAST(D.RDB$DEPENDED_ON_NAME AS VARCHAR(67)) AS RDB$DEPENDED_ON_NAME",
		"CAST(F.RDB$FUNCTION_NAME AS VARCHAR(67)) AS RDB$FUNCTION_NAME",
		"CAST(A.RDB$FUNCTION_NAME AS VARCHAR(67)) AS RDB$FUNCTION_NAME",
		"CAST(P.RDB$RELATION_NAME AS VARCHAR(67)) AS RDB$RELATION_NAME",
		"CAST(P.RDB$FIELD_NAME AS VARCHAR(67)) AS RDB$FIELD_NAME",
	} {
		if !fixture.hasQueryContaining(expected) {
			t.Errorf("catalog queries do not contain identifier projection %q", expected)
		}
	}
	for _, nonIdentifier := range []string{
		"CAST(RDB$OWNER_NAME AS VARCHAR(",
		"CAST(P.RDB$USER AS VARCHAR(",
		"CAST(P.RDB$GRANTOR AS VARCHAR(",
	} {
		if fixture.hasQueryContaining(nonIdentifier) {
			t.Errorf("catalog query unexpectedly changed non-identifier projection %q", nonIdentifier)
		}
	}
	constraintQuery := fixture.queryContaining("FROM RDB$RELATION_CONSTRAINTS C")
	upperConstraintQuery := strings.ToUpper(constraintQuery)
	if !strings.Contains(upperConstraintQuery, "LEFT JOIN RDB$RELATION_CONSTRAINTS PC") ||
		!strings.Contains(upperConstraintQuery, "ON PC.RDB$CONSTRAINT_NAME = R.RDB$CONST_NAME_UQ") ||
		!strings.Contains(upperConstraintQuery, "ORDER BY C.RDB$CONSTRAINT_NAME") ||
		strings.Contains(upperConstraintQuery, "COALESCE(PC.RDB$RELATION_NAME") {
		t.Fatalf("constraint query changed join/order semantics or collapsed a nullable projection: %q", constraintQuery)
	}
}

const (
	extendedConstraintAlpha  = "LONG_CONSTRAINT_PREFIX_ALPHA"
	extendedConstraintBravo  = "LONG_CONSTRAINT_PREFIX_BRAVO"
	extendedIndexAlpha       = "LONG_INDEX_PREFIX_ALPHA"
	extendedIndexBravo       = "LONG_INDEX_PREFIX_BRAVO"
	extendedRelationName     = "LONG_RELATION_PREFIX_TABLE"
	extendedFieldName        = "LONG_FIELD_PREFIX_VALUE"
	extendedCharacterSetName = "LONG_CHARACTER_SET_NAME"
	extendedCollationName    = "LONG_COLLATION_NAME"
	extendedSequenceName     = "LONG_SEQUENCE_PREFIX_GEN"
	extendedTriggerName      = "LONG_TRIGGER_PREFIX_UPDATE"
	extendedRoleName         = "LONG_ROLE_PREFIX_ACCOUNT"
	extendedDependentName    = "LONG_DEPENDENT_PREFIX_VIEW"
	extendedDependedOnName   = "LONG_DEPENDED_PREFIX_TABLE"
	extendedFunctionName     = "LONG_FUNCTION_PREFIX_CALC"
)

type extendedFullNameFixture struct {
	queries []string
}

func openExtendedFullNameFixture(t *testing.T) (*Catalog, *extendedFullNameFixture) {
	t.Helper()
	fixture := &extendedFullNameFixture{}
	db := sql.OpenDB(extendedFullNameConnector{fixture: fixture})
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = db.Close() })
	return New(db), fixture
}

func (f *extendedFullNameFixture) hasQueryContaining(needle string) bool {
	needle = strings.ToUpper(needle)
	for _, query := range f.queries {
		if strings.Contains(strings.ToUpper(query), needle) {
			return true
		}
	}
	return false
}

func (f *extendedFullNameFixture) queryContaining(needle string) string {
	needle = strings.ToUpper(needle)
	for _, query := range f.queries {
		if strings.Contains(strings.ToUpper(query), needle) {
			return query
		}
	}
	return ""
}

type extendedFullNameConnector struct {
	fixture *extendedFullNameFixture
}

func (c extendedFullNameConnector) Connect(context.Context) (driver.Conn, error) {
	return extendedFullNameConn{fixture: c.fixture}, nil
}

func (c extendedFullNameConnector) Driver() driver.Driver { return extendedFullNameDriver{} }

type extendedFullNameDriver struct{}

func (extendedFullNameDriver) Open(string) (driver.Conn, error) {
	return nil, errors.New("extended catalog fixture requires Connector")
}

type extendedFullNameConn struct {
	fixture *extendedFullNameFixture
}

func (extendedFullNameConn) Prepare(string) (driver.Stmt, error) { return nil, driver.ErrSkip }
func (extendedFullNameConn) Close() error                        { return nil }
func (extendedFullNameConn) Begin() (driver.Tx, error)           { return nil, driver.ErrSkip }

func (c extendedFullNameConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	c.fixture.queries = append(c.fixture.queries, query)
	upper := strings.ToUpper(query)
	switch {
	case strings.Contains(upper, "SELECT F.RDB$FIELD_LENGTH") && strings.Contains(upper, "FROM RDB$RELATION_FIELDS RF"):
		return extendedFixtureRows([]string{"FIELD_LENGTH"}, [][]driver.Value{{int64(67)}}), nil
	case strings.HasPrefix(strings.TrimSpace(upper), "SELECT CAST(RF.RDB$RELATION_NAME AS VARCHAR("):
		return extendedFixtureRows([]string{"RELATION_NAME", "FIELD_NAME", "FIELD_LENGTH"}, extendedIdentifierWidthRows()), nil
	case strings.Contains(upper, "FROM RDB$FIELDS F"):
		row := []driver.Value{
			projectExtendedIdentifier(query, "F.RDB$FIELD_NAME", "LONG_DOMAIN_PREFIX_NAME"),
			nil, nil, nil, int64(20), int64(0), int64(37), int64(0), nil, int64(0),
			int64(0), int64(0), int64(0), int64(0), nil, int64(0), int64(20), nil, int64(4), nil,
			projectExtendedIdentifier(query, "CS.RDB$CHARACTER_SET_NAME", extendedCharacterSetName),
			projectExtendedIdentifier(query, "CO.RDB$COLLATION_NAME", extendedCollationName),
		}
		return extendedFixtureRows(domainResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$GENERATORS G"):
		row := []driver.Value{projectExtendedIdentifier(query, "G.RDB$GENERATOR_NAME", extendedSequenceName), int64(1), int64(0)}
		return extendedFixtureRows(sequenceResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$INDICES I"):
		filter := extendedFixtureFilter(args)
		values := make([][]driver.Value, 0, 2)
		for _, data := range []struct {
			name       string
			constraint string
			foreignKey string
			field      string
		}{
			{extendedIndexAlpha, extendedConstraintAlpha, extendedConstraintAlpha, "ALPHA_SEGMENT_FIELD"},
			{extendedIndexBravo, extendedConstraintBravo, extendedConstraintBravo, "BRAVO_SEGMENT_FIELD"},
		} {
			if filter != "" && filter != data.name {
				continue
			}
			values = append(values, []driver.Value{
				projectExtendedIdentifier(query, "I.RDB$INDEX_NAME", data.name),
				projectExtendedIdentifier(query, "I.RDB$RELATION_NAME", extendedRelationName),
				int64(1), int64(1), nil, int64(1), int64(0), int64(0),
				projectExtendedIdentifier(query, "I.RDB$FOREIGN_KEY", data.foreignKey), int64(0), nil, float64(0.5),
				projectExtendedIdentifier(query, "RC.RDB$CONSTRAINT_NAME", data.constraint),
			})
		}
		return extendedFixtureRows(indexResultColumns, values), nil
	case strings.Contains(upper, "FROM RDB$INDEX_SEGMENTS S"):
		filter := extendedFixtureFilter(args)
		rows := make([][]driver.Value, 0, 1)
		for _, segment := range []struct{ index, field string }{
			{extendedIndexAlpha, "ALPHA_SEGMENT_FIELD"},
			{extendedIndexBravo, "BRAVO_SEGMENT_FIELD"},
		} {
			if filter == segment.index {
				rows = append(rows, []driver.Value{
					projectExtendedIdentifier(query, "S.RDB$INDEX_NAME", segment.index),
					projectExtendedIdentifier(query, "S.RDB$FIELD_NAME", segment.field),
					int64(0), float64(0.5),
				})
			}
		}
		return extendedFixtureRows(indexSegmentResultColumns, rows), nil
	case strings.Contains(upper, "FROM RDB$RELATION_CONSTRAINTS C"):
		filter := extendedFixtureFilter(args)
		values := [][]driver.Value{
			extendedConstraintRow(query, extendedConstraintAlpha, extendedIndexAlpha, "", nil),
			extendedConstraintRow(query, extendedConstraintBravo, extendedIndexBravo, "MISSING_PARENT_CONSTRAINT", nil),
			extendedConstraintRow(query, "MISSING_PARENT_CONSTRAINT", "", "MISSING_PARENT_CONSTRAINT", nil),
		}
		if filter != "" {
			filtered := values[:0]
			for _, row := range values {
				if row[0] == filter {
					filtered = append(filtered, row)
				}
			}
			values = filtered
		}
		return extendedFixtureRows(constraintResultColumns, values), nil
	case strings.Contains(upper, "FROM RDB$TRIGGERS T"):
		row := []driver.Value{
			projectExtendedIdentifier(query, "T.RDB$TRIGGER_NAME", extendedTriggerName),
			projectExtendedIdentifier(query, "T.RDB$RELATION_NAME", extendedRelationName),
			int64(1), int64(1), "AS BEGIN END", nil, int64(0), int64(0), int64(0),
		}
		return extendedFixtureRows(triggerResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$ROLES"):
		row := []driver.Value{projectExtendedIdentifier(query, "RDB$ROLE_NAME", extendedRoleName), "SYSDBA"}
		return extendedFixtureRows(roleResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$DEPENDENCIES D"):
		row := []driver.Value{
			projectExtendedIdentifier(query, "D.RDB$DEPENDENT_NAME", extendedDependentName), int64(1),
			projectExtendedIdentifier(query, "D.RDB$FIELD_NAME", extendedFieldName),
			projectExtendedIdentifier(query, "D.RDB$DEPENDED_ON_NAME", extendedDependedOnName), int64(0),
		}
		return extendedFixtureRows(dependencyResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$FUNCTION_ARGUMENTS A"):
		row := []driver.Value{
			projectExtendedIdentifier(query, "A.RDB$FUNCTION_NAME", extendedFunctionName),
			int64(0), int64(0), int64(4), int64(0), int64(8), int64(0), nil, int64(10), nil,
		}
		return extendedFixtureRows(functionArgumentResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$FUNCTIONS F"):
		row := []driver.Value{
			projectExtendedIdentifier(query, "F.RDB$FUNCTION_NAME", extendedFunctionName), int64(0), nil,
			"module", "entry", int64(0), int64(0),
		}
		return extendedFixtureRows(functionResultColumns, [][]driver.Value{row}), nil
	case strings.Contains(upper, "FROM RDB$USER_PRIVILEGES P"):
		row := []driver.Value{
			"SYSDBA", "SYSDBA", "S", int64(0),
			projectExtendedIdentifier(query, "P.RDB$RELATION_NAME", extendedRelationName),
			projectExtendedIdentifier(query, "P.RDB$FIELD_NAME", extendedFieldName), int64(8), int64(0),
		}
		return extendedFixtureRows(privilegeResultColumns, [][]driver.Value{row}), nil
	default:
		return nil, errors.New("extended catalog fixture: unexpected query: " + query)
	}
}

func extendedIdentifierWidthRows() [][]driver.Value {
	fields := []identifierField{
		{"RDB$FIELDS", "RDB$FIELD_NAME"},
		{"RDB$CHARACTER_SETS", "RDB$CHARACTER_SET_NAME"},
		{"RDB$COLLATIONS", "RDB$COLLATION_NAME"},
		{"RDB$GENERATORS", "RDB$GENERATOR_NAME"},
		{"RDB$INDICES", "RDB$INDEX_NAME"}, {"RDB$INDICES", "RDB$RELATION_NAME"}, {"RDB$INDICES", "RDB$FOREIGN_KEY"},
		{"RDB$INDEX_SEGMENTS", "RDB$INDEX_NAME"}, {"RDB$INDEX_SEGMENTS", "RDB$FIELD_NAME"},
		{"RDB$RELATION_CONSTRAINTS", "RDB$CONSTRAINT_NAME"}, {"RDB$RELATION_CONSTRAINTS", "RDB$RELATION_NAME"}, {"RDB$RELATION_CONSTRAINTS", "RDB$INDEX_NAME"},
		{"RDB$REF_CONSTRAINTS", "RDB$CONST_NAME_UQ"}, {"RDB$CHECK_CONSTRAINTS", "RDB$TRIGGER_NAME"},
		{"RDB$TRIGGERS", "RDB$TRIGGER_NAME"}, {"RDB$TRIGGERS", "RDB$RELATION_NAME"},
		{"RDB$ROLES", "RDB$ROLE_NAME"},
		{"RDB$DEPENDENCIES", "RDB$DEPENDENT_NAME"}, {"RDB$DEPENDENCIES", "RDB$FIELD_NAME"}, {"RDB$DEPENDENCIES", "RDB$DEPENDED_ON_NAME"},
		{"RDB$FUNCTIONS", "RDB$FUNCTION_NAME"}, {"RDB$FUNCTION_ARGUMENTS", "RDB$FUNCTION_NAME"},
		{"RDB$USER_PRIVILEGES", "RDB$RELATION_NAME"}, {"RDB$USER_PRIVILEGES", "RDB$FIELD_NAME"},
	}
	rows := make([][]driver.Value, 0, len(fields))
	for _, field := range fields {
		width := int64(67)
		if field.relation == "RDB$RELATION_CONSTRAINTS" && field.field == "RDB$CONSTRAINT_NAME" {
			width = 31
		}
		rows = append(rows, []driver.Value{field.relation, field.field, width})
	}
	return rows
}

func extendedFixtureRows(columns []string, values [][]driver.Value) driver.Rows {
	return &extendedFullNameRows{columns: columns, values: values}
}

type extendedFullNameRows struct {
	columns []string
	values  [][]driver.Value
	index   int
}

func (r *extendedFullNameRows) Columns() []string { return r.columns }
func (r *extendedFullNameRows) Close() error      { return nil }
func (r *extendedFullNameRows) Next(dest []driver.Value) error {
	if r.index >= len(r.values) {
		return io.EOF
	}
	copy(dest, r.values[r.index])
	r.index++
	return nil
}

func extendedFixtureFilter(args []driver.NamedValue) string {
	if len(args) == 0 {
		return ""
	}
	value, _ := args[0].Value.(string)
	return value
}

func projectExtendedIdentifier(query, ref, value string) driver.Value {
	if value == "" {
		return nil
	}
	cast := "CAST(" + strings.ToUpper(ref) + " AS VARCHAR("
	if strings.Contains(strings.ToUpper(query), cast) || len(value) <= 22 {
		return value
	}
	return value[:22]
}

func extendedConstraintRow(query, name, indexName, referencedConstraint string, referencedRelation driver.Value) []driver.Value {
	var indexValue driver.Value
	if indexName != "" {
		indexValue = projectExtendedIdentifier(query, "C.RDB$INDEX_NAME", indexName)
	}
	var referencedName, referencedIndex driver.Value
	if referencedConstraint != "" {
		referencedName = projectExtendedIdentifier(query, "R.RDB$CONST_NAME_UQ", referencedConstraint)
	}
	return []driver.Value{
		projectExtendedIdentifier(query, "C.RDB$CONSTRAINT_NAME", name), "UNIQUE",
		projectExtendedIdentifier(query, "C.RDB$RELATION_NAME", extendedRelationName), nil, nil, indexValue,
		nil, referencedName, nil, nil, nil, nil, nil, referencedRelation, referencedIndex,
	}
}

func TestCatalogReadsExtendedSchemaFamilies(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)
	ctx := context.Background()

	domains, err := catalog.Domains(ctx, "")
	if err != nil {
		t.Fatalf("Domains returned error: %v", err)
	}
	if names := domainNames(domains); !reflect.DeepEqual(names, []string{"IN_NAME", "IN_COUNT", "OUT_TOTAL"}) {
		t.Fatalf("domain names = %#v, want user domains in catalog order", names)
	}
	if domains[0].Name != "IN_NAME" || !domains[0].FieldType.Valid {
		t.Fatalf("domain metadata = %#v, want typed field metadata", domains[0])
	}

	sequences, err := catalog.Sequences(ctx, "GO_SEQUENCE")
	if err != nil {
		t.Fatalf("Sequences returned error: %v", err)
	}
	if len(sequences) != 1 || sequences[0].Name != "GO_SEQUENCE" {
		t.Fatalf("sequence result = %#v, want exact sequence", sequences)
	}

	indexes, err := catalog.Indexes(ctx, "GO_CHILD_UQ")
	if err != nil {
		t.Fatalf("Indexes returned error: %v", err)
	}
	if len(indexes) != 1 || indexes[0].RelationName != "GO_CHILD" {
		t.Fatalf("index result = %#v, want exact index and relation", indexes)
	}
	if names := indexSegmentNames(indexes[0].Segments); !reflect.DeepEqual(names, []string{"PARENT_ID"}) {
		t.Fatalf("index segments = %#v, want ordered segment names", names)
	}

	constraints, err := catalog.Constraints(ctx, "GO_CHILD_FK")
	if err != nil {
		t.Fatalf("Constraints returned error: %v", err)
	}
	if len(constraints) != 1 || constraints[0].ConstraintType != "FOREIGN KEY" {
		t.Fatalf("constraint result = %#v, want foreign-key metadata", constraints)
	}
	if constraints[0].ReferencedRelationName != "GO_PARENT" ||
		!reflect.DeepEqual(constraints[0].ReferencedColumns, []string{"ID"}) {
		t.Fatalf("foreign-key target = %#v, want scoped referenced relation and columns", constraints[0])
	}

	triggers, err := catalog.Triggers(ctx, "GO_CHILD_BI")
	if err != nil {
		t.Fatalf("Triggers returned error: %v", err)
	}
	if len(triggers) != 1 || triggers[0].RelationName.String != "GO_CHILD" || !triggers[0].Source.Valid {
		t.Fatalf("trigger result = %#v, want relation and exact source", triggers)
	}

	roles, err := catalog.Roles(ctx, "GO_SCHEMA_ROLE")
	if err != nil {
		t.Fatalf("Roles returned error: %v", err)
	}
	if len(roles) != 1 || roles[0].Name != "GO_SCHEMA_ROLE" {
		t.Fatalf("role result = %#v, want exact role", roles)
	}

	dependencies, err := catalog.Dependencies(ctx, "GO_CHILD")
	if err != nil {
		t.Fatalf("Dependencies returned error: %v", err)
	}
	if len(dependencies) == 0 || dependencies[0].DependentName != "GO_CHILD" {
		t.Fatalf("dependency result = %#v, want dependent-name filter", dependencies)
	}

	functions, err := catalog.Functions(ctx, "GO_EXTERNAL")
	if err != nil {
		t.Fatalf("Functions returned error: %v", err)
	}
	if len(functions) != 1 || functions[0].Name != "GO_EXTERNAL" {
		t.Fatalf("function result = %#v, want external-function metadata", functions)
	}
	if len(functions[0].Arguments) != 1 || functions[0].Arguments[0].Position.Int64 != 0 {
		t.Fatalf("function arguments = %#v, want argument metadata without invocation", functions[0].Arguments)
	}

	files, err := catalog.DatabaseFiles(ctx)
	if err != nil {
		t.Fatalf("DatabaseFiles returned error: %v", err)
	}
	if len(files) != 1 || !files[0].FileName.Valid {
		t.Fatalf("database files = %#v, want current database file metadata", files)
	}

	shadows, err := catalog.Shadows(ctx)
	if err != nil {
		t.Fatalf("Shadows returned error: %v", err)
	}
	if len(shadows) != 0 {
		t.Fatalf("shadows = %#v, want empty fixture result", shadows)
	}

	privileges, err := catalog.Privileges(ctx, "GO_SCHEMA_ROLE")
	if err != nil {
		t.Fatalf("Privileges returned error: %v", err)
	}
	if len(privileges) != 1 || privileges[0].Grantee != "GO_SCHEMA_ROLE" || privileges[0].PrivilegeCode != "S" {
		t.Fatalf("privilege result = %#v, want exact grantee and code", privileges)
	}
}

func TestCatalogTableLoadsDDLMetadata(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()

	table, err := New(db).Table(context.Background(), "ORDERS")
	if err != nil {
		t.Fatalf("Table returned error: %v", err)
	}
	if table == nil {
		t.Fatal("Table returned nil")
	}
	if !table.ConstraintsLoaded || !table.IndexesLoaded || !table.TriggersLoaded {
		t.Fatalf("table metadata loaded flags = (constraints=%t, indexes=%t, triggers=%t), want all true",
			table.ConstraintsLoaded, table.IndexesLoaded, table.TriggersLoaded)
	}

	indexes, err := New(db).IndexesForRelation(context.Background(), "GO_CHILD")
	if err != nil {
		t.Fatalf("IndexesForRelation returned error: %v", err)
	}
	if len(indexes) != 2 {
		t.Fatalf("IndexesForRelation returned %d indexes, want the two GO_CHILD indexes", len(indexes))
	}
	constraints, err := New(db).ConstraintsForRelation(context.Background(), "GO_CHILD")
	if err != nil {
		t.Fatalf("ConstraintsForRelation returned error: %v", err)
	}
	if len(constraints) != 2 {
		t.Fatalf("ConstraintsForRelation returned %d constraints, want one CHECK and one FOREIGN KEY", len(constraints))
	}
	triggers, err := New(db).TriggersForRelation(context.Background(), "GO_CHILD")
	if err != nil {
		t.Fatalf("TriggersForRelation returned error: %v", err)
	}
	if len(triggers) != 1 || triggers[0].Name != "GO_CHILD_BI" {
		t.Fatalf("TriggersForRelation returned %#v, want GO_CHILD_BI", triggers)
	}
}

func TestCatalogDeduplicatesCheckConstraints(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()

	constraints, err := New(db).Constraints(context.Background(), "")
	if err != nil {
		t.Fatalf("Constraints returned error: %v", err)
	}
	var checks []Constraint
	for _, constraint := range constraints {
		if constraint.Name == "GO_CHILD_CHECK" {
			checks = append(checks, constraint)
		}
	}
	if len(checks) != 1 {
		t.Fatalf("check constraints = %#v, want one logical constraint", checks)
	}
	if !reflect.DeepEqual(checks[0].TriggerNames, []string{"GO_CHILD_CHECK_TRG"}) {
		t.Fatalf("check constraint trigger names = %#v, want one trigger", checks[0].TriggerNames)
	}
}

func TestCatalogReadsNotNullColumnNameFromConstraintTriggerName(t *testing.T) {
	db := openFixtureDB(t)
	defer db.Close()

	constraints, err := New(db).ConstraintsForRelation(context.Background(), "ORDERS")
	if err != nil {
		t.Fatalf("ConstraintsForRelation returned error: %v", err)
	}
	var notNull *Constraint
	for index := range constraints {
		if constraints[index].ConstraintType == string(ConstraintNotNull) {
			notNull = &constraints[index]
			break
		}
	}
	if notNull == nil {
		t.Fatalf("constraints = %#v, want a NOT NULL constraint", constraints)
	}
	if !notNull.ColumnName.Valid || notNull.ColumnName.String != "TOTAL" {
		t.Fatalf("NOT NULL column name = %#v, want TOTAL", notNull.ColumnName)
	}
	if notNull.TriggerName.Valid {
		t.Fatalf("NOT NULL trigger name = %#v, want invalid", notNull.TriggerName)
	}
	if !reflect.DeepEqual(notNull.TriggerNames, []string(nil)) {
		t.Fatalf("NOT NULL trigger names = %#v, want none", notNull.TriggerNames)
	}
	if notNull.CheckSource.Valid {
		t.Fatalf("NOT NULL check source = %#v, want invalid", notNull.CheckSource)
	}
}

func TestCatalogExtendedFiltersAreBoundAndUseOfficialCatalogs(t *testing.T) {
	db, state := openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	const maliciousName = `GO_CHILD' OR 1=1 --`

	if _, err := New(db).Indexes(context.Background(), maliciousName); err != nil {
		t.Fatalf("malicious index filter returned error: %v", err)
	}
	calls := state.callsSnapshot()
	indexQuery := fixtureCallQuery(t, calls, "indexes")
	if strings.Contains(indexQuery, maliciousName) {
		t.Fatalf("index filter was interpolated into query: %q", indexQuery)
	}
	indexCall := fixtureCallForKind(t, calls, "indexes")
	if len(indexCall.args) != 1 || indexCall.args[0].Value != maliciousName {
		t.Fatalf("index filter args = %#v, want one bound argument", indexCall.args)
	}

	db, state = openFixtureDBWithOptions(t, fixtureOptions{})
	defer db.Close()
	if _, err := New(db).Constraints(context.Background(), "GO_CHILD"); err != nil {
		t.Fatalf("constraint filter returned error: %v", err)
	}
	constraintQuery := fixtureCallQuery(t, state.callsSnapshot(), "constraints")
	upper := strings.ToUpper(constraintQuery)
	for _, clause := range []string{
		"FROM RDB$RELATION_CONSTRAINTS",
		"LEFT JOIN RDB$REF_CONSTRAINTS",
		"LEFT JOIN RDB$CHECK_CONSTRAINTS",
		"RC.RDB$CONSTRAINT_TYPE = 'CHECK'",
		"ORDER BY",
	} {
		if !strings.Contains(upper, clause) {
			t.Fatalf("constraint query = %q, want official catalog clause %q", constraintQuery, clause)
		}
	}
}

func TestDDLGenerationQuotesIdentifiersAndPreservesSupportedClauses(t *testing.T) {
	domain := Domain{
		Name:             `Weird"Domain`,
		FieldType:        sql.NullInt64{Int64: 37, Valid: true},
		CharacterLength:  sql.NullInt64{Int64: 20, Valid: true},
		CharacterSetName: sql.NullString{String: "UTF8", Valid: true},
		CollationID:      sql.NullInt64{Int64: 1, Valid: true},
		CollationName:    sql.NullString{String: `WEIRD"COLLATION`, Valid: true},
		DefaultSource:    sql.NullString{String: "DEFAULT  42  ", Valid: true},
		ValidationSource: sql.NullString{String: "CHECK (VALUE > 0)  ", Valid: true},
	}
	got, err := domain.GenerateDDL()
	if err != nil {
		t.Fatalf("domain GenerateDDL returned error: %v", err)
	}
	want := `CREATE DOMAIN "Weird""Domain" AS VARCHAR(20) CHARACTER SET "UTF8" DEFAULT  42   CHECK (VALUE > 0)   COLLATE "WEIRD""COLLATION"`
	if got != want {
		t.Fatalf("domain DDL = %q, want %q", got, want)
	}

	table := Relation{
		Name:              `Order"Table`,
		Kind:              RelationTable,
		ConstraintsLoaded: true,
		IndexesLoaded:     true,
		TriggersLoaded:    true,
		Columns: []Column{{
			Name:     `Order"ID`,
			Domain:   &Domain{FieldType: sql.NullInt64{Int64: 8, Valid: true}},
			Nullable: sql.NullBool{Bool: false, Valid: true},
		}},
		Constraints: []Constraint{{
			Name:           `Order"PK`,
			RelationName:   `Order"Table`,
			ConstraintType: "PRIMARY KEY",
			Columns:        []string{`Order"ID`},
		}},
	}
	got, err = table.GenerateDDL()
	if err != nil {
		t.Fatalf("table GenerateDDL returned error: %v", err)
	}
	if !strings.Contains(got, `CREATE TABLE "Order""Table"`) ||
		!strings.Contains(got, `CONSTRAINT "Order""PK" PRIMARY KEY ("Order""ID")`) {
		t.Fatalf("table DDL = %q, want quoted table and primary key", got)
	}

	view := Relation{
		Name:       "Order View",
		Kind:       RelationView,
		ViewSource: sql.NullString{String: "SELECT 1  ", Valid: true},
		Columns:    []Column{{Name: "Value"}},
	}
	got, err = view.GenerateDDL()
	if err != nil {
		t.Fatalf("view GenerateDDL returned error: %v", err)
	}
	if want := `CREATE VIEW "Order View" ("Value") AS SELECT 1  `; got != want {
		t.Fatalf("view DDL = %q, want %q", got, want)
	}

	procedure := Procedure{
		Name:        "Do Work",
		Source:      sql.NullString{String: "BEGIN\n  SUSPEND;\nEND", Valid: true},
		InputCount:  sql.NullInt64{Int64: 1, Valid: true},
		OutputCount: sql.NullInt64{Int64: 1, Valid: true},
		InputParameters: []ProcedureParameter{{
			Name:          "Amount",
			Number:        sql.NullInt64{Int64: 0, Valid: true},
			ParameterType: sql.NullInt64{Int64: 0, Valid: true},
			FieldSource:   sql.NullString{String: "RDB$AMOUNT", Valid: true},
			Domain:        &Domain{FieldType: sql.NullInt64{Int64: 8, Valid: true}},
			Direction:     ParameterInput,
			Nullable:      sql.NullBool{Bool: true, Valid: true},
		}},
		OutputParameters: []ProcedureParameter{{
			Name:          "Result",
			Number:        sql.NullInt64{Int64: 0, Valid: true},
			ParameterType: sql.NullInt64{Int64: 1, Valid: true},
			FieldSource:   sql.NullString{String: "RDB$RESULT", Valid: true},
			Domain:        &Domain{FieldType: sql.NullInt64{Int64: 8, Valid: true}},
			Direction:     ParameterOutput,
			Nullable:      sql.NullBool{Bool: true, Valid: true},
		}},
	}
	got, err = procedure.GenerateDDL()
	if err != nil {
		t.Fatalf("procedure GenerateDDL returned error: %v", err)
	}
	if !strings.Contains(got, `CREATE PROCEDURE "Do Work"`) ||
		!strings.Contains(got, `"Amount" INTEGER`) ||
		!strings.Contains(got, `RETURNS ("Result" INTEGER)`) ||
		!strings.HasSuffix(got, procedure.Source.String) {
		t.Fatalf("procedure DDL = %q, want quoted parameters and exact source", got)
	}

	trigger := Trigger{
		Name:         "Order Trigger",
		RelationName: sql.NullString{String: `Order"Table`, Valid: true},
		TriggerType:  sql.NullInt64{Int64: 1, Valid: true},
		Sequence:     sql.NullInt64{Int64: 3, Valid: true},
		Source:       sql.NullString{String: "AS\nBEGIN\n  POST_EVENT 'x';\nEND", Valid: true},
	}
	got, err = trigger.GenerateDDL()
	if err != nil {
		t.Fatalf("trigger GenerateDDL returned error: %v", err)
	}
	if !strings.Contains(got, `CREATE TRIGGER "Order Trigger" FOR "Order""Table" ACTIVE BEFORE INSERT POSITION 3`) ||
		!strings.HasSuffix(got, trigger.Source.String) {
		t.Fatalf("trigger DDL = %q, want decoded event and exact source", got)
	}

	sequence := Sequence{Name: `Seq"Name`}
	got, err = sequence.GenerateDDL()
	if err != nil || got != `CREATE GENERATOR "Seq""Name"` {
		t.Fatalf("sequence DDL = (%q, %v), want quoted create", got, err)
	}

	role := Role{Name: `Role"Name`}
	got, err = role.GenerateDDL()
	if err != nil || got != `CREATE ROLE "Role""Name"` {
		t.Fatalf("role DDL = (%q, %v), want quoted create", got, err)
	}

	privilege := Privilege{
		Grantee:       `User"Name`,
		Grantor:       "SYSDBA",
		PrivilegeCode: "S",
		SubjectName:   `Order"Table`,
		FieldName:     sql.NullString{String: "Order ID", Valid: true},
		GranteeType:   sql.NullInt64{Int64: 8, Valid: true},
		SubjectType:   sql.NullInt64{Int64: 0, Valid: true},
		GrantOption:   sql.NullInt64{Int64: 1, Valid: true},
	}
	got, err = privilege.GenerateDDL()
	if err != nil || got != `GRANT SELECT ("Order ID") ON "Order""Table" TO "User""Name" WITH GRANT OPTION` {
		t.Fatalf("privilege DDL = (%q, %v), want quoted grant", got, err)
	}
}

func TestDDLReportsUnsupportedMetadataInsteadOfSilentlyDroppingIt(t *testing.T) {
	function := Function{Name: "GO_EXTERNAL"}
	if _, err := function.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("function GenerateDDL error = %v, want ErrUnsupportedDDL", err)
	}

	table := Relation{Name: "INCOMPLETE", Kind: RelationTable}
	if _, err := table.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("incomplete table GenerateDDL error = %v, want ErrUnsupportedDDL", err)
	}

	domain := Domain{Name: "ARRAY_DOMAIN", FieldType: sql.NullInt64{Int64: 8, Valid: true}, Dimensions: sql.NullInt64{Int64: 1, Valid: true}}
	if _, err := domain.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("array domain GenerateDDL error = %v, want ErrUnsupportedDDL", err)
	}
}

func TestDDLUsesTokenBoundariesAndCanonicalClauseOrder(t *testing.T) {
	domain := Domain{
		Name:             "ORDERED_DOMAIN",
		FieldType:        sql.NullInt64{Int64: fieldTypeVarchar, Valid: true},
		CharacterLength:  sql.NullInt64{Int64: 20, Valid: true},
		CharacterSetID:   sql.NullInt64{Int64: 4, Valid: true},
		CharacterSetName: sql.NullString{String: "UTF8", Valid: true},
		CollationID:      sql.NullInt64{Int64: 1, Valid: true},
		CollationName:    sql.NullString{String: "UTF8", Valid: true},
		DefaultSource:    sql.NullString{String: "DEFAULT\t'unknown'", Valid: true},
		NullFlag:         sql.NullInt64{Int64: 1, Valid: true},
		ValidationSource: sql.NullString{String: "CHECK(\nVALUE <> ''\n)", Valid: true},
	}
	ddl, err := domain.GenerateDDL()
	if err != nil {
		t.Fatalf("domain GenerateDDL returned error: %v", err)
	}
	upper := strings.ToUpper(ddl)
	if strings.Count(upper, "DEFAULT") != 1 || strings.Count(upper, "CHECK") != 1 {
		t.Fatalf("domain DDL = %q, want token-aware DEFAULT/CHECK clauses without duplication", ddl)
	}
	for _, pair := range [][2]string{{"DEFAULT", "NOT NULL"}, {"NOT NULL", "CHECK"}, {"CHECK", "COLLATE"}} {
		if strings.Index(upper, pair[0]) >= strings.Index(upper, pair[1]) {
			t.Fatalf("domain DDL = %q, want %s before %s", ddl, pair[0], pair[1])
		}
	}

	table := Relation{
		Name:              "ORDERED_TABLE",
		Kind:              RelationTable,
		ConstraintsLoaded: true,
		Columns: []Column{{
			Name:          "TEXT_VALUE",
			RelationName:  "ORDERED_TABLE",
			FieldSource:   sql.NullString{String: "RDB$TEXT", Valid: true},
			Domain:        &Domain{Name: "RDB$TEXT", SystemFlag: sql.NullInt64{Int64: 1, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeVarchar, Valid: true}, CharacterLength: sql.NullInt64{Int64: 20, Valid: true}, CharacterSetID: sql.NullInt64{Int64: 4, Valid: true}, CharacterSetName: sql.NullString{String: "UTF8", Valid: true}},
			DefaultSource: sql.NullString{String: "DEFAULT\n'v'", Valid: true},
			Nullable:      sql.NullBool{Bool: false, Valid: true},
			CollationID:   sql.NullInt64{Int64: 1, Valid: true},
			CollationName: sql.NullString{String: "UTF8", Valid: true},
		}},
	}
	tableDDL, err := table.GenerateDDL()
	if err != nil {
		t.Fatalf("table GenerateDDL returned error: %v", err)
	}
	columnUpper := strings.ToUpper(tableDDL)
	for _, pair := range [][2]string{{"DEFAULT", "NOT NULL"}, {"NOT NULL", "COLLATE"}} {
		if strings.Index(columnUpper, pair[0]) >= strings.Index(columnUpper, pair[1]) {
			t.Fatalf("table DDL = %q, want %s before %s", tableDDL, pair[0], pair[1])
		}
	}
}

func TestTableDDLPreservesNamedNotNullConstraintInline(t *testing.T) {
	table := Relation{
		Name:              "NAMED_NOT_NULL",
		Kind:              RelationTable,
		ConstraintsLoaded: true,
		Columns: []Column{{
			Name:         "VALUE",
			RelationName: "NAMED_NOT_NULL",
			Domain:       &Domain{Name: "RDB$VALUE", SystemFlag: sql.NullInt64{Int64: 1, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}},
		}},
		Constraints: []Constraint{{
			Name:           "NAMED_NOT_NULL_VALUE",
			ConstraintType: string(ConstraintNotNull),
			RelationName:   "NAMED_NOT_NULL",
			ColumnName:     sql.NullString{String: "VALUE", Valid: true},
		}},
	}

	ddl, err := table.GenerateDDL()
	if err != nil {
		t.Fatalf("table GenerateDDL returned error: %v", err)
	}
	if !strings.Contains(ddl, `"VALUE" INTEGER CONSTRAINT "NAMED_NOT_NULL_VALUE" NOT NULL`) {
		t.Fatalf("table DDL = %q, want named NOT NULL inline on VALUE", ddl)
	}
}

func TestDDLUsesExactCatalogEnumValuesAndPreservesSourceCase(t *testing.T) {
	table := Relation{
		Name:              "GTT_TABLE",
		Kind:              RelationTable,
		RelationType:      sql.NullString{String: "GLOBAL_TEMPORARY_PRESERVE_ROWS", Valid: true},
		ConstraintsLoaded: true,
		Columns: []Column{{
			Name:   "VALUE",
			Domain: &Domain{FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}},
		}},
	}
	ddl, err := table.GenerateDDL()
	if err != nil {
		t.Fatalf("GTT GenerateDDL returned error: %v", err)
	}
	if !strings.HasPrefix(ddl, `CREATE GLOBAL TEMPORARY TABLE "GTT_TABLE"`) || !strings.HasSuffix(ddl, "ON COMMIT PRESERVE ROWS") {
		t.Fatalf("GTT DDL = %q, want exact valid relation type rendering", ddl)
	}

	invalid := table
	invalid.RelationType = sql.NullString{String: "GLOBAL_TEMPORARY_UNKNOWN", Valid: true}
	if _, err := invalid.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("invalid relation type error = %v, want ErrUnsupportedDDL", err)
	}

	if _, err := constraintDefinition(Constraint{
		Name:           "LOWERCASE_CHECK",
		ConstraintType: "check",
		CheckSource:    sql.NullString{String: "CHECK (VALUE > 0)", Valid: true},
	}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("lowercase constraint type error = %v, want exact enum rejection", err)
	}

	domain := Domain{
		Name:          "CASE_SOURCE",
		FieldType:     sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
		DefaultSource: sql.NullString{String: "default  7", Valid: true},
	}
	if ddl, err := domain.GenerateDDL(); err != nil || !strings.Contains(ddl, " default  7") {
		t.Fatalf("source case DDL = (%q, %v), want original source case preserved", ddl, err)
	}
}

func TestDDLRejectsAmbiguousAndLossyTypeMetadata(t *testing.T) {
	tests := []struct {
		name   string
		domain Domain
	}{
		{
			name: "scaled double without numeric metadata",
			domain: Domain{
				Name:       "SCALED_DOUBLE",
				FieldType:  sql.NullInt64{Int64: fieldTypeDouble, Valid: true},
				FieldScale: sql.NullInt64{Int64: -2, Valid: true},
			},
		},
		{
			name: "multibyte character length missing",
			domain: Domain{
				Name:             "UTF8_BYTES_ONLY",
				FieldType:        sql.NullInt64{Int64: fieldTypeVarchar, Valid: true},
				FieldLength:      sql.NullInt64{Int64: 40, Valid: true},
				CharacterSetID:   sql.NullInt64{Int64: 4, Valid: true},
				CharacterSetName: sql.NullString{String: "UTF8", Valid: true},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.domain.SQLType(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("SQLType error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	validScaled := Domain{
		Name:           "SCALED_DOUBLE",
		FieldType:      sql.NullInt64{Int64: fieldTypeDouble, Valid: true},
		FieldSubType:   sql.NullInt64{Int64: 1, Valid: true},
		FieldPrecision: sql.NullInt64{Int64: 15, Valid: true},
		FieldScale:     sql.NullInt64{Int64: -2, Valid: true},
	}
	if got, err := validScaled.SQLType(); err != nil || got != "NUMERIC(15, 2)" {
		t.Fatalf("scaled DOUBLE SQLType = (%q, %v), want NUMERIC(15, 2)", got, err)
	}

	computed := Relation{
		Name:              "COMPUTED_TABLE",
		Kind:              RelationTable,
		ConstraintsLoaded: true,
		Columns: []Column{{
			Name:           "COMPUTED_VALUE",
			RelationName:   "COMPUTED_TABLE",
			FieldSource:    sql.NullString{String: "RDB$COMPUTED", Valid: true},
			ComputedSource: sql.NullString{String: "COMPUTED\tBY VALUE + 1", Valid: true},
			Domain:         &Domain{Name: "RDB$COMPUTED", SystemFlag: sql.NullInt64{Int64: 1, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}},
		}},
	}
	if _, err := computed.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("computed column DDL error = %v, want ErrUnsupportedDDL", err)
	}
}

func TestDialect1CanonicalScaledDoubleDDLAndDialect3Compatibility(t *testing.T) {
	legacy := Domain{Name: "AMOUNT_DOMAIN", FieldType: sql.NullInt64{Int64: fieldTypeDouble, Valid: true}, FieldScale: sql.NullInt64{Int64: -2, Valid: true}}
	if _, err := legacy.SQLType(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("default SQLType error = %v, want Dialect 3-compatible refusal", err)
	}
	got, err := legacy.SQLTypeWithOptions(DDLOptions{Dialect: 1})
	if err != nil || got != "NUMERIC(15, 2)" {
		t.Fatalf("Dialect 1 SQLTypeWithOptions = (%q, %v), want canonical NUMERIC(15, 2)", got, err)
	}
	ddl, err := legacy.GenerateDDLWithOptions(DDLOptions{Dialect: 1})
	if err != nil || !strings.Contains(ddl, "CREATE DOMAIN AMOUNT_DOMAIN AS NUMERIC(15, 2)") {
		t.Fatalf("Dialect 1 domain DDL = (%q, %v), want unquoted canonical declaration", ddl, err)
	}
	if ddl, err := legacy.GenerateDDL(); err == nil || !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("default GenerateDDL = (%q, %v), want preserved Dialect 3 refusal", ddl, err)
	}
	legacy.FieldSubType = sql.NullInt64{Int64: 0, Valid: true}
	if got, err := legacy.SQLTypeWithOptions(DDLOptions{Dialect: 1}); err != nil || got != "NUMERIC(15, 2)" {
		t.Fatalf("zero subtype = (%q, %v), want canonical numeric", got, err)
	}
	legacy.FieldScale = sql.NullInt64{Int64: -16, Valid: true}
	if _, err := legacy.SQLTypeWithOptions(DDLOptions{Dialect: 1}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("scale 16 SQLType error = %v, want unsupported", err)
	}
	legacy.FieldScale = sql.NullInt64{Int64: -1 << 63, Valid: true}
	if _, err := legacy.SQLTypeWithOptions(DDLOptions{Dialect: 1}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("overflow scale SQLType error = %v, want unsupported", err)
	}
	legacy.FieldScale = sql.NullInt64{Int64: -2, Valid: true}
	legacy.FieldSubType = sql.NullInt64{Int64: 1, Valid: true}
	legacy.FieldPrecision = sql.NullInt64{Int64: 16, Valid: true}
	if got, err := legacy.SQLTypeWithOptions(DDLOptions{Dialect: 1}); err != nil || got != "NUMERIC(16, 2)" {
		t.Fatalf("Dialect 1 precision 16 SQLType=(%q,%v), want valid recorded precision", got, err)
	}
	plainDouble := Domain{Name: "PLAIN_DOUBLE", FieldType: sql.NullInt64{Int64: fieldTypeDouble, Valid: true}, FieldScale: sql.NullInt64{Int64: 0, Valid: true}}
	if got, err := plainDouble.SQLTypeWithOptions(DDLOptions{Dialect: 1}); err != nil || got != "DOUBLE PRECISION" {
		t.Fatalf("plain DOUBLE = (%q, %v), want unchanged DOUBLE PRECISION", got, err)
	}
}

func TestNumericScaleRangesRemainValidThroughPrecisionEighteen(t *testing.T) {
	domain := Domain{
		FieldType:      sql.NullInt64{Int64: fieldTypeBigint, Valid: true},
		FieldSubType:   sql.NullInt64{Int64: 1, Valid: true},
		FieldPrecision: sql.NullInt64{Int64: 18, Valid: true},
		FieldScale:     sql.NullInt64{Int64: -18, Valid: true},
	}
	got, err := domain.SQLTypeWithOptions(DDLOptions{Dialect: Dialect3})
	if err != nil || got != "NUMERIC(18, 18)" {
		t.Fatalf("D3 max-scale type=(%q,%v), want NUMERIC(18, 18)", got, err)
	}
	domain.FieldScale = sql.NullInt64{Int64: -1 << 63, Valid: true}
	if _, err := domain.SQLTypeWithOptions(DDLOptions{Dialect: Dialect3}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("min-int scale error=%v, want graceful unsupported error", err)
	}
	domain.FieldType = sql.NullInt64{Int64: fieldTypeBigint, Valid: true}
	domain.FieldPrecision = sql.NullInt64{Int64: 18, Valid: true}
	domain.FieldScale = sql.NullInt64{Int64: -2, Valid: true}
	if _, err := domain.SQLTypeWithOptions(DDLOptions{Dialect: Dialect1}); !errors.Is(err, ErrUnsupportedDDL) || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("Dialect 1 exact-int precision above nine error=%v, want explicit storage mismatch", err)
	}
	domain.FieldSubType = sql.NullInt64{}
	if _, err := domain.SQLTypeWithOptions(DDLOptions{Dialect: Dialect1}); !errors.Is(err, ErrUnsupportedDDL) || !strings.Contains(err.Error(), "storage") {
		t.Fatalf("Dialect 1 inferred BIGINT storage error=%v, want explicit storage mismatch", err)
	}
	for _, test := range []struct{ fieldType, precision int64 }{
		{fieldTypeSmallint, 5}, {fieldTypeInteger, 4}, {fieldTypeDouble, 9},
	} {
		domain.FieldType = sql.NullInt64{Int64: test.fieldType, Valid: true}
		domain.FieldSubType = sql.NullInt64{Int64: 1, Valid: true}
		domain.FieldPrecision = sql.NullInt64{Int64: test.precision, Valid: true}
		if _, err := domain.SQLTypeWithOptions(DDLOptions{Dialect: Dialect1}); !errors.Is(err, ErrUnsupportedDDL) || !strings.Contains(err.Error(), "storage") {
			t.Errorf("Dialect 1 type=%d precision=%d error=%v, want explicit storage mismatch", test.fieldType, test.precision, err)
		}
	}
}

func TestDialect1RenderingAppliesIdentifierPolicyWithoutRewritingSource(t *testing.T) {
	domain := Domain{Name: "GOOD_DOMAIN1", FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}, DefaultSource: sql.NullString{String: "DEFAULT 'a\"b'", Valid: true}}
	got, err := domain.GenerateDDLWithOptions(DDLOptions{Dialect: 1})
	if err != nil || got != "CREATE DOMAIN GOOD_DOMAIN1 AS INTEGER DEFAULT 'a\"b'" {
		t.Fatalf("Dialect 1 DDL = (%q, %v), want identifier unquoted and source preserved", got, err)
	}
	domain.Name = strings.Repeat("A", 40)
	if got, err := domain.GenerateDDLWithOptions(DDLOptions{Dialect: 1}); err != nil || !strings.Contains(got, domain.Name) {
		t.Fatalf("Dialect 1 rejected catalog-width identifier: ddl=(%q,%v)", got, err)
	}
	domain.Name = "LOWERCASE"
	if _, err := domain.GenerateDDLWithOptions(DDLOptions{Dialect: 1}); err != nil {
		t.Fatalf("uppercase representable identifier rejected: %v", err)
	}
	domain.Name = "SELECT"
	if _, err := domain.GenerateDDLWithOptions(DDLOptions{Dialect: 1}); err == nil {
		t.Fatal("Dialect 1 accepted a reserved word as an unquoted identifier")
	}
	domain.Name = "éName"
	if got, err := domain.GenerateDDLWithOptions(DDLOptions{Dialect: 3}); err != nil || got != `CREATE DOMAIN "éName" AS INTEGER DEFAULT 'a"b'` {
		t.Fatalf("Dialect 3 Unicode DDL = (%q, %v), want quoted exact identifier", got, err)
	}
}

func TestDialect1TableNamesConstraintsAndLegacyInlineProvenance(t *testing.T) {
	table := Relation{Name: "PAYMENT", Kind: RelationTable, ConstraintsLoaded: true,
		Columns:     []Column{{Name: "AMOUNT", Domain: &Domain{FieldType: sql.NullInt64{Int64: fieldTypeDouble, Valid: true}, FieldScale: sql.NullInt64{Int64: -2, Valid: true}}, Nullable: sql.NullBool{Bool: false, Valid: true}, DefaultSource: sql.NullString{String: "DEFAULT  1.25", Valid: true}}},
		Constraints: []Constraint{{Name: "PAYMENT_PK", ConstraintType: string(ConstraintPrimaryKey), Columns: []string{"AMOUNT"}}},
	}
	ddl, err := table.GenerateDDLWithOptions(DDLOptions{Dialect: 1})
	if err != nil {
		t.Fatal(err)
	}
	for _, fragment := range []string{"CREATE TABLE PAYMENT", "AMOUNT NUMERIC(15, 2) /* legacy scaled DOUBLE", "*/ DEFAULT  1.25 NOT NULL", "CONSTRAINT PAYMENT_PK PRIMARY KEY (AMOUNT)"} {
		if !strings.Contains(ddl, fragment) {
			t.Errorf("Dialect 1 table DDL missing %q:\n%s", fragment, ddl)
		}
	}
	if strings.Contains(ddl, `"PAYMENT"`) {
		t.Errorf("Dialect 1 table DDL quoted identifiers:\n%s", ddl)
	}
}

func TestDialect1ReferencesUseRendererAndSameDialectSourcesRemainVerbatim(t *testing.T) {
	domain := Domain{Name: "GOOD_DOMAIN", FieldType: sql.NullInt64{Int64: fieldTypeVarchar, Valid: true}, CharacterLength: sql.NullInt64{Int64: 20, Valid: true}, CharacterSetName: sql.NullString{String: "UTF8", Valid: true}, CollationID: sql.NullInt64{Int64: 1, Valid: true}, CollationName: sql.NullString{String: "UTF8", Valid: true}}
	ddl, err := domain.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1})
	if err != nil || ddl != "CREATE DOMAIN GOOD_DOMAIN AS VARCHAR(20) CHARACTER SET UTF8 COLLATE UTF8" {
		t.Fatalf("Dialect 1 charset/collation DDL = (%q, %v)", ddl, err)
	}
	dateOnly := Domain{FieldType: sql.NullInt64{Int64: fieldTypeDate, Valid: true}}
	if _, err := dateOnly.SQLTypeWithOptions(DDLOptions{Dialect: Dialect1}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("Dialect 1 DATE-only error = %v, want semantic refusal", err)
	}
	legacyDate := Domain{FieldType: sql.NullInt64{Int64: fieldTypeTimestamp, Valid: true}}
	if got, err := legacyDate.SQLTypeWithOptions(DDLOptions{Dialect: Dialect1}); err != nil || got != "DATE" {
		t.Fatalf("Dialect 1 legacy DATE type=(%q,%v), want DATE preserving timestamp semantics", got, err)
	}
	timeOnly := Domain{FieldType: sql.NullInt64{Int64: fieldTypeTime, Valid: true}}
	if _, err := timeOnly.SQLTypeWithOptions(DDLOptions{Dialect: Dialect1}); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("Dialect 1 TIME error=%v, want semantic refusal", err)
	}
	view := Relation{Name: "VIEW_NAME", Kind: RelationView, ViewSource: sql.NullString{String: "SELECT 1", Valid: true}, Columns: []Column{{Name: "VALUE"}}}
	if ddl, err := view.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1}); err != nil || !strings.HasSuffix(ddl, view.ViewSource.String) {
		t.Fatalf("Dialect 1 same-source view DDL=(%q,%v), want unchanged view source", ddl, err)
	}
	index := Index{Name: "COMPUTED_INDEX", RelationName: "TABLE_NAME", UniqueFlag: sql.NullInt64{Int64: 0, Valid: true}, IndexType: sql.NullInt64{Int64: 0, Valid: true}, Expression: sql.NullString{String: "COMPUTED BY VALUE + 1", Valid: true}}
	if ddl, err := index.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1}); err != nil || !strings.Contains(ddl, index.Expression.String) {
		t.Fatalf("Dialect 1 same-source expression index DDL=(%q,%v)", ddl, err)
	}
	procedure := Procedure{Name: "PROC_NAME", InputCount: sql.NullInt64{Int64: 0, Valid: true}, OutputCount: sql.NullInt64{Int64: 0, Valid: true}, Source: sql.NullString{String: `AS BEGIN POST_EVENT 'same-dialect'; END`, Valid: true}}
	if ddl, err := procedure.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1}); err != nil || !strings.HasSuffix(ddl, procedure.Source.String) {
		t.Fatalf("Dialect 1 same-source procedure DDL=(%q,%v)", ddl, err)
	}
	trigger := Trigger{Name: "TRIGGER_NAME", TriggerType: sql.NullInt64{Int64: 1, Valid: true}, Sequence: sql.NullInt64{Int64: 0, Valid: true}, Source: sql.NullString{String: `AS BEGIN POST_EVENT 'same-dialect'; END`, Valid: true}}
	if ddl, err := trigger.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1}); err != nil || !strings.HasSuffix(ddl, trigger.Source.String) {
		t.Fatalf("Dialect 1 same-source trigger DDL=(%q,%v)", ddl, err)
	}
}

func TestProcedureDDLDoesNotDuplicateLeadingSourceWhitespace(t *testing.T) {
	procedure := Procedure{Name: "SOURCE_PROC", InputCount: sql.NullInt64{Int64: 0, Valid: true}, OutputCount: sql.NullInt64{Int64: 0, Valid: true}, Source: sql.NullString{String: " BEGIN POST_EVENT 'kept'; END", Valid: true}}
	ddl, err := procedure.GenerateDDLWithOptions(DDLOptions{Dialect: Dialect1})
	if err != nil {
		t.Fatal(err)
	}
	want := "CREATE PROCEDURE SOURCE_PROC AS" + procedure.Source.String
	if ddl != want {
		t.Fatalf("procedure DDL=%q, want exact delimiter+source %q", ddl, want)
	}
}

func TestProcedureDDLRequiresExactCountsAndCompleteParameters(t *testing.T) {
	base := Procedure{
		Name:        "COUNTED_PROCEDURE",
		Source:      sql.NullString{String: "BEGIN END", Valid: true},
		InputCount:  sql.NullInt64{Int64: 1, Valid: true},
		OutputCount: sql.NullInt64{Int64: 0, Valid: true},
		InputParameters: []ProcedureParameter{{
			Name:          "INPUT_VALUE",
			ProcedureName: "COUNTED_PROCEDURE",
			Number:        sql.NullInt64{Int64: 0, Valid: true},
			Direction:     ParameterInput,
			ParameterType: sql.NullInt64{Int64: 0, Valid: true},
			FieldSource:   sql.NullString{String: "RDB$INPUT", Valid: true},
			Domain:        &Domain{Name: "RDB$INPUT", SystemFlag: sql.NullInt64{Int64: 1, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}},
			Nullable:      sql.NullBool{Bool: true, Valid: true},
		}},
	}
	if _, err := base.GenerateDDL(); err != nil {
		t.Fatalf("complete procedure GenerateDDL returned error: %v", err)
	}

	for _, test := range []struct {
		name   string
		mutate func(*Procedure)
	}{
		{name: "input count mismatch", mutate: func(procedure *Procedure) { procedure.InputCount.Int64 = 2 }},
		{name: "input count unknown", mutate: func(procedure *Procedure) { procedure.InputCount.Valid = false }},
		{name: "parameter position unknown", mutate: func(procedure *Procedure) { procedure.InputParameters[0].Number.Valid = false }},
		{name: "parameter domain unknown", mutate: func(procedure *Procedure) { procedure.InputParameters[0].Domain = nil }},
		{name: "parameter nullability unknown", mutate: func(procedure *Procedure) { procedure.InputParameters[0].Nullable = sql.NullBool{} }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := base
			candidate.InputParameters = append([]ProcedureParameter(nil), base.InputParameters...)
			test.mutate(&candidate)
			if _, err := candidate.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("GenerateDDL error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}
}

func TestPrivilegeDDLHonorsGranteeAndSubjectTypes(t *testing.T) {
	tests := []struct {
		name      string
		privilege Privilege
		want      string
	}{
		{
			name:      "trigger grantee",
			privilege: Privilege{Grantee: "TRIGGER_GRANTEE", GranteeType: sql.NullInt64{Int64: 2, Valid: true}, PrivilegeCode: "S", SubjectName: "TABLE_NAME", SubjectType: sql.NullInt64{Int64: 0, Valid: true}},
			want:      `GRANT SELECT ON "TABLE_NAME" TO TRIGGER "TRIGGER_GRANTEE"`,
		},
		{
			name:      "view subject and procedure grantee",
			privilege: Privilege{Grantee: "PROC_GRANTEE", GranteeType: sql.NullInt64{Int64: 5, Valid: true}, PrivilegeCode: "S", SubjectName: "VIEW_NAME", SubjectType: sql.NullInt64{Int64: 1, Valid: true}},
			want:      `GRANT SELECT ON VIEW "VIEW_NAME" TO PROCEDURE "PROC_GRANTEE"`,
		},
		{
			name:      "role membership",
			privilege: Privilege{Grantee: "USER_NAME", GranteeType: sql.NullInt64{Int64: 8, Valid: true}, PrivilegeCode: "M", SubjectName: "ROLE_NAME", SubjectType: sql.NullInt64{Int64: 13, Valid: true}},
			want:      `GRANT "ROLE_NAME" TO "USER_NAME"`,
		},
		{
			name:      "role grantee",
			privilege: Privilege{Grantee: "ROLE_NAME", GranteeType: sql.NullInt64{Int64: 13, Valid: true}, PrivilegeCode: "S", SubjectName: "TABLE_NAME", SubjectType: sql.NullInt64{Int64: 0, Valid: true}},
			want:      `GRANT SELECT ON "TABLE_NAME" TO "ROLE_NAME"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.privilege.GenerateDDL()
			if err != nil || got != test.want {
				t.Fatalf("GenerateDDL = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}

	unknown := Privilege{Grantee: "UNKNOWN", GranteeType: sql.NullInt64{Int64: 99, Valid: true}, PrivilegeCode: "S", SubjectName: "TABLE", SubjectType: sql.NullInt64{Int64: 0, Valid: true}}
	if _, err := unknown.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("unknown grantee type error = %v, want ErrUnsupportedDDL", err)
	}
}

func TestTriggerEventDecodesAllDMLOperationsWithoutOverwriting(t *testing.T) {
	got, err := triggerEvent(17) // BEFORE INSERT OR UPDATE
	if err != nil {
		t.Fatalf("triggerEvent returned error: %v", err)
	}
	if got != "BEFORE INSERT OR UPDATE" {
		t.Fatalf("triggerEvent(17) = %q, want BEFORE INSERT OR UPDATE", got)
	}

	if _, err := triggerEvent(1 << 7); err == nil {
		t.Fatal("triggerEvent accepted a code with unsupported reserved bits")
	}

	trigger := Trigger{
		Name:         "MULTI_EVENT_TRIGGER",
		RelationName: sql.NullString{String: "TABLE_NAME", Valid: true},
		TriggerType:  sql.NullInt64{Int64: 17, Valid: true},
		Sequence:     sql.NullInt64{Int64: 0, Valid: true},
		Source:       sql.NullString{String: "AS BEGIN END", Valid: true},
	}
	if _, err := trigger.GenerateDDL(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("multi-event trigger GenerateDDL error = %v, want ErrUnsupportedDDL", err)
	}
}

func TestInactiveIndexExposesExecutableStatements(t *testing.T) {
	index := Index{
		Name:         "INACTIVE_INDEX",
		RelationName: "TABLE_NAME",
		UniqueFlag:   sql.NullInt64{Int64: 1, Valid: true},
		Inactive:     sql.NullInt64{Int64: 1, Valid: true},
		IndexType:    sql.NullInt64{Int64: 1, Valid: true},
		Segments:     []IndexSegment{{FieldName: "VALUE"}},
	}
	want := []string{
		`CREATE UNIQUE DESCENDING INDEX "INACTIVE_INDEX" ON "TABLE_NAME" ("VALUE")`,
		`ALTER INDEX "INACTIVE_INDEX" INACTIVE`,
	}
	got, err := index.Statements()
	if err != nil {
		t.Fatalf("Statements() returned error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Statements() = %#v, want %#v", got, want)
	}

	invalid := index
	invalid.IndexType = sql.NullInt64{Int64: 2, Valid: true}
	if _, err := invalid.Statements(); !errors.Is(err, ErrUnsupportedDDL) {
		t.Fatalf("invalid Statements() error = %v, want ErrUnsupportedDDL", err)
	}
}

func domainNames(domains []Domain) []string {
	names := make([]string, 0, len(domains))
	for _, domain := range domains {
		names = append(names, domain.Name)
	}
	return names
}

func TestCharacterSetClauseSharesOneGuardBody(t *testing.T) {
	quoted, err := characterSetClause("domain", "D", sql.NullString{String: `WEIRD"SET`, Valid: true}, sql.NullInt64{Int64: 4, Valid: true})
	if err != nil || quoted != `"WEIRD""SET"` {
		t.Fatalf("named character set = (%q, %v), want quoted name", quoted, err)
	}

	if quoted, err := characterSetClause("domain", "D", sql.NullString{String: "   ", Valid: true}, sql.NullInt64{}); err != nil || quoted != "" {
		t.Fatalf("blank name with no id = (%q, %v), want empty clause", quoted, err)
	}

	if quoted, err := characterSetClause("domain", "D", sql.NullString{}, sql.NullInt64{Int64: 0, Valid: true}); err != nil || quoted != "" {
		t.Fatalf("zero character set id = (%q, %v), want empty clause", quoted, err)
	}

	_, err = characterSetClause("function argument", "F_0", sql.NullString{}, sql.NullInt64{Int64: 4, Valid: true})
	var unsupported *UnsupportedDDLError
	if !errors.As(err, &unsupported) {
		t.Fatalf("missing name with non-zero id error = %v, want *UnsupportedDDLError", err)
	}
	if unsupported.Object != "function argument" || unsupported.Name != "F_0" || unsupported.Feature != "character set name is unavailable" {
		t.Fatalf("unsupported error = %#v, want caller-supplied object, name, and unavailable-name feature", unsupported)
	}
}

func TestTriggerEventExportsDecodedCatalogEvent(t *testing.T) {
	rendered := []struct {
		name        string
		triggerType int64
		want        string
	}{
		{name: "before insert", triggerType: 1, want: "BEFORE INSERT"},
		{name: "after insert", triggerType: 2, want: "AFTER INSERT"},
		{name: "multi event", triggerType: 17, want: "BEFORE INSERT OR UPDATE"},
		{name: "database connect", triggerType: 1 << 13, want: "ON CONNECT"},
		{name: "transaction rollback", triggerType: (1 << 13) + 4, want: "ON TRANSACTION ROLLBACK"},
	}
	for _, test := range rendered {
		t.Run(test.name, func(t *testing.T) {
			trigger := Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: test.triggerType, Valid: true}}
			got, err := trigger.Event()
			if err != nil || got != test.want {
				t.Fatalf("Event = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}

	unsupported := []struct {
		name    string
		trigger Trigger
	}{
		{name: "null trigger type", trigger: Trigger{Name: "EVENT_TRIGGER"}},
		{name: "no operation", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 0, Valid: true}}},
		{name: "reserved bits", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 1 << 7, Valid: true}}},
		{name: "negative code", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: -1, Valid: true}}},
		{name: "reserved mask two", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 2 << 13, Valid: true}}},
		{name: "reserved mask three", trigger: Trigger{Name: "EVENT_TRIGGER", TriggerType: sql.NullInt64{Int64: 3 << 13, Valid: true}}},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.trigger.Event(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("Event error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	// Drift guard: GenerateDDL and Event must keep decoding through the same
	// body, so every single-event string Event returns has to appear verbatim
	// in the generated statement.
	for _, test := range rendered[:2] {
		t.Run("ddl agrees on "+test.name, func(t *testing.T) {
			trigger := Trigger{
				Name:         "EVENT_TRIGGER",
				RelationName: sql.NullString{String: "GO_CHILD", Valid: true},
				TriggerType:  sql.NullInt64{Int64: test.triggerType, Valid: true},
				Sequence:     sql.NullInt64{Int64: 0, Valid: true},
				Source:       sql.NullString{String: "AS BEGIN END", Valid: true},
			}
			event, err := trigger.Event()
			if err != nil {
				t.Fatalf("Event returned error: %v", err)
			}
			ddl, err := trigger.GenerateDDL()
			if err != nil {
				t.Fatalf("GenerateDDL returned error: %v", err)
			}
			if !strings.Contains(ddl, event) {
				t.Fatalf("GenerateDDL = %q, want it to contain Event result %q", ddl, event)
			}
		})
	}

	db := openFixtureDB(t)
	defer db.Close()
	triggers, err := New(db).Triggers(context.Background(), "GO_CHILD_BI")
	if err != nil {
		t.Fatalf("Triggers returned error: %v", err)
	}
	if len(triggers) != 1 {
		t.Fatalf("triggers = %#v, want exactly one scanned trigger", triggers)
	}
	if got, err := triggers[0].Event(); err != nil || got != "BEFORE INSERT" {
		t.Fatalf("scanned trigger Event = (%q, %v), want (\"BEFORE INSERT\", nil)", got, err)
	}
}

func TestFunctionArgumentSQLTypeRendersExternalDeclarations(t *testing.T) {
	rendered := []struct {
		name     string
		argument FunctionArgument
		want     string
	}{
		{
			name: "integer",
			argument: FunctionArgument{
				Name:           "F_INT_0",
				FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
				FieldSubType:   sql.NullInt64{Int64: 0, Valid: true},
				FieldScale:     sql.NullInt64{Int64: 0, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 4, Valid: true},
				FieldPrecision: sql.NullInt64{Int64: 10, Valid: true},
			},
			want: "INTEGER",
		},
		{
			name: "numeric from subtype and scale",
			argument: FunctionArgument{
				Name:           "F_NUM_0",
				FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
				FieldSubType:   sql.NullInt64{Int64: 1, Valid: true},
				FieldScale:     sql.NullInt64{Int64: -2, Valid: true},
				FieldPrecision: sql.NullInt64{Int64: 9, Valid: true},
			},
			want: "NUMERIC(9, 2)",
		},
		{
			// The shape every measured CSTRING row has: RDB$CHARACTER_LENGTH
			// NULL, RDB$FIELD_LENGTH carrying the declared length verbatim.
			name: "cstring from field length",
			argument: FunctionArgument{
				Name:        "F_LEFT_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 80, Valid: true},
			},
			want: "CSTRING(80)",
		},
		{
			name: "cstring at the observed maximum",
			argument: FunctionArgument{
				Name:        "F_BIGSTRINGREPLACE_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 32000, Valid: true},
			},
			want: "CSTRING(32000)",
		},
		{
			name: "cstring at the observed minimum",
			argument: FunctionArgument{
				Name:        "F_CRLF_0",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 3, Valid: true},
			},
			want: "CSTRING(3)",
		},
		{
			// BLOB arguments carry RDB$FIELD_LENGTH 0; the BLOB branch reads
			// no length, so that is harmless.
			name: "blob without subtype",
			argument: FunctionArgument{
				Name:        "F_BLOB_0",
				FieldType:   sql.NullInt64{Int64: fieldTypeBlob, Valid: true},
				FieldLength: sql.NullInt64{Int64: 0, Valid: true},
			},
			want: "BLOB",
		},
		{
			name: "text blob",
			argument: FunctionArgument{
				Name:         "F_BLOB_1",
				FieldType:    sql.NullInt64{Int64: fieldTypeBlob, Valid: true},
				FieldSubType: sql.NullInt64{Int64: 1, Valid: true},
				FieldLength:  sql.NullInt64{Int64: 0, Valid: true},
			},
			want: "BLOB SUB_TYPE TEXT",
		},
	}
	for _, test := range rendered {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.argument.SQLType()
			if err != nil || got != test.want {
				t.Fatalf("SQLType = (%q, %v), want (%q, nil)", got, err, test.want)
			}
		})
	}

	unsupported := []struct {
		name     string
		argument FunctionArgument
	}{
		{
			name:     "null field type",
			argument: FunctionArgument{Name: "F_NULL_0"},
		},
		{
			name:     "unknown field type",
			argument: FunctionArgument{Name: "F_UNKNOWN_0", FieldType: sql.NullInt64{Int64: 99, Valid: true}},
		},
		{
			name:     "quad internal type",
			argument: FunctionArgument{Name: "F_QUAD_0", FieldType: sql.NullInt64{Int64: 9, Valid: true}},
		},
		{
			name:     "cstring with null length",
			argument: FunctionArgument{Name: "F_CSTR_0", FieldType: sql.NullInt64{Int64: fieldTypeCString, Valid: true}},
		},
		{
			name: "cstring with zero length",
			argument: FunctionArgument{
				Name:        "F_CSTR_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 0, Valid: true},
			},
		},
		{
			name: "cstring under a non-default character set",
			argument: FunctionArgument{
				Name:           "F_CSTR_2",
				FieldType:      sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 80, Valid: true},
				CharacterSetID: sql.NullInt64{Int64: 4, Valid: true},
			},
		},
		{
			name: "char under a non-default character set",
			argument: FunctionArgument{
				Name:            "F_CHAR_0",
				FieldType:       sql.NullInt64{Int64: fieldTypeChar, Valid: true},
				FieldLength:     sql.NullInt64{Int64: 40, Valid: true},
				CharacterLength: sql.NullInt64{Int64: 10, Valid: true},
				CharacterSetID:  sql.NullInt64{Int64: 4, Valid: true},
			},
		},
		{
			name: "blob under a non-default character set",
			argument: FunctionArgument{
				Name:           "F_BLOB_2",
				FieldType:      sql.NullInt64{Int64: fieldTypeBlob, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 0, Valid: true},
				CharacterSetID: sql.NullInt64{Int64: 4, Valid: true},
			},
		},
		{
			// The real-world CHAR case: RDB$CHARACTER_LENGTH is NULL for every
			// measured function argument, so CHAR and VARCHAR arguments are
			// expected to refuse. Do not substitute FieldLength here.
			name: "char without a character length",
			argument: FunctionArgument{
				Name:        "F_CHAR_1",
				FieldType:   sql.NullInt64{Int64: fieldTypeChar, Valid: true},
				FieldLength: sql.NullInt64{Int64: 40, Valid: true},
			},
		},
		{
			name: "varchar without a character length",
			argument: FunctionArgument{
				Name:        "F_VARCHAR_0",
				FieldType:   sql.NullInt64{Int64: fieldTypeVarchar, Valid: true},
				FieldLength: sql.NullInt64{Int64: 40, Valid: true},
			},
		},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.argument.SQLType(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("SQLType error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	// The delegated renderer must not leak the synthetic domain into the
	// caller's error.
	_, err := FunctionArgument{Name: "F_NULL_0"}.SQLType()
	var relabelled *UnsupportedDDLError
	if !errors.As(err, &relabelled) {
		t.Fatalf("delegated error = %v, want *UnsupportedDDLError", err)
	}
	if relabelled.Object != "function argument" || relabelled.Name != "F_NULL_0" {
		t.Fatalf("delegated error = %#v, want the function argument object and name", relabelled)
	}
	if relabelled.Feature != "field type is NULL" {
		t.Fatalf("delegated error feature = %q, want the underlying renderer reason", relabelled.Feature)
	}
}

func TestFunctionReturnTypeResolvesByArgumentPosition(t *testing.T) {
	denseReturn := Function{
		Name:           "GO_DENSE",
		ReturnArgument: sql.NullInt64{Int64: 0, Valid: true},
		Arguments: []FunctionArgument{{
			Name:           "GO_DENSE_0",
			Position:       sql.NullInt64{Int64: 0, Valid: true},
			FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
			FieldSubType:   sql.NullInt64{Int64: 0, Valid: true},
			FieldScale:     sql.NullInt64{Int64: 0, Valid: true},
			FieldLength:    sql.NullInt64{Int64: 4, Valid: true},
			FieldPrecision: sql.NullInt64{Int64: 10, Valid: true},
		}},
	}
	if got, err := denseReturn.ReturnType(); err != nil || got != "INTEGER" {
		t.Fatalf("position-0 ReturnType = (%q, %v), want (\"INTEGER\", nil)", got, err)
	}

	// RDB$RETURN_ARGUMENT is a position, not a slice index. Positions here are
	// 1 and 3, so indexing Arguments by ReturnArgument would panic or resolve
	// the wrong row; argument 3 is simultaneously an input and the return.
	sparseReturn := Function{
		Name:           "GO_SPARSE",
		ReturnArgument: sql.NullInt64{Int64: 3, Valid: true},
		Arguments: []FunctionArgument{
			{
				Name:           "GO_SPARSE_1",
				Position:       sql.NullInt64{Int64: 1, Valid: true},
				FieldType:      sql.NullInt64{Int64: fieldTypeInteger, Valid: true},
				FieldSubType:   sql.NullInt64{Int64: 0, Valid: true},
				FieldScale:     sql.NullInt64{Int64: 0, Valid: true},
				FieldLength:    sql.NullInt64{Int64: 4, Valid: true},
				FieldPrecision: sql.NullInt64{Int64: 10, Valid: true},
			},
			{
				Name:        "GO_SPARSE_3",
				Position:    sql.NullInt64{Int64: 3, Valid: true},
				FieldType:   sql.NullInt64{Int64: fieldTypeCString, Valid: true},
				FieldLength: sql.NullInt64{Int64: 80, Valid: true},
			},
		},
	}
	if got, err := sparseReturn.ReturnType(); err != nil || got != "CSTRING(80)" {
		t.Fatalf("positional ReturnType = (%q, %v), want (\"CSTRING(80)\", nil)", got, err)
	}

	unsupported := []struct {
		name     string
		function Function
	}{
		{
			name:     "null return argument",
			function: Function{Name: "GO_NULL_RETURN", Arguments: denseReturn.Arguments},
		},
		{
			name: "return position absent",
			function: Function{
				Name:           "GO_MISSING_RETURN",
				ReturnArgument: sql.NullInt64{Int64: 5, Valid: true},
				Arguments:      denseReturn.Arguments,
			},
		},
	}
	for _, test := range unsupported {
		t.Run(test.name, func(t *testing.T) {
			if _, err := test.function.ReturnType(); !errors.Is(err, ErrUnsupportedDDL) {
				t.Fatalf("ReturnType error = %v, want ErrUnsupportedDDL", err)
			}
		})
	}

	db := openFixtureDB(t)
	defer db.Close()
	catalog := New(db)
	ctx := context.Background()

	scanned, err := catalog.Functions(ctx, "GO_EXTERNAL")
	if err != nil {
		t.Fatalf("Functions returned error: %v", err)
	}
	if len(scanned) != 1 || len(scanned[0].Arguments) != 1 {
		t.Fatalf("scanned function = %#v, want one function with one argument", scanned)
	}
	if got, err := scanned[0].Arguments[0].SQLType(); err != nil || got != "INTEGER" {
		t.Fatalf("scanned argument SQLType = (%q, %v), want (\"INTEGER\", nil)", got, err)
	}
	if got, err := scanned[0].ReturnType(); err != nil || got != "INTEGER" {
		t.Fatalf("scanned ReturnType = (%q, %v), want (\"INTEGER\", nil)", got, err)
	}

	text, err := catalog.Functions(ctx, "GO_EXTERNAL_TEXT")
	if err != nil {
		t.Fatalf("Functions returned error: %v", err)
	}
	if len(text) != 1 || len(text[0].Arguments) != 1 {
		t.Fatalf("scanned CSTRING function = %#v, want one function with one argument", text)
	}
	if got, err := text[0].Arguments[0].SQLType(); err != nil || got != "CSTRING(80)" {
		t.Fatalf("scanned CSTRING argument SQLType = (%q, %v), want (\"CSTRING(80)\", nil)", got, err)
	}
	if got, err := text[0].ReturnType(); err != nil || got != "CSTRING(80)" {
		t.Fatalf("scanned CSTRING ReturnType = (%q, %v), want (\"CSTRING(80)\", nil)", got, err)
	}
}
