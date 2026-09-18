package interbase

import (
	"context"
	"database/sql"
	"testing"
)

func TestDirectAttachmentSupportsIndependentTransactions(t *testing.T) {
	var started int
	var rolledBack int
	var tpbs [][]byte
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		beginTransactionOverride: func(tpb []byte) (*nativeTransaction, error) {
			started++
			tpbs = append(tpbs, append([]byte(nil), tpb...))
			return &nativeTransaction{
				rollbackOverride: func() error {
					rolledBack++
					return nil
				},
			}, nil
		},
	}
	attachment := &Attachment{conn: &conn{native: native}}

	first, err := attachment.BeginTx(context.Background(), TransactionOptions{
		Isolation: sql.LevelReadCommitted,
	})
	if err != nil {
		t.Fatalf("begin first direct transaction: %v", err)
	}
	second, err := attachment.BeginTx(context.Background(), TransactionOptions{
		Isolation: sql.LevelSnapshot,
	})
	if err != nil {
		t.Fatalf("begin second direct transaction: %v", err)
	}
	if first == second {
		t.Fatal("independent direct transactions share the same Go transaction")
	}
	if started != 2 {
		t.Fatalf("native transaction starts = %d, want 2", started)
	}
	if len(tpbs) != 2 || string(tpbs[0]) == string(tpbs[1]) {
		t.Fatalf("transaction parameter blocks do not preserve independent options: %#v", tpbs)
	}

	if err := first.Rollback(); err != nil {
		t.Fatalf("rollback first direct transaction: %v", err)
	}
	if err := second.Rollback(); err != nil {
		t.Fatalf("rollback second direct transaction: %v", err)
	}
	if rolledBack != 2 {
		t.Fatalf("native transaction rollbacks = %d, want 2", rolledBack)
	}
}

func TestDirectAttachmentCloseRollsBackEveryActiveTransaction(t *testing.T) {
	var rolledBack int
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
			return &nativeTransaction{
				rollbackOverride: func() error {
					rolledBack++
					return nil
				},
			}, nil
		},
	}
	attachment := &Attachment{conn: &conn{native: native}}
	first, err := attachment.BeginTx(context.Background(), TransactionOptions{})
	if err != nil {
		t.Fatalf("begin first direct transaction: %v", err)
	}
	second, err := attachment.BeginTx(context.Background(), TransactionOptions{})
	if err != nil {
		t.Fatalf("begin second direct transaction: %v", err)
	}

	if err := attachment.Close(); err != nil {
		t.Fatalf("close attachment: %v", err)
	}
	if rolledBack != 2 {
		t.Fatalf("attachment close rollbacks = %d, want 2", rolledBack)
	}
	if !first.done || !second.done {
		t.Fatalf("attachment close left transactions active: first.done=%v second.done=%v", first.done, second.done)
	}
}
