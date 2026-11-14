package httpapi

import (
	"encoding/json"
	"net/http"
	"regexp"
	"sync"
	"time"
)

// Upload progress events implement the backend half of the upload SSE contract
// (.test-tmp/upload-sse-contract.md):
//
//	GET /v1/document-batches/upload-events/{upload_id}
//
// The Console subscribes first, waits for ready, then posts the multipart batch
// with the same id in X-Upload-ID. The batch handler publishes the cumulative
// content bytes of every file part and one save result per file, then a
// terminal done frame. The hub is process-local by design: a stream lives on
// the one rag-service instance that also serves the upload POST, and events for
// a stream without a subscriber are dropped instead of buffered for a late
// reader.
const (
	// uploadIDHeader carries the Console-generated id shared by the SSE
	// subscription and the batch POST. It is optional so callers that ignore
	// progress keep the plain batch contract.
	uploadIDHeader = "X-Upload-ID"
	// uploadEventHeartbeat keeps idle streams alive through intermediaries.
	uploadEventHeartbeat = 15 * time.Second
	// uploadEventBufferSize bounds one subscriber's queue. Progress is lossy on
	// purpose so a slow stream can never block or back up an upload request;
	// 1024 frames cover every throttled progress callback of one maximum-size
	// file (10 MiB / 32 KiB) with room to spare.
	uploadEventBufferSize = 1024
	// uploadEventReliableTimeout is the short best-effort wait for a result or
	// done frame: the HTTP response stays the source of truth.
	uploadEventReliableTimeout = 100 * time.Millisecond
	// uploadEventKeySeparator joins namespace and upload id so credentials from
	// one tenant can never observe another tenant's stream.
	uploadEventKeySeparator = "\x00"
)

// uploadIDPattern validates the client-generated upload id; it must be a
// conservative identifier and never carry separators or whitespace.
var uploadIDPattern = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)

// validUploadID reports whether a header or path upload id is well formed.
func validUploadID(uploadID string) bool {
	return uploadIDPattern.MatchString(uploadID)
}

// uploadEventKey scopes one upload stream to its authenticated namespace.
func uploadEventKey(namespace, uploadID string) string {
	return namespace + uploadEventKeySeparator + uploadID
}

// uploadEventReady announces that the subscription is registered; the Console
// may only POST after receiving it.
type uploadEventReady struct {
	Type     string `json:"type"`
	UploadID string `json:"upload_id"`
}

// uploadEventProgress reports the cumulative content bytes of one file part
// (multipart headers excluded). It is monotonic per index.
type uploadEventProgress struct {
	Type     string `json:"type"`
	Index    int    `json:"index"`
	Received int64  `json:"received"`
}

// uploadEventResult reports one file's save outcome. Status is saved, skipped
// or failed; reason and error may be null.
type uploadEventResult struct {
	Type   string  `json:"type"`
	Index  int     `json:"index"`
	Status string  `json:"status"`
	Reason *string `json:"reason"`
	Error  *string `json:"error"`
}

// uploadEventDone terminates the stream. batch_id is set when the batch was
// admitted; detail carries a safe error message for rejected uploads.
type uploadEventDone struct {
	Type    string  `json:"type"`
	BatchID *string `json:"batch_id"`
	Detail  *string `json:"detail"`
}

// uploadEventFrame is one pre-encoded SSE frame. A terminal frame carries done:
// the writer closes the stream after flushing it.
type uploadEventFrame struct {
	payload  []byte
	terminal bool
}

// newUploadEventFrame encodes one event as "data: <json>\n\n". A marshal error
// can only come from a programming mistake, so it yields an empty frame that
// publishers skip.
func newUploadEventFrame(event any, terminal bool) uploadEventFrame {
	body, err := json.Marshal(event)
	if err != nil {
		return uploadEventFrame{}
	}
	payload := make([]byte, 0, len(body)+8)
	payload = append(payload, "data: "...)
	payload = append(payload, body...)
	payload = append(payload, "\n\n"...)
	return uploadEventFrame{payload: payload, terminal: terminal}
}

// uploadEventSubscriber is one open SSE stream: a bounded frame queue plus a
// close signal. The events channel is never closed (sends select on closed), so
// a publisher racing a disconnect can never panic.
type uploadEventSubscriber struct {
	events    chan uploadEventFrame
	closed    chan struct{}
	closeOnce sync.Once
}

func newUploadEventSubscriber() *uploadEventSubscriber {
	return &uploadEventSubscriber{
		events: make(chan uploadEventFrame, uploadEventBufferSize),
		closed: make(chan struct{}),
	}
}

func (subscriber *uploadEventSubscriber) close() {
	subscriber.closeOnce.Do(func() { close(subscriber.closed) })
}

// uploadEventHub maps one (namespace, upload_id) stream to its subscriber.
type uploadEventHub struct {
	mu          sync.Mutex
	subscribers map[string]*uploadEventSubscriber
}

func newUploadEventHub() *uploadEventHub {
	return &uploadEventHub{subscribers: make(map[string]*uploadEventSubscriber)}
}

// subscribe registers the stream for key. A reconnecting client replaces its
// previous stream so a stale handler can never consume the new upload's frames.
func (hub *uploadEventHub) subscribe(key string) *uploadEventSubscriber {
	subscriber := newUploadEventSubscriber()
	hub.mu.Lock()
	previous := hub.subscribers[key]
	hub.subscribers[key] = subscriber
	hub.mu.Unlock()
	if previous != nil {
		previous.close()
	}
	return subscriber
}

// unsubscribe removes the stream only when it is still the registered one, so a
// handler that lost a reconnect race cannot evict the new stream.
func (hub *uploadEventHub) unsubscribe(key string, subscriber *uploadEventSubscriber) {
	hub.mu.Lock()
	if current := hub.subscribers[key]; current == subscriber {
		delete(hub.subscribers, key)
	}
	hub.mu.Unlock()
	subscriber.close()
}

// lookup returns the current subscriber for key (nil when nobody listens).
func (hub *uploadEventHub) lookup(key string) *uploadEventSubscriber {
	if hub == nil {
		return nil
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	return hub.subscribers[key]
}

// publishProgress delivers one cumulative byte count without blocking: progress
// is advisory and dropped when the stream is gone or its queue is full.
func (hub *uploadEventHub) publishProgress(key string, index int, received int64) {
	subscriber := hub.lookup(key)
	if subscriber == nil {
		return
	}
	frame := newUploadEventFrame(uploadEventProgress{Type: "progress", Index: index, Received: received}, false)
	if frame.payload == nil {
		return
	}
	select {
	case subscriber.events <- frame:
	case <-subscriber.closed:
	default:
		// A full queue means the stream is not being drained; the POST response
		// still reports the truth, so dropping is safe.
	}
}

// publishReliable best-effort delivers a result/done frame with a short timeout
// so a stalled stream delays the upload request only briefly.
func (hub *uploadEventHub) publishReliable(key string, frame uploadEventFrame) {
	subscriber := hub.lookup(key)
	if subscriber == nil || frame.payload == nil {
		return
	}
	timer := time.NewTimer(uploadEventReliableTimeout)
	defer timer.Stop()
	select {
	case subscriber.events <- frame:
	case <-subscriber.closed:
	case <-timer.C:
	}
}

// publishResult reports one file's save outcome.
func (hub *uploadEventHub) publishResult(key string, index int, status string, reason, eventError *string) {
	hub.publishReliable(key, newUploadEventFrame(uploadEventResult{
		Type:   "result",
		Index:  index,
		Status: status,
		Reason: reason,
		Error:  eventError,
	}, false))
}

// publishDone terminates the stream; batchID is set only for admitted batches.
func (hub *uploadEventHub) publishDone(key string, batchID, detail *string) {
	hub.publishReliable(key, newUploadEventFrame(uploadEventDone{
		Type:    "done",
		BatchID: batchID,
		Detail:  detail,
	}, true))
}

// handleUploadEvents streams one upload's progress frames. The response stays
// open until done, the client disconnects or the stream is replaced.
func (s *Server) handleUploadEvents(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	uploadID := pathParam(r, "upload_id")
	if !validUploadID(uploadID) {
		writeDetail(w, http.StatusBadRequest, "Invalid upload id.")
		return
	}
	if s.uploadEvents == nil {
		writeDetail(w, http.StatusServiceUnavailable, "Upload events are not available")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeDetail(w, http.StatusInternalServerError, "Streaming is not supported")
		return
	}

	key := uploadEventKey(namespace, uploadID)
	subscriber := s.uploadEvents.subscribe(key)
	defer s.uploadEvents.unsubscribe(key, subscriber)

	header := w.Header()
	header.Set("Content-Type", "text/event-stream")
	header.Set("Cache-Control", "no-store")
	header.Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	ready := newUploadEventFrame(uploadEventReady{Type: "ready", UploadID: uploadID}, false)
	if _, err := w.Write(ready.payload); err != nil {
		return
	}
	flusher.Flush()

	heartbeat := time.NewTicker(uploadEventHeartbeat)
	defer heartbeat.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-subscriber.closed:
			return
		case frame := <-subscriber.events:
			if _, err := w.Write(frame.payload); err != nil {
				return
			}
			flusher.Flush()
			if frame.terminal {
				return
			}
		case <-heartbeat.C:
			if _, err := w.Write([]byte(": ping\n\n")); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
