package monitor

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
)

// ErrInvalidLogQuery is the sentinel the HTTP layer maps to 400. Every
// validation failure wraps it, so callers use errors.Is instead of matching
// Chinese detail strings.
var ErrInvalidLogQuery = errors.New("日志查询参数非法")

// invalidLogQuery carries the operator-facing Chinese reason. Error() returns
// the reason alone so the HTTP detail stays exactly "未知日志组件: xxx" while
// errors.Is(err, ErrInvalidLogQuery) still matches.
type invalidLogQuery struct {
	reason string
}

// Error implements error.
func (e *invalidLogQuery) Error() string { return e.reason }

// Unwrap exposes the sentinel to errors.Is.
func (e *invalidLogQuery) Unwrap() error { return ErrInvalidLogQuery }

// Log source values of the Console log endpoint.
const (
	logSourceMemory = "memory"
	logSourceFile   = "file"
)

// LogQuery is one Console log query. The HTTP layer parses the raw parameters
// and MonitorLogs validates them in one place.
type LogQuery struct {
	// Source is memory (default) or file.
	Source string
	// Component optionally restricts the rows to one component id.
	Component string
	// Level optionally keeps entries at or above this level
	// (debug/info/warn/warning/error).
	Level string
	// Query optionally keeps entries whose message/source/attrs contain it.
	Query string
	// Limit is the page size; the caller normally passes DefaultLogLimit.
	Limit int
	// Offset skips that many matched rows.
	Offset int
}

// LogsView is the response body of GET /v1/admin/monitor/logs. The JSON field
// names are the Console contract and must not change.
type LogsView struct {
	GeneratedAt string                   `json:"generated_at"`
	Source      string                   `json:"source"`
	Available   bool                     `json:"available"`
	Entries     []logging.Entry          `json:"entries"`
	Total       int                      `json:"total"`
	HasMore     bool                     `json:"has_more"`
	Components  []logging.ComponentCount `json:"components"`
	File        logging.FileInfo         `json:"file"`
	Notes       []string                 `json:"notes"`
}

// MonitorLogs assembles one page of the Console log view from the in-memory
// ring or the startup-configured JSON Lines file. Query validation errors wrap
// ErrInvalidLogQuery; an unavailable source is a normal 200 response with
// available=false and a Chinese note.
func (s *Service) MonitorLogs(ctx context.Context, query LogQuery) (*LogsView, error) {
	normalized, filter, err := normalizeLogQuery(query)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	response := &LogsView{
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      normalized.Source,
		Available:   true,
		Entries:     []logging.Entry{},
		Components:  []logging.ComponentCount{},
		File:        logging.StatLogFile(s.options.LogFilePath),
		Notes:       []string{},
	}
	if normalized.Source == logSourceFile {
		return s.fileLogsView(response, normalized, filter)
	}
	if s.options.Logs == nil {
		response.Available = false
		response.Notes = append(response.Notes, "进程内日志缓冲未装配，内存日志不可用")
		return response, nil
	}
	page := s.options.Logs.Query(filter)
	response.Entries = page.Entries
	response.Total = page.Total
	response.HasMore = page.HasMore
	response.Components = page.Components
	return response, nil
}

// fileLogsView renders the file source. The path comes only from the startup
// wiring (Options.LogFilePath); request parameters never select a file.
func (s *Service) fileLogsView(response *LogsView, query LogQuery, filter logging.LogFilter) (*LogsView, error) {
	filePath := strings.TrimSpace(s.options.LogFilePath)
	if filePath == "" {
		response.Available = false
		response.File = logging.FileInfo{
			Configured: false,
			Path:       "",
			Note:       "未配置 RAG_LOG_FILE，文件日志不可用",
		}
		response.Notes = append(response.Notes, "未配置 RAG_LOG_FILE，文件日志不可用")
		return response, nil
	}
	snapshot, err := logging.ReadLogFile(filePath, logging.MaxTailBytes)
	response.File = snapshot.Info
	response.Available = snapshot.Available
	if len(snapshot.Notes) > 0 {
		response.Notes = append(response.Notes, snapshot.Notes...)
	}
	if err != nil {
		response.Notes = append(response.Notes, "日志文件读取失败："+shortError(err))
		return response, nil
	}
	if !snapshot.Available {
		return response, nil
	}
	page := logging.QueryEntries(snapshot.Entries, filter)
	response.Entries = page.Entries
	response.Total = page.Total
	response.HasMore = page.HasMore
	response.Components = page.Components
	return response, nil
}

// normalizeLogQuery validates and normalizes one Console query in a single
// place. It returns the normalized query plus the shared logging filter the
// memory and file sources both consume.
func normalizeLogQuery(query LogQuery) (LogQuery, logging.LogFilter, error) {
	normalized := LogQuery{}
	source := strings.ToLower(strings.TrimSpace(query.Source))
	if source == "" {
		source = logSourceMemory
	}
	if source != logSourceMemory && source != logSourceFile {
		return normalized, logging.LogFilter{}, invalidLogQueryf("未知日志来源: %s", strings.TrimSpace(query.Source))
	}
	normalized.Source = source

	component := strings.ToLower(strings.TrimSpace(query.Component))
	if component != "" && !logging.IsComponent(component) {
		return normalized, logging.LogFilter{}, invalidLogQueryf("未知日志组件: %s", strings.TrimSpace(query.Component))
	}
	normalized.Component = component

	level := ""
	if rawLevel := strings.TrimSpace(query.Level); rawLevel != "" {
		canonical, ok := logging.NormalizeLevelFilter(rawLevel)
		if !ok {
			return normalized, logging.LogFilter{}, invalidLogQueryf("未知日志级别: %s", rawLevel)
		}
		level = canonical
	}
	normalized.Level = level

	keyword := strings.TrimSpace(query.Query)
	if utf8.RuneCountInString(keyword) > logging.MaxLogQueryRunes {
		return normalized, logging.LogFilter{}, invalidLogQueryf(
			"查询关键字过长（最多 %d 个字符）", logging.MaxLogQueryRunes)
	}
	normalized.Query = keyword

	limit := query.Limit
	if limit <= 0 {
		limit = logging.DefaultLogLimit
	}
	if limit > logging.MaxLogLimit {
		limit = logging.MaxLogLimit
	}
	normalized.Limit = limit

	if query.Offset < 0 {
		return normalized, logging.LogFilter{}, invalidLogQueryf("偏移量不能为负数")
	}
	if query.Offset > logging.MaxLogOffset {
		return normalized, logging.LogFilter{}, invalidLogQueryf(
			"偏移量超出范围（最大 %d）", logging.MaxLogOffset)
	}
	normalized.Offset = query.Offset

	filter := logging.LogFilter{
		Component: normalized.Component,
		Level:     normalized.Level,
		Query:     normalized.Query,
		Limit:     normalized.Limit,
		Offset:    normalized.Offset,
	}
	return normalized, filter, nil
}

// invalidLogQueryf builds the reason-carrying validation error.
func invalidLogQueryf(format string, args ...any) error {
	return &invalidLogQuery{reason: fmt.Sprintf(format, args...)}
}
