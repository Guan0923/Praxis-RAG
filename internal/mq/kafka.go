package mq

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/segmentio/kafka-go"
	"github.com/segmentio/kafka-go/sasl"
	"github.com/segmentio/kafka-go/sasl/plain"
	"github.com/segmentio/kafka-go/sasl/scram"
)

// kafkaDialTimeout bounds broker connection establishment (TLS/SASL included).
const kafkaDialTimeout = 10 * time.Second

// newKafkaTLSConfig builds the TLS configuration for SSL/SASL_SSL. The CA pool
// is loaded from RAG_KAFKA_TLS_CA when set; TLSInsecure disables verification.
func newKafkaTLSConfig(settings Settings) (*tls.Config, error) {
	protocol := strings.ToUpper(strings.TrimSpace(settings.SecurityProtocol))
	if protocol != "SSL" && protocol != "SASL_SSL" {
		return nil, nil
	}
	config := &tls.Config{
		MinVersion:         tls.VersionTLS12,
		InsecureSkipVerify: settings.TLSInsecure,
	}
	if path := strings.TrimSpace(settings.TLSCA); path != "" {
		contents, err := os.ReadFile(path)
		if err != nil {
			return nil, fmt.Errorf("read kafka TLS CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(contents) {
			return nil, fmt.Errorf("kafka TLS CA is not valid PEM")
		}
		config.RootCAs = pool
	}
	return config, nil
}

// newKafkaSASLMechanism builds the SASL mechanism for SASL_* protocols.
func newKafkaSASLMechanism(settings Settings) (sasl.Mechanism, error) {
	protocol := strings.ToUpper(strings.TrimSpace(settings.SecurityProtocol))
	if protocol != "SASL_PLAINTEXT" && protocol != "SASL_SSL" {
		return nil, nil
	}
	mechanism := strings.ToUpper(strings.TrimSpace(settings.SASLMechanism))
	if mechanism == "" {
		mechanism = "PLAIN"
	}
	switch mechanism {
	case "PLAIN":
		return &plain.Mechanism{Username: settings.SASLUsername, Password: settings.SASLPassword}, nil
	case "SCRAM-SHA-256":
		return scram.Mechanism(scram.SHA256, settings.SASLUsername, settings.SASLPassword)
	case "SCRAM-SHA-512":
		return scram.Mechanism(scram.SHA512, settings.SASLUsername, settings.SASLPassword)
	default:
		return nil, newConfigError("RAG_KAFKA_SASL_MECHANISM must be PLAIN, SCRAM-SHA-256 or SCRAM-SHA-512")
	}
}

// newKafkaDialer builds the reader dialer with the resolved TLS/SASL settings.
func newKafkaDialer(settings Settings) (*kafka.Dialer, error) {
	tlsConfig, err := newKafkaTLSConfig(settings)
	if err != nil {
		return nil, err
	}
	mechanism, err := newKafkaSASLMechanism(settings)
	if err != nil {
		return nil, err
	}
	return &kafka.Dialer{
		Timeout:       kafkaDialTimeout,
		ClientID:      settings.ClientID,
		TLS:           tlsConfig,
		SASLMechanism: mechanism,
	}, nil
}

// newKafkaTransport builds the writer transport with the resolved TLS/SASL
// settings. Each writer owns one transport so Close is independent.
func newKafkaTransport(settings Settings) (*kafka.Transport, error) {
	tlsConfig, err := newKafkaTLSConfig(settings)
	if err != nil {
		return nil, err
	}
	mechanism, err := newKafkaSASLMechanism(settings)
	if err != nil {
		return nil, err
	}
	return &kafka.Transport{
		ClientID:    settings.ClientID,
		DialTimeout: kafkaDialTimeout,
		TLS:         tlsConfig,
		SASL:        mechanism,
	}, nil
}

// newKafkaWriter builds one synchronous, all-replicas writer for a topic.
func newKafkaWriter(settings Settings, topic string, writeTimeout time.Duration) (*kafka.Writer, error) {
	transport, err := newKafkaTransport(settings)
	if err != nil {
		return nil, err
	}
	if writeTimeout <= 0 {
		writeTimeout = DefaultPublishTimeout
	}
	return &kafka.Writer{
		Addr:                   kafka.TCP(settings.BootstrapServers...),
		Topic:                  topic,
		Balancer:               &kafka.LeastBytes{},
		RequiredAcks:           kafka.RequireAll,
		Async:                  false,
		BatchTimeout:           10 * time.Millisecond,
		WriteTimeout:           writeTimeout,
		Transport:              transport,
		AllowAutoTopicCreation: false,
	}, nil
}
