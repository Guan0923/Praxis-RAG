package e2e

// End-to-end coverage for the asynchronous REST upload entry point: a multipart
// POST admits durable document jobs and answers with artifact ids, the worker
// parses/embeds/indexes them through the same storeAndIndex pipeline, and the
// synchronous update/delete paths keep fencing queued admissions.

import (
	"bytes"
	"context"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/wire"
)

const restToken = "rest-test-token"

// newRESTHandler builds the real HTTP surface over the harness runtime with the
// asynchronous batch upload endpoint.
func newRESTHandler(t *testing.T, h *harness) http.Handler {
	t.Helper()
	settings := config.ServiceSettings{
		Host:     "127.0.0.1",
		Port:     8011,
		Token:    restToken,
		DataRoot: "unused",
	}
	server := httpapi.New(settings, nil, nil, testLogger(),
		httpapi.WithRuntime(wire.HTTPRuntime{Service: h.Service}))
	return server.Handler()
}

// restRequest performs one authenticated tenant request against the handler.
func restRequest(handler http.Handler, method, target, namespace, idempotencyKey, contentType string, body []byte) *httptest.ResponseRecorder {
	var reader *bytes.Reader
	if body == nil {
		reader = bytes.NewReader(nil)
	} else {
		reader = bytes.NewReader(body)
	}
	request := httptest.NewRequest(method, target, reader)
	request.Header.Set("Authorization", "Bearer "+restToken)
	request.Header.Set("X-RAG-Namespace", namespace)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

// restUploadFile is one multipart file part in submission order.
type restUploadFile struct{ name, content string }

// restMultipart renders one POST /v1/document-batches body.
func restMultipart(t *testing.T, files []restUploadFile) ([]byte, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	for _, file := range files {
		part, err := writer.CreateFormFile("files", file.name)
		if err != nil {
			t.Fatalf("CreateFormFile() error = %v", err)
		}
		if _, err := part.Write([]byte(file.content)); err != nil {
			t.Fatalf("write multipart content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("multipart Close() error = %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

func decodeRESTBody(t *testing.T, recorder *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	payload := map[string]any{}
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response %q: %v", recorder.Body.String(), err)
	}
	return payload
}

// firstRestJobArtifactID extracts the artifact id of one admitted file.
func firstRestJobArtifactID(t *testing.T, payload map[string]any) string {
	t.Helper()
	jobs, ok := payload["items"].([]any)
	if !ok || len(jobs) == 0 {
		t.Fatalf("response has no admitted jobs: %v", payload)
	}
	job, _ := jobs[0].(map[string]any)
	artifactID, _ := job["artifact_id"].(string)
	if artifactID == "" {
		t.Fatalf("admitted job has no artifact_id: %v", job)
	}
	return artifactID
}

// runDocumentWorkerUntil delivers committed outbox notifications through a fake
// Kafka transport (the Debezium seam) until the predicate holds, then stops the
// worker.
func runDocumentWorkerUntil(t *testing.T, h *harness, description string, done func() bool) {
	t.Helper()
	worker, err := documentworker.New(documentworker.Options{
		Jobs:            h.Stores.DocumentJobs,
		Ingester:        h.Service,
		JournalRecovery: h.Service,
		Logger:          testLogger(),
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	transport := &fakePullTransport{}
	consumer := mq.NewPullConsumer(
		consumerConfig("praxis_document_ingest", "praxis_document_ingest_workers"),
		transport, worker.MessageHandler(),
	)
	runCtx, cancel := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() { finished <- worker.RunConsumer(runCtx, consumer) }()

	// The fake transport has no consumer offsets, so track what was handed over.
	delivered := make(map[string]bool)
	waitFor(t, 90*time.Second, description, func() bool {
		if done() {
			return true
		}
		pushPendingDocumentNotifications(t, h, transport, delivered)
		return done()
	})
	cancel()
	select {
	case runErr := <-finished:
		if runErr != nil {
			t.Fatalf("RunConsumer() error = %v", runErr)
		}
	case <-time.After(90 * time.Second):
		t.Fatalf("document worker did not stop")
	}
}

// pushPendingDocumentNotifications publishes every not-yet-delivered committed
// document_job_outbox row as the schema-3 notification Debezium would emit.
func pushPendingDocumentNotifications(t *testing.T, h *harness, transport *fakePullTransport, delivered map[string]bool) {
	t.Helper()
	events, err := h.Stores.DocumentJobs.PendingOutbox(context.Background(), 10)
	if err != nil {
		t.Fatalf("PendingOutbox() error = %v", err)
	}
	for _, event := range events {
		if delivered[event.ID] {
			continue
		}
		notification, err := state.ValidateDocumentNotification(map[string]any{
			"schema":      float64(state.DocumentEnvelopeSchema),
			"artifact_id": event.ArtifactID,
			"namespace":   event.Namespace,
			"name":        event.Name,
			"created_at":  event.CreatedAt,
		})
		if err != nil {
			t.Fatalf("ValidateDocumentNotification() error = %v", err)
		}
		body, _, err := mq.EncodeDocumentNotification(notification)
		if err != nil {
			t.Fatalf("EncodeDocumentNotification() error = %v", err)
		}
		transport.push(mq.ReceivedMessage{Topic: "praxis_document_ingest", Body: body, DeliveryAttempt: 1})
		delivered[event.ID] = true
	}
}

// TestRESTAsyncUploadPipeline is the end-to-end contract of batch uploads:
// admission is durable and immediate, status reports worker stages, the published
// document becomes readable/deletable, and a queued admission that loses the name
// race to the synchronous write path fails terminally instead of overwriting it.
func TestRESTAsyncUploadPipeline(t *testing.T) {
	h := newHarness(t, "")
	handler := newRESTHandler(t, h)
	ctx := context.Background()

	// 1. The batch is admitted durably and returns per-item artifact ids.
	body, contentType := restMultipart(t, []restUploadFile{{"rest.md", documentText}})
	admitted := restRequest(handler, http.MethodPost, "/v1/document-batches", "alpha", "rest-batch-1", contentType, body)
	if admitted.Code != http.StatusAccepted {
		t.Fatalf("async upload status = %d body = %s", admitted.Code, admitted.Body.String())
	}
	payload := decodeRESTBody(t, admitted)
	artifactID := firstRestJobArtifactID(t, payload)
	job, _ := payload["items"].([]any)[0].(map[string]any)
	if job["name"] != "rest.md" || job["status"] != state.DocumentBatchItemAccepted {
		t.Fatalf("admitted job = %v", job)
	}

	// Admission commits a job row, not a document row: the name is not published
	// and cannot be deleted yet.
	if documents, err := h.Service.ListDocuments(ctx, "alpha"); err != nil || len(documents) != 0 {
		t.Fatalf("documents after admission = %v, %v", documents, err)
	}
	if deleted := restRequest(handler, http.MethodDelete, "/v1/documents/rest.md", "alpha", "", "", nil); deleted.Code != http.StatusNotFound {
		t.Fatalf("delete-before-publish status = %d body = %s", deleted.Code, deleted.Body.String())
	}

	// The status endpoint is tenant-scoped and never cached.
	status := restRequest(handler, http.MethodGet, "/v1/document-jobs/"+artifactID, "alpha", "", "", nil)
	if status.Code != http.StatusOK {
		t.Fatalf("job status = %d body = %s", status.Code, status.Body.String())
	}
	if got := status.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("job status Cache-Control = %q", got)
	}
	if statusPayload := decodeRESTBody(t, status); statusPayload["status"] != state.DocumentJobQueued {
		t.Fatalf("queued status = %v", statusPayload)
	}
	if foreign := restRequest(handler, http.MethodGet, "/v1/document-jobs/"+artifactID, "beta", "", "", nil); foreign.Code != http.StatusNotFound {
		t.Fatalf("cross-tenant status = %d", foreign.Code)
	}

	// A second admission of the still-queued filename is isolated per file.
	duplicateBody, duplicateType := restMultipart(t, []restUploadFile{{"rest.md", "other bytes"}})
	duplicate := restRequest(handler, http.MethodPost, "/v1/document-batches", "alpha", "rest-batch-2", duplicateType, duplicateBody)
	if duplicate.Code != http.StatusAccepted {
		t.Fatalf("duplicate admission status = %d body = %s", duplicate.Code, duplicate.Body.String())
	}
	duplicatePayload := decodeRESTBody(t, duplicate)
	if duplicatePayload["accepted"] != float64(0) || duplicatePayload["skipped"] != float64(1) {
		t.Fatalf("duplicate admission outcome = %v", duplicatePayload)
	}
	items, _ := duplicatePayload["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("duplicate batch items = %v", items)
	}
	failure, _ := items[0].(map[string]any)
	if failure["name"] != "rest.md" || failure["reason"] != state.DocumentBatchReasonQueued {
		t.Fatalf("duplicate skip = %v", failure)
	}

	// 2. The worker indexes the job; the status projection reaches succeeded.
	runDocumentWorkerUntil(t, h, "async REST job succeeded", func() bool {
		current, readErr := h.Stores.DocumentJobs.Get(context.Background(), artifactID, nil)
		return readErr == nil && current != nil && current.Status == state.DocumentJobSucceeded
	})
	status = restRequest(handler, http.MethodGet, "/v1/document-jobs/"+artifactID, "alpha", "", "", nil)
	statusPayload := decodeRESTBody(t, status)
	if statusPayload["status"] != state.DocumentJobSucceeded || statusPayload["stage"] != "completed" {
		t.Fatalf("succeeded status = %v", statusPayload)
	}
	documents, err := h.Service.ListDocuments(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0]["name"] != "rest.md" {
		t.Fatalf("documents after publish = %v, %v", documents, err)
	}
	if deleted := restRequest(handler, http.MethodDelete, "/v1/documents/rest.md", "alpha", "", "", nil); deleted.Code != http.StatusOK {
		t.Fatalf("delete-after-publish status = %d body = %s", deleted.Code, deleted.Body.String())
	}

	// 3. Mutual exclusion with the synchronous write path: the queued admission
	//    loses the name race, fails terminally, and never replaces the
	//    synchronously written document.
	mutexBody, mutexType := restMultipart(t, []restUploadFile{{"mutex.md", documentText}})
	mutexAdmitted := restRequest(handler, http.MethodPost, "/v1/document-batches", "alpha", "mutex-batch", mutexType, mutexBody)
	if mutexAdmitted.Code != http.StatusAccepted {
		t.Fatalf("mutex admission status = %d body = %s", mutexAdmitted.Code, mutexAdmitted.Body.String())
	}
	mutexID := firstRestJobArtifactID(t, decodeRESTBody(t, mutexAdmitted))
	if _, err := h.Service.WriteDocument(ctx, "alpha", "mutex.md",
		[]byte(documentText+"\n\nSynchronous replacement wins.\n"), false, ""); err != nil {
		t.Fatalf("synchronous WriteDocument() error = %v", err)
	}
	runDocumentWorkerUntil(t, h, "mutex job failed", func() bool {
		current, readErr := h.Stores.DocumentJobs.Get(context.Background(), mutexID, nil)
		return readErr == nil && current != nil && current.Status == state.DocumentJobFailed
	})
	mutexStatus := restRequest(handler, http.MethodGet, "/v1/document-jobs/"+mutexID, "alpha", "", "", nil)
	mutexPayload := decodeRESTBody(t, mutexStatus)
	if mutexPayload["status"] != state.DocumentJobFailed || mutexPayload["error"] == nil {
		t.Fatalf("mutex job status = %v", mutexPayload)
	}
	documents, err = h.Service.ListDocuments(ctx, "alpha")
	if err != nil || len(documents) != 1 || documents[0]["name"] != "mutex.md" {
		t.Fatalf("synchronous document after mutex race = %v, %v", documents, err)
	}
	if deleted := restRequest(handler, http.MethodDelete, "/v1/documents/mutex.md", "alpha", "", "", nil); deleted.Code != http.StatusOK {
		t.Fatalf("delete synchronous document status = %d body = %s", deleted.Code, deleted.Body.String())
	}
}
