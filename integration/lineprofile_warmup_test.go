//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	interbase "interbase-go"
)

// lpWarmUpRows is the row count of LP_WU_ROWS: wide rows, so the client's
// prefetch never fetches them all at once.
const lpWarmUpRows = 200

// lpWarmUpFixture creates the warm-up procedures in a fresh database.
func lpWarmUpFixture(t *testing.T, dialect int) (*sql.Conn, context.Context) {
	t.Helper()
	fixture, cfg, cleanup := createFixture(t, dialect, "")
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", dialect,
		interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
	ctx := readContextWithTimeout(t, 3*time.Minute)
	for _, statement := range []string{
		"CREATE GENERATOR LP_WU_GEN",
		"CREATE GENERATOR LP_WU_ROWS_GEN",
		"CREATE TABLE LP_WU_T (ID INTEGER)",
		"CREATE EXCEPTION LP_WU_ERR 'LP warm-up failure'",
		`CREATE PROCEDURE LP_WU_WRITE (N INTEGER) RETURNS (G INTEGER) AS
BEGIN
  G = GEN_ID(LP_WU_GEN, 1);
  INSERT INTO LP_WU_T (ID) VALUES (:N);
  UPDATE LP_WU_T SET ID = ID + 1 WHERE ID = :N;
END`,
		`CREATE PROCEDURE LP_WU_ROWS (N INTEGER) RETURNS (I INTEGER, PAD CHAR(1000)) AS
DECLARE VARIABLE G INTEGER;
BEGIN
  I = 0;
  PAD = 'x';
  WHILE (I < N) DO
  BEGIN
    I = I + 1;
    IF (I > 1) THEN G = GEN_ID(LP_WU_ROWS_GEN, 1);
    SUSPEND;
  END
END`,
		`CREATE PROCEDURE LP_WU_FAIL RETURNS (G INTEGER) AS
BEGIN
  G = GEN_ID(LP_WU_GEN, 1);
  EXCEPTION LP_WU_ERR;
END`,
		`CREATE PROCEDURE LP_WU_SLOW RETURNS (I INTEGER) AS
BEGIN
  I = 0;
  WHILE (I < 2000000000) DO
    I = I + 1;
END`,
	} {
		if _, err := db.ExecContext(ctx, statement); err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	conn, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	return conn, ctx
}

func lpGenerator(t *testing.T, ctx context.Context, conn *sql.Conn, name string) int64 {
	t.Helper()
	var value int64
	if err := conn.QueryRowContext(ctx, "SELECT GEN_ID("+name+", 0) FROM RDB$DATABASE").Scan(&value); err != nil {
		t.Fatalf("generator %s: %v", name, err)
	}
	return value
}

// lpRunCounted profiles query and returns the result and the generator's
// increase over the call.
func lpRunCounted(t *testing.T, ctx context.Context, conn *sql.Conn, generator string, opts interbase.LineProfileOptions) (interbase.LineProfileResult, int64) {
	t.Helper()
	before := lpGenerator(t, ctx, conn, generator)
	result, err := interbase.LineProfile(ctx, conn, opts)
	if err != nil {
		t.Fatalf("LineProfile(%s, warm-up %v): %v", opts.SQL, opts.WarmUp, err)
	}
	return result, lpGenerator(t, ctx, conn, generator) - before
}

// lpSameLineCounts fails unless both runs have the same lines with the same
// hits, rows, server calls, errors and record writes.
func lpSameLineCounts(t *testing.T, cold, warm interbase.LineProfileResult) {
	t.Helper()
	if len(cold.Routines) != len(warm.Routines) {
		t.Fatalf("routines %d vs %d", len(cold.Routines), len(warm.Routines))
	}
	for i := range cold.Routines {
		a, b := cold.Routines[i].Lines, warm.Routines[i].Lines
		if len(a) != len(b) {
			t.Fatalf("routine %d lines %v vs %v", i, a, b)
		}
		for line, m := range a {
			n, ok := b[line]
			if !ok || m.Hits != n.Hits || m.Rows != n.Rows || m.ServerCalls != n.ServerCalls || m.Errors != n.Errors {
				t.Fatalf("line %d: %+v vs %+v", line, m, n)
			}
			for id, tc := range m.Tables {
				wc := n.Tables[id]
				if tc.Inserts != wc.Inserts || tc.Updates != wc.Updates || tc.Deletes != wc.Deletes {
					t.Fatalf("line %d relation %d: %+v vs %+v", line, id, tc, wc)
				}
			}
		}
	}
	if cold.Rows != warm.Rows || cold.Outcome != warm.Outcome {
		t.Fatalf("rows %d/%d, outcome %s/%s", cold.Rows, warm.Rows, cold.Outcome, warm.Outcome)
	}
}

// TestLineProfileWarmUp checks ibg-3os: the warm-up executes the entry once
// natively in a rolled-back transaction before the measured run, which it
// leaves unchanged.
func TestLineProfileWarmUp(t *testing.T) {
	for _, dialect := range []int{3, 1} {
		t.Run(fmt.Sprintf("dialect %d", dialect), func(t *testing.T) {
			conn, ctx := lpWarmUpFixture(t, dialect)
			relation := lpRelationID(t, ctx, conn, "LP_WU_T")

			t.Run("executable writing entry", func(t *testing.T) {
				opts := interbase.LineProfileOptions{SQL: "EXECUTE PROCEDURE LP_WU_WRITE(7)", Dialect: dialect}
				cold, coldBumps := lpRunCounted(t, ctx, conn, "LP_WU_GEN", opts)
				opts.WarmUp = true
				warm, warmBumps := lpRunCounted(t, ctx, conn, "LP_WU_GEN", opts)
				if coldBumps != 1 || warmBumps != coldBumps+1 {
					t.Fatalf("generator +%d without warm-up, +%d with; want one more native execution", coldBumps, warmBumps)
				}
				if cold.Outcome != interbase.LineProfileCompleted || warm.WarmUp <= 0 || warm.WarmUpErr != nil || cold.WarmUp != 0 {
					t.Fatalf("cold %s/%v, warm-up %v / %v", cold.Outcome, cold.Err, warm.WarmUp, warm.WarmUpErr)
				}
				if !warm.RolledBack || warm.ReadOnly {
					t.Fatalf("rolled back %v, read-only %v", warm.RolledBack, warm.ReadOnly)
				}
				lpSameLineCounts(t, cold, warm)
				if tc := warm.OverheadActivity.Tables[relation]; tc.Inserts != 0 || tc.Updates != 0 {
					t.Fatalf("overhead counts the warm-up's writes: %+v", tc)
				}
				var rows int
				if err := conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM LP_WU_T").Scan(&rows); err != nil || rows != 0 {
					t.Fatalf("LP_WU_T rows %d / %v; want the writes rolled back", rows, err)
				}
			})

			t.Run("read-only warm-up cannot write", func(t *testing.T) {
				result, err := interbase.LineProfile(ctx, conn, interbase.LineProfileOptions{
					SQL: "EXECUTE PROCEDURE LP_WU_WRITE(8)", Dialect: dialect, ReadOnly: true, WarmUp: true,
				})
				if err != nil || result.WarmUpErr == nil {
					t.Fatalf("LineProfile = %v, warm-up error %v; want the read-only warm-up refused its write", err, result.WarmUpErr)
				}
				if result.Outcome != interbase.LineProfileFailed || result.FailedAt == nil ||
					result.FailedAt.Line != lpLine(t, result.Routines[0].Source, "INSERT INTO LP_WU_T") {
					t.Fatalf("measured run %s at %+v: %v", result.Outcome, result.FailedAt, result.Err)
				}
			})

			t.Run("selectable entry fetches every row", func(t *testing.T) {
				opts := interbase.LineProfileOptions{SQL: fmt.Sprintf("select * from LP_WU_ROWS(%d)", lpWarmUpRows), Dialect: dialect}
				cold, coldBumps := lpRunCounted(t, ctx, conn, "LP_WU_ROWS_GEN", opts)
				opts.WarmUp = true
				warm, warmBumps := lpRunCounted(t, ctx, conn, "LP_WU_ROWS_GEN", opts)
				if coldBumps != lpWarmUpRows-1 || warmBumps != 2*coldBumps {
					t.Fatalf("generator +%d without warm-up, +%d with; want +%d more", coldBumps, warmBumps, lpWarmUpRows-1)
				}
				if warm.WarmUp <= 0 || warm.WarmUpErr != nil || cold.Rows != lpWarmUpRows {
					t.Fatalf("warm-up %v / %v, rows %d", warm.WarmUp, warm.WarmUpErr, cold.Rows)
				}
				lpSameLineCounts(t, cold, warm)
			})

			t.Run("failing entry", func(t *testing.T) {
				result, bumps := lpRunCounted(t, ctx, conn, "LP_WU_GEN", interbase.LineProfileOptions{
					SQL: "EXECUTE PROCEDURE LP_WU_FAIL", Dialect: dialect, WarmUp: true,
				})
				if bumps != 2 || result.WarmUpErr == nil {
					t.Fatalf("generator +%d, warm-up error %v; want the warm-up's failure recorded", bumps, result.WarmUpErr)
				}
				if result.Outcome != interbase.LineProfileFailed || result.FailedAt == nil ||
					result.FailedAt.Line != lpLine(t, result.Routines[0].Source, "EXCEPTION LP_WU_ERR") {
					t.Fatalf("measured run %s at %+v: %v", result.Outcome, result.FailedAt, result.Err)
				}
			})

			t.Run("deadline during the warm-up", func(t *testing.T) {
				runCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
				defer cancel()
				result, err := interbase.LineProfile(runCtx, conn, interbase.LineProfileOptions{
					SQL: "EXECUTE PROCEDURE LP_WU_SLOW", Dialect: dialect, ReadOnly: true, WarmUp: true,
				})
				if err != nil || result.Outcome != interbase.LineProfileCanceled || !result.RolledBack {
					t.Fatalf("LineProfile = %v, outcome %s, rolled back %v: %v", err, result.Outcome, result.RolledBack, result.Err)
				}
				// The deadline also covers the analysis before the warm-up.
				if result.WarmUp < 200*time.Millisecond || result.Before.Samples != 0 || result.ServerCalls != 0 {
					t.Fatalf("warm-up %v, baseline %+v, server calls %d; want the call ended in the warm-up", result.WarmUp, result.Before, result.ServerCalls)
				}
				var one int
				if err := conn.QueryRowContext(ctx, "SELECT 1 FROM RDB$DATABASE").Scan(&one); err != nil || one != 1 {
					t.Fatalf("query after the deadline = %d, %v", one, err)
				}
			})
		})
	}
}
