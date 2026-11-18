package ingest

// The Docling integration test runs the real service over HTTP and exercises
// the existing ingest.Prepare entry point, mirroring the acceptance criterion
// "real DOCX and HTML through the real Docling service and the Go ingest
// entry". It is skipped unless RAG_DOCLING_SERVICE_URL points at a running
// service, so the default `go test ./...` stays hermetic.

import (
	"context"
	"os"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/document"
)

func doclingIntegrationParser(t *testing.T) *document.DoclingServiceParser {
	t.Helper()
	baseURL := strings.TrimSpace(os.Getenv("RAG_DOCLING_SERVICE_URL"))
	if baseURL == "" {
		t.Skip("set RAG_DOCLING_SERVICE_URL to run the real Docling integration test")
	}
	return &document.DoclingServiceParser{BaseURL: baseURL}
}

func doclingIntegrationOptions(t *testing.T, name string, staging string) Options {
	t.Helper()
	return Options{
		SourceName: name,
		Chunking:   chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100},
		StagingDir: staging,
		Docling:    DoclingOptions{Parser: doclingIntegrationParser(t)},
	}
}

// TestDoclingIntegrationDOCX reuses the project Word fixtures: a heading,
// paragraph, merged table, captioned picture and a following caption-like
// paragraph. The real service must preserve the table structure and the Go
// association must attach the explicit caption to the picture.
func TestDoclingIntegrationDOCX(t *testing.T) {
	body := headingParagraphXML("X7 系统手册", 1) +
		paragraphXML("本文档描述 X7 的安装、配置与故障处理流程。") +
		headingParagraphXML("硬件参数", 2) +
		paragraphXML("表 1 硬件参数表") +
		tableXML([]string{"部件", "规格", "数量"}, []string{"处理器", "8 核 3.2GHz", "1"}, []string{"内存", "32 GB", "2"}) +
		headingParagraphXML("架构图", 2) +
		paragraphXML("图 1 系统架构图") +
		pictureParagraph("架构图") +
		paragraphXML("图 1 展示了检索与生成之间的证据链路。")
	source := pictureDocxPackage(t, body, true)
	staging := t.TempDir()
	result, err := Prepare(context.Background(), source, doclingIntegrationOptions(t, "manual.docx", staging))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Metadata["parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("parser_version = %v, want %s", result.Metadata["parser_version"], document.DoclingWordParserVersion)
	}
	chunkingMetadata, ok := result.Metadata["chunking"].(map[string]any)
	if !ok || chunkingMetadata["word_parser_version"] != document.DoclingWordParserVersion {
		t.Fatalf("word_parser_version = %v", chunkingMetadata["word_parser_version"])
	}
	if len(result.Assets) < 2 {
		t.Fatalf("assets = %+v, want at least a table and a picture", result.Assets)
	}
	var table, picture *document.Asset
	for index := range result.Assets {
		switch result.Assets[index].Kind {
		case "table":
			table = &result.Assets[index]
		case "picture":
			picture = &result.Assets[index]
		}
	}
	if table == nil || table.Table == nil {
		t.Fatalf("table asset missing: %+v", result.Assets)
	}
	if table.Table.NumRows != 3 || table.Table.NumCols != 3 {
		t.Fatalf("table shape = %dx%d", table.Table.NumRows, table.Table.NumCols)
	}
	if !strings.Contains(table.Caption, "表 1 硬件参数表") {
		t.Fatalf("table caption = %q", table.Caption)
	}
	if picture == nil || len(picture.Data) == 0 {
		t.Fatalf("picture asset has no image data: %+v", picture)
	}
	if !strings.Contains(picture.Caption, "图 1 系统架构图") {
		t.Fatalf("picture caption = %q", picture.Caption)
	}
	var rowText, pictureText string
	for _, chunk := range result.Chunks {
		if strings.Contains(chunk.Text, "处理器") {
			rowText = chunk.Text
		}
		if chunk.Metadata["asset_kind"] == "picture" {
			pictureText = chunk.Text
		}
	}
	if !strings.Contains(rowText, "8 核 3.2GHz") {
		t.Fatalf("no table row chunk: %q", rowText)
	}
	if !strings.Contains(pictureText, "图 1 系统架构图") {
		t.Fatalf("picture chunk text = %q", pictureText)
	}
	if _, err := os.Stat(staging + "/manifest.json"); err != nil {
		t.Fatalf("staging manifest missing: %v", err)
	}
}

// TestDoclingIntegrationHTML writes the rowspan/colspan/figcaption sample and
// runs it through the same entry point. HTML chunks must carry
// html_parser_version, never word_parser_version.
func TestDoclingIntegrationHTML(t *testing.T) {
	html := `<!DOCTYPE html><html lang="zh-CN"><head><meta charset="utf-8"><title>X7 网页手册</title></head>
<body>
<h1>X7 网页手册</h1><p>本页描述网页版的安装说明与硬件参数。</p>
<h2>硬件参数</h2>
<table border="1"><caption>表 1 硬件参数表</caption>
<tr><th>部件</th><th colspan="2">规格</th></tr>
<tr><td>处理器</td><td>8 核</td><td>3.2GHz</td></tr>
<tr><td rowspan="2">内存</td><td>32 GB</td><td>2</td></tr>
<tr><td>64 GB</td><td>1</td></tr></table>
<h2>架构图</h2>
<figure><img src="data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mP8z8BQDwAEhQGAhKmMIQAAAABJRU5ErkJggg==" alt="架构图" /><figcaption>图 1 系统架构图</figcaption></figure>
<p>图 1 展示了证据链路。</p>
<h2>外部资源</h2><p>外部图片：<img src="https://example.invalid/remote.png" alt="远程图片" /></p>
</body></html>`
	source := writeText(t, "manual.html", html)
	staging := t.TempDir()
	result, err := Prepare(context.Background(), source, doclingIntegrationOptions(t, "manual.html", staging))
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Metadata["parser_version"] != document.HTMLParserVersion {
		t.Fatalf("parser_version = %v", result.Metadata["parser_version"])
	}
	if result.Metadata["document_suffix"] != ".html" {
		t.Fatalf("document_suffix = %v", result.Metadata["document_suffix"])
	}
	for _, chunk := range result.Chunks {
		if _, exists := chunk.Metadata["word_parser_version"]; exists {
			t.Fatalf("HTML chunk was tagged as Word: %+v", chunk.Metadata)
		}
		if chunk.Metadata["html_parser_version"] != document.HTMLParserVersion {
			t.Fatalf("html_parser_version = %v", chunk.Metadata["html_parser_version"])
		}
	}
	var table *document.Asset
	var picture *document.Asset
	for index := range result.Assets {
		switch result.Assets[index].Kind {
		case "table":
			table = &result.Assets[index]
		case "picture":
			if picture == nil {
				picture = &result.Assets[index]
			}
		}
	}
	if table == nil || table.Table == nil {
		t.Fatalf("HTML table asset missing")
	}
	if table.Table.NumRows != 4 || table.Table.NumCols != 3 {
		t.Fatalf("HTML table shape = %dx%d", table.Table.NumRows, table.Table.NumCols)
	}
	var rowspan, colspan int
	for _, cell := range table.Table.Cells {
		if cell["text"] == "内存" {
			rowspan, _ = cell["row_span"].(int)
		}
		if cell["text"] == "规格" {
			colspan, _ = cell["col_span"].(int)
		}
	}
	if rowspan != 2 || colspan != 2 {
		t.Fatalf("HTML spans = rowspan %d colspan %d", rowspan, colspan)
	}
	if picture == nil || !strings.Contains(picture.Caption, "图 1 系统架构图") {
		t.Fatalf("HTML picture caption = %+v", picture)
	}
	if len(picture.Data) == 0 {
		t.Fatalf("HTML inline image was not decoded")
	}
	var rowText string
	var externalCaption bool
	for _, chunk := range result.Chunks {
		if strings.Contains(chunk.Text, "处理器") {
			rowText = chunk.Text
		}
		if chunk.Metadata["asset_kind"] == "picture" && strings.Contains(chunk.Text, "远程图片") {
			externalCaption = true
		}
	}
	// Header cells are prepended to every row chunk so a hit on "8 核" carries
	// the column meaning, and the caption is part of the row chunk too.
	for _, want := range []string{"表 1 硬件参数表", "部件", "处理器", "8 核"} {
		if !strings.Contains(rowText, want) {
			t.Fatalf("HTML table row chunk misses %q: %q", want, rowText)
		}
	}
	// The external image was not fetched, but its alt/caption stays searchable.
	if !externalCaption {
		t.Fatalf("external picture caption was not indexed: %+v", result.Chunks)
	}
}

// TestDoclingIntegrationLegacyDOC keeps the original .doc identity after the
// LibreOffice conversion uploads the converted DOCX to Docling.
func TestDoclingIntegrationLegacyDOC(t *testing.T) {
	body := headingParagraphXML("旧版 Word 手册", 1) + paragraphXML("转换后的正文仍然走 Docling。")
	converted := plainDocxPackage(t, body, true)
	source := writeText(t, "legacy.doc", "legacy doc bytes")
	staging := t.TempDir()
	options := doclingIntegrationOptions(t, "legacy.doc", staging)
	options.Word.Converter = &fakeDOCConverter{target: converted}
	result, err := Prepare(context.Background(), source, options)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if result.Metadata["document_suffix"] != ".doc" {
		t.Fatalf("document_suffix = %v, want .doc", result.Metadata["document_suffix"])
	}
	if result.Chunks[0].Source != "legacy.doc" {
		t.Fatalf("chunk source = %q", result.Chunks[0].Source)
	}
	for _, chunk := range result.Chunks {
		if chunk.Metadata["word_parser_version"] != document.DoclingWordParserVersion {
			t.Fatalf("legacy .doc parser = %v", chunk.Metadata["word_parser_version"])
		}
	}
}

// TestDoclingIntegrationServiceUnavailable verifies the failure does not fall
// back to the in-process Word parser and does not publish staging artifacts.
func TestDoclingIntegrationServiceUnavailable(t *testing.T) {
	doclingIntegrationParser(t)
	source := plainDocxPackage(t, paragraphXML("正文"), false)
	staging := t.TempDir()
	options := doclingIntegrationOptions(t, "manual.docx", staging)
	options.Docling.Parser = &document.DoclingServiceParser{BaseURL: "http://127.0.0.1:1"}
	_, err := Prepare(context.Background(), source, options)
	if err == nil {
		t.Fatal("Prepare() succeeded with an unreachable Docling service")
	}
	if !strings.Contains(err.Error(), "Docling 解析服务不可用") {
		t.Fatalf("error = %v, want a Docling service-unavailable message", err)
	}
	entries, readErr := os.ReadDir(staging)
	if readErr != nil {
		t.Fatalf("ReadDir(staging) error = %v", readErr)
	}
	if len(entries) != 0 {
		t.Fatalf("staging was populated after a failed parse: %v", entries)
	}
}
