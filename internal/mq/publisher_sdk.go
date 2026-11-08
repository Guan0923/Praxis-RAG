package mq

import (
	"context"
	"crypto/tls"

	rmq "github.com/apache/rocketmq-clients/golang/v5"
	"github.com/apache/rocketmq-clients/golang/v5/credentials"
)

// sdkProducerTransport wraps the official Go client producer.
type sdkProducerTransport struct {
	producer rmq.Producer
}

func newSDKProducerTransport(settings Settings) (*sdkProducerTransport, error) {
	config := &rmq.Config{
		Endpoint:  settings.Endpoint,
		NameSpace: settings.Namespace,
		Credentials: &credentials.SessionCredentials{
			AccessKey:    settings.AccessKey,
			AccessSecret: settings.SecretKey,
		},
	}
	options := []rmq.ProducerOption{rmq.WithTopics(settings.DocumentTopic, settings.SearchTopic)}
	if settings.TLS {
		options = append(options, rmq.WithClientFunc(func(clientConfig *rmq.Config, clientOptions ...rmq.ClientOption) (rmq.Client, error) {
			clientOptions = append(clientOptions, rmq.WithConnOptions(rmq.WithTLSConfig(&tls.Config{
				MinVersion: tls.VersionTLS12,
			})))
			return rmq.NewClient(clientConfig, clientOptions...)
		}))
	}
	producer, err := rmq.NewProducer(config, options...)
	if err != nil {
		return nil, err
	}
	return &sdkProducerTransport{producer: producer}, nil
}

func (t *sdkProducerTransport) Start() error { return t.producer.Start() }

func (t *sdkProducerTransport) Send(ctx context.Context, topic, key string, body []byte) error {
	message := &rmq.Message{Topic: topic, Body: body}
	if key != "" {
		message.SetKeys(key)
	}
	_, err := t.producer.Send(ctx, message)
	return err
}

func (t *sdkProducerTransport) Stop() error { return t.producer.GracefulStop() }
