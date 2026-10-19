package interbase

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"interbase-go/psql"
)

// lpMaxDepth is the deepest stepped-into call; the entry routine is at
// depth 0.
const lpMaxDepth = 32

// errLPAbandon ends a stepped-into selectable routine whose caller stops
// reading its rows, as a native cursor closed early ends the procedure. No
// handler catches it.
var errLPAbandon = errors.New("interbase: line profile: stepped-into routine closed by its caller")

// lpCalleeKey is the aggregation key of a stepped-into routine's profile
// entry: every invocation of the procedure from one call line of one parent
// entry adds to the same LineProfileRoutine, so that each entry has one
// Parent and one CallLine and the entries form a tree.
type lpCalleeKey struct {
	name         string
	parent, line int
}

const (
	lpReasonShape = "only EXECUTE PROCEDURE, or SELECT/FOR SELECT of plain output columns FROM the procedure with INTO, can be stepped into"
	lpReasonBare  = "a procedure named in FROM without an argument list cannot be stepped into"
	// lpReasonRunsAhead labels a native FOR SELECT over a procedure that
	// cannot fetch one row at a time (ibg-mkx).
	lpReasonRunsAhead = "may run ahead; step in for exact interleaving"
)

// stepIntoNames normalizes the caller's step-in procedure names
// (psql.NormalizeName).
func stepIntoNames(names []string) map[string]bool {
	set := make(map[string]bool, len(names))
	for _, name := range names {
		set[psql.NormalizeName(name)] = true
	}
	return set
}

// stepIntoSet keeps the requested procedures that the analysis reached and
// that parse: the routines the run emulates when called in a step-in shape.
func stepIntoSet(plan *lpPlan, names []string) map[string]bool {
	requested := stepIntoNames(names)
	set := make(map[string]bool)
	for _, c := range plan.analysis.Callees {
		if meta := plan.procs[c.Name]; requested[c.Name] && c.Parses && meta != nil && meta.routine.Body != nil {
			set[c.Name] = true
		}
	}
	return set
}

// nativeCallSites lists the calls of procedures marked for step-in that run
// natively because of their shape, in the entry routine and in every
// routine the run can step into: one called in a step-in shape from the
// entry or from another such routine.
func nativeCallSites(plan *lpPlan, stepInto map[string]bool) []LineProfileCallSite {
	routines := []*lpRoutineMeta{plan.entry}
	reached := map[string]bool{plan.entry.name: true}
	var names []string
	for queue := []*lpRoutineMeta{plan.entry}; len(queue) > 0; queue = queue[1:] {
		for _, site := range queue[0].routine.CallSites() {
			if callee := plan.procs[site.Name]; site.StepInShape && stepInto[site.Name] && callee != nil && !reached[site.Name] {
				reached[site.Name] = true
				names = append(names, site.Name)
				queue = append(queue, callee)
			}
		}
	}
	sort.Strings(names)
	for _, name := range names {
		routines = append(routines, plan.procs[name])
	}
	var all []LineProfileCallSite
	for _, meta := range routines {
		var sites []LineProfileCallSite
		for _, site := range meta.routine.CallSites() {
			if stepInto[site.Name] && !site.StepInShape {
				sites = append(sites, LineProfileCallSite{Routine: meta.name, Line: site.Line, Procedure: site.Name, Reason: lpReasonShape})
			}
		}
		for _, source := range meta.routine.SourceNames() {
			if stepInto[source.Name] {
				for _, line := range source.Lines {
					sites = append(sites, LineProfileCallSite{Routine: meta.name, Line: line, Procedure: source.Name, Reason: lpReasonBare})
				}
			}
		}
		sort.SliceStable(sites, func(i, j int) bool { return sites[i].Line < sites[j].Line })
		all = append(all, sites...)
	}
	return all
}

// procedureCursorText returns the text to prepare for SELECT node n. A
// native FOR SELECT over a procedure gets FOR UPDATE when the procedure or
// the loop body may write (ibg-mkx): the client then fetches one row at a
// time, so the procedure runs no further ahead of the body than native PSQL
// lets it, at the price of a round trip per row. The transaction must be
// writable (it is whenever the routine may write). When FOR UPDATE does not
// prepare, the plain text is used and runsAhead is the call site to label.
func (r *lpRun) procedureCursorText(f *lpFrame, n *psql.Node, node *LineProfileNode) (string, *LineProfileCallSite) {
	text := n.SQL.Text
	if n.Kind != psql.NodeForSelect || r.opts.ReadOnly || hasForUpdate(text) {
		return text, nil
	}
	procedure, ok := r.procedureCursorWrites(f.meta, n)
	if !ok {
		return text, nil
	}
	if _, err := r.bindQuiet(f, node, text+" FOR UPDATE", n.SQL.Params, false); err == nil {
		return text + " FOR UPDATE", nil
	}
	return text, &LineProfileCallSite{Routine: f.meta.name, Line: n.Line, Procedure: procedure, Reason: lpReasonRunsAhead}
}

// procedureCursorWrites returns the first procedure that FOR SELECT node n
// of meta selects from, and whether that cursor needs row-by-row fetches:
// one of the procedures may write (a procedure not proved read-only by the
// analysis is assumed to) or the loop body may write. The procedures are
// the call sites on the lines from the statement's start to its body's
// first line.
func (r *lpRun) procedureCursorWrites(meta *lpRoutineMeta, n *psql.Node) (string, bool) {
	body := n.Children[0]
	var procedures []string
	for _, site := range r.procedureSites(meta) {
		if site.line >= n.Line && site.line <= body.Line {
			procedures = append(procedures, site.name)
		}
	}
	if len(procedures) == 0 {
		return "", false
	}
	if r.bodyWrites(meta, body) {
		return procedures[0], true
	}
	for _, name := range procedures {
		if callee := r.plan.procs[name]; callee == nil || !callee.readsOnly {
			return name, true
		}
	}
	return "", false
}

// lpSite is a procedure reference of a routine.
type lpSite struct {
	name string
	line int
}

// procedureSites lists the procedure references of meta: calls with an
// argument list and bare FROM names that the analysis resolved to
// procedures.
func (r *lpRun) procedureSites(meta *lpRoutineMeta) []lpSite {
	if sites, ok := r.sites[meta]; ok {
		return sites
	}
	var sites []lpSite
	for _, site := range meta.routine.CallSites() {
		sites = append(sites, lpSite{name: site.Name, line: site.Line})
	}
	for _, source := range meta.routine.SourceNames() {
		if _, isProcedure := r.plan.procs[source.Name]; isProcedure {
			for _, line := range source.Lines {
				sites = append(sites, lpSite{name: source.Name, line: line})
			}
		}
	}
	if r.sites == nil {
		r.sites = make(map[*lpRoutineMeta][]lpSite)
	}
	r.sites[meta] = sites
	return sites
}

// bodyWrites reports whether a loop body of meta may write: it contains a
// data statement, EXECUTE PROCEDURE or EXECUTE STATEMENT, or references a
// procedure.
func (r *lpRun) bodyWrites(meta *lpRoutineMeta, body *psql.Node) bool {
	for _, site := range r.procedureSites(meta) {
		if site.line >= body.Line && site.line <= body.EndLine {
			return true
		}
	}
	var walk func(n *psql.Node) bool
	walk = func(n *psql.Node) bool {
		switch n.Kind {
		case psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete, psql.NodeExecProc, psql.NodeExecStmt, psql.NodeForExecStmt:
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
	return walk(body)
}

// hasForUpdate reports whether a SELECT already ends with FOR UPDATE at
// parenthesis depth 0.
func hasForUpdate(text string) bool {
	tokens := topLevelTokens(text)
	for i := 1; i < len(tokens); i++ {
		if strings.EqualFold(text[tokens[i-1].start:tokens[i-1].end], "FOR") &&
			strings.EqualFold(text[tokens[i].start:tokens[i].end], "UPDATE") {
			return true
		}
	}
	return false
}

// stepsInto reports whether a SELECT … INTO or FOR SELECT node calls a
// procedure marked for step-in in a step-in shape.
func (r *lpRun) stepsInto(n *psql.Node) bool {
	return n.Proc != nil && r.stepInto[n.Proc.Name]
}

func (r *lpRun) newVar(typeText string) *lpVar {
	v := &lpVar{typ: psql.ParseTypeText(typeText)}
	v.stringsLocal = isStringFamily(v.typ) && stringsLocal(typeText, r.dbNone)
	return v
}

// calleeArgs evaluates a stepped-into call's arguments for the callee's
// input parameters as the native call would (R17): a variable of the
// parameter's exact type (or a BLOB for a BLOB) is passed as is; any other
// argument is evaluated and converted to the parameter type, locally when
// exact, otherwise on the server. InterBase refuses to compile a call with
// fewer arguments than inputs ("parameter mismatch", verified natively), so
// the count must match.
func (r *lpRun) calleeArgs(f *lpFrame, node *LineProfileNode, proc *psql.ProcCall, callee *lpRoutineMeta) ([]any, error) {
	if len(proc.Args) != len(callee.inputs) {
		return nil, fmt.Errorf("interbase: line profile: %d arguments for %d parameters of %s",
			len(proc.Args), len(callee.inputs), callee.name)
	}
	values := make([]any, len(proc.Args))
	for i, arg := range proc.Args {
		typeText := callee.inputs[i].typeText
		if local, ok := arg.Local.(psql.LocalVar); ok {
			if v, ok := f.vars[local.Name]; ok && (r.declaredType(v.typ.Text) == r.declaredType(typeText) ||
				isBlobType(v.typ.Text) && isBlobType(typeText)) {
				node.Local++
				values[i] = v.value
				continue
			}
		}
		value, err := r.evalValue(f, node, arg, r.newVar(typeText))
		if err != nil {
			return nil, err
		}
		values[i] = value
	}
	return values, nil
}

// enterFrame starts a frame for a stepped-into call of callee from node n
// of caller, with the given input values.
func (r *lpRun) enterFrame(caller *lpFrame, n *psql.Node, callee *lpRoutineMeta, args []any) (*lpFrame, error) {
	if caller.depth >= lpMaxDepth {
		return nil, fatal(fmt.Errorf("interbase: line profile: stepping into %s exceeds the depth limit of %d", callee.name, lpMaxDepth))
	}
	key := lpCalleeKey{name: callee.name, parent: caller.routine, line: n.Line}
	index, ok := r.calleeIndex[key]
	if !ok {
		profile := newRoutineProfile(callee)
		profile.Parent, profile.CallLine = caller.routine, n.Line
		r.routines = append(r.routines, &profile)
		index = len(r.routines) - 1
		if r.calleeIndex == nil {
			r.calleeIndex = make(map[lpCalleeKey]int)
		}
		r.calleeIndex[key] = index
	}
	profile := r.routines[index]
	profile.Calls++
	r.frames++
	g := newFrame(callee, r.dbNone, profile)
	g.routine, g.id, g.depth, g.guards = index, r.frames, caller.depth+1, caller.guards
	if r.active == nil {
		r.active = make(map[*lpRoutineMeta]int)
	}
	g.level = r.active[callee]
	r.active[callee]++
	for i, p := range callee.inputs {
		g.vars[p.name].value = args[i]
	}
	return g, nil
}

func (r *lpRun) leaveFrame(g *lpFrame) {
	r.active[g.meta]--
}

// escaped marks an error leaving a stepped-into routine as the call
// statement's, which never keeps its handling block's work (lpNodeError);
// the location stays the callee's.
func escaped(err error) error {
	var located *lpNodeError
	if !errors.As(err, &located) || !located.keepsBlock {
		return err
	}
	if direct, ok := err.(*lpNodeError); ok {
		err = direct.err
	}
	return &lpNodeError{routine: located.routine, line: located.line, err: err}
}

// stepExecute runs EXECUTE PROCEDURE of a procedure marked for step-in as a
// new frame, then assigns its outputs to the RETURNING_VALUES targets.
func (r *lpRun) stepExecute(f *lpFrame, n *psql.Node, node *LineProfileNode, callee *lpRoutineMeta) error {
	args, err := r.calleeArgs(f, node, n.Proc, callee)
	if err != nil {
		return err
	}
	g, err := r.enterFrame(f, n, callee, args)
	if err != nil {
		return err
	}
	_, err = r.exec(g, callee.routine.Body)
	r.leaveFrame(g)
	if err != nil {
		return escaped(err)
	}
	if len(n.Returning) > len(callee.outputs) {
		return fmt.Errorf("interbase: line profile: %d RETURNING_VALUES targets for %d outputs of %s",
			len(n.Returning), len(callee.outputs), callee.name)
	}
	for i, target := range n.Returning {
		out := callee.outputs[i]
		if v, ok := f.vars[target]; ok {
			if err := blobVariableInto(g.vars[out.name].value, out.typeText, v); err != nil {
				return err
			}
		}
		if err := r.assignTyped(f, node, target, g.vars[out.name].value, out.typeText); err != nil {
			return err
		}
	}
	return nil
}

// calleeColumns returns the outputs a step-in SELECT reads: every output
// for "*", otherwise the named ones in order.
func calleeColumns(proc *psql.ProcCall, callee *lpRoutineMeta) ([]lpParam, error) {
	if proc.Columns == nil {
		return callee.outputs, nil
	}
	columns := make([]lpParam, len(proc.Columns))
	for i, name := range proc.Columns {
		found := false
		for _, p := range callee.outputs {
			if p.name == name {
				columns[i], found = p, true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("interbase: line profile: %s is not an output of %s", name, callee.name)
		}
	}
	return columns, nil
}

// openCallee starts a stepped-into selectable call as a coroutine (spec
// 6.7) whose rows are its SUSPENDed outputs. The caller must close it.
func (r *lpRun) openCallee(f *lpFrame, n *psql.Node, node *LineProfileNode) (*lpCoroutine, []lpParam, error) {
	callee, ok := r.plan.procs[n.Proc.Name]
	if !ok {
		return nil, nil, fmt.Errorf("interbase: line profile: procedure %s is not loaded", n.Proc.Name)
	}
	columns, err := calleeColumns(n.Proc, callee)
	if err != nil {
		return nil, nil, err
	}
	args, err := r.calleeArgs(f, node, n.Proc, callee)
	if err != nil {
		return nil, nil, err
	}
	g, err := r.enterFrame(f, n, callee, args)
	if err != nil {
		return nil, nil, err
	}
	g.columns = columns
	co := startCoroutine(func(suspend func([]any) error) error {
		g.suspend = suspend
		_, err := r.exec(g, callee.routine.Body)
		return err
	}, func() { r.leaveFrame(g) })
	return co, columns, nil
}

// row returns the current values of a stepped-into selectable routine's
// selected outputs.
func (f *lpFrame) row() []any {
	row := make([]any, len(f.columns))
	for i, p := range f.columns {
		row[i] = f.vars[p.name].value
	}
	return row
}

// closeCallee closes a coroutine, adding a failure of its unwinding (the
// emulator's own work, which no handler catches) to err.
func closeCallee(co *lpCoroutine, err error) error {
	if closeErr := co.close(); closeErr != nil {
		return errors.Join(err, fatal(closeErr))
	}
	return err
}

// assignColumns assigns a stepped-into routine's row to targets.
func (r *lpRun) assignColumns(f *lpFrame, node *LineProfileNode, targets []string, row []any, columns []lpParam) error {
	if len(targets) > len(row) {
		return fmt.Errorf("interbase: line profile: %d targets for %d columns", len(targets), len(row))
	}
	for i, target := range targets {
		if err := r.assignTyped(f, node, target, row[i], columns[i].typeText); err != nil {
			return err
		}
	}
	return nil
}

// stepSelectInto runs a singleton SELECT from a stepped-into routine. As the
// server does, it fetches a second row to detect a multiple-row result, so
// the callee runs until its second SUSPEND or its end.
func (r *lpRun) stepSelectInto(f *lpFrame, n *psql.Node, node *LineProfileNode) (err error) {
	co, columns, err := r.openCallee(f, n, node)
	if err != nil {
		return err
	}
	defer func() { err = closeCallee(co, err) }()
	row, ok, err := co.next()
	if err != nil {
		return escaped(err)
	}
	if !ok {
		return nil
	}
	node.Metrics.Rows++
	_, more, err := co.next()
	if err != nil {
		return escaped(err)
	}
	if more {
		node.Metrics.Rows++
		node.Metrics.Errors++
		return errSingleton()
	}
	return r.assignColumns(f, node, n.Into, row, columns)
}

// stepForSelect runs FOR SELECT … DO over a stepped-into routine's rows.
func (r *lpRun) stepForSelect(f *lpFrame, n *psql.Node, node *LineProfileNode) (sig lpSignal, err error) {
	co, columns, err := r.openCallee(f, n, node)
	if err != nil {
		return lpContinue, err
	}
	defer func() { err = closeCallee(co, err) }()
	for {
		row, ok, err := co.next()
		if err != nil {
			return lpContinue, escaped(err)
		}
		if !ok {
			return lpContinue, nil
		}
		node.Metrics.Rows++
		if err := r.assignColumns(f, node, n.Into, row, columns); err != nil {
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

// lpCoroutine runs a stepped-into selectable routine on its own goroutine,
// strictly alternating with its caller over unbuffered channels: exactly
// one side runs at any time, so the connection, the transaction and the run
// state are never used concurrently, and every channel operation orders the
// two sides' memory accesses.
type lpCoroutine struct {
	resume   chan bool // caller → routine: true abandons it
	yield    chan lpYield
	finished chan struct{} // closed when the goroutine has returned
	done     bool
}

// lpYield is a row of the routine, or its end with its error.
type lpYield struct {
	row  []any
	err  error
	done bool
}

// startCoroutine starts body, which hands rows to the caller through
// suspend, on a goroutine that waits for the first next. cleanup runs on
// that goroutine when it ends, whether body ran or not.
func startCoroutine(body func(suspend func([]any) error) error, cleanup func()) *lpCoroutine {
	co := &lpCoroutine{resume: make(chan bool), yield: make(chan lpYield), finished: make(chan struct{})}
	go co.run(body, cleanup)
	return co
}

func (co *lpCoroutine) run(body func(suspend func([]any) error) error, cleanup func()) {
	defer close(co.finished)
	err := errLPAbandon
	if abandon := <-co.resume; !abandon {
		err = protect(func() error { return body(co.suspend) })
	}
	cleanup()
	co.yield <- lpYield{err: err, done: true}
}

// protect turns a panic of body into an error, so that a stepped-into
// routine never takes the process down.
func protect(body func() error) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fatal(fmt.Errorf("interbase: line profile: stepped-into routine panicked: %v", p))
		}
	}()
	return body()
}

// suspend hands row to the caller and waits until it asks for the next
// one; errLPAbandon means the caller closed the routine.
func (co *lpCoroutine) suspend(row []any) error {
	co.yield <- lpYield{row: row}
	if abandon := <-co.resume; abandon {
		return errLPAbandon
	}
	return nil
}

// next runs the routine to its next row; ok is false at its end, with the
// routine's error.
func (co *lpCoroutine) next() (row []any, ok bool, err error) {
	if co.done {
		return nil, false, nil
	}
	co.resume <- false
	y := <-co.yield
	if y.done {
		co.done = true
		<-co.finished
		return nil, false, y.err
	}
	return y.row, true, nil
}

// close abandons a routine that has not ended and waits for its goroutine.
// The error is a failure while the routine unwinds.
func (co *lpCoroutine) close() error {
	if co.done {
		return nil
	}
	co.done = true
	co.resume <- true
	y := <-co.yield
	for !y.done {
		co.resume <- true
		y = <-co.yield
	}
	<-co.finished
	if isAbandon(y.err) {
		return nil
	}
	return y.err
}

// isAbandon reports an errLPAbandon, located at the nodes it unwound and
// joined with no other failure.
func isAbandon(err error) bool {
	switch e := err.(type) {
	case *lpNodeError:
		return isAbandon(e.err)
	case interface{ Unwrap() []error }:
		errs := e.Unwrap()
		for _, inner := range errs {
			if !isAbandon(inner) {
				return false
			}
		}
		return len(errs) > 0
	}
	return err == errLPAbandon
}
