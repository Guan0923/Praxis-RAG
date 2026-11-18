// Package access is the knowledge-base authorization layer: it resolves what one
// Console account may read or modify inside a knowledge base, and it implements
// the invite-based sharing rules (only administrators and VIPs may share).
//
// The package owns no HTTP concerns. It returns typed errors that the httpapi
// layer maps onto status codes, and it exposes ScopeSet through the request
// context so the MCP endpoint can enforce the same visibility.
package access

import (
	"context"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// ErrorKind classifies a sharing failure for the HTTP/MCP layers.
type ErrorKind int

const (
	// KindValidation is a user-fixable input problem.
	KindValidation ErrorKind = iota
	// KindForbidden means the caller may not perform the operation.
	KindForbidden
	// KindNotFound means the knowledge base, user, or invitation is missing.
	KindNotFound
	// KindConflict means the invitation state does not allow the operation.
	KindConflict
)

// Error is a safe, user-facing sharing failure.
type Error struct {
	Kind    ErrorKind
	Message string
}

// Error implements error.
func (e *Error) Error() string { return e.Message }

// NewError builds a typed failure.
func NewError(kind ErrorKind, message string) *Error { return &Error{Kind: kind, Message: message} }

// Message constants keep the wording stable across handlers and tests.
const (
	MessageForbidden        = "需要 VIP 或管理员权限"
	MessageNotOwner         = "仅知识库创建者可以管理共享"
	MessageKnowledgeMissing = "知识库不存在"
	MessageKnowledgeAccess  = "无权访问该知识库"
	MessageWriteRequired    = "没有该知识库的修改权限"
	MessageInvitationGone   = "邀请不存在或链接已失效"
	MessageInvitationOwner  = "该邀请不属于当前账号"
	MessageInvitationUsed   = "邀请已处理或已失效"
	MessageInvitationExpiry = "邀请已过期"
	MessageInviterExpired   = "邀请人的会员权益已失效，无法接受该邀请"
	MessageUserMissing      = "用户不存在"
	MessageUserDisabled     = "该账号已被禁用"
	MessageSelfInvite       = "不能邀请自己"
	MessagePermissionBad    = "权限只能是 read 或 write"
	MessageScopeBad         = "范围只能是 all 或 files"
	MessageDocumentsEmpty   = "请选择要授权的文件"
	MessageDocumentsBad     = "文件名不合法："
	MessageDocumentsTooMany = "一次最多授权 200 个文件"
)

// Permission and scope values shared with the API surface.
const (
	PermissionRead  = state.PermissionRead
	PermissionWrite = state.PermissionWrite
	ScopeAll        = state.InvitationScopeAll
	ScopeFiles      = state.InvitationScopeFiles
)

// Document permission bits, shared with the group grant contract: read=0001,
// create=0010, update=0100, delete=1000. Personal grants keep the historical
// read/write levels and map onto the same bits.
const (
	PermissionBitRead   = state.PermissionBitRead
	PermissionBitCreate = state.PermissionBitCreate
	PermissionBitUpdate = state.PermissionBitUpdate
	PermissionBitDelete = state.PermissionBitDelete
	PermissionMaskAll   = state.PermissionMaskAll
)

// IsValidAllowMask reports whether value is a non-empty subset of the four
// permission bits.
func IsValidAllowMask(value int) bool { return state.IsValidAllowMask(value) }

// ScopeSet is the effective access of one account inside one knowledge base.
type ScopeSet struct {
	WholeRead      bool
	WholeWrite     bool
	ReadDocuments  map[string]struct{}
	WriteDocuments map[string]struct{}
	// WholeUpdate reports whether every document may be updated (replaced).
	// A whole-library write grant implies it; a group grant can set it alone.
	WholeUpdate bool
	// WholeDelete reports whether every document may be deleted. A
	// whole-library write grant implies it; a group grant can set it alone.
	WholeDelete bool
	// UpdateDocuments are the explicitly replaceable document names beyond
	// WholeUpdate and the write set.
	UpdateDocuments map[string]struct{}
	// DeleteDocuments are the explicitly deletable document names beyond
	// WholeDelete and the write set.
	DeleteDocuments map[string]struct{}
}

// NewScopeSet returns an empty (no access) scope.
func NewScopeSet() ScopeSet {
	return ScopeSet{
		ReadDocuments:   map[string]struct{}{},
		WriteDocuments:  map[string]struct{}{},
		UpdateDocuments: map[string]struct{}{},
		DeleteDocuments: map[string]struct{}{},
	}
}

// fullScope is the owner/administrator scope: every document, read and write.
func fullScope() ScopeSet {
	return ScopeSet{WholeRead: true, WholeWrite: true,
		ReadDocuments: map[string]struct{}{}, WriteDocuments: map[string]struct{}{},
		UpdateDocuments: map[string]struct{}{}, DeleteDocuments: map[string]struct{}{}}
}

// AllowsAnyRead reports whether the account may read anything in the knowledge
// base.
func (s ScopeSet) AllowsAnyRead() bool {
	return s.WholeRead || len(s.ReadDocuments) > 0 || s.WholeWrite
}

// HasAnyWrite reports whether the account may modify anything in the knowledge
// base.
func (s ScopeSet) HasAnyWrite() bool {
	return s.WholeWrite || len(s.WriteDocuments) > 0
}

// ReadsWhole reports whether the account may read every document.
func (s ScopeSet) ReadsWhole() bool { return s.WholeRead || s.WholeWrite }

// WritesWhole reports whether the account may modify every document.
func (s ScopeSet) WritesWhole() bool { return s.WholeWrite }

// CanCreate reports whether the account may create (upload) new documents.
func (s ScopeSet) CanCreate() bool { return s.WholeWrite }

// CanReadDocument reports whether one document name is readable.
func (s ScopeSet) CanReadDocument(name string) bool {
	if s.ReadsWhole() {
		return true
	}
	if _, ok := s.ReadDocuments[name]; ok {
		return true
	}
	if _, ok := s.WriteDocuments[name]; ok {
		return true
	}
	if _, ok := s.UpdateDocuments[name]; ok {
		return true
	}
	_, ok := s.DeleteDocuments[name]
	return ok
}

// CanWriteDocument reports whether one document name is writable (the union of
// replace and delete rights).
func (s ScopeSet) CanWriteDocument(name string) bool {
	return s.CanUpdateDocument(name) || s.CanDeleteDocument(name)
}

// CanUpdateDocument reports whether one document name may be replaced.
func (s ScopeSet) CanUpdateDocument(name string) bool {
	if s.WholeWrite || s.WholeUpdate {
		return true
	}
	if _, ok := s.WriteDocuments[name]; ok {
		return true
	}
	_, ok := s.UpdateDocuments[name]
	return ok
}

// CanDeleteDocument reports whether one document name may be deleted.
func (s ScopeSet) CanDeleteDocument(name string) bool {
	if s.WholeWrite || s.WholeDelete {
		return true
	}
	if _, ok := s.WriteDocuments[name]; ok {
		return true
	}
	_, ok := s.DeleteDocuments[name]
	return ok
}

// ReadDocumentNames returns the explicitly readable document names, sorted.
func (s ScopeSet) ReadDocumentNames() []string {
	seen := make(map[string]struct{}, len(s.ReadDocuments)+len(s.WriteDocuments)+
		len(s.UpdateDocuments)+len(s.DeleteDocuments))
	names := make([]string, 0, len(seen))
	for name := range s.ReadDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	for name := range s.WriteDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	for name := range s.UpdateDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	for name := range s.DeleteDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// WriteDocumentNames returns the explicitly writable document names, sorted.
func (s ScopeSet) WriteDocumentNames() []string {
	seen := make(map[string]struct{}, len(s.WriteDocuments)+len(s.UpdateDocuments)+len(s.DeleteDocuments))
	names := make([]string, 0, len(seen))
	for name := range s.WriteDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	for name := range s.UpdateDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	for name := range s.DeleteDocuments {
		if _, ok := seen[name]; !ok {
			seen[name] = struct{}{}
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// AllowsCreate reports whether at least one whole-library source allows
// creating documents (upload). Only the unified entry merges this; legacy
// file-level write keeps replacing only.
func (s ScopeSet) AllowsCreate() bool { return s.WholeWrite }

// UpdateDocumentNames returns the explicitly replaceable document names,
// sorted.
func (s ScopeSet) UpdateDocumentNames() []string {
	names := make([]string, 0, len(s.UpdateDocuments))
	for name := range s.UpdateDocuments {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// DeleteDocumentNames returns the explicitly deletable document names, sorted.
func (s ScopeSet) DeleteDocumentNames() []string {
	names := make([]string, 0, len(s.DeleteDocuments))
	for name := range s.DeleteDocuments {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Restricted reports whether reads must be limited to an explicit document set.
func (s ScopeSet) Restricted() bool { return !s.ReadsWhole() }

// DocumentAllowMask returns the group-merged allow bits of one document: the
// union of every whole-library bit and this document's file-level bits.
func (s ScopeSet) DocumentAllowMask(name string) int {
	mask := 0
	if s.WholeRead {
		mask |= PermissionBitRead
	}
	if s.WholeWrite {
		mask |= PermissionBitRead | PermissionBitCreate | PermissionBitUpdate | PermissionBitDelete
	}
	if s.WholeUpdate {
		mask |= PermissionBitUpdate
	}
	if s.WholeDelete {
		mask |= PermissionBitDelete
	}
	if name == "" {
		return mask
	}
	if _, ok := s.ReadDocuments[name]; ok {
		mask |= PermissionBitRead
	}
	if _, ok := s.WriteDocuments[name]; ok {
		mask |= PermissionBitRead | PermissionBitUpdate | PermissionBitDelete
	}
	if _, ok := s.UpdateDocuments[name]; ok {
		mask |= PermissionBitUpdate
	}
	if _, ok := s.DeleteDocuments[name]; ok {
		mask |= PermissionBitDelete
	}
	return mask
}

// EntitlementProvider reports whether an account's current personal membership
// includes the "may issue new share invitations" entitlement. The access layer
// keeps its role contract (admin/vip) and asks this provider for paid plans, so
// a purchase never mutates an account role.
type EntitlementProvider interface {
	CanShare(ctx context.Context, userID string) (bool, error)
}

// Options configures a Service.
type Options struct {
	// Now overrides the clock in tests.
	Now func() time.Time
	// InvitationTTL is how long one share link stays acceptable.
	InvitationTTL time.Duration
	// Entitlements supplies membership-based sharing for Plus/Pro accounts.
	// Nil keeps the historical role-only behaviour.
	Entitlements EntitlementProvider
}

// Service resolves access and implements knowledge-base sharing.
type Service struct {
	users          *state.UserRepository
	knowledgeBases *state.KnowledgeBaseRepository
	permissions    *state.PermissionRepository
	invitations    *state.InvitationRepository
	documents      *state.DocumentRepository
	entitlements   EntitlementProvider
	now            func() time.Time
	invitationTTL  time.Duration

	groups     *state.UserGroupRepository
	members    *state.UserGroupMemberRepository
	groupInvites *state.GroupInvitationRepository
	groupPerms   *state.KBGroupPermissionRepository
}

// NewService builds the access service on the shared state stores.
func NewService(stores *state.Stores, options Options) *Service {
	clock := options.Now
	if clock == nil {
		clock = time.Now
	}
	ttl := options.InvitationTTL
	if ttl <= 0 {
		ttl = 7 * 24 * time.Hour
	}
	service := &Service{
		users:          stores.Users,
		knowledgeBases: stores.KnowledgeBases,
		permissions:    stores.Permissions,
		invitations:    stores.Invitations,
		documents:      stores.Documents,
		entitlements:   options.Entitlements,
		now:            clock,
		invitationTTL:  ttl,
	}
	if stores.UserGroups != nil {
		service.groups = stores.UserGroups
		service.members = stores.UserGroupMembers
		service.groupInvites = stores.GroupInvitations
		service.groupPerms = stores.GroupPermissions
	}
	return service
}

// CanShareAccount reports whether the account may issue new share invitations:
// administrators and the existing manual VIP role always may, and Plus/Pro
// members may through their active paid entitlement. A purchase or expiry never
// changes the stored role.
func (s *Service) CanShareAccount(ctx context.Context, user state.User) (bool, error) {
	if !user.Enabled {
		return false, nil
	}
	if user.CanShare() {
		return true, nil
	}
	if s.entitlements == nil {
		return false, nil
	}
	return s.entitlements.CanShare(ctx, user.ID)
}

// scopeContextKey carries one request's resolved visibility into lower layers.
type scopeContextKey struct{}

// ContextWithScope attaches a resolved scope to a request context. The MCP
// endpoint reads it through ScopeFromContext so REST and MCP enforce the same
// document visibility.
func ContextWithScope(ctx context.Context, scope ScopeSet) context.Context {
	return context.WithValue(ctx, scopeContextKey{}, scope)
}

// ScopeFromContext returns the scope attached by ContextWithScope.
func ScopeFromContext(ctx context.Context) (ScopeSet, bool) {
	scope, ok := ctx.Value(scopeContextKey{}).(ScopeSet)
	return scope, ok
}

// Owner returns the owner account id of one knowledge base ("" when unowned or
// missing).
func (s *Service) Owner(ctx context.Context, namespace string) (string, error) {
	return s.knowledgeBases.OwnerOf(ctx, namespace)
}

// CanManage reports whether the account may administer sharing for one
// knowledge base: administrators may manage every knowledge base, VIPs only the
// ones they own.
func (s *Service) CanManage(ctx context.Context, user state.User, namespace string) (bool, error) {
	if !user.Enabled {
		return false, nil
	}
	if user.IsAdmin() {
		return true, nil
	}
	owner, err := s.knowledgeBases.OwnerOf(ctx, namespace)
	if err != nil {
		return false, err
	}
	return owner != "" && owner == user.ID, nil
}

// ScopeSet computes the effective access of one account inside one knowledge
// base. Administrators and owners get full access; everyone else gets the union
// of their personal grants and, through their current effective memberships,
// the allow bits of every group grant.
func (s *Service) ScopeSet(ctx context.Context, user state.User, namespace string) (ScopeSet, error) {
	if !user.Enabled {
		return NewScopeSet(), nil
	}
	if user.IsAdmin() {
		return fullScope(), nil
	}
	owner, err := s.knowledgeBases.OwnerOf(ctx, namespace)
	if err != nil {
		return NewScopeSet(), err
	}
	if owner != "" && owner == user.ID {
		return fullScope(), nil
	}
	grants, err := s.permissions.ListFor(ctx, namespace, user.ID)
	if err != nil {
		return NewScopeSet(), err
	}
	scope := NewScopeSet()
	for _, grant := range grants {
		write := grant.Permission == state.PermissionWrite
		if grant.DocumentName == "" {
			if write {
				scope.WholeWrite = true
			} else {
				scope.WholeRead = true
			}
			continue
		}
		if write {
			scope.WriteDocuments[grant.DocumentName] = struct{}{}
		} else {
			scope.ReadDocuments[grant.DocumentName] = struct{}{}
		}
	}
	// Group allow bits: every current effective membership contributes its
	// whole-library and per-document masks, all merged by union.
	groupScope, err := s.groupScope(ctx, user.ID, namespace)
	if err != nil {
		return NewScopeSet(), err
	}
	scope = scope.mergedWithGroup(groupScope)
	return scope, nil
}

// mergedWithGroup unions one group-merged allow-mask view into a personal
// scope and returns the result.
func (s ScopeSet) mergedWithGroup(group DocumentMaskScope) ScopeSet {
	if group.WholeMask&PermissionBitRead != 0 {
		s.WholeRead = true
	}
	if group.WholeMask&PermissionBitCreate != 0 {
		// create only ever applies at whole-library level; it also implies
		// whole update/delete for the unified entry (a whole-write grant
		// keeps the historical meaning).
		s.WholeWrite = true
	}
	if group.WholeMask&PermissionBitUpdate != 0 {
		s.WholeUpdate = true
	}
	if group.WholeMask&PermissionBitDelete != 0 {
		s.WholeDelete = true
	}
	for name, mask := range group.Documents {
		if mask&PermissionBitRead != 0 {
			s.ReadDocuments[name] = struct{}{}
		}
		if mask&PermissionBitUpdate != 0 {
			s.UpdateDocuments[name] = struct{}{}
		}
		if mask&PermissionBitDelete != 0 {
			s.DeleteDocuments[name] = struct{}{}
		}
	}
	return s
}

// DocumentMaskScope is the group contribution to one namespace: the whole-
// library allow bits and per-document allow bits of every applicable group
// rule.
type DocumentMaskScope struct {
	WholeMask int
	Documents map[string]int
}

// groupScope reads the group rules of one account inside one knowledge base.
// Only active memberships participate: leaving, being removed, having a group
// authorization revoked, or dissolving the group removes its contribution from
// the next request on.
func (s *Service) groupScope(ctx context.Context, userID, namespace string) (DocumentMaskScope, error) {
	group := DocumentMaskScope{Documents: map[string]int{}}
	if s.groups == nil || s.groupPerms == nil || s.members == nil {
		return group, nil
	}
	groupIDs, err := s.members.ActiveGroupIDsForUser(ctx, userID)
	if err != nil {
		return group, err
	}
	if len(groupIDs) == 0 {
		return group, nil
	}
	grants, err := s.groupPerms.ListForGroups(ctx, namespace, groupIDs)
	if err != nil {
		return group, err
	}
	for _, grant := range grants {
		if grant.DocumentName == "" {
			group.WholeMask |= grant.AllowMask
			continue
		}
		group.Documents[grant.DocumentName] |= grant.AllowMask
	}
	return group, nil
}

// CanShareKnowledgeBase reports whether the account may create invitations and
// manage grants for one knowledge base. Only VIPs and administrators may share,
// and a VIP only for knowledge bases they created.
func (s *Service) CanShareKnowledgeBase(ctx context.Context, user state.User, namespace string) (bool, error) {
	canShare, err := s.CanShareAccount(ctx, user)
	if err != nil || !canShare {
		return false, err
	}
	return s.CanManage(ctx, user, namespace)
}

// RequireManagePermission allows an owner or administrator to inspect and
// revoke existing sharing even after a paid membership expires. It deliberately
// does not check the "new invitation" entitlement: expiry must stop new paid
// invitations without trapping previously shared libraries.
func (s *Service) RequireManagePermission(ctx context.Context, user state.User, namespace string) error {
	manage, err := s.CanManage(ctx, user, namespace)
	if err != nil {
		return err
	}
	if !manage {
		return NewError(KindForbidden, MessageNotOwner)
	}
	return nil
}

// RequireSharePermission additionally requires the account to be allowed to
// issue new invitations (admin/vip or an active Plus/Pro entitlement).
func (s *Service) RequireSharePermission(ctx context.Context, user state.User, namespace string) error {
	canShare, err := s.CanShareAccount(ctx, user)
	if err != nil {
		return err
	}
	if !canShare {
		return NewError(KindForbidden, MessageForbidden)
	}
	return s.RequireManagePermission(ctx, user, namespace)
}

// VisibleNamespaces lists the knowledge bases one account may use at all:
// administrators see everything, everyone else sees owned knowledge bases plus
// knowledge bases with at least one grant.
func (s *Service) VisibleNamespaces(ctx context.Context, user state.User) ([]string, error) {
	all, err := s.knowledgeBases.Namespaces(ctx)
	if err != nil {
		return nil, err
	}
	if user.IsAdmin() {
		return all, nil
	}
	visible := make(map[string]struct{}, len(all))
	owned, err := s.knowledgeBases.OwnedNamespaces(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	for _, namespace := range owned {
		visible[namespace] = struct{}{}
	}
	grants, err := s.permissions.ListByGrantee(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	for _, grant := range grants {
		visible[grant.Namespace] = struct{}{}
	}
	// A group grant makes every granted namespace visible to its current
	// members: the account sees and can use the knowledge base even without a
	// personal grant.
	if s.members != nil && s.groupPerms != nil {
		groupIDs, err := s.members.ActiveGroupIDsForUser(ctx, user.ID)
		if err != nil {
			return nil, err
		}
		if len(groupIDs) > 0 {
			namespaces, err := s.groupPerms.NamespacesForGroups(ctx, groupIDs)
			if err != nil {
				return nil, err
			}
			for _, namespace := range namespaces {
				visible[namespace] = struct{}{}
			}
		}
	}
	names := make([]string, 0, len(visible))
	for _, namespace := range all {
		if _, ok := visible[namespace]; ok {
			names = append(names, namespace)
		}
	}
	return names, nil
}

// OwnedNamespaces lists the knowledge bases one account created. It is the
// binding set for account-owned credentials: an API key bypasses the
// knowledge-base/document authorization layer, so a normal account may only
// scope its keys to knowledge bases it owns.
func (s *Service) OwnedNamespaces(ctx context.Context, user state.User) ([]string, error) {
	if !user.Enabled {
		return []string{}, nil
	}
	namespaces, err := s.knowledgeBases.OwnedNamespaces(ctx, user.ID)
	if err != nil {
		return nil, err
	}
	if namespaces == nil {
		namespaces = []string{}
	}
	return namespaces, nil
}

// ResolveDocumentIDs maps document names to their indexed document ids inside
// one knowledge base. Names that have no document row are skipped.
func (s *Service) ResolveDocumentIDs(ctx context.Context, namespace string, names []string) ([]string, error) {
	ids := make([]string, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if name == "" {
			continue
		}
		record, err := s.documents.Get(ctx, namespace, name)
		if err != nil {
			return nil, err
		}
		if record == nil || record.DocumentID == "" {
			continue
		}
		if _, ok := seen[record.DocumentID]; ok {
			continue
		}
		seen[record.DocumentID] = struct{}{}
		ids = append(ids, record.DocumentID)
	}
	return ids, nil
}

// DocumentIDToName resolves one document id back to its name inside a namespace.
func (s *Service) DocumentIDToName(ctx context.Context, namespace, documentID string) (string, bool, error) {
	if documentID == "" {
		return "", false, nil
	}
	record, err := s.documents.GetByID(ctx, namespace, documentID)
	if err != nil {
		return "", false, err
	}
	if record == nil {
		return "", false, nil
	}
	return record.Name, true, nil
}

// IsDocumentNameValid applies the shared document-name rules used by grants.
func IsDocumentNameValid(name string) bool {
	if name == "" || len([]rune(name)) > 200 {
		return false
	}
	if name == "." || name == ".." {
		return false
	}
	return !strings.ContainsAny(name, "/\\\x00")
}

// asError unwraps a typed sharing failure.
func asError(err error) (*Error, bool) {
	var accessError *Error
	if errors.As(err, &accessError) {
		return accessError, true
	}
	return nil, false
}
