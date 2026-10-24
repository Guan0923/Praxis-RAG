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
| Accepted MCP document upload bytes | `blobs` (immutable upload; no upload TTL) |
| MCP document tasks and initial dispatch | `document_tasks`, `document_task_outbox` |
| MCP search parameters, business state and initial dispatch | `search_tasks`, `search_task_outbox` |
| Consumer retries, message invisibility and dead letters | RocketMQ Broker (not SQL task scheduling) |

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
`CREATE TABLE IF NOT EXISTS`). Auto schema also runs the additive document
outbox migration, adding `owner`, `lease_until`, `attempts`, and `last_error`
to existing `document_task_outbox` tables. Other existing columns are not
implicitly migrated. Disable auto schema when the runtime account cannot run
DDL: before starting API/relay/workers, use a deployment account to create
missing tables with `metadata.create_all(engine)` and explicitly run the state
storage backend's `migrate_document_outbox()` method. `create_all` alone does not
add columns to an existing table. Apply other schema changes with a migration
tool before rolling out a new version.

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
| `document_tasks` | `artifact_id` | accepted asynchronous MCP document business state; first terminal result wins |
| `document_task_outbox` | `artifact_id` | initial delivery generation, producer `owner`, `lease_until`, `attempts`, `retry_at`, sanitized `last_error` |
| `search_tasks` | `result_id` | search parameters, deadline and business state; first terminal result wins |
| `search_task_outbox` | `result_id` | initial search-message delivery only |

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
synchronously. MCP `add_document` first commits immutable raw bytes, a task and
an outbox row in one SQL transaction (T1), then immediately returns only
`artifact_id`. That request neither connects/sends to MQ nor flushes Redis.
The document worker later uses the same upload journal to parse, index and
publish. One file is one business transaction; a multi-file HTTP submission
retains independent success/failure results per file.

### Asynchronous document dispatch

Acceptance means durable SQL storage, not broker confirmation or indexing.
Missing MQ configuration still permits acceptance, but processing requires a
configured background relay, queue and separate `rag-document-worker` consumer.
Initial dispatch failures remain pending indefinitely; they do not consume
Broker delivery retries or expire the accepted upload bytes.

The relay claims an outbox owner, lease and generation in a short transaction,
sends outside the SQL transaction, and conditionally records delivery only for the
matching owner/generation. It persists initial dispatch `attempts`, `retry_at`
and a sanitized `last_error`, using exponential backoff with jitter. A single-item
failure does not block other tasks. Outages back off and emit throttled warnings.
Expired initial-send claims can be recovered without stealing a valid producer
lease. `RAG_DOCUMENT_DISPATCH_*` controls this first-send coordination only;
a confirmed delivery is not re-created by SQL consumer retry/redrive scheduling.
A crash around send confirmation can still cause duplicate delivery.

### Broker-owned consumption and terminal state

MySQL stores files/query parameters, business state and initial outbox delivery.
The Broker owns consumer retries, invisibility and DLQ routing; SQL tasks do not
own execution leases, heartbeats, retry counts or consumer redrive schedules.
Legacy consumer configuration is accepted only for backward-compatible config
parsing and is deprecated and ignored at runtime; `DocumentTaskSettings` is no
longer used. Existing consumer-lease schema fields remain inert for compatibility:
no destructive column-drop migration is required. This does not remove the
UploadJournal business document leases or compensation described below.
Temporary failures leave the message unacknowledged for Broker retry. A
deterministic failure is committed as final SQL business state before ACK.
Success follows the same commit-before-ACK ordering. Terminal transitions are
conditional: **first terminal result wins**; later duplicate deliveries cannot
overwrite it and can be ACKed after checking authoritative SQL state.

MQ visibility renewal failures receive bounded retries. Loss of renewal does not
promise immediate cancellation of in-flight work or downstream model/Qdrant calls.
Even after a renewal failure, the worker still attempts ACK if terminal SQL state
has committed; otherwise Broker redelivery remains possible.

The planned independent `rag-dead-letter-worker --kind document|search` consumes
normal Broker dead letters, finalizes nonterminal business state and ACKs only
after commit; it does not load models, infer, parse or rerun tasks. Its separate
handler groups default to `RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters`
and `RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters`. The actual normal DLQ topic
is `%DLQ%<source consumer group>`, not a topic named after the handler group.
Transport verification confirmed Broker-generated normal DLQ receive and ACK
under an independent handler group with Python SDK 5.1.2 / Apache Broker 5.3.3.
Explicit physical source groups such as `ns%group` were also verified with
`%DLQ%ns%group`; the transport preserves the physical receive/ACK topic name.
SDK namespace metadata alone does not prove physical isolation. Revalidate other
SDK/Proxy/Broker combinations and namespace mapping before rollout.
See [deployment and permissions](USAGE.md#broker-消费与死信处理).

UploadJournal's per-document/name business leases and their own SQL heartbeat
are retained for fenced version commits, compensation and recovery. Only SQL
**task** heartbeats are removed, not all document-related SQL heartbeats.
These business controls are separate from message invisibility; the Broker is
the sole consumption-retry scheduler. Producer outbox schema leases remain
initial-send coordination only, not consumer execution ownership.

`get_document_status` keeps `status="queued"` while dispatch is pending or
awaiting consumption. Stages distinguish `dispatch_wait` (initial dispatch),
`dispatch_retry` (retry pending) and `queued` (sent, awaiting consumption).
Only `succeeded` confirms publication. Status caching can lag; SQL remains the
authority. See [deployment settings and polling](USAGE.md#异步新增与状态轮询).

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
or ambiguous commit, retry with the same key and identical parameters/content.
Once an `artifact_id` is known, poll it rather than submitting again for a
background MQ outage: the durable outbox remains pending and retries. Use a new
key for new content, not merely because dispatch is delayed.

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

第一批缓存公开知识库配置、MCP catalog、文档列表及知识库列表/统计。第二批缓存 API Key 管理状态、发布指针、租约和事务/步骤/回滚的展示投影；实际鉴权、检索版本、上传事务租约执行权和日志恢复仍直接读取 MySQL。cache_revisions/cache_outbox 与状态修改在 MySQL 同事务提交；RocketMQ 处理失败刷新，Redis 故障回源。完整配置、启动和故障语义见 [状态缓存](state-cache.md)。

## UUID 结果与逻辑删除

搜索快照与产物分别持久化在 search_result_snapshots、artifact_snapshots，默认保留 1 小时。Redis 快照丢失时回源 MySQL，不重新检索。文档删除通过 del_flag 隐藏，不物理删除持久化源文件和 Qdrant 数据；原 UUID 仍可访问至到期。正常自动 schema 会为旧 documents 表补充 del_flag，DDL 禁用部署需手动升级。见 [UUID 引用与迁移](result-references.md)。
