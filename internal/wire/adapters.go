// Package wire binds the service runtime to the transport layers. The HTTP and
// MCP packages each declare their own consumer-side interfaces; this package
// contains the small adapters that convert between their types so the runtime
// itself stays transport-neutral.
package wire

import (
	"context"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/mcp"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
)

// HTTPRuntime adapts *runtime.Service to httpapi.Runtime.
type HTTPRuntime struct{ Service *runtime.Service }

// Compile-time checks for the consumer-side interfaces added by the ownership
// iteration: the tenant self-create route delegates to the runtime and the MCP
// endpoint needs the document visibility mapping.
var (
	_ httpapi.KnowledgeBaseSelfCreateRuntime = HTTPRuntime{}
	_ httpapi.ModelProfileRuntime            = HTTPRuntime{}
	_ mcp.Runtime                            = MCPRuntime{}
)

// Search implements httpapi.SearchRuntime.
func (a HTTPRuntime) Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	return a.Service.Search(ctx, namespace, request)
}

// GetArtifact implements httpapi.ArtifactRuntime.
func (a HTTPRuntime) GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error) {
	return a.Service.GetArtifact(ctx, namespace, artifactID, offset, length)
}

// DownloadArtifact implements httpapi.ArtifactRuntime.
func (a HTTPRuntime) DownloadArtifact(ctx context.Context, namespace, artifactID string) ([]byte, string, string, error) {
	return a.Service.DownloadArtifact(ctx, namespace, artifactID)
}

// ListDocuments implements httpapi.DocumentRuntime.
func (a HTTPRuntime) ListDocuments(ctx context.Context, namespace string) ([]map[string]any, error) {
	return a.Service.ListDocuments(ctx, namespace)
}

// GetDocumentStatus implements httpapi.DocumentRuntime.
func (a HTTPRuntime) GetDocumentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error) {
	return a.Service.GetDocumentStatus(ctx, namespace, artifactID)
}

// SubmitDocumentBatch implements httpapi.DocumentRuntime.
func (a HTTPRuntime) SubmitDocumentBatch(ctx context.Context, namespace string, files []httpapi.UploadFile, idempotencyKey, owner string) (map[string]any, error) {
	converted := make([]runtime.UploadFile, 0, len(files))
	for _, file := range files {
		converted = append(converted, runtime.UploadFile{Name: file.Name, Content: file.Content})
	}
	return a.Service.SubmitDocumentBatch(ctx, namespace, converted, idempotencyKey, owner)
}

// GetDocumentBatch implements httpapi.DocumentRuntime.
func (a HTTPRuntime) GetDocumentBatch(ctx context.Context, namespace, batchID, viewer string) (map[string]any, error) {
	return a.Service.GetDocumentBatch(ctx, namespace, batchID, viewer)
}

// ListDocumentBatches implements httpapi.DocumentRuntime.
func (a HTTPRuntime) ListDocumentBatches(ctx context.Context, namespace, viewer string, limit int) (map[string]any, error) {
	return a.Service.ListDocumentBatches(ctx, namespace, viewer, limit)
}

// Delete implements httpapi.DocumentRuntime.
func (a HTTPRuntime) Delete(ctx context.Context, namespace, name string) (map[string]any, error) {
	return a.Service.Delete(ctx, namespace, name)
}

// WriteDocument implements httpapi.DocumentRuntime: the Console replace
// endpoint reuses the synchronous MCP update_document write path.
func (a HTTPRuntime) WriteDocument(ctx context.Context, namespace, name string, content []byte, overwrite bool, idempotencyKey string) (map[string]any, error) {
	return a.Service.WriteDocument(ctx, namespace, name, content, overwrite, idempotencyKey)
}

// CheckDocuments implements httpapi.DocumentRuntime.
func (a HTTPRuntime) CheckDocuments(ctx context.Context, namespace string, files []httpapi.CheckFile) (map[string]any, error) {
	converted := make([]runtime.CheckFile, 0, len(files))
	for _, file := range files {
		converted = append(converted, runtime.CheckFile{Name: file.Name, SHA256: file.SHA256})
	}
	return a.Service.CheckDocuments(ctx, namespace, converted)
}

// ReadDocument implements httpapi.DocumentRuntime.
func (a HTTPRuntime) ReadDocument(ctx context.Context, namespace, name string) (string, string, error) {
	return a.Service.ReadDocument(ctx, namespace, name)
}

// ReadDocumentFile implements httpapi.DocumentRuntime.
func (a HTTPRuntime) ReadDocumentFile(ctx context.Context, namespace, name string) ([]byte, error) {
	return a.Service.ReadDocumentFile(ctx, namespace, name)
}

// PublicKnowledgeBase implements httpapi.KnowledgeBaseRuntime.
func (a HTTPRuntime) PublicKnowledgeBase(ctx context.Context, namespace string) (map[string]any, error) {
	return a.Service.PublicKnowledgeBase(ctx, namespace)
}

// MCPRuntime adapts *runtime.Service to mcp.Runtime.
type MCPRuntime struct{ Service *runtime.Service }

// ListDocuments implements mcp.Runtime.
func (a MCPRuntime) ListDocuments(ctx context.Context, namespace string) ([]mcp.DocumentRecord, error) {
	documents, err := a.Service.ListDocuments(ctx, namespace)
	if err != nil {
		return nil, err
	}
	records := make([]mcp.DocumentRecord, 0, len(documents))
	for _, document := range documents {
		records = append(records, mcp.DocumentRecord(document))
	}
	return records, nil
}

// ResolveDocumentIDs implements the mcp.Runtime document visibility mapping: it
// turns granted document names into the ids Qdrant payloads carry.
func (a MCPRuntime) ResolveDocumentIDs(ctx context.Context, namespace string, names []string) ([]string, error) {
	return a.Service.ResolveDocumentIDs(ctx, namespace, names)
}

// ReadDocument implements mcp.Runtime.
func (a MCPRuntime) ReadDocument(ctx context.Context, namespace, name string) (mcp.DocumentContent, error) {
	text, version, err := a.Service.ReadDocument(ctx, namespace, name)
	if err != nil {
		return mcp.DocumentContent{}, err
	}
	return mcp.DocumentContent{Text: text, Version: version}, nil
}

// Search implements mcp.Runtime.
func (a MCPRuntime) Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	return a.Service.Search(ctx, namespace, request)
}

// GetArtifact implements mcp.Runtime.
func (a MCPRuntime) GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error) {
	return a.Service.GetArtifact(ctx, namespace, artifactID, offset, length)
}

// AddDocument implements mcp.Runtime.
func (a MCPRuntime) AddDocument(ctx context.Context, namespace, name string, content []byte, idempotencyKey string) (map[string]any, error) {
	return a.Service.AddDocument(ctx, namespace, name, content, idempotencyKey)
}

// GetDocumentStatus implements mcp.Runtime.
func (a MCPRuntime) GetDocumentStatus(ctx context.Context, namespace, artifactID string) (map[string]any, error) {
	return a.Service.GetDocumentStatus(ctx, namespace, artifactID)
}

// WriteDocument implements mcp.Runtime.
func (a MCPRuntime) WriteDocument(ctx context.Context, namespace, name string, content []byte, overwrite bool, idempotencyKey string) (map[string]any, error) {
	return a.Service.WriteDocument(ctx, namespace, name, content, overwrite, idempotencyKey)
}

// DeleteDocument implements mcp.Runtime.
func (a MCPRuntime) DeleteDocument(ctx context.Context, namespace, name string) (map[string]any, error) {
	return a.Service.DeleteDocument(ctx, namespace, name)
}

// Notifier adapts the MCP endpoint to httpapi.ChangeNotifier, converting REST
// document lists to the MCP DocumentRecord type.
type Notifier struct{ Endpoint *mcp.Endpoint }

// ToolsChanged implements httpapi.ChangeNotifier.
func (n Notifier) ToolsChanged(namespace string) {
	if n.Endpoint != nil {
		n.Endpoint.ToolsChanged(namespace)
	}
}

// PromptsChanged implements httpapi.ChangeNotifier.
func (n Notifier) PromptsChanged(namespace string) {
	if n.Endpoint != nil {
		n.Endpoint.PromptsChanged(namespace)
	}
}

// DocumentsChanged implements httpapi.ChangeNotifier.
func (n Notifier) DocumentsChanged(namespace string, before, after []map[string]any) {
	if n.Endpoint == nil {
		return
	}
	n.Endpoint.DocumentsChanged(namespace, toRecords(before), toRecords(after))
}

func toRecords(items []map[string]any) []mcp.DocumentRecord {
	records := make([]mcp.DocumentRecord, 0, len(items))
	for _, item := range items {
		records = append(records, mcp.DocumentRecord(item))
	}
	return records
}

// UploadOperations implements httpapi.UploadStatusRuntime.
func (a HTTPRuntime) UploadOperations(ctx context.Context, namespace string) ([]map[string]any, error) {
	return a.Service.UploadOperations(ctx, namespace)
}

// UploadOperation implements httpapi.UploadStatusRuntime.
func (a HTTPRuntime) UploadOperation(ctx context.Context, namespace, operationID string) (map[string]any, bool, error) {
	return a.Service.UploadOperation(ctx, namespace, operationID)
}

// PublishedVersions implements httpapi.UploadStatusRuntime.
func (a HTTPRuntime) PublishedVersions(ctx context.Context, namespace string) (map[string]string, error) {
	return a.Service.PublishedVersions(ctx, namespace)
}

// UploadLeases implements httpapi.UploadStatusRuntime.
func (a HTTPRuntime) UploadLeases(ctx context.Context, namespace string) ([]map[string]any, error) {
	return a.Service.UploadLeases(ctx, namespace)
}

// ListKnowledgeBases implements httpapi.KnowledgeBaseAdminRuntime.
func (a HTTPRuntime) ListKnowledgeBases(ctx context.Context) ([]map[string]any, error) {
	return a.Service.ListKnowledgeBases(ctx)
}

// CreateKnowledgeBase implements httpapi.KnowledgeBaseAdminRuntime.
func (a HTTPRuntime) CreateKnowledgeBase(ctx context.Context, input httpapi.KnowledgeBaseCreateInput) (map[string]any, error) {
	return a.Service.CreateKnowledgeBase(ctx, kbconfig.CreateInput{
		Namespace:       input.Namespace,
		Chunking:        input.Chunking,
		EmbeddingModel:  input.EmbeddingModel,
		RerankerModel:   input.RerankerModel,
		EmbeddingValues: input.EmbeddingValues,
		RerankerValues:  input.RerankerValues,
		VLMModel:        input.VLMModel,
		VLMValues:       input.VLMValues,
		TokenizerModel:  a.Service.Config().ChunkTokenizerModel,
	})
}

// CreateKnowledgeBaseForUser implements httpapi.KnowledgeBaseSelfCreateRuntime.
// The tenant self-create request carries no provider configuration: the runtime
// clones the administrator default template for it.
func (a HTTPRuntime) CreateKnowledgeBaseForUser(
	ctx context.Context,
	ownerUserID string,
	isAdmin bool,
	input httpapi.KnowledgeBaseCreateInput,
) (map[string]any, error) {
	return a.Service.CreateKnowledgeBaseForUser(ctx, ownerUserID, isAdmin, kbconfig.CreateInput{
		Namespace:       input.Namespace,
		Chunking:        input.Chunking,
		EmbeddingModel:  input.EmbeddingModel,
		RerankerModel:   input.RerankerModel,
		EmbeddingValues: input.EmbeddingValues,
		RerankerValues:  input.RerankerValues,
		VLMModel:        input.VLMModel,
		VLMValues:       input.VLMValues,
		TokenizerModel:  a.Service.Config().ChunkTokenizerModel,
	})
}

// UpdateKnowledgeBase implements httpapi.KnowledgeBaseAdminRuntime.
func (a HTTPRuntime) UpdateKnowledgeBase(ctx context.Context, namespace string, input httpapi.KnowledgeBaseUpdateInput) (map[string]any, error) {
	return a.Service.UpdateKnowledgeBase(ctx, namespace, kbconfig.UpdateInput{
		Values:  input.Values,
		Reembed: input.Reembed,
	})
}

// KnowledgeBaseKey implements httpapi.KnowledgeBaseAdminRuntime.
func (a HTTPRuntime) KnowledgeBaseKey(ctx context.Context, namespace, kind string) (string, error) {
	return a.Service.KnowledgeBaseKey(ctx, namespace, kind)
}

// KnowledgeBaseModels implements httpapi.KnowledgeBaseAdminRuntime.
func (a HTTPRuntime) KnowledgeBaseModels(ctx context.Context, kind string, values map[string]any, namespace *string) ([]string, error) {
	return a.Service.KnowledgeBaseModels(ctx, kind, values, namespace)
}

// PurgeKnowledgeBase implements httpapi.KnowledgeBaseAdminRuntime.
func (a HTTPRuntime) PurgeKnowledgeBase(ctx context.Context, namespace string) (httpapi.KnowledgeBasePurgeResult, error) {
	result, err := a.Service.PurgeKnowledgeBase(ctx, namespace)
	if err != nil {
		return httpapi.KnowledgeBasePurgeResult{}, err
	}
	return httpapi.KnowledgeBasePurgeResult{
		Namespace:            result.Namespace,
		DocumentsDeleted:     result.DocumentsDeleted,
		FailedDocuments:      result.FailedDocuments,
		ConfigurationDeleted: result.ConfigurationDeleted,
	}, nil
}

// PurgeKnowledgeBases implements httpapi.KnowledgeBaseAdminRuntime: one batched
// purge transaction for the whole request.
func (a HTTPRuntime) PurgeKnowledgeBases(ctx context.Context, namespaces []string) ([]httpapi.KnowledgeBasePurgeResult, error) {
	results, err := a.Service.PurgeKnowledgeBases(ctx, namespaces)
	if err != nil {
		return nil, err
	}
	converted := make([]httpapi.KnowledgeBasePurgeResult, len(results))
	for index, result := range results {
		converted[index] = httpapi.KnowledgeBasePurgeResult{
			Namespace:            result.Namespace,
			DocumentsDeleted:     result.DocumentsDeleted,
			FailedDocuments:      result.FailedDocuments,
			ConfigurationDeleted: result.ConfigurationDeleted,
		}
	}
	return converted, nil
}

// ListModelProfiles implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) ListModelProfiles(ctx context.Context, viewerUserID string, isAdmin bool) ([]map[string]any, error) {
	return a.Service.ListModelProfiles(ctx, viewerUserID, isAdmin)
}

// ModelProfile implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) ModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string) (map[string]any, error) {
	return a.Service.ModelProfile(ctx, viewerUserID, isAdmin, id)
}

// CreateModelProfile implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) CreateModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, input httpapi.ModelProfileInput) (map[string]any, error) {
	return a.Service.CreateModelProfile(ctx, viewerUserID, isAdmin, modelProfileDraft(input))
}

// UpdateModelProfile implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) UpdateModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string, input httpapi.ModelProfileInput) (map[string]any, error) {
	return a.Service.UpdateModelProfile(ctx, viewerUserID, isAdmin, id, modelProfileDraft(input))
}

// DeleteModelProfile implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) DeleteModelProfile(ctx context.Context, viewerUserID string, isAdmin bool, id string) (bool, error) {
	return a.Service.DeleteModelProfile(ctx, viewerUserID, isAdmin, id)
}

// ModelProfileKey implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) ModelProfileKey(ctx context.Context, viewerUserID string, isAdmin bool, id string) (string, error) {
	return a.Service.ModelProfileKey(ctx, viewerUserID, isAdmin, id)
}

// ModelProfileModels implements httpapi.ModelProfileRuntime.
func (a HTTPRuntime) ModelProfileModels(ctx context.Context, viewerUserID string, isAdmin bool, id string) ([]string, error) {
	return a.Service.ModelProfileModels(ctx, viewerUserID, isAdmin, id)
}

// modelProfileDraft converts the transport-neutral input to the runtime draft.
func modelProfileDraft(input httpapi.ModelProfileInput) kbconfig.ModelProfileDraft {
	return kbconfig.ModelProfileDraft{
		Kind:      input.Kind,
		Name:      input.Name,
		BaseURL:   input.BaseURL,
		Model:     input.Model,
		APIKey:    input.APIKey,
		HasAPIKey: input.HasAPIKey,
	}
}
