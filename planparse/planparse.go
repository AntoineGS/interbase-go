// Package planparse parses InterBase execution-plan text into a tree.
package planparse

import (
	"fmt"
	"strings"
)

// Node is one element of an InterBase plan. Kind is "join", "sort", "merge",
// "group" (a parenthesized list with no operator) or "table".
type Node struct {
	Kind       string   // "join", "sort", "merge", "group" or "table".
	Children   []*Node  // Child plan elements, in plan order.
	Labels     []string // Table alias chain as printed, e.g. ["V", "A"].
	Relation   string   // Filled by interbase.DescribePlan; empty here.
	Access     string   // Table access: "natural", "index" or "order".
	Indexes    []string // INDEX (...) names; also ORDER x INDEX (...) names.
	OrderIndex string   // ORDER name.
}

// Tree contains the root expressions in an InterBase plan.
type Tree struct{ Roots []*Node }

// Parse parses raw InterBase plan text. Whitespace-only input returns an empty
// tree and nil. Unsupported or malformed input returns a non-nil error and a
// nil tree.
func Parse(raw string) (*Tree, error) {
	tokens, err := tokenize(raw)
	if err != nil {
		return nil, err
	}
	if len(tokens) == 0 {
		return &Tree{}, nil
	}

	p := parser{tokens: tokens}
	if !p.keyword("PLAN") {
		return nil, fmt.Errorf("plan must begin with PLAN")
	}
	p.pos++

	roots := make([]*Node, 0, 1)
	for {
		root, err := p.parseExpr()
		if err != nil {
			return nil, err
		}
		roots = append(roots, root)

		if p.done() {
			break
		}
		if p.keyword("PLAN") {
			p.pos++
		}
	}
	return &Tree{Roots: roots}, nil
}

type tokenKind uint8

const (
	tokenIdentifier tokenKind = iota
	tokenLeftParen
	tokenRightParen
	tokenComma
)

type token struct {
	kind   tokenKind
	text   string
	quoted bool
}

func tokenize(input string) ([]token, error) {
	tokens := make([]token, 0)
	for i := 0; i < len(input); {
		if isASCIIWhitespace(input[i]) {
			i++
			continue
		}

		switch input[i] {
		case '(':
			tokens = append(tokens, token{kind: tokenLeftParen, text: "("})
			i++
		case ')':
			tokens = append(tokens, token{kind: tokenRightParen, text: ")"})
			i++
		case ',':
			tokens = append(tokens, token{kind: tokenComma, text: ","})
			i++
		case '"':
			value, next, err := quotedIdentifier(input, i+1)
			if err != nil {
				return nil, err
			}
			tokens = append(tokens, token{kind: tokenIdentifier, text: value, quoted: true})
			i = next
		default:
			if !identifierStart(input[i]) {
				return nil, fmt.Errorf("unexpected byte 0x%02x at byte %d", input[i], i)
			}
			start := i
			for i < len(input) && identifierContinue(input[i]) {
				i++
			}
			tokens = append(tokens, token{kind: tokenIdentifier, text: input[start:i]})
		}
	}
	return tokens, nil
}

func quotedIdentifier(input string, i int) (string, int, error) {
	var value strings.Builder
	segment := i
	for i < len(input) {
		if input[i] == '"' {
			value.WriteString(input[segment:i])
			if i+1 < len(input) && input[i+1] == '"' {
				value.WriteByte('"')
				i += 2
				segment = i
				continue
			}
			return value.String(), i + 1, nil
		}
		i++
	}
	return "", 0, fmt.Errorf("unterminated quoted identifier")
}

func identifierStart(b byte) bool {
	return b >= 0x80 || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z' || b == '_' || b == '$'
}

func identifierContinue(b byte) bool {
	return identifierStart(b) || b >= '0' && b <= '9'
}

func isASCIIWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

type parser struct {
	tokens []token
	pos    int
}

func (p *parser) done() bool {
	return p.pos >= len(p.tokens)
}

func (p *parser) current() token {
	if p.done() {
		return token{}
	}
	return p.tokens[p.pos]
}

func (p *parser) keyword(value string) bool {
	return p.keywordAt(p.pos, value)
}

func (p *parser) keywordAt(pos int, value string) bool {
	if pos >= len(p.tokens) {
		return false
	}
	t := p.tokens[pos]
	return t.kind == tokenIdentifier && !t.quoted && asciiEqualFold(t.text, value)
}

func asciiEqualFold(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := 0; i < len(left); i++ {
		l, r := left[i], right[i]
		if l >= 'a' && l <= 'z' {
			l -= 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' {
			r -= 'a' - 'A'
		}
		if l != r {
			return false
		}
	}
	return true
}

func (p *parser) parseExpr() (*Node, error) {
	if p.done() {
		return nil, fmt.Errorf("expected plan expression")
	}
	if p.current().kind == tokenLeftParen {
		return p.parseGroup()
	}
	if p.isOperator(p.pos) && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == tokenLeftParen {
		return p.parseOperator()
	}
	return nil, fmt.Errorf("expected plan expression at token %q", p.current().text)
}

func (p *parser) parseGroup() (*Node, error) {
	p.pos++ // opening parenthesis
	children, err := p.parseList()
	if err != nil {
		return nil, err
	}
	return &Node{Kind: "group", Children: children}, nil
}

func (p *parser) parseOperator() (*Node, error) {
	kind := strings.ToLower(p.current().text)
	p.pos++
	p.pos++ // opening parenthesis, checked by parseExpr/parseElement
	children, err := p.parseList()
	if err != nil {
		return nil, err
	}
	return &Node{Kind: kind, Children: children}, nil
}

func (p *parser) isOperator(pos int) bool {
	return p.keywordAt(pos, "JOIN") || p.keywordAt(pos, "SORT") || p.keywordAt(pos, "MERGE")
}

func (p *parser) parseList() ([]*Node, error) {
	if p.done() || p.current().kind == tokenRightParen {
		return nil, fmt.Errorf("expected a non-empty plan list")
	}
	children := make([]*Node, 0, 2)
	for {
		child, err := p.parseElement()
		if err != nil {
			return nil, err
		}
		children = append(children, child)
		for p.keyword("PLAN") {
			p.pos++
			child, err = p.parseExpr()
			if err != nil {
				return nil, err
			}
			children = append(children, child)
		}

		if p.done() {
			return nil, fmt.Errorf("unterminated plan list")
		}
		switch p.current().kind {
		case tokenComma:
			p.pos++
			if p.done() || p.current().kind == tokenRightParen {
				return nil, fmt.Errorf("expected plan element after comma")
			}
		case tokenRightParen:
			p.pos++
			return children, nil
		default:
			return nil, fmt.Errorf("expected comma or closing parenthesis at token %q", p.current().text)
		}
	}
}

func (p *parser) parseElement() (*Node, error) {
	if p.done() {
		return nil, fmt.Errorf("expected plan element")
	}
	if p.current().kind == tokenLeftParen {
		return p.parseGroup()
	}
	if p.isOperator(p.pos) && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == tokenLeftParen {
		return p.parseOperator()
	}
	return p.parseTable()
}

func (p *parser) parseTable() (*Node, error) {
	if p.done() || p.current().kind != tokenIdentifier || p.current().text == "" {
		return nil, fmt.Errorf("expected table label")
	}
	labels := []string{p.current().text}
	p.pos++
	for !p.done() && p.current().kind == tokenIdentifier && !p.isAccess(p.pos) {
		if p.current().text == "" {
			return nil, fmt.Errorf("table label cannot be empty")
		}
		labels = append(labels, p.current().text)
		p.pos++
	}
	if p.done() || !p.isAccess(p.pos) {
		if p.done() {
			return nil, fmt.Errorf("table %q is missing an access method", strings.Join(labels, " "))
		}
		return nil, fmt.Errorf("table %q is missing an access method before token %q", strings.Join(labels, " "), p.current().text)
	}

	node := &Node{Kind: "table", Labels: labels}
	switch {
	case p.keyword("NATURAL"):
		node.Access = "natural"
		p.pos++
	case p.keyword("INDEX"):
		node.Access = "index"
		p.pos++
		indexes, err := p.parseIndexes()
		if err != nil {
			return nil, err
		}
		node.Indexes = indexes
	case p.keyword("ORDER"):
		node.Access = "order"
		p.pos++
		if p.done() || p.current().kind != tokenIdentifier || p.current().text == "" {
			return nil, fmt.Errorf("ORDER access requires an index name")
		}
		node.OrderIndex = p.current().text
		p.pos++
		if p.keyword("INDEX") && p.pos+1 < len(p.tokens) && p.tokens[p.pos+1].kind == tokenLeftParen {
			p.pos++
			indexes, err := p.parseIndexes()
			if err != nil {
				return nil, err
			}
			node.Indexes = indexes
		}
	}
	return node, nil
}

func (p *parser) isAccess(pos int) bool {
	return p.keywordAt(pos, "NATURAL") || p.keywordAt(pos, "INDEX") || p.keywordAt(pos, "ORDER")
}

func (p *parser) parseIndexes() ([]string, error) {
	if p.done() || p.current().kind != tokenLeftParen {
		return nil, fmt.Errorf("expected parenthesized index names")
	}
	p.pos++
	if p.done() || p.current().kind == tokenRightParen {
		return nil, fmt.Errorf("expected at least one index name")
	}
	indexes := make([]string, 0, 1)
	for {
		if p.done() || p.current().kind != tokenIdentifier {
			return nil, fmt.Errorf("expected index name")
		}
		indexes = append(indexes, p.current().text)
		p.pos++
		if p.done() {
			return nil, fmt.Errorf("unterminated index list")
		}
		switch p.current().kind {
		case tokenComma:
			p.pos++
			if p.done() || p.current().kind == tokenRightParen {
				return nil, fmt.Errorf("expected index name after comma")
			}
		case tokenRightParen:
			p.pos++
			return indexes, nil
		default:
			return nil, fmt.Errorf("expected comma or closing parenthesis after index name")
		}
	}
}
