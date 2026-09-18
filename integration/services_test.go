//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/services"
)

func openServices(t *testing.T, cleanup *fixtureCleanup, cfg testServiceConfig) *services.Manager {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	manager, err := services.Open(ctx, services.Config{
		Host:     cfg.host,
		User:     cfg.user,
		Password: cfg.password,
	})
	if err != nil {
		if manager != nil {
			if closeErr := manager.Close(); closeErr != nil {
				t.Logf("close services manager after attach error: %v", closeErr)
			}
		}
		t.Fatalf("open services manager: %v", err)
	}
	cleanup.addAttachment(manager)
	return manager
}

type testServiceConfig struct {
	host     string
	user     string
	password string
}

func TestServicesManagerInfoAndLog(t *testing.T) {
	fixture, fixtureCfg, cleanup := createFixture(t, 3)
	manager := openServices(t, cleanup, testServiceConfig{
		host:     fixtureCfg.Server,
		user:     fixtureCfg.User,
		password: fixtureCfg.Password,
	})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	info, err := manager.ServerInfo(ctx)
	if err != nil {
		t.Fatalf("server info: %v", err)
	}
	if info.ServiceManagerVersion != 2 {
		t.Fatalf("service manager version = %d, want 2", info.ServiceManagerVersion)
	}
	if !strings.Contains(info.ServerVersion, "V") {
		t.Fatalf("server version = %q, want a version marker", info.ServerVersion)
	}
	if !strings.Contains(info.Architecture, "InterBase") {
		t.Fatalf("architecture = %q, want InterBase", info.Architecture)
	}
	if !strings.Contains(strings.ToLower(info.SecurityDatabasePath), "admin.ib") {
		t.Fatalf("security database path = %q, want admin.ib", info.SecurityDatabasePath)
	}

	job, err := manager.Logs(ctx)
	if err != nil {
		t.Fatalf("start server log: %v", err)
	}
	if err := job.Wait(ctx); err != nil {
		t.Fatalf("wait server log: %v", err)
	}
	if _, err := manager.ConnectionCount(ctx); err != nil {
		t.Fatalf("connection count: %v", err)
	}
	if _, err := manager.AttachedDatabaseNames(ctx); err != nil {
		t.Fatalf("attached database names: %v", err)
	}
	_ = fixture
}

func TestServicesAliasesUsersBackupAndRestoreOwnedFixture(t *testing.T) {
	fixture, fixtureCfg, cleanup := createFixture(t, 3)
	manager := openServices(t, cleanup, testServiceConfig{
		host:     fixtureCfg.Server,
		user:     fixtureCfg.User,
		password: fixtureCfg.Password,
	})

	workDir := filepath.Dir(fixture.Path)
	alias := fmt.Sprintf("IBGO_ALIAS_%d", time.Now().UnixNano()%1000000000)
	backupPath := filepath.Join(workDir, "services-backup.ibk")
	restoredPath := filepath.Join(workDir, "services-restored.ib")
	dumpPath := filepath.Join(workDir, "services-dump.ibd")
	userName := fmt.Sprintf("IBGO_USER_%d", time.Now().UnixNano()%1000000000)
	restoredDatabase := restoredPath
	if fixtureCfg.Server != "" {
		restoredDatabase = fixtureCfg.Server + ":" + restoredPath
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	if err := manager.AddAlias(ctx, alias, fixture.Path); err != nil {
		t.Fatalf("add alias: %v", err)
	}
	cleanup.addCleanup(func() error {
		return manager.DeleteAlias(context.Background(), alias)
	})
	aliases, err := manager.Aliases(ctx)
	if err != nil {
		t.Fatalf("list aliases: %v", err)
	}
	if aliases[alias] != fixture.Path {
		t.Fatalf("alias %q = %q, want %q", alias, aliases[alias], fixture.Path)
	}

	if err := manager.AddUser(ctx, services.User{
		Name:       userName,
		Password:   "IBGO_TEST_PASSWORD",
		FirstName:  "InterBase",
		MiddleName: "Go",
		LastName:   "Services",
	}); err != nil {
		t.Fatalf("add user: %v", err)
	}
	cleanup.addCleanup(func() error {
		return manager.DeleteUser(context.Background(), userName)
	})
	users, err := manager.Users(ctx, userName)
	if err != nil {
		t.Fatalf("list created user: %v", err)
	}
	if len(users) != 1 || users[0].Name != strings.ToUpper(userName) || users[0].FirstName != "InterBase" {
		t.Fatalf("created users = %+v", users)
	}
	if err := manager.ModifyUser(ctx, services.User{
		Name:       userName,
		Password:   "IBGO_TEST_PASSWORD_2",
		FirstName:  "Updated",
		MiddleName: "User",
		LastName:   "Record",
	}); err != nil {
		t.Fatalf("modify user: %v", err)
	}
	users, err = manager.Users(ctx, userName)
	if err != nil {
		t.Fatalf("list modified user: %v", err)
	}
	if len(users) != 1 || users[0].FirstName != "Updated" || users[0].MiddleName != "User" || users[0].LastName != "Record" {
		t.Fatalf("modified users = %+v", users)
	}

	backupJob, err := manager.Backup(ctx, services.BackupRequest{
		SourceDatabase: fixture.Path,
		Destinations:   []services.BackupDestination{{Path: backupPath}},
	})
	if err != nil {
		t.Fatalf("start backup: %v", err)
	}
	if err := backupJob.Wait(ctx); err != nil {
		t.Fatalf("wait backup: %v", err)
	}
	if _, err := os.Stat(backupPath); err != nil {
		t.Fatalf("backup file: %v", err)
	}
	cleanup.addOwnedFile(backupPath)

	restoreJob, err := manager.Restore(ctx, services.RestoreRequest{
		SourceFiles:  []string{backupPath},
		Destinations: []services.RestoreDestination{{Path: restoredPath}},
	})
	if err != nil {
		t.Fatalf("start restore: %v", err)
	}
	if err := restoreJob.Wait(ctx); err != nil {
		t.Fatalf("wait restore: %v", err)
	}
	if _, err := os.Stat(restoredPath); err != nil {
		t.Fatalf("restored database: %v", err)
	}
	cleanup.addOwnedFile(restoredPath)

	restoredDB := openDatabase(t, cleanup, restoredDatabase, fixtureCfg.User, fixtureCfg.Password, "", 3,
		interbase.TransactionOptions{})
	var country string
	if err := restoredDB.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(1)).Scan(&country); err != nil {
		t.Fatalf("restored schema/data query: %v", err)
	}
	if country != "USA" {
		t.Fatalf("restored country = %q, want USA", country)
	}
	if _, err := restoredDB.ExecContext(ctx,
		"INSERT INTO GO_COUNTRY (ID, COUNTRY, CURRENCY) VALUES (?, ?, ?)", int64(99), "EXISTING", "Keep"); err != nil {
		t.Fatalf("mark existing restored database: %v", err)
	}
	if err := restoredDB.Close(); err != nil {
		t.Fatalf("close existing restored database: %v", err)
	}

	noReplaceJob, err := manager.Restore(ctx, services.RestoreRequest{
		SourceFiles:  []string{backupPath},
		Destinations: []services.RestoreDestination{{Path: restoredPath}},
	})
	var noReplaceErr error
	if err != nil {
		noReplaceErr = err
	} else {
		noReplaceErr = noReplaceJob.Wait(ctx)
		if noReplaceErr != nil && !noReplaceJob.CompletionKnown() {
			cleanup.retainOwnedFile(restoredPath)
			t.Fatalf("no-replace restore completion is unknown: %v", noReplaceErr)
		}
	}
	if noReplaceErr == nil {
		t.Fatal("no-replace restore succeeded for an existing database")
	}

	restoredDB = openDatabase(t, cleanup, restoredDatabase, fixtureCfg.User, fixtureCfg.Password, "", 3,
		interbase.TransactionOptions{})
	if err := restoredDB.QueryRowContext(ctx,
		"SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?", int64(99)).Scan(&country); err != nil {
		t.Fatalf("query existing restored database after no-replace restore: %v", err)
	}
	if country != "EXISTING" {
		t.Fatalf("existing restored country = %q, want EXISTING", country)
	}

	dumpJob, err := manager.CreateDump(ctx, fixture.Path, dumpPath, false)
	if err != nil {
		t.Fatalf("start dump: %v", err)
	}
	if err := dumpJob.Wait(ctx); err != nil {
		t.Fatalf("wait dump: %v", err)
	}
	if _, err := os.Stat(dumpPath); err != nil {
		t.Fatalf("dump file: %v", err)
	}
	cleanup.addOwnedFile(dumpPath)

	statsJob, err := manager.DatabaseStatistics(ctx, services.DatabaseStatisticsRequest{Database: fixture.Path})
	if err != nil {
		t.Fatalf("start statistics: %v", err)
	}
	statsOutput, err := io.ReadAll(statsJob)
	if err != nil {
		t.Fatalf("read statistics: %v", err)
	}
	if err := statsJob.Wait(ctx); err != nil {
		t.Fatalf("wait statistics: %v", err)
	}
	if len(statsOutput) == 0 || (statsOutput[0] < ' ' && statsOutput[0] != '\n' && statsOutput[0] != '\r' && statsOutput[0] != '\t') {
		t.Fatalf("statistics output starts with a non-readable protocol byte: %v", statsOutput)
	}
	if !bytes.Contains(statsOutput, []byte("GO_COUNTRY")) {
		t.Fatalf("statistics output = %q, want fixture table name", statsOutput)
	}

	// Keep io.Reader coverage explicit without retaining an unbounded report.
	logJob, err := manager.Logs(ctx)
	if err != nil {
		t.Fatalf("start second log: %v", err)
	}
	buffer := make([]byte, 128)
	_, readErr := logJob.Read(buffer)
	if readErr != nil && readErr != io.EOF {
		t.Fatalf("read log: %v", readErr)
	}
	if err := logJob.Close(); err != nil {
		t.Fatalf("close second log: %v", err)
	}
}
