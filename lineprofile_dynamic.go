package interbase

import (
	"errors"
	"fmt"
	"hash/fnv"
	"strings"
	"time"

	"interbase-go/psql"
)

// EXECUTE STATEMENT and FOR EXECUTE STATEMENT (spec 12.2) as InterBase runs
// them, verified natively in dialects 1 and 3:
//
//   - The text is any value expression; NULL or a blank text fails with
//     SQLCODE -104. It runs in the routine's transaction and sees its work.
//   - Every error of the dynamic statement is reported as
//     isc_exec_stmt2_execute: a prepare error is followed by the statement's
//     own status codes, with its SQLCODE; an execution error (a conversion
//     error of an INSERT, say) by nothing, with SQLCODE -204. WHEN handlers
//     catch it, and it undoes the handling block's earlier work, as a
//     SELECT error does.
//     COMMIT and ROLLBACK fail with isc_exec_stmt_disallow, EXECUTE PROCEDURE
//     and SET TRANSACTION with SQLCODE -204, a "?" parameter with -804.
//   - Statements InterBase runs but the emulator refuses (Ruling R26 for
//     DDL; errNotRun): DDL, SET GENERATOR, SAVEPOINT, RELEASE SAVEPOINT and
//     ROLLBACK [WORK] TO SAVEPOINT. The run fails at the line.
//   - Without INTO a SELECT is opened and closed, never fetched.
//   - With INTO the first row is assigned and a second one fetched (no
//     singleton error); a FOR EXECUTE STATEMENT loop ends at the end of the
//     rows. A fetch error is ignored: the targets keep their values, the
//     loop ends, and execution goes on.
//   - INTO copies each column into its target without any conversion, so a
//     column of another type yields garbage. The emulator assigns only the
//     columns InterBase copies exactly (lpRun.copiesExactly) and NULLs, and
//     fails the run on any other column. A column count that differs from
//     the targets, or INTO for a statement without rows, makes InterBase
//     raise an error whose code it does not report; the emulator fails the
//     run there too.
const (
	// lpDynamicLimit is the number of prepared texts kept per node.
	lpDynamicLimit = 32
	// lpDynamicTextType is the type a statement text is evaluated to.
	lpDynamicTextType = "VARCHAR(32765) CHARACTER SET NONE"
	// lpDynamicKeyPrefix starts the statements keys of dynamic texts.
	lpDynamicKeyPrefix = "\x00dynamic\x00"

	gdsExecStmtExecute  = 335544853 // isc_exec_stmt2_execute
	gdsExecStmtDisallow = 335544857 // isc_exec_stmt_disallow
	gdsDSQLError        = 335544569 // isc_dsql_error
)

// errExecuteStatement is the error InterBase raises when a dynamic
// statement fails: isc_exec_stmt2_execute, then the statement's own error
// (inner, nil when there is none) with its SQLCODE.
func errExecuteStatement(text string, sqlCode int, inner *Error) error {
	e := &Error{
		Operation:  "line profile EXECUTE STATEMENT",
		SQLCode:    sqlCode,
		NativeCode: gdsExecStmtExecute,
		Message:    "Could not execute statement : " + text,
		gdsCodes:   []int64{gdsExecStmtExecute},
	}
	if inner != nil {
		e.SQLCode = inner.SQLCode
		e.Message += "\n" + inner.Message
		e.gdsCodes = append(e.gdsCodes, inner.gdsCodes...)
		e.exceptionNumber, e.hasException = inner.exceptionNumber, inner.hasException
	}
	return e
}

// errDynamicDSQL is a DSQL error of a dynamic text that the emulator
// detects itself (a blank text, a parameter marker).
func errDynamicDSQL(text string, sqlCode int) error {
	return errExecuteStatement(text, sqlCode, &Error{SQLCode: sqlCode, NativeCode: gdsDSQLError,
		Message: fmt.Sprintf("Dynamic SQL Error\nSQL error code = %d", sqlCode), gdsCodes: []int64{gdsDSQLError}})
}

// dynamicError shapes a server error of a dynamic statement's prepare as
// InterBase reports it; other errors are returned unchanged.
func (r *lpRun) dynamicError(text string, err error) error {
	var nativeErr *Error
	if !r.catchable(err) || !errors.As(err, &nativeErr) {
		return err
	}
	return errExecuteStatement(text, 0, nativeErr)
}

// dynamicExecError shapes a server error of a dynamic statement's
// execution as InterBase reports it: SQLCODE -204 without the statement's
// own codes. Other errors are returned unchanged.
func (r *lpRun) dynamicExecError(text string, err error) error {
	if !r.catchable(err) {
		return err
	}
	return errExecuteStatement(text, -204, nil)
}

// dynamicText evaluates the statement text of node n and records it.
func (r *lpRun) dynamicText(f *lpFrame, node *LineProfileNode, e *psql.Expr) (string, error) {
	value, err := r.evalValue(f, node, e, r.newVar(lpDynamicTextType))
	if err != nil {
		return "", err
	}
	var text string
	switch v := value.(type) {
	case nil:
	case string:
		text = v
	case []byte:
		text = string(v)
	default:
		return "", fatal(fmt.Errorf("interbase: line profile: EXECUTE STATEMENT text evaluated to %T", value))
	}
	node.DynamicSQL = text
	if r.dynamicTexts == nil {
		r.dynamicTexts = make(map[*LineProfileNode]map[uint64]struct{})
	}
	seen := r.dynamicTexts[node]
	if seen == nil {
		seen = make(map[uint64]struct{})
		r.dynamicTexts[node] = seen
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(text))
	seen[h.Sum64()] = struct{}{}
	node.DynamicTexts = len(seen)
	if strings.TrimSpace(text) == "" {
		node.Metrics.Errors++
		return "", errDynamicDSQL(text, -104)
	}
	return text, nil
}

// dynamicStatement returns the prepared statement of text for node n,
// preparing it on first use. Each node keeps at most lpDynamicLimit texts
// prepared; the least recently used one is closed beyond that. A FOR
// EXECUTE STATEMENT over a procedure is prepared FOR UPDATE when it needs
// row-by-row fetches, as a FOR SELECT is (procedureCursorText).
func (r *lpRun) dynamicStatement(f *lpFrame, n *psql.Node, node *LineProfileNode, text string) (cachedStatement, error) {
	nodeKey := fmt.Sprintf("%s%p\x00%d\x00%d", lpDynamicKeyPrefix, f.meta, f.level, n.ID)
	key := nodeKey + "\x00" + text
	if r.dynamic == nil {
		r.dynamic = make(map[string][]string)
	}
	keys := r.dynamic[nodeKey]
	if cached, ok := r.statements[key]; ok {
		for i, k := range keys {
			if k == key {
				r.dynamic[nodeKey] = append(append(keys[:i:i], keys[i+1:]...), key)
				break
			}
		}
		return cached, nil
	}
	var stmt *PreparedStatement
	sent, runsAhead := text, ""
	if n.Kind == psql.NodeForExecStmt {
		procedure, rowByRow, err := r.dynamicRowByRow(f, n, text)
		if err != nil {
			return cachedStatement{}, err
		}
		if rowByRow {
			if stmt, err = r.prepareDynamic(text + " FOR UPDATE"); err == nil {
				sent += " FOR UPDATE"
			} else if errors.As(err, new(*lpFatalError)) {
				return cachedStatement{}, err
			} else {
				runsAhead = procedure
			}
		}
	}
	if stmt == nil {
		var err error
		if stmt, err = r.prepareDynamic(text); err != nil {
			node.Metrics.Errors++
			return cachedStatement{}, err
		}
	}
	// A statement leaves r.statements only once closed, so that
	// closeStatements still closes it when its own close fails.
	cached := cachedStatement{stmt: stmt, text: sent}
	r.statements[key] = cached
	var refused error
	switch {
	case stmt.kind == "ddl":
		// Ruling R26: DDL is never run (its effects on metadata are not
		// emulated), whatever InterBase would do.
		refused = errNotRun(text)
	case stmt.kind == "procedure":
		refused = errExecuteStatement(text, -204, nil)
	case len(stmt.Inputs()) > 0:
		refused = errDynamicDSQL(text, -804)
	}
	if refused != nil {
		node.Metrics.Errors++
		if err := r.closeDynamic(stmt); err != nil {
			return cachedStatement{}, fatal(errors.Join(err, refused))
		}
		delete(r.statements, key)
		return cachedStatement{}, refused
	}
	if runsAhead != "" {
		r.dynamicRunsAhead(f, n, runsAhead)
	}
	keys = append(keys, key)
	if len(keys) > lpDynamicLimit {
		oldest := keys[0]
		keys = keys[1:]
		r.dynamic[nodeKey] = keys
		if err := r.closeDynamic(r.statements[oldest].stmt); err != nil {
			return cachedStatement{}, fatal(err)
		}
		delete(r.statements, oldest)
	}
	r.dynamic[nodeKey] = keys
	return cached, nil
}

// errNotRun refuses a dynamic text the line profiler does not run; WHEN
// handlers cannot catch it.
func errNotRun(text string) error {
	return fatal(fmt.Errorf("interbase: line profile: EXECUTE STATEMENT %q: the line profiler does not run this kind of statement", text))
}

// prepareDynamic prepares a dynamic text; its time and counters are Prepare
// overhead, as for every prepare. Unlike the emulator's own statements, a
// text that does not prepare is an error of the routine, which WHEN
// handlers can catch.
func (r *lpRun) prepareDynamic(text string) (*PreparedStatement, error) {
	start := time.Now()
	stmt, err := r.tx.Prepare(r.ctx, text)
	r.result.Prepare += time.Since(start)
	if snapErr := r.snapshot(nil); snapErr != nil {
		if stmt != nil {
			snapErr = errors.Join(snapErr, stmt.Close())
		}
		return nil, fatal(errors.Join(err, snapErr))
	}
	switch {
	case err == nil:
		return stmt, nil
	case errors.Is(err, errPreparedStatementUnsupported):
		return nil, unsupportedDynamic(text)
	case r.catchable(err):
		return nil, r.dynamicError(text, err)
	}
	return nil, fatal(err)
}

// unsupportedDynamic is the outcome of a text the driver does not prepare:
// the error InterBase raises for COMMIT, ROLLBACK and SET TRANSACTION.
// Statements InterBase runs are refused (errNotRun): SET GENERATOR is not
// transactional, and SAVEPOINT, RELEASE SAVEPOINT and ROLLBACK [WORK] TO
// SAVEPOINT would interfere with the emulator's own savepoints.
func unsupportedDynamic(text string) error {
	var words []string
	for _, t := range topLevelTokens(text) {
		if !t.comma && len(words) < 3 {
			words = append(words, strings.ToUpper(text[t.start:t.end]))
		}
	}
	words = append(words, "", "", "")
	toSavepoint := words[1] == "TO" || words[1] == "WORK" && words[2] == "TO"
	switch {
	case words[0] == "COMMIT", words[0] == "ROLLBACK" && !toSavepoint:
		return &Error{Operation: "line profile EXECUTE STATEMENT", SQLCode: -204, NativeCode: gdsExecStmtDisallow,
			Message: "Sql statement not allowed in EXECUTE STATEMENT : " + text, gdsCodes: []int64{gdsExecStmtDisallow}}
	case words[0] == "SET" && words[1] == "TRANSACTION":
		return errExecuteStatement(text, -204, nil)
	}
	return errNotRun(text)
}

// closeDynamic closes a dynamic statement; the close is Prepare overhead.
func (r *lpRun) closeDynamic(stmt *PreparedStatement) error {
	start := time.Now()
	err := stmt.Close()
	r.result.Prepare += time.Since(start)
	return errors.Join(err, r.snapshot(nil))
}

// dynamicRowByRow reports whether FOR EXECUTE STATEMENT node n needs
// row-by-row fetches for text, as procedureCursorWrites decides for a FOR
// SELECT: the text selects from a procedure, and that procedure or the loop
// body may write. A procedure first met in a dynamic text is not analyzed,
// so it is assumed to write.
func (r *lpRun) dynamicRowByRow(f *lpFrame, n *psql.Node, text string) (string, bool, error) {
	if r.opts.ReadOnly || hasForUpdate(text) {
		return "", false, nil
	}
	refs := psql.ExtractStatementRefs(text)
	if refs.Kind != "select" {
		return "", false, nil
	}
	calls := make(map[string]bool, len(refs.Calls))
	for _, name := range refs.Calls {
		calls[name] = true
	}
	quoted := make(map[string]bool, len(refs.QuotedSources))
	for _, name := range refs.QuotedSources {
		quoted[name] = true
	}
	var procedures []string
	for _, name := range refs.Sources {
		isProcedure := calls[name]
		if !isProcedure {
			var err error
			if isProcedure, err = r.isProcedure(name, quoted[name]); err != nil {
				return "", false, err
			}
		}
		if isProcedure {
			procedures = append(procedures, name)
		}
	}
	if len(procedures) == 0 {
		return "", false, nil
	}
	if r.bodyWrites(f.meta, n.Children[0]) {
		return procedures[0], true, nil
	}
	for _, name := range procedures {
		if callee := r.plan.procs[name]; callee == nil || !callee.readsOnly {
			return name, true, nil
		}
	}
	return "", false, nil
}

// isProcedure reports whether a bare source name of a dynamic text is a
// procedure. Names unknown to the analysis are looked up in the catalog
// once per run; the lookup is Prepare overhead.
func (r *lpRun) isProcedure(name string, quoted bool) (bool, error) {
	if _, ok := r.plan.procs[name]; ok {
		return true, nil
	}
	if known, ok := r.procedureNames[name]; ok {
		return known, nil
	}
	start := time.Now()
	refs, err := r.plan.procedureRefs(r.ctx, r.tx, []lookupName{{name: name, quoted: quoted}})
	r.result.Prepare += time.Since(start)
	if snapErr := r.snapshot(nil); err == nil {
		err = snapErr
	}
	if err != nil {
		return false, fatal(err)
	}
	if r.procedureNames == nil {
		r.procedureNames = make(map[string]bool)
	}
	r.procedureNames[name] = len(refs) > 0
	return len(refs) > 0, nil
}

// dynamicRunsAhead lists a FOR EXECUTE STATEMENT over a procedure that
// could not be prepared FOR UPDATE, once per routine, line and procedure.
func (r *lpRun) dynamicRunsAhead(f *lpFrame, n *psql.Node, procedure string) {
	site := LineProfileCallSite{Routine: f.meta.name, Line: n.Line, Procedure: procedure, Reason: lpReasonRunsAhead}
	for _, existing := range r.result.NativeCallSites {
		if existing == site {
			return
		}
	}
	r.result.NativeCallSites = append(r.result.NativeCallSites, site)
}

// writesDynamically reports the statements that may write without a SQL
// text of their own.
func writesDynamically(kind psql.NodeKind) bool {
	switch kind {
	case psql.NodeExecProc, psql.NodeExecStmt, psql.NodeForExecStmt:
		return true
	}
	return false
}

// execStatement runs EXECUTE STATEMENT <text> [INTO <targets>].
func (r *lpRun) execStatement(f *lpFrame, n *psql.Node, node *LineProfileNode) (err error) {
	text, err := r.dynamicText(f, node, n.Value)
	if err != nil {
		return err
	}
	cached, err := r.dynamicStatement(f, n, node, text)
	if err != nil {
		return err
	}
	node.SQL = cached.text
	stmt := cached.stmt
	if n.Into == nil && !stmt.returnsRows {
		var affected int64
		if err := r.serverCall(node, func() (e error) {
			affected, e = stmt.Exec(r.ctx)
			return e
		}); err != nil {
			return r.dynamicExecError(text, err)
		}
		node.Metrics.Rows += affected
		return nil
	}
	if n.Into != nil {
		if err := r.checkDynamicColumns(n, stmt); err != nil {
			return err
		}
	}
	var cursor *Cursor
	if err := r.serverCall(node, func() (e error) {
		cursor, e = stmt.Query(r.ctx)
		return e
	}); err != nil {
		return r.dynamicExecError(text, err)
	}
	exhausted := false
	defer func() { err = errors.Join(err, r.closeCursor(cursor, exhausted)) }()
	if n.Into == nil {
		// A SELECT without INTO is opened and closed, never fetched.
		return nil
	}
	var first []Cell
	for i := 0; i < 2; i++ {
		row, state, err := r.dynamicFetch(node, cursor)
		if err != nil {
			return err
		}
		if state != lpFetchRow {
			exhausted = state == lpFetchEnd
			break
		}
		if i == 0 {
			first = row
		}
	}
	if first == nil {
		return nil
	}
	return r.assignDynamic(f, n.Into, first, stmt.Outputs())
}

// forExecStatement runs FOR EXECUTE STATEMENT <text> INTO <targets> DO
// <stmt>; the cursor is closed on every path.
func (r *lpRun) forExecStatement(f *lpFrame, n *psql.Node, node *LineProfileNode) (sig lpSignal, err error) {
	text, err := r.dynamicText(f, node, n.Value)
	if err != nil {
		return lpContinue, err
	}
	cached, err := r.dynamicStatement(f, n, node, text)
	if err != nil {
		return lpContinue, err
	}
	node.SQL = cached.text
	stmt := cached.stmt
	if err := r.checkDynamicColumns(n, stmt); err != nil {
		return lpContinue, err
	}
	var cursor *Cursor
	if err := r.serverCall(node, func() (e error) {
		cursor, e = stmt.Query(r.ctx)
		return e
	}); err != nil {
		return lpContinue, r.dynamicExecError(text, err)
	}
	exhausted := false
	defer func() { err = errors.Join(err, r.closeCursor(cursor, exhausted)) }()
	outputs := stmt.Outputs()
	for {
		row, state, err := r.dynamicFetch(node, cursor)
		if err != nil || state != lpFetchRow {
			exhausted = state == lpFetchEnd
			return lpContinue, err
		}
		if err := r.assignDynamic(f, n.Into, row, outputs); err != nil {
			return lpContinue, err
		}
		sig, err := r.exec(f, n.Children[0])
		if err != nil || sig == lpExit {
			return sig, err
		}
		if sig == lpBreak {
			return lpContinue, nil
		}
	}
}

type lpFetch uint8

const (
	lpFetchRow     lpFetch = iota
	lpFetchEnd             // end of rows; the cursor is closed
	lpFetchIgnored         // a fetch error, which InterBase ignores
)

// dynamicFetch fetches the next row of a dynamic statement as one measured
// server call. A server error of the fetch is ignored, as natively; it still
// counts in the node's Errors.
func (r *lpRun) dynamicFetch(node *LineProfileNode, cursor *Cursor) ([]Cell, lpFetch, error) {
	var hasRow bool
	var row []Cell
	err := r.serverCall(node, func() (e error) {
		if hasRow, e = cursor.Next(r.ctx); e == nil && hasRow {
			row, e = cursor.Row()
		}
		return e
	})
	switch {
	case err != nil && r.catchable(err):
		return nil, lpFetchIgnored, nil
	case err != nil:
		return nil, lpFetchIgnored, err
	case !hasRow:
		return nil, lpFetchEnd, nil
	}
	node.Metrics.Rows++
	return row, lpFetchRow, nil
}

// checkDynamicColumns refuses the INTO shapes InterBase fails with an error
// it does not report a code for: INTO a statement without rows, or a column
// count other than the target count.
func (r *lpRun) checkDynamicColumns(n *psql.Node, stmt *PreparedStatement) error {
	if !stmt.returnsRows {
		return fatal(errors.New("interbase: line profile: EXECUTE STATEMENT … INTO of a statement without rows: " +
			"InterBase fails it with an error the line profiler does not emulate"))
	}
	if columns := len(stmt.Outputs()); columns != len(n.Into) {
		return fatal(fmt.Errorf("interbase: line profile: EXECUTE STATEMENT text returns %d columns for %d targets: "+
			"InterBase fails it with an error the line profiler does not emulate", columns, len(n.Into)))
	}
	return nil
}

// assignDynamic assigns a dynamic statement's row to targets as InterBase
// does: each column is copied without conversion, so only NULLs and columns
// copied exactly are emulated (copiesExactly); any other column fails the
// run before a target changes.
func (r *lpRun) assignDynamic(f *lpFrame, targets []string, row []Cell, outputs []OutputDescriptor) error {
	vars := make([]*lpVar, len(targets))
	for i, name := range targets {
		target, ok := f.vars[name]
		if !ok {
			return fmt.Errorf("interbase: line profile: unknown target %s", name)
		}
		vars[i] = target
		value := cellValue(row[i])
		if value == nil {
			continue
		}
		exact, err := r.copiesExactly(outputs[i], value, target)
		if err != nil {
			return err
		}
		if !exact {
			source := r.descriptorType(outputs[i])
			if source == "" {
				source = outputs[i].Kind
			}
			return fatal(fmt.Errorf("interbase: line profile: EXECUTE STATEMENT … INTO copies a %s column into %s variable %s "+
				"without conversion; the line profiler does not emulate that copy", source, target.typ.Text, name))
		}
	}
	for i, target := range vars {
		value, err := r.holdValue(cellValue(row[i]))
		if err != nil {
			return err
		}
		target.value = value
	}
	return nil
}

// copiesExactly reports whether InterBase's copy of a value of the source
// column into target keeps the value: the column has the target's storage
// (same kind and scale; CHAR of the same length and character set; BLOB of
// the same subtype), is a VARCHAR of the target's character set whose value
// fits the target's length, or is an unscaled integer of a wider kind whose
// value fits the target (the copy keeps the low bytes, verified natively).
// The target's storage is described by the server once per type, with the
// database default character set for a text type declared without one.
func (r *lpRun) copiesExactly(source OutputDescriptor, value any, target *lpVar) (bool, error) {
	if isBlobType(target.typ.Text) {
		return source.Kind == "BLOB" && source.SubType == blobSubtype(target.typ.Text), nil
	}
	text := "SELECT CAST(NULL AS " + r.declaredType(target.typ.Text) + ") FROM RDB$DATABASE"
	stmt, err := r.fixedStatement(nil, text)
	if err != nil {
		return false, err
	}
	outputs := stmt.Outputs()
	if len(outputs) != 1 {
		return false, fatal(fmt.Errorf("interbase: line profile: describing type %s returned %d columns", target.typ.Text, len(outputs)))
	}
	want := outputs[0]
	if source.Scale != want.Scale {
		return false, nil
	}
	if source.Kind != want.Kind {
		// A wider integer column keeps its low bytes: exact when the value
		// fits the target. The low bytes come first only on a little-endian
		// server (verified natively on x86, which every supported InterBase
		// platform is).
		limit, narrower := lpIntegerLimits[want.Kind]
		i, isInt := value.(int64)
		return narrower && isInt && source.Scale == 0 && lpIntegerLimits[source.Kind] > limit && i >= -limit-1 && i <= limit, nil
	}
	switch source.Kind {
	case "CHAR":
		return source.Length == want.Length && source.SubType&0xff == want.SubType&0xff, nil
	case "VARCHAR":
		if source.SubType&0xff != want.SubType&0xff {
			return false, nil
		}
		switch v := value.(type) {
		case string:
			return len(v) <= want.Length, nil
		case []byte:
			return len(v) <= want.Length, nil
		}
		return source.Length <= want.Length, nil
	}
	return true, nil
}

// lpIntegerLimits are the largest values of the integer storage kinds.
var lpIntegerLimits = map[string]int64{"SMALLINT": 1<<15 - 1, "INTEGER": 1<<31 - 1, "BIGINT": 1<<63 - 1}

// blobSubtype is the subtype of a declared BLOB type: TEXT is 1, no
// SUB_TYPE is 0.
func blobSubtype(typeText string) int {
	fields := strings.Fields(strings.ToUpper(typeText))
	for i := 0; i+1 < len(fields); i++ {
		if fields[i] != "SUB_TYPE" {
			continue
		}
		if fields[i+1] == "TEXT" {
			return 1
		}
		var n int
		if _, err := fmt.Sscanf(fields[i+1], "%d", &n); err == nil {
			return n
		}
	}
	return 0
}
