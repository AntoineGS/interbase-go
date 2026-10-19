package interbase

import (
	"context"
	"errors"
	"testing"
)

type preparedTestCounts struct{ statementClose, cursorClose, free int }

func newPreparedTestTransaction(t *testing.T, returnsRows bool) (*Transaction, *preparedTestCounts) {
	t.Helper()
	counts := &preparedTestCounts{}
	native := &nativeTransaction{
		cancelSlotOverride:  &fakeCancelSlot{},
		commitOverride:      func() error { return nil },
		rollbackOverride:    func() error { return nil },
		handleStateOverride: func() nativeHandleState { return nativeHandleConsumed },
		freeOverride:        func() { counts.free++ },
		prepareContextOverride: func(string, bool, *nativeCancelOperation) (*nativeStatement, error) {
			return &nativeStatement{
				descriptionOverride: func() (StatementDescriptor, error) {
					return StatementDescriptor{Kind: "select", ReturnsRows: returnsRows, InputCount: 1}, nil
				},
				numInputOverride:      func() int { return 1 },
				inputMetadataOverride: func(int) (nativeInputMetadata, error) { return nativeInputMetadata{}, nil },
				outputsOverride: func() ([]OutputDescriptor, error) {
					return []OutputDescriptor{{Name: "V", Kind: "VARCHAR", Length: 10, Nullable: true}}, nil
				},
				queryOverride: func([]argument) (*nativeCursor, []string, error) {
					return &nativeCursor{closeOverride: func() error { counts.cursorClose++; return nil }},
						[]string{"V"}, nil
				},
				execOverride:  func([]argument) (int64, error) { return 1, nil },
				closeOverride: func() error { counts.statementClose++; return nil },
			}, nil
		},
	}
	connection := &conn{native: &nativeConnection{brokenOverride: func() bool { return false }}}
	attachment := &Attachment{conn: connection, generation: 1}
	tx := &Transaction{
		attachment: attachment,
		native:     native,
		generation: 1,
		cursors:    make(map[*Cursor]struct{}),
	}
	attachment.directTx = tx
	return tx, counts
}

func TestDirectPreparedStatementDescribesAndCloseIsIdempotent(t *testing.T) {
	tx, counts := newPreparedTestTransaction(t, true)
	statement, err := tx.Prepare(context.Background(), "SELECT V FROM LP_T WHERE ID = ?")
	if err != nil {
		t.Fatal(err)
	}
	if got := statement.Outputs(); len(got) != 1 || got[0].Name != "V" || got[0].Length != 10 {
		t.Fatalf("Outputs() = %#v", got)
	}
	if got := statement.Inputs(); len(got) != 1 {
		t.Fatalf("Inputs() = %#v", got)
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if counts.statementClose != 1 {
		t.Fatalf("native statement closed %d times, want 1", counts.statementClose)
	}
	if _, err := statement.Query(context.Background(), int64(1)); !errors.Is(err, errPreparedStatementClosed) {
		t.Fatalf("Query after Close error = %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if counts.statementClose != 1 {
		t.Fatalf("commit re-closed a closed statement: %d", counts.statementClose)
	}
}

func TestDirectPreparedStatementUnusableAfterTransactionEnds(t *testing.T) {
	for _, commit := range []bool{true, false} {
		tx, counts := newPreparedTestTransaction(t, true)
		statement, err := tx.Prepare(context.Background(), "SELECT V FROM LP_T WHERE ID = ?")
		if err != nil {
			t.Fatal(err)
		}
		if commit {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil {
			t.Fatal(err)
		}
		if counts.statementClose != 1 {
			t.Fatalf("transaction end closed statement %d times, want 1", counts.statementClose)
		}
		if _, err := statement.Query(context.Background(), int64(1)); !errors.Is(err, errTransactionDone) {
			t.Fatalf("Query error = %v, want transaction done", err)
		}
		if _, err := statement.Exec(context.Background(), int64(1)); !errors.Is(err, errTransactionDone) {
			t.Fatalf("Exec error = %v, want transaction done", err)
		}
		if err := statement.Close(); err != nil {
			t.Fatal(err)
		}
		if counts.statementClose != 1 {
			t.Fatalf("Close after transaction end re-closed statement: %d", counts.statementClose)
		}
	}
}

func TestDirectPreparedStatementRejectsRunWhileCursorOpen(t *testing.T) {
	tx, counts := newPreparedTestTransaction(t, true)
	statement, err := tx.Prepare(context.Background(), "SELECT V FROM LP_T WHERE ID = ?")
	if err != nil {
		t.Fatal(err)
	}
	cursor, err := statement.Query(context.Background(), int64(1))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statement.Query(context.Background(), int64(2)); !errors.Is(err, errPreparedStatementCursorOpen) {
		t.Fatalf("second Query error = %v", err)
	}
	if _, err := statement.Exec(context.Background(), int64(2)); !errors.Is(err, errPreparedStatementCursorOpen) {
		t.Fatalf("Exec error = %v", err)
	}
	if err := cursor.Close(); err != nil {
		t.Fatal(err)
	}
	cursor, err = statement.Query(context.Background(), int64(2))
	if err != nil {
		t.Fatalf("Query after cursor Close: %v", err)
	}
	// Closing the statement closes its cursor first.
	if err := statement.Close(); err != nil {
		t.Fatal(err)
	}
	if counts.cursorClose != 2 || counts.statementClose != 1 {
		t.Fatalf("closes: cursor %d statement %d, want 2 and 1", counts.cursorClose, counts.statementClose)
	}
	_ = cursor
}

func TestDirectPreparedStatementQueryRequiresRows(t *testing.T) {
	tx, _ := newPreparedTestTransaction(t, false)
	statement, err := tx.Prepare(context.Background(), "INSERT INTO LP_T (ID) VALUES (?)")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := statement.Query(context.Background(), int64(1)); !errors.Is(err, errPreparedStatementNoRows) {
		t.Fatalf("Query error = %v", err)
	}
	if affected, err := statement.Exec(context.Background(), int64(1)); err != nil || affected != 1 {
		t.Fatalf("Exec = (%d, %v)", affected, err)
	}
}

func TestDirectPreparedStatementExecRejectsRows(t *testing.T) {
	tx, _ := newPreparedTestTransaction(t, true)
	statement, err := tx.Prepare(context.Background(), "SELECT V FROM LP_T WHERE ID = ?")
	if err != nil {
		t.Fatal(err)
	}
	const message = "interbase: prepared exec: statement produces a result set; use Query instead"
	_, err = statement.Exec(context.Background(), int64(1))
	if !errors.Is(err, ErrPreparedStatementReturnsRows) || err.Error() != message {
		t.Fatalf("Exec error = %v, want ErrPreparedStatementReturnsRows with %q", err, message)
	}
}

type statementFailureOutcome struct {
	canceled  bool
	deadline  bool
	nativeErr *Error
	broken    bool // the transaction was invalidated for later use
}

// TestPreparedStatementFailuresClassifyLikeDirectStatements runs the same
// failing native query/exec through Transaction.Query/Exec and through a
// PreparedStatement and requires identical classification.
func TestPreparedStatementFailuresClassifyLikeDirectStatements(t *testing.T) {
	native := &Error{Operation: "execute", SQLCode: -902, NativeCode: 335544344, Message: "boom"}
	cancelled := &Error{Operation: "execute", NativeCode: nativeCancelledCode, Message: "isc_cancelled"}
	scenarios := []struct {
		name   string
		err    *Error
		cancel bool
		broken bool
	}{
		{name: "native error", err: native},
		{name: "context canceled during native call", err: cancelled, cancel: true},
		{name: "broken connection", err: native, broken: true},
	}
	for _, scenario := range scenarios {
		for _, operation := range []string{"query", "exec"} {
			run := func(prepared bool) statementFailureOutcome {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				armed := false
				fail := func() error {
					armed = true
					if scenario.cancel {
						cancel()
					}
					return scenario.err
				}
				// Exec of a result-set statement is rejected before the native
				// call, so the exec scenarios prepare a non-row statement.
				tx, _ := newPreparedTestTransaction(t, operation == "query")
				tx.attachment.conn.native.brokenOverride = func() bool { return armed && scenario.broken }
				tx.native.queryContextOverride = func(string, []argument, bool, *nativeCancelOperation) (*nativeCursor, []string, error) {
					return nil, nil, fail()
				}
				tx.native.execContextOverride = func(string, []argument, bool, *nativeCancelOperation) (int64, error) {
					return 0, fail()
				}
				var err error
				var statement *PreparedStatement
				if prepared {
					var prepareErr error
					statement, prepareErr = tx.Prepare(context.Background(), "SELECT V FROM LP_T WHERE ID = ?")
					if prepareErr != nil {
						t.Fatal(prepareErr)
					}
					statement.native.queryOverride = func([]argument) (*nativeCursor, []string, error) {
						return nil, nil, fail()
					}
					statement.native.execOverride = func([]argument) (int64, error) { return 0, fail() }
					if operation == "query" {
						_, err = statement.Query(ctx, int64(1))
					} else {
						_, err = statement.Exec(ctx, int64(1))
					}
				} else if operation == "query" {
					_, err = tx.Query(ctx, "SELECT V FROM LP_T WHERE ID = ?", int64(1))
				} else {
					_, err = tx.Exec(ctx, "UPDATE LP_T SET V = 'x' WHERE ID = ?", int64(1))
				}
				if err == nil {
					t.Fatalf("%s %s prepared=%v: no error", scenario.name, operation, prepared)
				}
				outcome := statementFailureOutcome{
					canceled: errors.Is(err, context.Canceled),
					deadline: errors.Is(err, context.DeadlineExceeded),
				}
				errors.As(err, &outcome.nativeErr)
				outcome.broken = tx.done
				if outcome.nativeErr != nil {
					copied := *outcome.nativeErr
					copied.Operation = ""
					outcome.nativeErr = &copied
				}
				return outcome
			}
			direct, prepared := run(false), run(true)
			if direct.canceled != prepared.canceled || direct.deadline != prepared.deadline ||
				direct.broken != prepared.broken ||
				(direct.nativeErr == nil) != (prepared.nativeErr == nil) ||
				(direct.nativeErr != nil && (direct.nativeErr.NativeCode != prepared.nativeErr.NativeCode ||
					direct.nativeErr.SQLCode != prepared.nativeErr.SQLCode ||
					direct.nativeErr.Message != prepared.nativeErr.Message)) {
				t.Errorf("%s %s: direct = %+v, prepared = %+v", scenario.name, operation, direct, prepared)
			}
			if scenario.cancel && !prepared.canceled {
				t.Errorf("%s %s: prepared error did not classify as context.Canceled", scenario.name, operation)
			}
			if scenario.broken && !prepared.broken {
				t.Errorf("%s %s: broken connection did not invalidate the transaction: %+v", scenario.name, operation, prepared)
			}
			if !scenario.cancel && !scenario.broken && (prepared.nativeErr == nil || prepared.nativeErr.NativeCode != native.NativeCode) {
				t.Errorf("%s %s: prepared error lost native code: %+v", scenario.name, operation, prepared)
			}
		}
	}
}
