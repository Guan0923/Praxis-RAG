package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

// DefaultSQLiteName is the SQLite file created under the data root.
const DefaultSQLiteName = "state.sqlite3"

// StateSettings configures the authoritative state database. It mirrors
// rag_mcp.state.settings.StateSettings but keeps both the raw URL and the
// parsed parts needed to build Go database/sql DSNs.
type StateSettings struct {
	URL           string
	EncryptionKey string
	PoolSize      int
	MaxOverflow   int
	PoolRecycle   int
	PoolTimeout   int
	Echo          bool
	AutoSchema    bool

	IsMySQL    bool
	SQLitePath string
	Host       string
	Port       int
	User       string
	Password   string
	Database   string
	SSLCA      string
}

// StateSettingsFromEnv resolves the state database selector. The precedence is
// RAG_STATE_URL, then RAG_MYSQL_HOST parts, then SQLite under the data root.
func StateSettingsFromEnv(dataRoot string) (StateSettings, error) {
	explicit := strings.TrimSpace(os.Getenv("RAG_STATE_URL"))
	settings := StateSettings{
		EncryptionKey: strings.TrimSpace(os.Getenv("RAG_STATE_ENCRYPTION_KEY")),
		Echo:          isTruthy(os.Getenv("RAG_STATE_ECHO")),
		AutoSchema:    envBoolDefault("RAG_STATE_AUTO_SCHEMA", true),
	}
	settings.PoolSize = maxInt(1, parseEnvInt("RAG_STATE_POOL_SIZE", 10))
	settings.MaxOverflow = maxInt(0, parseEnvInt("RAG_STATE_MAX_OVERFLOW", 20))
	settings.PoolRecycle = maxInt(60, parseEnvInt("RAG_STATE_POOL_RECYCLE", 1800))
	settings.PoolTimeout = maxInt(1, parseEnvInt("RAG_STATE_POOL_TIMEOUT", 30))

	switch {
	case explicit != "":
		settings.URL = explicit
		if err := settings.parseURL(explicit); err != nil {
			return StateSettings{}, err
		}
	case strings.TrimSpace(os.Getenv("RAG_MYSQL_HOST")) != "":
		host := strings.TrimSpace(os.Getenv("RAG_MYSQL_HOST"))
		user := defaultString(os.Getenv("RAG_MYSQL_USER"), "rag")
		password := os.Getenv("RAG_MYSQL_PASSWORD")
		database := defaultString(strings.TrimSpace(os.Getenv("RAG_MYSQL_DATABASE")), "praxis_rag")
		port := parseEnvInt("RAG_MYSQL_PORT", 3306)
		sslCA := strings.TrimSpace(os.Getenv("RAG_MYSQL_SSL_CA"))
		settings.IsMySQL = true
		settings.Host, settings.Port = host, port
		settings.User, settings.Password, settings.Database, settings.SSLCA = user, password, database, sslCA
		settings.URL = buildMySQLURL(host, port, user, password, database, sslCA)
	default:
		root := ExpandPath(dataRoot)
		if root == "" {
			home, err := os.UserHomeDir()
			if err != nil {
				return StateSettings{}, fmt.Errorf("Cannot resolve the home directory for the state database")
			}
			root = filepath.Join(home, ".praxis-rag", "data")
		}
		if err := os.MkdirAll(root, 0o755); err != nil {
			return StateSettings{}, fmt.Errorf("Cannot create the state database directory: %s", root)
		}
		path := filepath.Join(root, DefaultSQLiteName)
		settings.SQLitePath = path
		settings.URL = "sqlite+pysqlite:///" + filepath.ToSlash(path)
	}
	return settings, nil
}

// Describe returns the secret-free state summary reported by /health.
func (s StateSettings) Describe() map[string]any {
	backend := "sqlite"
	database := any(filepath.ToSlash(s.SQLitePath))
	host := any(nil)
	if s.IsMySQL {
		backend = "mysql"
		database = s.Database
		host = s.Host
	}
	return map[string]any{
		"backend":   backend,
		"database":  database,
		"host":      host,
		"encrypted": s.EncryptionKey != "",
	}
}

func (s *StateSettings) parseURL(raw string) error {
	lowered := strings.ToLower(raw)
	switch {
	case strings.HasPrefix(lowered, "mysql"):
		s.IsMySQL = true
		parsed, err := url.Parse(raw)
		if err != nil {
			return fmt.Errorf("RAG_STATE_URL is not a valid database URL")
		}
		s.Host = parsed.Hostname()
		if parsed.Port() != "" {
			var port int
			if _, scanErr := fmt.Sscanf(parsed.Port(), "%d", &port); scanErr == nil {
				s.Port = port
			}
		}
		if s.Port == 0 {
			s.Port = 3306
		}
		if parsed.User != nil {
			s.User = parsed.User.Username()
			if password, ok := parsed.User.Password(); ok {
				s.Password = password
			}
		}
		s.Database = strings.TrimPrefix(parsed.Path, "/")
		s.SSLCA = parsed.Query().Get("ssl_ca")
	case strings.HasPrefix(lowered, "sqlite"):
		index := strings.Index(raw, ":///")
		if index < 0 {
			index = strings.Index(raw, "://")
			if index < 0 {
				return fmt.Errorf("RAG_STATE_URL is not a valid SQLite URL")
			}
			s.SQLitePath = raw[index+3:]
		} else {
			s.SQLitePath = raw[index+4:]
		}
		if s.SQLitePath != "" && s.SQLitePath != ":memory:" && !strings.HasPrefix(s.SQLitePath, "file:") {
			parent := filepath.Dir(ExpandPath(s.SQLitePath))
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return fmt.Errorf("Cannot create the state database directory: %s", parent)
			}
		}
	default:
		return fmt.Errorf("RAG_STATE_URL must be a MySQL or SQLite URL")
	}
	return nil
}

func buildMySQLURL(host string, port int, user, password, database, sslCA string) string {
	escapedUser := url.QueryEscape(user)
	escapedPassword := url.QueryEscape(password)
	escapedDatabase := url.QueryEscape(database)
	result := fmt.Sprintf("mysql+pymysql://%s:%s@%s:%d/%s?charset=utf8mb4",
		escapedUser, escapedPassword, host, port, escapedDatabase)
	if sslCA != "" {
		result += "&ssl_ca=" + url.QueryEscape(sslCA)
	}
	return result
}

func isTruthy(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func envBoolDefault(name string, fallback bool) bool {
	raw, ok := os.LookupEnv(name)
	if !ok {
		return fallback
	}
	return isTruthy(raw)
}

func parseEnvInt(name string, fallback int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	var value int
	if _, err := fmt.Sscanf(raw, "%d", &value); err != nil {
		return fallback
	}
	return value
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
