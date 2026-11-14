package sysinfo

import (
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestSamplerDeltaAndWireShape pins the delta-sampling contract: the first
// Sample only captures the baseline (usage and network rates are null), the
// second one derives them, and the JSON object exposes the documented keys.
func TestSamplerDeltaAndWireShape(t *testing.T) {
	sampler, err := New()
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	first, err := sampler.Sample()
	if err != nil {
		t.Fatalf("first Sample() error = %v", err)
	}
	if first.CPU.UsagePercent != nil {
		t.Fatalf("first cpu.usage_percent = %v, want null", *first.CPU.UsagePercent)
	}
	if first.Network.RxBytesPerSecond != nil || first.Network.TxBytesPerSecond != nil {
		t.Fatalf("first network rates = %v/%v, want null",
			first.Network.RxBytesPerSecond, first.Network.TxBytesPerSecond)
	}
	if first.WindowSeconds != 0 {
		t.Fatalf("first window_seconds = %v, want 0", first.WindowSeconds)
	}
	if first.Memory.TotalBytes <= 0 {
		t.Fatalf("memory.total_bytes = %d, want > 0", first.Memory.TotalBytes)
	}
	if first.CPU.Cores <= 0 {
		t.Fatalf("cpu.cores = %d, want > 0", first.CPU.Cores)
	}
	if first.Process.PID <= 0 || first.Process.Goroutines <= 0 {
		t.Fatalf("process = %+v", first.Process)
	}
	if first.Process.HeapAllocBytes <= 0 {
		t.Fatalf("process.heap_alloc_bytes = %d, want > 0", first.Process.HeapAllocBytes)
	}
	if _, err := time.Parse(time.RFC3339, first.CollectedAt); err != nil {
		t.Fatalf("collected_at = %q is not RFC3339: %v", first.CollectedAt, err)
	}
	if first.CPU.Model == "" && runtime.GOOS == "windows" {
		t.Fatalf("cpu.model empty on Windows")
	}
	// Windows has no load average: the values stay null and the note explains it.
	if runtime.GOOS == "windows" {
		if first.CPU.Load1 != nil || first.CPU.Load5 != nil || first.CPU.Load15 != nil {
			t.Fatalf("windows load values = %v/%v/%v, want null",
				first.CPU.Load1, first.CPU.Load5, first.CPU.Load15)
		}
		if !strings.Contains(strings.Join(first.Notes, " "), "load average") {
			t.Fatalf("windows notes = %v, want a load-average note", first.Notes)
		}
	}

	// The JSON contract must contain the system object keys with null rates.
	encoded, err := json.Marshal(first)
	if err != nil {
		t.Fatalf("json.Marshal(Snapshot) error = %v", err)
	}
	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatalf("json.Unmarshal(Snapshot) error = %v", err)
	}
	for _, key := range []string{"collected_at", "window_seconds", "notes", "cpu", "memory", "network", "process"} {
		if _, present := document[key]; !present {
			t.Fatalf("snapshot misses key %q: %v", key, document)
		}
	}
	cpuObject, _ := document["cpu"].(map[string]any)
	for _, key := range []string{"usage_percent", "cores", "model", "load1", "load5", "load15"} {
		if _, present := cpuObject[key]; !present {
			t.Fatalf("cpu misses key %q: %v", key, cpuObject)
		}
	}
	if cpuObject["usage_percent"] != nil {
		t.Fatalf("cpu.usage_percent = %v on the first sample, want null", cpuObject["usage_percent"])
	}
	networkObject, _ := document["network"].(map[string]any)
	for _, key := range []string{"rx_bytes_per_second", "tx_bytes_per_second",
		"rx_bytes_total", "tx_bytes_total", "interfaces"} {
		if _, present := networkObject[key]; !present {
			t.Fatalf("network misses key %q: %v", key, networkObject)
		}
	}

	time.Sleep(25 * time.Millisecond)

	second, err := sampler.Sample()
	if err != nil {
		t.Fatalf("second Sample() error = %v", err)
	}
	if second.CPU.UsagePercent == nil {
		t.Fatal("second cpu.usage_percent = null, want a derived value")
	}
	if *second.CPU.UsagePercent < 0 || *second.CPU.UsagePercent > 100 {
		t.Fatalf("second cpu.usage_percent = %v, want 0..100", *second.CPU.UsagePercent)
	}
	if second.Network.RxBytesPerSecond == nil || second.Network.TxBytesPerSecond == nil {
		t.Fatalf("second network rates = %v/%v, want derived values",
			second.Network.RxBytesPerSecond, second.Network.TxBytesPerSecond)
	}
	if second.WindowSeconds <= 0 {
		t.Fatalf("second window_seconds = %v, want > 0", second.WindowSeconds)
	}
	if len(second.Network.Interfaces) > interfaceMax {
		t.Fatalf("network.interfaces = %d rows, want <= %d", len(second.Network.Interfaces), interfaceMax)
	}
	for index := 1; index < len(second.Network.Interfaces); index++ {
		previous := second.Network.Interfaces[index-1]
		current := second.Network.Interfaces[index]
		if previous.RxBytesTotal+previous.TxBytesTotal < current.RxBytesTotal+current.TxBytesTotal {
			t.Fatalf("network.interfaces not sorted by total traffic: %+v", second.Network.Interfaces)
		}
	}
}

// TestCPUUsageRejectsResetCounters asserts a reset or missing delta yields null
// instead of a fabricated 0% or a value outside the contract range.
func TestCPUUsageRejectsResetCounters(t *testing.T) {
	for name, values := range map[string]struct {
		previousTotal, previousIdle, currentTotal, currentIdle float64
		wantNil                                                bool
	}{
		"normal":        {100, 60, 200, 120, false},
		"zero elapsed":  {100, 60, 100, 60, true},
		"reset counter": {200, 120, 100, 60, true},
		"idle overflow": {100, 60, 200, 190, true},
	} {
		t.Run(name, func(t *testing.T) {
			usage := cpuUsage(values.previousTotal, values.previousIdle, values.currentTotal, values.currentIdle)
			if values.wantNil {
				if usage != nil {
					t.Fatalf("cpuUsage() = %v, want null", *usage)
				}
				return
			}
			if usage == nil {
				t.Fatal("cpuUsage() = null, want a value")
			}
			if *usage != 40.0 {
				t.Fatalf("cpuUsage() = %v, want 40", *usage)
			}
		})
	}
}
