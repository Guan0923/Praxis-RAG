// Package searchrefs projects immutable artifact references onto an
// already-executed search response.
//
// It is a port of rag_mcp/state/search_references.py: search results are copied
// defensively and image/table assets and document sources become expiring
// state.ArtifactRegistry snapshots (artifact references are attached without
// removing the inline data the MCP renderer consumes). Search responses are no
// longer stored: the captured payload is returned to the caller directly.
//
// The capture is failure tolerant: artifact problems never discard the
// retrieval results. Failures are logged by category only (no credentials or
// request data) and the original payload is returned unchanged.
package searchrefs

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// artifactAssetNames are the parser side-car files projected from PDF/Word
// document storage, matching search_references.py.
var artifactAssetNames = []string{"document.md", "manifest.json", "document.json", "ocr.json"}

// artifactAssetKinds maps the side-car filename to the artifact kind.
var artifactAssetKinds = map[string]string{
	"document.md":   "markdown",
	"manifest.json": "manifest",
	"document.json": "document",
	"ocr.json":      "ocr",
}

// mimeTypes is a deterministic subset of Python mimetypes for the document
// extensions this service stores; unknown extensions fall back to
// application/octet-stream exactly like mimetypes.guess_type.
var mimeTypes = map[string]string{
	".txt":  "text/plain",
	".md":   "text/markdown",
	".pdf":  "application/pdf",
	".doc":  "application/msword",
	".docx": "application/vnd.openxmlformats-officedocument.wordprocessingml.document",
	".json": "application/json",
	".csv":  "text/csv",
	".html": "text/html",
	".htm":  "text/html",
	".xml":  "application/xml",
	".png":  "image/png",
	".jpg":  "image/jpeg",
	".jpeg": "image/jpeg",
	".gif":  "image/gif",
	".webp": "image/webp",
	".bmp":  "image/bmp",
	".tif":  "image/tiff",
	".tiff": "image/tiff",
	".xls":  "application/vnd.ms-excel",
	".xlsx": "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
	".ppt":  "application/vnd.ms-powerpoint",
	".pptx": "application/vnd.openxmlformats-officedocument.presentationml.presentation",
}

// Capture ports SearchReferences.capture.
type Capture struct {
	Stores *state.Stores
	Logger *slog.Logger
	Clock  func() float64
}

// New builds the capture service. A nil logger uses slog.Default; a nil clock
// uses wall time.
func New(stores *state.Stores, logger *slog.Logger) *Capture {
	if logger == nil {
		logger = slog.Default()
	}
	return &Capture{Stores: stores, Logger: logger}
}

// Capture returns the payload enriched with artifact references, the expiring
// artifact list and the artifact limits. The response is returned to the caller
// directly; nothing is persisted as a search result.
//
// An error is returned only for invalid input (nil stores). Artifact failures
// are tolerated: the original retrieval payload is returned and the failure is
// logged, never surfaced as a Go error.
func (c *Capture) Capture(ctx context.Context, namespace string, payload map[string]any) (map[string]any, error) {
	if c.Stores == nil {
		return nil, fmt.Errorf("searchrefs: state stores are required")
	}
	if namespace != "" {
		ctx = logging.WithNamespace(ctx, namespace)
	}
	original := deepCopyMap(payload)
	result := deepCopyMap(payload)
	if result == nil {
		result = map[string]any{}
	}
	created := c.now()
	settings := artifactSettings()
	expires := created + float64(settings.TTLSeconds)

	session := &captureSession{
		capture:   c,
		ctx:       ctx,
		namespace: namespace,
		settings:  settings,
		expires:   expires,
		records:   map[recordKey][]map[string]any{},
		seen:      map[string]map[string]any{},
	}

	if err := session.project(namespace, result); err != nil {
		c.warn(ctx, "search artifact capture unavailable", logging.ErrorAttrs(err)...)
		degraded := deepCopyMap(original)
		if degraded == nil {
			degraded = map[string]any{}
		}
		return degraded, nil
	}
	return result, nil
}

func (c *Capture) logger() *slog.Logger {
	if c == nil || c.Logger == nil {
		return slog.Default()
	}
	return c.Logger
}

// warn records one tolerated capture failure with the context correlation
// fields and the redacted structured error classification.
func (c *Capture) warn(ctx context.Context, message string, args ...any) {
	c.logger().Warn(message, append(logging.Attrs(ctx), args...)...)
}

func (c *Capture) now() float64 {
	if c != nil && c.Clock != nil {
		return c.Clock()
	}
	return float64(time.Now().UnixNano()) / float64(time.Second)
}

// recordKey is the (document_id, upload_version) cache key of the Python
// implementation.
type recordKey struct {
	documentID     string
	hasVersion     bool
	version        string
	invalidVersion bool
}

// captureSession is the per-call artifact bookkeeping.
type captureSession struct {
	capture   *Capture
	ctx       context.Context
	namespace string
	settings  state.SnapshotSettings
	expires   float64

	records map[recordKey][]map[string]any

	registered map[string]map[string]any
	order      []map[string]any
	seen       map[string]map[string]any

	count          int
	bytesUsed      int
	artifactErrors bool
}

// project walks the results, registers artifacts and attaches the references.
func (s *captureSession) project(namespace string, result map[string]any) error {
	s.registered = map[string]map[string]any{}
	s.order = make([]map[string]any, 0)
	s.seen = map[string]map[string]any{}

	results, ok := result["results"].([]any)
	if !ok {
		results = []any{}
	}
	for _, item := range results {
		hit, ok := item.(map[string]any)
		if !ok {
			continue
		}
		documentID := documentIDText(hit["document_id"])
		if documentID == "" {
			continue
		}
		version, hasVersion, invalidVersion := uploadVersion(hit)
		key := recordKey{documentID: documentID, hasVersion: hasVersion, version: version, invalidVersion: invalidVersion}
		references, cached := s.records[key]
		if !cached {
			references = make([]map[string]any, 0)
			record, err := s.capture.Stores.Documents.GetByID(s.ctx, namespace, documentID)
			if err != nil {
				return err
			}
			if record != nil {
				name := record.Name
				actualVersion := ""
				if hasVersion && !invalidVersion {
					actualVersion = version
				}
				if actualVersion == "" {
					actualVersion = record.OperationID
				}
				// A legacy hit must never silently link to a newer source version.
				if !hasVersion && actualVersion != "" {
					s.records[key] = references
					continue
				}
				if invalidVersion {
					// Python passes the malformed value to register(), which fails
					// per artifact; the net effect is the same warning.
					s.artifactErrors = true
				} else {
					var sourceErr error
					references, sourceErr = s.sourceReferences(namespace, documentID, name, actualVersion, hasVersion, references)
					if sourceErr != nil {
						return sourceErr
					}
				}
			}
			s.records[key] = references
		}
		hit["artifacts"] = references
		if err := s.projectAsset(namespace, documentID, version, hasVersion, invalidVersion, hit); err != nil {
			return err
		}
	}
	result["artifacts"] = s.order
	result["artifact_limits"] = map[string]any{
		"max_count": s.settings.MaxArtifactsPerSearch,
		"max_bytes": s.settings.MaxArtifactBytes,
	}
	if s.artifactErrors {
		result["artifact_warning"] = "Some artifact references could not be stored"
	}
	return nil
}

// sourceReferences registers the source bytes and the PDF/Word side-car files.
// Storage read failures are propagated (Python lets them fail the whole capture,
// which the caller degrades to the original results); artifact registration
// failures are tolerated and flagged.
func (s *captureSession) sourceReferences(
	namespace, documentID, name, actualVersion string,
	hasVersion bool,
	references []map[string]any,
) ([]map[string]any, error) {
	var sourceVersion *string
	if actualVersion != "" || hasVersion {
		value := actualVersion
		sourceVersion = &value
	}

	var source []byte
	var found bool
	var err error
	if actualVersion != "" {
		source, found, err = s.capture.Stores.Uploads.GetObject(s.ctx, actualVersion, state.SourceBucket, name)
	} else {
		source, found, err = s.capture.Stores.Blobs.Get(s.ctx, namespace, state.SourceBucket, name)
	}
	if err != nil {
		return references, err
	}
	if found {
		if reference := s.register(documentID, sourceVersion, "source", name, guessMIME(name), source); reference != nil {
			references = append(references, reference)
		}
	}

	bucket := ""
	lowered := strings.ToLower(name)
	switch {
	case strings.HasSuffix(lowered, ".pdf"):
		bucket = state.PDFAssetBucket
	case strings.HasSuffix(lowered, ".doc"), strings.HasSuffix(lowered, ".docx"):
		bucket = state.WordAssetBucket
	}
	if bucket == "" {
		return references, nil
	}
	for _, shortName := range artifactAssetNames {
		key := documentID + "/" + shortName
		var content []byte
		var assetFound bool
		if actualVersion != "" {
			content, assetFound, err = s.capture.Stores.Uploads.GetObject(s.ctx, actualVersion, bucket, key)
		} else {
			content, assetFound, err = s.capture.Stores.Blobs.Get(s.ctx, namespace, bucket, key)
		}
		if err != nil {
			return references, err
		}
		if !assetFound {
			continue
		}
		mime := "application/json"
		if shortName == "document.md" {
			mime = "text/markdown"
		}
		kind := artifactAssetKinds[shortName]
		if reference := s.register(documentID, sourceVersion, kind, shortName, mime, content); reference != nil {
			references = append(references, reference)
		}
	}
	return references, nil
}

// projectAsset mirrors the image/table branch: the artifact reference is added
// to the inline asset without removing its data (MCP rendering splits it).
func (s *captureSession) projectAsset(
	namespace, documentID, version string,
	hasVersion, invalidVersion bool,
	hit map[string]any,
) error {
	asset, ok := hit["asset"].(map[string]any)
	if !ok {
		return nil
	}
	assetType, _ := asset["type"].(string)
	if assetType != "image" && assetType != "table" {
		return nil
	}
	if hasVersion && invalidVersion {
		// Python passes the malformed value to register(), which fails per
		// artifact; the net effect is the same warning.
		s.artifactErrors = true
		return nil
	}
	var versionPtr *string
	if hasVersion {
		value := version
		versionPtr = &value
	}
	name := truncateRunes(assetName(hit, assetType), 512)

	switch assetType {
	case "image":
		data, isString := asset["data"].(string)
		if !isString {
			return nil
		}
		content, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return fmt.Errorf("invalid image asset data: %w", err)
		}
		mime, _ := asset["mime_type"].(string)
		if mime == "" {
			mime = "image/png"
		}
		reference := s.register(documentID, versionPtr, "image", name, mime, content)
		if reference != nil {
			asset["artifact_id"] = reference["artifact_id"]
			asset["resource_uri"] = reference["resource_uri"]
			asset["expires_at"] = reference["expires_at"]
		}
	case "table":
		encoded, err := encodeJSON(asset)
		if err != nil {
			return err
		}
		reference := s.register(documentID, versionPtr, "table", name, "application/json", encoded)
		if reference != nil {
			asset["artifact_id"] = reference["artifact_id"]
			asset["resource_uri"] = reference["resource_uri"]
			asset["expires_at"] = reference["expires_at"]
		}
	}
	return nil
}

// assetName mirrors str((hit.get("metadata") or {}).get("asset_id", default))[:512].
func assetName(hit map[string]any, assetType string) string {
	defaultName := "image"
	if assetType == "table" {
		defaultName = "table"
	}
	metadata, _ := hit["metadata"].(map[string]any)
	raw, present := metadata["asset_id"]
	if !present || raw == nil {
		return defaultName
	}
	if text, ok := raw.(string); ok {
		if text == "" {
			return defaultName
		}
		return text
	}
	return fmt.Sprint(raw)
}

// register commits one artifact unless the per-search count/byte limits are
// exceeded, deduplicating identical (document, version, kind, name, sha256)
// content. Registration failures are tolerated and flagged.
func (s *captureSession) register(documentID string, version *string, kind, name, mime string, content []byte) map[string]any {
	versionText := ""
	if version != nil {
		versionText = *version
	}
	digest := sha256.Sum256(content)
	identity := fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s\x00%s",
		documentID, boolToInt(version != nil), versionText, kind, name, hex.EncodeToString(digest[:]))
	if existing, ok := s.registered[identity]; ok {
		return existing
	}
	if s.count >= s.settings.MaxArtifactsPerSearch {
		s.artifactErrors = true
		return nil
	}
	if len(content)+s.bytesUsed > s.settings.MaxArtifactBytes {
		s.artifactErrors = true
		return nil
	}
	if s.capture.Stores.Artifacts == nil {
		s.artifactErrors = true
		return nil
	}
	artifact, err := s.capture.Stores.Artifacts.Register(
		s.ctx, s.namespace, documentID, version, kind, name, mime, content, &s.expires)
	if err != nil {
		s.artifactErrors = true
		return nil
	}
	reference := map[string]any{
		"artifact_id":  artifact.ArtifactID,
		"document_id":  artifact.DocumentID,
		"version":      artifactVersion(artifact.Version),
		"kind":         artifact.Kind,
		"name":         artifact.Name,
		"mime_type":    artifact.MimeType,
		"expires_at":   artifact.ExpiresAt,
		"resource_uri": artifact.ResourceURI,
	}
	s.registered[identity] = reference
	s.order = append(s.order, reference)
	s.count++
	s.bytesUsed += len(content)
	return reference
}

// artifactVersion renders the optional artifact version as a JSON value.
func artifactVersion(version *string) any {
	if version == nil {
		return nil
	}
	return *version
}

// ---- helpers ---------------------------------------------------------------

// artifactSettings returns the artifact retention settings. Search responses no
// longer persist a result snapshot, so the only remaining settings are the
// artifact TTL and limits (RAG_RESULT_TTL_SECONDS / RAG_ARTIFACT_MAX_BYTES /
// RAG_RESULT_MAX_ARTIFACTS). Startup already validates them; invalid values fall
// back to the defaults here.
func artifactSettings() state.SnapshotSettings {
	settings, err := state.SnapshotSettingsFromEnv()
	if err != nil {
		return state.DefaultSnapshotSettings()
	}
	return settings
}

func documentIDText(value any) string {
	switch typed := value.(type) {
	case nil:
		return ""
	case string:
		return typed
	case json.Number:
		return typed.String()
	case int:
		return fmt.Sprint(typed)
	case int64:
		return fmt.Sprint(typed)
	case float64:
		return fmt.Sprint(typed)
	default:
		return ""
	}
}

// uploadVersion extracts metadata.upload_version, preserving the Python
// None/string distinction.
func uploadVersion(hit map[string]any) (version string, hasVersion bool, invalid bool) {
	metadata, _ := hit["metadata"].(map[string]any)
	if metadata == nil {
		return "", false, false
	}
	raw, present := metadata["upload_version"]
	if !present || raw == nil {
		return "", false, false
	}
	text, ok := raw.(string)
	if !ok {
		return "", true, true
	}
	return text, true, false
}

func boolToInt(value bool) int {
	if value {
		return 1
	}
	return 0
}

// guessMIME mirrors mimetypes.guess_type(name)[0] or application/octet-stream.
func guessMIME(name string) string {
	extension := strings.ToLower(filepath.Ext(name))
	if mime, ok := mimeTypes[extension]; ok {
		return mime
	}
	return "application/octet-stream"
}

func truncateRunes(value string, maximum int) string {
	if utf8.RuneCountInString(value) <= maximum {
		return value
	}
	runes := []rune(value)
	return string(runes[:maximum])
}

// deepCopyMap recursively copies the JSON-shaped payload, preserving Go numeric
// types like copy.deepcopy.
func deepCopyMap(payload map[string]any) map[string]any {
	if payload == nil {
		return nil
	}
	copied := make(map[string]any, len(payload))
	for key, value := range payload {
		copied[key] = deepCopyValue(value)
	}
	return copied
}

func deepCopyValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return deepCopyMap(typed)
	case []any:
		copied := make([]any, len(typed))
		for index, item := range typed {
			copied[index] = deepCopyValue(item)
		}
		return copied
	case []map[string]any:
		copied := make([]map[string]any, len(typed))
		for index, item := range typed {
			copied[index] = deepCopyMap(item)
		}
		return copied
	default:
		return value
	}
}

// encodeJSON serializes one artifact payload like json.dumps(ensure_ascii=False).
func encodeJSON(value any) ([]byte, error) {
	var buffer strings.Builder
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return nil, err
	}
	return []byte(strings.TrimRight(buffer.String(), "\n")), nil
}
