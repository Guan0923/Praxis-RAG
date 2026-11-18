package document

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
)

// The Docling HTTP service exposes one fixed contract:
//
//	POST {BaseURL}/v1/parse   multipart/form-data, field "file"
//	200 application/zip        ZIP with manifest.json and referenced images
//
// The service parses static HTML and DOCX (legacy .doc is converted to DOCX by
// the caller first) and returns a versioned Word-shaped manifest: ordered
// blocks, typed picture/table assets with row/column spans and captions, plus
// the images referenced by image_file. The ZIP payload is untrusted input, so
// the shared service transport enforces the same extraction bounds as the PDF
// client.
const (
	doclingServiceParsePath = "/v1/parse"
	// defaultDoclingServiceTimeout bounds one parse call when the caller's
	// context carries no deadline.
	defaultDoclingServiceTimeout = 5 * time.Minute
	// doclingServiceMaxSourceBytes bounds the local upload handed to the
	// service (100 MiB); the HTTP layer already caps uploads at 10 MiB and this
	// only guards direct callers.
	doclingServiceMaxSourceBytes int64 = 100 << 20
)

// Archive and response limits, kept as variables so tests can tighten them.
var (
	// doclingServiceResponseLimit bounds the ZIP response body (512 MiB).
	doclingServiceResponseLimit int64 = 512 << 20
	// doclingServiceArchiveMaxEntries bounds the number of archive members.
	doclingServiceArchiveMaxEntries = 512
	// doclingServiceArchiveEntryLimit bounds one uncompressed archive member.
	doclingServiceArchiveEntryLimit int64 = 512 << 20
	// doclingServiceArchiveTotalLimit bounds the combined uncompressed size.
	doclingServiceArchiveTotalLimit int64 = 512 << 20
)

func doclingServiceArchiveLimits() serviceArchiveLimits {
	return serviceArchiveLimits{
		Response:   doclingServiceResponseLimit,
		MaxEntries: doclingServiceArchiveMaxEntries,
		Entry:      doclingServiceArchiveEntryLimit,
		Total:      doclingServiceArchiveTotalLimit,
	}
}

// ErrDoclingParseTimeout marks a parse that exceeded its execution window. The
// Docling service returns HTTP 504 after its conversion budget expires; a
// caller-side context deadline is treated the same way. It stays retryable,
// unlike a malformed file.
var ErrDoclingParseTimeout = errors.New("Docling parse timeout")

// DoclingParseError is a safe, displayable Docling parse failure. It never
// quotes the response body.
type DoclingParseError struct {
	Message string
	// StatusCode is the service HTTP status for a non-200 response and 0 for
	// local failures (validation, transport, timeout).
	StatusCode int
	// cause keeps the classified underlying error discoverable through
	// errors.Is without changing the displayed message.
	cause error
}

// Error implements error.
func (e *DoclingParseError) Error() string { return e.Message }

// ErrorCategory implements the optional logging classifier interface.
func (e *DoclingParseError) ErrorCategory() string { return "docling_parse_error" }

// HTTPStatus returns the parse service HTTP status, or 0 for local failures.
func (e *DoclingParseError) HTTPStatus() int { return e.StatusCode }

// Unwrap exposes the classified cause to errors.Is/errors.As.
func (e *DoclingParseError) Unwrap() error { return e.cause }

// DoclingServiceParser is the Go client of the independent Docling HTTP
// service. It implements ingest.DoclingParser.
//
// ParseInto uploads one DOCX/HTML file, unpacks the returned ZIP into the
// staging directory (manifest.json plus the pictures referenced by image_file)
// and returns the structured package. Every failure is a *DoclingParseError
// with a message safe to show to callers.
type DoclingServiceParser struct {
	// BaseURL is the service base URL, for example http://127.0.0.1:8092.
	BaseURL string
	// Client overrides the HTTP client. nil uses a client without its own
	// timeout; the context (or the default safety deadline) still bounds the
	// call.
	Client *http.Client
}

// ParseInto implements ingest.DoclingParser. sourcePath is the DOCX/HTML file
// on disk (for legacy .doc it is the LibreOffice-converted DOCX); sourceName is
// the original display name used in error messages and asset metadata.
func (p *DoclingServiceParser) ParseInto(ctx context.Context, sourcePath, sourceName, staging string) (*WordPackage, error) {
	expectedVersion, validateErr := validateDoclingSource(sourcePath, sourceName)
	if validateErr != nil {
		// Keep the invalid-document cause reachable so the worker classifies a
		// malformed file as terminal instead of retrying it.
		return nil, &DoclingParseError{Message: validateErr.Error(), cause: validateErr}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	if baseURL == "" {
		return nil, &DoclingParseError{Message: "Docling 解析服务地址未配置"}
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, defaultDoclingServiceTimeout)
		defer cancel()
	}
	if err := os.MkdirAll(staging, 0o755); err != nil {
		return nil, &DoclingParseError{Message: "Docling 解析失败：无法创建解析结果目录"}
	}
	// The multipart filename tells the service which format to parse; for
	// legacy .doc it must be the converted DOCX, not the original display name.
	body, contentType, closeBody, err := serviceMultipartBody(sourcePath, filepath.Base(sourcePath), nil)
	if err != nil {
		return nil, &DoclingParseError{Message: fmt.Sprintf("Docling 解析失败：无法读取文件：%s", sourceName)}
	}
	defer closeBody()
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+doclingServiceParsePath, body)
	if err != nil {
		return nil, &DoclingParseError{Message: "Docling 解析服务地址无效"}
	}
	request.Header.Set("Content-Type", contentType)
	if requestID := logging.CorrelationID(ctx); requestID != "" {
		request.Header.Set("X-Request-ID", requestID)
	}
	client := p.Client
	if client == nil {
		client = &http.Client{}
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, doclingServiceRequestError(ctx, err, sourceName)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		var cause error
		if response.StatusCode == http.StatusGatewayTimeout {
			cause = ErrDoclingParseTimeout
		}
		return nil, &DoclingParseError{
			Message:    fmt.Sprintf("Docling 解析服务返回错误（HTTP %d）：%s", response.StatusCode, sourceName),
			StatusCode: response.StatusCode,
			cause:      cause,
		}
	}
	if err := serviceZIPResponseType(response.Header.Get("Content-Type")); err != nil {
		return nil, &DoclingParseError{Message: fmt.Sprintf("Docling 解析服务返回了非 ZIP 响应：%s", sourceName)}
	}
	limits := doclingServiceArchiveLimits()
	archive, closeArchive, err := serviceArchiveFromBody(response.Body, limits.Response)
	if err != nil {
		if errors.Is(err, errServiceResponseTooLarge) {
			return nil, &DoclingParseError{Message: fmt.Sprintf("Docling 解析服务返回内容过大：%s", sourceName)}
		}
		if contextErr, ok := doclingServiceContextError(ctx, err, sourceName); ok {
			return nil, contextErr
		}
		return nil, &DoclingParseError{Message: fmt.Sprintf("Docling 解析失败：解析服务返回的文件损坏：%s", sourceName)}
	}
	defer closeArchive()
	if err := extractServiceArchive(archive, staging, limits); err != nil {
		return nil, &DoclingParseError{Message: "Docling 解析失败：" + err.Error()}
	}
	manifest, images, err := readDoclingServiceManifest(staging, expectedVersion)
	if err != nil {
		return nil, err
	}
	return &WordPackage{Manifest: manifest, Images: images}, nil
}

// validateDoclingSource validates the local input before uploading it and
// returns the manifest version the service must answer with.
func validateDoclingSource(sourcePath, sourceName string) (string, error) {
	name := sourceName
	if strings.TrimSpace(name) == "" {
		name = filepath.Base(sourcePath)
	}
	suffix := strings.ToLower(filepath.Ext(sourcePath))
	switch suffix {
	case SuffixDOCX:
		if err := validateDOCX(sourcePath, name); err != nil {
			return "", err
		}
		return DoclingWordParserVersion, nil
	case SuffixHTML, SuffixHTM:
		if err := validateHTMLSource(sourcePath, name); err != nil {
			return "", err
		}
		return HTMLParserVersion, nil
	default:
		return "", invalidDocumentErrf("Docling 解析不支持该文件类型：%s", name)
	}
}

// validateHTMLSource applies the local size and UTF-8 checks for a static HTML
// upload before the service sees it.
func validateHTMLSource(sourcePath, name string) *ReadError {
	info, err := os.Stat(sourcePath)
	if err != nil {
		return readErrf("无法读取 HTML 文件：%s", name)
	}
	if !info.Mode().IsRegular() {
		return invalidDocumentErrf("HTML 文件格式无效：%s", name)
	}
	if info.Size() <= 0 {
		return invalidDocumentErrf("HTML 文件为空：%s", name)
	}
	if info.Size() > doclingServiceMaxSourceBytes {
		return invalidDocumentErrf("HTML 文件超过服务端大小限制：%s", name)
	}
	file, err := os.Open(sourcePath)
	if err != nil {
		return readErrf("无法读取 HTML 文件：%s", name)
	}
	defer file.Close()
	// Read at most the configured source budget; invalid UTF-8 cannot be a
	// static HTML upload and would only make the service fail later.
	data, err := io.ReadAll(io.LimitReader(file, doclingServiceMaxSourceBytes+1))
	if err != nil {
		return readErrf("无法读取 HTML 文件：%s", name)
	}
	if int64(len(data)) > doclingServiceMaxSourceBytes {
		return invalidDocumentErrf("HTML 文件超过服务端大小限制：%s", name)
	}
	content := strings.TrimSpace(strings.TrimPrefix(string(data), "\ufeff"))
	if content == "" {
		return invalidDocumentErrf("HTML 文件为空：%s", name)
	}
	if !utf8.Valid(data) {
		return invalidDocumentErrf("HTML 文件不是有效的 UTF-8 文本：%s", name)
	}
	return nil
}

// readDoclingServiceManifest decodes manifest.json, enforces the contract
// version and loads every referenced picture into memory so downstream
// BuildWordContent cannot fail on a missing asset after a successful parse.
func readDoclingServiceManifest(staging, expectedVersion string) (WordManifest, map[string][]byte, error) {
	raw, err := os.ReadFile(filepath.Join(staging, serviceManifestFile))
	if err != nil {
		return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析失败：解析服务返回的清单缺失"}
	}
	var manifest WordManifest
	if err := json.Unmarshal(raw, &manifest); err != nil {
		return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析失败：解析服务返回的清单无效"}
	}
	if manifest.Version != expectedVersion {
		if !IsSupportedDoclingManifestVersion(manifest.Version) {
			return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析结果版本不受支持，请重新导入"}
		}
		return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析结果与文件格式不匹配：" + manifest.Version}
	}
	images := make(map[string][]byte, len(manifest.Assets))
	for index := range manifest.Assets {
		asset := manifest.Assets[index]
		if asset == nil || asset.ImageFile == nil {
			continue
		}
		relative, ok := serviceSafeArchivePath(*asset.ImageFile)
		if !ok {
			return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析失败：解析服务返回了不安全的图片路径"}
		}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if !serviceWithinDirectory(staging, target) {
			return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析失败：解析服务返回了不安全的图片路径"}
		}
		data, readErr := os.ReadFile(target)
		if readErr != nil || len(data) == 0 {
			return WordManifest{}, nil, &DoclingParseError{Message: "Docling 解析失败：解析服务返回的清单引用了缺失的图片文件"}
		}
		images[asset.AssetID] = data
	}
	if manifest.Blocks == nil {
		manifest.Blocks = []WordManifestBlock{}
	}
	if manifest.Assets == nil {
		manifest.Assets = []*WordManifestAsset{}
	}
	if manifest.Diagnostics == nil {
		manifest.Diagnostics = []WordDiagnostic{}
	}
	return manifest, images, nil
}

// doclingServiceContextError converts a context deadline or cancellation into a
// safe DoclingParseError that still matches errors.Is.
func doclingServiceContextError(ctx context.Context, cause error, name string) (*DoclingParseError, bool) {
	timeout, canceled := serviceContextFailure(ctx, cause)
	switch {
	case timeout:
		return &DoclingParseError{
			Message: fmt.Sprintf("Docling 解析超时：%s", name),
			cause:   errors.Join(ErrDoclingParseTimeout, context.DeadlineExceeded),
		}, true
	case canceled:
		return &DoclingParseError{Message: fmt.Sprintf("Docling 解析已取消：%s", name), cause: context.Canceled}, true
	default:
		return nil, false
	}
}

// doclingServiceRequestError classifies a transport failure without exposing
// the underlying error text.
func doclingServiceRequestError(ctx context.Context, cause error, name string) error {
	if contextErr, ok := doclingServiceContextError(ctx, cause, name); ok {
		return contextErr
	}
	return &DoclingParseError{Message: fmt.Sprintf("Docling 解析服务不可用：%s", name), cause: cause}
}
