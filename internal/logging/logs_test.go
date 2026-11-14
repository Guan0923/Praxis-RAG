package logging

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// queryFixtureEntries builds the newest-first filter/pagination fixture.
func queryFixtureEntries() []Entry {
	return []Entry{
		{Seq: 3, Level: "ERROR", Component: ComponentMySQL, ComponentLabel: "MySQL",
			Message: "mysql down", Source: "internal/state/store.go:1"},
		{Seq: 2, Level: "WARN", Component: ComponentRedis, ComponentLabel: "Redis",
			Message: "redis slow", Attrs: map[string]string{"request_id": "abc"}},
		{Seq: 1, Level: "INFO", Component: ComponentMySQL, ComponentLabel: "MySQL",
			Message: "mysql ready"},
	}
}

// TestBufferRingEvictsOldest asserts the ring keeps the newest rows and the
// sequence keeps increasing across evictions.
func TestBufferRingEvictsOldest(t *testing.T) {
	buffer := NewBuffer(3)
	if buffer.Capacity() != 3 {
		t.Fatalf("Capacity() = %d, want 3", buffer.Capacity())
	}
	for index := 0; index < 5; index++ {
		buffer.Add(Entry{Level: "INFO", Component: ComponentService, Message: "row" + string(rune('0'+index))})
	}
	if buffer.Len() != 3 {
		t.Fatalf("Len() = %d, want 3", buffer.Len())
	}
	snapshot := buffer.Snapshot()
	if len(snapshot) != 3 {
		t.Fatalf("Snapshot() = %d rows, want 3", len(snapshot))
	}
	// Newest first: rows 4, 3, 2 with sequences 5, 4, 3.
	for index, wantSeq := range []int64{5, 4, 3} {
		if snapshot[index].Seq != wantSeq {
			t.Fatalf("snapshot[%d].Seq = %d, want %d", index, snapshot[index].Seq, wantSeq)
		}
	}
	if snapshot[0].Message != "row4" || snapshot[2].Message != "row2" {
		t.Fatalf("snapshot order = %+v", snapshot)
	}
}

// TestQueryEntriesFiltersAndPages asserts the shared filter/pagination path used
// by both the memory and file sources.
func TestQueryEntriesFiltersAndPages(t *testing.T) {
	entries := queryFixtureEntries()

	page := QueryEntries(entries, LogFilter{Component: ComponentMySQL})
	if page.Total != 2 || len(page.Entries) != 2 || page.HasMore {
		t.Fatalf("component filter = %+v", page)
	}
	if page.Components != nil && len(page.Components) != 2 {
		t.Fatalf("components = %+v, want mysql + redis counts", page.Components)
	}
	// components ignore the component filter: both tabs keep their badges.
	if len(page.Components) != 2 || page.Components[0].ID != ComponentMySQL ||
		page.Components[0].Count != 2 || page.Components[1].ID != ComponentRedis {
		t.Fatalf("components = %+v", page.Components)
	}

	warn := QueryEntries(entries, LogFilter{Level: "warn"})
	if warn.Total != 2 || warn.Entries[0].Level != "ERROR" || warn.Entries[1].Level != "WARN" {
		t.Fatalf("level filter = %+v", warn)
	}
	warningAlias := QueryEntries(entries, LogFilter{Level: "warning"})
	if warningAlias.Total != warn.Total {
		t.Fatalf("warning alias total = %d, want %d", warningAlias.Total, warn.Total)
	}

	keyword := QueryEntries(entries, LogFilter{Query: "REDIS"})
	if keyword.Total != 1 || keyword.Entries[0].Component != ComponentRedis {
		t.Fatalf("keyword filter = %+v", keyword)
	}
	attrKeyword := QueryEntries(entries, LogFilter{Query: "abc"})
	if attrKeyword.Total != 1 || attrKeyword.Entries[0].Seq != 2 {
		t.Fatalf("attr keyword filter = %+v", attrKeyword)
	}

	firstPage := QueryEntries(entries, LogFilter{Limit: 1})
	if firstPage.Total != 3 || len(firstPage.Entries) != 1 || !firstPage.HasMore {
		t.Fatalf("limit page = %+v", firstPage)
	}
	secondPage := QueryEntries(entries, LogFilter{Limit: 1, Offset: 1})
	if secondPage.Total != 3 || len(secondPage.Entries) != 1 || !secondPage.HasMore ||
		secondPage.Entries[0].Seq != 2 {
		t.Fatalf("offset page = %+v", secondPage)
	}
	lastPage := QueryEntries(entries, LogFilter{Limit: 1, Offset: 2})
	if lastPage.HasMore || lastPage.Entries[0].Seq != 1 {
		t.Fatalf("last page = %+v", lastPage)
	}
	// The page size is clamped to the contract maximum.
	clamped := QueryEntries(entries, LogFilter{Limit: 100000})
	if len(clamped.Entries) != 3 {
		t.Fatalf("clamped page = %+v", clamped)
	}
	outOfRange := QueryEntries(entries, LogFilter{Offset: 99})
	if outOfRange.Total != 3 || len(outOfRange.Entries) != 0 || outOfRange.HasMore {
		t.Fatalf("out-of-range offset = %+v", outOfRange)
	}
}

// TestComponentClassification pins the source-path table and the explicit
// component attribute precedence.
func TestComponentClassification(t *testing.T) {
	for source, want := range map[string]string{
		`D:\repo\internal\state\store.go`:          ComponentMySQL,
		"/repo/internal/redisstate/client.go":      ComponentRedis,
		"/repo/internal/qdrant/client.go":          ComponentQdrant,
		"/repo/internal/mq/consumer.go":            ComponentKafka,
		"/repo/internal/cacheconsumer/worker.go":   ComponentKafka,
		"/repo/internal/documentworker/worker.go":  ComponentWorker,
		"/repo/internal/ingest/pipeline.go":        ComponentWorker,
		"/repo/internal/document/parse.go":         ComponentWorker,
		"/repo/internal/cdcacceptance/switch.go":   ComponentDebezium,
		"/repo/internal/modelapi/client.go":        ComponentModel,
		"/repo/internal/retrieval/search.go":       ComponentSearch,
		"/repo/internal/searchrefs/refs.go":        ComponentSearch,
		"/repo/internal/sparse/encoder.go":         ComponentSearch,
		"/repo/internal/httpapi/server.go":         ComponentHTTP,
		"/repo/internal/mcp/tools.go":              ComponentHTTP,
		"/repo/internal/monitor/monitor.go":        ComponentMonitor,
		"/repo/cmd/rag-service/main.go":            ComponentService,
		"/repo/cmd/rag-document-worker/main.go":    ComponentWorker,
		"/repo/cmd/rag-cache-worker/main.go":       ComponentKafka,
		"/repo/cmd/rag-dead-letter-worker/main.go": ComponentWorker,
		"/repo/internal/auth/users.go":             ComponentOther,
		"":                                         ComponentOther,
	} {
		if got := ComponentForSource(source); got != want {
			t.Fatalf("ComponentForSource(%q) = %q, want %q", source, got, want)
		}
	}
	if !IsComponent(ComponentPDF) || IsComponent("nope") {
		t.Fatal("IsComponent() mismatch")
	}
	if label, ok := ComponentLabel(ComponentPDF); !ok || label != "PDF 解析服务" {
		t.Fatalf("ComponentLabel(pdf) = %q, %v", label, ok)
	}
	for _, component := range Components() {
		if !IsComponent(component.ID) || component.Label == "" {
			t.Fatalf("catalog entry = %+v", component)
		}
	}
}

// TestCaptureHandlerAttributes asserts the tee records WithAttrs/WithGroup
// values, resolves the source and redacts credential-shaped attribute values.
func TestCaptureHandlerAttributes(t *testing.T) {
	buffer := NewBuffer(10)
	logger := slog.New(&captureHandler{buffer: buffer, minLevel: slog.LevelDebug})
	logger.WithGroup("job").With("component", ComponentMySQL).
		Info("created", slog.Group("payload", "attempt", 2), "authorization", "Bearer abcdefgh")
	logger.Debug("hidden", "component", ComponentNotFoundForTest)

	snapshot := buffer.Snapshot()
	if len(snapshot) != 2 {
		t.Fatalf("buffer = %d entries, want 2", len(snapshot))
	}
	debugEntry := snapshot[0]
	if debugEntry.Level != "DEBUG" {
		t.Fatalf("debug entry level = %q", debugEntry.Level)
	}
	entry := snapshot[1]
	if entry.Level != "INFO" || entry.Component != ComponentMySQL ||
		entry.ComponentLabel != "MySQL" || entry.Message != "created" {
		t.Fatalf("entry = %+v", entry)
	}
	if _, present := entry.Attrs["job.component"]; present {
		t.Fatalf("component attribute leaked into attrs: %+v", entry.Attrs)
	}
	if entry.Attrs["job.payload.attempt"] != "2" {
		t.Fatalf("grouped attribute = %+v", entry.Attrs)
	}
	if !strings.Contains(entry.Attrs["job.authorization"], "***") {
		t.Fatalf("credential attribute not redacted: %+v", entry.Attrs)
	}
	if !strings.Contains(entry.Source, "logs_test.go:") {
		t.Fatalf("source = %q, want the test caller location", entry.Source)
	}
	if debugEntry.Component != ComponentOther {
		t.Fatalf("fallback component = %q, want other", debugEntry.Component)
	}
}

// ComponentNotFoundForTest keeps the explicit-component-unknown fallback test
// readable without exposing a fake id from the production catalog.
const ComponentNotFoundForTest = "not-a-component"

// TestReadLogFileTail asserts the bounded tail read drops the partial first
// line, parses JSON Lines and counts unparsable rows.
func TestReadLogFileTail(t *testing.T) {
	directory := t.TempDir()
	filePath := filepath.Join(directory, "praxis.jsonl")
	valid := `{"time":"2026-11-14T06:00:00Z","level":"INFO","msg":"first","source":{"function":"x","file":"/repo/internal/state/store.go","line":10}}`
	validMySQL := `{"time":"2026-11-14T06:00:01Z","level":"ERROR","msg":"second","component":"mysql","source":{"function":"x","file":"/repo/internal/monitor/monitor.go","line":20}}`
	content := valid + "\n" + validMySQL + "\n" + "not json\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	snapshot, err := ReadLogFile(filePath, MaxTailBytes)
	if err != nil {
		t.Fatalf("ReadLogFile() error = %v", err)
	}
	if !snapshot.Info.Configured || snapshot.Info.Path != filePath || snapshot.Info.Truncated {
		t.Fatalf("file info = %+v", snapshot.Info)
	}
	if snapshot.Info.SizeBytes != int64(len(content)) {
		t.Fatalf("size_bytes = %d, want %d", snapshot.Info.SizeBytes, len(content))
	}
	if len(snapshot.Entries) != 2 {
		t.Fatalf("entries = %+v, want 2 parsed rows", snapshot.Entries)
	}
	// Newest first: the second line is first in the response.
	if snapshot.Entries[0].Message != "second" || snapshot.Entries[1].Message != "first" {
		t.Fatalf("entry order = %+v", snapshot.Entries)
	}
	if snapshot.Entries[0].Component != ComponentMySQL || snapshot.Entries[0].Level != "ERROR" {
		t.Fatalf("explicit component/level = %+v", snapshot.Entries[0])
	}
	if snapshot.Entries[1].Component != ComponentMySQL {
		t.Fatalf("source-path component = %+v", snapshot.Entries[1])
	}
	if snapshot.Entries[0].Source != "internal/monitor/monitor.go:20" {
		t.Fatalf("source = %q", snapshot.Entries[0].Source)
	}
	if len(snapshot.Notes) != 1 || !strings.Contains(snapshot.Notes[0], "1 行无法解析") {
		t.Fatalf("notes = %+v", snapshot.Notes)
	}
	if snapshot.Entries[0].Seq != 2 || snapshot.Entries[1].Seq != 1 {
		t.Fatalf("seq = %d/%d, want 2/1", snapshot.Entries[0].Seq, snapshot.Entries[1].Seq)
	}
}

// TestReadLogFileTruncatedTail asserts a file larger than the read window is
// tail-read and the leading partial line is dropped.
func TestReadLogFileTruncatedTail(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "praxis.jsonl")
	var builder strings.Builder
	for index := 0; index < 40; index++ {
		builder.WriteString(`{"time":"2026-11-14T06:00:00Z","level":"INFO","msg":"row-`)
		builder.WriteString(strconv.Itoa(index))
		builder.WriteString(`"}`)
		builder.WriteString("\n")
	}
	if err := os.WriteFile(filePath, []byte(builder.String()), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	snapshot, err := ReadLogFile(filePath, 256)
	if err != nil {
		t.Fatalf("ReadLogFile() error = %v", err)
	}
	if !snapshot.Info.Truncated {
		t.Fatalf("truncated = false, want true")
	}
	if !strings.Contains(snapshot.Info.Note, "仅读取最近") {
		t.Fatalf("note = %q", snapshot.Info.Note)
	}
	if len(snapshot.Entries) == 0 {
		t.Fatal("no entries parsed from the tail window")
	}
	// Every parsed row is complete: the read window's partial first line was
	// dropped, so all messages have the row- prefix suffix.
	for _, entry := range snapshot.Entries {
		if !strings.HasPrefix(entry.Message, "row-") {
			t.Fatalf("partial line parsed as entry: %+v", entry)
		}
	}
	if snapshot.Entries[0].Seq != int64(len(snapshot.Entries)) {
		t.Fatalf("first seq = %d, want %d", snapshot.Entries[0].Seq, len(snapshot.Entries))
	}
}

// TestReadLogFileUnconfigured asserts file logging disabled is a normal 200
// path: configured=false with a Chinese note, never an error.
func TestReadLogFileUnconfigured(t *testing.T) {
	snapshot, err := ReadLogFile("", MaxTailBytes)
	if err != nil {
		t.Fatalf("ReadLogFile(\"\") error = %v", err)
	}
	if snapshot.Info.Configured || snapshot.Info.Path != "" {
		t.Fatalf("file info = %+v", snapshot.Info)
	}
	if !strings.Contains(snapshot.Info.Note, FileEnvVar) {
		t.Fatalf("note = %q, want a %s hint", snapshot.Info.Note, FileEnvVar)
	}
	if len(snapshot.Entries) != 0 {
		t.Fatalf("entries = %+v, want empty", snapshot.Entries)
	}
	missing, err := ReadLogFile(filepath.Join(t.TempDir(), "absent.log"), MaxTailBytes)
	if err != nil {
		t.Fatalf("ReadLogFile(missing) error = %v", err)
	}
	if !missing.Info.Configured || !strings.Contains(missing.Info.Note, "不存在") {
		t.Fatalf("missing file info = %+v", missing.Info)
	}
}

// TestBufferSizeFromEnv asserts the RAG_LOG_BUFFER_SIZE bounds fail fast.
func TestBufferSizeFromEnv(t *testing.T) {
	values := map[string]string{}
	getenv := func(name string) string { return values[name] }
	size, err := bufferSizeFromEnv(getenv)
	if err != nil || size != DefaultBufferSize {
		t.Fatalf("default size = %d, err = %v", size, err)
	}
	values[BufferSizeEnvVar] = " 250 "
	size, err = bufferSizeFromEnv(getenv)
	if err != nil || size != 250 {
		t.Fatalf("configured size = %d, err = %v", size, err)
	}
	for _, invalid := range []string{"abc", "99", "20001", "0"} {
		values[BufferSizeEnvVar] = invalid
		if _, err := bufferSizeFromEnv(getenv); err == nil {
			t.Fatalf("bufferSizeFromEnv(%q) = nil error, want a configuration error", invalid)
		}
	}
}

// TestSetupInstallsCaptureAndFile asserts Setup wires the ring, the JSON Lines
// file and the accessors the monitoring service reads.
func TestSetupInstallsCaptureAndFile(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })

	filePath := filepath.Join(t.TempDir(), "praxis-logs.jsonl")
	t.Setenv(FileEnvVar, filePath)
	t.Setenv(BufferSizeEnvVar, "150")
	t.Setenv(FormatEnvVar, "json")
	t.Setenv(LevelEnvVar, "info")
	t.Setenv(DebugTraceEnvVar, "")

	logger, err := SetupWithWriter(io.Discard)
	if err != nil {
		t.Fatalf("SetupWithWriter() error = %v", err)
	}
	t.Cleanup(func() { _ = CloseLogFile() })
	logger.Info("hello monitor", "component", ComponentRedis)
	logger.Debug("dropped by level", "component", ComponentRedis)

	buffer := DefaultBuffer()
	if buffer == nil {
		t.Fatal("DefaultBuffer() = nil after Setup")
	}
	if buffer.Capacity() != 150 {
		t.Fatalf("buffer capacity = %d, want 150", buffer.Capacity())
	}
	if buffer.Len() != 1 {
		t.Fatalf("buffer len = %d, want 1 (debug is below the configured level)", buffer.Len())
	}
	entry := buffer.Snapshot()[0]
	if entry.Message != "hello monitor" || entry.Component != ComponentRedis {
		t.Fatalf("captured entry = %+v", entry)
	}
	absolute, err := filepath.Abs(filePath)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	if got := LogFilePath(); got != absolute {
		t.Fatalf("LogFilePath() = %q, want %q", got, absolute)
	}

	// The same record must be appended as one JSON Lines row with source info.
	fileSnapshot, err := ReadLogFile(LogFilePath(), MaxTailBytes)
	if err != nil {
		t.Fatalf("ReadLogFile() error = %v", err)
	}
	if len(fileSnapshot.Entries) != 1 {
		t.Fatalf("file entries = %+v", fileSnapshot.Entries)
	}
	fileEntry := fileSnapshot.Entries[0]
	if fileEntry.Message != "hello monitor" || fileEntry.Component != ComponentRedis {
		t.Fatalf("file entry = %+v", fileEntry)
	}
	if fileEntry.Source == "" || !strings.Contains(fileEntry.Source, "logs_test.go:") {
		t.Fatalf("file entry source = %q, want AddSource information", fileEntry.Source)
	}
}

// TestSetupRejectsInvalidConfiguration asserts the two new environment
// variables fail fast with Chinese configuration errors.
func TestSetupRejectsInvalidConfiguration(t *testing.T) {
	previous := slog.Default()
	t.Cleanup(func() { slog.SetDefault(previous) })
	t.Setenv(LevelEnvVar, "")
	t.Setenv(FormatEnvVar, "")
	t.Setenv(DebugTraceEnvVar, "")
	t.Setenv(FileEnvVar, "")
	t.Setenv(BufferSizeEnvVar, "10")
	if _, err := SetupWithWriter(io.Discard); err == nil ||
		!strings.Contains(err.Error(), BufferSizeEnvVar) {
		t.Fatalf("SetupWithWriter() error = %v, want a %s configuration error", err, BufferSizeEnvVar)
	}

	t.Setenv(BufferSizeEnvVar, "200")
	t.Setenv(FileEnvVar, filepath.Join(t.TempDir(), "missing-directory", "logs.jsonl"))
	if _, err := SetupWithWriter(io.Discard); err == nil ||
		!strings.Contains(err.Error(), FileEnvVar) {
		t.Fatalf("SetupWithWriter() error = %v, want a %s configuration error", err, FileEnvVar)
	}
}

// TestNormalizeLevelFilter asserts the Console level aliases map to the four
// canonical minimum levels.
func TestNormalizeLevelFilter(t *testing.T) {
	for value, want := range map[string]string{
		"debug": "debug", "INFO": "info", "warn": "warn", "warning": "warn", "error": "error",
	} {
		got, ok := NormalizeLevelFilter(value)
		if !ok || got != want {
			t.Fatalf("NormalizeLevelFilter(%q) = %q, %v; want %q", value, got, ok, want)
		}
	}
	for _, invalid := range []string{"", "verbose", "WARN+1"} {
		if _, ok := NormalizeLevelFilter(invalid); ok {
			t.Fatalf("NormalizeLevelFilter(%q) = ok, want invalid", invalid)
		}
	}
}

// TestQueryEntriesEmpty asserts an empty buffer serializes arrays instead of
// null in the response.
func TestQueryEntriesEmpty(t *testing.T) {
	page := QueryEntries(nil, LogFilter{})
	if page.Entries == nil || page.Components == nil || page.Total != 0 || page.HasMore {
		t.Fatalf("empty page = %+v", page)
	}
}

// TestReadLogFilePermissionError documents that a read error other than a
// missing file is reported to the caller instead of silently rendering an
// empty page.
func TestReadLogFilePermissionError(t *testing.T) {
	directory := t.TempDir()
	filePath := filepath.Join(directory, "unreadable.jsonl")
	if err := os.WriteFile(filePath, []byte("x\n"), 0o000); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := ReadLogFile(filePath, MaxTailBytes)
	if err == nil {
		t.Skip("platform keeps the file readable; the error path is covered elsewhere")
	}
	if !strings.Contains(err.Error(), "日志文件") {
		t.Fatalf("error = %v, want a Chinese file-read diagnostic", err)
	}
}
