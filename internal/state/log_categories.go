package state

// ErrorCategory labels this package's error types for internal/logging. The
// category is stable and low-cardinality, so log queries and job error text can
// distinguish "not found", "validation" and "conflict" failures instead of
// printing only the Go type name.
func (e *StateError) ErrorCategory() string { return "state_error" }

// ErrorCategory labels lookup failures.
func (e *LookupError) ErrorCategory() string { return "not_found" }

// ErrorCategory labels permission failures.
func (e *PermissionError) ErrorCategory() string { return "permission_error" }

// ErrorCategory labels invalid-value failures.
func (e *ValueError) ErrorCategory() string { return "invalid_value" }

// ErrorCategory labels document validation failures.
func (e *DocumentValidationError) ErrorCategory() string { return "validation_error" }

// ErrorCategory labels upload conflicts (name collision, stale execution).
func (e *UploadConflict) ErrorCategory() string { return "upload_conflict" }

// ErrorCategory labels encryption key failures.
func (e *CryptoError) ErrorCategory() string { return "crypto_error" }

// ErrorCategory labels an unavailable transient cache.
func (e *CacheUnavailable) ErrorCategory() string { return "cache_unavailable" }

// ErrorCategory labels an unavailable stored artifact.
func (e *ArtifactUnavailable) ErrorCategory() string { return "artifact_unavailable" }
