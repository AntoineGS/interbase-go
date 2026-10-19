package interbase

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"time"

	"interbase-go/psql"
)

type lpSignal uint8

const (
	lpContinue lpSignal = iota
	lpBreak
	lpExit
)

// lpRun is one emulated execution over one connection and transaction.
type lpRun struct {
	ctx        context.Context
	conn       *conn
	tx         *Transaction
	plan       *lpPlan
	opts       LineProfileOptions
	result     *LineProfileResult
	dbNone     bool
	statements map[string]cachedStatement
	selects    map[string]lpSelectText // chosen SELECT text per node (R18)
	writes     map[*psql.Node]bool     // blockWrites results
	sites      map[*lpRoutineMeta][]lpSite
	// stepInto holds the procedures run under emulation when called in a
	// step-in shape (spec 6.7).
	stepInto map[string]bool
	// routines are the profile entries, [0] the entry routine's; the
	// callees' are appended to the result when the run ends. calleeIndex
	// finds a callee's entry by its aggregation key.
	routines    []*LineProfileRoutine
	calleeIndex map[lpCalleeKey]int
	frames      int                    // frames started, the entry's excluded
	active      map[*lpRoutineMeta]int // running frames per routine
	activity    Activity
	// haveActivity is set once the first snapshot of the run is taken.
	haveActivity bool
	// openBlob opens a BLOB for reading; nil means the transaction's
	// OpenBlob (a test seam).
	openBlob func(BlobRef) (io.ReadCloser, error)
	// countersUnavailable is set once the server cannot supply activity
	// counters; node Pages and Tables are then cleared (R16).
	countersUnavailable bool
	// dynamic holds the EXECUTE STATEMENT texts prepared per node, least
	// recently used first (their statements are in statements);
	// dynamicTexts the hashes of the distinct texts each node evaluated;
	// procedureNames the source names of dynamic texts found to be
	// procedures.
	dynamic        map[string][]string
	dynamicTexts   map[*LineProfileNode]map[uint64]struct{}
	procedureNames map[string]bool
}

// runInTransaction runs the entry routine inside the run's transaction and
// always rolls it back. Every prepared statement of the run is closed before
// the rollback on every path; cursors are closed where they are opened. The
// first error is the run outcome; the second is a close or rollback failure.
func (r *lpRun) runInTransaction() (outcomeErr, txErr error) {
	txErr = runProfileTransaction(r.tx.Rollback, func() (closeErr error) {
		defer func() { closeErr = r.closeStatements() }()
		outcomeErr = r.runEntry()
		return nil
	}, &r.result.RolledBack)
	for _, profile := range r.routines[min(1, len(r.routines)):] {
		r.result.Routines = append(r.result.Routines, *profile)
	}
	r.routines = nil
	if r.countersUnavailable {
		clearCounters(r.result.Routines)
		r.result.OverheadActivity = ActivityDelta{}
	}
	return outcomeErr, txErr
}

// closeStatements closes every statement prepared by the run.
func (r *lpRun) closeStatements() error {
	var err error
	for key, cached := range r.statements {
		err = errors.Join(err, cached.stmt.Close())
		delete(r.statements, key)
	}
	return err
}

// lpNodeError locates a run error at the node that raised it. keepsBlock
// is set when the statement is a DML or EXCEPTION statement of the routine
// whose handler catches it: InterBase then keeps the handling block's work
// done before the error (R19). An error from any other statement — an
// assignment, a condition, a SELECT or a procedure call, including every
// error leaving a stepped-into routine — undoes the handling block's work
// before its handler runs, as verified natively.
type lpNodeError struct {
	routine    string
	line       int
	err        error
	keepsBlock bool
}

func (e *lpNodeError) Error() string { return e.err.Error() }
func (e *lpNodeError) Unwrap() error { return e.err }

// lpFatalError is a failure of the emulator's own work (prepare, savepoint,
// activity snapshot) that PSQL would never raise; WHEN handlers never catch
// it.
type lpFatalError struct{ err error }

func (e *lpFatalError) Error() string { return e.err.Error() }
func (e *lpFatalError) Unwrap() error { return e.err }

func fatal(err error) error {
	if err == nil {
		return nil
	}
	return &lpFatalError{err: err}
}

// errUserException is the error EXCEPTION name raises, shaped like the
// server's: SQLCODE -836, isc_except with the exception number, and the
// message.
func errUserException(name string, exception lpException, message string) error {
	const except, random = 335544517, 335544382 // isc_except, isc_random
	return &Error{
		Operation:       "line profile EXCEPTION " + name,
		SQLCode:         -836,
		NativeCode:      except,
		Message:         fmt.Sprintf("exception %d\n%s", exception.number, message),
		gdsCodes:        []int64{except, random},
		exceptionNumber: exception.number,
		hasException:    true,
	}
}

// errSingleton is the singleton-select error PSQL raises when SELECT … INTO
// finds more than one row.
func errSingleton() error {
	const singleSelect = 335544652 // isc_sing_select_err
	return &Error{
		Operation:  "line profile singleton select",
		SQLCode:    -811,
		NativeCode: singleSelect,
		Message:    "multiple rows in singleton select",
		gdsCodes:   []int64{singleSelect},
	}
}

func (r *lpRun) exec(f *lpFrame, n *psql.Node) (lpSignal, error) {
	if err := r.ctx.Err(); err != nil {
		return lpContinue, &lpNodeError{routine: f.meta.name, line: n.Line, err: err}
	}
	node := &f.profile.Nodes[n.ID]
	if n.Kind != psql.NodeWhile {
		node.Metrics.Hits++
	}
	sig, err := r.execNode(f, n, node)
	if err != nil {
		var located *lpNodeError
		if !errors.As(err, &located) {
			err = &lpNodeError{routine: f.meta.name, line: n.Line, err: err, keepsBlock: keepsBlock(n.Kind)}
		}
		return lpContinue, err
	}
	return sig, nil
}

// keepsBlock reports the statements whose errors keep the handling block's
// earlier work (see lpNodeError).
func keepsBlock(kind psql.NodeKind) bool {
	switch kind {
	case psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete, psql.NodeException:
		return true
	}
	return false
}

// errKeepsBlock reports whether err keeps its handling block's earlier work.
func errKeepsBlock(err error) bool {
	var located *lpNodeError
	return errors.As(err, &located) && located.keepsBlock
}

func (r *lpRun) execNode(f *lpFrame, n *psql.Node, node *LineProfileNode) (lpSignal, error) {
	switch n.Kind {
	case psql.NodeBlock:
		return r.execBlock(f, n)
	case psql.NodeAssign:
		target, ok := f.vars[n.Target]
		if !ok {
			return lpContinue, fmt.Errorf("interbase: line profile: unknown target %s", n.Target)
		}
		value, err := r.evalValue(f, node, n.Value, target)
		if err != nil {
			return lpContinue, err
		}
		target.value = value
		return lpContinue, nil
	case psql.NodeIf:
		ok, err := r.evalCond(f, node, n.Cond)
		if err != nil {
			return lpContinue, err
		}
		if ok {
			return r.exec(f, n.Children[0])
		}
		if len(n.Children) > 1 {
			return r.exec(f, n.Children[1])
		}
		return lpContinue, nil
	case psql.NodeWhile:
		for {
			node.Metrics.Hits++
			ok, err := r.evalCond(f, node, n.Cond)
			if err != nil || !ok {
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
	case psql.NodeSelectInto:
		if r.stepsInto(n) {
			return lpContinue, r.stepSelectInto(f, n, node)
		}
		return lpContinue, r.execSelectInto(f, n, node)
	case psql.NodeForSelect:
		if r.stepsInto(n) {
			return r.stepForSelect(f, n, node)
		}
		return r.execForSelect(f, n, node)
	case psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete:
		b, err := r.bind(f, node, n.SQL.Text, n.SQL.Params)
		if err != nil {
			return lpContinue, err
		}
		node.SQL = b.text
		var affected int64
		if err := r.serverCall(node, func() (e error) {
			affected, e = b.stmt.Exec(r.ctx, b.args...)
			return e
		}); err != nil {
			return lpContinue, err
		}
		node.Metrics.Rows += affected
		return lpContinue, nil
	case psql.NodeExecProc:
		return lpContinue, r.execProcedure(f, n, node)
	case psql.NodeExecStmt:
		return lpContinue, r.execStatement(f, n, node)
	case psql.NodeForExecStmt:
		return r.forExecStatement(f, n, node)
	case psql.NodeSuspend:
		if f.suspend != nil {
			return lpContinue, f.suspend(f.row())
		}
		if !f.entry || !r.plan.analysis.Entry.Selectable {
			// In an executable procedure SUSPEND ends the routine like EXIT.
			return lpExit, nil
		}
		return lpContinue, r.emitRow(f)
	case psql.NodeExit:
		return lpExit, nil
	case psql.NodeBreak:
		return lpBreak, nil
	case psql.NodeEmpty:
		return lpContinue, nil
	case psql.NodePostEvent:
		// Events are delivered at commit; the run always rolls back.
		return lpContinue, nil
	case psql.NodeException:
		return lpContinue, r.raise(n, node)
	default:
		return lpContinue, fmt.Errorf("interbase: line profile: %s is not supported yet", n.Kind)
	}
}

func (r *lpRun) execChildren(f *lpFrame, n *psql.Node) (lpSignal, error) {
	for _, child := range n.Children {
		sig, err := r.exec(f, child)
		if err != nil || sig != lpContinue {
			return sig, err
		}
	}
	return lpContinue, nil
}

// execBlock runs a BEGIN … END (spec 6.6, as InterBase behaves). Each block
// is its own savepoint scope: when an error leaves a block, the block's work
// is undone; when a handler of a block catches an error, execution goes on
// after the block and the block's work before the error is kept or undone
// before the handlers run as lpNodeError describes. The savepoint is started
// only when it can matter: the block may write and has handlers or runs
// inside a block that has (in this frame or in a caller).
func (r *lpRun) execBlock(f *lpFrame, n *psql.Node) (lpSignal, error) {
	guarded := len(n.Handlers) > 0
	name := ""
	if (guarded || f.guards > 0) && r.blockWrites(n) {
		var err error
		if name, err = r.startSavepoint(f); err != nil {
			return lpContinue, &lpNodeError{routine: f.meta.name, line: n.Line, err: err}
		}
	}
	if guarded {
		f.guards++
	}
	sig, err := r.execChildren(f, n)
	if guarded {
		f.guards--
		if first := r.firstHandler(n, err); first >= 0 {
			if name != "" && !errKeepsBlock(err) {
				if spErr := r.savepoint("ROLLBACK TO SAVEPOINT " + name); spErr != nil {
					return lpContinue, &lpNodeError{routine: f.meta.name, line: n.EndLine, err: errors.Join(spErr, err)}
				}
				// The handlers' own work is the block's again.
				var spErr error
				if name, spErr = r.startSavepoint(f); spErr != nil {
					return lpContinue, &lpNodeError{routine: f.meta.name, line: n.EndLine, err: errors.Join(spErr, err)}
				}
			}
			sig, err = r.runHandlers(f, n, first)
		}
	}
	if name == "" {
		return sig, err
	}
	// A stepped-into routine abandoned by its caller keeps its work, as a
	// native cursor closed early does.
	if err == nil || errors.Is(err, errLPAbandon) && r.ctx.Err() == nil {
		if spErr := r.savepoint("RELEASE SAVEPOINT " + name); spErr != nil {
			return lpContinue, &lpNodeError{routine: f.meta.name, line: n.EndLine, err: errors.Join(spErr, err)}
		}
		return sig, err
	}
	// Undo only when a handler can still catch the error; otherwise the run
	// ends and its rollback undoes everything. InterBase ends the savepoint
	// with ROLLBACK TO SAVEPOINT.
	if f.guards > 0 && r.catchable(err) {
		if spErr := r.savepoint("ROLLBACK TO SAVEPOINT " + name); spErr != nil {
			return lpContinue, &lpNodeError{routine: f.meta.name, line: n.EndLine, err: errors.Join(spErr, err)}
		}
	}
	return lpContinue, err
}

// startSavepoint starts the frame's next savepoint and returns its name.
func (r *lpRun) startSavepoint(f *lpFrame) (string, error) {
	f.savepoints++
	name := fmt.Sprintf("LP$SP_%d_%d", f.id, f.savepoints)
	return name, r.savepoint("SAVEPOINT " + name)
}

// firstHandler returns the index of the first WHEN handler of block n
// whose condition matches err, -1 when err is nil, cannot be caught or
// matches none.
func (r *lpRun) firstHandler(n *psql.Node, err error) int {
	if err == nil || !r.catchable(err) {
		return -1
	}
	for i, h := range n.Handlers {
		if r.matches(h, err) {
			return i
		}
	}
	return -1
}

// runHandlers runs the WHEN handlers of block n as InterBase does: the
// first matching handler, then every later handler with ANY (the error no
// longer matches anything else once handled). The signal is the last
// handler's, so an EXIT is lost when a later ANY handler runs. An error
// raised by a handler propagates at once.
func (r *lpRun) runHandlers(f *lpFrame, n *psql.Node, first int) (lpSignal, error) {
	sig := lpContinue
	for i := first; i < len(n.Handlers); i++ {
		h := n.Handlers[i]
		if i > first && !handlesAny(h) {
			continue
		}
		var handlerErr error
		if sig, handlerErr = r.exec(f, h.Body); handlerErr != nil {
			return lpContinue, handlerErr
		}
	}
	return sig, nil
}

// savepoint runs a savepoint statement; its time is Savepoints overhead. An
// activity snapshot follows, whether or not it succeeded, so that its
// counters (an undo's backouts) are overhead, not the next node's.
func (r *lpRun) savepoint(text string) error {
	start := time.Now()
	_, err := r.tx.Exec(r.ctx, text)
	r.result.Savepoints += time.Since(start)
	if snapErr := r.snapshot(nil); err == nil {
		err = snapErr
	}
	return fatal(err)
}

// closeCursor closes a cursor. A cursor that was not read to its end is
// closed by a separate server call after the node's last snapshot, so an
// overhead snapshot follows; an exhausted cursor was already closed inside
// the measured fetch.
func (r *lpRun) closeCursor(cursor *Cursor, exhausted bool) error {
	err := cursor.Close()
	if !exhausted {
		err = errors.Join(err, fatal(r.snapshot(nil)))
	}
	return err
}

// catchable reports whether a WHEN handler may catch err: server errors and
// EXCEPTION only, never a cancellation nor a failure of the emulator's own
// work.
func (r *lpRun) catchable(err error) bool {
	var nativeErr *Error
	var fatalErr *lpFatalError
	return r.ctx.Err() == nil && !errors.As(err, &fatalErr) && errors.As(err, &nativeErr) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

// matches reports whether one of h's conditions matches err.
func (r *lpRun) matches(h *psql.Handler, err error) bool {
	var nativeErr *Error
	if !errors.As(err, &nativeErr) {
		return false
	}
	for _, c := range h.Conditions {
		switch c.Kind {
		case "any":
			return true
		case "sqlcode":
			if nativeErr.SQLCode == int(c.Code) {
				return true
			}
		case "gdscode":
			if code, ok := GDSCodeByName(c.Name); ok && nativeErr.GDSCode() == code {
				return true
			}
		case "exception":
			if number, ok := nativeErr.ExceptionNumber(); ok && r.plan.exceptionNames[number] == c.Name {
				return true
			}
		}
	}
	return false
}

func handlesAny(h *psql.Handler) bool {
	for _, c := range h.Conditions {
		if c.Kind == "any" {
			return true
		}
	}
	return false
}

// blockWrites reports whether a block may change data, so that undoing it
// can matter: it contains a data statement, EXECUTE PROCEDURE, EXECUTE
// STATEMENT or an expression with a subquery (which may select from a
// procedure).
func (r *lpRun) blockWrites(n *psql.Node) bool {
	if writes, ok := r.writes[n]; ok {
		return writes
	}
	var walk func(n *psql.Node) bool
	walk = func(n *psql.Node) bool {
		if n.SQL != nil || writesDynamically(n.Kind) || exprSelects(n.Value) || exprSelects(n.Cond) {
			return true
		}
		for _, child := range n.Children {
			if walk(child) {
				return true
			}
		}
		for _, h := range n.Handlers {
			if walk(h.Body) {
				return true
			}
		}
		return false
	}
	writes := walk(n)
	if r.writes == nil {
		r.writes = make(map[*psql.Node]bool)
	}
	r.writes[n] = writes
	return writes
}

func exprSelects(e *psql.Expr) bool {
	found := false
	if e != nil {
		wordsIn(e.Text, func(start, end int, _, _ bool) {
			found = found || strings.EqualFold(e.Text[start:end], "SELECT")
		})
	}
	return found
}

// raise runs EXCEPTION name with the exception's message from
// RDB$EXCEPTIONS. InterBase refuses a message expression (R19), so
// checkRoutine refuses it too.
func (r *lpRun) raise(n *psql.Node, node *LineProfileNode) error {
	exception, ok := r.plan.exceptions[n.Exception]
	if !ok || n.Value != nil {
		return fmt.Errorf("interbase: line profile: EXCEPTION %s is not supported", n.Exception)
	}
	node.Metrics.Errors++
	return errUserException(n.Exception, exception, exception.message)
}

func (r *lpRun) execSelectInto(f *lpFrame, n *psql.Node, node *LineProfileNode) error {
	b, cast, err := r.bindSelect(f, n, node)
	if err != nil {
		return err
	}
	rows, err := r.queryRows(node, b, 2)
	if err != nil && cast != nil && r.catchable(err) {
		// The failure may be a row's in-statement CAST (R18), where the
		// server raises the singleton error before converting any row
		// (ibg-pm8). bindSelect casts only statements that can run again,
		// so the plain statement decides, as it would natively.
		node.Metrics.Errors--
		if b, err = r.bind(f, node, n.SQL.Text, n.SQL.Params); err != nil {
			return err
		}
		cast = nil
		rows, err = r.queryRows(node, b, 2)
	}
	if err != nil {
		return err
	}
	node.Metrics.Rows += int64(len(rows))
	switch len(rows) {
	case 0:
		return nil
	case 1:
		return r.assignRow(f, node, n.Into, rows[0], b.stmt.Outputs(), cast)
	default:
		node.Metrics.Errors++
		return errSingleton()
	}
}

// assignRow assigns the columns of row to targets; exact[i] (when set)
// marks a column the statement already converted to its target's type.
func (r *lpRun) assignRow(f *lpFrame, node *LineProfileNode, targets []string, row []Cell, outputs []OutputDescriptor, exact []bool) error {
	if len(targets) > len(row) || len(row) != len(outputs) {
		return fmt.Errorf("interbase: line profile: %d targets for %d columns", len(targets), len(row))
	}
	for i, target := range targets {
		if err := r.assign(f, node, target, cellValue(row[i]), outputs[i], i < len(exact) && exact[i]); err != nil {
			return err
		}
	}
	return nil
}

func (r *lpRun) execForSelect(f *lpFrame, n *psql.Node, node *LineProfileNode) (sig lpSignal, err error) {
	b, cast, err := r.bindSelect(f, n, node)
	if err != nil {
		return lpContinue, err
	}
	node.SQL = b.text
	var cursor *Cursor
	if err := r.serverCall(node, func() (e error) {
		cursor, e = b.stmt.Query(r.ctx, b.args...)
		return e
	}); err != nil {
		return lpContinue, err
	}
	exhausted := false
	defer func() { err = errors.Join(err, r.closeCursor(cursor, exhausted)) }()
	outputs := b.stmt.Outputs()
	for {
		var hasRow bool
		if err := r.serverCall(node, func() (e error) {
			hasRow, e = cursor.Next(r.ctx)
			return e
		}); err != nil {
			return lpContinue, err
		}
		if !hasRow {
			exhausted = true
			return lpContinue, nil
		}
		node.Metrics.Rows++
		row, err := cursor.Row()
		if err != nil {
			return lpContinue, err
		}
		if err := r.assignRow(f, node, n.Into, row, outputs, cast); err != nil {
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

// execProcedure calls a procedure natively (spec 6.4). DSQL accepts only
// parameters and literals as EXECUTE PROCEDURE inputs, so every input is a
// plain "?" bound to a value that already has the callee parameter's type
// (R14): a variable of exactly that type is bound directly; literals are
// written in place; any other argument, or a variable of another type, is
// evaluated first for the parameter type and bound as a value.
func (r *lpRun) execProcedure(f *lpFrame, n *psql.Node, node *LineProfileNode) error {
	callee, ok := r.plan.procs[n.Proc.Name]
	if !ok {
		return fmt.Errorf("interbase: line profile: procedure %s is not loaded", n.Proc.Name)
	}
	if r.stepInto[callee.name] {
		return r.stepExecute(f, n, node, callee)
	}
	if len(n.Proc.Args) > len(callee.inputs) {
		return fmt.Errorf("interbase: line profile: %d arguments for %d parameters of %s",
			len(n.Proc.Args), len(callee.inputs), callee.name)
	}
	var pieces, params []string
	var temporaries []string
	defer func() {
		for _, name := range temporaries {
			delete(f.vars, name)
		}
	}()
	for i, arg := range n.Proc.Args {
		typeText := callee.inputs[i].typeText
		switch local := arg.Local.(type) {
		case psql.LocalVar:
			// A BLOB variable binds to a BLOB parameter as its content: the
			// driver writes a new BLOB, as PSQL passes the BLOB itself.
			if v, ok := f.vars[local.Name]; ok && (r.declaredType(v.typ.Text) == r.declaredType(typeText) ||
				isBlobType(v.typ.Text) && isBlobType(typeText)) {
				pieces, params = append(pieces, "?"), append(params, local.Name)
				continue
			}
		case psql.LocalInt, psql.LocalNull, psql.LocalString:
			pieces = append(pieces, arg.Text)
			continue
		}
		temp := r.newVar(typeText)
		value, err := r.evalValue(f, node, arg, temp)
		if err != nil {
			return err
		}
		temp.value = value
		name := fmt.Sprintf("\x00ARG%d", i)
		f.vars[name] = temp
		temporaries = append(temporaries, name)
		pieces, params = append(pieces, "?"), append(params, name)
	}
	text := "EXECUTE PROCEDURE " + procedureName(callee.name, n.Proc.Quoted)
	if len(pieces) > 0 {
		text += "(" + strings.Join(pieces, ", ") + ")"
	}
	b, err := r.bindParams(f, node, text, params, true)
	if err != nil {
		return err
	}
	outputs := b.stmt.Outputs()
	if len(outputs) == 0 {
		node.SQL = b.text
		return r.serverCall(node, func() error {
			_, e := b.stmt.Exec(r.ctx, b.args...)
			return e
		})
	}
	rows, err := r.queryRows(node, b, 1)
	if err != nil {
		return err
	}
	if len(rows) == 0 || len(n.Returning) == 0 {
		return nil
	}
	return r.assignRow(f, node, n.Returning, rows[0], outputs, nil)
}

func procedureName(name string, quoted bool) string {
	if !quoted {
		return name
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// emitRow counts one output row of the entry routine and keeps the first
// KeepRows of them. BLOB values are held as bytes already (R22); for display
// a kept BLOB is cut at keptBlobLimit and marked.
func (r *lpRun) emitRow(f *lpFrame) error {
	r.result.Rows++
	if len(r.result.KeptRows) >= r.opts.KeepRows {
		return nil
	}
	row := make([]Cell, len(r.result.Columns))
	for i, name := range r.result.Columns {
		v, ok := f.vars[name]
		if !ok {
			continue
		}
		value := v.value
		if _, bad := value.(lpBadBlob); bad {
			return errBlobNotFound()
		}
		if ref, isBlob := value.(BlobRef); isBlob {
			held, err := r.holdBlob(ref, keptBlobLimit, true)
			if err != nil {
				return err
			}
			value = held
		} else if isBlobType(v.typ.Text) {
			value = truncateKeptBlob(value)
		}
		row[i] = valueCell(value)
	}
	r.result.KeptRows = append(r.result.KeptRows, row)
	return nil
}

// keptBlobLimit caps the BLOB bytes shown in a kept output row.
const keptBlobLimit = 64 << 10

// keptBlobTruncated marks a kept BLOB value cut at keptBlobLimit.
const keptBlobTruncated = "…(truncated)"

// truncateKeptBlob cuts a held BLOB value for display.
func truncateKeptBlob(value any) any {
	switch v := value.(type) {
	case string:
		if len(v) > keptBlobLimit {
			return v[:keptBlobLimit] + keptBlobTruncated
		}
	case []byte:
		if len(v) > keptBlobLimit {
			return append(append([]byte(nil), v[:keptBlobLimit]...), keptBlobTruncated...)
		}
	}
	return value
}

// holdValue replaces a BLOB reference fetched from the server by the BLOB's
// content (R22): a reference dies with the transaction and binds only to a
// BLOB parameter of the same subtype and character set, while bytes bind
// anywhere PSQL would accept the value. Other values are returned as is. A
// BLOB larger than MaxBlobBytes is never truncated: it fails the run with a
// fatal error.
func (r *lpRun) holdValue(value any) (any, error) {
	ref, ok := value.(BlobRef)
	if !ok {
		return value, nil
	}
	return r.holdBlob(ref, r.maxBlobBytes(), false)
}

// maxBlobBytes is the effective MaxBlobBytes: the default for zero or a
// negative value, at most math.MaxInt64-1 so that a read of cap+1 bytes
// cannot overflow.
func (r *lpRun) maxBlobBytes() int64 {
	switch {
	case r.opts.MaxBlobBytes <= 0:
		return DefaultLineProfileMaxBlobBytes
	case r.opts.MaxBlobBytes == math.MaxInt64:
		return math.MaxInt64 - 1
	}
	return r.opts.MaxBlobBytes
}

// errBlobTooLarge reports a BLOB over the MaxBlobBytes cap.
func errBlobTooLarge(limit int64) error {
	return fmt.Errorf("interbase: line profile: BLOB too large to emulate (more than %d bytes)", limit)
}

// holdBlob reads a BLOB — text as a string, other subtypes as bytes — up to
// limit bytes. Past limit the value is cut and marked when cut is set (kept
// rows' display); otherwise the read fails with errBlobTooLarge. The read is
// overhead: its time goes to Blobs and an activity snapshot follows.
func (r *lpRun) holdBlob(ref BlobRef, limit int64, cut bool) (any, error) {
	start := time.Now()
	data, truncated, err := r.readBlob(ref, limit)
	r.result.Blobs += time.Since(start)
	if snapErr := r.snapshot(nil); err == nil {
		err = snapErr
	}
	if err == nil && truncated && !cut {
		err = errBlobTooLarge(limit)
	}
	if err != nil {
		return nil, fatal(err)
	}
	if truncated {
		data = append(data, keptBlobTruncated...)
	}
	if ref.subtype == 1 {
		return string(data), nil
	}
	return data, nil
}

func (r *lpRun) readBlob(ref BlobRef, limit int64) ([]byte, bool, error) {
	open := r.openBlob
	if open == nil {
		open = func(ref BlobRef) (io.ReadCloser, error) { return r.tx.OpenBlob(r.ctx, ref) }
	}
	reader, err := open(ref)
	if err != nil {
		return nil, false, err
	}
	data, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
	err = errors.Join(readErr, reader.Close())
	if int64(len(data)) > limit {
		return data[:limit], true, err
	}
	return data, false, err
}

// runEntry evaluates the entry arguments (setup overhead) and runs the entry
// routine. A returned error is the run's outcome error.
func (r *lpRun) runEntry() error {
	entry := r.plan.entry
	profile := &r.result.Routines[0]
	r.routines = []*LineProfileRoutine{profile}
	f := newFrame(entry, r.dbNone, profile)
	f.entry = true
	r.active = map[*lpRoutineMeta]int{entry: 1}
	setupStart, snapshotsBefore := time.Now(), r.result.Snapshots
	if err := r.snapshot(nil); err != nil {
		return err
	}
	for i, arg := range r.plan.analysis.Entry.Args {
		param := entry.inputs[i]
		text := "SELECT CAST((" + arg + ") AS " + castType(param.typeText) + ") FROM RDB$DATABASE"
		value, err := r.setupValue(text)
		if err != nil {
			return err
		}
		f.vars[param.name].value = value
	}
	r.result.Setup += time.Since(setupStart) - (r.result.Snapshots - snapshotsBefore)
	profile.Calls++
	if _, err := r.exec(f, entry.routine.Body); err != nil {
		return err
	}
	if !r.plan.analysis.Entry.Selectable && len(r.result.Columns) > 0 {
		return r.emitRow(f)
	}
	return nil
}

// setupValue evaluates a constant entry argument; its cost is setup
// overhead, not line time.
func (r *lpRun) setupValue(text string) (value any, err error) {
	// The argument's counters, prepare included, are overhead.
	defer func() { err = errors.Join(err, r.snapshot(nil)) }()
	stmt, err := r.tx.Prepare(r.ctx, text)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, stmt.Close()) }()
	cursor, err := stmt.Query(r.ctx)
	if err != nil {
		return nil, err
	}
	defer func() { err = errors.Join(err, cursor.Close()) }()
	hasRow, err := cursor.Next(r.ctx)
	if err != nil || !hasRow {
		return nil, errors.Join(err, errors.New("interbase: line profile: entry argument returned no value"))
	}
	row, err := cursor.Row()
	if err != nil {
		return nil, err
	}
	return r.holdValue(cellValue(row[0]))
}
