package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/connect"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/version"
)

// Test pipeline labels mirror the Kafka document queue contract; this package
// must not import internal/mq.
const (
	mqDocumentTopic = "praxis_document_ingest"
	mqDocumentGroup = "praxis_document_ingest_workers"
	mqCacheTopic    = "praxis_state_cache_refresh"
	mqCacheGroup    = "praxis_state_cache_refresh_workers"
)

// fakeStatsProvider records the call inputs and returns fixed counters.
type fakeStatsProvider struct {
	stats  *state.MonitorStats
	err    error
	now    time.Time
	window time.Duration
}

func (f *fakeStatsProvider) MonitorStats(_ context.Context, now time.Time, window time.Duration) (*state.MonitorStats, error) {
	f.now = now
	f.window = window
	return f.stats, f.err
}

// fakeMySQLPinger is the MySQL dependency probe.
type fakeMySQLPinger struct{ err error }

func (f fakeMySQLPinger) Ping(context.Context) error { return f.err }

// fakeRedisProbe is the optional cache probe.
type fakeRedisProbe struct {
	enabled bool
	err     error
}

func (f fakeRedisProbe) Enabled() bool              { return f.enabled }
func (f fakeRedisProbe) Ping(context.Context) error { return f.err }

// sampleStats returns a populated snapshot for the healthy-path assertions.
func sampleStats() *state.MonitorStats {
	return &state.MonitorStats{
		DocumentJobsByStatus: map[string]int{
			state.DocumentJobQueued: 2, state.DocumentJobProcessing: 1,
			state.DocumentJobSucceeded: 5, state.DocumentJobFailed: 1,
		},
		DocumentJobsByStage:  map[string]int{"parse": 1},
		OldestQueuedSeconds:  42.5,
		StuckProcessing:      1,
		FailedRecent:         1,
		JobsSucceededRecent:  3,
		SearchesRecent:       7,
		CacheDirtyKeys:       0,
		CacheRefreshFailures: 0,
		UploadsByStatus: map[string]int{
			state.UploadIntent: 1, state.UploadRunning: 1,
			state.UploadFailed: 0, state.UploadRollingBack: 0, state.UploadRollbackFailed: 0,
		},
		UploadsInProgress:     2,
		UploadsFailed:         0,
		UploadsRollingBack:    0,
		UploadsCleanupPending: 1,
		UnfinishedUploads:     3,
		Documents:             10,
		KnowledgeBases:        2,
		ArtifactSnapshots:     6,
	}
}

// qdrantServer serves the collections listing the Qdrant probe reads.
func qdrantServer(t *testing.T, collections ...string) *httptest.Server {
	t.Helper()
	names := make([]string, 0, len(collections))
	for _, collection := range collections {
		names = append(names, `{"name":"`+collection+`"}`)
	}
	body := `{"result":{"collections":[` + strings.Join(names, ",") + `]}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Path == "/collections" {
			_, _ = w.Write([]byte(body))
			return
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// indexedCollectionsFunc returns a fixed indexed-collection list for the probe.
func indexedCollectionsFunc(collections ...string) func(context.Context) ([]string, error) {
	return func(context.Context) ([]string, error) {
		return append([]string(nil), collections...), nil
	}
}

// qdrantProbeOptions builds a snapshot whose only inspected dependency is Qdrant.
func qdrantProbeOptions(t *testing.T, url string) Options {
	t.Helper()
	return Options{
		Stats:      &fakeStatsProvider{stats: sampleStats()},
		MySQL:      fakeMySQLPinger{},
		Redis:      fakeRedisProbe{enabled: false},
		QdrantURL:  url,
		PDFEngine:  "go",
		UploadMode: "async",
		Delivery:   "debezium",
	}
}

// pdfServer serves the remote vision health endpoint with a healthy converter payload.
func pdfServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"service":"praxis-rag-pdf-service","version":"1.0.0",` +
			`"status":"ok","converter":"vision","converter_requested":"vision",` +
			`"vision_configured":false}`))
	}))
	t.Cleanup(server.Close)
	return server
}

// jsonServer serves one path with a fixed status and body.
func jsonServer(t *testing.T, path string, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if path != "" && r.URL.Path != path {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// kafkaListener opens a real TCP port so the Kafka reachability probe succeeds.
func kafkaListener(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			connection, acceptErr := listener.Accept()
			if acceptErr != nil {
				return
			}
			_ = connection.Close()
		}
	}()
	return listener.Addr().String()
}

// unreachableAddress returns a loopback address nothing listens on.
func unreachableAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen() error = %v", err)
	}
	address := listener.Addr().String()
	_ = listener.Close()
	return address
}

// dependencyByName resolves one dependency row from the snapshot.
func dependencyByName(t *testing.T, overview map[string]any, name string) dependency {
	t.Helper()
	items, ok := overview["dependencies"].([]dependency)
	if !ok {
		t.Fatalf("dependencies has type %T", overview["dependencies"])
	}
	for _, item := range items {
		if item.Name == name {
			return item
		}
	}
	t.Fatalf("dependency %q not found in %+v", name, items)
	return dependency{}
}

func healthyOptions(t *testing.T) Options {
	t.Helper()
	qdrant := qdrantServer(t, "alpha", "knowledge")
	pdf := pdfServer(t, http.StatusOK)
	lightragService := lightragHealthyServer(t)
	return Options{
		Stats:                 &fakeStatsProvider{stats: sampleStats()},
		MySQL:                 fakeMySQLPinger{},
		DBStats:               func() sql.DBStats { return sql.DBStats{OpenConnections: 4, InUse: 1, Idle: 3, MaxOpenConnections: 20} },
		Redis:                 fakeRedisProbe{enabled: true},
		QdrantURL:             qdrant.URL,
		FallbackCollection:    "knowledge",
		IndexedCollections:    indexedCollectionsFunc("knowledge"),
		KafkaBootstrapServers: []string{kafkaListener(t)},
		KafkaProbe: func(context.Context) (KafkaStatus, error) {
			return KafkaStatus{Brokers: 1, Topic: mqDocumentTopic, TopicExists: true, Partitions: 3}, nil
		},
		ConnectorProbe:    healthyConnectProbe(),
		DocumentConnector: connectDocumentName,
		CacheConnector:    connectCacheName,
		DocumentTopic:     mqDocumentTopic,
		DocumentGroup:     mqDocumentGroup,
		CacheTopic:        mqCacheTopic,
		CacheGroup:        mqCacheGroup,
		KafkaGroupProgress: func(_ context.Context, topic, _ string) (KafkaProgress, error) {
			return fakeGroupProgress(topic), nil
		},
		WorkerHeartbeat: func(context.Context) (*WorkerHeartbeat, error) {
			return &WorkerHeartbeat{UpdatedAt: time.Now().Add(-2 * time.Second), Instances: 1}, nil
		},
		Delivery:               "debezium",
		UploadMode:             "async",
		PDFEngine:              pdfEngineVision,
		PDFServiceURL:          pdf.URL,
		LightRAGServiceURL:     lightragService.URL,
		LightRAGServiceToken:   "test-lightrag-token",
		LightRAGServiceTimeout: 5 * time.Second,
		StartedAt:              time.Now().Add(-90 * time.Second),
	}
}

// fakeGroupProgress returns a deterministic committed/end snapshot per topic so
// the two-group assertions need no broker.
func fakeGroupProgress(topic string) KafkaProgress {
	committed, end := int64(90), int64(95)
	switch topic {
	case mqCacheTopic:
		committed, end = 3, 7
	}
	lag := end - committed
	return KafkaProgress{CommittedOffset: &committed, EndOffset: &end, Lag: &lag}
}

// Connect connector names and a fake probe: the monitor only consumes the
// connect.ConnectorHealth document, so unit tests need no REST server.
const (
	connectDocumentName = "praxis-document-cdc"
	connectCacheName    = "praxis-cache-cdc"
)

// fakeConnectorProbe returns fixed health documents for the connectors the
// monitor asks about.
type fakeConnectorProbe struct {
	health map[string]connect.ConnectorHealth
	errs   map[string]error
}

func (f *fakeConnectorProbe) Health(_ context.Context, connector string) (connect.ConnectorHealth, error) {
	if err, ok := f.errs[connector]; ok {
		return f.health[connector], err
	}
	if health, ok := f.health[connector]; ok {
		return health, nil
	}
	return connect.ConnectorHealth{Connector: connector, State: connect.StateUnknown}, nil
}

// runningConnector builds a fully healthy connector with one RUNNING task.
func runningConnector(name, worker string) connect.ConnectorHealth {
	return connect.ConnectorHealth{
		Connector:         name,
		State:             connect.StateRunning,
		WorkerID:          worker,
		Tasks:             []connect.TaskHealth{{ID: 0, State: connect.StateRunning, WorkerID: worker}},
		SupervisorEnabled: true,
	}
}

// healthyConnectProbe returns both connectors healthy on two distinct workers.
func healthyConnectProbe() *fakeConnectorProbe {
	return &fakeConnectorProbe{health: map[string]connect.ConnectorHealth{
		connectDocumentName: runningConnector(connectDocumentName, "connect-1:8083"),
		connectCacheName:    runningConnector(connectCacheName, "connect-2:8083"),
	}}
}

// pipelineOf resolves the always-present pipeline object from the snapshot.
func pipelineOf(t *testing.T, overview map[string]any) pipelineSnapshot {
	t.Helper()
	pipeline, ok := overview["pipeline"].(pipelineSnapshot)
	if !ok {
		t.Fatalf("pipeline has type %T", overview["pipeline"])
	}
	return pipeline
}

// pipelineTestOptions returns minimal options with available stats and no
// dependency noise; tests override the pipeline-specific fields.
func pipelineTestOptions(stats *state.MonitorStats) Options {
	return Options{
		Stats:             &fakeStatsProvider{stats: stats},
		MySQL:             fakeMySQLPinger{},
		PDFEngine:         "go",
		DocumentConnector: connectDocumentName,
		CacheConnector:    connectCacheName,
	}
}

// TestMonitorOverviewHealthy asserts the full snapshot shape and every
// dependency transition on a healthy process.
func TestMonitorOverviewHealthy(t *testing.T) {
	overview := New(healthyOptions(t)).MonitorOverview(context.Background())

	if overview["ok"] != true {
		t.Fatalf("ok = %v", overview["ok"])
	}
	generatedAt, ok := overview["generated_at"].(string)
	if !ok || !strings.HasSuffix(generatedAt, "Z") {
		t.Fatalf("generated_at = %v", overview["generated_at"])
	}
	if _, err := time.Parse(time.RFC3339, generatedAt); err != nil {
		t.Fatalf("generated_at is not RFC3339: %v", err)
	}
	// A fully healthy snapshot carries the explicit three-state status.
	if overview["status"] != statusOK {
		t.Fatalf("status = %v", overview["status"])
	}

	service, ok := overview["service"].(map[string]any)
	if !ok {
		t.Fatalf("service has type %T", overview["service"])
	}
	if service["name"] != version.ServiceName || service["version"] != version.Version ||
		service["backend"] != "go" || service["collection"] != "knowledge" {
		t.Fatalf("service = %v", service)
	}
	if service["upload_mode"] != "async" || service["delivery"] != "debezium" ||
		service["pdf_engine"] != pdfEngineVision {
		t.Fatalf("service = %v", service)
	}
	if _, present := service["outbox_bridge"]; present {
		t.Fatalf("service.outbox_bridge should be removed: %v", service)
	}
	if uptime, _ := service["uptime_seconds"].(float64); uptime < 89 {
		t.Fatalf("uptime_seconds = %v", service["uptime_seconds"])
	}

	for _, name := range []string{"mysql", "redis", "qdrant", "kafka", "pdf-service", "lightrag"} {
		item := dependencyByName(t, overview, name)
		if item.Status != statusOK {
			t.Fatalf("dependency %q status = %q detail = %q", name, item.Status, item.Detail)
		}
		if item.Label == "" || item.Detail == "" {
			t.Fatalf("dependency %q = %+v", name, item)
		}
	}
	// The Qdrant row reports the collections the pipeline actually writes to.
	qdrantDependency := dependencyByName(t, overview, "qdrant")
	if !strings.Contains(qdrantDependency.Detail, "集合 2 个") ||
		!strings.Contains(qdrantDependency.Detail, "knowledge 已就绪") {
		t.Fatalf("qdrant detail = %q", qdrantDependency.Detail)
	}
	// Kafka is verified with a metadata round trip, not a raw TCP dial.
	kafkaDependency := dependencyByName(t, overview, "kafka")
	if !strings.Contains(kafkaDependency.Detail, "元数据正常") ||
		!strings.Contains(kafkaDependency.Detail, "1 个 broker") ||
		!strings.Contains(kafkaDependency.Detail, "3 个分区") {
		t.Fatalf("kafka detail = %q", kafkaDependency.Detail)
	}
	// The remote vision health payload selects the converter instead of trusting a 200.
	pdfDependency := dependencyByName(t, overview, "pdf-service")
	if !strings.Contains(pdfDependency.Detail, "转换器 vision") {
		t.Fatalf("pdf detail = %q", pdfDependency.Detail)
	}
	// The graph sidecar reports its workspace count, build version and the
	// upstream package version pair that detects an image drift.
	lightragDependency := dependencyByName(t, overview, "lightrag")
	if !strings.Contains(lightragDependency.Detail, "工作区 2 个") ||
		!strings.Contains(lightragDependency.Detail, "lightrag-hku 1.5.7（与 pin 一致）") ||
		lightragDependency.Upstream == nil || lightragDependency.Upstream.Drift {
		t.Fatalf("lightrag = %+v", lightragDependency)
	}

	queues, ok := overview["queues"].(map[string]any)
	if !ok {
		t.Fatalf("queues has type %T", overview["queues"])
	}
	jobs, _ := queues["document_jobs"].(map[string]any)
	if jobs == nil {
		t.Fatalf("document_jobs = %v", queues["document_jobs"])
	}
	byStatus, _ := jobs["by_status"].(map[string]int)
	if byStatus[state.DocumentJobQueued] != 2 || byStatus[state.DocumentJobSucceeded] != 5 {
		t.Fatalf("document_jobs.by_status = %v", byStatus)
	}
	if jobs["oldest_queued_seconds"] != 42.5 || jobs["stuck_processing"] != 1 ||
		jobs["failed_recent"] != 1 {
		t.Fatalf("document_jobs = %v", jobs)
	}
	if stages, _ := jobs["by_stage"].(map[string]int); stages["parse"] != 1 {
		t.Fatalf("document_jobs.by_stage = %v", jobs["by_stage"])
	}
	uploads, _ := queues["uploads"].(map[string]any)
	if uploads == nil || uploads["unfinished"] != 3 || uploads["in_progress"] != 2 ||
		uploads["failed"] != 0 || uploads["rolling_back"] != 0 ||
		uploads["cleanup_pending"] != 1 {
		t.Fatalf("uploads = %v", queues["uploads"])
	}
	uploadStatuses, _ := uploads["by_status"].(map[string]int)
	if uploadStatuses[state.UploadIntent] != 1 || uploadStatuses[state.UploadRunning] != 1 {
		t.Fatalf("uploads.by_status = %v", uploads["by_status"])
	}
	// The outbox record statistics moved out of queues into the pipeline object;
	// the queue section must not present PENDING rows as undelivered backlog.
	if _, present := queues["document_outbox"]; present {
		t.Fatalf("queues.document_outbox still present: %v", queues["document_outbox"])
	}
	if len(queues) != 2 {
		t.Fatalf("queues = %v, want exactly document_jobs/uploads", queues)
	}

	pipeline := pipelineOf(t, overview)
	if pipeline.Cache.Status != statusOK || pipeline.Cache.DirtyKeys == nil ||
		*pipeline.Cache.DirtyKeys != 0 || pipeline.Cache.RefreshFailures == nil ||
		*pipeline.Cache.RefreshFailures != 0 {
		t.Fatalf("pipeline.cache = %+v", pipeline.Cache)
	}
	// sampleStats has a stuck lease and a recent failure, so the job view
	// degrades while still reporting the raw counters.
	if pipeline.DocumentJobs.Status != statusDegraded {
		t.Fatalf("pipeline.document_jobs = %+v", pipeline.DocumentJobs)
	}
	if !strings.Contains(pipeline.DocumentJobs.Detail, "排队 2、处理中 1、成功 5、失败 1") ||
		!strings.Contains(pipeline.DocumentJobs.Detail, "租约过期 1 个") {
		t.Fatalf("pipeline.document_jobs.detail = %q", pipeline.DocumentJobs.Detail)
	}
	// The worker row is fed by a fresh heartbeat, not the static mode string.
	if pipeline.Worker.Status != statusOK ||
		!strings.Contains(pipeline.Worker.Detail, "最近心跳") ||
		!strings.Contains(pipeline.Worker.Detail, "1 个实例运行中") {
		t.Fatalf("pipeline.worker = %+v", pipeline.Worker)
	}
	// healthyOptions wires both named Kafka Connect connectors and the two-group
	// progress reader, so capture state is known while the binlog offset stays
	// null for both connectors.
	if pipeline.Debezium.Tasks.Status != statusOK ||
		!strings.Contains(pipeline.Debezium.Tasks.Detail, "praxis-document-cdc RUNNING") ||
		!strings.Contains(pipeline.Debezium.Tasks.Detail, "task 0 RUNNING@connect-1:8083") ||
		pipeline.Debezium.Tasks.CaptureProgress != nil {
		t.Fatalf("pipeline.debezium.tasks = %+v", pipeline.Debezium.Tasks)
	}
	if pipeline.Debezium.Cache.Status != statusOK ||
		!strings.Contains(pipeline.Debezium.Cache.Detail, "praxis-cache-cdc RUNNING") ||
		!strings.Contains(pipeline.Debezium.Cache.Detail, "task 0 RUNNING@connect-2:8083") ||
		pipeline.Debezium.Cache.CaptureProgress != nil {
		t.Fatalf("pipeline.debezium.cache = %+v", pipeline.Debezium.Cache)
	}
	if pipeline.Kafka.Status != statusOK ||
		!strings.Contains(pipeline.Kafka.Detail, "文档组滞后 5") ||
		!strings.Contains(pipeline.Kafka.Detail, "缓存组滞后 4") {
		t.Fatalf("pipeline.kafka = %+v", pipeline.Kafka)
	}
	if pipeline.Kafka.Groups.Document.ConsumerLag == nil || *pipeline.Kafka.Groups.Document.ConsumerLag != 5 ||
		pipeline.Kafka.Groups.Cache.ConsumerLag == nil || *pipeline.Kafka.Groups.Cache.ConsumerLag != 4 {
		t.Fatalf("pipeline.kafka.groups = %+v", pipeline.Kafka.Groups)
	}
	if pipeline.Kafka.Groups.Document.Topic != mqDocumentTopic ||
		pipeline.Kafka.Groups.Document.Group != mqDocumentGroup ||
		pipeline.Kafka.Groups.Cache.Topic != mqCacheTopic {
		t.Fatalf("pipeline.kafka.groups labels = %+v", pipeline.Kafka.Groups)
	}

	library, _ := overview["library"].(map[string]any)
	if library == nil || library["documents"] != 10 || library["knowledge_bases"] != 2 ||
		library["artifact_snapshots"] != 6 {
		t.Fatalf("library = %v", overview["library"])
	}
	activity, _ := overview["activity"].(map[string]any)
	if activity == nil || activity["window_hours"] != 24.0 ||
		activity["jobs_succeeded"] != 3 || activity["jobs_failed"] != 1 ||
		activity["searches"] != 7 {
		t.Fatalf("activity = %v", overview["activity"])
	}
	if errorsList, _ := overview["errors"].([]string); len(errorsList) != 0 {
		t.Fatalf("errors = %v", overview["errors"])
	}
}

// TestMonitorQdrantUsesIndexedCollections pins the collection rule of the
// Qdrant dependency row: it requires the collections that live documents are
// actually indexed into and never the RAG_COLLECTION fallback. Normal
// deployments store one generated knowledge_* collection per knowledge base
// and never create the fallback name, so requiring the fallback reported a
// healthy service as degraded.
func TestMonitorQdrantUsesIndexedCollections(t *testing.T) {
	const fallback = "mini_agent_knowledge_pdf_assets_v1"
	const indexed = "knowledge_e0fd7ba996b0e4d3adeb72b1bde6e853"

	t.Run("回退集合未创建不降级", func(t *testing.T) {
		options := qdrantProbeOptions(t, qdrantServer(t, indexed).URL)
		options.FallbackCollection = fallback
		options.IndexedCollections = indexedCollectionsFunc(indexed)
		overview := New(options).MonitorOverview(context.Background())
		item := dependencyByName(t, overview, "qdrant")
		if item.Status != statusOK || overview["status"] != statusOK {
			t.Fatalf("qdrant = %+v, status = %v", item, overview["status"])
		}
		if !strings.Contains(item.Detail, "集合 1 个") ||
			!strings.Contains(item.Detail, indexed+" 已就绪") {
			t.Fatalf("detail = %q", item.Detail)
		}
	})

	t.Run("入库集合缺失降级", func(t *testing.T) {
		options := qdrantProbeOptions(t, qdrantServer(t, fallback).URL)
		options.FallbackCollection = fallback
		options.IndexedCollections = indexedCollectionsFunc(indexed)
		overview := New(options).MonitorOverview(context.Background())
		item := dependencyByName(t, overview, "qdrant")
		if item.Status != statusDegraded ||
			!strings.Contains(item.Detail, "入库集合 "+indexed+" 不存在") {
			t.Fatalf("qdrant = %+v", item)
		}
		if overview["status"] != statusDegraded {
			t.Fatalf("status = %v", overview["status"])
		}
	})

	t.Run("多个入库集合缺失点名部分", func(t *testing.T) {
		options := qdrantProbeOptions(t, qdrantServer(t, "knowledge_a").URL)
		options.IndexedCollections = indexedCollectionsFunc(
			"knowledge_a", "knowledge_b", "knowledge_c", "knowledge_d", "knowledge_e")
		overview := New(options).MonitorOverview(context.Background())
		item := dependencyByName(t, overview, "qdrant")
		if item.Status != statusDegraded ||
			!strings.Contains(item.Detail, "4 个入库集合不存在") ||
			!strings.Contains(item.Detail, "knowledge_b、knowledge_c、knowledge_d 等") {
			t.Fatalf("qdrant = %+v", item)
		}
	})

	t.Run("没有已入库文档集合", func(t *testing.T) {
		options := qdrantProbeOptions(t, qdrantServer(t).URL)
		options.FallbackCollection = fallback
		options.IndexedCollections = indexedCollectionsFunc()
		overview := New(options).MonitorOverview(context.Background())
		item := dependencyByName(t, overview, "qdrant")
		if item.Status != statusOK || !strings.Contains(item.Detail, "集合 0 个；暂无入库集合") {
			t.Fatalf("qdrant = %+v, status = %v", item, overview["status"])
		}
	})

	t.Run("入库集合读取失败只记为未知", func(t *testing.T) {
		options := qdrantProbeOptions(t, qdrantServer(t, indexed).URL)
		options.IndexedCollections = func(context.Context) ([]string, error) {
			return nil, errors.New("state query failed")
		}
		overview := New(options).MonitorOverview(context.Background())
		item := dependencyByName(t, overview, "qdrant")
		if item.Status != statusUnknown || overview["status"] != statusOK {
			t.Fatalf("qdrant = %+v, status = %v", item, overview["status"])
		}
	})

	t.Run("空白与重复名称归一", func(t *testing.T) {
		options := qdrantProbeOptions(t, qdrantServer(t, "knowledge_a").URL)
		options.IndexedCollections = indexedCollectionsFunc(
			" knowledge_a ", "", "knowledge_a", "knowledge_a")
		overview := New(options).MonitorOverview(context.Background())
		item := dependencyByName(t, overview, "qdrant")
		if item.Status != statusOK || !strings.Contains(item.Detail, "knowledge_a 已就绪") {
			t.Fatalf("qdrant = %+v", item)
		}
	})
}

// TestMonitorOverviewDegradedAndDisabled covers the non-error states: a missing
// Qdrant collection degrades and unconfigured optional services disable.
func TestMonitorOverviewDegradedAndDisabled(t *testing.T) {
	qdrant := qdrantServer(t, "alpha", "beta")
	options := Options{
		Stats:              &fakeStatsProvider{stats: sampleStats()},
		MySQL:              fakeMySQLPinger{},
		QdrantURL:          qdrant.URL,
		FallbackCollection: "knowledge",
		IndexedCollections: indexedCollectionsFunc("knowledge"),
		PDFEngine:          "go",
		UploadMode:         "sync",
		Delivery:           "debezium",
		DocumentConnector:  connectDocumentName,
		CacheConnector:     connectCacheName,
	}
	overview := New(options).MonitorOverview(context.Background())

	// A degraded dependency must never read as 运行正常: the header follows
	// status=degraded while the snapshot itself still succeeds with all sections.
	if overview["status"] != statusDegraded || overview["ok"] != false {
		t.Fatalf("status/ok = %v/%v, want degraded/false", overview["status"], overview["ok"])
	}
	if item := dependencyByName(t, overview, "qdrant"); item.Status != statusDegraded ||
		!strings.Contains(item.Detail, "入库集合 knowledge 不存在") {
		t.Fatalf("qdrant status = %q detail = %q", item.Status, item.Detail)
	}
	if item := dependencyByName(t, overview, "redis"); item.Status != statusDisabled ||
		item.Detail != "未启用（可选）" {
		t.Fatalf("redis = %+v", item)
	}
	if item := dependencyByName(t, overview, "kafka"); item.Status != statusDisabled ||
		!strings.Contains(item.Detail, "未配置 Kafka") {
		t.Fatalf("kafka = %+v", item)
	}
	if item := dependencyByName(t, overview, "pdf-service"); item.Status != statusDisabled ||
		item.Detail != "Go 内置解析引擎" {
		t.Fatalf("pdf = %+v", item)
	}
	if item := dependencyByName(t, overview, "mysql"); item.Status != statusOK {
		t.Fatalf("mysql = %+v", item)
	}
	pipeline := pipelineOf(t, overview)
	if pipeline.Debezium.Tasks.Status != statusUnknown ||
		!strings.Contains(pipeline.Debezium.Tasks.Detail, "未配置 Kafka Connect 状态探测") {
		t.Fatalf("pipeline.debezium.tasks = %+v", pipeline.Debezium.Tasks)
	}
	if pipeline.Debezium.Cache.Status != statusUnknown ||
		!strings.Contains(pipeline.Debezium.Cache.Detail, "未配置 Kafka Connect 状态探测") {
		t.Fatalf("pipeline.debezium.cache = %+v", pipeline.Debezium.Cache)
	}
	if pipeline.Kafka.Status != statusDisabled ||
		pipeline.Kafka.Detail != "未配置 Kafka：任务/缓存通知不会投递" {
		t.Fatalf("pipeline.kafka = %+v", pipeline.Kafka)
	}
}

// TestMonitorOverviewErrors asserts a failed probe marks ok=false while the
// statistics sections stay populated.
func TestMonitorOverviewErrors(t *testing.T) {
	unreachable := unreachableAddress(t)
	options := Options{
		Stats:         &fakeStatsProvider{stats: sampleStats()},
		MySQL:         fakeMySQLPinger{err: errors.New("db down")},
		Redis:         fakeRedisProbe{enabled: true, err: errors.New("redis down")},
		QdrantURL:     "http://" + unreachable,
		PDFEngine:     pdfEngineVision,
		PDFServiceURL: "http://" + unreachable,
		UploadMode:    "async",
		Delivery:      "debezium",
	}
	overview := New(options).MonitorOverview(context.Background())

	if overview["ok"] != false || overview["status"] != statusError {
		t.Fatalf("status/ok = %v/%v", overview["status"], overview["ok"])
	}
	for _, name := range []string{"mysql", "redis", "qdrant", "pdf-service"} {
		if item := dependencyByName(t, overview, name); item.Status != statusError {
			t.Fatalf("dependency %q = %+v", name, item)
		}
	}
	if item := dependencyByName(t, overview, "kafka"); item.Status != statusDisabled {
		t.Fatalf("kafka = %+v", item)
	}
	if overview["queues"] == nil || overview["library"] == nil || overview["activity"] == nil {
		t.Fatalf("statistics sections were dropped: %v/%v/%v",
			overview["queues"], overview["library"], overview["activity"])
	}
	pipeline := pipelineOf(t, overview)
	if pipeline.Cache.Status != statusOK || pipeline.Cache.DirtyKeys == nil ||
		*pipeline.Cache.DirtyKeys != 0 {
		t.Fatalf("pipeline.cache = %+v", pipeline.Cache)
	}
	if pipeline.Kafka.Status != statusDisabled {
		t.Fatalf("pipeline.kafka = %+v", pipeline.Kafka)
	}
}

// TestMonitorOverviewStatsFailure asserts a failed state aggregate nulls the
// queue/library/activity sections and reports a Chinese error summary.
func TestMonitorOverviewStatsFailure(t *testing.T) {
	options := Options{
		Stats:     &fakeStatsProvider{err: errors.New("state aggregation failed")},
		MySQL:     fakeMySQLPinger{},
		Redis:     fakeRedisProbe{enabled: false},
		PDFEngine: "go",
	}
	overview := New(options).MonitorOverview(context.Background())

	if overview["ok"] != false || overview["status"] != statusError {
		t.Fatalf("status/ok = %v/%v, want error/false", overview["status"], overview["ok"])
	}
	if overview["queues"] != nil || overview["library"] != nil || overview["activity"] != nil {
		t.Fatalf("sections = %v/%v/%v, want nil",
			overview["queues"], overview["library"], overview["activity"])
	}
	pipeline := pipelineOf(t, overview)
	if pipeline.Cache.Status != statusUnknown || pipeline.Cache.DirtyKeys != nil ||
		pipeline.Cache.RefreshFailures != nil {
		t.Fatalf("pipeline.cache = %+v, want unknown/null without statistics", pipeline.Cache)
	}
	if pipeline.DocumentJobs.Status != statusUnknown ||
		pipeline.DocumentJobs.Detail != "状态库不可用，任务状态未知" {
		t.Fatalf("pipeline.document_jobs = %+v", pipeline.DocumentJobs)
	}
	// The pipeline object is always present: unknown capture health must not
	// disappear with the statistics.
	if pipeline.Debezium.Tasks.Status != statusUnknown ||
		pipeline.Debezium.Tasks.CaptureProgress != nil ||
		pipeline.Debezium.Cache.Status != statusUnknown {
		t.Fatalf("pipeline.debezium = %+v", pipeline.Debezium)
	}
	if pipeline.Kafka.Status != statusDisabled {
		t.Fatalf("pipeline.kafka = %+v", pipeline.Kafka)
	}
	errorsList, ok := overview["errors"].([]string)
	if !ok || len(errorsList) != 1 {
		t.Fatalf("errors = %v", overview["errors"])
	}
	if !strings.Contains(errorsList[0], "监控统计不可用") {
		t.Fatalf("errors[0] = %q", errorsList[0])
	}
	if item := dependencyByName(t, overview, "mysql"); item.Status != statusOK {
		t.Fatalf("mysql = %+v", item)
	}
}

// TestMonitorOverviewProbeTimeout asserts one slow dependency is cut off at the
// probe deadline instead of delaying the whole snapshot.
func TestMonitorOverviewProbeTimeout(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(slow.Close)
	options := Options{
		Stats:              &fakeStatsProvider{stats: sampleStats()},
		MySQL:              fakeMySQLPinger{},
		Redis:              fakeRedisProbe{enabled: false},
		QdrantURL:          slow.URL,
		FallbackCollection: "knowledge",
		PDFEngine:          "go",
	}
	started := time.Now()
	overview := New(options).MonitorOverview(context.Background())
	elapsed := time.Since(started)
	if elapsed > 3*time.Second {
		t.Fatalf("MonitorOverview took %v, want a bounded probe", elapsed)
	}
	if item := dependencyByName(t, overview, "qdrant"); item.Status != statusError {
		t.Fatalf("qdrant = %+v", item)
	}
}

// TestMonitorPipelineDocumentJobsDegraded covers the three degradation triggers
// and the healthy concatenated detail.
func TestMonitorPipelineDocumentJobsDegraded(t *testing.T) {
	cases := []struct {
		name   string
		stats  *state.MonitorStats
		status string
		detail string
	}{
		{
			name: "ok",
			stats: &state.MonitorStats{
				DocumentJobsByStatus: map[string]int{
					state.DocumentJobQueued: 2, state.DocumentJobProcessing: 1,
					state.DocumentJobSucceeded: 5, state.DocumentJobFailed: 1,
				},
				OldestQueuedSeconds: 42.5,
			},
			status: statusOK,
			detail: "排队 2、处理中 1、成功 5、失败 1；最老排队 42.5 秒",
		},
		{
			name: "stuck",
			stats: &state.MonitorStats{
				DocumentJobsByStatus: map[string]int{state.DocumentJobProcessing: 1},
				StuckProcessing:      1,
			},
			status: statusDegraded,
			detail: "租约过期 1 个",
		},
		{
			name: "recent failure",
			stats: &state.MonitorStats{
				DocumentJobsByStatus: map[string]int{state.DocumentJobFailed: 2},
				FailedRecent:         2,
			},
			status: statusDegraded,
			detail: "近 24 小时失败 2 个",
		},
		{
			name: "old queue",
			stats: &state.MonitorStats{
				DocumentJobsByStatus: map[string]int{state.DocumentJobQueued: 1},
				OldestQueuedSeconds:  700,
			},
			status: statusDegraded,
			detail: "最老排队超过 600 秒",
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			overview := New(pipelineTestOptions(testCase.stats)).MonitorOverview(context.Background())
			view := pipelineOf(t, overview)
			if view.DocumentJobs.Status != testCase.status {
				t.Fatalf("status = %q, want %q (detail %q)",
					view.DocumentJobs.Status, testCase.status, view.DocumentJobs.Detail)
			}
			if !strings.Contains(view.DocumentJobs.Detail, testCase.detail) {
				t.Fatalf("detail = %q, want substring %q", view.DocumentJobs.Detail, testCase.detail)
			}
		})
	}
}

// TestMonitorPipelineConnect covers the Kafka Connect capture rows: missing
// configuration, both connectors healthy, a failed task with an in-flight
// supervisor restart, an exhausted restart budget, a recovered task, a paused
// connector and an unreadable status. Capture progress is always null because
// Kafka Connect owns the binlog offset.
func TestMonitorPipelineConnect(t *testing.T) {
	missing := pipelineTestOptions(sampleStats())
	missing.DocumentConnector = ""
	missing.CacheConnector = ""
	view := pipelineOf(t, New(missing).MonitorOverview(context.Background()))
	if view.Debezium.Tasks.Status != statusUnknown ||
		view.Debezium.Tasks.Detail != "未配置文档采集连接器名称" {
		t.Fatalf("pipeline.debezium.tasks = %+v", view.Debezium.Tasks)
	}
	if view.Debezium.Cache.Status != statusUnknown ||
		view.Debezium.Cache.Detail != "未配置缓存采集连接器名称" {
		t.Fatalf("pipeline.debezium.cache = %+v", view.Debezium.Cache)
	}

	t.Run("未配置状态探测", func(t *testing.T) {
		view := pipelineOf(t, New(pipelineTestOptions(sampleStats())).MonitorOverview(context.Background()))
		if view.Debezium.Tasks.Status != statusUnknown ||
			!strings.Contains(view.Debezium.Tasks.Detail, "未配置 Kafka Connect 状态探测") {
			t.Fatalf("pipeline.debezium.tasks = %+v", view.Debezium.Tasks)
		}
		if view.Debezium.Tasks.CaptureProgress != nil {
			t.Fatalf("capture_progress must stay null: %+v", view.Debezium.Tasks)
		}
	})

	t.Run("两个连接器均健康", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = healthyConnectProbe()
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Tasks.Status != statusOK ||
			!strings.Contains(view.Debezium.Tasks.Detail, "task 0 RUNNING@connect-1:8083") ||
			!strings.Contains(view.Debezium.Tasks.Detail, "采集位点由 Kafka Connect 保存") {
			t.Fatalf("pipeline.debezium.tasks = %+v", view.Debezium.Tasks)
		}
		if view.Debezium.Cache.Status != statusOK ||
			!strings.Contains(view.Debezium.Cache.Detail, "task 0 RUNNING@connect-2:8083") {
			t.Fatalf("pipeline.debezium.cache = %+v", view.Debezium.Cache)
		}
		if view.Debezium.Tasks.CaptureProgress != nil || view.Debezium.Cache.CaptureProgress != nil {
			t.Fatalf("capture_progress must stay null: %+v", view.Debezium)
		}
	})

	t.Run("RUNNING 但尚未调度 task", func(t *testing.T) {
		unscheduled := runningConnector(connectDocumentName, "connect-1:8083")
		unscheduled.Tasks = nil
		probe := healthyConnectProbe()
		probe.health[connectDocumentName] = unscheduled
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = probe
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Tasks.Status != statusDegraded ||
			!strings.Contains(view.Debezium.Tasks.Detail, "尚未调度 task") {
			t.Fatalf("pipeline.debezium.tasks = %+v", view.Debezium.Tasks)
		}
		// The other capture route keeps its healthy verdict.
		if view.Debezium.Cache.Status != statusOK {
			t.Fatalf("pipeline.debezium.cache = %+v", view.Debezium.Cache)
		}
	})

	t.Run("失败任务正在重启", func(t *testing.T) {
		failed := runningConnector(connectDocumentName, "connect-1:8083")
		failed.Tasks = []connect.TaskHealth{{ID: 0, State: connect.StateFailed, WorkerID: "connect-1:8083",
			Trace: "Access denied for user 'debezium'"}}
		failed.FailedTasks = 1
		failed.FailureReason = "Access denied for user 'debezium'"
		failed.Restarting = true
		failed.RestartAttempts = 2
		failed.Recovery = connect.RecoveryRestarting
		failed.NextRestartAt = "2026-11-15T06:31:00Z"
		probe := healthyConnectProbe()
		probe.health[connectDocumentName] = failed
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = probe
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Tasks.Status != statusError ||
			!view.Debezium.Tasks.Restarting ||
			view.Debezium.Tasks.RestartAttempts != 2 ||
			view.Debezium.Tasks.FailedTasks != 1 ||
			!strings.Contains(view.Debezium.Tasks.FailureReason, "Access denied") ||
			!strings.Contains(view.Debezium.Tasks.Detail, "正在自动重启（第 2 次）") {
			t.Fatalf("pipeline.debezium.tasks = %+v", view.Debezium.Tasks)
		}
		if view.Debezium.Cache.Status != statusOK {
			t.Fatalf("the other capture path must stay healthy: %+v", view.Debezium.Cache)
		}
	})

	t.Run("重启次数耗尽", func(t *testing.T) {
		failed := runningConnector(connectCacheName, "connect-2:8083")
		failed.Tasks = []connect.TaskHealth{{ID: 0, State: connect.StateFailed, WorkerID: "connect-2:8083",
			Trace: "Unknown database 'praxis_rag'"}}
		failed.FailedTasks = 1
		failed.FailureReason = "Unknown database 'praxis_rag'"
		failed.RestartExhausted = true
		failed.RestartAttempts = 5
		failed.Recovery = connect.RecoveryExhausted
		probe := healthyConnectProbe()
		probe.health[connectCacheName] = failed
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = probe
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Cache.Status != statusError ||
			!view.Debezium.Cache.RestartExhausted ||
			view.Debezium.Cache.RestartAttempts != 5 ||
			!strings.Contains(view.Debezium.Cache.Detail, "自动重启已耗尽（5 次），需人工处理") {
			t.Fatalf("pipeline.debezium.cache = %+v", view.Debezium.Cache)
		}
	})

	t.Run("重启后已恢复", func(t *testing.T) {
		recovered := runningConnector(connectCacheName, "connect-2:8083")
		recovered.Recovery = connect.RecoveryRecovered
		recovered.RestartAttempts = 1
		recovered.LastRestartAt = "2026-11-15T06:20:00Z"
		recovered.LastRecoveredAt = "2026-11-15T06:20:08Z"
		probe := healthyConnectProbe()
		probe.health[connectCacheName] = recovered
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = probe
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Cache.Status != statusOK ||
			view.Debezium.Cache.Recovery != connect.RecoveryRecovered ||
			!strings.Contains(view.Debezium.Cache.Detail, "上次失败后已自动恢复") {
			t.Fatalf("pipeline.debezium.cache = %+v", view.Debezium.Cache)
		}
	})

	t.Run("连接器已暂停", func(t *testing.T) {
		paused := runningConnector(connectCacheName, "connect-2:8083")
		paused.State = connect.StatePaused
		paused.Tasks = []connect.TaskHealth{{ID: 0, State: connect.StatePaused, WorkerID: "connect-2:8083"}}
		probe := healthyConnectProbe()
		probe.health[connectCacheName] = paused
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = probe
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Cache.Status != statusDegraded ||
			!strings.Contains(view.Debezium.Cache.Detail, "已暂停，另一路采集不受影响") {
			t.Fatalf("pipeline.debezium.cache = %+v", view.Debezium.Cache)
		}
	})

	t.Run("状态不可读", func(t *testing.T) {
		probe := healthyConnectProbe()
		probe.errs = map[string]error{connectDocumentName: errors.New("connect worker unreachable")}
		options := pipelineTestOptions(sampleStats())
		options.ConnectorProbe = probe
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Tasks.Status != statusError ||
			!strings.Contains(view.Debezium.Tasks.Detail, "状态读取失败") {
			t.Fatalf("pipeline.debezium.tasks = %+v", view.Debezium.Tasks)
		}
	})
}

// TestMonitorPipelineKafka covers the two independent consumer-group rows:
// disabled without a broker, unknown when the reader is not wired or the group
// has not committed, ok with per-group lag, and an error that nulls the numbers.
func TestMonitorPipelineKafka(t *testing.T) {
	labels := func(options Options) Options {
		options.DocumentTopic = mqDocumentTopic
		options.DocumentGroup = mqDocumentGroup
		options.CacheTopic = mqCacheTopic
		options.CacheGroup = mqCacheGroup
		return options
	}
	t.Run("未配置 Kafka", func(t *testing.T) {
		options := labels(pipelineTestOptions(sampleStats()))
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusDisabled ||
			view.Kafka.Detail != "未配置 Kafka：任务/缓存通知不会投递" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.Topic != mqDocumentTopic || view.Kafka.Group != mqDocumentGroup {
			t.Fatalf("pipeline.kafka legacy labels = %q/%q", view.Kafka.Topic, view.Kafka.Group)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
		for _, name := range []string{"document", "cache"} {
			row := pipelineKafkaGroupByName(view.Kafka.Groups, name)
			if row.Status != statusDisabled {
				t.Fatalf("pipeline.kafka.groups.%s = %+v", name, row)
			}
		}
	})
	t.Run("未装配进度读取", func(t *testing.T) {
		options := labels(pipelineTestOptions(sampleStats()))
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusUnknown ||
			view.Kafka.Groups.Document.Detail != "未装配 Kafka 进度读取" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
	})
	t.Run("两组进度正常", func(t *testing.T) {
		options := labels(pipelineTestOptions(sampleStats()))
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaGroupProgress = func(_ context.Context, topic, _ string) (KafkaProgress, error) {
			return fakeGroupProgress(topic), nil
		}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusOK || !strings.Contains(view.Kafka.Detail, "文档组滞后 5") ||
			!strings.Contains(view.Kafka.Detail, "缓存组滞后 4") {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset == nil || *view.Kafka.CommittedOffset != 90 ||
			view.Kafka.ConsumerLag == nil || *view.Kafka.ConsumerLag != 5 {
			t.Fatalf("document mirror = %+v", view.Kafka)
		}
		if view.Kafka.Groups.Cache.ConsumerLag == nil || *view.Kafka.Groups.Cache.ConsumerLag != 4 {
			t.Fatalf("groups = %+v", view.Kafka.Groups)
		}
	})
	t.Run("未提交位点", func(t *testing.T) {
		options := labels(pipelineTestOptions(sampleStats()))
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaGroupProgress = func(context.Context, string, string) (KafkaProgress, error) {
			return KafkaProgress{}, nil
		}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusUnknown ||
			view.Kafka.Groups.Document.Detail != "文档任务消费组尚未提交位点" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
	})
	t.Run("读取失败", func(t *testing.T) {
		options := labels(pipelineTestOptions(sampleStats()))
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaGroupProgress = func(context.Context, string, string) (KafkaProgress, error) {
			return KafkaProgress{}, errors.New("broker unavailable")
		}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusError ||
			!strings.Contains(view.Kafka.Groups.Document.Detail, "文档任务消费进度读取失败：") {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
	})
}

// pipelineKafkaGroupByName resolves one group row for table-driven assertions.
func pipelineKafkaGroupByName(groups pipelineKafkaGroups, name string) pipelineKafkaGroup {
	switch name {
	case "document":
		return groups.Document
	default:
		return groups.Cache
	}
}

// decodeOverview marshals a snapshot and decodes it as plain JSON so the
// assertions see exactly the wire document the Console consumes.
func decodeOverview(t *testing.T, overview map[string]any) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(overview)
	if err != nil {
		t.Fatalf("json.Marshal(overview) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("json.Unmarshal(overview) error = %v", err)
	}
	return document
}

// requireKeys asserts a JSON object exposes exactly the expected keys.
func requireKeys(t *testing.T, label string, object map[string]any, keys ...string) {
	t.Helper()
	if len(object) != len(keys) {
		t.Fatalf("%s keys = %v, want %v", label, object, keys)
	}
	for _, key := range keys {
		if _, present := object[key]; !present {
			t.Fatalf("%s misses key %q: %v", label, key, object)
		}
	}
}

// TestMonitorPipelineWireShape pins the JSON contract: queues no longer carries
// document_outbox, the pipeline object is always present with the documented
// keys, and unknown progress is JSON null.
func TestMonitorPipelineWireShape(t *testing.T) {
	document := decodeOverview(t, New(pipelineTestOptions(sampleStats())).MonitorOverview(context.Background()))
	queues, _ := document["queues"].(map[string]any)
	if _, present := queues["document_outbox"]; present {
		t.Fatalf("queues.document_outbox = %v, want removed", queues["document_outbox"])
	}
	if _, present := document["status"]; !present {
		t.Fatalf("status missing from the wire document: %v", document)
	}
	// The uploads section exposes the split counters instead of one mixed
	// "unfinished" number the Console could mistake for files being uploaded.
	uploads, ok := queues["uploads"].(map[string]any)
	if !ok {
		t.Fatalf("queues.uploads has type %T", queues["uploads"])
	}
	requireKeys(t, "queues.uploads", uploads, "unfinished", "by_status", "in_progress",
		"failed", "rolling_back", "cleanup_pending")

	// Search results are returned inline and never stored, so the library only
	// exposes the durable counters (documents/knowledge_bases/artifact_snapshots).
	library, ok := document["library"].(map[string]any)
	if !ok {
		t.Fatalf("library has type %T", document["library"])
	}
	requireKeys(t, "library", library, "documents", "knowledge_bases", "artifact_snapshots")
	if library["artifact_snapshots"] != 6.0 {
		t.Fatalf("library.artifact_snapshots = %v, want 6", library["artifact_snapshots"])
	}
	pipeline, ok := document["pipeline"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline has type %T", document["pipeline"])
	}
	requireKeys(t, "pipeline", pipeline, "document_jobs", "worker", "debezium", "kafka", "cache")

	worker, ok := pipeline["worker"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.worker has type %T", pipeline["worker"])
	}
	requireKeys(t, "pipeline.worker", worker, "status", "detail")

	jobs, ok := pipeline["document_jobs"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.document_jobs has type %T", pipeline["document_jobs"])
	}
	requireKeys(t, "pipeline.document_jobs", jobs, "status", "detail")

	debezium, ok := pipeline["debezium"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.debezium has type %T", pipeline["debezium"])
	}
	requireKeys(t, "pipeline.debezium", debezium, "tasks", "cache")
	for _, name := range []string{"tasks", "cache"} {
		connector, ok := debezium[name].(map[string]any)
		if !ok {
			t.Fatalf("pipeline.debezium.%s has type %T", name, debezium[name])
		}
		requireKeys(t, "pipeline.debezium."+name, connector, "status", "detail", "capture_progress",
			"connector", "connector_state", "worker_id", "tasks", "failed_tasks",
			"failure_reason", "restarting", "restart_attempts", "restart_exhausted",
			"recovery", "last_restart_at", "last_recovered_at", "next_restart_at", "supervised")
		if connector["capture_progress"] != nil {
			t.Fatalf("pipeline.debezium.%s.capture_progress = %v, want null",
				name, connector["capture_progress"])
		}
	}

	kafka, ok := pipeline["kafka"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.kafka has type %T", pipeline["kafka"])
	}
	requireKeys(t, "pipeline.kafka", kafka, "status", "detail", "topic", "group",
		"committed_offset", "end_offset", "consumer_lag", "groups")
	groups, ok := kafka["groups"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.kafka.groups has type %T", kafka["groups"])
	}
	requireKeys(t, "pipeline.kafka.groups", groups, "document", "cache")
	for _, name := range []string{"document", "cache"} {
		group, ok := groups[name].(map[string]any)
		if !ok {
			t.Fatalf("pipeline.kafka.groups.%s has type %T", name, groups[name])
		}
		requireKeys(t, "pipeline.kafka.groups."+name, group, "status", "detail", "topic",
			"group", "committed_offset", "end_offset", "consumer_lag")
	}
	// pipelineTestOptions wires no broker, so the legacy document mirror stays
	// null instead of a fabricated zero.
	if kafka["committed_offset"] != nil || kafka["end_offset"] != nil || kafka["consumer_lag"] != nil {
		t.Fatalf("pipeline.kafka = %v, want null numbers", kafka)
	}

	cache, ok := pipeline["cache"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.cache has type %T", pipeline["cache"])
	}
	requireKeys(t, "pipeline.cache", cache, "status", "dirty_keys", "refresh_failures")
	if cache["dirty_keys"] != 0.0 || cache["refresh_failures"] != 0.0 {
		t.Fatalf("pipeline.cache = %v", cache)
	}
	if _, present := pipeline["outbox"]; present {
		t.Fatalf("pipeline.outbox should be removed: %v", pipeline["outbox"])
	}

	// A failed statistics read nulls the queue/library/activity sections and the
	// cache counters while the always-present pipeline object keeps the capture
	// and consumption rows.
	failed := decodeOverview(t, New(Options{
		Stats:     &fakeStatsProvider{err: errors.New("state aggregation failed")},
		MySQL:     fakeMySQLPinger{},
		PDFEngine: "go",
	}).MonitorOverview(context.Background()))
	failedPipeline, _ := failed["pipeline"].(map[string]any)
	if failedPipeline == nil {
		t.Fatalf("pipeline = %v, want present", failed["pipeline"])
	}
	failedCache, _ := failedPipeline["cache"].(map[string]any)
	if failedCache == nil || failedCache["dirty_keys"] != nil || failedCache["refresh_failures"] != nil {
		t.Fatalf("pipeline.cache = %v, want null numbers", failedPipeline["cache"])
	}
	failedJobs, _ := failedPipeline["document_jobs"].(map[string]any)
	if failedJobs["status"] != statusUnknown {
		t.Fatalf("pipeline.document_jobs = %v", failedJobs)
	}
	if failedPipeline["debezium"] == nil || failedPipeline["kafka"] == nil {
		t.Fatalf("pipeline = %v, want debezium/kafka present", failedPipeline)
	}
}

// healthyDependencyOptions is the base for the status-aggregation tests: every
// required dependency is healthy, optional services stay disabled and the
// worker/debezium/heartbeat views stay unknown.
func healthyDependencyOptions(t *testing.T) Options {
	t.Helper()
	qdrant := qdrantServer(t, "knowledge")
	return Options{
		Stats:              &fakeStatsProvider{stats: sampleStats()},
		MySQL:              fakeMySQLPinger{},
		Redis:              fakeRedisProbe{enabled: false},
		QdrantURL:          qdrant.URL,
		FallbackCollection: "knowledge",
		IndexedCollections: indexedCollectionsFunc("knowledge"),
		PDFEngine:          "go",
		DocumentConnector:  connectDocumentName,
		CacheConnector:     connectCacheName,
	}
}

// TestMonitorOverallStatusAggregation pins the three-state header: a degraded
// dependency must not read as 运行正常, and only infra failures turn it red.
func TestMonitorOverallStatusAggregation(t *testing.T) {
	t.Run("全部正常", func(t *testing.T) {
		overview := New(healthyDependencyOptions(t)).MonitorOverview(context.Background())
		if overview["status"] != statusOK || overview["ok"] != true {
			t.Fatalf("status/ok = %v/%v", overview["status"], overview["ok"])
		}
	})
	t.Run("依赖降级", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.QdrantURL = qdrantServer(t, "other").URL
		overview := New(options).MonitorOverview(context.Background())
		if overview["status"] != statusDegraded || overview["ok"] != false {
			t.Fatalf("status/ok = %v/%v, want degraded/false", overview["status"], overview["ok"])
		}
	})
	t.Run("依赖异常", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.MySQL = fakeMySQLPinger{err: errors.New("db down")}
		overview := New(options).MonitorOverview(context.Background())
		if overview["status"] != statusError || overview["ok"] != false {
			t.Fatalf("status/ok = %v/%v, want error/false", overview["status"], overview["ok"])
		}
	})
	t.Run("连接器采集异常", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		failed := runningConnector(connectDocumentName, "connect-1:8083")
		failed.Tasks = []connect.TaskHealth{{ID: 0, State: connect.StateFailed, WorkerID: "connect-1:8083",
			Trace: "boom"}}
		failed.FailedTasks = 1
		failed.FailureReason = "boom"
		probe := healthyConnectProbe()
		probe.health[connectDocumentName] = failed
		options.ConnectorProbe = probe
		overview := New(options).MonitorOverview(context.Background())
		if overview["status"] != statusError {
			t.Fatalf("status = %v, want error", overview["status"])
		}
	})
	t.Run("Worker 心跳过期", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.WorkerHeartbeat = func(context.Context) (*WorkerHeartbeat, error) {
			return &WorkerHeartbeat{UpdatedAt: time.Now().Add(-5 * time.Minute), Instances: 1}, nil
		}
		overview := New(options).MonitorOverview(context.Background())
		if overview["status"] != statusDegraded {
			t.Fatalf("status = %v, want degraded", overview["status"])
		}
		if view := pipelineOf(t, overview); view.Worker.Status != statusDegraded {
			t.Fatalf("pipeline.worker = %+v", view.Worker)
		}
	})
	t.Run("Kafka 进度读取失败仅降级", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.DocumentTopic = mqDocumentTopic
		options.DocumentGroup = mqDocumentGroup
		options.CacheTopic = mqCacheTopic
		options.CacheGroup = mqCacheGroup
		options.KafkaGroupProgress = func(context.Context, string, string) (KafkaProgress, error) {
			return KafkaProgress{}, errors.New("offset fetch denied")
		}
		overview := New(options).MonitorOverview(context.Background())
		if overview["status"] != statusDegraded {
			t.Fatalf("status = %v, want degraded", overview["status"])
		}
		if view := pipelineOf(t, overview); view.Kafka.Status != statusError {
			t.Fatalf("pipeline.kafka = %+v, want error", view.Kafka)
		}
	})
}

// TestMonitorPipelineWorker covers the heartbeat-fed worker row: a fresh beat is
// healthy, a stale beat degrades, and an unobserved worker stays unknown.
func TestMonitorPipelineWorker(t *testing.T) {
	cases := []struct {
		name   string
		beat   *WorkerHeartbeat
		err    error
		status string
		detail string
	}{
		{name: "运行中", beat: &WorkerHeartbeat{UpdatedAt: time.Now().Add(-2 * time.Second), Instances: 2},
			status: statusOK, detail: "2 个实例运行中"},
		{name: "心跳过期", beat: &WorkerHeartbeat{UpdatedAt: time.Now().Add(-3 * time.Minute), Instances: 1},
			status: statusDegraded, detail: "可能已停止"},
		{name: "未上报", status: statusUnknown, detail: "未收到文档 worker 心跳"},
		{name: "读取失败", err: errors.New("heartbeat read failed"),
			status: statusUnknown, detail: "Worker 心跳读取失败"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			options := pipelineTestOptions(sampleStats())
			options.WorkerHeartbeat = func(context.Context) (*WorkerHeartbeat, error) {
				return testCase.beat, testCase.err
			}
			view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
			if view.Worker.Status != testCase.status ||
				!strings.Contains(view.Worker.Detail, testCase.detail) {
				t.Fatalf("pipeline.worker = %+v, want %q containing %q",
					view.Worker, testCase.status, testCase.detail)
			}
		})
	}
}

// TestMonitorLibraryCounters asserts the library counters come from the MySQL
// state aggregate and disappear when statistics fail. Search results are no
// longer stored, so the library exposes only durable counters.
func TestMonitorLibraryCounters(t *testing.T) {
	t.Run("有计数", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.Stats = &fakeStatsProvider{stats: sampleStats()}
		library, _ := New(options).MonitorOverview(context.Background())["library"].(map[string]any)
		if library["documents"] != 10 || library["knowledge_bases"] != 2 ||
			library["artifact_snapshots"] != 6 {
			t.Fatalf("library = %v", library)
		}
		if _, present := library["result_snapshots"]; present {
			t.Fatalf("library.result_snapshots should be removed: %v", library)
		}
	})
	t.Run("统计不可用", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.Stats = &fakeStatsProvider{err: errors.New("state aggregation failed")}
		overview := New(options).MonitorOverview(context.Background())
		if overview["library"] != nil {
			t.Fatalf("library = %v, want null", overview["library"])
		}
	})
}

// TestMonitorKafkaMetadataProbe asserts the dependency row uses cluster
// metadata (brokers/topic/partitions) instead of a bare TCP dial.
func TestMonitorKafkaMetadataProbe(t *testing.T) {
	t.Run("元数据正常", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaProbe = func(context.Context) (KafkaStatus, error) {
			return KafkaStatus{Brokers: 2, Topic: mqDocumentTopic, TopicExists: true, Partitions: 6}, nil
		}
		item := dependencyByName(t, New(options).MonitorOverview(context.Background()), "kafka")
		if item.Status != statusOK || !strings.Contains(item.Detail, "2 个 broker") ||
			!strings.Contains(item.Detail, "6 个分区") || !strings.Contains(item.Detail, "元数据正常") {
			t.Fatalf("kafka = %+v", item)
		}
	})
	t.Run("读取失败", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaProbe = func(context.Context) (KafkaStatus, error) {
			return KafkaStatus{}, errors.New("metadata failed")
		}
		item := dependencyByName(t, New(options).MonitorOverview(context.Background()), "kafka")
		if item.Status != statusError || !strings.Contains(item.Detail, "Kafka 元数据读取失败") {
			t.Fatalf("kafka = %+v", item)
		}
	})
	t.Run("主题不存在", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.DocumentTopic = mqDocumentTopic
		options.KafkaProbe = func(context.Context) (KafkaStatus, error) {
			return KafkaStatus{Brokers: 1, Topic: mqDocumentTopic, TopicExists: false}, nil
		}
		item := dependencyByName(t, New(options).MonitorOverview(context.Background()), "kafka")
		if item.Status != statusDegraded || !strings.Contains(item.Detail, "不存在") {
			t.Fatalf("kafka = %+v", item)
		}
	})
}

// TestMonitorPDFHealthPayload asserts the remote vision probe reads the converter
// state instead of only the HTTP status code.
func TestMonitorPDFHealthPayload(t *testing.T) {
	visionOptions := func(t *testing.T, server *httptest.Server) Options {
		t.Helper()
		options := healthyDependencyOptions(t)
		options.PDFEngine = pdfEngineVision
		options.PDFServiceURL = server.URL
		return options
	}
	t.Run("转换器就绪", func(t *testing.T) {
		server := jsonServer(t, "/healthz", http.StatusOK,
			`{"status":"ok","converter":"vision","version":"1.2.3","vision_configured":true}`)
		item := dependencyByName(t, New(visionOptions(t, server)).MonitorOverview(context.Background()), "pdf-service")
		if item.Status != statusOK || !strings.Contains(item.Detail, "转换器 vision") ||
			!strings.Contains(item.Detail, "版本 1.2.3") || !strings.Contains(item.Detail, "视觉模型已配置") {
			t.Fatalf("pdf-service = %+v", item)
		}
	})
	t.Run("转换器不可用", func(t *testing.T) {
		server := jsonServer(t, "/healthz", http.StatusOK,
			`{"status":"degraded","converter":"vision","config_error":"vision model missing"}`)
		item := dependencyByName(t, New(visionOptions(t, server)).MonitorOverview(context.Background()), "pdf-service")
		if item.Status != statusDegraded || !strings.Contains(item.Detail, "vision model missing") {
			t.Fatalf("pdf-service = %+v", item)
		}
	})
	t.Run("响应不可解析", func(t *testing.T) {
		server := jsonServer(t, "/healthz", http.StatusOK, `<html>ok</html>`)
		item := dependencyByName(t, New(visionOptions(t, server)).MonitorOverview(context.Background()), "pdf-service")
		if item.Status != statusDegraded || item.Detail != "健康检查响应无法解析" {
			t.Fatalf("pdf-service = %+v", item)
		}
	})
	t.Run("HTTP 失败", func(t *testing.T) {
		server := jsonServer(t, "/healthz", http.StatusInternalServerError, `{}`)
		item := dependencyByName(t, New(visionOptions(t, server)).MonitorOverview(context.Background()), "pdf-service")
		if item.Status != statusError || !strings.Contains(item.Detail, "HTTP 500") {
			t.Fatalf("pdf-service = %+v", item)
		}
	})
}
