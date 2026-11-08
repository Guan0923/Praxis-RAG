# Wiring internal/documentworker into cmd/rag-document-worker

The package owns the durable job lease/heartbeat/reconcile state machine. Parsing,
chunking, embedding and Qdrant publication are injected through `Ingester`.

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
config.MaxAttempts = 5                                    // exhausted -> DLQ
consumer, err := mq.NewRocketMQPullConsumer(settings, config, worker.MessageHandler())
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

Broker-less single-process mode: `worker.Run(ctx)` performs startup `Recover`
once, then polls `OutboxReceiver` (`documentworker.NewOutboxReceiver(stores.DocumentJobs)`)
which drains committed PENDING outbox rows without a broker; job retries are
re-emitted by `Sweep`. Override `Options.Receiver` for tests/fakes.

Dead-letter binary (`cmd/rag-dead-letter-worker --kind document`): construct the
processor with only `Jobs` (no Ingester) and the DLQ topic
(`%DLQ%<document group>`), then run `worker.DeadLetterHandler()` through a pull
consumer. The handler acks only once the job is durably terminal (or absent), and
returns false while a live lease or unfinished journal publication exists.

## Semantics distilled from document_worker.py

1. `Validate` notification -> `Get` job (unknown = ack).
2. Terminal job = ack. `Reconcile` a committed publication before claiming
   (crash recovery).
3. `Claim` lease; `nil` means another live worker owns it or it turned terminal
   -> ack only if `Terminal`.
4. Heartbeat goroutine renews the lease during ingestion (failure lets lease
   expiry recover the job).
5. `Reconcile` again, `ExecutionBusy` check, then `Ingester.ProcessDocumentJob`.
6. Success requires a committed publication (`Reconcile` true), otherwise nack.
7. Failure: reconcile first; otherwise `Fail(retry)` where validation errors are
   terminal (`Invalid document or changed knowledge-base configuration`) and all
   other errors retry (`Document processing failed (<type>)`).
8. `Sweep` requeues expired leases and re-emits stranded queued notifications;
   `Recover` also runs the bounded GCs and optional journal recovery.
