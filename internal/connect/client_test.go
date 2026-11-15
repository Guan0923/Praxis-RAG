package connect

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// statusDocument builds one Kafka Connect status payload.
func statusDocument(name, connectorState, worker string, tasks ...[2]string) string {
	taskJSON := make([]string, 0, len(tasks))
	for index, task := range tasks {
		taskJSON = append(taskJSON, `{"id":`+itoa(index)+`,"state":"`+task[0]+`","worker_id":"`+task[1]+`"}`)
	}
	return `{"name":"` + name + `","connector":{"state":"` + connectorState +
		`","worker_id":"` + worker + `"},"tasks":[` + strings.Join(taskJSON, ",") + `]}`
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

// fakeConnect is a minimal Kafka Connect REST double.
type fakeConnect struct {
	mu          sync.Mutex
	status      map[string]string
	configs     map[string]map[string]string
	plugins     []Plugin
	restarted   []string
	paused      []string
	putCount    int
	statusCalls int
}

func newFakeConnect() *fakeConnect {
	return &fakeConnect{
		status:  map[string]string{},
		configs: map[string]map[string]string{},
	}
}

func (f *fakeConnect) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	path := r.URL.Path
	switch {
	case r.Method == http.MethodGet && path == "/connector-plugins":
		writeJSON(w, f.plugins)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/connectors/") && strings.HasSuffix(path, "/status"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/connectors/"), "/status")
		document, ok := f.status[name]
		f.statusCalls++
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		_, _ = io.WriteString(w, document)
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/connectors/") && strings.HasSuffix(path, "/config"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/connectors/"), "/config")
		config, ok := f.configs[name]
		if !ok {
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
			return
		}
		writeJSON(w, config)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/config"):
		name := strings.TrimSuffix(strings.TrimPrefix(path, "/connectors/"), "/config")
		var config map[string]string
		if err := json.NewDecoder(r.Body).Decode(&config); err != nil {
			http.Error(w, `{"message":"bad config"}`, http.StatusBadRequest)
			return
		}
		f.configs[name] = config
		f.putCount++
		writeJSON(w, map[string]any{"name": name, "config": config})
	case r.Method == http.MethodPost && strings.Contains(path, "/tasks/"):
		f.restarted = append(f.restarted, path)
		w.WriteHeader(http.StatusAccepted)
	case r.Method == http.MethodPut && strings.HasSuffix(path, "/pause"):
		f.paused = append(f.paused, strings.TrimSuffix(strings.TrimPrefix(path, "/connectors/"), "/pause"))
		w.WriteHeader(http.StatusAccepted)
	default:
		http.Error(w, `{"message":"unexpected "`+r.Method+` `+path+`"}`, http.StatusNotFound)
	}
}

// writeJSON writes a compact JSON response.
func writeJSON(w http.ResponseWriter, payload any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(payload)
}

// startFakeConnect serves the double and returns its URL.
func startFakeConnect(t *testing.T) (*fakeConnect, *httptest.Server) {
	t.Helper()
	fake := newFakeConnect()
	server := httptest.NewServer(fake)
	t.Cleanup(server.Close)
	return fake, server
}

func TestClientStatusDecodesTasksAndBoundedTrace(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.status["doc"] = statusDocument("doc", "RUNNING", "connect-1:8083",
		[2]string{"RUNNING", "connect-1:8083"}, [2]string{"FAILED", "connect-2:8083"})
	client, err := NewClient([]string{server.URL}, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	status, err := client.Status(context.Background(), "doc")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.Name != "doc" || status.ConnectorState != StateRunning ||
		status.ConnectorWorker != "connect-1:8083" || len(status.Tasks) != 2 ||
		status.Tasks[1].State != StateFailed || status.Tasks[1].WorkerID != "connect-2:8083" {
		t.Fatalf("status = %+v", status)
	}
}

func TestClientStatusNormalizesMissingState(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.status["doc"] = `{"name":"doc","connector":{},"tasks":[{"id":0}]}`
	client, _ := NewClient([]string{server.URL}, time.Second)
	status, err := client.Status(context.Background(), "doc")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if status.ConnectorState != StateUnknown || status.Tasks[0].State != StateUnknown {
		t.Fatalf("states = %q/%q", status.ConnectorState, status.Tasks[0].State)
	}
}

func TestClientFailsOverToReachableWorker(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.status["doc"] = statusDocument("doc", "RUNNING", "connect-2:8083", [2]string{"RUNNING", "connect-2:8083"})
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()
	client, err := NewClient([]string{deadURL, server.URL}, 500*time.Millisecond)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	status, err := client.Status(context.Background(), "doc")
	if err != nil {
		t.Fatalf("Status with a dead worker: %v", err)
	}
	if status.ConnectorState != StateRunning {
		t.Fatalf("status = %+v", status)
	}
	// The successful worker is remembered; the next call starts there.
	before := fake.statusCalls
	if _, err := client.Status(context.Background(), "doc"); err != nil {
		t.Fatalf("second Status: %v", err)
	}
	if fake.statusCalls != before+1 {
		t.Fatalf("statusCalls = %d, want %d", fake.statusCalls, before+1)
	}
}

func TestClientUpsertCreatesUpdatesAndSkipsUnchanged(t *testing.T) {
	fake, server := startFakeConnect(t)
	client, _ := NewClient([]string{server.URL}, time.Second)
	config := map[string]string{"connector.class": MySQLConnectorClass, "database.hostname": "mysql"}

	result, err := client.Upsert(context.Background(), "doc", config)
	if err != nil {
		t.Fatalf("Upsert create: %v", err)
	}
	if !result.Created || result.Updated {
		t.Fatalf("first upsert = %+v", result)
	}
	result, err = client.Upsert(context.Background(), "doc", config)
	if err != nil {
		t.Fatalf("Upsert unchanged: %v", err)
	}
	if result.Created || result.Updated {
		t.Fatalf("unchanged upsert = %+v", result)
	}
	if fake.putCount != 1 {
		t.Fatalf("putCount = %d, want 1 (an unchanged config must not bounce the connector)", fake.putCount)
	}

	updated := map[string]string{"connector.class": MySQLConnectorClass, "database.hostname": "mysql-2"}
	result, err = client.Upsert(context.Background(), "doc", updated)
	if err != nil {
		t.Fatalf("Upsert update: %v", err)
	}
	if result.Created || !result.Updated {
		t.Fatalf("updated upsert = %+v", result)
	}
	if fake.putCount != 2 {
		t.Fatalf("putCount = %d, want 2", fake.putCount)
	}
}

func TestClientUpsertRetriesFollowerConflict(t *testing.T) {
	leader, leaderServer := startFakeConnect(t)
	conflicts := 0
	follower := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/config"):
			http.Error(w, `{"message":"not found"}`, http.StatusNotFound)
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/config"):
			conflicts++
			http.Error(w, `{"message":"this worker is not the leader"}`, http.StatusConflict)
		default:
			http.Error(w, `{"message":"unexpected"}`, http.StatusNotFound)
		}
	}))
	t.Cleanup(follower.Close)

	// The follower is first in failover order; a 409 on PUT must not end the
	// registration before the leader gets a chance.
	client, err := NewClient([]string{follower.URL, leaderServer.URL}, time.Second)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	result, err := client.Upsert(context.Background(), "doc", map[string]string{
		"connector.class": MySQLConnectorClass, "database.hostname": "mysql",
	})
	if err != nil {
		t.Fatalf("Upsert through a follower: %v", err)
	}
	if !result.Created || conflicts != 1 {
		t.Fatalf("result = %+v, conflicts = %d", result, conflicts)
	}
	if leader.putCount != 1 {
		t.Fatalf("leader putCount = %d, want 1", leader.putCount)
	}
}

func TestClientRestartTaskAndPausePaths(t *testing.T) {
	fake, server := startFakeConnect(t)
	client, _ := NewClient([]string{server.URL}, time.Second)
	if err := client.RestartTask(context.Background(), "doc", 3); err != nil {
		t.Fatalf("RestartTask: %v", err)
	}
	if len(fake.restarted) != 1 || !strings.Contains(fake.restarted[0], "/connectors/doc/tasks/3/restart") {
		t.Fatalf("restarted = %v", fake.restarted)
	}
	if err := client.Pause(context.Background(), "cache"); err != nil {
		t.Fatalf("Pause: %v", err)
	}
	if len(fake.paused) != 1 || fake.paused[0] != "cache" {
		t.Fatalf("paused = %v", fake.paused)
	}
}

func TestClientNotFoundIsTyped(t *testing.T) {
	_, server := startFakeConnect(t)
	client, _ := NewClient([]string{server.URL}, time.Second)
	_, err := client.Status(context.Background(), "missing")
	if !errors.Is(err, ErrConnectorNotFound) {
		t.Fatalf("Status error = %v, want ErrConnectorNotFound", err)
	}
}

func TestClientSanitizesPasswordFromAPIError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, `{"message":"Invalid value database.password=super-secret for configuration"}`, http.StatusBadRequest)
	}))
	defer server.Close()
	client, _ := NewClient([]string{server.URL}, time.Second)
	_, err := client.ConnectorNames(context.Background())
	if err == nil {
		t.Fatal("expected an API error")
	}
	if strings.Contains(err.Error(), "super-secret") {
		t.Fatalf("error leaked the password: %v", err)
	}
	if !strings.Contains(err.Error(), "database.password=***") {
		t.Fatalf("error = %v, want a redacted marker", err)
	}
}

func TestVerifyConnectorPluginsRequiresEveryWorker(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.plugins = []Plugin{
		{Class: "org.apache.kafka.connect.mirror.MirrorSourceConnector", Type: "source", Version: "3.9.0"},
		{Class: MySQLConnectorClass, Type: "source", Version: "3.7.1.Final"},
	}
	dead := httptest.NewServer(http.NotFoundHandler())
	deadURL := dead.URL
	dead.Close()

	client, _ := NewClient([]string{server.URL, deadURL}, time.Second)
	if _, err := VerifyConnectorPlugins(context.Background(), client, "3.7.1.Final"); err == nil ||
		!strings.Contains(err.Error(), deadURL) {
		t.Fatalf("partial inventory error = %v, want the unreachable worker named", err)
	}

	client, _ = NewClient([]string{server.URL}, time.Second)
	versions, err := VerifyConnectorPlugins(context.Background(), client, "3.7.1.Final")
	if err != nil {
		t.Fatalf("VerifyConnectorPlugins: %v", err)
	}
	if versions[server.URL] != "3.7.1.Final" {
		t.Fatalf("versions = %v", versions)
	}
	if _, err := VerifyConnectorPlugins(context.Background(), client, "3.9.0"); err == nil {
		t.Fatal("expected a version mismatch error")
	}
	fake.plugins = fake.plugins[:1]
	if _, err := VerifyConnectorPlugins(context.Background(), client, ""); err == nil ||
		!strings.Contains(err.Error(), MySQLConnectorClass) {
		t.Fatalf("missing plugin error = %v", err)
	}
}

func TestParseURLsDropsDuplicatesAndRejectsBadSchemes(t *testing.T) {
	urls, err := ParseURLs("http://127.0.0.1:8083, http://127.0.0.1:8083, https://connect-2:8083/")
	if err != nil {
		t.Fatalf("ParseURLs: %v", err)
	}
	if len(urls) != 2 || urls[0] != "http://127.0.0.1:8083" || urls[1] != "https://connect-2:8083" {
		t.Fatalf("urls = %v", urls)
	}
	if _, err := ParseURLs("ftp://connect:8083"); err == nil {
		t.Fatal("expected scheme validation error")
	}
	if _, err := ParseURLs("not a url"); err == nil {
		t.Fatal("expected URL validation error")
	}
}
