// Package modelapi ports rag_mcp/model_api.py: HTTP-only embedding and
// reranking clients with safe, credential-free error messages.
package modelapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
)

// DefaultTimeout is the model service request timeout in seconds.
const DefaultTimeout = 60.0

// ModelAPIError is a safe, user-facing model service error without credentials
// or payloads. It mirrors rag_mcp.model_api.ModelAPIError. Status carries the
// provider HTTP status when one was received (0 for local failures), so callers
// can treat "no /models route" differently from a real fault.
type ModelAPIError struct {
	Message string
	Status  int
}

func (e *ModelAPIError) Error() string { return e.Message }

func modelError(message string) *ModelAPIError { return &ModelAPIError{Message: message} }

// ModelConnection is the immutable endpoint configuration for one model kind.
type ModelConnection struct {
	BaseURL    string
	Model      string
	APIKey     string
	VectorSize *int
}

// Configured reports whether both a base URL and a model are set.
func (c ModelConnection) Configured() bool {
	return c.BaseURL != "" && c.Model != ""
}

// Public renders the credential-free connection description exposed to the
// Console. It matches ModelConnection.public().
func (c ModelConnection) Public() map[string]any {
	var vectorSize any
	if c.VectorSize != nil {
		vectorSize = *c.VectorSize
	}
	return map[string]any{
		"base_url":    c.BaseURL,
		"model":       c.Model,
		"api_key_set": c.APIKey != "",
		"vector_size": vectorSize,
		"configured":  c.Configured(),
	}
}

// NormalizeBaseURL validates and normalizes a user-supplied model service URL
// with the same acceptance rules and messages as normalize_base_url.
func NormalizeBaseURL(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimRight(value, "/")
	// Python's urlsplit tolerates embedded whitespace and rejects the URL in
	// the second validation branch; keep that ordering for identical messages.
	if strings.ContainsFunc(value, unicode.IsSpace) {
		return "", modelError("base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段")
	}
	parsed, err := url.Parse(value)
	if err != nil {
		return "", modelError("base_url 地址无效")
	}
	port := parsed.Port()
	if port != "" {
		parsedPort, convErr := strconv.Atoi(port)
		if convErr != nil || parsedPort > 65535 {
			return "", modelError("base_url 地址无效")
		}
	}
	scheme := strings.ToLower(parsed.Scheme)
	schemeAllowed := scheme == "http" || scheme == "https"
	if !schemeAllowed || parsed.Hostname() == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" ||
		parsed.RawFragment != "" || port == "0" {
		return "", modelError("base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段")
	}
	return value, nil
}

// ModelHTTPClient is the shared HTTP transport for the model service.
type ModelHTTPClient struct {
	Connection ModelConnection
	Timeout    float64
	// HTTPClient overrides the default transport when non-nil (tests).
	HTTPClient *http.Client
}

// NewModelHTTPClient builds a client; a non-positive timeout falls back to the
// Python default of 60 seconds.
func NewModelHTTPClient(connection ModelConnection, timeout float64) *ModelHTTPClient {
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	return &ModelHTTPClient{Connection: connection, Timeout: timeout}
}

// Request performs one GET (body nil) or POST (body non-nil) model API call and
// returns the decoded JSON object. All failure modes are collapsed into safe
// ModelAPIError messages.
func (c *ModelHTTPClient) Request(ctx context.Context, endpoint string, body map[string]any) (map[string]any, error) {
	if c.Connection.BaseURL == "" {
		return nil, modelError("尚未配置模型服务地址")
	}
	headers := map[string]string{"Accept": "application/json"}
	if c.Connection.APIKey != "" {
		headers["Authorization"] = "Bearer " + c.Connection.APIKey
	}

	method := http.MethodGet
	var payload io.Reader
	if body != nil {
		method = http.MethodPost
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, modelError("模型服务请求失败")
		}
		payload = bytes.NewReader(encoded)
		headers["Content-Type"] = "application/json"
	}
	request, err := http.NewRequestWithContext(ctx, method, c.Connection.BaseURL+"/"+endpoint, payload)
	if err != nil {
		return nil, modelError("模型服务请求失败")
	}
	for key, value := range headers {
		request.Header.Set(key, value)
	}

	response, err := c.client().Do(request)
	if err != nil {
		return nil, safeTransportError(err)
	}
	defer response.Body.Close()

	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := "模型服务请求失败"
		switch response.StatusCode {
		case http.StatusUnauthorized:
			detail = "认证失败，请检查 apikey"
		case http.StatusForbidden:
			detail = "服务拒绝访问"
		case http.StatusNotFound:
			detail = "接口或模型不存在，请检查地址和模型名称"
		case http.StatusTooManyRequests:
			detail = "请求过于频繁或额度不足，请稍后重试"
		}
		return nil, &ModelAPIError{
			Message: fmt.Sprintf("HTTP %d：%s", response.StatusCode, detail),
			Status:  response.StatusCode,
		}
	}

	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return nil, safeTransportError(err)
	}
	if !utf8.Valid(raw) {
		return nil, modelError("模型服务返回了无效 JSON")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var decoded any
	if err := decoder.Decode(&decoded); err != nil {
		return nil, modelError("模型服务返回了无效 JSON")
	}
	object, ok := decoded.(map[string]any)
	if !ok {
		return nil, modelError("模型服务响应必须是 JSON 对象")
	}
	return object, nil
}

func (c *ModelHTTPClient) client() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	timeout := c.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	transport := &http.Transport{
		Proxy: nil, // trust_env=False
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	return &http.Client{
		Transport: transport,
		Timeout:   time.Duration(timeout * float64(time.Second)),
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// safeTransportError classifies transport failures with the Python messages.
func safeTransportError(err error) *ModelAPIError {
	var netError net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netError) && netError.Timeout()) {
		return modelError("模型服务请求超时")
	}
	return modelError("无法连接模型服务，请检查地址、端口和服务状态")
}

// Models returns the provider model ids in declaration order without
// duplicates.
func (c *ModelHTTPClient) Models(ctx context.Context) ([]string, error) {
	payload, err := c.Request(ctx, "models", nil)
	if err != nil {
		return nil, err
	}
	data, ok := payload["data"].([]any)
	if !ok {
		return nil, modelError("模型列表格式无效")
	}
	ids := make([]string, 0, len(data))
	seen := make(map[string]struct{}, len(data))
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, modelError("模型列表格式无效")
		}
		id, ok := object["id"].(string)
		if !ok || strings.TrimSpace(id) == "" {
			return nil, modelError("模型列表格式无效")
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids, nil
}

// HTTPEmbedder is the retrieval embedder backed by the model HTTP API.
type HTTPEmbedder struct {
	client *ModelHTTPClient
}

// NewHTTPEmbedder builds an HTTP embedder.
func NewHTTPEmbedder(connection ModelConnection, timeout float64) *HTTPEmbedder {
	return &HTTPEmbedder{client: NewModelHTTPClient(connection, timeout)}
}

// Connection returns the configured connection.
func (e *HTTPEmbedder) Connection() ModelConnection { return e.client.Connection }

// VectorSize returns the configured dense dimension, or zero when unset.
func (e *HTTPEmbedder) VectorSize() int {
	if e.client.Connection.VectorSize == nil {
		return 0
	}
	return *e.client.Connection.VectorSize
}

// EmbedDocuments encodes texts in batches of 32 and validates every vector.
func (e *HTTPEmbedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	if len(texts) == 0 {
		return [][]float64{}, nil
	}
	if !e.client.Connection.Configured() {
		return nil, modelError("Embedding 尚未配置，请配置服务地址并为知识库选择模型")
	}
	vectors := make([][]float64, 0, len(texts))
	dimension := e.VectorSize()
	for start := 0; start < len(texts); start += 32 {
		end := start + 32
		if end > len(texts) {
			end = len(texts)
		}
		batch := texts[start:end]
		payload, err := e.client.Request(ctx, "embeddings", map[string]any{
			"model": e.client.Connection.Model,
			"input": batch,
		})
		if err != nil {
			return nil, err
		}
		data, ok := payload["data"].([]any)
		if !ok || len(data) != len(batch) {
			return nil, modelError("Embedding 返回的向量数量与输入不一致")
		}
		ordered := make(map[int][]float64, len(batch))
		for _, item := range data {
			object, ok := item.(map[string]any)
			if !ok {
				return nil, modelError("Embedding 返回格式无效")
			}
			index, ok := strictInteger(object["index"])
			if !ok || index < 0 || index >= len(batch) {
				return nil, modelError("Embedding 返回的序号或向量无效")
			}
			if _, duplicate := ordered[index]; duplicate {
				return nil, modelError("Embedding 返回的序号或向量无效")
			}
			vector, ok := floatVector(object["embedding"])
			if !ok || len(vector) == 0 {
				return nil, modelError("Embedding 返回的序号或向量无效")
			}
			if dimension == 0 {
				dimension = len(vector)
			}
			if len(vector) != dimension {
				return nil, modelError("Embedding 向量长度与当前索引不一致")
			}
			ordered[index] = vector
		}
		for index := 0; index < len(batch); index++ {
			vector, ok := ordered[index]
			if !ok {
				return nil, modelError("Embedding 返回的序号或向量无效")
			}
			vectors = append(vectors, vector)
		}
	}
	return vectors, nil
}

// EmbedQuery encodes one query with the same dense representation.
func (e *HTTPEmbedder) EmbedQuery(ctx context.Context, text string) ([]float64, error) {
	vectors, err := e.EmbedDocuments(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	if len(vectors) == 0 {
		return nil, modelError("Embedding 返回的向量数量与输入不一致")
	}
	return vectors[0], nil
}

// IndexScore is one reranker relevance score keyed by document index.
type IndexScore struct {
	Index int
	Score float64
}

// HTTPReranker scores query/passage pairs through the model HTTP API.
type HTTPReranker struct {
	client        *ModelHTTPClient
	MaxCandidates int
	MaxTextChars  int
}

// NewHTTPReranker builds an HTTP reranker. Non-positive bounds fall back to the
// Python defaults (30 candidates, 4000 characters, 60 second timeout).
func NewHTTPReranker(connection ModelConnection, maxCandidates, maxTextChars int, timeout float64) *HTTPReranker {
	if maxCandidates <= 0 {
		maxCandidates = 30
	}
	if maxTextChars <= 0 {
		maxTextChars = 4000
	}
	return &HTTPReranker{
		client:        NewModelHTTPClient(connection, timeout),
		MaxCandidates: maxCandidates,
		MaxTextChars:  maxTextChars,
	}
}

// Connection returns the configured connection.
func (r *HTTPReranker) Connection() ModelConnection { return r.client.Connection }

// Scores requests one relevance score per document and returns them ordered by
// descending score, then ascending index.
func (r *HTTPReranker) Scores(ctx context.Context, query string, documents []string) ([]IndexScore, error) {
	if !r.client.Connection.Configured() {
		return nil, modelError("Reranker 尚未配置，请配置服务地址并为知识库选择模型")
	}
	payload, err := r.client.Request(ctx, "rerank", map[string]any{
		"model":     r.client.Connection.Model,
		"query":     query,
		"documents": documents,
		"top_n":     len(documents),
	})
	if err != nil {
		return nil, err
	}
	data, ok := payload["results"].([]any)
	if !ok || len(data) != len(documents) {
		return nil, modelError("Reranker 返回的结果数量不完整")
	}
	scores := make(map[int]float64, len(documents))
	for _, item := range data {
		object, ok := item.(map[string]any)
		if !ok {
			return nil, modelError("Reranker 返回格式无效")
		}
		index, ok := strictInteger(object["index"])
		if !ok || index < 0 || index >= len(documents) {
			return nil, modelError("Reranker 返回的序号或分数无效")
		}
		if _, duplicate := scores[index]; duplicate {
			return nil, modelError("Reranker 返回的序号或分数无效")
		}
		score, ok := finiteNumber(object["relevance_score"])
		if !ok {
			return nil, modelError("Reranker 返回的序号或分数无效")
		}
		scores[index] = score
	}
	ordered := make([]IndexScore, 0, len(scores))
	for index, score := range scores {
		ordered = append(ordered, IndexScore{Index: index, Score: score})
	}
	sort.Slice(ordered, func(i, j int) bool {
		if ordered[i].Score != ordered[j].Score {
			return ordered[i].Score > ordered[j].Score
		}
		return ordered[i].Index < ordered[j].Index
	})
	return ordered, nil
}

// Rerank replaces retrieval scores for a bounded prefix and preserves the tail.
func (r *HTTPReranker) Rerank(ctx context.Context, query string, candidates []contracts.Candidate) ([]contracts.Candidate, error) {
	if len(candidates) == 0 {
		return []contracts.Candidate{}, nil
	}
	selectedCount := r.MaxCandidates
	if selectedCount > len(candidates) {
		selectedCount = len(candidates)
	}
	selected := candidates[:selectedCount]
	documents := make([]string, len(selected))
	for index, candidate := range selected {
		documents[index] = truncateRunes(
			pythonStrip(candidate.Title+"\n"+candidate.Text),
			r.MaxTextChars,
		)
	}
	scores, err := r.Scores(ctx, query, documents)
	if err != nil {
		return nil, err
	}
	reranked := make([]contracts.Candidate, 0, len(scores))
	for _, item := range scores {
		original := selected[item.Index]
		metadata := make(map[string]any, len(original.Metadata)+1)
		for key, value := range original.Metadata {
			metadata[key] = value
		}
		metadata["retrieval_score"] = round6(original.Score)
		original.Score = item.Score
		original.Metadata = metadata
		reranked = append(reranked, original)
	}
	return append(reranked, candidates[selectedCount:]...), nil
}

// strictInteger accepts only JSON integers, matching Python's `type(x) is int`
// check (rejects floats, booleans, and strings).
func strictInteger(value any) (int, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	text := string(number)
	if strings.ContainsAny(text, ".eE") {
		return 0, false
	}
	parsed, err := strconv.Atoi(text)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

// finiteNumber accepts JSON integers and floats but rejects non-finite values.
func finiteNumber(value any) (float64, bool) {
	number, ok := value.(json.Number)
	if !ok {
		return 0, false
	}
	parsed, err := number.Float64()
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) {
		return 0, false
	}
	return parsed, true
}

// floatVector validates one embedding array.
func floatVector(value any) ([]float64, bool) {
	items, ok := value.([]any)
	if !ok {
		return nil, false
	}
	vector := make([]float64, len(items))
	for index, item := range items {
		parsed, ok := finiteNumber(item)
		if !ok {
			return nil, false
		}
		vector[index] = parsed
	}
	return vector, true
}

// round6 mirrors Python round(score, 6) for the values produced here.
func round6(value float64) float64 {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return value
	}
	text := strconv.FormatFloat(value, 'f', 6, 64)
	rounded, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return value
	}
	return rounded
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit])
}

// pythonStrip removes Unicode whitespace from both ends like str.strip().
func pythonStrip(text string) string {
	return strings.TrimFunc(text, unicode.IsSpace)
}

// TraceEnabled reports whether RAG_DEBUG_TRACE is on, mirroring the Python
// _trace helper used by the retrieval pipeline.
func TraceEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("RAG_DEBUG_TRACE"))) {
	case "1", "true", "yes":
		return true
	default:
		return false
	}
}
