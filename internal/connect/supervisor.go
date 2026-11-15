package connect

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

// Recovery markers reported by ConnectorHealth.Recovery. They are stable
// strings so the monitoring JSON and the Console keep one vocabulary.
const (
	RecoveryNone       = ""
	RecoveryRestarting = "restarting"
	RecoveryExhausted  = "exhausted"
	RecoveryRecovered  = "recovered"
)

// TaskHealth is one task's observed state plus the supervisor's restart view.
type TaskHealth struct {
	ID       int    `json:"id"`
	State    string `json:"state"`
	WorkerID string `json:"worker_id"`
	Trace    string `json:"trace,omitempty"`
}

// ConnectorHealth is the read-only view the monitoring probe consumes: live
// Kafka Connect state merged with the supervisor's restart bookkeeping.
//
// The binlog capture offset still lives inside Kafka Connect, so this type has
// no progress percentage. Times are RFC3339 strings; empty means "no such
// event since the process started".
type ConnectorHealth struct {
	Connector string       `json:"connector"`
	State     string       `json:"state"`
	WorkerID  string       `json:"worker_id"`
	Tasks     []TaskHealth `json:"tasks"`

	FailedTasks   int    `json:"failed_tasks"`
	FailureReason string `json:"failure_reason"`

	Restarting       bool   `json:"restarting"`
	RestartAttempts  int    `json:"restart_attempts"`
	RestartExhausted bool   `json:"restart_exhausted"`
	Recovery         string `json:"recovery"`

	LastRestartAt   string `json:"last_restart_at"`
	LastRecoveredAt string `json:"last_recovered_at"`
	NextRestartAt   string `json:"next_restart_at"`

	SupervisorEnabled bool `json:"supervisor_enabled"`
}

// statusClient is the narrow Kafka Connect surface the supervisor needs. Client
// implements it; unit tests inject a fake without HTTP.
type statusClient interface {
	Status(ctx context.Context, connector string) (ConnectorStatus, error)
	RestartTask(ctx context.Context, connector string, task int) error
}

// restartState is one (connector, task) failure episode. Attempts reset only
// after the task has been observed RUNNING for a stability window, so a flapping
// task cannot restart forever under a finite budget.
type restartState struct {
	attempts      int
	nextAttempt   time.Time
	exhausted     bool
	failureReason string
	failureAt     time.Time
	lastRestartAt time.Time
	recoveredAt   time.Time
	stableAt      time.Time
}

// DefaultStabilityWindow is how long a restarted task must stay RUNNING before
// its attempt budget is reset. A task that recovers and fails again inside this
// window continues spending the same budget instead of restarting forever.
const DefaultStabilityWindow = 30 * time.Second

// Supervisor periodically reads both connectors' task states and restarts only
// FAILED tasks with exponential backoff. Kafka Connect does not restart a task
// that entered FAILED by itself; without this loop a transient MySQL/network
// error silences one capture path indefinitely.
type Supervisor struct {
	client          statusClient
	connectors      []string
	interval        time.Duration
	maxAttempts     int
	baseBackoff     time.Duration
	maxBackoff      time.Duration
	stabilityWindow time.Duration
	logger          *slog.Logger
	now             func() time.Time

	mu         sync.Mutex
	tasks      map[taskKey]*restartState
	lastStatus map[string]ConnectorStatus
	lastError  map[string]string
}

type taskKey struct {
	connector string
	task      int
}

// NewSupervisor builds a supervisor from resolved settings. The logger may be
// nil (a discard logger is used) and the clock defaults to time.Now.
func NewSupervisor(client statusClient, settings Settings, logger *slog.Logger) *Supervisor {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(discardWriter{}, nil))
	}
	return &Supervisor{
		client:          client,
		connectors:      []string{settings.DocumentConnector, settings.CacheConnector},
		interval:        settings.SupervisorInterval,
		maxAttempts:     settings.RestartMaxAttempts,
		baseBackoff:     settings.RestartBaseBackoff,
		maxBackoff:      settings.RestartMaxBackoff,
		stabilityWindow: DefaultStabilityWindow,
		logger:          logger,
		now:             time.Now,
		tasks:           make(map[taskKey]*restartState),
		lastStatus:      make(map[string]ConnectorStatus),
		lastError:       make(map[string]string),
	}
}

// discardWriter is a tiny io.Writer for the nil-logger fallback.
type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }

// setClock is a test seam.
func (s *Supervisor) setClock(now func() time.Time) { s.now = now }

// Run polls until ctx is cancelled. The first tick runs immediately so a task
// that failed before the service started is repaired without waiting a period.
func (s *Supervisor) Run(ctx context.Context) {
	s.Tick(ctx)
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Tick(ctx)
		}
	}
}

// Tick performs one observation/restart round across both connectors. It is
// exported so the CLI can run one supervised round and tests can drive it
// deterministically.
func (s *Supervisor) Tick(ctx context.Context) {
	for _, connector := range s.connectors {
		status, err := s.client.Status(ctx, connector)
		s.mu.Lock()
		s.lastStatus[connector] = status
		if err != nil {
			s.lastError[connector] = shortError(err)
		} else {
			delete(s.lastError, connector)
		}
		s.mu.Unlock()
		if err != nil {
			s.logger.Warn("Kafka Connect 连接器状态读取失败",
				"connector", connector, "error", shortError(err))
			continue
		}
		s.evaluate(ctx, connector, status)
	}
}

// evaluate restarts the FAILED tasks of one connector subject to the attempt
// budget and the per-task backoff deadline.
func (s *Supervisor) evaluate(ctx context.Context, connector string, status ConnectorStatus) {
	now := s.now()
	for _, task := range status.Tasks {
		key := taskKey{connector: connector, task: task.ID}
		s.mu.Lock()
		state, exists := s.tasks[key]
		if !exists {
			state = &restartState{}
			s.tasks[key] = state
		}
		switch task.State {
		case StateRunning:
			if state.lastRestartAt.IsZero() {
				s.mu.Unlock()
				continue
			}
			if state.stableAt.IsZero() {
				state.stableAt = now
				state.recoveredAt = now
				reason := state.failureReason
				attempts := state.attempts
				s.mu.Unlock()
				s.logger.Info("Kafka Connect 任务已恢复",
					"connector", connector, "task", task.ID,
					"attempts", attempts, "failure", reason)
				continue
			}
			if now.Sub(state.stableAt) < s.stabilityWindow {
				s.mu.Unlock()
				continue
			}
			// The task has been stable long enough: the failure episode is over
			// and a future failure starts a fresh attempt budget.
			state.attempts = 0
			state.exhausted = false
			state.nextAttempt = time.Time{}
			state.stableAt = time.Time{}
			s.mu.Unlock()
			continue
		case StateFailed:
			// Fall through to the restart decision below.
		default:
			s.mu.Unlock()
			continue
		}

		reason := task.Trace
		if reason == "" {
			reason = status.ConnectorTrace
		}
		if reason != "" && reason != state.failureReason {
			state.failureReason = reason
			state.failureAt = now
		}
		// A failure inside the stability window continues the same episode.
		state.stableAt = time.Time{}
		if state.exhausted {
			s.mu.Unlock()
			continue
		}
		if now.Before(state.nextAttempt) {
			s.mu.Unlock()
			continue
		}
		if state.attempts >= s.maxAttempts {
			state.exhausted = true
			attempts := state.attempts
			failure := state.failureReason
			s.mu.Unlock()
			s.logger.Warn("Kafka Connect 任务重启次数已耗尽，保留失败原因等待人工处理",
				"connector", connector, "task", task.ID,
				"attempts", attempts, "failure", failure)
			continue
		}
		state.attempts++
		attempt := state.attempts
		state.lastRestartAt = now
		state.nextAttempt = now.Add(s.backoff(attempt))
		state.recoveredAt = time.Time{}
		s.mu.Unlock()

		if err := s.client.RestartTask(ctx, connector, task.ID); err != nil {
			s.logger.Warn("Kafka Connect 任务重启请求失败",
				"connector", connector, "task", task.ID,
				"attempt", attempt, "error", shortError(err))
			continue
		}
		s.logger.Info("已请求 Kafka Connect 重启失败任务",
			"connector", connector, "task", task.ID,
			"attempt", attempt, "max_attempts", s.maxAttempts,
			"backoff_seconds", s.backoff(attempt).Seconds(),
			"failure", reason)
	}
}

// backoff returns base * 2^(attempt-1), capped at maxBackoff.
func (s *Supervisor) backoff(attempt int) time.Duration {
	if attempt < 1 {
		attempt = 1
	}
	delay := s.baseBackoff
	for index := 1; index < attempt; index++ {
		delay *= 2
		if delay >= s.maxBackoff {
			return s.maxBackoff
		}
	}
	if delay > s.maxBackoff {
		return s.maxBackoff
	}
	return delay
}

// Health reads one connector's live status and merges the supervisor's restart
// bookkeeping. A REST failure returns the error and a partial placeholder; the
// monitor renders the row as an error instead of inventing a healthy state.
func (s *Supervisor) Health(ctx context.Context, connector string) (ConnectorHealth, error) {
	status, err := s.client.Status(ctx, connector)
	if err != nil {
		s.mu.Lock()
		cached, cachedOK := s.lastStatus[connector]
		s.mu.Unlock()
		if cachedOK {
			health := healthFromStatus(cached)
			health.SupervisorEnabled = true
			health.FailureReason = boundedText("状态读取失败："+shortError(err), 300)
			return health, err
		}
		return ConnectorHealth{
			Connector:         connector,
			State:             StateUnknown,
			SupervisorEnabled: true,
		}, err
	}
	health := healthFromStatus(status)
	health.SupervisorEnabled = true

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var (
		attempts    int
		restarting  bool
		exhausted   bool
		lastRestart time.Time
		lastRecover time.Time
		nextRestart time.Time
	)
	for index := range health.Tasks {
		if health.Tasks[index].State != StateFailed {
			continue
		}
		state, ok := s.tasks[taskKey{connector: connector, task: health.Tasks[index].ID}]
		if !ok {
			continue
		}
		if state.attempts > attempts {
			attempts = state.attempts
		}
		if state.exhausted {
			exhausted = true
		} else if !state.nextAttempt.IsZero() && state.nextAttempt.After(now) {
			restarting = true
			if nextRestart.IsZero() || state.nextAttempt.Before(nextRestart) {
				nextRestart = state.nextAttempt
			}
		}
		if state.lastRestartAt.After(lastRestart) {
			lastRestart = state.lastRestartAt
		}
	}
	// A recovered row keeps its timestamps so the Console can show the outcome
	// of the last restart episode instead of an unexplained green row.
	for key, state := range s.tasks {
		if key.connector == connector && state.recoveredAt.After(lastRecover) {
			lastRecover = state.recoveredAt
		}
	}
	health.RestartAttempts = attempts
	health.RestartExhausted = exhausted
	health.Restarting = restarting
	health.LastRestartAt = formatTime(lastRestart)
	health.LastRecoveredAt = formatTime(lastRecover)
	health.NextRestartAt = formatTime(nextRestart)
	switch {
	case exhausted:
		health.Recovery = RecoveryExhausted
	case restarting:
		health.Recovery = RecoveryRestarting
	case !lastRecover.IsZero() && health.FailedTasks == 0:
		health.Recovery = RecoveryRecovered
	}
	return health, nil
}

// healthFromStatus converts one live status document.
func healthFromStatus(status ConnectorStatus) ConnectorHealth {
	health := ConnectorHealth{
		Connector: status.Name,
		State:     status.ConnectorState,
		WorkerID:  status.ConnectorWorker,
		Tasks:     make([]TaskHealth, 0, len(status.Tasks)),
	}
	health.FailureReason = status.ConnectorTrace
	for _, task := range status.Tasks {
		health.Tasks = append(health.Tasks, TaskHealth{
			ID:       task.ID,
			State:    task.State,
			WorkerID: task.WorkerID,
			Trace:    task.Trace,
		})
		if task.State != StateFailed {
			continue
		}
		health.FailedTasks++
		if health.FailureReason == "" && task.Trace != "" {
			health.FailureReason = task.Trace
		}
	}
	return health
}

// formatTime renders a health timestamp, or "" for the zero value.
func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.UTC().Format(time.RFC3339)
}

// Describe renders a compact one-line state for CLI output.
func (h ConnectorHealth) Describe() string {
	parts := []string{fmt.Sprintf("%s=%s", h.Connector, h.State)}
	if h.WorkerID != "" {
		parts = append(parts, "worker="+h.WorkerID)
	}
	for _, task := range h.Tasks {
		worker := task.WorkerID
		if worker == "" {
			worker = "未分配"
		}
		parts = append(parts, fmt.Sprintf("task%d=%s@%s", task.ID, task.State, worker))
	}
	if h.FailedTasks > 0 {
		parts = append(parts, fmt.Sprintf("失败任务=%d", h.FailedTasks))
	}
	switch h.Recovery {
	case RecoveryRestarting:
		parts = append(parts, fmt.Sprintf("重启中（第 %d 次）", h.RestartAttempts))
	case RecoveryExhausted:
		parts = append(parts, "重启已耗尽")
	case RecoveryRecovered:
		parts = append(parts, "已恢复")
	}
	if h.FailureReason != "" {
		parts = append(parts, "原因="+boundedText(strings.ReplaceAll(h.FailureReason, "\n", " "), 120))
	}
	return strings.Join(parts, "；")
}
