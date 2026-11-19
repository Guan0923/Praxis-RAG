package httpapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// uploadEventEnvelope is the union of every upload SSE event payload.
type uploadEventEnvelope struct {
	Type     string  `json:"type"`
	UploadID string  `json:"upload_id"`
	Index    *int    `json:"index"`
	Received *int64  `json:"received"`
	Status   string  `json:"status"`
	Reason   *string `json:"reason"`
	Error    *string `json:"error"`
	BatchID  *string `json:"batch_id"`
	Detail   *string `json:"detail"`
}

// uploadPostResult carries one asynchronous POST /v1/document-batches outcome
// so the test goroutine never calls testing helpers off the test goroutine.
type uploadPostResult struct {
	status int
	body   []byte
	err    error
}

// uploadBatchMultipartBody builds an ordered multipart upload body. The order
// of the files parts is the index the SSE progress contract assigns.
func uploadBatchMultipartBody(t *testing.T, names []string, contents [][]byte) ([]byte, string) {
	t.Helper()
	buffer := &bytes.Buffer{}
	writer := multipart.NewWriter(buffer)
	for index, name := range names {
		part, err := writer.CreateFormFile("files", name)
		if err != nil {
			t.Fatalf("create form file: %v", err)
		}
		if _, err := part.Write(contents[index]); err != nil {
			t.Fatalf("write content: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return buffer.Bytes(), writer.FormDataContentType()
}

// openUploadEventStream subscribes to one upload's progress stream and asserts
// the frozen SSE response headers.
func openUploadEventStream(t *testing.T, baseURL, uploadID string, headers map[string]string) (*http.Response, *bufio.Reader) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, baseURL+"/v1/document-batches/upload-events/"+url.PathEscape(uploadID), nil)
	if err != nil {
		t.Fatalf("build upload event request: %v", err)
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	response, err := client.Do(request)
	if err != nil {
		t.Fatalf("open upload event stream: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		defer response.Body.Close()
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("upload event status = %d body = %s", response.StatusCode, body)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
		t.Fatalf("upload event content type = %q", contentType)
	}
	if cacheControl := response.Header.Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("upload event cache control = %q", cacheControl)
	}
	return response, bufio.NewReader(response.Body)
}

// readUploadEvent reads one SSE frame, skipping heartbeat comment lines.
func readUploadEvent(t *testing.T, reader *bufio.Reader) uploadEventEnvelope {
	t.Helper()
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read upload event: %v", err)
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			t.Fatalf("unexpected SSE line %q", line)
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event uploadEventEnvelope
		if err := json.Unmarshal([]byte(payload), &event); err != nil {
			t.Fatalf("decode upload event %q: %v", payload, err)
		}
		if event.Type == "" {
			t.Fatalf("upload event without type: %q", payload)
		}
		return event
	}
}

// postDocumentBatchAsync posts one multipart batch in the background so the
// test can drain the SSE stream while the server streams progress.
func postDocumentBatchAsync(baseURL, uploadID string, body []byte, contentType string, results chan<- uploadPostResult) {
	request, err := http.NewRequest(http.MethodPost, baseURL+"/v1/document-batches", bytes.NewReader(body))
	if err != nil {
		results <- uploadPostResult{err: err}
		return
	}
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("X-RAG-Namespace", "alpha")
	request.Header.Set("Content-Type", contentType)
	if uploadID != "" {
		request.Header.Set("X-Upload-ID", uploadID)
	}
	response, err := (&http.Client{Timeout: 60 * time.Second}).Do(request)
	if err != nil {
		results <- uploadPostResult{err: err}
		return
	}
	defer response.Body.Close()
	payload, readErr := io.ReadAll(response.Body)
	if readErr != nil {
		results <- uploadPostResult{status: response.StatusCode, err: readErr}
		return
	}
	results <- uploadPostResult{status: response.StatusCode, body: payload}
}

// TestUploadEventStreamReportsPerFileProgressAndResults is the end-to-end SSE
// contract check: subscribe, ready, one large and one small file upload with
// per-index cumulative byte progress, one result per file, then done.
func TestUploadEventStreamReportsPerFileProgressAndResults(t *testing.T) {
	runtime := newFakeDocRuntime()
	runtime.batchPayload = map[string]any{
		"batch_id": "batch-sse-1",
		"status":   "processing",
		"total":    2,
		"items": []any{
			map[string]any{"position": 0, "name": "big.pdf", "status": "accepted", "reason": nil, "artifact_id": "artifact-1", "error": nil},
			map[string]any{"position": 1, "name": "note.md", "status": "failed", "reason": "validation", "artifact_id": nil, "error": "不支持的文件类型"},
		},
	}
	server := httptest.NewServer(newRuntimeServer(t, runtime).Handler())
	defer server.Close()

	bigContent := bytes.Repeat([]byte("0123456789abcdef"), 5*1024*1024/16)
	smallContent := []byte("# note\n")
	body, contentType := uploadBatchMultipartBody(t,
		[]string{"big.pdf", "note.md"}, [][]byte{bigContent, smallContent})

	uploadID := "upload-sse-001"
	response, reader := openUploadEventStream(t, server.URL, uploadID, tenantAuth())
	defer response.Body.Close()

	ready := readUploadEvent(t, reader)
	if ready.Type != "ready" || ready.UploadID != uploadID {
		t.Fatalf("ready event = %+v", ready)
	}

	posted := make(chan uploadPostResult, 1)
	go postDocumentBatchAsync(server.URL, uploadID, body, contentType, posted)

	var events []uploadEventEnvelope
	for {
		event := readUploadEvent(t, reader)
		events = append(events, event)
		if event.Type == "done" {
			break
		}
	}

	// The progress stream must stay lossless for one maximum-size file: a slow
	// frame consumer must not lose a final byte count.
	progress := map[int][]int64{}
	finalProgress := map[int]int64{}
	results := map[int]uploadEventEnvelope{}
	sawResult := false
	sawDone := false
	for _, event := range events {
		switch event.Type {
		case "progress":
			if event.Index == nil || event.Received == nil {
				t.Fatalf("progress event without index/received: %+v", event)
			}
			if sawResult {
				t.Fatalf("progress event after a result event: %+v", event)
			}
			progress[*event.Index] = append(progress[*event.Index], *event.Received)
			finalProgress[*event.Index] = *event.Received
		case "result":
			if event.Index == nil {
				t.Fatalf("result event without index: %+v", event)
			}
			if sawDone {
				t.Fatalf("result event after done: %+v", event)
			}
			if _, exists := results[*event.Index]; exists {
				t.Fatalf("duplicate result event for index %d", *event.Index)
			}
			sawResult = true
			results[*event.Index] = event
		case "done":
			if sawDone {
				t.Fatalf("duplicate done event")
			}
			sawDone = true
		case "ready":
			t.Fatalf("duplicate ready event")
		default:
			t.Fatalf("unexpected upload event type %q", event.Type)
		}
	}
	if !sawDone {
		t.Fatal("upload event stream ended without done")
	}

	// Per-file progress: cumulative per index, multiple frames for the large
	// file, and each index ends on its exact content byte size.
	if len(progress[0]) < 2 {
		t.Fatalf("large file progress frames = %v", progress[0])
	}
	if len(progress[1]) == 0 {
		t.Fatalf("small file has no progress frame")
	}
	for index, values := range progress {
		for position := 1; position < len(values); position++ {
			if values[position] < values[position-1] {
				t.Fatalf("index %d progress not monotonic: %v", index, values)
			}
		}
	}
	if finalProgress[0] != int64(len(bigContent)) {
		t.Fatalf("index 0 final progress = %d, want %d", finalProgress[0], len(bigContent))
	}
	if finalProgress[1] != int64(len(smallContent)) {
		t.Fatalf("index 1 final progress = %d, want %d", finalProgress[1], len(smallContent))
	}

	// Per-file results: one failed row must not disturb the other row.
	if len(results) != 2 {
		t.Fatalf("result events = %+v", results)
	}
	saved := results[0]
	if saved.Status != "saved" || saved.Reason != nil || saved.Error != nil {
		t.Fatalf("index 0 result = %+v", saved)
	}
	failed := results[1]
	if failed.Status != "failed" || failed.Reason == nil || *failed.Reason != "validation" ||
		failed.Error == nil || *failed.Error != "不支持的文件类型" {
		t.Fatalf("index 1 result = %+v", failed)
	}

	done := events[len(events)-1]
	if done.BatchID == nil || *done.BatchID != "batch-sse-1" {
		t.Fatalf("done event = %+v", done)
	}
	if done.Detail != nil {
		t.Fatalf("done detail = %v", *done.Detail)
	}

	// The batch response itself is unchanged: 202 with the durable payload.
	result := <-posted
	if result.err != nil {
		t.Fatalf("post upload: %v", result.err)
	}
	if result.status != http.StatusAccepted {
		t.Fatalf("post status = %d body = %s", result.status, result.body)
	}
	var payload struct {
		BatchID string `json:"batch_id"`
		Items   []struct {
			Position int    `json:"position"`
			Name     string `json:"name"`
			Status   string `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(result.body, &payload); err != nil {
		t.Fatalf("decode batch payload %s: %v", result.body, err)
	}
	if payload.BatchID != "batch-sse-1" || len(payload.Items) != 2 ||
		payload.Items[0].Position != 0 || payload.Items[1].Position != 1 {
		t.Fatalf("batch payload = %+v", payload)
	}
	if len(runtime.batchFiles) != 2 || runtime.batchFiles[0].Name != "big.pdf" ||
		runtime.batchFiles[1].Name != "note.md" {
		t.Fatalf("runtime files = %+v", runtime.batchFiles)
	}
	if !bytes.Equal(runtime.batchFiles[0].Content, bigContent) ||
		!bytes.Equal(runtime.batchFiles[1].Content, smallContent) {
		t.Fatalf("runtime file contents were corrupted (%d/%d bytes)",
			len(runtime.batchFiles[0].Content), len(runtime.batchFiles[1].Content))
	}
}

// TestUploadWithoutUploadIDKeepsLegacyBehaviour checks that a batch POST with
// no X-Upload-ID publishes nothing: the subscriber for an unrelated id stays
// silent while the old 202 contract still holds.
func TestUploadWithoutUploadIDKeepsLegacyBehaviour(t *testing.T) {
	runtime := newFakeDocRuntime()
	server := httptest.NewServer(newRuntimeServer(t, runtime).Handler())
	defer server.Close()

	body, contentType := uploadBatchMultipartBody(t, []string{"note.md"}, [][]byte{[]byte("hello")})

	readCtx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	request, err := http.NewRequestWithContext(readCtx, http.MethodGet,
		server.URL+"/v1/document-batches/upload-events/upload-unused", nil)
	if err != nil {
		t.Fatalf("build upload event request: %v", err)
	}
	for key, value := range tenantAuth() {
		request.Header.Set(key, value)
	}
	response, err := (&http.Client{}).Do(request)
	if err != nil {
		t.Fatalf("open upload event stream: %v", err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	if ready := readUploadEvent(t, reader); ready.Type != "ready" {
		t.Fatalf("ready event = %+v", ready)
	}

	posted := make(chan uploadPostResult, 1)
	go postDocumentBatchAsync(server.URL, "", body, contentType, posted)
	result := <-posted
	if result.err != nil {
		t.Fatalf("post upload: %v", result.err)
	}
	if result.status != http.StatusAccepted {
		t.Fatalf("post status = %d body = %s", result.status, result.body)
	}
	if len(runtime.batchFiles) != 1 || runtime.batchFiles[0].Name != "note.md" {
		t.Fatalf("runtime files = %+v", runtime.batchFiles)
	}

	// No frame may follow: the stream is closed by the read deadline once the
	// upload had a chance to publish a (wrongly) associated event. Blank frame
	// separators and heartbeat comments are not events.
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		line = strings.TrimRight(line, "\r\n")
		if line == "" || strings.HasPrefix(line, ":") {
			continue
		}
		t.Fatalf("upload without X-Upload-ID published an event: %q", line)
	}
}

// TestUploadRejectsInvalidUploadID covers both the subscription and the POST
// rejecting a malformed upload id before the body is read.
func TestUploadRejectsInvalidUploadID(t *testing.T) {
	runtime := newFakeDocRuntime()
	server := httptest.NewServer(newRuntimeServer(t, runtime).Handler())
	defer server.Close()

	invalidID := "bad id!"
	subscribe, err := http.NewRequest(http.MethodGet,
		server.URL+"/v1/document-batches/upload-events/"+url.PathEscape(invalidID), nil)
	if err != nil {
		t.Fatalf("build upload event request: %v", err)
	}
	for key, value := range tenantAuth() {
		subscribe.Header.Set(key, value)
	}
	response, err := (&http.Client{Timeout: 5 * time.Second}).Do(subscribe)
	if err != nil {
		t.Fatalf("subscribe with invalid upload id: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid subscribe status = %d", response.StatusCode)
	}
	var subscribed struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(response.Body).Decode(&subscribed); err != nil {
		t.Fatalf("decode invalid subscribe body: %v", err)
	}
	if subscribed.Detail != "Invalid upload id." {
		t.Fatalf("invalid subscribe detail = %q", subscribed.Detail)
	}

	body, contentType := uploadBatchMultipartBody(t, []string{"note.md"}, [][]byte{[]byte("hello")})
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/document-batches", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("build batch request: %v", err)
	}
	request.Header.Set("Authorization", "Bearer test-token")
	request.Header.Set("X-RAG-Namespace", "alpha")
	request.Header.Set("Content-Type", contentType)
	request.Header.Set("X-Upload-ID", invalidID)
	posted, err := (&http.Client{Timeout: 5 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("post with invalid upload id: %v", err)
	}
	defer posted.Body.Close()
	if posted.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid upload id status = %d", posted.StatusCode)
	}
	var rejected struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(posted.Body).Decode(&rejected); err != nil {
		t.Fatalf("decode invalid upload id body: %v", err)
	}
	if rejected.Detail != "Invalid upload id." {
		t.Fatalf("invalid upload id detail = %q", rejected.Detail)
	}
	if len(runtime.batchFiles) != 0 {
		t.Fatalf("invalid upload id reached the runtime: %+v", runtime.batchFiles)
	}
}

// TestUploadEventStreamRequiresTenantCredentials keeps the SSE route behind the
// same credential check as every other tenant route.
func TestUploadEventStreamRequiresTenantCredentials(t *testing.T) {
	runtime := newFakeDocRuntime()
	server := httptest.NewServer(newRuntimeServer(t, runtime).Handler())
	defer server.Close()

	response, err := (&http.Client{Timeout: 5 * time.Second}).Get(
		server.URL + "/v1/document-batches/upload-events/upload-unauthenticated")
	if err != nil {
		t.Fatalf("subscribe without credentials: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated subscribe status = %d", response.StatusCode)
	}
	if contentType := response.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "application/json") {
		t.Fatalf("unauthenticated subscribe content type = %q", contentType)
	}
}

// TestUploadValidationErrorEmitsDone checks that a rejected upload still closes
// its stream with a done frame carrying a safe detail and no results.
func TestUploadValidationErrorEmitsDone(t *testing.T) {
	runtime := newFakeDocRuntime()
	server := httptest.NewServer(newRuntimeServer(t, runtime).Handler())
	defer server.Close()

	body, contentType := uploadBatchMultipartBody(t, []string{"payload.exe"}, [][]byte{[]byte("MZ")})
	uploadID := "upload-rejected-1"
	response, reader := openUploadEventStream(t, server.URL, uploadID, tenantAuth())
	defer response.Body.Close()
	if ready := readUploadEvent(t, reader); ready.Type != "ready" {
		t.Fatalf("ready event = %+v", ready)
	}

	posted := make(chan uploadPostResult, 1)
	go postDocumentBatchAsync(server.URL, uploadID, body, contentType, posted)

	var events []uploadEventEnvelope
	for {
		event := readUploadEvent(t, reader)
		events = append(events, event)
		if event.Type == "done" {
			break
		}
	}
	result := <-posted
	if result.err != nil {
		t.Fatalf("post rejected upload: %v", result.err)
	}
	if result.status != http.StatusBadRequest {
		t.Fatalf("rejected upload status = %d body = %s", result.status, result.body)
	}
	if len(runtime.batchFiles) != 0 {
		t.Fatalf("rejected upload reached the runtime: %+v", runtime.batchFiles)
	}

	done := events[len(events)-1]
	if done.Type != "done" {
		t.Fatalf("last event = %+v", done)
	}
	if done.BatchID != nil {
		t.Fatalf("rejected upload done batch id = %v", *done.BatchID)
	}
	if done.Detail == nil || *done.Detail == "" {
		t.Fatalf("rejected upload done detail = %v", done.Detail)
	}
	for _, event := range events {
		if event.Type == "result" {
			t.Fatalf("rejected upload published a result event: %+v", event)
		}
	}
	// The file part is still read (so progress may precede the rejection), but
	// the terminal frame must be the only terminal frame.
	terminal := 0
	for _, event := range events {
		if event.Type == "done" {
			terminal++
		}
	}
	if terminal != 1 {
		t.Fatalf("rejected upload stream carried %d done frames, want exactly 1", terminal)
	}
}
