package interbase

import (
	"fmt"
	"strings"

	"interbase-go/psql"
)

// lpVar is one variable or parameter of an emulated routine. value holds
// the driver's exact representation as returned by Cursor.Row (nil is NULL).
type lpVar struct {
	typ          psql.TypeInfo
	stringsLocal bool // CHAR/VARCHAR whose effective character set is NONE (R9)
	value        any
}

// lpFrame is the state of one emulated routine call (spec 6.1).
type lpFrame struct {
	meta       *lpRoutineMeta
	vars       map[string]*lpVar
	profile    *LineProfileRoutine
	routine    int // index of profile in LineProfileResult.Routines
	entry      bool
	id         int // frame number in savepoint names; 0 for the entry routine
	savepoints int // savepoints started by the frame so far
	depth      int // stepped-into calls below the entry routine; 0 for it
	// guards counts the blocks with WHEN handlers being run, the caller's
	// at the call included: a handler of any of them may catch an error.
	guards int
	// level counts the frames of the same routine still running when this
	// one started (recursion, or a callee suspended in a loop), so that
	// each keeps its own prepared statements and cursors.
	level int
	// suspend hands a row to the caller of a stepped-into selectable
	// routine (nil otherwise); columns are the outputs it selects.
	suspend func([]any) error
	columns []lpParam
}

func newFrame(meta *lpRoutineMeta, dbNone bool, profile *LineProfileRoutine) *lpFrame {
	f := &lpFrame{meta: meta, vars: make(map[string]*lpVar), profile: profile}
	add := func(name, typeText string) {
		typ := psql.ParseTypeText(typeText)
		f.vars[name] = &lpVar{typ: typ, stringsLocal: isStringFamily(typ) && stringsLocal(typeText, dbNone)}
	}
	for _, p := range meta.inputs {
		add(p.name, p.typeText)
	}
	for _, p := range meta.outputs {
		add(p.name, p.typeText)
	}
	for _, d := range meta.routine.Declarations {
		add(d.Name, d.TypeText)
	}
	return f
}

// localScope exposes the frame's local-eligible values.
func (f *lpFrame) localScope(dbNone bool) localScope {
	return localScope{
		literalStrings: dbNone,
		lookup: func(name string) (localValue, bool) {
			v, ok := f.vars[name]
			if !ok {
				return localValue{}, false
			}
			return localValueOf(v)
		},
	}
}

// localValueOf converts a held value to the local subset when its declared
// type allows exact local evaluation.
func localValueOf(v *lpVar) (localValue, bool) {
	if v.value == nil {
		// A NULL keeps its variable's family, so comparing it with a value
		// of another family still goes to the server.
		fam := famOther
		switch {
		case v.typ.Family == psql.TypeSmallint || v.typ.Family == psql.TypeInteger:
			fam = famInt
		case isStringFamily(v.typ) && v.stringsLocal:
			fam = famString
		}
		return localValue{kind: localNullKind, fam: fam}, true
	}
	switch v.typ.Family {
	case psql.TypeSmallint, psql.TypeInteger:
		if i, ok := v.value.(int64); ok {
			return localValue{kind: localIntKind, i: i}, true
		}
	case psql.TypeChar, psql.TypeVarchar:
		if s, ok := v.value.(string); ok && v.stringsLocal {
			return localValue{kind: localStringKind, s: []byte(s)}, true
		}
	}
	if isBlobType(v.typ.Text) {
		// Natively IS NULL is false for any BLOB variable holding a value,
		// even an integer taken as a BLOB id (lpBadBlob), whose other uses
		// fail (ibg-b6o).
		return localValue{kind: localOpaqueKind}, true
	}
	return localValue{}, false
}

func isStringFamily(typ psql.TypeInfo) bool {
	return typ.Family == psql.TypeChar || typ.Family == psql.TypeVarchar
}

// stringsLocal reports whether string values of a declared type compare and
// convert as bytes: its character set is NONE explicitly, or it has no
// character set and the database default is NONE (R9).
func stringsLocal(typeText string, dbNone bool) bool {
	typ := psql.ParseTypeText(typeText)
	if !isStringFamily(typ) {
		return false
	}
	normalized := strings.Join(strings.Fields(strings.ToUpper(typeText)), " ")
	if strings.Contains(normalized, "CHARACTER SET NONE") {
		return true
	}
	return dbNone && !strings.Contains(normalized, "CHARACTER SET")
}

// convertLocal converts v to typ when the result is certain to equal the
// server's (spec 6.2): NULL to any type, integers to SMALLINT/INTEGER in
// range, strings to CHAR(n)/VARCHAR(n) within n bytes (CHAR padded). As
// InterBase does (verified natively, dialects 1 and 3), a longer string
// whose bytes beyond n are all blanks is cut to n bytes; a non-blank byte
// beyond n is a truncation error, left to the server.
func convertLocal(v localValue, typ psql.TypeInfo, targetStringsLocal bool) (any, bool) {
	switch v.kind {
	case localNullKind:
		return nil, true
	case localIntKind:
		switch typ.Family {
		case psql.TypeSmallint:
			if v.i >= -32768 && v.i <= 32767 {
				return v.i, true
			}
		case psql.TypeInteger:
			if v.i >= minLocalInteger && v.i <= maxLocalInteger {
				return v.i, true
			}
		}
	case localStringKind:
		if !targetStringsLocal || !isStringFamily(typ) {
			return nil, false
		}
		text := v.s
		if len(text) > typ.Length {
			if strings.Trim(string(text[typ.Length:]), " ") != "" {
				return nil, false
			}
			text = text[:typ.Length]
		}
		if typ.Family == psql.TypeChar {
			return string(text) + strings.Repeat(" ", typ.Length-len(text)), true
		}
		return string(text), true
	}
	return nil, false
}

// comparePadded compares byte strings as CHAR values: the shorter operand
// is padded with blanks, so trailing blanks are insignificant.
func comparePadded(a, b []byte) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		x, y := byte(' '), byte(' ')
		if i < len(a) {
			x = a[i]
		}
		if i < len(b) {
			y = b[i]
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

// lpField is the catalog description of a parameter's domain.
type lpField struct {
	typ        int64
	subType    any // int64 or nil, as read from the catalog
	length     int64
	scale      any
	precision  any
	charLength any
	charset    string
}

func catalogInt(value any) (int64, bool) {
	v, ok := value.(int64)
	return v, ok
}

// typeText renders the declared type for CAST in the given dialect.
func (f lpField) typeText(dialect int) (string, error) {
	subType, _ := catalogInt(f.subType)
	scale, _ := catalogInt(f.scale)
	precision, _ := catalogInt(f.precision)
	numeric := func(defaultPrecision int64) string {
		name := "NUMERIC"
		if subType == 2 {
			name = "DECIMAL"
		}
		if precision <= 0 {
			precision = defaultPrecision
		}
		return fmt.Sprintf("%s(%d,%d)", name, precision, -scale)
	}
	switch f.typ {
	case 7, 8, 16:
		defaults := map[int64]int64{7: 4, 8: 9, 16: 18}
		if subType == 1 || subType == 2 || scale < 0 || f.typ == 16 {
			return numeric(defaults[f.typ]), nil
		}
		if f.typ == 7 {
			return "SMALLINT", nil
		}
		return "INTEGER", nil
	case 10:
		return "FLOAT", nil
	case 11, 27:
		if subType == 1 || subType == 2 || scale < 0 {
			return numeric(15), nil
		}
		return "DOUBLE PRECISION", nil
	case 12:
		return "DATE", nil
	case 13:
		return "TIME", nil
	case 35:
		if dialect == 1 {
			return "DATE", nil
		}
		return "TIMESTAMP", nil
	case 17:
		return "BOOLEAN", nil
	case 14, 37, 40:
		length := f.length
		if n, ok := catalogInt(f.charLength); ok && n > 0 {
			length = n
		}
		name := "CHAR"
		if f.typ != 14 {
			name = "VARCHAR"
		}
		text := fmt.Sprintf("%s(%d)", name, length)
		if f.charset != "" {
			text += " CHARACTER SET " + f.charset
		}
		return text, nil
	case 261:
		return fmt.Sprintf("BLOB SUB_TYPE %d", subType), nil
	default:
		return "", fmt.Errorf("unsupported parameter field type %d", f.typ)
	}
}

// sqlCode calls visit for each maximal [start, end) range of text outside
// string literals, double-quoted text and comments.
func sqlCode(text string, visit func(start, end int)) {
	start := 0
	for i := 0; i < len(text); {
		c := text[i]
		var skip int
		switch {
		case c == '\'' || c == '"':
			j := i + 1
			for j < len(text) {
				if text[j] == c {
					if j+1 < len(text) && text[j+1] == c {
						j += 2
						continue
					}
					break
				}
				j++
			}
			skip = j + 1
		case c == '/' && i+1 < len(text) && text[i+1] == '*':
			end := strings.Index(text[i+2:], "*/")
			if end < 0 {
				skip = len(text)
			} else {
				skip = i + 2 + end + 2
			}
		case c == '-' && i+1 < len(text) && text[i+1] == '-':
			end := strings.IndexByte(text[i:], '\n')
			if end < 0 {
				skip = len(text)
			} else {
				skip = i + end
			}
		default:
			i++
			continue
		}
		if skip > len(text) {
			skip = len(text)
		}
		if start < i {
			visit(start, i)
		}
		i, start = skip, skip
	}
	if start < len(text) {
		visit(start, len(text))
	}
}

// castParams rewrites the n-th "?" outside literals and comments as
// CAST(? AS types[n]) when types[n] is not empty.
func castParams(text string, types []string) string {
	var out strings.Builder
	cursor, index := 0, 0
	sqlCode(text, func(start, end int) {
		for i := start; i < end; i++ {
			if text[i] != '?' {
				continue
			}
			if index < len(types) && types[index] != "" {
				out.WriteString(text[cursor:i])
				out.WriteString("CAST(? AS " + types[index] + ")")
				cursor = i + 1
			}
			index++
		}
	})
	out.WriteString(text[cursor:])
	return out.String()
}

func isWordByte(c byte) bool {
	return c == '_' || c == '$' || c >= '0' && c <= '9' || c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= 0x80
}

// wordsIn calls visit for every identifier-like word outside literals and
// comments with its byte range; dotted is set for a name qualified by a
// preceding ".", colon for one preceded by ":".
func wordsIn(text string, visit func(start, end int, dotted, colon bool)) {
	sqlCode(text, func(start, end int) {
		for i := start; i < end; {
			if !isWordByte(text[i]) {
				i++
				continue
			}
			j := i
			for j < end && isWordByte(text[j]) {
				j++
			}
			before := i - 1
			for before >= start && (text[before] == ' ' || text[before] == '\t' || text[before] == '\r' || text[before] == '\n') {
				before--
			}
			dotted := before >= 0 && text[before] == '.'
			colon := before >= 0 && text[before] == ':'
			if text[i] < '0' || text[i] > '9' {
				visit(i, j, dotted, colon)
			}
			i = j
		}
	})
}

// unknownColonNames returns the :name references of text whose name is not
// in names, in order of appearance.
func unknownColonNames(text string, names map[string]bool) []string {
	var unknown []string
	wordsIn(text, func(start, end int, dotted, colon bool) {
		if !colon {
			return
		}
		name := strings.ToUpper(text[start:end])
		if !names[name] {
			unknown = append(unknown, name)
		}
	})
	return unknown
}

// cellValue returns a cell's value, nil for NULL.
func cellValue(cell Cell) any {
	if cell.Indicator&nullIndicator != 0 {
		return nil
	}
	return cell.Value
}

func valueCell(value any) Cell {
	if value == nil {
		return Cell{Indicator: nullIndicator}
	}
	return Cell{Value: value}
}
