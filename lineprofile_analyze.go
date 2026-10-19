package interbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"

	"interbase-go/psql"
)

const lineProfileParametersQuery = "SELECT pp.RDB$PARAMETER_NAME, pp.RDB$PARAMETER_TYPE, f.RDB$FIELD_TYPE, " +
	"f.RDB$FIELD_SUB_TYPE, f.RDB$FIELD_LENGTH, f.RDB$FIELD_SCALE, f.RDB$FIELD_PRECISION, " +
	"f.RDB$CHARACTER_LENGTH, cs.RDB$CHARACTER_SET_NAME " +
	"FROM RDB$PROCEDURE_PARAMETERS pp " +
	"JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = pp.RDB$FIELD_SOURCE " +
	"LEFT JOIN RDB$CHARACTER_SETS cs ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID " +
	"WHERE pp.RDB$PROCEDURE_NAME = ? ORDER BY pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER"

// planLineProfile performs the static analysis in one short read-only,
// read-committed transaction. Problems are returned in the analysis; an
// error means the catalog could not be read.
func planLineProfile(ctx context.Context, pooled *sql.Conn, entrySQL string, dialect int, stepInto []string) (*lpPlan, error) {
	if pooled == nil {
		return nil, ErrNotInterBaseConn
	}
	if dialect == 0 {
		dialect = 3
	}
	plan := &lpPlan{dialect: dialect, procs: make(map[string]*lpRoutineMeta)}
	entry, err := psql.ParseEntryCall(entrySQL, dialect)
	if err != nil {
		plan.analysis.Problems = []RoutineProblem{{Routine: "entry statement", Message: err.Error()}}
		return plan, nil
	}
	plan.analysis.Entry, plan.analysis.Routine = entry, entry.Procedure

	err = WithTransaction(ctx, pooled, TransactionOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	}, func(tx *Transaction) error {
		return plan.load(ctx, tx, stepInto)
	})
	if err != nil {
		return nil, err
	}
	return plan, nil
}

func (plan *lpPlan) problem(routine string, line, column int, format string, args ...any) {
	plan.analysis.Problems = append(plan.analysis.Problems, RoutineProblem{
		Routine: routine, Line: line, Column: column, Message: fmt.Sprintf(format, args...),
	})
}

func (plan *lpPlan) load(ctx context.Context, tx *Transaction, stepInto []string) error {
	var err error
	if plan.dbCharset, err = databaseCharset(ctx, tx); err != nil {
		return err
	}
	plan.dbNone = plan.dbCharset == "" || strings.EqualFold(plan.dbCharset, "NONE")
	if plan.charsets, err = characterSets(ctx, tx); err != nil {
		return err
	}
	if plan.exceptions, plan.exceptionNames, err = userExceptions(ctx, tx); err != nil {
		return err
	}
	entry := plan.analysis.Entry
	meta, err := loadRoutineMeta(ctx, tx, entry.Procedure, plan.dialect, plan.exceptions)
	if errors.Is(err, ErrRoutineNotFound) {
		isRelation, relationErr := relationExists(ctx, tx, entry.Procedure)
		if relationErr != nil {
			return relationErr
		}
		if isRelation {
			plan.problem("entry statement", 0, 0, "%s is a table or view, not a procedure", entry.Procedure)
		} else {
			plan.problem("entry statement", 0, 0, "procedure %s does not exist", entry.Procedure)
		}
		return nil
	}
	if err != nil {
		return err
	}
	plan.entry = meta
	plan.procs[meta.name] = meta
	plan.analysis.Problems = append(plan.analysis.Problems, meta.problems...)
	if len(entry.Args) != len(meta.inputs) {
		plan.problem("entry statement", 0, 0, "%d arguments given; %s has %d input parameters",
			len(entry.Args), meta.name, len(meta.inputs))
	}
	outputs := make(map[string]bool, len(meta.outputs))
	for _, p := range meta.outputs {
		outputs[p.name] = true
	}
	for _, column := range entry.Columns {
		if !outputs[column] {
			plan.problem("entry statement", 0, 0, "%s is not an output parameter of %s", column, meta.name)
		}
	}
	if meta.routine.Body == nil {
		return nil
	}
	callees, mayWrite, err := plan.callees(ctx, tx, meta, 1)
	if err != nil {
		return err
	}
	plan.analysis.MayWrite = mayWrite
	selected := stepIntoNames(stepInto)
	visited := map[string]bool{meta.name: true}
	for i := 0; i < len(callees); i++ {
		callee := callees[i]
		if !selected[callee.Name] || visited[callee.Name] {
			continue
		}
		visited[callee.Name] = true
		calleeMeta := plan.procs[callee.Name]
		if calleeMeta == nil {
			continue
		}
		plan.analysis.Problems = append(plan.analysis.Problems, calleeMeta.problems...)
		if !callee.Parses {
			continue
		}
		more, calleeWrites, err := plan.callees(ctx, tx, calleeMeta, callee.Depth+1)
		if err != nil {
			return err
		}
		plan.analysis.MayWrite = plan.analysis.MayWrite || calleeWrites
		for _, c := range more {
			if !visited[c.Name] {
				callees = append(callees, c)
			}
		}
	}
	plan.analysis.Callees = callees
	return nil
}

// callees lists the procedures meta calls, loading each one's metadata.
// Bare FROM/JOIN names that resolve in RDB$PROCEDURES are parameterless
// procedure calls (R6): they make the routine a writer and are listed, but
// are not step-in shapes.
func (plan *lpPlan) callees(ctx context.Context, tx *Transaction, meta *lpRoutineMeta, depth int) ([]LineProfileCallee, bool, error) {
	mayWrite := meta.routine.MayWrite()
	byName := make(map[string]*LineProfileCallee)
	var order []string
	add := func(c psql.Callee, stepIn bool) {
		existing, ok := byName[c.Name]
		if !ok {
			existing = &LineProfileCallee{Name: c.Name, Depth: depth}
			byName[c.Name] = existing
			order = append(order, c.Name)
		}
		existing.Lines = mergeLines(existing.Lines, c.Lines)
		existing.StepInShape = existing.StepInShape || stepIn
	}
	for _, c := range meta.routine.Callees() {
		add(c, c.StepInShape)
	}
	sources := meta.routine.SourceNames()
	if len(sources) > 0 {
		candidates := make([]lookupName, len(sources))
		for i, s := range sources {
			candidates[i] = lookupName{name: s.Name, quoted: true}
		}
		procedures, err := readProcedureRefsTx(ctx, tx, candidates)
		if err != nil {
			return nil, false, err
		}
		found := make(map[string]bool, len(procedures))
		for _, p := range procedures {
			found[p.Name] = true
		}
		for _, s := range sources {
			if found[s.Name] {
				add(s, false)
				mayWrite = true
			}
		}
	}
	callees := make([]LineProfileCallee, 0, len(order))
	for _, name := range order {
		callee := *byName[name]
		calleeMeta, ok := plan.procs[name]
		if !ok {
			loaded, err := loadRoutineMeta(ctx, tx, name, plan.dialect, plan.exceptions)
			if err != nil && !errors.Is(err, ErrRoutineNotFound) {
				return nil, false, err
			}
			if err == nil {
				plan.procs[name], calleeMeta, ok = loaded, loaded, true
			}
		}
		callee.Parses = ok && len(calleeMeta.problems) == 0
		callees = append(callees, callee)
	}
	sort.SliceStable(callees, func(i, j int) bool { return callees[i].Lines[0] < callees[j].Lines[0] })
	return callees, mayWrite, nil
}

func mergeLines(a, b []int) []int {
	seen := make(map[int]bool, len(a)+len(b))
	var lines []int
	for _, line := range append(append([]int(nil), a...), b...) {
		if !seen[line] {
			seen[line] = true
			lines = append(lines, line)
		}
	}
	sort.Ints(lines)
	return lines
}

// loadRoutineMeta reads a procedure's source and parameters and parses it.
func loadRoutineMeta(ctx context.Context, tx *Transaction, name string, dialect int, exceptions map[string]lpException) (*lpRoutineMeta, error) {
	ref, source, err := readRoutineSourceTx(ctx, tx, RoutineRef{Kind: "procedure", Name: name})
	if err != nil {
		return nil, err
	}
	meta := &lpRoutineMeta{name: ref.Name, source: source}
	cursor, err := tx.Query(ctx, lineProfileParametersQuery, ref.Name)
	if err != nil {
		return nil, fmt.Errorf("interbase: read procedure parameters: %w", err)
	}
	err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
		if len(row) != 9 {
			return errors.New("interbase: procedure parameter query returned an unexpected column count")
		}
		paramName, err := catalogText(row[0])
		if err != nil {
			return err
		}
		direction, err := activityCellInt64(row[1])
		if err != nil {
			return err
		}
		fieldType, err := activityCellInt64(row[2])
		if err != nil {
			return err
		}
		field := lpField{typ: fieldType, subType: cellValue(row[3]), scale: cellValue(row[5]),
			precision: cellValue(row[6]), charLength: cellValue(row[7])}
		if length, ok := cellValue(row[4]).(int64); ok {
			field.length = length
		}
		if charset, err := catalogText(row[8]); err == nil {
			field.charset = trimCatalogName(charset)
		}
		typeText, err := field.typeText(dialect)
		if err != nil {
			return fmt.Errorf("parameter %s: %w", trimCatalogName(paramName), err)
		}
		param := lpParam{name: trimCatalogName(paramName), typeText: typeText}
		if direction == 0 {
			meta.inputs = append(meta.inputs, param)
		} else {
			meta.outputs = append(meta.outputs, param)
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("interbase: read procedure parameters of %s: %w", ref.Name, err)
	}
	names := make([]string, 0, len(meta.inputs)+len(meta.outputs))
	for _, p := range append(append([]lpParam(nil), meta.inputs...), meta.outputs...) {
		names = append(names, p.name)
	}
	routine, problems := psql.ParseRoutine(source, psql.ParseOptions{Dialect: dialect, Params: names})
	meta.routine = routine
	for _, p := range problems {
		meta.problems = append(meta.problems, RoutineProblem{Routine: meta.name, Line: p.Line, Column: p.Column, Message: p.Message})
	}
	meta.problems = append(meta.problems, checkRoutine(meta, exceptions)...)
	sort.SliceStable(meta.problems, func(i, j int) bool {
		a, b := meta.problems[i], meta.problems[j]
		return a.Line < b.Line || a.Line == b.Line && a.Column < b.Column
	})
	return meta, nil
}

// checkRoutine reports constructs the interpreter refuses beyond the
// parser's own problems: unknown :name references (R10), unknown targets,
// unknown user exceptions, EXCEPTION with a message (which InterBase
// refuses, R19) and WHEN GDSCODE names without a known code.
// InterBase rejects SQLCODE and GDSCODE outside WHEN conditions, so they are
// ordinary words here.
func checkRoutine(meta *lpRoutineMeta, exceptions map[string]lpException) []RoutineProblem {
	var problems []RoutineProblem
	add := func(line, column int, format string, args ...any) {
		problems = append(problems, RoutineProblem{Routine: meta.name, Line: line, Column: column,
			Message: fmt.Sprintf(format, args...)})
	}
	names := make(map[string]bool)
	for _, d := range meta.routine.Declarations {
		names[d.Name] = true
	}
	for _, p := range append(append([]lpParam(nil), meta.inputs...), meta.outputs...) {
		names[p.name] = true
	}
	checkExpr := func(n *psql.Node, e *psql.Expr) {
		if e == nil {
			return
		}
		for _, name := range unknownColonNames(e.Text, names) {
			add(e.Line, e.Column, "unknown variable :%s", name)
		}
	}
	checkTargets := func(n *psql.Node, targets []string) {
		for _, target := range targets {
			if !names[target] {
				add(n.Line, n.Column, "unknown target %s", target)
			}
		}
	}
	for _, n := range meta.routine.Nodes {
		for _, h := range n.Handlers {
			for _, c := range h.Conditions {
				switch c.Kind {
				case "exception":
					if _, ok := exceptions[c.Name]; !ok {
						add(h.Line, 0, "unknown exception %s", c.Name)
					}
				case "gdscode":
					if _, ok := GDSCodeByName(c.Name); !ok {
						add(h.Line, 0, "unsupported GDSCODE %s", strings.ToLower(c.Name))
					}
				}
			}
		}
		switch n.Kind {
		case psql.NodeException:
			if _, ok := exceptions[n.Exception]; !ok {
				add(n.Line, n.Column, "unknown exception %s", n.Exception)
			}
			if n.Value != nil {
				add(n.Line, n.Column, "EXCEPTION with a message is not supported (InterBase refuses it)")
			}
		case psql.NodeAssign:
			checkTargets(n, []string{n.Target})
		}
		checkExpr(n, n.Value)
		checkExpr(n, n.Cond)
		if n.SQL != nil {
			for _, name := range n.SQL.Params {
				if !names[name] {
					add(n.Line, n.Column, "unknown variable :%s", name)
				}
			}
		}
		checkTargets(n, n.Into)
		checkTargets(n, n.Returning)
		if n.Kind == psql.NodeExecProc && n.Proc != nil {
			for _, arg := range n.Proc.Args {
				checkExpr(n, arg)
			}
		}
	}
	return problems
}

// databaseCharset returns the database default character set name, "" when
// none is set (which means NONE).
func databaseCharset(ctx context.Context, tx *Transaction) (string, error) {
	cursor, err := tx.Query(ctx, "SELECT RDB$CHARACTER_SET_NAME FROM RDB$DATABASE")
	if err != nil {
		return "", fmt.Errorf("interbase: read database character set: %w", err)
	}
	row, found, err := firstCatalogRow(ctx, cursor, 1)
	if err != nil {
		return "", fmt.Errorf("interbase: read database character set: %w", err)
	}
	if !found {
		return "", nil
	}
	name, err := catalogText(row[0])
	if err != nil {
		return "", err
	}
	return strings.ToUpper(trimCatalogName(name)), nil
}

// userExceptions reads RDB$EXCEPTIONS: names to number and message, and
// numbers back to names.
func userExceptions(ctx context.Context, tx *Transaction) (map[string]lpException, map[int32]string, error) {
	cursor, err := tx.Query(ctx, "SELECT RDB$EXCEPTION_NAME, RDB$EXCEPTION_NUMBER, RDB$MESSAGE FROM RDB$EXCEPTIONS")
	if err != nil {
		return nil, nil, fmt.Errorf("interbase: read exceptions: %w", err)
	}
	byName := make(map[string]lpException)
	byNumber := make(map[int32]string)
	err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
		if len(row) != 3 {
			return errors.New("interbase: exception query returned an unexpected column count")
		}
		name, err := catalogText(row[0])
		if err != nil {
			return err
		}
		number, err := activityCellInt64(row[1])
		if err != nil {
			return err
		}
		message, _ := catalogText(row[2])
		name = trimCatalogName(name)
		byName[name] = lpException{number: int32(number), message: message}
		byNumber[int32(number)] = name
		return nil
	})
	if err != nil {
		return nil, nil, fmt.Errorf("interbase: read exceptions: %w", err)
	}
	return byName, byNumber, nil
}

// lpCharset is a character set name and its maximum bytes per character.
type lpCharset struct {
	name         string
	bytesPerChar int
}

func characterSets(ctx context.Context, tx *Transaction) (map[int]lpCharset, error) {
	cursor, err := tx.Query(ctx, "SELECT RDB$CHARACTER_SET_ID, RDB$CHARACTER_SET_NAME, RDB$BYTES_PER_CHARACTER FROM RDB$CHARACTER_SETS")
	if err != nil {
		return nil, fmt.Errorf("interbase: read character sets: %w", err)
	}
	sets := make(map[int]lpCharset)
	err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
		if len(row) != 3 {
			return errors.New("interbase: character set query returned an unexpected column count")
		}
		id, err := activityCellInt64(row[0])
		if err != nil {
			return err
		}
		name, err := catalogText(row[1])
		if err != nil {
			return err
		}
		bytesPerChar, _ := cellValue(row[2]).(int64)
		if bytesPerChar < 1 {
			bytesPerChar = 1
		}
		sets[int(id)] = lpCharset{name: trimCatalogName(name), bytesPerChar: int(bytesPerChar)}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("interbase: read character sets: %w", err)
	}
	return sets, nil
}

func relationExists(ctx context.Context, tx *Transaction, name string) (bool, error) {
	cursor, err := tx.Query(ctx, "SELECT 1 FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = ?", name)
	if err != nil {
		return false, fmt.Errorf("interbase: read relation: %w", err)
	}
	_, found, err := firstCatalogRow(ctx, cursor, 1)
	if err != nil {
		return false, fmt.Errorf("interbase: read relation: %w", err)
	}
	return found, nil
}
