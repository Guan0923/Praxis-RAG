// Command rag-cache-worker consumes the cache-change topic and keeps the Redis
// read-model cache convergent with MySQL.
//
// It is the Go counterpart of the retired cache_outbox relay. Debezium captures
// the business tables that back the cached read models and routes identifier-only
// row changes to the cache topic; the worker maps each change to the affected
// cache kinds/scopes, bumps their revisions in MySQL and rebuilds the redacted
// views from MySQL into Redis. A periodic compensation pass retries keys whose
// Redis write failed, so a Redis outage only delays convergence.
package main

import (
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/cacheconsumer"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
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
	// defaultMaxAttempts bounds broker redeliveries before OnExhausted records
	// the exhausted message on the cache dead-letter topic. Override with
	// RAG_CACHE_MAX_ATTEMPTS.
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
	_ cacheconsumer.Refresher = (*state.CacheSynchronizer)(nil)
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "rag-cache-worker: %v\n", err)
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
	stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return configError(err)
	}
	mqSettings, err := mq.SettingsFromEnv()
	if err != nil {
		return configError(err)
	}
	if !mqSettings.Configured() {
		return usagef("RAG_KAFKA_BOOTSTRAP_SERVERS is required to run the cache worker")
	}
	maxAttempts, err := positiveIntEnv("RAG_CACHE_MAX_ATTEMPTS", defaultMaxAttempts)
	if err != nil {
		return configError(err)
	}
	concurrency, err := workerConcurrencyEnv("RAG_CACHE_WORKER_CONCURRENCY")
	if err != nil {
		return configError(err)
	}
	recoveryInterval, err := positiveSecondsEnv("RAG_CACHE_WORKER_RECOVER_SECONDS", cacheconsumer.DefaultRecoveryInterval)
	if err != nil {
		return configError(err)
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

	processor, err := cacheconsumer.New(cacheconsumer.Options{
		Refresher:        stores.CacheSync,
		Decoder:          decodeCacheChange,
		Logger:           logger,
		RecoveryInterval: recoveryInterval,
	})
	if err != nil {
		return err
	}

	consumerConfig := mq.DefaultConsumerConfig(mqSettings.CacheTopic, mqSettings.CacheGroup)
	consumerConfig.MaxAttempts = maxAttempts
	consumerConfig.Concurrency = concurrency
	consumer, err := mq.NewKafkaPullConsumer(mqSettings, consumerConfig, processor.MessageHandler())
	if err != nil {
		return err
	}
	consumer.OnExhausted = func(ctx context.Context, message mq.ReceivedMessage) error {
		// The raw payload is already on the cache dead-letter topic. Cache
		// refreshes are derived state: acknowledge the delivery and let the
		// dirty-key compensation pass converge once the cause is fixed.
		logger.Warn("cache change exhausted; recorded on the dead-letter topic",
			"topic", message.Topic, "partition", message.Partition, "offset", message.Offset)
		return nil
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	recoveryDone := make(chan struct{})
	go func() {
		defer close(recoveryDone)
		_ = processor.RunRecovery(ctx)
	}()
	logger.Info("Praxis-RAG cache worker starting",
		"version", version.Version,
		"topic", consumerConfig.Topic,
		"group", consumerConfig.Group,
		"max_attempts", consumerConfig.MaxAttempts,
		"concurrency", consumerConfig.Concurrency,
	)
	runErr := processor.RunConsumer(ctx, consumer)
	processor.Stop()
	<-recoveryDone
	logger.Info("Praxis-RAG cache worker stopping")
	return runErr
}

// decodeCacheChange adapts the transport decoder to the consumer's narrow change
// type. Ignorable records (tombstones, DDL, tables outside the captured set) are
// reported with state.ErrIgnorableCDCChange and acknowledged.
func decodeCacheChange(body []byte) (cacheconsumer.Change, error) {
	change, err := mq.DecodeCacheChange(body)
	if err != nil {
		return cacheconsumer.Change{}, err
	}
	return cacheconsumer.Change{
		Table:  change.Table,
		Op:     change.Op,
		Before: change.Before,
		After:  change.After,
	}, nil
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

// workerConcurrencyEnv parses the number of handler goroutines processing cache
// changes in parallel; the shared Kafka queue settings own the bounds.
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

// positiveSecondsEnv parses a positive duration expressed in seconds.
func positiveSecondsEnv(name string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of seconds", name)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
