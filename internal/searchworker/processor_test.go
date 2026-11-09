package searchworker

import (
	"context"
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

func submitTask(t *testing.T, stores *state.Stores, namespace string, arguments map[string]any) state.SearchEnvelope {
	t.Helper()
	envelope, err := stores.SearchTasks.Submit(context.Background(), namespace, arguments, nil)
	if err != nil {
		t.Fatalf("SearchTasks.Submit: %v", err)
	}
	return envelope
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

func taskView(t *testing.T, stores *state.Stores, namespace, resultID string) *state.SearchTaskView {
	t.Helper()
	view, err := stores.SearchTasks.Get(context.Background(), namespace, resultID, 1, 10)
	if err != nil {
		t.Fatalf("SearchTasks.Get(%s): %v", resultID, err)
	}
	return view
}

func TestHandleSuccess(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	searcher := &fakeSearcher{payload: map[string]any{"results": []any{map[string]any{"text": "evidence"}}}}
	capturer := &fakeCapturer{}
	processor := newProcessor(t, stores, searcher, capturer)

	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("Handle() must acknowledge a successful task")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("searcher calls = %d", searcher.callCount())
	}
	if len(capturer.saveSnapshot) != 1 || capturer.saveSnapshot[0] {
		t.Fatalf("capture saveSnapshot = %v, want [false] (completion owns the snapshot)", capturer.saveSnapshot)
	}
	view := taskView(t, stores, "manuals", envelope.ResultID)
	if view.Status != state.SearchTaskSucceeded {
		t.Fatalf("task status = %q", view.Status)
	}
	// A redelivery of a finished task is acknowledged without re-running it.
	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("redelivery must be acknowledged")
	}
	if searcher.callCount() != 1 {
		t.Fatalf("redelivery re-ran retrieval: %d calls", searcher.callCount())
	}
}

func TestHandleRetriesInfrastructureFailures(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	searcher := &fakeSearcher{err: errors.New("qdrant unavailable")}
	processor := newProcessor(t, stores, searcher, nil)

	if acked := processor.Handle(context.Background(), envelope); acked {
		t.Fatalf("retryable failure must not be acknowledged")
	}
	view := taskView(t, stores, "manuals", envelope.ResultID)
	if view.Status != state.SearchTaskQueued || view.Stage != "retry_wait" {
		t.Fatalf("retried task = %+v", view)
	}

	// The next delivery succeeds on the same task.
	searcher.setError(nil)
	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("retry delivery must be acknowledged after success")
	}
	if view := taskView(t, stores, "manuals", envelope.ResultID); view.Status != state.SearchTaskSucceeded {
		t.Fatalf("task status = %q", view.Status)
	}
}

func TestHandleValidationFailureIsTerminal(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	searcher := &fakeSearcher{err: &contracts.SearchValidationError{Message: "query must be a non-empty string"}}
	processor := newProcessor(t, stores, searcher, nil)

	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("terminal validation failure must be acknowledged")
	}
	terminal, err := stores.SearchTasks.Terminal(context.Background(), envelope)
	if err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}
	if _, err := stores.SearchTasks.Get(context.Background(), "manuals", envelope.ResultID, 1, 10); err == nil ||
		!strings.Contains(err.Error(), "Search failed") {
		t.Fatalf("failed task Get() error = %v", err)
	}
}

func TestHandleCaptureFailureKeepsResults(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	searcher := &fakeSearcher{}
	capturer := &fakeCapturer{err: errors.New("artifact registry unavailable")}
	processor := newProcessor(t, stores, searcher, capturer)

	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("capture failure must not fail the task (references are optional)")
	}
	if view := taskView(t, stores, "manuals", envelope.ResultID); view.Status != state.SearchTaskSucceeded {
		t.Fatalf("task status = %q", view.Status)
	}
}

func TestHandleExpiredDeadlineMarksTimedOut(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	clock := envelope.CreatedAt
	stores.SearchTasks.Clock = func() float64 { return clock }
	clock = envelope.Deadline + 1

	searcher := &fakeSearcher{}
	processor := newProcessor(t, stores, searcher, nil)
	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("an expired task must be acknowledged as terminal")
	}
	if searcher.callCount() != 0 {
		t.Fatalf("expired task must not run retrieval")
	}
	clock = envelope.Deadline + 2
	if _, err := stores.SearchTasks.Get(context.Background(), "manuals", envelope.ResultID, 1, 10); err == nil ||
		!strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timed-out Get() error = %v", err)
	}
}

func TestHandleForeignNamespaceIsTerminal(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	envelope.Namespace = "other"
	processor := newProcessor(t, stores, &fakeSearcher{}, nil)
	if acked := processor.Handle(context.Background(), envelope); !acked {
		t.Fatalf("foreign/unknown task deliveries are terminal and acknowledged")
	}
}

func TestMessageHandlerDecodesEnvelope(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	body, _, err := mq.EncodeSearchEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeSearchEnvelope: %v", err)
	}
	processor := newProcessor(t, stores, &fakeSearcher{}, &fakeCapturer{})
	handler := processor.MessageHandler()
	if acked := handler(context.Background(), mq.ReceivedMessage{Body: body}); !acked {
		t.Fatalf("valid message must be acknowledged")
	}
	if acked := handler(context.Background(), mq.ReceivedMessage{Body: []byte("{not json")}); acked {
		t.Fatalf("poison payload must be nacked for broker retry/DLQ")
	}
	if acked := handler(context.Background(), mq.ReceivedMessage{Body: []byte(`{"schema":1}`)}); acked {
		t.Fatalf("invalid envelope must be nacked for broker retry/DLQ")
	}
}

func TestDeadLetter(t *testing.T) {
	stores := newTestStores(t)
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	processor := newProcessor(t, stores, &fakeSearcher{}, nil)

	complete, err := processor.DeadLetter(context.Background(), envelope)
	if err != nil || !complete {
		t.Fatalf("DeadLetter() = %v, %v", complete, err)
	}
	terminal, err := stores.SearchTasks.Terminal(context.Background(), envelope)
	if err != nil || !terminal {
		t.Fatalf("Terminal() = %v, %v", terminal, err)
	}

	body, _, err := mq.EncodeSearchEnvelope(envelope)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if acked := processor.DeadLetterHandler()(context.Background(), mq.ReceivedMessage{Body: body}); !acked {
		t.Fatalf("dead-letter handler must acknowledge an already terminal task")
	}
	exhausted := processor.ExhaustedHandler()
	if err := exhausted(context.Background(), mq.ReceivedMessage{Body: []byte("bad")}); err != nil {
		t.Fatalf("ExhaustedHandler(poison) = %v", err)
	}
}

type fakeReceiver struct {
	mutex     sync.Mutex
	envelopes []state.SearchEnvelope
}

func (f *fakeReceiver) Receive(ctx context.Context) (state.SearchEnvelope, bool, error) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if len(f.envelopes) == 0 {
		return state.SearchEnvelope{}, false, nil
	}
	envelope := f.envelopes[0]
	f.envelopes = f.envelopes[1:]
	return envelope, true, nil
}

func (f *fakeReceiver) add(envelopes ...state.SearchEnvelope) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.envelopes = append(f.envelopes, envelopes...)
}

func TestRunOnceAndRunShutdown(t *testing.T) {
	stores := newTestStores(t)
	first := submitTask(t, stores, "manuals", map[string]any{"query": "first"})
	second := submitTask(t, stores, "manuals", map[string]any{"query": "second"})
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
	if view := taskView(t, stores, "manuals", first.ResultID); view.Status != state.SearchTaskSucceeded {
		t.Fatalf("first status = %q", view.Status)
	}

	third := submitTask(t, stores, "manuals", map[string]any{"query": "third"})
	receiver.add(third)
	done := make(chan error, 1)
	go func() { done <- processor.Run(context.Background()) }()
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if view, _ := stores.SearchTasks.Get(context.Background(), "manuals", third.ResultID, 1, 10); view != nil &&
			view.Status == state.SearchTaskSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if view := taskView(t, stores, "manuals", third.ResultID); view.Status != state.SearchTaskSucceeded {
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
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	processor, err := New(Options{Tasks: stores.SearchTasks, Logger: testLogger()})
	if err != nil {
		t.Fatalf("dead-letter-only New: %v", err)
	}
	if acked := processor.Handle(context.Background(), envelope); acked {
		t.Fatalf("dead-letter-only Handle must be retryable")
	}
	if complete, err := processor.DeadLetter(context.Background(), envelope); err != nil || !complete {
		t.Fatalf("DeadLetter() = %v, %v", complete, err)
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
	envelope := submitTask(t, stores, "manuals", map[string]any{"query": "manual"})
	body, _, err := mq.EncodeSearchEnvelope(envelope)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	processor := newProcessor(t, stores, &fakeSearcher{}, &fakeCapturer{})
	consumer := &fakeConsumer{handler: processor.MessageHandler(), messages: make(chan mq.ReceivedMessage, 1)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- processor.RunConsumer(ctx, consumer) }()

	consumer.messages <- mq.ReceivedMessage{Body: body}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if view, _ := stores.SearchTasks.Get(context.Background(), "manuals", envelope.ResultID, 1, 10); view != nil &&
			view.Status == state.SearchTaskSucceeded {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if view := taskView(t, stores, "manuals", envelope.ResultID); view.Status != state.SearchTaskSucceeded {
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
