package qdrant

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestReadVersionChunkForSag(t *testing.T) {
	for _, responseVersion := range []string{"v2", "v1"} {
		t.Run(responseVersion, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var body struct {
					IDs        []string `json:"ids"`
					WithVector bool     `json:"with_vector"`
				}
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode point request: %v", err)
				}
				store := Store{namespace: "rag-notes"}
				want := store.versionPointID("chunk", "chunkA", "v2", "agentic-rag")
				if r.Method != "POST" || r.URL.Path != "/collections/rag-docs/points" || len(body.IDs) != 1 || body.IDs[0] != want || body.WithVector {
					t.Errorf("unexpected point request: %s %s %+v", r.Method, r.URL.Path, body)
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"result": []Point{{
					ID: PointID(want),
					Payload: map[string]any{
						"record_type": "chunk", "namespace": "rag-notes", "document_id": "agentic-rag",
						"upload_version": responseVersion, "chunk_id": "chunkA", "text": "Agent 决定是否再次检索",
					},
				}}})
			}))
			defer server.Close()

			store := NewStoreWithClient(StoreConfig{Namespace: "rag-notes", Collection: "rag-docs"}, NewClient(server.URL, "", nil))
			chunk, err := store.ReadVersionChunk(context.Background(), "agentic-rag", "v2", "chunkA")
			if responseVersion == "v1" {
				if err == nil {
					t.Fatal("accepted an old chunk version")
				}
				return
			}
			if err != nil || chunk.Text != "Agent 决定是否再次检索" {
				t.Fatalf("ReadVersionChunk = %+v, %v", chunk, err)
			}
		})
	}
}
