# Shared state storage

Praxis-RAG keeps the retrieval index in Qdrant and permanent published data in
one shared relational database. The production target is MySQL 8.0+; a SQLite
file is still accepted for local development and the test suite. Redis is required
for transient MCP upload/search task state, document reassembly and search result
snapshots. RocketMQ carries complete requests, not pointers to SQL task rows.

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
| MCP document/query requests | RocketMQ schema 2 envelopes; complete query or document request chunks (512 KiB decoded maximum per chunk, 10 MiB maximum per file) |
| Transient MCP task states and document reassembly | Redis (required; no SQL fallback) |
| Search result snapshots | Redis; `RAG_RESULT_TTL_SECONDS=3600` by default; no SQL fallback |
| Issued version-bound artifact snapshots | MySQL `artifact_snapshots` (bytes and expiry) |
| Upload publication/recovery operations and business leases | MySQL UploadJournal tables (see below) |
| Persistent read-model refresh revisions and outbox | MySQL `cache_revisions`, `cache_outbox`; separate cache MQ topic |
| Consumer retries, message invisibility and dead letters | RocketMQ Broker (not SQL task scheduling) |
| Legacy MCP tasks, dispatch and search results | `document_tasks`, `document_task_outbox`, `search_tasks`, `search_task_outbox`, `search_result_snapshots`; retained, inactive, not migrated |

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
`CREATE TABLE IF NOT EXISTS`). Historical document outbox migrations added
`owner`, `lease_until`, `attempts`, and `last_error`; those tables are now legacy,
not an acceptance or delivery prerequisite. Keep existing tables and rows; do not
drop them as part of this rollout. Disable auto schema when the runtime account
cannot run DDL: use a deployment account to create missing active tables with
`metadata.create_all(engine)` and apply required additive migrations before
starting API/workers. `create_all` alone does not add columns to an existing
table. No transient task/result data migration is performed.

The MySQL runtime account needs `SELECT`, `INSERT`, `UPDATE`, `DELETE` on the
schema; the account running schema setup also needs `CREATE` and `ALTER` for
the additive migration. The server's `max_allowed_packet` must be
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
| `artifact_snapshots` | `artifact_id` | durable version-bound artifact bytes, metadata and expiry |
| `document_tasks` | `artifact_id` | **legacy, inactive** asynchronous document task state |
| `document_task_outbox` | `artifact_id` | **legacy, inactive** initial delivery generation, owner, lease, attempts and retry metadata |
| `search_tasks` | `result_id` | **legacy, inactive** search parameters, deadline and task state |
| `search_task_outbox` | `result_id` | **legacy, inactive** initial search-message delivery |
| `search_result_snapshots` | `result_id` | **legacy, inactive** SQL search snapshots; not a Redis fallback |

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

HTTP multipart uploads and MCP `update_document` run the write-ahead journal
synchronously. MCP `add_document` sends the complete document request through
RocketMQ and returns only `artifact_id` after **all chunks are broker-confirmed**
and Redis acceptance state is recorded. Transient task/reassembly state uses the
existing `RAG_RESULT_TTL_SECONDS` (default 3600 seconds).
The worker reassembles the request in Redis, then uses the same MySQL upload
journal to parse, index and publish. One file remains one business transaction;
a multi-file HTTP submission retains independent success/failure results per file.

### Asynchronous request delivery

Both MCP upload and queued search require reachable Redis and RocketMQ plus the
RocketMQ SDK. A schema 2 envelope contains the complete query or document request;
document payloads use chunks of at most **512 KiB decoded bytes**, preserving the
**10 MiB file limit**. API success waits for every message's broker confirmation,
even with search `wait_seconds=0`. Acceptance is not indexing completion.

Missing configuration, failed Redis prerequisites or MQ sends are surfaced as
errors. There is no SQL task/outbox acceptance during an outage and no background
SQL relay to finish a partial submission. A timeout after some sends can be
ambiguous; retry the same submission with the same idempotency key and content.
Redis task/reassembly loss or TTL expiry makes the ID unavailable and may require
resubmission. Transient data has no SQL fallback; published document data is not
lost with Redis. Logs contain only task/operation IDs and safe error categories,
never query text, file/message content or secrets.

### Broker-owned consumption and terminal state

Document, search and persistent read-model cache refresh use separate topics and
groups. Document and search run in separate worker processes. The Broker owns
consumer retries, invisibility and DLQ routing; Redis holds transient task state,
reassembly and search snapshots, not SQL task rows or outboxes.

Incomplete document chunks are **never ACKed**: Broker replay preserves the
original payload so Redis expiry/loss cannot silently discard previously received
chunks. Any chunk that completes reassembly can execute the request; once a
terminal state is recorded, remaining duplicate chunks can be ACKed.
Temporary failures leave the message unacknowledged for Broker retry.
Deterministic failures write final failure state in Redis before ACK. Success
stores the result and terminal state before ACK. Terminal transitions are
conditional: **first terminal result wins**; duplicate deliveries cannot overwrite
an existing Redis terminal result. Redis loss/expiry is not durable deduplication
for search. UploadJournal independently prevents duplicate document publication.

For documents, commit durable publication in MySQL **before** writing Redis
`succeeded`. These are separate operations, not an atomic Redis+MySQL transaction.
If Redis recording fails after publication, leave the message unacknowledged;
on retry reconcile the durable UploadJournal outcome and repair transient state
rather than publish again. Journal operations, business leases, SQL heartbeats,
fenced version commits, compensation and recovery remain in MySQL.

MQ visibility renewal failures receive bounded retries, without promising immediate
cancellation of in-flight model/Qdrant calls. After a terminal Redis state has been
recorded, workers still attempt ACK even if renewal failed; ACK failure may cause
redelivery. Nonterminal work is not ACKed as successful.

The planned independent `rag-dead-letter-worker --kind document|search` consumes
normal Broker dead letters, records nonterminal task failure in Redis and then
ACKs; it does not load models, infer, parse or rerun tasks. Its separate handler
groups default to `RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters` and
`RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters`. The actual normal DLQ topic is
`%DLQ%<source consumer group>`, not the handler group name. Historical transport
verification covered Python SDK 5.1.2 / Apache Broker 5.3.3 and explicit physical
source groups (`ns%group` → `%DLQ%ns%group`); it is not schema 2 application
validation. SDK namespace metadata alone does not prove physical isolation.
Revalidate other SDK/Proxy/Broker combinations and namespace mapping before rollout.
See [deployment and permissions](USAGE.md#broker-消费与死信处理).

`get_document_status` reads Redis; only `succeeded` confirms durable publication.
Missing/expired transient state is unavailable, not a successful empty response
or a reason to read legacy SQL tasks. See [polling](USAGE.md#异步新增与状态轮询).

### Upgrade boundary

Stop old producers/API submissions and drain old workers and their pending
outboxes/messages **before** upgrading. Deploy API and document/search workers
together: schema 2 full-request envelopes are incompatible with legacy UUID-only
messages. Do not mix versions or leave old messages for new workers. Old jobs and
SQL result snapshots are not migrated; legacy tables and rows are retained, not
dropped, but are outside the active path. Document worker `--dispatch-only` is
removed; dispatcher compatibility `start`/`run_once` entry points are no-ops,
not SQL relays. No filesystem MQ spool is used. None of these is an
upgrade/recovery strategy. The separate persistent read-model `cache_outbox`
is unaffected.

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
the original `operation_id` and result for synchronous writes, or the original
`artifact_id` for asynchronous `add_document`, without creating another task.
Different bytes or options with that key conflict. For HTTP batches the runtime
derives a per-file key from the submission key and file index. Keys are
namespace-scoped.

For asynchronous adds, provide the key on the first request. After a disconnect
or ambiguous broker confirmation, retry with the same key and identical
parameters/content. Once an `artifact_id` is known, poll it while its Redis state
exists. Redis loss/TTL expiry can require resubmission; there is no durable SQL
outbox to replay. Transient ID reuse is not guaranteed after Redis loss, but the
durable UploadJournal still reconciles an already committed publication. Use a
new key for genuinely new content, not merely a delayed confirmation.

Failed/rolled-back upload-journal keys are retained for audit and cannot be
reused as a new request; retry the business action with a new key after recovery.
Deduplicated requests commit a replayable no-op without switching the head.

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

## Knowledge-base index publication

Reembedding builds a new Qdrant collection before opening the short publication
transaction. That transaction checks the namespace lease and expected configuration
version, then commits the collection pointer, settings version and cache outbox
atomically. The lease is checked again immediately before COMMIT. Model calls and
Qdrant writes are not placed inside a long SQL transaction.

Construction failures and acknowledged pre-COMMIT rollbacks may delete the
unpublished collection. COMMIT/connection failures are treated as uncertain: a
locking current read confirms success only when the full intended settings and
next configuration version match. Reading the old pointer, observing a newer
configuration, or failing to read the database does not authorize collection
removal. An unconfirmed publication returns HTTP 503 and retains the collection;
check the authoritative configuration before retrying. Do not delete a retained
collection merely because a read still shows the old pointer: establish that the
original transaction has ended and the collection is unreferenced before manual
cleanup. This change does not add a durable automatic garbage collector for
retained uncertain collections.

After confirmed publication, cache refresh and namespace-lease cleanup failures
are logged independently and do not undo the published index or change the
successful response. Cache refresh events remain in the existing SQL outbox for
retry. HTTP response delivery itself is outside the publication transaction.

## Multi-instance notes

- Share the same MySQL database, Qdrant endpoint, encryption key, Redis endpoint/
  prefix and RocketMQ document/search/cache topic/group configuration.
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

第一批缓存公开知识库配置、MCP catalog、文档列表及知识库列表/统计。第二批缓存 API Key 管理状态、发布指针、租约和事务/步骤/回滚的展示投影；实际鉴权、检索版本、上传事务租约执行权和日志恢复仍直接读取 MySQL。cache_revisions/cache_outbox 与状态修改在 MySQL 同事务提交；RocketMQ 处理失败刷新，Redis 故障回源。完整配置、启动和故障语义见 [状态缓存](state-cache.md)。

## UUID 结果与逻辑删除

搜索快照只保存于 Redis，沿用 `RAG_RESULT_TTL_SECONDS=3600`；丢失或到期即不可用，无 SQL 回源，可能需重新提交查询。`search_result_snapshots` 为保留的历史表，不再参与活跃路径或迁移。产物仍持久化于 MySQL `artifact_snapshots`，已签发 artifact 的版本固定及到期前保留保证不变；不要将该保证扩展到搜索结果 UUID。文档删除通过 del_flag 隐藏，不物理删除持久化源文件和 Qdrant 数据。正常自动 schema 为旧 documents 表补充 del_flag，DDL 禁用部署需手动升级。见 [UUID 引用与迁移](result-references.md)。
