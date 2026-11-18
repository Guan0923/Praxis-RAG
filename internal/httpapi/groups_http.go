package httpapi

import (
	"net/http"

	"github.com/Guan0923/Praxis-RAG/internal/access"
)

// registerGroupRoutes mounts the user-group management surface: groups,
// members, invitations and the knowledge-base group grants. Every route
// requires a Console login session (authSession); role, group-ownership and
// knowledge-base-ownership checks run inside the access service.
func (s *Server) registerGroupRoutes() {
	s.router.add("GET", "/v1/groups", authSession, s.handleListGroups)
	s.router.add("POST", "/v1/groups", authSession, s.handleCreateGroup)
	s.router.add("GET", "/v1/groups/{group_id}", authSession, s.handleGroupDetail)
	s.router.add("DELETE", "/v1/groups/{group_id}", authSession, s.handleDissolveGroup)
	s.router.add("POST", "/v1/groups/{group_id}/invitations", authSession, s.handleCreateGroupInvitation)
	s.router.add("DELETE", "/v1/groups/{group_id}/members/{user_id}", authSession, s.handleRemoveGroupMember)
	s.router.add("DELETE", "/v1/groups/{group_id}/membership", authSession, s.handleLeaveGroup)
	s.router.add("DELETE", "/v1/group-invitations/{invitation_id}", authSession, s.handleRevokeGroupInvitation)
	s.router.add("GET", "/v1/group-invitations", authSession, s.handleGroupInvitationInbox)
	s.router.add("POST", "/v1/group-invitations/preview", authSession, s.handlePreviewGroupInvitation)
	s.router.add("POST", "/v1/group-invitations/accept", authSession, s.handleAcceptGroupInvitation)
	s.router.add("POST", "/v1/group-invitations/decline", authSession, s.handleDeclineGroupInvitation)
	s.router.add("GET", "/v1/knowledge-bases/{namespace}/group-permissions", authSession, s.handleListGroupGrants)
	s.router.add("PUT", "/v1/knowledge-bases/{namespace}/group-permissions/{group_id}", authSession, s.handleSetGroupGrant)
	s.router.add("DELETE", "/v1/knowledge-bases/{namespace}/group-permissions/{group_id}", authSession, s.handleRevokeGroupGrant)
	s.router.add("GET", "/v1/knowledge-bases/{namespace}/permission-sources", authSession, s.handlePermissionSources)
}

// requireGroupControl reports 503 when the sharing service (which owns the
// group surface) is not wired.
func (s *Server) requireGroupControl(w http.ResponseWriter) bool {
	if control, ok := s.access.(GroupControl); ok && control != nil {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable, "用户组服务未启用")
	return false
}

// createGroupBody is the group creation payload.
type createGroupBody struct {
	Name string `json:"name"`
}

// handleCreateGroup creates one group owned by the session account.
func (s *Server) handleCreateGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	var body createGroupBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	view, err := s.access.(GroupControl).CreateGroup(r.Context(), session.User, access.CreateGroupRequest{
		Name: body.Name,
	})
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"group": view})
}

// handleListGroups returns the groups the caller created or joined.
func (s *Server) handleListGroups(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	owned, joined, err := s.access.(GroupControl).ListGroups(r.Context(), session.User)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if owned == nil {
		owned = []access.GroupView{}
	}
	if joined == nil {
		joined = []access.GroupView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"owned_groups": owned, "joined_groups": joined})
}

// handleGroupDetail returns one group with its members and invitations.
func (s *Server) handleGroupDetail(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	group, members, invitations, err := s.access.(GroupControl).GroupDetail(
		r.Context(), session.User, pathParam(r, "group_id"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if members == nil {
		members = []access.GroupMemberView{}
	}
	if invitations == nil {
		invitations = []access.GroupInvitationView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"group":       group,
		"members":     members,
		"invitations": invitations,
	})
}

// handleDissolveGroup deactivates one group.
func (s *Server) handleDissolveGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	if err := s.access.(GroupControl).DissolveGroup(
		r.Context(), session.User, pathParam(r, "group_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// createGroupInvitationBody is the group invitation payload.
type createGroupInvitationBody struct {
	Username string `json:"username"`
}

// handleCreateGroupInvitation issues one membership invitation link. The raw
// token only appears in this response; the database stores its digest.
func (s *Server) handleCreateGroupInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	var body createGroupInvitationBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	invitation, token, err := s.access.(GroupControl).CreateGroupInvitation(
		r.Context(), session.User, access.CreateGroupInvitationRequest{
			GroupID:  pathParam(r, "group_id"),
			Username: body.Username,
		})
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"invitation": invitation,
		"invite_path": "/console#/group-invite?token=" + token,
	})
}

// handleRemoveGroupMember removes one member from one group.
func (s *Server) handleRemoveGroupMember(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	if err := s.access.(GroupControl).RemoveGroupMember(
		r.Context(), session.User, pathParam(r, "group_id"), pathParam(r, "user_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleLeaveGroup removes the caller's own membership.
func (s *Server) handleLeaveGroup(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	if err := s.access.(GroupControl).LeaveGroup(
		r.Context(), session.User, pathParam(r, "group_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeGroupInvitation withdraws one pending membership invitation.
func (s *Server) handleRevokeGroupInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	if err := s.access.(GroupControl).RevokeGroupInvitation(
		r.Context(), session.User, pathParam(r, "invitation_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleGroupInvitationInbox returns the membership invitations addressed to
// the caller.
func (s *Server) handleGroupInvitationInbox(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	invitations, err := s.access.(GroupControl).GroupInvitationInbox(r.Context(), session.User)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if invitations == nil {
		invitations = []access.GroupInvitationView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitations": invitations})
}

// handlePreviewGroupInvitation resolves one membership link for its invitee.
func (s *Server) handlePreviewGroupInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	token, ok := decodeInvitationToken(w, r)
	if !ok {
		return
	}
	invitation, err := s.access.(GroupControl).PreviewGroupInvitation(r.Context(), session.User, token)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitation": invitation})
}

// handleAcceptGroupInvitation turns one pending membership invitation into a
// membership.
func (s *Server) handleAcceptGroupInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	token, ok := decodeInvitationToken(w, r)
	if !ok {
		return
	}
	invitation, err := s.access.(GroupControl).AcceptGroupInvitation(r.Context(), session.User, token)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitation": invitation})
}

// handleDeclineGroupInvitation declines one pending membership invitation.
func (s *Server) handleDeclineGroupInvitation(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	token, ok := decodeInvitationToken(w, r)
	if !ok {
		return
	}
	invitation, err := s.access.(GroupControl).DeclineGroupInvitation(r.Context(), session.User, token)
	if err != nil {
		writeAccessError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"invitation": invitation})
}

// handleListGroupGrants lists the group grants of one knowledge base.
func (s *Server) handleListGroupGrants(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	grants, err := s.access.(GroupControl).ListGroupGrants(
		r.Context(), session.User, pathParam(r, "namespace"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if grants == nil {
		grants = []access.GroupGrantView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"group_permissions": grants})
}

// setGroupGrantBody is the group grant payload. document_names empty means
// the whole knowledge base.
type setGroupGrantBody struct {
	DocumentNames []string `json:"document_names"`
	AllowMask     int      `json:"allow_mask"`
}

// handleSetGroupGrant writes one whole-library or per-file group grant.
func (s *Server) handleSetGroupGrant(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	var body setGroupGrantBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if err := s.access.(GroupControl).SetGroupGrant(
		r.Context(), session.User, access.SetGroupGrantRequest{
			Namespace:     pathParam(r, "namespace"),
			GroupID:       pathParam(r, "group_id"),
			DocumentNames: body.DocumentNames,
			AllowMask:     body.AllowMask,
		}); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleRevokeGroupGrant removes every grant of one group inside one
// knowledge base.
func (s *Server) handleRevokeGroupGrant(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	if err := s.access.(GroupControl).RevokeGroupGrant(
		r.Context(), session.User, pathParam(r, "namespace"), pathParam(r, "group_id")); err != nil {
		writeAccessError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handlePermissionSources explains the origin of the caller's effective
// permissions for one namespace (all documents, or one document).
func (s *Server) handlePermissionSources(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireGroupControl(w) {
		return
	}
	control := s.access.(GroupControl)
	sources, err := control.DescribePermissionSources(
		r.Context(), session.User, pathParam(r, "namespace"), r.URL.Query().Get("document_name"))
	if err != nil {
		writeAccessError(w, err)
		return
	}
	if sources == nil {
		sources = []access.PermissionSourceView{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sources": sources})
}
