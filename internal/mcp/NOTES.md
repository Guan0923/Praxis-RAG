# internal/mcp — MCP Streamable HTTP layer

Port of `rag_mcp/server.py`, `rag_mcp/document_tools.py`, the MCP-facing part of
`rag_mcp/catalog.py` and `rag_mcp/mcp_http.py` on top of the official Go SDK
`github.com/modelcontextprotocol/go-sdk/mcp v1.7.0`.

## Public surface

| File | Responsibility |
| --- | --- |
| `runtime.go` | Consumer-side `Runtime` interface (no `internal/runtime` import), `Options`, namespace context helpers, shared error sentinels/constants. |
| `tools.go` | Ordered tool registry (search, get_search_result, add/update/delete_document, get_document_status), argument validation ported from `server.py`/`document_tools.py`, tool-error rendering and image-asset extraction. |
| `prompts.go` | Python-compatible `prompts/list` pagination (numeric cursor, 9 digits, page size 50) and `prompts/get` template rendering. |
| `endpoint.go` | `Endpoint`: lazily builds one `*mcp.Server` + stateful/stateless `StreamableHTTPHandler` pair per namespace, catalog-aware `tools/list`, change-notification refresh, `internal/httpapi.ToolRegistry`/`ChangeNotifier` implementations. |
| `http.go` | `/mcp` `http.Handler`: tenant namespace from request context (or an `Option` callback), Python-equivalent Content-Type/Host/Origin checks, request body bound, dual-era protocol dispatch. |

## Protocol eras

The Go SDK only serves protocol `>= 2026-07-28` (SEP-2575, sessionless) on a
**stateless** Streamable HTTP handler and legacy protocols (`<= 2025-11-25`) on
a **stateful** handler. `Handler.ServeHTTP` routes each POST:

1. `Mcp-Protocol-Version >= 2026-07-28` header → stateless;
2. else `Mcp-Session-Id` header present → stateful;
3. else inspect the JSON-RPC envelope: `server/discover` method or
   `_meta["io.modelcontextprotocol/protocolVersion"] >= 2026-07-28` → stateless;
4. else → stateful (legacy initialize).

GET (standalone SSE stream) and DELETE (session teardown) always go to the
stateful legacy handler.

Each namespace owns separate stateful sessions, so a legacy `Mcp-Session-Id`
used with another namespace's headers resolves to `404 Not Found` (parity with
`test_namespace_sessions_and_subscription_filters`).

## Transport security / auth

* The namespace must be injected by the HTTP API layer: default getter is
  `NamespaceFromContext`, override with `HTTPOptions.NamespaceFromRequest`
  (`httpapi.NamespaceFromContext` fits directly). `ErrUnauthorized` → 401 with
  `WWW-Authenticate: Bearer`; `ErrForbidden` → 403; `ErrInvalidNamespace` → 400.
* POST without `Content-Type: application/json…` → 400 `Invalid Content-Type header`.
* Host not matching `AllowedHosts` → 421 `Invalid Host header`;
  present but non-matching `Origin` → 403 `Invalid Origin header`.
  Patterns follow the Python middleware: exact value or `"<base>:*"` prefix
  match. An absent Origin is allowed.
* Bodies are bounded to `6*10 MiB + 64 KiB`; oversized → 413.

## Notifications

`Endpoint.ToolsChanged(namespace)` / `PromptsChanged(namespace)` implement
`internal/httpapi.ChangeNotifier`. They are no-ops for namespaces without a live
transport, mirroring Python's `if peer: publish(...)`. Refresh uses SDK
`AddTool`/`AddPrompt` so the SDK emits list-changed notifications to legacy
sessions (standalone SSE) and to modern sessions that opened
`subscriptions/listen` (implemented by the SDK). Document change notifications
(`resources/list_changed`, `resources/updated`) are **not** ported yet.

## Known parity gaps

1. **JSON text formatting** in tool results: Go emits compact JSON with sorted
   keys (`json.Encoder`, no HTML escaping); Python `json.dumps` uses `", "`/`": "`
   separators and preserves runtime key order. Payloads are semantically
   identical; tests parse JSON instead of comparing bytes.
2. **Cursor encoding**: `prompts/list` deliberately keeps the Python numeric
   cursor (`"50"`); the SDK's opaque gob cursors/pagination are bypassed by a
   receiving middleware. `tools/list` is served by the middleware too (Python
   ordering: search, get_search_result, add_document, update_document,
   delete_document, get_document_status; the SDK would sort registrations).
3. **Error bodies**: JSON-RPC level failures use the Go SDK's envelopes/messages
   where Python raised `MCPError` (e.g. unknown prompt message text, invalid
   cursor text). Tool validation/runtime failures keep the Python text exactly
   and are always `CallToolResult.IsError`, never protocol errors.
4. **Cancellation**: `StreamableHTTPOptions.PropagateRequestCancellation` is left
   off (default), so a cancelled POST does not cancel a running tool handler for
   modern requests; queued search still bounds its own wait and shields
   broker admission with `context.WithoutCancel`. Python used `CancelScope`.
5. **Legacy session lifecycle**: the SDK handler has no exported "forget session"
   hook; stateful sessions are dropped by DELETE/idle timeout only. Python
   additionally forgot sessions on notification delivery failures.
6. **resources/\***: `resources/list`, `resources/templates/list`,
   `resources/read`, `resources/subscribe`/`unsubscribe` and artifact URIs are
   deferred (todo list below). The `resources` capability is therefore not
   advertised yet.
7. **server/discover** is answered by the SDK; Python's `auto` client behaviour
   is preserved by the SDK client (discover first, legacy initialize fallback).

## Deferred TODOs (next pass)

* Resources: `resources/list` (documents, page size 50), resource templates
  (`rag://knowledge/<ns>/{name}{?page,version}` and artifact chunk template),
  `resources/read` via `Runtime.ReadDocument`/`Runtime.GetArtifact` +
  `internal/resources.ResourcePage`.
* Subscriptions: `ServerOptions.SubscribeHandler`/`UnsubscribeHandler` with
  subscription validation (document URI only, document must exist), plus
  `Endpoint.DocumentsChanged(before, after)` publishing
  `resources/list_changed` and per-URI `resources/updated`.
* Artifact URI parsing (`rag://artifacts/<ns>/<uuid>?offset&length`) and
  `ValidateArtifactRange` wiring.
* Document mutations should return the post-write document list to the
  notification path instead of dropping `"documents"`.
* Cancellation semantics for synchronous inference already running in the
  runtime (mirror Python's "cancelling the wait never terminates the work").
