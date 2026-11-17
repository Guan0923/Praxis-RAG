package ingest

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/document"
)

// DoclingParser parses one DOCX/HTML file through the independent Docling HTTP
// service. The implementation writes the service's manifest.json and image
// files into stagingDir and returns the structured package; it is
// document.DoclingServiceParser in production and a fake in tests.
//
// sourcePath is the file to upload (for legacy .doc it is the converted DOCX);
// sourceName is the original display name used in errors and metadata.
type DoclingParser interface {
	ParseInto(ctx context.Context, sourcePath, sourceName, stagingDir string) (*document.WordPackage, error)
}

// DoclingOptions selects the Docling service for Word and static HTML parsing.
// When Parser is nil, DOCX keeps the in-process OOXML parser and HTML import
// fails with a configuration error; DOC conversion is unchanged.
type DoclingOptions struct {
	// Parser calls the Docling service. nil disables the Docling path.
	Parser DoclingParser
}

// ErrDoclingNotConfigured marks an HTML import without a configured Docling
// parser; callers can map it to their validation error type.
var ErrDoclingNotConfigured = fmt.Errorf("HTML 文档需要配置 Docling 解析服务")

// prepareDocling parses .docx, .doc (after LibreOffice conversion) and
// .html/.htm through the Docling service, then reuses the Word asset/chunk
// pipeline so table structure, captions and images reach the evidence
// unchanged. The suffix comes from the original source name, so a converted
// .doc still records itself as .doc.
func prepareDocling(ctx context.Context, sourcePath string, options Options) (Result, error) {
	parser := options.Docling.Parser
	if parser == nil {
		return Result{}, ErrDoclingNotConfigured
	}
	staging, cleanup, err := ensureStaging(options.StagingDir)
	if err != nil {
		return Result{}, err
	}
	fail := func(err error) (Result, error) {
		cleanup()
		return Result{}, err
	}
	parsePath := sourcePath
	originalSuffix := strings.ToLower(filepath.Ext(options.SourceName))
	if originalSuffix == "" {
		originalSuffix = strings.ToLower(filepath.Ext(sourcePath))
	}
	if originalSuffix == document.SuffixDOC {
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
	pkg, err := parser.ParseInto(ctx, parsePath, options.SourceName, staging)
	if err != nil {
		return fail(err)
	}
	if err := doclingServiceStagingIsComplete(staging, pkg.Manifest); err != nil {
		return fail(err)
	}
	expectedVersion := document.DoclingWordParserVersion
	if originalSuffix == document.SuffixHTML || originalSuffix == document.SuffixHTM {
		expectedVersion = document.HTMLParserVersion
	}
	if pkg.Manifest.Version != expectedVersion {
		if !document.IsSupportedDoclingManifestVersion(pkg.Manifest.Version) {
			return fail(fmt.Errorf("Docling 解析结果版本不受支持，请重新导入"))
		}
		return fail(fmt.Errorf("Docling 解析结果与文件格式不匹配"))
	}
	// DOCX captions arrive as ordinary paragraphs; classify and attach the
	// unambiguous ones before the vision step, so explicit captions are never
	// replaced by generated descriptions.
	pkg.Manifest = document.AssociateDoclingCaptions(pkg.Manifest)
	if err := pkg.Manifest.WriteFile(filepath.Join(staging, "manifest.json")); err != nil {
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
	parserKey, parserVersion := "word_parser_version", document.DoclingWordParserVersion
	if originalSuffix == document.SuffixHTML || originalSuffix == document.SuffixHTM {
		parserKey, parserVersion = "html_parser_version", document.HTMLParserVersion
		// HTML assets live in their own bucket and payload storage variant, so
		// they are never presented as Word assets.
		for index := range content.Assets {
			content.Assets[index].Storage = "html"
		}
	}
	title := sourceStem(options.SourceName)
	chunks, err := buildWordChunks(content.Manifest, documentID, options.SourceName, title,
		options.Chunking, options.TokenizerModel, options.Counter, parserKey, parserVersion)
	if err != nil {
		return fail(err)
	}
	if len(chunks) == 0 {
		if parserKey == "html_parser_version" {
			return fail(noIndexableError("HTML 文档没有可索引的文本：" + filepath.Base(sourcePath)))
		}
		return fail(noIndexableError("Word document has no indexable content: " + filepath.Base(sourcePath)))
	}
	if err := content.WriteArtifacts(staging); err != nil {
		return fail(err)
	}
	return Result{
		Chunks:             chunks,
		Title:              title,
		Metadata:           doclingMetadata(content, options, originalSuffix, parserKey, parserVersion),
		Assets:             content.Assets,
		Diagnostics:        content.Diagnostics,
		SkippedObjectCount: content.SkippedObjectCount,
		StagingDir:         staging,
	}, nil
}

// doclingMetadata mirrors wordMetadata for Docling-produced content: the
// document suffix stays the original one and the recorded parser version names
// the service (docling-word-v1 / docling-html-v1), never word-assets-v1.
func doclingMetadata(content *document.WordContent, options Options, suffix, parserKey, parserVersion string) map[string]any {
	metadata := documentConfiguration(options.Chunking, options.TokenizerModel, parserKey, parserVersion)
	diagnostics := content.Diagnostics
	if diagnostics == nil {
		diagnostics = []document.Diagnostic{}
	}
	documentSuffix := suffix
	if documentSuffix == "" {
		documentSuffix = strings.ToLower(filepath.Ext(options.SourceName))
	}
	return map[string]any{
		"document_suffix":      documentSuffix,
		"chunking":             metadata,
		"parser_version":       parserVersion,
		"diagnostics":          diagnostics,
		"skipped_object_count": content.SkippedObjectCount,
	}
}

// doclingServiceStagingIsComplete verifies that a parser left the manifest and
// every referenced image in staging; it documents the client contract and gives
// tests a cheap assertion point.
func doclingServiceStagingIsComplete(dir string, manifest document.WordManifest) error {
	if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err != nil {
		return fmt.Errorf("Docling manifest is missing from staging")
	}
	for _, asset := range manifest.Assets {
		if asset == nil || asset.ImageFile == nil {
			continue
		}
		info, err := os.Stat(filepath.Join(dir, filepath.FromSlash(*asset.ImageFile)))
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("Docling image is missing from staging: %s", *asset.ImageFile)
		}
	}
	return nil
}
