package main

import (
	"context"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/mq"
)

func TestSagWakeOnPublication(t *testing.T) {
	head := `{"op":"u","source":{"table":"document_heads"},"after":{"namespace":"rag-notes","document_id":"agentic-rag","version":"v2"}}`
	wake := make(chan struct{}, 1)
	handler := wakeOnPublication(wake)
	for _, body := range []string{
		`{"op":"c","source":{"table":"documents"},"after":{"namespace":"rag-notes","name":"agentic-rag.md","document_id":"agentic-rag","del_flag":0}}`,
		`{"op":"d","source":{"table":"document_heads"},"before":{"namespace":"rag-notes","document_id":"agentic-rag","version":"v1"},"after":null}`,
		`null`,
	} {
		if !handler(context.Background(), mq.ReceivedMessage{Body: []byte(body)}) || len(wake) != 0 {
			t.Fatalf("unrelated notification should be acknowledged without waking: %s", body)
		}
	}
	for range 2 {
		if !handler(context.Background(), mq.ReceivedMessage{Body: []byte(head)}) {
			t.Fatal("publication should be acknowledged")
		}
	}
	if len(wake) != 1 {
		t.Fatalf("duplicate publication should coalesce to one wakeup, got %d", len(wake))
	}
}
