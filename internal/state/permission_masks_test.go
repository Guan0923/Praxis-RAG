package state

import (
	"context"
	"testing"
)

// TestPermissionMaskMigrationBackfill covers acceptance scenario 5: historical
// rows are backfilled with the documented masks and re-running the migration
// never overwrites a mask the user deliberately cleared.
func TestPermissionMaskMigrationBackfill(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	// Simulate a pre-mask database: the rows were inserted without masks, so
	// the migration left the columns NULL and derived the masks from the
	// permission column.
	// A fresh schema creates NOT NULL DEFAULT 0 mask columns; simulate a
	// pre-mask database by making them nullable so the backfill's NULL marker
	// is reachable, exactly like an upgraded production database.
	if _, err := stores.DB.ExecContext(ctx, "ALTER TABLE kb_permissions MODIFY allow_mask INT UNSIGNED NULL, MODIFY deny_mask INT UNSIGNED NULL"); err != nil {
		t.Fatalf("make nullable: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx, "ALTER TABLE kb_invitations MODIFY allow_mask INT UNSIGNED NULL"); err != nil {
		t.Fatalf("make invitation nullable: %v", err)
	}

	seed := func() {
		exec := stores.DB.ExecContext
		if _, err := exec(ctx, "DELETE FROM kb_permissions WHERE namespace = 'kb-mask'"); err != nil {
			t.Fatalf("clean: %v", err)
		}
		if _, err := exec(ctx,
			"INSERT INTO kb_permissions (id, namespace, grantee_user_id, document_name, permission, granted_by, created_at, updated_at) "+
				"VALUES ('p1', 'kb-mask', 'u1', '', 'write', 'admin', '2026-01-01T00:00:00', '2026-01-01T00:00:00'), "+
				"('p2', 'kb-mask', 'u2', 'a.md', 'read', 'admin', '2026-01-01T00:00:00', '2026-01-01T00:00:00'), "+
				"('p3', 'kb-mask', 'u3', 'b.md', 'write', 'admin', '2026-01-01T00:00:00', '2026-01-01T00:00:00')"); err != nil {
			t.Fatalf("seed: %v", err)
		}
		if _, err := exec(ctx, "UPDATE kb_permissions SET allow_mask = NULL, deny_mask = NULL WHERE namespace = 'kb-mask'"); err != nil {
			t.Fatalf("null masks: %v", err)
		}
		if _, err := exec(ctx,
			"INSERT INTO kb_invitations (id, namespace, inviter_user_id, invitee_user_id, permission, scope, document_names, token_digest, status, created_at, expires_at, accepted_at, updated_at) "+
				"VALUES ('i1', 'kb-mask', 'inv', 'u4', 'write', 'all', '[]', 'tok1', 'pending', '2026-01-01T00:00:00', '2027-01-01T00:00:00', NULL, '2026-01-01T00:00:00')"); err != nil {
			t.Fatalf("seed invitation: %v", err)
		}
		if _, err := exec(ctx, "UPDATE kb_invitations SET allow_mask = NULL WHERE id = 'i1'"); err != nil {
			t.Fatalf("null invitation mask: %v", err)
		}
	}

	seed()
	if err := stores.migratePermissionMasks(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	grants, err := stores.Permissions.ListByNamespace(ctx, "kb-mask")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, grant := range grants {
		var wantAllow, wantDeny uint16
		switch {
		case grant.DocumentName == "" && grant.Permission == PermissionWrite:
			wantAllow = 15
		case grant.DocumentName != "" && grant.Permission == PermissionWrite:
			wantAllow = 13
		case grant.Permission == PermissionRead:
			wantAllow = 1
		}
		if grant.AllowMask != wantAllow || grant.DenyMask != wantDeny {
			t.Fatalf("grant %s masks = %d/%d, want %d/%d",
				grant.DocumentName, grant.AllowMask, grant.DenyMask, wantAllow, wantDeny)
		}
	}
	invitation, err := stores.Invitations.ByID(ctx, "i1")
	if err != nil || invitation == nil {
		t.Fatalf("invitation read: %v %+v", err, invitation)
	}
	if invitation.InvitationAllowMask() != 15 {
		t.Fatalf("invitation allow mask = %d, want 15", invitation.InvitationAllowMask())
	}

	// The user now deliberately clears a mask to zero; re-running the
	// migration must not overwrite it back.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE kb_permissions SET allow_mask = 0 WHERE id = 'p2'"); err != nil {
		t.Fatalf("clear mask: %v", err)
	}
	if err := stores.migratePermissionMasks(ctx); err != nil {
		t.Fatalf("re-migrate: %v", err)
	}
	grants, err = stores.Permissions.ListByNamespace(ctx, "kb-mask")
	if err != nil {
		t.Fatalf("re-list: %v", err)
	}
	for _, grant := range grants {
		if grant.ID == "p2" && grant.AllowMask != 0 {
			t.Fatalf("re-migration overwrote the cleared mask: %d", grant.AllowMask)
		}
	}
}

// TestPermissionAcceptPendingPreservesDeny pins the acceptance rule: the
// accepted invitation overwrites the allow of the same scope but never lifts
// a stored deny.
func TestPermissionAcceptPendingPreservesDeny(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	// A manager's explicit deny exists first.
	if err := stores.Permissions.Upsert(ctx, Permission{
		Namespace: "kb-deny", GranteeUserID: "u1", DocumentName: "a.md",
		AllowMask: 0, DenyMask: 8, GrantedBy: "admin",
	}); err != nil {
		t.Fatalf("deny upsert: %v", err)
	}
	// The invitation carries an allow that would grant the denied operation.
	if _, err := stores.Invitations.Create(ctx, Invitation{
		Namespace: "kb-deny", InviterUserID: "inv", InviteeUserID: "u1",
		Permission: "write", AllowMask: 13, Scope: InvitationScopeFiles,
		DocumentNames: []string{"a.md"}, TokenDigest: "tok-accept",
		Status:        InvitationPending, ExpiresAt: "2999-01-01T00:00:00",
	}); err != nil {
		t.Fatalf("invitation: %v", err)
	}
	invitation, err := stores.Invitations.ByTokenDigest(ctx, "tok-accept")
	if err != nil || invitation == nil {
		t.Fatalf("by token: %v", invitation)
	}
	grants := []Permission{{
		Namespace: "kb-deny", GranteeUserID: "u1", DocumentName: "a.md",
		Permission: "write", AllowMask: 13, GrantedBy: "inv",
	}}
	final, err := stores.Invitations.AcceptPending(ctx, invitation.ID, "2026-01-02T00:00:00", grants)
	if err != nil || final == nil || final.Status != InvitationAccepted {
		t.Fatalf("AcceptPending = %+v, %v", final, err)
	}
	rows, err := stores.Permissions.ListFor(ctx, "kb-deny", "u1")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(rows))
	}
	if rows[0].AllowMask != 13 {
		t.Fatalf("allow = %d, want 13", rows[0].AllowMask)
	}
	if rows[0].DenyMask != 8 {
		t.Fatalf("deny = %d, want 8 (accept must preserve the deny)", rows[0].DenyMask)
	}
}

// TestInvitationStatusOnlyFromPending pins the guarded transition: declining,
// revoking or expiring only converts a pending row, and the reported change is
// checked.
func TestInvitationStatusOnlyFromPending(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if _, err := stores.Invitations.Create(ctx, Invitation{
		Namespace: "kb-status", InviterUserID: "inv", InviteeUserID: "u1",
		Permission: "read", AllowMask: 1, Scope: InvitationScopeAll,
		DocumentNames: []string{}, TokenDigest: "tok-status",
		Status:        InvitationPending, ExpiresAt: "2999-01-01T00:00:00",
	}); err != nil {
		t.Fatalf("create: %v", err)
	}
	invitation, err := stores.Invitations.ByTokenDigest(ctx, "tok-status")
	if err != nil || invitation == nil {
		t.Fatalf("by token: %v", err)
	}
	// Decline wins while pending.
	changed, err := stores.Invitations.SetStatus(ctx, invitation.ID, InvitationDeclined, "")
	if err != nil || !changed {
		t.Fatalf("decline pending = %v, %v", changed, err)
	}
	// A second transition from declined must not fire.
	changed, err = stores.Invitations.SetStatus(ctx, invitation.ID, InvitationRevoked, "")
	if err != nil {
		t.Fatalf("second SetStatus error: %v", err)
	}
	if changed {
		t.Fatal("a declined invitation must not transition to revoked")
	}
	final, err := stores.Invitations.ByID(ctx, invitation.ID)
	if err != nil || final == nil || final.Status != InvitationDeclined {
		t.Fatalf("final status = %+v, %v", final, err)
	}
}

// TestPermissionClearOperations pins the explicit revoke-allow / clear-deny
// operations: each zeroes only its own side and an emptied rule disappears.
func TestPermissionClearOperations(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Permissions.Upsert(ctx, Permission{
		Namespace: "kb-clear", GranteeUserID: "u1", DocumentName: "a.md",
		AllowMask: 5, DenyMask: 8, GrantedBy: "admin",
	}); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	// Clearing the deny keeps the allow.
	existed, err := stores.Permissions.ClearDeny(ctx, "kb-clear", "u1", "a.md")
	if err != nil || !existed {
		t.Fatalf("ClearDeny = %v, %v", existed, err)
	}
	rows, _ := stores.Permissions.ListFor(ctx, "kb-clear", "u1")
	if len(rows) != 1 || rows[0].AllowMask != 5 || rows[0].DenyMask != 0 {
		t.Fatalf("after clear-deny: %+v", rows)
	}
	// Revoking the allow zeroes it; the rule then carries only the (cleared)
	// deny side and disappears because neither mask remains. Re-grant first so
	// the deny survives the revoke on its own.
	if err := stores.Permissions.Upsert(ctx, Permission{
		Namespace: "kb-clear", GranteeUserID: "u2", DocumentName: "b.md",
		AllowMask: 5, DenyMask: 8, GrantedBy: "admin",
	}); err != nil {
		t.Fatalf("re-grant: %v", err)
	}
	removed, err := stores.Permissions.ClearAllow(ctx, "kb-clear", "u2", "b.md")
	if err != nil {
		t.Fatalf("ClearAllow: %v", err)
	}
	if removed != 1 {
		t.Fatalf("ClearAllow removed = %d, want 1", removed)
	}
	rows, err = stores.Permissions.ListFor(ctx, "kb-clear", "u2")
	if err != nil || len(rows) != 1 || rows[0].AllowMask != 0 || rows[0].DenyMask != 8 {
		t.Fatalf("after clear-allow the deny rule must survive: %+v, %v", rows, err)
	}
}

// TestPermissionLegacyUpsertCompat keeps the historical Upsert surface
// working: a Permission-level payload derives its mask, and a mask-only
// payload refreshes the legacy projection.
func TestPermissionLegacyUpsertCompat(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Permissions.Upsert(ctx, Permission{
		Namespace: "kb-legacy", GranteeUserID: "u1", DocumentName: "",
		Permission: "write", GrantedBy: "admin",
	}); err != nil {
		t.Fatalf("legacy upsert: %v", err)
	}
	rows, err := stores.Permissions.ListFor(ctx, "kb-legacy", "u1")
	if err != nil || len(rows) != 1 {
		t.Fatalf("legacy rows: %+v, %v", rows, err)
	}
	if rows[0].AllowMask != 15 || rows[0].Permission != "write" {
		t.Fatalf("legacy whole write = %d/%q", rows[0].AllowMask, rows[0].Permission)
	}
	if err := stores.Permissions.Upsert(ctx, Permission{
		Namespace: "kb-legacy", GranteeUserID: "u2", DocumentName: "f.md",
		Permission: "read", GrantedBy: "admin",
	}); err != nil {
		t.Fatalf("legacy read: %v", err)
	}
	rows, err = stores.Permissions.ListFor(ctx, "kb-legacy", "u2")
	if err != nil || len(rows) != 1 || rows[0].AllowMask != 1 {
		t.Fatalf("legacy read rows: %+v, %v", rows, err)
	}
}
