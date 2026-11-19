package main

import (
	"context"
	"errors"
	"flag"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/redisstate"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/workerprobe"
)

func main() {
	if err := run(); err != nil {
		logging.FromContext(context.Background()).Error("index cleanup worker stopped", logging.ErrorAttrs(err)...)
		os.Exit(1)
	}
}

func run() error {
	namespace := flag.String("namespace", "", "owned namespace to reconcile")
	collection := flag.String("collection", "", "owned collection to reconcile")
	reconcile := flag.Bool("reconcile", false, "scan external indexes once and register orphan candidates")
	flag.Parse()
	if _, err := config.LoadStartupConfig(); err != nil {
		return err
	}
	logger, err := logging.Setup()
	if err != nil {
		return err
	}
	serviceSettings, err := config.ServiceSettingsFromEnv()
	if err != nil {
		return err
	}
	ragConfig, err := config.RagConfigFromEnv()
	if err != nil {
		return err
	}
	if ragConfig.Backend != config.BackendQdrant {
		return errors.New("RAG_BACKEND=qdrant is required for index cleanup")
	}
	kafka, err := mq.SettingsFromEnv()
	if err != nil {
		return err
	}
	if !*reconcile && !kafka.Configured() {
		return errors.New("RAG_KAFKA_BOOTSTRAP_SERVERS is required for index cleanup")
	}
	settings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	stores, err := state.New(settings, settings.AutoSchema)
	if err != nil {
		return err
	}
	defer func() { _ = stores.Close() }()
	lockSettings, err := state.DocumentLockSettingsFromEnv()
	if err != nil {
		return err
	}
	locker, err := redisstate.NewDocumentLock(lockSettings)
	if err != nil {
		return err
	}
	defer func() { _ = locker.Close() }()
	if err := stores.AttachDocumentLocker(locker); err != nil {
		return err
	}
	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *reconcile {
		if *namespace != "" || *collection != "" {
			return service.ReconcileIndexScope(ctx, *namespace, *collection)
		}
		return service.ReconcileIndexes(ctx)
	}
	consumer, err := mq.NewKafkaPullConsumer(kafka, mq.DefaultConsumerConfig(kafka.DeleteTopic, kafka.DeleteGroup), func(ctx context.Context, m mq.ReceivedMessage) bool {
		locator, err := state.DecodeCleanupNotification(m.Body)
		if err != nil {
			if !state.IgnorableCDCChange(err) {
				logger.Warn("invalid index cleanup notification", "partition", m.Partition, "offset", m.Offset)
			}
			return true
		}
		if err := service.ProcessCleanupNotification(ctx, locator); err != nil {
			if ctx.Err() == nil {
				logger.Warn("index cleanup delivery will retry", logging.ErrorAttrs(err)...)
			}
			return false
		}
		return true
	})
	if err != nil {
		return err
	}
	closeProbe, err := workerprobe.Serve(ctx, consumer.ProbeStatus)
	if err != nil {
		return err
	}
	defer closeProbe()
	if err := consumer.Start(ctx); err != nil {
		return err
	}
	defer consumer.Stop()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	logger.Info("index cleanup worker started", "topic", kafka.DeleteTopic, "group", kafka.DeleteGroup)
	for ctx.Err() == nil {
		count, err := service.CleanupIndexBatch(ctx)
		if err != nil && ctx.Err() == nil {
			logger.Warn("index cleanup batch will retry", logging.ErrorAttrs(err)...)
		}
		if count == 100 && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-tick.C:
		}
	}
	return nil
}
