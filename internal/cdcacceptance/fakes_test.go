package cdcacceptance

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

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

// transientKey joins one cache kind/scope pair into a stable map key.
func transientKey(kind, scope string) string { return kind + "\x00" + scope }

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
