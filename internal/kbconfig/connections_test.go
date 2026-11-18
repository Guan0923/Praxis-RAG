package kbconfig

import (
	"errors"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

func TestStoredConnectionProtectsKeys(t *testing.T) {
	connection := modelapi.ModelConnection{BaseURL: "http://host:8080/v1", APIKey: "secret-key"}
	stored, err := StoredConnectionWithKey(connection, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error: %v", err)
	}
	if stored["base_url"] != "http://host:8080/v1" {
		t.Errorf("base_url = %v", stored["base_url"])
	}
	protected, _ := stored["protected_key"].(string)
	if !strings.HasPrefix(protected, "enc:v1:") {
		t.Fatalf("protected_key = %q, want enc:v1: prefix", protected)
	}
	if strings.Contains(protected, "secret-key") {
		t.Fatalf("protected_key leaked plaintext: %q", protected)
	}
}

func TestStoredConnectionKeepsEmptyKeyEmpty(t *testing.T) {
	stored, err := StoredConnectionWithKey(modelapi.ModelConnection{BaseURL: "http://host"}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey() error: %v", err)
	}
	if stored["protected_key"] != "" {
		t.Errorf("protected_key = %v, want empty", stored["protected_key"])
	}
}

func TestModelConnectionForRoundTrip(t *testing.T) {
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", testEncryptionKey)
	settings := testStoredSettings(t, "http://host:8080/v1", "secret-key")
	embedding, err := ModelConnectionForKey(settings, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(embedding) error: %v", err)
	}
	if embedding.BaseURL != "http://host:8080/v1" || embedding.Model != "embed-a" || embedding.APIKey != "secret-key" {
		t.Errorf("embedding connection = %+v", embedding)
	}
	if embedding.VectorSize == nil || *embedding.VectorSize != 3 {
		t.Errorf("embedding vector size = %v, want 3", embedding.VectorSize)
	}
	reranker, err := ModelConnectionForKey(settings, "reranker", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(reranker) error: %v", err)
	}
	if reranker.Model != "rank-a" || reranker.APIKey != "" {
		t.Errorf("reranker connection = %+v", reranker)
	}
	if reranker.VectorSize != nil {
		t.Errorf("reranker vector size must be nil, got %v", *reranker.VectorSize)
	}
	key, err := APIKeyFor(settings, "embedding")
	if err != nil {
		t.Fatalf("APIKeyFor() error: %v", err)
	}
	if key != "secret-key" {
		t.Errorf("APIKeyFor() = %q", key)
	}
}


func TestModelConnectionForEncryptedWithoutKeyFails(t *testing.T) {
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", "")
	settings := testStoredSettings(t, "http://host", "secret-key")
	_, err := ModelConnectionFor(settings, "embedding")
	var modelError *modelapi.ModelAPIError
	if err == nil || !errors.As(err, &modelError) {
		t.Fatalf("error = %v, want ModelAPIError", err)
	}
	if err.Error() != "读取 apikey 需要设置 RAG_STATE_ENCRYPTION_KEY" {
		t.Errorf("error = %q", err)
	}
}

func TestModelConnectionForEnvironmentKey(t *testing.T) {
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", testEncryptionKey)
	settings := testStoredSettings(t, "http://host", "env-key")
	connection, err := ModelConnectionFor(settings, "embedding")
	if err != nil {
		t.Fatalf("ModelConnectionFor() error: %v", err)
	}
	if connection.APIKey != "env-key" {
		t.Errorf("APIKey = %q", connection.APIKey)
	}
}

func TestModelConnectionForLegacyPlainKey(t *testing.T) {
	settings := map[string]any{
		"embedding_model": "embed-a",
		"vector_size":     3,
		"embedding":       map[string]any{"base_url": "http://host", "protected_key": "private-file:legacy-key"},
		"reranker_model":  "",
		"reranker":        map[string]any{"base_url": "", "protected_key": ""},
	}
	connection, err := ModelConnectionFor(settings, "embedding")
	if runtime.GOOS == "windows" {
		// private-file: values are rejected on Windows without leaking them.
		if err == nil {
			t.Fatalf("expected legacy private-file key to be rejected on Windows")
		}
		return
	}
	if err != nil {
		t.Fatalf("ModelConnectionFor() error: %v", err)
	}
	if connection.APIKey != "legacy-key" {
		t.Errorf("unexpected legacy key %q", connection.APIKey)
	}
}

func TestModelConnectionForMissingSettings(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]any
		kind     string
		want     string
	}{
		{"missing model", map[string]any{"vector_size": 3}, "embedding", `knowledge-base settings are missing "embedding_model"`},
		{"missing vector size", map[string]any{"embedding_model": "m"}, "embedding", `knowledge-base settings are missing "vector_size"`},
		{"bad vector size", map[string]any{"embedding_model": "m", "vector_size": 2.5}, "embedding",
			`knowledge-base settings "vector_size" must be an integer`},
		{"bad provider", map[string]any{"embedding_model": "m", "vector_size": 3, "embedding": "x"}, "embedding",
			`knowledge-base settings "embedding" must be an object`},
		{"unknown kind", map[string]any{}, "sql", `unknown model kind "sql"`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ModelConnectionFor(testCase.settings, testCase.kind)
			if err == nil || err.Error() != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestDraftConnectionRules(t *testing.T) {
	current := modelapi.ModelConnection{BaseURL: "http://host:8080/v1", APIKey: "stored-key"}
	cases := []struct {
		name       string
		values     map[string]any
		wantURL    string
		wantKey    string
		wantErrMsg string
	}{
		{"nil draft keeps everything", nil, "http://host:8080/v1", "stored-key", ""},
		{"empty draft keeps everything", map[string]any{}, "http://host:8080/v1", "stored-key", ""},
		{"trailing slash normalized", map[string]any{"base_url": "http://host:8080/v1/"}, "http://host:8080/v1", "stored-key", ""},
		{"changed base url clears key", map[string]any{"base_url": "https://other.example"}, "https://other.example", "", ""},
		{"changed base url with explicit key", map[string]any{"base_url": "https://other.example", "apikey": "new-key"},
			"https://other.example", "new-key", ""},
		{"explicit empty key clears", map[string]any{"apikey": ""}, "http://host:8080/v1", "", ""},
		{"explicit key without url", map[string]any{"apikey": "new-key"}, "http://host:8080/v1", "new-key", ""},
		{"crlf key rejected", map[string]any{"apikey": "a\r\nb"}, "", "", "apikey 格式无效"},
		{"non-string key rejected", map[string]any{"apikey": 5}, "", "", "apikey 格式无效"},
		{"empty base url rejected", map[string]any{"base_url": ""}, "", "",
			"base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段"},
		{"null base url rejected", map[string]any{"base_url": nil}, "", "",
			"base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段"},
		{"ftp scheme rejected", map[string]any{"base_url": "ftp://host"}, "", "",
			"base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			connection, err := DraftConnection(current, testCase.values)
			if testCase.wantErrMsg != "" {
				if err == nil || err.Error() != testCase.wantErrMsg {
					t.Fatalf("error = %v, want %q", err, testCase.wantErrMsg)
				}
				return
			}
			if err != nil {
				t.Fatalf("DraftConnection() error: %v", err)
			}
			if connection.BaseURL != testCase.wantURL || connection.APIKey != testCase.wantKey {
				t.Errorf("connection = %+v, want url=%q key=%q", connection, testCase.wantURL, testCase.wantKey)
			}
			if connection.Model != "" || connection.VectorSize != nil {
				t.Errorf("draft connection must not carry model/vector size: %+v", connection)
			}
		})
	}
}

func TestDraftConnectionRejectsInvalidCurrentURL(t *testing.T) {
	current := modelapi.ModelConnection{BaseURL: "not a url"}
	if _, err := DraftConnection(current, map[string]any{}); err == nil ||
		err.Error() != "base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段" {
		t.Fatalf("error = %v", err)
	}
}

func TestIsKind(t *testing.T) {
	if !IsKind("embedding") || !IsKind("reranker") || !IsKind("vlm") || !IsKind("jev") {
		t.Errorf("expected embedding/reranker/vlm/jev kinds")
	}
	if IsKind("") || IsKind("Embedding") || IsKind("bm25") || IsKind("ocr") {
		t.Errorf("unexpected kind accepted")
	}
	if !reflect.DeepEqual(Kinds, []string{"embedding", "reranker", "vlm", "jev"}) {
		t.Errorf("Kinds = %v", Kinds)
	}
	// The public projection only replaces the kinds that own a knowledge-base
	// provider connection; JEV is a Console-only configuration.
	if !reflect.DeepEqual(CredentialKinds, []string{"embedding", "reranker", "vlm"}) {
		t.Errorf("CredentialKinds = %v", CredentialKinds)
	}
}
