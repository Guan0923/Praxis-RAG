package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/BurntSushi/toml"
)

// DefaultConfig is written once to ~/.praxis-rag/config.toml on first start.
const DefaultConfig = `# Praxis-RAG service settings. Restart the service after editing.
# Environment variables override these values.
[environment]
RAG_BACKEND = "qdrant"
RAG_SERVICE_HOST = "127.0.0.1"
RAG_SERVICE_PORT = "8011"
RAG_SERVICE_DATA_ROOT = "~/.praxis-rag/data"
QDRANT_URL = "http://127.0.0.1:6333"
RAG_COLLECTION = "mini_agent_knowledge_pdf_assets_v1"
RAG_WORD_CONVERT_TIMEOUT = "120"

# Shared state store. Set RAG_MYSQL_HOST (plus RAG_MYSQL_USER/DATABASE) for a
# MySQL deployment, or RAG_STATE_URL for an explicit SQLAlchemy URL.
# Credentials belong in the environment, not in this file:
#   RAG_MYSQL_PASSWORD, RAG_STATE_ENCRYPTION_KEY
# RAG_MYSQL_HOST = "127.0.0.1"
# RAG_MYSQL_PORT = "3306"
# RAG_MYSQL_USER = "rag"
# RAG_MYSQL_DATABASE = "praxis_rag"

# Set actual local paths to enable DOC conversion and PDF/Word vision.
# RAG_WORD_CONVERTER_PATH = 'C:/Program Files/LibreOffice/program/soffice.exe'
# RAG_PDF_PARSER_PYTHON = 'C:/path/to/parser/python.exe'
# RAG_PDF_MODELS_PATH = 'C:/path/to/docling/models'
`

// observedKeys are the settings accepted inside [environment]. This mirrors
// startup_config._KEYS in the Python implementation.
var observedKeys = []string{
	"RAG_BACKEND", "RAG_SERVICE_HOST", "RAG_SERVICE_PORT",
	"RAG_SERVICE_DATA_ROOT", "RAG_SERVICE_TOKEN", "RAG_MCP_ALLOWED_HOSTS",
	"RAG_MCP_ALLOWED_ORIGINS", "QDRANT_URL", "QDRANT_PATH", "RAG_COLLECTION",
	"RAG_STATE_URL", "RAG_MYSQL_HOST", "RAG_MYSQL_PORT", "RAG_MYSQL_USER",
	"RAG_MYSQL_DATABASE", "RAG_MYSQL_SSL_CA", "RAG_STATE_POOL_SIZE",
	"RAG_STATE_MAX_OVERFLOW", "RAG_STATE_POOL_RECYCLE", "RAG_STATE_POOL_TIMEOUT",
	"RAG_RESULT_TTL_SECONDS", "RAG_RESULT_MAX_BYTES", "RAG_ARTIFACT_MAX_BYTES", "RAG_RESULT_MAX_ARTIFACTS",
	"RAG_REDIS_ENABLED", "RAG_REDIS_URL", "RAG_REDIS_PREFIX", "RAG_REDIS_TTL_SECONDS",
	"RAG_REDIS_TIMEOUT_SECONDS",
	"RAG_KAFKA_BOOTSTRAP_SERVERS", "RAG_KAFKA_CLIENT_ID", "RAG_KAFKA_SECURITY_PROTOCOL",
	"RAG_KAFKA_SASL_MECHANISM", "RAG_KAFKA_SASL_USERNAME", "RAG_KAFKA_SASL_PASSWORD",
	"RAG_KAFKA_TLS_CA", "RAG_KAFKA_TLS_INSECURE", "RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS",
	"RAG_CACHE_TOPIC", "RAG_CACHE_GROUP",
	"RAG_STATE_ECHO", "RAG_STATE_AUTO_SCHEMA", "RAG_UPLOAD_LEASE_SECONDS",
	"RAG_UPLOAD_CLEANUP_DELAY_SECONDS",
	"RAG_DOCUMENT_TOPIC", "RAG_DOCUMENT_GROUP", "RAG_DOCUMENT_DLQ_TOPIC",
	"RAG_DOCUMENT_DLQ_GROUP",
	"RAG_DOCUMENT_BROKER_MAX_RETRIES",
	"RAG_SEARCH_TOPIC", "RAG_SEARCH_GROUP", "RAG_SEARCH_DLQ_TOPIC",
	"RAG_SEARCH_DLQ_GROUP", "RAG_SEARCH_TIMEOUT_SECONDS",
	"RAG_SEARCH_MAX_ATTEMPTS", "RAG_SEARCH_LEASE_SECONDS", "RAG_SEARCH_RETRY_SECONDS",
	"RAG_EMBEDDING_MODEL", "RAG_EMBEDDING_DEVICE", "RAG_CHUNK_TOKENIZER_MODEL",
	"RAG_DENSE_VECTOR_SIZE", "RAG_PRELOAD_MODEL", "RAG_NAMESPACE", "RAG_RERANKER_MODEL",
	"RAG_RERANKER_DEVICE", "RAG_RERANKER_BATCH_SIZE", "RAG_RERANKER_MAX_CANDIDATES",
	"RAG_RERANKER_MAX_LENGTH", "RAG_RERANKER_MAX_TEXT_CHARS", "RAG_PRELOAD_RERANKER",
	"RAG_PDF_PARSER_PYTHON", "RAG_PDF_MODELS_PATH", "RAG_PDF_PARSE_TIMEOUT",
	"RAG_WORD_CONVERTER_PATH", "RAG_WORD_CONVERT_TIMEOUT",
}

// ignoredDocumentKeys are legacy settings still accepted but never applied.
var ignoredDocumentKeys = map[string]struct{}{
	"RAG_DOCUMENT_MAX_ATTEMPTS":               {},
	"RAG_DOCUMENT_LEASE_SECONDS":              {},
	"RAG_DOCUMENT_RETRY_SECONDS":              {},
	"RAG_DOCUMENT_DISPATCH_LEASE_SECONDS":     {},
	"RAG_DOCUMENT_DISPATCH_RETRY_SECONDS":     {},
	"RAG_DOCUMENT_DISPATCH_MAX_RETRY_SECONDS": {},
	"RAG_DOCUMENT_DISPATCH_ALERT_SECONDS":     {},
}

var allowedConfigKeys = func() map[string]struct{} {
	keys := make(map[string]struct{}, len(observedKeys)+len(ignoredDocumentKeys))
	for _, key := range observedKeys {
		keys[key] = struct{}{}
	}
	for key := range ignoredDocumentKeys {
		keys[key] = struct{}{}
	}
	return keys
}()

// LoadStartupConfig creates the default config once, validates the file, and
// applies its [environment] values to the process without overriding existing
// environment variables. It returns the config path.
func LoadStartupConfig() (string, error) {
	override := strings.TrimSpace(os.Getenv("RAG_CONFIG_FILE"))
	var path string
	if override != "" {
		path = ExpandPath(override)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("Cannot resolve the home directory for the Praxis-RAG config")
		}
		path = filepath.Join(home, ".praxis-rag", "config.toml")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return "", fmt.Errorf("Cannot read Praxis-RAG config: %s; check file access and TOML syntax", path)
		}
		file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err == nil {
			_, writeErr := file.WriteString(DefaultConfig)
			_ = file.Close()
			if writeErr != nil {
				return "", fmt.Errorf("Cannot write the default Praxis-RAG config: %s", path)
			}
		} else if !os.IsExist(err) {
			return "", fmt.Errorf("Cannot read Praxis-RAG config: %s; check file access and TOML syntax", path)
		}
	}

	var document map[string]any
	if _, err := toml.DecodeFile(path, &document); err != nil {
		return "", fmt.Errorf("Cannot read Praxis-RAG config: %s; check file access and TOML syntax", path)
	}
	if len(document) != 1 {
		return "", fmt.Errorf("%s: expected one [environment] section", path)
	}
	rawEnvironment, ok := document["environment"]
	if !ok {
		return "", fmt.Errorf("%s: expected one [environment] section", path)
	}
	environment, ok := rawEnvironment.(map[string]any)
	if !ok {
		return "", fmt.Errorf("%s: expected one [environment] section", path)
	}
	for key, value := range environment {
		if _, allowed := allowedConfigKeys[key]; !allowed {
			return "", fmt.Errorf("%s: unknown setting or non-string value: %s", path, key)
		}
		if _, isString := value.(string); !isString {
			return "", fmt.Errorf("%s: unknown setting or non-string value: %s", path, key)
		}
	}

	databaseOverride := os.Getenv("QDRANT_URL") != "" || os.Getenv("QDRANT_PATH") != ""
	for key, value := range environment {
		if _, ignored := ignoredDocumentKeys[key]; ignored {
			continue
		}
		if databaseOverride && (key == "QDRANT_URL" || key == "QDRANT_PATH") {
			continue
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		_ = os.Setenv(key, value.(string))
	}
	return path, nil
}
