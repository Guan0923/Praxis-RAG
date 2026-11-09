// Package qdrant implements the Qdrant HTTP REST surface used by the Python
// backend. It reproduces the requests that qdrant-client 1.19.1 sends from
// rag_mcp/storage/qdrant_store.py: collection existence/creation with named
// dense and sparse vectors, point upserts with deterministic ids, dense,
// sparse, and server-side RRF queries, scroll, count, and filter deletes.
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultTimeout mirrors qdrant-client's five second REST default.
const DefaultTimeout = 5 * time.Second

// APIError reports a non-2xx or malformed Qdrant response.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string { return e.Message }

// Client is the minimal Qdrant REST transport.
type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

// NewClient normalizes the base URL and applies the default timeout.
func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://localhost:6333"
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: DefaultTimeout}
	}
	return &Client{baseURL: baseURL, apiKey: apiKey, httpClient: httpClient}
}

// BaseURL returns the normalized REST root.
func (c *Client) BaseURL() string { return c.baseURL }

// Do performs one request, unwraps the Qdrant {"result": ...} envelope, and
// decodes the result into out when out is non-nil.
func (c *Client) Do(ctx context.Context, method, path string, query url.Values, body any, out any) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("cannot encode Qdrant request body: %w", err)
		}
		reader = bytes.NewReader(encoded)
	}
	endpoint := c.baseURL + path
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	if c.apiKey != "" {
		request.Header.Set("api-key", c.apiKey)
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	raw, err := io.ReadAll(response.Body)
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		detail := strings.TrimSpace(string(raw))
		if len(detail) > 512 {
			detail = detail[:512]
		}
		return &APIError{
			StatusCode: response.StatusCode,
			Message:    fmt.Sprintf("Qdrant request %s %s failed: HTTP %d %s", method, path, response.StatusCode, detail),
		}
	}
	if out == nil || len(raw) == 0 {
		return nil
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("Qdrant response for %s is not valid JSON: %w", path, err)
	}
	if len(envelope.Result) == 0 {
		return nil
	}
	if err := json.Unmarshal(envelope.Result, out); err != nil {
		return fmt.Errorf("Qdrant response for %s has an invalid result: %w", path, err)
	}
	return nil
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	return c.Do(ctx, http.MethodGet, path, nil, nil, out)
}

func (c *Client) post(ctx context.Context, path string, query url.Values, body any, out any) error {
	return c.Do(ctx, http.MethodPost, path, query, body, out)
}

func (c *Client) put(ctx context.Context, path string, query url.Values, body any, out any) error {
	return c.Do(ctx, http.MethodPut, path, query, body, out)
}
