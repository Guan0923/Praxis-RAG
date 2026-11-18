package runtime

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/document"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

const uploadDocumentText = "One document paragraph with enough text for chunking. " +
	"E03 水路供水不足的处理步骤需要排出管路空气。 " +
	"Another sentence keeps the chunk boundaries meaningful. "

// newUploadService builds a Service with a committed knowledge-base row, a fake
// Qdrant collection backend and a fake model API.
func newUploadService(t *testing.T) (*Service, *state.Stores, *fakeKBQdrant, *httpRecorder) {
	t.Helper()
	modelURL, modelCalls := newFakeModelAPI(t, kbModelHandler(nil))
	qdrantFake, qdrantURL := newFakeKBQdrant(t, "")
	service, stores := newKBService(t, qdrantURL)
	if err := stores.KnowledgeBases.Create(context.Background(),
		testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	return service, stores, qdrantFake, modelCalls
}

func repeatedDocumentText(repetitions int) string {
	var builder strings.Builder
	for index := 0; index < repetitions; index++ {
		builder.WriteString(uploadDocumentText)
		builder.WriteString(" \n\n")
	}
	return builder.String()
}

func uploadFile(name, content string) UploadFile {
	return UploadFile{Name: name, Content: []byte(content)}
}

func operationStatus(t *testing.T, stores *state.Stores, operationID string) *state.UploadOperation {
	t.Helper()
	operation, err := stores.Uploads.Get(context.Background(), operationID)
	if err != nil || operation == nil {
		t.Fatalf("Uploads.Get(%q) = %v, %v", operationID, operation, err)
	}
	return operation
}

func assertKnowledgeValidation(t *testing.T, err error, message string) {
	t.Helper()
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) {
		t.Fatalf("error = %v, want KnowledgeValidationError", err)
	}
	if message != "" && validation.Message != message {
		t.Fatalf("message = %q, want %q", validation.Message, message)
	}
}

func TestUploadMarkdownHappyPath(t *testing.T) {
	service, stores, qdrantFake, modelCalls := newUploadService(t)
	ctx := context.Background()
	content := repeatedDocumentText(6)

	payload, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", content)}, "")
	if err != nil {
		t.Fatalf("uploadDocuments() error = %v", err)
	}
	imported, _ := payload["imported"].([]map[string]any)
	skipped, _ := payload["skipped"].([]map[string]any)
	failed, _ := payload["failed"].([]map[string]any)
	if len(imported) != 1 || len(skipped) != 0 || len(failed) != 0 {
		t.Fatalf("imported/skipped/failed = %d/%d/%d (%v)", len(imported), len(skipped), len(failed), payload)
	}
	record := imported[0]
	if record["name"] != "guide.md" {
		t.Errorf("name = %v", record["name"])
	}
	operationID, _ := record["operation_id"].(string)
	if operationID == "" {
		t.Fatalf("operation_id missing: %v", record)
	}
	if record["sha256"] == "" || record["document_id"] == "" {
		t.Errorf("sha256/document_id = %v/%v", record["sha256"], record["document_id"])
	}
	chunkCount, _ := record["chunk_count"].(int)
	if chunkCount < 2 {
		t.Errorf("chunk_count = %v, want >= 2", record["chunk_count"])
	}
	if record["collection"] != "knowledge_test" {
		t.Errorf("collection = %v", record["collection"])
	}
	chunkingMetadata, _ := record["chunking"].(map[string]any)
	if chunkingMetadata["strategy"] != "character" || chunkingMetadata["chunk_size"] != 120 ||
		chunkingMetadata["embedding_model"] != "embed-a" {
		t.Errorf("chunking metadata = %v", chunkingMetadata)
	}
	for _, field := range []string{"modified_at", "indexed_at"} {
		if text, ok := record[field].(string); !ok || text == "" {
			t.Errorf("%s = %v", field, record[field])
		}
	}
	documents, _ := payload["documents"].([]map[string]any)
	if len(documents) != 1 || documents[0]["name"] != "guide.md" {
		t.Fatalf("documents = %v", payload["documents"])
	}

	stored, err := stores.Documents.Get(ctx, "alpha", "guide.md")
	if err != nil || stored == nil {
		t.Fatalf("Documents.Get() = %v, %v", stored, err)
	}
	if stored.SHA256 != record["sha256"] || stored.ChunkCount != chunkCount {
		t.Errorf("stored record = %+v", stored)
	}
	blob, found, err := stores.Blobs.Get(ctx, "alpha", state.SourceBucket, "guide.md")
	if err != nil || !found || string(blob) != content {
		t.Fatalf("source blob missing or different (found=%v err=%v)", found, err)
	}
	operation := operationStatus(t, stores, operationID)
	if operation.Status != state.UploadCommitted {
		t.Errorf("operation status = %q", operation.Status)
	}

	// Qdrant received one staged version with dense (3 dims) and sparse vectors.
	upserts := qdrantFake.find("PUT", "/points")
	if len(upserts) != 1 {
		t.Fatalf("Qdrant upserts = %d", len(upserts))
	}
	points, _ := upserts[0].Body["points"].([]any)
	if len(points) != chunkCount {
		t.Fatalf("staged points = %d, want %d", len(points), chunkCount)
	}
	first := points[0].(map[string]any)
	vector, _ := first["vector"].(map[string]any)
	dense, _ := vector["dense"].([]any)
	if len(dense) != 3 {
		t.Errorf("dense length = %d", len(dense))
	}
	if _, ok := vector["sparse"].(map[string]any); !ok {
		t.Errorf("sparse vector missing: %v", vector)
	}
	pointPayload, _ := first["payload"].(map[string]any)
	if pointPayload["record_type"] != "chunk" || pointPayload["upload_version"] != operationID {
		t.Errorf("point payload = %v", pointPayload)
	}
	if pointPayload["document_id"] == "" || pointPayload["chunk_id"] == "" || pointPayload["text"] == "" {
		t.Errorf("point payload fields = %v", pointPayload)
	}

	embeddings := modelCalls.find("POST", "/embeddings")
	if len(embeddings) != 1 {
		t.Fatalf("embedding calls = %d", len(embeddings))
	}
	if embeddings[0].Header.Get("Authorization") != "Bearer embedding-key" {
		t.Errorf("embedding Authorization = %q", embeddings[0].Header.Get("Authorization"))
	}
	inputs, _ := embeddings[0].Body["input"].([]any)
	if len(inputs) != chunkCount {
		t.Errorf("embedded texts = %d, want %d", len(inputs), chunkCount)
	}
}

func TestUploadDuplicateIsSkipped(t *testing.T) {
	service, stores, qdrantFake, _ := newUploadService(t)
	ctx := context.Background()
	content := repeatedDocumentText(4)
	first, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", content)}, "")
	if err != nil {
		t.Fatalf("first upload error = %v", err)
	}
	imported, _ := first["imported"].([]map[string]any)
	firstOperation := imported[0]["operation_id"]
	upsertsBefore := len(qdrantFake.find("PUT", "/points"))

	second, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", content)}, "")
	if err != nil {
		t.Fatalf("second upload error = %v", err)
	}
	skipped, _ := second["skipped"].([]map[string]any)
	imported, _ = second["imported"].([]map[string]any)
	if len(imported) != 0 || len(skipped) != 1 {
		t.Fatalf("second upload imported/skipped = %d/%d", len(imported), len(skipped))
	}
	entry := skipped[0]
	if entry["requested_name"] != "guide.md" || entry["operation_id"] == "" {
		t.Errorf("skipped entry = %v", entry)
	}
	if entry["request_sha256"] != imported0SHA(first) {
		t.Errorf("skipped request_sha256 = %v", entry["request_sha256"])
	}
	if _, popped := entry["skipped"]; popped {
		t.Errorf("skipped flag leaked into the public record")
	}
	documents, _ := stores.Documents.List(ctx, "alpha")
	if len(documents) != 1 {
		t.Fatalf("documents = %d", len(documents))
	}
	if len(qdrantFake.find("PUT", "/points")) != upsertsBefore {
		t.Errorf("duplicate upload staged new Qdrant points")
	}
	replay := operationStatus(t, stores, firstOperation.(string))
	if replay.Status != state.UploadCommitted {
		t.Errorf("first operation status = %q", replay.Status)
	}
}

func imported0SHA(payload map[string]any) any {
	imported, _ := payload["imported"].([]map[string]any)
	if len(imported) == 0 {
		return nil
	}
	return imported[0]["sha256"]
}

func TestUploadExistingNameCreatesNewVersion(t *testing.T) {
	service, stores, qdrantFake, _ := newUploadService(t)
	ctx := context.Background()
	if _, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", repeatedDocumentText(3))}, ""); err != nil {
		t.Fatalf("first upload error = %v", err)
	}
	newContent := repeatedDocumentText(2) + " changed content"
	payload, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", newContent)}, "")
	if err != nil {
		t.Fatalf("second upload error = %v", err)
	}
	imported, _ := payload["imported"].([]map[string]any)
	if len(imported) != 1 {
		t.Fatalf("imported = %v", payload["imported"])
	}
	newRecord := imported[0]
	if newRecord["sha256"] == "" {
		t.Fatalf("new record has no sha256")
	}
	documents, _ := stores.Documents.List(ctx, "alpha")
	if len(documents) != 1 || documents[0].SHA256 != newRecord["sha256"] {
		t.Fatalf("documents after overwrite = %+v", documents)
	}
	if len(qdrantFake.find("PUT", "/points")) != 2 {
		t.Errorf("each upload should stage one version")
	}
	blob, found, err := stores.Blobs.Get(ctx, "alpha", state.SourceBucket, "guide.md")
	if err != nil || !found || string(blob) != newContent {
		t.Errorf("source blob was not replaced (found=%v err=%v)", found, err)
	}
}

func TestUploadValidationErrors(t *testing.T) {
	service, _, _, _ := newUploadService(t)
	ctx := context.Background()
	if _, err := service.uploadDocuments(ctx, "alpha", nil, ""); err == nil {
		t.Fatalf("empty upload accepted")
	} else {
		assertKnowledgeValidation(t, err, "请选择要上传的 Markdown、TXT、PDF 或 DOCX 文件。")
	}
	tooMany := make([]UploadFile, maxFilesPerBatch+1)
	for index := range tooMany {
		tooMany[index] = uploadFile("file.txt", "text")
	}
	if _, err := service.uploadDocuments(ctx, "alpha", tooMany, ""); err == nil {
		t.Fatalf("too many files accepted")
	} else {
		assertKnowledgeValidation(t, err, "单次最多上传 10 个知识文件。")
	}
	if _, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("data.xyz", "text")}, ""); err == nil {
		t.Fatalf("unsupported extension accepted")
	} else {
		assertKnowledgeValidation(t, err, "暂不支持该文件类型：data.xyz；请选择 .doc、.docx、.htm、.html、.md、.pdf、.txt。")
	}
	missingKnowledgeBase := func() {
		if _, err := service.uploadDocuments(ctx, "missing", []UploadFile{uploadFile("a.txt", "text")}, ""); err == nil {
			t.Fatalf("upload to unknown namespace accepted")
		}
	}
	missingKnowledgeBase()

	// Oversized files are rejected per file with the Python size message.
	oversized := strings.Repeat("x", maxFileBytes+1)
	payload, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("big.txt", oversized)}, "")
	if err != nil {
		t.Fatalf("uploadDocuments() error = %v", err)
	}
	failed, _ := payload["failed"].([]map[string]any)
	if len(failed) != 1 || !strings.Contains(failed[0]["error"].(string), "单个知识文件不能超过 10 MiB：big.txt") {
		t.Fatalf("oversized failure = %v", failed)
	}
	if failed[0]["operation_id"] == "" {
		t.Errorf("failed entry missing operation_id: %v", failed[0])
	}
}

func TestUploadRejectsInvalidIdempotencyKey(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	// The REST batch hashes the caller key per file, so only the synchronous
	// write path can see an oversized raw key (Python validates 1..200 chars).
	longKey := strings.Repeat("k", 201)
	_, err := service.writeDocumentContent(context.Background(), "alpha", "note.txt",
		[]byte(repeatedDocumentText(2)), false, longKey)
	var knowledge *apierrors.KnowledgeError
	if !errors.As(err, &knowledge) || knowledge.Message != "Invalid idempotency key" {
		t.Fatalf("error = %v, want KnowledgeError(Invalid idempotency key)", err)
	}
	records, _ := stores.Documents.List(context.Background(), "alpha")
	if len(records) != 0 {
		t.Errorf("invalid idempotency key wrote a document")
	}
}

func TestUploadIdempotentReplay(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	ctx := context.Background()
	content := repeatedDocumentText(3)
	first, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", content)}, "batch-1")
	if err != nil {
		t.Fatalf("first upload error = %v", err)
	}
	imported, _ := first["imported"].([]map[string]any)
	firstOperation := imported[0]["operation_id"].(string)

	replay, err := service.uploadDocuments(ctx, "alpha", []UploadFile{uploadFile("guide.md", content)}, "batch-1")
	if err != nil {
		t.Fatalf("replay upload error = %v", err)
	}
	replayedRecords, _ := replay["imported"].([]map[string]any)
	if len(replayedRecords) != 1 || replayedRecords[0]["replayed"] != true {
		t.Fatalf("replayed record = %v", replay["imported"])
	}
	if replayedRecords[0]["operation_id"] != firstOperation {
		t.Errorf("replayed operation_id = %v, want %v", replayedRecords[0]["operation_id"], firstOperation)
	}
	documents, _ := stores.Documents.List(ctx, "alpha")
	if len(documents) != 1 {
		t.Fatalf("replayed upload created %d documents", len(documents))
	}

	// Same key with different bytes is a conflict, reported per file.
	conflict, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("guide.md", content+"different bytes")}, "batch-1")
	if err != nil {
		t.Fatalf("conflict upload error = %v", err)
	}
	failed, _ := conflict["failed"].([]map[string]any)
	if len(failed) != 1 || !strings.Contains(failed[0]["error"].(string), "UploadConflict") {
		t.Fatalf("conflicting replay failure = %v", failed)
	}
}

func TestUploadModelFailureRollsBack(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, unauthorizedModelHandler)
	qdrantFake, qdrantURL := newFakeKBQdrant(t, "")
	service, stores := newKBService(t, qdrantURL)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", modelURL, "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	payload, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("guide.md", repeatedDocumentText(3))}, "")
	if err != nil {
		t.Fatalf("uploadDocuments() error = %v", err)
	}
	failed, _ := payload["failed"].([]map[string]any)
	if len(failed) != 1 {
		t.Fatalf("failed = %v", failed)
	}
	if !strings.Contains(failed[0]["error"].(string), "HTTP 401") {
		t.Errorf("failure message = %v", failed[0]["error"])
	}
	operationID, _ := failed[0]["operation_id"].(string)
	if operationID == "" {
		t.Fatalf("operation_id missing")
	}
	operation := operationStatus(t, stores, operationID)
	if operation.Status == state.UploadCommitted || operation.Status == state.UploadRunning || operation.Status == state.UploadIntent {
		t.Errorf("failed operation status = %q", operation.Status)
	}
	documents, _ := stores.Documents.List(ctx, "alpha")
	if len(documents) != 0 {
		t.Errorf("failed upload left documents: %v", documents)
	}
	if len(qdrantFake.find("PUT", "/points")) != 0 {
		t.Errorf("failed upload staged Qdrant points")
	}
	if _, statErr := os.Stat(filepath.Join(os.TempDir(), "rag-txn-"+operationID)); !os.IsNotExist(statErr) {
		t.Errorf("failed upload left its workspace: %v", statErr)
	}
}

func TestUploadQdrantFailureCompensates(t *testing.T) {
	service, stores, qdrantFake, _ := newUploadService(t)
	qdrantFake.failUpsert = true
	ctx := context.Background()
	payload, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("guide.md", repeatedDocumentText(3))}, "")
	if err != nil {
		t.Fatalf("uploadDocuments() error = %v", err)
	}
	failed, _ := payload["failed"].([]map[string]any)
	if len(failed) != 1 || failed[0]["operation_id"] == "" {
		t.Fatalf("failed = %v", failed)
	}
	operation := operationStatus(t, stores, failed[0]["operation_id"].(string))
	if operation.Status == state.UploadCommitted {
		t.Errorf("operation was committed despite a Qdrant failure")
	}
	documents, _ := stores.Documents.List(ctx, "alpha")
	if len(documents) != 0 {
		t.Errorf("failed upload left documents: %v", documents)
	}
	// Version-scoped compensation deletes only the staged version.
	if len(qdrantFake.find("POST", "/points/delete")) == 0 {
		t.Errorf("Qdrant version compensation was not attempted")
	}
}

func TestDeleteDocument(t *testing.T) {
	service, stores, qdrantFake, _ := newUploadService(t)
	ctx := context.Background()
	if _, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("guide.md", repeatedDocumentText(3))}, ""); err != nil {
		t.Fatalf("upload error = %v", err)
	}
	deletesBefore := len(qdrantFake.find("POST", "/points/delete"))
	payload, err := service.deleteDocumentByName(ctx, "alpha", "guide.md")
	if err != nil {
		t.Fatalf("deleteDocumentByName() error = %v", err)
	}
	deleted, _ := payload["deleted"].(map[string]any)
	if deleted["name"] != "guide.md" {
		t.Errorf("deleted = %v", deleted)
	}
	documents, _ := payload["documents"].([]map[string]any)
	if len(documents) != 0 {
		t.Errorf("documents after delete = %v", documents)
	}
	if record, _ := stores.Documents.Get(ctx, "alpha", "guide.md"); record != nil {
		t.Errorf("document record still visible after delete")
	}
	if len(qdrantFake.find("POST", "/points/delete")) != deletesBefore {
		t.Errorf("logical delete must not touch Qdrant bytes")
	}

	if _, err := service.deleteDocumentByName(ctx, "alpha", "guide.md"); err == nil {
		t.Fatalf("second delete succeeded")
	} else {
		assertKnowledgeValidation(t, err, "知识文件不存在：guide.md")
	}

	if _, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("locked.txt", repeatedDocumentText(2))}, ""); err != nil {
		t.Fatalf("upload for lock test error = %v", err)
	}
	record, _ := stores.Documents.Get(ctx, "alpha", "locked.txt")
	if record == nil {
		t.Fatalf("locked document missing")
	}
	documentID := record.DocumentID
	token, err := stores.Uploads.AcquireLock(ctx, "alpha", &documentID)
	if err != nil {
		t.Fatalf("AcquireLock() error = %v", err)
	}
	defer func() { _ = stores.Uploads.ReleaseLock(ctx, token) }()
	if _, err := service.deleteDocumentByName(ctx, "alpha", "locked.txt"); err == nil {
		t.Fatalf("delete under lock succeeded")
	} else {
		assertKnowledgeValidation(t, err, "文档正在更新，请稍后再删除")
	}
}

func TestWriteDocumentCreateUpdateAndRules(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	ctx := context.Background()
	content := repeatedDocumentText(3)

	created, err := service.writeDocumentContent(ctx, "alpha", "note.md", []byte(content), false, "")
	if err != nil {
		t.Fatalf("create write error = %v", err)
	}
	if created["status"] != "added" {
		t.Errorf("create status = %v", created["status"])
	}
	if documents, _ := created["documents"].([]map[string]any); len(documents) != 1 {
		t.Errorf("created documents = %v", created["documents"])
	}

	updated, err := service.writeDocumentContent(ctx, "alpha", "note.md", []byte(content+" updated"), true, "")
	if err != nil {
		t.Fatalf("update write error = %v", err)
	}
	if updated["status"] != "updated" {
		t.Errorf("update status = %v", updated["status"])
	}
	unchanged, err := service.writeDocumentContent(ctx, "alpha", "note.md", []byte(content+" updated"), true, "")
	if err != nil {
		t.Fatalf("unchanged write error = %v", err)
	}
	if unchanged["status"] != "unchanged" {
		t.Errorf("unchanged status = %v", unchanged["status"])
	}

	if _, err := service.writeDocumentContent(ctx, "alpha", "missing.md", []byte(content), true, ""); err == nil {
		t.Fatalf("overwrite of a missing document succeeded")
	} else {
		assertKnowledgeValidation(t, err, "Document does not exist: missing.md")
	}
	if _, err := service.writeDocumentContent(ctx, "alpha", "note.md", []byte(content), false, ""); err == nil {
		t.Fatalf("duplicate create succeeded")
	} else {
		assertKnowledgeValidation(t, err, "Document already exists: note.md; use update_document")
	}
	if _, err := service.writeDocumentContent(ctx, "alpha", "bad:name.md", []byte(content), false, ""); err == nil {
		t.Fatalf("invalid name accepted")
	} else {
		assertKnowledgeValidation(t, err, "Invalid document name")
	}
	records, _ := stores.Documents.List(ctx, "alpha")
	if len(records) != 1 || records[0].Name != "note.md" {
		t.Errorf("documents = %+v", records)
	}
}

func TestUploadRecoversInterruptedOperation(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	ctx := context.Background()
	// Simulate a crashed worker: a short lease with no heartbeat that expires
	// before the next upload runs recovery.
	journal, err := state.NewUploadJournal(stores, state.WithLeaseSeconds(5.0), state.WithAutoHeartbeat(false))
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	documentID := chunking.StableID("doc", sparse.CaseFold("crashed.txt"))
	operation, err := journal.Begin(ctx, "alpha", "crashed.txt", documentID, "knowledge_test", state.BeginOptions{})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	workspace := filepath.Join(os.TempDir(), "rag-txn-"+operation.ID)
	if _, err := journal.RegisterWorkspace(ctx, operation, workspace, nil); err != nil {
		t.Fatalf("RegisterWorkspace() error = %v", err)
	}
	if err := journal.Intent(ctx, operation, "stage_qdrant", map[string]any{"version": operation.ID}); err != nil {
		t.Fatalf("Intent() error = %v", err)
	}
	time.Sleep(6 * time.Second)

	if _, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("guide.md", repeatedDocumentText(3))}, ""); err != nil {
		t.Fatalf("upload error = %v", err)
	}
	recovered := operationStatus(t, stores, operation.ID)
	if recovered.Status != state.UploadRolledBack && recovered.Status != state.UploadRollbackFailed {
		t.Errorf("interrupted operation status = %q, want rolled back", recovered.Status)
	}
	documents, _ := stores.Documents.List(ctx, "alpha")
	if len(documents) != 1 || documents[0].Name != "guide.md" {
		t.Errorf("documents after recovery = %+v", documents)
	}
	// The crashed operation's workspace is reclaimed by compensation.
	if _, statErr := os.Stat(workspace); !os.IsNotExist(statErr) {
		t.Errorf("recovered operation left its workspace: %v", statErr)
	}
}

// TestSafeUploadErrorTextPdfParseMessage pins that a PDF parse failure reaches
// the caller with its actionable message instead of a Go type name, and that
// plain Go errors still fall back to a Python-shaped category.
func TestSafeUploadErrorTextPdfParseMessage(t *testing.T) {
	message := "PDF 解析失败：文件损坏或格式不受支持；请检查 PDF 后重新上传"
	wrapped := &documentOperationError{
		Err:         &document.PDFParseError{Message: message},
		OperationID: "op",
	}
	if got := safeUploadErrorText(wrapped); got != message {
		t.Fatalf("safeUploadErrorText(PDFParseError) = %q, want %q", got, message)
	}
	plain := &documentOperationError{Err: errors.New("boom"), OperationID: "op"}
	if got := safeUploadErrorText(plain); got != "知识库操作失败：RuntimeError" {
		t.Fatalf("safeUploadErrorText(errors.New) = %q", got)
	}
}

// TestUploadStagedVersionIDsAreDeterministic pins the deterministic point id
// derivation used for staged versions (uuid5 over the operation id).
func TestUploadStagedVersionIDs(t *testing.T) {
	service, _, qdrantFake, _ := newUploadService(t)
	payload, err := service.uploadDocuments(context.Background(), "alpha",
		[]UploadFile{uploadFile("guide.md", repeatedDocumentText(2))}, "")
	if err != nil {
		t.Fatalf("upload error = %v", err)
	}
	imported, _ := payload["imported"].([]map[string]any)
	operationID := imported[0]["operation_id"].(string)
	upserts := qdrantFake.find("PUT", "/points")
	if len(upserts) == 0 {
		t.Fatalf("no Qdrant upsert")
	}
	points, _ := upserts[0].Body["points"].([]any)
	first := points[0].(map[string]any)
	id, _ := first["id"].(string)
	if len(id) != 36 || strings.Count(id, "-") != 4 {
		t.Errorf("point id = %q, want UUID", id)
	}
	if len(operationID) == 0 {
		t.Errorf("operation id missing")
	}
}

// TestPDFOptionsSelectsConfiguredEngine pins the runtime parser selection: the
// in-process pure-Go engine stays the default (nil Parser) and RAG_PDF_ENGINE
// vision builds the remote client from the configured URL and timeout.
func TestPDFOptionsSelectsConfiguredEngine(t *testing.T) {
	if options := (&Service{}).pdfOptions(nil); options.Parser != nil {
		t.Fatalf("default pdfOptions().Parser = %T, want nil for the in-process engine", options.Parser)
	}
	service := &Service{config: config.RagConfig{
		PDFEngine:         config.PDFEngineVision,
		PDFServiceURL:     "http://127.0.0.1:8091",
		PDFServiceTimeout: 900,
	}}
	parser, ok := service.pdfOptions(nil).Parser.(*document.PDFServiceParser)
	if !ok || parser == nil {
		t.Fatalf("pdfOptions().Parser = %T, want *document.PDFServiceParser", service.pdfOptions(nil).Parser)
	}
	if parser.BaseURL != "http://127.0.0.1:8091" {
		t.Fatalf("PDFServiceParser.BaseURL = %q", parser.BaseURL)
	}
	if parser.Client == nil || parser.Client.Timeout != 900*time.Second {
		t.Fatalf("PDFServiceParser.Client = %+v, want a 900s timeout", parser.Client)
	}
	if parser.VLM.Configured() {
		t.Fatalf("unconfigured pdfOptions() carried a VLM: %+v", parser)
	}
	// The legacy "docling" alias keeps selecting the remote parser.
	legacy := &Service{config: config.RagConfig{
		PDFEngine:         config.PDFEngineDocling,
		PDFServiceURL:     "http://127.0.0.1:8091",
		PDFServiceTimeout: 900,
	}}
	if _, ok := legacy.pdfOptions(nil).Parser.(*document.PDFServiceParser); !ok {
		t.Fatalf("legacy docling engine did not select the remote parser")
	}
}

// TestPDFOptionsCarriesVLM pins that the remote client receives the knowledge
// base's VLM endpoint so the parse service can use it per request (options
// multipart field) instead of its own environment.
func TestPDFOptionsCarriesVLM(t *testing.T) {
	service := &Service{config: config.RagConfig{
		PDFEngine:         config.PDFEngineVision,
		PDFServiceURL:     "http://127.0.0.1:8091",
		PDFServiceTimeout: 900,
	}}
	resources := &uploadResources{
		vlm: document.VLMSettings{
			BaseURL: "http://vlm:8000/v1", APIKey: "vlm-key", Model: "qwen2.5-vl-7b",
			Timeout: 120 * time.Second, Concurrency: 4,
		},
	}
	parser, ok := service.pdfOptions(resources).Parser.(*document.PDFServiceParser)
	if !ok || parser == nil {
		t.Fatalf("pdfOptions().Parser = %T", service.pdfOptions(resources).Parser)
	}
	if parser.VLM.BaseURL != "http://vlm:8000/v1" || parser.VLM.APIKey != "vlm-key" ||
		parser.VLM.Model != "qwen2.5-vl-7b" || parser.VLM.Timeout != 120*time.Second ||
		parser.VLM.Concurrency != 4 {
		t.Fatalf("parser VLM = %+v", parser.VLM)
	}
}

// TestResolveUploadResourcesCarriesVLM pins the knowledge-base → remote service
// configuration resolution used by both synchronous and queued uploads.
func TestResolveUploadResourcesCarriesVLM(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	ctx := context.Background()
	settings, err := stores.KnowledgeBases.Get(ctx, "alpha")
	if err != nil || settings == nil {
		t.Fatalf("KnowledgeBases.Get() = %v, %v", settings, err)
	}
	storedVLM, err := kbconfig.StoredConnectionWithKey(modelapi.ModelConnection{
		BaseURL: "http://vlm:8000/v1", APIKey: "vlm-key",
	}, testEncryptionKey)
	if err != nil {
		t.Fatalf("StoredConnectionWithKey(vlm) error = %v", err)
	}
	settings["vlm"] = storedVLM
	settings["vlm_model"] = "qwen2.5-vl-7b"
	if err := stores.KnowledgeBases.Update(ctx, settings); err != nil {
		t.Fatalf("KnowledgeBases.Update() error = %v", err)
	}

	resources, err := service.resolveUploadResources(ctx, "alpha")
	if err != nil {
		t.Fatalf("resolveUploadResources() error = %v", err)
	}
	if resources.vlm.BaseURL != "http://vlm:8000/v1" || resources.vlm.Model != "qwen2.5-vl-7b" ||
		resources.vlm.APIKey != "vlm-key" {
		t.Fatalf("resolved VLM = %+v", resources.vlm)
	}
	if !resources.vlm.Configured() {
		t.Fatalf("resolved VLM is not usable: %+v", resources)
	}
}

// TestValidateRemotePDFUploadRequiresVLM pins the acceptance-time check: a PDF
// upload under the remote vision engine is rejected before any journal/queue
// work unless a VLM (with a model) is configured.
func TestValidateRemotePDFUploadRequiresVLM(t *testing.T) {
	service := &Service{config: config.RagConfig{PDFEngine: config.PDFEngineVision}}
	pdf := []UploadFile{uploadFile("paper.pdf", "%PDF-1.4\n")}
	empty := &uploadResources{}

	assertKnowledgeValidation(t, service.validateRemotePDFUpload(pdf, empty), remotePDFModelsRequiredMessage)

	// Non-PDF uploads never need the parsing models.
	if err := service.validateRemotePDFUpload([]UploadFile{uploadFile("note.md", "text")}, empty); err != nil {
		t.Fatalf("markdown upload was rejected: %v", err)
	}

	// A VLM without a model id is not usable by the parse service.
	vlmOnly := &uploadResources{vlm: document.VLMSettings{BaseURL: "http://vlm:8000/v1"}}
	assertKnowledgeValidation(t, service.validateRemotePDFUpload(pdf, vlmOnly), remotePDFModelsRequiredMessage)

	configured := &uploadResources{
		vlm: document.VLMSettings{BaseURL: "http://vlm:8000/v1", Model: "qwen2.5-vl-7b"},
	}
	if err := service.validateRemotePDFUpload(pdf, configured); err != nil {
		t.Fatalf("configured upload was rejected: %v", err)
	}

	// The in-process Go engine never needs VLM configuration; the legacy
	// "docling" alias selects the same remote check.
	goService := &Service{config: config.RagConfig{PDFEngine: config.PDFEngineGo}}
	if err := goService.validateRemotePDFUpload(pdf, empty); err != nil {
		t.Fatalf("go engine upload was rejected: %v", err)
	}
	legacy := &Service{config: config.RagConfig{PDFEngine: config.PDFEngineDocling}}
	assertKnowledgeValidation(t, legacy.validateRemotePDFUpload(pdf, empty), remotePDFModelsRequiredMessage)
}

// TestUploadRejectsPDFWithoutVLM pins that the rejection happens while the
// upload is accepted: no journal operation or document is written.
func TestUploadRejectsPDFWithoutVLM(t *testing.T) {
	service, stores, qdrantFake, _ := newUploadService(t)
	service.config.PDFEngine = config.PDFEngineVision
	service.config.PDFServiceURL = "http://127.0.0.1:1"
	ctx := context.Background()

	_, err := service.uploadDocuments(ctx, "alpha",
		[]UploadFile{uploadFile("paper.pdf", "%PDF-1.4\n% fake pdf\n")}, "")
	assertKnowledgeValidation(t, err, remotePDFModelsRequiredMessage)

	var operations int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM upload_operations WHERE namespace = ?", "alpha").Scan(&operations); err != nil {
		t.Fatalf("count upload_operations error = %v", err)
	}
	if operations != 0 {
		t.Fatalf("upload journal has %d rows after a rejected upload", operations)
	}
	documents, err := stores.Documents.List(ctx, "alpha")
	if err != nil {
		t.Fatalf("Documents.List() error = %v", err)
	}
	if len(documents) != 0 {
		t.Fatalf("documents after a rejected upload = %+v", documents)
	}
	if puts := qdrantFake.find("PUT", "/points"); len(puts) != 0 {
		t.Fatalf("rejected upload wrote %d Qdrant upserts", len(puts))
	}
}

// TestIndexConfigurationRecordsPDFParserEngine pins the stored metadata version
// to the configured engine instead of one fixed constant.
func TestIndexConfigurationRecordsPDFParserEngine(t *testing.T) {
	cfg, err := chunking.NewConfig("character", 700, 100)
	if err != nil {
		t.Fatalf("chunking.NewConfig() error = %v", err)
	}
	for _, testCase := range []struct {
		engine string
		want   string
	}{
		{config.PDFEngineGo, document.PDFParserVersion},
		{config.PDFEngineVision, document.PDFParserVersionService},
		{config.PDFEngineDocling, document.PDFParserVersionService},
	} {
		metadata := indexConfiguration("paper.pdf", cfg, "BAAI/bge-m3", "bge-m3",
			document.PDFParserVersionForEngine(testCase.engine))
		if got := metadata["pdf_parser_version"]; got != testCase.want {
			t.Fatalf("engine %q pdf_parser_version = %v, want %q", testCase.engine, got, testCase.want)
		}
		if got := metadata["embedding_model"]; got != "bge-m3" {
			t.Fatalf("embedding_model = %v", got)
		}
	}
}

// TestIndexConfigurationRecordsDoclingParserVersions pins the stored parser
// contract after the Docling-only cutover: DOC/DOCX always record
// docling-word-v1 (Word parsing no longer exists outside the Docling
// service) and HTML always records docling-html-v1, so ReadDocument and
// duplicate detection pick the right asset bucket. Historical
// word-assets-v1 records stay readable through
// document.IsSupportedWordParserVersion.
func TestIndexConfigurationRecordsDoclingParserVersions(t *testing.T) {
	cfg, err := chunking.NewConfig("character", 700, 100)
	if err != nil {
		t.Fatalf("chunking.NewConfig() error = %v", err)
	}
	cases := []struct {
		name string
		key  string
		want string
	}{
		{name: "manual.docx", key: "word_parser_version", want: document.DoclingWordParserVersion},
		{name: "manual.doc", key: "word_parser_version", want: document.DoclingWordParserVersion},
		{name: "page.html", key: "html_parser_version", want: document.HTMLParserVersion},
		{name: "page.htm", key: "html_parser_version", want: document.HTMLParserVersion},
	}
	for _, testCase := range cases {
		metadata := indexConfiguration(testCase.name, cfg, "BAAI/bge-m3", "bge-m3",
			document.PDFParserVersion)
		if got := metadata[testCase.key]; got != testCase.want {
			t.Fatalf("%s %s = %v, want %q", testCase.name, testCase.key, got, testCase.want)
		}
	}
	if !document.IsSupportedWordParserVersion(document.WordParserVersion) {
		t.Fatalf("historical %s records must stay readable", document.WordParserVersion)
	}
}

// TestDoclingOptionsSelectsConfiguredService pins the runtime assembly: an empty
// RAG_DOCLING_SERVICE_URL keeps Docling.Parser nil (DOCX stays in-process) while
// a configured URL builds the HTTP client from the configured timeout.
func TestDoclingOptionsSelectsConfiguredService(t *testing.T) {
	if parser := (&Service{}).doclingOptions().Parser; parser != nil {
		t.Fatalf("default doclingOptions().Parser = %T, want nil", parser)
	}
	service := &Service{config: config.RagConfig{
		DoclingServiceURL:     "http://127.0.0.1:8092/",
		DoclingServiceTimeout: 300,
	}}
	parser, ok := service.doclingOptions().Parser.(*document.DoclingServiceParser)
	if !ok || parser == nil {
		t.Fatalf("doclingOptions().Parser = %T, want *document.DoclingServiceParser",
			service.doclingOptions().Parser)
	}
	if parser.BaseURL != "http://127.0.0.1:8092" {
		t.Fatalf("DoclingServiceParser.BaseURL = %q", parser.BaseURL)
	}
	if parser.Client == nil || parser.Client.Timeout != 300*time.Second {
		t.Fatalf("DoclingServiceParser.Client = %+v, want a 300s timeout", parser.Client)
	}
}

// TestValidateDoclingDocumentUploadRequiresService pins the admission-time
// check after the Docling-only cutover: Word (.doc/.docx) and HTML/HTM all
// need RAG_DOCLING_SERVICE_URL (there is no in-process Word parser any
// more), while the other formats are unaffected.
func TestValidateDoclingDocumentUploadRequiresService(t *testing.T) {
	service := &Service{}
	for _, name := range []string{"page.html", "page.HTM", "manual.docx", "manual.DOC"} {
		assertKnowledgeValidation(t, service.validateDoclingDocumentUpload([]UploadFile{uploadFile(name, "content")}),
			doclingServiceRequiredMessage)
	}
	for _, name := range []string{"note.md", "note.txt", "manual.pdf"} {
		if err := service.validateDoclingDocumentUpload([]UploadFile{uploadFile(name, "content")}); err != nil {
			t.Fatalf("%s rejected without Docling: %v", name, err)
		}
	}
	configured := &Service{config: config.RagConfig{DoclingServiceURL: "http://127.0.0.1:8092"}}
	for _, name := range []string{"page.html", "manual.docx", "manual.doc"} {
		if err := configured.validateDoclingDocumentUpload([]UploadFile{uploadFile(name, "content")}); err != nil {
			t.Fatalf("configured %s upload rejected: %v", name, err)
		}
	}
}

// TestUploadRejectsDoclingOnlyFormatsWithoutService pins that the rejection
// happens while the upload is accepted: no journal operation or document is
// written. Word and HTML are both rejected the same way now.
func TestUploadRejectsDoclingOnlyFormatsWithoutService(t *testing.T) {
	service, stores, qdrantFake, _ := newUploadService(t)
	ctx := context.Background()

	for _, name := range []string{"page.html", "manual.docx", "legacy.doc"} {
		_, err := service.uploadDocuments(ctx, "alpha",
			[]UploadFile{uploadFile(name, "content")}, "")
		assertKnowledgeValidation(t, err, doclingServiceRequiredMessage)
	}

	var operations int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM upload_operations WHERE namespace = ?", "alpha").Scan(&operations); err != nil {
		t.Fatalf("count upload_operations error = %v", err)
	}
	if operations != 0 {
		t.Fatalf("upload journal has %d rows after a rejected upload", operations)
	}
	documents, err := stores.Documents.List(ctx, "alpha")
	if err != nil {
		t.Fatalf("Documents.List() error = %v", err)
	}
	if len(documents) != 0 {
		t.Fatalf("documents after a rejected upload = %+v", documents)
	}
	if puts := qdrantFake.find("PUT", "/points"); len(puts) != 0 {
		t.Fatalf("rejected upload wrote %d Qdrant upserts", len(puts))
	}
}
