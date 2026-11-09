package state

import (
	"context"
	"testing"
)

// insertKnowledgeBase creates one knowledge-base row owned by ownerUserID.
func insertKnowledgeBase(t *testing.T, stores *Stores, namespace, ownerUserID string) {
	t.Helper()
	if err := stores.KnowledgeBases.CreateWithOwner(context.Background(),
		map[string]any{"namespace": namespace, "collection": namespace + "_collection"}, ownerUserID); err != nil {
		t.Fatalf("CreateWithOwner(%q) error = %v", namespace, err)
	}
}

func TestPermissionRepositoryLifecycle(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	insertKnowledgeBase(t, stores, "kb1", "owner-1")

	grant := Permission{
		Namespace: "kb1", GranteeUserID: "user-1", DocumentName: "a.md",
		Permission: PermissionRead, GrantedBy: "owner-1",
	}
	if err := stores.Permissions.Upsert(ctx, grant); err != nil {
		t.Fatalf("Upsert error = %v", err)
	}
	whole := grant
	whole.DocumentName = ""
	whole.Permission = PermissionWrite
	if err := stores.Permissions.Upsert(ctx, whole); err != nil {
		t.Fatalf("Upsert(whole) error = %v", err)
	}

	rows, err := stores.Permissions.ListFor(ctx, "kb1", "user-1")
	if err != nil {
		t.Fatalf("ListFor error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("ListFor returned %d rows, want 2", len(rows))
	}
	byDocument := map[string]Permission{}
	for _, row := range rows {
		byDocument[row.DocumentName] = row
		if row.ID == "" || row.CreatedAt == "" || row.UpdatedAt == "" {
			t.Fatalf("row is missing generated fields: %+v", row)
		}
	}
	if byDocument["a.md"].Permission != PermissionRead {
		t.Fatalf("a.md permission = %q", byDocument["a.md"].Permission)
	}
	if byDocument[""].Permission != PermissionWrite {
		t.Fatalf("whole permission = %q", byDocument[""].Permission)
	}

	// Re-granting the same (namespace, grantee, document) updates in place.
	upgraded := grant
	upgraded.Permission = PermissionWrite
	if err := stores.Permissions.Upsert(ctx, upgraded); err != nil {
		t.Fatalf("Upsert(upgrade) error = %v", err)
	}
	rows, err = stores.Permissions.ListFor(ctx, "kb1", "user-1")
	if err != nil {
		t.Fatalf("ListFor(upgraded) error = %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("after upgrade ListFor returned %d rows, want 2", len(rows))
	}
	for _, row := range rows {
		if row.DocumentName == "a.md" && row.Permission != PermissionWrite {
			t.Fatalf("a.md permission after upgrade = %q", row.Permission)
		}
	}

	byGrantee, err := stores.Permissions.ListByGrantee(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListByGrantee error = %v", err)
	}
	if len(byGrantee) != 2 {
		t.Fatalf("ListByGrantee returned %d rows, want 2", len(byGrantee))
	}

	// A second grantee in the same knowledge base is independent.
	other := Permission{Namespace: "kb1", GranteeUserID: "user-2", DocumentName: "b.md",
		Permission: PermissionRead, GrantedBy: "owner-1"}
	if err := stores.Permissions.UpsertMany(ctx, []Permission{other}); err != nil {
		t.Fatalf("UpsertMany error = %v", err)
	}
	byNamespace, err := stores.Permissions.ListByNamespace(ctx, "kb1")
	if err != nil {
		t.Fatalf("ListByNamespace error = %v", err)
	}
	if len(byNamespace) != 3 {
		t.Fatalf("ListByNamespace returned %d rows, want 3", len(byNamespace))
	}

	removed, err := stores.Permissions.DeleteDocument(ctx, "kb1", "user-1", "a.md")
	if err != nil || !removed {
		t.Fatalf("DeleteDocument = %v, %v", removed, err)
	}
	removed, err = stores.Permissions.DeleteDocument(ctx, "kb1", "user-1", "a.md")
	if err != nil || removed {
		t.Fatalf("second DeleteDocument = %v, %v; want false", removed, err)
	}

	if _, err := stores.Permissions.Delete(ctx, "kb1", "user-1"); err != nil {
		t.Fatalf("Delete error = %v", err)
	}
	if _, err := stores.Permissions.DeleteByGrantee(ctx, "user-2"); err != nil {
		t.Fatalf("DeleteByGrantee error = %v", err)
	}
	byNamespace, err = stores.Permissions.ListByNamespace(ctx, "kb1")
	if err != nil {
		t.Fatalf("ListByNamespace(after deletes) error = %v", err)
	}
	if len(byNamespace) != 0 {
		t.Fatalf("ListByNamespace after deletes returned %d rows, want 0", len(byNamespace))
	}
}

func TestKnowledgeBaseOwnershipAndSharingPurge(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	insertKnowledgeBase(t, stores, "kb-owned", "owner-1")
	insertKnowledgeBase(t, stores, "kb-legacy", "")

	owner, err := stores.KnowledgeBases.OwnerOf(ctx, "kb-owned")
	if err != nil || owner != "owner-1" {
		t.Fatalf("OwnerOf(kb-owned) = %q, %v", owner, err)
	}
	owner, err = stores.KnowledgeBases.OwnerOf(ctx, "kb-legacy")
	if err != nil || owner != "" {
		t.Fatalf("OwnerOf(kb-legacy) = %q, %v; want empty", owner, err)
	}
	exists, err := stores.KnowledgeBases.Exists(ctx, "kb-missing")
	if err != nil || exists {
		t.Fatalf("Exists(kb-missing) = %v, %v", exists, err)
	}
	exists, err = stores.KnowledgeBases.Exists(ctx, "kb-owned")
	if err != nil || !exists {
		t.Fatalf("Exists(kb-owned) = %v, %v", exists, err)
	}

	owned, err := stores.KnowledgeBases.OwnedNamespaces(ctx, "owner-1")
	if err != nil || len(owned) != 1 || owned[0] != "kb-owned" {
		t.Fatalf("OwnedNamespaces = %v, %v", owned, err)
	}
	namespaces, err := stores.KnowledgeBases.Namespaces(ctx)
	if err != nil || len(namespaces) != 2 {
		t.Fatalf("Namespaces = %v, %v", namespaces, err)
	}

	if changed, err := stores.KnowledgeBases.SetOwner(ctx, "kb-legacy", "owner-2"); err != nil || !changed {
		t.Fatalf("SetOwner = %v, %v", changed, err)
	}
	if owner, _ := stores.KnowledgeBases.OwnerOf(ctx, "kb-legacy"); owner != "owner-2" {
		t.Fatalf("OwnerOf after SetOwner = %q", owner)
	}

	// Deleting one knowledge base drops its sharing state.
	if err := stores.Permissions.Upsert(ctx, Permission{Namespace: "kb-owned", GranteeUserID: "u1",
		DocumentName: "", Permission: PermissionRead, GrantedBy: "owner-1"}); err != nil {
		t.Fatalf("Upsert error = %v", err)
	}
	if _, err := stores.Invitations.Create(ctx, Invitation{Namespace: "kb-owned", InviterUserID: "owner-1",
		InviteeUserID: "u1", Permission: PermissionRead, Scope: InvitationScopeAll,
		TokenDigest: "digest-kb-owned", Status: InvitationPending, ExpiresAt: nowISO()}); err != nil {
		t.Fatalf("Create invitation error = %v", err)
	}
	deleted, err := stores.KnowledgeBases.Delete(ctx, "kb-owned")
	if err != nil || !deleted {
		t.Fatalf("Delete knowledge base = %v, %v", deleted, err)
	}
	rows, err := stores.Permissions.ListByNamespace(ctx, "kb-owned")
	if err != nil || len(rows) != 0 {
		t.Fatalf("permissions after KB delete = %v, %v", rows, err)
	}
	invitations, err := stores.Invitations.ListByNamespace(ctx, "kb-owned", 10)
	if err != nil || len(invitations) != 0 {
		t.Fatalf("invitations after KB delete = %v, %v", invitations, err)
	}
}

func TestUserRolesAndDeleteCascade(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	admin, err := stores.Users.Create(ctx, "alice", "hash")
	if err != nil {
		t.Fatalf("Create(alice) error = %v", err)
	}
	if !admin.IsAdmin() || admin.IsVIP() || !admin.CanShare() {
		t.Fatalf("first user roles = %+v", admin)
	}
	member, err := stores.Users.Create(ctx, "bob", "hash")
	if err != nil {
		t.Fatalf("Create(bob) error = %v", err)
	}
	if member.IsAdmin() || member.IsVIP() || member.CanShare() {
		t.Fatalf("second user roles = %+v", member)
	}
	updated, err := stores.Users.Update(ctx, member.ID, RoleVIP, true)
	if err != nil || updated == nil || !updated.IsVIP() || !updated.CanShare() {
		t.Fatalf("Update(vip) = %+v, %v", updated, err)
	}
	if !IsRole(RoleAdmin) || !IsRole(RoleVIP) || !IsRole(RoleUser) || IsRole("owner") {
		t.Fatal("IsRole does not match the three supported roles")
	}

	// Deleting an account removes its grants and invitations but keeps issued
	// grants for other users.
	insertKnowledgeBase(t, stores, "kb1", admin.ID)
	if err := stores.Permissions.Upsert(ctx, Permission{Namespace: "kb1", GranteeUserID: member.ID,
		DocumentName: "", Permission: PermissionRead, GrantedBy: admin.ID}); err != nil {
		t.Fatalf("Upsert member grant error = %v", err)
	}
	if err := stores.Permissions.Upsert(ctx, Permission{Namespace: "kb1", GranteeUserID: "other",
		DocumentName: "a.md", Permission: PermissionRead, GrantedBy: admin.ID}); err != nil {
		t.Fatalf("Upsert other grant error = %v", err)
	}
	if _, err := stores.Invitations.Create(ctx, Invitation{Namespace: "kb1", InviterUserID: admin.ID,
		InviteeUserID: member.ID, Permission: PermissionRead, Scope: InvitationScopeFiles,
		DocumentNames: []string{"a.md"}, TokenDigest: "digest-member",
		Status: InvitationPending, ExpiresAt: nowISO()}); err != nil {
		t.Fatalf("Create invitation error = %v", err)
	}

	deleted, err := stores.Users.Delete(ctx, member.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete(member) = %v, %v", deleted, err)
	}
	if rows, _ := stores.Permissions.ListByNamespace(ctx, "kb1"); len(rows) != 1 {
		t.Fatalf("permissions after user delete = %v", rows)
	}
	if invitations, _ := stores.Invitations.ListByInvitee(ctx, member.ID, 10); len(invitations) != 0 {
		t.Fatalf("invitations after user delete = %v", invitations)
	}

	// Deleting an owner leaves the knowledge base unowned instead of deleting it.
	if err := stores.Permissions.Upsert(ctx, Permission{Namespace: "kb1", GranteeUserID: "other",
		DocumentName: "", Permission: PermissionRead, GrantedBy: admin.ID}); err != nil {
		t.Fatalf("Upsert owner grant error = %v", err)
	}
	if _, err := stores.Users.Delete(ctx, admin.ID); err != nil {
		t.Fatalf("Delete(admin) error = %v", err)
	}
	owner, err := stores.KnowledgeBases.OwnerOf(ctx, "kb1")
	if err != nil || owner != "" {
		t.Fatalf("OwnerOf after owner delete = %q, %v; want empty", owner, err)
	}
}
