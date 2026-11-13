package contracts

import "testing"

// TestErrorCategories pins the categories internal/logging persists. Untrusted
// MCP/HTTP arguments share the generic validation category; a broken retrieval
// component reports its own operation category.
func TestErrorCategories(t *testing.T) {
	validation := &SearchValidationError{Message: "top_k 必须是正整数"}
	if got := validation.ErrorCategory(); got != "validation_error" {
		t.Fatalf("SearchValidationError.ErrorCategory() = %q, want validation_error", got)
	}
	operation := NewRagOperationError("向量检索未配置")
	if got := operation.ErrorCategory(); got != "rag_operation_error" {
		t.Fatalf("RagOperationError.ErrorCategory() = %q, want rag_operation_error", got)
	}
}
