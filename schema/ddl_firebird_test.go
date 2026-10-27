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
