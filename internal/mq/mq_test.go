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
	"RAG_KAFKA_BOOTSTRAP_SERVERS", "RAG_KAFKA_CLIENT_ID", "RAG_KAFKA_SECURITY_PROTOCOL",
	"RAG_KAFKA_SASL_MECHANISM", "RAG_KAFKA_SASL_USERNAME", "RAG_KAFKA_SASL_PASSWORD",
	"RAG_KAFKA_TLS_CA", "RAG_KAFKA_TLS_INSECURE", "RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS",
	"RAG_CACHE_TOPIC", "RAG_CACHE_GROUP",
	"RAG_DOCUMENT_TOPIC", "RAG_DOCUMENT_GROUP", "RAG_DOCUMENT_DLQ_TOPIC", "RAG_DOCUMENT_DLQ_GROUP",
	"RAG_SEARCH_TOPIC", "RAG_SEARCH_GROUP", "RAG_SEARCH_DLQ_TOPIC", "RAG_SEARCH_DLQ_GROUP",
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
		DocumentTopic:    "doc-topic",
		DocumentGroup:    "doc-group",
		SearchTopic:      "search-topic",
		SearchGroup:      "search-group",
		CacheTopic:       "cache-topic",
		CacheGroup:       "cache-group",
		DocumentDLQTopic: "doc-dlq",
		SearchDLQTopic:   "search-dlq",
		PublishTimeout:   time.Second,
		ClientID:         DefaultClientID,
		SecurityProtocol: DefaultSecurityProtocol,
	}
	if configured {
		settings.BootstrapServers = []string{"localhost:9092"}
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
		t.Fatalf("expected brokerless settings: %+v", settings)
	}
	if settings.DocumentTopic != DefaultDocumentTopic || settings.DocumentGroup != DefaultDocumentGroup ||
		settings.SearchTopic != DefaultSearchTopic || settings.SearchGroup != DefaultSearchGroup ||
		settings.CacheTopic != DefaultCacheTopic || settings.CacheGroup != DefaultCacheGroup ||
		settings.DocumentDLQTopic != DefaultDocumentDLQTopic || settings.SearchDLQTopic != DefaultSearchDLQTopic {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
	if settings.ClientID != DefaultClientID || settings.SecurityProtocol != DefaultSecurityProtocol ||
		settings.PublishTimeout != DefaultPublishTimeout {
		t.Fatalf("unexpected security/timeout defaults: %+v", settings)
	}

	isolateEnv(t)
	t.Setenv("RAG_KAFKA_BOOTSTRAP_SERVERS", "broker-1:9092, broker-2:9093 ,")
	t.Setenv("RAG_KAFKA_CLIENT_ID", "worker-1")
	t.Setenv("RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS", "2.5")
	settings, err = SettingsFromEnv()
	if err != nil {
		t.Fatalf("configured SettingsFromEnv() error = %v", err)
	}
	if !settings.Configured() || len(settings.BootstrapServers) != 2 ||
		settings.BootstrapServers[0] != "broker-1:9092" || settings.BootstrapServers[1] != "broker-2:9093" {
		t.Fatalf("bootstrap servers = %+v", settings.BootstrapServers)
	}
	if settings.ClientID != "worker-1" || settings.PublishTimeout != 2500*time.Millisecond {
		t.Fatalf("configured settings = %+v", settings)
	}

	isolateEnv(t)
	t.Setenv("RAG_SEARCH_TOPIC", "praxis_document_ingest")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "Search topic/group must be separate") {
		t.Fatalf("search separation error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_SEARCH_GROUP", "")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "must not be empty") {
		t.Fatalf("empty search group error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS", "0")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "must be positive") {
		t.Fatalf("invalid publish timeout error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_DOCUMENT_TOPIC", "praxis_state_cache_refresh")
	if _, err := SettingsFromEnv(); err == nil {
		t.Fatal("document topic equal to cache topic accepted")
	}
	isolateEnv(t)
	t.Setenv("RAG_KAFKA_SECURITY_PROTOCOL", "KERBEROS")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_KAFKA_SECURITY_PROTOCOL") {
		t.Fatalf("invalid security protocol error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_KAFKA_SASL_MECHANISM", "GSSAPI")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "RAG_KAFKA_SASL_MECHANISM") {
		t.Fatalf("invalid SASL mechanism error = %v", err)
	}
	isolateEnv(t)
	t.Setenv("RAG_SEARCH_DLQ_TOPIC", "praxis_search")
	if _, err := SettingsFromEnv(); err == nil || !strings.Contains(err.Error(), "dead-letter topic must be separate") {
		t.Fatalf("DLQ collision error = %v", err)
	}
}

func TestSettingsValidateAndDLQTopicFor(t *testing.T) {
	settings := testSettings(true)
	if err := settings.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if topic := (Settings{DocumentTopic: "doc", DocumentDLQTopic: "doc-dlq"}).DLQTopicFor("doc"); topic != "doc-dlq" {
		t.Fatalf("DLQTopicFor(doc) = %q", topic)
	}
	if topic := (Settings{SearchTopic: "search", SearchDLQTopic: "search-dlq"}).DLQTopicFor("search"); topic != "search-dlq" {
		t.Fatalf("DLQTopicFor(search) = %q", topic)
	}
	if topic := testSettings(true).DLQTopicFor("other"); topic != "" {
		t.Fatalf("DLQTopicFor(other) = %q", topic)
	}
	missing := testSettings(true)
	missing.SearchTopic = ""
	if err := missing.Validate(); err == nil || !strings.Contains(err.Error(), "Search topic and consumer group must not be empty") {
		t.Fatalf("missing search topic error = %v", err)
	}
	cacheClash := testSettings(true)
	cacheClash.CacheGroup = "search-group"
	if err := cacheClash.Validate(); err == nil || !strings.Contains(err.Error(), "Cache topic/group must be separate") {
		t.Fatalf("cache separation error = %v", err)
	}
	sameDLQ := testSettings(true)
	sameDLQ.DocumentDLQTopic = "search-topic"
	if err := sameDLQ.Validate(); err == nil || !strings.Contains(err.Error(), "Document dead-letter topic") {
		t.Fatalf("DLQ validation error = %v", err)
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
}

func TestDecodeRejectsPoisonPayloads(t *testing.T) {
	if _, err := DecodeDocumentNotification([]byte(`{"schema":3}`)); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("invalid document decode error = %v", err)
	}
	if _, err := DecodeDocumentNotification([]byte("{not json")); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("malformed document decode error = %v", err)
	}
	if _, err := DecodeSearchEnvelope([]byte(`{"schema":2}`)); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("invalid search decode error = %v", err)
	}
	if _, err := DecodeSearchEnvelope([]byte("{} trailing")); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("trailing search decode error = %v", err)
	}
	if _, _, err := EncodeDocumentNotification(state.DocumentNotification{
		Schema: 3, ArtifactID: "not-a-uuid", Namespace: "manuals", Name: "manual.txt",
	}); !errors.Is(err, ErrPayloadInvalid) {
		t.Fatalf("invalid document encode error = %v", err)
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
	if _, err := DecodeDocumentNotification(sent[0].body); err != nil {
		t.Fatalf("published document body is not decodable: %v", err)
	}
	if _, err := DecodeSearchEnvelope(sent[1].body); err != nil {
		t.Fatalf("published search body is not decodable: %v", err)
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
	} else if strings.Contains(err.Error(), "private broker detail") {
		t.Fatalf("start failure leaked broker detail: %v", err)
	}
	notConfiguredTopic := testSettings(true)
	notConfiguredTopic.DocumentTopic = ""
	if err := NewPublisherWithTransport(notConfiguredTopic, &fakeProducerTransport{}).
		SendDocumentNotification(context.Background(), testDocumentNotification()); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("empty document topic error = %v", err)
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
	ackErr     error
	// offsets assigns deterministic per-topic offsets to deliveries that do not
	// carry one, so commit tracking sees a realistic record sequence.
	offsets map[string]int64
}

func (f *fakePullTransport) Start() error { return f.startErr }

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
		if f.offsets == nil {
			f.offsets = make(map[string]int64)
		}
		for index := range batch {
			if batch[index].Offset != 0 {
				if batch[index].Offset > f.offsets[batch[index].Topic] {
					f.offsets[batch[index].Topic] = batch[index].Offset
				}
				continue
			}
			f.offsets[batch[index].Topic]++
			batch[index].Offset = f.offsets[batch[index].Topic]
		}
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
	if f.ackErr != nil {
		return f.ackErr
	}
	f.acked = append(f.acked, message)
	return nil
}

func (f *fakePullTransport) ChangeInvisibleDuration(_ context.Context, message ReceivedMessage, _ time.Duration) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.changed = append(f.changed, message)
	return nil
}

func (f *fakePullTransport) snapshot() (acked, changed []ReceivedMessage, stopCalls int) {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	return append([]ReceivedMessage(nil), f.acked...), append([]ReceivedMessage(nil), f.changed...), f.stopCalls
}

type deadLetterRecord struct {
	topic, key string
	body       []byte
}

type fakeRawPublisher struct {
	mutex     sync.Mutex
	published []deadLetterRecord
	closeCall int
	err       error
}

func (f *fakeRawPublisher) Publish(_ context.Context, topic, key string, body []byte) error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	if f.err != nil {
		return f.err
	}
	f.published = append(f.published, deadLetterRecord{topic: topic, key: key, body: body})
	return nil
}

func (f *fakeRawPublisher) Close() error {
	f.mutex.Lock()
	defer f.mutex.Unlock()
	f.closeCall++
	return nil
}

func testConsumerConfig() ConsumerConfig {
	config := DefaultConsumerConfig("doc-topic", "doc-group")
	config.PollInterval = time.Millisecond
	config.RetryBaseDelay = time.Millisecond
	config.RetryMaxDelay = 5 * time.Millisecond
	return config
}

func TestConsumerAckCommits(t *testing.T) {
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "a", Body: []byte(`{"schema":3}`)},
		{Topic: "doc-topic", Key: "b", Body: []byte(`{"schema":3}`)},
	}}}
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(_ context.Context, message ReceivedMessage) bool {
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 2 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	acked, changed, _ := transport.snapshot()
	if len(acked) != 2 || acked[0].Key != "a" || acked[1].Key != "b" {
		t.Fatalf("acked = %+v", acked)
	}
	if len(changed) != 0 {
		t.Fatalf("Kafka consumer must not extend visibility: %+v", changed)
	}
	if consumer.LastError() != nil {
		t.Fatalf("LastError() = %v", consumer.LastError())
	}
}

func TestConsumerRetriesInProcessUntilSuccess(t *testing.T) {
	var mutex sync.Mutex
	attempts := make([]int, 0, 3)
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "retry", Body: []byte("payload")},
	}}}
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(_ context.Context, message ReceivedMessage) bool {
		mutex.Lock()
		attempts = append(attempts, message.DeliveryAttempt)
		mutex.Unlock()
		return len(attempts) == 3
	})
	consumer.Config.MaxAttempts = 5
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	mutex.Lock()
	gotAttempts := append([]int(nil), attempts...)
	mutex.Unlock()
	if len(gotAttempts) != 3 || gotAttempts[0] != 1 || gotAttempts[1] != 2 || gotAttempts[2] != 3 {
		t.Fatalf("handler attempts = %v", gotAttempts)
	}
	acked, changed, _ := transport.snapshot()
	if len(acked) != 1 || acked[0].Key != "retry" {
		t.Fatalf("acked = %+v", acked)
	}
	if len(changed) != 0 {
		t.Fatalf("nack must be retried in-process: %+v", changed)
	}
}

func TestConsumerExhaustionPublishesDLQAndCommits(t *testing.T) {
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "poison", Body: []byte("raw-payload")},
	}}}
	dlq := &fakeRawPublisher{}
	exhausted := make(chan ReceivedMessage, 1)
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(context.Context, ReceivedMessage) bool {
		return false
	})
	consumer.Config.MaxAttempts = 2
	consumer.Config.DLQTopic = "doc-dlq"
	consumer.DLQ = dlq
	consumer.OnExhausted = func(_ context.Context, message ReceivedMessage) error {
		exhausted <- message
		return nil
	}
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	acked, changed, _ := transport.snapshot()
	if len(acked) != 1 || acked[0].Key != "poison" {
		t.Fatalf("acked = %+v", acked)
	}
	if len(changed) != 0 {
		t.Fatalf("changed = %+v", changed)
	}
	dlq.mutex.Lock()
	published := append([]deadLetterRecord(nil), dlq.published...)
	closeCall := dlq.closeCall
	dlq.mutex.Unlock()
	if len(published) != 1 || published[0].topic != "doc-dlq" || published[0].key != "poison" ||
		string(published[0].body) != "raw-payload" {
		t.Fatalf("DLQ publish = %+v", published)
	}
	if closeCall != 1 {
		t.Fatalf("DLQ close calls = %d", closeCall)
	}
	select {
	case message := <-exhausted:
		if message.DeliveryAttempt != 2 {
			t.Fatalf("OnExhausted DeliveryAttempt = %d, want 2", message.DeliveryAttempt)
		}
	default:
		t.Fatal("OnExhausted did not run")
	}
}

func TestConsumerExhaustionWithoutDLQTopic(t *testing.T) {
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "no-dlq", Body: []byte("raw")},
	}}}
	exhausted := make(chan struct{}, 1)
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(context.Context, ReceivedMessage) bool {
		return false
	})
	consumer.Config.MaxAttempts = 1
	consumer.OnExhausted = func(context.Context, ReceivedMessage) error {
		exhausted <- struct{}{}
		return nil
	}
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if acked, _, _ := transport.snapshot(); len(acked) != 1 {
		t.Fatalf("acked = %+v", acked)
	}
	select {
	case <-exhausted:
	default:
		t.Fatal("OnExhausted did not run without a DLQ topic")
	}
}

func TestConsumerReceiveErrorIsSanitizedAndAckFailureRecorded(t *testing.T) {
	transport := &fakePullTransport{receiveErr: errors.New("broker detail with credentials")}
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(context.Context, ReceivedMessage) bool {
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lastErr := consumer.LastError(); lastErr != nil {
			if !errors.Is(lastErr, ErrReceiveFailed) || strings.Contains(lastErr.Error(), "credential") {
				t.Fatalf("receive error = %v", lastErr)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if lastErr := consumer.LastError(); !errors.Is(lastErr, ErrReceiveFailed) {
		t.Fatalf("LastError() = %v", lastErr)
	}

	ackTransport := &fakePullTransport{
		batches: [][]ReceivedMessage{{{Topic: "doc-topic", Key: "k", Body: []byte("v")}}},
		ackErr:  errors.New("commit detail"),
	}
	ackConsumer := NewPullConsumer(testConsumerConfig(), ackTransport, func(context.Context, ReceivedMessage) bool {
		return true
	})
	if err := ackConsumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lastErr := ackConsumer.LastError(); lastErr != nil {
			if !errors.Is(lastErr, ErrConsumerFailed) || strings.Contains(lastErr.Error(), "commit detail") {
				t.Fatalf("ack error = %v", lastErr)
			}
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := ackConsumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
}

func TestConsumerConstructorsValidationAndLifecycle(t *testing.T) {
	if _, err := NewKafkaPullConsumer(Settings{}, DefaultConsumerConfig("t", "g"), nil); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("unconfigured consumer error = %v", err)
	}
	if _, err := NewKafkaPullConsumer(testSettings(true), ConsumerConfig{Topic: "t"}, nil); err == nil {
		t.Fatal("invalid consumer config accepted")
	}
	invalid := testConsumerConfig()
	invalid.MaxAttempts = -1
	if err := invalid.Validate(); err == nil {
		t.Fatal("negative max attempts accepted")
	}
	invalidConcurrency := testConsumerConfig()
	invalidConcurrency.Concurrency = -1
	if err := invalidConcurrency.Validate(); err == nil {
		t.Fatal("negative concurrency accepted")
	}
	tooConcurrent := testConsumerConfig()
	tooConcurrent.Concurrency = MaxConsumerConcurrency + 1
	if err := tooConcurrent.Validate(); err == nil {
		t.Fatal("out-of-range concurrency accepted")
	}
	if err := NewPullConsumer(DefaultConsumerConfig("t", "g"), &fakePullTransport{}, nil).
		Start(context.Background()); err == nil {
		t.Fatal("nil handler accepted")
	}

	// Constructing the Kafka consumer must not dial: it only builds the reader
	// and lazy DLQ writer, and Stop closes them.
	settings := testSettings(true)
	config := DefaultConsumerConfig("doc-topic", "doc-group")
	kafkaConsumer, err := NewKafkaPullConsumer(settings, config, func(context.Context, ReceivedMessage) bool { return true })
	if err != nil {
		t.Fatalf("NewKafkaPullConsumer() error = %v", err)
	}
	if kafkaConsumer.Config.DLQTopic != settings.DocumentDLQTopic {
		t.Fatalf("DLQ topic = %q", kafkaConsumer.Config.DLQTopic)
	}
	if err := kafkaConsumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := kafkaConsumer.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}

	failing := NewPullConsumer(DefaultConsumerConfig("t", "g"), &fakePullTransport{startErr: errors.New("private")},
		func(context.Context, ReceivedMessage) bool { return true })
	startErr := failing.Start(context.Background())
	if !errors.Is(startErr, ErrConsumerFailed) || strings.Contains(startErr.Error(), "private") {
		t.Fatalf("start failure error = %v", startErr)
	}

	transport := &fakePullTransport{}
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(context.Context, ReceivedMessage) bool { return true })
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("second Start() error = %v", err)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("second Stop() error = %v", err)
	}
	if err := consumer.Start(context.Background()); !errors.Is(err, ErrConsumerFailed) {
		t.Fatalf("Start() after Stop() = %v", err)
	}
	var iface Consumer = consumer
	if iface == nil {
		t.Fatal("consumer interface is nil")
	}
}

func TestConsumerMaxAttemptsZeroRetriesUntilCancelled(t *testing.T) {
	var mutex sync.Mutex
	calls := 0
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "endless", Body: []byte("v")},
	}}}
	consumer := NewPullConsumer(testConsumerConfig(), transport, func(context.Context, ReceivedMessage) bool {
		mutex.Lock()
		calls++
		mutex.Unlock()
		return false
	})
	ctx, cancel := context.WithCancel(context.Background())
	if err := consumer.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	time.Sleep(30 * time.Millisecond)
	cancel()
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	mutex.Lock()
	gotCalls := calls
	mutex.Unlock()
	if gotCalls < 2 {
		t.Fatalf("handler calls = %d, want at least 2", gotCalls)
	}
	if acked, _, _ := transport.snapshot(); len(acked) != 0 {
		t.Fatalf("ack without exhaustion = %+v", acked)
	}
}

func TestConsumerRunsHandlersConcurrently(t *testing.T) {
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "a"},
		{Topic: "doc-topic", Key: "b"},
		{Topic: "doc-topic", Key: "c"},
	}}}
	config := testConsumerConfig()
	config.Concurrency = 3
	entered := make(chan struct{}, 3)
	release := make(chan struct{})
	consumer := NewPullConsumer(config, transport, func(context.Context, ReceivedMessage) bool {
		entered <- struct{}{}
		<-release
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	for started := 1; started <= 3; started++ {
		select {
		case <-entered:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d handlers started concurrently", started-1)
		}
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if acked, _, _ := transport.snapshot(); len(acked) != 3 {
		t.Fatalf("acked = %+v", acked)
	}
}

func TestConsumerCommitsOnlyContiguousCompletedPrefix(t *testing.T) {
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "a"},
		{Topic: "doc-topic", Key: "b"},
		{Topic: "doc-topic", Key: "c"},
	}}}
	config := testConsumerConfig()
	config.Concurrency = 3
	releaseB := make(chan struct{})
	bStarted := make(chan struct{})
	var once sync.Once
	consumer := NewPullConsumer(config, transport, func(_ context.Context, message ReceivedMessage) bool {
		if message.Key == "b" {
			once.Do(func() { close(bStarted) })
			<-releaseB
		}
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-bStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("blocked handler did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if acked, _, _ := transport.snapshot(); len(acked) != 1 || acked[0].Key != "a" {
		t.Fatalf("commits advanced past the blocked record: %+v", acked)
	}
	close(releaseB)
	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	acked, _, _ := transport.snapshot()
	if len(acked) != 3 || acked[0].Key != "a" || acked[1].Key != "b" || acked[2].Key != "c" {
		t.Fatalf("commit order = %+v", acked)
	}
}

func TestConsumerDropsDuplicateInFlightDelivery(t *testing.T) {
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: "dup", Offset: 7},
		{Topic: "doc-topic", Key: "dup", Offset: 7},
	}}}
	config := testConsumerConfig()
	config.Concurrency = 2
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	var mutex sync.Mutex
	calls := 0
	consumer := NewPullConsumer(config, transport, func(context.Context, ReceivedMessage) bool {
		mutex.Lock()
		calls++
		mutex.Unlock()
		select {
		case entered <- struct{}{}:
		default:
		}
		<-release
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("handler did not start")
	}
	time.Sleep(50 * time.Millisecond)
	mutex.Lock()
	gotCalls := calls
	mutex.Unlock()
	if gotCalls != 1 {
		t.Fatalf("handler calls = %d, want 1 for a duplicate in-flight record", gotCalls)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if acked, _, _ := transport.snapshot(); len(acked) != 1 {
		t.Fatalf("acked = %+v", acked)
	}
}

func TestConsumerCommitFailureStopsPartitionAdvance(t *testing.T) {
	transport := &fakePullTransport{
		batches: [][]ReceivedMessage{{
			{Topic: "doc-topic", Key: "a"},
			{Topic: "doc-topic", Key: "b"},
		}},
		ackErr: errors.New("commit rejected"),
	}
	config := testConsumerConfig()
	config.Concurrency = 2
	consumer := NewPullConsumer(config, transport, func(context.Context, ReceivedMessage) bool {
		return true
	})
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if lastErr := consumer.LastError(); lastErr != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if acked, _, _ := transport.snapshot(); len(acked) != 0 {
		t.Fatalf("commit after a failed commit = %+v", acked)
	}
	if lastErr := consumer.LastError(); !errors.Is(lastErr, ErrConsumerFailed) {
		t.Fatalf("LastError() = %v", lastErr)
	}
}
