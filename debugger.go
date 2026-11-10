package interbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"interbase-go/psql"
)

// DebugBreakpoint is a breakpoint on the statement starting on Line of Routine
// (routine names as stored: upper case unless created quoted).
type DebugBreakpoint struct {
	Routine string
	Line    int
}

// DebugRoutine is a routine the session can show: its source and the lines on
// which a statement starts (valid breakpoint and run-to lines).
type DebugRoutine struct {
	Name           string
	Source         string
	StatementLines []int
}

// DebugAnalysis is the static analysis of a debug entry statement.
type DebugAnalysis struct {
	LineProfileAnalysis
	ReadWriteFromStart bool
	ReadWriteReason    string
	Routines           []DebugRoutine
}

// AnalyzeDebug reads routine metadata in a short read-only transaction and
// returns the entry and its parseable, step-in-reachable routines.
func AnalyzeDebug(ctx context.Context, pooled *sql.Conn, entrySQL string, dialect int) (DebugAnalysis, error) {
	plan, err := planLineProfile(ctx, pooled, entrySQL, dialect, nil, true)
	if err != nil {
		return DebugAnalysis{}, err
	}
	return debugAnalysisFromPlan(plan), nil
}

// DebugOptions controls StartDebug.
type DebugOptions struct {
	SQL          string
	Dialect      int
	KeepRows     int
	MaxBlobBytes int64
	Breakpoints  []DebugBreakpoint
	AllowWrites  bool
}

var (
	ErrDebugRefused          = errors.New("interbase: debug refused")
	ErrDebugWritesNotAllowed = errors.New("interbase: debug needs write confirmation")
	ErrDebugNotPaused        = errors.New("interbase: debug session is not paused")
	ErrDebugEnded            = errors.New("interbase: debug session has ended")
)

type DebugCommandKind string

const (
	DebugContinue DebugCommandKind = "continue"
	DebugStepOver DebugCommandKind = "stepOver"
	DebugStepInto DebugCommandKind = "stepInto"
	DebugStepOut  DebugCommandKind = "stepOut"
	DebugRunTo    DebugCommandKind = "runTo"
	DebugPause    DebugCommandKind = "pause"
)

type DebugCommand struct {
	Kind    DebugCommandKind
	Routine string
	Line    int
}

type DebugStatus string

const (
	DebugPaused      DebugStatus = "paused"
	DebugRunning     DebugStatus = "running"
	DebugEndedStatus DebugStatus = "ended"
)

type DebugOutcome string

const (
	DebugCompleted DebugOutcome = "completed"
	DebugFailed    DebugOutcome = "failed"
	DebugStopped   DebugOutcome = "stopped"
)

type DebugPhase string

const (
	DebugReadOnly  DebugPhase = "readOnly"
	DebugReadWrite DebugPhase = "readWrite"
	DebugWritten   DebugPhase = "written"
)

// DebugValue is one variable; Value is the driver's exact value (nil = NULL),
// BLOBs held as string (text) or []byte.
type DebugValue struct {
	Name, Type string
	Value      any
	Changed    bool
}

// DebugCursor is an open FOR SELECT / FOR EXECUTE STATEMENT of a frame.
type DebugCursor struct {
	Line    int
	Fetched int64
	Targets []string
}

type DebugFrame struct {
	Routine                          string
	Kind                             string
	Line, Column, EndLine, EndColumn int
	Inputs, Outputs, Locals          []DebugValue
	Cursors                          []DebugCursor
}

type DebugMessage struct {
	Kind    string
	Text    string
	Routine string
	Line    int
}

// DebugSnapshot is published on every state change.
type DebugSnapshot struct {
	Seq        int64
	Status     DebugStatus
	Frames     []DebugFrame
	Phase      DebugPhase
	Mixed      string
	Columns    []string
	Rows       [][]Cell
	RowsTotal  int64
	Statements int64
	Messages   []DebugMessage
	Outcome    DebugOutcome
	Err        error
	FailedAt   *LineProfileLocation
}

type DebugEvaluation struct {
	Expression string
	Type       string
	Value      any
	Err        error
}

// DebugSession is one pinned, always-rolled-back debug run.
type DebugSession struct {
	analysis DebugAnalysis
	opts     DebugOptions

	ctx    context.Context
	cancel context.CancelFunc

	mu             sync.Mutex
	latest         DebugSnapshot
	snapshots      chan DebugSnapshot
	commands       chan DebugCommand
	evals          chan debugEvalRequest
	done           chan struct{}
	debug          *debugState
	run            *lpRun
	commandSent    bool
	evaluationSent bool
	ended          bool
	endOnce        sync.Once
	closeOnce      sync.Once
}

type debugEvalRequest struct {
	ctx         context.Context
	frame       int
	expressions []string
	result      chan debugEvalResult
}

type debugEvalResult struct {
	evaluations []DebugEvaluation
	err         error
}

type debugState struct {
	session *DebugSession
	run     *lpRun

	mode         DebugCommandKind
	firstStop    bool
	currentFrame *lpFrame
	stopDepth    int
	runTo        DebugBreakpoint
	stepFrameID  int
	stepNode     *psql.Node

	pauseRequested atomic.Bool
	breakpoints    map[DebugBreakpoint]struct{}
	previous       map[int]map[string]any
	statements     int64
	phase          DebugPhase
	mixed          string
	messages       []DebugMessage
	pending        []string
	debugForceLazy bool // offline safety-net test seam
}

func newDebugSession(ctx context.Context, analysis DebugAnalysis, opts DebugOptions) *DebugSession {
	if ctx == nil {
		ctx = context.Background()
	}
	sessionCtx, cancel := context.WithCancel(ctx)
	s := &DebugSession{
		analysis: cloneDebugAnalysis(analysis), opts: opts,
		ctx: sessionCtx, cancel: cancel,
		snapshots: make(chan DebugSnapshot, 64),
		commands:  make(chan DebugCommand),
		evals:     make(chan debugEvalRequest),
		done:      make(chan struct{}),
		latest:    DebugSnapshot{Status: DebugRunning},
	}
	phase := DebugReadOnly
	if analysis.ReadWriteFromStart {
		phase = DebugReadWrite
	}
	s.debug = &debugState{
		session: s, mode: DebugContinue, breakpoints: make(map[DebugBreakpoint]struct{}),
		previous: make(map[int]map[string]any), phase: phase,
	}
	s.SetBreakpoints(opts.Breakpoints)
	return s
}

// Analysis returns the immutable start analysis.
func (s *DebugSession) Analysis() DebugAnalysis {
	return cloneDebugAnalysis(s.analysis)
}

// Snapshots returns snapshots in order and closes after the session ends. The
// ended snapshot may be dropped if the buffer is full; after the channel closes,
// Latest returns that ended snapshot.
func (s *DebugSession) Snapshots() <-chan DebugSnapshot { return s.snapshots }

// Latest returns the latest published snapshot.
func (s *DebugSession) Latest() DebugSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneDebugSnapshot(s.latest)
}

// Command sends one command to a paused session. Pause is the exception: it
// requests a stop at the next hook while execution is running.
func (s *DebugSession) Command(cmd DebugCommand) error {
	if s == nil {
		return ErrDebugEnded
	}
	s.mu.Lock()
	if s.ended || s.latest.Status == DebugEndedStatus {
		s.mu.Unlock()
		return ErrDebugEnded
	}
	if cmd.Kind == DebugPause {
		if s.latest.Status != DebugRunning && !s.commandSent {
			s.mu.Unlock()
			return ErrDebugNotPaused
		}
		s.debug.pauseRequested.Store(true)
		s.mu.Unlock()
		return nil
	}
	if s.latest.Status != DebugPaused || s.commandSent || s.evaluationSent {
		s.mu.Unlock()
		return ErrDebugNotPaused
	}
	if !validDebugCommand(cmd.Kind) {
		s.mu.Unlock()
		return fmt.Errorf("interbase: unknown debug command %q", cmd.Kind)
	}
	if cmd.Kind == DebugRunTo && !s.validBreakpoint(DebugBreakpoint{Routine: cmd.Routine, Line: cmd.Line}) {
		s.mu.Unlock()
		return fmt.Errorf("interbase: debug run-to target %s line %d is not a statement", cmd.Routine, cmd.Line)
	}
	s.commandSent = true
	s.mu.Unlock()
	select {
	case s.commands <- cmd:
		return nil
	case <-s.done:
		s.mu.Lock()
		s.commandSent = false
		s.mu.Unlock()
		return ErrDebugEnded
	case <-s.ctx.Done():
		return ErrDebugEnded
	}
}

func validDebugCommand(kind DebugCommandKind) bool {
	switch kind {
	case DebugContinue, DebugStepOver, DebugStepInto, DebugStepOut, DebugRunTo:
		return true
	default:
		return false
	}
}

// SetBreakpoints replaces the active breakpoint set and returns accepted
// entries whose line begins a statement in an analyzed routine.
func (s *DebugSession) SetBreakpoints(bps []DebugBreakpoint) []DebugBreakpoint {
	if s == nil {
		return nil
	}
	accepted := make([]DebugBreakpoint, 0, len(bps))
	seen := make(map[DebugBreakpoint]bool, len(bps))
	for _, bp := range bps {
		if seen[bp] || !s.validBreakpoint(bp) {
			continue
		}
		seen[bp] = true
		accepted = append(accepted, bp)
	}
	s.mu.Lock()
	clear(s.debug.breakpoints)
	for _, bp := range accepted {
		s.debug.breakpoints[bp] = struct{}{}
	}
	s.mu.Unlock()
	return accepted
}

func (s *DebugSession) validBreakpoint(bp DebugBreakpoint) bool {
	for _, routine := range s.analysis.Routines {
		if routine.Name != bp.Routine {
			continue
		}
		for _, line := range routine.StatementLines {
			if line == bp.Line {
				return true
			}
		}
	}
	return false
}

// Evaluate queues an evaluation request to the paused hook. Admission is
// serialized against resume commands. Expression execution is added by the
// evaluation task; the request/response path is already serialized with the run.
func (s *DebugSession) Evaluate(ctx context.Context, frame int, expressions []string) ([]DebugEvaluation, error) {
	if s == nil {
		return nil, ErrDebugEnded
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.ended || s.latest.Status == DebugEndedStatus {
		s.mu.Unlock()
		return nil, ErrDebugEnded
	}
	if s.latest.Status != DebugPaused || s.commandSent || s.evaluationSent {
		s.mu.Unlock()
		return nil, ErrDebugNotPaused
	}
	s.evaluationSent = true
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.evaluationSent = false
		s.mu.Unlock()
	}()
	req := debugEvalRequest{ctx: ctx, frame: frame, expressions: append([]string(nil), expressions...), result: make(chan debugEvalResult, 1)}
	select {
	case s.evals <- req:
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrDebugEnded
	}
	select {
	case result := <-req.result:
		return result.evaluations, result.err
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-s.done:
		return nil, ErrDebugEnded
	}
}

// Stop cancels any in-flight server call, waits for statement cleanup and
// rollback, and is safe to call more than once.
func (s *DebugSession) Stop() {
	if s == nil {
		return
	}
	s.cancel()
	<-s.done
}

// Done closes when the session's final snapshot and transaction cleanup are
// complete.
func (s *DebugSession) Done() <-chan struct{} { return s.done }

// StartDebug analyzes opts.SQL before opening the run transaction, refuses
// static problems or unconfirmed writes, then runs on a pinned connection in
// its own goroutine and stops before the first statement.
func StartDebug(ctx context.Context, pooled *sql.Conn, opts DebugOptions) (*DebugSession, error) {
	if pooled == nil {
		return nil, ErrNotInterBaseConn
	}
	if opts.KeepRows < 0 {
		return nil, fmt.Errorf("interbase: debug KeepRows must not be negative: %d", opts.KeepRows)
	}
	if opts.KeepRows == 0 {
		opts.KeepRows = 200
	}
	if opts.Dialect == 0 {
		opts.Dialect = 3
	}
	if opts.Dialect != 1 && opts.Dialect != 3 {
		return nil, fmt.Errorf("interbase: debug SQL dialect must be 1 or 3: %d", opts.Dialect)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	plan, err := planLineProfile(ctx, pooled, opts.SQL, opts.Dialect, nil, true)
	if err != nil {
		return nil, err
	}
	if len(plan.analysis.Problems) != 0 {
		messages := make([]string, len(plan.analysis.Problems))
		for i, problem := range plan.analysis.Problems {
			messages[i] = problem.String()
		}
		return nil, fmt.Errorf("%w: %s", ErrDebugRefused, strings.Join(messages, "; "))
	}
	analysis := debugAnalysisFromPlan(plan)
	if analysis.MayWrite && !opts.AllowWrites {
		reason := analysis.ReadWriteReason
		if reason == "" {
			reason = "routine may write"
		}
		return nil, fmt.Errorf("%w: %s", ErrDebugWritesNotAllowed, reason)
	}
	s := newDebugSession(ctx, analysis, opts)
	go s.runDebug(pooled, plan)
	return s, nil
}

func (s *DebugSession) runDebug(pooled *sql.Conn, plan *lpPlan) {
	var beginErr error
	rawErr := pooled.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			beginErr = ErrNotInterBaseConn
			return nil
		}
		readWriteFromStart := s.analysis.ReadWriteFromStart
		result := &LineProfileResult{
			ReadOnly: !readWriteFromStart,
			Columns:  entryColumns(plan),
			Routines: []LineProfileRoutine{newRoutineProfile(plan.entry)},
		}
		lineOpts := LineProfileOptions{
			SQL: s.opts.SQL, Dialect: s.opts.Dialect, ReadOnly: !readWriteFromStart,
			KeepRows: s.opts.KeepRows, MaxBlobBytes: s.opts.MaxBlobBytes,
		}
		txOptions := TransactionOptions{ReadOnly: !readWriteFromStart, Isolation: sql.LevelReadCommitted, NoWait: readWriteFromStart}
		run, err := beginRunWithTransactionOptions(s.ctx, c, plan, lineOpts, result, debugStepIntoSet(plan), txOptions)
		if err != nil {
			beginErr = err
			return nil
		}
		if readWriteFromStart {
			reason := s.analysis.ReadWriteReason
			if reason == "" {
				reason = "writes may occur inside an open cursor"
			}
			s.debug.addMessage(DebugMessage{Kind: "info", Text: "Started read-write: " + reason + "."})
		} else {
			s.debug.addMessage(DebugMessage{Kind: "info", Text: "Started read-only."})
		}
		s.attachRun(run)
		outcome, txErr := run.runInTransaction()
		s.finishRun(run, outcome, txErr)
		return nil
	})
	if rawErr != nil {
		s.finishStartError(rawErr)
		return
	}
	if beginErr != nil {
		s.finishStartError(beginErr)
	}
}

func (s *DebugSession) attachRun(run *lpRun) {
	s.mu.Lock()
	s.run, s.debug.run = run, run
	run.ctx = s.ctx
	run.noMeasure = true
	run.stepInto = debugStepIntoSet(run.plan)
	run.stepDecider = s.debug.shouldStepInto
	run.debug = s.debug
	run.hook = s.debug.hook
	phase := s.debug.phase
	mixed := s.debug.mixed
	messages := append([]DebugMessage(nil), s.debug.messages...)
	s.mu.Unlock()
	s.publish(DebugSnapshot{Status: DebugRunning, Phase: phase, Mixed: mixed, Messages: messages})
}

func (s *DebugSession) finishStartError(err error) {
	s.endOnce.Do(func() {
		at, unwrapped := unwrapRunError(err)
		s.mu.Lock()
		s.ended = true
		phase := s.debug.phase
		mixed := s.debug.mixed
		messages := append([]DebugMessage(nil), s.debug.messages...)
		s.mu.Unlock()
		outcome := DebugFailed
		if s.ctx.Err() != nil || errors.Is(unwrapped, context.Canceled) || errors.Is(unwrapped, context.DeadlineExceeded) {
			outcome, unwrapped, at = DebugStopped, nil, nil
		}
		s.publish(DebugSnapshot{
			Status: DebugEndedStatus, Phase: phase, Mixed: mixed, Messages: messages,
			Outcome: outcome, Err: unwrapped, FailedAt: at,
		})
		close(s.done)
	})
}

func (s *DebugSession) finishRun(run *lpRun, outcomeErr, txErr error) {
	s.endOnce.Do(func() {
		location, runErr := unwrapRunError(outcomeErr)
		if txErr != nil {
			runErr = errors.Join(runErr, txErr)
		}
		outcome := DebugCompleted
		if s.ctx.Err() != nil || errors.Is(runErr, context.Canceled) || errors.Is(runErr, context.DeadlineExceeded) {
			outcome, location = DebugStopped, nil
			if txErr == nil {
				runErr = nil
			} else {
				runErr = txErr
			}
		} else if runErr != nil {
			outcome = DebugFailed
		}
		s.mu.Lock()
		s.ended = true
		phase := s.debug.phase
		mixed := s.debug.mixed
		messages := append([]DebugMessage(nil), s.debug.messages...)
		s.mu.Unlock()
		snap := DebugSnapshot{
			Status: DebugEndedStatus, Phase: phase, Mixed: mixed, Messages: messages,
			Outcome: outcome, Err: runErr, FailedAt: location,
		}
		if run != nil && run.result != nil {
			snap.Columns = append([]string(nil), run.result.Columns...)
			snap.Rows = cloneRows(run.result.KeptRows)
			snap.RowsTotal = run.result.Rows
		}
		s.mu.Lock()
		snap.Statements = s.debug.statements
		s.mu.Unlock()
		s.publish(snap)
		close(s.done)
	})
}

func (d *debugState) countStatement() {
	d.session.mu.Lock()
	d.statements++
	d.session.mu.Unlock()
}

func (d *debugState) didExecute(n *psql.Node) {
	switch n.Kind {
	case psql.NodeInsert, psql.NodeUpdate, psql.NodeDelete, psql.NodeExecStmt, psql.NodeForExecStmt:
		d.session.mu.Lock()
		if d.phase == DebugReadWrite {
			d.phase = DebugWritten
		}
		d.session.mu.Unlock()
	}
}

func (d *debugState) didNativeWrite() {
	d.session.mu.Lock()
	if d.phase == DebugReadWrite {
		d.phase = DebugWritten
	}
	d.session.mu.Unlock()
}

func (d *debugState) addMessage(message DebugMessage) {
	d.session.mu.Lock()
	d.messages = append(d.messages, message)
	d.session.mu.Unlock()
}

func (d *debugState) hook(f *lpFrame, n *psql.Node) error {
	if err := d.session.ctx.Err(); err != nil {
		return err
	}
	if d.shouldStop(f, n) {
		snapshot := buildDebugSnapshot(d, f, n, DebugPaused)
		d.session.publish(snapshot)
		if err := d.pauseLoop(f, n); err != nil {
			return err
		}
	}
	d.session.mu.Lock()
	readOnly := d.phase == DebugReadOnly
	if readOnly {
		d.currentFrame = f
	}
	d.session.mu.Unlock()
	if readOnly {
		mayWrite := d.run.nodeMayWrite(f.meta, n)
		d.session.mu.Lock()
		d.currentFrame = nil
		d.session.mu.Unlock()
		if mayWrite {
			return d.run.switchToReadWrite(f, n)
		}
	}
	return nil
}

func (d *debugState) shouldStop(f *lpFrame, n *psql.Node) bool {
	s := d.session
	s.mu.Lock()
	defer s.mu.Unlock()
	if !d.firstStop {
		d.firstStop = true
		return true
	}
	if _, ok := d.breakpoints[DebugBreakpoint{Routine: f.meta.name, Line: n.Line}]; ok {
		return true
	}
	if d.pauseRequested.Swap(false) {
		return true
	}
	switch d.mode {
	case DebugStepInto:
		return true
	case DebugStepOver:
		return f.depth <= d.stopDepth
	case DebugStepOut:
		return f.depth < d.stopDepth
	case DebugRunTo:
		return f.meta.name == d.runTo.Routine && n.Line == d.runTo.Line
	default:
		return false
	}
}

func (d *debugState) pauseLoop(f *lpFrame, n *psql.Node) error {
	for {
		select {
		case cmd := <-d.session.commands:
			d.acceptCommand(cmd, f, n)
			d.session.publish(buildDebugSnapshot(d, nil, nil, DebugRunning))
			d.session.mu.Lock()
			d.session.commandSent = false
			d.session.mu.Unlock()
			return nil
		case req := <-d.session.evals:
			d.serveEvaluation(req)
		case <-d.session.ctx.Done():
			return d.session.ctx.Err()
		}
	}
}

func (d *debugState) acceptCommand(cmd DebugCommand, f *lpFrame, n *psql.Node) {
	d.session.mu.Lock()
	defer d.session.mu.Unlock()
	d.stopDepth = f.depth
	d.stepFrameID, d.stepNode = 0, nil
	d.runTo = DebugBreakpoint{}
	d.mode = cmd.Kind
	switch cmd.Kind {
	case DebugStepInto:
		d.stepFrameID, d.stepNode = f.id, n
	case DebugStepOut:
		if f.depth == 0 {
			d.mode = DebugContinue
		}
	case DebugRunTo:
		d.runTo = DebugBreakpoint{Routine: cmd.Routine, Line: cmd.Line}
	}
}

func (d *debugState) shouldStepInto(f *lpFrame, n *psql.Node, callee string) bool {
	s := d.session
	s.mu.Lock()
	defer s.mu.Unlock()
	requested := d.mode == DebugStepInto && d.stepNode == n && f != nil && d.stepFrameID == f.id
	if requested {
		d.stepNode = nil
		if !d.run.stepInto[callee] {
			d.messages = append(d.messages, DebugMessage{
				Kind: "info", Text: callee + " ran on the server (it cannot be stepped into)",
				Routine: f.meta.name, Line: n.Line,
			})
			return false
		}
		return true
	}
	if !d.run.stepInto[callee] {
		return false
	}
	return d.reachableBreakpointOrRunTo(callee)
}

func (d *debugState) reachableBreakpointOrRunTo(name string) bool {
	queue := []string{name}
	seen := make(map[string]bool)
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seen[current] {
			continue
		}
		seen[current] = true
		for bp := range d.breakpoints {
			if bp.Routine == current {
				return true
			}
		}
		if d.mode == DebugRunTo && d.runTo.Routine == current {
			return true
		}
		meta := d.run.plan.procs[current]
		if meta == nil || meta.routine == nil {
			continue
		}
		for _, child := range meta.routine.Callees() {
			if child.StepInShape && d.run.stepInto[child.Name] && !seen[child.Name] {
				queue = append(queue, child.Name)
			}
		}
	}
	return false
}

func (d *debugState) serveEvaluation(req debugEvalRequest) {
	result := debugEvalResult{evaluations: make([]DebugEvaluation, len(req.expressions))}
	for i, expression := range req.expressions {
		result.evaluations[i] = DebugEvaluation{
			Expression: expression,
			Err:        errors.New("interbase: debug expression evaluation is not available yet"),
		}
	}
	select {
	case req.result <- result:
	case <-req.ctx.Done():
	}
}

func debugStepIntoSet(plan *lpPlan) map[string]bool {
	set := make(map[string]bool)
	if plan == nil || plan.entry == nil {
		return set
	}
	for _, routine := range debugReachableRoutines(plan) {
		if routine != plan.entry.name {
			set[routine] = true
		}
	}
	return set
}

func debugReachableRoutines(plan *lpPlan) []string {
	if plan == nil || plan.entry == nil || plan.entry.routine == nil {
		return nil
	}
	seen := map[string]bool{plan.entry.name: true}
	var reached []string
	queue := []*lpRoutineMeta{plan.entry}
	for len(queue) > 0 {
		meta := queue[0]
		queue = queue[1:]
		for _, callee := range meta.routine.Callees() {
			if !callee.StepInShape || seen[callee.Name] {
				continue
			}
			child := plan.procs[callee.Name]
			if child == nil || child.routine == nil || len(child.problems) != 0 || child.routine.Body == nil {
				continue
			}
			seen[callee.Name] = true
			reached = append(reached, callee.Name)
			queue = append(queue, child)
		}
	}
	sort.Strings(reached)
	return append([]string{plan.entry.name}, reached...)
}

func debugAnalysisFromPlan(plan *lpPlan) DebugAnalysis {
	if plan == nil {
		return DebugAnalysis{}
	}
	analysis := DebugAnalysis{LineProfileAnalysis: cloneLineProfileAnalysis(plan.analysis)}
	analysis.ReadWriteFromStart, analysis.ReadWriteReason = debugStartAnalysis(plan)
	for _, name := range debugReachableRoutines(plan) {
		meta := plan.procs[name]
		if meta == nil {
			continue
		}
		lines := make(map[int]bool)
		if meta.routine != nil {
			for _, node := range meta.routine.Nodes {
				if node.Kind != psql.NodeBlock && node.Kind != psql.NodeEmpty {
					lines[node.Line] = true
				}
			}
		}
		statementLines := make([]int, 0, len(lines))
		for line := range lines {
			statementLines = append(statementLines, line)
		}
		sort.Ints(statementLines)
		analysis.Routines = append(analysis.Routines, DebugRoutine{
			Name: meta.name, Source: meta.source, StatementLines: statementLines,
		})
	}
	return analysis
}

func cloneLineProfileAnalysis(a LineProfileAnalysis) LineProfileAnalysis {
	a.Entry.Columns = append([]string(nil), a.Entry.Columns...)
	a.Entry.Args = append([]string(nil), a.Entry.Args...)
	a.Problems = append([]RoutineProblem(nil), a.Problems...)
	a.Callees = append([]LineProfileCallee(nil), a.Callees...)
	for i := range a.Callees {
		a.Callees[i].Lines = append([]int(nil), a.Callees[i].Lines...)
	}
	return a
}

func cloneDebugAnalysis(a DebugAnalysis) DebugAnalysis {
	a.LineProfileAnalysis = cloneLineProfileAnalysis(a.LineProfileAnalysis)
	a.Routines = append([]DebugRoutine(nil), a.Routines...)
	for i := range a.Routines {
		a.Routines[i].StatementLines = append([]int(nil), a.Routines[i].StatementLines...)
	}
	return a
}

func (s *DebugSession) publish(snapshot DebugSnapshot) {
	s.mu.Lock()
	snapshot.Seq = s.latest.Seq + 1
	s.latest = cloneDebugSnapshot(snapshot)
	s.mu.Unlock()
	if snapshot.Status == DebugEndedStatus {
		select {
		case s.snapshots <- cloneDebugSnapshot(snapshot):
		default:
		}
		s.closeOnce.Do(func() { close(s.snapshots) })
		return
	}
	select {
	case s.snapshots <- cloneDebugSnapshot(snapshot):
	case <-s.ctx.Done():
	}
}
