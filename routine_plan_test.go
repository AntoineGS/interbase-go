package interbase

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"interbase-go/planparse"
	"interbase-go/psql"
)

func TestDescribePlanPreservesParsedTreeWhenCatalogTransactionFails(t *testing.T) {
	description, err := DescribePlan(context.Background(), nil, "SELECT * FROM CUSTOMER", "PLAN (CUSTOMER NATURAL)")
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
			aliases: map[string]string{"C": "CUSTOMER"},
			want:    "CUSTOMER",
		},
		{
			name:      "single relation label",
			node:      &planparse.Node{Kind: "table", Labels: []string{"CUSTOMER"}},
			relations: map[string]bool{"CUSTOMER": true},
			want:      "CUSTOMER",
		},
		{
			name:      "explicit alias beats same-named relation",
			node:      &planparse.Node{Kind: "table", Labels: []string{"C"}},
			aliases:   map[string]string{"C": "CUSTOMER"},
			relations: map[string]bool{"C": true, "CUSTOMER": true},
			want:      "CUSTOMER",
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
			aliases:   map[string]string{"V": "CUSTOMER"},
			relations: map[string]bool{"CUSTOMER": true},
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
		{name: "quoted is exact", in: lookupName{name: "customer", quoted: true}, want: []string{"customer"}},
		{name: "unquoted is canonicalized", in: lookupName{name: "customer"}, want: []string{"CUSTOMER"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lookupNameVariants([]lookupName{tt.in}); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("lookupNameVariants(%+v) = %v, want %v", tt.in, got, tt.want)
			}
		})
	}
}

func TestPlanLookupNamesPreserveQuotedIdentifiers(t *testing.T) {
	raw := `PLAN ("customer" INDEX ("ixCustomer"))`
	tree, err := planparse.Parse(raw)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	quotedNames := quotedIdentifierSet(`SELECT * FROM "customer"`, raw)
	if got, want := planRelationLabels(tree, quotedNames), []lookupName{{name: "customer", quoted: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planRelationLabels() = %v, want %v", got, want)
	}
	if got, want := planIndexNames(tree, quotedNames), []lookupName{{name: "ixCustomer", quoted: true}}; !reflect.DeepEqual(got, want) {
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
	if got, want := planRoutineCandidates(statement), []lookupName{{name: "P"}, {name: "C", quoted: true}}; !reflect.DeepEqual(got, want) {
		t.Fatalf("planRoutineCandidates() = %v, want %v", got, want)
	}
}
