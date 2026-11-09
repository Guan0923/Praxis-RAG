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

	"github.com/Guan0923/xinghe_rag/internal/state"
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

// ScopeSet is the effective access of one account inside one knowledge base.
type ScopeSet struct {
	WholeRead      bool
	WholeWrite     bool
	ReadDocuments  map[string]struct{}
	WriteDocuments map[string]struct{}
}

// NewScopeSet returns an empty (no access) scope.
func NewScopeSet() ScopeSet {
	return ScopeSet{ReadDocuments: map[string]struct{}{}, WriteDocuments: map[string]struct{}{}}
}

// fullScope is the owner/administrator scope: every document, read and write.
func fullScope() ScopeSet {
	return ScopeSet{WholeRead: true, WholeWrite: true,
		ReadDocuments: map[string]struct{}{}, WriteDocuments: map[string]struct{}{}}
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

// Restricted reports whether reads must be limited to an explicit document set.
func (s ScopeSet) Restricted() bool { return !s.ReadsWhole() }

// CanReadDocument reports whether one document name is readable.
func (s ScopeSet) CanReadDocument(name string) bool {
	if s.ReadsWhole() {
		return true
	}
	if _, ok := s.ReadDocuments[name]; ok {
		return true
	}
	_, ok := s.WriteDocuments[name]
	return ok
}

// CanWriteDocument reports whether one document name is writable.
func (s ScopeSet) CanWriteDocument(name string) bool {
	if s.WholeWrite {
		return true
	}
	_, ok := s.WriteDocuments[name]
	return ok
}

// ReadDocumentNames returns the explicitly readable document names, sorted.
func (s ScopeSet) ReadDocumentNames() []string {
	names := make([]string, 0, len(s.ReadDocuments)+len(s.WriteDocuments))
	for name := range s.ReadDocuments {
		names = append(names, name)
	}
	for name := range s.WriteDocuments {
		if _, ok := s.ReadDocuments[name]; !ok {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	return names
}

// WriteDocumentNames returns the explicitly writable document names, sorted.
func (s ScopeSet) WriteDocumentNames() []string {
	names := make([]string, 0, len(s.WriteDocuments))
	for name := range s.WriteDocuments {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Options configures a Service.
type Options struct {
	// Now overrides the clock in tests.
	Now func() time.Time
	// InvitationTTL is how long one share link stays acceptable.
	InvitationTTL time.Duration
}

// Service resolves access and implements knowledge-base sharing.
type Service struct {
	users          *state.UserRepository
	knowledgeBases *state.KnowledgeBaseRepository
	permissions    *state.PermissionRepository
	invitations    *state.InvitationRepository
	documents      *state.DocumentRepository
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
		now:            clock,
		invitationTTL:  ttl,
	}
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
// of their grants.
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
	return scope, nil
}

// CanShareKnowledgeBase reports whether the account may create invitations and
// manage grants for one knowledge base. Only VIPs and administrators may share,
// and a VIP only for knowledge bases they created.
func (s *Service) CanShareKnowledgeBase(ctx context.Context, user state.User, namespace string) (bool, error) {
	if !user.CanShare() {
		return false, nil
	}
	return s.CanManage(ctx, user, namespace)
}

// RequireSharePermission returns the typed error explaining why an account may
// not manage sharing for a knowledge base.
func (s *Service) RequireSharePermission(ctx context.Context, user state.User, namespace string) error {
	if !user.CanShare() {
		return NewError(KindForbidden, MessageForbidden)
	}
	manage, err := s.CanManage(ctx, user, namespace)
	if err != nil {
		return err
	}
	if !manage {
		return NewError(KindForbidden, MessageNotOwner)
	}
	return nil
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
