package interbase

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// ProfileOptions controls how Profile executes and measures a statement.
type ProfileOptions struct {
	ReadOnly        bool
	FetchAll        bool
	MaxRows         int
	RoutineActivity bool
}

// ProfileResult contains timings and activity measured by Profile. Rows are
// discarded as they are fetched; no result values are returned.
type ProfileResult struct {
	RawPlan                 string
	Prepare, Execute, Fetch time.Duration
	ReturnsRows             bool
	Rows                    int64
	Complete                bool
	Activity                ActivityDelta
	RelationNames           map[int]string
	Routines                map[RoutineKey]RoutineCounters
	RoutinesUnavailable     error
	RolledBack              bool
}

// ErrProfileRollback reports that Profile could not confirm its rollback.
var ErrProfileRollback = errors.New("interbase: profile rollback was not confirmed")

func normalizeProfileOptions(options ProfileOptions) (ProfileOptions, error) {
	if options.MaxRows < 0 {
		return ProfileOptions{}, fmt.Errorf("interbase: profile MaxRows must not be negative: %d", options.MaxRows)
	}
	if options.MaxRows == 0 {
		options.MaxRows = 256
	}
	return options, nil
}

func finishProfile(result ProfileResult, before, after Activity) (ProfileResult, error) {
	delta, err := after.Delta(before)
	if err != nil {
		return result, err
	}
	result.Activity = delta
	return result, nil
}

func runProfileTransaction(rollback func() error, operation func() error, rolledBack *bool) (err error) {
	rollbackAttempted := false
	defer func() {
		panicValue := recover()
		if rollbackAttempted {
			if panicValue != nil {
				panic(panicValue)
			}
			return
		}
		rollbackAttempted = true
		rollbackErr := rollback()
		if rollbackErr == nil && rolledBack != nil {
			*rolledBack = true
		}
		if panicValue != nil {
			if rollbackErr != nil {
				panic(profilePanicRollbackError(panicValue, rollbackErr))
			}
			panic(panicValue)
		}
		if rollbackErr != nil {
			err = errors.Join(err, fmt.Errorf("%w: %w", ErrProfileRollback, rollbackErr))
		}
	}()

	operationErr := operation()
	rollbackAttempted = true
	rollbackErr := rollback()
	if rollbackErr == nil && rolledBack != nil {
		*rolledBack = true
	}
	if rollbackErr != nil {
		rollbackErr = fmt.Errorf("%w: %w", ErrProfileRollback, rollbackErr)
	}
	return errors.Join(operationErr, rollbackErr)
}

func profilePanicRollbackError(panicValue any, rollbackErr error) error {
	var panicErr error
	if originalErr, ok := panicValue.(error); ok {
		panicErr = fmt.Errorf("profile panic: %w", originalErr)
	} else {
		panicErr = fmt.Errorf("profile panic: %v", panicValue)
	}
	return errors.Join(panicErr, fmt.Errorf("%w: %w", ErrProfileRollback, rollbackErr))
}

// Profile runs query once in its own read-committed transaction on the
// attachment behind conn, discards every fetched row, and always rolls the
// transaction back. Any caller transaction or cursor on conn is left
// untouched. On failure after execution starts it returns the partial result
// and an error; RolledBack reports whether rollback was confirmed.
func Profile(ctx context.Context, pooled *sql.Conn, query string, options ProfileOptions) (result ProfileResult, resultErr error) {
	if pooled == nil {
		return result, ErrNotInterBaseConn
	}
	options, err := normalizeProfileOptions(options)
	if err != nil {
		return result, err
	}
	if err := contextError(ctx); err != nil {
		return result, err
	}
	if err := validateDatabaseSQLQuery(query); err != nil {
		return result, err
	}

	var routinesBefore RoutineActivity
	var routinesBeforeErr error
	if options.RoutineActivity {
		routinesBefore, routinesBeforeErr = ReadRoutineActivity(ctx, pooled)
		if routinesBeforeErr != nil {
			result.RoutinesUnavailable = routinesBeforeErr
		}
	}

	before, err := ReadActivity(ctx, pooled)
	if err != nil {
		return result, err
	}

	transactionOptions := TransactionOptions{
		ReadOnly:  options.ReadOnly,
		Isolation: sql.LevelReadCommitted,
	}
	profileErr := pooled.Raw(func(driverConn any) error {
		c, ok := driverConn.(*conn)
		if !ok {
			return ErrNotInterBaseConn
		}
		attachment := &Attachment{conn: c}
		tx, err := attachment.BeginTx(ctx, transactionOptions)
		if err != nil {
			return err
		}

		return runProfileTransaction(tx.Rollback, func() error {
			result.RawPlan, result.Prepare, result.Execute, result.Fetch,
				result.ReturnsRows, result.Rows, result.Complete, err =
				tx.profileStatement(ctx, query, options.MaxRows, options.FetchAll)
			return err
		}, &result.RolledBack)
	})
	resultErr = errors.Join(resultErr, profileErr)

	cleanupContext := context.WithoutCancel(ctx)
	after, afterErr := ReadActivity(cleanupContext, pooled)
	if afterErr != nil {
		resultErr = errors.Join(resultErr, fmt.Errorf("interbase: read profile activity after execution: %w", afterErr))
	} else {
		result, err = finishProfile(result, before, after)
		if err != nil {
			resultErr = errors.Join(resultErr, err)
		} else if len(result.Activity.Tables) != 0 {
			ids := make([]int, 0, len(result.Activity.Tables))
			for id := range result.Activity.Tables {
				ids = append(ids, id)
			}
			names, namesErr := RelationNames(cleanupContext, pooled, ids)
			if namesErr != nil {
				resultErr = errors.Join(resultErr, fmt.Errorf("interbase: resolve profile relation names: %w", namesErr))
			} else {
				result.RelationNames = names
			}
		}
	}

	if options.RoutineActivity {
		routinesAfter, routinesAfterErr := ReadRoutineActivity(cleanupContext, pooled)
		if routinesAfterErr != nil {
			result.RoutinesUnavailable = errors.Join(result.RoutinesUnavailable, routinesAfterErr)
			result.Routines = nil
		} else if routinesBeforeErr == nil {
			result.Routines = routinesAfter.Delta(routinesBefore)
		}
	}
	return result, resultErr
}
