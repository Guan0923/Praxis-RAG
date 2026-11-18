package retrieval

import (
	"context"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

type fakeCrossEncoderModel struct {
	scores []float64
	err    error
	calls  [][][2]string
}

func (m *fakeCrossEncoderModel) ComputeScore(_ context.Context, pairs [][2]string) ([]float64, error) {
	m.calls = append(m.calls, pairs)
	return m.scores, m.err
}

type factoryCall struct {
	modelName string
	device    string
	batchSize int
	maxLength int
}

func TestCrossEncoderLazilyRescoresSortsAndPreservesRetrievalScore(t *testing.T) {
	model := &fakeCrossEncoderModel{scores: []float64{0.1, 0.9}}
	calls := make([]factoryCall, 0, 1)
	factory := func(name, device string, batchSize, maxLength int) (CrossEncoderModel, error) {
		calls = append(calls, factoryCall{name, device, batchSize, maxLength})
		return model, nil
	}
	reranker, err := NewCrossEncoderReranker(CrossEncoderOptions{
		ModelName: "test-reranker", Device: "cpu", BatchSize: 2,
		MaxCandidates: 2, MaxTextChars: 256, ModelFactory: factory,
	})
	if err != nil {
		t.Fatalf("NewCrossEncoderReranker() error: %v", err)
	}
	candidates := []contracts.Candidate{
		{DocumentID: "first", ChunkID: "chunk-first", Title: "title-first", Source: "first.md",
			Text: "第一条", Score: 0.8, Metadata: map[string]any{"kind": "test"}},
		{DocumentID: "second", ChunkID: "chunk-second", Title: "title-second", Source: "second.md",
			Text: "第二条", Score: 0.2, Metadata: map[string]any{"kind": "test"}},
	}
	if len(calls) != 0 || reranker.IsLoaded() {
		t.Fatalf("model must load lazily: calls=%v loaded=%v", calls, reranker.IsLoaded())
	}
	results, err := reranker.Rerank(context.Background(), "测试问题", candidates)
	if err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	if !reranker.IsLoaded() {
		t.Fatalf("model must be loaded after rerank")
	}
	if len(results) != 2 || results[0].DocumentID != "second" || results[1].DocumentID != "first" {
		t.Fatalf("order = %+v", results)
	}
	if math.Abs(results[0].Score-0.9) > 1e-12 {
		t.Errorf("score = %v", results[0].Score)
	}
	if results[0].Metadata["retrieval_score"] != 0.2 {
		t.Errorf("retrieval_score = %v, want 0.2", results[0].Metadata["retrieval_score"])
	}
	if results[0].Metadata["kind"] != "test" {
		t.Errorf("metadata = %v", results[0].Metadata)
	}
	wantCall := factoryCall{"test-reranker", "cpu", 2, 512}
	if !reflect.DeepEqual(calls, []factoryCall{wantCall}) {
		t.Errorf("factory calls = %v, want %v", calls, []factoryCall{wantCall})
	}
	if len(model.calls) != 1 || len(model.calls[0]) != 2 {
		t.Fatalf("model calls = %v", model.calls)
	}
	if model.calls[0][0][0] != "测试问题" || model.calls[0][0][1] != "title-first\n第一条" {
		t.Errorf("pair = %v", model.calls[0][0])
	}
}

func TestCrossEncoderBoundsCandidateCountAndPassageCharacters(t *testing.T) {
	model := &fakeCrossEncoderModel{scores: []float64{0.7}}
	reranker, err := NewCrossEncoderReranker(CrossEncoderOptions{
		ModelName: "test-reranker", MaxCandidates: 1, MaxTextChars: 256,
		ModelFactory: func(string, string, int, int) (CrossEncoderModel, error) { return model, nil },
	})
	if err != nil {
		t.Fatalf("NewCrossEncoderReranker() error: %v", err)
	}
	candidates := []contracts.Candidate{
		{DocumentID: "first", ChunkID: "chunk-first", Title: "title-first", Text: strings.Repeat("甲", 1000),
			Score: 0.8, Metadata: map[string]any{}},
		{DocumentID: "tail", ChunkID: "chunk-tail", Title: "title-tail", Text: "不会发送给模型",
			Score: 0.6, Metadata: map[string]any{}},
	}
	results, err := reranker.Rerank(context.Background(), "问题", candidates)
	if err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	if len(results) != 2 || results[0].DocumentID != "first" || results[1].DocumentID != "tail" {
		t.Fatalf("results = %+v", results)
	}
	if len(model.calls) != 1 || len(model.calls[0]) != 1 {
		t.Fatalf("model calls = %v", model.calls)
	}
	if got := len([]rune(model.calls[0][0][1])); got != 256 {
		t.Errorf("passage length = %d, want 256", got)
	}
	// The tail candidate keeps its retrieval score and is never scored.
	if results[1].Score != 0.6 {
		t.Errorf("tail score = %v", results[1].Score)
	}
}

func TestCrossEncoderRejectsInvalidModelScores(t *testing.T) {
	cases := []struct {
		name string
		run  func(reranker *CrossEncoderReranker) error
	}{
		{"empty", func(reranker *CrossEncoderReranker) error {
			_, err := reranker.Rerank(context.Background(), "问题", oneCandidate())
			return err
		}},
		{"nan", func(reranker *CrossEncoderReranker) error {
			_, err := reranker.Rerank(context.Background(), "问题", oneCandidate())
			return err
		}},
		{"invalid", func(reranker *CrossEncoderReranker) error {
			_, err := reranker.Rerank(context.Background(), "问题", oneCandidate())
			return err
		}},
	}
	models := []*fakeCrossEncoderModel{
		{scores: []float64{}},
		{scores: []float64{math.NaN()}},
		{err: fmt.Errorf("invalid scores")},
	}
	for index, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			model := models[index]
			reranker, err := NewCrossEncoderReranker(CrossEncoderOptions{
				ModelName: "test-reranker",
				ModelFactory: func(string, string, int, int) (CrossEncoderModel, error) {
					return model, nil
				},
			})
			if err != nil {
				t.Fatalf("NewCrossEncoderReranker() error: %v", err)
			}
			err = testCase.run(reranker)
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "score") {
				t.Fatalf("error = %v, want a score validation error", err)
			}
		})
	}
}

func TestCrossEncoderOptionsValidation(t *testing.T) {
	cases := []struct {
		name    string
		options CrossEncoderOptions
		want    string
	}{
		{"empty model", CrossEncoderOptions{ModelName: " ", BatchSize: 4, MaxCandidates: 30, MaxLength: 512, MaxTextChars: 4000},
			"Reranker model name must not be empty"},
		{"batch size", CrossEncoderOptions{ModelName: "m", BatchSize: 65, MaxCandidates: 30, MaxLength: 512, MaxTextChars: 4000},
			"Reranker batch size must be between 1 and 64"},
		{"max candidates", CrossEncoderOptions{ModelName: "m", BatchSize: 4, MaxCandidates: 101, MaxLength: 512, MaxTextChars: 4000},
			"Reranker max candidates must be between 1 and 100"},
		{"max length", CrossEncoderOptions{ModelName: "m", BatchSize: 4, MaxCandidates: 30, MaxLength: 32, MaxTextChars: 4000},
			"Reranker max length must be between 64 and 8192"},
		{"max text chars", CrossEncoderOptions{ModelName: "m", BatchSize: 4, MaxCandidates: 30, MaxLength: 512, MaxTextChars: 10},
			"Reranker max text characters must be between 256 and 100000"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := NewCrossEncoderReranker(testCase.options)
			if err == nil || err.Error() != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestCrossEncoderWarmupAndEmptyCandidates(t *testing.T) {
	model := &fakeCrossEncoderModel{scores: []float64{}}
	loadCount := 0
	reranker, err := NewCrossEncoderReranker(CrossEncoderOptions{
		ModelName: "m",
		ModelFactory: func(string, string, int, int) (CrossEncoderModel, error) {
			loadCount++
			return model, nil
		},
	})
	if err != nil {
		t.Fatalf("NewCrossEncoderReranker() error: %v", err)
	}
	if err := reranker.Warmup(); err != nil {
		t.Fatalf("Warmup() error: %v", err)
	}
	if !reranker.IsLoaded() || loadCount != 1 {
		t.Errorf("warmup did not load the model: loaded=%v count=%d", reranker.IsLoaded(), loadCount)
	}
	results, err := reranker.Rerank(context.Background(), "q", nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("empty rerank = %v, %v", results, err)
	}
	if len(model.calls) != 0 {
		t.Errorf("empty candidates must not call the model")
	}
}

func TestCrossEncoderWithoutFactoryFailsSafely(t *testing.T) {
	reranker, err := NewCrossEncoderReranker(CrossEncoderOptions{ModelName: "m"})
	if err != nil {
		t.Fatalf("NewCrossEncoderReranker() error: %v", err)
	}
	_, err = reranker.Rerank(context.Background(), "q", oneCandidate())
	if err == nil {
		t.Fatalf("expected a missing-model error")
	}
	assertRagOperationError(t, err)
}

func TestHTTPCrossEncoderRerankerScoresThroughModelAPI(t *testing.T) {
	var rerankBody map[string]any
	modelServer, modelRequests := newFakeModelServer(t, func(body map[string]any) (int, string) {
		rerankBody = body
		return 200, `{"results":[{"index":1,"relevance_score":0.9},{"index":0,"relevance_score":0.1}]}`
	})
	reranker, err := NewHTTPCrossEncoderReranker(
		modelapi.ModelConnection{BaseURL: modelServer, Model: "rerank-model"}, CrossEncoderOptions{
			ModelName: "rerank-model", MaxCandidates: 2, MaxTextChars: 4000,
		}, 5,
	)
	if err != nil {
		t.Fatalf("NewHTTPCrossEncoderReranker() error: %v", err)
	}
	candidates := []contracts.Candidate{
		{DocumentID: "first", ChunkID: "chunk-first", Title: "t1", Text: "第一条", Score: 0.8, Metadata: map[string]any{}},
		{DocumentID: "second", ChunkID: "chunk-second", Title: "t2", Text: "第二条", Score: 0.2, Metadata: map[string]any{}},
		{DocumentID: "tail", ChunkID: "chunk-tail", Title: "t3", Text: "尾部", Score: 0.6, Metadata: map[string]any{}},
	}
	results, err := reranker.Rerank(context.Background(), "查询", candidates)
	if err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	if len(results) != 3 || results[0].DocumentID != "second" || results[1].DocumentID != "first" ||
		results[2].DocumentID != "tail" {
		t.Fatalf("order = %+v", results)
	}
	if results[0].Metadata["retrieval_score"] != 0.2 {
		t.Errorf("retrieval_score = %v", results[0].Metadata["retrieval_score"])
	}
	if len(modelRequests.all()) != 1 {
		t.Fatalf("model requests = %d", len(modelRequests.all()))
	}
	if rerankBody["model"] != "rerank-model" || rerankBody["top_n"] != float64(2) {
		t.Errorf("rerank body = %v", rerankBody)
	}
	documents := rerankBody["documents"].([]any)
	if !reflect.DeepEqual(documents, []any{"t1\n第一条", "t2\n第二条"}) {
		t.Errorf("documents = %v", documents)
	}
}

func oneCandidate() []contracts.Candidate {
	return []contracts.Candidate{
		{DocumentID: "doc", ChunkID: "chunk", Title: "title", Source: "doc.md", Text: "证据",
			Score: 0.4, Metadata: map[string]any{"kind": "test"}},
	}
}
