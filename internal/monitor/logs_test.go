package monitor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/sysinfo"
)

// fakeSystemSampler is the injectable host-resource surface.
type fakeSystemSampler struct {
	snapshot *sysinfo.Snapshot
	err      error
}

func (f fakeSystemSampler) Sample() (*sysinfo.Snapshot, error) { return f.snapshot, f.err }

// sampleSystemSnapshot is a fully populated system view.
func sampleSystemSnapshot() *sysinfo.Snapshot {
	usage := 12.3
	load1, load5, load15 := 1.2, 1.1, 0.9
	rx, tx := 10240.0, 20480.0
	networkRx, networkTx := 10240.0, 20480.0
	return &sysinfo.Snapshot{
		CollectedAt:   "2026-11-14T06:00:00Z",
		WindowSeconds: 15.0,
		Notes:         []string{},
		CPU: sysinfo.CPUSnapshot{
			UsagePercent: &usage, Cores: 16, Model: "Test CPU",
			Load1: &load1, Load5: &load5, Load15: &load15,
		},
		Memory: sysinfo.MemorySnapshot{
			TotalBytes: 34359738368, UsedBytes: 20552089600, AvailableBytes: 13807648768,
			UsedPercent: 59.8, SwapTotalBytes: 4294967296, SwapUsedBytes: 0, SwapUsedPercent: 0,
		},
		Network: sysinfo.NetworkSnapshot{
			RxBytesPerSecond: &networkRx, TxBytesPerSecond: &networkTx,
			RxBytesTotal: 123456789, TxBytesTotal: 987654321,
			Interfaces: []sysinfo.InterfaceSnapshot{{
				Name: "eth0", RxBytesPerSecond: &rx, TxBytesPerSecond: &tx,
				RxBytesTotal: 123456789, TxBytesTotal: 987654321,
			}},
		},
		Process: sysinfo.ProcessSnapshot{
			PID: 1234, RSSBytes: 167772160, VMSBytes: 1073741824,
			Goroutines: 42, HeapAllocBytes: 33554432,
		},
	}
}

// TestMonitorOverviewSystemObject asserts the new top-level "system" object is
// the typed snapshot and does not participate in the health verdict.
func TestMonitorOverviewSystemObject(t *testing.T) {
	options := healthyOptions(t)
	baseline := New(options).MonitorOverview(context.Background())
	if _, present := baseline["system"]; !present {
		t.Fatalf("overview misses the system key: %v", baseline)
	}
	if baseline["system"] != nil {
		t.Fatalf("system = %v, want null when the sampler is not wired", baseline["system"])
	}

	options.System = fakeSystemSampler{snapshot: sampleSystemSnapshot()}
	overview := New(options).MonitorOverview(context.Background())
	document := decodeOverview(t, overview)
	system, ok := document["system"].(map[string]any)
	if !ok {
		t.Fatalf("system has type %T", document["system"])
	}
	for _, key := range []string{"collected_at", "window_seconds", "notes", "cpu", "memory", "network", "process"} {
		if _, present := system[key]; !present {
			t.Fatalf("system misses key %q: %v", key, system)
		}
	}
	cpu, _ := system["cpu"].(map[string]any)
	if cpu["usage_percent"] != 12.3 || cpu["cores"] != 16.0 || cpu["model"] != "Test CPU" {
		t.Fatalf("system.cpu = %v", cpu)
	}
	if cpu["load1"] != 1.2 || cpu["load15"] != 0.9 {
		t.Fatalf("system.cpu load = %v", cpu)
	}
	memory, _ := system["memory"].(map[string]any)
	if memory["total_bytes"] != 34359738368.0 || memory["used_percent"] != 59.8 {
		t.Fatalf("system.memory = %v", memory)
	}
	network, _ := system["network"].(map[string]any)
	if network["rx_bytes_per_second"] != 10240.0 {
		t.Fatalf("system.network = %v", network)
	}
	interfaces, _ := network["interfaces"].([]any)
	if len(interfaces) != 1 {
		t.Fatalf("system.network.interfaces = %v", network["interfaces"])
	}
	process, _ := system["process"].(map[string]any)
	if process["pid"] != 1234.0 || process["goroutines"] != 42.0 {
		t.Fatalf("system.process = %v", process)
	}
	if overview["status"] != baseline["status"] {
		t.Fatalf("system changed the health verdict: %v → %v", baseline["status"], overview["status"])
	}
	if errorsList, _ := overview["errors"].([]string); len(errorsList) != 0 {
		t.Fatalf("errors = %v, want empty", overview["errors"])
	}
}

// TestMonitorOverviewSystemFailure asserts a sampling failure nulls "system",
// appends the Chinese reason to "errors" and leaves the health verdict alone.
func TestMonitorOverviewSystemFailure(t *testing.T) {
	options := healthyOptions(t)
	baseline := New(options).MonitorOverview(context.Background())
	options.System = fakeSystemSampler{err: errors.New("cpu counter unavailable")}
	overview := New(options).MonitorOverview(context.Background())

	if overview["system"] != nil {
		t.Fatalf("system = %v, want null", overview["system"])
	}
	errorsList, ok := overview["errors"].([]string)
	if !ok || len(errorsList) != 1 {
		t.Fatalf("errors = %v", overview["errors"])
	}
	if !strings.Contains(errorsList[0], "系统资源采集失败") ||
		!strings.Contains(errorsList[0], "cpu counter unavailable") {
		t.Fatalf("errors[0] = %q", errorsList[0])
	}
	if overview["status"] != baseline["status"] || overview["ok"] != baseline["ok"] {
		t.Fatalf("sampling failure changed the health verdict: %v / %v", overview["status"], overview["ok"])
	}
}

// logsFixtureBuffer fills a ring with three rows across two components.
func logsFixtureBuffer(t *testing.T) *logging.Buffer {
	t.Helper()
	buffer := logging.NewBuffer(20)
	for _, entry := range []logging.Entry{
		{Time: "2026-11-14T06:00:00Z", Level: "INFO", Component: logging.ComponentMySQL,
			ComponentLabel: "MySQL", Message: "mysql ready"},
		{Time: "2026-11-14T06:00:01Z", Level: "WARN", Component: logging.ComponentRedis,
			ComponentLabel: "Redis", Message: "redis slow", Attrs: map[string]string{"request_id": "abc"}},
		{Time: "2026-11-14T06:00:02Z", Level: "ERROR", Component: logging.ComponentMySQL,
			ComponentLabel: "MySQL", Message: "mysql down", Source: "internal/state/store.go:10"},
	} {
		buffer.Add(entry)
	}
	return buffer
}

// TestMonitorLogsMemory asserts the default memory source, newest-first order,
// component badges, filtering and pagination.
func TestMonitorLogsMemory(t *testing.T) {
	service := New(Options{Logs: logsFixtureBuffer(t)})
	view, err := service.MonitorLogs(context.Background(), LogQuery{})
	if err != nil {
		t.Fatalf("MonitorLogs() error = %v", err)
	}
	if view.Source != "memory" || !view.Available || view.GeneratedAt == "" {
		t.Fatalf("view = %+v", view)
	}
	if len(view.Entries) != 3 || view.Total != 3 || view.HasMore {
		t.Fatalf("view = %+v", view)
	}
	if view.Entries[0].Message != "mysql down" || view.Entries[2].Message != "mysql ready" {
		t.Fatalf("entry order = %+v", view.Entries)
	}
	if view.File.Configured || !strings.Contains(view.File.Note, logging.FileEnvVar) {
		t.Fatalf("file view = %+v", view.File)
	}
	if len(view.Components) != 2 || view.Components[0].ID != logging.ComponentMySQL ||
		view.Components[0].Count != 2 || view.Components[1].ID != logging.ComponentRedis {
		t.Fatalf("components = %+v", view.Components)
	}

	filtered, err := service.MonitorLogs(context.Background(), LogQuery{
		Component: logging.ComponentMySQL, Level: "warn", Limit: 1,
	})
	if err != nil {
		t.Fatalf("MonitorLogs(filter) error = %v", err)
	}
	if filtered.Total != 1 || len(filtered.Entries) != 1 || filtered.HasMore ||
		filtered.Entries[0].Level != "ERROR" {
		t.Fatalf("filtered view = %+v", filtered)
	}
	// Tab badges ignore the component/level filters.
	if len(filtered.Components) != 2 {
		t.Fatalf("filtered components = %+v", filtered.Components)
	}

	keyword, err := service.MonitorLogs(context.Background(), LogQuery{Query: "redis"})
	if err != nil || keyword.Total != 1 || keyword.Entries[0].Component != logging.ComponentRedis {
		t.Fatalf("keyword view = %+v, err = %v", keyword, err)
	}
	attrKeyword, err := service.MonitorLogs(context.Background(), LogQuery{Query: "abc"})
	if err != nil || attrKeyword.Total != 1 {
		t.Fatalf("attr keyword view = %+v, err = %v", attrKeyword, err)
	}

	second, err := service.MonitorLogs(context.Background(), LogQuery{Limit: 1, Offset: 1})
	if err != nil || second.Total != 3 || len(second.Entries) != 1 || !second.HasMore ||
		second.Entries[0].Level != "WARN" {
		t.Fatalf("offset view = %+v, err = %v", second, err)
	}
}

// TestMonitorLogsMemoryUnavailable asserts a missing ring is a normal 200 view
// with available=false instead of an error.
func TestMonitorLogsMemoryUnavailable(t *testing.T) {
	view, err := New(Options{}).MonitorLogs(context.Background(), LogQuery{})
	if err != nil {
		t.Fatalf("MonitorLogs() error = %v", err)
	}
	if view.Available || len(view.Entries) != 0 {
		t.Fatalf("view = %+v", view)
	}
	if len(view.Notes) != 1 || !strings.Contains(view.Notes[0], "日志缓冲未装配") {
		t.Fatalf("notes = %+v", view.Notes)
	}
}

// TestMonitorLogsFile asserts the file source reads the bounded tail and keeps
// the file metadata in the response.
func TestMonitorLogsFile(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "praxis.jsonl")
	content := `{"time":"2026-11-14T06:00:00Z","level":"INFO","msg":"worker started","source":{"function":"x","file":"/repo/cmd/rag-document-worker/main.go","line":10}}` + "\n" +
		`{"time":"2026-11-14T06:00:01Z","level":"ERROR","msg":"worker failed","component":"worker","source":{"function":"x","file":"/repo/cmd/rag-document-worker/main.go","line":20}}` + "\n"
	if err := os.WriteFile(filePath, []byte(content), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := New(Options{LogFilePath: filePath})
	view, err := service.MonitorLogs(context.Background(), LogQuery{Source: "file"})
	if err != nil {
		t.Fatalf("MonitorLogs() error = %v", err)
	}
	if !view.Available || !view.File.Configured || view.File.Path != filePath {
		t.Fatalf("view = %+v file = %+v", view, view.File)
	}
	if view.File.SizeBytes != int64(len(content)) || view.File.Truncated {
		t.Fatalf("file = %+v", view.File)
	}
	if len(view.Entries) != 2 || view.Entries[0].Message != "worker failed" {
		t.Fatalf("entries = %+v", view.Entries)
	}
	if view.Entries[0].Component != logging.ComponentWorker ||
		view.Entries[0].ComponentLabel != "文档 Worker" {
		t.Fatalf("file entry component = %+v", view.Entries[0])
	}
	if view.Entries[1].Component != logging.ComponentWorker {
		t.Fatalf("source-path component = %+v", view.Entries[1])
	}
	filtered, err := service.MonitorLogs(context.Background(), LogQuery{
		Source: "file", Level: "error", Limit: 1,
	})
	if err != nil || filtered.Total != 1 || filtered.Entries[0].Level != "ERROR" {
		t.Fatalf("filtered file view = %+v, err = %v", filtered, err)
	}
}

// TestMonitorLogsFileUnavailable asserts the two file-unavailable cases stay a
// 200 view with available=false and a Chinese note.
func TestMonitorLogsFileUnavailable(t *testing.T) {
	unconfigured, err := New(Options{}).MonitorLogs(context.Background(), LogQuery{Source: "file"})
	if err != nil {
		t.Fatalf("unconfigured error = %v", err)
	}
	if unconfigured.Available || unconfigured.File.Configured || len(unconfigured.Entries) != 0 {
		t.Fatalf("unconfigured view = %+v", unconfigured)
	}
	if !strings.Contains(unconfigured.File.Note, logging.FileEnvVar) {
		t.Fatalf("unconfigured note = %q", unconfigured.File.Note)
	}

	missingPath := filepath.Join(t.TempDir(), "absent.jsonl")
	missing, err := New(Options{LogFilePath: missingPath}).MonitorLogs(context.Background(), LogQuery{Source: "file"})
	if err != nil {
		t.Fatalf("missing error = %v", err)
	}
	if missing.Available || !missing.File.Configured || !strings.Contains(missing.File.Note, "不存在") {
		t.Fatalf("missing view = %+v file = %+v", missing, missing.File)
	}
}

// TestNormalizeLogQuery asserts the single validation point: every invalid
// value wraps the sentinel with the Chinese detail, and valid values normalize.
func TestNormalizeLogQuery(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		query  LogQuery
		detail string
	}{
		{"unknown source", LogQuery{Source: "socket"}, "未知日志来源: socket"},
		{"unknown component", LogQuery{Component: "nope"}, "未知日志组件: nope"},
		{"unknown level", LogQuery{Level: "verbose"}, "未知日志级别: verbose"},
		{"query too long", LogQuery{Query: strings.Repeat("日", logging.MaxLogQueryRunes+1)}, "查询关键字过长"},
		{"negative offset", LogQuery{Offset: -1}, "偏移量不能为负数"},
		{"offset too large", LogQuery{Offset: logging.MaxLogOffset + 1}, "偏移量超出范围"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			_, _, err := normalizeLogQuery(testCase.query)
			if err == nil {
				t.Fatalf("normalizeLogQuery(%+v) = nil error", testCase.query)
			}
			if !errors.Is(err, ErrInvalidLogQuery) {
				t.Fatalf("error = %v, want the ErrInvalidLogQuery sentinel", err)
			}
			if !strings.Contains(err.Error(), testCase.detail) {
				t.Fatalf("error = %q, want detail %q", err.Error(), testCase.detail)
			}
		})
	}

	normalized, filter, err := normalizeLogQuery(LogQuery{
		Component: " MySQL ", Level: "WARNING", Query: "  down  ", Limit: 100000, Offset: 3,
	})
	if err != nil {
		t.Fatalf("normalizeLogQuery(valid) error = %v", err)
	}
	if normalized.Source != "memory" || normalized.Component != logging.ComponentMySQL ||
		normalized.Level != "warn" || normalized.Query != "down" ||
		normalized.Limit != logging.MaxLogLimit || normalized.Offset != 3 {
		t.Fatalf("normalized = %+v", normalized)
	}
	if filter.Component != logging.ComponentMySQL || filter.Level != "warn" ||
		filter.Limit != logging.MaxLogLimit || filter.Offset != 3 || filter.Query != "down" {
		t.Fatalf("filter = %+v", filter)
	}

	// limit below the minimum is clamped to the default instead of rejected.
	clamped, _, err := normalizeLogQuery(LogQuery{Limit: -5})
	if err != nil || clamped.Limit != logging.DefaultLogLimit {
		t.Fatalf("clamped = %+v, err = %v", clamped, err)
	}
	offsetLimit, _, err := normalizeLogQuery(LogQuery{Offset: logging.MaxLogOffset})
	if err != nil || offsetLimit.Offset != logging.MaxLogOffset {
		t.Fatalf("max offset = %+v, err = %v", offsetLimit, err)
	}
}

// TestMonitorLogsContextCancelled asserts an already-cancelled request does not
// read a log source.
func TestMonitorLogsContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := New(Options{Logs: logsFixtureBuffer(t)}).MonitorLogs(ctx, LogQuery{}); err == nil {
		t.Fatal("MonitorLogs(cancelled ctx) = nil error")
	}
}

// TestMonitorLogsFileTruncatedNote asserts the bounded tail metadata reaches the
// Console file object and notes.
func TestMonitorLogsFileTruncatedNote(t *testing.T) {
	filePath := filepath.Join(t.TempDir(), "praxis.jsonl")
	padding := strings.Repeat("x", 900)
	var builder strings.Builder
	for index := 0; index < 600; index++ {
		builder.WriteString(`{"time":"2026-11-14T06:00:00Z","level":"INFO","msg":"row-`)
		builder.WriteString(padding)
		builder.WriteString(`"}`)
		builder.WriteString("\n")
	}
	if err := os.WriteFile(filePath, []byte(builder.String()), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	service := New(Options{LogFilePath: filePath})
	view, err := service.MonitorLogs(context.Background(), LogQuery{Source: "file", Limit: 500})
	if err != nil {
		t.Fatalf("MonitorLogs() error = %v", err)
	}
	if !view.File.Truncated {
		t.Fatalf("file = %+v, want truncated", view.File)
	}
	if !strings.Contains(view.File.Note, "仅读取最近") {
		t.Fatalf("file note = %q", view.File.Note)
	}
	if !view.Available || view.Total == 0 {
		t.Fatalf("view = %+v", view)
	}
}
