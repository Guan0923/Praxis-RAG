package state

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

// TestSearchTaskArgumentsPreserveIntegerJSONTokens is the regression for the
// queued-search round-trip: search_tasks.arguments is JSON, and Python's json
// keeps integer tokens as int. The Go port must decode with
// json.Decoder.UseNumber so integer fields stay json.Number instead of float64,
// otherwise contracts.SearchRequestFromArguments (isinstance(int) semantics)
// rejects every broker-delivered candidate_k/top_k and the worker fails the task
// terminally.
func TestSearchTaskArgumentsPreserveIntegerJSONTokens(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	arguments := map[string]any{
		"query":       "E03 供水不足",
		"retrieval":   "hybrid",
		"rerank":      "cross_encoder",
		"candidate_k": 10,
		"top_k":       5,
		"filters":     map[string]any{"document_id": "doc-1"},
	}
	envelope, err := stores.SearchTasks.Submit(ctx, "manuals", arguments, nil)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}

	// The row read (used by Claim's digest comparison) preserves integer tokens.
	stored, err := stores.SearchTasks.read(ctx, envelope.ResultID)
	if err != nil || stored == nil {
		t.Fatalf("read() = %v, %v", stored, err)
	}
	candidateK, ok := stored.Arguments["candidate_k"].(json.Number)
	if !ok || candidateK.String() != "10" {
		t.Fatalf("stored candidate_k = %#v (%T), want json.Number(\"10\")", stored.Arguments["candidate_k"], stored.Arguments["candidate_k"])
	}
	topK, ok := stored.Arguments["top_k"].(json.Number)
	if !ok || topK.String() != "5" {
		t.Fatalf("stored top_k = %#v (%T), want json.Number(\"5\")", stored.Arguments["top_k"], stored.Arguments["top_k"])
	}

	// The JSON digest stays stable across the round-trip, so Claim still matches
	// the envelope it was admitted with.
	originalDigest, err := marshalJSON(arguments)
	if err != nil {
		t.Fatalf("marshalJSON(arguments) error = %v", err)
	}
	roundTrippedDigest, err := marshalJSON(stored.Arguments)
	if err != nil {
		t.Fatalf("marshalJSON(stored) error = %v", err)
	}
	if string(originalDigest) != string(roundTrippedDigest) {
		t.Fatalf("digest changed across the round-trip:\noriginal=%s\nstored=%s", originalDigest, roundTrippedDigest)
	}

	// A broker-style decode (json.Decoder.UseNumber, like mq.DecodeSearchEnvelope)
	// keeps the integer typing through Claim.
	encoded, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("json.Marshal(envelope) error = %v", err)
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		t.Fatalf("decode envelope error = %v", err)
	}
	decodedEnvelope, err := ValidateSearchEnvelope(payload)
	if err != nil {
		t.Fatalf("ValidateSearchEnvelope() error = %v", err)
	}
	task, err := stores.SearchTasks.Claim(ctx, decodedEnvelope)
	if err != nil || task == nil {
		t.Fatalf("Claim() = %v, %v", task, err)
	}
	claimedK, ok := task.Arguments["candidate_k"].(json.Number)
	if !ok || claimedK.String() != "10" {
		t.Fatalf("claimed candidate_k = %#v (%T), want json.Number(\"10\")", task.Arguments["candidate_k"], task.Arguments["candidate_k"])
	}

	// The exact worker conversion now accepts the claimed arguments.
	request, err := contracts.SearchRequestFromArguments(task.Arguments)
	if err != nil {
		t.Fatalf("SearchRequestFromArguments(claimed) error = %v", err)
	}
	if request.CandidateK != 10 || request.TopK != 5 || request.Query != "E03 供水不足" {
		t.Fatalf("converted request = %+v", request)
	}
}
