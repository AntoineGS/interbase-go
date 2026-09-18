//go:build integration

package integration_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/internal/testfixture"
)

type ownedLifecycleDatabase struct {
	directory  string
	path       string
	attachment *interbase.Attachment
}

func newOwnedLifecycleDatabase(t *testing.T) *ownedLifecycleDatabase {
	t.Helper()
	directory, err := os.MkdirTemp(os.TempDir(), "interbase-go-direct-lifecycle-")
	if err != nil {
		t.Fatalf("create lifecycle directory: %v", err)
	}
	database := &ownedLifecycleDatabase{
		directory: directory,
		path:      filepath.Join(directory, "database-'owned'.ib"),
	}
	t.Cleanup(func() {
		if database.attachment != nil {
			if err := database.attachment.Close(); err != nil {
				t.Errorf("close lifecycle attachment: %v", err)
			}
		}
		if _, err := os.Stat(database.path); err == nil {
			t.Logf("retaining lifecycle database after incomplete cleanup: %s", database.path)
			return
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Errorf("inspect lifecycle database %q: %v", database.path, err)
			return
		}
		if err := os.Remove(database.directory); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Errorf("remove lifecycle directory %q: %v", database.directory, err)
		}
	})
	return database
}

func lifecycleConfig(t *testing.T, cfg testfixture.Config, path string) interbase.Config {
	t.Helper()
	database := path
	if cfg.Server != "" {
		database = cfg.Server + ":" + path
	}
	return interbase.Config{
		Database: database,
		User:     cfg.User,
		Password: cfg.Password,
		Dialect:  3,
	}
}

func lifecycleTestConfig(t *testing.T, path string) interbase.Config {
	t.Helper()
	cfg, err := testfixture.FromEnv()
	if err != nil {
		t.Fatalf("lifecycle environment: %v", err)
	}
	return lifecycleConfig(t, cfg, path)
}

func lifecycleContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	return ctx
}

func createLifecycleAttachment(t *testing.T, database *ownedLifecycleDatabase) (*interbase.Attachment, interbase.Config, context.Context) {
	t.Helper()
	cfg := lifecycleTestConfig(t, database.path)
	ctx := lifecycleContext(t)
	attachment, err := interbase.CreateDatabase(ctx, cfg, interbase.CreateOptions{PageSize: 4096})
	if attachment != nil {
		database.attachment = attachment
	}
	if err != nil {
		t.Fatalf("CreateDatabase: %v", err)
	}
	if _, err := os.Stat(database.path); err != nil {
		t.Fatalf("created database path %q: %v", database.path, err)
	}
	return attachment, cfg, ctx
}

func createLifecycleTable(t *testing.T, attachment *interbase.Attachment, ctx context.Context) {
	t.Helper()
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin lifecycle seed transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, "CREATE TABLE GO_LIFECYCLE (ID INTEGER NOT NULL PRIMARY KEY, TEXT_VALUE VARCHAR(64))"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("create lifecycle table: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit lifecycle table: %v", err)
	}

	tx, err = attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin lifecycle row transaction: %v", err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO GO_LIFECYCLE (ID, TEXT_VALUE) VALUES (?, ?)", int64(1), "sentinel"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("seed lifecycle row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit lifecycle seed: %v", err)
	}
}

func TestDirectLifecycleCreatesAndDropsOwnedDatabase(t *testing.T) {
	database := newOwnedLifecycleDatabase(t)
	attachment, _, ctx := createLifecycleAttachment(t, database)
	createLifecycleTable(t, attachment, ctx)

	if err := attachment.DropDatabase(ctx); err != nil {
		t.Fatalf("DropDatabase: %v", err)
	}
	if _, err := os.Stat(database.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dropped database stat error = %v, want os.ErrNotExist", err)
	}
	if err := attachment.Close(); err != nil {
		t.Fatalf("Close after DropDatabase: %v", err)
	}
}

func TestDirectLifecycleDuplicateCreatePreservesSentinel(t *testing.T) {
	database := newOwnedLifecycleDatabase(t)
	attachment, cfg, ctx := createLifecycleAttachment(t, database)
	createLifecycleTable(t, attachment, ctx)
	if err := attachment.Close(); err != nil {
		t.Fatalf("close initial duplicate-create attachment: %v", err)
	}

	duplicate, err := interbase.CreateDatabase(ctx, cfg, interbase.CreateOptions{})
	if err == nil {
		if duplicate != nil {
			_ = duplicate.Close()
		}
		t.Fatal("duplicate CreateDatabase returned nil error")
	}
	if _, err := os.Stat(database.path); err != nil {
		t.Fatalf("database path after duplicate create: %v", err)
	}

	probe, err := interbase.Open(ctx, cfg)
	if err != nil {
		t.Fatalf("open duplicate-create probe: %v", err)
	}
	t.Cleanup(func() {
		if err := probe.Close(); err != nil {
			t.Errorf("close duplicate-create probe during cleanup: %v", err)
		}
	})
	tx, err := probe.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		_ = probe.Close()
		t.Fatalf("begin duplicate-create probe: %v", err)
	}
	cursor, err := tx.Query(ctx, "SELECT TEXT_VALUE FROM GO_LIFECYCLE WHERE ID = ?", int64(1))
	if err != nil {
		_ = tx.Rollback()
		_ = probe.Close()
		t.Fatalf("query duplicate-create probe: %v", err)
	}
	if hasRow, err := cursor.Next(ctx); err != nil || !hasRow {
		_ = cursor.Close()
		_ = tx.Rollback()
		_ = probe.Close()
		t.Fatalf("duplicate-create probe Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	closeErr := cursor.Close()
	rollbackErr := tx.Rollback()
	if err != nil || closeErr != nil || rollbackErr != nil {
		_ = probe.Close()
		t.Fatalf("duplicate-create probe cleanup/query errors: row=%v close=%v rollback=%v", err, closeErr, rollbackErr)
	}
	if len(row) != 1 || row[0].Value != "sentinel" {
		_ = probe.Close()
		t.Fatalf("sentinel after duplicate create = %#v, want sentinel", row)
	}

	if err := probe.DropDatabase(ctx); err != nil {
		t.Fatalf("DropDatabase on reopened fixture attachment: %v", err)
	}
}

func TestDirectLifecycleRefusesActiveResourcesThenDrops(t *testing.T) {
	database := newOwnedLifecycleDatabase(t)
	attachment, _, ctx := createLifecycleAttachment(t, database)
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin active-resource transaction: %v", err)
	}
	cursor, err := tx.Query(ctx, "SELECT 1 FROM RDB$DATABASE")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("open active-resource cursor: %v", err)
	}
	if err := attachment.DropDatabase(ctx); !errors.Is(err, interbase.ErrAttachmentBusy) {
		_ = cursor.Close()
		_ = tx.Rollback()
		t.Fatalf("DropDatabase with active resources = %v, want ErrAttachmentBusy", err)
	}
	if err := cursor.Close(); err != nil {
		_ = tx.Rollback()
		t.Fatalf("close active-resource cursor: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback active-resource transaction: %v", err)
	}
	if err := attachment.DropDatabase(ctx); err != nil {
		t.Fatalf("DropDatabase after releasing transaction: %v", err)
	}
}

func TestDirectLifecycleDiagnosticsAndDMLPlanDoNotExecute(t *testing.T) {
	database := newOwnedLifecycleDatabase(t)
	attachment, _, ctx := createLifecycleAttachment(t, database)
	createLifecycleTable(t, attachment, ctx)

	diagnostics, err := attachment.Diagnostics(ctx)
	if err != nil {
		t.Fatalf("Diagnostics: %v", err)
	}
	if diagnostics.ClientVersion == "" || diagnostics.ServerVersion == "" || diagnostics.DatabaseVersion == "" ||
		diagnostics.ODSVersion <= 0 || diagnostics.PageSize != 4096 || diagnostics.SQLDialect != 3 {
		t.Fatalf("Diagnostics = %#v, missing expected native values", diagnostics)
	}

	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		t.Fatalf("begin plan transaction: %v", err)
	}
	plan, err := tx.Plan(ctx, "UPDATE GO_LIFECYCLE SET TEXT_VALUE = 'changed' WHERE ID = 1")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("DML Plan: %v", err)
	}
	sentinelCursor, err := tx.Query(ctx, "SELECT TEXT_VALUE FROM GO_LIFECYCLE WHERE ID = 1")
	if err != nil {
		_ = tx.Rollback()
		t.Fatalf("query in planning transaction: %v", err)
	}
	hasRow, nextErr := sentinelCursor.Next(ctx)
	row, rowErr := sentinelCursor.Row()
	closeErr := sentinelCursor.Close()
	if nextErr != nil || rowErr != nil || closeErr != nil || !hasRow {
		_ = tx.Rollback()
		t.Fatalf("query in planning transaction = (row=%#v hasRow=%v next=%v row=%v close=%v), want sentinel",
			row, hasRow, nextErr, rowErr, closeErr)
	}
	if len(row) != 1 || row[0].Value != "sentinel" {
		_ = tx.Rollback()
		t.Fatalf("sentinel in planning transaction = %#v, want sentinel", row)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback plan transaction: %v", err)
	}
	if plan != "" {
		t.Logf("server returned a non-empty DML plan: %q", plan)
	}

	probe, err := interbase.Open(ctx, lifecycleTestConfig(t, database.path))
	if err != nil {
		t.Fatalf("open post-plan probe: %v", err)
	}
	probeTx, err := probe.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		_ = probe.Close()
		t.Fatalf("begin post-plan probe: %v", err)
	}
	cursor, err := probeTx.Query(ctx, "SELECT TEXT_VALUE FROM GO_LIFECYCLE WHERE ID = 1")
	if err != nil {
		_ = probeTx.Rollback()
		_ = probe.Close()
		t.Fatalf("query post-plan probe: %v", err)
	}
	if hasRow, err := cursor.Next(ctx); err != nil || !hasRow {
		_ = cursor.Close()
		_ = probeTx.Rollback()
		_ = probe.Close()
		t.Fatalf("post-plan probe Next() = (%v, %v), want (true, nil)", hasRow, err)
	}
	postPlanRow, postPlanRowErr := cursor.Row()
	postPlanCloseErr := cursor.Close()
	postPlanRollbackErr := probeTx.Rollback()
	postPlanProbeErr := probe.Close()
	if postPlanRowErr != nil || postPlanCloseErr != nil || postPlanRollbackErr != nil || postPlanProbeErr != nil {
		t.Fatalf("post-plan probe cleanup/query errors: row=%v close=%v rollback=%v closeAttachment=%v", postPlanRowErr, postPlanCloseErr, postPlanRollbackErr, postPlanProbeErr)
	}
	if len(postPlanRow) != 1 || postPlanRow[0].Value != "sentinel" {
		t.Fatalf("value after DML Plan = %#v, want sentinel", postPlanRow)
	}

	if err := attachment.DropDatabase(ctx); err != nil {
		t.Fatalf("DropDatabase after diagnostics/plan: %v", err)
	}
}

func TestDirectLifecycleQuoteAndCancelledCreateValidation(t *testing.T) {
	database := newOwnedLifecycleDatabase(t)
	cfg := lifecycleTestConfig(t, database.path)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := interbase.CreateDatabase(ctx, cfg, interbase.CreateOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("CreateDatabase(cancelled context) = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(database.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancelled create path stat error = %v, want os.ErrNotExist", err)
	}

	attachment, _, liveCtx := createLifecycleAttachment(t, database)
	// The database name includes an apostrophe. Creation succeeded without any
	// SQL credential interpolation; drop the generated database through the
	// owned attachment once validation has completed.
	if err := attachment.DropDatabase(liveCtx); err != nil {
		t.Fatalf("DropDatabase for quoted path: %v", err)
	}
}
