package mq

import (
	"context"
	"crypto/tls"
	"time"

	rmq "github.com/apache/rocketmq-clients/golang/v5"
	"github.com/apache/rocketmq-clients/golang/v5/credentials"
)

// sdkPullTransport wraps the official Go client simple consumer.
type sdkPullTransport struct {
	consumer rmq.SimpleConsumer
}

func newSDKPullTransport(settings Settings, config ConsumerConfig) (*sdkPullTransport, error) {
	sdkConfig := &rmq.Config{
		Endpoint:      settings.Endpoint,
		NameSpace:     settings.Namespace,
		ConsumerGroup: config.Group,
		Credentials: &credentials.SessionCredentials{
			AccessKey:    settings.AccessKey,
			AccessSecret: settings.SecretKey,
		},
	}
	awaitDuration := config.AwaitDuration
	if awaitDuration <= 0 {
		awaitDuration = DefaultAwaitDuration
	}
	options := []rmq.SimpleConsumerOption{
		rmq.WithSimpleAwaitDuration(awaitDuration),
		rmq.WithSimpleSubscriptionExpressions(map[string]*rmq.FilterExpression{
			config.Topic: rmq.SUB_ALL,
		}),
	}
	if settings.TLS {
		options = append(options, rmq.WithClientFuncForSimpleConsumer(func(clientConfig *rmq.Config, clientOptions ...rmq.ClientOption) (rmq.Client, error) {
			clientOptions = append(clientOptions, rmq.WithConnOptions(rmq.WithTLSConfig(&tls.Config{
				MinVersion: tls.VersionTLS12,
			})))
			return rmq.NewClient(clientConfig, clientOptions...)
		}))
	}
	consumer, err := rmq.NewSimpleConsumer(sdkConfig, options...)
	if err != nil {
		return nil, err
	}
	return &sdkPullTransport{consumer: consumer}, nil
}

func (t *sdkPullTransport) Start() error { return t.consumer.Start() }

func (t *sdkPullTransport) Stop() error { return t.consumer.GracefulStop() }

func (t *sdkPullTransport) Receive(ctx context.Context, maxMessages int32, invisibleDuration time.Duration) ([]ReceivedMessage, error) {
	views, err := t.consumer.Receive(ctx, maxMessages, invisibleDuration)
	if err != nil {
		return nil, err
	}
	messages := make([]ReceivedMessage, 0, len(views))
	for _, view := range views {
		message := ReceivedMessage{
			Topic:           view.GetTopic(),
			MessageID:       view.GetMessageId(),
			Body:            view.GetBody(),
			DeliveryAttempt: int(view.GetDeliveryAttempt()),
			handle:          view,
		}
		if keys := view.GetKeys(); len(keys) > 0 {
			message.Key = keys[0]
		}
		messages = append(messages, message)
	}
	return messages, nil
}

func (t *sdkPullTransport) Ack(ctx context.Context, message ReceivedMessage) error {
	view, ok := message.handle.(*rmq.MessageView)
	if !ok || view == nil {
		return newSafeError(ErrConsumerFailed, "consumer ack", nil)
	}
	return t.consumer.Ack(ctx, view)
}

func (t *sdkPullTransport) ChangeInvisibleDuration(_ context.Context, message ReceivedMessage, invisibleDuration time.Duration) error {
	view, ok := message.handle.(*rmq.MessageView)
	if !ok || view == nil {
		return newSafeError(ErrConsumerFailed, "consumer nack", nil)
	}
	return t.consumer.ChangeInvisibleDuration(view, invisibleDuration)
}
