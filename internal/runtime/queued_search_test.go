package runtime

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// newQueuedSearchService wires a runtime with a committed knowledge-base row and
// an in-memory result cache. Acceptance is a MySQL commit now, so no broker or
// publisher is involved.
func newQueuedSearchService(t *testing.T) (*Service, *state.Stores) {
	t.Helper()
	service, stores := newKBService(t, "http://127.0.0.1:1")
	stores.Results.Transient = newMemoryTransient()
	if err := service.StartBackground(context.Background(),
		WithMaintenanceInterval(0),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(service.StopBackground)
	return service, stores
}

func sampleSearchRequest() contracts.SearchRequest {
	return contracts.SearchRequest{
		Query:      "E03 供水不足",
		Retrieval:  contracts.RetrievalHybrid,
		Rerank:     contracts.RerankCrossEncoder,
		CandidateK: 10,
		TopK:       5,
		Filters:    map[string]any{"document_id": "doc-1"},
	}
}

// searchNotification reads the committed scheduling generation back so tests
// deliver the same identifier-only notification Debezium would.
func searchNotification(t *testing.T, stores *state.Stores, namespace, resultID string) state.SearchNotification {
	t.Helper()
	var version int64
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT schedule_version FROM search_tasks WHERE result_id = ?", resultID).Scan(&version); err != nil {
		t.Fatalf("read schedule_version: %v", err)
	}
	return state.SearchNotification{
		Schema:          state.SearchEnvelopeSchema,
		ResultID:        resultID,
		Namespace:       namespace,
		ScheduleVersion: version,
	}
}

// inlineSearchExecutor mirrors the worker's retrieval step for one accepted task.
func inlineSearchExecutor() func(context.Context, string, map[string]any) (map[string]any, error) {
	return func(_ context.Context, namespace string, arguments map[string]any) (map[string]any, error) {
		return map[string]any{
			"query":           arguments["query"],
			"evidence_status": "no_evidence",
			"answer_guidance": "The knowledge base is insufficient.",
			"results":         []any{},
			"debug":           map[string]any{"namespace": namespace},
		}, nil
	}
}

func TestSubmitSearchAcceptedWithoutBroker(t *testing.T) {
	service, stores := newQueuedSearchService(t)
	ctx := context.Background()

	resultID, err := service.submitSearchTask(ctx, "alpha", sampleSearchRequest())
	if err != nil {
		t.Fatalf("submitSearchTask() error = %v", err)
	}
	if !canonicalUUIDPattern.MatchString(resultID) {
		t.Fatalf("result_id = %q, want canonical UUID", resultID)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", resultID, 1, 10)
	if err != nil || view == nil {
		t.Fatalf("SearchTasks.Get() = %v, %v", view, err)
	}
	if view.Status != state.SearchTaskQueued {
		t.Fatalf("accepted task status = %q, want queued", view.Status)
	}
	var version int64
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT schedule_version FROM search_tasks WHERE result_id = ?", resultID).Scan(&version); err != nil {
		t.Fatalf("read schedule_version: %v", err)
	}
	if version != 1 {
		t.Fatalf("initial schedule_version = %d, want 1", version)
	}

	// wait_seconds=0 returns exactly the accepted result id.
	payload, err := service.submitSearch(ctx, "alpha", sampleSearchRequest(), 0)
	if err != nil {
		t.Fatalf("submitSearch() error = %v", err)
	}
	if len(payload) != 1 || payload["result_id"] == nil {
		t.Fatalf("admission payload = %v, want only result_id", payload)
	}
}

func TestSubmitSearchCompletesWithinWait(t *testing.T) {
	service, stores := newQueuedSearchService(t)
	ctx := context.Background()

	resultID, err := service.submitSearchTask(ctx, "alpha", sampleSearchRequest())
	if err != nil {
		t.Fatalf("submitSearchTask() error = %v", err)
	}
	notification := searchNotification(t, stores, "alpha", resultID)
	if err := service.executeQueuedSearchTaskOnce(ctx, notification, inlineSearchExecutor()); err != nil {
		t.Fatalf("executeQueuedSearchTaskOnce() error = %v", err)
	}

	payload, err := service.getSearchResultOrdered(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("getSearchResultOrdered() error = %v", err)
	}
	if payload["evidence_status"] != "no_evidence" {
		t.Fatalf("payload = %v", payload)
	}
	if payload["result_id"] != resultID || payload["snapshot_available"] != true {
		t.Errorf("stored result = %v", payload)
	}
	view, err := stores.SearchTasks.Get(ctx, "alpha", resultID, 1, 10)
	if err != nil || view == nil || view.Status != state.SearchTaskSucceeded {
		t.Fatalf("task view = %v, %v", view, err)
	}
}

func TestSubmitSearchWaitTimeoutReturnsOnlyResultID(t *testing.T) {
	service, stores := newQueuedSearchService(t)
	started := time.Now()
	payload, err := service.submitSearch(context.Background(), "alpha", sampleSearchRequest(), 0.2)
	if err != nil {
		t.Fatalf("submitSearch() error = %v", err)
	}
	if elapsed := time.Since(started); elapsed < 150*time.Millisecond {
		t.Errorf("wait returned after %s, want >= 150ms", elapsed)
	}
	if len(payload) != 1 {
		t.Fatalf("timeout payload = %v, want only result_id", payload)
	}
	resultID, _ := payload["result_id"].(string)
	read, err := service.getSearchResultOrdered(context.Background(), "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("getSearchResultOrdered() error = %v", err)
	}
	if read["status"] != "queued" || read["stage"] == "" || read["result_id"] != resultID {
		t.Fatalf("queued result = %v", read)
	}
	if view, err := stores.SearchTasks.Get(context.Background(), "alpha", resultID, 1, 10); err != nil ||
		view == nil || view.Status != state.SearchTaskQueued {
		t.Fatalf("task view = %v, %v", view, err)
	}
}

func TestSearchRetryBumpsScheduleVersion(t *testing.T) {
	service, stores := newQueuedSearchService(t)
	ctx := context.Background()
	resultID, err := service.submitSearchTask(ctx, "alpha", sampleSearchRequest())
	if err != nil {
		t.Fatalf("submitSearchTask() error = %v", err)
	}
	first := searchNotification(t, stores, "alpha", resultID)
	task, err := stores.SearchTasks.Claim(ctx, first)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %v, %v", task, err)
	}
	cause := "Search execution failed"
	if applied, err := stores.SearchTasks.Fail(ctx, task, true, &cause); err != nil || !applied {
		t.Fatalf("Fail(retry) = %v, %v", applied, err)
	}
	// The old generation can never be claimed again.
	if stale, err := stores.SearchTasks.Claim(ctx, first); err != nil || stale != nil {
		t.Fatalf("stale Claim() = %v, %v", stale, err)
	}
	second := searchNotification(t, stores, "alpha", resultID)
	if second.ScheduleVersion != first.ScheduleVersion+1 {
		t.Fatalf("schedule_version = %d, want %d", second.ScheduleVersion, first.ScheduleVersion+1)
	}
	fresh, err := stores.SearchTasks.Claim(ctx, second)
	if err != nil || fresh == nil {
		t.Fatalf("fresh Claim() = %v, %v", fresh, err)
	}
	if _, err := stores.SearchTasks.Complete(ctx, fresh, map[string]any{
		"query": "E03 供水不足", "evidence_status": "no_evidence",
		"answer_guidance": "The knowledge base is insufficient.",
		"results":         []any{}, "debug": map[string]any{},
	}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
}

func TestGetSearchResultOrderingAndErrors(t *testing.T) {
	service, stores := newQueuedSearchService(t)
	ctx := context.Background()
	// Unknown result ids fall through to the snapshot reader.
	unknown := "0f8fad5b-d9cb-469f-a165-70867728950e"
	if _, err := service.getSearchResultOrdered(ctx, "alpha", unknown, 1, 10); err == nil {
		t.Fatalf("unknown result id returned a payload")
	} else {
		var unavailable *apierrors.SnapshotUnavailable
		if !errors.As(err, &unavailable) {
			t.Fatalf("unknown result error = %v", err)
		}
	}
	// Invalid UUIDs are a snapshot-unavailable error.
	if _, err := service.getSearchResultOrdered(ctx, "alpha", "nope", 1, 10); err == nil {
		t.Fatalf("invalid result id returned a payload")
	}

	// MySQL is the fact source: dropping the Redis cache must not lose a
	// completed result.
	resultID, err := service.submitSearchTask(ctx, "alpha", sampleSearchRequest())
	if err != nil {
		t.Fatalf("submitSearchTask() error = %v", err)
	}
	notification := searchNotification(t, stores, "alpha", resultID)
	if err := service.executeQueuedSearchTaskOnce(ctx, notification, inlineSearchExecutor()); err != nil {
		t.Fatalf("executeQueuedSearchTaskOnce() error = %v", err)
	}
	stores.Results.Transient = nil
	read, err := service.getSearchResultOrdered(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("getSearchResultOrdered() after cache loss error = %v", err)
	}
	if read["result_id"] != resultID || read["snapshot_available"] != true {
		t.Fatalf("result after cache loss = %v", read)
	}
}

func TestSubmitSearchWaitSecondsValidation(t *testing.T) {
	service, _ := newQueuedSearchService(t)
	cases := []float64{-0.1, 30.1}
	for _, wait := range cases {
		_, err := service.submitSearch(context.Background(), "alpha", sampleSearchRequest(), wait)
		var validation *contracts.SearchValidationError
		if !errors.As(err, &validation) ||
			validation.Message != "wait_seconds must be a finite number between 0 and 30" {
			t.Fatalf("wait=%v error = %v", wait, err)
		}
	}
	nan := 0.0
	if nan == 0.0 {
		nan = nan / nan // NaN without importing math: 0/0
	}
	_, err := service.submitSearch(context.Background(), "alpha", sampleSearchRequest(), nan)
	var validation *contracts.SearchValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("NaN wait error = %v", err)
	}
}
