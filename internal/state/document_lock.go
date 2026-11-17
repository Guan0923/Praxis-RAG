package state

import (
	"context"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"time"
)

// Document lock modes. The mode is a deployment-wide switch: every process that
// writes, recovers or cleans document state must use the same mode, because a
// fleet that mixes both backends has no mutual exclusion at all.
const (
	// DocumentLockModeMySQL is the legacy backend: document_leases rows hold the
	// long-lived lease and every heartbeat renews expires_at over SQL.
	DocumentLockModeMySQL = "mysql"
	// DocumentLockModeRedis moves the long-lived lease to Redis. MySQL keeps the
	// holder registration and a monotonically increasing fencing epoch, which is
	// what every destructive SQL transaction verifies.
	DocumentLockModeRedis = "redis"
)

// DocumentLockSettings configures the cross-instance document lease backend.
//
// The display cache (RAG_REDIS_*) and the document lock are independent:
// disabling the cache must not disable the lock, and the two TTLs are separate.
type DocumentLockSettings struct {
	// Mode is RAG_DOCUMENT_LOCK_MODE: "mysql" (default) or "redis".
	Mode string
	// URL is RAG_REDIS_LOCK_URL, falling back to RAG_REDIS_URL.
	URL string
	// Prefix is the lock key namespace (RAG_REDIS_LOCK_PREFIX).
	Prefix string
	// TTLSeconds is the lease length (RAG_REDIS_LOCK_TTL_SECONDS, default 120).
	TTLSeconds float64
	// TimeoutSeconds bounds one Redis round trip.
	TimeoutSeconds float64
	// WaitSeconds bounds how long an acquisition retries an unavailable backend
	// before reporting a retryable error. Lock conflicts never wait: they keep
	// the pre-migration conflict response.
	WaitSeconds float64
}

// DefaultDocumentLockSettings returns the documented defaults.
func DefaultDocumentLockSettings() DocumentLockSettings {
	return DocumentLockSettings{
		Mode:           DocumentLockModeMySQL,
		Prefix:         "rag:lock:v1",
		TTLSeconds:     120,
		TimeoutSeconds: 1,
		WaitSeconds:    2,
	}
}

// DocumentLockSettingsFromEnv reads the RAG_DOCUMENT_LOCK_*/RAG_REDIS_LOCK_*
// contract. It mirrors CacheSettingsFromEnv style: trimmed values, explicit
// validation and no dialing.
func DocumentLockSettingsFromEnv() (DocumentLockSettings, error) {
	settings := DefaultDocumentLockSettings()
	if raw := strings.ToLower(strings.TrimSpace(os.Getenv("RAG_DOCUMENT_LOCK_MODE"))); raw != "" {
		settings.Mode = raw
	}
	settings.URL = strings.TrimSpace(os.Getenv("RAG_REDIS_LOCK_URL"))
	if settings.URL == "" {
		settings.URL = strings.TrimSpace(os.Getenv("RAG_REDIS_URL"))
	}
	settings.Prefix = strings.TrimSpace(envOrDefault("RAG_REDIS_LOCK_PREFIX", settings.Prefix))
	ttl, err := parseFloatSetting("RAG_REDIS_LOCK_TTL_SECONDS", "120")
	if err != nil {
		return DocumentLockSettings{}, err
	}
	settings.TTLSeconds = ttl
	timeout, err := parseFloatSetting("RAG_REDIS_LOCK_TIMEOUT_SECONDS", "1")
	if err != nil {
		return DocumentLockSettings{}, err
	}
	settings.TimeoutSeconds = timeout
	wait, err := parseFloatSetting("RAG_REDIS_LOCK_WAIT_SECONDS", "2")
	if err != nil {
		return DocumentLockSettings{}, err
	}
	settings.WaitSeconds = wait
	if lease := strings.TrimSpace(os.Getenv("RAG_UPLOAD_LEASE_SECONDS")); lease != "" {
		if _, present := os.LookupEnv("RAG_REDIS_LOCK_TTL_SECONDS"); !present {
			parsed, parseErr := parseFloatSetting("RAG_UPLOAD_LEASE_SECONDS", lease)
			if parseErr != nil {
				return DocumentLockSettings{}, parseErr
			}
			settings.TTLSeconds = parsed
		}
	}
	if err := settings.Validate(); err != nil {
		return DocumentLockSettings{}, err
	}
	return settings, nil
}

func parseFloatSetting(name, fallback string) (float64, error) {
	raw := strings.TrimSpace(envOrDefault(name, fallback))
	value, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive finite number", name)
	}
	return value, nil
}

// Validate rejects configurations that would silently disable mutual exclusion.
func (s DocumentLockSettings) Validate() error {
	switch s.Mode {
	case DocumentLockModeMySQL:
	case DocumentLockModeRedis:
		if s.URL == "" {
			return fmt.Errorf("RAG_DOCUMENT_LOCK_MODE=redis requires RAG_REDIS_LOCK_URL or RAG_REDIS_URL")
		}
	default:
		return fmt.Errorf("RAG_DOCUMENT_LOCK_MODE must be %q or %q", DocumentLockModeMySQL, DocumentLockModeRedis)
	}
	if s.Mode == DocumentLockModeRedis {
		if math.IsNaN(s.TTLSeconds) || math.IsInf(s.TTLSeconds, 0) || s.TTLSeconds <= 0 {
			return fmt.Errorf("RAG_REDIS_LOCK_TTL_SECONDS must be a positive finite number")
		}
		if math.IsNaN(s.TimeoutSeconds) || math.IsInf(s.TimeoutSeconds, 0) || s.TimeoutSeconds <= 0 {
			return fmt.Errorf("RAG_REDIS_LOCK_TIMEOUT_SECONDS must be a positive finite number")
		}
		if math.IsNaN(s.WaitSeconds) || math.IsInf(s.WaitSeconds, 0) || s.WaitSeconds < 0 {
			return fmt.Errorf("RAG_REDIS_LOCK_WAIT_SECONDS must be a finite nonnegative number")
		}
		if s.Prefix == "" {
			return fmt.Errorf("RAG_REDIS_LOCK_PREFIX must not be empty")
		}
	}
	return nil
}

// TTL returns the lease duration as a time.Duration.
func (s DocumentLockSettings) TTL() time.Duration {
	return time.Duration(s.TTLSeconds * float64(time.Second))
}

// RedisMode reports whether the long-lived document lease lives in Redis.
func (s DocumentLockSettings) RedisMode() bool { return s.Mode == DocumentLockModeRedis }

// LockBusy reports that another live holder owns at least one requested
// resource. It keeps the pre-migration conflict contract (callers map it to
// *UploadConflict), so a concurrent upload still fails fast instead of waiting.
type LockBusy struct{ Message string }

// Error implements error.
func (e *LockBusy) Error() string { return e.Message }

// NewLockBusy builds a busy error; it is exported so backends can report the
// same typed conflict the journal maps onto UploadConflict.
func NewLockBusy() *LockBusy {
	return &LockBusy{Message: "document resource is leased"}
}

// LockUnavailable reports that the lease backend could not confirm ownership
// (Redis unreachable, timeout, no backend attached). It is retryable and must
// never be treated as "the lease is free": callers stop starting new side
// effects and retry later.
type LockUnavailable struct{ Message string }

// Error implements error.
func (e *LockUnavailable) Error() string { return e.Message }

// NewLockUnavailable builds a sanitized unavailable error.
func NewLockUnavailable() *LockUnavailable {
	return &LockUnavailable{Message: "document lock backend is unavailable"}
}

// LockState is the display projection of one live lease. It never carries the
// raw owner token: OwnerDigest is the same short hash the lease read model
// already exposes.
type LockState struct {
	OwnerDigest string
	TTL         time.Duration
}

// Live reports whether the lease still holds.
func (s LockState) Live() bool { return s.TTL > 0 && s.OwnerDigest != "" }

// DocumentLocker is the cross-instance lease backend for document resources.
//
// Implementations must be safe for concurrent use, must keep one acquisition's
// resources all-or-nothing (no partial lease left behind on conflict), and must
// use the caller's owner token for every conditional renew/release.
type DocumentLocker interface {
	// Acquire leases every resource atomically for ttl. A live foreign holder
	// returns *LockBusy; a backend failure returns *LockUnavailable. Re-acquiring
	// resources already owned by owner refreshes them.
	Acquire(ctx context.Context, namespace string, resources []string, owner string, ttl time.Duration) error
	// Renew extends every resource. Losing ownership returns *LeaseLost; a
	// backend failure returns *LockUnavailable.
	Renew(ctx context.Context, namespace string, resources []string, owner string, ttl time.Duration) error
	// Release removes only resources still owned by owner and is idempotent.
	Release(ctx context.Context, namespace string, resources []string, owner string) error
	// Inspect reports one resource's live lease for display.
	Inspect(ctx context.Context, namespace, resource string) (LockState, error)
	// Ping checks backend connectivity for readiness probes.
	Ping(ctx context.Context) error
	// Close releases backend resources.
	Close() error
}

// DocumentLockInspector is the display-only subset of DocumentLocker used by the
// cached lease read model. Keeping it narrow makes the read path's dependency
// explicit: it can query liveness, never acquire or release.
type DocumentLockInspector interface {
	Inspect(ctx context.Context, namespace, resource string) (LockState, error)
}
