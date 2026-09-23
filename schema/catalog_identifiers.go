package schema

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

type identifierField struct {
	relation string
	field    string
}

const catalogIdentifierFieldWidthQuery = `
SELECT f.RDB$FIELD_LENGTH
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON f.RDB$FIELD_NAME = rf.RDB$FIELD_SOURCE
WHERE rf.RDB$RELATION_NAME = ?
  AND rf.RDB$FIELD_NAME = ?`

func catalogIdentifier(ref string, width int) (string, error) {
	if width <= 0 {
		return "", fmt.Errorf("schema: invalid catalog identifier width %d", width)
	}
	return fmt.Sprintf("CAST(%s AS VARCHAR(%d))", ref, width), nil
}

func cloneIdentifierWidths(widths map[identifierField]int) map[identifierField]int {
	clone := make(map[identifierField]int, len(widths))
	for field, width := range widths {
		clone[field] = width
	}
	return clone
}

func (c *Catalog) identifierWidths(ctx context.Context) (map[identifierField]int, error) {
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c == nil || c.queryer == nil {
		return nil, ErrNilQueryer
	}

	c.identifierWidthMu.Lock()
	defer c.identifierWidthMu.Unlock()
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if c.identifierWidthCache != nil {
		return cloneIdentifierWidths(c.identifierWidthCache), nil
	}

	relationNameWidth, err := c.identifierFieldWidth(ctx, "RDB$RELATION_FIELDS", "RDB$RELATION_NAME")
	if err != nil {
		return nil, err
	}
	fieldNameWidth, err := c.identifierFieldWidth(ctx, "RDB$RELATION_FIELDS", "RDB$FIELD_NAME")
	if err != nil {
		return nil, err
	}

	relationProjection, err := catalogIdentifier("rf.RDB$RELATION_NAME", relationNameWidth)
	if err != nil {
		return nil, err
	}
	fieldProjection, err := catalogIdentifier("rf.RDB$FIELD_NAME", fieldNameWidth)
	if err != nil {
		return nil, err
	}
	query := fmt.Sprintf(`
SELECT %s,
       %s,
       f.RDB$FIELD_LENGTH
FROM RDB$RELATION_FIELDS rf
JOIN RDB$FIELDS f ON rf.RDB$FIELD_SOURCE = f.RDB$FIELD_NAME`, relationProjection, fieldProjection)

	widths, err := c.readIdentifierWidths(ctx, query)
	if err != nil {
		return nil, err
	}
	c.identifierWidthCache = widths
	return cloneIdentifierWidths(c.identifierWidthCache), nil
}

func (c *Catalog) identifierFieldWidth(ctx context.Context, relation, field string) (int, error) {
	rows, err := c.query(ctx, catalogIdentifierFieldWidthQuery, relation, field)
	if err != nil {
		return 0, fmt.Errorf("schema: query catalog identifier width for %s.%s: %w", relation, field, err)
	}

	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return 0, fmt.Errorf("schema: iterate catalog identifier width for %s.%s: %w", relation, field, closeRows(rows, err))
		}
		if err := closeRows(rows, nil); err != nil {
			return 0, fmt.Errorf("schema: close catalog identifier width for %s.%s: %w", relation, field, err)
		}
		return 0, fmt.Errorf("schema: catalog identifier width for %s.%s is missing", relation, field)
	}

	var value sql.NullInt64
	if err := rows.Scan(&value); err != nil {
		return 0, fmt.Errorf("schema: scan catalog identifier width for %s.%s: %w", relation, field, closeRows(rows, err))
	}
	if rows.Next() {
		return 0, fmt.Errorf("schema: catalog identifier width for %s.%s: %w", relation, field, closeRows(rows, errors.New("multiple rows returned")))
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("schema: iterate catalog identifier width for %s.%s: %w", relation, field, closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return 0, fmt.Errorf("schema: close catalog identifier width for %s.%s: %w", relation, field, err)
	}
	if err := contextErr(ctx); err != nil {
		return 0, err
	}
	return checkedCatalogWidth(value, fmt.Sprintf("%s.%s", relation, field))
}

func (c *Catalog) readIdentifierWidths(ctx context.Context, query string) (map[identifierField]int, error) {
	rows, err := c.query(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("schema: query catalog identifier widths: %w", err)
	}
	widths := make(map[identifierField]int)
	for rows.Next() {
		var relationValue, fieldValue sql.NullString
		var widthValue sql.NullInt64
		if err := rows.Scan(&relationValue, &fieldValue, &widthValue); err != nil {
			return nil, fmt.Errorf("schema: scan catalog identifier widths: %w", closeRows(rows, err))
		}
		relation, err := requiredIdentifier(relationValue, "catalog identifier relation")
		if err != nil {
			return nil, fmt.Errorf("schema: scan catalog identifier widths: %w", closeRows(rows, err))
		}
		field, err := requiredIdentifier(fieldValue, "catalog identifier field")
		if err != nil {
			return nil, fmt.Errorf("schema: scan catalog identifier widths: %w", closeRows(rows, err))
		}
		width, err := checkedCatalogWidth(widthValue, relation+"."+field)
		if err != nil {
			return nil, closeRows(rows, err)
		}
		key := identifierField{relation: relation, field: field}
		if _, exists := widths[key]; exists {
			return nil, closeRows(rows, fmt.Errorf("schema: duplicate catalog identifier width for %s.%s", relation, field))
		}
		widths[key] = width
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("schema: iterate catalog identifier widths: %w", closeRows(rows, err))
	}
	if err := closeRows(rows, nil); err != nil {
		return nil, fmt.Errorf("schema: close catalog identifier widths: %w", err)
	}
	if err := contextErr(ctx); err != nil {
		return nil, err
	}
	if len(widths) == 0 {
		return nil, errors.New("schema: catalog identifier widths are missing")
	}
	return widths, nil
}

func checkedCatalogWidth(value sql.NullInt64, label string) (int, error) {
	if !value.Valid {
		return 0, fmt.Errorf("schema: catalog identifier width for %s is NULL", label)
	}
	if value.Int64 <= 0 {
		return 0, fmt.Errorf("schema: invalid catalog identifier width %d for %s", value.Int64, label)
	}
	if value.Int64 > int64(int(^uint(0)>>1)) {
		return 0, fmt.Errorf("schema: catalog identifier width %d for %s exceeds platform integer range", value.Int64, label)
	}
	return int(value.Int64), nil
}
