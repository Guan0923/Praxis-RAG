package runtime

import (
	"context"
	"errors"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// searchSubmissionFailedMessage is the safe fail-closed message when the
// accepted search task cannot be persisted. Acceptance is a MySQL commit; it
// does not depend on the broker.
const searchSubmissionFailedMessage = "Search submission unavailable"

// searchWaitPollInterval mirrors rag_mcp.mcp_http.search_queued's 0.1s poll.
const searchWaitPollInterval = 100 * time.Millisecond

// searchWaitMinimum/maximum mirror QueuedSearchRequest.wait_seconds bounds.
const (
	searchWaitMinimum = 0.0
	searchWaitMaximum = 30.0
)

// searchArguments renders the validated request as the queue payload
// (dataclasses.asdict(SearchRequest)); filters are always an object because
// SearchRequestFromArguments normalizes them.
func searchArguments(request contracts.SearchRequest) map[string]any {
	filters := request.Filters
	if filters == nil {
		filters = map[string]any{}
	}
	return map[string]any{
		"query":       request.Query,
		"retrieval":   string(request.Retrieval),
		"rerank":      string(request.Rerank),
		"candidate_k": request.CandidateK,
		"top_k":       request.TopK,
		"filters":     filters,
	}
}

// submitSearchTask is the admission half behind Service.SubmitSearch. It commits
// the durable search_tasks row (arguments, deadline, queued, schedule_version=1)
// and returns the result id. Debezium picks the committed row up and routes an
// identifier-only notification to the search consumer; a broker outage does not
// change acceptance, the task simply stays queued until delivery resumes.
func (s *Service) submitSearchTask(
	ctx context.Context,
	namespace string,
	request contracts.SearchRequest,
) (string, error) {
	if s.stores.SearchTasks == nil {
		return "", &apierrors.KnowledgeError{Message: searchSubmissionFailedMessage}
	}
	notification, err := s.stores.SearchTasks.Submit(ctx, namespace, searchArguments(request), nil)
	if err != nil {
		return "", searchSubmissionError(err)
	}
	return notification.ResultID, nil
}

// searchSubmissionError mirrors service_knowledge.submit_search: knowledge and
// snapshot errors pass through, everything else becomes the safe 503 message.
func searchSubmissionError(err error) error {
	if err == nil {
		return nil
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return knowledge
	}
	var unavailable *apierrors.SnapshotUnavailable
	if errors.As(err, &unavailable) {
		return unavailable
	}
	return &apierrors.KnowledgeError{Message: searchSubmissionFailedMessage}
}

// submitSearch implements the server-side queued search call: admission plus a
// bounded wait (rag_mcp.mcp_http.search_queued). When the deadline passes while
// the task is still queued only {"result_id": ...} is returned.
func (s *Service) submitSearch(
	ctx context.Context,
	namespace string,
	request contracts.SearchRequest,
	waitSeconds float64,
) (map[string]any, error) {
	if waitSeconds < searchWaitMinimum || waitSeconds > searchWaitMaximum ||
		waitSeconds != waitSeconds {
		return nil, &contracts.SearchValidationError{
			Message: "wait_seconds must be a finite number between 0 and 30",
		}
	}
	resultID, err := s.submitSearchTask(ctx, namespace, request)
	if err != nil {
		return nil, err
	}
	if waitSeconds <= 0 {
		return map[string]any{"result_id": resultID}, nil
	}
	deadline := time.Now().Add(time.Duration(waitSeconds * float64(time.Second)))
	for {
		payload, err := s.getSearchResultOrdered(ctx, namespace, resultID, 1, 10)
		if err != nil {
			return nil, err
		}
		if status, _ := payload["status"].(string); status != "queued" {
			return payload, nil
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return map[string]any{"result_id": resultID}, nil
		}
		pause := searchWaitPollInterval
		if remaining < pause {
			pause = remaining
		}
		select {
		case <-ctx.Done():
			return map[string]any{"result_id": resultID}, nil
		case <-time.After(pause):
		}
	}
}

// getSearchResultOrdered ports service_knowledge.get_search_result: the queued
// task view is authoritative first (queued/timed-out/failed proxies), and only
// completed tasks fall through to the immutable result snapshot.
func (s *Service) getSearchResultOrdered(
	ctx context.Context,
	namespace, resultID string,
	page, pageSize int,
) (map[string]any, error) {
	if s.stores.SearchTasks != nil {
		view, err := s.stores.SearchTasks.Get(ctx, namespace, resultID, page, pageSize)
		if err != nil {
			return nil, searchResultReadError(err)
		}
		if view != nil && view.Status != state.SearchTaskSucceeded {
			// Python's active projection is exactly {result_id, status, stage}.
			return map[string]any{
				"result_id": view.ResultID,
				"status":    view.Status,
				"stage":     view.Stage,
			}, nil
		}
	}
	if s.search == nil {
		return nil, apierrors.SnapshotUnavailableError("Search result expired or unavailable")
	}
	return s.search.GetSearchResult(ctx, namespace, resultID, page, pageSize)
}

// searchResultReadError maps state status-view errors to the runtime categories
// used by the synchronous result read path.
func searchResultReadError(err error) error {
	var unavailable *state.SnapshotUnavailable
	if errors.As(err, &unavailable) {
		return apierrors.SnapshotUnavailableError(unavailable.Message)
	}
	var valueError *state.ValueError
	if errors.As(err, &valueError) {
		return &contracts.SearchValidationError{Message: valueError.Message}
	}
	return err
}

// executeQueuedSearchTaskOnce claims and completes one accepted search task with
// an injected executor. It exists so the runtime tests (and a future in-process
// worker) can exercise the exact worker contract: claim, execute, complete.
func (s *Service) executeQueuedSearchTaskOnce(
	ctx context.Context,
	notification state.SearchNotification,
	executor func(ctx context.Context, namespace string, arguments map[string]any) (map[string]any, error),
) error {
	if s.stores.SearchTasks == nil {
		return nil
	}
	task, err := s.stores.SearchTasks.Claim(ctx, notification)
	if err != nil || task == nil {
		return err
	}
	payload, err := executor(ctx, task.Namespace, task.Arguments)
	if err != nil {
		failed := "Search execution failed"
		if _, failErr := s.stores.SearchTasks.Fail(ctx, task, true, &failed); failErr != nil {
			return failErr
		}
		return err
	}
	// Capture exactly like the synchronous path; the snapshot is persisted by
	// Complete (save_snapshot=false keeps Capture's own write out of the way).
	if capture := searchrefs.New(s.stores, nil); capture != nil {
		payload, _ = capture.Capture(ctx, task.Namespace, payload, false)
	}
	_, err = s.stores.SearchTasks.Complete(ctx, task, payload)
	return err
}
