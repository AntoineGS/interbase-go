package interbase

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ProfilePreparationOptions controls snapshots taken before the prepared
// profile statement is retained on its connection.
type ProfilePreparationOptions struct {
	RoutineActivity bool
}

type preparedProfileState uint8

const (
	preparedProfileReady preparedProfileState = iota
	preparedProfileRunning
	preparedProfileFinished
	preparedProfileClosed
)

// PreparedProfile is a one-shot described statement that can be profiled
// without preparing it again. It is bound to the *sql.Conn used to create it.
// Close it when it will not be run; do not execute other statements on that
// connection until Profile or Close.
type PreparedProfile struct {
	mu sync.Mutex

	conn              *sql.Conn
	query             string
	pending           *PendingStatement
	descriptor        StatementDescriptor
	rawPlan           string
	prepare           time.Duration
	activityBefore    Activity
	routinesBefore    RoutineActivity
	routinesBeforeErr error
	routineActivity   bool
	state             preparedProfileState
}

// PrepareProfile snapshots activity, then prepares and describes query once
// while retaining the statement for Profile. No explicit transaction is
// started. Activity reads must precede PrepareNext because any other SQL work
// on the connection discards its pending statement.
func PrepareProfile(ctx context.Context, pooled *sql.Conn, query string,
	options ProfilePreparationOptions) (*PreparedProfile, error) {
	if pooled == nil {
		return nil, ErrNotInterBaseConn
	}
	if err := contextError(ctx); err != nil {
		return nil, err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return nil, err
	}

	var routinesBefore RoutineActivity
	var routinesBeforeErr error
	if options.RoutineActivity {
		routinesBefore, routinesBeforeErr = ReadRoutineActivity(ctx, pooled)
	}
	activityBefore, err := ReadActivity(ctx, pooled)
	if err != nil {
		return nil, err
	}

	pending, prepare, err := prepareNext(ctx, pooled, query)
	if err != nil {
		return nil, err
	}
	rawPlan, err := pendingRawPlan(ctx, pending)
	if err != nil {
		return nil, errors.Join(err, pending.Close())
	}
	return &PreparedProfile{
		conn: pooled, query: query, pending: pending,
		descriptor: pending.Descriptor(), rawPlan: rawPlan, prepare: prepare,
		activityBefore: activityBefore, routinesBefore: routinesBefore,
		routinesBeforeErr: routinesBeforeErr, routineActivity: options.RoutineActivity,
	}, nil
}

// Descriptor returns a copy of the prepared statement's description.
func (p *PreparedProfile) Descriptor() StatementDescriptor {
	if p == nil {
		return StatementDescriptor{}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	descriptor := p.descriptor
	descriptor.Columns = append([]ColumnOrigin(nil), descriptor.Columns...)
	return descriptor
}

// Profile executes the prepared statement in a short read-committed
// transaction, discards result values, and always rolls back. The statement is
// consumed once. Close the prepared profile when it is not run.
func (p *PreparedProfile) Profile(ctx context.Context, options ProfileOptions) (result ProfileResult, resultErr error) {
	if p == nil {
		return result, errors.New("interbase: prepared profile is nil")
	}
	p.mu.Lock()
	if p.state != preparedProfileReady {
		p.mu.Unlock()
		return result, errors.New("interbase: prepared profile is not available")
	}
	p.state = preparedProfileRunning
	pending := p.pending
	p.mu.Unlock()
	defer func() {
		if closeErr := pending.Close(); closeErr != nil {
			resultErr = errors.Join(resultErr, closeErr)
		}
		p.mu.Lock()
		p.pending = nil
		p.state = preparedProfileFinished
		p.mu.Unlock()
	}()

	result.Prepare = p.prepare
	result.RawPlan = p.rawPlan
	result.ReturnsRows = p.descriptor.ReturnsRows
	if p.routinesBeforeErr != nil {
		result.RoutinesUnavailable = p.routinesBeforeErr
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}
	if options.RoutineActivity != p.routineActivity {
		return result, errors.New("interbase: prepared profile routine-activity option changed")
	}
	options, err := normalizeProfileOptions(options)
	if err != nil {
		return result, err
	}
	if p.descriptor.Kind == "unsupported" {
		return result, fmt.Errorf("interbase: profile statement type %q is unsupported", p.descriptor.Kind)
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}

	transaction, err := p.conn.BeginTx(context.WithoutCancel(ctx), &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
		ReadOnly:  options.ReadOnly,
	})
	if err != nil {
		return result, err
	}
	transactionFinished := false
	defer func() {
		if transactionFinished {
			return
		}
		rollbackErr := transaction.Rollback()
		transactionFinished = true
		if rollbackErr == nil {
			result.RolledBack = true
			return
		}
		resultErr = errors.Join(resultErr, fmt.Errorf("%w: %w", ErrProfileRollback, rollbackErr))
	}()

	if p.descriptor.ReturnsRows {
		started := time.Now()
		rows, queryErr := transaction.QueryContext(ctx, p.query)
		result.Execute = time.Since(started)
		resultErr = queryErr
		if queryErr == nil {
			started = time.Now()
			for options.FetchAll || result.Rows < int64(options.MaxRows) {
				if !rows.Next() {
					if rows.Err() != nil {
						resultErr = errors.Join(resultErr, rows.Err())
					} else {
						result.Complete = true
					}
					break
				}
				result.Rows++
			}
			result.Fetch = time.Since(started)
			if closeErr := rows.Close(); closeErr != nil {
				resultErr = errors.Join(resultErr, closeErr)
			}
		}
	} else {
		started := time.Now()
		_, resultErr = transaction.ExecContext(ctx, p.query)
		result.Execute = time.Since(started)
		result.Complete = resultErr == nil
	}

	rollbackErr := transaction.Rollback()
	transactionFinished = true
	if rollbackErr == nil {
		result.RolledBack = true
	} else {
		resultErr = errors.Join(resultErr, fmt.Errorf("%w: %w", ErrProfileRollback, rollbackErr))
	}

	cleanupContext := context.WithoutCancel(ctx)
	after, afterErr := ReadActivity(cleanupContext, p.conn)
	if afterErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("interbase: read profile activity after execution: %w", afterErr))
	} else {
		result, err = finishProfile(result, p.activityBefore, after)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		} else if len(result.Activity.Tables) != 0 {
			ids := make([]int, 0, len(result.Activity.Tables))
			for id := range result.Activity.Tables {
				ids = append(ids, id)
			}
			names, namesErr := RelationNames(cleanupContext, p.conn, ids)
			if namesErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("interbase: resolve profile relation names: %w", namesErr))
			} else {
				result.RelationNames = names
			}
		}
	}

	if p.routineActivity {
		routinesAfter, routinesAfterErr := ReadRoutineActivity(cleanupContext, p.conn)
		if routinesAfterErr != nil {
			result.RoutinesUnavailable = errors.Join(p.routinesBeforeErr, routinesAfterErr)
			result.Routines = nil
		} else if p.routinesBeforeErr == nil {
			result.Routines = routinesAfter.Delta(p.routinesBefore)
		}
	}
	return result, resultErr
}

// Close discards an unexecuted pending statement. It is idempotent and does
// not start a transaction.
func (p *PreparedProfile) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	if p.state == preparedProfileRunning {
		p.mu.Unlock()
		return errors.New("interbase: prepared profile is running")
	}
	if p.state != preparedProfileReady {
		p.mu.Unlock()
		return nil
	}
	p.state = preparedProfileClosed
	pending := p.pending
	p.pending = nil
	p.mu.Unlock()
	return pending.Close()
}

func pendingRawPlan(ctx context.Context, pending *PendingStatement) (string, error) {
	if pending == nil || pending.conn == nil {
		return "", errors.New("interbase: profile statement is unavailable")
	}
	var plan string
	err := pending.conn.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		c.lockDirect()
		defer c.mu.Unlock()
		if err := contextError(ctx); err != nil {
			return err
		}
		if c.closed || c.native == nil || c.native.broken() {
			return driver.ErrBadConn
		}
		if c.pending != pending.statement || pending.statement == nil || pending.statement.native == nil {
			return errors.New("interbase: profile statement is no longer pending")
		}
		releaseNative, err := enterNativeContext(ctx)
		if err != nil {
			return err
		}
		defer releaseNative()
		plan, err = pending.statement.native.plan()
		if err != nil {
			err = c.sanitizeError("read profile plan", err)
			if c.native.broken() {
				c.invalidateLocked(err)
			}
		}
		return err
	})
	return plan, err
}
