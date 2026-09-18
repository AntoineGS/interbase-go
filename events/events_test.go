package events

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"testing"
	"time"

	interbase "interbase-go"
)

func TestPublicSubscriptionSurface(t *testing.T) {
	var _ func(context.Context, Config, ...string) (*Subscription, error) = Subscribe
	var _ interface {
		Next(context.Context) (map[string]uint64, error)
		Close() error
	} = (*Subscription)(nil)

	_ = interbase.Config{
		Database: "example.ib",
		Host:     "localhost",
		User:     "SYSDBA",
	}
}

func TestSubscribeRejectsNilContextBeforeNativeAttach(t *testing.T) {
	_, err := Subscribe(nil, Config{}, "event_a")
	if err == nil {
		t.Fatal("Subscribe(nil, ...) returned nil error")
	}
}

func TestSubscribeRejectsCanceledContextBeforeNativeAttach(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := Subscribe(ctx, Config{}, "event_a")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Subscribe(canceled context) error = %v, want context.Canceled", err)
	}
}

func TestSubscribeValidatesEventNamesBeforeNativeAttach(t *testing.T) {
	cases := []struct {
		name       string
		eventNames []string
		want       string
	}{
		{name: "none", want: "at least one event name"},
		{name: "empty", eventNames: []string{""}, want: "event name 0 is empty"},
		{name: "duplicate", eventNames: []string{"event_a", "event_a"}, want: "duplicate event name"},
		{name: "invalid utf8", eventNames: []string{string([]byte{0xff})}, want: "not valid UTF-8"},
		{name: "nul", eventNames: []string{"event\x00a"}, want: "contains a NUL byte"},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, err := Subscribe(context.Background(), Config{}, test.eventNames...)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Subscribe() error = %v, want substring %q", err, test.want)
			}
		})
	}
}

func TestValidateEventNamesEnforcesNativeNameLimit(t *testing.T) {
	for _, test := range []struct {
		name       string
		length     int
		wantReject bool
	}{
		{name: "maximum accepted", length: 127},
		{name: "first rejected", length: 128, wantReject: true},
		{name: "legacy client boundary rejected", length: 255, wantReject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateEventNames([]string{strings.Repeat("x", test.length)})
			if test.wantReject {
				if err == nil || !strings.Contains(err.Error(), "too long") {
					t.Fatalf("validateEventNames(%d bytes) error = %v, want too-long error", test.length, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateEventNames(%d bytes) error = %v, want nil", test.length, err)
			}
		})
	}
}

func TestValidateEventNamesAllowsMultipleNativeBlocks(t *testing.T) {
	names := make([]string, 128)
	for index := range names {
		names[index] = fmt.Sprintf("event_%03d_%s", index, strings.Repeat("x", 117))
	}
	if err := validateEventNames(names); err != nil {
		t.Fatalf("validateEventNames() rejected valid multi-block names: %v", err)
	}
}

func TestNextReturnsAccumulatedCountsForAllNames(t *testing.T) {
	backend := &scriptedBackend{
		waits: []waitResult{{ready: true}},
		takes: []takeResult{{counts: []uint64{3, 0}, hasCounts: true}},
	}
	subscription := newSubscription(Config{}, []string{"event_a", "event_b"}, backend)

	got, err := subscription.Next(context.Background())
	if err != nil {
		t.Fatalf("Next() error = %v", err)
	}
	want := map[string]uint64{"event_a": 3, "event_b": 0}
	if len(got) != len(want) || got["event_a"] != want["event_a"] || got["event_b"] != want["event_b"] {
		t.Fatalf("Next() = %#v, want %#v", got, want)
	}
}

func TestNextContextCancellationDoesNotCloseSubscription(t *testing.T) {
	backend := &scriptedBackend{
		waits: []waitResult{{ready: true}},
		takes: []takeResult{{counts: []uint64{1}, hasCounts: true}},
	}
	subscription := newSubscription(Config{}, []string{"event_a"}, backend)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := subscription.Next(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Next(cancelled context) error = %v, want context.Canceled", err)
	}
	if backend.waitCalls != 0 {
		t.Fatalf("cancelled Next called backend wait %d times", backend.waitCalls)
	}
	got, err := subscription.Next(context.Background())
	if err != nil {
		t.Fatalf("Next() after cancellation error = %v", err)
	}
	if got["event_a"] != 1 {
		t.Fatalf("Next() after cancellation = %#v, want event_a=1", got)
	}
}

func TestNextCanceledContextDoesNotWaitBehindActiveNext(t *testing.T) {
	backend := &scriptedBackend{
		waitStarted: make(chan struct{}),
		stopDone:    make(chan struct{}),
	}
	subscription := newSubscription(Config{}, []string{"event_a"}, backend)

	activeDone := make(chan error, 1)
	go func() {
		_, err := subscription.Next(context.Background())
		activeDone <- err
	}()
	select {
	case <-backend.waitStarted:
	case <-time.After(time.Second):
		t.Fatal("active Next() did not reach backend wait")
	}

	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledDone := make(chan error, 1)
	go func() {
		_, err := subscription.Next(canceledCtx)
		canceledDone <- err
	}()
	select {
	case err := <-canceledDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled Next() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled Next() waited behind active Next()")
	}

	if err := subscription.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-activeDone; !errors.Is(err, ErrClosed) {
		t.Fatalf("active Next() after Close() error = %v, want ErrClosed", err)
	}
}

func TestWaitForNativeReadyWaitsForAllBaselines(t *testing.T) {
	backend := &scriptedBackend{
		readyResults: []waitResult{{ready: false}, {ready: true}},
	}

	if err := waitForNativeReady(context.Background(), backend); err != nil {
		t.Fatalf("waitForNativeReady() error = %v", err)
	}
	if backend.readyCalls != 2 {
		t.Fatalf("ready calls = %d, want 2", backend.readyCalls)
	}
}

func TestWaitForNativeReadyHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	backend := &scriptedBackend{}

	if err := waitForNativeReady(ctx, backend); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitForNativeReady() error = %v, want context.Canceled", err)
	}
	if backend.readyCalls != 0 {
		t.Fatalf("ready calls = %d for canceled context, want 0", backend.readyCalls)
	}
}

func TestNextClosesAfterBackendError(t *testing.T) {
	backend := &scriptedBackend{
		waits: []waitResult{{ready: true}},
		takes: []takeResult{{err: errors.New("backend failure")}},
	}
	subscription := newSubscription(Config{}, []string{"event_a"}, backend)

	if _, err := subscription.Next(context.Background()); err == nil ||
		!strings.Contains(err.Error(), "backend failure") {
		t.Fatalf("Next() error = %v, want backend failure", err)
	}
	if backend.stopCalls != 1 || backend.destroyCalls != 1 {
		t.Fatalf("backend cleanup calls = stop %d, destroy %d, want 1 each",
			backend.stopCalls, backend.destroyCalls)
	}
	if _, err := subscription.Next(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Next() after backend error = %v, want ErrClosed", err)
	}
}

func TestCloseUnblocksNextAndIsIdempotent(t *testing.T) {
	backend := &scriptedBackend{waitStarted: make(chan struct{}), stopDone: make(chan struct{})}
	subscription := newSubscription(Config{}, []string{"event_a"}, backend)

	nextDone := make(chan error, 1)
	go func() {
		_, err := subscription.Next(context.Background())
		nextDone <- err
	}()
	select {
	case <-backend.waitStarted:
	case <-time.After(time.Second):
		t.Fatal("Next() did not reach backend wait")
	}
	if err := subscription.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := <-nextDone; !errors.Is(err, ErrClosed) {
		t.Fatalf("Next() after concurrent Close error = %v, want ErrClosed", err)
	}
	if err := subscription.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	if backend.stopCalls != 1 || backend.destroyCalls != 1 {
		t.Fatalf("backend cleanup calls = stop %d, destroy %d, want 1 each", backend.stopCalls, backend.destroyCalls)
	}
}

func TestCloseRetainsBackendAfterCleanupFailure(t *testing.T) {
	backend := &scriptedBackend{
		destroyErrors: []error{errors.New("transient detach failure"), nil},
	}
	subscription := newSubscription(Config{}, []string{"event_a"}, backend)

	if err := subscription.Close(); err == nil ||
		!strings.Contains(err.Error(), "transient detach failure") {
		t.Fatalf("first Close() error = %v, want transient detach failure", err)
	}
	if backend.destroyCalls != 1 {
		t.Fatalf("first Close() destroy calls = %d, want 1", backend.destroyCalls)
	}
	if err := subscription.Close(); err != nil {
		t.Fatalf("retry Close() error = %v", err)
	}
	if backend.stopCalls != 2 || backend.destroyCalls != 2 {
		t.Fatalf("cleanup calls after retry = stop %d, destroy %d, want 2 each",
			backend.stopCalls, backend.destroyCalls)
	}
	if err := subscription.Close(); err != nil {
		t.Fatalf("idempotent Close() error = %v", err)
	}
	if backend.stopCalls != 2 || backend.destroyCalls != 2 {
		t.Fatalf("cleanup calls after successful close = stop %d, destroy %d, want 2 each",
			backend.stopCalls, backend.destroyCalls)
	}
}

func TestNativeInitialBaselineIsInstalledBeforeDiscard(t *testing.T) {
	subscription := newTestNativeSubscription(t, "event_a")
	defer destroyTestNativeSubscription(t, subscription)

	if err := subscription.emitCounts(0, []uint64{7}); err != nil {
		t.Fatalf("emit initial nonzero baseline: %v", err)
	}
	counts, hasCounts, err := subscription.take()
	if err != nil {
		t.Fatalf("take initial baseline: %v", err)
	}
	if hasCounts || counts[0] != 0 {
		t.Fatalf("initial baseline counts = %#v, hasCounts=%v; want no counts", counts, hasCounts)
	}

	if err := subscription.emitCounts(0, []uint64{8}); err != nil {
		t.Fatalf("emit event after baseline: %v", err)
	}
	counts, hasCounts, err = subscription.take()
	if err != nil {
		t.Fatalf("take event after baseline: %v", err)
	}
	if !hasCounts || counts[0] != 1 {
		t.Fatalf("event after initial baseline = %#v, hasCounts=%v; want one occurrence", counts, hasCounts)
	}
}

func TestNativeEventCounterWrapsAtNativeWidth(t *testing.T) {
	subscription := newTestNativeSubscription(t, "event_a")
	defer destroyTestNativeSubscription(t, subscription)

	if err := subscription.emitCounts(0, []uint64{math.MaxUint32 - 1}); err != nil {
		t.Fatalf("emit wrapping baseline: %v", err)
	}
	if _, hasCounts, err := subscription.take(); err != nil {
		t.Fatalf("take wrapping baseline: %v", err)
	} else if hasCounts {
		t.Fatal("wrapping baseline was reported as an event")
	}
	if err := subscription.emitCounts(0, []uint64{1}); err != nil {
		t.Fatalf("emit wrapped event: %v", err)
	}
	counts, hasCounts, err := subscription.take()
	if err != nil {
		t.Fatalf("take wrapped event: %v", err)
	}
	if !hasCounts || counts[0] != 3 {
		t.Fatalf("wrapped event counts = %#v, hasCounts=%v; want 3", counts, hasCounts)
	}
}

type waitResult struct {
	ready bool
	err   error
}

type takeResult struct {
	counts    []uint64
	hasCounts bool
	err       error
}

type scriptedBackend struct {
	mu            sync.Mutex
	waits         []waitResult
	takes         []takeResult
	waitCalls     int
	stopCalls     int
	destroyCalls  int
	waitStarted   chan struct{}
	stopDone      chan struct{}
	destroyErrors []error
	readyResults  []waitResult
	readyCalls    int
}

func (b *scriptedBackend) wait(context.Context, time.Duration) (bool, error) {
	b.mu.Lock()
	b.waitCalls++
	if b.waitStarted != nil {
		select {
		case <-b.waitStarted:
		default:
			close(b.waitStarted)
		}
	}
	if len(b.waits) != 0 {
		result := b.waits[0]
		b.waits = b.waits[1:]
		b.mu.Unlock()
		return result.ready, result.err
	}
	stopDone := b.stopDone
	b.mu.Unlock()
	if stopDone != nil {
		<-stopDone
		return true, nil
	}
	return false, nil
}

func (b *scriptedBackend) ready(context.Context, time.Duration) (bool, error) {
	b.mu.Lock()
	b.readyCalls++
	if len(b.readyResults) != 0 {
		result := b.readyResults[0]
		b.readyResults = b.readyResults[1:]
		b.mu.Unlock()
		return result.ready, result.err
	}
	b.mu.Unlock()
	return true, nil
}

func (b *scriptedBackend) take(context.Context, int) ([]uint64, bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.takes) == 0 {
		return nil, false, errors.New("scripted backend has no take result")
	}
	result := b.takes[0]
	b.takes = b.takes[1:]
	return result.counts, result.hasCounts, result.err
}

func (b *scriptedBackend) stop() error {
	b.mu.Lock()
	b.stopCalls++
	if b.stopDone != nil {
		select {
		case <-b.stopDone:
		default:
			close(b.stopDone)
		}
	}
	b.mu.Unlock()
	return nil
}

func (b *scriptedBackend) destroy() error {
	b.mu.Lock()
	b.destroyCalls++
	var err error
	if len(b.destroyErrors) != 0 {
		err = b.destroyErrors[0]
		b.destroyErrors = b.destroyErrors[1:]
	}
	b.mu.Unlock()
	return err
}
