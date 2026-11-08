package state

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAPIKeyCreateWithEmptyNamespacesStoresJSONArray(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	created, err := stores.APIKeys.Create(ctx, "empty", nil)
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	if strings.Contains(string(encoded), `"namespaces":null`) {
		t.Fatalf("empty namespaces marshalled to null: %s", encoded)
	}
	if !strings.Contains(string(encoded), `"namespaces":[]`) {
		t.Fatalf("empty namespaces marshalled unexpectedly: %s", encoded)
	}
	namespaces, err := stores.APIKeys.NamespacesForKey(ctx, created.Key)
	if err != nil {
		t.Fatalf("NamespacesForKey() error = %v", err)
	}
	if len(namespaces) != 0 {
		t.Fatalf("namespaces = %v, want none", namespaces)
	}
}
