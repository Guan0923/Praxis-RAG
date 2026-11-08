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

func TestRocketMQSettingsAndEventID(t *testing.T) {
	isolateStateEnv(t)
	settings := RocketMQSettingsFromEnv()
	if settings.Endpoint != "" || settings.Topic != "praxis_state_cache_refresh" ||
		settings.Group != "praxis_state_cache_refresh_workers" || settings.TLS {
		t.Fatalf("unexpected queue defaults: %+v", settings)
	}
	t.Setenv("RAG_ROCKETMQ_ENDPOINT", "localhost:8081")
	t.Setenv("RAG_ROCKETMQ_TOPIC", "topic")
	t.Setenv("RAG_ROCKETMQ_GROUP", "group")
	t.Setenv("RAG_ROCKETMQ_NAMESPACE", "ns")
	t.Setenv("RAG_ROCKETMQ_ACCESS_KEY", "key")
	t.Setenv("RAG_ROCKETMQ_SECRET_KEY", "secret")
	t.Setenv("RAG_ROCKETMQ_TLS", "YES")
	settings = RocketMQSettingsFromEnv()
	if settings.Endpoint != "localhost:8081" || settings.Topic != "topic" || settings.Group != "group" ||
		settings.Namespace != "ns" || settings.AccessKey != "key" || settings.SecretKey != "secret" || !settings.TLS {
		t.Fatalf("unexpected queue settings: %+v", settings)
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
	if settings.TTLSeconds != 3600 || settings.MaxResultBytes != 16*1024*1024 ||
		settings.MaxArtifactBytes != 64*1024*1024 || settings.MaxArtifactsPerSearch != 100 {
		t.Fatalf("unexpected snapshot defaults: %+v", settings)
	}
	t.Setenv("RAG_RESULT_TTL_SECONDS", "19")
	t.Setenv("RAG_RESULT_MAX_BYTES", "123")
	t.Setenv("RAG_ARTIFACT_MAX_BYTES", "456")
	t.Setenv("RAG_RESULT_MAX_ARTIFACTS", "7")
	settings, err = SnapshotSettingsFromEnv()
	if err != nil {
		t.Fatalf("SnapshotSettingsFromEnv() error = %v", err)
	}
	if settings.TTLSeconds != 19 || settings.MaxResultBytes != 123 ||
		settings.MaxArtifactBytes != 456 || settings.MaxArtifactsPerSearch != 7 {
		t.Fatalf("unexpected snapshot settings: %+v", settings)
	}
	for _, name := range []string{
		"RAG_RESULT_TTL_SECONDS", "RAG_RESULT_MAX_BYTES", "RAG_ARTIFACT_MAX_BYTES", "RAG_RESULT_MAX_ARTIFACTS",
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
