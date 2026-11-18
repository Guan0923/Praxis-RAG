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

// DocumentLockSettings configures the cross-instance document lease backend.
// Redis is the only backend: the long-lived lease lives in Redis while MySQL
// keeps the holder registration and a monotonically increasing fencing epoch,
// which is what every destructive SQL transaction verifies.
//
// The display cache (RAG_REDIS_*) and the document lock are independent:
// disabling the cache must not disable the lock, and the two TTLs are separate.
// The legacy RAG_DOCUMENT_LOCK_MODE switch is ignored: even when an old
// environment still exports it, the program always uses the Redis backend.
type DocumentLockSettings struct {
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

// DefaultDocumentLockSettings returns the documented defaults. URL stays empty
// until the environment (or an explicit option) resolves it; a journal without
// a URL and without an injected backend reports *LockUnavailable on every
// write path that needs the lease instead of falling back to MySQL leases.
func DefaultDocumentLockSettings() DocumentLockSettings {
	return DocumentLockSettings{
		Prefix:         "rag:lock:v1",
		TTLSeconds:     120,
		TimeoutSeconds: 1,
		WaitSeconds:    2,
	}
}

// documentLockURLFromEnv resolves RAG_REDIS_LOCK_URL, falling back to
// RAG_REDIS_URL. An empty result means the deployment has no lock address.
func documentLockURLFromEnv() string {
	url := strings.TrimSpace(os.Getenv("RAG_REDIS_LOCK_URL"))
	if url == "" {
		url = strings.TrimSpace(os.Getenv("RAG_REDIS_URL"))
	}
	return url
}

// DocumentLockSettingsFromEnv reads the RAG_REDIS_LOCK_* contract. Redis is the
// required document lock backend, so a missing address is a configuration
// error: the process must fail to start rather than silently lose mutual
// exclusion. It mirrors CacheSettingsFromEnv style: trimmed values, explicit
// validation and no dialing.
func DocumentLockSettingsFromEnv() (DocumentLockSettings, error) {
	settings := DefaultDocumentLockSettings()
	settings.URL = documentLockURLFromEnv()
	if settings.URL == "" {
		return DocumentLockSettings{}, fmt.Errorf("RAG_REDIS_LOCK_URL or RAG_REDIS_URL is required: Redis is the document lock backend")
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
	if s.URL == "" {
		return fmt.Errorf("RAG_REDIS_LOCK_URL or RAG_REDIS_URL is required: Redis is the document lock backend")
	}
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
	return nil
}

// TTL returns the lease duration as a time.Duration.
func (s DocumentLockSettings) TTL() time.Duration {
	return time.Duration(s.TTLSeconds * float64(time.Second))
}

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
