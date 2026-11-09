package state

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestSettingsRequiresMySQLConfiguration(t *testing.T) {
	isolateStateEnv(t)
	root := filepath.Join(t.TempDir(), "state")
	if _, err := SettingsFromEnv(root); err == nil {
		t.Fatal("SettingsFromEnv() error = nil, want a configuration error")
	}
}

func TestSettingsRejectsSQLiteURL(t *testing.T) {
	isolateStateEnv(t)
	t.Setenv("RAG_STATE_URL", "sqlite+pysqlite:///"+filepath.ToSlash(filepath.Join(t.TempDir(), "state.sqlite3")))
	_, err := SettingsFromEnv(t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "MySQL URL") {
		t.Fatalf("error = %v, want a MySQL-only rejection", err)
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
	if settings.Host != "db.local" || settings.Port != 3307 || settings.Database != "praxis" {
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
	t.Setenv("RAG_MYSQL_HOST", "ignored.local")
	t.Setenv("RAG_STATE_URL", "mysql+pymysql://explicit:pw@db.local:3308/chosen?charset=utf8mb4")
	settings, err := SettingsFromEnv(t.TempDir())
	if err != nil {
		t.Fatalf("SettingsFromEnv() error = %v", err)
	}
	if settings.Host != "db.local" || settings.Port != 3308 || settings.Database != "chosen" {
		t.Fatalf("explicit URL did not win: %+v", settings)
	}
}

func TestSettingsClampsAndBooleanSpellings(t *testing.T) {
	isolateStateEnv(t)
	t.Setenv("RAG_MYSQL_HOST", "db.local")
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
	// The port is only parsed when the MySQL parts select the backend.
	isolateStateEnv(t)
	t.Setenv("RAG_MYSQL_PORT", "not-a-number")
	t.Setenv("RAG_STATE_URL", "mysql+pymysql://rag:pw@db.local:3306/praxis_rag?charset=utf8mb4")
	if _, err := SettingsFromEnv(t.TempDir()); err != nil {
		t.Fatalf("explicit URL should not read RAG_MYSQL_PORT: %v", err)
	}
}

func TestDescribeParsesMySQLURLs(t *testing.T) {
	isolateStateEnv(t)
	cases := []struct {
		url      string
		database any
		host     any
	}{
		{"mysql+pymysql://rag:pw@db.local:3306/praxis_rag?charset=utf8mb4", "praxis_rag", "db.local"},
		{"mysql://rag:pw@127.0.0.1/praxis_rag", "praxis_rag", "127.0.0.1"},
	}
	for _, testCase := range cases {
		description := Describe(StateSettings{URL: testCase.url})
		if description["backend"] != "mysql" || description["database"] != testCase.database || description["host"] != testCase.host {
			t.Fatalf("%s: describe = %+v", testCase.url, description)
		}
	}
}

func TestDescribeFallsBackToParsedFields(t *testing.T) {
	isolateStateEnv(t)
	useTestEncryptionKey(t)
	settings := StateSettings{Host: "db.local", Database: "praxis", EncryptionKey: testEncryptionKey}
	description := Describe(settings)
	if description["backend"] != "mysql" || description["database"] != "praxis" || description["host"] != "db.local" {
		t.Fatalf("describe = %+v", description)
	}
	if description["encrypted"] != true {
		t.Fatalf("encrypted = %v", description["encrypted"])
	}
}
