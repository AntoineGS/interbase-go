package interbase

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

func pooledTransactionTestConn(t *testing.T, commitErr error) (*sql.Conn, *[]string) {
	t.Helper()
	var calls []string
	native := &nativeConnection{
		brokenOverride: func() bool { return false },
		closeOverride:  func() error { return nil },
		beginTransactionOverride: func([]byte) (*nativeTransaction, error) {
			calls = append(calls, "begin")
			return &nativeTransaction{
				commitOverride:      func() error { calls = append(calls, "commit"); return commitErr },
				rollbackOverride:    func() error { calls = append(calls, "rollback"); return nil },
				handleStateOverride: func() nativeHandleState { return nativeHandleConsumed },
			}, nil
		},
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pooled.Close() })
	return pooled, &calls
}

func TestWithTransactionCommitsOnSuccess(t *testing.T) {
	pooled, calls := pooledTransactionTestConn(t, nil)
	err := WithTransaction(context.Background(), pooled, TransactionOptions{NoWait: true}, func(tx *Transaction) error {
		if tx == nil {
			t.Fatal("nil transaction")
		}
		return nil
	})
	if err != nil || len(*calls) != 2 || (*calls)[1] != "commit" {
		t.Fatalf("err=%v calls=%v", err, *calls)
	}
}

func TestWithTransactionRollsBackOnCallbackError(t *testing.T) {
	pooled, calls := pooledTransactionTestConn(t, nil)
	want := errors.New("statement failed")
	err := WithTransaction(context.Background(), pooled, TransactionOptions{}, func(*Transaction) error { return want })
	if !errors.Is(err, want) || errors.Is(err, ErrTransactionCommit) || (*calls)[1] != "rollback" {
		t.Fatalf("err=%v calls=%v", err, *calls)
	}
}

func TestWithTransactionReportsCommitFailure(t *testing.T) {
	native := errors.New("connection lost")
	pooled, _ := pooledTransactionTestConn(t, native)
	err := WithTransaction(context.Background(), pooled, TransactionOptions{}, func(*Transaction) error { return nil })
	if !errors.Is(err, ErrTransactionCommit) {
		t.Fatalf("err = %v, want ErrTransactionCommit", err)
	}
}

func TestWithTransactionRejectsForeignConnections(t *testing.T) {
	if err := WithTransaction(context.Background(), nil, TransactionOptions{}, func(*Transaction) error { return nil }); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatalf("nil conn: %v", err)
	}
}
