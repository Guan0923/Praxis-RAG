// Package ingest is the storage-free document ingestion adapter the runtime
// calls for every supported upload. It dispatches by document type and returns
// chunks, assets, diagnostics and artifact staging paths; it has no state,
// Qdrant, embedding or queue dependencies and performs no persistence.
//
// Dispatch (mirrors DenseDocumentIngester.ingest_and_publish):
//   - .pdf: the injected PDFParser (default: in-process NativePDFParser)
//     produces a manifest, then BuildPDFContent + pdfcontent.ContentChunks.
//   - .doc/.docx: ParseWordPackage (legacy .doc converted first), optional
//     in-process VLM picture descriptions, BuildWordContent, then the Word
//     chunk builder (word_assets.word_chunks port).
//   - .md/.txt (and any other suffix, which fails in the reader): the
//     rag_mcp.chunking.chunk_document port (chunking.ChunkDocument).
//
// Chunk type mapping: Result.Chunks is []chunking.Chunk, the direct Go
// counterpart of rag_mcp.chunking.DocumentChunk (document_id/chunk_id/text/
// title/source/metadata) used by chunk_document, word_chunks and
// pdf_content.content_chunks. Document ids are chunking.DocumentID(sourceName),
// matching Python's _stable_id("doc", name.casefold()).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/document"
	"github.com/Guan0923/xinghe_rag/internal/document/pdfcontent"
)

const defaultTokenizerModel = "BAAI/bge-m3"

// PDFParser parses one PDF into a document manifest and writes the parser
// artifacts (manifest.json, document.md) into stagingDir.
// document.NativePDFParser satisfies it; tests inject fakes.
type PDFParser interface {
	ParseInto(ctx context.Context, source, stagingDir string) (document.PDFManifest, error)
}

// PDFOptions selects how PDFs are parsed. When Parser is nil the in-process
// document.NativePDFParser is used.
type PDFOptions struct {
	// Parser overrides the in-process parser (tests or custom engines).
	Parser PDFParser
}

// WordOptions selects Word parsing dependencies.
type WordOptions struct {
	// Converter converts legacy .doc files before OOXML parsing. nil means a
	// document.SofficeConverter (environment auto-detection) is used.
	Converter document.DOCConverter
	// Vision describes uncaptioned pictures through the remote VLM before
	// chunking. nil keeps generated_description empty.
	Vision *document.WordVisionWorker
}

// Options configure one Prepare call.
type Options struct {
	// SourceName is the display name including suffix (for example
	// "manual.docx"). It drives source/document_id/chunk metadata and, for
	// Word/PDF, the parser version record. When empty it falls back to the
	// base name of sourcePath.
	SourceName string
	// Chunking is the knowledge-base chunking configuration. The zero value
	// falls back to the legacy character configuration (700/100), matching
	// Python's `chunking or LEGACY_CONFIG`.
	Chunking chunking.Config
	// TokenizerModel is recorded in the chunking metadata (default BGE-M3).
	TokenizerModel string
	// PDF configures PDF parsing.
	PDF PDFOptions
	// Word configures legacy conversion and vision descriptions.
	Word WordOptions
	// Counter overrides the token counter (default chunking.DefaultCounter).
	Counter chunking.TokenCounter
	// StagingDir receives parser artifacts (manifest.json, document.md,
	// images). Empty means Prepare creates a temp directory; the caller owns
	// and must clean Result.StagingDir in both cases.
	StagingDir string
}

// Result is one prepared document, ready to persist and index.
type Result struct {
	// Chunks are the index chunks (rag_mcp.chunking.DocumentChunk equivalent).
	Chunks []chunking.Chunk
	// Title is the document title (same value as each chunk's Title).
	Title string
	// Metadata contains the document-level indexing configuration:
	//   "document_suffix"      string
	//   "chunking"             map[string]any (strategy/size/overlap/tokenizer
	//                          plus pdf_parser_version or word_parser_version)
	//   "parser_version"       string (Word/PDF only)
	//   "diagnostics"          []document.Diagnostic
	//   "skipped_object_count" int
	Metadata map[string]any
	// Assets are storage-decoupled Word/PDF picture and table records.
	Assets []document.Asset
	// Diagnostics are safe Word parser/association diagnostics.
	Diagnostics []document.Diagnostic
	// SkippedObjectCount counts Word objects skipped during association.
	SkippedObjectCount int
	// StagingDir holds parser artifacts (manifest.json/document.md/images) for
	// empty for text documents. The caller persists it and removes the
	// directory when done.
	StagingDir string
}

// ErrNoIndexableText marks a document that produced no chunks; callers can use
// errors.Is to map it to their validation error type.
var ErrNoIndexableText = errors.New("document has no indexable text")

// NoIndexableTextError carries the Python-compatible safe message for an empty
// document.
type NoIndexableTextError struct{ Message string }

func (e *NoIndexableTextError) Error() string { return e.Message }

// Unwrap supports errors.Is(err, ErrNoIndexableText).
func (e *NoIndexableTextError) Unwrap() error { return ErrNoIndexableText }

// ErrorCategory implements the logging classifier seam so runtime logs and
// persisted job reasons report a stable category instead of the Go type name.
func (e *NoIndexableTextError) ErrorCategory() string { return "no_indexable_text" }

// Prepare parses one supported document into chunks, assets and artifacts.
// sourcePath is the file on disk; options.SourceName is the display name used
// for ids and metadata. The returned staging directory is never removed by
// Prepare: the caller persists it and then removes it.
func Prepare(ctx context.Context, sourcePath string, options Options) (Result, error) {
	if strings.TrimSpace(options.SourceName) == "" {
		options.SourceName = filepath.Base(sourcePath)
	}
	if options.TokenizerModel == "" {
		options.TokenizerModel = defaultTokenizerModel
	}
	if options.Counter == nil {
		options.Counter = chunking.DefaultCounter
	}
	config, err := effectiveConfig(options.Chunking)
	if err != nil {
		return Result{}, err
	}
	options.Chunking = config
	if ctx == nil {
		ctx = context.Background()
	}
	suffix := strings.ToLower(filepath.Ext(sourcePath))
	switch suffix {
	case document.SuffixPDF:
		return preparePDF(ctx, sourcePath, options)
	case document.SuffixDOC, document.SuffixDOCX:
		return prepareWord(ctx, sourcePath, options)
	default:
		return prepareText(ctx, sourcePath, options)
	}
}

// effectiveConfig validates the requested configuration, mapping the zero value
// to Python's LEGACY_CONFIG (character, 700/100).
func effectiveConfig(config chunking.Config) (chunking.Config, error) {
	if config.Strategy == "" {
		return chunking.NewConfig("character", 700, 100)
	}
	return chunking.NewConfig(config.Strategy, config.ChunkSize, config.ChunkOverlap)
}

// prepareText ports chunk_document's path for Markdown/TXT (and any unsupported
// suffix, which then fails inside the reader with the Python message).
func prepareText(ctx context.Context, sourcePath string, options Options) (Result, error) {
	chunks, err := chunking.ChunkDocument(sourcePath, chunking.ChunkOptions{
		SourceName:     options.SourceName,
		Config:         &options.Chunking,
		TokenizerModel: options.TokenizerModel,
		Counter:        options.Counter,
		Context:        ctx,
	})
	if err != nil {
		return Result{}, err
	}
	if len(chunks) == 0 {
		return Result{}, noIndexableError("知识文件没有可索引的文本：" + options.SourceName)
	}
	documentSuffix := chunks[0].Metadata["document_suffix"]
	return Result{
		Chunks:      chunks,
		Title:       chunks[0].Title,
		Assets:      nil,
		Diagnostics: []document.Diagnostic{},
		Metadata: map[string]any{
			"document_suffix":      documentSuffix,
			"chunking":             documentConfiguration(options.Chunking, options.TokenizerModel, "", ""),
			"diagnostics":          []document.Diagnostic{},
			"skipped_object_count": 0,
		},
	}, nil
}

// prepareWord ports word_assets.parse_word + word_chunks.
func prepareWord(ctx context.Context, sourcePath string, options Options) (Result, error) {
	staging, cleanup, err := ensureStaging(options.StagingDir)
	if err != nil {
		return Result{}, err
	}
	fail := func(err error) (Result, error) {
		cleanup()
		return Result{}, err
	}
	parsePath := sourcePath
	if strings.ToLower(filepath.Ext(sourcePath)) == document.SuffixDOC {
		converter := options.Word.Converter
		if converter == nil {
			converter = &document.SofficeConverter{}
		}
		converted, convertCleanup, err := converter.Convert(ctx, sourcePath)
		if err != nil {
			return fail(err)
		}
		if convertCleanup != nil {
			defer convertCleanup()
		}
		parsePath = converted
	}
	pkg, err := document.ParseWordPackage(parsePath)
	if err != nil {
		return fail(err)
	}
	if err := pkg.WriteStaging(staging); err != nil {
		return fail(err)
	}
	if options.Word.Vision != nil && len(document.MissingPictureCaptions(pkg.Manifest)) > 0 {
		if err := options.Word.Vision.DescribeMissingCaptions(ctx, staging); err != nil {
			return fail(err)
		}
		if err := pkg.ReloadFromStaging(staging); err != nil {
			return fail(err)
		}
	}
	documentID := chunking.DocumentID(options.SourceName)
	content, err := document.BuildWordContent(documentID, pkg)
	if err != nil {
		return fail(err)
	}
	title := sourceStem(options.SourceName)
	chunks, err := buildWordChunks(content.Manifest, documentID, options.SourceName, title,
		options.Chunking, options.TokenizerModel, options.Counter)
	if err != nil {
		return fail(err)
	}
	if len(chunks) == 0 {
		// word_assets.parse_word reports the original path name, not source name.
		return fail(noIndexableError("Word document has no indexable content: " + filepath.Base(sourcePath)))
	}
	if err := content.WriteArtifacts(staging); err != nil {
		return fail(err)
	}
	return Result{
		Chunks:             chunks,
		Title:              title,
		Metadata:           wordMetadata(content, options),
		Assets:             content.Assets,
		Diagnostics:        content.Diagnostics,
		SkippedObjectCount: content.SkippedObjectCount,
		StagingDir:         staging,
	}, nil
}

func wordMetadata(content *document.WordContent, options Options) map[string]any {
	metadata := documentConfiguration(options.Chunking, options.TokenizerModel, "word_parser_version", document.WordParserVersion)
	diagnostics := content.Diagnostics
	if diagnostics == nil {
		diagnostics = []document.Diagnostic{}
	}
	return map[string]any{
		"document_suffix":      strings.ToLower(filepath.Ext(options.SourceName)),
		"chunking":             metadata,
		"parser_version":       document.WordParserVersion,
		"diagnostics":          diagnostics,
		"skipped_object_count": content.SkippedObjectCount,
	}
}

// preparePDF ports pdf_assets.parse_pdf + pdf_content.content_chunks.
func preparePDF(ctx context.Context, sourcePath string, options Options) (Result, error) {
	staging, cleanup, err := ensureStaging(options.StagingDir)
	if err != nil {
		return Result{}, err
	}
	fail := func(err error) (Result, error) {
		cleanup()
		return Result{}, err
	}
	parser := options.PDF.Parser
	if parser == nil {
		parser = &document.NativePDFParser{}
	}
	manifest, err := parser.ParseInto(ctx, sourcePath, staging)
	if err != nil {
		return fail(err)
	}
	if !document.IsSupportedPDFParserVersion(manifest.Version) {
		return fail(fmt.Errorf("PDF parser version mismatch; reimport this document"))
	}
	documentID := chunking.DocumentID(options.SourceName)
	content, err := document.BuildPDFContent(manifest, documentID, staging)
	if err != nil {
		return fail(err)
	}
	chunks, err := pdfcontent.ContentChunks(content.Manifest, pdfcontent.Options{
		DocumentID:     documentID,
		Source:         options.SourceName,
		FallbackTitle:  sourceStem(options.SourceName),
		Config:         options.Chunking,
		TokenizerModel: options.TokenizerModel,
		Counter:        options.Counter,
	})
	if err != nil {
		return fail(err)
	}
	if len(chunks) == 0 {
		return fail(noIndexableError("PDF 没有可索引的内容：" + filepath.Base(sourcePath)))
	}
	if err := content.WriteDocumentMarkdown(staging); err != nil {
		return fail(err)
	}
	return Result{
		Chunks:      chunks,
		Title:       chunks[0].Title,
		Metadata:    pdfMetadata(options, manifest.Version),
		Assets:      content.Assets,
		Diagnostics: []document.Diagnostic{},
		StagingDir:  staging,
	}, nil
}

// pdfMetadata records the parser version carried by the parsed manifest, so
// documents imported through the Docling engine keep their own version instead
// of the in-process default.
func pdfMetadata(options Options, parserVersion string) map[string]any {
	return map[string]any{
		"document_suffix":      document.SuffixPDF,
		"chunking":             documentConfiguration(options.Chunking, options.TokenizerModel, "pdf_parser_version", parserVersion),
		"parser_version":       parserVersion,
		"diagnostics":          []document.Diagnostic{},
		"skipped_object_count": 0,
	}
}

// documentConfiguration mirrors service_knowledge._index_configuration: the
// chunking metadata with the document-type parser version added.
func documentConfiguration(config chunking.Config, tokenizerModel, parserKey, parserVersion string) map[string]any {
	metadata := config.Metadata(tokenizerModel)
	if parserKey != "" {
		metadata[parserKey] = parserVersion
	}
	return metadata
}

// ensureStaging creates the caller's staging directory or a temporary one. The
// returned cleanup removes only directories prepared created itself; the caller
// still removes the returned staging directory after persisting it.
func ensureStaging(preferred string) (string, func(), error) {
	if preferred != "" {
		if err := os.MkdirAll(preferred, 0o755); err != nil {
			return "", func() {}, fmt.Errorf("document staging directory is not writable: %v", err)
		}
		return preferred, func() {}, nil
	}
	dir, err := os.MkdirTemp("", "rag-ingest-*")
	if err != nil {
		return "", func() {}, fmt.Errorf("document staging directory is not writable: %v", err)
	}
	return dir, func() { _ = os.RemoveAll(dir) }, nil
}

// sourceStem mirrors pathlib.Path(source_name).stem.
func sourceStem(name string) string {
	base := filepath.Base(strings.ReplaceAll(name, "/", string(filepath.Separator)))
	return strings.TrimSuffix(base, filepath.Ext(base))
}

func noIndexableError(message string) error {
	return &NoIndexableTextError{Message: message}
}
