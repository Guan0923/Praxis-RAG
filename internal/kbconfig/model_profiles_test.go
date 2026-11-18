package kbconfig

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// validModelDraft is a minimal accepted draft used by the validation table.
func validModelDraft() ModelProfileDraft {
	return ModelProfileDraft{
		Kind:    "embedding",
		Name:    "bge-m3",
		BaseURL: "http://127.0.0.1:9000/v1",
		Model:   "BAAI/bge-m3",
	}
}

func TestDraftModelProfileValidation(t *testing.T) {
	longName := strings.Repeat("名", ModelProfileNameMaxLength+1)
	longModel := strings.Repeat("m", ModelProfileModelMaxLength+1)
	cases := []struct {
		name        string
		kind        string
		profileName string
		draft       ModelProfileDraft
		current     modelapi.ModelConnection
		message     string
	}{
		{
			name: "invalid kind", kind: "sql", profileName: "x",
			draft: validModelDraft(), message: MessageModelKindInvalid,
		},
		{
			name: "empty name", kind: "embedding", profileName: "   ",
			draft: validModelDraft(), message: MessageModelProfileNameRequired,
		},
		{
			name: "long name", kind: "embedding", profileName: longName,
			draft: validModelDraft(), message: MessageModelProfileNameTooLong,
		},
		{
			name: "empty model", kind: "embedding", profileName: "x",
			draft:   ModelProfileDraft{BaseURL: "http://127.0.0.1:9000/v1", Model: " "},
			message: MessageModelModelRequired,
		},
		{
			name: "long model", kind: "embedding", profileName: "x",
			draft:   ModelProfileDraft{BaseURL: "http://127.0.0.1:9000/v1", Model: longModel},
			message: MessageModelModelTooLong,
		},
		{
			name: "missing base url", kind: "embedding", profileName: "x",
			draft:   ModelProfileDraft{Model: "BAAI/bge-m3"},
			message: MessageModelServiceURLRequired,
		},

	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := DraftModelProfile(testCase.current, testCase.kind, testCase.profileName, testCase.draft)
			if err == nil {
				t.Fatal("draft was accepted")
			}
			var validation *ValidationError
			if !errors.As(err, &validation) || validation.Message != testCase.message {
				t.Fatalf("error = %v, want validation %q", err, testCase.message)
			}
		})
	}

	// An unparsable base URL is rejected by normalize_base_url semantics.
	badURL := validModelDraft()
	badURL.BaseURL = "ftp://model.local/v1"
	if _, _, err := DraftModelProfile(modelapi.ModelConnection{}, "embedding", "x", badURL); err == nil {
		t.Fatal("ftp base URL was accepted")
	} else {
		var modelError *modelapi.ModelAPIError
		if !errors.As(err, &modelError) {
			t.Fatalf("invalid URL error = %v, want ModelAPIError", err)
		}
	}
}

func TestDraftModelProfileTrimsAndNormalizes(t *testing.T) {
	draft := ModelProfileDraft{
		Kind: "embedding", Name: "  bge m3  ",
		BaseURL: " http://127.0.0.1:9000/v1/ ", Model: "  BAAI/bge-m3 ",
	}
	connection, name, err := DraftModelProfile(modelapi.ModelConnection{}, "embedding", draft.Name, draft)
	if err != nil {
		t.Fatalf("DraftModelProfile() error = %v", err)
	}
	if name != "bge m3" {
		t.Fatalf("name = %q", name)
	}
	if connection.BaseURL != "http://127.0.0.1:9000/v1" {
		t.Fatalf("base_url = %q", connection.BaseURL)
	}
	if connection.Model != "BAAI/bge-m3" {
		t.Fatalf("model = %q", connection.Model)
	}
}

func TestDraftModelProfileKeyPreservation(t *testing.T) {
	current := modelapi.ModelConnection{
		BaseURL: "http://model.local/v1", Model: "embed-a", APIKey: "stored-key",
	}
	cases := []struct {
		name    string
		draft   ModelProfileDraft
		wantKey string
	}{
		{
			name:    "absent key and absent url keeps key",
			draft:   ModelProfileDraft{Model: "embed-b"},
			wantKey: "stored-key",
		},
		{
			name:    "absent key and unchanged url keeps key",
			draft:   ModelProfileDraft{BaseURL: "http://model.local/v1", Model: "embed-b"},
			wantKey: "stored-key",
		},
		{
			name:    "changed url clears the stored key",
			draft:   ModelProfileDraft{BaseURL: "http://other.local/v1", Model: "embed-b"},
			wantKey: "",
		},
		{
			name: "explicit key replaces the stored key",
			draft: ModelProfileDraft{
				BaseURL: "http://other.local/v1", Model: "embed-b",
				APIKey: "new-key", HasAPIKey: true,
			},
			wantKey: "new-key",
		},
		{
			name: "explicit empty key clears the stored key",
			draft: ModelProfileDraft{
				BaseURL: "http://model.local/v1", Model: "embed-b",
				APIKey: "", HasAPIKey: true,
			},
			wantKey: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connection, _, err := DraftModelProfile(current, "embedding", "profile", testCase.draft)
			if err != nil {
				t.Fatalf("DraftModelProfile() error = %v", err)
			}
			if connection.APIKey != testCase.wantKey {
				t.Fatalf("apikey = %q, want %q", connection.APIKey, testCase.wantKey)
			}
		})
	}
}

func TestProbeModelProfile(t *testing.T) {
	ctx := context.Background()
	connection := modelapi.ModelConnection{BaseURL: "http://model.local/v1", Model: "embed-a"}
	prober := &fakeProber{vectorSize: 768}

	vectorSize, err := ProbeModelProfile(ctx, connection, "embedding", prober)
	if err != nil {
		t.Fatalf("embedding probe error = %v", err)
	}
	if vectorSize == nil || *vectorSize != 768 {
		t.Fatalf("embedding vector size = %v", vectorSize)
	}
	if prober.embeddingCalls != 1 || prober.lastEmbedding.Model != "embed-a" {
		t.Fatalf("embedding probe calls = %d (%+v)", prober.embeddingCalls, prober.lastEmbedding)
	}

	if vectorSize, err := ProbeModelProfile(ctx, connection, "reranker", prober); err != nil || vectorSize != nil {
		t.Fatalf("reranker probe = %v, %v", vectorSize, err)
	}
	if prober.rerankerCalls != 1 {
		t.Fatalf("reranker probe calls = %d", prober.rerankerCalls)
	}

	// VLM and JEV configurations are never probed: no provider call may happen.
	if vectorSize, err := ProbeModelProfile(ctx, connection, "vlm", prober); err != nil || vectorSize != nil {
		t.Fatalf("vlm probe = %v, %v", vectorSize, err)
	}
	if vectorSize, err := ProbeModelProfile(ctx, connection, "jev", prober); err != nil || vectorSize != nil {
		t.Fatalf("jev probe = %v, %v", vectorSize, err)
	}
	if prober.embeddingCalls != 1 || prober.rerankerCalls != 1 {
		t.Fatalf("vlm/jev probe called the provider: %d/%d", prober.embeddingCalls, prober.rerankerCalls)
	}

	if _, err := ProbeModelProfile(ctx, connection, "sql", prober); err == nil {
		t.Fatal("invalid kind was probed")
	} else {
		var validation *ValidationError
		if !errors.As(err, &validation) || validation.Message != MessageModelKindInvalid {
			t.Fatalf("invalid kind error = %v", err)
		}
	}

	prober.embeddingError = &modelapi.ModelAPIError{Message: "HTTP 401：认证失败，请检查 apikey"}
	if _, err := ProbeModelProfile(ctx, connection, "embedding", prober); err == nil {
		t.Fatal("embedding probe error was swallowed")
	} else {
		var modelError *modelapi.ModelAPIError
		if !errors.As(err, &modelError) {
			t.Fatalf("embedding probe error = %v", err)
		}
	}
	prober.rerankerError = &modelapi.ModelAPIError{Message: "无法连接模型服务"}
	if _, err := ProbeModelProfile(ctx, connection, "reranker", prober); err == nil {
		t.Fatal("reranker probe error was swallowed")
	}
}

func TestPublicModelProfileNeverLeaksKey(t *testing.T) {
	vectorSize := 1024
	public := PublicModelProfile(ModelProfileStored{
		ID: "0123456789abcdef0123456789abcdef", Kind: "embedding", Name: "bge-m3",
		BaseURL: "http://model.local/v1", Model: "BAAI/bge-m3",
		ProtectedKey: "enc:v1:top-secret", VectorSize: &vectorSize,
		CreatedAt: "2026-11-10T00:00:00.000000+00:00",
		UpdatedAt: "2026-11-10T01:00:00.000000+00:00",
	})
	if public["id"] != "0123456789abcdef0123456789abcdef" || public["kind"] != "embedding" ||
		public["name"] != "bge-m3" || public["base_url"] != "http://model.local/v1" ||
		public["model"] != "BAAI/bge-m3" || public["api_key_set"] != true ||
		public["vector_size"] != 1024 || public["created_at"] != "2026-11-10T00:00:00.000000+00:00" ||
		public["updated_at"] != "2026-11-10T01:00:00.000000+00:00" {
		t.Fatalf("public projection = %v", public)
	}
	if _, leaked := public["protected_key"]; leaked {
		t.Fatalf("public projection leaked protected_key: %v", public)
	}
	encoded, err := json.Marshal(public)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), "top-secret") || strings.Contains(string(encoded), "enc:v1:") {
		t.Fatalf("public projection leaked the key: %s", encoded)
	}

	// A profile without a stored key and without a probed dimension projects
	// api_key_set=false and a nil vector size.
	empty := PublicModelProfile(ModelProfileStored{ID: "id", Kind: "vlm", Name: "vlm", Model: "m"})
	if empty["api_key_set"] != false || empty["vector_size"] != nil {
		t.Fatalf("empty projection = %v", empty)
	}
}

func TestModelProfileConnectionRoundTrip(t *testing.T) {
	vectorSize := 1024
	baseURL, protected, err := StoredModelProfileConnection(modelapi.ModelConnection{
		BaseURL: "http://model.local/v1", Model: "BAAI/bge-m3",
		APIKey: "private-key", VectorSize: &vectorSize,
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredModelProfileConnection() error = %v", err)
	}
	if baseURL != "http://model.local/v1" {
		t.Fatalf("baseURL = %q", baseURL)
	}
	if !strings.HasPrefix(protected, state.EncryptionPrefix) || strings.Contains(protected, "private-key") {
		t.Fatalf("protected key = %q", protected)
	}

	record := ModelProfileStored{
		ID: "id", Kind: "embedding", Name: "bge-m3", BaseURL: baseURL,
		Model: "BAAI/bge-m3", ProtectedKey: protected, VectorSize: &vectorSize,
		CreatedAt: "2026-11-10T00:00:00.000000+00:00",
	}
	connection, err := ModelProfileConnection(record, testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelProfileConnection() error = %v", err)
	}
	if connection.APIKey != "private-key" || connection.BaseURL != baseURL || connection.Model != "BAAI/bge-m3" {
		t.Fatalf("decoded connection = %+v", connection)
	}
	if connection.VectorSize == nil || *connection.VectorSize != 1024 {
		t.Fatalf("decoded vector size = %v", connection.VectorSize)
	}

	// Non-embedding profiles never carry a vector size, and an empty key stays
	// empty (nothing to decrypt).
	reranker, err := ModelProfileConnection(ModelProfileStored{
		Kind: "reranker", BaseURL: baseURL, Model: "rank-a", VectorSize: &vectorSize,
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelProfileConnection(reranker) error = %v", err)
	}
	if reranker.VectorSize != nil || reranker.APIKey != "" {
		t.Fatalf("reranker connection = %+v", reranker)
	}

	if _, err := ModelProfileConnection(record, "not-a-key"); err == nil {
		t.Fatal("undecryptable key was accepted")
	}
}

func TestStoredModelProfileConnectionRejectsInvalidEncryptionKey(t *testing.T) {
	if _, _, err := StoredModelProfileConnection(modelapi.ModelConnection{
		BaseURL: "http://model.local/v1", APIKey: "private-key",
	}, "not-a-key"); err == nil {
		t.Fatal("invalid encryption key was accepted")
	} else {
		var modelError *modelapi.ModelAPIError
		if !errors.As(err, &modelError) {
			t.Fatalf("error = %v, want ModelAPIError", err)
		}
	}
}
