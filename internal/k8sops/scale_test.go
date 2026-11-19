package k8sops

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestManagedWorkerScale(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Method != "PATCH" || r.URL.Path != "/apis/apps/v1/namespaces/praxis-rag/deployments/rag-sag-worker" || r.Header.Get("Content-Type") != "application/json-patch+json" {
			t.Errorf("unexpected request: %s %s", r.Method, r.URL)
		}
		var operations []struct {
			Op, Path string
			Value    any
		}
		if err := json.NewDecoder(r.Body).Decode(&operations); err != nil {
			t.Error(err)
			return
		}
		if len(operations) != 4 || operations[0].Value != "uid" || operations[1].Value != "praxis-rag" || operations[2].Value != float64(2) || operations[3].Value != float64(1) {
			t.Errorf("patch %+v", operations)
		}
		if calls > 1 {
			w.WriteHeader(http.StatusUnprocessableEntity)
		}
	}))
	defer srv.Close()
	cluster, err := NewCluster(testSettings(t, srv.URL), discardLogger(), WithHTTPClient(srv.Client()))
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range [][2]string{{"other", "rag-sag-worker"}, {"praxis-rag", "rag-service"}} {
		if err := cluster.ScaleWorker(context.Background(), target[0], target[1], "uid", 2, 1); !errors.Is(err, ErrForbidden) {
			t.Fatal(err)
		}
	}
	scale := func() error {
		return cluster.ScaleWorker(context.Background(), "praxis-rag", "rag-sag-worker", "uid", 2, 1)
	}
	if err := scale(); err != nil {
		t.Fatal(err)
	}
	if err := scale(); !errors.Is(err, ErrScaleConflict) {
		t.Fatal(err)
	}
}
