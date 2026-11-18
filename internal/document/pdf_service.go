package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
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
var errPDFServiceResponseTooLarge = errServiceResponseTooLarge

// pdfServiceArchiveLimits returns the current PDF archive bounds. The limits
// stay variables so tests can tighten them.
func pdfServiceArchiveLimits() serviceArchiveLimits {
	return serviceArchiveLimits{
		Response:   pdfServiceResponseLimit,
		MaxEntries: pdfServiceArchiveMaxEntries,
		Entry:      pdfServiceArchiveEntryLimit,
		Total:      pdfServiceArchiveTotalLimit,
	}
}

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
		// Keep the read/validation cause reachable so the ingest worker can
		// classify an invalid header or encrypted file as terminal.
		return PDFManifest{}, &PDFParseError{Message: err.Error(), cause: err}
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
	body, contentType, closeBody, err := serviceMultipartBody(source, name,
		[]serviceField{{Name: pdfServiceOptionsField, Value: string(options)}})
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
		var cause error
		if response.StatusCode == http.StatusGatewayTimeout {
			// The service returns 504 only after the conversion itself exceeds the
			// per-attempt parse window; waiting for a slot does not consume it.
			cause = ErrPDFParseTimeout
		}
		return PDFManifest{}, &PDFParseError{
			Message: fmt.Sprintf(
				"PDF 解析服务返回错误（HTTP %d）：%s", response.StatusCode, name),
			StatusCode: response.StatusCode,
			cause:      cause,
		}
	}
	if err := serviceZIPResponseType(response.Header.Get("Content-Type")); err != nil {
		return PDFManifest{}, &PDFParseError{Message: fmt.Sprintf("PDF 解析服务返回了非 ZIP 响应：%s", name)}
	}
	limits := pdfServiceArchiveLimits()
	archive, closeArchive, err := serviceArchiveFromBody(response.Body, limits.Response)
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
	if err := extractServiceArchive(archive, staging, limits); err != nil {
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
		return &PDFParseError{
			Message: fmt.Sprintf("PDF 解析超时：%s", name),
			cause:   errors.Join(ErrPDFParseTimeout, context.DeadlineExceeded),
		}, true
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
		relative, ok := serviceSafeArchivePath(asset.ImageFile)
		if !ok {
			return fmt.Errorf("unsafe image path %q", asset.ImageFile)
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if !serviceWithinDirectory(staging, target) {
			return fmt.Errorf("unsafe image path %q", asset.ImageFile)
		}
		info, err := os.Stat(target)
		if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("missing image %q", asset.ImageFile)
		}
	}
	return nil
}
