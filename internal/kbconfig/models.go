package kbconfig

import (
	"context"
	"errors"
	"fmt"
	"net/http"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
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
	connection, err := DraftConnection(current, values)
	if err != nil {
		return nil, err
	}
	return ModelsForConnection(ctx, connection, factory)
}

// ModelsForConnection lists provider models for one already-resolved connection,
// with no client overrides. It backs the save-model-configuration flow (the
// stored connection is decrypted with the service key rather than an environment
// lookup) and shares ModelsFor's "no /models route means no models" handling.
func ModelsForConnection(
	ctx context.Context,
	connection modelapi.ModelConnection,
	factory ModelListerFactory,
) ([]string, error) {
	if factory == nil {
		factory = DefaultModelListerFactory
	}
	models, err := factory(connection).Models(ctx)
	if err != nil {
		// A 404 usually means the provider has no /models route (reranker-only
		// endpoints are common). Model listing is a convenience for the Console,
		// so report "no models" instead of failing the form.
		var modelError *modelapi.ModelAPIError
		if errors.As(err, &modelError) && modelError.Status == http.StatusNotFound {
			return []string{}, nil
		}
		return nil, err
	}
	if models == nil {
		return []string{}, nil
	}
	return models, nil
}
