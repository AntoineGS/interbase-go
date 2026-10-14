package interbase

import (
	"context"
	"database/sql"
	"errors"

	"interbase-go/schema"
)

// ReadCatalogFingerprint reads a bounded, versioned definition fingerprint on
// a borrowed pooled attachment. No source/ACL BLOB content or native reference
// crosses the schema adapter. The underlying row reader owns and closes each
// READ COMMITTED/READ ONLY record-version transaction.
func ReadCatalogFingerprint(ctx context.Context, conn *sql.Conn, limit int64) (*schema.CatalogFingerprint, error) {
	return schema.ReadCatalogFingerprint(ctx, conn, limit, readSchemaFingerprintRows)
}
func readSchemaFingerprintRows(ctx context.Context, conn *sql.Conn, queries []schema.CatalogFingerprintQuery, visit schema.CatalogFingerprintVisitor) (schema.CatalogFingerprintStats, error) {
	native := make([]FingerprintQuery, len(queries))
	for i, q := range queries {
		native[i] = FingerprintQuery{SQL: q.SQL, Args: append([]any(nil), q.Args...)}
	}
	stats, err := ReadFingerprintRows(ctx, conn, native, func(q int, columns []string, cells []Cell) error {
		values := make([]any, len(cells))
		for i, c := range cells {
			if c.Indicator == 0xffff {
				continue
			}
			switch v := c.Value.(type) {
			case BlobIdentity:
				values[i] = schema.CatalogBlobIdentity{High: v.High, Low: v.Low}
			case []byte:
				values[i] = append([]byte(nil), v...)
			default:
				values[i] = v
			}
		}
		return visit(q, append([]string(nil), columns...), values)
	})
	if errors.Is(err, ErrNotInterBaseConn) {
		err = errors.Join(schema.ErrFingerprintCapability, err)
	}
	return schema.CatalogFingerprintStats{Queries: stats.Queries, Rows: stats.Rows, BlobOpens: stats.BlobOpens, SourceBytes: stats.SourceBytes}, err
}
