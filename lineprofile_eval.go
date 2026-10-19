package interbase

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"interbase-go/psql"
)

const (
	maxLocalInteger = 1<<31 - 1
	minLocalInteger = -1 << 31
)

type localKind uint8

const (
	localNullKind localKind = iota
	localIntKind
	localStringKind
	localTruthKind
)

// localFamily is the type family of a local value; a NULL read from a
// variable keeps its variable's family, the NULL literal has famAny.
type localFamily uint8

const (
	famAny localFamily = iota
	famInt
	famString
	famOther
)

type truth uint8

const (
	truthUnknown truth = iota
	truthFalse
	truthTrue
)

func (t truth) String() string {
	return [...]string{"unknown", "false", "true"}[t]
}

// localValue is a value or condition result of the exact local subset.
type localValue struct {
	kind localKind
	fam  localFamily // for localNullKind
	i    int64
	s    []byte
	t    truth
}

func (v localValue) family() localFamily {
	switch v.kind {
	case localIntKind:
		return famInt
	case localStringKind:
		return famString
	case localNullKind:
		return v.fam
	}
	return famOther
}

// localScope resolves variables to local values; lookup reports false for
// a value outside the local subset. literalStrings is set when string
// literals compare as bytes (database default character set NONE, R9).
type localScope struct {
	lookup         func(name string) (localValue, bool)
	literalStrings bool
}

// evalLocalCondition evaluates a condition locally; false means "not
// local": the caller must use the server.
func evalLocalCondition(n psql.LocalNode, scope localScope) (truth, bool) {
	v, ok := evalLocal(n, scope)
	if !ok || v.kind != localTruthKind {
		return truthUnknown, false
	}
	return v.t, true
}

// evalLocalValue evaluates a value locally; a condition is not a value.
func evalLocalValue(n psql.LocalNode, scope localScope) (localValue, bool) {
	v, ok := evalLocal(n, scope)
	if !ok || v.kind == localTruthKind {
		return localValue{}, false
	}
	return v, true
}

func truthOf(b bool) localValue {
	if b {
		return localValue{kind: localTruthKind, t: truthTrue}
	}
	return localValue{kind: localTruthKind, t: truthFalse}
}

func evalLocal(n psql.LocalNode, scope localScope) (localValue, bool) {
	switch n := n.(type) {
	case psql.LocalInt:
		if n.Value < minLocalInteger || n.Value > maxLocalInteger {
			return localValue{}, false
		}
		return localValue{kind: localIntKind, i: n.Value}, true
	case psql.LocalString:
		if !scope.literalStrings {
			return localValue{}, false
		}
		return localValue{kind: localStringKind, s: n.Value}, true
	case psql.LocalNull:
		return localValue{kind: localNullKind, fam: famAny}, true
	case psql.LocalVar:
		if scope.lookup == nil {
			return localValue{}, false
		}
		return scope.lookup(n.Name)
	case psql.LocalNot:
		x, ok := evalLocal(n.X, scope)
		if !ok || x.kind != localTruthKind {
			return localValue{}, false
		}
		switch x.t {
		case truthTrue:
			return truthOf(false), true
		case truthFalse:
			return truthOf(true), true
		}
		return localValue{kind: localTruthKind, t: truthUnknown}, true
	case psql.LocalIsNull:
		x, ok := evalLocal(n.X, scope)
		if !ok || x.kind == localTruthKind {
			return localValue{}, false
		}
		return truthOf((x.kind == localNullKind) != n.Negated), true
	case psql.LocalBinary:
		l, ok := evalLocal(n.L, scope)
		if !ok {
			return localValue{}, false
		}
		r, ok := evalLocal(n.R, scope)
		if !ok {
			return localValue{}, false
		}
		switch n.Op {
		case "AND", "OR":
			if l.kind != localTruthKind || r.kind != localTruthKind {
				return localValue{}, false
			}
			return logical(n.Op, l.t, r.t), true
		case "+", "-":
			return arithmetic(n.Op, l, r)
		case "=", "<>", "<", ">", "<=", ">=":
			return compare(n.Op, l, r)
		}
	}
	return localValue{}, false
}

func logical(op string, l, r truth) localValue {
	if op == "AND" {
		switch {
		case l == truthFalse || r == truthFalse:
			return truthOf(false)
		case l == truthTrue && r == truthTrue:
			return truthOf(true)
		}
	} else {
		switch {
		case l == truthTrue || r == truthTrue:
			return truthOf(true)
		case l == truthFalse && r == truthFalse:
			return truthOf(false)
		}
	}
	return localValue{kind: localTruthKind, t: truthUnknown}
}

func arithmetic(op string, l, r localValue) (localValue, bool) {
	for _, v := range []localValue{l, r} {
		if f := v.family(); f != famInt && f != famAny {
			return localValue{}, false
		}
	}
	if l.kind == localNullKind || r.kind == localNullKind {
		return localValue{kind: localNullKind, fam: famInt}, true
	}
	result := l.i + r.i
	if op == "-" {
		result = l.i - r.i
	}
	if result < minLocalInteger || result > maxLocalInteger {
		return localValue{}, false
	}
	return localValue{kind: localIntKind, i: result}, true
}

func compare(op string, l, r localValue) (localValue, bool) {
	if l.kind == localTruthKind || r.kind == localTruthKind {
		return localValue{}, false
	}
	lf, rf := l.family(), r.family()
	if lf == famOther || rf == famOther || lf != famAny && rf != famAny && lf != rf {
		return localValue{}, false
	}
	if l.kind == localNullKind || r.kind == localNullKind {
		return localValue{kind: localTruthKind, t: truthUnknown}, true
	}
	var c int
	if l.kind == localIntKind {
		switch {
		case l.i < r.i:
			c = -1
		case l.i > r.i:
			c = 1
		}
	} else {
		c = comparePadded(l.s, r.s)
	}
	switch op {
	case "=":
		return truthOf(c == 0), true
	case "<>":
		return truthOf(c != 0), true
	case "<":
		return truthOf(c < 0), true
	case ">":
		return truthOf(c > 0), true
	case "<=":
		return truthOf(c <= 0), true
	default:
		return truthOf(c >= 0), true
	}
}

// castType removes a COLLATE clause, which CAST does not accept.
func castType(typeText string) string {
	upper := strings.ToUpper(typeText)
	if i := strings.Index(upper, " COLLATE "); i >= 0 {
		return strings.TrimSpace(typeText[:i])
	}
	return strings.TrimSpace(typeText)
}

// isBlobType reports a BLOB type text.
func isBlobType(typeText string) bool {
	fields := strings.Fields(strings.ToUpper(typeText))
	return len(fields) > 0 && fields[0] == "BLOB"
}

// normalizeType renders a type text in one canonical spelling for equality.
func normalizeType(typeText string) string {
	text := strings.Join(strings.Fields(strings.ToUpper(castType(typeText))), " ")
	for _, pair := range [][2]string{{" (", "("}, {"( ", "("}, {" )", ")"}, {" ,", ","}, {", ", ","}} {
		text = strings.ReplaceAll(text, pair[0], pair[1])
	}
	return text
}

// boundStatement is a node statement ready to execute.
type boundStatement struct {
	stmt *PreparedStatement
	text string
	args []any
}

// bind prepares text (once per node and run) with params bound to the
// frame's current values. Every non-BLOB variable is written as
// CAST(? AS <declared type>) (R14): the parameter then has the variable's
// type, as in PSQL, and the driver never converts or rejects the held value
// against a narrower context type. BLOB variables stay plain "?"; when the
// server cannot type one (SQLCODE -804) it is cast too.
func (r *lpRun) bind(f *lpFrame, node *LineProfileNode, text string, params []string) (boundStatement, error) {
	return r.bindParams(f, node, text, params, false)
}

// bindParams is bind; plain keeps every parameter a plain "?", for
// EXECUTE PROCEDURE, whose inputs DSQL does not accept as expressions.
func (r *lpRun) bindParams(f *lpFrame, node *LineProfileNode, text string, params []string, plain bool) (boundStatement, error) {
	b, err := r.bindQuiet(f, node, text, params, plain)
	if err != nil {
		node.Metrics.Errors++
	}
	return b, err
}

// bindQuiet is bindParams without counting a prepare failure as a node
// error.
func (r *lpRun) bindQuiet(f *lpFrame, node *LineProfileNode, text string, params []string, plain bool) (boundStatement, error) {
	args := make([]any, len(params))
	typed := make([]string, len(params))
	allTyped := make([]string, len(params))
	for i, name := range params {
		v, ok := f.vars[name]
		if !ok {
			return boundStatement{}, fmt.Errorf("interbase: line profile: unknown variable %s", name)
		}
		args[i], allTyped[i] = v.value, castType(v.typ.Text)
		if plain {
			allTyped[i] = ""
		} else if !isBlobType(v.typ.Text) {
			typed[i] = allTyped[i]
		}
	}
	if err := checkBlobArgs(args); err != nil {
		return boundStatement{}, err
	}
	key := fmt.Sprintf("%p\x00%d\x00%d\x00%s", f.meta, f.level, node.ID, text)
	if stmt, ok := r.statements[key]; ok {
		return boundStatement{stmt: stmt.stmt, text: stmt.text, args: args}, nil
	}
	sent := castParams(text, typed)
	stmt, err := r.prepare(sent)
	var nativeErr *Error
	if err != nil && sent != castParams(text, allTyped) && errors.As(err, &nativeErr) && nativeErr.SQLCode == -804 {
		sent = castParams(text, allTyped)
		stmt, err = r.prepare(sent)
	}
	if err != nil {
		return boundStatement{}, err
	}
	r.statements[key] = cachedStatement{stmt: stmt, text: sent}
	return boundStatement{stmt: stmt, text: sent, args: args}, nil
}

type cachedStatement struct {
	stmt *PreparedStatement
	text string
}

// lpSelectText is the statement text chosen for a SELECT … INTO or
// FOR SELECT node, and the columns it converts to their targets' types.
type lpSelectText struct {
	text string
	cast []bool
}

// bindSelect binds a SELECT … INTO or FOR SELECT node (R18). A target whose
// conversion is neither identical nor locally exact is converted by the
// statement itself, as CAST(<item> AS <target type>), when the statement
// shape allows it: no conversion call per row. Otherwise, or when that
// statement does not prepare, the plain statement is used and such values
// are converted one by one. cast reports columns that already have their
// target's type.
func (r *lpRun) bindSelect(f *lpFrame, n *psql.Node, node *LineProfileNode) (boundStatement, []bool, error) {
	key := fmt.Sprintf("%p\x00%d", f.meta, n.ID)
	if chosen, ok := r.selects[key]; ok {
		b, err := r.bind(f, node, chosen.text, n.SQL.Params)
		return b, chosen.cast, err
	}
	base, runsAhead := r.procedureCursorText(f, n, node)
	b, err := r.bind(f, node, base, n.SQL.Params)
	if err != nil {
		return b, nil, err
	}
	if runsAhead != nil {
		r.result.NativeCallSites = append(r.result.NativeCallSites, *runsAhead)
	}
	if r.selects == nil {
		r.selects = make(map[string]lpSelectText)
	}
	r.selects[key] = lpSelectText{text: base}
	outputs := b.stmt.Outputs()
	types := make([]string, len(outputs))
	needed := false
	for i, name := range n.Into {
		if target, ok := f.vars[name]; ok && i < len(outputs) && !r.convertsWithoutServer(outputs[i], target) {
			types[i], needed = castType(target.typ.Text), true
		}
	}
	if !needed {
		return b, nil, nil
	}
	text, cast := castSelectItems(base, types)
	if cast == nil {
		return b, nil, nil
	}
	castBound, err := r.bindQuiet(f, node, text, n.SQL.Params, false)
	if err != nil {
		return b, nil, nil
	}
	r.selects[key] = lpSelectText{text: text, cast: cast}
	return castBound, cast, nil
}

// convertsWithoutServer reports whether assigning a value of the source
// column to target never needs a conversion call, or needs one only for a
// value the conversion rejects: identical types, NONE text into a NONE text
// target (R9; blank padding beyond the target length is cut locally),
// unscaled integers into SMALLINT/INTEGER, and BLOB targets, which CAST
// cannot produce.
func (r *lpRun) convertsWithoutServer(source OutputDescriptor, target *lpVar) bool {
	if isBlobType(target.typ.Text) {
		return true
	}
	if sourceType := r.descriptorType(source); sourceType != "" && r.declaredType(sourceType) == r.declaredType(target.typ.Text) {
		return true
	}
	switch source.Kind {
	case "CHAR", "VARCHAR":
		return source.SubType&0xff == 0 && target.stringsLocal
	case "SMALLINT", "INTEGER", "BIGINT":
		return source.Scale == 0 && (target.typ.Family == psql.TypeSmallint || target.typ.Family == psql.TypeInteger)
	}
	return false
}

// prepare prepares a statement; its time is overhead, never node time. A
// failure is the emulator's own (PSQL compiled the routine), so WHEN
// handlers never catch it.
// The server's work for a prepare (metadata loads, catalog reads) is kept
// out of node counters by an activity snapshot right after it, success or
// not; its delta is OverheadActivity.
func (r *lpRun) prepare(text string) (*PreparedStatement, error) {
	start := time.Now()
	stmt, err := r.tx.Prepare(r.ctx, text)
	r.result.Prepare += time.Since(start)
	if snapErr := r.snapshot(nil); snapErr != nil {
		if stmt != nil {
			snapErr = errors.Join(snapErr, stmt.Close())
		}
		return nil, fatal(errors.Join(err, snapErr))
	}
	return stmt, fatal(err)
}

// fixedStatement prepares a statement without variable parameters once per
// run.
func (r *lpRun) fixedStatement(node *LineProfileNode, text string) (*PreparedStatement, error) {
	key := "\x00fixed\x00" + text
	if stmt, ok := r.statements[key]; ok {
		return stmt.stmt, nil
	}
	stmt, err := r.prepare(text)
	if err != nil {
		if node != nil {
			node.Metrics.Errors++
		}
		return nil, err
	}
	r.statements[key] = cachedStatement{stmt: stmt, text: text}
	return stmt, nil
}

// queryRows executes b and fetches at most limit rows, closing the cursor
// on every path. Each execute and fetch is one measured server call. When
// the limit is reached, the same measured call probes for the end of the
// result, so a result of at most limit rows closes its cursor inside the
// node's measurement rather than in a separate call.
func (r *lpRun) queryRows(node *LineProfileNode, b boundStatement, limit int) (rows [][]Cell, err error) {
	node.SQL = b.text
	var cursor *Cursor
	if err := r.serverCall(node, func() (e error) {
		cursor, e = b.stmt.Query(r.ctx, b.args...)
		return e
	}); err != nil {
		return nil, err
	}
	exhausted := false
	defer func() { err = errors.Join(err, r.closeCursor(cursor, exhausted)) }()
	for !exhausted && len(rows) < limit {
		if err := r.serverCall(node, func() error {
			hasRow, err := cursor.Next(r.ctx)
			if err != nil || !hasRow {
				exhausted = err == nil
				return err
			}
			row, err := cursor.Row()
			if err != nil {
				return err
			}
			rows = append(rows, row)
			if len(rows) < limit {
				return nil
			}
			more, err := cursor.Next(r.ctx)
			exhausted = err == nil && !more
			return err
		}); err != nil {
			return nil, err
		}
	}
	return rows, nil
}

// evalValue evaluates e for assignment to target (spec 6.2/6.3): locally
// when the value and its conversion are exact, otherwise as
// SELECT CAST((<expr'>) AS <target type>) FROM RDB$DATABASE.
func (r *lpRun) evalValue(f *lpFrame, node *LineProfileNode, e *psql.Expr, target *lpVar) (any, error) {
	if e.Local != nil {
		if v, ok := evalLocalValue(e.Local, f.localScope(r.dbNone)); ok {
			if out, ok := convertLocal(v, target.typ, target.stringsLocal); ok {
				node.Local++
				return out, nil
			}
		}
	}
	text := "SELECT CAST((" + e.ServerText + ") AS " + castType(target.typ.Text) + ") FROM RDB$DATABASE"
	if isBlobType(target.typ.Text) {
		// DSQL cannot CAST to BLOB. A BLOB variable copies its content; any
		// other value is evaluated as is and converted by blobValue.
		if v, ok := e.Local.(psql.LocalVar); ok {
			if source, ok := f.vars[v.Name]; ok && isBlobType(source.typ.Text) {
				node.Local++
				return source.value, nil
			}
		}
		text = "SELECT (" + e.ServerText + ") FROM RDB$DATABASE"
	}
	b, err := r.bind(f, node, text, e.Params)
	if err != nil {
		return nil, err
	}
	rows, err := r.queryRows(node, b, 1)
	if err != nil {
		return nil, err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return nil, errors.New("interbase: line profile: value evaluation returned no value")
	}
	value, err := r.holdValue(cellValue(rows[0][0]))
	if err != nil || !isBlobType(target.typ.Text) {
		return value, err
	}
	return r.blobValue(value, r.descriptorType(b.stmt.Outputs()[0]))
}

// evalCond evaluates an IF/WHILE condition; unknown is false, as in PSQL.
func (r *lpRun) evalCond(f *lpFrame, node *LineProfileNode, e *psql.Expr) (bool, error) {
	if e.Local != nil {
		if t, ok := evalLocalCondition(e.Local, f.localScope(r.dbNone)); ok {
			node.Local++
			return t == truthTrue, nil
		}
	}
	b, err := r.bind(f, node, "SELECT 1 FROM RDB$DATABASE WHERE "+e.ServerText, e.Params)
	if err != nil {
		return false, err
	}
	rows, err := r.queryRows(node, b, 1)
	if err != nil {
		return false, err
	}
	return len(rows) == 1, nil
}

// lpBadBlob is what PSQL holds after assigning a non-zero integer to a
// BLOB variable: the integer taken as a BLOB id, which no BLOB has. Any use
// of it as a statement parameter fails as natively (errBlobNotFound).
type lpBadBlob struct{}

// blobValue is the value PSQL holds after assigning value, of type
// sourceType, to a BLOB variable; DSQL cannot CAST to BLOB. As verified
// natively (dialects 1 and 3): NULL stays NULL; a BLOB is copied; text
// (CHAR blank padding included) becomes the content; an integer 0 an empty
// BLOB and any other integer an id no BLOB has (lpBadBlob); a floating-point
// or scaled value fails with the internal error (SQLCODE -902) and a date or
// time with a conversion error (SQLCODE -413), both caught by WHEN like any
// server error. Other types are refused as unsupported.
func (r *lpRun) blobValue(value any, sourceType string) (any, error) {
	if value == nil {
		return nil, nil
	}
	if _, bad := value.(lpBadBlob); bad || isBlobType(sourceType) {
		return value, nil
	}
	normalized := normalizeType(sourceType)
	word := normalized
	if i := strings.IndexAny(word, " ("); i >= 0 {
		word = word[:i]
	}
	switch word {
	case "CHAR", "CHARACTER", "VARCHAR":
		return value, nil
	case "SMALLINT", "INTEGER", "INT", "BIGINT", "NUMERIC", "DECIMAL":
		if i, ok := value.(int64); ok && !strings.Contains(normalized, ",") || ok && strings.HasSuffix(normalized, ",0)") {
			if i == 0 {
				return "", nil
			}
			return lpBadBlob{}, nil
		}
		return nil, errBlobInternal()
	case "FLOAT", "DOUBLE":
		return nil, errBlobInternal()
	case "DATE", "TIME", "TIMESTAMP":
		return nil, errBlobConversion(value)
	}
	return nil, fatal(fmt.Errorf("interbase: line profile: assigning a %s value to a BLOB variable is not supported", sourceType))
}

// errBlobInternal is the error PSQL raises assigning a floating-point or
// scaled value to a BLOB variable.
func errBlobInternal() error {
	const bugCheck = 335544384 // isc_bug_check
	return &Error{Operation: "line profile BLOB assignment", SQLCode: -902, NativeCode: bugCheck,
		Message: "internal error", gdsCodes: []int64{bugCheck}}
}

// errBlobConversion is the error PSQL raises assigning a date or time value
// to a BLOB variable. The server writes the value in its own format; the
// message here may format it differently.
func errBlobConversion(value any) error {
	const convertError = 335544334 // isc_convert_error
	text := fmt.Sprint(value)
	if t, ok := value.(time.Time); ok {
		text = t.Format("2006-01-02 15:04:05.0000")
	}
	return &Error{Operation: "line profile BLOB assignment", SQLCode: -413, NativeCode: convertError,
		Message: fmt.Sprintf("conversion error from string %q", text), gdsCodes: []int64{convertError}}
}

// errBlobNotFound is the error PSQL raises using a BLOB variable that holds
// an integer taken as a BLOB id (lpBadBlob).
func errBlobNotFound() error {
	const random = 335544382 // isc_random
	return &Error{Operation: "line profile BLOB use", SQLCode: -901, NativeCode: random,
		Message: "BLOB not found", gdsCodes: []int64{random}}
}

// checkBlobArgs fails as natively when a parameter value is an lpBadBlob.
func checkBlobArgs(args []any) error {
	for _, arg := range args {
		if _, bad := arg.(lpBadBlob); bad {
			return errBlobNotFound()
		}
	}
	return nil
}

// assign stores value, read from a result column described by source, in
// the target variable with the PSQL assignment conversion (R14): kept
// unchanged when the statement already converted it (exact, R18); otherwise
// as store does.
func (r *lpRun) assign(f *lpFrame, node *LineProfileNode, name string, value any, source OutputDescriptor, exact bool) error {
	target, ok := f.vars[name]
	if !ok {
		return fmt.Errorf("interbase: line profile: unknown target %s", name)
	}
	value, err := r.holdValue(value)
	if err != nil {
		return err
	}
	if exact && source.Kind != "BLOB" {
		// Converted by the statement's own CAST: neither a local
		// conversion nor a server call of its own, so not counted.
		target.value = value
		return nil
	}
	local, isLocal := columnLocalValue(value, source)
	return r.store(node, target, value, r.descriptorType(source), local, isLocal)
}

// assignTyped stores value, held in a variable of type sourceType (a
// stepped-into routine's output), in the target variable as store does.
func (r *lpRun) assignTyped(f *lpFrame, node *LineProfileNode, name string, value any, sourceType string) error {
	target, ok := f.vars[name]
	if !ok {
		return fmt.Errorf("interbase: line profile: unknown target %s", name)
	}
	source := r.newVar(sourceType)
	source.value = value
	local, isLocal := localValueOf(source)
	return r.store(node, target, value, castType(sourceType), local, isLocal)
}

// store assigns a held value of type sourceType ("" when unknown; local is
// its local form when isLocal) to target with the PSQL assignment
// conversion (R14): BLOB to BLOB copies the content; kept unchanged when the
// source type equals the target's declared type; locally when exact;
// otherwise converted on the server as
// CAST(CAST(? AS <source type>) AS <target type>), where the inner type is
// how the value is held, so rounding, truncation and overflow are the
// server's. DSQL cannot CAST to BLOB: a value assigned to a BLOB variable
// is converted by blobValue.
func (r *lpRun) store(node *LineProfileNode, target *lpVar, value any, sourceType string, local localValue, isLocal bool) error {
	if isBlobType(sourceType) && isBlobType(target.typ.Text) {
		// Character sets apply on read.
		node.Local++
		target.value = value
		return nil
	}
	if isLocal {
		if out, ok := convertLocal(local, target.typ, target.stringsLocal); ok {
			node.Local++
			target.value = out
			return nil
		}
	}
	targetType := castType(target.typ.Text)
	if sourceType != "" && r.declaredType(sourceType) == r.declaredType(targetType) {
		node.Local++
		target.value = value
		return nil
	}
	if isBlobType(targetType) {
		held, err := r.blobValue(value, sourceType)
		if err != nil {
			return err
		}
		node.Local++
		target.value = held
		return nil
	}
	text := "SELECT CAST(? AS " + targetType + ") FROM RDB$DATABASE"
	if sourceType != "" && !isBlobType(sourceType) {
		text = "SELECT CAST(CAST(? AS " + sourceType + ") AS " + targetType + ") FROM RDB$DATABASE"
	}
	if err := checkBlobArgs([]any{value}); err != nil {
		return err
	}
	stmt, err := r.fixedStatement(node, text)
	if err != nil {
		return err
	}
	rows, err := r.queryRows(node, boundStatement{stmt: stmt, text: text, args: []any{value}}, 1)
	if err != nil {
		return err
	}
	if len(rows) != 1 || len(rows[0]) != 1 {
		return errors.New("interbase: line profile: conversion returned no value")
	}
	target.value, err = r.holdValue(cellValue(rows[0][0]))
	return err
}

// declaredType normalizes a declared type, adding the database default
// character set (NONE when unset) to a text type declared without one, so
// types written with and without the default compare equal.
func (r *lpRun) declaredType(typeText string) string {
	normalized := normalizeType(typeText)
	if isStringFamily(psql.ParseTypeText(typeText)) || strings.HasPrefix(normalized, "CHAR") || strings.HasPrefix(normalized, "VARCHAR") {
		if !strings.Contains(normalized, "CHARACTER SET") {
			charset := strings.ToUpper(r.plan.dbCharset)
			if charset == "" {
				charset = "NONE"
			}
			normalized += " CHARACTER SET " + charset
		}
	}
	return normalized
}

// columnLocalValue maps a result value to the local subset: integers, NULL
// and strings of character set NONE.
func columnLocalValue(value any, source OutputDescriptor) (localValue, bool) {
	switch v := value.(type) {
	case nil:
		return localValue{kind: localNullKind, fam: famAny}, true
	case int64:
		if source.Scale == 0 {
			return localValue{kind: localIntKind, i: v}, true
		}
	case string:
		if (source.Kind == "CHAR" || source.Kind == "VARCHAR") && source.SubType&0xff == 0 {
			return localValue{kind: localStringKind, s: []byte(v)}, true
		}
	}
	return localValue{}, false
}

// descriptorType renders the type a result column's value is held in, for
// CAST: text with its character set, integers (scaled as NUMERIC), floating
// point, date/time kinds and BLOB subtypes. It returns "" for kinds that
// cannot be written as a type.
func (r *lpRun) descriptorType(d OutputDescriptor) string {
	switch d.Kind {
	case "CHAR", "VARCHAR":
		charset, known := r.plan.charsets[d.SubType&0xff]
		length := d.Length
		if known {
			length /= charset.bytesPerChar
		}
		if length < 1 {
			length = 1
		}
		text := fmt.Sprintf("%s(%d)", d.Kind, length)
		if known {
			text += " CHARACTER SET " + charset.name
		}
		return text
	case "SMALLINT", "INTEGER", "BIGINT":
		if d.Scale == 0 {
			if d.Kind == "BIGINT" {
				return "NUMERIC(18,0)"
			}
			return d.Kind
		}
		// A scaled integer is held as an exact decimal string. Dialect 1
		// NUMERIC above precision 9 is DOUBLE, so its widest exact
		// scaled-integer type is NUMERIC(9,s).
		precision := 18
		if r.plan.dialect == 1 {
			precision = 9
		}
		return fmt.Sprintf("NUMERIC(%d,%d)", precision, -d.Scale)
	case "FLOAT", "DOUBLE PRECISION", "DATE", "TIME", "BOOLEAN":
		return d.Kind
	case "TIMESTAMP":
		if r.plan.dialect == 1 {
			return "DATE"
		}
		return "TIMESTAMP"
	case "BLOB":
		return fmt.Sprintf("BLOB SUB_TYPE %d", d.SubType)
	}
	return ""
}
