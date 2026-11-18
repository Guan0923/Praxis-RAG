package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// fakeGroupControl is an in-memory AccessControl that also implements the
// group surface, for transport tests. It embeds the legacy fake so the
// existing sharing assertions keep working; the group fields record the last
// call of each route.
type fakeGroupControl struct {
	fakeAccessControl

	groups     []access.GroupView
	groupDetail access.GroupView
	members    []access.GroupMemberView
	invitations []access.GroupInvitationView

	createGroupVal  access.GroupView
	createGroupErr  error
	lastCreateGroup access.CreateGroupRequest

	listErr error

	detailErr error

	createInviteVal   access.GroupInvitationView
	createInviteToken string
	createInviteErr   error
	lastCreateInvite access.CreateGroupInvitationRequest

	inboxVal []access.GroupInvitationView
	inboxErr error

	previewVal access.GroupInvitationView
	previewErr error

	acceptVal access.GroupInvitationView
	acceptErr error

	declineVal access.GroupInvitationView
	declineErr error

	revokedInvitationID string
	revokeInvitationErr error

	removedGroup string
	removedUser  string
	removeErr    error

	leftGroup string
	leaveErr  error

	dissolvedGroup string
	dissolveErr    error

	grants    []access.GroupGrantView
	grantsErr error

	lastSetGrant access.SetGroupGrantRequest
	setGrantErr error

	revokedGrantNS     string
	revokedGrantGroup  string
	revokeGrantErr     error

	sources    []access.PermissionSourceView
	sourcesErr error
}

func (f *fakeGroupControl) CreateGroup(_ context.Context, _ state.User, request access.CreateGroupRequest) (access.GroupView, error) {
	f.lastCreateGroup = request
	return f.createGroupVal, f.createGroupErr
}

func (f *fakeGroupControl) ListGroups(context.Context, state.User) ([]access.GroupView, []access.GroupView, error) {
	if f.listErr != nil {
		return nil, nil, f.listErr
	}
	return f.groups, f.groups, nil
}

func (f *fakeGroupControl) GroupDetail(context.Context, state.User, string) (access.GroupView, []access.GroupMemberView, []access.GroupInvitationView, error) {
	return f.groupDetail, f.members, f.invitations, f.detailErr
}

func (f *fakeGroupControl) CreateGroupInvitation(
	_ context.Context, _ state.User, request access.CreateGroupInvitationRequest,
) (access.GroupInvitationView, string, error) {
	f.lastCreateInvite = request
	return f.createInviteVal, f.createInviteToken, f.createInviteErr
}

func (f *fakeGroupControl) GroupInvitationInbox(context.Context, state.User) ([]access.GroupInvitationView, error) {
	return f.inboxVal, f.inboxErr
}

func (f *fakeGroupControl) PreviewGroupInvitation(context.Context, state.User, string) (access.GroupInvitationView, error) {
	return f.previewVal, f.previewErr
}

func (f *fakeGroupControl) AcceptGroupInvitation(context.Context, state.User, string) (access.GroupInvitationView, error) {
	return f.acceptVal, f.acceptErr
}

func (f *fakeGroupControl) DeclineGroupInvitation(context.Context, state.User, string) (access.GroupInvitationView, error) {
	return f.declineVal, f.declineErr
}

func (f *fakeGroupControl) RevokeGroupInvitation(_ context.Context, _ state.User, invitationID string) error {
	f.revokedInvitationID = invitationID
	return f.revokeInvitationErr
}

func (f *fakeGroupControl) RemoveGroupMember(_ context.Context, _ state.User, groupID, memberUserID string) error {
	f.removedGroup = groupID
	f.removedUser = memberUserID
	return f.removeErr
}

func (f *fakeGroupControl) LeaveGroup(_ context.Context, _ state.User, groupID string) error {
	f.leftGroup = groupID
	return f.leaveErr
}

func (f *fakeGroupControl) DissolveGroup(_ context.Context, _ state.User, groupID string) error {
	f.dissolvedGroup = groupID
	return f.dissolveErr
}

func (f *fakeGroupControl) ListGroupGrants(context.Context, state.User, string) ([]access.GroupGrantView, error) {
	return f.grants, f.grantsErr
}

func (f *fakeGroupControl) SetGroupGrant(_ context.Context, _ state.User, request access.SetGroupGrantRequest) error {
	f.lastSetGrant = request
	return f.setGrantErr
}

func (f *fakeGroupControl) RevokeGroupGrant(_ context.Context, _ state.User, namespace, groupID string) error {
	f.revokedGrantNS = namespace
	f.revokedGrantGroup = groupID
	return f.revokeGrantErr
}

func (f *fakeGroupControl) DescribePermissionSources(context.Context, state.User, string, string) ([]access.PermissionSourceView, error) {
	return f.sources, f.sourcesErr
}

// newGroupTestServer builds a session-authenticated server with the group
// control wired.
func newGroupTestServer(t *testing.T, control *fakeGroupControl) *Server {
	t.Helper()
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"vip-token":  vipSession("vip-token"),
		"user-token": userSession("user-token"),
	}}
	options := []Option{WithUserAuth(users)}
	if control != nil {
		options = append(options, WithAccessControl(control))
	}
	return New(authTestSettings(), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)), options...)
}

// TestGroupRoutesRequireSessionAndCsrf pins the transport contract: every
// group route needs a Console session, and cookie-authenticated writes need
// the CSRF header.
func TestGroupRoutesRequireSessionAndCsrf(t *testing.T) {
	control := &fakeGroupControl{}
	handler := newGroupTestServer(t, control).Handler()

	anonymous := performRequest(handler, testRequest{
		method: "GET", target: "/v1/groups", remote: "127.0.0.1:5000",
	})
	if anonymous.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous groups status = %d body = %s", anonymous.Code, anonymous.Body.String())
	}
	if cache := anonymous.Header().Get("Cache-Control"); cache != "no-store" {
		t.Fatalf("anonymous groups Cache-Control = %q", cache)
	}

	missingCSRF := performRequest(handler, testRequest{
		method: "POST", target: "/v1/groups", remote: "10.0.0.9:5000",
		headers: map[string]string{
			"Content-Type": "application/json",
			"Cookie":       sessionCookieName + "=vip-token",
		},
		body: `{"name":"研究组"}`,
	})
	if missingCSRF.Code != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d body = %s", missingCSRF.Code, missingCSRF.Body.String())
	}

	// A correct session reaches the service.
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/groups", remote: "10.0.0.9:5000",
		headers: mergeHeaders(map[string]string{"Content-Type": "application/json"}, sessionHeaders("vip-token", "")),
		body: `{"name":"研究组"}`,
	})
	if created.Code != http.StatusCreated {
		t.Fatalf("create group status = %d body = %s", created.Code, created.Body.String())
	}
}

// mergeHeaders is defined in auth_users_test.go.

// TestGroupTransportRoundTrip pins the JSON contracts of the group surface.
func TestGroupTransportRoundTrip(t *testing.T) {
	control := &fakeGroupControl{
		groups: []access.GroupView{{
			ID: "g1", Name: "研究组", OwnerUserID: "u-vip", OwnerUsername: "carol",
			Status: "active", MemberCount: 2, CreatedAt: "2026-11-18T10:00:00.000000+00:00",
		}},
		members: []access.GroupMemberView{{
			UserID: "u-user", Username: "bob", Role: "user", Enabled: true,
			JoinedAt: "2026-11-18T10:05:00.000000+00:00",
		}},
		invitations: []access.GroupInvitationView{{
			ID: "gi1", GroupID: "g1", GroupName: "研究组",
			InviterUserID: "u-vip", InviterUsername: "carol",
			InviteeUserID: "u-user", InviteeUsername: "bob",
			Status: "pending", CreatedAt: "2026-11-18T10:00:00.000000+00:00",
			ExpiresAt: "2026-11-25T10:00:00.000000+00:00",
		}},
		createGroupVal: access.GroupView{ID: "g2", Name: "新组", Status: "active"},
		createInviteVal: access.GroupInvitationView{ID: "gi2", GroupID: "g1", Status: "pending"},
		createInviteToken: "group-token",
		grants: []access.GroupGrantView{{
			GroupID: "g1", GroupName: "研究组", Namespace: "kb1",
			DocumentName: "", AllowMask: 6, GrantedByUsername: "carol",
		}},
		sources: []access.PermissionSourceView{{
			Bit: 1, BitName: "read", Kind: "group", Origin: "来自用户组「研究组」", DocumentName: "",
		}},
	}
	handler := newGroupTestServer(t, control).Handler()
	headers := sessionHeaders("vip-token", "")
	headers["Content-Type"] = "application/json"

	// List.
	list := performRequest(handler, testRequest{method: "GET", target: "/v1/groups", remote: "10.0.0.9:5000", headers: sessionHeaders("vip-token", "")})
	if list.Code != http.StatusOK {
		t.Fatalf("list groups status = %d body = %s", list.Code, list.Body.String())
	}
	body := decodeBody(t, list)
	if body["owned_groups"] == nil || body["joined_groups"] == nil {
		t.Fatalf("list groups body = %v", body)
	}

	// Detail.
	detail := performRequest(handler, testRequest{method: "GET", target: "/v1/groups/g1", remote: "10.0.0.9:5000", headers: sessionHeaders("vip-token", "")})
	if detail.Code != http.StatusOK {
		t.Fatalf("group detail status = %d body = %s", detail.Code, detail.Body.String())
	}
	detailBody := decodeBody(t, detail)
	if detailBody["group"] == nil || detailBody["members"] == nil || detailBody["invitations"] == nil {
		t.Fatalf("group detail body = %v", detailBody)
	}

	// Create.
	create := performRequest(handler, testRequest{
		method: "POST", target: "/v1/groups", remote: "10.0.0.9:5000",
		headers: headers, body: `{"name":"新组"}`,
	})
	if create.Code != http.StatusCreated {
		t.Fatalf("create group status = %d body = %s", create.Code, create.Body.String())
	}
	if control.lastCreateGroup.Name != "新组" {
		t.Fatalf("lastCreateGroup = %+v", control.lastCreateGroup)
	}

	// Invitation.
	invite := performRequest(handler, testRequest{
		method: "POST", target: "/v1/groups/g1/invitations", remote: "10.0.0.9:5000",
		headers: headers, body: `{"username":"bob"}`,
	})
	if invite.Code != http.StatusCreated {
		t.Fatalf("create group invitation status = %d body = %s", invite.Code, invite.Body.String())
	}
	inviteBody := decodeBody(t, invite)
	if path, _ := inviteBody["invite_path"].(string); path != "/console#/group-invite?token=group-token" {
		t.Fatalf("invite_path = %v", inviteBody["invite_path"])
	}
	if control.lastCreateInvite.GroupID != "g1" || control.lastCreateInvite.Username != "bob" {
		t.Fatalf("lastCreateInvite = %+v", control.lastCreateInvite)
	}

	// Inbox / preview / accept / decline.
	for _, item := range []struct {
		method string
		target string
		body   string
	}{} {
		_ = item
	}
	inbox := performRequest(handler, testRequest{method: "GET", target: "/v1/group-invitations", remote: "10.0.0.9:5000", headers: sessionHeaders("vip-token", "")})
	if inbox.Code != http.StatusOK || decodeBody(t, inbox)["invitations"] == nil {
		t.Fatalf("inbox status = %d body = %s", inbox.Code, inbox.Body.String())
	}
	accept := performRequest(handler, testRequest{
		method: "POST", target: "/v1/group-invitations/accept", remote: "10.0.0.9:5000",
		headers: headers, body: `{"token":"raw-token"}`,
	})
	if accept.Code != http.StatusOK || decodeBody(t, accept)["invitation"] == nil {
		t.Fatalf("accept status = %d body = %s", accept.Code, accept.Body.String())
	}

	// Remove member / leave / dissolve / revoke invitation.
	remove := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/groups/g1/members/u-user", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if remove.Code != http.StatusNoContent {
		t.Fatalf("remove member status = %d body = %s", remove.Code, remove.Body.String())
	}
	if control.removedGroup != "g1" || control.removedUser != "u-user" {
		t.Fatalf("remove member call = %s %s", control.removedGroup, control.removedUser)
	}

	leave := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/groups/g1/membership", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if leave.Code != http.StatusNoContent {
		t.Fatalf("leave status = %d body = %s", leave.Code, leave.Body.String())
	}
	if control.leftGroup != "g1" {
		t.Fatalf("leave call = %s", control.leftGroup)
	}

	dissolve := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/groups/g1", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if dissolve.Code != http.StatusNoContent {
		t.Fatalf("dissolve status = %d body = %s", dissolve.Code, dissolve.Body.String())
	}
	if control.dissolvedGroup != "g1" {
		t.Fatalf("dissolve call = %s", control.dissolvedGroup)
	}

	revokeInv := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/group-invitations/gi1", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if revokeInv.Code != http.StatusNoContent {
		t.Fatalf("revoke invitation status = %d body = %s", revokeInv.Code, revokeInv.Body.String())
	}
	if control.revokedInvitationID != "gi1" {
		t.Fatalf("revoke invitation call = %s", control.revokedInvitationID)
	}

	// Group grants.
	listGrants := performRequest(handler, testRequest{
		method: "GET", target: "/v1/knowledge-bases/kb1/group-permissions", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if listGrants.Code != http.StatusOK || decodeBody(t, listGrants)["group_permissions"] == nil {
		t.Fatalf("list grants status = %d body = %s", listGrants.Code, listGrants.Body.String())
	}

	setGrant := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/knowledge-bases/kb1/group-permissions/g1", remote: "10.0.0.9:5000",
		headers: headers, body: `{"allow_mask":6}`,
	})
	if setGrant.Code != http.StatusNoContent {
		t.Fatalf("set grant status = %d body = %s", setGrant.Code, setGrant.Body.String())
	}
	if control.lastSetGrant.Namespace != "kb1" || control.lastSetGrant.GroupID != "g1" ||
		control.lastSetGrant.AllowMask != 6 || len(control.lastSetGrant.DocumentNames) != 0 {
		t.Fatalf("set grant call = %+v", control.lastSetGrant)
	}

	setFileGrant := performRequest(handler, testRequest{
		method: "PUT", target: "/v1/knowledge-bases/kb1/group-permissions/g1", remote: "10.0.0.9:5000",
		headers: headers, body: `{"allow_mask":5,"document_names":["spec.md"]}`,
	})
	if setFileGrant.Code != http.StatusNoContent {
		t.Fatalf("set file grant status = %d body = %s", setFileGrant.Code, setFileGrant.Body.String())
	}
	if control.lastSetGrant.AllowMask != 5 || len(control.lastSetGrant.DocumentNames) != 1 ||
		control.lastSetGrant.DocumentNames[0] != "spec.md" {
		t.Fatalf("set file grant call = %+v", control.lastSetGrant)
	}

	revokeGrant := performRequest(handler, testRequest{
		method: "DELETE", target: "/v1/knowledge-bases/kb1/group-permissions/g1", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if revokeGrant.Code != http.StatusNoContent {
		t.Fatalf("revoke grant status = %d body = %s", revokeGrant.Code, revokeGrant.Body.String())
	}
	if control.revokedGrantNS != "kb1" || control.revokedGrantGroup != "g1" {
		t.Fatalf("revoke grant call = %s %s", control.revokedGrantNS, control.revokedGrantGroup)
	}

	// Permission sources.
	sources := performRequest(handler, testRequest{
		method: "GET", target: "/v1/knowledge-bases/kb1/permission-sources?document_name=spec.md", remote: "10.0.0.9:5000",
		headers: sessionHeaders("vip-token", ""),
	})
	if sources.Code != http.StatusOK || decodeBody(t, sources)["sources"] == nil {
		t.Fatalf("permission sources status = %d body = %s", sources.Code, sources.Body.String())
	}
}

// TestGroupRoutesRefusePlainUserByService pins that role enforcement lives in
// the access service: the transport only relays the typed error (403 here).
func TestGroupRoutesRefusePlainUserByService(t *testing.T) {
	control := &fakeGroupControl{
		createGroupErr: access.NewError(access.KindForbidden, access.MessageGroupCreateForbidden),
	}
	handler := newGroupTestServer(t, control).Handler()
	created := performRequest(handler, testRequest{
		method: "POST", target: "/v1/groups", remote: "10.0.0.9:5000",
		headers: mergeHeaders(sessionHeaders("user-token", ""), map[string]string{"Content-Type": "application/json"}),
		body: `{"name":"越权组"}`,
	})
	if created.Code != http.StatusForbidden {
		t.Fatalf("user create group status = %d body = %s", created.Code, created.Body.String())
	}
}
