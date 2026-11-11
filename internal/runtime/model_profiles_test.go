package runtime

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

var modelProfileIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// modelProfileDraft builds one provider draft for the fake model API.
func modelProfileDraft(kind, name, modelURL, model, apiKey string) kbconfig.ModelProfileDraft {
	return kbconfig.ModelProfileDraft{
		Kind: kind, Name: name, BaseURL: modelURL, Model: model,
		APIKey: apiKey, HasAPIKey: apiKey != "",
	}
}

func TestModelProfileLifecycle(t *testing.T) {
	modelURL, calls := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	empty, err := service.ListModelProfiles(ctx)
	if err != nil {
		t.Fatalf("ListModelProfiles() error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("initial list = %v, want an empty slice", empty)
	}

	embedding, err := service.CreateModelProfile(ctx, modelProfileDraft(
		"embedding", "bge-m3", modelURL, "embed-a", "private-key"))
	if err != nil {
		t.Fatalf("CreateModelProfile(embedding) error = %v", err)
	}
	embeddingID, _ := embedding["id"].(string)
	if !modelProfileIDPattern.MatchString(embeddingID) {
		t.Fatalf("embedding id = %q", embeddingID)
	}
	if embedding["kind"] != "embedding" || embedding["name"] != "bge-m3" ||
		embedding["base_url"] != modelURL || embedding["model"] != "embed-a" {
		t.Fatalf("embedding projection = %v", embedding)
	}
	if embedding["api_key_set"] != true || embedding["vector_size"] != 3 {
		t.Fatalf("embedding projection = %v", embedding)
	}
	if encoded, _ := json.Marshal(embedding); strings.Contains(string(encoded), "private-key") {
		t.Fatalf("projection leaked the key: %s", encoded)
	}

	// The stored row protects the key and records the probed dimension.
	stored, err := stores.ModelProfiles.Get(ctx, embeddingID)
	if err != nil || stored == nil {
		t.Fatalf("ModelProfiles.Get() = %v, %v", stored, err)
	}
	if !strings.HasPrefix(stored.ProtectedKey, state.EncryptionPrefix) || strings.Contains(stored.ProtectedKey, "private-key") {
		t.Fatalf("stored protected_key = %q", stored.ProtectedKey)
	}
	if stored.VectorSize == nil || *stored.VectorSize != 3 {
		t.Fatalf("stored vector size = %v", stored.VectorSize)
	}
	embeddingProbes := calls.find(http.MethodPost, "/embeddings")
	if len(embeddingProbes) != 1 {
		t.Fatalf("embedding probes = %d", len(embeddingProbes))
	}
	if embeddingProbes[0].Header.Get("Authorization") != "Bearer private-key" ||
		embeddingProbes[0].Body["model"] != "embed-a" {
		t.Fatalf("embedding probe = %+v", embeddingProbes[0])
	}
	// Every list/details projection never exposes credentials.
	listed, err := service.ListModelProfiles(ctx)
	if err != nil || len(listed) != 1 {
		t.Fatalf("list after create = %v, %v", listed, err)
	}
	if encoded, _ := json.Marshal(listed); strings.Contains(string(encoded), "private-key") ||
		strings.Contains(string(encoded), "enc:v1:") {
		t.Fatalf("list leaked credentials: %s", encoded)
	}

	// A reranker profile probes accessibility and stores no dimension.
	reranker, err := service.CreateModelProfile(ctx, modelProfileDraft(
		"reranker", "bge-reranker", modelURL, "rank-a", "rerank-key"))
	if err != nil {
		t.Fatalf("CreateModelProfile(reranker) error = %v", err)
	}
	rerankerID, _ := reranker["id"].(string)
	if reranker["vector_size"] != nil {
		t.Fatalf("reranker vector size = %v", reranker["vector_size"])
	}
	rerankProbes := calls.find(http.MethodPost, "/rerank")
	if len(rerankProbes) != 1 || rerankProbes[0].Header.Get("Authorization") != "Bearer rerank-key" {
		t.Fatalf("rerank probe = %+v", rerankProbes)
	}

	// A VLM profile is stored without any provider probe.
	providerCallsBefore := len(calls.all())
	vlm, err := service.CreateModelProfile(ctx, modelProfileDraft(
		"vlm", "qwen-vl", modelURL, "qwen2.5-vl-7b", ""))
	if err != nil {
		t.Fatalf("CreateModelProfile(vlm) error = %v", err)
	}
	if len(calls.all()) != providerCallsBefore {
		t.Fatalf("vlm creation probed the provider: %+v", calls.all()[providerCallsBefore:])
	}
	if vlm["vector_size"] != nil || vlm["api_key_set"] != false {
		t.Fatalf("vlm projection = %v", vlm)
	}

	// List order is kind then display name.
	listed, err = service.ListModelProfiles(ctx)
	if err != nil || len(listed) != 3 {
		t.Fatalf("ListModelProfiles() = %v, %v", listed, err)
	}
	wantKinds := []string{"embedding", "reranker", "vlm"}
	for index, kind := range wantKinds {
		if listed[index]["kind"] != kind {
			t.Fatalf("list order = %v", listed)
		}
	}

	key, err := service.ModelProfileKey(ctx, embeddingID)
	if err != nil || key != "private-key" {
		t.Fatalf("ModelProfileKey(embedding) = %q, %v", key, err)
	}
	if key, err := service.ModelProfileKey(ctx, rerankerID); err != nil || key != "rerank-key" {
		t.Fatalf("ModelProfileKey(reranker) = %q, %v", key, err)
	}

	// Provider models are listed through the stored connection (key reused).
	models, err := service.ModelProfileModels(ctx, embeddingID)
	if err != nil {
		t.Fatalf("ModelProfileModels() error = %v", err)
	}
	if len(models) != 3 || models[2] != "rank-a" {
		t.Fatalf("models = %v", models)
	}
	modelCalls := calls.find(http.MethodGet, "/models")
	if len(modelCalls) != 1 || modelCalls[0].Header.Get("Authorization") != "Bearer private-key" {
		t.Fatalf("models request = %+v", modelCalls)
	}

	detail, err := service.ModelProfile(ctx, embeddingID)
	if err != nil || detail["id"] != embeddingID || detail["name"] != "bge-m3" {
		t.Fatalf("ModelProfile() = %v, %v", detail, err)
	}

	// Renaming keeps the provider configuration and does not probe again.
	probesBeforeRename := calls.count(http.MethodPost, "/embeddings")
	renamed, err := service.UpdateModelProfile(ctx, embeddingID, kbconfig.ModelProfileDraft{Name: "bge-m3-renamed"})
	if err != nil {
		t.Fatalf("UpdateModelProfile(rename) error = %v", err)
	}
	if renamed["name"] != "bge-m3-renamed" || renamed["model"] != "embed-a" ||
		renamed["base_url"] != modelURL || renamed["vector_size"] != 3 {
		t.Fatalf("renamed projection = %v", renamed)
	}
	if calls.count(http.MethodPost, "/embeddings") != probesBeforeRename {
		t.Fatal("rename probed the embedding provider")
	}

	// Changing the model probes again and records the new dimension.
	probesBeforeModel := calls.count(http.MethodPost, "/embeddings")
	updated, err := service.UpdateModelProfile(ctx, embeddingID, kbconfig.ModelProfileDraft{Model: "embed-b"})
	if err != nil {
		t.Fatalf("UpdateModelProfile(model) error = %v", err)
	}
	if updated["model"] != "embed-b" || updated["vector_size"] != 5 {
		t.Fatalf("updated projection = %v", updated)
	}
	if calls.count(http.MethodPost, "/embeddings") != probesBeforeModel+1 {
		t.Fatal("model change did not probe the embedding provider")
	}

	// A key-only change probes too (the stored credential changed).
	probesBeforeKey := calls.count(http.MethodPost, "/embeddings")
	if _, err := service.UpdateModelProfile(ctx, embeddingID, kbconfig.ModelProfileDraft{
		APIKey: "rotated-key", HasAPIKey: true,
	}); err != nil {
		t.Fatalf("UpdateModelProfile(key) error = %v", err)
	}
	if calls.count(http.MethodPost, "/embeddings") != probesBeforeKey+1 {
		t.Fatal("key change did not probe the embedding provider")
	}
	if key, err := service.ModelProfileKey(ctx, embeddingID); err != nil || key != "rotated-key" {
		t.Fatalf("rotated key = %q, %v", key, err)
	}

	deleted, err := service.DeleteModelProfile(ctx, embeddingID)
	if err != nil || !deleted {
		t.Fatalf("DeleteModelProfile() = %v, %v", deleted, err)
	}
	if deleted, err := service.DeleteModelProfile(ctx, embeddingID); err != nil || deleted {
		t.Fatalf("second DeleteModelProfile() = %v, %v", deleted, err)
	}
	if remaining, err := service.ListModelProfiles(ctx); err != nil || len(remaining) != 2 {
		t.Fatalf("list after delete = %v, %v", remaining, err)
	}
}

func TestModelProfileErrorMapping(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	created, err := service.CreateModelProfile(ctx, modelProfileDraft(
		"embedding", "bge-m3", modelURL, "embed-a", "private-key"))
	if err != nil {
		t.Fatalf("CreateModelProfile() error = %v", err)
	}
	profileID, _ := created["id"].(string)

	// Duplicate (kind, name) is an HTTP 409 conflict.
	_, err = service.CreateModelProfile(ctx, modelProfileDraft(
		"embedding", "bge-m3", modelURL, "embed-a", "private-key"))
	var conflict *apierrors.KnowledgeConflictError
	if !errors.As(err, &conflict) || conflict.Message != kbconfig.MessageModelProfileExists {
		t.Fatalf("duplicate error = %v", err)
	}

	// Invalid kind and missing service URL are validation errors.
	_, err = service.CreateModelProfile(ctx, kbconfig.ModelProfileDraft{
		Kind: "sql", Name: "x", BaseURL: modelURL, Model: "m",
	})
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageModelKindInvalid {
		t.Fatalf("invalid kind error = %v", err)
	}
	_, err = service.CreateModelProfile(ctx, kbconfig.ModelProfileDraft{
		Kind: "embedding", Name: "no-url", Model: "m",
	})
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageModelServiceURLRequired {
		t.Fatalf("missing url error = %v", err)
	}

	// Provider failures are validation errors and never persist a row.
	failingURL, _ := newFakeModelAPI(t, func(string, map[string]any) (int, string) {
		return http.StatusUnauthorized, `{"detail":"private upstream error"}`
	})
	before, _ := service.ListModelProfiles(ctx)
	if _, err := service.CreateModelProfile(ctx, modelProfileDraft(
		"embedding", "failing", failingURL, "embed-a", "bad-key")); err == nil {
		t.Fatal("failing provider was accepted")
	} else if !errors.As(err, &validation) || validation.Message != "HTTP 401：认证失败，请检查 apikey" {
		t.Fatalf("provider error = %v", err)
	}
	if after, _ := service.ListModelProfiles(ctx); len(after) != len(before) {
		t.Fatalf("failed create persisted a row: %v", after)
	}

	// A failed update leaves the stored configuration untouched.
	if _, err := service.UpdateModelProfile(ctx, profileID, kbconfig.ModelProfileDraft{
		BaseURL: failingURL, Model: "embed-a",
	}); err == nil {
		t.Fatal("failing provider update was accepted")
	} else if !errors.As(err, &validation) {
		t.Fatalf("update provider error = %v", err)
	}
	stored, err := stores.ModelProfiles.Get(ctx, profileID)
	if err != nil || stored == nil || stored.BaseURL != modelURL {
		t.Fatalf("stored profile after failed update = %+v, %v", stored, err)
	}

	// Missing profiles are 404-shaped validation errors on every by-id route.
	missingID := "0123456789abcdef0123456789abcdef"
	missingCalls := map[string]func() error{
		"ModelProfile": func() error {
			_, err := service.ModelProfile(ctx, missingID)
			return err
		},
		"UpdateModelProfile": func() error {
			_, err := service.UpdateModelProfile(ctx, missingID, kbconfig.ModelProfileDraft{Name: "x"})
			return err
		},
		"ModelProfileKey": func() error {
			_, err := service.ModelProfileKey(ctx, missingID)
			return err
		},
		"ModelProfileModels": func() error {
			_, err := service.ModelProfileModels(ctx, missingID)
			return err
		},
	}
	for name, call := range missingCalls {
		if err := call(); err == nil {
			t.Fatalf("%s(missing) returned no error", name)
		} else if !errors.As(err, &validation) || validation.Message != kbconfig.MessageModelProfileMissing {
			t.Fatalf("%s(missing) error = %v", name, err)
		}
	}
	if deleted, err := service.DeleteModelProfile(ctx, missingID); err != nil || deleted {
		t.Fatalf("DeleteModelProfile(missing) = %v, %v", deleted, err)
	}
}

// TestModelProfileOCREngine pins the OCR model-configuration path: the engine id
// is stored as the model, no provider probe happens (OCR runs inside the parsing
// service) and updates keep base_url/API key empty.
func TestModelProfileOCREngine(t *testing.T) {
	// The knowledge-base service points at an unreachable Qdrant URL and has no
	// fake model API: OCR create/update must not call any provider.
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	created, err := service.CreateModelProfile(ctx, kbconfig.ModelProfileDraft{
		Kind: "ocr", Name: "easyocr", Model: " EasyOCR ",
	})
	if err != nil {
		t.Fatalf("CreateModelProfile(ocr) error = %v", err)
	}
	if created["kind"] != "ocr" || created["model"] != "easyocr" ||
		created["base_url"] != "" || created["api_key_set"] != false || created["vector_size"] != nil {
		t.Fatalf("ocr projection = %v", created)
	}
	id, _ := created["id"].(string)
	if !modelProfileIDPattern.MatchString(id) {
		t.Fatalf("ocr id = %q", id)
	}

	updated, err := service.UpdateModelProfile(ctx, id, kbconfig.ModelProfileDraft{Model: "tesseract"})
	if err != nil {
		t.Fatalf("UpdateModelProfile(ocr) error = %v", err)
	}
	if updated["model"] != "tesseract" || updated["base_url"] != "" || updated["name"] != "easyocr" {
		t.Fatalf("updated ocr projection = %v", updated)
	}

	stored, err := stores.ModelProfiles.Get(ctx, id)
	if err != nil || stored == nil {
		t.Fatalf("ModelProfiles.Get() = %v, %v", stored, err)
	}
	if stored.Kind != "ocr" || stored.Model != "tesseract" || stored.BaseURL != "" ||
		stored.ProtectedKey != "" || stored.VectorSize != nil {
		t.Fatalf("stored ocr row = %+v", stored)
	}

	// The engine whitelist is enforced with a Chinese validation error.
	_, err = service.CreateModelProfile(ctx, kbconfig.ModelProfileDraft{
		Kind: "ocr", Name: "bad", Model: "paddleocr",
	})
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageOCREngineInvalid {
		t.Fatalf("invalid engine error = %v", err)
	}
}
