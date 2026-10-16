package psql

import (
	"strings"
	"unicode"
	"unicode/utf8"
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

		r, size := utf8.DecodeRuneInString(input[i:])
		if unicode.IsSpace(r) {
			start := i
			i += size
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
				i += size
				if size == 0 {
					i++
				}
				tokens = append(tokens, token{kind: tokenPunctuation, text: input[start:i], start: start, end: i, line: tokenLine, depth: depth})
			}
		}
	}
	return tokens
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
		_, size := utf8.DecodeRuneInString(input[i:])
		if size == 0 {
			break
		}
		value.WriteString(input[i : i+size])
		i += size
	}
	return value.String(), len(input)
}

func scanIdentifier(input string, start int) int {
	i := start
	for i < len(input) {
		r, size := utf8.DecodeRuneInString(input[i:])
		if size == 0 || !isIdentifierContinue(r) {
			break
		}
		i += size
	}
	return i
}

func isIdentifierStartAt(input string, i int) bool {
	if i >= len(input) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(input[i:])
	return r == '_' || r == '$' || unicode.IsLetter(r)
}

func isIdentifierContinue(r rune) bool {
	return r == '_' || r == '$' || unicode.IsLetter(r) || unicode.IsDigit(r)
}

func isNameToken(t token) bool {
	return t.kind == tokenIdentifier || t.kind == tokenQuotedIdentifier
}

func keyword(t token, value string) bool {
	return t.kind == tokenIdentifier && strings.EqualFold(t.text, value)
}

func normalizedName(t token) string {
	if t.kind == tokenQuotedIdentifier {
		return t.text
	}
	return strings.ToUpper(t.text)
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
