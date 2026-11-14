package state

import (
	"math"
	"testing"
)

func TestCacheSettingsDefaultsAndEnvironment(t *testing.T) {
	isolateStateEnv(t)
	settings, err := CacheSettingsFromEnv()
	if err != nil {
		t.Fatalf("CacheSettingsFromEnv() error = %v", err)
	}
	if settings.URL != "" || settings.Prefix != "rag:state:v1" || settings.TTLSeconds != 60 {
		t.Fatalf("unexpected cache defaults: %+v", settings)
	}
	if settings.TimeoutSeconds != 0.3 || settings.Enabled != nil || settings.backendEnabled() {
		t.Fatalf("unexpected cache defaults: %+v", settings)
	}

	t.Setenv("RAG_REDIS_URL", "redis://private-user:private-password@localhost/0")
	t.Setenv("RAG_REDIS_PREFIX", "my-state")
	t.Setenv("RAG_REDIS_TTL_SECONDS", "90")
	t.Setenv("RAG_REDIS_TIMEOUT_SECONDS", "0.7")
	settings, err = CacheSettingsFromEnv()
	if err != nil {
		t.Fatalf("CacheSettingsFromEnv() error = %v", err)
	}
	if settings.TTLSeconds != 90 || settings.TimeoutSeconds != 0.7 || settings.Prefix != "my-state" {
		t.Fatalf("unexpected cache settings: %+v", settings)
	}
	if !settings.backendEnabled() {
		t.Fatal("configured Redis URL did not enable the backend")
	}
	enabled := false
	t.Setenv("RAG_REDIS_ENABLED", "off")
	settings, err = CacheSettingsFromEnv()
	if err != nil {
		t.Fatalf("CacheSettingsFromEnv() error = %v", err)
	}
	settings.Enabled = &enabled
	if settings.backendEnabled() {
		t.Fatal("RAG_REDIS_ENABLED=off did not disable the backend")
	}
}

func TestCacheSettingsInvalidEnvironment(t *testing.T) {
	for name, testCase := range map[string]struct {
		variable string
		value    string
		message  string
	}{
		"ttl zero":     {"RAG_REDIS_TTL_SECONDS", "0", "RAG_REDIS_TTL_SECONDS must be a positive integer"},
		"ttl negative": {"RAG_REDIS_TTL_SECONDS", "-1", "RAG_REDIS_TTL_SECONDS must be a positive integer"},
		"ttl junk":     {"RAG_REDIS_TTL_SECONDS", "bad-secret-value", "RAG_REDIS_TTL_SECONDS must be a positive integer"},
		"timeout zero": {"RAG_REDIS_TIMEOUT_SECONDS", "0", "RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number"},
		"timeout nan":  {"RAG_REDIS_TIMEOUT_SECONDS", "nan", "RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number"},
		"timeout inf":  {"RAG_REDIS_TIMEOUT_SECONDS", "inf", "RAG_REDIS_TIMEOUT_SECONDS must be a positive finite number"},
		"enabled bad":  {"RAG_REDIS_ENABLED", "bad-secret-value", "RAG_REDIS_ENABLED must be a boolean"},
		"prefix empty": {"RAG_REDIS_PREFIX", "", "RAG_REDIS_PREFIX must not be empty"},
	} {
		isolateStateEnv(t)
		t.Setenv(testCase.variable, testCase.value)
		_, err := CacheSettingsFromEnv()
		if err == nil || err.Error() != testCase.message {
			t.Fatalf("%s: error = %v, want %q", name, err, testCase.message)
		}
	}
	// Errors never echo the configured secret value.
	isolateStateEnv(t)
	t.Setenv("RAG_REDIS_TTL_SECONDS", "bad-secret-value")
	_, err := CacheSettingsFromEnv()
	if err == nil || containsSubstring(err.Error(), "bad-secret-value") {
		t.Fatalf("error leaked the secret value: %v", err)
	}
}

func TestCacheSettingsValidation(t *testing.T) {
	valid := DefaultCacheSettings()
	if err := valid.Validate(); err != nil {
		t.Fatalf("default settings invalid: %v", err)
	}
	for name, mutate := range map[string]func(*CacheSettings){
		"ttl":     func(settings *CacheSettings) { settings.TTLSeconds = 0 },
		"timeout": func(settings *CacheSettings) { settings.TimeoutSeconds = 0 },
		"nan":     func(settings *CacheSettings) { settings.TimeoutSeconds = math.NaN() },
		"prefix":  func(settings *CacheSettings) { settings.Prefix = "" },
	} {
		broken := valid
		mutate(&broken)
		if err := broken.Validate(); err == nil {
			t.Fatalf("%s: Validate() succeeded", name)
		}
	}
}

func TestBrokerSettingsAndEventID(t *testing.T) {
	isolateStateEnv(t)
	settings := BrokerSettingsFromEnv()
	if settings.BootstrapServers != "" || settings.CacheTopic != "praxis_state_cache_refresh" ||
		settings.CacheGroup != "praxis_state_cache_refresh_workers" ||
		settings.ClientID != "praxis-rag" || settings.SecurityProtocol != "PLAINTEXT" ||
		settings.SASLMechanism != "" || settings.TLSCA != "" || settings.TLSInsecure {
		t.Fatalf("unexpected broker defaults: %+v", settings)
	}
	t.Setenv("RAG_KAFKA_BOOTSTRAP_SERVERS", "localhost:9092,localhost:9093")
	t.Setenv("RAG_CACHE_TOPIC", "topic")
	t.Setenv("RAG_CACHE_GROUP", "group")
	t.Setenv("RAG_KAFKA_CLIENT_ID", "client")
	t.Setenv("RAG_KAFKA_SECURITY_PROTOCOL", "SASL_SSL")
	t.Setenv("RAG_KAFKA_SASL_MECHANISM", "SCRAM-SHA-512")
	t.Setenv("RAG_KAFKA_SASL_USERNAME", "user")
	t.Setenv("RAG_KAFKA_SASL_PASSWORD", "secret")
	t.Setenv("RAG_KAFKA_TLS_CA", "C:/certs/ca.pem")
	t.Setenv("RAG_KAFKA_TLS_INSECURE", "YES")
	settings = BrokerSettingsFromEnv()
	if settings.BootstrapServers != "localhost:9092,localhost:9093" || settings.CacheTopic != "topic" ||
		settings.CacheGroup != "group" || settings.ClientID != "client" ||
		settings.SecurityProtocol != "SASL_SSL" || settings.SASLMechanism != "SCRAM-SHA-512" ||
		settings.SASLUsername != "user" || settings.SASLPassword != "secret" ||
		settings.TLSCA != "C:/certs/ca.pem" || !settings.TLSInsecure {
		t.Fatalf("unexpected broker settings: %+v", settings)
	}
	if !ValidateCacheEventID("0123456789abcdef0123456789abcdef") {
		t.Fatal("valid event id rejected")
	}
	for _, invalid := range []string{"not an event", "", "0123456789ABCDEF0123456789abcdef", "0123456789abcdef"} {
		if ValidateCacheEventID(invalid) {
			t.Fatalf("invalid event id %q accepted", invalid)
		}
	}
}

func TestSnapshotSettings(t *testing.T) {
	isolateStateEnv(t)
	settings, err := SnapshotSettingsFromEnv()
	if err != nil {
		t.Fatalf("SnapshotSettingsFromEnv() error = %v", err)
	}
	if settings.TTLSeconds != 3600 || settings.MaxArtifactBytes != 64*1024*1024 ||
		settings.MaxArtifactsPerSearch != 100 {
		t.Fatalf("unexpected artifact defaults: %+v", settings)
	}
	t.Setenv("RAG_RESULT_TTL_SECONDS", "19")
	t.Setenv("RAG_RESULT_MAX_BYTES", "123")
	t.Setenv("RAG_ARTIFACT_MAX_BYTES", "456")
	t.Setenv("RAG_RESULT_MAX_ARTIFACTS", "7")
	settings, err = SnapshotSettingsFromEnv()
	if err != nil {
		t.Fatalf("SnapshotSettingsFromEnv() error = %v", err)
	}
	// RAG_RESULT_MAX_BYTES is a legacy key: it is ignored, not parsed.
	if settings.TTLSeconds != 19 || settings.MaxArtifactBytes != 456 ||
		settings.MaxArtifactsPerSearch != 7 {
		t.Fatalf("unexpected artifact settings: %+v", settings)
	}
	t.Setenv("RAG_RESULT_MAX_BYTES", "0")
	if _, err := SnapshotSettingsFromEnv(); err != nil {
		t.Fatalf("legacy RAG_RESULT_MAX_BYTES must be ignored, error = %v", err)
	}
	for _, name := range []string{
		"RAG_RESULT_TTL_SECONDS", "RAG_ARTIFACT_MAX_BYTES", "RAG_RESULT_MAX_ARTIFACTS",
	} {
		isolateStateEnv(t)
		t.Setenv(name, "0")
		if _, err := SnapshotSettingsFromEnv(); err == nil || err.Error() != name+" must be a positive integer" {
			t.Fatalf("%s: error = %v", name, err)
		}
	}
}

func containsSubstring(value, substring string) bool {
	return len(substring) == 0 || (len(value) >= len(substring) && indexOf(value, substring) >= 0)
}

func indexOf(value, substring string) int {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return index
		}
	}
	return -1
}
