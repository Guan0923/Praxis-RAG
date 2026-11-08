// Package runtime owns the long-lived service state: configuration, state
// database, Qdrant client, and tenant resources. The G0 skeleton only reports
// health; later phases attach storage, retrieval, and workers here.
package runtime

import (
	"context"
	"fmt"
	"os"

	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// Service is the process-wide RAG service runtime.
type Service struct {
	config   config.RagConfig
	state    config.StateSettings
	dataRoot string
}

// New validates the runtime configuration and prepares the data root. A
// backend other than qdrant is rejected exactly like RagServiceRuntime.
func New(cfg config.RagConfig, state config.StateSettings, dataRoot string) (*Service, error) {
	if cfg.Backend != config.BackendQdrant {
		return nil, fmt.Errorf("Standalone RAG service requires RAG_BACKEND=qdrant.")
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
	return &Service{config: cfg, state: state, dataRoot: root}, nil
}

// DataRoot returns the resolved data root.
func (s *Service) DataRoot() string { return s.dataRoot }

// Config returns the resolved retrieval configuration.
func (s *Service) Config() config.RagConfig { return s.config }

// State returns the resolved state database settings.
func (s *Service) State() config.StateSettings { return s.state }

// Status renders the secret-free health payload returned by /health.
func (s *Service) Status(context.Context) map[string]any {
	return map[string]any{
		"status":     "ok",
		"service":    version.ServiceName,
		"backend":    string(s.config.Backend),
		"collection": s.config.Collection,
		"state":      s.state.Describe(),
	}
}

// Close releases runtime resources. The G0 skeleton has nothing to release.
func (s *Service) Close(context.Context) error { return nil }
