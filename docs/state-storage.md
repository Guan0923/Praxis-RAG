# Shared state storage

Praxis-RAG keeps the retrieval index in Qdrant and every other durable object in
one shared relational database. The production target is MySQL 8.0+; a SQLite
file is still accepted for local development and the test suite.

## What is stored where

| Data | Storage |
| --- | --- |
| Chunks, dense/sparse vectors, asset metadata | Qdrant |
| Source documents (bytes) | `blobs` (`bucket = source`) |
| PDF/Word parse artifacts (`manifest.json`, `document.md`, `document.json`, `ocr.json`, images) | `blobs` (`bucket = pdf-assets` / `word-assets`) |
| Document manifest (name, hash, chunk count, indexing config) | `documents` |
| Knowledge-base settings and provider keys | `knowledge_bases` |
| MCP tool/prompt settings | `mcp_catalogs` |
| Service API keys | `api_keys` |

`RAG_SERVICE_DATA_ROOT` remains the default location only for the local
`state.sqlite3` fallback; it is no longer the authoritative document store.

## Configuration

Set either a full SQLAlchemy URL:

```text
RAG_STATE_URL=<SQLAlchemy MySQL URL, include charset=utf8mb4>
```

or the individual parts:

```text
RAG_MYSQL_HOST
RAG_MYSQL_PORT (default 3306)
RAG_MYSQL_USER (default rag)
RAG_MYSQL_DATABASE (default praxis_rag)
RAG_MYSQL_PASSWORD        # environment only
RAG_MYSQL_SSL_CA          # optional CA bundle path
```

Optional tuning: `RAG_STATE_POOL_SIZE`, `RAG_STATE_MAX_OVERFLOW`,
`RAG_STATE_POOL_RECYCLE`, `RAG_STATE_POOL_TIMEOUT`, `RAG_STATE_ECHO`, and
`RAG_STATE_AUTO_SCHEMA` (default on, creates missing tables with
`CREATE TABLE IF NOT EXISTS`). Disable auto schema when the database account is
not allowed to run DDL and create the tables separately. Auto schema only
creates missing tables; it does not alter existing columns, so apply schema
changes with a migration tool before rolling out a new version.

The MySQL account needs `SELECT`, `INSERT`, `UPDATE`, `DELETE` on the schema and
`CREATE` when auto schema is enabled. The server's `max_allowed_packet` must be
larger than the 10 MiB upload limit (16 MiB or more is recommended) because
source files and parse artifacts are written as single `LONGBLOB` values.

## Credential encryption

Provider keys and recoverable service API keys are encrypted before they are
written:

```text
RAG_STATE_ENCRYPTION_KEY=base64:<32-byte key>
```

The value is AES-GCM (`enc:v1:` prefix). Without the key, Windows falls back to
DPAPI and other platforms store a plaintext `private-file:` value; neither is
usable from another machine, so set the key for every instance. Existing
`dpapi:` / `private-file:` rows remain readable and are re-encrypted by the
migration CLI.

## Schema

| Table | Key | Purpose |
| --- | --- | --- |
| `api_keys` | `id`, unique `digest` | service credentials |
| `knowledge_bases` | `namespace` | committed KB settings, monotonic `version` |
| `documents` | `(namespace, name)` | per-document manifest |
| `blobs` | `(namespace, bucket, key)` | source bytes and parse artifacts (`LONGBLOB`) |
| `mcp_catalogs` | `namespace` | MCP tool/prompt settings |

Identifier columns (`namespace`, document `name`, `digest`, blob `key`) use a
binary collation in MySQL so `Alpha` and `alpha` do not collapse into one row.
`settings`, `diagnostics` and MCP catalog data are `LONGTEXT`; blobs are
`LONGBLOB`.

`knowledge_bases.version` increments on every update. Instances compare the
stored version before reusing a cached tenant, so a configuration change made on
one instance is picked up by the others without a restart.

## Local Docker stack

The repository ships `docker-compose.yml` with MySQL 8 and Qdrant. Copy
`.env.example` to `.env` (or let the start script do it), then fill in the
passwords and `RAG_STATE_ENCRYPTION_KEY`. `.env` is gitignored.

```powershell
.\scripts\start-stack.ps1 -MigrateOnly   # start MySQL/Qdrant and import legacy state
.\scripts\start-stack.ps1                # start dependencies, migrate and serve
.\scripts\stop-stack.ps1                 # stop containers, keep volumes
```

The Compose stack maps MySQL to `127.0.0.1:3307` by default to avoid colliding
with an existing 3306 instance; set `RAG_MYSQL_PORT` in `.env` to change it.
The MySQL user and database are created from `RAG_MYSQL_USER` and
`RAG_MYSQL_DATABASE` on first boot. Volume data survives `stop-stack.ps1`;
use `-RemoveVolumes` only when you intend to delete it.

## Durable upload transactions

HTTP multipart uploads and MCP `add_document` / `update_document` use the same
write-ahead journal. One file is one business transaction; a multi-file HTTP
submission retains independent success/failure results per file.

### Persisted state

| Table | Purpose |
| --- | --- |
| `upload_operations` | operation ID, request fingerprint, owner, old/new version, outcome and cleanup marker |
| `upload_steps` | committed step intent, completion or failure; rollback intent/outcome |
| `upload_objects` | immutable source, chunks and parse artifacts scoped to an operation |
| `document_heads` | published version for each namespace/document |
| `document_leases` | renewable document/name leases and exclusive namespace coordination |

New tables are created by the existing schema initialization; no existing rows
or Qdrant collections are rewritten automatically. Identifiers and document
names remain case sensitive. Legacy unversioned points stay readable until a
versioned upload for that document is published.

### Execution

1. Recover expired, unfinished conflicting uploads. Never steal a live lease.
2. Commit operation intent and acquire document/name leases.
3. Record the local workspace path **before** creating it (`rag-txn-<operation_id>`).
4. For each phase, commit a `RUNNING` intent before the action, then `DONE`:
   `receive_source`, `deduplicate`, `parse` (PDF/Word), `render_artifacts`,
   `chunking_tokenizer`, `embedding`, `sparse`, `stage_mysql`, `stage_qdrant`.
5. Store new vectors under an immutable `upload_version`; queries filter against
   MySQL heads so unpublished points cannot appear in dense/sparse/hybrid results.
6. Record `publish` intent. One MySQL transaction updates document metadata,
   current blobs, the head pointer, step completion and `COMMITTED` status.
7. Return `operation_id`; cleanup happens after commit and is retried by recovery.

Source and artifact reads use immutable objects from the selected head. Old
version data is retained for `RAG_UPLOAD_CLEANUP_DELAY_SECONDS` (default 3600)
so an in-flight reader can finish. After publication, cleanup removes only the
recorded old version; it never deletes another upload's active version. A cleanup
failure does not turn a committed upload into a failed upload.

### Idempotency

REST callers can send `Idempotency-Key` (1..128 characters). MCP callers can set
`idempotency_key` on document writes. The browser generates a UUID for each actual
upload submission. Retry the **same bytes and options** with the same key to get
the original `operation_id` and result without indexing again. Different bytes or
options with that key conflict. For HTTP batches the runtime derives a per-file
key from the submission key and file index. Keys are namespace-scoped.

Failed/rolled-back keys are retained for audit and cannot be reused as a new
request; retry the business action with a new key after recovery. Deduplicated
requests commit a replayable no-op without switching the head.

### Recovery and limits

At service startup and every 10 seconds the recovery worker scans durable
records. Leases renew during parsing and network calls; `RAG_UPLOAD_LEASE_SECONDS`
(default 120) controls expiry. Every publish/step/object write checks ownership,
so an expired worker cannot publish after another worker claims recovery.

Recovery compensates all expired nonterminal operations, not only logged errors.
An unfinished `stage_qdrant` intent may already have written vectors: delete only
that namespace/document/version, remove staged MySQL objects, and mark rollback
complete. Compensation failures remain `ROLLBACK_FAILED` for later retry. The
original failure type and failed step remain in the audit record; payloads and
credentials are not put into log messages.

Workspaces are reclaimed only from registered journal paths on their original
host, never by blindly sweeping another process's temporary directories. DOC
conversion scratch files are children of the same registered workspace. If a
node is permanently lost its local scratch storage must be reclaimed with that
node; recovery on another node can still compensate shared MySQL/Qdrant state.

This is a recoverable business transaction, **not distributed ACID/XA**. MySQL is
the publication authority; no database transaction remains open while the models
or Qdrant execute. Qdrant has no lease fencing API: late writes remain invisible
and version-scoped compensation is retried. There is no promise of a consistent
snapshot across multiple independent HTTP reads. Legacy CLI direct indexing is
not a substitute for the journaled HTTP/MCP upload path.

## Migration

Run the migration on a machine that can still read the old secrets (DPAPI-bound
keys require the original Windows account):

```powershell
rag-state-migrate --legacy-root ~/.praxis-rag/data
rag-state-migrate --legacy-root ~/.praxis-rag/data --apply
```

The first command is a read-only dry run. `--apply` imports:

- `api-keys.sqlite3` (`id`, `digest`, assignments and protected key);
- `knowledge-bases.sqlite3` (settings JSON, re-encrypted with the new key);
- `<namespace>/manifest.json` plus `<namespace>/knowledge/*` source files;
- `<namespace>/knowledge/.pdf-assets/<document_id>` and `.word-assets`;
- `<namespace>/mcp_catalog.json`.

The command is idempotent and does not delete the legacy files. Use `--force` to
overwrite document records already present in the target and `--skip-assets` to
skip artifact copy. Warnings are printed for records that cannot be decrypted or
whose source file is missing; any warning makes the command exit nonzero.

Keep `api-keys.sqlite3`, `knowledge-bases.sqlite3` and the tenant directories as
a rollback copy until a MySQL deployment has been verified.

## Multi-instance notes

- Share the same MySQL database, Qdrant endpoint and encryption key.
- Document/name leases coordinate uploads and conflict with document deletion.
  Knowledge-base edits/reembedding take an exclusive namespace lease.
- API keys are read on every request. Knowledge-base snapshots include their
  version so other instances refresh tenant caches on the next operation.
- MCP notification delivery remains process-local; this change does not provide
  a distributed subscription bus. Reconnecting clients must reload state.
- Management endpoints still require a direct loopback request; configure a
  separate authenticated administration surface before putting them behind a LB.

## Verification

```powershell
python -m pytest tests/test_upload_journal.py tests/test_upload_coordinator.py tests/test_upload_transaction.py tests/test_upload_entrypoints.py tests/test_qdrant_versions.py
```

The default tests use SQLite and in-memory Qdrant without inference. For real
integration set `RAG_TEST_MYSQL_URL` to a disposable schema named
`praxis_txn_test_*`, optionally `RAG_TEST_QDRANT_URL`, and run
`tests/test_mysql_upload_transactions.py`. This suite deletes its schema's rows
and its own `txn_test_*` Qdrant collection; never point it at production state.

## Redis 状态读模型

第一批缓存公开知识库配置、MCP catalog、文档列表及知识库列表/统计。第二批缓存 API Key 管理状态、发布指针、租约和事务/步骤/回滚的展示投影；实际鉴权、检索版本、租约执行权和日志恢复仍直接读取 MySQL。cache_revisions/cache_outbox 与状态修改在 MySQL 同事务提交；RocketMQ 处理失败刷新，Redis 故障回源。完整配置、启动和故障语义见 [状态缓存](state-cache.md)。

## UUID 结果与逻辑删除

搜索快照与产物分别持久化在 search_result_snapshots、artifact_snapshots，默认保留 1 小时。Redis 快照丢失时回源 MySQL，不重新检索。文档删除通过 del_flag 隐藏，不物理删除持久化源文件和 Qdrant 数据；原 UUID 仍可访问至到期。正常自动 schema 会为旧 documents 表补充 del_flag，DDL 禁用部署需手动升级。见 [UUID 引用与迁移](result-references.md)。
