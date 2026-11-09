package state

import (
	"context"
	"errors"
	"testing"
)

// TestCatalogMutationIsPersistedAndAtomic ports the repository-level behavior
// of tests/test_state_store.py::test_catalog_mutation_is_persisted_and_atomic
// (the CatalogStore facade itself belongs to the MCP package).
func TestCatalogMutationIsPersistedAndAtomic(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	loaded, found, err := stores.Catalogs.Load(ctx, "alpha")
	if err != nil || found || loaded != "" {
		t.Fatalf("Load(empty) = %q, %v, %v", loaded, found, err)
	}

	firstData := `{"tools":{"search":{"enabled":false}},"prompts":{}}`
	result, err := stores.Catalogs.Mutate(ctx, "alpha", func(current *string) (string, any, bool, error) {
		if current != nil {
			return "", nil, false, errors.New("unexpected existing catalog")
		}
		return firstData, true, true, nil
	})
	if err != nil {
		t.Fatalf("Mutate(insert) error = %v", err)
	}
	if result != true {
		t.Fatalf("Mutate(insert) result = %v, want true", result)
	}
	loaded, found, err = stores.Catalogs.Load(ctx, "alpha")
	if err != nil || !found || loaded != firstData {
		t.Fatalf("Load() = %q, %v, %v", loaded, found, err)
	}
	if version := catalogVersion(t, stores, "alpha"); version != 1 {
		t.Fatalf("version = %d, want 1", version)
	}

	// An unchanged mutation keeps the stored row and the version.
	result, err = stores.Catalogs.Mutate(ctx, "alpha", func(current *string) (string, any, bool, error) {
		if current == nil || *current != firstData {
			return "", nil, false, errors.New("catalog content was not preserved")
		}
		return *current, false, false, nil
	})
	if err != nil {
		t.Fatalf("Mutate(unchanged) error = %v", err)
	}
	if result != false {
		t.Fatalf("Mutate(unchanged) result = %v, want false", result)
	}
	if version := catalogVersion(t, stores, "alpha"); version != 1 {
		t.Fatalf("version after unchanged mutation = %d, want 1", version)
	}

	// A transform error rolls the transaction (including the revision bump) back.
	sentinel := errors.New("transform rejected the change")
	_, err = stores.Catalogs.Mutate(ctx, "alpha", func(current *string) (string, any, bool, error) {
		return `{"tampered":true}`, nil, true, sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Mutate(error) = %v, want the transform error", err)
	}
	loaded, found, err = stores.Catalogs.Load(ctx, "alpha")
	if err != nil || !found || loaded != firstData {
		t.Fatalf("Load() after failed mutation = %q, %v, %v", loaded, found, err)
	}
	if version := catalogVersion(t, stores, "alpha"); version != 1 {
		t.Fatalf("version after failed mutation = %d, want 1", version)
	}

	secondData := `{"tools":{},"prompts":{}}`
	result, err = stores.Catalogs.Mutate(ctx, "alpha", func(current *string) (string, any, bool, error) {
		if current == nil || *current != firstData {
			return "", nil, false, errors.New("catalog content was not preserved")
		}
		return secondData, true, true, nil
	})
	if err != nil || result != true {
		t.Fatalf("Mutate(update) = %v, %v", result, err)
	}
	if version := catalogVersion(t, stores, "alpha"); version != 2 {
		t.Fatalf("version after update = %d, want 2", version)
	}
	loaded, found, err = stores.Catalogs.Load(ctx, "alpha")
	if err != nil || !found || loaded != secondData {
		t.Fatalf("Load() after update = %q, %v, %v", loaded, found, err)
	}

	// Namespaces are independent rows, like CatalogStore.for_namespace.
	if _, err := stores.Catalogs.Mutate(ctx, "beta", func(current *string) (string, any, bool, error) {
		return `{"tools":{},"prompts":{}}`, true, true, nil
	}); err != nil {
		t.Fatalf("Mutate(beta) error = %v", err)
	}
	beta, found, err := stores.Catalogs.Load(ctx, "beta")
	if err != nil || !found || beta != `{"tools":{},"prompts":{}}` {
		t.Fatalf("Load(beta) = %q, %v, %v", beta, found, err)
	}
}

func catalogVersion(t *testing.T, stores *Stores, namespace string) int64 {
	t.Helper()
	var version int64
	if err := stores.DB.QueryRowContext(context.Background(),
		"SELECT version FROM mcp_catalogs WHERE namespace = ?", namespace).Scan(&version); err != nil {
		t.Fatalf("catalog version query error = %v", err)
	}
	return version
}
