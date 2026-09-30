package schema

import (
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ErrUnsupportedDDL reports that catalog metadata cannot be rendered without
// losing a semantic or textual part of the object definition.
var ErrUnsupportedDDL = errors.New("schema: unsupported DDL")

// DDLDialect identifies the InterBase SQL dialect used to render identifiers
// and dialect-sensitive catalog types.
type DDLDialect int

const (
	Dialect3 DDLDialect = 3
	Dialect1 DDLDialect = 1
)

// DDLOptions selects the SQL dialect for one rendering operation. A zero
// Dialect preserves the historical Dialect 3 default. SQL source catalog
// fields are preserved verbatim rather than translated; when rendering such
// source, callers must select the dialect compatible with the source database.
type DDLOptions struct {
	Dialect DDLDialect
}

type ddlRenderer struct{ dialect DDLDialect }

func newDDLRenderer(options DDLOptions) (ddlRenderer, error) {
	dialect := options.Dialect
	if dialect == 0 {
		dialect = Dialect3
	}
	if dialect != Dialect1 && dialect != Dialect3 {
		return ddlRenderer{}, fmt.Errorf("schema: unsupported SQL dialect %d", dialect)
	}
	return ddlRenderer{dialect: dialect}, nil
}

func (r ddlRenderer) identifier(name, label string) (string, error) {
	if r.dialect == Dialect3 {
		return quoteRequiredIdentifier(name, label)
	}
	if name == "" || len(name) > 67 || !asciiIdentifierStart(name[0]) || name != strings.ToUpper(name) || dialect1ReservedIdentifier(name) {
		return "", fmt.Errorf("schema: %s %q cannot be represented in SQL dialect 1", label, name)
	}
	for i := 1; i < len(name); i++ {
		if !asciiIdentifierPart(name[i]) {
			return "", fmt.Errorf("schema: %s %q cannot be represented in SQL dialect 1", label, name)
		}
	}
	return name, nil
}

func dialect1ReservedIdentifier(name string) bool {
	switch name {
	case "ADD", "ALL", "ALTER", "AND", "ANY", "AS", "ASC", "AVG", "BEGIN", "BETWEEN", "BIGINT", "BLOB", "BOOLEAN", "BY", "CASE", "CAST", "CHAR", "CHARACTER", "CHECK", "COLLATE", "COMMIT", "CONNECT", "CONSTRAINT", "COUNT", "CREATE", "CROSS", "CURRENT_DATE", "CURRENT_TIME", "CURRENT_TIMESTAMP", "DATABASE", "DATE", "DECIMAL", "DEFAULT", "DELETE", "DESC", "DISTINCT", "DOUBLE", "DROP", "ELSE", "END", "EXISTS", "EXTERNAL", "FETCH", "FILTER", "FLOAT", "FOR", "FOREIGN", "FROM", "FULL", "FUNCTION", "GRANT", "GROUP", "HAVING", "IN", "INDEX", "INNER", "INSERT", "INTEGER", "INTO", "IS", "JOIN", "LEFT", "LIKE", "LONG", "MAX", "MIN", "NOT", "NULL", "NUMERIC", "ON", "OR", "ORDER", "OUTER", "PLAN", "PRIMARY", "PROCEDURE", "REAL", "REFERENCES", "RETURNS", "REVOKE", "RIGHT", "ROLLBACK", "SELECT", "SET", "SMALLINT", "SUM", "TABLE", "THEN", "TIME", "TIMESTAMP", "TO", "TRIGGER", "UNION", "UNIQUE", "UPDATE", "USER", "USING", "VALUES", "VARCHAR", "VIEW", "WHEN", "WHERE", "WHILE", "WITH":
		return true
	default:
		return false
	}
}

func asciiIdentifierStart(c byte) bool { return c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' }
func asciiIdentifierPart(c byte) bool {
	return asciiIdentifierStart(c) || c >= '0' && c <= '9' || c == '_' || c == '$'
}

// UnsupportedDDLError identifies the object and metadata facet that prevents
// faithful DDL generation.
type UnsupportedDDLError struct {
	Object  string
	Name    string
	Feature string
}

func (e *UnsupportedDDLError) Error() string {
	if e == nil {
		return ErrUnsupportedDDL.Error()
	}
	if e.Feature == "" {
		return fmt.Sprintf("schema: unsupported DDL for %s %q", e.Object, e.Name)
	}
	return fmt.Sprintf("schema: unsupported DDL for %s %q: %s", e.Object, e.Name, e.Feature)
}

func (e *UnsupportedDDLError) Unwrap() error { return ErrUnsupportedDDL }

// DDLer is implemented by catalog objects for which an executable individual
// definition can be rendered without silently dropping metadata.
type DDLer interface {
	GenerateDDL() (string, error)
}

// Statementer is implemented by catalog objects whose executable definition
// consists of more than one SQL statement. GenerateDDL remains the single-
// string API; callers that execute generated SQL should prefer Statements when
// it is available so validation errors are not silently discarded.
type Statementer interface {
	Statements() ([]string, error)
}

func unsupportedDDL(object, name, feature string) error {
	return &UnsupportedDDLError{Object: object, Name: name, Feature: feature}
}

func quoteIdentifier(name string) (string, error) {
	if name == "" {
		return "", errors.New("schema: empty identifier")
	}
	if strings.IndexByte(name, 0) >= 0 {
		return "", errors.New("schema: identifier contains NUL")
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}

func quoteRequiredIdentifier(name, label string) (string, error) {
	quoted, err := quoteIdentifier(name)
	if err != nil {
		return "", fmt.Errorf("schema: %s: %w", label, err)
	}
	return quoted, nil
}

func appendDDLClause(builder *strings.Builder, clause string) {
	if clause == "" {
		return
	}
	builder.WriteByte(' ')
	builder.WriteString(clause)
}

func sourceClause(source sqlNullString, keyword string) (string, error) {
	if !source.Valid {
		return "", nil
	}
	if strings.TrimSpace(source.String) == "" {
		return "", errors.New("catalog source is empty")
	}
	if sourceStartsWithKeyword(source.String, keyword) {
		return source.String, nil
	}
	return keyword + " " + source.String, nil
}

func sourceStartsWithKeyword(source, keyword string) bool {
	trimmed := strings.TrimLeftFunc(source, unicode.IsSpace)
	position := 0
	for tokenIndex, token := range strings.Fields(keyword) {
		if tokenIndex != 0 {
			start := position
			for position < len(trimmed) {
				r, size := utf8.DecodeRuneInString(trimmed[position:])
				if !unicode.IsSpace(r) {
					break
				}
				position += size
			}
			if position == start {
				return false
			}
		}
		if len(trimmed)-position < len(token) || !strings.EqualFold(trimmed[position:position+len(token)], token) {
			return false
		}
		position += len(token)
	}
	remainder := trimmed[position:]
	if remainder == "" {
		return true
	}
	r, _ := utf8.DecodeRuneInString(remainder)
	return !unicode.IsLetter(r) && !unicode.IsDigit(r) && r != '_' && r != '$'
}

// sqlNullString is the small subset of sql.NullString needed by sourceClause.
// Keeping the helper independent of database/sql makes its source handling
// explicit at call sites while avoiding accidental value normalization.
type sqlNullString struct {
	String string
	Valid  bool
}

func defaultClause(source sqlNullString) (string, error) {
	return sourceClause(source, "DEFAULT")
}

func checkClause(source sqlNullString) (string, error) {
	if !source.Valid {
		return "", nil
	}
	if strings.TrimSpace(source.String) == "" {
		return "", errors.New("catalog check source is empty")
	}
	if sourceStartsWithKeyword(source.String, "CHECK") {
		return source.String, nil
	}
	return "CHECK (" + source.String + ")", nil
}

func renderNullableFlag(flag bool, valid bool) string {
	if valid && !flag {
		return "NOT NULL"
	}
	return ""
}

const (
	fieldTypeSmallint  = int64(7)
	fieldTypeInteger   = int64(8)
	fieldTypeFloat     = int64(10)
	fieldTypeDate      = int64(12)
	fieldTypeTime      = int64(13)
	fieldTypeChar      = int64(14)
	fieldTypeBigint    = int64(16)
	fieldTypeBoolean   = int64(17)
	fieldTypeDouble    = int64(27)
	fieldTypeTimestamp = int64(35)
	fieldTypeVarchar   = int64(37)
	fieldTypeCString   = int64(40)
	fieldTypeBlobID    = int64(45)
	fieldTypeBlob      = int64(261)
)

var blobSubtypeNames = map[int64]string{
	0: "BINARY",
	1: "TEXT",
	2: "BLR",
	3: "ACL",
	4: "RANGES",
	5: "SUMMARY",
	6: "FORMAT",
	7: "TRANSACTION_DESCRIPTION",
	8: "EXTERNAL_FILE_DESCRIPTION",
}

// SQLType renders the SQL data type represented by the RDB$FIELDS metadata.
// It returns an error when the catalog does not carry enough information for a
// faithful declaration, rather than guessing from an internal code.
func (d Domain) SQLType() (string, error) {
	return d.SQLTypeWithOptions(DDLOptions{})
}

// SQLTypeWithOptions renders a type for the requested SQL dialect.
func (d Domain) SQLTypeWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	parts, err := d.sqlTypePartsWithRenderer(renderer)
	if err != nil {
		return "", err
	}
	return parts.render(true), nil
}

type sqlTypeParts struct {
	base               string
	charset            string
	collation          string
	legacyScaledDouble bool
}

func (p sqlTypeParts) render(includeCollation bool) string {
	result := p.base
	if p.charset != "" {
		result += " CHARACTER SET " + p.charset
	}
	if includeCollation && p.collation != "" {
		result += " COLLATE " + p.collation
	}
	return result
}

func (d Domain) sqlTypeParts() (sqlTypeParts, error) {
	return d.sqlTypePartsWithRenderer(ddlRenderer{dialect: Dialect3})
}

func (d Domain) sqlTypePartsWithRenderer(renderer ddlRenderer) (sqlTypeParts, error) {
	if d.Dimensions.Valid && d.Dimensions.Int64 != 0 {
		return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "array bounds require RDB$FIELD_DIMENSIONS metadata")
	}
	if !d.FieldType.Valid {
		return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "field type is NULL")
	}

	fieldType := d.FieldType.Int64
	var result string
	var partsLegacy bool
	switch fieldType {
	case fieldTypeSmallint, fieldTypeInteger, fieldTypeBigint:
		if renderer.dialect == Dialect1 && fieldType == fieldTypeBigint {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "Dialect 1 has no faithful exact BIGINT storage declaration")
		}
		result = integerTypeName(fieldType)
		if d.FieldScale.Valid && d.FieldScale.Int64 > 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "positive numeric scale is invalid")
		}
		if d.FieldSubType.Valid && (d.FieldSubType.Int64 == 1 || d.FieldSubType.Int64 == 2) {
			if !d.FieldPrecision.Valid || d.FieldPrecision.Int64 <= 0 {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "numeric subtype has no precision")
			}
			if !d.FieldScale.Valid {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "numeric subtype has no scale")
			}
			name := "NUMERIC"
			if d.FieldSubType.Int64 == 2 {
				name = "DECIMAL"
			}
			if !validNumericDeclaration(d.FieldPrecision.Int64, d.FieldScale.Int64) {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "numeric precision or scale is out of range")
			}
			if renderer.dialect == Dialect1 && !dialect1NumericStorageCompatible(fieldType, d.FieldPrecision.Int64) {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "Dialect 1 numeric precision would use a different physical storage type than the recorded catalog field")
			}
			result = fmt.Sprintf("%s(%d, %d)", name, d.FieldPrecision.Int64, -d.FieldScale.Int64)
		} else if d.FieldSubType.Valid && d.FieldSubType.Int64 != 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, fmt.Sprintf("unknown numeric subtype %d", d.FieldSubType.Int64))
		} else if d.FieldScale.Valid && d.FieldScale.Int64 < 0 {
			precision := int64(9)
			switch fieldType {
			case fieldTypeSmallint:
				precision = 4
			case fieldTypeBigint:
				precision = 18
			}
			if !validNumericDeclaration(precision, d.FieldScale.Int64) {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "inferred numeric precision or scale is out of range")
			}
			result = fmt.Sprintf("NUMERIC(%d, %d)", precision, -d.FieldScale.Int64)
		}
	case fieldTypeFloat:
		result = "FLOAT"
	case fieldTypeDouble:
		if d.FieldScale.Valid && d.FieldScale.Int64 > 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "positive numeric scale is invalid")
		}
		if d.FieldSubType.Valid && (d.FieldSubType.Int64 == 1 || d.FieldSubType.Int64 == 2) {
			if !d.FieldPrecision.Valid || d.FieldPrecision.Int64 <= 0 {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "numeric subtype has no precision")
			}
			if !d.FieldScale.Valid {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "numeric subtype has no scale")
			}
			name := "NUMERIC"
			if d.FieldSubType.Int64 == 2 {
				name = "DECIMAL"
			}
			if !validNumericDeclaration(d.FieldPrecision.Int64, d.FieldScale.Int64) {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "numeric precision or scale is out of range")
			}
			if renderer.dialect == Dialect1 && !dialect1NumericStorageCompatible(fieldTypeDouble, d.FieldPrecision.Int64) {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "Dialect 1 numeric precision would use a different physical storage type than the recorded DOUBLE field")
			}
			result = fmt.Sprintf("%s(%d, %d)", name, d.FieldPrecision.Int64, -d.FieldScale.Int64)
		} else if d.FieldSubType.Valid && d.FieldSubType.Int64 != 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, fmt.Sprintf("unknown numeric subtype %d", d.FieldSubType.Int64))
		} else if d.FieldScale.Valid && d.FieldScale.Int64 < 0 && renderer.dialect == Dialect1 && (!d.FieldSubType.Valid || d.FieldSubType.Int64 == 0) && !d.FieldPrecision.Valid {
			if d.FieldScale.Int64 < -15 {
				return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "legacy scaled DOUBLE scale exceeds canonical NUMERIC(15) range")
			}
			result = fmt.Sprintf("NUMERIC(15, %d)", -d.FieldScale.Int64)
			partsLegacy = true
		} else if d.FieldScale.Valid && d.FieldScale.Int64 != 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "scaled DOUBLE metadata has no numeric subtype")
		} else if !d.FieldScale.Valid {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "DOUBLE scale is NULL")
		} else {
			result = "DOUBLE PRECISION"
		}
	case fieldTypeDate:
		if renderer.dialect == Dialect1 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "DATE has timestamp semantics in SQL dialect 1 and cannot preserve the catalog type")
		}
		result = "DATE"
	case fieldTypeTime:
		if renderer.dialect == Dialect1 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "TIME is not representable with SQL dialect 1 semantics")
		}
		result = "TIME"
	case fieldTypeTimestamp:
		result = "TIMESTAMP"
		if renderer.dialect == Dialect1 {
			result = "DATE"
		}
	case fieldTypeChar, fieldTypeVarchar:
		length := d.CharacterLength
		if !length.Valid {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "character length is NULL")
		}
		if !length.Valid || length.Int64 < 0 {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "character length is invalid")
		}
		name := "CHAR"
		if fieldType == fieldTypeVarchar {
			name = "VARCHAR"
		}
		result = fmt.Sprintf("%s(%d)", name, length.Int64)
	case fieldTypeBoolean:
		result = "BOOLEAN"
	case fieldTypeBlob:
		result = "BLOB"
		if d.FieldSubType.Valid {
			if subtype, ok := blobSubtypeNames[d.FieldSubType.Int64]; ok {
				result += " SUB_TYPE " + subtype
			} else {
				result += " SUB_TYPE " + strconv.FormatInt(d.FieldSubType.Int64, 10)
			}
		}
		if d.SegmentLength.Valid && d.SegmentLength.Int64 > 0 {
			result += fmt.Sprintf(" SEGMENT SIZE %d", d.SegmentLength.Int64)
		}
	case 9:
		return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "QUAD is an internal type")
	case fieldTypeCString:
		return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "CSTRING is only supported for external arguments")
	case fieldTypeBlobID:
		return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "BLOB_ID is an internal type")
	default:
		return sqlTypeParts{}, unsupportedDDL("domain", d.Name, fmt.Sprintf("unknown field type %d", fieldType))
	}

	parts := sqlTypeParts{base: result, legacyScaledDouble: partsLegacy}
	if fieldType == fieldTypeChar || fieldType == fieldTypeVarchar || fieldType == fieldTypeBlob {
		charset, err := characterSetClauseWithRenderer("domain", d.Name, d.CharacterSetName, d.CharacterSetID, renderer)
		if err != nil {
			return sqlTypeParts{}, err
		}
		parts.charset = charset
	}
	if (fieldType == fieldTypeChar || fieldType == fieldTypeVarchar || fieldType == fieldTypeBlob) && d.CollationID.Valid && d.CollationID.Int64 != 0 {
		if !d.CollationName.Valid || strings.TrimSpace(d.CollationName.String) == "" {
			return sqlTypeParts{}, unsupportedDDL("domain", d.Name, "collation name is unavailable")
		}
		collation, err := renderer.identifier(strings.TrimRight(d.CollationName.String, " "), "collation")
		if err != nil {
			return sqlTypeParts{}, err
		}
		parts.collation = collation
	}
	return parts, nil
}

func characterSetClause(object, name string, setName sql.NullString, setID sql.NullInt64) (string, error) {
	return characterSetClauseWithRenderer(object, name, setName, setID, ddlRenderer{dialect: Dialect3})
}

func characterSetClauseWithRenderer(object, name string, setName sql.NullString, setID sql.NullInt64, renderer ddlRenderer) (string, error) {
	if setName.Valid && strings.TrimSpace(setName.String) != "" {
		charset, err := renderer.identifier(strings.TrimRight(setName.String, " "), "character set")
		if err != nil {
			return "", err
		}
		return charset, nil
	}
	if setID.Valid && setID.Int64 != 0 {
		return "", unsupportedDDL(object, name, "character set name is unavailable")
	}
	return "", nil
}

func validNumericDeclaration(precision, scale int64) bool {
	if precision < 1 || precision > 18 || scale > 0 || scale < -precision {
		return false
	}
	return true
}

func dialect1NumericStorageCompatible(fieldType, precision int64) bool {
	switch fieldType {
	case fieldTypeSmallint:
		return precision <= 4
	case fieldTypeInteger:
		return precision >= 5 && precision <= 9
	case fieldTypeDouble:
		return precision >= 10 && precision <= 18
	default:
		return false
	}
}

// DataType is an alias for SQLType.
func (d Domain) DataType() (string, error) { return d.SQLType() }

func integerTypeName(fieldType int64) string {
	switch fieldType {
	case fieldTypeSmallint:
		return "SMALLINT"
	case fieldTypeInteger:
		return "INTEGER"
	default:
		return "BIGINT"
	}
}

// GenerateDDL returns an executable CREATE DOMAIN statement for a user domain.
func (d Domain) GenerateDDL() (string, error) {
	return d.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a domain using the requested SQL dialect.
func (d Domain) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	if d.Name == "" {
		return "", errors.New("schema: domain name is required")
	}
	if (d.SystemFlag.Valid && d.SystemFlag.Int64 != 0) || strings.HasPrefix(strings.ToUpper(d.Name), "RDB$") {
		return "", unsupportedDDL("domain", d.Name, "system domains are read-only")
	}
	if d.ComputedSource.Valid && strings.TrimSpace(d.ComputedSource.String) != "" {
		return "", unsupportedDDL("domain", d.Name, "computed field metadata is not a standalone domain")
	}
	name, err := renderer.identifier(d.Name, "domain name")
	if err != nil {
		return "", err
	}
	parts, err := d.sqlTypePartsWithRenderer(renderer)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	fmt.Fprintf(&builder, "CREATE DOMAIN %s AS %s", name, parts.render(false))
	if parts.legacyScaledDouble {
		builder.WriteString(" /* legacy scaled DOUBLE; canonical Dialect 1 NUMERIC(15, scale) */")
	}
	defaultText, err := defaultClause(sqlNullString{String: d.DefaultSource.String, Valid: d.DefaultSource.Valid})
	if err != nil {
		return "", unsupportedDDL("domain", d.Name, err.Error())
	}
	appendDDLClause(&builder, defaultText)
	if d.NullFlag.Valid && d.NullFlag.Int64 != 0 {
		appendDDLClause(&builder, "NOT NULL")
	}
	checkText, err := checkClause(sqlNullString{String: d.ValidationSource.String, Valid: d.ValidationSource.Valid})
	if err != nil {
		return "", unsupportedDDL("domain", d.Name, err.Error())
	}
	appendDDLClause(&builder, checkText)
	appendDDLClause(&builder, collationClause(parts.collation))
	return builder.String(), nil
}

func collationClause(collation string) string {
	if collation == "" {
		return ""
	}
	return "COLLATE " + collation
}

func userDomainReference(domain *Domain) bool {
	if domain == nil || domain.Name == "" || strings.HasPrefix(strings.ToUpper(domain.Name), "RDB$") {
		return false
	}
	return !domain.SystemFlag.Valid || domain.SystemFlag.Int64 == 0
}

func columnCollation(column Column, domain *Domain, renderer ddlRenderer) (string, error) {
	if !column.CollationID.Valid || column.CollationID.Int64 == 0 {
		return "", nil
	}
	if domain != nil && domain.CollationID.Valid && domain.CollationID.Int64 == column.CollationID.Int64 &&
		domain.CollationName.Valid && strings.TrimSpace(domain.CollationName.String) != "" {
		return "", nil
	}
	if !column.CollationName.Valid || strings.TrimSpace(column.CollationName.String) == "" {
		return "", unsupportedDDL("table", column.RelationName, fmt.Sprintf("column %q collation name is unavailable", column.Name))
	}
	collation, err := renderer.identifier(strings.TrimRight(column.CollationName.String, " "), "column collation")
	if err != nil {
		return "", err
	}
	return collationClause(collation), nil
}

func columnDefinition(column Column, notNullConstraint *Constraint, renderer ddlRenderer) (string, error) {
	name, err := renderer.identifier(column.Name, "column name")
	if err != nil {
		return "", err
	}
	var definition strings.Builder
	definition.WriteString(name)
	computed := column.ComputedSource.Valid && strings.TrimSpace(column.ComputedSource.String) != ""
	userDomain := userDomainReference(column.Domain) && column.FieldSource.Valid && strings.TrimRight(column.FieldSource.String, " ") != ""
	var domainParts sqlTypeParts
	if computed {
		return "", unsupportedDDL("table", column.RelationName, fmt.Sprintf("computed column %q has no faithful typed declaration in the catalog DDL grammar", column.Name))
	} else if userDomain {
		domainName, err := renderer.identifier(strings.TrimRight(column.Domain.Name, " "), "column domain")
		if err != nil {
			return "", err
		}
		appendDDLClause(&definition, domainName)
	} else {
		if column.Domain == nil {
			return "", unsupportedDDL("table", column.RelationName, fmt.Sprintf("column %q has no resolved domain", column.Name))
		}
		domainParts, err = column.Domain.sqlTypePartsWithRenderer(renderer)
		if err != nil {
			return "", err
		}
		appendDDLClause(&definition, domainParts.render(false))
		if domainParts.legacyScaledDouble {
			appendDDLClause(&definition, "/* legacy scaled DOUBLE; canonical Dialect 1 NUMERIC(15, scale) */")
		}
	}
	defaultText, err := defaultClause(sqlNullString{String: column.DefaultSource.String, Valid: column.DefaultSource.Valid})
	if err != nil {
		return "", unsupportedDDL("table", column.RelationName, fmt.Sprintf("column %q: %s", column.Name, err))
	}
	appendDDLClause(&definition, defaultText)
	if notNullConstraint != nil && notNullConstraint.Name != "" && !implicitNotNullConstraintName(notNullConstraint.Name) {
		constraintName, err := renderer.identifier(notNullConstraint.Name, "NOT NULL constraint name")
		if err != nil {
			return "", err
		}
		appendDDLClause(&definition, "CONSTRAINT "+constraintName+" NOT NULL")
	} else if notNullConstraint != nil {
		appendDDLClause(&definition, "NOT NULL")
	} else if column.Nullable.Valid {
		appendDDLClause(&definition, renderNullableFlag(column.Nullable.Bool, true))
	}
	if !computed && !userDomain && column.Domain != nil {
		checkText, err := checkClause(sqlNullString{String: column.Domain.ValidationSource.String, Valid: column.Domain.ValidationSource.Valid})
		if err != nil {
			return "", unsupportedDDL("table", column.RelationName, fmt.Sprintf("column %q: %s", column.Name, err))
		}
		appendDDLClause(&definition, checkText)
	}
	collation, err := columnCollation(column, column.Domain, renderer)
	if err != nil {
		return "", err
	}
	if collation == "" && !userDomain && !computed {
		collation = collationClause(domainParts.collation)
	}
	appendDDLClause(&definition, collation)
	return definition.String(), nil
}

func implicitNotNullConstraintName(name string) bool {
	// InterBase creates INTEG_* names for unnamed NOT NULL constraints, but
	// rejects those system-generated names when they are repeated in CREATE
	// TABLE. Preserve explicit names while rendering these as bare NOT NULL.
	return strings.HasPrefix(strings.ToUpper(strings.TrimSpace(name)), "INTEG_")
}

func constraintColumns(constraint Constraint) []string {
	if len(constraint.Columns) != 0 {
		return constraint.Columns
	}
	if constraint.Index != nil {
		return indexSegmentNames(constraint.Index.Segments)
	}
	return nil
}

func referencedConstraintColumns(constraint Constraint) []string {
	if len(constraint.ReferencedColumns) != 0 {
		return constraint.ReferencedColumns
	}
	if constraint.PartnerConstraint != nil {
		return constraintColumns(*constraint.PartnerConstraint)
	}
	return nil
}

func constraintDefinition(constraint Constraint) (string, error) {
	return constraintDefinitionWithRenderer(constraint, ddlRenderer{dialect: Dialect3})
}

func constraintDefinitionWithRenderer(constraint Constraint, renderer ddlRenderer) (string, error) {
	constraintType := strings.TrimSpace(constraint.ConstraintType)
	var builder strings.Builder
	if constraint.Name != "" {
		name, err := renderer.identifier(constraint.Name, "constraint name")
		if err != nil {
			return "", err
		}
		builder.WriteString("CONSTRAINT ")
		builder.WriteString(name)
		builder.WriteByte(' ')
	}
	switch constraintType {
	case string(ConstraintPrimaryKey), string(ConstraintUnique):
		columns := constraintColumns(constraint)
		if len(columns) == 0 {
			return "", unsupportedDDL("constraint", constraint.Name, "index segments are unavailable")
		}
		quoted, err := quoteIdentifiers(columns, "constraint column", renderer)
		if err != nil {
			return "", err
		}
		if constraintType == string(ConstraintPrimaryKey) {
			builder.WriteString("PRIMARY KEY (")
		} else {
			builder.WriteString("UNIQUE (")
		}
		builder.WriteString(strings.Join(quoted, ", "))
		builder.WriteByte(')')
	case string(ConstraintForeignKey):
		columns := constraintColumns(constraint)
		referencedColumns := referencedConstraintColumns(constraint)
		if len(columns) == 0 || len(referencedColumns) == 0 {
			return "", unsupportedDDL("constraint", constraint.Name, "foreign-key index segments are unavailable")
		}
		if constraint.ReferencedRelationName == "" {
			return "", unsupportedDDL("constraint", constraint.Name, "referenced relation is unavailable")
		}
		quotedColumns, err := quoteIdentifiers(columns, "foreign-key column", renderer)
		if err != nil {
			return "", err
		}
		quotedReferencedColumns, err := quoteIdentifiers(referencedColumns, "referenced column", renderer)
		if err != nil {
			return "", err
		}
		referencedRelation, err := renderer.identifier(constraint.ReferencedRelationName, "referenced relation")
		if err != nil {
			return "", err
		}
		builder.WriteString("FOREIGN KEY (")
		builder.WriteString(strings.Join(quotedColumns, ", "))
		builder.WriteString(") REFERENCES ")
		builder.WriteString(referencedRelation)
		builder.WriteString(" (")
		builder.WriteString(strings.Join(quotedReferencedColumns, ", "))
		builder.WriteByte(')')
		if rule := normalizedRule(constraint.DeleteRule); rule != "" && rule != "RESTRICT" {
			appendDDLClause(&builder, "ON DELETE "+rule)
		}
		if rule := normalizedRule(constraint.UpdateRule); rule != "" && rule != "RESTRICT" {
			appendDDLClause(&builder, "ON UPDATE "+rule)
		}
	case string(ConstraintCheck):
		checkText, err := checkClause(sqlNullString{String: constraint.CheckSource.String, Valid: constraint.CheckSource.Valid})
		if err != nil || checkText == "" {
			if err == nil {
				err = errors.New("check source is unavailable")
			}
			return "", unsupportedDDL("constraint", constraint.Name, err.Error())
		}
		builder.WriteString(checkText)
	case string(ConstraintNotNull):
		return "", unsupportedDDL("constraint", constraint.Name, "NOT NULL is rendered on its column")
	default:
		return "", unsupportedDDL("constraint", constraint.Name, fmt.Sprintf("constraint type %q", constraint.ConstraintType))
	}
	if constraint.Deferrable.Valid && !strings.EqualFold(strings.TrimSpace(constraint.Deferrable.String), "NO") {
		appendDDLClause(&builder, "DEFERRABLE")
	}
	if constraint.InitiallyDeferred.Valid && !strings.EqualFold(strings.TrimSpace(constraint.InitiallyDeferred.String), "NO") {
		appendDDLClause(&builder, "INITIALLY DEFERRED")
	}
	return builder.String(), nil
}

func normalizedRule(rule sql.NullString) string {
	if !rule.Valid {
		return ""
	}
	return strings.ToUpper(strings.TrimSpace(rule.String))
}

func quoteIdentifiers(names []string, label string, renderer ddlRenderer) ([]string, error) {
	result := make([]string, 0, len(names))
	for _, name := range names {
		quoted, err := renderer.identifier(name, label)
		if err != nil {
			return nil, err
		}
		result = append(result, quoted)
	}
	return result, nil
}

// GenerateDDL returns an executable CREATE TABLE statement. A complete table
// catalog read is required because omitting a discovered constraint would make
// the result misleading. Standalone indexes and triggers are separate typed
// objects and are generated by their own GenerateDDL methods.
func (r Relation) GenerateDDL() (string, error) {
	return r.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a relation using the requested dialect.
func (r Relation) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	name, err := renderer.identifier(r.Name, "relation name")
	if err != nil {
		return "", err
	}
	switch r.Kind {
	case RelationView:
		if !r.ViewSource.Valid || strings.TrimSpace(r.ViewSource.String) == "" {
			return "", unsupportedDDL("view", r.Name, "view source is unavailable")
		}
		columns := make([]string, 0, len(r.Columns))
		for _, column := range r.Columns {
			quoted, err := renderer.identifier(column.Name, "view column name")
			if err != nil {
				return "", err
			}
			columns = append(columns, quoted)
		}
		if len(columns) == 0 {
			return "", unsupportedDDL("view", r.Name, "view columns are unavailable")
		}
		return "CREATE VIEW " + name + " (" + strings.Join(columns, ", ") + ") AS " + r.ViewSource.String, nil
	case RelationTable:
		if !r.ConstraintsLoaded {
			return "", unsupportedDDL("table", r.Name, "complete constraint metadata was not loaded; use Catalog.Table")
		}
	default:
		return "", unsupportedDDL("relation", r.Name, fmt.Sprintf("relation kind %q", r.Kind))
	}

	prefix := "CREATE TABLE "
	relationType := strings.TrimSpace(r.RelationType.String)
	switch relationType {
	case "", "PERSISTENT", "EXTERNAL":
	case "GLOBAL_TEMPORARY_DELETE_ROWS", "GLOBAL_TEMPORARY_PRESERVE_ROWS":
		prefix = "CREATE GLOBAL TEMPORARY TABLE "
	default:
		if r.RelationType.Valid {
			return "", unsupportedDDL("table", r.Name, fmt.Sprintf("relation type %q", r.RelationType.String))
		}
	}
	isExternal := relationType == "EXTERNAL" || (r.ExternalFile.Valid && strings.TrimSpace(r.ExternalFile.String) != "")
	if isExternal {
		if prefix != "CREATE TABLE " || !r.ExternalFile.Valid || strings.TrimSpace(r.ExternalFile.String) == "" {
			return "", unsupportedDDL("table", r.Name, "external table filename is unavailable or conflicts with relation type")
		}
		for _, column := range r.Columns {
			if column.Domain == nil {
				return "", unsupportedDDL("table", r.Name, fmt.Sprintf("external column %q has no field metadata", column.Name))
			}
			domain := column.Domain
			if !domain.ExternalLength.Valid || !domain.ExternalScale.Valid || !domain.ExternalType.Valid ||
				domain.ExternalLength.Int64 != 0 || domain.ExternalScale.Int64 != 0 || domain.ExternalType.Int64 != 0 {
				return "", unsupportedDDL("table", r.Name, fmt.Sprintf("external column %q has incomplete or non-default external field metadata", column.Name))
			}
			if domain.Dimensions.Valid && domain.Dimensions.Int64 != 0 || domain.FieldType.Valid && domain.FieldType.Int64 == fieldTypeBlob {
				return "", unsupportedDDL("table", r.Name, fmt.Sprintf("external column %q uses an unsupported array or BLOB type", column.Name))
			}
		}
	}
	var builder strings.Builder
	builder.WriteString(prefix)
	builder.WriteString(name)
	if r.ExternalFile.Valid && strings.TrimSpace(r.ExternalFile.String) != "" {
		appendDDLClause(&builder, "EXTERNAL FILE "+quoteStringLiteral(r.ExternalFile.String))
	}
	builder.WriteString(" (\n  ")
	definitions := make([]string, 0, len(r.Columns)+len(r.Constraints))
	notNullConstraints := make(map[string]Constraint)
	for _, constraint := range r.Constraints {
		if strings.TrimSpace(constraint.ConstraintType) != string(ConstraintNotNull) {
			continue
		}
		if !constraint.ColumnName.Valid || strings.TrimRight(constraint.ColumnName.String, " ") == "" {
			return "", unsupportedDDL("table", r.Name, fmt.Sprintf("NOT NULL constraint %q has no column name", constraint.Name))
		}
		columnName := strings.TrimRight(constraint.ColumnName.String, " ")
		found := false
		for _, column := range r.Columns {
			if column.Name == columnName {
				found = true
				break
			}
		}
		if !found {
			return "", unsupportedDDL("table", r.Name, fmt.Sprintf("NOT NULL constraint %q references unavailable column %q", constraint.Name, columnName))
		}
		if _, exists := notNullConstraints[columnName]; exists {
			return "", unsupportedDDL("table", r.Name, fmt.Sprintf("multiple NOT NULL constraints reference column %q", columnName))
		}
		notNullConstraints[columnName] = constraint
	}
	for _, column := range r.Columns {
		notNullConstraint, hasNotNullConstraint := notNullConstraints[column.Name]
		var notNullConstraintPtr *Constraint
		if hasNotNullConstraint {
			notNullConstraintPtr = &notNullConstraint
		}
		definition, err := columnDefinition(column, notNullConstraintPtr, renderer)
		if err != nil {
			return "", err
		}
		definitions = append(definitions, definition)
	}
	for _, constraint := range r.Constraints {
		if strings.TrimSpace(constraint.ConstraintType) == string(ConstraintNotNull) {
			continue
		}
		definition, err := constraintDefinitionWithRenderer(constraint, renderer)
		if err != nil {
			return "", err
		}
		definitions = append(definitions, definition)
	}
	if len(definitions) == 0 {
		return "", unsupportedDDL("table", r.Name, "columns are unavailable")
	}
	builder.WriteString(strings.Join(definitions, ",\n  "))
	builder.WriteString("\n)")
	if relationType == "GLOBAL_TEMPORARY_PRESERVE_ROWS" {
		appendDDLClause(&builder, "ON COMMIT PRESERVE ROWS")
	}
	return builder.String(), nil
}

func quoteStringLiteral(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

func parameterDefinition(parameter ProcedureParameter, renderer ddlRenderer) (string, error) {
	name, err := renderer.identifier(parameter.Name, "parameter name")
	if err != nil {
		return "", err
	}
	if parameter.Domain == nil {
		return "", unsupportedDDL("procedure", parameter.ProcedureName, fmt.Sprintf("parameter %q has no resolved domain", parameter.Name))
	}
	if parameter.Direction != ParameterInput && parameter.Direction != ParameterOutput {
		return "", unsupportedDDL("procedure", parameter.ProcedureName, fmt.Sprintf("parameter %q has unknown direction %q", parameter.Name, parameter.Direction))
	}
	if !parameter.ParameterType.Valid || parameter.ParameterType.Int64 != 0 && parameter.ParameterType.Int64 != 1 {
		return "", unsupportedDDL("procedure", parameter.ProcedureName, fmt.Sprintf("parameter %q has unknown parameter type", parameter.Name))
	}
	if !parameter.FieldSource.Valid || strings.TrimRight(parameter.FieldSource.String, " ") == "" {
		return "", unsupportedDDL("procedure", parameter.ProcedureName, fmt.Sprintf("parameter %q field source is unavailable", parameter.Name))
	}
	if parameter.Domain.ComputedSource.Valid && strings.TrimSpace(parameter.Domain.ComputedSource.String) != "" {
		return "", unsupportedDDL("procedure", parameter.ProcedureName, fmt.Sprintf("parameter %q uses a computed domain", parameter.Name))
	}
	var typeName string
	if userDomainReference(parameter.Domain) && parameter.FieldSource.Valid && strings.TrimRight(parameter.FieldSource.String, " ") != "" {
		typeName, err = renderer.identifier(parameter.Domain.Name, "parameter domain")
	} else {
		parts, partsErr := parameter.Domain.sqlTypePartsWithRenderer(renderer)
		err = partsErr
		if err == nil {
			typeName = parts.render(true)
			if parts.legacyScaledDouble {
				typeName += " /* legacy scaled DOUBLE; canonical Dialect 1 NUMERIC(15, scale) */"
			}
		}
	}
	if err != nil {
		return "", err
	}
	result := name + " " + typeName
	if !parameter.Nullable.Valid {
		return "", unsupportedDDL("procedure", parameter.ProcedureName, fmt.Sprintf("parameter %q nullability is unknown", parameter.Name))
	}
	if !parameter.Nullable.Bool {
		result += " NOT NULL"
	}
	return result, nil
}

func validateProcedureParameters(procedure Procedure, parameters []ProcedureParameter, expectedCount sql.NullInt64, direction ParameterDirection) error {
	if !expectedCount.Valid || expectedCount.Int64 < 0 || expectedCount.Int64 != int64(len(parameters)) {
		return unsupportedDDL("procedure", procedure.Name, fmt.Sprintf("%s parameter count is NULL or does not match the catalog rows", direction))
	}
	for position, parameter := range parameters {
		if !parameter.Number.Valid || parameter.Number.Int64 != int64(position) {
			return unsupportedDDL("procedure", procedure.Name, fmt.Sprintf("%s parameter %d position is NULL or out of order", direction, position))
		}
		if parameter.Direction != direction {
			return unsupportedDDL("procedure", procedure.Name, fmt.Sprintf("parameter %q has direction %q, expected %q", parameter.Name, parameter.Direction, direction))
		}
		if parameter.ProcedureName != "" && parameter.ProcedureName != procedure.Name {
			return unsupportedDDL("procedure", procedure.Name, fmt.Sprintf("parameter %q belongs to procedure %q", parameter.Name, parameter.ProcedureName))
		}
	}
	return nil
}

// GenerateDDL returns an executable CREATE PROCEDURE statement with the
// catalog's exact PSQL source appended unchanged.
func (p Procedure) GenerateDDL() (string, error) {
	return p.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a procedure declaration for one dialect.
func (p Procedure) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	name, err := renderer.identifier(p.Name, "procedure name")
	if err != nil {
		return "", err
	}
	if !p.Source.Valid || strings.TrimSpace(p.Source.String) == "" {
		return "", unsupportedDDL("procedure", p.Name, "procedure source is unavailable")
	}
	if err := validateProcedureParameters(p, p.InputParameters, p.InputCount, ParameterInput); err != nil {
		return "", err
	}
	if err := validateProcedureParameters(p, p.OutputParameters, p.OutputCount, ParameterOutput); err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString("CREATE PROCEDURE ")
	builder.WriteString(name)
	if len(p.InputParameters) != 0 {
		parameters := make([]string, 0, len(p.InputParameters))
		for _, parameter := range p.InputParameters {
			definition, err := parameterDefinition(parameter, renderer)
			if err != nil {
				return "", err
			}
			parameters = append(parameters, definition)
		}
		builder.WriteString(" (")
		builder.WriteString(strings.Join(parameters, ", "))
		builder.WriteByte(')')
	}
	if len(p.OutputParameters) != 0 {
		parameters := make([]string, 0, len(p.OutputParameters))
		for _, parameter := range p.OutputParameters {
			definition, err := parameterDefinition(parameter, renderer)
			if err != nil {
				return "", err
			}
			parameters = append(parameters, definition)
		}
		builder.WriteString(" RETURNS (")
		builder.WriteString(strings.Join(parameters, ", "))
		builder.WriteByte(')')
	}
	builder.WriteString(" AS")
	if strings.TrimLeftFunc(p.Source.String, unicode.IsSpace) == p.Source.String {
		builder.WriteByte(' ')
	}
	builder.WriteString(p.Source.String)
	return builder.String(), nil
}

const (
	triggerTypeMask = int64(3 << 13)
	triggerTypeDB   = int64(1 << 13)
)

func triggerEvent(triggerType int64) (string, error) {
	const dmlMask = int64(0x7f)
	const knownMask = triggerTypeMask | dmlMask
	if triggerType < 0 || triggerType&^knownMask != 0 {
		return "", fmt.Errorf("trigger type %d contains unsupported bits", triggerType)
	}
	if triggerType&triggerTypeMask == triggerTypeDB {
		name, ok := map[int64]string{0: "CONNECT", 1: "DISCONNECT", 2: "TRANSACTION START", 3: "TRANSACTION COMMIT", 4: "TRANSACTION ROLLBACK"}[triggerType&^triggerTypeMask]
		if !ok {
			return "", fmt.Errorf("unknown database trigger type %d", triggerType)
		}
		return "ON " + name, nil
	}
	if triggerType&triggerTypeMask != 0 {
		return "", fmt.Errorf("unknown database trigger type %d", triggerType)
	}
	actionTime := (triggerType + 1) & 1
	prefix := "BEFORE"
	if actionTime == 1 {
		prefix = "AFTER"
	}
	suffixes := []string{"", "INSERT", "UPDATE", "DELETE"}
	parts := []string{prefix}
	zeroSeen := false
	for slot := int64(1); slot <= 3; slot++ {
		operation := ((triggerType + 1) >> (slot*2 - 1)) & 3
		if operation == 0 {
			if slot == 1 {
				return "", fmt.Errorf("DML trigger type %d has no operation", triggerType)
			}
			zeroSeen = true
			continue
		}
		if zeroSeen {
			return "", fmt.Errorf("DML trigger type %d has a non-contiguous operation list", triggerType)
		}
		parts = append(parts, suffixes[operation])
	}
	if len(parts) == 1 {
		return "", fmt.Errorf("DML trigger type %d has no operation", triggerType)
	}
	return prefix + " " + strings.Join(parts[1:], " OR "), nil
}

// Event returns the decoded trigger event, for example "BEFORE INSERT",
// "BEFORE INSERT OR UPDATE", or "ON CONNECT" for a database trigger. Unlike
// GenerateDDL it accepts a multi-event code: the event is real catalog
// metadata even where InterBase trigger DDL cannot express it. Callers that
// need the individual operations can split the result on " OR ".
func (t Trigger) Event() (string, error) {
	if !t.TriggerType.Valid {
		return "", unsupportedDDL("trigger", t.Name, "trigger type is NULL")
	}
	event, err := triggerEvent(t.TriggerType.Int64)
	if err != nil {
		return "", unsupportedDDL("trigger", t.Name, err.Error())
	}
	return event, nil
}

// GenerateDDL returns an executable CREATE TRIGGER statement with the exact
// catalog source preserved.
func (t Trigger) GenerateDDL() (string, error) {
	return t.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a trigger declaration for one dialect.
func (t Trigger) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	name, err := renderer.identifier(t.Name, "trigger name")
	if err != nil {
		return "", err
	}
	if !t.TriggerType.Valid {
		return "", unsupportedDDL("trigger", t.Name, "trigger type is NULL")
	}
	event, err := triggerEvent(t.TriggerType.Int64)
	if err != nil {
		return "", unsupportedDDL("trigger", t.Name, err.Error())
	}
	if strings.Contains(event, " OR ") {
		return "", unsupportedDDL("trigger", t.Name, "multiple DML events are not valid in InterBase trigger DDL")
	}
	if !t.Source.Valid || strings.TrimSpace(t.Source.String) == "" {
		return "", unsupportedDDL("trigger", t.Name, "trigger source is unavailable")
	}
	if !t.Sequence.Valid {
		return "", unsupportedDDL("trigger", t.Name, "trigger position is NULL")
	}
	active := "ACTIVE"
	if t.Inactive.Valid && t.Inactive.Int64 != 0 {
		active = "INACTIVE"
	}
	var builder strings.Builder
	builder.WriteString("CREATE TRIGGER ")
	builder.WriteString(name)
	if t.RelationName.Valid && strings.TrimRight(t.RelationName.String, " ") != "" {
		relation, err := renderer.identifier(strings.TrimRight(t.RelationName.String, " "), "trigger relation name")
		if err != nil {
			return "", err
		}
		builder.WriteString(" FOR ")
		builder.WriteString(relation)
	}
	builder.WriteByte(' ')
	builder.WriteString(active)
	builder.WriteByte(' ')
	builder.WriteString(event)
	fmt.Fprintf(&builder, " POSITION %d ", t.Sequence.Int64)
	builder.WriteString(t.Source.String)
	return builder.String(), nil
}

// GenerateDDL returns an executable CREATE INDEX statement. An inactive index
// is represented by a second ALTER statement because CREATE INDEX has no
// portable inactive clause.
func (i Index) GenerateDDL() (string, error) {
	return i.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates index DDL for one dialect.
func (i Index) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	statements, err := i.StatementsWithOptions(options)
	if err != nil {
		return "", err
	}
	return strings.Join(statements, ";\n"), nil
}

// Statements returns the independently executable statements that reproduce
// the index metadata. An inactive index requires CREATE followed by ALTER.
func (i Index) Statements() ([]string, error) {
	return i.StatementsWithOptions(DDLOptions{})
}

// StatementsWithOptions returns index statements for one dialect.
func (i Index) StatementsWithOptions(options DDLOptions) ([]string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return nil, err
	}
	name, err := renderer.identifier(i.Name, "index name")
	if err != nil {
		return nil, err
	}
	relation, err := renderer.identifier(i.RelationName, "index relation name")
	if err != nil {
		return nil, err
	}
	if (i.SystemFlag.Valid && i.SystemFlag.Int64 != 0) || strings.HasPrefix(strings.ToUpper(i.Name), "RDB$") {
		return nil, unsupportedDDL("index", i.Name, "system indexes are read-only")
	}
	if !i.UniqueFlag.Valid {
		return nil, unsupportedDDL("index", i.Name, "unique flag is NULL")
	}
	if i.UniqueFlag.Int64 != 0 && i.UniqueFlag.Int64 != 1 {
		return nil, unsupportedDDL("index", i.Name, fmt.Sprintf("unique flag code %d", i.UniqueFlag.Int64))
	}
	if !i.IndexType.Valid {
		return nil, unsupportedDDL("index", i.Name, "index direction is NULL")
	}
	direction := "ASCENDING"
	if i.IndexType.Int64 == 1 {
		direction = "DESCENDING"
	} else if i.IndexType.Int64 != 0 {
		return nil, unsupportedDDL("index", i.Name, fmt.Sprintf("index direction code %d", i.IndexType.Int64))
	}
	if i.Inactive.Valid && i.Inactive.Int64 != 0 && i.Inactive.Int64 != 1 {
		return nil, unsupportedDDL("index", i.Name, fmt.Sprintf("inactive flag code %d", i.Inactive.Int64))
	}
	if i.SegmentCount.Valid && i.SegmentCount.Int64 < 0 {
		return nil, unsupportedDDL("index", i.Name, "segment count is negative")
	}
	var builder strings.Builder
	builder.WriteString("CREATE ")
	if i.UniqueFlag.Int64 != 0 {
		builder.WriteString("UNIQUE ")
	}
	builder.WriteString(direction)
	builder.WriteString(" INDEX ")
	builder.WriteString(name)
	builder.WriteString(" ON ")
	builder.WriteString(relation)
	if i.Expression.Valid && strings.TrimSpace(i.Expression.String) != "" {
		expression, err := sourceClause(sqlNullString{String: i.Expression.String, Valid: true}, "COMPUTED BY")
		if err != nil {
			return nil, unsupportedDDL("index", i.Name, err.Error())
		}
		builder.WriteByte(' ')
		builder.WriteString(expression)
	} else {
		if len(i.Segments) == 0 {
			return nil, unsupportedDDL("index", i.Name, "index segments are unavailable")
		}
		if i.SegmentCount.Valid && i.SegmentCount.Int64 != int64(len(i.Segments)) {
			return nil, unsupportedDDL("index", i.Name, "segment count does not match catalog segments")
		}
		segments := make([]string, 0, len(i.Segments))
		for _, segment := range i.Segments {
			quoted, err := renderer.identifier(segment.FieldName, "index segment field name")
			if err != nil {
				return nil, err
			}
			segments = append(segments, quoted)
		}
		builder.WriteString(" (")
		builder.WriteString(strings.Join(segments, ", "))
		builder.WriteByte(')')
	}
	statements := []string{builder.String()}
	if i.Inactive.Valid && i.Inactive.Int64 != 0 {
		statements = append(statements, "ALTER INDEX "+name+" INACTIVE")
	}
	return statements, nil
}

// GenerateDDL returns an executable InterBase generator statement. InterBase
// exposes generators and sequences through the same catalog, but its SQL
// grammar accepts CREATE GENERATOR rather than the Firebird CREATE SEQUENCE
// spelling.
func (s Sequence) GenerateDDL() (string, error) {
	return s.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a generator declaration for one dialect.
func (s Sequence) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	name, err := renderer.identifier(s.Name, "sequence name")
	if err != nil {
		return "", err
	}
	if (s.SystemFlag.Valid && s.SystemFlag.Int64 != 0) || strings.HasPrefix(strings.ToUpper(s.Name), "RDB$") {
		return "", unsupportedDDL("sequence", s.Name, "system sequences are read-only")
	}
	return "CREATE GENERATOR " + name, nil
}

// GenerateDDL returns an executable CREATE ROLE statement.
func (r Role) GenerateDDL() (string, error) {
	return r.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a role declaration for one dialect.
func (r Role) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	name, err := renderer.identifier(r.Name, "role name")
	if err != nil {
		return "", err
	}
	if (r.SystemFlag.Valid && r.SystemFlag.Int64 != 0) || strings.HasPrefix(strings.ToUpper(r.Name), "RDB$") {
		return "", unsupportedDDL("role", r.Name, "system roles are read-only")
	}
	return "CREATE ROLE " + name, nil
}

func privilegeName(code string) (string, error) {
	switch strings.ToUpper(strings.TrimSpace(code)) {
	case "S":
		return "SELECT", nil
	case "I":
		return "INSERT", nil
	case "U":
		return "UPDATE", nil
	case "D":
		return "DELETE", nil
	case "R":
		return "REFERENCES", nil
	case "X":
		return "EXECUTE", nil
	case "M":
		return "", nil
	default:
		return "", fmt.Errorf("unknown privilege code %q", code)
	}
}

func privilegeObjectType(objectType sql.NullInt64, objectName, feature string, grantee bool) (string, error) {
	if !objectType.Valid {
		return "", unsupportedDDL("privilege", objectName, feature+" type is NULL")
	}
	switch objectType.Int64 {
	case 0:
		if grantee {
			return "", unsupportedDDL("privilege", objectName, "unknown grantee type 0")
		}
		return "", nil // relation/table
	case 1:
		return "VIEW", nil
	case 2:
		return "TRIGGER", nil
	case 5:
		return "PROCEDURE", nil
	case 8:
		if !grantee {
			return "", unsupportedDDL("privilege", objectName, "unknown subject type 8")
		}
		return "", nil // user
	case 13:
		if grantee {
			return "", nil // role
		}
		return "ROLE", nil
	default:
		return "", unsupportedDDL("privilege", objectName, fmt.Sprintf("unknown %s type %d", feature, objectType.Int64))
	}
}

// GenerateDDL returns an executable GRANT statement for a privilege row.
func (p Privilege) GenerateDDL() (string, error) {
	return p.GenerateDDLWithOptions(DDLOptions{})
}

// GenerateDDLWithOptions generates a GRANT statement for one dialect.
func (p Privilege) GenerateDDLWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	grantee, err := renderer.identifier(p.Grantee, "privilege grantee")
	if err != nil {
		return "", err
	}
	subject, err := renderer.identifier(p.SubjectName, "privilege subject")
	if err != nil {
		return "", err
	}
	code := strings.ToUpper(strings.TrimSpace(p.PrivilegeCode))
	privilege, err := privilegeName(code)
	if err != nil {
		return "", unsupportedDDL("privilege", p.SubjectName, err.Error())
	}
	granteeType, err := privilegeObjectType(p.GranteeType, p.Grantee, "grantee", true)
	if err != nil {
		return "", err
	}
	subjectType, err := privilegeObjectType(p.SubjectType, p.SubjectName, "subject", false)
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	if code == "M" {
		if subjectType != "ROLE" {
			return "", unsupportedDDL("privilege", p.SubjectName, "role membership has a non-role subject")
		}
		builder.WriteString("GRANT ")
		builder.WriteString(subject)
		builder.WriteString(" TO ")
		if granteeType != "" {
			builder.WriteString(granteeType)
			builder.WriteByte(' ')
		}
		builder.WriteString(grantee)
	} else {
		builder.WriteString("GRANT ")
		builder.WriteString(privilege)
		if p.FieldName.Valid && strings.TrimRight(p.FieldName.String, " ") != "" {
			fields, err := renderer.identifier(strings.TrimRight(p.FieldName.String, " "), "privilege field name")
			if err != nil {
				return "", err
			}
			builder.WriteString(" (")
			builder.WriteString(fields)
			builder.WriteByte(')')
		}
		builder.WriteString(" ON ")
		if subjectType != "" {
			builder.WriteString(subjectType)
			builder.WriteByte(' ')
		}
		builder.WriteString(subject)
		builder.WriteString(" TO ")
		if granteeType != "" {
			builder.WriteString(granteeType)
			builder.WriteByte(' ')
		}
		builder.WriteString(grantee)
	}
	if p.GrantOption.Valid && p.GrantOption.Int64 != 0 {
		if code == "M" {
			builder.WriteString(" WITH ADMIN OPTION")
		} else {
			builder.WriteString(" WITH GRANT OPTION")
		}
	}
	if p.Grantor != "" && !strings.EqualFold(strings.TrimSpace(p.Grantor), "SYSDBA") {
		grantor, err := renderer.identifier(p.Grantor, "privilege grantor")
		if err != nil {
			return "", err
		}
		builder.WriteString(" GRANTED BY ")
		builder.WriteString(grantor)
	}
	return builder.String(), nil
}

// SQLType renders the SQL declaration of an external function argument, for
// example "INTEGER" or "CSTRING(80)". Every type other than CSTRING is
// rendered by the same renderer that serves domains, so the two cannot drift.
// No character-set suffix is ever emitted: RDB$FUNCTION_ARGUMENTS supplies no
// character-set name, so an argument declared under a non-default character
// set returns an error wrapping ErrUnsupportedDDL rather than a declaration
// with the clause silently dropped.
func (a FunctionArgument) SQLType() (string, error) {
	return a.SQLTypeWithOptions(DDLOptions{})
}

// SQLTypeWithOptions renders an external function argument type for one
// dialect.
func (a FunctionArgument) SQLTypeWithOptions(options DDLOptions) (string, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return "", err
	}
	if a.FieldType.Valid && a.FieldType.Int64 == fieldTypeCString {
		// CSTRING declares a byte length, and RDB$CHARACTER_LENGTH is not
		// populated for function arguments; RDB$FIELD_LENGTH is used verbatim.
		if !a.FieldLength.Valid || a.FieldLength.Int64 <= 0 {
			return "", unsupportedDDL("function argument", a.Name, "CSTRING length is unavailable")
		}
		// The name is always invalid here: RDB$FUNCTION_ARGUMENTS supplies no
		// character-set name, so this call can only return "" or the refusal.
		if _, err := characterSetClause("function argument", a.Name, sql.NullString{}, a.CharacterSetID); err != nil {
			return "", err
		}
		return fmt.Sprintf("CSTRING(%d)", a.FieldLength.Int64), nil
	}

	domain := Domain{
		Name:            a.Name,
		FieldType:       a.FieldType,
		FieldSubType:    a.FieldSubType,
		FieldScale:      a.FieldScale,
		FieldLength:     a.FieldLength,
		FieldPrecision:  a.FieldPrecision,
		CharacterLength: a.CharacterLength,
		CharacterSetID:  a.CharacterSetID,
	}
	parts, err := domain.sqlTypePartsWithRenderer(renderer)
	if err != nil {
		var unsupported *UnsupportedDDLError
		if errors.As(err, &unsupported) {
			return "", unsupportedDDL("function argument", a.Name, unsupported.Feature)
		}
		return "", err
	}
	return parts.render(true), nil
}

// ReturnType renders the SQL declaration of the function's return value.
// RDB$RETURN_ARGUMENT is an argument position, not an index into Arguments:
// when it is N > 0 the return value is argument N, which is simultaneously an
// input. Catalog positions are unique per function, so the first match wins.
func (f Function) ReturnType() (string, error) {
	return f.ReturnTypeWithOptions(DDLOptions{})
}

// ReturnTypeWithOptions renders the return argument type for one dialect.
func (f Function) ReturnTypeWithOptions(options DDLOptions) (string, error) {
	if !f.ReturnArgument.Valid {
		return "", unsupportedDDL("external function", f.Name, "return argument position is NULL")
	}
	for _, argument := range f.Arguments {
		if argument.Position.Valid && argument.Position.Int64 == f.ReturnArgument.Int64 {
			return argument.SQLTypeWithOptions(options)
		}
	}
	return "", unsupportedDDL("external function", f.Name, fmt.Sprintf("no argument at return position %d", f.ReturnArgument.Int64))
}

// GenerateDDL reconstructs an external-function declaration only when the
// catalog facts completely identify its ordered inputs, return convention and
// library entry point. It never loads or invokes the external function.
func (f Function) GenerateDDL() (string, error) {
	unsupported := func(feature string) (string, error) {
		return "", unsupportedDDL("external function", f.Name, feature)
	}
	if !f.ReturnArgument.Valid || f.ReturnArgument.Int64 < 0 {
		return unsupported("return argument position is unavailable or invalid")
	}
	if len(f.Arguments) == 0 {
		return unsupported("function arguments are unavailable")
	}
	if !f.ModuleName.Valid || f.ModuleName.String == "" || strings.IndexByte(f.ModuleName.String, 0) >= 0 {
		return unsupported("module name is unavailable or invalid")
	}
	if !f.EntryPoint.Valid || f.EntryPoint.String == "" || strings.IndexByte(f.EntryPoint.String, 0) >= 0 {
		return unsupported("entry point is unavailable or invalid")
	}

	renderer, err := newDDLRenderer(DDLOptions{})
	if err != nil {
		return "", err
	}
	name, err := renderer.identifier(f.Name, "external function name")
	if err != nil {
		return unsupported(err.Error())
	}
	entryPoint, err := externalFunctionStringLiteral(f.EntryPoint.String, "entry point")
	if err != nil {
		return unsupported(err.Error())
	}
	moduleName, err := externalFunctionStringLiteral(f.ModuleName.String, "module name")
	if err != nil {
		return unsupported(err.Error())
	}

	arguments := make([]FunctionArgument, len(f.Arguments))
	copy(arguments, f.Arguments)
	for i, argument := range arguments {
		if !argument.Position.Valid || argument.Position.Int64 < 0 {
			return unsupported(fmt.Sprintf("argument %d position is unavailable or invalid", i+1))
		}
		if argument.FunctionName != f.Name {
			return unsupported(fmt.Sprintf("argument %d does not identify this function", i+1))
		}
		if !argument.Mechanism.Valid {
			return unsupported(fmt.Sprintf("argument at position %d mechanism is unavailable", argument.Position.Int64))
		}
		if i > 0 && arguments[i-1].Position.Int64 >= argument.Position.Int64 {
			return unsupported("argument positions are duplicated or out of order")
		}
	}

	returnPosition := f.ReturnArgument.Int64
	var returnArgument *FunctionArgument
	for i := range arguments {
		if arguments[i].Position.Int64 == returnPosition {
			returnArgument = &arguments[i]
			break
		}
	}
	if returnArgument == nil {
		return unsupported(fmt.Sprintf("no argument at return position %d", returnPosition))
	}
	if returnArgument.FieldType.Valid && returnArgument.FieldType.Int64 == fieldTypeCString {
		// The catalog does not preserve FREE_IT, so a CSTRING return cannot be
		// declared without guessing a memory-management convention.
		return unsupported("CSTRING return does not preserve the FREE_IT convention")
	}

	var inputArguments []FunctionArgument
	returnClause := ""
	if returnPosition == 0 {
		if arguments[0].Position.Int64 != 0 {
			return unsupported("arguments do not include the separate return value at position 0")
		}
		for i, argument := range arguments {
			if argument.Position.Int64 != int64(i) {
				return unsupported("argument positions are not contiguous from the return value")
			}
		}
		inputArguments = arguments[1:]
		returnType, err := returnArgument.SQLTypeWithOptions(DDLOptions{})
		if err != nil {
			return unsupported(err.Error())
		}
		returnClause = "RETURNS " + returnType
		mechanismClause, err := externalReturnMechanism(returnArgument.Mechanism.Int64)
		if err != nil {
			return unsupported(err.Error())
		}
		returnClause += mechanismClause
	} else {
		if arguments[0].Position.Int64 != 1 {
			return unsupported("parameter return has an ambiguous zero-based argument")
		}
		for i, argument := range arguments {
			if argument.Position.Int64 != int64(i+1) {
				return unsupported("parameter positions are not contiguous from 1")
			}
		}
		if returnPosition > int64(len(arguments)) {
			return unsupported(fmt.Sprintf("return parameter %d is outside the ordered input arguments", returnPosition))
		}
		inputArguments = arguments
		returnClause = fmt.Sprintf("RETURNS PARAMETER %d", returnPosition)
	}

	inputDeclarations := make([]string, 0, len(inputArguments))
	for _, argument := range inputArguments {
		typeName, err := argument.SQLTypeWithOptions(DDLOptions{})
		if err != nil {
			return unsupported(err.Error())
		}
		mechanism, err := externalInputMechanism(argument.Mechanism.Int64)
		if err != nil {
			return unsupported(fmt.Sprintf("argument at position %d: %s", argument.Position.Int64, err.Error()))
		}
		inputDeclarations = append(inputDeclarations, typeName+mechanism)
	}

	var builder strings.Builder
	builder.WriteString("DECLARE EXTERNAL FUNCTION ")
	builder.WriteString(name)
	if len(inputDeclarations) != 0 {
		builder.WriteByte(' ')
		builder.WriteString(strings.Join(inputDeclarations, ", "))
	}
	builder.WriteByte(' ')
	builder.WriteString(returnClause)
	builder.WriteString(" ENTRY_POINT ")
	builder.WriteString(entryPoint)
	builder.WriteString(" MODULE_NAME ")
	builder.WriteString(moduleName)
	return builder.String(), nil
}

func externalInputMechanism(mechanism int64) (string, error) {
	switch mechanism {
	case 1: // BY REFERENCE is the declaration default.
		return "", nil
	case 2:
		return " BY DESCRIPTOR", nil
	default:
		return "", fmt.Errorf("unsupported input mechanism %d", mechanism)
	}
}

func externalReturnMechanism(mechanism int64) (string, error) {
	switch mechanism {
	case 0:
		return " BY VALUE", nil
	case 1: // BY REFERENCE is the declaration default.
		return "", nil
	case 2:
		return " BY DESCRIPTOR", nil
	default:
		return "", fmt.Errorf("unsupported return mechanism %d", mechanism)
	}
}

func externalFunctionStringLiteral(value, label string) (string, error) {
	if value == "" {
		return "", fmt.Errorf("%s is empty", label)
	}
	if strings.IndexByte(value, 0) >= 0 {
		return "", fmt.Errorf("%s contains NUL", label)
	}
	return "'" + strings.ReplaceAll(value, "'", "''") + "'", nil
}

// GenerateDDL is intentionally unsupported for database extension files.
func (f DatabaseFile) GenerateDDL() (string, error) {
	return "", unsupportedDDL("database file", f.Name, "file placement is controlled by database creation")
}

// GenerateDDL is intentionally unsupported for shadows because creation and
// cleanup have server-wide filesystem effects outside this catalog package.
func (s Shadow) GenerateDDL() (string, error) {
	return "", unsupportedDDL("shadow", fmt.Sprint(s.ID.Int64), "shadow creation requires an explicit filesystem policy")
}
