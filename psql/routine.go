package psql

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// ParseOptions configures ParseRoutine.
type ParseOptions struct {
	Dialect int      // 1 or 3; 0 means 3
	Params  []string // input and output parameter names as stored in metadata (upper case unless created quoted); trailing blanks are ignored
}

// Problem is a construct ParseRoutine cannot represent. Line and Column are
// 1-based; Column counts bytes.
type Problem struct {
	Line, Column int
	Message      string
}

// Declaration is a DECLARE VARIABLE of the routine.
type Declaration struct {
	Name     string // normalized (upper case unless quoted)
	TypeText string // declared type text as written, e.g. "VARCHAR(20)", "NUMERIC(15,2)", "CHAR(1)"
	Line     int
}

// NodeKind identifies a statement node.
type NodeKind string

const (
	NodeBlock, NodeAssign, NodeIf, NodeWhile                          NodeKind = "block", "assign", "if", "while"
	NodeSelectInto, NodeForSelect, NodeInsert, NodeUpdate, NodeDelete NodeKind = "selectInto", "forSelect", "insert", "update", "delete"
	NodeExecProc, NodeSuspend, NodeExit, NodeBreak                    NodeKind = "executeProcedure", "suspend", "exit", "break"
	NodeException, NodePostEvent, NodeEmpty                           NodeKind = "exception", "postEvent", "empty"
	NodeExecStmt, NodeForExecStmt                                     NodeKind = "executeStatement", "forExecuteStatement"
)

// Node is one statement. Line/Column locate its first byte and
// EndLine/EndColumn its last byte (the terminating ";" or END included).
type Node struct {
	ID                               int // index in Routine.Nodes
	Kind                             NodeKind
	Line, Column, EndLine, EndColumn int
	Children                         []*Node    // block: statements; if: [then] or [then, else]; while/forSelect/forExecuteStatement: [body]
	Handlers                         []*Handler // block only
	Target                           string     // assign: normalized variable name
	Value                            *Expr      // assign value; exception message; postEvent name; executeStatement/forExecuteStatement statement text
	Cond                             *Expr      // if/while
	SQL                              *SQLText   // selectInto/forSelect/insert/update/delete
	Into                             []string   // selectInto/forSelect/executeStatement/forExecuteStatement targets (normalized); nil for executeStatement without INTO
	Proc                             *ProcCall  // executeProcedure: callee + args; selectInto/forSelect: set when the shape is "plain output columns FROM p(args)" (spec 6.7)
	Returning                        []string   // executeProcedure RETURNING_VALUES targets
	Exception                        string     // exception: normalized exception name
}

// Handler is a WHEN … DO at the end of a block.
type Handler struct {
	Line       int
	Conditions []WhenCondition
	Body       *Node
}

// WhenCondition is one condition of a WHEN handler.
type WhenCondition struct {
	Kind string // "exception" | "sqlcode" | "gdscode" | "any"
	Name string // exception or gds name, normalized
	Code int32  // sqlcode
}

// SQLText is a data statement ready for DSQL.
type SQLText struct {
	Text   string   // statement with INTO removed and :name replaced by "?"
	Params []string // normalized variable names per "?"
}

// ProcCall is a procedure invocation.
type ProcCall struct {
	Name    string
	Quoted  bool
	Args    []*Expr  // each argument expression
	Columns []string // selectInto/forSelect: selected output columns in order (normalized)
}

// Routine is a parsed procedure body.
type Routine struct {
	Declarations []Declaration
	Body         *Node   // the outermost BEGIN … END
	Nodes        []*Node // every node, Nodes[i].ID == i

	calls   []callSite
	sources []callSite // bare FROM/JOIN source names
	genID   bool
}

type callSite struct {
	name        string
	quoted      bool
	line        int
	stepInShape bool
}

// Callee is a procedure referenced by a routine.
type Callee struct {
	Name        string
	Quoted      bool
	Lines       []int
	StepInShape bool // at least one call site has a step-in shape (spec 6.7)
}

type parser struct {
	src      string
	toks     []token
	pos      int
	problems []Problem
	routine  *Routine
	dialect  int
	names    map[string]bool // declared variables and parameters
	params   []string
}

// ParseRoutine parses a routine body as stored in RDB$PROCEDURE_SOURCE
// (declarations, then BEGIN … END) into a statement tree. Every unsupported
// construct is reported; the routine is usable only when no problem is
// returned.
func ParseRoutine(body string, opts ParseOptions) (r *Routine, problems []Problem) {
	r = &Routine{}
	p := &parser{src: body, routine: r, params: opts.Params}
	defer func() {
		if recovered := recover(); recovered != nil {
			r = &Routine{}
			problems = []Problem{{Line: 1, Column: 1, Message: fmt.Sprintf("internal parser error: %v", recovered)}}
		}
	}()
	dialect := opts.Dialect
	if dialect == 0 {
		dialect = 3
	}
	p.dialect = dialect
	p.toks = lexDialect(body, dialect)
	p.parseRoutine()
	for i := 0; i+1 < len(p.toks); i++ {
		if keyword(p.toks[i], "GEN_ID") && p.punct(i+1, "(") {
			r.genID = true
		}
	}
	if r.Body != nil {
		var walk func(n *Node)
		walk = func(n *Node) {
			n.ID = len(r.Nodes)
			r.Nodes = append(r.Nodes, n)
			for _, child := range n.Children {
				walk(child)
			}
			for _, h := range n.Handlers {
				walk(h.Body)
			}
		}
		walk(r.Body)
	}
	sort.SliceStable(p.problems, func(i, j int) bool {
		a, b := p.problems[i], p.problems[j]
		return a.Line < b.Line || a.Line == b.Line && a.Column < b.Column
	})
	return r, p.problems
}

// HasSuspend reports whether the routine contains SUSPEND.
func (r *Routine) HasSuspend() bool {
	for _, n := range r.Nodes {
		if n.Kind == NodeSuspend {
			return true
		}
	}
	return false
}

// MayWrite reports whether running the routine may change data: any DML,
// EXECUTE PROCEDURE, EXECUTE STATEMENT (its text is known only at run
// time), procedure in FROM, or GEN_ID. POST_EVENT alone does not count: the
// line profiler runs it as a no-op.
func (r *Routine) MayWrite() bool {
	if r.genID || len(r.calls) > 0 {
		return true
	}
	for _, n := range r.Nodes {
		switch n.Kind {
		case NodeInsert, NodeUpdate, NodeDelete, NodeExecProc, NodeExecStmt, NodeForExecStmt:
			return true
		}
	}
	return false
}

// Callees lists the procedures referenced by EXECUTE PROCEDURE or in FROM,
// sorted by first line.
func (r *Routine) Callees() []Callee {
	return groupSites(r.calls)
}

// CallSite is one procedure reference of Callees: StepInShape is set when
// this call has a step-in shape (spec 6.7).
type CallSite struct {
	Name        string
	Quoted      bool
	Line        int
	StepInShape bool
}

// CallSites lists every procedure reference of Callees, in source order.
func (r *Routine) CallSites() []CallSite {
	sites := make([]CallSite, len(r.calls))
	for i, site := range r.calls {
		sites[i] = CallSite{Name: site.name, Quoted: site.quoted, Line: site.line, StepInShape: site.stepInShape}
	}
	return sites
}

// SourceNames lists the FROM/JOIN source names written without an argument
// list, sorted by first line. Each is a table, a view or a parameterless
// procedure; only the catalog can tell which.
func (r *Routine) SourceNames() []Callee {
	return groupSites(r.sources)
}

func groupSites(sites []callSite) []Callee {
	index := make(map[string]int)
	var callees []Callee
	for _, site := range sites {
		i, ok := index[site.name]
		if !ok {
			i = len(callees)
			index[site.name] = i
			callees = append(callees, Callee{Name: site.name, Quoted: site.quoted})
		}
		c := &callees[i]
		if n := len(c.Lines); n == 0 || c.Lines[n-1] != site.line {
			c.Lines = append(c.Lines, site.line)
		}
		c.StepInShape = c.StepInShape || site.stepInShape
	}
	for i := range callees {
		sort.Ints(callees[i].Lines)
	}
	sort.SliceStable(callees, func(i, j int) bool {
		return callees[i].Lines[0] < callees[j].Lines[0]
	})
	return callees
}

func (p *parser) atEnd() bool { return p.pos >= len(p.toks) }

func (p *parser) kw(i int, name string) bool {
	return i >= 0 && i < len(p.toks) && keyword(p.toks[i], name)
}

func (p *parser) punct(i int, text string) bool {
	return i >= 0 && i < len(p.toks) && p.toks[i].kind == tokenPunctuation && p.toks[i].text == text
}

func (p *parser) semicolon(i int) bool {
	return p.punct(i, ";") && p.toks[i].depth == 0
}

func (p *parser) problemAt(i int, format string, args ...any) {
	line, column := 1, 1
	if len(p.toks) > 0 {
		if i >= len(p.toks) {
			i = len(p.toks) - 1
		}
		if i < 0 {
			i = 0
		}
		line, column = p.toks[i].line, p.toks[i].column
	}
	p.problems = append(p.problems, Problem{Line: line, Column: column, Message: fmt.Sprintf(format, args...)})
}

func (p *parser) describe(i int) string {
	if i >= len(p.toks) {
		return "end of body"
	}
	return fmt.Sprintf("%q", p.toks[i].text)
}

func (p *parser) newNode(kind NodeKind, i int) *Node {
	t := p.toks[i]
	return &Node{Kind: kind, Line: t.line, Column: t.column}
}

// finish sets the node's end to the last byte of token i.
func (p *parser) finish(n *Node, i int) {
	if i >= len(p.toks) {
		i = len(p.toks) - 1
	}
	t := p.toks[i]
	line, lineStart := countLines(p.src, t.start, t.end-1, t.line, t.start-t.column+1)
	n.EndLine, n.EndColumn = line, t.end-lineStart
}

func (p *parser) finishLike(n, last *Node) {
	n.EndLine, n.EndColumn = last.EndLine, last.EndColumn
}

func (p *parser) text(start, end int) string {
	if start >= end {
		return ""
	}
	return p.src[p.toks[start].start:p.toks[end-1].end]
}

// expr also records procedures referenced by subqueries in the expression.
func (p *parser) expr(start, end int) *Expr {
	p.recordSQLCalls(start, -1, end, false)
	return p.analyze(start, end, false)
}

// analyze builds the Expr of tokens [start, end); sqlOnly applies the SQL
// variable rule (only :name).
func (p *parser) analyze(start, end int, sqlOnly bool) *Expr {
	return analyzeExpr(p.text(start, end), p.toks[start].line, p.toks[start].column, p.names, p.dialect, sqlOnly)
}

// args analyzes the comma-separated arguments in tokens [start, end); ok
// is false for an empty argument or unbalanced parentheses.
func (p *parser) args(start, end int, sqlOnly bool) ([]*Expr, bool) {
	pieces, ok := splitArgs(p.toks, start, end)
	if !ok {
		return nil, false
	}
	var args []*Expr
	for _, piece := range pieces {
		if sqlOnly {
			args = append(args, p.analyze(piece[0], piece[1], true))
		} else {
			args = append(args, p.expr(piece[0], piece[1]))
		}
	}
	return args, true
}

// scanTo returns the index of the first depth-0 token from start matching
// stop, or len(tokens).
func (p *parser) scanTo(start int, stop func(int) bool) int {
	for i := start; i < len(p.toks); i++ {
		if p.toks[i].depth == 0 && stop(i) {
			return i
		}
	}
	return len(p.toks)
}

// skipStatement resumes after the next ";" at the same BEGIN/END depth,
// stopping before an END that closes the enclosing block.
func (p *parser) skipStatement() {
	nesting := 0
	for !p.atEnd() {
		switch {
		case p.toks[p.pos].depth != 0:
		case p.kw(p.pos, "BEGIN"):
			nesting++
		case p.kw(p.pos, "END"):
			if nesting == 0 {
				return
			}
			nesting--
		case p.semicolon(p.pos) && nesting == 0:
			p.pos++
			return
		}
		p.pos++
	}
}

func (p *parser) parseRoutine() {
	for p.kw(p.pos, "DECLARE") {
		p.parseDeclaration()
	}
	p.names = make(map[string]bool, len(p.routine.Declarations)+len(p.params))
	for _, d := range p.routine.Declarations {
		p.names[d.Name] = true
	}
	for _, name := range p.params {
		p.names[strings.TrimRight(name, " ")] = true
	}
	if !p.kw(p.pos, "BEGIN") {
		p.problemAt(p.pos, "expected BEGIN, found %s", p.describe(p.pos))
		for !p.atEnd() && !(p.kw(p.pos, "BEGIN") && p.toks[p.pos].depth == 0) {
			p.pos++
		}
		if p.atEnd() {
			return
		}
	}
	p.routine.Body = p.parseBlock()
	for p.semicolon(p.pos) {
		p.pos++
	}
	if !p.atEnd() {
		p.problemAt(p.pos, "unexpected %s after the routine's END", p.describe(p.pos))
	}
}

func (p *parser) parseDeclaration() {
	start := p.pos
	p.pos++
	end := p.scanTo(p.pos, p.semicolon)
	defer func() { p.pos = end + 1 }()
	if !p.kw(p.pos, "VARIABLE") {
		p.problemAt(start, "DECLARE %s is not supported", p.describe(p.pos))
		return
	}
	nameIndex := p.pos + 1
	if nameIndex >= end || !isNameToken(p.toks[nameIndex]) {
		p.problemAt(start, "DECLARE VARIABLE without a name")
		return
	}
	typeStart := nameIndex + 1
	if typeStart >= end {
		p.problemAt(start, "DECLARE VARIABLE without a type")
		return
	}
	for i := typeStart; i < end; i++ {
		if p.toks[i].depth == 0 && (p.punct(i, "=") || p.kw(i, "DEFAULT")) {
			p.problemAt(start, "DECLARE VARIABLE with an initial value (DEFAULT or =) is not supported")
			return
		}
	}
	if end >= len(p.toks) {
		p.problemAt(start, "DECLARE VARIABLE without a terminating \";\"")
	}
	p.routine.Declarations = append(p.routine.Declarations, Declaration{
		Name: normalizedName(p.toks[nameIndex]), TypeText: p.text(typeStart, end), Line: p.toks[start].line,
	})
}

func (p *parser) parseBlock() *Node {
	begin := p.pos
	n := p.newNode(NodeBlock, begin)
	p.pos++
	for {
		if p.atEnd() {
			p.problemAt(begin, "BEGIN without END")
			p.finish(n, len(p.toks)-1)
			return n
		}
		if p.kw(p.pos, "END") {
			p.finish(n, p.pos)
			p.pos++
			return n
		}
		if p.kw(p.pos, "WHEN") {
			if h := p.parseHandler(); h != nil {
				n.Handlers = append(n.Handlers, h)
			}
			continue
		}
		before := p.pos
		if len(n.Handlers) > 0 {
			p.problemAt(p.pos, "statement after a WHEN handler; handlers must end the block")
		}
		stmt := p.parseStatement()
		if stmt != nil && len(n.Handlers) == 0 {
			n.Children = append(n.Children, stmt)
		}
		if p.pos == before {
			p.pos++
		}
	}
}

func (p *parser) parseHandler() *Handler {
	h := &Handler{Line: p.toks[p.pos].line}
	p.pos++
	for {
		i := p.pos
		switch {
		case p.kw(i, "EXCEPTION") && i+1 < len(p.toks) && isNameToken(p.toks[i+1]):
			h.Conditions = append(h.Conditions, WhenCondition{Kind: "exception", Name: normalizedName(p.toks[i+1])})
			p.pos += 2
		case p.kw(i, "GDSCODE") && i+1 < len(p.toks) && isNameToken(p.toks[i+1]):
			h.Conditions = append(h.Conditions, WhenCondition{Kind: "gdscode", Name: normalizedName(p.toks[i+1])})
			p.pos += 2
		case p.kw(i, "ANY"):
			h.Conditions = append(h.Conditions, WhenCondition{Kind: "any"})
			p.pos++
		case p.kw(i, "SQLCODE"):
			j, sign := i+1, ""
			if p.punct(j, "-") || p.punct(j, "+") {
				sign = p.toks[j].text
				j++
			}
			if j >= len(p.toks) || p.toks[j].kind != tokenNumber {
				p.problemAt(i, "WHEN SQLCODE without a number")
				return p.recoverHandler()
			}
			code, err := strconv.ParseInt(sign+p.toks[j].text, 10, 32)
			if err != nil {
				p.problemAt(j, "WHEN SQLCODE %s%s is out of range", sign, p.toks[j].text)
				return p.recoverHandler()
			}
			h.Conditions = append(h.Conditions, WhenCondition{Kind: "sqlcode", Code: int32(code)})
			p.pos = j + 1
		default:
			p.problemAt(i, "unsupported WHEN condition %s", p.describe(i))
			return p.recoverHandler()
		}
		if p.punct(p.pos, ",") {
			p.pos++
			continue
		}
		break
	}
	if !p.kw(p.pos, "DO") {
		p.problemAt(p.pos, "expected DO in WHEN, found %s", p.describe(p.pos))
		return p.recoverHandler()
	}
	p.pos++
	if h.Body = p.parseStatement(); h.Body == nil {
		return nil
	}
	return h
}

// recoverHandler skips a malformed handler's conditions and its body.
func (p *parser) recoverHandler() *Handler {
	do := p.scanTo(p.pos, func(i int) bool { return p.kw(i, "DO") || p.semicolon(i) || p.kw(i, "END") })
	p.pos = do
	if p.kw(do, "DO") {
		p.pos++
		p.parseStatement()
	} else if p.semicolon(do) {
		p.pos++
	}
	return nil
}

// parseStatement parses one statement. It returns nil after reporting a
// problem; it does not consume END, ELSE or WHEN when one starts a statement.
func (p *parser) parseStatement() *Node {
	if p.atEnd() {
		p.problemAt(len(p.toks), "expected a statement, found end of body")
		return nil
	}
	i := p.pos
	t := p.toks[i]
	switch {
	case p.punct(i, ";"):
		n := p.newNode(NodeEmpty, i)
		p.finish(n, i)
		p.pos++
		return n
	case isNameToken(t) && p.punct(i+1, "=") && t.depth == 0:
		return p.parseAssign()
	case p.kw(i, "BEGIN"):
		return p.parseBlock()
	case p.kw(i, "IF"):
		return p.parseIf()
	case p.kw(i, "WHILE"):
		return p.parseWhile()
	case p.kw(i, "SELECT"):
		return p.parseSelectInto()
	case p.kw(i, "FOR"):
		return p.parseFor()
	case p.kw(i, "INSERT"):
		return p.parseDML(NodeInsert)
	case p.kw(i, "UPDATE"):
		return p.parseDML(NodeUpdate)
	case p.kw(i, "DELETE"):
		return p.parseDML(NodeDelete)
	case p.kw(i, "EXECUTE") && p.kw(i+1, "PROCEDURE"):
		return p.parseExecProc()
	case p.kw(i, "EXECUTE") && p.kw(i+1, "STATEMENT"):
		return p.parseExecStmt()
	case p.kw(i, "SUSPEND"):
		return p.parseSimple(NodeSuspend)
	case p.kw(i, "EXIT"):
		return p.parseSimple(NodeExit)
	case p.kw(i, "BREAK"), p.kw(i, "LEAVE"):
		return p.parseSimple(NodeBreak)
	case p.kw(i, "EXCEPTION"):
		return p.parseException()
	case p.kw(i, "POST_EVENT"):
		return p.parsePostEvent()
	case p.kw(i, "END"), p.kw(i, "ELSE"), p.kw(i, "WHEN"):
		p.problemAt(i, "expected a statement, found %s", p.describe(i))
		return nil
	default:
		p.problemAt(i, "unsupported statement starting with %s", asciiUpper(p.describe(i)))
		p.skipStatement()
		return nil
	}
}

// terminate consumes the ";" ending node n.
func (p *parser) terminate(n *Node) *Node {
	if !p.semicolon(p.pos) {
		p.problemAt(p.pos, "expected \";\", found %s", p.describe(p.pos))
		p.skipStatement()
		return nil
	}
	p.finish(n, p.pos)
	p.pos++
	return n
}

func (p *parser) parseSimple(kind NodeKind) *Node {
	n := p.newNode(kind, p.pos)
	p.pos++
	return p.terminate(n)
}

func (p *parser) parseAssign() *Node {
	n := p.newNode(NodeAssign, p.pos)
	n.Target = normalizedName(p.toks[p.pos])
	start := p.pos + 2
	end := p.scanTo(start, p.semicolon)
	if start >= end {
		p.problemAt(p.pos, "assignment without a value")
		p.pos = start
		p.skipStatement()
		return nil
	}
	n.Value = p.expr(start, end)
	p.pos = end
	return p.terminate(n)
}

// parseCondition parses "(cond)" followed by the keyword then.
func (p *parser) parseCondition(n *Node, then string) bool {
	open := p.pos
	if !p.punct(open, "(") {
		p.problemAt(open, "expected \"(\" after %s, found %s", n.Kind, p.describe(open))
		p.skipStatement()
		return false
	}
	closeIndex := -1
	for i := open + 1; i < len(p.toks); i++ {
		if p.punct(i, ")") && p.toks[i].depth == p.toks[open].depth+1 {
			closeIndex = i
			break
		}
	}
	if closeIndex < 0 || closeIndex == open+1 {
		p.problemAt(open, "malformed condition")
		p.pos = open
		p.skipStatement()
		return false
	}
	n.Cond = p.expr(open+1, closeIndex)
	p.pos = closeIndex + 1
	if !p.kw(p.pos, then) {
		p.problemAt(p.pos, "expected %s, found %s", then, p.describe(p.pos))
		p.skipStatement()
		return false
	}
	p.pos++
	return true
}

func (p *parser) parseIf() *Node {
	n := p.newNode(NodeIf, p.pos)
	p.pos++
	if !p.parseCondition(n, "THEN") {
		return nil
	}
	then := p.parseStatement()
	var elseStmt *Node
	hasElse := p.kw(p.pos, "ELSE")
	if hasElse {
		p.pos++
		if elseStmt = p.parseStatement(); elseStmt == nil {
			return nil
		}
	}
	if then == nil {
		return nil
	}
	n.Children = []*Node{then}
	if hasElse {
		n.Children = append(n.Children, elseStmt)
	}
	p.finishLike(n, n.Children[len(n.Children)-1])
	return n
}

func (p *parser) parseWhile() *Node {
	n := p.newNode(NodeWhile, p.pos)
	p.pos++
	if !p.parseCondition(n, "DO") {
		return nil
	}
	return p.withBody(n)
}

func (p *parser) withBody(n *Node) *Node {
	body := p.parseStatement()
	if body == nil {
		return nil
	}
	n.Children = []*Node{body}
	p.finishLike(n, body)
	return n
}

// intoClause finds the trailing "INTO targets" of tokens [start, end) and
// returns the INTO index and the normalized targets.
func (p *parser) intoClause(start, end int) (int, []string) {
	for i := start; i < end; i++ {
		if p.toks[i].depth != 0 || !p.kw(i, "INTO") {
			continue
		}
		last, ok := intoTargetListEnd(p.toks, p.src, i+1, end)
		if !ok || last+1 != end {
			continue
		}
		return i, p.targetNames(i+1, last+1)
	}
	return -1, nil
}

// targetNames normalizes a target list already validated by intoTargetListEnd.
func (p *parser) targetNames(start, end int) []string {
	names := make([]string, 0)
	for i := start; i < end; i++ {
		t := p.toks[i]
		switch {
		case t.kind == tokenParameter:
			names = append(names, asciiUpper(t.text))
		case p.punct(i, ":") || p.punct(i, ","):
		case isNameToken(t):
			name := normalizedName(t)
			if p.punct(i+1, ".") && i+2 < end {
				name += "." + normalizedName(p.toks[i+2])
				i += 2
			}
			names = append(names, name)
		default:
			names = append(names, t.text)
		}
	}
	return names
}

func (p *parser) sqlText(kind NodeKind, start, end int) *SQLText {
	rewriteKind := string(kind)
	if kind == NodeSelectInto {
		rewriteKind = "select"
	}
	text, params := rewriteSQLParams(p.src, p.toks, start, end, p.toks[end-1].end, rewriteKind, false)
	return &SQLText{Text: text, Params: params}
}

// stepInProc builds the ProcCall of a step-in shape whose procedure name is
// at token name and whose SELECT is at token selectIndex.
func (p *parser) stepInProc(selectIndex, name int) (*ProcCall, bool) {
	t := p.toks[name]
	proc := &ProcCall{Name: normalizedName(t), Quoted: t.kind == tokenQuotedIdentifier}
	if !p.punct(selectIndex+1, "*") {
		proc.Columns = []string{}
		for i := selectIndex + 1; !p.kw(i, "FROM"); i++ {
			if !p.punct(i, ",") {
				proc.Columns = append(proc.Columns, normalizedName(p.toks[i]))
			}
		}
	}
	args, ok := p.args(name+2, matchingClose(p.toks, name+1), true)
	proc.Args = args
	return proc, ok
}

func (p *parser) parseSelectInto() *Node {
	n := p.newNode(NodeSelectInto, p.pos)
	start := p.pos
	end := p.scanTo(start, p.semicolon)
	into, targets := p.intoClause(start, end)
	if into < 0 {
		p.problemAt(start, "SELECT without a trailing INTO is not supported")
		p.skipStatement()
		return nil
	}
	n.SQL, n.Into = p.sqlText(n.Kind, start, end), targets
	if name := p.recordSQLCalls(start, into, end, false); name >= 0 {
		var ok bool
		if n.Proc, ok = p.stepInProc(start, name); !ok {
			p.problemAt(name, "malformed procedure argument list")
			p.pos = end
			p.skipStatement()
			return nil
		}
	}
	p.pos = end
	return p.terminate(n)
}

func (p *parser) parseFor() *Node {
	n := p.newNode(NodeForSelect, p.pos)
	forIndex := p.pos
	start := p.pos + 1
	if p.kw(start, "EXECUTE") && p.kw(start+1, "STATEMENT") {
		return p.parseForExecStmt()
	}
	if !p.kw(start, "SELECT") {
		p.problemAt(forIndex, "FOR %s is not supported", asciiUpper(p.describe(start)))
		do := p.scanTo(start, func(i int) bool { return p.kw(i, "DO") || p.semicolon(i) })
		p.pos = do
		if p.kw(do, "DO") {
			p.pos++
			p.parseStatement()
		} else {
			p.skipStatement()
		}
		return nil
	}
	end := p.scanTo(start, func(i int) bool { return p.kw(i, "DO") || p.semicolon(i) })
	if !p.kw(end, "DO") {
		p.problemAt(forIndex, "FOR SELECT without DO")
		p.pos = end
		p.skipStatement()
		return nil
	}
	ok := true
	for i := start; i+1 < end; i++ {
		if p.toks[i].depth == 0 && p.kw(i, "AS") && p.kw(i+1, "CURSOR") {
			p.problemAt(i, "FOR SELECT … AS CURSOR is not supported")
			ok = false
		}
	}
	into, targets := -1, []string(nil)
	if ok {
		if into, targets = p.intoClause(start, end); into < 0 {
			p.problemAt(forIndex, "FOR SELECT without a trailing INTO is not supported")
			ok = false
		}
	}
	p.pos = end + 1
	if !ok {
		p.parseStatement()
		return nil
	}
	n.SQL, n.Into = p.sqlText(n.Kind, start, end), targets
	if name := p.recordSQLCalls(start, into, end, false); name >= 0 {
		var ok bool
		if n.Proc, ok = p.stepInProc(start, name); !ok {
			p.problemAt(name, "malformed procedure argument list")
			p.parseStatement()
			return nil
		}
	}
	return p.withBody(n)
}

func (p *parser) parseDML(kind NodeKind) *Node {
	n := p.newNode(kind, p.pos)
	start := p.pos
	end := p.scanTo(start, p.semicolon)
	for i := start; i+2 < end; i++ {
		if p.toks[i].depth == 0 && p.kw(i, "WHERE") && p.kw(i+1, "CURRENT") && p.kw(i+2, "OF") {
			p.problemAt(i, "WHERE CURRENT OF is not supported")
			p.pos = end
			p.skipStatement()
			return nil
		}
	}
	n.SQL = p.sqlText(kind, start, end)
	p.recordSQLCalls(start, -1, end, kind == NodeDelete)
	p.pos = end
	return p.terminate(n)
}

// recordSQLCalls records procedures referenced in the FROM/JOIN list of a
// query or subquery in tokens [start, end). For a SELECT … INTO (into >= 0)
// whose shape is plain output columns FROM p(args), it returns the index of
// the procedure name, otherwise -1.
func (p *parser) recordSQLCalls(start, into, end int, skipDeleteTarget bool) int {
	sources := sourceSites(p.toks, start, end, skipDeleteTarget)
	var sites []int
	for _, i := range sources {
		t := p.toks[i]
		if i+1 < end && isPunct(p.toks[i+1], "(") {
			sites = append(sites, i)
		} else {
			p.routine.sources = append(p.routine.sources, callSite{
				name: normalizedName(t), quoted: t.kind == tokenQuotedIdentifier, line: t.line,
			})
		}
	}
	if len(sites) == 0 {
		return -1
	}
	stepIn := -1
	if into >= 0 {
		stepIn = p.stepInCall(start, into)
	}
	for _, i := range sites {
		t := p.toks[i]
		p.routine.calls = append(p.routine.calls, callSite{
			name: normalizedName(t), quoted: t.kind == tokenQuotedIdentifier, line: t.line, stepInShape: i == stepIn,
		})
	}
	return stepIn
}

// sourceSites returns the indices of the source names in the FROM/JOIN
// list of a query in tokens [start, end); a name followed by "(" is a
// procedure. A FROM counts only after a SELECT in the same parenthesis group,
// so FROM inside EXTRACT(… FROM …) or SUBSTRING(… FROM …) and function
// arguments are not sources.
func sourceSites(tokens []token, start, end int, skipDeleteTarget bool) []int {
	var sites []int
	query := make(map[int]bool)
	fromList := make(map[int]bool)
	add := func(i int) {
		if i < end && isNameToken(tokens[i]) {
			sites = append(sites, i)
		}
	}
	for i := start; i < end; i++ {
		t := tokens[i]
		d := t.depth
		switch {
		case isPunct(t, "("):
			query[d+1], fromList[d+1] = false, false
		case isPunct(t, ")"):
			query[d], fromList[d] = false, false
		case isPunct(t, ";"):
			fromList[d] = false
		case isPunct(t, ","):
			if fromList[d] {
				add(i + 1)
			}
		case t.kind != tokenIdentifier:
		case keyword(t, "SELECT"):
			query[d] = true
		case isSourceListEnd(t):
			fromList[d] = false
		case keyword(t, "FROM"):
			if skipDeleteTarget && i > start && keyword(tokens[i-1], "DELETE") && tokens[i-1].depth == d {
				continue
			}
			if fromList[d] = query[d]; fromList[d] {
				add(i + 1)
			}
		case keyword(t, "JOIN"):
			if fromList[d] {
				add(i + 1)
			}
		}
	}
	return sites
}

// stepInCall returns the procedure name index when tokens [select, into)
// are "SELECT * | col, … FROM p(args)", otherwise -1.
func (p *parser) stepInCall(selectIndex, into int) int {
	i := selectIndex + 1
	if p.punct(i, "*") {
		i++
	} else {
		for {
			if i >= into || !isNameToken(p.toks[i]) || p.kw(i, "FROM") {
				return -1
			}
			i++
			if !p.punct(i, ",") {
				break
			}
			i++
		}
	}
	if !p.kw(i, "FROM") || i+2 >= into || !isNameToken(p.toks[i+1]) || !p.punct(i+2, "(") {
		return -1
	}
	name, open := i+1, i+2
	for j := open + 1; j < into; j++ {
		if p.punct(j, ")") && p.toks[j].depth == p.toks[open].depth+1 {
			if j+1 == into {
				return name
			}
			return -1
		}
	}
	return -1
}

func (p *parser) parseExecProc() *Node {
	n := p.newNode(NodeExecProc, p.pos)
	nameIndex := p.pos + 2
	if nameIndex >= len(p.toks) || !isNameToken(p.toks[nameIndex]) {
		p.problemAt(p.pos, "EXECUTE PROCEDURE without a procedure name")
		p.skipStatement()
		return nil
	}
	end := p.scanTo(nameIndex, func(i int) bool { return p.semicolon(i) || p.kw(i, "RETURNING_VALUES") })
	t := p.toks[nameIndex]
	n.Proc = &ProcCall{Name: normalizedName(t), Quoted: t.kind == tokenQuotedIdentifier}
	argStart, argEnd := nameIndex+1, end
	if p.punct(argStart, "(") && matchingClose(p.toks[:end], argStart) == end-1 {
		argStart, argEnd = argStart+1, end-1
	}
	args, ok := p.args(argStart, argEnd, false)
	if !ok {
		p.problemAt(nameIndex, "malformed EXECUTE PROCEDURE argument list")
		p.skipStatement()
		return nil
	}
	n.Proc.Args = args
	p.pos = end
	if p.kw(end, "RETURNING_VALUES") {
		listEnd := p.scanTo(end+1, p.semicolon)
		start, stop := end+1, listEnd
		if p.punct(start, "(") && stop-1 > start && p.punct(stop-1, ")") {
			start, stop = start+1, stop-1
		}
		last, ok := intoTargetListEnd(p.toks, p.src, start, stop)
		if !ok || last+1 != stop {
			p.problemAt(end, "malformed RETURNING_VALUES list")
			p.skipStatement()
			return nil
		}
		n.Returning = p.targetNames(start, stop)
		p.pos = listEnd
	}
	p.routine.calls = append(p.routine.calls, callSite{name: n.Proc.Name, quoted: n.Proc.Quoted, line: t.line, stepInShape: true})
	return p.terminate(n)
}

// execStmtText parses "EXECUTE STATEMENT <expr> [INTO <targets>]" in
// tokens [p.pos, end) and sets n's text and targets; ok is false after a
// reported problem.
func (p *parser) execStmtText(n *Node, end int) bool {
	start := p.pos + 2
	into, targets := p.intoClause(start, end)
	exprEnd := end
	if into >= 0 {
		exprEnd = into
	}
	if start >= exprEnd {
		p.problemAt(p.pos, "EXECUTE STATEMENT without a statement text")
		return false
	}
	n.Value, n.Into = p.expr(start, exprEnd), targets
	return true
}

func (p *parser) parseExecStmt() *Node {
	n := p.newNode(NodeExecStmt, p.pos)
	end := p.scanTo(p.pos, p.semicolon)
	if !p.execStmtText(n, end) {
		p.skipStatement()
		return nil
	}
	p.pos = end
	return p.terminate(n)
}

// parseForExecStmt parses FOR EXECUTE STATEMENT <expr> INTO <targets> DO
// <stmt>; InterBase requires the INTO.
func (p *parser) parseForExecStmt() *Node {
	n := p.newNode(NodeForExecStmt, p.pos)
	forIndex := p.pos
	p.pos++
	end := p.scanTo(p.pos, func(i int) bool { return p.kw(i, "DO") || p.semicolon(i) })
	if !p.kw(end, "DO") {
		p.problemAt(forIndex, "FOR EXECUTE STATEMENT without DO")
		p.pos = end
		p.skipStatement()
		return nil
	}
	ok := p.execStmtText(n, end)
	if ok && n.Into == nil {
		p.problemAt(forIndex, "FOR EXECUTE STATEMENT without a trailing INTO is not supported")
		ok = false
	}
	p.pos = end + 1
	if !ok {
		p.parseStatement()
		return nil
	}
	return p.withBody(n)
}

func (p *parser) parseException() *Node {
	n := p.newNode(NodeException, p.pos)
	nameIndex := p.pos + 1
	if nameIndex >= len(p.toks) || !isNameToken(p.toks[nameIndex]) {
		p.problemAt(p.pos, "EXCEPTION without an exception name is not supported")
		p.skipStatement()
		return nil
	}
	n.Exception = normalizedName(p.toks[nameIndex])
	end := p.scanTo(nameIndex+1, p.semicolon)
	if nameIndex+1 < end {
		n.Value = p.expr(nameIndex+1, end)
	}
	p.pos = end
	return p.terminate(n)
}

func (p *parser) parsePostEvent() *Node {
	n := p.newNode(NodePostEvent, p.pos)
	start := p.pos + 1
	end := p.scanTo(start, p.semicolon)
	if start >= end {
		p.problemAt(p.pos, "POST_EVENT without an event name")
		p.skipStatement()
		return nil
	}
	n.Value = p.expr(start, end)
	p.pos = end
	return p.terminate(n)
}
