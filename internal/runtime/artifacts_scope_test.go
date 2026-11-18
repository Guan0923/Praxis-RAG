package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
)

// TestArtifactReadsEnforceDocumentGate covers the runtime artifact entries with
// a document-level read gate attached to the request context.
func TestArtifactReadsEnforceDocumentGate(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	insertReadableDocument(t, stores, "alpha", "report.pdf", "doc-a", "", nil)

	content := []byte("%PDF-1.7\nartifact bytes")
	artifact, err := stores.Artifacts.Register(ctx, "alpha", "doc-a", nil,
		"source", "report.pdf", "application/pdf", content, nil)
	if err != nil {
		t.Fatalf("Artifacts.Register() error = %v", err)
	}

	granted := map[string]bool{"report.pdf": true}
	allow := func(name string) bool { return granted[name] }

	// Without a gate (management credentials, whole-knowledge-base readers) the
	// historical unrestricted behaviour is preserved.
	payload, err := service.GetArtifact(ctx, "alpha", artifact.ArtifactID, 0, 1024)
	if err != nil || payload["name"] != "report.pdf" {
		t.Fatalf("GetArtifact(no gate) = %v, %v", payload, err)
	}
	body, mime, name, err := service.DownloadArtifact(ctx, "alpha", artifact.ArtifactID)
	if err != nil || string(body) != string(content) || mime != "application/pdf" || name != "report.pdf" {
		t.Fatalf("DownloadArtifact(no gate) = %q, %q, %q, %v", body, mime, name, err)
	}

	// The parent document is readable through the gate.
	gated := WithDocumentReadGate(ctx, allow)
	payload, err = service.GetArtifact(gated, "alpha", artifact.ArtifactID, 0, 1024)
	if err != nil || payload["total_bytes"] != float64(len(content)) {
		t.Fatalf("GetArtifact(granted) = %v, %v", payload, err)
	}
	body, _, _, err = service.DownloadArtifact(gated, "alpha", artifact.ArtifactID)
	if err != nil || string(body) != string(content) {
		t.Fatalf("DownloadArtifact(granted) = %q, %v", body, err)
	}

	// A gate that may not read the parent document denies both entries before
	// any byte is returned.
	denied := WithDocumentReadGate(ctx, func(string) bool { return false })
	_, err = service.GetArtifact(denied, "alpha", artifact.ArtifactID, 0, 1024)
	requireArtifactForbidden(t, err)
	_, _, _, err = service.DownloadArtifact(denied, "alpha", artifact.ArtifactID)
	requireArtifactForbidden(t, err)

	// The document-level gate uses the parent document name, not the artifact
	// name: a markdown side-car of a readable document stays readable even
	// though its own name ("document.md") is not a granted document name.
	sideCar, err := stores.Artifacts.Register(ctx, "alpha", "doc-a", nil,
		"markdown", "document.md", "text/markdown", []byte("# markdown"), nil)
	if err != nil {
		t.Fatalf("Artifacts.Register(side-car) error = %v", err)
	}
	payload, err = service.GetArtifact(gated, "alpha", sideCar.ArtifactID, 0, 1024)
	if err != nil || payload["name"] != "document.md" {
		t.Fatalf("GetArtifact(side-car) = %v, %v", payload, err)
	}

	// Deleting the parent document fails the gated read closed (the artifact
	// itself survives, matching the unrestricted historical behaviour).
	if _, err := stores.Documents.Delete(ctx, "alpha", "report.pdf"); err != nil {
		t.Fatalf("Documents.Delete() error = %v", err)
	}
	_, err = service.GetArtifact(gated, "alpha", artifact.ArtifactID, 0, 1024)
	requireArtifactForbidden(t, err)
	_, _, _, err = service.DownloadArtifact(gated, "alpha", artifact.ArtifactID)
	requireArtifactForbidden(t, err)
	body, _, _, err = service.DownloadArtifact(ctx, "alpha", artifact.ArtifactID)
	if err != nil || !strings.Contains(string(body), "artifact bytes") {
		t.Fatalf("DownloadArtifact(no gate, after delete) = %q, %v", body, err)
	}

	// Unknown or expired ids keep the frozen 410 contract for every caller.
	missing := strings.Repeat("0", 7) + "-0000-4000-8000-" + strings.Repeat("0", 12)
	_, err = service.GetArtifact(gated, "alpha", missing, 0, 1024)
	requireSnapshotUnavailable(t, err)
	_, _, _, err = service.DownloadArtifact(ctx, "alpha", missing)
	requireSnapshotUnavailable(t, err)

	// DocumentReadGate returns nil when no gate is attached; a nil gate passed
	// to WithDocumentReadGate also keeps the unrestricted behaviour.
	if gate := DocumentReadGate(ctx); gate != nil {
		t.Fatalf("DocumentReadGate(no ctx gate) is present, want nil")
	}
	if gate := DocumentReadGate(gated); gate == nil || !gate("report.pdf") || gate("other.md") {
		t.Fatalf("DocumentReadGate(gated) rounds trip incorrectly")
	}
	nilGate := WithDocumentReadGate(ctx, nil)
	if _, err := service.GetArtifact(nilGate, "alpha", artifact.ArtifactID, 0, 1024); err != nil {
		t.Fatalf("GetArtifact(nil gate) error = %v", err)
	}
}

func requireArtifactForbidden(t *testing.T, err error) {
	t.Helper()
	var forbidden *apierrors.ArtifactForbiddenError
	if !errors.As(err, &forbidden) {
		t.Fatalf("error = %v, want *apierrors.ArtifactForbiddenError", err)
	}
	// The state layer hard-codes access.MessageKnowledgeAccess (state cannot
	// import access), so the runtime denial wording is pinned here.
	if forbidden.Message != "无权访问该知识库" {
		t.Fatalf("message = %q, want the document access denial wording", forbidden.Message)
	}
}

func requireSnapshotUnavailable(t *testing.T, err error) {
	t.Helper()
	var unavailable *apierrors.SnapshotUnavailable
	if !errors.As(err, &unavailable) {
		t.Fatalf("error = %v, want *apierrors.SnapshotUnavailable", err)
	}
}
