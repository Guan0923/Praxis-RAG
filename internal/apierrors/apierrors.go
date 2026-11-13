// Package apierrors defines the error categories shared by the service layers.
// They mirror the Python exceptions that the HTTP layer maps to status codes:
// KnowledgeValidationError -> 400/404/409, KnowledgeError -> 503,
// SnapshotUnavailable -> 410, and UploadConflict/KnowledgeConflictError -> 409.
package apierrors

// KnowledgeError marks a safe service failure rendered as HTTP 503.
type KnowledgeError struct{ Message string }

// Error implements error.
func (e *KnowledgeError) Error() string { return e.Message }

// KnowledgeValidationError marks a user-input failure rendered as HTTP 400.
type KnowledgeValidationError struct{ Message string }

// Error implements error.
func (e *KnowledgeValidationError) Error() string { return e.Message }

// SnapshotUnavailable marks an expired or missing stored result snapshot
// (HTTP 410, never retried from SQL).
type SnapshotUnavailable struct{ Message string }

// Error implements error.
func (e *SnapshotUnavailable) Error() string { return e.Message }

// UploadConflict marks a colliding document name, queued upload or reused
// idempotency key (HTTP 409).
type UploadConflict struct{ Message string }

// Error implements error.
func (e *UploadConflict) Error() string { return e.Message }

// KnowledgeConflict marks a knowledge-base conflict that is not an input
// validation failure (HTTP 409). The tenant self-create flow uses it when the
// administrator default knowledge-base template is not configured.
type KnowledgeConflictError struct{ Message string }

// Error implements error.
func (e *KnowledgeConflictError) Error() string { return e.Message }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package. The value is stable and
// low-cardinality because it is persisted in job error text and log records.
func (e *KnowledgeError) ErrorCategory() string { return "knowledge_error" }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package.
func (e *KnowledgeValidationError) ErrorCategory() string { return "validation_error" }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package.
func (e *SnapshotUnavailable) ErrorCategory() string { return "snapshot_unavailable" }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package.
func (e *UploadConflict) ErrorCategory() string { return "upload_conflict" }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package.
func (e *KnowledgeConflictError) ErrorCategory() string { return "knowledge_conflict" }

// SnapshotUnavailableError builds a *SnapshotUnavailable.
func SnapshotUnavailableError(message string) *SnapshotUnavailable {
	return &SnapshotUnavailable{Message: message}
}

// IsSnapshotUnavailable reports whether err is a *SnapshotUnavailable.
func IsSnapshotUnavailable(err error) bool {
	_, ok := err.(*SnapshotUnavailable)
	return ok
}
