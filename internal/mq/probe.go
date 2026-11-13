package mq

import (
	"context"
	"errors"
	"time"

	"github.com/segmentio/kafka-go"
)

// kafkaProbeTimeout 限制一次 broker 元数据探测的时长。它和消费者进度读取一样
// 刻意保持较短：系统监控页只需要一次有界的观测，慢 broker 不能拖住页面。
const kafkaProbeTimeout = 3 * time.Second

// ErrBrokerUnavailable 表示 Kafka 元数据探测失败：broker 不可达、TLS/SASL
// 认证未通过、或元数据响应异常。和本包其它 SafeError 一样，Error() 只返回
// 净化后的分类，不包含 broker 地址、账号或密码。
var ErrBrokerUnavailable = errors.New("kafka broker is unavailable")

// BrokerStatus 是一次有界的 broker 集群元数据观测结果。
//
// Brokers 是元数据响应报告的 broker 数；一次成功的元数据往返同时证明 broker
// 可达、TLS/SASL 认证通过。TopicExists/Partitions 描述文档主题是否存在以及
// 分区数——这些都不是原始 TCP 端口探测能证明的。
//
// 主题不存在（或主题名非法）不算探测失败：broker 本身仍然健康，只是队列主题
// 尚未创建，因此返回 TopicExists=false 且 error 为 nil。
type BrokerStatus struct {
	Brokers     int
	Topic       string
	TopicExists bool
	Partitions  int
}

// metadataSource 是 BrokerHealth 身后的 broker seam。测试注入 fake，
// 因此单测不需要真实 broker。
type metadataSource interface {
	metadata(ctx context.Context, topic string) (*kafka.MetadataResponse, error)
}

// BrokerHealth 向 broker 发送一次 Metadata 请求（主题为文档队列主题），
// 用来证明 broker 可达、TLS/SASL 认证通过、主题存在，并统计 broker 数与
// 分区数。它不会加入任何 consumer group，可以安全地由监控请求调用。
func BrokerHealth(ctx context.Context, settings Settings) (BrokerStatus, error) {
	if !settings.Configured() {
		return BrokerStatus{}, newSafeError(ErrNotConfigured, "kafka broker health", nil)
	}
	source, err := newKafkaMetadataSource(settings)
	if err != nil {
		return BrokerStatus{}, err
	}
	return brokerHealthFromSource(ctx, source, settings.DocumentTopic)
}

// brokerHealthFromSource 通过 seam 读取一次元数据并汇总成 BrokerStatus。
func brokerHealthFromSource(ctx context.Context, source metadataSource, topic string) (BrokerStatus, error) {
	if source == nil {
		return BrokerStatus{}, newSafeError(ErrBrokerUnavailable, "kafka broker health", nil)
	}
	metadata, err := source.metadata(ctx, topic)
	if err != nil {
		return BrokerStatus{}, newSafeError(ErrBrokerUnavailable, "kafka broker health", err)
	}
	if metadata == nil {
		return BrokerStatus{}, newSafeError(ErrBrokerUnavailable, "kafka broker health", nil)
	}
	status := BrokerStatus{Topic: topic, Brokers: len(metadata.Brokers)}
	if status.Brokers == 0 {
		// 一次成功的元数据往返至少应报告 broker 自身；没有 broker 说明响应
		// 异常，不能当作健康。
		return status, newSafeError(ErrBrokerUnavailable, "kafka broker health", nil)
	}
	matches := 0
	var topicMetadata *kafka.Topic
	for index := range metadata.Topics {
		if metadata.Topics[index].Name != topic {
			continue
		}
		topicMetadata = &metadata.Topics[index]
		matches++
	}
	if matches > 1 {
		return status, newSafeError(ErrBrokerUnavailable, "kafka broker health", nil)
	}
	if topicMetadata == nil {
		// 响应里没有目标主题：broker 可达，但主题尚未创建。
		return status, nil
	}
	if err := topicMetadata.Error; err != nil {
		// UnknownTopicOrPartition / InvalidTopic 只说明主题不可用，
		// broker 与认证本身是通的；其它主题级错误（如授权失败）按探测失败处理。
		if errors.Is(err, kafka.UnknownTopicOrPartition) || errors.Is(err, kafka.InvalidTopic) {
			return status, nil
		}
		return status, newSafeError(ErrBrokerUnavailable, "kafka broker health", err)
	}
	status.TopicExists = true
	status.Partitions = len(topicMetadata.Partitions)
	return status, nil
}

// kafkaMetadataSource 通过 kafka-go 协议客户端读取元数据。它复用
// newKafkaTransport，因此 TLS/SASL 设置与 producer/consumer 完全一致，
// 并且不会打开 consumer-group 成员。
type kafkaMetadataSource struct {
	client *kafka.Client
}

// newKafkaMetadataSource 构建一次探测使用的客户端。
func newKafkaMetadataSource(settings Settings) (*kafkaMetadataSource, error) {
	transport, err := newKafkaTransport(settings)
	if err != nil {
		return nil, err
	}
	return &kafkaMetadataSource{client: &kafka.Client{
		Addr:      kafka.TCP(settings.BootstrapServers...),
		Timeout:   kafkaProbeTimeout,
		Transport: transport,
	}}, nil
}

// metadata 发送一次 Metadata 请求；主题由调用方指定。
func (s *kafkaMetadataSource) metadata(ctx context.Context, topic string) (*kafka.MetadataResponse, error) {
	return s.client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
}
