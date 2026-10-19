package psql

import (
	"errors"
	"fmt"
)

// EntryCall is a supported entry statement (spec 4).
type EntryCall struct {
	Procedure  string
	Quoted     bool
	Selectable bool     // SELECT … FROM P(...)
	Bare       bool     // SELECT … FROM P without parentheses: P may also be a table or view
	Columns    []string // nil for "*"
	Args       []string // argument SQL texts
}

// ParseEntryCall parses the statement a line profile starts from: one
// EXECUTE PROCEDURE p [(args) | args] or SELECT * | columns FROM p [(args)]
// with no other clause. A bare FROM p is reported with Bare set; only the
// catalog can tell a parameterless procedure from a table or view. Arguments must be constant: no parameters, column
// references, subqueries or GEN_ID. The error explains why a statement is
// not a supported entry.
func ParseEntryCall(sql string, dialect int) (EntryCall, error) {
	if dialect == 0 {
		dialect = 3
	}
	tokens := lexDialect(sql, dialect)
	end := len(tokens)
	if end > 0 && isTerminator(tokens[end-1]) {
		end--
	}
	if end == 0 {
		return EntryCall{}, errors.New("the entry statement is empty")
	}
	for i := 0; i < end; i++ {
		if isTerminator(tokens[i]) {
			return EntryCall{}, errors.New("the entry statement must be exactly one statement")
		}
	}
	for i := 0; i < end; i++ {
		if keyword(tokens[i], "RETURNING_VALUES") {
			return EntryCall{}, errors.New("RETURNING_VALUES is not supported in an entry statement")
		}
	}

	var call EntryCall
	var argStart, argEnd int
	switch {
	case keyword(tokens[0], "EXECUTE") && end > 1 && keyword(tokens[1], "PROCEDURE"):
		if end < 3 || !isNameToken(tokens[2]) {
			return EntryCall{}, errors.New("EXECUTE PROCEDURE without a procedure name")
		}
		call.Procedure, call.Quoted = normalizedName(tokens[2]), tokens[2].kind == tokenQuotedIdentifier
		argStart, argEnd = 3, end
		if argStart < end && isPunct(tokens[argStart], "(") && matchingClose(tokens[:end], argStart) == end-1 {
			argStart, argEnd = argStart+1, end-1
		}
	case keyword(tokens[0], "SELECT"):
		call.Selectable = true
		i := 1
		if i < end && isPunct(tokens[i], "*") {
			i++
		} else {
			call.Columns = []string{}
			for {
				if i >= end || !isNameToken(tokens[i]) || keyword(tokens[i], "FROM") ||
					i+1 < end && !keyword(tokens[i+1], "FROM") && !isPunct(tokens[i+1], ",") {
					return EntryCall{}, errors.New("the output column list must be * or plain column names")
				}
				call.Columns = append(call.Columns, normalizedName(tokens[i]))
				i++
				if i >= end || !isPunct(tokens[i], ",") {
					break
				}
				i++
			}
		}
		if i >= end || !keyword(tokens[i], "FROM") || i+1 >= end || !isNameToken(tokens[i+1]) {
			return EntryCall{}, errors.New("expected FROM followed by a procedure name")
		}
		name := tokens[i+1]
		call.Procedure, call.Quoted = normalizedName(name), name.kind == tokenQuotedIdentifier
		open := i + 2
		if open >= end || !isPunct(tokens[open], "(") {
			if open < end {
				return EntryCall{}, fmt.Errorf("unexpected %s after the procedure name; only FROM P(args) is allowed", asciiUpper(tokens[open].text))
			}
			call.Bare = true
			return call, nil
		}
		closeIndex := matchingClose(tokens[:end], open)
		if closeIndex < 0 {
			return EntryCall{}, errors.New("unbalanced parentheses in the argument list")
		}
		if closeIndex+1 < end {
			return EntryCall{}, fmt.Errorf("unexpected %s after the procedure call; no other clause is allowed", asciiUpper(tokens[closeIndex+1].text))
		}
		argStart, argEnd = open+1, closeIndex
	default:
		return EntryCall{}, errors.New("only EXECUTE PROCEDURE p … or SELECT … FROM p(…) can be profiled")
	}

	pieces, ok := splitArgs(tokens, argStart, argEnd)
	if !ok {
		return EntryCall{}, errors.New("malformed argument list: empty argument or unbalanced parentheses")
	}
	for n, piece := range pieces {
		if err := checkConstantArg(tokens, piece[0], piece[1]); err != nil {
			return EntryCall{}, fmt.Errorf("argument %d %v", n+1, err)
		}
		call.Args = append(call.Args, sql[tokens[piece[0]].start:tokens[piece[1]-1].end])
	}
	return call, nil
}

func isPunct(t token, value string) bool {
	return t.kind == tokenPunctuation && t.text == value
}

func isTerminator(t token) bool {
	return isPunct(t, ";") && t.depth == 0
}

// splitArgs splits tokens [start, end) at top-level commas into
// [start, end) pieces. ok is false for an empty piece or unbalanced
// parentheses.
func splitArgs(tokens []token, start, end int) (pieces [][2]int, ok bool) {
	if start >= end {
		return nil, true
	}
	base, open := tokens[start].depth, 0
	pieceStart := start
	for i := start; i < end; i++ {
		t := tokens[i]
		switch {
		case isPunct(t, "("):
			open++
		case isPunct(t, ")"):
			if open--; open < 0 || t.depth != base+open+1 {
				return nil, false
			}
		case isPunct(t, ",") && open == 0:
			if i == pieceStart {
				return nil, false
			}
			pieces = append(pieces, [2]int{pieceStart, i})
			pieceStart = i + 1
		}
	}
	if open != 0 || pieceStart >= end {
		return nil, false
	}
	return append(pieces, [2]int{pieceStart, end}), true
}

var constantKeywords = map[string]bool{
	"NULL": true, "CURRENT_DATE": true, "CURRENT_TIME": true, "CURRENT_TIMESTAMP": true,
	"USER": true, "CURRENT_USER": true, "CURRENT_ROLE": true,
}

// operatorKeywords are words that join or follow operands, so they never
// start or end an operand themselves.
var operatorKeywords = map[string]bool{
	"AND": true, "OR": true, "NOT": true, "IS": true, "IN": true, "LIKE": true,
	"BETWEEN": true, "AS": true, "COLLATE": true, "CONTAINING": true, "STARTING": true,
	"ESCAPE": true, "SIMILAR": true,
}

// operandEnd reports whether t can end a value; operandStart whether it can
// begin one.
func operandEnd(t token) bool {
	return t.kind == tokenNumber || t.kind == tokenString || t.kind == tokenQuotedIdentifier ||
		isPunct(t, ")") || operandStart(t)
}

func operandStart(t token) bool {
	return t.kind == tokenNumber || t.kind == tokenString || t.kind == tokenQuotedIdentifier ||
		t.kind == tokenIdentifier && !operatorKeywords[asciiUpper(t.text)]
}

// checkConstantArg rejects arguments that are not constant expressions.
func checkConstantArg(tokens []token, start, end int) error {
	castDepth := -1
	for i := start; i < end; i++ {
		t := tokens[i]
		if castDepth >= 0 && isPunct(t, ")") && t.depth == castDepth {
			castDepth = -1
		}
		if castDepth < 0 && i > start && operandEnd(tokens[i-1]) && operandStart(t) {
			return errors.New("has two values with no comma between them")
		}
		switch {
		case t.kind == tokenParameter || isPunct(t, "?") || isPunct(t, ":"):
			return errors.New("uses a parameter; use literal values")
		case t.kind == tokenQuotedIdentifier:
			return fmt.Errorf("references a column or variable %q", t.text)
		case t.kind != tokenIdentifier || castDepth >= 0:
		case keyword(t, "SELECT"):
			return errors.New("contains a subquery (SELECT)")
		case keyword(t, "GEN_ID"):
			return errors.New("calls GEN_ID, which changes a generator")
		case keyword(t, "AS") && t.depth > tokens[start].depth:
			castDepth = t.depth
		case i+1 < end && isPunct(tokens[i+1], "(") && (i == start || !isPunct(tokens[i-1], ".")):
		case constantKeywords[asciiUpper(t.text)] && !(i+1 < end && isPunct(tokens[i+1], ".")):
		default:
			return fmt.Errorf("references a column or variable %s", asciiUpper(t.text))
		}
	}
	return nil
}
