package connect

import (
	"strings"
	"testing"
	"time"
)

// envMap turns a map into the getenv seam.
func envMap(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestSettingsDefaultsWithoutConnectURLs(t *testing.T) {
	settings, err := settingsFrom(envMap(nil))
	if err != nil {
		t.Fatalf("settingsFrom: %v", err)
	}
	if settings.Configured() || settings.SupervisorEnabled {
		t.Fatalf("unconfigured settings = %+v", settings)
	}
	if settings.DocumentConnector != DefaultDocumentConnector ||
		settings.CacheConnector != DefaultCacheConnector ||
		settings.ExpectedVersion != DefaultExpectedVersion ||
		settings.Timeout != DefaultTimeout ||
		settings.RestartMaxAttempts != DefaultRestartAttempts {
		t.Fatalf("defaults = %+v", settings)
	}
}

func TestSettingsFromEnvParsesConnectAndSupervisorValues(t *testing.T) {
	settings, err := settingsFrom(envMap(map[string]string{
		URLEnvVar:                "http://connect-1:8083, http://connect-2:8083",
		DocumentConnectorEnvVar:  "doc-cdc",
		CacheConnectorEnvVar:     "cache-cdc",
		TimeoutEnvVar:            "5",
		SupervisorEnabledEnvVar:  "false",
		SupervisorIntervalEnvVar: "30",
		RestartMaxAttemptsEnvVar: "7",
		RestartBaseBackoffEnvVar: "3",
		RestartMaxBackoffEnvVar:  "60",
		ExpectedVersionEnvVar:    "3.7.1.Final",
	}))
	if err != nil {
		t.Fatalf("settingsFrom: %v", err)
	}
	if len(settings.URLs) != 2 || settings.DocumentConnector != "doc-cdc" ||
		settings.CacheConnector != "cache-cdc" || settings.Timeout != 5*time.Second ||
		settings.SupervisorEnabled || settings.SupervisorInterval != 30*time.Second ||
		settings.RestartMaxAttempts != 7 || settings.RestartBaseBackoff != 3*time.Second ||
		settings.RestartMaxBackoff != 60*time.Second {
		t.Fatalf("settings = %+v", settings)
	}
}

func TestSettingsValidatesRestartBudgetAndConnectorNames(t *testing.T) {
	cases := []map[string]string{
		{URLEnvVar: "http://connect:8083", DocumentConnectorEnvVar: "same", CacheConnectorEnvVar: "same"},
		{URLEnvVar: "http://connect:8083", RestartMaxAttemptsEnvVar: "0"},
		{URLEnvVar: "http://connect:8083", RestartMaxAttemptsEnvVar: "abc"},
		{URLEnvVar: "http://connect:8083", RestartBaseBackoffEnvVar: "10", RestartMaxBackoffEnvVar: "5"},
		{URLEnvVar: "ftp://connect:8083"},
		{URLEnvVar: "http://connect:8083", SupervisorEnabledEnvVar: "maybe"},
		{URLEnvVar: "http://connect:8083", TimeoutEnvVar: "0"},
		{URLEnvVar: "http://connect:8083", TimeoutEnvVar: "31"},
	}
	for index, values := range cases {
		if _, err := settingsFrom(envMap(values)); err == nil {
			t.Fatalf("case %d: expected a settings error for %v", index, values)
		}
	}
}

func TestConnectorTemplateExpansion(t *testing.T) {
	expanded, err := expandString(
		`${RAG_MYSQL_HOST:-mysql}:${RAG_MYSQL_PORT:-3306}/${RAG_MYSQL_DATABASE}`,
		envMap(map[string]string{"RAG_MYSQL_DATABASE": "praxis_rag"}),
	)
	if err != nil {
		t.Fatalf("expandString: %v", err)
	}
	if expanded != "mysql:3306/praxis_rag" {
		t.Fatalf("expanded = %q", expanded)
	}
	// Password values may contain quotes/backslashes; expansion happens on the
	// decoded string, so the value must survive unchanged and be JSON-encoded
	// later by the REST client.
	value := `pa"ss\word`
	expanded, err = expandString("${PASS}", envMap(map[string]string{"PASS": value}))
	if err != nil || expanded != value {
		t.Fatalf("expanded = %q err = %v", expanded, err)
	}
	if _, err := expandString("${MISSING}", envMap(nil)); err == nil ||
		!strings.Contains(err.Error(), "MISSING") {
		t.Fatalf("missing variable error = %v", err)
	}
}
