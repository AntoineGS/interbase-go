package psql

// AliasScopes is the FROM-list analysis for each SELECT query block in SQL.
// Query blocks include derived tables, scalar/subquery expressions, and UNION
// branches. Resolution is deliberately conservative when the same label has
// different meanings in different blocks.
type AliasScopes struct {
	scopes   []*aliasScope
	top      *aliasScope
	topLevel []*aliasScope
	quoted   map[string]struct{}
	isSelect bool
}

type aliasScope struct {
	depth     int
	start     int
	end       int
	aliases   map[string]string
	relations map[string]string
	derived   map[string][]*aliasScope
	names     map[string]string
	nameOrder []string
	nameSeen  map[string]struct{}
}

// AnalyzeAliases parses SQL into query-block scopes and their FROM-list aliases.
// It is a lexical analysis, not a complete SQL parser.
func AnalyzeAliases(sql string) AliasScopes {
	tokens := lex(sql)
	result := AliasScopes{quoted: make(map[string]struct{})}
	for _, token := range tokens {
		if token.kind == tokenQuotedIdentifier {
			result.quoted[token.text] = struct{}{}
		}
	}

	for index, token := range tokens {
		if !keyword(token, "SELECT") {
			continue
		}
		scope := &aliasScope{
			depth: token.depth, start: index,
			aliases: make(map[string]string), relations: make(map[string]string),
			derived: make(map[string][]*aliasScope), names: make(map[string]string),
			nameSeen: make(map[string]struct{}),
		}
		scope.end = queryBlockEnd(tokens, index)
		result.scopes = append(result.scopes, scope)
		if scope.depth == 0 {
			result.topLevel = append(result.topLevel, scope)
			if result.top == nil {
				result.top = scope
			}
		}
	}

	result.isSelect = isSelectStatement(tokens, result.top)
	for _, scope := range result.scopes {
		readScopeFromList(tokens, scope, result.scopes)
	}
	return result
}

// Resolve returns the underlying relation for a plan label chain. A chain
// beginning with a derived-table alias is resolved in that table's query
// block; a single label is accepted only when its meaning is unambiguous.
func (scopes AliasScopes) Resolve(chain []string) (string, bool) {
	if len(chain) == 0 {
		return "", false
	}
	return scopes.resolve(scopes.scopes, chain)
}

// IsSelect reports whether the SQL statement is a top-level SELECT (including
// a SELECT preceded by CTE definitions).
func (scopes AliasScopes) IsSelect() bool { return scopes.isSelect }

// TopLevelNames returns relations and aliases from all outermost SELECT
// branches, including derived-table aliases.
func (scopes AliasScopes) TopLevelNames() []string {
	var names []string
	seen := make(map[string]struct{})
	for _, top := range scopes.topLevel {
		for _, name := range top.nameOrder {
			key := scopes.nameKey(name)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			names = append(names, name)
		}
	}
	return names
}

// IsTopLevelName reports whether name is one of the outermost FROM-list
// relations or aliases.
func (scopes AliasScopes) IsTopLevelName(name string) bool {
	key := scopes.nameKey(name)
	for _, top := range scopes.topLevel {
		if _, ok := top.names[key]; ok {
			return true
		}
	}
	return false
}

// TopLevelRelations returns source names associated with a direct top-level
// FROM-list relation or alias. Callers must use catalog metadata to determine
// whether each source is a relation. Derived-table aliases have no direct
// relation and return no names.
func (scopes AliasScopes) TopLevelRelations(name string) []string {
	key := scopes.nameKey(name)
	var relations []string
	seen := make(map[string]struct{})
	for _, top := range scopes.topLevel {
		if relation, ok := top.aliases[key]; ok {
			if _, exists := seen[relation]; !exists {
				seen[relation] = struct{}{}
				relations = append(relations, relation)
			}
		}
	}
	return relations
}

// TopLevelDerivedRelations returns source names referenced inside a top-level
// derived-table source. It is separate from TopLevelRelations so callers do
// not mistake an inner table plan for a plan of the derived source.
func (scopes AliasScopes) TopLevelDerivedRelations(name string) []string {
	key := scopes.nameKey(name)
	var relations []string
	seen := make(map[string]struct{})
	for _, top := range scopes.topLevel {
		for _, inner := range top.derived[key] {
			scopes.appendScopeRelations(inner, seen, &relations)
		}
	}
	return relations
}

func (scopes AliasScopes) appendScopeRelations(scope *aliasScope, seen map[string]struct{}, relations *[]string) {
	for _, name := range scope.nameOrder {
		key := scopes.nameKey(name)
		if relation, ok := scope.aliases[key]; ok {
			if _, exists := seen[relation]; !exists {
				seen[relation] = struct{}{}
				*relations = append(*relations, relation)
			}
		}
		for _, inner := range scope.derived[key] {
			scopes.appendScopeRelations(inner, seen, relations)
		}
	}
}

// HasDerived reports whether any query block defines name as a derived-table
// alias. It lets catalog resolution distinguish an unresolved derived chain
// from a view chain.
func (scopes AliasScopes) HasDerived(name string) bool {
	key := scopes.nameKey(name)
	for _, scope := range scopes.scopes {
		if _, ok := scope.derived[key]; ok {
			return true
		}
	}
	return false
}

func (scopes AliasScopes) resolve(search []*aliasScope, chain []string) (string, bool) {
	if len(chain) == 1 {
		return scopes.resolveSingle(search, chain[0])
	}

	key := scopes.nameKey(chain[0])
	var resolved string
	found := false
	for _, scope := range search {
		inner, ok := scope.derived[key]
		if !ok {
			continue
		}
		relation, ok := scopes.resolve(inner, chain[1:])
		if !ok {
			continue
		}
		if found && !sameIdentifier(resolved, relation) {
			return "", false
		}
		resolved, found = relation, true
	}
	return resolved, found
}

func (scopes AliasScopes) resolveSingle(search []*aliasScope, name string) (string, bool) {
	key := scopes.nameKey(name)
	var resolved string
	found := false
	conflict := false
	for _, scope := range search {
		if relation, ok := scope.aliases[key]; ok {
			if found && !sameIdentifier(resolved, relation) {
				conflict = true
			}
			resolved, found = relation, true
		}
	}
	if found && !conflict {
		return resolved, true
	}
	if conflict {
		for _, scope := range search {
			if relation, ok := scope.relations[key]; ok {
				return relation, true
			}
		}
	}
	return "", false
}

func (scopes AliasScopes) nameKey(name string) string {
	if _, quoted := scopes.quoted[name]; quoted {
		return name
	}
	return asciiUpper(name)
}

func queryBlockEnd(tokens []token, start int) int {
	depth := tokens[start].depth
	for index := start + 1; index < len(tokens); index++ {
		current := tokens[index]
		if current.depth != depth {
			continue
		}
		if current.kind == tokenPunctuation && current.text == ")" {
			return index
		}
		if keyword(current, "UNION") {
			return index
		}
	}
	return len(tokens)
}

func isSelectStatement(tokens []token, top *aliasScope) bool {
	if top == nil {
		return false
	}
	first := -1
	for index, current := range tokens {
		if current.depth == 0 && (current.kind == tokenIdentifier || current.kind == tokenQuotedIdentifier) {
			first = index
			break
		}
	}
	if first < 0 {
		return false
	}
	if keyword(tokens[first], "SELECT") {
		return true
	}
	if !keyword(tokens[first], "WITH") {
		return false
	}
	for index := first + 1; index < len(tokens); index++ {
		if tokens[index].depth == 0 && keyword(tokens[index], "SELECT") {
			return true
		}
		if tokens[index].depth == 0 && (keyword(tokens[index], "INSERT") || keyword(tokens[index], "UPDATE") || keyword(tokens[index], "DELETE")) {
			return false
		}
	}
	return false
}

func readScopeFromList(tokens []token, scope *aliasScope, scopes []*aliasScope) {
	fromList := false
	for index := scope.start; index < scope.end; index++ {
		current := tokens[index]
		if current.depth != scope.depth {
			continue
		}
		if current.kind == tokenPunctuation {
			if current.text == "," && fromList {
				addScopeSource(tokens, index+1, scope, scopes)
			}
			continue
		}
		if current.kind != tokenIdentifier {
			continue
		}
		if isSourceListEnd(current) {
			fromList = false
			continue
		}
		if keyword(current, "FROM") || keyword(current, "JOIN") {
			fromList = true
			addScopeSource(tokens, index+1, scope, scopes)
		}
	}
}

func addScopeSource(tokens []token, index int, scope *aliasScope, scopes []*aliasScope) {
	if index >= len(tokens) || tokens[index].depth != scope.depth {
		return
	}
	if tokens[index].kind == tokenPunctuation && tokens[index].text == "(" {
		close := matchingClose(tokens, index)
		if close < 0 {
			return
		}
		aliasIndex := aliasIndexAfter(tokens, close+1, scope.depth)
		if aliasIndex < 0 {
			return
		}
		alias := normalizedName(tokens[aliasIndex])
		key := aliasKey(tokens[aliasIndex])
		inner := make([]*aliasScope, 0, 1)
		for _, candidate := range scopes {
			if candidate.depth == scope.depth+1 && candidate.start > index && candidate.start < close {
				inner = append(inner, candidate)
			}
		}
		if len(inner) == 0 {
			return
		}
		scope.derived[key] = append(scope.derived[key], inner...)
		scope.addName(key, alias)
		return
	}
	if !isNameToken(tokens[index]) {
		return
	}

	relation := normalizedName(tokens[index])
	key := aliasKey(tokens[index])
	after := index + 1
	for after+1 < len(tokens) && tokens[after].depth == scope.depth && tokens[after].kind == tokenPunctuation &&
		tokens[after].text == "." && isNameToken(tokens[after+1]) && tokens[after+1].depth == scope.depth {
		relation = normalizedName(tokens[after+1])
		key = aliasKey(tokens[after+1])
		after += 2
	}
	scope.aliases[key] = relation
	scope.relations[key] = relation
	scope.addName(key, relation)

	if after < len(tokens) && tokens[after].depth == scope.depth && tokens[after].kind == tokenPunctuation && tokens[after].text == "(" {
		after = matchingClose(tokens, after) + 1
		if after == 0 {
			return
		}
	}
	aliasIndex := aliasIndexAfter(tokens, after, scope.depth)
	if aliasIndex < 0 {
		return
	}
	aliasToken := tokens[aliasIndex]
	alias := normalizedName(aliasToken)
	aliasNameKey := aliasKey(aliasToken)
	scope.aliases[aliasNameKey] = relation
	scope.addName(aliasNameKey, alias)
}

func aliasIndexAfter(tokens []token, index, depth int) int {
	if index >= len(tokens) || tokens[index].depth != depth {
		return -1
	}
	if keyword(tokens[index], "AS") {
		index++
		if index >= len(tokens) || tokens[index].depth != depth || !isNameToken(tokens[index]) || isAliasKeyword(tokens[index]) {
			return -1
		}
		return index
	}
	if isNameToken(tokens[index]) && !isAliasKeyword(tokens[index]) {
		return index
	}
	return -1
}

func matchingClose(tokens []token, open int) int {
	if open >= len(tokens) || tokens[open].kind != tokenPunctuation || tokens[open].text != "(" {
		return -1
	}
	depth := tokens[open].depth + 1
	for index := open + 1; index < len(tokens); index++ {
		if tokens[index].kind == tokenPunctuation && tokens[index].text == ")" && tokens[index].depth == depth {
			return index
		}
	}
	return -1
}

func aliasKey(token token) string { return normalizedName(token) }

func (scope *aliasScope) addName(key, name string) {
	if _, exists := scope.nameSeen[key]; exists {
		return
	}
	scope.nameSeen[key] = struct{}{}
	scope.names[key] = name
	scope.nameOrder = append(scope.nameOrder, name)
}

func sameIdentifier(left, right string) bool {
	return left == right
}
