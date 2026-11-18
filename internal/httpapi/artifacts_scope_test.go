package httpapi

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
)

// gateRuntime wraps fakeServiceRuntime so the artifact handlers run against a
// runtime that enforces the request's document read gate exactly like the real
// service: the gate decides whether the artifact's parent document is readable,
// and the observed gate is recorded for assertions.
type gateRuntime struct {
	*fakeServiceRuntime
	// artifactDocument is the parent document name the fake resolves for the
	// requested artifact.
	artifactDocument string

	contentGate  func(string) bool
	downloadGate func(string) bool
}

func (f *gateRuntime) GetArtifact(ctx context.Context, namespace, artifactID string, offset, length int) (map[string]any, error) {
	f.contentGate = runtime.DocumentReadGate(ctx)
	if gate := f.contentGate; gate != nil && !gate(f.artifactDocument) {
		return nil, &apierrors.ArtifactForbiddenError{Message: access.MessageKnowledgeAccess}
	}
	return f.fakeServiceRuntime.GetArtifact(ctx, namespace, artifactID, offset, length)
}

func (f *gateRuntime) DownloadArtifact(ctx context.Context, namespace, artifactID string) ([]byte, string, string, error) {
	f.downloadGate = runtime.DocumentReadGate(ctx)
	if gate := f.downloadGate; gate != nil && !gate(f.artifactDocument) {
		return nil, "", "", &apierrors.ArtifactForbiddenError{Message: access.MessageKnowledgeAccess}
	}
	return f.fakeServiceRuntime.DownloadArtifact(ctx, namespace, artifactID)
}

// TestArtifactEndpointsEnforceDocumentScope pins the document-level permission
// rule of the two REST artifact entries: a restricted file-level scope may only
// read artifacts of readable documents, and management credentials plus
// whole-knowledge-base scopes keep the unrestricted historical behaviour.
func TestArtifactEndpointsEnforceDocumentScope(t *testing.T) {
	artifactID := "123e4567-e89b-12d3-a456-426614174000"
	users := &fakeUserAuth{sessions: map[string]*auth.SessionUser{
		"user-token":  userSession("user-token"),
		"admin-token": adminSession("admin-token"),
	}}
	control := &fakeAccessControl{scopes: map[string]access.ScopeSet{
		"u-user|alpha":  readFilesScope("a.md"),
		"u-admin|alpha": fullTestScope(),
	}}
	service := &gateRuntime{
		fakeServiceRuntime: &fakeServiceRuntime{
			artifactPayload: map[string]any{"offset": 0, "length": 4, "data": "aGVsbG8="},
			downloadBody:    []byte("hello"),
			downloadName:    "b.md",
			downloadMime:    "text/markdown",
		},
		artifactDocument: "b.md",
	}
	handler := newAccessTestServer(t, users, control, service).Handler()
	contentTarget := "/v1/artifacts/" + artifactID + "/content?offset=0&length=20"
	downloadTarget := "/v1/artifacts/" + artifactID

	// The restricted account has one read grant on "a.md"; the artifact belongs
	// to "b.md", so both entries answer the document access denial.
	deniedContent := performRequest(handler, testRequest{
		method: "GET", target: contentTarget, remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if deniedContent.Code != http.StatusForbidden || decodeBody(t, deniedContent)["detail"] != access.MessageKnowledgeAccess {
		t.Fatalf("denied content status = %d body = %s", deniedContent.Code, deniedContent.Body.String())
	}
	if got := deniedContent.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("denied Cache-Control = %q", got)
	}
	deniedDownload := performRequest(handler, testRequest{
		method: "GET", target: downloadTarget, remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if deniedDownload.Code != http.StatusForbidden || decodeBody(t, deniedDownload)["detail"] != access.MessageKnowledgeAccess {
		t.Fatalf("denied download status = %d body = %s", deniedDownload.Code, deniedDownload.Body.String())
	}
	if service.contentGate == nil || service.downloadGate == nil {
		t.Fatalf("restricted scope did not attach the document gate")
	}
	if !service.contentGate("a.md") || service.contentGate("b.md") {
		t.Fatalf("content gate does not mirror the read scope")
	}

	// Grant the artifact's parent document: both entries succeed.
	control.scopes["u-user|alpha"] = readFilesScope("a.md", "b.md")
	grantedContent := performRequest(handler, testRequest{
		method: "GET", target: contentTarget, remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if grantedContent.Code != http.StatusOK {
		t.Fatalf("granted content status = %d body = %s", grantedContent.Code, grantedContent.Body.String())
	}
	grantedDownload := performRequest(handler, testRequest{
		method: "GET", target: downloadTarget, remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if grantedDownload.Code != http.StatusOK || grantedDownload.Body.String() != "hello" {
		t.Fatalf("granted download status = %d body = %s", grantedDownload.Code, grantedDownload.Body.String())
	}

	// A whole-knowledge-base scope keeps the unrestricted behaviour: no gate.
	adminContent := performRequest(handler, testRequest{
		method: "GET", target: contentTarget, remote: "10.0.0.9:5000",
		headers: sessionHeaders("admin-token", "alpha"),
	})
	if adminContent.Code != http.StatusOK {
		t.Fatalf("admin content status = %d body = %s", adminContent.Code, adminContent.Body.String())
	}
	if service.contentGate != nil {
		t.Fatalf("whole scope attached a gate, want unrestricted")
	}

	// Management credentials (service token, no session) stay unrestricted.
	tokenRuntime := &gateRuntime{
		fakeServiceRuntime: &fakeServiceRuntime{artifactPayload: map[string]any{"data": ""}},
		artifactDocument:   "b.md",
	}
	tokenHandler := New(testSettings("test-token"), nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)),
		WithRuntime(tokenRuntime)).Handler()
	tokenContent := performRequest(tokenHandler, testRequest{
		method: "GET", target: contentTarget, headers: tenantAuth(),
	})
	if tokenContent.Code != http.StatusOK {
		t.Fatalf("service-token content status = %d body = %s", tokenContent.Code, tokenContent.Body.String())
	}
	if tokenRuntime.contentGate != nil {
		t.Fatalf("service token attached a gate, want unrestricted")
	}

	// Without the access service the historical unrestricted behaviour is kept.
	legacy := &gateRuntime{
		fakeServiceRuntime: &fakeServiceRuntime{artifactPayload: map[string]any{"data": ""}},
		artifactDocument:   "b.md",
	}
	legacyHandler := newAccessTestServer(t, users, nil, legacy).Handler()
	legacyContent := performRequest(legacyHandler, testRequest{
		method: "GET", target: contentTarget, remote: "10.0.0.9:5000",
		headers: sessionHeaders("user-token", "alpha"),
	})
	if legacyContent.Code != http.StatusOK {
		t.Fatalf("legacy content status = %d body = %s", legacyContent.Code, legacyContent.Body.String())
	}
	if legacy.contentGate != nil {
		t.Fatalf("legacy mode attached a gate, want unrestricted")
	}
}
