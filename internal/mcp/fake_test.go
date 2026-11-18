package mcp

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"sync"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/catalog"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
)

// newTestEndpoint builds an Endpoint with in-memory catalog storage and a
// discarded logger unless the caller overrides them.
func newTestEndpoint(t *testing.T, runtime Runtime, options Options) *Endpoint {
	t.Helper()
	options.Runtime = runtime
	if options.Catalog == nil {
		options.Catalog = catalog.NewMemoryStore()
	}
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	endpoint, err := NewEndpoint(options)
	if err != nil {
		t.Fatalf("NewEndpoint: %v", err)
	}
	return endpoint
}

// fakeRuntime is an in-memory Runtime that records the calls MCP made. Tests
// override the payload fields to drive rendering and error paths.
type fakeRuntime struct {
	mu sync.Mutex

	namespaces []string

	listDocuments []DocumentRecord
	listErr       error

	resolveCalls [][]string
	resolveErr   error

	readDocument DocumentContent
	readErr      error

	searchPayload map[string]any
	searchErr     error
	searchCalls   []contracts.SearchRequest

	artifactPayload map[string]any
	artifactErr     error
	artifactCalls   []fakeArtifactCall
	// artifactDocument is the parent document name the fake resolves for a
	// requested artifact; the document read gate decides on it.
	artifactDocument string

	addPayload map[string]any
	addErr     error
	addCalls   []string

	statusPayload map[string]any
	statusErr     error

	writePayload map[string]any
	writeErr     error
	writeCalls   []string

	deletePayload map[string]any
	deleteErr     error
	deleteCalls   []string
}

func newFakeRuntime() *fakeRuntime {
	return &fakeRuntime{}
}

func (f *fakeRuntime) ListDocuments(_ context.Context, namespace string) ([]DocumentRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	return append([]DocumentRecord(nil), f.listDocuments...), f.listErr
}

func (f *fakeRuntime) ReadDocument(_ context.Context, namespace, _ string) (DocumentContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	return f.readDocument, f.readErr
}

// ResolveDocumentIDs maps fake document names to stable ids so tests can assert
// the visibility restriction end to end.
func (f *fakeRuntime) ResolveDocumentIDs(_ context.Context, namespace string, names []string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	f.resolveCalls = append(f.resolveCalls, append([]string(nil), names...))
	if f.resolveErr != nil {
		return nil, f.resolveErr
	}
	ids := make([]string, 0, len(names))
	for _, name := range names {
		ids = append(ids, fakeDocumentID(name))
	}
	return ids, nil
}

// fakeDocumentID is the fake runtime's document-name to document-id mapping.
func fakeDocumentID(name string) string { return "doc:" + name }

func (f *fakeRuntime) Search(_ context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	f.searchCalls = append(f.searchCalls, request)
	if f.searchErr != nil {
		return nil, f.searchErr
	}
	return f.searchPayload, nil
}

func (f *fakeRuntime) GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	f.artifactCalls = append(f.artifactCalls, fakeArtifactCall{
		Namespace: namespace, ArtifactID: artifactID, Offset: offset, Length: length,
	})
	// The fake mirrors the real runtime: a document read gate denies an
	// artifact whose parent document the caller may not read.
	if gate := runtime.DocumentReadGate(ctx); gate != nil && !gate(f.artifactDocument) {
		return nil, &apierrors.ArtifactForbiddenError{Message: access.MessageKnowledgeAccess}
	}
	return f.artifactPayload, f.artifactErr
}

func (f *fakeRuntime) AddDocument(_ context.Context, namespace, name string, _ []byte, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	f.addCalls = append(f.addCalls, name)
	if f.addErr != nil {
		return nil, f.addErr
	}
	return f.addPayload, nil
}

func (f *fakeRuntime) GetDocumentStatus(_ context.Context, namespace, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	return f.statusPayload, f.statusErr
}

func (f *fakeRuntime) WriteDocument(_ context.Context, namespace, name string, _ []byte, _ bool, _ string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	f.writeCalls = append(f.writeCalls, name)
	if f.writeErr != nil {
		return nil, f.writeErr
	}
	return f.writePayload, nil
}

func (f *fakeRuntime) DeleteDocument(_ context.Context, namespace, name string) (map[string]any, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.namespaces = append(f.namespaces, namespace)
	f.deleteCalls = append(f.deleteCalls, name)
	if f.deleteErr != nil {
		return nil, f.deleteErr
	}
	return f.deletePayload, nil
}

// fakeArtifactCall records one Runtime.GetArtifact invocation.
type fakeArtifactCall struct {
	Namespace  string
	ArtifactID string
	Offset     int
	Length     int
}

// headerTransport injects the tenant headers the HTTP API layer would have
// authenticated for a real request.
type headerTransport struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t *headerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	cloned := request.Clone(request.Context())
	for name, value := range t.headers {
		cloned.Header.Set(name, value)
	}
	if t.base == nil {
		return http.DefaultTransport.RoundTrip(cloned)
	}
	return t.base.RoundTrip(cloned)
}
