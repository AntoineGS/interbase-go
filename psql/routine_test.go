package psql

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func renderRoutine(r *Routine) string {
	var out strings.Builder
	if r == nil || r.Body == nil {
		return ""
	}
	renderNode(&out, r.Body, "")
	return out.String()
}

func renderNode(out *strings.Builder, n *Node, indent string) {
	fmt.Fprintf(out, "%s%s %d:%d-%d:%d", indent, n.Kind, n.Line, n.Column, n.EndLine, n.EndColumn)
	if n.Target != "" {
		fmt.Fprintf(out, " target=%s", n.Target)
	}
	if n.Exception != "" {
		fmt.Fprintf(out, " exception=%s", n.Exception)
	}
	if n.Cond != nil {
		fmt.Fprintf(out, " cond=%q", n.Cond.Text)
	}
	if n.Value != nil {
		fmt.Fprintf(out, " value=%q", n.Value.Text)
	}
	if n.SQL != nil {
		fmt.Fprintf(out, " sql=%q", n.SQL.Text)
	}
	if n.Into != nil {
		fmt.Fprintf(out, " into=%v", n.Into)
	}
	if n.Proc != nil {
		args := make([]string, 0, len(n.Proc.Args))
		for _, a := range n.Proc.Args {
			args = append(args, a.Text)
		}
		fmt.Fprintf(out, " proc=%s args=%q", n.Proc.Name, args)
		if n.Proc.Columns != nil {
			fmt.Fprintf(out, " columns=%v", n.Proc.Columns)
		}
	}
	if n.Returning != nil {
		fmt.Fprintf(out, " returning=%v", n.Returning)
	}
	out.WriteString("\n")
	for _, child := range n.Children {
		renderNode(out, child, indent+"  ")
	}
	for _, h := range n.Handlers {
		conditions := make([]string, 0, len(h.Conditions))
		for _, c := range h.Conditions {
			switch c.Kind {
			case "sqlcode":
				conditions = append(conditions, fmt.Sprintf("sqlcode:%d", c.Code))
			case "any":
				conditions = append(conditions, "any")
			default:
				conditions = append(conditions, c.Kind+":"+c.Name)
			}
		}
		fmt.Fprintf(out, "%s  when %d %v\n", indent, h.Line, conditions)
		renderNode(out, h.Body, indent+"    ")
	}
}

func parseClean(t *testing.T, body string, opts ParseOptions) *Routine {
	t.Helper()
	r, problems := ParseRoutine(body, opts)
	if len(problems) != 0 {
		t.Fatalf("ParseRoutine() problems = %+v", problems)
	}
	if r == nil || r.Body == nil {
		t.Fatalf("ParseRoutine() returned no body")
	}
	checkNodeIDs(t, r)
	return r
}

func checkNodeIDs(t *testing.T, r *Routine) {
	t.Helper()
	for i, n := range r.Nodes {
		if n.ID != i {
			t.Fatalf("Nodes[%d].ID = %d", i, n.ID)
		}
	}
}

func TestLexDialectColumnsAndStrings(t *testing.T) {
	tokens := lexDialect("a\r\n  b\rc \"NOW\"", 1)
	type pos struct {
		kind         tokenKind
		text         string
		line, column int
	}
	var got []pos
	for _, tok := range tokens {
		got = append(got, pos{tok.kind, tok.text, tok.line, tok.column})
	}
	want := []pos{
		{tokenIdentifier, "a", 1, 1},
		{tokenIdentifier, "b", 2, 3},
		{tokenIdentifier, "c", 3, 1},
		{tokenString, "NOW", 3, 3},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("lexDialect() = %+v, want %+v", got, want)
	}
	if kind := lexDialect(`"NOW"`, 3)[0].kind; kind != tokenQuotedIdentifier {
		t.Fatalf("dialect 3 kind = %v, want quoted identifier", kind)
	}
	if kind := lex(`"NOW"`)[0].kind; kind != tokenQuotedIdentifier {
		t.Fatalf("lex kind = %v, want quoted identifier", kind)
	}
}

func TestParseRoutineDeclarations(t *testing.T) {
	body := "\n" +
		"DECLARE VARIABLE Cnt INTEGER;\n" +
		"declare variable name_x VARCHAR(20);\n" +
		"Declare Variable amt NUMERIC(15,2);\n" +
		"DECLARE VARIABLE d DATE;\n" +
		"DECLARE VARIABLE flag CHAR(1);\n" +
		"DECLARE VARIABLE \"Mixed\" VARCHAR(10) CHARACTER SET NONE;\n" +
		"DECLARE VARIABLE ratio DOUBLE PRECISION;\n" +
		"DECLARE VARIABLE small SMALLINT;\n" +
		"DECLARE VARIABLE ts TIMESTAMP;\n" +
		"DECLARE VARIABLE big DECIMAL(18, 4);\n" +
		"begin\nend"
	r := parseClean(t, body, ParseOptions{})
	want := []Declaration{
		{Name: "CNT", TypeText: "INTEGER", Line: 2},
		{Name: "NAME_X", TypeText: "VARCHAR(20)", Line: 3},
		{Name: "AMT", TypeText: "NUMERIC(15,2)", Line: 4},
		{Name: "D", TypeText: "DATE", Line: 5},
		{Name: "FLAG", TypeText: "CHAR(1)", Line: 6},
		{Name: "Mixed", TypeText: "VARCHAR(10) CHARACTER SET NONE", Line: 7},
		{Name: "RATIO", TypeText: "DOUBLE PRECISION", Line: 8},
		{Name: "SMALL", TypeText: "SMALLINT", Line: 9},
		{Name: "TS", TypeText: "TIMESTAMP", Line: 10},
		{Name: "BIG", TypeText: "DECIMAL(18, 4)", Line: 11},
	}
	if !reflect.DeepEqual(r.Declarations, want) {
		t.Fatalf("Declarations = %+v, want %+v", r.Declarations, want)
	}
	if got, want := renderRoutine(r), "block 12:1-13:3\n"; got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}
}

func TestParseRoutineStatementTree(t *testing.T) {
	body := strings.Join([]string{
		"begin",
		"x = 1;",
		"if (x = 1) then",
		"if (y = 2) then x = 2;",
		"else x = 3;",
		"while (x > 0) do",
		"begin",
		"x = x - 1;",
		"break;",
		"end",
		"select a from t where b = :x into :x, y;",
		"for select a, b from t into :x, :y do",
		"begin",
		"suspend;",
		"end",
		"insert into t (a) values (:x);",
		"update t set a = 1 where b = :y;",
		"delete from t where a = 2;",
		"execute procedure p_one;",
		"execute procedure p_two :x, 2;",
		"execute procedure p_three(:x, 'a;b') returning_values :x, :y;",
		"exception e_one;",
		"exception e_two 'boom ' || x;",
		"post_event 'evt';",
		";;",
		"leave;",
		"exit;",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{})
	want := strings.Join([]string{
		"block 1:1-28:3",
		"  assign 2:1-2:6 target=X value=\"1\"",
		"  if 3:1-5:11 cond=\"x = 1\"",
		"    if 4:1-5:11 cond=\"y = 2\"",
		"      assign 4:17-4:22 target=X value=\"2\"",
		"      assign 5:6-5:11 target=X value=\"3\"",
		"  while 6:1-10:3 cond=\"x > 0\"",
		"    block 7:1-10:3",
		"      assign 8:1-8:10 target=X value=\"x - 1\"",
		"      break 9:1-9:6",
		"  selectInto 11:1-11:40 sql=\"select a from t where b = ?\" into=[X Y]",
		"  forSelect 12:1-15:3 sql=\"select a, b from t\" into=[X Y]",
		"    block 13:1-15:3",
		"      suspend 14:1-14:8",
		"  insert 16:1-16:30 sql=\"insert into t (a) values (?)\"",
		"  update 17:1-17:32 sql=\"update t set a = 1 where b = ?\"",
		"  delete 18:1-18:26 sql=\"delete from t where a = 2\"",
		"  executeProcedure 19:1-19:24 proc=P_ONE args=[]",
		"  executeProcedure 20:1-20:30 proc=P_TWO args=[\":x\" \"2\"]",
		"  executeProcedure 21:1-21:61 proc=P_THREE args=[\":x\" \"'a;b'\"] returning=[X Y]",
		"  exception 22:1-22:16 exception=E_ONE",
		"  exception 23:1-23:29 exception=E_TWO value=\"'boom ' || x\"",
		"  postEvent 24:1-24:17 value=\"'evt'\"",
		"  empty 25:1-25:1",
		"  empty 25:2-25:2",
		"  break 26:1-26:6",
		"  exit 27:1-27:5",
		"",
	}, "\n")
	if got := renderRoutine(r); got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}
	if len(r.Nodes) != 27 || r.Nodes[0] != r.Body {
		t.Fatalf("Nodes = %d, first is body %v", len(r.Nodes), r.Nodes[0] == r.Body)
	}
	if r.Nodes[1].Kind != NodeAssign || r.Nodes[2].Kind != NodeIf || r.Nodes[3].Kind != NodeIf {
		t.Fatalf("Nodes are not in pre-order: %s %s %s", r.Nodes[1].Kind, r.Nodes[2].Kind, r.Nodes[3].Kind)
	}
}

func TestParseRoutineWhenHandlers(t *testing.T) {
	body := strings.Join([]string{
		"begin",
		"begin",
		"insert into t (a) values (1);",
		"when exception e_dup do x = 1;",
		"when sqlcode -803, gdscode lock_conflict do",
		"begin",
		"x = 2;",
		"end",
		"when any do x = 3;",
		"end",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{})
	want := strings.Join([]string{
		"block 1:1-11:3",
		"  block 2:1-10:3",
		"    insert 3:1-3:29 sql=\"insert into t (a) values (1)\"",
		"    when 4 [exception:E_DUP]",
		"      assign 4:25-4:30 target=X value=\"1\"",
		"    when 5 [sqlcode:-803 gdscode:LOCK_CONFLICT]",
		"      block 6:1-8:3",
		"        assign 7:1-7:6 target=X value=\"2\"",
		"    when 9 [any]",
		"      assign 9:13-9:18 target=X value=\"3\"",
		"",
	}, "\n")
	if got := renderRoutine(r); got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}
	if len(r.Nodes) != 7 {
		t.Fatalf("Nodes = %d, want 7 (handler bodies included)", len(r.Nodes))
	}
}

func TestParseRoutineMixedCaseCRLF(t *testing.T) {
	body := "\r\nDECLARE VARIABLE FlagValue CHAR(1);\r\nBEGIN\r\n" +
		"  IF ((:X IS NULL) OR (:X <> 'Y')) THEN X = 'N';\r\n" +
		"  if(FlagValue = 'Y' and :cnt <> 0) then\r\n" +
		"  begin\r\n" +
		"    suspend;\r\n" +
		"  end else begin\r\n" +
		"    exit;\r\n" +
		"  end\r\n" +
		"End"
	r := parseClean(t, body, ParseOptions{})
	want := strings.Join([]string{
		"block 3:1-11:3",
		"  if 4:3-4:48 cond=\"(:X IS NULL) OR (:X <> 'Y')\"",
		"    assign 4:41-4:48 target=X value=\"'N'\"",
		"  if 5:3-10:5 cond=\"FlagValue = 'Y' and :cnt <> 0\"",
		"    block 6:3-8:5",
		"      suspend 7:5-7:12",
		"    block 8:12-10:5",
		"      exit 9:5-9:9",
		"",
	}, "\n")
	if got := renderRoutine(r); got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}
}

func TestParseRoutineComments(t *testing.T) {
	body := strings.Join([]string{
		"begin",
		"/* begin; end; suspend; if (x) then */",
		"/* one /* two; end */ x = 1;",
		"-- exit; end",
		"y = 2; /* trailing ; */",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{})
	want := strings.Join([]string{
		"block 1:1-6:3",
		"  assign 3:23-3:28 target=X value=\"1\"",
		"  assign 5:1-5:6 target=Y value=\"2\"",
		"",
	}, "\n")
	if got := renderRoutine(r); got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}
}

func TestParseRoutineDialect(t *testing.T) {
	body := "begin\nd = \"NOW\";\nselect a from t where m = \"TAG_X\" into :x;\nend"
	r := parseClean(t, body, ParseOptions{Dialect: 1})
	want := strings.Join([]string{
		"block 1:1-4:3",
		"  assign 2:1-2:10 target=D value=\"\\\"NOW\\\"\"",
		"  selectInto 3:1-3:42 sql=\"select a from t where m = \\\"TAG_X\\\"\" into=[X]",
		"",
	}, "\n")
	if got := renderRoutine(r); got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}

	quoted := "begin\n\"Mixed\" = 1;\nend"
	r = parseClean(t, quoted, ParseOptions{})
	if got := r.Body.Children[0].Target; got != "Mixed" {
		t.Fatalf("dialect 3 target = %q, want Mixed", got)
	}
	if _, problems := ParseRoutine(quoted, ParseOptions{Dialect: 1}); len(problems) != 1 || problems[0].Line != 2 {
		t.Fatalf("dialect 1 quoted target problems = %+v, want one at line 2", problems)
	}
}

func TestParseRoutineExecuteStatement(t *testing.T) {
	body := strings.Join([]string{
		"declare variable s varchar(100);",
		"begin",
		"  execute statement 'update t set a = 1';",
		"  execute statement ('select a, b from t') into :a, b;",
		"  execute statement s || ' where id = ' || :a into a;",
		"  for execute statement 'select a from t ' || s into :a do",
		"  begin",
		"    suspend;",
		"  end",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{Params: []string{"A", "B"}})
	want := strings.Join([]string{
		"block 2:1-10:3",
		"  executeStatement 3:3-3:41 value=\"'update t set a = 1'\"",
		"  executeStatement 4:3-4:54 value=\"('select a, b from t')\" into=[A B]",
		"  executeStatement 5:3-5:53 value=\"s || ' where id = ' || :a\" into=[A]",
		"  forExecuteStatement 6:3-9:5 value=\"'select a from t ' || s\" into=[A]",
		"    block 7:3-9:5",
		"      suspend 8:5-8:12",
		"",
	}, "\n")
	if got := renderRoutine(r); got != want {
		t.Fatalf("tree =\n%s\nwant\n%s", got, want)
	}
	if es := r.Body.Children[2]; es.Value.Local != nil || len(es.Value.Params) != 2 {
		t.Fatalf("concatenation expression %+v, want a server expression of S and A", es.Value)
	}
	if es := r.Body.Children[0]; es.Into != nil {
		t.Fatalf("EXECUTE STATEMENT without INTO has targets %v", es.Into)
	}
}

func TestParseRoutineRefusals(t *testing.T) {
	tests := []struct {
		name      string
		body      string
		wantLines []int
		wantText  []string
	}{
		{
			name:      "for execute statement without into",
			body:      "begin\n  for execute statement 'select a from t' do\n    suspend;\n  x = 1;\nend",
			wantLines: []int{2},
			wantText:  []string{"FOR EXECUTE STATEMENT without"},
		},
		{
			name:      "execute statement without a text",
			body:      "begin\n  execute statement;\n  x = 1;\nend",
			wantLines: []int{2},
			wantText:  []string{"EXECUTE STATEMENT without"},
		},
		{
			name:      "for select as cursor and where current of",
			body:      "begin\nfor select a from t into :x\nas cursor c do\nbegin\nupdate t set a = 1 where current of c;\nend\nend",
			wantLines: []int{3, 5},
			wantText:  []string{"CURSOR", "CURRENT OF"},
		},
		{
			name:      "declare with initial value",
			body:      "DECLARE VARIABLE X INTEGER = 1;\nDECLARE VARIABLE Y INTEGER DEFAULT 0;\nbegin\nend",
			wantLines: []int{1, 2},
			wantText:  []string{"DECLARE VARIABLE", "DECLARE VARIABLE"},
		},
		{
			name:      "unknown statement start",
			body:      "begin\n  frobnicate x;\n  y = 1;\nend",
			wantLines: []int{2},
			wantText:  []string{"FROBNICATE"},
		},
		{
			name: "multiple problems reported together",
			body: "begin\nupdate t set a = 1 where current of c;\nfrobnicate;\nif (x = 1) then\n  frobnicate;\n" +
				"for execute statement 'c' do\nbegin\n  suspend;\nend\nend",
			wantLines: []int{2, 3, 5, 6},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, problems := ParseRoutine(tt.body, ParseOptions{})
			var lines []int
			for _, p := range problems {
				lines = append(lines, p.Line)
				if p.Column < 1 || p.Message == "" {
					t.Fatalf("problem without column or message: %+v", p)
				}
			}
			if !reflect.DeepEqual(lines, tt.wantLines) {
				t.Fatalf("problem lines = %v, want %v (%+v)", lines, tt.wantLines, problems)
			}
			for i, text := range tt.wantText {
				if !strings.Contains(problems[i].Message, text) {
					t.Fatalf("problem %d = %q, want it to mention %q", i, problems[i].Message, text)
				}
			}
		})
	}
}

func TestParseRoutineRefusalKeepsParsing(t *testing.T) {
	r, problems := ParseRoutine("begin\n  frobnicate 'delete from t';\n  x = 1;\nend", ParseOptions{})
	if len(problems) != 1 || problems[0].Column != 3 {
		t.Fatalf("problems = %+v, want one at column 3", problems)
	}
	if r == nil || r.Body == nil || len(r.Body.Children) != 1 || r.Body.Children[0].Kind != NodeAssign {
		t.Fatalf("parsing did not continue after refusal: %s", renderRoutine(r))
	}
}

func TestParseRoutineMalformed(t *testing.T) {
	inputs := []string{
		"", "begin", "end", "begin if", "begin if (", "begin x = ", "declare", "declare variable",
		"declare variable x", "begin when", "begin execute", "begin execute procedure",
		"begin execute procedure p returning_values", "begin end end", "(((", ")))", "begin for",
		"begin for select", "begin for execute statement", "begin for execute statement x into", "begin execute statement", "begin execute statement x into", "begin when sqlcode - do x = 1; end", "begin else end", "begin if (x) then else end",
		"begin begin", "begin x = 1; when any do", "begin when any do x = 1; y = 2; end",
		"begin select a from t; end", "begin exception; end", "begin while (x) suspend; end",
		"begin when sqlcode 99999999999 do exit; end", "begin suspend end",
	}
	for _, input := range inputs {
		r, problems := ParseRoutine(input, ParseOptions{})
		if len(problems) == 0 {
			t.Errorf("ParseRoutine(%q) reported no problem", input)
		}
		for _, p := range problems {
			if p.Line < 1 || p.Column < 1 {
				t.Errorf("ParseRoutine(%q) problem position %+v", input, p)
			}
		}
		if r == nil {
			t.Errorf("ParseRoutine(%q) returned nil routine", input)
			continue
		}
		checkNodeIDs(t, r)
		_ = r.HasSuspend()
		_ = r.MayWrite()
		_ = r.Callees()
	}
}

func TestRoutineHasSuspend(t *testing.T) {
	if r := parseClean(t, "begin\nif (x = 1) then suspend;\nend", ParseOptions{}); !r.HasSuspend() {
		t.Fatal("HasSuspend() = false, want true")
	}
	if r := parseClean(t, "begin\nx = 1;\nend", ParseOptions{}); r.HasSuspend() {
		t.Fatal("HasSuspend() = true, want false")
	}
}

func TestRoutineMayWrite(t *testing.T) {
	tests := []struct {
		name string
		body string
		want bool
	}{
		{"read only", "begin\nx = 1;\nselect a from t into :a;\nfor select a from t into :a do\nif (a = 1) then suspend;\nend", false},
		{"insert", "begin\ninsert into t (a) values (1);\nend", true},
		{"update", "begin\nupdate t set a = 1;\nend", true},
		{"delete", "begin\ndelete from t;\nend", true},
		{"execute procedure", "begin\nexecute procedure p;\nend", true},
		{"post event", "begin\npost_event 'e';\nend", false},
		{"gen_id in assignment", "begin\nx = gen_id(g, 1);\nend", true},
		{"gen_id in select", "begin\nselect gen_id(g, 1) from rdb$database into :x;\nend", true},
		{"procedure in from", "begin\nselect a from p_src(:x) into :a;\nend", true},
		{"procedure in for select join", "begin\nfor select a from t join p_src(1) s on s.id = t.id into :a do suspend;\nend", true},
		{"subquery on table in condition", "begin\nif (exists(select 1 from t where a = :x)) then exit;\nend", false},
		{"procedure in if condition", "begin\nif (exists(select 1 from p_src(1))) then exit;\nend", true},
		{"procedure in while condition", "begin\nwhile (:a in (select a from p_src(1))) do a = a - 1;\nend", true},
		{"procedure in assignment", "begin\nx = (select max(a) from p_src(2));\nend", true},
		{"procedure in exception message", "begin\nexception e_one (select first 1 m from p_src(3));\nend", true},
		{"procedure in post_event", "begin\npost_event (select first 1 n from p_src(4));\nend", true},
		{"execute statement", "begin\nexecute statement 'select a from t' into :a;\nend", true},
		{"for execute statement", "begin\nfor execute statement 'select a from t' into :a do suspend;\nend", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := parseClean(t, tt.body, ParseOptions{})
			if got := r.MayWrite(); got != tt.want {
				t.Fatalf("MayWrite() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRoutineCallees(t *testing.T) {
	body := strings.Join([]string{
		"begin",
		"execute procedure p_exec 1;",
		"select a, b from p_sel(:x) into :a, :b;",
		"for select * from p_sel(1) into :a do suspend;",
		"select max(a) from p_agg(1) into :a;",
		"select a from t join p_join(1) j on j.id = t.id into :a;",
		"execute procedure \"Mixed\" 1;",
		"execute procedure p_exec 2;",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{Params: []string{"X"}})
	want := []Callee{
		{Name: "P_EXEC", Lines: []int{2, 8}, StepInShape: true},
		{Name: "P_SEL", Lines: []int{3, 4}, StepInShape: true},
		{Name: "P_AGG", Lines: []int{5}},
		{Name: "P_JOIN", Lines: []int{6}},
		{Name: "Mixed", Quoted: true, Lines: []int{7}, StepInShape: true},
	}
	if got := r.Callees(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Callees() = %+v, want %+v", got, want)
	}
	wantSites := []CallSite{
		{Name: "P_EXEC", Line: 2, StepInShape: true}, {Name: "P_SEL", Line: 3, StepInShape: true},
		{Name: "P_SEL", Line: 4, StepInShape: true}, {Name: "P_AGG", Line: 5}, {Name: "P_JOIN", Line: 6},
		{Name: "Mixed", Quoted: true, Line: 7, StepInShape: true}, {Name: "P_EXEC", Line: 8, StepInShape: true},
	}
	if got := r.CallSites(); !reflect.DeepEqual(got, wantSites) {
		t.Fatalf("CallSites() = %+v, want %+v", got, wantSites)
	}
	children := r.Body.Children
	if p := children[1].Proc; p == nil || p.Name != "P_SEL" || p.Quoted || len(p.Args) != 1 || p.Args[0].Text != ":x" ||
		p.Args[0].ServerText != "?" || !reflect.DeepEqual(p.Args[0].Params, []string{"X"}) || !reflect.DeepEqual(p.Columns, []string{"A", "B"}) {
		t.Fatalf("select step-in Proc = %+v", p)
	}
	if p := children[2].Proc; p == nil || p.Name != "P_SEL" || p.Columns != nil || len(p.Args) != 1 || p.Args[0].Text != "1" {
		t.Fatalf("for select step-in Proc = %+v", p)
	}
	if children[3].Proc != nil || children[4].Proc != nil {
		t.Fatalf("non step-in shapes have Proc: %+v %+v", children[3].Proc, children[4].Proc)
	}
	if p := children[5].Proc; p == nil || p.Name != "Mixed" || !p.Quoted {
		t.Fatalf("quoted execute Proc = %+v", p)
	}
}

func TestRoutineSourceNames(t *testing.T) {
	body := strings.Join([]string{
		"begin",
		"select a from t_one into :a;",
		"select a from p_bare into :a;",
		"select a from t_two x, t_three join \"Mixed\" m on m.id = x.id into :a;",
		"delete from t_del where a in (select a from t_sub);",
		"for select a from p_sel(1) into :a do suspend;",
		"a = (select max(a) from t_one);",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{Params: []string{"A"}})
	want := []Callee{
		{Name: "T_ONE", Lines: []int{2, 7}},
		{Name: "P_BARE", Lines: []int{3}},
		{Name: "T_TWO", Lines: []int{4}},
		{Name: "T_THREE", Lines: []int{4}},
		{Name: "Mixed", Quoted: true, Lines: []int{4}},
		{Name: "T_SUB", Lines: []int{5}},
	}
	if got := r.SourceNames(); !reflect.DeepEqual(got, want) {
		t.Fatalf("SourceNames() = %+v, want %+v", got, want)
	}
}

func TestRoutineCalleesInExpressions(t *testing.T) {
	body := strings.Join([]string{
		"begin",
		"if (exists(select 1 from p_cond(1))) then exit;",
		"while (:a in (select a from p_loop(1))) do",
		"a = a - 1;",
		"x = (select max(a) from p_value(2));",
		"exception e_one (select first 1 m from p_msg(3));",
		"post_event (select first 1 n from p_event(4));",
		"x = (select max(a) from p_cond(5));",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{})
	want := []Callee{
		{Name: "P_COND", Lines: []int{2, 8}},
		{Name: "P_LOOP", Lines: []int{3}},
		{Name: "P_VALUE", Lines: []int{5}},
		{Name: "P_MSG", Lines: []int{6}},
		{Name: "P_EVENT", Lines: []int{7}},
	}
	if got := r.Callees(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Callees() = %+v, want %+v", got, want)
	}
	for _, n := range r.Nodes {
		if n.Proc != nil {
			t.Fatalf("expression node %s at line %d has Proc %+v", n.Kind, n.Line, n.Proc)
		}
	}
}

func TestParseRoutineSQLText(t *testing.T) {
	body := strings.Join([]string{
		"DECLARE VARIABLE id INTEGER;",
		"begin",
		"select a from t where id = :id into :x;",
		"select a from t where id = id into :x;",
		"insert into t(a,b) values (:Br || :SUFFIX, :x);",
		"select a",
		"  from t",
		"  where b = :b",
		"  into :x;",
		"select ('now' - ts_)*60*24 from t where id = :id into :mins;",
		"select count(*) from t where a = :a or note_1 = :b into :cnt;",
		"for select a, b from t where c = : c into :x, :y do suspend;",
		"delete from t where a = :\"Mixed\";",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{})
	type sqlWant struct {
		text   string
		params []string
		into   []string
	}
	want := []sqlWant{
		{"select a from t where id = ?", []string{"ID"}, []string{"X"}},
		{"select a from t where id = id", nil, []string{"X"}},
		{"insert into t(a,b) values (? || ?, ?)", []string{"BR", "SUFFIX", "X"}, nil},
		{"select a\n  from t\n  where b = ?", []string{"B"}, []string{"X"}},
		{"select ('now' - ts_)*60*24 from t where id = ?", []string{"ID"}, []string{"MINS"}},
		{"select count(*) from t where a = ? or note_1 = ?", []string{"A", "B"}, []string{"CNT"}},
		{"select a, b from t where c = ?", []string{"C"}, []string{"X", "Y"}},
		{"delete from t where a = ?", []string{"Mixed"}, nil},
	}
	children := r.Body.Children
	if len(children) != len(want) {
		t.Fatalf("children = %d\n%s", len(children), renderRoutine(r))
	}
	for i, w := range want {
		n := children[i]
		if n.SQL == nil || n.SQL.Text != w.text || !reflect.DeepEqual(n.SQL.Params, w.params) || !reflect.DeepEqual(n.Into, w.into) {
			t.Errorf("statement %d: SQL = %+v Into = %v, want %+v", i, n.SQL, n.Into, w)
		}
	}

	r = parseClean(t, "begin\nUPDATE T SET READ_AT = \"NOW\" WHERE ID = :ID;\nend", ParseOptions{Dialect: 1})
	if sql := r.Body.Children[0].SQL; sql.Text != `UPDATE T SET READ_AT = "NOW" WHERE ID = ?` || !reflect.DeepEqual(sql.Params, []string{"ID"}) {
		t.Fatalf("dialect 1 update SQL = %+v", sql)
	}
}

func TestParseRoutineExpressions(t *testing.T) {
	body := strings.Join([]string{
		"DECLARE VARIABLE items INTEGER;",
		"DECLARE VARIABLE deltaValue INTEGER;",
		"begin",
		"items = items + cnt;",
		"IF ((:Allow IS NULL) OR (:Allow <> 'Y')) THEN Allow = 'N';",
		"if(DOC = 'Y' and :cnt <> 0) then exit;",
		"deltaValue = -1 * id;",
		"while (items > 0) do items = items - 1;",
		"exception e_one 'bad ' || items;",
		"post_event 'evt';",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{Params: []string{"CNT", "ALLOW", "DOC", "ID "}})
	children := r.Body.Children
	check := func(name string, e *Expr, server string, params []string, local bool, line, column int) {
		t.Helper()
		if e == nil {
			t.Fatalf("%s: nil Expr", name)
		}
		if e.ServerText != server || !reflect.DeepEqual(e.Params, params) || (e.Local != nil) != local || e.Line != line || e.Column != column {
			t.Errorf("%s: Expr = %+v, want server %q params %v local %v at %d:%d", name, e, server, params, local, line, column)
		}
	}
	check("assign", children[0].Value, "? + ?", []string{"ITEMS", "CNT"}, true, 4, 9)
	check("allow cond", children[1].Cond, "(? IS NULL) OR (? <> 'Y')", []string{"ALLOW", "ALLOW"}, true, 5, 5)
	check("allow value", children[1].Children[0].Value, "'N'", nil, true, 5, 55)
	check("doc cond", children[2].Cond, "? = 'Y' and ? <> 0", []string{"DOC", "CNT"}, true, 6, 4)
	check("discount", children[3].Value, "-1 * ?", []string{"ID"}, false, 7, 14)
	check("while", children[4].Cond, "? > 0", []string{"ITEMS"}, true, 8, 8)
	check("exception", children[5].Value, "'bad ' || ?", []string{"ITEMS"}, false, 9, 17)
	check("post_event", children[6].Value, "'evt'", nil, true, 10, 12)

	r = parseClean(t, "begin\nitems = items + cnt;\nend", ParseOptions{})
	check("undeclared", r.Body.Children[0].Value, "items + cnt", nil, false, 2, 9)
}

func TestParseRoutineProcCalls(t *testing.T) {
	body := strings.Join([]string{
		"DECLARE VARIABLE a INTEGER;",
		"begin",
		"EXECUTE PROCEDURE P_FIN(:A,:B,:C);",
		"execute procedure p_two a, 2 + a returning_values :a;",
		"execute procedure p_none;",
		"select x from P(:id) into :a;",
		"select min(x) from P(:id) into :a;",
		"SELECT ID FROM GET_ID(:DB, 'NAME') into :id;",
		"for select * from p_rows(a) into :a do suspend;",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{Params: []string{"B", "C", "ID", "DB"}})
	children := r.Body.Children
	type argWant struct{ text, server string }
	check := func(i int, name string, args []argWant, columns []string) {
		t.Helper()
		p := children[i].Proc
		if p == nil || p.Name != name {
			t.Fatalf("statement %d: Proc = %+v, want %s", i, p, name)
		}
		if len(p.Args) != len(args) {
			t.Fatalf("statement %d: %d args, want %d", i, len(p.Args), len(args))
		}
		for j, w := range args {
			if p.Args[j].Text != w.text || p.Args[j].ServerText != w.server {
				t.Errorf("statement %d arg %d = %+v, want %+v", i, j, p.Args[j], w)
			}
		}
		if !reflect.DeepEqual(p.Columns, columns) {
			t.Errorf("statement %d: Columns = %#v, want %#v", i, p.Columns, columns)
		}
	}
	check(0, "P_FIN", []argWant{{":A", "?"}, {":B", "?"}, {":C", "?"}}, nil)
	check(1, "P_TWO", []argWant{{"a", "?"}, {"2 + a", "2 + ?"}}, nil)
	if !reflect.DeepEqual(children[1].Returning, []string{"A"}) {
		t.Errorf("Returning = %v", children[1].Returning)
	}
	if args := children[1].Proc.Args; args[0].Column != 25 || args[1].Column != 28 || args[1].Line != 4 {
		t.Errorf("arg positions = %d:%d %d:%d", args[0].Line, args[0].Column, args[1].Line, args[1].Column)
	}
	check(2, "P_NONE", nil, nil)
	check(3, "P", []argWant{{":id", "?"}}, []string{"X"})
	if children[4].Proc != nil {
		t.Errorf("aggregate select has Proc %+v", children[4].Proc)
	}
	check(5, "GET_ID", []argWant{{":DB", "?"}, {"'NAME'", "'NAME'"}}, []string{"ID"})
	// Inside SQL only :name is a variable.
	check(6, "P_ROWS", []argWant{{"a", "a"}}, nil)

	if _, problems := ParseRoutine("begin\nexecute procedure p(1,,2);\nend", ParseOptions{}); len(problems) != 1 || problems[0].Line != 2 {
		t.Fatalf("empty argument problems = %+v", problems)
	}
}

func TestRoutineFunctionCallsAreNotProcedures(t *testing.T) {
	body := strings.Join([]string{
		"DECLARE VARIABLE Msg VARCHAR(255);",
		"DECLARE VARIABLE Tmp_List VARCHAR(255);",
		"DECLARE VARIABLE Cnt INTEGER;",
		"begin",
		"Msg = F_Left(Msg || 'Item '||:code||' missing.',255);",
		"Tmp_List = f_right(:Tmp_List,f_stringlength(:Tmp_List)-:Cnt);",
		"Cnt = f_substr(';',:Tmp_List);",
		"if (F_STRINGLENGTH(SUFFIX_ID) <> 2) then exit;",
		"Cnt = extract(year from f_date(1));",
		"select f_a(1, f_b(2)) from t where a = f_c(3, f_d(4)) into :Cnt;",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{Params: []string{"CODE", "SUFFIX_ID"}})
	if callees := r.Callees(); len(callees) != 0 {
		t.Fatalf("Callees() = %+v, want none", callees)
	}
	if r.MayWrite() {
		t.Fatal("MayWrite() = true, want false")
	}

	body = strings.Join([]string{
		"begin",
		"x = (select max(a) from p_src(2));",
		"select a from t, p_src(1) into :x;",
		"select f_a(1,",
		"  p_two(2))",
		"  from p_two(1) into :x;",
		"end",
	}, "\n")
	r = parseClean(t, body, ParseOptions{})
	want := []Callee{
		{Name: "P_SRC", Lines: []int{2, 3}},
		{Name: "P_TWO", Lines: []int{6}},
	}
	if got := r.Callees(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Callees() = %+v, want %+v", got, want)
	}
	if !r.MayWrite() {
		t.Fatal("MayWrite() = false, want true")
	}
}

func TestParseRoutineReturningValuesParentheses(t *testing.T) {
	body := strings.Join([]string{
		"DECLARE VARIABLE a INTEGER;",
		"DECLARE VARIABLE b INTEGER;",
		"begin",
		"execute procedure p_two 1, 2 returning_values (:a, :b);",
		"execute procedure p_two(1, 2) returning_values (a);",
		"execute procedure p_two (1, 2) returning_values :a, b;",
		"end",
	}, "\n")
	r := parseClean(t, body, ParseOptions{})
	for i, want := range [][]string{{"A", "B"}, {"A"}, {"A", "B"}} {
		n := r.Body.Children[i]
		if n.Proc == nil || len(n.Proc.Args) != 2 || !reflect.DeepEqual(n.Returning, want) {
			t.Fatalf("statement %d: Proc %+v Returning %v, want 2 args and %v", i, n.Proc, n.Returning, want)
		}
	}
}

func TestParseRoutineMalformedPositions(t *testing.T) {
	for _, tt := range []struct {
		body         string
		line, column int
		message      string
	}{
		{"begin\n  x = 1;\n  execute procedure p 1 returning_values (:a, 1);\nend", 3, 25, "malformed RETURNING_VALUES list"},
		{"begin\n  execute procedure p 1 returning_values :a +;\nend", 2, 25, "malformed RETURNING_VALUES list"},
		{"begin\n  execute procedure p 1 returning_values ();\nend", 2, 25, "malformed RETURNING_VALUES list"},
		{"begin\n\n    execute procedure p(1,,2);\nend", 3, 23, "malformed EXECUTE PROCEDURE argument list"},
		{"begin\n  execute procedure;\nend", 2, 3, "EXECUTE PROCEDURE without a procedure name"},
	} {
		_, problems := ParseRoutine(tt.body, ParseOptions{})
		if len(problems) != 1 || problems[0].Line != tt.line || problems[0].Column != tt.column || problems[0].Message != tt.message {
			t.Errorf("ParseRoutine(%q) problems = %+v, want %q at %d:%d", tt.body, problems, tt.message, tt.line, tt.column)
		}
	}
}

func TestRoutineCalleeLinesAreSortedAndUnique(t *testing.T) {
	r := &Routine{calls: []callSite{{name: "P", line: 5}, {name: "P", line: 3}, {name: "P", line: 5}, {name: "Q", line: 4}, {name: "P", line: 3}}}
	want := []Callee{{Name: "P", Lines: []int{3, 5}}, {Name: "Q", Lines: []int{4}}}
	if got := r.Callees(); !reflect.DeepEqual(got, want) {
		t.Fatalf("Callees() = %+v, want %+v", got, want)
	}
}
