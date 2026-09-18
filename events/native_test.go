package events

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestNativeCallbackErrorIsObservableAndOwned(t *testing.T) {
	subscription := newTestNativeSubscription(t, "event_a")
	defer destroyTestNativeSubscription(t, subscription)

	if err := subscription.emit(nil, 0); err != nil {
		t.Fatalf("test callback emit failed: %v", err)
	}
	ready, err := subscription.wait(time.Second)
	if err != nil {
		t.Fatalf("wait for callback error failed: %v", err)
	}
	if !ready {
		t.Fatal("callback error did not signal the notification pipe")
	}
	if _, _, err := subscription.take(); err == nil {
		t.Fatal("take returned nil error after invalid callback")
	} else if !strings.Contains(err.Error(), "invalid result buffer") {
		t.Fatalf("callback error = %v, want invalid callback error", err)
	}
}

func TestNativeEventBlocksAccumulateAcrossChunks(t *testing.T) {
	names := make([]string, 16)
	for index := range names {
		names[index] = fmt.Sprintf("event_%02d", index)
	}
	subscription := newTestNativeSubscription(t, names...)
	defer destroyTestNativeSubscription(t, subscription)

	if err := subscription.emitCounts(0, make([]uint64, 15)); err != nil {
		t.Fatalf("emit first block baseline: %v", err)
	}
	if err := subscription.emitCounts(1, []uint64{0}); err != nil {
		t.Fatalf("emit second block baseline: %v", err)
	}
	firstBlock := make([]uint64, 15)
	firstBlock[0] = 3
	if err := subscription.emitCounts(0, firstBlock); err != nil {
		t.Fatalf("emit first block delta: %v", err)
	}
	if err := subscription.emitCounts(1, []uint64{5}); err != nil {
		t.Fatalf("emit second block delta: %v", err)
	}
	ready, err := subscription.wait(time.Second)
	if err != nil {
		t.Fatalf("wait for chunked event notification: %v", err)
	}
	if !ready {
		t.Fatal("chunked event notification did not signal the pipe")
	}
	counts, hasCounts, err := subscription.takeCounts(len(names))
	if err != nil {
		t.Fatalf("take chunked event counts: %v", err)
	}
	if !hasCounts || counts[0] != 3 || counts[15] != 5 {
		t.Fatalf("chunked event counts = %#v, hasCounts=%v; want counts[0]=3 and counts[15]=5",
			counts, hasCounts)
	}
}

func TestNativeStopDrainsOverlappingCallbackBeforeDestroy(t *testing.T) {
	subscription := newTestNativeSubscription(t, "event_a")
	if err := subscription.holdCallback(); err != nil {
		destroyTestNativeSubscription(t, subscription)
		t.Fatal(err)
	}

	emitDone := make(chan error, 1)
	go func() { emitDone <- subscription.emit(nil, 0) }()
	if entered, err := subscription.waitCallback(time.Second); err != nil {
		destroyTestNativeSubscription(t, subscription)
		t.Fatalf("wait for callback entry failed: %v", err)
	} else if !entered {
		destroyTestNativeSubscription(t, subscription)
		t.Fatal("callback did not enter the deterministic hold")
	}

	stopDone := make(chan error, 1)
	go func() { stopDone <- subscription.stop() }()
	select {
	case err := <-stopDone:
		if err != nil {
			destroyTestNativeSubscription(t, subscription)
			t.Fatalf("stop overlapping callback = %v", err)
		}
	case <-time.After(time.Second):
		destroyTestNativeSubscription(t, subscription)
		t.Fatal("stop did not drain overlapping callback")
	}
	if err := <-emitDone; err != nil {
		destroyTestNativeSubscription(t, subscription)
		t.Fatalf("callback emit = %v", err)
	}
	if err := subscription.destroy(); err != nil {
		t.Fatalf("destroy after drained callback = %v", err)
	}
}
