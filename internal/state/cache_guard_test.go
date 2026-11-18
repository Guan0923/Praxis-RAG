package state

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Directed tests for the display-cache circuit breaker and the fallback guard
// behind CacheSynchronizer.GetOrLoad: a controllable clock, no MySQL.

// manualClock is the breaker cooldown test seam.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func newManualClock() *manualClock {
	return &manualClock{now: time.Date(2026, 11, 18, 12, 0, 0, 0, time.UTC)}
}

// availabilityFailure is one backend outage sample (connection error class).
func availabilityFailure() error {
	return NewCacheUnavailableNetwork("State cache read unavailable")
}

// corruptFailure is one cached payload that cannot be decoded.
func corruptFailure() error {
	return NewCacheUnavailableCorrupt("State cache read unavailable")
}

// cancelFailure is one read aborted because the caller canceled the request.
func cancelFailure() error {
	return NewCacheUnavailableCanceled("State cache read unavailable")
}

func TestCacheBreakerTripsAfterThresholdAndSkipsReads(t *testing.T) {
	clock := newManualClock()
	breaker := newCacheBreaker(3, 10*time.Second, clock.Now)

	for i := 0; i < 2; i++ {
		if read, probe := breaker.allowRead(); !read || probe {
			t.Fatalf("read %d = (%v, %v), want (true, false)", i+1, read, probe)
		}
		breaker.recordReadResult(availabilityFailure(), false)
	}
	if read, _ := breaker.allowRead(); !read {
		t.Fatalf("breaker tripped before the threshold")
	}
	breaker.recordReadResult(availabilityFailure(), false)

	// Tripped: new requests must not touch the backend anymore.
	if read, probe := breaker.allowRead(); read || probe {
		t.Fatalf("open breaker allowRead() = (%v, %v), want (false, false)", read, probe)
	}
	if state := breaker.currentState(); state != breakerOpen {
		t.Fatalf("state = %v, want open", state)
	}

	// Still open before the cooldown elapses.
	clock.advance(9 * time.Second)
	if read, _ := breaker.allowRead(); read {
		t.Fatalf("breaker opened early")
	}

	// Cooldown elapsed: exactly one probe is granted.
	clock.advance(2 * time.Second)
	probes := 0
	for i := 0; i < 5; i++ {
		if read, probe := breaker.allowRead(); read && probe {
			probes++
		} else if read {
			t.Fatalf("non-probe read while half-open")
		}
	}
	if probes != 1 {
		t.Fatalf("cooldown granted %d probes, want 1", probes)
	}

	// A failing probe re-opens the breaker for another cooldown.
	breaker.recordReadResult(availabilityFailure(), true)
	if state := breaker.currentState(); state != breakerOpen {
		t.Fatalf("state after failed probe = %v, want open", state)
	}
	if read, _ := breaker.allowRead(); read {
		t.Fatalf("failed probe still grants reads")
	}

	// The next cooldown's probe succeeds (Redis answering "key missing" is a
	// successful probe) and closes the circuit.
	clock.advance(10 * time.Second)
	if read, probe := breaker.allowRead(); !read || !probe {
		t.Fatalf("second cooldown allowRead() = (%v, %v), want (true, true)", read, probe)
	}
	breaker.recordReadResult(nil, true)
	if state := breaker.currentState(); state != breakerClosed {
		t.Fatalf("state after successful probe = %v, want closed", state)
	}
	if read, probe := breaker.allowRead(); !read || probe {
		t.Fatalf("closed breaker allowRead() = (%v, %v), want (true, false)", read, probe)
	}
}

func TestCacheBreakerIgnoresCorruptionAndCancellation(t *testing.T) {
	clock := newManualClock()
	breaker := newCacheBreaker(2, 10*time.Second, clock.Now)

	// Corrupt single entries and caller cancellations never count.
	for i := 0; i < 10; i++ {
		if read, _ := breaker.allowRead(); !read {
			t.Fatalf("corruption tripped the breaker")
		}
		breaker.recordReadResult(corruptFailure(), false)
		breaker.recordReadResult(cancelFailure(), false)
	}
	if state := breaker.currentState(); state != breakerClosed {
		t.Fatalf("state = %v, want closed", state)
	}

	// A success resets the availability-failure streak.
	breaker.recordReadResult(availabilityFailure(), false)
	breaker.recordReadResult(nil, false)
	breaker.recordReadResult(availabilityFailure(), false)
	if state := breaker.currentState(); state != breakerClosed {
		t.Fatalf("a success did not reset the failure streak")
	}

	// The legacy unclassified CacheUnavailable still counts as availability.
	breaker.recordReadResult(newCacheUnavailable("legacy"), false)
	breaker.recordReadResult(newCacheUnavailable("legacy"), false)
	if state := breaker.currentState(); state != breakerOpen {
		t.Fatalf("legacy failures did not trip the breaker")
	}
}

func TestCacheBreakerCanceledProbeCoolsDownAgain(t *testing.T) {
	clock := newManualClock()
	breaker := newCacheBreaker(1, 5*time.Second, clock.Now)

	breaker.recordReadResult(availabilityFailure(), false)
	clock.advance(5 * time.Second)
	if _, probe := breaker.allowRead(); !probe {
		t.Fatalf("cooldown did not grant a probe")
	}
	// The caller canceled before the probe finished: inconclusive, so cool
	// down again rather than closing on missing evidence.
	breaker.recordReadResult(cancelFailure(), true)
	if state := breaker.currentState(); state != breakerOpen {
		t.Fatalf("state after canceled probe = %v, want open", state)
	}
	clock.advance(5 * time.Second)
	if _, probe := breaker.allowRead(); !probe {
		t.Fatalf("canceled probe did not schedule the next one")
	}
	// A probe that finds a corrupt entry proves the backend answers.
	breaker.recordReadResult(corruptFailure(), true)
	if state := breaker.currentState(); state != breakerClosed {
		t.Fatalf("state after corrupt probe = %v, want closed", state)
	}
}

// runTracker counts guard executions and observes the number of simultaneously
// running queries, which must never exceed the budget.
type runTracker struct {
	running int64
	peak    int64
	calls   int64
}

func (r *runTracker) enter() {
	current := atomic.AddInt64(&r.running, 1)
	for {
		peak := atomic.LoadInt64(&r.peak)
		if current <= peak || atomic.CompareAndSwapInt64(&r.peak, peak, current) {
			break
		}
	}
	atomic.AddInt64(&r.calls, 1)
}

func (r *runTracker) exit() { atomic.AddInt64(&r.running, -1) }

func TestFallbackGuardMergesConcurrentSameKeyQueries(t *testing.T) {
	guard := newFallbackGuard(2)
	tracker := &runTracker{}
	block := make(chan struct{})

	key := callKey{kind: "documents", scope: "alpha"}
	const callers = 8
	results := make(chan string, callers)
	for i := 0; i < callers; i++ {
		go func() {
			value, err, _ := guard.execute(context.Background(), key, func(context.Context) (any, error) {
				tracker.enter()
				defer tracker.exit()
				<-block // keep the first query executing
				return "snapshot", nil
			})
			if err != nil {
				results <- "err:" + err.Error()
				return
			}
			if text, ok := value.(string); ok {
				results <- "ok:" + text
				return
			}
			results <- "unexpected-value"
		}()
	}
	// Wait for the merged query to start, then let it finish.
	deadline := time.After(5 * time.Second)
	for {
		if atomic.LoadInt64(&tracker.calls) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("merged query never started")
		default:
			time.Sleep(time.Millisecond)
		}
	}
	close(block)
	for i := 0; i < callers; i++ {
		if got := <-results; got != "ok:snapshot" {
			t.Fatalf("caller result = %q", got)
		}
	}
	if calls := atomic.LoadInt64(&tracker.calls); calls != 1 {
		t.Fatalf("loader ran %d times, want 1", calls)
	}
	if inUse := guard.inUseCount(); inUse != 0 {
		t.Fatalf("inUse after completion = %d, want 0", inUse)
	}
}

func TestFallbackGuardRejectsThirdQueryWhenBudgetFull(t *testing.T) {
	guard := newFallbackGuard(2)
	tracker := &runTracker{}
	releaseFirst := make(chan struct{})
	releaseSecond := make(chan struct{})
	startedFirst := make(chan struct{})
	startedSecond := make(chan struct{})

	run := func(release chan struct{}, started chan struct{}) func(context.Context) (any, error) {
		return func(context.Context) (any, error) {
			tracker.enter()
			defer tracker.exit()
			close(started)
			<-release
			return "value", nil
		}
	}

	first := make(chan struct{})
	go func() {
		value, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "kb-1"},
			run(releaseFirst, startedFirst))
		if err != nil || value != "value" {
			t.Errorf("first query = (%v, %v)", value, err)
		}
		close(first)
	}()
	<-startedFirst
	second := make(chan struct{})
	go func() {
		value, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "kb-2"},
			run(releaseSecond, startedSecond))
		if err != nil || value != "value" {
			t.Errorf("second query = (%v, %v)", value, err)
		}
		close(second)
	}()
	<-startedSecond

	// Both slots are busy: a third, different knowledge base must be rejected
	// immediately instead of queueing.
	rejected := make(chan error, 1)
	go func() {
		_, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "kb-3"},
			func(context.Context) (any, error) { return "never", nil })
		rejected <- err
	}()
	select {
	case err := <-rejected:
		if !IsViewBackpressure(err) {
			t.Fatalf("third query error = %v, want ViewBackpressureError", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("third query queued instead of being rejected")
	}

	// Same-key callers still join the in-flight call and never consume a slot.
	joined := make(chan string, 1)
	go func() {
		value, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "kb-1"},
			func(context.Context) (any, error) { return "never", nil })
		if err != nil || value != "value" {
			joined <- "unexpected join result"
			return
		}
		joined <- "joined"
	}()
	// Give the joiner time to register on the in-flight call before releasing
	// it; if it missed the join it would run its own query (calls would grow).
	time.Sleep(20 * time.Millisecond)

	close(releaseFirst)
	<-first
	select {
	case got := <-joined:
		if got != "joined" {
			t.Fatalf("join result = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("same-key join never returned")
	}
	if calls := atomic.LoadInt64(&tracker.calls); calls != 2 {
		t.Fatalf("joiner started its own query: %d runs, want 2", calls)
	}
	close(releaseSecond)
	<-second

	if peak := atomic.LoadInt64(&tracker.peak); peak != 2 {
		t.Fatalf("observed query concurrency = %d, want <= 2", peak)
	}

	// Freed slots serve a fresh key again.
	third := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		value, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "kb-3"},
			func(context.Context) (any, error) { return "third", nil })
		third <- struct {
			value any
			err   error
		}{value, err}
	}()
	select {
	case result := <-third:
		if result.err != nil || result.value != "third" {
			t.Fatalf("third query after release = (%v, %v)", result.value, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("freed slot never served the next query")
	}
}

func TestFallbackGuardWaiterCancelKeepsSharedResult(t *testing.T) {
	guard := newFallbackGuard(1)
	tracker := &runTracker{}

	waiterDone := make(chan error, 1)
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leader := make(chan struct{})
	// The leader starts the query, then disconnects while it is running.
	go func() {
		_, err, _ := guard.execute(leaderCtx, callKey{kind: "documents", scope: "alpha"},
			func(context.Context) (any, error) {
				tracker.enter()
				defer tracker.exit()
				<-time.After(30 * time.Millisecond)
				return "shared", nil
			})
		leader <- struct{}{}
		_ = err
		close(leader)
	}()
	// A waiter joins the same in-flight query and must receive its result even
	// after the leader (whose context started the query) canceled.
	waitLeaderStart := make(chan struct{})
	go func() {
		<-waitLeaderStart
		value, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "alpha"},
			func(context.Context) (any, error) { return "never", nil })
		waiterDone <- func() error {
			if err != nil {
				return err
			}
			if value != "shared" {
				return errors.New("waiter received " + anyToString(value))
			}
			return nil
		}()
	}()
	// Give the leader a head start so it owns the call, then cancel it while
	// the query is still executing.
	time.Sleep(10 * time.Millisecond)
	close(waitLeaderStart)
	cancelLeader()
	<-leader
	if err := <-waiterDone; err != nil {
		t.Fatalf("waiter after leader cancel: %v", err)
	}
	if calls := atomic.LoadInt64(&tracker.calls); calls != 1 {
		t.Fatalf("loader ran %d times, want 1", calls)
	}
}

// anyToString renders a value for failure messages.
func anyToString(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	return "unexpected value"
}

func TestFallbackGuardWaiterMayExitOnOwnCancel(t *testing.T) {
	guard := newFallbackGuard(1)
	release := make(chan struct{})
	leader := make(chan struct{})
	go func() {
		value, err, _ := guard.execute(context.Background(), callKey{kind: "documents", scope: "alpha"},
			func(context.Context) (any, error) {
				<-release
				return "shared", nil
			})
		if err != nil || value != "shared" {
			t.Errorf("leader = (%v, %v)", value, err)
		}
		close(leader)
	}()
	time.Sleep(10 * time.Millisecond)

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	waiterExited := make(chan error, 1)
	go func() {
		_, err, _ := guard.execute(waiterCtx, callKey{kind: "documents", scope: "alpha"},
			func(context.Context) (any, error) { return "never", nil })
		waiterExited <- err
	}()
	time.Sleep(10 * time.Millisecond)
	cancelWaiter()
	select {
	case err := <-waiterExited:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter exit error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("waiter could not exit on its own cancellation")
	}

	// The shared query still completes for everyone else.
	close(release)
	<-leader
}
