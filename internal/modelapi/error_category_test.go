package modelapi

import "testing"

// TestModelAPIErrorLogMetadata pins the category and upstream status exposed to
// internal/logging: a provider HTTP failure keeps its status, a local failure
// reports 0 so it is not mistaken for a remote rejection.
func TestModelAPIErrorLogMetadata(t *testing.T) {
	remote := &ModelAPIError{Message: "model service returned HTTP 502", Status: 502}
	if got := remote.ErrorCategory(); got != "model_api_error" {
		t.Fatalf("ErrorCategory() = %q, want model_api_error", got)
	}
	if got := remote.HTTPStatus(); got != 502 {
		t.Fatalf("HTTPStatus() = %d, want 502", got)
	}

	local := modelError("embedding service timeout")
	if got := local.ErrorCategory(); got != "model_api_error" {
		t.Fatalf("local ErrorCategory() = %q, want model_api_error", got)
	}
	if got := local.HTTPStatus(); got != 0 {
		t.Fatalf("local HTTPStatus() = %d, want 0", got)
	}
}
