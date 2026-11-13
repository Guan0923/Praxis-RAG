package mq

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/segmentio/kafka-go"
)

// fakeMetadataSource 是 metadataSource seam：返回固定的元数据响应，
// 单测因此不需要真实 broker。
type fakeMetadataSource struct {
	response *kafka.MetadataResponse
	err      error
	calls    int
	topics   []string
}

func (f *fakeMetadataSource) metadata(_ context.Context, topic string) (*kafka.MetadataResponse, error) {
	f.calls++
	f.topics = append(f.topics, topic)
	if f.err != nil {
		return nil, f.err
	}
	return f.response, nil
}

// TestBrokerHealthFromSourceReportsClusterAndTopic 覆盖健康路径：broker 数、
// 主题存在性与分区数都来自同一次元数据响应。
func TestBrokerHealthFromSourceReportsClusterAndTopic(t *testing.T) {
	source := &fakeMetadataSource{response: &kafka.MetadataResponse{
		Brokers: []kafka.Broker{
			{ID: 1, Host: "broker-1", Port: 9092},
			{ID: 2, Host: "broker-2", Port: 9092},
			{ID: 3, Host: "broker-3", Port: 9092},
		},
		Topics: []kafka.Topic{{
			Name: "praxis_document_ingest",
			Partitions: []kafka.Partition{
				{ID: 0}, {ID: 1}, {ID: 2},
			},
		}},
	}}
	status, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest")
	if err != nil {
		t.Fatalf("brokerHealthFromSource() error = %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("source calls = %d, want 1", source.calls)
	}
	if len(source.topics) != 1 || source.topics[0] != "praxis_document_ingest" {
		t.Fatalf("probed topics = %v", source.topics)
	}
	want := BrokerStatus{Brokers: 3, Topic: "praxis_document_ingest", TopicExists: true, Partitions: 3}
	if status != want {
		t.Fatalf("status = %+v, want %+v", status, want)
	}
}

// TestBrokerHealthMissingTopicIsReachable 断言元数据里没有目标主题时 broker
// 仍算可达：只报告主题未创建，不返回错误。
func TestBrokerHealthMissingTopicIsReachable(t *testing.T) {
	source := &fakeMetadataSource{response: &kafka.MetadataResponse{
		Brokers: []kafka.Broker{{ID: 1}, {ID: 2}},
	}}
	status, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest")
	if err != nil {
		t.Fatalf("brokerHealthFromSource() error = %v", err)
	}
	if status.Brokers != 2 || status.Topic != "praxis_document_ingest" {
		t.Fatalf("status = %+v", status)
	}
	if status.TopicExists || status.Partitions != 0 {
		t.Fatalf("missing topic status = %+v, want not-exists with zero partitions", status)
	}
}

// TestBrokerHealthUnknownTopicErrorIsNotAFailure 断言 broker 对未知主题返回
// UnknownTopicOrPartition 时同样不视为探测失败。
func TestBrokerHealthUnknownTopicErrorIsNotAFailure(t *testing.T) {
	source := &fakeMetadataSource{response: &kafka.MetadataResponse{
		Brokers: []kafka.Broker{{ID: 1}},
		Topics: []kafka.Topic{{
			Name:  "praxis_document_ingest",
			Error: kafka.UnknownTopicOrPartition,
		}},
	}}
	status, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest")
	if err != nil {
		t.Fatalf("brokerHealthFromSource() error = %v", err)
	}
	if status.TopicExists {
		t.Fatalf("status = %+v, want topic to be absent", status)
	}
	if status.Brokers != 1 {
		t.Fatalf("Brokers = %d, want 1", status.Brokers)
	}
}

// TestBrokerHealthTopicAuthorizationFailure 断言主题级授权失败按探测失败处理
// （认证信息或权限配置错误不能被误报成健康）。
func TestBrokerHealthTopicAuthorizationFailure(t *testing.T) {
	source := &fakeMetadataSource{response: &kafka.MetadataResponse{
		Brokers: []kafka.Broker{{ID: 1}},
		Topics: []kafka.Topic{{
			Name:  "praxis_document_ingest",
			Error: kafka.TopicAuthorizationFailed,
		}},
	}}
	status, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest")
	if !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("error = %v, want ErrBrokerUnavailable", err)
	}
	if status.Topic != "praxis_document_ingest" || status.Brokers != 1 {
		t.Fatalf("partial status = %+v, want broker/topic useful for diagnostics", status)
	}
	if status.TopicExists {
		t.Fatalf("status = %+v, want topic to be absent", status)
	}
}

// TestBrokerHealthSourceFailureIsSanitized 断言传输层错误被映射成安全分类，
// Error() 不泄露 broker 凭据，原始错误只通过 Cause() 暴露。
func TestBrokerHealthSourceFailureIsSanitized(t *testing.T) {
	secret := "SASL password=top-secret"
	failure := errors.New(secret)
	source := &fakeMetadataSource{err: failure}
	status, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest")
	if !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("error = %v, want ErrBrokerUnavailable", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("error leaked credentials: %v", err)
	}
	var safe *SafeError
	if !errors.As(err, &safe) || !errors.Is(safe.Cause(), failure) {
		t.Fatalf("Cause() = %v, want the original failure", safe)
	}
	if status.TopicExists {
		t.Fatalf("status = %+v, want no topic on failure", status)
	}

	if _, err := brokerHealthFromSource(context.Background(), nil, "praxis_document_ingest"); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("nil source error = %v, want ErrBrokerUnavailable", err)
	}
	if _, err := brokerHealthFromSource(context.Background(), &fakeMetadataSource{}, "praxis_document_ingest"); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("nil response error = %v, want ErrBrokerUnavailable", err)
	}
}

// TestBrokerHealthEmptyClusterIsAFailure 断言元数据响应没有任何 broker 时按
// 探测失败处理，避免把异常响应渲染成健康。
func TestBrokerHealthEmptyClusterIsAFailure(t *testing.T) {
	source := &fakeMetadataSource{response: &kafka.MetadataResponse{}}
	if _, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest"); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("error = %v, want ErrBrokerUnavailable", err)
	}
}

// TestBrokerHealthDuplicateTopicEntryIsAFailure 断言同一主题在元数据里重复
// 出现时按响应异常处理，而不是随便挑一个。
func TestBrokerHealthDuplicateTopicEntryIsAFailure(t *testing.T) {
	source := &fakeMetadataSource{response: &kafka.MetadataResponse{
		Brokers: []kafka.Broker{{ID: 1}},
		Topics: []kafka.Topic{
			{Name: "praxis_document_ingest", Partitions: []kafka.Partition{{ID: 0}}},
			{Name: "praxis_document_ingest", Partitions: []kafka.Partition{{ID: 0}}},
		},
	}}
	if _, err := brokerHealthFromSource(context.Background(), source, "praxis_document_ingest"); !errors.Is(err, ErrBrokerUnavailable) {
		t.Fatalf("error = %v, want ErrBrokerUnavailable", err)
	}
}

// TestBrokerHealthNotConfigured 断言未配置 broker 的部署在拨号前就返回
// ErrNotConfigured。
func TestBrokerHealthNotConfigured(t *testing.T) {
	status, err := BrokerHealth(context.Background(), Settings{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
	if status.Brokers != 0 || status.TopicExists {
		t.Fatalf("status = %+v, want zero value", status)
	}
}

// TestBrokerHealthRejectsInvalidSecurity 断言非法的 SASL 机制在拨号前被
// 校验拦截（不会打印密码）。
func TestBrokerHealthRejectsInvalidSecurity(t *testing.T) {
	settings := Settings{
		BootstrapServers: []string{"127.0.0.1:9092"},
		SecurityProtocol: "SASL_PLAINTEXT",
		SASLMechanism:    "GSSAPI",
		SASLUsername:     "user",
		SASLPassword:     "secret",
		DocumentTopic:    "praxis_document_ingest",
		DocumentGroup:    "praxis_document_ingest_workers",
	}
	_, err := BrokerHealth(context.Background(), settings)
	var configErr *ConfigError
	if !errors.As(err, &configErr) {
		t.Fatalf("error = %v, want *ConfigError", err)
	}
	if strings.Contains(err.Error(), settings.SASLPassword) {
		t.Fatalf("error leaked credentials: %v", err)
	}
}
