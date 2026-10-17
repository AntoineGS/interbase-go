package planparse

import (
	"strings"
	"testing"
)

func TestParsePlanShapes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want string
	}{
		{"bare group", "PLAN (A NATURAL)", "group(table[A]natural)"},
		{"nested sort and join", "\nPLAN SORT (JOIN (C NATURAL, H INDEX (IX_HEADER_ENTITY), U INDEX (IX_IDENTITIES_PK)))", "sort(join(table[C]natural,table[H]index(IX_HEADER_ENTITY),table[U]index(IX_IDENTITIES_PK)))"},
		{"join indexes", "PLAN JOIN (A INDEX (X, Y), B NATURAL)", "join(table[A]index(X,Y),table[B]natural)"},
		{"merge sorts", "PLAN MERGE (SORT (A NATURAL), SORT (B NATURAL))", "merge(sort(table[A]natural),sort(table[B]natural))"},
		{"nested merge", "PLAN SORT (MERGE (SORT (A NATURAL), SORT (B NATURAL)))", "sort(merge(sort(table[A]natural),sort(table[B]natural)))"},
		{"order", "PLAN (T ORDER PK_T)", "group(table[T]order(PK_T))"},
		{"order and index", "PLAN (T ORDER PK_T INDEX (IX_T))", "group(table[T]order(PK_T)index(IX_T))"},
		{"alias chain", "PLAN JOIN (V A NATURAL, V B INDEX (PK_B))", "join(table[V A]natural,table[V B]index(PK_B))"},
		{"multiple plan roots", "PLAN (B INDEX (PK_B))\nPLAN (A NATURAL)", "group(table[B]index(PK_B)) | group(table[A]natural)"},
		{"multiple procedure groups", "\nPLAN (UNIT_TOTALS_VIEW NATURAL)(UNIT_TOTALS_VIEW NATURAL)SORT ((A NATURAL))", "group(table[UNIT_TOTALS_VIEW]natural) | group(table[UNIT_TOTALS_VIEW]natural) | sort(group(table[A]natural))"},
		{"whitespace only", "   ", ""},
		{"quoted identifiers and keyword case", `plan join ("VaL" Alias natural, "CaSe" index ("Ix Name", second$), "T" order "Ord Name" index ("I_X"))`, `join(table[VaL Alias]natural,table[CaSe]index(Ix Name,second$),table[T]order(Ord Name)index(I_X))`},
		{"CP1252 identifier bytes", "PLAN (T\xe9 INDEX (IX_\xe9))", "group(table[T\xe9]index(IX_\xe9))"},
		{"UTF-8 identifier", "PLAN (Té NATURAL)", "group(table[Té]natural)"},
		{"empty quoted index name", `PLAN (ORDER_STORE_SETTINGS NATURAL)JOIN (A INDEX (""),B INDEX (RDB$PRIMARY9100))`,
			"group(table[ORDER_STORE_SETTINGS]natural) | join(table[A]index(),table[B]index(RDB$PRIMARY9100))"},
		{"nested PLAN subplans in lists", `PLAN (CATALOG_ID ORDER RDB$PRIMARY9101)(CONTROL_CONFIG INDEX (IX_CONTROL_REGION))SORT ((PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1)))SORT ((PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1))
PLAN (PRICE_BOOKS INDEX (IX_PRICE_BOOKS_1)))`,
			"group(table[CATALOG_ID]order(RDB$PRIMARY9101)) | group(table[CONTROL_CONFIG]index(IX_CONTROL_REGION)) | " +
				"sort(group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1)),group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1))," +
				"group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1)),group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1))," +
				"group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1))) | " +
				"sort(group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1)),group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1))," +
				"group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1)),group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1))," +
				"group(table[PRICE_BOOKS]index(IX_PRICE_BOOKS_1)))"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tree, err := Parse(tc.raw)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if tree == nil {
				t.Fatal("Parse() returned a nil tree")
			}
			if got := renderTree(tree); got != tc.want {
				t.Errorf("Parse() shape = %q, want %q", got, tc.want)
			}
			if !relationsEmpty(tree.Roots) {
				t.Error("Parse() populated a relation name")
			}
		})
	}
}

func TestParsePlanKeepsEmptyQuotedIndexName(t *testing.T) {
	tree, err := Parse(`PLAN (ORDER_STORE_SETTINGS NATURAL)JOIN (A INDEX (""),B INDEX (RDB$PRIMARY9100))`)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	got := tree.Roots[1].Children[0].Indexes
	if len(got) != 1 || got[0] != "" {
		t.Fatalf("empty index name = %#v, want []string{\"\"}", got)
	}
}

func TestParseRejectsUnsupportedPlans(t *testing.T) {
	for _, raw := range []string{
		"PLAN (A FOO)",
		"PLAN (A NATURAL",
		"PLAN JOIN A NATURAL",
		"PLAN (A INDEX ())",
		`PLAN ("" NATURAL)`,
		`PLAN (T ORDER "")`,
		"PLAN",
		"SELECT 1",
	} {
		t.Run(raw, func(t *testing.T) {
			tree, err := Parse(raw)
			if err == nil {
				t.Fatal("Parse() error = nil, want an error")
			}
			if tree != nil {
				t.Errorf("Parse() tree = %#v, want nil on error", tree)
			}
		})
	}
}

func renderTree(tree *Tree) string {
	parts := make([]string, len(tree.Roots))
	for i, root := range tree.Roots {
		parts[i] = renderNode(root)
	}
	return strings.Join(parts, " | ")
}

func renderNode(node *Node) string {
	if node.Kind == "table" {
		result := "table[" + strings.Join(node.Labels, " ") + "]" + node.Access
		switch node.Access {
		case "index":
			result += "(" + strings.Join(node.Indexes, ",") + ")"
		case "order":
			result += "(" + node.OrderIndex + ")"
			if len(node.Indexes) > 0 {
				result += "index(" + strings.Join(node.Indexes, ",") + ")"
			}
		}
		return result
	}

	children := make([]string, len(node.Children))
	for i, child := range node.Children {
		children[i] = renderNode(child)
	}
	return node.Kind + "(" + strings.Join(children, ",") + ")"
}

func relationsEmpty(nodes []*Node) bool {
	for _, node := range nodes {
		if node.Relation != "" || !relationsEmpty(node.Children) {
			return false
		}
	}
	return true
}
