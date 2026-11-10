package state

// Per-account API-key ownership coverage: which account issued a credential, how
// the owner projection is returned, and what happens when the owning account is
// deleted. The tests run against the shared MySQL test schema through
// newTestStores, so they fail on the pre-ownership schema (there is no
// owner_user_id column, no ListByOwner/CreateForOwner, and deleting a user keeps
// its credentials alive).

import (
	"context"
	"database/sql"
	"testing"
)

// TestAPIKeyOwnerCreateListGet covers CreateForOwner/ListByOwner/Get and the
// owner projection, plus the legacy unowned Create path.
func TestAPIKeyOwnerCreateListGet(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	alice, err := stores.Users.Create(ctx, "alice-owner", "x")
	if err != nil {
		t.Fatalf("Users.Create(alice) error = %v", err)
	}
	bob, err := stores.Users.Create(ctx, "bob-owner", "x")
	if err != nil {
		t.Fatalf("Users.Create(bob) error = %v", err)
	}

	aliceKey, err := stores.APIKeys.CreateForOwner(ctx, "alice-key", []string{"alpha"}, alice.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(alice) error = %v", err)
	}
	if aliceKey.OwnerUserID != alice.ID {
		t.Fatalf("created owner_user_id = %q, want %q", aliceKey.OwnerUserID, alice.ID)
	}
	if aliceKey.ID == "" || aliceKey.Key == "" {
		t.Fatalf("CreateForOwner returned an incomplete credential: %+v", aliceKey)
	}

	bobKey, err := stores.APIKeys.CreateForOwner(ctx, "bob-key", []string{"beta"}, bob.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(bob) error = %v", err)
	}
	if bobKey.ID == aliceKey.ID {
		t.Fatal("two CreateForOwner calls returned the same key id")
	}

	// ListByOwner only returns the account's own credentials, enriched with the
	// display-only owner username.
	aliceKeys, err := stores.APIKeys.ListByOwner(ctx, alice.ID)
	if err != nil {
		t.Fatalf("ListByOwner(alice) error = %v", err)
	}
	if len(aliceKeys) != 1 || aliceKeys[0].ID != aliceKey.ID {
		t.Fatalf("ListByOwner(alice) = %+v, want only %q", aliceKeys, aliceKey.ID)
	}
	if aliceKeys[0].OwnerUserID != alice.ID || aliceKeys[0].OwnerUsername != alice.Username {
		t.Fatalf("ListByOwner(alice) owner projection = %+v", aliceKeys[0])
	}
	if len(aliceKeys[0].Namespaces) != 1 || aliceKeys[0].Namespaces[0] != "alpha" {
		t.Fatalf("ListByOwner(alice) namespaces = %v", aliceKeys[0].Namespaces)
	}

	bobKeys, err := stores.APIKeys.ListByOwner(ctx, bob.ID)
	if err != nil {
		t.Fatalf("ListByOwner(bob) error = %v", err)
	}
	if len(bobKeys) != 1 || bobKeys[0].ID != bobKey.ID || bobKeys[0].OwnerUsername != bob.Username {
		t.Fatalf("ListByOwner(bob) = %+v, want only %q", bobKeys, bobKey.ID)
	}

	// Get returns the same owner projection.
	got, err := stores.APIKeys.Get(ctx, aliceKey.ID)
	if err != nil {
		t.Fatalf("Get(alice) error = %v", err)
	}
	if got == nil {
		t.Fatal("Get(alice) = nil, want the created key")
	}
	if got.OwnerUserID != alice.ID || got.OwnerUsername != alice.Username || !got.Exportable {
		t.Fatalf("Get(alice) = %+v", got)
	}
	missing, err := stores.APIKeys.Get(ctx, "does-not-exist")
	if err != nil || missing != nil {
		t.Fatalf("Get(missing) = %+v, %v; want nil, nil", missing, err)
	}

	// The legacy Create path keeps issuing unowned credentials (NULL owner) and
	// List still returns every credential, owned or not.
	legacy, err := stores.APIKeys.Create(ctx, "legacy-key", []string{"shared"})
	if err != nil {
		t.Fatalf("Create(legacy) error = %v", err)
	}
	if legacy.OwnerUserID != "" || legacy.OwnerUsername != "" {
		t.Fatalf("legacy Create owner = %q/%q, want empty", legacy.OwnerUserID, legacy.OwnerUsername)
	}
	var legacyOwner sql.NullString
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT owner_user_id FROM api_keys WHERE id = ?", legacy.ID).Scan(&legacyOwner); err != nil {
		t.Fatalf("reading the legacy owner column: %v", err)
	}
	if legacyOwner.Valid {
		t.Fatalf("legacy api_keys row owner_user_id = %q, want NULL", legacyOwner.String)
	}
	var aliceOwner string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT owner_user_id FROM api_keys WHERE id = ?", aliceKey.ID).Scan(&aliceOwner); err != nil {
		t.Fatalf("reading the owned owner column: %v", err)
	}
	if aliceOwner != alice.ID {
		t.Fatalf("stored owner_user_id = %q, want %q", aliceOwner, alice.ID)
	}

	all, err := stores.APIKeys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("List() returned %d keys, want 3: %+v", len(all), all)
	}
	aliceKeys, err = stores.APIKeys.ListByOwner(ctx, alice.ID)
	if err != nil || len(aliceKeys) != 1 || aliceKeys[0].ID != aliceKey.ID {
		t.Fatalf("ListByOwner(alice) after legacy create = %+v, %v", aliceKeys, err)
	}
}

// TestAPIKeyOwnerUpdateAndDelete verifies that metadata updates keep the owner
// and that Delete removes the credential entirely.
func TestAPIKeyOwnerUpdateAndDelete(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	carol, err := stores.Users.Create(ctx, "carol-owner", "x")
	if err != nil {
		t.Fatalf("Users.Create(carol) error = %v", err)
	}
	created, err := stores.APIKeys.CreateForOwner(ctx, "carol-key", []string{"alpha"}, carol.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(carol) error = %v", err)
	}

	updated, err := stores.APIKeys.Update(ctx, created.ID, "carol-renamed", []string{"alpha", "beta"}, false)
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if updated == nil {
		t.Fatal("Update() = nil, want the updated key")
	}
	if updated.Name != "carol-renamed" || updated.Enabled {
		t.Fatalf("Update() = %+v", updated)
	}
	if updated.OwnerUserID != carol.ID || updated.OwnerUsername != carol.Username {
		t.Fatalf("Update() dropped the owner projection: %+v", updated)
	}

	got, err := stores.APIKeys.Get(ctx, created.ID)
	if err != nil || got == nil {
		t.Fatalf("Get() after Update = %+v, %v", got, err)
	}
	if got.OwnerUserID != carol.ID || got.OwnerUsername != carol.Username {
		t.Fatalf("Get() after Update owner = %+v", got)
	}
	if len(got.Namespaces) != 2 || got.Namespaces[0] != "alpha" || got.Namespaces[1] != "beta" {
		t.Fatalf("Get() after Update namespaces = %v", got.Namespaces)
	}

	deleted, err := stores.APIKeys.Delete(ctx, created.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v; want true, nil", deleted, err)
	}
	gone, err := stores.APIKeys.Get(ctx, created.ID)
	if err != nil || gone != nil {
		t.Fatalf("Get() after Delete = %+v, %v; want nil, nil", gone, err)
	}
	remaining, err := stores.APIKeys.ListByOwner(ctx, carol.ID)
	if err != nil || len(remaining) != 0 {
		t.Fatalf("ListByOwner() after Delete = %+v, %v; want empty", remaining, err)
	}
}

// TestUserDeleteCascadesAPIKeys pins the account-deletion cascade: the deleted
// account's credentials disappear with it, while legacy unowned credentials and
// other accounts' credentials survive.
func TestUserDeleteCascadesAPIKeys(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	alice, err := stores.Users.Create(ctx, "alice-cascade", "x")
	if err != nil {
		t.Fatalf("Users.Create(alice) error = %v", err)
	}
	bob, err := stores.Users.Create(ctx, "bob-cascade", "x")
	if err != nil {
		t.Fatalf("Users.Create(bob) error = %v", err)
	}
	aliceKey, err := stores.APIKeys.CreateForOwner(ctx, "alice-key", []string{"alpha"}, alice.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(alice) error = %v", err)
	}
	bobKey, err := stores.APIKeys.CreateForOwner(ctx, "bob-key", []string{"beta"}, bob.ID)
	if err != nil {
		t.Fatalf("CreateForOwner(bob) error = %v", err)
	}
	legacy, err := stores.APIKeys.Create(ctx, "legacy-key", []string{"shared"})
	if err != nil {
		t.Fatalf("Create(legacy) error = %v", err)
	}

	deleted, err := stores.Users.Delete(ctx, alice.ID)
	if err != nil {
		t.Fatalf("Users.Delete(alice) error = %v", err)
	}
	if !deleted {
		t.Fatal("Users.Delete(alice) = false, want true")
	}

	if key, err := stores.APIKeys.Get(ctx, aliceKey.ID); err != nil || key != nil {
		t.Fatalf("deleted account's key = %+v, %v; want nil, nil", key, err)
	}
	if keys, err := stores.APIKeys.ListByOwner(ctx, alice.ID); err != nil || len(keys) != 0 {
		t.Fatalf("ListByOwner(deleted) = %+v, %v; want empty", keys, err)
	}
	var ownedRows int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM api_keys WHERE owner_user_id = ?", alice.ID).Scan(&ownedRows); err != nil {
		t.Fatalf("counting the deleted account's keys: %v", err)
	}
	if ownedRows != 0 {
		t.Fatalf("api_keys rows for the deleted account = %d, want 0", ownedRows)
	}

	// Other accounts and legacy unowned credentials are untouched.
	survivors, err := stores.APIKeys.List(ctx)
	if err != nil {
		t.Fatalf("List() after cascade error = %v", err)
	}
	if len(survivors) != 2 {
		t.Fatalf("List() after cascade returned %d keys, want 2: %+v", len(survivors), survivors)
	}
	bobStored, err := stores.APIKeys.Get(ctx, bobKey.ID)
	if err != nil || bobStored == nil || bobStored.OwnerUserID != bob.ID {
		t.Fatalf("bob's key after cascade = %+v, %v", bobStored, err)
	}
	legacyStored, err := stores.APIKeys.Get(ctx, legacy.ID)
	if err != nil || legacyStored == nil || legacyStored.OwnerUserID != "" {
		t.Fatalf("legacy key after cascade = %+v, %v", legacyStored, err)
	}
}
