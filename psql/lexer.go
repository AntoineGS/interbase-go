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
	kind   tokenKind
	text   string
	start  int // byte offset
	end    int
	line   int // 1-based
	column int // 1-based byte column within line
	depth  int
}

func lex(input string) []token {
	return lexDialect(input, 3)
}

// lexDialect tokenizes input; in SQL dialect 1 double-quoted text is a string
// literal rather than a quoted identifier.
func lexDialect(input string, dialect int) []token {
	tokens := make([]token, 0)
	line, lineStart, depth := 1, 0, 0
	quotedKind := tokenQuotedIdentifier
	if dialect == 1 {
		quotedKind = tokenString
	}

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
			line, lineStart = countLines(input, start, i, line, lineStart)
			continue
		}

		if isASCIIWhitespace(input[i]) {
			start := i
			i++
			line, lineStart = countLines(input, start, i, line, lineStart)
			continue
		}

		start, tokenLine, tokenColumn := i, line, i-lineStart+1
		add := func(kind tokenKind, text string, end, depth int) {
			tokens = append(tokens, token{kind: kind, text: text, start: start, end: end, line: tokenLine, column: tokenColumn, depth: depth})
		}
		switch input[i] {
		case '\'':
			text, next := readDelimited(input, i, '\'')
			line, lineStart = countLines(input, i, next, line, lineStart)
			add(tokenString, text, next, depth)
			i = next
		case '"':
			text, next := readDelimited(input, i, '"')
			line, lineStart = countLines(input, i, next, line, lineStart)
			add(quotedKind, text, next, depth)
			i = next
		case ':':
			if i+1 < len(input) && isIdentifierStartAt(input, i+1) {
				i++
				nameStart := i
				i = scanIdentifier(input, i)
				add(tokenParameter, input[nameStart:i], i, depth)
			} else {
				i++
				add(tokenPunctuation, input[start:i], i, depth)
			}
		case '(':
			i++
			add(tokenPunctuation, "(", i, depth)
			depth++
		case ')':
			i++
			add(tokenPunctuation, ")", i, depth)
			if depth > 0 {
				depth--
			}
		default:
			if isIdentifierStartAt(input, i) {
				i = scanIdentifier(input, i)
				add(tokenIdentifier, input[start:i], i, depth)
			} else if input[i] >= '0' && input[i] <= '9' {
				i++
				for i < len(input) && input[i] >= '0' && input[i] <= '9' {
					i++
				}
				add(tokenNumber, input[start:i], i, depth)
			} else {
				i++
				add(tokenPunctuation, input[start:i], i, depth)
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

// NormalizeName normalizes a caller-supplied object name as a name in PSQL
// text is normalized: upper-cased unless double-quoted, in which case the
// quotes are removed and the name kept exactly.
func NormalizeName(name string) string {
	name = strings.TrimSpace(name)
	if tokens := lex(name); len(tokens) == 1 && isNameToken(tokens[0]) {
		return normalizedName(tokens[0])
	}
	return asciiUpper(name)
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

// countLines advances line and the byte offset where it starts over
// input[start:end]; CRLF, LF and a lone CR each end a line.
func countLines(input string, start, end, line, lineStart int) (int, int) {
	for i := start; i < end; i++ {
		switch input[i] {
		case '\n':
			line++
			lineStart = i + 1
		case '\r':
			if i+1 >= len(input) || input[i+1] != '\n' {
				line++
				lineStart = i + 1
			}
		}
	}
	return line, lineStart
}
