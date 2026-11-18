package runtime

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// fakeAssetFetcher is the projector's Qdrant seam.
type fakeAssetFetcher struct {
	assets map[string]map[string]any
	err    error
	ids    []string
}

func (f *fakeAssetFetcher) GetAssets(_ context.Context, assetIDs []string) (map[string]map[string]any, error) {
	f.ids = append([]string(nil), assetIDs...)
	if f.err != nil {
		return nil, f.err
	}
	return f.assets, nil
}

// fakeAssetObjects is an in-memory assetObjectReader keyed by version|bucket|key.
type fakeAssetObjects struct {
	objects map[string][]byte
}

func (f fakeAssetObjects) ReadAssetObject(_ context.Context, version, bucket, key string) ([]byte, bool) {
	content, found := f.objects[version+"|"+bucket+"|"+key]
	return content, found
}

func testCandidate(metadata map[string]any) contracts.Candidate {
	return contracts.Candidate{
		DocumentID: "doc-picture",
		ChunkID:    "doc-picture:1",
		Title:      "manual",
		Source:     "manual.pdf",
		Text:       "figure evidence",
		Score:      0.123456789,
		Metadata:   metadata,
	}
}

func testPictureAsset(storage string) map[string]any {
	asset := map[string]any{
		"record_type":    "asset",
		"asset_id":       "doc-picture:picture-1",
		"document_id":    "doc-picture",
		"asset_kind":     "picture",
		"asset_storage":  storage,
		"upload_version": "version-1",
		"caption":        "Figure 1",
		"caption_source": "parser",
		"mime_type":      "image/png",
		"image_file":     "doc-picture/pictures/picture-1.png",
		"captions":       []any{map[string]any{"text": "caption note"}},
		"footnotes":      []any{map[string]any{"text": "footnote note"}},
	}
	if storage == "word" || storage == "html" {
		asset["location"] = map[string]any{"headings": []any{"Chapter 1"}, "paragraph": 3.0}
	} else {
		asset["page"] = 2.0
		asset["bbox"] = map[string]any{"left": 1.0, "top": 2.0, "right": 3.0, "bottom": 4.0}
	}
	return asset
}

func TestProjectAssetResultsImagePDF(t *testing.T) {
	fetcher := &fakeAssetFetcher{assets: map[string]map[string]any{
		"doc-picture:picture-1": testPictureAsset("pdf"),
	}}
	objects := fakeAssetObjects{objects: map[string][]byte{
		"version-1|" + state.PDFAssetBucket + "|doc-picture/pictures/picture-1.png": []byte("image-bytes"),
	}}
	candidates := []contracts.Candidate{testCandidate(map[string]any{
		"asset_id": "doc-picture:picture-1", "upload_version": "version-1",
	})}

	results := projectAssetResults(context.Background(), fetcher, objects, candidates)
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	hit := results[0]
	if _, hasText := hit["text"]; hasText {
		t.Errorf("asset hit must not carry the plain text field: %v", hit)
	}
	if hit["rank"] != 1 || hit["score"] != 0.123457 {
		t.Errorf("rank/score = %v/%v", hit["rank"], hit["score"])
	}
	asset, ok := hit["asset"].(map[string]any)
	if !ok {
		t.Fatalf("asset = %#v", hit["asset"])
	}
	if asset["type"] != "image" || asset["mime_type"] != "image/png" ||
		asset["data"] != base64.StdEncoding.EncodeToString([]byte("image-bytes")) {
		t.Errorf("image asset = %v", asset)
	}
	if asset["caption"] != "Figure 1" || asset["page"] != 2.0 {
		t.Errorf("image caption/page = %v/%v", asset["caption"], asset["page"])
	}
	if _, has := asset["location"]; has {
		t.Errorf("PDF image must not carry a word location: %v", asset)
	}
	metadata, _ := hit["metadata"].(map[string]any)
	if metadata["asset_id"] != "doc-picture:picture-1" || metadata["asset_kind"] != "picture" {
		t.Errorf("metadata identity = %v", metadata)
	}
	if metadata["page"] != 2.0 || metadata["bbox"] == nil {
		t.Errorf("metadata page/bbox = %v/%v", metadata["page"], metadata["bbox"])
	}
	if metadata["caption_source"] != "parser" {
		t.Errorf("caption_source = %v", metadata["caption_source"])
	}
	if !reflect.DeepEqual(metadata["captions"], []any{map[string]any{"text": "caption note"}}) {
		t.Errorf("captions = %v", metadata["captions"])
	}
	if !reflect.DeepEqual(metadata["footnotes"], []any{map[string]any{"text": "footnote note"}}) {
		t.Errorf("footnotes = %v", metadata["footnotes"])
	}
	// The candidate metadata map must not be mutated.
	if _, mutated := candidates[0].Metadata["asset_kind"]; mutated {
		t.Errorf("candidate metadata was mutated: %v", candidates[0].Metadata)
	}
}

// TestProjectAssetResultsImageHTML pins that Docling HTML pictures use the same
// structured projection as Word (no page/bbox, location instead) and are loaded
// from the html asset bucket.
func TestProjectAssetResultsImageHTML(t *testing.T) {
	fetcher := &fakeAssetFetcher{assets: map[string]map[string]any{
		"doc-picture:picture-1": testPictureAsset("html"),
	}}
	objects := fakeAssetObjects{objects: map[string][]byte{
		"version-1|" + state.HTMLAssetBucket + "|doc-picture/pictures/picture-1.png": []byte("html-image-bytes"),
	}}
	candidates := []contracts.Candidate{testCandidate(map[string]any{
		"asset_id": "doc-picture:picture-1", "upload_version": "version-1",
	})}

	results := projectAssetResults(context.Background(), fetcher, objects, candidates)
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	hit := results[0]
	asset, ok := hit["asset"].(map[string]any)
	if !ok {
		t.Fatalf("asset = %#v", hit["asset"])
	}
	if asset["data"] != base64.StdEncoding.EncodeToString([]byte("html-image-bytes")) {
		t.Errorf("image data = %v", asset["data"])
	}
	if _, has := asset["page"]; has {
		t.Errorf("HTML image must not carry page: %v", asset)
	}
	if _, has := asset["bbox"]; has {
		t.Errorf("HTML image must not carry bbox: %v", asset)
	}
	if asset["location"] == nil {
		t.Errorf("HTML image must carry a location: %v", asset)
	}
	metadata, _ := hit["metadata"].(map[string]any)
	if _, has := metadata["page"]; has {
		t.Errorf("HTML metadata must not carry page: %v", metadata)
	}
	if metadata["location"] == nil {
		t.Errorf("HTML metadata must carry a location: %v", metadata)
	}
}

func TestProjectAssetResultsWordTable(t *testing.T) {
	table := map[string]any{
		"rows":          []any{[]any{"a", "b"}, []any{"c", "d"}},
		"headers":       []any{"H1", "H2"},
		"header_rows":   []any{[]any{"H1", "H2"}},
		"cells":         []any{map[string]any{"row": 0.0, "col": 0.0, "text": "a"}},
		"num_rows":      2.0,
		"num_cols":      2.0,
		"footnotes":     []any{"table note"},
		"nested_tables": map[string]any{"doc-picture:table-2": map[string]any{"rows": []any{}}},
		"parent_cell":   map[string]any{"asset_id": "doc-picture:table-0", "row": 1.0, "col": 0.0},
	}
	asset := map[string]any{
		"record_type":    "asset",
		"asset_id":       "doc-picture:table-1",
		"document_id":    "doc-picture",
		"asset_kind":     "table",
		"asset_storage":  "word",
		"upload_version": "version-1",
		"caption":        "Table 1",
		"caption_source": "style",
		"table":          table,
		"location":       map[string]any{"headings": []any{"Chapter 2"}, "paragraph": 7.0},
	}
	fetcher := &fakeAssetFetcher{assets: map[string]map[string]any{"doc-picture:table-1": asset}}
	candidates := []contracts.Candidate{testCandidate(map[string]any{
		"asset_id": "doc-picture:table-1", "upload_version": "version-1",
	})}

	results := projectAssetResults(context.Background(), fetcher, fakeAssetObjects{}, candidates)
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	hit := results[0]
	if _, hasText := hit["text"]; hasText {
		t.Errorf("table hit must not carry the plain text field")
	}
	rendered, ok := hit["asset"].(map[string]any)
	if !ok {
		t.Fatalf("table asset = %#v", hit["asset"])
	}
	if rendered["type"] != "table" || rendered["caption"] != "Table 1" {
		t.Errorf("table asset = %v", rendered)
	}
	if _, has := rendered["page"]; has {
		t.Errorf("word table must not carry page: %v", rendered)
	}
	if rendered["location"] == nil || rendered["nested_tables"] == nil || rendered["parent_cell"] == nil {
		t.Errorf("word table extras = location=%v nested=%v parent=%v",
			rendered["location"], rendered["nested_tables"], rendered["parent_cell"])
	}
	metadata := hit["metadata"].(map[string]any)
	if _, has := metadata["page"]; has {
		t.Errorf("word metadata must not carry page: %v", metadata)
	}
	if metadata["location"] == nil || metadata["captions"] == nil || metadata["footnotes"] == nil {
		t.Errorf("word metadata = %v", metadata)
	}
}

func TestProjectAssetResultsVersionFilteringAndMissingAssets(t *testing.T) {
	asset := testPictureAsset("pdf")
	asset["upload_version"] = "version-2"
	fetcher := &fakeAssetFetcher{assets: map[string]map[string]any{
		"doc-picture:picture-1": asset,
	}}
	objects := fakeAssetObjects{objects: map[string][]byte{
		"version-1|" + state.PDFAssetBucket + "|doc-picture/pictures/picture-1.png": []byte("image-bytes"),
	}}
	candidates := []contracts.Candidate{
		testCandidate(map[string]any{"asset_id": "doc-picture:picture-1", "upload_version": "version-1"}),
		testCandidate(map[string]any{"asset_id": "doc-picture:foreign", "upload_version": "version-1"}),
	}
	results := projectAssetResults(context.Background(), fetcher, objects, candidates)
	if len(results) != 0 {
		t.Fatalf("stale/foreign assets must be dropped, results = %v", results)
	}

	// A missing staged image also drops the hit (python `content is None`).
	asset = testPictureAsset("pdf")
	fetcher = &fakeAssetFetcher{assets: map[string]map[string]any{"doc-picture:picture-1": asset}}
	results = projectAssetResults(context.Background(), fetcher, fakeAssetObjects{}, candidates[:1])
	if len(results) != 0 {
		t.Fatalf("missing image bytes must drop the hit, results = %v", results)
	}
}

func TestProjectAssetResultsPlainAndFailureFallback(t *testing.T) {
	plain := testCandidate(map[string]any{"kind": "test"})
	results := projectAssetResults(context.Background(), &fakeAssetFetcher{}, fakeAssetObjects{}, []contracts.Candidate{plain})
	if len(results) != 1 || results[0]["text"] != "figure evidence" {
		t.Fatalf("plain candidate = %v", results)
	}
	if results[0]["metadata"] == nil || results[0]["document_id"] != "doc-picture" {
		t.Errorf("plain projection = %v", results[0])
	}

	// Failure-safe: a Qdrant asset lookup error returns unprojected candidates.
	withAsset := testCandidate(map[string]any{"asset_id": "doc-picture:picture-1", "upload_version": "version-1"})
	fetcher := &fakeAssetFetcher{err: assertError("qdrant unavailable")}
	results = projectAssetResults(context.Background(), fetcher, fakeAssetObjects{}, []contracts.Candidate{withAsset})
	if len(results) != 1 || results[0]["text"] != "figure evidence" {
		t.Fatalf("fallback results = %v", results)
	}
	if _, has := results[0]["asset"]; has {
		t.Errorf("fallback must not invent an asset: %v", results[0])
	}
}

type assertError string

func (e assertError) Error() string { return string(e) }

// newAssetQdrant serves the Qdrant surface used by Search plus GetAssets:
// collection exists/get, points/query (retrieved chunks) and points/scroll
// (asset records).
func newAssetQdrant(t *testing.T, queryPoints, assetPoints []map[string]any) string {
	t.Helper()
	queryJSON, err := json.Marshal(queryPoints)
	if err != nil {
		t.Fatalf("marshal query points: %v", err)
	}
	// Scroll returns Qdrant points wrapping the asset payloads.
	wrapped := make([]map[string]any, len(assetPoints))
	for index, payload := range assetPoints {
		wrapped[index] = map[string]any{"id": fmt.Sprintf("asset-point-%d", index), "payload": payload}
	}
	assetJSON, err := json.Marshal(wrapped)
	if err != nil {
		t.Fatalf("marshal asset points: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		path := request.URL.Path
		switch {
		case strings.HasSuffix(path, "/exists"):
			_, _ = writer.Write([]byte(`{"result":{"exists":true}}`))
		case strings.HasSuffix(path, "/points/query"):
			_, _ = writer.Write([]byte(`{"result":{"points":` + string(queryJSON) + `}}`))
		case strings.HasSuffix(path, "/points/scroll"):
			_, _ = writer.Write([]byte(`{"result":{"points":` + string(assetJSON) + `,"next_page_offset":null}}`))
		case request.Method == http.MethodGet && strings.HasPrefix(path, "/collections/"):
			_, _ = writer.Write([]byte(`{"result":{"config":{"params":{"sparse_vectors":{"sparse":{"modifier":"idf"}}}}}}`))
		default:
			_, _ = writer.Write([]byte(`{"result":true}`))
		}
	}))
	t.Cleanup(server.Close)
	return server.URL
}

func TestTenantProjectorHydratesSearchAndRawAssets(t *testing.T) {
	assetRecord := testPictureAsset("pdf")
	assetRecord["namespace"] = "alpha"
	queryPoints := []map[string]any{{
		"id":      "p1",
		"version": 1,
		"score":   0.9,
		"payload": map[string]any{
			"record_type":    "chunk",
			"namespace":      "alpha",
			"document_id":    "doc-picture",
			"chunk_id":       "doc-picture:1",
			"text":           "figure evidence",
			"title":          "manual",
			"source":         "manual.pdf",
			"upload_version": "version-1",
			"metadata": map[string]any{
				"asset_id":       "doc-picture:picture-1",
				"asset_kind":     "picture",
				"upload_version": "version-1",
			},
		},
	}}
	qdrantURL := newAssetQdrant(t, queryPoints, []map[string]any{assetRecord})
	modelURL, _ := newFakeModelAPI(t, nil)
	engine, stores := newSearchEngineForTest(t, qdrantURL, modelURL)

	insertUploadObject(t, stores, "version-1", state.PDFAssetBucket,
		"doc-picture/pictures/picture-1.png", []byte("image-bytes"))

	request := mustSearchRequest(t, map[string]any{
		"query": "figure", "retrieval": "embedding", "rerank": "none", "candidate_k": 1, "top_k": 1,
	})
	payload, err := engine.Search(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	results := payloadResults(t, payload)
	if len(results) != 1 {
		t.Fatalf("results = %v", results)
	}
	hit := results[0]
	asset, ok := hit["asset"].(map[string]any)
	if !ok {
		t.Fatalf("search hit asset = %#v", hit["asset"])
	}
	if asset["data"] != base64.StdEncoding.EncodeToString([]byte("image-bytes")) {
		t.Errorf("asset data = %v", asset["data"])
	}
	if _, hasText := hit["text"]; hasText {
		t.Errorf("asset hit must not carry text: %v", hit)
	}
	metadata := hit["metadata"].(map[string]any)
	if metadata["caption_source"] != "parser" || metadata["captions"] == nil {
		t.Errorf("metadata = %v", metadata)
	}

	// SearchRaw (search worker seam) must project assets too.
	rawPayload, err := engine.SearchRaw(context.Background(), "alpha", request)
	if err != nil {
		t.Fatalf("SearchRaw() error = %v", err)
	}
	rawResults := payloadResults(t, rawPayload)
	if len(rawResults) != 1 || rawResults[0]["asset"] == nil {
		t.Fatalf("SearchRaw results = %v", rawResults)
	}
	rawAsset := rawResults[0]["asset"].(map[string]any)
	if rawAsset["type"] != "image" {
		t.Errorf("SearchRaw asset = %v", rawAsset)
	}
}
