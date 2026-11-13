package cdcacceptance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// cloneJSONValue detaches a value through JSON, matching what a serializing
// Redis backend hands back on the next read.
func cloneJSONValue(value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var detached any
	if err := json.Unmarshal(encoded, &detached); err != nil {
		return nil, err
	}
	return detached, nil
}

// ---------------------------------------------------------------------------
// fakeTransient: the expiring search-result cache (Redis stand-in).

type fakeTransient struct {
	mu       sync.Mutex
	values   map[string]any
	failPuts int
	gets     int
	puts     int
}

func newFakeTransient() *fakeTransient {
	return &fakeTransient{values: map[string]any{}}
}

func transientKey(kind, identity string) string { return kind + "\x00" + identity }

func (f *fakeTransient) Get(kind, identity string) (any, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.gets++
	value, found := f.values[transientKey(kind, identity)]
	return value, found, nil
}

func (f *fakeTransient) Put(kind, identity string, value any, _ float64, nx bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.puts++
	if f.failPuts > 0 {
		f.failPuts--
		return false, errors.New("redis write failed")
	}
	key := transientKey(kind, identity)
	if nx {
		if _, exists := f.values[key]; exists {
			return false, nil
		}
	}
	detached, err := cloneJSONValue(value)
	if err != nil {
		return false, err
	}
	f.values[key] = detached
	return true, nil
}

func (f *fakeTransient) Delete(kind, identity string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	key := transientKey(kind, identity)
	if _, exists := f.values[key]; !exists {
		return 0, nil
	}
	delete(f.values, key)
	return 1, nil
}

// clear drops every cached result (a total Redis data loss).
func (f *fakeTransient) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values = map[string]any{}
}

// has reports whether one cached result is present.
func (f *fakeTransient) has(kind, identity string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, found := f.values[transientKey(kind, identity)]
	return found
}

// poison overwrites one cached value with an arbitrary (corrupt) payload.
func (f *fakeTransient) poison(kind, identity string, value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values[transientKey(kind, identity)] = value
}

// failNextPuts makes the next n Put calls fail with a Redis outage.
func (f *fakeTransient) failNextPuts(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPuts = count
}

func (f *fakeTransient) putCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.puts
}

// ---------------------------------------------------------------------------
// fakeStateCache: the read-model snapshot cache (Redis StateCache stand-in),
// including the revision watermark semantics of the production Lua script.

type fakeStateCache struct {
	mu        sync.Mutex
	enabled   bool
	values    map[string]any
	revisions map[string]int64
	failPuts  int
	puts      int
}

func newFakeStateCache() *fakeStateCache {
	return &fakeStateCache{
		enabled:   true,
		values:    map[string]any{},
		revisions: map[string]int64{},
	}
}

func (f *fakeStateCache) Enabled() bool { return f.enabled }

func (f *fakeStateCache) Get(_ context.Context, kind, scope string) (bool, any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled {
		return false, nil, nil
	}
	value, found := f.values[transientKey(kind, scope)]
	return found, value, nil
}

// Put stores a snapshot only when its revision is not older than the published
// one: the production backend rejects a stale write so a slow refresh can never
// overwrite a newer value.
func (f *fakeStateCache) Put(_ context.Context, kind, scope string, revision int64, value any, deleted bool) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.enabled {
		return false, nil
	}
	if f.failPuts > 0 {
		f.failPuts--
		return false, &state.CacheUnavailable{Message: "redis write failed"}
	}
	key := transientKey(kind, scope)
	if published, exists := f.revisions[key]; exists && revision < published {
		return false, nil
	}
	if deleted {
		delete(f.values, key)
	} else {
		detached, err := cloneJSONValue(value)
		if err != nil {
			return false, err
		}
		f.values[key] = detached
	}
	f.revisions[key] = revision
	f.puts++
	return true, nil
}

func (f *fakeStateCache) Close() error { return nil }

// value returns the cached snapshot for one kind/scope.
func (f *fakeStateCache) value(kind, scope string) any {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.values[transientKey(kind, scope)]
}

// revision returns the published watermark for one kind/scope.
func (f *fakeStateCache) revision(kind, scope string) (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	revision, found := f.revisions[transientKey(kind, scope)]
	return revision, found
}

// failNextPuts makes the next n snapshot writes fail with a Redis outage.
func (f *fakeStateCache) failNextPuts(count int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.failPuts = count
}

func (f *fakeStateCache) clear() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.values = map[string]any{}
	f.revisions = map[string]int64{}
}

// ---------------------------------------------------------------------------
// journalIngester: the runtime ingestion seam. Success mimics the real runtime
// publication: it commits an upload journal operation (the Reconcile proof) and
// upserts the document row. Failure injection returns the configured errors in
// order before any journal write.

type journalIngester struct {
	stores *state.Stores

	mu       sync.Mutex
	calls    int
	failures []error
}

func (i *journalIngester) ProcessDocumentJob(ctx context.Context, job documentworker.Payload) (map[string]any, error) {
	i.mu.Lock()
	i.calls++
	var injected error
	if len(i.failures) > 0 {
		injected = i.failures[0]
		i.failures = i.failures[1:]
	}
	i.mu.Unlock()
	if injected != nil {
		return nil, injected
	}
	if err := i.publish(ctx, job); err != nil {
		return nil, err
	}
	return map[string]any{"sha256": job.SHA256, "size": float64(job.Size)}, nil
}

// publish commits the journal operation and the document row exactly like the
// runtime's upload coordinator does at the end of a successful ingestion.
func (i *journalIngester) publish(ctx context.Context, job documentworker.Payload) error {
	documentID := "doc-" + job.ArtifactID[:8]
	collection := "acceptance-" + job.Namespace
	idempotencyKey := "document-task:" + job.ArtifactID + ":acceptance"
	operation, err := i.stores.Uploads.Begin(ctx, job.Namespace, job.Name, documentID, collection,
		state.BeginOptions{IdempotencyKey: &idempotencyKey})
	if err != nil {
		return fmt.Errorf("upload journal begin: %w", err)
	}
	transaction, err := i.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := i.stores.Documents.UpsertTx(ctx, transaction, job.Namespace, state.DocumentRecord{
		Name:        job.Name,
		DocumentID:  documentID,
		Collection:  collection,
		Size:        job.Size,
		SHA256:      job.SHA256,
		Chunking:    map[string]any{"strategy": "acceptance"},
		ModifiedAt:  "",
		IndexedAt:   "",
		Diagnostics: []any{},
	}); err != nil {
		return fmt.Errorf("document upsert: %w", err)
	}
	result := map[string]any{
		"sha256":                     job.SHA256,
		"size":                       float64(job.Size),
		"task_configuration_version": float64(job.ConfigurationVersion),
	}
	if _, err := i.stores.Uploads.Commit(ctx, operation, result, transaction); err != nil {
		return fmt.Errorf("upload journal commit: %w", err)
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	return nil
}

func (i *journalIngester) callCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.calls
}

// ---------------------------------------------------------------------------
// fakeSearcher: the retrieval seam.

type fakeSearcher struct {
	mu       sync.Mutex
	calls    int
	requests []contracts.SearchRequest
	failures []error
}

func (s *fakeSearcher) Search(_ context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	s.mu.Lock()
	s.calls++
	s.requests = append(s.requests, request)
	var injected error
	if len(s.failures) > 0 {
		injected = s.failures[0]
		s.failures = s.failures[1:]
	}
	s.mu.Unlock()
	if injected != nil {
		return nil, injected
	}
	return map[string]any{
		"results": []any{
			map[string]any{
				"rank":     1,
				"text":     "evidence for " + request.Query,
				"score":    0.93,
				"document": "guide.txt",
			},
		},
		"debug": map[string]any{"retrieval": "hybrid", "namespace": namespace},
	}, nil
}

func (s *fakeSearcher) callCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *fakeSearcher) lastRequest() contracts.SearchRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.requests) == 0 {
		return contracts.SearchRequest{}
	}
	return s.requests[len(s.requests)-1]
}
