package connect

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTemplate writes one connector template into a temp dir.
func writeTemplate(t *testing.T, dir, name, body string) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write template: %v", err)
	}
	return path
}

func TestLoadConnectorFileExpandsNameAndConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeTemplate(t, dir, "document.json", `{
  "name": "${RAG_CONNECT_DOCUMENT_CONNECTOR:-praxis-document-cdc}",
  "config": {
    "connector.class": "io.debezium.connector.mysql.MySqlConnector",
    "database.hostname": "${RAG_MYSQL_HOST:-mysql}",
    "database.password": "${RAG_MYSQL_DEBEZIUM_PASSWORD}",
    "tasks.max": "1"
  }
}`)
	spec, err := LoadConnectorFile(path, envMap(map[string]string{
		"RAG_MYSQL_HOST":              "mysql-2",
		"RAG_MYSQL_DEBEZIUM_PASSWORD": "pa\"ss",
	}))
	if err != nil {
		t.Fatalf("LoadConnectorFile: %v", err)
	}
	if spec.Name != DefaultDocumentConnector || spec.Source != "document.json" {
		t.Fatalf("spec = %+v", spec)
	}
	if spec.Config["database.hostname"] != "mysql-2" ||
		spec.Config["database.password"] != `pa"ss` ||
		spec.Config["tasks.max"] != "1" {
		t.Fatalf("config = %v", spec.Config)
	}
}

func TestLoadConnectorFileRequiresUnsetPassword(t *testing.T) {
	dir := t.TempDir()
	path := writeTemplate(t, dir, "cache.json", `{
  "name": "cache",
  "config": {"database.password": "${RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD}"}
}`)
	_, err := LoadConnectorFile(path, envMap(nil))
	if err == nil || !strings.Contains(err.Error(), "RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD") {
		t.Fatalf("error = %v, want a missing-password error", err)
	}
}

func TestLoadConnectorFileRejectsNonStringConfig(t *testing.T) {
	dir := t.TempDir()
	path := writeTemplate(t, dir, "bad.json", `{"name":"bad","config":{"tasks.max":1}}`)
	if _, err := LoadConnectorFile(path, envMap(nil)); err == nil ||
		!strings.Contains(err.Error(), "must be a string") {
		t.Fatalf("error = %v", err)
	}
}

func TestLoadConnectorDirLoadsBothInOrderAndRejectsDuplicates(t *testing.T) {
	dir := t.TempDir()
	writeTemplate(t, dir, "b-cache.json", `{"name":"cache-cdc","config":{"k":"b"}}`)
	writeTemplate(t, dir, "a-document.json", `{"name":"doc-cdc","config":{"k":"a"}}`)
	writeTemplate(t, dir, "notes.txt", "ignored")
	specs, err := LoadConnectorDir(dir, envMap(nil))
	if err != nil {
		t.Fatalf("LoadConnectorDir: %v", err)
	}
	if len(specs) != 2 || specs[0].Name != "doc-cdc" || specs[1].Name != "cache-cdc" {
		t.Fatalf("specs = %+v", specs)
	}

	duplicateDir := t.TempDir()
	writeTemplate(t, duplicateDir, "one.json", `{"name":"same","config":{}}`)
	writeTemplate(t, duplicateDir, "two.json", `{"name":"same","config":{}}`)
	if _, err := LoadConnectorDir(duplicateDir, envMap(nil)); err == nil ||
		!strings.Contains(err.Error(), "declared by both") {
		t.Fatalf("duplicate error = %v", err)
	}

	emptyDir := t.TempDir()
	if _, err := LoadConnectorDir(emptyDir, envMap(nil)); err == nil {
		t.Fatal("expected an empty-directory error")
	}
}
