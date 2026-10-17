package psql

import (
	"strings"
)

type tokenKind uint8

const (
	tokenIdentifier tokenKind = iota
	tokenQuotedIdentifier
	tokenString
	tokenNumber
	tokenParameter
	tokenPunctuation
)

type token struct {
	kind  tokenKind
	text  string
	start int
	end   int
	line  int
	depth int
}

func lex(input string) []token {
	tokens := make([]token, 0)
	line, depth := 1, 0

	for i := 0; i < len(input); {
		if input[i] == '-' && i+1 < len(input) && input[i+1] == '-' {
			i += 2
			for i < len(input) && input[i] != '\n' && input[i] != '\r' {
				i++
			}
			continue
		}
		if input[i] == '/' && i+1 < len(input) && input[i+1] == '*' {
			start := i
			i += 2
			for i+1 < len(input) && !(input[i] == '*' && input[i+1] == '/') {
				i++
			}
			if i+1 < len(input) {
				i += 2
			} else {
				i = len(input)
			}
			line = countLines(input, start, i, line)
			continue
		}

		if isASCIIWhitespace(input[i]) {
			start := i
			i++
			line = countLines(input, start, i, line)
			continue
		}

		start, tokenLine := i, line
		switch input[i] {
		case '\'':
			text, next := readDelimited(input, i, '\'')
			line = countLines(input, i, next, line)
			tokens = append(tokens, token{kind: tokenString, text: text, start: i, end: next, line: tokenLine, depth: depth})
			i = next
		case '"':
			text, next := readDelimited(input, i, '"')
			line = countLines(input, i, next, line)
			tokens = append(tokens, token{kind: tokenQuotedIdentifier, text: text, start: i, end: next, line: tokenLine, depth: depth})
			i = next
		case ':':
			if i+1 < len(input) && isIdentifierStartAt(input, i+1) {
				i++
				nameStart := i
				i = scanIdentifier(input, i)
				tokens = append(tokens, token{kind: tokenParameter, text: input[nameStart:i], start: start, end: i, line: tokenLine, depth: depth})
			} else {
				i++
				tokens = append(tokens, token{kind: tokenPunctuation, text: input[start:i], start: start, end: i, line: tokenLine, depth: depth})
			}
		case '(':
			i++
			tokens = append(tokens, token{kind: tokenPunctuation, text: "(", start: start, end: i, line: tokenLine, depth: depth})
			depth++
		case ')':
			tokenDepth := depth
			i++
			tokens = append(tokens, token{kind: tokenPunctuation, text: ")", start: start, end: i, line: tokenLine, depth: tokenDepth})
			if depth > 0 {
				depth--
			}
		default:
			if isIdentifierStartAt(input, i) {
				i = scanIdentifier(input, i)
				tokens = append(tokens, token{kind: tokenIdentifier, text: input[start:i], start: start, end: i, line: tokenLine, depth: depth})
			} else if input[i] >= '0' && input[i] <= '9' {
				i++
				for i < len(input) && input[i] >= '0' && input[i] <= '9' {
					i++
				}
				tokens = append(tokens, token{kind: tokenNumber, text: input[start:i], start: start, end: i, line: tokenLine, depth: depth})
			} else {
				i++
				tokens = append(tokens, token{kind: tokenPunctuation, text: input[start:i], start: start, end: i, line: tokenLine, depth: depth})
			}
		}
	}
	return tokens
}

// QuotedIdentifiers returns the distinct contents of quoted identifiers in SQL.
func QuotedIdentifiers(input string) []string {
	seen := make(map[string]struct{})
	var identifiers []string
	for _, token := range lex(input) {
		if token.kind != tokenQuotedIdentifier {
			continue
		}
		if _, ok := seen[token.text]; ok {
			continue
		}
		seen[token.text] = struct{}{}
		identifiers = append(identifiers, token.text)
	}
	return identifiers
}

func readDelimited(input string, start int, delimiter byte) (string, int) {
	var value strings.Builder
	for i := start + 1; i < len(input); {
		if input[i] == delimiter {
			if i+1 < len(input) && input[i+1] == delimiter {
				value.WriteByte(delimiter)
				i += 2
				continue
			}
			return value.String(), i + 1
		}
		value.WriteByte(input[i])
		i++
	}
	return value.String(), len(input)
}

func scanIdentifier(input string, start int) int {
	i := start
	for i < len(input) && isIdentifierContinue(input[i]) {
		i++
	}
	return i
}

func isIdentifierStartAt(input string, i int) bool {
	if i >= len(input) {
		return false
	}
	return isIdentifierStartByte(input[i])
}

func isIdentifierContinue(b byte) bool {
	return isIdentifierStartByte(b) || b >= '0' && b <= '9'
}

func isIdentifierStartByte(b byte) bool {
	return b >= 0x80 || b == '_' || b == '$' || b >= 'A' && b <= 'Z' || b >= 'a' && b <= 'z'
}

func isASCIIWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r', '\v', '\f':
		return true
	default:
		return false
	}
}

func isNameToken(t token) bool {
	return t.kind == tokenIdentifier || t.kind == tokenQuotedIdentifier
}

func keyword(t token, value string) bool {
	return t.kind == tokenIdentifier && asciiEqualFold(t.text, value)
}

func normalizedName(t token) string {
	if t.kind == tokenQuotedIdentifier {
		return t.text
	}
	return asciiUpper(t.text)
}

func asciiUpper(value string) string {
	bytes := []byte(value)
	for i, b := range bytes {
		if b >= 'a' && b <= 'z' {
			bytes[i] = b - ('a' - 'A')
		}
	}
	return string(bytes)
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

func countLines(input string, start, end, line int) int {
	for i := start; i < end; i++ {
		switch input[i] {
		case '\n':
			line++
		case '\r':
			if i+1 >= len(input) || input[i+1] != '\n' {
				line++
			}
		}
	}
	return line
}
