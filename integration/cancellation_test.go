//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"os"
	"runtime"
	"strconv"
	"testing"
	"time"

	interbase "interbase-go"
)

const cancellationFixtureSchema = `
CREATE TABLE GO_CANCEL_SEED (ID INTEGER NOT NULL PRIMARY KEY);
INSERT INTO GO_CANCEL_SEED VALUES (1);
INSERT INTO GO_CANCEL_SEED VALUES (2);
INSERT INTO GO_CANCEL_SEED VALUES (3);
INSERT INTO GO_CANCEL_SEED VALUES (4);
INSERT INTO GO_CANCEL_SEED VALUES (5);
INSERT INTO GO_CANCEL_SEED VALUES (6);
INSERT INTO GO_CANCEL_SEED VALUES (7);
INSERT INTO GO_CANCEL_SEED VALUES (8);
INSERT INTO GO_CANCEL_SEED VALUES (9);
INSERT INTO GO_CANCEL_SEED VALUES (10);
INSERT INTO GO_CANCEL_SEED VALUES (11);
INSERT INTO GO_CANCEL_SEED VALUES (12);
INSERT INTO GO_CANCEL_SEED VALUES (13);
INSERT INTO GO_CANCEL_SEED VALUES (14);
INSERT INTO GO_CANCEL_SEED VALUES (15);
INSERT INTO GO_CANCEL_SEED VALUES (16);

CREATE TABLE GO_CANCEL_DELAY_ROWS (ID INTEGER NOT NULL PRIMARY KEY);
INSERT INTO GO_CANCEL_DELAY_ROWS (ID)
SELECT (S1.ID - 1) * 256 + (S2.ID - 1) * 16 + S3.ID
FROM GO_CANCEL_SEED S1
, GO_CANCEL_SEED S2
, GO_CANCEL_SEED S3;

CREATE TABLE GO_CANCEL_TARGET (
    ID INTEGER NOT NULL PRIMARY KEY,
    WRITE_VALUE INTEGER NOT NULL,
    LABEL VARCHAR(64) NOT NULL
);
CREATE TABLE GO_CANCEL_CONTROL (
    ID INTEGER NOT NULL PRIMARY KEY,
    LABEL VARCHAR(64) NOT NULL,
    TOKEN INTEGER NOT NULL
);
CREATE TABLE GO_CANCEL_LOG (
    LOG_ID INTEGER NOT NULL PRIMARY KEY,
    OPERATION VARCHAR(16) NOT NULL,
    TARGET_ID INTEGER NOT NULL,
    LABEL VARCHAR(64) NOT NULL
);

INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL)
VALUES (1, 10, 'initial');
CREATE GENERATOR GO_CANCEL_GENERATOR;
CREATE GENERATOR GO_CANCEL_RACE_GENERATOR;

SET TERM ^ ;
CREATE PROCEDURE GO_CANCEL_DELAY AS
DECLARE VARIABLE PASS_NUMBER INTEGER;
DECLARE VARIABLE DELAY_VALUE INTEGER;
BEGIN
    PASS_NUMBER = 0;
    WHILE (PASS_NUMBER < 10000) DO
    BEGIN
        FOR SELECT ID FROM GO_CANCEL_DELAY_ROWS INTO :DELAY_VALUE DO
        BEGIN
            DELAY_VALUE = DELAY_VALUE + 1;
        END
        PASS_NUMBER = PASS_NUMBER + 1;
    END
END^

CREATE PROCEDURE GO_CANCEL_MAYBE_DELAY (DELAY_ENABLED INTEGER) AS
BEGIN
    IF (DELAY_ENABLED = 1) THEN
        EXECUTE PROCEDURE GO_CANCEL_DELAY;
END^

CREATE PROCEDURE GO_CANCEL_PARTIAL_MUTATION (FIRST_ID INTEGER, SECOND_ID INTEGER) AS
BEGIN
    INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN)
    VALUES (:FIRST_ID, 'partial first mutation', GEN_ID(GO_CANCEL_GENERATOR, 1));
    EXECUTE PROCEDURE GO_CANCEL_DELAY;
    INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL)
    VALUES (:SECOND_ID, 200, 'partial second mutation');
END^

CREATE PROCEDURE GO_CANCEL_RACE_DELAY AS
DECLARE VARIABLE RACE_TOKEN INTEGER;
BEGIN
    RACE_TOKEN = GEN_ID(GO_CANCEL_RACE_GENERATOR, 1);
    EXECUTE PROCEDURE GO_CANCEL_DELAY;
END^

CREATE PROCEDURE GO_CANCEL_RACE_COMPLETE AS
DECLARE VARIABLE RACE_TOKEN INTEGER;
BEGIN
    RACE_TOKEN = GEN_ID(GO_CANCEL_RACE_GENERATOR, 1);
END^

CREATE TRIGGER GO_CANCEL_TARGET_BI FOR GO_CANCEL_TARGET
ACTIVE BEFORE INSERT POSITION 0
AS
DECLARE VARIABLE LOG_ID INTEGER;
BEGIN
    LOG_ID = GEN_ID(GO_CANCEL_GENERATOR, 1);
    EXECUTE PROCEDURE GO_CANCEL_DELAY;
    INSERT INTO GO_CANCEL_LOG (LOG_ID, OPERATION, TARGET_ID, LABEL)
    VALUES (:LOG_ID, 'insert', NEW.ID, NEW.LABEL);
END^

CREATE TRIGGER GO_CANCEL_TARGET_BU FOR GO_CANCEL_TARGET
ACTIVE BEFORE UPDATE POSITION 0
AS
DECLARE VARIABLE LOG_ID INTEGER;
BEGIN
    LOG_ID = GEN_ID(GO_CANCEL_GENERATOR, 1);
    EXECUTE PROCEDURE GO_CANCEL_DELAY;
    INSERT INTO GO_CANCEL_LOG (LOG_ID, OPERATION, TARGET_ID, LABEL)
    VALUES (:LOG_ID, 'update', NEW.ID, NEW.LABEL);
END^

CREATE TRIGGER GO_CANCEL_TARGET_BD FOR GO_CANCEL_TARGET
ACTIVE BEFORE DELETE POSITION 0
AS
DECLARE VARIABLE LOG_ID INTEGER;
BEGIN
    LOG_ID = GEN_ID(GO_CANCEL_GENERATOR, 1);
    EXECUTE PROCEDURE GO_CANCEL_DELAY;
    INSERT INTO GO_CANCEL_LOG (LOG_ID, OPERATION, TARGET_ID, LABEL)
    VALUES (:LOG_ID, 'delete', OLD.ID, OLD.LABEL);
END^
SET TERM ; ^
COMMIT;
`

const (
	cancellationOptIn         = "INTERBASE_CANCELLATION"
	cancellationStartDelay    = 250 * time.Millisecond
	cancellationResultLimit   = 30 * time.Second
	cancellationIterationsEnv = "INTERBASE_CANCELLATION_ITERATIONS"
	nativeCancelledStatus     = int64(335544794)
)

func requireLiveCancellation(t *testing.T) {
	t.Helper()
	if os.Getenv(cancellationOptIn) != "1" {
		t.Skipf("set %s=1 to run live DSQL cancellation contracts", cancellationOptIn)
	}
}

func cancellationIterations(t *testing.T) int {
	t.Helper()
	value := os.Getenv(cancellationIterationsEnv)
	if value == "" {
		return 10
	}
	iterations, err := strconv.Atoi(value)
	if err != nil || iterations < 1 || iterations > 10000 {
		t.Fatalf("%s = %q is invalid; want an integer in [1, 10000]", cancellationIterationsEnv, value)
	}
	return iterations
}

// TestLiveCancellationRaces repeats the live paths that can overlap a DSQL
// call. The runner supplies a process-level deadline because native cancel is
// best effort rather than a deadline guarantee.
func TestLiveCancellationRaces(t *testing.T) {
	requireLiveCancellation(t)
	iterations := cancellationIterations(t)
	fixture, cfg, cleanup := createFixture(t, 3, cancellationFixtureSchema)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3, interbase.TransactionOptions{})
	verifier := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3, interbase.TransactionOptions{})
	db.SetMaxOpenConns(3)
	db.SetMaxIdleConns(3)
	beforeGoroutines := runtime.NumGoroutine()
	beforeFDs, fdsAvailable := openFileDescriptorCount()

	const cpuQuery = "EXECUTE PROCEDURE GO_CANCEL_DELAY"
	for iteration := 0; iteration < iterations; iteration++ {
		t.Run("iteration", func(t *testing.T) {
			// Direct CPU query cancellation.
			runCanceledRead(t, func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, cpuQuery)
				return err
			})
			runCompletionWinningRace(t, db, verifier)
			runCancellationWinningRace(t, db, verifier)

			prepared, err := db.PrepareContext(context.Background(), "EXECUTE PROCEDURE GO_CANCEL_MAYBE_DELAY(?)")
			if err != nil {
				t.Fatalf("prepare repeated CPU query: %v", err)
			}
			preparedClosed := false
			t.Cleanup(func() {
				if !preparedClosed {
					_ = prepared.Close()
				}
			})
			runCanceledRead(t, func(ctx context.Context) error {
				_, err := prepared.ExecContext(ctx, 1)
				return err
			})
			if _, err := prepared.ExecContext(context.Background(), 0); err != nil {
				t.Fatalf("reuse prepared statement after cancellation: %v", err)
			}
			if err := prepared.Close(); err != nil {
				t.Fatalf("close canceled prepared query: %v", err)
			}
			preparedClosed = true

			runCanceledRead(t, func(ctx context.Context) error {
				rows, err := db.QueryContext(ctx, "SELECT A.ID FROM GO_CANCEL_DELAY_ROWS A, GO_CANCEL_DELAY_ROWS B")
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var id int64
					if err := rows.Scan(&id); err != nil {
						return err
					}
				}
				return rows.Err()
			})

			id := int64(iteration + 100)
			runCanceledWrite(t, func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, "INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)", id, id, "repeated implicit cancellation")
				return err
			})
			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
			defer verifyCancel()
			assertCancellationTarget(t, verifyCtx, verifier, id, false, 0, "")

			tx, err := db.BeginTx(context.Background(), nil)
			if err != nil {
				t.Fatalf("begin repeated explicit transaction: %v", err)
			}
			transactionFinished := false
			t.Cleanup(func() {
				if !transactionFinished {
					_ = tx.Rollback()
				}
			})
			runCanceledWrite(t, func(ctx context.Context) error {
				_, err := tx.ExecContext(ctx, "INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)", id+10000, id, "repeated explicit cancellation")
				return err
			})
			assertCanceledTransactionTargetAbsent(t, verifyCtx, tx, id+10000)
			if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback repeated explicit transaction: %v", err)
			}
			transactionFinished = true
			insertCancellationControl(t, verifyCtx, db, id, "pool recovery")
		})
	}
	if stats := db.Stats(); stats.InUse != 0 {
		t.Fatalf("cancellation pool retained in-use connections: %+v", stats)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close cancellation pool: %v", err)
	}
	if err := verifier.Close(); err != nil {
		t.Fatalf("close cancellation verifier pool: %v", err)
	}
	if stats := db.Stats(); stats.OpenConnections != 0 || stats.InUse != 0 || stats.Idle != 0 {
		t.Fatalf("cancellation pool remained open after cleanup: %+v", stats)
	}
	runtime.GC()
	if got := runtime.NumGoroutine(); got > beforeGoroutines {
		t.Fatalf("goroutines after repeated cancellation = %d, baseline = %d", got, beforeGoroutines)
	}
	if fdsAvailable {
		if got, available := openFileDescriptorCount(); available && got > beforeFDs {
			t.Fatalf("file descriptors after repeated cancellation = %d, baseline = %d", got, beforeFDs)
		}
	}
}

func TestCanceledFetchReturnsContextCancellation(t *testing.T) {
	requireLiveCancellation(t)

	fixture, cfg, cleanup := createFixture(t, 3, cancellationFixtureSchema)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rows, err := db.QueryContext(ctx,
		"SELECT A.ID FROM GO_CANCEL_DELAY_ROWS A, GO_CANCEL_DELAY_ROWS B")
	if err != nil {
		t.Fatalf("start large fetch: %v", err)
	}
	result := make(chan error, 1)
	go func() {
		defer rows.Close()
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				result <- err
				return
			}
		}
		result <- rows.Err()
	}()

	timer := time.NewTimer(cancellationStartDelay)
	defer timer.Stop()
	select {
	case err := <-result:
		if err == nil {
			t.Fatal("large fetch completed before cancellation")
		}
		runCanceledReadError(t, err)
	case <-timer.C:
		cancel()
		select {
		case err := <-result:
			runCanceledReadError(t, err)
		case <-time.After(cancellationResultLimit):
			t.Fatal("canceled fetch did not return before its live bound")
		}
	}

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
	defer verifyCancel()
	var count int64
	if err := db.QueryRowContext(verifyCtx,
		"SELECT COUNT(*) FROM GO_CANCEL_SEED").Scan(&count); err != nil || count != 16 {
		t.Fatalf("connection reuse after canceled fetch = (%d, %v), want (16, nil)", count, err)
	}
}

func TestCanceledRowLockWaitReturnsNativeCancellation(t *testing.T) {
	requireLiveCancellation(t)

	fixture, cfg, cleanup := createFixture(t, 3, cancellationFixtureSchema)
	locker := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	contender := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	ctx := context.Background()
	lockerTx, err := locker.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin row-lock owner: %v", err)
	}
	lockerDone := false
	t.Cleanup(func() {
		if !lockerDone {
			_ = lockerTx.Rollback()
		}
	})
	if _, err := lockerTx.ExecContext(ctx,
		"UPDATE GO_CANCEL_TARGET SET WRITE_VALUE = ?, LABEL = ? WHERE ID = ?",
		int64(11), "row-lock owner", int64(1)); err != nil {
		t.Fatalf("acquire row lock: %v", err)
	}

	contenderTx, err := contender.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin row-lock contender: %v", err)
	}
	contenderDone := false
	t.Cleanup(func() {
		if !contenderDone {
			_ = contenderTx.Rollback()
		}
	})
	runCanceledWrite(t, func(cancelCtx context.Context) error {
		_, err := contenderTx.ExecContext(cancelCtx,
			"UPDATE GO_CANCEL_TARGET SET WRITE_VALUE = ?, LABEL = ? WHERE ID = ?",
			int64(12), "canceled row-lock contender", int64(1))
		return err
	})

	assertCancellationTargetTx(t, ctx, contenderTx, 1, true, 10, "initial")
	if _, err := contenderTx.ExecContext(ctx,
		"INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN) VALUES (?, ?, GEN_ID(GO_CANCEL_GENERATOR, 1))",
		int64(100), "after row-lock cancellation"); err != nil {
		t.Fatalf("row-lock contender transaction was not reusable: %v", err)
	}
	if err := contenderTx.Rollback(); err != nil {
		t.Fatalf("rollback row-lock contender: %v", err)
	}
	contenderDone = true
	if err := lockerTx.Rollback(); err != nil {
		t.Fatalf("rollback row-lock owner: %v", err)
	}
	lockerDone = true

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
	defer verifyCancel()
	assertCancellationTarget(t, verifyCtx, contender, 1, true, 10, "initial")
}

func TestCanceledPartialProcedureIsAtomic(t *testing.T) {
	requireLiveCancellation(t)

	db, verifier := newCancellationDatabases(t)
	const firstID = int64(100)
	const secondID = int64(20)
	runCanceledWrite(t, func(ctx context.Context) error {
		_, err := db.ExecContext(ctx,
			"EXECUTE PROCEDURE GO_CANCEL_PARTIAL_MUTATION(?, ?)", firstID, secondID)
		return err
	})

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
	defer verifyCancel()
	if got := queryCancellationCount(t, verifyCtx, verifier,
		"SELECT COUNT(*) FROM GO_CANCEL_CONTROL WHERE ID = ?", firstID); got != 0 {
		t.Fatalf("partial procedure first mutation rows = %d, want 0", got)
	}
	assertCancellationTarget(t, verifyCtx, verifier, secondID, false, 0, "")
	assertCancellationLogEmpty(t, verifyCtx, verifier)
}

func TestCanceledPartialProcedurePreservesExplicitTransaction(t *testing.T) {
	requireLiveCancellation(t)

	db, verifier := newCancellationDatabases(t)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("begin partial explicit transaction: %v", err)
	}
	transactionDone := false
	t.Cleanup(func() {
		if !transactionDone {
			_ = tx.Rollback()
		}
	})
	insertCancellationControlTx(t, context.Background(), tx, 1, "before partial procedure")
	runCanceledWrite(t, func(ctx context.Context) error {
		_, err := tx.ExecContext(ctx,
			"EXECUTE PROCEDURE GO_CANCEL_PARTIAL_MUTATION(?, ?)", int64(101), int64(21))
		return err
	})
	if got := queryCancellationCountTx(t, context.Background(), tx,
		"SELECT COUNT(*) FROM GO_CANCEL_CONTROL WHERE ID = ?", int64(101)); got != 0 {
		t.Fatalf("explicit partial first mutation rows = %d, want 0", got)
	}
	assertCancellationTargetTx(t, context.Background(), tx, 21, false, 0, "")
	insertCancellationControlTx(t, context.Background(), tx, 2, "after partial procedure")
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit explicit partial transaction: %v", err)
	}
	transactionDone = true

	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
	defer verifyCancel()
	if got := queryCancellationCount(t, verifyCtx, verifier,
		"SELECT COUNT(*) FROM GO_CANCEL_CONTROL"); got != 2 {
		t.Fatalf("explicit partial control rows = %d, want 2", got)
	}
	assertCancellationTarget(t, verifyCtx, verifier, 21, false, 0, "")
	assertCancellationLogEmpty(t, verifyCtx, verifier)
}

func runCompletionWinningRace(t *testing.T, db, verifier *sql.DB) {
	t.Helper()
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
	defer verifyCancel()
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	removeHook := interbase.SetNativeDSQLCompletionTestHook(entered, release)
	t.Cleanup(removeHook)
	before := queryCancellationCount(t, verifyCtx, verifier,
		"SELECT GEN_ID(GO_CANCEL_RACE_GENERATOR, 0) FROM RDB$DATABASE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, "EXECUTE PROCEDURE GO_CANCEL_RACE_COMPLETE")
		result <- err
	}()
	select {
	case <-entered:
	case <-time.After(cancellationResultLimit):
		t.Fatal("completion-winning race did not reach the post-DSQL boundary")
	}
	cancel()
	close(release)
	removeHook()
	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("completion-winning race error = %v, want successful native result", err)
		}
	case <-time.After(cancellationResultLimit):
		t.Fatal("completion-winning race did not return before its live bound")
	}
	if got := queryCancellationCount(t, verifyCtx, verifier,
		"SELECT GEN_ID(GO_CANCEL_RACE_GENERATOR, 0) FROM RDB$DATABASE"); got != before+1 {
		t.Fatalf("completion-winning non-idempotent marker = %d, want %d", got, before+1)
	}
	var count int64
	if err := db.QueryRowContext(verifyCtx, "SELECT COUNT(*) FROM GO_CANCEL_SEED").Scan(&count); err != nil || count != 16 {
		t.Fatalf("connection reuse after completion-winning race = (%d, %v), want (16, nil)", count, err)
	}
}

func runCancellationWinningRace(t *testing.T, db, verifier *sql.DB) {
	t.Helper()
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), cancellationResultLimit)
	defer verifyCancel()
	before := queryCancellationCount(t, verifyCtx, verifier,
		"SELECT GEN_ID(GO_CANCEL_RACE_GENERATOR, 0) FROM RDB$DATABASE")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, "EXECUTE PROCEDURE GO_CANCEL_RACE_DELAY")
		result <- err
	}()
	waitForCancellationRaceMarker(t, verifyCtx, verifier, before+1)
	cancel()
	select {
	case err := <-result:
		runCanceledReadError(t, err)
	case <-time.After(cancellationResultLimit):
		t.Fatal("native-overlap cancellation race did not return before its live bound")
	}
	var count int64
	if err := db.QueryRowContext(verifyCtx, "SELECT COUNT(*) FROM GO_CANCEL_SEED").Scan(&count); err != nil || count != 16 {
		t.Fatalf("connection reuse after cancellation-winning race = (%d, %v), want (16, nil)", count, err)
	}
}

func waitForCancellationRaceMarker(t *testing.T, ctx context.Context, db *sql.DB, want int64) {
	t.Helper()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		got := queryCancellationCount(t, ctx, db, "SELECT GEN_ID(GO_CANCEL_RACE_GENERATOR, 0) FROM RDB$DATABASE")
		if got == want {
			return
		}
		if got > want {
			t.Fatalf("cancellation race marker = %d, want %d", got, want)
		}
		select {
		case <-ctx.Done():
			t.Fatalf("wait for cancellation race marker: %v", ctx.Err())
		case <-ticker.C:
		}
	}
}

func newCancellationDatabases(t *testing.T) (*sql.DB, *sql.DB) {
	t.Helper()
	fixture, cfg, cleanup := createFixture(t, 3, cancellationFixtureSchema)
	writer := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", 3, interbase.TransactionOptions{})
	verifier := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", 3, interbase.TransactionOptions{})
	return writer, verifier
}

func runCanceledWrite(t *testing.T, execute func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	result := make(chan error, 1)
	go func() {
		result <- execute(ctx)
	}()

	timer := time.NewTimer(cancellationStartDelay)
	defer timer.Stop()
	select {
	case err := <-result:
		if err == nil {
			t.Fatalf("mutating operation completed before cancellation request")
		}
		assertCanceledWriteError(t, err)
		return
	case <-timer.C:
		cancel()
	}

	select {
	case err := <-result:
		assertCanceledWriteError(t, err)
	case <-time.After(cancellationResultLimit):
		t.Fatal("canceled mutating operation did not return before its live bound")
	}
}

func runCanceledRead(t *testing.T, execute func(context.Context) error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { result <- execute(ctx) }()
	timer := time.NewTimer(cancellationStartDelay)
	defer timer.Stop()
	select {
	case err := <-result:
		if err == nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled read returned %v, want context cancellation", err)
		}
		return
	case <-timer.C:
		cancel()
	}
	select {
	case err := <-result:
		runCanceledReadError(t, err)
	case <-time.After(cancellationResultLimit):
		t.Fatal("canceled read did not return before its live bound")
	}
}

func runCanceledReadError(t *testing.T, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, context.Canceled) || errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("canceled read returned %v, want non-bad-connection context cancellation", err)
	}
}

func assertCanceledWriteError(t *testing.T, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("canceled write returned nil error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write error = %v, want context.Canceled", err)
	}
	if errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("canceled write error = %v, must not match driver.ErrBadConn", err)
	}
	var cancellationErr *interbase.CancellationError
	if !errors.As(err, &cancellationErr) {
		t.Fatalf("canceled write error = %v, want CancellationError", err)
	}
	var nativeErr *interbase.NativeError
	if !errors.As(err, &nativeErr) {
		t.Fatalf("canceled write error = %v, want native cancellation diagnostic", err)
	}
	if nativeErr.NativeCode != nativeCancelledStatus {
		t.Fatalf("canceled write native code = %d, want isc_cancelled (%d)",
			nativeErr.NativeCode, nativeCancelledStatus)
	}
	var uncertainErr *interbase.UncertainOutcomeError
	if errors.As(err, &uncertainErr) {
		t.Fatalf("healthy canceled write was uncertain: %v", err)
	}
}

func queryCancellationCount(t *testing.T, ctx context.Context, db *sql.DB, query string,
	args ...any) int64 {
	t.Helper()
	var count int64
	if err := db.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return count
}

func queryCancellationGenerator(t *testing.T, ctx context.Context, db *sql.DB) int64 {
	t.Helper()
	return queryCancellationCount(t, ctx, db,
		"SELECT GEN_ID(GO_CANCEL_GENERATOR, 0) FROM RDB$DATABASE")
}

func assertCancellationTarget(t *testing.T, ctx context.Context, db *sql.DB, id int64,
	wantExists bool, wantValue int64, wantLabel string) {
	t.Helper()
	var value int64
	var label string
	err := db.QueryRowContext(ctx,
		"SELECT WRITE_VALUE, LABEL FROM GO_CANCEL_TARGET WHERE ID = ?", id).
		Scan(&value, &label)
	if !wantExists {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("target %d query error = %v, want sql.ErrNoRows", id, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("target %d query: %v", id, err)
	}
	if value != wantValue || label != wantLabel {
		t.Fatalf("target %d = (%d, %q), want (%d, %q)",
			id, value, label, wantValue, wantLabel)
	}
}

func assertCancellationTargetTx(t *testing.T, ctx context.Context, tx *sql.Tx, id int64,
	wantExists bool, wantValue int64, wantLabel string) {
	t.Helper()
	var value int64
	var label string
	err := tx.QueryRowContext(ctx,
		"SELECT WRITE_VALUE, LABEL FROM GO_CANCEL_TARGET WHERE ID = ?", id).
		Scan(&value, &label)
	if !wantExists {
		if !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("transaction target %d query error = %v, want sql.ErrNoRows", id, err)
		}
		return
	}
	if err != nil {
		t.Fatalf("transaction target %d query: %v", id, err)
	}
	if value != wantValue || label != wantLabel {
		t.Fatalf("transaction target %d = (%d, %q), want (%d, %q)",
			id, value, label, wantValue, wantLabel)
	}
}

func assertCancellationLogEmpty(t *testing.T, ctx context.Context, db *sql.DB) {
	t.Helper()
	if got := queryCancellationCount(t, ctx, db,
		"SELECT COUNT(*) FROM GO_CANCEL_LOG"); got != 0 {
		t.Fatalf("cancellation log rows = %d, want 0", got)
	}
}

func insertCancellationControl(t *testing.T, ctx context.Context, db *sql.DB, id int64,
	label string) {
	t.Helper()
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN) VALUES (?, ?, GEN_ID(GO_CANCEL_GENERATOR, 1))",
		id, label); err != nil {
		t.Fatalf("control insert %d: %v", id, err)
	}
}

func insertCancellationControlTx(t *testing.T, ctx context.Context, tx *sql.Tx, id int64,
	label string) {
	t.Helper()
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN) VALUES (?, ?, GEN_ID(GO_CANCEL_GENERATOR, 1))",
		id, label); err != nil {
		t.Fatalf("transaction control insert %d: %v", id, err)
	}
}

// TestCanceledImplicitDMLRollsBackAndDoesNotReplay uses the generator as a
// non-transactional execution marker. The trigger increments it once before
// entering the expensive procedure; the target row and log row are both
// transactional and must disappear with the driver's implicit rollback.
func TestCanceledImplicitDMLRollsBackAndDoesNotReplay(t *testing.T) {
	requireLiveCancellation(t)

	tests := []struct {
		name       string
		query      string
		args       []any
		targetID   int64
		wantExists bool
		wantValue  int64
		wantLabel  string
	}{
		{
			name:       "insert",
			query:      "INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			args:       []any{int64(2), int64(20), "canceled insert"},
			targetID:   2,
			wantExists: false,
		},
		{
			name:       "update",
			query:      "UPDATE GO_CANCEL_TARGET SET WRITE_VALUE = ?, LABEL = ? WHERE ID = ?",
			args:       []any{int64(20), "canceled update", int64(1)},
			targetID:   1,
			wantExists: true,
			wantValue:  10,
			wantLabel:  "initial",
		},
		{
			name:       "delete",
			query:      "DELETE FROM GO_CANCEL_TARGET WHERE ID = ?",
			args:       []any{int64(1)},
			targetID:   1,
			wantExists: true,
			wantValue:  10,
			wantLabel:  "initial",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			db, verifier := newCancellationDatabases(t)
			runCanceledWrite(t, func(ctx context.Context) error {
				_, err := db.ExecContext(ctx, test.query, test.args...)
				return err
			})

			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer verifyCancel()
			assertCancellationTarget(t, verifyCtx, verifier, test.targetID,
				test.wantExists, test.wantValue, test.wantLabel)
			assertCancellationLogEmpty(t, verifyCtx, verifier)
			if got := queryCancellationGenerator(t, verifyCtx, verifier); got != 1 {
				t.Fatalf("generator after one canceled %s = %d, want exactly one execution marker",
					test.name, got)
			}

			// The same pooled attachment must remain usable, and this succeeding
			// write provides a second non-idempotent marker without replaying the
			// canceled statement.
			insertCancellationControl(t, verifyCtx, db, 1, "after canceled "+test.name)
			if got := queryCancellationGenerator(t, verifyCtx, verifier); got != 2 {
				t.Fatalf("generator after recovery write = %d, want 2", got)
			}
			if got := queryCancellationCount(t, verifyCtx, verifier,
				"SELECT COUNT(*) FROM GO_CANCEL_CONTROL WHERE ID = 1"); got != 1 {
				t.Fatalf("recovery control rows = %d, want 1", got)
			}
		})
	}
}

func assertCanceledTransactionTargetAbsent(t *testing.T, ctx context.Context, tx *sql.Tx,
	id int64) {
	t.Helper()
	if got := queryCancellationCountTx(t, ctx, tx,
		"SELECT COUNT(*) FROM GO_CANCEL_TARGET WHERE ID = ?", id); got != 0 {
		t.Fatalf("transaction target %d rows after cancellation = %d, want 0", id, got)
	}
	if got := queryCancellationCountTx(t, ctx, tx,
		"SELECT COUNT(*) FROM GO_CANCEL_LOG"); got != 0 {
		t.Fatalf("transaction cancellation log rows = %d, want 0", got)
	}
}

func queryCancellationCountTx(t *testing.T, ctx context.Context, tx *sql.Tx, query string,
	args ...any) int64 {
	t.Helper()
	var count int64
	if err := tx.QueryRowContext(ctx, query, args...).Scan(&count); err != nil {
		t.Fatalf("transaction query %q: %v", query, err)
	}
	return count
}

func TestCanceledExplicitWritePreservesTransactionOwnership(t *testing.T) {
	requireLiveCancellation(t)

	for _, test := range []struct {
		name        string
		commit      bool
		wantControl int64
	}{
		{name: "commit", commit: true, wantControl: 2},
		{name: "rollback", commit: false, wantControl: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			db, verifier := newCancellationDatabases(t)
			ctx := context.Background()
			tx, err := db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin explicit transaction: %v", err)
			}
			registerTransactionRollback(t, tx)

			insertCancellationControlTx(t, ctx, tx, 1, "before canceled write")
			runCanceledWrite(t, func(cancelCtx context.Context) error {
				_, err := tx.ExecContext(cancelCtx,
					"INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
					int64(10), int64(100), "canceled explicit insert")
				return err
			})
			assertCanceledTransactionTargetAbsent(t, ctx, tx, 10)
			if got := queryCancellationCountTx(t, ctx, tx,
				"SELECT GEN_ID(GO_CANCEL_GENERATOR, 0) FROM RDB$DATABASE"); got != 2 {
				t.Fatalf("transaction generator after canceled write = %d, want 2", got)
			}

			insertCancellationControlTx(t, ctx, tx, 2, "after canceled write")
			if got := queryCancellationCountTx(t, ctx, tx,
				"SELECT GEN_ID(GO_CANCEL_GENERATOR, 0) FROM RDB$DATABASE"); got != 3 {
				t.Fatalf("transaction generator after later write = %d, want 3", got)
			}
			if test.commit {
				if err := tx.Commit(); err != nil {
					t.Fatalf("commit usable explicit transaction: %v", err)
				}
			} else if err := tx.Rollback(); err != nil {
				t.Fatalf("rollback usable explicit transaction: %v", err)
			}

			verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer verifyCancel()
			if got := queryCancellationCount(t, verifyCtx, verifier,
				"SELECT COUNT(*) FROM GO_CANCEL_CONTROL"); got != test.wantControl {
				t.Fatalf("persisted control rows = %d, want %d", got, test.wantControl)
			}
			assertCancellationTarget(t, verifyCtx, verifier, 10, false, 0, "")
			assertCancellationLogEmpty(t, verifyCtx, verifier)
			if got := queryCancellationGenerator(t, verifyCtx, verifier); got != 3 {
				t.Fatalf("persisted generator after %s = %d, want 3", test.name, got)
			}
		})
	}
}

func queryDirectCancellationCount(t *testing.T, ctx context.Context, tx *interbase.Transaction,
	query string, args ...any) int64 {
	t.Helper()
	cursor, err := tx.Query(ctx, query, args...)
	if err != nil {
		t.Fatalf("direct query %q: %v", query, err)
	}
	defer func() {
		if err := cursor.Close(); err != nil {
			t.Errorf("close direct query %q: %v", query, err)
		}
	}()
	hasRow, err := cursor.Next(ctx)
	if err != nil {
		t.Fatalf("direct next %q: %v", query, err)
	}
	if !hasRow {
		t.Fatalf("direct query %q returned no row", query)
	}
	row, err := cursor.Row()
	if err != nil {
		t.Fatalf("direct row %q: %v", query, err)
	}
	if len(row) != 1 {
		t.Fatalf("direct query %q returned %d columns, want 1", query, len(row))
	}
	value, ok := row[0].Value.(int64)
	if !ok {
		t.Fatalf("direct query %q returned %T, want int64", query, row[0].Value)
	}
	return value
}

func TestCanceledDistributedParticipantRemainsUsable(t *testing.T) {
	requireLiveCancellation(t)

	firstFixture, firstConfig, firstCleanup := createFixture(t, 3, cancellationFixtureSchema)
	secondFixture, secondConfig, secondCleanup := createFixture(t, 3, cancellationFixtureSchema)
	first := openDirectAttachment(t, firstCleanup, firstFixture.ConnectionString(), firstConfig.User,
		firstConfig.Password, "", "", 3, interbase.TransactionOptions{})
	second := openDirectAttachment(t, secondCleanup, secondFixture.ConnectionString(), secondConfig.User,
		secondConfig.Password, "", "", 3, interbase.TransactionOptions{})

	ctx := context.Background()
	distributed, err := interbase.BeginDistributed(ctx, []interbase.Participant{
		{Attachment: first, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
		{Attachment: second, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
	})
	if err != nil {
		t.Fatalf("begin distributed transaction: %v", err)
	}
	t.Cleanup(func() { _ = distributed.Rollback(context.Background()) })

	firstTx, err := distributed.Participant(0)
	if err != nil {
		t.Fatalf("first distributed participant: %v", err)
	}
	secondTx, err := distributed.Participant(1)
	if err != nil {
		t.Fatalf("second distributed participant: %v", err)
	}

	if _, err := firstTx.Exec(ctx,
		"INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN) VALUES (?, ?, GEN_ID(GO_CANCEL_GENERATOR, 1))",
		int64(1), "before distributed cancellation"); err != nil {
		t.Fatalf("first participant control write: %v", err)
	}
	runCanceledWrite(t, func(cancelCtx context.Context) error {
		_, err := firstTx.Exec(cancelCtx,
			"INSERT INTO GO_CANCEL_TARGET (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			int64(10), int64(100), "canceled distributed insert")
		return err
	})

	if got := queryDirectCancellationCount(t, ctx, firstTx,
		"SELECT COUNT(*) FROM GO_CANCEL_TARGET WHERE ID = ?", int64(10)); got != 0 {
		t.Fatalf("first participant target rows after cancellation = %d, want 0", got)
	}
	if _, err := firstTx.Exec(ctx,
		"INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN) VALUES (?, ?, GEN_ID(GO_CANCEL_GENERATOR, 1))",
		int64(2), "after distributed cancellation"); err != nil {
		t.Fatalf("first participant write after cancellation: %v", err)
	}
	if _, err := secondTx.Exec(ctx,
		"INSERT INTO GO_CANCEL_CONTROL (ID, LABEL, TOKEN) VALUES (?, ?, GEN_ID(GO_CANCEL_GENERATOR, 1))",
		int64(1), "second participant write"); err != nil {
		t.Fatalf("second participant write: %v", err)
	}

	if err := distributed.Prepare(ctx, []byte("canceled-participant-live-recovery")); err != nil {
		t.Fatalf("prepare after canceled participant write: %v", err)
	}
	if err := distributed.Commit(ctx); err != nil {
		t.Fatalf("commit after canceled participant write: %v", err)
	}

	firstDB := openDatabase(t, firstCleanup, firstFixture.ConnectionString(), firstConfig.User,
		firstConfig.Password, "", 3, interbase.TransactionOptions{})
	secondDB := openDatabase(t, secondCleanup, secondFixture.ConnectionString(), secondConfig.User,
		secondConfig.Password, "", 3, interbase.TransactionOptions{})
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer verifyCancel()

	if got := queryCancellationCount(t, verifyCtx, firstDB,
		"SELECT COUNT(*) FROM GO_CANCEL_CONTROL"); got != 2 {
		t.Fatalf("first distributed control rows = %d, want 2", got)
	}
	assertCancellationTarget(t, verifyCtx, firstDB, 10, false, 0, "")
	assertCancellationLogEmpty(t, verifyCtx, firstDB)
	if got := queryCancellationGenerator(t, verifyCtx, firstDB); got != 3 {
		t.Fatalf("first distributed generator = %d, want 3", got)
	}
	if got := queryCancellationCount(t, verifyCtx, secondDB,
		"SELECT COUNT(*) FROM GO_CANCEL_CONTROL"); got != 1 {
		t.Fatalf("second distributed control rows = %d, want 1", got)
	}
	if got := queryCancellationGenerator(t, verifyCtx, secondDB); got != 1 {
		t.Fatalf("second distributed generator = %d, want 1", got)
	}
}
