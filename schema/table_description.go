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
	if len(r.Columns) == 0 {
		return CatalogDescription{}, errors.New("schema: relation columns are unavailable")
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
	describedConstraints := make([]Constraint, 0, len(r.Constraints))
	for _, constraint := range r.Constraints {
		if strings.TrimSpace(constraint.ConstraintType) != string(ConstraintNotNull) {
			describedConstraints = append(describedConstraints, constraint)
		}
	}
	body.WriteString("-- Informational catalog description; not executable DDL.\n")
	body.WriteString("CREATE TABLE ")
	tableSpan := appendDescriptionName(&body, tableName)
	body.WriteString(" (\n")

	descriptionColumns := make([]DescriptionColumn, 0, len(r.Columns))
	for index, column := range r.Columns {
		body.WriteString("  ")
		span := appendDescriptionName(&body, quotedColumns[index])
		descriptionColumns = append(descriptionColumns, DescriptionColumn{
			Name: column.Name,
			Span: span,
		})
		if column.Domain == nil {
			body.WriteString(" /* type unknown: no resolved domain metadata */")
		} else if column.ComputedSource.Valid && strings.TrimSpace(column.ComputedSource.String) != "" {
			body.WriteString(" /* computed column declaration not reconstructed */")
		} else if isLegacyScaledDouble(*column.Domain) {
			body.WriteString(fmt.Sprintf(" NUMERIC(15, %d) /* normalized legacy scaled DOUBLE catalog type; %s */", -column.Domain.FieldScale.Int64, rawCatalogTypeMetadata(column.Domain)))
		} else {
			typeName, typeErr := column.Domain.SQLTypeWithOptions(options)
			if typeErr != nil {
				body.WriteString(" /* type unknown: ")
				body.WriteString(strings.ReplaceAll(strings.ReplaceAll(typeErr.Error(), "*/", "* /"), "\n", " "))
				body.WriteString("; ")
				body.WriteString(rawCatalogTypeMetadata(column.Domain))
				body.WriteString(" */")
			} else {
				body.WriteByte(' ')
				body.WriteString(typeName)
			}
		}
		defaultSource := column.DefaultSource
		if !defaultSource.Valid && column.Domain != nil {
			defaultSource = column.Domain.DefaultSource
		}
		if defaultSource.Valid {
			defaultText, defaultErr := defaultClause(sqlNullString{String: defaultSource.String, Valid: true})
			if defaultErr == nil {
				appendDDLClause(&body, defaultText)
			} else {
				appendDDLClause(&body, "/* default unknown: "+strings.ReplaceAll(strings.ReplaceAll(defaultErr.Error(), "*/", "* /"), "\n", " ")+" */")
			}
		}
		if column.Nullable.Valid && !column.Nullable.Bool {
			appendDDLClause(&body, "NOT NULL")
		}
		if index+1 < len(r.Columns) || len(describedConstraints) != 0 {
			body.WriteByte(',')
		}
		body.WriteByte('\n')
	}
	for index, constraint := range describedConstraints {
		definition, constraintErr := constraintDefinitionWithRenderer(constraint, renderer)
		if constraintErr != nil {
			definition = "/* constraint unavailable: " + strings.ReplaceAll(strings.ReplaceAll(constraintErr.Error(), "*/", "* /"), "\n", " ") + " */"
		}
		body.WriteString("  ")
		body.WriteString(definition)
		if index+1 < len(describedConstraints) {
			body.WriteByte(',')
		}
		body.WriteByte('\n')
	}
	if !r.ConstraintsLoaded {
		body.WriteString("  /* constraints unknown: complete metadata was not loaded */\n")
	}
	body.WriteByte(')')

	result := strings.TrimSuffix(body.String(), "\n")
	return CatalogDescription{
		Body:    result,
		Table:   tableSpan,
		Columns: descriptionColumns,
	}, nil
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

func appendCatalogComment(body *strings.Builder, text string) {
	text = strings.NewReplacer("\r", `\r`, "\n", `\n`).Replace(text)
	body.WriteString("-- ")
	body.WriteString(text)
	body.WriteByte('\n')
}

func appendDomainCatalogDescription(body *strings.Builder, domain *Domain) {
	if domain == nil {
		appendCatalogComment(body, "  Type label unavailable: column has no resolved domain metadata.")
		appendCatalogComment(body, "  Type provenance: catalog domain metadata was unavailable.")
		appendCatalogComment(body, "  Raw catalog metadata unavailable: no domain row was resolved.")
		return
	}

	label, normalized := domain.CatalogTypeLabel()
	switch {
	case normalized:
		appendCatalogComment(body, "  Type label: "+label+" (normalized legacy display; not recovered SQL)")
		appendCatalogComment(body, "  Type provenance: conventional numeric display normalized from legacy scaled DOUBLE catalog metadata.")
	case label != "":
		appendCatalogComment(body, "  Type label: "+label+" (recorded domain type metadata)")
		appendCatalogComment(body, "  Type provenance: rendered from recorded RDB$FIELDS domain metadata.")
	default:
		_, err := domain.SQLType()
		reason := "domain metadata does not define a supported SQL type"
		if err != nil {
			reason = err.Error()
		}
		appendCatalogComment(body, "  Type label unavailable: "+reason)
		appendCatalogComment(body, "  Type provenance: raw domain metadata retained; no SQL type label was inferred.")
	}

	appendCatalogComment(body, "  Raw catalog metadata: "+rawCatalogTypeMetadata(domain))
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
