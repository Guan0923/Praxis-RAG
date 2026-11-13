package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// CDC cutover bookkeeping keys stored in cdc_cutover_state
// (docs/cdc-migration.md §3.4/§8).
const (
	// CutoverKeyCDCStartFile is the SHOW MASTER STATUS File recorded while the
	// write path was paused.
	CutoverKeyCDCStartFile = "cdc_start_file"
	// CutoverKeyCDCStartPos is the SHOW MASTER STATUS Position for that file.
	CutoverKeyCDCStartPos = "cdc_start_pos"
	// CutoverKeyCutoverAt is the Unix second (or RFC3339 text) the cutover
	// window opened.
	CutoverKeyCutoverAt = "cutover_at"
	// CutoverKeyScheduleBaseline is the schedule_version baseline every active
	// task must reach before the new connectors are trusted.
	CutoverKeyScheduleBaseline = "schedule_baseline"
	// CutoverKeyPhase is the operator-visible cutover phase marker.
	CutoverKeyPhase = "phase"
)

// CutoverTaskInventory is the unfinished-task inventory of one task table.
type CutoverTaskInventory struct {
	// ByStatus counts unfinished (queued/processing) rows per status. Both
	// known active statuses are always present, zero when absent.
	ByStatus map[string]int
	// Active is the total number of unfinished rows.
	Active int
	// OldestActiveSeconds is the age of the oldest unfinished row (0 when none).
	OldestActiveSeconds float64
	// MinActiveScheduleVersion is the smallest schedule_version among unfinished
	// rows (0 when none). --verify asserts it is >= baseline+1.
	MinActiveScheduleVersion int64
}

// CutoverInventory is the read-only pre-cutover snapshot printed by
// `rag-cdc-cutover --inventory`.
type CutoverInventory struct {
	DocumentJobs CutoverTaskInventory
	SearchTasks  CutoverTaskInventory
	// CacheKeys is the number of tracked read-model keys (cache_revisions rows).
	CacheKeys int
	// CacheDirtyKeys is the number of tracked keys whose Redis value is behind
	// the database revision (refreshed_revision < revision).
	CacheDirtyKeys int
}

// CutoverStateGet reads one cdc_cutover_state value. ok is false when the key
// has not been recorded yet.
func (s *Stores) CutoverStateGet(ctx context.Context, key string) (string, bool, error) {
	if key == "" || len(key) > 64 {
		return "", false, newValueError("Invalid cdc_cutover_state key")
	}
	var value string
	err := s.DB.QueryRowContext(ctx,
		"SELECT v FROM cdc_cutover_state WHERE k = ?", key).Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return value, true, nil
}

// CutoverStateSet upserts one cdc_cutover_state value.
func (s *Stores) CutoverStateSet(ctx context.Context, key, value string) error {
	if key == "" || len(key) > 64 {
		return newValueError("Invalid cdc_cutover_state key")
	}
	_, err := s.DB.ExecContext(ctx,
		"INSERT INTO cdc_cutover_state (k, v) VALUES (?, ?) ON DUPLICATE KEY UPDATE v = ?",
		key, value, value)
	return err
}

// CutoverStateAll reads every cdc_cutover_state row.
func (s *Stores) CutoverStateAll(ctx context.Context) (map[string]string, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT k, v FROM cdc_cutover_state ORDER BY k")
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	return values, rows.Err()
}

// CutoverCacheKey is one read-model key watermark printed by
// `rag-cdc-cutover --inventory` ("各 key 当前 revision",
// docs/cdc-migration.md §8).
type CutoverCacheKey struct {
	Kind              string  `json:"kind"`
	Scope             string  `json:"scope"`
	Revision          int64   `json:"revision"`
	RefreshedRevision int64   `json:"refreshed_revision"`
	LastError         *string `json:"last_error"`
	UpdatedAt         float64 `json:"updated_at"`
}

// CutoverCacheRevisions lists every tracked read-model key ordered by kind and
// scope. It is read-only and unbounded: the inventory CLI prints it as-is.
func (s *Stores) CutoverCacheRevisions(ctx context.Context) ([]CutoverCacheKey, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT kind, scope, revision, refreshed_revision, last_error, updated_at "+
			"FROM cache_revisions ORDER BY kind, scope")
	if err != nil {
		return nil, fmt.Errorf("cache_revisions 盘点失败：%w", err)
	}
	defer func() { _ = rows.Close() }()
	keys := make([]CutoverCacheKey, 0)
	for rows.Next() {
		var key CutoverCacheKey
		var lastError sql.NullString
		if err := rows.Scan(&key.Kind, &key.Scope, &key.Revision,
			&key.RefreshedRevision, &lastError, &key.UpdatedAt); err != nil {
			return nil, err
		}
		if lastError.Valid {
			key.LastError = &lastError.String
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// CutoverInventory reports the unfinished task and cache state without
// modifying anything. now is the reference time for the age fields.
func (s *Stores) CutoverInventory(ctx context.Context, now time.Time) (*CutoverInventory, error) {
	documentJobs, err := s.cutoverTaskInventory(ctx, now,
		"document_jobs", "artifact_id", []string{DocumentJobQueued, DocumentJobProcessing})
	if err != nil {
		return nil, fmt.Errorf("document_jobs 盘点失败：%w", err)
	}
	searchTasks, err := s.cutoverTaskInventory(ctx, now,
		"search_tasks", "result_id", searchActiveStatuses)
	if err != nil {
		return nil, fmt.Errorf("search_tasks 盘点失败：%w", err)
	}
	cacheKeys, err := s.monitorCount(ctx, "SELECT COUNT(*) FROM cache_revisions")
	if err != nil {
		return nil, fmt.Errorf("cache_revisions 盘点失败：%w", err)
	}
	cacheDirty, err := s.monitorCount(ctx,
		"SELECT COUNT(*) FROM cache_revisions WHERE refreshed_revision < revision")
	if err != nil {
		return nil, fmt.Errorf("cache_revisions 脏 key 盘点失败：%w", err)
	}
	return &CutoverInventory{
		DocumentJobs:   documentJobs,
		SearchTasks:    searchTasks,
		CacheKeys:      cacheKeys,
		CacheDirtyKeys: cacheDirty,
	}, nil
}

// cutoverTaskInventory aggregates one task table for the cutover inventory.
func (s *Stores) cutoverTaskInventory(ctx context.Context, now time.Time,
	table, identifierColumn string, activeStatuses []string) (CutoverTaskInventory, error) {
	inventory := CutoverTaskInventory{ByStatus: make(map[string]int, len(activeStatuses))}
	for _, status := range activeStatuses {
		inventory.ByStatus[status] = 0
	}
	statusPlaceholders := make([]string, len(activeStatuses))
	arguments := make([]any, len(activeStatuses))
	for index, status := range activeStatuses {
		statusPlaceholders[index] = "?"
		arguments[index] = status
	}
	query := "SELECT status, COUNT(*) FROM " + quoteIdentifier(table) +
		" WHERE status IN (" + joinPlaceholders(statusPlaceholders) + ") GROUP BY status"
	rows, err := s.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return inventory, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var status string
		var count int
		if err := rows.Scan(&status, &count); err != nil {
			return inventory, err
		}
		inventory.ByStatus[status] = count
		inventory.Active += count
	}
	if err := rows.Err(); err != nil {
		return inventory, err
	}
	var oldest sql.NullFloat64
	var minVersion sql.NullInt64
	aggregate := "SELECT MIN(created_at), MIN(schedule_version) FROM " + quoteIdentifier(table) +
		" WHERE status IN (" + joinPlaceholders(statusPlaceholders) + ")"
	if err := s.DB.QueryRowContext(ctx, aggregate, arguments...).Scan(&oldest, &minVersion); err != nil {
		return inventory, err
	}
	if oldest.Valid && oldest.Float64 > 0 {
		age := float64(now.Unix()) - oldest.Float64
		if age > 0 {
			inventory.OldestActiveSeconds = age
		}
	}
	if minVersion.Valid {
		inventory.MinActiveScheduleVersion = minVersion.Int64
	}
	return inventory, nil
}

// joinPlaceholders renders "?, ?, ?"-style placeholders.
func joinPlaceholders(placeholders []string) string {
	joined := ""
	for index, placeholder := range placeholders {
		if index > 0 {
			joined += ", "
		}
		joined += placeholder
	}
	return joined
}

// RequeueActive bumps every unfinished document job to the cutover baseline and
// resets it to queued in one transaction, so Debezium emits exactly one
// consumable notification per task. It is idempotent: only rows whose
// schedule_version is not newer than baseline are touched, so a second run
// reports 0. Callers loop until the returned count is 0.
func (d *DocumentJobs) RequeueActive(ctx context.Context, limit int, baseline int64) (int, error) {
	return requeueActive(ctx, d.stores.DB, "document_jobs", "artifact_id",
		documentJobActiveStatuses, limit, baseline, "execution_id = NULL, ")
}

// RequeueActive is the search-task variant of DocumentJobs.RequeueActive.
func (s *SearchTasks) RequeueActive(ctx context.Context, limit int, baseline int64) (int, error) {
	return requeueActive(ctx, s.stores.DB, "search_tasks", "result_id",
		searchActiveStatuses, limit, baseline, "")
}

// requeueActive is the shared cutover backfill statement.
func requeueActive(ctx context.Context, database *sql.DB, table, identifierColumn string,
	activeStatuses []string, limit int, baseline int64, extraReset string) (int, error) {
	if limit <= 0 {
		limit = 200
	}
	if baseline < 0 {
		return 0, newValueError("schedule baseline must not be negative")
	}
	now := unixSeconds()
	transaction, err := database.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	statusPlaceholders := make([]string, len(activeStatuses))
	arguments := make([]any, 0, len(activeStatuses)+3)
	for index, status := range activeStatuses {
		statusPlaceholders[index] = "?"
		arguments = append(arguments, status)
	}
	arguments = append(arguments, baseline, limit)
	selection := "SELECT " + quoteIdentifier(identifierColumn) + " FROM " + quoteIdentifier(table) +
		" WHERE status IN (" + joinPlaceholders(statusPlaceholders) + ") " +
		"AND schedule_version <= ? ORDER BY created_at LIMIT ? FOR UPDATE"
	rows, err := transaction.QueryContext(ctx, selection, arguments...)
	if err != nil {
		return 0, err
	}
	identifiers := make([]string, 0, limit)
	for rows.Next() {
		var identifier string
		if err := rows.Scan(&identifier); err != nil {
			_ = rows.Close()
			return 0, err
		}
		identifiers = append(identifiers, identifier)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if len(identifiers) == 0 {
		return 0, nil
	}
	updatePlaceholders := make([]string, len(identifiers))
	updateArguments := make([]any, 0, len(identifiers)+6)
	updateArguments = append(updateArguments, baseline, DocumentJobQueued, "retry_wait", now, now, baseline)
	for index, identifier := range identifiers {
		updatePlaceholders[index] = "?"
		updateArguments = append(updateArguments, identifier)
	}
	result, err := transaction.ExecContext(ctx,
		"UPDATE "+quoteIdentifier(table)+" SET schedule_version = GREATEST(schedule_version, ?) + 1, "+
			"status = ?, stage = ?, "+
			"owner = NULL, "+extraReset+"lease_until = 0, retry_at = ?, updated_at = ? "+
			"WHERE schedule_version <= ? AND "+quoteIdentifier(identifierColumn)+
			" IN ("+joinPlaceholders(updatePlaceholders)+")",
		updateArguments...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return int(affected), nil
}
