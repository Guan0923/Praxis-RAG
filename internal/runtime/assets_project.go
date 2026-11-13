package runtime

import (
	"context"
	"encoding/base64"
	"fmt"
	"reflect"
	"strconv"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// assetFetcher is the Qdrant asset lookup seam consumed by the projector. It is
// implemented by *qdrant.Store and faked in tests.
type assetFetcher interface {
	GetAssets(ctx context.Context, assetIDs []string) (map[string]map[string]any, error)
}

// assetObjectReader resolves a staged asset file. Versioned uploads keep their
// parser artifacts in the upload journal object store; legacy documents fall
// back to the immutable per-namespace blob bucket (pdf_assets.project_results
// reads exactly these two sources).
type assetObjectReader interface {
	ReadAssetObject(ctx context.Context, version, bucket, key string) ([]byte, bool)
}

// storeAssetObjects reads asset bytes from the upload journal first and the
// blob store for unversioned (legacy) records.
type storeAssetObjects struct {
	journals  *state.UploadJournal
	blobs     *state.BlobStore
	namespace string
}

// ReadAssetObject implements assetObjectReader.
func (o storeAssetObjects) ReadAssetObject(ctx context.Context, version, bucket, key string) ([]byte, bool) {
	if version != "" {
		if o.journals == nil {
			return nil, false
		}
		content, found, err := o.journals.GetObject(ctx, version, bucket, key)
		if err != nil || !found {
			return nil, false
		}
		return content, true
	}
	if o.blobs == nil {
		return nil, false
	}
	content, found, err := o.blobs.Get(ctx, o.namespace, bucket, key)
	if err != nil || !found {
		return nil, false
	}
	return content, true
}

// newAssetProjector returns the retrieval orchestrator projector that hydrates
// asset hits from their Qdrant asset records, mirroring
// rag_mcp.pdf_assets.project_results.
//
// The retrieval.Orchestrator projector hook has no context parameter (Python's
// projector also runs synchronously inside the request thread), so the Qdrant
// and blob reads use a background context. Failures are absorbed: an asset
// lookup error returns the plain candidates instead of failing retrieval.
func newAssetProjector(fetcher assetFetcher, objects assetObjectReader) func([]contracts.Candidate) []map[string]any {
	if fetcher == nil {
		return nil
	}
	return func(candidates []contracts.Candidate) []map[string]any {
		return projectAssetResults(context.Background(), fetcher, objects, candidates)
	}
}

// projectAssetResults ports pdf_assets.project_results: candidates with
// metadata.asset_id are hydrated with the matching asset payload, stale upload
// versions and missing assets are dropped exactly like Python, and candidates
// without an asset id keep the plain Candidate.ToResult projection.
func projectAssetResults(
	ctx context.Context,
	fetcher assetFetcher,
	objects assetObjectReader,
	candidates []contracts.Candidate,
) []map[string]any {
	assetIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		if assetID := projectionString(candidate.Metadata["asset_id"], ""); assetID != "" {
			assetIDs = append(assetIDs, assetID)
		}
	}
	if len(assetIDs) == 0 {
		return plainCandidateResults(candidates)
	}
	assets, err := fetcher.GetAssets(ctx, assetIDs)
	if err != nil {
		logging.FromContext(ctx).Warn("search asset projection unavailable; returning unprojected results", logging.ErrorAttrs(err)...)
		return plainCandidateResults(candidates)
	}
	results := make([]map[string]any, 0, len(candidates))
	for index, candidate := range candidates {
		rank := index + 1
		assetID := projectionString(candidate.Metadata["asset_id"], "")
		if assetID == "" {
			results = append(results, candidate.ToResult(rank))
			continue
		}
		asset := assets[assetID]
		if asset == nil {
			// Python skips hits whose asset record is not visible (foreign or
			// deleted asset point).
			continue
		}
		if !projectionValuesEqual(candidate.Metadata["upload_version"], asset["upload_version"]) {
			// A stale upload version must never expose an old asset.
			continue
		}
		word := projectionString(asset["asset_storage"], "") == "word"
		caption := projectionString(asset["caption"], "")
		metadata := cloneProjectionMap(candidate.Metadata)
		metadata["asset_id"] = assetID
		metadata["asset_kind"] = asset["asset_kind"]
		metadata["page"] = asset["page"]
		metadata["bbox"] = asset["bbox"]
		metadata["captions"] = projectionFieldDefault(asset, "captions", []any{})
		metadata["footnotes"] = projectionFieldDefault(asset, "footnotes", []any{})
		metadata["caption_source"] = projectionFieldDefault(asset, "caption_source", "none")
		if word {
			delete(metadata, "page")
			delete(metadata, "bbox")
			metadata["location"] = asset["location"]
		}

		hit := map[string]any{
			"rank":        rank,
			"document_id": candidate.DocumentID,
			"chunk_id":    candidate.ChunkID,
			"title":       candidate.Title,
			"source":      candidate.Source,
			"score":       roundProjectionScore(candidate.Score),
			"metadata":    metadata,
		}
		if projectionString(asset["asset_kind"], "") == "picture" {
			relative, ok := asset["image_file"].(string)
			if !ok {
				continue
			}
			bucket := state.PDFAssetBucket
			if word {
				bucket = state.WordAssetBucket
			}
			version := projectionString(candidate.Metadata["upload_version"], "")
			content, found := objects.ReadAssetObject(ctx, version, bucket, relative)
			if !found {
				continue
			}
			image := map[string]any{
				"type":      "image",
				"mime_type": projectionString(asset["mime_type"], "image/png"),
				"data":      base64.StdEncoding.EncodeToString(content),
				"caption":   caption,
				"page":      asset["page"],
				"bbox":      asset["bbox"],
			}
			if word {
				delete(image, "page")
				delete(image, "bbox")
				image["location"] = asset["location"]
			}
			hit["asset"] = image
		} else {
			table, ok := asset["table"].(map[string]any)
			if !ok {
				continue
			}
			rendered := map[string]any{
				"type":        "table",
				"caption":     caption,
				"page":        asset["page"],
				"bbox":        asset["bbox"],
				"rows":        projectionTableValue(table, "rows", []any{}),
				"headers":     projectionTableValue(table, "headers", []any{}),
				"header_rows": projectionTableValue(table, "header_rows", []any{}),
				"cells":       projectionTableValue(table, "cells", []any{}),
				"num_rows":    projectionTableValue(table, "num_rows", 0),
				"num_cols":    projectionTableValue(table, "num_cols", 0),
				"footnotes":   projectionTableValue(table, "footnotes", []any{}),
			}
			if word {
				rendered["nested_tables"] = projectionTableValue(table, "nested_tables", map[string]any{})
				rendered["parent_cell"] = table["parent_cell"]
			}
			if word {
				delete(rendered, "page")
				delete(rendered, "bbox")
				rendered["location"] = asset["location"]
			}
			hit["asset"] = rendered
		}
		results = append(results, hit)
	}
	return results
}

// plainCandidateResults projects candidates without asset hydration, used both
// when no hit has an asset id and when the asset lookup is unavailable.
func plainCandidateResults(candidates []contracts.Candidate) []map[string]any {
	results := make([]map[string]any, 0, len(candidates))
	for index, candidate := range candidates {
		results = append(results, candidate.ToResult(index+1))
	}
	return results
}

// cloneProjectionMap shallow-copies a metadata object, matching Python's
// {**candidate.metadata, ...} update without mutating the candidate.
func cloneProjectionMap(source map[string]any) map[string]any {
	cloned := make(map[string]any, len(source)+8)
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

// projectionString mirrors Python's str(value or default) for the scalar asset
// fields: missing/null/false values become the default (or ""), while numbers
// and booleans use Python's text spelling.
func projectionString(value any, fallback string) string {
	switch typed := value.(type) {
	case nil:
		return fallback
	case string:
		if typed == "" {
			return fallback
		}
		return typed
	case bool:
		if !typed {
			return fallback
		}
		return "True"
	case float64:
		if typed == 0 {
			return fallback
		}
		return strconv.FormatFloat(typed, 'g', -1, 64)
	case int:
		if typed == 0 {
			return fallback
		}
		return strconv.Itoa(typed)
	case int64:
		if typed == 0 {
			return fallback
		}
		return strconv.FormatInt(typed, 10)
	default:
		return fmt.Sprint(value)
	}
}

// projectionValuesEqual mirrors Python's strict version comparison: absent
// versions compare equal (only then may an unversioned asset be projected).
func projectionValuesEqual(left, right any) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return reflect.DeepEqual(left, right)
}
func projectionList(value any) []any {
	if list, ok := value.([]any); ok {
		return list
	}
	return []any{}
}

// projectionTableValue reads one table field with the Python dict.get default.
func projectionTableValue(table map[string]any, key string, fallback any) any {
	if value, ok := table[key]; ok {
		return value
	}
	return fallback
}

// projectionFieldDefault mirrors dict.get(key, default): the default is only
// used when the key is absent (an explicit null stays null).
func projectionFieldDefault(values map[string]any, key string, fallback any) any {
	if value, ok := values[key]; ok {
		return value
	}
	return fallback
}

// roundProjectionScore mirrors Python's round(score, 6), matching
// contracts.Candidate.ToResult.
func roundProjectionScore(value float64) float64 {
	text := strconv.FormatFloat(value, 'f', 6, 64)
	rounded, err := strconv.ParseFloat(text, 64)
	if err != nil {
		return value
	}
	return rounded
}
