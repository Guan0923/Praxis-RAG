package state

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

// StateSettings is the configuration of the authoritative state database. It
// aliases config.StateSettings so configuration parsing has one implementation.
type StateSettings = config.StateSettings

// SettingsFromEnv resolves the MySQL state database connection. The integer
// environment variables are validated here because the Python helper reads them
// with int() and raises "<NAME> must be an integer"; the configuration package
// deliberately tolerates malformed integers with a fallback, so the state layer
// checks them before delegating.
func SettingsFromEnv(dataRoot string) (StateSettings, error) {
	if err := validateIntegerSettings(); err != nil {
		return StateSettings{}, err
	}
	return config.StateSettingsFromEnv(dataRoot)
}

// validateIntegerSettings mirrors rag_mcp.state.settings._int_env for every
// integer variable the state settings read.
func validateIntegerSettings() error {
	poolIntegers := []struct {
		name         string
		defaultValue int
	}{
		{"RAG_STATE_POOL_SIZE", 10},
		{"RAG_STATE_MAX_OVERFLOW", 20},
		{"RAG_STATE_POOL_RECYCLE", 1800},
		{"RAG_STATE_POOL_TIMEOUT", 30},
	}
	for _, item := range poolIntegers {
		if _, err := integerEnv(item.name, item.defaultValue); err != nil {
			return err
		}
	}
	// The MySQL port is only read when the URL is not explicit.
	if strings.TrimSpace(os.Getenv("RAG_STATE_URL")) == "" && strings.TrimSpace(os.Getenv("RAG_MYSQL_HOST")) != "" {
		if _, err := integerEnv("RAG_MYSQL_PORT", 3306); err != nil {
			return err
		}
	}
	return nil
}

func integerEnv(name string, defaultValue int) (int, error) {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return defaultValue, nil
	}
	value, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return 0, fmt.Errorf("%s must be an integer", name)
	}
	return value, nil
}

// Describe renders the secret-free state summary returned by
// StateStores.describe: backend, database, host, and whether credentials are
// encrypted. Values come from the URL (like SQLAlchemy make_url) with the
// parsed StateSettings fields as a fallback for manually constructed settings.
func Describe(settings StateSettings) map[string]any {
	database := any(nil)
	host := any(nil)
	if parsed, err := url.Parse(settings.URL); err == nil {
		if value := strings.TrimPrefix(parsed.Path, "/"); value != "" {
			database = value
		} else if settings.Database != "" {
			database = settings.Database
		}
		if value := parsed.Hostname(); value != "" {
			host = value
		} else if settings.Host != "" {
			host = settings.Host
		}
	} else {
		if settings.Database != "" {
			database = settings.Database
		}
		if settings.Host != "" {
			host = settings.Host
		}
	}
	return map[string]any{
		"backend":   "mysql",
		"database":  database,
		"host":      host,
		"encrypted": settings.EncryptionKey != "",
	}
}
