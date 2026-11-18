package state

import (
	"context"
	"errors"
	"sync"
	"time"
)

// This file owns the two protections wrapped around every display read view
// fallback (CacheSynchronizer.GetOrLoad):
//
//   - cacheBreaker pauses Redis reads while the backend keeps timing out, so a
//     dead Redis costs one fast error per request instead of a socket timeout.
//   - fallbackGuard first merges concurrent fallbacks for the same kind+scope
//     into one MySQL query, then bounds the number of queries actually running
//     against MySQL per service instance. A request rejected by the guard gets
//     *ViewBackpressureError and must not be retried elsewhere.

// cacheBreaker is the shared display-cache health state of one service
// instance. Every knowledge base's display request reaches the same Redis
// backend, so they share one breaker.
type cacheBreaker struct {
	threshold int
	cooldown  time.Duration
	// clock is the test seam for the cooldown; nil uses time.Now.
	clock func() time.Time

	mu       sync.Mutex
	state    breakerState
	failures int
	openedAt time.Time
	probing  bool
}

type breakerState uint8

const (
	breakerClosed breakerState = iota
	breakerOpen
	breakerHalfOpen
)

func newCacheBreaker(threshold int, cooldown time.Duration, clock func() time.Time) *cacheBreaker {
	return &cacheBreaker{
		threshold: threshold,
		cooldown:  cooldown,
		clock:     clock,
		state:     breakerClosed,
	}
}

func (b *cacheBreaker) now() time.Time {
	if b.clock != nil {
		return b.clock()
	}
	return time.Now()
}

// allowRead reports whether the caller should consult the cache backend. The
// second return value is true when this call is the single half-open probe: it
// is granted once per cooldown expiry and its outcome decides the state.
func (b *cacheBreaker) allowRead() (read bool, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	switch b.state {
	case breakerClosed:
		return true, false
	case breakerOpen:
		if b.now().Sub(b.openedAt) < b.cooldown {
			return false, false
		}
		// Cooldown elapsed: this request becomes the one probe.
		b.state = breakerHalfOpen
		b.probing = true
		return true, true
	default: // breakerHalfOpen: a probe is already in flight.
		return false, false
	}
}

// recordReadResult folds one backend read outcome back into the breaker. A
// backend that answers at all (hit, miss, or a corrupt single entry) proves it
// is alive; only availability failures (connection errors, timeouts) count
// against it, and a caller cancellation is inconclusive.
func (b *cacheBreaker) recordReadResult(err error, probe bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if probe {
		b.probing = false
		switch {
		case isCacheCanceled(err):
			// Inconclusive: cool down again and let a later request probe.
			b.state = breakerOpen
			b.openedAt = b.now()
		case err == nil || !IsCacheAvailabilityFailure(err):
			// The backend answered (hit, miss, corruption of one entry or an
			// unexpected error type): the probe succeeded.
			b.state = breakerClosed
			b.failures = 0
		default:
			b.state = breakerOpen
			b.openedAt = b.now()
		}
		return
	}
	if b.state != breakerClosed {
		return
	}
	switch {
	case err == nil || !IsCacheAvailabilityFailure(err):
		b.failures = 0
	case isCacheCanceled(err):
		// The caller went away; the backend's health is unknown.
	default:
		b.failures++
		if b.failures >= b.threshold {
			b.state = breakerOpen
			b.openedAt = b.now()
		}
	}
}

// isCacheCanceled reports a caller-canceled backend failure.
func isCacheCanceled(err error) bool {
	var unavailable *CacheUnavailable
	return errors.As(err, &unavailable) && unavailable.Reason == CacheFailureCanceled
}

// state reports the breaker state (diagnostics and tests).
func (b *cacheBreaker) currentState() breakerState {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.state
}

// callKey identifies one merged fallback query: the full cache kind plus scope,
// so different knowledge bases never share a query.
type callKey struct {
	kind  string
	scope string
}

// inflightViewCall is one running (or already finished) fallback query and its
// shared result.
type inflightViewCall struct {
	done  chan struct{}
	value any
	err   error
}

// fallbackGuard merges concurrent fallbacks per callKey and bounds the number of
// queries simultaneously running against MySQL. It lives on the cache
// synchronizer, so the budget applies per service instance.
type fallbackGuard struct {
	mu     sync.Mutex
	limit  int
	inUse  int
	calls  map[callKey]*inflightViewCall
}

func newFallbackGuard(limit int) *fallbackGuard {
	guard := &fallbackGuard{limit: limit}
	guard.calls = map[callKey]*inflightViewCall{}
	return guard
}

// inUseCount reports the number of running queries (diagnostics and tests).
func (g *fallbackGuard) inUseCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.inUse
}

// execute runs one fallback query under the guard.
//
// Concurrent callers for the same kind+scope join the in-flight call instead of
// starting their own query and share its exact result; they do not consume a
// concurrency slot. A caller whose kind+scope has no in-flight call first tries
// to take one of the limit slots: when none is free it receives
// *ViewBackpressureError immediately (no queueing). The leader runs the query
// with a cancellation-detached context so a participant leaving — the leader
// included — never aborts the result the remaining waiters still need; waiters
// may return early on their own cancellation.
//
// The final return value reports whether this caller's run function executed
// (the leader): only the leader observed the loaded revision, so only it may
// backfill the backend afterwards.
func (g *fallbackGuard) execute(ctx context.Context, key callKey, run func(context.Context) (any, error)) (any, error, bool) {
	g.mu.Lock()
	if existing, ok := g.calls[key]; ok {
		g.mu.Unlock()
		select {
		case <-existing.done:
			return existing.value, existing.err, false
		case <-ctx.Done():
			return nil, ctx.Err(), false
		}
	}
	if g.inUse >= g.limit {
		g.mu.Unlock()
		return nil, NewViewBackpressureError(), false
	}
	call := &inflightViewCall{done: make(chan struct{})}
	g.calls[key] = call
	g.inUse++
	g.mu.Unlock()

	go func() {
		// The shared query outlives every participant: detaching cancellation
		// keeps one waiter's (or the leader's) disconnect from aborting the
		// result the others still use.
		value, err := run(context.WithoutCancel(ctx))
		g.mu.Lock()
		call.value, call.err = value, err
		delete(g.calls, key)
		g.inUse--
		g.mu.Unlock()
		close(call.done)
	}()

	select {
	case <-call.done:
		return call.value, call.err, true
	case <-ctx.Done():
		return nil, ctx.Err(), false
	}
}
