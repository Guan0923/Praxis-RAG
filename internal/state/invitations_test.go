package state

import (
	"context"
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
