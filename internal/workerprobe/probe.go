package workerprobe

import (
	"context"
	"net"
	"net/http"
	"os"
	"sync/atomic"
	"time"
)

// Pulse is advanced by the monitored loop, including its idle polls.
type Pulse struct{ last atomic.Int64 }

func (p *Pulse) Beat() { p.last.Store(time.Now().UnixNano()) }
func (p *Pulse) Recent(window time.Duration) bool {
	last := p.last.Load()
	return last != 0 && time.Since(time.Unix(0, last)) < window
}

// Serve only binds a port when configured, so multiple host workers can share
// a machine. The Kubernetes manifests give each Pod its own listener.
func Serve(ctx context.Context, status func() (live, ready bool)) (func(), error) {
	addr := os.Getenv("RAG_WORKER_PROBE_ADDR")
	if addr == "" {
		return func() {}, nil
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	server := &http.Server{ReadHeaderTimeout: 3 * time.Second, Handler: handler(ctx, status)}
	go func() { _ = server.Serve(listener) }()
	return func() { _ = server.Close() }, nil
}
func handler(ctx context.Context, status func() (bool, bool)) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /live", func(w http.ResponseWriter, r *http.Request) {
		live, _ := status()
		if !live && ctx.Err() == nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, r *http.Request) {
		live, ready := status()
		if !live || !ready || ctx.Err() != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	return mux
}
