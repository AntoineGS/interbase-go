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
		{"nested sort and join", "\nPLAN SORT (JOIN (C NATURAL, H INDEX (HD1_CUSTID), U INDEX (PK_USERS)))", "sort(join(table[C]natural,table[H]index(HD1_CUSTID),table[U]index(PK_USERS)))"},
		{"join indexes", "PLAN JOIN (A INDEX (X, Y), B NATURAL)", "join(table[A]index(X,Y),table[B]natural)"},
		{"merge sorts", "PLAN MERGE (SORT (A NATURAL), SORT (B NATURAL))", "merge(sort(table[A]natural),sort(table[B]natural))"},
		{"nested merge", "PLAN SORT (MERGE (SORT (A NATURAL), SORT (B NATURAL)))", "sort(merge(sort(table[A]natural),sort(table[B]natural)))"},
		{"order", "PLAN (T ORDER PK_T)", "group(table[T]order(PK_T))"},
		{"order and index", "PLAN (T ORDER PK_T INDEX (IX_T))", "group(table[T]order(PK_T)index(IX_T))"},
		{"alias chain", "PLAN JOIN (V A NATURAL, V B INDEX (PK_B))", "join(table[V A]natural,table[V B]index(PK_B))"},
		{"multiple plan roots", "PLAN (B INDEX (PK_B))\nPLAN (A NATURAL)", "group(table[B]index(PK_B)) | group(table[A]natural)"},
		{"multiple procedure groups", "\nPLAN (CUNITS_VR NATURAL)(CUNITS_VR NATURAL)SORT ((A NATURAL))", "group(table[CUNITS_VR]natural) | group(table[CUNITS_VR]natural) | sort(group(table[A]natural))"},
		{"whitespace only", "   ", ""},
		{"quoted identifiers and keyword case", `plan join ("VaL" Alias natural, "CaSe" index ("Ix Name", second$), "T" order "Ord Name" index ("I_X"))`, `join(table[VaL Alias]natural,table[CaSe]index(Ix Name,second$),table[T]order(Ord Name)index(I_X))`},
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

func TestParseRejectsUnsupportedPlans(t *testing.T) {
	for _, raw := range []string{
		"PLAN (A FOO)",
		"PLAN (A NATURAL",
		"PLAN JOIN A NATURAL",
		"PLAN (A INDEX ())",
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
