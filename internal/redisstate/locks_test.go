package redisstate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// lockTestNamespace is the namespace every integration subtest leases in.
const lockTestNamespace = "ns:integration"

// lockTestPrefix is deliberately not the production prefix.
const lockTestPrefix = "rag:test:lock"

// Two 32-hex owner tokens, the shape the upload journal issues.
const (
	lockOwnerA = "0123456789abcdef0123456789abcdef"
	lockOwnerB = "fedcba9876543210fedcba9876543210"
)

// TestDocumentLock exercises the Redis lease backend against a real server. It
// needs Lua and TIME semantics, which miniredis does not reproduce, so it reads
// RAG_TEST_REDIS_URL (for example redis://127.0.0.1:6379/15) and skips without it.
// Every subtest flushes the test database at the start.
func TestDocumentLock(t *testing.T) {
	if strings.TrimSpace(os.Getenv("RAG_TEST_REDIS_URL")) == "" {
		t.Skip("RAG_TEST_REDIS_URL is not set; skipping the Redis document-lock integration test")
	}
	t.Run("storage-layout", testDocumentLockStorageLayout)
	t.Run("cross-client-mutual-exclusion", testDocumentLockMutualExclusion)
	t.Run("namespace-exclusive", testDocumentLockNamespaceExclusive)
	t.Run("foreign-owner", testDocumentLockForeignOwner)
	t.Run("lease-expiry", testDocumentLockLeaseExpiry)
	t.Run("inspect-projection", testDocumentLockInspectProjection)
	t.Run("release-semantics", testDocumentLockReleaseSemantics)
}

// redisIntegrationURL returns the test Redis URL or skips the test.
func redisIntegrationURL(t *testing.T) string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("RAG_TEST_REDIS_URL"))
	if raw == "" {
		t.Skip("RAG_TEST_REDIS_URL is not set; skipping the Redis document-lock integration test")
	}
	return raw
}

// lockSettings returns valid settings pointing at rawURL.
func lockSettings(t *testing.T, rawURL string) state.DocumentLockSettings {
	t.Helper()
	settings := state.DefaultDocumentLockSettings()
	settings.URL = rawURL
	settings.Prefix = lockTestPrefix
	settings.TimeoutSeconds = 2
	settings.WaitSeconds = 0
	if err := settings.Validate(); err != nil {
		t.Fatalf("settings are invalid: %v", err)
	}
	return settings
}

// lockFixture is one simulated service instance: an independent go-redis client
// and the backend that uses it. Close (via the cleanup) closes the client.
type lockFixture struct {
	lock   *DocumentLock
	client *redis.Client
}

func newLockFixture(t *testing.T) *lockFixture {
	t.Helper()
	rawURL := redisIntegrationURL(t)
	options, err := redisOptionsFromURL(rawURL, 2)
	if err != nil {
		t.Fatalf("redisOptionsFromURL: %v", err)
	}
	client := redis.NewClient(options)
	lock, err := NewDocumentLock(lockSettings(t, rawURL), WithRedisClient(client))
	if err != nil {
		t.Fatalf("NewDocumentLock: %v", err)
	}
	t.Cleanup(func() { _ = lock.Close() })
	return &lockFixture{lock: lock, client: client}
}

// newLockPair returns two independent instances over one freshly flushed
// test database, which is what cross-client mutual exclusion needs.
func newLockPair(t *testing.T) (*lockFixture, *lockFixture) {
	t.Helper()
	first := newLockFixture(t)
	if err := first.client.FlushDB(context.Background()).Err(); err != nil {
		t.Fatalf("FLUSHDB: %v", err)
	}
	return first, newLockFixture(t)
}

// lockResource returns "kind:sha256hex(identity)", the resource naming scheme the
// upload journal uses.
func lockResource(kind, identity string) string {
	return kind + ":" + lockDigest(identity)
}

// lockDigest is the lowercase sha256 hex digest used for resources and owner
// projections.
func lockDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

// waitForLock polls condition every 20ms until it holds or limit expires.
func waitForLock(t *testing.T, limit time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s (waited up to %v)", description, limit)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func expectBusy(t *testing.T, err error) {
	t.Helper()
	var busy *state.LockBusy
	if !errors.As(err, &busy) {
		t.Fatalf("want *state.LockBusy, got %v", err)
	}
}

func expectLeaseLost(t *testing.T, err error) {
	t.Helper()
	var lost *state.LeaseLost
	if !errors.As(err, &lost) {
		t.Fatalf("want *state.LeaseLost, got %v", err)
	}
	// Callers map a lost lease onto the pre-migration UploadConflict identity.
	var conflict *state.UploadConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a lost lease must unwrap to *state.UploadConflict, got %v", err)
	}
	// The message is a fixed, token-free sentence from the contract.
	if message := lost.Error(); message != "document lease expired or was replaced" {
		t.Fatalf("lease lost message = %q", message)
	}
	for _, token := range []string{lockOwnerA, lockOwnerB} {
		if strings.Contains(err.Error(), token) {
			t.Fatalf("lease lost error leaks the owner token: %v", err)
		}
	}
}

// testDocumentLockStorageLayout pins the documented storage model: one hash per
// namespace, a hash tag for Cluster compatibility, one field per resource,
// "<owner>|<expiry_unix_millis>" values and a key TTL of about twice the lease.
func testDocumentLockStorageLayout(t *testing.T) {
	first, _ := newLockPair(t)
	ctx := context.Background()
	const lease = 20 * time.Second
	document := lockResource("document", "layout")
	if err := first.lock.Acquire(ctx, lockTestNamespace, []string{document}, lockOwnerA, lease); err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	key := first.lock.leaseKey(lockTestNamespace)
	wantKey := lockTestPrefix + ":{" + lockDigest(lockTestNamespace)[:32] + "}:leases"
	if key != wantKey {
		t.Fatalf("lease key = %q, want %q", key, wantKey)
	}
	if strings.Count(key, "{") != 1 || !strings.Contains(key, "}") {
		t.Fatalf("lease key must carry exactly one hash tag: %q", key)
	}
	if other := first.lock.leaseKey("ns:other"); other == key {
		t.Fatalf("namespaces must not share a lease hash: %q", other)
	}

	keyTTL, err := first.client.PTTL(ctx, key).Result()
	if err != nil {
		t.Fatalf("PTTL: %v", err)
	}
	if keyTTL <= lease || keyTTL > 2*lease {
		t.Fatalf("key TTL = %v, want (%v, %v]", keyTTL, lease, 2*lease)
	}

	value, err := first.client.HGet(ctx, key, document).Result()
	if err != nil {
		t.Fatalf("HGet: %v", err)
	}
	owner, expiry, ok := parseLeaseValue(value)
	if !ok || owner != lockOwnerA {
		t.Fatalf("lease field %q = %q", document, value)
	}
	// The expiry must come from the Redis server clock and be in milliseconds.
	localNow := time.Now().UnixMilli()
	leaseInMillis := int64(lease / time.Millisecond)
	if expiry <= localNow || expiry > localNow+2*leaseInMillis {
		t.Fatalf("expiry = %d, local now = %d: want a millisecond server-side expiry in (now, now+2*lease]", expiry, localNow)
	}
}

// testDocumentLockMutualExclusion proves two independent clients exclude each
// other, and that a conflict is all-or-nothing.
func testDocumentLockMutualExclusion(t *testing.T) {
	a, b := newLockPair(t)
	ctx := context.Background()
	const lease = 10 * time.Second

	document := lockResource("document", "x")
	name := lockResource("name", "y")
	otherDocument := lockResource("document", "z")
	owned := []string{document, name}

	if err := a.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerA, lease); err != nil {
		t.Fatalf("A Acquire: %v", err)
	}
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerB, lease))
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, []string{document}, lockOwnerB, lease))
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, []string{name, otherDocument}, lockOwnerB, lease))

	// The failed acquisitions left nothing behind: exactly A's two fields, all
	// carrying A's token, and no field for the rejected resource.
	fields, err := a.client.HGetAll(ctx, a.lock.leaseKey(lockTestNamespace)).Result()
	if err != nil {
		t.Fatalf("HGetAll: %v", err)
	}
	if len(fields) != len(owned) {
		t.Fatalf("hash fields = %v, want only %v", fields, owned)
	}
	for _, resource := range owned {
		value, present := fields[resource]
		if !present || !strings.HasPrefix(value, lockOwnerA+"|") {
			t.Fatalf("field %q = %q", resource, value)
		}
	}
	if _, present := fields[otherDocument]; present {
		t.Fatalf("a conflicting acquisition left %q behind", otherDocument)
	}

	// A re-acquiring what it already owns refreshes instead of conflicting; a
	// disjoint resource stays available to the other instance.
	if err := a.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerA, lease); err != nil {
		t.Fatalf("A re-Acquire: %v", err)
	}
	if err := b.lock.Acquire(ctx, lockTestNamespace, []string{otherDocument}, lockOwnerB, lease); err != nil {
		t.Fatalf("B Acquire disjoint: %v", err)
	}
	if inspected, err := b.lock.Inspect(ctx, lockTestNamespace, otherDocument); err != nil || !inspected.Live() {
		t.Fatalf("B disjoint lease = %+v, %v", inspected, err)
	}
}

// testDocumentLockNamespaceExclusive covers both directions of the exclusive
// rule and confirms independent resources in one namespace run in parallel.
func testDocumentLockNamespaceExclusive(t *testing.T) {
	a, b := newLockPair(t)
	ctx := context.Background()
	const lease = 10 * time.Second
	otherNamespace := "ns:other"
	document := lockResource("document", "x")
	otherDocument := lockResource("document", "y")

	if err := a.lock.Acquire(ctx, lockTestNamespace, []string{exclusiveResource}, lockOwnerA, lease); err != nil {
		t.Fatalf("A exclusive: %v", err)
	}
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, []string{document}, lockOwnerB, lease))
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, []string{exclusiveResource}, lockOwnerB, lease))
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, []string{document, lockResource("name", "y")}, lockOwnerB, lease))
	// Another namespace is a different hash and stays available.
	if err := b.lock.Acquire(ctx, otherNamespace, []string{document}, lockOwnerB, lease); err != nil {
		t.Fatalf("B Acquire other namespace: %v", err)
	}

	if err := a.lock.Release(ctx, lockTestNamespace, []string{exclusiveResource}, lockOwnerA); err != nil {
		t.Fatalf("A release exclusive: %v", err)
	}
	// Reverse direction: a live resource holder blocks an exclusive request.
	if err := a.lock.Acquire(ctx, lockTestNamespace, []string{document}, lockOwnerA, lease); err != nil {
		t.Fatalf("A Acquire document: %v", err)
	}
	expectBusy(t, b.lock.Acquire(ctx, lockTestNamespace, []string{exclusiveResource}, lockOwnerB, lease))
	// Distinct documents are independent.
	if err := b.lock.Acquire(ctx, lockTestNamespace, []string{otherDocument}, lockOwnerB, lease); err != nil {
		t.Fatalf("B Acquire second document: %v", err)
	}
	if err := a.lock.Release(ctx, lockTestNamespace, []string{document}, lockOwnerA); err != nil {
		t.Fatalf("A release document: %v", err)
	}
	if err := b.lock.Release(ctx, lockTestNamespace, []string{otherDocument}, lockOwnerB); err != nil {
		t.Fatalf("B release document: %v", err)
	}
	// With the namespace clear the exclusive lease is granted again.
	if err := b.lock.Acquire(ctx, lockTestNamespace, []string{exclusiveResource}, lockOwnerB, lease); err != nil {
		t.Fatalf("B exclusive after cleanup: %v", err)
	}
}

// testDocumentLockForeignOwner proves a token grants ownership: another owner
// can neither renew nor release it, and a partial set fails without side effects.
func testDocumentLockForeignOwner(t *testing.T) {
	a, b := newLockPair(t)
	ctx := context.Background()
	const lease = 10 * time.Second
	document := lockResource("document", "x")
	name := lockResource("name", "y")
	owned := []string{document, name}

	if err := a.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerA, lease); err != nil {
		t.Fatalf("A Acquire: %v", err)
	}
	expectLeaseLost(t, b.lock.Renew(ctx, lockTestNamespace, owned, lockOwnerB, lease))
	expectLeaseLost(t, b.lock.Renew(ctx, lockTestNamespace, []string{document}, lockOwnerB, lease))
	// Renewing a resource nobody holds also loses the lease.
	expectLeaseLost(t, a.lock.Renew(ctx, lockTestNamespace, []string{lockResource("document", "z")}, lockOwnerA, lease))

	// A foreign release is a silent no-op, and A keeps a renewable lease.
	if err := b.lock.Release(ctx, lockTestNamespace, owned, lockOwnerB); err != nil {
		t.Fatalf("foreign Release: %v", err)
	}
	if err := a.lock.Renew(ctx, lockTestNamespace, owned, lockOwnerA, lease); err != nil {
		t.Fatalf("A must still own the lease after a foreign release: %v", err)
	}
	if inspected, err := b.lock.Inspect(ctx, lockTestNamespace, document); err != nil || inspected.OwnerDigest != lockDigest(lockOwnerA)[:16] {
		t.Fatalf("B sees %+v, %v; want A's digest", inspected, err)
	}
}

// testDocumentLockLeaseExpiry checks the server-side expiry path without a fixed
// sleep: it polls for the lease to lapse, then for the other instance to take
// over.
func testDocumentLockLeaseExpiry(t *testing.T) {
	a, b := newLockPair(t)
	ctx := context.Background()
	const shortLease = 150 * time.Millisecond
	document := lockResource("document", "expiring")
	owned := []string{document}

	if err := a.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerA, shortLease); err != nil {
		t.Fatalf("A Acquire: %v", err)
	}
	inspected, err := a.lock.Inspect(ctx, lockTestNamespace, document)
	if err != nil || !inspected.Live() {
		t.Fatalf("Inspect right after Acquire = %+v, %v", inspected, err)
	}
	if inspected.TTL > shortLease {
		t.Fatalf("Inspect TTL = %v, want at most %v", inspected.TTL, shortLease)
	}

	waitForLock(t, 3*time.Second, "the lease never expired", func() bool {
		current, err := a.lock.Inspect(ctx, lockTestNamespace, document)
		return err == nil && !current.Live() && current == state.LockState{}
	})
	expectLeaseLost(t, a.lock.Renew(ctx, lockTestNamespace, owned, lockOwnerA, 10*time.Second))

	// The expired holder cannot refresh, so the resource is up for grabs; poll so
	// a slow server never turns timing into a failure.
	var terminal error
	waitForLock(t, 3*time.Second, "B never acquired the expired lease", func() bool {
		err := b.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerB, 10*time.Second)
		if err == nil {
			return true
		}
		var busy *state.LockBusy
		if errors.As(err, &busy) {
			return false
		}
		terminal = err
		return true
	})
	if terminal != nil {
		t.Fatalf("B Acquire after expiry: %v", terminal)
	}
	inspected, err = b.lock.Inspect(ctx, lockTestNamespace, document)
	if err != nil || !inspected.Live() || inspected.OwnerDigest != lockDigest(lockOwnerB)[:16] {
		t.Fatalf("B lease after takeover = %+v, %v", inspected, err)
	}
	expectLeaseLost(t, a.lock.Renew(ctx, lockTestNamespace, owned, lockOwnerA, 10*time.Second))
}

// testDocumentLockInspectProjection checks the display projection: a short owner
// digest, never the raw token, and a zero state for unheld resources.
func testDocumentLockInspectProjection(t *testing.T) {
	a, b := newLockPair(t)
	ctx := context.Background()
	const lease = 30 * time.Second
	document := lockResource("document", "x")

	if err := a.lock.Acquire(ctx, lockTestNamespace, []string{document}, lockOwnerA, lease); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	inspected, err := a.lock.Inspect(ctx, lockTestNamespace, document)
	if err != nil {
		t.Fatalf("Inspect: %v", err)
	}
	wantDigest := lockDigest(lockOwnerA)[:16]
	if inspected.OwnerDigest != wantDigest {
		t.Fatalf("OwnerDigest = %q, want %q", inspected.OwnerDigest, wantDigest)
	}
	if inspected.OwnerDigest == lockOwnerA {
		t.Fatalf("Inspect must never return the raw owner token")
	}
	if len(inspected.OwnerDigest) != 16 || strings.Trim(inspected.OwnerDigest, "0123456789abcdef") != "" {
		t.Fatalf("OwnerDigest = %q, want 16 lowercase hex characters", inspected.OwnerDigest)
	}
	if inspected.TTL <= 0 || inspected.TTL > lease || !inspected.Live() {
		t.Fatalf("Inspect TTL = %v, want (0, %v]", inspected.TTL, lease)
	}

	// The other instance sees the same projection: Redis is the shared source.
	fromB, err := b.lock.Inspect(ctx, lockTestNamespace, document)
	if err != nil || fromB.OwnerDigest != inspected.OwnerDigest {
		t.Fatalf("B Inspect = %+v, %v", fromB, err)
	}

	// An unheld resource is a zero state, not an error.
	free, err := a.lock.Inspect(ctx, lockTestNamespace, lockResource("document", "absent"))
	if err != nil {
		t.Fatalf("Inspect absent: %v", err)
	}
	if free != (state.LockState{}) {
		t.Fatalf("absent resource = %+v, want the zero state", free)
	}
}

// testDocumentLockReleaseSemantics covers release-then-reacquire, hash cleanup
// and a late release that must not drop a new holder.
func testDocumentLockReleaseSemantics(t *testing.T) {
	a, b := newLockPair(t)
	ctx := context.Background()
	const lease = 10 * time.Second
	document := lockResource("document", "x")
	name := lockResource("name", "y")
	owned := []string{document, name}
	key := a.lock.leaseKey(lockTestNamespace)

	if err := a.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerA, lease); err != nil {
		t.Fatalf("A Acquire: %v", err)
	}
	if err := a.lock.Release(ctx, lockTestNamespace, owned, lockOwnerA); err != nil {
		t.Fatalf("A Release: %v", err)
	}
	// An emptied hash is deleted, not kept alive by the safety TTL.
	if exists, err := a.client.Exists(ctx, key).Result(); err != nil || exists != 0 {
		t.Fatalf("Exists(%q) = %d, %v; want 0", key, exists, err)
	}
	if inspected, err := a.lock.Inspect(ctx, lockTestNamespace, document); err != nil || inspected != (state.LockState{}) {
		t.Fatalf("Inspect after release = %+v, %v", inspected, err)
	}
	// Release is idempotent, including for resources nobody holds.
	if err := a.lock.Release(ctx, lockTestNamespace, owned, lockOwnerA); err != nil {
		t.Fatalf("repeat Release: %v", err)
	}

	if err := b.lock.Acquire(ctx, lockTestNamespace, owned, lockOwnerB, lease); err != nil {
		t.Fatalf("B Acquire after release: %v", err)
	}
	// A's late release uses A's token and must not drop B's lease.
	if err := a.lock.Release(ctx, lockTestNamespace, owned, lockOwnerA); err != nil {
		t.Fatalf("A late Release: %v", err)
	}
	if inspected, err := a.lock.Inspect(ctx, lockTestNamespace, document); err != nil || !inspected.Live() || inspected.OwnerDigest != lockDigest(lockOwnerB)[:16] {
		t.Fatalf("B lease after A's late release = %+v, %v", inspected, err)
	}
	if err := b.lock.Renew(ctx, lockTestNamespace, owned, lockOwnerB, lease); err != nil {
		t.Fatalf("B Renew after A's late release: %v", err)
	}
}

// TestDocumentLockSettingsValidation is the Redis-free unit test: construction
// validates settings, never dials, and every operation reports a typed
// unavailable error when no backend is attached.
func TestDocumentLockSettingsValidation(t *testing.T) {
	// The Lua scripts hardcode the exclusive field name; keep it aligned with the
	// contract constant every caller uses.
	if exclusiveResource != state.UploadNamespaceResource {
		t.Fatalf("exclusiveResource = %q, want %q", exclusiveResource, state.UploadNamespaceResource)
	}

	// A settings value without a URL is rejected: Redis is the required document
	// lock backend, so a missing address is a configuration error instead of a
	// silent no-op.
	noURL := state.DefaultDocumentLockSettings()
	if _, err := NewDocumentLock(noURL); err == nil {
		t.Fatalf("settings without a URL must be rejected")
	}

	// Invalid knobs are rejected, and the message never contains the
	// configured URL.
	invalidTTL := state.DefaultDocumentLockSettings()
	invalidTTL.URL = "redis://127.0.0.1:1/0"
	invalidTTL.TTLSeconds = 0
	if _, err := NewDocumentLock(invalidTTL); err == nil {
		t.Fatalf("non-positive TTL must be rejected")
	} else if strings.Contains(err.Error(), "127.0.0.1") || strings.Contains(err.Error(), "redis://") {
		t.Fatalf("validation error leaks the Redis URL: %v", err)
	}

	emptyPrefix := state.DefaultDocumentLockSettings()
	emptyPrefix.URL = "redis://127.0.0.1:1/0"
	emptyPrefix.Prefix = ""
	if _, err := NewDocumentLock(emptyPrefix); err == nil {
		t.Fatalf("empty prefix must be rejected")
	}

	// A valid configuration constructs lazily: an address nothing listens on must
	// not fail construction, and closing a never-used backend is clean.
	lazy := state.DefaultDocumentLockSettings()
	lazy.URL = "redis://127.0.0.1:1/0"
	lazy.TimeoutSeconds = 0.05
	unused, err := NewDocumentLock(lazy)
	if err != nil {
		t.Fatalf("construction must not dial: %v", err)
	}
	if err := unused.Close(); err != nil {
		t.Fatalf("Close before first use: %v", err)
	}
}
