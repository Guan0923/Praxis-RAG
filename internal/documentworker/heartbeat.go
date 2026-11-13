package documentworker

import (
	"context"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// WorkerHeartbeatEnv overrides the worker heartbeat interval in seconds.
	// It is separate from RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS, which renews the
	// per-job lease; this interval only announces that the worker process is
	// alive so the Console monitoring page can tell it apart from a stopped one.
	WorkerHeartbeatEnv = "RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS"

	// defaultWorkerHeartbeatInterval is the heartbeat period used when the
	// environment override is absent or invalid.
	defaultWorkerHeartbeatInterval = 15 * time.Second

	// heartbeatWriteTimeout bounds one heartbeat write so a slow state database
	// cannot pin the background goroutine (or the initial startup write).
	heartbeatWriteTimeout = 5 * time.Second

	// maxWorkerInstanceLength matches the runtime_heartbeats.instance column.
	maxWorkerInstanceLength = 200
)

// HeartbeatRecorder records one worker liveness heartbeat. *state.RuntimeHeartbeats
// satisfies it directly.
type HeartbeatRecorder interface {
	Record(ctx context.Context, role, instance string, now time.Time) error
}

// DefaultWorkerHeartbeatInterval returns the heartbeat period: 15 seconds unless
// RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS holds a positive number of seconds.
// Non-numeric, zero and negative values fall back to the default.
func DefaultWorkerHeartbeatInterval() time.Duration {
	raw, present := os.LookupEnv(WorkerHeartbeatEnv)
	if !present {
		return defaultWorkerHeartbeatInterval
	}
	seconds, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || math.IsNaN(seconds) || math.IsInf(seconds, 0) || seconds <= 0 {
		return defaultWorkerHeartbeatInterval
	}
	interval := time.Duration(seconds * float64(time.Second))
	if interval <= 0 {
		return defaultWorkerHeartbeatInterval
	}
	return interval
}

// WorkerInstanceID returns the heartbeat identity of this process in the
// hostname-pid form, bounded to the 200 characters the state column stores.
func WorkerInstanceID() string {
	host, err := os.Hostname()
	host = strings.TrimSpace(host)
	if err != nil || host == "" {
		host = "unknown-host"
	}
	suffix := "-" + strconv.Itoa(os.Getpid())
	maxHostLength := maxWorkerInstanceLength - len(suffix)
	if maxHostLength < 0 {
		maxHostLength = 0
	}
	if runes := []rune(host); len(runes) > maxHostLength {
		host = string(runes[:maxHostLength])
	}
	return host + suffix
}

// StartHeartbeat announces that this worker process is alive. It writes one
// heartbeat immediately, then once per interval until ctx is canceled or the
// returned stop function is called. The periodic writes run on a background
// goroutine, so the worker main loop is never blocked by heartbeat traffic;
// stop is idempotent and waits for that goroutine. A nil recorder makes the
// call a no-op.
func StartHeartbeat(
	ctx context.Context,
	recorder HeartbeatRecorder,
	role string,
	interval time.Duration,
	logger *slog.Logger,
) (stop func()) {
	if recorder == nil {
		return func() {}
	}
	if logger == nil {
		logger = slog.Default()
	}
	if interval <= 0 {
		interval = DefaultWorkerHeartbeatInterval()
	}
	instance := WorkerInstanceID()
	runContext, cancel := context.WithCancel(ctx)
	record := func() {
		beatContext, beatCancel := context.WithTimeout(runContext, heartbeatWriteTimeout)
		defer beatCancel()
		if err := recorder.Record(beatContext, role, instance, time.Now()); err != nil {
			logger.Warn("文档 worker 心跳写入失败", "role", role, "instance", instance, "error", err)
		}
	}
	// The first beat is written before returning so a freshly started worker is
	// visible on the monitoring page without waiting one full interval.
	record()
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-runContext.Done():
				return
			case <-ticker.C:
				record()
			}
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			cancel()
			<-done
		})
	}
}
