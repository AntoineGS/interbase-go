//go:build integration

package integration_test

import (
	"context"
	"errors"
	"testing"

	interbase "interbase-go"
)

func lpCount(t *testing.T, ctx context.Context, tx *interbase.Transaction, id int64) int {
	t.Helper()
	cursor, err := tx.Query(ctx, "SELECT ID FROM LP_T WHERE ID = ?", id)
	if err != nil {
		t.Fatalf("query LP_T %d: %v", id, err)
	}
	defer cursor.Close()
	count := 0
	for {
		hasRow, err := cursor.Next(ctx)
		if err != nil {
			t.Fatalf("Next LP_T %d: %v", id, err)
		}
		if !hasRow {
			return count
		}
		count++
	}
}

func TestLineProfilePrimitivesUserExceptionNumber(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	for _, ddl := range []string{
		"CREATE EXCEPTION LP_EX 'boom'",
		"CREATE PROCEDURE LP_RAISE AS BEGIN EXCEPTION LP_EX; END",
	} {
		if _, err := db.ExecContext(ctx, ddl); err != nil {
			t.Fatalf("%s: %v", ddl, err)
		}
	}

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	cursor, err := tx.Query(ctx, "SELECT RDB$EXCEPTION_NUMBER FROM RDB$EXCEPTIONS WHERE RDB$EXCEPTION_NAME = 'LP_EX'")
	if err != nil {
		t.Fatalf("query exception number: %v", err)
	}
	if hasRow, err := cursor.Next(ctx); err != nil || !hasRow {
		t.Fatalf("exception row = (%v, %v)", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil || len(row) != 1 {
		t.Fatalf("exception row = %#v, %v", row, err)
	}
	want, ok := row[0].Value.(int64)
	if !ok {
		t.Fatalf("exception number value = %#v (%T)", row[0].Value, row[0].Value)
	}
	_ = cursor.Close()

	_, execErr := tx.Exec(ctx, "EXECUTE PROCEDURE LP_RAISE")
	var nativeErr *interbase.Error
	if !errors.As(execErr, &nativeErr) {
		t.Fatalf("EXECUTE PROCEDURE error = %v (%T), want *interbase.Error", execErr, execErr)
	}
	if nativeErr.GDSCode() != 335544517 {
		t.Fatalf("GDSCode() = %d (codes %v), want isc_except 335544517", nativeErr.GDSCode(), nativeErr.GDSCodes())
	}
	number, found := nativeErr.ExceptionNumber()
	if !found || int64(number) != want {
		t.Fatalf("ExceptionNumber() = %d, %v, want %d", number, found, want)
	}
	if code, ok := interbase.GDSCodeByName("except"); !ok || code != nativeErr.GDSCode() {
		t.Fatalf("GDSCodeByName(except) = %d, %v", code, ok)
	}
}

func TestLineProfilePrimitivesSavepointStatements(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	attachment := openDirectAttachment(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password,
		"", "", 3, interbase.TransactionOptions{})
	ctx := readContext(t)

	if _, err := db.ExecContext(ctx, "CREATE TABLE LP_T (ID INTEGER)"); err != nil {
		t.Fatalf("create LP_T: %v", err)
	}
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })

	steps := []string{
		"SAVEPOINT LP_SP",
		"INSERT INTO LP_T (ID) VALUES (1)",
		"ROLLBACK TO SAVEPOINT LP_SP",
		"SAVEPOINT LP_SP2",
		"INSERT INTO LP_T (ID) VALUES (2)",
		"RELEASE SAVEPOINT LP_SP2",
	}
	for _, step := range steps {
		if _, err := tx.Exec(ctx, step); err != nil {
			t.Fatalf("%s: %v", step, err)
		}
	}
	if got := lpCount(t, ctx, tx, 1); got != 0 {
		t.Fatalf("rows with ID 1 after ROLLBACK TO SAVEPOINT = %d, want 0", got)
	}
	if got := lpCount(t, ctx, tx, 2); got != 1 {
		t.Fatalf("rows with ID 2 after RELEASE SAVEPOINT = %d, want 1", got)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit: %v", err)
	}
}
