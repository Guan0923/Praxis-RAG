package config

import (
	"fmt"
	"net/url"
	"os"
	"strings"
)

// StateSettings configures the authoritative state database. MySQL is the only
// supported backend: the service never falls back to a local file database.
type StateSettings struct {
	URL           string
	EncryptionKey string
	PoolSize      int
	MaxOverflow   int
	PoolRecycle   int
	PoolTimeout   int
	Echo          bool
	AutoSchema    bool

	Host     string
	Port     int
	User     string
	Password string
	Database string
	SSLCA    string
}

// StateSettingsFromEnv resolves the MySQL state database connection. The
// precedence is an explicit RAG_STATE_URL, then the RAG_MYSQL_* parts; when
// neither is configured it fails instead of opening a local database.
//
// dataRoot is still accepted because callers pass their service data
// directory, but it never selects the database.
func StateSettingsFromEnv(dataRoot string) (StateSettings, error) {
	_ = dataRoot
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
		settings.Host, settings.Port = host, port
		settings.User, settings.Password, settings.Database, settings.SSLCA = user, password, database, sslCA
		settings.URL = buildMySQLURL(host, port, user, password, database, sslCA)
	default:
		return StateSettings{}, fmt.Errorf(
			"the state database is not configured: set RAG_STATE_URL, or RAG_MYSQL_HOST/RAG_MYSQL_USER/RAG_MYSQL_DATABASE/RAG_MYSQL_PASSWORD")
	}
	return settings, nil
}

// Describe returns the secret-free state summary reported by /health.
func (s StateSettings) Describe() map[string]any {
	return map[string]any{
		"backend":   "mysql",
		"database":  s.Database,
		"host":      s.Host,
		"encrypted": s.EncryptionKey != "",
	}
}

// ParseStateURL parses a MySQL URL into connection settings, so callers that
// hold only the URL (tests, tooling) get the same host/port/user/database the
// environment resolution produces.
func ParseStateURL(raw string) (StateSettings, error) {
	settings := StateSettings{URL: raw}
	if err := settings.parseURL(raw); err != nil {
		return StateSettings{}, err
	}
	return settings, nil
}

func (s *StateSettings) parseURL(raw string) error {
	if !strings.HasPrefix(strings.ToLower(raw), "mysql") {
		return fmt.Errorf("RAG_STATE_URL must be a MySQL URL")
	}
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
