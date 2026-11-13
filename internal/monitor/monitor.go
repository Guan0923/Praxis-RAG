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
	// pdfEngineDocling mirrors config.PDFEngineDocling without importing the
	// configuration package for one literal.
	pdfEngineDocling = "docling"
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
	wait.Wait()

	dependencies := make([]dependency, 0, len(probes))
	dependencyFailed := false
	for index, item := range dependencyLabels {
		result := results[index]
		if result.status == statusError {
			dependencyFailed = true
		}
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
		Debezium: pipelineDebezium{
			Status: debezium.status,
			Detail: debezium.detail,
			// capture_progress stays nil: only Debezium can read the binlog offset.
		},
		Kafka: kafka,
	}

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
			"uploads": map[string]any{"unfinished": stats.UnfinishedUploads},
		}
		library = map[string]any{
			"documents":          stats.Documents,
			"knowledge_bases":    stats.KnowledgeBases,
			"result_snapshots":   stats.ResultSnapshots,
			"artifact_snapshots": stats.ArtifactSnapshots,
		}
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
		"ok":           statsErr == nil && !dependencyFailed,
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

// debeziumHealth is the subset of the Debezium/Connect health payload the probe
// reads.
type debeziumHealth struct {
	Status string `json:"status"`
}

// probeDebezium checks the optional capture-connector health endpoint. It never
// reports a capture offset: only Debezium knows the binlog position.
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
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return probeResult{status: statusError, detail: fmt.Sprintf("健康检查返回 HTTP %d", response.StatusCode)}
	}
	if readErr != nil {
		return probeResult{status: statusDegraded, detail: "健康检查响应读取失败"}
	}
	var health debeziumHealth
	if err := json.Unmarshal(body, &health); err != nil {
		return probeResult{status: statusDegraded, detail: "健康检查响应无法解析"}
	}
	switch strings.ToUpper(strings.TrimSpace(health.Status)) {
	case "UP":
		return probeResult{status: statusOK, detail: "健康检查正常；采集位点需在 Debezium 侧查看"}
	case "DOWN":
		return probeResult{status: statusError, detail: "Debezium 状态为 DOWN"}
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

// probeKafka performs a TCP reachability check against the first broker. It
// never authenticates, so a reachable port is not proof of a usable cluster.
func (s *Service) probeKafka(ctx context.Context) probeResult {
	brokers := kafkaBrokerList(s.options.KafkaBootstrapServers)
	if len(brokers) == 0 {
		return probeResult{status: statusDisabled, detail: "未配置 Kafka：同步检索 / 文档任务无法投递"}
	}
	dialer := net.Dialer{}
	connection, err := dialer.DialContext(ctx, "tcp", brokers[0])
	if err != nil {
		return probeResult{status: statusError, detail: "Kafka broker 不可达：" + brokers[0]}
	}
	_ = connection.Close()
	return probeResult{status: statusOK, detail: "仅 TCP 可达，未验证认证/授权（" + brokers[0] + "）"}
}

// probePDF checks the remote Docling parser health endpoint; the in-process Go
// engine has no external dependency.
func (s *Service) probePDF(ctx context.Context) probeResult {
	if s.options.PDFEngine != pdfEngineDocling {
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
	_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return probeResult{status: statusError, detail: fmt.Sprintf("健康检查返回 HTTP %d", response.StatusCode)}
	}
	return probeResult{status: statusOK, detail: "健康检查通过"}
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
