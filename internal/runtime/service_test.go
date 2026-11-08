package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

func newTestStores(t *testing.T) *state.Stores {
	t.Helper()
	root := t.TempDir()
	settings := config.StateSettings{
		URL:        "sqlite+pysqlite:///" + filepath.ToSlash(filepath.Join(root, "state.sqlite3")),
		SQLitePath: filepath.Join(root, "state.sqlite3"),
		AutoSchema: true,
	}
	stores, err := state.New(settings, true)
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

func TestNewRequiresQdrantBackend(t *testing.T) {
	_, err := New(config.RagConfig{Backend: config.BackendMock}, newTestStores(t), t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "RAG_BACKEND=qdrant") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewRequiresStateStore(t *testing.T) {
	_, err := New(config.RagConfig{Backend: config.BackendQdrant}, nil, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "shared state store") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewCreatesDataRootAndReportsStatus(t *testing.T) {
	dataRoot := filepath.Join(t.TempDir(), "data")
	service, err := New(config.RagConfig{
		Backend:    config.BackendQdrant,
		Collection: "mini_agent_knowledge_pdf_assets_v1",
	}, newTestStores(t), dataRoot)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer func() { _ = service.Close(context.Background()) }()

	if info, statErr := os.Stat(dataRoot); statErr != nil || !info.IsDir() {
		t.Fatalf("data root was not created: %v", statErr)
	}
	status := service.Status(context.Background())
	if status["status"] != "ok" || status["service"] != "praxis-rag" {
		t.Fatalf("status = %v", status)
	}
	if status["backend"] != "qdrant" || status["collection"] != "mini_agent_knowledge_pdf_assets_v1" {
		t.Fatalf("status = %v", status)
	}
	statePayload, ok := status["state"].(map[string]any)
	if !ok || statePayload["backend"] != "sqlite" {
		t.Fatalf("state = %v", status["state"])
	}
}
