// Package monitor assembles the read-only snapshot behind the Console
// monitoring page: service metadata, bounded dependency probes, queue depth and
// library/activity counters. It never writes state and never exposes
// credentials, queries, document content or broker payloads.
package monitor

import (
	"context"
	"database/sql"
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

	OutboxBridge   string
	UploadMode     string
	DocumentWorker string
	PDFEngine      string
	PDFServiceURL  string

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
			"document_outbox": map[string]any{
				"by_status":              monitorStringIntMap(stats.OutboxByStatus),
				"oldest_pending_seconds": roundOneDecimal(stats.OldestPendingOutboxSeconds),
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
			"name":            version.ServiceName,
			"version":         version.Version,
			"backend":         "go",
			"collection":      s.options.Collection,
			"started_at":      startedAt.UTC().Format(time.RFC3339),
			"uptime_seconds":  roundOneDecimal(uptime),
			"upload_mode":     s.options.UploadMode,
			"outbox_bridge":   s.options.OutboxBridge,
			"document_worker": s.options.DocumentWorker,
			"pdf_engine":      s.options.PDFEngine,
		},
		"dependencies": dependencies,
		"queues":       queues,
		"library":      library,
		"activity":     activity,
		"errors":       errorsList,
	}
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
	brokers := make([]string, 0, len(s.options.KafkaBootstrapServers))
	for _, broker := range s.options.KafkaBootstrapServers {
		if trimmed := strings.TrimSpace(broker); trimmed != "" {
			brokers = append(brokers, trimmed)
		}
	}
	if len(brokers) == 0 {
		return probeResult{status: statusDisabled, detail: "未配置 Kafka：同步检索 / 进程内 outbox"}
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
