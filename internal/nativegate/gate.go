// Package nativegate coordinates calls into the InterBase client library.
package nativegate

import (
	"context"
	"errors"
	"sync"
)

// Global is the process-wide gate shared by every Go package that calls the
// InterBase client library. The vendor library is a single process resource,
// even when an application owns attachments through different APIs.
var Global Gate

// Gate permits concurrent native calls, but gives a lifecycle call exclusive
// access once the calls already in progress have drained. A lifecycle waiter
// deliberately does not prevent new calls from entering while it waits: a
// native call may be waiting for another call to release a database lock.
// Blocking those new calls would turn lifecycle cleanup into a database-lock
// deadlock.
//
// The zero value is ready for use. A Gate must not be copied after first use.
type Gate struct {
	mu               sync.Mutex
	condition        *sync.Cond
	notify           chan struct{}
	activeCalls      uint64
	ordinaryWaiters  uint64
	exclusive        bool
	exclusiveWaiters uint64
}

// State is a point-in-time observation of a Gate. It is useful for diagnostics
// and for tests that must wait on admission rather than guess from timing.
type State struct {
	ActiveCalls      uint64
	Exclusive        bool
	ExclusiveWaiters uint64
}

// State returns the current admission counts without changing gate behavior.
func (g *Gate) State() State {
	g.mu.Lock()
	defer g.mu.Unlock()
	return State{
		ActiveCalls:      g.activeCalls,
		Exclusive:        g.exclusive,
		ExclusiveWaiters: g.exclusiveWaiters,
	}
}

func (g *Gate) conditionLocked() *sync.Cond {
	if g.condition == nil {
		g.condition = sync.NewCond(&g.mu)
	}
	return g.condition
}

// waitChannelLocked returns a channel that is closed whenever a gate state
// change may allow a waiter to make progress. The channel is replaced under
// the same mutex after it is closed, so a context-aware waiter can select on
// both gate progress and cancellation without a helper goroutine per waiter.
func (g *Gate) waitChannelLocked() <-chan struct{} {
	if g.notify == nil {
		g.notify = make(chan struct{})
	}
	return g.notify
}

func (g *Gate) broadcastLocked() {
	if g.condition != nil {
		g.condition.Broadcast()
	}
	if g.notify != nil {
		close(g.notify)
	}
	g.notify = make(chan struct{})
}

func contextError(ctx context.Context) error {
	if ctx == nil {
		return errors.New("nativegate: nil context")
	}
	return ctx.Err()
}

// Enter admits one ordinary native call and returns its release function.
// Ordinary calls may enter while an exclusive caller is waiting for existing
// calls to finish, but not while an exclusive caller is active.
func (g *Gate) Enter() func() {
	g.mu.Lock()
	condition := g.conditionLocked()
	for g.exclusive {
		condition.Wait()
	}
	g.activeCalls++
	g.mu.Unlock()

	return sync.OnceFunc(func() {
		g.mu.Lock()
		g.activeCalls--
		if g.activeCalls == 0 {
			g.broadcastLocked()
		}
		g.mu.Unlock()
	})
}

// EnterContext admits one ordinary native call, or returns ctx.Err() if the
// call cannot be admitted before its context is canceled. Ordinary calls may
// enter while an exclusive caller is waiting for existing calls to finish,
// but not while an exclusive caller is active.
func (g *Gate) EnterContext(ctx context.Context) (func(), error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	g.mu.Lock()
	waiting := false
	for g.exclusive {
		if !waiting {
			g.ordinaryWaiters++
			waiting = true
		}
		wait := g.waitChannelLocked()
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			if waiting {
				g.ordinaryWaiters--
			}
			g.broadcastLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-wait:
			g.mu.Lock()
		}
	}
	if waiting {
		g.ordinaryWaiters--
	}
	if err := ctx.Err(); err != nil {
		g.broadcastLocked()
		g.mu.Unlock()
		return nil, err
	}
	g.activeCalls++
	g.mu.Unlock()

	return sync.OnceFunc(func() {
		g.mu.Lock()
		g.activeCalls--
		if g.activeCalls == 0 {
			g.broadcastLocked()
		}
		g.mu.Unlock()
	}), nil
}

// EnterExclusive admits one lifecycle call after all ordinary calls already
// in progress have finished. New ordinary calls remain admissible while this
// method waits; once it returns, they wait until the release function runs.
func (g *Gate) EnterExclusive() func() {
	g.mu.Lock()
	condition := g.conditionLocked()
	g.exclusiveWaiters++
	for g.activeCalls != 0 || g.exclusive {
		condition.Wait()
	}
	g.exclusiveWaiters--
	g.exclusive = true
	g.mu.Unlock()

	return sync.OnceFunc(func() {
		g.mu.Lock()
		g.exclusive = false
		g.broadcastLocked()
		g.mu.Unlock()
	})
}

// EnterExclusiveContext admits one lifecycle call after all ordinary calls
// already in progress have finished, or returns ctx.Err() while it waits.
// New ordinary calls remain admissible while this method waits; once it
// returns, they wait until the release function runs.
func (g *Gate) EnterExclusiveContext(ctx context.Context) (func(), error) {
	if err := contextError(ctx); err != nil {
		return nil, err
	}

	g.mu.Lock()
	g.exclusiveWaiters++
	for g.activeCalls != 0 || g.exclusive {
		wait := g.waitChannelLocked()
		g.mu.Unlock()
		select {
		case <-ctx.Done():
			g.mu.Lock()
			g.exclusiveWaiters--
			g.broadcastLocked()
			g.mu.Unlock()
			return nil, ctx.Err()
		case <-wait:
			g.mu.Lock()
		}
	}
	g.exclusiveWaiters--
	if err := ctx.Err(); err != nil {
		g.broadcastLocked()
		g.mu.Unlock()
		return nil, err
	}
	g.exclusive = true
	g.mu.Unlock()

	return sync.OnceFunc(func() {
		g.mu.Lock()
		g.exclusive = false
		g.broadcastLocked()
		g.mu.Unlock()
	}), nil
}
