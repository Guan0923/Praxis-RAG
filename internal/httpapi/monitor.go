package httpapi

import (
	"context"
	"net/http"
	"time"
)

// MonitorRuntime renders the read-only Console monitoring snapshot. The monitor
// package implements it; this package only owns the transport concerns.
type MonitorRuntime interface {
	MonitorOverview(ctx context.Context) map[string]any
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
