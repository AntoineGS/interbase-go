package interbase

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"
	"time"

	"interbase-go/internal/nativegate"
)

type pendingFake struct {
	prepares       []string
	oneShotQueries []string
	oneShotExecs   []string
	statementRuns  int
	events         []string
	queryErr       error
	execErr        error
	describeErr    error
	closeErr       error
	descriptor     StatementDescriptor
}

func (f *pendingFake) connection() *conn {
	native := introspectionTestNative(3)
	native.cancelSlotOverride = &fakeCancelSlot{}
	native.prepareOverride = func(query string) (*nativeStatement, error) {
		f.prepares = append(f.prepares, query)
		id := len(f.prepares)
		return &nativeStatement{
			numInputOverride: func() int { return 0 },
			descriptionOverride: func() (StatementDescriptor, error) {
				if f.describeErr != nil {
					return StatementDescriptor{}, f.describeErr
				}
				if f.descriptor.Kind != "" {
					return f.descriptor, nil
				}
				return StatementDescriptor{Kind: "select", ReturnsRows: true}, nil
			},
			queryOverride: func([]argument) (*nativeCursor, []string, error) {
				f.statementRuns++
				if f.queryErr != nil {
					return nil, nil, f.queryErr
				}
				return &nativeCursor{
					nextOverride:  func() (bool, error) { return false, nil },
					closeOverride: func() error { f.events = append(f.events, "cursor"); return nil },
				}, []string{"X"}, nil
			},
			execOverride: func([]argument) (int64, error) {
				f.statementRuns++
				return 3, f.execErr
			},
			closeOverride: func() error {
				f.events = append(f.events, "statement"+string(rune('0'+id)))
				return f.closeErr
			},
		}, nil
	}
	native.queryContextOverride = func(query string, _ []argument, _ bool, _ *nativeCancelOperation) (*nativeCursor, []string, error) {
		f.oneShotQueries = append(f.oneShotQueries, query)
		return &nativeCursor{nextOverride: func() (bool, error) { return false, nil }}, []string{"X"}, nil
	}
	native.execContextOverride = func(query string, _ []argument, _ bool, _ *nativeCancelOperation) (int64, error) {
		f.oneShotExecs = append(f.oneShotExecs, query)
		return 1, nil
	}
	return &conn{native: native}
}

func pendingTestConn(t *testing.T, f *pendingFake) *sql.Conn {
	t.Helper()
	db := openDatabaseSQLTestDB(t, f.connection())
	connection, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	return connection
}

const pendingSQL = "SELECT X FROM T"

func TestPrepareNextReusesStatementForIdenticalQuery(t *testing.T) {
	f := &pendingFake{}
	connection := pendingTestConn(t, f)
	pending, err := PrepareNext(context.Background(), connection, pendingSQL)
	if err != nil {
		t.Fatal(err)
	}
	if got := pending.Descriptor(); got.Kind != "select" || !got.ReturnsRows {
		t.Fatalf("descriptor = %+v", got)
	}
	rows, err := connection.QueryContext(context.Background(), pendingSQL)
	if err != nil {
		t.Fatal(err)
	}
	if rows.Next() {
		t.Fatal("unexpected row")
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if len(f.prepares) != 1 || len(f.oneShotQueries) != 0 || f.statementRuns != 1 {
		t.Fatalf("prepares=%v oneShot=%v statementRuns=%d", f.prepares, f.oneShotQueries, f.statementRuns)
	}
	if want := []string{"cursor", "statement1"}; !reflect.DeepEqual(f.events, want) {
		t.Fatalf("close order = %v, want %v", f.events, want)
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("Close after consumption: %v", err)
	}
	if len(f.events) != 2 {
		t.Fatalf("Close after consumption closed again: %v", f.events)
	}
}

func TestPrepareNextPreservesStatementKinds(t *testing.T) {
	for _, kind := range []string{
		"start-transaction", "commit", "rollback", "set-generator", "savepoint",
	} {
		t.Run(kind, func(t *testing.T) {
			f := &pendingFake{descriptor: StatementDescriptor{
				Kind: kind, Mutating: kind == "set-generator",
			}}
			pending, err := PrepareNext(context.Background(), pendingTestConn(t, f), pendingSQL)
			if err != nil {
				t.Fatal(err)
			}
			if got := pending.Descriptor(); got.Kind != kind || got.Mutating != (kind == "set-generator") {
				t.Fatalf("descriptor = %+v, want kind %q and expected mutability", got, kind)
			}
			if err := pending.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestPrepareNextReusesStatementForIdenticalExec(t *testing.T) {
	f := &pendingFake{}
	connection := pendingTestConn(t, f)
	const update = "UPDATE T SET X = 1"
	if _, err := PrepareNext(context.Background(), connection, update); err != nil {
		t.Fatal(err)
	}
	result, err := connection.ExecContext(context.Background(), update)
	if err != nil {
		t.Fatal(err)
	}
	if affected, _ := result.RowsAffected(); affected != 3 {
		t.Fatalf("affected = %d, want the reused statement's 3", affected)
	}
	if len(f.prepares) != 1 || len(f.oneShotExecs) != 0 || f.statementRuns != 1 {
		t.Fatalf("prepares=%v oneShot=%v statementRuns=%d", f.prepares, f.oneShotExecs, f.statementRuns)
	}
	if want := []string{"statement1"}; !reflect.DeepEqual(f.events, want) {
		t.Fatalf("closes = %v, want %v", f.events, want)
	}
}

func TestPrepareNextDiscardsOnDifferentTextOrArguments(t *testing.T) {
	f := &pendingFake{}
	connection := pendingTestConn(t, f)
	if _, err := PrepareNext(context.Background(), connection, pendingSQL); err != nil {
		t.Fatal(err)
	}
	rows, err := connection.QueryContext(context.Background(), "SELECT Y FROM T")
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if !reflect.DeepEqual(f.events, []string{"statement1"}) || len(f.oneShotQueries) != 1 || f.statementRuns != 0 {
		t.Fatalf("different text: events=%v oneShot=%v runs=%d", f.events, f.oneShotQueries, f.statementRuns)
	}

	if _, err := PrepareNext(context.Background(), connection, "UPDATE T SET X = ?"); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(context.Background(), "UPDATE T SET X = ?", 1); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1", "statement2"}) || len(f.oneShotExecs) != 1 || f.statementRuns != 0 {
		t.Fatalf("arguments: events=%v oneShot=%v runs=%d", f.events, f.oneShotExecs, f.statementRuns)
	}
}

func TestPrepareNextDiscardsOnOtherStatementWorkAndRepeatedPrepare(t *testing.T) {
	f := &pendingFake{}
	connection := pendingTestConn(t, f)
	if _, err := PrepareNext(context.Background(), connection, pendingSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := DescribeStatement(context.Background(), connection, "SELECT Z FROM T"); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1", "statement2"}) {
		t.Fatalf("describe: events=%v", f.events)
	}
	if _, err := PrepareNext(context.Background(), connection, pendingSQL); err != nil {
		t.Fatal(err)
	}
	if _, err := PrepareNext(context.Background(), connection, pendingSQL); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1", "statement2", "statement3"}) {
		t.Fatalf("repeated prepare: events=%v", f.events)
	}
}

func TestPendingStatementCloseDiscardsAndIsIdempotent(t *testing.T) {
	f := &pendingFake{}
	connection := pendingTestConn(t, f)
	pending, err := PrepareNext(context.Background(), connection, pendingSQL)
	if err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1"}) {
		t.Fatalf("events = %v", f.events)
	}
	rows, err := connection.QueryContext(context.Background(), pendingSQL)
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(f.oneShotQueries) != 1 || f.statementRuns != 0 {
		t.Fatalf("query after Close reused a discarded statement: oneShot=%v runs=%d", f.oneShotQueries, f.statementRuns)
	}
	if err := connection.Close(); err != nil {
		t.Fatal(err)
	}
	if err := pending.Close(); err != nil {
		t.Fatalf("Close after the connection closed: %v", err)
	}
}

func TestPrepareNextFailedQueryClosesStatementAndKeepsError(t *testing.T) {
	want := &Error{Operation: "execute prepared SELECT", SQLCode: -204, Message: "table unknown"}
	f := &pendingFake{queryErr: want}
	connection := pendingTestConn(t, f)
	if _, err := PrepareNext(context.Background(), connection, pendingSQL); err != nil {
		t.Fatal(err)
	}
	_, err := connection.QueryContext(context.Background(), pendingSQL)
	var got *Error
	if !errors.As(err, &got) || got.SQLCode != -204 {
		t.Fatalf("error = %v, want the native failure", err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1"}) || f.statementRuns != 1 || len(f.oneShotQueries) != 0 {
		t.Fatalf("events=%v runs=%d oneShot=%v", f.events, f.statementRuns, f.oneShotQueries)
	}
}

func TestPrepareNextFailedExecClosesStatementAndKeepsError(t *testing.T) {
	want := &Error{Operation: "execute prepared statement", SQLCode: -803, Message: "duplicate"}
	f := &pendingFake{execErr: want}
	connection := pendingTestConn(t, f)
	const insert = "INSERT INTO T VALUES (1)"
	if _, err := PrepareNext(context.Background(), connection, insert); err != nil {
		t.Fatal(err)
	}
	_, err := connection.ExecContext(context.Background(), insert)
	var got *Error
	if !errors.As(err, &got) || got.SQLCode != -803 {
		t.Fatalf("error = %v, want the native failure", err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1"}) {
		t.Fatalf("events=%v", f.events)
	}
}

func TestPrepareNextDescribeFailureClosesStatement(t *testing.T) {
	want := errors.New("describe failed")
	f := &pendingFake{describeErr: want}
	connection := pendingTestConn(t, f)
	if _, err := PrepareNext(context.Background(), connection, pendingSQL); !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1"}) {
		t.Fatalf("events=%v", f.events)
	}
	rows, err := connection.QueryContext(context.Background(), pendingSQL)
	if err != nil {
		t.Fatal(err)
	}
	_ = rows.Close()
	if len(f.oneShotQueries) != 1 {
		t.Fatalf("failed PrepareNext left a pending statement: oneShot=%v", f.oneShotQueries)
	}
}

func TestPrepareNextRejectsCanceledContextBeforePrepare(t *testing.T) {
	f := &pendingFake{}
	connection := pendingTestConn(t, f)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareNext(ctx, connection, pendingSQL); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v", err)
	}
	if len(f.prepares) != 0 {
		t.Fatalf("prepared %v", f.prepares)
	}
}

func TestPrepareNextExecReportsKnownSuccessWhenStatementCloseFails(t *testing.T) {
	f := &pendingFake{closeErr: errors.New("drop failed")}
	c := f.connection()
	const insert = "INSERT INTO T VALUES (1)"
	if _, _, err := c.prepareNext(context.Background(), insert); err != nil {
		t.Fatal(err)
	}
	result, err := c.ExecContext(context.Background(), insert, nil)
	if err != nil {
		t.Fatalf("a completed write reported failure: %v", err)
	}
	if affected, _ := result.RowsAffected(); affected != 3 {
		t.Fatalf("affected = %d", affected)
	}
	if !c.closed {
		t.Fatal("a failed statement close did not invalidate the connection")
	}
}

func TestPrepareNextCanceledWriteWithFailedCloseIsUncertain(t *testing.T) {
	f := &pendingFake{closeErr: errors.New("drop failed")}
	c := f.connection()
	const update = "UPDATE T SET X = 1"
	if _, _, err := c.prepareNext(context.Background(), update); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f.execErr = &Error{Operation: "execute prepared statement", NativeCode: nativeCancelledCode, Message: "isc_cancelled"}
	c.pending.native.execOverride = func([]argument) (int64, error) {
		cancel()
		return 0, f.execErr
	}
	_, err := c.ExecContext(ctx, update, nil)
	var uncertain *UncertainOutcomeError
	if !errors.As(err, &uncertain) {
		t.Fatalf("error = %v, want an uncertain outcome", err)
	}
}

func TestPrepareNextDiscardWaitsForAdmissionWithItsContext(t *testing.T) {
	f := &pendingFake{}
	c := f.connection()
	if _, _, err := c.prepareNext(context.Background(), pendingSQL); err != nil {
		t.Fatal(err)
	}
	release := nativegate.Global.EnterExclusive()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryContext(ctx, "SELECT Y FROM T", nil)
		done <- err
	}()
	select {
	case err := <-done:
		release()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want the context deadline", err)
		}
	case <-time.After(2 * time.Second):
		release()
		<-done
		t.Fatal("discarding the pending statement ignored the context while the gate was exclusive")
	}
	if c.pending == nil || len(f.events) != 0 {
		t.Fatalf("an unadmitted call consumed the pending statement: events=%v", f.events)
	}
}

func TestPrepareNextFailedDiscardInvalidatesWithoutDeadlock(t *testing.T) {
	f := &pendingFake{closeErr: errors.New("drop failed")}
	c := f.connection()
	if _, _, err := c.prepareNext(context.Background(), pendingSQL); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := c.QueryContext(context.Background(), "SELECT Y FROM T", nil)
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !c.closed {
			t.Fatalf("error=%v closed=%v, want a close failure that invalidates the connection", err, c.closed)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a failed discard deadlocked while the call held native admission")
	}
}

func TestConnCloseReleasesPendingStatement(t *testing.T) {
	f := &pendingFake{}
	c := f.connection()
	if _, _, err := c.prepareNext(context.Background(), pendingSQL); err != nil {
		t.Fatal(err)
	}
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.events, []string{"statement1"}) || c.pending != nil {
		t.Fatalf("events=%v pending=%v", f.events, c.pending)
	}
}
