package psql

import (
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestExtractPSQLBody(t *testing.T) {
	body := "\r\ndeclare variable cnt integer;\r\nbegin\r\n" +
		"  /* comment; with :colon */\r\n" +
		"  select count(*) from cash_batches where entity_id=:entity_id and note = 'a;b:c' into :cnt;\r\n" +
		"  if (cnt=0) then insert into cash_batches (entity_id) values (:entity_id); -- trailing ; comment\r\n" +
		"  for select a.location_id, count(*) from unit_totals_view a where a.location_id<>'00'\r\n" +
		"    group by a.location_id into :location_id, :cnt do begin\r\n" +
		"      update table_rules set cnt = :cnt where table_name = :location_id;\r\n" +
		"      suspend;\r\n    end\r\n" +
		"  execute procedure refresh_table_rules :entity_id;\r\n" +
		"  execute statement 'delete from x';\r\n" +
		"  select x from get_ids(:entity_id) g join items i on i.id = g.id into :cnt;\r\n" +
		"  delete from form_templates where template_name = :v_template_name;\r\n" +
		"end"

	want := []Statement{
		{Line: 5, Kind: "select", SQL: "select count(*) from cash_batches where entity_id=? and note = 'a;b:c'", Sources: []string{"CASH_BATCHES"}},
		{Line: 6, Kind: "insert", SQL: "insert into cash_batches (entity_id) values (?)", Target: "CASH_BATCHES"},
		{Line: 7, Kind: "forSelect", SQL: "select a.location_id, count(*) from unit_totals_view a where a.location_id<>'00'\r\n    group by a.location_id", Sources: []string{"UNIT_TOTALS_VIEW"}},
		{Line: 9, Kind: "update", SQL: "update table_rules set cnt = ? where table_name = ?", Target: "TABLE_RULES"},
		{Line: 12, Kind: "executeProcedure", Procedure: "REFRESH_TABLE_RULES"},
		{Line: 13, Kind: "executeStatement"},
		{Line: 14, Kind: "select", SQL: "select x from get_ids(?) g join items i on i.id = g.id", Sources: []string{"GET_IDS", "ITEMS"}, Calls: []string{"GET_IDS"}},
		{Line: 15, Kind: "delete", SQL: "delete from form_templates where template_name = ?", Target: "FORM_TEMPLATES"},
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
			sql:  "select * from feature_summary_view",
			want: Statement{Line: 1, Kind: "select", SQL: "select * from feature_summary_view", Sources: []string{"FEATURE_SUMMARY_VIEW"}},
		},
		{
			name: "input SQL is preserved exactly",
			sql:  "\n  select * from feature_summary_view; \n",
			want: Statement{Line: 2, Kind: "select", SQL: "\n  select * from feature_summary_view; \n", Sources: []string{"FEATURE_SUMMARY_VIEW"}},
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

func TestExtractRemovesSelectIntoLists(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		kind    string
		wantSQL string
	}{
		{
			name: "trigger NEW target in nested IF block",
			body: "AS BEGIN IF (NEW.ENTITY_ID <> OLD.ENTITY_ID) THEN BEGIN " +
				"SELECT PAYMENT_TERM FROM ENTITY_ACCOUNTS WHERE ENTITY_ID = :entity_id INTO NEW.PAYMENT_TERM; END END",
			kind: "select", wantSQL: "SELECT PAYMENT_TERM FROM ENTITY_ACCOUNTS WHERE ENTITY_ID = ?",
		},
		{
			name: "WHEN DO block with colon target",
			body: "AS BEGIN WHEN SQLCODE -1 DO BEGIN " +
				"SELECT EXCHANGE_RATE FROM CURRENCIES WHERE CURRENCY_ID = :currency_id INTO :currencyValue; END END",
			kind: "select", wantSQL: "SELECT EXCHANGE_RATE FROM CURRENCIES WHERE CURRENCY_ID = ?",
		},
		{
			name: "multiple NEW and OLD targets",
			body: "AS BEGIN IF (NEW.SKUID > 0) THEN BEGIN " +
				"select product_id,dimension_1 from stock_levels where stock_id = :stock_id INTO NEW.product_id, old.dimension_1; END END",
			kind: "select", wantSQL: "select product_id,dimension_1 from stock_levels where stock_id = ?",
		},
		{
			name: "multiple colon-prefixed targets",
			body: "AS BEGIN SELECT ADDRESS_LINE_1, ADDRESS_LINE_2, CITY, REGION, COUNTRY_ID, POSTAL_CODE, TAX_ZONE " +
				"FROM ENTITY_ADDRESSES WHERE ADDRESS_ID = :address_id INTO :ADDRESS_LINE_1, :ADDRESS_LINE_2, :CITY, " +
				":REGION, :COUNTRY_ID, :POSTAL_CODE, :TAX_ZONE; END",
			kind: "select", wantSQL: "SELECT ADDRESS_LINE_1, ADDRESS_LINE_2, CITY, REGION, COUNTRY_ID, POSTAL_CODE, TAX_ZONE " +
				"FROM ENTITY_ADDRESSES WHERE ADDRESS_ID = ?",
		},
		{
			name: "FOR SELECT with function arguments",
			body: "AS BEGIN FOR Select ControlId,UnitPrice,PriceTier1 from GET_UNIT_PRICE ( :a, :b, :c, :d, :e ) " +
				"into :control_id,:unit_price,:price_tier_1 DO BEGIN SUSPEND; END END",
			kind: "forSelect", wantSQL: "Select ControlId,UnitPrice,PriceTier1 from GET_UNIT_PRICE ( ?, ?, ?, ?, ? )",
		},
		{
			name: "colon and variable separated by whitespace",
			body: "AS BEGIN select currency_id from sales_regions where region_id = :region_id into : default_currency; END",
			kind: "select", wantSQL: "select currency_id from sales_regions where region_id = ?",
		},
		{
			name: "bare output variable after IF",
			body: "AS BEGIN IF (NEW.PRODUCTID IS NOT NULL) THEN BEGIN " +
				"select count(*) from products where cost_type='S' and product_id=:product_id into has_cost; END END",
			kind: "select", wantSQL: "select count(*) from products where cost_type='S' and product_id=?",
		},
		{
			name: "bare output variable after WHEN DO",
			body: "AS BEGIN WHEN ANY DO BEGIN Select ResultValue from F_EXECUTE('SAMPLE_ACTION') into result_value; END END",
			kind: "select", wantSQL: "Select ResultValue from F_EXECUTE('SAMPLE_ACTION')",
		},
		{
			name: "bare output variable matches selected column",
			body: "AS BEGIN SELECT ID FROM ORDER_LINE_HISTORY WHERE HISTORY_ID = :history_id AND LINE_ID = :line_id INTO ID; END",
			kind: "select", wantSQL: "SELECT ID FROM ORDER_LINE_HISTORY WHERE HISTORY_ID = ? AND LINE_ID = ?",
		},
		{
			name: "question-mark output variable and literal predicate",
			body: "AS BEGIN select debit_region from sales_regions where region_id = 'X1' into ?; END",
			kind: "select", wantSQL: "select debit_region from sales_regions where region_id = 'X1'",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Extract(tt.body)
			if len(got) != 1 {
				t.Fatalf("Extract() returned %d statements, want 1: %#v", len(got), got)
			}
			if got[0].Kind != tt.kind || got[0].SQL != tt.wantSQL {
				t.Fatalf("Extract() statement = (%q, %q), want (%q, %q)", got[0].Kind, got[0].SQL, tt.kind, tt.wantSQL)
			}
		})
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
	sql := "SELECT * FROM ENTITY_MASTER E JOIN HEADER_LINES AS H ON H.ENTITY_ID=E.ENTITY_ID LEFT OUTER JOIN IDENTITIES I ON I.ID=H.ACTOR_ID, REGIONS WHERE 1=1"
	want := map[string]string{
		"E": "ENTITY_MASTER", "ENTITY_MASTER": "ENTITY_MASTER",
		"H": "HEADER_LINES", "HEADER_LINES": "HEADER_LINES",
		"I": "IDENTITIES", "IDENTITIES": "IDENTITIES",
		"REGIONS": "REGIONS",
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

func TestAliasScopesResolveDerivedTableChains(t *testing.T) {
	// The outer aliases deliberately collide with the aliases used inside the
	// derived lookup tables, as required by this regression fixture.
	sql, err := os.ReadFile("../testdata/scoped-alias-query.sql")
	if err != nil {
		t.Fatalf("read scoped alias SQL fixture: %v", err)
	}
	scopes := AnalyzeAliases(string(sql))
	tests := []struct {
		chain []string
		want  string
	}{
		{[]string{"LKP_64", "B"}, "PRODUCT_GROUPS"},
		{[]string{"LKP_64", "A"}, "PRODUCT_ITEMS"},
		{[]string{"LKP_29", "A"}, "COLOR_ATTRIBUTES"},
		{[]string{"LKP_29", "B"}, "COLOR_CLASSES"},
		{[]string{"SUB_Q", "A"}, "PRODUCTS"},
		{[]string{"SUB_Q", "B"}, "VENDOR_ACCOUNTS"},
		{[]string{"LKP_3", "PRODUCT_ATTRIBUTES"}, "PRODUCT_ATTRIBUTES"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.chain, " "), func(t *testing.T) {
			if got, ok := scopes.Resolve(tt.chain); !ok || got != tt.want {
				t.Fatalf("Resolve(%v) = (%q, %t), want (%q, true)", tt.chain, got, ok, tt.want)
			}
		})
	}
}

func TestAliasScopesDoNotGuessAmbiguousSingleAlias(t *testing.T) {
	scopes := AnalyzeAliases(`SELECT * FROM first_table A WHERE ID IN (SELECT ID FROM second_table A)`)
	if got, ok := scopes.Resolve([]string{"A"}); ok {
		t.Fatalf("Resolve(A) = %q, want unresolved because scopes disagree", got)
	}
}

func TestAliasScopesResolveUnionBranchesIndependently(t *testing.T) {
	scopes := AnalyzeAliases(`SELECT * FROM (SELECT a.id FROM first_table a UNION SELECT b.id FROM second_table b) d`)
	for _, test := range []struct{ label, want string }{{"A", "FIRST_TABLE"}, {"B", "SECOND_TABLE"}} {
		if got, ok := scopes.Resolve([]string{"D", test.label}); !ok || got != test.want {
			t.Errorf("Resolve(D %s) = (%q, %t), want (%q, true)", test.label, got, ok, test.want)
		}
	}
}

func TestAliasScopesExposeAllTopLevelUnionSources(t *testing.T) {
	scopes := AnalyzeAliases(`SELECT a.x FROM T1 a UNION SELECT b.y FROM T2 b`)
	if got, want := scopes.TopLevelNames(), []string{"T1", "A", "T2", "B"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("TopLevelNames() = %v, want %v", got, want)
	}
	if !scopes.IsTopLevelName("B") {
		t.Fatal("IsTopLevelName(B) = false, want true for the second UNION branch")
	}
}

func TestAliasScopesPreserveQuotedNamesAndASCIIOnlyFolding(t *testing.T) {
	scopes := AnalyzeAliases("SELECT * FROM \"MixedTable\" \"Alias\" JOIN caf\xe9 c ON 1=1")
	if got, ok := scopes.Resolve([]string{"Alias"}); !ok || got != "MixedTable" {
		t.Fatalf("Resolve(Alias) = (%q, %t), want exact quoted relation", got, ok)
	}
	if got, ok := scopes.Resolve([]string{"CAF\xe9"}); !ok || got != "CAF\xe9" {
		t.Fatalf("Resolve(CP1252 relation) = (%q, %t), want preserved bytes", got, ok)
	}
	if _, ok := scopes.Resolve([]string{"caf\xea"}); ok {
		t.Fatal("Resolve() equated distinct high bytes")
	}
}

func TestAliasScopesMatchQuotedIdentifiersExactly(t *testing.T) {
	scopes := AnalyzeAliases(`SELECT * FROM first_table "Mixed" WHERE ID IN (SELECT ID FROM second_table "MIXED")`)
	for _, test := range []struct{ name, want string }{{"Mixed", "FIRST_TABLE"}, {"MIXED", "SECOND_TABLE"}} {
		if got, ok := scopes.Resolve([]string{test.name}); !ok || got != test.want {
			t.Errorf("Resolve(%q) = (%q, %t), want (%q, true)", test.name, got, ok, test.want)
		}
	}
}
