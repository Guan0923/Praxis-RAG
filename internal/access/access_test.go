package access

import (
	"context"
	"encoding/base64"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// testEncryptionKey is the same 32-byte "k" key the state fixtures use.
var testEncryptionKey = "base64:" + base64.StdEncoding.EncodeToString([]byte("kkkkkkkkkkkkkkkkkkkkkkkkkkkkkkkk"))

// newTestStores opens the disposable MySQL test schema.
func newTestStores(t *testing.T) *state.Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

// clockState is the mutable test clock shared with the access service.
type clockState struct{ now time.Time }

// fixture is one prepared sharing scenario.
type fixture struct {
	stores   *state.Stores
	clock    *clockState
	service  *Service
	admin    state.User
	owner    state.User
	vip      state.User
	member   state.User
	outsider state.User
}

// newFixture builds accounts and one knowledge base owned by owner (a VIP).
func newFixture(t *testing.T) *fixture {
	t.Helper()
	stores := newTestStores(t)
	ctx := context.Background()
	admin, err := stores.Users.Create(ctx, "alice", "hash")
	if err != nil {
		t.Fatalf("Create(alice) error = %v", err)
	}
	updatedAdmin, err := stores.Users.Update(ctx, admin.ID, state.RoleAdmin, true)
	if err != nil || updatedAdmin == nil {
		t.Fatalf("Update(alice, admin) error = %v", err)
	}
	admin = *updatedAdmin
	owner, err := stores.Users.Create(ctx, "bob", "hash")
	if err != nil {
		t.Fatalf("Create(bob) error = %v", err)
	}
	updatedOwner, err := stores.Users.Update(ctx, owner.ID, state.RoleVIP, true)
	if err != nil || updatedOwner == nil {
		t.Fatalf("Update(bob, vip) error = %v", err)
	}
	owner = *updatedOwner
	vip, err := stores.Users.Create(ctx, "carol", "hash")
	if err != nil {
		t.Fatalf("Create(carol) error = %v", err)
	}
	updatedVIP, err := stores.Users.Update(ctx, vip.ID, state.RoleVIP, true)
	if err != nil || updatedVIP == nil {
		t.Fatalf("Update(carol, vip) error = %v", err)
	}
	vip = *updatedVIP
	member, err := stores.Users.Create(ctx, "dave", "hash")
	if err != nil {
		t.Fatalf("Create(dave) error = %v", err)
	}
	outsider, err := stores.Users.Create(ctx, "erin", "hash")
	if err != nil {
		t.Fatalf("Create(erin) error = %v", err)
	}
	now := clockState{now: time.Date(2026, 11, 9, 12, 0, 0, 0, time.UTC)}
	service := NewService(stores, Options{Now: func() time.Time { return now.now }, InvitationTTL: time.Hour})
	if err := stores.KnowledgeBases.CreateWithOwner(ctx,
		map[string]any{"namespace": "kb1", "collection": "kb1_collection"}, owner.ID); err != nil {
		t.Fatalf("CreateWithOwner error = %v", err)
	}
	if err := stores.KnowledgeBases.CreateWithOwner(ctx,
		map[string]any{"namespace": "admin-kb", "collection": "admin_kb_collection"}, admin.ID); err != nil {
		t.Fatalf("CreateWithOwner(admin-kb) error = %v", err)
	}
	return &fixture{stores: stores, clock: &now, service: service,
		admin: admin, owner: owner, vip: vip, member: member, outsider: outsider}
}

func TestScopeSetDocumentRules(t *testing.T) {
	scope := NewScopeSet()
	scope.FileAllow["a.md"] = uint16(OpRead)
	scope.FileAllow["b.md"] = uint16(OpRead | OpUpdate)
	if !scope.AllowsAnyRead() || !scope.HasAnyWrite() || scope.ReadsWhole() || scope.WritesWhole() {
		t.Fatalf("partial scope flags = %+v", scope)
	}
	if !scope.Restricted() {
		t.Fatal("partial scope must be restricted")
	}
	if !scope.CanReadDocument("a.md") || !scope.CanReadDocument("b.md") || scope.CanReadDocument("c.md") {
		t.Fatalf("CanReadDocument = %v", scope)
	}
	if scope.CanWriteDocument("a.md") || !scope.CanWriteDocument("b.md") {
		t.Fatalf("CanWriteDocument = %v", scope)
	}
	if names := scope.ReadDocumentNames(); len(names) != 2 || names[0] != "a.md" || names[1] != "b.md" {
		t.Fatalf("ReadDocumentNames = %v", names)
	}
	if names := scope.WriteDocumentNames(); len(names) != 1 || names[0] != "b.md" {
		t.Fatalf("WriteDocumentNames = %v", names)
	}

	full := NewScopeSet()
	full.DefaultAllow = 15
	if !full.AllowsAnyRead() || !full.HasAnyWrite() || !full.ReadsWhole() || full.Restricted() {
		t.Fatalf("full scope flags = %+v", full)
	}
	if !full.CanReadDocument("anything") || !full.CanWriteDocument("anything") {
		t.Fatal("full scope must allow every document")
	}

	empty := NewScopeSet()
	if empty.AllowsAnyRead() || empty.HasAnyWrite() {
		t.Fatal("empty scope must allow nothing")
	}
}

func TestScopeContextRoundTrip(t *testing.T) {
	ctx := context.Background()
	if _, ok := ScopeFromContext(ctx); ok {
		t.Fatal("plain context must not carry a scope")
	}
	scope := NewScopeSet()
	scope.WholeAllow = uint16(OpRead)
	ctx = ContextWithScope(ctx, scope)
	loaded, ok := ScopeFromContext(ctx)
	if !ok || !loaded.ReadsWhole() {
		t.Fatalf("ScopeFromContext = %+v, %v", loaded, ok)
	}
}

func TestScopeSetsForOwnerAdminAndGrantees(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	adminScope, err := f.service.ScopeSet(ctx, f.admin, "kb1")
	if err != nil || !adminScope.ReadsWhole() || !adminScope.WritesWhole() {
		t.Fatalf("admin scope = %+v, %v", adminScope, err)
	}
	ownerScope, err := f.service.ScopeSet(ctx, f.owner, "kb1")
	if err != nil || !ownerScope.ReadsWhole() || !ownerScope.WritesWhole() {
		t.Fatalf("owner scope = %+v, %v", ownerScope, err)
	}
	strangerScope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil || strangerScope.AllowsAnyRead() || strangerScope.HasAnyWrite() {
		t.Fatalf("stranger scope = %+v, %v", strangerScope, err)
	}

	// A single-file read grant plus a whole-knowledge-base write grant.
	if err := f.stores.Permissions.Upsert(ctx, state.Permission{Namespace: "kb1",
		GranteeUserID: f.member.ID, DocumentName: "manual.md",
		Permission: state.PermissionRead, GrantedBy: f.owner.ID}); err != nil {
		t.Fatalf("Upsert(read) error = %v", err)
	}
	if err := f.stores.Permissions.Upsert(ctx, state.Permission{Namespace: "kb1",
		GranteeUserID: f.member.ID, DocumentName: "",
		Permission: state.PermissionWrite, GrantedBy: f.owner.ID}); err != nil {
		t.Fatalf("Upsert(write) error = %v", err)
	}
	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet(member) error = %v", err)
	}
	if !scope.WritesWhole() || !scope.CanReadDocument("manual.md") || !scope.CanReadDocument("other.md") {
		t.Fatalf("member scope = %+v", scope)
	}

	// A file-scoped write grant implies read for that document only.
	if _, err := f.stores.Permissions.Delete(ctx, "kb1", f.member.ID); err != nil {
		t.Fatalf("Delete(member grants) error = %v", err)
	}
	if err := f.stores.Permissions.Upsert(ctx, state.Permission{Namespace: "kb1",
		GranteeUserID: f.member.ID, DocumentName: "notes.txt",
		Permission: state.PermissionWrite, GrantedBy: f.owner.ID}); err != nil {
		t.Fatalf("Upsert(file write) error = %v", err)
	}
	scope, err = f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("ScopeSet(file write) error = %v", err)
	}
	if scope.WritesWhole() || !scope.CanWriteDocument("notes.txt") || !scope.CanReadDocument("notes.txt") ||
		scope.CanReadDocument("manual.md") {
		t.Fatalf("file write scope = %+v", scope)
	}
}

func TestManageAndSharePermissions(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	manage, err := f.service.CanManage(ctx, f.owner, "kb1")
	if err != nil || !manage {
		t.Fatalf("owner CanManage = %v, %v", manage, err)
	}
	manage, err = f.service.CanManage(ctx, f.admin, "kb1")
	if err != nil || !manage {
		t.Fatalf("admin CanManage = %v, %v", manage, err)
	}
	manage, err = f.service.CanManage(ctx, f.vip, "kb1")
	if err != nil || manage {
		t.Fatalf("other vip CanManage = %v, %v", manage, err)
	}
	if err := f.service.RequireSharePermission(ctx, f.member, "kb1"); err == nil {
		t.Fatal("normal user must not pass RequireSharePermission")
	}
	if err := f.service.RequireSharePermission(ctx, f.vip, "kb1"); err == nil {
		t.Fatal("VIP must not manage a knowledge base they do not own")
	}
	if err := f.service.RequireSharePermission(ctx, f.owner, "kb1"); err != nil {
		t.Fatalf("owner RequireSharePermission error = %v", err)
	}

	namespaces, err := f.service.VisibleNamespaces(ctx, f.admin)
	if err != nil || len(namespaces) != 2 {
		t.Fatalf("admin VisibleNamespaces = %v, %v", namespaces, err)
	}
	namespaces, err = f.service.VisibleNamespaces(ctx, f.owner)
	if err != nil || len(namespaces) != 1 || namespaces[0] != "kb1" {
		t.Fatalf("owner VisibleNamespaces = %v, %v", namespaces, err)
	}
	if err := f.stores.Permissions.Upsert(ctx, state.Permission{Namespace: "admin-kb",
		GranteeUserID: f.member.ID, DocumentName: "", Permission: state.PermissionRead,
		GrantedBy: f.admin.ID}); err != nil {
		t.Fatalf("Upsert(grant) error = %v", err)
	}
	namespaces, err = f.service.VisibleNamespaces(ctx, f.member)
	if err != nil || len(namespaces) != 1 || namespaces[0] != "admin-kb" {
		t.Fatalf("member VisibleNamespaces = %v, %v", namespaces, err)
	}
}

func TestInvitationCreateAndAccept(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	// A normal user cannot invite anybody.
	if _, _, err := f.service.CreateInvitation(ctx, f.member, CreateInvitationRequest{
		Namespace: "kb1", Username: f.outsider.Username, Permission: PermissionRead, Scope: ScopeAll,
	}); err == nil {
		t.Fatal("normal user must not create invitations")
	} else if typed, ok := asError(err); !ok || typed.Kind != KindForbidden {
		t.Fatalf("normal-user invite error = %v", err)
	}
	// A VIP cannot invite into another account's knowledge base.
	if _, _, err := f.service.CreateInvitation(ctx, f.vip, CreateInvitationRequest{
		Namespace: "kb1", Username: f.outsider.Username, Permission: PermissionRead, Scope: ScopeAll,
	}); err == nil {
		t.Fatal("VIP must not share a knowledge base they do not own")
	}

	view, token, err := f.service.CreateInvitation(ctx, f.owner, CreateInvitationRequest{
		Namespace: "kb1", Username: "DAVE", Permission: PermissionWrite,
		Scope: ScopeFiles, DocumentNames: []string{"manual.md", "manual.md", "guide.pdf"},
	})
	if err != nil {
		t.Fatalf("CreateInvitation error = %v", err)
	}
	if token == "" || view.Status != "pending" || view.Scope != ScopeFiles {
		t.Fatalf("created invitation = %+v (token set: %v)", view, token != "")
	}
	if len(view.DocumentNames) != 2 || view.DocumentNames[0] != "guide.pdf" || view.DocumentNames[1] != "manual.md" {
		t.Fatalf("normalized document names = %v", view.DocumentNames)
	}
	if view.InviterUsername != f.owner.Username || view.InviteeUsername != f.member.Username {
		t.Fatalf("usernames = %+v", view)
	}

	// Only the addressed account may preview or accept.
	if _, err := f.service.PreviewInvitation(ctx, f.outsider, token); err == nil {
		t.Fatal("other accounts must not preview the invitation")
	}
	preview, err := f.service.PreviewInvitation(ctx, f.member, token)
	if err != nil || preview.ID != view.ID {
		t.Fatalf("PreviewInvitation = %+v, %v", preview, err)
	}

	inbox, err := f.service.Inbox(ctx, f.member)
	if err != nil || len(inbox) != 1 || inbox[0].Status != "pending" {
		t.Fatalf("Inbox = %+v, %v", inbox, err)
	}

	accepted, err := f.service.AcceptInvitation(ctx, f.member, token)
	if err != nil || accepted.Status != "accepted" {
		t.Fatalf("AcceptInvitation = %+v, %v", accepted, err)
	}
	scope, err := f.service.ScopeSet(ctx, f.member, "kb1")
	if err != nil || !scope.CanWriteDocument("manual.md") || !scope.CanWriteDocument("guide.pdf") ||
		scope.CanWriteDocument("other.md") {
		t.Fatalf("scope after accept = %+v, %v", scope, err)
	}
	// Accepting twice is idempotent.
	if again, err := f.service.AcceptInvitation(ctx, f.member, token); err != nil || again.Status != "accepted" {
		t.Fatalf("second AcceptInvitation = %+v, %v", again, err)
	}

	grants, err := f.service.ListGrants(ctx, f.owner, "kb1")
	if err != nil || len(grants) != 2 {
		t.Fatalf("ListGrants = %+v, %v", grants, err)
	}
	if grants[0].Username != f.member.Username || grants[0].GrantedByUsername != f.owner.Username {
		t.Fatalf("grant usernames = %+v", grants)
	}
	if err := f.service.RevokeGrant(ctx, f.owner, "kb1", f.member.ID); err != nil {
		t.Fatalf("RevokeGrant error = %v", err)
	}
	if grants, _ := f.service.ListGrants(ctx, f.owner, "kb1"); len(grants) != 0 {
		t.Fatalf("grants after revoke = %+v", grants)
	}
	if err := f.service.RevokeGrant(ctx, f.owner, "kb1", f.member.ID); err == nil {
		t.Fatal("revoking a missing grant must fail")
	}
}

func TestInvitationDeclineRevokeAndExpiry(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	create := func(scope string, names []string) (InvitationView, string) {
		view, token, err := f.service.CreateInvitation(ctx, f.owner, CreateInvitationRequest{
			Namespace: "kb1", Username: f.member.Username, Permission: PermissionRead,
			Scope: scope, DocumentNames: names,
		})
		if err != nil {
			t.Fatalf("CreateInvitation error = %v", err)
		}
		return view, token
	}

	_, token := create(ScopeAll, nil)
	declined, err := f.service.DeclineInvitation(ctx, f.member, token)
	if err != nil || declined.Status != "declined" {
		t.Fatalf("DeclineInvitation = %+v, %v", declined, err)
	}
	if _, err := f.service.AcceptInvitation(ctx, f.member, token); err == nil {
		t.Fatal("accepting a declined invitation must fail")
	}

	// Re-inviting the same account revokes the older pending link: its preview
	// reports the revoked status and it can no longer be accepted.
	_, firstToken := create(ScopeAll, nil)
	_, secondToken := create(ScopeAll, nil)
	superseded, err := f.service.PreviewInvitation(ctx, f.member, firstToken)
	if err != nil || superseded.Status != "revoked" {
		t.Fatalf("superseded invitation preview = %+v, %v; want revoked", superseded, err)
	}
	if _, err := f.service.AcceptInvitation(ctx, f.member, firstToken); err == nil {
		t.Fatal("superseded invitation must not accept")
	}

	// The owner may revoke a pending invitation; it then cannot be accepted.
	invitations, err := f.service.ListNamespaceInvitations(ctx, f.owner, "kb1")
	if err != nil || len(invitations) == 0 {
		t.Fatalf("ListNamespaceInvitations = %+v, %v", invitations, err)
	}
	var pending InvitationView
	for _, item := range invitations {
		if item.Status == "pending" {
			pending = item
		}
	}
	if pending.ID == "" {
		t.Fatalf("no pending invitation in %+v", invitations)
	}
	if err := f.service.RevokeInvitation(ctx, f.owner, pending.ID); err != nil {
		t.Fatalf("RevokeInvitation error = %v", err)
	}
	if _, err := f.service.AcceptInvitation(ctx, f.member, secondToken); err == nil {
		t.Fatal("accepting a revoked invitation must fail")
	}

	// Expiry: advance the clock past the TTL.
	_, expiredToken := create(ScopeAll, nil)
	f.clock.now = f.clock.now.Add(2 * time.Hour)
	preview, err := f.service.PreviewInvitation(ctx, f.member, expiredToken)
	if err != nil {
		t.Fatalf("PreviewInvitation(expired) error = %v", err)
	}
	if preview.Status != "expired" {
		t.Fatalf("expired preview status = %q", preview.Status)
	}
	if _, err := f.service.AcceptInvitation(ctx, f.member, expiredToken); err == nil {
		t.Fatal("accepting an expired invitation must fail")
	} else if typed, ok := asError(err); !ok || typed.Kind != KindConflict {
		t.Fatalf("expired accept error = %v", err)
	}
}

func TestInvitationValidation(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	cases := []struct {
		name    string
		request CreateInvitationRequest
	}{
		{"bad permission", CreateInvitationRequest{Namespace: "kb1", Username: f.member.Username,
			Permission: "owner", Scope: ScopeAll}},
		{"bad scope", CreateInvitationRequest{Namespace: "kb1", Username: f.member.Username,
			Permission: PermissionRead, Scope: "folder"}},
		{"no documents", CreateInvitationRequest{Namespace: "kb1", Username: f.member.Username,
			Permission: PermissionRead, Scope: ScopeFiles}},
		{"self invite", CreateInvitationRequest{Namespace: "kb1", Username: f.owner.Username,
			Permission: PermissionRead, Scope: ScopeAll}},
		{"unknown user", CreateInvitationRequest{Namespace: "kb1", Username: "nobody",
			Permission: PermissionRead, Scope: ScopeAll}},
		{"unknown knowledge base", CreateInvitationRequest{Namespace: "missing",
			Username: f.member.Username, Permission: PermissionRead, Scope: ScopeAll}},
		{"bad document name", CreateInvitationRequest{Namespace: "kb1", Username: f.member.Username,
			Permission: PermissionRead, Scope: ScopeFiles, DocumentNames: []string{"../secret.md"}}},
	}
	for _, testCase := range cases {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			if _, _, err := f.service.CreateInvitation(ctx, f.owner, testCase.request); err == nil {
				t.Fatalf("%s: expected an error", testCase.name)
			}
		})
	}

	// Disabled invitee is rejected.
	if _, err := f.stores.Users.Update(ctx, f.member.ID, state.RoleUser, false); err != nil {
		t.Fatalf("Update(disable) error = %v", err)
	}
	if _, _, err := f.service.CreateInvitation(ctx, f.owner, CreateInvitationRequest{
		Namespace: "kb1", Username: f.member.Username, Permission: PermissionRead, Scope: ScopeAll,
	}); err == nil {
		t.Fatal("inviting a disabled account must fail")
	}
}

func TestInvitableUsersAndDescribe(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()

	users, err := f.service.ListInvitableUsers(ctx, f.owner)
	if err != nil {
		t.Fatalf("ListInvitableUsers error = %v", err)
	}
	if len(users) != 4 {
		t.Fatalf("ListInvitableUsers returned %d users, want 4 (excluding self)", len(users))
	}
	if _, err := f.service.ListInvitableUsers(ctx, f.member); err == nil {
		t.Fatal("normal users must not list invitable accounts")
	}

	if err := f.stores.Permissions.Upsert(ctx, state.Permission{Namespace: "kb1",
		GranteeUserID: f.member.ID, DocumentName: "manual.md",
		Permission: state.PermissionRead, GrantedBy: f.owner.ID}); err != nil {
		t.Fatalf("Upsert error = %v", err)
	}
	view, err := f.service.Describe(ctx, f.member, "kb1")
	if err != nil {
		t.Fatalf("Describe(member) error = %v", err)
	}
	if view.CanManage || view.CanShare || !view.CanRead || view.CanWrite {
		t.Fatalf("member access view = %+v", view)
	}
	if view.Scope.WholeRead || len(view.Scope.ReadDocuments) != 1 || view.Scope.ReadDocuments[0] != "manual.md" {
		t.Fatalf("member scope view = %+v", view.Scope)
	}
	ownerView, err := f.service.Describe(ctx, f.owner, "kb1")
	if err != nil {
		t.Fatalf("Describe(owner) error = %v", err)
	}
	if !ownerView.Owner || !ownerView.CanManage || !ownerView.CanShare || !ownerView.CanWrite ||
		!ownerView.Scope.WholeWrite {
		t.Fatalf("owner access view = %+v", ownerView)
	}
	// An administrator sees can_share for every knowledge base.
	adminView, err := f.service.Describe(ctx, f.admin, "kb1")
	if err != nil {
		t.Fatalf("Describe(admin) error = %v", err)
	}
	if adminView.Owner || !adminView.CanManage || !adminView.CanShare {
		t.Fatalf("admin access view = %+v", adminView)
	}
}

func TestOwnerOfAndDocumentIDs(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	if owner, err := f.service.Owner(ctx, "kb1"); err != nil || owner != f.owner.ID {
		t.Fatalf("Owner = %q, %v", owner, err)
	}
	if owner, err := f.service.Owner(ctx, "missing"); err != nil || owner != "" {
		t.Fatalf("Owner(missing) = %q, %v", owner, err)
	}
	if err := f.stores.Documents.Upsert(ctx, "kb1", state.DocumentRecord{
		Name: "manual.md", DocumentID: "doc-1", Size: 10, SHA256: "sha",
		ChunkCount: 2, Collection: "kb1_collection",
	}); err != nil {
		t.Fatalf("Documents.Upsert error = %v", err)
	}
	ids, err := f.service.ResolveDocumentIDs(ctx, "kb1", []string{"manual.md", "missing.md"})
	if err != nil || len(ids) != 1 || ids[0] != "doc-1" {
		t.Fatalf("ResolveDocumentIDs = %v, %v", ids, err)
	}
	name, ok, err := f.service.DocumentIDToName(ctx, "kb1", "doc-1")
	if err != nil || !ok || name != "manual.md" {
		t.Fatalf("DocumentIDToName = %q, %v, %v", name, ok, err)
	}
}
