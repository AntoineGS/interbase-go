package interbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"interbase-go/planparse"
	"interbase-go/psql"
)

const routinePlanChunkSize = 500

const nullIndicator = uint16(1 << 15)

// ErrRoutineNotFound reports that the requested procedure or trigger is absent.
var ErrRoutineNotFound = errors.New("interbase: routine not found")

// PlanIndex describes an index referenced by a server-generated plan.
type PlanIndex struct {
	Name, Relation string
	Columns        []string
	Unique         bool
	Selectivity    *float64
}

// PlanDescription contains parsed plan text and available catalog details.
type PlanDescription struct {
	Raw     string
	Tree    *planparse.Tree
	Problem string
	Indexes map[string]PlanIndex
}

// RoutineRef identifies a procedure or a trigger and its relation/event.
type RoutineRef struct {
	Kind     string
	Name     string
	Relation string
	Event    string
}

// RoutineLane is one extracted statement and its prepare-only plan result.
type RoutineLane struct {
	Line    int
	Kind    string
	SQL     string
	Plan    PlanDescription
	Message string
	Refs    []RoutineRef
}

// RoutinePlan contains the extracted statement lanes for one stored routine.
type RoutinePlan struct {
	Ref       RoutineRef
	Lanes     []RoutineLane
	Truncated bool
}

// DescribePlan parses raw plan text and looks up relation and index details
// using a short read-only transaction. It never executes query.
func DescribePlan(ctx context.Context, conn *sql.Conn, query, raw string) (PlanDescription, error) {
	description := PlanDescription{Raw: raw, Indexes: make(map[string]PlanIndex)}
	if strings.TrimSpace(raw) == "" {
		return description, nil
	}
	tree, err := planparse.Parse(raw)
	if err != nil {
		description.Problem = err.Error()
		return description, nil
	}
	description.Tree = tree

	err = WithTransaction(ctx, conn, TransactionOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	}, func(tx *Transaction) error {
		var err error
		description, err = describePlanTx(ctx, tx, query, raw)
		return err
	})
	return description, err
}

// StatementRoutines verifies procedure and trigger references in query with
// catalog reads inside one short read-only transaction.
func StatementRoutines(ctx context.Context, conn *sql.Conn, query string) ([]RoutineRef, error) {
	statement := psql.ExtractStatementRefs(query)
	var routines []RoutineRef
	err := WithTransaction(ctx, conn, TransactionOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	}, func(tx *Transaction) error {
		var err error
		routines, err = routinesForStatementTx(ctx, tx, statement, statementRoutineCandidates(statement))
		return err
	})
	if err != nil {
		return nil, err
	}
	return routines, nil
}

// PlanRoutine derives statement plans from a stored procedure or trigger
// source. Every lane is prepared only; extracted SQL is never executed.
func PlanRoutine(ctx context.Context, conn *sql.Conn, ref RoutineRef) (RoutinePlan, error) {
	result := RoutinePlan{}
	err := WithTransaction(ctx, conn, TransactionOptions{
		ReadOnly:  true,
		Isolation: sql.LevelReadCommitted,
	}, func(tx *Transaction) error {
		actualRef, source, err := readRoutineSourceTx(ctx, tx, ref)
		if err != nil {
			return err
		}
		result.Ref = actualRef
		statements := psql.Extract(source)
		if len(statements) > routinePlanChunkSize {
			statements = statements[:routinePlanChunkSize]
			result.Truncated = true
		}
		result.Lanes = make([]RoutineLane, 0, len(statements))
		for _, statement := range statements {
			if err := contextError(ctx); err != nil {
				return err
			}
			lane := laneFromStatement(statement)
			lane.Refs, err = routinesForStatementTx(ctx, tx, statement, planRoutineCandidates(statement))
			if err != nil {
				return err
			}

			if statement.Kind != "executeStatement" && statement.Kind != "executeProcedure" && lane.SQL != "" {
				if err := contextError(ctx); err != nil {
					return err
				}
				raw, planErr := tx.Plan(ctx, lane.SQL)
				if planErr != nil {
					if err := contextError(ctx); err != nil {
						return err
					}
					lane.Message = prepareFailureMessage(planErr)
				} else if strings.TrimSpace(raw) == "" {
					lane.Message = "no tables"
				} else {
					lane.Plan, err = describePlanTx(ctx, tx, lane.SQL, raw)
					if err != nil {
						return err
					}
				}
			}
			result.Lanes = append(result.Lanes, lane)
		}
		return nil
	})
	return result, err
}

func describePlanTx(ctx context.Context, tx *Transaction, query, raw string) (PlanDescription, error) {
	description := PlanDescription{Raw: raw, Indexes: make(map[string]PlanIndex)}
	if strings.TrimSpace(raw) == "" {
		return description, nil
	}
	tree, err := planparse.Parse(raw)
	if err != nil {
		description.Problem = err.Error()
		return description, nil
	}
	description.Tree = tree
	if err := contextError(ctx); err != nil {
		return description, err
	}

	labels := planRelationLabels(tree)
	indexNames := planIndexNames(tree)
	relationNames, err := readPlanRelationsTx(ctx, tx, labels)
	if err != nil {
		return description, err
	}
	description.Indexes, err = readPlanIndexesTx(ctx, tx, indexNames)
	if err != nil {
		return description, err
	}
	resolveRelations(tree, psql.Aliases(query), func(name string) bool {
		_, ok := relationNames[catalogNameKey(name)]
		return ok
	})
	return description, nil
}

func resolveRelations(tree *planparse.Tree, aliases map[string]string, isRelation func(string) bool) {
	if tree == nil {
		return
	}
	var visit func(*planparse.Node)
	visit = func(node *planparse.Node) {
		if node == nil {
			return
		}
		if node.Kind == "table" && len(node.Labels) != 0 {
			label := trimCatalogName(node.Labels[len(node.Labels)-1])
			if isRelation != nil && isRelation(label) {
				node.Relation = label
			} else if relation, ok := lookupAlias(aliases, label); ok {
				node.Relation = relation
			}
		}
		for _, child := range node.Children {
			visit(child)
		}
	}
	for _, root := range tree.Roots {
		visit(root)
	}
}

func lookupAlias(aliases map[string]string, label string) (string, bool) {
	if relation, ok := aliases[label]; ok {
		return trimCatalogName(relation), true
	}
	if relation, ok := aliases[strings.ToUpper(label)]; ok {
		return trimCatalogName(relation), true
	}
	return "", false
}

func planRelationLabels(tree *planparse.Tree) []string {
	var names []string
	var visit func(*planparse.Node)
	visit = func(node *planparse.Node) {
		if node == nil {
			return
		}
		if node.Kind == "table" {
			names = append(names, node.Labels...)
		}
		for _, child := range node.Children {
			visit(child)
		}
	}
	for _, root := range tree.Roots {
		visit(root)
	}
	return lookupNameVariants(names)
}

func planIndexNames(tree *planparse.Tree) []string {
	var names []string
	var visit func(*planparse.Node)
	visit = func(node *planparse.Node) {
		if node == nil {
			return
		}
		if node.Kind == "table" {
			names = append(names, node.Indexes...)
			if node.OrderIndex != "" {
				names = append(names, node.OrderIndex)
			}
		}
		for _, child := range node.Children {
			visit(child)
		}
	}
	for _, root := range tree.Roots {
		visit(root)
	}
	return lookupNameVariants(names)
}

func readPlanRelationsTx(ctx context.Context, tx *Transaction, names []string) (map[string]string, error) {
	relations := make(map[string]string)
	for start := 0; start < len(names); start += routinePlanChunkSize {
		end := start + routinePlanChunkSize
		if end > len(names) {
			end = len(names)
		}
		chunk := names[start:end]
		query := "SELECT RDB$RELATION_NAME FROM RDB$RELATIONS WHERE RDB$RELATION_NAME IN (" +
			inPlaceholders(len(chunk)) + ")"
		cursor, err := tx.Query(ctx, query, stringArguments(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("interbase: read plan relations: %w", err)
		}
		err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
			if len(row) != 1 {
				return errors.New("interbase: plan relation query returned an unexpected column count")
			}
			name, err := catalogText(row[0])
			if err != nil {
				return fmt.Errorf("interbase: decode plan relation: %w", err)
			}
			name = trimCatalogName(name)
			relations[catalogNameKey(name)] = name
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("interbase: read plan relations: %w", err)
		}
	}
	return relations, nil
}

func readPlanIndexesTx(ctx context.Context, tx *Transaction, names []string) (map[string]PlanIndex, error) {
	indexes := make(map[string]PlanIndex)
	for start := 0; start < len(names); start += routinePlanChunkSize {
		end := start + routinePlanChunkSize
		if end > len(names) {
			end = len(names)
		}
		chunk := names[start:end]
		query := "SELECT i.RDB$INDEX_NAME, i.RDB$RELATION_NAME, i.RDB$UNIQUE_FLAG, i.RDB$STATISTICS, s.RDB$FIELD_NAME " +
			"FROM RDB$INDICES i LEFT JOIN RDB$INDEX_SEGMENTS s ON s.RDB$INDEX_NAME = i.RDB$INDEX_NAME " +
			"WHERE i.RDB$INDEX_NAME IN (" + inPlaceholders(len(chunk)) +
			") ORDER BY i.RDB$INDEX_NAME, s.RDB$FIELD_POSITION"
		cursor, err := tx.Query(ctx, query, stringArguments(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("interbase: read plan indexes: %w", err)
		}
		err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
			if len(row) != 5 {
				return errors.New("interbase: plan index query returned an unexpected column count")
			}
			name, err := catalogText(row[0])
			if err != nil {
				return fmt.Errorf("decode index name: %w", err)
			}
			relation, err := catalogText(row[1])
			if err != nil {
				return fmt.Errorf("decode index relation: %w", err)
			}
			name = trimCatalogName(name)
			relation = trimCatalogName(relation)
			key := catalogNameKey(name)
			index, ok := indexes[key]
			if !ok {
				index = PlanIndex{Name: name, Relation: relation}
				if !isCatalogNull(row[2]) {
					unique, err := activityCellInt64(row[2])
					if err != nil {
						return fmt.Errorf("decode index unique flag: %w", err)
					}
					index.Unique = unique != 0
				}
				if !isCatalogNull(row[3]) {
					selectivity, err := catalogFloat64(row[3])
					if err != nil {
						return fmt.Errorf("decode index selectivity: %w", err)
					}
					index.Selectivity = &selectivity
				}
			}
			if !isCatalogNull(row[4]) {
				field, err := catalogText(row[4])
				if err != nil {
					return fmt.Errorf("decode index field: %w", err)
				}
				index.Columns = append(index.Columns, trimCatalogName(field))
			}
			indexes[key] = index
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("interbase: read plan indexes: %w", err)
		}
	}
	return indexes, nil
}

func routinesForStatementTx(ctx context.Context, tx *Transaction, statement psql.Statement, candidates []string) ([]RoutineRef, error) {
	procedures, err := readProcedureRefsTx(ctx, tx, candidates)
	if err != nil {
		return nil, err
	}
	routines := append([]RoutineRef(nil), procedures...)
	if statement.Kind != "insert" && statement.Kind != "update" && statement.Kind != "delete" {
		return routines, nil
	}
	if trimCatalogName(statement.Target) == "" {
		return routines, nil
	}
	query := "SELECT RDB$TRIGGER_NAME, RDB$TRIGGER_TYPE FROM RDB$TRIGGERS " +
		"WHERE RDB$RELATION_NAME = ? " +
		"AND (RDB$TRIGGER_INACTIVE IS NULL OR RDB$TRIGGER_INACTIVE = 0) " +
		"AND (RDB$SYSTEM_FLAG IS NULL OR RDB$SYSTEM_FLAG = 0) " +
		"ORDER BY RDB$TRIGGER_SEQUENCE"
	cursor, err := tx.Query(ctx, query, trimCatalogName(statement.Target))
	if err != nil {
		return nil, fmt.Errorf("interbase: read statement triggers: %w", err)
	}
	err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
		if len(row) != 2 {
			return errors.New("interbase: statement trigger query returned an unexpected column count")
		}
		name, err := catalogText(row[0])
		if err != nil {
			return fmt.Errorf("decode trigger name: %w", err)
		}
		typ, err := activityCellInt64(row[1])
		if err != nil {
			return fmt.Errorf("decode trigger type: %w", err)
		}
		event, ok := triggerEvent(int(typ))
		if !ok || !triggerMatchesStatement(statement.Kind, int(typ)) {
			return nil
		}
		routines = append(routines, RoutineRef{
			Kind: "trigger", Name: trimCatalogName(name),
			Relation: trimCatalogName(statement.Target), Event: event,
		})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("interbase: read statement triggers: %w", err)
	}
	return deduplicateRoutineRefs(routines), nil
}

func statementRoutineCandidates(statement psql.Statement) []string {
	candidates := []string{statement.Procedure}
	return append(candidates, statement.Sources...)
}

func planRoutineCandidates(statement psql.Statement) []string {
	candidates := []string{statement.Procedure}
	return append(candidates, statement.Calls...)
}

func readProcedureRefsTx(ctx context.Context, tx *Transaction, candidates []string) ([]RoutineRef, error) {
	names := lookupNameVariants(candidates)
	var routines []RoutineRef
	for start := 0; start < len(names); start += routinePlanChunkSize {
		end := start + routinePlanChunkSize
		if end > len(names) {
			end = len(names)
		}
		chunk := names[start:end]
		query := "SELECT RDB$PROCEDURE_NAME FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME IN (" +
			inPlaceholders(len(chunk)) + ") ORDER BY RDB$PROCEDURE_NAME"
		cursor, err := tx.Query(ctx, query, stringArguments(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("interbase: read statement procedures: %w", err)
		}
		err = visitCatalogCursor(ctx, cursor, func(row []Cell) error {
			if len(row) != 1 {
				return errors.New("interbase: statement procedure query returned an unexpected column count")
			}
			name, err := catalogText(row[0])
			if err != nil {
				return fmt.Errorf("decode procedure name: %w", err)
			}
			routines = append(routines, RoutineRef{Kind: "procedure", Name: trimCatalogName(name)})
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("interbase: read statement procedures: %w", err)
		}
	}
	return deduplicateRoutineRefs(routines), nil
}

func readRoutineSourceTx(ctx context.Context, tx *Transaction, ref RoutineRef) (RoutineRef, string, error) {
	ref.Kind = strings.ToLower(strings.TrimSpace(ref.Kind))
	ref.Name = trimCatalogName(ref.Name)
	ref.Relation = trimCatalogName(ref.Relation)
	switch ref.Kind {
	case "procedure":
		for _, candidate := range lookupNameVariants([]string{ref.Name}) {
			cursor, err := tx.Query(ctx,
				"SELECT RDB$PROCEDURE_NAME, RDB$PROCEDURE_SOURCE FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?",
				candidate)
			if err != nil {
				return RoutineRef{}, "", fmt.Errorf("interbase: read procedure source: %w", err)
			}
			row, found, err := firstCatalogRow(ctx, cursor, 2)
			if err != nil {
				return RoutineRef{}, "", fmt.Errorf("interbase: read procedure source: %w", err)
			}
			if !found {
				continue
			}
			name, err := catalogText(row[0])
			if err != nil {
				return RoutineRef{}, "", fmt.Errorf("interbase: decode procedure name: %w", err)
			}
			source, err := catalogTextTx(ctx, tx, row[1])
			if err != nil {
				return RoutineRef{}, "", fmt.Errorf("interbase: decode procedure source: %w", err)
			}
			return RoutineRef{Kind: "procedure", Name: trimCatalogName(name)}, source, nil
		}
		return RoutineRef{}, "", ErrRoutineNotFound
	case "trigger":
		nameCandidates := lookupNameVariants([]string{ref.Name})
		relationCandidates := []string{ref.Relation}
		if ref.Relation != "" {
			relationCandidates = lookupNameVariants(relationCandidates)
		}
		for _, nameCandidate := range nameCandidates {
			for _, relationCandidate := range relationCandidates {
				query := "SELECT RDB$TRIGGER_NAME, RDB$TRIGGER_SOURCE, RDB$RELATION_NAME, RDB$TRIGGER_TYPE " +
					"FROM RDB$TRIGGERS WHERE RDB$TRIGGER_NAME = ?"
				args := []any{nameCandidate}
				if relationCandidate != "" {
					query += " AND RDB$RELATION_NAME = ?"
					args = append(args, relationCandidate)
				}
				cursor, err := tx.Query(ctx, query, args...)
				if err != nil {
					return RoutineRef{}, "", fmt.Errorf("interbase: read trigger source: %w", err)
				}
				row, found, err := firstCatalogRow(ctx, cursor, 4)
				if err != nil {
					return RoutineRef{}, "", fmt.Errorf("interbase: read trigger source: %w", err)
				}
				if !found {
					continue
				}
				name, err := catalogText(row[0])
				if err != nil {
					return RoutineRef{}, "", fmt.Errorf("interbase: decode trigger name: %w", err)
				}
				source, err := catalogTextTx(ctx, tx, row[1])
				if err != nil {
					return RoutineRef{}, "", fmt.Errorf("interbase: decode trigger source: %w", err)
				}
				relation, err := catalogText(row[2])
				if err != nil {
					return RoutineRef{}, "", fmt.Errorf("interbase: decode trigger relation: %w", err)
				}
				typ, err := activityCellInt64(row[3])
				if err != nil {
					return RoutineRef{}, "", fmt.Errorf("interbase: decode trigger type: %w", err)
				}
				event, _ := triggerEvent(int(typ))
				return RoutineRef{
					Kind: "trigger", Name: trimCatalogName(name),
					Relation: trimCatalogName(relation), Event: event,
				}, source, nil
			}
		}
		return RoutineRef{}, "", ErrRoutineNotFound
	default:
		return RoutineRef{}, "", fmt.Errorf("interbase: unsupported routine kind %q", ref.Kind)
	}
}

func firstCatalogRow(ctx context.Context, cursor *Cursor, columns int) (row []Cell, found bool, resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, cursor.Close())
	}()
	hasRow, err := cursor.Next(ctx)
	if err != nil {
		return nil, false, err
	}
	if !hasRow {
		return nil, false, nil
	}
	row, err = cursor.Row()
	if err != nil {
		return nil, false, err
	}
	if len(row) != columns {
		return nil, false, fmt.Errorf("interbase: catalog query returned %d columns, want %d", len(row), columns)
	}
	return row, true, nil
}

func visitCatalogCursor(ctx context.Context, cursor *Cursor, visit func([]Cell) error) (resultErr error) {
	defer func() {
		resultErr = errors.Join(resultErr, cursor.Close())
	}()
	for {
		hasRow, err := cursor.Next(ctx)
		if err != nil {
			return err
		}
		if !hasRow {
			return nil
		}
		row, err := cursor.Row()
		if err != nil {
			return err
		}
		if err := visit(row); err != nil {
			return err
		}
	}
}

func laneFromStatement(statement psql.Statement) RoutineLane {
	lane := RoutineLane{Line: statement.Line, Kind: statement.Kind}
	switch statement.Kind {
	case "executeStatement":
		lane.Message = "dynamic SQL"
	case "executeProcedure":
		if name := trimCatalogName(statement.Procedure); name != "" {
			lane.Refs = []RoutineRef{{Kind: "procedure", Name: name}}
		}
	default:
		lane.SQL = statement.SQL
	}
	return lane
}

func triggerEvent(triggerType int) (string, bool) {
	switch triggerType {
	case 1:
		return "before insert", true
	case 2:
		return "after insert", true
	case 3:
		return "before update", true
	case 4:
		return "after update", true
	case 5:
		return "before delete", true
	case 6:
		return "after delete", true
	default:
		return "", false
	}
}

func triggerMatchesStatement(kind string, triggerType int) bool {
	switch kind {
	case "insert":
		return triggerType == 1 || triggerType == 2
	case "update":
		return triggerType == 3 || triggerType == 4
	case "delete":
		return triggerType == 5 || triggerType == 6
	default:
		return false
	}
}

func deduplicateRoutineRefs(refs []RoutineRef) []RoutineRef {
	seen := make(map[string]struct{}, len(refs))
	result := make([]RoutineRef, 0, len(refs))
	for _, ref := range refs {
		key := strings.ToLower(ref.Kind) + "\x00" + catalogNameKey(ref.Name) + "\x00" +
			catalogNameKey(ref.Relation) + "\x00" + ref.Event
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		result = append(result, ref)
	}
	return result
}

func lookupNameVariants(names []string) []string {
	seen := make(map[string]struct{}, len(names)*2)
	result := make([]string, 0, len(names)*2)
	add := func(name string) {
		name = trimCatalogName(name)
		if name == "" {
			return
		}
		if _, ok := seen[name]; ok {
			return
		}
		seen[name] = struct{}{}
		result = append(result, name)
	}
	for _, name := range names {
		name = trimCatalogName(name)
		add(name)
		add(strings.ToUpper(name))
	}
	return result
}

func inPlaceholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?,", count), ",")
}

func stringArguments(values []string) []any {
	args := make([]any, len(values))
	for index, value := range values {
		args[index] = value
	}
	return args
}

func catalogText(cell Cell) (string, error) {
	if isCatalogNull(cell) {
		return "", nil
	}
	return activityCellText(cell)
}

func catalogTextTx(ctx context.Context, tx *Transaction, cell Cell) (string, error) {
	if isCatalogNull(cell) {
		return "", nil
	}
	reference, ok := cell.Value.(BlobRef)
	if !ok {
		return activityCellText(cell)
	}
	reader, err := tx.OpenBlob(ctx, reference)
	if err != nil {
		return "", err
	}
	data, readErr := io.ReadAll(reader)
	closeErr := reader.Close()
	if err := errors.Join(readErr, closeErr); err != nil {
		return "", err
	}
	return string(data), nil
}

func isCatalogNull(cell Cell) bool {
	return cell.Indicator&nullIndicator != 0 || cell.Value == nil
}

func catalogFloat64(cell Cell) (float64, error) {
	switch value := cell.Value.(type) {
	case float64:
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return 0, errors.New("non-finite floating-point value")
		}
		return value, nil
	case float32:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case []byte:
		parsed, err := strconv.ParseFloat(string(value), 64)
		return parsed, err
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		return parsed, err
	default:
		return 0, fmt.Errorf("unexpected floating-point value %T", cell.Value)
	}
}

func trimCatalogName(name string) string {
	return strings.TrimRight(name, " ")
}

func catalogNameKey(name string) string {
	return strings.ToUpper(trimCatalogName(name))
}

func prepareFailureMessage(err error) string {
	const prefix = "prepare failed: "
	message := err.Error()
	if line, _, ok := strings.Cut(message, "\n"); ok {
		message = line
	}
	message = strings.TrimSpace(message)
	maxMessageBytes := 200 - len(prefix)
	if len(message) > maxMessageBytes {
		message = message[:maxMessageBytes]
		for !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return prefix + message
}
