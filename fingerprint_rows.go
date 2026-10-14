package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// FingerprintQuery is a trusted catalog SELECT and its positional arguments.
// Read-only classification does not sandbox external UDF side effects.
type FingerprintQuery struct {
	SQL  string
	Args []any
}

// FingerprintReadStats counts executed queries, visited rows, and actual native
// BLOB opens and source bytes read (before charset conversion).
type FingerprintReadStats struct {
	Queries, Rows, BlobOpens, SourceBytes int64
}

// FingerprintVisitor receives a Go-owned snapshot bounded to one row. It must
// be I/O-free and must not recursively use the borrowed connection: Raw and the
// driver's connection lock remain held during the callback.
type FingerprintVisitor func(query int, columns []string, cells []Cell) error

// FingerprintRowReader is an optional capability of a borrowed driver connection.
// The receiver must not be retained beyond its sql.Conn.Raw callback.
type FingerprintRowReader interface {
	ReadFingerprintRows(context.Context, []FingerprintQuery, FingerprintVisitor) (FingerprintReadStats, error)
}

// ReadFingerprintRows uses the pooled attachment with an owned, short-lived
// READ COMMITTED/READ ONLY, record-version transaction. Caller transactions are
// rejected, never completed. BLOB cells contain copied BlobIdentity values;
// arrays are rejected before value access. Partial statistics survive errors.
func ReadFingerprintRows(ctx context.Context, pooled *sql.Conn, queries []FingerprintQuery, visit FingerprintVisitor) (stats FingerprintReadStats, err error) {
	if pooled == nil {
		return stats, ErrNotInterBaseConn
	}
	err = pooled.Raw(func(raw any) error {
		reader, ok := raw.(FingerprintRowReader)
		if !ok {
			return ErrNotInterBaseConn
		}
		stats, err = reader.ReadFingerprintRows(ctx, queries, visit)
		return err
	})
	return stats, err
}

func (c *conn) ReadFingerprintRows(ctx context.Context, queries []FingerprintQuery, visit FingerprintVisitor) (stats FingerprintReadStats, err error) {
	if err = contextError(ctx); err != nil {
		return stats, err
	}
	if visit == nil {
		return stats, errors.New("interbase: nil fingerprint visitor")
	}
	c.lockDirect()
	defer c.mu.Unlock()
	if c.distributed != nil {
		return stats, ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return stats, driver.ErrBadConn
	}
	release, err := enterNativeContext(ctx)
	if err != nil {
		return stats, err
	}
	defer release()
	if c.native.transactionState() != nativeHandleConsumed {
		return stats, errors.New("interbase: fingerprint read requires no active transaction")
	}
	tpb, err := buildTPB(driver.TxOptions{ReadOnly: true, Isolation: driver.IsolationLevel(sql.LevelReadCommitted)}, TransactionOptions{})
	if err != nil {
		return stats, err
	}
	tx, err := c.native.beginTransaction(tpb)
	if err != nil {
		if tx != nil {
			cleanupErr, _ := tx.rollbackCleanup()
			err = errors.Join(err, cleanupErr)
			tx.free()
		}
		release()
		c.invalidateLocked(err)
		return stats, errors.Join(c.sanitizeError("begin fingerprint read", err), driver.ErrBadConn)
	}
	var cleanupErr error
	defer func() {
		if err == nil {
			cleanupErr = errors.Join(cleanupErr, tx.commit())
			if cleanupErr != nil && tx.handleState() != nativeHandleConsumed {
				rollbackErr, _ := tx.rollbackCleanup()
				cleanupErr = errors.Join(cleanupErr, rollbackErr)
			}
		} else {
			rollbackErr, _ := tx.rollbackCleanup()
			cleanupErr = errors.Join(cleanupErr, rollbackErr)
		}
		if tx.handleState() != nativeHandleConsumed {
			cleanupErr = errors.Join(cleanupErr, errors.New("interbase: fingerprint transaction cleanup left a native handle"))
		}
		tx.free()
		err = errors.Join(err, c.sanitizeError("fingerprint cleanup", cleanupErr))
		bad := cleanupErr != nil || c.native.broken()
		release()
		if bad {
			c.invalidateLocked(err)
			err = errors.Join(err, driver.ErrBadConn)
		}
	}()
	for index, query := range queries {
		if err = validateDatabaseSQLQuery(query.SQL); err != nil {
			return stats, err
		}
		args, convertErr := convertDirectArguments(query.Args)
		if convertErr != nil {
			return stats, convertErr
		}
		// Prepare only to classify; execution still uses the direct identity path.
		// Array outputs are rejected here and again by fingerprintValue.
		statement, prepareErr := tx.prepare(ctx, query.SQL, false)
		if prepareErr != nil {
			return stats, c.sanitizeError("prepare fingerprint read", prepareErr)
		}
		description, describeErr := statement.description()
		closeErr := statement.close()
		cleanupErr = errors.Join(cleanupErr, closeErr)
		if describeErr != nil || closeErr != nil {
			return stats, errors.Join(describeErr, closeErr)
		}
		if description.Kind != "select" || description.Mutating || !description.ReturnsRows {
			return stats, errors.New("interbase: fingerprint read requires a read-only SELECT")
		}
		cursor, columns, queryErr := tx.query(ctx, query.SQL, args, true)
		if queryErr != nil {
			return stats, c.sanitizeError("query fingerprint read", queryErr)
		}
		stats.Queries++
		readErr := func() error {
			for {
				if e := contextError(ctx); e != nil {
					return e
				}
				hasRow, e := cursor.next(ctx)
				if e != nil {
					return e
				}
				if !hasRow {
					return nil
				}
				cells := make([]Cell, len(columns))
				for col := range cells {
					cells[col].Indicator, e = cursor.indicator(col)
					if e != nil {
						return e
					}
					cells[col].Value, e = cursor.fingerprintValue(col)
					if e != nil {
						return e
					}
				}
				stats.Rows++
				if e = visit(index, append([]string(nil), columns...), cells); e != nil {
					return e
				}
			}
		}()
		opens, bytes := cursor.blobReadStats()
		stats.BlobOpens += opens
		stats.SourceBytes += bytes
		if readErr != nil {
			closeErr = cursor.abort()
		} else {
			closeErr = cursor.close()
		}
		cleanupErr = errors.Join(cleanupErr, closeErr)
		if readErr != nil || closeErr != nil {
			return stats, errors.Join(readErr, closeErr)
		}
	}
	return stats, contextError(ctx)
}
