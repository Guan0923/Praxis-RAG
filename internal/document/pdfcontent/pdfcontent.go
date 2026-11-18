// Package pdfcontent ports rag_mcp.pdf_content: it turns one PDF manifest
// into index chunks with formula/table boundaries preserved, and
// provides the FORMULA_BLOCK-aware content pagination used by document
// resources.
//
// It lives in a subpackage because internal/chunking imports internal/document
// (for Content/Block/Span); placing this port in package document would create
// an import cycle. The shared _title/_split_ranges equivalents are exported by
// internal/chunking (chunking.Title / chunking.CharacterRanges).
package pdfcontent

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/document"
)

// PageCharacters mirrors resources.PAGE_CHARACTERS.
const PageCharacters = 8000

// FormulaBlock mirrors pdf_content.FORMULA_BLOCK: display math blocks
// (`$$ ... $$` on their own lines) must never be split by pagination.
var FormulaBlock = regexp.MustCompile(`(?m)^\$\$[ \t]*\n[\s\S]*?^\$\$[ \t]*(?:\n|$)`)

const defaultTokenizerModel = "BAAI/bge-m3"

// Options configures ContentChunks.
type Options struct {
	// DocumentID is the document-scoped id (also used for chunk ids).
	DocumentID string
	// Source is the public source name stored in chunk metadata.
	Source string
	// FallbackTitle is used when the manifest has no text line to title from.
	FallbackTitle string
	// Config is the validated chunking configuration.
	Config chunking.Config
	// TokenizerModel is recorded in the chunking metadata (default BGE-M3).
	TokenizerModel string
	// Counter overrides the token counter (default chunking.DefaultCounter).
	Counter chunking.TokenCounter
}

// ContentChunks ports pdf_content.content_chunks: regular text blocks are
// buffered and split with the configured strategy, while formula blocks and
// picture/table assets stay atomic and keep their manifest metadata.
func ContentChunks(manifest document.PDFManifest, options Options) ([]chunking.Chunk, error) {
	if options.Counter == nil {
		options.Counter = chunking.DefaultCounter
	}
	if options.TokenizerModel == "" {
		options.TokenizerModel = defaultTokenizerModel
	}
	config, err := chunking.NewConfig(options.Config.Strategy, options.Config.ChunkSize, options.Config.ChunkOverlap)
	if err != nil {
		return nil, err
	}
	probe, err := chunking.NewTokenBudget(options.Counter, chunking.Config{Strategy: "token", ChunkSize: 1})
	if err != nil {
		return nil, err
	}
	maximum := probe.Maximum()
	budgetConfig := config
	if config.Strategy == "character" {
		// The character strategy only counts for the per-item model limit.
		budgetConfig = chunking.Config{Strategy: "token", ChunkSize: maximum, ChunkOverlap: 0}
	}
	budget, err := chunking.NewTokenBudget(options.Counter, budgetConfig)
	if err != nil {
		return nil, err
	}
	builder := &chunkBuilder{
		manifest:   manifest,
		documentID: options.DocumentID,
		source:     options.Source,
		config:     config,
		budget:     budget,
		maximum:    maximum,
		title:      chunking.Title(manifestTitleText(manifest), options.FallbackTitle),
		base: map[string]any{
			"document_suffix": document.SuffixPDF,
			"chunking":        config.Metadata(options.TokenizerModel),
			// The version comes from the parsed manifest so chunks record the
			// engine that produced the content (Go native or remote vision service).
			"pdf_parser_version": manifest.Version,
		},
		result: make([]chunking.Chunk, 0, 16),
	}
	if err := builder.run(); err != nil {
		return nil, err
	}
	return builder.result, nil
}

func manifestTitleText(manifest document.PDFManifest) string {
	parts := make([]string, len(manifest.Blocks))
	for index, block := range manifest.Blocks {
		parts[index] = block.Text
	}
	return strings.Join(parts, "\n")
}

type chunkBuilder struct {
	manifest   document.PDFManifest
	documentID string
	source     string
	config     chunking.Config
	budget     *chunking.TokenBudget
	maximum    int
	title      string
	base       map[string]any
	pending    []document.PDFBlock
	result     []chunking.Chunk
}

func (b *chunkBuilder) run() error {
	assets := make(map[string]document.PDFAsset, len(b.manifest.Assets))
	for _, asset := range b.manifest.Assets {
		assets[asset.AssetID] = asset
	}
	for _, block := range b.manifest.Blocks {
		if block.AssetID == "" && block.Kind != "formula" {
			if block.Text != "" {
				b.pending = append(b.pending, block)
			}
			continue
		}
		if err := b.flush(); err != nil {
			return err
		}
		metadata := map[string]any{
			"kind":       block.Kind,
			"page":       block.Page,
			"order":      block.Order,
			"pages":      blockPages(block),
			"refs":       []string{block.Ref},
			"provenance": []map[string]any{provenanceOf(block)},
		}
		if block.Kind == "formula" {
			metadata["latex"] = block.LaTeX
			metadata["original_text"] = block.OriginalText
			metadata["equation_number"] = block.EquationNumber
			if err := b.appendChunk(block.Text, metadata); err != nil {
				return err
			}
			continue
		}
		asset, ok := assets[block.AssetID]
		if !ok {
			return fmt.Errorf("PDF manifest references an unknown asset: %s", block.AssetID)
		}
		metadata["asset_id"] = asset.AssetID
		metadata["asset_kind"] = asset.Kind
		caption := asset.Caption
		if caption == "" {
			caption = asset.GeneratedDescription
		}
		if asset.Kind == "picture" {
			if err := b.appendChunk(caption, metadata); err != nil {
				return err
			}
			continue
		}
		if asset.Table == nil {
			return fmt.Errorf("PDF manifest table asset has no table data: %s", asset.AssetID)
		}
		headerRows := asset.Table.HeaderRows
		if len(headerRows) == 0 {
			headerRows = [][]string{asset.Table.Headers}
		}
		headers := make([]string, len(headerRows))
		for index, row := range headerRows {
			headers[index] = document.TableRowText(row)
		}
		headerText := strings.Join(headers, "\n")
		for rowNumber, row := range asset.Table.Rows {
			rowText := document.TableRowText(row)
			parts := make([]string, 0, 3)
			for _, part := range []string{caption, headerText, rowText} {
				if part != "" {
					parts = append(parts, part)
				}
			}
			rowMetadata := cloneMetadata(metadata)
			rowMetadata["table_row"] = rowNumber
			if err := b.appendChunk(strings.Join(parts, "\n"), rowMetadata); err != nil {
				return err
			}
		}
	}
	return b.flush()
}

// flush ports the inner flush closure of pdf_content.content_chunks.
func (b *chunkBuilder) flush() error {
	if len(b.pending) == 0 {
		return nil
	}
	pieces := make([]string, 0, len(b.pending))
	spans := make([]document.Span, 0, len(b.pending))
	offset := 0
	for _, block := range b.pending {
		if len(pieces) > 0 {
			offset += 2
		}
		kind := block.Kind
		if kind == "title" || kind == "section_header" {
			kind = document.KindHeading
		}
		length := utf8.RuneCountInString(block.Text)
		spans = append(spans, document.Span{
			Start:     offset,
			End:       offset + length,
			Block:     document.Block{Text: block.Text, Kind: kind, Page: block.Page},
			Paragraph: len(spans),
		})
		pieces = append(pieces, block.Text)
		offset += length
	}
	text := strings.Join(pieces, "\n\n")
	content := document.Content{Text: text, Spans: spans}
	runes := []rune(text)
	var ranges []chunking.Range
	if b.config.Strategy == "character" {
		ranges = chunking.CharacterRanges(text, b.config.ChunkSize, b.config.ChunkOverlap)
	} else {
		var err error
		ranges, err = chunking.SplitContent(content, document.SuffixPDF, b.config, b.budget)
		if err != nil {
			return err
		}
	}
	for _, span := range ranges {
		covered := make([]document.PDFBlock, 0, len(b.pending))
		for index, block := range b.pending {
			if spans[index].Start < span.End && spans[index].End > span.Start {
				covered = append(covered, block)
			}
		}
		if len(covered) == 0 {
			return fmt.Errorf("PDF chunk range has no source blocks: [%d,%d)", span.Start, span.End)
		}
		metadata := chunking.SourceMetadata(content, span.Start, span.End)
		refs := make([]string, len(covered))
		for index, block := range covered {
			refs[index] = block.Ref
		}
		metadata["refs"] = refs
		metadata["page"] = covered[0].Page
		metadata["order"] = covered[0].Order
		pageSet := make(map[int]struct{}, 4)
		for _, block := range covered {
			for _, page := range blockPages(block) {
				pageSet[page] = struct{}{}
			}
		}
		pages := make([]int, 0, len(pageSet))
		for page := range pageSet {
			pages = append(pages, page)
		}
		sort.Ints(pages)
		metadata["pages"] = pages
		provenance := make([]map[string]any, len(covered))
		for index, block := range covered {
			provenance[index] = provenanceOf(block)
		}
		metadata["provenance"] = provenance
		if err := b.appendChunk(string(runes[span.Start:span.End]), metadata); err != nil {
			return err
		}
	}
	b.pending = b.pending[:0]
	return nil
}

// appendChunk ports the append closure of pdf_content.content_chunks.
func (b *chunkBuilder) appendChunk(text string, metadata map[string]any) error {
	if strings.TrimSpace(text) == "" {
		return nil
	}
	count := b.budget.Count(text)
	if count > b.maximum {
		return fmt.Errorf("PDF item exceeds model input limit (%d tokens): %s", b.maximum, pythonRepr(metadata["refs"]))
	}
	pages, ok := intSlice(metadata["pages"])
	if !ok {
		page, _ := metadata["page"].(int)
		pages = []int{page}
	}
	warnings := make([]document.PDFWarning, 0, len(b.manifest.Warnings))
	for _, warning := range b.manifest.Warnings {
		if containsInt(pages, warning.Page) {
			warnings = append(warnings, warning)
		}
	}
	if len(warnings) > 0 {
		metadata = cloneMetadata(metadata)
		metadata["source_warnings"] = warnings
	}
	index := len(b.result)
	chunkMetadata := map[string]any{"chunk_index": index}
	for key, value := range b.base {
		chunkMetadata[key] = value
	}
	for key, value := range metadata {
		chunkMetadata[key] = value
	}
	chunkMetadata["token_count"] = count
	chunkID := chunking.StableID("chunk", fmt.Sprintf("%s\x00%d\x00%s", b.documentID, index, text))
	b.result = append(b.result, chunking.Chunk{
		DocumentID: b.documentID,
		ChunkID:    chunkID,
		Text:       text,
		Title:      b.title,
		Source:     b.source,
		Metadata:   chunkMetadata,
	})
	return nil
}

// blockPages ports pdf_content._pages: sorted location pages, falling back to
// the block page only when the manifest has no locations key at all (Python's
// block.get("locations", [{"page": block["page"]}]) behaviour).
func blockPages(block document.PDFBlock) []int {
	set := make(map[int]struct{}, 2)
	if block.Locations != nil {
		for _, location := range block.Locations {
			if location.Page != 0 {
				set[location.Page] = struct{}{}
			}
		}
	} else if block.Page != 0 {
		set[block.Page] = struct{}{}
	}
	pages := make([]int, 0, len(set))
	for page := range set {
		pages = append(pages, page)
	}
	sort.Ints(pages)
	return pages
}

// provenanceOf ports pdf_content._provenance.
func provenanceOf(block document.PDFBlock) map[string]any {
	provenance := map[string]any{
		"ref":   block.Ref,
		"order": block.Order,
		"kind":  block.Kind,
	}
	if block.Page != 0 {
		provenance["page"] = block.Page
	}
	if block.BBox != nil {
		provenance["bbox"] = block.BBox
	}
	if block.Locations != nil {
		provenance["locations"] = block.Locations
	}
	if block.Association != "" {
		provenance["association"] = block.Association
	}
	if block.AssociatedAssets != nil {
		provenance["associated_assets"] = block.AssociatedAssets
	}
	return provenance
}

func intSlice(value any) ([]int, bool) {
	pages, ok := value.([]int)
	return pages, ok
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneMetadata(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// pythonRepr formats values the way Python's f-string renders them in the model
// limit error message (list of refs or None).
func pythonRepr(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case []string:
		quoted := make([]string, len(typed))
		for index, item := range typed {
			quoted[index] = "'" + item + "'"
		}
		return "[" + strings.Join(quoted, ", ") + "]"
	case string:
		return "'" + typed + "'"
	default:
		return fmt.Sprint(typed)
	}
}

// ParagraphPages ports resources.paragraph_pages: split document.md into bounded
// pages without ever splitting a display formula (FORMULA_BLOCK) and, for Word
// content, without splitting inline `$...$` math either.
func ParagraphPages(text string, protectInlineMath bool) []string {
	if text == "" {
		return []string{""}
	}
	byteRanges := FormulaBlock.FindAllStringIndex(text, -1)
	converted := make([][2]int, 0, len(byteRanges))
	for _, location := range byteRanges {
		converted = append(converted, [2]int{location[0], location[1]})
	}
	if protectInlineMath {
		converted = append(converted, inlineMathRanges(text)...)
	}
	sort.Slice(converted, func(left, right int) bool {
		if converted[left][0] == converted[right][0] {
			return converted[left][1] < converted[right][1]
		}
		return converted[left][0] < converted[right][0]
	})
	formulas := byteRangesToRuneRanges(text, converted)
	runes := []rune(text)
	pages := make([]string, 0, len(runes)/PageCharacters+1)
	start := 0
	for start < len(runes) {
		if len(runes)-start <= PageCharacters {
			pages = append(pages, string(runes[start:]))
			break
		}
		window := runes[start : start+PageCharacters]
		end := -1
		for index := len(window) - 2; index >= 0; index-- {
			if window[index] == '\n' && window[index+1] == '\n' {
				end = start + index + 2
				break
			}
		}
		if end < start {
			end = start + PageCharacters
		}
		for _, formula := range formulas {
			if formula[0] < end && end < formula[1] {
				if formula[0] > start {
					end = formula[0]
				} else {
					end = formula[1]
				}
				break
			}
		}
		pages = append(pages, string(runes[start:end]))
		start = end
	}
	if len(pages) == 0 {
		return []string{""}
	}
	return pages
}

// inlineMathRanges mirrors resources.paragraph_pages'
// r"(?<!\$)\$(?!\$)[^$]+\$(?!\$)" scan (Go regexp has no lookaround).
func inlineMathRanges(text string) [][2]int {
	result := make([][2]int, 0, 4)
	for index := 0; index < len(text); index++ {
		if text[index] != '$' {
			continue
		}
		if index > 0 && text[index-1] == '$' {
			continue
		}
		if index+1 < len(text) && text[index+1] == '$' {
			index++
			continue
		}
		closing := index + 1
		for closing < len(text) && text[closing] != '$' {
			closing++
		}
		if closing >= len(text) || closing == index+1 {
			continue
		}
		if closing+1 < len(text) && text[closing+1] == '$' {
			continue
		}
		result = append(result, [2]int{index, closing + 1})
		index = closing
	}
	return result
}

// byteRangesToRuneRanges converts ascending, non-overlapping byte offsets to
// rune offsets.
func byteRangesToRuneRanges(text string, ranges [][2]int) [][2]int {
	result := make([][2]int, 0, len(ranges))
	byteCursor := 0
	runeCursor := 0
	for _, location := range ranges {
		if location[0] < byteCursor {
			continue
		}
		runeCursor += utf8.RuneCountInString(text[byteCursor:location[0]])
		start := runeCursor
		runeCursor += utf8.RuneCountInString(text[location[0]:location[1]])
		result = append(result, [2]int{start, runeCursor})
		byteCursor = location[1]
	}
	return result
}
