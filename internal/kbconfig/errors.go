// Package kbconfig ports the knowledge-base configuration layer from
// rag_mcp: per-base model connections, protected credential storage, public
// settings projection, and the create/update validation rules. It is the
// configuration half of rag_mcp/service_knowledge.py; storage and indexing stay
// in internal/runtime and internal/state.
package kbconfig

// Message constants are the user-facing Chinese strings raised by the Python
// implementation (KnowledgeValidationError). They are exported so the runtime
// layer can reuse the exact wording for its state-dependent checks.
const (
	// MessageNamespaceInvalid is raised when the namespace fails the required
	// pattern (rag_mcp.service_knowledge.create_knowledge_base).
	MessageNamespaceInvalid = "知识库标识无效"
	// MessageKnowledgeBaseExists is raised when a knowledge base already exists.
	MessageKnowledgeBaseExists = "该知识库已存在，请在知识库页面修改配置"
	// MessageNamespaceHasDocuments is raised when a namespace already owns
	// documents but has no configuration record.
	MessageNamespaceHasDocuments = "该 Namespace 已有文档，请使用新的知识库标识"
	// MessageEmbeddingModelRequired is raised when the embedding model is blank.
	MessageEmbeddingModelRequired = "请选择 Embedding 模型"
	// MessageKnowledgeBaseMissing is raised when no configuration record exists.
	MessageKnowledgeBaseMissing = "该知识库没有配置记录，请先创建知识库"
	// MessageEmbeddingChangeRequiresReembed is raised when the embedding URL or
	// model changes without a re-embedding request.
	MessageEmbeddingChangeRequiresReembed = "修改 Embedding URL 或模型前，必须重新向量化（re embedding）"
	// MessageVectorSizeRequiresReembed is raised when the probed embedding
	// dimension differs from the published index without a re-embedding request.
	MessageVectorSizeRequiresReembed = "Embedding 向量长度已改变，必须重新向量化（re embedding）"
	// MessagePublicationRolledBack is raised when a concurrent publication won
	// the configuration version check.
	MessagePublicationRolledBack = "知识库发布已回滚，请刷新配置后重试"
	// MessagePublicationUnconfirmed is raised when publication outcome cannot be
	// confirmed; the new index is retained and operators must inspect it.
	MessagePublicationUnconfirmed = "Knowledge-base publication outcome is unconfirmed; the new index was retained. " +
		"Check the current configuration before retrying."
	// MessageVLMURLRequired is raised when a VLM model is configured without a
	// service URL.
	MessageVLMURLRequired = "请填写 VLM 服务 URL"
	// MessageVLMModelRequired is raised when a VLM service URL is configured
	// without a model name.
	MessageVLMModelRequired = "请填写 VLM 模型名称"

	// MessageModelKindInvalid is raised when a saved model configuration names
	// an unsupported provider kind.
	MessageModelKindInvalid = "模型类型无效"
	// MessageModelProfileNameRequired is raised when a model configuration has
	// no display name.
	MessageModelProfileNameRequired = "请填写模型配置名称"
	// MessageModelProfileNameTooLong is raised when a model configuration name
	// exceeds 80 characters.
	MessageModelProfileNameTooLong = "模型配置名称最多 80 个字符"
	// MessageModelModelRequired is raised when a model configuration has no
	// provider model id.
	MessageModelModelRequired = "请填写模型名称"
	// MessageModelModelTooLong is raised when a model id exceeds 512 characters.
	MessageModelModelTooLong = "模型名称最多 512 个字符"
	// MessageModelServiceURLRequired is raised when a model configuration has no
	// service URL.
	MessageModelServiceURLRequired = "请填写服务地址"
	// MessageModelProfileExists is raised when another saved model configuration
	// already uses the same (kind, name) pair.
	MessageModelProfileExists = "该模型配置名称已存在"
	// MessageModelProfileMissing is raised when the requested model
	// configuration does not exist.
	MessageModelProfileMissing = "该模型配置不存在"
)

// Error mirrors KnowledgeError: a safe knowledge-base error that may occur after
// storage has been touched.
type Error struct{ Message string }

// Error implements error.
func (e *Error) Error() string { return e.Message }

// ValidationError mirrors KnowledgeValidationError: raised before an
// unsupported or unsafe operation changes storage.
type ValidationError struct{ Message string }

// Error implements error.
func (e *ValidationError) Error() string { return e.Message }

// NewValidationError builds a ValidationError with the supplied message.
func NewValidationError(message string) *ValidationError {
	return &ValidationError{Message: message}
}
