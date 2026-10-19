package interbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"interbase-go/psql"
)

// LineProfileOptions controls LineProfile.
type LineProfileOptions struct {
	SQL      string   // entry statement (spec 4)
	Dialect  int      // connection dialect (1 or 3)
	ReadOnly bool     // caller's decision from AnalyzeLineProfile.MayWrite
	StepInto []string // procedures to emulate when called in a step-in shape (spec 6.7); names are upper-cased unless double-quoted
	KeepRows int      // default 10
	// MaxBlobBytes caps the bytes of a BLOB held in an emulated variable
	// (0 or a negative value means DefaultLineProfileMaxBlobBytes). A larger BLOB ends the run
	// as failed at its line ("BLOB too large to emulate"); a variable is
	// never truncated, and WHEN handlers cannot catch the failure.
	MaxBlobBytes int64
	// WarmUp runs one discarded profile pass before the measured run, on the
	// same attachment, so first-use costs (such as the server compiling a
	// procedure on its first call from the attachment) are paid outside the
	// measurement. The pass emulates the entry exactly as the measured run
	// does (same StepInto) in its own read-committed transaction, read-only
	// when ReadOnly, always rolled back, under the same ctx; everything it
	// measures is discarded and the measured run starts afresh. Effects that
	// are not transactional happen twice. See LineProfileResult.WarmUp and
	// WarmUpErr.
	WarmUp bool
}

// DefaultLineProfileMaxBlobBytes is the default LineProfileOptions.MaxBlobBytes.
const DefaultLineProfileMaxBlobBytes = 16 << 20

// LineProfileAnalysis is the static analysis of an entry statement.
type LineProfileAnalysis struct {
	Entry    psql.EntryCall
	Routine  string // entry procedure name
	MayWrite bool   // entry routine (and selected step-in callees) may write
	Problems []RoutineProblem
	Callees  []LineProfileCallee
}

// RoutineProblem is a reason a line profile is refused. Line is 0 for a
// problem of the entry statement itself.
type RoutineProblem struct {
	Routine      string
	Line, Column int
	Message      string
}

// LineProfileCallee is a procedure the profiled routines call. Parses
// reports whether its body passes the parser (a step-in requirement); Depth
// is 1 for the entry routine's callees.
type LineProfileCallee struct {
	Name        string
	Lines       []int
	StepInShape bool
	Parses      bool
	Depth       int
}

// LineProfileOutcome is how a line profile run ended.
type LineProfileOutcome string

const (
	LineProfileCompleted LineProfileOutcome = "completed"
	LineProfileFailed    LineProfileOutcome = "failed"
	LineProfileCanceled  LineProfileOutcome = "canceled" // ctx canceled or deadline exceeded; caller relabels "limit"
)

// LineMetrics are the measurements of a node, a line or a frame. Hits counts
// executions of a node, except for a WHILE node, whose Hits count condition
// evaluations (N+1 for a loop whose body runs N times).
//
// FirstRaw and FirstAdjusted are the part of Raw and Adjusted spent in a
// node's first hit (a WHILE node's first condition evaluation; a FOR
// SELECT's first execution with all its fetches), so that a first-use cost,
// such as the server compiling a native callee on its first call in the
// attachment, stays visible next to the total. A line's are the sum over
// its nodes. They are zero in a routine's Inclusive and Exclusive totals.
type LineMetrics struct {
	Hits, ServerCalls, RoundTrips, Rows, Errors int64
	Raw, Adjusted                               time.Duration
	FirstRaw, FirstAdjusted                     time.Duration
	Pages                                       *PageCounters         // nil when unavailable
	Tables                                      map[int]TableCounters // non-zero only
}

// LineProfileNode is one statement node of a profiled routine. See
// LineMetrics for how Hits count a WHILE node.
type LineProfileNode struct {
	ID            int
	Kind          string
	Line, EndLine int
	SQL           string // last SQL text sent for this node ("" when evaluated locally only)
	Local         int64  // evaluations and conversions done locally; conversions done by an in-statement CAST (R18) are not counted
	// DynamicSQL is the last statement text an EXECUTE STATEMENT or FOR
	// EXECUTE STATEMENT node evaluated, and DynamicTexts the number of
	// distinct texts it evaluated in the run; SQL is the text sent for it.
	// The node keeps its 32 most recently used texts prepared per call
	// depth of its routine, so a recursion of depth d can hold d × 32
	// prepared statements for the node until the run ends.
	DynamicSQL   string
	DynamicTexts int
	Metrics      LineMetrics
}

// LineProfileRoutine is one profiled routine: the entry routine, or the
// stepped-into invocations of a procedure from one call line of one parent
// entry, aggregated (Calls counts them). Parent is the index of that entry
// in LineProfileResult.Routines, always lower than this one's.
type LineProfileRoutine struct {
	Name                 string
	Source               string
	Parent               int // -1 for the entry routine
	CallLine             int // line in parent, 0 for entry
	Calls                int64
	Nodes                []LineProfileNode
	Lines                map[int]LineMetrics // sum of nodes starting on the line
	Inclusive, Exclusive LineMetrics
}

// RoundTripBaseline summarizes round-trip probe samples.
type RoundTripBaseline struct {
	Min, Median time.Duration
	Samples     int
}

// LineProfileLocation is the routine and source line where a failed run
// stopped.
type LineProfileLocation struct {
	Routine string
	Line    int
}

// LineProfileResult is the outcome and measurements of a line profile run.
type LineProfileResult struct {
	Outcome                               LineProfileOutcome
	Err                                   error // run error when Outcome != completed (sanitized driver error)
	FailedAt                              *LineProfileLocation
	Routines                              []LineProfileRoutine // [0] entry
	Columns                               []string
	Rows                                  int64
	KeptRows                              [][]Cell
	Before, After                         RoundTripBaseline
	Raw, Adjusted                         time.Duration // run totals (sum over entry inclusive)
	Prepare, Savepoints, Snapshots, Setup time.Duration // overhead
	// Blobs is the overhead of reading BLOB values fetched into variables
	// (they are held as bytes, R22) and of reading kept BLOB outputs.
	Blobs time.Duration
	// Retries is the time of statements the emulator ran only to choose
	// how to run a node: a singleton SELECT whose in-statement CAST failed
	// and that ran again without it (the node counts the second run only).
	Retries time.Duration
	// WarmUp is the elapsed time of the warm-up pass
	// (LineProfileOptions.WarmUp), its transaction included; it is in no
	// other bucket and no line. WarmUpErr is the error the pass ended with
	// (a sanitized driver error, without its location), nil when there was
	// no warm-up or it completed; a failed warm-up does not stop the
	// measured run. A ctx cancel or deadline
	// during the warm-up ends the call as canceled before the measured run;
	// RolledBack then reports the warm-up's rollback. The round-trip
	// baselines and activity snapshots start after the warm-up, so node
	// counters and OverheadActivity exclude it.
	WarmUp    time.Duration
	WarmUpErr error
	// OverheadActivity holds the attachment counters of overhead work —
	// prepares (with their catalog reads), savepoint statements, entry
	// arguments, BLOB reads and early cursor closes — which never count in
	// node counters.
	OverheadActivity        ActivityDelta
	ServerCalls, RoundTrips int64
	RelationNames           map[int]string
	NativeCallSites         []LineProfileCallSite
	RolledBack              bool
	ReadOnly                bool
}

// LineProfileCallSite is a call of a procedure marked for step-in that runs
// natively because of its shape, or a native FOR SELECT over a procedure
// that may run ahead of its loop body because it could not be fetched one
// row at a time.
type LineProfileCallSite struct {
	Routine           string
	Line              int
	Procedure, Reason string
}

// ErrLineProfileRefused reports static problems that prevent a line
// profile; no transaction was started.
var ErrLineProfileRefused = errors.New("interbase: line profile refused")

// lpParam is a procedure parameter with its declared type text.
type lpParam struct{ name, typeText string }

// lpRoutineMeta is a procedure loaded from the catalog and parsed.
type lpRoutineMeta struct {
	name            string
	source          string
	inputs, outputs []lpParam
	routine         *psql.Routine
	problems        []RoutineProblem
	// readsOnly is set when the analysis proved that running the routine
	// never writes (see lpPlan.checkReadsOnly); false means it may write.
	readsOnly, readsChecked bool
	// sourceProcs holds the bare FROM/JOIN names that are procedures, nil
	// until looked up (lpPlan.sourceProcedures).
	sourceProcs map[string]bool
}

// lpPlan is everything AnalyzeLineProfile reads, kept for the run.
type lpPlan struct {
	analysis  LineProfileAnalysis
	dialect   int
	dbNone    bool
	dbCharset string // database default character set, "" when unset
	charsets  map[int]lpCharset
	entry     *lpRoutineMeta
	procs     map[string]*lpRoutineMeta
	// exceptions maps user exception names to their number and message;
	// exceptionNames maps numbers back to names (RDB$EXCEPTIONS).
	exceptions     map[string]lpException
	exceptionNames map[int32]string
	// readProcedureRefs replaces readProcedureRefsTx in tests.
	readProcedureRefs func(context.Context, *Transaction, []lookupName) ([]RoutineRef, error)
}

// lpException is a user exception from RDB$EXCEPTIONS.
type lpException struct {
	number  int32
	message string
}

// AnalyzeLineProfile reads metadata in a short read-only, read-committed
// transaction and parses the entry routine (and stepInto callees, named as
// in LineProfileOptions.StepInto). It never executes routine code.
func AnalyzeLineProfile(ctx context.Context, pooled *sql.Conn, entrySQL string, dialect int, stepInto []string) (LineProfileAnalysis, error) {
	plan, err := planLineProfile(ctx, pooled, entrySQL, dialect, stepInto)
	if err != nil {
		return LineProfileAnalysis{}, err
	}
	return plan.analysis, nil
}

// LineProfile runs the entry statement under emulation in its own
// read-committed transaction (read-only when opts.ReadOnly) and ALWAYS rolls
// it back. Static problems return an error before any transaction starts.
func LineProfile(ctx context.Context, pooled *sql.Conn, opts LineProfileOptions) (result LineProfileResult, resultErr error) {
	if pooled == nil {
		return result, ErrNotInterBaseConn
	}
	if opts.KeepRows < 0 {
		return result, fmt.Errorf("interbase: line profile KeepRows must not be negative: %d", opts.KeepRows)
	}
	if opts.KeepRows == 0 {
		opts.KeepRows = 10
	}
	if opts.Dialect == 0 {
		opts.Dialect = 3
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}
	setupStart := time.Now()
	plan, err := planLineProfile(ctx, pooled, opts.SQL, opts.Dialect, opts.StepInto)
	if err != nil {
		return result, err
	}
	if problems := plan.analysis.Problems; len(problems) > 0 {
		messages := make([]string, len(problems))
		for i, p := range problems {
			messages[i] = p.String()
		}
		return result, fmt.Errorf("%w: %s", ErrLineProfileRefused, strings.Join(messages, "; "))
	}
	result.Setup = time.Since(setupStart)
	result.ReadOnly = opts.ReadOnly
	result.Routines = []LineProfileRoutine{newRoutineProfile(plan.entry)}
	result.Columns = entryColumns(plan)
	stepInto := stepIntoSet(plan, opts.StepInto)
	result.NativeCallSites = nativeCallSites(plan, stepInto)

	var outcomeErr error
	runErr := pooled.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		var err error
		outcomeErr, err = profileOnConn(ctx, c, plan, opts, &result, stepInto)
		return err
	})
	resultErr = runErr

	finishLineProfile(ctx, &result, outcomeErr, runErr)
	if ids := relationIDs(&result); len(ids) > 0 {
		names, err := RelationNames(context.WithoutCancel(ctx), pooled, ids)
		if err != nil {
			resultErr = errors.Join(resultErr, fmt.Errorf("interbase: resolve line profile relation names: %w", err))
		} else {
			result.RelationNames = names
		}
	}
	return result, resultErr
}

// profileOnConn runs the warm-up pass (opts.WarmUp), the baselines and the
// measured run on the pinned connection c. The first error is the run
// outcome; the second a baseline, close or rollback failure.
func profileOnConn(ctx context.Context, c *conn, plan *lpPlan, opts LineProfileOptions,
	result *LineProfileResult, stepInto map[string]bool) (outcomeErr, err error) {
	if opts.WarmUp {
		start := time.Now()
		warmErr, txErr := warmUp(ctx, c, plan, opts, result.Columns, stepInto, &result.RolledBack)
		result.WarmUp = time.Since(start)
		switch {
		case ctx.Err() != nil:
			if !errors.Is(warmErr, context.Canceled) && !errors.Is(warmErr, context.DeadlineExceeded) {
				warmErr = ctx.Err()
			}
			return warmErr, txErr
		case txErr != nil:
			return nil, fmt.Errorf("interbase: line profile warm-up: %w", txErr)
		}
		_, result.WarmUpErr = unwrapRunError(warmErr)
		result.RolledBack = false
	}
	if result.Before, err = measureBaseline(ctx, c); err != nil {
		return nil, err
	}
	run, err := beginRun(ctx, c, plan, opts, result, stepInto)
	if err != nil {
		return nil, err
	}
	outcomeErr, txErr := run.runInTransaction()
	after, afterErr := measureBaseline(context.WithoutCancel(ctx), c)
	result.After = after
	if afterErr != nil {
		afterErr = fmt.Errorf("interbase: line profile baseline after the run: %w", afterErr)
	}
	return outcomeErr, errors.Join(txErr, afterErr)
}

// beginRun starts a run's read-committed transaction (read-only when
// opts.ReadOnly) on c; the run measures into result.
func beginRun(ctx context.Context, c *conn, plan *lpPlan, opts LineProfileOptions,
	result *LineProfileResult, stepInto map[string]bool) (*lpRun, error) {
	tx, err := (&Attachment{conn: c}).BeginTx(ctx, TransactionOptions{
		ReadOnly:  opts.ReadOnly,
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		return nil, err
	}
	return &lpRun{
		ctx: ctx, conn: c, tx: tx, plan: plan, opts: opts, result: result, dbNone: plan.dbNone,
		statements: make(map[string]cachedStatement), stepInto: stepInto,
	}, nil
}

// warmUp runs the emulation once as the measured run will (Ruling W1), in
// its own transaction, always rolled back, and discards everything it
// measured. The first error is the pass's outcome; the second a close or
// rollback failure.
func warmUp(ctx context.Context, c *conn, plan *lpPlan, opts LineProfileOptions, columns []string,
	stepInto map[string]bool, rolledBack *bool) (outcomeErr, txErr error) {
	discarded := &LineProfileResult{
		ReadOnly: opts.ReadOnly, Columns: columns,
		Routines: []LineProfileRoutine{newRoutineProfile(plan.entry)},
	}
	run, err := beginRun(ctx, c, plan, opts, discarded, stepInto)
	if err != nil {
		return err, nil
	}
	outcomeErr, txErr = run.runInTransaction()
	*rolledBack = discarded.RolledBack
	return outcomeErr, txErr
}

func (p RoutineProblem) String() string {
	if p.Line == 0 {
		return fmt.Sprintf("%s: %s", p.Routine, p.Message)
	}
	return fmt.Sprintf("%s line %d: %s", p.Routine, p.Line, p.Message)
}

func newRoutineProfile(meta *lpRoutineMeta) LineProfileRoutine {
	profile := LineProfileRoutine{Name: meta.name, Source: meta.source, Parent: -1}
	profile.Nodes = make([]LineProfileNode, len(meta.routine.Nodes))
	for i, n := range meta.routine.Nodes {
		profile.Nodes[i] = LineProfileNode{ID: n.ID, Kind: string(n.Kind), Line: n.Line, EndLine: n.EndLine}
	}
	return profile
}

// entryColumns lists the result columns: the selected output columns, or
// every output parameter.
func entryColumns(plan *lpPlan) []string {
	if plan.analysis.Entry.Selectable && plan.analysis.Entry.Columns != nil {
		return append([]string{}, plan.analysis.Entry.Columns...)
	}
	columns := make([]string, len(plan.entry.outputs))
	for i, p := range plan.entry.outputs {
		columns[i] = p.name
	}
	return columns
}

func finishLineProfile(ctx context.Context, result *LineProfileResult, outcomeErr, runErr error) {
	aggregateRoutines(result.Routines)
	if len(result.Routines) > 0 {
		result.Raw = result.Routines[0].Inclusive.Raw
		result.Adjusted = result.Routines[0].Inclusive.Adjusted
	}
	err := outcomeErr
	if err == nil && runErr != nil && !result.RolledBack {
		err = runErr
	}
	if err == nil {
		result.Outcome = LineProfileCompleted
		return
	}
	result.FailedAt, err = unwrapRunError(err)
	result.Err = err
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.Outcome = LineProfileCanceled
		result.FailedAt = nil
		return
	}
	result.Outcome = LineProfileFailed
}

// unwrapRunError returns the driver error of a run outcome and the location
// where it stopped (nil when it is not located).
func unwrapRunError(err error) (*LineProfileLocation, error) {
	var at *LineProfileLocation
	var located *lpNodeError
	if errors.As(err, &located) {
		at = &LineProfileLocation{Routine: located.routine, Line: located.line}
		err = located.err
	}
	var fatalErr *lpFatalError
	if errors.As(err, &fatalErr) {
		err = fatalErr.err
	}
	return at, err
}

// relationIDs lists the relations of the node and overhead counters, sorted.
func relationIDs(result *LineProfileResult) []int {
	seen := make(map[int]struct{})
	var ids []int
	add := func(tables map[int]TableCounters) {
		for id := range tables {
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				ids = append(ids, id)
			}
		}
	}
	for _, routine := range result.Routines {
		for _, node := range routine.Nodes {
			add(node.Metrics.Tables)
		}
	}
	add(result.OverheadActivity.Tables)
	sort.Ints(ids)
	return ids
}
