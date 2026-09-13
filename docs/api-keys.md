# API Key access

The console separates API Key management (`#/api-keys`), model settings
(`#/models`), and knowledge-base creation (`#/workspaces/create`).
Documents and search use `#/workspaces/documents` and `#/workspaces/search`,
with the knowledge base selected on the page.

## Managed keys

- Create a named key and select one or more namespaces. One namespace may be
  assigned to multiple keys. Empty namespace assignments are rejected.
- Lists show only each key's prefix. Each row has a Copy MCP JSON button.
  It copies an mcpServers object with one entry per authorized namespace,
  including the real Authorization header. Export requires direct local access.
- Requests send `Authorization: Bearer <API_KEY>` and
  `X-RAG-Namespace: <namespace>`. Each request addresses one namespace.
- Both REST and HTTP MCP check the stored assignment on each HTTP request.
  Disabling or deleting a key blocks subsequent requests; this does not cancel
  work or streams already accepted by the service.
- Management endpoints require direct local access, independently of managed keys.
- Metadata, authentication digests and recoverable protected keys are persisted
  in `RAG_SERVICE_DATA_ROOT/api-keys.sqlite3`. Windows uses the existing DPAPI
  protection; other platforms use the existing private-file protection with
  owner-only database permissions. Export responses are marked no-store.
- Existing digest-only keys remain valid but cannot be displayed or recovered.
  Create a new key to generate a complete configuration; no keys are automatically
  replaced or revoked.

## Management access

- Management is allowed only for direct loopback requests; no administrator credential is used.
- Without a configured service token, direct local REST requests can manage the
  service and access workspaces without a key. HTTP MCP always requires an
  Authorization header, including localhost requests. Remote and forwarded
  anonymous requests are rejected.
- The existing environment `RAG_SERVICE_TOKEN` is a full-service credential;
  it is not a managed, namespace-scoped key. Do not distribute it to clients
  that should only access selected namespaces.
- Model settings, API Key management, knowledge-base creation and MCP management
  require direct local access. Remote and forwarded management requests are rejected.
