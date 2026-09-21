//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"strings"
	"testing"

	interbase "interbase-go"
	catalogschema "interbase-go/schema"
)

func TestSchemaRelationsViewsAndExactFilters(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createSchemaObjects(t, db, ctx,
		`CREATE TABLE GO_SCHEMA_BASE_A (ID INTEGER NOT NULL)`,
		`CREATE TABLE GO_SCHEMA_BASE_B (ID INTEGER NOT NULL)`,
		`CREATE VIEW GO_SCHEMA_VIEW_A (A_ID) AS SELECT ID FROM GO_SCHEMA_BASE_A`,
		`CREATE VIEW GO_SCHEMA_VIEW_B (B_ID) AS SELECT ID FROM GO_SCHEMA_BASE_B`,
	)

	contextA := schemaViewContext(t, db, ctx, "GO_SCHEMA_VIEW_A")
	contextB := schemaViewContext(t, db, ctx, "GO_SCHEMA_VIEW_B")
	if contextA != contextB {
		t.Fatalf("view contexts = (%d, %d), want the same context for the join regression", contextA, contextB)
	}

	catalog := catalogschema.New(db)
	table, err := catalog.Table(ctx, "GO_SCHEMA_BASE_A")
	if err != nil {
		t.Fatalf("Table exact lookup: %v", err)
	}
	if table == nil || table.Kind != catalogschema.RelationTable {
		t.Fatalf("Table exact lookup = %#v, want one table", table)
	}
	if !table.RelationType.Valid || table.RelationType.String != "PERSISTENT" {
		t.Fatalf("table relation type = %#v, want padded PERSISTENT catalog text", table.RelationType)
	}
	if tables, err := catalog.Tables(ctx, "GO_SCHEMA_BASE"); err != nil {
		t.Fatalf("Tables prefix lookup: %v", err)
	} else if len(tables) != 0 {
		t.Fatalf("Tables prefix lookup returned %#v, want exact matching", tables)
	}
	allTables, err := catalog.Tables(ctx, "")
	if err != nil {
		t.Fatalf("Tables empty lookup: %v", err)
	}
	if !hasSchemaRelation(allTables, "GO_SCHEMA_BASE_A") || !hasSchemaRelation(allTables, "GO_SCHEMA_BASE_B") {
		t.Fatalf("Tables empty lookup = %#v, want both created tables", schemaRelationNames(allTables))
	}

	viewA, err := catalog.View(ctx, "GO_SCHEMA_VIEW_A")
	if err != nil {
		t.Fatalf("View A exact lookup: %v", err)
	}
	if viewA == nil || len(viewA.Columns) != 1 {
		t.Fatalf("View A = %#v, want one column", viewA)
	}
	if !viewA.RelationType.Valid || viewA.RelationType.String != "VIEW" {
		t.Fatalf("View A relation type = %#v, want padded VIEW catalog text", viewA.RelationType)
	}
	if !viewA.ViewSource.Valid || !strings.Contains(strings.ToUpper(viewA.ViewSource.String), "GO_SCHEMA_BASE_A") {
		t.Fatalf("View A source = %#v, want SQL source BLOB text", viewA.ViewSource)
	}
	if column := viewA.Columns[0]; !column.BaseRelation.Valid || column.BaseRelation.String != "GO_SCHEMA_BASE_A" {
		t.Fatalf("View A base relation = %#v, want GO_SCHEMA_BASE_A", viewA.Columns[0].BaseRelation)
	}

	viewB, err := catalog.View(ctx, "GO_SCHEMA_VIEW_B")
	if err != nil {
		t.Fatalf("View B exact lookup: %v", err)
	}
	if viewB == nil || len(viewB.Columns) != 1 {
		t.Fatalf("View B = %#v, want one column", viewB)
	}
	if column := viewB.Columns[0]; !column.BaseRelation.Valid || column.BaseRelation.String != "GO_SCHEMA_BASE_B" {
		t.Fatalf("View B base relation = %#v, want GO_SCHEMA_BASE_B", viewB.Columns[0].BaseRelation)
	}
	if views, err := catalog.Views(ctx, "GO_SCHEMA_VIEW"); err != nil {
		t.Fatalf("Views prefix lookup: %v", err)
	} else if len(views) != 0 {
		t.Fatalf("Views prefix lookup returned %#v, want exact matching", views)
	}
	allViews, err := catalog.Views(ctx, "")
	if err != nil {
		t.Fatalf("Views empty lookup: %v", err)
	}
	if !hasSchemaRelation(allViews, "GO_SCHEMA_VIEW_A") || !hasSchemaRelation(allViews, "GO_SCHEMA_VIEW_B") {
		t.Fatalf("Views empty lookup = %#v, want both created views", schemaRelationNames(allViews))
	}

	for name, lookup := range map[string]func(context.Context, string) error{
		"table":     func(ctx context.Context, name string) error { _, err := catalog.Table(ctx, name); return err },
		"view":      func(ctx context.Context, name string) error { _, err := catalog.View(ctx, name); return err },
		"procedure": func(ctx context.Context, name string) error { _, err := catalog.Procedure(ctx, name); return err },
	} {
		if err := lookup(ctx, ""); err == nil {
			t.Errorf("%s empty singular lookup succeeded, want an error", name)
		}
	}
}

func TestSchemaColumnNullabilityUsesLocalAndDomainRestrictions(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createSchemaObjects(t, db, ctx,
		`CREATE DOMAIN GO_SCHEMA_OPT_DOM AS INTEGER`,
		`CREATE DOMAIN GO_SCHEMA_REQ_DOM AS INTEGER NOT NULL`,
		`CREATE TABLE GO_SCHEMA_NULLABILITY (
  LOCAL_REQUIRED INTEGER NOT NULL,
  LOCAL_REQ_OPT_DOM GO_SCHEMA_OPT_DOM NOT NULL,
  DOMAIN_REQUIRED GO_SCHEMA_REQ_DOM,
  DOMAIN_OPTIONAL GO_SCHEMA_OPT_DOM
)`,
	)

	table, err := catalogschema.New(db).Table(ctx, "GO_SCHEMA_NULLABILITY")
	if err != nil {
		t.Fatalf("nullability table lookup: %v", err)
	}
	if table == nil {
		t.Fatal("nullability table lookup returned nil")
	}
	columns := schemaColumnsByName(t, table.Columns)

	assertSchemaNotNullable(t, columns["LOCAL_REQUIRED"], "local NOT NULL column")
	localOverNullableDomain := columns["LOCAL_REQ_OPT_DOM"]
	if localOverNullableDomain.Domain == nil {
		t.Fatalf("local NOT NULL nullable-domain metadata = %#v, columns = %#v, want a resolved domain", localOverNullableDomain, schemaColumnNames(table.Columns))
	}
	if localOverNullableDomain.Domain.Nullable.Valid && !localOverNullableDomain.Domain.Nullable.Bool {
		t.Fatalf("local NOT NULL domain nullable = %#v, want no non-null domain restriction", localOverNullableDomain.Domain.Nullable)
	}
	assertSchemaNotNullable(t, localOverNullableDomain, "local NOT NULL over nullable domain")

	domainRequired := columns["DOMAIN_REQUIRED"]
	if domainRequired.Domain == nil {
		t.Fatal("domain-required column has no resolved domain")
	}
	assertSchemaNotNullable(t, domainRequired, "non-nullable domain column")
	if !domainRequired.Domain.NullFlag.Valid || domainRequired.Domain.NullFlag.Int64 == 0 {
		t.Fatalf("non-nullable domain raw null flag = %#v, want a nonzero flag", domainRequired.Domain.NullFlag)
	}

	domainOptional := columns["DOMAIN_OPTIONAL"]
	if domainOptional.Domain == nil {
		t.Fatalf("nullable-domain metadata = %#v, want a resolved domain", domainOptional)
	}
	if domainOptional.Domain.Nullable.Valid && !domainOptional.Domain.Nullable.Bool {
		t.Fatalf("nullable-domain metadata = %#v, must not claim NOT NULL", domainOptional.Domain.Nullable)
	}
	if domainOptional.Nullable.Valid && !domainOptional.Nullable.Bool {
		t.Fatalf("nullable-domain column = %#v, must not claim NOT NULL", domainOptional.Nullable)
	}
}

func TestSchemaProcedureParametersAndSQLSources(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	createSchemaObjects(t, db, ctx,
		`CREATE PROCEDURE GO_SCHEMA_PROC (
  OPTIONAL_INPUT INTEGER,
  REQUIRED_INPUT INTEGER
)
RETURNS (OUTPUT_VALUE INTEGER)
AS
BEGIN
  OUTPUT_VALUE = :REQUIRED_INPUT;
  SUSPEND;
END`,
	)

	catalog := catalogschema.New(db)
	procedure, err := catalog.Procedure(ctx, "GO_SCHEMA_PROC")
	if err != nil {
		t.Fatalf("procedure exact lookup: %v", err)
	}
	if procedure == nil {
		t.Fatal("procedure exact lookup returned nil")
	}
	if !procedure.Source.Valid || !strings.Contains(strings.ToUpper(procedure.Source.String), "OUTPUT_VALUE") {
		t.Fatalf("procedure source = %#v, want SQL source BLOB text", procedure.Source)
	}
	if names := schemaParameterNames(procedure.InputParameters); len(names) != 2 || names[0] != "OPTIONAL_INPUT" || names[1] != "REQUIRED_INPUT" {
		t.Fatalf("input parameters = %#v, want catalog order", names)
	}
	if len(procedure.OutputParameters) != 1 || procedure.OutputParameters[0].Name != "OUTPUT_VALUE" {
		t.Fatalf("output parameters = %#v, want OUTPUT_VALUE", procedure.OutputParameters)
	}

	optional := procedure.InputParameters[0]
	if !optional.FieldSource.Valid || optional.Domain == nil {
		t.Fatalf("optional parameter domain metadata = %#v, want resolved domain", optional)
	}
	if optional.Nullable.Valid {
		t.Fatalf("optional parameter nullable = %#v, want unknown declaration nullability", optional.Nullable)
	}
	required := procedure.InputParameters[1]
	if !required.FieldSource.Valid || required.Domain == nil {
		t.Fatalf("required parameter domain metadata = %#v, want resolved domain", required)
	}
	if _, err := procedure.GenerateDDL(); !errors.Is(err, catalogschema.ErrUnsupportedDDL) || !strings.Contains(err.Error(), "nullability") {
		t.Fatalf("procedure with unknown parameter nullability error = %v, want deliberate ErrUnsupportedDDL", err)
	}

	if procedures, err := catalog.Procedures(ctx, "GO_SCHEMA"); err != nil {
		t.Fatalf("procedures prefix lookup: %v", err)
	} else if len(procedures) != 0 {
		t.Fatalf("procedures prefix lookup returned %#v, want exact matching", procedures)
	}
	allProcedures, err := catalog.Procedures(ctx, "")
	if err != nil {
		t.Fatalf("procedures empty lookup: %v", err)
	}
	if !hasSchemaProcedure(allProcedures, "GO_SCHEMA_PROC") {
		t.Fatalf("procedures empty lookup = %#v, want created procedure", schemaProcedureNames(allProcedures))
	}
}

func TestSchemaExtendedCatalogFamiliesAndDDL(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	createSchemaObjects(t, db, ctx,
		`CREATE DOMAIN GO_SCHEMA_EXT_NAME AS VARCHAR(20) CHARACTER SET UTF8 DEFAULT 'unknown' CHECK (VALUE <> '')`,
		`CREATE DOMAIN GO_SCHEMA_EXT_ID AS INTEGER NOT NULL`,
		`CREATE GENERATOR GO_SCHEMA_EXT_SEQUENCE`,
		`CREATE TABLE GO_SCHEMA_EXT_PARENT (
  ID GO_SCHEMA_EXT_ID
)`,
		`ALTER TABLE GO_SCHEMA_EXT_PARENT ADD CONSTRAINT GO_EXT_PARENT_PK PRIMARY KEY (ID)`,
		`CREATE TABLE GO_SCHEMA_EXT_CHILD (
  ID INTEGER NOT NULL,
  PARENT_ID INTEGER,
  VALUE_TEXT VARCHAR(20)
)`,
		`ALTER TABLE GO_SCHEMA_EXT_CHILD ADD CONSTRAINT GO_EXT_CHILD_PK PRIMARY KEY (ID)`,
		`ALTER TABLE GO_SCHEMA_EXT_CHILD ADD CONSTRAINT GO_EXT_CHILD_FK FOREIGN KEY (PARENT_ID) REFERENCES GO_SCHEMA_EXT_PARENT (ID) ON DELETE CASCADE`,
		`ALTER TABLE GO_SCHEMA_EXT_CHILD ADD CONSTRAINT GO_EXT_CHILD_CHECK CHECK (ID > 0)`,
		`CREATE INDEX GO_SCHEMA_EXT_INDEX ON GO_SCHEMA_EXT_CHILD (PARENT_ID, VALUE_TEXT)`,
		`CREATE PROCEDURE GO_SCHEMA_EXT_PROC (INPUT_VALUE INTEGER)
RETURNS (OUTPUT_VALUE INTEGER)
AS
BEGIN
  OUTPUT_VALUE = :INPUT_VALUE;
  SUSPEND;
END`,
		`CREATE TRIGGER GO_SCHEMA_EXT_TRIGGER FOR GO_SCHEMA_EXT_CHILD
ACTIVE BEFORE INSERT POSITION 0
AS
BEGIN
  IF (NEW.ID IS NULL) THEN NEW.ID = GEN_ID(GO_SCHEMA_EXT_SEQUENCE, 1);
END`,
		`DECLARE EXTERNAL FUNCTION GO_SCHEMA_EXT_FUNCTION INTEGER RETURNS INTEGER BY VALUE ENTRY_POINT 'go_schema_ext_function' MODULE_NAME 'go_schema_ext_library'`,
		`DECLARE EXTERNAL FUNCTION GO_SCHEMA_EXT_CSTRING CSTRING(80)
RETURNS CSTRING(80) FREE_IT
ENTRY_POINT 'go_schema_ext_cstring' MODULE_NAME 'go_schema_ext_library'`,
		`CREATE ROLE GO_SCHEMA_EXT_ROLE`,
		`GRANT SELECT ON GO_SCHEMA_EXT_CHILD TO GO_SCHEMA_EXT_ROLE`,
	)

	catalog := catalogschema.New(db)

	domain, err := catalog.Domain(ctx, "GO_SCHEMA_EXT_NAME")
	if err != nil {
		t.Fatalf("domain lookup: %v", err)
	}
	if domain == nil || !domain.CharacterSetName.Valid || domain.CharacterSetName.String != "UTF8" {
		t.Fatalf("domain = %#v, want UTF8 character set metadata", domain)
	}
	if sqlType, err := domain.SQLType(); err != nil || !strings.Contains(sqlType, "VARCHAR(20)") {
		t.Fatalf("domain SQL type = %q, error = %v, want VARCHAR(20)", sqlType, err)
	}
	if ddl, err := domain.GenerateDDL(); err != nil {
		t.Fatalf("domain DDL: %v", err)
	} else if !strings.Contains(ddl, `CREATE DOMAIN "GO_SCHEMA_EXT_NAME"`) ||
		!strings.Contains(ddl, "DEFAULT") || !strings.Contains(ddl, "CHECK") {
		t.Fatalf("domain DDL = %q, want default and check clauses", ddl)
	}

	sequence, err := catalog.Sequence(ctx, "GO_SCHEMA_EXT_SEQUENCE")
	if err != nil {
		t.Fatalf("sequence lookup: %v", err)
	}
	if sequence == nil || sequence.Name != "GO_SCHEMA_EXT_SEQUENCE" {
		t.Fatalf("sequence = %#v, want exact sequence", sequence)
	}
	if ddl, err := sequence.GenerateDDL(); err != nil {
		t.Fatalf("sequence DDL: %v", err)
	} else if ddl != `CREATE GENERATOR "GO_SCHEMA_EXT_SEQUENCE"` {
		t.Fatalf("sequence DDL = %q, want executable InterBase generator syntax", ddl)
	}

	index, err := catalog.Index(ctx, "GO_SCHEMA_EXT_INDEX")
	if err != nil {
		t.Fatalf("index lookup: %v", err)
	}
	if index == nil || index.RelationName != "GO_SCHEMA_EXT_CHILD" || len(index.Segments) != 2 {
		t.Fatalf("index = %#v, want relation and two ordered segments", index)
	}
	if ddl, err := index.GenerateDDL(); err != nil {
		t.Fatalf("index DDL: %v", err)
	} else if !strings.Contains(ddl, `CREATE ASCENDING INDEX "GO_SCHEMA_EXT_INDEX"`) {
		t.Fatalf("index DDL = %q, want ascending index", ddl)
	}

	constraints, err := catalog.ConstraintsForRelation(ctx, "GO_SCHEMA_EXT_CHILD")
	if err != nil {
		t.Fatalf("relation constraints: %v", err)
	}
	constraintNames := make(map[string]catalogschema.Constraint)
	for _, constraint := range constraints {
		if _, exists := constraintNames[constraint.Name]; exists {
			t.Fatalf("duplicate logical constraint %q in %#v", constraint.Name, constraints)
		}
		constraintNames[constraint.Name] = constraint
	}
	for _, name := range []string{
		"GO_EXT_CHILD_PK",
		"GO_EXT_CHILD_FK",
		"GO_EXT_CHILD_CHECK",
	} {
		if _, ok := constraintNames[name]; !ok {
			t.Fatalf("constraints = %#v, missing %q", constraintNames, name)
		}
	}
	foreignKey := constraintNames["GO_EXT_CHILD_FK"]
	if foreignKey.ReferencedRelationName != "GO_SCHEMA_EXT_PARENT" ||
		len(foreignKey.Columns) != 1 || foreignKey.Columns[0] != "PARENT_ID" ||
		len(foreignKey.ReferencedColumns) != 1 || foreignKey.ReferencedColumns[0] != "ID" {
		t.Fatalf("foreign key = %#v, want resolved columns and relation", foreignKey)
	}
	table, err := catalog.Table(ctx, "GO_SCHEMA_EXT_CHILD")
	if err != nil {
		t.Fatalf("table lookup: %v", err)
	}
	if table == nil || !table.ConstraintsLoaded || !table.IndexesLoaded || !table.TriggersLoaded {
		t.Fatalf("table = %#v, want complete DDL metadata", table)
	}
	tableDDL, err := table.GenerateDDL()
	if err != nil {
		t.Fatalf("table DDL: %v", err)
	}
	for _, clause := range []string{
		`CREATE TABLE "GO_SCHEMA_EXT_CHILD"`,
		`CONSTRAINT "GO_EXT_CHILD_PK" PRIMARY KEY ("ID")`,
		`CONSTRAINT "GO_EXT_CHILD_FK" FOREIGN KEY ("PARENT_ID") REFERENCES "GO_SCHEMA_EXT_PARENT" ("ID") ON DELETE CASCADE`,
		`CONSTRAINT "GO_EXT_CHILD_CHECK" CHECK (ID > 0)`,
	} {
		if !strings.Contains(tableDDL, clause) {
			t.Fatalf("table DDL = %q, missing %q", tableDDL, clause)
		}
	}

	trigger, err := catalog.Trigger(ctx, "GO_SCHEMA_EXT_TRIGGER")
	if err != nil {
		t.Fatalf("trigger lookup: %v", err)
	}
	if trigger == nil || !trigger.Source.Valid || trigger.RelationName.String != "GO_SCHEMA_EXT_CHILD" {
		t.Fatalf("trigger = %#v, want relation and source", trigger)
	}
	triggerDDL, err := trigger.GenerateDDL()
	if err != nil {
		t.Fatalf("trigger DDL: %v", err)
	}
	if !strings.Contains(triggerDDL, `FOR "GO_SCHEMA_EXT_CHILD" ACTIVE BEFORE INSERT POSITION 0`) ||
		!strings.Contains(triggerDDL, "GEN_ID(GO_SCHEMA_EXT_SEQUENCE, 1)") {
		t.Fatalf("trigger DDL = %q, want event, position, and exact source", triggerDDL)
	}
	if event, err := trigger.Event(); err != nil || event != "BEFORE INSERT" {
		t.Fatalf("trigger Event = (%q, %v), want (\"BEFORE INSERT\", nil)", event, err)
	}

	procedure, err := catalog.Procedure(ctx, "GO_SCHEMA_EXT_PROC")
	if err != nil {
		t.Fatalf("procedure lookup: %v", err)
	}
	if procedure == nil || len(procedure.InputParameters) != 1 || len(procedure.OutputParameters) != 1 {
		t.Fatalf("procedure = %#v, want one input and one output", procedure)
	}
	if _, err := procedure.GenerateDDL(); !errors.Is(err, catalogschema.ErrUnsupportedDDL) || !strings.Contains(err.Error(), "nullability") {
		t.Fatalf("procedure DDL error = %v, want deliberate ErrUnsupportedDDL for unknown nullability", err)
	}

	role, err := catalog.Role(ctx, "GO_SCHEMA_EXT_ROLE")
	if err != nil {
		t.Fatalf("role lookup: %v", err)
	}
	if role == nil || role.Name != "GO_SCHEMA_EXT_ROLE" {
		t.Fatalf("role = %#v, want exact role", role)
	}
	privileges, err := catalog.Privileges(ctx, "GO_SCHEMA_EXT_ROLE")
	if err != nil {
		t.Fatalf("privilege lookup: %v", err)
	}
	if len(privileges) == 0 {
		t.Fatal("privileges = empty, want granted SELECT privilege")
	}
	privilege := privileges[0]
	if privilege.SubjectName != "GO_SCHEMA_EXT_CHILD" || privilege.PrivilegeCode != "S" {
		t.Fatalf("privilege = %#v, want SELECT on child table", privilege)
	}
	if ddl, err := privilege.GenerateDDL(); err != nil {
		t.Fatalf("privilege DDL: %v", err)
	} else if !strings.Contains(ddl, `GRANT SELECT ON "GO_SCHEMA_EXT_CHILD" TO "GO_SCHEMA_EXT_ROLE"`) {
		t.Fatalf("privilege DDL = %q, want quoted grant", ddl)
	}

	dependencies, err := catalog.Dependencies(ctx, "GO_SCHEMA_EXT_TRIGGER")
	if err != nil {
		t.Fatalf("dependency lookup: %v", err)
	}
	if len(dependencies) == 0 {
		t.Fatal("dependencies = empty, want child dependency rows")
	}

	function, err := catalog.Function(ctx, "GO_SCHEMA_EXT_FUNCTION")
	if err != nil {
		t.Fatalf("external function lookup: %v", err)
	}
	if function == nil || len(function.Arguments) == 0 {
		t.Fatalf("external function = %#v, want argument metadata", function)
	}
	if _, err := function.GenerateDDL(); !errors.Is(err, catalogschema.ErrUnsupportedDDL) {
		t.Fatalf("external function DDL error = %v, want ErrUnsupportedDDL", err)
	}
	if returnType, err := function.ReturnType(); err != nil || returnType != "INTEGER" {
		t.Fatalf("external function ReturnType = (%q, %v), want (\"INTEGER\", nil)", returnType, err)
	}
	checkedArgument := false
	for _, argument := range function.Arguments {
		if !argument.Position.Valid || argument.Position.Int64 != 0 {
			continue
		}
		checkedArgument = true
		if sqlType, err := argument.SQLType(); err != nil || sqlType != "INTEGER" {
			t.Fatalf("external function argument 0 SQLType = (%q, %v), want (\"INTEGER\", nil)", sqlType, err)
		}
	}
	// Without this the loop passes vacuously when the catalog places no
	// argument at position 0, and the assertion above never runs.
	if !checkedArgument {
		t.Fatalf("external function arguments = %#v, want one at position 0", function.Arguments)
	}

	cstringFunction, err := catalog.Function(ctx, "GO_SCHEMA_EXT_CSTRING")
	if err != nil {
		t.Fatalf("CSTRING external function lookup: %v", err)
	}
	if cstringFunction == nil || len(cstringFunction.Arguments) == 0 {
		t.Fatalf("CSTRING external function = %#v, want declared argument metadata", cstringFunction)
	}
	// The declared length is CSTRING(80). A mismatch here means the rule reads
	// the wrong catalog column; fix the column choice, never add a +/-1.
	if returnType, err := cstringFunction.ReturnType(); err != nil || returnType != "CSTRING(80)" {
		t.Fatalf("CSTRING ReturnType = (%q, %v), want (\"CSTRING(80)\", nil)", returnType, err)
	}
	for _, argument := range cstringFunction.Arguments {
		sqlType, err := argument.SQLType()
		if err != nil || sqlType != "CSTRING(80)" {
			t.Fatalf("CSTRING argument %d SQLType = (%q, %v), want (\"CSTRING(80)\", nil)", argument.Position.Int64, sqlType, err)
		}
	}

	shadows, err := catalog.Shadows(ctx)
	if err != nil {
		t.Fatalf("shadows lookup: %v", err)
	}
	if len(shadows) != 0 {
		t.Fatalf("shadows = %#v, want no shadows in a fresh fixture", shadows)
	}

	files, err := catalog.DatabaseFiles(ctx)
	if err != nil {
		t.Fatalf("database files lookup: %v", err)
	}
	if len(files) == 0 || !files[0].FileName.Valid {
		t.Fatalf("database files = %#v, want current database file", files)
	}
}

func TestSchemaGeneratedDDLReplay(t *testing.T) {
	// InterBase accepts one DML event per relation trigger in this live
	// fixture. Multi-event catalog-mask decoding is covered by the pure-Go
	// tests because the server rejects the reference Firebird-style OR syntax.
	const sourceSchema = `
CREATE DOMAIN GO_REPLAY_NAME AS VARCHAR(32) CHARACTER SET UTF8 DEFAULT 'unknown' CHECK (VALUE <> '');
CREATE DOMAIN GO_REPLAY_ID AS INTEGER NOT NULL;
CREATE GENERATOR GO_REPLAY_SEQUENCE;
CREATE TABLE GO_REPLAY_PARENT (ID GO_REPLAY_ID);
ALTER TABLE GO_REPLAY_PARENT ADD CONSTRAINT GO_REPLAY_PARENT_PK PRIMARY KEY (ID);
CREATE TABLE GO_REPLAY_CHILD (
  ID INTEGER NOT NULL,
  PARENT_ID INTEGER,
  VALUE_TEXT GO_REPLAY_NAME,
  COMPUTED_VALUE COMPUTED BY (ID + PARENT_ID),
  UNIQUE_VALUE VARCHAR(16) CONSTRAINT GO_REPLAY_CHILD_UNIQUE NOT NULL
);
ALTER TABLE GO_REPLAY_CHILD ADD CONSTRAINT GO_REPLAY_CHILD_PK PRIMARY KEY (ID);
ALTER TABLE GO_REPLAY_CHILD ADD CONSTRAINT GO_REPLAY_CHILD_FK FOREIGN KEY (PARENT_ID) REFERENCES GO_REPLAY_PARENT (ID) ON DELETE CASCADE;
ALTER TABLE GO_REPLAY_CHILD ADD CONSTRAINT GO_REPLAY_CHILD_UQ UNIQUE (UNIQUE_VALUE);
ALTER TABLE GO_REPLAY_CHILD ADD CONSTRAINT GO_REPLAY_CHILD_CHECK CHECK (PARENT_ID IS NULL OR PARENT_ID > 0);
CREATE DESCENDING INDEX GO_REPLAY_DESC_INDEX ON GO_REPLAY_CHILD (VALUE_TEXT);
CREATE INDEX GO_REPLAY_INACTIVE ON GO_REPLAY_CHILD (PARENT_ID);
ALTER INDEX GO_REPLAY_INACTIVE INACTIVE;
CREATE VIEW GO_REPLAY_VIEW (ID, VALUE_TEXT) AS
SELECT ID, VALUE_TEXT FROM GO_REPLAY_CHILD
WHERE VALUE_TEXT <> 'blocked'
WITH CHECK OPTION;
SET TERM ^;
CREATE PROCEDURE GO_REPLAY_PROC (INPUT_VALUE INTEGER)
RETURNS (OUTPUT_VALUE INTEGER)
AS
BEGIN
  OUTPUT_VALUE = :INPUT_VALUE + 1;
  SUSPEND;
END^
CREATE TRIGGER GO_REPLAY_INSERT FOR GO_REPLAY_CHILD
ACTIVE BEFORE INSERT POSITION 0
AS
BEGIN
  IF (NEW.ID IS NULL) THEN NEW.ID = GEN_ID(GO_REPLAY_SEQUENCE, 1);
END^
SET TERM ;^
CREATE ROLE GO_REPLAY_ROLE;
GRANT SELECT ON GO_REPLAY_CHILD TO GO_REPLAY_ROLE;
`

	sourceFixture, cfg, sourceCleanup := createFixture(t, 3, sourceSchema)
	sourceDB := openDatabase(t, sourceCleanup, sourceFixture.ConnectionString(), cfg.User, cfg.Password, "", 3, interbase.TransactionOptions{})
	targetFixture, targetCfg, targetCleanup := createFixture(t, 3, "")
	targetDB := openDatabase(t, targetCleanup, targetFixture.ConnectionString(), targetCfg.User, targetCfg.Password, "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	sourceCatalog := catalogschema.New(sourceDB)
	var statements []string
	unsupported := make([]string, 0, 2)
	appendDDL := func(label string, object catalogschema.DDLer) {
		t.Helper()
		statement, err := object.GenerateDDL()
		if err != nil {
			t.Fatalf("generate %s DDL: %v", label, err)
		}
		statements = append(statements, statement)
	}
	recordUnsupported := func(label string, err error, feature string) {
		t.Helper()
		if !errors.Is(err, catalogschema.ErrUnsupportedDDL) {
			t.Fatalf("generate %s DDL error = %v, want ErrUnsupportedDDL", label, err)
		}
		if !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(feature)) {
			t.Fatalf("generate %s DDL error = %v, want feature %q", label, err, feature)
		}
		unsupported = append(unsupported, label+": "+err.Error())
		t.Logf("deliberately unsupported %s: %v", label, err)
	}
	for _, name := range []string{"GO_REPLAY_NAME", "GO_REPLAY_ID"} {
		domain, err := sourceCatalog.Domain(ctx, name)
		if err != nil || domain == nil {
			t.Fatalf("source domain %q: %v", name, err)
		}
		appendDDL("domain "+name, domain)
	}
	sequence, err := sourceCatalog.Sequence(ctx, "GO_REPLAY_SEQUENCE")
	if err != nil || sequence == nil {
		t.Fatalf("source sequence: %v", err)
	}
	appendDDL("sequence", sequence)
	for _, name := range []string{"GO_REPLAY_PARENT"} {
		table, err := sourceCatalog.Table(ctx, name)
		if err != nil || table == nil {
			t.Fatalf("source table %q: %v", name, err)
		}
		appendDDL("table "+name, table)
	}
	child, err := sourceCatalog.Table(ctx, "GO_REPLAY_CHILD")
	if err != nil || child == nil {
		t.Fatalf("source table %q: %v", "GO_REPLAY_CHILD", err)
	}
	childDDL, childErr := child.GenerateDDL()
	if childErr == nil {
		t.Fatalf("generate table GO_REPLAY_CHILD DDL unexpectedly succeeded despite computed column")
	}
	recordUnsupported("table GO_REPLAY_CHILD", childErr, "computed column")
	withoutComputed := *child
	withoutComputed.Columns = make([]catalogschema.Column, 0, len(child.Columns)-1)
	for _, column := range child.Columns {
		if column.ComputedSource.Valid && strings.TrimSpace(column.ComputedSource.String) != "" {
			continue
		}
		withoutComputed.Columns = append(withoutComputed.Columns, column)
	}
	childDDL, err = withoutComputed.GenerateDDL()
	if err != nil {
		t.Fatalf("generate supported portion of child table DDL: %v", err)
	}
	statements = append(statements, childDDL)
	statements = append(statements, `ALTER TABLE "GO_REPLAY_CHILD" ADD "COMPUTED_VALUE" COMPUTED BY (ID + PARENT_ID)`)
	view, err := sourceCatalog.View(ctx, "GO_REPLAY_VIEW")
	if err != nil || view == nil {
		t.Fatalf("source view: %v", err)
	}
	appendDDL("view", view)
	procedure, err := sourceCatalog.Procedure(ctx, "GO_REPLAY_PROC")
	if err != nil || procedure == nil {
		t.Fatalf("source procedure: %v", err)
	}
	procedureDDL, procedureErr := procedure.GenerateDDL()
	if procedureErr == nil {
		statements = append(statements, procedureDDL)
	} else {
		recordUnsupported("procedure", procedureErr, "nullability")
		statements = append(statements, `CREATE PROCEDURE "GO_REPLAY_PROC" ("INPUT_VALUE" INTEGER) RETURNS ("OUTPUT_VALUE" INTEGER) AS `+procedure.Source.String)
	}
	trigger, err := sourceCatalog.Trigger(ctx, "GO_REPLAY_INSERT")
	if err != nil || trigger == nil {
		t.Fatalf("source trigger: %v", err)
	}
	appendDDL("trigger", trigger)
	for _, name := range []string{"GO_REPLAY_DESC_INDEX", "GO_REPLAY_INACTIVE"} {
		index, err := sourceCatalog.Index(ctx, name)
		if err != nil || index == nil {
			t.Fatalf("source index %q: %v", name, err)
		}
		if _, err := index.GenerateDDL(); err != nil {
			t.Fatalf("generate source index %q DDL from %#v: %v", name, index, err)
		}
		statementer, ok := any(index).(catalogschema.Statementer)
		if !ok {
			t.Fatalf("source index %q does not implement Statementer", name)
		}
		indexStatements, err := statementer.Statements()
		if err != nil {
			t.Fatalf("source index %q statements: %v", name, err)
		}
		if len(indexStatements) == 0 {
			t.Fatalf("source index %q produced no statements", name)
		}
		statements = append(statements, indexStatements...)
	}
	role, err := sourceCatalog.Role(ctx, "GO_REPLAY_ROLE")
	if err != nil || role == nil {
		t.Fatalf("source role: %v", err)
	}
	appendDDL("role", role)
	privileges, err := sourceCatalog.Privileges(ctx, "GO_REPLAY_ROLE")
	if err != nil || len(privileges) == 0 {
		t.Fatalf("source role privileges: %v", err)
	}
	for _, privilege := range privileges {
		appendDDL("role privilege", privilege)
	}
	if len(unsupported) != 2 {
		t.Fatalf("explicit unsupported metadata = %#v, want computed table and unknown-nullability procedure", unsupported)
	}

	for index, statement := range statements {
		if _, err := targetDB.ExecContext(ctx, statement); err != nil {
			t.Fatalf("replay statement %d (%s): %v", index+1, statement, err)
		}
	}

	targetCatalog := catalogschema.New(targetDB)
	replayedDomain, err := targetCatalog.Domain(ctx, "GO_REPLAY_NAME")
	if err != nil || replayedDomain == nil {
		t.Fatalf("replayed domain: %v", err)
	}
	if got, err := replayedDomain.SQLType(); err != nil || got != `VARCHAR(32) CHARACTER SET "UTF8"` {
		t.Fatalf("replayed domain SQL type = (%q, %v), want UTF8 VARCHAR", got, err)
	}
	replayedTable, err := targetCatalog.Table(ctx, "GO_REPLAY_CHILD")
	if err != nil || replayedTable == nil {
		t.Fatalf("replayed table: %v", err)
	}
	columns := schemaColumnsByName(t, replayedTable.Columns)
	if !columns["ID"].Nullable.Valid || columns["ID"].Nullable.Bool {
		t.Fatalf("replayed ID nullability = %#v, want NOT NULL", columns["ID"].Nullable)
	}
	if columns["VALUE_TEXT"].Domain == nil || columns["VALUE_TEXT"].Domain.Name != "GO_REPLAY_NAME" {
		t.Fatalf("replayed domain-based column = %#v, want GO_REPLAY_NAME", columns["VALUE_TEXT"])
	}
	if !columns["COMPUTED_VALUE"].ComputedSource.Valid {
		t.Fatalf("replayed computed column = %#v, want computed source", columns["COMPUTED_VALUE"])
	}
	constraints, err := targetCatalog.ConstraintsForRelation(ctx, "GO_REPLAY_CHILD")
	if err != nil {
		t.Fatalf("replayed constraints: %v", err)
	}
	constraintNames := make(map[string]bool, len(constraints))
	for _, constraint := range constraints {
		constraintNames[constraint.Name] = true
	}
	for _, name := range []string{"GO_REPLAY_CHILD_PK", "GO_REPLAY_CHILD_FK", "GO_REPLAY_CHILD_UQ", "GO_REPLAY_CHILD_CHECK", "GO_REPLAY_CHILD_UNIQUE"} {
		if !constraintNames[name] {
			t.Fatalf("replayed constraints = %#v, missing %q", constraintNames, name)
		}
	}
	var namedNotNull *catalogschema.Constraint
	for index := range constraints {
		if constraints[index].Name == "GO_REPLAY_CHILD_UNIQUE" {
			namedNotNull = &constraints[index]
			break
		}
	}
	if namedNotNull == nil || !namedNotNull.ColumnName.Valid || namedNotNull.ColumnName.String != "UNIQUE_VALUE" {
		t.Fatalf("replayed named NOT NULL constraint = %#v, want UNIQUE_VALUE", namedNotNull)
	}
	for _, name := range []string{"GO_REPLAY_DESC_INDEX", "GO_REPLAY_INACTIVE"} {
		index, err := targetCatalog.Index(ctx, name)
		if err != nil || index == nil {
			t.Fatalf("replayed index %q: %v", name, err)
		}
		if name == "GO_REPLAY_INACTIVE" && (!index.Inactive.Valid || index.Inactive.Int64 == 0) {
			t.Fatalf("replayed inactive index = %#v, want inactive", index)
		}
	}
	replayedTrigger, err := targetCatalog.Trigger(ctx, "GO_REPLAY_INSERT")
	if err != nil || replayedTrigger == nil || !replayedTrigger.Source.Valid {
		t.Fatalf("replayed trigger = %#v, error = %v", replayedTrigger, err)
	}
	if !strings.Contains(strings.ToUpper(replayedTrigger.Source.String), "NEW.ID") {
		t.Fatalf("replayed trigger source = %#v, want preserved source", replayedTrigger.Source)
	}
	replayedProcedure, err := targetCatalog.Procedure(ctx, "GO_REPLAY_PROC")
	if err != nil || replayedProcedure == nil || replayedProcedure.InputCount.Int64 != 1 || len(replayedProcedure.InputParameters) != 1 {
		t.Fatalf("replayed procedure = %#v, error = %v", replayedProcedure, err)
	}
	replayedView, err := targetCatalog.View(ctx, "GO_REPLAY_VIEW")
	if err != nil || replayedView == nil || len(replayedView.Columns) != 2 || !replayedView.ViewSource.Valid {
		t.Fatalf("replayed view = %#v, error = %v", replayedView, err)
	}
	if !strings.Contains(strings.ToUpper(replayedView.ViewSource.String), "WITH CHECK OPTION") {
		t.Fatalf("replayed view source = %q, want WITH CHECK OPTION", replayedView.ViewSource.String)
	}

	if _, err := targetDB.ExecContext(ctx, `INSERT INTO GO_REPLAY_PARENT (ID) VALUES (1)`); err != nil {
		t.Fatalf("insert replay parent: %v", err)
	}
	if _, err := targetDB.ExecContext(ctx, `INSERT INTO GO_REPLAY_CHILD (PARENT_ID, VALUE_TEXT, UNIQUE_VALUE) VALUES (1, 'value', 'unique')`); err != nil {
		t.Fatalf("insert replay child: %v", err)
	}
	var childID int64
	if err := targetDB.QueryRowContext(ctx, `SELECT ID FROM GO_REPLAY_CHILD WHERE UNIQUE_VALUE = 'unique'`).Scan(&childID); err != nil {
		t.Fatalf("query replay trigger result: %v", err)
	}
	if childID == 0 {
		t.Fatalf("replayed trigger assigned child ID %d, want generated ID", childID)
	}
	var viewValue string
	if err := targetDB.QueryRowContext(ctx, `SELECT VALUE_TEXT FROM GO_REPLAY_VIEW WHERE ID = ?`, childID).Scan(&viewValue); err != nil || viewValue != "value" {
		t.Fatalf("replayed view value = (%q, %v), want value", viewValue, err)
	}
	scanChildValues := func() map[int64]string {
		t.Helper()
		rows, err := targetDB.QueryContext(ctx, `SELECT ID, VALUE_TEXT FROM GO_REPLAY_CHILD ORDER BY ID`)
		if err != nil {
			t.Fatalf("scan replay child rows: %v", err)
		}
		defer rows.Close()
		values := make(map[int64]string)
		for rows.Next() {
			var id int64
			var value string
			if err := rows.Scan(&id, &value); err != nil {
				t.Fatalf("scan replay child row: %v", err)
			}
			values[id] = value
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("iterate replay child rows: %v", err)
		}
		return values
	}
	if _, err := targetDB.ExecContext(ctx, `UPDATE GO_REPLAY_VIEW SET VALUE_TEXT = 'allowed' WHERE ID = ?`, childID); err != nil {
		t.Fatalf("replayed view rejected an allowed update: %v", err)
	}
	allowedSnapshot := scanChildValues()
	if got, ok := allowedSnapshot[childID]; !ok || got != "allowed" {
		t.Fatalf("underlying row after allowed view update = (%q, %t), want allowed", got, ok)
	}
	if _, err := targetDB.ExecContext(ctx, `UPDATE GO_REPLAY_VIEW SET VALUE_TEXT = 'blocked' WHERE ID = ?`, childID); err == nil {
		t.Fatal("replayed view accepted an update rejected by WITH CHECK OPTION")
	}
	blockedSnapshot := scanChildValues()
	if !reflect.DeepEqual(blockedSnapshot, allowedSnapshot) {
		t.Fatalf("underlying rows after rejected view update = %#v, want unchanged %#v", blockedSnapshot, allowedSnapshot)
	}
	var procedureValue int64
	if err := targetDB.QueryRowContext(ctx, `SELECT OUTPUT_VALUE FROM GO_REPLAY_PROC(41)`).Scan(&procedureValue); err != nil || procedureValue != 42 {
		t.Fatalf("replayed procedure value = (%d, %v), want 42", procedureValue, err)
	}
}

func createSchemaObjects(t *testing.T, db *sql.DB, ctx context.Context, statements ...string) {
	t.Helper()
	for index, statement := range statements {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("schema fixture statement %d (%s): %v", index+1, statement, err)
		}
	}
}

func schemaViewContext(t *testing.T, db *sql.DB, ctx context.Context, name string) int64 {
	t.Helper()
	var value sql.NullInt64
	if err := db.QueryRowContext(ctx, `
SELECT RDB$VIEW_CONTEXT
FROM RDB$VIEW_RELATIONS
WHERE RDB$VIEW_NAME = ?`, name).Scan(&value); err != nil {
		t.Fatalf("view %q context: %v", name, err)
	}
	if !value.Valid {
		t.Fatalf("view %q context is NULL", name)
	}
	return value.Int64
}

func hasSchemaRelation(relations []catalogschema.Relation, name string) bool {
	for _, relation := range relations {
		if relation.Name == name {
			return true
		}
	}
	return false
}

func schemaRelationNames(relations []catalogschema.Relation) []string {
	names := make([]string, 0, len(relations))
	for _, relation := range relations {
		names = append(names, relation.Name)
	}
	return names
}

func schemaColumnsByName(t *testing.T, columns []catalogschema.Column) map[string]catalogschema.Column {
	t.Helper()
	result := make(map[string]catalogschema.Column, len(columns))
	for _, column := range columns {
		result[column.Name] = column
	}
	return result
}

func schemaColumnNames(columns []catalogschema.Column) []string {
	names := make([]string, 0, len(columns))
	for _, column := range columns {
		names = append(names, column.Name)
	}
	return names
}

func assertSchemaNotNullable(t *testing.T, column catalogschema.Column, label string) {
	t.Helper()
	if !column.Nullable.Valid || column.Nullable.Bool {
		t.Fatalf("%s nullable = %#v, want valid false", label, column.Nullable)
	}
}

func schemaParameterNames(parameters []catalogschema.ProcedureParameter) []string {
	names := make([]string, 0, len(parameters))
	for _, parameter := range parameters {
		names = append(names, parameter.Name)
	}
	return names
}

func hasSchemaProcedure(procedures []catalogschema.Procedure, name string) bool {
	for _, procedure := range procedures {
		if procedure.Name == name {
			return true
		}
	}
	return false
}

func schemaProcedureNames(procedures []catalogschema.Procedure) []string {
	names := make([]string, 0, len(procedures))
	for _, procedure := range procedures {
		names = append(names, procedure.Name)
	}
	return names
}
