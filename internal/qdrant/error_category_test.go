package qdrant

import "testing"

// TestAPIErrorLogMetadata pins the category and upstream status exposed to
// internal/logging.
func TestAPIErrorLogMetadata(t *testing.T) {
	err := &APIError{StatusCode: 500, Message: "Qdrant returned HTTP 500"}
	if got := err.ErrorCategory(); got != "qdrant_error" {
		t.Fatalf("ErrorCategory() = %q, want qdrant_error", got)
	}
	if got := err.HTTPStatus(); got != 500 {
		t.Fatalf("HTTPStatus() = %d, want 500", got)
	}

	// A malformed response without a status is a local failure.
	local := &APIError{Message: "Qdrant returned a malformed response"}
	if got := local.HTTPStatus(); got != 0 {
		t.Fatalf("local HTTPStatus() = %d, want 0", got)
	}
}
