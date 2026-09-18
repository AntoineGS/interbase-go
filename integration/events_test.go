//go:build integration

package integration_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"testing"
	"time"

	interbase "interbase-go"
	"interbase-go/events"
)

func createEventTrigger(t *testing.T, db *sql.DB, ctx context.Context, eventA, eventB string) {
	t.Helper()
	_, err := db.ExecContext(ctx, fmt.Sprintf(`
CREATE TRIGGER GO_EVENTS_AI FOR GO_WRITE ACTIVE AFTER INSERT POSITION 0
AS
BEGIN
  POST_EVENT '%s';
  POST_EVENT '%s';
END`, eventA, eventB))
	if err != nil {
		t.Fatalf("create event trigger: %v", err)
	}
}

func TestEventsAccumulateNotifications(t *testing.T) {
	eventA := uniqueIntegrationIdentifier("EVENT_A")
	eventB := uniqueIntegrationIdentifier("EVENT_B")
	fixture, cfg, cleanup := createFixture(t, 3)
	ctx := readContextWithTimeout(t, 30*time.Second)

	subscription, err := events.Subscribe(ctx, interbase.Config{
		Database: fixture.ConnectionString(),
		User:     cfg.User,
		Password: cfg.Password,
		Dialect:  3,
	}, eventA, eventB)
	if err != nil {
		t.Fatalf("subscribe to fixture events: %v", err)
	}
	cleanup.addAttachment(subscription)

	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	createEventTrigger(t, db, ctx, eventA, eventB)
	for _, id := range []int64{101, 102} {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
			id, id, "event"); err != nil {
			t.Fatalf("insert event row %d: %v", id, err)
		}
	}

	counts := map[string]uint64{}
	for counts[eventA] < 2 || counts[eventB] < 2 {
		nextCounts, err := subscription.Next(ctx)
		if err != nil {
			t.Fatalf("read event counts: %v", err)
		}
		for name, count := range nextCounts {
			counts[name] += count
		}
	}
	if counts[eventA] != 2 || counts[eventB] != 2 {
		t.Fatalf("event counts across notification batches = %#v, want both events=2", counts)
	}
}

func TestEventsCanceledNextPreservesNotification(t *testing.T) {
	eventA := uniqueIntegrationIdentifier("EVENT_A")
	eventB := uniqueIntegrationIdentifier("EVENT_B")
	fixture, cfg, cleanup := createFixture(t, 3)
	ctx := readContextWithTimeout(t, 30*time.Second)

	subscription, err := events.Subscribe(ctx, interbase.Config{
		Database: fixture.ConnectionString(),
		User:     cfg.User,
		Password: cfg.Password,
		Dialect:  3,
	}, eventA)
	if err != nil {
		t.Fatalf("subscribe to fixture event: %v", err)
	}
	cleanup.addAttachment(subscription)

	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	createEventTrigger(t, db, ctx, eventA, eventB)
	if _, err := db.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		103, 103, "pending event"); err != nil {
		t.Fatalf("insert pending event row: %v", err)
	}

	canceledCtx, cancel := context.WithCancel(ctx)
	cancel()
	_, err = subscription.Next(canceledCtx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Next() with canceled context error = %v, want context canceled", err)
	}

	counts, err := subscription.Next(ctx)
	if err != nil {
		t.Fatalf("read event after canceled Next: %v", err)
	}
	if counts[eventA] != 1 {
		t.Fatalf("event count after canceled Next = %#v, want %s=1", counts, eventA)
	}
}

func TestEventsRequireCommitAndIgnoreRollback(t *testing.T) {
	eventA := uniqueIntegrationIdentifier("EVENT_A")
	eventB := uniqueIntegrationIdentifier("EVENT_B")
	fixture, cfg, cleanup := createFixture(t, 3)
	ctx := readContextWithTimeout(t, 30*time.Second)

	subscription, err := events.Subscribe(ctx, interbase.Config{
		Database: fixture.ConnectionString(),
		User:     cfg.User,
		Password: cfg.Password,
		Dialect:  3,
	}, eventA)
	if err != nil {
		t.Fatalf("subscribe to fixture event: %v", err)
	}
	cleanup.addAttachment(subscription)

	db := openDatabase(t, cleanup, fixture.ConnectionString(), cfg.User, cfg.Password, "", 3,
		interbase.TransactionOptions{})
	createEventTrigger(t, db, ctx, eventA, eventB)

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin uncommitted event transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		201, 201, "uncommitted event"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert uncommitted event row: %v", err)
	}
	assertNoEventBeforeDeadline(t, subscription, ctx, "uncommitted insert")
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback event transaction: %v", err)
	}
	assertNoEventBeforeDeadline(t, subscription, ctx, "rolled-back insert")

	tx, err = db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin committed event transaction: %v", err)
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO GO_WRITE (ID, WRITE_VALUE, LABEL) VALUES (?, ?, ?)",
		202, 202, "committed event"); err != nil {
		_ = tx.Rollback()
		t.Fatalf("insert committed event row: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit event transaction: %v", err)
	}

	counts, err := subscription.Next(ctx)
	if err != nil {
		t.Fatalf("read committed event counts: %v", err)
	}
	if counts[eventA] != 1 {
		t.Fatalf("committed event counts = %#v, want %s=1", counts, eventA)
	}
	assertNoEventBeforeDeadline(t, subscription, ctx, "duplicate notification after committed insert")
}

func assertNoEventBeforeDeadline(t *testing.T, subscription *events.Subscription,
	ctx context.Context, operation string) {
	t.Helper()
	waitCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	_, err := subscription.Next(waitCtx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%s returned error %v, want context deadline without an event", operation, err)
	}
}
