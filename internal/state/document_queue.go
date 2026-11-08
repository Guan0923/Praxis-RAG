package state

import (
	"context"
	"log"
	"strings"
	"sync"
	"time"
)

// DocumentNotificationPublisher publishes identifier-only schema-3 document
// notifications (RocketMQ in production; fakes in tests).
type DocumentNotificationPublisher interface {
	SendDocumentNotification(ctx context.Context, notification DocumentNotification) error
	Close() error
}

// DocumentQueueSettings mirrors document_queue.document_queue_settings().
type DocumentQueueSettings struct {
	Endpoint  string
	Topic     string
	Group     string
	Namespace string
	AccessKey string
	SecretKey string
	TLS       bool
}

// DocumentQueueSettingsFromEnv validates the document topic/group and that they
// are separate from the cache refresh queue.
func DocumentQueueSettingsFromEnv() (DocumentQueueSettings, error) {
	cache := RocketMQSettingsFromEnv()
	settings := DocumentQueueSettings{
		Endpoint:  cache.Endpoint,
		Topic:     strings.TrimSpace(envOrDefault("RAG_DOCUMENT_TOPIC", "praxis_document_ingest")),
		Group:     strings.TrimSpace(envOrDefault("RAG_DOCUMENT_GROUP", "praxis_document_ingest_workers")),
		Namespace: cache.Namespace,
		AccessKey: cache.AccessKey,
		SecretKey: cache.SecretKey,
		TLS:       cache.TLS,
	}
	if settings.Topic == "" || settings.Group == "" {
		return DocumentQueueSettings{}, newValueError("Document topic and consumer group must not be empty")
	}
	if settings.Topic == cache.Topic || settings.Group == cache.Group {
		return DocumentQueueSettings{}, newValueError("Document topic/group must be separate from cache refresh")
	}
	return settings, nil
}

// DocumentBridgeModes are the supported outbox bridge modes.
var DocumentBridgeModes = []string{"relay", "debezium"}

// DocumentBridgeModeFromEnv reads RAG_OUTBOX_BRIDGE (default relay).
func DocumentBridgeModeFromEnv() (string, error) {
	value := strings.ToLower(strings.TrimSpace(envOrDefault("RAG_OUTBOX_BRIDGE", "relay")))
	for _, mode := range DocumentBridgeModes {
		if value == mode {
			return value, nil
		}
	}
	return "", newValueError("RAG_OUTBOX_BRIDGE must be relay or debezium")
}

// DocumentOutboxRelay publishes committed outbox rows when no external CDC
// bridge is configured (document_queue.DocumentOutboxRelay).
type DocumentOutboxRelay struct {
	Jobs        *DocumentJobs
	Transport   func() DocumentNotificationPublisher
	PollSeconds float64

	mutex  sync.Mutex
	stop   chan struct{}
	worker bool
}

// NewDocumentOutboxRelay builds the relay; transport is resolved lazily so tests
// and the runtime can swap it after construction.
func NewDocumentOutboxRelay(jobs *DocumentJobs, transport func() DocumentNotificationPublisher, pollSeconds float64) *DocumentOutboxRelay {
	if pollSeconds <= 0 {
		pollSeconds = 2
	}
	return &DocumentOutboxRelay{Jobs: jobs, Transport: transport, PollSeconds: pollSeconds}
}

func (r *DocumentOutboxRelay) transport() DocumentNotificationPublisher {
	if r.Transport == nil {
		return nil
	}
	return r.Transport()
}

// PublishOne turns one committed PENDING outbox row into a notification. It
// returns false (without error) for missing/already-published rows, a missing
// transport, or a broker failure (the row stays pending for the next pass).
func (r *DocumentOutboxRelay) PublishOne(ctx context.Context, eventID string) (bool, error) {
	transport := r.transport()
	if transport == nil {
		return false, nil
	}
	event, err := r.Jobs.OutboxEvent(ctx, eventID)
	if err != nil || event == nil || event.Status != "PENDING" {
		return false, err
	}
	notification, err := ValidateDocumentNotification(map[string]any{
		"schema": float64(DocumentEnvelopeSchema), "artifact_id": event.ArtifactID,
		"namespace": event.Namespace, "name": event.Name, "created_at": event.CreatedAt,
	})
	if err != nil {
		return false, err
	}
	if err := transport.SendDocumentNotification(ctx, notification); err != nil {
		if markErr := r.Jobs.MarkPublished(ctx, eventID, err); markErr != nil {
			return false, markErr
		}
		return false, nil
	}
	if err := r.Jobs.MarkPublished(ctx, eventID, nil); err != nil {
		return false, err
	}
	return true, nil
}

// PublishPending publishes the due outbox rows, oldest first.
func (r *DocumentOutboxRelay) PublishPending(ctx context.Context, limit int) (int, error) {
	if r.transport() == nil {
		return 0, nil
	}
	events, err := r.Jobs.PendingOutbox(ctx, limit)
	if err != nil {
		return 0, err
	}
	published := 0
	for _, event := range events {
		ok, err := r.PublishOne(ctx, event.ID)
		if err != nil {
			return published, err
		}
		if ok {
			published++
		}
	}
	return published, nil
}

// Start launches the background relay loop.
func (r *DocumentOutboxRelay) Start() {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.worker {
		return
	}
	r.worker = true
	stop := make(chan struct{})
	r.stop = stop
	go func() {
		interval := time.Duration(r.PollSeconds * float64(time.Second))
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				if _, err := r.PublishPending(context.Background(), 50); err != nil {
					log.Printf("Document outbox relay pass failed error_type=%s", errorTypeName(err))
				}
			}
		}
	}()
}

// Stop terminates the background relay loop (idempotent).
func (r *DocumentOutboxRelay) Stop() {
	r.mutex.Lock()
	stop := r.stop
	r.stop = nil
	r.worker = false
	r.mutex.Unlock()
	if stop != nil {
		close(stop)
	}
}

// DocumentJobDispatcher accepts uploads by committing MySQL; delivery is
// separate (document_queue.DocumentJobDispatcher).
type DocumentJobDispatcher struct {
	Jobs      *DocumentJobs
	Transport DocumentNotificationPublisher
	Mode      string
	Relay     *DocumentOutboxRelay
	Settings  DocumentQueueSettings
}

// NewDocumentJobDispatcher reads the bridge mode and queue settings. A nil
// transport is allowed: relay mode then keeps notifications pending.
func NewDocumentJobDispatcher(stores *Stores, transport DocumentNotificationPublisher) (*DocumentJobDispatcher, error) {
	mode, err := DocumentBridgeModeFromEnv()
	if err != nil {
		return nil, err
	}
	settings, err := DocumentQueueSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	dispatcher := &DocumentJobDispatcher{
		Jobs:      stores.DocumentJobs,
		Transport: transport,
		Mode:      mode,
		Settings:  settings,
	}
	if mode == "relay" {
		dispatcher.Relay = NewDocumentOutboxRelay(stores.DocumentJobs, func() DocumentNotificationPublisher {
			return dispatcher.Transport
		}, 2)
		if transport == nil {
			log.Printf("Document outbox relay has no broker; uploads stay queued until one is configured")
		}
	}
	return dispatcher, nil
}

// Submit commits the durable job then best-effort publishes its notification.
func (d *DocumentJobDispatcher) Submit(ctx context.Context, namespace, name string, content []byte, idempotencyKey *string) (DocumentAdmission, error) {
	admission, err := d.Jobs.Submit(ctx, namespace, name, content, idempotencyKey)
	if err != nil {
		return DocumentAdmission{}, err
	}
	if d.Relay != nil && !admission.Reused && admission.EventID != nil {
		if _, publishErr := d.Relay.PublishOne(ctx, *admission.EventID); publishErr != nil {
			log.Printf("Document notification deferred error_type=%s", errorTypeName(publishErr))
		}
	}
	return admission, nil
}

// RunOnce is a no-op hook kept for dispatcher symmetry.
func (d *DocumentJobDispatcher) RunOnce() int { return 0 }

// Start starts the relay loop when relay mode is active.
func (d *DocumentJobDispatcher) Start() {
	if d.Relay != nil {
		d.Relay.Start()
	}
}

// Stop stops the relay loop.
func (d *DocumentJobDispatcher) Stop() {
	if d.Relay != nil {
		d.Relay.Stop()
	}
}

// Close stops the relay and closes the publisher.
func (d *DocumentJobDispatcher) Close() error {
	d.Stop()
	if d.Transport != nil {
		return d.Transport.Close()
	}
	return nil
}
