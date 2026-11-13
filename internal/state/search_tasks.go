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

	MaxSearchArgumentBytes   = 1024 * 1024
	searchUnavailableMessage = "Search result expired or unavailable"
)

var searchActiveStatuses = []string{SearchTaskQueued, SearchTaskProcessing}

// SearchTaskUnavailable mirrors the RuntimeError raised when search admission
// cannot be persisted (duplicate admission). Admission itself never waits for a
// broker confirmation.
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
	// ScheduleVersion is the scheduling generation Debezium watches. It starts
	// at 1 and is incremented inside the transaction that requeues the task.
	ScheduleVersion int64 `json:"schedule_version"`
}

// SearchTaskView is the public status projection returned by Get.
type SearchTaskView struct {
	ResultID  string  `json:"result_id"`
	Status    string  `json:"status"`
	Stage     string  `json:"stage"`
	ExpiresAt float64 `json:"expires_at"`
	Deadline  float64 `json:"deadline"`
}

// SearchTasks is the durable search-task state machine. MySQL is the source of
// truth; Debezium delivers the identifier-only notifications.
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
	"lease_until, retry_at, deadline, created_at, updated_at, schedule_version"

func scanSearchTask(scanner rowScanner) (*SearchTask, error) {
	var (
		task      SearchTask
		arguments string
		owner     sql.NullString
	)
	if err := scanner.Scan(&task.ResultID, &task.Namespace, &arguments, &task.Status, &task.Stage,
		&task.Attempts, &owner, &task.LeaseUntil, &task.RetryAt, &task.Deadline,
		&task.CreatedAt, &task.UpdatedAt, &task.ScheduleVersion); err != nil {
		return nil, err
	}
	task.Owner = nullStringPointer(owner)
	// Decode with UseNumber so integer JSON tokens stay json.Number. Python's
	// json keeps ints as int, and contracts.SearchRequestFromArguments validates
	// candidate_k/top_k with isinstance(int) semantics; plain json.Unmarshal would
	// turn them into float64 after the database round-trip and make every real
	// queued task fail validation.
	argumentDecoder := json.NewDecoder(strings.NewReader(arguments))
	argumentDecoder.UseNumber()
	if err := argumentDecoder.Decode(&task.Arguments); err != nil {
		return nil, err
	}
	return &task, nil
}

// Submit persists one queued task and returns its identifier-only schema-3
// notification. Submission is accepted when the MySQL transaction commits; it
// no longer waits for (or rolls back on) a Kafka acknowledgement. Debezium
// emits the notification from the committed row, and a paused broker only
// delays the task, it cannot reject it.
func (s *SearchTasks) Submit(ctx context.Context, namespace string, arguments map[string]any, resultID *string) (SearchNotification, error) {
	identifier := ""
	if resultID == nil {
		generated, err := uuid4String()
		if err != nil {
			return SearchNotification{}, err
		}
		identifier = generated
	} else {
		if !canonicalUUID(*resultID) {
			return SearchNotification{}, newValueError("result_id must be a canonical UUID")
		}
		identifier = *resultID
	}
	if len(namespace) < 1 || len(namespace) > 64 {
		return SearchNotification{}, newValueError("Invalid search namespace")
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	encoded, err := marshalJSON(arguments)
	if err != nil {
		return SearchNotification{}, err
	}
	if len(encoded) > MaxSearchArgumentBytes {
		return SearchNotification{}, newValueError("Search arguments exceed the size limit")
	}
	now := s.now()
	expires := now + s.resultTTL()
	deadline := now + s.Settings.TimeoutSeconds
	if expires < deadline {
		deadline = expires
	}
	transaction, err := s.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return SearchNotification{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO search_tasks (result_id, namespace, arguments, status, stage, attempts, owner, "+
			"lease_until, retry_at, deadline, created_at, updated_at, schedule_version) "+
			"VALUES (?, ?, ?, ?, ?, 0, NULL, 0, ?, ?, ?, ?, 1)",
		identifier, namespace, encoded, SearchTaskQueued, SearchTaskQueued,
		now, deadline, now, now); err != nil {
		if isUniqueViolation(err) {
			return SearchNotification{}, newSearchTaskUnavailable("Search admission unavailable")
		}
		return SearchNotification{}, err
	}
	if err := transaction.Commit(); err != nil {
		return SearchNotification{}, err
	}
	return SearchNotification{
		Schema:          SearchEnvelopeSchema,
		ResultID:        identifier,
		Namespace:       namespace,
		ScheduleVersion: 1,
	}, nil
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
		if errors.As(err, new(*ValueError)) || IgnorableCDCChange(err) {
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

// resolveEnvelope normalizes a notification (typed), a decoded map (app native
// envelope or raw Debezium change), or a bare result identifier.
func (s *SearchTasks) resolveEnvelope(ctx context.Context, envelope any) (string, string, error) {
	switch typed := envelope.(type) {
	case SearchNotification:
		validated, err := ValidateSearchNotification(map[string]any{
			"schema": float64(SearchEnvelopeSchema), "result_id": typed.ResultID,
			"namespace": typed.Namespace, "schedule_version": typed.ScheduleVersion,
		})
		if err != nil {
			return "", "", err
		}
		return validated.ResultID, validated.Namespace, nil
	case map[string]any:
		validated, err := ValidateSearchNotification(typed)
		if err != nil {
			return "", "", err
		}
		return validated.ResultID, validated.Namespace, nil
	case string:
		if !canonicalUUID(typed) {
			return "", "", newValueError("Invalid search message envelope")
		}
		task, err := s.read(ctx, typed)
		if err != nil || task == nil {
			return typed, "", err
		}
		return typed, task.Namespace, nil
	default:
		return "", "", newValueError("Invalid search message envelope")
	}
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

// markTimedOut converges an overdue active task. It is a plain UPDATE: the task
// is terminal, so no new scheduling generation is emitted.
func (s *SearchTasks) markTimedOut(ctx context.Context, task *SearchTask) error {
	_, err := s.stores.DB.ExecContext(ctx,
		"UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, updated_at = ? "+
			"WHERE result_id = ? AND status IN (?, ?)",
		SearchTaskTimedOut, SearchTaskFailed, s.now(), task.ResultID, SearchTaskQueued, SearchTaskProcessing)
	return err
}

// Claim takes the execution lease for one schema-3 notification. Returning
// (nil, nil) means the delivery is safely skippable: the row is missing,
// foreign, terminal, carries a different schedule_version, is overdue, or is
// owned by a live lease.
func (s *SearchTasks) Claim(ctx context.Context, notification SearchNotification) (*SearchTask, error) {
	validated, err := ValidateSearchNotification(map[string]any{
		"schema": float64(SearchEnvelopeSchema), "result_id": notification.ResultID,
		"namespace": notification.Namespace, "schedule_version": notification.ScheduleVersion,
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
	if task.Namespace != validated.Namespace || task.ScheduleVersion != validated.ScheduleVersion ||
		!s.isActive(task.Status) {
		return nil, nil
	}
	if s.isExpired(task) {
		if _, err := transaction.ExecContext(ctx,
			"UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, updated_at = ? "+
				"WHERE result_id = ?",
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
	task.RemainingSec = math.Max(0, task.Deadline-now)
	return task, nil
}

// Complete stores the immutable result snapshot and marks the task succeeded in
// one MySQL transaction. MySQL is the fact source, so a crash between the
// snapshot and the terminal status cannot happen; the Redis result cache is
// written best-effort after the commit.
func (s *SearchTasks) Complete(ctx context.Context, task *SearchTask, payload map[string]any) (map[string]any, error) {
	if task == nil {
		return nil, newValueError("Invalid search task")
	}
	envelope, err := s.stores.Results.Prepare(task.Namespace, payload, &task.ResultID, nil)
	if err != nil {
		return nil, err
	}
	preparedPayload, ok := envelope["payload"].(map[string]any)
	if !ok {
		return nil, newSnapshotUnavailable(searchUnavailableMessage)
	}
	now := s.now()
	transaction, err := s.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = transaction.Rollback() }()
	current, err := scanSearchTask(transaction.QueryRowContext(ctx,
		"SELECT "+searchTaskColumns+" FROM search_tasks WHERE result_id = ?"+s.stores.Uploads.forUpdate(),
		task.ResultID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newSnapshotUnavailable(searchUnavailableMessage)
	}
	if err != nil {
		return nil, err
	}
	if current.Namespace != task.Namespace {
		return nil, newSnapshotUnavailable(searchUnavailableMessage)
	}
	if current.Status == SearchTaskSucceeded {
		// A duplicate delivery after the commit: the snapshot is already durable.
		if err := transaction.Commit(); err != nil {
			return nil, err
		}
		return preparedPayload, nil
	}
	if !s.isActive(current.Status) {
		return nil, newSnapshotUnavailable("Search task is no longer active")
	}
	if task.Owner != nil && (current.Owner == nil || *current.Owner != *task.Owner) {
		return nil, newSnapshotUnavailable("Search task is no longer current")
	}
	if err := s.stores.Results.insertTx(ctx, transaction, envelope); err != nil {
		return nil, err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE search_tasks SET status = ?, stage = ?, owner = NULL, lease_until = 0, updated_at = ? "+
			"WHERE result_id = ?",
		SearchTaskSucceeded, "completed", now, task.ResultID); err != nil {
		return nil, err
	}
	if err := transaction.Commit(); err != nil {
		return nil, err
	}
	// The search already succeeded in MySQL; a Redis failure only costs a cache
	// miss that Get repairs by reading the fact row.
	_ = s.stores.Results.cacheEnvelope(envelope)
	return preparedPayload, nil
}

// Fail requeues or terminates an active task. A retry is the only UPDATE that
// must produce a new Debezium notification, so it increments schedule_version
// in the same statement.
func (s *SearchTasks) Fail(ctx context.Context, task *SearchTask, retry bool, cause *string) (bool, error) {
	_ = cause
	now := s.now()
	status, stage := SearchTaskFailed, SearchTaskFailed
	versionIncrement := ""
	retryAt := now
	if retry {
		status, stage = SearchTaskQueued, "retry_wait"
		versionIncrement = "schedule_version = schedule_version + 1, "
		retryAt = now + s.Settings.RetrySeconds
	}
	query := "UPDATE search_tasks SET " + versionIncrement +
		"status = ?, stage = ?, owner = NULL, lease_until = 0, retry_at = ?, " +
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

// Expire converges active tasks whose deadline passed into timed_out.
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
	updateArguments = append(updateArguments, SearchTaskQueued, SearchTaskProcessing)
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
