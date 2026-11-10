# API Key access
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。


The console separates API Key management (`#/api-keys`), model settings
(`#/models`), and knowledge-base creation (`#/workspaces/create`).
Documents and search use `#/workspaces/documents` and `#/workspaces/search`,
with the knowledge base selected on the page.

## Managed keys

- Create a named key and select one or more namespaces. One namespace may be
  assigned to multiple keys. Empty namespace assignments are rejected.
- Every key is owned by the Console account that created it: `api_keys.owner_user_id`
  records the creator, and key JSON entries expose the optional
  `owner_user_id` / `owner_username` fields (`owner_username` is display-only;
  both are omitted for legacy unowned keys).
- Visibility follows ownership: an administrator sees every key, including
  legacy unowned ones; a `vip` or `user` account sees only the keys it created.
  Updating, deleting or exporting another account's key returns HTTP 404
  `API Key 不存在`.
- Namespace binding is limited for non-admins: a `vip` or `user` account may
  only bind namespaces whose knowledge base it owns
  (`knowledge_bases.owner_user_id`). Binding a namespace it does not own returns
  HTTP 403 with detail `只能为本人创建的知识库创建 API Key`. Administrators and
  legacy local management may bind any namespace.
- Lists show only each key's prefix. Each row has a Copy MCP JSON button.
  It copies an mcpServers object with one entry per authorized namespace,
  including the real Authorization header. Export is allowed for the key owner,
  for administrators, and for trusted direct local access.
- Requests send `Authorization: Bearer <API_KEY>` and
  `X-RAG-Namespace: <namespace>`. Each request addresses one namespace.
- Both REST and HTTP MCP check the stored assignment on each HTTP request.
  Disabling or deleting a key blocks subsequent requests; this does not cancel
  work or streams already accepted by the service.
- Metadata, authentication digests and recoverable protected keys are persisted
  in the shared state database (`api_keys` table; MySQL in production).
  Set `RAG_STATE_ENCRYPTION_KEY` so every service instance can decrypt the
  protected key; without it, Windows falls back to DPAPI and other platforms to
  plaintext, which is not portable across instances. Export responses are
  marked no-store.
- Deleting an account deletes the API keys it owns; applications still using
  those keys stop working immediately.
- Existing digest-only keys remain valid but cannot be displayed or recovered.
  Create a new key to generate a complete configuration; no keys are automatically
  replaced or revoked.

## Management access

- The five management routes (`GET/POST /v1/admin/api-keys`, `PUT/DELETE
  /v1/admin/api-keys/{key_id}`, `GET /v1/admin/api-keys/{key_id}/mcp-config`)
  accept a Console login session of **any** role for the keys that session owns
  (cookie + `X-CSRF-Token` on writes, or a Bearer session token). Their paths are
  unchanged for compatibility even though they are no longer admin-only.
- Without a session, only trusted local loopback management is accepted (legacy
  break-glass behaviour). That mode sees every key, including legacy unowned
  ones, and creating a key there produces an unowned key.
- All other `/v1/admin/*` routes keep their previous rule: direct loopback or an
  administrator session.
- Without a configured service token, direct local REST requests can manage the
  service and access workspaces without a key. HTTP MCP always requires an
  Authorization header, including localhost requests. Remote and forwarded
  anonymous requests are rejected.
- The existing environment `RAG_SERVICE_TOKEN` is a full-service credential;
  it is not a managed, namespace-scoped key. Do not distribute it to clients
  that should only access selected namespaces.
- Tenant authentication by a managed key is unchanged: the key's namespace
  whitelist still applies, and request-time knowledge-base ownership is not
  re-checked for managed keys.
- Model settings, knowledge-base creation and MCP management still require
  direct local access; only the API Key routes also accept logged-in accounts as
  described above. Remote and forwarded management requests are rejected.
