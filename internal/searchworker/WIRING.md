# Wiring internal/searchworker into cmd/rag-search-worker

The package owns durable search-task state only. Retrieval and reference capture
are injected, so the worker binary never imports the HTTP/MCP layers.

The worker is a **CDC notification consumer**: MySQL `search_tasks` is the only
source of truth and Debezium publishes one identifier-only notification per
committed INSERT (or `schedule_version` bump). Submission is accepted as soon as
its MySQL transaction commits; it neither waits for a broker acknowledgement nor
writes a transactional outbox.

## Event source: two Debezium connectors, three topics/groups

`docs/cdc-migration.md` is the contract. One task connector (capturing
`search_tasks`) feeds this worker; the other two streams belong to the document
and cache workers:

| Connector | Captures | Topic (env) | Consumer group (env) | Consumer binary |
| --- | --- | --- | --- | --- |
| task connector | `search_tasks` (INSERT + `schedule_version` changes only; `column.include.list=result_id,namespace,schedule_version`, `skip.messages.without.change=true`, `skipped.operations=d`) | `RAG_SEARCH_TOPIC` (default `praxis_search`) | `RAG_SEARCH_GROUP` (default `praxis_search_workers`) | `cmd/rag-search-worker` |
| task connector | `document_jobs` (same column/skip rules) | `RAG_DOCUMENT_TOPIC` (default `praxis_document_ingest`) | `RAG_DOCUMENT_GROUP` (default `praxis_document_ingest_workers`) | `cmd/rag-document-worker` |
| cache connector | 8 cache read-model tables (all c/u/d) | `RAG_CACHE_TOPIC` (default `praxis_state_cache_refresh`) | `RAG_CACHE_GROUP` (default `praxis_state_cache_refresh_workers`) | `cmd/rag-cache-worker` |

Topics/consumer groups are never reused. Each stream has its own DLQ topic and
group; this worker's DLQ is `RAG_SEARCH_DLQ_TOPIC` (consumed by
`cmd/rag-dead-letter-worker --kind search`).

### Search notification (schema 3)

The native envelope (tests and manual replay) carries locating identifiers only:

```json
{"schema":3,"result_id":"<uuid>","namespace":"<ns>","schedule_version":2}
```

The physical record is the raw Debezium event
(`debezium.format.value.schemas.enable=false`):

```json
{"before":null,"after":{"result_id":"<uuid>","namespace":"<ns>","schedule_version":2},"source":{"table":"search_tasks"},"op":"c","ts_ms":1700000000000}
```

`mq.DecodeSearchNotification` accepts both shapes; `op` is `c` (new admission),
`r` (initial snapshot) or `u` (requeue by `schedule_version` bump). A delete
(`op=d`), tombstone (`null` value) or before-image-only record is not task work:
it decodes to `state.ErrIgnorableCDCChange` and `MessageHandler` ACKs it without
retry, without DLQ and without touching `search_tasks`. Malformed payloads are
nacked so the broker retry/DLQ policy still applies to real poison messages. The
retired schema-2 envelope (which carried arguments/deadline) is not accepted.

### Cache change events (schema 1)

The cache connector publishes row-level changes of the 8 cache read-model tables.
Those events never reach this worker; the cache consumer (`cmd/rag-cache-worker`)
derives `(table, op, locator columns)` from them:

```json
{"schema":1,"table":"documents","op":"u","before":{"namespace":"<ns>","name":"a.pdf"},"after":{"namespace":"<ns>","name":"a.pdf"}}
```

`op` is `c`/`u`/`d`; a tombstone is ignored. `before`/`after` only carry the
locator columns of `column.include.list`, never file bytes, query text, KB
settings, catalog data or credentials. The search worker must never subscribe to
that topic.

## Runtime seam the binary must provide

`Searcher` must be **raw retrieval without result-snapshot capture**. The
synchronous service path (`runtime.Service.Search` / `SearchEngine.Search` with
`SetReferenceCapture` wired) already captures artifacts and saves a snapshot; a
queued worker must not do that twice. Expose one of:

```go
// runtime (task-18/Lead scope)
func (s *Service) SearchRaw(ctx context.Context, namespace string, request contracts.SearchRequest) (map[string]any, error)
```

(It should run the orchestrator and add `snapshot_context` like Python's
`runtime.search(..., save_snapshot=False)` but must not call `searchrefs.Capture`
or `ResultSnapshots.Save`.)

The worker calls `Searcher` with a context deadline derived from the durable
task deadline, so the retrieval implementation must honour `ctx`.

## main.go lifecycle sketch

```go
// Kafka settings: bootstrap servers (RAG_KAFKA_BOOTSTRAP_SERVERS) plus the
// search topic/group (RAG_SEARCH_TOPIC / RAG_SEARCH_GROUP).
settings, err := mq.SettingsFromEnv()
if err != nil { return err }

stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
if err != nil { return err }
stores, err := state.New(stateSettings, stateSettings.AutoSchema)
if err != nil { return err }
defer func() { _ = stores.Close() }()

cache, err := redisstate.NewFromEnv()          // task-13: enables snapshots/transient state
if err != nil { return err }
if err := redisstate.Attach(stores, cache); err != nil { return err }
stores.CacheSync.Start()

service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot) // owns raw retrieval
if err != nil { return err }

worker, err := searchworker.New(searchworker.Options{
    Tasks:    stores.SearchTasks,
    Searcher: rawSearchAdapter{Service: service},           // Service.SearchRaw above
    Capturer: searchrefs.New(stores, logger),
    Logger:   logger,
})
if err != nil { return err }

config := mq.DefaultConsumerConfig(settings.SearchTopic, settings.SearchGroup)
config.MaxAttempts = 5                                     // Kafka consumer retries in-process; exhausted -> DLQ topic
config.DLQTopic = settings.SearchDLQTopic
consumer, err := mq.NewKafkaPullConsumer(settings, config, worker.MessageHandler())
if err != nil { return err }
consumer.OnExhausted = worker.ExhaustedHandler()           // durable terminal state, then ack

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
if err := worker.RunConsumer(ctx, consumer); err != nil { return err }   // Start -> block -> Stop
_ = worker.Close()
return service.Close(ctx)
```

Dead-letter binary (`cmd/rag-dead-letter-worker --kind search`): construct the
processor with only `Tasks` (no Searcher) and a Kafka consumer subscribed to the
search DLQ topic (`RAG_SEARCH_DLQ_TOPIC`), then run
`mq.NewKafkaPullConsumer(settings, dlqConfig, worker.DeadLetterHandler())`. The
Kafka adapter publishes exhausted deliveries to that topic; the handler returns
true only after the durable task is terminal (or already absent), so the consumer
only acks confirmed outcomes. Delete/tombstone records replayed through the DLQ
are already complete: `DeadLetterMessage` ACKs them without touching any task.

## Broker-less / local mode

`processor.Run(ctx)` + `RunOnce(ctx)` poll an injected `Receiver`; tests use a
fake and single-process deployments can feed notifications from their own source.
Production workers use `RunConsumer` with `mq.PullConsumer`.

## ACK rule (docs/cdc-migration.md §5.3)

ACK the delivery **iff the outcome is already durable in MySQL**:

1. the notification was safely skipped — unknown/foreign row, terminal task,
   live lease held by another worker, stale `schedule_version`, or a deadline
   already converged to `timed_out`; or
2. the task was claimed (`status=processing`, `attempts+1`), retrieval ran and
   `Complete` committed the snapshot + `succeeded` in one transaction; or
3. the failure branch recorded its durable outcome —
   `Fail(retry=true)` (`schedule_version+1`, `status=queued`, `stage=retry_wait`)
   or `Fail(retry=false)` (`failed`); or
4. a dead-letter outcome was recorded.

A DB error (claim/complete/fail write failure) returns false → nack, so broker
retry and the DLQ handle it. Redis/capture failures are never a nack reason: the
snapshot fact row lives in MySQL (`search_result_snapshots`) and the Redis
transient entry is only a read cache backfilled on the next `Get`.

## Semantics

* `Handle`: validate identifiers -> `Claim` (single `SELECT ... FOR UPDATE`; it
  requires `schedule_version` equality, `status IN (queued, processing)`,
  `lease_until <= now` and `deadline > now`; an overdue task is converged to
  `timed_out` and returns nil) -> `SearchRequestFromArguments` -> `Searcher`
  (deadline-scoped ctx) -> `Capturer(false)` -> `SearchTasks.Complete`.
  `nil` from `Claim` is a safe skip and ACK; failures call `SearchTasks.Fail`
  where Python-ValueError-shaped errors are terminal and everything else retries
  with `schedule_version+1`. `Complete` writes `search_result_snapshots` and the
  `succeeded` terminal row in the same transaction; the Redis write is
  best-effort after the commit.
* Requeueing is the only UPDATE that increments `schedule_version`, which is the
  only signal Debezium re-emits; a duplicate delivery of an already-completed
  task is ACKed without re-running retrieval, and a duplicate `Complete` returns
  the existing payload without inserting a second fact row.
* Poison/malformed broker payloads are nacked (`MessageHandler` returns false)
  so the Kafka consumer's in-process retry policy applies (exhausted deliveries
  are published to the search DLQ topic by the adapter); dead-letter payloads
  that are not task changes are ACKed, and undecodable ones are left retryable
  by `DeadLetterHandler`.
* No comment is written on `Fail` (state keeps the Python-compatible status only;
  the cause is logged by category).
