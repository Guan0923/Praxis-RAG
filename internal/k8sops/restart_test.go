package k8sops

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
)

// currentRestart returns the tracked restart of one workload row.
func currentRestart(cluster *Cluster, name string) (*RestartState, bool) {
	view, err := cluster.Workloads(context.Background(), Filter{})
	if err != nil {
		return nil, false
	}
	for _, workload := range view.Workloads {
		if workload.Name == name {
			return workload.Restart, true
		}
	}
	return nil, false
}

func testedRestart(t *testing.T, cluster *Cluster, name string) *RestartState {
	t.Helper()
	state, ok := currentRestart(cluster, name)
	if !ok || state == nil {
		t.Fatalf("workload %q has no restart state", name)
	}
	return state
}

func testPatchResponse(name, namespace, uid string, generation int64) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "uid": uid, "generation": generation,
		},
	}
}

// TestRestartFlow drives one Deployment restart through the whole state
// machine: submitted -> rolling -> completed, asserting the merge patch body,
// the duplicate-submit guard and the terminal-state retention.
func TestRestartFlow(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("web", "praxis-rag", "uid-deploy", 1))
	fake.setPatchResponse(ResourceDeployments, "praxis-rag", "web",
		testPatchResponse("web", "praxis-rag", "uid-deploy", 2))
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 1
	})
	fake.waitWatchers(ResourceDeployments, 1)

	state, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "web")
	if err != nil {
		t.Fatalf("Restart: %v", err)
	}
	if state.Status != RestartSubmitted || state.Message != "已提交" ||
		state.StartedAt.IsZero() || state.UpdatedAt.IsZero() {
		t.Fatalf("restart state = %+v", state)
	}

	body, contentType, ok := fake.patchBody(ResourceDeployments, "praxis-rag", "web")
	if !ok {
		t.Fatal("restart did not PATCH the deployment")
	}
	if contentType != "application/merge-patch+json" {
		t.Fatalf("patch content type = %q", contentType)
	}
	restartAnnotationFromPatch(t, body)

	// The cached object still carries generation 1 / observedGeneration 1, so
	// the tracker is in "submitted, waiting for the controller".
	eventually(t, func() bool {
		restart, ok := currentRestart(cluster, "web")
		return ok && restart != nil && restart.Status == RestartSubmitted &&
			restart.Message == "已提交，等待控制器处理"
	})

	if _, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "web"); !errors.Is(err, ErrRestartInProgress) {
		t.Fatalf("duplicate restart error = %v, want ErrRestartInProgress", err)
	} else if !strings.Contains(err.Error(), "滚动重启") || strings.Contains(err.Error(), "kubernetes") {
		t.Fatalf("duplicate restart message = %v, want a pure Chinese reason", err)
	}

	rolling := testDeployment("web", "praxis-rag", "uid-deploy", 2)
	rolling["status"] = deploymentStatusJSON(2, 2, 1, 2, 2,
		conditionJSON("Available", "True", "", ""),
		conditionJSON("Progressing", "True", "ReplicaSetUpdated", ""))
	fake.push(ResourceDeployments, "MODIFIED", rolling)
	eventually(t, func() bool {
		restart, ok := currentRestart(cluster, "web")
		return ok && restart != nil && restart.Status == RestartRolling &&
			restart.Message == "滚动中：已更新 1/2，就绪 2/2" &&
			restart.Updated == 1 && restart.Desired == 2 && restart.Ready == 2
	})

	completed := testDeployment("web", "praxis-rag", "uid-deploy", 2)
	completed["status"] = deploymentStatusJSON(2, 2, 2, 2, 2,
		conditionJSON("Available", "True", "", ""),
		conditionJSON("Progressing", "True", "NewReplicaSetAvailable", ""))
	fake.push(ResourceDeployments, "MODIFIED", completed)
	eventually(t, func() bool {
		restart, ok := currentRestart(cluster, "web")
		return ok && restart != nil && restart.Status == RestartCompleted && restart.Message == "已完成"
	})

	// Terminal states expire after the retention window.
	key := restartKey(ResourceDeployments, "praxis-rag", "web")
	cluster.restartMu.Lock()
	cluster.restarts[key].updatedAt = time.Now().Add(-31 * time.Minute)
	cluster.restartMu.Unlock()
	view, err := cluster.Workloads(ctx, Filter{})
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	if row := workloadByName(t, view, "web"); row.Restart != nil {
		t.Fatalf("expired restart still present: %+v", row.Restart)
	}
}

// TestRestartDeleted marks the tracked restart failed when the object
// disappears from the cache.
func TestRestartDeleted(t *testing.T) {
	fake := newFakeAPI(t)
	object := testDeployment("gone", "praxis-rag", "uid-deploy", 1)
	fake.setList(ResourceDeployments, object)
	fake.setPatchResponse(ResourceDeployments, "praxis-rag", "gone",
		testPatchResponse("gone", "praxis-rag", "uid-deploy", 2))
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 1
	})
	fake.waitWatchers(ResourceDeployments, 1)
	if _, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "gone"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	fake.push(ResourceDeployments, "DELETED", object)
	// The deleted object no longer has a workload row, so the tracker is
	// observed directly; its failure state stays for the retention window.
	key := restartKey(ResourceDeployments, "praxis-rag", "gone")
	eventually(t, func() bool {
		// Snapshots drive the tracker evaluation; the real row disappears with
		// the object, so the synthetic "已删除" row appears instead.
		if _, err := cluster.Workloads(ctx, Filter{}); err != nil {
			return false
		}
		cluster.restartMu.Lock()
		defer cluster.restartMu.Unlock()
		tracker, ok := cluster.restarts[key]
		return ok && tracker.status == RestartFailed && tracker.message == "资源已被删除"
	})

	var ghost Workload
	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		if err != nil {
			return false
		}
		for _, row := range view.Workloads {
			if row.Name == "gone" {
				ghost = row
				return true
			}
		}
		return false
	})
	if ghost.Kind != "Deployment" || ghost.Resource != ResourceDeployments ||
		ghost.Namespace != "praxis-rag" || ghost.UID != "uid-deploy" {
		t.Fatalf("ghost row identity = %+v", ghost)
	}
	if ghost.Status != "已删除" || ghost.Restartable ||
		ghost.RestartBlockedReason != "资源已被删除，无法继续查看" || len(ghost.Pods) != 0 {
		t.Fatalf("ghost row content = %+v", ghost)
	}
	if ghost.Restart == nil || ghost.Restart.Status != RestartFailed ||
		ghost.Restart.Message != "资源已被删除" {
		t.Fatalf("ghost restart state = %+v", ghost.Restart)
	}

	// The shared filter applies to the synthetic row too.
	filtered, err := cluster.Workloads(ctx, Filter{Name: "GONE"})
	if err != nil {
		t.Fatalf("Workloads(name): %v", err)
	}
	if len(filtered.Workloads) != 1 || filtered.Workloads[0].Name != "gone" {
		t.Fatalf("ghost name filter = %v", workloadKinds(filtered.Workloads))
	}
	other, err := cluster.Workloads(ctx, Filter{Namespace: "other"})
	if err != nil {
		t.Fatalf("Workloads(namespace): %v", err)
	}
	if len(other.Workloads) != 0 {
		t.Fatalf("ghost namespace filter = %v", workloadKinds(other.Workloads))
	}

	// The synthetic row expires together with the tracker.
	cluster.restartMu.Lock()
	cluster.restarts[key].updatedAt = time.Now().Add(-31 * time.Minute)
	cluster.restartMu.Unlock()
	view, err := cluster.Workloads(ctx, Filter{})
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	if len(view.Workloads) != 0 {
		t.Fatalf("expired ghost row still present: %v", workloadKinds(view.Workloads))
	}
}

// TestRestartStrategyValidation rejects paused / Recreate / OnDelete /
// partitioned workloads with ErrNotRestartable and a Chinese reason, without
// ever PATCHing the API server.
func TestRestartStrategyValidation(t *testing.T) {
	fake := newFakeAPI(t)

	paused := testDeployment("paused-deploy", "praxis-rag", "uid-paused", 1)
	paused["spec"].(map[string]any)["paused"] = true
	recreate := testDeployment("recreate-deploy", "praxis-rag", "uid-recreate", 1)
	recreate["spec"].(map[string]any)["strategy"] = map[string]any{"type": "Recreate"}
	onDeleteStatefulSet := testStatefulSet("ondelete-sts", "praxis-rag", "uid-ondelete-sts")
	onDeleteStatefulSet["spec"].(map[string]any)["updateStrategy"] = map[string]any{"type": "OnDelete"}
	partitioned := testStatefulSet("partition-sts", "praxis-rag", "uid-partition-sts")
	partitioned["spec"].(map[string]any)["updateStrategy"] = map[string]any{
		"type":          "RollingUpdate",
		"rollingUpdate": map[string]any{"partition": 1},
	}
	onDeleteDaemonSet := testDaemonSet("ondelete-ds", "kube-system", "uid-ondelete-ds")
	onDeleteDaemonSet["spec"].(map[string]any)["updateStrategy"] = map[string]any{"type": "OnDelete"}

	fake.setList(ResourceDeployments, paused, recreate)
	fake.setList(ResourceStatefulSets, onDeleteStatefulSet, partitioned)
	fake.setList(ResourceDaemonSets, onDeleteDaemonSet)
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 5
	})

	for _, item := range []struct {
		resource string
		name     string
		contains string
	}{
		{ResourceDeployments, "paused-deploy", "已暂停"},
		{ResourceDeployments, "recreate-deploy", "Recreate"},
		{ResourceStatefulSets, "ondelete-sts", "OnDelete"},
		{ResourceStatefulSets, "partition-sts", "partition=1"},
		{ResourceDaemonSets, "ondelete-ds", "OnDelete"},
	} {
		namespace := "praxis-rag"
		if item.name == "ondelete-ds" {
			namespace = "kube-system"
		}
		_, err := cluster.Restart(ctx, item.resource, namespace, item.name)
		if !errors.Is(err, ErrNotRestartable) {
			t.Fatalf("%s/%s restart error = %v, want ErrNotRestartable", item.resource, item.name, err)
		}
		if !strings.Contains(err.Error(), item.contains) {
			t.Fatalf("%s/%s error = %v, want reason containing %q", item.resource, item.name, err, item.contains)
		}
		// The HTTP layer returns err.Error() directly as the 409 detail, so it
		// must be the pure Chinese reason, not a wrapped sentinel.
		if strings.Contains(err.Error(), "kubernetes") || strings.Contains(err.Error(), "not restartable") {
			t.Fatalf("%s/%s error = %v, want a pure Chinese reason", item.resource, item.name, err)
		}
	}
	if patches := fake.patches(); patches != 0 {
		t.Fatalf("rejected restarts issued %d PATCH requests", patches)
	}
	if _, err := cluster.Restart(ctx, "pods", "praxis-rag", "x"); err == nil ||
		!strings.Contains(err.Error(), "不支持重启的资源类型") {
		t.Fatalf("pods restart error = %v", err)
	}

	// The workload rows expose the blocked strategy to the Console too.
	view, err := cluster.Workloads(ctx, Filter{})
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	if row := workloadByName(t, view, "paused-deploy"); row.Restartable ||
		!strings.Contains(row.RestartBlockedReason, "已暂停") {
		t.Fatalf("paused row = %+v", row)
	}
}

// TestRestartAPIFailures maps 403/404 to the shared sentinels.
func TestRestartAPIFailures(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("forbidden", "praxis-rag", "uid-forbidden", 1))
	fake.setPatchStatus(ResourceDeployments, "praxis-rag", "forbidden", http.StatusForbidden)
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 1
	})
	if _, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "forbidden"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("403 restart error = %v, want ErrForbidden", err)
	}
	if _, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "absent"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("404 restart error = %v, want ErrNotFound", err)
	}
}

// TestRestartProgressDeadlineExceeded maps Progressing=False to a failed state
// with the condition message (truncated to 200 runes).
func TestRestartProgressDeadlineExceeded(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("deadline", "praxis-rag", "uid-deploy", 1))
	fake.setPatchResponse(ResourceDeployments, "praxis-rag", "deadline",
		testPatchResponse("deadline", "praxis-rag", "uid-deploy", 2))
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 1
	})
	fake.waitWatchers(ResourceDeployments, 1)
	if _, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "deadline"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	message := strings.Repeat("超时", 150) // 300 runes, must be truncated.
	failed := testDeployment("deadline", "praxis-rag", "uid-deploy", 2)
	failed["status"] = deploymentStatusJSON(2, 2, 1, 1, 1,
		conditionJSON("Available", "True", "", ""),
		conditionJSON("Progressing", "False", "ProgressDeadlineExceeded", message))
	fake.push(ResourceDeployments, "MODIFIED", failed)

	eventually(t, func() bool {
		restart, ok := currentRestart(cluster, "deadline")
		return ok && restart != nil && restart.Status == RestartFailed &&
			len([]rune(restart.Message)) == 200 && strings.Contains(restart.Message, "超时")
	})
}

// TestRestartTimeoutAndCleanup reports the 10 minute rollout deadline and
// drops the tracker once a failed state is older than the retention window.
func TestRestartTimeoutAndCleanup(t *testing.T) {
	fake := newFakeAPI(t)
	fake.setList(ResourceDeployments, testDeployment("slow", "praxis-rag", "uid-deploy", 1))
	fake.setPatchResponse(ResourceDeployments, "praxis-rag", "slow",
		testPatchResponse("slow", "praxis-rag", "uid-deploy", 2))
	cluster := newTestCluster(t, fake)
	ctx := t.Context()

	eventually(t, func() bool {
		view, err := cluster.Workloads(ctx, Filter{})
		return err == nil && len(view.Workloads) == 1
	})
	fake.waitWatchers(ResourceDeployments, 1)
	if _, err := cluster.Restart(ctx, ResourceDeployments, "praxis-rag", "slow"); err != nil {
		t.Fatalf("Restart: %v", err)
	}
	stalled := testDeployment("slow", "praxis-rag", "uid-deploy", 2)
	stalled["status"] = deploymentStatusJSON(2, 2, 0, 0, 0,
		conditionJSON("Available", "False", "MinimumReplicasUnavailable", ""),
		conditionJSON("Progressing", "True", "ReplicaSetUpdated", ""))
	fake.push(ResourceDeployments, "MODIFIED", stalled)
	eventually(t, func() bool {
		restart, ok := currentRestart(cluster, "slow")
		return ok && restart != nil && restart.Status == RestartRolling
	})

	key := restartKey(ResourceDeployments, "praxis-rag", "slow")
	cluster.restartMu.Lock()
	cluster.restarts[key].startedAt = time.Now().Add(-11 * time.Minute)
	cluster.restartMu.Unlock()
	eventually(t, func() bool {
		restart, ok := currentRestart(cluster, "slow")
		return ok && restart != nil && restart.Status == RestartFailed &&
			restart.Message == "发布超时：控制器未在 10 分钟内完成滚动更新"
	})

	cluster.restartMu.Lock()
	cluster.restarts[key].updatedAt = time.Now().Add(-31 * time.Minute)
	cluster.restartMu.Unlock()
	view, err := cluster.Workloads(ctx, Filter{})
	if err != nil {
		t.Fatalf("Workloads: %v", err)
	}
	if row := workloadByName(t, view, "slow"); row.Restart != nil {
		t.Fatalf("expired failed restart still present: %+v", row.Restart)
	}
}

// TestRestartStatefulSetAndDaemonSetCompletion covers the other two supported
// controllers' completion conditions.
func TestRestartStatefulSetAndDaemonSetCompletion(t *testing.T) {
	t.Run("statefulset", func(t *testing.T) {
		fake := newFakeAPI(t)
		object := testStatefulSet("db", "praxis-rag", "uid-sts")
		object["spec"].(map[string]any)["replicas"] = 2
		object["status"] = map[string]any{
			"observedGeneration": 1, "replicas": 2, "readyReplicas": 2, "updatedReplicas": 2,
			"availableReplicas": 2, "currentRevision": "db-1", "updateRevision": "db-1",
		}
		fake.setList(ResourceStatefulSets, object)
		fake.setPatchResponse(ResourceStatefulSets, "praxis-rag", "db",
			testPatchResponse("db", "praxis-rag", "uid-sts", 2))
		cluster := newTestCluster(t, fake)
		ctx := t.Context()
		eventually(t, func() bool {
			view, err := cluster.Workloads(ctx, Filter{})
			return err == nil && len(view.Workloads) == 1
		})
		fake.waitWatchers(ResourceStatefulSets, 1)
		if _, err := cluster.Restart(ctx, ResourceStatefulSets, "praxis-rag", "db"); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		rolling := testStatefulSet("db", "praxis-rag", "uid-sts")
		rolling["spec"].(map[string]any)["replicas"] = 2
		rolling["status"] = map[string]any{
			"observedGeneration": 2, "replicas": 2, "readyReplicas": 2, "updatedReplicas": 1,
			"availableReplicas": 2, "currentRevision": "db-1", "updateRevision": "db-2",
		}
		fake.push(ResourceStatefulSets, "MODIFIED", rolling)
		eventually(t, func() bool {
			restart, ok := currentRestart(cluster, "db")
			return ok && restart != nil && restart.Status == RestartRolling &&
				restart.Message == "滚动中：已更新 1/2，就绪 2/2"
		})
		completed := testStatefulSet("db", "praxis-rag", "uid-sts")
		completed["spec"].(map[string]any)["replicas"] = 2
		completed["status"] = map[string]any{
			"observedGeneration": 2, "replicas": 2, "readyReplicas": 2, "updatedReplicas": 2,
			"availableReplicas": 2, "currentRevision": "db-2", "updateRevision": "db-2",
		}
		fake.push(ResourceStatefulSets, "MODIFIED", completed)
		eventually(t, func() bool {
			restart, ok := currentRestart(cluster, "db")
			return ok && restart != nil && restart.Status == RestartCompleted
		})
	})

	t.Run("daemonset", func(t *testing.T) {
		fake := newFakeAPI(t)
		object := testDaemonSet("agent", "kube-system", "uid-ds")
		object["status"] = map[string]any{
			"observedGeneration": 1, "desiredNumberScheduled": 2, "currentNumberScheduled": 2,
			"updatedNumberScheduled": 2, "numberReady": 2, "numberAvailable": 2,
		}
		fake.setList(ResourceDaemonSets, object)
		fake.setPatchResponse(ResourceDaemonSets, "kube-system", "agent",
			testPatchResponse("agent", "kube-system", "uid-ds", 2))
		cluster := newTestCluster(t, fake)
		ctx := t.Context()
		eventually(t, func() bool {
			view, err := cluster.Workloads(ctx, Filter{})
			return err == nil && len(view.Workloads) == 1
		})
		fake.waitWatchers(ResourceDaemonSets, 1)
		if _, err := cluster.Restart(ctx, ResourceDaemonSets, "kube-system", "agent"); err != nil {
			t.Fatalf("Restart: %v", err)
		}
		rolling := testDaemonSet("agent", "kube-system", "uid-ds")
		rolling["status"] = map[string]any{
			"observedGeneration": 2, "desiredNumberScheduled": 2, "currentNumberScheduled": 2,
			"updatedNumberScheduled": 1, "numberReady": 1, "numberAvailable": 1,
		}
		fake.push(ResourceDaemonSets, "MODIFIED", rolling)
		eventually(t, func() bool {
			restart, ok := currentRestart(cluster, "agent")
			return ok && restart != nil && restart.Status == RestartRolling &&
				restart.Message == "滚动中：已更新 1/2，就绪 1/2"
		})
		completed := testDaemonSet("agent", "kube-system", "uid-ds")
		completed["status"] = map[string]any{
			"observedGeneration": 2, "desiredNumberScheduled": 2, "currentNumberScheduled": 2,
			"updatedNumberScheduled": 2, "numberReady": 2, "numberAvailable": 2,
		}
		fake.push(ResourceDaemonSets, "MODIFIED", completed)
		eventually(t, func() bool {
			restart, ok := currentRestart(cluster, "agent")
			return ok && restart != nil && restart.Status == RestartCompleted
		})
	})
}
