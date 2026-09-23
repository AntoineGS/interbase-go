package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Queryer is the part of database/sql needed by Catalog.
//
// *sql.DB, *sql.Conn, and *sql.Tx all implement this interface. Supplying a
// transaction is useful when the caller needs all metadata queries to share
// one explicit consistency boundary.
type Queryer interface {
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

// ErrNilQueryer reports that a Catalog was created without a query source.
var ErrNilQueryer = errors.New("schema: nil Queryer")

// RelationKind identifies the kind of a Relation.
type RelationKind string

const (
	// RelationTable identifies a persistent or temporary table returned by
	// Tables or Relations.
	RelationTable RelationKind = "table"
	// RelationView identifies a view returned by Views or Relations.
	RelationView RelationKind = "view"
)

// Catalog reads the bounded set of InterBase catalog objects supported by this
// package.
type Catalog struct {
	queryer              Queryer
	identifierWidthMu    sync.Mutex
	identifierWidthCache map[identifierField]int
}

// New returns a catalog backed by queryer. A nil queryer is accepted so the
// zero value remains constructible; operations on it return ErrNilQueryer.
func New(queryer Queryer) *Catalog {
	return &Catalog{queryer: queryer}
}

// Relation describes a table or view and its ordered columns.
type Relation struct {
	Name          string
	Kind          RelationKind
	ID            sql.NullInt64
	ViewSource    sql.NullString
	Description   sql.NullString
	SecurityClass sql.NullString
	OwnerName     sql.NullString
	DefaultClass  sql.NullString
	DBKeyLength   sql.NullInt64
	Format        sql.NullInt64
	ExternalFile  sql.NullString
	Flags         sql.NullInt64
	RelationType  sql.NullString
	SystemFlag    sql.NullInt64
	Columns       []Column
	// Constraints are populated by Catalog.Table and by callers that assemble
	// a complete relation snapshot. Tables and Relations keep the initial
	// column-only read inexpensive, so GenerateDDL requires ConstraintsLoaded.
	Constraints       []Constraint
	ConstraintsLoaded bool
	Indexes           []Index
	IndexesLoaded     bool
	Triggers          []Trigger
	TriggersLoaded    bool
}

// Column describes a column in a relation. FieldSource preserves the nullable
// catalog identifier, and Domain contains the corresponding RDB$FIELDS
// metadata when the catalog row resolves to a domain.
type Column struct {
	Name           string
	RelationName   string
	FieldSource    sql.NullString
	Position       sql.NullInt64
	UpdateFlag     sql.NullInt64
	FieldID        sql.NullInt64
	Description    sql.NullString
	SystemFlag     sql.NullInt64
	SecurityClass  sql.NullString
	NullFlag       sql.NullInt64
	Nullable       sql.NullBool
	DefaultSource  sql.NullString
	CollationID    sql.NullInt64
	CollationName  sql.NullString
	BaseField      sql.NullString
	BaseRelation   sql.NullString
	ComputedSource sql.NullString
	Domain         *Domain
}

// Domain contains the RDB$FIELDS attributes associated with a column or
// procedure parameter. Source fields are intentionally not normalized: their
// exact catalog text is useful to callers.
type Domain struct {
	Name             string
	ValidationSource sql.NullString
	ComputedSource   sql.NullString
	DefaultSource    sql.NullString
	FieldLength      sql.NullInt64
	FieldScale       sql.NullInt64
	FieldType        sql.NullInt64
	FieldSubType     sql.NullInt64
	Description      sql.NullString
	SystemFlag       sql.NullInt64
	SegmentLength    sql.NullInt64
	ExternalLength   sql.NullInt64
	ExternalScale    sql.NullInt64
	ExternalType     sql.NullInt64
	Dimensions       sql.NullInt64
	NullFlag         sql.NullInt64
	Nullable         sql.NullBool
	CharacterLength  sql.NullInt64
	CollationID      sql.NullInt64
	CharacterSetID   sql.NullInt64
	FieldPrecision   sql.NullInt64
	CharacterSetName sql.NullString
	CollationName    sql.NullString
}

// ParameterDirection identifies whether a procedure parameter is an input or
// output parameter.
type ParameterDirection string

const (
	// ParameterInput identifies an input parameter.
	ParameterInput ParameterDirection = "input"
	// ParameterOutput identifies an output parameter.
	ParameterOutput ParameterDirection = "output"
)

// Procedure describes a stored procedure and its ordered input and output
// parameters.
type Procedure struct {
	Name             string
	ID               sql.NullInt64
	InputCount       sql.NullInt64
	OutputCount      sql.NullInt64
	Description      sql.NullString
	Source           sql.NullString
	SecurityClass    sql.NullString
	OwnerName        sql.NullString
	SystemFlag       sql.NullInt64
	InputParameters  []ProcedureParameter
	OutputParameters []ProcedureParameter
}

// ProcedureParameter describes one procedure parameter using the
// reference-compatible RDB$PROCEDURE_PARAMETERS projection. Domain is resolved
// separately from FieldSource when available.
type ProcedureParameter struct {
	Name          string
	ProcedureName string
	Number        sql.NullInt64
	Direction     ParameterDirection
	ParameterType sql.NullInt64
	FieldSource   sql.NullString
	Description   sql.NullString
	SystemFlag    sql.NullInt64
	Domain        *Domain
	// Nullable is valid only when the applicable domain's NOT NULL flag
	// proves the parameter is non-null. The reference-compatible parameter
	// projection does not expose a declaration nullability flag, so a nullable
	// domain cannot prove that the parameter itself is nullable.
	Nullable sql.NullBool
}

const relationProjection = `
SELECT r.RDB$RELATION_NAME, r.RDB$RELATION_ID, r.RDB$VIEW_SOURCE,
       r.RDB$DESCRIPTION, r.RDB$SECURITY_CLASS, r.RDB$OWNER_NAME,
       r.RDB$DEFAULT_CLASS, r.RDB$DBKEY_LENGTH, r.RDB$FORMAT,
       r.RDB$EXTERNAL_FILE, r.RDB$FLAGS, r.RDB$RELATION_TYPE,
       r.RDB$SYSTEM_FLAG,
       CASE WHEN r.RDB$VIEW_BLR IS NULL THEN 0 ELSE 1 END AS RELATION_KIND
FROM RDB$RELATIONS r`

const relationColumnsQuery = `
SELECT rf.RDB$FIELD_NAME, rf.RDB$RELATION_NAME, rf.RDB$FIELD_SOURCE,
       rf.RDB$FIELD_POSITION, rf.RDB$UPDATE_FLAG, rf.RDB$FIELD_ID,
       rf.RDB$DESCRIPTION, rf.RDB$SYSTEM_FLAG, rf.RDB$SECURITY_CLASS,
       rf.RDB$NULL_FLAG, rf.RDB$DEFAULT_SOURCE, rf.RDB$COLLATION_ID,
       rf.RDB$BASE_FIELD, v.RDB$RELATION_NAME AS BASE_RELATION,
       f.RDB$FIELD_NAME AS DOMAIN_NAME, f.RDB$VALIDATION_SOURCE,
       f.RDB$COMPUTED_SOURCE, f.RDB$DEFAULT_SOURCE AS DOMAIN_DEFAULT_SOURCE,
       f.RDB$FIELD_LENGTH, f.RDB$FIELD_SCALE, f.RDB$FIELD_TYPE,
       f.RDB$FIELD_SUB_TYPE, f.RDB$DESCRIPTION AS DOMAIN_DESCRIPTION,
       f.RDB$SYSTEM_FLAG AS DOMAIN_SYSTEM_FLAG, f.RDB$SEGMENT_LENGTH,
       f.RDB$EXTERNAL_LENGTH, f.RDB$EXTERNAL_SCALE, f.RDB$EXTERNAL_TYPE,
       f.RDB$DIMENSIONS, f.RDB$NULL_FLAG AS DOMAIN_NULL_FLAG,
       f.RDB$CHARACTER_LENGTH, f.RDB$COLLATION_ID AS DOMAIN_COLLATION_ID,
       f.RDB$CHARACTER_SET_ID, f.RDB$FIELD_PRECISION,
        cs.RDB$CHARACTER_SET_NAME, co.RDB$COLLATION_NAME,
        rco.RDB$COLLATION_NAME AS COLUMN_COLLATION_NAME
 FROM RDB$RELATION_FIELDS rf
 LEFT JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
 LEFT JOIN RDB$CHARACTER_SETS cs ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
  LEFT JOIN RDB$COLLATIONS co ON co.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
                             AND co.RDB$COLLATION_ID = f.RDB$COLLATION_ID
  LEFT JOIN RDB$COLLATIONS rco ON rco.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
                              AND rco.RDB$COLLATION_ID = rf.RDB$COLLATION_ID
 LEFT JOIN RDB$VIEW_RELATIONS v ON v.RDB$VIEW_NAME = rf.RDB$RELATION_NAME
                               AND v.RDB$VIEW_CONTEXT = rf.RDB$VIEW_CONTEXT
WHERE rf.RDB$RELATION_NAME = ?
 ORDER BY rf.RDB$FIELD_POSITION`

const domainQuery = `
SELECT f.RDB$FIELD_NAME, f.RDB$VALIDATION_SOURCE,
       f.RDB$COMPUTED_SOURCE, f.RDB$DEFAULT_SOURCE, f.RDB$FIELD_LENGTH,
       f.RDB$FIELD_SCALE, f.RDB$FIELD_TYPE, f.RDB$FIELD_SUB_TYPE,
       f.RDB$DESCRIPTION, f.RDB$SYSTEM_FLAG, f.RDB$SEGMENT_LENGTH,
       f.RDB$EXTERNAL_LENGTH, f.RDB$EXTERNAL_SCALE, f.RDB$EXTERNAL_TYPE,
       f.RDB$DIMENSIONS, f.RDB$NULL_FLAG, f.RDB$CHARACTER_LENGTH,
       f.RDB$COLLATION_ID, f.RDB$CHARACTER_SET_ID, f.RDB$FIELD_PRECISION,
       cs.RDB$CHARACTER_SET_NAME, co.RDB$COLLATION_NAME
 FROM RDB$FIELDS f
 LEFT JOIN RDB$CHARACTER_SETS cs ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
 LEFT JOIN RDB$COLLATIONS co ON co.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
                            AND co.RDB$COLLATION_ID = f.RDB$COLLATION_ID
 WHERE f.RDB$FIELD_NAME = ?`

const procedureProjection = `
SELECT p.RDB$PROCEDURE_NAME, p.RDB$PROCEDURE_ID,
       p.RDB$PROCEDURE_INPUTS, p.RDB$PROCEDURE_OUTPUTS,
       p.RDB$DESCRIPTION, p.RDB$PROCEDURE_SOURCE,
       p.RDB$SECURITY_CLASS, p.RDB$OWNER_NAME, p.RDB$SYSTEM_FLAG
FROM RDB$PROCEDURES p`

const procedureParametersQuery = `
SELECT pp.RDB$PARAMETER_NAME, pp.RDB$PROCEDURE_NAME,
       pp.RDB$PARAMETER_NUMBER, pp.RDB$PARAMETER_TYPE,
       pp.RDB$FIELD_SOURCE, pp.RDB$DESCRIPTION, pp.RDB$SYSTEM_FLAG
FROM RDB$PROCEDURE_PARAMETERS pp
WHERE pp.RDB$PROCEDURE_NAME = ?
ORDER BY pp.RDB$PARAMETER_TYPE, pp.RDB$PARAMETER_NUMBER`

// Tables returns user table relations matching name exactly. An empty name
// lists all user tables in catalog order.
func (c *Catalog) Tables(ctx context.Context, name string) ([]Relation, error) {
	return c.relations(ctx, name, RelationTable)
}

// Views returns user views matching name exactly. An empty name lists all user
// views in catalog order.
func (c *Catalog) Views(ctx context.Context, name string) ([]Relation, error) {
	return c.relations(ctx, name, RelationView)
}

// Relations returns all user tables and views matching name exactly. An empty
// name lists both kinds in catalog order.
func (c *Catalog) Relations(ctx context.Context, name string) ([]Relation, error) {
	return c.relations(ctx, name, "")
}

// Table returns the matching table, or nil when no such user table exists.
func (c *Catalog) Table(ctx context.Context, name string) (*Relation, error) {
	if name == "" {
		return nil, errors.New("schema: table name is required")
	}
	relations, err := c.Tables(ctx, name)
	if err != nil || len(relations) == 0 {
		return nil, err
	}
	if err := c.loadRelationDDLMetadata(ctx, &relations[0]); err != nil {
		return nil, err
	}
	return &relations[0], nil
}

// View returns the matching view, or nil when no such user view exists.
func (c *Catalog) View(ctx context.Context, name string) (*Relation, error) {
	if name == "" {
		return nil, errors.New("schema: view name is required")
	}
	relations, err := c.Views(ctx, name)
	if err != nil || len(relations) == 0 {
		return nil, err
	}
	return &relations[0], nil
}

func (c *Catalog) relations(ctx context.Context, name string, kind RelationKind) ([]Relation, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.queryer == nil {
		return nil, ErrNilQueryer
	}

	query := relationProjection + "\nWHERE COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0"
	if kind == RelationTable {
		query += "\n  AND r.RDB$VIEW_BLR IS NULL"
	} else if kind == RelationView {
		query = relationProjection + "\nWHERE r.RDB$VIEW_BLR IS NOT NULL\n  AND COALESCE(r.RDB$SYSTEM_FLAG, 0) = 0"
	}
	args := make([]any, 0, 1)
	if name != "" {
		query += "\n  AND r.RDB$RELATION_NAME = ?"
		args = append(args, name)
	}
	query += "\nORDER BY r.RDB$RELATION_NAME"

	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query %s relations: %w", relationKindLabel(kind), err)
	}
	relations := make([]Relation, 0)
	for rows.Next() {
		var (
			nameValue sql.NullString
			relation  Relation
			kindValue sql.NullInt64
		)
		if err := rows.Scan(
			&nameValue, &relation.ID, &relation.ViewSource,
			&relation.Description, &relation.SecurityClass, &relation.OwnerName,
			&relation.DefaultClass, &relation.DBKeyLength, &relation.Format,
			&relation.ExternalFile, &relation.Flags, &relation.RelationType,
			&relation.SystemFlag, &kindValue,
		); err != nil {
			return nil, fmt.Errorf("schema: scan %s relation: %w", relationKindLabel(kind), closeRows(rows, err))
		}
		relation.Name, err = requiredIdentifier(nameValue, "relation name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan %s relation: %w", relationKindLabel(kind), closeRows(rows, err))
		}
		if kind != "" {
			relation.Kind = kind
		} else {
			switch {
			case kindValue.Valid && kindValue.Int64 == 0:
				relation.Kind = RelationTable
			case kindValue.Valid && kindValue.Int64 == 1:
				relation.Kind = RelationView
			default:
				return nil, fmt.Errorf("schema: scan all relation: %w", closeRows(rows, errors.New("relation kind is invalid")))
			}
		}
		relation.SecurityClass = trimIdentifier(relation.SecurityClass)
		relation.OwnerName = trimIdentifier(relation.OwnerName)
		relation.DefaultClass = trimIdentifier(relation.DefaultClass)
		relation.RelationType = trimIdentifier(relation.RelationType)
		relations = append(relations, relation)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate %s relations: %w", relationKindLabel(kind), closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close %s relations: %w", relationKindLabel(kind), err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}

	for index := range relations {
		columns, err := c.Columns(ctx, relations[index].Name)
		if err != nil {
			return nil, fmt.Errorf("schema: load columns for %s %q: %w", relationKindLabel(kind), relations[index].Name, err)
		}
		relations[index].Columns = columns
	}
	return relations, nil
}

func relationKindLabel(kind RelationKind) string {
	if kind == "" {
		return "all"
	}
	return string(kind)
}

func (c *Catalog) loadRelationDDLMetadata(ctx context.Context, relation *Relation) error {
	if relation == nil {
		return errors.New("schema: relation is required")
	}
	constraints, err := c.ConstraintsForRelation(ctx, relation.Name)
	if err != nil {
		return fmt.Errorf("schema: load constraints for table %q: %w", relation.Name, err)
	}
	relation.Constraints = constraints
	relation.ConstraintsLoaded = true
	indexes, err := c.IndexesForRelation(ctx, relation.Name)
	if err != nil {
		return fmt.Errorf("schema: load indexes for table %q: %w", relation.Name, err)
	}
	relation.Indexes = indexes
	relation.IndexesLoaded = true
	triggers, err := c.TriggersForRelation(ctx, relation.Name)
	if err != nil {
		return fmt.Errorf("schema: load triggers for table %q: %w", relation.Name, err)
	}
	relation.Triggers = triggers
	relation.TriggersLoaded = true
	return nil
}

// Columns returns ordered columns for relationName. Relation names are
// matched exactly; an unknown name returns an empty slice.
func (c *Catalog) Columns(ctx context.Context, relationName string) ([]Column, error) {
	if relationName == "" {
		return nil, errors.New("schema: relation name is required")
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.queryer == nil {
		return nil, ErrNilQueryer
	}
	rows, err := c.query(ctx, relationColumnsQuery, relationName)
	if err != nil {
		return nil, fmt.Errorf("schema: query columns for %q: %w", relationName, err)
	}
	columns := make([]Column, 0)
	for rows.Next() {
		column, err := scanColumn(rows)
		if err != nil {
			return nil, fmt.Errorf("schema: scan column for %q: %w", relationName, closeRows(rows, err))
		}
		columns = append(columns, column)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate columns for %q: %w", relationName, closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close columns for %q: %w", relationName, err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return columns, nil
}

// Procedures returns user procedures matching name exactly. An empty name
// lists all user procedures in catalog order.
func (c *Catalog) Procedures(ctx context.Context, name string) ([]Procedure, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.queryer == nil {
		return nil, ErrNilQueryer
	}

	query := procedureProjection + "\nWHERE COALESCE(p.RDB$SYSTEM_FLAG, 0) = 0"
	args := make([]any, 0, 1)
	if name != "" {
		query += "\n  AND p.RDB$PROCEDURE_NAME = ?"
		args = append(args, name)
	}
	query += "\nORDER BY p.RDB$PROCEDURE_NAME"

	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query procedures: %w", err)
	}
	procedures := make([]Procedure, 0)
	for rows.Next() {
		var (
			nameValue sql.NullString
			procedure Procedure
		)
		if err := rows.Scan(
			&nameValue, &procedure.ID, &procedure.InputCount,
			&procedure.OutputCount, &procedure.Description, &procedure.Source,
			&procedure.SecurityClass, &procedure.OwnerName, &procedure.SystemFlag,
		); err != nil {
			return nil, fmt.Errorf("schema: scan procedure: %w", closeRows(rows, err))
		}
		procedure.Name, err = requiredIdentifier(nameValue, "procedure name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan procedure: %w", closeRows(rows, err))
		}
		procedure.SecurityClass = trimIdentifier(procedure.SecurityClass)
		procedure.OwnerName = trimIdentifier(procedure.OwnerName)
		procedures = append(procedures, procedure)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate procedures: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close procedures: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}

	for index := range procedures {
		input, output, err := c.procedureParameters(ctx, procedures[index].Name)
		if err != nil {
			return nil, fmt.Errorf("schema: load parameters for procedure %q: %w", procedures[index].Name, err)
		}
		procedures[index].InputParameters = input
		procedures[index].OutputParameters = output
	}
	return procedures, nil
}

// Procedure returns the matching user procedure, or nil when no such
// procedure exists.
func (c *Catalog) Procedure(ctx context.Context, name string) (*Procedure, error) {
	if name == "" {
		return nil, errors.New("schema: procedure name is required")
	}
	procedures, err := c.Procedures(ctx, name)
	if err != nil || len(procedures) == 0 {
		return nil, err
	}
	return &procedures[0], nil
}

func (c *Catalog) procedureParameters(ctx context.Context, procedureName string) ([]ProcedureParameter, []ProcedureParameter, error) {
	rows, err := c.query(ctx, procedureParametersQuery, procedureName)
	if err != nil {
		return nil, nil, fmt.Errorf("schema: query parameters for %q: %w", procedureName, err)
	}
	input := make([]ProcedureParameter, 0)
	output := make([]ProcedureParameter, 0)
	for rows.Next() {
		parameter, err := scanProcedureParameter(rows)
		if err != nil {
			return nil, nil, fmt.Errorf("schema: scan parameter for %q: %w", procedureName, closeRows(rows, err))
		}
		switch parameter.Direction {
		case ParameterInput:
			input = append(input, parameter)
		case ParameterOutput:
			output = append(output, parameter)
		default:
			err := fmt.Errorf("schema: parameter %q has unknown direction %q", parameter.Name, parameter.Direction)
			return nil, nil, closeRows(rows, err)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("schema: iterate parameters for %q: %w", procedureName, closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, nil, fmt.Errorf("schema: close parameters for %q: %w", procedureName, err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, nil, err
	}
	for index := range input {
		if err := c.loadParameterDomain(ctx, &input[index]); err != nil {
			return nil, nil, fmt.Errorf("schema: load domain for input parameter %q: %w", input[index].Name, err)
		}
	}
	for index := range output {
		if err := c.loadParameterDomain(ctx, &output[index]); err != nil {
			return nil, nil, fmt.Errorf("schema: load domain for output parameter %q: %w", output[index].Name, err)
		}
	}
	return input, output, nil
}

func (c *Catalog) loadParameterDomain(ctx context.Context, parameter *ProcedureParameter) error {
	if !parameter.FieldSource.Valid || parameter.FieldSource.String == "" {
		return nil
	}
	domain, err := c.domain(ctx, parameter.FieldSource.String)
	if err != nil {
		return err
	}
	parameter.Domain = domain
	parameter.Nullable = parameterNullable(domain)
	return nil
}

func (c *Catalog) domain(ctx context.Context, name string) (*Domain, error) {
	rows, err := c.query(ctx, domainQuery, name)
	if err != nil {
		return nil, fmt.Errorf("schema: query domain %q: %w", name, err)
	}
	var result *Domain
	for rows.Next() {
		domain, err := scanDomain(rows)
		if err != nil {
			return nil, fmt.Errorf("schema: scan domain %q: %w", name, closeRows(rows, err))
		}
		if result != nil {
			return nil, fmt.Errorf("schema: scan domain %q: %w", name, closeRows(rows, errors.New("multiple domain rows returned")))
		}
		result = domain
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate domain %q: %w", name, closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close domain %q: %w", name, err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

func scanDomain(rows *sql.Rows) (*Domain, error) {
	var raw domainRow
	if err := rows.Scan(raw.destinations()...); err != nil {
		return nil, err
	}
	return raw.value()
}

func parameterNullable(domain *Domain) sql.NullBool {
	if domain == nil || !domain.Nullable.Valid || domain.Nullable.Bool {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: false, Valid: true}
}

func columnNullable(flag sql.NullInt64, domain *Domain) sql.NullBool {
	if flag.Valid && flag.Int64 != 0 {
		return sql.NullBool{Bool: false, Valid: true}
	}
	if domain != nil && domain.NullFlag.Valid && domain.NullFlag.Int64 != 0 {
		return sql.NullBool{Bool: false, Valid: true}
	}
	if flag.Valid {
		return sql.NullBool{Bool: true, Valid: true}
	}
	return sql.NullBool{}
}

type parameterRow struct {
	name          sql.NullString
	procedureName sql.NullString
	number        sql.NullInt64
	parameterType sql.NullInt64
	fieldSource   sql.NullString
	description   sql.NullString
	systemFlag    sql.NullInt64
}

func (r *parameterRow) destinations() []any {
	return []any{
		&r.name, &r.procedureName, &r.number, &r.parameterType,
		&r.fieldSource, &r.description, &r.systemFlag,
	}
}

func scanProcedureParameter(rows *sql.Rows) (ProcedureParameter, error) {
	var raw parameterRow
	if err := rows.Scan(raw.destinations()...); err != nil {
		return ProcedureParameter{}, err
	}
	name, err := requiredIdentifier(raw.name, "parameter name")
	if err != nil {
		return ProcedureParameter{}, err
	}
	procedureName, err := requiredIdentifier(raw.procedureName, "parameter procedure name")
	if err != nil {
		return ProcedureParameter{}, err
	}
	var direction ParameterDirection
	switch {
	case raw.parameterType.Valid && raw.parameterType.Int64 == 0:
		direction = ParameterInput
	case raw.parameterType.Valid && raw.parameterType.Int64 == 1:
		direction = ParameterOutput
	default:
		return ProcedureParameter{}, fmt.Errorf("parameter %q has invalid parameter type %v", name, raw.parameterType)
	}
	return ProcedureParameter{
		Name:          name,
		ProcedureName: procedureName,
		Number:        raw.number,
		Direction:     direction,
		ParameterType: raw.parameterType,
		FieldSource:   trimIdentifier(raw.fieldSource),
		Description:   raw.description,
		SystemFlag:    raw.systemFlag,
	}, nil
}

type columnRow struct {
	fieldName     sql.NullString
	relationName  sql.NullString
	fieldSource   sql.NullString
	position      sql.NullInt64
	updateFlag    sql.NullInt64
	fieldID       sql.NullInt64
	description   sql.NullString
	systemFlag    sql.NullInt64
	securityClass sql.NullString
	nullFlag      sql.NullInt64
	defaultSource sql.NullString
	collationID   sql.NullInt64
	collationName sql.NullString
	baseField     sql.NullString
	baseRelation  sql.NullString
	domain        domainRow
}

func (r *columnRow) destinations() []any {
	return []any{
		&r.fieldName, &r.relationName, &r.fieldSource,
		&r.position, &r.updateFlag, &r.fieldID, &r.description,
		&r.systemFlag, &r.securityClass, &r.nullFlag, &r.defaultSource,
		&r.collationID, &r.baseField, &r.baseRelation,
		&r.domain.fieldName, &r.domain.validationSource, &r.domain.computedSource,
		&r.domain.defaultSource, &r.domain.fieldLength, &r.domain.fieldScale,
		&r.domain.fieldType, &r.domain.fieldSubType, &r.domain.description,
		&r.domain.systemFlag, &r.domain.segmentLength, &r.domain.externalLength,
		&r.domain.externalScale, &r.domain.externalType, &r.domain.dimensions,
		&r.domain.nullFlag, &r.domain.characterLength, &r.domain.collationID,
		&r.domain.characterSetID, &r.domain.fieldPrecision,
		&r.domain.characterSetName, &r.domain.collationName, &r.collationName,
	}
}

func scanColumn(rows *sql.Rows) (Column, error) {
	var raw columnRow
	if err := rows.Scan(raw.destinations()...); err != nil {
		return Column{}, err
	}
	name, err := requiredIdentifier(raw.fieldName, "column name")
	if err != nil {
		return Column{}, err
	}
	relationName, err := requiredIdentifier(raw.relationName, "column relation name")
	if err != nil {
		return Column{}, err
	}
	fieldSource := trimIdentifier(raw.fieldSource)
	baseField, err := optionalIdentifier(raw.baseField)
	if err != nil {
		return Column{}, err
	}
	baseRelation, err := optionalIdentifier(raw.baseRelation)
	if err != nil {
		return Column{}, err
	}
	var domain *Domain
	if raw.fieldSource.Valid && strings.TrimRight(raw.fieldSource.String, " ") != "" {
		domain, err = raw.domain.value()
		if err != nil {
			return Column{}, err
		}
	}
	column := Column{
		Name:          name,
		RelationName:  relationName,
		FieldSource:   fieldSource,
		Position:      raw.position,
		UpdateFlag:    raw.updateFlag,
		FieldID:       raw.fieldID,
		Description:   raw.description,
		SystemFlag:    raw.systemFlag,
		SecurityClass: trimIdentifier(raw.securityClass),
		NullFlag:      raw.nullFlag,
		Nullable:      columnNullable(raw.nullFlag, domain),
		DefaultSource: raw.defaultSource,
		CollationID:   raw.collationID,
		CollationName: trimIdentifier(raw.collationName),
		BaseField:     nullableIdentifier(baseField, raw.baseField.Valid),
		BaseRelation:  nullableIdentifier(baseRelation, raw.baseRelation.Valid),
		Domain:        domain,
	}
	if domain != nil {
		column.ComputedSource = domain.ComputedSource
	}
	return column, nil
}

type domainRow struct {
	fieldName        sql.NullString
	validationSource sql.NullString
	computedSource   sql.NullString
	defaultSource    sql.NullString
	fieldLength      sql.NullInt64
	fieldScale       sql.NullInt64
	fieldType        sql.NullInt64
	fieldSubType     sql.NullInt64
	description      sql.NullString
	systemFlag       sql.NullInt64
	segmentLength    sql.NullInt64
	externalLength   sql.NullInt64
	externalScale    sql.NullInt64
	externalType     sql.NullInt64
	dimensions       sql.NullInt64
	nullFlag         sql.NullInt64
	characterLength  sql.NullInt64
	collationID      sql.NullInt64
	characterSetID   sql.NullInt64
	fieldPrecision   sql.NullInt64
	characterSetName sql.NullString
	collationName    sql.NullString
}

func (r *domainRow) destinations() []any {
	return []any{
		&r.fieldName, &r.validationSource, &r.computedSource, &r.defaultSource,
		&r.fieldLength, &r.fieldScale, &r.fieldType, &r.fieldSubType,
		&r.description, &r.systemFlag, &r.segmentLength, &r.externalLength,
		&r.externalScale, &r.externalType, &r.dimensions, &r.nullFlag,
		&r.characterLength, &r.collationID, &r.characterSetID, &r.fieldPrecision,
		&r.characterSetName, &r.collationName,
	}
}

func (r *domainRow) value() (*Domain, error) {
	if !r.fieldName.Valid {
		return nil, nil
	}
	name, err := requiredIdentifier(r.fieldName, "domain name")
	if err != nil {
		return nil, err
	}
	return &Domain{
		Name:             name,
		ValidationSource: r.validationSource,
		ComputedSource:   r.computedSource,
		DefaultSource:    r.defaultSource,
		FieldLength:      r.fieldLength,
		FieldScale:       r.fieldScale,
		FieldType:        r.fieldType,
		FieldSubType:     r.fieldSubType,
		Description:      r.description,
		SystemFlag:       r.systemFlag,
		SegmentLength:    r.segmentLength,
		ExternalLength:   r.externalLength,
		ExternalScale:    r.externalScale,
		ExternalType:     r.externalType,
		Dimensions:       r.dimensions,
		NullFlag:         r.nullFlag,
		Nullable:         nullable(r.nullFlag),
		CharacterLength:  r.characterLength,
		CollationID:      r.collationID,
		CharacterSetID:   r.characterSetID,
		FieldPrecision:   r.fieldPrecision,
		CharacterSetName: trimIdentifier(r.characterSetName),
		CollationName:    trimIdentifier(r.collationName),
	}, nil
}

func (c *Catalog) query(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	rows, err := c.queryer.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	if rows == nil {
		return nil, errors.New("schema: QueryContext returned nil rows")
	}
	return rows, nil
}

func closeRows(rows *sql.Rows, primary error) error {
	closeErr := rows.Close()
	if primary == nil {
		return closeErr
	}
	if closeErr == nil {
		return primary
	}
	return errors.Join(primary, closeErr)
}

func contextErr(ctx context.Context) error {
	if ctx == nil {
		return errors.New("schema: nil context")
	}
	return ctx.Err()
}

func requiredIdentifier(value sql.NullString, label string) (string, error) {
	if !value.Valid {
		return "", fmt.Errorf("%s is NULL", label)
	}
	result := strings.TrimRight(value.String, " ")
	if result == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	return result, nil
}

func optionalIdentifier(value sql.NullString) (string, error) {
	if !value.Valid {
		return "", nil
	}
	return strings.TrimRight(value.String, " "), nil
}

func nullableIdentifier(value string, valid bool) sql.NullString {
	return sql.NullString{String: value, Valid: valid}
}

func trimIdentifier(value sql.NullString) sql.NullString {
	if value.Valid {
		value.String = strings.TrimRight(value.String, " ")
	}
	return value
}

func nullable(flag sql.NullInt64) sql.NullBool {
	if !flag.Valid {
		return sql.NullBool{}
	}
	return sql.NullBool{Bool: flag.Int64 == 0, Valid: true}
}
