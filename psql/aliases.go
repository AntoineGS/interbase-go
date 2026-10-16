package psql

// Aliases returns a relation's self mapping and any aliases found in FROM
// lists. Unquoted names are normalized to upper case; quoted names retain
// their exact contents.
func Aliases(sql string) map[string]string {
	tokens := lex(sql)
	aliases := make(map[string]string)
	fromList := make(map[int]bool)
	addRelation := func(index int) {
		if index >= len(tokens) || !isNameToken(tokens[index]) {
			return
		}
		relation := normalizedName(tokens[index])
		aliases[relation] = relation

		next := relationAfter(tokens, index)
		if next >= len(tokens) {
			return
		}
		aliasIndex := -1
		if keyword(tokens[next], "AS") {
			if next+1 < len(tokens) && isNameToken(tokens[next+1]) && !isAliasKeyword(tokens[next+1]) {
				aliasIndex = next + 1
			}
		} else if isNameToken(tokens[next]) && !isAliasKeyword(tokens[next]) {
			aliasIndex = next
		}
		if aliasIndex >= 0 {
			aliases[normalizedName(tokens[aliasIndex])] = relation
		}
	}

	for i, t := range tokens {
		if t.kind == tokenPunctuation {
			if t.text == ")" {
				fromList[t.depth] = false
			}
			if t.text == "," && fromList[t.depth] {
				addRelation(i + 1)
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
			fromList[t.depth] = true
			addRelation(i + 1)
		}
	}
	return aliases
}

func relationAfter(tokens []token, relation int) int {
	next := relation + 1
	if next >= len(tokens) || tokens[next].kind != tokenPunctuation || tokens[next].text != "(" {
		return next
	}
	depth := tokens[next].depth + 1
	for i := next + 1; i < len(tokens); i++ {
		if tokens[i].kind == tokenPunctuation && tokens[i].text == ")" && tokens[i].depth == depth {
			return i + 1
		}
	}
	return len(tokens)
}

func isAliasKeyword(t token) bool {
	if t.kind != tokenIdentifier {
		return false
	}
	switch asciiUpper(t.text) {
	case "WHERE", "ON", "JOIN", "LEFT", "RIGHT", "INNER", "OUTER", "FULL", "CROSS", "GROUP", "ORDER", "HAVING", "UNION", "PLAN", "ROWS", "INTO", "FOR", "NATURAL":
		return true
	default:
		return false
	}
}
