package searchworker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

const testNamespace = "manuals"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

// submitTask admits one queued search task and returns the schema-3
// notification Debezium publishes for the committed row (schedule_version = 1).
func submitTask(t *testing.T, stores *state.Stores, arguments map[string]any) state.SearchNotification {
	t.Helper()
	notification, err := stores.SearchTasks.Submit(context.Background(), testNamespace, arguments, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit: %v", err)
	}
	return notification
}

// notificationAtVersion rewrites the scheduling generation of a notification to
// model a stale or replacement delivery.
func notificationAtVersion(notification state.SearchNotification, version int64) state.SearchNotification {
	notification.ScheduleVersion = version
	return notification
}

// nativeBody encodes the application-native schema-3 envelope.
func nativeBody(t *testing.T, notification state.SearchNotification) []byte {
	t.Helper()
	body, err := json.Marshal(notification)
	if err != nil {
		t.Fatalf("json.Marshal(SearchNotification): %v", err)
	}
	return body
}

// searchCDCRow renders the search_tasks row image Debezium captures: only the
// locating columns of column.include.list, never the arguments or deadline.
func searchCDCRow(notification state.SearchNotification) map[string]any {
	return map[string]any{
		"result_id":        notification.ResultID,
		"namespace":        notification.Namespace,
		"schedule_version": notification.ScheduleVersion,
	}
}

// searchCDCEvent renders one raw Debezium change event
// (debezium.format.value.schemas.enable=false).
func searchCDCEvent(t *testing.T, before, after any, op string) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"before": before,
		"after":  after,
		"source": map[string]any{
			"schema": "praxis_rag",
			"table":  "search_tasks",
		},
		"op":    op,
		"ts_ms": float64(1700000000000),
	})
	if err != nil {
		t.Fatalf("json.Marshal(change event): %v", err)
	}
	return body
}

// taskRow reads the durable task state without the Get projection, which maps
// every active status to "queued".
func taskRow(t *testing.T, stores *state.Stores, resultID string) (status, stage string, version int64) {
	t.Helper()
	err := stores.DB.QueryRow(
		"SELECT status, stage, schedule_version FROM search_tasks WHERE result_id = ?",
		resultID).Scan(&status, &stage, &version)
	if err != nil {
		t.Fatalf("read search_tasks row: %v", err)
	}
	return status, stage, version
}

// snapshotCount counts the MySQL fact rows of one result.
func snapshotCount(t *testing.T, stores *state.Stores, resultID string) int {
	t.Helper()
	var count int
	if err := stores.DB.QueryRow(
		"SELECT COUNT(*) FROM search_result_snapshots WHERE id = ?", resultID).Scan(&count); err != nil {
		t.Fatalf("count search_result_snapshots: %v", err)
	}
	return count
}

type fakeSearcher struct {
	mutex    sync.Mutex
	payload  map[string]any
	err      error
	calls    int
	requests []contracts.SearchRequest
	onSearch func(ctx context.Context)
}

func (f *fakeSearcher) Search(ctx context.Context, _ string, request contracts.SearchRequest) (map[string]any, error) {
	f.mutex.Lock()
	f.calls++
	f.requests = append(f.requests, request)
	onSearch := f.onSearch
	err := f.err
	payload := f.payload
	f.mutex.Unlock()
	if onSearch != nil {
		onSearch(ctx)
	}
	if err != nil {
		return nil, err
	}
	if payload == nil {
		return map[string]any{"results": []any{}}, nil
	}
	return payload, nil
}

func (f *fakeSearcher) setError(err error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.err = err
}

func (f *fakeSearcher) callCount() int {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return f.calls
}

type fakeCapturer struct {
	mutex        sync.Mutex
	err          error
	calls        int
	saveSnapshot []bool
	enrich       func(payload map[string]any) map[string]any
}

func (f *fakeCapturer) Capture(_ context.Context, _ string, payload map[string]any, saveSnapshot bool) (map[string]any, error) {
	f.mutex.Lock()
	f.calls++
	f.saveSnapshot = append(f.saveSnapshot, saveSnapshot)
	err := f.err
	enrich := f.enrich
	f.mutex.Unlock()
	if err != nil {
		return nil, err
	}
	if enrich != nil {
		return enrich(payload), nil
	}
	payload["captured"] = true
	return payload, nil
}

func newProcessor(t *testing.T, stores *state.Stores, searcher Searcher, capturer Capturer) *Processor {
	t.Helper()
	processor, err := New(Options{
		Tasks:    stores.SearchTasks,
		Searcher: searcher,
		Capturer: capturer,
		Logger:   testLogger(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return processor
}

func taskView(t *testing.T, stores *state.Stores, resultID string) *state.SearchTaskView {
	t.Helper()
	view, err := stores.SearchTasks.Get(context.Background(), testNamespace, resultID, 1, 10)
	if err != nil {
		t.Fatalf("SearchTasks.Get(%s): %v", resultID, err)
	}
	return view
}

// taskDeadline reads the durable deadline of a queued task.
func taskDeadline(t *testing.T, stores *state.Stores, resultID string) float64 {
	t.Helper()
	return taskView(t, stores, resultID).Deadline
}

// evidencePayload is the retrieval result the workers must persist in MySQL.
func evidencePayload() map[string]any {
	return map[string]any{"results": []any{
		map[string]any{"text": "evidence", "document": "manual.txt"},
	}}
}

// TestHandleSuccessWritesMySQLFactRow covers the new happy path end to end:
// Claim -> retrieval -> Complete in one MySQL transaction, with the result fact
// row readable even when the Redis transient cache is absent.
func TestHandleSuccessWritesMySQLFactRow(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	searcher := &fakeSearcher{payload: evidencePayload()}
	capturer := &fakeCapturer{}
	processor := newProcessor(t, stores, searcher, capturer)

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("Handle() must acknowledge a completed task")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d", searcher.callCount())
	}
	if len(capturer.saveSnapshot) != 1 || capturer.saveSnapshot[0] {
		t.Fatalf("capture saveSnapshot = %v, want [false] (completion owns the snapshot)", capturer.saveSnapshot)
	}
	status, stage, version := taskRow(t, stores, notification.ResultID)
	if status != state.SearchTaskSucceeded || stage != "completed" || version != 1 {
		t.Fatalf("durable task = %q/%q version %d", status, stage, version)
	}
	if count := snapshotCount(t, stores, notification.ResultID); count != 1 {
		t.Fatalf("search_result_snapshots rows = %d, want 1", count)
	}
	// Redis is only a read cache: the fact row must still be readable without it.
	stores.Results.Transient = nil
	result, err := stores.Results.Get(testNamespace, notification.ResultID, 1, 10)
	if err != nil {
		t.Fatalf("Results.Get without Redis: %v", err)
	}
	if results, ok := result["results"].([]any); !ok || len(results) != 1 {
		t.Fatalf("result payload = %+v", result)
	}
	if result["snapshot_available"] != true {
		t.Fatalf("snapshot_available = %v", result["snapshot_available"])
	}

	// A redelivery of the completed task is acknowledged without re-running it.
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("redelivery must be acknowledged")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("redelivery re-ran retrieval: %d calls", searcher.callCount())
	}
}

// TestCompleteIsIdempotent replays the completion of a task whose terminal row
// and snapshot are already committed: no second fact row and no overwrite.
func TestCompleteIsIdempotent(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	searcher := &fakeSearcher{payload: evidencePayload()}
	processor := newProcessor(t, stores, searcher, nil)
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("Handle() must acknowledge a completed task")
	}

	duplicate := &state.SearchTask{ResultID: notification.ResultID, Namespace: testNamespace}
	payload, err := stores.SearchTasks.Complete(context.Background(), duplicate, map[string]any{"results": []any{}})
	if err != nil || payload == nil {
		t.Fatalf("duplicate Complete() = %v, %v; want the prepared payload and no error", payload, err)
	}
	if count := snapshotCount(t, stores, notification.ResultID); count != 1 {
		t.Fatalf("duplicate Complete inserted rows: %d", count)
	}
	stores.Results.Transient = nil
	result, err := stores.Results.Get(testNamespace, notification.ResultID, 1, 10)
	if err != nil {
		t.Fatalf("Results.Get: %v", err)
	}
	if results, ok := result["results"].([]any); !ok || len(results) != 1 {
		t.Fatalf("committed payload was overwritten: %+v", result)
	}
}

// TestHandleRetryIncrementsScheduleVersion covers docs/cdc-migration.md §4.1 for
// search: Fail(retry=true) bumps schedule_version and requeues, the stale
// generation is skipped, and the replacement generation executes.
func TestHandleRetryIncrementsScheduleVersion(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	searcher := &fakeSearcher{err: errors.New("qdrant unavailable")}
	processor := newProcessor(t, stores, searcher, nil)

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("a recorded retry must be acknowledged (the replacement notification carries the new version)")
	}
	status, stage, version := taskRow(t, stores, notification.ResultID)
	if status != state.SearchTaskQueued || stage != "retry_wait" || version != 2 {
		t.Fatalf("retried task = %q/%q version %d", status, stage, version)
	}
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d", searcher.callCount())
	}

	// The stale generation is skipped without running retrieval again.
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("stale schedule_version delivery must be acknowledged")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("stale delivery re-ran retrieval: %d calls", searcher.callCount())
	}

	// The replacement generation is claimable and completes.
	searcher.setError(nil)
	replacement := notificationAtVersion(notification, version)
	if acked := processor.Handle(context.Background(), replacement); !acked {
		t.Fatalf("replacement delivery must be acknowledged")
	}
	if status, _, _ := taskRow(t, stores, notification.ResultID); status != state.SearchTaskSucceeded {
		t.Fatalf("task status = %q", status)
	}
	if searcher.callCount() != 2 {
		t.Fatalf("searcher calls = %d, want 2", searcher.callCount())
	}
}

func TestHandleValidationFailureIsTerminal(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	searcher := &fakeSearcher{err: &contracts.SearchValidationError{Message: "query must be a non-empty string"}}
	processor := newProcessor(t, stores, searcher, nil)

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("terminal validation failure must be acknowledged")
	}
	status, _, version := taskRow(t, stores, notification.ResultID)
	if status != state.SearchTaskFailed || version != 1 {
		t.Fatalf("terminal task = %q version %d", status, version)
	}
	if terminal, err := stores.SearchTasks.Terminal(context.Background(), notification); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	if _, err := stores.SearchTasks.Get(context.Background(), testNamespace, notification.ResultID, 1, 10); err == nil ||
		!strings.Contains(err.Error(), "Search failed") {
		t.Fatalf("failed task Get() error = %v", err)
	}
}

func TestHandleCaptureFailureKeepsResults(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	searcher := &fakeSearcher{}
	capturer := &fakeCapturer{err: errors.New("artifact registry unavailable")}
	processor := newProcessor(t, stores, searcher, capturer)

	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("capture failure must not fail the task (references are optional)")
	}
	if status, _, _ := taskRow(t, stores, notification.ResultID); status != state.SearchTaskSucceeded {
		t.Fatalf("task status = %q", status)
	}
}

// TestHandleExpiredDeadlineMarksTimedOut covers the deadline convergence in
// Claim: an overdue active task becomes timed_out and the delivery is ACKed
// without running retrieval.
func TestHandleExpiredDeadlineMarksTimedOut(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	deadline := taskDeadline(t, stores, notification.ResultID)
	stores.SearchTasks.Clock = func() float64 { return deadline + 1 }

	searcher := &fakeSearcher{}
	processor := newProcessor(t, stores, searcher, nil)
	if acked := processor.Handle(context.Background(), notification); !acked {
		t.Fatalf("an expired task must be acknowledged as terminal")
	}
	if searcher.callCount() != 0 {
		t.Fatalf("expired task must not run retrieval")
	}
	if status, stage, _ := taskRow(t, stores, notification.ResultID); status != state.SearchTaskTimedOut || stage != "failed" {
		t.Fatalf("timed-out task = %q/%q", status, stage)
	}
	if _, err := stores.SearchTasks.Get(context.Background(), testNamespace, notification.ResultID, 1, 10); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timed-out Get() error = %v", err)
	}
}

func TestHandleForeignNamespaceIsAcknowledged(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	foreign := notification
	foreign.Namespace = "other"
	searcher := &fakeSearcher{}
	processor := newProcessor(t, stores, searcher, nil)

	if acked := processor.Handle(context.Background(), foreign); !acked {
		t.Fatalf("foreign/unknown task deliveries are safely skipped and acknowledged")
	}
	if searcher.callCount() != 0 {
		t.Fatalf("foreign delivery ran retrieval")
	}
	if status, _, version := taskRow(t, stores, notification.ResultID); status != state.SearchTaskQueued || version != 1 {
		t.Fatalf("task changed by foreign delivery: %q version %d", status, version)
	}
}

// TestMessageHandlerDecodesNativeAndDebeziumEnvelopes proves both accepted wire
// shapes execute: the application-native schema-3 envelope and the raw Debezium
// change event (op c/r/u).
func TestMessageHandlerDecodesNativeAndDebeziumEnvelopes(t *testing.T) {
	cases := []struct {
		name string
		body func(t *testing.T, notification state.SearchNotification) []byte
	}{
		{name: "native schema-3", body: nativeBody},
		{name: "debezium create", body: func(t *testing.T, notification state.SearchNotification) []byte {
			return searchCDCEvent(t, nil, searchCDCRow(notification), "c")
		}},
		{name: "debezium snapshot read", body: func(t *testing.T, notification state.SearchNotification) []byte {
			return searchCDCEvent(t, nil, searchCDCRow(notification), "r")
		}},
		{name: "debezium schedule bump", body: func(t *testing.T, notification state.SearchNotification) []byte {
			return searchCDCEvent(t, searchCDCRow(notification), searchCDCRow(notification), "u")
		}},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			stores := newTestStores(t)
			notification := submitTask(t, stores, map[string]any{"query": "manual"})
			processor := newProcessor(t, stores, &fakeSearcher{payload: evidencePayload()}, &fakeCapturer{})
			if acked := processor.MessageHandler()(context.Background(), mq.ReceivedMessage{
				Topic: "praxis_search", Body: testCase.body(t, notification),
			}); !acked {
				t.Fatalf("valid notification must be acknowledged")
			}
			if status, _, _ := taskRow(t, stores, notification.ResultID); status != state.SearchTaskSucceeded {
				t.Fatalf("task status = %q", status)
			}
		})
	}
}

func TestMessageHandlerRejectsMalformedEnvelopes(t *testing.T) {
	stores := newTestStores(t)
	processor := newProcessor(t, stores, &fakeSearcher{}, nil)
	handler := processor.MessageHandler()
	for _, body := range [][]byte{
		[]byte("{not json"),
		// Search schema 3 requires result_id and schedule_version.
		[]byte(`{"schema":2,"result_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals","schedule_version":1}`),
		[]byte(`{"schema":1}`),
		[]byte(`{"schema":3,"result_id":"not-a-uuid","namespace":"manuals","schedule_version":1}`),
		[]byte(`{"schema":3,"result_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals"}`),
		[]byte(`{"schema":3,"result_id":"3c1b9d0e-1111-4c2a-9f3e-9d0e11114c2a","namespace":"manuals","schedule_version":0}`),
	} {
		if acked := handler(context.Background(), mq.ReceivedMessage{Body: body}); acked {
			t.Fatalf("poison payload %q must be nacked for broker retry/DLQ", body)
		}
	}
}

// TestDeadLetterRecordsTerminalAndIgnoresNonTaskEvents covers both dead-letter
// duties: a reachable task becomes failed, and records that are not task changes
// (deletes, tombstones) are already complete.
func TestDeadLetterRecordsTerminalAndIgnoresNonTaskEvents(t *testing.T) {
	stores := newTestStores(t)
	reachable := submitTask(t, stores, map[string]any{"query": "reachable"})
	untouched := submitTask(t, stores, map[string]any{"query": "untouched"})
	processor := newProcessor(t, stores, &fakeSearcher{}, nil)
	ctx := context.Background()

	complete, err := processor.DeadLetter(ctx, reachable)
	if err != nil || !complete {
		t.Fatalf("DeadLetter() = %v, %v", complete, err)
	}
	if status, _, _ := taskRow(t, stores, reachable.ResultID); status != state.SearchTaskFailed {
		t.Fatalf("dead-lettered task status = %q", status)
	}
	if terminal, err := stores.SearchTasks.Terminal(ctx, reachable); err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	if acked := processor.DeadLetterHandler()(ctx, mq.ReceivedMessage{Body: nativeBody(t, reachable)}); !acked {
		t.Fatalf("dead-letter handler must acknowledge an already terminal task")
	}

	deleteBody := searchCDCEvent(t, searchCDCRow(untouched), nil, "d")
	for name, body := range map[string][]byte{
		"delete":                deleteBody,
		"tombstone (nil)":       nil,
		"tombstone (JSON null)": []byte("null"),
	} {
		complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{Body: body})
		if err != nil || !complete {
			t.Fatalf("DeadLetterMessage(%s) = %v, %v; want (true, nil)", name, complete, err)
		}
		if acked := processor.DeadLetterHandler()(ctx, mq.ReceivedMessage{Body: body}); !acked {
			t.Fatalf("DeadLetterHandler(%s) must acknowledge", name)
		}
		if err := processor.ExhaustedHandler()(ctx, mq.ReceivedMessage{Body: body}); err != nil {
			t.Fatalf("ExhaustedHandler(%s) error = %v", name, err)
		}
	}
	if status, _, version := taskRow(t, stores, untouched.ResultID); status != state.SearchTaskQueued || version != 1 {
		t.Fatalf("ignorable dead letter changed the task: %q version %d", status, version)
	}

	// A poison dead-letter payload must stay retryable.
	if complete, err := processor.DeadLetterMessage(ctx, mq.ReceivedMessage{Body: []byte("{not json")}); err != nil || complete {
		t.Fatalf("poison dead-letter message = %v, %v; want (false, nil)", complete, err)
	}
}

type fakeReceiver struct {
	mutex         sync.Mutex
	notifications []state.SearchNotification
}

func (f *fakeReceiver) Receive(context.Context) (state.SearchNotification, bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if len(f.notifications) == 0 {
		return state.SearchNotification{}, false, nil
	}
	notification := f.notifications[0]
	f.notifications = f.notifications[1:]
	return notification, true, nil
}

func (f *fakeReceiver) add(notifications ...state.SearchNotification) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.notifications = append(f.notifications, notifications...)
}

func TestRunOnceAndRunShutdown(t *testing.T) {
	stores := newTestStores(t)
	first := submitTask(t, stores, map[string]any{"query": "first"})
	second := submitTask(t, stores, map[string]any{"query": "second"})
	receiver := &fakeReceiver{}
	receiver.add(first, second)
	processor, err := New(Options{
		Tasks:        stores.SearchTasks,
		Searcher:     &fakeSearcher{},
		Capturer:     &fakeCapturer{},
		Logger:       testLogger(),
		Receiver:     receiver,
		BatchSize:    2,
		PollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	processed, err := processor.RunOnce(context.Background())
	if err != nil || processed != 2 {
		t.Fatalf("RunOnce() = %d, %v", processed, err)
	}
	if view := taskView(t, stores, first.ResultID); view.Status != state.SearchTaskSucceeded {
		t.Fatalf("first status = %q", view.Status)
	}

	third := submitTask(t, stores, map[string]any{"query": "third"})
	receiver.add(third)
	done := make(chan error, 1)
	go func() { done <- processor.Run(context.Background()) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if view, _ := stores.SearchTasks.Get(context.Background(), testNamespace, third.ResultID, 1, 10); view != nil &&
			view.Status == state.SearchTaskSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if view := taskView(t, stores, third.ResultID); view.Status != state.SearchTaskSucceeded {
		t.Fatalf("Run loop did not process the third task: %q", view.Status)
	}

	processor.Stop()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run() = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("Run() did not stop")
	}
	processor.Stop() // idempotent
	if err := processor.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := processor.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if err := processor.Run(context.Background()); err == nil {
		t.Fatalf("Run after Close must fail")
	}
}

func TestRunRequiresNoReceiverForRunOnce(t *testing.T) {
	stores := newTestStores(t)
	processor := newProcessor(t, stores, &fakeSearcher{}, nil)
	if processed, err := processor.RunOnce(context.Background()); err != nil || processed != 0 {
		t.Fatalf("RunOnce without receiver = %d, %v", processed, err)
	}
}

func TestDeadLetterOnlyWorker(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	processor, err := New(Options{Tasks: stores.SearchTasks, Logger: testLogger()})
	if err != nil {
		t.Fatalf("dead-letter-only New: %v", err)
	}
	if acked := processor.Handle(context.Background(), notification); acked {
		t.Fatalf("dead-letter-only Handle must be retryable")
	}
	if complete, err := processor.DeadLetter(context.Background(), notification); err != nil || !complete {
		t.Fatalf("dead-letter-only DeadLetter() = %v, %v", complete, err)
	}
	if _, err := New(Options{}); err == nil {
		t.Fatalf("missing Tasks must fail")
	}
}

// fakeConsumer is a minimal mq.Consumer that feeds one channel through the
// configured handler, covering the production RunConsumer lifecycle.
type fakeConsumer struct {
	handler  mq.Handler
	messages chan mq.ReceivedMessage

	mutex   sync.Mutex
	started bool
	stopped bool
}

func (c *fakeConsumer) Start(ctx context.Context) error {
	c.mutex.Lock()
	c.started = true
	c.mutex.Unlock()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case message := <-c.messages:
				c.handler(ctx, message)
			}
		}
	}()
	return nil
}

func (c *fakeConsumer) Stop() error {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	c.stopped = true
	return nil
}

func (c *fakeConsumer) state() (bool, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()
	return c.started, c.stopped
}

func TestRunConsumerLifecycle(t *testing.T) {
	stores := newTestStores(t)
	notification := submitTask(t, stores, map[string]any{"query": "manual"})
	body := nativeBody(t, notification)
	processor := newProcessor(t, stores, &fakeSearcher{}, &fakeCapturer{})
	consumer := &fakeConsumer{handler: processor.MessageHandler(), messages: make(chan mq.ReceivedMessage, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.RunConsumer(ctx, consumer) }()

	consumer.messages <- mq.ReceivedMessage{Body: body}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if view, _ := stores.SearchTasks.Get(context.Background(), testNamespace, notification.ResultID, 1, 10); view != nil &&
			view.Status == state.SearchTaskSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if view := taskView(t, stores, notification.ResultID); view.Status != state.SearchTaskSucceeded {
		t.Fatalf("consumer handler did not finish the task: %q", view.Status)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunConsumer() = %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("RunConsumer did not stop")
	}
	started, stopped := consumer.state()
	if !started || !stopped {
		t.Fatalf("consumer lifecycle started=%v stopped=%v", started, stopped)
	}
	if err := processor.RunConsumer(context.Background(), nil); err == nil {
		t.Fatalf("RunConsumer(nil) must fail")
	}
}
