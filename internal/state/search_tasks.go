package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"math"
	"os"
	"strconv"
	"strings"
)

// Search task states and limits (search_tasks.py + schema search_tasks).
const (
	SearchTaskQueued     = "queued"
	SearchTaskProcessing = "processing"
	SearchTaskSucceeded  = "succeeded"
	SearchTaskFailed     = "failed"
	SearchTaskTimedOut   = "timed_out"

	SearchEnvelopeSchema     = 2
	MaxSearchArgumentBytes   = 1024 * 1024
	MaxSearchEnvelopeBytes   = MaxSearchArgumentBytes + 4096
	searchUnavailableMessage = "Search result expired or unavailable"
)

var searchActiveStatuses = []string{SearchTaskQueued, SearchTaskProcessing}

// SearchTaskUnavailable mirrors the RuntimeError raised when search admission
// cannot be confirmed (no broker / duplicate admission).
type SearchTaskUnavailable struct{ Message string }

// Error implements error.
func (e *SearchTaskUnavailable) Error() string { return e.Message }

func newSearchTaskUnavailable(message string) *SearchTaskUnavailable {
	return &SearchTaskUnavailable{Message: message}
}

// SearchTaskSettings mirrors search_tasks.SearchTaskSettings.
type SearchTaskSettings struct {
	TimeoutSeconds float64
	LeaseSeconds   float64
	RetrySeconds   float64
}

// DefaultSearchTaskSettings returns the Python defaults (lease/retry are the Go
// durable-state extensions).
func DefaultSearchTaskSettings() SearchTaskSettings {
	return SearchTaskSettings{TimeoutSeconds: 120, LeaseSeconds: 120, RetrySeconds: 5}
}

// SearchTaskSettingsFromEnv reads RAG_SEARCH_TIMEOUT_SECONDS and the state-layer
// lease/retry extensions.
func SearchTaskSettingsFromEnv() (SearchTaskSettings, error) {
	settings := DefaultSearchTaskSettings()
	raw := strings.TrimSpace(envOrDefault("RAG_SEARCH_TIMEOUT_SECONDS", "120"))
	timeout, err := strconv.ParseFloat(raw, 64)
	if err != nil || math.IsNaN(timeout) || math.IsInf(timeout, 0) || timeout <= 0 {
		return SearchTaskSettings{}, newValueError("Search task timeout_seconds must be finite and positive")
	}
	settings.TimeoutSeconds = timeout
	if raw := strings.TrimSpace(os.Getenv("RAG_SEARCH_TASK_LEASE_SECONDS")); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || value <= 0 {
			return SearchTaskSettings{}, newValueError("RAG_SEARCH_TASK_LEASE_SECONDS must be positive")
		}
		settings.LeaseSeconds = value
	}
	return settings, nil
}

// SearchEnvelope is the full-request schema-2 broker message.
type SearchEnvelope struct {
	Schema    int            `json:"schema"`
	ResultID  string         `json:"result_id"`
	Namespace string         `json:"namespace"`
	Arguments map[string]any `json:"arguments"`
	CreatedAt float64        `json:"created_at"`
	ExpiresAt float64        `json:"expires_at"`
	Deadline  float64        `json:"deadline"`
}

// ValidateSearchEnvelope mirrors validate_search_envelope; a malformed transport
// payload is a plain ValueError("Invalid search message envelope").
func ValidateSearchEnvelope(payload any) (SearchEnvelope, error) {
	invalid := func() (SearchEnvelope, error) {
		return SearchEnvelope{}, newValueError("Invalid search message envelope")
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return invalid()
	}
	expected := map[string]struct{}{
		"schema": {}, "result_id": {}, "namespace": {}, "arguments": {},
		"created_at": {}, "expires_at": {}, "deadline": {},
	}
	if len(object) != len(expected) {
		return invalid()
	}
	for key := range object {
		if _, known := expected[key]; !known {
			return invalid()
		}
	}
	schema, ok := finiteNumericValue(object["schema"])
	if !ok || schema != float64(SearchEnvelopeSchema) {
		return invalid()
	}
	resultID, ok := object["result_id"].(string)
	if !ok || !canonicalUUID(resultID) {
		return invalid()
	}
	namespace, ok := object["namespace"].(string)
	if !ok || len(namespace) < 1 || len(namespace) > 64 {
		return invalid()
	}
	arguments, ok := object["arguments"].(map[string]any)
	if !ok {
		return invalid()
	}
	createdAt, ok := finiteNumericValue(object["created_at"])
	if !ok {
		return invalid()
	}
	expiresAt, ok := finiteNumericValue(object["expires_at"])
	if !ok {
		return invalid()
	}
	deadline, ok := finiteNumericValue(object["deadline"])
	if !ok {
		return invalid()
	}
	if !(createdAt < deadline && deadline <= expiresAt) {
		return invalid()
	}
	argumentJSON, err := marshalJSON(arguments)
	if err != nil {
		return invalid()
	}
	encoded, err := marshalJSON(map[string]any{
		"schema": SearchEnvelopeSchema, "result_id": resultID, "namespace": namespace,
		"arguments": arguments, "created_at": createdAt, "expires_at": expiresAt, "deadline": deadline,
	})
	if err != nil {
		return invalid()
	}
	if len(argumentJSON) > MaxSearchArgumentBytes || len(encoded) > MaxSearchEnvelopeBytes {
		return invalid()
	}
	return SearchEnvelope{
		Schema: SearchEnvelopeSchema, ResultID: resultID, Namespace: namespace,
		Arguments: arguments, CreatedAt: createdAt, ExpiresAt: expiresAt, Deadline: deadline,
	}, nil
}

func searchRequestDigest(envelope SearchEnvelope) string {
	encoded, err := marshalJSON(map[string]any{
		"namespace": envelope.Namespace, "arguments": envelope.Arguments,
		"created_at": envelope.CreatedAt, "expires_at": envelope.ExpiresAt, "deadline": envelope.Deadline,
	})
	if err != nil {
		return ""
	}
	return sha256Hex(encoded)
}

// SearchTask is one durable search task row.
type SearchTask struct {
	ResultID     string         `json:"result_id"`
	Namespace    string         `json:"namespace"`
	Arguments    map[string]any `json:"arguments"`
	Status       string         `json:"status"`
	Stage        string         `json:"stage"`
	Attempts     int            `json:"attempts"`
	Owner        *string        `json:"owner"`
	LeaseUntil   float64        `json:"lease_until"`
	RetryAt      float64        `json:"retry_at"`
	Deadline     float64        `json:"deadline"`
	CreatedAt    float64        `json:"created_at"`
	UpdatedAt    float64        `json:"updated_at"`
	RemainingSec float64        `json:"remaining_seconds"`
}

// SearchTaskView is the public status projection returned by Get.
type SearchTaskView struct {
	ResultID  string  `json:"result_id"`
	Status    string  `json:"status"`
	Stage     string  `json:"stage"`
	ExpiresAt float64 `json:"expires_at"`
	Deadline  float64 `json:"deadline"`
}

// SearchTasks is the durable search-task state machine. Python keeps active
// task state in Redis; the Go port uses the existing search_tasks table so the
// state machine and outbox are testable before the Redis client lands.
type SearchTasks struct {
	stores   *Stores
	Settings SearchTaskSettings
	Clock    func() float64
}

// NewSearchTasks builds the task repository.
func NewSearchTasks(stores *Stores) (*SearchTasks, error) {
	settings, err := SearchTaskSettingsFromEnv()
	if err != nil {
		return nil, err
	}
	return &SearchTasks{stores: stores, Settings: settings, Clock: unixSeconds}, nil
}

func (s *SearchTasks) now() float64 {
	if s.Clock == nil {
		return unixSeconds()
	}
	return s.Clock()
}

func (s *SearchTasks) resultTTL() float64 {
	return float64(s.stores.Results.Settings.TTLSeconds)
}

const searchTaskColumns = "result_id, namespace, arguments, status, stage, attempts, owner, " +
	"lease_until, retry_at, deadline, created_at, updated_at"

func scanSearchTask(scanner rowScanner) (*SearchTask, error) {
	var (
		task      SearchTask
		arguments string
		owner     sql.NullString
	)
	if err := scanner.Scan(&task.ResultID, &task.Namespace, &arguments, &task.Status, &task.Stage,
		&task.Attempts, &owner, &task.LeaseUntil, &task.RetryAt, &task.Deadline,
		&task.CreatedAt, &task.UpdatedAt); err != nil {
		return nil, err
	}
	task.Owner = nullStringPointer(owner)
	if err := json.Unmarshal([]byte(arguments), &task.Arguments); err != nil {
		return nil, err
	}
	return &task, nil
}

// Submit persists a queued task and its pending outbox row atomically.
func (s *SearchTasks) Submit(ctx context.Context, namespace string, arguments map[string]any, resultID *string) (SearchEnvelope, error) {
	identifier := ""
	if resultID == nil {
		generated, err := uuid4String()
		if err != nil {
			return SearchEnvelope{}, err
		}
		identifier = generated
	} else {
		if !canonicalUUID(*resultID) {
			return SearchEnvelope{}, newValueError("result_id must be a canonical UUID")
		}
		identifier = *resultID
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	now := s.now()
	expires := now + s.resultTTL()
	deadline := now + s.Settings.TimeoutSeconds
	if expires < deadline {
		deadline = expires
	}
	envelope, err := ValidateSearchEnvelope(map[string]any{
		"schema": float64(SearchEnvelopeSchema), "result_id": identifier, "namespace": namespace,
		"arguments": arguments, "created_at": now, "expires_at": expires, "deadline": deadline,
	})
	if err != nil {
		return SearchEnvelope{}, err
	}
	encoded, err := marshalJSON(envelope.Arguments)
	if err != nil {
		return SearchEnvelope{}, err
	}
	transaction, err := s.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return SearchEnvelope{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO search_tasks (result_id, namespace, arguments, status, stage, attempts, owner, "+
			"lease_until, retry_at, deadline, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, 0, NULL, 0, ?, ?, ?, ?)",
		identifier, namespace, encoded, SearchTaskQueued, SearchTaskQueued,
		now, deadline, now, now); err != nil {
		if isUniqueViolation(err) {
			return SearchEnvelope{}, newSearchTaskUnavailable("Search admission unavailable")
		}
		return SearchEnvelope{}, err
	}
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO search_task_outbox (result_id, pending, generation, retry_at) VALUES (?, 1, 0, ?)",
		identifier, now); err != nil {
		if isUniqueViolation(err) {
			return SearchEnvelope{}, newSearchTaskUnavailable("Search admission unavailable")
		}
		return SearchEnvelope{}, err
	}
	if err := transaction.Commit(); err != nil {
		return SearchEnvelope{}, err
	}
	return envelope, nil
}

// Get returns the status view; nil means the task is unknown/foreign.
func (s *SearchTasks) Get(ctx context.Context, namespace, resultID string, page, pageSize int) (*SearchTaskView, error) {
	_ = page
	_ = pageSize
	if !canonicalUUID(resultID) {
		return nil, newSnapshotUnavailable(searchUnavailableMessage)
	}
	task, err := s.read(ctx, resultID)
	if err != nil || task == nil || task.Namespace != namespace {
		return nil, err
	}
	if s.isActive(task.Status) && s.isExpired(task) {
		if updateErr := s.markTimedOut(ctx, task); updateErr != nil {
			return nil, updateErr
		}
		task.Status = SearchTaskTimedOut
		task.Stage = SearchTaskFailed
	}
	view := &SearchTaskView{
		ResultID: resultID, Status: task.Status, Stage: task.Stage,
		ExpiresAt: task.CreatedAt + s.resultTTL(), Deadline: task.Deadline,
	}
	switch task.Status {
	case SearchTaskSucceeded:
		return view, nil
	case SearchTaskTimedOut:
		return nil, newSnapshotUnavailable("Search timed out before completion; submit a new search")
	case SearchTaskFailed:
		return nil, newSnapshotUnavailable("Search failed; submit a new search")
	default:
		view.Status = SearchTaskQueued
		return view, nil
	}
}

// Terminal reports whether the task is unknown, invalid, or finished.
func (s *SearchTasks) Terminal(ctx context.Context, envelope any) (bool, error) {
	resultID, namespace, err := s.resolveEnvelope(ctx, envelope)
	if err != nil {
		if errors.As(err, new(*ValueError)) {
			return true, nil
		}
		return false, err
	}
	task, err := s.read(ctx, resultID)
	if err != nil {
		return false, err
	}
	if task == nil || task.Namespace != namespace {
		return true, nil
	}
	if s.isActive(task.Status) && s.isExpired(task) {
		if err := s.markTimedOut(ctx, task); err != nil {
			return false, err
		}
		return true, nil
	}
	return !s.isActive(task.Status), nil
}

func (s *SearchTasks) resolveEnvelope(ctx context.Context, envelope any) (string, string, error) {
	if object, ok := envelope.(map[string]any); ok {
		validated, err := ValidateSearchEnvelope(object)
		if err != nil {
			return "", "", err
		}
		return validated.ResultID, validated.Namespace, nil
	}
	resultID, ok := envelope.(string)
	if !ok || !canonicalUUID(resultID) {
		return "", "", newValueError("Invalid search message envelope")
	}
	task, err := s.read(ctx, resultID)
	if err != nil || task == nil {
		return resultID, "", err
	}
	return resultID, task.Namespace, nil
}

func (s *SearchTasks) read(ctx context.Context, resultID string) (*SearchTask, error) {
	task, err := scanSearchTask(s.stores.DB.QueryRowContext(ctx,
		"SELECT "+searchTaskColumns+" FROM search_tasks WHERE result_id = ?", resultID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return task, err
}

func (s *SearchTasks) isActive(status string) bool {
	for _, active := range searchActiveStatuses {
		if status == active {
			return true
		}
	}
	return false
}

func (s *SearchTasks) isExpired(task *SearchTask) bool {
	now := s.now()
	expires := task.CreatedAt + s.resultTTL()
	return now >= task.Deadline || now >= expires
}

func (s *SearchTasks) markTimedOut(ctx context.Context, task *SearchTask) error {
	_, err := s.stores.DB.ExecContext(ctx,
		"UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, updated_at = ? "+
			"WHERE result_id = ? AND status IN (?, ?)",
		SearchTaskTimedOut, SearchTaskFailed, s.now(), task.ResultID, SearchTaskQueued, SearchTaskProcessing)
	return err
}

// Claim takes the execution lease; nil means terminal, foreign, live, or expired.
func (s *SearchTasks) Claim(ctx context.Context, envelope SearchEnvelope) (*SearchTask, error) {
	validated, err := ValidateSearchEnvelope(map[string]any{
		"schema": float64(envelope.Schema), "result_id": envelope.ResultID,
		"namespace": envelope.Namespace, "arguments": envelope.Arguments,
		"created_at": envelope.CreatedAt, "expires_at": envelope.ExpiresAt, "deadline": envelope.Deadline,
	})
	if err != nil {
		return nil, err
	}
	now := s.now()
	transaction, err := s.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	task, err := scanSearchTask(transaction.QueryRowContext(ctx,
		"SELECT "+searchTaskColumns+" FROM search_tasks WHERE result_id = ?"+s.stores.Uploads.forUpdate(),
		validated.ResultID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if task.Namespace != validated.Namespace || !s.isActive(task.Status) {
		return nil, nil
	}
	if s.isExpired(task) {
		if _, err := transaction.ExecContext(ctx,
			"UPDATE search_tasks SET status = ?, stage = ?, updated_at = ? WHERE result_id = ?",
			SearchTaskTimedOut, SearchTaskFailed, now, task.ResultID); err != nil {
			return nil, err
		}
		if err := transaction.Commit(); err != nil {
			return nil, err
		}
		return nil, nil
	}
	if task.Status == SearchTaskProcessing && task.LeaseUntil > now {
		return nil, nil
	}
	storedDigest := ""
	if stored, err := marshalJSON(task.Arguments); err == nil {
		storedDigest = sha256Hex(stored)
	}
	envelopeArguments, err := marshalJSON(validated.Arguments)
	if err != nil {
		return nil, err
	}
	if storedDigest != sha256Hex(envelopeArguments) || task.Deadline != validated.Deadline ||
		math.Abs(task.CreatedAt-validated.CreatedAt) > 1e-9 {
		return nil, nil
	}
	attempts := task.Attempts + 1
	owner, err := randomHex(16)
	if err != nil {
		return nil, err
	}
	leaseUntil := now + s.Settings.LeaseSeconds
	if leaseUntil > task.Deadline {
		leaseUntil = task.Deadline
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE search_tasks SET status = ?, stage = ?, attempts = ?, owner = ?, lease_until = ?, updated_at = ? "+
			"WHERE result_id = ?",
		SearchTaskProcessing, SearchTaskProcessing, attempts, owner, leaseUntil, now, task.ResultID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	task.Status = SearchTaskProcessing
	task.Stage = SearchTaskProcessing
	task.Attempts = attempts
	task.Owner = &owner
	task.LeaseUntil = leaseUntil
	task.UpdatedAt = now
	task.Arguments = validated.Arguments
	task.RemainingSec = math.Max(0, task.Deadline-now)
	return task, nil
}

// Complete marks the task succeeded after preparing (and, when a transient store
// is wired, saving) the immutable result snapshot.
func (s *SearchTasks) Complete(ctx context.Context, task *SearchTask, payload map[string]any) (map[string]any, error) {
	prepared, err := s.stores.Results.Prepare(task.Namespace, payload, &task.ResultID, nil)
	if err != nil {
		return nil, err
	}
	if s.stores.Results.Transient != nil {
		if _, err := s.stores.Results.Save(task.Namespace, payload, &task.ResultID, nil); err != nil {
			return nil, err
		}
	}
	preparedPayload, _ := prepared["payload"].(map[string]any)
	query := "UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, updated_at = ? " +
		"WHERE result_id = ?"
	arguments := []any{SearchTaskSucceeded, "completed", s.now(), task.ResultID}
	if task.Owner != nil {
		query += " AND owner = ?"
		arguments = append(arguments, *task.Owner)
	}
	if _, err := s.stores.DB.ExecContext(ctx, query, arguments...); err != nil {
		return nil, err
	}
	return preparedPayload, nil
}

// Fail requeues or terminates an active task.
func (s *SearchTasks) Fail(ctx context.Context, task *SearchTask, retry bool, cause *string) (bool, error) {
	_ = cause
	now := s.now()
	status, stage := SearchTaskFailed, SearchTaskFailed
	retryAt := now
	if retry {
		status, stage = SearchTaskQueued, "retry_wait"
		retryAt = now + s.Settings.RetrySeconds
	}
	query := "UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, retry_at = ?, " +
		"updated_at = ? WHERE result_id = ? AND status IN (?, ?)"
	arguments := []any{status, stage, retryAt, now, task.ResultID, SearchTaskQueued, SearchTaskProcessing}
	if task.Owner != nil {
		query += " AND owner = ?"
		arguments = append(arguments, *task.Owner)
	}
	result, err := s.stores.DB.ExecContext(ctx, query, arguments...)
	if err != nil {
		return false, err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed > 0, nil
}

// DeadLetter terminates an undeliverable task.
func (s *SearchTasks) DeadLetter(ctx context.Context, envelope any) (bool, error) {
	resultID, namespace, err := s.resolveEnvelope(ctx, envelope)
	if err != nil {
		return true, nil
	}
	task, err := s.read(ctx, resultID)
	if err != nil {
		return false, err
	}
	if task == nil || task.Namespace != namespace || !s.isActive(task.Status) {
		return true, nil
	}
	task.Owner = nil
	failed := "Message retry limit reached"
	if _, err := s.Fail(ctx, task, false, &failed); err != nil {
		return false, err
	}
	return true, nil
}

// Expire fails active tasks whose deadline passed.
func (s *SearchTasks) Expire(ctx context.Context, limit int) (int, error) {
	if limit <= 0 {
		limit = 20
	}
	now := s.now()
	rows, err := s.stores.DB.QueryContext(ctx,
		"SELECT result_id FROM search_tasks WHERE status IN (?, ?) AND deadline <= ? LIMIT ?",
		SearchTaskQueued, SearchTaskProcessing, now, limit)
	if err != nil {
		return 0, err
	}
	identifiers := make([]string, 0)
	for rows.Next() {
		var resultID string
		if err := rows.Scan(&resultID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		identifiers = append(identifiers, resultID)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(identifiers) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(identifiers))
	updateArguments := make([]any, 0, len(identifiers)+4)
	for index, resultID := range identifiers {
		placeholders[index] = "?"
		updateArguments = append(updateArguments, resultID)
	}
	updateArguments = append(updateArguments, now, SearchTaskQueued, SearchTaskProcessing)
	result, err := s.stores.DB.ExecContext(ctx,
		"UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, updated_at = ? "+
			"WHERE result_id IN ("+strings.Join(placeholders, ", ")+") AND status IN (?, ?)",
		append([]any{SearchTaskTimedOut, SearchTaskFailed, now}, updateArguments...)...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}

// SearchNotificationPublisher publishes full-request schema-2 envelopes.
type SearchNotificationPublisher interface {
	SendSearchEnvelope(ctx context.Context, envelope SearchEnvelope) error
	Close() error
}

// DispatchPending publishes due pending outbox rows for queued tasks.
func (s *SearchTasks) DispatchPending(ctx context.Context, publisher SearchNotificationPublisher, limit int) (int, error) {
	if publisher == nil {
		return 0, nil
	}
	if limit <= 0 {
		limit = 20
	}
	now := s.now()
	rows, err := s.stores.DB.QueryContext(ctx,
		"SELECT result_id FROM search_task_outbox WHERE pending = 1 AND retry_at <= ? ORDER BY retry_at LIMIT ?",
		now, limit)
	if err != nil {
		return 0, err
	}
	identifiers := make([]string, 0)
	for rows.Next() {
		var resultID string
		if err := rows.Scan(&resultID); err != nil {
			_ = rows.Close()
			return 0, err
		}
		identifiers = append(identifiers, resultID)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	published := 0
	for _, resultID := range identifiers {
		task, err := s.read(ctx, resultID)
		if err != nil {
			return published, err
		}
		if task == nil || task.Status != SearchTaskQueued {
			if _, err := s.stores.DB.ExecContext(ctx,
				"UPDATE search_task_outbox SET pending = 0 WHERE result_id = ?", resultID); err != nil {
				return published, err
			}
			continue
		}
		envelope, err := ValidateSearchEnvelope(map[string]any{
			"schema": float64(SearchEnvelopeSchema), "result_id": task.ResultID,
			"namespace": task.Namespace, "arguments": task.Arguments,
			"created_at": task.CreatedAt, "expires_at": task.CreatedAt + s.resultTTL(),
			"deadline": task.Deadline,
		})
		if err != nil {
			if _, markErr := s.stores.DB.ExecContext(ctx,
				"UPDATE search_task_outbox SET pending = 0 WHERE result_id = ?", resultID); markErr != nil {
				return published, markErr
			}
			continue
		}
		if sendErr := publisher.SendSearchEnvelope(ctx, envelope); sendErr != nil {
			if _, updateErr := s.stores.DB.ExecContext(ctx,
				"UPDATE search_task_outbox SET retry_at = ?, generation = generation + 1 WHERE result_id = ?",
				now+s.Settings.RetrySeconds, resultID); updateErr != nil {
				return published, updateErr
			}
			continue
		}
		if _, err := s.stores.DB.ExecContext(ctx,
			"UPDATE search_task_outbox SET pending = 0 WHERE result_id = ?", resultID); err != nil {
			return published, err
		}
		published++
	}
	return published, nil
}

// SearchTaskDispatcher is the broker-facing admission seam
// (search_queue.SearchTaskDispatcher) with a narrow publisher interface.
type SearchTaskDispatcher struct {
	Tasks     *SearchTasks
	Publisher SearchNotificationPublisher
}

// NewSearchTaskDispatcher builds the dispatcher over the store's task state.
func NewSearchTaskDispatcher(stores *Stores, publisher SearchNotificationPublisher) *SearchTaskDispatcher {
	return &SearchTaskDispatcher{Tasks: stores.SearchTasks, Publisher: publisher}
}

// Submit admits one search request and confirms broker acceptance.
func (d *SearchTaskDispatcher) Submit(ctx context.Context, namespace string, arguments map[string]any) (SearchEnvelope, error) {
	if d.Publisher == nil {
		return SearchEnvelope{}, newSearchTaskUnavailable("Search broker unavailable")
	}
	envelope, err := d.Tasks.Submit(ctx, namespace, arguments, nil)
	if err != nil {
		return SearchEnvelope{}, err
	}
	published, err := d.Tasks.DispatchPending(ctx, d.Publisher, 1)
	if err != nil || published == 0 {
		failed := "Search admission unconfirmed"
		task := &SearchTask{ResultID: envelope.ResultID, Namespace: envelope.Namespace}
		_, _ = d.Tasks.Fail(ctx, task, false, &failed)
		return SearchEnvelope{}, newSearchTaskUnavailable("Search queue acceptance was not confirmed")
	}
	return envelope, nil
}

// RunOnce publishes due search tasks (relay seam for worker binaries).
func (d *SearchTaskDispatcher) RunOnce(ctx context.Context) (int, error) {
	return d.Tasks.DispatchPending(ctx, d.Publisher, 50)
}

// Start is a no-op hook for dispatcher symmetry.
func (d *SearchTaskDispatcher) Start() {}

// Stop is a no-op hook for dispatcher symmetry.
func (d *SearchTaskDispatcher) Stop() {}

// Close closes the publisher.
func (d *SearchTaskDispatcher) Close() error {
	if d.Publisher != nil {
		return d.Publisher.Close()
	}
	return nil
}
