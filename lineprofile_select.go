package interbase

import "strings"

// sqlToken is a word or a comma of a statement at parenthesis depth 0.
type sqlToken struct {
	start, end int
	comma      bool
}

// topLevelTokens lists the words and commas of text at parenthesis depth 0,
// outside literals, quoted identifiers and comments.
func topLevelTokens(text string) []sqlToken {
	var tokens []sqlToken
	depth := 0
	sqlCode(text, func(start, end int) {
		for i := start; i < end; {
			c := text[i]
			switch {
			case c == '(':
				depth++
			case c == ')':
				depth--
			case depth == 0 && c == ',':
				tokens = append(tokens, sqlToken{start: i, end: i + 1, comma: true})
			case depth == 0 && isWordByte(c):
				j := i
				for j < end && isWordByte(text[j]) {
					j++
				}
				tokens = append(tokens, sqlToken{start: i, end: j})
				i = j
				continue
			}
			i++
		}
	})
	return tokens
}

// castSelectItems rewrites the select-list item i of a SELECT statement as
// CAST(<item> AS types[i]) when types[i] is not empty (R18), so the server
// converts each column to its INTO target while fetching. It returns "" when
// nothing is rewritten: the statement has DISTINCT, UNION, GROUP BY, HAVING
// or ORDER BY by position, an item is "*", or the item count differs from
// len(types). FIRST and SKIP arguments before the list (an integer or "?";
// any other argument prevents the rewrite) and a trailing ROWS clause are
// not select items. An aliased item keeps its alias after the CAST; an item with
// a COLLATE clause or a "--" comment is left unchanged.
// cast reports the rewritten items.
func castSelectItems(text string, types []string) (string, []bool) {
	tokens := topLevelTokens(text)
	word := func(i int, w string) bool {
		return i >= 0 && i < len(tokens) && !tokens[i].comma && strings.EqualFold(text[tokens[i].start:tokens[i].end], w)
	}
	if !word(0, "SELECT") {
		return "", nil
	}
	listStart, first := tokens[0].end, 1
	for _, limit := range []string{"FIRST", "SKIP"} {
		if !word(first, limit) {
			continue
		}
		end, ok := limitArgEnd(text, tokens[first].end)
		if !ok {
			return "", nil
		}
		for first < len(tokens) && tokens[first].start < end {
			first++
		}
		listStart = end
	}
	if word(first, "DISTINCT") {
		return "", nil
	}
	if word(first, "ALL") {
		listStart, first = tokens[first].end, first+1
	}
	from := -1
	for i := first; i < len(tokens); i++ {
		if word(i, "FROM") {
			from = i
			break
		}
	}
	if from < 0 {
		return "", nil
	}
	for i := from; i < len(tokens); i++ {
		if word(i, "UNION") || word(i, "GROUP") || word(i, "HAVING") {
			return "", nil
		}
		if word(i, "BY") && word(i-1, "ORDER") {
			for j := i + 1; j < len(tokens); j++ {
				if !tokens[j].comma && (j == i+1 || tokens[j-1].comma) && text[tokens[j].start] >= '0' && text[tokens[j].start] <= '9' {
					return "", nil
				}
			}
		}
	}
	var items [][2]int
	start := listStart
	for i := first; i < from; i++ {
		if tokens[i].comma {
			items = append(items, [2]int{start, tokens[i].start})
			start = tokens[i].end
		}
	}
	items = append(items, [2]int{start, tokens[from].start})
	if len(items) != len(types) {
		return "", nil
	}
	var out strings.Builder
	cast := make([]bool, len(items))
	rewritten := false
	cursor := 0
	for i, item := range items {
		raw := text[item[0]:item[1]]
		core := strings.TrimSpace(raw)
		if core == "*" || strings.HasSuffix(core, ".*") {
			return "", nil
		}
		if types[i] == "" || strings.Contains(core, "--") {
			continue
		}
		expr, alias, ok := splitAlias(core)
		if !ok || expr == "" {
			continue
		}
		lead := strings.Index(raw, core)
		out.WriteString(text[cursor : item[0]+lead])
		out.WriteString("CAST(" + expr + " AS " + types[i] + ")" + alias)
		cursor = item[0] + lead + len(core)
		cast[i], rewritten = true, true
	}
	if !rewritten {
		return "", nil
	}
	out.WriteString(text[cursor:])
	return out.String(), cast
}

// limitArgEnd returns the end of the FIRST or SKIP argument that follows
// whitespace at pos: an integer or "?". ok is false for any other argument,
// such as a parenthesized expression, which castSelectItems does not rewrite.
func limitArgEnd(text string, pos int) (int, bool) {
	i := pos
	for i < len(text) && isASCIISpace(text[i]) {
		i++
	}
	if i == pos || i == len(text) {
		return 0, false
	}
	if text[i] == '?' {
		i++
	} else {
		j := i
		for i < len(text) && text[i] >= '0' && text[i] <= '9' {
			i++
		}
		if i == j {
			return 0, false
		}
	}
	if i < len(text) && !isASCIISpace(text[i]) {
		return 0, false
	}
	return i, true
}

func isASCIISpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

// aliasOperators are the words after which a final name is an operand,
// not an alias (A IS NULL, A LIKE B, A STARTING WITH B, …).
var aliasOperators = map[string]bool{
	"IS": true, "NOT": true, "AND": true, "OR": true, "LIKE": true, "CONTAINING": true, "STARTING": true,
	"WITH": true, "BETWEEN": true, "IN": true, "ESCAPE": true, "SIMILAR": true, "TO": true,
}

// splitAlias splits a select-list item into its expression and its alias
// ("" when it has none): a final name, plain or double-quoted, preceded by
// AS, or directly by a word, a quoted name, a literal or ")". CASE … END
// has no alias. ok is false when the item ends with a COLLATE clause, which
// CAST cannot wrap, or with an operand of a predicate operator (or NULL),
// which this simple split cannot tell from an alias; such an item is left
// unchanged.
func splitAlias(item string) (expr, alias string, ok bool) {
	if item == "" {
		return item, "", true
	}
	k := len(item)
	switch last := item[k-1]; {
	case last == '"':
		k--
		for k > 0 {
			k--
			if item[k] != '"' {
				continue
			}
			if k > 0 && item[k-1] == '"' {
				k--
				continue
			}
			break
		}
	case isWordByte(last):
		for k > 0 && isWordByte(item[k-1]) {
			k--
		}
		switch strings.ToUpper(item[k:]) {
		case "END":
			return item, "", true
		case "NULL":
			if strings.TrimSpace(item[:k]) != "" {
				return item, "", false
			}
		}
	default:
		return item, "", true
	}
	before := strings.TrimRight(item[:k], " \t\r\n")
	if before == "" {
		return item, "", true
	}
	c := before[len(before)-1]
	switch {
	case isWordByte(c):
		w := len(before)
		for w > 0 && isWordByte(before[w-1]) {
			w--
		}
		switch word := strings.ToUpper(before[w:]); {
		case word == "COLLATE" || aliasOperators[word]:
			return item, "", false
		case word == "AS":
			expr := strings.TrimRight(before[:w], " \t\r\n")
			return expr, item[len(expr):], true
		}
		return before, item[len(before):], true
	case c == '"' || c == '\'' || c == ')':
		return before, item[len(before):], true
	}
	return item, "", true
}
