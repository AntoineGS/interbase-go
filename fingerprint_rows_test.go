package interbase

import (
	"context"
	"database/sql/driver"
	"errors"
	"reflect"
	"testing"
)

func TestBlobIdentityCopiesPartsWithoutNativeRead(t *testing.T) {
	ref := BlobRef{attachment: &Attachment{}, tx: &Transaction{}, high: -7, low: 19}
	got, valid := ref.Identity()
	if !valid || got != (BlobIdentity{High: -7, Low: 19}) {
		t.Fatalf("%+v %v", got, valid)
	}
	if _, valid := (BlobRef{}).Identity(); valid {
		t.Fatal("zero ref is not valid")
	}
}

func TestFingerprintRowsLifecycle(t *testing.T) {
	for _, mode := range []string{"success", "callback", "fetch", "close", "mutating", "active", "distributed"} {
		t.Run(mode, func(t *testing.T) {
			sentinel := errors.New("injected failure")
			commits, rollbacks, frees, closes, queries, visits := 0, 0, 0, 0, 0, 0
			tx := &nativeTransaction{commitOverride: func() error { commits++; return nil }, rollbackOverride: func() error { rollbacks++; return nil }, freeOverride: func() { frees++ }}
			tx.cancelSlotOverride = &fakeCancelSlot{}
			tx.prepareContextOverride = func(_ string, _ bool, _ *nativeCancelOperation) (*nativeStatement, error) {
				return &nativeStatement{descriptionOverride: func() (StatementDescriptor, error) {
					return StatementDescriptor{Kind: "select", ReturnsRows: true, Mutating: mode == "mutating"}, nil
				}, closeOverride: func() error { return nil }}, nil
			}
			tx.queryContextOverride = func(_ string, _ []argument, identity bool, _ *nativeCancelOperation) (*nativeCursor, []string, error) {
				queries++
				if !identity {
					t.Fatal("not identity mode")
				}
				first := true
				return &nativeCursor{nextOverride: func() (bool, error) {
					if mode == "fetch" {
						return false, context.Canceled
					}
					got := first
					first = false
					return got, nil
				},
					fingerprintValueOverride: func(int) (any, error) { return BlobIdentity{High: -7, Low: 19}, nil }, indicatorOverride: func(int) (uint16, error) { return 4, nil },
					closeOverride: func() error {
						closes++
						if mode == "close" {
							return sentinel
						}
						return nil
					}, abortOverride: func() error { closes++; return nil }}, []string{"SOURCE"}, nil
			}
			native := introspectionTestNative(3)
			native.transactionStateOverride = func() nativeHandleState {
				if mode == "active" {
					return nativeHandleLive
				}
				return nativeHandleConsumed
			}
			native.beginTransactionOverride = func(tpb []byte) (*nativeTransaction, error) {
				if !reflect.DeepEqual(tpb, []byte{3, 8, 6, 15, 17}) {
					t.Fatalf("TPB=%v", tpb)
				}
				return tx, nil
			}
			c := &conn{native: native}
			if mode == "distributed" {
				c.distributed = &DistributedTransaction{}
			}
			db := openDatabaseSQLTestDB(t, c)
			pooled, err := db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer pooled.Close()
			stats, err := ReadFingerprintRows(context.Background(), pooled, []FingerprintQuery{{SQL: "SELECT SOURCE FROM CATALOG"}}, func(q int, cols []string, cells []Cell) error {
				visits++
				if q != 0 || cols[0] != "SOURCE" || cells[0].Value != (BlobIdentity{High: -7, Low: 19}) || cells[0].Indicator != 4 {
					t.Fatalf("row=%v", cells)
				}
				if mode == "callback" {
					return sentinel
				}
				return nil
			})
			if mode == "success" {
				if err != nil || stats.Rows != 1 || stats.Queries != 1 || commits != 1 || rollbacks != 0 || frees != 1 || closes != 1 {
					t.Fatalf("%+v err=%v commits=%d rollback=%d frees=%d closes=%d", stats, err, commits, rollbacks, frees, closes)
				}
				return
			}
			if err == nil {
				t.Fatal("expected rejection")
			}
			if mode == "active" || mode == "distributed" {
				if frees+commits+rollbacks+queries != 0 {
					t.Fatal("caller transaction touched")
				}
				return
			}
			if commits != 0 || rollbacks != 1 || frees != 1 {
				t.Fatalf("commit=%d rollback=%d free=%d", commits, rollbacks, frees)
			}
			if mode == "mutating" && queries != 0 {
				t.Fatal("mutating statement executed")
			}
			if mode == "callback" && !errors.Is(err, sentinel) {
				t.Fatalf("lost callback error: %v", err)
			}
			if mode == "fetch" && !errors.Is(err, context.Canceled) {
				t.Fatalf("lost cancellation: %v", err)
			}
			if mode == "close" && !c.closed {
				t.Fatal("cleanup failure did not invalidate")
			}
		})
	}
}

func TestFingerprintRowsRejectNil(t *testing.T) {
	db := openDatabaseSQLTestDB(t, notInterBaseTestConn{})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()
	if _, err := ReadFingerprintRows(context.Background(), pooled, nil, func(int, []string, []Cell) error { return nil }); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatal(err)
	}
	if _, err := ReadFingerprintRows(context.Background(), nil, nil, func(int, []string, []Cell) error { return nil }); !errors.Is(err, ErrNotInterBaseConn) {
		t.Fatal(err)
	}
	c := &conn{native: introspectionTestNative(3)}
	if _, err := c.ReadFingerprintRows(context.Background(), nil, nil); err == nil {
		t.Fatal("nil visitor accepted")
	}
	c.closed = true
	if _, err := c.ReadFingerprintRows(context.Background(), nil, func(int, []string, []Cell) error { return nil }); !errors.Is(err, driver.ErrBadConn) {
		t.Fatal(err)
	}
}

func TestFingerprintRowsFailedBeginOwnsCleanup(t *testing.T) {
	startErr, cleanupErr := errors.New("start failed"), errors.New("rollback failed")
	rolled, freed := false, false
	tx := &nativeTransaction{
		rollbackCleanupOverride: func() (error, nativeHandleState) { rolled = true; return cleanupErr, nativeHandleLive },
		freeOverride:            func() { freed = true },
	}
	native := introspectionTestNative(3)
	native.beginTransactionOverride = func([]byte) (*nativeTransaction, error) { return tx, startErr }
	c := &conn{native: native}
	_, err := c.ReadFingerprintRows(context.Background(), nil, func(int, []string, []Cell) error { return nil })
	if !rolled || !freed || !c.closed || !errors.Is(err, startErr) || !errors.Is(err, cleanupErr) || !errors.Is(err, driver.ErrBadConn) {
		t.Fatalf("rolled=%v freed=%v closed=%v err=%v", rolled, freed, c.closed, err)
	}
}
