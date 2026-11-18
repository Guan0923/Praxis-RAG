package e2e

// Real-entry coverage for the display-view fallback budget: a REST /v1/documents
// request walks REST -> runtime.Service.ListDocuments -> cached read view ->
// CacheSynchronizer.GetOrLoad. When the instance-level fallback budget is
// exhausted, the request must receive 503 "服务繁忙，请稍后重试" and the runtime
// must NOT retry it through authoritativeDocuments (which reads MySQL directly
// and would bypass the limit).

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// blockingViewLoader replaces the "documents" view loader: it reports that the
// MySQL query started, blocks until released and returns a marker document so
// the response proves the display view (not the authoritative repository)
// served it.
type blockingViewLoader struct {
	started chan string
	release chan struct{}
	calls   int32
}

func newBlockingViewLoader() *blockingViewLoader {
	return &blockingViewLoader{started: make(chan string, 8), release: make(chan struct{})}
}

func (l *blockingViewLoader) load(_ context.Context, _ state.ViewQuerier, scope string) (any, error) {
	atomic.AddInt32(&l.calls, 1)
	l.started <- scope
	<-l.release
	return []any{map[string]any{"name": "from-view-loader", "scope": scope}}, nil
}

func (l *blockingViewLoader) callCount() int { return int(atomic.LoadInt32(&l.calls)) }

func (l *blockingViewLoader) open() { close(l.release) }

// TestRESTViewFallbackBudgetExhaustedServes503 proves the full real chain:
//
//  1. a blocked view query (one knowledge base) occupies the whole budget;
//  2. a second, different knowledge base receives an immediate 503 with the
//     shared busy detail — because a fall-through to authoritativeDocuments
//     would have answered 200 (MySQL is healthy and its only error path is a
//     database failure that does not occur here), the 503 itself proves the
//     authoritative repository was never consulted;
//  3. the rejected request never reached the view loader;
//  4. releasing the budget completes the first request with the view loader's
//     own value, and a follow-up request is served normally.
func TestRESTViewFallbackBudgetExhaustedServes503(t *testing.T) {
	h := newHarness(t, "")
	// One display fallback per instance: the blocked query exhausts it.
	h.Stores.CacheSync.Settings.FallbackConcurrency = 1
	loader := newBlockingViewLoader()
	h.Stores.CacheSync.Register("documents", loader.load)
	handler := newRESTHandler(t, h)

	// The first request (alpha) takes the only slot and stays executing.
	firstDone := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		firstDone <- restRequest(handler, http.MethodGet, "/v1/documents", "alpha", "", "", nil)
	}()
	select {
	case <-loader.started:
	case <-time.After(10 * time.Second):
		t.Fatalf("the first view query never started")
	}

	// A second, different knowledge base is rejected immediately: no queueing,
	// and the runtime must not quietly re-read the documents repository.
	rejected := restRequest(handler, http.MethodGet, "/v1/documents", "beta", "", "", nil)
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("busy status = %d body = %s", rejected.Code, rejected.Body.String())
	}
	if detail := decodeRESTBody(t, rejected)["detail"]; detail != state.ViewBackpressureMessage {
		t.Fatalf("busy detail = %v, want %q", detail, state.ViewBackpressureMessage)
	}
	// The rejected request never started a second view query.
	if calls := loader.callCount(); calls != 1 {
		t.Fatalf("the rejected request reached the view loader: %d calls", calls)
	}

	// Release the budget: the first request finishes through the view path.
	loader.open()
	first := <-firstDone
	if first.Code != http.StatusOK {
		t.Fatalf("first request status = %d body = %s", first.Code, first.Body.String())
	}
	documents, ok := decodeRESTBody(t, first)["documents"].([]any)
	if !ok || len(documents) != 1 {
		t.Fatalf("first request documents = %v", documents)
	}
	firstDocument, _ := documents[0].(map[string]any)
	if firstDocument["name"] != "from-view-loader" {
		t.Fatalf("first request document = %v, want the view loader's marker", firstDocument)
	}

	// The budget is free again: a fresh knowledge base is served normally.
	followUp := restRequest(handler, http.MethodGet, "/v1/documents", "beta", "", "", nil)
	if followUp.Code != http.StatusOK {
		t.Fatalf("follow-up status = %d body = %s", followUp.Code, followUp.Body.String())
	}
	followUpDocuments, _ := decodeRESTBody(t, followUp)["documents"].([]any)
	if len(followUpDocuments) != 1 {
		t.Fatalf("follow-up documents = %v", followUpDocuments)
	}
	if calls := loader.callCount(); calls != 2 {
		t.Fatalf("follow-up view queries = %d, want 2", calls)
	}
}
