package httpapi

import (
	"context"
	"net/http"
	"testing"
)

// TestAPIKeyBatchDeleteSessionOwnership covers the account filter of the atomic
// batch delete: a normal account deletes its own credential, another account's
// credential is reported as missing (the single-key 404 semantics) and survives,
// and an administrator may delete the rest.
func TestAPIKeyBatchDeleteSessionOwnership(t *testing.T) {
	fixture := newAPIKeyOwnerFixture(t)
	handler := fixture.server.Handler()
	ctx := context.Background()

	aliceKey := fixture.createAPIKeyOwnerKey(t, "batch-owner-alice-key", []string{"alice-kb"}, fixture.alice.ID)
	bobKey := fixture.createAPIKeyOwnerKey(t, "batch-owner-bob-key", []string{"bob-kb"}, fixture.bob.ID)

	// Bob's batch tries to delete both keys; only his own is deleted.
	response := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-bob"),
		body:    batchDeleteBody(t, aliceKey.ID, bobKey.ID, "batch-owner-missing-id"),
	})
	if response.Code != http.StatusOK {
		t.Fatalf("batch delete status = %d body = %s", response.Code, response.Body.String())
	}
	body := decodeBody(t, response)
	if deleted := batchDeleteStringList(t, body["deleted"]); len(deleted) != 1 || deleted[0] != bobKey.ID {
		t.Fatalf("deleted = %v, want [%s]", deleted, bobKey.ID)
	}
	notFound := batchDeleteStringList(t, body["not_found"])
	if len(notFound) != 2 || notFound[0] != aliceKey.ID || notFound[1] != "batch-owner-missing-id" {
		t.Fatalf("not_found = %v", notFound)
	}
	if failed, ok := body["failed"].([]any); !ok || len(failed) != 0 {
		t.Fatalf("failed = %v", body["failed"])
	}
	if body["deleted_count"] != float64(1) {
		t.Fatalf("deleted_count = %v", body["deleted_count"])
	}
	if key, err := fixture.stores.APIKeys.Get(ctx, aliceKey.ID); err != nil || key == nil {
		t.Fatalf("alice key = %+v, %v; want it to survive bob's batch", key, err)
	}
	if key, err := fixture.stores.APIKeys.Get(ctx, bobKey.ID); err != nil || key != nil {
		t.Fatalf("bob key = %+v, %v; want nil", key, err)
	}

	// The administrator's batch removes the remaining key.
	admin := performRequest(handler, testRequest{
		method:  "POST",
		target:  "/v1/admin/api-keys/batch-delete",
		remote:  "10.0.0.9:5000",
		headers: apiKeyOwnerWriteHeaders("token-admin"),
		body:    batchDeleteBody(t, aliceKey.ID),
	})
	if admin.Code != http.StatusOK {
		t.Fatalf("admin batch delete status = %d body = %s", admin.Code, admin.Body.String())
	}
	if deleted := batchDeleteStringList(t, decodeBody(t, admin)["deleted"]); len(deleted) != 1 || deleted[0] != aliceKey.ID {
		t.Fatalf("admin deleted = %v", deleted)
	}
	if key, err := fixture.stores.APIKeys.Get(ctx, aliceKey.ID); err != nil || key != nil {
		t.Fatalf("alice key after admin batch = %+v, %v; want nil", key, err)
	}
}
