package connect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// Runtime owns the Kafka Connect client and the failed-task supervisor for one
// service process. It is safe to build with an empty RAG_CONNECT_URLS: Enabled
// then reports false and Health returns ErrNotConfigured, leaving the monitoring
// rows 未知 instead of failing service startup.
type Runtime struct {
	settings   Settings
	client     *Client
	supervisor *Supervisor
	logger     *slog.Logger
	cancel     context.CancelFunc
}

// NewRuntime builds the integration from resolved settings. A misconfigured
// endpoint list is a startup error: silently disabling capture observability
// would hide the very failure the operator needs to see.
func NewRuntime(settings Settings, logger *slog.Logger) (*Runtime, error) {
	if logger == nil {
		logger = slog.Default()
	}
	runtime := &Runtime{settings: settings, logger: logger}
	if !settings.Configured() {
		return runtime, nil
	}
	client, err := NewClient(settings.URLs, settings.Timeout)
	if err != nil {
		return nil, fmt.Errorf("Kafka Connect REST 配置无效：%w", err)
	}
	runtime.client = client
	if settings.SupervisorEnabled {
		runtime.supervisor = NewSupervisor(client, settings, logger)
	}
	return runtime, nil
}

// Enabled reports whether at least one REST endpoint is configured.
func (r *Runtime) Enabled() bool { return r.client != nil }

// Settings returns the resolved settings.
func (r *Runtime) Settings() Settings { return r.settings }

// Client returns the REST client, or nil when Connect is not configured.
func (r *Runtime) Client() *Client { return r.client }

// Supervisor returns the restart supervisor, or nil when disabled.
func (r *Runtime) Supervisor() *Supervisor { return r.supervisor }

// Start launches the supervisor loop in the background. It is a no-op when the
// integration or the supervisor is disabled. Closing the runtime (or the parent
// context) stops the loop.
func (r *Runtime) Start(ctx context.Context) {
	if r.supervisor == nil {
		return
	}
	if r.cancel != nil {
		return
	}
	loopContext, cancel := context.WithCancel(ctx)
	r.cancel = cancel
	go r.supervisor.Run(loopContext)
	r.logger.Info("Kafka Connect 失败任务自动重启已启动",
		"connectors", strings.Join([]string{r.settings.DocumentConnector, r.settings.CacheConnector}, ","),
		"interval_seconds", r.settings.SupervisorInterval.Seconds(),
		"max_attempts", r.settings.RestartMaxAttempts)
}

// Close stops the supervisor loop. It does not stop the Kafka Connect cluster.
func (r *Runtime) Close() {
	if r.cancel != nil {
		r.cancel()
		r.cancel = nil
	}
}

// Health reads one connector's status merged with the supervisor state. When
// the supervisor is disabled the live status is still reported, so the Console
// shows the real task state without claiming restart supervision.
func (r *Runtime) Health(ctx context.Context, connector string) (ConnectorHealth, error) {
	if r.client == nil {
		return ConnectorHealth{Connector: connector, State: StateUnknown}, ErrNotConfigured
	}
	connector = strings.TrimSpace(connector)
	if connector == "" {
		return ConnectorHealth{State: StateUnknown}, fmt.Errorf("connector name must not be empty")
	}
	if r.supervisor != nil {
		return r.supervisor.Health(ctx, connector)
	}
	status, err := r.client.Status(ctx, connector)
	if err != nil {
		return ConnectorHealth{Connector: connector, State: StateUnknown}, err
	}
	health := healthFromStatus(status)
	health.SupervisorEnabled = false
	return health, nil
}

// ConnectorNames returns the two fixed connector names.
func (r *Runtime) ConnectorNames() []string {
	return []string{r.settings.DocumentConnector, r.settings.CacheConnector}
}

// WaitForReady polls both connectors until each one is RUNNING and every task
// is RUNNING, or the timeout expires. The returned error names the connector
// (and failed task) that is not ready, so the stack start can fail fast with an
// actionable message instead of a generic timeout.
func WaitForReady(ctx context.Context, client *Client, connectors []string, timeout time.Duration, poll time.Duration) error {
	if client == nil {
		return ErrNotConfigured
	}
	if timeout <= 0 {
		timeout = 180 * time.Second
	}
	if poll <= 0 {
		poll = 2 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastProblem string
	for {
		ready := true
		lastProblem = ""
		for _, connector := range connectors {
			status, err := client.Status(ctx, connector)
			if err != nil {
				ready = false
				lastProblem = connector + "：" + shortError(err)
				break
			}
			if status.ConnectorState != StateRunning {
				ready = false
				lastProblem = fmt.Sprintf("%s 连接器状态为 %s（等待 RUNNING）", connector, status.ConnectorState)
				break
			}
			failed := false
			for _, task := range status.Tasks {
				if task.State == StateRunning {
					continue
				}
				ready = false
				failed = true
				lastProblem = fmt.Sprintf("%s task %d 状态为 %s（等待 RUNNING）",
					connector, task.ID, task.State)
				if task.Trace != "" {
					lastProblem += "：" + boundedText(task.Trace, 160)
				}
				break
			}
			if failed {
				break
			}
			if len(status.Tasks) == 0 {
				ready = false
				lastProblem = connector + " 尚未调度任何 task"
				break
			}
		}
		if ready {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("Kafka Connect 连接器在 %s 内未就绪：%s", timeout, lastProblem)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("等待 Kafka Connect 连接器就绪被取消：%w", ctx.Err())
		case <-time.After(poll):
		}
	}
}

// VerifyConnectorPlugins asserts that every reachable worker reports the pinned
// Debezium MySQL connector version. A mixed-version cluster is refused rather
// than allowed to capture with two different semantics.
func VerifyConnectorPlugins(ctx context.Context, client *Client, expectedVersion string) (map[string]string, error) {
	if client == nil {
		return nil, ErrNotConfigured
	}
	inventories, err := client.PluginsFor(ctx)
	if err != nil {
		return nil, err
	}
	// A partial inventory is not enough: a worker that joins later with a
	// different Debezium version would silently change capture semantics. The
	// start path must see every configured worker answer.
	missing := make([]string, 0)
	for _, origin := range client.Endpoints() {
		if _, ok := inventories[origin]; !ok {
			missing = append(missing, origin)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("Kafka Connect worker(s) did not answer the plugin check: %s",
			strings.Join(missing, ", "))
	}
	versions := make(map[string]string, len(inventories))
	want := strings.TrimSpace(expectedVersion)
	for origin, plugins := range inventories {
		found := false
		for _, plugin := range plugins {
			if plugin.Class != MySQLConnectorClass {
				continue
			}
			found = true
			version := strings.TrimSpace(plugin.Version)
			versions[origin] = version
			if want != "" && version != want {
				return versions, fmt.Errorf("%s 上的 Debezium MySQL Connector 版本为 %q，期望 %q",
					origin, version, want)
			}
			break
		}
		if !found {
			return versions, fmt.Errorf("%s 未安装 %s", origin, MySQLConnectorClass)
		}
	}
	if len(versions) == 0 {
		return nil, errors.New("no kafka connect worker reported a connector plugin inventory")
	}
	return versions, nil
}
