package document

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/logging"
)

// The remote vision PDF parse microservice exposes one fixed HTTP contract:
//
//	POST {BaseURL}/v1/parse   multipart/form-data, field "file"
//	200 application/zip        ZIP with manifest.json and referenced images
//
// The ZIP payload is untrusted input, so unpacking enforces the same bounds a
// document reader would: no absolute/traversal paths, no symlinks, no duplicate
// members and hard limits on member count and uncompressed size.
const (
	pdfServiceParsePath    = "/v1/parse"
	pdfServiceFileField    = "file"
	pdfServiceOptionsField = "options"
	pdfServiceManifestFile = "manifest.json"
)

// defaultPDFServiceTimeout bounds one parse call when the caller's context
// carries no deadline. The runtime normally applies RAG_PDF_SERVICE_TIMEOUT
// through the context as well.
const defaultPDFServiceTimeout = 15 * time.Minute

// Archive and response limits. They are variables so tests can tighten them,
// mirroring how the document extraction limits are exercised.
var (
	// pdfServiceResponseLimit bounds the ZIP response body (512 MiB).
	pdfServiceResponseLimit int64 = 512 << 20
	// pdfServiceArchiveMaxEntries bounds the number of archive members.
	pdfServiceArchiveMaxEntries = 512
	// pdfServiceArchiveEntryLimit bounds one uncompressed archive member.
	pdfServiceArchiveEntryLimit int64 = 512 << 20
	// pdfServiceArchiveTotalLimit bounds the combined uncompressed archive size.
	pdfServiceArchiveTotalLimit int64 = 512 << 20
)

// errPDFServiceResponseTooLarge reports a response body beyond
// pdfServiceResponseLimit so ParseInto can pick a dedicated message.
var errPDFServiceResponseTooLarge = errors.New("PDF parse service response exceeds the size limit")

// PDFServiceParser is the Go client of the remote vision PDF parse service.
// It implements ingest.PDFParser and document.PDFPages.
//
// ParseInto uploads the PDF, unpacks the returned ZIP into the staging
// directory (manifest.json plus the picture files referenced by image_file) and
// returns the validated manifest. Every failure is a *PDFParseError with a
// message that is safe to show to callers: it never quotes the response body.
//
// VLM is the per-knowledge-base vision configuration: layout, OCR, table and
// formula recognition and picture descriptions all run through the same remote
// endpoint. When it is configured ParseInto adds a text field "options" to the
// multipart request so the service uses those values instead of its own
// RAG_VLM_* environment. The options body carries the VLM API key, so it is
// never logged or quoted in an error message.
type PDFServiceParser struct {
	// BaseURL is the service base URL, for example http://127.0.0.1:8091.
	BaseURL string
	// Client overrides the HTTP client. nil uses a client without its own
	// timeout; the context (or the default safety deadline) still bounds the
	// call.
	Client *http.Client
	// VLM is the vision-language endpoint used for layout, OCR, table and
	// formula recognition and picture descriptions; Timeout/Concurrency come
	// from the service-level RAG_VLM_* parsing values.
	VLM VLMSettings
}

// ParseInto implements ingest.PDFParser against the remote vision parse service.
func (p *PDFServiceParser) ParseInto(ctx context.Context, source, staging string) (PDFManifest, error) {
	name := filepath.Base(source)
	if err := ValidatePDFFile(source, name); err != nil {
		return PDFManifest{}, &PDFParseError{Message: err.Error()}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if baseURL == "" {
		return PDFManifest{}, &PDFParseError{Message: "PDF 解析服务地址未配置"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultPDFServiceTimeout)
		defer cancel()
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return PDFManifest{}, &PDFParseError{Message: "PDF 解析失败：无法创建解析结果目录"}
	}
	options, err := p.optionsJSON()
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: "PDF 解析失败：解析选项无效"}
	}
	body, contentType, closeBody, err := pdfServiceMultipartBody(source, name, options)
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：无法读取 PDF 文件：%s", name)}
	}
	defer closeBody()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+pdfServiceParsePath, body)
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: "PDF 解析服务地址无效"}
	}
	request.Header.Set("Content-Type", contentType)
	// Forward the request correlation id so the parse service can join its
	// structured log lines to this backend request. It is optional: a
	// background parse without a request id sends the header unchanged.
	if requestID := logging.CorrelationID(ctx); requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		return PDFManifest{}, pdfServiceRequestError(ctx, err, name)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return PDFManifest{}, &PDFParseError{
			Message: fmt.Sprintf(
				"PDF 解析服务返回错误（HTTP %d）：%s", response.StatusCode, name),
			StatusCode: response.StatusCode,
		}
	}
	if err := checkPDFServiceResponseType(response.Header.Get("Content-Type")); err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析服务返回了非 ZIP 响应：%s", name)}
	}
	archive, closeArchive, err := pdfServiceArchiveFromBody(response.Body)
	if err != nil {
		if errors.Is(err, errPDFServiceResponseTooLarge) {
			return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析服务返回内容过大：%s", name)}
		}
		if contextErr, ok := pdfServiceContextError(ctx, err, name); ok {
			return PDFManifest{}, contextErr
		}
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析失败：解析服务返回的文件损坏：%s", name)}
	}
	defer closeArchive()
	if err := extractPDFServiceArchive(archive, staging); err != nil {
		return PDFManifest{}, &PDFParseError{Message: "PDF 解析失败：" + err.Error()}
	}
	manifest, err := readPDFServiceManifest(staging)
	if err != nil {
		return PDFManifest{}, &PDFParseError{Message: err.Error()}
	}
	if !IsSupportedPDFParserVersion(manifest.Version) {
		return PDFManifest{}, &PDFParseError{Message: "PDF parser version mismatch; reimport this document"}
	}
	if err := validatePDFServiceManifestImages(manifest, staging); err != nil {
		return PDFManifest{}, &PDFParseError{Message: "PDF 解析失败：解析服务返回的清单引用了缺失的图片文件"}
	}
	return manifest, nil
}

// Pages implements PDFPages by parsing into a temporary directory and grouping
// the manifest text blocks per page.
func (p *PDFServiceParser) Pages(ctx context.Context, source string) ([]Page, error) {
	if err := ValidatePDFFile(source, filepath.Base(source)); err != nil {
		return nil, err
	}
	staging, err := os.MkdirTemp("", "rag-pdf-service-pages-*")
	if err != nil {
		return nil, &PDFParseError{Message: "PDF 解析失败：无法创建解析结果目录"}
	}
	defer os.RemoveAll(staging)
	manifest, err := p.ParseInto(ctx, source, staging)
	if err != nil {
		return nil, err
	}
	return PagesFromManifest(manifest), nil
}

// pdfServiceContextError converts a context deadline or cancellation into a safe
// PDFParseError that still matches errors.Is. It reports false for other
// failures.
func pdfServiceContextError(ctx context.Context, cause error, name string) (*PDFParseError, bool) {
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded):
		return &PDFParseError{Message: fmt.Sprintf("PDF 解析超时：%s", name), cause: context.DeadlineExceeded}, true
	case errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, context.Canceled):
		return &PDFParseError{Message: fmt.Sprintf("PDF 解析已取消：%s", name), cause: context.Canceled}, true
	default:
		return nil, false
	}
}

// pdfServiceRequestError classifies a transport failure without exposing the
// underlying error text. Deadline and cancellation stay discoverable through
// errors.Is on the returned PDFParseError.
func pdfServiceRequestError(ctx context.Context, cause error, name string) error {
	if contextErr, ok := pdfServiceContextError(ctx, cause, name); ok {
		return contextErr
	}
	return &PDFParseError{Message: fmt.Sprintf("PDF 解析服务不可用：%s", name), cause: cause}
}

// pdfServiceParseOptions is the JSON document sent in the multipart "options"
// text field. Its shape is shared with the parse service contract
// (python-workers/pdf-service parser.parse_options_payload): the vision section
// is only emitted when the remote endpoint is configured.
type pdfServiceParseOptions struct {
	Vision *pdfServiceVisionOptions `json:"vision,omitempty"`
}

// pdfServiceVisionOptions mirrors a configured remote vision endpoint. Timeout
// is in seconds and omitted when the caller did not set one; the service
// applies its own default then.
type pdfServiceVisionOptions struct {
	BaseURL     string `json:"base_url"`
	APIKey      string `json:"api_key"`
	Model       string `json:"model"`
	Timeout     int    `json:"timeout,omitempty"`
	Concurrency int    `json:"concurrency,omitempty"`
}

// optionsJSON renders the multipart "options" payload. It returns nil when no
// vision endpoint is configured so the field is not sent at all (the service
// keeps its RAG_VLM_* environment defaults).
func (p *PDFServiceParser) optionsJSON() ([]byte, error) {
	if !p.VLM.Configured() {
		return nil, nil
	}
	options := pdfServiceParseOptions{
		Vision: &pdfServiceVisionOptions{
			BaseURL:     strings.TrimSpace(p.VLM.BaseURL),
			APIKey:      p.VLM.APIKey,
			Model:       strings.TrimSpace(p.VLM.Model),
			Timeout:     int(p.VLM.Timeout / time.Second),
			Concurrency: p.VLM.Concurrency,
		},
	}
	encoded, err := json.Marshal(options)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}

// pdfServiceMultipartBody streams the PDF as multipart/form-data without buffering
// the whole file. A non-empty options payload is written as the "options" text
// part before the file part. The returned close function stops the writer
// goroutine and releases the file; it is safe to call once after the request
// finishes.
func pdfServiceMultipartBody(source, name string, options []byte) (io.ReadCloser, string, func(), error) {
	file, err := os.Open(source)
	if err != nil {
		return nil, "", func() {}, err
	}
	reader, writer := io.Pipe()
	form := multipart.NewWriter(writer)
	contentType := form.FormDataContentType()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer file.Close()
		writeErr := func() error {
			if len(options) > 0 {
				if err := form.WriteField(pdfServiceOptionsField, string(options)); err != nil {
					return err
				}
			}
			part, err := form.CreateFormFile(pdfServiceFileField, name)
			if err != nil {
				return err
			}
			if _, err := io.Copy(part, file); err != nil {
				return err
			}
			return form.Close()
		}()
		_ = writer.CloseWithError(writeErr)
	}()
	closeBody := func() {
		_ = reader.Close()
		<-done
	}
	return reader, contentType, closeBody, nil
}

// checkPDFServiceResponseType accepts the ZIP content types the parse service may
// announce. An empty type is tolerated because the payload is validated as a
// ZIP anyway; anything else (for example an HTML error page with status 200) is
// rejected before reading the body.
func checkPDFServiceResponseType(raw string) error {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return err
	}
	switch mediaType {
	case "", "application/zip", "application/x-zip-compressed", "application/octet-stream":
		return nil
	default:
		return fmt.Errorf("unexpected PDF service response content type %q", mediaType)
	}
}

// pdfServiceArchiveFromBody spools the response body to a temporary file and opens
// it as a ZIP archive. Spooling keeps memory bounded for large documents while
// still giving zip.NewReader the random access it requires.
func pdfServiceArchiveFromBody(body io.Reader) (*zip.Reader, func(), error) {
	directory, err := os.MkdirTemp("", "rag-pdf-service-*")
	if err != nil {
		return nil, func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(directory) }
	archivePath := filepath.Join(directory, "response.zip")
	file, err := os.Create(archivePath)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	written, copyErr := io.Copy(file, io.LimitReader(body, pdfServiceResponseLimit+1))
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return nil, func() {}, copyErr
	}
	if closeErr != nil {
		cleanup()
		return nil, func() {}, closeErr
	}
	if written > pdfServiceResponseLimit {
		cleanup()
		return nil, func() {}, errPDFServiceResponseTooLarge
	}
	archive, err := zip.OpenReader(archivePath)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	closeArchive := func() {
		_ = archive.Close()
		_ = os.RemoveAll(directory)
	}
	return &archive.Reader, closeArchive, nil
}

// extractPDFServiceArchive writes the archive members into staging after the
// safety checks. Member errors are short, content-free phrases the caller
// prefixes with "PDF 解析失败：".
func extractPDFServiceArchive(archive *zip.Reader, staging string) error {
	if len(archive.File) > pdfServiceArchiveMaxEntries {
		return errors.New("解析服务返回的文件条目过多")
	}
	seen := make(map[string]struct{}, len(archive.File))
	manifestFound := false
	var total int64
	for _, member := range archive.File {
		relative, ok := safePDFServiceArchivePath(member.Name)
		if !ok {
			return errors.New("解析服务返回的文件包含不安全的路径")
		}
		key := strings.ToLower(relative)
		if _, exists := seen[key]; exists {
			return errors.New("解析服务返回的文件包含重复条目")
		}
		seen[key] = struct{}{}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if !withinDirectory(staging, target) {
			return errors.New("解析服务返回的文件包含不安全的路径")
		}
		mode := member.Mode()
		if mode&os.ModeSymlink != 0 {
			return errors.New("解析服务返回的文件包含符号链接")
		}
		if mode&os.ModeType != 0 && !mode.IsDir() {
			return errors.New("解析服务返回的文件包含不支持的条目类型")
		}
		if mode.IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return errors.New("解析结果无法写入")
			}
			continue
		}
		if member.UncompressedSize64 > uint64(pdfServiceArchiveEntryLimit) {
			return errors.New("解析服务返回的文件过大")
		}
		written, err := extractPDFServiceMember(member, target)
		if err != nil {
			return err
		}
		total += written
		if total > pdfServiceArchiveTotalLimit {
			return errors.New("解析服务返回的文件过大")
		}
		if relative == pdfServiceManifestFile {
			manifestFound = true
		}
	}
	if !manifestFound {
		return errors.New("解析服务返回的清单缺失")
	}
	return nil
}

// extractPDFServiceMember writes one regular archive member and returns the number
// of uncompressed bytes actually written.
func extractPDFServiceMember(member *zip.File, target string) (int64, error) {
	reader, err := member.Open()
	if err != nil {
		return 0, errors.New("解析服务返回的文件损坏")
	}
	defer reader.Close()
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return 0, errors.New("解析结果无法写入")
	}
	file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return 0, errors.New("解析结果无法写入")
	}
	written, copyErr := io.Copy(file, io.LimitReader(reader, pdfServiceArchiveEntryLimit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return written, errors.New("解析服务返回的文件损坏")
	}
	if closeErr != nil {
		return written, errors.New("解析结果无法写入")
	}
	if written > pdfServiceArchiveEntryLimit {
		return written, errors.New("解析服务返回的文件过大")
	}
	return written, nil
}

// safePDFServiceArchivePath normalizes one archive member name and rejects paths
// that could escape the staging directory or collide with Windows device
// syntax. The returned path is slash-separated and relative.
func safePDFServiceArchivePath(name string) (string, bool) {
	normalized := strings.ReplaceAll(name, "\\", "/")
	trimmed := strings.TrimSuffix(normalized, "/")
	if trimmed == "" || strings.HasPrefix(trimmed, "/") || path.IsAbs(trimmed) {
		return "", false
	}
	// A Windows drive-qualified path ("C:/...") is absolute even though the
	// path package does not treat it as such.
	if len(trimmed) >= 2 && trimmed[1] == ':' {
		return "", false
	}
	for _, part := range strings.Split(trimmed, "/") {
		if part == "" || part == "." || part == ".." {
			return "", false
		}
		for _, char := range part {
			if char < 0x20 || char == 0x7F || char == ':' {
				return "", false
			}
		}
	}
	cleaned := path.Clean(trimmed)
	if cleaned == "." || strings.HasPrefix(cleaned, "../") {
		return "", false
	}
	return cleaned, true
}

// withinDirectory reports whether target stays inside root after cleaning.
func withinDirectory(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(relative)
}

// readPDFServiceManifest reads and decodes the manifest.json written into staging.
func readPDFServiceManifest(staging string) (PDFManifest, error) {
	raw, err := os.ReadFile(filepath.Join(staging, pdfServiceManifestFile))
	if err != nil {
		return PDFManifest{}, errors.New("PDF 解析失败：解析服务返回的清单缺失")
	}
	var manifest PDFManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return PDFManifest{}, errors.New("PDF 解析失败：解析服务返回的清单无效")
	}
	return manifest, nil
}

// validatePDFServiceManifestImages verifies that every picture referenced by the
// manifest was actually written into staging, so downstream
// document.BuildPDFContent cannot fail on a missing asset after a successful
// parse.
func validatePDFServiceManifestImages(manifest PDFManifest, staging string) error {
	for _, asset := range manifest.Assets {
		if asset.ImageFile == "" {
			continue
		}
		relative, ok := safePDFServiceArchivePath(asset.ImageFile)
		if !ok {
			return fmt.Errorf("unsafe image path %q", asset.ImageFile)
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if !withinDirectory(staging, target) {
			return fmt.Errorf("unsafe image path %q", asset.ImageFile)
		}
		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("missing image %q", asset.ImageFile)
		}
	}
	return nil
}
