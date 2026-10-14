//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/schema"
	"interbase-go/services"
)

func TestFingerprintResourcePairBoundedNative(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	stop, joined := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(joined)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				for p := peak.Load(); m.HeapAlloc > p; p = peak.Load() {
					if peak.CompareAndSwap(p, m.HeapAlloc) {
						break
					}
				}
			}
		}
	}()
	defer func() { close(stop); <-joined }()
	started := time.Now()
	baseline, err := interbase.ReadCatalogFingerprint(ctx, c, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := interbase.ReadCatalogFingerprint(ctx, c, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(started)
	if !baseline.Equal(candidate) || baseline.AccountedBytes > 64<<20 || candidate.AccountedBytes > 64<<20 || baseline.Stats.BlobOpens != 0 || candidate.Stats.BlobOpens != 0 || baseline.Stats.SourceBytes != 0 || candidate.Stats.SourceBytes != 0 {
		t.Fatal("resource pair contract failed")
	}
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(baseline)
	runtime.KeepAlive(candidate)
	t.Logf("two serial native scans elapsed=%s baseline-accounted=%d candidate-accounted=%d pair-accounted=%d heap-before=%d heap-after=%d sampled-heap-peak=%d sample-period=1ms (not hard process peak/limit) blobs=0 source-bytes=0", elapsed, baseline.AccountedBytes, candidate.AccountedBytes, baseline.AccountedBytes+candidate.AccountedBytes, before.HeapAlloc, after.HeapAlloc, peak.Load())
}

func TestFingerprintManifestNative(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	f, err := interbase.ReadCatalogFingerprint(ctx, c, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	g, err := interbase.ReadCatalogFingerprint(ctx, c, 64<<20)
	if err != nil {
		t.Fatal(err)
	}
	if !f.Equal(g) || f.Stats.BlobOpens != 0 || f.Stats.SourceBytes != 0 {
		t.Fatalf("unstable fingerprint stats=%+v", f.Stats)
	}
	t.Logf("fingerprint version=%s records=%d retained=%d stats=%+v", f.Version, f.RecordCount(), f.AccountedBytes, f.Stats)
	if _, err := interbase.ReadCatalogFingerprint(ctx, c, f.AccountedBytes-1); !errors.Is(err, schema.ErrFingerprintLimit) {
		t.Fatalf("limit: %v", err)
	}
}

func TestFingerprintAllFamiliesAlterSweep(t *testing.T) {
	fixture, cfg, cleanup := createFixture(t, 3)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "NONE", 3, interbase.TransactionOptions{})
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	c, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	exec := func(query string) {
		t.Helper()
		if _, err := c.ExecContext(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	for step, query := range []string{
		"CREATE DOMAIN GO_FP_DOMAIN AS INTEGER DEFAULT 1 CHECK (VALUE >= 0)",
		"CREATE DOMAIN GO_FP_ARRAY AS INTEGER[1:3]",
		"CREATE TABLE GO_FP_PARENT (ID INTEGER NOT NULL PRIMARY KEY)",
		"CREATE TABLE GO_FP_CHILD (ID INTEGER NOT NULL PRIMARY KEY, PID INTEGER REFERENCES GO_FP_PARENT(ID), N GO_FP_DOMAIN, A GO_FP_ARRAY, CONSTRAINT GO_FP_CHECK CHECK (N >= 0))",
		"CREATE VIEW GO_FP_VIEW AS SELECT ID, N FROM GO_FP_CHILD",
		"CREATE INDEX GO_FP_IDX ON GO_FP_CHILD(N)",
		"CREATE PROCEDURE GO_FP_PROC (A INTEGER) RETURNS (N INTEGER) AS BEGIN N = A; SUSPEND; END",
		"CREATE TRIGGER GO_FP_TRIG FOR GO_FP_CHILD BEFORE INSERT AS BEGIN NEW.N = 1; END",
		"INSERT INTO RDB$TRIGGER_MESSAGES (RDB$TRIGGER_NAME,RDB$MESSAGE_NUMBER,RDB$MESSAGE) VALUES ('GO_FP_TRIG',1,'fixture message')",
		"CREATE GENERATOR GO_FP_GEN",
		"CREATE EXCEPTION GO_FP_EX 'fixture exception'",
		"DECLARE EXTERNAL FUNCTION GO_FP_UDF INTEGER RETURNS INTEGER BY VALUE ENTRY_POINT 'unused' MODULE_NAME 'unused_fixture'",
		"CREATE ROLE GO_FP_ROLE",
		"GRANT SELECT ON GO_FP_CHILD TO GO_FP_ROLE",
		"UPDATE RDB$DATABASE SET RDB$DESCRIPTION = 'fixture fingerprint'",
	} {
		if _, err := c.ExecContext(ctx, query); err != nil {
			t.Fatalf("fixture setup step %d: %v", step, err)
		}
	}
	scan := func() *schema.CatalogFingerprint {
		t.Helper()
		attempt, stop := context.WithTimeout(ctx, 30*time.Second)
		defer stop()
		f, err := interbase.ReadCatalogFingerprint(attempt, c, 64<<20)
		if err != nil {
			t.Fatal(err)
		}
		if f.Stats.BlobOpens != 0 || f.Stats.SourceBytes != 0 {
			t.Fatalf("BLOB materialization %+v", f.Stats)
		}
		return f
	}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	start := time.Now()
	base := scan()
	runtime.GC()
	runtime.ReadMemStats(&after)
	runtime.KeepAlive(base)
	t.Logf("scan elapsed=%s accounted=%d retained-heap-delta=%d stats=%+v", time.Since(start), base.AccountedBytes, int64(after.HeapAlloc)-int64(before.HeapAlloc), base.Stats)
	familyCounts := map[string]uint32{}
	base.VisitRecords(func(r schema.FingerprintRecord) bool {
		familyCounts[r.Family] += r.Count
		return true
	})
	for _, family := range []string{"RELATIONS", "RELATION_FIELDS", "VIEW_RELATIONS", "FIELDS", "FIELD_DIMENSIONS", "PROCEDURES", "PROCEDURE_PARAMETERS", "TRIGGERS", "TRIGGER_MESSAGES", "INDICES", "INDEX_SEGMENTS", "RELATION_CONSTRAINTS", "REF_CONSTRAINTS", "CHECK_CONSTRAINTS", "GENERATORS", "EXCEPTIONS", "FUNCTIONS", "FUNCTION_ARGUMENTS", "DEPENDENCIES", "CHARACTER_SETS", "COLLATIONS", "USER_PRIVILEGES", "ROLES", "SECURITY_CLASSES", "DATABASE"} {
		if familyCounts["RDB$"+family] == 0 {
			t.Fatal("unexercised family", family)
		}
		t.Logf("family %s rows=%d", family, familyCounts["RDB$"+family])
	}
	if !base.Equal(scan()) {
		t.Fatal("unchanged scan differed")
	}
	// User data, generator values and index selectivity are not definitions.
	exec("INSERT INTO GO_FP_PARENT VALUES (1)")
	exec("SET GENERATOR GO_FP_GEN TO 19")
	exec("SET STATISTICS INDEX GO_FP_IDX")
	if !base.Equal(scan()) {
		t.Fatal("nondefinition change affected fingerprint")
	}
	tx, err := c.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.ExecContext(ctx, "ALTER PROCEDURE GO_FP_PROC (A INTEGER) RETURNS (N INTEGER) AS BEGIN N = A + 99; SUSPEND; END"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if !base.Equal(scan()) {
		t.Fatal("rolled back DDL affected fingerprint")
	}
	manager := openServices(t, cleanup, testServiceConfig{host: cfg.Server, user: cfg.User, password: cfg.Password})
	for i := 0; i < 3; i++ {
		exec(fmt.Sprintf("ALTER PROCEDURE GO_FP_PROC (A INTEGER) RETURNS (N INTEGER) AS BEGIN N = A + %d; SUSPEND; END", i+2))
		candidate := scan()
		if base.Equal(candidate) {
			t.Fatal("body-only ALTER not detected")
		}
		base = candidate
		job, err := manager.Sweep(ctx, services.SweepRequest{Database: fixture.Path})
		if err != nil {
			t.Fatal(err)
		}
		if err := job.Wait(ctx); err != nil {
			t.Fatal(err)
		}
		if !base.Equal(scan()) {
			t.Fatal("sweep changed unchanged identities")
		}
	}
	// Observation is deliberately not a guarantee of BLOB-ID uniqueness across
	// future reuse, restore or change/revert cycles.
}

func TestFingerprintCatalogBlobIsIdentity(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()
	if _, err := pooled.ExecContext(ctx, "UPDATE RDB$DATABASE SET RDB$DESCRIPTION = 'fingerprint identity fixture'"); err != nil {
		t.Fatal(err)
	}
	if _, err := pooled.ExecContext(ctx, "CREATE PROCEDURE GO_FINGERPRINT RETURNS (N INTEGER) AS BEGIN N = 1; SUSPEND; END"); err != nil {
		t.Fatal(err)
	}
	var saved [][]interbase.Cell
	stats, err := interbase.ReadFingerprintRows(ctx, pooled, []interbase.FingerprintQuery{
		{SQL: "SELECT RDB$DESCRIPTION FROM RDB$DATABASE"},
		{SQL: "SELECT RDB$PROCEDURE_SOURCE, RDB$PROCEDURE_BLR FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = ?", Args: []any{"GO_FINGERPRINT"}},
	}, func(q int, cols []string, cells []interbase.Cell) error {
		if len(cols) != len(cells) {
			return fmt.Errorf("columns/cells mismatch")
		}
		for _, cell := range cells {
			if _, ok := cell.Value.(interbase.BlobIdentity); !ok {
				return fmt.Errorf("query %d materialized or NULL value %T", q, cell.Value)
			}
		}
		saved = append(saved, cells)
		return nil
	})
	if err != nil || len(saved) != 2 || stats.Queries != 2 || stats.Rows != 2 || stats.BlobOpens != 0 || stats.SourceBytes != 0 {
		t.Fatalf("rows=%d stats=%+v err=%v", len(saved), stats, err)
	}
	// Ordinary SQL materialization remains unchanged, and the same connection
	// is usable immediately after the maintenance transaction ends.
	var description, source string
	var blr []byte
	if err := pooled.QueryRowContext(ctx, "SELECT RDB$DESCRIPTION FROM RDB$DATABASE").Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description != "fingerprint identity fixture" {
		t.Fatalf("description=%q", description)
	}
	if err := pooled.QueryRowContext(ctx, "SELECT RDB$PROCEDURE_SOURCE, RDB$PROCEDURE_BLR FROM RDB$PROCEDURES WHERE RDB$PROCEDURE_NAME = 'GO_FINGERPRINT'").Scan(&source, &blr); err != nil {
		t.Fatal(err)
	}
	if source == "" || len(blr) == 0 {
		t.Fatal("source/BLR fixture missing")
	}
	if saved[0][0].Value == nil || saved[1][0].Value == nil {
		t.Fatal("saved row did not survive completion")
	}
}

func TestFingerprintRejectsCallerTransactionAndMutations(t *testing.T) {
	db := newDatabaseWithDialect(t, 3)
	ctx := readContext(t)
	pooled, err := db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer pooled.Close()
	visit := func(int, []string, []interbase.Cell) error { t.Fatal("unexpected row"); return nil }
	tx, err := pooled.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	if _, err := interbase.ReadFingerprintRows(ctx, pooled, []interbase.FingerprintQuery{{SQL: "SELECT 1 FROM RDB$DATABASE"}}, visit); err == nil {
		t.Fatal("active transaction accepted")
	}
	var n int
	if err := tx.QueryRowContext(ctx, "SELECT 1 FROM RDB$DATABASE").Scan(&n); err != nil {
		t.Fatalf("caller transaction unusable: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
	if _, err := pooled.ExecContext(ctx, "CREATE PROCEDURE GO_FP_MUTATE RETURNS (N INTEGER) AS BEGIN N = 1; SUSPEND; END"); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{"UPDATE RDB$DATABASE SET RDB$DESCRIPTION = 'must not execute'", "EXECUTE PROCEDURE GO_FP_MUTATE", "SELECT RDB$DESCRIPTION FROM RDB$DATABASE FOR UPDATE"} {
		if _, err := interbase.ReadFingerprintRows(ctx, pooled, []interbase.FingerprintQuery{{SQL: query}}, visit); err == nil {
			t.Fatalf("accepted %s", query)
		}
	}
	var description sql.NullString
	if err := pooled.QueryRowContext(ctx, "SELECT RDB$DESCRIPTION FROM RDB$DATABASE").Scan(&description); err != nil {
		t.Fatal(err)
	}
	if description.Valid && description.String == "must not execute" {
		t.Fatal("mutating SQL executed")
	}
	// Callback failure and cancellation leave no cursor/transaction held.
	sentinel := errors.New("visitor stopped")
	if _, err := interbase.ReadFingerprintRows(ctx, pooled, []interbase.FingerprintQuery{{SQL: "SELECT 1 FROM RDB$DATABASE"}}, func(int, []string, []interbase.Cell) error { return sentinel }); !errors.Is(err, sentinel) {
		t.Fatalf("callback: %v", err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := interbase.ReadFingerprintRows(canceled, pooled, []interbase.FingerprintQuery{{SQL: "SELECT 1 FROM RDB$DATABASE"}}, visit); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel: %v", err)
	}
	if err := pooled.QueryRowContext(ctx, "SELECT 1 FROM RDB$DATABASE").Scan(&n); err != nil {
		t.Fatalf("connection reuse: %v", err)
	}
}
