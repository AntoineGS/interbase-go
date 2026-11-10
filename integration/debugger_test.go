//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
)

type debuggerFixture struct {
	conn     *sql.Conn
	database string
	user     string
	password string
	dialect  int
	cleanup  *fixtureCleanup
	ctx      context.Context
}

func newDebuggerFixture(t *testing.T, dialect int) *debuggerFixture {
	t.Helper()
	fixture, cfg, cleanup := createFixture(t, dialect, "")
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", dialect,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 3*time.Minute)
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatalf("debug fixture db.Conn(): %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close debug fixture connection: %v", err)
		}
	})

	for _, statement := range []string{
		"CREATE TABLE DBG_T (ID INTEGER NOT NULL PRIMARY KEY, A INTEGER)",
		"INSERT INTO DBG_T (ID, A) VALUES (1, 10)",
		"INSERT INTO DBG_T (ID, A) VALUES (2, 20)",
		"CREATE EXCEPTION DBG_E 'debug fixture exception'",
		`CREATE PROCEDURE DBG_READ RETURNS (ID INTEGER, A INTEGER) AS
BEGIN
  FOR SELECT ID, A FROM DBG_T ORDER BY ID INTO :ID, :A DO SUSPEND;
END`,
		`CREATE PROCEDURE DBG_WRITE (P_ID INTEGER) RETURNS (A INTEGER) AS
BEGIN
  UPDATE DBG_T SET A = A + 1 WHERE ID = :P_ID;
  SELECT A FROM DBG_T WHERE ID = :P_ID INTO :A;
  SUSPEND;
  BEGIN
    INSERT INTO DBG_T (ID, A) VALUES (99, 99);
    EXCEPTION DBG_E;
  WHEN EXCEPTION DBG_E DO A = A;
  END
END`,
		`CREATE PROCEDURE DBG_LOOP_WRITE (P_COUNT INTEGER) RETURNS (ITERATION INTEGER, A INTEGER) AS
BEGIN
  ITERATION = 0;
  WHILE (ITERATION < P_COUNT) DO
  BEGIN
    ITERATION = ITERATION + 1;
    UPDATE DBG_T SET A = A + 1 WHERE ID = 1;
    SELECT A FROM DBG_T WHERE ID = 1 INTO :A;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE DBG_CALLEE RETURNS (ID INTEGER, A INTEGER) AS
BEGIN
  FOR SELECT ID, A FROM DBG_T ORDER BY ID INTO :ID, :A DO SUSPEND;
END`,
		`CREATE PROCEDURE DBG_CALLER RETURNS (ID INTEGER, A INTEGER) AS
BEGIN
  FOR SELECT ID, A FROM DBG_CALLEE ORDER BY ID INTO :ID, :A DO
  BEGIN
    A = A + 1;
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE DBG_ROWS RETURNS (I INTEGER, A INTEGER) AS
BEGIN
  I = 0;
  WHILE (I < 3) DO
  BEGIN
    I = I + 1;
    A = I * 10;
    SUSPEND;
  END
END`,
	} {
		if _, err := conn.ExecContext(ctx, statement); err != nil {
			t.Fatalf("debug fixture DDL %q: %v", strings.SplitN(statement, "\n", 2)[0], err)
		}
	}

	return &debuggerFixture{
		conn: conn, database: fixture.ConnectionString(), user: cfg.User, password: cfg.Password,
		dialect: dialect, cleanup: cleanup, ctx: ctx,
	}
}

func (f *debuggerFixture) openConn(t *testing.T, options interbase.TransactionOptions) *sql.Conn {
	t.Helper()
	db := openDatabase(t, f.cleanup, f.database, f.user, f.password, "NONE", f.dialect, options)
	conn, err := db.Conn(f.ctx)
	if err != nil {
		t.Fatalf("debug fixture attachment db.Conn(): %v", err)
	}
	t.Cleanup(func() {
		if err := conn.Close(); err != nil {
			t.Errorf("close debug fixture attachment: %v", err)
		}
	})
	return conn
}

func TestDebugParityWithLineProfile(t *testing.T) {
	queries := []struct {
		name  string
		query string
	}{
		{name: "read", query: "SELECT * FROM DBG_READ"},
		{name: "write", query: "SELECT * FROM DBG_WRITE(1)"},
		{name: "loop write", query: "SELECT * FROM DBG_LOOP_WRITE(2)"},
		{name: "caller", query: "SELECT * FROM DBG_CALLER"},
		{name: "callee", query: "SELECT * FROM DBG_CALLEE"},
		{name: "suspend rows", query: "SELECT * FROM DBG_ROWS"},
	}
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			f := newDebuggerFixture(t, dialect)
			for _, tc := range queries {
				t.Run(tc.name, func(t *testing.T) {
					analysis, err := interbase.AnalyzeLineProfile(f.ctx, f.conn, tc.query, dialect, nil)
					if err != nil || len(analysis.Problems) != 0 {
						t.Fatalf("AnalyzeLineProfile(%q) = %+v, %v", tc.query, analysis, err)
					}
					nativeRows, nativeErr := lpNative(t, f.ctx, f.conn, tc.query)
					if nativeErr != nil {
						t.Fatalf("native rolled-back run of %q: %v", tc.query, nativeErr)
					}

					lineProfile, err := interbase.LineProfile(f.ctx, f.conn, interbase.LineProfileOptions{
						SQL: tc.query, Dialect: dialect, ReadOnly: !analysis.MayWrite,
						StepInto: debuggerLineProfileStepInto(analysis),
					})
					if err != nil {
						t.Fatalf("LineProfile(%q): %v", tc.query, err)
					}
					if lineProfile.Outcome != interbase.LineProfileCompleted || !lineProfile.RolledBack {
						t.Fatalf("LineProfile(%q) outcome/rollback = %s/%t, error %v",
							tc.query, lineProfile.Outcome, lineProfile.RolledBack, lineProfile.Err)
					}

					debugged := completeDebug(t, f.ctx, f.conn, tc.query, dialect)
					if debugged.Outcome != interbase.DebugCompleted {
						t.Fatalf("StartDebug(%q) outcome = %s, error %v", tc.query, debugged.Outcome, debugged.Err)
					}

					lineRows := debuggerRows(lineProfile.KeptRows)
					debugRows := debuggerRows(debugged.Rows)
					if !reflect.DeepEqual(lineRows, nativeRows) {
						t.Fatalf("LineProfile(%q) rows = %#v, native = %#v", tc.query, lineRows, nativeRows)
					}
					if !reflect.DeepEqual(debugRows, nativeRows) {
						t.Fatalf("StartDebug(%q) rows = %#v, native = %#v", tc.query, debugRows, nativeRows)
					}
					if !reflect.DeepEqual(debugged.Columns, lineProfile.Columns) ||
						debugged.RowsTotal != lineProfile.Rows || debugged.RowsTotal != int64(len(nativeRows)) {
						t.Fatalf("debug columns/rows = %v/%d, LineProfile = %v/%d, native rows = %d",
							debugged.Columns, debugged.RowsTotal, lineProfile.Columns, lineProfile.Rows, len(nativeRows))
					}
				})
			}
		})
	}
}

func debuggerLineProfileStepInto(analysis interbase.LineProfileAnalysis) []string {
	var routines []string
	for _, callee := range analysis.Callees {
		if callee.StepInShape {
			routines = append(routines, callee.Name)
		}
	}
	return routines
}

func debuggerRows(rows [][]interbase.Cell) [][]any {
	values := make([][]any, len(rows))
	for i, row := range rows {
		values[i] = lpValues(row)
	}
	return values
}

func completeDebug(t *testing.T, ctx context.Context, conn *sql.Conn, query string, dialect int) interbase.DebugSnapshot {
	t.Helper()
	session, err := interbase.StartDebug(ctx, conn, interbase.DebugOptions{
		SQL: query, Dialect: dialect, AllowWrites: true,
	})
	if err != nil {
		t.Fatalf("StartDebug(%q): %v", query, err)
	}
	t.Cleanup(session.Stop)
	waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused
	}, "entry pause")
	if err := session.Command(interbase.DebugCommand{Kind: interbase.DebugContinue}); err != nil {
		t.Fatalf("Continue(%q): %v", query, err)
	}
	select {
	case <-session.Done():
	case <-time.After(45 * time.Second):
		t.Fatalf("debug run of %q did not finish", query)
	}
	return session.Latest()
}

func waitDebugSnapshot(t *testing.T, session *interbase.DebugSession,
	predicate func(interbase.DebugSnapshot) bool, description string) interbase.DebugSnapshot {
	t.Helper()
	timer := time.NewTimer(30 * time.Second)
	defer timer.Stop()
	for {
		select {
		case snapshot, ok := <-session.Snapshots():
			if !ok {
				snapshot = session.Latest()
				if predicate(snapshot) {
					return snapshot
				}
				t.Fatalf("debug snapshots closed before %s (latest status %s, outcome %s)",
					description, snapshot.Status, snapshot.Outcome)
			}
			if predicate(snapshot) {
				return snapshot
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for debug %s", description)
		}
	}
}

func TestDebugLazySwitchOnServer(t *testing.T) {
	f := newDebuggerFixture(t, 3)
	observer := f.openConn(t, interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	baseline := debuggerTransactionCount(t, f.ctx, observer)
	query := "SELECT * FROM DBG_WRITE(1)"
	analysis, err := interbase.AnalyzeDebug(f.ctx, f.conn, query, 3)
	if err != nil {
		t.Fatalf("AnalyzeDebug(): %v", err)
	}
	if !analysis.MayWrite || analysis.ReadWriteFromStart {
		t.Fatalf("DBG_WRITE analysis = may-write %t, read-write-from-start %t; want lazy write",
			analysis.MayWrite, analysis.ReadWriteFromStart)
	}
	suspendLine := lpLine(t, analysis.Routines[0].Source, "SUSPEND;")
	session, err := interbase.StartDebug(f.ctx, f.conn, interbase.DebugOptions{
		SQL: query, Dialect: 3, AllowWrites: true,
		Breakpoints: []interbase.DebugBreakpoint{{Routine: "DBG_WRITE", Line: suspendLine}},
	})
	if err != nil {
		t.Fatalf("StartDebug(): %v", err)
	}
	t.Cleanup(session.Stop)
	entry := waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused
	}, "DBG_WRITE entry pause")
	if entry.Phase != interbase.DebugReadOnly {
		t.Fatalf("entry phase = %s, want read-only", entry.Phase)
	}
	during := debuggerTransactionCount(t, f.ctx, observer)
	if during <= baseline {
		t.Fatalf("transaction count while paused = %d, baseline %d; want debug transaction present", during, baseline)
	}
	if err := session.Command(interbase.DebugCommand{Kind: interbase.DebugContinue}); err != nil {
		t.Fatalf("Continue(): %v", err)
	}
	paused := waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused && len(snapshot.Frames) != 0 &&
			snapshot.Frames[0].Routine == "DBG_WRITE" && snapshot.Frames[0].Line == suspendLine
	}, "pause after DBG_WRITE update")
	if paused.Phase != interbase.DebugWritten {
		t.Fatalf("phase after UPDATE = %s, want written", paused.Phase)
	}
	session.Stop()
	ended := session.Latest()
	if ended.Outcome != interbase.DebugStopped {
		t.Fatalf("Stop() outcome = %s, error %v; want stopped", ended.Outcome, ended.Err)
	}
	var value int64
	if err := observer.QueryRowContext(f.ctx, "SELECT A FROM DBG_T WHERE ID = 1").Scan(&value); err != nil {
		t.Fatalf("read row after Stop(): %v", err)
	}
	if value != 10 {
		t.Fatalf("row after Stop() = %d, want original value 10", value)
	}
	after := debuggerTransactionCount(t, f.ctx, observer)
	if after != baseline {
		t.Fatalf("transaction count after Stop() = %d, baseline %d", after, baseline)
	}
}

func TestDebugLocksReleasedOnStop(t *testing.T) {
	f := newDebuggerFixture(t, 3)
	query := "SELECT * FROM DBG_WRITE(1)"
	analysis, err := interbase.AnalyzeDebug(f.ctx, f.conn, query, 3)
	if err != nil {
		t.Fatalf("AnalyzeDebug(): %v", err)
	}
	suspendLine := lpLine(t, analysis.Routines[0].Source, "SUSPEND;")
	session, err := interbase.StartDebug(f.ctx, f.conn, interbase.DebugOptions{
		SQL: query, Dialect: 3, AllowWrites: true,
		Breakpoints: []interbase.DebugBreakpoint{{Routine: "DBG_WRITE", Line: suspendLine}},
	})
	if err != nil {
		t.Fatalf("StartDebug(): %v", err)
	}
	t.Cleanup(session.Stop)
	waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused
	}, "DBG_WRITE entry pause")
	if err := session.Command(interbase.DebugCommand{Kind: interbase.DebugContinue}); err != nil {
		t.Fatalf("Continue(): %v", err)
	}
	waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused && len(snapshot.Frames) != 0 &&
			snapshot.Frames[0].Line == suspendLine
	}, "pause after DBG_WRITE update")

	contenderConn := f.openConn(t, interbase.TransactionOptions{
		Isolation: sql.LevelReadCommitted, NoWait: true,
	})
	contender, err := contenderConn.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin no-wait contender: %v", err)
	}
	registerTransactionRollback(t, contender)
	_, err = contender.ExecContext(f.ctx, "UPDATE DBG_T SET A = A + 1 WHERE ID = 1")
	lockErr := requireNativeError(t, err)
	if lockErr.NativeCode != iscLockConflict && lockErr.GDSCode() != iscLockConflict {
		t.Fatalf("contending update status = %d (GDS %d), want lock conflict %d",
			lockErr.NativeCode, lockErr.GDSCode(), iscLockConflict)
	}
	if err := contender.Rollback(); err != nil {
		t.Fatalf("rollback conflicting update: %v", err)
	}

	session.Stop()
	if got := session.Latest().Outcome; got != interbase.DebugStopped {
		t.Fatalf("Stop() outcome = %s, want stopped", got)
	}
	retry, err := contenderConn.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin retry after Stop(): %v", err)
	}
	registerTransactionRollback(t, retry)
	if _, err := retry.ExecContext(f.ctx, "UPDATE DBG_T SET A = A + 1 WHERE ID = 1"); err != nil {
		t.Fatalf("update after Stop() should acquire released row lock: %v", err)
	}
	if err := retry.Rollback(); err != nil {
		t.Fatalf("rollback retry update: %v", err)
	}
}

func TestDebugNoWaitConflictAtLine(t *testing.T) {
	f := newDebuggerFixture(t, 3)
	blockerConn := f.openConn(t, interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	blocker, err := blockerConn.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin row-2 blocker: %v", err)
	}
	registerTransactionRollback(t, blocker)
	if _, err := blocker.ExecContext(f.ctx, "UPDATE DBG_T SET A = A + 1 WHERE ID = 2"); err != nil {
		t.Fatalf("hold row-2 lock: %v", err)
	}

	query := "SELECT * FROM DBG_WRITE(2)"
	analysis, err := interbase.AnalyzeDebug(f.ctx, f.conn, query, 3)
	if err != nil {
		t.Fatalf("AnalyzeDebug(): %v", err)
	}
	updateLine := lpLine(t, analysis.Routines[0].Source, "UPDATE DBG_T SET A = A + 1")
	session, err := interbase.StartDebug(f.ctx, f.conn, interbase.DebugOptions{
		SQL: query, Dialect: 3, AllowWrites: true,
	})
	if err != nil {
		t.Fatalf("StartDebug(): %v", err)
	}
	t.Cleanup(session.Stop)
	waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused
	}, "DBG_WRITE entry pause")
	if err := session.Command(interbase.DebugCommand{Kind: interbase.DebugContinue}); err != nil {
		t.Fatalf("Continue(): %v", err)
	}
	select {
	case <-session.Done():
	case <-time.After(30 * time.Second):
		t.Fatal("debug session did not fail at the locked UPDATE")
	}
	failed := session.Latest()
	if failed.Outcome != interbase.DebugFailed || failed.FailedAt == nil ||
		failed.FailedAt.Routine != "DBG_WRITE" || failed.FailedAt.Line != updateLine {
		t.Fatalf("debug failure = outcome %s at %+v, error %v; want DBG_WRITE line %d",
			failed.Outcome, failed.FailedAt, failed.Err, updateLine)
	}
	lockErr := requireNativeError(t, failed.Err)
	if lockErr.NativeCode != iscLockConflict && lockErr.GDSCode() != iscLockConflict {
		t.Fatalf("debug update status = %d (GDS %d), want lock conflict %d",
			lockErr.NativeCode, lockErr.GDSCode(), iscLockConflict)
	}
	if err := blocker.Rollback(); err != nil {
		t.Fatalf("rollback row-2 blocker: %v", err)
	}
	var value int64
	if err := f.conn.QueryRowContext(f.ctx, "SELECT A FROM DBG_T WHERE ID = 2").Scan(&value); err != nil {
		t.Fatalf("read row 2 after failed debug: %v", err)
	}
	if value != 20 {
		t.Fatalf("row 2 after failed debug = %d, want original value 20", value)
	}
}

func TestDebugStopDuringLongStatement(t *testing.T) {
	f := newDebuggerFixture(t, 3)
	query := "SELECT * FROM DBG_LOOP_WRITE(1000000)"
	session, err := interbase.StartDebug(f.ctx, f.conn, interbase.DebugOptions{
		SQL: query, Dialect: 3, AllowWrites: true,
	})
	if err != nil {
		t.Fatalf("StartDebug(): %v", err)
	}
	t.Cleanup(session.Stop)
	waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugPaused
	}, "DBG_LOOP_WRITE entry pause")
	if err := session.Command(interbase.DebugCommand{Kind: interbase.DebugContinue}); err != nil {
		t.Fatalf("Continue(): %v", err)
	}
	waitDebugSnapshot(t, session, func(snapshot interbase.DebugSnapshot) bool {
		return snapshot.Status == interbase.DebugRunning
	}, "running long loop")
	time.Sleep(100 * time.Millisecond)
	if snapshot := session.Latest(); snapshot.Status != interbase.DebugRunning {
		t.Fatalf("long loop ended before Stop(): status %s, outcome %s", snapshot.Status, snapshot.Outcome)
	}
	started := time.Now()
	stopped := make(chan struct{})
	go func() {
		session.Stop()
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(5 * time.Second):
		t.Fatal("Stop() did not finish within 5 seconds during the running loop")
	}
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("Stop() took %s, want at most 5 seconds", elapsed)
	}
	if snapshot := session.Latest(); snapshot.Outcome != interbase.DebugStopped {
		t.Fatalf("Stop() outcome = %s, error %v; want stopped", snapshot.Outcome, snapshot.Err)
	}
}

func TestDebugSavepointsInReadOnlyTransaction(t *testing.T) {
	f := newDebuggerFixture(t, 3)
	attachment := openDirectAttachment(t, f.cleanup, f.database, f.user, f.password, "", "NONE", 3,
		interbase.TransactionOptions{})
	tx, err := attachment.BeginTx(f.ctx, interbase.TransactionOptions{
		Isolation: sql.LevelReadCommitted, ReadOnly: true,
	})
	if err != nil {
		t.Fatalf("begin read-only transaction: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	_, savepointErr := tx.Exec(f.ctx, "SAVEPOINT DBG_READ_ONLY_SP")
	if savepointErr == nil {
		t.Log("server accepts SAVEPOINT in a read-only transaction")
	} else {
		t.Logf("server rejects SAVEPOINT in a read-only transaction: %v", savepointErr)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback read-only transaction: %v", err)
	}
}

func debuggerTransactionCount(t *testing.T, ctx context.Context, conn *sql.Conn) int64 {
	t.Helper()
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM TMP$TRANSACTIONS").Scan(&count); err != nil {
		t.Fatalf("count active transactions: %v", err)
	}
	return count
}
