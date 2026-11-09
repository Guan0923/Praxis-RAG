package ingest

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/document"
)

// wordInlineEquation mirrors word_assets.word_chunks' `re.finditer(r"\$[^$]+\$",
// text)`: inline formulas are kept inside one chunk.
var wordInlineEquation = regexp.MustCompile(`\$[^$]+\$`)

// buildWordChunks ports word_assets.word_chunks: pending text blocks are split
// with the configured strategy, inline equations never split, and picture/table
// assets become atomic caption/row chunks carrying the asset metadata.
func buildWordChunks(manifest document.WordManifest, documentID, source, title string,
	config chunking.Config, tokenizerModel string, counter chunking.TokenCounter) ([]chunking.Chunk, error) {
	probe, err := chunking.NewTokenBudget(counter, chunking.Config{Strategy: "token", ChunkSize: 1})
	if err != nil {
		return nil, err
	}
	maximum := probe.Maximum()
	budgetConfig := config
	if config.Strategy == "character" {
		budgetConfig = chunking.Config{Strategy: "token", ChunkSize: maximum, ChunkOverlap: 0}
	}
	budget, err := chunking.NewTokenBudget(counter, budgetConfig)
	if err != nil {
		return nil, err
	}
	assets := make(map[string]*document.WordManifestAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		assets[asset.AssetID] = asset
	}
	suffix := strings.ToLower(fileExtension(source))
	chunkingMetadata := config.Metadata(tokenizerModel)
	chunks := make([]chunking.Chunk, 0, 16)
	pending := make([]document.WordManifestBlock, 0, 8)

	appendChunk := func(text string, metadata map[string]any) error {
		if strings.TrimSpace(text) == "" {
			return nil
		}
		count := budget.Count(text)
		if count > maximum {
			return fmt.Errorf("Word item exceeds model input limit (%d tokens): %s", maximum, pythonRepr(metadata["refs"]))
		}
		index := len(chunks)
		chunkMetadata := map[string]any{
			"document_suffix":     suffix,
			"chunk_index":         index,
			"chunking":            cloneMap(chunkingMetadata),
			"word_parser_version": document.WordParserVersion,
			"token_count":         count,
		}
		for key, value := range metadata {
			chunkMetadata[key] = value
		}
		chunks = append(chunks, chunking.Chunk{
			DocumentID: documentID,
			ChunkID:    chunking.StableID("chunk", fmt.Sprintf("%s\x00%d\x00%s", documentID, index, text)),
			Text:       text,
			Title:      title,
			Source:     source,
			Metadata:   chunkMetadata,
		})
		return nil
	}

	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		pieces := make([]string, 0, len(pending))
		spans := make([]document.Span, 0, len(pending))
		offset := 0
		for _, block := range pending {
			if len(pieces) > 0 {
				offset += 2
			}
			length := utf8.RuneCountInString(block.Text)
			spans = append(spans, document.Span{
				Start: offset,
				End:   offset + length,
				Block: document.Block{
					Text: block.Text, Kind: block.Kind, Headings: block.Location.Headings,
				},
				Paragraph: len(spans),
			})
			pieces = append(pieces, block.Text)
			offset += length
		}
		text := strings.Join(pieces, "\n\n")
		content := document.Content{Text: text, Spans: spans}
		var ranges []chunking.Range
		if config.Strategy == "character" {
			ranges = chunking.CharacterRanges(text, config.ChunkSize, config.ChunkOverlap)
		} else {
			ranges, err = chunking.SplitContent(content, document.SuffixDOCX, config, budget)
			if err != nil {
				return err
			}
		}
		ranges = protectWordEquations([]rune(text), ranges)
		runes := []rune(text)
		for _, span := range ranges {
			covered := make([]document.WordManifestBlock, 0, len(pending))
			for index, block := range pending {
				if spans[index].Start < span.End && spans[index].End > span.Start {
					covered = append(covered, block)
				}
			}
			refs := make([]string, len(covered))
			locations := make([]map[string]any, len(covered))
			for index, block := range covered {
				refs[index] = block.Ref
				locations[index] = block.Location.Payload()
			}
			if err := appendChunk(string(runes[span.Start:span.End]), map[string]any{
				"refs": refs, "locations": locations, "kind": "text",
			}); err != nil {
				return err
			}
		}
		pending = pending[:0]
		return nil
	}

	for _, block := range manifest.Blocks {
		asset := assets[block.AssetID]
		if asset == nil {
			if len(block.AssociatedAssets) > 0 &&
				(block.Kind == document.KindCaption || block.Kind == document.KindNote) {
				continue
			}
			if block.Text != "" && !block.InCell {
				pending = append(pending, block)
			}
			continue
		}
		if err := flush(); err != nil {
			return nil, err
		}
		metadata := map[string]any{
			"kind":       asset.Kind,
			"asset_id":   asset.AssetID,
			"asset_kind": asset.Kind,
			"refs":       []string{block.Ref},
			"location":   block.Location.Payload(),
		}
		caption := asset.Caption
		if caption == "" {
			caption = asset.GeneratedDescription
		}
		noteTexts := make([]string, len(asset.Footnotes))
		for index, note := range asset.Footnotes {
			noteTexts[index] = note.Text
		}
		notes := strings.Join(noteTexts, "\n")
		if asset.Kind == "picture" {
			parts := make([]string, 0, 2)
			for _, part := range []string{caption, notes} {
				if part != "" {
					parts = append(parts, part)
				}
			}
			if err := appendChunk(strings.Join(parts, "\n"), metadata); err != nil {
				return nil, err
			}
			continue
		}
		if asset.Table == nil {
			return nil, fmt.Errorf("Word manifest table asset has no table data: %s", asset.AssetID)
		}
		headers := make([]string, len(asset.Table.HeaderRows))
		for index, row := range asset.Table.HeaderRows {
			headers[index] = document.TableRowText(row)
		}
		headerText := strings.Join(headers, "\n")
		for rowNumber, row := range asset.Table.Rows {
			parts := make([]string, 0, 4)
			for _, part := range []string{caption, headerText, document.TableRowText(row), notes} {
				if part != "" {
					parts = append(parts, part)
				}
			}
			rowMetadata := cloneMap(metadata)
			rowMetadata["table_row"] = rowNumber
			if err := appendChunk(strings.Join(parts, "\n"), rowMetadata); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return chunks, nil
}

// protectWordEquations ports the inline-equation guard in word_assets.flush:
// chunk boundaries that fall inside a `$...$` run are widened, and duplicate
// adjusted ranges are emitted only once.
func protectWordEquations(runes []rune, ranges []chunking.Range) []chunking.Range {
	text := string(runes)
	if !strings.Contains(text, "$") {
		return ranges
	}
	equations := make([]chunking.Range, 0, 4)
	byteCursor := 0
	runeCursor := 0
	for _, location := range wordInlineEquation.FindAllStringIndex(text, -1) {
		runeCursor += utf8.RuneCountInString(text[byteCursor:location[0]])
		start := runeCursor
		runeCursor += utf8.RuneCountInString(text[location[0]:location[1]])
		equations = append(equations, chunking.Range{Start: start, End: runeCursor})
		byteCursor = location[1]
	}
	result := make([]chunking.Range, 0, len(ranges))
	emitted := make(map[[2]int]struct{}, len(ranges))
	for _, candidate := range ranges {
		start, end := candidate.Start, candidate.End
		for _, equation := range equations {
			if equation.Start < start && start < equation.End {
				start = equation.Start
			}
			if equation.Start < end && end < equation.End {
				end = equation.End
			}
		}
		key := [2]int{start, end}
		if _, ok := emitted[key]; ok {
			continue
		}
		emitted[key] = struct{}{}
		result = append(result, chunking.Range{Start: start, End: end})
	}
	return result
}

// fileExtension mirrors pathlib.Path(source).suffix.lower() for display names.
func fileExtension(name string) string {
	base := name
	if index := strings.LastIndexAny(base, `/\`); index >= 0 {
		base = base[index+1:]
	}
	if index := strings.LastIndex(base, "."); index > 0 {
		return base[index:]
	}
	return ""
}

// pythonRepr formats values the way Python f-strings render them in the model
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

func cloneMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}
