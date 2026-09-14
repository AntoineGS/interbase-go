//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/internal/testfixture"
)

//go:embed testdata/schema.sql
var fixtureSchema string

const fixtureSetupTimeout = 2 * time.Minute

func cleanupDatabaseAndFixture(closeDatabase, closeFixture func() error, fixturePath string) error {
	if err := closeDatabase(); err != nil {
		return fmt.Errorf("close fixture database: %w; retaining fixture at %q", err, fixturePath)
	}
	if err := closeFixture(); err != nil {
		return fmt.Errorf("close fixture: %w", err)
	}
	return nil
}

func newDatabase(t *testing.T) *sql.DB {
	t.Helper()
	return newDatabaseWithCharset(t, "")
}

func newDatabaseWithCharset(t *testing.T, charset string) *sql.DB {
	t.Helper()

	cfg, err := testfixture.FromEnv()
	if err != nil {
		t.Fatalf("fixture configuration: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), fixtureSetupTimeout)
	defer cancel()
	fixture, createErr := testfixture.Create(ctx, cfg, fixtureSchema)
	closeDatabase := func() error { return nil }
	if fixture != nil {
		directory := filepath.Dir(fixture.Path)
		t.Cleanup(func() {
			if err := cleanupDatabaseAndFixture(closeDatabase, fixture.Close, fixture.Path); err != nil {
				t.Errorf("fixture cleanup: %v", err)
				return
			}
			if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("fixture directory still exists after cleanup: %v", err)
			}
		})
	}
	if createErr != nil {
		t.Fatalf("create fixture: %v", createErr)
	}

	connector, err := interbase.NewConnector(interbase.Config{
		Database: fixture.Path,
		User:     cfg.User,
		Password: cfg.Password,
		Charset:  charset,
	})
	if err != nil {
		t.Fatalf("fixture connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	closeDatabase = db.Close
	if err := pingDatabase(ctx, db); err != nil {
		t.Fatalf("fixture setup ping: %v", err)
	}
	return db
}

func pingDatabase(ctx context.Context, db *sql.DB) error {
	return db.PingContext(ctx)
}

func TestPingDatabaseUsesProvidedContext(t *testing.T) {
	connector, err := interbase.NewConnector(interbase.Config{
		Database: "/tmp/testfixture-no-database.ib",
		User:     "SYSDBA",
	})
	if err != nil {
		t.Fatal(err)
	}
	db := sql.OpenDB(connector)
	defer db.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pingDatabase(ctx, db); !errors.Is(err, context.Canceled) {
		t.Fatalf("pingDatabase error = %v, want context.Canceled", err)
	}
}

func TestReadFixtureSmoke(t *testing.T) {
	db := newDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	var country string
	if err := db.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("fixture query: %v", err)
	}
	if country != "USA" {
		t.Fatalf("country = %q, want USA", country)
	}
}

func TestCleanupDatabaseAndFixtureRetainsFixtureOnDatabaseCloseFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "database.ib")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write fixture placeholder: %v", err)
	}

	closeErr := errors.New("injected database close failure")
	fixtureClosed := false
	err := cleanupDatabaseAndFixture(
		func() error { return closeErr },
		func() error {
			fixtureClosed = true
			return nil
		},
		path,
	)
	if !errors.Is(err, closeErr) {
		t.Fatalf("cleanup error = %v, want database close error", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("cleanup error = %v, want retained fixture path %q", err, path)
	}
	if fixtureClosed {
		t.Fatal("fixture close ran after database close failure")
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retained fixture path: %v", err)
	}
}
