// Package runtime owns the long-lived service state: configuration, the shared
// state database, and (in later phases) Qdrant clients, retrieval components and
// tenant resources. The current skeleton reports health and owns the database
// lifecycle; document/search orchestration is attached in G5.
package runtime

import (
	"context"
	"fmt"
	"os"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// Service is the process-wide RAG service runtime.
type Service struct {
	config   config.RagConfig
	stores   *state.Stores
	dataRoot string
}

// New validates the runtime configuration and prepares the data root. A
// backend other than qdrant is rejected exactly like RagServiceRuntime.
func New(cfg config.RagConfig, stores *state.Stores, dataRoot string) (*Service, error) {
	if cfg.Backend != config.BackendQdrant {
		return nil, fmt.Errorf("Standalone RAG service requires RAG_BACKEND=qdrant.")
	}
	if stores == nil {
		return nil, fmt.Errorf("Standalone RAG service requires the shared state store.")
	}
	root := config.ExpandPath(dataRoot)
	if root == "" {
		return nil, fmt.Errorf("RAG service data root cannot be empty.")
	}
	if info, err := os.Lstat(root); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("RAG service data root cannot be a symbolic link.")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("Cannot create the RAG service data root: %s", root)
	}
	return &Service{config: cfg, stores: stores, dataRoot: root}, nil
}

// DataRoot returns the resolved data root.
func (s *Service) DataRoot() string { return s.dataRoot }

// Config returns the resolved retrieval configuration.
func (s *Service) Config() config.RagConfig { return s.config }

// State returns the shared state stores.
func (s *Service) Stores() *state.Stores { return s.stores }

// Status renders the secret-free health payload returned by /health.
func (s *Service) Status(context.Context) map[string]any {
	return map[string]any{
		"status":     "ok",
		"service":    version.ServiceName,
		"backend":    string(s.config.Backend),
		"collection": s.config.Collection,
		"state":      s.stores.Describe(),
	}
}

// Close releases runtime resources (the shared database).
func (s *Service) Close(context.Context) error { return s.stores.Close() }
