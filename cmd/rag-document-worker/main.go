// Command rag-document-worker consumes durable document-ingest jobs: it leases
// one committed job by schedule_version, keeps the SQL lease alive while the
// runtime parses and indexes it, reconciles committed journal publications after
// a crash, and records retryable or terminal failures.
//
// It is the only document consumer. Debezium captures committed document_jobs
// rows (INSERT plus schedule_version bumps) and routes identifier-only
// notifications to RAG_DOCUMENT_TOPIC; the worker joins RAG_DOCUMENT_GROUP and
// reads the job parameters back from MySQL. RAG_KAFKA_BOOTSTRAP_SERVERS is
// required. rag-service never parses documents itself.
//
// It is the Go counterpart of rag_mcp.document_worker.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// Exit codes: 0 graceful stop, 1 runtime failure, 2 configuration/usage error.
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2

	// defaultMaxAttempts bounds broker redeliveries before OnExhausted records a
	// durable terminal state. Override with RAG_DOCUMENT_BROKER_MAX_RETRIES
	// (config-file friendly) or RAG_DOCUMENT_MAX_ATTEMPTS.
	defaultMaxAttempts = 5
	shutdownTimeout    = 10 * time.Second
)

// usageError marks configuration/usage failures so main can exit 2.
type usageError struct{ err error }

// Error implements error.
func (e *usageError) Error() string { return e.err.Error() }

// Unwrap exposes the cause.
func (e *usageError) Unwrap() error { return e.err }

func configError(err error) error { return &usageError{err: err} }

func usagef(format string, args ...any) error {
	return &usageError{err: fmt.Errorf(format, args...)}
}

var (
	_ documentworker.Ingester         = (*runtime.Service)(nil)
	_ documentworker.JournalRecoverer = (*runtime.Service)(nil)
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rag-document-worker: %v\n", err)
		var usage *usageError
		if errors.As(err, &usage) {
			os.Exit(exitConfig)
		}
		os.Exit(exitFailure)
	}
	os.Exit(exitOK)
}

func run() error {
	logger, err := logging.Setup()
	if err != nil {
		return configError(err)
	}
	if _, err := config.LoadStartupConfig(); err != nil {
		return configError(err)
	}
	serviceSettings, err := config.ServiceSettingsFromEnv()
	if err != nil {
		return configError(err)
	}
	ragConfig, err := config.RagConfigFromEnv()
	if err != nil {
		return configError(err)
	}
	if ragConfig.Backend != config.BackendQdrant {
		return usagef("RAG_BACKEND=qdrant is required to run the document worker (got %q)", ragConfig.Backend)
	}
	stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return configError(err)
	}
	mqSettings, err := mq.SettingsFromEnv()
	if err != nil {
		return configError(err)
	}
	maxAttempts, err := positiveIntEnv(defaultMaxAttempts, "RAG_DOCUMENT_BROKER_MAX_RETRIES", "RAG_DOCUMENT_MAX_ATTEMPTS")
	if err != nil {
		return configError(err)
	}
	concurrency, err := workerConcurrencyEnv("RAG_DOCUMENT_WORKER_CONCURRENCY")
	if err != nil {
		return configError(err)
	}
	recoverInterval, err := durationSecondsEnv("RAG_DOCUMENT_WORKER_RECOVER_SECONDS", 30*time.Second)
	if err != nil {
		return configError(err)
	}
	if !mqSettings.Configured() {
		return usagef("RAG_KAFKA_BOOTSTRAP_SERVERS is required to run the document worker")
	}

	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		return err
	}
	defer func() { _ = stores.Close() }()

	cache, err := redisstate.NewFromEnv()
	if err != nil {
		return configError(err)
	}
	if err := redisstate.Attach(stores, cache); err != nil {
		return err
	}
	stores.CacheSync.Start()

	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	options := documentworker.Options{
		Jobs:            stores.DocumentJobs,
		Ingester:        service,
		JournalRecovery: service,
		Logger:          logger,
	}
	worker, err := documentworker.New(options)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()
	// Record worker liveness so the Console monitor can prove this independent
	// worker actually runs; the configured deployment mode alone proves nothing.
	heartbeatInterval := documentworker.DefaultWorkerHeartbeatInterval()
	stopHeartbeat := documentworker.StartHeartbeat(
		workerCtx, stores.Heartbeats, state.HeartbeatRoleDocumentWorker, heartbeatInterval, logger)
	defer stopHeartbeat()

	logger.Info("Praxis-RAG document worker starting",
		"version", version.Version,
		"topic", mqSettings.DocumentTopic,
		"group", mqSettings.DocumentGroup,
		"max_attempts", maxAttempts,
		"concurrency", concurrency,
		"recover_seconds", recoverInterval.Seconds(),
		"heartbeat_seconds", heartbeatInterval.Seconds(),
		"backend", string(ragConfig.Backend),
	)

	go recoveryLoop(workerCtx, worker, recoverInterval, logger)
	if err := worker.Recover(ctx); err != nil {
		logger.Warn("document worker startup recovery failed", logging.ErrorAttrs(err)...)
	}
	consumerConfig := mq.DefaultConsumerConfig(mqSettings.DocumentTopic, mqSettings.DocumentGroup)
	consumerConfig.MaxAttempts = maxAttempts
	consumerConfig.Concurrency = concurrency
	consumer, err := mq.NewKafkaPullConsumer(mqSettings, consumerConfig, worker.MessageHandler())
	if err != nil {
		return err
	}
	consumer.OnExhausted = worker.ExhaustedHandler()
	if err := worker.RunConsumer(ctx, consumer); err != nil {
		return err
	}
	logger.Info("Praxis-RAG document worker stopping")
	return shutdown(service)
}

// recoveryLoop runs the periodic maintenance pass: Sweep requeues stranded jobs
// by incrementing schedule_version inside MySQL, and Debezium emits the
// replacement notification. No outbox row or broker publish happens here.
func recoveryLoop(ctx context.Context, worker *documentworker.Processor, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := worker.Recover(ctx); err != nil {
				logger.Warn("document worker maintenance pass failed", logging.ErrorAttrs(err)...)
			}
		}
	}
}

func shutdown(service *runtime.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return service.Close(ctx)
}

func positiveIntEnv(fallback int, names ...string) (int, error) {
	for _, name := range names {
		raw := strings.TrimSpace(os.Getenv(name))
		if raw == "" {
			continue
		}
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 {
			return 0, fmt.Errorf("%s must be a non-negative integer", name)
		}
		return value, nil
	}
	return fallback, nil
}

// workerConcurrencyEnv parses the number of handler goroutines processing
// documents in parallel; the shared Kafka queue settings own the bounds.
func workerConcurrencyEnv(name string) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return mq.DefaultConsumerConcurrency, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 1 || value > mq.MaxConsumerConcurrency {
		return 0, fmt.Errorf("%s must be an integer between 1 and %d", name, mq.MaxConsumerConcurrency)
	}
	return value, nil
}

func durationSecondsEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of seconds", name)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
