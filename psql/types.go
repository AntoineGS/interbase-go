package psql

import "strconv"

// TypeFamily classifies a declared type for exact local evaluation
// (spec 6.1/6.2).
type TypeFamily int

const (
	TypeOther TypeFamily = iota
	TypeSmallint
	TypeInteger
	TypeChar
	TypeVarchar
)

// TypeInfo is a declared type. Length is the CHAR/VARCHAR length; Text is
// the type text as given.
type TypeInfo struct {
	Family TypeFamily
	Length int
	Text   string
}

const maxStringLength = 32767

// ParseTypeText classifies declared type text such as "CHAR(1)",
// "VARCHAR(20) CHARACTER SET NONE" or "INTEGER". CHAR/VARCHAR are only
// recognized without a character set or with CHARACTER SET NONE and without
// COLLATE; anything else is TypeOther.
func ParseTypeText(text string) TypeInfo {
	info := TypeInfo{Text: text}
	tokens := lex(text)
	word := func(i int, name string) bool { return i < len(tokens) && keyword(tokens[i], name) }
	punct := func(i int, value string) bool {
		return i < len(tokens) && tokens[i].kind == tokenPunctuation && tokens[i].text == value
	}
	switch {
	case len(tokens) == 1 && word(0, "SMALLINT"):
		info.Family = TypeSmallint
		return info
	case len(tokens) == 1 && (word(0, "INTEGER") || word(0, "INT")):
		info.Family = TypeInteger
		return info
	}

	family, i := TypeOther, 1
	switch {
	case (word(0, "CHAR") || word(0, "CHARACTER")) && word(1, "VARYING"):
		family, i = TypeVarchar, 2
	case word(0, "CHAR") || word(0, "CHARACTER"):
		family = TypeChar
	case word(0, "VARCHAR"):
		family = TypeVarchar
	default:
		return info
	}
	length := 1
	if punct(i, "(") {
		if i+2 >= len(tokens) || tokens[i+1].kind != tokenNumber || !punct(i+2, ")") {
			return info
		}
		n, err := strconv.Atoi(tokens[i+1].text)
		if err != nil || n < 1 || n > maxStringLength {
			return info
		}
		length, i = n, i+3
	} else if family == TypeVarchar {
		return info
	}
	if word(i, "CHARACTER") && word(i+1, "SET") && word(i+2, "NONE") {
		i += 3
	}
	if i != len(tokens) {
		return info
	}
	info.Family, info.Length = family, length
	return info
}
