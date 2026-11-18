package retrieval

// This file implements the SAG2 embedding-free multi-hop retriever: a lexical
// index supplies candidates, a JEV (TypeSafe System One / Noul) decision model
// scores how necessary each passage is for the answer chain, selected passages
// supply expansion terms, and newly retrieved evidence updates the scoring
// context. The flow is a port of the SAG2 reference implementation
// (SAG-Benchmark sag2/scripts/sag2.py, `search_sag2`).
//
// Declared divergences from the reference implementation:
//   - Multiword capitalized-name phrases, accent aliases and title-phrase
//     repair are not implemented; expansion units are single tokenizer tokens.
//   - The reference's query-pruning optimisation (IDF subset bound) and its
//     scoring-statistics cache are omitted; every round scores from scratch.
//   - The reference hard codes the final-round adjustment to "the third
//     scoring round" (hop == 2). This port applies it to the last configured
//     round (hop == MaxHops && hop > 0) so a 1-hop configuration also gets the
//     bounded final pool.
//   - JEV usage accounting (tokens, requests) is ignored; only per-candidate
//     scores are consumed.
//   - MaxHops is a per-field option: zero means the default of two expansion
//     rounds (three scoring rounds in total), and values above two are clamped,
//     because the reference supports at most two expansions.

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
	"sync"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

const (
	// sag2DefaultMaxHops is the reference's H = 2 (two expansion rounds after
	// the initial retrieval, i.e. at most three scoring rounds).
	sag2DefaultMaxHops = 2
	// sag2MaxHops is the reference's hard ceiling ("SAG2 supports at most two
	// expansion rounds").
	sag2MaxHops = 2
	// sag2DefaultSeedK is the reference's s = 10 expansion seeds per round.
	sag2DefaultSeedK = 10
	// sag2DefaultExpansionTerms is the reference's L = 20 terms per round.
	sag2DefaultExpansionTerms = 20
	// sag2DefaultFinalDPP is the reference's top-ten diversity reordering.
	sag2DefaultFinalDPP = 10
	// sag2ProtectedInitial is the reference's p = 5 initial passages that stay
	// in every scoring pool.
	sag2ProtectedInitial = 5
	// sag2FinalNewCandidates caps unseen passages admitted by the last round.
	sag2FinalNewCandidates = 20
	// sag2EvidenceTopK is the reference's ten-passage scoring context.
	sag2EvidenceTopK = 10
	// sag2FusionOffset is the reciprocal-rank offset. The reference stores
	// 1/(60+rho) for a one-based rank rho, equivalently 1/(61+zeroBasedRank).
	sag2FusionOffset = 61.0
)

// JevScorer is the necessary-evidence decision model used by SAG2. A
// modelapi.JevClient satisfies it; tests provide a deterministic fake.
type JevScorer interface {
	Score(ctx context.Context, request modelapi.JevScoreRequest) (modelapi.JevScoreResult, error)
}

// Sag2Corpus is one loaded corpus snapshot. Revision is a caller-supplied
// fingerprint: the retriever rebuilds its lexical index whenever the revision
// changes. An empty revision is legal and means "stable corpus".
type Sag2Corpus struct {
	Revision  string
	Documents []LexicalDoc
}

// CorpusLoader loads the current corpus snapshot for one search.
type CorpusLoader func(ctx context.Context) (Sag2Corpus, error)

// Sag2Options tunes the retrieval rounds. Zero values fall back to the
// reference defaults (2 hops / 10 seeds / 20 terms / top-10 DPP).
type Sag2Options struct {
	MaxHops        int // default 2 (three scoring rounds in total)
	SeedK          int // default 10
	ExpansionTerms int // default 20
	FinalDPP       int // default 10
}

// Sag2Retriever implements Retriever with the SAG2 lexical multi-hop flow.
// The lexical index is cached per corpus revision under a mutex; retrieving and
// scoring for one query never hold that mutex.
type Sag2Retriever struct {
	loader  CorpusLoader
	scorer  func(ctx context.Context) (JevScorer, error)
	options Sag2Options

	mu       sync.Mutex
	built    bool
	revision string
	index    *LexicalIndex
}

var _ Retriever = (*Sag2Retriever)(nil)

// NewSag2Retriever wires the corpus loader, the JEV scorer factory and the
// round options. The scorer factory is resolved once per search, so a missing
// configuration surfaces as an operation error instead of a panic.
func NewSag2Retriever(
	loader CorpusLoader,
	scorer func(ctx context.Context) (JevScorer, error),
	options Sag2Options,
) *Sag2Retriever {
	return &Sag2Retriever{
		loader:  loader,
		scorer:  scorer,
		options: normalizeSag2Options(options),
	}
}

func normalizeSag2Options(options Sag2Options) Sag2Options {
	if options.MaxHops <= 0 {
		options.MaxHops = sag2DefaultMaxHops
	}
	if options.MaxHops > sag2MaxHops {
		options.MaxHops = sag2MaxHops
	}
	if options.SeedK <= 0 {
		options.SeedK = sag2DefaultSeedK
	}
	if options.ExpansionTerms <= 0 {
		options.ExpansionTerms = sag2DefaultExpansionTerms
	}
	if options.FinalDPP <= 0 {
		options.FinalDPP = sag2DefaultFinalDPP
	}
	return options
}

// Search runs the bounded SAG2 retrieval procedure:
//
//  1. hop 0 scores the OR-query BM25 candidates with JEV and keeps the first
//     five passages protected.
//  2. each expansion round picks up to SeedK unexpanded seeds, derives up to
//     ExpansionTerms terms, interleaves the per-term result lists, and scores
//     the deduplicated pool with the shared evidence context.
//  3. the last round admits at most twenty unseen passages and fills the pool
//     with previously scored candidates.
//  4. the final ranking is sorted by JEV score, fusion rank and index; the top
//     passages are reordered by the local determinantal diversity rule.
func (r *Sag2Retriever) Search(
	ctx context.Context,
	query string,
	limit int,
	filters map[string]any,
) ([]contracts.Candidate, error) {
	if limit <= 0 {
		return []contracts.Candidate{}, nil
	}
	if r == nil || r.loader == nil {
		return nil, contracts.NewRagOperationError("SAG2 retrieval failed: corpus loader is not configured")
	}
	if r.scorer == nil {
		return nil, contracts.NewRagOperationError("SAG2 retrieval failed: JEV scorer is not configured")
	}
	corpus, err := r.loader(ctx)
	if err != nil {
		return nil, sag2Failure(err)
	}
	lexical := r.lexicalIndex(corpus)
	if lexical.Len() == 0 {
		return []contracts.Candidate{}, nil
	}
	queryTerms := lexicalQueryTerms(query)
	if len(queryTerms) == 0 {
		return []contracts.Candidate{}, nil
	}
	querySet := make(map[string]struct{}, len(queryTerms))
	for _, term := range queryTerms {
		querySet[term] = struct{}{}
	}
	excluded := make(map[string]struct{}, len(queryTerms))
	for key := range lexical.RejectedKeys(filters) {
		excluded[key] = struct{}{}
	}
	scorer, err := r.scorer(ctx)
	if err != nil {
		return nil, sag2Failure(err)
	}

	// hop 0: BM25 candidates for the OR query expression.
	hits := lexical.Search(queryTerms, excluded, limit)
	if len(hits) == 0 {
		return []contracts.Candidate{}, nil
	}
	initial := make([]*sag2Candidate, 0, len(hits))
	for rank, hit := range hits {
		initial = append(initial, newSag2Candidate(hit, 1/(sag2FusionOffset+float64(rank))))
	}
	protected := initial
	if len(protected) > sag2ProtectedInitial {
		protected = protected[:sag2ProtectedInitial]
	}

	options := r.options
	retained := make(map[string]*sag2Candidate, len(initial))
	latest := make(map[string]*sag2Candidate, len(initial))
	// excluded accumulates the filter-rejected keys plus every passage that has
	// already been recalled (the reference's `seen` set).
	expanded := make(map[string]struct{}, options.SeedK)
	carried := make([]*sag2Candidate, 0, options.SeedK*2)
	carriedSet := make(map[string]struct{}, options.SeedK*2)

	var frontier []*sag2Candidate
	var ranking []*sag2Candidate
	candidates := initial

	for hop := 0; hop <= options.MaxHops; hop++ {
		if hop > 0 && len(frontier) == 0 {
			break
		}
		var pool []*sag2Candidate
		var evidence []modelapi.JevEvidence
		if hop > 0 {
			terms := lexical.ExpansionTerms(sag2Seeds(frontier), querySet, options.ExpansionTerms)
			if len(terms) == 0 {
				break
			}
			candidates = r.expand(lexical, terms, queryTerms, excluded, limit)
			if len(candidates) == 0 {
				break
			}
		}
		// Seeds accumulate across rounds; lower-scoring seeds stay in the pool.
		for _, seed := range frontier {
			if _, duplicate := carriedSet[seed.key]; duplicate {
				continue
			}
			carriedSet[seed.key] = struct{}{}
			carried = append(carried, seed)
		}
		if hop == 0 {
			pool = candidates
		} else {
			pool = dedupeSag2(protected, carried, candidates)
			evidence = sag2Evidence(carried, candidates, options.SeedK)
			if hop == options.MaxHops {
				pool, evidence = sag2FinalRound(pool, latest, ranking)
			}
		}
		if len(pool) == 0 {
			break
		}
		result, err := scorer.Score(ctx, modelapi.JevScoreRequest{
			Question:   query,
			Evidence:   evidence,
			Candidates: sag2JevCandidates(pool),
		})
		if err != nil {
			return nil, sag2Failure(err)
		}
		if len(result.Scores) != len(pool) {
			return nil, contracts.NewRagOperationError("SAG2 retrieval failed: JEV score count does not match the candidate count")
		}
		for position, candidate := range pool {
			// A skipped candidate keeps the zero score the client reports.
			candidate.jevScore = result.Scores[position]
			latest[candidate.key] = candidate
		}
		retained = make(map[string]*sag2Candidate, len(pool))
		for _, candidate := range pool {
			retained[candidate.key] = candidate
			excluded[candidate.key] = struct{}{}
		}
		ranking = sag2Ranking(retained)
		frontier = sag2Frontier(ranking, expanded, options.SeedK)
		for _, seed := range frontier {
			expanded[seed.key] = struct{}{}
		}
	}

	final := ranking
	if len(final) > limit {
		final = final[:limit]
	}
	if len(final) > options.FinalDPP {
		reordered := sag2DPPOrder(lexical, final[:options.FinalDPP])
		final = append(reordered, final[options.FinalDPP:]...)
	}
	results := make([]contracts.Candidate, 0, len(final))
	for _, candidate := range final {
		results = append(results, contracts.Candidate{
			DocumentID: candidate.documentID,
			ChunkID:    candidate.chunkID,
			Title:      candidate.title,
			Source:     candidate.source,
			Text:       candidate.text,
			Score:      candidate.jevScore,
			Metadata:   copyMap(candidate.metadata),
		})
	}
	return results, nil
}

// lexicalIndex returns the cached index for the corpus revision, rebuilding it
// when the revision changes.
func (r *Sag2Retriever) lexicalIndex(corpus Sag2Corpus) *LexicalIndex {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.built && r.revision == corpus.Revision {
		return r.index
	}
	r.index = NewLexicalIndex(corpus.Documents)
	r.revision = corpus.Revision
	r.built = true
	return r.index
}

// expand retrieves each expansion term separately and interleaves the term
// lists by rank in term-priority order, removing duplicates, until limit
// passages are collected. Each term list is scored with the full ranking
// expression (expansion terms plus the original query terms) while matching the
// term itself is required by SearchTerm.
func (r *Sag2Retriever) expand(
	lexical *LexicalIndex,
	terms []ExpansionTerm,
	queryTerms []string,
	excluded map[string]struct{},
	limit int,
) []*sag2Candidate {
	rankingTerms := make([]string, 0, len(terms)+len(queryTerms))
	for _, term := range terms {
		rankingTerms = append(rankingTerms, term.Term)
	}
	rankingTerms = append(rankingTerms, queryTerms...)
	lists := make([][]LexicalHit, len(terms))
	for position, term := range terms {
		lists[position] = lexical.SearchTerm(term.Term, rankingTerms, excluded, limit)
	}
	expanded := make([]*sag2Candidate, 0, limit)
	seen := make(map[string]struct{}, limit)
	for rank := 0; rank < limit; rank++ {
		for position := range lists {
			if rank >= len(lists[position]) {
				continue
			}
			hit := lists[position][rank]
			if _, duplicate := seen[hit.Key]; duplicate {
				continue
			}
			seen[hit.Key] = struct{}{}
			expanded = append(expanded, newSag2Candidate(hit, 1/(sag2FusionOffset+float64(len(expanded)))))
			if len(expanded) == limit {
				return expanded
			}
		}
	}
	return expanded
}

// sag2Candidate is one passage carried through the retrieval rounds. Identity
// is the lexical key (ChunkID, or the synthetic "#index").
type sag2Candidate struct {
	key        string
	index      int
	chunkID    string
	documentID string
	title      string
	source     string
	text       string
	metadata   map[string]any
	jevScore   float64
	fusion     float64
}

func newSag2Candidate(hit LexicalHit, fusion float64) *sag2Candidate {
	return &sag2Candidate{
		key:        hit.Key,
		index:      hit.Index,
		chunkID:    hit.ChunkID,
		documentID: hit.DocumentID,
		title:      hit.Title,
		source:     hit.Source,
		text:       hit.Text,
		metadata:   hit.Metadata,
		fusion:     fusion,
	}
}

// lexicalQueryTerms tokenizes the question, removes duplicates in first-seen
// order and keeps at most 80 terms (the reference's OR-expression cap).
func lexicalQueryTerms(query string) []string {
	tokens := sparse.Tokenize(query)
	terms := make([]string, 0, len(tokens))
	seen := make(map[string]struct{}, len(tokens))
	for _, token := range tokens {
		if _, duplicate := seen[token]; duplicate {
			continue
		}
		seen[token] = struct{}{}
		terms = append(terms, token)
		if len(terms) >= lexicalMaxQueryTerms {
			break
		}
	}
	return terms
}

func sag2Seeds(frontier []*sag2Candidate) []ExpansionSeed {
	seeds := make([]ExpansionSeed, 0, len(frontier))
	for _, candidate := range frontier {
		seeds = append(seeds, ExpansionSeed{Index: candidate.index, Score: candidate.jevScore})
	}
	return seeds
}

func sag2JevCandidates(pool []*sag2Candidate) []modelapi.JevCandidate {
	candidates := make([]modelapi.JevCandidate, 0, len(pool))
	for _, candidate := range pool {
		candidates = append(candidates, modelapi.JevCandidate{
			ID:      candidate.key,
			Title:   candidate.title,
			Passage: candidate.text,
		})
	}
	return candidates
}

// dedupeSag2 concatenates groups keeping the first occurrence of each identity.
func dedupeSag2(groups ...[]*sag2Candidate) []*sag2Candidate {
	total := 0
	for _, group := range groups {
		total += len(group)
	}
	merged := make([]*sag2Candidate, 0, total)
	seen := make(map[string]struct{}, total)
	for _, group := range groups {
		for _, candidate := range group {
			if _, duplicate := seen[candidate.key]; duplicate {
				continue
			}
			seen[candidate.key] = struct{}{}
			merged = append(merged, candidate)
		}
	}
	return merged
}

// sag2Evidence builds the shared scoring context for an intermediate round:
// the accumulated seeds followed by the first SeedK new candidates.
func sag2Evidence(carried, candidates []*sag2Candidate, seedK int) []modelapi.JevEvidence {
	limited := candidates
	if len(limited) > seedK {
		limited = limited[:seedK]
	}
	group := dedupeSag2(carried, limited)
	evidence := make([]modelapi.JevEvidence, 0, len(group))
	for _, candidate := range group {
		evidence = append(evidence, modelapi.JevEvidence{
			ID:      candidate.key,
			Title:   candidate.title,
			Passage: candidate.text,
		})
	}
	return evidence
}

// sag2FinalRound applies the reference's last-round pool adjustment: keep the
// already scored members of the pool first, fill the remaining slots from the
// previously scored ranking, and admit at most twenty unseen passages. The
// scoring context becomes the previous ranking's top ten with their tentative
// scores attached.
func sag2FinalRound(
	pool []*sag2Candidate,
	latest map[string]*sag2Candidate,
	retainedRanking []*sag2Candidate,
) ([]*sag2Candidate, []modelapi.JevEvidence) {
	carriedCandidates := make([]*sag2Candidate, 0, len(pool))
	incoming := make([]*sag2Candidate, 0, sag2FinalNewCandidates)
	for _, candidate := range pool {
		if _, scored := latest[candidate.key]; scored {
			carriedCandidates = append(carriedCandidates, candidate)
			continue
		}
		if len(incoming) < sag2FinalNewCandidates {
			incoming = append(incoming, candidate)
		}
	}
	previous := sag2Ranking(latest)
	history := dedupeSag2(carriedCandidates, previous)
	tail := len(pool) - len(incoming)
	if tail < 0 {
		tail = 0
	}
	if tail > len(history) {
		tail = len(history)
	}
	adjusted := make([]*sag2Candidate, 0, tail+len(incoming))
	adjusted = append(adjusted, history[:tail]...)
	adjusted = append(adjusted, incoming...)

	top := retainedRanking
	if len(top) > sag2EvidenceTopK {
		top = top[:sag2EvidenceTopK]
	}
	evidence := make([]modelapi.JevEvidence, 0, len(top))
	for _, candidate := range top {
		previousScore := candidate.jevScore
		evidence = append(evidence, modelapi.JevEvidence{
			ID:            candidate.key,
			Title:         candidate.title,
			Passage:       candidate.text,
			PreviousScore: &previousScore,
		})
	}
	return adjusted, evidence
}

// sag2Ranking orders the scored passages by descending JEV score, descending
// fusion rank and ascending index (the reference's rowid tie break).
func sag2Ranking(candidates map[string]*sag2Candidate) []*sag2Candidate {
	ranking := make([]*sag2Candidate, 0, len(candidates))
	for _, candidate := range candidates {
		ranking = append(ranking, candidate)
	}
	sort.Slice(ranking, func(i, j int) bool {
		if ranking[i].jevScore != ranking[j].jevScore {
			return ranking[i].jevScore > ranking[j].jevScore
		}
		if ranking[i].fusion != ranking[j].fusion {
			return ranking[i].fusion > ranking[j].fusion
		}
		return ranking[i].index < ranking[j].index
	})
	return ranking
}

// sag2Frontier returns the first SeedK ranked passages that have not been
// expanded yet.
func sag2Frontier(ranking []*sag2Candidate, expanded map[string]struct{}, seedK int) []*sag2Candidate {
	frontier := make([]*sag2Candidate, 0, seedK)
	for _, candidate := range ranking {
		if _, done := expanded[candidate.key]; done {
			continue
		}
		frontier = append(frontier, candidate)
		if len(frontier) == seedK {
			break
		}
	}
	return frontier
}

// sag2DPPOrder is the reference's greedy determinantal diversity reordering:
// sparse TF-IDF vectors are L2-normalised, the kernel multiplies their cosine
// similarity by the two JEV scores, and the greedy maximum-residual selection
// preserves the first passage and the set. Ties and numerically zero gains
// keep the previous order.
func sag2DPPOrder(index *LexicalIndex, candidates []*sag2Candidate) []*sag2Candidate {
	count := len(candidates)
	if count < 2 {
		return candidates
	}
	vectors := make([]map[string]float64, count)
	for position, candidate := range candidates {
		counts := index.BodyCounts(candidate.index)
		vector := make(map[string]float64, len(counts))
		norm := 0.0
		for term, tf := range counts {
			value := (1 + math.Log(float64(tf))) * index.DPPIDF(term)
			vector[term] = value
			norm += value * value
		}
		norm = math.Sqrt(norm)
		if norm > 0 {
			for term := range vector {
				vector[term] /= norm
			}
		}
		vectors[position] = vector
	}
	scores := make([]float64, count)
	for position, candidate := range candidates {
		scores[position] = candidate.jevScore
	}
	kernel := make([][]float64, count)
	for i := 0; i < count; i++ {
		kernel[i] = make([]float64, count)
		for j := 0; j < count; j++ {
			kernel[i][j] = scores[i] * scores[j] * sag2Cosine(vectors[i], vectors[j])
		}
		kernel[i][i] = scores[i] * scores[i]
	}
	residuals := make([]float64, count)
	maxResidual := 0.0
	for position, score := range scores {
		residuals[position] = score * score
		if residuals[position] > maxResidual {
			maxResidual = residuals[position]
		}
	}
	tolerance := float64(count) * (math.Nextafter(maxResidual, math.Inf(1)) - maxResidual)

	coefficients := make([][]float64, count)
	remaining := make([]int, count)
	for position := range remaining {
		remaining[position] = position
	}
	order := make([]int, 0, count)
	for len(remaining) > 0 {
		winner := 0
		for position := 1; position < len(remaining); position++ {
			if residuals[remaining[position]] > residuals[remaining[winner]] {
				winner = position
			}
		}
		winnerPosition := remaining[winner]
		if residuals[winnerPosition] <= tolerance {
			order = append(order, remaining...)
			break
		}
		order = append(order, winnerPosition)
		remaining = append(remaining[:winner], remaining[winner+1:]...)
		denominator := math.Sqrt(residuals[winnerPosition])
		for _, other := range remaining {
			projection := kernel[winnerPosition][other]
			limit := len(coefficients[winnerPosition])
			if len(coefficients[other]) < limit {
				limit = len(coefficients[other])
			}
			for k := 0; k < limit; k++ {
				projection -= coefficients[winnerPosition][k] * coefficients[other][k]
			}
			projection /= denominator
			coefficients[other] = append(coefficients[other], projection)
			residuals[other] = math.Max(0, residuals[other]-projection*projection)
		}
	}
	reordered := make([]*sag2Candidate, 0, count)
	for _, position := range order {
		reordered = append(reordered, candidates[position])
	}
	return reordered
}

// sag2Cosine is the dot product of two L2-normalised sparse vectors.
func sag2Cosine(left, right map[string]float64) float64 {
	if len(left) > len(right) {
		left, right = right, left
	}
	dot := 0.0
	for term, value := range left {
		dot += value * right[term]
	}
	return dot
}

// sag2Failure preserves configured-component errors and wraps everything else
// as an operation error, mirroring the other retrievers.
func sag2Failure(err error) error {
	if err == nil {
		return nil
	}
	var operation *contracts.RagOperationError
	if errors.As(err, &operation) {
		return err
	}
	var apiError *modelapi.ModelAPIError
	if errors.As(err, &apiError) {
		return err
	}
	return contracts.NewRagOperationError(fmt.Sprintf("SAG2 retrieval failed: %v", err))
}
