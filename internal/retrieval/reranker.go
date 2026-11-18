package retrieval

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

// CrossEncoderModel is the scoring seam behind the bounded cross-encoder
// reranker. The production implementation calls the HTTP model API; tests
// inject deterministic fakes.
type CrossEncoderModel interface {
	ComputeScore(ctx context.Context, pairs [][2]string) ([]float64, error)
}

// CrossEncoderModelFactory lazily builds the scoring model on first use.
type CrossEncoderModelFactory func(modelName, device string, batchSize, maxLength int) (CrossEncoderModel, error)

// CrossEncoderOptions mirrors the CrossEncoderReranker constructor arguments.
type CrossEncoderOptions struct {
	ModelName     string
	Device        string
	BatchSize     int
	MaxCandidates int
	MaxLength     int
	MaxTextChars  int
	ModelFactory  CrossEncoderModelFactory
}

// CrossEncoderReranker scores bounded query/passage pairs with one lazily
// loaded model, matching rag_mcp.rerankers.cross_encoder.
type CrossEncoderReranker struct {
	modelName     string
	device        string
	batchSize     int
	maxCandidates int
	maxLength     int
	maxTextChars  int
	factory       CrossEncoderModelFactory

	loadMu      sync.Mutex
	model       CrossEncoderModel
	inferenceMu sync.Mutex
}

// NewCrossEncoderReranker validates the bounded configuration; non-positive
// values fall back to the Python defaults.
func NewCrossEncoderReranker(options CrossEncoderOptions) (*CrossEncoderReranker, error) {
	if options.ModelName == "" {
		options.ModelName = config.DefaultRerankerModel
	}
	if options.BatchSize == 0 {
		options.BatchSize = 4
	}
	if options.MaxCandidates == 0 {
		options.MaxCandidates = 30
	}
	if options.MaxLength == 0 {
		options.MaxLength = 512
	}
	if options.MaxTextChars == 0 {
		options.MaxTextChars = 4000
	}
	if strings.TrimSpace(options.ModelName) == "" {
		return nil, fmt.Errorf("Reranker model name must not be empty")
	}
	if options.BatchSize < 1 || options.BatchSize > 64 {
		return nil, fmt.Errorf("Reranker batch size must be between 1 and 64")
	}
	if options.MaxCandidates < 1 || options.MaxCandidates > 100 {
		return nil, fmt.Errorf("Reranker max candidates must be between 1 and 100")
	}
	if options.MaxLength < 64 || options.MaxLength > 8192 {
		return nil, fmt.Errorf("Reranker max length must be between 64 and 8192")
	}
	if options.MaxTextChars < 256 || options.MaxTextChars > 100_000 {
		return nil, fmt.Errorf("Reranker max text characters must be between 256 and 100000")
	}
	return &CrossEncoderReranker{
		modelName:     strings.TrimSpace(options.ModelName),
		device:        options.Device,
		batchSize:     options.BatchSize,
		maxCandidates: options.MaxCandidates,
		maxLength:     options.MaxLength,
		maxTextChars:  options.MaxTextChars,
		factory:       options.ModelFactory,
	}, nil
}

// ModelName returns the configured reranker model.
func (r *CrossEncoderReranker) ModelName() string { return r.modelName }

// IsLoaded reports whether the scoring model has been constructed.
func (r *CrossEncoderReranker) IsLoaded() bool { return r.model != nil }

// Warmup builds the model without running inference.
func (r *CrossEncoderReranker) Warmup() error {
	_, err := r.modelInstance()
	return err
}

// Rerank replaces retrieval scores for a bounded prefix and preserves the tail.
func (r *CrossEncoderReranker) Rerank(
	ctx context.Context,
	query string,
	candidates []contracts.Candidate,
) ([]contracts.Candidate, error) {
	if len(candidates) == 0 {
		return []contracts.Candidate{}, nil
	}
	selectedCount := r.maxCandidates
	if selectedCount > len(candidates) {
		selectedCount = len(candidates)
	}
	selected := candidates[:selectedCount]
	pairs := make([][2]string, len(selected))
	for index, candidate := range selected {
		pairs[index] = [2]string{query, rerankerPassage(candidate, r.maxTextChars)}
	}
	model, err := r.modelInstance()
	if err != nil {
		return nil, err
	}
	r.inferenceMu.Lock()
	rawScores, err := model.ComputeScore(ctx, pairs)
	r.inferenceMu.Unlock()
	if err != nil {
		var operationError *contracts.RagOperationError
		if ok := asRagOperationError(err, &operationError); ok {
			return nil, err
		}
		return nil, contracts.NewRagOperationError(fmt.Sprintf("Cross-encoder reranking failed: %v", err))
	}
	scores, err := validateScores(rawScores, len(selected))
	if err != nil {
		return nil, err
	}
	rescored := make([]contracts.Candidate, len(selected))
	for index, candidate := range selected {
		metadata := copyMap(candidate.Metadata)
		metadata["retrieval_score"] = roundScore(candidate.Score, 6)
		candidate.Score = scores[index]
		candidate.Metadata = metadata
		rescored[index] = candidate
	}
	sort.SliceStable(rescored, func(i, j int) bool {
		if rescored[i].Score != rescored[j].Score {
			return rescored[i].Score > rescored[j].Score
		}
		if rescored[i].DocumentID != rescored[j].DocumentID {
			return rescored[i].DocumentID < rescored[j].DocumentID
		}
		return rescored[i].ChunkID < rescored[j].ChunkID
	})
	tail := candidates[selectedCount:]
	combined := make([]contracts.Candidate, 0, len(rescored)+len(tail))
	combined = append(combined, rescored...)
	combined = append(combined, tail...)
	return combined, nil
}

func (r *CrossEncoderReranker) modelInstance() (CrossEncoderModel, error) {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	if r.model != nil {
		return r.model, nil
	}
	if r.factory == nil {
		return nil, contracts.NewRagOperationError("FlagEmbedding is required for cross-encoder reranking")
	}
	model, err := r.factory(r.modelName, r.device, r.batchSize, r.maxLength)
	if err != nil {
		return nil, err
	}
	r.model = model
	return r.model, nil
}

// rerankerPassage builds the bounded title+text passage sent to the model.
func rerankerPassage(candidate contracts.Candidate, maxChars int) string {
	title := strings.TrimSpace(candidate.Title)
	combined := candidate.Text
	if title != "" {
		combined = title + "\n" + candidate.Text
	}
	runes := []rune(combined)
	if len(runes) <= maxChars {
		return combined
	}
	return string(runes[:maxChars])
}

// validateScores enforces the Cross Encoder score contract.
func validateScores(scores []float64, expected int) ([]float64, error) {
	if len(scores) != expected {
		return nil, contracts.NewRagOperationError("Cross-encoder score count does not match candidate count")
	}
	for _, score := range scores {
		if math.IsNaN(score) || math.IsInf(score, 0) {
			return nil, contracts.NewRagOperationError("Cross-encoder returned a non-finite score")
		}
	}
	return scores, nil
}

func asRagOperationError(err error, target **contracts.RagOperationError) bool {
	return errors.As(err, target)
}

// roundScore mirrors Python round(value, places) for the score values used by
// the retrieval pipeline.
func roundScore(value float64, places int) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	text := strconv.FormatFloat(value, 'f', places, 64)
	rounded, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return value
	}
	return rounded
}

// HTTPCrossEncoderModel adapts the model API rerank endpoint to the local
// cross-encoder scoring seam. Go has no in-process FlagEmbedding, so the
// cross-encoder stage always scores through the configured model service.
type HTTPCrossEncoderModel struct {
	Reranker *modelapi.HTTPReranker
}

// ComputeScore scores all pairs through one rerank request.
func (m *HTTPCrossEncoderModel) ComputeScore(ctx context.Context, pairs [][2]string) ([]float64, error) {
	if len(pairs) == 0 {
		return []float64{}, nil
	}
	query := pairs[0][0]
	documents := make([]string, len(pairs))
	for index, pair := range pairs {
		documents[index] = pair[1]
	}
	scored, err := m.Reranker.Scores(ctx, query, documents)
	if err != nil {
		return nil, err
	}
	scores := make([]float64, len(documents))
	for _, item := range scored {
		scores[item.Index] = item.Score
	}
	return scores, nil
}

// HTTPCrossEncoderFactory returns a lazy factory over an HTTP reranker.
func HTTPCrossEncoderFactory(reranker *modelapi.HTTPReranker) CrossEncoderModelFactory {
	return func(string, string, int, int) (CrossEncoderModel, error) {
		return &HTTPCrossEncoderModel{Reranker: reranker}, nil
	}
}

// NewHTTPCrossEncoderReranker builds the production cross-encoder reranker on
// top of the HTTP model API.
func NewHTTPCrossEncoderReranker(
	connection modelapi.ModelConnection,
	options CrossEncoderOptions,
	timeout float64,
) (*CrossEncoderReranker, error) {
	model := modelapi.NewHTTPReranker(connection, 30, 4000, timeout)
	if options.ModelName == "" {
		options.ModelName = connection.Model
	}
	options.ModelFactory = HTTPCrossEncoderFactory(model)
	return NewCrossEncoderReranker(options)
}
