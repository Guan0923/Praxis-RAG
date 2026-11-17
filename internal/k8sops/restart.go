package k8sops

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	// restartAnnotation is the pod-template annotation `kubectl rollout
	// restart` writes: changing it forces the controller to roll new pods.
	restartAnnotation = "kubectl.kubernetes.io/restartedAt"
	// restartTimeout bounds one tracked rollout before it is reported failed.
	restartTimeout = 10 * time.Minute
	// restartRetention keeps terminal restart states visible for a while
	// before the tracker is cleaned up.
	restartRetention = 30 * time.Minute

	restartMessageSubmitted  = "已提交"
	restartMessagePending    = "已提交，等待控制器处理"
	restartMessageCompleted  = "已完成"
	restartMessageDeleted    = "资源已被删除"
	restartMessageTimedOut   = "发布超时：控制器未在 10 分钟内完成滚动更新"
	restartMessageUnknown    = "资源类型无法判断滚动状态"
	restartMessageInProgress = "该工作负载正在滚动重启（%s），请等待完成后再试"
)

// trackedRestart is the backend state of one submitted rolling restart. The
// fields are guarded by Cluster.restartMu.
type trackedRestart struct {
	resource   string
	namespace  string
	name       string
	uid        string
	generation int64
	startedAt  time.Time
	updatedAt  time.Time
	status     string
	message    string
	updated    int32
	desired    int32
	ready      int32
	// seen records that the cache has observed the object at least once, so
	// its later absence really means deletion (a fresh live-GET target may not
	// have reached the watch cache yet).
	seen bool
}

// BlockedError reports a workload whose update strategy cannot perform a
// rolling restart. Error returns only the Chinese reason shown inline on the
// Console row (no English sentinel prefix), while Is keeps errors.Is(err,
// ErrNotRestartable) working for the transport mapping.
type BlockedError struct {
	Reason string
}

func (err *BlockedError) Error() string { return err.Reason }

func (err *BlockedError) Is(target error) bool { return target == ErrNotRestartable }

// InProgressError reports a workload that already has a submitted/rolling
// restart. Like BlockedError, its Error is the pure Chinese reason for the
// Console detail.
type InProgressError struct {
	Reason string
}

func (err *InProgressError) Error() string { return err.Reason }

func (err *InProgressError) Is(target error) bool { return target == ErrRestartInProgress }

// deletedWorkloadTrackers returns copies of the trackers whose object vanished
// from the cache ("资源已被删除"). The snapshot layer renders them as
// synthetic rows because the real workload row disappears with the object.
func (cluster *Cluster) deletedWorkloadTrackers() []trackedRestart {
	cluster.restartMu.Lock()
	defer cluster.restartMu.Unlock()
	trackers := make([]trackedRestart, 0)
	for _, tracker := range cluster.restarts {
		if tracker.status != RestartFailed || !tracker.seen || tracker.message != restartMessageDeleted {
			continue
		}
		trackers = append(trackers, *tracker)
	}
	return trackers
}

// restartKey identifies one tracked restart.
func restartKey(resource, namespace, name string) string {
	return resource + "\x00" + namespace + "/" + name
}

func (tracker *trackedRestart) state() RestartState {
	return RestartState{
		Status:    tracker.status,
		Message:   tracker.message,
		StartedAt: tracker.startedAt,
		UpdatedAt: tracker.updatedAt,
		Updated:   tracker.updated,
		Desired:   tracker.desired,
		Ready:     tracker.ready,
	}
}

func (tracker *trackedRestart) set(status, message string, now time.Time) {
	tracker.status = status
	tracker.message = message
	tracker.updatedAt = now
}

// fillCounters refreshes the tracker counters from the latest cached object.
func (tracker *trackedRestart) fillCounters(object cachedObject) {
	updated, desired, ready := restartCounters(object)
	tracker.updated = updated
	tracker.desired = desired
	tracker.ready = ready
}

// Restart triggers a rolling restart by patching only the pod-template
// restart annotation. The request is accepted as "submitted"; the actual
// progress is tracked on every following Workloads snapshot.
func (cluster *Cluster) Restart(ctx context.Context, resource, namespace, name string) (*RestartState, error) {
	if cluster == nil || !cluster.settings.Enabled() {
		return nil, ErrNotConfigured
	}
	resource = strings.ToLower(strings.TrimSpace(resource))
	namespace = strings.TrimSpace(namespace)
	name = strings.TrimSpace(name)
	spec, known := specForResource(resource)
	if !known || !restartableResource(resource) {
		return nil, fmt.Errorf("不支持重启的资源类型 %q: 仅支持 deployments、statefulsets、daemonsets", resource)
	}
	if namespace == "" || name == "" {
		return nil, errors.New("命名空间和工作负载名称不能为空")
	}

	// Serialize the check-then-patch sequence so two concurrent clicks cannot
	// both submit a restart.
	cluster.restartSubmitMu.Lock()
	defer cluster.restartSubmitMu.Unlock()

	object, fromCache, err := cluster.restartTarget(ctx, spec, namespace, name)
	if err != nil {
		return nil, err
	}
	if reason := restartBlockedReason(object); reason != "" {
		return nil, &BlockedError{Reason: reason}
	}
	key := restartKey(resource, namespace, name)
	cluster.restartMu.Lock()
	existing, exists := cluster.restarts[key]
	cluster.restartMu.Unlock()
	if exists && (existing.status == RestartSubmitted || existing.status == RestartRolling) {
		return nil, &InProgressError{Reason: fmt.Sprintf(restartMessageInProgress, existing.message)}
	}

	startedAt := time.Now().UTC()
	patch, err := restartPatch(startedAt)
	if err != nil {
		return nil, err
	}
	payload, status, err := cluster.do(ctx, http.MethodPatch,
		cluster.objectPath(spec, namespace, name), patch, "application/merge-patch+json")
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK, http.StatusCreated:
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: 滚动重启被 RBAC 拒绝（%s）", ErrForbidden, spec.resource)
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, namespace, name)
	default:
		return nil, fmt.Errorf("Kubernetes API 返回 HTTP %d，滚动重启失败", status)
	}

	generation, uid := restartResponseIdentity(payload)
	if generation == 0 || uid == "" {
		live, getErr := cluster.liveGet(ctx, spec, namespace, name)
		if getErr != nil {
			return nil, getErr
		}
		meta := live.meta()
		generation, uid = meta.Generation, meta.UID
	}
	tracker := &trackedRestart{
		resource:   resource,
		namespace:  namespace,
		name:       name,
		uid:        uid,
		generation: generation,
		startedAt:  startedAt,
		updatedAt:  time.Now().UTC(),
		status:     RestartSubmitted,
		message:    restartMessageSubmitted,
		seen:       fromCache,
	}
	tracker.fillCounters(object)
	cluster.restartMu.Lock()
	cluster.restarts[key] = tracker
	cluster.restartMu.Unlock()
	state := tracker.state()
	return &state, nil
}

// restartTarget prefers the shared cache (no extra API call), falling back to
// one live GET when the cache has not observed the object yet. The boolean
// reports whether the object came from the cache.
func (cluster *Cluster) restartTarget(ctx context.Context, spec resourceSpec, namespace, name string) (cachedObject, bool, error) {
	if object, ok := cluster.cacheGet(spec.resource, namespace, name); ok {
		return object, true, nil
	}
	object, err := cluster.liveGet(ctx, spec, namespace, name)
	return object, false, err
}

// liveGet reads one object from the API server. It maps 404 to ErrNotFound and
// 403 to ErrForbidden, like the legacy client.
func (cluster *Cluster) liveGet(ctx context.Context, spec resourceSpec, namespace, name string) (cachedObject, error) {
	payload, status, err := cluster.do(ctx, http.MethodGet, cluster.objectPath(spec, namespace, name), nil, "")
	if err != nil {
		return nil, err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("%w: %s/%s", ErrNotFound, namespace, name)
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: 读取工作负载被 RBAC 拒绝", ErrForbidden)
	default:
		return nil, fmt.Errorf("Kubernetes API 返回 HTTP %d，读取工作负载失败", status)
	}
	object := spec.newObject()
	if err := json.Unmarshal(payload, object); err != nil {
		return nil, fmt.Errorf("无法解析 Kubernetes 响应: %w", err)
	}
	return object, nil
}

func restartableResource(resource string) bool {
	switch resource {
	case ResourceDeployments, ResourceStatefulSets, ResourceDaemonSets:
		return true
	default:
		return false
	}
}

// restartPatch is the exact merge patch `kubectl rollout restart` issues: the
// pod template carries only the restart annotation.
func restartPatch(startedAt time.Time) ([]byte, error) {
	patch := map[string]any{
		"spec": map[string]any{
			"template": map[string]any{
				"metadata": map[string]any{
					"annotations": map[string]any{
						restartAnnotation: startedAt.UTC().Format(time.RFC3339),
					},
				},
			},
		},
	}
	return json.Marshal(patch)
}

// restartResponseIdentity extracts metadata.generation and metadata.uid from
// the merge-patch response (the API server returns the full updated object).
func restartResponseIdentity(payload []byte) (int64, string) {
	var response struct {
		Metadata struct {
			Generation int64  `json:"generation"`
			UID        string `json:"uid"`
		} `json:"metadata"`
	}
	_ = json.Unmarshal(payload, &response)
	return response.Metadata.Generation, response.Metadata.UID
}

// restartBlockedReason applies the kind-specific update strategy checks. An
// empty string means the object can perform a rolling restart.
func restartBlockedReason(object cachedObject) string {
	switch typed := object.(type) {
	case *deploymentObject:
		return deploymentRestartBlockedReason(typed)
	case *statefulSetObject:
		return statefulSetRestartBlockedReason(typed)
	case *daemonSetObject:
		return daemonSetRestartBlockedReason(typed)
	default:
		return "该资源类型不支持滚动重启"
	}
}

func deploymentRestartBlockedReason(object *deploymentObject) string {
	if object.Spec.Paused {
		return "Deployment 已暂停（spec.paused=true），请先恢复后再滚动重启"
	}
	if strings.EqualFold(strings.TrimSpace(object.Spec.Strategy.Type), "Recreate") {
		return "Deployment 更新策略为 Recreate，不支持滚动重启"
	}
	return ""
}

func statefulSetRestartBlockedReason(object *statefulSetObject) string {
	if strings.EqualFold(strings.TrimSpace(object.Spec.UpdateStrategy.Type), "OnDelete") {
		return "StatefulSet 更新策略为 OnDelete，不支持滚动重启"
	}
	if partition := object.Spec.UpdateStrategy.RollingUpdate.Partition; partition != nil && *partition > 0 {
		return fmt.Sprintf("StatefulSet 处于分区更新（partition=%d），不支持滚动重启", *partition)
	}
	return ""
}

func daemonSetRestartBlockedReason(object *daemonSetObject) string {
	if strings.EqualFold(strings.TrimSpace(object.Spec.UpdateStrategy.Type), "OnDelete") {
		return "DaemonSet 更新策略为 OnDelete，不支持滚动重启"
	}
	return ""
}

// restartCounters returns (updated, desired, ready) for the three supported
// kinds.
func restartCounters(object cachedObject) (updated, desired, ready int32) {
	switch typed := object.(type) {
	case *deploymentObject:
		return typed.Status.UpdatedReplicas, desiredReplicas(typed.Spec.Replicas), typed.Status.ReadyReplicas
	case *statefulSetObject:
		return typed.Status.UpdatedReplicas, desiredReplicas(typed.Spec.Replicas), typed.Status.ReadyReplicas
	case *daemonSetObject:
		return typed.Status.UpdatedNumberScheduled, typed.Status.DesiredNumberScheduled, typed.Status.NumberReady
	default:
		return 0, 0, 0
	}
}

// evaluateRestarts advances every active tracker against the current cache and
// returns the states to embed into the workload rows. Terminal states are kept
// for restartRetention and then dropped.
func (cluster *Cluster) evaluateRestarts() map[string]*RestartState {
	if cluster == nil {
		return nil
	}
	cluster.restartMu.Lock()
	defer cluster.restartMu.Unlock()
	states := make(map[string]*RestartState, len(cluster.restarts))
	now := time.Now().UTC()
	for key, tracker := range cluster.restarts {
		switch tracker.status {
		case RestartCompleted, RestartFailed:
			if now.Sub(tracker.updatedAt) > restartRetention {
				delete(cluster.restarts, key)
				continue
			}
		default:
			cluster.evaluateRestart(tracker, now)
		}
		state := tracker.state()
		states[key] = &state
	}
	return states
}

func (cluster *Cluster) evaluateRestart(tracker *trackedRestart, now time.Time) {
	if !cluster.cacheListed(tracker.resource) {
		// The first LIST has not finished: absence from the cache is not a
		// deletion. Keep the submitted state.
		return
	}
	object, ok := cluster.cacheGet(tracker.resource, tracker.namespace, tracker.name)
	if !ok || object.meta().UID != tracker.uid {
		if !tracker.seen {
			// The object was patched through the live GET fallback and the
			// watch cache has not caught up yet; absence is not a deletion.
			if now.Sub(tracker.startedAt) > restartTimeout {
				tracker.set(RestartFailed, restartMessageTimedOut, now)
			}
			return
		}
		tracker.set(RestartFailed, restartMessageDeleted, now)
		return
	}
	tracker.seen = true
	tracker.fillCounters(object)
	switch typed := object.(type) {
	case *deploymentObject:
		cluster.evaluateDeploymentRestart(tracker, typed, now)
	case *statefulSetObject:
		cluster.evaluateStatefulSetRestart(tracker, typed, now)
	case *daemonSetObject:
		cluster.evaluateDaemonSetRestart(tracker, typed, now)
	default:
		tracker.set(RestartFailed, restartMessageUnknown, now)
	}
}

func (cluster *Cluster) evaluateDeploymentRestart(tracker *trackedRestart, object *deploymentObject, now time.Time) {
	desired := desiredReplicas(object.Spec.Replicas)
	updated := object.Status.UpdatedReplicas
	ready := object.Status.ReadyReplicas
	tracker.updated, tracker.desired, tracker.ready = updated, desired, ready
	if object.Status.ObservedGeneration < tracker.generation {
		if now.Sub(tracker.startedAt) > restartTimeout {
			tracker.set(RestartFailed, restartMessageTimedOut, now)
			return
		}
		tracker.set(RestartSubmitted, restartMessagePending, now)
		return
	}
	complete := updated >= desired && object.Status.Replicas == updated && object.Status.AvailableReplicas >= desired
	if complete {
		tracker.set(RestartCompleted, restartMessageCompleted, now)
		return
	}
	if progressing := findCondition(object.Status.Conditions, "Progressing"); progressing != nil &&
		progressing.Status == "False" && progressing.Reason == "ProgressDeadlineExceeded" {
		message := strings.TrimSpace(progressing.Message)
		if message == "" {
			message = "发布失败：ProgressDeadlineExceeded"
		}
		tracker.set(RestartFailed, truncateRunes(message, 200), now)
		return
	}
	if now.Sub(tracker.startedAt) > restartTimeout {
		tracker.set(RestartFailed, restartMessageTimedOut, now)
		return
	}
	tracker.set(RestartRolling, rollingMessage(updated, desired, ready), now)
}

func (cluster *Cluster) evaluateStatefulSetRestart(tracker *trackedRestart, object *statefulSetObject, now time.Time) {
	desired := desiredReplicas(object.Spec.Replicas)
	updated := object.Status.UpdatedReplicas
	ready := object.Status.ReadyReplicas
	tracker.updated, tracker.desired, tracker.ready = updated, desired, ready
	if object.Status.ObservedGeneration < tracker.generation {
		if now.Sub(tracker.startedAt) > restartTimeout {
			tracker.set(RestartFailed, restartMessageTimedOut, now)
			return
		}
		tracker.set(RestartSubmitted, restartMessagePending, now)
		return
	}
	complete := object.Status.UpdateRevision == object.Status.CurrentRevision &&
		updated == desired && ready >= desired
	if complete {
		tracker.set(RestartCompleted, restartMessageCompleted, now)
		return
	}
	if now.Sub(tracker.startedAt) > restartTimeout {
		tracker.set(RestartFailed, restartMessageTimedOut, now)
		return
	}
	tracker.set(RestartRolling, rollingMessage(updated, desired, ready), now)
}

func (cluster *Cluster) evaluateDaemonSetRestart(tracker *trackedRestart, object *daemonSetObject, now time.Time) {
	desired := object.Status.DesiredNumberScheduled
	updated := object.Status.UpdatedNumberScheduled
	ready := object.Status.NumberReady
	tracker.updated, tracker.desired, tracker.ready = updated, desired, ready
	if object.Status.ObservedGeneration < tracker.generation {
		if now.Sub(tracker.startedAt) > restartTimeout {
			tracker.set(RestartFailed, restartMessageTimedOut, now)
			return
		}
		tracker.set(RestartSubmitted, restartMessagePending, now)
		return
	}
	complete := updated >= desired && ready >= desired && object.Status.NumberAvailable >= desired
	if complete {
		tracker.set(RestartCompleted, restartMessageCompleted, now)
		return
	}
	if now.Sub(tracker.startedAt) > restartTimeout {
		tracker.set(RestartFailed, restartMessageTimedOut, now)
		return
	}
	tracker.set(RestartRolling, rollingMessage(updated, desired, ready), now)
}

func rollingMessage(updated, desired, ready int32) string {
	return fmt.Sprintf("滚动中：已更新 %d/%d，就绪 %d/%d", updated, desired, ready, desired)
}
