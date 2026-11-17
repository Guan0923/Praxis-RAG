package k8sops

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

func workloadByName(t *testing.T, view *WorkloadView, name string) Workload {
	t.Helper()
	for _, workload := range view.Workloads {
		if workload.Name == name {
			return workload
		}
	}
	t.Fatalf("workload %q not found", name)
	return Workload{}
}

func podByName(t *testing.T, view *PodView, name string) Pod {
	t.Helper()
	for _, pod := range view.Pods {
		if pod.Name == name {
			return pod
		}
	}
	t.Fatalf("pod %q not found in %v", name, podNames(view.Pods))
	return Pod{}
}

func podNames(pods []Pod) []string {
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		names = append(names, pod.Name)
	}
	return names
}

// TestClusterWorkloadSnapshot exercises the shared cache: eight list/watch
// streams, the Deployment -> ReplicaSet -> Pod and CronJob -> Job -> Pod
// associations, all seven workload kinds, filters, pod counters and messages.
func TestClusterWorkloadSnapshot(t *testing.T) {
	fake := newFakeAPI(t)

	deployment := testDeployment("api-gateway", "praxis-rag", "uid-deploy", 3)
	replicaSet := testReplicaSet("api-gateway-7d9f", "praxis-rag", "uid-rs", "api-gateway", "uid-deploy")
	statefulSet := testStatefulSet("db", "praxis-rag", "uid-sts")
	daemonSet := testDaemonSet("log-agent", "kube-system", "uid-ds")
	replicationController := testReplicationController("legacy", "praxis-rag", "uid-rc")
	cronJob := testCronJob("nightly", "praxis-rag", "uid-cj")
	job := testJob("nightly-job", "praxis-rag", "uid-job", "nightly", "uid-cj")

	worker := testPod("api-gateway-7d9f-abc", "praxis-rag", "uid-pod-worker", "node-a", "Running",
		"ReplicaSet", "api-gateway-7d9f", "uid-rs")
	worker["spec"] = map[string]any{
		"nodeName":   "node-a",
		"containers": []any{map[string]any{"name": "app"}, map[string]any{"name": "sidecar"}},
	}
	worker["status"] = map[string]any{
		"phase": "Running",
		"containerStatuses": []any{
			map[string]any{"name": "app", "ready": true, "restartCount": 2,
				"state": map[string]any{"running": map[string]any{}}},
			map[string]any{"name": "sidecar", "ready": false, "restartCount": 0,
				"state": map[string]any{"running": map[string]any{}}},
		},
	}

	statefulPod := testPod("db-0", "praxis-rag", "uid-pod-db", "node-b", "Running",
		"StatefulSet", "db", "uid-sts")
	daemonPod := testPod("log-agent-x1", "kube-system", "uid-pod-ds", "node-c", "Running",
		"DaemonSet", "log-agent", "uid-ds")
	jobPod := testPod("nightly-job-abc", "praxis-rag", "uid-pod-job", "", "Pending",
		"Job", "nightly-job", "uid-job")
	jobPod["status"] = map[string]any{
		"phase": "Pending",
		"containerStatuses": []any{
			map[string]any{"name": "main", "ready": false, "restartCount": 0,
				"state": map[string]any{"waiting": map[string]any{"reason": "ContainerCreating"}}},
		},
	}
	standalone := testPod("debug-pod", "default", "uid-pod-debug", "", "Pending", "", "", "")
	standalone["spec"] = map[string]any{
		"nodeName":   "",
		"containers": []any{map[string]any{"name": "main"}},
	}
	standalone["status"] = map[string]any{
		"phase":   "Pending",
		"reason":  "ContainersNotReady",
		"message": strings.Repeat("x", 300),
		"containerStatuses": []any{
			map[string]any{"name": "main", "ready": false, "restartCount": 1,
				"state": map[string]any{"waiting": map[string]any{"reason": "ErrImagePull"}}},
		},
	}

	fake.setList(ResourceDeployments, deployment)
	fake.setList("replicasets", replicaSet)
	fake.setList(ResourceStatefulSets, statefulSet)
	fake.setList(ResourceDaemonSets, daemonSet)
	fake.setList("replicationcontrollers", replicationController)
	fake.setList("cronjobs", cronJob)
	fake.setList("jobs", job)
	fake.setList("pods", worker, statefulPod, daemonPod, jobPod, standalone)

	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	var workloads *WorkloadView
	var podView *PodView
	eventually(t, func() bool {
		workloadSnapshot, err := cluster.Workloads(ctx, Filter{})
		if err != nil {
			return false
		}
		podSnapshot, err := cluster.Pods(ctx, Filter{})
		if err != nil {
			return false
		}
		workloads, podView = workloadSnapshot, podSnapshot
		return len(workloadSnapshot.Workloads) == 7 && len(podSnapshot.Pods) == 5
	})
	// Re-snapshot after the caches converged: the condition above can complete
	// between the two calls of one poll.
	workloads, err := cluster.Workloads(ctx, Filter{})
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	podView, err = cluster.Pods(ctx, Filter{})
	if err != nil {
		t.Fatalf("Pods: %v", err)
	}

	if workloads.GeneratedAt.IsZero() || workloads.GeneratedAt.Location() != time.UTC {
		t.Fatalf("GeneratedAt = %v, want UTC timestamp", workloads.GeneratedAt)
	}
	if info := workloads.Cluster; info.Name != "in-cluster" || info.Source != SourceServiceAccount {
		t.Fatalf("ClusterInfo = %+v", info)
	}
	wantNamespaces := []string{"default", "kube-system", "praxis-rag"}
	if strings.Join(workloads.Namespaces, ",") != strings.Join(wantNamespaces, ",") {
		t.Fatalf("Namespaces = %v, want %v", workloads.Namespaces, wantNamespaces)
	}
	kinds := make(map[string]bool)
	for _, workload := range workloads.Workloads {
		kinds[workload.Kind] = true
	}
	for _, kind := range []string{"Deployment", "StatefulSet", "DaemonSet", "ReplicaSet",
		"ReplicationController", "Job", "CronJob"} {
		if !kinds[kind] {
			t.Fatalf("kind %q missing from %v", kind, kinds)
		}
	}

	gateway := workloadByName(t, workloads, "api-gateway")
	if gateway.Resource != ResourceDeployments || gateway.Namespace != "praxis-rag" ||
		gateway.UID != "uid-deploy" || gateway.Status != "运行中" ||
		gateway.Desired != 2 || gateway.Ready != 2 || gateway.Available != 2 || gateway.Updated != 2 {
		t.Fatalf("deployment row = %+v", gateway)
	}
	if !gateway.Restartable || gateway.RestartBlockedReason != "" {
		t.Fatalf("deployment restartable = %v (%q)", gateway.Restartable, gateway.RestartBlockedReason)
	}
	if len(gateway.Pods) != 1 || gateway.Pods[0].Name != "api-gateway-7d9f-abc" {
		t.Fatalf("deployment pods = %v", podNames(gateway.Pods))
	}
	if pod := gateway.Pods[0]; pod.Node != "node-a" || pod.Phase != "Running" ||
		pod.ReadyContainers != 1 || pod.TotalContainers != 2 || pod.Restarts != 2 ||
		pod.Owner == nil || pod.Owner.Kind != "ReplicaSet" || pod.Owner.Name != "api-gateway-7d9f" {
		t.Fatalf("worker pod = %+v", pod)
	}

	replicaSetRow := workloadByName(t, workloads, "api-gateway-7d9f")
	if replicaSetRow.Owner == nil || replicaSetRow.Owner.Kind != "Deployment" ||
		replicaSetRow.Owner.Name != "api-gateway" || replicaSetRow.Restartable {
		t.Fatalf("replicaset row = %+v", replicaSetRow)
	}

	statefulSetRow := workloadByName(t, workloads, "db")
	if statefulSetRow.Status != "运行中" || len(statefulSetRow.Pods) != 1 ||
		statefulSetRow.Pods[0].Name != "db-0" {
		t.Fatalf("statefulset row = %+v", statefulSetRow)
	}

	daemonSetRow := workloadByName(t, workloads, "log-agent")
	if daemonSetRow.Namespace != "kube-system" || daemonSetRow.Status != "运行中" ||
		len(daemonSetRow.Pods) != 1 {
		t.Fatalf("daemonset row = %+v", daemonSetRow)
	}

	if row := workloadByName(t, workloads, "legacy"); row.Status != "运行中" || row.Restartable {
		t.Fatalf("replicationcontroller row = %+v", row)
	}

	cronJobRow := workloadByName(t, workloads, "nightly")
	if cronJobRow.Schedule != "*/5 * * * *" || cronJobRow.Suspended || cronJobRow.Active != 1 ||
		cronJobRow.Status != "运行中" || len(cronJobRow.Pods) != 1 || cronJobRow.Pods[0].Name != "nightly-job-abc" {
		t.Fatalf("cronjob row = %+v", cronJobRow)
	}

	jobRow := workloadByName(t, workloads, "nightly-job")
	if jobRow.Owner == nil || jobRow.Owner.Kind != "CronJob" || jobRow.Owner.Name != "nightly" ||
		jobRow.Active != 1 || len(jobRow.Pods) != 1 {
		t.Fatalf("job row = %+v", jobRow)
	}
	if jobRow.Pods[0].Reason != "ContainerCreating" {
		t.Fatalf("job pod reason = %q", jobRow.Pods[0].Reason)
	}

	debug := podByName(t, podView, "debug-pod")
	if debug.Namespace != "default" || debug.Owner != nil || debug.Reason != "ErrImagePull" ||
		debug.Phase != "Pending" || len([]rune(debug.Message)) != 200 {
		t.Fatalf("standalone pod = %+v (message runes %d)", debug, len([]rune(debug.Message)))
	}

	// Filters: namespace exact, name case-insensitive substring.
	filtered, err := cluster.Workloads(ctx, Filter{Namespace: "praxis-rag"})
	if err != nil {
		t.Fatalf("Workloads(namespace): %v", err)
	}
	if len(filtered.Workloads) != 6 {
		t.Fatalf("namespace filter rows = %v", workloadKinds(filtered.Workloads))
	}
	filtered, err = cluster.Workloads(ctx, Filter{Name: "GATEWAY"})
	if err != nil {
		t.Fatalf("Workloads(name): %v", err)
	}
	if len(filtered.Workloads) != 2 {
		t.Fatalf("name filter rows = %v", workloadKinds(filtered.Workloads))
	}
	filteredPods, err := cluster.Pods(ctx, Filter{Namespace: "default"})
	if err != nil {
		t.Fatalf("Pods(namespace): %v", err)
	}
	if len(filteredPods.Pods) != 1 || filteredPods.Pods[0].Name != "debug-pod" {
		t.Fatalf("namespace pod filter = %v", podNames(filteredPods.Pods))
	}
	filteredPods, err = cluster.Pods(ctx, Filter{Name: "DB"})
	if err != nil {
		t.Fatalf("Pods(name): %v", err)
	}
	if len(filteredPods.Pods) != 1 || filteredPods.Pods[0].Name != "db-0" {
		t.Fatalf("name pod filter = %v", podNames(filteredPods.Pods))
	}

	// Watch events must update the shared cache without a re-list.
	fake.waitWatchers(ResourceDeployments, 1)
	fake.waitWatchers("pods", 1)
	modifiedDeployment := testDeployment("api-gateway", "praxis-rag", "uid-deploy", 3)
	modifiedDeployment["status"] = deploymentStatusJSON(3, 2, 1, 2, 2,
		conditionJSON("Available", "True", "", ""))
	fake.push(ResourceDeployments, "MODIFIED", modifiedDeployment)
	eventually(t, func() bool {
		snapshot, err := cluster.Workloads(ctx, Filter{})
		if err != nil {
			return false
		}
		row := workloadByName(t, snapshot, "api-gateway")
		return row.Updated == 1 && row.Status == "滚动更新中"
	})

	fake.push("pods", "DELETED", worker)
	eventually(t, func() bool {
		snapshot, err := cluster.Workloads(ctx, Filter{})
		if err != nil {
			return false
		}
		pods, err := cluster.Pods(ctx, Filter{})
		if err != nil {
			return false
		}
		return len(workloadByName(t, snapshot, "api-gateway").Pods) == 0 && len(pods.Pods) == 4
	})

	staging := testPod("staging-debug", "staging", "uid-pod-staging", "", "Running", "", "", "")
	fake.push("pods", "ADDED", staging)
	eventually(t, func() bool {
		pods, err := cluster.Pods(ctx, Filter{})
		if err != nil {
			return false
		}
		for _, namespace := range pods.Namespaces {
			if namespace == "staging" {
				return len(pods.Pods) == 5
			}
		}
		return false
	})
}

func workloadKinds(workloads []Workload) []string {
	names := make([]string, 0, len(workloads))
	for _, workload := range workloads {
		names = append(names, workload.Kind+"/"+workload.Name)
	}
	return names
}

// TestClusterListPaginationAndWatchStart asserts the LIST paging contract
// (limit=500, continue) and that the watch resumes from the last page's
// resourceVersion with bookmarks enabled.
func TestClusterListPaginationAndWatchStart(t *testing.T) {
	var mu sync.Mutex
	var listQueries []url.Values
	var watchQuery url.Values
	deploymentDocument := func(name, uid string) map[string]any {
		return map[string]any{
			"metadata": map[string]any{
				"name": name, "namespace": "praxis-rag", "uid": uid,
				"generation": 1, "resourceVersion": "1",
			},
			"spec":   map[string]any{"replicas": 1},
			"status": map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "availableReplicas": 1},
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		if query.Get("watch") == "true" {
			if r.URL.Path == "/apis/apps/v1/deployments" {
				mu.Lock()
				watchQuery = query
				mu.Unlock()
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != "/apis/apps/v1/deployments" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"resourceVersion": "1"},
				"items":    []any{},
			})
			return
		}
		mu.Lock()
		listQueries = append(listQueries, query)
		mu.Unlock()
		if query.Get("continue") == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{
				"metadata": map[string]any{"resourceVersion": "10", "continue": "next-page"},
				"items":    []any{deploymentDocument("one", "uid-1")},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"resourceVersion": "20"},
			"items":    []any{deploymentDocument("two", "uid-2")},
		})
	}))
	defer server.Close()

	cluster, err := NewCluster(testSettings(t, server.URL), discardLogger(),
		WithHTTPClient(server.Client()), WithWatchTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	cluster.Start(context.Background())
	defer cluster.Close()

	ctx := t.Context()
	eventually(t, func() bool {
		snapshot, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(snapshot.Workloads) == 2
	})

	mu.Lock()
	pages := append([]url.Values(nil), listQueries...)
	mu.Unlock()
	if len(pages) != 2 {
		t.Fatalf("LIST requests = %d, want 2 (pagination)", len(pages))
	}
	if pages[0].Get("limit") != strconv.Itoa(listLimit) || pages[0].Get("continue") != "" {
		t.Fatalf("first page query = %v", pages[0])
	}
	if pages[1].Get("limit") != strconv.Itoa(listLimit) || pages[1].Get("continue") != "next-page" {
		t.Fatalf("second page query = %v", pages[1])
	}

	eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return watchQuery != nil
	})
	mu.Lock()
	watch := watchQuery
	mu.Unlock()
	if watch.Get("resourceVersion") != "20" {
		t.Fatalf("watch resourceVersion = %q, want 20", watch.Get("resourceVersion"))
	}
	if watch.Get("allowWatchBookmarks") != "true" {
		t.Fatalf("watch allowWatchBookmarks = %q", watch.Get("allowWatchBookmarks"))
	}
	if seconds, err := strconv.Atoi(watch.Get("timeoutSeconds")); err != nil || seconds < 1 {
		t.Fatalf("watch timeoutSeconds = %q", watch.Get("timeoutSeconds"))
	}
}

// TestClusterWatchGoneRelists sends a 410 ERROR event and asserts the cache is
// rebuilt with a fresh LIST (the new object becomes visible).
func TestClusterWatchGoneRelists(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("first", "praxis-rag", "uid-1", 1))
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		snapshot, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(snapshot.Workloads) == 1
	})
	fake.waitWatchers(ResourceDeployments, 1)

	fake.setList(ResourceDeployments,
		testDeployment("first", "praxis-rag", "uid-1", 1),
		testDeployment("second", "praxis-rag", "uid-2", 1))
	fake.push(ResourceDeployments, "ERROR", map[string]any{"code": 410, "reason": "Expired"})

	eventually(t, func() bool {
		snapshot, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(snapshot.Workloads) == 2
	})
	fake.waitWatchers(ResourceDeployments, 2)
}

// TestClusterCacheUnavailable surfaces a failed initial LIST (RBAC missing /
// API server unreachable) as an error instead of a misleading empty snapshot,
// and recovers once the API server answers again.
func TestClusterCacheUnavailable(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("web", "praxis-rag", "uid-deploy", 1))
	fake.setList("pods", testPod("web-pod", "praxis-rag", "uid-pod", "node-a", "Running", "", "", ""))
	fake.setListStatus(ResourceDeployments, http.StatusForbidden)
	fake.setListStatus("pods", http.StatusForbidden)
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventuallyWithin(t, 5*time.Second, func() bool {
		_, err := cluster.Workloads(ctx, Filter{})
		return err != nil && strings.Contains(err.Error(), "Kubernetes 资源缓存不可用")
	})
	eventuallyWithin(t, 5*time.Second, func() bool {
		_, err := cluster.Pods(ctx, Filter{})
		return err != nil && strings.Contains(err.Error(), "Kubernetes 资源缓存不可用")
	})

	fake.setListStatus(ResourceDeployments, 0)
	fake.setListStatus("pods", 0)
	eventuallyWithin(t, 5*time.Second, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 1
	})
	eventuallyWithin(t, 5*time.Second, func() bool {
		view, err := cluster.Pods(ctx, Filter{})
		return err == nil && len(view.Pods) == 1
	})
}

// TestClusterCloseKeepsSnapshot asserts Close is idempotent and the last
// snapshot stays readable (the Console may still be polling during shutdown).
func TestClusterCloseKeepsSnapshot(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("kept", "praxis-rag", "uid-1", 1))
	cluster, err := NewCluster(testSettings(t, fake.url()), discardLogger(),
		WithHTTPClient(fake.client()), WithWatchTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	cluster.Start(context.Background())
	ctx := t.Context()
	eventually(t, func() bool {
		snapshot, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(snapshot.Workloads) == 1
	})
	cluster.Close()
	cluster.Close()
	cluster.Start(context.Background()) // no-op after Close

	snapshot, err := cluster.Workloads(ctx, Filter{})
	if err != nil {
		t.Fatalf("Workloads after Close: %v", err)
	}
	if len(snapshot.Workloads) != 1 || snapshot.Workloads[0].Name != "kept" {
		t.Fatalf("snapshot after Close = %+v", snapshot.Workloads)
	}
	if pods, err := cluster.Pods(ctx, Filter{}); err != nil || pods == nil {
		t.Fatalf("Pods after Close = %v, %v", pods, err)
	}
}

// TestWorkloadViewJSONShape is a light guard on the public contract: the JSON
// field names are what the Console (and the hidden integration tests) read.
func TestWorkloadViewJSONShape(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("api-gateway", "praxis-rag", "uid-deploy", 1))
	fake.setList("replicasets", testReplicaSet("api-gateway-abc", "praxis-rag", "uid-rs", "api-gateway", "uid-deploy"))
	fake.setList("pods", testPod("api-gateway-abc-1", "praxis-rag", "uid-pod", "node-a", "Running",
		"ReplicaSet", "api-gateway-abc", "uid-rs"))
	suspended := testCronJob("nightly", "praxis-rag", "uid-cj")
	suspended["spec"] = map[string]any{"schedule": "*/5 * * * *", "suspend": true}
	fake.setList("cronjobs", suspended)

	cluster := newTestCluster(t, fake)
	ctx := t.Context()
	var snapshot *WorkloadView
	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		if err != nil || len(view.Workloads) != 3 {
			return false
		}
		for _, workload := range view.Workloads {
			if workload.Name != "api-gateway" {
				continue
			}
			if len(workload.Pods) != 1 {
				return false
			}
			snapshot = view
			return true
		}
		return false
	})
	nodes, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	for _, field := range []string{
		`"generated_at"`, `"cluster"`, `"namespaces"`, `"workloads"`,
		`"kind"`, `"resource"`, `"namespace"`, `"name"`, `"uid"`, `"status"`,
		`"desired"`, `"ready"`, `"available"`, `"updated"`, `"completions"`,
		`"succeeded"`, `"failed"`, `"active"`, `"schedule"`, `"suspended"`,
		`"restartable"`, `"pods"`, `"ready_containers"`, `"total_containers"`, `"restarts"`,
	} {
		if !strings.Contains(string(nodes), field) {
			t.Fatalf("workload JSON missing %s: %s", field, nodes)
		}
	}
}
