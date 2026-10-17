package psql

import (
	"reflect"
	"testing"
)

func TestExtractPSQLBody(t *testing.T) {
	body := "\r\ndeclare variable cnt integer;\r\nbegin\r\n" +
		"  /* comment; with :colon */\r\n" +
		"  select count(*) from cashunits where custid=:custid and note = 'a;b:c' into :cnt;\r\n" +
		"  if (cnt=0) then insert into cashunits (custid) values (:custid); -- trailing ; comment\r\n" +
		"  for select a.branchid, count(*) from cunits_vr a where a.branchid<>'00'\r\n" +
		"    group by a.branchid into :branchid, :cnt do begin\r\n" +
		"      update tbrules set cnt = :cnt where tablename = :branchid;\r\n" +
		"      suspend;\r\n    end\r\n" +
		"  execute procedure tbbrpolled_rebuild :custid;\r\n" +
		"  execute statement 'delete from x';\r\n" +
		"  select x from get_ids(:custid) g join items i on i.id = g.id into :cnt;\r\n" +
		"  delete from forms where formname = :v_formname;\r\n" +
		"end"

	want := []Statement{
		{Line: 5, Kind: "select", SQL: "select count(*) from cashunits where custid=? and note = 'a;b:c'", Sources: []string{"CASHUNITS"}},
		{Line: 6, Kind: "insert", SQL: "insert into cashunits (custid) values (?)", Target: "CASHUNITS"},
		{Line: 7, Kind: "forSelect", SQL: "select a.branchid, count(*) from cunits_vr a where a.branchid<>'00'\r\n    group by a.branchid", Sources: []string{"CUNITS_VR"}},
		{Line: 9, Kind: "update", SQL: "update tbrules set cnt = ? where tablename = ?", Target: "TBRULES"},
		{Line: 12, Kind: "executeProcedure", Procedure: "TBBRPOLLED_REBUILD"},
		{Line: 13, Kind: "executeStatement"},
		{Line: 14, Kind: "select", SQL: "select x from get_ids(?) g join items i on i.id = g.id", Sources: []string{"GET_IDS", "ITEMS"}, Calls: []string{"GET_IDS"}},
		{Line: 15, Kind: "delete", SQL: "delete from forms where formname = ?", Target: "FORMS"},
	}

	if got := Extract(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractPreservesCP1252IdentifierBytes(t *testing.T) {
	body := "begin\n select x from caf\xe9 where a = :p into :v;\nend"
	want := []Statement{{
		Line: 2, Kind: "select", SQL: "select x from caf\xe9 where a = ?", Sources: []string{"CAF\xe9"},
	}}
	if got := Extract(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractDoesNotTrimHighBytesFromIdentifier(t *testing.T) {
	body := "begin\n select x from caf\xc2\xa0;\nend"
	want := []Statement{{
		Line: 2, Kind: "select", SQL: "select x from caf\xc2\xa0", Sources: []string{"CAF\xc2\xa0"},
	}}
	if got := Extract(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractTriggerAndNestedSources(t *testing.T) {
	t.Run("trigger OLD and NEW references", func(t *testing.T) {
		body := "AS\nBEGIN\n  IF (NEW.QTY <> OLD.QTY) THEN\n    UPDATE STOCK SET QTY = QTY + NEW.QTY - OLD.QTY WHERE ID = NEW.ID;\nEND"
		want := []Statement{{
			Line: 4, Kind: "update", SQL: "UPDATE STOCK SET QTY = QTY + ? - ? WHERE ID = ?", Target: "STOCK",
		}}
		if got := Extract(body); !reflect.DeepEqual(got, want) {
			t.Fatalf("Extract() = %#v, want %#v", got, want)
		}
	})

	t.Run("subquery sources", func(t *testing.T) {
		body := "begin\n select a from t where b in (select c from u) into :a;\nend"
		want := []Statement{{Line: 2, Kind: "select", SQL: "select a from t where b in (select c from u)", Sources: []string{"T", "U"}}}
		if got := Extract(body); !reflect.DeepEqual(got, want) {
			t.Fatalf("Extract() = %#v, want %#v", got, want)
		}
	})
}

func TestExtractStatementRefs(t *testing.T) {
	tests := []struct {
		name string
		sql  string
		want Statement
	}{
		{
			name: "execute procedure preserves input SQL",
			sql:  "EXECUTE PROCEDURE P1 1, 2",
			want: Statement{Line: 1, Kind: "executeProcedure", Procedure: "P1", SQL: "EXECUTE PROCEDURE P1 1, 2"},
		},
		{
			name: "select sources",
			sql:  "select * from license_recap",
			want: Statement{Line: 1, Kind: "select", SQL: "select * from license_recap", Sources: []string{"LICENSE_RECAP"}},
		},
		{
			name: "input SQL is preserved exactly",
			sql:  "\n  select * from license_recap; \n",
			want: Statement{Line: 2, Kind: "select", SQL: "\n  select * from license_recap; \n", Sources: []string{"LICENSE_RECAP"}},
		},
		{
			name: "update target",
			sql:  "UPDATE t SET a=1",
			want: Statement{Line: 1, Kind: "update", SQL: "UPDATE t SET a=1", Target: "T"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ExtractStatementRefs(tt.sql); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ExtractStatementRefs(%q) = %#v, want %#v", tt.sql, got, tt.want)
			}
		})
	}
}

func TestExtractRemovesDMLReturningInto(t *testing.T) {
	body := "begin\n update stock set qty = :qty returning id into :new_id;\nend"
	want := []Statement{{Line: 2, Kind: "update", SQL: "update stock set qty = ? returning id", Target: "STOCK"}}
	if got := Extract(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestExtractQuotedIdentifiersKeepCase(t *testing.T) {
	got := ExtractStatementRefs(`SELECT * FROM "MixedTable" "Alias" JOIN lower_name L ON 1=1`)
	want := Statement{
		Line:          1,
		Kind:          "select",
		SQL:           `SELECT * FROM "MixedTable" "Alias" JOIN lower_name L ON 1=1`,
		Sources:       []string{"MixedTable", "LOWER_NAME"},
		QuotedSources: []string{"MixedTable"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExtractStatementRefs() = %#v, want %#v", got, want)
	}
}

func TestExtractPreservesQuotedIdentifierMetadata(t *testing.T) {
	body := `AS BEGIN SELECT * FROM "mixedTable"() JOIN lower_name L ON 1=1; END`
	got := Extract(body)
	if len(got) != 1 {
		t.Fatalf("Extract() returned %d statements, want 1", len(got))
	}
	if want := []string{"mixedTable", "LOWER_NAME"}; !reflect.DeepEqual(got[0].Sources, want) {
		t.Fatalf("Sources = %v, want %v", got[0].Sources, want)
	}
	if want := []string{"mixedTable"}; !reflect.DeepEqual(got[0].QuotedSources, want) {
		t.Fatalf("QuotedSources = %v, want %v", got[0].QuotedSources, want)
	}
	if want := []string{"mixedTable"}; !reflect.DeepEqual(got[0].QuotedCalls, want) {
		t.Fatalf("QuotedCalls = %v, want %v", got[0].QuotedCalls, want)
	}
}

func TestExtractStatementRefsMarksQuotedProcedure(t *testing.T) {
	got := ExtractStatementRefs(`EXECUTE PROCEDURE "MixedProc"`)
	if got.Procedure != "MixedProc" || !got.ProcedureQuoted {
		t.Fatalf("quoted procedure = (%q, %t), want (MixedProc, true)", got.Procedure, got.ProcedureQuoted)
	}
}

func TestQuotedIdentifiersIgnoresCommentsAndStringLiterals(t *testing.T) {
	sql := `SELECT "MixedName" FROM "lowerTable" WHERE note = '"not an identifier"' /* "not one either" */`
	want := []string{"MixedName", "lowerTable"}
	if got := QuotedIdentifiers(sql); !reflect.DeepEqual(got, want) {
		t.Fatalf("QuotedIdentifiers() = %v, want %v", got, want)
	}
}

func TestExtractIgnoresUnknownStatementStarts(t *testing.T) {
	body := "begin\n suspend;\n exit;\n cnt = 1;\nend"
	if got := Extract(body); len(got) != 0 {
		t.Fatalf("Extract() = %#v, want no data statements", got)
	}
}

func TestExtractLeavesQuotedTextAndCommentsUntouched(t *testing.T) {
	body := "begin\n select x from t where x = 'a'';:v' /* :ignored; */; -- :ignored;\nend"
	want := []Statement{{Line: 2, Kind: "select", SQL: "select x from t where x = 'a'';:v' /* :ignored; */", Sources: []string{"T"}}}
	if got := Extract(body); !reflect.DeepEqual(got, want) {
		t.Fatalf("Extract() = %#v, want %#v", got, want)
	}
}

func TestAliases(t *testing.T) {
	sql := "SELECT * FROM CUSTOMER C JOIN HD1 AS H ON H.CUSTID=C.CUSTID LEFT OUTER JOIN USERS U ON U.ID=H.USERID, BRANCH WHERE 1=1"
	want := map[string]string{
		"C": "CUSTOMER", "CUSTOMER": "CUSTOMER",
		"H": "HD1", "HD1": "HD1",
		"U": "USERS", "USERS": "USERS",
		"BRANCH": "BRANCH",
	}
	if got := Aliases(sql); !reflect.DeepEqual(got, want) {
		t.Fatalf("Aliases() = %#v, want %#v", got, want)
	}

	t.Run("quoted identifiers keep case", func(t *testing.T) {
		got := Aliases(`SELECT * FROM "MixedTable" AS "TableAlias"`)
		want := map[string]string{"MixedTable": "MixedTable", "TableAlias": "MixedTable"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Aliases() = %#v, want %#v", got, want)
		}
	})

	t.Run("CP1252 alias bytes are preserved", func(t *testing.T) {
		got := Aliases("SELECT * FROM table caf\xe9")
		want := map[string]string{"TABLE": "TABLE", "CAF\xe9": "TABLE"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("Aliases() = %#v, want %#v", got, want)
		}
	})
}
