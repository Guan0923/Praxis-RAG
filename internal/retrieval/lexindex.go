package retrieval

// This file implements the in-memory lexical index that feeds the SAG2
// (embedding-free multi-hop) retriever. It is a Go port of the lexical layer
// of the SAG2 reference implementation (SAG-Benchmark sag2/scripts/sag2.py):
// BM25 over title/body fields, body-level document frequency, exact per-term
// retrieval for expansion, and the passage-to-term-to-passage term weights.
//
// Divergences from the reference implementation, all deliberate:
//   - Tokenization uses internal/sparse.Tokenize (NFKC + casefold, CJK
//     unigrams plus adjacent bigrams, whole Latin tokens) so the lexical
//     statistics stay comparable with the existing bm25 mode. The reference
//     uses Jieba with extra Latin handling, which has no faithful Go port.
//   - The reference scores one combined row length (title+heading+body) with
//     field weights folded into the term frequency. This port applies BM25 per
//     field with per-field average length, and multiplies the saturated term
//     frequency by the field weight: w * tf-saturation * idf.
//   - IDF uses ln(1 + (N-df+0.5)/(df+0.5)) for BM25 as well as for expansion;
//     the reference's FTS5 extension uses ln((N-df+0.5)/(df+0.5)) clamped to
//     1e-6 for BM25. The same (always positive) formula is used everywhere and
//     the 1e-6 clamp is kept as a defensive guard.
//   - Multiword capitalized-name phrases, accent aliases and title-phrase
//     repair are not implemented; every expansion unit is one tokenizer token.
//   - The reference's language-model sentence splitter is replaced by a
//     punctuation/newline split, as agreed for this port.

import (
	"math"
	"regexp"
	"sort"
	"strconv"

	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

const (
	// lexicalK1 and lexicalB are the BM25 parameters of the reference
	// implementation (FTS5 defaults, hard coded in native/sag_rank.c).
	lexicalK1 = 1.2
	lexicalB  = 0.75
	// lexicalTitleWeight and lexicalBodyWeight are the reference field weights
	// (title, heading, body) = (2, 2, 1). Knowledge chunks in this system carry
	// no heading field, so only title and body are scored.
	lexicalTitleWeight = 2.0
	lexicalBodyWeight  = 1.0
	// lexicalMaxQueryTerms mirrors the reference's 80-token OR expression cap.
	lexicalMaxQueryTerms = 80
)

// lexicalSentenceSeparator is the simplified sentence boundary used by the
// expansion-term builder ([.!?。！？\n]+ in the reference).
var lexicalSentenceSeparator = regexp.MustCompile("[.!?。！？\\n]+")

// LexicalDoc is one indexed passage. It is the retrieval-layer projection of a
// stored chunk; the SAG2 retriever turns it back into a contracts.Candidate.
type LexicalDoc struct {
	ChunkID    string
	DocumentID string
	Title      string
	Source     string
	Text       string
	Metadata   map[string]any
}

// LexicalHit is one scored passage returned by a lexical query.
type LexicalHit struct {
	// Index is the position of the passage in the index (the reference's
	// numeric rowid; used as the final tie breaker).
	Index int
	// Key is the stable identity of the passage (ChunkID, or "#<index>" when
	// the corpus left ChunkID empty). It is the key understood by the
	// excluded parameter of the query methods.
	Key        string
	ChunkID    string
	DocumentID string
	Title      string
	Source     string
	Text       string
	Metadata   map[string]any
	Score      float64
	Matched    []string
}

// ExpansionSeed is one expansion source passage with its evidence score.
type ExpansionSeed struct {
	Index int
	Score float64
}

// ExpansionTerm is one scored expansion candidate.
type ExpansionTerm struct {
	Term         string
	Score        float64
	Unweighted   float64
	TF           int
	DF           int
	QueryContext float64
	SeedWeight   float64
	SeedScore    float64
	SeedIndex    int
}

// lexicalPosting is one (passage, term frequency) entry of a field postings
// list. Posting lists are kept sorted by passage index for determinism.
type lexicalPosting struct {
	doc int
	tf  int
}

// LexicalIndex is an immutable in-memory lexical index. It is safe for
// concurrent readers once built; the SAG2 retriever rebuilds it only when the
// corpus revision changes.
type LexicalIndex struct {
	docs  []LexicalDoc
	keys  []string
	index map[string]int

	titleCounts  []map[string]int
	bodyCounts   []map[string]int
	titleLengths []int
	bodyLengths  []int

	titlePostings map[string][]lexicalPosting
	bodyPostings  map[string][]lexicalPosting

	titleAvgDL float64
	bodyAvgDL  float64
}

// NewLexicalIndex builds the index. Passages whose identity key repeats are
// skipped, mirroring the reference's duplicate-text skip at index time.
func NewLexicalIndex(docs []LexicalDoc) *LexicalIndex {
	index := &LexicalIndex{
		titlePostings: make(map[string][]lexicalPosting),
		bodyPostings:  make(map[string][]lexicalPosting),
		index:         make(map[string]int, len(docs)),
	}
	for position := range docs {
		doc := docs[position]
		slot := len(index.docs)
		key := doc.ChunkID
		if key == "" {
			key = "#" + strconv.Itoa(slot)
		}
		if _, duplicate := index.index[key]; duplicate {
			continue
		}
		index.index[key] = slot
		index.docs = append(index.docs, doc)
		index.keys = append(index.keys, key)
		titleCounts := countLexicalTerms(sparse.Tokenize(doc.Title))
		bodyCounts := countLexicalTerms(sparse.Tokenize(doc.Text))
		index.titleCounts = append(index.titleCounts, titleCounts)
		index.bodyCounts = append(index.bodyCounts, bodyCounts)
		index.titleLengths = append(index.titleLengths, totalLexicalCount(titleCounts))
		index.bodyLengths = append(index.bodyLengths, totalLexicalCount(bodyCounts))
		for term, tf := range titleCounts {
			index.titlePostings[term] = append(index.titlePostings[term], lexicalPosting{doc: slot, tf: tf})
		}
		for term, tf := range bodyCounts {
			index.bodyPostings[term] = append(index.bodyPostings[term], lexicalPosting{doc: slot, tf: tf})
		}
	}
	index.sortPostings()
	index.titleAvgDL = averageLength(index.titleLengths)
	index.bodyAvgDL = averageLength(index.bodyLengths)
	return index
}

// countLexicalTerms returns token frequencies plus the number of tokens.
func countLexicalTerms(tokens []string) map[string]int {
	counts := make(map[string]int, len(tokens))
	for _, token := range tokens {
		counts[token]++
	}
	return counts
}

func totalLexicalCount(counts map[string]int) int {
	total := 0
	for _, count := range counts {
		total += count
	}
	return total
}

func averageLength(lengths []int) float64 {
	if len(lengths) == 0 {
		return 0
	}
	total := 0
	for _, length := range lengths {
		total += length
	}
	return float64(total) / float64(len(lengths))
}

// sortPostings keeps posting lists ordered by passage index so iteration and
// tie breaking are deterministic.
func (x *LexicalIndex) sortPostings() {
	for term, postings := range x.titlePostings {
		sort.Slice(postings, func(i, j int) bool { return postings[i].doc < postings[j].doc })
		x.titlePostings[term] = postings
	}
	for term, postings := range x.bodyPostings {
		sort.Slice(postings, func(i, j int) bool { return postings[i].doc < postings[j].doc })
		x.bodyPostings[term] = postings
	}
}

// Len returns the number of indexed passages.
func (x *LexicalIndex) Len() int {
	if x == nil {
		return 0
	}
	return len(x.docs)
}

// N returns the number of indexed passages (the reference's |D|).
func (x *LexicalIndex) N() int { return x.Len() }

// AvgTitleLength returns the mean title token count.
func (x *LexicalIndex) AvgTitleLength() float64 { return x.titleAvgDL }

// AvgBodyLength returns the mean body token count.
func (x *LexicalIndex) AvgBodyLength() float64 { return x.bodyAvgDL }

// Doc returns the passage stored at an index position.
func (x *LexicalIndex) Doc(position int) (LexicalDoc, bool) {
	if x == nil || position < 0 || position >= len(x.docs) {
		return LexicalDoc{}, false
	}
	return x.docs[position], true
}

// Key returns the identity key of a passage.
func (x *LexicalIndex) Key(position int) string {
	if x == nil || position < 0 || position >= len(x.keys) {
		return ""
	}
	return x.keys[position]
}

// BodyCounts returns the body token frequencies of one passage. The map is
// owned by the index and must not be mutated by callers.
func (x *LexicalIndex) BodyCounts(position int) map[string]int {
	if x == nil || position < 0 || position >= len(x.bodyCounts) {
		return nil
	}
	return x.bodyCounts[position]
}

// DF returns the body-level document frequency of a term: the number of
// indexed passages whose body contains it (the reference's df(t)).
func (x *LexicalIndex) DF(term string) int {
	if x == nil {
		return 0
	}
	return len(x.bodyPostings[term])
}

// idf is the reference's rarity weight I(t) = ln(1 + (N-df+0.5)/(df+0.5)).
// The 1e-6 floor is the reference's defensive clamp; this form is always
// positive but the guard keeps the contract explicit.
func (x *LexicalIndex) idf(term string) float64 {
	if x == nil || len(x.docs) == 0 {
		return 0
	}
	value := math.Log(1 + (float64(len(x.docs))-float64(x.DF(term))+0.5)/(float64(x.DF(term))+0.5))
	if value <= 0 {
		return 1e-6
	}
	return value
}

// DPPIDF returns the diversity-ordering inverse document frequency
// ln((1+N)/(1+df))+1 used by the greedy determinantal reordering.
func (x *LexicalIndex) DPPIDF(term string) float64 {
	if x == nil || len(x.docs) == 0 {
		return 1
	}
	return math.Log(float64(1+len(x.docs))/float64(1+x.DF(term))) + 1
}

// RejectedKeys returns the identity keys of every passage that does not match
// the equality filters (document_id / source / title). The SAG2 retriever
// feeds this set into the query methods so filtered passages never consume the
// candidate budget.
func (x *LexicalIndex) RejectedKeys(filters map[string]any) map[string]struct{} {
	if x == nil || len(filters) == 0 {
		return nil
	}
	rejected := make(map[string]struct{})
	for position, doc := range x.docs {
		if lexicalDocMatchesFilters(doc, filters) {
			continue
		}
		rejected[x.keys[position]] = struct{}{}
	}
	return rejected
}

// lexicalDocMatchesFilters applies the closed filter vocabulary with the same
// equality semantics as the mock retriever (type mismatches do not match). A
// []string document_id condition (the server-side visibility allow-list)
// accepts any listed document; the impossible marker value matches nothing.
func lexicalDocMatchesFilters(doc LexicalDoc, filters map[string]any) bool {
	if len(filters) == 0 {
		return true
	}
	values := map[string]any{
		"document_id": doc.DocumentID,
		"source":      doc.Source,
		"title":       doc.Title,
	}
	for key, want := range filters {
		if list, isList := want.([]string); isList {
			if key != "document_id" {
				return false
			}
			matched := false
			for _, allowed := range list {
				if allowed == doc.DocumentID {
					matched = true
					break
				}
			}
			if !matched {
				return false
			}
			continue
		}
		if !pythonEqual(values[key], want) {
			return false
		}
	}
	return true
}

// Search runs the OR expression of terms and returns passages that match at
// least one term, scored by the sum of their per-field BM25 contributions.
// Results are ordered by descending score then ascending index.
func (x *LexicalIndex) Search(terms []string, excluded map[string]struct{}, limit int) []LexicalHit {
	if x == nil || len(x.docs) == 0 || limit <= 0 || len(terms) == 0 {
		return nil
	}
	scores := make(map[int]float64, 16)
	matched := make(map[int]map[string]struct{}, 16)
	for _, term := range terms {
		idf := x.idf(term)
		if idf == 0 {
			continue
		}
		touched := make(map[int]struct{})
		for _, posting := range x.titlePostings[term] {
			if x.isExcluded(posting.doc, excluded) {
				continue
			}
			scores[posting.doc] += lexicalTitleWeight * x.saturate(posting.tf, x.titleLengths[posting.doc], x.titleAvgDL) * idf
			touched[posting.doc] = struct{}{}
		}
		for _, posting := range x.bodyPostings[term] {
			if x.isExcluded(posting.doc, excluded) {
				continue
			}
			scores[posting.doc] += lexicalBodyWeight * x.saturate(posting.tf, x.bodyLengths[posting.doc], x.bodyAvgDL) * idf
			touched[posting.doc] = struct{}{}
		}
		for doc := range touched {
			if matched[doc] == nil {
				matched[doc] = make(map[string]struct{}, 2)
			}
			matched[doc][term] = struct{}{}
		}
	}
	return x.collect(scores, matched, limit)
}

// SearchTerm retrieves passages that match term (title or body) and scores
// each of them with the full ranking expression, mirroring the reference's
// per-term query "(ranking expression) AND term". Matching the expansion term
// is required; matching a ranking term is not.
func (x *LexicalIndex) SearchTerm(term string, rankingTerms []string, excluded map[string]struct{}, limit int) []LexicalHit {
	if x == nil || len(x.docs) == 0 || limit <= 0 || term == "" {
		return nil
	}
	containing := make(map[int]struct{}, 8)
	for _, posting := range x.titlePostings[term] {
		if !x.isExcluded(posting.doc, excluded) {
			containing[posting.doc] = struct{}{}
		}
	}
	for _, posting := range x.bodyPostings[term] {
		if !x.isExcluded(posting.doc, excluded) {
			containing[posting.doc] = struct{}{}
		}
	}
	if len(containing) == 0 {
		return nil
	}
	scores := make(map[int]float64, len(containing))
	matched := make(map[int]map[string]struct{}, len(containing))
	for _, ranking := range rankingTerms {
		idf := x.idf(ranking)
		if idf == 0 {
			continue
		}
		touched := make(map[int]struct{})
		for _, posting := range x.titlePostings[ranking] {
			if _, ok := containing[posting.doc]; !ok {
				continue
			}
			scores[posting.doc] += lexicalTitleWeight * x.saturate(posting.tf, x.titleLengths[posting.doc], x.titleAvgDL) * idf
			touched[posting.doc] = struct{}{}
		}
		for _, posting := range x.bodyPostings[ranking] {
			if _, ok := containing[posting.doc]; !ok {
				continue
			}
			scores[posting.doc] += lexicalBodyWeight * x.saturate(posting.tf, x.bodyLengths[posting.doc], x.bodyAvgDL) * idf
			touched[posting.doc] = struct{}{}
		}
		for doc := range touched {
			if matched[doc] == nil {
				matched[doc] = make(map[string]struct{}, 2)
			}
			matched[doc][ranking] = struct{}{}
		}
	}
	// A passage that contains only the required term still enters the list; it
	// simply has the score of that term's contribution.
	for doc := range containing {
		if _, ok := scores[doc]; !ok {
			scores[doc] = 0
		}
	}
	return x.collect(scores, matched, limit)
}

// saturate is the BM25 term-frequency saturation with length normalization.
func (x *LexicalIndex) saturate(tf, length int, avgdl float64) float64 {
	if tf <= 0 {
		return 0
	}
	normalization := 1.0
	if avgdl > 0 {
		normalization = 1 - lexicalB + lexicalB*float64(length)/avgdl
	}
	return float64(tf) * (lexicalK1 + 1) / (float64(tf) + lexicalK1*normalization)
}

func (x *LexicalIndex) isExcluded(position int, excluded map[string]struct{}) bool {
	if len(excluded) == 0 {
		return false
	}
	_, skip := excluded[x.keys[position]]
	return skip
}

func (x *LexicalIndex) collect(scores map[int]float64, matched map[int]map[string]struct{}, limit int) []LexicalHit {
	hits := make([]LexicalHit, 0, len(scores))
	for position, score := range scores {
		doc := x.docs[position]
		hit := LexicalHit{
			Index:      position,
			Key:        x.keys[position],
			ChunkID:    doc.ChunkID,
			DocumentID: doc.DocumentID,
			Title:      doc.Title,
			Source:     doc.Source,
			Text:       doc.Text,
			Metadata:   doc.Metadata,
			Score:      score,
		}
		if terms, ok := matched[position]; ok {
			hit.Matched = sortedTerms(terms)
		}
		hits = append(hits, hit)
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Score != hits[j].Score {
			return hits[i].Score > hits[j].Score
		}
		return hits[i].Index < hits[j].Index
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits
}

// ExpansionTerms scores passage terms for the next retrieval round. It follows
// the reference formula
//
//	a(t) = max over seeds [ (1+ln tf(t,d)) * I(t) * (1 + g_q(t,d)) * w(d) ]
//
// with w(d) = 0.5 + 0.5*s(d)/max(s), g_q the best sentence-level query
// connection, and I(t) the rarity weight. Terms that are themselves query
// terms and terms with zero body document frequency are excluded; ties break
// on the term string.
func (x *LexicalIndex) ExpansionTerms(seeds []ExpansionSeed, queryTerms map[string]struct{}, limit int) []ExpansionTerm {
	if x == nil || len(x.docs) == 0 || limit <= 0 || len(seeds) == 0 {
		return nil
	}
	maxScore := 0.0
	for _, seed := range seeds {
		if seed.Score > maxScore {
			maxScore = seed.Score
		}
	}
	ranked := make(map[string]ExpansionTerm, 32)
	for _, seed := range seeds {
		if seed.Index < 0 || seed.Index >= len(x.docs) {
			continue
		}
		counts := make(map[string]int, 32)
		contextScores := make(map[string]float64, 32)
		for _, sentence := range lexicalSentenceSeparator.Split(x.docs[seed.Index].Text, -1) {
			tokens := sparse.Tokenize(sentence)
			if len(tokens) == 0 {
				continue
			}
			distinct := make(map[string]struct{}, len(tokens))
			for _, token := range tokens {
				distinct[token] = struct{}{}
			}
			context := 0.0
			for token := range distinct {
				if _, ok := queryTerms[token]; ok {
					context += x.idf(token)
				}
			}
			for token := range distinct {
				counts[token]++
				if context > contextScores[token] {
					contextScores[token] = context
				}
			}
		}
		weight := 0.5
		if maxScore > 0 {
			weight = 0.5 + 0.5*(seed.Score/maxScore)
		}
		for term, tf := range counts {
			if _, isQueryTerm := queryTerms[term]; isQueryTerm {
				continue
			}
			df := x.DF(term)
			if df == 0 {
				continue
			}
			idf := x.idf(term)
			unweighted := (1 + math.Log(float64(tf))) * idf * (1 + contextScores[term])
			score := unweighted * weight
			current, ok := ranked[term]
			if ok && score <= current.Score {
				continue
			}
			ranked[term] = ExpansionTerm{
				Term:         term,
				Score:        score,
				Unweighted:   unweighted,
				TF:           tf,
				DF:           df,
				QueryContext: contextScores[term],
				SeedWeight:   weight,
				SeedScore:    seed.Score,
				SeedIndex:    seed.Index,
			}
		}
	}
	terms := make([]ExpansionTerm, 0, len(ranked))
	for _, term := range ranked {
		terms = append(terms, term)
	}
	sort.Slice(terms, func(i, j int) bool {
		if terms[i].Score != terms[j].Score {
			return terms[i].Score > terms[j].Score
		}
		return terms[i].Term < terms[j].Term
	})
	if len(terms) > limit {
		terms = terms[:limit]
	}
	return terms
}
