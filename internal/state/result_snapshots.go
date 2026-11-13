package state

import (
	"context"
	"encoding/json"
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

// TransientStore is the expiring key/value storage used for result snapshots.
// In production it is the Redis-backed rag_mcp.state.transient.TransientStore;
// the Go port keeps it behind this interface so no Redis dependency is needed.
// Values must be JSON-marshalable maps.
type TransientStore interface {
	Get(kind, identity string) (any, bool, error)
	Put(kind, identity string, value any, ttlSeconds float64, nx bool) (bool, error)
	Delete(kind, identity string) (int64, error)
}

// TransientProvider is implemented by a cache backend (the future Redis client)
// that can also serve expiring transient values. The adapter is a separate view
// because StateCache and TransientStore have different Get/Put shapes.
type TransientProvider interface {
	TransientStore() TransientStore
}

// SnapshotIndex 可选地记录每个已保存结果快照的身份与过期时间，供监控页统计
// 有效快照数。快照本身存放在 Redis transient 的 sha256 哈希 key 里，无法按
// 前缀扫描，所以由后端额外维护一份可计数的索引（见 redisstate 的实现）。
//
// 它必须可并发调用；索引失败只影响观测数据，绝不能让快照保存失败。
type SnapshotIndex interface {
	// AddSnapshot 记录一条快照身份及过期时间（Unix 秒会话，可带小数）。
	AddSnapshot(ctx context.Context, identity string, expiresAt float64) error
}

// ResultSnapshots stores immutable, tenant-scoped search results. It is a port
// of rag_mcp.state.result_snapshots.ResultSnapshots.
type ResultSnapshots struct {
	Stores    *Stores
	Settings  SnapshotSettings
	Clock     func() float64
	Transient TransientStore
	// Index 是可选的快照索引；nil 时 Save 行为与引入索引前完全一致。
	Index SnapshotIndex
}

// NewResultSnapshots builds the snapshot service. A nil clock uses wall time; a
// nil Transient store makes Save/Get report SnapshotUnavailable, mirroring a
// disabled Redis backend.
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

// Save prepares and stores one immutable response.
func (s *ResultSnapshots) Save(namespace string, payload map[string]any, resultID *string, createdAt *float64) (map[string]any, error) {
	envelope, err := s.Prepare(namespace, payload, resultID, createdAt)
	if err != nil {
		return nil, err
	}
	payloadValue, ok := envelope["payload"].(map[string]any)
	if !ok {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	result := payloadValue
	if s.Transient == nil {
		return nil, newSnapshotUnavailable("Search result storage is temporarily unavailable")
	}
	expires, _ := finiteSnapshotNumber(result["expires_at"])
	ttl := math.Ceil(expires - s.now())
	identifier, _ := snapshotText(result["result_id"])
	stored, err := s.Transient.Put("search-result", identifier, envelope, ttl, true)
	if err != nil {
		return nil, newSnapshotUnavailable("Search result storage is temporarily unavailable")
	}
	if !stored {
		return nil, newSnapshotUnavailable("Search result ID already exists")
	}
	// 快照以 sha256 哈希 key 存进 transient，监控无法按前缀统计，因此保存成功
	// 后补记一次可选索引。索引失败只丢失观测数据，不影响已保存的快照和返回值。
	if s.Index != nil {
		_ = s.Index.AddSnapshot(context.Background(), identifier, expires)
	}
	return result, nil
}

// Get pages one stored response, validating tenant, identity, and expiry.
func (s *ResultSnapshots) Get(namespace, resultID string, page, pageSize int) (map[string]any, error) {
	if !canonicalUUID(resultID) {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	if page < 1 || pageSize < 1 || pageSize > 100 {
		return nil, newValueError("page must be positive and page_size must be 1..100")
	}
	if s.Transient == nil {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	value, found, err := s.Transient.Get("search-result", resultID)
	if err != nil || !found {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	envelope, ok := value.(map[string]any)
	if !ok {
		return nil, newSnapshotUnavailable("Search result expired or unavailable")
	}
	result, err := s.validatedResult(namespace, resultID, envelope)
	if err != nil {
		return nil, err
	}
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

// PurgeExpired is a bounded compatibility no-op: Redis TTL owns reclamation.
func (s *ResultSnapshots) PurgeExpired(limit int) (int, error) {
	if limit < 1 || limit > 1000 {
		return 0, newValueError("limit must be 1..1000")
	}
	return 0, nil
}
