package httpapi

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

type sessionAdmin interface {
	ListSessionAccounts(context.Context, time.Time) ([]state.SessionAccount, error)
	ForceLogout(context.Context, string) (int64, error)
}

func WithSessionAdmin(store sessionAdmin) Option { return func(s *Server) { s.sessionAdmin = store } }

func (s *Server) handleSessionAccounts(w http.ResponseWriter, r *http.Request) {
	if s.sessionAdmin == nil {
		writeDetail(w, 503, "用户会话管理未启用")
		return
	}
	accounts, err := s.sessionAdmin.ListSessionAccounts(r.Context(), time.Now())
	if err != nil {
		writeDetail(w, 503, "无法读取用户会话")
		return
	}
	writeJSON(w, 200, map[string]any{"users": accounts})
}

func (s *Server) handleForceLogout(w http.ResponseWriter, r *http.Request) {
	if s.sessionAdmin == nil {
		writeDetail(w, 503, "用户会话管理未启用")
		return
	}
	id := pathParam(r, "user_id")
	if id == sessionUserID(r) {
		writeDetail(w, 400, "请使用退出登录结束自己的会话")
		return
	}
	count, err := s.sessionAdmin.ForceLogout(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeDetail(w, 404, auth.MessageUserNotFound)
		return
	}
	if err != nil {
		writeDetail(w, 503, "无法撤销用户会话")
		return
	}
	writeJSON(w, 200, map[string]any{"revoked_sessions": count})
}

// Only the route template is retained: concrete paths can contain invite tokens.
func (s *Server) recordRejection(r *http.Request, actor, action string) {
	if s.audit == nil {
		return
	}
	match, _, _ := s.router.match(r.Method, r.URL.Path)
	if match.route == nil {
		return
	}
	ctx := r.Context()
	if actor == "" {
		actor = audit.Actor(ctx)
	}
	if actor == "" {
		actor = "未认证请求"
	}
	ns, _ := NamespaceFromContext(ctx)
	audit.Record(audit.WithActor(ctx, actor), s.audit, s.logger, "REST", action, ns, audit.Target{Name: r.Method + " " + match.route.pattern, Outcome: "rejected"})
}

func WithAudit(store audit.Repository) Option { return func(s *Server) { s.audit = store } }
func sessionActor(session *auth.SessionUser) string {
	if session == nil {
		return "本机直连"
	}
	return "用户 " + session.User.Username + "（" + session.User.ID + "）"
}
func (s *Server) record(r *http.Request, action, ns, name, outcome string) {
	audit.Record(r.Context(), s.audit, s.logger, "REST", action, ns, audit.Target{Name: name, Outcome: outcome})
}
func (s *Server) handleAuditEvents(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if s.audit == nil {
		writeDetail(w, http.StatusServiceUnavailable, "操作审计未装配")
		return
	}
	limit, details := intQuery(r, "limit", 50, 1, 200)
	if details != nil {
		writeValidation(w, http.StatusBadRequest, details)
		return
	}
	q := r.URL.Query()
	filter := audit.Filter{
		Namespace: q.Get("namespace"), Actor: q.Get("actor"), Action: q.Get("action"),
		Target: q.Get("target"), Before: q.Get("before"), Outcome: q.Get("outcome"),
	}
	items, err := s.audit.List(r.Context(), filter, limit+1)
	if err != nil {
		writeDetail(w, http.StatusServiceUnavailable, "操作审计读取失败")
		return
	}
	next := ""
	if len(items) > limit {
		items = items[:limit]
		next = items[limit-1].ID
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items, "next_cursor": next})
}
