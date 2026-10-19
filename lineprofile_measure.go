package interbase

import (
	"context"
	"errors"
	"sort"
	"time"
)

// lineProfileBaselineSamples is the number of round-trip probes taken before
// and after a run (spec 7).
const lineProfileBaselineSamples = 15

// baselineOf returns the minimum and median of the samples.
func baselineOf(samples []time.Duration) RoundTripBaseline {
	if len(samples) == 0 {
		return RoundTripBaseline{}
	}
	sorted := append([]time.Duration(nil), samples...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	middle := len(sorted) / 2
	median := sorted[middle]
	if len(sorted)%2 == 0 {
		median = (sorted[middle-1] + sorted[middle]) / 2
	}
	return RoundTripBaseline{Min: sorted[0], Median: median, Samples: len(sorted)}
}

// classifyServerCall reports whether a call of duration d was a network
// round trip (at least half the baseline minimum) and its adjusted time:
// d minus the baseline median, clamped at zero, for round trips; d otherwise.
func classifyServerCall(before RoundTripBaseline, d time.Duration) (bool, time.Duration) {
	if d < before.Min/2 {
		return false, d
	}
	adjusted := d - before.Median
	if adjusted < 0 {
		adjusted = 0
	}
	return true, adjusted
}

// measureBaseline times lineProfileBaselineSamples round-trip probes on c.
func measureBaseline(ctx context.Context, c *conn) (RoundTripBaseline, error) {
	samples := make([]time.Duration, 0, lineProfileBaselineSamples)
	for range lineProfileBaselineSamples {
		c.lockDirect()
		d, err := c.probeRoundTrip(ctx)
		c.mu.Unlock()
		if err != nil {
			return baselineOf(samples), err
		}
		samples = append(samples, d)
	}
	return baselineOf(samples), nil
}

// serverCall times op as one server call of node, then takes the activity
// snapshot that attributes the call's counters to the node. Only op is timed
// into the node; the snapshot time is overhead.
func (r *lpRun) serverCall(node *LineProfileNode, op func() error) error {
	start := time.Now()
	err := op()
	d := time.Since(start)
	roundTrip, adjusted := classifyServerCall(r.result.Before, d)
	m := &node.Metrics
	m.ServerCalls++
	m.Raw += d
	m.Adjusted += adjusted
	if m.Hits == 1 {
		m.FirstRaw += d
		m.FirstAdjusted += adjusted
	}
	r.result.ServerCalls++
	if roundTrip {
		m.RoundTrips++
		r.result.RoundTrips++
	}
	if err != nil && r.ctx.Err() == nil {
		m.Errors++
	}
	if snapErr := r.snapshot(m); snapErr != nil && err == nil {
		err = fatal(snapErr)
	}
	return err
}

// snapshot reads the attachment counters in one native info call and adds
// the delta since the previous snapshot to m, or to OverheadActivity when m
// is nil. It runs even after the run's
// context is canceled so the canceled call's counters are still attributed.
// When the server does not supply the counters (an error item, absent page
// items or a counter that went backwards) the counters become unavailable
// for the run and the run continues (R16); any other failure ends the run.
func (r *lpRun) snapshot(m *LineMetrics) error {
	if r.countersUnavailable {
		return nil
	}
	start := time.Now()
	r.conn.lockDirect()
	activity, complete, err := r.conn.readActivitySnapshotLocked(context.WithoutCancel(r.ctx))
	r.conn.mu.Unlock()
	r.result.Snapshots += time.Since(start)
	if errors.Is(err, errInfoItemUnsupported) || err == nil && !complete {
		r.countersUnavailable = true
		return nil
	}
	if err != nil {
		return err
	}
	previous, had := r.activity, r.haveActivity
	r.activity, r.haveActivity = activity, true
	if !had {
		return nil
	}
	delta, err := activity.Delta(previous)
	if errors.Is(err, ErrActivityCounterReset) {
		r.countersUnavailable = true
		return nil
	}
	if err != nil {
		return err
	}
	if m == nil {
		r.result.OverheadActivity = r.result.OverheadActivity.Add(delta)
		return nil
	}
	addActivity(m, delta)
	return nil
}

// clearCounters drops every node's counters once they became unavailable,
// so partial counters are never shown as complete ones.
func clearCounters(routines []LineProfileRoutine) {
	for i := range routines {
		for j := range routines[i].Nodes {
			routines[i].Nodes[j].Metrics.Pages = nil
			routines[i].Nodes[j].Metrics.Tables = nil
		}
	}
}

func addActivity(m *LineMetrics, delta ActivityDelta) {
	if delta.Pages != nil {
		pages := addPageCounters(pagesOf(m.Pages), *delta.Pages)
		m.Pages = &pages
	}
	for id, counters := range delta.Tables {
		if m.Tables == nil {
			m.Tables = make(map[int]TableCounters)
		}
		m.Tables[id] = addTableCounters(m.Tables[id], counters)
	}
}

func pagesOf(p *PageCounters) PageCounters {
	if p == nil {
		return PageCounters{}
	}
	return *p
}

// addMetrics adds b to a without aliasing b's map or pointer.
func addMetrics(a *LineMetrics, b LineMetrics) {
	a.Hits += b.Hits
	a.ServerCalls += b.ServerCalls
	a.RoundTrips += b.RoundTrips
	a.Rows += b.Rows
	a.Errors += b.Errors
	a.Raw += b.Raw
	a.Adjusted += b.Adjusted
	a.FirstRaw += b.FirstRaw
	a.FirstAdjusted += b.FirstAdjusted
	if b.Pages != nil {
		pages := addPageCounters(pagesOf(a.Pages), *b.Pages)
		a.Pages = &pages
	}
	for id, counters := range b.Tables {
		if a.Tables == nil {
			a.Tables = make(map[int]TableCounters)
		}
		a.Tables[id] = addTableCounters(a.Tables[id], counters)
	}
}

// aggregateRoutines fills every routine's Lines and totals (spec 7):
// exclusive is the routine's own nodes; inclusive adds the inclusive
// metrics of its stepped-into callees. A callee's entry always follows its
// parent's, so one backward pass sums the tree.
func aggregateRoutines(routines []LineProfileRoutine) {
	for i := range routines {
		aggregateRoutine(&routines[i])
	}
	for i := len(routines) - 1; i > 0; i-- {
		if parent := routines[i].Parent; parent >= 0 && parent < i {
			addMetrics(&routines[parent].Inclusive, routines[i].Inclusive)
		}
	}
}

// aggregateRoutine fills Lines (nodes summed by start line, executed lines
// only) and the frame totals, the inclusive ones without callees.
func aggregateRoutine(routine *LineProfileRoutine) {
	routine.Lines = make(map[int]LineMetrics)
	routine.Exclusive = LineMetrics{}
	for _, node := range routine.Nodes {
		if node.Metrics.Hits == 0 {
			continue
		}
		line := routine.Lines[node.Line]
		addMetrics(&line, node.Metrics)
		routine.Lines[node.Line] = line
		addMetrics(&routine.Exclusive, node.Metrics)
	}
	// First hits are per node and line; a routine total has none.
	routine.Exclusive.FirstRaw, routine.Exclusive.FirstAdjusted = 0, 0
	routine.Inclusive = LineMetrics{}
	addMetrics(&routine.Inclusive, routine.Exclusive)
}
