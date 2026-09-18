//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/services"
)

const (
	distributedRecoveryChildEnvironment = "INTERBASE_GO_DISTRIBUTED_RECOVERY_CHILD"
	distributedRecoveryFirstDatabase    = "INTERBASE_GO_DISTRIBUTED_RECOVERY_FIRST_DATABASE"
	distributedRecoverySecondDatabase   = "INTERBASE_GO_DISTRIBUTED_RECOVERY_SECOND_DATABASE"
	distributedRecoveryUser             = "INTERBASE_GO_DISTRIBUTED_RECOVERY_USER"
	distributedRecoveryPassword         = "INTERBASE_GO_DISTRIBUTED_RECOVERY_PASSWORD"
	distributedRecoveryArtifact         = "INTERBASE_GO_DISTRIBUTED_RECOVERY_ARTIFACT"

	limboSingleTransactionID byte = 19
	limboMultiTransactionID  byte = 20
)

type distributedRecoveryArtifactData struct {
	DatabasePaths []string                               `json:"database_paths"`
	RecoveryInfo  []interbase.DistributedParticipantInfo `json:"recovery_info"`
}

func TestWriteSyncedPrivateFileWritesBeforeReturning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-recovery-artifact")
	want := []byte(`{"recovery_info":[]}`)
	if err := writeSyncedPrivateFile(path, want, 0o600); err != nil {
		t.Fatalf("writeSyncedPrivateFile() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read synced private file: %v", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("synced private file = %q, want %q", got, want)
	}
}

func TestDistributedRecoveryThroughServicesAfterClientDeath(t *testing.T) {
	if os.Getenv(distributedRecoveryChildEnvironment) == "1" {
		testDistributedRecoveryClientDeathChild(t)
		return
	}

	for _, test := range []struct {
		name   string
		commit bool
	}{
		{name: "commit", commit: true},
		{name: "rollback", commit: false},
	} {
		test := test
		t.Run(test.name, func(t *testing.T) {
			runDistributedRecoveryCase(t, test.commit)
		})
	}
}

func runDistributedRecoveryCase(t *testing.T, commit bool) {
	t.Helper()
	firstFixture, firstConfig, firstCleanup := createFixture(t, 3)
	secondFixture, secondConfig, secondCleanup := createFixture(t, 3)
	first := openDirectAttachment(t, firstCleanup, firstFixture.ConnectionString(), firstConfig.User,
		firstConfig.Password, "", "", 3, interbase.TransactionOptions{})
	second := openDirectAttachment(t, secondCleanup, secondFixture.ConnectionString(), secondConfig.User,
		secondConfig.Password, "", "", 3, interbase.TransactionOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	advanceTransactionCounter(t, first, ctx, 32)

	artifactPath := filepath.Join(t.TempDir(), "distributed-recovery.json")
	command := exec.CommandContext(ctx, os.Args[0],
		"-test.run=^TestDistributedRecoveryThroughServicesAfterClientDeath$",
		"-test.count=1", "-test.v")
	command.Env = append(os.Environ(),
		distributedRecoveryChildEnvironment+"=1",
		distributedRecoveryFirstDatabase+"="+firstFixture.ConnectionString(),
		distributedRecoverySecondDatabase+"="+secondFixture.ConnectionString(),
		distributedRecoveryUser+"="+firstConfig.User,
		distributedRecoveryPassword+"="+firstConfig.Password,
		distributedRecoveryArtifact+"="+artifactPath,
	)
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
		t.Fatalf("distributed recovery child timed out: %v\n%s", ctx.Err(), output)
	}
	if err != nil {
		t.Fatalf("distributed recovery child failed: %v\n%s", err, output)
	}

	artifact := readDistributedRecoveryArtifact(t, artifactPath)
	if len(artifact.DatabasePaths) != 2 || artifact.DatabasePaths[0] != firstFixture.ConnectionString() ||
		artifact.DatabasePaths[1] != secondFixture.ConnectionString() {
		t.Fatalf("child database paths = %#v, want the two fixture connection strings", artifact.DatabasePaths)
	}
	if len(artifact.RecoveryInfo) != 2 {
		t.Fatalf("child recovery info count = %d, want 2", len(artifact.RecoveryInfo))
	}

	firstDatabaseInfo, err := first.DatabaseInfo(ctx, interbase.InfoDatabaseID)
	if err != nil {
		t.Fatalf("first database info: %v", err)
	}
	secondDatabaseInfo, err := second.DatabaseInfo(ctx, interbase.InfoDatabaseID)
	if err != nil {
		t.Fatalf("second database info: %v", err)
	}
	for index, want := range []interbase.InfoItem{firstDatabaseInfo, secondDatabaseInfo} {
		if !bytes.Equal(artifact.RecoveryInfo[index].DatabaseID.Data, want.Data) {
			t.Fatalf("recovery info[%d] database ID does not identify the input database", index)
		}
	}

	transactionIDs := make([]int64, len(artifact.RecoveryInfo))
	for index, info := range artifact.RecoveryInfo {
		value, err := info.TransactionID.Uint64()
		if err != nil {
			t.Fatalf("recovery info[%d] transaction ID: %v", index, err)
		}
		if value > uint64(^uint32(0)) {
			t.Fatalf("recovery info[%d] transaction ID = %d, exceeds native range", index, value)
		}
		transactionIDs[index] = int64(value)
	}
	if transactionIDs[0] == transactionIDs[1] {
		t.Fatalf("transaction IDs are equal (%v); live ordering proof requires deliberately different counters", transactionIDs)
	}

	manager, err := services.Open(ctx, services.Config{
		Host:     firstConfig.Server,
		User:     firstConfig.User,
		Password: firstConfig.Password,
	})
	if err != nil {
		if manager != nil {
			err = errors.Join(err, manager.Close())
		}
		t.Fatalf("open recovery Services Manager: %v", err)
	}
	resolved := false
	t.Cleanup(func() {
		if !resolved {
			cleanupDistributedLimbo(manager, firstFixture.ConnectionString(), secondFixture.ConnectionString())
		}
		if err := manager.Close(); err != nil {
			t.Errorf("close recovery Services Manager: %v", err)
		}
	})

	for index, database := range []string{firstFixture.ConnectionString(), secondFixture.ConnectionString()} {
		ids := waitForLimboTransaction(t, ctx, manager, database, transactionIDs[index])
		if len(ids) != 1 || ids[0] != transactionIDs[index] {
			t.Fatalf("limbo IDs for participant %d at %q = %v, want exactly %d", index, database,
				ids, transactionIDs[index])
		}
	}
	mode := "rollback"
	if commit {
		mode = "commit"
	}
	decisionPath := filepath.Join(filepath.Dir(artifactPath), "distributed-recovery-decision")
	if err := writeSyncedPrivateFile(decisionPath, []byte(mode+"\n"), 0o600); err != nil {
		t.Fatalf("persist durable recovery decision: %v", err)
	}
	if persisted, err := os.ReadFile(decisionPath); err != nil || string(persisted) != mode+"\n" {
		t.Fatalf("read durable recovery decision: value=%q error=%v", persisted, err)
	}
	t.Logf("live Services %s recovery matched transaction IDs %v to the two ordered database participants", mode, transactionIDs)

	for index, database := range []string{firstFixture.ConnectionString(), secondFixture.ConnectionString()} {
		request := services.ResolveLimboRequest{Database: database, TransactionID: transactionIDs[index]}
		if commit {
			if err := manager.CommitLimbo(ctx, request); err != nil {
				t.Fatalf("commit limbo participant %d: %v", index, err)
			}
		} else if err := manager.RollbackLimbo(ctx, request); err != nil {
			t.Fatalf("rollback limbo participant %d: %v", index, err)
		}
	}
	resolved = true

	firstDB := openDatabase(t, firstCleanup, firstFixture.ConnectionString(), firstConfig.User, firstConfig.Password, "", 3,
		interbase.TransactionOptions{})
	secondDB := openDatabase(t, secondCleanup, secondFixture.ConnectionString(), secondConfig.User, secondConfig.Password, "", 3,
		interbase.TransactionOptions{})
	wantCount := int64(0)
	if commit {
		wantCount = 1
	}
	for index, item := range []struct {
		db *sql.DB
		id int64
	}{
		{db: firstDB, id: 9911},
		{db: secondDB, id: 9912},
	} {
		var count int64
		if err := item.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM GO_WRITE WHERE ID = ?", item.id).Scan(&count); err != nil {
			t.Fatalf("participant %d sentinel query: %v", index, err)
		}
		if count != wantCount {
			t.Fatalf("participant %d sentinel count = %d, want %d", index, count, wantCount)
		}
	}
}

func testDistributedRecoveryClientDeathChild(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	first := openRecoveryChildAttachment(t, ctx, os.Getenv(distributedRecoveryFirstDatabase))
	second := openRecoveryChildAttachment(t, ctx, os.Getenv(distributedRecoverySecondDatabase))
	distributed, err := interbase.BeginDistributed(ctx, []interbase.Participant{
		{Attachment: first, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
		{Attachment: second, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
	})
	if err != nil {
		t.Fatalf("child begin distributed transaction: %v", err)
	}
	firstTx, err := distributed.Participant(0)
	if err != nil {
		t.Fatalf("child first participant: %v", err)
	}
	secondTx, err := distributed.Participant(1)
	if err != nil {
		t.Fatalf("child second participant: %v", err)
	}
	if _, err := firstTx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)", int64(9911), int64(91), "recovery first"); err != nil {
		t.Fatalf("child first sentinel: %v", err)
	}
	if _, err := secondTx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)", int64(9912), int64(92), "recovery second"); err != nil {
		t.Fatalf("child second sentinel: %v", err)
	}
	recoveryInfo, err := distributed.RecoveryInfo(ctx)
	if err != nil {
		t.Fatalf("child recovery info: %v", err)
	}
	artifact := distributedRecoveryArtifactData{
		DatabasePaths: []string{os.Getenv(distributedRecoveryFirstDatabase), os.Getenv(distributedRecoverySecondDatabase)},
		RecoveryInfo:  recoveryInfo,
	}
	encoded, err := json.Marshal(artifact)
	if err != nil {
		t.Fatalf("marshal child recovery artifact: %v", err)
	}
	if err := writeSyncedPrivateFile(os.Getenv(distributedRecoveryArtifact), encoded, 0o600); err != nil {
		t.Fatalf("write child recovery artifact: %v", err)
	}
	if err := distributed.Prepare(ctx, []byte("go distributed recovery client death")); err != nil {
		t.Fatalf("child prepare distributed transaction: %v", err)
	}
	// os.Exit intentionally skips all defers and leaves the prepared native
	// transaction for the parent process to resolve through Services.
	os.Exit(0)
}

func openRecoveryChildAttachment(t *testing.T, ctx context.Context, database string) *interbase.Attachment {
	t.Helper()
	attachment, err := interbase.Open(ctx, interbase.Config{
		Database: database,
		User:     os.Getenv(distributedRecoveryUser),
		Password: os.Getenv(distributedRecoveryPassword),
		Dialect:  3,
	})
	if err != nil {
		t.Fatalf("open child attachment %q: %v", database, err)
	}
	return attachment
}

func advanceTransactionCounter(t *testing.T, attachment *interbase.Attachment, ctx context.Context, count int) {
	t.Helper()
	for index := 0; index < count; index++ {
		tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{Isolation: sql.LevelReadCommitted})
		if err != nil {
			t.Fatalf("advance transaction counter begin %d: %v", index, err)
		}
		if _, err := tx.Exec(ctx, "UPDATE GO_COUNTRY SET COUNTRY = COUNTRY WHERE ID = ?", int64(1)); err != nil {
			_ = tx.Rollback()
			t.Fatalf("advance transaction counter exec %d: %v", index, err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatalf("advance transaction counter commit %d: %v", index, err)
		}
	}
}

func readDistributedRecoveryArtifact(t *testing.T, path string) distributedRecoveryArtifactData {
	t.Helper()
	encoded, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read child recovery artifact: %v", err)
	}
	var artifact distributedRecoveryArtifactData
	if err := json.Unmarshal(encoded, &artifact); err != nil {
		t.Fatalf("decode child recovery artifact: %v", err)
	}
	return artifact
}

func writeSyncedPrivateFile(path string, data []byte, mode os.FileMode) (err error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	defer func() {
		closeErr := file.Close()
		if err == nil {
			err = closeErr
		}
	}()

	written, err := file.Write(data)
	if err != nil {
		return err
	}
	if written != len(data) {
		return io.ErrShortWrite
	}
	return file.Sync()
}

func waitForLimboTransaction(t *testing.T, ctx context.Context, manager *services.Manager, database string, want int64) []int64 {
	t.Helper()
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	for {
		ids, err := listLimboTransactionIDs(ctx, manager, database)
		if err != nil {
			t.Fatalf("list limbo transactions for %q: %v", database, err)
		}
		for _, id := range ids {
			if id == want {
				return ids
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("waiting for limbo transaction %d at %q: %v", want, database, ctx.Err())
		case <-deadline.C:
			t.Fatalf("limbo transaction %d did not appear at %q; got %v", want, database, ids)
		case <-time.After(25 * time.Millisecond):
		}
	}
}

func listLimboTransactionIDs(ctx context.Context, manager *services.Manager, database string) ([]int64, error) {
	job, err := manager.ListLimbo(ctx, services.LimboTransactionsRequest{Database: database})
	if err != nil {
		return nil, err
	}
	output, err := io.ReadAll(job)
	if err != nil {
		return nil, fmt.Errorf("read limbo output: %w", err)
	}
	return decodeLimboTransactionIDs(output)
}

func decodeLimboTransactionIDs(output []byte) ([]int64, error) {
	ids := make([]int64, 0, 1)
	for position := 0; position < len(output); {
		if output[position] != limboSingleTransactionID && output[position] != limboMultiTransactionID {
			if len(ids) != 0 {
				return nil, fmt.Errorf("unexpected limbo output byte %d at position %d", output[position], position)
			}
			position++
			continue
		}
		if len(output)-position < 9 {
			return nil, fmt.Errorf("truncated limbo transaction record at position %d", position)
		}
		ids = append(ids, int64(binary.LittleEndian.Uint32(output[position+1:position+5])))
		position += 9
	}
	return ids, nil
}

func cleanupDistributedLimbo(manager *services.Manager, databases ...string) {
	if manager == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	for _, database := range databases {
		ids, err := listLimboTransactionIDs(ctx, manager, database)
		if err != nil {
			continue
		}
		for _, id := range ids {
			_ = manager.RollbackLimbo(ctx, services.ResolveLimboRequest{Database: database, TransactionID: id})
		}
	}
}
