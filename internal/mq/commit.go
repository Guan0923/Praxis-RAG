package mq

import "sync"

// trackedMessage is one dispatched record awaiting completion. The consumer
// commits only a contiguous prefix per topic partition because a Kafka consumer
// group stores a single offset per partition: committing offset N implicitly
// commits every earlier record of that partition.
type trackedMessage struct {
	message ReceivedMessage
	done    bool
}

// partitionKey identifies one topic partition inside one consumer group.
type partitionKey struct {
	topic     string
	partition int32
}

func partitionKeyOf(message ReceivedMessage) partitionKey {
	return partitionKey{topic: message.Topic, partition: message.Partition}
}

// commitTracker orders dispatched records per partition. Records are tracked in
// delivery order (transports deliver a partition's records in offset order) and
// only the contiguous completed prefix is released for committing, so a slow or
// failed record can never be skipped by a later commit.
type commitTracker struct {
	mutex      sync.Mutex
	partitions map[partitionKey][]*trackedMessage
	// blocked partitions hit a commit error: their records stay uncommitted so
	// the batch is redelivered instead of being skipped by a later offset.
	blocked map[partitionKey]bool
}

func newCommitTracker() *commitTracker {
	return &commitTracker{
		partitions: make(map[partitionKey][]*trackedMessage),
		blocked:    make(map[partitionKey]bool),
	}
}

// track records one delivered record and returns its tracking entry. A duplicate
// delivery of a record that is still in flight returns nil: the in-flight copy
// owns the offset and will commit it.
func (t *commitTracker) track(message ReceivedMessage) *trackedMessage {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	key := partitionKeyOf(message)
	if t.blocked[key] {
		return nil
	}
	for _, existing := range t.partitions[key] {
		if existing.message.Offset == message.Offset {
			return nil
		}
	}
	entry := &trackedMessage{message: message}
	t.partitions[key] = append(t.partitions[key], entry)
	return entry
}

// complete marks one tracked record done and returns the contiguous completed
// prefix in delivery order. A blocked partition returns nothing: its offset must
// not advance past a failed commit.
func (t *commitTracker) complete(entry *trackedMessage) []ReceivedMessage {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	key := partitionKeyOf(entry.message)
	if t.blocked[key] {
		return nil
	}
	queue := t.partitions[key]
	found := false
	for _, item := range queue {
		if item == entry {
			item.done = true
			found = true
			break
		}
	}
	if !found {
		return nil
	}
	var commits []ReceivedMessage
	for len(queue) > 0 && queue[0].done {
		commits = append(commits, queue[0].message)
		queue = queue[1:]
	}
	if len(queue) == 0 {
		delete(t.partitions, key)
	} else {
		t.partitions[key] = queue
	}
	return commits
}

// block stops committing one partition for the rest of this run. Records already
// completed but uncommitted are redelivered after restart and re-processed; the
// durable job state makes that replay idempotent.
func (t *commitTracker) block(key partitionKey) {
	t.mutex.Lock()
	defer t.mutex.Unlock()
	t.blocked[key] = true
	delete(t.partitions, key)
}
