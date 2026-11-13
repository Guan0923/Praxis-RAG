package runtime

import (
	"context"
	"errors"
	"regexp"
	"strings"
	"sync"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

var canonicalUUIDPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// fakeBroker implements BackgroundPublisher (both notification seams) and can
// run an inline search executor to simulate the separsearch worker.
type fakeBroker struct {
	mu sync.Mutex

	searchEnvelopes []state.SearchEnvelope
	searchErr       error
	onSearch        func(state.SearchEnvelope) error

	documentNotifications []state.DocumentNotification
	documentErr           error
}

func (f *fakeBroker) SendSearchEnvelope(_ context.Context, envelope state.SearchEnvelope) error {
	f.mu.Lock()
	f.searchEnvelopes = append(f.searchEnvelopes, envelope)
	handler := f.onSearch
	sendErr := f.searchErr
	f.mu.Unlock()
	if handler != nil {
		return handler(envelope)
	}
	return sendErr
}

func (f *fakeBroker) SendDocumentNotification(_ context.Context, notification state.DocumentNotification) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.documentNotifications = append(f.documentNotifications, notification)
	return f.documentErr
}

func (f *fakeBroker) Close() error { return nil }

func (f *fakeBroker) searchEnvelopeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.searchEnvelopes)
}

func (f *fakeBroker) lastSearchEnvelope() state.SearchEnvelope {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.searchEnvelopes) == 0 {
		return state.SearchEnvelope{}
	}
	return f.searchEnvelopes[len(f.searchEnvelopes)-1]
}

func (f *fakeBroker) documentNotificationCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.documentNotifications)
}

// memoryTransient is an in-memory state.TransientStore for result snapshots.
type memoryTransient struct {
	mu     sync.Mutex
	values map[string]any
}

func newMemoryTransient() *memoryTransient {
	return &memoryTransient{values: map[string]any{}}
}

func transientKey(kind, identity string) string { return kind + ":" + identity }

func (m *memoryTransient) Get(kind, identity string) (any, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, found := m.values[transientKey(kind, identity)]
	return value, found, nil
}

func (m *memoryTransient) Put(kind, identity string, value any, _ float64, nx bool) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := transientKey(kind, identity)
	if _, exists := m.values[key]; exists && nx {
		return false, nil
	}
	m.values[key] = value
	return true, nil
}

func (m *memoryTransient) Delete(kind, identity string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := transientKey(kind, identity)
	if _, exists := m.values[key]; !exists {
		return 0, nil
	}
	delete(m.values, key)
	return 1, nil
}

// newAsyncService builds a runtime with a committed knowledge-base row, an
// in-memory result snapshot store and a fake broker wired through the real
// background dispatchers.
func newAsyncService(t *testing.T) (*Service, *state.Stores, *fakeBroker) {
	t.Helper()
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "alpha", "http://127.0.0.1:1", "knowledge_test")); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	stores.Results.Transient = newMemoryTransient()
	broker := &fakeBroker{}
	if err := service.StartBackground(ctx,
		WithBackgroundPublisher(broker),
		WithMaintenanceInterval(0),
		WithInitialMaintenancePass(false),
	); err != nil {
		t.Fatalf("StartBackground() error = %v", err)
	}
	t.Cleanup(service.StopBackground)
	return service, stores, broker
}

func TestAddDocumentAsyncAdmissionReplayAndConflicts(t *testing.T) {
	service, stores, broker := newAsyncService(t)
	ctx := context.Background()
	content := []byte("# Manual\n\nWater supply procedure.")

	payload, err := service.addDocumentAsync(ctx, "alpha", "manual.md", content, "key-1")
	if err != nil {
		t.Fatalf("addDocumentAsync() error = %v", err)
	}
	artifactID, _ := payload["artifact_id"].(string)
	if !canonicalUUIDPattern.MatchString(artifactID) {
		t.Fatalf("artifact_id = %v, want canonical UUID", payload["artifact_id"])
	}
	if len(payload) != 1 {
		t.Errorf("admission payload = %v, want only artifact_id", payload)
	}
	if broker.documentNotificationCount() != 1 {
		t.Errorf("published document notifications = %d", broker.documentNotificationCount())
	}
	broker.mu.Lock()
	notification := broker.documentNotifications[0]
	broker.mu.Unlock()
	if notification.ArtifactID != artifactID || notification.Namespace != "alpha" || notification.Name != "manual.md" {
		t.Errorf("notification = %+v", notification)
	}

	// Same idempotency key and bytes replays the same artifact.
	replay, err := service.addDocumentAsync(ctx, "alpha", "manual.md", content, "key-1")
	if err != nil {
		t.Fatalf("replayed addDocumentAsync() error = %v", err)
	}
	if replay["artifact_id"] != artifactID {
		t.Errorf("replayed artifact_id = %v, want %v", replay["artifact_id"], artifactID)
	}
	// Same key with different bytes is a conflict.
	_, err = service.addDocumentAsync(ctx, "alpha", "manual.md", []byte("different"), "key-1")
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != asyncUploadConflictMessage {
		t.Fatalf("conflicting replay error = %v", err)
	}
	// A queued filename with another key is a conflict as well.
	_, err = service.addDocumentAsync(ctx, "alpha", "manual.md", content, "key-2")
	if !errors.As(err, &validation) || validation.Message != asyncUploadConflictMessage {
		t.Fatalf("queued-name conflict error = %v", err)
	}

	job, err := stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	if job.Status != state.DocumentJobQueued {
		t.Errorf("job status = %q", job.Status)
	}
	// A published document with the same name also conflicts.
	if err := stores.Documents.Upsert(ctx, "alpha", state.DocumentRecord{
		Name: "published.md", DocumentID: "doc-published", Size: 1, ChunkCount: 1,
		Collection: "knowledge_test",
	}); err != nil {
		t.Fatalf("Documents.Upsert() error = %v", err)
	}
	if _, err := service.addDocumentAsync(ctx, "alpha", "published.md", content, ""); err == nil {
		t.Fatalf("published-name admission succeeded")
	} else if !errors.As(err, &validation) || validation.Message != asyncUploadConflictMessage {
		t.Fatalf("published-name conflict = %v", err)
	}
}

// TestAddDocumentAsyncRejectsPDFWithoutDoclingModels pins that the asynchronous
// Console upload path also rejects a PDF while the job is admitted: the
// knowledge base has no VLM/OCR configuration, so no document job is queued and
// the caller gets the actionable Chinese message instead of a failed job later.
func TestAddDocumentAsyncRejectsPDFWithoutDoclingModels(t *testing.T) {
	service, stores, _ := newAsyncService(t)
	service.config.PDFEngine = config.PDFEngineDocling
	service.config.PDFServiceURL = "http://127.0.0.1:1"
	ctx := context.Background()

	_, err := service.addDocumentAsync(ctx, "alpha", "manual.pdf", []byte("%PDF-1.4\n"), "pdf-1")
	assertKnowledgeValidation(t, err, doclingPDFModelsRequiredMessage)

	var jobs int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM document_jobs WHERE namespace = ?", "alpha").Scan(&jobs); err != nil {
		t.Fatalf("count document_jobs error = %v", err)
	}
	if jobs != 0 {
		t.Fatalf("document_jobs has %d rows after a rejected PDF admission", jobs)
	}

	// Non-PDF admissions still work; the restriction only applies to PDFs.
	if _, err := service.addDocumentAsync(ctx, "alpha", "manual.md", []byte("# ok"), "md-1"); err != nil {
		t.Fatalf("markdown admission was rejected: %v", err)
	}
}

func TestAddDocumentAsyncValidationAndStatus(t *testing.T) {
	service, stores, _ := newAsyncService(t)
	ctx := context.Background()
	cases := []struct {
		name     string
		filename string
		content  []byte
		key      string
		want     string
	}{
		{"invalid extension", "manual.exe", []byte("x"), "", "Invalid document filename or file type"},
		{"empty content", "manual.md", nil, "", "File must be non-empty and no larger than 10 MiB"},
		{"long idempotency key", "manual.md", []byte("x"), strings.Repeat("k", 201),
			"idempotency_key must be a string of 1 to 128 characters"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := service.addDocumentAsync(ctx, "alpha", testCase.filename, testCase.content, testCase.key)
			var validation *apierrors.KnowledgeValidationError
			if !errors.As(err, &validation) || validation.Message != testCase.want {
				t.Fatalf("error = %v, want %q", err, testCase.want)
			}
		})
	}
	// A namespace without a knowledge-base row is a validation error.
	_, err := service.addDocumentAsync(ctx, "missing", "manual.md", []byte("x"), "")
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != "Knowledge base is not configured" {
		t.Fatalf("unknown namespace error = %v", err)
	}

	// Status transitions: queued -> processing/stage -> failed.
	payload, err := service.addDocumentAsync(ctx, "alpha", "status.md", []byte("status bytes"), "")
	if err != nil {
		t.Fatalf("addDocumentAsync() error = %v", err)
	}
	artifactID := payload["artifact_id"].(string)
	status, err := service.documentStatus(ctx, "alpha", artifactID)
	if err != nil {
		t.Fatalf("documentStatus() error = %v", err)
	}
	if len(status) != 4 || status["artifact_id"] != artifactID || status["status"] != state.DocumentJobQueued {
		t.Fatalf("queued status = %v", status)
	}
	job, err := stores.DocumentJobs.Get(ctx, artifactID, nil)
	if err != nil || job == nil {
		t.Fatalf("DocumentJobs.Get() = %v, %v", job, err)
	}
	notification, err := state.DocumentJobEnvelope(job)
	if err != nil {
		t.Fatalf("DocumentJobEnvelope() error = %v", err)
	}
	task, err := stores.DocumentJobs.Claim(ctx, notification)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %v, %v", task, err)
	}
	if err := stores.DocumentJobs.Progress(ctx, task, "parsing"); err != nil {
		t.Fatalf("Progress() error = %v", err)
	}
	status, err = service.documentStatus(ctx, "alpha", artifactID)
	if err != nil {
		t.Fatalf("processing documentStatus() error = %v", err)
	}
	if status["status"] != state.DocumentJobProcessing || status["stage"] != "parsing" {
		t.Fatalf("processing status = %v", status)
	}
	failure := "indexing failed"
	if err := stores.DocumentJobs.Fail(ctx, task, false, &failure); err != nil {
		t.Fatalf("Fail() error = %v", err)
	}
	status, err = service.documentStatus(ctx, "alpha", artifactID)
	if err != nil {
		t.Fatalf("failed documentStatus() error = %v", err)
	}
	if status["status"] != state.DocumentJobFailed || status["error"] != failure {
		t.Fatalf("failed status = %v", status)
	}

	// Invalid and unknown artifact ids map to validation errors.
	if _, err := service.documentStatus(ctx, "alpha", "not-a-uuid"); err == nil {
		t.Fatalf("invalid artifact id accepted")
	} else if !errors.As(err, &validation) || validation.Message != "artifact_id must be a canonical UUID" {
		t.Fatalf("invalid artifact id error = %v", err)
	}
	if _, err := service.documentStatus(ctx, "alpha", "0f8fad5b-d9cb-469f-a165-70867728950e"); err == nil {
		t.Fatalf("unknown artifact id accepted")
	} else if !errors.As(err, &validation) || validation.Message != "Document upload not found" {
		t.Fatalf("unknown artifact id error = %v", err)
	}
}
