package schema

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// CatalogDescription is a comment-only, non-executable description of a table
// assembled from its catalog metadata.
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
		(!d.FieldSubType.Valid || d.FieldSubType.Int64 == 0) && !d.FieldPrecision.Valid {
		return fmt.Sprintf("NUMERIC(15, %d)", -d.FieldScale.Int64), true
	}

	label, err := d.SQLType()
	if err != nil {
		return "", false
	}
	return label, false
}

// DescribeCatalog returns a comment-only description of this relation using
// its exact table and ordered column names. The result is informational and is
// not executable DDL.
func (r Relation) DescribeCatalog() (CatalogDescription, error) {
	tableName, err := quoteDescriptionIdentifier(r.Name, "relation name")
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

		quotedColumns[index], err = quoteDescriptionIdentifier(column.Name, "column name")
		if err != nil {
			return CatalogDescription{}, err
		}
	}

	var body strings.Builder
	appendCatalogComment(&body, "Informational catalog description; not executable DDL.")
	body.WriteString("-- Table: ")
	tableSpan := appendDescriptionName(&body, tableName)
	body.WriteByte('\n')

	descriptionColumns := make([]DescriptionColumn, 0, len(r.Columns))
	for index, column := range r.Columns {
		body.WriteString("-- Column: ")
		span := appendDescriptionName(&body, quotedColumns[index])
		body.WriteByte('\n')
		descriptionColumns = append(descriptionColumns, DescriptionColumn{
			Name: column.Name,
			Span: span,
		})
		appendDomainCatalogDescription(&body, column.Domain)
	}

	result := strings.TrimSuffix(body.String(), "\n")
	return CatalogDescription{
		Body:    result,
		Table:   tableSpan,
		Columns: descriptionColumns,
	}, nil
}

func quoteDescriptionIdentifier(name, label string) (string, error) {
	if strings.ContainsAny(name, "\r\n") {
		return "", fmt.Errorf("schema: %s contains a line break and cannot be represented in a comment-only description", label)
	}
	return quoteRequiredIdentifier(name, label)
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
