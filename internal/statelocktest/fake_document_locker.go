// Package statelocktest provides the deterministic in-memory stand-in for the
// Redis document-lock backend, shared by every package (outside internal/state
// itself) whose tests exercise document writes. It reproduces the observable
// contract (atomic multi-resource acquire, owner-checked renew/release, TTL
// expiry) and adds controlled hooks so tests can pause a request between
// acquiring the lease and registering it in MySQL without relying on sleeps.
package statelocktest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// fakeLease is one in-memory lease held by FakeDocumentLocker.
type fakeLease struct {
	owner  string
	expiry time.Time
}

// FakeDocumentLocker implements state.DocumentLocker.
type FakeDocumentLocker struct {
	mutex       sync.Mutex
	fields      map[string]map[string]fakeLease
	unavailable bool
	releaseErr  error
	acquireHook func()
	acquires    int
}

// NewFakeDocumentLocker returns an empty in-memory lease backend.
func NewFakeDocumentLocker() *FakeDocumentLocker {
	return &FakeDocumentLocker{fields: map[string]map[string]fakeLease{}}
}

// OwnerDigest mirrors the Redis backend and the lease read model: the first 16
// lowercase hex characters of sha256(owner).
func OwnerDigest(owner string) string {
	sum := sha256.Sum256([]byte(owner))
	return hex.EncodeToString(sum[:])[:16]
}

func containsResource(resources []string, resource string) bool {
	for _, candidate := range resources {
		if candidate == resource {
			return true
		}
	}
	return false
}

func (f *FakeDocumentLocker) prune(namespace string, now time.Time) map[string]fakeLease {
	store := f.fields[namespace]
	if store == nil {
		store = map[string]fakeLease{}
		f.fields[namespace] = store
	}
	for resource, lease := range store {
		if !lease.expiry.After(now) {
			delete(store, resource)
		}
	}
	return store
}

// Acquire implements state.DocumentLocker.
func (f *FakeDocumentLocker) Acquire(_ context.Context, namespace string, resources []string, owner string, ttl time.Duration) error {
	f.mutex.Lock()
	if f.unavailable {
		f.mutex.Unlock()
		return state.NewLockUnavailable()
	}
	now := time.Now()
	store := f.prune(namespace, now)
	resources = dedupeResources(resources)
	exclusive := containsResource(resources, state.UploadNamespaceResource)
	busy := false
	for resource, lease := range store {
		if lease.owner == owner {
			continue
		}
		if exclusive || resource == state.UploadNamespaceResource || containsResource(resources, resource) {
			busy = true
			break
		}
	}
	if busy {
		f.mutex.Unlock()
		return state.NewLockBusy()
	}
	for _, resource := range resources {
		store[resource] = fakeLease{owner: owner, expiry: now.Add(ttl)}
	}
	f.acquires++
	hook := f.acquireHook
	f.mutex.Unlock()
	if hook != nil {
		hook()
	}
	return nil
}

// Renew implements state.DocumentLocker.
func (f *FakeDocumentLocker) Renew(_ context.Context, namespace string, resources []string, owner string, ttl time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return state.NewLockUnavailable()
	}
	now := time.Now()
	store := f.prune(namespace, now)
	resources = dedupeResources(resources)
	if len(resources) == 0 {
		return &state.LeaseLost{}
	}
	for _, resource := range resources {
		lease, ok := store[resource]
		if !ok || lease.owner != owner {
			return &state.LeaseLost{}
		}
	}
	for _, resource := range resources {
		store[resource] = fakeLease{owner: owner, expiry: now.Add(ttl)}
	}
	return nil
}

// Release implements state.DocumentLocker.
func (f *FakeDocumentLocker) Release(_ context.Context, namespace string, resources []string, owner string) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return state.NewLockUnavailable()
	}
	if f.releaseErr != nil {
		return f.releaseErr
	}
	store := f.fields[namespace]
	for _, resource := range dedupeResources(resources) {
		if lease, ok := store[resource]; ok && lease.owner == owner {
			delete(store, resource)
		}
	}
	return nil
}

// Inspect implements state.DocumentLocker.
func (f *FakeDocumentLocker) Inspect(_ context.Context, namespace, resource string) (state.LockState, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return state.LockState{}, state.NewLockUnavailable()
	}
	now := time.Now()
	store := f.prune(namespace, now)
	lease, ok := store[resource]
	if !ok {
		return state.LockState{}, nil
	}
	return state.LockState{OwnerDigest: OwnerDigest(lease.owner), TTL: lease.expiry.Sub(now)}, nil
}

// Ping implements state.DocumentLocker.
func (f *FakeDocumentLocker) Ping(context.Context) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.unavailable {
		return state.NewLockUnavailable()
	}
	return nil
}

// Close implements state.DocumentLocker.
func (f *FakeDocumentLocker) Close() error { return nil }

// ExpireAll simulates a Redis restart or a lease TTL elapsing.
func (f *FakeDocumentLocker) ExpireAll() {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.fields = map[string]map[string]fakeLease{}
}

// SetUnavailable makes every backend call report *state.LockUnavailable.
func (f *FakeDocumentLocker) SetUnavailable(unavailable bool) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.unavailable = unavailable
}

// SetReleaseError makes Release fail (post-commit release failure).
func (f *FakeDocumentLocker) SetReleaseError(err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.releaseErr = err
}

// SetAcquireHook installs a hook that runs after a successful acquire, before
// the journal starts its MySQL registration.
func (f *FakeDocumentLocker) SetAcquireHook(hook func()) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.acquireHook = hook
}

// HeldOwner returns the owner of one live resource ("" when free).
func (f *FakeDocumentLocker) HeldOwner(namespace, resource string) string {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.prune(namespace, time.Now())
	return f.fields[namespace][resource].owner
}

// AcquireCount reports how many successful acquisitions the backend served.
func (f *FakeDocumentLocker) AcquireCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.acquires
}

// dedupeResources keeps the first occurrence order and removes duplicates.
func dedupeResources(resources []string) []string {
	seen := make(map[string]struct{}, len(resources))
	cleaned := make([]string, 0, len(resources))
	for _, resource := range resources {
		if _, present := seen[resource]; present {
			continue
		}
		seen[resource] = struct{}{}
		cleaned = append(cleaned, resource)
	}
	return cleaned
}

// Compile-time interface check.
var _ state.DocumentLocker = (*FakeDocumentLocker)(nil)
