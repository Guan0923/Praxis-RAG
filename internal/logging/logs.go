package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Environment variables understood by Setup for the Console log view.
const (
	// FileEnvVar optionally makes every process that calls Setup append JSON
	// Lines to that file in addition to stderr. The monitoring page can then
	// read the workers' logs too.
	FileEnvVar = "RAG_LOG_FILE"
	// BufferSizeEnvVar bounds the in-memory ring the monitoring page reads.
	BufferSizeEnvVar = "RAG_LOG_BUFFER_SIZE"
)

// Log component ids shared with the Console top bar. The ids are part of the
// HTTP contract and must not change.
const (
	ComponentMySQL    = "mysql"
	ComponentRedis    = "redis"
	ComponentQdrant   = "qdrant"
	ComponentKafka    = "kafka"
	ComponentPDF      = "pdf"
	ComponentWorker   = "worker"
	ComponentDebezium = "debezium"
	ComponentModel    = "model"
	ComponentSearch   = "search"
	ComponentHTTP     = "http"
	ComponentMonitor  = "monitor"
	ComponentService  = "service"
	ComponentOther    = "other"
)

// Limits of the Console log query and the bounded file tail read.
const (
	// DefaultLogLimit is the entries page size when the caller omits limit.
	DefaultLogLimit = 100
	// MaxLogLimit is the largest accepted page size (larger values are clamped).
	MaxLogLimit = 500
	// MaxLogOffset bounds the accepted pagination offset.
	MaxLogOffset = 100000
	// MaxLogQueryRunes bounds the keyword filter.
	MaxLogQueryRunes = 200
	// MaxTailBytes bounds how much of the JSON Lines file one request reads.
	MaxTailBytes = 512 * 1024
	// DefaultBufferSize is the ring size when RAG_LOG_BUFFER_SIZE is unset.
	DefaultBufferSize = 1000
	// MinBufferSize / MaxBufferSize are the accepted RAG_LOG_BUFFER_SIZE range.
	MinBufferSize = 100
	MaxBufferSize = 20000

	// maxAttrValueRunes / maxMessageRunes bound one rendered log line.
	maxAttrValueRunes = 500
	maxMessageRunes   = 2000
)

// Component is one Console log tab.
type Component struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// componentCatalog is the fixed tab order shared with the Console.
var componentCatalog = []Component{
	{ComponentMySQL, "MySQL"},
	{ComponentRedis, "Redis"},
	{ComponentQdrant, "Qdrant"},
	{ComponentKafka, "Kafka"},
	{ComponentPDF, "PDF 解析服务"},
	{ComponentWorker, "文档 Worker"},
	{ComponentDebezium, "Debezium"},
	{ComponentModel, "模型调用"},
	{ComponentSearch, "检索"},
	{ComponentHTTP, "HTTP API"},
	{ComponentMonitor, "系统监控"},
	{ComponentService, "服务"},
	{ComponentOther, "其他"},
}

// sourcePathComponents maps the caller's source package to a log component.
// The table is ordered from the most specific marker to the least specific and
// matches on a path segment so "internal/documentworker" never resolves to
// "internal/document".
var sourcePathComponents = []struct {
	marker    string
	component string
}{
	{"internal/state/", ComponentMySQL},
	{"internal/redisstate/", ComponentRedis},
	{"internal/qdrant/", ComponentQdrant},
	{"internal/mq/", ComponentKafka},
	{"internal/cacheconsumer/", ComponentKafka},
	{"internal/documentworker/", ComponentWorker},
	{"internal/ingest/", ComponentWorker},
	{"internal/document/", ComponentWorker},
	{"internal/cdcacceptance/", ComponentDebezium},
	{"internal/modelapi/", ComponentModel},
	{"internal/retrieval/", ComponentSearch},
	{"internal/searchrefs/", ComponentSearch},
	{"internal/sparse/", ComponentSearch},
	{"internal/httpapi/", ComponentHTTP},
	{"internal/mcp/", ComponentHTTP},
	{"internal/monitor/", ComponentMonitor},
	{"cmd/rag-service/", ComponentService},
	{"cmd/rag-document-worker/", ComponentWorker},
	{"cmd/rag-cache-worker/", ComponentKafka},
	{"cmd/rag-dead-letter-worker/", ComponentWorker},
}

// Components returns a copy of the fixed component catalog.
func Components() []Component {
	return append([]Component(nil), componentCatalog...)
}

// ComponentLabel resolves one component id to its Console label.
func ComponentLabel(id string) (string, bool) {
	for _, component := range componentCatalog {
		if component.ID == id {
			return component.Label, true
		}
	}
	return "", false
}

// IsComponent reports whether id is a known component id.
func IsComponent(id string) bool {
	_, ok := ComponentLabel(id)
	return ok
}

// ComponentForSource maps a caller source path to a component id. Unknown
// packages map to "other".
func ComponentForSource(sourcePath string) string {
	normalized := "/" + strings.ReplaceAll(strings.TrimSpace(sourcePath), "\\", "/")
	for _, rule := range sourcePathComponents {
		if strings.Contains(normalized, "/"+rule.marker) {
			return rule.component
		}
	}
	return ComponentOther
}

// Entry is one log row of GET /v1/admin/monitor/logs. The JSON field names are
// the Console contract.
type Entry struct {
	Seq            int64             `json:"seq"`
	Time           string            `json:"time"`
	Level          string            `json:"level"`
	Component      string            `json:"component"`
	ComponentLabel string            `json:"component_label"`
	Message        string            `json:"message"`
	Attrs          map[string]string `json:"attrs"`
	Source         string            `json:"source"`
}

// ComponentCount is the number of buffered entries per component; the Console
// renders it as the 全部日志 component filter counts. It ignores
// component/level/query filters.
type ComponentCount struct {
	ID    string `json:"id"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

// LogFilter is one validated Console log query.
type LogFilter struct {
	Component string
	// Level is the canonical minimum level (debug/info/warn/error); empty means
	// no level filter.
	Level  string
	Query  string
	Limit  int
	Offset int
}

// LogPage is the filtered/paginated result of QueryEntries.
type LogPage struct {
	Entries    []Entry          `json:"entries"`
	Total      int              `json:"total"`
	HasMore    bool             `json:"has_more"`
	Components []ComponentCount `json:"components"`
}

// Buffer is the process-wide bounded in-memory log ring. It is safe for
// concurrent use.
type Buffer struct {
	mu       sync.Mutex
	capacity int
	entries  []Entry
	start    int
	length   int
	sequence int64
}

// NewBuffer builds a ring with the given capacity (defaulting to
// DefaultBufferSize for non-positive values).
func NewBuffer(capacity int) *Buffer {
	if capacity <= 0 {
		capacity = DefaultBufferSize
	}
	return &Buffer{capacity: capacity, entries: make([]Entry, capacity)}
}

// Capacity reports the configured ring size.
func (b *Buffer) Capacity() int {
	if b == nil {
		return 0
	}
	return b.capacity
}

// Len reports how many entries are currently buffered.
func (b *Buffer) Len() int {
	if b == nil {
		return 0
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.length
}

// Add appends one entry, assigning its monotonically increasing sequence and
// evicting the oldest row once the ring is full.
func (b *Buffer) Add(entry Entry) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if entry.Component == "" {
		entry.Component = ComponentOther
		entry.ComponentLabel, _ = ComponentLabel(ComponentOther)
	}
	b.sequence++
	entry.Seq = b.sequence
	if b.length < b.capacity {
		b.entries[(b.start+b.length)%b.capacity] = entry
		b.length++
		return
	}
	b.entries[b.start] = entry
	b.start = (b.start + 1) % b.capacity
}

// Snapshot returns a copy of the buffered entries, newest first.
func (b *Buffer) Snapshot() []Entry {
	if b == nil {
		return []Entry{}
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entries := make([]Entry, 0, b.length)
	for index := 0; index < b.length; index++ {
		entries = append(entries, b.entries[(b.start+index)%b.capacity])
	}
	reverseEntries(entries)
	return entries
}

// Query filters and pages the buffered entries.
func (b *Buffer) Query(filter LogFilter) LogPage {
	return QueryEntries(b.Snapshot(), filter)
}

// QueryEntries filters the newest-first entries by component (exact id),
// minimum level and keyword, then paginates them. Use CountComponents for the
// unfiltered tab badges.
func QueryEntries(entries []Entry, filter LogFilter) LogPage {
	matched := make([]Entry, 0, len(entries))
	minimumLevel := 0
	levelFiltered := strings.TrimSpace(filter.Level) != ""
	if levelFiltered {
		minimumLevel = levelRank(filter.Level)
	}
	component := strings.TrimSpace(filter.Component)
	needle := strings.ToLower(strings.TrimSpace(filter.Query))
	for _, entry := range entries {
		if component != "" && entry.Component != component {
			continue
		}
		if levelFiltered && levelRank(entry.Level) < minimumLevel {
			continue
		}
		if needle != "" && !entryMatches(entry, needle) {
			continue
		}
		matched = append(matched, entry)
	}
	total := len(matched)
	limit := filter.Limit
	if limit <= 0 {
		limit = DefaultLogLimit
	}
	if limit > MaxLogLimit {
		limit = MaxLogLimit
	}
	offset := filter.Offset
	if offset < 0 {
		offset = 0
	}
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	page := matched[offset:end]
	if page == nil {
		page = []Entry{}
	}
	return LogPage{
		Entries:    page,
		Total:      total,
		HasMore:    offset+len(page) < total,
		Components: CountComponents(entries),
	}
}

// CountComponents tallies every entry per component id in the fixed catalog
// order, dropping zero counts.
func CountComponents(entries []Entry) []ComponentCount {
	if len(entries) == 0 {
		return []ComponentCount{}
	}
	counts := make(map[string]int, len(componentCatalog))
	for _, entry := range entries {
		counts[entry.Component]++
	}
	result := make([]ComponentCount, 0, len(counts))
	for _, component := range componentCatalog {
		if counts[component.ID] == 0 {
			continue
		}
		result = append(result, ComponentCount{
			ID:    component.ID,
			Label: component.Label,
			Count: counts[component.ID],
		})
	}
	return result
}

// entryMatches reports whether the lower-cased needle appears in the message,
// source, component label or one attribute value. It is a case-insensitive
// substring match: the Console search box is a quick filter, not a query
// language.
func entryMatches(entry Entry, needle string) bool {
	if strings.Contains(strings.ToLower(entry.Message), needle) ||
		strings.Contains(strings.ToLower(entry.Source), needle) ||
		strings.Contains(strings.ToLower(entry.ComponentLabel), needle) {
		return true
	}
	for key, value := range entry.Attrs {
		if strings.Contains(strings.ToLower(key), needle) ||
			strings.Contains(strings.ToLower(value), needle) {
			return true
		}
	}
	return false
}

// NormalizeLevelFilter maps a Console level filter (debug/info/warn/warning/
// error) to the canonical minimum level. ok=false means the value is invalid.
func NormalizeLevelFilter(value string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return "debug", true
	case "info":
		return "info", true
	case "warn", "warning":
		return "warn", true
	case "error":
		return "error", true
	default:
		return "", false
	}
}

// levelRank orders the four rendered levels; unknown file levels rank as info
// so they stay visible unless a warn/error filter is applied.
func levelRank(level string) int {
	switch strings.ToUpper(strings.TrimSpace(level)) {
	case "DEBUG":
		return 0
	case "WARN", "WARNING":
		return 2
	case "ERROR":
		return 3
	default:
		return 1
	}
}

// levelLabel buckets a slog level into the four rendered names.
func levelLabel(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return "ERROR"
	case level >= slog.LevelWarn:
		return "WARN"
	case level >= slog.LevelInfo:
		return "INFO"
	default:
		return "DEBUG"
	}
}

// reverseEntries flips a slice in place.
func reverseEntries(entries []Entry) {
	for left, right := 0, len(entries)-1; left < right; left, right = left+1, right-1 {
		entries[left], entries[right] = entries[right], entries[left]
	}
}

// tailLine is one parsed JSON Lines row before the component/sequence fields
// are resolved.
type tailLine struct {
	entry      Entry
	sourcePath string
}

// ReadLogFile reads the bounded tail of the configured JSON Lines log file.
// path must come from the startup configuration (RAG_LOG_FILE), never from a
// request parameter. The first partial line of the tail window is dropped, and
// lines that are not valid JSON are counted in Info.Note/Notes instead of
// failing the whole read.
func ReadLogFile(filePath string, maxBytes int64) (FileSnapshot, error) {
	snapshot := FileSnapshot{
		Entries: []Entry{},
		Info:    FileInfo{Configured: strings.TrimSpace(filePath) != "", Path: strings.TrimSpace(filePath)},
		Notes:   []string{},
	}
	if !snapshot.Info.Configured {
		snapshot.Info.Note = "未配置 " + FileEnvVar + "，文件日志不可用"
		return snapshot, nil
	}
	if maxBytes <= 0 || maxBytes > MaxTailBytes {
		maxBytes = MaxTailBytes
	}
	file, err := os.Open(snapshot.Info.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			snapshot.Info.Note = "日志文件不存在（尚未写入）"
			return snapshot, nil
		}
		return snapshot, fmt.Errorf("打开日志文件失败：%w", err)
	}
	defer func() { _ = file.Close() }()
	snapshot.Available = true
	stat, err := file.Stat()
	if err != nil {
		return snapshot, fmt.Errorf("读取日志文件状态失败：%w", err)
	}
	snapshot.Info.SizeBytes = stat.Size()
	offset := int64(0)
	if stat.Size() > maxBytes {
		offset = stat.Size() - maxBytes
		snapshot.Info.Truncated = true
		snapshot.Info.Note = fmt.Sprintf("日志文件较大，仅读取最近 %d KiB", maxBytes/1024)
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return snapshot, fmt.Errorf("定位日志文件失败：%w", err)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes))
	if err != nil {
		return snapshot, fmt.Errorf("读取日志文件失败：%w", err)
	}
	if offset > 0 {
		// The window starts in the middle of a line: drop up to the first newline.
		newline := bytes.IndexByte(data, '\n')
		if newline < 0 {
			data = nil
		} else {
			data = data[newline+1:]
		}
	}
	lines := bytes.Split(data, []byte("\n"))
	parsed := make([]tailLine, 0, len(lines))
	unparsable := 0
	for _, line := range lines {
		line = bytes.TrimRight(line, "\r")
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		item, ok := parseTailLine(line)
		if !ok {
			unparsable++
			continue
		}
		parsed = append(parsed, item)
	}
	if unparsable > 0 {
		message := fmt.Sprintf("文件日志有 %d 行无法解析，已跳过", unparsable)
		snapshot.Notes = append(snapshot.Notes, message)
		if snapshot.Info.Note == "" {
			snapshot.Info.Note = fmt.Sprintf("有 %d 行无法解析", unparsable)
		} else {
			snapshot.Info.Note += "；" + fmt.Sprintf("有 %d 行无法解析", unparsable)
		}
	}
	// The tail is chronological; the Console contract is newest first.
	for index := len(parsed) - 1; index >= 0; index-- {
		entry := parsed[index].entry
		entry.Seq = int64(index + 1)
		snapshot.Entries = append(snapshot.Entries, entry)
	}
	if snapshot.Info.SizeBytes == 0 {
		snapshot.Info.Note = "日志文件为空（尚未写入）"
	}
	return snapshot, nil
}

// FileInfo is the Console "file" object of the log response.
type FileInfo struct {
	Configured bool   `json:"configured"`
	Path       string `json:"path"`
	SizeBytes  int64  `json:"size_bytes"`
	Truncated  bool   `json:"truncated"`
	Note       string `json:"note"`
}

// FileSnapshot is one bounded tail read of the configured log file.
type FileSnapshot struct {
	Entries []Entry
	Info    FileInfo
	Notes   []string
	// Available reports whether file logging is configured and the file could
	// be read (an empty or missing-but-configured file is still not readable).
	Available bool
}

// StatLogFile reports the configured file metadata without reading any rows.
func StatLogFile(filePath string) FileInfo {
	info := FileInfo{Configured: strings.TrimSpace(filePath) != "", Path: strings.TrimSpace(filePath)}
	if !info.Configured {
		info.Note = "未配置 " + FileEnvVar + "，文件日志不可用"
		return info
	}
	stat, err := os.Stat(info.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			info.Note = "日志文件不存在（尚未写入）"
		} else {
			info.Note = "日志文件状态读取失败：" + err.Error()
		}
		return info
	}
	info.SizeBytes = stat.Size()
	return info
}

// parseTailLine decodes one JSON Lines row written by the file JSON handler.
// The known fields are time/level/msg/source; every other top-level key is an
// attribute. A line that is not a JSON object is reported as unparsable.
func parseTailLine(line []byte) (tailLine, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		return tailLine{}, false
	}
	item := tailLine{entry: Entry{
		Level:     "INFO",
		Component: ComponentOther,
		Attrs:     map[string]string{},
	}}
	if value, ok := raw["time"]; ok {
		var text string
		if json.Unmarshal(value, &text) == nil {
			item.entry.Time = normalizeTime(text)
		}
	}
	if value, ok := raw["level"]; ok {
		var text string
		if json.Unmarshal(value, &text) == nil {
			item.entry.Level = levelLabel(levelFromText(text))
		}
	}
	if value, ok := raw["msg"]; ok {
		var text string
		if json.Unmarshal(value, &text) == nil {
			item.entry.Message = RedactLimit(text, maxMessageRunes)
		}
	}
	if value, ok := raw["source"]; ok {
		var source struct {
			File string `json:"file"`
			Line int    `json:"line"`
		}
		var text string
		switch {
		case json.Unmarshal(value, &source) == nil && strings.TrimSpace(source.File) != "":
			item.sourcePath = source.File
			item.entry.Source = relativeSource(source.File, source.Line)
		case json.Unmarshal(value, &text) == nil:
			item.entry.Source = RedactLimit(text, 300)
		}
	}
	for key, value := range raw {
		switch key {
		case "time", "level", "msg", "source":
			continue
		}
		item.entry.Attrs[key] = RedactLimit(rawValueText(value), maxAttrValueRunes)
	}
	item.entry.Component, item.entry.Attrs = classifyComponent(item.entry.Attrs, item.sourcePath)
	item.entry.ComponentLabel, _ = ComponentLabel(item.entry.Component)
	return item, true
}

// levelFromText converts one stored level name back to a slog level.
func levelFromText(text string) slog.Level {
	switch strings.ToUpper(strings.TrimSpace(text)) {
	case "DEBUG":
		return slog.LevelDebug
	case "WARN", "WARNING":
		return slog.LevelWarn
	case "ERROR":
		return slog.LevelError
	default:
		return slog.LevelInfo
	}
}

// normalizeTime renders a stored timestamp as RFC3339 (keeping the source text
// when it cannot be parsed).
func normalizeTime(text string) string {
	text = strings.TrimSpace(text)
	if parsed, err := time.Parse(time.RFC3339Nano, text); err == nil {
		return parsed.UTC().Format(time.RFC3339)
	}
	return RedactLimit(text, 100)
}

// rawValueText renders one JSON attribute value as the Console string. Nested
// objects/arrays stay JSON text so a structured attribute is not silently lost.
func rawValueText(value json.RawMessage) string {
	if len(value) == 0 {
		return ""
	}
	var text string
	if json.Unmarshal(value, &text) == nil {
		return text
	}
	return string(value)
}

// classifyComponent resolves the explicit component attribute first (it must be
// a known id), then the caller source path, and removes the component attribute
// from the rendered attrs so it is not duplicated.
func classifyComponent(attrs map[string]string, sourcePath string) (string, map[string]string) {
	component := ""
	if explicit := explicitComponent(attrs); explicit != "" {
		component = explicit
	}
	for key := range attrs {
		if key == "component" || strings.HasSuffix(key, ".component") {
			delete(attrs, key)
		}
	}
	if component == "" {
		component = ComponentForSource(sourcePath)
	}
	return component, attrs
}

// explicitComponent returns the known component id carried by an explicit
// component attribute, supporting grouped attribute keys ("group.component").
// Unknown values are ignored so the source path decides instead.
func explicitComponent(attrs map[string]string) string {
	if value, ok := attrs["component"]; ok {
		if trimmed := strings.TrimSpace(value); IsComponent(trimmed) {
			return trimmed
		}
	}
	keys := make([]string, 0, 2)
	for key := range attrs {
		if strings.HasSuffix(key, ".component") {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	for _, key := range keys {
		if trimmed := strings.TrimSpace(attrs[key]); IsComponent(trimmed) {
			return trimmed
		}
	}
	return ""
}

// relativeSource shortens an absolute caller path to the repository-relative
// form the Console renders ("internal/state/store.go:431").
func relativeSource(file string, line int) string {
	normalized := strings.ReplaceAll(strings.TrimSpace(file), "\\", "/")
	if normalized == "" {
		return ""
	}
	for _, marker := range []string{"/internal/", "/cmd/"} {
		if index := strings.LastIndex(normalized, marker); index >= 0 {
			normalized = normalized[index+1:]
			break
		}
	}
	if line > 0 {
		return normalized + ":" + strconv.Itoa(line)
	}
	return normalized
}

// captureHandler records every enabled record into the process ring and then
// forwards it to the real handler (stderr plus the optional JSON file). It must
// be the outermost handler so WithAttrs/WithGroup reach both the buffer
// classification and the real sinks.
type captureHandler struct {
	buffer   *Buffer
	minLevel slog.Level
	next     slog.Handler
	attrs    []slog.Attr
	groups   []string
}

// Enabled reports whether the record passes the configured process level.
func (h *captureHandler) Enabled(ctx context.Context, level slog.Level) bool {
	if level >= h.minLevel {
		return true
	}
	return h.next != nil && h.next.Enabled(ctx, level)
}

// Handle records the entry and forwards it to the real sinks.
func (h *captureHandler) Handle(ctx context.Context, record slog.Record) error {
	if h.buffer != nil && record.Level >= h.minLevel {
		h.buffer.Add(h.entry(record))
	}
	if h.next != nil {
		return h.next.Handle(ctx, record)
	}
	return nil
}

// WithAttrs keeps the attributes for classification and passes them down.
func (h *captureHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	clone := &captureHandler{
		buffer:   h.buffer,
		minLevel: h.minLevel,
		attrs:    append(append([]slog.Attr(nil), h.attrs...), attrs...),
		groups:   append([]string(nil), h.groups...),
	}
	if h.next != nil {
		clone.next = h.next.WithAttrs(attrs)
	}
	return clone
}

// WithGroup keeps the open group for attribute flattening and passes it down.
func (h *captureHandler) WithGroup(name string) slog.Handler {
	clone := &captureHandler{
		buffer:   h.buffer,
		minLevel: h.minLevel,
		attrs:    append([]slog.Attr(nil), h.attrs...),
		groups:   append(append([]string(nil), h.groups...), name),
	}
	if h.next != nil {
		clone.next = h.next.WithGroup(name)
	}
	return clone
}

// entry converts one slog record into the Console row.
func (h *captureHandler) entry(record slog.Record) Entry {
	entry := Entry{
		Time:      record.Time.UTC().Format("2006-01-02T15:04:05Z07:00"),
		Level:     levelLabel(record.Level),
		Message:   RedactLimit(record.Message, maxMessageRunes),
		Component: ComponentOther,
		Attrs:     map[string]string{},
	}
	sourcePath := ""
	if record.PC != 0 {
		frame, _ := runtime.CallersFrames([]uintptr{record.PC}).Next()
		sourcePath = frame.File
		entry.Source = relativeSource(frame.File, frame.Line)
	}
	prefix := strings.Join(h.groups, ".")
	for _, attr := range h.attrs {
		flattenAttr(entry.Attrs, prefix, attr)
	}
	record.Attrs(func(attr slog.Attr) bool {
		flattenAttr(entry.Attrs, prefix, attr)
		return true
	})
	entry.Component, entry.Attrs = classifyComponent(entry.Attrs, sourcePath)
	entry.ComponentLabel, _ = ComponentLabel(entry.Component)
	return entry
}

// flattenAttr normalizes one slog attribute (including nested groups) into the
// flat string map the Console renders.
func flattenAttr(target map[string]string, prefix string, attr slog.Attr) {
	attr.Value = attr.Value.Resolve()
	key := attr.Key
	if prefix != "" {
		key = prefix + "." + key
	}
	if attr.Value.Kind() == slog.KindGroup {
		for _, child := range attr.Value.Group() {
			flattenAttr(target, key, child)
		}
		return
	}
	if key == "" {
		return
	}
	target[key] = RedactLimit(attr.Value.String(), maxAttrValueRunes)
}

// teeHandler fans one record out to every sink that accepts its level.
type teeHandler struct {
	handlers []slog.Handler
}

// newTeeHandler builds a fan-out handler for the given sinks.
func newTeeHandler(handlers []slog.Handler) *teeHandler {
	return &teeHandler{handlers: handlers}
}

// Enabled reports whether any sink accepts the level.
func (t *teeHandler) Enabled(ctx context.Context, level slog.Level) bool {
	for _, handler := range t.handlers {
		if handler.Enabled(ctx, level) {
			return true
		}
	}
	return false
}

// Handle forwards the record to every enabled sink.
func (t *teeHandler) Handle(ctx context.Context, record slog.Record) error {
	var firstErr error
	for _, handler := range t.handlers {
		if !handler.Enabled(ctx, record.Level) {
			continue
		}
		if err := handler.Handle(ctx, record.Clone()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// WithAttrs forwards the attributes to every sink.
func (t *teeHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	cloned := make([]slog.Handler, 0, len(t.handlers))
	for _, handler := range t.handlers {
		cloned = append(cloned, handler.WithAttrs(attrs))
	}
	return &teeHandler{handlers: cloned}
}

// WithGroup forwards the open group to every sink.
func (t *teeHandler) WithGroup(name string) slog.Handler {
	cloned := make([]slog.Handler, 0, len(t.handlers))
	for _, handler := range t.handlers {
		cloned = append(cloned, handler.WithGroup(name))
	}
	return &teeHandler{handlers: cloned}
}

// processState publishes the buffer and file path Setup installed. Setup runs
// once per process; tests call it more than once, so the accessors are guarded.
var processState struct {
	mu     sync.RWMutex
	buffer *Buffer
	file   string
}

// logFileState owns the open RAG_LOG_FILE sink so a graceful shutdown (and
// tests) can release it deterministically.
var logFileState struct {
	mu   sync.Mutex
	file *os.File
}

// CloseLogFile closes the RAG_LOG_FILE sink opened by Setup. It is safe to call
// when file logging is disabled and safe to call more than once.
func CloseLogFile() error {
	logFileState.mu.Lock()
	defer logFileState.mu.Unlock()
	if logFileState.file == nil {
		return nil
	}
	err := logFileState.file.Close()
	logFileState.file = nil
	return err
}

// DefaultBuffer returns the process-wide log ring installed by Setup, or nil
// when Setup has not run.
func DefaultBuffer() *Buffer {
	processState.mu.RLock()
	defer processState.mu.RUnlock()
	return processState.buffer
}

// LogFilePath returns the absolute RAG_LOG_FILE path configured at startup, or
// "" when file logging is disabled.
func LogFilePath() string {
	processState.mu.RLock()
	defer processState.mu.RUnlock()
	return processState.file
}

// publishProcessState stores the current Setup wiring for the accessors.
func publishProcessState(buffer *Buffer, filePath string) {
	processState.mu.Lock()
	processState.buffer = buffer
	processState.file = filePath
	processState.mu.Unlock()
}

// bufferSizeFromEnv resolves RAG_LOG_BUFFER_SIZE, failing fast on invalid
// values so the Console does not silently read a differently sized ring.
func bufferSizeFromEnv(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv(BufferSizeEnvVar))
	if raw == "" {
		return DefaultBufferSize, nil
	}
	size, err := strconv.Atoi(raw)
	if err != nil || size < MinBufferSize || size > MaxBufferSize {
		return 0, fmt.Errorf("%s 必须是 %d 到 %d 之间的整数（当前 %q）",
			BufferSizeEnvVar, MinBufferSize, MaxBufferSize, raw)
	}
	return size, nil
}

// openLogFile resolves RAG_LOG_FILE and builds the JSON Lines sink. A file that
// cannot be opened is a configuration error: the entry points fail fast instead
// of silently dropping the workers' logs.
func openLogFile(options Options) (string, slog.Handler, error) {
	raw := strings.TrimSpace(os.Getenv(FileEnvVar))
	if raw == "" {
		_ = CloseLogFile()
		return "", nil, nil
	}
	absolute, err := filepath.Abs(raw)
	if err != nil {
		return "", nil, fmt.Errorf("%s 路径无效（%q）：%w", FileEnvVar, raw, err)
	}
	// A repeated Setup (unit tests) must not leak the previous descriptor.
	_ = CloseLogFile()
	file, err := os.OpenFile(absolute, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return "", nil, fmt.Errorf("无法打开日志文件 %q（%s）：%w；请确认目录存在且进程有写权限",
			absolute, FileEnvVar, err)
	}
	logFileState.mu.Lock()
	logFileState.file = file
	logFileState.mu.Unlock()
	handler := slog.NewJSONHandler(file, &slog.HandlerOptions{
		Level:     options.Level,
		AddSource: true,
	})
	return absolute, handler, nil
}
