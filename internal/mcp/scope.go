package mcp

// Per-request visibility enforcement.
//
// httpapi authenticates /mcp requests and puts the caller's access.ScopeSet in
// the request context (access.ContextWithScope). The go-sdk does not expose that
// per-request context to tool/resource handlers on its stateful (legacy)
// transport: a legacy session is created by the initialize request and its
// handlers run on the session read goroutine with the *initialize* request's
// context. A request that reuses a session therefore sees the initializing
// account's context, not its own, which would both lose the restriction and leak
// a broader scope across accounts sharing a session.
//
// The HTTP adapter (Handler.ServeHTTP) therefore copies the resolved scope into
// an internal request header for every request. The SDK attaches the complete
// HTTP header set of each message as RequestExtra.Header, so handlers recover
// the exact per-request visibility on both transports. The header is always
// rewritten by the adapter (never trusted from the client); requests that bypass
// the adapter (unit tests, in-memory transports) fall back to the context scope.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"sort"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/Praxis-RAG/internal/access"
)

const (
	// scopeHeaderName is the internal header that carries one request's access
	// scope from the HTTP adapter to the SDK handlers.
	scopeHeaderName = "X-Rag-Mcp-Scope"
	// scopeHeaderNone marks an authenticated request that has no user scope (API
	// key, service token, session-less loopback): legacy unrestricted behavior.
	scopeHeaderNone = "none"

	// messageDocumentWriteRequired rejects a write tool targeting a document the
	// caller may not modify.
	messageDocumentWriteRequired = "没有该文档的修改权限"
)

// scopeEnvelope is the compact wire form of access.ScopeSet used only inside the
// process, between Handler.ServeHTTP and the SDK handlers.
type scopeEnvelope struct {
	WholeRead   bool     `json:"whole_read,omitempty"`
	WholeWrite  bool     `json:"whole_write,omitempty"`
	WholeUpdate bool     `json:"whole_update,omitempty"`
	WholeDelete bool     `json:"whole_delete,omitempty"`
	Read        []string `json:"read,omitempty"`
	Write       []string `json:"write,omitempty"`
	Update      []string `json:"update,omitempty"`
	Delete      []string `json:"delete,omitempty"`
}

// encodeScopeHeader renders one scope for the internal request header. The
// Write/Update/Delete lists keep their distinct meanings (full write,
// update-only, delete-only) so the decoder can rebuild the exact sets.
func encodeScopeHeader(scope access.ScopeSet) string {
	envelope := scopeEnvelope{
		WholeRead:   scope.WholeRead,
		WholeWrite:  scope.WholeWrite,
		WholeUpdate: scope.WholeUpdate,
		WholeDelete: scope.WholeDelete,
		Read:        documentSetNames(scope.ReadDocuments),
		Write:       documentSetNames(scope.WriteDocuments),
		Update:      scope.UpdateDocumentNames(),
		Delete:      scope.DeleteDocumentNames(),
	}
	encoded, err := json.Marshal(envelope)
	if err != nil {
		// Deny everything rather than fall back to the unrestricted default.
		return base64.RawURLEncoding.EncodeToString([]byte("{}"))
	}
	return base64.RawURLEncoding.EncodeToString(encoded)
}

// documentSetNames renders one document-name set, sorted.
func documentSetNames(set map[string]struct{}) []string {
	if len(set) == 0 {
		return nil
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// decodeScopeHeader parses a header written by encodeScopeHeader. It reports
// whether the request carries a user scope at all. A tampered or malformed value
// resolves to the empty (no access) scope instead of the unrestricted default.
func decodeScopeHeader(value string) (access.ScopeSet, bool) {
	if value == "" || value == scopeHeaderNone {
		return access.ScopeSet{}, false
	}
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return access.NewScopeSet(), true
	}
	var envelope scopeEnvelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return access.NewScopeSet(), true
	}
	scope := access.ScopeSet{
		WholeRead:      envelope.WholeRead,
		WholeWrite:     envelope.WholeWrite,
		WholeUpdate:    envelope.WholeUpdate,
		WholeDelete:    envelope.WholeDelete,
		ReadDocuments:  make(map[string]struct{}, len(envelope.Read)),
		WriteDocuments: make(map[string]struct{}, len(envelope.Write)),
		UpdateDocuments: make(map[string]struct{}, len(envelope.Update)),
		DeleteDocuments: make(map[string]struct{}, len(envelope.Delete)),
	}
	for _, name := range envelope.Read {
		if name != "" {
			scope.ReadDocuments[name] = struct{}{}
		}
	}
	for _, name := range envelope.Write {
		if name != "" {
			scope.WriteDocuments[name] = struct{}{}
		}
	}
	for _, name := range envelope.Update {
		if name != "" {
			scope.UpdateDocuments[name] = struct{}{}
		}
	}
	for _, name := range envelope.Delete {
		if name != "" {
			scope.DeleteDocuments[name] = struct{}{}
		}
	}
	return scope, true
}

// resolvedScope is the per-request visibility attached to a handler context. The
// presence flag distinguishes "no user scope" from "restricted to nothing".
type resolvedScope struct {
	scope   access.ScopeSet
	present bool
}

type scopeContextKey struct{}

// withResolvedScope attaches the request visibility to a handler context. It
// always stores the presence flag so downstream code never falls back to a
// stale session context on the stateful transport.
func withResolvedScope(ctx context.Context, scope access.ScopeSet, present bool) context.Context {
	return context.WithValue(ctx, scopeContextKey{}, resolvedScope{scope: scope, present: present})
}

// scopeFor returns the effective visibility of the current handler invocation.
// It prefers the value attached by the HTTP/request layer and otherwise falls
// back to access.ScopeFromContext (direct handler calls in tests).
func scopeFor(ctx context.Context) (access.ScopeSet, bool) {
	if resolved, ok := ctx.Value(scopeContextKey{}).(resolvedScope); ok {
		return resolved.scope, resolved.present
	}
	return access.ScopeFromContext(ctx)
}

// requestScope resolves one message's scope from its transport metadata. The
// internal header is authoritative because the HTTP adapter writes it on every
// request; the context is the fallback for transports without HTTP headers.
func requestScope(ctx context.Context, extra *sdk.RequestExtra) (access.ScopeSet, bool) {
	if extra != nil && extra.Header != nil {
		if value := extra.Header.Get(scopeHeaderName); value != "" {
			return decodeScopeHeader(value)
		}
	}
	return access.ScopeFromContext(ctx)
}

// withRequestScope attaches the visibility resolved from one message to ctx.
func withRequestScope(ctx context.Context, extra *sdk.RequestExtra) context.Context {
	scope, present := requestScope(ctx, extra)
	return withResolvedScope(ctx, scope, present)
}

// restrictedScope returns the scope only when reads must be limited to explicit
// documents. An absent scope (management credentials) or a whole-knowledge-base
// scope keeps the historical unrestricted behavior.
func restrictedScope(ctx context.Context) (access.ScopeSet, bool) {
	scope, ok := scopeFor(ctx)
	if !ok || !scope.Restricted() {
		return access.ScopeSet{}, false
	}
	return scope, true
}

// resolvedDocumentIDs translates the caller's readable document names into the
// document ids retrieval payloads carry. The empty set stays a non-nil slice so
// the runtime treats the search as restricted-to-nothing rather than as
// unrestricted.
func (e *Endpoint) resolvedDocumentIDs(ctx context.Context, namespace string, scope access.ScopeSet) ([]string, error) {
	ids, err := e.opts.Runtime.ResolveDocumentIDs(ctx, namespace, scope.ReadDocumentNames())
	if err != nil {
		return nil, fmt.Errorf("文档权限解析失败：%w", err)
	}
	if ids == nil {
		ids = []string{}
	}
	return ids, nil
}

// searchResultItems normalizes the result list shapes runtime adapters return:
// the HTTP runtime hands back JSON-decoded []any items, while in-process
// implementations may build []map[string]any directly.
func searchResultItems(raw any) []map[string]any {
	switch results := raw.(type) {
	case []any:
		items := make([]map[string]any, 0, len(results))
		for _, entry := range results {
			if item, ok := entry.(map[string]any); ok {
				items = append(items, item)
			}
		}
		return items
	case []map[string]any:
		return results
	default:
		return nil
	}
}
