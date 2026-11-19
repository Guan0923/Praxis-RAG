package connect

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeStatusClient serves connector states and records restart requests.
type fakeStatusClient struct {
	mu        sync.Mutex
	statuses  map[string]ConnectorStatus
	errors    map[string]error
	restarted []string
}

func (f *fakeStatusClient) Status(_ context.Context, connector string) (ConnectorStatus, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err, ok := f.errors[connector]; ok {
		return ConnectorStatus{}, err
	}
	status, ok := f.statuses[connector]
	if !ok {
		return ConnectorStatus{Name: connector, ConnectorState: StateUnassigned}, nil
	}
	return status, nil
}

func (f *fakeStatusClient) RestartTask(_ context.Context, connector string, task int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.restarted = append(f.restarted, fmt.Sprintf("%s#%d", connector, task))
	return nil
}

func (f *fakeStatusClient) restartCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.restarted)
}

// setTask mutates one task state for the next tick.
func (f *fakeStatusClient) setTasks(connector string, states ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	status := f.statuses[connector]
	status.Name = connector
	if status.ConnectorState == "" {
		status.ConnectorState = StateRunning
	}
	status.Tasks = status.Tasks[:0]
	for index, state := range states {
		status.Tasks = append(status.Tasks, TaskState{ID: index, State: state, WorkerID: "connect-1:8083"})
	}
	f.statuses[connector] = status
}

// supervisorFixture builds a supervisor with both connectors registered.
func supervisorFixture(t *testing.T) (*Supervisor, *fakeStatusClient, *time.Time) {
	t.Helper()
	client := &fakeStatusClient{statuses: map[string]ConnectorStatus{}, errors: map[string]error{}}
	settings := Settings{
		DocumentConnector:  "doc-cdc",
		CacheConnector:     "cache-cdc",
		SupervisorInterval: 10 * time.Second,
		RestartMaxAttempts: 3,
		RestartBaseBackoff: time.Second,
		RestartMaxBackoff:  8 * time.Second,
		SupervisorEnabled:  true,
	}
	supervisor := NewSupervisor(client, settings, nil)
	now := time.Date(2026, 11, 15, 6, 0, 0, 0, time.UTC)
	supervisor.setClock(func() time.Time { return now })
	return supervisor, client, &now
}

func TestSupervisorRestartsOnlyFailedTaskWithBackoff(t *testing.T) {
	supervisor, client, now := supervisorFixture(t)
	client.setTasks("doc-cdc", StateRunning, StateFailed)
	supervisor.Tick(context.Background())
	if client.restartCount() != 1 || client.restarted[0] != "doc-cdc#1" {
		t.Fatalf("restarted = %v, want only the FAILED task", client.restarted)
	}
	// Within the backoff window the supervisor must not hammer the task.
	supervisor.Tick(context.Background())
	if client.restartCount() != 1 {
		t.Fatalf("restarted = %v, want no restart before the backoff expires", client.restarted)
	}
	*now = now.Add(time.Second)
	supervisor.Tick(context.Background())
	if client.restartCount() != 2 {
		t.Fatalf("restarted = %v, want the second attempt after the backoff", client.restarted)
	}
	*now = now.Add(2 * time.Second)
	supervisor.Tick(context.Background())
	if client.restartCount() != 3 {
		t.Fatalf("restarted = %v, want the third attempt", client.restarted)
	}
	// Budget exhausted: the fourth tick must not restart again.
	*now = now.Add(time.Minute)
	supervisor.Tick(context.Background())
	if client.restartCount() != 3 {
		t.Fatalf("restarted = %v, want no restart after the budget is exhausted", client.restarted)
	}
	health, err := supervisor.Health(context.Background(), "doc-cdc")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.RestartExhausted || health.RestartAttempts != 3 || health.FailedTasks != 1 ||
		health.Recovery != RecoveryExhausted {
		t.Fatalf("health = %+v", health)
	}
}

func TestSupervisorRecoversAndResetsBudgetAfterStableWindow(t *testing.T) {
	supervisor, client, now := supervisorFixture(t)
	client.setTasks("cache-cdc", StateFailed)
	supervisor.Tick(context.Background())
	if client.restartCount() != 1 {
		t.Fatalf("restarted = %v", client.restarted)
	}
	client.setTasks("cache-cdc", StateRunning)
	supervisor.Tick(context.Background())
	health, err := supervisor.Health(context.Background(), "cache-cdc")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Recovery != RecoveryRecovered || health.Restarting || health.RestartExhausted ||
		health.LastRecoveredAt == "" {
		t.Fatalf("health = %+v", health)
	}
	// Before the stability window the attempt budget is kept: a new failure
	// within the window continues the same episode and restarts again.
	*now = now.Add(5 * time.Second)
	supervisor.Tick(context.Background())
	client.setTasks("cache-cdc", StateFailed)
	*now = now.Add(time.Second)
	supervisor.Tick(context.Background())
	if client.restartCount() != 2 {
		t.Fatalf("restarted = %v, want the same-episode restart inside the window", client.restarted)
	}
	within, err := supervisor.Health(context.Background(), "cache-cdc")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if within.RestartAttempts != 2 {
		t.Fatalf("restart attempts inside the window = %d, want the kept budget of 2", within.RestartAttempts)
	}
	// The task runs again before the boundary so the episode can settle.
	client.setTasks("cache-cdc", StateRunning)
	*now = now.Add(time.Second)
	supervisor.Tick(context.Background())
	// After the window a new failure starts at attempt one again.
	*now = now.Add(30 * time.Second)
	supervisor.Tick(context.Background())
	client.setTasks("cache-cdc", StateFailed)
	*now = now.Add(time.Second)
	supervisor.Tick(context.Background())
	if client.restartCount() != 3 {
		t.Fatalf("restarted = %v, want the new-episode restart after the window", client.restarted)
	}
	health, _ = supervisor.Health(context.Background(), "cache-cdc")
	if health.RestartAttempts != 1 {
		t.Fatalf("restart attempts = %d, want the budget reset to 1", health.RestartAttempts)
	}
}

func TestSupervisorIgnoresHealthyConnector(t *testing.T) {
	supervisor, client, _ := supervisorFixture(t)
	client.setTasks("doc-cdc", StateRunning)
	client.setTasks("cache-cdc", StateRunning)
	supervisor.Tick(context.Background())
	if client.restartCount() != 0 {
		t.Fatalf("restarted = %v, want none", client.restarted)
	}
}

func TestSupervisorHealthReportsWorkerAndFailure(t *testing.T) {
	supervisor, client, _ := supervisorFixture(t)
	client.mu.Lock()
	client.statuses["doc-cdc"] = ConnectorStatus{
		Name:            "doc-cdc",
		ConnectorState:  StateRunning,
		ConnectorWorker: "connect-1:8083",
		Tasks: []TaskState{{
			ID: 0, State: StateFailed, WorkerID: "connect-1:8083",
			Trace: "Access denied for user 'debezium'",
		}},
	}
	client.mu.Unlock()
	health, err := supervisor.Health(context.Background(), "doc-cdc")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if health.Connector != "doc-cdc" || health.State != StateRunning ||
		health.WorkerID != "connect-1:8083" || health.FailedTasks != 1 ||
		health.Tasks[0].WorkerID != "connect-1:8083" ||
		!contains(health.FailureReason, "Access denied") || !health.SupervisorEnabled {
		t.Fatalf("health = %+v", health)
	}
}

// contains is a tiny substring helper for the health assertions.
func contains(text, want string) bool {
	for index := 0; index+len(want) <= len(text); index++ {
		if text[index:index+len(want)] == want {
			return true
		}
	}
	return false
}

func TestSupervisorBackoffDoublesAndCaps(t *testing.T) {
	supervisor, _, _ := supervisorFixture(t)
	cases := map[int]time.Duration{
		1: time.Second,
		2: 2 * time.Second,
		3: 4 * time.Second,
		4: 8 * time.Second,
		5: 8 * time.Second,
		9: 8 * time.Second,
	}
	for attempt, want := range cases {
		if got := supervisor.backoff(attempt); got != want {
			t.Fatalf("backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestSupervisorStatusErrorDoesNotRestart(t *testing.T) {
	supervisor, client, _ := supervisorFixture(t)
	client.mu.Lock()
	client.errors["doc-cdc"] = fmt.Errorf("worker unreachable")
	client.mu.Unlock()
	supervisor.Tick(context.Background())
	if client.restartCount() != 0 {
		t.Fatalf("restarted = %v, want none when the status is unreadable", client.restarted)
	}
}

// TestSupervisorHealthMergesCachedStatusOnReadFailure pins the degraded
// merge: when the REST status read fails after a healthy observation, Health
// returns the last observed status with a read-failure reason instead of an
// empty placeholder.
func TestSupervisorHealthMergesCachedStatusOnReadFailure(t *testing.T) {
	supervisor, client, _ := supervisorFixture(t)
	client.setTasks("doc-cdc", StateRunning)
	supervisor.Tick(context.Background())
	client.mu.Lock()
	client.errors["doc-cdc"] = errors.New("connection refused")
	client.mu.Unlock()

	health, err := supervisor.Health(context.Background(), "doc-cdc")
	if err == nil || !contains(err.Error(), "connection refused") {
		t.Fatalf("Health() error = %v, want the propagated status failure", err)
	}
	if health.Connector != "doc-cdc" || health.State != StateRunning ||
		!health.SupervisorEnabled ||
		!contains(health.FailureReason, "状态读取失败") ||
		!contains(health.FailureReason, "connection refused") {
		t.Fatalf("health = %+v, want the cached RUNNING status with the read-failure reason", health)
	}
	if len(health.Tasks) != 1 || health.Tasks[0].State != StateRunning {
		t.Fatalf("health tasks = %+v, want the cached running task", health.Tasks)
	}
}
