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

// newQueuedSearchService wires a runtime whose broker captures the admitted
// envelope and (optionally) runs an inline executor.
func newQueuedSearchService(t *testing.T) (*Service, *state.Stores, *fakeBroker) {
	t.Helper()
	service, stores := newKBService(t, "http://127.0.0.1:1")
	stores.Results.Transient = newMemoryTransient()
	broker := &fakeBroker{}
	broker.onSearch = func(envelope state.SearchEnvelope) error {
		return service.executeQueuedSearchTaskOnce(context.Background(), envelope,
			func(_ context.Context, namespace string, arguments map[string]any) (map[string]any, error) {
				return map[string]any{
					"query":           arguments["query"],
					"evidence_status": "no_evidence",
					"answer_guidance": "The knowledge base is insufficient.",
					"results":         []any{},
					"debug":           map[string]any{"namespace": namespace},
				}, nil
			})
	}
	if err := service.StartBackground(context.Background(),
		WithBackgroundPublisher(broker),
		WithMaintenanceInterval(0),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(service.StopBackground)
	return service, stores, broker
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

func TestSubmitSearchNoBrokerFailsClosed(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	_, err := service.submitSearchTask(ctx, "alpha", sampleSearchRequest())
	var knowledge *apierrors.KnowledgeError
	if !errors.As(err, &knowledge) || knowledge.Message != searchSubmissionFailedMessage {
		t.Fatalf("error = %v, want fail-closed KnowledgeError", err)
	}
	if _, err := service.submitSearch(ctx, "alpha", sampleSearchRequest(), 0); err == nil {
		t.Fatalf("queued search without a broker succeeded")
	}
	// Fail-closed means no durable result id was handed out.
	var tasks int
	if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM search_tasks").Scan(&tasks); err != nil {
		t.Fatalf("count search_tasks: %v", err)
	}
	if tasks != 0 {
		t.Errorf("search_tasks rows = %d, want 0", tasks)
	}
}

func TestSubmitSearchCompletesWithinWait(t *testing.T) {
	service, stores, broker := newQueuedSearchService(t)
	ctx := context.Background()
	payload, err := service.submitSearch(ctx, "alpha", sampleSearchRequest(), 3)
	if err != nil {
		t.Fatalf("submitSearch() error = %v", err)
	}
	if payload["status"] != nil {
		// The full snapshot payload does not carry a queued status.
		t.Errorf("unexpected status field: %v", payload["status"])
	}
	if payload["evidence_status"] != "no_evidence" {
		t.Fatalf("payload = %v", payload)
	}
	if _, ok := payload["results"].([]any); !ok {
		t.Errorf("results = %T", payload["results"])
	}
	resultID, _ := payload["result_id"].(string)
	if resultID == "" {
		t.Fatalf("result_id missing: %v", payload)
	}
	if broker.searchEnvelopeCount() != 1 {
		t.Fatalf("published envelopes = %d", broker.searchEnvelopeCount())
	}
	envelope := broker.lastSearchEnvelope()
	if envelope.Namespace != "alpha" {
		t.Errorf("envelope namespace = %q", envelope.Namespace)
	}
	if envelope.Arguments["query"] != "E03 供水不足" || envelope.Arguments["retrieval"] != "hybrid" ||
		envelope.Arguments["rerank"] != "cross_encoder" {
		t.Errorf("envelope arguments = %v", envelope.Arguments)
	}
	filters, _ := envelope.Arguments["filters"].(map[string]any)
	if filters["document_id"] != "doc-1" {
		t.Errorf("filters = %v", envelope.Arguments["filters"])
	}
	task, err := stores.SearchTasks.Get(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("SearchTasks.Get() error = %v", err)
	}
	if task == nil || task.Status != state.SearchTaskSucceeded {
		t.Fatalf("task = %+v", task)
	}

	// The ordered result read goes through the task view first and then the
	// frozen snapshot.
	read, err := service.getSearchResultOrdered(ctx, "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("getSearchResultOrdered() error = %v", err)
	}
	if read["result_id"] != resultID || read["snapshot_available"] != true {
		t.Errorf("stored result = %v", read)
	}
}

func TestSubmitSearchWaitTimeoutReturnsOnlyResultID(t *testing.T) {
	service, stores, broker := newQueuedSearchService(t)
	// Disable the inline executor: the admitted task stays queued.
	broker.mu.Lock()
	broker.onSearch = nil
	broker.mu.Unlock()
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
	if !canonicalUUIDPattern.MatchString(resultID) {
		t.Fatalf("result_id = %v", payload["result_id"])
	}
	// The queued task view is returned while it is still active.
	read, err := service.getSearchResultOrdered(context.Background(), "alpha", resultID, 1, 10)
	if err != nil {
		t.Fatalf("getSearchResultOrdered() error = %v", err)
	}
	if read["status"] != "queued" || read["stage"] == "" || read["result_id"] != resultID {
		t.Fatalf("queued result = %v", read)
	}
	task, err := stores.SearchTasks.Get(context.Background(), "alpha", resultID, 1, 10)
	if err != nil || task == nil || task.Status != state.SearchTaskQueued {
		t.Fatalf("task = %+v, %v", task, err)
	}
}

func TestSubmitSearchPublishFailureMarksTaskFailed(t *testing.T) {
	service, stores, broker := newQueuedSearchService(t)
	broker.mu.Lock()
	broker.onSearch = nil
	broker.searchErr = errors.New("broker unavailable")
	broker.mu.Unlock()
	_, err := service.submitSearchTask(context.Background(), "alpha", sampleSearchRequest())
	var knowledge *apierrors.KnowledgeError
	if !errors.As(err, &knowledge) || knowledge.Message != searchSubmissionFailedMessage {
		t.Fatalf("error = %v, want fail-closed KnowledgeError", err)
	}
	if broker.searchEnvelopeCount() != 1 {
		t.Fatalf("published envelopes = %d", broker.searchEnvelopeCount())
	}
	resultID := broker.lastSearchEnvelope().ResultID
	_, err = stores.SearchTasks.Get(context.Background(), "alpha", resultID, 1, 10)
	var unavailable *state.SnapshotUnavailable
	if !errors.As(err, &unavailable) || unavailable.Message != "Search failed; submit a new search" {
		t.Fatalf("failed task read error = %v", err)
	}
}

func TestGetSearchResultOrderingAndErrors(t *testing.T) {
	service, stores, _ := newQueuedSearchService(t)
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
	// Invalid UUIDs are a snapshot-unavailable error (Python get raises it for
	// any invalid id).
	if _, err := service.getSearchResultOrdered(ctx, "alpha", "nope", 1, 10); err == nil {
		t.Fatalf("invalid result id returned a payload")
	}
	// A succeeded task without a stored snapshot is expired/unavailable.
	arguments := searchArguments(sampleSearchRequest())
	envelope, err := stores.SearchTasks.Submit(ctx, "alpha", arguments, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit() error = %v", err)
	}
	task, err := stores.SearchTasks.Claim(ctx, envelope)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %v, %v", task, err)
	}
	if _, err := stores.SearchTasks.Complete(ctx, task, map[string]any{"results": []any{}}); err != nil {
		t.Fatalf("Complete() error = %v", err)
	}
	stores.Results.Transient = nil
	if _, err := service.getSearchResultOrdered(ctx, "alpha", envelope.ResultID, 1, 10); err == nil {
		t.Fatalf("succeeded task without a snapshot returned a payload")
	}
	var unavailable *apierrors.SnapshotUnavailable
	if _, err := service.getSearchResultOrdered(ctx, "alpha", envelope.ResultID, 1, 10); !errors.As(err, &unavailable) {
		t.Fatalf("missing snapshot error = %v", err)
	}
}

func TestSubmitSearchWaitSecondsValidation(t *testing.T) {
	service, _, _ := newQueuedSearchService(t)
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
