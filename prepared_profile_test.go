package interbase

import (
	"context"
	"database/sql"
	"reflect"
	"testing"
)

type preparedProfileCounts struct {
	prepares       int
	statementRuns  int
	begins         int
	rollbacks      int
	statementClose int
	tpb            []byte
}

func preparedProfileTestConn(t *testing.T, descriptor StatementDescriptor) (*sql.Conn, *preparedProfileCounts) {
	t.Helper()
	counts := &preparedProfileCounts{}
	native := introspectionTestNative(3)
	native.cancelSlotOverride = &fakeCancelSlot{}
	native.databaseInfoItemsOverride = perCodeInfoItems(func(byte) ([]byte, error) {
		return []byte{infoEnd}, nil
	})
	native.beginOverride = func(tpb []byte) error {
		counts.begins++
		counts.tpb = append([]byte(nil), tpb...)
		return nil
	}
	native.rollbackOverride = func() error {
		counts.rollbacks++
		return nil
	}
	native.prepareOverride = func(string) (*nativeStatement, error) {
		counts.prepares++
		return &nativeStatement{
			numInputOverride: func() int { return 0 },
			descriptionOverride: func() (StatementDescriptor, error) {
				return descriptor, nil
			},
			planOverride: func() (string, error) { return "PLAN (T NATURAL)", nil },
			queryOverride: func([]argument) (*nativeCursor, []string, error) {
				counts.statementRuns++
				return &nativeCursor{
					nextOverride:  func() (bool, error) { return false, nil },
					closeOverride: func() error { return nil },
				}, []string{"X"}, nil
			},
			execOverride: func([]argument) (int64, error) {
				counts.statementRuns++
				return 1, nil
			},
			closeOverride: func() error {
				counts.statementClose++
				return nil
			},
		}, nil
	}
	db := openDatabaseSQLTestDB(t, &conn{native: native})
	pooled, err := db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pooled.Close() })
	return pooled, counts
}

func TestPreparedProfileCloseDoesNotExecuteOrStartTransaction(t *testing.T) {
	pooled, counts := preparedProfileTestConn(t, StatementDescriptor{Kind: "delete", Mutating: true})
	prepared, err := PrepareProfile(context.Background(), pooled, "DELETE FROM T", ProfilePreparationOptions{})
	if err != nil {
		t.Fatalf("PrepareProfile(): %v", err)
	}
	if counts.prepares != 1 || counts.begins != 0 || counts.statementRuns != 0 {
		t.Fatalf("before close prepares=%d begins=%d runs=%d", counts.prepares, counts.begins, counts.statementRuns)
	}
	if got := prepared.Descriptor(); got.Kind != "delete" || !got.Mutating {
		t.Fatalf("Descriptor() = %+v", got)
	}
	if err := prepared.Close(); err != nil {
		t.Fatalf("Close(): %v", err)
	}
	if counts.prepares != 1 || counts.begins != 0 || counts.statementRuns != 0 || counts.statementClose != 1 {
		t.Fatalf("after close prepares=%d begins=%d runs=%d closes=%d", counts.prepares, counts.begins, counts.statementRuns, counts.statementClose)
	}
}

func TestPreparedProfileRunsSameStatementAndRollsBack(t *testing.T) {
	pooled, counts := preparedProfileTestConn(t, StatementDescriptor{Kind: "select", ReturnsRows: true})
	prepared, err := PrepareProfile(context.Background(), pooled, "SELECT X FROM T", ProfilePreparationOptions{})
	if err != nil {
		t.Fatalf("PrepareProfile(): %v", err)
	}
	if counts.prepares != 1 || counts.begins != 0 || counts.statementRuns != 0 {
		t.Fatalf("after prepare prepares=%d begins=%d runs=%d", counts.prepares, counts.begins, counts.statementRuns)
	}

	result, err := prepared.Profile(context.Background(), ProfileOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("Profile(): %v", err)
	}
	if !result.Complete || !result.RolledBack || result.RawPlan != "PLAN (T NATURAL)" || !result.ReturnsRows {
		t.Fatalf("Profile() = %+v", result)
	}
	if counts.prepares != 1 || counts.begins != 1 || counts.statementRuns != 1 || counts.rollbacks != 1 || counts.statementClose != 1 {
		t.Fatalf("prepares=%d begins=%d runs=%d rollbacks=%d closes=%d", counts.prepares, counts.begins, counts.statementRuns, counts.rollbacks, counts.statementClose)
	}
	if want := []byte{3, 8, 6, 15, 17}; !reflect.DeepEqual(counts.tpb, want) {
		t.Fatalf("profile transaction TPB=%v, want read-only read-committed %v", counts.tpb, want)
	}
}

func TestProfileCompatibilityWrapperPreparesOnce(t *testing.T) {
	pooled, counts := preparedProfileTestConn(t, StatementDescriptor{Kind: "select", ReturnsRows: true})
	result, err := Profile(context.Background(), pooled, "SELECT X FROM T", ProfileOptions{ReadOnly: true})
	if err != nil {
		t.Fatalf("Profile(): %v", err)
	}
	if !result.Complete || !result.RolledBack || result.Prepare <= 0 || result.RawPlan != "PLAN (T NATURAL)" {
		t.Fatalf("Profile() = %+v", result)
	}
	if counts.prepares != 1 || counts.begins != 1 || counts.statementRuns != 1 || counts.rollbacks != 1 || counts.statementClose != 1 {
		t.Fatalf("prepares=%d begins=%d runs=%d rollbacks=%d closes=%d", counts.prepares, counts.begins, counts.statementRuns, counts.rollbacks, counts.statementClose)
	}
}
