package searchrefs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

const testNamespace = "manuals"

func newTestStores(t *testing.T) *state.Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

// useRedis sets a miniredis-backed transient store and returns the server.
func useRedis(t *testing.T, stores *state.Stores) *miniredis.Miniredis {
	t.Helper()
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	settings := state.DefaultCacheSettings()
	settings.URL = "redis://" + server.Addr() + "/0"
	backend, err := redisstate.New(settings, redisstate.WithRedisClient(client))
	if err != nil {
		t.Fatalf("redisstate.New: %v", err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	stores.Results.Transient = backend.TransientStore()
	return server
}

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func addDocument(t *testing.T, stores *state.Stores, namespace, name, documentID string) {
	t.Helper()
	if err := stores.Documents.Upsert(context.Background(), namespace, state.DocumentRecord{
		Name:       name,
		DocumentID: documentID,
		Size:       1,
		SHA256:     strings.Repeat("0", 64),
	}); err != nil {
		t.Fatalf("add document %s: %v", name, err)
	}
}

func addBlob(t *testing.T, stores *state.Stores, namespace, bucket, key string, content []byte, documentID string) {
	t.Helper()
	owner := documentID
	if err := stores.Blobs.Put(context.Background(), namespace, bucket, key, content, &owner); err != nil {
		t.Fatalf("add blob %s: %v", key, err)
	}
}

func toResultList(hits ...map[string]any) []any {
	results := make([]any, 0, len(hits))
	for _, hit := range hits {
		results = append(results, hit)
	}
	return results
}

func capturePayload(hits ...map[string]any) map[string]any {
	return map[string]any{
		"query":           "manual",
		"evidence_status": "found",
		"results":         toResultList(hits...),
	}
}

func hitFor(documentID string, extra map[string]any) map[string]any {
	hit := map[string]any{
		"document_id": documentID,
		"chunk_id":    "chunk-" + documentID,
		"text":        "evidence text",
		"metadata":    map[string]any{"title": "Manual"},
	}
	for key, value := range extra {
		hit[key] = value
	}
	return hit
}

func artifactsOf(t *testing.T, payload map[string]any) []map[string]any {
	t.Helper()
	raw, ok := payload["artifacts"]
	if !ok {
		t.Fatalf("missing artifacts: %#v", payload)
	}
	switch typed := raw.(type) {
	case []map[string]any:
		return typed
	case []any:
		items := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			value, ok := item.(map[string]any)
			if !ok {
				t.Fatalf("artifact is not an object: %#v", item)
			}
			items = append(items, value)
		}
		return items
	default:
		t.Fatalf("artifacts type = %T", raw)
		return nil
	}
}

func hitArtifacts(t *testing.T, hit map[string]any) []map[string]any {
	t.Helper()
	switch typed := hit["artifacts"].(type) {
	case []map[string]any:
		return typed
	case []any:
		items := make([]map[string]any, 0, len(typed))
		for _, item := range typed {
			items = append(items, item.(map[string]any))
		}
		return items
	default:
		t.Fatalf("hit artifacts type = %T", hit["artifacts"])
		return nil
	}
}

func assertNumeric(t *testing.T, value any) {
	t.Helper()
	switch value.(type) {
	case json.Number, float64, int:
		return
	default:
		t.Fatalf("value is not numeric: %#v", value)
	}
}

func TestCaptureRegistersSourceAndSnapshot(t *testing.T) {
	stores := newTestStores(t)
	server := useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("hello source"), "doc-1")

	payload := capturePayload(hitFor("doc-1", nil))
	capture := New(stores, testLogger())
	result, err := capture.Capture(ctx, testNamespace, payload, true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if result["snapshot_available"] != true {
		t.Fatalf("snapshot_available = %#v", result["snapshot_available"])
	}
	resultID, _ := result["result_id"].(string)
	if len(resultID) != 36 {
		t.Fatalf("result_id = %q", resultID)
	}
	if rawTTL := server.TTL(transientKeyForTest(stores, resultID)); rawTTL <= 0 {
		t.Fatalf("snapshot key has no TTL")
	}

	items := artifactsOf(t, result)
	if len(items) != 1 {
		t.Fatalf("artifacts = %#v", items)
	}
	source := items[0]
	if source["kind"] != "source" || source["name"] != "manual.txt" || source["mime_type"] != "text/plain" {
		t.Fatalf("source artifact = %#v", source)
	}
	if source["version"] != nil {
		t.Fatalf("source version = %#v", source["version"])
	}
	artifactID, _ := source["artifact_id"].(string)
	if artifactID == "" {
		t.Fatalf("missing artifact id")
	}
	if !strings.HasPrefix(source["resource_uri"].(string), "rag://artifacts/manuals/") {
		t.Fatalf("resource_uri = %#v", source["resource_uri"])
	}
	if numberOf(source["expires_at"]) == nil {
		t.Fatalf("missing expires_at")
	}
	hits := result["results"].([]any)
	if len(hitArtifacts(t, hits[0].(map[string]any))) != 1 {
		t.Fatalf("hit artifacts = %#v", hits[0])
	}

	// Artifact bytes are readable and the snapshot is pageable.
	artifact, content, err := stores.Artifacts.Read(ctx, testNamespace, artifactID)
	if err != nil || string(content) != "hello source" {
		t.Fatalf("artifact read = (%#v, %q, %v)", artifact, content, err)
	}
	stored, err := stores.Results.Get(testNamespace, resultID, 1, 10)
	if err != nil {
		t.Fatalf("snapshot Get: %v", err)
	}
	if stored["total_results"] != 1 || stored["snapshot_available"] != true {
		t.Fatalf("stored snapshot = %#v", stored)
	}

	// The input payload must not be mutated.
	originalHit := payload["results"].([]any)[0].(map[string]any)
	if _, mutated := originalHit["artifacts"]; mutated {
		t.Fatalf("Capture mutated the input payload")
	}
}

func TestCaptureProjectsImageAndTableAssets(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")

	imageData := base64.StdEncoding.EncodeToString([]byte("png-bytes"))
	imageHit := hitFor("doc-1", map[string]any{
		"metadata": map[string]any{"asset_id": "img-1"},
		"asset": map[string]any{
			"type": "image", "mime_type": "image/png", "data": imageData,
		},
	})
	tableHit := hitFor("doc-1", map[string]any{
		"metadata": map[string]any{"asset_id": "table-1"},
		"asset": map[string]any{
			"type": "table", "caption": "Rows", "headers": []any{"a", "b"},
		},
	})
	capture := New(stores, testLogger())
	result, err := capture.Capture(ctx, testNamespace, capturePayload(imageHit, tableHit), false)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if _, hasSnapshot := result["result_id"]; hasSnapshot {
		t.Fatalf("saveSnapshot=false must not save a snapshot")
	}
	items := artifactsOf(t, result)
	kinds := map[string]map[string]any{}
	for _, item := range items {
		kinds[item["kind"].(string)] = item
	}
	image := kinds["image"]
	if image == nil || image["mime_type"] != "image/png" || image["name"] != "img-1" {
		t.Fatalf("image artifact = %#v", image)
	}
	table := kinds["table"]
	if table == nil || table["mime_type"] != "application/json" || table["name"] != "table-1" {
		t.Fatalf("table artifact = %#v", table)
	}
	// The inline data stays in the payload for MCP ImageContent rendering.
	imageAsset := result["results"].([]any)[0].(map[string]any)["asset"].(map[string]any)
	if imageAsset["data"] != imageData {
		t.Fatalf("image data was removed from the payload")
	}
	if imageAsset["artifact_id"] != image["artifact_id"] || imageAsset["resource_uri"] != image["resource_uri"] {
		t.Fatalf("image asset refs = %#v", imageAsset)
	}
	// The stored table artifact is the pre-reference table JSON.
	_, content, err := stores.Artifacts.Read(ctx, testNamespace, table["artifact_id"].(string))
	if err != nil {
		t.Fatalf("table artifact read: %v", err)
	}
	var tablePayload map[string]any
	if err := json.Unmarshal(content, &tablePayload); err != nil {
		t.Fatalf("table artifact is not JSON: %v", err)
	}
	if tablePayload["type"] != "table" || tablePayload["artifact_id"] != nil {
		t.Fatalf("table artifact payload = %#v", tablePayload)
	}
}

func TestCaptureDeduplicatesPerDocument(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")

	first := hitFor("doc-1", map[string]any{"chunk_id": "chunk-1"})
	second := hitFor("doc-1", map[string]any{"chunk_id": "chunk-2"})
	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(first, second), true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if count := len(artifactsOf(t, result)); count != 1 {
		t.Fatalf("artifacts = %d, want 1 (deduplicated)", count)
	}
	for _, item := range result["results"].([]any) {
		hit := item.(map[string]any)
		if count := len(hitArtifacts(t, hit)); count != 1 {
			t.Fatalf("hit artifacts = %d, want 1", count)
		}
	}
}

func TestCapturePdfSideCarsAndLimits(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.pdf", "doc-pdf")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.pdf", []byte("%PDF"), "doc-pdf")
	addBlob(t, stores, testNamespace, state.PDFAssetBucket, "doc-pdf/document.md", []byte("# page"), "doc-pdf")
	addBlob(t, stores, testNamespace, state.PDFAssetBucket, "doc-pdf/manifest.json", []byte("{}"), "doc-pdf")

	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(hitFor("doc-pdf", nil)), true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	items := artifactsOf(t, result)
	if len(items) != 3 {
		t.Fatalf("artifacts = %#v", items)
	}
	kinds := map[string]string{}
	for _, item := range items {
		kinds[item["kind"].(string)] = item["mime_type"].(string)
	}
	if kinds["source"] != "application/pdf" || kinds["markdown"] != "text/markdown" || kinds["manifest"] != "application/json" {
		t.Fatalf("artifact kinds = %#v", kinds)
	}
	if count, ok := numericInt(result["artifact_limits"].(map[string]any)["max_count"]); !ok || count != stores.Results.Settings.MaxArtifactsPerSearch {
		t.Fatalf("artifact limits = %#v", result["artifact_limits"])
	}
}

func TestCaptureArtifactLimitWarnsAndKeepsResults(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	stores.Results.Settings.MaxArtifactsPerSearch = 1
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")
	imageData := base64.StdEncoding.EncodeToString([]byte("png"))
	hit := hitFor("doc-1", map[string]any{
		"asset": map[string]any{"type": "image", "mime_type": "image/png", "data": imageData},
	})
	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(hit), true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if result["artifact_warning"] != "Some artifact references could not be stored" {
		t.Fatalf("artifact_warning = %#v", result["artifact_warning"])
	}
	if count := len(artifactsOf(t, result)); count != 1 {
		t.Fatalf("artifacts = %d, want 1", count)
	}
	asset := result["results"].([]any)[0].(map[string]any)["asset"].(map[string]any)
	if _, referenced := asset["artifact_id"]; referenced {
		t.Fatalf("limited asset must stay unreferenced: %#v", asset)
	}
	if asset["data"] != imageData {
		t.Fatalf("results were discarded on limit: %#v", asset)
	}
}

func TestCaptureArtifactRegistryFailureIsTolerated(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	zero := int64(0)
	failing, err := state.NewArtifactRegistry(stores, 3600, nil, &zero)
	if err != nil {
		t.Fatalf("NewArtifactRegistry: %v", err)
	}
	stores.Artifacts = failing
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")

	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(hitFor("doc-1", nil)), true)
	if err != nil {
		t.Fatalf("artifact failures must not fail capture: %v", err)
	}
	if result["snapshot_available"] != true {
		t.Fatalf("retrieval results/snapshot must survive artifact failure: %#v", result)
	}
	if len(artifactsOf(t, result)) != 0 || result["artifact_warning"] == nil {
		t.Fatalf("expected an artifact warning: %#v", result)
	}
	if len(result["results"].([]any)) != 1 {
		t.Fatalf("retrieval results were discarded")
	}
}

type failingTransient struct{ err error }

func (f failingTransient) Get(string, string) (any, bool, error) { return nil, false, f.err }
func (f failingTransient) Put(string, string, any, float64, bool) (bool, error) {
	return false, f.err
}
func (f failingTransient) Delete(string, string) (int64, error) { return 0, f.err }

func TestCaptureSnapshotFailureDegrades(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	stores.Results.Transient = failingTransient{err: errors.New("redis down")}
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")

	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(hitFor("doc-1", nil)), true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if result["snapshot_available"] != false {
		t.Fatalf("snapshot_available = %#v", result["snapshot_available"])
	}
	if result["snapshot_error"] != MessageSnapshotUnavailable {
		t.Fatalf("snapshot_error = %#v", result["snapshot_error"])
	}
	if len(artifactsOf(t, result)) != 1 {
		t.Fatalf("artifacts must survive snapshot failure: %#v", result)
	}
	if len(result["results"].([]any)) != 1 {
		t.Fatalf("results must survive snapshot failure")
	}
}

func TestCaptureUnexpectedFailureReturnsOriginalPayload(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")

	payload := capturePayload(hitFor("doc-1", map[string]any{
		"asset": map[string]any{"type": "image", "mime_type": "image/png", "data": "not-base64!"},
	}))
	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, payload, true)
	if err != nil {
		t.Fatalf("Capture must absorb unexpected failures: %v", err)
	}
	if result["snapshot_available"] != false || result["snapshot_error"] != MessageSearchResultReferencesUnavailable {
		t.Fatalf("degraded result = %#v", result)
	}
	degradedHits := result["results"].([]any)
	if _, projected := degradedHits[0].(map[string]any)["artifacts"]; projected {
		t.Fatalf("degraded result must be the original payload")
	}
}

func TestCaptureLegacyHitWithoutVersionIsNotLinked(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")
	if _, err := stores.DB.ExecContext(ctx,
		"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
		testNamespace, "doc-1", "op-1", 1.0); err != nil {
		t.Fatalf("insert head: %v", err)
	}

	imageData := base64.StdEncoding.EncodeToString([]byte("png"))
	hit := hitFor("doc-1", map[string]any{
		"asset": map[string]any{"type": "image", "mime_type": "image/png", "data": imageData},
	})
	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(hit), true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if len(artifactsOf(t, result)) != 0 {
		t.Fatalf("legacy hit must not link a newer source version: %#v", result["artifacts"])
	}
	hitResult := result["results"].([]any)[0].(map[string]any)
	if _, referenced := hitResult["asset"].(map[string]any)["artifact_id"]; referenced {
		t.Fatalf("legacy hit asset must not be referenced")
	}
}

func TestCaptureSnapshotPagingHandoff(t *testing.T) {
	stores := newTestStores(t)
	useRedis(t, stores)
	ctx := context.Background()
	addDocument(t, stores, testNamespace, "manual.txt", "doc-1")
	addBlob(t, stores, testNamespace, state.SourceBucket, "manual.txt", []byte("source"), "doc-1")
	hits := make([]map[string]any, 0, 15)
	for index := 0; index < 15; index++ {
		hits = append(hits, hitFor("doc-1", map[string]any{"chunk_id": "chunk-" + string(rune('a'+index))}))
	}
	result, err := New(stores, testLogger()).Capture(ctx, testNamespace, capturePayload(hits...), true)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	resultID := result["result_id"].(string)
	first, err := stores.Results.Get(testNamespace, resultID, 1, 10)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if first["total_results"] != 15 || first["next_page"] != 2 {
		t.Fatalf("page 1 = %#v", first)
	}
	second, err := stores.Results.Get(testNamespace, resultID, 2, 10)
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if len(second["results"].([]any)) != 5 || second["next_page"] != nil {
		t.Fatalf("page 2 = %#v", second)
	}
}

// transientKeyForTest reconstructs the Redis key used by the snapshot handoff.
func transientKeyForTest(stores *state.Stores, resultID string) string {
	_ = stores
	return "rag:state:v1:transient:{" + digestForTest("search-result", resultID) + "}"
}

func digestForTest(kind, identity string) string {
	encoded, _ := json.Marshal([]string{kind, identity})
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func numericInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	case json.Number:
		parsed, err := typed.Int64()
		if err == nil {
			return int(parsed), true
		}
	}
	return 0, false
}

func numberOf(value any) any {
	switch typed := value.(type) {
	case json.Number:
		return typed.String()
	case float64:
		return typed
	case int:
		return typed
	default:
		return nil
	}
}
