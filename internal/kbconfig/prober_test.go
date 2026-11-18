package kbconfig

import (
	"context"
	"net/http"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
)

func TestHTTPProberEmbeddingVectorSize(t *testing.T) {
	url, calls := newFakeModelServer(t, func(call modelCall) (int, string) {
		if call.Path != "/embeddings" {
			return http.StatusOK, `{"data":[{"index":0,"embedding":[1.0,0.0,0.0]}]}`
		}
		if call.Body["input"].([]any)[0] != connectionProbeText {
			t.Errorf("probe text = %v", call.Body["input"])
		}
		return http.StatusOK, `{"data":[{"index":0,"embedding":[1.0,0.5,-1.0]}]}`
	})
	prober := HTTPProber{Timeout: 5}
	size, err := prober.EmbeddingVectorSize(context.Background(), modelapi.ModelConnection{
		BaseURL: url, Model: "embed-a", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("EmbeddingVectorSize() error: %v", err)
	}
	if size != 3 {
		t.Errorf("size = %d, want 3", size)
	}
	recorded := calls()
	if len(recorded) != 1 || recorded[0].Path != "/embeddings" || recorded[0].Authorization != "Bearer k" {
		t.Errorf("probe request = %+v", recorded)
	}
}

func TestHTTPProberRerankerAccessible(t *testing.T) {
	url, calls := newFakeModelServer(t, func(call modelCall) (int, string) {
		if call.Path != "/rerank" {
			t.Errorf("unexpected probe path %q", call.Path)
		}
		documents := call.Body["documents"].([]any)
		if len(documents) != 1 || documents[0] != connectionProbeText {
			t.Errorf("probe documents = %v", documents)
		}
		return http.StatusOK, `{"results":[{"index":0,"relevance_score":0.5}]}`
	})
	prober := HTTPProber{Timeout: 5}
	err := prober.RerankerAccessible(context.Background(), modelapi.ModelConnection{
		BaseURL: url, Model: "rank-a", APIKey: "k",
	})
	if err != nil {
		t.Fatalf("RerankerAccessible() error: %v", err)
	}
	recorded := calls()
	if len(recorded) != 1 || recorded[0].Path != "/rerank" || recorded[0].Authorization != "Bearer k" {
		t.Errorf("probe request = %+v", recorded)
	}
}

func TestHTTPProberRequiresConfiguredConnections(t *testing.T) {
	prober := HTTPProber{}
	if _, err := prober.EmbeddingVectorSize(context.Background(), modelapi.ModelConnection{}); err == nil ||
		err.Error() != "Embedding 尚未配置，请配置服务地址并为知识库选择模型" {
		t.Errorf("unconfigured embedding error = %v", err)
	}
	if err := prober.RerankerAccessible(context.Background(), modelapi.ModelConnection{}); err == nil ||
		err.Error() != "Reranker 尚未配置，请配置服务地址并为知识库选择模型" {
		t.Errorf("unconfigured reranker error = %v", err)
	}
}

func TestHTTPProberPropagatesProviderErrors(t *testing.T) {
	url, _ := newFakeModelServer(t, func(modelCall) (int, string) {
		return http.StatusTooManyRequests, `{"detail":"private upstream error"}`
	})
	prober := HTTPProber{Timeout: 5}
	_, err := prober.EmbeddingVectorSize(context.Background(), modelapi.ModelConnection{BaseURL: url, Model: "m"})
	if err == nil || err.Error() != "HTTP 429：请求过于频繁或额度不足，请稍后重试" {
		t.Errorf("embedding error = %v", err)
	}
	err = prober.RerankerAccessible(context.Background(), modelapi.ModelConnection{BaseURL: url, Model: "m"})
	if err == nil || err.Error() != "HTTP 429：请求过于频繁或额度不足，请稍后重试" {
		t.Errorf("reranker error = %v", err)
	}
}
