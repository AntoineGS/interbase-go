//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"reflect"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	interbase "interbase-go"
)

const (
	soakOptInEnv          = "INTERBASE_SOAK"
	soakDurationEnv       = "INTERBASE_SOAK_DURATION"
	soakWorkersEnv        = "INTERBASE_SOAK_WORKERS"
	soakSampleEnv         = "INTERBASE_SOAK_SAMPLE_INTERVAL"
	soakDefaultRuntime    = 15 * time.Second
	soakDefaultWorkers    = 4
	soakDefaultSample     = time.Second
	soakMaxRuntime        = 24 * time.Hour
	soakMaxWorkers        = 64
	soakOperationLimit    = 30 * time.Second
	soakQuery             = "SELECT ID, COUNTRY FROM GO_COUNTRY WHERE ID = ?"
	soakEarlyQuery        = "SELECT ID, COUNTRY FROM GO_COUNTRY ORDER BY ID"
	soakWriteQuery        = "UPDATE GO_WRITE SET WRITE_VALUE = WRITE_VALUE + 1 WHERE ID = ?"
	soakWriteReadQuery    = "SELECT WRITE_VALUE FROM GO_WRITE WHERE ID = ?"
	soakCancellationQuery = "EXECUTE PROCEDURE GO_CANCEL_DELAY"
)

func TestSoakConcurrentWorkload(t *testing.T) {
	if os.Getenv(soakOptInEnv) != "1" {
		t.Skip("set INTERBASE_SOAK=1 to run the concurrent disposable-database soak")
	}
	duration := soakDuration(t, soakDurationEnv, soakDefaultRuntime)
	workers := soakInt(t, soakWorkersEnv, soakDefaultWorkers, 1, soakMaxWorkers)
	sampleInterval := soakDuration(t, soakSampleEnv, soakDefaultSample)
	cancellationEnabled := os.Getenv(cancellationOptIn) == "1"

	schema := fixtureSchema
	if cancellationEnabled {
		schema += cancellationFixtureSchema
	}
	fixture, cfg, cleanup := createFixture(t, 1, schema)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1, interbase.TransactionOptions{})
	churnDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1, interbase.TransactionOptions{})
	cancellationWorkers := 0
	if cancellationEnabled {
		cancellationWorkers = max(1, workers/4)
	}
	db.SetMaxOpenConns(workers + cancellationWorkers)
	db.SetMaxIdleConns(workers + cancellationWorkers)
	churnDB.SetMaxOpenConns(1)
	churnDB.SetMaxIdleConns(0)
	if stats := churnDB.Stats(); stats.OpenConnections != 0 || stats.InUse != 0 || stats.Idle != 0 {
		t.Fatalf("churn pool retained its setup connection after SetMaxIdleConns(0): %+v", stats)
	}

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()
	for worker := 0; worker < workers; worker++ {
		result, err := db.ExecContext(setupCtx, "INSERT INTO GO_WRITE (ID, WRITE_VALUE) VALUES (?, 0)", int64(worker+1))
		if err != nil {
			t.Fatalf("initialize soak write row for worker %d: %v", worker, err)
		}
		affected, err := result.RowsAffected()
		if err != nil {
			t.Fatalf("inspect initialization rows for worker %d: %v", worker, err)
		}
		if affected != 1 {
			t.Fatalf("initialize soak write row for worker %d affected %d rows, want 1", worker, affected)
		}
	}
	stmt, err := db.PrepareContext(setupCtx, soakQuery)
	if err != nil {
		t.Fatalf("prepare soak query: %v", err)
	}
	stmtClosed := false
	t.Cleanup(func() {
		if stmtClosed {
			return
		}
		if err := stmt.Close(); err != nil {
			t.Errorf("close soak query: %v", err)
		}
	})
	durationCtx, durationCancel := context.WithTimeout(context.Background(), duration)
	defer durationCancel()
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	var operations atomic.Uint64
	var earlyCloses atomic.Uint64
	var rowValidations atomic.Uint64
	var earlyCloseRows atomic.Uint64
	var writeAttempts atomic.Uint64
	var committedWrites atomic.Uint64
	var rolledBackWrites atomic.Uint64
	var operationNanos atomic.Uint64
	var maxOperationNanos atomic.Uint64
	var cancellationAttempts atomic.Uint64
	var canceledOperations atomic.Uint64
	committedByWorker := make([]atomic.Uint64, workers)
	rolledBackByWorker := make([]atomic.Uint64, workers)
	var churnTracker soakChurnTracker
	errorsCh := make(chan error, workers)
	var workerGroup sync.WaitGroup
	workerGroup.Add(workers)

	beforeSample := logSoakSample(t, db, churnDB, "before")
	sampleDone := make(chan struct{})
	go sampleSoakResources(t, db, churnDB, durationCtx, sampleInterval, sampleDone)
	workloadStarted := time.Now()

	for worker := 0; worker < workers; worker++ {
		worker := worker
		go func() {
			defer workerGroup.Done()
			for iteration := 0; ; iteration++ {
				select {
				case <-durationCtx.Done():
					return
				case <-runCtx.Done():
					return
				default:
				}

				id := int64((worker+iteration)%3 + 1)
				operationStarted := time.Now()
				operationCtx, operationCancel := context.WithTimeout(runCtx, soakOperationLimit)
				iterationResult, iterationErr := runSoakIteration(operationCtx, db, stmt, churnDB, &churnTracker, id, int64(worker+1), iteration, iteration%4 == 0)
				operationCancel()
				if iterationErr != nil {
					expectedContextStop := runCtx.Err() != nil && soakOnlyContextError(iterationErr)
					if !expectedContextStop {
						select {
						case errorsCh <- fmt.Errorf("worker %d iteration %d: %w", worker, iteration, iterationErr):
						default:
						}
						runCancel()
					}
					return
				}
				elapsedNanos := uint64(time.Since(operationStarted).Nanoseconds())
				operationNanos.Add(elapsedNanos)
				for {
					previous := maxOperationNanos.Load()
					if elapsedNanos <= previous || maxOperationNanos.CompareAndSwap(previous, elapsedNanos) {
						break
					}
				}
				operations.Add(1)
				rowValidations.Add(1)
				earlyCloseRows.Add(1)
				writeAttempts.Add(1)
				switch iterationResult.writeOutcome {
				case soakWriteCommitted:
					committedWrites.Add(1)
					committedByWorker[worker].Add(1)
				case soakWriteRolledBack:
					rolledBackWrites.Add(1)
					rolledBackByWorker[worker].Add(1)
				default:
					select {
					case errorsCh <- fmt.Errorf("worker %d iteration %d returned unknown write outcome %d", worker, iteration, iterationResult.writeOutcome):
					default:
					}
					runCancel()
					return
				}
				if iterationResult.didEarlyClose {
					earlyCloses.Add(1)
				}
			}
		}()
	}
	for worker := 0; worker < cancellationWorkers; worker++ {
		workerGroup.Add(1)
		go func(worker int) {
			defer workerGroup.Done()
			for {
				select {
				case <-durationCtx.Done():
					return
				case <-runCtx.Done():
					return
				default:
				}
				cancellationAttempts.Add(1)
				if err := runSoakCancellation(runCtx, db); err != nil {
					select {
					case errorsCh <- fmt.Errorf("cancellation worker %d: %w", worker, err):
					default:
					}
					runCancel()
					return
				}
				canceledOperations.Add(1)
			}
		}(worker)
	}

	workerGroup.Wait()
	workloadElapsed := time.Since(workloadStarted)
	durationCancel()
	<-sampleDone
	afterWorkloadSample := logSoakSample(t, db, churnDB, "after-workload")
	close(errorsCh)
	for soakErr := range errorsCh {
		t.Fatalf("soak workload failed: %v", soakErr)
	}

	if got := operations.Load(); got == 0 {
		t.Fatal("soak completed without a successful operation")
	}
	if cancellationEnabled && (cancellationAttempts.Load() == 0 || canceledOperations.Load() == 0) {
		t.Fatalf("cancellation soak operations = attempted %d canceled %d; want both nonzero", cancellationAttempts.Load(), canceledOperations.Load())
	}
	if got, want := rowValidations.Load(), operations.Load(); got != want {
		t.Fatalf("row validations = %d, want %d", got, want)
	}
	if got, want := earlyCloseRows.Load(), operations.Load(); got != want {
		t.Fatalf("early-close row validations = %d, want %d", got, want)
	}
	if got, want := earlyCloses.Load(), operations.Load(); got != want {
		t.Fatalf("early row closes = %d, want %d", got, want)
	}
	if err := stmt.Close(); err != nil {
		t.Fatalf("close soak query before verification: %v", err)
	}
	stmtClosed = true
	verifyCtx, verifyCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer verifyCancel()
	var persistedWrites int64
	var expectedWrites int64
	for worker := 0; worker < workers; worker++ {
		committed := committedByWorker[worker].Load()
		rolledBack := rolledBackByWorker[worker].Load()
		if committed == 0 || rolledBack == 0 {
			t.Fatalf("worker %d write outcomes = committed %d, rolled back %d; want both outcomes", worker, committed, rolledBack)
		}
		var persisted int64
		if err := db.QueryRowContext(verifyCtx, soakWriteReadQuery, int64(worker+1)).Scan(&persisted); err != nil {
			t.Fatalf("read persisted write value for worker %d: %v", worker, err)
		}
		expected := int64(committed)
		if persisted != expected {
			t.Fatalf("worker %d persisted write value = %d, want committed count %d (rolled-back writes must be absent)", worker, persisted, expected)
		}
		persistedWrites += persisted
		expectedWrites += expected
	}
	if err := validateSoakWriteMetrics(operations.Load(), writeAttempts.Load(), committedWrites.Load(), rolledBackWrites.Load(), persistedWrites, expectedWrites); err != nil {
		t.Fatal(err)
	}
	churnMetrics := churnTracker.snapshot()
	if err := validateSoakChurnMetrics(churnMetrics); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("close soak database before after-close sample: %v", err)
	}
	if err := churnDB.Close(); err != nil {
		t.Fatalf("close churn database before after-close sample: %v", err)
	}
	afterCloseSample := logSoakSample(t, db, churnDB, "after-close")
	assertSoakResourceComparison(t, beforeSample, afterWorkloadSample, afterCloseSample)
	throughput := float64(0)
	if workloadElapsed > 0 {
		throughput = float64(operations.Load()) / workloadElapsed.Seconds()
	}
	averageLatency := time.Duration(operationNanos.Load() / operations.Load())
	t.Logf("soak summary: elapsed=%s configured_duration=%s workers=%d cancellation_workers=%d operations=%d cancellation_attempts=%d canceled_operations=%d row_validations=%d early_close_rows=%d early_row_closes=%d write_attempts=%d committed_writes=%d rolled_back_writes=%d persisted_write_values=%d throughput_ops_per_second=%.2f average_operation_latency=%s max_operation_latency=%s", workloadElapsed, duration, workers, cancellationWorkers, operations.Load(), cancellationAttempts.Load(), canceledOperations.Load(), rowValidations.Load(), earlyCloseRows.Load(), earlyCloses.Load(), writeAttempts.Load(), committedWrites.Load(), rolledBackWrites.Load(), persistedWrites, throughput, averageLatency, time.Duration(maxOperationNanos.Load()))
	t.Logf("soak churn: borrow_return_operations=%d physical_opens=%d physical_closes=%d attachment_identity_replacements=%d prepare_recreations=%d", churnMetrics.borrowReturns, churnMetrics.physicalOpens, churnMetrics.physicalCloses, churnMetrics.identityReplacements, churnMetrics.prepareRecreations)
	t.Log("soak note: database/sql OpenConnections is the available pool attachment proxy; exact native allocation counters and server-wide attachment counts are unavailable")
}

func runSoakCancellation(parent context.Context, db *sql.DB) error {
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		_, err := db.ExecContext(ctx, soakCancellationQuery)
		result <- err
	}()
	timer := time.NewTimer(cancellationStartDelay)
	defer timer.Stop()
	select {
	case err := <-result:
		if err == nil {
			return errors.New("cancellation query completed before cancellation request")
		}
		return fmt.Errorf("cancellation query returned before request: %w", err)
	case <-timer.C:
		cancel()
	}
	select {
	case err := <-result:
		if err == nil || !errors.Is(err, context.Canceled) {
			return fmt.Errorf("cancellation query error = %v, want context cancellation", err)
		}
		return nil
	case <-time.After(cancellationResultLimit):
		return errors.New("cancellation query did not return before its live bound")
	}
}

type soakWriteOutcome uint8

const (
	soakWriteUnknown soakWriteOutcome = iota
	soakWriteCommitted
	soakWriteRolledBack
)

type soakIterationResult struct {
	didEarlyClose bool
	writeOutcome  soakWriteOutcome
}

func runSoakIteration(ctx context.Context, db *sql.DB, prepared *sql.Stmt, churnDB *sql.DB, churnTracker *soakChurnTracker, id, workerID int64, iteration int, churn bool) (soakIterationResult, error) {
	result := soakIterationResult{}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	finished := false
	defer func() {
		if !finished {
			_ = tx.Rollback()
		}
	}()

	writeResult, err := tx.ExecContext(ctx, soakWriteQuery, workerID)
	if err != nil {
		return result, err
	}
	if affected, err := writeResult.RowsAffected(); err != nil {
		return result, err
	} else if affected != 1 {
		return result, fmt.Errorf("soak write affected %d rows for worker %d, want 1", affected, workerID)
	}

	var preparedID int64
	var preparedCountry string
	if err := tx.StmtContext(ctx, prepared).QueryRowContext(ctx, id).Scan(&preparedID, &preparedCountry); err != nil {
		return result, err
	}
	if preparedID != id || soakCountry(id) != preparedCountry {
		return result, fmt.Errorf("prepared row = (%d, %q), want (%d, %q)", preparedID, preparedCountry, id, soakCountry(id))
	}

	rows, err := tx.QueryContext(ctx, soakEarlyQuery)
	if err != nil {
		return result, err
	}
	if !rows.Next() {
		rowsErr := rows.Err()
		closeErr := rows.Close()
		if rowsErr == nil {
			rowsErr = errors.New("early-close query returned no rows")
		}
		if closeErr != nil {
			rowsErr = errors.Join(rowsErr, closeErr)
		}
		return result, rowsErr
	}
	var firstID int64
	var firstCountry string
	if err := rows.Scan(&firstID, &firstCountry); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return result, errors.Join(err, closeErr)
		}
		return result, err
	}
	closeErr := rows.Close()
	if closeErr != nil {
		return result, closeErr
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	if firstID != 1 || firstCountry != soakCountry(1) {
		return result, fmt.Errorf("early-close row = (%d, %q), want (1, %q)", firstID, firstCountry, soakCountry(1))
	}
	result.didEarlyClose = true

	if iteration%2 == 0 {
		if err := tx.Commit(); err != nil {
			return result, err
		}
		finished = true
		result.writeOutcome = soakWriteCommitted
	} else {
		if err := tx.Rollback(); err != nil {
			return result, err
		}
		finished = true
		result.writeOutcome = soakWriteRolledBack
	}
	if !churn {
		return result, nil
	}
	if err := runSoakChurn(ctx, churnDB, churnTracker, id); err != nil {
		return result, err
	}
	return result, nil
}

type soakChurnMetrics struct {
	borrowReturns        uint64
	physicalOpens        uint64
	physicalCloses       uint64
	identityReplacements uint64
	prepareRecreations   uint64
}

type soakChurnTracker struct {
	cycleMu sync.Mutex
	stateMu sync.Mutex

	lastIdentity uintptr
	previousRaw  any
	metrics      soakChurnMetrics
}

func (t *soakChurnTracker) observe(connection *sql.Conn) error {
	var identity uintptr
	var raw any
	if err := connection.Raw(func(driverConnection any) error {
		value := reflect.ValueOf(driverConnection)
		if !value.IsValid() || value.Kind() != reflect.Ptr || value.IsNil() {
			return fmt.Errorf("churn connection has unexpected driver value %T", driverConnection)
		}
		identity = value.Pointer()
		raw = driverConnection
		return nil
	}); err != nil {
		return fmt.Errorf("inspect churn connection identity: %w", err)
	}
	if identity == 0 {
		return errors.New("churn connection has an empty driver identity")
	}

	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	replacement := t.metrics.physicalOpens != 0
	if replacement && identity == t.lastIdentity {
		return fmt.Errorf("churn connection identity %x was reused without a replacement", identity)
	}
	t.metrics.physicalOpens++
	if replacement {
		t.metrics.identityReplacements++
	}
	t.lastIdentity = identity
	// Keep the immediately preceding wrapper alive so a Go allocator address
	// cannot be reused for the next physical connection before comparison.
	t.previousRaw = raw
	return nil
}

func (t *soakChurnTracker) recordPrepare() {
	t.stateMu.Lock()
	t.metrics.prepareRecreations++
	t.stateMu.Unlock()
}

func (t *soakChurnTracker) recordClose() {
	t.stateMu.Lock()
	t.metrics.physicalCloses++
	t.metrics.borrowReturns++
	t.stateMu.Unlock()
}

func (t *soakChurnTracker) snapshot() soakChurnMetrics {
	t.stateMu.Lock()
	defer t.stateMu.Unlock()
	return t.metrics
}

func runSoakChurn(ctx context.Context, churnDB *sql.DB, tracker *soakChurnTracker, id int64) error {
	tracker.cycleMu.Lock()
	defer tracker.cycleMu.Unlock()

	connection, err := churnDB.Conn(ctx)
	if err != nil {
		return err
	}
	if stats := churnDB.Stats(); stats.OpenConnections != 1 || stats.InUse != 1 || stats.Idle != 0 {
		return closeSoakChurnConnection(connection, fmt.Errorf("churn borrow stats = %+v, want one in-use connection", stats))
	}
	if err := tracker.observe(connection); err != nil {
		return closeSoakChurnConnection(connection, err)
	}

	statement, err := connection.PrepareContext(ctx, soakQuery)
	if err != nil {
		return closeSoakChurnConnection(connection, fmt.Errorf("prepare churn query: %w", err))
	}
	var gotID int64
	var gotCountry string
	queryErr := statement.QueryRowContext(ctx, id).Scan(&gotID, &gotCountry)
	if queryErr == nil && (gotID != id || gotCountry != soakCountry(id)) {
		queryErr = fmt.Errorf("churn row = (%d, %q), want (%d, %q)", gotID, gotCountry, id, soakCountry(id))
	}
	statementCloseErr := statement.Close()
	statsBeforeClose := churnDB.Stats()
	connectionCloseErr := connection.Close()
	statsAfterClose := churnDB.Stats()

	var operationErr error
	if queryErr != nil {
		operationErr = queryErr
	}
	if statementCloseErr != nil {
		operationErr = errors.Join(operationErr, statementCloseErr)
	}
	if connectionCloseErr != nil {
		operationErr = errors.Join(operationErr, connectionCloseErr)
	}
	if operationErr != nil {
		return operationErr
	}
	if statsAfterClose.OpenConnections != 0 || statsAfterClose.InUse != 0 || statsAfterClose.Idle != 0 {
		return fmt.Errorf("churn close stats = %+v, want no pooled connection", statsAfterClose)
	}
	if statsAfterClose.MaxIdleClosed != statsBeforeClose.MaxIdleClosed+1 {
		return fmt.Errorf("churn close count changed MaxIdleClosed from %d to %d, want one close", statsBeforeClose.MaxIdleClosed, statsAfterClose.MaxIdleClosed)
	}

	tracker.recordPrepare()
	tracker.recordClose()
	return nil
}

func closeSoakChurnConnection(connection *sql.Conn, operationErr error) error {
	if closeErr := connection.Close(); closeErr != nil {
		return errors.Join(operationErr, closeErr)
	}
	return operationErr
}

func validateSoakChurnMetrics(metrics soakChurnMetrics) error {
	if metrics.borrowReturns == 0 {
		return errors.New("soak churn did not borrow any dedicated connections")
	}
	if metrics.physicalOpens != metrics.borrowReturns {
		return fmt.Errorf("soak churn physical opens = %d, borrow returns = %d", metrics.physicalOpens, metrics.borrowReturns)
	}
	if metrics.physicalCloses != metrics.borrowReturns {
		return fmt.Errorf("soak churn physical closes = %d, borrow returns = %d", metrics.physicalCloses, metrics.borrowReturns)
	}
	if metrics.prepareRecreations != metrics.physicalOpens {
		return fmt.Errorf("soak churn prepare recreations = %d, physical opens = %d", metrics.prepareRecreations, metrics.physicalOpens)
	}
	if metrics.identityReplacements == 0 {
		return errors.New("soak churn did not observe an attachment identity replacement")
	}
	if metrics.identityReplacements+1 != metrics.physicalOpens {
		return fmt.Errorf("soak churn identity replacements = %d, physical opens = %d", metrics.identityReplacements, metrics.physicalOpens)
	}
	return nil
}

func validateSoakWriteMetrics(operations, writes, committed, rolledBack uint64, persisted, expected int64) error {
	if operations == 0 {
		return errors.New("soak completed without a write operation")
	}
	if writes != operations {
		return fmt.Errorf("soak write attempts = %d, operations = %d", writes, operations)
	}
	if committed == 0 || rolledBack == 0 {
		return fmt.Errorf("soak write outcomes = committed %d, rolled back %d; want both outcomes", committed, rolledBack)
	}
	if committed+rolledBack != operations {
		return fmt.Errorf("soak write outcomes = committed %d + rolled back %d, operations = %d", committed, rolledBack, operations)
	}
	if persisted != expected {
		return fmt.Errorf("soak persisted write values = %d, expected committed values = %d", persisted, expected)
	}
	return nil
}

func soakOnlyContextError(err error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !soakOnlyContextError(child) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return soakOnlyContextError(wrapped.Unwrap())
	}
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func soakCountry(id int64) string {
	switch id {
	case 1:
		return "USA"
	case 2:
		return "England"
	case 3:
		return "Japan"
	default:
		return ""
	}
}

type soakResourceSample struct {
	heapAlloc         uint64
	heapInUse         uint64
	heapObjects       uint64
	goroutines        int
	processRSS        uint64
	rssAvailable      bool
	openFileDescs     int
	fdsAvailable      bool
	workloadPoolStats sql.DBStats
	churnPoolStats    sql.DBStats
}

func sampleSoakResources(t *testing.T, db, churnDB *sql.DB, ctx context.Context, interval time.Duration, done chan<- struct{}) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	defer close(done)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			logSoakSample(t, db, churnDB, "during")
		}
	}
}

func logSoakSample(t *testing.T, db, churnDB *sql.DB, phase string) soakResourceSample {
	sample := captureSoakSample(db, churnDB)
	rss := "unavailable"
	if sample.rssAvailable {
		rss = strconv.FormatUint(sample.processRSS, 10)
	}
	fds := "unavailable"
	if sample.fdsAvailable {
		fds = strconv.Itoa(sample.openFileDescs)
	}
	t.Logf("soak resources phase=%s heap_alloc_bytes=%d heap_inuse_bytes=%d heap_objects=%d goroutines=%d process_rss_bytes=%s open_file_descriptors=%s workload_pool_open_connections=%d workload_pool_in_use=%d workload_pool_idle=%d workload_pool_wait_count=%d workload_pool_wait_duration=%s churn_pool_open_connections=%d churn_pool_in_use=%d churn_pool_idle=%d churn_pool_wait_count=%d churn_pool_wait_duration=%s", phase, sample.heapAlloc, sample.heapInUse, sample.heapObjects, sample.goroutines, rss, fds, sample.workloadPoolStats.OpenConnections, sample.workloadPoolStats.InUse, sample.workloadPoolStats.Idle, sample.workloadPoolStats.WaitCount, sample.workloadPoolStats.WaitDuration, sample.churnPoolStats.OpenConnections, sample.churnPoolStats.InUse, sample.churnPoolStats.Idle, sample.churnPoolStats.WaitCount, sample.churnPoolStats.WaitDuration)
	return sample
}

func captureSoakSample(db, churnDB *sql.DB) soakResourceSample {
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	rss, rssAvailable := processRSSBytes()
	fds, fdsAvailable := openFileDescriptorCount()
	return soakResourceSample{
		heapAlloc:         memory.HeapAlloc,
		heapInUse:         memory.HeapInuse,
		heapObjects:       memory.HeapObjects,
		goroutines:        runtime.NumGoroutine(),
		processRSS:        rss,
		rssAvailable:      rssAvailable,
		openFileDescs:     fds,
		fdsAvailable:      fdsAvailable,
		workloadPoolStats: db.Stats(),
		churnPoolStats:    churnDB.Stats(),
	}
}

func assertSoakResourceComparison(t *testing.T, before, afterWorkload, afterClose soakResourceSample) {
	t.Helper()
	if stats := afterWorkload.churnPoolStats; stats.OpenConnections != 0 || stats.InUse != 0 || stats.Idle != 0 {
		t.Fatalf("churn pool retained connections after workload: %+v", stats)
	}
	if stats := afterClose.workloadPoolStats; stats.OpenConnections != 0 || stats.InUse != 0 || stats.Idle != 0 {
		t.Fatalf("workload pool remained open after cleanup: %+v", stats)
	}
	if stats := afterClose.churnPoolStats; stats.OpenConnections != 0 || stats.InUse != 0 || stats.Idle != 0 {
		t.Fatalf("churn pool remained open after cleanup: %+v", stats)
	}
	if before.fdsAvailable && afterClose.fdsAvailable && afterClose.openFileDescs > before.openFileDescs {
		t.Fatalf("file descriptors after cleanup = %d, baseline = %d", afterClose.openFileDescs, before.openFileDescs)
	}
	if afterClose.goroutines > before.goroutines {
		t.Fatalf("goroutines after cleanup = %d, baseline = %d", afterClose.goroutines, before.goroutines)
	}
}

func processRSSBytes() (uint64, bool) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "VmRSS:" || fields[2] != "kB" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0, false
		}
		return value * 1024, true
	}
	return 0, false
}

func openFileDescriptorCount() (int, bool) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	return len(entries), true
}

func soakDuration(t *testing.T, name string, fallback time.Duration) time.Duration {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 || duration > soakMaxRuntime {
		t.Fatalf("%s = %q is invalid; want a duration in (0, %s]", name, value, soakMaxRuntime)
	}
	return duration
}

func soakInt(t *testing.T, name string, fallback, minimum, maximum int) int {
	t.Helper()
	value := os.Getenv(name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed < minimum || parsed > maximum {
		t.Fatalf("%s = %q is invalid; want an integer in [%d, %d]", name, value, minimum, maximum)
	}
	return parsed
}

func TestValidateSoakChurnMetricsRequiresPhysicalReplacement(t *testing.T) {
	tests := []struct {
		name    string
		metrics soakChurnMetrics
		wantErr bool
	}{
		{
			name: "physical replacements",
			metrics: soakChurnMetrics{
				borrowReturns:        3,
				physicalOpens:        3,
				physicalCloses:       3,
				identityReplacements: 2,
				prepareRecreations:   3,
			},
		},
		{
			name: "borrow and return only",
			metrics: soakChurnMetrics{
				borrowReturns: 3,
			},
			wantErr: true,
		},
		{
			name: "no identity replacement",
			metrics: soakChurnMetrics{
				borrowReturns:        3,
				physicalOpens:        3,
				physicalCloses:       3,
				identityReplacements: 0,
				prepareRecreations:   3,
			},
			wantErr: true,
		},
		{
			name: "prepare count mismatch",
			metrics: soakChurnMetrics{
				borrowReturns:        3,
				physicalOpens:        3,
				physicalCloses:       3,
				identityReplacements: 2,
				prepareRecreations:   2,
			},
			wantErr: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSoakChurnMetrics(test.metrics)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSoakChurnMetrics(%+v) error = %v, wantErr=%t", test.metrics, err, test.wantErr)
			}
		})
	}
}

func TestValidateSoakWriteMetricsRequiresCommittedAndRolledBackPersistence(t *testing.T) {
	tests := []struct {
		name       string
		operations uint64
		writes     uint64
		committed  uint64
		rolledBack uint64
		persisted  int64
		expected   int64
		wantErr    bool
	}{
		{
			name:       "committed and rolled back",
			operations: 4,
			writes:     4,
			committed:  2,
			rolledBack: 2,
			persisted:  2,
			expected:   2,
		},
		{
			name:       "missing rollback",
			operations: 4,
			writes:     4,
			committed:  4,
			persisted:  4,
			expected:   2,
			wantErr:    true,
		},
		{
			name:       "rollback persisted",
			operations: 4,
			writes:     4,
			committed:  2,
			rolledBack: 2,
			persisted:  3,
			expected:   2,
			wantErr:    true,
		},
		{
			name:       "write count mismatch",
			operations: 4,
			writes:     3,
			committed:  2,
			rolledBack: 2,
			persisted:  2,
			expected:   2,
			wantErr:    true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSoakWriteMetrics(test.operations, test.writes, test.committed, test.rolledBack, test.persisted, test.expected)
			if (err != nil) != test.wantErr {
				t.Fatalf("validateSoakWriteMetrics(%d, %d, %d, %d, %d, %d) error = %v, wantErr=%t", test.operations, test.writes, test.committed, test.rolledBack, test.persisted, test.expected, err, test.wantErr)
			}
		})
	}
}

func TestSoakOnlyContextErrorDoesNotMaskNativeError(t *testing.T) {
	if !soakOnlyContextError(fmt.Errorf("operation canceled: %w", context.Canceled)) {
		t.Fatal("wrapped context cancellation was not recognized")
	}
	if soakOnlyContextError(errors.Join(context.DeadlineExceeded, errors.New("native close failed"))) {
		t.Fatal("joined native error was treated as an expected duration stop")
	}
	if soakOnlyContextError(errors.New("native query failed")) {
		t.Fatal("native error was treated as an expected duration stop")
	}
}
