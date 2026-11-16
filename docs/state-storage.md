# Shared state storage
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。
>
> **2026-11-13 更新（事务 outbox 已下线）**：`document_job_outbox`、`search_task_outbox`、`cache_outbox`、`document_task_outbox` 已从活跃路径移除，改由两个 Debezium 连接器直接采集 `document_jobs` 与缓存相关业务表（见 [CDC 迁移契约](cdc-migration.md)）。本文中所有以 outbox 为中介的投递描述仅作历史记录；任务表新增 `schedule_version`，`cache_revisions` 扩展为 revision 水位。
> **2026-11-13 二次更新（搜索同步化）**：排队搜索与结果快照已下线；`search_tasks`、`search_result_snapshots` 保留历史数据、不再读写（`retired; no longer read or written`），不 DROP、不清数据。搜索为同步执行，artifact 以 `artifact_snapshots` 为事实来源，保留期沿用 `RAG_RESULT_TTL_SECONDS=3600`。见 [产物引用](result-references.md)。


Praxis-RAG keeps the retrieval index in Qdrant and permanent published data in
one shared MySQL 8.0+ database. MySQL is the only supported backend: there is
no SQLite file fallback for development, tests or production. MySQL is the
single source of truth for asynchronous document ingestion and queued search:
the complete document job (file bytes included), its status/`schedule_version`,
the queued-search task and the search result snapshot all live in MySQL. Kafka
carries identifiers only; Redis holds display caches and search-result read
caches, and is not part of the task-state or delivery path. **Since 2026-11-13**
two Debezium connectors read MySQL tables directly (task connector:
`document_jobs`; cache connector: eight read-model tables); the
retired transactional outboxes are no longer written or polled.

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
| Document task notifications (current) | Debezium task connector reads `document_jobs`; Kafka schema 4 identifier-only (`artifact_id/namespace/schedule_version`). The retired `document_job_outbox` is **no longer on the active path** |
| MCP search (current) | Fully synchronous: one `tools/call` runs retrieval + rerank and returns evidence; no task row, no notification, no polling. |
| ~~Search result snapshots~~ | **retired 2026-11-13**: the queue, schema-3 notification, worker and snapshot read path were removed. `search_result_snapshots` rows are retained as history and are no longer read or written. Artifacts (`artifact_snapshots`) remain the durable reference, TTL `RAG_RESULT_TTL_SECONDS=3600`. |
| Issued version-bound artifact snapshots | MySQL `artifact_snapshots` (bytes and expiry) |
| Upload publication/recovery operations and business leases | MySQL UploadJournal tables (see below) |
| Persistent read-model refresh revisions | MySQL `cache_revisions` (`revision`, `refreshed_revision`, `attempts`, `last_error`, `updated_at`); Debezium cache connector → separate cache Kafka topic. `cache_outbox` is retired |
| Consumer retries and dead-letter publishing | Kafka consumer in the worker processes (in-process retry; explicit RAG_*_DLQ_TOPIC; not SQL task scheduling) |
| Retired outbox tables (2026-11-13) | `document_job_outbox`, `search_task_outbox`, `cache_outbox`, `document_task_outbox`; dropped by `rag-service-migrate --retire-outbox`. `document_tasks` is legacy and inactive |

`RAG_SERVICE_DATA_ROOT` only locates local service files such as the upload
recovery workspace; the database is always reached through `RAG_STATE_URL` or
the `RAG_MYSQL_*` variables.

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
`RAG_STATE_AUTO_SCHEMA` (default on, creates missing tables and applies
additive column migrations). `document_jobs`, `search_tasks`, `search_result_snapshots`,
`cache_revisions` and `cdc_cutover_state` are maintained by the normal schema
initialization; the four retired outbox tables are not created and existing rows
are left untouched until `rag-service-migrate --retire-outbox` drops them.
Disable auto schema when the runtime account cannot run DDL: use a deployment
account to apply the same additive migrations before starting API/workers. No
transient task/result data migration is performed.

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
| `blobs` | `(namespace, bucket, key)` | source bytes and parse artifacts; bytes live in `content_objects` |
| `upload_objects` | `(operation_id, bucket, key)` | immutable staged upload objects; bytes live in `content_objects` |
| `content_objects` | `hash` (SHA-256) | **content-addressed byte store**: one row per unique payload, shared by every tenant/document that uploaded it |
| `mcp_catalogs` | `namespace` | MCP tool/prompt settings |
| `artifact_snapshots` | `artifact_id` | durable version-bound artifact bytes, metadata and expiry |
| `activity_counters` | `(metric, window_start)` | **active** hourly activity counters for the Console monitor (for example every retrieval execution counted as `search`); additive only, no event rows |
| `runtime_heartbeats` | `(role, instance)` | **active** worker liveness heartbeats (`document-worker`); each beat upserts `updated_at`, rows older than 24h are pruned; the monitor reads the newest row and recent instance count |
| `document_jobs` | `artifact_id` | **active** asynchronous ingest job: complete request bytes, status/stage, `schedule_version`, attempts, owner, execution id and SQL lease |
| `document_job_outbox` | `id` | **retired 2026-11-13** transactional outbox; dropped by `--retire-outbox` |
| `document_batches` | `id` | **active** Console batch upload: namespace/owner, `Idempotency-Key`, stored lifecycle (`saving`/`active`) and accepted/skipped/failed counters; no file bytes |
| `document_batch_items` | `(batch_id, position)` | **active** per-file batch outcome: `saving`/`accepted`/`skipped`/`failed`, skip/failure reason, `artifact_id`, safe error text |
| `document_tasks` | `artifact_id` | **legacy, inactive** asynchronous document task state |
| `document_task_outbox` | `artifact_id` | **retired 2026-11-13** legacy initial-delivery metadata; dropped by `--retire-outbox` |
| `search_tasks` | `result_id` | **retired 2026-11-13**: queued-search task table retained as history; no longer read or written, not dropped |
| `search_task_outbox` | `result_id` | **retired 2026-11-13**; dropped by `--retire-outbox` |
| `search_result_snapshots` | `result_id` | **retired 2026-11-13**: result snapshot table retained as history; no longer read or written, not dropped. Artifacts live in `artifact_snapshots` |
| `cache_revisions` | `(kind, scope)` | **active** read-model revision plus refresh watermark (`refreshed_revision`, `attempts`, `last_error`, `updated_at`) |
| `cdc_cutover_state` | `k` | **transition-only** key/value state for the CDC cutover (`cdc_start_file`, `cdc_start_pos`, `cutover_at`, `schedule_baseline`, `phase`) |
| `users` | `id`, unique `username`, unique nullable `email` | Console accounts: PBKDF2 password hash, role/enabled, profile columns `email_verified`/`age`/`gender` |
| `user_sessions` | `id`, unique `token_digest` | issued login sessions; only the token digest and CSRF token are stored |
| `user_avatars` | `user_id` | one profile image per account (`LONGBLOB`, ≤2 MiB); the users projection only joins its `updated_at` |
| `email_verification_codes` | `id` | registration / e-mail-change codes: SHA-256 digest, purpose, attempts, expiry; issued codes invalidate older ones |
| `app_settings` | `setting_key` | Console-editable settings; the `mail` row holds the SMTP configuration with its password protected as `enc:v1:` |
| `feishu_connections` | `user_id` | **optional** per-account Feishu OAuth grant: open/union id, display name/avatar, `access_token`/`refresh_token` protected as `enc:v1:`, expiry and granted scope; the Feishu App Secret is never stored here |
| `feishu_oauth_states` | `state_digest` (SHA-256) | **optional** one-time OAuth callback state bound to a Console account, 10-minute expiry, pruned on the next state creation; only the digest is stored |

Identifier columns (`namespace`, document `name`, `digest`, blob `key`) use a
binary collation in MySQL so `Alpha` and `alpha` do not collapse into one row.
`settings`, `diagnostics` and MCP catalog data are `LONGTEXT`; `content_objects.content`
is `LONGBLOB`.

### Content-addressed bytes

`content_objects` stores each distinct payload once, keyed by its SHA-256.
`upload_objects.content_hash` and `blobs.sha256` are reference columns: when a
hundred tenants upload the same file, MySQL keeps one `content_objects` row plus
one lightweight reference row per tenant/document. Rows keep an empty `content`
placeholder so the column type stays unchanged.

- Reads (`UploadJournal.GetObject`, `BlobStore.Get`) join `content_objects` and
  fall back to inline `content` for rows written before this layout; the
  maintenance loop migrates those legacy rows in bounded batches
  (`BackfillContentObjects`, 100 rows per pass).
- Deletes remove only reference rows (by document, operation or blob key) in the
  same transaction as the logical document delete. Every path that drops a
  reference (document delete, upload rollback/no-op/old-version cleanup, blob
  overwrite, knowledge-base purge) sets `content_objects.retired_at` on payloads
  whose last reference just disappeared.
- `content_objects` rows are removed by the maintenance GC
  (`PurgeUnreferencedContent`) only when the row is retired, no
  `upload_objects`/`blobs` row references the hash, and `retired_at` is older than
  the retention window `ContentObjectRetentionSeconds` (**8 days**, i.e. more than
  one week). A payload that receives a new reference in the meantime has
  `retired_at` cleared by the insert path, so a live object is never deleted.
  Reference rows are still removed immediately; only the bytes are retained.
- Qdrant is unchanged: every knowledge base keeps its own collection and owns its
  vectors. Deduplication is a MySQL storage concern only.

Existing databases are upgraded in place by `ensureSchema` (idempotent):
`content_objects`, `upload_objects.content_hash`, `content_objects.retired_at`,
`ix_upload_objects_content`, `ix_blobs_sha256` and `ix_content_objects_retired` are
created when missing, and the maintenance loop migrates legacy inline rows in
bounded batches. Deployments that disable automatic DDL
(`RAG_STATE_AUTO_SCHEMA=false`) must apply the equivalent statements manually:

```sql
CREATE TABLE `content_objects` (
  `hash` varchar(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NOT NULL,
  `size` bigint NOT NULL,
  `content` longblob NOT NULL,
  `created_at` double NOT NULL,
  `retired_at` double NULL,
  PRIMARY KEY (`hash`),
  INDEX `ix_content_objects_retired` (`retired_at`)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_bin;
ALTER TABLE `upload_objects`
  ADD COLUMN `content_hash` varchar(64) CHARACTER SET utf8mb4 COLLATE utf8mb4_bin NULL,
  ADD INDEX `ix_upload_objects_content` (`content_hash`);
ALTER TABLE `blobs` ADD INDEX `ix_blobs_sha256` (`sha256`);
```

### Batched knowledge-base deletion

`DELETE /v1/admin/knowledge-bases/{namespace}` and
`POST /v1/admin/knowledge-bases/batch-delete` share one batched purge: all
requested namespaces are deleted in **one transaction** with a fixed (~30)
statement count, independent of the number of documents or namespaces. Lookup
values (upload operations, content hashes, search result ids) are materialised
into temporary tables, and cache revisions/outbox rows are bumped with four
set-based statements instead of four per key. Qdrant collections are deleted one
HTTP request (`DELETE /collections/{name}`) per knowledge base, and only when no
other knowledge base references the collection. Namespaces with a live upload
lease are reported as failed and left untouched.

`knowledge_bases.version` increments on every update. Instances compare the
stored version before reusing a cached tenant, so a configuration change made on
one instance is picked up by the others without a restart.

## Local Docker stack

The repository ships `docker-compose.yml` with MySQL 8, Qdrant, Redis and a
single-node Kafka broker and the Debezium service. Copy
`.env.example` to `.env` (or let the start script do it), then fill in the
passwords and `RAG_STATE_ENCRYPTION_KEY`. `.env` is gitignored.

```powershell
.\scripts\start-stack.ps1 -MigrateOnly   # start dependencies, create topics, migrate
.\scripts\start-stack.ps1                # start dependencies, migrate and serve
.\scripts\stop-stack.ps1                 # stop containers, keep volumes
```

The Compose stack maps MySQL to `127.0.0.1:3307` by default to avoid colliding
with an existing 3306 instance; set `RAG_MYSQL_PORT` in `.env` to change it.
The MySQL user and database are created from `RAG_MYSQL_USER` and
`RAG_MYSQL_DATABASE` on first boot. Kafka uses the host listener
`127.0.0.1:9092` and containers use `kafka:9092`; it persists in `kafka_data`, and
`start-stack.ps1` explicitly creates the cache/document/search and DLQ topics.
Volume data survives `stop-stack.ps1`; use `-RemoveVolumes` only when you intend
to delete it.

## Durable upload transactions

HTTP multipart uploads and MCP `update_document` run the write-ahead journal
synchronously. MCP `add_document` is asynchronous but durable: one MySQL transaction inserts the
complete job into `document_jobs` (request bytes, size, checksum, configuration
version, `schedule_version=1`, status `queued`), then commits. No outbox row is
written. The call returns `{"artifact_id": "..."}` as soon as that transaction
commits; Kafka delivery and indexing are not part of acceptance. The worker reads
the bytes back from `document_jobs` and reuses the same upload journal to parse,
index and publish.

### Asynchronous notification delivery

Kafka 通知统一由两个 Debezium 连接器从 MySQL 表级采集投递。API 只在 MySQL 单事务中提交
文档任务；任务连接器只读取 `document_jobs` 的 binlog 并发送到 `RAG_DOCUMENT_TOPIC`，
缓存连接器读取 8 张业务表并发送到 `RAG_CACHE_TOPIC`。搜索为同步执行，不产生通知，
也没有搜索 topic/消费组（原 `search_tasks` 采集已删除）。
应用没有 outbox 轮询发布器或投递模式开关（该机制已于 2026-11-13 下线）。
连接器配置见 [deploy/kafka-connect/README.md](../deploy/kafka-connect/README.md) 与 [CDC 迁移契约](cdc-migration.md)。

The Kafka message contains identifiers only:
`{"schema": 4, "artifact_id", "namespace", "schedule_version"}`. No file bytes,
no query text, no chunking and no Redis state. The worker also accepts a raw
Debezium change event (`{"before": ..., "after": {...}, "op": "c"}`) and reads
`after`, so no extra SMT/script transform is required. The 10 MiB single-file
limit is enforced when the job is committed.

Search is fully synchronous (2026-11-13): `Service.Search` runs retrieval + rerank
in the request and returns evidence directly, so there is no task row, schema-3
notification or worker. `search_tasks`/`search_result_snapshots` are retained as
history and are no longer read or written. For documents, Redis and Kafka are not
on the acceptance path either; only MySQL availability (and a running consumer, to
make progress) matters. Logs contain only task/operation IDs and safe error
categories, never query text, file/message content or secrets.

### Kafka consumption and terminal state

Document and persistent read-model cache refresh use separate topics and groups.
Document and cache run in separate worker processes; search runs synchronously in
`rag-service` (no topic, no worker since 2026-11-13). The Go Kafka
consumer adapter owns in-process retries and dead-letter publishing; all document
task state lives in MySQL, never only in Redis.

A document notification is identifier-only: the worker loads the job from
`document_jobs`, takes a SQL lease (`RAG_DOCUMENT_JOB_LEASE_SECONDS`, default 300
seconds) and renews it every `RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS` (default 20
seconds) while parsing and calling models. The offset is committed only when the
job already has a terminal status in MySQL (`succeeded` or `failed`). A
temporary failure (handler returns false) retries the same record in-process with
exponential backoff (100 ms base, 5 s cap), `DeliveryAttempt` 1..`MaxAttempts`
(default 3; independent from the per-document execution budget below);
on exhaustion the raw payload is published to the explicit
`RAG_DOCUMENT_DLQ_TOPIC`, the durable exhausted-state handler runs, and only then
is the offset committed (at-least-once delivery). `MaxAttempts=0` retries until
the context/process is cancelled. `jobs.sweep()` emits the replacement notification for a due `retry_wait` job or
reclaims an expired `processing` lease, using
`RAG_DOCUMENT_JOB_SWEEP_SECONDS` (default 120 seconds) to pace re-emissions; a
job that is merely queued is left alone and waiting does not consume an
execution. It increments `schedule_version` inside MySQL instead of writing an
outbox row. `document_jobs.attempts` caps executions at 3 (first attempt plus at
most two retries), and the third failure writes `failed` with the last reason.
`get_document_status` reads MySQL directly: there is no
TTL and no Redis dependency, and `succeeded` confirms durable publication.

Terminal transitions are conditional: **first terminal result wins**; duplicate
deliveries cannot overwrite an existing terminal status. UploadJournal
independently prevents duplicate document publication; a replayed notification
reconciles the committed journal result instead of publishing again.

Search is executed synchronously and therefore has no consumer, no notification
and no terminal-state transaction: the response is produced in the request and
artifacts are committed to `artifact_snapshots` before returning. Search-related
`schedule_version`/snapshot concepts are retired. Nonterminal work is never
committed as successful; in-flight model/Qdrant work is cancelled when the
request context is cancelled (client disconnect or `RAG_SEARCH_TIMEOUT_SECONDS`).

The independent `rag-dead-letter-worker --kind document|cache` consumes
the explicit dead-letter topics (`RAG_DOCUMENT_DLQ_TOPIC` /
`RAG_CACHE_DLQ_TOPIC`) with independent
`RAG_*_DLQ_GROUP` consumer groups and records nonterminal task failure before
committing (document: `failed` in MySQL; cache: alert only, because the
revision watermark is retried by `RefreshDirty`). It does not load models, infer,
parse or rerun tasks. Dead-letter topics are explicitly configured rather than
derived from a source group. The `--kind search` branch and `RAG_SEARCH_DLQ_*`
were removed with synchronous search. Historical live verification covered the retired
outbox pipeline; the dual-connector architecture has not been verified on a live
cluster yet. See [VALIDATION](../VALIDATION.md) and
[deployment](DEPLOYMENT.md).

### Upgrade boundary

Stop old producers/API submissions and drain old workers and their pending
messages **before** upgrading. Deploy API and document/search/cache workers
together: document notifications are schema 4, queued-search notifications are
schema 3 and cache events are schema 1; mixed versions must not run or leave old
messages for new workers. The full cutover sequence (record binlog position,
backfill `schedule_version`, mark cache baseline, verify, then
`--retire-outbox`) is in [docs/cdc-migration.md](cdc-migration.md) §8. Old jobs
and SQL result snapshots are not migrated; retired tables are kept until the
explicit retirement step.

### Persisted state

| Table | Purpose |
| --- | --- |
| `upload_operations` | operation ID, request fingerprint, owner, old/new version, outcome and cleanup marker |
| `upload_steps` | committed step intent, completion or failure; rollback intent/outcome |
| `upload_objects` | immutable source, chunks and parse artifacts scoped to an operation; bytes are content-addressed in `content_objects` |
| `document_heads` | published version for each namespace/document |
| `document_leases` | renewable document/name leases and exclusive namespace coordination |

`document_batches` / `document_batch_items` are progress metadata for
`POST /v1/document-batches`: the batch row is committed before the per-file
admissions (so the Console can poll while saves run) and the item rows are
settled afterwards. They never store file bytes — every admitted file is still
a `document_jobs` row with its bytes — and are pruned after seven days.

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

confirmation exists for documents; the outbox row is committed or it is not.
Once an `artifact_id` is known, `get_document_status` reads MySQL and has no TTL,
so the job remains queryable regardless of Redis or Kafka state. The durable
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

There is no SQLite importer any more: the state database is MySQL and
`rag-state-migrate` only verifies (default) or creates (`--apply`) the state
schema. Legacy knowledge sources are imported separately by
`rag-service-migrate --legacy-root <dir>` (preview by default, `--apply` to
write), which re-uploads the source files instead of copying vectors.

Existing deployments whose data still lives in `state.sqlite3` must import it
with the last release that shipped the SQLite importer, then point
`RAG_STATE_URL` at MySQL before upgrading to this version.

## Knowledge-base index publication

Reembedding builds a new Qdrant collection before opening the short publication
transaction. That transaction checks the namespace lease and expected configuration
version, then commits the collection pointer, settings version and the
`cache_revisions` bump atomically. The lease is checked again immediately before
COMMIT. Model calls and Qdrant writes are not placed inside a long SQL transaction.

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
successful response. Cache refresh state stays in `cache_revisions` and is
retried by the cache worker/`RefreshDirty`. HTTP response delivery itself is
outside the publication transaction.

## Multi-instance notes

- Share the same MySQL database, Qdrant endpoint, encryption key, Redis endpoint/
  prefix, Kafka document/search/cache topic/group and DLQ-topic configuration, and
  the two Debezium connectors (task/cache) with independent server IDs and
  offset/schema-history storage. Every instance relies on the shared connectors to
  deliver notifications.
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
go test -count=1 ./internal/state/... ./internal/qdrant/...
```

The Go suite runs against a disposable MySQL schema and in-memory Qdrant without
inference: export RAG_TEST_MYSQL_URL (the database name must contain 	est) and
run go test -p 1 ./...; packages serialize through a MySQL advisory lock. Historical
Python live suites (including `RAG_TEST_MYSQL_URL`/`RAG_TEST_QDRANT_URL` runs) were
removed with the Python backend; current Kafka/MySQL/Debezium live verification is
recorded in [GO_MIGRATION](GO_MIGRATION.md) and [VALIDATION](../VALIDATION.md).

## Redis 状态读模型

第一批缓存公开知识库配置、MCP catalog、文档列表及知识库列表/统计。第二批缓存 API Key 管理状态、发布指针、租约和事务/步骤/回滚的展示投影；实际鉴权、检索版本、上传事务租约执行权和日志恢复仍直接读取 MySQL。cache_revisions 与状态修改在 MySQL 同事务提交，随后由 Debezium 缓存连接器投递、`rag-cache-worker` 按 revision 水位刷新；Redis 故障时读模型回源，脏 key 由 `RefreshDirty` 重试。**异步文档任务状态与文件字节不经 Redis**：`document_jobs` 是权威，Redis 只承担展示缓存；搜索为同步执行，不写 Redis。完整配置、启动和故障语义见 [状态缓存](state-cache.md)。

## UUID 结果与逻辑删除

artifact 以 MySQL `artifact_snapshots` 为事实来源，沿用 `RAG_RESULT_TTL_SECONDS=3600`（语义为 artifact TTL）；已签发 artifact 的版本固定及到期前保留保证不变。`search_tasks`/`search_result_snapshots` 保留历史数据但不再读写，因此不存在“搜索结果 UUID 回源”路径，重新检索即可获得新证据。文档删除通过 del_flag 隐藏，并在同一事务中删除该文档的 `upload_objects`/`blobs` 引用行；失去所有引用的 `content_objects` 行会写入 `retired_at`，由维护循环在 8 天（>1 周）保留期后回收，期间若被重新引用会自动清除 `retired_at`。Qdrant 数据仍不随单文档删除物理删除；删除整个知识库时才按集合删除（每库一次请求）。正常自动 schema 为旧 documents 表补充 del_flag，DDL 禁用部署需手动升级。见 [产物引用](result-references.md)。
