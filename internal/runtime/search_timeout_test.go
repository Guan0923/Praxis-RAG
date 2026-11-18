package runtime

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/retrieval"
)

// blockingRetriever blocks until the retrieval context is done, then returns the
// context error. It proves the synchronous search budget (and a client
// disconnect) reaches the retrieval layer instead of being swallowed above it.
type blockingRetriever struct {
	observed chan struct{}
}

func (b *blockingRetriever) Search(ctx context.Context, _ string, _ int, _ map[string]any) ([]contracts.Candidate, error) {
	<-ctx.Done()
	if b.observed != nil {
		close(b.observed)
	}
	return nil, ctx.Err()
}

// searchServiceWithRetriever builds the minimal Service/ SearchEngine pair for a
// synchronous search: a pre-populated tenant cache avoids the MySQL-backed
// tenant build, so the test needs no external service.
func searchServiceWithRetriever(t *testing.T, retriever retrieval.Retriever, timeout time.Duration) *Service {
	t.Helper()
	orchestrator := retrieval.NewOrchestrator(
		map[contracts.RetrievalMode]retrieval.Retriever{contracts.RetrievalBM25: retriever},
		map[contracts.RerankMode]retrieval.Reranker{},
		"timeout-test",
		nil,
		nil,
		nil,
	)
	engine := &SearchEngine{
		tenants: map[string]*tenantResources{
			"alpha": {
				settings:     map[string]any{},
				collection:   "knowledge_timeout_test",
				orchestrator: orchestrator,
			},
		},
	}
	return &Service{search: engine, searchTimeout: timeout}
}

func timeoutSearchRequest() contracts.SearchRequest {
	return contracts.SearchRequest{
		Query:      "evidence",
		Retrieval:  contracts.RetrievalBM25,
		Rerank:     contracts.RerankNone,
		CandidateK: 5,
		TopK:       2,
	}
}

// TestServiceSearchBudgetMapsDeadlineToRagOperationError pins the synchronous
// search budget: the derived context deadline aborts a slow retrieval and the
// failure is reported with the operational category (HTTP 503 / MCP tool error)
// instead of a generic internal error.
func TestServiceSearchBudgetMapsDeadlineToRagOperationError(t *testing.T) {
	service := searchServiceWithRetriever(t, &blockingRetriever{}, 20*time.Millisecond)

	_, err := service.Search(context.Background(), "alpha", timeoutSearchRequest())
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("Search() error = %v, want *contracts.RagOperationError", err)
	}
	if operation.Message != "Search timed out" {
		t.Fatalf("message = %q, want %q", operation.Message, "Search timed out")
	}
}

// TestServiceSearchPropagatesClientCancellation pins that a cancelled request
// context reaches the retriever and keeps its cancellation identity (it must not
// be relabelled as a timeout).
func TestServiceSearchPropagatesClientCancellation(t *testing.T) {
	observed := make(chan struct{})
	retriever := &blockingRetriever{observed: observed}
	service := searchServiceWithRetriever(t, retriever, 30*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	defer cancel()

	_, err := service.Search(ctx, "alpha", timeoutSearchRequest())
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Search() error = %v, want context.Canceled", err)
	}
	var operation *contracts.RagOperationError
	if errors.As(err, &operation) {
		t.Fatalf("cancellation must not be relabelled as %q", operation.Message)
	}
	select {
	case <-observed:
	case <-time.After(5 * time.Second):
		t.Fatal("the retrieval layer never observed the cancellation")
	}
}

// unsetEnvForTest clears one variable for the test and restores it afterwards.
func unsetEnvForTest(t *testing.T, key string) {
	t.Helper()
	previous, existed := os.LookupEnv(key)
	if err := os.Unsetenv(key); err != nil {
		t.Fatalf("os.Unsetenv(%s) error = %v", key, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	})
}

// TestSearchTimeoutFromEnvDefault pins the historical 120-second default when
// the budget variable is absent.
func TestSearchTimeoutFromEnvDefault(t *testing.T) {
	unsetEnvForTest(t, searchTimeoutEnvName)

	timeout, err := searchTimeoutFromEnv()
	if err != nil {
		t.Fatalf("searchTimeoutFromEnv() error = %v", err)
	}
	if timeout != 120*time.Second {
		t.Fatalf("timeout = %v, want 120s", timeout)
	}
}

// TestSearchTimeoutFromEnvParsesSeconds pins the positive-integer parsing.
func TestSearchTimeoutFromEnvParsesSeconds(t *testing.T) {
	t.Setenv(searchTimeoutEnvName, " 30 ")

	timeout, err := searchTimeoutFromEnv()
	if err != nil {
		t.Fatalf("searchTimeoutFromEnv() error = %v", err)
	}
	if timeout != 30*time.Second {
		t.Fatalf("timeout = %v, want 30s", timeout)
	}
}

// TestSearchTimeoutFromEnvInvalidFailsStartup pins that a malformed budget is a
// configuration error rather than a silent unbounded search.
func TestSearchTimeoutFromEnvInvalidFailsStartup(t *testing.T) {
	for _, raw := range []string{"", "abc", "0", "-5", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			t.Setenv(searchTimeoutEnvName, raw)
			_, err := searchTimeoutFromEnv()
			if err == nil {
				t.Fatalf("searchTimeoutFromEnv(%q) accepted an invalid value", raw)
			}
			if !strings.Contains(err.Error(), searchTimeoutEnvName) {
				t.Fatalf("error = %v, want it to name %s", err, searchTimeoutEnvName)
			}
		})
	}
}
