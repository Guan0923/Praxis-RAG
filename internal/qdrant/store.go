package qdrant

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
)

// Named vector names used by the Phase-3 dual-vector schema.
const (
	DenseVectorName  = "dense"
	SparseVectorName = "sparse"
)

// ErrLocalPathUnsupported documents the one Python qdrant-client feature the
// Go backend deliberately does not implement.
var ErrLocalPathUnsupported = errors.New(
	"QDRANT_PATH local-file mode is not supported by the Go backend; configure QDRANT_URL instead",
)

// StoreConfig mirrors rag_mcp.storage.qdrant_store.QdrantStore arguments.
type StoreConfig struct {
	Collection     string
	URL            string
	Path           string
	VectorSize     int
	Namespace      string
	ActiveVersions func() map[string]string
	APIKey         string
	HTTPClient     *http.Client
}

// Store owns dual-vector collection setup, idempotent writes, and scoped
// queries. It is safe for concurrent use when the injected HTTP client is.
type Store struct {
	collection     string
	vectorSize     int
	namespace      string
	activeVersions func() map[string]string
	client         *Client
}

// Chunk is the storage-facing projection of a document chunk.
type Chunk struct {
	DocumentID string
	ChunkID    string
	Text       string
	Title      string
	Source     string
	Metadata   map[string]any
}

// Point is one Qdrant point as returned by query and scroll responses.
type Point struct {
	ID      PointID        `json:"id"`
	Version int64          `json:"version,omitempty"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
	Vector  map[string]any `json:"vector"`
}

// PointID accepts Qdrant's string or numeric point id representation.
type PointID string

// UnmarshalJSON accepts both UUID strings and integer point ids.
func (id *PointID) UnmarshalJSON(data []byte) error {
	if string(data) == "null" {
		*id = ""
		return nil
	}
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		*id = PointID(text)
		return nil
	}
	*id = PointID(strings.TrimSpace(string(data)))
	return nil
}

// String returns the point id text used for deduplication and deletes.
func (id PointID) String() string { return string(id) }

// NewStore validates the configuration and builds the REST client. Python's
// url/path exclusivity check is preserved; local-file mode is rejected with an
// explicit message instead of silently opening a different backend.
func NewStore(cfg StoreConfig) (*Store, error) {
	if cfg.URL != "" && cfg.Path != "" {
		return nil, fmt.Errorf("Set only one of url and path")
	}
	if cfg.Path != "" {
		return nil, ErrLocalPathUnsupported
	}
	vectorSize := cfg.VectorSize
	if vectorSize <= 0 {
		vectorSize = config.DenseVectorSize
	}
	client := NewClient(cfg.URL, cfg.APIKey, cfg.HTTPClient)
	return &Store{
		collection:     cfg.Collection,
		vectorSize:     vectorSize,
		namespace:      strings.TrimSpace(cfg.Namespace),
		activeVersions: cfg.ActiveVersions,
		client:         client,
	}, nil
}

// NewStoreWithClient builds a store around an existing client (used when a
// reindex must write into a second collection on the same Qdrant endpoint).
func NewStoreWithClient(cfg StoreConfig, client *Client) *Store {
	vectorSize := cfg.VectorSize
	if vectorSize <= 0 {
		vectorSize = config.DenseVectorSize
	}
	return &Store{
		collection:     cfg.Collection,
		vectorSize:     vectorSize,
		namespace:      strings.TrimSpace(cfg.Namespace),
		activeVersions: cfg.ActiveVersions,
		client:         client,
	}
}

// Collection returns the collection name.
func (s *Store) Collection() string { return s.collection }

// Namespace returns the trimmed namespace ("" means none).
func (s *Store) Namespace() string { return s.namespace }

// VectorSize returns the configured dense dimension.
func (s *Store) VectorSize() int { return s.vectorSize }

func (s *Store) collectionPath(suffix string) string {
	return "/collections/" + url.PathEscape(s.collection) + suffix
}

type collectionResponse struct {
	Config struct {
		Params struct {
			SparseVectors map[string]json.RawMessage `json:"sparse_vectors"`
		} `json:"params"`
	} `json:"config"`
}

// CollectionExists reports whether the collection exists.
func (s *Store) CollectionExists(ctx context.Context) (bool, error) {
	var result struct {
		Exists bool `json:"exists"`
	}
	if err := s.client.get(ctx, s.collectionPath("/exists"), &result); err != nil {
		return false, err
	}
	return result.Exists, nil
}

// EnsureCollection creates the dual-vector collection or verifies that an
// existing collection carries the sparse vector (Phase-3 migration guard).
func (s *Store) EnsureCollection(ctx context.Context) error {
	exists, err := s.CollectionExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		var response collectionResponse
		if err := s.client.get(ctx, s.collectionPath(""), &response); err != nil {
			return err
		}
		configured := response.Config.Params.SparseVectors
		if _, ok := configured[SparseVectorName]; !ok {
			return fmt.Errorf(
				"Qdrant collection '%s' uses the Phase-2 dense-only schema; "+
					"configure a new collection and reindex the preserved source documents",
				s.collection,
			)
		}
		return nil
	}
	body := map[string]any{
		"vectors": map[string]any{
			DenseVectorName: map[string]any{"size": s.vectorSize, "distance": "Cosine"},
		},
		"sparse_vectors": map[string]any{
			SparseVectorName: map[string]any{
				"index":    map[string]any{"on_disk": false},
				"modifier": "idf",
			},
		},
	}
	return s.client.put(ctx, s.collectionPath(""), nil, body, nil)
}

type sparseVectorJSON struct {
	Indices []uint32  `json:"indices"`
	Values  []float64 `json:"values"`
}

type pointWrite struct {
	ID      string         `json:"id"`
	Vector  map[string]any `json:"vector"`
	Payload map[string]any `json:"payload"`
}

type queryRequest struct {
	Prefetch    []prefetch `json:"prefetch,omitempty"`
	Query       any        `json:"query"`
	Using       string     `json:"using,omitempty"`
	Filter      *Filter    `json:"filter,omitempty"`
	Limit       int        `json:"limit"`
	WithVector  bool       `json:"with_vector"`
	WithPayload bool       `json:"with_payload"`
}

type prefetch struct {
	Query  any     `json:"query"`
	Using  string  `json:"using,omitempty"`
	Filter *Filter `json:"filter,omitempty"`
	Limit  int     `json:"limit"`
}

type fusionQuery struct {
	Fusion string `json:"fusion"`
}

type queryResponse struct {
	Points []Point `json:"points"`
}

type scrollRequest struct {
	Limit       int      `json:"limit"`
	Filter      *Filter  `json:"filter,omitempty"`
	WithPayload bool     `json:"with_payload"`
	WithVector  bool     `json:"with_vector"`
	Offset      *PointID `json:"offset,omitempty"`
}

type scrollResult struct {
	Points         []Point  `json:"points"`
	NextPageOffset *PointID `json:"next_page_offset"`
}

func (s *Store) upsert(ctx context.Context, points []pointWrite) error {
	return s.client.put(ctx, s.collectionPath("/points"), url.Values{"wait": {"true"}},
		map[string]any{"points": points}, nil)
}

func (s *Store) deleteByFilter(ctx context.Context, filter *Filter) error {
	return s.client.post(ctx, s.collectionPath("/points/delete"), url.Values{"wait": {"true"}},
		map[string]any{"filter": filter}, nil)
}

func (s *Store) deleteByIDs(ctx context.Context, ids []string) error {
	return s.client.post(ctx, s.collectionPath("/points/delete"), url.Values{"wait": {"true"}},
		map[string]any{"points": ids}, nil)
}

func (s *Store) query(ctx context.Context, request queryRequest) ([]Point, error) {
	var result queryResponse
	if err := s.client.post(ctx, s.collectionPath("/points/query"), nil, request, &result); err != nil {
		return nil, err
	}
	return result.Points, nil
}

func (s *Store) scrollAll(
	ctx context.Context,
	filter *Filter,
	limit int,
	withVectors bool,
	visit func(page []Point) error,
) error {
	var offset *PointID
	for {
		var result scrollResult
		request := scrollRequest{
			Limit: limit, Filter: filter, WithPayload: true, WithVector: withVectors, Offset: offset,
		}
		if err := s.client.post(ctx, s.collectionPath("/points/scroll"), nil, request, &result); err != nil {
			return err
		}
		if err := visit(result.Points); err != nil {
			return err
		}
		if result.NextPageOffset == nil {
			return nil
		}
		offset = result.NextPageOffset
	}
}

// StageVersion idempotently upserts one unpublished version without touching
// other points. Visibility is controlled only by the active_versions provider.
func (s *Store) StageVersion(
	ctx context.Context,
	chunks []Chunk,
	vectors [][]float64,
	sparseVectors []sparse.SparseEmbedding,
	assetPayloads []map[string]any,
	version string,
) (int, error) {
	if strings.TrimSpace(version) == "" {
		return 0, fmt.Errorf("version must not be empty")
	}
	if len(chunks) != len(vectors) {
		return 0, fmt.Errorf("Chunk and vector counts must match")
	}
	if sparseVectors != nil && len(chunks) != len(sparseVectors) {
		return 0, fmt.Errorf("Chunk and sparse vector counts must match")
	}
	if len(chunks) == 0 {
		return 0, nil
	}
	documentID, err := singleDocumentID(chunks, "stage_version accepts chunks from exactly one document")
	if err != nil {
		return 0, err
	}
	if strings.TrimSpace(documentID) == "" {
		return 0, fmt.Errorf("document_id must not be empty")
	}
	if err := s.validateDenseVectors(vectors); err != nil {
		return 0, err
	}

	points := make([]pointWrite, 0, len(chunks)+len(assetPayloads))
	for index, chunk := range chunks {
		vectorMap := map[string]any{DenseVectorName: vectors[index]}
		if sparseVectors != nil && len(sparseVectors[index].Indices) > 0 {
			vectorMap[SparseVectorName] = sparseVectorJSON{
				Indices: sparseVectors[index].Indices,
				Values:  sparseVectors[index].Values,
			}
		}
		metadata := copyAnyMap(chunk.Metadata)
		metadata["upload_version"] = version
		payload := map[string]any{
			"record_type":    "chunk",
			"document_id":    chunk.DocumentID,
			"chunk_id":       chunk.ChunkID,
			"text":           chunk.Text,
			"title":          chunk.Title,
			"source":         chunk.Source,
			"metadata":       metadata,
			"upload_version": version,
		}
		if s.namespace != "" {
			payload["namespace"] = s.namespace
		}
		points = append(points, pointWrite{
			ID:      s.versionPointID("chunk", chunk.ChunkID, version, documentID),
			Vector:  vectorMap,
			Payload: payload,
		})
	}
	for _, assetPayload := range assetPayloads {
		if assetPayload["document_id"] != documentID || !truthy(assetPayload["asset_id"]) {
			return 0, fmt.Errorf("Asset payload must belong to the staged document")
		}
		asset := copyAnyMap(assetPayload)
		delete(asset, "namespace")
		asset["record_type"] = "asset"
		asset["upload_version"] = version
		if s.namespace != "" {
			asset["namespace"] = s.namespace
		}
		points = append(points, pointWrite{
			ID:      s.versionPointID("asset", pyStringAny(assetPayload["asset_id"]), version, documentID),
			Vector:  map[string]any{},
			Payload: asset,
		})
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return 0, err
	}
	if err := s.upsert(ctx, points); err != nil {
		return 0, err
	}
	return len(chunks), nil
}

// ReplaceDocument replaces one document's legacy (unversioned) points with
// dense and optional sparse vectors, restoring the old index on failure.
func (s *Store) ReplaceDocument(
	ctx context.Context,
	chunks []Chunk,
	vectors [][]float64,
	sparseVectors []sparse.SparseEmbedding,
	assetPayloads []map[string]any,
	afterWrite func() error,
) (int, error) {
	if len(chunks) == 0 {
		return 0, nil
	}
	if len(chunks) != len(vectors) {
		return 0, fmt.Errorf("Chunk and vector counts must match")
	}
	if sparseVectors != nil && len(chunks) != len(sparseVectors) {
		return 0, fmt.Errorf("Chunk and sparse vector counts must match")
	}
	documentID, err := singleDocumentID(chunks, "replace_document accepts chunks from exactly one document")
	if err != nil {
		return 0, err
	}
	if err := s.validateDenseVectors(vectors); err != nil {
		return 0, err
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return 0, err
	}

	oldPoints := make([]Point, 0, 32)
	if err := s.scrollAll(ctx, s.versionScopeFilter(map[string]any{"document_id": documentID}), 256, true,
		func(page []Point) error {
			oldPoints = append(oldPoints, page...)
			return nil
		}); err != nil {
		return 0, err
	}

	points := make([]pointWrite, 0, len(chunks)+len(assetPayloads))
	for index, chunk := range chunks {
		vectorMap := map[string]any{DenseVectorName: vectors[index]}
		if sparseVectors != nil && len(sparseVectors[index].Indices) > 0 {
			vectorMap[SparseVectorName] = sparseVectorJSON{
				Indices: sparseVectors[index].Indices,
				Values:  sparseVectors[index].Values,
			}
		}
		payload := map[string]any{
			"record_type": "chunk",
			"document_id": chunk.DocumentID,
			"chunk_id":    chunk.ChunkID,
			"text":        chunk.Text,
			"title":       chunk.Title,
			"source":      chunk.Source,
			"metadata":    copyAnyMap(chunk.Metadata),
		}
		if s.namespace != "" {
			payload["namespace"] = s.namespace
		}
		points = append(points, pointWrite{
			ID:      s.pointID(chunk.ChunkID),
			Vector:  vectorMap,
			Payload: payload,
		})
	}
	for _, assetPayload := range assetPayloads {
		if assetPayload["document_id"] != documentID || !truthy(assetPayload["asset_id"]) {
			return 0, fmt.Errorf("Asset payload must belong to the replaced document")
		}
		asset := copyAnyMap(assetPayload)
		if s.namespace != "" {
			asset["namespace"] = s.namespace
		}
		points = append(points, pointWrite{
			ID:      s.pointID(pyStringAny(assetPayload["asset_id"])),
			Vector:  map[string]any{},
			Payload: asset,
		})
	}

	writeErr := s.upsert(ctx, points)
	if writeErr == nil {
		keep := make(map[string]struct{}, len(points))
		for _, point := range points {
			keep[point.ID] = struct{}{}
		}
		stale := make([]string, 0, len(oldPoints))
		for _, oldPoint := range oldPoints {
			if _, ok := keep[oldPoint.ID.String()]; !ok {
				stale = append(stale, oldPoint.ID.String())
			}
		}
		if len(stale) > 0 {
			writeErr = s.deleteByIDs(ctx, stale)
		}
	}
	if writeErr == nil && afterWrite != nil {
		writeErr = afterWrite()
	}
	if writeErr != nil {
		newIDs := make([]string, len(points))
		for index, point := range points {
			newIDs[index] = point.ID
		}
		if restoreErr := s.restoreDocument(ctx, oldPoints, newIDs); restoreErr != nil {
			return 0, fmt.Errorf("Qdrant replacement failed and the previous index could not be restored: %w", restoreErr)
		}
		return 0, writeErr
	}
	return len(chunks), nil
}

func (s *Store) restoreDocument(ctx context.Context, oldPoints []Point, newIDs []string) error {
	oldIDs := make(map[string]struct{}, len(oldPoints))
	for _, point := range oldPoints {
		oldIDs[point.ID.String()] = struct{}{}
	}
	introduced := make([]string, 0, len(newIDs))
	for _, pointID := range newIDs {
		if _, ok := oldIDs[pointID]; !ok {
			introduced = append(introduced, pointID)
		}
	}
	if len(oldPoints) > 0 {
		restore := make([]pointWrite, len(oldPoints))
		for index, point := range oldPoints {
			vector := point.Vector
			if vector == nil {
				vector = map[string]any{}
			}
			payload := point.Payload
			if payload == nil {
				payload = map[string]any{}
			}
			restore[index] = pointWrite{ID: point.ID.String(), Vector: vector, Payload: payload}
		}
		if err := s.upsert(ctx, restore); err != nil {
			return err
		}
	}
	if len(introduced) > 0 {
		if err := s.deleteByIDs(ctx, introduced); err != nil {
			return err
		}
	}
	return nil
}

// Embedder is the subset of the model API client needed by ReembedInto.
type Embedder interface {
	VectorSize() int
	EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error)
}

// ReembedInto copies the visible points into an unpublished index, replacing
// only dense vectors and verifying the copied chunk counts.
func (s *Store) ReembedInto(
	ctx context.Context,
	collection string,
	embedder Embedder,
	expectedChunks map[string]int,
) error {
	target := NewStoreWithClient(StoreConfig{
		Collection: collection,
		VectorSize: embedder.VectorSize(),
		Namespace:  s.namespace,
	}, s.client)
	if err := target.EnsureCollection(ctx); err != nil {
		return err
	}
	copied := map[string]int{}
	exists, err := s.CollectionExists(ctx)
	if err != nil {
		return err
	}
	if exists {
		if err := s.scrollAll(ctx, s.queryFilter(map[string]any{}), 64, true, func(points []Point) error {
			chunks := make([]Point, 0, len(points))
			for _, point := range points {
				if payloadString(point.Payload, "record_type") == "chunk" {
					chunks = append(chunks, point)
				}
			}
			var vectors [][]float64
			if len(chunks) > 0 {
				texts := make([]string, len(chunks))
				for index, chunk := range chunks {
					text, ok := chunk.Payload["text"].(string)
					if !ok {
						return fmt.Errorf("Qdrant point has an invalid evidence payload: text")
					}
					texts[index] = text
				}
				embedded, embedErr := embedder.EmbedDocuments(ctx, texts)
				if embedErr != nil {
					return embedErr
				}
				if len(embedded) != len(chunks) {
					return fmt.Errorf("Expected %d vectors, received %d", len(chunks), len(embedded))
				}
				vectors = embedded
			}
			dense := make(map[string][]float64, len(chunks))
			for index, chunk := range chunks {
				dense[chunk.ID.String()] = vectors[index]
			}
			replacements := make([]pointWrite, len(points))
			for index, point := range points {
				vector := copyAnyMap(point.Vector)
				if vectorDense, ok := dense[point.ID.String()]; ok {
					vector[DenseVectorName] = vectorDense
					documentID := pyStringAny(point.Payload["document_id"])
					copied[documentID]++
				}
				replacements[index] = pointWrite{
					ID:      point.ID.String(),
					Vector:  vector,
					Payload: point.Payload,
				}
			}
			if len(replacements) > 0 {
				body := map[string]any{"points": replacements}
				return s.client.put(ctx, target.collectionPath("/points"), url.Values{"wait": {"true"}}, body, nil)
			}
			return nil
		}); err != nil {
			return err
		}
	}
	if !reflect.DeepEqual(copied, expectedChunks) {
		return contracts.NewRagOperationError("文档清单与索引片段数量不一致，未切换模型；请检查现有索引")
	}
	return nil
}

// GetAssets resolves visible asset payloads by asset id.
func (s *Store) GetAssets(ctx context.Context, assetIDs []string) (map[string]map[string]any, error) {
	if len(assetIDs) == 0 {
		return map[string]map[string]any{}, nil
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return nil, err
	}
	query := s.queryFilter(map[string]any{"record_type": "asset"})
	unique := make([]any, 0, len(assetIDs))
	seen := make(map[string]struct{}, len(assetIDs))
	for _, assetID := range assetIDs {
		if _, duplicate := seen[assetID]; duplicate {
			continue
		}
		seen[assetID] = struct{}{}
		unique = append(unique, assetID)
	}
	query.Must = append(query.Must, fieldCondition{Key: "asset_id", Match: matchAny{Any: unique}})
	result := map[string]map[string]any{}
	if err := s.scrollAll(ctx, query, 256, false, func(points []Point) error {
		for _, point := range points {
			payload := point.Payload
			if payloadString(payload, "record_type") != "asset" {
				continue
			}
			if !s.matchesNamespace(payload) {
				continue
			}
			result[pyStringAny(payload["asset_id"])] = payload
		}
		return nil
	}); err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) matchesNamespace(payload map[string]any) bool {
	value, present := payload["namespace"]
	if s.namespace == "" {
		return !present || value == nil
	}
	text, ok := value.(string)
	return ok && text == s.namespace
}

// DeleteVersion deletes only this document's version in this namespace.
func (s *Store) DeleteVersion(ctx context.Context, documentID, version string) error {
	if strings.TrimSpace(documentID) == "" || strings.TrimSpace(version) == "" {
		return fmt.Errorf("document_id and version must not be empty")
	}
	exists, err := s.CollectionExists(ctx)
	if err != nil || !exists {
		return err
	}
	return s.deleteByFilter(ctx, s.versionScopeFilter(map[string]any{
		"document_id":    documentID,
		"upload_version": version,
	}))
}

// DeleteLegacy deletes only this document's unversioned scoped points.
func (s *Store) DeleteLegacy(ctx context.Context, documentID string) error {
	if strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("document_id must not be empty")
	}
	exists, err := s.CollectionExists(ctx)
	if err != nil || !exists {
		return err
	}
	query := s.versionScopeFilter(map[string]any{"document_id": documentID})
	query.Must = append(query.Must, isEmptyCondition{IsEmpty: payloadField{Key: "upload_version"}})
	return s.deleteByFilter(ctx, query)
}

// RemoveOldVersions removes every scoped version except keepVersion.
func (s *Store) RemoveOldVersions(ctx context.Context, documentID, keepVersion string) error {
	if strings.TrimSpace(documentID) == "" || strings.TrimSpace(keepVersion) == "" {
		return fmt.Errorf("document_id and keep_version must not be empty")
	}
	exists, err := s.CollectionExists(ctx)
	if err != nil || !exists {
		return err
	}
	query := s.versionScopeFilter(map[string]any{"document_id": documentID})
	query.MustNot = []any{fieldCondition{Key: "upload_version", Match: matchValue{Value: keepVersion}}}
	return s.deleteByFilter(ctx, query)
}

// CleanupVersions removes every scoped version except the published version.
func (s *Store) CleanupVersions(ctx context.Context, documentID, keepVersion string) error {
	return s.RemoveOldVersions(ctx, documentID, keepVersion)
}

// DeleteDocument deletes every vector belonging to one stable document id.
func (s *Store) DeleteDocument(ctx context.Context, documentID string) error {
	if strings.TrimSpace(documentID) == "" {
		return fmt.Errorf("document_id must not be empty")
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return err
	}
	return s.deleteByFilter(ctx, s.versionScopeFilter(map[string]any{"document_id": documentID}))
}

// DenseSearch queries the dense named vector and returns scored points.
func (s *Store) DenseSearch(
	ctx context.Context,
	vector []float64,
	limit int,
	filters map[string]any,
) ([]Point, error) {
	if len(vector) != s.vectorSize {
		return nil, fmt.Errorf("Dense query vector must have %d dimensions", s.vectorSize)
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return nil, err
	}
	return s.query(ctx, queryRequest{
		Query:       vector,
		Using:       DenseVectorName,
		Filter:      s.queryFilter(filters),
		Limit:       limit,
		WithVector:  false,
		WithPayload: true,
	})
}

// Search is the Phase-2 dense search compatibility alias.
func (s *Store) Search(
	ctx context.Context,
	vector []float64,
	limit int,
	filters map[string]any,
) ([]Point, error) {
	return s.DenseSearch(ctx, vector, limit, filters)
}

// SparseSearch queries the BM25 sparse named vector.
func (s *Store) SparseSearch(
	ctx context.Context,
	vector sparse.SparseEmbedding,
	limit int,
	filters map[string]any,
) ([]Point, error) {
	if len(vector.Indices) == 0 {
		return nil, nil
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return nil, err
	}
	return s.query(ctx, queryRequest{
		Query:       sparseVectorJSON{Indices: vector.Indices, Values: vector.Values},
		Using:       SparseVectorName,
		Filter:      s.queryFilter(filters),
		Limit:       limit,
		WithVector:  false,
		WithPayload: true,
	})
}

// HybridSearch prefetches dense and sparse candidates and fuses them with
// Qdrant's server-side RRF.
func (s *Store) HybridSearch(
	ctx context.Context,
	dense []float64,
	sparseVector sparse.SparseEmbedding,
	limit int,
	filters map[string]any,
) ([]Point, error) {
	if len(dense) != s.vectorSize {
		return nil, fmt.Errorf("Dense query vector must have %d dimensions", s.vectorSize)
	}
	if len(sparseVector.Indices) == 0 {
		return s.DenseSearch(ctx, dense, limit, filters)
	}
	if err := s.EnsureCollection(ctx); err != nil {
		return nil, err
	}
	queryFilter := s.queryFilter(filters)
	return s.query(ctx, queryRequest{
		Prefetch: []prefetch{
			{Query: dense, Using: DenseVectorName, Filter: queryFilter, Limit: limit},
			{
				Query:  sparseVectorJSON{Indices: sparseVector.Indices, Values: sparseVector.Values},
				Using:  SparseVectorName,
				Filter: queryFilter,
				Limit:  limit,
			},
		},
		Query:       fusionQuery{Fusion: "rrf"},
		Limit:       limit,
		WithVector:  false,
		WithPayload: true,
	})
}

// Count returns the number of points matching the collection (tests and
// maintenance helpers only; the Python pipeline never calls it).
func (s *Store) Count(ctx context.Context, exact bool) (int, error) {
	var result struct {
		Count int `json:"count"`
	}
	body := map[string]any{"exact": exact}
	if err := s.client.post(ctx, s.collectionPath("/points/count"), nil, body, &result); err != nil {
		return 0, err
	}
	return result.Count, nil
}

func (s *Store) validateDenseVectors(vectors [][]float64) error {
	for _, vector := range vectors {
		if len(vector) != s.vectorSize {
			return fmt.Errorf("Dense vector must have %d dimensions", s.vectorSize)
		}
	}
	return nil
}

func singleDocumentID(chunks []Chunk, message string) (string, error) {
	documentIDs := map[string]struct{}{}
	for _, chunk := range chunks {
		documentIDs[chunk.DocumentID] = struct{}{}
	}
	if len(documentIDs) != 1 {
		return "", fmt.Errorf("%s", message)
	}
	for documentID := range documentIDs {
		return documentID, nil
	}
	return "", fmt.Errorf("%s", message)
}

func copyAnyMap(values map[string]any) map[string]any {
	copied := make(map[string]any, len(values))
	for key, value := range values {
		copied[key] = value
	}
	return copied
}

func payloadString(payload map[string]any, key string) string {
	value, ok := payload[key].(string)
	if !ok {
		return ""
	}
	return value
}

func truthy(value any) bool {
	switch typed := value.(type) {
	case nil:
		return false
	case bool:
		return typed
	case string:
		return typed != ""
	case json.Number:
		parsed, err := typed.Float64()
		return err == nil && parsed != 0
	case float64:
		return typed != 0
	case int:
		return typed != 0
	case int64:
		return typed != 0
	default:
		return true
	}
}

// pyStringAny mirrors Python's str() for the payload scalars used as ids.
func pyStringAny(value any) string {
	switch typed := value.(type) {
	case nil:
		return "None"
	case string:
		return typed
	case bool:
		if typed {
			return "True"
		}
		return "False"
	case json.Number:
		return string(typed)
	case float64:
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case int:
		return strconv.Itoa(typed)
	case int64:
		return strconv.FormatInt(typed, 10)
	default:
		return fmt.Sprint(typed)
	}
}
