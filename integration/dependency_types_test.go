//go:build integration

package integration_test

import (
	"strings"
	"testing"

	interbase "interbase-go"
	catalogschema "interbase-go/schema"
)

const dependencyFixtureSchema = `
CREATE EXCEPTION GO_DEP_EXC 'dependency fixture';
CREATE GENERATOR GO_DEP_GEN;
DECLARE EXTERNAL FUNCTION GO_DEP_ABS DOUBLE PRECISION RETURNS DOUBLE PRECISION BY VALUE ENTRY_POINT 'IB_UDF_abs' MODULE_NAME 'ib_udf';
CREATE TABLE GO_DEP_T (ID INTEGER NOT NULL, AMOUNT INTEGER, DOUBLED COMPUTED BY (AMOUNT * 2));
ALTER TABLE GO_DEP_T ADD CONSTRAINT GO_DEP_T_CHECK CHECK (AMOUNT >= 0);
CREATE VIEW GO_DEP_V AS SELECT ID, AMOUNT FROM GO_DEP_T;
SET TERM ^;
CREATE PROCEDURE GO_DEP_INNER AS BEGIN EXIT; END^
CREATE PROCEDURE GO_DEP_OUTER (X INTEGER) AS
DECLARE VARIABLE V INTEGER;
DECLARE VARIABLE D DOUBLE PRECISION;
BEGIN
  SELECT AMOUNT FROM GO_DEP_T WHERE ID = :X INTO :V;
  V = GEN_ID(GO_DEP_GEN, 1);
  D = GO_DEP_ABS(V);
  EXECUTE PROCEDURE GO_DEP_INNER;
  IF (V < 0) THEN EXCEPTION GO_DEP_EXC;
END^
CREATE TRIGGER GO_DEP_T_BI FOR GO_DEP_T ACTIVE BEFORE INSERT POSITION 0 AS
BEGIN
  NEW.ID = GEN_ID(GO_DEP_GEN, 1);
END^
SET TERM ;^
`

func TestDependencyObjectTypeCodes(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3, dependencyFixtureSchema)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)
	rows, err := catalogschema.New(db).Dependencies(ctx, "")
	if err != nil {
		t.Fatalf("Dependencies: %v", err)
	}
	type edge struct {
		dependent     string
		dependentType catalogschema.DependencyObjectType
		on            string
		onType        catalogschema.DependencyObjectType
		field         string
	}
	have := make(map[edge]bool, len(rows))
	checkTrigger, computedDomain := false, false
	for _, row := range rows {
		e := edge{row.DependentName, catalogschema.DependencyObjectType(row.DependentType.Int64),
			row.DependedOnName, catalogschema.DependencyObjectType(row.DependedOnType.Int64), row.FieldName.String}
		have[e] = true
		if strings.HasPrefix(e.dependent, "CHECK_") && e.dependentType == catalogschema.DependencyTrigger && e.on == "GO_DEP_T" && e.field == "AMOUNT" {
			checkTrigger = true
		}
		if strings.HasPrefix(e.dependent, "RDB$") && e.dependentType == catalogschema.DependencyComputedField && e.on == "GO_DEP_T" && e.field == "AMOUNT" {
			computedDomain = true
		}
	}
	for _, want := range []edge{
		{"GO_DEP_V", catalogschema.DependencyView, "GO_DEP_T", catalogschema.DependencyRelation, ""},
		{"GO_DEP_OUTER", catalogschema.DependencyProcedure, "GO_DEP_T", catalogschema.DependencyRelation, "AMOUNT"},
		{"GO_DEP_OUTER", catalogschema.DependencyProcedure, "GO_DEP_GEN", catalogschema.DependencyGenerator, ""},
		{"GO_DEP_OUTER", catalogschema.DependencyProcedure, "GO_DEP_ABS", catalogschema.DependencyFunction, ""},
		{"GO_DEP_OUTER", catalogschema.DependencyProcedure, "GO_DEP_INNER", catalogschema.DependencyProcedure, ""},
		{"GO_DEP_OUTER", catalogschema.DependencyProcedure, "GO_DEP_EXC", catalogschema.DependencyException, ""},
		{"GO_DEP_T_BI", catalogschema.DependencyTrigger, "GO_DEP_GEN", catalogschema.DependencyGenerator, ""},
	} {
		if !have[want] {
			t.Errorf("missing dependency %+v", want)
		}
	}
	if !checkTrigger {
		t.Error("no CHECK_n trigger (type 2) dependency on GO_DEP_T.AMOUNT")
	}
	if !computedDomain {
		t.Error("no RDB$n computed-field (type 3) dependency on GO_DEP_T.AMOUNT")
	}
	if t.Failed() {
		for _, row := range rows {
			t.Logf("row: %s(%d) -> %s(%d) field=%q", row.DependentName, row.DependentType.Int64, row.DependedOnName, row.DependedOnType.Int64, row.FieldName.String)
		}
	}
}
