package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/ingest"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func vectorTestUpload(t *testing.T, service *Service, stores *state.Stores) (*state.UploadJournal, *state.UploadOperation, *indexedUpload, *uploadResources) {
	t.Helper()
	resources, err := service.resolveUploadResources(context.Background(), "alpha")
	if err != nil {
		t.Fatalf("resolveUploadResources() error = %v", err)
	}
	journal, err := state.NewUploadJournal(stores, state.WithAutoHeartbeat(false))
	if err != nil {
		t.Fatalf("NewUploadJournal() error = %v", err)
	}
	t.Cleanup(journal.Close)
	operation, err := journal.Begin(context.Background(), "alpha", "vectors.md", chunking.DocumentID("vectors.md"), resources.collection, state.BeginOptions{})
	if err != nil {
		t.Fatalf("Begin() error = %v", err)
	}
	indexed := &indexedUpload{result: ingest.Result{Chunks: []chunking.Chunk{
		{Text: "Water pressure 水压检查"},
	}}}
	return journal, operation, indexed, resources
}

func TestEncodeDocumentVectorsConcurrent(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	journal, operation, indexed, resources := vectorTestUpload(t, service, stores)
	observed := make(chan error, 1)
	modelURL, _ := newFakeModelAPI(t, func(path string, body map[string]any) (int, string) {
		// Hold the embedding response until BM25 is durably DONE. A serial
		// implementation cannot satisfy this condition before the timeout.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			var done int
			err := stores.DB.QueryRowContext(ctx,
				"SELECT COUNT(*) FROM upload_steps WHERE operation_id = ? AND name = 'sparse' AND status = 'DONE'", operation.ID).Scan(&done)
			if err != nil || done == 1 {
				observed <- err
				break
			}
			select {
			case <-ctx.Done():
				observed <- ctx.Err()
				return http.StatusGatewayTimeout, `{}`
			case <-ticker.C:
			}
		}
		return kbModelHandler(nil)(path, body)
	})
	resources.embedder = modelapi.NewHTTPEmbedder(modelapi.ModelConnection{BaseURL: modelURL, Model: "embed-a"}, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	dense, lexical, err := service.encodeDocumentVectors(ctx, journal, operation, indexed, resources)
	if err != nil {
		t.Fatalf("encodeDocumentVectors() error = %v", err)
	}
	if err := <-observed; err != nil {
		t.Fatalf("BM25 did not finish while embedding was waiting: %v", err)
	}
	if len(dense) != 1 || len(dense[0]) != 3 {
		t.Fatalf("dense vectors = %v", dense)
	}
	want, err := resources.encoder.EncodeDocuments([]string{indexed.result.Chunks[0].Text})
	if err != nil || !reflect.DeepEqual(lexical, want) {
		t.Fatalf("BM25 vectors = %v, want %v (error %v)", lexical, want, err)
	}
	saved := operationStatus(t, stores, operation.ID)
	if len(saved.Steps) != 2 {
		t.Fatalf("journal steps = %v", saved.Steps)
	}
	for _, step := range saved.Steps {
		if step.Status != "DONE" {
			t.Fatalf("vector step did not finish: %+v", step)
		}
	}
}

func TestEncodeDocumentVectorsCancellation(t *testing.T) {
	service, stores, _, _ := newUploadService(t)
	journal, operation, indexed, resources := vectorTestUpload(t, service, stores)
	started := make(chan struct{})
	requestCancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, request.Body)
		close(started)
		<-request.Context().Done()
		close(requestCancelled)
	}))
	t.Cleanup(server.Close)
	resources.embedder = modelapi.NewHTTPEmbedder(modelapi.ModelConnection{BaseURL: server.URL, Model: "embed-a"}, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, _, err := service.encodeDocumentVectors(ctx, journal, operation, indexed, resources)
		finished <- err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("embedding request did not start")
	}
	cancel()
	select {
	case err := <-finished:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled vector computation error = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("vector computation did not stop after cancellation")
	}
	select {
	case <-requestCancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("embedding HTTP request was not cancelled")
	}
}
