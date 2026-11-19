package apierrors

import "testing"

// categorizer mirrors the optional interface internal/logging uses, so this
// test fails to compile if a category method disappears or changes signature.
type categorizer interface{ ErrorCategory() string }

// TestErrorCategories pins the stable categories internal/logging persists in
// job error text and log records. They are low-cardinality by contract.
func TestErrorCategories(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"knowledge", &KnowledgeError{Message: "知识库暂时不可用"}, "knowledge_error"},
		{"validation", &KnowledgeValidationError{Message: "参数无效"}, "validation_error"},
		{"snapshot", &SnapshotUnavailable{Message: "快照已过期"}, "snapshot_unavailable"},
		{"artifact_forbidden", &ArtifactForbiddenError{Message: "无权访问该知识库"}, "artifact_forbidden"},
		{"upload_conflict", &UploadConflict{Message: "文档已存在"}, "upload_conflict"},
		{"knowledge_conflict", &KnowledgeConflictError{Message: "知识库冲突"}, "knowledge_conflict"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			categorized, ok := testCase.err.(categorizer)
			if !ok {
				t.Fatalf("%T does not implement ErrorCategory()", testCase.err)
			}
			if got := categorized.ErrorCategory(); got != testCase.want {
				t.Fatalf("ErrorCategory() = %q, want %q", got, testCase.want)
			}
		})
	}
}
