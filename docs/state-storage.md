# Shared state storage

Praxis-RAG keeps the retrieval index in Qdrant and permanent published data in
one shared MySQL 8.0+ database. There is no SQLite backend: MySQL is the only
supported state store, for development, tests and production alike. MySQL is the
single source of truth for asynchronous document ingestion: the complete job
(file bytes included), its status and the outbox notification commit in one
transaction. RocketMQ carries identifiers only for documents; Redis holds
transient MCP search task state and result snapshots plus display caches, and is
not part of the document task-state or delivery path. Debezium is an optional
bridge that turns a committed `document_job_outbox` INSERT into the broker
notification; without it, the in-process relay polls the outbox.

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
| Async document ingest jobs (complete request, file bytes, status, stage, lease) | `document_jobs` (MySQL, source of truth) |
| Transactional outbox for document notifications | `document_job_outbox` (`PENDING` → `PUBLISHED`); relay or Debezium publishes it |
| MCP document notifications | RocketMQ schema 3 identifier-only notifications; no file content, no chunking and no Redis state |
| MCP search requests | RocketMQ schema 2 complete query envelopes (unchanged path) |
| Transient MCP search task states and result snapshots | Redis (required for queued search; no SQL fallback) |
| Search result snapshots | Redis; `RAG_RESULT_TTL_SECONDS=3600` by default; no SQL fallback |
| Issued version-bound artifact snapshots | MySQL `artifact_snapshots` (bytes and expiry) |
| Upload publication/recovery operations and business leases | MySQL UploadJournal tables (see below) |
| Persistent read-model refresh revisions and outbox | MySQL `cache_revisions`, `cache_outbox`; separate cache MQ topic |
| Consumer retries, message invisibility and dead letters | RocketMQ Broker (not SQL task scheduling) |
| Legacy MCP tasks, dispatch and search results | `document_tasks`, `document_task_outbox`, `search_tasks`, `search_task_outbox`, `search_result_snapshots`; retained, inactive, not migrated |

`RAG_SERVICE_DATA_ROOT` only locates service working files; the database is
always reached through `RAG_STATE_URL` or the `RAG_MYSQL_*` variables.

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
`CREATE TABLE IF NOT EXISTS`). `document_jobs` and `document_job_outbox` are
created by the normal schema initialization. Historical document outbox
migrations added `owner`, `lease_until`, `attempts`, and `last_error` to the
retired `document_task_outbox`; that table is legacy and is not an acceptance or
delivery prerequisite. Keep existing tables and rows; do not drop them as part of
this rollout. Disable auto schema when the runtime account cannot run DDL: use a
deployment account to create missing active tables with
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
`dpapi:` / `private-file:` rows remain readable but must be re-saved through the
API to pick up the shared key; the removed SQLite importer no longer rewrites them.

## Schema

| Table | Key | Purpose |
| --- | --- | --- |
| `api_keys` | `id`, unique `digest` | service credentials |
| `knowledge_bases` | `namespace` | committed KB settings, monotonic `version` |
| `documents` | `(namespace, name)` | per-document manifest |
| `blobs` | `(namespace, bucket, key)` | source bytes and parse artifacts (`LONGBLOB`) |
| `mcp_catalogs` | `namespace` | MCP tool/prompt settings |
| `artifact_snapshots` | `artifact_id` | durable version-bound artifact bytes, metadata and expiry |
| `document_jobs` | `artifact_id` | **active** asynchronous ingest job: complete request bytes, status/stage, attempts, owner, execution id and SQL lease |
| `document_job_outbox` | `id` | **active** transactional outbox row per accepted job (`PENDING`/`PUBLISHED`, retry metadata) |
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
synchronously. MCP `add_document` is asynchronous but durable: one MySQL transaction inserts the
complete job into `document_jobs` (request bytes, size, checksum, configuration
version, status `queued`) and one `document_job_outbox` row, then commits. The
call returns `{"artifact_id": "..."}` as soon as that transaction commits; broker
delivery and indexing are not part of acceptance. The worker reads the bytes back
from `document_jobs` and reuses the same upload journal to parse, index and publish.

### Asynchronous notification delivery

Delivery mode is chosen by `RAG_OUTBOX_BRIDGE` (two mutually exclusive modes):

- `relay` (default; for deployments without a CDC pipeline): the
  API process runs `DocumentOutboxRelay`, which makes one best-effort publish
  right after commit, then polls `PENDING` outbox rows every 2 seconds, publishes
  each notification to RocketMQ and marks the row `PUBLISHED`. Failures leave the
  row pending for the next pass, so a broker outage cannot undo or block an
  accepted upload.
- `debezium`: the in-process relay is disabled; Debezium reads the
  `document_job_outbox` INSERT from the MySQL ROW binlog and publishes it to
  `RAG_DOCUMENT_TOPIC`. This requires MySQL ROW binlog and a separately deployed
  connector; see [deploy/debezium/README.md](../deploy/debezium/README.md).

The broker message contains identifiers only:
`{"schema": 3, "artifact_id", "namespace", "name", "created_at"}`. No file bytes,
no chunking and no Redis state. The worker also accepts a raw Debezium
change event (`{"before": ..., "after": {...}, "op": "c"}`) and reads `after`, so
no extra SMT/script transform is required. The 10 MiB single-file limit is
enforced when the job is committed.

Queued search is unchanged: it still requires reachable Redis and RocketMQ plus
the RocketMQ SDK, sends a schema 2 complete-query envelope, and returns success
only after broker confirmation and Redis admission. For documents, Redis and the
broker are not on the acceptance path; only MySQL availability (and a running
consumer, to make progress) matters. Logs contain only task/operation IDs and
safe error categories, never query text, file/message content or secrets.

### Broker-owned consumption and terminal state

Document, search and persistent read-model cache refresh use separate topics and
groups. Document and search run in separate worker processes. The Broker owns
consumer retries, invisibility and DLQ routing; Redis holds transient search task
state and snapshots, never document job state.

A document notification is identifier-only: the worker loads the job from
`document_jobs`, takes a SQL lease (`RAG_DOCUMENT_JOB_LEASE_SECONDS`, default 300
seconds) and renews it every `RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS` (default 20
seconds) while parsing and calling models. The message is ACKed only when the job
already has a terminal status in MySQL (`succeeded` or `failed`). Temporary
failures leave the message unacknowledged for Broker retry; `jobs.sweep()` also
re-emits a notification for a stranded `queued` job or reclaims an expired
`processing` lease, using `RAG_DOCUMENT_JOB_SWEEP_SECONDS` (default 120 seconds)
as the stale window, and `jobs.gc_outbox()` removes outbox rows older than
`RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS` (default 7 days); job rows are not
removed by that cleanup. `get_document_status` reads MySQL directly: there is no
TTL and no Redis dependency, and `succeeded` confirms durable publication.

Terminal transitions are conditional: **first terminal result wins**; duplicate
deliveries cannot overwrite an existing terminal status. UploadJournal
independently prevents duplicate document publication; a replayed notification
reconciles the committed journal result instead of publishing again.

For search, deterministic failures write the final failure state in Redis before
ACK, success stores the result and terminal state before ACK, and Redis loss/TTL
expiry is not durable deduplication. MQ visibility renewal failures receive
bounded retries, without promising immediate cancellation of in-flight
model/Qdrant calls. After a terminal state has been recorded, workers still
attempt ACK even if renewal failed; ACK failure may cause redelivery. Nonterminal
work is not ACKed as successful.

The independent `rag-dead-letter-worker --kind document|search` consumes normal
Broker dead letters and records nonterminal task failure before ACK (document:
`failed` in MySQL; search: failure state in Redis); it does not load models,
infer, parse or rerun tasks. Its separate handler groups default to
`RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters` and
`RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters`. The actual normal DLQ topic is
`%DLQ%<source consumer group>`, not the handler group name. Historical transport
verification covered Python SDK 5.1.2 / Apache Broker 5.3.3 and explicit physical
source groups (`ns%group` → `%DLQ%ns%group`); it is not application-level schema 3
validation. SDK namespace metadata alone does not prove physical isolation.
Revalidate other SDK/Proxy/Broker combinations and namespace mapping before
rollout. See [deployment and permissions](USAGE.md#broker-消费与死信处理).

### Upgrade boundary

Stop old producers/API submissions and drain old workers and their pending
outboxes/messages **before** upgrading. Deploy API and document/search workers
together: document messages changed from schema 2 full requests to schema 3
identifier-only notifications, and mixed versions must not run or leave old
messages for new workers. Queued search remains schema 2 and incompatible with
older UUID-only messages. Old jobs and SQL result snapshots are not migrated;
legacy tables and rows are retained, not dropped, but are outside the active path.
The separate persistent read-model `cache_outbox` is unaffected.

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

For asynchronous adds, provide the key on the first request. Submission is one
MySQL transaction: if the response is lost, retry with the same key and identical
parameters/content and the original `artifact_id` is returned instead of a new
job. Broker delivery is not part of the response, so no ambiguous broker
confirmation exists for documents; the outbox row is committed or it is not.
Once an `artifact_id` is known, `get_document_status` reads MySQL and has no TTL,
so the job remains queryable regardless of Redis or MQ state. The durable
UploadJournal still reconciles an already committed publication on replay. Use a
new key for genuinely new content.

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

State databases are MySQL only; the old SQLite/JSON importer was removed. A
deployment that still holds legacy files must be migrated once with the release
that shipped `rag-state-migrate`, before upgrading to a version without it.

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
  prefix and RocketMQ document/search/cache topic/group configuration. Every
  instance must use the same `RAG_OUTBOX_BRIDGE` mode; in `debezium` mode all
  in-process relays stay off so only the CDC connector delivers notifications.
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

The whole suite runs against a disposable MySQL schema. Set `RAG_TEST_MYSQL_URL`
(or put it in `.env`) to a database whose name contains `test`, for example
`mysql+pymysql://rag:...@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4`.
`tests/conftest.py` exports it as `RAG_STATE_URL`, creates the schema once and
truncates every state table before each test. Without it the state-backed tests
fail with the `StateSettings` configuration error.

Opt-in integration suites additionally use their own disposable schemas
(`praxis_txn_test_*`, `praxis_cache_test_*`, ...) plus `RAG_TEST_QDRANT_URL`,
`RAG_TEST_REDIS_URL` and `RAG_TEST_ROCKETMQ_*`; each deletes only its own rows,
collections and prefixes. Never point any of them at production state.

## Redis 状态读模型

第一批缓存公开知识库配置、MCP catalog、文档列表及知识库列表/统计。第二批缓存 API Key 管理状态、发布指针、租约和事务/步骤/回滚的展示投影；实际鉴权、检索版本、上传事务租约执行权和日志恢复仍直接读取 MySQL。cache_revisions/cache_outbox 与状态修改在 MySQL 同事务提交；RocketMQ 处理失败刷新，Redis 故障回源。**异步文档任务状态与文件字节不经 Redis**：`document_jobs` / `document_job_outbox` 是权威，Redis 只承担搜索结果快照与上述展示缓存。完整配置、启动和故障语义见 [状态缓存](state-cache.md)。

## UUID 结果与逻辑删除

搜索快照只保存于 Redis，沿用 `RAG_RESULT_TTL_SECONDS=3600`；丢失或到期即不可用，无 SQL 回源，可能需重新提交查询。`search_result_snapshots` 为保留的历史表，不再参与活跃路径或迁移。产物仍持久化于 MySQL `artifact_snapshots`，已签发 artifact 的版本固定及到期前保留保证不变；不要将该保证扩展到搜索结果 UUID。文档删除通过 del_flag 隐藏，不物理删除持久化源文件和 Qdrant 数据。正常自动 schema 为旧 documents 表补充 del_flag，DDL 禁用部署需手动升级。见 [UUID 引用与迁移](result-references.md)。
