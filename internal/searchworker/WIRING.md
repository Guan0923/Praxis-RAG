# Wiring internal/searchworker into cmd/rag-search-worker

The package owns durable search-task state only. Retrieval and reference capture
are injected, so the worker binary never imports the HTTP/MCP layers.

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
config.MaxAttempts = 5                                     // broker retry budget; exhausted -> DLQ
consumer, err := mq.NewRocketMQPullConsumer(settings, config, worker.MessageHandler())
if err != nil { return err }
consumer.OnExhausted = worker.ExhaustedHandler()           // durable terminal state, then ack

ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
defer stop()
if err := worker.RunConsumer(ctx, consumer); err != nil { return err }   // Start -> block -> Stop
_ = worker.Close()
return service.Close(ctx)
```

Dead-letter binary (`cmd/rag-dead-letter-worker --kind search`): construct the
processor with only `Tasks` (no Searcher) and the DLQ topic configuration
(`%DLQ%<search group>`), then run
`mq.NewPullConsumer(config, transport, worker.DeadLetterHandler())`. The handler
returns true only after the durable task is terminal (or already absent), so the
broker only acks confirmed outcomes.

## Broker-less / local mode

`processor.Run(ctx)` + `RunOnce(ctx)` poll an injected `Receiver`; tests use a
fake and single-process deployments can feed envelopes from their own source.
Production workers use `RunConsumer` with `mq.PullConsumer`.

## Semantics

* `Handle`: `Claim` -> `SearchRequestFromArguments` -> `Searcher` (deadline ctx)
  -> `Capturer(false)` -> `SearchTasks.Complete`; failures call
  `SearchTasks.Fail(retry)` where Python-ValueError-shaped errors are terminal.
  Ack = `Terminal` says unknown/timed-out/failed/succeeded.
* Poison/malformed broker payloads are nacked (`MessageHandler` returns false)
  so the broker retry/DLQ policy applies; dead-letter payloads that cannot be
  decoded are ignored (not acked) by `DeadLetterHandler`.
* No comment is written on `Fail` (state keeps the Python-compatible status only;
  the cause is logged by category).
