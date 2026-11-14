// Package sysinfo samples host CPU, memory, network and process resources for
// the Console monitoring page. It is read-only: gopsutil reads the operating
// system counters and never writes state.
//
// CPU usage and network rates use the delta between two samples, so the very
// first Sample after New reports null for those values (the Console renders
// 「—」 and the caller decides when the second sample is available). Windows has
// no load average, so load1/5/15 stay null and a note explains why.
package sysinfo

import (
	"fmt"
	"math"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/shirou/gopsutil/v4/cpu"
	"github.com/shirou/gopsutil/v4/load"
	"github.com/shirou/gopsutil/v4/mem"
	gopsutilnet "github.com/shirou/gopsutil/v4/net"
	"github.com/shirou/gopsutil/v4/process"
)

// Snapshot is the JSON contract of the Console "system" object. All byte
// counters are int64; percentages and rates are float64 with one decimal.
// Nil pointers mean "not available yet" and serialize as null, never as a
// fabricated zero.
type Snapshot struct {
	CollectedAt   string          `json:"collected_at"`
	WindowSeconds float64         `json:"window_seconds"`
	Notes         []string        `json:"notes"`
	CPU           CPUSnapshot     `json:"cpu"`
	Memory        MemorySnapshot  `json:"memory"`
	Network       NetworkSnapshot `json:"network"`
	Process       ProcessSnapshot `json:"process"`
}

// CPUSnapshot is the host CPU state. UsagePercent and the load values are null
// when no difference sample exists or the platform does not support them.
type CPUSnapshot struct {
	UsagePercent *float64 `json:"usage_percent"`
	Cores        int      `json:"cores"`
	Model        string   `json:"model"`
	Load1        *float64 `json:"load1"`
	Load5        *float64 `json:"load5"`
	Load15       *float64 `json:"load15"`
}

// MemorySnapshot is the host physical memory and swap state.
type MemorySnapshot struct {
	TotalBytes      int64   `json:"total_bytes"`
	UsedBytes       int64   `json:"used_bytes"`
	AvailableBytes  int64   `json:"available_bytes"`
	UsedPercent     float64 `json:"used_percent"`
	SwapTotalBytes  int64   `json:"swap_total_bytes"`
	SwapUsedBytes   int64   `json:"swap_used_bytes"`
	SwapUsedPercent float64 `json:"swap_used_percent"`
}

// NetworkSnapshot is the aggregate host traffic plus the busiest interfaces.
// Rates are null until the second sample; totals always come from the current
// counters.
type NetworkSnapshot struct {
	RxBytesPerSecond *float64            `json:"rx_bytes_per_second"`
	TxBytesPerSecond *float64            `json:"tx_bytes_per_second"`
	RxBytesTotal     int64               `json:"rx_bytes_total"`
	TxBytesTotal     int64               `json:"tx_bytes_total"`
	Interfaces       []InterfaceSnapshot `json:"interfaces"`
}

// InterfaceSnapshot is one network interface's counters and rates.
type InterfaceSnapshot struct {
	Name             string   `json:"name"`
	RxBytesPerSecond *float64 `json:"rx_bytes_per_second"`
	TxBytesPerSecond *float64 `json:"tx_bytes_per_second"`
	RxBytesTotal     int64    `json:"rx_bytes_total"`
	TxBytesTotal     int64    `json:"tx_bytes_total"`
}

// ProcessSnapshot is the current process's own resource usage. RSS/VMS are 0
// when the operating system refuses the process memory read (the snapshot then
// carries a note); the Go values always come from the runtime.
type ProcessSnapshot struct {
	PID            int   `json:"pid"`
	RSSBytes       int64 `json:"rss_bytes"`
	VMSBytes       int64 `json:"vms_bytes"`
	Goroutines     int   `json:"goroutines"`
	HeapAllocBytes int64 `json:"heap_alloc_bytes"`
}

// interfaceMax bounds the per-interface rows the Console renders: the busiest
// eight by total traffic are enough to spot a saturated link.
const interfaceMax = 8

// netCounter is one interface's cumulative byte counters captured at a sample.
type netCounter struct {
	rx int64
	tx int64
}

// Sampler keeps the previous CPU/network observation so rates can be derived
// from the delta. It is safe for concurrent use; sampling is serialized.
type Sampler struct {
	mu    sync.Mutex
	cores int
	model string
	// staticNotes explain values that can never be collected in this process.
	staticNotes []string

	initialized    bool
	lastSampleTime time.Time
	lastCPUTotal   float64
	lastCPUIdle    float64
	lastNet        map[string]netCounter

	// Windows reads the page-file counters through WMI, whose first query can
	// take over a second. New warms that path in the background so a monitoring
	// request never blocks on it; swapReady/swapErr publish the warmup outcome.
	swapMu    sync.Mutex
	swapReady bool
	swapErr   error
}

// New builds a sampler and caches the values that are expensive to read (logical
// core count and CPU model). The CPU/network baseline is captured by the first
// Sample call, so the first snapshot reports null usage and rates.
func New() (*Sampler, error) {
	sampler := &Sampler{lastNet: make(map[string]netCounter)}
	cores, err := cpu.Counts(true)
	if err != nil || cores <= 0 {
		cores = runtime.NumCPU()
	}
	sampler.cores = cores
	model, modelErr := cpuModel()
	if modelErr != nil {
		sampler.staticNotes = append(sampler.staticNotes, "CPU 型号读取失败："+shortError(modelErr))
	}
	sampler.model = model
	if runtime.GOOS == "windows" {
		sampler.staticNotes = append(sampler.staticNotes,
			"当前平台（Windows）没有 load average，load1/5/15 显示为未知")
		go sampler.warmSwap()
	}
	return sampler, nil
}

// warmSwap performs the first (slow on Windows) swap read off the request path.
func (s *Sampler) warmSwap() {
	_, err := mem.SwapMemory()
	s.swapMu.Lock()
	s.swapReady = true
	s.swapErr = err
	s.swapMu.Unlock()
}

// cpuModel reads the model name of the first physical processor. Some virtual
// environments leave it empty; that is reported as an empty string, not "未知",
// so the Console can choose its own placeholder.
func cpuModel() (string, error) {
	infos, err := cpu.Info()
	if err != nil {
		return "", err
	}
	for _, info := range infos {
		if name := strings.TrimSpace(info.ModelName); name != "" {
			return name, nil
		}
		if model := strings.TrimSpace(info.Model); model != "" {
			return model, nil
		}
	}
	return "", nil
}

// Sample reads one host snapshot. A failed essential read (CPU times, physical
// memory, network counters) returns an error so the caller can render system as
// null and report the reason; optional reads (swap, load, process memory) only
// add a note.
func (s *Sampler) Sample() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	snapshot := &Snapshot{
		CollectedAt: now.UTC().Format(time.RFC3339),
		Notes:       append([]string(nil), s.staticNotes...),
		CPU: CPUSnapshot{
			Cores: s.cores,
			Model: s.model,
		},
		Network: NetworkSnapshot{Interfaces: make([]InterfaceSnapshot, 0, interfaceMax)},
	}
	if snapshot.Notes == nil {
		snapshot.Notes = make([]string, 0)
	}

	times, err := cpu.Times(false)
	if err != nil || len(times) == 0 {
		return nil, fmt.Errorf("CPU 使用率读取失败：%s", shortError(err))
	}
	cpuTotal, cpuIdle := cpuTotals(times[0])

	load1, load5, load15 := readLoad(snapshot)
	snapshot.CPU.Load1, snapshot.CPU.Load5, snapshot.CPU.Load15 = load1, load5, load15

	virtual, err := mem.VirtualMemory()
	if err != nil || virtual == nil {
		return nil, fmt.Errorf("内存使用量读取失败：%s", shortError(err))
	}
	snapshot.Memory = MemorySnapshot{
		TotalBytes:     int64(virtual.Total),
		UsedBytes:      int64(virtual.Used),
		AvailableBytes: int64(virtual.Available),
		UsedPercent:    roundOneDecimal(virtual.UsedPercent),
	}
	s.fillSwap(snapshot)

	counters, err := gopsutilnet.IOCounters(true)
	if err != nil {
		return nil, fmt.Errorf("网络流量读取失败：%s", shortError(err))
	}
	current := s.buildNetwork(counters, &snapshot.Network)

	// Every essential read succeeded: commit the delta baseline for the next
	// sample and only then derive this sample's rates.
	if s.initialized {
		elapsed := now.Sub(s.lastSampleTime).Seconds()
		snapshot.WindowSeconds = roundThreeDecimals(elapsed)
		snapshot.CPU.UsagePercent = cpuUsage(s.lastCPUTotal, s.lastCPUIdle, cpuTotal, cpuIdle)
		applyNetworkRates(&snapshot.Network, s.lastNet, current, now.Sub(s.lastSampleTime))
	}
	s.initialized = true
	s.lastSampleTime = now
	s.lastCPUTotal = cpuTotal
	s.lastCPUIdle = cpuIdle
	s.lastNet = current

	s.fillProcess(snapshot)
	return snapshot, nil
}

// readLoad fills the load-average values. Windows has no load average (gopsutil
// synthesizes a processor-queue estimate), so the contract keeps the values null
// and New already recorded the note. A failed read on another platform is also
// null with a note instead of failing the whole snapshot.
func readLoad(snapshot *Snapshot) (*float64, *float64, *float64) {
	if runtime.GOOS == "windows" {
		return nil, nil, nil
	}
	average, err := load.Avg()
	if err != nil || average == nil {
		snapshot.Notes = append(snapshot.Notes, "系统负载读取失败："+shortError(err))
		return nil, nil, nil
	}
	load1 := roundOneDecimal(average.Load1)
	load5 := roundOneDecimal(average.Load5)
	load15 := roundOneDecimal(average.Load15)
	return &load1, &load5, &load15
}

// fillSwap records the swap/page-file counters. Windows reads them through WMI,
// so New warms that path in the background; until the warmup finishes the values
// stay zero with a note instead of blocking the request or inventing numbers.
// Other platforms read /proc/meminfo-equivalent state directly and cheaply.
func (s *Sampler) fillSwap(snapshot *Snapshot) {
	if runtime.GOOS == "windows" {
		s.swapMu.Lock()
		ready, warmErr := s.swapReady, s.swapErr
		s.swapMu.Unlock()
		if !ready {
			snapshot.Notes = append(snapshot.Notes, "交换分区统计正在初始化，稍后可用")
			return
		}
		if warmErr != nil {
			snapshot.Notes = append(snapshot.Notes, "交换分区读取失败："+shortError(warmErr))
			return
		}
	}
	swap, err := mem.SwapMemory()
	if err != nil {
		snapshot.Notes = append(snapshot.Notes, "交换分区读取失败："+shortError(err))
		return
	}
	if swap == nil {
		return
	}
	snapshot.Memory.SwapTotalBytes = int64(swap.Total)
	snapshot.Memory.SwapUsedBytes = int64(swap.Used)
	snapshot.Memory.SwapUsedPercent = roundOneDecimal(swap.UsedPercent)
}

// cpuTotals reduces a TimesStat to the counters the usage percentage needs.
// User/System already include guest time on Linux, so guest fields are skipped
// to avoid counting the same time twice. Iowait counts as idle: the CPU was not
// executing runnable work.
func cpuTotals(times cpu.TimesStat) (total, idle float64) {
	total = times.User + times.System + times.Nice + times.Iowait + times.Irq +
		times.Softirq + times.Steal + times.Idle
	idle = times.Idle + times.Iowait
	return total, idle
}

// cpuUsage derives the busy percentage from two cumulative CPU-time reads.
// A missing or reset counter (for example a narrower Windows counter fallback)
// yields nil instead of a misleading number.
func cpuUsage(previousTotal, previousIdle, currentTotal, currentIdle float64) *float64 {
	totalDelta := currentTotal - previousTotal
	idleDelta := currentIdle - previousIdle
	if totalDelta <= 0 || idleDelta < 0 || idleDelta > totalDelta {
		return nil
	}
	usage := (1 - idleDelta/totalDelta) * 100
	usage = clampPercent(usage)
	usage = roundOneDecimal(usage)
	return &usage
}

// buildNetwork renders per-interface totals (sorted by total traffic) and
// returns the counter map the next sample needs. Rates are filled later once a
// previous sample exists; the aggregate totals cover every interface, while the
// Console only renders the busiest interfaceMax rows.
func (s *Sampler) buildNetwork(counters []gopsutilnet.IOCountersStat, network *NetworkSnapshot) map[string]netCounter {
	current := make(map[string]netCounter, len(counters))
	interfaces := make([]InterfaceSnapshot, 0, len(counters))
	var rxTotal, txTotal int64
	for _, counter := range counters {
		name := strings.TrimSpace(counter.Name)
		if name == "" {
			name = "未知接口"
		}
		rx := int64(counter.BytesRecv)
		tx := int64(counter.BytesSent)
		current[name] = netCounter{rx: rx, tx: tx}
		rxTotal += rx
		txTotal += tx
		interfaces = append(interfaces, InterfaceSnapshot{
			Name:         name,
			RxBytesTotal: rx,
			TxBytesTotal: tx,
		})
	}
	sort.SliceStable(interfaces, func(left, right int) bool {
		leftTotal := interfaces[left].RxBytesTotal + interfaces[left].TxBytesTotal
		rightTotal := interfaces[right].RxBytesTotal + interfaces[right].TxBytesTotal
		if leftTotal != rightTotal {
			return leftTotal > rightTotal
		}
		return interfaces[left].Name < interfaces[right].Name
	})
	if len(interfaces) > interfaceMax {
		interfaces = interfaces[:interfaceMax]
	}
	network.RxBytesTotal = rxTotal
	network.TxBytesTotal = txTotal
	network.Interfaces = interfaces
	return current
}

// applyNetworkRates fills aggregate and per-interface rates from the delta
// between the previous and the current counters. A counter that went backwards
// (interface reset or driver restart) is skipped instead of reporting a
// negative speed.
func applyNetworkRates(network *NetworkSnapshot, previous, current map[string]netCounter, elapsed time.Duration) {
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return
	}
	var rxDelta, txDelta int64
	aggregateKnown := false
	for name, now := range current {
		before, ok := previous[name]
		if !ok {
			continue
		}
		if now.rx < before.rx || now.tx < before.tx {
			continue
		}
		rxDelta += now.rx - before.rx
		txDelta += now.tx - before.tx
		aggregateKnown = true
	}
	for index := range network.Interfaces {
		item := &network.Interfaces[index]
		before, ok := previous[item.Name]
		if !ok {
			continue
		}
		if item.RxBytesTotal < before.rx || item.TxBytesTotal < before.tx {
			continue
		}
		rx := roundOneDecimal(float64(item.RxBytesTotal-before.rx) / seconds)
		tx := roundOneDecimal(float64(item.TxBytesTotal-before.tx) / seconds)
		item.RxBytesPerSecond = &rx
		item.TxBytesPerSecond = &tx
	}
	if !aggregateKnown {
		return
	}
	rxRate := roundOneDecimal(float64(rxDelta) / seconds)
	txRate := roundOneDecimal(float64(txDelta) / seconds)
	network.RxBytesPerSecond = &rxRate
	network.TxBytesPerSecond = &txRate
}

// fillProcess records the Go runtime values (always available) and the process
// memory read. A failure only adds a note: the caller still renders the rest of
// the system object.
func (s *Sampler) fillProcess(snapshot *Snapshot) {
	var stats runtime.MemStats
	runtime.ReadMemStats(&stats)
	snapshot.Process = ProcessSnapshot{
		PID:            os.Getpid(),
		Goroutines:     runtime.NumGoroutine(),
		HeapAllocBytes: int64(stats.HeapAlloc),
	}
	proc, err := process.NewProcess(int32(os.Getpid()))
	if err != nil {
		snapshot.Notes = append(snapshot.Notes, "进程资源读取失败："+shortError(err))
		return
	}
	memory, err := proc.MemoryInfo()
	if err != nil || memory == nil {
		snapshot.Notes = append(snapshot.Notes, "进程内存读取失败："+shortError(err))
		return
	}
	snapshot.Process.RSSBytes = int64(memory.RSS)
	snapshot.Process.VMSBytes = int64(memory.VMS)
}

// clampPercent bounds a computed percentage to the 0..100 contract range.
func clampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 100 {
		return 100
	}
	return value
}

// roundThreeDecimals keeps the sample window readable while neighbourhood
// tests can observe sub-second intervals.
func roundThreeDecimals(value float64) float64 {
	return math.Round(value*1000) / 1000
}

// roundOneDecimal keeps percentages and rates compact and stable in JSON.
func roundOneDecimal(value float64) float64 {
	return math.Round(value*10) / 10
}

// shortError renders one bounded diagnostic line for the notes array.
func shortError(err error) string {
	if err == nil {
		return "未知错误"
	}
	message := strings.TrimSpace(err.Error())
	if message == "" {
		message = "未知错误"
	}
	if len(message) > 200 {
		message = message[:200] + "…"
	}
	return message
}
