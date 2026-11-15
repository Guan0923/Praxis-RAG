package connect

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestRuntimeDisabledWithoutURLs(t *testing.T) {
	runtime, err := NewRuntime(Settings{DocumentConnector: "doc", CacheConnector: "cache"}, nil)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if runtime.Enabled() || runtime.Supervisor() != nil {
		t.Fatalf("runtime = %+v, want disabled", runtime)
	}
	if _, err := runtime.Health(context.Background(), "doc"); err != ErrNotConfigured {
		t.Fatalf("Health error = %v, want ErrNotConfigured", err)
	}
	// Start/Close are safe no-ops when the integration is disabled.
	runtime.Start(context.Background())
	runtime.Close()
}

func TestRuntimeHealthMergesSupervisorState(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.status["doc"] = statusDocument("doc", "RUNNING", "connect-1:8083",
		[2]string{"FAILED", "connect-1:8083"})
	settings := Settings{
		URLs:               []string{server.URL},
		DocumentConnector:  "doc",
		CacheConnector:     "cache",
		Timeout:            time.Second,
		SupervisorEnabled:  true,
		SupervisorInterval: time.Minute,
		RestartMaxAttempts: 2,
		RestartBaseBackoff: time.Second,
		RestartMaxBackoff:  time.Minute,
	}
	runtime, err := NewRuntime(settings, nil)
	if err != nil {
		t.Fatalf("NewRuntime: %v", err)
	}
	if runtime.Supervisor() == nil {
		t.Fatal("supervisor is nil for an enabled runtime")
	}
	health, err := runtime.Health(context.Background(), "doc")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.SupervisorEnabled || health.FailedTasks != 1 || health.Tasks[0].State != StateFailed {
		t.Fatalf("health = %+v", health)
	}
}

func TestWaitForReadySucceedsForBothConnectors(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.status["doc"] = statusDocument("doc", "RUNNING", "connect-1:8083", [2]string{"RUNNING", "connect-1:8083"})
	fake.status["cache"] = statusDocument("cache", "RUNNING", "connect-2:8083", [2]string{"RUNNING", "connect-2:8083"})
	client, _ := NewClient([]string{server.URL}, time.Second)
	if err := WaitForReady(context.Background(), client, []string{"doc", "cache"}, 2*time.Second, 10*time.Millisecond); err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
}

func TestWaitForReadyReportsFailedTask(t *testing.T) {
	fake, server := startFakeConnect(t)
	fake.status["doc"] = statusDocument("doc", "RUNNING", "connect-1:8083", [2]string{"FAILED", "connect-1:8083"})
	fake.status["cache"] = statusDocument("cache", "RUNNING", "connect-2:8083", [2]string{"RUNNING", "connect-2:8083"})
	client, _ := NewClient([]string{server.URL}, time.Second)
	err := WaitForReady(context.Background(), client, []string{"doc", "cache"}, 200*time.Millisecond, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "task 0") || !strings.Contains(err.Error(), "FAILED") {
		t.Fatalf("WaitForReady error = %v", err)
	}
}

func TestWaitForReadyReportsMissingConnector(t *testing.T) {
	_, server := startFakeConnect(t)
	client, _ := NewClient([]string{server.URL}, time.Second)
	err := WaitForReady(context.Background(), client, []string{"missing"}, 100*time.Millisecond, 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("WaitForReady error = %v", err)
	}
}
