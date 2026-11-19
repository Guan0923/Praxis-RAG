package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/workerprobe"
)

const consumerGroup = "praxis_sag_extraction_workers"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
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
	stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	mqSettings, err := mq.SettingsFromEnv()
	if err != nil {
		return err
	}

	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		return err
	}
	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		_ = stores.Close()
		return err
	}
	defer func() { _ = service.Close(context.Background()) }()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	wake := make(chan struct{}, 1)
	var consumer *mq.PullConsumer
	if mqSettings.Configured() {
		if consumerGroup == mqSettings.CacheGroup {
			return fmt.Errorf("RAG_CACHE_GROUP must differ from %s", consumerGroup)
		}
		// The notification only wakes a database drain. The cache worker owns
		// the cache DLQ and is responsible for reporting invalid CDC records.
		mqSettings.CacheDLQTopic = ""
		consumer, err = mq.NewKafkaPullConsumer(mqSettings,
			mq.DefaultConsumerConfig(mqSettings.CacheTopic, consumerGroup), wakeOnPublication(wake))
		if err != nil {
			return err
		}
		if err := consumer.Start(ctx); err != nil {
			return err
		}
		defer func() { _ = consumer.Stop() }()
		logger.Info("SAG worker subscribed", "topic", mqSettings.CacheTopic, "group", consumerGroup)
	}
	logger.Info("SAG extraction worker started")
	var pulse workerprobe.Pulse
	closeProbe, err := workerprobe.Serve(ctx, func() (bool, bool) {
		live := pulse.Recent(5 * time.Minute)
		if consumer == nil {
			return live, live
		}
		kafkaLive, kafkaReady := consumer.ProbeStatus()
		return live && kafkaLive, live && kafkaReady
	})
	if err != nil {
		return err
	}
	defer closeProbe()
	service.RunSagExtraction(ctx, wake, logger, pulse.Beat)
	return nil
}

func wakeOnPublication(wake chan<- struct{}) mq.Handler {
	return func(_ context.Context, message mq.ReceivedMessage) bool {
		change, err := mq.DecodeCacheChange(message.Body)
		if err == nil && change.Table == "document_heads" && change.After != nil {
			select {
			case wake <- struct{}{}:
			default:
			}
		}
		return true
	}
}
