package connect

import (
	"context"
	"testing"
	"time"
)

// testSupervisorSettings mirrors supervisorFixture's settings.
func testSupervisorSettings() Settings {
	return Settings{
		DocumentConnector:  "praxis-document-cdc",
		CacheConnector:     "praxis-cache-cdc",
		SupervisorInterval: 10 * time.Second,
		RestartMaxAttempts: 3,
		RestartBaseBackoff: time.Second,
		RestartMaxBackoff:  8 * time.Second,
		SupervisorEnabled:  true,
	}
}

// TestResetRestartBudgetClearsExhaustedEpisodes proves the Console restart
// action can recover a task the supervisor gave up on: after exhausting the
// budget, ResetRestartBudget clears the episode so the next FAILED
// observation starts a fresh budget instead of staying exhausted.
func TestResetRestartBudgetClearsExhaustedEpisodes(t *testing.T) {
	client := &fakeStatusClient{statuses: map[string]ConnectorStatus{
		"praxis-document-cdc": {Name: "praxis-document-cdc", ConnectorState: StateRunning, Tasks: []TaskState{{ID: 0, State: StateFailed, WorkerID: "w1"}}},
	}, errors: map[string]error{}}
	supervisor := NewSupervisor(client, testSupervisorSettings(), nil)

	// Drive one real exhausted episode: attempt 1 at t, then advance the fake
	// clock past each backoff deadline so attempts 2 and 3 fire; the fourth
	// observation must leave the episode exhausted.
	clock := time.Date(2026, 11, 17, 8, 0, 0, 0, time.UTC)
	supervisor.setClock(func() time.Time { return clock })
	supervisor.Tick(context.Background()) // attempt 1 (backoff 1s)
	clock = clock.Add(2 * time.Second)
	supervisor.Tick(context.Background()) // attempt 2 (backoff 2s)
	clock = clock.Add(4 * time.Second)
	supervisor.Tick(context.Background()) // attempt 3 (budget spent)
	clock = clock.Add(60 * time.Second)
	supervisor.Tick(context.Background()) // exhausted: no further restart
	health, err := supervisor.Health(context.Background(), "praxis-document-cdc")
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if !health.RestartExhausted || health.Recovery != RecoveryExhausted {
		t.Fatalf("health before reset = %+v", health)
	}

	key := taskKey{connector: "praxis-document-cdc", task: 0}
	supervisor.ResetRestartBudget("praxis-document-cdc")

	supervisor.mu.Lock()
	cleared, exists := supervisor.tasks[key]
	supervisor.mu.Unlock()
	if !exists || cleared.attempts != 0 || cleared.exhausted {
		t.Fatalf("episode after reset = %+v (exists=%v)", cleared, exists)
	}

	// After the reset the next tick restarts the still-FAILED task again
	// (fresh budget, first attempt) instead of staying exhausted.
	clock = clock.Add(60 * time.Second)
	supervisor.Tick(context.Background())
	if client.restartCount() != 4 || client.restarted[3] != "praxis-document-cdc#0" {
		t.Fatalf("restarted = %v, want a fresh-budget restart after reset", client.restarted)
	}

	// The cache connector's episode is untouched by the document reset.
	cacheKey := taskKey{connector: "praxis-cache-cdc", task: 0}
	supervisor.mu.Lock()
	supervisor.tasks[cacheKey] = &restartState{attempts: 3, exhausted: true}
	supervisor.mu.Unlock()
	supervisor.ResetRestartBudget("praxis-document-cdc")
	supervisor.mu.Lock()
	untouched := supervisor.tasks[cacheKey]
	supervisor.mu.Unlock()
	if untouched.attempts != 3 || !untouched.exhausted {
		t.Fatalf("cache episode was modified: %+v", untouched)
	}

	// An unknown connector name is a no-op, not an error.
	supervisor.ResetRestartBudget("other-connector")
}

// TestResetRestartBudgetUnknownConnectorKeepsEpisodes is the degenerate-input
// guard: resetting a name that never failed must not clear anything.
func TestResetRestartBudgetUnknownConnectorKeepsEpisodes(t *testing.T) {
	supervisor := NewSupervisor(&fakeStatusClient{}, testSupervisorSettings(), nil)
	supervisor.mu.Lock()
	supervisor.tasks[taskKey{connector: "praxis-document-cdc", task: 0}] =
		&restartState{attempts: 2, exhausted: true}
	before := len(supervisor.tasks)
	supervisor.mu.Unlock()

	supervisor.ResetRestartBudget("missing-connector")

	supervisor.mu.Lock()
	after := len(supervisor.tasks)
	supervisor.mu.Unlock()
	if after != before {
		t.Fatalf("episodes changed: %d -> %d", before, after)
	}
}
