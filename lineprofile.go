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
		if result.Before, err = measureBaseline(ctx, c); err != nil {
			return err
		}
		tx, err := (&Attachment{conn: c}).BeginTx(ctx, TransactionOptions{
			ReadOnly:  opts.ReadOnly,
			Isolation: sql.LevelReadCommitted,
		})
		if err != nil {
			return err
		}
		run := &lpRun{
			ctx: ctx, conn: c, tx: tx, plan: plan, opts: opts, result: &result, dbNone: plan.dbNone,
			statements: make(map[string]cachedStatement), stepInto: stepInto,
		}
		var txErr error
		outcomeErr, txErr = run.runInTransaction()
		after, afterErr := measureBaseline(context.WithoutCancel(ctx), c)
		result.After = after
		if afterErr != nil {
			afterErr = fmt.Errorf("interbase: line profile baseline after the run: %w", afterErr)
		}
		return errors.Join(txErr, afterErr)
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
	var located *lpNodeError
	if errors.As(err, &located) {
		result.FailedAt = &LineProfileLocation{Routine: located.routine, Line: located.line}
		err = located.err
	}
	var fatalErr *lpFatalError
	if errors.As(err, &fatalErr) {
		err = fatalErr.err
	}
	result.Err = err
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		result.Outcome = LineProfileCanceled
		result.FailedAt = nil
		return
	}
	result.Outcome = LineProfileFailed
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
