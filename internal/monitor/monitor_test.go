package monitor

import (
	"context"
	"database/sql"
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
		DocumentJobsByStage:        map[string]int{"parse": 1},
		OldestQueuedSeconds:        42.5,
		StuckProcessing:            1,
		FailedRecent:               1,
		JobsSucceededRecent:        3,
		OutboxByStatus:             map[string]int{"PENDING": 2},
		OldestPendingOutboxSeconds: 12,
		SearchByStatus: map[string]int{
			state.SearchTaskQueued: 1, state.SearchTaskProcessing: 2,
			state.SearchTaskSucceeded: 3, state.SearchTaskFailed: 0,
			state.SearchTaskTimedOut: 1,
		},
		OldestActiveSearchSeconds: 5,
		SearchesRecent:            7,
		UnfinishedUploads:         2,
		Documents:                 10,
		KnowledgeBases:            2,
		ResultSnapshots:           4,
		ArtifactSnapshots:         6,
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

// pdfServer serves the Docling health endpoint.
func pdfServer(t *testing.T, status int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.WriteHeader(status)
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
		OutboxBridge:          "debezium",
		UploadMode:            "async",
		DocumentWorker:        "in-process",
		PDFEngine:             pdfEngineDocling,
		PDFServiceURL:         pdf.URL,
		StartedAt:             time.Now().Add(-90 * time.Second),
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

	service, ok := overview["service"].(map[string]any)
	if !ok {
		t.Fatalf("service has type %T", overview["service"])
	}
	if service["name"] != version.ServiceName || service["version"] != version.Version ||
		service["backend"] != "go" || service["collection"] != "knowledge" {
		t.Fatalf("service = %v", service)
	}
	if service["upload_mode"] != "async" || service["outbox_bridge"] != "debezium" ||
		service["document_worker"] != "in-process" || service["pdf_engine"] != pdfEngineDocling {
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
	outbox, _ := queues["document_outbox"].(map[string]any)
	if outbox == nil || outbox["oldest_pending_seconds"] != 12.0 {
		t.Fatalf("document_outbox = %v", queues["document_outbox"])
	}
	searches, _ := queues["search_tasks"].(map[string]any)
	if searches == nil || searches["oldest_active_seconds"] != 5.0 {
		t.Fatalf("search_tasks = %v", queues["search_tasks"])
	}
	uploads, _ := queues["uploads"].(map[string]any)
	if uploads == nil || uploads["unfinished"] != 2 {
		t.Fatalf("uploads = %v", queues["uploads"])
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
		Stats:          &fakeStatsProvider{stats: sampleStats()},
		MySQL:          fakeMySQLPinger{},
		QdrantURL:      qdrant.URL,
		Collection:     "knowledge",
		PDFEngine:      "go",
		UploadMode:     "sync",
		OutboxBridge:   "debezium",
		DocumentWorker: "external",
	}
	overview := New(options).MonitorOverview(context.Background())

	if overview["ok"] != true {
		t.Fatalf("ok = %v (degraded/disabled must not fail the snapshot)", overview["ok"])
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
}

// TestMonitorOverviewErrors asserts a failed probe marks ok=false while the
// statistics sections stay populated.
func TestMonitorOverviewErrors(t *testing.T) {
	unreachable := unreachableAddress(t)
	options := Options{
		Stats:          &fakeStatsProvider{stats: sampleStats()},
		MySQL:          fakeMySQLPinger{err: errors.New("db down")},
		Redis:          fakeRedisProbe{enabled: true, err: errors.New("redis down")},
		QdrantURL:      "http://" + unreachable,
		Collection:     "knowledge",
		PDFEngine:      pdfEngineDocling,
		PDFServiceURL:  "http://" + unreachable,
		UploadMode:     "async",
		OutboxBridge:   "debezium",
		DocumentWorker: "in-process",
	}
	overview := New(options).MonitorOverview(context.Background())

	if overview["ok"] != false {
		t.Fatalf("ok = %v", overview["ok"])
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

	if overview["ok"] != false {
		t.Fatalf("ok = %v", overview["ok"])
	}
	if overview["queues"] != nil || overview["library"] != nil || overview["activity"] != nil {
		t.Fatalf("sections = %v/%v/%v, want nil",
			overview["queues"], overview["library"], overview["activity"])
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
