package state

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SnapshotSettings bounds the expiring artifact snapshots that a search response
// may reference. Search results themselves are returned inline and are no longer
// stored, so only the artifact limits and the artifact retention window remain.
//
// The type name and the environment variables are kept for contract stability;
// RAG_RESULT_TTL_SECONDS is now the artifact retention window (the historical
// name is retained so existing deployments do not need to rename it).
type SnapshotSettings struct {
	TTLSeconds            int
	MaxArtifactBytes      int
	MaxArtifactsPerSearch int
}

// DefaultSnapshotSettings returns the historical dataclass defaults.
func DefaultSnapshotSettings() SnapshotSettings {
	return SnapshotSettings{
		TTLSeconds:            3600,
		MaxArtifactBytes:      64 * 1024 * 1024,
		MaxArtifactsPerSearch: 100,
	}
}

// SnapshotSettingsFromEnv reads the artifact retention settings.
//
// RAG_RESULT_MAX_BYTES is intentionally no longer read: it bounded the removed
// result-snapshot payload. An old deployment that still exports it starts
// normally because the value is simply ignored.
func SnapshotSettingsFromEnv() (SnapshotSettings, error) {
	settings := DefaultSnapshotSettings()
	values := []struct {
		name     string
		fallback int
		target   *int
	}{
		{"RAG_RESULT_TTL_SECONDS", 3600, &settings.TTLSeconds},
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
