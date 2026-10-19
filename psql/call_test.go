package psql

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseEntryCall(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		dialect int
		want    EntryCall
	}{
		{
			name: "select star", sql: "select * from P(2183)",
			want: EntryCall{Procedure: "P", Selectable: true, Args: []string{"2183"}},
		},
		{
			name: "select columns from quoted procedure", sql: `SELECT a, b FROM "Mixed"(1, 'x')`,
			want: EntryCall{Procedure: "Mixed", Quoted: true, Selectable: true, Columns: []string{"A", "B"}, Args: []string{"1", "'x'"}},
		},
		{
			name: "execute without parentheses", sql: "execute procedure P 1, 2",
			want: EntryCall{Procedure: "P", Args: []string{"1", "2"}},
		},
		{
			name: "execute with parentheses", sql: "execute procedure P(1)",
			want: EntryCall{Procedure: "P", Args: []string{"1"}},
		},
		{
			name: "execute without arguments, comments and semicolon", sql: "/* run */\n  EXECUTE PROCEDURE p_one ; -- done\n",
			want: EntryCall{Procedure: "P_ONE"},
		},
		{
			name: "comments and semicolon around select", sql: "-- entry\nselect * from p_src(1, /* two */ 2);\n/* end */",
			want: EntryCall{Procedure: "P_SRC", Selectable: true, Args: []string{"1", "2"}},
		},
		{
			name: "constant expressions", sql: "execute procedure P(CAST('2020-01-01' AS DATE), NULL, -1, 'a' || 'b', f_x(2), current_date)",
			want: EntryCall{Procedure: "P", Args: []string{"CAST('2020-01-01' AS DATE)", "NULL", "-1", "'a' || 'b'", "f_x(2)", "current_date"}},
		},
		{
			name: "cast to a multi-word type", sql: "select * from P(cast('x' as varchar(10) character set none), cast(1 as double precision))",
			want: EntryCall{Procedure: "P", Selectable: true, Args: []string{"cast('x' as varchar(10) character set none)", "cast(1 as double precision)"}},
		},
		{
			name: "dialect 1 double quotes are strings", sql: `select * from P("x")`, dialect: 1,
			want: EntryCall{Procedure: "P", Selectable: true, Args: []string{`"x"`}},
		},
		{
			name: "select from a bare name", sql: "select a from P",
			want: EntryCall{Procedure: "P", Selectable: true, Bare: true, Columns: []string{"A"}},
		},
		{
			name: "string bytes kept", sql: "execute procedure P('caf\xe9')",
			want: EntryCall{Procedure: "P", Args: []string{"'caf\xe9'"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialect := tt.dialect
			if dialect == 0 {
				dialect = 3
			}
			got, err := ParseEntryCall(tt.sql, dialect)
			if err != nil {
				t.Fatalf("ParseEntryCall() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("ParseEntryCall() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseEntryCallRejects(t *testing.T) {
	tests := []struct {
		name    string
		sql     string
		dialect int
		reason  string
	}{
		{"parameter", "select * from P(?)", 3, "parameter"},
		{"colon parameter", "execute procedure P(:a)", 3, "parameter"},
		{"other clause", "select * from P(1) where a = 1", 3, "WHERE"},
		{"alias", "select * from P(1) x", 3, "X"},
		{"bare name with alias", "select * from T x", 3, "X"},
		{"two statements", "select * from P(1); select * from P(2)", 3, "one statement"},
		{"column reference", "execute procedure P(x)", 3, "column"},
		{"dialect 3 quoted identifier", `select * from P("x")`, 3, "column"},
		{"dotted reference", "execute procedure P(t.x)", 3, "column"},
		{"subquery", "select * from P((select 1 from rdb$database))", 3, "SELECT"},
		{"generator", "select * from P(gen_id(g, 1))", 3, "GEN_ID"},
		{"expression column", "select a + 1 from P(1)", 3, "column"},
		{"delete", "delete from t", 3, "EXECUTE PROCEDURE"},
		{"empty", "  /* nothing */ ", 3, "empty"},
		{"empty argument", "execute procedure P(1,,2)", 3, "argument"},
		{"unbalanced", "execute procedure P(1", 3, "argument"},
		{"execute statement", "execute statement 'x'", 3, "EXECUTE PROCEDURE"},
		{"returning values", "execute procedure P 1 returning_values :x", 3, "RETURNING_VALUES"},
		{"juxtaposed numbers", "execute procedure P 1 2", 3, "comma"},
		{"juxtaposed after parenthesis", "execute procedure P (1) 2", 3, "comma"},
		{"juxtaposed string and name", "select * from P('a' b)", 3, "comma"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := ParseEntryCall(tt.sql, tt.dialect)
			if err == nil {
				t.Fatalf("ParseEntryCall(%q) accepted", tt.sql)
			}
			if !strings.Contains(err.Error(), tt.reason) {
				t.Fatalf("ParseEntryCall(%q) error = %q, want it to mention %q", tt.sql, err, tt.reason)
			}
		})
	}
}

func TestNormalizeName(t *testing.T) {
	for in, want := range map[string]string{
		"lp_gen":    "LP_GEN",
		" Lp_Gen ":  "LP_GEN",
		`"Mixed"`:   "Mixed",
		`"a""b"`:    `a"b`,
		"LP_TWICE":  "LP_TWICE",
		`"lp_gen" `: "lp_gen",
	} {
		if got := NormalizeName(in); got != want {
			t.Errorf("NormalizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
