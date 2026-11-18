package access

import (
	"context"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// newGroupFixture builds one sharing scenario plus a group-owner (vip), a
// plain member and a stranger account.
func newGroupFixture(t *testing.T) *fixture {
	t.Helper()
	base := newFixture(t)
	return base
}

// createGroup creates one group directly through the service.
func createGroup(t *testing.T, f *fixture, user state.User, name string) GroupView {
	t.Helper()
	view, err := f.service.CreateGroup(context.Background(), user, CreateGroupRequest{Name: name})
	if err != nil {
		t.Fatalf("CreateGroup(%q by %s) error = %v", name, user.Username, err)
	}
	return view
}

// inviteToGroup issues one invitation and returns the raw link token.
func inviteToGroup(t *testing.T, f *fixture, owner state.User, groupID, username string) string {
	t.Helper()
	_, token, err := f.service.CreateGroupInvitation(context.Background(), owner, CreateGroupInvitationRequest{
		GroupID: groupID, Username: username,
	})
	if err != nil {
		t.Fatalf("CreateGroupInvitation(%s, %s) error = %v", groupID, username, err)
	}
	return token
}

// acceptGroupInvite accepts one membership link for the invitee.
func acceptGroupInvite(t *testing.T, f *fixture, invitee state.User, token string) GroupInvitationView {
	t.Helper()
	view, err := f.service.AcceptGroupInvitation(context.Background(), invitee, token)
	if err != nil {
		t.Fatalf("AcceptGroupInvitation error = %v", err)
	}
	return view
}

// TestGroupCreateRoleRules pins the role contract: enabled vip/admin may create
// groups, plain users and disabled accounts may not.
func TestGroupCreateRoleRules(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()

	if _, err := f.service.CreateGroup(ctx, f.user0(t), CreateGroupRequest{Name: "nope"}); err == nil {
		t.Fatal("user creating group must fail")
	} else if kind := errorKind(err); kind != KindForbidden {
		t.Fatalf("user create error kind = %v, want forbidden", kind)
	}

	if _, err := f.service.CreateGroup(ctx, f.vip, CreateGroupRequest{Name: "vip-group"}); err != nil {
		t.Fatalf("vip create error = %v", err)
	}
	if _, err := f.service.CreateGroup(ctx, f.admin, CreateGroupRequest{Name: "admin-group"}); err != nil {
		t.Fatalf("admin create error = %v", err)
	}

	disabled := f.disabledUser(t)
	if _, err := f.service.CreateGroup(ctx, disabled, CreateGroupRequest{Name: "bad"}); err == nil {
		t.Fatal("disabled account creating group must fail")
	}
}

// user0 returns the plain-user account of the fixture.
func (f *fixture) user0(t *testing.T) state.User {
	t.Helper()
	return f.member
}

// disabledUser returns one disabled vip account.
func (f *fixture) disabledUser(t *testing.T) state.User {
	t.Helper()
	ctx := context.Background()
	disabled, err := f.stores.Users.Create(ctx, "gina", "hash")
	if err != nil {
		t.Fatalf("Create(gina) error = %v", err)
	}
	updated, err := f.stores.Users.Update(ctx, disabled.ID, state.RoleVIP, false)
	if err != nil || updated == nil {
		t.Fatalf("Update(gina, disabled) error = %v", err)
	}
	return *updated
}

// errorKind unwraps the typed failure kind.
func errorKind(err error) ErrorKind {
	var accessError *Error
	if asError, ok := err.(*Error); ok {
		accessError = asError
	}
	if accessError == nil {
		return -1
	}
	return accessError.Kind
}

// TestGroupInvitationFlow pins the invitation lifecycle: only the invitee may
// accept, the inviter must keep the role, and acceptance lands a membership.
func TestGroupInvitationFlow(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "研究组")

	token := inviteToGroup(t, f, f.owner, group.ID, "dave")

	// The invitee preview works; other accounts cannot even see the link.
	if _, err := f.service.PreviewGroupInvitation(ctx, f.outsider, token); err == nil {
		t.Fatal("outsider previewing someone else's invitation must fail")
	}
	if _, err := f.service.PreviewGroupInvitation(ctx, f.member, token); err != nil {
		t.Fatalf("invitee preview error = %v", err)
	}

	accepted := acceptGroupInvite(t, f, f.member, token)
	if accepted.Status != state.GroupInvitationAccepted {
		t.Fatalf("accepted status = %q", accepted.Status)
	}

	// The membership landed.
	present, err := f.stores.UserGroupMembers.Exists(ctx, group.ID, f.member.ID)
	if err != nil || !present {
		t.Fatalf("member exists = %v err = %v", present, err)
	}

	// A member may now see the group detail; the stranger may not.
	if _, _, _, err := f.service.GroupDetail(ctx, f.member, group.ID); err != nil {
		t.Fatalf("member GroupDetail error = %v", err)
	}
	if _, _, _, err := f.service.GroupDetail(ctx, f.outsider, group.ID); err == nil {
		t.Fatal("outsider GroupDetail must fail")
	}

	// A demoted inviter cannot invite anyone else.
	demoted, err := f.stores.Users.Update(ctx, f.owner.ID, state.RoleUser, true)
	if err != nil || demoted == nil {
		t.Fatalf("Update(owner, user) error = %v", err)
	}
	if _, _, err := f.service.CreateGroupInvitation(ctx, *demoted, CreateGroupInvitationRequest{
		GroupID: group.ID, Username: "erin",
	}); err == nil {
		t.Fatal("demoted inviter must not issue invitations")
	}
}

// TestGroupInvitationAcceptRechecksInviterRole pins acceptance rule 2: after
// the inviter is demoted to user, a pending invitation no longer adds members,
// while the already-landed membership and grant stay effective.
func TestGroupInvitationAcceptRechecksInviterRole(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "降级组")

	// Existing member joined before the demotion.
	oldToken := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, oldToken)
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("SetGroupGrant error = %v", err)
	}

	// Demote the owner, then invite erin through the repository directly
	// (the service would refuse the new invitation outright).
	demoted, err := f.stores.Users.Update(ctx, f.owner.ID, state.RoleUser, true)
	if err != nil || demoted == nil {
		t.Fatalf("Update(owner, user) error = %v", err)
	}
	invitation, token, err := newGroupInvitationToken()
	if err != nil {
		t.Fatalf("newGroupInvitationToken error = %v", err)
	}
	if _, err := f.stores.GroupInvitations.Create(ctx, state.GroupInvitation{
		GroupID:       group.ID,
		InviterUserID: f.owner.ID,
		InviteeUserID: f.outsider.ID,
		TokenDigest:   token,
		Status:        state.GroupInvitationPending,
		ExpiresAt:     isoTime(f.clock.now.UTC().Add(time.Hour)),
	}); err != nil {
		t.Fatalf("GroupInvitations.Create error = %v", err)
	}

	if _, err := f.service.AcceptGroupInvitation(ctx, f.outsider, invitation); err == nil {
		t.Fatal("demoted inviter's pending invitation must not add members")
	}
	present, err := f.stores.UserGroupMembers.Exists(ctx, group.ID, f.outsider.ID)
	if err != nil || present {
		t.Fatalf("outsider joined = %v err = %v", present, err)
	}

	// The existing membership and grant keep working.
	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet error = %v", err)
	}
	if !scope.AllowsAnyRead() {
		t.Fatal("existing member lost the group grant after inviter demotion")
	}
}

// TestGroupPermissionMerge pins the merge arithmetic: allow bits union across
// the personal grant and every applicable group rule, and (with the first
// version being allow-only) a member keeps its personal rights untouched.
func TestGroupPermissionMerge(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "行业研究组")

	token := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, token)

	// Whole-library group grant: read+update (0110).
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: PermissionBitRead | PermissionBitUpdate,
	}); err != nil {
		t.Fatalf("SetGroupGrant(0110) error = %v", err)
	}

	// Personal grant: update only (0100).
	if err := f.stores.Permissions.Upsert(ctx, state.Permission{
		Namespace: "kb1", GranteeUserID: f.member.ID, DocumentName: "",
		Permission: state.PermissionWrite,
	}); err != nil {
		t.Fatalf("personal Upsert error = %v", err)
	}

	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet error = %v", err)
	}
	mask := scope.DocumentAllowMask("")
	if mask != PermissionBitRead|PermissionBitCreate|PermissionBitUpdate|PermissionBitDelete {
		t.Fatalf("merged mask = %04b, want all bits (personal write implies full)", mask)
	}

	// Now drop the personal grant; the group 0110 alone must remain.
	if _, err := f.stores.Permissions.Delete(ctx, "kb1", f.member.ID); err != nil {
		t.Fatalf("personal Delete error = %v", err)
	}
	scope, err = f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet(2) error = %v", err)
	}
	mask = scope.DocumentAllowMask("")
	if want := PermissionBitRead | PermissionBitUpdate; mask != want {
		t.Fatalf("group-only mask = %04b, want %04b", mask, want)
	}
}

// TestGroupMultipleGroupsUnion pins acceptance rule 3 (multiple groups): the
// group contribution is the union of each group's bits. The kb owner owns both
// groups, per the ownership rules.
func TestGroupMultipleGroupsUnion(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	readGroup := createGroup(t, f, f.owner, "阅读组")
	writeGroup := createGroup(t, f, f.owner, "修改组")

	tokenA := inviteToGroup(t, f, f.owner, readGroup.ID, "dave")
	acceptGroupInvite(t, f, f.member, tokenA)
	tokenB := inviteToGroup(t, f, f.owner, writeGroup.ID, "dave")
	acceptGroupInvite(t, f, f.member, tokenB)

	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: readGroup.ID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("SetGroupGrant(readGroup) error = %v", err)
	}
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: writeGroup.ID, AllowMask: PermissionBitUpdate,
	}); err != nil {
		t.Fatalf("SetGroupGrant(writeGroup) error = %v", err)
	}

	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet error = %v", err)
	}
	mask := scope.DocumentAllowMask("")
	if want := PermissionBitRead | PermissionBitUpdate; mask != want {
		t.Fatalf("union mask = %04b, want %04b", mask, want)
	}
}

// TestGroupFileLevelGrant pins per-file group grants: only the named document
// gains the bits, and create on a file-level grant is refused.
func TestGroupFileLevelGrant(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "文件组")
	if err := f.stores.Documents.Upsert(ctx, "kb1", state.DocumentRecord{
		Name: "spec.md", DocumentID: "doc-spec",
	}); err != nil {
		t.Fatalf("Documents.Upsert error = %v", err)
	}

	// create bit on a file-level grant is refused.
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, DocumentNames: []string{"spec.md"},
		AllowMask: PermissionBitCreate,
	}); err == nil {
		t.Fatal("create bit on a file-level group grant must be refused")
	}

	token := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, token)

	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, DocumentNames: []string{"spec.md"},
		AllowMask: PermissionBitRead | PermissionBitUpdate,
	}); err != nil {
		t.Fatalf("SetGroupGrant(file) error = %v", err)
	}

	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet error = %v", err)
	}
	if !scope.CanReadDocument("spec.md") || !scope.CanUpdateDocument("spec.md") {
		t.Fatalf("spec.md scope = %+v", scope)
	}
	if scope.CanReadDocument("other.md") || scope.CanUpdateDocument("other.md") {
		t.Fatalf("other.md leaked through the file-level grant: %+v", scope)
	}
	if scope.WritesWhole() {
		t.Fatal("file-level grant must not grant whole-library write")
	}
}

// TestGroupMembershipRemovalKillsAccess pins acceptance rule 4: leaving,
// being removed, revoking the grant and dissolving the group each removes the
// group contribution on the next request, while other sources stay.
func TestGroupMembershipRemovalKillsAccess(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	groupA := createGroup(t, f, f.owner, "组A")
	groupB := createGroup(t, f, f.owner, "组B")

	tokenA := inviteToGroup(t, f, f.owner, groupA.ID, "dave")
	acceptGroupInvite(t, f, f.member, tokenA)
	tokenB := inviteToGroup(t, f, f.owner, groupB.ID, "dave")
	acceptGroupInvite(t, f, f.member, tokenB)

	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: groupA.ID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("SetGroupGrant(A) error = %v", err)
	}
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: groupB.ID, AllowMask: PermissionBitUpdate,
	}); err != nil {
		t.Fatalf("SetGroupGrant(B) error = %v", err)
	}
	// A personal grant keeps read alive across every group change below.
	if err := f.stores.Permissions.Upsert(ctx, state.Permission{
		Namespace: "kb1", GranteeUserID: f.member.ID, DocumentName: "",
		Permission: state.PermissionRead,
	}); err != nil {
		t.Fatalf("personal Upsert error = %v", err)
	}

	// 1. Remove the member from group A: read via A disappears, update via B
	// stays, personal read stays.
	if err := f.service.RemoveGroupMember(ctx, f.owner, groupA.ID, f.member.ID); err != nil {
		t.Fatalf("RemoveGroupMember error = %v", err)
	}
	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet(after remove) error = %v", err)
	}
	if mask := scope.DocumentAllowMask(""); mask != PermissionBitRead|PermissionBitUpdate {
		t.Fatalf("mask after remove = %04b, want read|update", mask)
	}

	// 2. Revoke group B's grant: update disappears, personal read stays.
	if err := f.service.RevokeGroupGrant(ctx, f.owner, "kb1", groupB.ID); err != nil {
		t.Fatalf("RevokeGroupGrant error = %v", err)
	}
	scope, err = f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet(after revoke) error = %v", err)
	}
	if mask := scope.DocumentAllowMask(""); mask != PermissionBitRead {
		t.Fatalf("mask after revoke = %04b, want read only (personal)", mask)
	}

	// 3. Rejoin B, then dissolve B: the whole contribution disappears.
	tokenB2 := inviteToGroup(t, f, f.owner, groupB.ID, "dave")
	acceptGroupInvite(t, f, f.member, tokenB2)
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: groupB.ID, AllowMask: PermissionBitUpdate,
	}); err != nil {
		t.Fatalf("SetGroupGrant(B2) error = %v", err)
	}
	if err := f.service.DissolveGroup(ctx, f.owner, groupB.ID); err != nil {
		t.Fatalf("DissolveGroup error = %v", err)
	}
	scope, err = f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet(after dissolve) error = %v", err)
	}
	if mask := scope.DocumentAllowMask(""); mask != PermissionBitRead {
		t.Fatalf("mask after dissolve = %04b, want read only (personal)", mask)
	}
	// A dissolved group cannot be joined anymore.
	tokenB3, err := func() (string, error) {
		_, token, err := f.service.CreateGroupInvitation(ctx, f.owner, CreateGroupInvitationRequest{
			GroupID: groupB.ID, Username: "dave",
		})
		return token, err
	}()
	if err == nil {
		_ = tokenB3
		t.Fatal("inviting into a dissolved group must fail")
	}
}

// TestGroupInvitationIdempotentRetry pins acceptance rule 5: retrying an old
// accepted link stays idempotent (no error) but never re-adds an account that
// left or was removed.
func TestGroupInvitationIdempotentRetry(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "重试组")

	token := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, token)

	// Leave the group.
	if err := f.service.LeaveGroup(ctx, f.member, group.ID); err != nil {
		t.Fatalf("LeaveGroup error = %v", err)
	}

	// Retrying the old accepted link must not re-add the member; the retry
	// stays idempotent (an accepted row returns its state without writes).
	retry, err := f.service.AcceptGroupInvitation(ctx, f.member, token)
	if err != nil {
		t.Fatalf("retry error = %v (idempotent retries must succeed without writing)", err)
	}
	if retry.Status != state.GroupInvitationAccepted {
		t.Fatalf("retry status = %q, want accepted (idempotent)", retry.Status)
	}
	present, err := f.stores.UserGroupMembers.Exists(ctx, group.ID, f.member.ID)
	if err != nil {
		t.Fatalf("Exists error = %v", err)
	}
	if present {
		t.Fatal("retrying an old accepted link re-added a member that left")
	}

	// The same contract holds for a removed member.
	token2 := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, token2)
	if err := f.service.RemoveGroupMember(ctx, f.owner, group.ID, f.member.ID); err != nil {
		t.Fatalf("RemoveGroupMember error = %v", err)
	}
	if _, err := f.service.AcceptGroupInvitation(ctx, f.member, token2); err != nil {
		t.Fatalf("retry(2) error = %v", err)
	}
	present, err = f.stores.UserGroupMembers.Exists(ctx, group.ID, f.member.ID)
	if err != nil {
		t.Fatalf("Exists(2) error = %v", err)
	}
	if present {
		t.Fatal("retrying an old link re-added a removed member")
	}
}

// TestGroupGrantOwnership pins acceptance rule 6: a group owner may only
// grant on knowledge bases they own; a stranger cannot manage someone else's
// group or grant.
func TestGroupGrantOwnership(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "归属组")

	// The owner's group may not grant on another account's knowledge base.
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "admin-kb", GroupID: group.ID, AllowMask: PermissionBitRead,
	}); err == nil {
		t.Fatal("granting a group on another owner's knowledge base must fail")
	}
	// An administrator owns every knowledge base.
	if err := f.service.SetGroupGrant(ctx, f.admin, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: PermissionBitRead,
	}); err == nil {
		t.Fatal("an administrator who does not own the group must not grant for it")
	}
	// A stranger cannot manage the group.
	if err := f.service.DissolveGroup(ctx, f.outsider, group.ID); err == nil {
		t.Fatal("a stranger dissolving someone else's group must fail")
	}
	// The owner's own knowledge base works.
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("SetGroupGrant(own kb) error = %v", err)
	}
}

// TestGroupVisibleNamespaces pins the namespace visibility merge: a group
// grant exposes the knowledge base to its current members only.
func TestGroupVisibleNamespaces(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "可见组")

	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: PermissionBitRead,
	}); err != nil {
		t.Fatalf("SetGroupGrant error = %v", err)
	}

	// A group grant does not expose the namespace before joining; only the
	// grant plus a current membership does.
	visible, err := f.service.VisibleNamespaces(ctx, f.member)
	if err != nil {
		t.Fatalf("VisibleNamespaces error = %v", err)
	}
	if contains(visible, "kb1") {
		t.Fatalf("kb1 visible before joining: %v", visible)
	}

	token := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, token)
	visible, err = f.service.VisibleNamespaces(ctx, f.member)
	if err != nil {
		t.Fatalf("VisibleNamespaces(2) error = %v", err)
	}
	if !contains(visible, "kb1") {
		t.Fatalf("kb1 not visible after joining: %v", visible)
	}

	if err := f.service.DissolveGroup(ctx, f.owner, group.ID); err != nil {
		t.Fatalf("DissolveGroup error = %v", err)
	}
	visible, err = f.service.VisibleNamespaces(ctx, f.member)
	if err != nil {
		t.Fatalf("VisibleNamespaces(3) error = %v", err)
	}
	if contains(visible, "kb1") {
		t.Fatalf("kb1 still visible after dissolve: %v", visible)
	}
}

// TestGroupPermissionSources pins the source explanation: role, personal and
// named-group origins are all reported with their bits.
func TestGroupPermissionSources(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "行业研究组")

	token := inviteToGroup(t, f, f.owner, group.ID, "dave")
	acceptGroupInvite(t, f, f.member, token)
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: PermissionBitRead | PermissionBitUpdate,
	}); err != nil {
		t.Fatalf("SetGroupGrant error = %v", err)
	}

	sources, err := f.service.DescribePermissionSources(ctx, f.member, "kb1", "")
	if err != nil {
		t.Fatalf("DescribePermissionSources error = %v", err)
	}
	var groupRead, groupUpdate bool
	for _, source := range sources {
		if source.Kind == "group" && source.Bit == PermissionBitRead {
			groupRead = true
		}
		if source.Kind == "group" && source.Bit == PermissionBitUpdate {
			groupUpdate = true
		}
	}
	if !groupRead || !groupUpdate {
		t.Fatalf("group sources = %+v", sources)
	}
}

// TestGroupGrantOnForeignKbAndMaskValidation pins the payload validation of
// group grants.
func TestGroupGrantOnForeignKbAndMaskValidation(t *testing.T) {
	f := newGroupFixture(t)
	ctx := context.Background()
	group := createGroup(t, f, f.owner, "校验组")

	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: 0,
	}); err == nil {
		t.Fatal("zero allow mask must be refused")
	}
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "kb1", GroupID: group.ID, AllowMask: 16,
	}); err == nil {
		t.Fatal("allow mask above 15 must be refused")
	}
	if err := f.service.SetGroupGrant(ctx, f.owner, SetGroupGrantRequest{
		Namespace: "missing-kb", GroupID: group.ID, AllowMask: PermissionBitRead,
	}); err == nil {
		t.Fatal("granting on a missing knowledge base must fail")
	}
}

// contains reports whether the slice holds the value.
func contains(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
