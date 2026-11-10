package interbase

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"interbase-go/psql"
)

const debugReadOnlyTransactionGDSCode int64 = 335544361 // isc_read_only_trans

type lpDebugHeaderError struct{ err error }

func (e *lpDebugHeaderError) Error() string { return e.err.Error() }
func (e *lpDebugHeaderError) Unwrap() error { return e.err }

func (r *lpRun) debugHeaderError(err error) error {
	if err == nil || r.debug == nil {
		return err
	}
	return &lpDebugHeaderError{err: err}
}

// readOnly reports the transaction access mode currently in use. Ordinary
// line-profile runs retain their fixed caller-selected mode.
func (r *lpRun) readOnly() bool {
	if r.debug == nil {
		return r.opts.ReadOnly
	}
	r.debug.session.mu.Lock()
	defer r.debug.session.mu.Unlock()
	return r.debug.phase == DebugReadOnly
}

func (d *debugState) addPendingSavepoint(name string) {
	d.session.mu.Lock()
	defer d.session.mu.Unlock()
	for _, existing := range d.pending {
		if existing == name {
			return
		}
	}
	d.pending = append(d.pending, name)
}

func (d *debugState) isPendingSavepoint(name string) bool {
	d.session.mu.Lock()
	defer d.session.mu.Unlock()
	for _, existing := range d.pending {
		if existing == name {
			return true
		}
	}
	return false
}

func (d *debugState) removePendingSavepoint(name string) {
	d.session.mu.Lock()
	defer d.session.mu.Unlock()
	for i, existing := range d.pending {
		if existing == name {
			d.pending = append(d.pending[:i], d.pending[i+1:]...)
			return
		}
	}
}

func (d *debugState) rollbackPendingSavepoint(name string) {
	d.session.mu.Lock()
	defer d.session.mu.Unlock()
	for i, existing := range d.pending {
		if existing == name {
			d.pending = d.pending[:i+1]
			return
		}
	}
}

func parseSavepointOperation(text string) (operation, name string, ok bool) {
	words := strings.Fields(text)
	if len(words) >= 3 && strings.EqualFold(words[0], "RELEASE") && strings.EqualFold(words[1], "SAVEPOINT") {
		return "release", words[2], true
	}
	to := 1
	if len(words) > 1 && strings.EqualFold(words[1], "WORK") {
		to++
	}
	if len(words) >= to+3 && strings.EqualFold(words[0], "ROLLBACK") &&
		strings.EqualFold(words[to], "TO") && strings.EqualFold(words[to+1], "SAVEPOINT") {
		return "rollback", words[to+2], true
	}
	return "", "", false
}

// debugStartAnalysis finds a write that could run while a cursor is open.
// Other writes stay lazy and switch immediately before their own statement.
func debugStartAnalysis(plan *lpPlan) (bool, string) {
	if plan == nil || plan.entry == nil || plan.entry.routine == nil {
		return false, ""
	}
	run := &lpRun{plan: plan}
	seen := make(map[string]bool)
	queue := []*lpRoutineMeta{plan.entry}
	seen[plan.entry.name] = true
	for len(queue) > 0 {
		meta := queue[0]
		queue = queue[1:]
		if meta.routine == nil {
			continue
		}
		for _, node := range meta.routine.Nodes {
			if node.Kind == psql.NodeForSelect || node.Kind == psql.NodeForExecStmt {
				if run.nodeMayWrite(meta, node) || debugNodeTreeMayWrite(run, meta, node.Children[0]) {
					return true, fmt.Sprintf("writes inside the FOR SELECT at line %d of %s", node.Line, meta.name)
				}
			}
		}
		for _, site := range run.procedureSites(meta) {
			callee := plan.procs[site.name]
			if callee == nil || callee.routine == nil || len(callee.problems) != 0 || seen[callee.name] {
				continue
			}
			seen[callee.name] = true
			queue = append(queue, callee)
		}
	}
	return false, ""
}

func debugNodeTreeMayWrite(run *lpRun, meta *lpRoutineMeta, node *psql.Node) bool {
	if node == nil {
		return false
	}
	if run.nodeMayWrite(meta, node) {
		return true
	}
	for _, child := range node.Children {
		if debugNodeTreeMayWrite(run, meta, child) {
			return true
		}
	}
	for _, handler := range node.Handlers {
		if debugNodeTreeMayWrite(run, meta, handler.Body) {
			return true
		}
	}
	return false
}

// nodeMayWrite classifies only the statement header. Compound-node bodies
// are visited separately by start analysis and by the normal execution hook.
func (r *lpRun) nodeMayWrite(meta *lpRoutineMeta, node *psql.Node) bool {
	if node == nil {
		return false
	}
	texts, ranges := debugNodeHeaderTexts(node)
	for _, text := range texts {
		if textContainsGenID(text) {
			return true
		}
	}
	switch node.Kind {
	case psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete, psql.NodeExecStmt, psql.NodeForExecStmt:
		return true
	case psql.NodeExecProc:
		if node.Proc == nil || r.plan == nil {
			return true
		}
		callee := r.plan.procs[node.Proc.Name]
		if callee == nil {
			return true
		}
		return !callee.readsOnly && !r.debugCallWillBeEmulated(meta, node, node.Proc.Name)
	}

	if meta == nil || meta.routine == nil || r.plan == nil {
		return false
	}
	emulated := ""
	if node.Proc != nil && r.debugCallWillBeEmulated(meta, node, node.Proc.Name) {
		emulated = node.Proc.Name
	}
	for _, site := range r.procedureSites(meta) {
		if site.name == emulated || !debugSiteInRanges(site.line, ranges) {
			continue
		}
		callee := r.plan.procs[site.name]
		if callee == nil || !callee.readsOnly {
			return true
		}
	}
	return false
}

func (r *lpRun) debugCallWillBeEmulated(meta *lpRoutineMeta, node *psql.Node, callee string) bool {
	if r.debug == nil || node == nil || node.Proc == nil || node.Proc.Name != callee {
		return false
	}
	return r.debug.willStepInto(meta, node, callee)
}

type debugLineRange struct{ first, last int }

func debugNodeHeaderTexts(node *psql.Node) ([]string, []debugLineRange) {
	var texts []string
	var ranges []debugLineRange
	add := func(line int, text string) {
		if text == "" {
			return
		}
		texts = append(texts, text)
		ranges = append(ranges, debugLineRange{first: line, last: line + strings.Count(text, "\n")})
	}
	switch node.Kind {
	case psql.NodeIf, psql.NodeWhile:
		if node.Cond != nil {
			add(node.Cond.Line, node.Cond.Text)
		}
	case psql.NodeAssign, psql.NodeExecStmt, psql.NodeForExecStmt, psql.NodePostEvent:
		if node.Value != nil {
			add(node.Value.Line, node.Value.Text)
		}
	case psql.NodeExecProc:
		if node.Proc != nil {
			for _, arg := range node.Proc.Args {
				if arg != nil {
					add(arg.Line, arg.Text)
				}
			}
		}
	case psql.NodeSelectInto, psql.NodeForSelect, psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete:
		if node.SQL != nil {
			add(node.Line, node.SQL.Text)
		}
	}
	return texts, ranges
}

func debugSiteInRanges(line int, ranges []debugLineRange) bool {
	for _, span := range ranges {
		if line >= span.first && line <= span.last {
			return true
		}
	}
	return false
}

func textContainsGenID(text string) bool {
	found := false
	wordsIn(text, func(start, end int, _, _ bool) {
		found = found || strings.EqualFold(text[start:end], "GEN_ID")
	})
	return found
}

func (d *debugState) willStepInto(meta *lpRoutineMeta, node *psql.Node, callee string) bool {
	s := d.session
	s.mu.Lock()
	defer s.mu.Unlock()
	f := d.currentFrame
	return d.willStepIntoLocked(f, meta, node, callee)
}

func (d *debugState) willStepIntoFrame(f *lpFrame, node *psql.Node, callee string) bool {
	s := d.session
	s.mu.Lock()
	defer s.mu.Unlock()
	return d.willStepIntoLocked(f, f.meta, node, callee)
}

func (d *debugState) willStepIntoLocked(f *lpFrame, meta *lpRoutineMeta, node *psql.Node, callee string) bool {
	if f == nil || f.meta != meta || node == nil || d.run == nil || !d.run.stepInto[callee] {
		return false
	}
	requested := d.mode == DebugStepInto && d.stepNode == node && d.stepFrameID == f.id
	if requested {
		return true
	}
	return d.reachableBreakpointOrRunTo(callee)
}

func (r *lpRun) debugLeafMayRetry(f *lpFrame, node *psql.Node) bool {
	switch node.Kind {
	case psql.NodeAssign, psql.NodeSelectInto, psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete, psql.NodeExecStmt:
		return true
	case psql.NodeExecProc:
		if node.Proc == nil {
			return true
		}
		callee := r.plan.procs[node.Proc.Name]
		return callee == nil || !r.debug.willStepIntoFrame(f, node, callee.name)
	default:
		return false
	}
}

func isDebugReadOnlyTransactionError(err error) bool {
	var nativeErr *Error
	return errors.As(err, &nativeErr) && nativeErr.GDSCode() == debugReadOnlyTransactionGDSCode
}

func debugReadOnlyRestartFailure(err error) error {
	message := "This statement needs a read-write transaction; restart the debug session."
	var located *lpNodeError
	if errors.As(err, &located) {
		return &lpNodeError{routine: located.routine, line: located.line,
			err: fatal(fmt.Errorf("%s: %w", message, located.err))}
	}
	return fatal(fmt.Errorf("%s: %w", message, err))
}

func (r *lpRun) switchToReadWrite(frame *lpFrame, node *psql.Node) error {
	if r.debug == nil || !r.readOnly() {
		return nil
	}
	oldTx := r.tx
	if oldTx == nil {
		return fatal(errors.New("interbase: debug: read-only transaction is unavailable"))
	}
	var pending []string
	r.debug.session.mu.Lock()
	pending = append(pending, r.debug.pending...)
	r.debug.session.mu.Unlock()

	open := len(oldTx.cursors)
	if open == 0 {
		closeErr := r.closeStatements()
		commitErr := oldTx.Commit()
		if commitErr == nil {
			r.tx = nil
		}
		if err := errors.Join(closeErr, commitErr); err != nil {
			return fatal(err)
		}
	} else {
		if r.retired != nil {
			return fatal(errors.New("interbase: debug: more than one read-only transaction was retired"))
		}
		r.retired = &lpRetiredTransaction{tx: oldTx, statements: r.statements, open: open}
		r.tx = nil
		r.statements = make(map[string]cachedStatement)
	}
	r.resetStatementCaches()

	tx, err := (&Attachment{conn: r.conn}).BeginTx(r.ctx, TransactionOptions{
		ReadOnly: false, Isolation: sql.LevelReadCommitted, NoWait: true,
	})
	if err != nil {
		return fatal(err)
	}
	r.tx = tx
	r.opts.ReadOnly = false
	r.debug.session.mu.Lock()
	r.debug.phase = DebugReadWrite
	r.debug.pending = nil
	r.debug.session.mu.Unlock()
	for _, name := range pending {
		if err := r.savepoint("SAVEPOINT " + name); err != nil {
			return err
		}
	}
	if open > 0 {
		cursorLine := debugOpenCursorLine(frame)
		mixed := fmt.Sprintf("Mixed transactions from line %d: rows of the FOR SELECT at line %d do not see this session's writes.", node.Line, cursorLine)
		r.debug.session.mu.Lock()
		r.debug.mixed = mixed
		r.debug.messages = append(r.debug.messages, DebugMessage{Kind: "warning", Text: mixed, Routine: frame.meta.name, Line: node.Line})
		r.debug.session.mu.Unlock()
	}
	r.debug.addMessage(DebugMessage{Kind: "info", Text: fmt.Sprintf("Switched to read-write before line %d of %s.", node.Line, frame.meta.name), Routine: frame.meta.name, Line: node.Line})
	r.debug.session.publish(buildDebugSnapshot(r.debug, nil, nil, DebugRunning))
	return nil
}

func debugOpenCursorLine(frame *lpFrame) int {
	for current := frame; current != nil; current = current.caller {
		if count := len(current.cursors); count > 0 {
			return current.cursors[count-1].line
		}
	}
	return 0
}

func (r *lpRun) resetStatementCaches() {
	r.selects = make(map[string]lpSelectText)
	r.dynamic = make(map[string][]string)
	r.dynamicTexts = make(map[*LineProfileNode]map[uint64]struct{})
}

func (r *lpRun) retiredCursorClosed() error {
	retired := r.retired
	if retired == nil {
		return nil
	}
	retired.open--
	if retired.open > 0 {
		return nil
	}
	return r.finishRetiredTransaction()
}

func (r *lpRun) finishRetiredTransaction() error {
	retired := r.retired
	if retired == nil {
		return nil
	}
	r.retired = nil
	var closeErr error
	for key, cached := range retired.statements {
		closeErr = errors.Join(closeErr, cached.stmt.Close())
		delete(retired.statements, key)
	}
	return errors.Join(closeErr, retired.tx.Commit())
}
