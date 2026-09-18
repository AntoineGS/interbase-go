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

type fixtureAttachment interface {
	Close() error
}

type fixtureCloseFunc func() error

func (f fixtureCloseFunc) Close() error {
	return f()
}

// fixtureCleanup owns the fixture and every database pool attached to it.
// There must be one cleanup callback for the whole ownership graph: a failed
// attachment close means the fixture must remain available for inspection.
type fixtureCleanup struct {
	fixturePath    string
	directory      string
	closeFixture   func() error
	attachments    []fixtureAttachment
	cleanupActions []func() error
	ownedFiles     []string
}

func newFixtureCleanup(fixturePath, directory string, closeFixture func() error) *fixtureCleanup {
	return &fixtureCleanup{
		fixturePath:  fixturePath,
		directory:    directory,
		closeFixture: closeFixture,
	}
}

func (c *fixtureCleanup) addAttachment(attachment fixtureAttachment) {
	if attachment != nil {
		c.attachments = append(c.attachments, attachment)
	}
}

func (c *fixtureCleanup) addCleanup(action func() error) {
	if action != nil {
		c.cleanupActions = append(c.cleanupActions, action)
	}
}

func (c *fixtureCleanup) addOwnedFile(path string) {
	if path == "" || !c.ownsPath(path) {
		return
	}
	for _, existing := range c.ownedFiles {
		if existing == path {
			return
		}
	}
	c.ownedFiles = append(c.ownedFiles, path)
}

func (c *fixtureCleanup) retainOwnedFile(path string) {
	// Removing a path from this list is deliberate: the operation that created
	// it was not proven complete, so cleanup must leave the artifact in place.
	for index := 0; index < len(c.ownedFiles); index++ {
		if c.ownedFiles[index] == path {
			c.ownedFiles = append(c.ownedFiles[:index], c.ownedFiles[index+1:]...)
			index--
		}
	}
}

func (c *fixtureCleanup) ownsPath(path string) bool {
	cleanPath := filepath.Clean(path)
	cleanDirectory := filepath.Clean(c.directory)
	relative, err := filepath.Rel(cleanDirectory, cleanPath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return false
	}
	return cleanPath != filepath.Clean(c.fixturePath)
}

func (c *fixtureCleanup) close() error {
	var cleanupErr error
	for index := len(c.cleanupActions) - 1; index >= 0; index-- {
		if err := c.cleanupActions[index](); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	for index := len(c.attachments) - 1; index >= 0; index-- {
		if err := c.attachments[index].Close(); err != nil {
			cleanupErr = errors.Join(cleanupErr, err)
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("close fixture dependencies: %w; retaining fixture at %q", cleanupErr, c.fixturePath)
	}
	for index := len(c.ownedFiles) - 1; index >= 0; index-- {
		if err := os.Remove(c.ownedFiles[index]); err != nil && !errors.Is(err, os.ErrNotExist) {
			cleanupErr = errors.Join(cleanupErr, fmt.Errorf("remove owned file %q: %w", c.ownedFiles[index], err))
		}
	}
	if cleanupErr != nil {
		return fmt.Errorf("remove fixture outputs: %w; retaining fixture at %q", cleanupErr, c.fixturePath)
	}
	if err := c.closeFixture(); err != nil {
		return fmt.Errorf("close fixture: %w; retaining fixture at %q", err, c.fixturePath)
	}
	if _, err := os.Stat(c.directory); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("inspect fixture directory %q: %w", c.directory, err)
	}
	return fmt.Errorf("fixture directory still exists after cleanup: %q", c.directory)
}

func newDatabase(t *testing.T) *sql.DB {
	t.Helper()
	// Existing contracts are Dialect 1 contracts; pass it explicitly so the
	// public zero-value default can be exercised through newDatabaseWithDialect.
	return newDatabaseWithDialectAndCharset(t, 1, "")
}

func newDatabaseWithCharset(t *testing.T, charset string) *sql.DB {
	t.Helper()
	return newDatabaseWithDialectAndCharset(t, 1, charset)
}

func newDatabaseWithDialect(t *testing.T, dialect int) *sql.DB {
	t.Helper()
	return newDatabaseWithDialectAndCharset(t, dialect, "")
}

func newDatabaseWithDialectAndCharset(t *testing.T, dialect int, charset string) *sql.DB {
	t.Helper()
	return newDatabaseWithDialectCharsetAndTransactionOptions(t, dialect, charset,
		interbase.TransactionOptions{})
}

func newDatabaseWithTransactionOptions(t *testing.T, options interbase.TransactionOptions) *sql.DB {
	t.Helper()
	return newDatabaseWithDialectCharsetAndTransactionOptions(t, 1, "", options)
}

func newDatabaseWithDialectCharsetAndTransactionOptions(t *testing.T, dialect int,
	charset string, transactionOptions interbase.TransactionOptions) *sql.DB {
	t.Helper()

	fixture, cfg, cleanup := createFixture(t, dialect)
	return openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, charset, dialect,
		transactionOptions)
}

func createFixture(t *testing.T, dialect int, schemaOverride ...string) (*testfixture.Database, testfixture.Config, *fixtureCleanup) {
	t.Helper()

	cfg, err := testfixture.FromEnv()
	if err != nil {
		t.Fatalf("fixture configuration: %v", err)
	}
	cfg.Dialect = dialect

	ctx, cancel := context.WithTimeout(context.Background(), fixtureSetupTimeout)
	defer cancel()
	schema := fixtureSchema
	if len(schemaOverride) != 0 {
		schema = schemaOverride[0]
	}
	fixture, createErr := testfixture.Create(ctx, cfg, schema)
	var cleanup *fixtureCleanup
	if fixture != nil {
		directory := filepath.Dir(fixture.Path)
		cleanup = newFixtureCleanup(fixture.Path, directory, fixture.Close)
		t.Cleanup(func() {
			if err := cleanup.close(); err != nil {
				t.Errorf("fixture cleanup: %v", err)
			}
		})
	}
	if createErr != nil {
		t.Fatalf("create fixture: %v", createErr)
	}
	return fixture, cfg, cleanup
}

func openDatabase(t *testing.T, cleanup *fixtureCleanup, database, user, password, charset string, dialect int,
	transactionOptions interbase.TransactionOptions) *sql.DB {
	t.Helper()
	return openDatabaseWithRole(t, cleanup, database, user, password, "", charset, dialect,
		transactionOptions)
}

func openDatabaseWithRole(t *testing.T, cleanup *fixtureCleanup, database, user, password, role, charset string,
	dialect int, transactionOptions interbase.TransactionOptions) *sql.DB {
	t.Helper()

	connector, err := interbase.NewConnector(interbase.Config{
		Database:           database,
		User:               user,
		Password:           password,
		Role:               role,
		Charset:            charset,
		Dialect:            dialect,
		TransactionOptions: transactionOptions,
	})
	if err != nil {
		t.Fatalf("fixture connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	cleanup.addAttachment(db)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := pingDatabase(ctx, db); err != nil {
		t.Fatalf("fixture setup ping: %v", err)
	}
	return db
}

func openDirectAttachment(t *testing.T, cleanup *fixtureCleanup, database, user, password, role, charset string,
	dialect int, transactionOptions interbase.TransactionOptions) *interbase.Attachment {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	attachment, err := interbase.Open(ctx, interbase.Config{
		Database:           database,
		User:               user,
		Password:           password,
		Role:               role,
		Charset:            charset,
		Dialect:            dialect,
		TransactionOptions: transactionOptions,
	})
	if err != nil {
		t.Fatalf("open direct attachment: %v", err)
	}
	cleanup.addAttachment(attachment)
	return attachment
}

func pingDatabase(ctx context.Context, db *sql.DB) error {
	return db.PingContext(ctx)
}

func TestPingDatabaseUsesProvidedContext(t *testing.T) {
	connector, err := interbase.NewConnector(interbase.Config{
		Database: "/tmp/testfixture-no-database.ib",
		User:     "SYSDBA",
		Dialect:  1,
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

func TestFixtureCleanupRetainsFixtureWhenAnyAttachmentCloseFails(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "database.ib")
	if err := os.WriteFile(path, []byte("fixture"), 0o600); err != nil {
		t.Fatalf("write fixture placeholder: %v", err)
	}

	firstCloseErr := errors.New("injected first attachment close failure")
	secondCloseErr := errors.New("injected second attachment close failure")
	var closeOrder []string
	fixtureClosed := false
	cleanup := newFixtureCleanup(path, directory, func() error {
		fixtureClosed = true
		return nil
	})
	cleanup.addAttachment(fixtureCloseFunc(func() error {
		closeOrder = append(closeOrder, "first")
		return firstCloseErr
	}))
	cleanup.addAttachment(fixtureCloseFunc(func() error {
		closeOrder = append(closeOrder, "second")
		return secondCloseErr
	}))
	err := cleanup.close()
	if !errors.Is(err, firstCloseErr) || !errors.Is(err, secondCloseErr) {
		t.Fatalf("cleanup error = %v, want both attachment close errors", err)
	}
	if !strings.Contains(err.Error(), path) {
		t.Fatalf("cleanup error = %v, want retained fixture path %q", err, path)
	}
	if fixtureClosed {
		t.Fatal("fixture close ran after database close failure")
	}
	if got, want := strings.Join(closeOrder, ","), "second,first"; got != want {
		t.Fatalf("attachment cleanup order = %q, want %q", got, want)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("retained fixture path: %v", err)
	}
}

func TestFixtureCleanupRunsActionsBeforeAttachmentsAndRemovesOwnedFiles(t *testing.T) {
	directory := t.TempDir()
	fixturePath := filepath.Join(directory, "database.ib")
	outputPath := filepath.Join(directory, "service-output.ibd")
	for _, path := range []string{fixturePath, outputPath} {
		if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
			t.Fatalf("write owned file: %v", err)
		}
	}

	var order []string
	cleanup := newFixtureCleanup(fixturePath, directory, func() error {
		order = append(order, "fixture")
		if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("owned output was not removed before fixture close: %v", err)
		}
		if err := os.Remove(fixturePath); err != nil {
			return err
		}
		return os.Remove(directory)
	})
	cleanup.addAttachment(fixtureCloseFunc(func() error {
		order = append(order, "manager")
		return nil
	}))
	cleanup.addCleanup(func() error {
		order = append(order, "action")
		return nil
	})
	cleanup.addOwnedFile(outputPath)

	if err := cleanup.close(); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if got, want := strings.Join(order, ","), "action,manager,fixture"; got != want {
		t.Fatalf("cleanup order = %q, want %q", got, want)
	}
	if _, err := os.Stat(outputPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned output after cleanup: %v", err)
	}
}

func TestFixtureCleanupRetainsUnprovenOwnedFile(t *testing.T) {
	directory := t.TempDir()
	fixturePath := filepath.Join(directory, "database.ib")
	outputPath := filepath.Join(directory, "unknown-output.ibd")
	for _, path := range []string{fixturePath, outputPath} {
		if err := os.WriteFile(path, []byte("owned"), 0o600); err != nil {
			t.Fatalf("write owned file: %v", err)
		}
	}

	cleanup := newFixtureCleanup(fixturePath, directory, func() error {
		return os.Remove(fixturePath)
	})
	cleanup.addOwnedFile(outputPath)
	cleanup.retainOwnedFile(outputPath)
	if err := cleanup.close(); err == nil {
		t.Fatal("cleanup succeeded while retaining an unproven output")
	}
	if _, err := os.Stat(outputPath); err != nil {
		t.Fatalf("unproven output was removed: %v", err)
	}
}
