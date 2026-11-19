package qdrant

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// TestScrollActiveChunksFiltersCorpus pins the corpus contract of
// ScrollActiveChunks: only chunk points of the store's namespace reach the
// visit callback — asset points, foreign namespaces and unnamespaced points
// never do.
func TestScrollActiveChunksFiltersCorpus(t *testing.T) {
	scrollBody := `{"result":{"points":[
		{"id":"11111111-1111-5111-8111-111111111111",
		 "payload":{"record_type":"chunk","chunk_id":"chunk-1","document_id":"Doc","text":"alpha chunk","namespace":"alpha"}},
		{"id":"22222222-2222-5222-8222-222222222222",
		 "payload":{"record_type":"asset","asset_id":"asset-1","document_id":"Doc","namespace":"alpha"}},
		{"id":"33333333-3333-5333-8333-333333333333",
		 "payload":{"record_type":"chunk","chunk_id":"chunk-beta","document_id":"Doc","text":"beta chunk","namespace":"beta"}},
		{"id":"44444444-4444-5444-8444-444444444444",
		 "payload":{"record_type":"chunk","chunk_id":"chunk-none","document_id":"Doc","text":"unnamespaced"}}
	],"next_page_offset":null}}`
	fake, server := newFakeQdrant(t, func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/scroll") {
			return http.StatusOK, scrollBody
		}
		return 0, ""
	})
	store := testStore(t, server, func(config *StoreConfig) { config.Namespace = "alpha" })

	var visited [][]Point
	if err := store.ScrollActiveChunks(context.Background(), func(points []Point) error {
		visited = append(visited, points)
		return nil
	}); err != nil {
		t.Fatalf("ScrollActiveChunks() error: %v", err)
	}
	if len(visited) != 1 || len(visited[0]) != 1 {
		t.Fatalf("visited pages = %s", prettyJSON(visited))
	}
	if payloadString(visited[0][0].Payload, "chunk_id") != "chunk-1" {
		t.Fatalf("visited chunk = %+v", visited[0][0].Payload)
	}
	if scroll := fake.only(t, http.MethodPost, "/points/scroll"); scroll.Body["limit"] != float64(256) {
		t.Fatalf("scroll limit = %v", scroll.Body["limit"])
	}
}

// TestHydrateChunksKeepsFirstMatchPerID pins the evidence-text source: the
// first active chunk carrying a wanted chunk_id wins over later duplicates,
// every wanted id is hydrated across pages, and an empty request never
// scrolls.
func TestHydrateChunksKeepsFirstMatchPerID(t *testing.T) {
	pages := []string{
		`{"result":{"points":[
			{"id":"11111111-1111-5111-8111-111111111111",
			 "payload":{"record_type":"chunk","chunk_id":"chunk-1","text":"first","namespace":"alpha"}}
		],"next_page_offset":"22222222-2222-5222-8222-222222222222"}}`,
		`{"result":{"points":[
			{"id":"33333333-3333-5333-8333-333333333333",
			 "payload":{"record_type":"chunk","chunk_id":"chunk-1","text":"second","namespace":"alpha"}},
			{"id":"44444444-4444-5444-8444-444444444444",
			 "payload":{"record_type":"chunk","chunk_id":"chunk-2","text":"only","namespace":"alpha"}}
		],"next_page_offset":null}}`,
	}
	calls := 0
	_, server := newFakeQdrant(t, func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/scroll") {
			page := pages[min(calls, len(pages)-1)]
			calls++
			return http.StatusOK, page
		}
		return 0, ""
	})
	store := testStore(t, server, func(config *StoreConfig) { config.Namespace = "alpha" })

	empty, err := store.HydrateChunks(context.Background(), nil)
	if err != nil || len(empty) != 0 {
		t.Fatalf("HydrateChunks(nil) = %v, %v", empty, err)
	}
	if calls != 0 {
		t.Fatalf("empty request scrolled %d pages", calls)
	}

	hydrated, err := store.HydrateChunks(context.Background(), []string{"chunk-1", "chunk-2"})
	if err != nil {
		t.Fatalf("HydrateChunks() error = %v", err)
	}
	if len(hydrated) != 2 {
		t.Fatalf("hydrated = %s", prettyJSON(hydrated))
	}
	if payloadString(hydrated[0].Payload, "text") != "first" {
		t.Fatalf("chunk-1 text = %q, want the first-scrolled value", payloadString(hydrated[0].Payload, "text"))
	}
	if payloadString(hydrated[1].Payload, "chunk_id") != "chunk-2" {
		t.Fatalf("second hydrated = %s", prettyJSON(hydrated))
	}
	if calls != 2 {
		t.Fatalf("scroll pages = %d, want 2", calls)
	}
}
