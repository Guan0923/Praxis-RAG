package qdrant

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/sparse"
)

// The expected point ids in this file were generated with the Python
// reference implementation (uuid.uuid5 + json.dumps), so they lock parity.

type qdrantRequest struct {
	Method string
	Path   string
	Query  url.Values
	Body   map[string]any
}

type fakeQdrant struct {
	t        *testing.T
	mu       sync.Mutex
	requests []qdrantRequest
}

func (f *fakeQdrant) all() []qdrantRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]qdrantRequest, len(f.requests))
	copy(copied, f.requests)
	return copied
}

func (f *fakeQdrant) count() int { return len(f.all()) }

func (f *fakeQdrant) find(method, suffix string) []qdrantRequest {
	matches := make([]qdrantRequest, 0, 2)
	for _, request := range f.all() {
		if request.Method == method && strings.HasSuffix(request.Path, suffix) {
			matches = append(matches, request)
		}
	}
	return matches
}

func (f *fakeQdrant) only(t *testing.T, method, suffix string) qdrantRequest {
	t.Helper()
	matches := f.find(method, suffix)
	if len(matches) != 1 {
		t.Fatalf("expected exactly one %s *%s request, got %d", method, suffix, len(matches))
	}
	return matches[0]
}

func newFakeQdrant(t *testing.T, handler func(qdrantRequest) (int, string)) (*fakeQdrant, *httptest.Server) {
	t.Helper()
	fake := &fakeQdrant{t: t}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, err := io.ReadAll(request.Body)
		if err != nil {
			t.Errorf("cannot read request body: %v", err)
		}
		recorded := qdrantRequest{Method: request.Method, Path: request.URL.Path, Query: request.URL.Query()}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &recorded.Body); err != nil {
				t.Errorf("request body is not a JSON object: %v (%s)", err, raw)
			}
		}
		fake.mu.Lock()
		fake.requests = append(fake.requests, recorded)
		fake.mu.Unlock()
		status, response := 0, ""
		if handler != nil {
			status, response = handler(recorded)
		}
		if status == 0 {
			status = http.StatusOK
		}
		if response == "" {
			response = defaultQdrantResponse(recorded)
		}
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return fake, server
}

func defaultQdrantResponse(request qdrantRequest) string {
	path := request.Path
	switch {
	case strings.HasSuffix(path, "/exists"):
		return `{"result":{"exists":true}}`
	case strings.HasSuffix(path, "/points/scroll"):
		return `{"result":{"points":[],"next_page_offset":null}}`
	case strings.HasSuffix(path, "/points/query"):
		return `{"result":{"points":[]}}`
	case strings.HasSuffix(path, "/points/count"):
		return `{"result":{"count":0}}`
	case strings.HasSuffix(path, "/points/delete"):
		return `{"result":{"status":"completed"}}`
	case strings.HasSuffix(path, "/points") && request.Method == http.MethodPut:
		return `{"result":{"status":"completed"}}`
	case request.Method == http.MethodPut && strings.HasPrefix(path, "/collections/"):
		return `{"result":true}`
	case request.Method == http.MethodGet && strings.HasPrefix(path, "/collections/"):
		return `{"result":{"config":{"params":{"sparse_vectors":{"sparse":{"modifier":"idf"}}}}}}`
	default:
		return `{"result":true}`
	}
}

func jsonObject(t *testing.T, raw string) map[string]any {
	t.Helper()
	var value map[string]any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		t.Fatalf("invalid JSON fixture: %v\n%s", err, raw)
	}
	return value
}

func assertBody(t *testing.T, request qdrantRequest, wantJSON string) {
	t.Helper()
	want := jsonObject(t, wantJSON)
	if !reflect.DeepEqual(request.Body, want) {
		t.Fatalf("body mismatch for %s %s\n got: %s\nwant: %s",
			request.Method, request.Path, prettyJSON(request.Body), prettyJSON(want))
	}
}

func prettyJSON(value any) string {
	encoded, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

func testStore(t *testing.T, server *httptest.Server, mutate func(*StoreConfig)) *Store {
	t.Helper()
	config := StoreConfig{Collection: "knowledge", URL: server.URL, VectorSize: 4}
	if mutate != nil {
		mutate(&config)
	}
	store, err := NewStore(config)
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	return store
}

var (
	denseA   = []float64{1, 0, 0, 0}
	denseB   = []float64{0, 1, 0, 0}
	sparseAB = sparse.SparseEmbedding{
		Indices: []uint32{7, 9},
		Values:  []float64{1.0, 0.5},
	}
)

func TestVersionPointIDsMatchPython(t *testing.T) {
	cases := []struct {
		namespace  string
		recordType string
		itemID     string
		version    string
		documentID string
		want       string
	}{
		{"", "chunk", "chunk-a", "v1", "doc-a", "01a58213-57f8-5e81-87bf-d25385d2d033"},
		{"", "chunk", "chunk-b", "v1", "doc-a", "a38d7848-01da-5ea3-8c03-985ea81466b6"},
		{"", "asset", "asset-1", "v1", "doc-a", "c59e869f-627f-50af-8665-bd3bf91717d2"},
		{"alpha", "chunk", "chunk-a", "v1", "doc-a", "41c7368b-645c-5bf0-808a-6142bdcffdd6"},
		{"alpha", "asset", "asset-1", "v1", "doc-a", "b7ac7607-a81b-594d-8b8f-aec62c7567d2"},
		{"beta", "chunk", "chunk-a", "v1", "doc-a", "4784946f-d682-55eb-912f-c89c52a80c7f"},
		{"", "chunk", `we"ird`, "v1", "文档-😀", "872d243e-2d19-5774-81c2-aa8a4cd1381e"},
	}
	for _, testCase := range cases {
		store := &Store{namespace: testCase.namespace}
		got := store.versionPointID(testCase.recordType, testCase.itemID, testCase.version, testCase.documentID)
		if got != testCase.want {
			t.Errorf("versionPointID(ns=%q,%s,%s,%s,%s) = %s, want %s",
				testCase.namespace, testCase.recordType, testCase.itemID, testCase.version, testCase.documentID, got, testCase.want)
		}
	}
}

func TestLegacyPointIDsMatchPython(t *testing.T) {
	cases := []struct {
		namespace string
		itemID    string
		want      string
	}{
		{"", "chunk-a", "c46ddc8a-7b6b-5750-b838-d34101171949"},
		{"", "asset-1", "0d808c2f-f604-5b70-92fb-2c53876e2907"},
		{"alpha", "chunk-a", "f6a1da9c-386e-5214-83ab-8ec68bb20f26"},
		{"alpha", "asset-1", "ff540e7b-986a-5233-9606-cc702db1357e"},
	}
	for _, testCase := range cases {
		store := &Store{namespace: testCase.namespace}
		if got := store.pointID(testCase.itemID); got != testCase.want {
			t.Errorf("pointID(ns=%q,%s) = %s, want %s", testCase.namespace, testCase.itemID, got, testCase.want)
		}
	}
}

func TestPythonJSONEscaping(t *testing.T) {
	cases := map[string]string{
		"plain":          `"plain"`,
		`we"ird`:         `"we\"ird"`,
		`back\slash`:     `"back\\slash"`,
		"tab\tnewline\n": `"tab\tnewline\n"`,
		"\x01\x1f":       `"\u0001\u001f"`,
		"文档-😀":           `"\u6587\u6863-\ud83d\ude00"`,
		"混合":             `"\u6df7\u5408"`,
		"/slash/":        `"/slash/"`,
		"\x7f":           `"\u007f"`,
	}
	for value, want := range cases {
		if got := pythonJSONString(value); got != want {
			t.Errorf("pythonJSONString(%q) = %s, want %s", value, got, want)
		}
	}
	wantList := `["version", null, "\u6587\u6863-\ud83d\ude00", "v1", "chunk", "we\"ird"]`
	if got := pythonJSONList([]any{"version", nil, "文档-😀", "v1", "chunk", `we"ird`}); got != wantList {
		t.Errorf("pythonJSONList() = %s, want %s", got, wantList)
	}
}

func TestEnsureCollectionCreatesDualVectorSchema(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/exists") {
			return http.StatusOK, `{"result":{"exists":false}}`
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	if err := store.EnsureCollection(context.Background()); err != nil {
		t.Fatalf("EnsureCollection() error: %v", err)
	}
	requests := fake.all()
	if len(requests) != 2 {
		t.Fatalf("expected exists+create requests, got %d: %s", len(requests), prettyJSON(requests))
	}
	if requests[0].Method != http.MethodGet || requests[0].Path != "/collections/knowledge/exists" {
		t.Errorf("first request = %s %s", requests[0].Method, requests[0].Path)
	}
	create := requests[1]
	if create.Method != http.MethodPut || create.Path != "/collections/knowledge" {
		t.Errorf("create request = %s %s", create.Method, create.Path)
	}
	assertBody(t, create, `{
		"vectors": {"dense": {"size": 4, "distance": "Cosine"}},
		"sparse_vectors": {"sparse": {"index": {"on_disk": false}, "modifier": "idf"}}
	}`)
}

func TestEnsureCollectionAcceptsPhase3Schema(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	if err := store.EnsureCollection(context.Background()); err != nil {
		t.Fatalf("EnsureCollection() error: %v", err)
	}
	var creates []qdrantRequest
	for _, request := range fake.all() {
		if request.Method == http.MethodPut {
			creates = append(creates, request)
		}
	}
	if len(creates) != 0 {
		t.Errorf("existing collection must not be recreated: %s", prettyJSON(creates))
	}
}

func TestEnsureCollectionRejectsDenseOnlySchema(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/exists") {
			return http.StatusOK, `{"result":{"exists":true}}`
		}
		if request.Method == http.MethodGet {
			return http.StatusOK, `{"result":{"config":{"params":{"vectors":{"dense":{"size":4}}}}}}`
		}
		return 0, ""
	}
	_, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	err := store.EnsureCollection(context.Background())
	const want = "Qdrant collection 'knowledge' uses the Phase-2 dense-only schema; " +
		"configure a new collection and reindex the preserved source documents"
	if err == nil || err.Error() != want {
		t.Fatalf("EnsureCollection() error = %v, want %q", err, want)
	}
}

func TestStageVersionUsesDeterministicVersionedIDs(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	chunks := []Chunk{
		{DocumentID: "doc-a", ChunkID: "chunk-a", Text: "A 文档", Title: "Title", Source: "a.md",
			Metadata: map[string]any{"kind": "test"}},
		{DocumentID: "doc-a", ChunkID: "chunk-b", Text: "A 文档第二段", Title: "Title", Source: "a.md",
			Metadata: map[string]any{"kind": "test"}},
	}
	assets := []map[string]any{
		{"record_type": "asset", "document_id": "doc-a", "asset_id": "asset-1", "text": "asset text"},
	}
	count, err := store.StageVersion(context.Background(), chunks, [][]float64{denseA, denseB},
		[]sparse.SparseEmbedding{sparseAB, {}}, assets, "v1")
	if err != nil {
		t.Fatalf("StageVersion() error: %v", err)
	}
	if count != 2 {
		t.Errorf("StageVersion() = %d, want 2", count)
	}
	upserts := fake.find(http.MethodPut, "/points")
	if len(upserts) != 1 {
		t.Fatalf("expected one upsert, got %d", len(upserts))
	}
	if upserts[0].Query.Get("wait") != "true" {
		t.Errorf("wait query = %q, want true", upserts[0].Query.Get("wait"))
	}
	assertBody(t, upserts[0], `{
		"points": [
			{
				"id": "01a58213-57f8-5e81-87bf-d25385d2d033",
				"vector": {"dense": [1, 0, 0, 0], "sparse": {"indices": [7, 9], "values": [1, 0.5]}},
				"payload": {
					"record_type": "chunk", "document_id": "doc-a", "chunk_id": "chunk-a",
					"text": "A 文档", "title": "Title", "source": "a.md",
					"metadata": {"kind": "test", "upload_version": "v1"}, "upload_version": "v1"
				}
			},
			{
				"id": "a38d7848-01da-5ea3-8c03-985ea81466b6",
				"vector": {"dense": [0, 1, 0, 0]},
				"payload": {
					"record_type": "chunk", "document_id": "doc-a", "chunk_id": "chunk-b",
					"text": "A 文档第二段", "title": "Title", "source": "a.md",
					"metadata": {"kind": "test", "upload_version": "v1"}, "upload_version": "v1"
				}
			},
			{
				"id": "c59e869f-627f-50af-8665-bd3bf91717d2",
				"vector": {},
				"payload": {"record_type": "asset", "document_id": "doc-a", "asset_id": "asset-1",
					"text": "asset text", "upload_version": "v1"}
			}
		]
	}`)
}

func TestStageVersionAddsNamespaceScope(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, func(config *StoreConfig) { config.Namespace = " alpha " })
	chunks := []Chunk{{DocumentID: "doc-a", ChunkID: "chunk-a", Text: "t", Title: "T", Source: "a.md",
		Metadata: map[string]any{}}}
	assets := []map[string]any{{"document_id": "doc-a", "asset_id": "asset-1", "text": "asset"}}
	if _, err := store.StageVersion(context.Background(), chunks, [][]float64{denseA}, nil, assets, "v1"); err != nil {
		t.Fatalf("StageVersion() error: %v", err)
	}
	upsert := fake.only(t, http.MethodPut, "/points")
	assertBody(t, upsert, `{
		"points": [
			{
				"id": "41c7368b-645c-5bf0-808a-6142bdcffdd6",
				"vector": {"dense": [1, 0, 0, 0]},
				"payload": {"record_type": "chunk", "document_id": "doc-a", "chunk_id": "chunk-a",
					"text": "t", "title": "T", "source": "a.md",
					"metadata": {"upload_version": "v1"}, "upload_version": "v1", "namespace": "alpha"}
			},
			{
				"id": "b7ac7607-a81b-594d-8b8f-aec62c7567d2",
				"vector": {},
				"payload": {"document_id": "doc-a", "asset_id": "asset-1", "text": "asset",
					"record_type": "asset", "upload_version": "v1", "namespace": "alpha"}
			}
		]
	}`)
}

func TestStageVersionValidation(t *testing.T) {
	valid := []Chunk{{DocumentID: "doc-a", ChunkID: "chunk-a", Text: "t", Title: "T", Source: "s",
		Metadata: map[string]any{}}}
	cases := []struct {
		name          string
		chunks        []Chunk
		vectors       [][]float64
		sparseVectors []sparse.SparseEmbedding
		assets        []map[string]any
		version       string
		want          string
	}{
		{"empty version", valid, [][]float64{denseA}, nil, nil, "  ", "version must not be empty"},
		{"chunk vector mismatch", valid, [][]float64{denseA, denseB}, nil, nil, "v1", "Chunk and vector counts must match"},
		{"sparse count mismatch", valid, [][]float64{denseA}, []sparse.SparseEmbedding{{}, {}}, nil, "v1",
			"Chunk and sparse vector counts must match"},
		{"multi document", []Chunk{valid[0], {DocumentID: "doc-b", ChunkID: "chunk-b"}}, [][]float64{denseA, denseB},
			nil, nil, "v1", "stage_version accepts chunks from exactly one document"},
		{"empty document id", []Chunk{{DocumentID: " ", ChunkID: "chunk-a"}}, [][]float64{denseA}, nil, nil, "v1",
			"document_id must not be empty"},
		{"dense dimension", valid, [][]float64{{1, 0, 0}}, nil, nil, "v1", "Dense vector must have 4 dimensions"},
		{"asset wrong document", valid, [][]float64{denseA}, nil,
			[]map[string]any{{"document_id": "doc-b", "asset_id": "asset-1"}}, "v1",
			"Asset payload must belong to the staged document"},
		{"asset missing id", valid, [][]float64{denseA}, nil,
			[]map[string]any{{"document_id": "doc-a", "asset_id": ""}}, "v1",
			"Asset payload must belong to the staged document"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fake, server := newFakeQdrant(t, nil)
			store := testStore(t, server, nil)
			_, err := store.StageVersion(context.Background(), testCase.chunks, testCase.vectors,
				testCase.sparseVectors, testCase.assets, testCase.version)
			if err == nil || err.Error() != testCase.want {
				t.Fatalf("StageVersion() error = %v, want %q", err, testCase.want)
			}
			if fake.count() != 0 {
				t.Errorf("validation must not reach Qdrant, got %d requests", fake.count())
			}
		})
	}
}

func TestStageVersionEmptyChunksIsNoop(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	count, err := store.StageVersion(context.Background(), nil, nil, nil, nil, "v1")
	if err != nil || count != 0 {
		t.Fatalf("StageVersion() = %d, %v", count, err)
	}
	if fake.count() != 0 {
		t.Errorf("empty staging must not reach Qdrant")
	}
}

func TestDenseSearchRequestShape(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/query") {
			return http.StatusOK, `{"result":{"points":[{
				"id":"01a58213-57f8-5e81-87bf-d25385d2d033","version":2,"score":0.42,
				"payload":{"record_type":"chunk","document_id":"doc-a","chunk_id":"chunk-a",
					"text":"证据","title":"Ti","source":"a.md","metadata":{"kind":"test"},"upload_version":"v1"}
			}]}}`
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	points, err := store.DenseSearch(context.Background(), []float64{0.25, 0.5, 0.75, 1}, 5,
		map[string]any{"document_id": "doc-a"})
	if err != nil {
		t.Fatalf("DenseSearch() error: %v", err)
	}
	if len(points) != 1 || points[0].Score != 0.42 || points[0].ID != "01a58213-57f8-5e81-87bf-d25385d2d033" {
		t.Fatalf("points = %s", prettyJSON(points))
	}
	query := fake.only(t, http.MethodPost, "/points/query")
	assertBody(t, query, `{
		"query": [0.25, 0.5, 0.75, 1],
		"using": "dense",
		"filter": {"must": [
			{"key": "document_id", "match": {"value": "doc-a"}},
			{"is_empty": {"key": "namespace"}},
			{"should": [{"must": [{"is_empty": {"key": "upload_version"}}]}]}
		]},
		"limit": 5,
		"with_vector": false,
		"with_payload": true
	}`)
}

func TestDenseSearchDimensionValidation(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	if _, err := store.DenseSearch(context.Background(), []float64{1, 0}, 5, nil); err == nil ||
		err.Error() != "Dense query vector must have 4 dimensions" {
		t.Fatalf("error = %v", err)
	}
	if fake.count() != 0 {
		t.Errorf("dimension validation must not reach Qdrant")
	}
}

func TestSparseSearchRequestShape(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	if _, err := store.SparseSearch(context.Background(), sparseAB, 3, map[string]any{}); err != nil {
		t.Fatalf("SparseSearch() error: %v", err)
	}
	query := fake.only(t, http.MethodPost, "/points/query")
	assertBody(t, query, `{
		"query": {"indices": [7, 9], "values": [1, 0.5]},
		"using": "sparse",
		"filter": {"must": [
			{"is_empty": {"key": "namespace"}},
			{"should": [{"must": [{"is_empty": {"key": "upload_version"}}]}]}
		]},
		"limit": 3,
		"with_vector": false,
		"with_payload": true
	}`)
}

func TestSparseSearchEmptyEmbeddingIsNoop(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	points, err := store.SparseSearch(context.Background(), sparse.SparseEmbedding{}, 5, nil)
	if err != nil || points != nil {
		t.Fatalf("SparseSearch() = %v, %v", points, err)
	}
	if fake.count() != 0 {
		t.Errorf("empty sparse query must not reach Qdrant")
	}
}

func TestHybridSearchUsesServerSideRRF(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	if _, err := store.HybridSearch(context.Background(), denseA, sparseAB, 2,
		map[string]any{"document_id": "doc-a"}); err != nil {
		t.Fatalf("HybridSearch() error: %v", err)
	}
	query := fake.only(t, http.MethodPost, "/points/query")
	filterJSON := `{"must": [
		{"key": "document_id", "match": {"value": "doc-a"}},
		{"is_empty": {"key": "namespace"}},
		{"should": [{"must": [{"is_empty": {"key": "upload_version"}}]}]}
	]}`
	assertBody(t, query, `{
		"prefetch": [
			{"query": [1, 0, 0, 0], "using": "dense", "filter": `+filterJSON+`, "limit": 2},
			{"query": {"indices": [7, 9], "values": [1, 0.5]}, "using": "sparse", "filter": `+filterJSON+`, "limit": 2}
		],
		"query": {"fusion": "rrf"},
		"limit": 2,
		"with_vector": false,
		"with_payload": true
	}`)
}

func TestHybridSearchFallsBackToDenseWithoutSparse(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	if _, err := store.HybridSearch(context.Background(), denseA, sparse.SparseEmbedding{}, 4, nil); err != nil {
		t.Fatalf("HybridSearch() error: %v", err)
	}
	query := fake.only(t, http.MethodPost, "/points/query")
	assertBody(t, query, `{
		"query": [1, 0, 0, 0],
		"using": "dense",
		"filter": {"must": [
			{"is_empty": {"key": "namespace"}},
			{"should": [{"must": [{"is_empty": {"key": "upload_version"}}]}]}
		]},
		"limit": 4,
		"with_vector": false,
		"with_payload": true
	}`)
}

func TestQueryFilterPublicationFollowsActiveVersions(t *testing.T) {
	heads := map[string]string{"Doc": "new", "deleted": "", "fresh": ""}
	store := &Store{
		namespace:      "alpha",
		activeVersions: func() map[string]string { return heads },
	}
	filter := store.queryFilter(map[string]any{"document_id": "Doc"})
	encoded, err := json.Marshal(filter)
	if err != nil {
		t.Fatalf("cannot encode filter: %v", err)
	}
	want := jsonObject(t, `{
		"must": [
			{"key": "document_id", "match": {"value": "Doc"}},
			{"key": "namespace", "match": {"value": "alpha"}},
			{"should": [
				{"must": [
					{"key": "document_id", "match": {"value": "Doc"}},
					{"key": "upload_version", "match": {"value": "new"}}
				]},
				{"must": [{"is_empty": {"key": "upload_version"}}],
				 "must_not": [{"key": "document_id", "match": {"any": ["Doc", "deleted", "fresh"]}}]}
			]}
		]
	}`)
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("cannot decode filter: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("query filter mismatch\n got: %s\nwant: %s", prettyJSON(got), prettyJSON(want))
	}
}

func TestDeleteFiltersMatchPython(t *testing.T) {
	cases := []struct {
		name   string
		call   func(store *Store) error
		method string
		body   string
	}{
		{
			"delete version", func(store *Store) error { return store.DeleteVersion(context.Background(), "Doc", "v1") },
			http.MethodPost,
			`{"filter":{"must":[
				{"key":"document_id","match":{"value":"Doc"}},
				{"key":"namespace","match":{"value":"alpha"}},
				{"key":"upload_version","match":{"value":"v1"}}
			]}}`,
		},
		{
			"delete legacy", func(store *Store) error { return store.DeleteLegacy(context.Background(), "Doc") },
			http.MethodPost,
			`{"filter":{"must":[
				{"key":"document_id","match":{"value":"Doc"}},
				{"key":"namespace","match":{"value":"alpha"}},
				{"is_empty":{"key":"upload_version"}}
			]}}`,
		},
		{
			"remove old versions", func(store *Store) error {
				return store.RemoveOldVersions(context.Background(), "Doc", "keep")
			},
			http.MethodPost,
			`{"filter":{"must":[
				{"key":"document_id","match":{"value":"Doc"}},
				{"key":"namespace","match":{"value":"alpha"}}
			],"must_not":[{"key":"upload_version","match":{"value":"keep"}}]}}`,
		},
		{
			"delete document", func(store *Store) error { return store.DeleteDocument(context.Background(), "Doc") },
			http.MethodPost,
			`{"filter":{"must":[
				{"key":"document_id","match":{"value":"Doc"}},
				{"key":"namespace","match":{"value":"alpha"}}
			]}}`,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			fakeDelete, serverDelete := newFakeQdrant(t, nil)
			storeDelete := testStore(t, serverDelete, func(config *StoreConfig) { config.Namespace = "alpha" })
			if err := testCase.call(storeDelete); err != nil {
				t.Fatalf("%s error: %v", testCase.name, err)
			}
			deletes := fakeDelete.find(http.MethodPost, "/points/delete")
			if len(deletes) != 1 {
				t.Fatalf("%s issued %d delete requests", testCase.name, len(deletes))
			}
			if deletes[0].Query.Get("wait") != "true" {
				t.Errorf("wait query = %q", deletes[0].Query.Get("wait"))
			}
			assertBody(t, deletes[0], testCase.body)
		})
	}
}

func TestDeleteOnMissingCollectionIsNoop(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/exists") {
			return http.StatusOK, `{"result":{"exists":false}}`
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	for _, call := range []func() error{
		func() error { return store.DeleteVersion(context.Background(), "Doc", "v1") },
		func() error { return store.DeleteLegacy(context.Background(), "Doc") },
		func() error { return store.RemoveOldVersions(context.Background(), "Doc", "keep") },
	} {
		if err := call(); err != nil {
			t.Fatalf("missing collection delete error: %v", err)
		}
	}
	if len(fake.find(http.MethodPost, "/points/delete")) != 0 {
		t.Errorf("missing collection must not receive deletes")
	}
}

func TestDeleteValidation(t *testing.T) {
	_, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	cases := []struct {
		name string
		call func() error
		want string
	}{
		{"delete version", func() error { return store.DeleteVersion(context.Background(), "", "v1") },
			"document_id and version must not be empty"},
		{"delete legacy", func() error { return store.DeleteLegacy(context.Background(), " ") },
			"document_id must not be empty"},
		{"remove old", func() error { return store.RemoveOldVersions(context.Background(), "Doc", "") },
			"document_id and keep_version must not be empty"},
		{"delete document", func() error { return store.DeleteDocument(context.Background(), "") },
			"document_id must not be empty"},
	}
	for _, testCase := range cases {
		if err := testCase.call(); err == nil || err.Error() != testCase.want {
			t.Errorf("%s error = %v, want %q", testCase.name, err, testCase.want)
		}
	}
}

func TestGetAssetsFiltersAndResolvesNamespace(t *testing.T) {
	scrollBody := `{"result":{"points":[
		{"id":"11111111-1111-5111-8111-111111111111",
		 "payload":{"record_type":"asset","asset_id":"asset-1","document_id":"Doc","text":"new",
			"upload_version":"new","namespace":"alpha"}},
		{"id":"22222222-2222-5222-8222-222222222222",
		 "payload":{"record_type":"asset","asset_id":"asset-1","document_id":"Doc","text":"legacy",
			"namespace":"alpha"}},
		{"id":"33333333-3333-5333-8333-333333333333",
		 "payload":{"record_type":"chunk","asset_id":"asset-chunk","document_id":"Doc"}},
		{"id":"44444444-4444-5444-8444-444444444444",
		 "payload":{"record_type":"asset","asset_id":"asset-foreign","document_id":"Foreign","namespace":"beta"}},
		{"id":"55555555-5555-5555-8555-555555555555",
		 "payload":{"record_type":"asset","asset_id":"asset-none","document_id":"Old"}}
	],"next_page_offset":null}}`
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/scroll") {
			return http.StatusOK, scrollBody
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, func(config *StoreConfig) { config.Namespace = "alpha" })
	assets, err := store.GetAssets(context.Background(), []string{"asset-1", "asset-1"})
	if err != nil {
		t.Fatalf("GetAssets() error: %v", err)
	}
	if len(assets) != 1 {
		t.Fatalf("assets = %s", prettyJSON(assets))
	}
	if assets["asset-1"]["text"] != "legacy" {
		t.Errorf("asset text = %v, want last-scrolled legacy value", assets["asset-1"]["text"])
	}
	scroll := fake.only(t, http.MethodPost, "/points/scroll")
	assertBody(t, scroll, `{
		"limit": 256,
		"filter": {"must": [
			{"key": "namespace", "match": {"value": "alpha"}},
			{"key": "record_type", "match": {"value": "asset"}},
			{"should": [{"must": [{"is_empty": {"key": "upload_version"}}]}]},
			{"key": "asset_id", "match": {"any": ["asset-1"]}}
		]},
		"with_payload": true,
		"with_vector": false
	}`)
}

func TestGetAssetsEmptyIsNoopAndPaginates(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	assets, err := store.GetAssets(context.Background(), nil)
	if err != nil || len(assets) != 0 {
		t.Fatalf("GetAssets(nil) = %v, %v", assets, err)
	}
	if fake.count() != 0 {
		t.Errorf("empty asset lookup must not reach Qdrant")
	}

	handler := func(request qdrantRequest) (int, string) {
		if !strings.HasSuffix(request.Path, "/points/scroll") {
			return 0, ""
		}
		if request.Body["offset"] == nil {
			return http.StatusOK, `{"result":{"points":[
				{"id":"11111111-1111-5111-8111-111111111111","payload":{"record_type":"asset","asset_id":"a1","document_id":"D"}}
			],"next_page_offset":"11111111-1111-5111-8111-111111111111"}}`
		}
		return http.StatusOK, `{"result":{"points":[
			{"id":"22222222-2222-5222-8222-222222222222","payload":{"record_type":"asset","asset_id":"a2","document_id":"D"}}
		],"next_page_offset":null}}`
	}
	fakePaged, serverPaged := newFakeQdrant(t, handler)
	pagedStore := testStore(t, serverPaged, nil)
	assets, err = pagedStore.GetAssets(context.Background(), []string{"a1", "a2"})
	if err != nil {
		t.Fatalf("GetAssets() error: %v", err)
	}
	if len(assets) != 2 {
		t.Fatalf("assets = %s", prettyJSON(assets))
	}
	scrolls := fakePaged.find(http.MethodPost, "/points/scroll")
	if len(scrolls) != 2 {
		t.Fatalf("expected two scroll pages, got %d", len(scrolls))
	}
	if scrolls[1].Body["offset"] != "11111111-1111-5111-8111-111111111111" {
		t.Errorf("second page offset = %v", scrolls[1].Body["offset"])
	}
}

func TestReplaceDocumentWritesLegacyIDsAndDeletesStale(t *testing.T) {
	scrollBody := `{"result":{"points":[
		{"id":"c46ddc8a-7b6b-5750-b838-d34101171949",
		 "payload":{"record_type":"chunk","document_id":"doc-a","chunk_id":"chunk-a"},
		 "vector":{"dense":[0,0,0,1]}},
		{"id":"11111111-1111-5111-8111-111111111111",
		 "payload":{"record_type":"chunk","document_id":"doc-a","chunk_id":"old-chunk"},
		 "vector":{"dense":[0,0,0,1]}}
	],"next_page_offset":null}}`
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/scroll") {
			return http.StatusOK, scrollBody
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	chunks := []Chunk{{DocumentID: "doc-a", ChunkID: "chunk-a", Text: "new text", Title: "Title",
		Source: "a.md", Metadata: map[string]any{"kind": "test"}}}
	assets := []map[string]any{{"record_type": "asset", "document_id": "doc-a", "asset_id": "asset-1", "text": "asset"}}
	afterWriteCalled := false
	count, err := store.ReplaceDocument(context.Background(), chunks, [][]float64{denseA},
		[]sparse.SparseEmbedding{sparseAB}, assets, func() error {
			afterWriteCalled = true
			return nil
		})
	if err != nil {
		t.Fatalf("ReplaceDocument() error: %v", err)
	}
	if count != 1 || !afterWriteCalled {
		t.Fatalf("ReplaceDocument() = %d, afterWrite=%v", count, afterWriteCalled)
	}
	upsert := fake.only(t, http.MethodPut, "/points")
	assertBody(t, upsert, `{"points":[
		{"id":"c46ddc8a-7b6b-5750-b838-d34101171949",
		 "vector":{"dense":[1,0,0,0],"sparse":{"indices":[7,9],"values":[1,0.5]}},
		 "payload":{"record_type":"chunk","document_id":"doc-a","chunk_id":"chunk-a",
			"text":"new text","title":"Title","source":"a.md","metadata":{"kind":"test"}}},
		{"id":"0d808c2f-f604-5b70-92fb-2c53876e2907","vector":{},
		 "payload":{"record_type":"asset","document_id":"doc-a","asset_id":"asset-1","text":"asset"}}
	]}`)
	deletes := fake.find(http.MethodPost, "/points/delete")
	if len(deletes) != 1 {
		t.Fatalf("expected stale delete, got %d", len(deletes))
	}
	assertBody(t, deletes[0], `{"points":["11111111-1111-5111-8111-111111111111"]}`)
}

func TestReplaceDocumentRestoresOnAfterWriteFailure(t *testing.T) {
	scrollBody := `{"result":{"points":[
		{"id":"c46ddc8a-7b6b-5750-b838-d34101171949",
		 "payload":{"record_type":"chunk","document_id":"doc-a","chunk_id":"chunk-a"},
		 "vector":{"dense":[0,0,0,1]}}
	],"next_page_offset":null}}`
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/scroll") {
			return http.StatusOK, scrollBody
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	chunks := []Chunk{{DocumentID: "doc-a", ChunkID: "chunk-a", Text: "new", Title: "T", Source: "s",
		Metadata: map[string]any{}}}
	writeErr := fmt.Errorf("publication failed")
	_, err := store.ReplaceDocument(context.Background(), chunks, [][]float64{denseA}, nil, nil,
		func() error { return writeErr })
	if err == nil || err.Error() != "publication failed" {
		t.Fatalf("ReplaceDocument() error = %v, want publication failed", err)
	}
	upserts := fake.find(http.MethodPut, "/points")
	if len(upserts) != 2 {
		t.Fatalf("expected write+restore upserts, got %d", len(upserts))
	}
	assertBody(t, upserts[1], `{"points":[
		{"id":"c46ddc8a-7b6b-5750-b838-d34101171949",
		 "vector":{"dense":[0,0,0,1]},
		 "payload":{"record_type":"chunk","document_id":"doc-a","chunk_id":"chunk-a"}}
	]}`)
}

func TestReplaceDocumentReportsRestoreFailure(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/scroll") {
			return http.StatusOK, `{"result":{"points":[
				{"id":"c46ddc8a-7b6b-5750-b838-d34101171949","payload":{"document_id":"doc-a"},"vector":{}}
			],"next_page_offset":null}}`
		}
		if strings.HasSuffix(request.Path, "/points") && request.Method == http.MethodPut {
			return http.StatusInternalServerError, `{"status":{"error":"boom"}}`
		}
		return 0, ""
	}
	_, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	chunks := []Chunk{{DocumentID: "doc-a", ChunkID: "chunk-a", Text: "new", Title: "T", Source: "s",
		Metadata: map[string]any{}}}
	_, err := store.ReplaceDocument(context.Background(), chunks, [][]float64{denseA}, nil, nil, nil)
	if err == nil || !strings.Contains(err.Error(), "previous index could not be restored") {
		t.Fatalf("ReplaceDocument() error = %v", err)
	}
}

func TestReplaceDocumentValidation(t *testing.T) {
	fake, server := newFakeQdrant(t, nil)
	store := testStore(t, server, nil)
	chunk := Chunk{DocumentID: "doc-a", ChunkID: "chunk-a"}
	cases := []struct {
		name    string
		chunks  []Chunk
		vectors [][]float64
		want    string
	}{
		{"vector mismatch", []Chunk{chunk}, nil, "Chunk and vector counts must match"},
		{"multi document", []Chunk{chunk, {DocumentID: "doc-b", ChunkID: "chunk-b"}},
			[][]float64{denseA, denseA}, "replace_document accepts chunks from exactly one document"},
		{"dimension", []Chunk{chunk}, [][]float64{{1, 0}}, "Dense vector must have 4 dimensions"},
	}
	for _, testCase := range cases {
		_, err := store.ReplaceDocument(context.Background(), testCase.chunks, testCase.vectors, nil, nil, nil)
		if err == nil || err.Error() != testCase.want {
			t.Errorf("%s error = %v, want %q", testCase.name, err, testCase.want)
		}
	}
	if fake.count() != 0 {
		t.Errorf("validation must not reach Qdrant")
	}
	if count, err := store.ReplaceDocument(context.Background(), nil, nil, nil, nil, nil); err != nil || count != 0 {
		t.Errorf("empty replace = %v, %v", count, err)
	}
}

type fakeEmbedder struct {
	vectorSize int
	texts      []string
}

func (f *fakeEmbedder) VectorSize() int { return f.vectorSize }

func (f *fakeEmbedder) EmbedDocuments(_ context.Context, texts []string) ([][]float64, error) {
	f.texts = append(f.texts, texts...)
	vectors := make([][]float64, len(texts))
	for index := range texts {
		vectors[index] = []float64{0, 1, 0, 0}
	}
	return vectors, nil
}

func TestReembedIntoCopiesVisiblePointsAndPreservesSparse(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		switch {
		case strings.HasSuffix(request.Path, "/exists") && strings.Contains(request.Path, "/target/"):
			return http.StatusOK, `{"result":{"exists":false}}`
		case strings.HasSuffix(request.Path, "/exists"):
			return http.StatusOK, `{"result":{"exists":true}}`
		case strings.HasSuffix(request.Path, "/points/scroll"):
			if request.Body["offset"] == nil {
				return http.StatusOK, `{"result":{"points":[
					{"id":"11111111-1111-5111-8111-111111111111",
					 "payload":{"record_type":"chunk","document_id":"Doc","text":"active","upload_version":"active"},
					 "vector":{"dense":[0,0,0,1],"sparse":{"indices":[7],"values":[1.0]}}},
					{"id":"22222222-2222-5222-8222-222222222222",
					 "payload":{"record_type":"asset","asset_id":"a1","document_id":"Doc","text":"asset","upload_version":"active"},
					 "vector":{}}
				],"next_page_offset":"11111111-1111-5111-8111-111111111111"}}`
			}
			return http.StatusOK, `{"result":{"points":[
				{"id":"33333333-3333-5333-8333-333333333333",
				 "payload":{"record_type":"chunk","document_id":"doc","text":"legacy-doc"},
				 "vector":{"dense":[0,0,0,1]}}
			],"next_page_offset":null}}`
		default:
			return 0, ""
		}
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	embedder := &fakeEmbedder{vectorSize: 4}
	if err := store.ReembedInto(context.Background(), "target", embedder,
		map[string]int{"Doc": 1, "doc": 1}); err != nil {
		t.Fatalf("ReembedInto() error: %v", err)
	}
	if !reflect.DeepEqual(embedder.texts, []string{"active", "legacy-doc"}) {
		t.Errorf("embedded texts = %v", embedder.texts)
	}
	creates := fake.find(http.MethodPut, "/collections/target")
	if len(creates) != 1 {
		t.Fatalf("expected target collection creation, got %d", len(creates))
	}
	assertBody(t, creates[0], `{
		"vectors": {"dense": {"size": 4, "distance": "Cosine"}},
		"sparse_vectors": {"sparse": {"index": {"on_disk": false}, "modifier": "idf"}}
	}`)
	targetUpserts := fake.find(http.MethodPut, "/collections/target/points")
	if len(targetUpserts) != 2 {
		t.Fatalf("expected two target upserts, got %d", len(targetUpserts))
	}
	assertBody(t, targetUpserts[0], `{"points":[
		{"id":"11111111-1111-5111-8111-111111111111",
		 "vector":{"dense":[0,1,0,0],"sparse":{"indices":[7],"values":[1.0]}},
		 "payload":{"record_type":"chunk","document_id":"Doc","text":"active","upload_version":"active"}},
		{"id":"22222222-2222-5222-8222-222222222222",
		 "vector":{},
		 "payload":{"record_type":"asset","asset_id":"a1","document_id":"Doc","text":"asset","upload_version":"active"}}
	]}`)
}

func TestReembedIntoRejectsChunkCountMismatch(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/exists") && strings.Contains(request.Path, "/target/") {
			return http.StatusOK, `{"result":{"exists":false}}`
		}
		if strings.HasSuffix(request.Path, "/points/scroll") {
			return http.StatusOK, `{"result":{"points":[
				{"id":"11111111-1111-5111-8111-111111111111",
				 "payload":{"record_type":"chunk","document_id":"Doc","text":"active"},
				 "vector":{"dense":[0,0,0,1]}}
			],"next_page_offset":null}}`
		}
		return 0, ""
	}
	_, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	err := store.ReembedInto(context.Background(), "target", &fakeEmbedder{vectorSize: 4}, map[string]int{"Doc": 2})
	if err == nil || !strings.Contains(err.Error(), "片段数量") {
		t.Fatalf("ReembedInto() error = %v", err)
	}
}

func TestNewStoreValidation(t *testing.T) {
	if _, err := NewStore(StoreConfig{Collection: "c", URL: "http://q", Path: "local"}); err == nil ||
		err.Error() != "Set only one of url and path" {
		t.Errorf("url+path error = %v", err)
	}
	if _, err := NewStore(StoreConfig{Collection: "c", Path: "local"}); err == nil ||
		err != ErrLocalPathUnsupported {
		t.Errorf("path-only error = %v", err)
	}
	store, err := NewStore(StoreConfig{Collection: "c", URL: "http://q/"})
	if err != nil {
		t.Fatalf("NewStore() error: %v", err)
	}
	if store.VectorSize() != 1024 || store.Collection() != "c" || store.Namespace() != "" {
		t.Errorf("defaults = %d %q %q", store.VectorSize(), store.Collection(), store.Namespace())
	}
	if store.client.BaseURL() != "http://q" {
		t.Errorf("base URL = %q", store.client.BaseURL())
	}
}

func TestHTTPErrorPropagates(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		return http.StatusInternalServerError, `{"status":{"error":"boom"}}`
	}
	_, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	err := store.EnsureCollection(context.Background())
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") {
		t.Fatalf("EnsureCollection() error = %v", err)
	}
}

func TestCount(t *testing.T) {
	handler := func(request qdrantRequest) (int, string) {
		if strings.HasSuffix(request.Path, "/points/count") {
			return http.StatusOK, `{"result":{"count":7}}`
		}
		return 0, ""
	}
	fake, server := newFakeQdrant(t, handler)
	store := testStore(t, server, nil)
	count, err := store.Count(context.Background(), true)
	if err != nil || count != 7 {
		t.Fatalf("Count() = %d, %v", count, err)
	}
	assertBody(t, fake.only(t, http.MethodPost, "/points/count"), `{"exact": true}`)
}

func TestPointIDUnmarshal(t *testing.T) {
	var point Point
	if err := json.Unmarshal([]byte(`{"id":"abc","score":0.5,"payload":{}}`), &point); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if point.ID.String() != "abc" || point.Score != 0.5 {
		t.Errorf("point = %+v", point)
	}
	if err := json.Unmarshal([]byte(`{"id":17}`), &point); err != nil {
		t.Fatalf("unmarshal numeric id: %v", err)
	}
	if point.ID.String() != "17" {
		t.Errorf("numeric id = %q", point.ID.String())
	}
}
