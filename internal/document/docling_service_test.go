package document

import (
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
)

// doclingServiceManifestFixture is the minimal Word-shaped manifest the Docling
// service returns. It mirrors the real contract: one heading, one table and one
// picture asset whose image_file points into the ZIP.
func doclingServiceManifestFixture(version string) WordManifest {
	imageFile := "images/picture-1.png"
	return WordManifest{
		Version: version,
		Blocks: []WordManifestBlock{
			{Ref: "#/texts/0", Order: 0, Kind: KindHeading, Text: "Galaxy X7 系统手册",
				Location: WordLocation{Part: "body", Paragraph: 1, Headings: []string{"Galaxy X7 系统手册"}, Scope: "body", Section: 1}},
			{Ref: "#/tables/0", Order: 1, Kind: "table", AssetID: "table-1",
				Location: WordLocation{Part: "body", Paragraph: 2, Headings: []string{"Galaxy X7 系统手册"}, Scope: "body", Section: 1}},
			{Ref: "#/pictures/0", Order: 2, Kind: KindPicture, AssetID: "picture-1",
				Location: WordLocation{Part: "body", Paragraph: 3, Headings: []string{"Galaxy X7 系统手册"}, Scope: "body", Section: 1}},
		},
		Assets: []*WordManifestAsset{
			{AssetID: "table-1", Kind: "table", Caption: "表 1 硬件参数表", CaptionSource: "document",
				Table: &WordTableData{NumRows: 2, NumCols: 2, Rows: [][]string{{"部件", "规格"}, {"处理器", "8 核"}},
					HeaderRows: [][]string{{"部件", "规格"}}, Headers: []string{"部件", "规格"},
					Cells: []WordTableCellData{{Row: 0, Col: 0, RowSpan: 1, ColSpan: 1, Text: "部件"}}}},
			{AssetID: "picture-1", Kind: "picture", Caption: "图 1 系统架构图", CaptionSource: "document",
				ImageFile: &imageFile, MIMEType: stringPtr("image/png")},
		},
		Diagnostics:        []WordDiagnostic{},
		SkippedObjectCount: 0,
	}
}

func stringPtr(value string) *string { return &value }

type doclingServiceCall struct {
	path        string
	filename    string
	contentType string
	body        []byte
}

func newDoclingServiceServer(t *testing.T, respond func(http.ResponseWriter, *http.Request)) (*httptest.Server, *doclingServiceCall) {
	t.Helper()
	call := &doclingServiceCall{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call.path = r.URL.Path
		call.contentType = r.Header.Get("Content-Type")
		file, header, err := r.FormFile("file")
		if err != nil {
			t.Errorf("parse request has no file part: %v", err)
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		defer file.Close()
		call.filename = header.Filename
		call.body, _ = io.ReadAll(io.LimitReader(file, 1<<20))
		respond(w, r)
	}))
	t.Cleanup(server.Close)
	return server, call
}

func doclingServiceArchive(t *testing.T, manifest WordManifest, extra ...pdfServiceZipMember) []byte {
	t.Helper()
	raw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	members := append([]pdfServiceZipMember{{name: "manifest.json", data: raw}}, extra...)
	return buildPDFServiceArchive(t, members...)
}

func writeDoclingServiceZip(w http.ResponseWriter, archive []byte) {
	w.Header().Set("Content-Type", "application/zip")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(archive)
}

func TestDoclingServiceParserParseInto(t *testing.T) {
	manifest := doclingServiceManifestFixture(DoclingWordParserVersion)
	archive := doclingServiceArchive(t, manifest,
		pdfServiceZipMember{name: "images/picture-1.png", data: []byte("PNGDATA")})
	server, call := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeDoclingServiceZip(w, archive)
	})
	source := wordPicturePackage(t, pictureParagraph("rId1", "架构图"))
	staging := t.TempDir()
	parser := &DoclingServiceParser{BaseURL: server.URL + "/"}
	pkg, err := parser.ParseInto(context.Background(), source, "manual.docx", staging)
	if err != nil {
		t.Fatalf("ParseInto() error = %v", err)
	}
	if call.path != doclingServiceParsePath {
		t.Fatalf("request path = %q", call.path)
	}
	if call.filename != filepath.Base(source) {
		t.Fatalf("multipart filename = %q, want the uploaded path name", call.filename)
	}
	if !strings.HasPrefix(call.contentType, "multipart/form-data") {
		t.Fatalf("content type = %q", call.contentType)
	}
	if len(call.body) == 0 {
		t.Fatal("uploaded body is empty")
	}
	if pkg.Manifest.Version != DoclingWordParserVersion {
		t.Fatalf("manifest version = %q", pkg.Manifest.Version)
	}
	if len(pkg.Manifest.Assets) != 2 {
		t.Fatalf("assets = %d", len(pkg.Manifest.Assets))
	}
	if string(pkg.Images["picture-1"]) != "PNGDATA" {
		t.Fatalf("image bytes = %q", pkg.Images["picture-1"])
	}
	if _, err := os.Stat(filepath.Join(staging, "manifest.json")); err != nil {
		t.Fatalf("staging manifest missing: %v", err)
	}
	if data, err := os.ReadFile(filepath.Join(staging, "images", "picture-1.png")); err != nil || string(data) != "PNGDATA" {
		t.Fatalf("staging image = %q, %v", data, err)
	}
}

func TestDoclingServiceParserParsesHTML(t *testing.T) {
	manifest := doclingServiceManifestFixture(HTMLParserVersion)
	archive := doclingServiceArchive(t, manifest,
		pdfServiceZipMember{name: "images/picture-1.png", data: []byte("PNGDATA")})
	server, call := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeDoclingServiceZip(w, archive)
	})
	source := filepath.Join(t.TempDir(), "manual.html")
	if err := os.WriteFile(source, []byte("<!doctype html><html><body><p>正文</p></body></html>"), 0o644); err != nil {
		t.Fatal(err)
	}
	parser := &DoclingServiceParser{BaseURL: server.URL}
	pkg, err := parser.ParseInto(context.Background(), source, "manual.html", t.TempDir())
	if err != nil {
		t.Fatalf("ParseInto() error = %v", err)
	}
	if pkg.Manifest.Version != HTMLParserVersion {
		t.Fatalf("version = %q", pkg.Manifest.Version)
	}
	if call.filename != "manual.html" {
		t.Fatalf("multipart filename = %q", call.filename)
	}
}

func TestDoclingServiceParserRejectsMismatchedManifestVersions(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		version    string
		wantDetail string
	}{
		{name: "html_version_for_docx", version: HTMLParserVersion, wantDetail: "不匹配"},
		{name: "foreign_version", version: "docling-future-v9", wantDetail: "版本不受支持"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			manifest := doclingServiceManifestFixture(testCase.version)
			archive := doclingServiceArchive(t, manifest,
				pdfServiceZipMember{name: "images/picture-1.png", data: []byte("PNGDATA")})
			server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
				writeDoclingServiceZip(w, archive)
			})
			parser := &DoclingServiceParser{BaseURL: server.URL}
			_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
			var parseErr *DoclingParseError
			if !errors.As(err, &parseErr) || !strings.Contains(parseErr.Message, testCase.wantDetail) {
				t.Fatalf("error = %v, want %q", err, testCase.wantDetail)
			}
		})
	}
}

func TestDoclingServiceParserRejectsMissingImage(t *testing.T) {
	manifest := doclingServiceManifestFixture(DoclingWordParserVersion)
	archive := doclingServiceArchive(t, manifest)
	server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeDoclingServiceZip(w, archive)
	})
	parser := &DoclingServiceParser{BaseURL: server.URL}
	_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "缺失的图片文件") {
		t.Fatalf("error = %v", err)
	}
}

func TestDoclingServiceParserRejectsUnsafeArchivePaths(t *testing.T) {
	manifest := doclingServiceManifestFixture(DoclingWordParserVersion)
	archive := doclingServiceArchive(t, manifest,
		pdfServiceZipMember{name: "../escape.png", data: []byte("PNGDATA")})
	server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
		writeDoclingServiceZip(w, archive)
	})
	parser := &DoclingServiceParser{BaseURL: server.URL}
	_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "不安全的路径") {
		t.Fatalf("error = %v", err)
	}
}

func TestDoclingServiceParserHTTPFailures(t *testing.T) {
	t.Run("server_error", func(t *testing.T) {
		server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", http.StatusInternalServerError)
		})
		parser := &DoclingServiceParser{BaseURL: server.URL}
		_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
		var parseErr *DoclingParseError
		if !errors.As(err, &parseErr) || parseErr.StatusCode != http.StatusInternalServerError {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("gateway_timeout", func(t *testing.T) {
		server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "timeout", http.StatusGatewayTimeout)
		})
		parser := &DoclingServiceParser{BaseURL: server.URL}
		_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
		if !errors.Is(err, ErrDoclingParseTimeout) {
			t.Fatalf("error = %v, want ErrDoclingParseTimeout", err)
		}
	})
	t.Run("non_zip_response", func(t *testing.T) {
		server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html")
			_, _ = w.Write([]byte("<html>oops</html>"))
		})
		parser := &DoclingServiceParser{BaseURL: server.URL}
		_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "非 ZIP") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("unreachable", func(t *testing.T) {
		parser := &DoclingServiceParser{BaseURL: "http://127.0.0.1:1"}
		_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "不可用") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("missing_base_url", func(t *testing.T) {
		parser := &DoclingServiceParser{}
		_, err := parser.ParseInto(context.Background(), wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
		if err == nil || !strings.Contains(err.Error(), "地址未配置") {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestDoclingServiceParserRejectsInvalidLocalInput(t *testing.T) {
	parser := &DoclingServiceParser{BaseURL: "http://127.0.0.1:1"}
	t.Run("broken_docx", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "broken.docx")
		if err := os.WriteFile(source, []byte("not a zip"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := parser.ParseInto(context.Background(), source, "broken.docx", t.TempDir())
		if !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("error = %v, want ErrInvalidDocument", err)
		}
	})
	t.Run("invalid_utf8_html", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "broken.html")
		if err := os.WriteFile(source, []byte{0xff, 0xfe, '<', 'h', 't', 'm', 'l', '>'}, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := parser.ParseInto(context.Background(), source, "broken.html", t.TempDir())
		if !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("error = %v, want ErrInvalidDocument", err)
		}
	})
	t.Run("empty_html", func(t *testing.T) {
		source := filepath.Join(t.TempDir(), "empty.html")
		if err := os.WriteFile(source, []byte("   \n"), 0o644); err != nil {
			t.Fatal(err)
		}
		_, err := parser.ParseInto(context.Background(), source, "empty.html", t.TempDir())
		if !errors.Is(err, ErrInvalidDocument) {
			t.Fatalf("error = %v, want ErrInvalidDocument", err)
		}
	})
	t.Run("cancelled_context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		server, _ := newDoclingServiceServer(t, func(w http.ResponseWriter, _ *http.Request) {
			writeDoclingServiceZip(w, doclingServiceArchive(t, doclingServiceManifestFixture(DoclingWordParserVersion),
				pdfServiceZipMember{name: "images/picture-1.png", data: []byte("PNGDATA")}))
		})
		parser := &DoclingServiceParser{BaseURL: server.URL}
		_, err := parser.ParseInto(ctx, wordPicturePackage(t, pictureParagraph("rId1", "")), "manual.docx", t.TempDir())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("error = %v, want context.Canceled", err)
		}
	})
}
