package ingest

import (
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/document"
)

func mustConfig(t *testing.T, strategy string, size, overlap int) chunking.Config {
	t.Helper()
	config, err := chunking.NewConfig(strategy, size, overlap)
	if err != nil {
		t.Fatalf("NewConfig(%q,%d,%d) error = %v", strategy, size, overlap, err)
	}
	return config
}

func chunksWithMetadata(chunks []chunking.Chunk, key string, value any) []chunking.Chunk {
	result := make([]chunking.Chunk, 0, len(chunks))
	for _, chunk := range chunks {
		if chunk.Metadata[key] == value {
			result = append(result, chunk)
		}
	}
	return result
}

func TestWordChunkStrategiesPreserveTableHeaders(t *testing.T) {
	// Port of tests/test_word_fidelity.py::test_word_chunk_strategies_preserve_table_headers.
	body := headingParagraphXML("Specification", 1) +
		tableXML([]string{"Model", "Limit"}, []string{"ZX256", "90"})
	path := plainDocxPackage(t, body, true)
	for _, strategy := range []string{"character", "token", "sentence", "recursive", "structure"} {
		t.Run(strategy, func(t *testing.T) {
			config := mustConfig(t, strategy, 256, 16)
			result, err := Prepare(t.Context(), path, Options{
				SourceName: "spec.docx",
				Chunking:   config,
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			rows := chunksWithMetadata(result.Chunks, "asset_kind", "table")
			if len(rows) != 2 {
				t.Fatalf("table chunks = %d, want 2 (%v)", len(rows), result.Chunks)
			}
			for _, row := range rows {
				if !strings.Contains(row.Text, "Model | Limit") {
					t.Fatalf("table header missing: %q", row.Text)
				}
			}
			if !strings.Contains(rows[len(rows)-1].Text, "ZX256 | 90") {
				t.Fatalf("table row missing: %q", rows[len(rows)-1].Text)
			}
			if result.Title != "spec" {
				t.Fatalf("title = %q, want source stem", result.Title)
			}
			for _, chunk := range result.Chunks {
				if chunk.DocumentID != chunking.DocumentID("spec.docx") {
					t.Fatalf("document id = %q", chunk.DocumentID)
				}
				if chunk.Source != "spec.docx" || chunk.Title != "spec" {
					t.Fatalf("chunk identity = %+v", chunk)
				}
			}
		})
	}
}

func TestWordPictureCaptionAndNoteChunk(t *testing.T) {
	body := pictureParagraph("Figure 1") +
		paragraphXML("Figure 1: Red status lamp") +
		paragraphXML("Note: keep this lamp visible")
	path := pictureDocxPackage(t, body, false)
	result, err := Prepare(t.Context(), path, Options{SourceName: "picture.docx"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	pictures := chunksWithMetadata(result.Chunks, "asset_kind", "picture")
	if len(pictures) != 1 {
		t.Fatalf("picture chunks = %d", len(pictures))
	}
	picture := pictures[0]
	if picture.Text != "Figure 1: Red status lamp\nNote: keep this lamp visible" {
		t.Fatalf("picture chunk text = %q", picture.Text)
	}
	if picture.Metadata["kind"] != "picture" {
		t.Fatalf("kind = %v", picture.Metadata["kind"])
	}
	assets := result.Assets
	if len(assets) != 1 {
		t.Fatalf("assets = %d", len(assets))
	}
	if !strings.HasPrefix(assets[0].ID, "doc-") || !strings.Contains(assets[0].ID, ":picture-1") {
		t.Fatalf("asset id = %q", assets[0].ID)
	}
	if picture.Metadata["asset_id"] != assets[0].ID {
		t.Fatalf("chunk asset_id = %v, asset = %v", picture.Metadata["asset_id"], assets[0].ID)
	}
	location, ok := picture.Metadata["location"].(map[string]any)
	if !ok || location["part"] != "word/document.xml" || location["scope"] != "body" {
		t.Fatalf("location = %v", picture.Metadata["location"])
	}
	refs, ok := picture.Metadata["refs"].([]string)
	if !ok || len(refs) != 1 {
		t.Fatalf("refs = %v", picture.Metadata["refs"])
	}
}

func TestWordInlineEquationIsNotSplit(t *testing.T) {
	longPrefix := strings.Repeat("a", 90)
	longSuffix := strings.Repeat("b", 200)
	body := `<w:p><w:r><w:t>` + longPrefix + `</w:t></w:r>` +
		`<w:r><m:oMath><m:f><m:num><m:r><m:t>x</m:t></m:r></m:num>` +
		`<m:den><m:r><m:t>2</m:t></m:r></m:den></m:f></m:oMath></w:r>` +
		`<w:r><w:t>` + longSuffix + `</w:t></w:r></w:p>`
	path := plainDocxPackage(t, body, false)
	config := mustConfig(t, "character", 100, 10)
	result, err := Prepare(t.Context(), path, Options{SourceName: "formula.docx", Chunking: config})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	formula := `$\frac{x}{2}$`
	found := 0
	for _, chunk := range result.Chunks {
		if !strings.Contains(chunk.Text, "$") {
			continue
		}
		found++
		if strings.Count(chunk.Text, "$") != 2 || !strings.Contains(chunk.Text, formula) {
			t.Fatalf("inline formula was split: %q", chunk.Text)
		}
	}
	if found == 0 {
		t.Fatalf("no chunk contains the inline formula: %v", result.Chunks)
	}
}

func TestWordAmbiguousAssetsAreSkipped(t *testing.T) {
	body := pictureParagraph("") + paragraphXML("Figure 7: ambiguous") + pictureParagraph("")
	path := pictureDocxPackage(t, body, false)
	result, err := Prepare(t.Context(), path, Options{SourceName: "ambiguous.docx"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.SkippedObjectCount != 2 {
		t.Fatalf("skipped = %d, want 2", result.SkippedObjectCount)
	}
	if len(result.Diagnostics) != 1 || result.Diagnostics[0].Kind != "unassociated_caption" {
		t.Fatalf("diagnostics = %+v", result.Diagnostics)
	}
	if len(result.Assets) != 0 {
		t.Fatalf("skipped assets are still present: %+v", result.Assets)
	}
	if len(chunksWithMetadata(result.Chunks, "asset_kind", "picture")) != 0 {
		t.Fatalf("skipped picture produced an asset chunk")
	}
	joined := make([]string, len(result.Chunks))
	for index, chunk := range result.Chunks {
		joined[index] = chunk.Text
	}
	if !strings.Contains(strings.Join(joined, "\n"), "Figure 7: ambiguous") {
		t.Fatalf("caption text was lost: %v", joined)
	}
	if result.Metadata["skipped_object_count"] != 2 {
		t.Fatalf("metadata skipped_object_count = %v", result.Metadata["skipped_object_count"])
	}
	if diagnostics, ok := result.Metadata["diagnostics"].([]document.Diagnostic); !ok || len(diagnostics) != 1 {
		t.Fatalf("metadata diagnostics = %v", result.Metadata["diagnostics"])
	}
}

func TestWordChunkMetadataShape(t *testing.T) {
	body := headingParagraphXML("Manual", 1) + paragraphXML("Body text under the heading.")
	path := plainDocxPackage(t, body, true)
	config := mustConfig(t, "token", 64, 12)
	result, err := Prepare(t.Context(), path, Options{SourceName: "manual.docx", Chunking: config, TokenizerModel: "test-model"})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	textChunks := chunksWithMetadata(result.Chunks, "kind", "text")
	if len(textChunks) == 0 {
		t.Fatalf("no text chunks")
	}
	chunk := textChunks[0]
	if chunk.Metadata["document_suffix"] != ".docx" ||
		chunk.Metadata["word_parser_version"] != document.WordParserVersion {
		t.Fatalf("chunk metadata = %v", chunk.Metadata)
	}
	chunkingMetadata := chunk.Metadata["chunking"].(map[string]any)
	if chunkingMetadata["strategy"] != "token" || chunkingMetadata["tokenizer"] != "test-model" ||
		chunkingMetadata["chunk_size"] != 64 {
		t.Fatalf("chunking metadata = %v", chunkingMetadata)
	}
	if chunk.Metadata["chunk_index"] != 0 {
		t.Fatalf("chunk_index = %v", chunk.Metadata["chunk_index"])
	}
	count, ok := chunk.Metadata["token_count"].(int)
	if !ok || count <= 0 || count > 64 {
		t.Fatalf("token_count = %v", chunk.Metadata["token_count"])
	}
	if refs, ok := chunk.Metadata["refs"].([]string); !ok || len(refs) == 0 {
		t.Fatalf("refs = %v", chunk.Metadata["refs"])
	}
	locations, ok := chunk.Metadata["locations"].([]map[string]any)
	if !ok || len(locations) == 0 {
		t.Fatalf("locations = %v", chunk.Metadata["locations"])
	}
	headings, ok := locations[0]["headings"].([]string)
	if !ok || len(headings) != 1 || headings[0] != "Manual" {
		t.Fatalf("heading path = %v", locations[0]["headings"])
	}
	documentMetadata := result.Metadata
	if documentMetadata["document_suffix"] != ".docx" || documentMetadata["parser_version"] != document.WordParserVersion {
		t.Fatalf("result metadata = %v", documentMetadata)
	}
	if result.Title != "manual" {
		t.Fatalf("title = %q", result.Title)
	}
}

func TestWordCharacterChunkTextLimit(t *testing.T) {
	body := paragraphXML(strings.Repeat("设备正常工作。", 60))
	path := plainDocxPackage(t, body, false)
	config := mustConfig(t, "character", 100, 10)
	result, err := Prepare(t.Context(), path, Options{SourceName: "limits.docx", Chunking: config})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if len(result.Chunks) < 2 {
		t.Fatalf("chunks = %d", len(result.Chunks))
	}
	for _, chunk := range result.Chunks {
		if stringLength := len([]rune(chunk.Text)); stringLength > 100 {
			t.Fatalf("character chunk over limit (%d): %q", stringLength, chunk.Text)
		}
	}
}

func TestWordEmptyDocumentHasClearError(t *testing.T) {
	path := plainDocxPackage(t, `<w:p/>`, false)
	_, err := Prepare(t.Context(), path, Options{SourceName: "empty.docx"})
	if err == nil || !strings.Contains(err.Error(), "Word document has no indexable content") {
		t.Fatalf("error = %v", err)
	}
	if !isNoIndexable(err) {
		t.Fatalf("error is not ErrNoIndexableText: %v", err)
	}
}

func isNoIndexable(err error) bool {
	for current := err; current != nil; {
		if current == ErrNoIndexableText {
			return true
		}
		unwrapper, ok := current.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		current = unwrapper.Unwrap()
	}
	return false
}
