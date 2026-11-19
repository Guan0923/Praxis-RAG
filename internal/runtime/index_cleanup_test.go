package runtime

import (
	"context"
	"encoding/json"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// Exercise the worker entrypoint with a real disposable MySQL store and the
// Qdrant/LightRAG HTTP contracts, including a same-name replacement on retry.
func TestDeleteWorkerReconcileAndFence(t *testing.T) {
	var mu sync.Mutex
	var qDeletes, graphDeletes int
	var problems []string
	remote := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/collections/manuals/exists":
			w.Write([]byte(`{"result":{"exists":true}}`))
		case "/collections/manuals/points/scroll":
			w.Write([]byte(`{"result":{"points":[{"id":"1","payload":{"document_id":"doc-x7","upload_version":"v1"}}],"next_page_offset":null}}`))
		case "/collections/manuals/points/delete":
			var body struct {
				Filter struct {
					Must []struct {
						Key   string
						Match struct{ Value string }
					}
				}
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				problems = append(problems, err.Error())
			}
			filter := map[string]string{}
			for _, field := range body.Filter.Must {
				filter[field.Key] = field.Match.Value
			}
			if filter["namespace"] != "manuals" || filter["document_id"] != "doc-x7" || filter["upload_version"] != "v1" {
				problems = append(problems, "incorrect deletion filter")
			}
			qDeletes++
			w.Write([]byte(`{"result":{"status":"completed"}}`))
		case "/v1/workspaces/manuals/inventory":
			if r.URL.Query().Get("after") == "" {
				w.Write([]byte(`{"documents":[{"document_id":"doc-x7","revision":"old"}]}`))
			} else {
				w.Write([]byte(`{"documents":[]}`))
			}
		case "/v1/workspaces/manuals/documents/doc-x7/revision":
			w.Write([]byte(`{"revision":"old"}`))
		case "/v1/workspaces/manuals/documents/doc-x7/prune":
			graphDeletes++
			w.WriteHeader(http.StatusServiceUnavailable)
			w.Write([]byte(`{"detail":"offline"}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer remote.Close()
	s, stores := newKBService(t, remote.URL)
	s.config.LightRAGServiceURL = remote.URL
	ctx := context.Background()
	if err := s.ReconcileIndexScope(ctx, "manuals", "manuals"); err != nil {
		t.Fatal(err)
	}
	due := func() {
		t.Helper()
		if _, err := stores.DB.Exec("UPDATE index_cleanup_jobs SET due_at=0 WHERE status='waiting'"); err != nil {
			t.Fatal(err)
		}
		if err := stores.ScheduleIndexCleanup(ctx); err != nil {
			t.Fatal(err)
		}
	}
	due()
	if n, err := s.CleanupIndexBatch(ctx); n != 2 || err == nil {
		t.Fatalf("first drain %d %v", n, err)
	}
	if err := stores.Documents.Upsert(ctx, "manuals", state.DocumentRecord{Name: "X7.md", DocumentID: "doc-x7", Collection: "manuals", Chunking: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	due()
	if n, err := s.CleanupIndexBatch(ctx); n != 1 || err != nil {
		t.Fatalf("second drain %d %v", n, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if qDeletes != 1 || graphDeletes != 1 || len(problems) > 0 {
		t.Fatal(qDeletes, graphDeletes, problems)
	}
}
