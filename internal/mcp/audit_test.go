package mcp

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/audit"
)

type auditMessages chan audit.Entry

func (c auditMessages) Append(_ context.Context, entries []audit.Entry) error {
	for _, e := range entries {
		c <- e
	}
	return nil
}

func TestAuditUsesCurrentMCPRequestActor(t *testing.T) {
	events := make(auditMessages, 2)
	runtime := newFakeRuntime()
	runtime.readDocument = DocumentContent{Text: "private text", Version: "v1"}
	_, server := newTestHTTPHandler(t, runtime, Options{Audit: events})
	// Model the authenticated HTTP boundary with two principals sharing a legacy MCP session.
	authenticated := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor := "用户 alice"
		if r.Header.Get("Authorization") == "Bearer second" {
			actor = "用户 bob"
		}
		server.Config.Handler.ServeHTTP(w, r.WithContext(audit.WithActor(r.Context(), actor)))
	}))
	defer authenticated.Close()
	client := newRawRPC(t, authenticated.URL, manualsNS)
	client.initialize()
	_, payload := client.do(map[string]any{"jsonrpc": "2.0", "id": 10, "method": "resources/read", "params": map[string]any{"uri": "rag://knowledge/manuals/manual.txt"}}, map[string]string{"Authorization": "Bearer second", actorHeaderName: base64.RawURLEncoding.EncodeToString([]byte("spoofed"))})
	if payload["error"] != nil {
		t.Fatal(payload)
	}
	select {
	case e := <-events:
		if e.Actor != "用户 bob" || e.Action != "read_document" || e.Target != "manual.txt" || e.Channel != "MCP" {
			t.Fatalf("event=%+v", e)
		}
	default:
		t.Fatal("resource access was not audited")
	}
}
