package interbase

import (
	"context"
	"errors"
	"os"
	"reflect"
	"testing"

	"interbase-go/planparse"
	"interbase-go/psql"
)

func TestMainPlanOmittedDetection(t *testing.T) {
	scopedSQL, err := os.ReadFile("testdata/scoped-alias-query.sql")
	if err != nil {
		t.Fatalf("read scoped alias SQL fixture: %v", err)
	}
	tests := []struct {
		name      string
		sql       string
		raw       string
		want      bool
		roots     int
		relations []string
	}{
		{
			name: "main query root is last",
			sql: "select a.product_id, (select max(x.class_name) from product_classes x where x.class_id=a.class_id), " +
				"(select count(*) from currencies) from products a join product_classes pc on pc.class_id=a.class_id " +
				"where a.product_id in (select product_id from product_items)",
			raw: "PLAN (PRODUCT_ITEMS INDEX (RDB$FOREIGN9101))\n" +
				"PLAN (CURRENCIES NATURAL)\n" +
				"PLAN (X INDEX (RDB$PRIMARY9102))\n" +
				"PLAN JOIN (PC NATURAL,A INDEX (RDB$FOREIGN9103))",
			want: false, roots: 4, relations: []string{"PRODUCTS", "PRODUCT_CLASSES"},
		},
		{
			name: "synthetic query omitted main root after eleven subquery plans",
			sql:  string(scopedSQL),
			raw: "PLAN JOIN (LKP_64 B NATURAL,LKP_64 A INDEX (RDB$PRIMARY9104))\n" +
				"PLAN JOIN (LKP_29 A NATURAL,LKP_29 B INDEX (RDB$PRIMARY9105))\n" +
				"PLAN (LKP_1 BRANDS NATURAL)\n" +
				"PLAN (LKP_5 VENDORS NATURAL)\n" +
				"PLAN (LKP_6 VENDOR_ACCOUNTS NATURAL)\n" +
				"PLAN (LKP_12 CATEGORIES NATURAL)\n" +
				"PLAN (LKP_41 CURRENCIES NATURAL)\n" +
				"PLAN (LKP_55 PRODUCTS NATURAL)\n" +
				"PLAN (LKP_56 STOCK_LEVELS NATURAL)\n" +
				"PLAN (Y NATURAL)\n" +
				"PLAN (X INDEX (RDB$PRIMARY9106))",
			want: true, roots: 11, relations: []string{"PRODUCTS", "VENDOR_ACCOUNTS", "VENDORS", "PRODUCT_GROUPS", "BRANDS", "CATEGORIES", "CURRENCIES", "COLOR_ATTRIBUTES", "COLOR_CLASSES", "COLORS", "STOCK_LEVELS", "LOCATIONS", "UNIT_TYPES", "PRODUCT_ATTRIBUTES", "PRODUCT_ITEMS"},
		},
		{
			name: "selectable procedure plan is not an omitted main relation plan",
			sql:  "select * from FEATURE_SUMMARY_VIEW",
			raw:  "PLAN (UNIT_TOTALS_VIEW NATURAL)(UNIT_TOTALS_VIEW NATURAL)SORT ((A NATURAL))",
			want: false, roots: 3,
		},
		{
			name: "system relation plan matches main query",
			sql:  "SELECT 1 FROM RDB$DATABASE",
			raw:  "PLAN (RDB$DATABASE NATURAL)",
			want: false, roots: 1, relations: []string{"RDB$DATABASE"},
		},
		{
			name: "union second branch plan matches main query",
			sql:  "SELECT a.x FROM T1 a UNION SELECT b.y FROM T2 b",
			raw:  "PLAN (B NATURAL)",
			want: false, roots: 1, relations: []string{"T1", "T2"},
		},
		{
			name: "union plans for both branches match main query",
			sql:  "SELECT a.x FROM T1 a UNION SELECT b.y FROM T2 b",
			raw:  "PLAN (A NATURAL)\nPLAN (B NATURAL)",
			want: false, roots: 2, relations: []string{"T1", "T2"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tree, err := planparse.Parse(tt.raw)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if len(tree.Roots) != tt.roots {
				t.Fatalf("plan roots = %d, want %d", len(tree.Roots), tt.roots)
			}
			relationSet := make(map[string]struct{}, len(tt.relations))
			for _, relation := range tt.relations {
				relationSet[relation] = struct{}{}
			}
			if got := mainPlanOmitted(psql.AnalyzeAliases(tt.sql), tree, func(name string) bool {
				_, ok := relationSet[name]
				return ok
			}); got != tt.want {
				t.Fatalf("mainPlanOmitted() = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestResolvePlanViewChainRecursesThroughNestedViews(t *testing.T) {
	scopes := psql.AnalyzeAliases("SELECT * FROM RP_OUTER")
	views := planViewContexts{
		"RP_OUTER": {"A": "RP_INNER"},
		"RP_INNER": {"X": "RP_TABLE"},
	}
	if got, ok := resolvePlanViewChain([]string{"RP_OUTER", "A", "X"}, scopes, views, nil); !ok || got != "RP_TABLE" {
		t.Fatalf("resolvePlanViewChain() = (%q, %t), want (RP_TABLE, true)", got, ok)
	}
}

func TestResolvePlanRelationsUsesLastAliasForBaseRelationChains(t *testing.T) {
	node := &planparse.Node{Kind: "table", Labels: []string{"SOURCE_TABLE", "C"}}
	tree := &planparse.Tree{Roots: []*planparse.Node{node}}
	resolvePlanRelations(tree, psql.AnalyzeAliases("SELECT * FROM SOURCE_TABLE C"), nil, nil, nil)
	if node.Relation != "SOURCE_TABLE" {
		t.Fatalf("base relation chain resolved to %q, want SOURCE_TABLE", node.Relation)
	}
}

func TestDescribePlanPreservesParsedTreeWhenCatalogTransactionFails(t *testing.T) {
	description, err := DescribePlan(context.Background(), nil, "SELECT * FROM ENTITY_MASTER", "PLAN (ENTITY_MASTER NATURAL)")
	if !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("DescribePlan error = %v, want ErrNotInterBaseConn", err)
	}
	if description.Tree == nil {
		t.Fatal("DescribePlan discarded the successfully parsed tree when catalog lookup failed")
	}
}

func TestResolveRelations(t *testing.T) {
	tests := []struct {
		name      string
		node      *planparse.Node
		aliases   map[string]string
		relations map[string]bool
		want      string
	}{
		{
			name:    "single label alias",
			node:    &planparse.Node{Kind: "table", Labels: []string{"C"}},
			aliases: map[string]string{"C": "ENTITY_MASTER"},
			want:    "ENTITY_MASTER",
		},
		{
			name:      "single relation label",
			node:      &planparse.Node{Kind: "table", Labels: []string{"ENTITY_MASTER"}},
			relations: map[string]bool{"ENTITY_MASTER": true},
			want:      "ENTITY_MASTER",
		},
		{
			name:      "explicit alias beats same-named relation",
			node:      &planparse.Node{Kind: "table", Labels: []string{"C"}},
			aliases:   map[string]string{"C": "ENTITY_MASTER"},
			relations: map[string]bool{"C": true, "ENTITY_MASTER": true},
			want:      "ENTITY_MASTER",
		},
		{
			name:      "last label in relation chain",
			node:      &planparse.Node{Kind: "table", Labels: []string{"V", "B"}},
			relations: map[string]bool{"B": true},
			want:      "B",
		},
		{
			name:      "unknown last label does not fall back to earlier alias",
			node:      &planparse.Node{Kind: "table", Labels: []string{"V", "UNKNOWN"}},
			aliases:   map[string]string{"V": "ENTITY_MASTER"},
			relations: map[string]bool{"ENTITY_MASTER": true},
		},
		{
			name:    "CP1252 alias keeps high byte during lookup",
			node:    &planparse.Node{Kind: "table", Labels: []string{"caf\xe9"}},
			aliases: map[string]string{"CAF\xe9": "ENTITY_MASTER"},
			want:    "ENTITY_MASTER",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resolveRelations(&planparse.Tree{Roots: []*planparse.Node{tt.node}}, tt.aliases,
				func(name string) bool { return tt.relations[name] })
			if got := tt.node.Relation; got != tt.want {
				t.Fatalf("Relation = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLookupNameVariantsPreservesQuotedNames(t *testing.T) {
	tests := []struct {
		name string
		in   lookupName
		want []string
	}{
		{name: "quoted is exact", in: lookupName{name: "entity_master", quoted: true}, want: []string{"entity_master"}},
		{name: "unquoted is canonicalized", in: lookupName{name: "entity_master"}, want: []string{"ENTITY_MASTER"}},
		{name: "CP1252 bytes are preserved", in: lookupName{name: "caf\xe9"}, want: []string{"CAF\xe9"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lookupNameVariants([]lookupName{tt.in}); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("lookupNameVariants(%+v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestCatalogNameKeyAndLookupIgnoreCaseOnlyForASCII(t *testing.T) {
	if got, want := catalogNameKey("caf\xe9 "), "CAF\xe9"; got != want {
		t.Fatalf("catalogNameKey() = %q, want %q", got, want)
	}
	if sameLookupName("caf\xe9", "caf\xea", false) {
		t.Fatal("sameLookupName() equated distinct invalid UTF-8 identifier bytes")
	}
}

func TestPlanLookupNamesPreserveQuotedIdentifiers(t *testing.T) {
	raw := `PLAN ("entity_master" INDEX ("ixEntityMaster"))`
	tree, err := planparse.Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	quotedNames := quotedIdentifierSet(`SELECT * FROM "entity_master"`, raw)
	if got, want := planRelationLabels(tree, quotedNames), []lookupName{{name: "entity_master", quoted: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planRelationLabels() = %v, want %v", got, want)
	}
	if got, want := planIndexNames(tree, quotedNames), []lookupName{{name: "ixEntityMaster", quoted: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planIndexNames() = %v, want %v", got, want)
	}
}

func TestTriggerEvent(t *testing.T) {
	tests := []struct {
		typ  int
		want string
		ok   bool
	}{
		{typ: 1, want: "before insert", ok: true},
		{typ: 2, want: "after insert", ok: true},
		{typ: 3, want: "before update", ok: true},
		{typ: 4, want: "after update", ok: true},
		{typ: 5, want: "before delete", ok: true},
		{typ: 6, want: "after delete", ok: true},
		{typ: 0},
		{typ: 7},
	}
	for _, tt := range tests {
		got, ok := triggerEvent(tt.typ)
		if got != tt.want || ok != tt.ok {
			t.Errorf("triggerEvent(%d) = (%q, %t), want (%q, %t)", tt.typ, got, ok, tt.want, tt.ok)
		}
	}
}

func TestLaneFromStatement(t *testing.T) {
	t.Run("dynamic SQL", func(t *testing.T) {
		lane := laneFromStatement(psql.Statement{Line: 4, Kind: "executeStatement"})
		if lane.Message != "dynamic SQL" || lane.SQL != "" || len(lane.Refs) != 0 {
			t.Fatalf("laneFromStatement(executeStatement) = %+v", lane)
		}
	})

	t.Run("execute procedure", func(t *testing.T) {
		lane := laneFromStatement(psql.Statement{Line: 7, Kind: "executeProcedure", Procedure: "P"})
		if lane.SQL != "" || lane.Message != "" || lane.Plan.Raw != "" || lane.Plan.Tree != nil {
			t.Fatalf("executeProcedure lane should have no SQL, plan, or message: %+v", lane)
		}
		want := []RoutineRef{{Kind: "procedure", Name: "P"}}
		if !reflect.DeepEqual(lane.Refs, want) {
			t.Fatalf("Refs = %+v, want %+v", lane.Refs, want)
		}
	})
}

func TestRoutineCandidatesUseSourcesOrCallsByContext(t *testing.T) {
	statement := psql.Statement{
		Procedure:     "P",
		Sources:       []string{"S"},
		Calls:         []string{"C"},
		QuotedSources: []string{"S"},
		QuotedCalls:   []string{"C"},
	}
	if got, want := statementRoutineCandidates(statement), []lookupName{{name: "P"}, {name: "S", quoted: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("statementRoutineCandidates() = %v, want %v", got, want)
	}
	if got, want := planRoutineCandidates(statement), []lookupName{{name: "P"}, {name: "S", quoted: true}, {name: "C", quoted: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planRoutineCandidates() = %v, want %v", got, want)
	}
}

func TestDeduplicateRoutineRefsPreservesExactCatalogIdentity(t *testing.T) {
	refs := []RoutineRef{
		{Kind: "procedure", Name: "Foo"},
		{Kind: "procedure", Name: "FOO"},
		{Kind: "procedure", Name: "Foo"},
		{Kind: "trigger", Name: "Foo", Relation: "Bar"},
		{Kind: "trigger", Name: "Foo", Relation: "BAR"},
	}
	want := []RoutineRef{
		{Kind: "procedure", Name: "Foo"},
		{Kind: "procedure", Name: "FOO"},
		{Kind: "trigger", Name: "Foo", Relation: "Bar"},
		{Kind: "trigger", Name: "Foo", Relation: "BAR"},
	}
	if got := deduplicateRoutineRefs(refs); !reflect.DeepEqual(got, want) {
		t.Fatalf("deduplicateRoutineRefs() = %+v, want %+v", got, want)
	}
}
