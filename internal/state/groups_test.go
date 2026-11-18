package state

import (
	"context"
	"testing"
	"time"
)

// newGroupStateFixture prepares users and one group for repository tests.
func newGroupStateFixture(t *testing.T) (*Stores, context.Context, string, User) {
	t.Helper()
	stores := newTestStores(t)
	ctx := context.Background()
	owner, err := stores.Users.Create(ctx, "g-owner", "hash")
	if err != nil {
		t.Fatalf("Create(g-owner) error = %v", err)
	}
	updated, err := stores.Users.Update(ctx, owner.ID, RoleVIP, true)
	if err != nil || updated == nil {
		t.Fatalf("Update(g-owner, vip) error = %v", err)
	}
	owner = *updated
	group, err := stores.UserGroups.Create(ctx, UserGroup{
		Name:        "组",
		OwnerUserID: owner.ID,
	})
	if err != nil {
		t.Fatalf("UserGroups.Create error = %v", err)
	}
	return stores, ctx, group.ID, owner
}

// groupInvitationRow inserts one invitation directly and returns its token.
func groupInvitationRow(t *testing.T, stores *Stores, ctx context.Context, groupID, inviterID, inviteeID string) (string, GroupInvitation) {
	t.Helper()
	return groupInvitationRowUnique(t, stores, ctx, groupID, inviterID, inviteeID)
}

// groupInvitationRowUnique inserts one invitation with a fresh token so two
// rows for the same invitee never collide on the digest unique key.
func groupInvitationRowUnique(t *testing.T, stores *Stores, ctx context.Context, groupID, inviterID, inviteeID string) (string, GroupInvitation) {
	t.Helper()
	token := "tok-" + inviteeID + "-" + randomTestHex(t, 8)
	invitation, err := stores.GroupInvitations.Create(ctx, GroupInvitation{
		GroupID:       groupID,
		InviterUserID: inviterID,
		InviteeUserID: inviteeID,
		TokenDigest:   tokenDigestHex(token),
		ExpiresAt:     nowISOPlus(time.Hour),
	})
	if err != nil {
		t.Fatalf("GroupInvitations.Create error = %v", err)
	}
	return token, invitation
}

// randomTestHex generates unique test material.
func randomTestHex(t *testing.T, size int) string {
	t.Helper()
	value, err := randomHex(size)
	if err != nil {
		t.Fatalf("randomHex error = %v", err)
	}
	return value
}

// tokenDigestHex computes the digest the repositories expect for a token.
func tokenDigestHex(token string) string {
	return sha256Hex(token)
}

// TestUserGroupRepositoryRoundTrip pins the group, member and grant rows.
func TestUserGroupRepositoryRoundTrip(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)

	// A member joins through the invitation acceptance.
	member, err := stores.Users.Create(ctx, "g-member", "hash")
	if err != nil {
		t.Fatalf("Create(g-member) error = %v", err)
	}
	token, invitation := groupInvitationRow(t, stores, ctx, groupID, owner.ID, member.ID)
	final, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != nil {
		t.Fatalf("AcceptPending error = %v", err)
	}
	if final == nil || final.Status != GroupInvitationAccepted {
		t.Fatalf("accepted = %+v", final)
	}
	present, err := stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || !present {
		t.Fatalf("member exists = %v err = %v", present, err)
	}

	// The token still resolves the invitation.
	byToken, err := stores.GroupInvitations.ByTokenDigest(ctx, tokenDigestHex(token))
	if err != nil || byToken == nil || byToken.Status != GroupInvitationAccepted {
		t.Fatalf("ByTokenDigest = %+v err = %v", byToken, err)
	}

	// Members and joined groups list correctly.
	members, err := stores.UserGroupMembers.ListByGroup(ctx, groupID)
	if err != nil || len(members) != 1 || members[0].UserID != member.ID {
		t.Fatalf("ListByGroup = %+v err = %v", members, err)
	}
	joined, err := stores.UserGroupMembers.ListJoinedByUser(ctx, member.ID)
	if err != nil || len(joined) != 1 || joined[0].ID != groupID {
		t.Fatalf("ListJoinedByUser = %+v err = %v", joined, err)
	}
	ids, err := stores.UserGroupMembers.ActiveGroupIDsForUser(ctx, member.ID)
	if err != nil || len(ids) != 1 || ids[0] != groupID {
		t.Fatalf("ActiveGroupIDsForUser = %v err = %v", ids, err)
	}

	// Grants persist and round-trip.
	if err := stores.GroupPermissions.Upsert(ctx, KBGroupPermission{
		Namespace: "kb1", GroupID: groupID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("GroupPermissions.Upsert error = %v", err)
	}
	grants, err := stores.GroupPermissions.ListForGroups(ctx, "kb1", []string{groupID})
	if err != nil || len(grants) != 1 || grants[0].AllowMask != PermissionBitRead {
		t.Fatalf("ListForGroups = %+v err = %v", grants, err)
	}
	namespaces, err := stores.GroupPermissions.NamespacesForGroups(ctx, []string{groupID})
	if err != nil || len(namespaces) != 1 || namespaces[0] != "kb1" {
		t.Fatalf("NamespacesForGroups = %v err = %v", namespaces, err)
	}

	// Dissolve clears members, pending invitations and grants.
	_, err = stores.UserGroups.Dissolve(ctx, groupID)
	if err != nil {
		t.Fatalf("Dissolve error = %v", err)
	}
	present, err = stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || present {
		t.Fatalf("member after dissolve = %v err = %v", present, err)
	}
	grants, err = stores.GroupPermissions.ListForGroups(ctx, "kb1", []string{groupID})
	if err != nil || len(grants) != 0 {
		t.Fatalf("grants after dissolve = %+v err = %v", grants, err)
	}
	group, err := stores.UserGroups.ByID(ctx, groupID)
	if err != nil || group == nil || group.Status != UserGroupDissolved || group.DissolvedAt == nil {
		t.Fatalf("group after dissolve = %+v err = %v", group, err)
	}

	// A dissolved group's memberships no longer count.
	ids, err = stores.UserGroupMembers.ActiveGroupIDsForUser(ctx, member.ID)
	if err != nil || len(ids) != 0 {
		t.Fatalf("active ids after dissolve = %v err = %v", ids, err)
	}
}

// TestGroupInvitationAcceptVersusRevokeConcurrency pins acceptance rule 5:
// between a concurrent revoke and accept only one state transition wins; the
// loser sees a typed conflict and no membership row.
func TestGroupInvitationAcceptVersusRevokeConcurrency(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)
	member, err := stores.Users.Create(ctx, "g-racer", "hash")
	if err != nil {
		t.Fatalf("Create(g-racer) error = %v", err)
	}
	_, invitation := groupInvitationRow(t, stores, ctx, groupID, owner.ID, member.ID)

	// A pending→revoked transition first: the accept must fail and must not
	// write the membership. The row comes back for reference with its final
	// status.
	revoked, err := stores.GroupInvitations.RevokeByID(ctx, invitation.ID)
	if err != nil || !revoked {
		t.Fatalf("RevokeByID = %v err = %v", revoked, err)
	}
	final, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != ErrGroupInvitationGone {
		t.Fatalf("accept after revoke err = %v, want ErrGroupInvitationGone", err)
	}
	if final == nil || final.Status != GroupInvitationRevoked {
		t.Fatalf("accept after revoke row = %+v", final)
	}
	present, err := stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || present {
		t.Fatalf("member after losing accept = %v err = %v", present, err)
	}
}

// TestGroupInvitationDeclineVersusAcceptConcurrency pins the symmetric race:
// a decline that lands first blocks the accept.
func TestGroupInvitationDeclineVersusAcceptConcurrency(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)
	member, err := stores.Users.Create(ctx, "g-decliner", "hash")
	if err != nil {
		t.Fatalf("Create(g-decliner) error = %v", err)
	}
	_, invitation := groupInvitationRow(t, stores, ctx, groupID, owner.ID, member.ID)

	declined, err := stores.GroupInvitations.DeclinePending(ctx, invitation.ID)
	if err != nil || !declined {
		t.Fatalf("DeclinePending = %v err = %v", declined, err)
	}
	final, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != ErrGroupInvitationGone {
		t.Fatalf("accept after decline err = %v, want ErrGroupInvitationGone", err)
	}
	if final == nil || final.Status != GroupInvitationDeclined {
		t.Fatalf("accept after decline row = %+v", final)
	}
	present, err := stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || present {
		t.Fatalf("member after declined accept = %v err = %v", present, err)
	}
}

// TestGroupInvitationAcceptTwiceIsIdempotentWithoutRejoin pins the
// idempotency contract: a second accept returns the accepted row without
// touching members, and after leaving the group the retry still does not
// re-add the member.
func TestGroupInvitationAcceptTwiceIsIdempotentWithoutRejoin(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)
	member, err := stores.Users.Create(ctx, "g-retry", "hash")
	if err != nil {
		t.Fatalf("Create(g-retry) error = %v", err)
	}
	_, invitation := groupInvitationRow(t, stores, ctx, groupID, owner.ID, member.ID)

	if _, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO()); err != nil {
		t.Fatalf("first accept error = %v", err)
	}
	second, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != nil || second == nil || second.Status != GroupInvitationAccepted {
		t.Fatalf("second accept = %+v err = %v", second, err)
	}

	// The member leaves; the retry must not re-add them.
	if _, err := stores.UserGroupMembers.Remove(ctx, groupID, member.ID); err != nil {
		t.Fatalf("Remove error = %v", err)
	}
	third, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != nil || third == nil || third.Status != GroupInvitationAccepted {
		t.Fatalf("third accept = %+v err = %v", third, err)
	}
	present, err := stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || present {
		t.Fatalf("member re-added = %v err = %v", present, err)
	}
}

// TestGroupInvitationAcceptRejectsExpiredLink pins the expiry check inside
// the acceptance transaction.
func TestGroupInvitationAcceptRejectsExpiredLink(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)
	member, err := stores.Users.Create(ctx, "g-late", "hash")
	if err != nil {
		t.Fatalf("Create(g-late) error = %v", err)
	}
	token := "tok-late"
	invitation, err := stores.GroupInvitations.Create(ctx, GroupInvitation{
		GroupID:       groupID,
		InviterUserID: owner.ID,
		InviteeUserID: member.ID,
		TokenDigest:   tokenDigestHex(token),
		ExpiresAt:     nowISOPlus(-time.Minute),
	})
	if err != nil {
		t.Fatalf("GroupInvitations.Create error = %v", err)
	}
	final, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != ErrGroupInvitationExpired {
		t.Fatalf("expired accept err = %v, want ErrGroupInvitationExpired", err)
	}
	if final == nil || final.Status != GroupInvitationPending {
		t.Fatalf("expired accept row = %+v", final)
	}
	present, err := stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || present {
		t.Fatalf("member after expired accept = %v err = %v", present, err)
	}
	// The expired link is now revoked, so a later retry also fails.
	after, err := stores.GroupInvitations.ByID(ctx, invitation.ID)
	if err != nil || after == nil || after.Status != GroupInvitationRevoked {
		t.Fatalf("invitation after expiry flip = %+v err = %v", after, err)
	}
}

// TestGroupInvitationAcceptChecksGroupAndInviter pins the in-transaction
// guards: a dissolved group or a demoted inviter refuses the acceptance.
func TestGroupInvitationAcceptChecksGroupAndInviter(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)

	// Demoted inviter.
	member, err := stores.Users.Create(ctx, "g-demoted-target", "hash")
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	_, err = stores.Users.Update(ctx, owner.ID, RoleUser, true)
	if err != nil {
		t.Fatalf("Update(owner, user) error = %v", err)
	}
	_, invitation := groupInvitationRow(t, stores, ctx, groupID, owner.ID, member.ID)
	final, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO())
	if err != ErrGroupInviterUnqualified {
		t.Fatalf("demoted inviter accept err = %v, want ErrGroupInviterUnqualified", err)
	}
	if final == nil || final.Status != GroupInvitationPending {
		t.Fatalf("demoted inviter row = %+v", final)
	}

	// Dissolved group.
	if _, err := stores.UserGroups.Dissolve(ctx, groupID); err != nil {
		t.Fatalf("Dissolve error = %v", err)
	}
	_, invitation2 := groupInvitationRowUnique(t, stores, ctx, groupID, owner.ID, member.ID)
	final2, err := stores.GroupInvitations.AcceptPending(ctx, invitation2.ID, nowISO(), nowISO())
	if err != ErrGroupDissolved {
		t.Fatalf("dissolved group accept err = %v, want ErrGroupDissolved", err)
	}
	if final2 == nil || final2.Status != GroupInvitationPending {
		t.Fatalf("dissolved group row = %+v", final2)
	}
}

// TestUserDeleteCascadesGroups pins the account-deletion cleanup: owned
// groups vanish with members, invitations and grants; own memberships and
// invitations disappear too.
func TestUserDeleteCascadesGroups(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)

	member, err := stores.Users.Create(ctx, "g-gone", "hash")
	if err != nil {
		t.Fatalf("Create(g-gone) error = %v", err)
	}
	_, invitation := groupInvitationRow(t, stores, ctx, groupID, owner.ID, member.ID)
	if _, err := stores.GroupInvitations.AcceptPending(ctx, invitation.ID, nowISO(), nowISO()); err != nil {
		t.Fatalf("AcceptPending error = %v", err)
	}
	if err := stores.GroupPermissions.Upsert(ctx, KBGroupPermission{
		Namespace: "kb1", GroupID: groupID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("GroupPermissions.Upsert error = %v", err)
	}
	// The member also owns another group with a grant.
	other, err := stores.UserGroups.Create(ctx, UserGroup{Name: "另一组", OwnerUserID: member.ID})
	if err != nil {
		t.Fatalf("UserGroups.Create error = %v", err)
	}
	if err := stores.GroupPermissions.Upsert(ctx, KBGroupPermission{
		Namespace: "kb1", GroupID: other.ID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("GroupPermissions.Upsert(2) error = %v", err)
	}

	if deleted, err := stores.Users.Delete(ctx, member.ID); err != nil || !deleted {
		t.Fatalf("Users.Delete = %v err = %v", deleted, err)
	}

	// The member's own group is gone with its grant.
	if group, err := stores.UserGroups.ByID(ctx, other.ID); err != nil || group != nil {
		t.Fatalf("member's group after delete = %+v err = %v", group, err)
	}
	grants, err := stores.GroupPermissions.ListByNamespace(ctx, "kb1")
	if err != nil {
		t.Fatalf("ListByNamespace error = %v", err)
	}
	for _, grant := range grants {
		if grant.GroupID == other.ID {
			t.Fatalf("member's group grant survived: %+v", grant)
		}
	}
	// Its membership in the owner's group is gone too.
	present, err := stores.UserGroupMembers.Exists(ctx, groupID, member.ID)
	if err != nil || present {
		t.Fatalf("membership after delete = %v err = %v", present, err)
	}
	// The owner's group survived untouched.
	if group, err := stores.UserGroups.ByID(ctx, groupID); err != nil || group == nil {
		t.Fatalf("owner's group after member delete = %+v err = %v", group, err)
	}
}

// TestKBPurgeCleansGroupPermissions pins the namespace-purge cleanup: a
// purged knowledge base loses its group grants.
func TestKBPurgeCleansGroupPermissions(t *testing.T) {
	stores, ctx, groupID, owner := newGroupStateFixture(t)
	if err := stores.KnowledgeBases.CreateWithOwner(ctx,
		map[string]any{"namespace": "g-purge-kb", "collection": "g_purge_collection"}, owner.ID); err != nil {
		t.Fatalf("CreateWithOwner error = %v", err)
	}
	if err := stores.GroupPermissions.Upsert(ctx, KBGroupPermission{
		Namespace: "g-purge-kb", GroupID: groupID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("GroupPermissions.Upsert error = %v", err)
	}
	report, err := stores.KnowledgeBases.Purge(ctx, []string{"g-purge-kb"})
	if err != nil {
		t.Fatalf("Purge error = %v", err)
	}
	_ = report
	grants, err := stores.GroupPermissions.ListByNamespace(ctx, "g-purge-kb")
	if err != nil || len(grants) != 0 {
		t.Fatalf("group grants after purge = %+v err = %v", grants, err)
	}
}

// nowISOPlus renders a timestamp relative to now.
func nowISOPlus(delta time.Duration) string {
	return isoTimeFor(time.Now().UTC().Add(delta))
}

// isoTimeFor renders the same format nowISO uses.
func isoTimeFor(moment time.Time) string {
	return moment.UTC().Format("2006-01-02T15:04:05.000000+00:00")
}
