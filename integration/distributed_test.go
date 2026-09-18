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

func TestDistributedTransactionCommitsAcrossAttachments(t *testing.T) {
	firstFixture, firstConfig, firstCleanup := createFixture(t, 3)
	secondFixture, secondConfig, secondCleanup := createFixture(t, 3)
	first := openDirectAttachment(t, firstCleanup, firstFixture.ConnectionString(), firstConfig.User,
		firstConfig.Password, "", "", 3, interbase.TransactionOptions{})
	second := openDirectAttachment(t, secondCleanup, secondFixture.ConnectionString(), secondConfig.User,
		secondConfig.Password, "", "", 3, interbase.TransactionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	distributed, err := interbase.BeginDistributed(ctx, []interbase.Participant{
		{Attachment: first, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
		{Attachment: second, Options: interbase.TxOptions{Isolation: sql.LevelReadCommitted}},
	})
	if err != nil {
		t.Fatalf("begin distributed transaction: %v", err)
	}
	firstTx, err := distributed.Participant(0)
	if err != nil {
		t.Fatalf("first participant: %v", err)
	}
	secondTx, err := distributed.Participant(1)
	if err != nil {
		t.Fatalf("second participant: %v", err)
	}
	t.Cleanup(func() { _ = distributed.Rollback(context.Background()) })

	for name, tx := range map[string]*interbase.Transaction{
		"first":  firstTx,
		"second": secondTx,
	} {
		info, err := tx.Info(ctx, interbase.InfoTransactionID)
		if err != nil {
			t.Fatalf("%s participant transaction info: %v", name, err)
		}
		if len(info.Data) == 0 {
			t.Fatalf("%s participant transaction info is empty", name)
		}
	}
	for name, attachment := range map[string]*interbase.Attachment{
		"first":  first,
		"second": second,
	} {
		info, err := attachment.DatabaseInfo(ctx, interbase.InfoDatabaseID)
		if err != nil {
			t.Fatalf("%s participant database info: %v", name, err)
		}
		if len(info.Data) == 0 {
			t.Fatalf("%s participant database info is empty", name)
		}
	}
	recoveryInfo, err := distributed.RecoveryInfo(ctx)
	if err != nil {
		t.Fatalf("distributed recovery info: %v", err)
	}
	if len(recoveryInfo) != 2 {
		t.Fatalf("distributed recovery info count = %d, want 2", len(recoveryInfo))
	}
	for index, info := range recoveryInfo {
		if len(info.DatabaseID.Data) == 0 || len(info.TransactionID.Data) == 0 {
			t.Fatalf("distributed recovery info[%d] = %#v, want database and transaction IDs", index, info)
		}
	}

	if _, err := firstTx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(9801), int64(98), "distributed first"); err != nil {
		t.Fatalf("first participant write: %v", err)
	}
	if _, err := secondTx.Exec(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		int64(9802), int64(98), "distributed second"); err != nil {
		t.Fatalf("second participant write: %v", err)
	}
	if err := distributed.Prepare(ctx, []byte("distributed-integration-recovery")); err != nil {
		t.Fatalf("prepare distributed transaction: %v", err)
	}
	if _, err := firstTx.Exec(ctx, "SELECT ID FROM GO_WRITE"); !errors.Is(err, interbase.ErrDistributedParticipantManaged) {
		t.Fatalf("participant operation after prepare = %v, want coordinator-managed error", err)
	}
	if err := distributed.Commit(ctx); err != nil {
		t.Fatalf("commit distributed transaction: %v", err)
	}

	firstDB := openDatabase(t, firstCleanup, firstFixture.ConnectionString(), firstConfig.User,
		firstConfig.Password, "", 3, interbase.TransactionOptions{})
	secondDB := openDatabase(t, secondCleanup, secondFixture.ConnectionString(), secondConfig.User,
		secondConfig.Password, "", 3, interbase.TransactionOptions{})
	for name, item := range map[string]struct {
		db *sql.DB
		id int64
	}{
		"first":  {db: firstDB, id: 9801},
		"second": {db: secondDB, id: 9802},
	} {
		db, id := item.db, item.id
		var got string
		if err := db.QueryRowContext(ctx, "SELECT LABEL FROM GO_WRITE WHERE ID = ?", id).Scan(&got); err != nil {
			t.Fatalf("%s committed row: %v", name, err)
		}
		if got == "" {
			t.Fatalf("%s committed row label is empty", name)
		}
	}
}
