# Wiring internal/documentworker into cmd/rag-document-worker

The package owns the durable job lease/heartbeat/reconcile state machine. Parsing,
chunking, embedding and Qdrant publication are injected through `Ingester`.

The worker is a **CDC notification consumer**: MySQL `document_jobs` is the only
source of truth and Debezium publishes one identifier-only notification per
committed INSERT (or `schedule_version` bump). The application writes no
transactional outbox, and the worker never polls a queue table.

## Event source: two Debezium connectors, two topics/groups

`docs/cdc-migration.md` is the contract. The deployment runs two Debezium Server
instances against the same MySQL (different `database.server.id`, offset and
schema-history storage, health port):

| Connector | Captures | Topic (env) | Consumer group (env) | Consumer binary |
| --- | --- | --- | --- | --- |
| task connector | `document_jobs` (INSERT + `schedule_version` changes only; `column.include.list=artifact_id,namespace,schedule_version`, `skip.messages.without.change=true`, `skipped.operations=d`) | `RAG_DOCUMENT_TOPIC` (default `praxis_document_ingest`) | `RAG_DOCUMENT_GROUP` (default `praxis_document_ingest_workers`) | `cmd/rag-document-worker` |
| cache connector | 8 cache read-model tables (all c/u/d) | `RAG_CACHE_TOPIC` (default `praxis_state_cache_refresh`) | `RAG_CACHE_GROUP` (default `praxis_state_cache_refresh_workers`) | `cmd/rag-cache-worker` |

Topics/consumer groups are never reused. Each stream has its own DLQ topic and
group (`RAG_DOCUMENT_DLQ_TOPIC` / `RAG_DOCUMENT_DLQ_GROUP`,
`RAG_CACHE_DLQ_TOPIC` / `RAG_CACHE_DLQ_GROUP`); this document worker consumes
only the document DLQ via `cmd/rag-dead-letter-worker --kind document`.
Search is synchronous and never flows through Kafka, so the task connector does
not capture `search_tasks` and there is no search topic/group or DLQ lane.

### Document notification (schema 4)

The native envelope (tests and manual replay) carries locating identifiers only:

```json
{"schema":4,"artifact_id":"<uuid>","namespace":"<ns>","schedule_version":3}
```

The physical record is the raw Debezium event
(`debezium.format.value.schemas.enable=false`):

```json
{"before":null,"after":{"artifact_id":"<uuid>","namespace":"<ns>","schedule_version":3},"source":{"table":"document_jobs"},"op":"c","ts_ms":1700000000000}
```

`mq.DecodeDocumentNotification` accepts both shapes; `op` is `c` (new admission),
`r` (initial snapshot) or `u` (requeue by `schedule_version` bump). A delete
(`op=d`), tombstone (`null` value) or before-image-only record is not task work:
it decodes to `state.ErrIgnorableCDCChange` and `MessageHandler` ACKs it without
retry, without DLQ and without touching `document_jobs`. Malformed payloads are
nacked so the broker retry/DLQ policy still applies to real poison messages.

### Cache change events (schema 1)

The cache connector publishes row-level changes of the 8 cache read-model tables.
Those events never reach this worker; the cache consumer (`cmd/rag-cache-worker`)
derives `(table, op, locator columns)` from them:

```json
{"schema":1,"table":"documents","op":"u","before":{"namespace":"<ns>","name":"a.pdf"},"after":{"namespace":"<ns>","name":"a.pdf"}}
```

`op` is `c`/`u`/`d`; a tombstone is ignored. `before`/`after` only carry the
locator columns of `column.include.list`, never file bytes, query text, KB
settings, catalog data or credentials. The document worker must never subscribe
to that topic.

## Runtime seam the binary must provide

```go
// runtime (task-18/Lead scope)
func (s *Service) ProcessDocumentJob(ctx context.Context, job documentworker.Payload) error
```

Requirements, matching `RagServiceRuntime.process_document_task`:

* read the committed job/upload journal state by `job.ArtifactID`
  (`DocumentJobs.Bind`/`Progress` are available to the implementation);
* parse/chunk/embed/upsert and commit the upload-journal publication with the
  task-identifying result fields (`sha256`, `size`, `task_configuration_version`)
  before returning;
* return `*apierrors.KnowledgeValidationError` (or `*state.DocumentValidationError`)
  for permanently invalid input so the worker records a terminal failure; return
  any other error to retry;
* returning nil without a committed publication makes the worker nack (the
  sweep recovers it), so never return nil on an uncommitted publication.

Optional `JournalRecoverer` (`RecoverDocumentUploads(ctx) error`) is called from
`Processor.Recover` (Python `UploadCoordinator.recover`); a runtime stub may
return nil until G2 journal recovery lands.

## main.go lifecycle sketch

```go
// Kafka settings: bootstrap servers (RAG_KAFKA_BOOTSTRAP_SERVERS) plus the
// document topic/group (RAG_DOCUMENT_TOPIC / RAG_DOCUMENT_GROUP).
settings, err := mq.SettingsFromEnv()
if err != nil { return err }
stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
if err != nil { return err }
stores, err := state.New(stateSettings, stateSettings.AutoSchema)
if err != nil { return err }
defer func() { _ = stores.Close() }()

cache, err := redisstate.NewFromEnv()     // state cache + snapshots, same as rag-service
if err != nil { return err }
if err := redisstate.Attach(stores, cache); err != nil { return err }
stores.CacheSync.Start()

service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
if err != nil { return err }

worker, err := documentworker.New(documentworker.Options{
    Jobs:            stores.DocumentJobs,
    Ingester:        documentIngester{Service: service},   // Service.ProcessDocumentJob above
    JournalRecovery: service,                              // optional RecoverDocumentUploads
    Logger:          logger,
})
if err != nil { return err }

config := mq.DefaultConsumerConfig(settings.DocumentTopic, settings.DocumentGroup)
config.MaxAttempts = 3                                    // Kafka consumer retries in-process; exhausted -> DLQ topic
config.DLQTopic = settings.DocumentDLQTopic
consumer, err := mq.NewKafkaPullConsumer(settings, config, worker.MessageHandler())
if err != nil { return err }
consumer.OnExhausted = worker.ExhaustedHandler()          // durable "failed", then ack

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()

// Python's loop calls worker.recover() every consumption pass; RunConsumer owns
// Start/block/Stop, so run maintenance on its own ticker.
go func() {
    ticker := time.NewTicker(30 * time.Second)
    defer ticker.Stop()
    for {
        select {
        case <-ctx.Done(): return
        case <-ticker.C: _ = worker.Recover(ctx)
        }
    }
}()

if err := worker.RunConsumer(ctx, consumer); err != nil { return err }
_ = worker.Close()
return service.Close(ctx)
```

Operators must verify the deployment no longer points `RAG_DOCUMENT_TOPIC` at an
outbox-derived topic: the retired `document_job_outbox` table no longer exists,
and the task connector must be the only producer of this topic.

Dead-letter binary (`cmd/rag-dead-letter-worker --kind document`): construct the
processor with only `Jobs` (no Ingester) and a Kafka consumer subscribed to the
document DLQ topic, then run `worker.DeadLetterHandler()` through that consumer.
The Kafka adapter publishes exhausted deliveries to the DLQ topic; the handler
acks only once the job is durably terminal (or absent), and returns false while a
live lease or unfinished journal publication exists. Delete/tombstone records
replayed through the DLQ are already complete: `DeadLetterMessage` ACKs them
without touching any job row.

## ACK rule (docs/cdc-migration.md §5.3)

ACK the delivery **iff the outcome is already durable in MySQL**:

1. the notification was safely skipped — the row is unknown/foreign, terminal,
   owned by a live lease, or carries a stale `schedule_version`; or
2. the job was claimed (`status=processing`, `attempts+1`) and this round recorded
   either a terminal state (`succeeded`/`failed`) or a retry wait
   (`status=queued`, `stage=retry_wait`, `retry_at`; no generation bump yet); or
3. a dead-letter outcome was recorded (`dead-letter-worker`).

A DB error (claim/reconcile/fail write failure) returns false → nack, so broker
retry and the DLQ handle it. Redis/cache failures are never a nack reason: the
job fact lives in MySQL.

## Semantics distilled from the CDC design

1. `MessageHandler` decodes the schema-4 envelope or raw Debezium record;
   ignorable changes (d/tombstone/before-only) ACK, malformed payloads nack.
2. `Handle` revalidates identifiers, `Get`s the job (unknown = ACK), and ACKs a
   terminal job; `Reconcile` adopts an already committed publication before
   claiming (crash recovery).
3. `Claim` is a single `SELECT ... FOR UPDATE` transaction: it requires
   `schedule_version` equality, `status IN (queued, processing)`,
   `lease_until <= now` and (for `retry_wait`) `retry_at <= now`; a stale
   version, terminal row, live lease, early retry or missing row returns nil →
   ACK. `document_jobs.attempts` is incremented here; at 3 the row is written
   `failed` instead of claiming a fourth execution. `Sweep` is the only path
   that increments `schedule_version` (due retry or expired lease), which is the
   only signal Debezium re-emits.
4. Heartbeat goroutine renews the lease during ingestion (failure lets lease
   expiry recover the job).
5. `Reconcile` again, `ExecutionBusy` check, then `Ingester.ProcessDocumentJob`.
6. Success requires a committed publication (`Reconcile` true), otherwise nack.
7. Failure: reconcile first; otherwise `Fail(retry)` where validation errors are
   terminal (`Invalid document or changed knowledge-base configuration`) and all
   other errors wait for `retry_at` (`Document processing failed: attempt N/3: ...`).
   The third failure is terminal inside `Fail`; a parse timeout is recorded as
   `attempt N/3: parse timeout`. Both branches recorded a durable outcome →
   ACK; the sweep emits the replacement generation when the retry is due.
8. `Recover` runs `Sweep` (due `retry_wait` + expired leases, each bumping
   `schedule_version`; merely queued jobs are left untouched), `GCJobs` (terminal
   retention) and the optional journal recovery. It reads/writes no outbox table.
