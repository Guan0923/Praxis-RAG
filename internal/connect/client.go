package connect

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Connector classes and states used by Kafka Connect.
const (
	// MySQLConnectorClass must be present on every worker; the connector
	// templates reference it and the plugin check refuses a mixed cluster.
	MySQLConnectorClass = "io.debezium.connector.mysql.MySqlConnector"

	StateRunning    = "RUNNING"
	StatePaused     = "PAUSED"
	StateStopped    = "STOPPED"
	StateFailed     = "FAILED"
	StateUnassigned = "UNASSIGNED"
	StateUnknown    = "UNKNOWN"
	StateRestarting = "RESTARTING"
)

// ErrNotConfigured means RAG_CONNECT_URLS is empty.
var ErrNotConfigured = errors.New("kafka connect is not configured")

// ErrConnectorNotFound means Kafka Connect answered 404 for a connector.
var ErrConnectorNotFound = errors.New("kafka connect connector not found")

// APIError is a bounded, sanitized Kafka Connect REST failure. The message
// never carries connector configuration values, so a validation error that
// echoes a password cannot leak into the service log.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	if e.Message == "" {
		return fmt.Sprintf("kafka connect returned HTTP %d", e.StatusCode)
	}
	return fmt.Sprintf("kafka connect returned HTTP %d: %s", e.StatusCode, e.Message)
}

// ConnectorStatus is one normalized connector status document.
type ConnectorStatus struct {
	Name            string
	ConnectorState  string
	ConnectorWorker string
	ConnectorTrace  string
	Tasks           []TaskState
}

// TaskState is one connector task's observed state.
type TaskState struct {
	ID       int
	State    string
	WorkerID string
	Trace    string
}

// Plugin describes one connector plugin reported by a worker.
type Plugin struct {
	Class   string `json:"class"`
	Type    string `json:"type"`
	Version string `json:"version"`
}

// UpsertResult reports what an idempotent connector registration did.
type UpsertResult struct {
	Name    string
	Created bool
	Updated bool
}

// Client is a small Kafka Connect REST client with worker failover: every call
// tries the configured workers in order, starting with the last one that
// answered, so a stopped worker only costs one failed round trip per probe.
type Client struct {
	endpoints []endpoint
	http      *http.Client
	timeout   time.Duration

	mu        sync.Mutex
	preferred int
}

type endpoint struct {
	base   *url.URL
	auth   string
	origin string
}

// NewClient validates the endpoints and builds a client. A zero timeout falls
// back to DefaultTimeout.
func NewClient(urls []string, timeout time.Duration) (*Client, error) {
	if len(urls) == 0 {
		return nil, ErrNotConfigured
	}
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	endpoints := make([]endpoint, 0, len(urls))
	for _, raw := range urls {
		parsed, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("invalid kafka connect url")
		}
		auth := ""
		if parsed.User != nil {
			password, _ := parsed.User.Password()
			token := base64.StdEncoding.EncodeToString([]byte(parsed.User.Username() + ":" + password))
			auth = "Basic " + token
			parsed.User = nil
		}
		endpoints = append(endpoints, endpoint{
			base:   parsed,
			auth:   auth,
			origin: parsed.Scheme + "://" + parsed.Host,
		})
	}
	return &Client{
		endpoints: endpoints,
		http:      &http.Client{Timeout: timeout},
		timeout:   timeout,
	}, nil
}

// Endpoints returns the redacted REST origins in failover order. Credentials in
// a userinfo segment are never returned.
func (c *Client) Endpoints() []string {
	origins := make([]string, 0, len(c.endpoints))
	for _, item := range c.endpoints {
		origins = append(origins, item.origin)
	}
	return origins
}

// Status reads one connector's status (connector + task states).
func (c *Client) Status(ctx context.Context, connector string) (ConnectorStatus, error) {
	var document struct {
		Name      string `json:"name"`
		Connector struct {
			State    string `json:"state"`
			WorkerID string `json:"worker_id"`
			Trace    string `json:"trace"`
		} `json:"connector"`
		Tasks []struct {
			ID       int    `json:"id"`
			State    string `json:"state"`
			WorkerID string `json:"worker_id"`
			Trace    string `json:"trace"`
		} `json:"tasks"`
	}
	if err := c.do(ctx, http.MethodGet, "/connectors/"+url.PathEscape(connector)+"/status", nil, &document); err != nil {
		return ConnectorStatus{}, err
	}
	status := ConnectorStatus{
		Name:            strings.TrimSpace(document.Name),
		ConnectorState:  normalizeState(document.Connector.State),
		ConnectorWorker: strings.TrimSpace(document.Connector.WorkerID),
		ConnectorTrace:  boundedTrace(document.Connector.Trace),
		Tasks:           make([]TaskState, 0, len(document.Tasks)),
	}
	if status.Name == "" {
		status.Name = connector
	}
	for _, task := range document.Tasks {
		status.Tasks = append(status.Tasks, TaskState{
			ID:       task.ID,
			State:    normalizeState(task.State),
			WorkerID: strings.TrimSpace(task.WorkerID),
			Trace:    boundedTrace(task.Trace),
		})
	}
	return status, nil
}

// ConnectorNames lists the connectors registered in the cluster.
func (c *Client) ConnectorNames(ctx context.Context) ([]string, error) {
	var names []string
	if err := c.do(ctx, http.MethodGet, "/connectors", nil, &names); err != nil {
		return nil, err
	}
	sort.Strings(names)
	return names, nil
}

// PluginList reads the connector plugins of one worker. The first reachable
// worker answers; plugin sets are identical across a correctly provisioned
// cluster.
func (c *Client) PluginList(ctx context.Context) ([]Plugin, error) {
	var plugins []Plugin
	if err := c.do(ctx, http.MethodGet, "/connector-plugins", nil, &plugins); err != nil {
		return nil, err
	}
	return plugins, nil
}

// PluginsFor lists the workers' plugin inventories, one entry per configured
// endpoint. An unreachable worker is reported as an error for that worker; the
// caller decides whether a partial inventory is acceptable.
func (c *Client) PluginsFor(ctx context.Context) (map[string][]Plugin, error) {
	inventories := make(map[string][]Plugin, len(c.endpoints))
	errorsSeen := make([]string, 0)
	for _, item := range c.endpoints {
		var plugins []Plugin
		if err := c.doOne(ctx, item, http.MethodGet, "/connector-plugins", nil, &plugins); err != nil {
			errorsSeen = append(errorsSeen, item.origin+": "+shortError(err))
			continue
		}
		inventories[item.origin] = plugins
	}
	if len(inventories) == 0 {
		return nil, fmt.Errorf("no kafka connect worker answered (%s)", strings.Join(errorsSeen, "; "))
	}
	return inventories, nil
}

// Config reads one connector's stored configuration.
func (c *Client) Config(ctx context.Context, connector string) (map[string]string, error) {
	var config map[string]string
	if err := c.do(ctx, http.MethodGet, "/connectors/"+url.PathEscape(connector)+"/config", nil, &config); err != nil {
		return nil, err
	}
	if config == nil {
		config = map[string]string{}
	}
	return config, nil
}

// Upsert creates the connector when it does not exist and updates it when the
// stored configuration differs. An identical configuration is left untouched:
// repeatedly running the stack start must not bounce a running capture task.
func (c *Client) Upsert(ctx context.Context, name string, config map[string]string) (UpsertResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return UpsertResult{}, fmt.Errorf("connector name must not be empty")
	}
	existing, err := c.Config(ctx, name)
	switch {
	case err == nil:
		if configsEqual(existing, config) {
			return UpsertResult{Name: name}, nil
		}
		if err := c.do(ctx, http.MethodPut, "/connectors/"+url.PathEscape(name)+"/config", config, nil); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Name: name, Updated: true}, nil
	case errors.Is(err, ErrConnectorNotFound):
		if err := c.do(ctx, http.MethodPut, "/connectors/"+url.PathEscape(name)+"/config", config, nil); err != nil {
			return UpsertResult{}, err
		}
		return UpsertResult{Name: name, Created: true}, nil
	default:
		return UpsertResult{}, err
	}
}

// RestartTask asks Kafka Connect to restart one failed task. Connect only
// restarts a task whose connector is RUNNING.
func (c *Client) RestartTask(ctx context.Context, connector string, task int) error {
	path := fmt.Sprintf("/connectors/%s/tasks/%d/restart", url.PathEscape(connector), task)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// RestartConnector restarts the connector (optionally its tasks too).
func (c *Client) RestartConnector(ctx context.Context, connector string, includeTasks bool) error {
	path := fmt.Sprintf("/connectors/%s/restart?includeTasks=%t&onlyFailed=false",
		url.PathEscape(connector), includeTasks)
	return c.do(ctx, http.MethodPost, path, nil, nil)
}

// Pause stops the connector and its tasks while keeping the configuration and
// committed source offsets. Used by the acceptance run to prove the two capture
// paths are independent.
func (c *Client) Pause(ctx context.Context, connector string) error {
	return c.do(ctx, http.MethodPut, "/connectors/"+url.PathEscape(connector)+"/pause", nil, nil)
}

// Resume restarts a paused connector.
func (c *Client) Resume(ctx context.Context, connector string) error {
	return c.do(ctx, http.MethodPut, "/connectors/"+url.PathEscape(connector)+"/resume", nil, nil)
}

// Delete removes one connector.
func (c *Client) Delete(ctx context.Context, connector string) error {
	return c.do(ctx, http.MethodDelete, "/connectors/"+url.PathEscape(connector), nil, nil)
}

// do issues one request with worker failover.
func (c *Client) do(ctx context.Context, method, path string, payload any, out any) error {
	c.mu.Lock()
	start := c.preferred
	c.mu.Unlock()
	if len(c.endpoints) == 0 {
		return ErrNotConfigured
	}
	var lastErr error
	for offset := 0; offset < len(c.endpoints); offset++ {
		index := (start + offset) % len(c.endpoints)
		err := c.doOne(ctx, c.endpoints[index], method, path, payload, out)
		if err == nil {
			c.mu.Lock()
			c.preferred = index
			c.mu.Unlock()
			return nil
		}
		var apiErr *APIError
		if method != http.MethodGet && errors.As(err, &apiErr) &&
			apiErr.StatusCode == http.StatusConflict && len(c.endpoints) > 1 {
			// Kafka Connect only accepts some writes on the group leader; a
			// follower answers 409. The peer may be the leader, so try it.
			lastErr = err
			continue
		}
		if errors.Is(err, ErrConnectorNotFound) || isClientError(err) {
			// A 4xx answer is authoritative for the cluster, not a worker
			// problem: retrying another worker would report the same thing.
			return err
		}
		lastErr = err
		if ctx.Err() != nil {
			return fmt.Errorf("kafka connect request cancelled: %w", ctx.Err())
		}
	}
	return lastErr
}

// isClientError reports whether another worker cannot change the answer.
func isClientError(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	return apiErr.StatusCode >= 400 && apiErr.StatusCode < 500 && apiErr.StatusCode != http.StatusNotFound
}

// doOne performs the request against exactly one endpoint.
func (c *Client) doOne(ctx context.Context, target endpoint, method, path string, payload any, out any) error {
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return fmt.Errorf("encode kafka connect request: %w", err)
		}
		body = bytes.NewReader(encoded)
	}
	request, err := http.NewRequestWithContext(ctx, method, target.origin+path, body)
	if err != nil {
		return fmt.Errorf("build kafka connect request: %w", err)
	}
	request.Header.Set("Accept", "application/json")
	if payload != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if target.auth != "" {
		request.Header.Set("Authorization", target.auth)
	}
	response, err := c.http.Do(request)
	if err != nil {
		return fmt.Errorf("%s is unreachable: %w", target.origin, err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, readErr := io.ReadAll(io.LimitReader(response.Body, 1<<20))
	if response.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%w: %s", ErrConnectorNotFound, path)
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &APIError{
			StatusCode: response.StatusCode,
			Message:    sanitizedAPIMessage(raw),
		}
	}
	if readErr != nil {
		return fmt.Errorf("read kafka connect response: %w", readErr)
	}
	if out == nil || len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode kafka connect response: %w", err)
	}
	return nil
}

// sanitizedAPIMessage extracts the Connect error message and removes values that
// could be secrets (configuration validation errors echo connector settings).
func sanitizedAPIMessage(raw []byte) string {
	var document struct {
		Message string `json:"message"`
		Error   string `json:"error_code"`
	}
	message := ""
	if json.Unmarshal(raw, &document) == nil && strings.TrimSpace(document.Message) != "" {
		message = document.Message
	} else {
		message = strings.TrimSpace(string(raw))
	}
	message = passwordPattern.ReplaceAllString(message, "$1=***")
	message = whitespacePattern.ReplaceAllString(message, " ")
	return boundedText(message, 300)
}

var (
	passwordPattern   = regexp.MustCompile(`(?i)(password|secret|token)(["']?\s*[=:]\s*)("[^"]*"|[^\s,;}]+)`)
	whitespacePattern = regexp.MustCompile(`\s+`)
	statePattern      = regexp.MustCompile(`^[A-Z]+$`)
)

// normalizeState uppercases Connect's state strings and tolerates a missing one.
func normalizeState(raw string) string {
	state := strings.ToUpper(strings.TrimSpace(raw))
	if state == "" {
		return StateUnknown
	}
	if !statePattern.MatchString(state) {
		return StateUnknown
	}
	return state
}

// boundedTrace keeps one task failure reason readable in a log line and the
// monitoring page without copying a megabyte stack trace.
func boundedTrace(raw string) string {
	return boundedText(whitespacePattern.ReplaceAllString(strings.TrimSpace(raw), " "), 400)
}

// boundedText truncates at a UTF-8 boundary; callers only use it for display.
func boundedText(text string, limit int) string {
	if limit <= 0 || len(text) <= limit {
		return text
	}
	cut := limit
	for cut > 0 && (text[cut]&0xC0) == 0x80 {
		cut--
	}
	return text[:cut] + "…"
}

// configsEqual compares two connector configurations exactly. Kafka Connect
// returns every stored key, so an equality check is enough to decide whether a
// PUT would bounce the connector.
func configsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		other, ok := right[key]
		if !ok || other != value {
			return false
		}
	}
	return true
}

// shortError bounds an error for a one-line diagnostic.
func shortError(err error) string {
	if err == nil {
		return ""
	}
	return boundedText(whitespacePattern.ReplaceAllString(err.Error(), " "), 200)
}

// Timeout returns the configured per-request timeout.
func (c *Client) Timeout() time.Duration { return c.timeout }
