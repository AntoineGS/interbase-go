package interbase

import (
	"context"
	"errors"
	"fmt"
)

var (
	errPreparedStatementClosed     = errors.New("interbase: prepared statement is closed")
	errPreparedStatementCursorOpen = errors.New("interbase: prepared statement has an open cursor")
	errPreparedStatementNoRows     = errors.New("interbase: prepared statement does not return rows")
)

// OutputDescriptor is one output column description returned by the server
// for a prepared statement. Length is the SQLDA length in bytes; for text
// columns SubType is the character set id.
type OutputDescriptor struct {
	Name                   string // alias as described
	Kind                   string // same vocabulary as InputDescriptor.Kind
	SubType, Scale, Length int
	Nullable               bool
}

// PreparedStatement is a statement prepared once inside a direct Transaction
// and executed any number of times with new arguments. It becomes unusable
// when its transaction completes; Close is idempotent.
type PreparedStatement struct {
	tx          *Transaction
	native      *nativeStatement
	cursor      *Cursor
	inputs      []InputDescriptor
	outputs     []OutputDescriptor
	returnsRows bool
}

// Prepare prepares one statement in the direct transaction. The statement
// stays valid until it is closed or the transaction completes.
func (t *Transaction) Prepare(ctx context.Context, query string) (*PreparedStatement, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateQueryText(query); err != nil {
		return nil, err
	}
	if t == nil || t.attachment == nil || t.attachment.conn == nil {
		return nil, errTransactionDone
	}
	t.attachment.conn.mu.Lock()
	defer t.attachment.conn.mu.Unlock()
	a, err := t.checkLocked()
	if err != nil {
		return nil, err
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	statement, err := t.native.prepare(ctx, query, true)
	releaseNative()
	if err != nil {
		evidence := nativeCancellationEvidenceOf(err)
		parts := splitNativeExecutionError(err)
		operationErr := a.conn.sanitizeError("prepare statement", parts.primary)
		cleanupErr := a.conn.sanitizeError("", parts.cleanup)
		operationErr = classifyNativeOutcome("prepare statement", false,
			contextCancellation(ctx), operationErr, cleanupErr, evidence)
		operationErr = joinNativeRequestDiagnostic(operationErr,
			a.conn.sanitizeError("", parts.request))
		if a.conn.native.broken() {
			t.invalidateLocked(operationErr)
		}
		return nil, operationErr
	}

	prepared := &PreparedStatement{tx: t, native: statement}
	describeErr := func() error {
		description, err := statement.description()
		if err != nil {
			return a.conn.sanitizeError("describe prepared statement", err)
		}
		if description.Kind == "unsupported" {
			return fmt.Errorf("interbase: prepared statement type %q is unsupported", description.Kind)
		}
		prepared.returnsRows = description.ReturnsRows
		if prepared.inputs, err = statement.inputDescriptors(); err != nil {
			return a.conn.sanitizeError("describe prepared inputs", err)
		}
		if prepared.outputs, err = statement.outputDescriptors(); err != nil {
			return a.conn.sanitizeError("describe prepared outputs", err)
		}
		return contextError(ctx)
	}()
	if describeErr != nil {
		closeErr := statement.close()
		if closeErr != nil {
			closeErr = a.conn.sanitizeError("close prepared statement", closeErr)
			if a.conn.native.broken() {
				t.invalidateLocked(closeErr)
			}
		}
		return nil, errors.Join(describeErr, closeErr)
	}
	if t.prepared == nil {
		t.prepared = make(map[*PreparedStatement]struct{})
	}
	t.prepared[prepared] = struct{}{}
	return prepared, nil
}

// lock locks the connection and verifies the statement and its
// transaction are still usable. The caller unlocks the returned attachment's
// connection.
func (s *PreparedStatement) lock() (*Attachment, error) {
	if s == nil || s.tx == nil || s.tx.attachment == nil || s.tx.attachment.conn == nil {
		return nil, errTransactionDone
	}
	s.tx.attachment.conn.mu.Lock()
	a, err := s.tx.checkLocked()
	if err == nil && s.native == nil {
		err = errPreparedStatementClosed
	}
	if err != nil {
		s.tx.attachment.conn.mu.Unlock()
		return nil, err
	}
	return a, nil
}

func (s *PreparedStatement) cursorOpenLocked() bool {
	return s.cursor != nil && !s.cursor.closed
}

// Query executes the statement and returns its result cursor. The cursor
// must be closed before the statement runs again.
func (s *PreparedStatement) Query(ctx context.Context, args ...any) (*Cursor, error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	converted, err := convertDirectArguments(args)
	if err != nil {
		return nil, err
	}
	a, err := s.lock()
	if err != nil {
		return nil, err
	}
	defer a.conn.mu.Unlock()
	t := s.tx
	if !s.returnsRows {
		return nil, errPreparedStatementNoRows
	}
	if s.cursorOpenLocked() {
		return nil, errPreparedStatementCursorOpen
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateDirectArguments(converted, t); err != nil {
		return nil, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseNative()
	nativeCursor, columns, err := s.native.query(ctx, converted)
	releaseNative()
	if err != nil {
		return nil, t.classifyNativeStatementFailureLocked(ctx, a, "prepared query",
			s.native.queryMutating(), err)
	}
	cursor := &Cursor{
		tx:         t,
		generation: t.generation,
		native:     nativeCursor,
		columns:    append([]string(nil), columns...),
		ctx:        ctx,
	}
	nativeCursor.directTransaction = t
	t.cursors[cursor] = struct{}{}
	s.cursor = cursor
	if err := contextError(ctx); err != nil {
		closeErr := cursor.closeLocked(true, err)
		return nil, errors.Join(err, closeErr)
	}
	return cursor, nil
}

// Exec executes the statement and returns the rows affected, or 0 when the
// server does not report a count.
func (s *PreparedStatement) Exec(ctx context.Context, args ...any) (int64, error) {
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	converted, err := convertDirectArguments(args)
	if err != nil {
		return 0, err
	}
	a, err := s.lock()
	if err != nil {
		return 0, err
	}
	defer a.conn.mu.Unlock()
	t := s.tx
	if s.cursorOpenLocked() {
		return 0, errPreparedStatementCursorOpen
	}
	if err := contextError(ctx); err != nil {
		return 0, err
	}
	if err := validateDirectArguments(converted, t); err != nil {
		return 0, err
	}
	releaseNative, err := enterNativeContext(ctx)
	if err != nil {
		return 0, err
	}
	defer releaseNative()
	affected, err := s.native.exec(ctx, converted)
	releaseNative()
	if err != nil {
		return 0, t.classifyNativeStatementFailureLocked(ctx, a, "prepared exec", true, err)
	}
	return affected, nil
}

// Inputs describes the statement's positional parameters.
func (s *PreparedStatement) Inputs() []InputDescriptor {
	if s == nil {
		return nil
	}
	return append([]InputDescriptor(nil), s.inputs...)
}

// Outputs describes the statement's result columns.
func (s *PreparedStatement) Outputs() []OutputDescriptor {
	if s == nil {
		return nil
	}
	return append([]OutputDescriptor(nil), s.outputs...)
}

// Close releases the statement and any cursor it still has open. It is safe
// to call repeatedly and after the transaction has completed.
func (s *PreparedStatement) Close() error {
	if s == nil || s.tx == nil || s.tx.attachment == nil || s.tx.attachment.conn == nil {
		return nil
	}
	c := s.tx.attachment.conn
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.native == nil {
		return nil
	}
	t := s.tx
	delete(t.prepared, s)
	var closeErr error
	if s.cursorOpenLocked() {
		closeErr = s.cursor.closeLocked(false, nil)
	}
	s.cursor = nil
	native := s.native
	s.native = nil
	if err := native.close(); err != nil {
		err = c.sanitizeError("close prepared statement", err)
		closeErr = errors.Join(closeErr, err)
		if c.native != nil && c.native.broken() {
			t.invalidateLocked(err)
		}
	}
	return closeErr
}

// closePreparedLocked releases every prepared statement of the transaction.
func (t *Transaction) closePreparedLocked() error {
	if t == nil || len(t.prepared) == 0 {
		return nil
	}
	var firstErr error
	for statement := range t.prepared {
		delete(t.prepared, statement)
		native := statement.native
		statement.native = nil
		statement.cursor = nil
		if native == nil {
			continue
		}
		if err := native.close(); err != nil {
			firstErr = errors.Join(firstErr,
				t.attachment.conn.sanitizeError("close prepared statement", err))
		}
	}
	return firstErr
}
