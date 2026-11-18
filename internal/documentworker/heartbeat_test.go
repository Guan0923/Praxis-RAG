package documentworker

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// heartbeatCall is one heartbeat observed by the test double.
type heartbeatCall struct {
	role     string
	instance string
	now      time.Time
}

// recordingHeartbeat is a HeartbeatRecorder test double. Calls carrying an
// already-canceled context are not counted, so heartbeat traffic that arrives
// after stop() stays observable.
type recordingHeartbeat struct {
	mutex sync.Mutex
	calls []heartbeatCall
	fail  bool
}

func (r *recordingHeartbeat) Record(ctx context.Context, role, instance string, now time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	r.mutex.Lock()
	defer r.mutex.Unlock()
	if r.fail {
		return errors.New("state database unavailable")
	}
	r.calls = append(r.calls, heartbeatCall{role: role, instance: instance, now: now})
	return nil
}

func (r *recordingHeartbeat) setFailure(fail bool) {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	r.fail = fail
}

func (r *recordingHeartbeat) snapshot() []heartbeatCall {
	r.mutex.Lock()
	defer r.mutex.Unlock()
	return append([]heartbeatCall(nil), r.calls...)
}

// waitForHeartbeats polls until at least want heartbeats were recorded.
func waitForHeartbeats(recorder *recordingHeartbeat, want int, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if len(recorder.snapshot()) >= want {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return len(recorder.snapshot()) >= want
}

// TestStartHeartbeatRecordsImmediatelyAndKeepsTicking asserts the liveness row
// is written before the first interval elapses and then refreshed periodically.
func TestStartHeartbeatRecordsImmediatelyAndKeepsTicking(t *testing.T) {
	recorder := &recordingHeartbeat{}
	stop := StartHeartbeat(context.Background(), recorder,
		state.HeartbeatRoleDocumentWorker, 10*time.Millisecond, testLogger())
	defer stop()

	calls := recorder.snapshot()
	if len(calls) != 1 {
		t.Fatalf("heartbeats right after start = %d, want 1", len(calls))
	}
	first := calls[0]
	if first.role != state.HeartbeatRoleDocumentWorker {
		t.Fatalf("heartbeat role = %q, want %q", first.role, state.HeartbeatRoleDocumentWorker)
	}
	if first.instance == "" || !strings.Contains(first.instance, "-") {
		t.Fatalf("heartbeat instance = %q, want a hostname-pid identity", first.instance)
	}
	if age := time.Since(first.now); age < 0 || age > time.Minute {
		t.Fatalf("heartbeat time age = %v, want the current time", age)
	}
	if !waitForHeartbeats(recorder, 3, 2*time.Second) {
		t.Fatalf("heartbeats = %d after waiting, want at least 3", len(recorder.snapshot()))
	}

	stop()
	stopped := len(recorder.snapshot())
	time.Sleep(50 * time.Millisecond)
	if got := len(recorder.snapshot()); got != stopped {
		t.Fatalf("heartbeats after stop = %d, want %d", got, stopped)
	}
}

// TestStartHeartbeatStopsWithContext asserts canceling the context ends the
// background ticker even when stop is called afterwards.
func TestStartHeartbeatStopsWithContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	recorder := &recordingHeartbeat{}
	stop := StartHeartbeat(ctx, recorder,
		state.HeartbeatRoleDocumentWorker, 10*time.Millisecond, testLogger())
	defer stop()

	if !waitForHeartbeats(recorder, 2, 2*time.Second) {
		t.Fatalf("heartbeats = %d, want at least 2", len(recorder.snapshot()))
	}
	cancel()
	stop()
	stopped := len(recorder.snapshot())
	time.Sleep(50 * time.Millisecond)
	if got := len(recorder.snapshot()); got != stopped {
		t.Fatalf("heartbeats after cancellation = %d, want %d", got, stopped)
	}
}

// TestStartHeartbeatRecoversFromWriteFailure asserts a failed heartbeat write is
// logged and the next interval still tries again.
func TestStartHeartbeatRecoversFromWriteFailure(t *testing.T) {
	recorder := &recordingHeartbeat{fail: true}
	stop := StartHeartbeat(context.Background(), recorder,
		state.HeartbeatRoleDocumentWorker, 5*time.Millisecond, testLogger())
	defer stop()

	if got := len(recorder.snapshot()); got != 0 {
		t.Fatalf("recorded heartbeats while failing = %d, want 0", got)
	}
	recorder.setFailure(false)
	if !waitForHeartbeats(recorder, 1, 2*time.Second) {
		t.Fatal("heartbeat did not resume after the write failure cleared")
	}
}

// TestStartHeartbeatNilRecorder asserts the disabled wiring is a safe no-op.
func TestStartHeartbeatNilRecorder(t *testing.T) {
	stop := StartHeartbeat(context.Background(), nil,
		state.HeartbeatRoleDocumentWorker, time.Hour, nil)
	stop()
	stop()
}

// TestDefaultWorkerHeartbeatInterval asserts the 15 second default and the
// RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS override, including invalid fallbacks.
func TestDefaultWorkerHeartbeatInterval(t *testing.T) {
	saved, present := os.LookupEnv(WorkerHeartbeatEnv)
	_ = os.Unsetenv(WorkerHeartbeatEnv)
	t.Cleanup(func() {
		if present {
			_ = os.Setenv(WorkerHeartbeatEnv, saved)
			return
		}
		_ = os.Unsetenv(WorkerHeartbeatEnv)
	})

	if got := DefaultWorkerHeartbeatInterval(); got != 15*time.Second {
		t.Fatalf("default interval = %v, want 15s", got)
	}
	for _, testCase := range []struct {
		raw  string
		want time.Duration
	}{
		{"30", 30 * time.Second},
		{"0.25", 250 * time.Millisecond},
	} {
		t.Setenv(WorkerHeartbeatEnv, testCase.raw)
		if got := DefaultWorkerHeartbeatInterval(); got != testCase.want {
			t.Fatalf("interval(%q) = %v, want %v", testCase.raw, got, testCase.want)
		}
	}
	for _, raw := range []string{"abc", "", "0", "-5", "NaN"} {
		t.Setenv(WorkerHeartbeatEnv, raw)
		if got := DefaultWorkerHeartbeatInterval(); got != 15*time.Second {
			t.Fatalf("interval(%q) = %v, want the 15s default", raw, got)
		}
	}
}

// TestWorkerInstanceID asserts the hostname-pid shape and the column width.
func TestWorkerInstanceID(t *testing.T) {
	id := WorkerInstanceID()
	if id == "" || !strings.Contains(id, "-") {
		t.Fatalf("WorkerInstanceID() = %q, want a hostname-pid identity", id)
	}
	if runes := utf8.RuneCountInString(id); runes > maxWorkerInstanceLength {
		t.Fatalf("WorkerInstanceID() has %d characters, want at most %d",
			runes, maxWorkerInstanceLength)
	}
	if again := WorkerInstanceID(); again != id {
		t.Fatalf("WorkerInstanceID() = %q then %q, want a stable identity", id, again)
	}
}
