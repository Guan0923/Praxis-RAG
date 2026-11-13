package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

// Test pipeline labels mirror the Kafka document queue contract; this package
// must not import internal/mq.
const (
	mqDocumentTopic = "praxis_document_ingest"
	mqDocumentGroup = "praxis_document_ingest_workers"
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
		DocumentJobsByStage:       map[string]int{"parse": 1},
		OldestQueuedSeconds:       42.5,
		StuckProcessing:           1,
		FailedRecent:              1,
		JobsSucceededRecent:       3,
		OutboxByStatus:            map[string]int{"PENDING": 2, "PUBLISHED": 3},
		OldestOutboxRecordSeconds: 12,
		SearchByStatus: map[string]int{
			state.SearchTaskQueued: 1, state.SearchTaskProcessing: 2,
			state.SearchTaskSucceeded: 3, state.SearchTaskFailed: 0,
			state.SearchTaskTimedOut: 1,
		},
		OldestActiveSearchSeconds: 5,
		SearchesRecent:            7,
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

// qdrantServer serves the two endpoints the probe reads.
func qdrantServer(t *testing.T, collection string, points int64, missing bool) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections":
			_, _ = w.Write([]byte(`{"result":{"collections":[{"name":"alpha"},{"name":"beta"}]}}`))
		case r.Method == http.MethodGet && r.URL.Path == "/collections/"+collection:
			if missing {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":{"error":"Not found: Collection ` + collection + ` doesn't exist!"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"status":"green","points_count":` +
				strconv.FormatInt(points, 10) + `}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	t.Cleanup(server.Close)
	return server
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
	qdrant := qdrantServer(t, "knowledge", 123, false)
	pdf := pdfServer(t, http.StatusOK)
	return Options{
		Stats:                 &fakeStatsProvider{stats: sampleStats()},
		MySQL:                 fakeMySQLPinger{},
		DBStats:               func() sql.DBStats { return sql.DBStats{OpenConnections: 4, InUse: 1, Idle: 3, MaxOpenConnections: 20} },
		Redis:                 fakeRedisProbe{enabled: true},
		QdrantURL:             qdrant.URL,
		Collection:            "knowledge",
		KafkaBootstrapServers: []string{kafkaListener(t)},
		KafkaProbe: func(context.Context) (KafkaStatus, error) {
			return KafkaStatus{Brokers: 1, Topic: mqDocumentTopic, TopicExists: true, Partitions: 3}, nil
		},
		DocumentTopic: mqDocumentTopic,
		DocumentGroup: mqDocumentGroup,
		ResultSnapshots: func(context.Context) (int, error) {
			return 4, nil
		},
		WorkerHeartbeat: func(context.Context) (*WorkerHeartbeat, error) {
			return &WorkerHeartbeat{UpdatedAt: time.Now().Add(-2 * time.Second), Instances: 1}, nil
		},
		OutboxBridge:  "debezium",
		UploadMode:    "async",
		PDFEngine:     pdfEngineVision,
		PDFServiceURL: pdf.URL,
		StartedAt:     time.Now().Add(-90 * time.Second),
	}
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
		Stats:     &fakeStatsProvider{stats: stats},
		MySQL:     fakeMySQLPinger{},
		PDFEngine: "go",
	}
}

// debeziumServer serves a fixed health payload.
func debeziumServer(t *testing.T, status int, body string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server
}

// int64Pointer is the pointer form the pipeline JSON uses for known numbers.
func int64Pointer(value int64) *int64 { return &value }

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
	if service["upload_mode"] != "async" || service["outbox_bridge"] != "debezium" ||
		service["pdf_engine"] != pdfEngineVision {
		t.Fatalf("service = %v", service)
	}
	if uptime, _ := service["uptime_seconds"].(float64); uptime < 89 {
		t.Fatalf("uptime_seconds = %v", service["uptime_seconds"])
	}

	for _, name := range []string{"mysql", "redis", "qdrant", "kafka", "pdf-service"} {
		item := dependencyByName(t, overview, name)
		if item.Status != statusOK {
			t.Fatalf("dependency %q status = %q detail = %q", name, item.Status, item.Detail)
		}
		if item.Label == "" || item.Detail == "" {
			t.Fatalf("dependency %q = %+v", name, item)
		}
		if item.LatencyMS < 0 {
			t.Fatalf("dependency %q latency = %v", name, item.LatencyMS)
		}
	}
	qdrantDependency := dependencyByName(t, overview, "qdrant")
	if qdrantDependency.Points == nil || *qdrantDependency.Points != 123 {
		t.Fatalf("qdrant points = %v", qdrantDependency.Points)
	}
	if !strings.Contains(qdrantDependency.Detail, "集合 2 个") {
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
	searches, _ := queues["search_tasks"].(map[string]any)
	if searches == nil || searches["oldest_active_seconds"] != 5.0 {
		t.Fatalf("search_tasks = %v", queues["search_tasks"])
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
	if len(queues) != 3 {
		t.Fatalf("queues = %v, want exactly document_jobs/search_tasks/uploads", queues)
	}

	pipeline := pipelineOf(t, overview)
	if pipeline.Outbox == nil {
		t.Fatal("pipeline.outbox = nil with available stats")
	}
	if pipeline.Outbox.Total != 5 || pipeline.Outbox.ByStatus["PENDING"] != 2 ||
		pipeline.Outbox.ByStatus["PUBLISHED"] != 3 ||
		pipeline.Outbox.OldestRecordSeconds != 12.0 {
		t.Fatalf("pipeline.outbox = %+v", pipeline.Outbox)
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
	// healthyOptions wires neither a Debezium health URL nor a progress reader,
	// so both capture and consumption progress are explicitly unknown.
	if pipeline.Debezium.Status != statusUnknown ||
		pipeline.Debezium.Detail != "未配置 Debezium 健康检查地址，采集进度未知" ||
		pipeline.Debezium.CaptureProgress != nil {
		t.Fatalf("pipeline.debezium = %+v", pipeline.Debezium)
	}
	if pipeline.Kafka.Status != statusUnknown || pipeline.Kafka.Detail != "未装配 Kafka 进度读取" ||
		pipeline.Kafka.CommittedOffset != nil || pipeline.Kafka.EndOffset != nil ||
		pipeline.Kafka.ConsumerLag != nil {
		t.Fatalf("pipeline.kafka = %+v", pipeline.Kafka)
	}

	library, _ := overview["library"].(map[string]any)
	if library == nil || library["documents"] != 10 || library["knowledge_bases"] != 2 ||
		library["result_snapshots"] != 4 || library["artifact_snapshots"] != 6 {
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

// TestMonitorOverviewDegradedAndDisabled covers the non-error states: a missing
// Qdrant collection degrades and unconfigured optional services disable.
func TestMonitorOverviewDegradedAndDisabled(t *testing.T) {
	qdrant := qdrantServer(t, "knowledge", 0, true)
	options := Options{
		Stats:        &fakeStatsProvider{stats: sampleStats()},
		MySQL:        fakeMySQLPinger{},
		QdrantURL:    qdrant.URL,
		Collection:   "knowledge",
		PDFEngine:    "go",
		UploadMode:   "sync",
		OutboxBridge: "debezium",
	}
	overview := New(options).MonitorOverview(context.Background())

	// A degraded dependency must never read as 运行正常: the header follows
	// status=degraded while the snapshot itself still succeeds with all sections.
	if overview["status"] != statusDegraded || overview["ok"] != false {
		t.Fatalf("status/ok = %v/%v, want degraded/false", overview["status"], overview["ok"])
	}
	if item := dependencyByName(t, overview, "qdrant"); item.Status != statusDegraded {
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
	if pipeline.Debezium.Status != statusUnknown ||
		pipeline.Debezium.Detail != "未配置 Debezium 健康检查地址，采集进度未知" {
		t.Fatalf("pipeline.debezium = %+v", pipeline.Debezium)
	}
	if pipeline.Kafka.Status != statusDisabled ||
		pipeline.Kafka.Detail != "未配置 Kafka：文档通知不会投递" {
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
		Collection:    "knowledge",
		PDFEngine:     pdfEngineVision,
		PDFServiceURL: "http://" + unreachable,
		UploadMode:    "async",
		OutboxBridge:  "debezium",
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
	if pipeline.Outbox == nil || pipeline.Outbox.Total != 5 {
		t.Fatalf("pipeline.outbox = %+v", pipeline.Outbox)
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
	if pipeline.Outbox != nil {
		t.Fatalf("pipeline.outbox = %+v, want nil without statistics", pipeline.Outbox)
	}
	if pipeline.DocumentJobs.Status != statusUnknown ||
		pipeline.DocumentJobs.Detail != "状态库不可用，任务状态未知" {
		t.Fatalf("pipeline.document_jobs = %+v", pipeline.DocumentJobs)
	}
	// The pipeline object is always present: unknown capture progress and the
	// disabled in-process delivery must not disappear with the statistics.
	if pipeline.Debezium.Status != statusUnknown || pipeline.Debezium.CaptureProgress != nil {
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
		Stats:      &fakeStatsProvider{stats: sampleStats()},
		MySQL:      fakeMySQLPinger{},
		Redis:      fakeRedisProbe{enabled: false},
		QdrantURL:  slow.URL,
		Collection: "knowledge",
		PDFEngine:  "go",
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

// TestMonitorPipelineOutboxSeparatedFromJobs asserts a succeeded document whose
// outbox row is still PENDING reads as a healthy job pipeline: the PENDING row
// is a retained Debezium record, not an undelivered backlog or a failure.
func TestMonitorPipelineOutboxSeparatedFromJobs(t *testing.T) {
	stats := &state.MonitorStats{
		DocumentJobsByStatus: map[string]int{
			state.DocumentJobQueued: 0, state.DocumentJobProcessing: 0,
			state.DocumentJobSucceeded: 1, state.DocumentJobFailed: 0,
		},
		OutboxByStatus:            map[string]int{"PENDING": 1},
		OldestOutboxRecordSeconds: 30,
	}
	options := pipelineTestOptions(stats)
	// Kafka is configured but its progress read is unavailable; the PENDING
	// outbox row must still not degrade the job view.
	options.KafkaBootstrapServers = []string{kafkaListener(t)}
	options.DocumentTopic = mqDocumentTopic
	options.DocumentGroup = mqDocumentGroup
	overview := New(options).MonitorOverview(context.Background())

	queues, _ := overview["queues"].(map[string]any)
	if _, present := queues["document_outbox"]; present {
		t.Fatalf("queues.document_outbox still present: %v", queues["document_outbox"])
	}
	jobs, _ := queues["document_jobs"].(map[string]any)
	byStatus, _ := jobs["by_status"].(map[string]int)
	if byStatus[state.DocumentJobSucceeded] != 1 {
		t.Fatalf("document_jobs.by_status = %v", byStatus)
	}
	if byStatus[state.DocumentJobQueued] != 0 || byStatus[state.DocumentJobFailed] != 0 {
		t.Fatalf("document_jobs.by_status = %v, want no queued/failed jobs", byStatus)
	}

	pipeline := pipelineOf(t, overview)
	if pipeline.Outbox == nil || pipeline.Outbox.Total != 1 ||
		pipeline.Outbox.ByStatus["PENDING"] != 1 ||
		pipeline.Outbox.OldestRecordSeconds != 30.0 {
		t.Fatalf("pipeline.outbox = %+v", pipeline.Outbox)
	}
	if pipeline.DocumentJobs.Status != statusOK {
		t.Fatalf("pipeline.document_jobs = %+v, want ok", pipeline.DocumentJobs)
	}
	if !strings.Contains(pipeline.DocumentJobs.Detail, "成功 1") ||
		!strings.Contains(pipeline.DocumentJobs.Detail, "排队 0") {
		t.Fatalf("pipeline.document_jobs.detail = %q", pipeline.DocumentJobs.Detail)
	}
	if strings.Contains(pipeline.DocumentJobs.Detail, "租约过期") ||
		strings.Contains(pipeline.DocumentJobs.Detail, "失败 1") {
		t.Fatalf("pipeline.document_jobs.detail = %q, want no failure wording", pipeline.DocumentJobs.Detail)
	}
	if pipeline.Kafka.Status != statusUnknown || pipeline.Kafka.Detail != "未装配 Kafka 进度读取" ||
		pipeline.Kafka.Topic != mqDocumentTopic || pipeline.Kafka.Group != mqDocumentGroup {
		t.Fatalf("pipeline.kafka = %+v", pipeline.Kafka)
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

// TestMonitorPipelineDebezium covers the capture-connector health states. The
// capture progress is always null: the application cannot read the binlog
// offset.
func TestMonitorPipelineDebezium(t *testing.T) {
	t.Run("未配置", func(t *testing.T) {
		overview := New(pipelineTestOptions(sampleStats())).MonitorOverview(context.Background())
		view := pipelineOf(t, overview)
		if view.Debezium.Status != statusUnknown ||
			view.Debezium.Detail != "未配置 Debezium 健康检查地址，采集进度未知" {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
		if view.Debezium.CaptureProgress != nil {
			t.Fatalf("capture_progress = %v, want null", view.Debezium.CaptureProgress)
		}
	})
	t.Run("健康", func(t *testing.T) {
		server := debeziumServer(t, http.StatusOK, `{"status":"UP"}`)
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = server.URL
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusOK ||
			view.Debezium.Detail != "健康检查正常；采集位点需在 Debezium 侧查看" {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
		if view.Debezium.CaptureProgress != nil {
			t.Fatalf("capture_progress = %v, want null", view.Debezium.CaptureProgress)
		}
	})
	t.Run("HTTP 失败", func(t *testing.T) {
		server := debeziumServer(t, http.StatusInternalServerError, `{"status":"UP"}`)
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = server.URL
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusError ||
			!strings.Contains(view.Debezium.Detail, "HTTP 500") {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
		if view.Debezium.CaptureProgress != nil {
			t.Fatalf("capture_progress = %v, want null", view.Debezium.CaptureProgress)
		}
	})
	t.Run("不可达", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = "http://" + unreachableAddress(t)
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusError ||
			view.Debezium.Detail != "Debezium 健康检查不可达" {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
	})
	t.Run("DOWN", func(t *testing.T) {
		server := debeziumServer(t, http.StatusOK, `{"status":"DOWN"}`)
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = server.URL
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusError ||
			!strings.Contains(view.Debezium.Detail, "DOWN") {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
	})
	t.Run("响应不可解析", func(t *testing.T) {
		server := debeziumServer(t, http.StatusOK, `<html>not json</html>`)
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = server.URL
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusDegraded ||
			view.Debezium.Detail != "健康检查响应无法解析" {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
	})
}

// TestMonitorPipelineKafka covers the consumer-progress states: disabled without
// a broker, unknown when not wired or uncommitted, ok with numbers, and error.
func TestMonitorPipelineKafka(t *testing.T) {
	t.Run("未配置 Kafka", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.DocumentTopic = mqDocumentTopic
		options.DocumentGroup = mqDocumentGroup
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusDisabled ||
			view.Kafka.Detail != "未配置 Kafka：文档通知不会投递" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.Topic != mqDocumentTopic || view.Kafka.Group != mqDocumentGroup {
			t.Fatalf("pipeline.kafka labels = %q/%q", view.Kafka.Topic, view.Kafka.Group)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
	})
	t.Run("未装配进度读取", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusUnknown ||
			view.Kafka.Detail != "未装配 Kafka 进度读取" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
	})
	t.Run("进度正常", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.DocumentTopic = mqDocumentTopic
		options.DocumentGroup = mqDocumentGroup
		options.KafkaProgress = func(context.Context) (KafkaProgress, error) {
			return KafkaProgress{
				CommittedOffset: int64Pointer(12),
				EndOffset:       int64Pointer(20),
				Lag:             int64Pointer(8),
			}, nil
		}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusOK || view.Kafka.Detail != "消费组滞后 8 条" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset == nil || *view.Kafka.CommittedOffset != 12 ||
			view.Kafka.EndOffset == nil || *view.Kafka.EndOffset != 20 ||
			view.Kafka.ConsumerLag == nil || *view.Kafka.ConsumerLag != 8 {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
	})
	t.Run("未提交位点", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaProgress = func(context.Context) (KafkaProgress, error) {
			return KafkaProgress{}, nil
		}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusUnknown || view.Kafka.Detail != "消费组尚未提交位点" {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
	})
	t.Run("读取失败", func(t *testing.T) {
		options := pipelineTestOptions(sampleStats())
		options.KafkaBootstrapServers = []string{kafkaListener(t)}
		options.KafkaProgress = func(context.Context) (KafkaProgress, error) {
			return KafkaProgress{}, errors.New("broker unavailable")
		}
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Kafka.Status != statusError ||
			!strings.Contains(view.Kafka.Detail, "Kafka 消费进度读取失败：") {
			t.Fatalf("pipeline.kafka = %+v", view.Kafka)
		}
		if view.Kafka.CommittedOffset != nil || view.Kafka.EndOffset != nil ||
			view.Kafka.ConsumerLag != nil {
			t.Fatalf("pipeline.kafka = %+v, want null numbers", view.Kafka)
		}
	})
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

	// The live result-snapshot count comes from Redis; without the counter the
	// library value stays null (未知) instead of a wrong MySQL table count.
	library, ok := document["library"].(map[string]any)
	if !ok {
		t.Fatalf("library has type %T", document["library"])
	}
	if library["result_snapshots"] != nil {
		t.Fatalf("library.result_snapshots = %v, want null without the Redis counter",
			library["result_snapshots"])
	}
	pipeline, ok := document["pipeline"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline has type %T", document["pipeline"])
	}
	requireKeys(t, "pipeline", pipeline, "outbox", "document_jobs", "worker", "debezium", "kafka")

	worker, ok := pipeline["worker"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.worker has type %T", pipeline["worker"])
	}
	requireKeys(t, "pipeline.worker", worker, "status", "detail")

	outbox, ok := pipeline["outbox"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.outbox has type %T", pipeline["outbox"])
	}
	requireKeys(t, "pipeline.outbox", outbox, "by_status", "total", "oldest_record_seconds")
	if outbox["total"] != 5.0 || outbox["oldest_record_seconds"] != 12.0 {
		t.Fatalf("pipeline.outbox = %v", outbox)
	}

	jobs, ok := pipeline["document_jobs"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.document_jobs has type %T", pipeline["document_jobs"])
	}
	requireKeys(t, "pipeline.document_jobs", jobs, "status", "detail")

	debezium, ok := pipeline["debezium"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.debezium has type %T", pipeline["debezium"])
	}
	requireKeys(t, "pipeline.debezium", debezium, "status", "detail", "capture_progress")
	if debezium["capture_progress"] != nil {
		t.Fatalf("capture_progress = %v, want null", debezium["capture_progress"])
	}

	kafka, ok := pipeline["kafka"].(map[string]any)
	if !ok {
		t.Fatalf("pipeline.kafka has type %T", pipeline["kafka"])
	}
	requireKeys(t, "pipeline.kafka", kafka, "status", "detail", "topic", "group",
		"committed_offset", "end_offset", "consumer_lag")
	if kafka["committed_offset"] != nil || kafka["end_offset"] != nil || kafka["consumer_lag"] != nil {
		t.Fatalf("pipeline.kafka = %v, want null numbers", kafka)
	}

	// A failed statistics read nulls pipeline.outbox while the always-present
	// pipeline object keeps the capture/consumption rows.
	failed := decodeOverview(t, New(Options{
		Stats:     &fakeStatsProvider{err: errors.New("state aggregation failed")},
		MySQL:     fakeMySQLPinger{},
		PDFEngine: "go",
	}).MonitorOverview(context.Background()))
	failedPipeline, _ := failed["pipeline"].(map[string]any)
	if failedPipeline == nil {
		t.Fatalf("pipeline = %v, want present", failed["pipeline"])
	}
	if failedPipeline["outbox"] != nil {
		t.Fatalf("pipeline.outbox = %v, want null", failedPipeline["outbox"])
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
	qdrant := qdrantServer(t, "knowledge", 5, false)
	return Options{
		Stats:      &fakeStatsProvider{stats: sampleStats()},
		MySQL:      fakeMySQLPinger{},
		Redis:      fakeRedisProbe{enabled: false},
		QdrantURL:  qdrant.URL,
		Collection: "knowledge",
		PDFEngine:  "go",
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
		options.QdrantURL = qdrantServer(t, "knowledge", 0, true).URL
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
	t.Run("Debezium 异常", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.DebeziumHealthURL = debeziumServer(t, http.StatusOK, `{"status":"DOWN"}`).URL
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
		options.KafkaProgress = func(context.Context) (KafkaProgress, error) {
			return KafkaProgress{}, errors.New("offset fetch denied")
		}
		overview := New(options).MonitorOverview(context.Background())
		if overview["status"] != statusDegraded {
			t.Fatalf("status = %v, want degraded", overview["status"])
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

// TestMonitorLibraryResultSnapshots asserts the snapshot count comes from the
// Redis counter and stays null when that side read fails.
func TestMonitorLibraryResultSnapshots(t *testing.T) {
	t.Run("有计数", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.ResultSnapshots = func(context.Context) (int, error) { return 7, nil }
		library, _ := New(options).MonitorOverview(context.Background())["library"].(map[string]any)
		if library["result_snapshots"] != 7 {
			t.Fatalf("library.result_snapshots = %v", library["result_snapshots"])
		}
	})
	t.Run("计数不可用", func(t *testing.T) {
		options := healthyDependencyOptions(t)
		options.ResultSnapshots = func(context.Context) (int, error) {
			return 0, errors.New("redis unavailable")
		}
		library, _ := New(options).MonitorOverview(context.Background())["library"].(map[string]any)
		if library["result_snapshots"] != nil {
			t.Fatalf("library.result_snapshots = %v, want null", library["result_snapshots"])
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

// TestMonitorDebeziumRuntimeChecks asserts the health probe reports the
// per-check runtime state instead of trusting the aggregate HTTP code.
func TestMonitorDebeziumRuntimeChecks(t *testing.T) {
	t.Run("运行检查通过", func(t *testing.T) {
		server := jsonServer(t, "/q/health", http.StatusOK,
			`{"status":"UP","checks":[{"name":"debezium","status":"UP"},{"name":"Database connections health check","status":"UP"}]}`)
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = server.URL + "/q/health"
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusOK ||
			!strings.Contains(view.Debezium.Detail, "2 项运行检查通过") {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
	})
	t.Run("运行检查失败", func(t *testing.T) {
		server := jsonServer(t, "/q/health", http.StatusServiceUnavailable,
			`{"status":"DOWN","checks":[{"name":"debezium","status":"DOWN"},{"name":"Database connections health check","status":"UP"}]}`)
		options := pipelineTestOptions(sampleStats())
		options.DebeziumHealthURL = server.URL + "/q/health"
		view := pipelineOf(t, New(options).MonitorOverview(context.Background()))
		if view.Debezium.Status != statusError ||
			!strings.Contains(view.Debezium.Detail, "检查失败：debezium") {
			t.Fatalf("pipeline.debezium = %+v", view.Debezium)
		}
	})
}
