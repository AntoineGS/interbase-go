package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// Sequence describes a user generator/sequence from RDB$GENERATORS.
//
// InterBase exposes generators and sequences through the same catalog. The
// two Catalog methods are aliases and return the same type.
type Sequence struct {
	Name       string
	ID         sql.NullInt64
	SystemFlag sql.NullInt64
}

// Generator is retained as a descriptive alias for Sequence.
type Generator = Sequence

// IndexSegment describes one ordered segment of an index.
type IndexSegment struct {
	IndexName  string
	FieldName  string
	Position   sql.NullInt64
	Statistics sql.NullFloat64
}

// Index describes an InterBase index and its ordered segments.
type Index struct {
	Name           string
	RelationName   string
	ID             sql.NullInt64
	UniqueFlag     sql.NullInt64
	Description    sql.NullString
	SegmentCount   sql.NullInt64
	Inactive       sql.NullInt64
	IndexType      sql.NullInt64
	ForeignKey     sql.NullString
	SystemFlag     sql.NullInt64
	Expression     sql.NullString
	Statistics     sql.NullFloat64
	ConstraintName sql.NullString
	Segments       []IndexSegment
}

// ConstraintKind is the normalized text stored in RDB$CONSTRAINT_TYPE.
type ConstraintKind string

const (
	ConstraintPrimaryKey ConstraintKind = "PRIMARY KEY"
	ConstraintUnique     ConstraintKind = "UNIQUE"
	ConstraintForeignKey ConstraintKind = "FOREIGN KEY"
	ConstraintCheck      ConstraintKind = "CHECK"
	ConstraintNotNull    ConstraintKind = "NOT NULL"
)

// Constraint describes a relation constraint. Columns and referenced columns
// are loaded from the enforcing and referenced indexes when those indexes are
// available. CheckSource preserves the check trigger source verbatim.
type Constraint struct {
	Name                     string
	ConstraintType           string
	RelationName             string
	Deferrable               sql.NullString
	InitiallyDeferred        sql.NullString
	IndexName                sql.NullString
	TriggerName              sql.NullString
	ReferencedConstraintName sql.NullString
	MatchOption              sql.NullString
	UpdateRule               sql.NullString
	DeleteRule               sql.NullString
	ReferencedRelationName   string
	ReferencedIndexName      sql.NullString
	ColumnName               sql.NullString
	TriggerNames             []string
	CheckSource              sql.NullString
	Columns                  []string
	ReferencedColumns        []string
	Index                    *Index
	PartnerConstraint        *Constraint
}

// Trigger describes a DML or database trigger and preserves its PSQL source.
type Trigger struct {
	Name         string
	RelationName sql.NullString
	Sequence     sql.NullInt64
	TriggerType  sql.NullInt64
	Source       sql.NullString
	Description  sql.NullString
	Inactive     sql.NullInt64
	SystemFlag   sql.NullInt64
	Flags        sql.NullInt64
}

// Role describes a database role.
type Role struct {
	Name       string
	OwnerName  sql.NullString
	SystemFlag sql.NullInt64
}

// Dependency describes one edge in the catalog dependency relation.
type Dependency struct {
	DependentName  string
	DependentType  sql.NullInt64
	FieldName      sql.NullString
	DependedOnName string
	DependedOnType sql.NullInt64
}

// Function describes an external function declaration. This package only
// reads the declaration and its arguments; it never invokes the UDF.
type Function struct {
	Name           string
	FunctionType   sql.NullInt64
	Description    sql.NullString
	ModuleName     sql.NullString
	EntryPoint     sql.NullString
	ReturnArgument sql.NullInt64
	SystemFlag     sql.NullInt64
	Arguments      []FunctionArgument
}

// ExternalFunction is a descriptive alias for Function.
type ExternalFunction = Function

// FunctionArgument describes one external function argument.
type FunctionArgument struct {
	Name            string
	FunctionName    string
	Position        sql.NullInt64
	Mechanism       sql.NullInt64
	FieldLength     sql.NullInt64
	FieldScale      sql.NullInt64
	FieldType       sql.NullInt64
	FieldSubType    sql.NullInt64
	CharacterSetID  sql.NullInt64
	FieldPrecision  sql.NullInt64
	CharacterLength sql.NullInt64
}

// UDFArgument is a descriptive alias for FunctionArgument.
type UDFArgument = FunctionArgument

// DatabaseFile describes one database or shadow extension file.
type DatabaseFile struct {
	Name         string
	FileName     sql.NullString
	Sequence     sql.NullInt64
	Start        sql.NullInt64
	Length       sql.NullInt64
	ShadowNumber sql.NullInt64
}

// Shadow describes a database shadow and its ordered files.
type Shadow struct {
	ID    sql.NullInt64
	Flags sql.NullInt64
	Files []DatabaseFile
}

// Privilege describes one row from RDB$USER_PRIVILEGES.
type Privilege struct {
	Grantee       string
	Grantor       string
	PrivilegeCode string
	GrantOption   sql.NullInt64
	SubjectName   string
	FieldName     sql.NullString
	GranteeType   sql.NullInt64
	SubjectType   sql.NullInt64
}

const extendedDomainQueryTemplate = `
SELECT %s, f.RDB$VALIDATION_SOURCE,
       f.RDB$COMPUTED_SOURCE, f.RDB$DEFAULT_SOURCE, f.RDB$FIELD_LENGTH,
       f.RDB$FIELD_SCALE, f.RDB$FIELD_TYPE, f.RDB$FIELD_SUB_TYPE,
       f.RDB$DESCRIPTION, f.RDB$SYSTEM_FLAG, f.RDB$SEGMENT_LENGTH,
       f.RDB$EXTERNAL_LENGTH, f.RDB$EXTERNAL_SCALE, f.RDB$EXTERNAL_TYPE,
       f.RDB$DIMENSIONS, f.RDB$NULL_FLAG, f.RDB$CHARACTER_LENGTH,
       f.RDB$COLLATION_ID, f.RDB$CHARACTER_SET_ID, f.RDB$FIELD_PRECISION,
        %s, %s
FROM RDB$FIELDS f
LEFT JOIN RDB$CHARACTER_SETS cs
       ON cs.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
LEFT JOIN RDB$COLLATIONS co
       ON co.RDB$CHARACTER_SET_ID = f.RDB$CHARACTER_SET_ID
      AND co.RDB$COLLATION_ID = f.RDB$COLLATION_ID`

const sequenceQueryTemplate = `
SELECT %s, g.RDB$GENERATOR_ID, g.RDB$SYSTEM_FLAG
FROM RDB$GENERATORS g`

const indexQueryTemplate = `
SELECT %s, %s, i.RDB$INDEX_ID,
       i.RDB$UNIQUE_FLAG, i.RDB$DESCRIPTION, i.RDB$SEGMENT_COUNT,
       i.RDB$INDEX_INACTIVE, i.RDB$INDEX_TYPE, %s,
       i.RDB$SYSTEM_FLAG, i.RDB$EXPRESSION_SOURCE, i.RDB$STATISTICS,
       %s
FROM RDB$INDICES i
LEFT JOIN RDB$RELATION_CONSTRAINTS rc
       ON rc.RDB$INDEX_NAME = i.RDB$INDEX_NAME`

const indexSegmentsQueryTemplate = `
SELECT %s, %s, s.RDB$FIELD_POSITION,
       s.RDB$STATISTICS
FROM RDB$INDEX_SEGMENTS s
WHERE s.RDB$INDEX_NAME = ?
ORDER BY s.RDB$FIELD_POSITION`

const constraintQueryTemplate = `
SELECT %s, c.RDB$CONSTRAINT_TYPE,
       %s, c.RDB$DEFERRABLE,
       c.RDB$INITIALLY_DEFERRED, %s,
       %s, %s,
       r.RDB$MATCH_OPTION, r.RDB$UPDATE_RULE, r.RDB$DELETE_RULE,
       %s, t.RDB$TRIGGER_SOURCE,
       %s, %s
FROM RDB$RELATION_CONSTRAINTS c
JOIN RDB$RELATIONS cr
       ON cr.RDB$RELATION_NAME = c.RDB$RELATION_NAME
LEFT JOIN RDB$REF_CONSTRAINTS r
       ON r.RDB$CONSTRAINT_NAME = c.RDB$CONSTRAINT_NAME
LEFT JOIN RDB$CHECK_CONSTRAINTS k
       ON k.RDB$CONSTRAINT_NAME = c.RDB$CONSTRAINT_NAME
LEFT JOIN RDB$RELATION_CONSTRAINTS rc
       ON rc.RDB$CONSTRAINT_NAME = c.RDB$CONSTRAINT_NAME
      AND rc.RDB$CONSTRAINT_TYPE = 'CHECK'
LEFT JOIN RDB$TRIGGERS t
       ON t.RDB$TRIGGER_NAME = k.RDB$TRIGGER_NAME
      AND rc.RDB$CONSTRAINT_NAME IS NOT NULL
LEFT JOIN RDB$RELATION_CONSTRAINTS pc
       ON pc.RDB$CONSTRAINT_NAME = r.RDB$CONST_NAME_UQ`

const triggerQueryTemplate = `
SELECT %s, %s,
       t.RDB$TRIGGER_SEQUENCE, t.RDB$TRIGGER_TYPE,
       t.RDB$TRIGGER_SOURCE, t.RDB$DESCRIPTION,
       t.RDB$TRIGGER_INACTIVE, t.RDB$SYSTEM_FLAG, t.RDB$FLAGS
FROM RDB$TRIGGERS t`

const roleQueryTemplate = `
SELECT %s, RDB$OWNER_NAME
FROM RDB$ROLES`

const dependencyQueryTemplate = `
SELECT %s, d.RDB$DEPENDENT_TYPE,
       %s, %s,
       d.RDB$DEPENDED_ON_TYPE
FROM RDB$DEPENDENCIES d`

const functionQueryTemplate = `
SELECT %s, f.RDB$FUNCTION_TYPE,
       f.RDB$DESCRIPTION, f.RDB$MODULE_NAME, f.RDB$ENTRYPOINT,
       f.RDB$RETURN_ARGUMENT, f.RDB$SYSTEM_FLAG
FROM RDB$FUNCTIONS f`

const functionArgumentsQueryTemplate = `
SELECT %s, a.RDB$ARGUMENT_POSITION,
       a.RDB$MECHANISM, a.RDB$FIELD_LENGTH, a.RDB$FIELD_SCALE,
       a.RDB$FIELD_TYPE, a.RDB$FIELD_SUB_TYPE, a.RDB$CHARACTER_SET_ID,
       a.RDB$FIELD_PRECISION, a.RDB$CHARACTER_LENGTH
FROM RDB$FUNCTION_ARGUMENTS a
WHERE a.RDB$FUNCTION_NAME = ?
ORDER BY a.RDB$ARGUMENT_POSITION`

const databaseFilesQuery = `
SELECT f.RDB$FILE_NAME, f.RDB$FILE_SEQUENCE, f.RDB$FILE_START,
       f.RDB$FILE_LENGTH, f.RDB$SHADOW_NUMBER
FROM RDB$FILES f
WHERE f.RDB$SHADOW_NUMBER = 0
ORDER BY f.RDB$FILE_SEQUENCE`

const shadowsQuery = `
SELECT f.RDB$SHADOW_NUMBER, f.RDB$FILE_FLAGS
FROM RDB$FILES f
WHERE f.RDB$SHADOW_NUMBER > 0 AND f.RDB$FILE_SEQUENCE = 0
ORDER BY f.RDB$SHADOW_NUMBER`

const shadowFilesQuery = `
SELECT f.RDB$FILE_NAME, f.RDB$FILE_SEQUENCE, f.RDB$FILE_START,
       f.RDB$FILE_LENGTH, f.RDB$SHADOW_NUMBER
FROM RDB$FILES f
WHERE f.RDB$SHADOW_NUMBER = ?
ORDER BY f.RDB$FILE_SEQUENCE`

const privilegeQueryTemplate = `
SELECT p.RDB$USER, p.RDB$GRANTOR, p.RDB$PRIVILEGE,
       p.RDB$GRANT_OPTION, %s, %s,
       p.RDB$USER_TYPE, p.RDB$OBJECT_TYPE
FROM RDB$USER_PRIVILEGES p`

type extendedIdentifierSpec struct {
	relation string
	field    string
	ref      string
	alias    string
}

func (c *Catalog) extendedProjectionQuery(ctx context.Context, template string, identifiers ...extendedIdentifierSpec) (string, error) {
	widths, err := c.identifierWidths(ctx)
	if err != nil {
		return "", err
	}
	projections := make([]string, len(identifiers))
	for index, identifier := range identifiers {
		projection, err := identifierProjection(widths, identifier.relation, identifier.field, identifier.ref, identifier.alias)
		if err != nil {
			return "", err
		}
		projections[index] = projection
	}
	return fmt.Sprintf(template, stringsToAny(projections)...), nil
}

func stringsToAny(values []string) []any {
	result := make([]any, len(values))
	for index := range values {
		result[index] = values[index]
	}
	return result
}

// Domains returns user-defined domains matching name exactly. An empty name
// returns all user domains in catalog order.
func (c *Catalog) Domains(ctx context.Context, name string) ([]Domain, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	projection, err := c.domainQuery(ctx, extendedDomainQueryTemplate)
	if err != nil {
		return nil, err
	}
	query := projection + "\nWHERE COALESCE(f.RDB$SYSTEM_FLAG, 0) = 0\n  AND f.RDB$FIELD_NAME NOT STARTING WITH 'RDB$'"
	args := make([]any, 0, 1)
	if name != "" {
		query += "\n  AND f.RDB$FIELD_NAME = ?"
		args = append(args, name)
	}
	query += "\nORDER BY f.RDB$FIELD_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query domains: %w", err)
	}
	result := make([]Domain, 0)
	for rows.Next() {
		domain, scanErr := scanDomain(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("schema: scan domain: %w", closeRows(rows, scanErr))
		}
		if domain != nil {
			result = append(result, *domain)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate domains: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close domains: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Domain returns one user-defined domain or nil when it does not exist.
func (c *Catalog) Domain(ctx context.Context, name string) (*Domain, error) {
	if name == "" {
		return nil, errors.New("schema: domain name is required")
	}
	domains, err := c.Domains(ctx, name)
	if err != nil || len(domains) == 0 {
		return nil, err
	}
	return &domains[0], nil
}

// Sequences returns user generators/sequences matching name exactly. An empty
// name returns all user sequences in catalog order.
func (c *Catalog) Sequences(ctx context.Context, name string) ([]Sequence, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, sequenceQueryTemplate,
		extendedIdentifierSpec{"RDB$GENERATORS", "RDB$GENERATOR_NAME", "g.RDB$GENERATOR_NAME", "RDB$GENERATOR_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build sequence query: %w", err)
	}
	query += "\nWHERE COALESCE(g.RDB$SYSTEM_FLAG, 0) = 0"
	args := make([]any, 0, 1)
	if name != "" {
		query += "\n  AND g.RDB$GENERATOR_NAME = ?"
		args = append(args, name)
	}
	query += "\nORDER BY g.RDB$GENERATOR_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query sequences: %w", err)
	}
	result := make([]Sequence, 0)
	for rows.Next() {
		var raw struct {
			name       sql.NullString
			id         sql.NullInt64
			systemFlag sql.NullInt64
		}
		if err := rows.Scan(&raw.name, &raw.id, &raw.systemFlag); err != nil {
			return nil, fmt.Errorf("schema: scan sequence: %w", closeRows(rows, err))
		}
		objectName, err := requiredIdentifier(raw.name, "sequence name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan sequence: %w", closeRows(rows, err))
		}
		result = append(result, Sequence{Name: objectName, ID: raw.id, SystemFlag: raw.systemFlag})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate sequences: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close sequences: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Generators is an alias for Sequences.
func (c *Catalog) Generators(ctx context.Context, name string) ([]Sequence, error) {
	return c.Sequences(ctx, name)
}

// Sequence returns one user sequence or nil when it does not exist.
func (c *Catalog) Sequence(ctx context.Context, name string) (*Sequence, error) {
	if name == "" {
		return nil, errors.New("schema: sequence name is required")
	}
	sequences, err := c.Sequences(ctx, name)
	if err != nil || len(sequences) == 0 {
		return nil, err
	}
	return &sequences[0], nil
}

// Generator is an alias for Sequence.
func (c *Catalog) Generator(ctx context.Context, name string) (*Sequence, error) {
	return c.Sequence(ctx, name)
}

// Indexes returns user indexes matching name exactly. An empty name returns
// all user indexes in catalog order. Index segments are loaded sequentially.
func (c *Catalog) Indexes(ctx context.Context, name string) ([]Index, error) {
	return c.indexes(ctx, name, "")
}

// IndexesForRelation returns user indexes belonging to relationName. An empty
// relation name is rejected because it would otherwise hide an accidental
// unscoped catalog read.
func (c *Catalog) IndexesForRelation(ctx context.Context, relationName string) ([]Index, error) {
	if relationName == "" {
		return nil, errors.New("schema: index relation name is required")
	}
	return c.indexes(ctx, "", relationName)
}

func (c *Catalog) indexes(ctx context.Context, name, relationName string) ([]Index, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, indexQueryTemplate,
		extendedIdentifierSpec{"RDB$INDICES", "RDB$INDEX_NAME", "i.RDB$INDEX_NAME", "RDB$INDEX_NAME"},
		extendedIdentifierSpec{"RDB$INDICES", "RDB$RELATION_NAME", "i.RDB$RELATION_NAME", "RDB$RELATION_NAME"},
		extendedIdentifierSpec{"RDB$INDICES", "RDB$FOREIGN_KEY", "i.RDB$FOREIGN_KEY", "RDB$FOREIGN_KEY"},
		extendedIdentifierSpec{"RDB$RELATION_CONSTRAINTS", "RDB$CONSTRAINT_NAME", "rc.RDB$CONSTRAINT_NAME", "RDB$CONSTRAINT_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build index query: %w", err)
	}
	query += "\nWHERE COALESCE(i.RDB$SYSTEM_FLAG, 0) = 0"
	args := make([]any, 0, 2)
	if name != "" {
		query += "\n  AND i.RDB$INDEX_NAME = ?"
		args = append(args, name)
	}
	if relationName != "" {
		query += "\n  AND i.RDB$RELATION_NAME = ?"
		args = append(args, relationName)
	}
	query += "\nORDER BY i.RDB$INDEX_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query indexes: %w", err)
	}
	result := make([]Index, 0)
	for rows.Next() {
		index, scanErr := scanIndex(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("schema: scan index: %w", closeRows(rows, scanErr))
		}
		result = append(result, index)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate indexes: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close indexes: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	for index := range result {
		segments, err := c.IndexSegments(ctx, result[index].Name)
		if err != nil {
			return nil, fmt.Errorf("schema: load segments for index %q: %w", result[index].Name, err)
		}
		result[index].Segments = segments
	}
	return result, nil
}

// Index returns one user index or nil when it does not exist.
func (c *Catalog) Index(ctx context.Context, name string) (*Index, error) {
	if name == "" {
		return nil, errors.New("schema: index name is required")
	}
	indexes, err := c.Indexes(ctx, name)
	if err != nil || len(indexes) == 0 {
		return nil, err
	}
	return &indexes[0], nil
}

// IndexSegments returns ordered segments for indexName.
func (c *Catalog) IndexSegments(ctx context.Context, indexName string) ([]IndexSegment, error) {
	if indexName == "" {
		return nil, errors.New("schema: index name is required")
	}
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, indexSegmentsQueryTemplate,
		extendedIdentifierSpec{"RDB$INDEX_SEGMENTS", "RDB$INDEX_NAME", "s.RDB$INDEX_NAME", "RDB$INDEX_NAME"},
		extendedIdentifierSpec{"RDB$INDEX_SEGMENTS", "RDB$FIELD_NAME", "s.RDB$FIELD_NAME", "RDB$FIELD_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build index segment query for %q: %w", indexName, err)
	}
	rows, err := c.query(ctx, query, indexName)
	if err != nil {
		return nil, fmt.Errorf("schema: query index segments for %q: %w", indexName, err)
	}
	result := make([]IndexSegment, 0)
	for rows.Next() {
		var raw struct {
			indexName  sql.NullString
			fieldName  sql.NullString
			position   sql.NullInt64
			statistics sql.NullFloat64
		}
		if err := rows.Scan(&raw.indexName, &raw.fieldName, &raw.position, &raw.statistics); err != nil {
			return nil, fmt.Errorf("schema: scan index segment for %q: %w", indexName, closeRows(rows, err))
		}
		rowIndexName, err := requiredIdentifier(raw.indexName, "index segment index name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan index segment for %q: %w", indexName, closeRows(rows, err))
		}
		fieldName, err := requiredIdentifier(raw.fieldName, "index segment field name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan index segment for %q: %w", indexName, closeRows(rows, err))
		}
		result = append(result, IndexSegment{IndexName: rowIndexName, FieldName: fieldName, Position: raw.position, Statistics: raw.statistics})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate index segments for %q: %w", indexName, closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close index segments for %q: %w", indexName, err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Constraints returns constraints matching constraintName exactly. An empty
// name returns all user-relation constraints in catalog order.
func (c *Catalog) Constraints(ctx context.Context, constraintName string) ([]Constraint, error) {
	return c.constraints(ctx, constraintName, "")
}

// ConstraintsForRelation returns constraints belonging to relationName.
func (c *Catalog) ConstraintsForRelation(ctx context.Context, relationName string) ([]Constraint, error) {
	if relationName == "" {
		return nil, errors.New("schema: constraint relation name is required")
	}
	return c.constraints(ctx, "", relationName)
}

func (c *Catalog) constraints(ctx context.Context, constraintName, relationName string) ([]Constraint, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, constraintQueryTemplate,
		extendedIdentifierSpec{"RDB$RELATION_CONSTRAINTS", "RDB$CONSTRAINT_NAME", "c.RDB$CONSTRAINT_NAME", "RDB$CONSTRAINT_NAME"},
		extendedIdentifierSpec{"RDB$RELATION_CONSTRAINTS", "RDB$RELATION_NAME", "c.RDB$RELATION_NAME", "RDB$RELATION_NAME"},
		extendedIdentifierSpec{"RDB$RELATION_CONSTRAINTS", "RDB$INDEX_NAME", "c.RDB$INDEX_NAME", "RDB$INDEX_NAME"},
		extendedIdentifierSpec{"RDB$CHECK_CONSTRAINTS", "RDB$TRIGGER_NAME", "k.RDB$TRIGGER_NAME", "RDB$TRIGGER_NAME"},
		extendedIdentifierSpec{"RDB$REF_CONSTRAINTS", "RDB$CONST_NAME_UQ", "r.RDB$CONST_NAME_UQ", "RDB$CONST_NAME_UQ"},
		// The same check-trigger field is selected twice in the established layout.
		extendedIdentifierSpec{"RDB$CHECK_CONSTRAINTS", "RDB$TRIGGER_NAME", "k.RDB$TRIGGER_NAME", "RDB$TRIGGER_NAME"},
		extendedIdentifierSpec{"RDB$RELATION_CONSTRAINTS", "RDB$RELATION_NAME", "pc.RDB$RELATION_NAME", "RDB$RELATION_NAME"},
		extendedIdentifierSpec{"RDB$RELATION_CONSTRAINTS", "RDB$INDEX_NAME", "pc.RDB$INDEX_NAME", "RDB$INDEX_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build constraint query: %w", err)
	}
	query += "\nWHERE COALESCE(cr.RDB$SYSTEM_FLAG, 0) = 0"
	args := make([]any, 0, 2)
	if constraintName != "" {
		query += "\n  AND c.RDB$CONSTRAINT_NAME = ?"
		args = append(args, constraintName)
	}
	if relationName != "" {
		query += "\n  AND c.RDB$RELATION_NAME = ?"
		args = append(args, relationName)
	}
	query += "\nORDER BY c.RDB$CONSTRAINT_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query constraints: %w", err)
	}
	result := make([]Constraint, 0)
	type constraintKey struct {
		name     string
		relation string
	}
	constraintIndexes := make(map[constraintKey]int)
	for rows.Next() {
		constraint, scanErr := scanConstraint(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("schema: scan constraint: %w", closeRows(rows, scanErr))
		}
		key := constraintKey{name: constraint.Name, relation: constraint.RelationName}
		if existingIndex, ok := constraintIndexes[key]; ok &&
			strings.TrimSpace(constraint.ConstraintType) == string(ConstraintCheck) &&
			constraintsRepresentSameCheck(result[existingIndex], constraint) {
			mergeConstraint(&result[existingIndex], constraint)
			continue
		}
		constraintIndexes[key] = len(result)
		result = append(result, constraint)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate constraints: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close constraints: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	for index := range result {
		if result[index].IndexName.Valid && result[index].IndexName.String != "" {
			indexed, err := c.Index(ctx, result[index].IndexName.String)
			if err != nil {
				return nil, fmt.Errorf("schema: load index for constraint %q: %w", result[index].Name, err)
			}
			result[index].Index = indexed
			if indexed != nil {
				result[index].Columns = indexSegmentNames(indexed.Segments)
			}
		}
		if result[index].ReferencedIndexName.Valid && result[index].ReferencedIndexName.String != "" {
			referenced, err := c.Index(ctx, result[index].ReferencedIndexName.String)
			if err != nil {
				return nil, fmt.Errorf("schema: load referenced index for constraint %q: %w", result[index].Name, err)
			}
			result[index].ReferencedColumns = indexSegmentNamesFromIndex(referenced)
			result[index].PartnerConstraint = &Constraint{
				Name:         result[index].ReferencedConstraintName.String,
				RelationName: result[index].ReferencedRelationName,
				IndexName:    result[index].ReferencedIndexName,
				Columns:      append([]string(nil), result[index].ReferencedColumns...),
				Index:        referenced,
			}
		}
	}
	return result, nil
}

func mergeConstraint(dst *Constraint, src Constraint) {
	if !dst.CheckSource.Valid && src.CheckSource.Valid {
		dst.CheckSource = src.CheckSource
	}
	addTriggerName := func(name string) {
		name = strings.TrimRight(name, " ")
		if name == "" {
			return
		}
		for _, existing := range dst.TriggerNames {
			if existing == name {
				return
			}
		}
		dst.TriggerNames = append(dst.TriggerNames, name)
	}
	if dst.TriggerName.Valid {
		addTriggerName(dst.TriggerName.String)
	}
	if !dst.TriggerName.Valid && src.TriggerName.Valid {
		dst.TriggerName = src.TriggerName
	}
	if src.TriggerName.Valid {
		addTriggerName(src.TriggerName.String)
	}
	for _, triggerName := range src.TriggerNames {
		addTriggerName(triggerName)
	}
}

func constraintsRepresentSameCheck(first, second Constraint) bool {
	if strings.TrimSpace(first.ConstraintType) != string(ConstraintCheck) ||
		strings.TrimSpace(second.ConstraintType) != string(ConstraintCheck) {
		return false
	}
	if first.CheckSource.Valid && second.CheckSource.Valid {
		return strings.TrimSpace(first.CheckSource.String) == strings.TrimSpace(second.CheckSource.String)
	}
	return true
}

// Constraint returns one constraint or nil when it does not exist.
func (c *Catalog) Constraint(ctx context.Context, name string) (*Constraint, error) {
	if name == "" {
		return nil, errors.New("schema: constraint name is required")
	}
	constraints, err := c.Constraints(ctx, name)
	if err != nil || len(constraints) == 0 {
		return nil, err
	}
	return &constraints[0], nil
}

// Triggers returns user triggers matching name exactly. An empty name returns
// all user triggers in catalog order.
func (c *Catalog) Triggers(ctx context.Context, name string) ([]Trigger, error) {
	return c.triggers(ctx, name, "")
}

// TriggersForRelation returns user triggers scoped to relationName. Database
// triggers are not returned by this method.
func (c *Catalog) TriggersForRelation(ctx context.Context, relationName string) ([]Trigger, error) {
	if relationName == "" {
		return nil, errors.New("schema: trigger relation name is required")
	}
	return c.triggers(ctx, "", relationName)
}

func (c *Catalog) triggers(ctx context.Context, name, relationName string) ([]Trigger, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, triggerQueryTemplate,
		extendedIdentifierSpec{"RDB$TRIGGERS", "RDB$TRIGGER_NAME", "t.RDB$TRIGGER_NAME", "RDB$TRIGGER_NAME"},
		extendedIdentifierSpec{"RDB$TRIGGERS", "RDB$RELATION_NAME", "t.RDB$RELATION_NAME", "RDB$RELATION_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build trigger query: %w", err)
	}
	query += "\nWHERE COALESCE(t.RDB$SYSTEM_FLAG, 0) = 0"
	args := make([]any, 0, 2)
	if name != "" {
		query += "\n  AND t.RDB$TRIGGER_NAME = ?"
		args = append(args, name)
	}
	if relationName != "" {
		query += "\n  AND t.RDB$RELATION_NAME = ?"
		args = append(args, relationName)
	}
	query += "\nORDER BY t.RDB$TRIGGER_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query triggers: %w", err)
	}
	result := make([]Trigger, 0)
	for rows.Next() {
		trigger, scanErr := scanTrigger(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("schema: scan trigger: %w", closeRows(rows, scanErr))
		}
		result = append(result, trigger)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate triggers: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close triggers: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Trigger returns one user trigger or nil when it does not exist.
func (c *Catalog) Trigger(ctx context.Context, name string) (*Trigger, error) {
	if name == "" {
		return nil, errors.New("schema: trigger name is required")
	}
	triggers, err := c.Triggers(ctx, name)
	if err != nil || len(triggers) == 0 {
		return nil, err
	}
	return &triggers[0], nil
}

// Roles returns user roles matching name exactly. An empty name returns all
// user roles in catalog order.
func (c *Catalog) Roles(ctx context.Context, name string) ([]Role, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, roleQueryTemplate,
		extendedIdentifierSpec{"RDB$ROLES", "RDB$ROLE_NAME", "RDB$ROLE_NAME", "RDB$ROLE_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build role query: %w", err)
	}
	args := make([]any, 0, 1)
	if name != "" {
		query += "\nWHERE RDB$ROLE_NAME = ?"
		args = append(args, name)
	}
	query += "\nORDER BY RDB$ROLE_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query roles: %w", err)
	}
	result := make([]Role, 0)
	for rows.Next() {
		var raw struct {
			name  sql.NullString
			owner sql.NullString
		}
		if err := rows.Scan(&raw.name, &raw.owner); err != nil {
			return nil, fmt.Errorf("schema: scan role: %w", closeRows(rows, err))
		}
		roleName, err := requiredIdentifier(raw.name, "role name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan role: %w", closeRows(rows, err))
		}
		result = append(result, Role{Name: roleName, OwnerName: trimIdentifier(raw.owner)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate roles: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close roles: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Role returns one user role or nil when it does not exist.
func (c *Catalog) Role(ctx context.Context, name string) (*Role, error) {
	if name == "" {
		return nil, errors.New("schema: role name is required")
	}
	roles, err := c.Roles(ctx, name)
	if err != nil || len(roles) == 0 {
		return nil, err
	}
	return &roles[0], nil
}

// Dependencies returns dependency rows whose dependent name matches exactly.
// An empty name returns all rows in catalog order.
func (c *Catalog) Dependencies(ctx context.Context, dependentName string) ([]Dependency, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, dependencyQueryTemplate,
		extendedIdentifierSpec{"RDB$DEPENDENCIES", "RDB$DEPENDENT_NAME", "d.RDB$DEPENDENT_NAME", "RDB$DEPENDENT_NAME"},
		extendedIdentifierSpec{"RDB$DEPENDENCIES", "RDB$FIELD_NAME", "d.RDB$FIELD_NAME", "RDB$FIELD_NAME"},
		extendedIdentifierSpec{"RDB$DEPENDENCIES", "RDB$DEPENDED_ON_NAME", "d.RDB$DEPENDED_ON_NAME", "RDB$DEPENDED_ON_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build dependency query: %w", err)
	}
	args := make([]any, 0, 1)
	if dependentName != "" {
		query += "\nWHERE d.RDB$DEPENDENT_NAME = ?"
		args = append(args, dependentName)
	}
	query += "\nORDER BY d.RDB$DEPENDENT_NAME, d.RDB$DEPENDED_ON_NAME, d.RDB$FIELD_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query dependencies: %w", err)
	}
	result := make([]Dependency, 0)
	for rows.Next() {
		var raw struct {
			dependentName  sql.NullString
			dependentType  sql.NullInt64
			fieldName      sql.NullString
			dependedOnName sql.NullString
			dependedOnType sql.NullInt64
		}
		if err := rows.Scan(&raw.dependentName, &raw.dependentType, &raw.fieldName, &raw.dependedOnName, &raw.dependedOnType); err != nil {
			return nil, fmt.Errorf("schema: scan dependency: %w", closeRows(rows, err))
		}
		dependent, err := requiredIdentifier(raw.dependentName, "dependent name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan dependency: %w", closeRows(rows, err))
		}
		dependedOn, err := requiredIdentifier(raw.dependedOnName, "depended-on name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan dependency: %w", closeRows(rows, err))
		}
		result = append(result, Dependency{DependentName: dependent, DependentType: raw.dependentType, FieldName: trimIdentifier(raw.fieldName), DependedOnName: dependedOn, DependedOnType: raw.dependedOnType})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate dependencies: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close dependencies: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Functions returns user external functions matching name exactly. An empty
// name returns all user declarations and their argument metadata.
func (c *Catalog) Functions(ctx context.Context, name string) ([]Function, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, functionQueryTemplate,
		extendedIdentifierSpec{"RDB$FUNCTIONS", "RDB$FUNCTION_NAME", "f.RDB$FUNCTION_NAME", "RDB$FUNCTION_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build function query: %w", err)
	}
	query += "\nWHERE COALESCE(f.RDB$SYSTEM_FLAG, 0) = 0"
	args := make([]any, 0, 1)
	if name != "" {
		query += "\n  AND f.RDB$FUNCTION_NAME = ?"
		args = append(args, name)
	}
	query += "\nORDER BY f.RDB$FUNCTION_NAME"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query functions: %w", err)
	}
	result := make([]Function, 0)
	for rows.Next() {
		function, scanErr := scanFunction(rows)
		if scanErr != nil {
			return nil, fmt.Errorf("schema: scan function: %w", closeRows(rows, scanErr))
		}
		result = append(result, function)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate functions: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close functions: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	for index := range result {
		arguments, err := c.functionArguments(ctx, result[index].Name)
		if err != nil {
			return nil, fmt.Errorf("schema: load arguments for function %q: %w", result[index].Name, err)
		}
		result[index].Arguments = arguments
	}
	return result, nil
}

// Function returns one user external function or nil when it does not exist.
func (c *Catalog) Function(ctx context.Context, name string) (*Function, error) {
	if name == "" {
		return nil, errors.New("schema: function name is required")
	}
	functions, err := c.Functions(ctx, name)
	if err != nil || len(functions) == 0 {
		return nil, err
	}
	return &functions[0], nil
}

func (c *Catalog) functionArguments(ctx context.Context, functionName string) ([]FunctionArgument, error) {
	query, err := c.extendedProjectionQuery(ctx, functionArgumentsQueryTemplate,
		extendedIdentifierSpec{"RDB$FUNCTION_ARGUMENTS", "RDB$FUNCTION_NAME", "a.RDB$FUNCTION_NAME", "RDB$FUNCTION_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build function argument query for %q: %w", functionName, err)
	}
	rows, err := c.query(ctx, query, functionName)
	if err != nil {
		return nil, fmt.Errorf("schema: query function arguments for %q: %w", functionName, err)
	}
	result := make([]FunctionArgument, 0)
	for rows.Next() {
		var raw struct {
			functionName    sql.NullString
			position        sql.NullInt64
			mechanism       sql.NullInt64
			fieldLength     sql.NullInt64
			fieldScale      sql.NullInt64
			fieldType       sql.NullInt64
			fieldSubType    sql.NullInt64
			characterSetID  sql.NullInt64
			fieldPrecision  sql.NullInt64
			characterLength sql.NullInt64
		}
		if err := rows.Scan(&raw.functionName, &raw.position, &raw.mechanism, &raw.fieldLength, &raw.fieldScale, &raw.fieldType, &raw.fieldSubType, &raw.characterSetID, &raw.fieldPrecision, &raw.characterLength); err != nil {
			return nil, fmt.Errorf("schema: scan function argument for %q: %w", functionName, closeRows(rows, err))
		}
		rowFunctionName, err := requiredIdentifier(raw.functionName, "function argument function name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan function argument for %q: %w", functionName, closeRows(rows, err))
		}
		argumentName := rowFunctionName
		if raw.position.Valid {
			argumentName = fmt.Sprintf("%s_%d", rowFunctionName, raw.position.Int64)
		}
		result = append(result, FunctionArgument{Name: argumentName, FunctionName: rowFunctionName, Position: raw.position, Mechanism: raw.mechanism, FieldLength: raw.fieldLength, FieldScale: raw.fieldScale, FieldType: raw.fieldType, FieldSubType: raw.fieldSubType, CharacterSetID: raw.characterSetID, FieldPrecision: raw.fieldPrecision, CharacterLength: raw.characterLength})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate function arguments for %q: %w", functionName, closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close function arguments for %q: %w", functionName, err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// DatabaseFiles returns the current database's extension-file metadata. The
// current database is represented by shadow number zero.
func (c *Catalog) DatabaseFiles(ctx context.Context) ([]DatabaseFile, error) {
	return c.databaseFiles(ctx, databaseFilesQuery)
}

// Files is an alias for DatabaseFiles.
func (c *Catalog) Files(ctx context.Context) ([]DatabaseFile, error) {
	return c.DatabaseFiles(ctx)
}

func (c *Catalog) databaseFiles(ctx context.Context, query string, args ...any) ([]DatabaseFile, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query database files: %w", err)
	}
	result, err := scanDatabaseFiles(rows)
	if err != nil {
		return nil, err
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Shadows returns database shadows and their ordered file metadata.
func (c *Catalog) Shadows(ctx context.Context) ([]Shadow, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := c.query(ctx, shadowsQuery)
	if err != nil {
		return nil, fmt.Errorf("schema: query shadows: %w", err)
	}
	result := make([]Shadow, 0)
	for rows.Next() {
		var shadow Shadow
		if err := rows.Scan(&shadow.ID, &shadow.Flags); err != nil {
			return nil, fmt.Errorf("schema: scan shadow: %w", closeRows(rows, err))
		}
		result = append(result, shadow)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate shadows: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close shadows: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	for index := range result {
		files, err := c.databaseFiles(ctx, shadowFilesQuery, result[index].ID)
		if err != nil {
			return nil, fmt.Errorf("schema: load files for shadow %v: %w", result[index].ID, err)
		}
		result[index].Files = files
	}
	return result, nil
}

// Privileges returns grants whose grantee matches exactly. An empty grantee
// returns all privilege rows in catalog order.
func (c *Catalog) Privileges(ctx context.Context, grantee string) ([]Privilege, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	query, err := c.extendedProjectionQuery(ctx, privilegeQueryTemplate,
		extendedIdentifierSpec{"RDB$USER_PRIVILEGES", "RDB$RELATION_NAME", "p.RDB$RELATION_NAME", "RDB$RELATION_NAME"},
		extendedIdentifierSpec{"RDB$USER_PRIVILEGES", "RDB$FIELD_NAME", "p.RDB$FIELD_NAME", "RDB$FIELD_NAME"},
	)
	if err != nil {
		return nil, fmt.Errorf("schema: build privilege query: %w", err)
	}
	args := make([]any, 0, 1)
	if grantee != "" {
		query += "\nWHERE p.RDB$USER = ?"
		args = append(args, grantee)
	}
	query += "\nORDER BY p.RDB$USER, p.RDB$RELATION_NAME, p.RDB$FIELD_NAME, p.RDB$PRIVILEGE"
	rows, err := c.query(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("schema: query privileges: %w", err)
	}
	result := make([]Privilege, 0)
	for rows.Next() {
		var raw struct {
			grantee       sql.NullString
			grantor       sql.NullString
			privilegeCode sql.NullString
			grantOption   sql.NullInt64
			subjectName   sql.NullString
			fieldName     sql.NullString
			granteeType   sql.NullInt64
			subjectType   sql.NullInt64
		}
		if err := rows.Scan(&raw.grantee, &raw.grantor, &raw.privilegeCode, &raw.grantOption, &raw.subjectName, &raw.fieldName, &raw.granteeType, &raw.subjectType); err != nil {
			return nil, fmt.Errorf("schema: scan privilege: %w", closeRows(rows, err))
		}
		granteeName, err := requiredIdentifier(raw.grantee, "privilege grantee")
		if err != nil {
			return nil, fmt.Errorf("schema: scan privilege: %w", closeRows(rows, err))
		}
		grantorName, err := requiredIdentifier(raw.grantor, "privilege grantor")
		if err != nil {
			return nil, fmt.Errorf("schema: scan privilege: %w", closeRows(rows, err))
		}
		code, err := requiredIdentifier(raw.privilegeCode, "privilege code")
		if err != nil {
			return nil, fmt.Errorf("schema: scan privilege: %w", closeRows(rows, err))
		}
		subjectName, err := requiredIdentifier(raw.subjectName, "privilege subject name")
		if err != nil {
			return nil, fmt.Errorf("schema: scan privilege: %w", closeRows(rows, err))
		}
		result = append(result, Privilege{Grantee: granteeName, Grantor: grantorName, PrivilegeCode: code, GrantOption: raw.grantOption, SubjectName: subjectName, FieldName: trimIdentifier(raw.fieldName), GranteeType: raw.granteeType, SubjectType: raw.subjectType})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate privileges: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close privileges: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// Grants is an alias for Privileges.
func (c *Catalog) Grants(ctx context.Context, grantee string) ([]Privilege, error) {
	return c.Privileges(ctx, grantee)
}

func (c *Catalog) ready(ctx context.Context) error {
	if err := contextErr(ctx); err != nil {
		return err
	}
	if c == nil || c.queryer == nil {
		return ErrNilQueryer
	}
	return nil
}

func scanIndex(rows *sql.Rows) (Index, error) {
	var raw struct {
		name           sql.NullString
		relationName   sql.NullString
		id             sql.NullInt64
		uniqueFlag     sql.NullInt64
		description    sql.NullString
		segmentCount   sql.NullInt64
		inactive       sql.NullInt64
		indexType      sql.NullInt64
		foreignKey     sql.NullString
		systemFlag     sql.NullInt64
		expression     sql.NullString
		statistics     sql.NullFloat64
		constraintName sql.NullString
	}
	if err := rows.Scan(&raw.name, &raw.relationName, &raw.id, &raw.uniqueFlag, &raw.description, &raw.segmentCount, &raw.inactive, &raw.indexType, &raw.foreignKey, &raw.systemFlag, &raw.expression, &raw.statistics, &raw.constraintName); err != nil {
		return Index{}, err
	}
	name, err := requiredIdentifier(raw.name, "index name")
	if err != nil {
		return Index{}, err
	}
	relationName, err := requiredIdentifier(raw.relationName, "index relation name")
	if err != nil {
		return Index{}, err
	}
	return Index{Name: name, RelationName: relationName, ID: raw.id, UniqueFlag: raw.uniqueFlag, Description: raw.description, SegmentCount: raw.segmentCount, Inactive: raw.inactive, IndexType: raw.indexType, ForeignKey: trimIdentifier(raw.foreignKey), SystemFlag: raw.systemFlag, Expression: raw.expression, Statistics: raw.statistics, ConstraintName: trimIdentifier(raw.constraintName)}, nil
}

func scanConstraint(rows *sql.Rows) (Constraint, error) {
	var raw struct {
		name                   sql.NullString
		constraintType         sql.NullString
		relationName           sql.NullString
		deferrable             sql.NullString
		initiallyDeferred      sql.NullString
		indexName              sql.NullString
		triggerName            sql.NullString
		referencedConstraint   sql.NullString
		matchOption            sql.NullString
		updateRule             sql.NullString
		deleteRule             sql.NullString
		checkTriggerName       sql.NullString
		checkSource            sql.NullString
		referencedRelationName sql.NullString
		referencedIndexName    sql.NullString
	}
	if err := rows.Scan(&raw.name, &raw.constraintType, &raw.relationName, &raw.deferrable, &raw.initiallyDeferred, &raw.indexName, &raw.triggerName, &raw.referencedConstraint, &raw.matchOption, &raw.updateRule, &raw.deleteRule, &raw.checkTriggerName, &raw.checkSource, &raw.referencedRelationName, &raw.referencedIndexName); err != nil {
		return Constraint{}, err
	}
	name, err := requiredIdentifier(raw.name, "constraint name")
	if err != nil {
		return Constraint{}, err
	}
	constraintType, err := requiredIdentifier(raw.constraintType, "constraint type")
	if err != nil {
		return Constraint{}, err
	}
	relationName, err := requiredIdentifier(raw.relationName, "constraint relation name")
	if err != nil {
		return Constraint{}, err
	}
	constraint := Constraint{
		Name:                     name,
		ConstraintType:           constraintType,
		RelationName:             relationName,
		Deferrable:               trimIdentifier(raw.deferrable),
		InitiallyDeferred:        trimIdentifier(raw.initiallyDeferred),
		IndexName:                trimIdentifier(raw.indexName),
		TriggerName:              trimIdentifier(raw.triggerName),
		ReferencedConstraintName: trimIdentifier(raw.referencedConstraint),
		MatchOption:              trimIdentifier(raw.matchOption),
		UpdateRule:               trimIdentifier(raw.updateRule),
		DeleteRule:               trimIdentifier(raw.deleteRule),
		ReferencedRelationName:   "",
		ReferencedIndexName:      trimIdentifier(raw.referencedIndexName),
	}
	if raw.referencedRelationName.Valid {
		constraint.ReferencedRelationName = strings.TrimRight(raw.referencedRelationName.String, " ")
	}
	if strings.TrimSpace(constraintType) == string(ConstraintNotNull) {
		constraint.ColumnName = trimIdentifier(raw.triggerName)
		constraint.TriggerName = sql.NullString{}
	} else if strings.TrimSpace(constraintType) == string(ConstraintCheck) {
		constraint.CheckSource = raw.checkSource
		if raw.checkTriggerName.Valid {
			if triggerName := strings.TrimRight(raw.checkTriggerName.String, " "); triggerName != "" {
				constraint.TriggerNames = []string{triggerName}
			}
		}
	}
	return constraint, nil
}

func scanTrigger(rows *sql.Rows) (Trigger, error) {
	var raw Trigger
	var name sql.NullString
	if err := rows.Scan(&name, &raw.RelationName, &raw.Sequence, &raw.TriggerType, &raw.Source, &raw.Description, &raw.Inactive, &raw.SystemFlag, &raw.Flags); err != nil {
		return Trigger{}, err
	}
	triggerName, err := requiredIdentifier(name, "trigger name")
	if err != nil {
		return Trigger{}, err
	}
	raw.Name = triggerName
	raw.RelationName = trimIdentifier(raw.RelationName)
	return raw, nil
}

func scanFunction(rows *sql.Rows) (Function, error) {
	var raw Function
	var name sql.NullString
	if err := rows.Scan(&name, &raw.FunctionType, &raw.Description, &raw.ModuleName, &raw.EntryPoint, &raw.ReturnArgument, &raw.SystemFlag); err != nil {
		return Function{}, err
	}
	functionName, err := requiredIdentifier(name, "function name")
	if err != nil {
		return Function{}, err
	}
	raw.Name = functionName
	raw.ModuleName = trimIdentifier(raw.ModuleName)
	raw.EntryPoint = trimIdentifier(raw.EntryPoint)
	return raw, nil
}

func scanDatabaseFiles(rows *sql.Rows) ([]DatabaseFile, error) {
	result := make([]DatabaseFile, 0)
	for rows.Next() {
		var raw struct {
			fileName     sql.NullString
			sequence     sql.NullInt64
			start        sql.NullInt64
			length       sql.NullInt64
			shadowNumber sql.NullInt64
		}
		if err := rows.Scan(&raw.fileName, &raw.sequence, &raw.start, &raw.length, &raw.shadowNumber); err != nil {
			return nil, fmt.Errorf("schema: scan database file: %w", closeRows(rows, err))
		}
		name := ""
		if raw.sequence.Valid {
			name = fmt.Sprintf("FILE_%d", raw.sequence.Int64)
		}
		result = append(result, DatabaseFile{Name: name, FileName: trimIdentifier(raw.fileName), Sequence: raw.sequence, Start: raw.start, Length: raw.length, ShadowNumber: raw.shadowNumber})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate database files: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close database files: %w", err)
	}
	return result, nil
}

func indexSegmentNames(segments []IndexSegment) []string {
	names := make([]string, 0, len(segments))
	for _, segment := range segments {
		names = append(names, segment.FieldName)
	}
	return names
}

func indexSegmentNamesFromIndex(index *Index) []string {
	if index == nil {
		return nil
	}
	return indexSegmentNames(index.Segments)
}
