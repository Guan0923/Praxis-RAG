package retrieval

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

// fakeJevScorer is a deterministic, offline stand-in for the hosted decision
// model. It returns the configured score per candidate ID and records every
// request so tests can assert what each round actually sent.
type fakeJevScorer struct {
	mu       sync.Mutex
	calls    []modelapi.JevScoreRequest
	scores   map[string]float64
	fallback float64
	skipped  int
	err      error
}

func (f *fakeJevScorer) Score(_ context.Context, request modelapi.JevScoreRequest) (modelapi.JevScoreResult, error) {
	f.mu.Lock()
	f.calls = append(f.calls, request)
	f.mu.Unlock()
	if f.err != nil {
		return modelapi.JevScoreResult{}, f.err
	}
	result := modelapi.JevScoreResult{
		Scores:    make([]float64, len(request.Candidates)),
		Judgments: len(request.Candidates),
		Requests:  1,
		Batches:   1,
		Skipped:   f.skipped,
	}
	for position, candidate := range request.Candidates {
		if score, ok := f.scores[candidate.ID]; ok {
			result.Scores[position] = score
			continue
		}
		result.Scores[position] = f.fallback
	}
	return result, nil
}

func (f *fakeJevScorer) allCalls() []modelapi.JevScoreRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	copied := make([]modelapi.JevScoreRequest, len(f.calls))
	copy(copied, f.calls)
	return copied
}

// fakeCorpusLoader serves a mutable corpus snapshot with an explicit revision.
type fakeCorpusLoader struct {
	mu     sync.Mutex
	corpus Sag2Corpus
	err    error
	calls  int
}

func (l *fakeCorpusLoader) Load(context.Context) (Sag2Corpus, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.calls++
	return l.corpus, l.err
}

func (l *fakeCorpusLoader) setCorpus(corpus Sag2Corpus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.corpus = corpus
}

func newSag2TestRetriever(t *testing.T, loader *fakeCorpusLoader, scorer JevScorer, options Sag2Options) *Sag2Retriever {
	t.Helper()
	return NewSag2Retriever(loader.Load, func(context.Context) (JevScorer, error) {
		return scorer, nil
	}, options)
}

func newSag2Fixture(t *testing.T, options Sag2Options, scores map[string]float64) (*Sag2Retriever, *fakeJevScorer, *fakeCorpusLoader) {
	t.Helper()
	loader := &fakeCorpusLoader{corpus: Sag2Corpus{Revision: "r1", Documents: lexicalTestDocs()}}
	scorer := &fakeJevScorer{scores: scores, fallback: 0.2}
	return newSag2TestRetriever(t, loader, scorer, options), scorer, loader
}

func sag2ResultByID(results []contracts.Candidate) map[string]contracts.Candidate {
	byID := make(map[string]contracts.Candidate, len(results))
	for _, result := range results {
		byID[result.ChunkID] = result
	}
	return byID
}

func sag2CallContains(request modelapi.JevScoreRequest, id string) bool {
	for _, candidate := range request.Candidates {
		if candidate.ID == id {
			return true
		}
	}
	return false
}

func TestSag2OptionsDefaults(t *testing.T) {
	defaults := normalizeSag2Options(Sag2Options{})
	if defaults.MaxHops != 2 || defaults.SeedK != 10 || defaults.ExpansionTerms != 20 || defaults.FinalDPP != 10 {
		t.Fatalf("defaults = %+v, want 2/10/20/10", defaults)
	}
	clamped := normalizeSag2Options(Sag2Options{MaxHops: 9, SeedK: -1, ExpansionTerms: 0, FinalDPP: 0})
	if clamped.MaxHops != 2 || clamped.SeedK != 10 || clamped.ExpansionTerms != 20 || clamped.FinalDPP != 10 {
		t.Fatalf("clamped = %+v, want 2/10/20/10", clamped)
	}
	explicit := normalizeSag2Options(Sag2Options{MaxHops: 1, SeedK: 3, ExpansionTerms: 5, FinalDPP: 2})
	if explicit.MaxHops != 1 || explicit.SeedK != 3 || explicit.ExpansionTerms != 5 || explicit.FinalDPP != 2 {
		t.Fatalf("explicit = %+v, want 1/3/5/2", explicit)
	}
	var _ Retriever = NewSag2Retriever(nil, nil, Sag2Options{})
}

func TestSag2EmptyCorpusAndEmptyQuery(t *testing.T) {
	loader := &fakeCorpusLoader{corpus: Sag2Corpus{Revision: "empty"}}
	scorerFactoryCalls := 0
	retriever := NewSag2Retriever(loader.Load, func(context.Context) (JevScorer, error) {
		scorerFactoryCalls++
		return &fakeJevScorer{}, nil
	}, Sag2Options{})
	results, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 5, nil)
	if err != nil {
		t.Fatalf("Search(empty corpus) error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(empty corpus) = %+v, want empty", results)
	}
	if scorerFactoryCalls != 0 {
		t.Fatalf("scorer factory called %d times for an empty corpus, want 0", scorerFactoryCalls)
	}

	// A question with no lexical terms must not call the scorer either.
	fixture, scorer, _ := newSag2Fixture(t, Sag2Options{}, map[string]float64{})
	results, err = fixture.Search(context.Background(), "？！。", 5, nil)
	if err != nil {
		t.Fatalf("Search(empty query) error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(empty query) = %+v, want empty", results)
	}
	if calls := scorer.allCalls(); len(calls) != 0 {
		t.Fatalf("scorer called %d times for an empty query, want 0", len(calls))
	}
}

func TestSag2InitialRoundAndScoreMapping(t *testing.T) {
	retriever, scorer, _ := newSag2Fixture(t, Sag2Options{MaxHops: 1}, map[string]float64{"a1": 0.6, "b1": 0.9})
	results, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	calls := scorer.allCalls()
	if len(calls) < 2 {
		t.Fatalf("scorer called %d times, want at least the initial and one expansion round", len(calls))
	}
	initial := calls[0]
	if len(initial.Evidence) != 0 {
		t.Fatalf("initial round evidence = %+v, want empty", initial.Evidence)
	}
	if len(initial.Candidates) != 1 || initial.Candidates[0].ID != "a1" {
		t.Fatalf("initial candidates = %+v, want only a1", initial.Candidates)
	}
	if initial.Question != "Alice 出生在哪里？" {
		t.Fatalf("initial question = %q, want the original question", initial.Question)
	}
	byID := sag2ResultByID(results)
	a1, ok := byID["a1"]
	if !ok {
		t.Fatalf("result is missing a1: %+v", results)
	}
	if a1.Score != 0.6 {
		t.Fatalf("a1 score = %v, want the JEV score 0.6", a1.Score)
	}
	if a1.DocumentID != "doc-a" || a1.Source != "a.md" || a1.Title != "Alice 出生地" || !strings.Contains(a1.Text, "蒙特贝洛") {
		t.Fatalf("a1 citation fields were not projected: %+v", a1)
	}
	if a1.Metadata["topic"] != "geo" {
		t.Fatalf("a1 metadata = %+v, want the document metadata copy", a1.Metadata)
	}
	// The highest JEV score wins the first position, which the DPP pass must
	// preserve.
	if len(results) == 0 || results[0].ChunkID != "b1" || results[0].Score != 0.9 {
		t.Fatalf("first result = %+v, want b1 with score 0.9", results)
	}
	// Metadata is a copy: mutating the result must not corrupt the cached index.
	byID["a1"].Metadata["topic"] = "mutated"
	again, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err != nil {
		t.Fatalf("second Search() error: %v", err)
	}
	if topic := sag2ResultByID(again)["a1"].Metadata["topic"]; topic != "geo" {
		t.Fatalf("cached metadata leaked a mutation: %v", topic)
	}
}

func TestSag2ExpansionRecallsBridgePassage(t *testing.T) {
	retriever, scorer, _ := newSag2Fixture(t, Sag2Options{MaxHops: 1}, map[string]float64{"a1": 0.6, "b1": 0.9})
	results, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	calls := scorer.allCalls()
	if len(calls) < 2 {
		t.Fatalf("scorer called %d times, want an expansion round", len(calls))
	}
	if sag2CallContains(calls[0], "b1") {
		t.Fatal("bridge passage b1 was retrieved by the initial query terms")
	}
	if !sag2CallContains(calls[1], "b1") {
		t.Fatalf("expansion round candidates = %+v, want the bridge passage b1", calls[1].Candidates)
	}
	if _, ok := sag2ResultByID(results)["b1"]; !ok {
		t.Fatalf("results are missing the bridge passage b1: %+v", results)
	}
}

func TestSag2SeenPassagesAreUniquePerRound(t *testing.T) {
	retriever, scorer, _ := newSag2Fixture(t, Sag2Options{MaxHops: 2}, map[string]float64{"a1": 0.6, "b1": 0.9})
	if _, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil); err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	calls := scorer.allCalls()
	if len(calls) < 2 {
		t.Fatalf("scorer called %d times, want expansion rounds", len(calls))
	}
	for index, call := range calls {
		seen := make(map[string]struct{}, len(call.Candidates))
		a1Count := 0
		for _, candidate := range call.Candidates {
			if _, duplicate := seen[candidate.ID]; duplicate {
				t.Fatalf("round %d scored %q twice: %+v", index, candidate.ID, call.Candidates)
			}
			seen[candidate.ID] = struct{}{}
			if candidate.ID == "a1" {
				a1Count++
			}
		}
		if a1Count != 1 {
			t.Fatalf("round %d scored a1 %d times, want exactly once: %+v", index, a1Count, call.Candidates)
		}
	}
}

func TestSag2FiltersApplyBeforeRecall(t *testing.T) {
	retriever, scorer, _ := newSag2Fixture(t, Sag2Options{MaxHops: 2}, map[string]float64{"b1": 0.9})
	results, err := retriever.Search(context.Background(), "蒙特贝洛", 10, map[string]any{"document_id": "doc-b"})
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	if len(results) != 1 || results[0].ChunkID != "b1" {
		t.Fatalf("filtered results = %+v, want only b1", results)
	}
	calls := scorer.allCalls()
	if len(calls) == 0 {
		t.Fatal("scorer was never called")
	}
	for _, candidate := range calls[0].Candidates {
		if candidate.ID != "b1" {
			t.Fatalf("initial round leaked a filtered passage into the budget: %+v", calls[0].Candidates)
		}
	}
}

func TestSag2FinalRoundUsesPreviousScores(t *testing.T) {
	// A wide term budget keeps the second expansion productive so the third
	// scoring round (the configured final round) actually runs.
	options := Sag2Options{MaxHops: 2, ExpansionTerms: 200}
	retriever, scorer, _ := newSag2Fixture(t, options, map[string]float64{"a1": 0.6, "b1": 0.9})
	if _, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil); err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	calls := scorer.allCalls()
	if len(calls) != 3 {
		t.Fatalf("scorer called %d times, want three scoring rounds", len(calls))
	}
	intermediate := calls[1]
	if len(intermediate.Evidence) == 0 {
		t.Fatal("intermediate round evidence is empty")
	}
	for _, evidence := range intermediate.Evidence {
		if evidence.PreviousScore != nil {
			t.Fatalf("intermediate evidence %q carries a previous score", evidence.ID)
		}
	}
	final := calls[2]
	if len(final.Evidence) == 0 {
		t.Fatal("final round evidence is empty")
	}
	for _, evidence := range final.Evidence {
		if evidence.PreviousScore == nil {
			t.Fatalf("final evidence %q has no previous score", evidence.ID)
		}
	}
	if got := len(final.Evidence); got > sag2EvidenceTopK {
		t.Fatalf("final evidence size = %d, want at most %d", got, sag2EvidenceTopK)
	}
}

func TestSag2DPPPreservesFirstPassageAndSet(t *testing.T) {
	index := newLexicalTestIndex(t)
	candidates := []*sag2Candidate{
		{key: "b1", index: 1, chunkID: "b1", jevScore: 0.90},
		{key: "c1", index: 2, chunkID: "c1", jevScore: 0.88},
		{key: "a1", index: 0, chunkID: "a1", jevScore: 0.80},
	}
	reordered := sag2DPPOrder(index, candidates)
	if len(reordered) != len(candidates) {
		t.Fatalf("DPP returned %d passages, want %d", len(reordered), len(candidates))
	}
	if reordered[0].chunkID != "b1" {
		t.Fatalf("DPP first passage = %q, want b1", reordered[0].chunkID)
	}
	want := map[string]bool{"a1": true, "b1": true, "c1": true}
	for _, candidate := range reordered {
		if !want[candidate.chunkID] {
			t.Fatalf("DPP invented passage %q", candidate.chunkID)
		}
		delete(want, candidate.chunkID)
	}
	if len(want) != 0 {
		t.Fatalf("DPP dropped passages: %+v", want)
	}
	// Numerically zero gains keep the incoming order.
	zero := []*sag2Candidate{
		{key: "b1", index: 1, chunkID: "b1"},
		{key: "c1", index: 2, chunkID: "c1"},
		{key: "a1", index: 0, chunkID: "a1"},
	}
	kept := sag2DPPOrder(index, zero)
	for position, candidate := range kept {
		if candidate.chunkID != zero[position].chunkID {
			t.Fatalf("zero-score DPP reordered %v to %v", zero[position].chunkID, candidate.chunkID)
		}
	}
}

func TestSag2ScorerErrorsArePropagated(t *testing.T) {
	apiFailure := &modelapi.ModelAPIError{Message: "jev offline"}
	loader := &fakeCorpusLoader{corpus: Sag2Corpus{Revision: "r1", Documents: lexicalTestDocs()}}
	retriever := newSag2TestRetriever(t, loader, &fakeJevScorer{err: apiFailure}, Sag2Options{MaxHops: 1})
	_, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err == nil {
		t.Fatal("Search() returned no error for a failing JEV scorer")
	}
	var shaped *modelapi.ModelAPIError
	if !errors.As(err, &shaped) || shaped.Message != "jev offline" {
		t.Fatalf("JEV error was not preserved: %T %v", err, err)
	}

	generic := errors.New("socket closed")
	retriever = newSag2TestRetriever(t, loader, &fakeJevScorer{err: generic}, Sag2Options{MaxHops: 1})
	_, err = retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err == nil {
		t.Fatal("Search() returned no error for a generic scorer failure")
	}
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("generic scorer error was not wrapped as an operation error: %T %v", err, err)
	}
	if operation.Message != "SAG2 retrieval failed: socket closed" {
		t.Fatalf("wrapped message = %q", operation.Message)
	}
}

func TestSag2LoaderErrorsArePropagated(t *testing.T) {
	loader := &fakeCorpusLoader{err: contracts.NewRagOperationError("corpus unavailable")}
	retriever := newSag2TestRetriever(t, loader, &fakeJevScorer{}, Sag2Options{})
	_, err := retriever.Search(context.Background(), "Alice", 10, nil)
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) || operation.Message != "corpus unavailable" {
		t.Fatalf("operation error was not preserved: %T %v", err, err)
	}

	loader = &fakeCorpusLoader{err: errors.New("bad snapshot")}
	retriever = newSag2TestRetriever(t, loader, &fakeJevScorer{}, Sag2Options{})
	_, err = retriever.Search(context.Background(), "Alice", 10, nil)
	if !errors.As(err, &operation) || operation.Message != "SAG2 retrieval failed: bad snapshot" {
		t.Fatalf("generic loader error was not wrapped: %T %v", err, err)
	}
}

func TestSag2RebuildsIndexOnRevisionChange(t *testing.T) {
	loader := &fakeCorpusLoader{corpus: Sag2Corpus{
		Revision:  "r1",
		Documents: lexicalTestDocs()[:1],
	}}
	scorer := &fakeJevScorer{fallback: 0.5}
	retriever := newSag2TestRetriever(t, loader, scorer, Sag2Options{MaxHops: 1})

	results, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err != nil {
		t.Fatalf("Search(r1) error: %v", err)
	}
	if len(results) != 1 || results[0].ChunkID != "a1" {
		t.Fatalf("Search(r1) = %+v, want a1", results)
	}

	loader.setCorpus(Sag2Corpus{
		Revision: "r2",
		Documents: []LexicalDoc{{
			ChunkID: "c1", DocumentID: "doc-c", Title: "Bob 面包店", Source: "c.md",
			Text: "Bob 是蒙特贝洛一家面包店的员工。",
		}},
	})
	results, err = retriever.Search(context.Background(), "Bob 面包店", 10, nil)
	if err != nil {
		t.Fatalf("Search(r2) error: %v", err)
	}
	if len(results) != 1 || results[0].ChunkID != "c1" {
		t.Fatalf("Search(r2) = %+v, want c1", results)
	}

	// The same revision reuses the cached index: restoring a1 under r2 does
	// not make it retrievable until the revision changes.
	loader.setCorpus(Sag2Corpus{Revision: "r2", Documents: lexicalTestDocs()})
	results, err = retriever.Search(context.Background(), "Alice 出生在哪里？", 10, nil)
	if err != nil {
		t.Fatalf("Search(cached r2) error: %v", err)
	}
	if len(results) != 0 {
		t.Fatalf("Search(cached r2) = %+v, want the cached r2 index (no a1)", results)
	}
}

func TestSag2SkippedCandidatesKeepZeroScore(t *testing.T) {
	loader := &fakeCorpusLoader{corpus: Sag2Corpus{Revision: "r1", Documents: lexicalTestDocs()}}
	scorer := &fakeJevScorer{scores: map[string]float64{"a1": 0}, fallback: 0.3, skipped: 1}
	retriever := newSag2TestRetriever(t, loader, scorer, Sag2Options{MaxHops: 1})
	results, err := retriever.Search(context.Background(), "Alice 出生在哪里？", 50, nil)
	if err != nil {
		t.Fatalf("Search() error: %v", err)
	}
	a1, ok := sag2ResultByID(results)["a1"]
	if !ok {
		t.Fatalf("skipped candidate a1 was dropped from the results: %+v", results)
	}
	if a1.Score != 0 {
		t.Fatalf("skipped candidate score = %v, want 0", a1.Score)
	}
}
