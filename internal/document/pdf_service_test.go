package document

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// pdfServiceTestImage is the picture payload returned by the fake parse service.
const pdfServiceTestImage = "PNGDATA"

// pdfServiceManifestFixture is a minimal remote-service manifest with a
// text block, a formula block and one picture asset.
func pdfServiceManifestFixture(version string) PDFManifest {
	return PDFManifest{
		Version: version,
		Blocks: []PDFBlock{
			{Ref: "text-1", Kind: "text", Page: 1, Order: 0, Text: "First page paragraph."},
			{Ref: "formula-1", Kind: "formula", Page: 1, Order: 1, Text: "$$\n\\frac{a}{b}\n$$", LaTeX: `\frac{a}{b}`},
			{Ref: "#/pictures/0", Kind: "picture", Page: 2, Order: 2, AssetID: "#/pictures/0"},
		},
		Assets: []PDFAsset{{
			AssetID: "#/pictures/0", Kind: "picture", Page: 2, Caption: "Fig. 1",
			CaptionSource: "document", ImageFile: "pictures/picture-0.png",
		}},
		Warnings: []PDFWarning{},
		Parser:   map[string]any{"engine": "pdf-service"},
		Metrics:  map[string]any{"pages": 2},
	}
}

// pdfServiceZipMember is one entry of a synthetic service response archive.
type pdfServiceZipMember struct {
	name string
	data []byte
	mode os.FileMode
}

// buildPDFServiceArchive builds a ZIP from the given members, preserving order and
// allowing duplicate names (used by the duplicate-entry safety test).
func buildPDFServiceArchive(t *testing.T, members ...pdfServiceZipMember) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, member := range members {
		header := &zip.FileHeader{Name: member.name, Method: zip.Deflate}
		mode := member.mode
		if mode == 0 {
			mode = 0o644
		}
		header.SetMode(mode)
		handle, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("CreateHeader(%q) error = %v", member.name, err)
		}
		if _, err := handle.Write(member.data); err != nil {
			t.Fatalf("write %q error = %v", member.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("zip.Close() error = %v", err)
	}
	return buffer.Bytes()
}

// pdfServiceManifestArchive builds the normal manifest + image response archive.
func pdfServiceManifestArchive(t *testing.T, manifest PDFManifest, extra ...pdfServiceZipMember) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	members := append([]pdfServiceZipMember{{name: "manifest.json", data: raw}}, extra...)
	return buildPDFServiceArchive(t, members...)
}

// pdfServiceCall captures one multipart request received by the fake
// service.
type pdfServiceCall struct {
	path        string
	requestURI  string
	contentType string
	filename    string
	options     string
	body        []byte
}

// newPDFServiceServer starts a fake parse service that records the uploaded
// file (and the optional options text field) and then delegates the response to
// respond.
func newPDFServiceServer(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*httptest.Server, *pdfServiceCall) {
	t.Helper()
	call := &pdfServiceCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call.path = r.URL.Path
		call.requestURI = r.URL.RequestURI()
		call.contentType = r.Header.Get("Content-Type")
		file, header, err := r.FormFile(pdfServiceFileField)
		if err != nil {
			t.Errorf("parse request has no %q file part: %v", pdfServiceFileField, err)
			http.Error(w, "bad multipart request", http.StatusBadRequest)
			return
		}
		defer file.Close()
		call.filename = header.Filename
		call.options = r.FormValue(pdfServiceOptionsField)
		call.body, _ = io.ReadAll(file)
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, call
}

// writePDFServiceZip writes one successful ZIP response.
func writePDFServiceZip(w http.ResponseWriter, archive []byte) {
	w.Header().Set("Content-Type", "application/zip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive)
}

func TestPDFServiceParserParseInto(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	archive := pdfServiceManifestArchive(t, manifest,
		pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)})
	server, call := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
		writePDFServiceZip(w, archive)
	})
	source := writeValidPDF(t, "manual.pdf")
	staging := t.TempDir()
	// The trailing slash exercises base URL normalization.
	parser := &PDFServiceParser{BaseURL: server.URL + "/"}
	got, err := parser.ParseInto(context.Background(), source, staging)
	if err != nil {
		t.Fatalf("ParseInto() error = %v", err)
	}
	if call.path != pdfServiceParsePath {
		t.Fatalf("request path = %q, want %q", call.path, pdfServiceParsePath)
	}
	if !strings.HasPrefix(call.contentType, "multipart/form-data; boundary=") {
		t.Fatalf("request content type = %q", call.contentType)
	}
	if call.filename != "manual.pdf" {
		t.Fatalf("uploaded filename = %q", call.filename)
	}
	if string(call.body) != minimalPDF {
		t.Fatalf("uploaded PDF = %q", call.body)
	}
	// Without a VLM configuration the options field is omitted entirely so the
	// service keeps its own environment defaults.
	if call.options != "" {
		t.Fatalf("options field = %q, want none for an unconfigured parser", call.options)
	}
	if got.Version != PDFParserVersionService {
		t.Fatalf("manifest version = %q", got.Version)
	}
	if len(got.Blocks) != 3 || got.Blocks[1].Kind != "formula" || got.Blocks[1].LaTeX != `\frac{a}{b}` {
		t.Fatalf("manifest blocks = %+v", got.Blocks)
	}
	if len(got.Assets) != 1 || got.Assets[0].ImageFile != "pictures/picture-0.png" {
		t.Fatalf("manifest assets = %+v", got.Assets)
	}
	image, err := os.ReadFile(filepath.Join(staging, "pictures", "picture-0.png"))
	if err != nil || string(image) != pdfServiceTestImage {
		t.Fatalf("staged image = %q, err = %v", image, err)
	}
	raw, err := os.ReadFile(filepath.Join(staging, "manifest.json"))
	if err != nil {
		t.Fatalf("staged manifest.json error = %v", err)
	}
	var written PDFManifest
	if err := json.Unmarshal(raw, &written); err != nil {
		t.Fatalf("staged manifest.json is not valid JSON: %v", err)
	}
	if written.Version != PDFParserVersionService || len(written.Blocks) != 3 {
		t.Fatalf("staged manifest = %+v", written)
	}
}

func TestPDFServiceParserAcceptsLegacyVersion(t *testing.T) {
	manifest := pdfServiceManifestFixture("docling-formula-remote-v3")
	archive := pdfServiceManifestArchive(t, manifest,
		pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)})
	server, _ := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
		writePDFServiceZip(w, archive)
	})
	parser := &PDFServiceParser{BaseURL: server.URL}
	if _, err := parser.ParseInto(context.Background(), writeValidPDF(t, "legacy.pdf"), t.TempDir()); err != nil {
		t.Fatalf("ParseInto() error = %v", err)
	}
}

// TestPDFServiceParserSendsParseOptions pins the multipart "options" text
// field contract: the vision endpoint and credentials are sent as the single
// "vision" object, and the field is omitted unless a VLM is configured.
func TestPDFServiceParserSendsParseOptions(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	archive := pdfServiceManifestArchive(t, manifest,
		pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)})
	cases := []struct {
		name string
		vlm  VLMSettings
		want string
	}{
		{
			name: "vision",
			vlm: VLMSettings{
				BaseURL: "http://vlm:8000/v1", APIKey: "vlm-key", Model: "qwen2.5-vl-7b",
				Timeout: 120 * time.Second, Concurrency: 4,
			},
			want: `{"vision":{"base_url":"http://vlm:8000/v1","api_key":"vlm-key",` +
				`"model":"qwen2.5-vl-7b","timeout":120,"concurrency":4}}`,
		},
		{
			name: "vision_without_optional_fields",
			vlm:  VLMSettings{BaseURL: "http://vlm:8000/v1", Model: "qwen2.5-vl-7b"},
			want: `{"vision":{"base_url":"http://vlm:8000/v1","api_key":"","model":"qwen2.5-vl-7b"}}`,
		},
		{
			name: "unconfigured",
			want: "",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, call := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
				writePDFServiceZip(w, archive)
			})
			parser := &PDFServiceParser{
				BaseURL: server.URL,
				VLM:     testCase.vlm,
			}
			if _, err := parser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir()); err != nil {
				t.Fatalf("ParseInto() error = %v", err)
			}
			if call.options != testCase.want {
				t.Fatalf("options = %q, want %q", call.options, testCase.want)
			}
		})
	}

	// The file part is still uploaded when options are present and the API key
	// never appears in the request line.
	server, call := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
		writePDFServiceZip(w, archive)
	})
	parser := &PDFServiceParser{
		BaseURL: server.URL,
		VLM:     VLMSettings{BaseURL: "http://vlm:8000/v1", APIKey: "sk-super-secret", Model: "m"},
	}
	if _, err := parser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir()); err != nil {
		t.Fatalf("ParseInto() error = %v", err)
	}
	if string(call.body) != minimalPDF || call.filename != "manual.pdf" {
		t.Fatalf("uploaded file = %q (%q)", call.body, call.filename)
	}
	if strings.Contains(call.requestURI, "sk-super-secret") {
		t.Fatalf("request URI leaks the API key: %q", call.requestURI)
	}

	// A service failure must not quote the API key either.
	failing, _ := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom "+r.FormValue(pdfServiceOptionsField), http.StatusInternalServerError)
	})
	failingParser := &PDFServiceParser{
		BaseURL: failing.URL,
		VLM:     VLMSettings{BaseURL: "http://vlm:8000/v1", APIKey: "sk-super-secret", Model: "m"},
	}
	_, err := failingParser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir())
	if err == nil {
		t.Fatal("ParseInto() error = nil, want a service failure")
	}
	if strings.Contains(err.Error(), "sk-super-secret") {
		t.Fatalf("error message leaks the API key: %v", err)
	}
}
func TestPDFServiceParserAcceptedContentTypes(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	archive := pdfServiceManifestArchive(t, manifest,
		pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)})
	for _, contentType := range []string{"application/zip", "application/octet-stream", "application/x-zip-compressed"} {
		t.Run(contentType, func(t *testing.T) {
			server, _ := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(archive)
			})
			parser := &PDFServiceParser{BaseURL: server.URL}
			if _, err := parser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir()); err != nil {
				t.Fatalf("ParseInto() error = %v", err)
			}
		})
	}
}

func TestPDFServiceParserRejectsInvalidResponses(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	validImage := pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)}
	unsupported := pdfServiceManifestFixture("old-parser")
	cases := []struct {
		name         string
		respond      func(http.ResponseWriter, *http.Request)
		wantContains string
		wantAbsent   string
	}{
		{
			name: "http_error",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("top secret server detail"))
			},
			wantContains: "HTTP 500",
			wantAbsent:   "top secret",
		},
		{
			name: "html_with_status_200",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/html")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("<html>proxy error</html>"))
			},
			wantContains: "非 ZIP",
		},
		{
			name: "broken_zip",
			respond: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/zip")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte("this is not a zip archive"))
			},
			wantContains: "文件损坏",
		},
		{
			name: "missing_manifest",
			respond: func(w http.ResponseWriter, r *http.Request) {
				writePDFServiceZip(w, buildPDFServiceArchive(t, validImage))
			},
			wantContains: "清单缺失",
		},
		{
			name: "invalid_manifest",
			respond: func(w http.ResponseWriter, r *http.Request) {
				writePDFServiceZip(w, buildPDFServiceArchive(t,
					pdfServiceZipMember{name: "manifest.json", data: []byte(`{"version":`)},
					validImage))
			},
			wantContains: "清单无效",
		},
		{
			name: "unsupported_version",
			respond: func(w http.ResponseWriter, r *http.Request) {
				writePDFServiceZip(w, pdfServiceManifestArchive(t, unsupported, validImage))
			},
			wantContains: "PDF parser version mismatch",
		},
		{
			name: "missing_asset_image",
			respond: func(w http.ResponseWriter, r *http.Request) {
				writePDFServiceZip(w, pdfServiceManifestArchive(t, manifest))
			},
			wantContains: "图片文件",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newPDFServiceServer(t, testCase.respond)
			parser := &PDFServiceParser{BaseURL: server.URL}
			_, err := parser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir())
			var parseErr *PDFParseError
			if !errors.As(err, &parseErr) {
				t.Fatalf("error = %v, want *PDFParseError", err)
			}
			if !strings.Contains(parseErr.Message, testCase.wantContains) {
				t.Fatalf("message = %q, want substring %q", parseErr.Message, testCase.wantContains)
			}
			if testCase.wantAbsent != "" && strings.Contains(parseErr.Message, testCase.wantAbsent) {
				t.Fatalf("message leaked response body: %q", parseErr.Message)
			}
		})
	}
}

func TestPDFServiceParserRejectsUnsafeArchives(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	image := pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)}
	cases := []struct {
		name    string
		archive []byte
		want    string
	}{
		{
			name:    "absolute_path",
			archive: pdfServiceManifestArchive(t, manifest, image, pdfServiceZipMember{name: "/tmp/evil.png", data: []byte("x")}),
			want:    "不安全的路径",
		},
		{
			name:    "parent_traversal",
			archive: pdfServiceManifestArchive(t, manifest, image, pdfServiceZipMember{name: "../evil.png", data: []byte("x")}),
			want:    "不安全的路径",
		},
		{
			name:    "windows_traversal",
			archive: pdfServiceManifestArchive(t, manifest, image, pdfServiceZipMember{name: `..\evil.png`, data: []byte("x")}),
			want:    "不安全的路径",
		},
		{
			name: "symlink",
			archive: pdfServiceManifestArchive(t, manifest, image, pdfServiceZipMember{
				name: "pictures/link.png", data: []byte("manifest.json"), mode: os.ModeSymlink | 0o777,
			}),
			want: "符号链接",
		},
		{
			name:    "duplicate_entry",
			archive: pdfServiceManifestArchive(t, manifest, image, image),
			want:    "重复条目",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			server, _ := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
				writePDFServiceZip(w, testCase.archive)
			})
			parser := &PDFServiceParser{BaseURL: server.URL}
			_, err := parser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir())
			var parseErr *PDFParseError
			if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, testCase.want) {
				t.Fatalf("error = %v, want *PDFParseError containing %q", err, testCase.want)
			}
		})
	}
}

func TestPDFServiceParserArchiveLimits(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("json.Marshal(manifest) error = %v", err)
	}
	limit := int64(len(raw)) + 32
	image := pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)}

	run := func(t *testing.T, archive []byte, want string) {
		t.Helper()
		server, _ := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
			writePDFServiceZip(w, archive)
		})
		parser := &PDFServiceParser{BaseURL: server.URL}
		_, err := parser.ParseInto(context.Background(), writeValidPDF(t, "manual.pdf"), t.TempDir())
		var parseErr *PDFParseError
		if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, want) {
			t.Fatalf("error = %v, want *PDFParseError containing %q", err, want)
		}
	}

	t.Run("entry_count", func(t *testing.T) {
		overrideInt(t, &pdfServiceArchiveMaxEntries, 2)
		run(t, pdfServiceManifestArchive(t, manifest, image,
			pdfServiceZipMember{name: "extra.txt", data: []byte("x")}), "条目过多")
	})
	t.Run("entry_size", func(t *testing.T) {
		overrideInt64(t, &pdfServiceArchiveEntryLimit, limit)
		run(t, pdfServiceManifestArchive(t, manifest, pdfServiceZipMember{
			name: "pictures/picture-0.png", data: bytes.Repeat([]byte("x"), int(limit)+64),
		}), "文件过大")
	})
	t.Run("total_size", func(t *testing.T) {
		overrideInt64(t, &pdfServiceArchiveTotalLimit, limit)
		run(t, pdfServiceManifestArchive(t, manifest, pdfServiceZipMember{
			name: "pictures/picture-0.png", data: bytes.Repeat([]byte("x"), 128),
		}), "文件过大")
	})
	t.Run("response_size", func(t *testing.T) {
		overrideInt64(t, &pdfServiceResponseLimit, 32)
		run(t, pdfServiceManifestArchive(t, manifest, image), "返回内容过大")
	})
}

func TestPDFServiceParserTimeoutAndCancel(t *testing.T) {
	source := writeValidPDF(t, "manual.pdf")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Drain the upload so the server can notice the client going away, then
		// hold the response until the client deadline fires (with a hard bound
		// so server cleanup can never hang).
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(server.Close)
	parser := &PDFServiceParser{BaseURL: server.URL}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := parser.ParseInto(ctx, source, t.TempDir())
	var parseErr *PDFParseError
	if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, "超时") {
		t.Fatalf("timeout error = %v, want *PDFParseError containing 超时", err)
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout error = %v, want errors.Is(context.DeadlineExceeded)", err)
	}

	cancelled, cancelNow := context.WithCancel(context.Background())
	cancelNow()
	_, err = parser.ParseInto(cancelled, source, t.TempDir())
	if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, "已取消") {
		t.Fatalf("cancel error = %v, want *PDFParseError containing 已取消", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error = %v, want errors.Is(context.Canceled)", err)
	}

	// A response that stalls after sending its headers must also be reported as
	// a timeout rather than a damaged archive.
	stalled := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/zip")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(stalled.Close)
	stalledCtx, stalledCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer stalledCancel()
	_, err = (&PDFServiceParser{BaseURL: stalled.URL}).ParseInto(stalledCtx, source, t.TempDir())
	if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, "超时") ||
		!errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("stalled body error = %v, want timeout PDFParseError", err)
	}
}

func TestPDFServiceParserConnectionAndInputFailures(t *testing.T) {
	source := writeValidPDF(t, "manual.pdf")
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()

	_, err := (&PDFServiceParser{BaseURL: closedURL}).ParseInto(context.Background(), source, t.TempDir())
	var parseErr *PDFParseError
	if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, "不可用") {
		t.Fatalf("connection error = %v, want *PDFParseError containing 不可用", err)
	}
	if strings.Contains(parseErr.Message, "dial") {
		t.Fatalf("connection error leaked transport detail: %q", parseErr.Message)
	}

	if _, err := (&PDFServiceParser{}).ParseInto(context.Background(), source, t.TempDir()); err == nil {
		t.Fatalf("empty base URL was accepted")
	}

	broken := writeTemp(t, "broken.pdf", []byte("this is not a pdf"))
	if _, err := (&PDFServiceParser{BaseURL: "http://127.0.0.1:1"}).ParseInto(
		context.Background(), broken, t.TempDir()); !errors.As(err, &parseErr) ||
		!strings.Contains(parseErr.Message, "PDF 文件损坏或格式无效") {
		t.Fatalf("broken input error = %v", err)
	}
}

func TestPDFServiceParserPages(t *testing.T) {
	manifest := pdfServiceManifestFixture(PDFParserVersionService)
	archive := pdfServiceManifestArchive(t, manifest,
		pdfServiceZipMember{name: "pictures/picture-0.png", data: []byte(pdfServiceTestImage)})
	server, _ := newPDFServiceServer(t, func(w http.ResponseWriter, r *http.Request) {
		writePDFServiceZip(w, archive)
	})
	parser := &PDFServiceParser{BaseURL: server.URL}
	pages, err := parser.Pages(context.Background(), writeValidPDF(t, "manual.pdf"))
	if err != nil {
		t.Fatalf("Pages() error = %v", err)
	}
	if len(pages) != 1 || pages[0].Number != 1 {
		t.Fatalf("pages = %+v", pages)
	}
	if !strings.Contains(pages[0].Text, "First page paragraph.") ||
		!strings.Contains(pages[0].Text, `\frac{a}{b}`) {
		t.Fatalf("page text = %q", pages[0].Text)
	}
}

// TestPDFServiceParserLiveService calls a real service when
// RAG_PDF_SERVICE_URL is set; it is skipped in normal test runs.
func TestPDFServiceParserLiveService(t *testing.T) {
	baseURL := strings.TrimSpace(os.Getenv("RAG_PDF_SERVICE_URL"))
	if baseURL == "" {
		t.Skip("RAG_PDF_SERVICE_URL is not set")
	}
	source := writeTemp(t, "live.pdf", buildNativeTextPDF(t, []string{"PDF service live smoke test"}))
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	parser := &PDFServiceParser{BaseURL: baseURL}
	manifest, err := parser.ParseInto(ctx, source, t.TempDir())
	if err != nil {
		t.Fatalf("live ParseInto() error = %v", err)
	}
	if !IsSupportedPDFParserVersion(manifest.Version) {
		t.Fatalf("live manifest version = %q", manifest.Version)
	}
}
