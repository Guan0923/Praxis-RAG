package modelapi

// Tests for the SAG extraction client: JSON parsing, entity normalization,
// and failure classification. Uses httptest servers like jev_test.go.
import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// extractionAnswer writes a chat-completion response whose message content is
// the given JSON string.
func extractionAnswer(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{
			map[string]any{
				"message": map[string]any{"role": "assistant", "content": content},
			},
		},
	})
}

func TestExtractionClientParsesEventAndEntities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer ext-key" {
			t.Errorf("Authorization = %q", got)
		}
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			t.Errorf("path = %q, want .../chat/completions", r.URL.Path)
		}
		extractionAnswer(w, `{"event": "Atlas 项目于 2024 年启动。", "entities": ["Atlas", "Atlas", "项目组", ""]}`)
	}))
	defer server.Close()
	client := NewExtractionClient(ModelConnection{BaseURL: server.URL, Model: "qwen-flash", APIKey: "ext-key"}, DefaultTimeout)
	result, err := client.Extract(context.Background(), ExtractionRequest{
		ChunkID: "c1",
		Title:   "Doc",
		Text:    "Project Atlas started in 2024.",
	})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if result.ChunkID != "c1" {
		t.Fatalf("ChunkID = %q", result.ChunkID)
	}
	if result.Event != "Atlas 项目于 2024 年启动。" {
		t.Fatalf("Event = %q", result.Event)
	}
	// Duplicates and blanks are dropped.
	if len(result.Entities) != 2 || result.Entities[0] != "Atlas" || result.Entities[1] != "项目组" {
		t.Fatalf("Entities = %v", result.Entities)
	}
}

func TestExtractionClientCapsEntities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		names := make([]string, 20)
		for i := range names {
			names[i] = strings.Repeat("e", i+1)
		}
		payload, _ := json.Marshal(map[string]any{"event": "e", "entities": names})
		extractionAnswer(w, string(payload))
	}))
	defer server.Close()
	client := NewExtractionClient(ModelConnection{BaseURL: server.URL, Model: "m"}, DefaultTimeout)
	result, err := client.Extract(context.Background(), ExtractionRequest{ChunkID: "c1", Text: "t"})
	if err != nil {
		t.Fatalf("Extract() error = %v", err)
	}
	if len(result.Entities) != 12 {
		t.Fatalf("Entities = %d, want the 12-cap", len(result.Entities))
	}
}

func TestExtractionClientRejectsInvalidPayloads(t *testing.T) {
	cases := []struct {
		name    string
		content string
	}{
		{"not json", "plain text"},
		{"empty event", `{"event": "", "entities": []}`},
		{"missing event", `{"entities": ["a"]}`},
		{"entities not strings", `{"event": "e", "entities": [1, 2]}`},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				extractionAnswer(w, testCase.content)
			}))
			defer server.Close()
			client := NewExtractionClient(ModelConnection{BaseURL: server.URL, Model: "m"}, DefaultTimeout)
			if _, err := client.Extract(context.Background(), ExtractionRequest{ChunkID: "c", Text: "t"}); err == nil {
				t.Fatalf("Extract() = nil error, want a model error for %q", testCase.content)
			}
		})
	}
}

func TestExtractionClientRejectsEmptyInput(t *testing.T) {
	client := NewExtractionClient(ModelConnection{BaseURL: "http://127.0.0.1:1", Model: "m"}, DefaultTimeout)
	if _, err := client.Extract(context.Background(), ExtractionRequest{ChunkID: "c", Text: "  "}); err == nil {
		t.Fatalf("Extract(blank) = nil error, want rejection")
	}
}
