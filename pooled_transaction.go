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
// back otherwise, including before re-panicking if fn panics. A database/sql
// transaction or cursor already open on pooled is left untouched; this is how
// a short write runs beside an open read-only cursor without a second
// attachment. The *Transaction must not be retained after fn returns.
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
		completed := false
		defer func() {
			if completed {
				return
			}
			panicValue := recover()
			_ = closePooledTransactionResources(tx)
			_ = tx.Rollback()
			if panicValue != nil {
				panic(panicValue)
			}
		}()

		if callbackErr := fn(tx); callbackErr != nil {
			cleanupErr := closePooledTransactionResources(tx)
			rollbackErr := tx.Rollback()
			completed = true
			return errors.Join(callbackErr, cleanupErr, rollbackErr)
		}
		if err := closePooledTransactionResources(tx); err != nil {
			rollbackErr := tx.Rollback()
			completed = true
			return errors.Join(err, rollbackErr)
		}
		if err := tx.Commit(); err != nil {
			completed = true
			return fmt.Errorf("%w: %w", ErrTransactionCommit, err)
		}
		completed = true
		return nil
	})
}

func closePooledTransactionResources(tx *Transaction) error {
	if tx == nil || tx.attachment == nil || tx.attachment.conn == nil {
		return errTransactionDone
	}
	c := tx.attachment.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := tx.checkLocked(); err != nil {
		return err
	}
	var cleanupErr error
	if err := tx.closeCursorsLocked(); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	if err := tx.closeBlobsLocked(true); err != nil {
		cleanupErr = errors.Join(cleanupErr, err)
	}
	return cleanupErr
}
