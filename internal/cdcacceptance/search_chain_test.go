package cdcacceptance

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// TestSearchChainSubmitWithoutBrokerCompletes covers the queued-search happy
// path: Submit commits to MySQL without any broker acknowledgement, the raw
// Debezium insert drives the worker, and Complete writes the MySQL fact snapshot
// and the succeeded terminal state in one transaction.
func TestSearchChainSubmitWithoutBrokerCompletes(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-search-ok")
	transient := newFakeTransient()
	stores.Results.Transient = transient
	searcher := &fakeSearcher{}
	worker := newSearchProcessor(t, stores, searcher)

	notification, err := stores.SearchTasks.Submit(ctx, namespace,
		map[string]any{"query": "E03 供水不足", "top_k": 5}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	if notification.Schema != state.SearchEnvelopeSchema || notification.ScheduleVersion != 1 ||
		notification.Namespace != namespace || len(notification.ResultID) != 36 {
		t.Fatalf("Submit notification = %+v", notification)
	}
	// Submission is accepted purely by MySQL: the task is queued and no broker
	// confirmation or publisher exists in the state layer.
	requireSearchTask(t, stores, notification.ResultID, state.SearchTaskQueued, "queued", 1)

	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 1, "c")); !acked {
		t.Fatal("initial search delivery was not acknowledged")
	}
	requireSearchTask(t, stores, notification.ResultID, state.SearchTaskSucceeded, "completed", 1)
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d, want 1", searcher.callCount())
	}
	if request := searcher.lastRequest(); request.Query != "E03 供水不足" || request.TopK != 5 {
		t.Fatalf("searched request = %+v", request)
	}
	// MySQL is the fact source: the snapshot row exists even though the Redis
	// read cache could be lost at any time.
	if countRows(t, stores,
		"SELECT COUNT(*) FROM search_result_snapshots WHERE id = ? AND namespace = ?",
		notification.ResultID, namespace) != 1 {
		t.Fatal("completed search has no MySQL result snapshot")
	}
	page, err := stores.Results.Get(namespace, notification.ResultID, 1, 10)
	if err != nil {
		t.Fatalf("Results.Get() error = %v", err)
	}
	results, ok := page["results"].([]any)
	if !ok || len(results) != 1 || page["total_results"] != 1 {
		t.Fatalf("result page = %+v", page)
	}
	first := results[0].(map[string]any)
	if text, _ := first["text"].(string); !strings.Contains(text, "E03 供水不足") {
		t.Fatalf("result text = %v", first["text"])
	}
	if !transient.has("search-result", notification.ResultID) {
		t.Fatal("Complete did not write the Redis result cache")
	}
	// Duplicate delivery after the commit is ACKed without re-running retrieval.
	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 1, "r")); !acked {
		t.Fatal("duplicate search delivery was not acknowledged")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("duplicate delivery re-ran retrieval: calls = %d", searcher.callCount())
	}
	if countRows(t, stores, "SELECT COUNT(*) FROM search_result_snapshots WHERE id = ?",
		notification.ResultID) != 1 {
		t.Fatal("duplicate delivery duplicated the result snapshot")
	}
	// The application-native schema-3 envelope is also ACKed directly by Handle
	// for the succeeded task.
	direct := state.SearchNotification{
		Schema: state.SearchEnvelopeSchema, ResultID: notification.ResultID,
		Namespace: namespace, ScheduleVersion: 1,
	}
	if direct.Schema != 3 {
		t.Fatalf("SearchEnvelopeSchema = %d, want 3", direct.Schema)
	}
	if acked := worker.Handle(ctx, direct); !acked {
		t.Fatal("terminal native search envelope was not acknowledged")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("direct terminal delivery re-ran retrieval: calls = %d", searcher.callCount())
	}
}

// TestSearchChainResultSurvivesRedisLossAndPoison covers the "Redis 丢失/污染仍可
// 读" row: MySQL rebuilds the page and repairs the cache, and a corrupt or
// cross-tenant cache entry can never replace the authoritative fact row.
func TestSearchChainResultSurvivesRedisLossAndPoison(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-search-loss")
	transient := newFakeTransient()
	stores.Results.Transient = transient
	worker := newSearchProcessor(t, stores, &fakeSearcher{})
	notification, err := stores.SearchTasks.Submit(ctx, namespace, map[string]any{"query": "durable"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 1, "c")); !acked {
		t.Fatal("search delivery was not acknowledged")
	}

	// Total Redis data loss: the fact row still returns the result and the read
	// path backfills the cache.
	transient.clear()
	page, err := stores.Results.Get(namespace, notification.ResultID, 1, 10)
	if err != nil || page == nil {
		t.Fatalf("Results.Get(after cache loss) = %+v, %v", page, err)
	}
	if !transient.has("search-result", notification.ResultID) {
		t.Fatal("cache loss was not repaired by the MySQL read-through")
	}
	// Corrupt values are discarded in favour of the fact row.
	for index, corrupt := range []any{
		[]byte("not json"),
		map[string]any{},
		map[string]any{"namespace": "other", "expires_at": 99999999999.0, "payload": nil},
		[]any{},
	} {
		transient.poison("search-result", notification.ResultID, corrupt)
		page, err := stores.Results.Get(namespace, notification.ResultID, 1, 10)
		if err != nil {
			t.Fatalf("corrupt cache entry %d was not recovered: %v", index, err)
		}
		if results := page["results"].([]any); len(results) != 1 {
			t.Fatalf("corrupt cache entry %d page = %+v", index, page)
		}
	}
	// A foreign tenant still cannot read the result.
	var unavailable *state.SnapshotUnavailable
	if _, err := stores.Results.Get("other", notification.ResultID, 1, 10); !errors.As(err, &unavailable) {
		t.Fatalf("cross-namespace Get() error = %v", err)
	}
}

// TestSearchChainDeadlineConvergesTimedOut covers expiry: Get/Expire/Claim all
// converge an overdue active task to timed_out, the result page is unavailable,
// and the stale delivery is ACKed as a safe skip.
func TestSearchChainDeadlineConvergesTimedOut(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-search-timeout")
	stores.Results.Transient = newFakeTransient()
	searcher := &fakeSearcher{}
	worker := newSearchProcessor(t, stores, searcher)
	notification, err := stores.SearchTasks.Submit(ctx, namespace, map[string]any{"query": "slow"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	second, err := stores.SearchTasks.Submit(ctx, namespace, map[string]any{"query": "also slow"}, nil)
	if err != nil {
		t.Fatalf("second Submit() error = %v", err)
	}
	// Jump past the task deadline (default 120s) and the result TTL.
	stores.SearchTasks.Clock = func() float64 { return float64(time.Now().Unix()) + 7200 }

	var unavailable *state.SnapshotUnavailable
	if _, err := stores.SearchTasks.Get(ctx, namespace, notification.ResultID, 1, 10); !errors.As(err, &unavailable) {
		t.Fatalf("Get(expired) error = %v, want the timed-out contract", err)
	}
	requireSearchTask(t, stores, notification.ResultID, state.SearchTaskTimedOut, state.SearchTaskFailed, 1)
	if countRows(t, stores, "SELECT COUNT(*) FROM search_result_snapshots WHERE id = ?",
		notification.ResultID) != 0 {
		t.Fatal("timed-out search wrote a result snapshot")
	}
	if _, err := stores.Results.Get(namespace, notification.ResultID, 1, 10); !errors.As(err, &unavailable) {
		t.Fatalf("Results.Get(timed out) error = %v", err)
	}
	// The queued notification that arrives after the deadline is a safe skip.
	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 1, "c")); !acked {
		t.Fatal("expired delivery was not acknowledged")
	}
	if searcher.callCount() != 0 {
		t.Fatalf("expired task was executed: calls = %d", searcher.callCount())
	}

	// The periodic Expire pass converges active-but-overdue rows as well.
	expired, err := stores.SearchTasks.Expire(ctx, 10)
	if err != nil || expired != 1 {
		t.Fatalf("Expire() = %d, %v; want 1", expired, err)
	}
	requireSearchTask(t, stores, second.ResultID, state.SearchTaskTimedOut, state.SearchTaskFailed, 1)
}

// TestSearchChainRetryableFailureBumpsScheduleVersion covers the retry row: an
// infrastructure failure is recorded as a durable requeue with a new generation,
// the stale generation is ACKed without execution, and the new one completes.
func TestSearchChainRetryableFailureBumpsScheduleVersion(t *testing.T) {
	stores := newStores(t)
	ctx := context.Background()
	namespace := uniqueNamespace(t, "acc-search-retry")
	stores.Results.Transient = newFakeTransient()
	searcher := &fakeSearcher{failures: []error{errors.New("qdrant unavailable: connection reset")}}
	worker := newSearchProcessor(t, stores, searcher)
	notification, err := stores.SearchTasks.Submit(ctx, namespace, map[string]any{"query": "retry"}, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 1, "c")); !acked {
		t.Fatal("retryable failure was not acknowledged")
	}
	requireSearchTask(t, stores, notification.ResultID, state.SearchTaskQueued, "retry_wait", 2)
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d, want 1", searcher.callCount())
	}
	// Stale generation: acknowledged, never executed.
	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 1, "u")); !acked {
		t.Fatal("stale generation was not acknowledged")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("stale generation re-ran retrieval: calls = %d", searcher.callCount())
	}
	// The schedule_version bump notification completes the retry.
	if acked := deliver(t, worker.MessageHandler(), "praxis_search_tasks",
		rawSearchEvent(t, notification.ResultID, namespace, 2, "u")); !acked {
		t.Fatal("retry generation was not acknowledged")
	}
	requireSearchTask(t, stores, notification.ResultID, state.SearchTaskSucceeded, "completed", 2)
	if searcher.callCount() != 2 {
		t.Fatalf("searcher calls = %d, want 2", searcher.callCount())
	}
}
