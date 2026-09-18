//go:build integration

package integration_test

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	interbase "interbase-go"
)

const (
	nativeLifecycleRaceEnv        = "INTERBASE_NATIVE_LIFECYCLE_RACE"
	nativeLifecycleRaceWorkers    = 4
	nativeLifecycleRaceIterations = 1000
)

// TestNativeLifecycleRace keeps several attachments busy while another pool
// repeatedly opens and closes physical attachments.  It is opt-in because a
// native client abort terminates the test process instead of returning an
// ordinary Go error.
func TestNativeLifecycleRace(t *testing.T) {
	if os.Getenv(nativeLifecycleRaceEnv) != "1" {
		t.Skip("set INTERBASE_NATIVE_LIFECYCLE_RACE=1 to run the native lifecycle regression")
	}

	fixture, cfg, cleanup := createFixture(t, 1)
	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1, interbase.TransactionOptions{})
	churnDB := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 1, interbase.TransactionOptions{})
	db.SetMaxOpenConns(nativeLifecycleRaceWorkers)
	db.SetMaxIdleConns(nativeLifecycleRaceWorkers)
	churnDB.SetMaxOpenConns(1)
	churnDB.SetMaxIdleConns(0)

	setupCtx, setupCancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer setupCancel()
	for worker := 0; worker < nativeLifecycleRaceWorkers; worker++ {
		if _, err := db.ExecContext(setupCtx,
			"INSERT INTO GO_WRITE (ID, WRITE_VALUE) VALUES (?, 0)", int64(worker+1)); err != nil {
			t.Fatalf("initialize race write row for worker %d: %v", worker, err)
		}
	}
	stmt, err := db.PrepareContext(setupCtx, soakQuery)
	if err != nil {
		t.Fatalf("prepare race query: %v", err)
	}
	defer func() {
		if err := stmt.Close(); err != nil {
			t.Errorf("close race query: %v", err)
		}
	}()

	runCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	start := make(chan struct{})
	var operations atomic.Uint64
	var workers sync.WaitGroup
	var firstErr error
	var firstErrOnce sync.Once
	recordError := func(err error) {
		if err == nil {
			return
		}
		firstErrOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}
	workers.Add(nativeLifecycleRaceWorkers + 1)

	for worker := 0; worker < nativeLifecycleRaceWorkers; worker++ {
		worker := worker
		go func() {
			defer workers.Done()
			<-start
			tracker := new(soakChurnTracker)
			for iteration := 0; iteration < nativeLifecycleRaceIterations; iteration++ {
				id := int64((worker+iteration)%3 + 1)
				if _, err := runSoakIteration(runCtx, db, stmt, churnDB, tracker,
					id, int64(worker+1), iteration, false); err != nil {
					recordError(fmt.Errorf("worker %d iteration %d: %w", worker, iteration, err))
					return
				}
				operations.Add(1)
			}
		}()
	}
	go func() {
		defer workers.Done()
		<-start
		tracker := new(soakChurnTracker)
		for iteration := 0; iteration < nativeLifecycleRaceIterations; iteration++ {
			if err := runSoakChurn(runCtx, churnDB, tracker,
				int64(iteration%3+1)); err != nil {
				recordError(fmt.Errorf("churn iteration %d: %w", iteration, err))
				return
			}
		}
	}()
	close(start)
	workers.Wait()
	if firstErr != nil {
		t.Fatalf("native lifecycle race failed: %v", firstErr)
	}
	if got := operations.Load(); got != nativeLifecycleRaceWorkers*nativeLifecycleRaceIterations {
		t.Fatalf("completed operations = %d, want %d", got,
			nativeLifecycleRaceWorkers*nativeLifecycleRaceIterations)
	}
}
