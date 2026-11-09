// Package sparse implements the deterministic BM25-style sparse encoder used
// by the Qdrant hybrid index. It is a direct port of rag_mcp/sparse.py:
// Latin/CJK tokenization, a personalized BLAKE2s hash vocabulary, and the
// document-side BM25 term-frequency component. Qdrant's IDF modifier supplies
// collection-level inverse document frequency at query time, so the encoder
// keeps no corpus state.
package sparse

import (
	"encoding/binary"
	"fmt"
	"math"
	"sort"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

// hashPerson is the BLAKE2s personalization string used by rag_mcp.sparse.
var hashPerson = [8]byte{'r', 'a', 'g', '-', 'b', 'm', '2', '5'}

// dotlessI is matched by Python's re.IGNORECASE for the [a-z] class.
const dotlessI = '\u0131'

var (
	sparseNormalizer = norm.NFKC
	sparseFolder     = cases.Fold()
)

// SparseEmbedding is a sorted sparse vector independent of any database SDK.
type SparseEmbedding struct {
	Indices []uint32
	Values  []float64
}

// NewSparseEmbedding validates the invariants enforced by the Python
// SparseEmbedding dataclass.
func NewSparseEmbedding(indices []uint32, values []float64) (SparseEmbedding, error) {
	if len(indices) != len(values) {
		return SparseEmbedding{}, fmt.Errorf("Sparse indices and values must have equal lengths")
	}
	for index := 1; index < len(indices); index++ {
		if indices[index] <= indices[index-1] {
			return SparseEmbedding{}, fmt.Errorf("Sparse indices must be unique and sorted")
		}
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 {
			return SparseEmbedding{}, fmt.Errorf("Sparse values must be finite positive numbers")
		}
	}
	return SparseEmbedding{Indices: indices, Values: values}, nil
}

// Empty reports whether the embedding carries no terms.
func (e SparseEmbedding) Empty() bool { return len(e.Indices) == 0 }

// EncoderOptions mirrors Bm25SparseEncoder(k1=1.2, b=0.75, length_pivot=100).
type EncoderOptions struct {
	K1          float64
	B           float64
	LengthPivot float64
}

// DefaultEncoderOptions returns the production BM25 parameters.
func DefaultEncoderOptions() EncoderOptions {
	return EncoderOptions{K1: 1.2, B: 0.75, LengthPivot: 100.0}
}

// Encoder is a deterministic BM25 sparse encoder.
type Encoder struct {
	k1          float64
	b           float64
	lengthPivot float64
}

// NewEncoder validates the BM25 parameters exactly like the Python
// constructor does.
func NewEncoder(options EncoderOptions) (*Encoder, error) {
	if options.K1 <= 0 || options.B < 0 || options.B > 1 || options.LengthPivot <= 0 {
		return nil, fmt.Errorf("BM25 k1, b, and length_pivot are out of range")
	}
	return &Encoder{k1: options.K1, b: options.B, lengthPivot: options.LengthPivot}, nil
}

// NewDefaultEncoder builds the production encoder; it cannot fail because the
// defaults are valid.
func NewDefaultEncoder() *Encoder {
	encoder, err := NewEncoder(DefaultEncoderOptions())
	if err != nil {
		panic(err)
	}
	return encoder
}

// EncodeDocuments encodes every text without sharing mutable corpus state.
func (e *Encoder) EncodeDocuments(texts []string) ([]SparseEmbedding, error) {
	embeddings := make([]SparseEmbedding, 0, len(texts))
	for _, text := range texts {
		embedding, err := e.EncodeDocument(text)
		if err != nil {
			return nil, err
		}
		embeddings = append(embeddings, embedding)
	}
	return embeddings, nil
}

// EncodeDocument returns the BM25-saturated lexical weights for one chunk.
func (e *Encoder) EncodeDocument(text string) (SparseEmbedding, error) {
	tokens := Tokenize(text)
	if len(tokens) == 0 {
		return SparseEmbedding{}, nil
	}
	counts, order := countTokens(tokens)
	lengthNormalization := 1.0 - e.b + e.b*float64(len(tokens))/e.lengthPivot
	weighted := make([]weightedToken, 0, len(counts))
	for _, token := range order {
		count := counts[token]
		weighted = append(weighted, weightedToken{
			index: tokenIndex(token),
			value: float64(count) * (e.k1 + 1.0) / (float64(count) + e.k1*lengthNormalization),
		})
	}
	return buildEmbedding(weighted)
}

// EncodeQuery uses bounded query term frequency; Qdrant adds collection IDF.
func (e *Encoder) EncodeQuery(text string) (SparseEmbedding, error) {
	counts, order := countTokens(Tokenize(text))
	if len(counts) == 0 {
		return SparseEmbedding{}, nil
	}
	weighted := make([]weightedToken, 0, len(counts))
	for _, token := range order {
		weighted = append(weighted, weightedToken{
			index: tokenIndex(token),
			value: 1.0 + math.Log(float64(counts[token])),
		})
	}
	return buildEmbedding(weighted)
}

type weightedToken struct {
	index uint32
	value float64
}

// countTokens returns term frequencies plus first-occurrence order, mirroring
// the insertion order of collections.Counter in the Python encoder.
func countTokens(tokens []string) (map[string]int, []string) {
	counts := make(map[string]int, len(tokens))
	order := make([]string, 0, len(tokens))
	for _, token := range tokens {
		if _, seen := counts[token]; !seen {
			order = append(order, token)
		}
		counts[token]++
	}
	return counts, order
}

// buildEmbedding merges hash collisions before sorting, exactly like
// _embedding in the Python module.
func buildEmbedding(weighted []weightedToken) (SparseEmbedding, error) {
	merged := make(map[uint32]float64, len(weighted))
	order := make([]uint32, 0, len(weighted))
	for _, item := range weighted {
		if _, seen := merged[item.index]; !seen {
			order = append(order, item.index)
		}
		merged[item.index] += item.value
	}
	sort.Slice(order, func(i, j int) bool { return order[i] < order[j] })
	indices := order
	values := make([]float64, len(indices))
	for i, index := range indices {
		values[i] = merged[index]
	}
	return NewSparseEmbedding(indices, values)
}

// Tokenize normalizes Latin tokens and emits CJK unigrams plus adjacent
// bigrams, matching rag_mcp.sparse.tokenize.
func Tokenize(text string) []string {
	normalized := sparseFolder.String(sparseNormalizer.String(text))
	runes := []rune(normalized)
	tokens := make([]string, 0, len(runes))
	for index := 0; index < len(runes); {
		current := runes[index]
		if isLatinTokenRune(current) {
			end := index + 1
			for end < len(runes) && isLatinTokenRune(runes[end]) {
				end++
			}
			tokens = append(tokens, string(runes[index:end]))
			index = end
			continue
		}
		if isCJKRune(current) {
			end := index + 1
			for end < len(runes) && isCJKRune(runes[end]) {
				end++
			}
			segment := runes[index:end]
			for _, char := range segment {
				tokens = append(tokens, string(char))
			}
			for position := 0; position+1 < len(segment); position++ {
				tokens = append(tokens, string(segment[position:position+2]))
			}
			index = end
			continue
		}
		index++
	}
	return tokens
}

func isLatinTokenRune(char rune) bool {
	return (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') || char == dotlessI
}

// isCJKRune covers the two explicit ranges in the Python segment pattern:
// U+3400-U+4DBF and U+4E00-U+9FFF.
func isCJKRune(char rune) bool {
	return (char >= 0x3400 && char <= 0x4DBF) || (char >= 0x4E00 && char <= 0x9FFF)
}

// tokenIndex hashes one token to an unsigned 32-bit vocabulary index using
// BLAKE2s(digest_size=4, person=b"rag-bm25") interpreted big-endian.
func tokenIndex(token string) uint32 {
	digest := blake2sDigest([]byte(token), 4, hashPerson)
	return binary.BigEndian.Uint32(digest)
}

// Fold applies the Unicode NFKC + casefold normalization used by the Python
// tokenizer. It is exported for the deterministic mock retriever, which folds
// terms with str.casefold().
func Fold(text string) string {
	return sparseFolder.String(sparseNormalizer.String(text))
}

// CaseFold applies Unicode case folding without NFKC normalization, matching
// Python str.casefold(). The mock retriever uses this form.
func CaseFold(text string) string {
	return sparseFolder.String(text)
}
