// Package psql extracts data statements and relation references from
// InterBase procedural SQL text.
package psql

import (
	"sort"
	"strings"
	"unicode"
)

// Statement is one data statement found in a PSQL body or DSQL text.
type Statement struct {
	Line      int
	Kind      string
	SQL       string
	Target    string
	Procedure string
	Sources   []string
	Calls     []string
}

// Extract finds standalone data statements in a PSQL body. It does not
// interpret procedural control flow; only SQL statement starts are returned.
func Extract(body string) []Statement {
	tokens := lex(body)
	statements := make([]Statement, 0)
	for i := 0; i < len(tokens); i++ {
		kind, sqlStart, procedure, ok := statementStart(tokens, i, true)
		if !ok {
			continue
		}

		end := statementEnd(tokens, sqlStart, kind)
		statement := Statement{Line: tokens[i].line, Kind: kind}
		if kind == "executeProcedure" {
			statement.Procedure = procedure
		} else if kind != "executeStatement" {
			endOffset := tokenRangeEnd(tokens, end, len(body))
			statement.SQL = rewriteSQL(body, tokens, sqlStart, end, endOffset)
			statement.Target = statementTarget(tokens, i, end, kind)
			statement.Sources, statement.Calls = collectSources(tokens, sqlStart, end, kind == "delete")
		}
		statements = append(statements, statement)
		i = end
	}
	return statements
}

// ExtractStatementRefs describes a single DSQL statement without rewriting
// its SQL field.
func ExtractStatementRefs(sql string) Statement {
	tokens := lex(sql)
	for i := range tokens {
		kind, sqlStart, procedure, ok := statementStart(tokens, i, i == firstToken(tokens))
		if !ok {
			continue
		}
		end := statementEnd(tokens, sqlStart, kind)
		statement := Statement{Line: tokens[i].line, Kind: kind, SQL: sql}
		if kind == "executeProcedure" {
			statement.Procedure = procedure
		} else if kind != "executeStatement" {
			statement.Target = statementTarget(tokens, i, end, kind)
			statement.Sources, statement.Calls = collectSources(tokens, sqlStart, end, kind == "delete")
		}
		return statement
	}
	return Statement{SQL: sql}
}

func firstToken(tokens []token) int {
	if len(tokens) == 0 {
		return -1
	}
	return 0
}

func statementStart(tokens []token, i int, allowFirst bool) (kind string, sqlStart int, procedure string, ok bool) {
	if i < 0 || i >= len(tokens) || tokens[i].depth != 0 || tokens[i].kind != tokenIdentifier {
		return "", 0, "", false
	}

	previousBoundary := i == 0 && allowFirst
	if i > 0 {
		previousBoundary = isStatementBoundary(tokens[i-1])
	}
	if !previousBoundary {
		return "", 0, "", false
	}

	switch {
	case keyword(tokens[i], "SELECT"):
		if i > 0 && keyword(tokens[i-1], "FOR") {
			return "", 0, "", false
		}
		return "select", i, "", true
	case keyword(tokens[i], "INSERT"):
		return "insert", i, "", true
	case keyword(tokens[i], "UPDATE"):
		return "update", i, "", true
	case keyword(tokens[i], "DELETE"):
		return "delete", i, "", true
	case keyword(tokens[i], "FOR"):
		if i+1 < len(tokens) && keyword(tokens[i+1], "SELECT") && tokens[i+1].depth == 0 {
			return "forSelect", i + 1, "", true
		}
	case keyword(tokens[i], "EXECUTE"):
		if i+1 >= len(tokens) {
			return "", 0, "", false
		}
		switch {
		case keyword(tokens[i+1], "PROCEDURE"):
			if i+2 < len(tokens) && isNameToken(tokens[i+2]) {
				return "executeProcedure", i, normalizedName(tokens[i+2]), true
			}
		case keyword(tokens[i+1], "STATEMENT"):
			return "executeStatement", i, "", true
		}
	}
	return "", 0, "", false
}

func isStatementBoundary(t token) bool {
	if t.kind == tokenPunctuation && t.text == ";" && t.depth == 0 {
		return true
	}
	if t.kind != tokenIdentifier || t.depth != 0 {
		return false
	}
	switch strings.ToUpper(t.text) {
	case "BEGIN", "THEN", "ELSE", "DO", "END", "AS":
		return true
	default:
		return false
	}
}

// statementEnd returns the exclusive token index. A semicolon or FOR SELECT's
// DO token is excluded from the standalone SQL.
func statementEnd(tokens []token, start int, kind string) int {
	for i := start; i < len(tokens); i++ {
		if tokens[i].depth != 0 {
			continue
		}
		if tokens[i].kind == tokenPunctuation && tokens[i].text == ";" {
			return i
		}
		if kind == "forSelect" && keyword(tokens[i], "DO") {
			return i
		}
	}
	return len(tokens)
}

func tokenRangeEnd(tokens []token, end, inputLength int) int {
	if end < len(tokens) {
		return tokens[end].start
	}
	if len(tokens) == 0 {
		return inputLength
	}
	return tokens[len(tokens)-1].end
}

type textEdit struct {
	start       int
	end         int
	replacement string
}

func rewriteSQL(input string, tokens []token, start, end, endOffset int) string {
	startOffset := tokens[start].start
	if endOffset < startOffset {
		return ""
	}
	edits := make([]textEdit, 0)
	removedStart, removedEnd := -1, -1

	for i := start; i < end; i++ {
		if tokens[i].depth != 0 || !keyword(tokens[i], "INTO") || i+1 >= end || tokens[i+1].kind != tokenParameter {
			continue
		}
		last := i + 1
		for last+2 < end && tokens[last+1].kind == tokenPunctuation && tokens[last+1].text == "," && tokens[last+2].kind == tokenParameter {
			last += 2
		}
		removeEnd := tokens[last].end
		if last+1 < end && onlyWhitespace(input[tokens[last].end:tokens[last+1].start]) {
			removeEnd = tokens[last+1].start
		}
		edits = append(edits, textEdit{start: tokens[i].start, end: removeEnd})
		removedStart, removedEnd = i, last
		break
	}

	for i := start; i < end; i++ {
		if i >= removedStart && i <= removedEnd {
			continue
		}
		if tokens[i].kind == tokenParameter {
			edits = append(edits, textEdit{start: tokens[i].start, end: tokens[i].end, replacement: "?"})
			continue
		}
		if tokens[i].kind == tokenIdentifier && (keyword(tokens[i], "NEW") || keyword(tokens[i], "OLD")) && i+2 < end &&
			tokens[i+1].kind == tokenPunctuation && tokens[i+1].text == "." && isNameToken(tokens[i+2]) {
			edits = append(edits, textEdit{start: tokens[i].start, end: tokens[i+2].end, replacement: "?"})
			i += 2
		}
	}
	sort.Slice(edits, func(i, j int) bool {
		return edits[i].start < edits[j].start
	})

	var output strings.Builder
	cursor := startOffset
	for _, edit := range edits {
		if edit.start < cursor || edit.end > endOffset {
			continue
		}
		output.WriteString(input[cursor:edit.start])
		output.WriteString(edit.replacement)
		cursor = edit.end
	}
	output.WriteString(input[cursor:endOffset])
	return strings.TrimSpace(output.String())
}

func onlyWhitespace(text string) bool {
	for _, r := range text {
		if !unicode.IsSpace(r) {
			return false
		}
	}
	return true
}

func statementTarget(tokens []token, start, end int, kind string) string {
	if kind == "update" && start+1 < end && isNameToken(tokens[start+1]) {
		return normalizedName(tokens[start+1])
	}
	keywordName := ""
	switch kind {
	case "insert":
		keywordName = "INTO"
	case "delete":
		keywordName = "FROM"
	default:
		return ""
	}
	for i := start + 1; i+1 < end; i++ {
		if tokens[i].depth == 0 && keyword(tokens[i], keywordName) && isNameToken(tokens[i+1]) {
			return normalizedName(tokens[i+1])
		}
	}
	return ""
}

func collectSources(tokens []token, start, end int, skipDeleteTarget bool) ([]string, []string) {
	var sources []string
	var calls []string
	seenSources := make(map[string]bool)
	seenCalls := make(map[string]bool)
	fromList := make(map[int]bool)
	addSource := func(index int) {
		if index >= end || !isNameToken(tokens[index]) {
			return
		}
		name := normalizedName(tokens[index])
		if !seenSources[name] {
			seenSources[name] = true
			sources = append(sources, name)
		}
		if index+1 < end && tokens[index+1].kind == tokenPunctuation && tokens[index+1].text == "(" && !seenCalls[name] {
			seenCalls[name] = true
			calls = append(calls, name)
		}
	}

	for i := start; i < end; i++ {
		t := tokens[i]
		if t.kind == tokenPunctuation {
			if t.text == ")" {
				fromList[t.depth] = false
			}
			if t.text == ";" {
				fromList[t.depth] = false
			}
			if t.text == "," && fromList[t.depth] {
				addSource(i + 1)
			}
			continue
		}
		if t.kind != tokenIdentifier {
			continue
		}
		if isSourceListEnd(t) {
			fromList[t.depth] = false
			continue
		}
		if keyword(t, "FROM") || keyword(t, "JOIN") {
			if skipDeleteTarget && keyword(t, "FROM") && i > start && keyword(tokens[i-1], "DELETE") && tokens[i-1].depth == t.depth {
				fromList[t.depth] = false
				continue
			}
			fromList[t.depth] = true
			addSource(i + 1)
		}
	}
	return sources, calls
}

func isSourceListEnd(t token) bool {
	switch strings.ToUpper(t.text) {
	case "WHERE", "GROUP", "ORDER", "HAVING", "UNION", "PLAN", "ROWS", "INTO", "DO":
		return true
	default:
		return false
	}
}
