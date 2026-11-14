# internal/mcp — MCP Streamable HTTP layer

Port of `rag_mcp/server.py`, `rag_mcp/document_tools.py`, the MCP-facing part of
`rag_mcp/catalog.py`, `rag_mcp/resources.py` and `rag_mcp/mcp_http.py` on top of
the official Go SDK `github.com/modelcontextprotocol/go-sdk/mcp v1.7.0`.

## Public surface

| File | Responsibility |
| --- | --- |
| `runtime.go` | Consumer-side `Runtime` interface (no `internal/runtime` import), `DocumentRecord`, `Options`, namespace context helpers, shared error sentinels/constants. |
| `tools.go` | Ordered tool registry (`search`, `add_document`, `update_document`, `delete_document`, `get_document_status` — 5 tools, fixed order), argument validation ported from `server.py`/`document_tools.py`, synchronous search execution, tool-error rendering and image-asset extraction. Update/delete publish document change notifications. |
| `prompts.go` | Python-compatible `prompts/list` pagination (numeric cursor, 9 digits, page size 50) and `prompts/get` template rendering. |
| `resources.go` | `resources/list` + `resources/templates/list` + `resources/read` (paragraph pages and artifact byte chunks), artifact URI parsing, subscription validation and `Endpoint.DocumentsChanged(before, after)` notifications. |
| `endpoint.go` | `Endpoint`: lazily builds one `*mcp.Server` + stateful/stateless `StreamableHTTPHandler` pair per namespace, catalog-aware listing middleware, change-notification refresh, `internal/httpapi.ToolRegistry`/`ChangeNotifier` implementations. |
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
stateful legacy handler. Each namespace owns separate stateful sessions, so a
legacy `Mcp-Session-Id` used with another namespace's headers resolves to
`404 Not Found`.

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

## Tools / prompts / resources

* Tool names, descriptions, JSON Schemas, annotations and tool ordering come
  from the Go contract: `search`, `add_document`, `update_document`,
  `delete_document`, `get_document_status` (5 tools). Validation and
  runtime failures are `CallToolResult.IsError` text, never protocol errors;
  unknown or disabled tools are tool errors with the Python-compatible message.
* Search is **always synchronous**: one `tools/call` runs retrieval + rerank and
  returns evidence (`results`/`artifacts`/`evidence_status`/`answer_guidance`/
  `debug`) directly. There is no `wait_seconds`, no `result_id`, no polling and
  no `get_search_result` tool (removed 2026-11-13). Scope filtering still returns
  `DocumentIDs` to the runtime; restricted scopes now execute the synchronous
  search instead of being rejected.
* `prompts/list` keeps Python pagination (numeric cursor `"50"`, page size 50)
  and `prompts/get` renders `{{argument}}` placeholders with the Python error
  semantics.
* `resources/list` returns the namespace documents (URI via
  `resources.DocumentURI`, `application/json`, paragraph-page description) with
  the same pagination; `resources/templates/list` returns the knowledge-page and
  artifact-chunk templates; `resources/read` returns a JSON text content block:
  * document URIs call `Runtime.ReadDocument` and `resources.ResourcePage`
    (version-conflict, missing-page and version-required errors included);
  * `rag://artifacts/<ns>/<uuid>?offset&length` is parsed with the strict
    `_parse_artifact_uri` rules (canonical UUID, no control chars/fragment,
    canonical namespace, canonical integer query, `ValidateArtifactRange`) and
    calls `Runtime.GetArtifact`, decorating the payload with `next_uri`.
* Subscriptions: `SubscribeHandler` validates "canonical document URI of an
  existing document", then the SDK keeps the per-session subscription bookkeeping
  for both eras (`resources/subscribe` for legacy, `subscriptions/listen` for
  modern). `UnsubscribeHandler` is a no-op like Python.
* `Endpoint.DocumentsChanged(namespace, before, after)` ports
  `McpHttpEndpoint.documents_changed`: identical name→record maps publish
  nothing; otherwise `resources/list_changed` (via SDK resource registration
  Add/RemoveResources) plus `resources/updated` for every old document that
  changed or disappeared. MCP `update_document`/`delete_document` call it with
  the pre-write document list and the runtime's post-write `documents` list.

## Known parity gaps

1. **JSON text formatting** in tool results and resource contents: Go emits
   compact JSON with sorted keys (`json.Encoder`, no HTML escaping); Python
   `json.dumps` uses `", "`/`": "` separators and preserves runtime key order.
   Payloads are semantically identical; tests parse JSON instead of comparing
   bytes.
2. **Cursor encoding**: `prompts/list` and `resources/list` deliberately keep the
   Python numeric cursor; the SDK's opaque gob cursors/pagination are bypassed by
   a receiving middleware. `tools/list` is served by the middleware too (Python
   ordering, no pagination). An explicitly empty-string cursor pages from the
   start instead of erroring (the typed SDK params cannot distinguish absent from
   empty).
3. **JSON-RPC error bodies**: application failures keep the Python message text,
   but where Python raised `MCPError` the Go SDK may use a slightly different
   message (e.g. unknown prompt wording). Tool failures never become protocol
   errors.
4. **SEP-2575 error statuses and the Go SDK client**: for protocol `>= 2026-07-28`
   INVALID_PARAMS responses must be HTTP 400 with a JSON-RPC error body. The Go
   SDK *server* does this correctly (verified with raw HTTP), but the v1.7.0 Go
   SDK *client* tears down its session on non-2xx responses, so Go-client tests
   assert modern application errors through raw HTTP and legacy tests through the
   SDK/legacy path. Python clients handle these responses normally.
5. **Artifact offset range**: the offset/length are decoded with `strconv.Atoi`
   (64-bit); Python accepts arbitrary precision integers. Values beyond int64 are
   rejected as invalid chunk parameters.
6. **Cancellation**: `StreamableHTTPOptions.PropagateRequestCancellation` is
   enabled for synchronous search, so a cancelled POST cancels the running
   handler; the service derives a bounded deadline from `RAG_SEARCH_TIMEOUT_SECONDS`
   and propagates cancellation to Qdrant and model calls.
7. **Legacy session lifecycle**: the SDK handler has no exported "forget session"
   hook; stateful sessions are dropped by DELETE/idle timeout only, and stale
   entries in the SDK's resource-subscription map are pruned only by session
   close. Python additionally forgot sessions on notification delivery failures.
8. **server/discover** is answered by the SDK; Python's `auto` client behaviour
   is preserved by the SDK client (discover first, legacy initialize fallback).

## Deferred TODOs (next pass)

* Real `internal/runtime` integration: the `Runtime` adapter must return
  `DocumentRecord` maps (`{"name": ...}` plus metadata) and post-write
  `"documents"` lists; REST document upload/delete routes must call
  `Endpoint.DocumentsChanged(before, after)` (the HTTP API layer has no
  document-change notifier hook yet).
* Verify end-to-end cancellation of synchronous inference now that
  `PropagateRequestCancellation` is enabled (the service derives its deadline
  from `RAG_SEARCH_TIMEOUT_SECONDS` and forwards cancellation to Qdrant/model
  calls; no in-process work shielding remains).
* Optimize `DocumentsChanged` resource re-registration for very large document
  sets (currently every change re-registers the full document set).
