//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	interbase "interbase-go"
)

func TestActivityAndRoutineSnapshots(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3, "")
	dbA := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	dbB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 2*time.Minute)

	if _, err := dbA.ExecContext(ctx, "CREATE TABLE PROF_T (ID INTEGER NOT NULL PRIMARY KEY, V INTEGER)"); err != nil {
		t.Fatalf("create PROF_T: %v", err)
	}
	tx, err := dbA.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("begin insert transaction: %v", err)
	}
	statement, err := tx.PrepareContext(ctx, "INSERT INTO PROF_T (ID, V) VALUES (?, ?)")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("prepare PROF_T insert: %v", err)
	}
	for id := int64(1); id <= 100; id++ {
		if _, err := statement.ExecContext(ctx, id, id*10); err != nil {
			_ = statement.Close()
			_ = tx.Rollback()
			t.Fatalf("insert PROF_T row %d: %v", id, err)
		}
	}
	if err := statement.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close PROF_T insert: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit PROF_T inserts: %v", err)
	}

	connA, err := dbA.Conn(ctx)
	if err != nil {
		t.Fatalf("pool A Conn(): %v", err)
	}
	defer connA.Close()
	connB, err := dbB.Conn(ctx)
	if err != nil {
		t.Fatalf("pool B Conn(): %v", err)
	}
	defer connB.Close()

	var relationID int
	if err := connA.QueryRowContext(ctx,
		"SELECT RDB$RELATION_ID FROM RDB$RELATIONS WHERE RDB$RELATION_NAME = 'PROF_T'").Scan(&relationID); err != nil {
		t.Fatalf("read PROF_T relation ID: %v", err)
	}
	names, err := interbase.RelationNames(ctx, connA, []int{relationID})
	if err != nil {
		t.Fatalf("RelationNames(): %v", err)
	}
	if names[relationID] != "PROF_T" {
		t.Fatalf("RelationNames()[%d] = %q, want PROF_T", relationID, names[relationID])
	}

	pageWindowStart, err := interbase.ReadActivity(ctx, connA)
	if err != nil {
		t.Fatalf("pool A initial ReadActivity(): %v", err)
	}
	beforeB, err := interbase.ReadActivity(ctx, connB)
	if err != nil {
		t.Fatalf("pool B initial ReadActivity(): %v", err)
	}
	for scan := 0; scan < 5; scan++ {
		if count := countProfileRows(t, ctx, connB); count != 100 {
			t.Fatalf("pool B full scan %d returned %d rows, want 100", scan+1, count)
		}
	}
	afterB, err := interbase.ReadActivity(ctx, connB)
	if err != nil {
		t.Fatalf("pool B final ReadActivity(): %v", err)
	}
	afterBOnly, err := interbase.ReadActivity(ctx, connA)
	if err != nil {
		t.Fatalf("pool A snapshot after pool B scans: %v", err)
	}
	bWork, err := afterB.Delta(beforeB)
	if err != nil {
		t.Fatalf("pool B activity delta: %v", err)
	}
	if got := bWork.Tables[relationID].SeqReads; got != 500 {
		t.Fatalf("pool B PROF_T SeqReads delta = %d, want 500", got)
	}
	aPageDelta, err := afterBOnly.Delta(pageWindowStart)
	if err != nil {
		t.Fatalf("pool A page activity delta during pool B scans: %v", err)
	}
	t.Logf("pool B PROF_T work: %+v; pool B page delta: %+v; pool A page delta while pool A did no SQL: %+v",
		bWork.Tables[relationID], bWork.Pages, aPageDelta.Pages)
	if bWork.Pages == nil || bWork.Pages.Fetches == 0 {
		t.Fatalf("pool B page fetch delta = %+v, want attachment-scoped page counters with fetch activity", bWork.Pages)
	}
	if aPageDelta.Pages == nil {
		t.Fatal("pool A page delta is nil; expected attachment-scoped page counters")
	}
	if *aPageDelta.Pages != (interbase.PageCounters{}) {
		t.Fatalf("pool A page delta during pool B scans = %+v, want zero", *aPageDelta.Pages)
	}

	if count := countProfileRows(t, ctx, connA); count != 100 {
		t.Fatalf("pool A SELECT COUNT(*) returned %d, want 100", count)
	}
	afterA, err := interbase.ReadActivity(ctx, connA)
	if err != nil {
		t.Fatalf("pool A final ReadActivity(): %v", err)
	}
	aWork, err := afterA.Delta(pageWindowStart)
	if err != nil {
		t.Fatalf("pool A PROF_T activity delta: %v", err)
	}
	if got := aWork.Tables[relationID].SeqReads; got != 100 {
		t.Fatalf("pool A PROF_T SeqReads delta = %d, want 100 (pool B work must not appear)", got)
	}

	if _, err := connA.ExecContext(ctx, "CREATE PROCEDURE PROF_P AS DECLARE VARIABLE C INTEGER; BEGIN SELECT COUNT(*) FROM PROF_T INTO :C; END"); err != nil {
		t.Fatalf("create PROF_P: %v", err)
	}
	routinesBefore, err := interbase.ReadRoutineActivity(ctx, connA)
	if err != nil {
		if errors.Is(err, interbase.ErrRoutineActivityUnavailable) {
			t.Skipf("routine activity unavailable on this server/attachment: %v", err)
		}
		t.Fatalf("initial ReadRoutineActivity(): %v", err)
	}
	for invocation := 0; invocation < 2; invocation++ {
		if _, err := connA.ExecContext(ctx, "EXECUTE PROCEDURE PROF_P"); err != nil {
			t.Fatalf("execute PROF_P invocation %d: %v", invocation+1, err)
		}
	}
	routinesAfter, err := interbase.ReadRoutineActivity(ctx, connA)
	if err != nil {
		t.Fatalf("final ReadRoutineActivity(): %v", err)
	}
	routineDelta := routinesAfter.Delta(routinesBefore)
	procedureKey := interbase.RoutineKey{Kind: "procedure", Name: "PROF_P"}
	if got := routineDelta[procedureKey].Invocations; got < 2 {
		t.Fatalf("PROF_P invocation delta = %d, want at least 2 (routine delta: %+v)", got, routineDelta)
	}
}

func countProfileRows(t *testing.T, ctx context.Context, conn *sql.Conn) int64 {
	t.Helper()
	var count int64
	if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM PROF_T").Scan(&count); err != nil {
		t.Fatalf("SELECT COUNT(*) FROM PROF_T: %v", err)
	}
	return count
}
