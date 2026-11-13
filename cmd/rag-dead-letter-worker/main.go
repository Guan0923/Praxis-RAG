// Command rag-dead-letter-worker records broker retry exhaustion as a durable
// terminal state. It never re-runs inference and never re-enqueues: a
// dead-lettered document job becomes "failed" in document_jobs and a dead
// lettered search task becomes failed in search_tasks, which clients observe
// through the normal status endpoints.
//
// Usage:
//
//	rag-dead-letter-worker --kind search|document|cache
//
// The consumer listens on an explicit Kafka dead-letter topic
// (RAG_DOCUMENT_DLQ_TOPIC / RAG_SEARCH_DLQ_TOPIC / RAG_CACHE_DLQ_TOPIC) with an
// independent RAG_*_DLQ_GROUP. Document/search dead letters ack only after the
// durable terminal outcome is saved (or the task is already absent/terminal); a
// live lease or an uncommitted journal publication returns false so the record
// stays uncommitted and is redelivered. Malformed document/search dead-letter
// payloads are logged by safe category and left unacked.
//
// Cache changes are derived state: the affected keys stay dirty in MySQL
// (cache_revisions) and the cache worker's compensation pass converges once the
// cause is fixed, so a cache dead letter is recorded and acknowledged without
// touching any business state.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/documentworker"
	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/searchworker"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// Exit codes: 0 graceful stop, 1 runtime failure, 2 configuration/usage error.
const (
	exitOK      = 0
	exitFailure = 1
	exitConfig  = 2

	shutdownTimeout = 10 * time.Second
)

// deadLetterGroupPattern is the Kafka consumer-group character set.
var deadLetterGroupPattern = regexp.MustCompile(`^[A-Za-z0-9._-]+$`)

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

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(exitOK)
		}
		fmt.Fprintf(os.Stderr, "rag-dead-letter-worker: %v\n", err)
		var usage *usageError
		if errors.As(err, &usage) {
			os.Exit(exitConfig)
		}
		os.Exit(exitFailure)
	}
	os.Exit(exitOK)
}

func run(args []string) error {
	logger, err := logging.Setup()
	if err != nil {
		return configError(err)
	}
	kind, err := parseKind(args)
	if err != nil {
		return err
	}
	if _, err := config.LoadStartupConfig(); err != nil {
		return configError(err)
	}
	dataRoot, err := dataRootFromEnv()
	if err != nil {
		return err
	}
	stateSettings, err := state.SettingsFromEnv(dataRoot)
	if err != nil {
		return configError(err)
	}
	mqSettings, err := mq.SettingsFromEnv()
	if err != nil {
		return configError(err)
	}
	// Validate the dead-letter consumer groups before the bootstrap requirement,
	// so a misconfigured group is reported first.
	target, err := deadLetterTarget(kind, mqSettings)
	if err != nil {
		return err
	}
	if !mqSettings.Configured() {
		return usagef("RAG_KAFKA_BOOTSTRAP_SERVERS is required to run the dead-letter worker")
	}

	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		return err
	}
	defer func() { _ = stores.Close() }()

	var handler mq.Handler
	var closeWorker func() error
	switch kind {
	case "search":
		worker, err := searchworker.New(searchworker.Options{
			Tasks:  stores.SearchTasks,
			Logger: logger,
		})
		if err != nil {
			return err
		}
		handler = worker.DeadLetterHandler()
		closeWorker = worker.Close
	case "document":
		worker, err := documentworker.New(documentworker.Options{
			Jobs:   stores.DocumentJobs,
			Logger: logger,
		})
		if err != nil {
			return err
		}
		handler = worker.DeadLetterHandler()
	case "cache":
		// Cache refreshes are derived state with their own MySQL dirty-key
		// retry water line; a dead letter is only recorded, never replayed, and
		// the delivery is always acknowledged so poison records cannot loop.
		handler = cacheDeadLetterHandler(logger)
	default:
		return usagef(`--kind must be "search", "document" or "cache"`)
	}

	consumerConfig := mq.DefaultConsumerConfig(target.topic, target.group)
	// Never ack exhausted deliveries here: a dead letter that cannot be
	// recorded yet (live lease, transient storage error) must stay retryable.
	consumerConfig.MaxAttempts = 0
	consumer, err := mq.NewKafkaPullConsumer(mqSettings, consumerConfig, handler)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	logger.Info("Praxis-RAG dead-letter worker starting",
		"version", version.Version,
		"kind", kind,
		"topic", target.topic,
		"group", target.group,
		"source_group", target.sourceGroup,
	)
	if err := consumer.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	logger.Info("Praxis-RAG dead-letter worker stopping", "kind", kind)

	stopErr := consumer.Stop()
	var workerErr error
	if closeWorker != nil {
		workerErr = closeWorker()
	}
	if stopErr != nil {
		return stopErr
	}
	return workerErr
}

type consumerTarget struct {
	topic       string
	group       string
	sourceGroup string
}

func parseKind(args []string) (string, error) {
	flags := flag.NewFlagSet("rag-dead-letter-worker", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	kind := flags.String("kind", "", "dead-letter source kind: search, document or cache")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return "", err
		}
		return "", &usageError{err: err}
	}
	if flags.NArg() > 0 {
		return "", usagef("unexpected arguments: %s", strings.Join(flags.Args(), " "))
	}
	value := strings.ToLower(strings.TrimSpace(*kind))
	if value != "search" && value != "document" && value != "cache" {
		return "", usagef(`--kind is required and must be "search", "document" or "cache"`)
	}
	return value, nil
}

func deadLetterTarget(kind string, settings mq.Settings) (consumerTarget, error) {
	sources := map[string]string{
		"document": settings.DocumentGroup,
		"search":   settings.SearchGroup,
		"cache":    settings.CacheGroup,
	}
	topics := map[string]string{
		"document": settings.DocumentDLQTopic,
		"search":   settings.SearchDLQTopic,
		"cache":    settings.CacheDLQTopic,
	}
	cacheGroup := strings.TrimSpace(settings.CacheDLQGroup)
	if cacheGroup == "" {
		cacheGroup = strings.TrimSpace(envOr("RAG_CACHE_DLQ_GROUP", defaultDeadLetterGroup("cache")))
	}
	groups := map[string]string{
		"document": strings.TrimSpace(envOr("RAG_DOCUMENT_DLQ_GROUP", defaultDeadLetterGroup("document"))),
		"search":   strings.TrimSpace(envOr("RAG_SEARCH_DLQ_GROUP", defaultDeadLetterGroup("search"))),
		"cache":    cacheGroup,
	}
	source, ok := sources[kind]
	if !ok {
		return consumerTarget{}, usagef(`--kind is required and must be "search", "document" or "cache"`)
	}
	topic := strings.TrimSpace(topics[kind])
	if topic == "" {
		return consumerTarget{}, usagef("RAG_%s_DLQ_TOPIC must not be empty", strings.ToUpper(kind))
	}
	// The three dead-letter groups must be valid and mutually independent: two
	// DLQ consumers sharing a group would split one topic's records.
	if !deadLetterGroupPattern.MatchString(groups["document"]) ||
		!deadLetterGroupPattern.MatchString(groups["search"]) ||
		!deadLetterGroupPattern.MatchString(groups["cache"]) ||
		groups["document"] == groups["search"] ||
		groups["document"] == groups["cache"] ||
		groups["search"] == groups["cache"] {
		return consumerTarget{}, usagef("Dead-letter consumer groups must be valid, independent groups")
	}
	reserved := map[string]struct{}{
		settings.DocumentGroup: {},
		settings.SearchGroup:   {},
		settings.CacheGroup:    {},
	}
	for _, group := range groups {
		if _, clash := reserved[group]; clash {
			return consumerTarget{}, usagef("Dead-letter consumer groups must be valid, independent groups")
		}
	}
	if !deadLetterGroupPattern.MatchString(source) {
		return consumerTarget{}, usagef("Invalid dead-letter source consumer group")
	}
	return consumerTarget{topic: topic, group: groups[kind], sourceGroup: source}, nil
}

// cacheDeadLetterHandler drains the cache dead-letter topic. The payload is
// recorded by category (table/op are locator metadata, never row content) and
// acknowledged; cache keys keep their dirty revision row in MySQL, so the cache
// worker's compensation pass converges once the underlying cause is fixed.
func cacheDeadLetterHandler(logger *slog.Logger) mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		change, err := mq.DecodeCacheChange(message.Body)
		if err != nil && !state.IgnorableCDCChange(err) {
			logger.Warn("cache dead letter recorded",
				"topic", message.Topic,
				"partition", message.Partition,
				"offset", message.Offset,
				"bytes", len(message.Body),
				"reason", "undecodable payload")
			return true
		}
		logger.Warn("cache dead letter recorded",
			"topic", message.Topic,
			"partition", message.Partition,
			"offset", message.Offset,
			"table", change.Table,
			"op", change.Op)
		return true
	}
}

// defaultDeadLetterGroup mirrors the shared contract defaults.
func defaultDeadLetterGroup(kind string) string {
	switch kind {
	case "document":
		return "praxis_document_dead_letters"
	case "cache":
		return mq.DefaultCacheDLQGroup
	default:
		return "praxis_search_dead_letters"
	}
}
func dataRootFromEnv() (string, error) {
	raw := strings.TrimSpace(os.Getenv("RAG_SERVICE_DATA_ROOT"))
	if raw == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", usagef("Cannot resolve the home directory for RAG_SERVICE_DATA_ROOT")
		}
		raw = filepath.Join(home, ".praxis-rag", "data")
	}
	return config.ExpandPath(raw), nil
}

func envOr(name, fallback string) string {
	if value, ok := os.LookupEnv(name); ok {
		return value
	}
	return fallback
}
