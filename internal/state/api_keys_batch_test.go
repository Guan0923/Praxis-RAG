package state

import (
	"context"
	"testing"
)

// batchContains reports whether values holds want.
func batchContains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

// TestAPIKeyRepositoryDeleteManyBatchesTransactionally covers the atomic batch
// delete: one SELECT classifies the requested ids, one multi-row DELETE removes
// the existing rows, and the read-model revision plus outbox event are written
// once for the whole batch instead of once per key.
func TestAPIKeyRepositoryDeleteManyBatchesTransactionally(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	first, err := stores.APIKeys.Create(ctx, "batch-first", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create(batch-first) error = %v", err)
	}
	second, err := stores.APIKeys.Create(ctx, "batch-second", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create(batch-second) error = %v", err)
	}
	third, err := stores.APIKeys.Create(ctx, "batch-third", []string{"alpha"})
	if err != nil {
		t.Fatalf("Create(batch-third) error = %v", err)
	}

	revisionBefore := revision(t, stores, "api_keys", "")
	outboxBefore := outboxCount(t, stores)

	deleted, missing, err := stores.APIKeys.DeleteMany(ctx,
		[]string{first.ID, "batch-missing-id", second.ID, second.ID, ""}, "")
	if err != nil {
		t.Fatalf("DeleteMany() error = %v", err)
	}
	if len(deleted) != 2 || deleted[0] != first.ID || deleted[1] != second.ID {
		t.Fatalf("deleted = %v, want [%s %s]", deleted, first.ID, second.ID)
	}
	if len(missing) != 1 || missing[0] != "batch-missing-id" {
		t.Fatalf("missing = %v, want [batch-missing-id]", missing)
	}
	if got := revision(t, stores, "api_keys", ""); got != revisionBefore+1 {
		t.Fatalf("revision = %d, want %d (one bump for the whole batch)", got, revisionBefore+1)
	}
	if got := outboxCount(t, stores); got != outboxBefore+1 {
		t.Fatalf("outbox count = %d, want %d (one event for the whole batch)", got, outboxBefore+1)
	}
	for _, keyID := range []string{first.ID, second.ID} {
		key, err := stores.APIKeys.Get(ctx, keyID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", keyID, err)
		}
		if key != nil {
			t.Fatalf("Get(%s) = %+v, want nil after the batch delete", keyID, key)
		}
	}
	survivor, err := stores.APIKeys.Get(ctx, third.ID)
	if err != nil || survivor == nil {
		t.Fatalf("Get(batch-third) = %+v, %v; want the unsubmitted key to survive", survivor, err)
	}

	// A second batch of already-deleted ids reports them as missing and writes
	// no revision or outbox row.
	revisionAfter := revision(t, stores, "api_keys", "")
	outboxAfter := outboxCount(t, stores)
	deletedAgain, missingAgain, err := stores.APIKeys.DeleteMany(ctx, []string{first.ID, second.ID}, "")
	if err != nil {
		t.Fatalf("DeleteMany(again) error = %v", err)
	}
	if len(deletedAgain) != 0 {
		t.Fatalf("deleted again = %v, want none", deletedAgain)
	}
	if !batchContains(missingAgain, first.ID) || !batchContains(missingAgain, second.ID) {
		t.Fatalf("missing again = %v", missingAgain)
	}
	if got := revision(t, stores, "api_keys", ""); got != revisionAfter {
		t.Fatalf("revision after empty batch = %d, want %d", got, revisionAfter)
	}
	if got := outboxCount(t, stores); got != outboxAfter {
		t.Fatalf("outbox count after empty batch = %d, want %d", got, outboxAfter)
	}
}

// TestAPIKeyRepositoryDeleteManyOwnerScoped covers the account filter: a normal
// account may delete its own key, while another account's key is reported as
// missing (the single-key 404 semantics) and survives; an empty owner id keeps
// the trusted local management behaviour.
func TestAPIKeyRepositoryDeleteManyOwnerScoped(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	alice, err := stores.Users.Create(ctx, "batch-scope-alice", "x")
	if err != nil {
		t.Fatalf("Users.Create(alice) error = %v", err)
	}
	bob, err := stores.Users.Create(ctx, "batch-scope-bob", "x")
	if err != nil {
		t.Fatalf("Users.Create(bob) error = %v", err)
	}
	aliceKey, err := stores.APIKeys.CreateForOwner(ctx, "alice-batch-key", []string{"alpha"}, alice.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(alice) error = %v", err)
	}
	bobKey, err := stores.APIKeys.CreateForOwner(ctx, "bob-batch-key", []string{"alpha"}, bob.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(bob) error = %v", err)
	}

	deleted, missing, err := stores.APIKeys.DeleteMany(ctx, []string{aliceKey.ID, bobKey.ID}, bob.ID)
	if err != nil {
		t.Fatalf("DeleteMany(bob) error = %v", err)
	}
	if len(deleted) != 1 || deleted[0] != bobKey.ID {
		t.Fatalf("deleted = %v, want [%s]", deleted, bobKey.ID)
	}
	if len(missing) != 1 || missing[0] != aliceKey.ID {
		t.Fatalf("missing = %v, want [%s]", missing, aliceKey.ID)
	}
	if key, err := stores.APIKeys.Get(ctx, aliceKey.ID); err != nil || key == nil {
		t.Fatalf("Get(alice key) = %+v, %v; want it to survive bob's batch", key, err)
	}
	if key, err := stores.APIKeys.Get(ctx, bobKey.ID); err != nil || key != nil {
		t.Fatalf("Get(bob key) = %+v, %v; want nil", key, err)
	}

	// The trusted local surface deletes the remaining key.
	deleted, missing, err = stores.APIKeys.DeleteMany(ctx, []string{aliceKey.ID}, "")
	if err != nil {
		t.Fatalf("DeleteMany(local) error = %v", err)
	}
	if len(deleted) != 1 || deleted[0] != aliceKey.ID || len(missing) != 0 {
		t.Fatalf("local batch = %v / %v", deleted, missing)
	}
}
