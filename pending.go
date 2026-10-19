package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
)

// PendingStatement is a statement PrepareNext prepared and left on its
// connection for the next execution of the same text.
type PendingStatement struct {
	conn       *sql.Conn
	statement  *stmt
	descriptor StatementDescriptor
}

// PrepareNext prepares query once on the pinned connection and returns its
// description, as DescribeStatement does, but keeps the prepared statement on
// the connection. The connection's next QueryContext or ExecContext, including
// one made through a transaction begun meanwhile, runs the kept statement
// instead of preparing again when it has the identical text and no arguments.
// Any other text or arguments, another PrepareNext, DescribeStatement,
// DescribeInputs, Plan or database/sql Prepare on the connection discard it.
//
// Close the PendingStatement on every path that does not execute it: the
// prepared statement keeps the objects it references in use, which blocks
// their DDL by other attachments.
func PrepareNext(ctx context.Context, pooled *sql.Conn, query string) (*PendingStatement, error) {
	if pooled == nil {
		return nil, ErrNotInterBaseConn
	}
	pending := &PendingStatement{conn: pooled}
	if err := pooled.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		var err error
		pending.statement, pending.descriptor, err = c.prepareNext(ctx, query)
		return err
	}); err != nil {
		return nil, err
	}
	return pending, nil
}

// Descriptor returns the prepared statement's description.
func (p *PendingStatement) Descriptor() StatementDescriptor {
	if p == nil {
		return StatementDescriptor{}
	}
	return p.descriptor
}

// Close discards the statement when it is still pending. It is a no-op once
// the statement ran or was discarded, and is idempotent.
func (p *PendingStatement) Close() error {
	if p == nil || p.conn == nil || p.statement == nil {
		return nil
	}
	err := p.conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.closed || c.pending != p.statement {
			return nil
		}
		return c.discardPendingLocked()
	})
	if errors.Is(err, sql.ErrConnDone) {
		return nil
	}
	return err
}

func (c *conn) prepareNext(ctx context.Context, query string) (*stmt, StatementDescriptor, error) {
	if err := contextError(ctx); err != nil {
		return nil, StatementDescriptor{}, err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return nil, StatementDescriptor{}, err
	}
	c.lockDirect()
	defer c.mu.Unlock()
	if c.distributed != nil {
		return nil, StatementDescriptor{}, ErrDistributedParticipantManaged
	}
	if c.closed || c.native == nil || c.native.broken() {
		return nil, StatementDescriptor{}, driver.ErrBadConn
	}
	if err := contextError(ctx); err != nil {
		return nil, StatementDescriptor{}, err
	}
	var description StatementDescriptor
	native, err := c.prepareInspectedLocked(ctx, query, "describe statement", func(statement *nativeStatement) error {
		var inspectErr error
		description, inspectErr = statement.description()
		return inspectErr
	}, true)
	if err != nil {
		return nil, StatementDescriptor{}, err
	}
	prepared := &stmt{conn: c, native: native, numInput: native.numInput()}
	if c.statements == nil {
		c.statements = make(map[*stmt]struct{})
	}
	c.statements[prepared] = struct{}{}
	c.pending, c.pendingQuery = prepared, query
	return prepared, description, nil
}

// takePendingLocked returns the pending statement when query and args select
// it, and otherwise discards any pending statement.
func (c *conn) takePendingLocked(query string, args []argument) (*stmt, error) {
	pending := c.pending
	if pending == nil {
		return nil, nil
	}
	if query == c.pendingQuery && len(args) == 0 {
		c.pending, c.pendingQuery = nil, ""
		return pending, nil
	}
	return nil, c.discardPendingLocked()
}

func (c *conn) discardPendingLocked() error {
	pending := c.pending
	c.pending, c.pendingQuery = nil, ""
	return c.releaseStatementLocked(pending)
}

// releaseStatementLocked closes a statement the connection owns. A close
// failure invalidates the connection, as stmt.Close does.
func (c *conn) releaseStatementLocked(s *stmt) error {
	if s == nil || c.closed {
		return nil
	}
	if err := c.closeStatementLocked(s); err != nil {
		err = c.sanitizeError("close statement", err)
		c.invalidateLocked(err)
		return err
	}
	return nil
}
