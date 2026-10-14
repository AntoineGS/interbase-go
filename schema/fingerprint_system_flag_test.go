package schema

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"
)

func TestFingerprintSystemFlagRoutingFact(t *testing.T) {
	for _, value := range []any{nil, int64(0), int64(1), int64(-7), int64(math.MaxInt64), int64(math.MinInt64)} {
		row := fingerprintRow("RDB$FIELDS", map[string]any{"RDB$FIELD_NAME": "D", "RDB$SYSTEM_FLAG": value})
		f, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(map[string][][]any{"RDB$FIELDS": {row}}, false, ""))
		if err != nil {
			t.Fatal(err)
		}
		want := sql.NullInt64{}
		if value != nil {
			want = sql.NullInt64{Int64: value.(int64), Valid: true}
		}
		f.VisitRecords(func(r FingerprintRecord) bool {
			if r.SystemFlag != want {
				t.Fatalf("flag=%+v want=%+v", r.SystemFlag, want)
			}
			saved, ok := f.LookupRecord(r.Family, r.Key)
			if !ok || saved.SystemFlag != want {
				t.Fatalf("lookup lost flag: %+v", saved)
			}
			saved.SystemFlag = sql.NullInt64{Int64: 42, Valid: true}
			again, _ := f.LookupRecord(r.Family, r.Key)
			if again.SystemFlag != want {
				t.Fatal("DTO mutation reached storage")
			}
			return true
		})
	}
	bad := fingerprintRow("RDB$FIELDS", map[string]any{"RDB$FIELD_NAME": "D", "RDB$SYSTEM_FLAG": "0"})
	_, err := ReadCatalogFingerprint(context.Background(), nil, 1<<20, fingerprintFixtureReader(map[string][][]any{"RDB$FIELDS": {bad}}, false, ""))
	if !errors.Is(err, ErrFingerprintCapability) {
		t.Fatalf("unexpected scalar: %v", err)
	}
}
