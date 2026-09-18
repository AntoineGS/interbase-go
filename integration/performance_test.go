//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"strconv"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/internal/testfixture"
)

const (
	performanceOptIn = "1"
	performanceEnv   = "INTERBASE_PERF"
	performanceQuery = "SELECT COUNTRY FROM GO_COUNTRY WHERE ID = ?"
	largeResultQuery = `
SELECT a.ID, b.ID, c.ID, d.ID, e.ID, f.ID, g.ID
FROM GO_COUNTRY a, GO_COUNTRY b, GO_COUNTRY c, GO_COUNTRY d,
     GO_COUNTRY e, GO_COUNTRY f, GO_COUNTRY g`
	performanceBlobSize = 256 << 10
)

func BenchmarkLiveSmallQuery(b *testing.B) {
	requirePerformanceOptIn(b)
	_, _, db := newPerformanceDatabase(b)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var country string
		id := int64(i%3 + 1)
		if err := db.QueryRowContext(ctx, performanceQuery, id).Scan(&country); err != nil {
			b.Fatalf("small query for country %d: %v", id, err)
		}
		if country == "" {
			b.Fatal("small query returned an empty country")
		}
	}
}

func BenchmarkLivePreparedQuery(b *testing.B) {
	requirePerformanceOptIn(b)
	_, _, db := newPerformanceDatabase(b)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	stmt, err := db.PrepareContext(ctx, performanceQuery)
	if err != nil {
		b.Fatalf("prepare live query: %v", err)
	}
	b.Cleanup(func() {
		if err := stmt.Close(); err != nil {
			b.Errorf("close prepared live query: %v", err)
		}
	})

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var country string
		id := int64(i%3 + 1)
		if err := stmt.QueryRowContext(ctx, id).Scan(&country); err != nil {
			b.Fatalf("prepared query for country %d: %v", id, err)
		}
		if country == "" {
			b.Fatal("prepared query returned an empty country")
		}
	}
}

func BenchmarkLiveLargeResult(b *testing.B) {
	requirePerformanceOptIn(b)
	_, _, db := newPerformanceDatabase(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	var values [7]int64
	destinations := make([]any, len(values))
	for i := range values {
		destinations[i] = &values[i]
	}
	const expectedRows = 3 * 3 * 3 * 3 * 3 * 3 * 3

	b.ReportAllocs()
	b.ResetTimer()
	start := time.Now()
	rowsRead := 0
	for i := 0; i < b.N; i++ {
		rows, err := db.QueryContext(ctx, largeResultQuery)
		if err != nil {
			b.Fatalf("large-result query: %v", err)
		}
		count := 0
		for rows.Next() {
			if err := rows.Scan(destinations...); err != nil {
				_ = rows.Close()
				b.Fatalf("large-result scan: %v", err)
			}
			for column, value := range values {
				if value < 1 || value > 3 {
					_ = rows.Close()
					b.Fatalf("large-result value at column %d = %d, want 1..3", column, value)
				}
			}
			count++
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			b.Fatalf("large-result rows error: %v", err)
		}
		if err := rows.Close(); err != nil {
			b.Fatalf("close large-result rows: %v", err)
		}
		if count != expectedRows {
			b.Fatalf("large-result row count = %d, want %d", count, expectedRows)
		}
		rowsRead += count
	}
	b.StopTimer()
	b.ReportMetric(float64(expectedRows), "rows/op")
	if elapsed := time.Since(start); elapsed > 0 {
		b.ReportMetric(float64(rowsRead)/elapsed.Seconds(), "rows/s")
	}
}

func BenchmarkLiveStreamingBlob(b *testing.B) {
	requirePerformanceOptIn(b)
	fixture, cfg, db := newPerformanceDatabase(b)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	blob := make([]byte, performanceBlobSize)
	for i := range blob {
		blob[i] = byte((i*31 + 17) % 251)
	}
	result, err := db.ExecContext(ctx,
		"INSERT INTO GO_DATA (ID, BINARY_BLOB) VALUES (?, ?)", int64(10), blob)
	if err != nil {
		b.Fatalf("insert benchmark BLOB: %v", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil || rowsAffected != 1 {
		b.Fatalf("benchmark BLOB insert affected (%d, %v), want (1, nil)", rowsAffected, err)
	}
	if err := db.Close(); err != nil {
		b.Fatalf("close setup database before direct BLOB benchmark: %v", err)
	}

	attachment, err := interbase.Open(ctx, interbase.Config{
		Database: fixture.ConnectionString(),
		User:     cfg.User,
		Password: cfg.Password,
		Dialect:  cfg.Dialect,
	})
	if err != nil {
		b.Fatalf("open direct attachment for BLOB benchmark: %v", err)
	}
	b.Cleanup(func() {
		if err := attachment.Close(); err != nil {
			b.Errorf("close direct BLOB benchmark attachment: %v", err)
		}
	})
	tx, err := attachment.BeginTx(ctx, interbase.TransactionOptions{})
	if err != nil {
		b.Fatalf("begin direct BLOB benchmark transaction: %v", err)
	}
	b.Cleanup(func() { _ = tx.Rollback() })
	cursor, err := tx.Query(ctx, "SELECT BINARY_BLOB FROM GO_DATA WHERE ID = ?", int64(10))
	if err != nil {
		b.Fatalf("query benchmark BLOB reference: %v", err)
	}
	if hasRow, err := cursor.Next(ctx); err != nil || !hasRow {
		_ = cursor.Close()
		b.Fatalf("advance benchmark BLOB reference cursor = (%t, %v), want (true, nil)", hasRow, err)
	}
	row, err := cursor.Row()
	if err != nil {
		_ = cursor.Close()
		b.Fatalf("read benchmark BLOB reference row: %v", err)
	}
	if err := cursor.Close(); err != nil {
		b.Fatalf("close benchmark BLOB reference cursor: %v", err)
	}
	if len(row) != 1 {
		b.Fatalf("benchmark BLOB reference columns = %d, want 1", len(row))
	}
	ref, ok := row[0].Value.(interbase.BlobRef)
	if !ok {
		b.Fatalf("benchmark BLOB reference type = %T, want interbase.BlobRef", row[0].Value)
	}

	buffer := make([]byte, 32*1024)
	b.ReportAllocs()
	b.SetBytes(int64(len(blob)))
	b.ResetTimer()
	start := time.Now()
	bytesRead := int64(0)
	for i := 0; i < b.N; i++ {
		reader, err := tx.OpenBlob(ctx, ref)
		if err != nil {
			b.Fatalf("open benchmark BLOB stream: %v", err)
		}
		read, readErr := readBenchmarkBlob(reader, buffer)
		closeErr := reader.Close()
		if readErr != nil {
			b.Fatalf("read benchmark BLOB stream: %v", readErr)
		}
		if closeErr != nil {
			b.Fatalf("close benchmark BLOB stream: %v", closeErr)
		}
		if read != int64(len(blob)) {
			b.Fatalf("benchmark BLOB bytes read = %d, want %d", read, len(blob))
		}
		bytesRead += read
	}
	b.StopTimer()
	if elapsed := time.Since(start); elapsed > 0 {
		b.ReportMetric(float64(bytesRead)/elapsed.Seconds(), "bytes/s")
	}
}

func requirePerformanceOptIn(b *testing.B) {
	b.Helper()
	if os.Getenv(performanceEnv) != performanceOptIn {
		b.Skip("set INTERBASE_PERF=1 to run live disposable-database benchmarks")
	}
}

func newPerformanceDatabase(b *testing.B) (*testfixture.Database, testfixture.Config, *sql.DB) {
	b.Helper()
	cfg, err := testfixture.FromEnv()
	if err != nil {
		b.Fatalf("performance fixture configuration: %v", err)
	}
	cfg.Dialect = 1
	ctx, cancel := context.WithTimeout(context.Background(), fixtureSetupTimeout)
	defer cancel()
	fixture, createErr := testfixture.Create(ctx, cfg, fixtureSchema)
	if fixture != nil {
		b.Cleanup(func() {
			if err := fixture.Close(); err != nil {
				b.Errorf("performance fixture cleanup: %v", err)
			}
		})
	}
	if createErr != nil {
		b.Fatalf("create performance fixture: %v", createErr)
	}

	connector, err := interbase.NewConnector(interbase.Config{
		Database: fixture.ConnectionString(),
		User:     cfg.User,
		Password: cfg.Password,
		Dialect:  cfg.Dialect,
	})
	if err != nil {
		b.Fatalf("performance fixture connector: %v", err)
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(8)
	db.SetMaxIdleConns(8)
	b.Cleanup(func() {
		if err := db.Close(); err != nil {
			b.Errorf("close performance database: %v", err)
		}
	})
	pingCtx, pingCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer pingCancel()
	if err := db.PingContext(pingCtx); err != nil {
		b.Fatalf("ping performance database: %v", err)
	}
	return fixture, cfg, db
}

func readBenchmarkBlob(reader io.Reader, buffer []byte) (int64, error) {
	if reader == nil {
		return 0, errors.New("nil BLOB reader")
	}
	var total int64
	noProgress := 0
	for {
		count, err := reader.Read(buffer)
		if count < 0 || count > len(buffer) {
			return total, errors.New("BLOB reader returned an invalid byte count")
		}
		total += int64(count)
		if err != nil {
			if errors.Is(err, io.EOF) {
				return total, nil
			}
			return total, err
		}
		if count == 0 {
			noProgress++
			if noProgress >= 100 {
				return total, io.ErrNoProgress
			}
		} else {
			noProgress = 0
		}
	}
}

func benchmarkIntString(value int) string {
	return strconv.Itoa(value)
}
