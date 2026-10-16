// Package planparse parses InterBase execution-plan text into a tree.
package planparse

import (
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
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
		r, size := utf8.DecodeRuneInString(input[i:])
		if r == utf8.RuneError && size == 1 {
			return nil, fmt.Errorf("invalid UTF-8 at byte %d", i)
		}
		if unicode.IsSpace(r) {
			i += size
			continue
		}

		switch r {
		case '(':
			tokens = append(tokens, token{kind: tokenLeftParen, text: "("})
			i += size
		case ')':
			tokens = append(tokens, token{kind: tokenRightParen, text: ")"})
			i += size
		case ',':
			tokens = append(tokens, token{kind: tokenComma, text: ","})
			i += size
		case '"':
			value, next, err := quotedIdentifier(input, i+size)
			if err != nil {
				return nil, err
			}
			if value == "" {
				return nil, fmt.Errorf("empty quoted identifier at byte %d", i)
			}
			tokens = append(tokens, token{kind: tokenIdentifier, text: value, quoted: true})
			i = next
		default:
			if !identifierStart(r) {
				return nil, fmt.Errorf("unexpected character %q at byte %d", r, i)
			}
			start := i
			i += size
			for i < len(input) {
				next, nextSize := utf8.DecodeRuneInString(input[i:])
				if next == utf8.RuneError && nextSize == 1 {
					return nil, fmt.Errorf("invalid UTF-8 at byte %d", i)
				}
				if !identifierContinue(next) {
					break
				}
				i += nextSize
			}
			tokens = append(tokens, token{kind: tokenIdentifier, text: input[start:i]})
		}
	}
	return tokens, nil
}

func quotedIdentifier(input string, i int) (string, int, error) {
	var value strings.Builder
	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])
		if r == utf8.RuneError && size == 1 {
			return "", 0, fmt.Errorf("invalid UTF-8 in quoted identifier at byte %d", i)
		}
		if r == '"' {
			if i+size < len(input) && input[i+size] == '"' {
				value.WriteByte('"')
				i += size + 1
				continue
			}
			return value.String(), i + size, nil
		}
		value.WriteRune(r)
		i += size
	}
	return "", 0, fmt.Errorf("unterminated quoted identifier")
}

func identifierStart(r rune) bool {
	return r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r == '_' || r == '$'
}

func identifierContinue(r rune) bool {
	return identifierStart(r) || r >= '0' && r <= '9'
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
	return t.kind == tokenIdentifier && !t.quoted && strings.EqualFold(t.text, value)
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
	if p.done() || p.current().kind != tokenIdentifier {
		return nil, fmt.Errorf("expected table label")
	}
	labels := []string{p.current().text}
	p.pos++
	for !p.done() && p.current().kind == tokenIdentifier && !p.isAccess(p.pos) {
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
		if p.done() || p.current().kind != tokenIdentifier {
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
