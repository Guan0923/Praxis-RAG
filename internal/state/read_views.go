package state

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"strings"
)

// CredentialKinds are the provider settings replaced by redacted public views
// (the provider half of model_settings.KINDS in Python): each one owns a
// base_url/protected_key pair.
var CredentialKinds = []string{"embedding", "reranker", "vlm"}

// PublicSettings mirrors rag_mcp.knowledge_bases.public_settings: provider
// credentials are replaced with base_url/api_key_set so cached values can never
// leak a protected key.
func PublicSettings(settings map[string]any) map[string]any {
	public := make(map[string]any, len(settings))
	for key, value := range settings {
		credential := false
		for _, kind := range CredentialKinds {
			if key == kind {
				credential = true
				break
			}
		}
		if !credential {
			public[key] = value
		}
	}
	for _, kind := range CredentialKinds {
		provider := map[string]any{}
		if raw, ok := settings[kind].(map[string]any); ok {
			provider = raw
		}
		baseURL, _ := provider["base_url"].(string)
		protected, hasKey := provider["protected_key"]
		apiKeySet := false
		if hasKey {
			if text, ok := protected.(string); ok {
				apiKeySet = text != ""
			} else {
				apiKeySet = protected != nil
			}
		}
		public[kind] = map[string]any{"base_url": baseURL, "api_key_set": apiKeySet}
	}
	return public
}

// CachedReadViews loads every disposable public read model using only the
// connection supplied by cache synchronization. Repository reads remain
// authoritative. It is a port of rag_mcp.state.read_views.CachedReadViews.
type CachedReadViews struct {
	stores *Stores
}

// NewCachedReadViews registers the refresh loaders for every cached read model.
func NewCachedReadViews(stores *Stores) *CachedReadViews {
	views := &CachedReadViews{stores: stores}
	if stores.CacheSync != nil {
		stores.CacheSync.Register("kb_public", views.loadKnowledgeBase)
		stores.CacheSync.Register("kb_list", views.loadKnowledgeBases)
		stores.CacheSync.Register("documents", views.loadDocuments)
		stores.CacheSync.Register("catalog", views.loadCatalog)
		stores.CacheSync.Register("api_keys", views.loadAPIKeys)
		stores.CacheSync.Register("heads", views.loadHeads)
		stores.CacheSync.Register("leases", views.loadLeases)
		stores.CacheSync.Register("upload_list", views.loadUploadList)
		stores.CacheSync.Register("upload_status", views.loadUploadStatus)
	}
	return views
}

func (v *CachedReadViews) getOrLoad(ctx context.Context, kind, scope string) (any, error) {
	value, err := v.stores.CacheSync.GetOrLoad(ctx, kind, scope)
	if err != nil {
		var stateErr *StateError
		if errors.As(err, &stateErr) {
			return nil, err
		}
		return nil, newStateError("Cached read view cannot be loaded")
	}
	return value, nil
}

func viewLoadError(err error) error {
	if err == nil {
		return nil
	}
	return newStateError("Cached read view cannot be loaded")
}

// decodeCached detaches a cached value through JSON so hits match the payloads
// a serializing backend (Redis) would return.
func decodeCached[T any](value any) (T, bool, error) {
	var target T
	if value == nil {
		return target, false, nil
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return target, false, newStateError("Cached read view cannot be loaded")
	}
	if err := json.Unmarshal(encoded, &target); err != nil {
		return target, false, newStateError("Cached read view cannot be loaded")
	}
	return target, true, nil
}

// loaders ---------------------------------------------------------------------

func (v *CachedReadViews) knowledgeBaseSettings(ctx context.Context, querier viewQuerier, namespace string) (map[string]any, error) {
	var raw string
	err := querier.QueryRowContext(ctx,
		"SELECT settings FROM knowledge_bases WHERE namespace = ?", namespace).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, viewLoadError(err)
	}
	settings, err := decodeObject(raw)
	if err != nil {
		return nil, err
	}
	return settings, nil
}

func (v *CachedReadViews) loadKnowledgeBase(ctx context.Context, querier viewQuerier, namespace string) (any, error) {
	settings, err := v.knowledgeBaseSettings(ctx, querier, namespace)
	if err != nil || settings == nil {
		return nil, err
	}
	return PublicSettings(settings), nil
}

func (v *CachedReadViews) loadKnowledgeBases(ctx context.Context, querier viewQuerier, _ string) (any, error) {
	rows, err := querier.QueryContext(ctx, "SELECT namespace, settings FROM knowledge_bases")
	if err != nil {
		return nil, viewLoadError(err)
	}
	registered := make(map[string]map[string]any)
	for rows.Next() {
		var namespace, raw string
		if err := rows.Scan(&namespace, &raw); err != nil {
			_ = rows.Close()
			return nil, viewLoadError(err)
		}
		settings, err := decodeObject(raw)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		registered[namespace] = PublicSettings(settings)
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	counts := make(map[string]int)
	countRows, err := querier.QueryContext(ctx,
		"SELECT namespace, COUNT(*) FROM documents WHERE del_flag = 0 GROUP BY namespace")
	if err != nil {
		return nil, viewLoadError(err)
	}
	for countRows.Next() {
		var namespace string
		var count int64
		if err := countRows.Scan(&namespace, &count); err != nil {
			_ = countRows.Close()
			return nil, viewLoadError(err)
		}
		counts[namespace] = int(count)
	}
	if err := countRows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := countRows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	namespaces := make([]string, 0, len(registered)+len(counts))
	seen := make(map[string]struct{}, len(registered)+len(counts))
	for namespace := range registered {
		seen[namespace] = struct{}{}
		namespaces = append(namespaces, namespace)
	}
	for namespace := range counts {
		if _, ok := seen[namespace]; !ok {
			namespaces = append(namespaces, namespace)
		}
	}
	sort.Strings(namespaces)
	items := make([]map[string]any, 0, len(namespaces))
	for _, namespace := range namespaces {
		item := make(map[string]any)
		if settings, ok := registered[namespace]; ok {
			for key, value := range settings {
				item[key] = value
			}
		}
		if _, ok := item["namespace"]; !ok {
			item["namespace"] = namespace
		}
		_, configured := registered[namespace]
		item["configured"] = configured
		item["document_count"] = counts[namespace]
		items = append(items, item)
	}
	return items, nil
}

// documentViewQuery mirrors repositories._document_select for the public view.
const documentViewQuery = "SELECT d.name, d.document_id, d.size, d.sha256, d.chunking, d.collection, " +
	"d.chunk_count, d.modified_at, d.indexed_at, d.diagnostics, d.skipped_object_count, h.version " +
	"FROM documents d LEFT JOIN document_heads h ON d.namespace = h.namespace AND d.document_id = h.document_id " +
	"WHERE d.del_flag = 0 AND d.namespace = ?"

// documentViewRecord renders one document row like repositories._document_record.
func documentViewRecord(record DocumentRecord) map[string]any {
	item := map[string]any{
		"name":                 record.Name,
		"size":                 record.Size,
		"modified_at":          record.ModifiedAt,
		"indexed_at":           record.IndexedAt,
		"document_id":          record.DocumentID,
		"chunk_count":          record.ChunkCount,
		"collection":           record.Collection,
		"diagnostics":          record.Diagnostics,
		"skipped_object_count": record.SkippedObjectCount,
		"sha256":               record.SHA256,
		"chunking":             record.Chunking,
	}
	if record.OperationID != "" {
		item["operation_id"] = record.OperationID
	}
	return item
}

func (v *CachedReadViews) loadDocuments(ctx context.Context, querier viewQuerier, namespace string) (any, error) {
	settings, err := v.knowledgeBaseSettings(ctx, querier, namespace)
	if err != nil {
		return nil, err
	}
	rows, err := querier.QueryContext(ctx, documentViewQuery, namespace)
	if err != nil {
		return nil, viewLoadError(err)
	}
	records := make([]map[string]any, 0)
	for rows.Next() {
		record, err := scanDocument(rows)
		if err != nil {
			_ = rows.Close()
			return nil, err
		}
		item := documentViewRecord(record)
		if len(settings) > 0 {
			if collection, ok := settings["collection"]; ok {
				item["collection"] = collection
			}
			chunking := map[string]any{}
			if existing, ok := item["chunking"].(map[string]any); ok {
				for key, value := range existing {
					chunking[key] = value
				}
			}
			if model, ok := settings["embedding_model"]; ok {
				chunking["embedding_model"] = model
			}
			item["chunking"] = chunking
		}
		records = append(records, item)
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	sort.SliceStable(records, func(left, right int) bool {
		leftName, _ := records[left]["name"].(string)
		rightName, _ := records[right]["name"].(string)
		return strings.ToLower(leftName) < strings.ToLower(rightName)
	})
	return records, nil
}

func (v *CachedReadViews) loadCatalog(ctx context.Context, querier viewQuerier, namespace string) (any, error) {
	var data string
	err := querier.QueryRowContext(ctx,
		"SELECT data FROM mcp_catalogs WHERE namespace = ?", namespace).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, viewLoadError(err)
	}
	return data, nil
}

func (v *CachedReadViews) loadAPIKeys(ctx context.Context, querier viewQuerier, _ string) (any, error) {
	rows, err := querier.QueryContext(ctx,
		"SELECT "+apiKeySelectColumns+" FROM api_keys ORDER BY created_at DESC, id DESC")
	if err != nil {
		return nil, viewLoadError(err)
	}
	keys := make([]APIKey, 0)
	for rows.Next() {
		key, err := scanAPIKey(rows)
		if err != nil {
			_ = rows.Close()
			return nil, viewLoadError(err)
		}
		keys = append(keys, key)
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	return keys, nil
}

func (v *CachedReadViews) loadHeads(ctx context.Context, querier viewQuerier, namespace string) (any, error) {
	rows, err := querier.QueryContext(ctx,
		"SELECT h.document_id, h.version FROM document_heads h "+
			"LEFT JOIN documents d ON d.namespace = h.namespace AND d.document_id = h.document_id "+
			"WHERE h.namespace = ? AND (d.del_flag = 0 OR d.document_id IS NULL)", namespace)
	if err != nil {
		return nil, viewLoadError(err)
	}
	versions := make(map[string]string)
	for rows.Next() {
		var documentID, version string
		if err := rows.Scan(&documentID, &version); err != nil {
			_ = rows.Close()
			return nil, viewLoadError(err)
		}
		versions[documentID] = version
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	return versions, nil
}

func (v *CachedReadViews) loadLeases(ctx context.Context, querier viewQuerier, namespace string) (any, error) {
	rows, err := querier.QueryContext(ctx,
		"SELECT resource, owner, operation_id, expires_at FROM document_leases "+
			"WHERE namespace = ? AND resource != ? ORDER BY resource", namespace, "namespace:guard")
	if err != nil {
		return nil, viewLoadError(err)
	}
	records := make([]map[string]any, 0)
	for rows.Next() {
		var resource, owner string
		var operationID sql.NullString
		var expiresAt float64
		if err := rows.Scan(&resource, &owner, &operationID, &expiresAt); err != nil {
			_ = rows.Close()
			return nil, viewLoadError(err)
		}
		var operation any
		if operationID.Valid {
			operation = operationID.String
		}
		records = append(records, map[string]any{
			"resource":     resource,
			"holder_id":    sha256Hex(owner)[:16],
			"operation_id": operation,
			"expires_at":   expiresAt,
		})
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	return records, nil
}

const uploadSummaryColumns = "id, namespace, name, document_id, collection, old_version, status, error, " +
	"cleanup_pending, created_at, updated_at"

func scanUploadSummary(scanner rowScanner) (map[string]any, error) {
	var (
		id, namespace, name, documentID, collection string
		oldVersion, errorText                       sql.NullString
		status                                      string
		cleanupPending                              bool
		createdAt, updatedAt                        float64
	)
	if err := scanner.Scan(&id, &namespace, &name, &documentID, &collection, &oldVersion,
		&status, &errorText, &cleanupPending, &createdAt, &updatedAt); err != nil {
		return nil, err
	}
	var oldVersionValue any
	if oldVersion.Valid {
		oldVersionValue = oldVersion.String
	}
	var errorValue any
	if errorText.Valid {
		errorValue = errorText.String
	}
	return map[string]any{
		"id":              id,
		"namespace":       namespace,
		"name":            name,
		"document_id":     documentID,
		"collection":      collection,
		"old_version":     oldVersionValue,
		"status":          status,
		"error":           errorValue,
		"cleanup_pending": cleanupPending,
		"created_at":      createdAt,
		"updated_at":      updatedAt,
	}, nil
}

func (v *CachedReadViews) loadUploadList(ctx context.Context, querier viewQuerier, namespace string) (any, error) {
	rows, err := querier.QueryContext(ctx,
		"SELECT "+uploadSummaryColumns+" FROM upload_operations WHERE namespace = ? "+
			"ORDER BY created_at DESC, id DESC LIMIT 100", namespace)
	if err != nil {
		return nil, viewLoadError(err)
	}
	records := make([]map[string]any, 0)
	for rows.Next() {
		record, err := scanUploadSummary(rows)
		if err != nil {
			_ = rows.Close()
			return nil, viewLoadError(err)
		}
		records = append(records, record)
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	return records, nil
}

func (v *CachedReadViews) loadUploadStatus(ctx context.Context, querier viewQuerier, operationID string) (any, error) {
	record, err := scanUploadSummary(querier.QueryRowContext(ctx,
		"SELECT "+uploadSummaryColumns+" FROM upload_operations WHERE id = ?", operationID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, viewLoadError(err)
	}
	var owner string
	if err := querier.QueryRowContext(ctx,
		"SELECT owner FROM upload_operations WHERE id = ?", operationID).Scan(&owner); err != nil {
		return nil, viewLoadError(err)
	}
	record["version"] = record["id"]
	record["holder_id"] = sha256Hex(owner)[:16]
	rows, err := querier.QueryContext(ctx,
		"SELECT name, status, error, started_at, finished_at FROM upload_steps "+
			"WHERE operation_id = ? ORDER BY started_at, name LIMIT 257", operationID)
	if err != nil {
		return nil, viewLoadError(err)
	}
	steps := make([]any, 0)
	for rows.Next() {
		var (
			name, status string
			errorText    sql.NullString
			startedAt    float64
			finishedAt   sql.NullFloat64
		)
		if err := rows.Scan(&name, &status, &errorText, &startedAt, &finishedAt); err != nil {
			_ = rows.Close()
			return nil, viewLoadError(err)
		}
		var errorValue any
		if errorText.Valid {
			errorValue = errorText.String
		}
		var finishedValue any
		if finishedAt.Valid {
			finishedValue = finishedAt.Float64
		}
		steps = append(steps, map[string]any{
			"name":        name,
			"status":      status,
			"error":       errorValue,
			"started_at":  startedAt,
			"finished_at": finishedValue,
		})
	}
	if err := rows.Close(); err != nil {
		return nil, viewLoadError(err)
	}
	if err := rows.Err(); err != nil {
		return nil, viewLoadError(err)
	}
	truncated := len(steps) > 256
	if truncated {
		steps = steps[:256]
	}
	record["steps"] = steps
	record["steps_truncated"] = truncated
	return record, nil
}

// Accessors -------------------------------------------------------------------

// KnowledgeBase returns the redacted public settings, nil when absent.
func (v *CachedReadViews) KnowledgeBase(ctx context.Context, namespace string) (map[string]any, error) {
	value, err := v.getOrLoad(ctx, "kb_public", namespace)
	if err != nil {
		return nil, err
	}
	settings, found, err := decodeCached[map[string]any](value)
	if err != nil || !found {
		return nil, err
	}
	return settings, nil
}

// KnowledgeBases returns the merged configuration/document list.
func (v *CachedReadViews) KnowledgeBases(ctx context.Context) ([]map[string]any, error) {
	value, err := v.getOrLoad(ctx, "kb_list", "")
	if err != nil {
		return nil, err
	}
	items, found, err := decodeCached[[]map[string]any](value)
	if err != nil {
		return nil, err
	}
	if !found {
		return []map[string]any{}, nil
	}
	return items, nil
}

// Documents returns the public document list overlaid with active settings.
func (v *CachedReadViews) Documents(ctx context.Context, namespace string) ([]map[string]any, error) {
	value, err := v.getOrLoad(ctx, "documents", namespace)
	if err != nil {
		return nil, err
	}
	items, found, err := decodeCached[[]map[string]any](value)
	if err != nil {
		return nil, err
	}
	if !found {
		return []map[string]any{}, nil
	}
	return items, nil
}

// Catalog returns the stored catalog JSON; found=false when absent.
func (v *CachedReadViews) Catalog(ctx context.Context, namespace string) (string, bool, error) {
	value, err := v.getOrLoad(ctx, "catalog", namespace)
	if err != nil {
		return "", false, err
	}
	data, found, err := decodeCached[string](value)
	if err != nil {
		return "", false, err
	}
	return data, found, nil
}

// APIKeys is display-only metadata; never use it to authorize.
func (v *CachedReadViews) APIKeys(ctx context.Context) ([]APIKey, error) {
	value, err := v.getOrLoad(ctx, "api_keys", "")
	if err != nil {
		return nil, err
	}
	keys, found, err := decodeCached[[]APIKey](value)
	if err != nil {
		return nil, err
	}
	if !found {
		return []APIKey{}, nil
	}
	return keys, nil
}

// PublishedVersions is the display snapshot of document heads.
func (v *CachedReadViews) PublishedVersions(ctx context.Context, namespace string) (map[string]string, error) {
	value, err := v.getOrLoad(ctx, "heads", namespace)
	if err != nil {
		return nil, err
	}
	versions, found, err := decodeCached[map[string]string](value)
	if err != nil {
		return nil, err
	}
	if !found {
		return map[string]string{}, nil
	}
	return versions, nil
}

// Leases returns the display snapshot with expiry recomputed per call.
func (v *CachedReadViews) Leases(ctx context.Context, namespace string) ([]map[string]any, error) {
	value, err := v.getOrLoad(ctx, "leases", namespace)
	if err != nil {
		return nil, err
	}
	records, found, err := decodeCached[[]map[string]any](value)
	if err != nil {
		return nil, err
	}
	if !found {
		return []map[string]any{}, nil
	}
	now := unixSeconds()
	results := make([]map[string]any, 0, len(records))
	for _, record := range records {
		expires, _ := record["expires_at"].(float64)
		item := make(map[string]any, len(record)+1)
		for key, value := range record {
			item[key] = value
		}
		item["expired"] = expires <= now
		results = append(results, item)
	}
	return results, nil
}

// UploadOperations returns the latest 100 display operations.
func (v *CachedReadViews) UploadOperations(ctx context.Context, namespace string) ([]map[string]any, error) {
	value, err := v.getOrLoad(ctx, "upload_list", namespace)
	if err != nil {
		return nil, err
	}
	records, found, err := decodeCached[[]map[string]any](value)
	if err != nil {
		return nil, err
	}
	if !found {
		return []map[string]any{}, nil
	}
	return records, nil
}

// UploadOperation returns one operation's display detail; UUIDs never grant
// cross-namespace access.
func (v *CachedReadViews) UploadOperation(ctx context.Context, namespace, operationID string) (map[string]any, error) {
	value, err := v.getOrLoad(ctx, "upload_status", operationID)
	if err != nil {
		return nil, err
	}
	result, found, err := decodeCached[map[string]any](value)
	if err != nil || !found {
		return nil, err
	}
	if owner, ok := result["namespace"].(string); !ok || owner != namespace {
		return nil, nil
	}
	return result, nil
}
