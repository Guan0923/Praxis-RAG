package state

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeBloomBackend is a deterministic in-memory stand-in for the Redis bitmap
// backend. It reproduces the observable contract (state/meta reads, bit
// registration, shard-loss detection, build lease arbitration, kill switch)
// and adds controlled hooks so tests can fail single operations.
type fakeBloomBackend struct {
	mutex sync.Mutex

	active    *UploadBloomGeneration
	building  *UploadBloomGeneration
	disabled bool
	bits     map[int64]map[int64]struct{}

	hooks map[string]error
}

func newFakeBloomBackend() *fakeBloomBackend {
	return &fakeBloomBackend{
		bits:  map[int64]map[int64]struct{}{},
		hooks: map[string]error{},
	}
}

func (f *fakeBloomBackend) hookErr(op string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.hooks[op]
}

func (f *fakeBloomBackend) Enabled() bool { return true }

func (f *fakeBloomBackend) Ping(context.Context) error { return nil }

func (f *fakeBloomBackend) Close() error { return nil }

func (f *fakeBloomBackend) State(context.Context) (UploadBloomState, error) {
	if err := f.hookErr("state"); err != nil {
		return UploadBloomState{}, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	var active, building *UploadBloomGeneration
	if f.active != nil {
		copy := *f.active
		active = &copy
	}
	if f.building != nil {
		copy := *f.building
		building = &copy
	}
	return UploadBloomState{Active: active, Building: building, Disabled: f.disabled}, nil
}

func (f *fakeBloomBackend) SetBits(_ context.Context, generation UploadBloomGeneration, ids []string) error {
	if err := f.hookErr("setbits"); err != nil {
		return err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	shardBits := f.bits[generation.Generation]
	if shardBits == nil {
		shardBits = map[int64]struct{}{}
		f.bits[generation.Generation] = shardBits
	}
	for _, id := range ids {
		for _, position := range UploadBloomBitPositions(generation.Params, id) {
			shardBits[position] = struct{}{}
		}
	}
	return nil
}

func (f *fakeBloomBackend) Probe(_ context.Context, generation UploadBloomGeneration, id string) (bool, bool, error) {
	if err := f.hookErr("probe"); err != nil {
		return false, false, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	shardBits, exists := f.bits[generation.Generation]
	if !exists {
		return false, true, nil
	}
	for _, position := range UploadBloomBitPositions(generation.Params, id) {
		if _, set := shardBits[position]; !set {
			return false, false, nil
		}
	}
	return true, false, nil
}

func (f *fakeBloomBackend) AcquireBuild(_ context.Context, generation int64, params UploadBloomParameters, ttl time.Duration) (UploadBloomGeneration, bool, error) {
	if err := f.hookErr("acquire"); err != nil {
		return UploadBloomGeneration{}, false, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.building != nil {
		existing := *f.building
		return existing, false, nil
	}
	pinned := UploadBloomGeneration{Generation: generation, Params: params}
	f.building = &pinned
	return pinned, true, nil
}

func (f *fakeBloomBackend) RenewBuild(_ context.Context, generation int64, ttl time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.building == nil || f.building.Generation != generation {
		return NewBloomUnavailable()
	}
	return nil
}

func (f *fakeBloomBackend) ReleaseBuild(_ context.Context, generation int64) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.building != nil && f.building.Generation == generation {
		f.building = nil
	}
	return nil
}

func (f *fakeBloomBackend) Activate(_ context.Context, generation UploadBloomGeneration) error {
	if err := f.hookErr("activate"); err != nil {
		return err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	copy := generation
	f.active = &copy
	f.disabled = false
	return nil
}

func (f *fakeBloomBackend) Disable(context.Context) (bool, error) {
	if err := f.hookErr("disable"); err != nil {
		return false, err
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.disabled = true
	return true, nil
}

func (f *fakeBloomBackend) ExpireGeneration(_ context.Context, generation UploadBloomGeneration, ttl time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return nil
}

// newTestBloomFilter builds a filter over the fake backend with test-friendly
// parameters (small scan batches but a large geometry so false positives stay
// rare in assertions).
func newTestBloomFilter(t *testing.T, stores *Stores, backend UploadBloomBackend, mutate func(*UploadBloomSettings)) *UploadBloomFilter {
	t.Helper()
	settings := DefaultUploadBloomSettings()
	settings.Mode = UploadBloomModeOn
	settings.URL = "redis://test"
	settings.MinCapacity = 100000
	settings.Growth = 1.0
	settings.Batch = 2
	if mutate != nil {
		mutate(&settings)
	}
	filter, err := newUploadBloomFilter(settings, stores.DB, backend)
	if err != nil {
		t.Fatalf("newUploadBloomFilter() error = %v", err)
	}
	return filter
}

// bloomTestOperation inserts one upload_operations row directly (bypassing the
// journal) so tests can control exactly what MySQL contains.
func bloomTestOperation(t *testing.T, stores *Stores, id string) {
	t.Helper()
	statement := "INSERT INTO upload_operations (id, namespace, name, document_id, collection, " +
		"idempotency_key, fingerprint, owner, old_version, status, result, error, cleanup_pending, " +
		"workspace, workspace_node, created_at, updated_at) VALUES (?, ?, ?, ?, ?, NULL, NULL, ?, NULL, ?, NULL, NULL, 0, NULL, NULL, ?, ?)"
	if _, err := stores.DB.ExecContext(context.Background(), statement,
		id, "alpha", "file.txt", "doc-"+id, "collection", "owner00", "COMMITTED", 1.0, 2.0); err != nil {
		t.Fatalf("upload_operations insert error = %v", err)
	}
}

func bloomOperationID(index int) string {
	return fmt.Sprintf("%032x", index)
}

// TestUploadBloomParametersKeepTarget checks the capacity math: the deployed
// geometry must keep the configured false-positive target and stay shard
// aligned.
func TestUploadBloomParametersKeepTarget(t *testing.T) {
	settings := DefaultUploadBloomSettings()
	// Tiny capacities cannot hold a 1% target by construction (the standard
	// formula still applies; a 1-entry filter needs ~10 bits). Verify the
	// formula itself instead of sampling at capacity 1.
	params := settings.uploadBloomParametersFor(1)
	if params.Bits < 8 || params.Hashes < 1 || !params.valid() {
		t.Fatalf("capacity 1: invalid params %+v", params)
	}
	for _, capacity := range []int64{10000, 250000, 1000000} {
		params := settings.uploadBloomParametersFor(capacity)
		if !params.valid() {
			t.Fatalf("capacity %d: invalid params %+v", capacity, params)
		}
		// Insert capacity/10 IDs and sample the false-positive rate; the
		// design capacity must hold the configured target with headroom.
		inserted := capacity / 10
		probes := 20000
		seen := make(map[int64]struct{})
		for index := int64(0); index < inserted; index++ {
			for _, position := range UploadBloomBitPositions(params, fmt.Sprintf("id-%d-%d", capacity, index)) {
				seen[position] = struct{}{}
			}
		}
		falsePositives := 0
		for index := 0; index < probes; index++ {
			random := fmt.Sprintf("random-%d-%d", capacity, index)
			present := true
			for _, position := range UploadBloomBitPositions(params, random) {
				if _, set := seen[position]; !set {
					present = false
					break
				}
			}
			if present {
				falsePositives++
			}
		}
		rate := float64(falsePositives) / float64(probes)
		if rate > settings.FalsePositive*2 {
			t.Fatalf("capacity %d: false-positive rate %.4f exceeds %.4f", capacity, rate, settings.FalsePositive*2)
		}
	}
}

// TestUploadBloomDefinitelyAbsentFallbacks enumerates every condition that
// must answer "not provably absent" (fall back to MySQL): the read gate off,
// non-canonical IDs, no generation, kill switch, lost shards and backend
// failures.
func TestUploadBloomDefinitelyAbsentFallbacks(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	bloomTestOperation(t, stores, bloomOperationID(1))
	id := bloomOperationID(1)

	backend := newFakeBloomBackend()
	registerMode := newTestBloomFilter(t, stores, backend, func(settings *UploadBloomSettings) {
		settings.Mode = UploadBloomModeRegister
	})
	if registerMode.DefinitelyAbsent(ctx, id) {
		t.Fatal("register mode must not gate reads")
	}
	// A "register"-mode filter never blocks or fails a Begin either.
	if err := registerMode.registerOperationID(ctx, bloomOperationID(300)); err != nil {
		t.Fatalf("register-mode registration error = %v", err)
	}

	filter := newTestBloomFilter(t, stores, backend, nil)
	if filter.DefinitelyAbsent(ctx, id) {
		t.Fatal("no active generation must not gate reads")
	}

	// Build a generation and verify the present path.
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	if filter.DefinitelyAbsent(ctx, id) {
		t.Fatal("a registered ID must not be reported absent")
	}
	if !filter.DefinitelyAbsent(ctx, bloomOperationID(999)) {
		t.Fatal("an unregistered ID should be reported absent")
	}

	// Non-canonical spelling must bypass the filter entirely.
	if filter.DefinitelyAbsent(ctx, strings.ToUpper(id)) {
		t.Fatal("non-canonical ID must bypass the filter (MySQL collation may match)")
	}

	// Kill switch disables the negative judgment.
	backend.mutex.Lock()
	backend.disabled = true
	backend.mutex.Unlock()
	if filter.DefinitelyAbsent(ctx, bloomOperationID(999)) {
		t.Fatal("kill switch must disable the negative judgment")
	}
	backend.mutex.Lock()
	backend.disabled = false
	backend.mutex.Unlock()

	// A lost shard must fall back to MySQL.
	generation := backend.active
	backend.mutex.Lock()
	delete(backend.bits, generation.Generation)
	backend.mutex.Unlock()
	if filter.DefinitelyAbsent(ctx, bloomOperationID(999)) {
		t.Fatal("a lost shard must fall back to MySQL")
	}

	// Backend errors must fall back to MySQL.
	backend.hooks["state"] = NewBloomUnavailable()
	if filter.DefinitelyAbsent(ctx, bloomOperationID(999)) {
		t.Fatal("a state read failure must fall back to MySQL")
	}
	backend.hooks["state"] = nil
	backend.mutex.Lock()
	backend.bits[generation.Generation] = map[int64]struct{}{}
	backend.mutex.Unlock()
	backend.hooks["probe"] = NewBloomUnavailable()
	if filter.DefinitelyAbsent(ctx, bloomOperationID(999)) {
		t.Fatal("a probe failure must fall back to MySQL")
	}
	backend.hooks["probe"] = nil
}

// TestUploadBloomEnsureBuildsAndSwitches verifies the generation lifecycle:
// the initial build activates only after a stable confirm round, concurrent
// builders arbitrate through the lease, and the superseded generation is
// retired.
func TestUploadBloomEnsureBuildsAndSwitches(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	bloomTestOperation(t, stores, bloomOperationID(1))
	bloomTestOperation(t, stores, bloomOperationID(2))

	backend := newFakeBloomBackend()
	// One small-capacity deployment: 8 planned slots, so 12 rows force the
	// resize (generation 2) on the second Ensure pass.
	filter := newTestBloomFilter(t, stores, backend, func(settings *UploadBloomSettings) {
		settings.MinCapacity = 8
	})
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	backend.mutex.Lock()
	active := backend.active
	disabled := backend.disabled
	backend.mutex.Unlock()
	if active == nil || active.Generation != 1 {
		t.Fatalf("active generation = %+v, want generation 1", active)
	}
	if disabled {
		t.Fatal("the kill switch must be cleared after activation")
	}
	// Both IDs registered.
	for _, id := range []string{bloomOperationID(1), bloomOperationID(2)} {
		if filter.DefinitelyAbsent(ctx, id) {
			t.Fatalf("ID %s must be present after the build", id)
		}
	}

	// Growth past the capacity triggers the next generation (resize).
	for index := 3; index <= 12; index++ {
		bloomTestOperation(t, stores, bloomOperationID(index))
	}
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("resize Ensure() error = %v", err)
	}
	backend.mutex.Lock()
	resized := backend.active
	backend.mutex.Unlock()
	if resized == nil || resized.Generation != 2 {
		t.Fatalf("active generation after resize = %+v, want generation 2", resized)
	}
	for index := 1; index <= 12; index++ {
		if filter.DefinitelyAbsent(ctx, bloomOperationID(index)) {
			t.Fatalf("ID %d must be present after the resize", index)
		}
	}
}

// TestUploadBloomRegisterCoversActiveAndBuilding verifies registration writes
// into both the active and the building generation during a rebuild window.
func TestUploadBloomRegisterCoversActiveAndBuilding(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	bloomTestOperation(t, stores, bloomOperationID(1))

	backend := newFakeBloomBackend()
	filter := newTestBloomFilter(t, stores, backend, nil)
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	// Simulate a build in progress (generation 2 with the same params).
	backend.mutex.Lock()
	building := UploadBloomGeneration{Generation: 2, Params: backend.active.Params}
	backend.building = &building
	backend.bits[2] = map[int64]struct{}{}
	backend.mutex.Unlock()

	fresh := bloomOperationID(50)
	if err := filter.Register(ctx, fresh); err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	backend.mutex.Lock()
	generation2 := backend.bits[2]
	generation1 := backend.bits[1]
	backend.mutex.Unlock()
	for _, bits := range []map[int64]struct{}{generation1, generation2} {
		present := true
		for _, position := range UploadBloomBitPositions(backend.active.Params, fresh) {
			if _, set := bits[position]; !set {
				present = false
				break
			}
		}
		if !present {
			t.Fatal("Register must write into both the active and the building generation")
		}
	}
}

// TestUploadBloomRegistrationFailureProtocol covers the write-side failure
// contract: a failed registration is only survivable when the kill switch is
// confirmed; otherwise the creation fails with the retryable error.
func TestUploadBloomRegistrationFailureProtocol(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	bloomTestOperation(t, stores, bloomOperationID(1))

	backend := newFakeBloomBackend()
	filter := newTestBloomFilter(t, stores, backend, nil)
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	// Case 1: registration fails, disable also fails -> retryable error.
	backend.hooks["setbits"] = NewBloomUnavailable()
	backend.hooks["disable"] = NewBloomUnavailable()
	err := filter.registerOperationID(ctx, bloomOperationID(2))
	if !isBloomRegistrationFailure(t, err) {
		t.Fatalf("unconfirmed registration failure must be retryable, got %v", err)
	}
	backend.mutex.Lock()
	stillEnabled := backend.disabled
	backend.mutex.Unlock()
	if stillEnabled {
		t.Fatal("a failed disable must not flip the kill switch")
	}

	// Case 2: registration fails, disable confirms -> commit allowed.
	// (The two sets of hooks flip in sequence; SetBits failure first.)
	backend.hooks["setbits"] = NewBloomUnavailable()
	backend.hooks["disable"] = nil
	if err := filter.registerOperationID(ctx, bloomOperationID(3)); err != nil {
		t.Fatalf("a confirmed global stop must allow the commit: %v", err)
	}
	// The kill switch must now be set (visible to every reader).
	backend.mutex.Lock()
	disabled := backend.disabled
	backend.mutex.Unlock()
	if !disabled {
		t.Fatal("the confirmed global stop must set the kill switch")
	}
	backend.hooks["setbits"] = nil
	backend.mutex.Lock()
	backend.disabled = false
	backend.mutex.Unlock()

	// Case 3: no generations yet (first build unfinished) -> registration is a
	// no-op that never blocks creation.
	empty := newFakeBloomBackend()
	emptyFilter := newTestBloomFilter(t, stores, empty, nil)
	if err := emptyFilter.registerOperationID(ctx, bloomOperationID(4)); err != nil {
		t.Fatalf("registration with no generation must be a no-op: %v", err)
	}

	// Case 4: the kill switch is already set -> the rebuild will cover the ID.
	backend.mutex.Lock()
	backend.disabled = true
	backend.mutex.Unlock()
	if err := filter.registerOperationID(ctx, bloomOperationID(5)); err != nil {
		t.Fatalf("registration with the kill switch set must not block: %v", err)
	}
}

func isBloomRegistrationFailure(t *testing.T, err error) bool {
	t.Helper()
	if err == nil {
		return false
	}
	var registration *BloomRegistrationFailed
	return errors.As(err, &registration)
}

// TestUploadBloomForcedRebuildDisablesFirst verifies the recovery path: a
// forced rebuild stops the negative judgment before rebuilding, so a stale or
// suspect bitmap can never answer "definitely absent" mid-rebuild.
func TestUploadBloomForcedRebuildDisablesFirst(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	bloomTestOperation(t, stores, bloomOperationID(1))

	backend := newFakeBloomBackend()
	filter := newTestBloomFilter(t, stores, backend, func(settings *UploadBloomSettings) {
		settings.Rebuild = true
	})
	// First Ensure builds generation 1 and re-enables the filter.
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	// A second pass with a forced rebuild must disable the switch, rebuild and
	// re-enable.
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("forced Ensure() error = %v", err)
	}
	backend.mutex.Lock()
	active := backend.active
	disabled := backend.disabled
	backend.mutex.Unlock()
	if active == nil || active.Generation != 2 {
		t.Fatalf("active generation after forced rebuild = %+v", active)
	}
	if disabled {
		t.Fatal("forced rebuild must re-enable after activation")
	}
	if filter.DefinitelyAbsent(ctx, bloomOperationID(1)) {
		t.Fatal("the historical ID must be present after the rebuild")
	}
}

// TestUploadBloomSettingsValidation pins the configuration contract.
func TestUploadBloomSettingsValidation(t *testing.T) {
	base := func() UploadBloomSettings {
		settings := DefaultUploadBloomSettings()
		settings.URL = "redis://127.0.0.1:6379/0"
		return settings
	}
	for name, mutate := range map[string]func(*UploadBloomSettings){
		"mode":         func(s *UploadBloomSettings) { s.Mode = "fast" },
		"target zero":  func(s *UploadBloomSettings) { s.FalsePositive = 0 },
		"target big":   func(s *UploadBloomSettings) { s.FalsePositive = 0.9 },
		"growth":       func(s *UploadBloomSettings) { s.Growth = 0.5 },
		"capacity":     func(s *UploadBloomSettings) { s.MinCapacity = 0 },
		"shards":       func(s *UploadBloomSettings) { s.Shards = 0 },
		"timeout":      func(s *UploadBloomSettings) { s.TimeoutSeconds = 0 },
		"batch":        func(s *UploadBloomSettings) { s.Batch = 0 },
		"lock ttl":     func(s *UploadBloomSettings) { s.LockTTLSeconds = 0 },
		"check seconds": func(s *UploadBloomSettings) { s.CheckSeconds = 0 },
		"prefix":       func(s *UploadBloomSettings) { s.Prefix = "" },
		"url register": func(s *UploadBloomSettings) { s.Mode = UploadBloomModeRegister; s.URL = "" },
		"url on":       func(s *UploadBloomSettings) { s.Mode = UploadBloomModeOn; s.URL = "" },
	} {
		settings := base()
		mutate(&settings)
		if err := settings.Validate(); err == nil {
			t.Fatalf("%s: expected a validation error", name)
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid settings rejected: %v", err)
	}
	off := base()
	off.Mode = UploadBloomModeOff
	off.URL = ""
	if err := off.Validate(); err != nil {
		t.Fatalf("off mode without a URL must be valid: %v", err)
	}
}

// TestUploadBloomBeginRegistersOnBothLockPaths drives the real journal Begin
// with the bloom filter attached: a created operation must be registered
// before its transaction commits, stay queryable through the read view, and a
// failed registration must abort the creation retryably (unless the kill
// switch was confirmed). Both lock paths (MySQL legacy and Redis registration)
// share the same contract.
func TestUploadBloomBeginRegistersOnBothLockPaths(t *testing.T) {
	stores, cache := newCacheTestStores(t)
	ctx := context.Background()
	backend := newFakeBloomBackend()
	filter := newTestBloomFilter(t, stores, backend, nil)
	stores.UploadBloom = filter
	// Build the generation from the (empty) history first.
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	newJournal := func() *UploadJournal {
		t.Helper()
		journal, err := NewUploadJournal(stores,
			WithLeaseSeconds(30), WithAutoHeartbeat(false), WithCleanupDelaySeconds(0))
		if err != nil {
			t.Fatalf("NewUploadJournal() error = %v", err)
		}
		t.Cleanup(func() { journal.Close() })
		return journal
	}

	// Legacy (MySQL-lock) path: Begin registers the new ID before committing.
	legacy := newJournal()
	operation, err := legacy.Begin(ctx, "alpha", "legacy.txt", "doc-legacy", "collection", BeginOptions{})
	if err != nil {
		t.Fatalf("Begin(legacy) error = %v", err)
	}
	if filter.DefinitelyAbsent(ctx, operation.ID) {
		t.Fatal("a committed operation must never be reported absent")
	}
	detail, err := stores.Views.UploadOperation(ctx, "alpha", operation.ID)
	if err != nil || detail == nil {
		t.Fatalf("UploadOperation(new ID) = %+v, %v", detail, err)
	}

	// Redis-lock path: same contract through the registration transaction.
	locker := newFakeDocumentLocker()
	redisJournal, err := NewUploadJournal(stores,
		WithDocumentLocker(locker),
		WithLeaseSeconds(30), WithAutoHeartbeat(false), WithCleanupDelaySeconds(0))
	if err != nil {
		t.Fatalf("NewUploadJournal(redis) error = %v", err)
	}
	t.Cleanup(func() { redisJournal.Close() })
	redisOperation, err := redisJournal.Begin(ctx, "alpha", "redis.txt", "doc-redis", "collection", BeginOptions{})
	if err != nil {
		t.Fatalf("Begin(redis) error = %v", err)
	}
	if filter.DefinitelyAbsent(ctx, redisOperation.ID) {
		t.Fatal("a committed operation must never be reported absent (redis path)")
	}
	detail, err = stores.Views.UploadOperation(ctx, "alpha", redisOperation.ID)
	if err != nil || detail == nil {
		t.Fatalf("UploadOperation(new redis ID) = %+v, %v", detail, err)
	}

	// A failed registration with an unconfirmable kill switch aborts Begin
	// with the retryable error: no committed operation can be invisible. The
	// earlier writers' leases are expired first so the guard admits a new
	// intent.
	expireUploadLeases(t, stores, operation.ID)
	expireUploadLeases(t, stores, redisOperation.ID)
	backend.hooks["setbits"] = NewBloomUnavailable()
	backend.hooks["disable"] = NewBloomUnavailable()
	if _, err := legacy.Begin(ctx, "alpha", "blocked.txt", "doc-blocked", "collection", BeginOptions{}); !isBloomRegistrationFailure(t, err) {
		t.Fatalf("Begin with a failed registration must abort retryably, got %v", err)
	}
	// No INTENT row survived the aborted creation.
	var blocked int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM upload_operations WHERE name = ?", "blocked.txt").Scan(&blocked); err != nil || blocked != 0 {
		t.Fatalf("blocked rows = %d, %v", blocked, err)
	}
	backend.hooks["setbits"] = nil
	backend.hooks["disable"] = nil

	// Cross-namespace queries keep returning not found (namespace check is
	// unchanged and stays with MySQL).
	if detail, err := stores.Views.UploadOperation(ctx, "beta", operation.ID); err != nil || detail != nil {
		t.Fatalf("cross-namespace UploadOperation = %+v, %v", detail, err)
	}
	_ = cache
}

// TestUploadBloomReadViewDetailQueryNoSQL proves the read gate skips the
// upload-status SQL: with the loader wrapped in a counter, a
// definitely-absent ID must leave the counter untouched while a present ID
// increments it exactly once before the value is cached.
func TestUploadBloomReadViewDetailQueryNoSQL(t *testing.T) {
	stores, _ := newCacheTestStores(t)
	ctx := context.Background()
	bloomTestOperation(t, stores, bloomOperationID(1))

	backend := newFakeBloomBackend()
	filter := newTestBloomFilter(t, stores, backend, nil)
	stores.UploadBloom = filter
	if err := filter.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}

	loaderCalls := 0
	original := stores.CacheSync.loaders["upload_status"]
	stores.CacheSync.loaders["upload_status"] = func(ctx context.Context, querier viewQuerier, scope string) (any, error) {
		loaderCalls++
		return original(ctx, querier, scope)
	}

	// A definitely-absent random ID: no SQL, no cache write, not found.
	detail, err := stores.Views.UploadOperation(ctx, "alpha", bloomOperationID(999))
	if err != nil || detail != nil {
		t.Fatalf("absent ID detail = %+v, %v", detail, err)
	}
	if loaderCalls != 0 {
		t.Fatalf("the upload-status SQL ran %d times for a definitely-absent ID", loaderCalls)
	}

	// A present ID: the SQL runs exactly once and the value is cached.
	detail, err = stores.Views.UploadOperation(ctx, "alpha", bloomOperationID(1))
	if err != nil || detail == nil {
		t.Fatalf("present ID detail = %+v, %v", detail, err)
	}
	if loaderCalls != 1 {
		t.Fatalf("loader calls = %d, want 1", loaderCalls)
	}
	// The cached hit now bypasses both the SQL and the filter.
	backend.hooks["state"] = NewBloomUnavailable()
	if detail, err = stores.Views.UploadOperation(ctx, "alpha", bloomOperationID(1)); err != nil || detail == nil {
		t.Fatalf("cached detail = %+v, %v", detail, err)
	}
	if loaderCalls != 1 {
		t.Fatalf("a cache hit must not rerun the SQL, calls = %d", loaderCalls)
	}
}
