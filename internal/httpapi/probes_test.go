package httpapi

import (
	"context"
	"errors"
	"testing"
)

func TestProbeServiceDependency(t *testing.T) {
	s := newTestServer(t, testSettings("test-token"), nil)
	var dbError error
	WithReadiness(func(context.Context) error { return dbError })(s)
	check := func(path string, want int) {
		t.Helper()
		r := performRequest(s.Handler(), testRequest{method: "GET", target: path})
		if r.Code != want {
			t.Errorf("%s: %d, want %d", path, r.Code, want)
		}
	}
	check("/ready", 200)
	dbError = errors.New("unavailable")
	check("/ready", 503)
	check("/live", 200)
	check("/health", 200)
}
