package document

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// PDF parser versions. PDFParserVersion identifies manifests written by the
// in-process NativePDFParser; PDFParserVersionDocling identifies manifests
// written by the remote Docling parse service. Readers accept both so imports
// from either engine stay ingestible.
const (
	// PDFParserVersion is the manifest contract of NativePDFParser.
	PDFParserVersion = "go-native-v1"
	// PDFParserVersionDocling is the manifest contract of the remote Docling
	// parse service (DoclingServiceParser).
	PDFParserVersionDocling = "docling-service-v1"
	// legacyPDFParserVersionDocling is the manifest version written by the
	// former external Docling worker. It is no longer produced but stays
	// readable so stored artifacts remain usable.
	legacyPDFParserVersionDocling = "docling-formula-remote-v3"
)

// IsSupportedPDFParserVersion reports whether version is a manifest contract
// this build can ingest. Foreign or future versions are rejected so callers
// reimport with a compatible parser.
func IsSupportedPDFParserVersion(version string) bool {
	switch version {
	case PDFParserVersion, PDFParserVersionDocling, legacyPDFParserVersionDocling:
		return true
	default:
		return false
	}
}

// PDFParserVersionForEngine maps a configured PDF engine name to the manifest
// version that engine writes. Empty and unknown engine names map to the
// in-process Go parser.
func PDFParserVersionForEngine(engine string) string {
	if strings.EqualFold(strings.TrimSpace(engine), "docling") {
		return PDFParserVersionDocling
	}
	return PDFParserVersion
}

// Page is one PDF page of plain text (1-based page number).
type Page struct {
	Number int
	Text   string
}

// PDFPages extracts bounded per-page plain text for document text extraction
// and chunk_document parity. NativePDFParser is the in-process implementation,
// DoclingServiceParser calls the remote parse service; tests inject doubles.
type PDFPages interface {
	Pages(ctx context.Context, source string) ([]Page, error)
}

// PDF manifest types. They are the JSON contract produced by NativePDFParser
// and consumed by RenderContent / pdfcontent. Assets and warnings are kept in
// the contract for stored manifests and future asset extraction; the Go parser
// currently emits text blocks only.
type (
	// PDFManifest is the parser manifest written to manifest.json.
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

	// PDFTable is a stored table structure asset.
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

	// PDFWarning is one parser warning (for example a page with no text).
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

// ValidatePDFFile performs the safe pre-checks before parsing: a PDF header, no
// encryption dictionary and a readable file.
//
// The encryption check is a trailer heuristic because this layer does not parse
// PDF trailers. PDFs encrypted with an empty password (which some readers can
// open) are rejected here.
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

// PDFParseError is a PDF parse failure whose message is safe to return to
// callers: it never quotes document text or credentials, so the upload API can
// show it instead of a bare error category.
type PDFParseError struct {
	Message string
	// cause keeps the classified underlying error (a context deadline or
	// cancel reason, or a transport failure) discoverable through errors.Is
	// without changing or extending the displayed message.
	cause error
}

// Error implements error.
func (e *PDFParseError) Error() string { return e.Message }

// Unwrap exposes the classified cause to errors.Is/errors.As. It never
// contributes to the message returned by Error.
func (e *PDFParseError) Unwrap() error { return e.cause }

// PagesFromManifest groups manifest text blocks into per-page plain text.
// Tables are flattened into their page and picture placeholders are skipped
// because picture/table payloads stay on assets. The Go parser emits text
// blocks only, but stored manifests may still contain such assets.
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

// pdfPages reads the per-page text through the configured PDFPages
// implementation: page cap, accumulated text cap, OCR-required detection.
func (r *Reader) pdfPages(ctx context.Context, resolved, name string) ([]Page, error) {
	if err := ValidatePDFFile(resolved, name); err != nil {
		return nil, err
	}
	if r.PDF == nil {
		return nil, readErrf("PDF 文本解析未配置解析器：%s", name)
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
