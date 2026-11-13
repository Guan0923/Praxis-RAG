package mq

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

const cdcTestArtifactID = "d1feec53-1036-4af0-8035-86ef6990fa6a"

// cdcRow builds one document_job_outbox row image as Debezium renders it.
func cdcRow(overrides map[string]any) map[string]any {
	row := map[string]any{
		"id":          "0123456789abcdef0123456789abcdef",
		"artifact_id": cdcTestArtifactID,
		"namespace":   "manuals",
		"name":        "manual.txt",
		"status":      "PENDING",
		"attempts":    float64(0),
		"retry_at":    1700000000.5,
		"created_at":  1700000000.5,
	}
	for key, value := range overrides {
		row[key] = value
	}
	return row
}

// cdcEvent marshals one flattened Debezium envelope (schemas.enable=false, the
// format deploy/debezium/application.properties publishes).
func cdcEvent(t *testing.T, fields map[string]any) []byte {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatalf("json.Marshal(%+v): %v", fields, err)
	}
	return body
}

func TestDecodeDocumentMessageClassifiesJobAndDeletion(t *testing.T) {
	native, _, err := EncodeDocumentNotification(testDocumentNotification())
	if err != nil {
		t.Fatalf("EncodeDocumentNotification: %v", err)
	}
	cases := []struct {
		name         string
		body         []byte
		wantKind     DocumentMessageKind
		wantArtifact string
		wantErr      bool
	}{
		{
			name:         "native schema-3",
			body:         native,
			wantKind:     DocumentMessageJob,
			wantArtifact: testDocumentNotification().ArtifactID,
		},
		{
			name: "cdc create",
			body: cdcEvent(t, map[string]any{
				"before": nil, "after": cdcRow(nil), "op": "c",
			}),
			wantKind:     DocumentMessageJob,
			wantArtifact: cdcTestArtifactID,
		},
		{
			name: "cdc read",
			body: cdcEvent(t, map[string]any{
				"before": nil, "after": cdcRow(nil), "op": "r",
			}),
			wantKind:     DocumentMessageJob,
			wantArtifact: cdcTestArtifactID,
		},
		{
			name: "cdc update",
			body: cdcEvent(t, map[string]any{
				"before": cdcRow(map[string]any{"name": "old.txt"}),
				"after":  cdcRow(nil),
				"op":     "u",
			}),
			wantKind:     DocumentMessageJob,
			wantArtifact: cdcTestArtifactID,
		},
		{
			name: "cdc delete with op",
			body: cdcEvent(t, map[string]any{
				"before": cdcRow(nil), "after": nil, "op": "d",
			}),
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name: "delete without op",
			body: cdcEvent(t, map[string]any{
				"before": cdcRow(nil), "after": nil,
			}),
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name: "delete with absent after and op",
			body: cdcEvent(t, map[string]any{
				"before": cdcRow(nil),
			}),
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name: "delete with op and no before image",
			body: cdcEvent(t, map[string]any{
				"before": nil, "after": nil, "op": "d",
			}),
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name:     "tombstone json null",
			body:     []byte("null"),
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name:     "tombstone whitespace body",
			body:     []byte(" \n\t "),
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name:     "tombstone empty body",
			body:     []byte{},
			wantKind: DocumentMessageOutboxDeletion,
		},
		{
			name:     "malformed json",
			body:     []byte("{not json"),
			wantKind: DocumentMessageJob,
			wantErr:  true,
		},
		{
			name:     "invalid native envelope",
			body:     []byte(`{"schema":3}`),
			wantKind: DocumentMessageJob,
			wantErr:  true,
		},
		{
			name:     "non-object json value",
			body:     []byte(`[]`),
			wantKind: DocumentMessageJob,
			wantErr:  true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			notification, kind, err := DecodeDocumentMessage(testCase.body)
			if testCase.wantErr {
				if !errors.Is(err, ErrPayloadInvalid) {
					t.Fatalf("DecodeDocumentMessage() error = %v, want ErrPayloadInvalid", err)
				}
				if kind != DocumentMessageJob {
					t.Fatalf("kind = %v, want DocumentMessageJob for malformed payload", kind)
				}
				return
			}
			if err != nil {
				t.Fatalf("DecodeDocumentMessage() error = %v", err)
			}
			if kind != testCase.wantKind {
				t.Fatalf("kind = %v, want %v", kind, testCase.wantKind)
			}
			if kind == DocumentMessageOutboxDeletion {
				if notification != (state.DocumentNotification{}) {
					t.Fatalf("deletion notification = %+v, want zero value", notification)
				}
				return
			}
			if notification.ArtifactID != testCase.wantArtifact {
				t.Fatalf("artifact_id = %q, want %q", notification.ArtifactID, testCase.wantArtifact)
			}
		})
	}
}

// TestConsumerOutboxDeletionIsCommittedWithoutRetryOrDLQ drives a routine
// outbox-retention delete through the real PullConsumer against the package's
// fake transport. internal/mq must not import internal/documentworker (the
// worker imports mq), so this handler mirrors
// documentworker.Processor.MessageHandler's ack contract at the same seam; the
// real handler is covered by internal/documentworker's consumer test.
func TestConsumerOutboxDeletionIsCommittedWithoutRetryOrDLQ(t *testing.T) {
	deleteBody := cdcEvent(t, map[string]any{
		"before": cdcRow(nil), "after": nil, "op": "d",
	})
	transport := &fakePullTransport{batches: [][]ReceivedMessage{{
		{Topic: "doc-topic", Key: cdcTestArtifactID, Body: deleteBody},
	}}}
	dlq := &fakeRawPublisher{}
	var calls int32
	handler := func(_ context.Context, message ReceivedMessage) bool {
		_, kind, err := DecodeDocumentMessage(message.Body)
		if err != nil {
			return false
		}
		if kind == DocumentMessageOutboxDeletion {
			atomic.AddInt32(&calls, 1)
			// Mirror documentworker.Processor.MessageHandler: a deletion record
			// is acknowledged immediately and never retried.
			return true
		}
		return false
	}
	config := testConsumerConfig()
	config.MaxAttempts = 3
	config.DLQTopic = "doc-dlq"
	consumer := NewPullConsumer(config, transport, handler)
	consumer.DLQ = dlq
	if err := consumer.Start(context.Background()); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if acked, _, _ := transport.snapshot(); len(acked) == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := consumer.Stop(); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	acked, changed, _ := transport.snapshot()
	if len(acked) != 1 {
		t.Fatalf("acked records = %d, want 1 (delete event must be committed)", len(acked))
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("handler calls = %d, want 1 (delete event must not be retried)", got)
	}
	if len(changed) != 0 {
		t.Fatalf("visibility changes = %d, want 0", len(changed))
	}
	if len(dlq.published) != 0 {
		t.Fatalf("DLQ publishes = %+v, want none", dlq.published)
	}
	if err := consumer.LastError(); err != nil {
		t.Fatalf("LastError() = %v", err)
	}
}
