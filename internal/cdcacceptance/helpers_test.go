// Package cdcacceptance validates the Debezium/CDC acceptance matrix of
// docs/cdc-migration.md §11 without a real Kafka/Debezium deployment.
//
// The tests drive the production state repositories, the document/cache
// consumers and the cache synchronizer against the disposable MySQL test schema
// (statetest / RAG_TEST_MYSQL_URL). Broker delivery is simulated by feeding the
// raw Debezium JSON change events through the mq decoders and the workers'
// MessageHandler, which is the same path production workers consume.
package cdcacceptance

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/cacheconsumer"
	"github.com/Guan0923/Praxis-RAG/internal/documentworker"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// stateEnvNames lists every state-layer environment variable that could leak
// into the acceptance store from a developer shell; the tests pin the defaults.
var stateEnvNames = []string{
	"RAG_REDIS_URL",
	"RAG_REDIS_ENABLED",
	"RAG_REDIS_PREFIX",
	"RAG_REDIS_TTL_SECONDS",
	"RAG_REDIS_TIMEOUT_SECONDS",
	"RAG_DOCUMENT_TOPIC",
	"RAG_DOCUMENT_GROUP",
	"RAG_DOCUMENT_DLQ_TOPIC",
	"RAG_DOCUMENT_JOB_LEASE_SECONDS",
	"RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS",
	"RAG_DOCUMENT_JOB_SWEEP_SECONDS",
	"RAG_DOCUMENT_JOB_RETENTION_SECONDS",
	"RAG_SEARCH_TIMEOUT_SECONDS",
	"RAG_RESULT_TTL_SECONDS",
	"RAG_ARTIFACT_MAX_BYTES",
	"RAG_RESULT_MAX_ARTIFACTS",
}

// testLogger discards worker diagnostics so test output stays readable.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// clearStateEnv unsets the state-layer env vars for the duration of one test.
func clearStateEnv(t *testing.T) {
	t.Helper()
	for _, name := range stateEnvNames {
		value, present := os.LookupEnv(name)
		if !present {
			continue
		}
		name, value := name, value
		t.Cleanup(func() { _ = os.Setenv(name, value) })
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
}

// newStores opens the disposable MySQL schema, truncates it, and returns the
// full production Stores wiring (repositories + cache synchronizer + workers'
// state).
func newStores(t *testing.T) *state.Stores {
	t.Helper()
	clearStateEnv(t)
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New() error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

// uniqueNamespace builds a per-test namespace so repeated runs never collide.
func uniqueNamespace(t *testing.T, prefix string) string {
	t.Helper()
	buffer := make([]byte, 6)
	if _, err := rand.Read(buffer); err != nil {
		t.Fatalf("rand.Read() error = %v", err)
	}
	return prefix + "-" + hex.EncodeToString(buffer)
}

// createKnowledgeBase creates the knowledge-base row a document/search job
// validates its configuration version against.
func createKnowledgeBase(t *testing.T, stores *state.Stores, namespace, collection string) {
	t.Helper()
	if err := stores.KnowledgeBases.Create(context.Background(), map[string]any{
		"namespace":  namespace,
		"collection": collection,
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
}

// newDocumentProcessor builds the production document consumer over fake
// ingestion; the long heartbeat keeps the test free of background ticker noise.
func newDocumentProcessor(t *testing.T, stores *state.Stores, ingester documentworker.Ingester) *documentworker.Processor {
	t.Helper()
	processor, err := documentworker.New(documentworker.Options{
		Jobs:              stores.DocumentJobs,
		Ingester:          ingester,
		Logger:            testLogger(),
		HeartbeatInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("documentworker.New() error = %v", err)
	}
	return processor
}

// newCacheProcessor builds the production cache consumer over the real
// CacheSynchronizer (its loaders were registered by state.New).
func newCacheProcessor(t *testing.T, stores *state.Stores) *cacheconsumer.Processor {
	t.Helper()
	decoder := func(body []byte) (cacheconsumer.Change, error) {
		change, err := mq.DecodeCacheChange(body)
		if err != nil {
			// Preserves state.ErrIgnorableCDCChange for tombstones/DDL.
			return cacheconsumer.Change{}, err
		}
		return cacheconsumer.Change{
			Table: change.Table, Op: change.Op,
			Before: change.Before, After: change.After,
		}, nil
	}
	processor, err := cacheconsumer.New(cacheconsumer.Options{
		Refresher: stores.CacheSync,
		Decoder:   decoder,
		Logger:    testLogger(),
	})
	if err != nil {
		t.Fatalf("cacheconsumer.New() error = %v", err)
	}
	return processor
}

// deliver feeds one raw Debezium JSON event through a worker handler and reports
// whether the delivery was acknowledged (Kafka offset would be committed).
func deliver(t *testing.T, handler mq.Handler, topic string, body []byte) bool {
	t.Helper()
	return handler(context.Background(), mq.ReceivedMessage{
		Topic:           topic,
		MessageID:       "acceptance-" + topic,
		Body:            body,
		DeliveryAttempt: 1,
	})
}

// rawDocumentEvent renders one raw Debezium document_jobs change event
// (schemas.enable=false) exactly as the task connector publishes it.
func rawDocumentEvent(t *testing.T, artifactID, namespace string, version int64, op string) []byte {
	t.Helper()
	return rawChangeEvent(t, "document_jobs", op,
		map[string]any{"artifact_id": artifactID, "namespace": namespace, "schedule_version": version},
		map[string]any{"artifact_id": artifactID, "namespace": namespace, "schedule_version": version})
}

// rawCacheEvent renders one raw Debezium cache-connector change event for one
// captured business table. before is only carried for deletes and updates that
// the connector includes a before image for.
func rawCacheEvent(t *testing.T, table, op string, before, after map[string]any) []byte {
	t.Helper()
	return rawChangeEvent(t, table, op, before, after)
}

// rawChangeEvent renders a Debezium change event; row is placed before for
// deletes and after for c/r/u.
func rawChangeEvent(t *testing.T, table, op string, before, after map[string]any) []byte {
	t.Helper()
	payload := map[string]any{
		"source": map[string]any{"connector": "mysql", "table": table},
		"op":     op,
		"ts_ms":  time.Now().UnixMilli(),
	}
	switch op {
	case "d":
		payload["before"] = before
		payload["after"] = nil
	default:
		payload["before"] = before
		payload["after"] = after
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("json.Marshal(%s %s event) error = %v", table, op, err)
	}
	return encoded
}

// documentJob reads one job through the repository.
func documentJob(t *testing.T, stores *state.Stores, artifactID string) *state.DocumentJob {
	t.Helper()
	job, err := stores.DocumentJobs.Get(context.Background(), artifactID, nil)
	if err != nil {
		t.Fatalf("DocumentJobs.Get(%s) error = %v", artifactID, err)
	}
	if job == nil {
		t.Fatalf("document job %s is missing", artifactID)
	}
	return job
}

// requireDocumentJob asserts one job's status/stage/schedule_version.
func requireDocumentJob(t *testing.T, stores *state.Stores, artifactID, status, stage string, version int64) *state.DocumentJob {
	t.Helper()
	job := documentJob(t, stores, artifactID)
	if job.Status != status || job.Stage != stage || job.ScheduleVersion != version {
		t.Fatalf("document job %s = status %q stage %q version %d, want %q/%q/%d",
			artifactID, job.Status, job.Stage, job.ScheduleVersion, status, stage, version)
	}
	return job
}

// countRows runs one fixed count query (test-owned SQL only).
func countRows(t *testing.T, stores *state.Stores, query string, arguments ...any) int {
	t.Helper()
	var count int
	if err := stores.DB.QueryRowContext(context.Background(), query, arguments...).Scan(&count); err != nil {
		t.Fatalf("count query %q error = %v", query, err)
	}
	return count
}

// execSQL runs one test-owned mutation against the state schema.
func execSQL(t *testing.T, stores *state.Stores, query string, arguments ...any) {
	t.Helper()
	if _, err := stores.DB.ExecContext(context.Background(), query, arguments...); err != nil {
		t.Fatalf("exec %q error = %v", query, err)
	}
}

// schemaDeclaresTable reports whether the canonical schema still declares one
// table (a reused test database may physically retain retired tables because
// ensureSchema never drops anything).
func schemaDeclaresTable(table string) bool {
	_, found := state.TableByName(table)
	return found
}

// documentResultNames extracts the sorted document names from a cached
// documents view value.
func documentResultNames(value any) []string {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if name, ok := record["name"].(string); ok {
			names = append(names, name)
		}
	}
	return names
}

// knowledgeBaseListItem finds one namespace entry in the kb_list cache value.
func knowledgeBaseListItem(value any, namespace string) map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, item := range items {
		record, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if record["namespace"] == namespace {
			return record
		}
	}
	return nil
}

// containsCacheKey reports whether keys holds one kind/scope pair.
func containsCacheKey(keys []state.CacheKey, kind, scope string) bool {
	for _, key := range keys {
		if key.Kind == kind && key.Scope == scope {
			return true
		}
	}
	return false
}
