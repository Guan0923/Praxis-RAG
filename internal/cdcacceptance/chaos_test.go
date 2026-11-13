package cdcacceptance

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestChaosPausedConnectorsResumeWithEveryTaskProcessed covers the
// Debezium-pause/resume row for both task topics: submissions while the
// connector is paused stay in MySQL, the document sweep re-emits a new
// generation, and every unfinished task is processed exactly once after resume.
func TestChaosPausedConnectorsResumeWithEveryTaskProcessed(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-chaos-pause")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	ingester := &journalIngester{stores: stores}
	documentWorker := newDocumentProcessor(t, stores, ingester)
	searcher := &fakeSearcher{}
	searchWorker := newSearchProcessor(t, stores, searcher)
	stores.Results.Transient = newFakeTransient()

	// The connectors are paused: only MySQL writes happen.
	firstDoc, err := stores.DocumentJobs.Submit(ctx, namespace, "paused-a.txt", []byte("a"), nil)
	if err != nil {
		t.Fatalf("Submit(paused-a) error = %v", err)
	}
	secondDoc, err := stores.DocumentJobs.Submit(ctx, namespace, "paused-b.txt", []byte("b"), nil)
	if err != nil {
		t.Fatalf("Submit(paused-b) error = %v", err)
	}
	search, err := stores.SearchTasks.Submit(ctx, namespace, map[string]any{"query": "paused"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	if ingester.callCount() != 0 || searcher.callCount() != 0 {
		t.Fatal("paused connectors still executed work")
	}
	// Document recovery while paused: the sweep emits new generations.
	stores.DocumentJobs.Settings.SweepAfterSeconds = -1
	swept, err := stores.DocumentJobs.Sweep(ctx, 10)
	if err != nil || swept != 2 {
		t.Fatalf("Sweep(paused) = %d, %v; want 2", swept, err)
	}
	requireDocumentJob(t, stores, firstDoc.ArtifactID, state.DocumentJobQueued, "retry_wait", 2)
	requireDocumentJob(t, stores, secondDoc.ArtifactID, state.DocumentJobQueued, "retry_wait", 2)

	// Resume: the connector publishes the backlog. The pre-sweep generation is
	// stale; the replacement generation is processed.
	for _, artifactID := range []string{firstDoc.ArtifactID, secondDoc.ArtifactID} {
		if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
			rawDocumentEvent(t, artifactID, namespace, 1, "c")); !acked {
			t.Fatalf("stale delivery for %s was not acknowledged", artifactID)
		}
		if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
			rawDocumentEvent(t, artifactID, namespace, 2, "u")); !acked {
			t.Fatalf("resumed delivery for %s was not acknowledged", artifactID)
		}
		requireDocumentJob(t, stores, artifactID, state.DocumentJobSucceeded, "completed", 2)
	}
	if ingester.callCount() != 2 {
		t.Fatalf("ingester calls = %d, want 2 (one per document)", ingester.callCount())
	}
	// The search backlog is delivered from its initial insert event.
	if acked := deliver(t, searchWorker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, search.ResultID, namespace, 1, "c")); !acked {
		t.Fatal("search backlog delivery was not acknowledged")
	}
	requireSearchTask(t, stores, search.ResultID, state.SearchTaskSucceeded, "completed", 1)
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d, want 1", searcher.callCount())
	}
}

// TestChaosCrashAndDuplicateDeliveryConverge covers the worker-crash and
// duplicate-delivery rows: every duplicate is ACKed, a crashed lease is
// recovered by sweep (documents) or by re-claiming (search), and terminal state
// is recorded exactly once.
func TestChaosCrashAndDuplicateDeliveryConverge(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-chaos-crash")
	createKnowledgeBase(t, stores, namespace, "acceptance-collection")
	ingester := &journalIngester{stores: stores}
	documentWorker := newDocumentProcessor(t, stores, ingester)
	searcher := &fakeSearcher{}
	searchWorker := newSearchProcessor(t, stores, searcher)
	stores.Results.Transient = newFakeTransient()

	// --- Document: crash after claim, recovered by the sweep.
	docBase := float64(time.Now().Unix())
	docOffset := 0.0
	stores.DocumentJobs.Clock = func() float64 { return docBase + docOffset }
	crashed, err := stores.DocumentJobs.Submit(ctx, namespace, "crashed.txt", []byte("crashed"), nil)
	if err != nil {
		t.Fatalf("Submit(crashed) error = %v", err)
	}
	crashedNotification := state.DocumentNotification{
		Schema: state.DocumentEnvelopeSchema, ArtifactID: crashed.ArtifactID,
		Namespace: namespace, ScheduleVersion: 1,
	}
	task, err := stores.DocumentJobs.Claim(ctx, crashedNotification)
	if err != nil || task == nil {
		t.Fatalf("Claim(crashed) = %+v, %v", task, err)
	}
	docOffset = task.LeaseUntil - docBase + 1
	if swept, err := stores.DocumentJobs.Sweep(ctx, 10); err != nil || swept != 1 {
		t.Fatalf("Sweep(crashed) = %d, %v; want 1", swept, err)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
			rawDocumentEvent(t, crashed.ArtifactID, namespace, 1, "c")); !acked {
			t.Fatalf("crashed duplicate %d was not acknowledged", attempt)
		}
	}
	if ingester.callCount() != 0 {
		t.Fatalf("crashed generation executed: calls = %d", ingester.callCount())
	}
	if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, crashed.ArtifactID, namespace, 2, "u")); !acked {
		t.Fatal("recovered document generation was not acknowledged")
	}
	deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, crashed.ArtifactID, namespace, 2, "r"))
	requireDocumentJob(t, stores, crashed.ArtifactID, state.DocumentJobSucceeded, "completed", 2)
	if ingester.callCount() != 1 {
		t.Fatalf("document ingester calls = %d, want 1", ingester.callCount())
	}

	// --- Search: crash after claim is recovered by re-claiming on redelivery.
	// A short lease lets it expire before the task deadline (production default
	// lease is longer than the search timeout, so a crash converges timed_out).
	stores.SearchTasks.Settings.LeaseSeconds = 5
	search, err := stores.SearchTasks.Submit(ctx, namespace, map[string]any{"query": "crash"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	searchNotification := state.SearchNotification{
		Schema: state.SearchEnvelopeSchema, ResultID: search.ResultID,
		Namespace: namespace, ScheduleVersion: 1,
	}
	claimed, err := stores.SearchTasks.Claim(ctx, searchNotification)
	if err != nil || claimed == nil {
		t.Fatalf("SearchTasks.Claim(crash) = %+v, %v", claimed, err)
	}
	// The worker dies; the lease expires, and the redelivery re-claims.
	stores.SearchTasks.Clock = func() float64 { return float64(time.Now().Unix()) + 31 }
	if acked := deliver(t, searchWorker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, search.ResultID, namespace, 1, "c")); !acked {
		t.Fatal("search redelivery after crash was not acknowledged")
	}
	requireSearchTask(t, stores, search.ResultID, state.SearchTaskSucceeded, "completed", 1)
	// Duplicate terminal deliveries are ACKed and never re-run retrieval.
	for attempt := 0; attempt < 2; attempt++ {
		if acked := deliver(t, searchWorker.MessageHandler(), "praxis_search_tasks",
			rawSearchEvent(t, search.ResultID, namespace, 1, "r")); !acked {
			t.Fatalf("terminal duplicate %d was not acknowledged", attempt)
		}
	}
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d, want 1", searcher.callCount())
	}
	if countRows(t, stores, "SELECT COUNT(*) FROM search_result_snapshots WHERE id = ?",
		search.ResultID) != 1 {
		t.Fatal("crash recovery wrote more than one result snapshot")
	}

	// --- Document: a retryable failure + duplicate stale generations ACK.
	retryDoc, err := stores.DocumentJobs.Submit(ctx, namespace, "retry-dupes.txt", []byte("r"), nil)
	if err != nil {
		t.Fatalf("Submit(retry-dupes) error = %v", err)
	}
	ingester.mu.Lock()
	ingester.failures = append(ingester.failures, errors.New("embedding service unavailable"))
	ingester.mu.Unlock()
	if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, retryDoc.ArtifactID, namespace, 1, "c")); !acked {
		t.Fatal("retryable document failure was not acknowledged")
	}
	requireDocumentJob(t, stores, retryDoc.ArtifactID, state.DocumentJobQueued, "retry_wait", 2)
	for attempt := 0; attempt < 2; attempt++ {
		if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
			rawDocumentEvent(t, retryDoc.ArtifactID, namespace, 1, "u")); !acked {
			t.Fatalf("stale generation duplicate %d was not acknowledged", attempt)
		}
	}
	if acked := deliver(t, documentWorker.MessageHandler(), "praxis_document_ingest",
		rawDocumentEvent(t, retryDoc.ArtifactID, namespace, 2, "u")); !acked {
		t.Fatal("retry generation was not acknowledged")
	}
	requireDocumentJob(t, stores, retryDoc.ArtifactID, state.DocumentJobSucceeded, "completed", 2)
}

// TestChaosRedisFailuresDoNotAffectTaskTerminalStates covers the Redis-outage
// row for both consumers: a failed search-result cache write does not fail the
// search, and a failed cache-view publish is recorded on the watermark and
// recovered by the dirty-key sweep.
func TestChaosRedisFailuresDoNotAffectTaskTerminalStates(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()

	// --- Search: the result cache write fails, the task still succeeds.
	searchNamespace := uniqueNamespace(t, "acc-chaos-search")
	transient := newFakeTransient()
	stores.Results.Transient = transient
	searcher := &fakeSearcher{}
	searchWorker := newSearchProcessor(t, stores, searcher)
	search, err := stores.SearchTasks.Submit(ctx, searchNamespace, map[string]any{"query": "cache outage"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	transient.failNextPuts(1)
	if acked := deliver(t, searchWorker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, search.ResultID, searchNamespace, 1, "c")); !acked {
		t.Fatal("search with a cache write failure was not acknowledged")
	}
	requireSearchTask(t, stores, search.ResultID, state.SearchTaskSucceeded, "completed", 1)
	if !transient.has("search-result", search.ResultID) {
		// The failed Put was only the first attempt; the completed task's cache
		// may legitimately be missing, so verify the MySQL fallback read.
		page, err := stores.Results.Get(searchNamespace, search.ResultID, 1, 10)
		if err != nil || page == nil {
			t.Fatalf("Results.Get(after cache write failure) = %+v, %v", page, err)
		}
	}

	// --- Cache: the view publish fails, the change is ACKed and recovered.
	cacheNamespace := uniqueNamespace(t, "acc-chaos-cache")
	backend := newFakeStateCache()
	stores.CacheSync.Backend = backend
	cacheProcessor := newCacheProcessor(t, stores)
	createKnowledgeBase(t, stores, cacheNamespace, "acceptance-collection")
	if err := stores.CacheSync.RefreshDirty(ctx, 1000); err != nil {
		t.Fatalf("RefreshDirty(baseline) error = %v", err)
	}
	insertDocumentSQL(t, stores, cacheNamespace, "outage.txt", "doc-outage", "sha-outage")
	backend.failNextPuts(1)
	if acked := deliver(t, cacheProcessor.MessageHandler(), "praxis_cache_change",
		rawCacheEvent(t, "documents", "c", nil,
			documentLocators(cacheNamespace, "outage.txt", "doc-outage"))); !acked {
		t.Fatal("cache change with a Redis write failure was not acknowledged")
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 100); err != nil || len(dirty) == 0 {
		t.Fatalf("DirtyKeys() after outage = %+v, %v; want a compensation entry", dirty, err)
	}
	if err := stores.CacheSync.RefreshDirty(ctx, 1000); err != nil {
		t.Fatalf("RefreshDirty(recovery) error = %v", err)
	}
	if dirty, err := stores.CacheSync.DirtyKeys(ctx, 100); err != nil || len(dirty) != 0 {
		t.Fatalf("DirtyKeys() after recovery = %+v, %v; want none", dirty, err)
	}
	requireDocumentNames(t, backend, cacheNamespace, "outage.txt")
}
