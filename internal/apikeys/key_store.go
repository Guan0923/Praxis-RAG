// Package apikeys is the service-credential facade over the shared API-key
// repository. It mirrors rag_mcp/api_keys.py: format validation and the safe
// error categories stay at this boundary, secrets never leave the repository
// except in Create and SecretForNamespace.
package apikeys

import (
	"context"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// Error is a safe, user-facing credential error.
type Error struct{ Message string }

// Error implements error.
func (e *Error) Error() string { return e.Message }

// Store wraps an API-key repository.
type Store struct {
	repository *state.APIKeyRepository
}

// New builds a Store over the shared state repository.
func New(repository *state.APIKeyRepository) *Store {
	return &Store{repository: repository}
}

// List returns display-only metadata, newest first. Secrets are never included.
func (s *Store) List(ctx context.Context) ([]state.APIKey, error) {
	return s.repository.List(ctx)
}

// ListByOwner returns only the keys issued by one Console account.
func (s *Store) ListByOwner(ctx context.Context, ownerUserID string) ([]state.APIKey, error) {
	return s.repository.ListByOwner(ctx, ownerUserID)
}

// Get returns one key (with its owner projection), or nil when it is missing.
func (s *Store) Get(ctx context.Context, keyID string) (*state.APIKey, error) {
	return s.repository.Get(ctx, keyID)
}

// Create issues one new credential. Duplicate digests map to "API Key 已存在".
func (s *Store) Create(ctx context.Context, name string, namespaces []string) (state.CreatedAPIKey, error) {
	created, err := s.repository.Create(ctx, name, namespaces)
	if err != nil {
		var conflict *state.StateConflict
		if asStateConflict(err, &conflict) {
			return state.CreatedAPIKey{}, &Error{Message: "API Key 已存在"}
		}
		return state.CreatedAPIKey{}, err
	}
	return created, nil
}

// CreateForOwner issues one new credential owned by the given Console account.
// An empty ownerUserID creates an unowned (administrator-only) credential, the
// same as Create.
func (s *Store) CreateForOwner(ctx context.Context, name string, namespaces []string, ownerUserID string) (state.CreatedAPIKey, error) {
	created, err := s.repository.CreateForOwner(ctx, name, namespaces, ownerUserID)
	if err != nil {
		var conflict *state.StateConflict
		if asStateConflict(err, &conflict) {
			return state.CreatedAPIKey{}, &Error{Message: "API Key 已存在"}
		}
		return state.CreatedAPIKey{}, err
	}
	return created, nil
}

// Update applies a full metadata replacement; nil means the key is missing.
func (s *Store) Update(ctx context.Context, keyID, name string, namespaces []string, enabled bool) (*state.APIKey, error) {
	return s.repository.Update(ctx, keyID, name, namespaces, enabled)
}

// Delete removes a key and reports whether it existed.
func (s *Store) Delete(ctx context.Context, keyID string) (bool, error) {
	return s.repository.Delete(ctx, keyID)
}

// NamespacesForKey authenticates one secret. It mirrors
// ApiKeyStore.namespaces_for_key: malformed secrets are rejected before the
// digest lookup and repository errors become safe credential errors.
func (s *Store) NamespacesForKey(ctx context.Context, secret string) ([]string, error) {
	if !strings.HasPrefix(secret, "prk_") || len(secret) > 128 {
		return nil, &Error{Message: "访问 API Key 格式错误，需要 prk 开头的服务 API Key"}
	}
	namespaces, err := s.repository.NamespacesForKey(ctx, secret)
	if err != nil {
		var lookup *state.LookupError
		if asLookupError(err, &lookup) {
			return nil, &Error{Message: lookup.Message}
		}
		var permission *state.PermissionError
		if asPermissionError(err, &permission) {
			return nil, &Error{Message: permission.Message}
		}
		return nil, err
	}
	return namespaces, nil
}

// SecretForNamespace returns the exportable secret for one namespace. Errors
// are the repository's LookupError / PermissionError / ValueError types so the
// HTTP layer can map them to 404 / 403 / 409.
func (s *Store) SecretForNamespace(ctx context.Context, keyID, namespace string) (string, error) {
	return s.repository.SecretForNamespace(ctx, keyID, namespace)
}

func asStateConflict(err error, target **state.StateConflict) bool {
	conflict, ok := err.(*state.StateConflict)
	if ok {
		*target = conflict
	}
	return ok
}

func asLookupError(err error, target **state.LookupError) bool {
	lookup, ok := err.(*state.LookupError)
	if ok {
		*target = lookup
	}
	return ok
}

func asPermissionError(err error, target **state.PermissionError) bool {
	permission, ok := err.(*state.PermissionError)
	if ok {
		*target = permission
	}
	return ok
}
