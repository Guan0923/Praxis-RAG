// Package monitor assembles the read-only snapshot behind the Console
// monitoring page: service metadata, bounded dependency probes, queue depth and
// library/activity counters. It never writes state and never exposes
// credentials, queries, document content or broker payloads.
package monitor

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/qdrant"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
)

const (
	// probeTimeout bounds every dependency probe independently so one slow
	// dependency cannot delay the whole snapshot.
	probeTimeout = 1500 * time.Millisecond
	// recentWindow is the activity window reported by the Console page.
	recentWindow = 24 * time.Hour
	// pdfEngineVision mirrors config.PDFEngineVision without importing the
	// configuration package for one literal.
	pdfEngineVision = "vision"
)

// Dependency status values shared with the Console contract.
const (
	statusOK       = "ok"
	statusDegraded = "degraded"
	statusError    = "error"
	statusDisabled = "disabled"
	statusUnknown  = "unknown"
)

// StatsProvider is the read-only state aggregate surface. *state.Stores
// implements it.
type StatsProvider interface {
	MonitorStats(ctx context.Context, now time.Time, recentWindow time.Duration) (*state.MonitorStats, error)
}

// MySQLPinger is the lightweight database connectivity probe. *state.Stores
// implements it.
type MySQLPinger interface {
	Ping(ctx context.Context) error
}

// RedisProbe is the cache connectivity probe. *redisstate.Client implements it;
// this package deliberately does not import the Redis package.
type RedisProbe interface {
	Enabled() bool
	Ping(ctx context.Context) error
}

// Options is the static wiring captured once at service startup.
type Options struct {
	Stats StatsProvider
	MySQL MySQLPinger
	// DBStats reports the database connection-pool counters (store.DB.Stats).
	DBStats func() sql.DBStats
	Redis   RedisProbe

	QdrantURL  string
	Collection string

	KafkaBootstrapServers []string

	// DebeziumHealthURL is the optional Debezium/Connect health endpoint. It
	// reports whether the capture connector is running, never the binlog offset:
	// the service cannot observe that from its own state database.
	DebeziumHealthURL string
	// DocumentTopic/DocumentGroup label the document delivery pipeline. They are
	// reported even when the progress read is unavailable.
	DocumentTopic string
	DocumentGroup string
	// KafkaProgress reads the document consumer group offsets. nil means the
	// Kafka delivery progress is not wired in this deployment.
	KafkaProgress func(ctx context.Context) (KafkaProgress, error)
	// KafkaProbe reads the broker metadata for the document topic. It replaces
	// the legacy TCP dial: a metadata round trip proves reachability, TLS/SASL
	// authentication and topic existence. nil keeps the TCP-only fallback for
	// broker-less embeddings and tests.
	KafkaProbe func(ctx context.Context) (KafkaStatus, error)
	// ResultSnapshots counts the live search-result snapshots kept in Redis.
	// The snapshots live in the cache, not MySQL, so the state aggregate cannot
	// count them. nil or an error keeps the number unknown (JSON null).
	ResultSnapshots func(ctx context.Context) (int, error)
	// WorkerHeartbeat reads the newest document-worker liveness heartbeat from
	// the state database. nil means the heartbeat store is not wired, so the
	// worker row stays unknown instead of claiming the worker runs.
	WorkerHeartbeat func(ctx context.Context) (*WorkerHeartbeat, error)

	OutboxBridge  string
	UploadMode    string
	PDFEngine     string
	PDFServiceURL string

	StartedAt time.Time
	// HTTPClient is shared by the Qdrant and PDF probes; nil uses a client
	// bounded by probeTimeout.
	HTTPClient *http.Client
}

// Service renders monitoring snapshots. It is safe for concurrent use.
type Service struct {
	options    Options
	httpClient *http.Client
}

// New builds the monitoring service from the resolved runtime options.
func New(options Options) *Service {
	httpClient := options.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: probeTimeout}
	}
	return &Service{options: options, httpClient: httpClient}
}

// dependency is one row of the dependencies array. The JSON field names are
// part of the Console contract and must not change.
type dependency struct {
	Name      string  `json:"name"`
	Label     string  `json:"label"`
	Status    string  `json:"status"`
	LatencyMS float64 `json:"latency_ms"`
	Detail    string  `json:"detail"`
	Points    *int64  `json:"points,omitempty"`
}

// dependencyLabels are the display labels of the fixed dependency rows.
var dependencyLabels = []struct {
	name  string
	label string
}{
	{"mysql", "MySQL"},
	{"redis", "Redis"},
	{"qdrant", "Qdrant"},
	{"kafka", "Kafka"},
	{"pdf-service", "PDF 解析服务"},
}

// probeResult is the outcome of one bounded dependency probe.
type probeResult struct {
	status    string
	detail    string
	points    *int64
	latencyMS float64
}

// KafkaProgress is one observation of the document consumer group offsets.
// A nil pointer means the value is unknown (the group has not committed yet or
// the broker refused the read), never zero.
type KafkaProgress struct {
	CommittedOffset *int64
	EndOffset       *int64
	Lag             *int64
}

// KafkaStatus is one bounded observation of the broker cluster metadata.
// Brokers is the number of live brokers the metadata response reports; a
// metadata round trip also proves TLS/SASL authentication and topic existence,
// which a raw TCP dial cannot.
type KafkaStatus struct {
	Brokers     int
	Topic       string
	TopicExists bool
	Partitions  int
}

// WorkerHeartbeat is the newest recorded document-worker liveness heartbeat.
// Instances counts the worker instances that recorded one inside the freshness
// window, so a restarted worker is visible even when an old row remains.
type WorkerHeartbeat struct {
	UpdatedAt time.Time
	Instances int
}

// pipelineSnapshot is the delivery-pipeline view of the monitoring snapshot. It
// separates four questions the old queues.document_outbox row conflated:
//
//   - outbox: how many delivery records are retained and how old the oldest is
//     (record statistics, not undelivered backlog),
//   - document_jobs: the processing state of admitted documents,
//   - debezium: whether the capture connector is healthy (the binlog offset is
//     only visible inside Debezium),
//   - kafka: how far the document worker's consumer group has advanced.
//
// Null numbers are unknown: the Console renders them as 未知/未启用.
type pipelineSnapshot struct {
	Outbox       *pipelineOutbox  `json:"outbox"`
	DocumentJobs pipelineJobs     `json:"document_jobs"`
	Worker       pipelineWorker   `json:"worker"`
	Debezium     pipelineDebezium `json:"debezium"`
	Kafka        pipelineKafka    `json:"kafka"`
}

// pipelineOutbox reports the retained document_job_outbox records. It is nil
// when the state aggregate is unavailable.
type pipelineOutbox struct {
	ByStatus            map[string]int `json:"by_status"`
	Total               int            `json:"total"`
	OldestRecordSeconds float64        `json:"oldest_record_seconds"`
}

// pipelineJobs is the document job processing state (ok/degraded/unknown).
type pipelineJobs struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// pipelineWorker reports whether a document worker is actually running, read
// from its periodic heartbeat row.
type pipelineWorker struct {
	Status string `json:"status"`
	Detail string `json:"detail"`
}

// pipelineDebezium reports the capture-connector health. CaptureProgress is
// always nil: the application side cannot read the binlog offset and must not
// invent one.
type pipelineDebezium struct {
	Status          string `json:"status"`
	Detail          string `json:"detail"`
	CaptureProgress *int64 `json:"capture_progress"`
}

// pipelineKafka reports the document consumer group progress. Topic/Group are
// always present so the Console can label the row even when the offsets are
// unknown.
type pipelineKafka struct {
	Status          string `json:"status"`
	Detail          string `json:"detail"`
	Topic           string `json:"topic"`
	Group           string `json:"group"`
	CommittedOffset *int64 `json:"committed_offset"`
	EndOffset       *int64 `json:"end_offset"`
	ConsumerLag     *int64 `json:"consumer_lag"`
}

// MonitorOverview renders the full monitoring snapshot. The response is always
// a complete document: a failed statistics query degrades queues/library/activity
// to null and reports the error instead of failing the request.
func (s *Service) MonitorOverview(ctx context.Context) map[string]any {
	now := time.Now()
	probes := []func(context.Context) probeResult{
		s.probeMySQL,
		s.probeRedis,
		s.probeQdrant,
		s.probeKafka,
		s.probePDF,
	}
	results := make([]probeResult, len(probes))
	var wait sync.WaitGroup
	for index, probe := range probes {
		wait.Add(1)
		go func(index int, probe func(context.Context) probeResult) {
			defer wait.Done()
			results[index] = s.runProbe(ctx, probe)
		}(index, probe)
	}

	var stats *state.MonitorStats
	var statsErr error
	wait.Add(1)
	go func() {
		defer wait.Done()
		if s.options.Stats == nil {
			statsErr = errors.New("监控统计未装配")
			return
		}
		stats, statsErr = s.options.Stats.MonitorStats(ctx, now, recentWindow)
		if statsErr == nil && stats == nil {
			statsErr = errors.New("监控统计返回空结果")
		}
	}()

	// The delivery-progress probes run next to the statistics read: neither the
	// snapshot nor statsErr may wait on a slow capture connector or broker.
	var debezium probeResult
	var kafka pipelineKafka
	wait.Add(1)
	go func() {
		defer wait.Done()
		debezium = s.runProbe(ctx, s.probeDebezium)
	}()
	wait.Add(1)
	go func() {
		defer wait.Done()
		probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		kafka = s.probeKafkaProgress(probeContext)
	}()

	// The live snapshot count lives in Redis and the worker liveness in its
	// heartbeat rows; both are optional side reads whose failure only makes
	// their own value unknown.
	var snapshotCount *int
	wait.Add(1)
	go func() {
		defer wait.Done()
		if s.options.ResultSnapshots == nil {
			return
		}
		probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		count, countErr := s.options.ResultSnapshots(probeContext)
		if countErr != nil {
			return
		}
		snapshotCount = &count
	}()
	var workerBeat *WorkerHeartbeat
	var workerErr error
	wait.Add(1)
	go func() {
		defer wait.Done()
		if s.options.WorkerHeartbeat == nil {
			workerErr = errors.New("Worker 心跳未装配")
			return
		}
		probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
		defer cancel()
		workerBeat, workerErr = s.options.WorkerHeartbeat(probeContext)
	}()
	wait.Wait()

	dependencies := make([]dependency, 0, len(probes))
	for index, item := range dependencyLabels {
		result := results[index]
		dependencies = append(dependencies, dependency{
			Name:      item.name,
			Label:     item.label,
			Status:    result.status,
			LatencyMS: roundOneDecimal(result.latencyMS),
			Detail:    result.detail,
			Points:    result.points,
		})
	}

	pipeline := pipelineSnapshot{
		Outbox:       pipelineOutboxView(stats, statsErr),
		DocumentJobs: pipelineDocumentJobsView(stats, statsErr),
		Worker:       pipelineWorkerView(workerBeat, workerErr, now),
		Debezium: pipelineDebezium{
			Status: debezium.status,
			Detail: debezium.detail,
			// capture_progress stays nil: only Debezium can read the binlog offset.
		},
		Kafka: kafka,
	}
	// ok means "every observed dependency is healthy". A degraded dependency
	// (for example a missing Qdrant collection) must never be rendered as
	// 运行正常, so the response carries an explicit three-state status and the
	// boolean stay aligned with it.
	status := overallStatus(statsErr, dependencies, pipeline)

	errorsList := make([]string, 0)
	var queues, library, activity any
	if statsErr == nil {
		queues = map[string]any{
			"document_jobs": map[string]any{
				"by_status":             stats.DocumentJobsByStatus,
				"by_stage":              monitorStringIntMap(stats.DocumentJobsByStage),
				"oldest_queued_seconds": roundOneDecimal(stats.OldestQueuedSeconds),
				"stuck_processing":      stats.StuckProcessing,
				"failed_recent":         stats.FailedRecent,
			},
			"search_tasks": map[string]any{
				"by_status":             stats.SearchByStatus,
				"oldest_active_seconds": roundOneDecimal(stats.OldestActiveSearchSeconds),
			},
			"uploads": monitorUploadsView(stats),
		}
		library = monitorLibraryView(stats, snapshotCount)
		activity = map[string]any{
			"window_hours":   recentWindow.Hours(),
			"jobs_succeeded": stats.JobsSucceededRecent,
			"jobs_failed":    stats.FailedRecent,
			"searches":       stats.SearchesRecent,
		}
	} else {
		errorsList = append(errorsList, "监控统计不可用："+shortError(statsErr))
	}

	startedAt := s.options.StartedAt
	if startedAt.IsZero() {
		startedAt = now
	}
	uptime := now.Sub(startedAt).Seconds()
	if uptime < 0 {
		uptime = 0
	}
	return map[string]any{
		"generated_at": now.UTC().Format(time.RFC3339),
		"status":       status,
		"ok":           status == statusOK,
		"service": map[string]any{
			"name":           version.ServiceName,
			"version":        version.Version,
			"backend":        "go",
			"collection":     s.options.Collection,
			"started_at":     startedAt.UTC().Format(time.RFC3339),
			"uptime_seconds": roundOneDecimal(uptime),
			"upload_mode":    s.options.UploadMode,
			"outbox_bridge":  s.options.OutboxBridge,
			"pdf_engine":     s.options.PDFEngine,
		},
		"dependencies": dependencies,
		"queues":       queues,
		"pipeline":     pipeline,
		"library":      library,
		"activity":     activity,
		"errors":       errorsList,
	}
}

// monitorUploadsView renders the upload operations. The old single
// "未完成上传" number mixed three very different states, so the Console gets the
// status buckets and the disjoint group counters instead of one misleading
// total: in_progress (INTENT/RUNNING), failed (FAILED/ROLLBACK_FAILED),
// rolling_back (ROLLING_BACK) and cleanup_pending (committed operations whose
// old-version cleanup is still pending). "unfinished" is kept as the union
// count for compatibility; it is not "files currently uploading".
func monitorUploadsView(stats *state.MonitorStats) map[string]any {
	return map[string]any{
		"unfinished":      stats.UnfinishedUploads,
		"by_status":       monitorStringIntMap(stats.UploadsByStatus),
		"in_progress":     stats.UploadsInProgress,
		"failed":          stats.UploadsFailed,
		"rolling_back":    stats.UploadsRollingBack,
		"cleanup_pending": stats.UploadsCleanupPending,
	}
}

// monitorLibraryView renders the library counters. result_snapshots comes from
// the Redis transient store, where the immutable snapshots actually live; the
// legacy MySQL search_result_snapshots table is not the source of truth. A nil
// count stays JSON null so the Console prints 未知 instead of a wrong zero.
func monitorLibraryView(stats *state.MonitorStats, resultSnapshots *int) map[string]any {
	var snapshots any
	if resultSnapshots != nil {
		snapshots = *resultSnapshots
	}
	return map[string]any{
		"documents":          stats.Documents,
		"knowledge_bases":    stats.KnowledgeBases,
		"result_snapshots":   snapshots,
		"artifact_snapshots": stats.ArtifactSnapshots,
	}
}

// overallStatus ranks the observed health into the three-state contract the
// Console header renders. Only infra-level views participate: document job
// counters already have their own degraded view, and a single failed document
// must not turn the whole service status red.
func overallStatus(statsErr error, dependencies []dependency, pipeline pipelineSnapshot) string {
	if statsErr != nil {
		return statusError
	}
	status := statusOK
	raise := func(candidate string, errorIsFatal bool) {
		switch candidate {
		case statusError:
			if errorIsFatal {
				status = statusError
			} else if status == statusOK {
				status = statusDegraded
			}
		case statusDegraded:
			if status == statusOK {
				status = statusDegraded
			}
		}
	}
	for _, item := range dependencies {
		raise(item.Status, true)
	}
	// Debezium delivers the document outbox; a dead capture connector breaks the
	// delivery path, so its error is fatal to the overall status.
	raise(pipeline.Debezium.Status, true)
	// A missing/stopped worker is a capacity problem: jobs stay queued.
	raise(pipeline.Worker.Status, false)
	// An unreadable consumer-offset snapshot degrades observability only; the
	// broker probe above already reports an unreachable cluster as an error.
	raise(pipeline.Kafka.Status, false)
	return status
}

// workerHeartbeatStaleAfter is the heartbeat age at which the worker row
// degrades. Workers beat every 15 seconds, so 60 seconds means three missed
// beats and is strong evidence the process stopped.
const workerHeartbeatStaleAfter = 60 * time.Second

// pipelineWorkerView renders the real worker liveness probe. The deployment
// always runs rag-document-worker as an independent process, so only a fresh
// heartbeat row proves it is actually running.
func pipelineWorkerView(beat *WorkerHeartbeat, beatErr error, now time.Time) pipelineWorker {
	if beatErr != nil {
		return pipelineWorker{Status: statusUnknown, Detail: "Worker 心跳读取失败"}
	}
	if beat == nil || beat.UpdatedAt.IsZero() {
		return pipelineWorker{Status: statusUnknown, Detail: "未收到文档 worker 心跳（确认 rag-document-worker 已启动）"}
	}
	age := now.Sub(beat.UpdatedAt)
	if age < 0 {
		age = 0
	}
	instances := beat.Instances
	if instances < 1 {
		instances = 1
	}
	if age <= workerHeartbeatStaleAfter {
		return pipelineWorker{
			Status: statusOK,
			Detail: fmt.Sprintf("最近心跳 %.1f 秒前，%d 个实例运行中", age.Seconds(), instances),
		}
	}
	return pipelineWorker{
		Status: statusDegraded,
		Detail: fmt.Sprintf("最近心跳 %.1f 秒前，worker 可能已停止", age.Seconds()),
	}
}

// pipelineOutboxView renders the retained outbox record statistics, or nil when
// the state aggregate is unavailable (the Console prints 未知).
func pipelineOutboxView(stats *state.MonitorStats, statsErr error) *pipelineOutbox {
	if statsErr != nil || stats == nil {
		return nil
	}
	byStatus := monitorStringIntMap(stats.OutboxByStatus)
	total := 0
	for _, count := range byStatus {
		total += count
	}
	return &pipelineOutbox{
		ByStatus:            byStatus,
		Total:               total,
		OldestRecordSeconds: roundOneDecimal(stats.OldestOutboxRecordSeconds),
	}
}

// oldestQueuedDegradedSeconds is the queue age at which the document job view
// degrades. An old queued job means no worker is draining the queue.
const oldestQueuedDegradedSeconds = 600.0

// pipelineDocumentJobsView summarizes the document job processing state. It only
// reads document_jobs: a succeeded job whose outbox row is still PENDING is a
// normal Debezium record and must never make this view degraded.
func pipelineDocumentJobsView(stats *state.MonitorStats, statsErr error) pipelineJobs {
	if statsErr != nil || stats == nil {
		return pipelineJobs{Status: statusUnknown, Detail: "状态库不可用，任务状态未知"}
	}
	byStatus := stats.DocumentJobsByStatus
	queued := byStatus[state.DocumentJobQueued]
	processing := byStatus[state.DocumentJobProcessing]
	succeeded := byStatus[state.DocumentJobSucceeded]
	failed := byStatus[state.DocumentJobFailed]
	detail := fmt.Sprintf("排队 %d、处理中 %d、成功 %d、失败 %d", queued, processing, succeeded, failed)
	if stats.OldestQueuedSeconds > 0 {
		detail += fmt.Sprintf("；最老排队 %.1f 秒", roundOneDecimal(stats.OldestQueuedSeconds))
	}
	reasons := make([]string, 0, 3)
	if stats.StuckProcessing > 0 {
		reasons = append(reasons, fmt.Sprintf("租约过期 %d 个", stats.StuckProcessing))
	}
	if stats.FailedRecent > 0 {
		reasons = append(reasons, fmt.Sprintf("近 24 小时失败 %d 个", stats.FailedRecent))
	}
	if stats.OldestQueuedSeconds > oldestQueuedDegradedSeconds {
		reasons = append(reasons, "最老排队超过 600 秒")
	}
	if len(reasons) > 0 {
		detail += "；" + strings.Join(reasons, "、")
		return pipelineJobs{Status: statusDegraded, Detail: detail}
	}
	return pipelineJobs{Status: statusOK, Detail: detail}
}

// debeziumHealth is the subset of the Debezium health payload the probe reads.
// Debezium Server exposes Quarkus SmallRye Health: the aggregate status plus the
// individual runtime checks. A failed connector task or database connection
// turns its check DOWN, so reading the checks is an actual running check instead
// of an HTTP status-only probe.
type debeziumHealth struct {
	Status string `json:"status"`
	Checks []struct {
		Name   string `json:"name"`
		Status string `json:"status"`
	} `json:"checks"`
}

// debeziumDownDetail names the failing runtime checks so the Console shows what
// is actually broken instead of repeating the aggregate status.
func debeziumDownDetail(health debeziumHealth) string {
	failing := make([]string, 0, len(health.Checks))
	for _, check := range health.Checks {
		if !strings.EqualFold(strings.TrimSpace(check.Status), "UP") {
			name := strings.TrimSpace(check.Name)
			if name == "" {
				name = "未命名检查"
			}
			failing = append(failing, boundedText(name, 40))
		}
	}
	if len(failing) == 0 {
		return "Debezium 状态为 DOWN"
	}
	return "Debezium 状态为 DOWN（检查失败：" + strings.Join(failing, "、") + "）"
}

// probeDebezium checks the optional capture-connector health endpoint and its
// per-check runtime state. It never reports a capture offset: only Debezium
// knows the binlog position.
func (s *Service) probeDebezium(ctx context.Context) probeResult {
	healthURL := strings.TrimSpace(s.options.DebeziumHealthURL)
	if healthURL == "" {
		return probeResult{status: statusUnknown, detail: "未配置 Debezium 健康检查地址，采集进度未知"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, healthURL, nil)
	if err != nil {
		return probeResult{status: statusError, detail: "Debezium 健康检查地址无效"}
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return probeResult{status: statusError, detail: "Debezium 健康检查不可达"}
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	var health debeziumHealth
	parsed := readErr == nil && json.Unmarshal(body, &health) == nil
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		// Quarkus answers 503 when a check is DOWN; keep the failing check names
		// when the body is still a health document.
		if parsed && strings.EqualFold(strings.TrimSpace(health.Status), "DOWN") {
			return probeResult{status: statusError, detail: debeziumDownDetail(health)}
		}
		return probeResult{status: statusError, detail: fmt.Sprintf("健康检查返回 HTTP %d", response.StatusCode)}
	}
	if readErr != nil {
		return probeResult{status: statusDegraded, detail: "健康检查响应读取失败"}
	}
	if !parsed {
		return probeResult{status: statusDegraded, detail: "健康检查响应无法解析"}
	}
	switch strings.ToUpper(strings.TrimSpace(health.Status)) {
	case "UP":
		detail := "健康检查正常"
		if len(health.Checks) > 0 {
			detail = fmt.Sprintf("健康检查正常（%d 项运行检查通过）", len(health.Checks))
		}
		return probeResult{status: statusOK, detail: detail + "；采集位点需在 Debezium 侧查看"}
	case "DOWN":
		return probeResult{status: statusError, detail: debeziumDownDetail(health)}
	case "":
		return probeResult{status: statusDegraded, detail: "健康检查未返回状态"}
	default:
		return probeResult{status: statusDegraded, detail: "Debezium 状态为 " + boundedText(health.Status, 40)}
	}
}

// probeKafkaProgress reads the document consumer group offsets through the
// injected progress reader. Unknown values stay nil so the Console prints 未知.
func (s *Service) probeKafkaProgress(ctx context.Context) pipelineKafka {
	view := pipelineKafka{
		Topic: strings.TrimSpace(s.options.DocumentTopic),
		Group: strings.TrimSpace(s.options.DocumentGroup),
	}
	if len(kafkaBrokerList(s.options.KafkaBootstrapServers)) == 0 {
		view.Status = statusDisabled
		view.Detail = "未配置 Kafka：文档通知不会投递"
		return view
	}
	if s.options.KafkaProgress == nil {
		view.Status = statusUnknown
		view.Detail = "未装配 Kafka 进度读取"
		return view
	}
	progress, err := s.options.KafkaProgress(ctx)
	if err != nil {
		view.Status = statusError
		view.Detail = "Kafka 消费进度读取失败：" + shortError(err)
		return view
	}
	if progress.CommittedOffset == nil || progress.EndOffset == nil {
		view.Status = statusUnknown
		view.Detail = "消费组尚未提交位点"
		return view
	}
	lag := *progress.EndOffset - *progress.CommittedOffset
	if progress.Lag != nil {
		lag = *progress.Lag
	}
	if lag < 0 {
		lag = 0
	}
	view.CommittedOffset = progress.CommittedOffset
	view.EndOffset = progress.EndOffset
	view.ConsumerLag = &lag
	view.Status = statusOK
	view.Detail = fmt.Sprintf("消费组滞后 %d 条", lag)
	return view
}

// boundedText trims a remote-provided label and bounds it for one Chinese
// detail line.
func boundedText(text string, limit int) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "未知"
	}
	runes := []rune(text)
	if limit > 0 && len(runes) > limit {
		return string(runes[:limit]) + "…"
	}
	return text
}

// kafkaBrokerList trims the configured bootstrap servers.
func kafkaBrokerList(servers []string) []string {
	brokers := make([]string, 0, len(servers))
	for _, broker := range servers {
		if trimmed := strings.TrimSpace(broker); trimmed != "" {
			brokers = append(brokers, trimmed)
		}
	}
	return brokers
}

// runProbe bounds one probe with probeTimeout and measures its latency.
func (s *Service) runProbe(ctx context.Context, probe func(context.Context) probeResult) probeResult {
	started := time.Now()
	probeContext, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	result := probe(probeContext)
	result.latencyMS = float64(time.Since(started).Nanoseconds()) / float64(time.Millisecond)
	return result
}

// probeMySQL checks the database connectivity and reports the pool counters.
func (s *Service) probeMySQL(ctx context.Context) probeResult {
	if s.options.MySQL == nil {
		return probeResult{status: statusUnknown, detail: "未装配数据库探测"}
	}
	if err := s.options.MySQL.Ping(ctx); err != nil {
		return probeResult{status: statusError, detail: "数据库连接失败"}
	}
	detail := "连接正常"
	if s.options.DBStats != nil {
		stats := s.options.DBStats()
		detail = fmt.Sprintf("连接 %d，使用 %d，空闲 %d", stats.OpenConnections, stats.InUse, stats.Idle)
		if stats.MaxOpenConnections > 0 {
			detail += fmt.Sprintf("，上限 %d", stats.MaxOpenConnections)
		}
	}
	return probeResult{status: statusOK, detail: detail}
}

// probeRedis reports the optional cache state.
func (s *Service) probeRedis(ctx context.Context) probeResult {
	if s.options.Redis == nil || !s.options.Redis.Enabled() {
		return probeResult{status: statusDisabled, detail: "未启用（可选）"}
	}
	if err := s.options.Redis.Ping(ctx); err != nil {
		return probeResult{status: statusError, detail: "Redis 连接失败"}
	}
	return probeResult{status: statusOK, detail: "已启用"}
}

// probeQdrant lists the collections and reads the configured collection's point
// count. A missing collection is a degraded state, not a connectivity error.
func (s *Service) probeQdrant(ctx context.Context) probeResult {
	client := qdrant.NewClient(s.options.QdrantURL, "", s.httpClient)
	var collections struct {
		Collections []struct {
			Name string `json:"name"`
		} `json:"collections"`
	}
	if err := client.Do(ctx, http.MethodGet, "/collections", nil, nil, &collections); err != nil {
		return probeResult{status: statusError, detail: "Qdrant 连接失败"}
	}
	total := len(collections.Collections)
	collection := strings.TrimSpace(s.options.Collection)
	if collection == "" {
		return probeResult{status: statusOK, detail: fmt.Sprintf("集合 %d 个；未指定集合", total)}
	}
	var info struct {
		PointsCount *int64 `json:"points_count"`
	}
	err := client.Do(ctx, http.MethodGet, "/collections/"+url.PathEscape(collection), nil, nil, &info)
	if err != nil {
		var apiError *qdrant.APIError
		if errors.As(err, &apiError) && apiError.StatusCode == http.StatusNotFound {
			return probeResult{status: statusDegraded, detail: "集合 " + collection + " 不存在"}
		}
		return probeResult{status: statusError, detail: "Qdrant 集合信息读取失败"}
	}
	if info.PointsCount == nil {
		return probeResult{status: statusOK, detail: fmt.Sprintf("集合 %d 个；%s 已就绪", total, collection)}
	}
	points := *info.PointsCount
	return probeResult{
		status: statusOK,
		detail: fmt.Sprintf("集合 %d 个；%s 共 %d 个点", total, collection, points),
		points: &points,
	}
}

// probeKafka verifies the broker with a metadata round trip: unlike a raw TCP
// dial it also proves TLS/SASL authentication and that the document topic
// exists. The TCP fallback is kept only for embeddings that do not wire the
// metadata probe and is labelled as such.
func (s *Service) probeKafka(ctx context.Context) probeResult {
	brokers := kafkaBrokerList(s.options.KafkaBootstrapServers)
	if len(brokers) == 0 {
		return probeResult{status: statusDisabled, detail: "未配置 Kafka：同步检索 / 文档任务无法投递"}
	}
	if s.options.KafkaProbe != nil {
		status, err := s.options.KafkaProbe(ctx)
		if err != nil {
			return probeResult{status: statusError, detail: "Kafka 元数据读取失败：broker 不可达或认证失败"}
		}
		topic := strings.TrimSpace(status.Topic)
		if topic == "" {
			topic = strings.TrimSpace(s.options.DocumentTopic)
		}
		if !status.TopicExists {
			if topic == "" {
				return probeResult{status: statusDegraded, detail: "Kafka 已连接，但文档主题不存在"}
			}
			return probeResult{status: statusDegraded, detail: "Kafka 已连接；主题 " + topic + " 不存在"}
		}
		return probeResult{
			status: statusOK,
			detail: fmt.Sprintf("元数据正常（%d 个 broker；主题 %s 共 %d 个分区）", status.Brokers, topic, status.Partitions),
		}
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return probeResult{status: statusError, detail: "Kafka broker 不可达：" + brokers[0]}
	}
	_ = connection.Close()
	return probeResult{status: statusOK, detail: "仅 TCP 可达，未验证认证/授权（" + brokers[0] + "）"}
}

// pdfHealth is the remote vision parse service /healthz payload. The endpoint
// reports the selected converter, whether a vision model is configured and any
// configuration problem, so the probe can tell a healthy parser from a process
// that answers 200 but cannot parse.
type pdfHealth struct {
	Service            string  `json:"service"`
	Version            string  `json:"version"`
	Status             string  `json:"status"`
	Converter          string  `json:"converter"`
	ConverterRequested string  `json:"converter_requested"`
	ConfigError        *string `json:"config_error"`
	VisionConfigured   *bool   `json:"vision_configured"`
	VisionModel        *string `json:"vision_model"`
}

// probePDF checks the remote vision parser health endpoint and the converter
// state it reports; the in-process Go engine has no external dependency. A 200
// response alone is not proof the parser works (an unconfigured vision model
// still answers), so the payload decides between ok and degraded.
func (s *Service) probePDF(ctx context.Context) probeResult {
	if s.options.PDFEngine != pdfEngineVision {
		return probeResult{status: statusDisabled, detail: "Go 内置解析引擎"}
	}
	baseURL := strings.TrimRight(strings.TrimSpace(s.options.PDFServiceURL), "/")
	if baseURL == "" {
		return probeResult{status: statusError, detail: "未配置 PDF 解析服务地址"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return probeResult{status: statusError, detail: "PDF 解析服务地址无效"}
	}
	response, err := s.httpClient.Do(request)
	if err != nil {
		return probeResult{status: statusError, detail: "PDF 解析服务不可达"}
	}
	defer func() { _ = response.Body.Close() }()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return probeResult{status: statusError, detail: fmt.Sprintf("健康检查返回 HTTP %d", response.StatusCode)}
	}
	if readErr != nil {
		return probeResult{status: statusDegraded, detail: "健康检查响应读取失败"}
	}
	var health pdfHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return probeResult{status: statusDegraded, detail: "健康检查响应无法解析"}
	}
	switch strings.ToLower(strings.TrimSpace(health.Status)) {
	case "ok":
		converter := strings.TrimSpace(health.Converter)
		if converter == "" {
			converter = "未知转换器"
		}
		detail := "转换器 " + boundedText(converter, 40)
		if version := strings.TrimSpace(health.Version); version != "" {
			detail += "，版本 " + boundedText(version, 40)
		}
		if health.VisionConfigured != nil && *health.VisionConfigured {
			detail += "，视觉模型已配置"
		}
		return probeResult{status: statusOK, detail: detail}
	case "degraded":
		detail := "转换器不可用"
		if health.ConfigError != nil && strings.TrimSpace(*health.ConfigError) != "" {
			detail += "：" + boundedText(*health.ConfigError, 80)
		} else if converter := strings.TrimSpace(health.Converter); converter != "" {
			detail = "转换器 " + boundedText(converter, 40) + " 不可用"
		}
		return probeResult{status: statusDegraded, detail: detail}
	case "":
		return probeResult{status: statusDegraded, detail: "健康检查未返回状态"}
	default:
		return probeResult{status: statusError, detail: "PDF 解析服务状态为 " + boundedText(health.Status, 40)}
	}
}

// monitorStringIntMap normalizes a nil map to an empty object so the JSON never
// contains null where the Console expects an object.
func monitorStringIntMap(values map[string]int) map[string]int {
	if values == nil {
		return map[string]int{}
	}
	return values
}

// shortError renders an error as one bounded line for the errors array.
func shortError(err error) string {
	if err == nil {
		return "未知错误"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "未知错误"
	}
	const limit = 300
	if len(message) > limit {
		message = message[:limit] + "…"
	}
	return message
}

// roundOneDecimal keeps durations/latencies compact and stable in JSON.
func roundOneDecimal(value float64) float64 {
	return math.Round(value*10) / 10
}
