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
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// maintenanceIntervalEnv optionally overrides the maintenance cadence. Python's
// upload recovery task runs every 10 seconds; the sweep/GC work rides on it.
const maintenanceIntervalEnv = "RAG_MAINTENANCE_INTERVAL_SECONDS"

// defaultMaintenanceInterval matches Python's 10 second recovery loop.
const defaultMaintenanceInterval = 10 * time.Second

// backgroundOptions configure Service.StartBackground. They exist so tests can
// use a fast cadence without touching the environment.
type backgroundOptions struct {
	interval time.Duration
	initial  bool
}

// BackgroundOption adjusts StartBackground.
type BackgroundOption func(*backgroundOptions)

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
	service  *Service
	interval time.Duration
	cancel   context.CancelFunc
	done     chan struct{}
	once     sync.Once
}

var (
	backgroundMu         sync.Mutex
	backgroundsByService = map[*Service]*serviceBackground{}
)

// StartBackground launches the periodic maintenance loop (upload
// recovery/cleanup, search expiry, document job sweep/GC, snapshot/artifact
// purges). Delivery itself is owned by Debezium: the application writes MySQL and
// Debezium publishes the identifier-only notifications, so startup never depends
// on broker availability. It also starts the cache synchronizer, whose rebuilds
// are driven by the cache consumer.
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

	background := &serviceBackground{
		service:  s,
		interval: configuration.interval,
		done:     make(chan struct{}),
	}
	backgroundCtx, cancel := context.WithCancel(context.Background())
	background.cancel = cancel

	logging.FromContext(ctx).Info("Task notifications delivered by Debezium")
	if s.stores.CacheSync != nil && s.stores.CacheSync.Enabled() {
		s.stores.CacheSync.Start()
	}

	backgroundMu.Lock()
	backgroundsByService[s] = background
	backgroundMu.Unlock()

	go background.run(backgroundCtx, configuration.initial)
	return nil
}

// StopBackground stops the maintenance loop. It is idempotent and safe to call
// before Stores.Close (which owns final transport shutdown).
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

// maintenancePass runs one recovery/cleanup sweep. Every step is best-effort:
// failures are logged by category and never stop the loop.
//
// Requeueing a stranded or expired-lease task increments its schedule_version
// inside MySQL; Debezium emits the replacement notification, so no outbox relay
// or broker publish happens here.
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
	if s.stores.DocumentJobs != nil {
		if _, err := s.stores.DocumentJobs.Sweep(ctx, 50); err != nil {
			logBackgroundFailure(ctx, "document job sweep", err)
		}
		if _, err := s.stores.DocumentJobs.GCJobs(ctx, nil); err != nil {
			logBackgroundFailure(ctx, "document job GC", err)
		}
	}
	if s.stores.Artifacts != nil {
		if _, err := s.stores.Artifacts.PurgeExpired(ctx, 100); err != nil {
			logBackgroundFailure(ctx, "artifact purge", err)
		}
	}
	if _, err := s.stores.BackfillContentObjects(ctx, 100); err != nil {
		logBackgroundFailure(ctx, "content backfill", err)
	}
	if _, err := s.stores.PurgeUnreferencedContent(ctx, state.ContentObjectGraceSeconds, 100); err != nil {
		logBackgroundFailure(ctx, "content purge", err)
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
