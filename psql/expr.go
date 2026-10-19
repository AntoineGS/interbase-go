package psql

import (
	"strconv"
	"strings"
)

// Expr is a PSQL value or condition expression.
type Expr struct {
	Text         string // original text
	Line, Column int
	ServerText   string    // variable references replaced by "?" (spec 5: bare or :name in expressions)
	Params       []string  // normalized variable names, one per "?" in ServerText order
	Local        LocalNode // nil when not in the exact local subset (spec 6.2)
}

// LocalNode is the exact local subset AST.
type LocalNode interface{ localNode() }

// LocalInt is an INTEGER-range literal.
type LocalInt struct{ Value int64 }

// LocalString is a string literal's bytes (charset NONE).
type LocalString struct{ Value []byte }

// LocalNull is the NULL literal.
type LocalNull struct{}

// LocalVar reads a variable or parameter.
type LocalVar struct{ Name string }

// LocalNot is NOT X.
type LocalNot struct{ X LocalNode }

// LocalIsNull is X IS [NOT] NULL.
type LocalIsNull struct {
	X       LocalNode
	Negated bool
}

// LocalBinary is L Op R; Op is one of "AND", "OR", "=", "<>", "<", ">",
// "<=", ">=", "+", "-".
type LocalBinary struct {
	Op   string
	L, R LocalNode
}

func (LocalInt) localNode()    {}
func (LocalString) localNode() {}
func (LocalNull) localNode()   {}
func (LocalVar) localNode()    {}
func (LocalNot) localNode()    {}
func (LocalIsNull) localNode() {}
func (LocalBinary) localNode() {}

const maxInteger = 1<<31 - 1

// AnalyzeExpr builds an Expr from source text using the variable name set.
// Names are normalized: upper case unless the variable was declared quoted.
// A bare name is a variable when it is in names and is neither called nor
// part of a dotted name; :name always refers to a variable and is left
// unchanged (with Local nil) when unknown. Inside a subquery only :name is
// a variable.
func AnalyzeExpr(text string, line, column int, names map[string]bool, dialect int) *Expr {
	return analyzeExpr(text, line, column, names, dialect, false)
}

// analyzeExpr applies the SQL variable rule (only :name) everywhere when
// sqlOnly is set.
func analyzeExpr(text string, line, column int, names map[string]bool, dialect int, sqlOnly bool) *Expr {
	if dialect == 0 {
		dialect = 3
	}
	tokens := lexDialect(text, dialect)
	e := &Expr{Text: text, Line: line, Column: column}
	refs, exact := variableRefs(text, tokens, names, sqlOnly)

	var server strings.Builder
	cursor := 0
	for i := 0; i < len(tokens); i++ {
		ref, ok := refs[i]
		if !ok {
			continue
		}
		server.WriteString(text[cursor:tokens[i].start])
		server.WriteString("?")
		cursor = tokens[ref.last].end
		e.Params = append(e.Params, ref.name)
		i = ref.last
	}
	server.WriteString(text[cursor:])
	e.ServerText = server.String()

	if exact {
		l := &localParser{text: text, toks: tokens, refs: refs}
		if node, _, ok := l.parseOr(); ok && l.pos == len(tokens) {
			e.Local = node
		}
	}
	return e
}

type variableRef struct {
	name string
	last int // index of the reference's last token
}

// variableRefs maps the first token of each variable reference to it.
// exact is false when a :name reference is unknown.
func variableRefs(text string, tokens []token, names map[string]bool, sqlOnly bool) (map[int]variableRef, bool) {
	refs := make(map[int]variableRef)
	exact := true
	sqlDepth, skipDepth, skipAt := -1, -1, -1
	punct := func(i int, value string) bool {
		return i >= 0 && i < len(tokens) && tokens[i].kind == tokenPunctuation && tokens[i].text == value
	}
	for i := 0; i < len(tokens); i++ {
		t := tokens[i]
		inSQL := sqlOnly || sqlDepth >= 0 && t.depth >= sqlDepth
		if sqlDepth >= 0 && punct(i, ")") && t.depth == sqlDepth {
			sqlDepth = -1
		}
		skipped := skipDepth >= 0 && t.depth >= skipDepth
		if skipDepth >= 0 && punct(i, ")") && t.depth == skipDepth {
			skipDepth = -1
		}
		switch {
		case t.kind == tokenParameter:
			name := asciiUpper(t.text)
			if names[name] {
				refs[i] = variableRef{name: name, last: i}
			} else {
				exact = false
			}
		case punct(i, ":"):
			if i+1 < len(tokens) && isNameToken(tokens[i+1]) && onlyWhitespace(text[t.end:tokens[i+1].start]) {
				name := normalizedName(tokens[i+1])
				if names[name] {
					refs[i] = variableRef{name: name, last: i + 1}
				} else {
					exact = false
				}
				i++
			} else {
				exact = false
			}
		case punct(i, "(") && i+1 < len(tokens) && keyword(tokens[i+1], "SELECT"):
			if sqlDepth < 0 && !sqlOnly {
				sqlDepth = t.depth + 1
			}
		case !isNameToken(t) || inSQL:
		case keyword(t, "AS"):
			if skipDepth < 0 && t.depth > 0 {
				skipDepth = t.depth
			}
		case keyword(t, "COLLATE") || keyword(t, "SET") && i > 0 && keyword(tokens[i-1], "CHARACTER"):
			skipAt = i + 1
		case (keyword(t, "EXTRACT") || keyword(t, "GEN_ID")) && punct(i+1, "("):
			skipAt = i + 2
		case skipped || i == skipAt:
		case punct(i+1, "(") || punct(i+1, ".") || punct(i-1, "."):
		case names[normalizedName(t)]:
			refs[i] = variableRef{name: normalizedName(t), last: i}
		}
	}
	return refs, exact
}

// localParser parses the exact local subset. Each parse method returns the
// node, whether it is a condition (rather than a value), and success.
type localParser struct {
	text string
	toks []token
	refs map[int]variableRef
	pos  int
}

func (l *localParser) keyword(name string) bool {
	return l.pos < len(l.toks) && keyword(l.toks[l.pos], name) && l.refs[l.pos].name == ""
}

// operator returns the operator at pos, joining adjacent "<", ">", "!" and
// "=" characters, and the number of tokens it spans.
func (l *localParser) operator() (string, int) {
	if l.pos >= len(l.toks) || l.toks[l.pos].kind != tokenPunctuation {
		return "", 0
	}
	t := l.toks[l.pos]
	if l.pos+1 < len(l.toks) {
		next := l.toks[l.pos+1]
		if next.kind == tokenPunctuation && next.start == t.end {
			switch t.text + next.text {
			case "<>", "!=":
				return "<>", 2
			case "<=", ">=":
				return t.text + next.text, 2
			}
		}
	}
	switch t.text {
	case "=", "<", ">", "+", "-":
		return t.text, 1
	}
	return t.text, 0
}

func (l *localParser) parseOr() (LocalNode, bool, bool) {
	return l.parseLogical("OR", l.parseAnd)
}

func (l *localParser) parseAnd() (LocalNode, bool, bool) {
	return l.parseLogical("AND", l.parseNot)
}

func (l *localParser) parseLogical(op string, operand func() (LocalNode, bool, bool)) (LocalNode, bool, bool) {
	left, isCond, ok := operand()
	if !ok {
		return nil, false, false
	}
	for l.keyword(op) {
		l.pos++
		right, rightCond, ok := operand()
		if !ok || !isCond || !rightCond {
			return nil, false, false
		}
		left = LocalBinary{Op: op, L: left, R: right}
	}
	return left, isCond, true
}

func (l *localParser) parseNot() (LocalNode, bool, bool) {
	if !l.keyword("NOT") {
		return l.parsePredicate()
	}
	l.pos++
	x, isCond, ok := l.parseNot()
	if !ok || !isCond {
		return nil, false, false
	}
	return LocalNot{X: x}, true, true
}

func (l *localParser) parsePredicate() (LocalNode, bool, bool) {
	left, isCond, ok := l.parseAdditive()
	if !ok {
		return nil, false, false
	}
	if l.keyword("IS") {
		l.pos++
		negated := l.keyword("NOT")
		if negated {
			l.pos++
		}
		if !l.keyword("NULL") || isCond {
			return nil, false, false
		}
		l.pos++
		return LocalIsNull{X: left, Negated: negated}, true, true
	}
	op, width := l.operator()
	switch op {
	case "=", "<>", "<", ">", "<=", ">=":
		if width == 0 {
			return nil, false, false
		}
	default:
		return left, isCond, true
	}
	l.pos += width
	right, rightCond, ok := l.parseAdditive()
	if !ok || isCond || rightCond {
		return nil, false, false
	}
	return LocalBinary{Op: op, L: left, R: right}, true, true
}

func (l *localParser) parseAdditive() (LocalNode, bool, bool) {
	left, isCond, ok := l.parseUnary()
	if !ok {
		return nil, false, false
	}
	for {
		op, width := l.operator()
		if width != 1 || op != "+" && op != "-" {
			return left, isCond, true
		}
		l.pos++
		right, rightCond, ok := l.parseUnary()
		if !ok || isCond || rightCond {
			return nil, false, false
		}
		left = LocalBinary{Op: op, L: left, R: right}
	}
}

// parseUnary accepts a minus sign only before an integer literal.
func (l *localParser) parseUnary() (LocalNode, bool, bool) {
	if op, width := l.operator(); op == "-" && width == 1 {
		l.pos++
		if l.pos >= len(l.toks) || l.toks[l.pos].kind != tokenNumber {
			return nil, false, false
		}
		value, ok := l.integer()
		if !ok {
			return nil, false, false
		}
		return LocalInt{Value: -value}, false, true
	}
	return l.parsePrimary()
}

func (l *localParser) integer() (int64, bool) {
	t := l.toks[l.pos]
	value, err := strconv.ParseInt(t.text, 10, 64)
	if err != nil || value > maxInteger {
		return 0, false
	}
	l.pos++
	return value, true
}

func (l *localParser) parsePrimary() (LocalNode, bool, bool) {
	if l.pos >= len(l.toks) {
		return nil, false, false
	}
	t := l.toks[l.pos]
	if ref, ok := l.refs[l.pos]; ok {
		l.pos = ref.last + 1
		return LocalVar{Name: ref.name}, false, true
	}
	switch {
	case t.kind == tokenNumber:
		value, ok := l.integer()
		if !ok {
			return nil, false, false
		}
		return LocalInt{Value: value}, false, true
	case t.kind == tokenString:
		if !stringTerminated(l.text, t) {
			return nil, false, false
		}
		l.pos++
		return LocalString{Value: []byte(t.text)}, false, true
	case keyword(t, "NULL"):
		l.pos++
		return LocalNull{}, false, true
	case t.kind == tokenPunctuation && t.text == "(":
		l.pos++
		inner, isCond, ok := l.parseOr()
		if !ok || l.pos >= len(l.toks) || l.toks[l.pos].kind != tokenPunctuation || l.toks[l.pos].text != ")" {
			return nil, false, false
		}
		l.pos++
		return inner, isCond, true
	}
	return nil, false, false
}

// stringTerminated reports whether string token t ends with its closing
// delimiter rather than at the end of the input.
func stringTerminated(input string, t token) bool {
	delimiter := input[t.start]
	for i := t.start + 1; i < t.end; i++ {
		if input[i] != delimiter {
			continue
		}
		if i+1 < t.end && input[i+1] == delimiter {
			i++
			continue
		}
		return i+1 == t.end
	}
	return false
}
