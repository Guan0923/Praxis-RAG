package state

import (
	"context"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"math"
	"strings"
)

// ArtifactUnavailable reports an artifact that is missing, expired, or outside
// the requested namespace. It mirrors rag_mcp.state.artifacts.ArtifactUnavailable
// (a ValueError in Python); the Go type is distinct so callers can map it to the
// public HTTP/MCP error.
type ArtifactUnavailable struct{ Message string }

// Error implements error.
func (e *ArtifactUnavailable) Error() string { return e.Message }

func newArtifactUnavailable(message string) *ArtifactUnavailable {
	return &ArtifactUnavailable{Message: message}
}

// DefaultArtifactTTLSeconds and DefaultArtifactMaxBytes are the Python defaults.
const (
	DefaultArtifactTTLSeconds = 3600.0
	DefaultArtifactMaxBytes   = int64(64 * 1024 * 1024)
)

// Artifact is the public, content-free metadata of one artifact snapshot.
type Artifact struct {
	ArtifactID  string  `json:"artifact_id"`
	DocumentID  string  `json:"document_id"`
	Version     *string `json:"version"`
	Kind        string  `json:"kind"`
	Name        string  `json:"name"`
	MimeType    string  `json:"mime_type"`
	ExpiresAt   float64 `json:"expires_at"`
	ResourceURI string  `json:"resource_uri"`
}

// ArtifactChunk is one bounded base64 page of artifact bytes.
type ArtifactChunk struct {
	Artifact
	Encoding   string `json:"encoding"`
	Data       string `json:"data"`
	TotalBytes int    `json:"total_bytes"`
	Offset     int    `json:"offset"`
	NextOffset *int   `json:"next_offset"`
}

// ArtifactRegistry keeps immutable bytes in SQL, never in the disposable Redis
// cache. Identity excludes MIME metadata and expiry; re-registering a retained
// identity only extends its lifetime. It is a port of
// rag_mcp.state.artifacts.ArtifactRegistry.
type ArtifactRegistry struct {
	stores     *Stores
	TTLSeconds float64
	// MaxBytes nil disables the per-artifact size ceiling.
	MaxBytes *int64
	Clock    func() float64
}

// NewArtifactRegistry builds a registry with Python-equivalent validation.
func NewArtifactRegistry(stores *Stores, ttlSeconds float64, clock func() float64, maxBytes *int64) (*ArtifactRegistry, error) {
	if clock == nil {
		clock = unixSeconds
	}
	if math.IsNaN(ttlSeconds) || math.IsInf(ttlSeconds, 0) {
		return nil, newValueError("ttl_seconds must be a finite number")
	}
	if ttlSeconds <= 0 {
		return nil, newValueError("ttl_seconds must be positive")
	}
	if maxBytes != nil && *maxBytes < 0 {
		return nil, newValueError("max_bytes must be a nonnegative integer or None")
	}
	return &ArtifactRegistry{stores: stores, TTLSeconds: ttlSeconds, Clock: clock, MaxBytes: maxBytes}, nil
}

func (r *ArtifactRegistry) now() (float64, error) {
	value := float64(0)
	if r.Clock != nil {
		value = r.Clock()
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, newValueError("clock must be a finite number")
	}
	return value, nil
}

func validateArtifactText(value, label string, maximum int) error {
	if value == "" || len(value) > maximum {
		return newValueError(fmt.Sprintf("%s must be a nonempty string of at most %d characters", label, maximum))
	}
	return nil
}

func artifactIdentityVersion(version *string) any {
	if version == nil {
		return nil
	}
	return *version
}

// Register commits a snapshot and returns public metadata without its bytes.
func (r *ArtifactRegistry) Register(
	ctx context.Context,
	namespace, documentID string,
	version *string,
	kind, name, mimeType string,
	content []byte,
	expiresAt *float64,
) (Artifact, error) {
	for _, item := range []struct {
		label   string
		value   string
		maximum int
	}{
		{"namespace", namespace, 64},
		{"document_id", documentID, 64},
		{"kind", kind, 32},
		{"name", name, 512},
		{"mime_type", mimeType, 128},
	} {
		if err := validateArtifactText(item.value, item.label, item.maximum); err != nil {
			return Artifact{}, err
		}
	}
	if version != nil {
		if err := validateArtifactText(*version, "version", 64); err != nil {
			return Artifact{}, err
		}
	}
	if r.MaxBytes != nil && int64(len(content)) > *r.MaxBytes {
		return Artifact{}, newValueError("Artifact exceeds the per-artifact byte limit")
	}
	now, err := r.now()
	if err != nil {
		return Artifact{}, err
	}
	expiry := now + r.TTLSeconds
	if expiresAt != nil {
		expiry = *expiresAt
	}
	if math.IsNaN(expiry) || math.IsInf(expiry, 0) {
		return Artifact{}, newValueError("expires_at must be a finite number")
	}
	identityJSON, err := marshalJSON([]any{
		namespace, documentID, artifactIdentityVersion(version), kind, name, sha256Sum(content),
	})
	if err != nil {
		return Artifact{}, err
	}
	identity := sha256Hex(identityJSON)
	artifactID := uuid5(uuidNamespaceURL, identity)

	statement := artifactUpsertSQL(r.stores.Dialect())
	var versionValue any
	if version != nil {
		versionValue = *version
	}
	transaction, err := r.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return Artifact{}, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx, statement,
		artifactID, namespace, identity, documentID, versionValue, kind, name, mimeType,
		content, now, expiry); err != nil {
		return Artifact{}, err
	}
	artifact, err := scanArtifact(transaction.QueryRowContext(ctx,
		"SELECT id, namespace, document_id, version, kind, name, mime_type, expires_at FROM artifact_snapshots "+
			"WHERE namespace = ? AND identity = ?", namespace, identity))
	if err != nil {
		return Artifact{}, err
	}
	if err := transaction.Commit(); err != nil {
		return Artifact{}, err
	}
	return publicArtifact(artifact), nil
}

// artifactUpsertSQL renders the atomic identity upsert. Re-registering a
// retained identity only extends its lifetime (GREATEST/max with the stored
// expiry), preserving the original snapshot and creation time.
func artifactUpsertSQL(dialect Dialect) string {
	if dialect == DialectMySQL {
		return "INSERT INTO artifact_snapshots (id, namespace, identity, document_id, version, kind, name, " +
			"mime_type, content, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
			"ON DUPLICATE KEY UPDATE expires_at = GREATEST(artifact_snapshots.expires_at, VALUES(expires_at))"
	}
	return "INSERT INTO artifact_snapshots (id, namespace, identity, document_id, version, kind, name, " +
		"mime_type, content, created_at, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?) " +
		"ON CONFLICT(namespace, identity) DO UPDATE SET " +
		"expires_at = max(artifact_snapshots.expires_at, excluded.expires_at)"
}

// rowArtifact is the scanned database shape (content excluded).
type rowArtifact struct {
	artifactID string
	namespace  string
	documentID string
	version    sql.NullString
	kind       string
	name       string
	mimeType   string
	expiresAt  float64
}

func scanArtifact(scanner rowScanner) (rowArtifact, error) {
	var value rowArtifact
	err := scanner.Scan(&value.artifactID, &value.namespace, &value.documentID, &value.version,
		&value.kind, &value.name, &value.mimeType, &value.expiresAt)
	return value, err
}

func publicArtifact(value rowArtifact) Artifact {
	var version *string
	if value.version.Valid {
		text := value.version.String
		version = &text
	}
	return Artifact{
		ArtifactID:  value.artifactID,
		DocumentID:  value.documentID,
		Version:     version,
		Kind:        value.kind,
		Name:        value.name,
		MimeType:    value.mimeType,
		ExpiresAt:   value.expiresAt,
		ResourceURI: "rag://artifacts/" + quotePython(value.namespace) + "/" + value.artifactID,
	}
}

// Read returns the canonical UUID's metadata and bytes without consulting
// documents, uploads, or cache.
func (r *ArtifactRegistry) Read(ctx context.Context, namespace, artifactID string) (Artifact, []byte, error) {
	if namespace == "" || len(namespace) > 64 {
		return Artifact{}, nil, newArtifactUnavailable("Artifact is unavailable")
	}
	if !canonicalUUID(artifactID) {
		return Artifact{}, nil, newArtifactUnavailable("Artifact is unavailable")
	}
	now, err := r.now()
	if err != nil {
		return Artifact{}, nil, err
	}
	var content []byte
	var value rowArtifact
	err = r.stores.DB.QueryRowContext(ctx,
		"SELECT id, namespace, document_id, version, kind, name, mime_type, expires_at, content "+
			"FROM artifact_snapshots WHERE namespace = ? AND id = ? AND expires_at > ?",
		namespace, artifactID, now).
		Scan(&value.artifactID, &value.namespace, &value.documentID, &value.version,
			&value.kind, &value.name, &value.mimeType, &value.expiresAt, &content)
	if errors.Is(err, sql.ErrNoRows) {
		return Artifact{}, nil, newArtifactUnavailable("Artifact is unavailable")
	}
	if err != nil {
		return Artifact{}, nil, err
	}
	// MySQL PAD SPACE collations can match a namespace with trailing spaces.
	if value.namespace != namespace {
		return Artifact{}, nil, newArtifactUnavailable("Artifact is unavailable")
	}
	return publicArtifact(value), content, nil
}

// Chunk returns at most 1 MiB as base64; EOF is valid, offsets past EOF are not.
func (r *ArtifactRegistry) Chunk(ctx context.Context, namespace, artifactID string, offset, length int) (ArtifactChunk, error) {
	if offset < 0 {
		return ArtifactChunk{}, newValueError("offset must be a nonnegative integer")
	}
	if length < 1 || length > 1024*1024 {
		return ArtifactChunk{}, newValueError("length must be an integer between 1 and 1048576")
	}
	artifact, content, err := r.Read(ctx, namespace, artifactID)
	if err != nil {
		return ArtifactChunk{}, err
	}
	if offset > len(content) {
		return ArtifactChunk{}, newValueError("offset exceeds the artifact byte length")
	}
	end := offset + length
	if end > len(content) {
		end = len(content)
	}
	chunk := ArtifactChunk{
		Artifact:   artifact,
		Encoding:   "base64",
		Data:       base64.StdEncoding.EncodeToString(content[offset:end]),
		TotalBytes: len(content),
		Offset:     offset,
	}
	if end < len(content) {
		next := end
		chunk.NextOffset = &next
	}
	return chunk, nil
}

// PurgeExpired deletes a bounded batch, rechecking expiry after concurrent
// renewal.
func (r *ArtifactRegistry) PurgeExpired(ctx context.Context, limit int) (int, error) {
	if limit < 1 {
		return 0, newValueError("limit must be a positive integer")
	}
	now, err := r.now()
	if err != nil {
		return 0, err
	}
	rows, err := r.stores.DB.QueryContext(ctx,
		"SELECT id FROM artifact_snapshots WHERE expires_at <= ? ORDER BY expires_at, id LIMIT ?", now, limit)
	if err != nil {
		return 0, err
	}
	candidates := make([]string, 0, limit)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return 0, err
		}
		candidates = append(candidates, id)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(candidates) == 0 {
		return 0, nil
	}
	placeholders := make([]string, len(candidates))
	arguments := make([]any, 0, len(candidates)+1)
	for index, id := range candidates {
		placeholders[index] = "?"
		arguments = append(arguments, id)
	}
	arguments = append(arguments, now)
	// The write predicate protects concurrently renewed ids.
	result, err := r.stores.DB.ExecContext(ctx,
		"DELETE FROM artifact_snapshots WHERE id IN ("+strings.Join(placeholders, ", ")+") AND expires_at <= ?",
		arguments...)
	if err != nil {
		return 0, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(affected), nil
}
