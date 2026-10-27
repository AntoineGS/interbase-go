package schema

import (
	"database/sql"
	"strings"
	"testing"
)

func TestFirebirdMultiEventTriggerDDL(t *testing.T) {
	trigger := Trigger{
		Name:         "T_BIU",
		RelationName: sql.NullString{String: "CUSTOMER", Valid: true},
		TriggerType:  sql.NullInt64{Int64: 17, Valid: true}, // BEFORE INSERT OR UPDATE
		Sequence:     sql.NullInt64{Int64: 0, Valid: true},
		Source:       sql.NullString{String: "AS BEGIN END", Valid: true},
	}
	if _, err := trigger.GenerateDDLWithOptions(DDLOptions{}); err == nil {
		t.Fatal("InterBase DDL accepted a multi-event trigger")
	}
	ddl, err := trigger.GenerateDDLWithOptions(DDLOptions{Firebird: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(ddl, "BEFORE INSERT OR UPDATE POSITION 0 AS BEGIN END") {
		t.Fatalf("ddl = %q", ddl)
	}
}

func TestFirebirdNumericRelationTypesGenerateTableDDL(t *testing.T) {
	tests := []struct {
		name         string
		tableName    string
		relationType string
		want         []string
	}{
		{name: "persistent", tableName: "FB_CUSTOMER", relationType: "0", want: []string{`CREATE TABLE "FB_CUSTOMER"`}},
		{name: "preserve rows", tableName: "FB_GTT_PRESERVE", relationType: "4", want: []string{`CREATE GLOBAL TEMPORARY TABLE "FB_GTT_PRESERVE"`, "ON COMMIT PRESERVE ROWS"}},
		{name: "delete rows", tableName: "FB_GTT_DELETE", relationType: "5", want: []string{`CREATE GLOBAL TEMPORARY TABLE "FB_GTT_DELETE"`, "ON COMMIT DELETE ROWS"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			relation := Relation{
				Name:              test.tableName,
				Kind:              RelationTable,
				RelationType:      sql.NullString{String: test.relationType, Valid: true},
				ConstraintsLoaded: true,
				Columns:           []Column{{Name: "ID", Domain: &Domain{FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}}}},
			}
			ddl, err := relation.GenerateDDLWithOptions(DDLOptions{Firebird: true})
			if err != nil {
				t.Fatal(err)
			}
			for _, fragment := range test.want {
				if !strings.Contains(ddl, fragment) {
					t.Errorf("DDL missing %q:\n%s", fragment, ddl)
				}
			}
			if _, err := relation.GenerateDDLWithOptions(DDLOptions{}); err == nil {
				t.Fatal("InterBase DDL accepted a Firebird numeric relation type")
			}
		})
	}
}

func TestFirebirdProcedureDDLPreservesTypeOfReferences(t *testing.T) {
	tests := []struct {
		name      string
		parameter ProcedureParameter
		want      string
	}{
		{
			name: "column",
			parameter: ProcedureParameter{
				Name: "P_ID", ProcedureName: "FB_PROC", Number: sql.NullInt64{Int64: 0, Valid: true},
				Direction: ParameterInput, ParameterType: sql.NullInt64{Int64: 0, Valid: true},
				FieldSource:                sql.NullString{String: "RDB$1", Valid: true},
				Domain:                     &Domain{Name: "RDB$1", SystemFlag: sql.NullInt64{Int64: 1, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}},
				Nullable:                   sql.NullBool{Bool: true, Valid: true},
				FirebirdParameterMechanism: sql.NullInt64{Int64: 1, Valid: true},
				FirebirdTypeOfFieldName:    sql.NullString{String: "ID", Valid: true},
				FirebirdTypeOfRelationName: sql.NullString{String: "FB_CUSTOMER", Valid: true},
			},
			want: `"P_ID" TYPE OF COLUMN "FB_CUSTOMER"."ID"`,
		},
		{
			name: "domain",
			parameter: ProcedureParameter{
				Name: "P_VALUE", ProcedureName: "FB_PROC", Number: sql.NullInt64{Int64: 0, Valid: true},
				Direction: ParameterInput, ParameterType: sql.NullInt64{Int64: 0, Valid: true},
				FieldSource:                sql.NullString{String: "FB_DOMAIN", Valid: true},
				Domain:                     &Domain{Name: "FB_DOMAIN", SystemFlag: sql.NullInt64{Int64: 0, Valid: true}, FieldType: sql.NullInt64{Int64: fieldTypeInteger, Valid: true}},
				Nullable:                   sql.NullBool{Bool: true, Valid: true},
				FirebirdParameterMechanism: sql.NullInt64{Int64: 1, Valid: true},
			},
			want: `"P_VALUE" TYPE OF "FB_DOMAIN"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			procedure := Procedure{
				Name: "FB_PROC", Source: sql.NullString{String: "AS BEGIN END", Valid: true},
				InputCount: sql.NullInt64{Int64: 1, Valid: true}, OutputCount: sql.NullInt64{Int64: 0, Valid: true},
				InputParameters: []ProcedureParameter{test.parameter},
			}
			ddl, err := procedure.GenerateDDLWithOptions(DDLOptions{Firebird: true})
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(ddl, test.want) {
				t.Fatalf("Firebird DDL missing %q:\n%s", test.want, ddl)
			}
			if test.name == "column" {
				interBaseDDL, err := procedure.GenerateDDLWithOptions(DDLOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if strings.Contains(interBaseDDL, "TYPE OF") {
					t.Fatalf("InterBase DDL unexpectedly used Firebird parameter metadata:\n%s", interBaseDDL)
				}
			}
		})
	}
}
