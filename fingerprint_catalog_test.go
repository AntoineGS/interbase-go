package interbase

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"interbase-go/schema"
)

func TestCatalogFingerprintMissingNativeCapability(t *testing.T) {
	if _, err := ReadCatalogFingerprint(context.Background(), nil, 64<<20); !errors.Is(err, schema.ErrFingerprintCapability) {
		t.Fatal(err)
	}
}

func TestSchemaFingerprintAdapterCopiesIdentityAndFlags(t *testing.T) {
	tx := &nativeTransaction{commitOverride: func() error { return nil }, freeOverride: func() {}}
	tx.cancelSlotOverride = &fakeCancelSlot{}
	tx.prepareContextOverride = func(string, bool, *nativeCancelOperation) (*nativeStatement, error) {
		return &nativeStatement{descriptionOverride: func() (StatementDescriptor, error) {
			return StatementDescriptor{Kind: "select", ReturnsRows: true}, nil
		}, closeOverride: func() error { return nil }}, nil
	}
	tx.queryContextOverride = func(_ string, args []argument, identity bool, _ *nativeCancelOperation) (*nativeCursor, []string, error) {
		if !identity || len(args) != 1 {
			t.Fatal("query adapter")
		}
		first := true
		return &nativeCursor{nextOverride: func() (bool, error) { v := first; first = false; return v, nil }, fingerprintValueOverride: func(i int) (any, error) {
			if i == 0 {
				return BlobIdentity{High: -7, Low: 19}, nil
			}
			return nil, nil
		}, indicatorOverride: func(i int) (uint16, error) {
			if i == 0 {
				return 8, nil
			}
			return 0xffff, nil
		}, closeOverride: func() error { return nil }}, []string{"BLOB", "NULL_BLOB"}, nil
	}
	native := introspectionTestNative(3)
	native.transactionStateOverride = func() nativeHandleState { return nativeHandleConsumed }
	native.beginTransactionOverride = func(tpb []byte) (*nativeTransaction, error) {
		if !reflect.DeepEqual(tpb, []byte{3, 8, 6, 15, 17}) {
			t.Fatalf("native flags %v", tpb)
		}
		t.Logf("native TPB=%v: version3/read/wait/read-committed/record-version", tpb)
		return tx, nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	c, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	var saved []any
	stats, err := readSchemaFingerprintRows(context.Background(), c, []schema.CatalogFingerprintQuery{{SQL: "SELECT BLOB,NULL_BLOB FROM CATALOG WHERE ID=?", Args: []any{int64(1)}}}, func(q int, cols []string, values []any) error {
		if q != 0 || cols[0] != "BLOB" {
			t.Fatal("projection mapping")
		}
		saved = values
		return nil
	})
	if err != nil || stats.Rows != 1 || stats.Queries != 1 || stats.BlobOpens != 0 || stats.SourceBytes != 0 {
		t.Fatalf("stats=%+v err=%v", stats, err)
	}
	if len(saved) != 2 || saved[0] != (schema.CatalogBlobIdentity{High: -7, Low: 19}) || saved[1] != nil {
		t.Fatalf("saved=%v", saved)
	}
}
