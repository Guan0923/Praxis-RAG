// Package wire binds the service runtime to the transport layers. The HTTP and
// MCP packages each declare their own consumer-side interfaces; this package
// contains the small adapters that convert between their types so the runtime
// itself stays transport-neutral.
package wire

import (
	"context"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/mcp"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
)

// HTTPRuntime adapts *runtime.Service to httpapi.Runtime.
type HTTPRuntime struct{ Service *runtime.Service }

// Search implements httpapi.SearchRuntime.
func (a HTTPRuntime) Search(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	return a.Service.Search(ctx, namespace, request)
}

// GetSearchResult implements httpapi.ResultRuntime.
func (a HTTPRuntime) GetSearchResult(ctx context.Context, namespace, resultID string, page, pageSize int) (map[string]any, error) {
	return a.Service.GetSearchResult(ctx, namespace, resultID, page, pageSize)
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

// Upload implements httpapi.DocumentRuntime.
func (a HTTPRuntime) Upload(ctx context.Context, namespace string, files []httpapi.UploadFile, idempotencyKey string) (map[string]any, error) {
	converted := make([]runtime.UploadFile, 0, len(files))
	for _, file := range files {
		converted = append(converted, runtime.UploadFile{Name: file.Name, Content: file.Content})
	}
	return a.Service.Upload(ctx, namespace, converted, idempotencyKey)
}

// Delete implements httpapi.DocumentRuntime.
func (a HTTPRuntime) Delete(ctx context.Context, namespace, name string) (map[string]any, error) {
	return a.Service.Delete(ctx, namespace, name)
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

// SubmitSearch implements mcp.Runtime.
func (a MCPRuntime) SubmitSearch(ctx context.Context, namespace string, request contracts.SearchRequest) (string, error) {
	return a.Service.SubmitSearch(ctx, namespace, request)
}

// GetSearchResult implements mcp.Runtime.
func (a MCPRuntime) GetSearchResult(ctx context.Context, namespace, resultID string, page, pageSize int) (map[string]any, error) {
	return a.Service.GetSearchResult(ctx, namespace, resultID, page, pageSize)
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
