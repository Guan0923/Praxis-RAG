package mq

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

var mqEnvKeys = []string{
	"RAG_ROCKETMQ_ENDPOINT", "RAG_ROCKETMQ_NAMESPACE", "RAG_ROCKETMQ_TLS",
	"RAG_ROCKETMQ_ACCESS_KEY", "RAG_ROCKETMQ_SECRET_KEY", "RAG_ROCKETMQ_TOPIC",
	"RAG_ROCKETMQ_GROUP", "RAG_DOCUMENT_TOPIC", "RAG_DOCUMENT_GROUP",
	"RAG_SEARCH_TOPIC", "RAG_SEARCH_GROUP",
	"RAG_ROCKETMQ_PUBLISH_TIMEOUT_SECONDS", "RAG_ROCKETMQ_VISIBILITY_SECONDS",
	"RAG_ROCKETMQ_AWAIT_SECONDS",
}

func isolateEnv(t *testing.T) {
	t.Helper()
	type saved struct {
		value   string
		present bool
	}
	snapshot := make(map[string]saved, len(mqEnvKeys))
	for _, name := range mqEnvKeys {
		value, present := os.LookupEnv(name)
		snapshot[name] = saved{value: value, present: present}
		_ = os.Unsetenv(name)
	}
	t.Cleanup(func() {
		for name, item := range snapshot {
			if item.present {
				_ = os.Setenv(name, item.value)
			} else {
				_ = os.Unsetenv(name)
			}
		}
	})
}

func testSettings(configured bool) Settings {
	settings := Settings{
		DocumentTopic:     "doc-topic",
		DocumentGroup:     "doc-group",
		SearchTopic:       "search-topic",
		SearchGroup:       "search-group",
		PublishTimeout:    time.Second,
		VisibilityTimeout: time.Second,
		AwaitDuration:     time.Second,
	}
	if configured {
		settings.Endpoint = "localhost:8081"
	}
	return settings
}

func testDocumentNotification() state.DocumentNotification {
	return state.DocumentNotification{
		Schema: state.DocumentEnvelopeSchema, ArtifactID: "d1feec53-1036-4af0-8035-86ef6990fa6a",
		Namespace: "manuals", Name: "manual.txt", CreatedAt: 1700000000.5,
	}
}

func testSearchEnvelope() state.SearchEnvelope {
	return state.SearchEnvelope{
		Schema: state.SearchEnvelopeSchema, ResultID: "d1feec53-1036-4af0-8035-86ef6990fa6a",
		Namespace: "manuals", Arguments: map[string]any{"query": "hello", "top_k": float64(3)},
		CreatedAt: 100, ExpiresAt: 500, Deadline: 200,
	}
}

func TestSettingsFromEnvDefaultsAndValidation(t *testing.T) {
	isolateEnv(t)
	settings, err := SettingsFromEnv()
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.Configured() {
		t.Fatalf("expected unconfigured settings: %+v", settings)
	}
	if settings.DocumentTopic != "praxis_document_ingest" || settings.DocumentGroup != "praxis_document_ingest_workers" ||
		settings.SearchTopic != "praxis_search" || settings.SearchGroup != "praxis_search_workers" {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if settings.PublishTimeout != DefaultPublishTimeout || settings.VisibilityTimeout != DefaultVisibilityTimeout {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	t.Setenv("RAG_SEARCH_TOPIC", "praxis_document_ingest")
	_, err = SettingsFromEnv()
	if err == nil || !strings.Contains(err.Error(), "Search topic/group must be separate") {
		t.Fatalf("search separation error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_SEARCH_GROUP", "")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty search group error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_ROCKETMQ_PUBLISH_TIMEOUT_SECONDS", "0")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("invalid timeout error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_DOCUMENT_TOPIC", "praxis_state_cache_refresh")
	if _, err := SettingsFromEnv(); err == nil {
		t.Fatal("document topic equal to cache topic accepted")
	}
}

func TestDocumentEnvelopeExactPayload(t *testing.T) {
	notification := testDocumentNotification()
	body, key, err := EncodeDocumentNotification(notification)
	if err != nil {
		t.Fatalf("EncodeDocumentNotification() error = %v", err)
	}
	if key != notification.ArtifactID {
		t.Fatalf("key = %q", key)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	expectedKeys := map[string]bool{"schema": true, "artifact_id": true, "namespace": true, "name": true, "created_at": true}
	if len(payload) != len(expectedKeys) {
		t.Fatalf("payload keys = %v", payload)
	}
	for name := range payload {
		if !expectedKeys[name] {
			t.Fatalf("unexpected payload key %q: %s", name, body)
		}
	}
	if payload["schema"] != float64(3) || payload["artifact_id"] != notification.ArtifactID ||
		payload["namespace"] != "manuals" || payload["name"] != "manual.txt" {
		t.Fatalf("payload = %v", payload)
	}
	for _, forbidden := range []string{"content", "bytes", "sha256"} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("payload leaked %q: %s", forbidden, body)
		}
	}
	decoded, err := DecodeDocumentNotification(body)
	if err != nil || decoded.ArtifactID != notification.ArtifactID || decoded.CreatedAt != notification.CreatedAt {
		t.Fatalf("DecodeDocumentNotification() = %+v, %v", decoded, err)
	}
	// Consumers also accept raw Debezium change events.
	cdc := []byte(`{"op":"c","after":{"artifact_id":"` + notification.ArtifactID +
		`","namespace":"manuals","name":"manual.txt","created_at":1.5}}`)
	decoded, err = DecodeDocumentNotification(cdc)
	if err != nil || decoded.CreatedAt != 1.5 || decoded.Schema != 3 {
		t.Fatalf("Debezium decode = %+v, %v", decoded, err)
	}
	if _, err := DecodeDocumentNotification([]byte(`{"schema":3}`)); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("invalid decode error = %v", err)
	}
	if _, _, err := EncodeDocumentNotification(state.DocumentNotification{
		Schema: 3, ArtifactID: "not-a-uuid", Namespace: "manuals", Name: "manual.txt",
	}); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("invalid encode error = %v", err)
	}
}

func TestSearchEnvelopeRoundTrip(t *testing.T) {
	envelope := testSearchEnvelope()
	body, key, err := EncodeSearchEnvelope(envelope)
	if err != nil {
		t.Fatalf("EncodeSearchEnvelope() error = %v", err)
	}
	if key != envelope.ResultID {
		t.Fatalf("key = %q", key)
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("payload is not JSON: %v", err)
	}
	for _, name := range []string{"schema", "result_id", "namespace", "arguments", "created_at", "expires_at", "deadline"} {
		if _, present := payload[name]; !present {
			t.Fatalf("payload missing %q: %s", name, body)
		}
	}
	if len(payload) != 7 {
		t.Fatalf("payload keys = %v", payload)
	}
	decoded, err := DecodeSearchEnvelope(body)
	if err != nil || decoded.ResultID != envelope.ResultID || decoded.Namespace != envelope.Namespace ||
		decoded.Arguments["query"] != "hello" || decoded.Deadline != envelope.Deadline {
		t.Fatalf("DecodeSearchEnvelope() = %+v, %v", decoded, err)
	}
	if _, _, err := EncodeSearchEnvelope(state.SearchEnvelope{
		Schema: 2, ResultID: "nope", Namespace: "manuals", Arguments: map[string]any{},
	}); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("invalid search encode error = %v", err)
	}
}

type sentMessage struct {
	topic, key string
	body       []byte
}

type fakeProducerTransport struct {
	mutex     sync.Mutex
	starts    int
	stopCalls int
	sent      []sentMessage
	startErr  error
	sendErr   error
	block     bool
}

func (f *fakeProducerTransport) Start() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.starts++
	return f.startErr
}

func (f *fakeProducerTransport) Send(ctx context.Context, topic, key string, body []byte) error {
	if f.block {
		<-ctx.Done()
		return ctx.Err()
	}
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.sendErr != nil {
		return f.sendErr
	}
	f.sent = append(f.sent, sentMessage{topic: topic, key: key, body: body})
	return nil
}

func (f *fakeProducerTransport) Stop() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.stopCalls++
	return nil
}

func TestPublisherPublishesBothTopicsAndLazyStarts(t *testing.T) {
	transport := &fakeProducerTransport{}
	publisher := NewPublisherWithTransport(testSettings(true), transport)
	defer func() { _ = publisher.Close() }()
	if err := publisher.SendDocumentNotification(context.Background(), testDocumentNotification()); err != nil {
		t.Fatalf("SendDocumentNotification() error = %v", err)
	}
	if err := publisher.SendSearchEnvelope(context.Background(), testSearchEnvelope()); err != nil {
		t.Fatalf("SendSearchEnvelope() error = %v", err)
	}
	transport.mutex.Lock()
	starts := transport.starts
	sent := append([]sentMessage(nil), transport.sent...)
	transport.mutex.Unlock()
	if starts != 1 || len(sent) != 2 {
		t.Fatalf("starts = %d, sent = %d", starts, len(sent))
	}
	if sent[0].topic != "doc-topic" || sent[0].key != testDocumentNotification().ArtifactID {
		t.Fatalf("document message = %+v", sent[0])
	}
	if sent[1].topic != "search-topic" || sent[1].key != testSearchEnvelope().ResultID {
		t.Fatalf("search message = %+v", sent[1])
	}
}

func TestPublisherSafeErrorMappingAndClose(t *testing.T) {
	if _, err := NewPublisher(Settings{}); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured publisher error = %v", err)
	}
	timeoutTransport := &fakeProducerTransport{block: true}
	settings := testSettings(true)
	settings.PublishTimeout = 20 * time.Millisecond
	publisher := NewPublisherWithTransport(settings, timeoutTransport)
	err := publisher.SendDocumentNotification(context.Background(), testDocumentNotification())
	if !errors.Is(err, ErrPublishTimeout) {
		t.Fatalf("timeout error = %v", err)
	}
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "manual.txt") {
		t.Fatalf("error leaked transport details: %v", err)
	}
	if cause := errors.Unwrap(err); cause != ErrPublishTimeout {
		t.Fatalf("unwrap = %v", cause)
	}
	if err := publisher.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if err := publisher.Close(); err != nil {
		t.Fatalf("second Close() error = %v", err)
	}
	timeoutTransport.mutex.Lock()
	stopped := timeoutTransport.stopCalls
	timeoutTransport.mutex.Unlock()
	if stopped != 1 {
		t.Fatalf("stop calls = %d", stopped)
	}
	if err := publisher.SendDocumentNotification(context.Background(), testDocumentNotification()); !errors.Is(err, ErrPublisherClosed) {
		t.Fatalf("send after close = %v", err)
	}
	failed := NewPublisherWithTransport(testSettings(true), &fakeProducerTransport{startErr: errors.New("private broker detail")})
	if err := failed.SendDocumentNotification(context.Background(), testDocumentNotification()); !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("start failure error = %v", err)
	}
	if strings.Contains(err.Error(), "private broker detail") {
		t.Fatalf("start failure leaked broker detail: %v", err)
	}
}

type fakePullTransport struct {
	mutex      sync.Mutex
	batches    [][]ReceivedMessage
	acked      []ReceivedMessage
	changed    []ReceivedMessage
	startErr   error
	stopCalls  int
	receiveErr error
}

func (f *fakePullTransport) Start() error {
	if f.startErr != nil {
		return f.startErr
	}
	return nil
}

func (f *fakePullTransport) Stop() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.stopCalls++
	return nil
}

func (f *fakePullTransport) Receive(ctx context.Context, _ int32, _ time.Duration) ([]ReceivedMessage, error) {
	f.mutex.Lock()
	if f.receiveErr != nil {
		err := f.receiveErr
		f.receiveErr = nil
		f.mutex.Unlock()
		return nil, err
	}
	if len(f.batches) > 0 {
		batch := f.batches[0]
		f.batches = f.batches[1:]
		f.mutex.Unlock()
		return batch, nil
	}
	f.mutex.Unlock()
	<-ctx.Done()
	return nil, ctx.Err()
}

func (f *fakePullTransport) Ack(_ context.Context, message ReceivedMessage) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.acked = append(f.acked, message)
	return nil
}

func (f *fakePullTransport) ChangeInvisibleDuration(_ context.Context, message ReceivedMessage, _ time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.changed = append(f.changed, message)
	return nil
}

func TestConsumerAckNackAndExhaustion(t *testing.T) {
	handled := make(chan string, 4)
	exhausted := make(chan string, 1)
	transport := &fakePullTransport{batches: [][]ReceivedMessage{
		{
			{Topic: "doc-topic", Key: "a", Body: []byte(`{"schema":3}`), DeliveryAttempt: 1},
			{Topic: "doc-topic", Key: "b", Body: []byte(`{"schema":3}`), DeliveryAttempt: 1},
			{Topic: "doc-topic", Key: "c", Body: []byte(`{"schema":3}`), DeliveryAttempt: 3},
		},
	}}
	config := DefaultConsumerConfig("doc-topic", "doc-group")
	config.MaxAttempts = 3
	config.PollInterval = time.Millisecond
	consumer := NewPullConsumer(config, transport, func(_ context.Context, message ReceivedMessage) bool {
		handled <- message.Key
		return message.Key == "a"
	})
	consumer.OnExhausted = func(_ context.Context, message ReceivedMessage) error {
		exhausted <- message.Key
		return nil
	}
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	for index := 0; index < 3; index++ {
		select {
		case <-handled:
		case <-time.After(2 * time.Second):
			t.Fatal("handler did not receive all messages")
		}
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	transport.mutex.Lock()
	acked := append([]ReceivedMessage(nil), transport.acked...)
	changed := append([]ReceivedMessage(nil), transport.changed...)
	transport.mutex.Unlock()
	if len(acked) != 2 || acked[0].Key != "a" || acked[1].Key != "c" {
		t.Fatalf("acked = %+v", acked)
	}
	if len(changed) != 1 || changed[0].Key != "b" {
		t.Fatalf("changed = %+v", changed)
	}
	select {
	case key := <-exhausted:
		if key != "c" {
			t.Fatalf("exhausted key = %q", key)
		}
	default:
		t.Fatal("OnExhausted did not run")
	}
	if err := consumer.Start(context.Background()); !errors.Is(err, ErrConsumerFailed) {
		t.Fatalf("Start() after Stop() = %v", err)
	}
	if lastErr := consumer.LastError(); lastErr != nil {
		t.Fatalf("LastError() = %v", lastErr)
	}
}

func TestConsumerReceiveErrorIsSanitized(t *testing.T) {
	handled := make(chan struct{}, 1)
	transport := &fakePullTransport{receiveErr: errors.New("broker detail with credentials")}
	consumer := NewPullConsumer(DefaultConsumerConfig("t", "g"), transport, func(context.Context, ReceivedMessage) bool {
		handled <- struct{}{}
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lastErr := consumer.LastError(); lastErr != nil {
			_ = consumer.Stop()
			if !errors.Is(lastErr, ErrReceiveFailed) || strings.Contains(lastErr.Error(), "credential") {
				t.Fatalf("receive error = %v", lastErr)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	_ = consumer.Stop()
	t.Fatal("receive error was not recorded")
}

func TestConsumerConstructorsAndValidation(t *testing.T) {
	if _, err := NewRocketMQPullConsumer(Settings{}, DefaultConsumerConfig("t", "g"), nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured consumer error = %v", err)
	}
	if _, err := NewRocketMQPullConsumer(testSettings(true), ConsumerConfig{Topic: "t"}, nil); err == nil {
		t.Fatal("invalid consumer config accepted")
	}
	failing := NewPullConsumer(DefaultConsumerConfig("t", "g"), &fakePullTransport{startErr: errors.New("private")},
		func(context.Context, ReceivedMessage) bool { return true })
	startErr := failing.Start(context.Background())
	if !errors.Is(startErr, ErrConsumerFailed) {
		t.Fatalf("start failure error = %v", startErr)
	}
	if strings.Contains(startErr.Error(), "private") {
		t.Fatalf("start failure leaked detail: %v", startErr)
	}
	var consumer Consumer = NewPullConsumer(DefaultConsumerConfig("t", "g"), &fakePullTransport{}, nil)
	if consumer == nil {
		t.Fatal("consumer interface is nil")
	}
}
