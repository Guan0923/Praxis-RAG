package k8sops

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeAPI is a minimal Kubernetes API server for the cache/restart tests. It
// serves the eight cluster-wide LIST endpoints, streams watch events from Go
// channels, serves one-object GETs for the restart fallback, and records
// merge-patch requests.
type fakeAPI struct {
	t      *testing.T
	server *httptest.Server

	mu             sync.Mutex
	lists          map[string][]any
	objects        map[string]any
	patchResponses map[string]any
	patchStatuses  map[string]int
	patchBodies    map[string][]byte
	patchTypes     map[string]string
	patchCount     int
	listStatuses   map[string]int
	watchStatuses  map[string]int
	watchers       map[string][]chan string
	watchSeen      map[string]int
	lastQueries    map[string]url.Values
	listQueries    map[string][]url.Values
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()
	fake := &fakeAPI{
		t:              t,
		lists:          make(map[string][]any),
		objects:        make(map[string]any),
		patchResponses: make(map[string]any),
		patchStatuses:  make(map[string]int),
		patchBodies:    make(map[string][]byte),
		patchTypes:     make(map[string]string),
		listStatuses:   make(map[string]int),
		watchStatuses:  make(map[string]int),
		watchers:       make(map[string][]chan string),
		watchSeen:      make(map[string]int),
		lastQueries:    make(map[string]url.Values),
		listQueries:    make(map[string][]url.Values),
	}
	for _, spec := range resourceSpecs {
		fake.lists[spec.apiPath+"/"+spec.resource] = []any{}
	}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serveHTTP))
	t.Cleanup(fake.server.Close)
	return fake
}

func (fake *fakeAPI) client() *http.Client { return fake.server.Client() }
func (fake *fakeAPI) url() string          { return fake.server.URL }

func (fake *fakeAPI) collectionPath(resource string) string {
	spec, ok := specForResource(resource)
	if !ok {
		fake.t.Fatalf("unknown resource %q", resource)
	}
	return spec.apiPath + "/" + spec.resource
}

func (fake *fakeAPI) objectPath(resource, namespace, name string) string {
	spec, _ := specForResource(resource)
	return spec.apiPath + "/namespaces/" + namespace + "/" + spec.resource + "/" + name
}

// setList installs the initial LIST payload for one resource.
func (fake *fakeAPI) setList(resource string, objects ...any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.lists[fake.collectionPath(resource)] = objects
}

// setObject installs one live GET payload (restart fallback).
func (fake *fakeAPI) setObject(resource, namespace, name string, object any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.objects[fake.objectPath(resource, namespace, name)] = object
}

// setPatchResponse installs the merge-patch response (with metadata.generation
// and metadata.uid) for one object.
func (fake *fakeAPI) setPatchResponse(resource, namespace, name string, object any) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.patchResponses[fake.objectPath(resource, namespace, name)] = object
}

func (fake *fakeAPI) setPatchStatus(resource, namespace, name string, status int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.patchStatuses[fake.objectPath(resource, namespace, name)] = status
}

func (fake *fakeAPI) setWatchStatus(resource string, status int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.watchStatuses[fake.collectionPath(resource)] = status
}

// setListStatus makes the LIST endpoint answer the given status (0 restores the
// normal 200 response). It models RBAC/connectivity failures.
func (fake *fakeAPI) setListStatus(resource string, status int) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.listStatuses[fake.collectionPath(resource)] = status
}

func (fake *fakeAPI) patchBody(resource, namespace, name string) ([]byte, string, bool) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	path := fake.objectPath(resource, namespace, name)
	body, ok := fake.patchBodies[path]
	return body, fake.patchTypes[path], ok
}

func (fake *fakeAPI) patches() int {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.patchCount
}

// push broadcasts one watch event to every connected watcher of the resource.
func (fake *fakeAPI) push(resource, eventType string, object any) {
	event, err := json.Marshal(map[string]any{"type": eventType, "object": object})
	if err != nil {
		fake.t.Fatalf("marshal watch event: %v", err)
	}
	event = append(event, '\n')
	fake.mu.Lock()
	channels := append([]chan string(nil), fake.watchers[fake.collectionPath(resource)]...)
	fake.mu.Unlock()
	for _, channel := range channels {
		select {
		case channel <- string(event):
		case <-time.After(2 * time.Second):
			fake.t.Errorf("watch event %s/%s could not be delivered", resource, eventType)
		}
	}
}

// waitWatchers blocks until n watch connections for the resource are open, so
// a test never pushes an event into the void.
func (fake *fakeAPI) waitWatchers(resource string, count int) {
	fake.t.Helper()
	path := fake.collectionPath(resource)
	deadline := time.Now().Add(2 * time.Second)
	for {
		fake.mu.Lock()
		seen := fake.watchSeen[path]
		fake.mu.Unlock()
		if seen >= count {
			return
		}
		if time.Now().After(deadline) {
			fake.t.Fatalf("watch for %s not connected: saw %d, want %d", resource, seen, count)
		}
		time.Sleep(time.Millisecond)
	}
}

// lastQuery returns the query parameters of the most recent non-watch request
// for the resource (the LIST, normally).
func (fake *fakeAPI) lastQuery(resource string) url.Values {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.lastQueries[fake.collectionPath(resource)]
}

// lastListQuery returns the query parameters of the most recent LIST request,
// even after a watch request overwrote lastQueries.
func (fake *fakeAPI) lastListQuery(resource string) url.Values {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	queries := fake.listQueries[fake.collectionPath(resource)]
	if len(queries) == 0 {
		return nil
	}
	return queries[len(queries)-1]
}

// watchQuery returns the query parameters of the most recent watch request.
func (fake *fakeAPI) watchQuery(resource string) url.Values {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	return fake.lastQueries[fake.collectionPath(resource)]
}

func (fake *fakeAPI) serveHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	query := r.URL.Query()
	if query.Get("watch") == "true" {
		fake.serveWatch(w, r, path, query)
		return
	}
	if r.Method == http.MethodPatch {
		fake.servePatch(w, r, path)
		return
	}

	fake.mu.Lock()
	items, isList := fake.lists[path]
	object, isObject := fake.objects[path]
	listStatus := fake.listStatuses[path]
	fake.lastQueries[path] = query
	fake.mu.Unlock()

	switch {
	case isList:
		if listStatus != 0 {
			w.WriteHeader(listStatus)
			return
		}
		fake.mu.Lock()
		fake.listQueries[path] = append(fake.listQueries[path], query)
		fake.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"metadata": map[string]any{"resourceVersion": "100"},
			"items":    items,
		})
	case isObject:
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(object)
	default:
		http.NotFound(w, r)
	}
}

func (fake *fakeAPI) serveWatch(w http.ResponseWriter, r *http.Request, path string, query url.Values) {
	fake.mu.Lock()
	status := fake.watchStatuses[path]
	fake.watchSeen[path]++
	fake.lastQueries[path] = query
	fake.mu.Unlock()
	if status != 0 {
		w.WriteHeader(status)
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	channel := make(chan string, 128)
	fake.mu.Lock()
	fake.watchers[path] = append(fake.watchers[path], channel)
	fake.mu.Unlock()

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()
	for {
		select {
		case event := <-channel:
			if _, err := io.WriteString(w, event); err != nil {
				return
			}
			flusher.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (fake *fakeAPI) servePatch(w http.ResponseWriter, r *http.Request, path string) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		fake.t.Errorf("read patch body: %v", err)
	}
	fake.mu.Lock()
	fake.patchBodies[path] = body
	fake.patchTypes[path] = r.Header.Get("Content-Type")
	fake.patchCount++
	response, hasResponse := fake.patchResponses[path]
	status := fake.patchStatuses[path]
	fake.mu.Unlock()

	if status != 0 {
		w.WriteHeader(status)
		return
	}
	if !hasResponse {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(response)
}

// newTestCluster builds a Cluster against the fake server. The short watch
// timeout keeps reconnects cheap; the fake streams block until Close.
func newTestCluster(t *testing.T, fake *fakeAPI) *Cluster {
	t.Helper()
	cluster, err := NewCluster(testSettings(t, fake.url()), discardLogger(),
		WithHTTPClient(fake.client()), WithWatchTimeout(50*time.Millisecond))
	if err != nil {
		t.Fatalf("NewCluster: %v", err)
	}
	cluster.Start(context.Background())
	t.Cleanup(cluster.Close)
	return cluster
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// eventually polls until condition is true (1ms interval, 2s deadline).
func eventually(t *testing.T, condition func() bool) {
	t.Helper()
	eventuallyWithin(t, 2*time.Second, condition)
}

// eventuallyWithin is the bounded variant for tests that must wait out a
// list retry backoff.
func eventuallyWithin(t *testing.T, deadline time.Duration, condition func() bool) {
	t.Helper()
	limit := time.Now().Add(deadline)
	for time.Now().Before(limit) {
		if condition() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	if !condition() {
		t.Fatalf("condition not met within %s", deadline)
	}
}

// --- JSON fixtures ---------------------------------------------------------

func metadataJSON(name, namespace, uid string, generation int64) map[string]any {
	return map[string]any{
		"name": name, "namespace": namespace, "uid": uid,
		"generation": generation, "resourceVersion": "1",
	}
}

func ownerRefJSON(kind, name, uid string) map[string]any {
	return map[string]any{
		"apiVersion": "apps/v1", "kind": kind, "name": name, "uid": uid, "controller": true,
	}
}

func testDeployment(name, namespace, uid string, generation int64) map[string]any {
	return map[string]any{
		"metadata": metadataJSON(name, namespace, uid, generation),
		"spec": map[string]any{
			"replicas": 2,
			"strategy": map[string]any{"type": "RollingUpdate"},
			"template": map[string]any{"metadata": map[string]any{"annotations": map[string]any{"app": name}}},
		},
		"status": deploymentStatusJSON(generation, 2, 2, 2, 2,
			conditionJSON("Available", "True", "", ""),
			conditionJSON("Progressing", "True", "NewReplicaSetAvailable", "")),
	}
}

func deploymentStatusJSON(observed int64, replicas, updated, ready, available int32, conditions ...map[string]any) map[string]any {
	status := map[string]any{
		"observedGeneration": observed,
		"replicas":           replicas,
		"updatedReplicas":    updated,
		"readyReplicas":      ready,
		"availableReplicas":  available,
	}
	if len(conditions) > 0 {
		status["conditions"] = conditions
	}
	return status
}

func conditionJSON(conditionType, status, reason, message string) map[string]any {
	return map[string]any{"type": conditionType, "status": status, "reason": reason, "message": message}
}

func testReplicaSet(name, namespace, uid, ownerName, ownerUID string) map[string]any {
	return map[string]any{
		"metadata": map[string]any{
			"name": name, "namespace": namespace, "uid": uid, "generation": 1, "resourceVersion": "1",
			"ownerReferences": []any{ownerRefJSON("Deployment", ownerName, ownerUID)},
		},
		"spec": map[string]any{"replicas": 2},
		"status": map[string]any{
			"observedGeneration": 1, "replicas": 2, "readyReplicas": 2, "availableReplicas": 2,
		},
	}
}

func testStatefulSet(name, namespace, uid string) map[string]any {
	return map[string]any{
		"metadata": metadataJSON(name, namespace, uid, 1),
		"spec": map[string]any{
			"replicas":       1,
			"updateStrategy": map[string]any{"type": "RollingUpdate"},
		},
		"status": map[string]any{
			"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "updatedReplicas": 1,
			"availableReplicas": 1, "currentRevision": name + "-1", "updateRevision": name + "-1",
		},
	}
}

func testDaemonSet(name, namespace, uid string) map[string]any {
	return map[string]any{
		"metadata": metadataJSON(name, namespace, uid, 1),
		"spec":     map[string]any{"updateStrategy": map[string]any{"type": "RollingUpdate"}},
		"status": map[string]any{
			"observedGeneration": 1, "desiredNumberScheduled": 1, "currentNumberScheduled": 1,
			"updatedNumberScheduled": 1, "numberReady": 1, "numberAvailable": 1,
		},
	}
}

func testReplicationController(name, namespace, uid string) map[string]any {
	return map[string]any{
		"metadata": metadataJSON(name, namespace, uid, 1),
		"spec":     map[string]any{"replicas": 1},
		"status":   map[string]any{"observedGeneration": 1, "replicas": 1, "readyReplicas": 1, "availableReplicas": 1},
	}
}

func testCronJob(name, namespace, uid string) map[string]any {
	return map[string]any{
		"metadata": metadataJSON(name, namespace, uid, 1),
		"spec":     map[string]any{"schedule": "*/5 * * * *", "suspend": false},
		"status":   map[string]any{"active": []any{map[string]any{}}},
	}
}

func testJob(name, namespace, uid, ownerName, ownerUID string) map[string]any {
	metadata := metadataJSON(name, namespace, uid, 1)
	if ownerUID != "" {
		metadata["ownerReferences"] = []any{ownerRefJSON("CronJob", ownerName, ownerUID)}
	}
	return map[string]any{
		"metadata": metadata,
		"spec":     map[string]any{"completions": 1},
		"status":   map[string]any{"observedGeneration": 1, "active": 1},
	}
}

func testPod(name, namespace, uid, node, phase string, ownerKind, ownerName, ownerUID string) map[string]any {
	metadata := metadataJSON(name, namespace, uid, 1)
	if ownerUID != "" {
		metadata["ownerReferences"] = []any{ownerRefJSON(ownerKind, ownerName, ownerUID)}
	}
	return map[string]any{
		"metadata": metadata,
		"spec": map[string]any{
			"nodeName":   node,
			"containers": []any{map[string]any{"name": "main"}},
		},
		"status": map[string]any{
			"phase": phase,
			"containerStatuses": []any{
				map[string]any{"name": "main", "ready": true, "restartCount": 0, "state": map[string]any{"running": map[string]any{}}},
			},
		},
	}
}

// restartAnnotationFromPatch decodes the merge-patch body and returns the
// timestamp annotation, asserting the body contains nothing else.
func restartAnnotationFromPatch(t *testing.T, body []byte) string {
	t.Helper()
	var patch struct {
		Spec map[string]json.RawMessage `json:"spec"`
	}
	if err := json.Unmarshal(body, &patch); err != nil {
		t.Fatalf("patch body is not JSON: %v (%s)", err, body)
	}
	if len(patch.Spec) != 1 {
		t.Fatalf("patch spec keys = %v, want only template", patch.Spec)
	}
	templateRaw, ok := patch.Spec["template"]
	if !ok {
		t.Fatalf("patch spec = %v, want template", patch.Spec)
	}
	var template struct {
		Metadata struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"metadata"`
	}
	if err := json.Unmarshal(templateRaw, &template); err != nil {
		t.Fatalf("patch template: %v", err)
	}
	if len(template.Metadata.Annotations) != 1 {
		t.Fatalf("annotations = %v, want exactly the restart annotation", template.Metadata.Annotations)
	}
	value, ok := template.Metadata.Annotations[restartAnnotation]
	if !ok {
		t.Fatalf("annotations = %v, missing %s", template.Metadata.Annotations, restartAnnotation)
	}
	if _, err := time.Parse(time.RFC3339, value); err != nil {
		t.Fatalf("restart timestamp %q is not RFC3339: %v", value, err)
	}
	return value
}
