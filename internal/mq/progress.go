package mq

import (
	"context"
	"fmt"
	"time"

	"github.com/segmentio/kafka-go"
)

// kafkaProgressTimeout bounds one document-consumer progress read. It is
// deliberately short: the Console monitoring snapshot only needs a bounded
// observation, and a slow broker must not stall the page.
const kafkaProgressTimeout = 3 * time.Second

// ConsumerProgress is one observation of the document consumer group's
// committed offsets against the document topic's end offsets.
//
// A nil CommittedOffset/EndOffset/Lag means the value is unknown (the group has
// not committed yet, or the broker refused the read); callers must render that
// as 未知 instead of inventing a number. Lag is clamped to zero.
type ConsumerProgress struct {
	Topic string
	Group string

	CommittedOffset *int64
	EndOffset       *int64
	Lag             *int64
}

// documentProgressSource is the broker seam behind DocumentConsumerProgress.
// Tests inject a fake so no live broker is needed.
type documentProgressSource interface {
	offsets(ctx context.Context, topic, group string) (map[int]int64, map[int]int64, error)
}

// DocumentConsumerProgress reads how far the document worker's consumer group
// has advanced through the document topic. It never joins the consumer group:
// it only reads topic metadata, the per-partition end offsets and the committed
// group offsets, so it is safe to call from a monitoring request.
func DocumentConsumerProgress(ctx context.Context, settings Settings) (ConsumerProgress, error) {
	if !settings.Configured() {
		return ConsumerProgress{}, newSafeError(ErrNotConfigured, "document consumer progress", nil)
	}
	source, err := newKafkaProgressSource(settings)
	if err != nil {
		return ConsumerProgress{}, err
	}
	return documentProgressFromSource(ctx, source, settings.DocumentTopic, settings.DocumentGroup)
}

// documentProgressFromSource reads one progress observation through the seam and
// aggregates it.
func documentProgressFromSource(ctx context.Context, source documentProgressSource, topic, group string) (ConsumerProgress, error) {
	if source == nil {
		return ConsumerProgress{}, newSafeError(ErrConsumerFailed, "document consumer progress", nil)
	}
	ends, committed, err := source.offsets(ctx, topic, group)
	if err != nil {
		return ConsumerProgress{}, err
	}
	return aggregateConsumerProgress(topic, group, ends, committed), nil
}

// aggregateConsumerProgress sums the per-partition end and committed offsets.
//
// A committed offset below zero (or a partition without a committed entry) means
// the group has not committed the whole topic yet, so every number stays nil:
// the aggregate would otherwise overstate the lag by counting uncommitted
// partitions as zero progress.
func aggregateConsumerProgress(topic, group string, ends, committed map[int]int64) ConsumerProgress {
	progress := ConsumerProgress{Topic: topic, Group: group}
	if len(ends) == 0 {
		return progress
	}
	var endTotal int64
	for _, end := range ends {
		endTotal += end
	}
	var committedTotal int64
	for partition := range ends {
		committedOffset, known := committed[partition]
		if !known || committedOffset < 0 {
			return progress
		}
		committedTotal += committedOffset
	}
	lag := endTotal - committedTotal
	if lag < 0 {
		lag = 0
	}
	progress.CommittedOffset = &committedTotal
	progress.EndOffset = &endTotal
	progress.Lag = &lag
	return progress
}

// kafkaProgressSource reads offsets through the kafka-go protocol client. It
// reuses newKafkaTransport so TLS/SASL settings are honored and never opens a
// consumer-group member.
type kafkaProgressSource struct {
	client *kafka.Client
}

// newKafkaProgressSource builds the client used for one bounded progress read.
func newKafkaProgressSource(settings Settings) (*kafkaProgressSource, error) {
	transport, err := newKafkaTransport(settings)
	if err != nil {
		return nil, err
	}
	return &kafkaProgressSource{client: &kafka.Client{
		Addr:      kafka.TCP(settings.BootstrapServers...),
		Timeout:   kafkaProgressTimeout,
		Transport: transport,
	}}, nil
}

// offsets returns the last offset of every topic partition plus the consumer
// group's committed offset for the same partitions.
func (s *kafkaProgressSource) offsets(ctx context.Context, topic, group string) (map[int]int64, map[int]int64, error) {
	metadata, err := s.client.Metadata(ctx, &kafka.MetadataRequest{Topics: []string{topic}})
	if err != nil {
		return nil, nil, fmt.Errorf("读取 Kafka 主题元数据失败：%w", err)
	}
	var topicMetadata *kafka.Topic
	responseTopics := 0
	for index := range metadata.Topics {
		if metadata.Topics[index].Name != topic {
			continue
		}
		topicMetadata = &metadata.Topics[index]
		responseTopics++
	}
	if topicMetadata == nil || responseTopics != 1 {
		return nil, nil, fmt.Errorf("Kafka 主题 %q 元数据缺失", topic)
	}
	if err := topicMetadata.Error; err != nil {
		return nil, nil, fmt.Errorf("Kafka 主题 %q 元数据不可用：%w", topic, err)
	}
	partitionIDs := make([]int, 0, len(topicMetadata.Partitions))
	lastOffsetRequests := make([]kafka.OffsetRequest, 0, len(topicMetadata.Partitions))
	for _, partition := range topicMetadata.Partitions {
		if err := partition.Error; err != nil {
			return nil, nil, fmt.Errorf("Kafka 主题 %q 分区 %d 不可用：%w", topic, partition.ID, err)
		}
		partitionIDs = append(partitionIDs, partition.ID)
		lastOffsetRequests = append(lastOffsetRequests, kafka.LastOffsetOf(partition.ID))
	}
	if len(partitionIDs) == 0 {
		return nil, nil, fmt.Errorf("Kafka 主题 %q 没有分区", topic)
	}

	lastOffsets, err := s.client.ListOffsets(ctx, &kafka.ListOffsetsRequest{
		Topics: map[string][]kafka.OffsetRequest{topic: lastOffsetRequests},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("读取 Kafka 主题 %q 末端位点失败：%w", topic, err)
	}
	ends := make(map[int]int64, len(partitionIDs))
	for _, partition := range lastOffsets.Topics[topic] {
		if err := partition.Error; err != nil {
			return nil, nil, fmt.Errorf("Kafka 主题 %q 分区 %d 末端位点不可用：%w", topic, partition.Partition, err)
		}
		if partition.LastOffset < 0 {
			return nil, nil, fmt.Errorf("Kafka 主题 %q 分区 %d 末端位点未知", topic, partition.Partition)
		}
		ends[partition.Partition] = partition.LastOffset
	}
	if len(ends) != len(partitionIDs) {
		return nil, nil, fmt.Errorf("Kafka 主题 %q 末端位点不完整", topic)
	}

	fetched, err := s.client.OffsetFetch(ctx, &kafka.OffsetFetchRequest{
		GroupID: group,
		Topics:  map[string][]int{topic: partitionIDs},
	})
	if err != nil {
		return nil, nil, fmt.Errorf("读取 Kafka 消费组 %q 已提交位点失败：%w", group, err)
	}
	if err := fetched.Error; err != nil {
		return nil, nil, fmt.Errorf("Kafka 消费组 %q 位点不可用：%w", group, err)
	}
	committed := make(map[int]int64, len(fetched.Topics[topic]))
	for _, partition := range fetched.Topics[topic] {
		if err := partition.Error; err != nil {
			return nil, nil, fmt.Errorf("Kafka 消费组 %q 分区 %d 位点不可用：%w", group, partition.Partition, err)
		}
		committed[partition.Partition] = partition.CommittedOffset
	}
	return ends, committed, nil
}
