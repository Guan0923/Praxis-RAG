package redisstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// TestRandomizedTTLDistribution pins the randomized payload TTL: T=60 must
// draw integers in [48, 60] and both bounds must be reachable.
func TestRandomizedTTLDistribution(t *testing.T) {
	seenMin, seenMax := int(1<<30), 0
	for range 600 {
		drawn := state.RandomTTLSeconds(60)
		if drawn < 48 || drawn > 60 {
			t.Fatalf("RandomTTLSeconds(60) = %d, want within [48, 60]", drawn)
		}
		if drawn < seenMin {
			seenMin = drawn
		}
		if drawn > seenMax {
			seenMax = drawn
		}
	}
	if seenMin != 48 || seenMax != 60 {
		t.Fatalf("observed range [%d, %d], want both bounds 48 and 60", seenMin, seenMax)
	}
	// Small and odd upper bounds keep the floor formula: [T-floor(T/5), T].
	for _, ttl := range []int{1, 2, 3, 5, 7, 12} {
		floor := ttl - ttl/5
		for range 100 {
			drawn := state.RandomTTLSeconds(ttl)
			if drawn < floor || drawn > ttl {
				t.Fatalf("RandomTTLSeconds(%d) = %d, want within [%d, %d]", ttl, drawn, floor, ttl)
			}
		}
	}
	if drawn := state.RandomTTLSeconds(0); drawn != 0 {
		t.Fatalf("RandomTTLSeconds(0) = %d, want 0", drawn)
	}
}

// TestBackfillLockAcquireRenewRelease covers the lock lifecycle: SET NX PX
// acquisition, token-checked renewal and token-checked release.
func TestBackfillLockAcquireRenewRelease(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()
	if key := backend.BackfillLockKey("documents", "ns-a"); key == backend.PayloadKey("documents", "ns-a") {
		t.Fatalf("lock key must differ from the payload key")
	}

	// The lock is free: the first token wins and the key carries the TTL.
	held, err := backend.AcquireBackfillLock(ctx, "documents", "ns-a", "token-1", 2*time.Second)
	if err != nil || !held {
		t.Fatalf("AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
	if ttl := server.TTL(backend.BackfillLockKey("documents", "ns-a")); ttl <= 0 || ttl > 2*time.Second {
		t.Fatalf("lock TTL = %v, want within (0, 2s]", ttl)
	}

	// A second token cannot take or renew the live lock.
	held, err = backend.AcquireBackfillLock(ctx, "documents", "ns-a", "token-2", 2*time.Second)
	if err != nil || held {
		t.Fatalf("contending AcquireBackfillLock = (%v, %v), want (false, nil)", held, err)
	}
	renewed, err := backend.RenewBackfillLock(ctx, "documents", "ns-a", "token-2", 2*time.Second)
	if err != nil || renewed {
		t.Fatalf("foreign RenewBackfillLock = (%v, %v), want (false, nil)", renewed, err)
	}

	// The owner renews: the TTL is refreshed to the full lease.
	time.Sleep(20 * time.Millisecond)
	renewed, err = backend.RenewBackfillLock(ctx, "documents", "ns-a", "token-1", 2*time.Second)
	if err != nil || !renewed {
		t.Fatalf("owner RenewBackfillLock = (%v, %v), want (true, nil)", renewed, err)
	}
	if ttl := server.TTL(backend.BackfillLockKey("documents", "ns-a")); ttl <= 0 || ttl > 2*time.Second {
		t.Fatalf("renewed lock TTL = %v, want within (0, 2s]", ttl)
	}

	// Different keys (knowledge bases) hold independent locks.
	held, err = backend.AcquireBackfillLock(ctx, "documents", "ns-b", "token-2", 2*time.Second)
	if err != nil || !held {
		t.Fatalf("parallel AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}

	// Release by a foreign token leaves the lock in place.
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns-a", "token-2"); err != nil {
		t.Fatalf("foreign ReleaseBackfillLock error = %v", err)
	}
	held, err = backend.AcquireBackfillLock(ctx, "documents", "ns-a", "token-3", 2*time.Second)
	if err != nil || held {
		t.Fatalf("post-foreign-release AcquireBackfillLock = (%v, %v), want (false, nil)", held, err)
	}

	// The owner's release frees the lock and is idempotent.
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns-a", "token-1"); err != nil {
		t.Fatalf("owner ReleaseBackfillLock error = %v", err)
	}
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns-a", "token-1"); err != nil {
		t.Fatalf("idempotent ReleaseBackfillLock error = %v", err)
	}
	held, err = backend.AcquireBackfillLock(ctx, "documents", "ns-a", "token-3", 2*time.Second)
	if err != nil || !held {
		t.Fatalf("post-release AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
}

// TestBackfillLockTakeoverAfterExpiry covers holder exit by lease expiry: an
// expired lock no longer blocks a takeover and the old token cannot renew or
// release the new holder's lock.
func TestBackfillLockTakeoverAfterExpiry(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()

	held, err := backend.AcquireBackfillLock(ctx, "documents", "ns-a", "old-token", 1*time.Second)
	if err != nil || !held {
		t.Fatalf("AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
	server.FastForward(2 * time.Second)

	held, err = backend.AcquireBackfillLock(ctx, "documents", "ns-a", "new-token", 1*time.Second)
	if err != nil || !held {
		t.Fatalf("expired AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
	renewed, err := backend.RenewBackfillLock(ctx, "documents", "ns-a", "old-token", 1*time.Second)
	if err != nil || renewed {
		t.Fatalf("stale RenewBackfillLock = (%v, %v), want (false, nil)", renewed, err)
	}
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns-a", "old-token"); err != nil {
		t.Fatalf("stale ReleaseBackfillLock error = %v", err)
	}
	held, err = backend.AcquireBackfillLock(ctx, "documents", "ns-a", "probe", 1*time.Second)
	if err != nil || held {
		t.Fatalf("post-stale-release AcquireBackfillLock = (%v, %v), want (false, nil)", held, err)
	}
}

func TestBackfillPutRejectsExpiredHolderBeforeSuccessorPublishes(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()
	const kind, scope = "documents", "manuals"
	const oldToken, newToken = "old-token", "new-token"

	if held, err := backend.AcquireBackfillLock(ctx, kind, scope, oldToken, time.Second); err != nil || !held {
		t.Fatalf("acquire old lease = (%v, %v)", held, err)
	}
	if stored, err := backend.Put(ctx, kind, scope, oldToken, 19, []string{"published.pdf"}, false); err != nil || !stored {
		t.Fatalf("publish existing list = (%v, %v)", stored, err)
	}
	server.FastForward(time.Second)
	if stored, err := backend.Put(ctx, kind, scope, oldToken, 20, []string{"stale.pdf"}, false); stored || !errors.Is(err, state.ErrCacheBackfillLeaseLost) {
		t.Fatalf("publish after expiry = (%v, %v), want lease loss", stored, err)
	}

	if held, err := backend.AcquireBackfillLock(ctx, kind, scope, newToken, time.Second); err != nil || !held {
		t.Fatalf("acquire successor lease = (%v, %v)", held, err)
	}
	if stored, err := backend.Put(ctx, kind, scope, oldToken, 20, []string{"stale.pdf"}, false); stored || !errors.Is(err, state.ErrCacheBackfillLeaseLost) {
		t.Fatalf("publish after takeover = (%v, %v), want lease loss", stored, err)
	}
	if revision, err := server.Get(backend.RevisionKey(kind, scope)); err != nil || revision != "19:0" {
		t.Fatalf("revision after rejected writes = (%q, %v), want 19:0", revision, err)
	}
	if hit, cached, err := backend.Get(ctx, kind, scope); err != nil || !hit {
		t.Fatalf("read existing list = (%v, %v, %v)", hit, cached, err)
	} else if list, ok := cached.([]any); !ok || len(list) != 1 || list[0] != "published.pdf" {
		t.Fatalf("list after rejected writes = %v, want existing list", cached)
	}
	if stored, err := backend.Put(ctx, kind, scope, newToken, 21, []string{"published.pdf", "new.pdf"}, false); err != nil || !stored {
		t.Fatalf("publish as successor = (%v, %v)", stored, err)
	}
}

// TestBackfillLockUnreachableBackendIsUnavailable covers the outage semantics:
// an unreachable backend is a sanitized CacheUnavailable, never a silent
// "lock is free" answer.
func TestBackfillLockUnreachableBackendIsUnavailable(t *testing.T) {
	backend, server := newTestBackend(t)
	ctx := context.Background()
	held, err := backend.AcquireBackfillLock(ctx, "documents", "ns-a", "token", 2*time.Second)
	if err != nil || !held {
		t.Fatalf("AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
	server.SetError("simulated redis outage")
	if _, err := backend.AcquireBackfillLock(ctx, "documents", "ns-b", "token", 2*time.Second); err == nil {
		t.Fatal("unreachable backend must report CacheUnavailable")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) || unavailable.Message != "State cache backfill lock unavailable" {
			t.Fatalf("unreachable acquire error = %v, want the sanitized backfill lock message", err)
		}
	}
	if _, err := backend.RenewBackfillLock(ctx, "documents", "ns-a", "token", 2*time.Second); err == nil {
		t.Fatal("unreachable renew must report CacheUnavailable")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) {
			t.Fatalf("unreachable renew error = %v, want CacheUnavailable", err)
		}
	}
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns-a", "token"); err == nil {
		t.Fatal("unreachable release must report CacheUnavailable")
	} else {
		var unavailable *state.CacheUnavailable
		if !errors.As(err, &unavailable) {
			t.Fatalf("unreachable release error = %v, want CacheUnavailable", err)
		}
	}
}

// TestBackfillLockRejectsBadArguments pins the input guards.
func TestBackfillLockRejectsBadArguments(t *testing.T) {
	backend, _ := newTestBackend(t)
	ctx := context.Background()
	if _, err := backend.AcquireBackfillLock(ctx, "documents", "ns", "", time.Second); err == nil {
		t.Fatal("empty token acquire must fail")
	} else {
		var valueErr *state.ValueError
		if !errors.As(err, &valueErr) {
			t.Fatalf("empty token error = %v, want ValueError", err)
		}
	}
	if _, err := backend.AcquireBackfillLock(ctx, "documents", "ns", "t", 0); err == nil {
		t.Fatal("zero TTL acquire must fail")
	}
	if _, err := backend.RenewBackfillLock(ctx, "documents", "ns", "", time.Second); err == nil {
		t.Fatal("empty token renew must fail")
	}
	if _, err := backend.RenewBackfillLock(ctx, "documents", "ns", "t", -time.Second); err == nil {
		t.Fatal("negative TTL renew must fail")
	}
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns", ""); err == nil {
		t.Fatal("empty token release must fail")
	}
}

// TestDisabledBackendLockIsAlwaysFree covers the no-cache mode: a disabled
// backend has no payload to guard, so locking is a free pass-through.
func TestDisabledBackendLockIsAlwaysFree(t *testing.T) {
	backend := NewDisabled()
	ctx := context.Background()
	held, err := backend.AcquireBackfillLock(ctx, "documents", "ns", "token", time.Second)
	if err != nil || !held {
		t.Fatalf("disabled AcquireBackfillLock = (%v, %v), want (true, nil)", held, err)
	}
	renewed, err := backend.RenewBackfillLock(ctx, "documents", "ns", "token", time.Second)
	if err != nil || !renewed {
		t.Fatalf("disabled RenewBackfillLock = (%v, %v), want (true, nil)", renewed, err)
	}
	if err := backend.ReleaseBackfillLock(ctx, "documents", "ns", "token"); err != nil {
		t.Fatalf("disabled ReleaseBackfillLock error = %v", err)
	}
}
