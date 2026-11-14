# Knowledge-base configuration
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。


Create Knowledge Base combines connection URL, API key, model selection and chunk
settings. Opening a model dropdown fetches models using that form's URL and key.
Embedding is required; Reranker is optional. Creation probes vector dimensions.

The sidebar Knowledge Bases page lists each registered configuration and opens an
editable drawer. Keys are masked; explicit reveal requires direct local access.
Each knowledge base stores independent protected credentials, models, dimensions
and collection in the shared `knowledge_bases` table (MySQL in production).
No global model settings are used.

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

## Model profiles vs. per-knowledge-base connections (2026-11-10)

The Console no longer types a provider connection into the create/edit
knowledge-base form. A dedicated 模型配置 page (`#/model-profiles`,
administrators only) maintains a reusable list of Embedding / Reranker / VLM /
OCR / JEV connections in the `model_profiles` table: `id`, `kind`, `name`, `base_url`,
`model`, `protected_key`, `vector_size`, `created_at`, `updated_at`, with a
unique `(kind, name)` pair. The key is encrypted with
`RAG_STATE_ENCRYPTION_KEY`; list and detail responses expose only
`api_key_set`. Saving an Embedding profile probes the provider once and records
the dense dimension in `vector_size`; a Reranker profile gets one bounded rerank
call; VLM is not probed.

The saved-model vocabulary also includes `jev` (TypeSafe JEV / System One
decision-model endpoint). A JEV profile stores name, base URL, model and the
encrypted key like a VLM profile, is never probed, has no provider `/models`
listing, and is not offered when creating or editing a knowledge base;
knowledge bases still copy only embedding/reranker/vlm/ocr connections.
The `retrieval: sag2` search mode resolves the most recently updated JEV
profile on every query and calls the hosted decision endpoint with the
candidate passages, so the query text and candidate chunks leave the service
toward the configured JEV provider.

A knowledge base still stores its own copy. Creating or editing a knowledge base
copies the selected profile's `base_url`, decrypted key and `model` into the
usual per-knowledge-base setting (the REST contract is unchanged and still takes
inline provider objects). Consequently:

- profile edits or deletes never mutate knowledge bases that already copied the
  connection — they keep working with the connection they saved;
- knowledge bases do not reference a profile by id, so a deleted profile simply
  disappears from future selections;
- the edit drawer preselects the profile matching the stored connection (same
  `kind`, `model` and base URL ignoring a trailing slash). When the stored
  connection is not in the list, it keeps a 当前配置 fallback option that saves
  the original connection and skips re-embedding;
- selecting a different Embedding connection still requires the re-embedding
  confirmation.

Model-profile administration uses `/v1/admin/model-profiles` (list, create,
details, PATCH, delete, plus `/{id}/key` and `/{id}/models`), restricted to local
admin access or an administrator Console session.

## Vision-model unification (2026-11-13)

PDF parsing no longer has a separate OCR engine/model type. The `ocr` kind is
removed from `model_profiles`; the saved-model vocabulary is now `embedding` /
`reranker` / `vlm` / `jev`, and the knowledge-base `vlm` connection is the only
PDF vision selection. Layout analysis, OCR, table structure, formula LaTeX and
picture descriptions all run through that one remote OpenAI-compatible vision
endpoint (the PDF service itself only reads/renders/crops locally).

The startup schema migration deletes legacy `kind='ocr'` profiles and strips the
`ocr`/`ocr_model` keys from `knowledge_bases.settings`; the write path also
removes those keys on knowledge-base update. The REST contract for knowledge
bases no longer carries `ocr_model`, and `/v1/admin/knowledge-bases/{namespace}`
responses omit it. Historical sections above describe the earlier 2026-11-10
state and are kept for migration context.
