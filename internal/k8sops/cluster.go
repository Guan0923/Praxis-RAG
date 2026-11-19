package k8sops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	// listLimit keeps one LIST page bounded (the API server default is 500
	// anyway; naming it makes the pagination loop explicit).
	listLimit = 500
	// defaultWatchTimeout is the server-side watch timeout. The loop simply
	// reconnects from the last seen resourceVersion when it expires.
	defaultWatchTimeout = 5 * time.Minute
	// listTimeout bounds one LIST page so a stuck connection cannot stall the
	// watcher forever.
	listTimeout = 30 * time.Second
	// watch backoff bounds.
	initialWatchBackoff = time.Second
	maxWatchBackoff     = 30 * time.Second
	// maxResponseBytes bounds one LIST page.
	maxResponseBytes = 32 << 20
)

// errWatchGone signals that the watch resourceVersion is too old (HTTP 410 or
// an ERROR event with code 410): the cache must be rebuilt with a fresh LIST.
var errWatchGone = errors.New("kubernetes watch resource version is too old")

// Option tunes a Cluster. Options exist for tests (injected HTTP client and a
// short watch timeout); production callers pass none.
type Option func(*Cluster)

// WithHTTPClient replaces the HTTP client used for list/watch/patch. It is
// intended for httptest servers and custom transports.
func WithHTTPClient(client *http.Client) Option {
	return func(cluster *Cluster) {
		if client != nil {
			cluster.httpClient = client
		}
	}
}

// WithWatchTimeout overrides the server-side watch timeoutSeconds. Tests use a
// short value so the loops recycle quickly.
func WithWatchTimeout(timeout time.Duration) Option {
	return func(cluster *Cluster) {
		if timeout > 0 {
			cluster.watchTimeout = timeout
		}
	}
}

// apiStatusError carries the HTTP status of a non-200 API response so the
// watch loop can distinguish 410 Gone from transient failures.
type apiStatusError struct {
	status    int
	operation string
}

func (e *apiStatusError) Error() string {
	return fmt.Sprintf("Kubernetes API %s 请求失败（HTTP %d）", e.operation, e.status)
}

// resourceCache is one resource stream's shared snapshot. Objects are
// immutable: every watch event decodes a fresh value and replaces the map
// entry, so readers may keep the pointers after releasing the lock.
type resourceCache struct {
	mu        sync.RWMutex
	objects   map[string]cachedObject
	listed    bool
	firstList time.Time
	lastError string
}

func newResourceCache() *resourceCache {
	return &resourceCache{objects: make(map[string]cachedObject)}
}

func cacheKey(namespace, name string) string { return namespace + "/" + name }

func (cache *resourceCache) replaceAll(objects map[string]cachedObject) (firstList bool) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.objects = objects
	cache.listed = true
	cache.lastError = ""
	if cache.firstList.IsZero() {
		cache.firstList = time.Now()
		firstList = true
	}
	return firstList
}

func (cache *resourceCache) upsert(object cachedObject) {
	meta := object.meta()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.objects[cacheKey(meta.Namespace, meta.Name)] = object
}

func (cache *resourceCache) remove(namespace, name string) {
	cache.mu.Lock()
	defer cache.mu.Unlock()
	delete(cache.objects, cacheKey(namespace, name))
}

func (cache *resourceCache) get(namespace, name string) (cachedObject, bool) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	object, ok := cache.objects[cacheKey(namespace, name)]
	return object, ok
}

func (cache *resourceCache) snapshot() []cachedObject {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	objects := make([]cachedObject, 0, len(cache.objects))
	for _, object := range cache.objects {
		objects = append(objects, object)
	}
	return objects
}

func (cache *resourceCache) isListed() bool {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.listed
}

// state returns the cache readiness and the last recorded list/watch error.
// Both are read-only diagnostics for the snapshot layer: a cache that never
// completed its first LIST (and already failed) is reported as unavailable
// instead of rendering an empty table.
func (cache *resourceCache) state() (listed bool, lastError string) {
	cache.mu.RLock()
	defer cache.mu.RUnlock()
	return cache.listed, cache.lastError
}

func (cache *resourceCache) recordError(err error) {
	if err == nil {
		return
	}
	cache.mu.Lock()
	defer cache.mu.Unlock()
	cache.lastError = err.Error()
}

// Cluster is the process-wide Kubernetes read cache plus the rolling-restart
// tracker. It owns exactly one list/watch goroutine per resource.
type Cluster struct {
	settings     Settings
	logger       *slog.Logger
	httpClient   *http.Client
	watchTimeout time.Duration

	startMu sync.Mutex
	started bool
	closed  bool
	ctx     context.Context
	cancel  context.CancelFunc
	wg      sync.WaitGroup

	caches       map[string]*resourceCache
	memoryMu     sync.Mutex
	memorySample *clusterMemorySample

	// restartSubmitMu serializes the "check for an in-flight restart, PATCH,
	// record the tracker" sequence so two concurrent clicks cannot both PATCH.
	restartSubmitMu sync.Mutex
	restartMu       sync.Mutex
	restarts        map[string]*trackedRestart
}

// NewCluster builds the shared cache. It does not contact the API server until
// Start is called. Unconfigured settings return ErrNotConfigured.
func NewCluster(settings Settings, logger *slog.Logger, options ...Option) (*Cluster, error) {
	if !settings.Enabled() {
		return nil, ErrNotConfigured
	}
	if logger == nil {
		logger = slog.Default()
	}
	client, err := newClusterHTTPClient(settings)
	if err != nil {
		return nil, err
	}
	cluster := &Cluster{
		settings:     settings,
		logger:       logger,
		httpClient:   client,
		watchTimeout: defaultWatchTimeout,
		caches:       make(map[string]*resourceCache, len(resourceSpecs)),
		restarts:     make(map[string]*trackedRestart),
	}
	for _, option := range options {
		option(cluster)
	}
	for _, spec := range resourceSpecs {
		cluster.caches[spec.resource] = newResourceCache()
	}
	return cluster, nil
}

// NewClusterFromEnv resolves the cluster access from the environment and
// builds the cache. No configuration returns (nil, nil) so callers can degrade
// without special-casing a sentinel; a broken explicit kubeconfig returns the
// configuration error and must fail startup.
func NewClusterFromEnv(logger *slog.Logger, options ...Option) (*Cluster, error) {
	settings, err := SettingsFromEnv()
	if err != nil {
		return nil, err
	}
	if !settings.Enabled() {
		return nil, nil
	}
	return NewCluster(settings, logger, options...)
}

// newClusterHTTPClient builds the default transport. There is deliberately no
// http.Client.Timeout: a watch is a long-lived stream bounded by its own
// timeoutSeconds and by the request context.
func newClusterHTTPClient(settings Settings) (*http.Client, error) {
	tlsSettings, err := settings.tlsConfig()
	if err != nil {
		return nil, err
	}
	return &http.Client{
		Transport: &http.Transport{TLSClientConfig: tlsSettings},
	}, nil
}

// Configured reports whether cluster credentials were resolved.
func (cluster *Cluster) Configured() bool {
	return cluster != nil && cluster.settings.Enabled()
}

// ClusterInfo returns the identity shown on the Console page: the kubeconfig
// context name (never the API server URL) or "in-cluster".
func (cluster *Cluster) ClusterInfo() ClusterInfo {
	if cluster == nil {
		return ClusterInfo{}
	}
	source := cluster.settings.Source
	if source == "" {
		source = SourceServiceAccount
	}
	name := cluster.settings.ContextName
	if name == "" {
		name = "in-cluster"
	}
	return ClusterInfo{Name: name, Source: source}
}

// Start launches the eight list/watch loops in the background. It is
// idempotent: repeated calls (and calls after Close) are no-ops.
func (cluster *Cluster) Start(ctx context.Context) {
	if cluster == nil || !cluster.settings.Enabled() {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	cluster.startMu.Lock()
	if cluster.started || cluster.closed {
		cluster.startMu.Unlock()
		return
	}
	cluster.started = true
	cluster.ctx, cluster.cancel = context.WithCancel(ctx)
	loopContext := cluster.ctx
	cluster.startMu.Unlock()

	for _, spec := range resourceSpecs {
		spec := spec
		cluster.wg.Add(1)
		go func() {
			defer cluster.wg.Done()
			cluster.watchLoop(loopContext, spec)
		}()
	}
}

// Close stops the watchers and waits for them. It is idempotent, and the last
// successfully listed snapshot remains readable afterwards.
func (cluster *Cluster) Close() {
	if cluster == nil {
		return
	}
	cluster.startMu.Lock()
	cluster.closed = true
	cancel := cluster.cancel
	cluster.startMu.Unlock()
	if cancel != nil {
		cancel()
	}
	cluster.wg.Wait()
}

// collectionPath builds the cluster-wide collection URL.
func (cluster *Cluster) collectionPath(spec resourceSpec) string {
	return strings.TrimSuffix(cluster.settings.APIEndpoint, "/") + spec.apiPath + "/" + spec.resource
}

// objectPath builds one namespaced object URL (used by the restart fallback).
func (cluster *Cluster) objectPath(spec resourceSpec, namespace, name string) string {
	return strings.TrimSuffix(cluster.settings.APIEndpoint, "/") + spec.apiPath +
		"/namespaces/" + url.PathEscape(namespace) + "/" + spec.resource + "/" + url.PathEscape(name)
}

func (cluster *Cluster) newRequest(ctx context.Context, method, rawURL string, body []byte, contentType string) (*http.Request, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	request, err := http.NewRequestWithContext(ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	request.Header.Set("Accept", "application/json")
	if body != nil && contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if err := cluster.settings.authorize(request); err != nil {
		return nil, err
	}
	return request, nil
}

// do issues one bounded request and returns the payload and status.
func (cluster *Cluster) do(ctx context.Context, method, rawURL string, body []byte, contentType string) ([]byte, int, error) {
	request, err := cluster.newRequest(ctx, method, rawURL, body, contentType)
	if err != nil {
		return nil, 0, err
	}
	response, err := cluster.httpClient.Do(request)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes))
	if err != nil {
		return nil, response.StatusCode, err
	}
	return payload, response.StatusCode, nil
}

// listResponse is the metadata-bearing part of a Kubernetes list.
type listResponse struct {
	Metadata struct {
		ResourceVersion string `json:"resourceVersion"`
		Continue        string `json:"continue"`
	} `json:"metadata"`
	Items []json.RawMessage `json:"items"`
}

// watchEvent is one JSON line of a watch stream.
type watchEvent struct {
	Type   string          `json:"type"`
	Object json.RawMessage `json:"object"`
}

// watchStatus is the payload of an ERROR watch event.
type watchStatus struct {
	Code    int    `json:"code"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
}

func (cluster *Cluster) watchTimeoutSeconds() int64 {
	seconds := int64(math.Ceil(cluster.watchTimeout.Seconds()))
	if seconds < 1 {
		seconds = 1
	}
	return seconds
}

// listResource pages through the cluster-wide collection and atomically
// replaces the cache. It returns the resourceVersion of the last page, which is
// the starting point of the subsequent watch.
func (cluster *Cluster) listResource(ctx context.Context, spec resourceSpec) (string, error) {
	cache := cluster.caches[spec.resource]
	base := cluster.collectionPath(spec)
	collected := make(map[string]cachedObject)
	resourceVersion := ""
	continueToken := ""
	for {
		query := url.Values{}
		query.Set("limit", strconv.Itoa(listLimit))
		if continueToken != "" {
			query.Set("continue", continueToken)
		}
		listContext, cancel := context.WithTimeout(ctx, listTimeout)
		payload, status, err := cluster.do(listContext, http.MethodGet, base+"?"+query.Encode(), nil, "")
		cancel()
		if err != nil {
			return "", err
		}
		if status != http.StatusOK {
			return "", &apiStatusError{status: status, operation: "list " + spec.resource}
		}
		var response listResponse
		if err := json.Unmarshal(payload, &response); err != nil {
			return "", fmt.Errorf("无法解析 %s 列表响应: %w", spec.resource, err)
		}
		for _, raw := range response.Items {
			object := spec.newObject()
			if err := json.Unmarshal(raw, object); err != nil {
				return "", fmt.Errorf("无法解析 %s 对象: %w", spec.resource, err)
			}
			meta := object.meta()
			collected[cacheKey(meta.Namespace, meta.Name)] = object
		}
		if response.Metadata.ResourceVersion != "" {
			resourceVersion = response.Metadata.ResourceVersion
		}
		continueToken = response.Metadata.Continue
		if continueToken == "" {
			break
		}
	}
	if first := cache.replaceAll(collected); first {
		cluster.logger.Info("Kubernetes 资源缓存已建立", "resource", spec.resource, "objects", len(collected))
	}
	return resourceVersion, nil
}

// watchResource consumes one watch stream. A clean end (server timeout) returns
// nil so the caller reconnects from the last seen resourceVersion; a 410
// returns errWatchGone so the caller rebuilds the cache with a LIST.
func (cluster *Cluster) watchResource(ctx context.Context, spec resourceSpec, resourceVersion string) (string, error) {
	query := url.Values{}
	query.Set("watch", "true")
	query.Set("allowWatchBookmarks", "true")
	query.Set("timeoutSeconds", strconv.FormatInt(cluster.watchTimeoutSeconds(), 10))
	if resourceVersion == "" {
		resourceVersion = "0"
	}
	query.Set("resourceVersion", resourceVersion)

	request, err := cluster.newRequest(ctx, http.MethodGet, cluster.collectionPath(spec)+"?"+query.Encode(), nil, "")
	if err != nil {
		return resourceVersion, err
	}
	response, err := cluster.httpClient.Do(request)
	if err != nil {
		return resourceVersion, err
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return resourceVersion, &apiStatusError{status: response.StatusCode, operation: "watch " + spec.resource}
	}

	cache := cluster.caches[spec.resource]
	decoder := json.NewDecoder(response.Body)
	lastVersion := resourceVersion
	for {
		var event watchEvent
		if err := decoder.Decode(&event); err != nil {
			if ctx.Err() != nil {
				return lastVersion, ctx.Err()
			}
			if errors.Is(err, io.EOF) {
				return lastVersion, nil
			}
			return lastVersion, fmt.Errorf("读取 %s watch 事件失败: %w", spec.resource, err)
		}
		switch event.Type {
		case "ADDED", "MODIFIED":
			object := spec.newObject()
			if err := json.Unmarshal(event.Object, object); err != nil {
				return lastVersion, fmt.Errorf("无法解析 %s watch 对象: %w", spec.resource, err)
			}
			if version := object.meta().ResourceVersion; version != "" {
				lastVersion = version
			}
			cache.upsert(object)
		case "DELETED":
			object := spec.newObject()
			if err := json.Unmarshal(event.Object, object); err != nil {
				return lastVersion, fmt.Errorf("无法解析 %s watch 对象: %w", spec.resource, err)
			}
			if version := object.meta().ResourceVersion; version != "" {
				lastVersion = version
			}
			meta := object.meta()
			cache.remove(meta.Namespace, meta.Name)
		case "BOOKMARK":
			var bookmark struct {
				Metadata struct {
					ResourceVersion string `json:"resourceVersion"`
				} `json:"metadata"`
			}
			if err := json.Unmarshal(event.Object, &bookmark); err == nil && bookmark.Metadata.ResourceVersion != "" {
				lastVersion = bookmark.Metadata.ResourceVersion
			}
		case "ERROR":
			var status watchStatus
			_ = json.Unmarshal(event.Object, &status)
			if status.Code == http.StatusGone {
				return lastVersion, errWatchGone
			}
			cluster.logger.Warn("Kubernetes watch 事件报错", "resource", spec.resource,
				"reason", status.Reason, "message", truncateRunes(status.Message, 200))
		}
	}
}

// watchLoop is one resource's permanent reconciliation loop.
func (cluster *Cluster) watchLoop(ctx context.Context, spec resourceSpec) {
	cache := cluster.caches[spec.resource]
	backoff := initialWatchBackoff
	resourceVersion := ""
	for ctx.Err() == nil {
		if resourceVersion == "" {
			version, err := cluster.listResource(ctx, spec)
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				cache.recordError(err)
				cluster.logger.Warn("Kubernetes 资源列表失败，稍后重试", "resource", spec.resource, "error", err, "backoff", backoff)
				if !sleepContext(ctx, backoff) {
					return
				}
				backoff = nextWatchBackoff(backoff)
				continue
			}
			resourceVersion = version
			backoff = initialWatchBackoff
		}
		lastVersion, err := cluster.watchResource(ctx, spec, resourceVersion)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			if isWatchGone(err) {
				cluster.logger.Info("Kubernetes watch 资源版本过期，重新列表", "resource", spec.resource)
				resourceVersion = ""
				backoff = initialWatchBackoff
				continue
			}
			cache.recordError(err)
			cluster.logger.Warn("Kubernetes watch 失败，稍后重试", "resource", spec.resource, "error", err, "backoff", backoff)
			if lastVersion != "" {
				resourceVersion = lastVersion
			}
			if !sleepContext(ctx, backoff) {
				return
			}
			backoff = nextWatchBackoff(backoff)
			continue
		}
		if lastVersion != "" {
			resourceVersion = lastVersion
		}
		backoff = initialWatchBackoff
	}
}

func nextWatchBackoff(backoff time.Duration) time.Duration {
	backoff *= 2
	if backoff > maxWatchBackoff {
		return maxWatchBackoff
	}
	return backoff
}

// isWatchGone reports a stale resourceVersion, either as an ERROR watch event
// or as the HTTP 410 status of the watch request itself.
func isWatchGone(err error) bool {
	if errors.Is(err, errWatchGone) {
		return true
	}
	var statusError *apiStatusError
	return errors.As(err, &statusError) && statusError.status == http.StatusGone
}

func sleepContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// cacheSnapshot returns the cached objects of one resource. The returned
// pointers are immutable values, safe to read after the lock is released.
func (cluster *Cluster) cacheSnapshot(resource string) []cachedObject {
	cache, ok := cluster.caches[resource]
	if !ok {
		return nil
	}
	return cache.snapshot()
}

func (cluster *Cluster) cacheGet(resource, namespace, name string) (cachedObject, bool) {
	cache, ok := cluster.caches[resource]
	if !ok {
		return nil, false
	}
	return cache.get(namespace, name)
}

func (cluster *Cluster) cacheListed(resource string) bool {
	cache, ok := cluster.caches[resource]
	if !ok {
		return false
	}
	return cache.isListed()
}

// cacheState reports one resource cache's readiness and last error.
func (cluster *Cluster) cacheState(resource string) (listed bool, lastError string) {
	cache, ok := cluster.caches[resource]
	if !ok {
		return false, ""
	}
	return cache.state()
}

// namespaces returns the sorted union of every cached object's namespace.
func (cluster *Cluster) namespaces() []string {
	seen := make(map[string]struct{})
	for _, spec := range resourceSpecs {
		for _, object := range cluster.cacheSnapshot(spec.resource) {
			namespace := object.meta().Namespace
			if namespace != "" {
				seen[namespace] = struct{}{}
			}
		}
	}
	namespaces := make([]string, 0, len(seen))
	for namespace := range seen {
		namespaces = append(namespaces, namespace)
	}
	sort.Strings(namespaces)
	return namespaces
}
