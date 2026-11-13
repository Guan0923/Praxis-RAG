package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// SnapshotUnavailable reports a missing, expired, evicted, or temporarily
// inaccessible search-result reference (SnapshotUnavailable in Python).
type SnapshotUnavailable struct{ Message string }

// Error implements error.
func (e *SnapshotUnavailable) Error() string { return e.Message }

func newSnapshotUnavailable(message string) *SnapshotUnavailable {
	return &SnapshotUnavailable{Message: message}
}

// SnapshotSettings mirrors rag_mcp.state.result_snapshots.SnapshotSettings.
type SnapshotSettings struct {
	TTLSeconds            int
	MaxResultBytes        int
	MaxArtifactBytes      int
	MaxArtifactsPerSearch int
}

// DefaultSnapshotSettings returns the Python dataclass defaults.
func DefaultSnapshotSettings() SnapshotSettings {
	return SnapshotSettings{
		TTLSeconds:            3600,
		MaxResultBytes:        16 * 1024 * 1024,
		MaxArtifactBytes:      64 * 1024 * 1024,
		MaxArtifactsPerSearch: 100,
	}
}

// SnapshotSettingsFromEnv mirrors SnapshotSettings.from_env.
func SnapshotSettingsFromEnv() (SnapshotSettings, error) {
	settings := DefaultSnapshotSettings()
	values := []struct {
		name     string
		fallback int
		target   *int
	}{
		{"RAG_RESULT_TTL_SECONDS", 3600, &settings.TTLSeconds},
		{"RAG_RESULT_MAX_BYTES", 16 * 1024 * 1024, &settings.MaxResultBytes},
		{"RAG_ARTIFACT_MAX_BYTES", 64 * 1024 * 1024, &settings.MaxArtifactBytes},
		{"RAG_RESULT_MAX_ARTIFACTS", 100, &settings.MaxArtifactsPerSearch},
	}
	for _, item := range values {
		value, err := positiveIntegerEnv(item.name, item.fallback)
		if err != nil {
			return SnapshotSettings{}, err
		}
		*item.target = value
	}
	return settings, nil
}

func positiveIntegerEnv(name string, fallback int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return value, nil
}

// TransientStore is the expiring key/value storage used as the search-result
// read cache. In production it is the Redis-backed
// rag_mcp.state.transient.TransientStore; the Go port keeps it behind this
// interface so no Redis dependency is needed. Values must be JSON-marshalable
// maps.
type TransientStore interface {
	Get(kind, identity string) (any, bool, error)
	Put(kind, identity string, value any, ttlSeconds float64, nx bool) (bool, error)
	Delete(kind, identity string) (int64, error)
}

// TransientProvider is implemented by a cache backend (the Redis client) that
// can also serve expiring transient values. The adapter is a separate view
// because StateCache and TransientStore have different Get/Put shapes.
type TransientProvider interface {
	TransientStore() TransientStore
}

// SnapshotIndex optionally records the identity and expiry of every saved
// result snapshot so monitoring can count the live snapshots. The result
// snapshots themselves live in MySQL (and in the Redis read cache); the index is
// an observation aid only.
//
// It must be safe for concurrent use; an index failure only loses observation
// data and must never fail the snapshot write.
type SnapshotIndex interface {
	// AddSnapshot 记录一条快照身份及过期时间（Unix 秒会话，可带小数）。
	AddSnapshot(ctx context.Context, identity string, expiresAt float64) error
}

// ResultSnapshots stores immutable, tenant-scoped search results. MySQL
// search_result_snapshots is the fact source; the Redis transient store is a
// read cache that is backfilled from the fact row after a miss.
type ResultSnapshots struct {
	Stores    *Stores
	Settings  SnapshotSettings
	Clock     func() float64
	Transient TransientStore
	// Index 是可选的快照索引；nil 时行为与引入索引前完全一致。
	Index SnapshotIndex
}

// NewResultSnapshots builds the snapshot service. A nil clock uses wall time; a
// nil Transient store only disables the read cache (the MySQL fact row is still
// authoritative).
func NewResultSnapshots(stores *Stores, settings SnapshotSettings, clock func() float64) *ResultSnapshots {
	if clock == nil {
		clock = unixSeconds
	}
	return &ResultSnapshots{Stores: stores, Settings: settings, Clock: clock}
}

func (s *ResultSnapshots) now() float64 {
	if s.Clock == nil {
		return unixSeconds()
	}
	return s.Clock()
}

func finiteSnapshotNumber(value any) (float64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) {
			return 0, false
		}
		return typed, true
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case json.Number:
		parsed, err := typed.Float64()
		if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}

func snapshotText(value any) (string, bool) {
	text, ok := value.(string)
	return text, ok
}

// Prepare validates and detaches a full response envelope (Python `prepare`).
func (s *ResultSnapshots) Prepare(namespace string, payload map[string]any, resultID *string, createdAt *float64) (map[string]any, error) {
	if namespace == "" || len(namespace) > 64 {
		return nil, newValueError("Invalid result namespace")
	}
	identifier := ""
	if resultID == nil {
		generated, err := uuid4String()
		if err != nil {
			return nil, err
		}
		identifier = generated
	} else {
		if !canonicalUUID(*resultID) {
			return nil, newValueError("Expected a canonical UUID")
		}
		identifier = *resultID
	}
	now := s.now()
	created := now
	if createdAt != nil {
		created = *createdAt
	}
	if math.IsNaN(created) || math.IsInf(created, 0) {
		return nil, newValueError("Invalid result creation time")
	}
	expires := created + float64(s.Settings.TTLSeconds)
	if math.IsNaN(expires) || math.IsInf(expires, 0) || expires <= created {
		return nil, newValueError("Invalid result expiry time")
	}
	if expires <= s.now() {
		return nil, newSnapshotUnavailable("Search result has already expired")
	}
	response := make(map[string]any, len(payload)+4)
	for key, value := range payload {
		response[key] = value
	}
	results := any([]any{})
	if value, ok := response["results"]; ok {
		if _, isList := value.([]any); !isList {
			return nil, newValueError("Search results must be a list")
		}
		results = value
	}
	response["results"] = results
	response["result_id"] = identifier
	response["created_at"] = created
	response["expires_at"] = expires
	response["snapshot_available"] = true
	envelope := map[string]any{
		"namespace":  namespace,
		"expires_at": expires,
		"status":     "succeeded",
		"stage":      "completed",
		"payload":    response,
	}
	content, err := json.Marshal(envelope)
	if err != nil {
		return nil, fmt.Errorf("Search result is not JSON-serializable: %w", err)
	}
	if len(content) > s.Settings.MaxResultBytes {
		return nil, newSnapshotUnavailable("Search result exceeds snapshot size limit")
	}
	var detached map[string]any
	if err := json.Unmarshal(content, &detached); err != nil {
		return nil, newValueError("Invalid result envelope")
	}
	return detached, nil
}

// insertTx writes one prepared envelope into the MySQL fact table inside the
// caller's transaction. A duplicate result_id means the immutable identity is
// already taken.
func (s *ResultSnapshots) insertTx(ctx context.Context, tx *sql.Tx, envelope map[string]any) error {
	payloadValue, ok := envelope["payload"].(map[string]any)
	if !ok {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	identifier, ok := snapshotText(payloadValue["result_id"])
	if !ok || !canonicalUUID(identifier) {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	namespace, ok := snapshotText(envelope["namespace"])
	if !ok {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	expires, ok := finiteSnapshotNumber(envelope["expires_at"])
	if !ok {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	created, ok := finiteSnapshotNumber(payloadValue["created_at"])
	if !ok || created >= expires {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	content, err := json.Marshal(envelope)
	if err != nil {
		return newSnapshotUnavailable("Search result storage is temporarily unavailable")
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT INTO search_result_snapshots (id, namespace, content, created_at, expires_at) "+
			"VALUES (?, ?, ?, ?, ?)",
		identifier, namespace, string(content), created, expires); err != nil {
		if isUniqueViolation(err) {
			return newSnapshotUnavailable("Search result ID already exists")
		}
		return err
	}
	return nil
}

// SaveTx prepares (or accepts) one result envelope and writes it into the
// caller's MySQL transaction, so a task terminal state and its result snapshot
// commit atomically. The Redis cache is not written here.
func (s *ResultSnapshots) SaveTx(ctx context.Context, tx *sql.Tx, namespace string, payload map[string]any,
	resultID *string, createdAt *float64) (map[string]any, error) {
	envelope, err := s.Prepare(namespace, payload, resultID, createdAt)
	if err != nil {
		return nil, err
	}
	if err := s.insertTx(ctx, tx, envelope); err != nil {
		return nil, err
	}
	payloadValue, ok := envelope["payload"].(map[string]any)
	if !ok {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	return payloadValue, nil
}

// Save prepares and stores one immutable response. MySQL is the fact source, so
// a successful save survives a complete Redis loss; the Redis cache write is
// best-effort.
func (s *ResultSnapshots) Save(namespace string, payload map[string]any, resultID *string, createdAt *float64) (map[string]any, error) {
	envelope, err := s.Prepare(namespace, payload, resultID, createdAt)
	if err != nil {
		return nil, err
	}
	persisted := false
	if s.Stores != nil && s.Stores.DB != nil {
		ctx := context.Background()
		transaction, err := s.Stores.DB.BeginTx(ctx, nil)
		if err != nil {
			return nil, newSnapshotUnavailable("Search result storage is temporarily unavailable")
		}
		if err := s.insertTx(ctx, transaction, envelope); err != nil {
			_ = transaction.Rollback()
			return nil, err
		}
		if err := transaction.Commit(); err != nil {
			return nil, newSnapshotUnavailable("Search result storage is temporarily unavailable")
		}
		persisted = true
	}
	if cacheErr := s.cacheEnvelope(envelope); cacheErr != nil && !persisted {
		return nil, cacheErr
	}
	payloadValue, ok := envelope["payload"].(map[string]any)
	if !ok {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	return payloadValue, nil
}

// cacheEnvelope writes one prepared envelope into the Redis read cache with the
// remaining TTL. It is best-effort: callers ignore the error because MySQL holds
// the authoritative row.
func (s *ResultSnapshots) cacheEnvelope(envelope map[string]any) error {
	if s.Transient == nil {
		return newSnapshotUnavailable("Search result storage is temporarily unavailable")
	}
	payloadValue, ok := envelope["payload"].(map[string]any)
	if !ok {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	identifier, ok := snapshotText(payloadValue["result_id"])
	if !ok {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	expires, ok := finiteSnapshotNumber(payloadValue["expires_at"])
	if !ok {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	ttl := math.Ceil(expires - s.now())
	if ttl <= 0 {
		return newSnapshotUnavailable("Search result expired or unavailable")
	}
	if _, err := s.Transient.Put("search-result", identifier, envelope, ttl, false); err != nil {
		return newSnapshotUnavailable("Search result storage is temporarily unavailable")
	}
	// 快照以 sha256 哈希 key 存进 transient，监控无法按前缀统计，因此保存成功
	// 后补记一次可选索引。索引失败只丢失观测数据，不影响已保存的快照和返回值。
	if s.Index != nil {
		_ = s.Index.AddSnapshot(context.Background(), identifier, expires)
	}
	return nil
}

// readEnvelope loads one result envelope from the MySQL fact table.
func (s *ResultSnapshots) readEnvelope(resultID string) (map[string]any, error) {
	if s.Stores == nil || s.Stores.DB == nil {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	var content string
	err := s.Stores.DB.QueryRowContext(context.Background(),
		"SELECT content FROM search_result_snapshots WHERE id = ?", resultID).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	if err != nil {
		return nil, newSnapshotUnavailable("Search result storage is temporarily unavailable")
	}
	var envelope map[string]any
	if err := json.Unmarshal([]byte(content), &envelope); err != nil {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	return envelope, nil
}

// Get pages one stored response, validating tenant, identity, and expiry. The
// Redis cache is consulted first; a miss, an unavailable cache, or a cache entry
// that fails validation (corrupt, stale, cross-tenant) falls back to the MySQL
// fact row, which is also used to repair the cache.
func (s *ResultSnapshots) Get(namespace, resultID string, page, pageSize int) (map[string]any, error) {
	if !canonicalUUID(resultID) {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, newValueError("page must be positive and page_size must be 1..100")
	}
	if s.Transient != nil {
		if value, found, err := s.Transient.Get("search-result", resultID); err == nil && found {
			if envelope, ok := value.(map[string]any); ok {
				if result, err := s.validatedResult(namespace, resultID, envelope); err == nil {
					return pageResult(result, page, pageSize)
				}
			}
		}
	}
	envelope, err := s.readEnvelope(resultID)
	if err != nil {
		return nil, err
	}
	result, err := s.validatedResult(namespace, resultID, envelope)
	if err != nil {
		return nil, err
	}
	// Best-effort repair so the next read hits the cache again.
	_ = s.cacheEnvelope(envelope)
	return pageResult(result, page, pageSize)
}

// pageResult applies the page/page_size projection to one validated payload.
func pageResult(result map[string]any, page, pageSize int) (map[string]any, error) {
	items, ok := result["results"].([]any)
	if !ok {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	total := len(items)
	start := (page - 1) * pageSize
	if start >= total && page != 1 {
		return nil, newValueError("Result page does not exist")
	}
	if start > total {
		start = total
	}
	end := start + pageSize
	if end > total {
		end = total
	}
	nextPage := any(nil)
	if start+pageSize < total {
		nextPage = page + 1
	}
	response := make(map[string]any, len(result)+5)
	for key, item := range result {
		response[key] = item
	}
	response["results"] = items[start:end]
	response["page"] = page
	response["page_size"] = pageSize
	response["total_results"] = total
	response["next_page"] = nextPage
	return response, nil
}

// validatedResult reproduces the defensive envelope validation of get().
func (s *ResultSnapshots) validatedResult(namespace, resultID string, envelope map[string]any) (map[string]any, error) {
	invalid := func() (map[string]any, error) {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	storedNamespace, ok := snapshotText(envelope["namespace"])
	if !ok || storedNamespace != namespace {
		return invalid()
	}
	expires, ok := finiteSnapshotNumber(envelope["expires_at"])
	if !ok {
		return invalid()
	}
	payload, ok := envelope["payload"].(map[string]any)
	if !ok {
		return invalid()
	}
	created, ok := finiteSnapshotNumber(payload["created_at"])
	if !ok || created >= expires {
		return invalid()
	}
	if expires <= s.now() {
		return invalid()
	}
	payloadExpires, ok := finiteSnapshotNumber(payload["expires_at"])
	if !ok || payloadExpires != expires {
		return invalid()
	}
	if identifier, ok := snapshotText(payload["result_id"]); !ok || identifier != resultID {
		return invalid()
	}
	if available, ok := payload["snapshot_available"].(bool); !ok || !available {
		return invalid()
	}
	if _, ok := payload["results"].([]any); !ok {
		return invalid()
	}
	encoded, err := json.Marshal(envelope)
	if err != nil || len(encoded) > s.Settings.MaxResultBytes {
		return invalid()
	}
	return payload, nil
}

// PurgeExpired deletes expired MySQL result snapshots in bounded batches. The
// Redis side relies on its TTL.
func (s *ResultSnapshots) PurgeExpired(limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, newValueError("limit must be 1..1000")
	}
	if s.Stores == nil || s.Stores.DB == nil {
		return 0, nil
	}
	result, err := s.Stores.DB.ExecContext(context.Background(),
		"DELETE FROM search_result_snapshots WHERE expires_at <= ? LIMIT ?", s.now(), limit)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}
