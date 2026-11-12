// Command rag-search-worker consumes queued full-request search envelopes from
// the broker, runs retrieval, captures expiring artifact references, and stores
// the immutable result plus the durable terminal task state.
//
// It is the Go counterpart of rag_mcp.search_worker. The worker owns no HTTP
// surface: it shares the state store, the Redis transient/snapshot cache and the
// runtime retrieval stack with rag-service, then consumes the search topic.
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
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/searchworker"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// Exit codes: 0 graceful stop, 1 runtime failure, 2 configuration/usage error.
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2
)

const (
	// defaultMaxAttempts bounds broker redeliveries before OnExhausted records a
	// durable terminal state. Override with RAG_SEARCH_MAX_ATTEMPTS.
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
	_ searchworker.Searcher = (*runtime.Service)(nil)
	_ searchworker.Capturer = (*searchrefs.Capture)(nil)
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rag-search-worker: %v\n", err)
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
		return usagef("RAG_BACKEND=qdrant is required to run the search worker (got %q)", ragConfig.Backend)
	}
	stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return configError(err)
	}
	mqSettings, err := mq.SettingsFromEnv()
	if err != nil {
		return configError(err)
	}
	if !mqSettings.Configured() {
		return usagef("RAG_KAFKA_BOOTSTRAP_SERVERS is required to run the search worker")
	}
	maxAttempts, err := positiveIntEnv("RAG_SEARCH_MAX_ATTEMPTS", defaultMaxAttempts)
	if err != nil {
		return configError(err)
	}
	concurrency, err := workerConcurrencyEnv("RAG_SEARCH_WORKER_CONCURRENCY")
	if err != nil {
		return configError(err)
	}

	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		return err
	}
	defer func() { _ = stores.Close() }()
	logger := newLogger()

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
	worker, err := searchworker.New(searchworker.Options{
		Tasks:    stores.SearchTasks,
		Searcher: service,
		Capturer: searchrefs.New(stores, logger),
		Logger:   logger,
	})
	if err != nil {
		return err
	}

	consumerConfig := mq.DefaultConsumerConfig(mqSettings.SearchTopic, mqSettings.SearchGroup)
	consumerConfig.MaxAttempts = maxAttempts
	consumerConfig.Concurrency = concurrency
	consumer, err := mq.NewKafkaPullConsumer(mqSettings, consumerConfig, worker.MessageHandler())
	if err != nil {
		return err
	}
	consumer.OnExhausted = worker.ExhaustedHandler()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("Praxis-RAG search worker starting",
		"version", version.Version,
		"topic", consumerConfig.Topic,
		"group", consumerConfig.Group,
		"max_attempts", consumerConfig.MaxAttempts,
		"concurrency", consumerConfig.Concurrency,
		"backend", string(ragConfig.Backend),
	)
	if err := worker.RunConsumer(ctx, consumer); err != nil {
		return err
	}
	logger.Info("Praxis-RAG search worker stopping")
	return shutdown(worker, service)
}

func shutdown(worker *searchworker.Processor, service *runtime.Service) error {
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

func positiveIntEnv(name string, fallback int) (int, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s must be a non-negative integer", name)
	}
	return value, nil
}

// workerConcurrencyEnv parses the number of handler goroutines processing
// searches in parallel; the shared Kafka queue settings own the bounds.
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
