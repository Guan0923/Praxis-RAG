package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsDefaultsToSQLiteUnderDataRoot(t *testing.T) {
	isolateStateEnv(t)
	root := filepath.Join(t.TempDir(), "state")
	settings, err := SettingsFromEnv(root)
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.IsMySQL {
		t.Fatal("expected SQLite settings")
	}
	expected := filepath.Join(root, DefaultSQLiteName)
	if settings.SQLitePath != expected {
		t.Fatalf("SQLitePath = %q, want %q", settings.SQLitePath, expected)
	}
	if !strings.HasPrefix(settings.URL, "sqlite+pysqlite:///") {
		t.Fatalf("URL = %q", settings.URL)
	}
	if settings.PoolSize != 10 || settings.MaxOverflow != 20 || settings.PoolRecycle != 1800 || settings.PoolTimeout != 30 {
		t.Fatalf("unexpected pool defaults: %+v", settings)
	}
	if settings.Echo || !settings.AutoSchema || settings.EncryptionKey != "" {
		t.Fatalf("unexpected defaults: %+v", settings)
	}
}

func TestSettingsMySQLPartsAndDescribe(t *testing.T) {
	isolateStateEnv(t)
	t.Setenv("RAG_MYSQL_HOST", "db.local")
	t.Setenv("RAG_MYSQL_PORT", "3307")
	t.Setenv("RAG_MYSQL_USER", "rag")
	t.Setenv("RAG_MYSQL_PASSWORD", "p@ss word")
	t.Setenv("RAG_MYSQL_DATABASE", "praxis")
	t.Setenv("RAG_STATE_ENCRYPTION_KEY", "  "+testEncryptionKey+"  ")
	settings, err := SettingsFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if !settings.IsMySQL || settings.Host != "db.local" || settings.Port != 3307 || settings.Database != "praxis" {
		t.Fatalf("unexpected MySQL settings: %+v", settings)
	}
	if settings.EncryptionKey != testEncryptionKey {
		t.Fatalf("EncryptionKey = %q, want trimmed key", settings.EncryptionKey)
	}
	if settings.URL != "mysql+pymysql://rag:p%40ss+word@db.local:3307/praxis?charset=utf8mb4" {
		t.Fatalf("URL = %q", settings.URL)
	}
	description := Describe(settings)
	if description["backend"] != "mysql" || description["database"] != "praxis" || description["host"] != "db.local" {
		t.Fatalf("unexpected describe payload: %+v", description)
	}
	if description["encrypted"] != true {
		t.Fatalf("encrypted = %v", description["encrypted"])
	}
}

func TestSettingsExplicitURLWinsOverMySQLParts(t *testing.T) {
	isolateStateEnv(t)
	root := filepath.Join(t.TempDir(), "state")
	t.Setenv("RAG_MYSQL_HOST", "ignored.local")
	t.Setenv("RAG_STATE_URL", "sqlite+pysqlite:///"+filepath.ToSlash(filepath.Join(root, "custom.sqlite3")))
	settings, err := SettingsFromEnv(root)
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.IsMySQL {
		t.Fatal("explicit SQLite URL did not win over RAG_MYSQL_HOST")
	}
	if settings.SQLitePath != filepath.ToSlash(filepath.Join(root, "custom.sqlite3")) {
		t.Fatalf("SQLitePath = %q", settings.SQLitePath)
	}
}

func TestSettingsClampsAndBooleanSpellings(t *testing.T) {
	isolateStateEnv(t)
	t.Setenv("RAG_STATE_POOL_SIZE", "0")
	t.Setenv("RAG_STATE_MAX_OVERFLOW", "-5")
	t.Setenv("RAG_STATE_POOL_RECYCLE", "10")
	t.Setenv("RAG_STATE_POOL_TIMEOUT", "0")
	t.Setenv("RAG_STATE_ECHO", "YES")
	t.Setenv("RAG_STATE_AUTO_SCHEMA", "off")
	settings, err := SettingsFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.PoolSize != 1 || settings.MaxOverflow != 0 || settings.PoolRecycle != 60 || settings.PoolTimeout != 1 {
		t.Fatalf("unexpected clamped settings: %+v", settings)
	}
	if !settings.Echo || settings.AutoSchema {
		t.Fatalf("unexpected booleans: echo=%v auto_schema=%v", settings.Echo, settings.AutoSchema)
	}
}

func TestSettingsInvalidIntegers(t *testing.T) {
	isolateStateEnv(t)
	for _, name := range []string{
		"RAG_STATE_POOL_SIZE",
		"RAG_STATE_MAX_OVERFLOW",
		"RAG_STATE_POOL_RECYCLE",
		"RAG_STATE_POOL_TIMEOUT",
	} {
		isolateStateEnv(t)
		t.Setenv(name, "")
		_, err := SettingsFromEnv(t.TempDir())
		if err == nil || err.Error() != name+" must be an integer" {
			t.Fatalf("%s: error = %v, want %q", name, err, name+" must be an integer")
		}
	}
	isolateStateEnv(t)
	t.Setenv("RAG_STATE_POOL_SIZE", "ten")
	if _, err := SettingsFromEnv(t.TempDir()); err == nil || err.Error() != "RAG_STATE_POOL_SIZE must be an integer" {
		t.Fatalf("non-numeric pool size error = %v", err)
	}
}

func TestSettingsInvalidMySQLPort(t *testing.T) {
	isolateStateEnv(t)
	t.Setenv("RAG_MYSQL_HOST", "db.local")
	t.Setenv("RAG_MYSQL_PORT", "not-a-number")
	_, err := SettingsFromEnv(t.TempDir())
	if err == nil || err.Error() != "RAG_MYSQL_PORT must be an integer" {
		t.Fatalf("error = %v", err)
	}
	// The port is only parsed when the MySQL parts actually select the backend.
	isolateStateEnv(t)
	t.Setenv("RAG_MYSQL_PORT", "not-a-number")
	t.Setenv("RAG_STATE_URL", "sqlite+pysqlite:///"+filepath.ToSlash(filepath.Join(t.TempDir(), "state.sqlite3")))
	if _, err := SettingsFromEnv(t.TempDir()); err != nil {
		t.Fatalf("explicit URL should not read RAG_MYSQL_PORT: %v", err)
	}
}

func TestDescribeParsesURLsLikeSQLAlchemy(t *testing.T) {
	isolateStateEnv(t)
	cases := []struct {
		url      string
		backend  any
		database any
		host     any
	}{
		{"sqlite+pysqlite:///C:/data/state.sqlite3", "sqlite", "C:/data/state.sqlite3", nil},
		{"sqlite+pysqlite:////var/lib/praxis/state.sqlite3", "sqlite", "/var/lib/praxis/state.sqlite3", nil},
		{"sqlite+pysqlite:///:memory:", "sqlite", ":memory:", nil},
		{"mysql+pymysql://rag:pw@db.local:3306/praxis_rag?charset=utf8mb4", "mysql", "praxis_rag", "db.local"},
	}
	for _, testCase := range cases {
		description := Describe(StateSettings{URL: testCase.url})
		if description["backend"] != testCase.backend || description["database"] != testCase.database || description["host"] != testCase.host {
			t.Fatalf("%s: describe = %+v", testCase.url, description)
		}
	}
}

func TestDescribeSQLiteUsesPosixPath(t *testing.T) {
	isolateStateEnv(t)
	useTestEncryptionKey(t)
	root := filepath.Join(t.TempDir(), "data")
	settings, err := SettingsFromEnv(root)
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	description := Describe(settings)
	expected := filepath.ToSlash(filepath.Join(root, DefaultSQLiteName))
	if description["backend"] != "sqlite" || description["database"] != expected || description["host"] != nil {
		t.Fatalf("describe = %+v, want database %q", description, expected)
	}
	if description["encrypted"] != true {
		t.Fatalf("encrypted = %v", description["encrypted"])
	}
}
