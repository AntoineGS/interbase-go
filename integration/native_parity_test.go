//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	interbase "interbase-go"
)

var integrationIdentifierCounter atomic.Uint64

func uniqueIntegrationIdentifier(prefix string) string {
	return fmt.Sprintf("GO_%s_%x_%x", prefix, os.Getpid(), integrationIdentifierCounter.Add(1))
}

func uniqueIntegrationPassword() string {
	return fmt.Sprintf("GoPass%x%x", os.Getpid(), integrationIdentifierCounter.Add(1))
}

func requireNativeError(t *testing.T, err error) *interbase.Error {
	t.Helper()
	if err == nil {
		t.Fatal("expected an InterBase error")
	}
	var nativeErr *interbase.Error
	if !errors.As(err, &nativeErr) {
		t.Fatalf("error type = %T, want *interbase.Error: %v", err, err)
	}
	if nativeErr.SQLCode == 0 || nativeErr.NativeCode == 0 {
		t.Fatalf("native error metadata = %#v, want non-zero SQLCode and NativeCode", nativeErr)
	}
	return nativeErr
}

func TestConnectorTPBPreservesQuotedIdentifierCase(t *testing.T) {
	schema := fixtureSchema + `
CREATE TABLE "GoMixedCase" ("Id" INTEGER NOT NULL PRIMARY KEY);
INSERT INTO "GoMixedCase" ("Id") VALUES (1);
`
	fixture, cfg, cleanup := createFixture(t, 3, schema)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{
			TableReservations: []interbase.TableReservation{{
				Table:       `"GoMixedCase"`,
				SharingMode: interbase.TableSharingProtected,
				AccessMode:  interbase.TableAccessRead,
			}},
		})
	ctx := readContext(t)
	tx, err := db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
		ReadOnly:  true,
	})
	if err != nil {
		t.Fatalf("begin quoted-identifier transaction: %v", err)
	}
	registerTransactionRollback(t, tx)

	var id int64
	if err := tx.QueryRowContext(ctx, `SELECT "Id" FROM "GoMixedCase"`).Scan(&id); err != nil {
		t.Fatalf("query quoted-identifier table: %v", err)
	}
	if id != 1 {
		t.Fatalf("quoted-identifier row id = %d, want 1", id)
	}
}

func TestEffectiveRoleAuthorization(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	adminDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	roleName := uniqueIntegrationIdentifier("ROLE")
	userName := uniqueIntegrationIdentifier("USER")
	password := uniqueIntegrationPassword()
	roleCreated := false
	userCreated := false
	selectGranted := false
	roleGranted := false

	// Register this before opening the user attachments. Their helper cleanups
	// then run first, leaving the administrator attachment for revocation and
	// DROP statements below.
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		for _, statement := range []struct {
			condition bool
			query     string
		}{
			{condition: roleGranted, query: fmt.Sprintf("REVOKE %s FROM %s", roleName, userName)},
			{condition: selectGranted, query: fmt.Sprintf("REVOKE SELECT ON GO_COUNTRY FROM %s", roleName)},
			{condition: roleCreated, query: fmt.Sprintf("DROP ROLE %s", roleName)},
			{condition: userCreated, query: fmt.Sprintf("DROP USER %s", userName)},
		} {
			if !statement.condition {
				continue
			}
			if _, err := adminDB.ExecContext(ctx, statement.query); err != nil {
				t.Errorf("cleanup %q: %v", statement.query, err)
			}
		}
	})

	ctx := readContext(t)
	for _, statement := range []struct {
		name  string
		query string
		set   *bool
	}{
		{name: "create role", query: fmt.Sprintf("CREATE ROLE %s", roleName), set: &roleCreated},
		{name: "create user", query: fmt.Sprintf("CREATE USER %s SET PASSWORD '%s'", userName, password), set: &userCreated},
		{name: "grant select", query: fmt.Sprintf("GRANT SELECT ON GO_COUNTRY TO %s", roleName), set: &selectGranted},
		{name: "grant role", query: fmt.Sprintf("GRANT %s TO %s", roleName, userName), set: &roleGranted},
	} {
		if _, err := adminDB.ExecContext(ctx, statement.query); err != nil {
			t.Fatalf("admin %s: %v", statement.name, err)
		}
		*statement.set = true
	}

	withoutRole := openDatabaseWithRole(t, cleanup, fixture.ConnectionString(), userName, password, "", "", 3,
		interbase.TransactionOptions{})
	withRole := openDatabaseWithRole(t, cleanup, fixture.ConnectionString(), userName, password, roleName, "", 3,
		interbase.TransactionOptions{})
	var denied string
	err := withoutRole.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&denied)
	requireNativeError(t, err)

	var country string
	if err := withRole.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("role-authorized query: %v", err)
	}
	if country != "USA" {
		t.Fatalf("role-authorized country = %q, want USA", country)
	}
}

func TestConstraintViolationReturnsTypedNativeError(t *testing.T) {
	db := newDatabase(t)
	ctx := readContext(t)
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(1), "first"); err != nil {
		t.Fatalf("insert initial unique row: %v", err)
	}
	_, err := db.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(10), int64(2), "duplicate")
	nativeErr := requireNativeError(t, err)
	if nativeErr.Operation == "" {
		t.Fatalf("constraint error has no operation: %#v", nativeErr)
	}
}

func TestTransactionSnapshotAndReadCommittedVisibility(t *testing.T) {
	for _, test := range []struct {
		name       string
		isolation  sql.IsolationLevel
		wantBefore string
		wantAfter  string
	}{
		{name: "read committed", isolation: sql.LevelReadCommitted, wantBefore: "USA", wantAfter: "pending"},
		{name: "snapshot", isolation: sql.LevelSnapshot, wantBefore: "USA", wantAfter: "USA"},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture, cfg, cleanup := createFixture(t, 1)
			db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
				interbase.TransactionOptions{})
			db.SetMaxOpenConns(2)
			db.SetMaxIdleConns(2)
			ctx := readContext(t)

			restoreContext, cancelRestore := context.WithTimeout(context.Background(), 15*time.Second)
			t.Cleanup(func() {
				defer cancelRestore()
				if _, err := db.ExecContext(restoreContext,
					"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "USA", int64(1)); err != nil {
					t.Errorf("restore fixture country: %v", err)
				}
			})

			writerConn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("open writer attachment: %v", err)
			}
			t.Cleanup(func() {
				if err := writerConn.Close(); err != nil {
					t.Errorf("close writer attachment: %v", err)
				}
			})
			readerConn, err := db.Conn(ctx)
			if err != nil {
				t.Fatalf("open reader attachment: %v", err)
			}
			t.Cleanup(func() {
				if err := readerConn.Close(); err != nil {
					t.Errorf("close reader attachment: %v", err)
				}
			})

			writer, err := writerConn.BeginTx(ctx, nil)
			if err != nil {
				t.Fatalf("begin writer transaction: %v", err)
			}
			registerTransactionRollback(t, writer)
			if _, err := writer.ExecContext(ctx,
				"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "pending", int64(1)); err != nil {
				t.Fatalf("write uncommitted country: %v", err)
			}

			reader, err := readerConn.BeginTx(ctx, &sql.TxOptions{Isolation: test.isolation})
			if err != nil {
				t.Fatalf("begin %s reader transaction: %v", test.name, err)
			}
			registerTransactionRollback(t, reader)
			var country string
			if err := reader.QueryRowContext(ctx,
				"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
				t.Fatalf("read before writer commit: %v", err)
			}
			if country != test.wantBefore {
				t.Fatalf("country before writer commit = %q, want %q", country, test.wantBefore)
			}

			if err := writer.Commit(); err != nil {
				t.Fatalf("commit writer transaction: %v", err)
			}
			if err := reader.QueryRowContext(ctx,
				"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
				t.Fatalf("read after writer commit: %v", err)
			}
			if country != test.wantAfter {
				t.Fatalf("country after writer commit = %q, want %q", country, test.wantAfter)
			}
			if err := reader.Rollback(); err != nil {
				t.Fatalf("rollback reader transaction: %v", err)
			}
		})
	}
}

const noWaitChildEnvironment = "INTERBASE_GO_NOWAIT_CHILD"

func TestTransactionNoWaitAndReservationEnforcement(t *testing.T) {
	if os.Getenv(noWaitChildEnvironment) == "1" {
		testNoWaitAndReservationEnforcementChild(t)
		return
	}

	runBoundedIntegrationChild(t, "TestTransactionNoWaitAndReservationEnforcement",
		noWaitChildEnvironment, 45*time.Second)
}

const recordVersionChildEnvironment = "INTERBASE_GO_RECORD_VERSION_CHILD"

// isc_lock_conflict from the InterBase client status definitions.
const iscLockConflict int64 = 335544345

func TestTransactionRecordVersionAndNoRecordVersionConflict(t *testing.T) {
	if os.Getenv(recordVersionChildEnvironment) == "1" {
		testRecordVersionAndNoRecordVersionConflictChild(t)
		return
	}

	runBoundedIntegrationChild(t, "TestTransactionRecordVersionAndNoRecordVersionConflict",
		recordVersionChildEnvironment, 20*time.Second)
}

func runBoundedIntegrationChild(t *testing.T, testName, environment string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^"+testName+"$", "-test.count=1", "-test.v")
	command.Env = append(os.Environ(), environment+"=1")
	command.SysProcAttr = &syscall.SysProcAttr{
		Setpgid:   true,
		Pdeathsig: syscall.SIGKILL,
	}
	command.Cancel = func() error {
		if command.Process == nil {
			return nil
		}
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return err
	}
	command.WaitDelay = 2 * time.Second
	output, err := command.CombinedOutput()
	if ctx.Err() != nil {
		t.Fatalf("bounded %s subprocess timed out: %v\n%s", testName, ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("bounded %s subprocess failed: %v\n%s", testName, err, output)
	}
}

func testNoWaitAndReservationEnforcementChild(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	writerDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{})
	contenderDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{NoWait: true})
	ctx := readContext(t)

	writer, err := writerDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin conflicting writer transaction: %v", err)
	}
	registerTransactionRollback(t, writer)
	if _, err := writer.ExecContext(ctx,
		"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "locked", int64(1)); err != nil {
		t.Fatalf("hold conflicting row update: %v", err)
	}

	contender, err := contenderDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin NOWAIT contender transaction: %v", err)
	}
	registerTransactionRollback(t, contender)
	conflictContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	start := time.Now()
	_, err = contender.ExecContext(conflictContext,
		"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "contender", int64(1))
	elapsed := time.Since(start)
	cancel()
	if elapsed > 3*time.Second {
		t.Fatalf("NOWAIT conflicting update took %s", elapsed)
	}
	requireNativeError(t, err)
	if err := writer.Rollback(); err != nil {
		t.Fatalf("rollback conflicting writer transaction: %v", err)
	}
	if err := contender.Rollback(); err != nil {
		t.Fatalf("rollback NOWAIT contender transaction: %v", err)
	}

	reservationDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{
			TableReservations: []interbase.TableReservation{{
				Table:       "GO_COUNTRY",
				SharingMode: interbase.TableSharingExclusive,
				AccessMode:  interbase.TableAccessWrite,
			}},
		})
	observerDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{NoWait: true})
	reservation, err := reservationDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin table-reservation transaction: %v", err)
	}
	registerTransactionRollback(t, reservation)
	var country string
	if err := reservation.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("acquire table reservation: %v", err)
	}

	start = time.Now()
	observer, beginErr := observerDB.BeginTx(ctx, nil)
	elapsed = time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("NOWAIT table reservation begin took %s", elapsed)
	}
	if beginErr != nil {
		requireNativeError(t, beginErr)
		return
	}
	registerTransactionRollback(t, observer)
	start = time.Now()
	_, err = observer.ExecContext(ctx,
		"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "observer", int64(1))
	elapsed = time.Since(start)
	if elapsed > 3*time.Second {
		t.Fatalf("NOWAIT table reservation update took %s", elapsed)
	}
	requireNativeError(t, err)
}

func testRecordVersionAndNoRecordVersionConflictChild(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 1)
	writerDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{})
	recordVersionDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{NoWait: true})
	noRecordVersionDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1,
		interbase.TransactionOptions{NoWait: true, NoRecordVersion: true})
	ctx := readContextWithTimeout(t, 2*time.Second)

	writer, err := writerDB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin record-version writer transaction: %v", err)
	}
	registerTransactionRollback(t, writer)
	if _, err := writer.ExecContext(ctx,
		"UPDATE GO_COUNTRY SET COUNTRY = ? WHERE ID = ?", "uncommitted", int64(1)); err != nil {
		t.Fatalf("write uncommitted record-version value: %v", err)
	}

	recordVersion, err := recordVersionDB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatalf("begin record-version reader transaction: %v", err)
	}
	registerTransactionRollback(t, recordVersion)
	var country string
	if err := recordVersion.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("record-version read: %v", err)
	}
	if country != "USA" {
		t.Fatalf("record-version country = %q, want previous committed value %q", country, "USA")
	}
	if err := recordVersion.Rollback(); err != nil {
		t.Fatalf("rollback record-version reader transaction: %v", err)
	}

	noRecordVersion, beginErr := noRecordVersionDB.BeginTx(ctx,
		&sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if beginErr != nil {
		t.Fatalf("begin no-record-version reader transaction: %v", beginErr)
	}
	registerTransactionRollback(t, noRecordVersion)
	conflictContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	start := time.Now()
	var conflictingCountry string
	err = noRecordVersion.QueryRowContext(conflictContext,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&conflictingCountry)
	elapsed := time.Since(start)
	cancel()
	if elapsed > 2*time.Second {
		t.Fatalf("no-record-version NOWAIT conflict took %s", elapsed)
	}
	nativeErr := requireNativeError(t, err)
	if nativeErr.NativeCode != iscLockConflict {
		t.Fatalf("no-record-version conflict native status = %d, want isc_lock_conflict (%d); SQLCODE=%d",
			nativeErr.NativeCode, iscLockConflict, nativeErr.SQLCode)
	}
}
