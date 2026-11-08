package kbconfig

import (
	"context"
	"fmt"

	"github.com/Guan0923/xinghe_rag/internal/modelapi"
)

// ModelLister is the minimal provider model-listing transport seam.
type ModelLister interface {
	Models(ctx context.Context) ([]string, error)
}

// ModelListerFactory builds a lister for one resolved draft connection.
type ModelListerFactory func(connection modelapi.ModelConnection) ModelLister

// DefaultModelListerFactory lists models through the HTTP model API with the
// standard 60 second timeout.
func DefaultModelListerFactory(connection modelapi.ModelConnection) ModelLister {
	return modelapi.NewModelHTTPClient(connection, modelapi.DefaultTimeout)
}

// ModelsFor lists provider models for one knowledge-base model kind, matching
// RagServiceRuntime.knowledge_base_models: the stored connection is used when it
// is valid, then the client draft (base_url/apikey) is layered on top before
// calling GET /models.
//
// currentSettings is the stored knowledge-base settings, or nil when the
// knowledge base does not exist yet (create form). A nil factory uses
// DefaultModelListerFactory.
func ModelsFor(
	ctx context.Context,
	kind string,
	currentSettings map[string]any,
	values map[string]any,
	factory ModelListerFactory,
) ([]string, error) {
	if !IsKind(kind) {
		return nil, fmt.Errorf("unknown model kind %q", kind)
	}
	current := modelapi.ModelConnection{}
	if currentSettings != nil {
		resolved, err := ModelConnectionFor(currentSettings, kind)
		if err != nil {
			return nil, err
		}
		current = resolved
	}
	draft, err := DraftConnection(current, values)
	if err != nil {
		return nil, err
	}
	if factory == nil {
		factory = DefaultModelListerFactory
	}
	return factory(draft).Models(ctx)
}
