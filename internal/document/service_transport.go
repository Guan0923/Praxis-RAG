package document

import (
	"archive/zip"
	"context"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// This file holds the transport and archive handling shared by the remote
// parse-service clients (PDFServiceParser and DoclingServiceParser). Both
// contracts are the same on the wire: one multipart POST to a fixed path, a
// ZIP response bounded by hard limits, and member extraction that never lets a
// malicious archive escape the staging directory. Manifest versions and
// content interpretation stay in each client.
const serviceManifestFile = "manifest.json"

// errServiceResponseTooLarge reports a response body beyond the configured
// limit so a client can pick its own dedicated message.
var errServiceResponseTooLarge = errors.New("parse service response exceeds the size limit")

// serviceField is one text field of a multipart parse request. Fields are
// written before the file part, mirroring the PDF service's "options" field.
type serviceField struct {
	Name  string
	Value string
}

// serviceArchiveLimits bounds one untrusted service response.
type serviceArchiveLimits struct {
	// Response bounds the compressed ZIP body.
	Response int64
	// MaxEntries bounds the number of archive members.
	MaxEntries int
	// Entry bounds one uncompressed member.
	Entry int64
	// Total bounds the combined uncompressed size.
	Total int64
}

// serviceMultipartBody streams one file as multipart/form-data without
// buffering it. A non-empty options payload is written as a text part before
// the file part. The returned close function stops the writer goroutine and
// releases the file; it is safe to call once after the request finishes.
func serviceMultipartBody(source, name string, fields []serviceField) (io.ReadCloser, string, func(), error) {
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
			for _, field := range fields {
				if field.Value == "" {
					continue
				}
				if err := form.WriteField(field.Name, field.Value); err != nil {
					return err
				}
			}
			part, err := form.CreateFormFile(serviceFileField, name)
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

// serviceFileField is the multipart field carrying the uploaded document; both
// parse services use the same name.
const serviceFileField = "file"

// serviceZIPResponseType accepts the ZIP content types a parse service may
// announce. An empty type is tolerated because the payload is validated as a
// ZIP anyway; anything else (for example an HTML error page with status 200)
// is rejected before reading the body.
func serviceZIPResponseType(raw string) error {
	mediaType, _, err := mime.ParseMediaType(raw)
	if err != nil {
		return err
	}
	switch mediaType {
	case "", "application/zip", "application/x-zip-compressed", "application/octet-stream":
		return nil
	default:
		return errors.New("unexpected parse service response content type")
	}
}

// serviceArchiveFromBody spools the response body to a temporary file and opens
// it as a ZIP archive. Spooling keeps memory bounded for large documents while
// still giving zip.NewReader the random access it requires.
func serviceArchiveFromBody(body io.Reader, limit int64) (*zip.Reader, func(), error) {
	directory, err := os.MkdirTemp("", "rag-parse-service-*")
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
	written, copyErr := io.Copy(file, io.LimitReader(body, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		cleanup()
		return nil, func() {}, copyErr
	}
	if closeErr != nil {
		cleanup()
		return nil, func() {}, closeErr
	}
	if written > limit {
		cleanup()
		return nil, func() {}, errServiceResponseTooLarge
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

// extractServiceArchive writes the archive members into staging after the
// safety checks. Member errors are short, content-free phrases the caller
// prefixes with its own parse context.
func extractServiceArchive(archive *zip.Reader, staging string, limits serviceArchiveLimits) error {
	if len(archive.File) > limits.MaxEntries {
		return errors.New("解析服务返回的文件条目过多")
	}
	seen := make(map[string]struct{}, len(archive.File))
	manifestFound := false
	var total int64
	for _, member := range archive.File {
		relative, ok := serviceSafeArchivePath(member.Name)
		if !ok {
			return errors.New("解析服务返回的文件包含不安全的路径")
		}
		key := strings.ToLower(relative)
		if _, exists := seen[key]; exists {
			return errors.New("解析服务返回的文件包含重复条目")
		}
		seen[key] = struct{}{}
		target := filepath.Join(staging, filepath.FromSlash(relative))
		if !serviceWithinDirectory(staging, target) {
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
		if member.UncompressedSize64 > uint64(limits.Entry) {
			return errors.New("解析服务返回的文件过大")
		}
		written, err := extractServiceMember(member, target, limits.Entry)
		if err != nil {
			return err
		}
		total += written
		if total > limits.Total {
			return errors.New("解析服务返回的文件过大")
		}
		if relative == serviceManifestFile {
			manifestFound = true
		}
	}
	if !manifestFound {
		return errors.New("解析服务返回的清单缺失")
	}
	return nil
}

// extractServiceMember writes one regular archive member and returns the number
// of uncompressed bytes actually written.
func extractServiceMember(member *zip.File, target string, limit int64) (int64, error) {
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
	written, copyErr := io.Copy(file, io.LimitReader(reader, limit+1))
	closeErr := file.Close()
	if copyErr != nil {
		return written, errors.New("解析服务返回的文件损坏")
	}
	if closeErr != nil {
		return written, errors.New("解析结果无法写入")
	}
	if written > limit {
		return written, errors.New("解析服务返回的文件过大")
	}
	return written, nil
}

// serviceSafeArchivePath normalizes one archive member name and rejects paths
// that could escape the staging directory or collide with Windows device
// syntax. The returned path is slash-separated and relative.
func serviceSafeArchivePath(name string) (string, bool) {
	normalized := strings.ReplaceAll(name, `\`, "/")
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

// serviceWithinDirectory reports whether target stays inside root after
// cleaning.
func serviceWithinDirectory(root, target string) bool {
	relative, err := filepath.Rel(root, target)
	if err != nil {
		return false
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return false
	}
	return !filepath.IsAbs(relative)
}

// serviceContextFailure classifies a transport failure as a deadline or a
// cancellation so each client can build its own safe error. It reports false
// for other failures.
func serviceContextFailure(ctx context.Context, cause error) (timeout bool, canceled bool) {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) || errors.Is(cause, context.DeadlineExceeded) {
		return true, false
	}
	if errors.Is(ctx.Err(), context.Canceled) || errors.Is(cause, context.Canceled) {
		return false, true
	}
	return false, false
}
