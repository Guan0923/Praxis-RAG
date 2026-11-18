package state

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Upload bloom filter modes. The mode is a deployment-wide rollout switch:
// every process that creates upload operations (rag-service,
// rag-document-worker, rag-mcp-ingest) must move through the modes together.
//
//   - off:      no bloom usage at all (default; behavior unchanged).
//   - register: new upload IDs are registered in Redis, but reads never trust
//               the filter. This is the first rollout step: upgrade every
//               writer first so no operation can be created unregistered.
//   - on:       registration continues and GET /v1/uploads/{operation_id} may
//               answer "definitely absent" from the filter. This is the second
//               step, activated only after every writer registers.
const (
	UploadBloomModeOff      = "off"
	UploadBloomModeRegister = "register"
	UploadBloomModeOn       = "on"
)

// BloomUnavailable reports that the bloom backend could not be consulted. It
// is sanitized like CacheUnavailable: readers fall back to MySQL, and writers
// run the registration-failure protocol.
type BloomUnavailable struct{ Message string }

// Error implements error.
func (e *BloomUnavailable) Error() string { return e.Message }

// NewBloomUnavailable builds the sanitized backend failure.
func NewBloomUnavailable() *BloomUnavailable {
	return &BloomUnavailable{Message: "upload bloom backend is unavailable"}
}

// BloomRegistrationFailed reports that a new upload ID could not be registered
// and the filter's negative judgment could not be confirmed as globally
// stopped. The creation is aborted and must be retried (same idempotency key
// where applicable); a committed operation must never become invisible.
type BloomRegistrationFailed struct{ Message string }

// Error implements error.
func (e *BloomRegistrationFailed) Error() string { return e.Message }

func newBloomRegistrationFailed() *BloomRegistrationFailed {
	return &BloomRegistrationFailed{
		Message: "Upload admission is temporarily unavailable; retry with the same idempotency key",
	}
}

// UploadBloomSettings configures the upload-operation bloom filter.
//
// The filter is an optimization for GET /v1/uploads/{operation_id} only: it
// answers "definitely absent" for random IDs so the upload-status SQL is not
// executed. Namespace access control is unchanged and stays with the MySQL
// lookup.
type UploadBloomSettings struct {
	// Mode is RAG_UPLOAD_BLOOM_MODE: off | register | on.
	Mode string
	// URL is RAG_UPLOAD_BLOOM_URL, falling back to RAG_REDIS_URL.
	URL string
	// Prefix is the Redis key namespace (RAG_UPLOAD_BLOOM_PREFIX).
	Prefix string
	// FalsePositive is the target false-positive rate (default 0.01).
	FalsePositive float64
	// Growth multiplies (history+1) into the planned capacity.
	Growth float64
	// MinCapacity floors the planned capacity so a small history still yields
	// a usable filter.
	MinCapacity int64
	// Shards splits the bitmap across Redis string keys.
	Shards int
	// TimeoutSeconds bounds one Redis round trip.
	TimeoutSeconds float64
	// Batch is the MySQL scan batch size while building a generation.
	Batch int
	// LockTTLSeconds bounds one generation build lease.
	LockTTLSeconds float64
	// CheckSeconds is the maintenance interval (rebuild/resize check).
	CheckSeconds float64
	// Rebuild forces a fresh generation at the next maintenance pass
	// (RAG_UPLOAD_BLOOM_REBUILD=1): use it after a database restore, a direct
	// record import, or restoring Redis from an old backup.
	Rebuild bool
}

// DefaultUploadBloomSettings returns the documented defaults.
func DefaultUploadBloomSettings() UploadBloomSettings {
	return UploadBloomSettings{
		Mode:           UploadBloomModeOff,
		Prefix:         "rag:uploadbloom:v1",
		FalsePositive:  0.01,
		Growth:         2.0,
		MinCapacity:    10000,
		Shards:         16,
		TimeoutSeconds: 0.3,
		Batch:          500,
		LockTTLSeconds: 600,
		CheckSeconds:   300,
	}
}

// UploadBloomSettingsFromEnv reads the RAG_UPLOAD_BLOOM_* contract in the same
// trimmed-and-validated style as the document lock settings. It never dials.
func UploadBloomSettingsFromEnv() (UploadBloomSettings, error) {
	settings := DefaultUploadBloomSettings()
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("RAG_UPLOAD_BLOOM_MODE"))); raw != "" {
		settings.Mode = raw
	}
	settings.URL = strings.TrimSpace(os.Getenv("RAG_UPLOAD_BLOOM_URL"))
	if settings.URL == "" {
		settings.URL = strings.TrimSpace(os.Getenv("RAG_REDIS_URL"))
	}
	settings.Prefix = strings.TrimSpace(envOrDefault("RAG_UPLOAD_BLOOM_PREFIX", settings.Prefix))
	fpr, err := parseFloatSetting("RAG_UPLOAD_BLOOM_FALSE_POSITIVE", "0.01")
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_FALSE_POSITIVE must be a number in (0, 0.5]")
	}
	settings.FalsePositive = fpr
	growth, err := parseFloatSetting("RAG_UPLOAD_BLOOM_GROWTH", "2.0")
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_GROWTH must be a positive finite number")
	}
	settings.Growth = growth
	minCapacity, err := strconv.ParseInt(strings.TrimSpace(envOrDefault("RAG_UPLOAD_BLOOM_MIN_CAPACITY", "10000")), 10, 64)
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_MIN_CAPACITY must be a positive integer")
	}
	settings.MinCapacity = minCapacity
	shards, err := strconv.Atoi(strings.TrimSpace(envOrDefault("RAG_UPLOAD_BLOOM_SHARDS", "16")))
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_SHARDS must be an integer between 1 and 1024")
	}
	settings.Shards = shards
	timeout, err := parseFloatSetting("RAG_UPLOAD_BLOOM_TIMEOUT_SECONDS", "0.3")
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_TIMEOUT_SECONDS must be a positive finite number")
	}
	settings.TimeoutSeconds = timeout
	batch, err := strconv.Atoi(strings.TrimSpace(envOrDefault("RAG_UPLOAD_BLOOM_BATCH", "500")))
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_BATCH must be a positive integer")
	}
	settings.Batch = batch
	lockTTL, err := parseFloatSetting("RAG_UPLOAD_BLOOM_LOCK_TTL_SECONDS", "600")
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_LOCK_TTL_SECONDS must be a positive finite number")
	}
	settings.LockTTLSeconds = lockTTL
	check, err := parseFloatSetting("RAG_UPLOAD_BLOOM_CHECK_SECONDS", "300")
	if err != nil {
		return UploadBloomSettings{}, fmt.Errorf("RAG_UPLOAD_BLOOM_CHECK_SECONDS must be a positive finite number")
	}
	settings.CheckSeconds = check
	settings.Rebuild = isTruthyValue(os.Getenv("RAG_UPLOAD_BLOOM_REBUILD"))
	if err := settings.Validate(); err != nil {
		return UploadBloomSettings{}, err
	}
	return settings, nil
}

// Validate rejects configurations that would silently disable the feature or
// produce a degenerate filter.
func (s UploadBloomSettings) Validate() error {
	switch s.Mode {
	case UploadBloomModeOff, UploadBloomModeRegister, UploadBloomModeOn:
	default:
		return fmt.Errorf("RAG_UPLOAD_BLOOM_MODE must be one of %q, %q, %q",
			UploadBloomModeOff, UploadBloomModeRegister, UploadBloomModeOn)
	}
	if s.Mode != UploadBloomModeOff && s.URL == "" {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_MODE=%s requires RAG_UPLOAD_BLOOM_URL or RAG_REDIS_URL", s.Mode)
	}
	if s.FalsePositive <= 0 || s.FalsePositive > 0.5 || math.IsNaN(s.FalsePositive) || math.IsInf(s.FalsePositive, 0) {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_FALSE_POSITIVE must be a number in (0, 0.5]")
	}
	if s.Growth < 1 || math.IsNaN(s.Growth) || math.IsInf(s.Growth, 0) {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_GROWTH must be at least 1")
	}
	if s.MinCapacity < 1 {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_MIN_CAPACITY must be a positive integer")
	}
	if s.Shards < 1 || s.Shards > 1024 {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_SHARDS must be an integer between 1 and 1024")
	}
	if s.TimeoutSeconds <= 0 || math.IsNaN(s.TimeoutSeconds) || math.IsInf(s.TimeoutSeconds, 0) {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_TIMEOUT_SECONDS must be a positive finite number")
	}
	if s.Batch < 1 {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_BATCH must be a positive integer")
	}
	if s.LockTTLSeconds <= 0 || math.IsNaN(s.LockTTLSeconds) || math.IsInf(s.LockTTLSeconds, 0) {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_LOCK_TTL_SECONDS must be a positive finite number")
	}
	if s.CheckSeconds <= 0 || math.IsNaN(s.CheckSeconds) || math.IsInf(s.CheckSeconds, 0) {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_CHECK_SECONDS must be a positive finite number")
	}
	if s.Prefix == "" {
		return fmt.Errorf("RAG_UPLOAD_BLOOM_PREFIX must not be empty")
	}
	return nil
}

// Registers reports whether new upload IDs must be registered into the
// filter.
func (s UploadBloomSettings) Registers() bool { return s.Mode == UploadBloomModeRegister || s.Mode == UploadBloomModeOn }

// GatesReads reports whether the upload-status read path may trust the
// filter's "definitely absent" answer.
func (s UploadBloomSettings) GatesReads() bool { return s.Mode == UploadBloomModeOn }

// UploadBloomParameters is the deployed geometry of one filter generation.
// It is persisted in Redis (the meta/building keys) so every instance derives
// identical bit positions regardless of its local configuration.
type UploadBloomParameters struct {
	Bits     int64 `json:"bits"`
	Shards   int   `json:"shards"`
	Hashes   int   `json:"hashes"`
	Capacity int64 `json:"capacity"`
}

// BitsPerShard returns the per-shard bit count; Bits is shard-aligned by
// construction so the division is exact.
func (p UploadBloomParameters) BitsPerShard() int64 { return p.Bits / int64(p.Shards) }

// UploadBloomParametersValid reports whether a stored geometry is usable
// (public wrapper of the internal validity check, used by backends).
func UploadBloomParametersValid(p UploadBloomParameters) bool {
	return p.Bits >= int64(p.Shards)*8 && p.Shards >= 1 && p.Hashes >= 1 && p.Hashes <= 64 &&
		p.Bits%int64(p.Shards) == 0 && p.Capacity >= 1
}

// valid rejects a degenerate or inconsistent geometry.
func (p UploadBloomParameters) valid() bool { return UploadBloomParametersValid(p) }

// uploadBloomParametersFor computes the geometry for one planned capacity.
//
// bits = -n·ln(p)/(ln2)² and hashes = (bits/n)·ln2, both rounded so the 1%
// target holds; the bit array is then rounded up to a byte- and shard-aligned
// size so every shard is a whole Redis string.
func (s UploadBloomSettings) uploadBloomParametersFor(capacity int64) UploadBloomParameters {
	if capacity < 1 {
		capacity = 1
	}
	ln2 := math.Log(2)
	bitsPerEntry := -math.Log(s.FalsePositive) / (ln2 * ln2)
	bits := math.Ceil(float64(capacity) * bitsPerEntry)
	hashes := int(math.Round(bitsPerEntry * ln2))
	if hashes < 1 {
		hashes = 1
	}
	if hashes > 32 {
		hashes = 32
	}
	bitsPerShard := (bits + float64(s.Shards) - 1) / float64(s.Shards)
	bitsPerShard = math.Ceil(bitsPerShard/8) * 8
	total := int64(bitsPerShard) * int64(s.Shards)
	if total < int64(s.Shards)*8 {
		total = int64(s.Shards) * 8
	}
	return UploadBloomParameters{Bits: total, Shards: s.Shards, Hashes: hashes, Capacity: capacity}
}

// UploadBloomBitPositions derives the k bit positions of one operation ID.
//
// The scheme is Kirsch–Mitzenmacher double hashing over sha256(id): positions
// are (h1 + i·h2) mod bits. It is deterministic across processes and versions;
// changing it invalidates every stored generation.
func UploadBloomBitPositions(params UploadBloomParameters, id string) []int64 {
	digest := sha256.Sum256([]byte(id))
	h1 := binary.BigEndian.Uint64(digest[0:8])
	h2 := binary.BigEndian.Uint64(digest[8:16])
	if h2 == 0 {
		h2 = 1
	}
	bits := uint64(params.Bits)
	positions := make([]int64, params.Hashes)
	carry := h1 % bits
	step := h2 % bits
	if step == 0 {
		step = 1
	}
	for index := range positions {
		positions[index] = int64(carry)
		carry = (carry + step) % bits
	}
	return positions
}

// UploadBloomGeneration identifies one filter generation (number + geometry).
type UploadBloomGeneration struct {
	Generation int64                  `json:"generation"`
	Params     UploadBloomParameters `json:"params"`
}

// UploadBloomState is the backend's consolidated view: the active generation
// (nil when not ready), an in-progress rebuild (nil when none), and the global
// kill switch.
type UploadBloomState struct {
	Active   *UploadBloomGeneration
	Building *UploadBloomGeneration
	Disabled bool
}

// UploadBloomBackend is the Redis bitmap storage behind the filter. The
// production implementation lives in internal/redisstate; tests inject fakes.
//
// Every method must be safe for concurrent use and report failures as
// *BloomUnavailable so callers fall back to MySQL instead of failing reads.
type UploadBloomBackend interface {
	Enabled() bool
	Ping(ctx context.Context) error
	Close() error
	// State reads the kill switch, the active generation and the building
	// generation in one round trip.
	State(ctx context.Context) (UploadBloomState, error)
	// SetBits registers IDs in one generation.
	SetBits(ctx context.Context, generation UploadBloomGeneration, ids []string) error
	// Probe reports whether the ID is present; shardMissing=true means part of
	// the bitmap was lost and the caller must fall back to MySQL.
	Probe(ctx context.Context, generation UploadBloomGeneration, id string) (present bool, shardMissing bool, err error)
	// AcquireBuild takes the generation build lease and pins its parameters.
	// It returns the effective generation (an existing pin wins so concurrent
	// or restarted builders agree on the geometry) and false when another
	// instance is already building.
	AcquireBuild(ctx context.Context, generation int64, params UploadBloomParameters, ttl time.Duration) (UploadBloomGeneration, bool, error)
	// RenewBuild extends the build lease; losing it aborts the build.
	RenewBuild(ctx context.Context, generation int64, ttl time.Duration) error
	// ReleaseBuild drops the build lease.
	ReleaseBuild(ctx context.Context, generation int64) error
	// Activate publishes the new generation atomically (single meta SET) and
	// clears the kill switch afterwards.
	Activate(ctx context.Context, generation UploadBloomGeneration) error
	// Disable sets the global kill switch and verifies it is visible; the
	// returned bool is only true when every instance will see it.
	Disable(ctx context.Context) (bool, error)
	// ExpireGeneration retires a superseded generation's shards.
	ExpireGeneration(ctx context.Context, generation UploadBloomGeneration, ttl time.Duration) error
}

// uploadBloomRetireTTL keeps a superseded generation's bitmap around while
// in-flight readers that fetched the old meta still probe it.
const uploadBloomRetireTTL = 24 * time.Hour

// uploadBloomBuildMaxRounds bounds the scan/confirm rounds of one build.
const uploadBloomBuildMaxRounds = 4

// UploadBloomFilter is the deployment's upload-operation bloom filter: the
// write-side registration, the read-side gate and the generation builder.
//
// MySQL stays the only source of truth: the filter never decides existence for
// an ID it says is "possibly present", and every fallback path is the original
// MySQL query.
type UploadBloomFilter struct {
	settings UploadBloomSettings
	backend  UploadBloomBackend
	db       *sql.DB

	buildMutex sync.Mutex
}

// newUploadBloomFilter wires a filter over one backend.
func newUploadBloomFilter(settings UploadBloomSettings, db *sql.DB, backend UploadBloomBackend) (*UploadBloomFilter, error) {
	if backend == nil {
		return nil, newValueError("upload bloom backend is required")
	}
	if err := settings.Validate(); err != nil {
		return nil, newValueError(err.Error())
	}
	return &UploadBloomFilter{settings: settings, backend: backend, db: db}, nil
}

// NewUploadBloomFilterWith wires the filter onto an existing Stores (used by
// the process entry points after the backend is constructed).
func NewUploadBloomFilterWith(stores *Stores, settings UploadBloomSettings, backend UploadBloomBackend) *UploadBloomFilter {
	if stores == nil {
		return nil
	}
	filter, err := newUploadBloomFilter(settings, stores.DB, backend)
	if err != nil {
		return nil
	}
	return filter
}

// RunMaintenance builds, resizes and repairs the filter periodically. It is
// the long-lived maintenance loop (service startup + periodic check): the
// first pass builds the initial generation, and later passes only act when
// the history outgrows the capacity or a forced rebuild was requested.
func (f *UploadBloomFilter) RunMaintenance(ctx context.Context, logger *slog.Logger) {
	if !f.backendEnabled() {
		return
	}
	interval := time.Duration(f.settings.CheckSeconds * float64(time.Second))
	for {
		if err := f.Ensure(ctx); err != nil {
			if logger != nil {
				logger.Warn("Upload bloom maintenance failed; the read gate stays disabled until the next pass", "error", err.Error())
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}

// Settings exposes the effective configuration (no secrets).
func (f *UploadBloomFilter) Settings() UploadBloomSettings { return f.settings }

// Close releases the backend.
func (f *UploadBloomFilter) Close() error { return f.backend.Close() }

// backendEnabled reports whether the backend is usable in this process.
func (f *UploadBloomFilter) backendEnabled() bool {
	return f != nil && f.backend != nil && f.backend.Enabled()
}

// Register records a new upload operation ID before its MySQL transaction
// commits, so a committed operation can never be reported as absent.
//
// A rolled-back transaction only leaves a false positive (one extra query
// later). A registration failure never returns nil silently: the caller must
// run the failure protocol (global disable) or abort the creation.
func (f *UploadBloomFilter) Register(ctx context.Context, operationID string) error {
	if !f.backendEnabled() || !f.settings.Registers() {
		return nil
	}
	current, err := f.backend.State(ctx)
	if err != nil {
		return err
	}
	if current.Disabled {
		// The negative judgment is globally stopped; the next rebuild (a full
		// MySQL rescan) will cover this ID, so committing stays safe.
		return nil
	}
	targets := make([]UploadBloomGeneration, 0, 2)
	if current.Active != nil {
		targets = append(targets, *current.Active)
	}
	if current.Building != nil {
		targets = append(targets, *current.Building)
	}
	if len(targets) == 0 {
		// No generation exists yet (first build not finished): the initial
		// build scans MySQL, and any race window is closed by its confirming
		// rounds plus this process's own registration once a generation
		// appears.
		return nil
	}
	for _, target := range targets {
		if err := f.backend.SetBits(ctx, target, []string{operationID}); err != nil {
			return err
		}
	}
	return nil
}

// DefinitelyAbsent reports whether the filter can prove the operation ID does
// not exist. It answers false (fall back to MySQL) whenever anything is not
// provable: the read gate is off, the ID is not in canonical lowercase form,
// the filter is not ready, the kill switch is set, a shard was lost, or the
// backend cannot be read.
func (f *UploadBloomFilter) DefinitelyAbsent(ctx context.Context, operationID string) bool {
	if !f.backendEnabled() || !f.settings.GatesReads() {
		return false
	}
	// MySQL compares upload_operations.id under utf8mb4_bin today, but a
	// differently-cased spelling must not become a fast 404 if the collation
	// ever changes: any non-canonical probe bypasses the filter and keeps the
	// historical query behavior.
	if operationID != strings.ToLower(operationID) {
		return false
	}
	current, err := f.backend.State(ctx)
	if err != nil || current.Disabled || current.Active == nil || !current.Active.Params.valid() {
		return false
	}
	present, shardMissing, err := f.backend.Probe(ctx, *current.Active, operationID)
	if err != nil || shardMissing {
		return false
	}
	return !present
}

// Disable stops the filter's negative judgment globally. It returns true only
// when the kill switch is confirmed visible to every instance.
func (f *UploadBloomFilter) Disable(ctx context.Context) (bool, error) {
	if !f.backendEnabled() {
		return false, NewBloomUnavailable()
	}
	return f.backend.Disable(ctx)
}

// registerOperationID is the journal-side registration with the required
// failure protocol: if the ID cannot be registered, the filter's negative
// judgment must be confirmed globally stopped before the upload transaction
// may commit; otherwise the creation fails retryably.
func (f *UploadBloomFilter) registerOperationID(ctx context.Context, operationID string) error {
	if err := f.Register(ctx, operationID); err == nil {
		return nil
	}
	if confirmed, disableErr := f.Disable(ctx); disableErr == nil && confirmed {
		// The filter is now inert everywhere; committing is safe and the next
		// maintenance pass rebuilds it from MySQL.
		return nil
	}
	return newBloomRegistrationFailed()
}

// uploadOperationCount returns the number of upload operations in MySQL.
func (f *UploadBloomFilter) uploadOperationCount(ctx context.Context) (int64, error) {
	var count int64
	if err := f.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM upload_operations").Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}

// Ensure builds, resizes and repairs the filter. It is the maintenance entry
// point (service startup + periodic check); concurrent instances arbitrate
// through the backend's build lease.
//
// The build keeps serving the previous generation while the new one fills:
// registrations go to both, the full MySQL scan plus confirming rounds close
// the build window, and the switch itself is one atomic meta SET. A forced
// rebuild (settings.Rebuild) first sets the kill switch so a stale or suspect
// bitmap can never answer "definitely absent" during the rebuild.
func (f *UploadBloomFilter) Ensure(ctx context.Context) error {
	if !f.backendEnabled() || f.db == nil {
		return nil
	}
	f.buildMutex.Lock()
	defer f.buildMutex.Unlock()
	current, err := f.backend.State(ctx)
	if err != nil {
		return err
	}
	count, err := f.uploadOperationCount(ctx)
	if err != nil {
		return err
	}
	need := current.Active == nil || current.Disabled || count > current.Active.Params.Capacity
	if f.settings.Rebuild {
		// Recovery path (database restore, direct import, Redis restored from
		// an old backup): stop the negative judgment before rebuilding.
		if current.Active != nil {
			if _, disableErr := f.Disable(ctx); disableErr != nil {
				return disableErr
			}
			current.Disabled = true
		}
		need = true
	}
	if !need {
		return nil
	}
	next := count + 1
	if next < f.settings.MinCapacity {
		next = f.settings.MinCapacity
	}
	capacity := int64(math.Ceil(float64(next) * f.settings.Growth))
	params := f.settings.uploadBloomParametersFor(capacity)
	generation := int64(1)
	if current.Active != nil && current.Active.Generation >= generation {
		generation = current.Active.Generation + 1
	}
	if current.Building != nil && current.Building.Generation >= generation {
		generation = current.Building.Generation + 1
	}
	building, acquired, err := f.backend.AcquireBuild(ctx, generation, params,
		time.Duration(f.settings.LockTTLSeconds*float64(time.Second)))
	if err != nil {
		return err
	}
	if !acquired {
		// Another instance is building this generation; it will activate it.
		return nil
	}
	// A restarted builder adopts the pinned geometry, which can differ from
	// what this instance computed: always build with the pinned parameters.
	if err := f.buildGeneration(ctx, building); err != nil {
		_ = f.backend.ReleaseBuild(ctx, building.Generation)
		return err
	}
	if err := f.backend.Activate(ctx, building); err != nil {
		_ = f.backend.ReleaseBuild(ctx, building.Generation)
		return err
	}
	_ = f.backend.ReleaseBuild(ctx, building.Generation)
	if current.Active != nil && current.Active.Generation != building.Generation {
		if err := f.backend.ExpireGeneration(ctx, *current.Active, uploadBloomRetireTTL); err != nil {
			return err
		}
	}
	return nil
}

// buildGeneration scans MySQL into one generation. The first round is the
// full scan; each following round must observe zero new IDs before the
// generation may be activated (bounded by uploadBloomBuildMaxRounds).
func (f *UploadBloomFilter) buildGeneration(ctx context.Context, building UploadBloomGeneration) error {
	seen := make(map[string]struct{})
	batch := f.settings.Batch
	lockTTL := time.Duration(f.settings.LockTTLSeconds * float64(time.Second))
	previousSize := -1
	for round := 0; round < uploadBloomBuildMaxRounds; round++ {
		if err := f.scanUploadOperationIDs(ctx, building, seen, batch, lockTTL); err != nil {
			return err
		}
		if len(seen) == previousSize {
			// No ID appeared since the previous round: the build window is
			// closed for committed rows, and every Begin that started during
			// the build registered into this generation directly.
			return nil
		}
		previousSize = len(seen)
	}
	return nil
}

// scanUploadOperationIDs performs one full keyset-paginated scan of
// upload_operations, registering the IDs earlier rounds have not seen.
func (f *UploadBloomFilter) scanUploadOperationIDs(
	ctx context.Context,
	building UploadBloomGeneration,
	seen map[string]struct{},
	batch int,
	lockTTL time.Duration,
) error {
	lastID := ""
	pending := make([]string, 0, batch)
	flush := func() error {
		if len(pending) == 0 {
			return nil
		}
		if err := f.backend.SetBits(ctx, building, pending); err != nil {
			return err
		}
		pending = pending[:0]
		return f.backend.RenewBuild(ctx, building.Generation, lockTTL)
	}
	for {
		rows, err := f.db.QueryContext(ctx,
			"SELECT id FROM upload_operations WHERE id > ? ORDER BY id LIMIT ?", lastID, batch)
		if err != nil {
			return err
		}
		scanned := 0
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				_ = rows.Close()
				return err
			}
			lastID = id
			scanned++
			if _, duplicate := seen[id]; duplicate {
				continue
			}
			seen[id] = struct{}{}
			pending = append(pending, id)
			if len(pending) >= batch {
				if err := flush(); err != nil {
					_ = rows.Close()
					return err
				}
			}
		}
		if err := rows.Err(); err != nil {
			_ = rows.Close()
			return err
		}
		if err := rows.Close(); err != nil {
			return err
		}
		if scanned < batch {
			break
		}
	}
	return flush()
}

// bloomRegisterGate is the upload-journal entry point used by both Begin
// implementations (MySQL-lock legacy path and Redis-lock registration path).
func (j *UploadJournal) registerOperationID(ctx context.Context, operationID string) error {
	filter := j.stores.UploadBloom
	if filter == nil {
		return nil
	}
	return filter.registerOperationID(ctx, operationID)
}
