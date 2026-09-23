package schema

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CatalogDescription is a CREATE TABLE-shaped navigation description. It is
// informational and does not imply the complete catalog snapshot needed for
// executable DDL generation.
type CatalogDescription struct {
	Body    string
	Table   DescriptionSpan
	Columns []DescriptionColumn
}

// DescriptionSpan identifies a byte range in a CatalogDescription body.
type DescriptionSpan struct {
	Start int
	End   int
}

// DescriptionColumn identifies a column name and its byte range in a
// CatalogDescription body. Columns remain in catalog order.
type DescriptionColumn struct {
	Name string
	Span DescriptionSpan
}

// CatalogTypeLabel returns a display-only type label for catalog metadata.
// The normalized flag is true when the conventional legacy scaled DOUBLE
// rendering does not represent a recovered SQL declaration.
func (d Domain) CatalogTypeLabel() (label string, normalized bool) {
	if d.FieldType.Valid && d.FieldType.Int64 == fieldTypeDouble &&
		d.FieldScale.Valid && d.FieldScale.Int64 < 0 &&
		d.FieldScale.Int64 >= -15 &&
		(!d.FieldSubType.Valid || d.FieldSubType.Int64 == 0) && !d.FieldPrecision.Valid {
		return fmt.Sprintf("NUMERIC(15, %d)", -d.FieldScale.Int64), true
	}

	label, err := d.SQLType()
	if err != nil {
		return "", false
	}
	return label, false
}

// DescribeCatalog returns a SQL-shaped informational description using the
// historical Dialect 3 rendering default.
func (r Relation) DescribeCatalog() (CatalogDescription, error) {
	return r.DescribeCatalogWithOptions(DDLOptions{})
}

// DescribeCatalogWithOptions renders a SQL-shaped navigation description for
// the requested dialect. It does not weaken GenerateDDL's complete-catalog
// requirement.
func (r Relation) DescribeCatalogWithOptions(options DDLOptions) (CatalogDescription, error) {
	renderer, err := newDDLRenderer(options)
	if err != nil {
		return CatalogDescription{}, err
	}
	tableName, err := renderer.identifier(r.Name, "relation name")
	if err != nil {
		return CatalogDescription{}, err
	}
	quotedColumns := make([]string, len(r.Columns))
	seen := make(map[string]struct{}, len(r.Columns))
	for index, column := range r.Columns {
		if _, exists := seen[column.Name]; exists {
			return CatalogDescription{}, fmt.Errorf("schema: relation %q has duplicate column name %q", r.Name, column.Name)
		}
		seen[column.Name] = struct{}{}

		quotedColumns[index], err = renderer.identifier(column.Name, "column name")
		if err != nil {
			return CatalogDescription{}, err
		}
	}

	var body strings.Builder
	body.WriteString("-- Informational catalog description; not executable DDL.\n")
	if r.Kind == RelationView {
		body.WriteString("CREATE VIEW ")
		tableSpan := appendDescriptionName(&body, tableName)
		columns := make([]DescriptionColumn, 0, len(r.Columns))
		if len(quotedColumns) != 0 {
			body.WriteString(" (")
			for index, column := range quotedColumns {
				if index != 0 {
					body.WriteString(", ")
				}
				span := appendDescriptionName(&body, column)
				columns = append(columns, DescriptionColumn{Name: r.Columns[index].Name, Span: span})
			}
			body.WriteByte(')')
		}
		body.WriteString(" AS ")
		if r.ViewSource.Valid {
			body.WriteString(r.ViewSource.String)
		} else {
			body.WriteString("/* view source unknown */")
		}
		return CatalogDescription{Body: body.String(), Table: tableSpan, Columns: columns}, nil
	}
	if len(r.Columns) == 0 {
		return CatalogDescription{}, errors.New("schema: relation columns are unavailable")
	}
	prefix, suffix, externalFile, relationErr := descriptionRelationShape(r)
	if relationErr != nil {
		return CatalogDescription{}, relationErr
	}
	notNullConstraints := make(map[string]Constraint)
	otherConstraints := make([]Constraint, 0, len(r.Constraints))
	for _, constraint := range r.Constraints {
		if strings.TrimSpace(constraint.ConstraintType) == string(ConstraintNotNull) && constraint.ColumnName.Valid {
			notNullConstraints[strings.TrimRight(constraint.ColumnName.String, " ")] = constraint
		} else {
			otherConstraints = append(otherConstraints, constraint)
		}
	}
	body.WriteString(prefix)
	tableSpan := appendDescriptionName(&body, tableName)
	if externalFile != "" {
		appendDDLClause(&body, "EXTERNAL FILE "+quoteStringLiteral(externalFile))
	}
	body.WriteString(" (\n")

	descriptionColumns := make([]DescriptionColumn, 0, len(r.Columns))
	for index, column := range r.Columns {
		body.WriteString("  ")
		span := DescriptionSpan{Start: body.Len(), End: body.Len() + len(quotedColumns[index])}
		descriptionColumns = append(descriptionColumns, DescriptionColumn{Name: column.Name, Span: span})
		notNull, hasNotNull := notNullConstraints[column.Name]
		var notNullPtr *Constraint
		if hasNotNull {
			notNullPtr = &notNull
		}
		definition, definitionErr := columnDefinition(column, notNullPtr, renderer)
		if definitionErr != nil {
			definition = descriptionColumnFallback(column, notNullPtr, renderer, definitionErr)
		} else if userDomainReference(column.Domain) && column.Domain.ValidationSource.Valid {
			check, checkErr := checkClause(sqlNullString{String: column.Domain.ValidationSource.String, Valid: true})
			if checkErr == nil {
				collation, _ := columnCollation(column, column.Domain, renderer)
				if collation != "" && strings.HasSuffix(definition, " "+collation) {
					definition = strings.TrimSuffix(definition, " "+collation)
					definition = appendDDLClauseString(definition, check)
					definition = appendDDLClauseString(definition, collation)
				} else {
					definition = appendDDLClauseString(definition, check)
				}
			}
		}
		if !column.DefaultSource.Valid && column.Domain != nil && column.Domain.DefaultSource.Valid {
			defaultText, defaultErr := defaultClause(sqlNullString{String: column.Domain.DefaultSource.String, Valid: true})
			if defaultErr == nil {
				definition = appendDDLClauseString(definition, "/* inherited domain "+descriptionComment(defaultText)+" */")
			}
		}
		body.WriteString(definition)
		if index+1 < len(r.Columns) || len(otherConstraints) != 0 {
			body.WriteByte(',')
		}
		body.WriteByte('\n')
	}
	for index, constraint := range otherConstraints {
		definition, constraintErr := constraintDefinitionWithRenderer(constraint, renderer)
		if constraintErr != nil {
			definition = "/* constraint unavailable: " + strings.ReplaceAll(strings.ReplaceAll(constraintErr.Error(), "*/", "* /"), "\n", " ") + " */"
		}
		body.WriteString("  ")
		body.WriteString(definition)
		if index+1 < len(otherConstraints) {
			body.WriteByte(',')
		}
		body.WriteByte('\n')
	}
	if !r.ConstraintsLoaded {
		body.WriteString("  /* constraints unknown: complete metadata was not loaded */\n")
	}
	body.WriteByte(')')
	if suffix != "" {
		appendDDLClause(&body, suffix)
	}

	result := strings.TrimSuffix(body.String(), "\n")
	return CatalogDescription{
		Body:    result,
		Table:   tableSpan,
		Columns: descriptionColumns,
	}, nil
}

func descriptionRelationShape(relation Relation) (prefix, suffix, externalFile string, err error) {
	relationType := strings.TrimSpace(relation.RelationType.String)
	switch relationType {
	case "", "PERSISTENT":
		prefix = "CREATE TABLE "
	case "EXTERNAL":
		prefix = "CREATE TABLE "
	case "GLOBAL_TEMPORARY_DELETE_ROWS":
		prefix = "CREATE GLOBAL TEMPORARY TABLE "
	case "GLOBAL_TEMPORARY_PRESERVE_ROWS":
		prefix, suffix = "CREATE GLOBAL TEMPORARY TABLE ", "ON COMMIT PRESERVE ROWS"
	default:
		if relation.RelationType.Valid {
			return "", "", "", unsupportedDDL("table", relation.Name, "relation type "+fmt.Sprintf("%q is not represented by catalog description", relationType))
		}
	}
	if relationType == "EXTERNAL" || relation.ExternalFile.Valid && strings.TrimSpace(relation.ExternalFile.String) != "" {
		if relation.ExternalFile.Valid {
			externalFile = relation.ExternalFile.String
		} else {
			return "", "", "", unsupportedDDL("table", relation.Name, "external file name is unavailable")
		}
	}
	if prefix == "" {
		prefix = "CREATE TABLE "
	}
	return prefix, suffix, externalFile, nil
}

func descriptionColumnFallback(column Column, notNull *Constraint, renderer ddlRenderer, cause error) string {
	var definition strings.Builder
	name, err := renderer.identifier(column.Name, "column name")
	if err != nil {
		name = "<unknown column>"
	}
	definition.WriteString(name)
	if column.Domain != nil {
		if isLegacyScaledDouble(*column.Domain) {
			fmt.Fprintf(&definition, " NUMERIC(15, %d) /* normalized legacy scaled DOUBLE catalog type; %s */", -column.Domain.FieldScale.Int64, rawCatalogTypeMetadata(column.Domain))
		} else if userDomainReference(column.Domain) && column.FieldSource.Valid && strings.TrimSpace(column.FieldSource.String) != "" {
			domainName, nameErr := renderer.identifier(strings.TrimRight(column.Domain.Name, " "), "column domain")
			if nameErr == nil {
				appendDDLClause(&definition, domainName)
			} else {
				definition.WriteString(" /* type unknown: " + descriptionComment(nameErr.Error()) + " */")
			}
		} else if label, typeErr := column.Domain.SQLTypeWithOptions(DDLOptions{Dialect: renderer.dialect}); typeErr == nil {
			appendDDLClause(&definition, label)
		} else {
			definition.WriteString(" /* type unknown: " + descriptionComment(typeErr.Error()) + "; " + rawCatalogTypeMetadata(column.Domain) + " */")
		}
	} else {
		definition.WriteString(" /* type unknown: no resolved domain metadata */")
	}
	if column.DefaultSource.Valid {
		if value, defaultErr := defaultClause(sqlNullString{String: column.DefaultSource.String, Valid: true}); defaultErr == nil {
			appendDDLClause(&definition, value)
		} else {
			definition.WriteString(" /* default unknown: " + descriptionComment(defaultErr.Error()) + " */")
		}
	}
	if notNull != nil {
		if notNull.Name != "" {
			if constraintName, nameErr := renderer.identifier(notNull.Name, "NOT NULL constraint name"); nameErr == nil {
				appendDDLClause(&definition, "CONSTRAINT "+constraintName+" NOT NULL")
			} else {
				appendDDLClause(&definition, "NOT NULL /* name unknown: "+descriptionComment(nameErr.Error())+" */")
			}
		} else {
			appendDDLClause(&definition, "NOT NULL")
		}
	} else if column.Nullable.Valid && !column.Nullable.Bool {
		appendDDLClause(&definition, "NOT NULL")
	}
	if column.Domain != nil && column.Domain.ValidationSource.Valid {
		if check, checkErr := checkClause(sqlNullString{String: column.Domain.ValidationSource.String, Valid: true}); checkErr == nil {
			appendDDLClause(&definition, check)
		}
	}
	if collation, collationErr := columnCollation(column, column.Domain, renderer); collationErr == nil {
		appendDDLClause(&definition, collation)
	} else {
		appendDDLClause(&definition, "/* collation unknown: "+descriptionComment(collationErr.Error())+" */")
	}
	definition.WriteString(" /* declaration incomplete: " + descriptionComment(cause.Error()) + " */")
	return definition.String()
}

func appendDDLClauseString(builder, clause string) string {
	if clause != "" {
		return builder + " " + clause
	}
	return builder
}

func descriptionComment(text string) string {
	text = strings.NewReplacer("*/", "* /", "\r", " ", "\n", " ").Replace(text)
	return text
}

func isLegacyScaledDouble(domain Domain) bool {
	return domain.FieldType.Valid && domain.FieldType.Int64 == fieldTypeDouble && domain.FieldScale.Valid && domain.FieldScale.Int64 < 0 && domain.FieldScale.Int64 >= -15 && (!domain.FieldSubType.Valid || domain.FieldSubType.Int64 == 0) && !domain.FieldPrecision.Valid
}

func appendDescriptionName(body *strings.Builder, name string) DescriptionSpan {
	span := DescriptionSpan{Start: body.Len()}
	body.WriteString(name)
	span.End = body.Len()
	return span
}

func rawCatalogTypeMetadata(domain *Domain) string {
	return "type=" + catalogIntValue(domain.FieldType) +
		", scale=" + catalogIntValue(domain.FieldScale) +
		", subtype=" + catalogIntValue(domain.FieldSubType) +
		", precision=" + catalogIntValue(domain.FieldPrecision)
}

func catalogIntValue(value sql.NullInt64) string {
	if !value.Valid {
		return "NULL"
	}
	return fmt.Sprintf("%d", value.Int64)
}
