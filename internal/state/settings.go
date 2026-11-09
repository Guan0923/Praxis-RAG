package state

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/config"
)

// DefaultSQLiteName is the SQLite file created under the data root.
const DefaultSQLiteName = config.DefaultSQLiteName

// StateSettings is the configuration of the authoritative state database. It
// aliases config.StateSettings so configuration parsing has one implementation
// (the environment keys, defaults, and clamping already match
// rag_mcp.state.settings.StateSettings).
type StateSettings = config.StateSettings

// SettingsFromEnv resolves the state database selector exactly like
// StateSettings.from_env: RAG_STATE_URL first, then RAG_MYSQL_* parts, then
// SQLite under the data root.
//
// The integer environment variables are validated here because the Python
// helper reads them with int() and raises "<NAME> must be an integer"; the
// configuration package deliberately tolerates malformed integers with a
// fallback, so the state layer checks them before delegating.
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
	// _mysql_url_from_parts only reads the port when the URL is not explicit
	// and RAG_MYSQL_HOST selects the MySQL backend.
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
	backend := "sqlite"
	database := any(nil)
	host := any(nil)
	lowered := strings.ToLower(settings.URL)
	if settings.IsMySQL || strings.HasPrefix(lowered, "mysql") {
		backend = "mysql"
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
	} else {
		database = sqliteDatabaseFromURL(settings.URL)
		if database == nil && settings.SQLitePath != "" {
			database = filepath.ToSlash(settings.SQLitePath)
		}
	}
	return map[string]any{
		"backend":   backend,
		"database":  database,
		"host":      host,
		"encrypted": settings.EncryptionKey != "",
	}
}

// sqliteDatabaseFromURL extracts URL.database the way SQLAlchemy does for
// sqlite URLs: the path with the single leading separator of "///" removed,
// without any query string. An empty database is reported as nil (None).
func sqliteDatabaseFromURL(raw string) any {
	if raw == "" {
		return nil
	}
	indexed := strings.Index(raw, "://")
	if indexed < 0 {
		return nil
	}
	remainder := raw[indexed+3:]
	if cut, _, found := strings.Cut(remainder, "?"); found {
		remainder = cut
	}
	if remainder == "" {
		return nil
	}
	// A single leading slash is part of the "sqlite:///" separator; any
	// additional slashes are part of an absolute filesystem path.
	if strings.HasPrefix(remainder, "/") {
		remainder = remainder[1:]
	}
	if remainder == "" {
		return nil
	}
	return remainder
}
