package k8sops

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"
)

// match applies one snapshot filter. Namespace is exact, name is a
// case-insensitive substring (the Console search box behavior).
func (filter Filter) match(namespace, name string) bool {
	if filter.Namespace != "" && filter.Namespace != namespace {
		return false
	}
	if filter.Name == "" {
		return true
	}
	return strings.Contains(strings.ToLower(name), strings.ToLower(filter.Name))
}

// truncateRunes cuts a message to at most limit runes without splitting a
// multi-byte character (Kubernetes messages are often Chinese or mixed).
func truncateRunes(value string, limit int) string {
	if limit <= 0 {
		return ""
	}
	runes := []rune(value)
	if len(runes) <= limit {
		return value
	}
	return string(runes[:limit])
}

// Workloads builds one workload snapshot. Every call also advances the rolling
// restart trackers, so "已提交" becomes "滚动中"/"已完成" on the same poll.
func (cluster *Cluster) Workloads(ctx context.Context, filter Filter) (*WorkloadView, error) {
	if cluster == nil || !cluster.settings.Enabled() {
		return nil, ErrNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// A cache that never completed its first LIST is not an empty cluster: the
	// API server is unreachable or RBAC is missing. Surface it instead of
	// rendering a misleading empty table (the transport maps it to 502). A
	// cache that is merely still starting has lastError == "" and stays 200.
	if err := cluster.workloadCachesError(); err != nil {
		return nil, err
	}
	restarts := cluster.evaluateRestarts()

	pods := cluster.podObjects()
	podsByOwner := make(map[string][]*podObject, len(pods))
	for _, pod := range pods {
		owner := controllerOwner(pod.Metadata.OwnerReferences)
		if owner == nil || owner.UID == "" {
			continue
		}
		podsByOwner[owner.UID] = append(podsByOwner[owner.UID], pod)
	}
	replicaSetsByDeployment := make(map[string][]*replicaSetObject)
	for _, replicaSet := range cluster.replicaSetObjects() {
		owner := controllerOwner(replicaSet.Metadata.OwnerReferences)
		if owner == nil || owner.UID == "" || owner.Kind != "Deployment" {
			continue
		}
		replicaSetsByDeployment[owner.UID] = append(replicaSetsByDeployment[owner.UID], replicaSet)
	}
	jobsByCronJob := make(map[string][]*jobObject)
	for _, job := range cluster.jobObjects() {
		owner := controllerOwner(job.Metadata.OwnerReferences)
		if owner == nil || owner.UID == "" || owner.Kind != "CronJob" {
			continue
		}
		jobsByCronJob[owner.UID] = append(jobsByCronJob[owner.UID], job)
	}

	view := &WorkloadView{
		GeneratedAt: time.Now().UTC(),
		Cluster:     cluster.ClusterInfo(),
		Namespaces:  cluster.namespaces(),
		Workloads:   make([]Workload, 0),
	}
	deletedTrackers := cluster.deletedWorkloadTrackers()
	deletedKeys := make(map[string]struct{}, len(deletedTrackers))
	for _, tracker := range deletedTrackers {
		deletedKeys[restartKey(tracker.resource, tracker.namespace, tracker.name)] = struct{}{}
	}
	for _, spec := range workloadSpecs {
		objects := cluster.cacheSnapshot(spec.resource)
		sort.Slice(objects, func(i, j int) bool { return objectLess(objects[i], objects[j]) })
		for _, object := range objects {
			meta := object.meta()
			if !filter.match(meta.Namespace, meta.Name) {
				continue
			}
			row, ok := workloadRow(spec, object, podsByOwner, replicaSetsByDeployment, jobsByCronJob)
			if !ok {
				continue
			}
			key := restartKey(spec.resource, meta.Namespace, meta.Name)
			// A tracker for a deleted object must not leak onto a new object
			// that happens to reuse the same namespaced name; it is rendered as
			// a synthetic row instead.
			if _, deleted := deletedKeys[key]; !deleted {
				if state, exists := restarts[key]; exists {
					row.Restart = state
				}
			}
			view.Workloads = append(view.Workloads, row)
		}
	}
	// The workload row disappears with the object, so a deleted workload would
	// otherwise lose its "failed: resource deleted" state before the operator
	// sees it. Append one synthetic row per retained tracker; it expires with
	// the tracker after the 30 minute terminal-state retention.
	ghostRows := make([]Workload, 0)
	for _, tracker := range deletedTrackers {
		if _, stillCached := cluster.cacheGet(tracker.resource, tracker.namespace, tracker.name); stillCached {
			continue
		}
		spec, known := specForResource(tracker.resource)
		if !known {
			continue
		}
		if !filter.match(tracker.namespace, tracker.name) {
			continue
		}
		state := tracker.state()
		ghostRows = append(ghostRows, Workload{
			Kind:                 spec.kind,
			Resource:             tracker.resource,
			Namespace:            tracker.namespace,
			Name:                 tracker.name,
			UID:                  tracker.uid,
			Status:               "已删除",
			Restartable:          false,
			RestartBlockedReason: "资源已被删除，无法继续查看",
			Restart:              &state,
			Pods:                 make([]Pod, 0),
		})
	}
	sort.Slice(ghostRows, func(i, j int) bool {
		if ghostRows[i].Resource != ghostRows[j].Resource {
			return ghostRows[i].Resource < ghostRows[j].Resource
		}
		if ghostRows[i].Namespace != ghostRows[j].Namespace {
			return ghostRows[i].Namespace < ghostRows[j].Namespace
		}
		return ghostRows[i].Name < ghostRows[j].Name
	})
	view.Workloads = append(view.Workloads, ghostRows...)
	return view, nil
}

// Pods builds the "all pods" snapshot. It contains every cached pod, including
// pods without an owning workload.
func (cluster *Cluster) Pods(ctx context.Context, filter Filter) (*PodView, error) {
	if cluster == nil || !cluster.settings.Enabled() {
		return nil, ErrNotConfigured
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := cluster.podCacheError(); err != nil {
		return nil, err
	}
	pods := cluster.podObjects()
	rows := make([]Pod, 0, len(pods))
	for _, pod := range pods {
		if !filter.match(pod.Metadata.Namespace, pod.Metadata.Name) {
			continue
		}
		rows = append(rows, podRow(pod))
	}
	sortPods(rows)
	return &PodView{
		GeneratedAt: time.Now().UTC(),
		Namespaces:  cluster.namespaces(),
		Pods:        rows,
	}, nil
}

// Status summaries for the workload table. "滚动更新中" is used for the
// controllers that actually roll pods (Deployment/StatefulSet/DaemonSet); the
// legacy replica controllers report a plain "更新中".
const (
	statusRollingUpdate = "滚动更新中"
	statusUpdating      = "更新中"
)

// workloadRow converts one cached workload object into its Console row.
func workloadRow(
	spec resourceSpec,
	object cachedObject,
	podsByOwner map[string][]*podObject,
	replicaSetsByDeployment map[string][]*replicaSetObject,
	jobsByCronJob map[string][]*jobObject,
) (Workload, bool) {
	meta := object.meta()
	row := Workload{
		Kind:      spec.kind,
		Resource:  spec.resource,
		Namespace: meta.Namespace,
		Name:      meta.Name,
		UID:       meta.UID,
		Owner:     displayedOwner(meta.OwnerReferences),
		Pods:      make([]Pod, 0),
	}
	switch typed := object.(type) {
	case *deploymentObject:
		row.Desired = desiredReplicas(typed.Spec.Replicas)
		row.Ready = typed.Status.ReadyReplicas
		row.Available = typed.Status.AvailableReplicas
		row.Updated = typed.Status.UpdatedReplicas
		row.Status = deploymentStatus(typed)
		blocked := deploymentRestartBlockedReason(typed)
		row.Restartable = blocked == ""
		row.RestartBlockedReason = blocked
		ownerUIDs := []string{meta.UID}
		for _, replicaSet := range replicaSetsByDeployment[meta.UID] {
			ownerUIDs = append(ownerUIDs, replicaSet.Metadata.UID)
		}
		row.Pods = ownedPodRows(ownerUIDs, podsByOwner)
	case *statefulSetObject:
		row.Desired = desiredReplicas(typed.Spec.Replicas)
		row.Ready = typed.Status.ReadyReplicas
		row.Available = typed.Status.AvailableReplicas
		row.Updated = typed.Status.UpdatedReplicas
		row.Status = statefulSetStatus(typed)
		blocked := statefulSetRestartBlockedReason(typed)
		row.Restartable = blocked == ""
		row.RestartBlockedReason = blocked
		row.Pods = ownedPodRows([]string{meta.UID}, podsByOwner)
	case *daemonSetObject:
		row.Desired = typed.Status.DesiredNumberScheduled
		row.Ready = typed.Status.NumberReady
		row.Available = typed.Status.NumberAvailable
		row.Updated = typed.Status.UpdatedNumberScheduled
		row.Status = daemonSetStatus(typed)
		blocked := daemonSetRestartBlockedReason(typed)
		row.Restartable = blocked == ""
		row.RestartBlockedReason = blocked
		row.Pods = ownedPodRows([]string{meta.UID}, podsByOwner)
	case *replicaSetObject:
		row.Desired = desiredReplicas(typed.Spec.Replicas)
		row.Ready = typed.Status.ReadyReplicas
		row.Available = typed.Status.AvailableReplicas
		row.Updated = typed.Status.Replicas
		row.Status = replicaStatus(row.Desired, row.Ready)
		row.Restartable = false
		row.RestartBlockedReason = "ReplicaSet 不支持滚动重启，请重启所属 Deployment"
		row.Pods = ownedPodRows([]string{meta.UID}, podsByOwner)
	case *replicationControllerObject:
		row.Desired = desiredReplicas(typed.Spec.Replicas)
		row.Ready = typed.Status.ReadyReplicas
		row.Available = typed.Status.AvailableReplicas
		row.Updated = typed.Status.Replicas
		row.Status = replicaStatus(row.Desired, row.Ready)
		row.Restartable = false
		row.RestartBlockedReason = "ReplicationController 不支持滚动重启"
		row.Pods = ownedPodRows([]string{meta.UID}, podsByOwner)
	case *jobObject:
		row.Completions = jobCompletions(typed)
		row.Succeeded = typed.Status.Succeeded
		row.Failed = typed.Status.Failed
		row.Active = typed.Status.Active
		row.Status = jobStatus(typed)
		row.Restartable = false
		row.RestartBlockedReason = "Job 不支持滚动重启"
		row.Pods = ownedPodRows([]string{meta.UID}, podsByOwner)
	case *cronJobObject:
		row.Schedule = typed.Spec.Schedule
		row.Suspended = typed.Spec.Suspend != nil && *typed.Spec.Suspend
		row.Active = int32(len(typed.Status.Active))
		row.Status = cronJobStatus(typed)
		row.Restartable = false
		row.RestartBlockedReason = "CronJob 不支持滚动重启"
		jobUIDs := []string{meta.UID}
		for _, job := range jobsByCronJob[meta.UID] {
			jobUIDs = append(jobUIDs, job.Metadata.UID)
		}
		row.Pods = ownedPodRows(jobUIDs, podsByOwner)
	default:
		return Workload{}, false
	}
	return row, true
}

func deploymentStatus(object *deploymentObject) string {
	if object.Spec.Paused {
		return "已暂停"
	}
	desired := desiredReplicas(object.Spec.Replicas)
	if desired == 0 {
		return "已停止"
	}
	if progressing := findCondition(object.Status.Conditions, "Progressing"); progressing != nil &&
		progressing.Status == "False" && progressing.Reason == "ProgressDeadlineExceeded" {
		return "失败"
	}
	if object.Status.ReadyReplicas >= desired && object.Status.UpdatedReplicas >= desired &&
		object.Status.AvailableReplicas >= desired {
		return "运行中"
	}
	return statusRollingUpdate
}

func statefulSetStatus(object *statefulSetObject) string {
	desired := desiredReplicas(object.Spec.Replicas)
	if desired == 0 {
		return "已停止"
	}
	if object.Status.CurrentRevision != "" && object.Status.CurrentRevision == object.Status.UpdateRevision &&
		object.Status.ReadyReplicas >= desired && object.Status.UpdatedReplicas >= desired {
		return "运行中"
	}
	return statusRollingUpdate
}

func daemonSetStatus(object *daemonSetObject) string {
	if object.Status.ObservedGeneration < object.Metadata.Generation {
		return statusRollingUpdate
	}
	desired := object.Status.DesiredNumberScheduled
	if desired == 0 {
		return "运行中"
	}
	if object.Status.NumberReady >= desired && object.Status.UpdatedNumberScheduled >= desired &&
		object.Status.NumberAvailable >= desired {
		return "运行中"
	}
	return statusRollingUpdate
}

func replicaStatus(desired, ready int32) string {
	if desired == 0 {
		return "已停止"
	}
	if ready >= desired {
		return "运行中"
	}
	return statusUpdating
}

func jobCompletions(object *jobObject) int32 {
	if object.Spec.Completions == nil {
		return 1
	}
	return *object.Spec.Completions
}

func jobStatus(object *jobObject) string {
	if complete := findCondition(object.Status.Conditions, "Complete"); complete != nil && complete.Status == "True" {
		return "已完成"
	}
	if failed := findCondition(object.Status.Conditions, "Failed"); failed != nil && failed.Status == "True" {
		return "失败"
	}
	if object.Spec.Suspend != nil && *object.Spec.Suspend {
		return "已暂停"
	}
	if object.Status.Active > 0 {
		return "运行中"
	}
	if desired := jobCompletions(object); desired > 0 && object.Status.Succeeded >= desired {
		return "已完成"
	}
	return "等待中"
}

func cronJobStatus(object *cronJobObject) string {
	if object.Spec.Suspend != nil && *object.Spec.Suspend {
		return "已暂停"
	}
	if len(object.Status.Active) > 0 {
		return "运行中"
	}
	return "空闲"
}

// ownedPodRows collects the pods owned by any of ownerUIDs, deduplicates by
// namespace/name (a Deployment and its ReplicaSet can never own the same pod,
// but the index is shared), and sorts the result for a stable Console render.
func ownedPodRows(ownerUIDs []string, podsByOwner map[string][]*podObject) []Pod {
	seen := make(map[string]struct{})
	rows := make([]Pod, 0)
	for _, uid := range ownerUIDs {
		if uid == "" {
			continue
		}
		for _, pod := range podsByOwner[uid] {
			key := cacheKey(pod.Metadata.Namespace, pod.Metadata.Name)
			if _, exists := seen[key]; exists {
				continue
			}
			seen[key] = struct{}{}
			rows = append(rows, podRow(pod))
		}
	}
	sortPods(rows)
	return rows
}

// podRow converts one cached pod into its Console row.
func podRow(pod *podObject) Pod {
	var ready int32
	var restarts int32
	waitingReason := ""
	for _, status := range pod.Status.ContainerStatuses {
		if status.Ready {
			ready++
		}
		restarts += status.RestartCount
		if waitingReason == "" && status.State.Waiting != nil && status.State.Waiting.Reason != "" {
			waitingReason = status.State.Waiting.Reason
		}
	}
	reason := waitingReason
	if reason == "" {
		reason = pod.Status.Reason
	}
	return Pod{
		Namespace:       pod.Metadata.Namespace,
		Name:            pod.Metadata.Name,
		Node:            pod.Spec.NodeName,
		Phase:           pod.Status.Phase,
		Reason:          reason,
		Message:         truncateRunes(pod.Status.Message, 200),
		ReadyContainers: ready,
		TotalContainers: int32(len(pod.Spec.Containers)),
		Restarts:        restarts,
		Owner:           podOwnerRef(pod.Metadata.OwnerReferences),
	}
}

// podOwnerRef renders the owner reference shown in the pod table: the
// controller reference when present, else the first reference.
func podOwnerRef(references []ownerReference) *OwnerRef {
	reference := controllerOwner(references)
	if reference == nil {
		return nil
	}
	return &OwnerRef{Kind: reference.Kind, Name: reference.Name}
}

// displayedOwner renders the owner of a workload row, but only when the owner
// is another built-in workload kind (ReplicaSet -> Deployment, Job -> CronJob).
func displayedOwner(references []ownerReference) *OwnerRef {
	for index := range references {
		reference := &references[index]
		if reference.Controller != nil && *reference.Controller && knownWorkloadKind(reference.Kind) {
			return &OwnerRef{Kind: reference.Kind, Name: reference.Name}
		}
	}
	for index := range references {
		reference := &references[index]
		if knownWorkloadKind(reference.Kind) {
			return &OwnerRef{Kind: reference.Kind, Name: reference.Name}
		}
	}
	return nil
}

func knownWorkloadKind(kind string) bool {
	switch kind {
	case "Deployment", "StatefulSet", "DaemonSet", "ReplicaSet", "ReplicationController", "Job", "CronJob":
		return true
	default:
		return false
	}
}

func objectLess(left, right cachedObject) bool {
	leftMeta := left.meta()
	rightMeta := right.meta()
	if leftMeta.Namespace != rightMeta.Namespace {
		return leftMeta.Namespace < rightMeta.Namespace
	}
	return leftMeta.Name < rightMeta.Name
}

func sortPods(pods []Pod) {
	sort.Slice(pods, func(i, j int) bool {
		if pods[i].Namespace != pods[j].Namespace {
			return pods[i].Namespace < pods[j].Namespace
		}
		return pods[i].Name < pods[j].Name
	})
}

func (cluster *Cluster) podObjects() []*podObject {
	objects := cluster.cacheSnapshot(specPods.resource)
	pods := make([]*podObject, 0, len(objects))
	for _, object := range objects {
		if pod, ok := object.(*podObject); ok {
			pods = append(pods, pod)
		}
	}
	sort.Slice(pods, func(i, j int) bool { return objectLess(pods[i], pods[j]) })
	return pods
}

// workloadCachesError reports the first workload cache that failed its initial
// LIST (RBAC missing, API server unreachable, ...). A cache that is merely
// starting has no recorded error and is not reported.
func (cluster *Cluster) workloadCachesError() error {
	for _, spec := range workloadSpecs {
		listed, lastError := cluster.cacheState(spec.resource)
		if !listed && lastError != "" {
			return fmt.Errorf("Kubernetes 资源缓存不可用（检查 RBAC 与集群连通性）: %s", lastError)
		}
	}
	return nil
}

// podCacheError applies the same availability rule to the pod stream.
func (cluster *Cluster) podCacheError() error {
	listed, lastError := cluster.cacheState(specPods.resource)
	if !listed && lastError != "" {
		return fmt.Errorf("Kubernetes 资源缓存不可用（检查 RBAC 与集群连通性）: %s", lastError)
	}
	return nil
}

func (cluster *Cluster) replicaSetObjects() []*replicaSetObject {
	objects := cluster.cacheSnapshot(specReplicaSets.resource)
	replicaSets := make([]*replicaSetObject, 0, len(objects))
	for _, object := range objects {
		if replicaSet, ok := object.(*replicaSetObject); ok {
			replicaSets = append(replicaSets, replicaSet)
		}
	}
	sort.Slice(replicaSets, func(i, j int) bool { return objectLess(replicaSets[i], replicaSets[j]) })
	return replicaSets
}

func (cluster *Cluster) jobObjects() []*jobObject {
	objects := cluster.cacheSnapshot(specJobs.resource)
	jobs := make([]*jobObject, 0, len(objects))
	for _, object := range objects {
		if job, ok := object.(*jobObject); ok {
			jobs = append(jobs, job)
		}
	}
	sort.Slice(jobs, func(i, j int) bool { return objectLess(jobs[i], jobs[j]) })
	return jobs
}
