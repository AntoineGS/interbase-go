package interbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrTransactionCommit wraps a commit failure from WithTransaction: the
// commit was sent, so its outcome is unknown and must not be retried blindly.
var ErrTransactionCommit = errors.New("interbase: transaction commit outcome is unknown")

// WithTransaction runs fn in a new, independent native transaction on the
// attachment behind pooled, then commits it when fn returns nil and rolls it
// back otherwise. A database/sql transaction or cursor already open on pooled
// is left untouched; this is how a short write runs beside an open read-only
// cursor without a second attachment. The *Transaction must not be retained
// after fn returns.
func WithTransaction(ctx context.Context, pooled *sql.Conn, options TransactionOptions, fn func(*Transaction) error) error {
	if pooled == nil {
		return ErrNotInterBaseConn
	}
	return pooled.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		// A scoped direct view of the pooled connection: it owns only the
		// transaction it begins and is never closed (closing would detach).
		attachment := &Attachment{conn: c}
		tx, err := attachment.BeginTx(ctx, options)
		if err != nil {
			return err
		}
		if err := fn(tx); err != nil {
			return errors.Join(err, tx.Rollback())
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("%w: %w", ErrTransactionCommit, err)
		}
		return nil
	})
}
