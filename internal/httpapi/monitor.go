package httpapi

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/monitor"
)

// MonitorRuntime renders the read-only Console monitoring snapshot and the log
// view. The monitor package implements it; this package only owns the transport
// concerns.
type MonitorRuntime interface {
	MonitorOverview(ctx context.Context) map[string]any
	MonitorLogs(ctx context.Context, query monitor.LogQuery) (*monitor.LogsView, error)
}

// WithMonitorRuntime wires the monitoring snapshot handler.
func WithMonitorRuntime(runtime MonitorRuntime) Option {
	return func(s *Server) { s.monitor = runtime }
}

// handleMonitorOverview returns the read-only monitoring snapshot. The response
// is never cached and the runtime call is bounded so a slow dependency cannot
// hold the request open.
func (s *Server) handleMonitorOverview(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeDetail(w, http.StatusServiceUnavailable, "监控服务未启用")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	overview := s.monitor.MonitorOverview(ctx)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, overview)
}

// handleMonitorLogs returns one filtered page of the process log view
// (in-memory ring by default, or the startup-configured JSON Lines file). Query
// validation lives in internal/monitor: a wrapped ErrInvalidLogQuery becomes a
// 400 with the Chinese reason, anything else stays a 500.
func (s *Server) handleMonitorLogs(w http.ResponseWriter, r *http.Request) {
	if s.monitor == nil {
		writeDetail(w, http.StatusServiceUnavailable, "监控服务未启用")
		return
	}
	values := r.URL.Query()
	query := monitor.LogQuery{
		Source:    values.Get("source"),
		Component: values.Get("component"),
		Level:     values.Get("level"),
		Query:     values.Get("query"),
	}
	if raw := strings.TrimSpace(values.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			writeDetail(w, http.StatusBadRequest, "limit 必须是整数")
			return
		}
		query.Limit = limit
	}
	if raw := strings.TrimSpace(values.Get("offset")); raw != "" {
		offset, err := strconv.Atoi(raw)
		if err != nil {
			writeDetail(w, http.StatusBadRequest, "offset 必须是整数")
			return
		}
		query.Offset = offset
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	logs, err := s.monitor.MonitorLogs(ctx, query)
	if err != nil {
		if errors.Is(err, monitor.ErrInvalidLogQuery) {
			writeDetail(w, http.StatusBadRequest, err.Error())
			return
		}
		writeDetail(w, http.StatusInternalServerError, "日志查询失败")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, logs)
}
