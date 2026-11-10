package document

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// PDFParserVersion mirrors pdf_content.PDF_PARSER_VERSION. Parsed PDF content
// is only reusable while the external parser reports this version.
const PDFParserVersion = "docling-formula-remote-v3"

// Page is one PDF page of plain text (1-based page number), the unit produced
// by document_readers._pdf_pages through pypdf.
type Page struct {
	Number int
	Text   string
}

// PDFPages extracts bounded per-page plain text for document text extraction
// and chunk_document parity. Implementations are the in-process native reader
// (NativePDFParser, the default) or the optional external Docling worker
// (ExternalPDFParser); tests inject doubles.
type PDFPages interface {
	Pages(ctx context.Context, source string) ([]Page, error)
}

// PDF manifest types. They mirror the JSON contract written by
// rag_mcp.pdf_worker (see pdf_worker._export) and consumed by
// pdf_assets.parse_pdf / pdf_content.render_content.
type (
	// PDFManifest is the worker manifest written through --manifest.
	PDFManifest struct {
		Version  string         `json:"version"`
		Blocks   []PDFBlock     `json:"blocks"`
		Assets   []PDFAsset     `json:"assets"`
		Warnings []PDFWarning   `json:"warnings"`
		Parser   map[string]any `json:"parser"`
		Metrics  map[string]any `json:"metrics"`
	}

	// PDFBlock is one ordered PDF content block.
	PDFBlock struct {
		Ref              string         `json:"ref"`
		Kind             string         `json:"kind"`
		Page             int            `json:"page"`
		Order            int            `json:"order"`
		Text             string         `json:"text"`
		AssetID          string         `json:"asset_id"`
		BBox             map[string]any `json:"bbox"`
		Locations        []PDFLocation  `json:"locations"`
		LaTeX            string         `json:"latex"`
		OriginalText     string         `json:"original_text"`
		EquationNumber   any            `json:"equation_number"`
		Association      string         `json:"association"`
		AssociatedAssets []string       `json:"associated_assets"`
	}

	// PDFLocation is one page/bbox provenance entry.
	PDFLocation struct {
		Page     int            `json:"page"`
		BBox     map[string]any `json:"bbox"`
		Charspan []int          `json:"charspan"`
	}

	// PDFAnnotation is a caption or footnote reference on an asset.
	PDFAnnotation struct {
		Ref       string         `json:"ref"`
		Text      string         `json:"text"`
		Page      int            `json:"page"`
		BBox      map[string]any `json:"bbox"`
		Locations []PDFLocation  `json:"locations"`
	}

	// PDFAsset is one picture or table asset in the manifest.
	PDFAsset struct {
		AssetID              string          `json:"asset_id"`
		Kind                 string          `json:"kind"`
		Page                 int             `json:"page"`
		BBox                 map[string]any  `json:"bbox"`
		Caption              string          `json:"caption"`
		CaptionSource        string          `json:"caption_source"`
		GeneratedDescription string          `json:"generated_description"`
		ImageFile            string          `json:"image_file"`
		Table                *PDFTable       `json:"table"`
		Captions             []PDFAnnotation `json:"captions"`
		Footnotes            []PDFAnnotation `json:"footnotes"`
	}

	// PDFTable is the parsed table structure exported by the worker.
	PDFTable struct {
		NumRows     int              `json:"num_rows"`
		NumCols     int              `json:"num_cols"`
		Orientation string           `json:"orientation"`
		Headers     []string         `json:"headers"`
		HeaderRows  [][]string       `json:"header_rows"`
		Rows        [][]string       `json:"rows"`
		Cells       []map[string]any `json:"cells"`
		Footnotes   []string         `json:"footnotes"`
	}

	// PDFWarning is one parser warning (for example low-confidence OCR).
	PDFWarning struct {
		Page    int    `json:"page"`
		Kind    string `json:"kind"`
		Count   int    `json:"count"`
		Message string `json:"message"`
	}
)

// TableRowText mirrors pdf_content.table_row_text and the Word row rendering in
// document_readers._docx_blocks.
func TableRowText(row []string) string {
	cells := make([]string, len(row))
	for index, cell := range row {
		cell = strings.ReplaceAll(cell, "\n", " ")
		cell = strings.ReplaceAll(cell, "|", `\|`)
		cells[index] = cell
	}
	return strings.Join(cells, " | ")
}

// RenderContent ports pdf_content.render_content: ordered manifest text with
// table rows flattened and picture placeholders, bounded by the extracted text
// size limit.
func RenderContent(manifest PDFManifest) (string, error) {
	assets := make(map[string]PDFAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		assets[asset.AssetID] = asset
	}
	parts := make([]string, 0, len(manifest.Blocks))
	for _, block := range manifest.Blocks {
		if block.AssetID != "" {
			asset, ok := assets[block.AssetID]
			if !ok {
				return "", fmt.Errorf("PDF manifest references an unknown asset: %s", block.AssetID)
			}
			if asset.Kind == "table" {
				if asset.Table == nil {
					return "", fmt.Errorf("PDF manifest table asset has no table data: %s", asset.AssetID)
				}
				rows := make([]string, len(asset.Table.Rows))
				for index, row := range asset.Table.Rows {
					rows[index] = TableRowText(row)
				}
				parts = append(parts, strings.Join(rows, "\n"))
			} else {
				parts = append(parts, "[Image: "+block.Ref+"]")
			}
			continue
		}
		if block.Text != "" {
			parts = append(parts, block.Text)
		}
	}
	text := strings.Join(parts, "\n\n")
	if utf8.RuneCountInString(text) > MaxExtractedCharacters {
		return "", fmt.Errorf("Parsed PDF exceeds the extracted text limit")
	}
	return text, nil
}

// pdfEncryptMarker matches an encryption dictionary reference or inline
// dictionary in the trailer/xref area. A bare "/Encrypt" scan would produce
// false positives for PDFs whose visible text mentions that token.
var pdfEncryptMarker = regexp.MustCompile(`/Encrypt\s*(?:\d+\s+\d+\s+R|<<|<)`)

// ValidatePDFFile performs the safe pre-checks that pypdf would provide before
// the external parser runs: a PDF header, no encryption dictionary and a
// readable file.
//
// Parity note: the encryption check is a trailer heuristic because Go does not
// parse PDF trailers in this phase. PDFs encrypted with an empty password (which
// pypdf can open) are rejected here; encrypted documents that hide the marker
// still fail safely through the external parser instead. Docling remains the
// authoritative parser for ingestion.
func ValidatePDFFile(path, name string) error {
	handle, err := os.Open(path)
	if err != nil {
		return readErrf("PDF 文件损坏或格式无效：%s", name)
	}
	defer handle.Close()
	info, err := handle.Stat()
	if err != nil || info.IsDir() {
		return readErrf("PDF 文件损坏或格式无效：%s", name)
	}
	header := make([]byte, 1024)
	read, _ := io.ReadFull(handle, header)
	if !bytes.Contains(header[:read], []byte("%PDF-")) {
		return readErrf("PDF 文件损坏或格式无效：%s", name)
	}
	tailSize := int64(8192)
	if info.Size() < tailSize {
		tailSize = info.Size()
	}
	tail := make([]byte, tailSize)
	if _, err := handle.ReadAt(tail, info.Size()-tailSize); err != nil && !errors.Is(err, io.EOF) {
		return readErrf("PDF 文件损坏或格式无效：%s", name)
	}
	if pdfEncryptMarker.Match(tail) {
		return readErrf("PDF 已加密，暂不支持需要密码的文件：%s", name)
	}
	return nil
}

// CommandRunner runs one external parser command. It is injectable so the
// worker protocol can be tested without a Python/Docling installation.
type CommandRunner interface {
	Run(ctx context.Context, name string, args []string, env map[string]string) error
}

// PDFParseError is a PDF parse failure whose message is safe to return to
// callers: it never quotes worker output, document text or credentials, so the
// upload API can show it instead of a bare error category.
type PDFParseError struct{ Message string }

// Error implements error.
func (e *PDFParseError) Error() string { return e.Message }

// commandFailure keeps the worker's stderr so ParseInto can classify a failure
// (for example a missing Python module) without exposing the raw output.
type commandFailure struct {
	err    error
	stderr string
}

// Error implements error.
func (e *commandFailure) Error() string { return e.err.Error() }

// Unwrap exposes the underlying process error.
func (e *commandFailure) Unwrap() error { return e.err }

// ExternalPDFParser is the Go client of the external Docling worker protocol
// defined by src/rag_mcp/pdf_assets.parse_pdf and src/rag_mcp/pdf_worker.py:
//
//	python -m rag_mcp.pdf_worker --input <pdf> --output <staging>
//	       --manifest <staging/manifest.json> [--models <models>]
//
// with PYTHONUTF8=1 and the Python package root on PYTHONPATH. The manifest is
// read back as PDFManifest and its version is checked before use.
type ExternalPDFParser struct {
	// Executable is the Python interpreter. Empty means "python".
	Executable string
	// Module is the worker module path. Empty means "rag_mcp.pdf_worker".
	Module string
	// PythonPath is prepended to PYTHONPATH so the worker package imports.
	PythonPath string
	// ModelsPath is passed as --models when set. It carries the local layout,
	// table and OCR weights; the VLM weights are not needed any more.
	ModelsPath string
	// VLM configures the remote vision-language endpoint used for formulas and
	// picture descriptions.
	VLM VLMSettings
	// Timeout bounds one worker run. Zero means 120 seconds.
	Timeout time.Duration
	// Runner overrides process execution for tests.
	Runner CommandRunner
}

// Parse runs the external worker in a temporary staging directory and returns
// the validated manifest.
func (p *ExternalPDFParser) Parse(ctx context.Context, source string) (PDFManifest, error) {
	staging, err := os.MkdirTemp("", "rag-pdf-*")
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF parser could not start: %v", err)}
	}
	defer os.RemoveAll(staging)
	return p.ParseInto(ctx, source, staging)
}

// ParseInto is Parse with a caller managed staging directory. The staging
// directory also receives the worker's document.json, ocr.json and images, so
// callers that persist PDF assets should pass their own workspace.
func (p *ExternalPDFParser) ParseInto(ctx context.Context, source, staging string) (PDFManifest, error) {
	name := filepath.Base(source)
	manifestPath := filepath.Join(staging, "manifest.json")
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF parser could not start: %v", err)}
	}
	executable := p.Executable
	if executable == "" {
		executable = "python"
	}
	module := p.Module
	if module == "" {
		module = "rag_mcp.pdf_worker"
	}
	args := []string{"-m", module, "--input", source, "--output", staging, "--manifest", manifestPath}
	if p.ModelsPath != "" {
		args = append(args, "--models", p.ModelsPath)
	}
	env := map[string]string{"PYTHONUTF8": "1"}
	p.VLM.mergeInto(env)
	if p.PythonPath != "" {
		existing := os.Getenv("PYTHONPATH")
		if existing != "" {
			env["PYTHONPATH"] = p.PythonPath + string(os.PathListSeparator) + existing
		} else {
			env["PYTHONPATH"] = p.PythonPath
		}
	}
	timeout := p.Timeout
	if timeout <= 0 {
		timeout = 120 * time.Second
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	runner := p.Runner
	if runner == nil {
		runner = execCommandRunner{}
	}
	if err := runner.Run(runCtx, executable, args, env); err != nil {
		switch {
		case errors.Is(runCtx.Err(), context.DeadlineExceeded):
			return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析超过 %d 秒：%s", int(timeout/time.Second), name)}
		case isExecStartError(err):
			return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF parser could not start: %s", "exec.Error")}
		default:
			if module := missingPythonModule(err); module != "" {
				return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf(
					"PDF 解析不可用：Python 解析环境缺少模块 %s；请安装 python-workers/requirements.txt 的依赖，并用 RAG_PDF_PARSER_PYTHON 与 PYTHONPATH 指向该环境（见 docs/USAGE.md）", module)}
			}
			return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：%s", name)}
		}
	}
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：%s", name)}
	}
	var manifest PDFManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：%s", name)}
	}
	if manifest.Version != PDFParserVersion {
		return PDFManifest{}, &PDFParseError{Message: "PDF parser version mismatch; reimport this document"}
	}
	return manifest, nil
}

// Pages implements PDFPages by running the worker and grouping text blocks by
// page number.
func (p *ExternalPDFParser) Pages(ctx context.Context, source string) ([]Page, error) {
	if err := ValidatePDFFile(source, filepath.Base(source)); err != nil {
		return nil, err
	}
	manifest, err := p.Parse(ctx, source)
	if err != nil {
		return nil, err
	}
	return PagesFromManifest(manifest), nil
}

// PagesFromManifest groups manifest text blocks into per-page plain text.
// Tables are flattened into their page and picture placeholders are skipped,
// because the worker keeps picture/table payloads on assets. This is an
// approximation of pypdf's page text; the Docling ingest path
// (RenderContent/pdf_content.content_chunks) remains authoritative.
func PagesFromManifest(manifest PDFManifest) []Page {
	assets := make(map[string]PDFAsset, len(manifest.Assets))
	for _, asset := range manifest.Assets {
		assets[asset.AssetID] = asset
	}
	grouped := make(map[int][]string)
	order := make([]int, 0, 8)
	seen := make(map[int]struct{}, 8)
	for _, block := range manifest.Blocks {
		if block.Page <= 0 {
			continue
		}
		text := block.Text
		if block.AssetID != "" {
			asset, ok := assets[block.AssetID]
			if !ok {
				continue
			}
			if asset.Kind != "table" || asset.Table == nil {
				continue
			}
			rows := make([]string, len(asset.Table.Rows))
			for index, row := range asset.Table.Rows {
				rows[index] = TableRowText(row)
			}
			text = strings.Join(rows, "\n")
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		if _, ok := seen[block.Page]; !ok {
			seen[block.Page] = struct{}{}
			order = append(order, block.Page)
		}
		grouped[block.Page] = append(grouped[block.Page], text)
	}
	sort.Ints(order)
	pages := make([]Page, 0, len(order))
	for _, number := range order {
		pages = append(pages, Page{Number: number, Text: strings.Join(grouped[number], "\n\n")})
	}
	return pages
}

// pdfPages mirrors document_readers._pdf_pages with the external parser in
// place of pypdf: page cap, accumulated text cap, OCR-required detection.
func (r *Reader) pdfPages(ctx context.Context, resolved, name string) ([]Page, error) {
	if err := ValidatePDFFile(resolved, name); err != nil {
		return nil, err
	}
	if r.PDF == nil {
		return nil, readErrf("PDF 文本解析需要外部 Docling 解析器（未配置 PDFPages）：%s", name)
	}
	pages, err := r.PDF.Pages(ctx, resolved)
	if err != nil {
		if _, ok := AsReadError(err); ok {
			return nil, err
		}
		return nil, &ReadError{Message: err.Error()}
	}
	if len(pages) > MaxPDFPages {
		return nil, readErrf("PDF 页数不能超过 %d 页：%s", MaxPDFPages, name)
	}
	result := make([]Page, 0, len(pages))
	extracted := 0
	for _, page := range pages {
		extracted += utf8.RuneCountInString(page.Text)
		if extracted > MaxExtractedCharacters {
			return nil, readErrf("文档提取文本过大：%s", name)
		}
		if strings.TrimSpace(page.Text) != "" {
			result = append(result, page)
		}
	}
	if len(result) == 0 {
		return nil, readErrf("PDF 未提取到文本，扫描件需要先进行 OCR：%s", name)
	}
	return result, nil
}

// execCommandRunner is the production CommandRunner.
type execCommandRunner struct{}

func (execCommandRunner) Run(ctx context.Context, name string, args []string, env map[string]string) error {
	command := exec.CommandContext(ctx, name, args...)
	command.Env = mergeEnvironment(env)
	command.Stdout = io.Discard
	var stderr bytes.Buffer
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		logExternalFailure(name, stderr.String())
		return &commandFailure{err: err, stderr: stderr.String()}
	}
	return nil
}

// logExternalFailure writes the parser's stderr tail to the service log. The
// text stays server-side: callers only ever observe PDFParseError messages.
func logExternalFailure(name, stderr string) {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return
	}
	const limit = 2000
	if len(stderr) > limit {
		stderr = stderr[len(stderr)-limit:]
	}
	log.Printf("PDF parser %s failed: %s", name, stderr)
}

// missingPythonModule extracts the module name from a Python
// "ModuleNotFoundError: No module named 'x'" traceback line, so a missing
// Docling environment can be reported as a configuration problem.
func missingPythonModule(err error) string {
	var failure *commandFailure
	if !errors.As(err, &failure) {
		return ""
	}
	const marker = "No module named "
	index := strings.Index(failure.stderr, marker)
	if index < 0 {
		return ""
	}
	rest := failure.stderr[index+len(marker):]
	for _, quote := range []byte{'\'', '"'} {
		if len(rest) > 0 && rest[0] == quote {
			rest = rest[1:]
			if end := strings.IndexByte(rest, quote); end >= 0 {
				return rest[:end]
			}
			return ""
		}
	}
	if end := strings.IndexAny(rest, " \r\n\t"); end >= 0 {
		return rest[:end]
	}
	return rest
}

// mergeEnvironment overlays extra variables on the process environment without
// duplicating keys.
func mergeEnvironment(extra map[string]string) []string {
	values := make(map[string]string, len(extra)+8)
	for _, entry := range os.Environ() {
		index := strings.Index(entry, "=")
		if index <= 0 {
			continue
		}
		values[entry[:index]] = entry[index+1:]
	}
	for key, value := range extra {
		values[key] = value
	}
	environment := make([]string, 0, len(values))
	for key, value := range values {
		environment = append(environment, key+"="+value)
	}
	return environment
}

func isExecStartError(err error) bool {
	var execError *exec.Error
	return errors.As(err, &execError)
}
