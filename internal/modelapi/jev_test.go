package modelapi

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// jevAnswerPayload writes a valid response that echoes candidate_ keys with
// the given score.
func jevAnswerPayload(w http.ResponseWriter, r *http.Request, calls *int) {
	var body struct {
		Questions map[string]json.RawMessage `json:"questions"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	answers := make(map[string]map[string]any, len(body.Questions))
	for key := range body.Questions {
		answers[key] = map[string]any{"type": "noul", "noul": 0.5}
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"answers": answers,
		"model":   "jev-test",
		"usage":   map[string]any{"input_tokens": 100, "output_tokens": 10},
	})
}

func TestJevClientAppendsSystemOneEndpoint(t *testing.T) {
	client := NewJevClient(ModelConnection{BaseURL: "https://api.typesafe.ai/v1", Model: "jev-latest"}, DefaultTimeout)
	if client.endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("endpoint = %q", client.endpoint)
	}
	client = NewJevClient(ModelConnection{BaseURL: "https://api.typesafe.ai/v1/systemone", Model: "jev-latest"}, DefaultTimeout)
	if client.endpoint != "https://api.typesafe.ai/v1/systemone" {
		t.Fatalf("endpoint = %q", client.endpoint)
	}
	client = NewJevClient(ModelConnection{BaseURL: "https://api.example.com/api/alpha/decisions", Model: "jev-latest"}, DefaultTimeout)
	if client.endpoint != "https://api.example.com/api/alpha/decisions" {
		t.Fatalf("endpoint = %q", client.endpoint)
	}
}

func TestJevClientScoresCandidatesInOrder(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "Bearer jev-key" {
			t.Errorf("Authorization = %q", got)
		}
		jevAnswerPayload(w, r, &calls)
	}))
	defer server.Close()
	client := NewJevClient(ModelConnection{BaseURL: server.URL, Model: "jev-latest", APIKey: "jev-key"}, DefaultTimeout)
	result, err := client.Score(context.Background(), JevScoreRequest{
		Question: "Alice 出生在哪里？",
		Evidence: []JevEvidence{{ID: "c1", Title: "Bio", Passage: "Alice 出生于北京。"}},
		Candidates: []JevCandidate{
			{ID: "0", Title: "Bio", Passage: "Alice was born in Beijing."},
			{ID: "1", Title: "Doc", Passage: "Beijing is the capital."},
		},
	})
	if err != nil {
		t.Fatalf("Score error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("requests = %d", calls)
	}
	if len(result.Scores) != 2 || result.Scores[0] != 0.5 || result.Scores[1] != 0.5 {
		t.Fatalf("scores = %v", result.Scores)
	}
	if result.Judgments != 2 || result.Batches != 1 || result.InputTokens != 100 {
		t.Fatalf("result = %+v", result)
	}
}

func TestJevClientRetriesRetryableStatusThenSucceeds(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		jevAnswerPayload(w, r, &calls)
	}))
	defer server.Close()
	client := NewJevClient(ModelConnection{BaseURL: server.URL, Model: "jev-latest"}, DefaultTimeout)
	client.backoff = func(int) time.Duration { return 0 }
	result, err := client.Score(context.Background(), JevScoreRequest{
		Question:   "q",
		Candidates: []JevCandidate{{ID: "0", Title: "t", Passage: "p"}},
	})
	if err != nil {
		t.Fatalf("Score error = %v", err)
	}
	if calls != 2 {
		t.Fatalf("requests = %d", calls)
	}
	if result.Scores[0] != 0.5 {
		t.Fatalf("scores = %v", result.Scores)
	}
}

func TestJevClientStopsAfterAttemptCap(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	client := NewJevClient(ModelConnection{BaseURL: server.URL, Model: "jev-latest"}, DefaultTimeout)
	client.backoff = func(int) time.Duration { return 0 }
	_, err := client.Score(context.Background(), JevScoreRequest{
		Question:   "q",
		Candidates: []JevCandidate{{ID: "0", Title: "t", Passage: "p"}},
	})
	if err == nil {
		t.Fatal("expected an error")
	}
	if calls != jevMaxAttempts {
		t.Fatalf("requests = %d, want %d", calls, jevMaxAttempts)
	}
	var modelError *ModelAPIError
	if !asModelAPIError(err, &modelError) || modelError.Status != http.StatusServiceUnavailable {
		t.Fatalf("err = %v", err)
	}
}

func asModelAPIError(err error, target **ModelAPIError) bool {
	modelError, ok := err.(*ModelAPIError)
	if ok {
		*target = modelError
	}
	return ok
}

func TestJevClientRejectsMismatchedAnswers(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"answers": map[string]any{
				"candidate_9": map[string]any{"type": "noul", "noul": 1},
			},
			"model": "jev-test",
		})
	}))
	defer server.Close()
	client := NewJevClient(ModelConnection{BaseURL: server.URL, Model: "jev-latest"}, DefaultTimeout)
	_, err := client.Score(context.Background(), JevScoreRequest{
		Question:   "q",
		Candidates: []JevCandidate{{ID: "0", Title: "t", Passage: "p"}},
	})
	if err == nil || !strings.Contains(err.Error(), "答案数量") {
		t.Fatalf("err = %v", err)
	}
}

func TestJevClientSkipsOversizedCandidates(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		jevAnswerPayload(w, r, &calls)
	}))
	defer server.Close()
	client := NewJevClient(ModelConnection{BaseURL: server.URL, Model: "jev-latest"}, DefaultTimeout)
	huge := strings.Repeat("北京", 40000) // ~80000 runes ≈ 80000 estimated tokens
	result, err := client.Score(context.Background(), JevScoreRequest{
		Question:   "q",
		Candidates: []JevCandidate{{ID: "0", Title: "t", Passage: huge}, {ID: "1", Title: "t", Passage: "small"}},
	})
	if err != nil {
		t.Fatalf("Score error = %v", err)
	}
	if calls != 1 {
		t.Fatalf("requests = %d", calls)
	}
	if result.Skipped != 1 || result.Judgments != 1 {
		t.Fatalf("result = %+v", result)
	}
	if result.Scores[0] != 0 || result.Scores[1] != 0.5 {
		t.Fatalf("scores = %v", result.Scores)
	}
}

func TestJevClientBatchesLargeRounds(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		jevAnswerPayload(w, r, &calls)
	}))
	defer server.Close()
	client := NewJevClient(ModelConnection{BaseURL: server.URL, Model: "jev-latest"}, DefaultTimeout)
	candidates := make([]JevCandidate, 0, 64)
	for index := 0; index < 64; index++ {
		candidates = append(candidates, JevCandidate{
			ID:      "n",
			Title:   "title",
			Passage: strings.Repeat("段落", 600), // ~1200 runes ≈ 1200 tokens per question
		})
	}
	result, err := client.Score(context.Background(), JevScoreRequest{Question: "q", Candidates: candidates})
	if err != nil {
		t.Fatalf("Score error = %v", err)
	}
	if result.Batches < 2 {
		t.Fatalf("batches = %d, want a split", result.Batches)
	}
	if result.Judgments != 64 {
		t.Fatalf("judgments = %d", result.Judgments)
	}
	for _, score := range result.Scores {
		if score != 0.5 {
			t.Fatalf("scores = %v", result.Scores)
		}
	}
}
