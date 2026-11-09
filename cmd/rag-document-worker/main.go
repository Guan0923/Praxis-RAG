// Command rag-document-worker consumes durable document-ingest jobs: it leases
// one committed job, keeps the SQL lease alive while the runtime parses and
// indexes it, reconciles committed journal publications after a crash, and
// records retryable or terminal failures.
//
// Two modes are supported:
//   - broker mode (default; RAG_KAFKA_BOOTSTRAP_SERVERS set): pull the document topic
//     with the document consumer group;
//   - single-process outbox mode (RAG_DOCUMENT_WORKER_LOCAL=1, no endpoint): poll
//     the committed document_job_outbox rows directly, for development and
//     broker-less deployments.
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
	pollInterval, err := durationSecondsEnv("RAG_DOCUMENT_WORKER_POLL_SECONDS", 500*time.Millisecond)
	if err != nil {
		return configError(err)
	}
	recoverInterval, err := durationSecondsEnv("RAG_DOCUMENT_WORKER_RECOVER_SECONDS", 30*time.Second)
	if err != nil {
		return configError(err)
	}
	localMode := isTruthy(os.Getenv("RAG_DOCUMENT_WORKER_LOCAL"))
	if !mqSettings.Configured() && !localMode {
		return usagef("RAG_KAFKA_BOOTSTRAP_SERVERS is required unless RAG_DOCUMENT_WORKER_LOCAL=1 selects the single-process outbox mode")
	}

	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		return err
	}
	defer func() { _ = stores.Close() }()
	logger := newLogger()
	if mqSettings.Configured() && localMode {
		logger.Warn("RAG_DOCUMENT_WORKER_LOCAL is ignored because RAG_KAFKA_BOOTSTRAP_SERVERS is configured; using broker mode")
		localMode = false
	}

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
		PollInterval:    pollInterval,
	}
	if localMode {
		options.Receiver = documentworker.NewOutboxReceiver(stores.DocumentJobs)
	}
	worker, err := documentworker.New(options)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	workerCtx, cancelWorker := context.WithCancel(ctx)
	defer cancelWorker()

	mode := "broker"
	if localMode {
		mode = "outbox-local"
	}
	logger.Info("Praxis-RAG document worker starting",
		"version", version.Version,
		"mode", mode,
		"topic", mqSettings.DocumentTopic,
		"group", mqSettings.DocumentGroup,
		"max_attempts", maxAttempts,
		"recover_seconds", recoverInterval.Seconds(),
		"backend", string(ragConfig.Backend),
	)

	go recoveryLoop(workerCtx, worker, recoverInterval, logger)
	if localMode {
		if err := worker.Run(workerCtx); err != nil {
			return err
		}
	} else {
		if err := worker.Recover(ctx); err != nil {
			logger.Warn("document worker startup recovery failed", "error_type", errorTypeName(err))
		}
		consumerConfig := mq.DefaultConsumerConfig(mqSettings.DocumentTopic, mqSettings.DocumentGroup)
		consumerConfig.MaxAttempts = maxAttempts
		consumer, err := mq.NewKafkaPullConsumer(mqSettings, consumerConfig, worker.MessageHandler())
		if err != nil {
			return err
		}
		consumer.OnExhausted = worker.ExhaustedHandler()
		if err := worker.RunConsumer(ctx, consumer); err != nil {
			return err
		}
	}
	logger.Info("Praxis-RAG document worker stopping", "mode", mode)
	return shutdown(worker, service)
}

func recoveryLoop(ctx context.Context, worker *documentworker.Processor, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := worker.Recover(ctx); err != nil {
				logger.Warn("document worker maintenance pass failed", "error_type", errorTypeName(err))
			}
		}
	}
}

func shutdown(worker *documentworker.Processor, service *runtime.Service) error {
	ctx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	if err := worker.Close(); err != nil {
		return err
	}
	return service.Close(ctx)
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if isTruthy(os.Getenv("RAG_DEBUG_TRACE")) {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
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

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func errorTypeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
