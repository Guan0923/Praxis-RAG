package k8sops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type MemoryView struct {
	GeneratedAt time.Time    `json:"generated_at"`
	Nodes       []NodeMemory `json:"nodes"`
	Pods        []PodMemory  `json:"pods"`
	Warnings    []string     `json:"warnings"`
}

type NodeMemory struct {
	Name        string `json:"name"`
	Capacity    string `json:"capacity"`
	Allocatable string `json:"allocatable"`
	Pressure    string `json:"pressure"`
	Usage       string `json:"usage,omitempty"`
	Timestamp   string `json:"timestamp,omitempty"`
}

type ContainerMemory struct {
	Termination string `json:"termination,omitempty"`
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Request     string `json:"request,omitempty"`
	Limit       string `json:"limit,omitempty"`
	Usage       string `json:"usage,omitempty"`
}
type PodMemory struct {
	Namespace     string            `json:"namespace"`
	Name          string            `json:"name"`
	Node          string            `json:"node"`
	Priority      *int32            `json:"priority"`
	PriorityClass string            `json:"priority_class"`
	Request       string            `json:"request,omitempty"`
	Limit         string            `json:"limit,omitempty"`
	Reason        string            `json:"reason,omitempty"`
	Message       string            `json:"message,omitempty"`
	Timestamp     string            `json:"timestamp,omitempty"`
	Containers    []ContainerMemory `json:"containers"`
}

type nodeRecord struct {
	Metadata objectMeta
	Status   struct {
		Capacity, Allocatable map[string]string
		Conditions            []condition
	}
}
type memoryMetric struct {
	Metadata   objectMeta
	Timestamp  string
	Usage      map[string]string
	Containers []struct {
		Name  string
		Usage map[string]string
	}
}
type clusterMemorySample struct {
	at       time.Time
	nodes    []NodeMemory
	pods     map[string]memoryMetric
	warnings []string
}

func readMemoryList[T any](ctx context.Context, cluster *Cluster, path string) ([]T, error) {
	endpoint := strings.TrimRight(cluster.settings.APIEndpoint, "/") + path
	next := endpoint + "?limit=500"
	var items []T
	for {
		body, status, err := cluster.do(ctx, http.MethodGet, next, nil, "")
		if err != nil {
			return nil, err
		}
		if status != http.StatusOK {
			return nil, fmt.Errorf("HTTP %d", status)
		}
		var page struct {
			Items    []T
			Metadata struct{ Continue string }
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return nil, err
		}
		items = append(items, page.Items...)
		if page.Metadata.Continue == "" {
			return items, nil
		}
		next = endpoint + "?limit=500&continue=" + url.QueryEscape(page.Metadata.Continue)
	}
}

// Keep one sample per cluster across Console sessions. The core Pod watch
// remains independent of Metrics API availability.
func (cluster *Cluster) sampleMemory(ctx context.Context) (*clusterMemorySample, error) {
	cluster.memoryMu.Lock()
	defer cluster.memoryMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if sample := cluster.memorySample; sample != nil && time.Since(sample.at) < 15*time.Second {
		return sample, nil
	}
	var nodes []nodeRecord
	var nodeMetrics, podMetrics []memoryMetric
	var nodesErr, nodeMetricsErr, podMetricsErr error
	var wg sync.WaitGroup
	// Keep time for returning resource specifications when the aggregated API is slow.
	readCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	wg.Go(func() { nodes, nodesErr = readMemoryList[nodeRecord](readCtx, cluster, "/api/v1/nodes") })
	wg.Go(func() {
		nodeMetrics, nodeMetricsErr = readMemoryList[memoryMetric](readCtx, cluster, "/apis/metrics.k8s.io/v1beta1/nodes")
	})
	wg.Go(func() {
		podMetrics, podMetricsErr = readMemoryList[memoryMetric](readCtx, cluster, "/apis/metrics.k8s.io/v1beta1/pods")
	})
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sample := &clusterMemorySample{at: time.Now().UTC(), nodes: []NodeMemory{}, pods: map[string]memoryMetric{}}
	if nodesErr != nil {
		sample.warnings = append(sample.warnings, "节点状态不可用，请检查集群连接和 nodes 列表权限。")
	}
	if nodeMetricsErr != nil {
		sample.warnings = append(sample.warnings, "节点用量不可用，请检查 Metrics API 和节点指标读取权限。")
	}
	if podMetricsErr != nil {
		sample.warnings = append(sample.warnings, "Pod 用量不可用，请检查 Metrics API 和 Pod 指标读取权限。")
	}
	byName := map[string]memoryMetric{}
	for _, m := range nodeMetrics {
		byName[m.Metadata.Name] = m
	}
	for _, n := range nodes {
		m := byName[n.Metadata.Name]
		row := NodeMemory{Name: n.Metadata.Name, Capacity: n.Status.Capacity["memory"], Allocatable: n.Status.Allocatable["memory"], Usage: m.Usage["memory"], Timestamp: m.Timestamp}
		for _, c := range n.Status.Conditions {
			if c.Type == "MemoryPressure" {
				row.Pressure = c.Status
				break
			}
		}
		sample.nodes = append(sample.nodes, row)
	}
	sort.Slice(sample.nodes, func(i, j int) bool { return sample.nodes[i].Name < sample.nodes[j].Name })
	for _, m := range podMetrics {
		sample.pods[m.Metadata.Namespace+"/"+m.Metadata.Name] = m
	}
	cluster.memorySample = sample
	return sample, nil
}

func (cluster *Cluster) Memory(ctx context.Context) (*MemoryView, error) {
	if !cluster.Configured() {
		return nil, ErrNotConfigured
	}
	sample, err := cluster.sampleMemory(ctx)
	if err != nil {
		return nil, err
	}
	view := &MemoryView{GeneratedAt: sample.at, Nodes: sample.nodes, Pods: []PodMemory{}, Warnings: append([]string{}, sample.warnings...)}
	if listed, _ := cluster.cacheState(specPods.resource); !listed {
		view.Warnings = append(view.Warnings, "Pod 清单尚不可用，请检查集群连接和 Pod 列表权限。")
	}
	for _, p := range cluster.podObjects() {
		metric := sample.pods[p.Metadata.Namespace+"/"+p.Metadata.Name]
		created, _ := time.Parse(time.RFC3339Nano, p.Metadata.CreationTimestamp)
		sampled, _ := time.Parse(time.RFC3339Nano, metric.Timestamp)
		if sampled.Before(created) || (metric.Metadata.UID != "" && metric.Metadata.UID != p.Metadata.UID) {
			metric = memoryMetric{}
		}
		view.Pods = append(view.Pods, podMemoryRow(p, metric))
	}
	return view, nil
}

func podMemoryRow(p *podObject, m memoryMetric) PodMemory {
	row := PodMemory{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, Node: p.Spec.NodeName, Priority: p.Spec.Priority, PriorityClass: p.Spec.PriorityClassName,
		Request: p.Spec.Resources.Requests["memory"], Limit: p.Spec.Resources.Limits["memory"], Reason: p.Status.Reason, Message: truncateRunes(p.Status.Message, 512), Timestamp: m.Timestamp, Containers: []ContainerMemory{}}
	usage := map[string]string{}
	for _, c := range m.Containers {
		usage[c.Name] = c.Usage["memory"]
	}
	termination := map[string]string{}
	for _, statuses := range [][]containerStatus{p.Status.ContainerStatuses, p.Status.InitContainerStatuses, p.Status.EphemeralContainerStatuses} {
		for _, c := range statuses {
			if c.State.Terminated != nil {
				termination[c.Name] = c.State.Terminated.Reason
			} else if c.LastState.Terminated != nil {
				termination[c.Name] = c.LastState.Terminated.Reason
			}
		}
	}
	add := func(kind string, containers []podContainer) {
		for _, c := range containers {
			row.Containers = append(row.Containers, ContainerMemory{Termination: termination[c.Name], Name: c.Name, Kind: kind, Request: c.Resources.Requests["memory"], Limit: c.Resources.Limits["memory"], Usage: usage[c.Name]})
		}
	}
	add("应用", p.Spec.Containers)
	add("初始化", p.Spec.InitContainers)
	add("临时", p.Spec.EphemeralContainers)
	return row
}
