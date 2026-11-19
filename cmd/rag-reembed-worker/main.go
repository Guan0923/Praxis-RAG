// Command rag-reembed-worker rebuilds a knowledge base's dense vector index
// from durable MySQL jobs delivered through Kafka Connect.
package main

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"syscall"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/redisstate"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func main() {
	if err := run(); err != nil {
		logging.FromContext(context.Background()).Error("re-embedding worker stopped", logging.ErrorAttrs(err)...)
		os.Exit(1)
	}
}

func run() error {
	logger, err := logging.Setup()
	if err != nil {
		return err
	}
	if _, err := config.LoadStartupConfig(); err != nil {
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
		return errors.New("RAG_BACKEND=qdrant is required for re-embedding")
	}
	kafka, err := mq.SettingsFromEnv()
	if err != nil {
		return err
	}
	if !kafka.Configured() {
		return errors.New("RAG_KAFKA_BOOTSTRAP_SERVERS is required for re-embedding")
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
	consumerConfig := mq.DefaultConsumerConfig(kafka.ReembedTopic, kafka.ReembedGroup)
	consumer, err := mq.NewKafkaPullConsumer(kafka, consumerConfig, func(ctx context.Context, message mq.ReceivedMessage) bool {
		namespace, id, err := state.DecodeReembedNotification(message.Body)
		if err != nil {
			if !state.IgnorableCDCChange(err) {
				logger.Warn("invalid re-embedding notification", "partition", message.Partition, "offset", message.Offset)
			}
			return true
		}
		if err := service.ProcessReembed(ctx, namespace, id); err != nil {
			if ctx.Err() == nil {
				logger.Warn("re-embedding delivery will retry", append([]any{"job_id", id}, logging.ErrorAttrs(err)...)...)
			}
			return false
		}
		return true
	})
	if err != nil {
		return err
	}
	if err := consumer.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = consumer.Stop() }()
	logger.Info("re-embedding worker started", "topic", kafka.ReembedTopic, "group", kafka.ReembedGroup)
	<-ctx.Done()
	return nil
}
