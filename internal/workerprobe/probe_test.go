package workerprobe

import (
	"context"
	"net/http/httptest"
	"testing"
)

func TestProbeLifecycle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	live, ready := false, false
	h := handler(ctx, func() (bool, bool) { return live, ready })
	check := func(path string, want int) {
		t.Helper()
		r := httptest.NewRecorder()
		h.ServeHTTP(r, httptest.NewRequest("GET", path, nil))
		if r.Code != want {
			t.Errorf("%s: %d, want %d", path, r.Code, want)
		}
	}
	check("/ready", 503)
	live = true
	check("/live", 200)
	check("/ready", 503)
	ready = true
	check("/ready", 200)
	cancel()
	check("/ready", 503)
	check("/live", 200)
}
