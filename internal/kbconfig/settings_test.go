package kbconfig

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

var collectionNamePattern = regexp.MustCompile(`^knowledge_[0-9a-f]{32}$`)

func assertValidationMessage(t *testing.T, err error, message string) {
	t.Helper()
	var validationError *ValidationError
	if !errors.As(err, &validationError) {
		t.Fatalf("error = %v, want *ValidationError", err)
	}
	if validationError.Message != message {
		t.Fatalf("message = %q, want %q", validationError.Message, message)
	}
}

func TestValidateNamespace(t *testing.T) {
	valid := []string{"alpha", "a", "A.b-c_1", "0abc", strings.Repeat("a", 64)}
	for _, namespace := range valid {
		if err := ValidateNamespace(namespace); err != nil {
			t.Errorf("ValidateNamespace(%q) error = %v", namespace, err)
		}
	}
	invalid := []string{"", "-alpha", ".alpha", "_alpha", "a b", "中文", "a/b", strings.Repeat("a", 65), "a\n"}
	for _, namespace := range invalid {
		err := ValidateNamespace(namespace)
		if err == nil {
			t.Errorf("ValidateNamespace(%q) accepted invalid namespace", namespace)
			continue
		}
		assertValidationMessage(t, err, MessageNamespaceInvalid)
	}
}

func TestNewCollectionName(t *testing.T) {
	first, err := NewCollectionName()
	if err != nil {
		t.Fatalf("NewCollectionName() error: %v", err)
	}
	if !collectionNamePattern.MatchString(first) {
		t.Fatalf("collection name = %q, want knowledge_<32 hex>", first)
	}
	second, err := NewCollectionName()
	if err != nil {
		t.Fatalf("NewCollectionName() error: %v", err)
	}
	if first == second {
		t.Fatalf("collection names must be unique: %q", first)
	}
}

func TestISOTimestamp(t *testing.T) {
	if got := ISOTimestamp(fixedTimestamp()); got != "2026-11-08T10:00:00.000000+00:00" {
		t.Errorf("ISOTimestamp() = %q", got)
	}
	local := time.Date(2026, 11, 8, 18, 30, 15, 123456789, time.FixedZone("CST", 8*3600))
	if got := ISOTimestamp(local); got != "2026-11-08T10:30:15.123456+00:00" {
		t.Errorf("ISOTimestamp(local) = %q", got)
	}
}

func TestCreateSettingsProbesAndStores(t *testing.T) {
	prober := &fakeProber{vectorSize: 3}
	input := newTestCreateInput("http://host:8080/v1/")
	input.EmbeddingModel = "  embed-a  "
	input.RerankerModel = " rank-a "
	settings, err := CreateSettings(context.Background(), input, prober)
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	if settings["namespace"] != "alpha" || settings["vector_size"] != 3 {
		t.Errorf("namespace/vector_size = %v/%v", settings["namespace"], settings["vector_size"])
	}
	if settings["embedding_model"] != "embed-a" || settings["reranker_model"] != "rank-a" {
		t.Errorf("models = %v/%v", settings["embedding_model"], settings["reranker_model"])
	}
	if !collectionNamePattern.MatchString(settings["collection"].(string)) {
		t.Errorf("collection = %v", settings["collection"])
	}
	if settings["tokenizer"] != DefaultTokenizerModel {
		t.Errorf("tokenizer = %v", settings["tokenizer"])
	}
	if settings["strategy"] != "character" || settings["chunk_size"] != 120 || settings["chunk_overlap"] != 20 {
		t.Errorf("chunking = %v/%v/%v", settings["strategy"], settings["chunk_size"], settings["chunk_overlap"])
	}
	if settings["created_at"] != ISOTimestamp(fixedTimestamp()) {
		t.Errorf("created_at = %v", settings["created_at"])
	}
	if prober.embeddingCalls != 1 || prober.rerankerCalls != 1 {
		t.Fatalf("prober calls = %d/%d", prober.embeddingCalls, prober.rerankerCalls)
	}
	if prober.lastEmbedding.BaseURL != "http://host:8080/v1" || prober.lastEmbedding.Model != "embed-a" ||
		prober.lastEmbedding.APIKey != "private-key" || prober.lastEmbedding.VectorSize != nil {
		t.Errorf("embedding probe connection = %+v", prober.lastEmbedding)
	}
	if prober.lastReranker.Model != "rank-a" || prober.lastReranker.BaseURL != "http://host:8080/v1" {
		t.Errorf("reranker probe connection = %+v", prober.lastReranker)
	}

	// Stored credentials round-trip through the encryption key.
	embedding, err := ModelConnectionForKey(settings, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey() error: %v", err)
	}
	if embedding.APIKey != "private-key" || embedding.BaseURL != "http://host:8080/v1" {
		t.Errorf("round-trip connection = %+v", embedding)
	}
	reranker, err := ModelConnectionForKey(settings, "reranker", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(reranker) error: %v", err)
	}
	if reranker.APIKey != "" || reranker.Model != "rank-a" {
		t.Errorf("reranker round-trip = %+v", reranker)
	}
	public := PublicSettings(settings)
	if public["embedding"].(map[string]any)["api_key_set"] != true {
		t.Errorf("public embedding = %v", public["embedding"])
	}
	if _, leaked := public["protected_key"]; leaked {
		t.Errorf("public settings leaked protected_key")
	}
}

func TestCreateSettingsDefaults(t *testing.T) {
	prober := &fakeProber{vectorSize: 1024}
	input := CreateInput{
		Namespace:       "alpha",
		Chunking:        chunking.Config{Strategy: "recursive", ChunkSize: 512, ChunkOverlap: 64},
		EmbeddingModel:  "embed-a",
		EmbeddingValues: map[string]any{"base_url": "http://host"},
		EncryptionKey:   testEncryptionKey,
	}
	settings, err := CreateSettings(context.Background(), input, prober)
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	if settings["collection"] == "" {
		t.Errorf("collection must be generated")
	}
	if settings["tokenizer"] != DefaultTokenizerModel {
		t.Errorf("tokenizer = %v", settings["tokenizer"])
	}
	if prober.rerankerCalls != 0 {
		t.Errorf("reranker probe must not run without a model")
	}
	stored := settings["reranker"].(map[string]any)
	if stored["base_url"] != "" || stored["protected_key"] != "" {
		t.Errorf("empty reranker stored = %v", stored)
	}
	created, ok := settings["created_at"].(string)
	if !ok || created == "" {
		t.Fatalf("created_at = %v", settings["created_at"])
	}
	if _, err := time.Parse(TimestampLayout, created); err != nil {
		t.Errorf("created_at %q is not a Python isoformat timestamp: %v", created, err)
	}

	input.Collection = "knowledge_explicit"
	settings, err = CreateSettings(context.Background(), input, prober)
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	if settings["collection"] != "knowledge_explicit" {
		t.Errorf("explicit collection = %v", settings["collection"])
	}
}

func TestCreateSettingsValidationErrors(t *testing.T) {
	base := newTestCreateInput("http://host")
	cases := []struct {
		name   string
		prober *fakeProber
		mutate func(*CreateInput)
		check  func(*testing.T, error)
	}{
		{
			name: "invalid namespace", prober: &fakeProber{vectorSize: 3},
			mutate: func(input *CreateInput) { input.Namespace = "bad namespace" },
			check:  func(t *testing.T, err error) { assertValidationMessage(t, err, MessageNamespaceInvalid) },
		},
		{
			name: "empty embedding model", prober: &fakeProber{vectorSize: 3},
			mutate: func(input *CreateInput) { input.EmbeddingModel = "   " },
			check:  func(t *testing.T, err error) { assertValidationMessage(t, err, MessageEmbeddingModelRequired) },
		},
		{
			name: "unknown chunking strategy", prober: &fakeProber{vectorSize: 3},
			mutate: func(input *CreateInput) { input.Chunking.Strategy = "bogus" },
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != "未知切分策略" {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "invalid chunk overlap", prober: &fakeProber{vectorSize: 3},
			mutate: func(input *CreateInput) { input.Chunking.ChunkOverlap = 500 },
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != "重叠长度必须是非负整数且小于块长度" {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "prober failure", prober: &fakeProber{embeddingError: &modelapi.ModelAPIError{Message: "模型服务请求超时"}},
			mutate: func(input *CreateInput) {},
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != "模型服务请求超时" {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "invalid apikey", prober: &fakeProber{vectorSize: 3},
			mutate: func(input *CreateInput) {
				input.EmbeddingValues = map[string]any{"base_url": "http://host", "apikey": "a\nb"}
			},
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != "apikey 格式无效" {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "invalid base url", prober: &fakeProber{vectorSize: 3},
			mutate: func(input *CreateInput) {
				input.EmbeddingValues = map[string]any{"base_url": "ftp://host"}
			},
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != "base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段" {
					t.Fatalf("error = %v", err)
				}
			},
		},
		{
			name: "reranker probe failure", prober: &fakeProber{vectorSize: 3, rerankerError: &modelapi.ModelAPIError{Message: "服务拒绝访问"}},
			mutate: func(input *CreateInput) {},
			check: func(t *testing.T, err error) {
				if err == nil || err.Error() != "服务拒绝访问" {
					t.Fatalf("error = %v", err)
				}
			},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			input := base
			testCase.mutate(&input)
			_, err := CreateSettings(context.Background(), input, testCase.prober)
			testCase.check(t, err)
		})
	}
}

func TestUpdateSettingsKeepsUnchangedConfiguration(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 3}
	result, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values: map[string]any{}, UpdatedAt: fixedTimestamp(), EncryptionKey: testEncryptionKey,
	}, prober)
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	if result.StagingCollection != "" {
		t.Errorf("staging collection = %q, want empty", result.StagingCollection)
	}
	if result.Settings["embedding_model"] != "embed-a" || result.Settings["reranker_model"] != "rank-a" {
		t.Errorf("models = %v/%v", result.Settings["embedding_model"], result.Settings["reranker_model"])
	}
	if result.Settings["collection"] != current["collection"] {
		t.Errorf("collection changed: %v", result.Settings["collection"])
	}
	if result.Settings["vector_size"] != 3 {
		t.Errorf("vector_size = %v", result.Settings["vector_size"])
	}
	if result.Settings["updated_at"] != ISOTimestamp(fixedTimestamp()) {
		t.Errorf("updated_at = %v", result.Settings["updated_at"])
	}
	if prober.embeddingCalls != 1 || prober.rerankerCalls != 1 {
		t.Errorf("prober calls = %d/%d (existing reranker must be probed)", prober.embeddingCalls, prober.rerankerCalls)
	}
	embedding, err := ModelConnectionForKey(result.Settings, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey() error: %v", err)
	}
	if embedding.APIKey != "private-key" {
		t.Errorf("stored key = %q", embedding.APIKey)
	}
}

func TestUpdateSettingsKeyOnlyChangeDoesNotReembed(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 3}
	result, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"embedding": map[string]any{"apikey": "new-key"}},
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, prober)
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	if result.StagingCollection != "" || result.Settings["collection"] != current["collection"] {
		t.Errorf("key-only update must not reindex: staging=%q", result.StagingCollection)
	}
	embedding, err := ModelConnectionForKey(result.Settings, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey() error: %v", err)
	}
	if embedding.APIKey != "new-key" {
		t.Errorf("stored key = %q, want new-key", embedding.APIKey)
	}
}

func TestUpdateSettingsRejectsEmbeddingChangeWithoutReembed(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 3}
	_, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values: map[string]any{"embedding_model": "embed-b"}, EncryptionKey: testEncryptionKey,
	}, prober)
	assertValidationMessage(t, err, MessageEmbeddingChangeRequiresReembed)
	if prober.embeddingCalls != 0 || prober.rerankerCalls != 0 {
		t.Errorf("validation must run before provider probes")
	}
}

func TestUpdateSettingsRejectsVectorSizeChangeWithoutReembed(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 5}
	_, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values: map[string]any{}, EncryptionKey: testEncryptionKey,
	}, prober)
	assertValidationMessage(t, err, MessageVectorSizeRequiresReembed)
}

func TestUpdateSettingsWithReembedReturnsStaging(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 5}
	result, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"embedding_model": "embed-b"},
		Reembed:       true,
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, prober)
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	if !collectionNamePattern.MatchString(result.StagingCollection) {
		t.Fatalf("staging collection = %q", result.StagingCollection)
	}
	if result.Settings["collection"] != current["collection"] {
		t.Errorf("collection must stay published until activation: %v", result.Settings["collection"])
	}
	if result.Settings["embedding_model"] != "embed-b" || result.Settings["vector_size"] != 5 {
		t.Errorf("updated settings = %v/%v", result.Settings["embedding_model"], result.Settings["vector_size"])
	}
	if result.Embedding.Model != "embed-b" || result.Embedding.VectorSize == nil || *result.Embedding.VectorSize != 5 {
		t.Errorf("resolved embedding = %+v", result.Embedding)
	}
	activated := ActivateReembed(result.Settings, result.StagingCollection, fixedTimestamp())
	if activated["collection"] != result.StagingCollection {
		t.Errorf("activated collection = %v", activated["collection"])
	}
	if activated["reembedded_at"] != ISOTimestamp(fixedTimestamp()) {
		t.Errorf("reembedded_at = %v", activated["reembedded_at"])
	}
	if result.Settings["collection"] == result.StagingCollection {
		t.Errorf("ActivateReembed must not mutate its input")
	}
}

func TestUpdateSettingsChangedBaseURLClearsKey(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 3}
	result, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"embedding": map[string]any{"base_url": "https://other.example"}},
		Reembed:       true,
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, prober)
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	embedding, err := ModelConnectionForKey(result.Settings, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey() error: %v", err)
	}
	if embedding.BaseURL != "https://other.example" || embedding.APIKey != "" {
		t.Errorf("embedding = %+v (base_url change must clear the key)", embedding)
	}
}

func TestUpdateSettingsClearsAndChangesReranker(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 3}
	cleared, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"reranker_model": ""},
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, prober)
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	if cleared.Settings["reranker_model"] != "" || prober.rerankerCalls != 0 {
		t.Errorf("cleared reranker = %v, calls=%d", cleared.Settings["reranker_model"], prober.rerankerCalls)
	}
	storedReranker := cleared.Settings["reranker"].(map[string]any)
	if storedReranker["base_url"] != "" || storedReranker["protected_key"] != "" {
		t.Errorf("cleared stored reranker = %v", storedReranker)
	}

	prober = &fakeProber{vectorSize: 3}
	changed, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values: map[string]any{
			"reranker_model": "rank-b",
			"reranker":       map[string]any{"base_url": "https://rank.example", "apikey": "rk"},
		},
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, prober)
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	if changed.Settings["reranker_model"] != "rank-b" || prober.rerankerCalls != 1 {
		t.Errorf("changed reranker = %v, calls=%d", changed.Settings["reranker_model"], prober.rerankerCalls)
	}
	reranker, err := ModelConnectionForKey(changed.Settings, "reranker", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(reranker) error: %v", err)
	}
	if reranker.BaseURL != "https://rank.example" || reranker.APIKey != "rk" || reranker.Model != "rank-b" {
		t.Errorf("reranker = %+v", reranker)
	}
}

func TestUpdateSettingsValidationErrors(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	cases := []struct {
		name    string
		current map[string]any
		values  map[string]any
		message string
	}{
		{"missing current", nil, map[string]any{}, MessageKnowledgeBaseMissing},
		{"empty embedding model", current, map[string]any{"embedding_model": ""}, MessageEmbeddingModelRequired},
		{"non-string embedding model", current, map[string]any{"embedding_model": 5}, "模型名称必须是字符串"},
		{"non-object embedding draft", current, map[string]any{"embedding": "x"}, "模型服务配置格式无效"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := UpdateSettings(context.Background(), testCase.current, UpdateInput{
				Values: testCase.values, EncryptionKey: testEncryptionKey,
			}, &fakeProber{vectorSize: 3})
			assertValidationMessage(t, err, testCase.message)
		})
	}
}

func TestUpdateSettingsPropagatesProbeErrors(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	prober := &fakeProber{vectorSize: 3, embeddingError: &modelapi.ModelAPIError{Message: "模型服务请求超时"}}
	_, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values: map[string]any{}, EncryptionKey: testEncryptionKey,
	}, prober)
	if err == nil || err.Error() != "模型服务请求超时" {
		t.Fatalf("error = %v", err)
	}
}

func TestCreateSettingsStoresVLMProvider(t *testing.T) {
	prober := &fakeProber{vectorSize: 3}
	input := newTestCreateInput("http://host:8080/v1/")
	input.VLMModel = " qwen2.5-vl-7b "
	input.VLMValues = map[string]any{"base_url": "http://vlm:8000/v1/", "apikey": "vlm-key"}
	settings, err := CreateSettings(context.Background(), input, prober)
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	connection, err := ModelConnectionForKey(settings, "vlm", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(vlm) error: %v", err)
	}
	if connection.BaseURL != "http://vlm:8000/v1" || connection.Model != "qwen2.5-vl-7b" || connection.APIKey != "vlm-key" {
		t.Fatalf("vlm connection = %+v", connection)
	}
	public := PublicSettings(settings)
	vlm, ok := public["vlm"].(map[string]any)
	if !ok || vlm["base_url"] != "http://vlm:8000/v1" || vlm["api_key_set"] != true {
		t.Fatalf("public vlm = %v", public["vlm"])
	}
	if public["vlm_model"] != "qwen2.5-vl-7b" {
		t.Fatalf("public vlm_model = %v", public["vlm_model"])
	}
}

func TestCreateSettingsRejectsIncompleteVLM(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*CreateInput)
		message string
	}{
		"model without url": {
			mutate:  func(input *CreateInput) { input.VLMModel = "qwen2.5-vl-7b" },
			message: MessageVLMURLRequired,
		},
		"url without model": {
			mutate:  func(input *CreateInput) { input.VLMValues = map[string]any{"base_url": "http://vlm:8000/v1"} },
			message: MessageVLMModelRequired,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			input := newTestCreateInput("http://host:8080/v1/")
			testCase.mutate(&input)
			_, err := CreateSettings(context.Background(), input, &fakeProber{vectorSize: 3})
			assertValidationMessage(t, err, testCase.message)
		})
	}
}

func TestCreateSettingsDoesNotWriteOCRModel(t *testing.T) {
	input := newTestCreateInput("http://host:8080/v1/")
	settings, err := CreateSettings(context.Background(), input, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	if _, present := settings["ocr_model"]; present {
		t.Fatalf("CreateSettings wrote ocr_model = %v", settings["ocr_model"])
	}
	if _, present := settings["ocr"]; present {
		t.Fatalf("CreateSettings wrote an OCR provider object = %v", settings["ocr"])
	}
	public := PublicSettings(settings)
	if _, present := public["ocr_model"]; present {
		t.Fatalf("public settings exposed ocr_model = %v", public["ocr_model"])
	}
	if _, projected := public["ocr"]; projected {
		t.Fatalf("public projection exposed an OCR provider: %v", public["ocr"])
	}
}

func TestUpdateSettingsStripsLegacyOCRKeys(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	// Rows written by versions that still supported a local OCR model carry
	// these keys; any update rewrites the settings without them.
	current["ocr"] = map[string]any{"base_url": "http://legacy.local"}
	current["ocr_model"] = "easyocr"
	result, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"embedding_model": "embed-a"},
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	if _, present := result.Settings["ocr"]; present {
		t.Fatalf("legacy ocr setting survived the update: %v", result.Settings["ocr"])
	}
	if _, present := result.Settings["ocr_model"]; present {
		t.Fatalf("legacy ocr_model setting survived the update: %v", result.Settings["ocr_model"])
	}
}
func TestUpdateSettingsKeepsVLMWhenOmitted(t *testing.T) {
	input := newTestCreateInput("http://host:8080/v1/")
	input.VLMModel = "qwen2.5-vl-7b"
	input.VLMValues = map[string]any{"base_url": "http://vlm:8000/v1", "apikey": "vlm-key"}
	current, err := CreateSettings(context.Background(), input, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	result, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"embedding_model": "embed-a", "reranker_model": ""},
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	connection, err := ModelConnectionForKey(result.Settings, "vlm", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(vlm) error: %v", err)
	}
	if connection.BaseURL != "http://vlm:8000/v1" || connection.Model != "qwen2.5-vl-7b" || connection.APIKey != "vlm-key" {
		t.Fatalf("vlm connection after unrelated update = %+v", connection)
	}

	cleared, err := UpdateSettings(context.Background(), result.Settings, UpdateInput{
		Values:        map[string]any{"vlm": map[string]any{"base_url": "", "apikey": ""}, "vlm_model": ""},
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("UpdateSettings(clear) error: %v", err)
	}
	clearedConnection, err := ModelConnectionForKey(cleared.Settings, "vlm", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(cleared) error: %v", err)
	}
	if clearedConnection.Configured() {
		t.Fatalf("cleared vlm connection = %+v", clearedConnection)
	}
}
