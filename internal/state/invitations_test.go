package state

import (
	"context"
	"strings"
	"testing"
)

func TestInvitationRepositoryLifecycle(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	insertKnowledgeBase(t, stores, "kb1", "owner-1")

	created, err := stores.Invitations.Create(ctx, Invitation{
		Namespace:     "kb1",
		InviterUserID: "owner-1",
		InviteeUserID: "user-1",
		Permission:    PermissionWrite,
		Scope:         InvitationScopeFiles,
		DocumentNames: []string{"a.md", "b.pdf"},
		TokenDigest:   "digest-1",
		Status:        InvitationPending,
		ExpiresAt:     "2099-01-01T00:00:00.000000+00:00",
	})
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	if created.ID == "" || created.CreatedAt == "" || created.UpdatedAt == "" {
		t.Fatalf("created invitation is missing generated fields: %+v", created)
	}

	found, err := stores.Invitations.ByID(ctx, created.ID)
	if err != nil || found == nil {
		t.Fatalf("ByID = %+v, %v", found, err)
	}
	if len(found.DocumentNames) != 2 || found.DocumentNames[0] != "a.md" || found.DocumentNames[1] != "b.pdf" {
		t.Fatalf("document_names round trip = %v", found.DocumentNames)
	}
	if found.Permission != PermissionWrite || found.Scope != InvitationScopeFiles {
		t.Fatalf("permission/scope = %q/%q", found.Permission, found.Scope)
	}

	byDigest, err := stores.Invitations.ByTokenDigest(ctx, "digest-1")
	if err != nil || byDigest == nil || byDigest.ID != created.ID {
		t.Fatalf("ByTokenDigest = %+v, %v", byDigest, err)
	}
	missing, err := stores.Invitations.ByTokenDigest(ctx, "unknown")
	if err != nil || missing != nil {
		t.Fatalf("ByTokenDigest(unknown) = %+v, %v", missing, err)
	}

	// A second, older pending invitation; listing puts pending rows first and
	// newest first inside the same status.
	second, err := stores.Invitations.Create(ctx, Invitation{
		Namespace: "kb2", InviterUserID: "owner-1", InviteeUserID: "user-1",
		Permission: PermissionRead, Scope: InvitationScopeAll, TokenDigest: "digest-2",
		Status: InvitationPending, ExpiresAt: "2099-01-01T00:00:00.000000+00:00",
	})
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	inbox, err := stores.Invitations.ListByInvitee(ctx, "user-1", 10)
	if err != nil || len(inbox) != 2 {
		t.Fatalf("ListByInvitee = %v, %v", inbox, err)
	}

	if changed, err := stores.Invitations.SetStatus(ctx, created.ID, InvitationAccepted,
		"2026-11-09T00:00:00.000000+00:00"); err != nil || !changed {
		t.Fatalf("SetStatus = %v, %v", changed, err)
	}
	accepted, err := stores.Invitations.ByID(ctx, created.ID)
	if err != nil || accepted == nil {
		t.Fatalf("ByID(accepted) = %+v, %v", accepted, err)
	}
	if accepted.Status != InvitationAccepted || accepted.AcceptedAt == nil {
		t.Fatalf("accepted status = %+v", accepted)
	}

	// RevokePending only touches pending rows of the addressed invitee.
	revoked, err := stores.Invitations.RevokePending(ctx, "kb2", "user-1")
	if err != nil || revoked != 1 {
		t.Fatalf("RevokePending = %d, %v", revoked, err)
	}
	reloaded, err := stores.Invitations.ByID(ctx, second.ID)
	if err != nil || reloaded == nil || reloaded.Status != InvitationRevoked {
		t.Fatalf("second invitation after revoke = %+v, %v", reloaded, err)
	}

	byNamespace, err := stores.Invitations.ListByNamespace(ctx, "kb1", 10)
	if err != nil || len(byNamespace) != 1 {
		t.Fatalf("ListByNamespace = %v, %v", byNamespace, err)
	}

	if removed, err := stores.Invitations.DeleteByUser(ctx, "user-1"); err != nil || removed != 2 {
		t.Fatalf("DeleteByUser = %d, %v", removed, err)
	}
}

func TestInvitationUniqueTokenDigest(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	first := Invitation{Namespace: "kb1", InviterUserID: "o", InviteeUserID: "u",
		Permission: PermissionRead, Scope: InvitationScopeAll, TokenDigest: "same-digest",
		Status: InvitationPending, ExpiresAt: nowISO()}
	if _, err := stores.Invitations.Create(ctx, first); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if _, err := stores.Invitations.Create(ctx, first); err == nil {
		t.Fatal("Create with a duplicate token digest must fail")
	} else if _, ok := err.(*StateConflict); !ok {
		t.Fatalf("duplicate digest error = %T, want *StateConflict", err)
	}
}

// TestInvitationAcceptPendingMergesGrantAndStatus covers the single-transaction
// accept: the grants and the accepted status always land together, so a
// non-pending invitation keeps its grants and a failed grant write rolls the
// status flip back.
func TestInvitationAcceptPendingMergesGrantAndStatus(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	insertKnowledgeBase(t, stores, "kb1", "owner-1")

	create := func(digest string) Invitation {
		t.Helper()
		created, err := stores.Invitations.Create(ctx, Invitation{
			Namespace:     "kb1",
			InviterUserID: "owner-1",
			InviteeUserID: "user-1",
			Permission:    PermissionWrite,
			Scope:         InvitationScopeFiles,
			DocumentNames: []string{"a.md", "b.pdf"},
			TokenDigest:   digest,
			Status:        InvitationPending,
			ExpiresAt:     "2099-01-01T00:00:00.000000+00:00",
		})
		if err != nil {
			t.Fatalf("Create(%s) error = %v", digest, err)
		}
		return created
	}
	grants := []Permission{
		{Namespace: "kb1", GranteeUserID: "user-1", DocumentName: "a.md", Permission: PermissionWrite, GrantedBy: "owner-1"},
		{Namespace: "kb1", GranteeUserID: "user-1", DocumentName: "b.pdf", Permission: PermissionWrite, GrantedBy: "owner-1"},
	}

	// A declined invitation never receives grants: the update matches only a
	// pending row.
	declined := create("digest-declined")
	if _, err := stores.Invitations.SetStatus(ctx, declined.ID, InvitationDeclined, ""); err != nil {
		t.Fatalf("SetStatus(declined) error = %v", err)
	}
	final, err := stores.Invitations.AcceptPending(ctx, declined.ID, nowISO(), grants)
	if err != nil || final == nil || final.Status != InvitationDeclined {
		t.Fatalf("AcceptPending(declined) = %+v, %v", final, err)
	}
	if rows, err := stores.Permissions.ListFor(ctx, "kb1", "user-1"); err != nil || len(rows) != 0 {
		t.Fatalf("grants after declined accept = %v, %v", rows, err)
	}

	// The happy path commits the grants together with the accepted status.
	pending := create("digest-pending")
	final, err = stores.Invitations.AcceptPending(ctx, pending.ID, nowISO(), grants)
	if err != nil || final == nil || final.Status != InvitationAccepted || final.AcceptedAt == nil {
		t.Fatalf("AcceptPending(pending) = %+v, %v", final, err)
	}
	if rows, err := stores.Permissions.ListFor(ctx, "kb1", "user-1"); err != nil || len(rows) != 2 {
		t.Fatalf("grants after accept = %v, %v", rows, err)
	}

	// A second accept is idempotent: the pending match fails, nothing is
	// written and the row still reports accepted.
	final, err = stores.Invitations.AcceptPending(ctx, pending.ID, nowISO(), grants)
	if err != nil || final == nil || final.Status != InvitationAccepted {
		t.Fatalf("second AcceptPending = %+v, %v", final, err)
	}
	if rows, err := stores.Permissions.ListFor(ctx, "kb1", "user-1"); err != nil || len(rows) != 2 {
		t.Fatalf("grants after second accept = %v, %v", rows, err)
	}

	// A grant write that cannot be stored rolls the status flip back with the
	// earlier grants of the same transaction: an oversized document_name
	// violates the column length, so nothing of this accept persists.
	rollback := create("digest-rollback")
	broken := []Permission{
		{Namespace: "kb1", GranteeUserID: "user-2", DocumentName: "c.md", Permission: PermissionWrite, GrantedBy: "owner-1"},
		{Namespace: "kb1", GranteeUserID: "user-2", DocumentName: strings.Repeat("x", 201), Permission: PermissionWrite, GrantedBy: "owner-1"},
	}
	if _, err := stores.Invitations.AcceptPending(ctx, rollback.ID, nowISO(), broken); err == nil {
		t.Fatal("AcceptPending with an oversized document name must fail")
	}
	reloaded, err := stores.Invitations.ByID(ctx, rollback.ID)
	if err != nil || reloaded == nil || reloaded.Status != InvitationPending {
		t.Fatalf("invitation after failed accept = %+v, %v", reloaded, err)
	}
	if rows, err := stores.Permissions.ListFor(ctx, "kb1", "user-2"); err != nil || len(rows) != 0 {
		t.Fatalf("grants after failed accept = %v, %v", rows, err)
	}
}
