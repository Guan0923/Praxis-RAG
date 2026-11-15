// Package connect integrates the distributed Kafka Connect deployment that runs
// the two Debezium MySQL capture connectors (document tasks and cache
// read-model tables).
//
// It owns three things the old Debezium Server layout did not need:
//
//   - an idempotent connector registrar (PUT by fixed name) so repeated stack
//     starts register or update the same connectors instead of racing against
//     free-form configuration;
//   - a bounded Kafka Connect REST client that reads connector/task state and
//     asks Connect to restart one failed task;
//   - a supervisor that detects tasks left in FAILED state (Kafka Connect never
//     restarts them on its own) and restarts only those tasks with exponential
//     backoff and a finite attempt budget.
//
// The package is transport-only: it never publishes task notifications and never
// writes application state.
package connect

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Environment variables understood by this package. They are part of the
// deployment contract (see docs/DEPLOYMENT.md and .env.example).
const (
	// URLEnvVar lists the Kafka Connect REST endpoints (comma separated). Any
	// worker answers cluster-wide connector state, so a deployment lists every
	// worker for failover: one unreachable worker must not blind the monitor.
	URLEnvVar = "RAG_CONNECT_URLS"
	// DocumentConnectorEnvVar / CacheConnectorEnvVar are the fixed connector
	// names. Registration, monitoring and the restart supervisor all address
	// the connectors by these names.
	DocumentConnectorEnvVar = "RAG_CONNECT_DOCUMENT_CONNECTOR"
	CacheConnectorEnvVar    = "RAG_CONNECT_CACHE_CONNECTOR"
	// TimeoutEnvVar bounds one Kafka Connect REST round trip.
	TimeoutEnvVar = "RAG_CONNECT_TIMEOUT_SECONDS"
	// SupervisorEnabledEnvVar toggles the failed-task restart loop. Unset means
	// "enabled when RAG_CONNECT_URLS is configured".
	SupervisorEnabledEnvVar = "RAG_CONNECT_SUPERVISOR_ENABLED"
	// SupervisorIntervalEnvVar is the polling period of the restart loop.
	SupervisorIntervalEnvVar = "RAG_CONNECT_SUPERVISOR_INTERVAL_SECONDS"
	// RestartMaxAttemptsEnvVar limits restart attempts per failure episode.
	RestartMaxAttemptsEnvVar = "RAG_CONNECT_RESTART_MAX_ATTEMPTS"
	// RestartBaseBackoffEnvVar is the first backoff delay; it doubles per attempt.
	RestartBaseBackoffEnvVar = "RAG_CONNECT_RESTART_BASE_BACKOFF_SECONDS"
	// RestartMaxBackoffEnvVar caps the exponential backoff.
	RestartMaxBackoffEnvVar = "RAG_CONNECT_RESTART_MAX_BACKOFF_SECONDS"
	// ExpectedVersionEnvVar is the Debezium MySQL connector version every
	// worker must report. Registration fails when a worker advertises another
	// version, so a mixed cluster cannot silently split capture semantics.
	ExpectedVersionEnvVar = "DEBEZIUM_CONNECT_VERSION"
)

// Defaults are intentionally conservative: a monitor request must never hang on
// a wedged Connect worker, and a restart loop must not hammer a broken task.
const (
	DefaultDocumentConnector = "praxis-document-cdc"
	DefaultCacheConnector    = "praxis-cache-cdc"
	DefaultTimeout           = 3 * time.Second
	DefaultSupervisorPeriod  = 15 * time.Second
	DefaultRestartAttempts   = 5
	DefaultRestartBaseDelay  = 2 * time.Second
	DefaultRestartMaxDelay   = 5 * time.Minute
	// DefaultExpectedVersion tracks the pinned Debezium image in compose.
	DefaultExpectedVersion = "3.7.1.Final"
	// MaxTimeout bounds a configured REST timeout so one probe cannot stall the
	// Console snapshot for minutes.
	MaxTimeout = 30 * time.Second
)

// Settings is the resolved Kafka Connect integration configuration.
type Settings struct {
	// URLs are the REST endpoints of every Connect worker (at least one).
	URLs []string
	// DocumentConnector / CacheConnector are the fixed connector names.
	DocumentConnector string
	CacheConnector    string
	Timeout           time.Duration

	SupervisorEnabled  bool
	SupervisorInterval time.Duration
	RestartMaxAttempts int
	RestartBaseBackoff time.Duration
	RestartMaxBackoff  time.Duration

	ExpectedVersion string
}

// Configured reports whether at least one Connect REST endpoint is set. When it
// is false the service runs without capture observability (the workers still
// consume whatever Kafka contains, and the monitoring rows stay 未知).
func (s Settings) Configured() bool { return len(s.URLs) > 0 }

// SettingsFromEnv resolves the integration settings from the environment.
func SettingsFromEnv() (Settings, error) {
	return settingsFrom(os.Getenv)
}

// settingsFrom is the pure parser behind SettingsFromEnv; tests inject a map.
func settingsFrom(getenv func(string) string) (Settings, error) {
	urls, err := ParseURLs(getenv(URLEnvVar))
	if err != nil {
		return Settings{}, err
	}
	settings := Settings{
		URLs:               urls,
		DocumentConnector:  strings.TrimSpace(getenv(DocumentConnectorEnvVar)),
		CacheConnector:     strings.TrimSpace(getenv(CacheConnectorEnvVar)),
		SupervisorEnabled:  len(urls) > 0,
		SupervisorInterval: DefaultSupervisorPeriod,
		RestartMaxAttempts: DefaultRestartAttempts,
		RestartBaseBackoff: DefaultRestartBaseDelay,
		RestartMaxBackoff:  DefaultRestartMaxDelay,
		ExpectedVersion:    strings.TrimSpace(getenv(ExpectedVersionEnvVar)),
	}
	if settings.DocumentConnector == "" {
		settings.DocumentConnector = DefaultDocumentConnector
	}
	if settings.CacheConnector == "" {
		settings.CacheConnector = DefaultCacheConnector
	}
	if settings.ExpectedVersion == "" {
		settings.ExpectedVersion = DefaultExpectedVersion
	}
	if settings.DocumentConnector == settings.CacheConnector {
		return Settings{}, fmt.Errorf("%s and %s must name two different connectors",
			DocumentConnectorEnvVar, CacheConnectorEnvVar)
	}
	settings.Timeout, err = durationSetting(TimeoutEnvVar, getenv(TimeoutEnvVar), DefaultTimeout)
	if err != nil {
		return Settings{}, err
	}
	if settings.Timeout > MaxTimeout {
		return Settings{}, fmt.Errorf("%s must not exceed %s", TimeoutEnvVar, MaxTimeout)
	}
	if raw := strings.TrimSpace(getenv(SupervisorEnabledEnvVar)); raw != "" {
		enabled, parseErr := strconv.ParseBool(raw)
		if parseErr != nil {
			return Settings{}, fmt.Errorf("%s must be true or false", SupervisorEnabledEnvVar)
		}
		settings.SupervisorEnabled = enabled && len(urls) > 0
	}
	settings.SupervisorInterval, err = durationSetting(SupervisorIntervalEnvVar,
		getenv(SupervisorIntervalEnvVar), DefaultSupervisorPeriod)
	if err != nil {
		return Settings{}, err
	}
	settings.RestartBaseBackoff, err = durationSetting(RestartBaseBackoffEnvVar,
		getenv(RestartBaseBackoffEnvVar), DefaultRestartBaseDelay)
	if err != nil {
		return Settings{}, err
	}
	settings.RestartMaxBackoff, err = durationSetting(RestartMaxBackoffEnvVar,
		getenv(RestartMaxBackoffEnvVar), DefaultRestartMaxDelay)
	if err != nil {
		return Settings{}, err
	}
	if settings.RestartMaxBackoff < settings.RestartBaseBackoff {
		return Settings{}, fmt.Errorf("%s must not be smaller than %s",
			RestartMaxBackoffEnvVar, RestartBaseBackoffEnvVar)
	}
	if raw := strings.TrimSpace(getenv(RestartMaxAttemptsEnvVar)); raw != "" {
		attempts, parseErr := strconv.Atoi(raw)
		if parseErr != nil || attempts < 1 || attempts > 100 {
			return Settings{}, fmt.Errorf("%s must be an integer between 1 and 100", RestartMaxAttemptsEnvVar)
		}
		settings.RestartMaxAttempts = attempts
	}
	return settings, nil
}

// ParseURLs validates the comma-separated REST endpoint list. Duplicates are
// dropped while preserving order. The CLI uses it for a --urls override without
// re-reading the environment.
func ParseURLs(raw string) ([]string, error) {
	seen := make(map[string]struct{})
	urls := make([]string, 0, 2)
	for _, item := range strings.Split(raw, ",") {
		trimmed := strings.TrimSpace(item)
		if trimmed == "" {
			continue
		}
		parsed, err := url.Parse(trimmed)
		if err != nil || parsed.Host == "" {
			return nil, fmt.Errorf("%s contains an invalid URL", URLEnvVar)
		}
		switch parsed.Scheme {
		case "http", "https":
		default:
			return nil, fmt.Errorf("%s must use http or https", URLEnvVar)
		}
		normalized := strings.TrimRight(parsed.String(), "/")
		if _, ok := seen[normalized]; ok {
			continue
		}
		seen[normalized] = struct{}{}
		urls = append(urls, normalized)
	}
	return urls, nil
}

// durationSetting parses a positive number of seconds.
func durationSetting(name, raw string, fallback time.Duration) (time.Duration, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return fallback, nil
	}
	seconds, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || seconds <= 0 {
		return 0, fmt.Errorf("%s must be a positive number of seconds", name)
	}
	return time.Duration(seconds * float64(time.Second)), nil
}
