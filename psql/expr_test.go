package psql

import (
	"reflect"
	"testing"
)

func nameSet(names ...string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[name] = true
	}
	return set
}

func TestAnalyzeExpr(t *testing.T) {
	tests := []struct {
		name       string
		text       string
		names      map[string]bool
		dialect    int
		wantServer string
		wantParams []string
		wantLocal  LocalNode
	}{
		{
			name: "bare variables", text: "items + cnt", names: nameSet("ITEMS", "CNT"),
			wantServer: "? + ?", wantParams: []string{"ITEMS", "CNT"},
			wantLocal: LocalBinary{Op: "+", L: LocalVar{Name: "ITEMS"}, R: LocalVar{Name: "CNT"}},
		},
		{
			name: "colon variable", text: ":cnt <> 0", names: nameSet("CNT"),
			wantServer: "? <> 0", wantParams: []string{"CNT"},
			wantLocal: LocalBinary{Op: "<>", L: LocalVar{Name: "CNT"}, R: LocalInt{Value: 0}},
		},
		{
			name: "function call named like a variable", text: "F_LEFT(x, 1)", names: nameSet("F_LEFT", "X"),
			wantServer: "F_LEFT(?, 1)", wantParams: []string{"X"},
		},
		{
			name: "nested conditions", text: "(cnt = 0) or ((cnt2 = 0) and (A = 'N'))", names: nameSet("CNT", "CNT2", "A"),
			wantServer: "(? = 0) or ((? = 0) and (? = 'N'))", wantParams: []string{"CNT", "CNT2", "A"},
			wantLocal: LocalBinary{Op: "OR",
				L: LocalBinary{Op: "=", L: LocalVar{Name: "CNT"}, R: LocalInt{Value: 0}},
				R: LocalBinary{Op: "AND",
					L: LocalBinary{Op: "=", L: LocalVar{Name: "CNT2"}, R: LocalInt{Value: 0}},
					R: LocalBinary{Op: "=", L: LocalVar{Name: "A"}, R: LocalString{Value: []byte("N")}}}},
		},
		{
			name: "is not null", text: "x is not null", names: nameSet("X"),
			wantServer: "? is not null", wantParams: []string{"X"},
			wantLocal: LocalIsNull{X: LocalVar{Name: "X"}, Negated: true},
		},
		{
			name: "is null and not", text: "not (:x is null)", names: nameSet("X"),
			wantServer: "not (? is null)", wantParams: []string{"X"},
			wantLocal: LocalNot{X: LocalIsNull{X: LocalVar{Name: "X"}}},
		},
		{
			name: "not binds looser than comparison", text: "NOT x = 1", names: nameSet("X"),
			wantServer: "NOT ? = 1", wantParams: []string{"X"},
			wantLocal: LocalNot{X: LocalBinary{Op: "=", L: LocalVar{Name: "X"}, R: LocalInt{Value: 1}}},
		},
		{
			name: "dialect 1 double quotes are a string", text: `"NOW"`, dialect: 1,
			wantServer: `"NOW"`, wantLocal: LocalString{Value: []byte("NOW")},
		},
		{
			name: "dialect 3 double quotes are an identifier", text: `"NOW"`, dialect: 3,
			wantServer: `"NOW"`,
		},
		{
			name: "quote doubling", text: "'it''s'",
			wantServer: "'it''s'", wantLocal: LocalString{Value: []byte("it's")},
		},
		{
			name: "string bytes kept as is", text: "'caf\xe9'",
			wantServer: "'caf\xe9'", wantLocal: LocalString{Value: []byte("caf\xe9")},
		},
		{name: "integer outside INTEGER range", text: "3000000000", wantServer: "3000000000"},
		{name: "INTEGER max", text: "2147483647", wantServer: "2147483647", wantLocal: LocalInt{Value: 2147483647}},
		{name: "negative literal", text: "-1", wantServer: "-1", wantLocal: LocalInt{Value: -1}},
		{name: "negative literal at INTEGER min is not local", text: "-2147483648", wantServer: "-2147483648"},
		{
			name: "multiplication", text: "-1 * id", names: nameSet("ID"),
			wantServer: "-1 * ?", wantParams: []string{"ID"},
		},
		{
			name: "unary minus on variable", text: "-id", names: nameSet("ID"),
			wantServer: "-?", wantParams: []string{"ID"},
		},
		{name: "null", text: "NULL", wantServer: "NULL", wantLocal: LocalNull{}},
		{
			name: "not equal synonyms", text: "a != 1 and a <= 2 and a >= 0 and a < 3 and a > -1", names: nameSet("A"),
			wantServer: "? != 1 and ? <= 2 and ? >= 0 and ? < 3 and ? > -1", wantParams: []string{"A", "A", "A", "A", "A"},
			wantLocal: LocalBinary{Op: "AND",
				L: LocalBinary{Op: "AND",
					L: LocalBinary{Op: "AND",
						L: LocalBinary{Op: "AND",
							L: LocalBinary{Op: "<>", L: LocalVar{Name: "A"}, R: LocalInt{Value: 1}},
							R: LocalBinary{Op: "<=", L: LocalVar{Name: "A"}, R: LocalInt{Value: 2}}},
						R: LocalBinary{Op: ">=", L: LocalVar{Name: "A"}, R: LocalInt{Value: 0}}},
					R: LocalBinary{Op: "<", L: LocalVar{Name: "A"}, R: LocalInt{Value: 3}}},
				R: LocalBinary{Op: ">", L: LocalVar{Name: "A"}, R: LocalInt{Value: -1}}},
		},
		{
			name: "subtraction is left associative", text: "a - 1 - b", names: nameSet("A", "B"),
			wantServer: "? - 1 - ?", wantParams: []string{"A", "B"},
			wantLocal: LocalBinary{Op: "-", L: LocalBinary{Op: "-", L: LocalVar{Name: "A"}, R: LocalInt{Value: 1}}, R: LocalVar{Name: "B"}},
		},
		{
			name: "separated comparison tokens are not local", text: "a < > 1", names: nameSet("A"),
			wantServer: "? < > 1", wantParams: []string{"A"},
		},
		{
			name: "allow condition", text: "(:Allow IS NULL) OR (:Allow <> 'Y')", names: nameSet("ALLOW"),
			wantServer: "(? IS NULL) OR (? <> 'Y')", wantParams: []string{"ALLOW", "ALLOW"},
			wantLocal: LocalBinary{Op: "OR",
				L: LocalIsNull{X: LocalVar{Name: "ALLOW"}},
				R: LocalBinary{Op: "<>", L: LocalVar{Name: "ALLOW"}, R: LocalString{Value: []byte("Y")}}},
		},
		{
			name: "mixed bare and colon", text: "DOC = 'Y' and :cnt <> 0", names: nameSet("DOC", "CNT"),
			wantServer: "? = 'Y' and ? <> 0", wantParams: []string{"DOC", "CNT"},
			wantLocal: LocalBinary{Op: "AND",
				L: LocalBinary{Op: "=", L: LocalVar{Name: "DOC"}, R: LocalString{Value: []byte("Y")}},
				R: LocalBinary{Op: "<>", L: LocalVar{Name: "CNT"}, R: LocalInt{Value: 0}}},
		},
		{
			name: "unknown colon name is left unchanged", text: ":nope = 1", names: nameSet("X"),
			wantServer: ":nope = 1",
		},
		{
			name: "unknown bare name is left unchanged", text: "nope = 1", names: nameSet("X"),
			wantServer: "nope = 1",
		},
		{
			name: "case insensitive unless quoted", text: `Cnt + "cnt" + "Mixed"`, names: nameSet("CNT", "Mixed"),
			wantServer: `? + "cnt" + ?`, wantParams: []string{"CNT", "Mixed"},
		},
		{
			name: "quoted variable", text: `"Mixed" = 1`, names: nameSet("Mixed"),
			wantServer: "? = 1", wantParams: []string{"Mixed"},
			wantLocal: LocalBinary{Op: "=", L: LocalVar{Name: "Mixed"}, R: LocalInt{Value: 1}},
		},
		{
			name: "dotted names are not variables", text: "t.a = a", names: nameSet("T", "A"),
			wantServer: "t.a = ?", wantParams: []string{"A"},
		},
		{
			name: "concatenation and udf", text: "F_Left(Msg || 'Item '||:code||' missing.',255)",
			names:      nameSet("MSG", "CODE"),
			wantServer: "F_Left(? || 'Item '||?||' missing.',255)", wantParams: []string{"MSG", "CODE"},
		},
		{
			name: "subquery uses SQL variable rule", text: "(select max(a) from t where b = :x and c = x) + x", names: nameSet("A", "X"),
			wantServer: "(select max(a) from t where b = ? and c = x) + ?", wantParams: []string{"X", "X"},
		},
		{
			name: "cast type, extract part and generator are not variables",
			text: "cast(x as integer) + extract(year from d) + gen_id(g, 1)", names: nameSet("X", "INTEGER", "YEAR", "D", "G"),
			wantServer: "cast(? as integer) + extract(year from ?) + gen_id(g, 1)", wantParams: []string{"X", "D"},
		},
		{
			name: "decimal literal", text: "x = 1.5", names: nameSet("X"),
			wantServer: "? = 1.5", wantParams: []string{"X"},
		},
		{
			name: "date arithmetic", text: "('now' - ts_)*60*24", names: nameSet("TS_"),
			wantServer: "('now' - ?)*60*24", wantParams: []string{"TS_"},
		},
		{name: "in list", text: "x in (1, 2)", names: nameSet("X"), wantServer: "? in (1, 2)", wantParams: []string{"X"}},
		{name: "stray AS outside CAST keeps rewriting", text: "x as 1 + cnt", names: nameSet("X", "CNT"), wantServer: "? as 1 + ?", wantParams: []string{"X", "CNT"}},
		{name: "context variable", text: "current_date", wantServer: "current_date"},
		{name: "empty", text: "", wantServer: ""},
		{name: "unbalanced", text: "(x = 1", names: nameSet("X"), wantServer: "(? = 1", wantParams: []string{"X"}},
		{name: "trailing operator", text: "x +", names: nameSet("X"), wantServer: "? +", wantParams: []string{"X"}},
		{name: "logic on values", text: "x and 1", names: nameSet("X"), wantServer: "? and 1", wantParams: []string{"X"}},
		{name: "arithmetic on conditions", text: "(x = 1) + 1", names: nameSet("X"), wantServer: "(? = 1) + 1", wantParams: []string{"X"}},
		{name: "chained comparison", text: "x = 1 = 1", names: nameSet("X"), wantServer: "? = 1 = 1", wantParams: []string{"X"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dialect := tt.dialect
			if dialect == 0 {
				dialect = 3
			}
			got := AnalyzeExpr(tt.text, 4, 7, tt.names, dialect)
			if got == nil {
				t.Fatal("AnalyzeExpr() = nil")
			}
			if got.Text != tt.text || got.Line != 4 || got.Column != 7 {
				t.Fatalf("Text/Line/Column = %q %d:%d", got.Text, got.Line, got.Column)
			}
			if got.ServerText != tt.wantServer {
				t.Fatalf("ServerText = %q, want %q", got.ServerText, tt.wantServer)
			}
			if !reflect.DeepEqual(got.Params, tt.wantParams) {
				t.Fatalf("Params = %#v, want %#v", got.Params, tt.wantParams)
			}
			if !reflect.DeepEqual(got.Local, tt.wantLocal) {
				t.Fatalf("Local = %#v, want %#v", got.Local, tt.wantLocal)
			}
		})
	}
}

func FuzzAnalyzeExpr(f *testing.F) {
	seeds := []string{
		"items + cnt", ":cnt <> 0", "F_LEFT(x, 1)", "(cnt = 0) or ((cnt2 = 0) and (A = 'N'))",
		"x is not null", `"NOW"`, "'it''s'", "3000000000", "-1 * id", "(select a from t where b = :x)",
		"cast(x as varchar(10) character set none)", "((((", "))))", ": x", "not not not x",
	}
	for _, seed := range seeds {
		f.Add(seed, 3)
		f.Add(seed, 1)
	}
	names := nameSet("X", "CNT", "CNT2", "A", "ITEMS", "ID")
	f.Fuzz(func(t *testing.T, text string, dialect int) {
		_, _ = ParseEntryCall(text, dialect)
		_ = ParseTypeText(text)
		e := AnalyzeExpr(text, 1, 1, names, dialect)
		if e == nil {
			t.Fatal("nil Expr")
		}
		questions := 0
		for _, tok := range lexDialect(e.ServerText, dialect) {
			if tok.kind == tokenPunctuation && tok.text == "?" {
				questions++
			}
		}
		original := 0
		for _, tok := range lexDialect(text, dialect) {
			if tok.kind == tokenPunctuation && tok.text == "?" {
				original++
			}
		}
		if questions-original != len(e.Params) {
			t.Fatalf("ServerText %q has %d new parameters, Params %v", e.ServerText, questions-original, e.Params)
		}
	})
}
