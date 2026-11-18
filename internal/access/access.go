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
	MessageMaskBad          = "权限掩码必须是 0 到 15 之间的整数"
	MessageMaskCreateFile   = "新增权限只能设置在知识库级别，不能针对单个文件"
	MessageRuleMissing      = "授权规则不存在"
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

// ScopeSet is the effective access of one account inside one knowledge base,
// expressed with the operation bit masks of bitmask.go. It carries the raw
// rule state (role defaults, whole-library masks and per-file masks); every
// allows/denies question is answered by the shared merge in scope.go so
// REST, MCP and retrieval enforce the same judgement.
type ScopeSet struct {
	// DefaultAllow/DefaultDeny are the role defaults: administrators and the
	// knowledge-base owner start with every operation allowed, everyone else
	// starts with nothing. Both still participate in the deny merge, so an
	// explicit deny rule restricts administrators and owners too.
	DefaultAllow PermissionMask
	DefaultDeny  PermissionMask
	// WholeAllow/WholeDeny are the whole-knowledge-base rule masks (the
	// grant row with an empty document name).
	WholeAllow PermissionMask
	WholeDeny  PermissionMask
	// FileAllow/FileDeny are the per-document rule masks.
	FileAllow map[string]PermissionMask
	FileDeny  map[string]PermissionMask
}

// NewScopeSet returns an empty (no access) scope.
func NewScopeSet() ScopeSet {
	return ScopeSet{FileAllow: map[string]PermissionMask{}, FileDeny: map[string]PermissionMask{}}
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
	return &Service{
		users:          stores.Users,
		knowledgeBases: stores.KnowledgeBases,
		permissions:    stores.Permissions,
		invitations:    stores.Invitations,
		documents:      stores.Documents,
		entitlements:   options.Entitlements,
		now:            clock,
		invitationTTL:  ttl,
	}
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
// base. Administrators and the owner keep the historical default full-library
// grant, but they no longer short-circuit: their explicit rules are read and
// merged too, so a deny rule restricts them exactly like anyone else.
func (s *Service) ScopeSet(ctx context.Context, user state.User, namespace string) (ScopeSet, error) {
	if !user.Enabled {
		return NewScopeSet(), nil
	}
	scope := NewScopeSet()
	if user.IsAdmin() {
		scope.DefaultAllow = uint16(MaskAll)
	} else {
		owner, err := s.knowledgeBases.OwnerOf(ctx, namespace)
		if err != nil {
			return NewScopeSet(), err
		}
		if owner != "" && owner == user.ID {
			scope.DefaultAllow = uint16(MaskAll)
		}
	}
	grants, err := s.permissions.ListFor(ctx, namespace, user.ID)
	if err != nil {
		return NewScopeSet(), err
	}
	for _, grant := range grants {
		if grant.DocumentName == "" {
			scope.WholeAllow |= grant.AllowMask
			scope.WholeDeny |= grant.DenyMask
			continue
		}
		if scope.FileAllow == nil {
			scope.FileAllow = map[string]PermissionMask{}
		}
		if scope.FileDeny == nil {
			scope.FileDeny = map[string]PermissionMask{}
		}
		scope.FileAllow[grant.DocumentName] |= grant.AllowMask
		scope.FileDeny[grant.DocumentName] |= grant.DenyMask
	}
	return scope, nil
}

// ResolveVisibleDocumentIDs translates one scope into the server-side
// retrieval restriction: nil when retrieval is unrestricted, otherwise the
// document ids of every existing document whose read bit survives the merge
// (an empty non-nil slice means nothing is visible and retrieval returns no
// evidence at all).
func (s *Service) ResolveVisibleDocumentIDs(ctx context.Context, namespace string, scope ScopeSet) ([]string, error) {
	if !scope.Restricted() {
		return nil, nil
	}
	documents, err := s.documents.List(ctx, namespace)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(documents))
	seen := make(map[string]struct{}, len(documents))
	for _, document := range documents {
		if !scope.CanReadDocument(document.Name) {
			continue
		}
		if document.DocumentID == "" {
			continue
		}
		if _, duplicate := seen[document.DocumentID]; duplicate {
			continue
		}
		seen[document.DocumentID] = struct{}{}
		ids = append(ids, document.DocumentID)
	}
	return ids, nil
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
