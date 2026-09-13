# Knowledge-base creation settings

Create a knowledge base in the connection page before uploading documents.
Creation requires administrator access, using the same rule as API Key management.

The stored settings are namespace, chunk strategy, chunk size, chunk overlap,
vector size, embedding model and tokenizer. They are persisted in
`RAG_SERVICE_DATA_ROOT/knowledge-bases.sqlite3`. There is no update endpoint.

The service currently shares one embedding model and Qdrant collection across
namespaces. The chosen vector size must match the saved, verified model output.
Configure the model before creating a knowledge base. Once a knowledge base is
created, changing the global embedding model or dimension is rejected, even if
that knowledge base is empty.

- `GET /v1/admin/knowledge-bases/defaults` returns creation defaults and strategies.
- `POST /v1/admin/knowledge-bases` creates the immutable settings.
- `GET /v1/knowledge-base` returns the current namespace settings after key authorization.
- Uploads accept files only. Preflight accepts only the file fingerprints.
- HTTP uploads and MCP document additions/updates use the stored chunk settings.

Existing documents are not migrated or reindexed. Namespaces with documents but
without saved knowledge-base settings remain readable; create a new namespace
for new uploads. Creation cannot overwrite an existing populated namespace.
