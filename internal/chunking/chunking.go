// Package chunking holds the chunking configuration contract and strategy
// catalog, and ports the text splitter from rag_mcp.chunk_strategies plus the
// document chunker from rag_mcp.chunking.
//
// Token parity gap: Python counted tokens with a transformers AutoTokenizer
// (BAAI/bge-m3, SentencePiece). The Go port replaces it with an injectable
// TokenCounter; DefaultCounter is a deterministic approximation, so token
// counts and token-budget chunk boundaries differ from Python even though
// sentence/paragraph/structure boundaries and the overlap rules are ports of
// the original algorithms. To reach exact parity a BGE-M3 tokenizer must be
// ported to Go (or run as an external process) and injected through
// ChunkOptions.Counter / NewTokenBudget. This is tracked in
// docs/GO_MIGRATION.md ("Tokenizer 依赖").
package chunking

import "fmt"

// Strategy describes one user-selectable splitting strategy.
type Strategy struct {
	ID           string `json:"id"`
	Label        string `json:"label"`
	Unit         string `json:"unit"`
	ChunkSize    int    `json:"chunk_size"`
	ChunkOverlap int    `json:"chunk_overlap"`
}

// Strategies mirrors chunk_strategies.STRATEGIES in declaration order.
var Strategies = []Strategy{
	{ID: "character", Label: "按字符", Unit: "characters", ChunkSize: 700, ChunkOverlap: 100},
	{ID: "token", Label: "按 token", Unit: "tokens", ChunkSize: 512, ChunkOverlap: 64},
	{ID: "sentence", Label: "按句子", Unit: "tokens", ChunkSize: 512, ChunkOverlap: 64},
	{ID: "recursive", Label: "递归切分", Unit: "tokens", ChunkSize: 512, ChunkOverlap: 64},
	{ID: "structure", Label: "按文档结构", Unit: "tokens", ChunkSize: 512, ChunkOverlap: 64},
}

// DefaultStrategy is used when a knowledge base does not set one.
const DefaultStrategy = "recursive"

// Catalog returns the public strategy catalog.
type Catalog struct {
	Default    string     `json:"default"`
	Strategies []Strategy `json:"strategies"`
}

// StrategyCatalog mirrors chunk_strategies.strategy_catalog.
func StrategyCatalog() Catalog {
	items := make([]Strategy, len(Strategies))
	copy(items, Strategies)
	return Catalog{Default: DefaultStrategy, Strategies: items}
}

// Config is the validated knowledge-base chunking configuration.
type Config struct {
	Strategy     string
	ChunkSize    int
	ChunkOverlap int
}

// NewConfig validates an explicit chunking configuration. Error messages match
// rag_mcp.chunk_strategies.ChunkingConfig.
func NewConfig(strategy string, chunkSize, chunkOverlap int) (Config, error) {
	if _, ok := strategyByID(strategy); !ok {
		return Config{}, fmt.Errorf("未知切分策略")
	}
	if chunkSize < 1 || chunkSize > 100_000 {
		return Config{}, fmt.Errorf("块长度必须是 1 到 100000 的整数")
	}
	if chunkOverlap < 0 || chunkOverlap >= chunkSize {
		return Config{}, fmt.Errorf("重叠长度必须是非负整数且小于块长度")
	}
	if strategy == "character" && chunkSize < 100 {
		return Config{}, fmt.Errorf("字符切分的块长度至少为 100")
	}
	return Config{Strategy: strategy, ChunkSize: chunkSize, ChunkOverlap: chunkOverlap}, nil
}

// ConfigFromRequest validates strategy/size/overlap input the way
// http_service._chunking_config does, applying per-strategy defaults.
func ConfigFromRequest(strategy string, chunkSize, chunkOverlap *int) (Config, error) {
	defaultSize := 512
	defaultOverlap := 64
	defaultUnit := "tokens"
	if strategy == "character" {
		defaultSize = 700
		defaultOverlap = 100
		defaultUnit = "characters"
	}
	size := defaultSize
	if chunkSize != nil {
		size = *chunkSize
	}
	overlap := defaultOverlap
	if chunkOverlap != nil {
		overlap = *chunkOverlap
	}
	_ = defaultUnit
	return NewConfig(strategy, size, overlap)
}

// Metadata renders the chunking metadata stored with each chunk.
func (c Config) Metadata(tokenizerModel string) map[string]any {
	return map[string]any{
		"strategy":      c.Strategy,
		"chunk_size":    c.ChunkSize,
		"chunk_overlap": c.ChunkOverlap,
		"tokenizer":     tokenizerModel,
	}
}

// DefaultsFor returns the default size/overlap for a strategy, matching
// strategy_catalog's rows.
func DefaultsFor(strategy string) (size, overlap int, ok bool) {
	item, ok := strategyByID(strategy)
	if !ok {
		return 0, 0, false
	}
	return item.ChunkSize, item.ChunkOverlap, true
}

func strategyByID(id string) (Strategy, bool) {
	for _, item := range Strategies {
		if item.ID == id {
			return item, true
		}
	}
	return Strategy{}, false
}

// Title ports rag_mcp.chunking._title: the first non-empty line with leading
// "#" markers removed, truncated to 200 runes, falling back to the supplied
// value. It was previously pinned inside internal/document/pdfcontent; external
// pipelines (Word chunking) use it to keep titles identical across formats.
func Title(text, fallback string) string {
	return titleOf(text, fallback)
}

// CharacterRanges ports rag_mcp.chunking._split_ranges: the legacy character
// splitter (break on newline/CJK punctuation near the target end, trim
// whitespace, overlap from the raw end). It replaces the pinned copy that lived
// in internal/document/pdfcontent.
func CharacterRanges(text string, chunkSize, overlap int) []Range {
	return splitCharacterRanges([]rune(text), chunkSize, overlap)
}

// DocumentID ports `_stable_id("doc", source.casefold())`, the stable
// document-scoped id shared by chunk_document and the Word/PDF asset pipelines.
func DocumentID(source string) string {
	return StableID("doc", pythonCaseFold(source))
}
