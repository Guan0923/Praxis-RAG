package runtime

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// BackgroundPublisher publishes queued-search notifications. Document outbox
// events are delivered by Debezium independently of the application.
type BackgroundPublisher interface {
	state.SearchNotificationPublisher
}

// maintenanceIntervalEnv optionally overrides the maintenance cadence. Python's
// upload recovery task runs every 10 seconds; the sweep/GC work rides on it.
const maintenanceIntervalEnv = "RAG_MAINTENANCE_INTERVAL_SECONDS"

// defaultMaintenanceInterval matches Python's 10 second recovery loop.
const defaultMaintenanceInterval = 10 * time.Second

// backgroundOptions configure Service.StartBackground. They exist so tests can
// inject a fake broker and a fast cadence without touching the environment.
type backgroundOptions struct {
	publisher BackgroundPublisher
	interval  time.Duration
	initial   bool
}

// BackgroundOption adjusts StartBackground.
type BackgroundOption func(*backgroundOptions)

// WithBackgroundPublisher injects the broker adapter (tests or a worker main).
func WithBackgroundPublisher(publisher BackgroundPublisher) BackgroundOption {
	return func(options *backgroundOptions) { options.publisher = publisher }
}

// WithMaintenanceInterval overrides the periodic maintenance cadence.
func WithMaintenanceInterval(interval time.Duration) BackgroundOption {
	return func(options *backgroundOptions) { options.interval = interval }
}

// WithInitialMaintenancePass controls whether recovery runs before the loop.
func WithInitialMaintenancePass(enabled bool) BackgroundOption {
	return func(options *backgroundOptions) { options.initial = enabled }
}

// serviceBackground is the running background state attached to one Service.
// The Service struct is Lead-owned, so the state lives in a package registry
// keyed by the service pointer rather than in new struct fields.
type serviceBackground struct {
	service        *Service
	publisher      BackgroundPublisher
	closePublisher bool
	interval       time.Duration
	cancel         context.CancelFunc
	done           chan struct{}
	once           sync.Once
}

var (
	backgroundMu         sync.Mutex
	backgroundsByService = map[*Service]*serviceBackground{}
)

// StartBackground wires queued-search publishing and cache synchronization,
// and launches the periodic maintenance loop (upload recovery/cleanup, search expiry, document
// job sweep/GC, snapshot/artifact purges). It never blocks on broker
// availability: queued searches stay fail-closed without a publisher, while
// document admission only needs its MySQL transaction.
func (s *Service) StartBackground(ctx context.Context, options ...BackgroundOption) error {
	configuration := backgroundOptions{
		interval: defaultBackgroundInterval(),
		initial:  true,
	}
	for _, option := range options {
		option(&configuration)
	}
	backgroundMu.Lock()
	if _, exists := backgroundsByService[s]; exists {
		backgroundMu.Unlock()
		return nil
	}
	backgroundMu.Unlock()

	publisher := configuration.publisher
	closePublisher := false
	if publisher == nil {
		publisher = s.newBrokerPublisher()
		closePublisher = publisher != nil
	}
	background := &serviceBackground{
		service:        s,
		publisher:      publisher,
		closePublisher: closePublisher,
		interval:       configuration.interval,
		done:           make(chan struct{}),
	}
	backgroundCtx, cancel := context.WithCancel(context.Background())
	background.cancel = cancel

	// Search admission confirms broker acceptance through this publisher.
	if s.stores.SearchDispatcher != nil {
		s.stores.SearchDispatcher.Publisher = publisher
		s.stores.SearchDispatcher.Start()
	}
	logging.FromContext(ctx).Info("Document outbox delivery=debezium")
	if s.stores.CacheSync != nil && s.stores.CacheSync.Enabled() {
		s.stores.CacheSync.Start()
	}

	backgroundMu.Lock()
	backgroundsByService[s] = background
	backgroundMu.Unlock()

	go background.run(backgroundCtx, configuration.initial)
	return nil
}

// StopBackground stops cache synchronization and the maintenance loop. It is idempotent and
// safe to call before Stores.Close (which owns final transport shutdown).
func (s *Service) StopBackground() {
	backgroundMu.Lock()
	background, exists := backgroundsByService[s]
	if exists {
		delete(backgroundsByService, s)
	}
	backgroundMu.Unlock()
	if !exists || background == nil {
		return
	}
	background.once.Do(func() {
		background.cancel()
		<-background.done
		if s.stores.CacheSync != nil {
			s.stores.CacheSync.Stop()
		}
	})
}

// BackgroundRunning reports whether StartBackground has an active loop (tests
// and health wiring use it).
func (s *Service) BackgroundRunning() bool {
	backgroundMu.Lock()
	defer backgroundMu.Unlock()
	_, exists := backgroundsByService[s]
	return exists
}

// newBrokerPublisher resolves Kafka settings lazily. A missing bootstrap server
// list means "run without a broker" (nil, no error); an invalid configuration is
// logged and also degrades to nil so the service can start and report
// fail-closed submissions instead of crashing.
func (s *Service) newBrokerPublisher() BackgroundPublisher {
	settings, err := mq.SettingsFromEnv()
	if err != nil {
		slog.Warn("Kafka settings are invalid; queued searches stay fail-closed", logging.ErrorAttrs(err)...)
		return nil
	}
	if !settings.Configured() {
		slog.Warn("Kafka bootstrap servers are not configured; queued searches stay fail-closed")
		return nil
	}
	publisher, err := mq.NewPublisher(settings)
	if err != nil {
		slog.Warn("Kafka publisher unavailable; queued searches stay fail-closed", logging.ErrorAttrs(err)...)
		return nil
	}
	return publisher
}

// defaultBackgroundInterval reads the optional cadence override.
func defaultBackgroundInterval() time.Duration {
	raw := strings.TrimSpace(os.Getenv(maintenanceIntervalEnv))
	if raw == "" {
		return defaultMaintenanceInterval
	}
	seconds, err := strconv.ParseFloat(raw, 64)
	if err != nil || seconds <= 0 {
		slog.Warn("Ignoring invalid maintenance interval; using the default", "env", maintenanceIntervalEnv)
		return defaultMaintenanceInterval
	}
	return time.Duration(seconds * float64(time.Second))
}

// run executes the initial pass and then the periodic maintenance loop until the
// context is cancelled.
func (background *serviceBackground) run(ctx context.Context, initial bool) {
	defer close(background.done)
	defer background.publisherClose()
	if initial {
		background.service.maintenancePass(ctx)
	}
	if background.interval <= 0 {
		<-ctx.Done()
		return
	}
	ticker := time.NewTicker(background.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			background.service.maintenancePass(ctx)
		}
	}
}

// publisherClose closes the broker adapter this background owns. Injected
// publishers belong to the caller and are left open.
func (background *serviceBackground) publisherClose() {
	if background.closePublisher && background.publisher != nil {
		_ = background.publisher.Close()
	}
}

// maintenancePass runs one recovery/cleanup sweep. Every step is best-effort:
// failures are logged by category and never stop the loop.
func (s *Service) maintenancePass(ctx context.Context) {
	if s.stores == nil {
		return
	}
	select {
	case <-ctx.Done():
		return
	default:
	}
	s.recoverUploadsGlobal(ctx)
	if s.stores.SearchTasks != nil {
		if _, err := s.stores.SearchTasks.Expire(ctx, 20); err != nil {
			logBackgroundFailure(ctx, "search task expiry", err)
		}
	}
	if s.stores.SearchDispatcher != nil && s.stores.SearchDispatcher.Publisher != nil {
		if _, err := s.stores.SearchDispatcher.RunOnce(ctx); err != nil {
			logBackgroundFailure(ctx, "search outbox relay", err)
		}
	}
	if s.stores.DocumentJobs != nil {
		if _, err := s.stores.DocumentJobs.Sweep(ctx, 50); err != nil {
			logBackgroundFailure(ctx, "document job sweep", err)
		}
		if _, err := s.stores.DocumentJobs.GCJobs(ctx, nil); err != nil {
			logBackgroundFailure(ctx, "document job GC", err)
		}
		if _, err := s.stores.DocumentJobs.GCOutbox(ctx, nil); err != nil {
			logBackgroundFailure(ctx, "document outbox GC", err)
		}
	}
	if s.stores.Results != nil {
		if _, err := s.stores.Results.PurgeExpired(100); err != nil {
			logBackgroundFailure(ctx, "result snapshot purge", err)
		}
	}
	if s.stores.Artifacts != nil {
		if _, err := s.stores.Artifacts.PurgeExpired(ctx, 100); err != nil {
			logBackgroundFailure(ctx, "artifact purge", err)
		}
	}
}

// recoverUploadsGlobal runs the upload coordinator recovery/cleanup pass for
// every namespace (Python RagServiceRuntime.recover_uploads).
func (s *Service) recoverUploadsGlobal(ctx context.Context) {
	coordinator, err := s.uploadCoordinator()
	if err != nil {
		logBackgroundFailure(ctx, "upload coordinator", err)
		return
	}
	coordinator.Recover(ctx, s.indexStoreFactory, nil)
}

func logBackgroundFailure(ctx context.Context, stage string, err error) {
	if err == nil {
		return
	}
	logging.FromContext(ctx).Warn("Background maintenance "+stage+" failed", logging.ErrorAttrs(err)...)
}
