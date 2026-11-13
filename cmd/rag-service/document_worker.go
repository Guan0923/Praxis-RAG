// In-process document worker: a broker-less deployment embeds the outbox-backed
// document worker in the HTTP service so admitted uploads are parsed without
// running a second binary. Kafka deployments keep the external
// rag-document-worker instead; RAG_DOCUMENT_WORKER_INPROCESS selects the mode.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

const (
	// defaultDocumentWorkerPoll is the embedded outbox poll cadence.
	defaultDocumentWorkerPoll = 500 * time.Millisecond
	// documentWorkerShutdownTimeout bounds the embedded worker stop so a stuck
	// ingestion cannot delay service shutdown indefinitely.
	documentWorkerShutdownTimeout = 5 * time.Second
)

// resolveDocumentWorkerMode parses RAG_DOCUMENT_WORKER_INPROCESS. The default
// "auto" embeds the worker exactly when no Kafka broker is configured; "on"
// requires an empty RAG_KAFKA_BOOTSTRAP_SERVERS so the same outbox rows are never
// consumed twice, and "off" leaves parsing to the external rag-document-worker.
func resolveDocumentWorkerMode(raw string, kafkaConfigured bool) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "", "auto":
		return !kafkaConfigured, nil
	case "1", "true", "yes", "on":
		if kafkaConfigured {
			return false, fmt.Errorf("RAG_DOCUMENT_WORKER_INPROCESS requires RAG_KAFKA_BOOTSTRAP_SERVERS to be empty; run rag-document-worker instead")
		}
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, fmt.Errorf("RAG_DOCUMENT_WORKER_INPROCESS must be auto, on or off")
	}
}

// documentWorkerPollInterval parses RAG_DOCUMENT_WORKER_POLL_SECONDS: a positive
// float number of seconds used as the embedded outbox poll cadence, defaulting to
// 0.5s. An invalid value is a startup error naming the variable.
func documentWorkerPollInterval(raw string) (time.Duration, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultDocumentWorkerPoll, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, fmt.Errorf("RAG_DOCUMENT_WORKER_POLL_SECONDS must be a positive number of seconds")
	}
	return time.Duration(seconds * float64(time.Second)), nil
}

// embeddedDocumentWorker owns the in-process Processor started with the HTTP
// service. A nil value is valid and makes stop a no-op.
type embeddedDocumentWorker struct {
	worker *documentworker.Processor
	logger *slog.Logger
	cancel context.CancelFunc
	done   chan error
	// stopHeartbeat stops the worker liveness writer. The heartbeat row is what
	// the Console monitor reads to prove the worker actually runs.
	stopHeartbeat func()
}

// startEmbeddedDocumentWorker runs the outbox-backed worker in a goroutine and
// returns its lifecycle handle. It returns (nil, nil) when the document job store
// is unavailable: the HTTP service still starts, only automatic parsing is
// skipped with a warning.
func startEmbeddedDocumentWorker(
	stores *state.Stores,
	service *runtime.Service,
	logger *slog.Logger,
	pollInterval time.Duration,
) (*embeddedDocumentWorker, error) {
	if stores == nil || stores.DocumentJobs == nil {
		logger.Warn("in-process document worker skipped: document job store is unavailable")
		return nil, nil
	}
	worker, err := documentworker.New(documentworker.WithOutboxReceiver(documentworker.Options{
		Jobs:            stores.DocumentJobs,
		Ingester:        service,
		JournalRecovery: service,
		Logger:          logger,
		PollInterval:    pollInterval,
	}))
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	// Record a liveness heartbeat next to the Run loop: the monitoring page
	// must be able to tell a stopped in-process worker from a configured one.
	heartbeatInterval := documentworker.DefaultWorkerHeartbeatInterval()
	stopHeartbeat := documentworker.StartHeartbeat(
		ctx, stores.Heartbeats, state.HeartbeatRoleDocumentWorker, heartbeatInterval, logger)
	embedded := &embeddedDocumentWorker{
		worker:        worker,
		logger:        logger,
		cancel:        cancel,
		done:          make(chan error, 1),
		stopHeartbeat: stopHeartbeat,
	}
	go func() { embedded.done <- worker.Run(ctx) }()
	logger.Info("Praxis-RAG in-process document worker started",
		"poll_seconds", pollInterval.Seconds(),
		"heartbeat_seconds", heartbeatInterval.Seconds())
	return embedded, nil
}

// stop cancels the Run loop, waits for it to return and closes the processor. A
// context deadline only logs a warning: shutdown stays bounded.
func (w *embeddedDocumentWorker) stop(ctx context.Context) {
	if w == nil {
		return
	}
	if w.stopHeartbeat != nil {
		w.stopHeartbeat()
	}
	w.cancel()
	select {
	case err := <-w.done:
		if err != nil {
			w.logger.Warn("in-process document worker run failed", "error_type", errorTypeName(err))
		}
	case <-ctx.Done():
		w.logger.Warn("in-process document worker did not stop before the shutdown deadline")
	}
	if err := w.worker.Close(); err != nil {
		w.logger.Warn("in-process document worker close failed", "error_type", errorTypeName(err))
	}
}

// errorTypeName renders an error type for structured logs without exposing its
// (possibly internal) message.
func errorTypeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
