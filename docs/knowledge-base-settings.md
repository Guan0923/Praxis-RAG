# Knowledge-base configuration

Create Knowledge Base combines connection URL, API key, model selection and chunk
settings. Opening a model dropdown fetches models using that form's URL and key.
Embedding is required; Reranker is optional. Creation probes vector dimensions.

The sidebar Knowledge Bases page lists each registered configuration and opens an
editable drawer. Keys are masked; explicit reveal requires direct local access.
Each knowledge base stores independent protected credentials, models, dimensions
and collection in knowledge-bases.sqlite3. No global model settings are used.

Changing the Embedding URL, model or vector dimensions requires reembedding.
Existing chunks and assets are copied into a new collection with new embeddings.
Only after every chunk succeeds does a single database update activate the new
configuration. Failure preserves the original configuration and index. Original
source files and manifests are unchanged; the old collection is retained.
Key-only and Reranker changes do not rebuild the Embedding index.

- GET /v1/admin/knowledge-bases lists registered and populated namespaces.
- GET /v1/admin/knowledge-bases/defaults returns chunk strategies.
- POST /v1/admin/knowledge-bases/models/{embedding|reranker} fetches model names.
- POST /v1/admin/knowledge-bases creates a knowledge base with provider settings.
- GET /v1/admin/knowledge-bases/{namespace} returns redacted settings.
- PATCH /v1/admin/knowledge-bases/{namespace} edits settings; reembed=true rebuilds.
- GET /v1/admin/knowledge-bases/{namespace}/key/{kind} explicitly reveals a key.
- GET /v1/knowledge-base returns redacted settings for the authenticated namespace.

Management requires direct local access. Omitted keys retain saved credentials
only for the same URL; an empty key clears them. Keys never enter browser storage.
Legacy manifest-only namespaces remain listed without an editable configuration;
they are not automatically migrated or assigned inferred provider credentials.
