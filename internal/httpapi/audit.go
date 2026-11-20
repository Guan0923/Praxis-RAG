package httpapi

import (
	"net/http"

	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
)

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
		Target: q.Get("target"), Before: q.Get("before"),
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
