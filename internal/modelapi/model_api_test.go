package modelapi

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

type capturedRequest struct {
	Method string
	Path   string
	Header http.Header
	Body   string
}

func newModelServer(t *testing.T, respond func(body map[string]any) (int, string)) (*httptest.Server, *[]capturedRequest) {
	t.Helper()
	requests := make([]capturedRequest, 0, 4)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		captured := capturedRequest{Method: request.Method, Path: request.URL.Path, Header: request.Header.Clone()}
		captured.Body = string(raw)
		requests = append(requests, captured)
		var body map[string]any
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &body); err != nil {
				t.Errorf("request body is not a JSON object: %v (%s)", err, raw)
			}
		}
		status, response := respond(body)
		writer.WriteHeader(status)
		_, _ = writer.Write([]byte(response))
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func jsonNumber(value int) *int { return &value }

func TestNormalizeBaseURL(t *testing.T) {
	valid := map[string]string{
		"http://localhost:6333":         "http://localhost:6333",
		" https://api.example.com/v1/ ": "https://api.example.com/v1",
		"HTTP://Host.Example:8080/":     "HTTP://Host.Example:8080",
		"https://host":                  "https://host",
	}
	for value, want := range valid {
		got, err := NormalizeBaseURL(value)
		if err != nil {
			t.Errorf("NormalizeBaseURL(%q) error: %v", value, err)
			continue
		}
		if got != want {
			t.Errorf("NormalizeBaseURL(%q) = %q, want %q", value, got, want)
		}
	}

	invalidAddress := []string{
		"http://host:abc",
		"http://host:99999",
		"http://host:-1",
		"http://[::1",
	}
	for _, value := range invalidAddress {
		if _, err := NormalizeBaseURL(value); err == nil || err.Error() != "base_url 地址无效" {
			t.Errorf("NormalizeBaseURL(%q) error = %v, want base_url 地址无效", value, err)
		}
	}

	invalidFormat := []string{
		"",
		"ftp://host",
		"http://user:pass@host",
		"http://host?x=1",
		"http://host#fragment",
		"http://host:0",
		"http://",
		"not a url",
		"http://exa mple.com",
	}
	const formatMessage = "base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段"
	for _, value := range invalidFormat {
		if _, err := NormalizeBaseURL(value); err == nil || err.Error() != formatMessage {
			t.Errorf("NormalizeBaseURL(%q) error = %v, want %q", value, err, formatMessage)
		}
	}
}

func TestModelConnectionPublic(t *testing.T) {
	connection := ModelConnection{BaseURL: "http://host:1", Model: "m", APIKey: "secret"}
	public := connection.Public()
	if public["api_key_set"] != true || public["configured"] != true {
		t.Errorf("unexpected public payload: %v", public)
	}
	if _, leaked := public["api_key"]; leaked {
		t.Error("api_key leaked into public payload")
	}
	if public["vector_size"] != nil {
		t.Errorf("vector_size = %v, want nil", public["vector_size"])
	}

	size := 1024
	connection.VectorSize = &size
	if got := connection.Public()["vector_size"]; got != 1024 {
		t.Errorf("vector_size = %v, want 1024", got)
	}
	if (ModelConnection{}).Configured() {
		t.Error("empty connection must not be configured")
	}
	if !(ModelConnection{BaseURL: "http://h", Model: "m"}).Configured() {
		t.Error("connection with base_url and model must be configured")
	}
}

func TestModelsRequestIsUnauthenticatedGET(t *testing.T) {
	server, requests := newModelServer(t, func(body map[string]any) (int, string) {
		if len(body) != 0 {
			t.Errorf("unexpected GET body: %v", body)
		}
		return http.StatusOK, `{"data":[{"id":"alpha"},{"id":"beta"},{"id":"alpha"}]}`
	})
	client := NewModelHTTPClient(ModelConnection{BaseURL: server.URL}, 5)
	models, err := client.Models(context.Background())
	if err != nil {
		t.Fatalf("Models() error: %v", err)
	}
	if !reflect.DeepEqual(models, []string{"alpha", "beta"}) {
		t.Errorf("Models() = %v", models)
	}
	if len(*requests) != 1 {
		t.Fatalf("expected one request, got %d", len(*requests))
	}
	request := (*requests)[0]
	if request.Method != http.MethodGet || request.Path != "/models" {
		t.Errorf("request = %s %s, want GET /models", request.Method, request.Path)
	}
	if request.Header.Get("Accept") != "application/json" {
		t.Errorf("Accept header = %q", request.Header.Get("Accept"))
	}
	if request.Header.Get("Authorization") != "" {
		t.Errorf("unexpected Authorization header: %q", request.Header.Get("Authorization"))
	}
}

func TestModelsInvalidFormat(t *testing.T) {
	responses := []string{
		`{"data":"alpha"}`,
		`{"data":[{"noid":"alpha"}]}`,
		`{"data":[{"id":"  "}]}`,
		`{"data":[{"id":5}]}`,
		`{"data":[null]}`,
		`{}`,
	}
	for _, response := range responses {
		server, _ := newModelServer(t, func(map[string]any) (int, string) {
			return http.StatusOK, response
		})
		client := NewModelHTTPClient(ModelConnection{BaseURL: server.URL}, 5)
		if _, err := client.Models(context.Background()); err == nil || err.Error() != "模型列表格式无效" {
			t.Errorf("Models() response %s error = %v, want 模型列表格式无效", response, err)
		}
	}
}

func TestEmbedderRequestShapeAndIndexOrdering(t *testing.T) {
	server, requests := newModelServer(t, func(body map[string]any) (int, string) {
		if body["model"] != "embed-model" {
			t.Errorf("model = %v", body["model"])
		}
		input := body["input"].([]any)
		if len(input) != 2 {
			t.Fatalf("input = %v", input)
		}
		// Shuffled indices must be re-ordered by the client.
		return http.StatusOK, `{"data":[{"index":1,"embedding":[0.0,2.0]},{"index":0,"embedding":[0.0,1.0]}]}`
	})
	embedder := NewHTTPEmbedder(ModelConnection{
		BaseURL: server.URL, Model: "embed-model", APIKey: "key-1", VectorSize: jsonNumber(2),
	}, 5)
	vectors, err := embedder.EmbedDocuments(context.Background(), []string{"第一段", "第二段"})
	if err != nil {
		t.Fatalf("EmbedDocuments() error: %v", err)
	}
	want := [][]float64{{0, 1}, {0, 2}}
	if !reflect.DeepEqual(vectors, want) {
		t.Errorf("vectors = %v, want %v", vectors, want)
	}
	request := (*requests)[0]
	if request.Method != http.MethodPost || request.Path != "/embeddings" {
		t.Errorf("request = %s %s, want POST /embeddings", request.Method, request.Path)
	}
	if request.Header.Get("Authorization") != "Bearer key-1" {
		t.Errorf("Authorization = %q", request.Header.Get("Authorization"))
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	wantBody := map[string]any{"model": "embed-model", "input": []any{"第一段", "第二段"}}
	if !reflect.DeepEqual(body, wantBody) {
		t.Errorf("body = %#v, want %#v", body, wantBody)
	}
}

func TestEmbedderBatchesThirtyTwo(t *testing.T) {
	texts := make([]string, 33)
	for index := range texts {
		texts[index] = "text"
	}
	server, requests := newModelServer(t, func(body map[string]any) (int, string) {
		inputs := body["input"].([]any)
		data := make([]map[string]any, len(inputs))
		for index := range inputs {
			data[index] = map[string]any{"index": index, "embedding": []float64{float64(index)}}
		}
		encoded, _ := json.Marshal(map[string]any{"data": data})
		return http.StatusOK, string(encoded)
	})
	embedder := NewHTTPEmbedder(ModelConnection{BaseURL: server.URL, Model: "m", VectorSize: jsonNumber(1)}, 5)
	vectors, err := embedder.EmbedDocuments(context.Background(), texts)
	if err != nil {
		t.Fatalf("EmbedDocuments() error: %v", err)
	}
	if len(vectors) != 33 {
		t.Fatalf("got %d vectors", len(vectors))
	}
	if len(*requests) != 2 {
		t.Fatalf("expected two batches, got %d", len(*requests))
	}
	for batchIndex, request := range *requests {
		var body map[string]any
		if err := json.Unmarshal([]byte(request.Body), &body); err != nil {
			t.Fatalf("request body: %v", err)
		}
		wantLength := 32
		if batchIndex == 1 {
			wantLength = 1
		}
		if got := len(body["input"].([]any)); got != wantLength {
			t.Errorf("batch %d size = %d, want %d", batchIndex, got, wantLength)
		}
	}
}

func TestEmbedderValidationErrors(t *testing.T) {
	cases := []struct {
		name       string
		response   string
		vectorSize *int
		want       string
	}{
		{"data not a list", `{"data":{}}`, nil, "Embedding 返回的向量数量与输入不一致"},
		{"data length mismatch", `{"data":[]}`, nil, "Embedding 返回的向量数量与输入不一致"},
		{"item not an object", `{"data":[1]}`, nil, "Embedding 返回格式无效"},
		{"float index", `{"data":[{"index":0.0,"embedding":[1.0]}]}`, nil, "Embedding 返回的序号或向量无效"},
		{"index out of range", `{"data":[{"index":3,"embedding":[1.0]}]}`, nil, "Embedding 返回的序号或向量无效"},
		{"duplicate index", `{"data":[{"index":0,"embedding":[1.0]},{"index":0,"embedding":[2.0]}]}`, nil, "Embedding 返回的序号或向量无效"},
		{"empty vector", `{"data":[{"index":0,"embedding":[]}]}`, nil, "Embedding 返回的序号或向量无效"},
		{"non numeric vector", `{"data":[{"index":0,"embedding":["x"]}]}`, nil, "Embedding 返回的序号或向量无效"},
		{"dimension mismatch", `{"data":[{"index":0,"embedding":[1.0,2.0]},{"index":1,"embedding":[1.0]}]}`, nil, "Embedding 向量长度与当前索引不一致"},
		{"configured dimension mismatch", `{"data":[{"index":0,"embedding":[1.0]}]}`, jsonNumber(3), "Embedding 向量长度与当前索引不一致"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := testCase.response
			server, _ := newModelServer(t, func(map[string]any) (int, string) {
				return http.StatusOK, response
			})
			count := 1
			if testCase.name == "duplicate index" || testCase.name == "dimension mismatch" {
				count = 2
			}
			texts := make([]string, count)
			for index := range texts {
				texts[index] = "t"
			}
			embedder := NewHTTPEmbedder(ModelConnection{
				BaseURL: server.URL, Model: "m", VectorSize: testCase.vectorSize,
			}, 5)
			if _, err := embedder.EmbedDocuments(context.Background(), texts); err == nil || err.Error() != testCase.want {
				t.Errorf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestEmbedderUnconfiguredAndEmptyInput(t *testing.T) {
	embedder := NewHTTPEmbedder(ModelConnection{}, 5)
	vectors, err := embedder.EmbedDocuments(context.Background(), nil)
	if err != nil || len(vectors) != 0 {
		t.Fatalf("empty input must not require configuration: %v, %v", vectors, err)
	}
	const want = "Embedding 尚未配置，请配置服务地址并为知识库选择模型"
	if _, err := embedder.EmbedDocuments(context.Background(), []string{"x"}); err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	if _, err := embedder.EmbedQuery(context.Background(), "x"); err == nil || err.Error() != want {
		t.Errorf("EmbedQuery error = %v, want %q", err, want)
	}
}

func TestRequestFailureModes(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   string
	}{
		{401, `{}`, "HTTP 401：认证失败，请检查 apikey"},
		{403, `{}`, "HTTP 403：服务拒绝访问"},
		{404, `{}`, "HTTP 404：接口或模型不存在，请检查地址和模型名称"},
		{429, `{}`, "HTTP 429：请求过于频繁或额度不足，请稍后重试"},
		{500, `{}`, "HTTP 500：模型服务请求失败"},
		{302, `{}`, "HTTP 302：模型服务请求失败"},
		{200, `not json`, "模型服务返回了无效 JSON"},
		{200, `[1,2]`, "模型服务响应必须是 JSON 对象"},
		{200, `{}`, ""},
	}
	for _, testCase := range cases {
		server, _ := newModelServer(t, func(map[string]any) (int, string) {
			return testCase.status, testCase.body
		})
		client := NewModelHTTPClient(ModelConnection{BaseURL: server.URL}, 5)
		_, err := client.Request(context.Background(), "models", nil)
		if testCase.want == "" {
			if err != nil {
				t.Errorf("status %d: unexpected error %v", testCase.status, err)
			}
			continue
		}
		if err == nil || err.Error() != testCase.want {
			t.Errorf("status %d error = %v, want %q", testCase.status, err, testCase.want)
		}
	}
}

func TestRequestInvalidUTF8Body(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte{0xff, 0xfe, 0xfd})
	}))
	defer server.Close()
	client := NewModelHTTPClient(ModelConnection{BaseURL: server.URL}, 5)
	if _, err := client.Request(context.Background(), "models", nil); err == nil || err.Error() != "模型服务返回了无效 JSON" {
		t.Errorf("error = %v", err)
	}
}

func TestRequestUnreachableAndTimeout(t *testing.T) {
	closed := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	closedURL := closed.URL
	closed.Close()
	client := NewModelHTTPClient(ModelConnection{BaseURL: closedURL}, 1)
	if _, err := client.Request(context.Background(), "models", nil); err == nil || err.Error() != "无法连接模型服务，请检查地址、端口和服务状态" {
		t.Errorf("unreachable error = %v", err)
	}

	slow := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{}`))
	}))
	defer slow.Close()
	fastClient := NewModelHTTPClient(ModelConnection{BaseURL: slow.URL}, 0.05)
	if _, err := fastClient.Request(context.Background(), "models", nil); err == nil || err.Error() != "模型服务请求超时" {
		t.Errorf("timeout error = %v", err)
	}
}

func TestRerankerScoresRequestAndOrdering(t *testing.T) {
	server, requests := newModelServer(t, func(body map[string]any) (int, string) {
		if body["model"] != "rerank-model" || body["query"] != "查询" || body["top_n"] != float64(3) {
			t.Errorf("unexpected rerank body: %v", body)
		}
		documents := body["documents"].([]any)
		if !reflect.DeepEqual(documents, []any{"a", "b", "c"}) {
			t.Errorf("documents = %v", documents)
		}
		return http.StatusOK, `{"results":[{"index":2,"relevance_score":0.5},{"index":0,"relevance_score":0.9},{"index":1,"relevance_score":0.5}]}`
	})
	reranker := NewHTTPReranker(ModelConnection{BaseURL: server.URL, Model: "rerank-model"}, 30, 4000, 5)
	scores, err := reranker.Scores(context.Background(), "查询", []string{"a", "b", "c"})
	if err != nil {
		t.Fatalf("Scores() error: %v", err)
	}
	want := []IndexScore{{Index: 0, Score: 0.9}, {Index: 1, Score: 0.5}, {Index: 2, Score: 0.5}}
	if !reflect.DeepEqual(scores, want) {
		t.Errorf("scores = %v, want %v", scores, want)
	}
	if len(*requests) != 1 || (*requests)[0].Path != "/rerank" || (*requests)[0].Method != http.MethodPost {
		t.Errorf("unexpected requests: %+v", *requests)
	}
}

func TestRerankerRerankBoundedPrefix(t *testing.T) {
	server, requests := newModelServer(t, func(body map[string]any) (int, string) {
		return http.StatusOK, `{"results":[{"index":0,"relevance_score":0.1},{"index":1,"relevance_score":0.9}]}`
	})
	candidates := []contracts.Candidate{
		{DocumentID: "first", ChunkID: "chunk-first", Title: "title-first", Source: "first.md",
			Text: "第一条", Score: 0.8, Metadata: map[string]any{"kind": "test"}},
		{DocumentID: "second", ChunkID: "chunk-second", Title: "title-second", Source: "second.md",
			Text: "第二条", Score: 0.2, Metadata: map[string]any{"kind": "test"}},
		{DocumentID: "tail", ChunkID: "chunk-tail", Title: "title-tail", Source: "tail.md",
			Text: "未发送给模型", Score: 0.6, Metadata: map[string]any{"kind": "test"}},
	}
	reranker := NewHTTPReranker(ModelConnection{BaseURL: server.URL, Model: "m"}, 2, 4000, 5)
	results, err := reranker.Rerank(context.Background(), "q", candidates)
	if err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	if len(results) != 3 {
		t.Fatalf("got %d results", len(results))
	}
	if results[0].DocumentID != "second" || results[1].DocumentID != "first" || results[2].DocumentID != "tail" {
		t.Errorf("ordering = %v", []string{results[0].DocumentID, results[1].DocumentID, results[2].DocumentID})
	}
	if results[0].Score != 0.9 {
		t.Errorf("rescored score = %v", results[0].Score)
	}
	if results[0].Metadata["retrieval_score"] != 0.2 {
		t.Errorf("retrieval_score = %v, want 0.2", results[0].Metadata["retrieval_score"])
	}
	if results[0].Metadata["kind"] != "test" {
		t.Errorf("original metadata lost: %v", results[0].Metadata)
	}
	// The tail candidate is never sent to the model and keeps its score.
	if results[2].Score != 0.6 {
		t.Errorf("tail score = %v", results[2].Score)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte((*requests)[0].Body), &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	documents := body["documents"].([]any)
	if len(documents) != 2 || documents[0] != "title-first\n第一条" || body["top_n"] != float64(2) {
		t.Errorf("sent documents = %v", documents)
	}
}

func TestRerankerPassageTruncation(t *testing.T) {
	captured := ""
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		raw, _ := io.ReadAll(request.Body)
		captured = string(raw)
		writer.WriteHeader(http.StatusOK)
		_, _ = writer.Write([]byte(`{"results":[{"index":0,"relevance_score":0.5}]}`))
	}))
	defer server.Close()
	reranker := NewHTTPReranker(ModelConnection{BaseURL: server.URL, Model: "m"}, 1, 5, 5)
	candidate := contracts.Candidate{
		DocumentID: "doc", ChunkID: "chunk", Title: "abcdefghij", Source: "doc.md",
		Text: "xy", Score: 0.1, Metadata: map[string]any{},
	}
	if _, err := reranker.Rerank(context.Background(), "q", []contracts.Candidate{candidate}); err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(captured), &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	documents := body["documents"].([]any)
	if documents[0] != "abcde" {
		t.Errorf("passage = %q, want abcde", documents[0])
	}

	// A title containing only whitespace leaves just the stripped body.
	emptyTitle := candidate
	emptyTitle.Title = "   "
	rerankerFull := NewHTTPReranker(ModelConnection{BaseURL: server.URL, Model: "m"}, 1, 4000, 5)
	if _, err := rerankerFull.Rerank(context.Background(), "q", []contracts.Candidate{emptyTitle}); err != nil {
		t.Fatalf("Rerank() error: %v", err)
	}
	if err := json.Unmarshal([]byte(captured), &body); err != nil {
		t.Fatalf("request body: %v", err)
	}
	if body["documents"].([]any)[0] != "xy" {
		t.Errorf("passage = %q, want xy", body["documents"].([]any)[0])
	}
}

func TestRerankerValidationErrors(t *testing.T) {
	cases := []struct {
		name     string
		response string
		want     string
	}{
		{"count mismatch", `{"results":[]}`, "Reranker 返回的结果数量不完整"},
		{"results not a list", `{"results":{}}`, "Reranker 返回的结果数量不完整"},
		{"item not an object", `{"results":[1]}`, "Reranker 返回格式无效"},
		{"float index", `{"results":[{"index":0.0,"relevance_score":0.1}]}`, "Reranker 返回的序号或分数无效"},
		{"index out of range", `{"results":[{"index":4,"relevance_score":0.1}]}`, "Reranker 返回的序号或分数无效"},
		{"duplicate index", `{"results":[{"index":0,"relevance_score":0.1},{"index":0,"relevance_score":0.2}]}`, "Reranker 返回的序号或分数无效"},
		{"string score", `{"results":[{"index":0,"relevance_score":"high"}]}`, "Reranker 返回的序号或分数无效"},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			response := testCase.response
			server, _ := newModelServer(t, func(map[string]any) (int, string) {
				return http.StatusOK, response
			})
			documents := []string{"a"}
			if testCase.name == "duplicate index" {
				documents = []string{"a", "b"}
			}
			reranker := NewHTTPReranker(ModelConnection{BaseURL: server.URL, Model: "m"}, 30, 4000, 5)
			if _, err := reranker.Scores(context.Background(), "q", documents); err == nil || err.Error() != testCase.want {
				t.Errorf("error = %v, want %q", err, testCase.want)
			}
		})
	}
}

func TestRerankerEmptyCandidatesAndUnconfigured(t *testing.T) {
	reranker := NewHTTPReranker(ModelConnection{}, 30, 4000, 5)
	results, err := reranker.Rerank(context.Background(), "q", nil)
	if err != nil || len(results) != 0 {
		t.Fatalf("empty candidates must not need configuration: %v, %v", results, err)
	}
	const want = "Reranker 尚未配置，请配置服务地址并为知识库选择模型"
	if _, err := reranker.Scores(context.Background(), "q", []string{"a"}); err == nil || err.Error() != want {
		t.Errorf("error = %v, want %q", err, want)
	}
	candidate := contracts.Candidate{DocumentID: "d", ChunkID: "c", Title: "t", Source: "s", Text: "x", Metadata: map[string]any{}}
	if _, err := reranker.Rerank(context.Background(), "q", []contracts.Candidate{candidate}); err == nil || err.Error() != want {
		t.Errorf("Rerank error = %v, want %q", err, want)
	}
}

func TestEmbedderVectorSize(t *testing.T) {
	if got := NewHTTPEmbedder(ModelConnection{}, 5).VectorSize(); got != 0 {
		t.Errorf("VectorSize() = %d, want 0", got)
	}
	size := 1024
	if got := NewHTTPEmbedder(ModelConnection{VectorSize: &size}, 5).VectorSize(); got != 1024 {
		t.Errorf("VectorSize() = %d, want 1024", got)
	}
}

func TestTimeoutDefaultsAndTrace(t *testing.T) {
	client := NewModelHTTPClient(ModelConnection{}, 0)
	if client.Timeout != DefaultTimeout {
		t.Errorf("Timeout = %v, want %v", client.Timeout, DefaultTimeout)
	}
	if TraceEnabled() {
		t.Errorf("trace must default to disabled")
	}
	t.Setenv("RAG_DEBUG_TRACE", "YES")
	if !TraceEnabled() {
		t.Errorf("trace env ignored")
	}
}

func TestRound6(t *testing.T) {
	cases := map[float64]float64{
		0.123456789: 0.123457,
		0.2:         0.2,
		1.0:         1.0,
	}
	for value, want := range cases {
		if got := round6(value); got != want {
			t.Errorf("round6(%v) = %v, want %v", value, got, want)
		}
	}
}

func TestRequestUsesInjectedClient(t *testing.T) {
	server, _ := newModelServer(t, func(map[string]any) (int, string) {
		return http.StatusOK, `{"ok":true}`
	})
	connection := ModelConnection{BaseURL: server.URL}
	client := &ModelHTTPClient{Connection: connection, Timeout: 5, HTTPClient: server.Client()}
	payload, err := client.Request(context.Background(), "models", nil)
	if err != nil || payload["ok"] != true {
		t.Fatalf("Request() = %v, %v", payload, err)
	}
	_ = strings.TrimSpace("")
}
