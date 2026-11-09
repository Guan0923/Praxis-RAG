package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/document"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

func newTestStores(t *testing.T) *state.Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

func TestNewRequiresQdrantBackend(t *testing.T) {
	_, err := New(config.RagConfig{Backend: config.BackendMock}, newTestStores(t), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "RAG_BACKEND=qdrant") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewRequiresStateStore(t *testing.T) {
	_, err := New(config.RagConfig{Backend: config.BackendQdrant}, nil, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "shared state store") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewCreatesDataRootAndReportsStatus(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "data")
	service, err := New(config.RagConfig{
		Backend:    config.BackendQdrant,
		Collection: "mini_agent_knowledge_pdf_assets_v1",
	}, newTestStores(t), dataRoot)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = service.Close(context.Background()) }()

	if info, statErr := os.Stat(dataRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("data root was not created: %v", statErr)
	}
	status := service.Status(context.Background())
	if status["status"] != "ok" || status["service"] != "praxis-rag" {
		t.Fatalf("status = %v", status)
	}
	if status["backend"] != "qdrant" || status["collection"] != "mini_agent_knowledge_pdf_assets_v1" {
		t.Fatalf("status = %v", status)
	}
	statePayload, ok := status["state"].(map[string]any)
	if !ok || statePayload["backend"] != "mysql" {
		t.Fatalf("state = %v", status["state"])
	}
}

// insertReadableDocument stores one document record and (when operationID is
// set) its published head version, mirroring the publication join.
func insertReadableDocument(t *testing.T, stores *state.Stores, namespace, name, documentID, operationID string, chunking map[string]any) {
	t.Helper()
	ctx := context.Background()
	record := state.DocumentRecord{
		Name:       name,
		DocumentID: documentID,
		Size:       1,
		ModifiedAt: "2026-11-08T10:00:00.000000+00:00",
		IndexedAt:  "2026-11-08T10:00:00.000000+00:00",
		Chunking:   chunking,
		Collection: "knowledge_test",
	}
	if err := stores.Documents.Upsert(ctx, namespace, record); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if operationID != "" {
		if _, err := stores.DB.ExecContext(ctx,
			"INSERT INTO document_heads (namespace, document_id, version, updated_at) VALUES (?, ?, ?, ?)",
			namespace, documentID, operationID, 1.0); err != nil {
			t.Fatalf("document head insert error = %v", err)
		}
	}
}

// insertUploadObject stores one immutable upload-journal object (the versioned
// publication artifact).
func insertUploadObject(t *testing.T, stores *state.Stores, version, bucket, key string, content []byte) {
	t.Helper()
	if _, err := stores.DB.ExecContext(context.Background(),
		"INSERT INTO upload_objects (operation_id, bucket, `key`, content, metadata, created_at) VALUES (?, ?, ?, ?, NULL, ?)",
		version, bucket, key, content, 1.0); err != nil {
		t.Fatalf("upload object insert error = %v", err)
	}
}

func putSourceBlob(t *testing.T, stores *state.Stores, namespace, name string, content []byte) {
	t.Helper()
	if err := stores.Blobs.Put(context.Background(), namespace, state.SourceBucket, name, content, nil); err != nil {
		t.Fatalf("Blobs.Put() error = %v", err)
	}
}

func TestReadDocumentServesVersionedSavedMarkdown(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	operationID := strings.Repeat("a", 32)
	cases := []struct {
		name   string
		bucket string
	}{
		{"manual.pdf", state.PDFAssetBucket},
		{"manual.docx", state.WordAssetBucket},
	}
	for index, testCase := range cases {
		documentID := "doc-" + strings.ReplaceAll(testCase.name, ".", "-")
		version := operationID[:31] + string(rune('0'+index))
		insertReadableDocument(t, stores, "alpha", testCase.name, documentID, version, map[string]any{
			"pdf_parser_version":  document.PDFParserVersion,
			"word_parser_version": document.WordParserVersion,
		})
		insertUploadObject(t, stores, version, testCase.bucket, documentID+"/document.md",
			[]byte("# Saved "+testCase.name))
		// No source blob is staged: Python's versioned branch reads only the
		// upload journal object.
		text, gotVersion, err := service.ReadDocument(ctx, "alpha", testCase.name)
		if err != nil {
			t.Fatalf("ReadDocument(%s) error = %v", testCase.name, err)
		}
		if text != "# Saved "+testCase.name {
			t.Errorf("ReadDocument(%s) text = %q", testCase.name, text)
		}
		if gotVersion != "2026-11-08T10:00:00.000000+00:00" {
			t.Errorf("ReadDocument(%s) version = %q", testCase.name, gotVersion)
		}
	}
}

func TestReadDocumentMissingVersionedObject(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	insertReadableDocument(t, stores, "alpha", "manual.pdf", "doc-missing", strings.Repeat("b", 32), nil)
	putSourceBlob(t, stores, "alpha", "manual.pdf", []byte("%PDF-1.7"))

	_, _, err := service.ReadDocument(context.Background(), "alpha", "manual.pdf")
	assertKnowledgeValidation(t, err, "Saved content unavailable; reimport this document")
}

func TestReadDocumentUnversionedSavedArtifacts(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	// PDF fallback reads the blob asset bucket after the parser-version guard.
	pdfID := chunking.DocumentID("legacy.pdf")
	insertReadableDocument(t, stores, "alpha", "legacy.pdf", pdfID, "", map[string]any{
		"pdf_parser_version": document.PDFParserVersion,
	})
	if err := stores.Blobs.Put(ctx, "alpha", state.PDFAssetBucket, pdfID+"/document.md",
		[]byte("# Saved legacy PDF"), nil); err != nil {
		t.Fatalf("Blobs.Put(pdf) error = %v", err)
	}
	if text, _, err := service.ReadDocument(ctx, "alpha", "legacy.pdf"); err != nil || text != "# Saved legacy PDF" {
		t.Fatalf("legacy PDF read = %q, %v", text, err)
	}

	// A parser-version mismatch is a validation error, not a silent reparse.
	insertReadableDocument(t, stores, "alpha", "old.pdf", "doc-old-pdf", "", map[string]any{
		"pdf_parser_version": "docling-old",
	})
	_, _, err := service.ReadDocument(ctx, "alpha", "old.pdf")
	assertKnowledgeValidation(t, err, "PDF parser version changed; explicitly reimport this PDF")
}

func TestReadDocumentPlainTextValidation(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	putSourceBlob(t, stores, "alpha", "bad.txt", []byte{0xff, 0xfe})
	insertReadableDocument(t, stores, "alpha", "bad.txt", "doc-bad", "", nil)
	_, _, err := service.ReadDocument(ctx, "alpha", "bad.txt")
	assertKnowledgeValidation(t, err, "文本文件必须使用 UTF-8 编码：bad.txt")

	previous := document.MaxExtractedCharacters
	document.MaxExtractedCharacters = 5
	t.Cleanup(func() { document.MaxExtractedCharacters = previous })
	putSourceBlob(t, stores, "alpha", "big.txt", []byte("abcdef"))
	insertReadableDocument(t, stores, "alpha", "big.txt", "doc-big", "", nil)
	_, _, err = service.ReadDocument(ctx, "alpha", "big.txt")
	assertKnowledgeValidation(t, err, "文档提取文本过大：big.txt")
}

type stubDocumentViews struct {
	documents []map[string]any
	err       error
	calls     int
}

func (s *stubDocumentViews) Documents(context.Context, string) ([]map[string]any, error) {
	s.calls++
	if s.err != nil {
		return nil, s.err
	}
	return s.documents, nil
}

func TestListDocumentsUsesCachedReadViewsAndFallback(t *testing.T) {
	stores := newTestStores(t)
	service, err := New(config.RagConfig{Backend: config.BackendQdrant}, stores, t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = service.Close(context.Background()) }()
	ctx := context.Background()

	// The cached view is used when it succeeds (no repository read needed).
	cached := &stubDocumentViews{documents: []map[string]any{{"name": "cached.md"}}}
	service.views = cached
	documents, err := service.ListDocuments(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0]["name"] != "cached.md" || cached.calls != 1 {
		t.Fatalf("cached ListDocuments() = %+v, %v, calls=%d", documents, err, cached.calls)
	}

	// A failing view falls back to the authoritative repository.
	insertReadableDocument(t, stores, "alpha", "real.md", "doc-real", "", map[string]any{})
	failing := &stubDocumentViews{err: context.DeadlineExceeded}
	service.views = failing
	documents, err = service.ListDocuments(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0]["name"] != "real.md" || failing.calls != 1 {
		t.Fatalf("fallback ListDocuments() = %+v, %v, calls=%d", documents, err, failing.calls)
	}

	// No views configured (store without cache support) also falls back.
	service.views = nil
	documents, err = service.ListDocuments(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0]["name"] != "real.md" {
		t.Fatalf("authoritative ListDocuments() = %+v, %v", documents, err)
	}
}

func TestCheckDocumentsRejectsUnsupportedExtension(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx, testKnowledgeBaseSettings(t, "alpha", "http://127.0.0.1:1", "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	_, err := service.CheckDocuments(ctx, "alpha", []CheckFile{{Name: "bad.exe", SHA256: strings.Repeat("a", 64)}})
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("CheckDocuments() error = %v", err)
	}
	want := "暂不支持该文件类型：bad.exe；请选择 .doc、.docx、.md、.pdf、.txt。"
	if validation.Message != want {
		t.Fatalf("message = %q, want %q", validation.Message, want)
	}
	if SupportedUploadExtension("bad.exe") || !SupportedUploadExtension("a.MD") {
		t.Fatalf("supported-extension helper is wrong")
	}
	// A supported suffix reaches the duplicate probe instead of validation.
	payload, err := service.CheckDocuments(ctx, "alpha", []CheckFile{{Name: "ok.md", SHA256: strings.Repeat("b", 64)}})
	if err != nil {
		t.Fatalf("supported CheckDocuments() error = %v", err)
	}
	if files, ok := payload["files"].([]map[string]any); !ok || len(files) != 1 {
		t.Fatalf("payload = %+v", payload)
	}
}
