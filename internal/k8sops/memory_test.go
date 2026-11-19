package k8sops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

func TestMemorySnapshot(t *testing.T) {
	var calls atomic.Int64
	var disabled atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		switch r.URL.Path {
		case "/api/v1/nodes":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"worker-node"},"status":{"capacity":{"memory":"8Gi"},"allocatable":{"memory":"7Gi"},"conditions":[{"type":"MemoryPressure","status":"True"}]}}]}`)
		case "/apis/metrics.k8s.io/v1beta1/nodes":
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"worker-node"},"usage":{"memory":"6Gi"}}]}`)
		case "/apis/metrics.k8s.io/v1beta1/pods":
			if disabled.Load() {
				w.WriteHeader(http.StatusForbidden)
				return
			}
			fmt.Fprint(w, `{"items":[{"metadata":{"name":"doc","namespace":"rag"},"timestamp":"2026-11-19T00:00:00Z","containers":[{"name":"worker","usage":{"memory":"0"}}]}]}`)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(404)
		}
	}))
	defer server.Close()
	cluster, err := NewCluster(testSettings(t, server.URL), discardLogger(), WithHTTPClient(server.Client()))
	if err != nil {
		t.Fatal(err)
	}
	p := &podObject{}
	if err := json.Unmarshal([]byte(`{"metadata":{"name":"doc","namespace":"rag"},"spec":{"nodeName":"worker-node","priority":0,"containers":[{"name":"worker","resources":{"requests":{"memory":"512Mi"},"limits":{"memory":"2Gi"}}}]},"status":{"containerStatuses":[{"name":"worker","lastState":{"terminated":{"reason":"OOMKilled"}}}]}}`), p); err != nil {
		t.Fatal(err)
	}
	cluster.caches["pods"].replaceAll(map[string]cachedObject{"rag/doc": p})
	view, err := cluster.Memory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 0 || len(view.Nodes) != 1 || view.Nodes[0].Pressure != "True" || view.Nodes[0].Usage != "6Gi" {
		t.Fatalf("node snapshot: %+v", view)
	}
	pod := view.Pods[0]
	c := pod.Containers[0]
	if pod.Priority == nil || *pod.Priority != 0 || c.Request != "512Mi" || c.Limit != "2Gi" || c.Usage != "0" || c.Termination != "OOMKilled" {
		t.Fatalf("pod snapshot: %+v", pod)
	}
	if _, err = cluster.Memory(context.Background()); err != nil || calls.Load() != 3 {
		t.Fatalf("sample reuse: %v, %d calls", err, calls.Load())
	}
	cluster.memorySample = nil
	disabled.Store(true)
	view, err = cluster.Memory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Warnings) != 1 || view.Pods[0].Containers[0].Usage != "" || view.Pods[0].Containers[0].Limit != "2Gi" || view.Nodes[0].Pressure != "True" {
		t.Fatalf("partial response: %+v", view)
	}
}
