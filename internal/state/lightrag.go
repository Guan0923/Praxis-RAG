// Package state: the knowledge-graph mirror ledger.
//
// lightrag_documents records which documents have been copied into the LightRAG
// knowledge-graph sidecar and whether that copy is current. The table is a
// derived-index register, not a source of truth: MySQL keeps the document
// itself, and the sidecar keeps the graph. This ledger exists so a failed
// mirror can be retried, a deleted document can be removed from the graph, and
// an operator can see which documents are actually queryable through the graph.
//
// Chunk text and model credentials never enter this table.

package state

import (
	"context"
	"database/sql"
	"errors"
)

// Mirror status values of one graph copy.
const (
	// LightRAGStatusIndexed means the graph copy matches the current document
	// version under the recorded workspace fingerprint.
	LightRAGStatusIndexed = "indexed"
	// LightRAGStatusFailed means the last mirror attempt failed; the row keeps
	// the attempt count and the safe error text for a later retry.
	LightRAGStatusFailed = "failed"
	// LightRAGStatusDeleted means the document was removed from the graph.
	LightRAGStatusDeleted = "deleted"
)

// LightRAGDocument is one knowledge-graph mirror row.
type LightRAGDocument struct {
	// Namespace is the knowledge base the document belongs to.
	Namespace string
	// DocumentID is the Praxis document identifier used inside the graph.
	DocumentID string
	// DocumentName is the display name recorded for evidence rendering.
	DocumentName string
	// ConfigFingerprint is the sidecar workspace fingerprint the row was
	// indexed under. A mismatch means the graph copy is stale.
	ConfigFingerprint string
	// ChunkCount is the number of chunks mirrored into the graph.
	ChunkCount int
	// Status is one of the LightRAGStatus* values.
	Status string
	// Attempts counts mirror attempts that did not end in indexed.
	Attempts int
	// LastError is the safe failure text of the last attempt.
	LastError string
	// IndexedAt is the timestamp of the last successful mirror, empty when the
	// document was never mirrored.
	IndexedAt string
	// UpdatedAt is the timestamp of the last write.
	UpdatedAt string
}

// LightRAGDocumentRepository reads and writes the graph mirror ledger.
type LightRAGDocumentRepository struct {
	stores *Stores
}

// NewLightRAGDocumentRepository builds the repository on the shared store.
func NewLightRAGDocumentRepository(stores *Stores) *LightRAGDocumentRepository {
	return &LightRAGDocumentRepository{stores: stores}
}

// Get reads one mirror row. A missing row returns (nil, nil) so callers can
// distinguish "never mirrored" from a real error.
//
// Parameters:
//   - ctx: request context.
//   - namespace: knowledge base identifier.
//   - documentID: Praxis document identifier.
func (r *LightRAGDocumentRepository) Get(
	ctx context.Context,
	namespace, documentID string,
) (*LightRAGDocument, error) {
	var (
		row       LightRAGDocument
		lastError sql.NullString
		indexedAt sql.NullString
	)
	err := r.stores.DB.QueryRowContext(ctx,
		"SELECT namespace, document_id, document_name, config_fingerprint, chunk_count, "+
			"status, attempts, last_error, indexed_at, updated_at "+
			"FROM lightrag_documents WHERE namespace = ? AND document_id = ?",
		namespace, documentID).
		Scan(&row.Namespace, &row.DocumentID, &row.DocumentName, &row.ConfigFingerprint,
			&row.ChunkCount, &row.Status, &row.Attempts, &lastError, &indexedAt, &row.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	row.LastError = lastError.String
	row.IndexedAt = indexedAt.String
	return &row, nil
}

// Upsert writes one mirror row, replacing the previous state of the same
// document. The caller sets Status; UpdatedAt is filled here.
//
// Parameters:
//   - ctx: request context.
//   - document: the row to store; Namespace and DocumentID are required.
func (r *LightRAGDocumentRepository) Upsert(ctx context.Context, document *LightRAGDocument) error {
	if document == nil {
		return newValueError("LightRAG document is required")
	}
	if document.Namespace == "" || document.DocumentID == "" {
		return newValueError("LightRAG document requires namespace and document_id")
	}
	document.UpdatedAt = nowISO()
	var indexedAt any
	if document.IndexedAt != "" {
		indexedAt = document.IndexedAt
	}
	var lastError any
	if document.LastError != "" {
		lastError = document.LastError
	}
	_, err := r.stores.DB.ExecContext(ctx,
		"INSERT INTO lightrag_documents (namespace, document_id, document_name, "+
			"config_fingerprint, chunk_count, status, attempts, last_error, indexed_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?) "+
			"ON DUPLICATE KEY UPDATE document_name = VALUES(document_name), "+
			"config_fingerprint = VALUES(config_fingerprint), chunk_count = VALUES(chunk_count), "+
			"status = VALUES(status), attempts = VALUES(attempts), last_error = VALUES(last_error), "+
			"indexed_at = VALUES(indexed_at), updated_at = VALUES(updated_at)",
		document.Namespace, document.DocumentID, document.DocumentName,
		document.ConfigFingerprint, document.ChunkCount, document.Status, document.Attempts,
		lastError, indexedAt, document.UpdatedAt)
	return err
}

// MarkFailed records one failed mirror attempt without destroying a previous
// successful index timestamp: a retry that fails must not make an older graph
// copy look newer than it is.
//
// Parameters:
//   - ctx: request context.
//   - namespace: knowledge base identifier.
//   - documentID: Praxis document identifier.
//   - message: safe failure text (never document content or credentials).
func (r *LightRAGDocumentRepository) MarkFailed(
	ctx context.Context,
	namespace, documentID, message string,
) error {
	_, err := r.stores.DB.ExecContext(ctx,
		"INSERT INTO lightrag_documents (namespace, document_id, document_name, "+
			"config_fingerprint, chunk_count, status, attempts, last_error, updated_at) "+
			"VALUES (?, ?, '', '', 0, ?, 1, ?, ?) "+
			"ON DUPLICATE KEY UPDATE status = ?, attempts = attempts + 1, last_error = ?, updated_at = ?",
		namespace, documentID, LightRAGStatusFailed, message, nowISO(),
		LightRAGStatusFailed, message, nowISO())
	return err
}

// Delete removes one mirror row. Removing a document from the graph is
// idempotent, so a missing row is not an error.
//
// Parameters:
//   - ctx: request context.
//   - namespace: knowledge base identifier.
//   - documentID: Praxis document identifier.
func (r *LightRAGDocumentRepository) Delete(ctx context.Context, namespace, documentID string) error {
	_, err := r.stores.DB.ExecContext(ctx,
		"DELETE FROM lightrag_documents WHERE namespace = ? AND document_id = ?",
		namespace, documentID)
	return err
}

// ListPending returns mirror rows whose graph copy is not current, newest
// attempt first. It is the retry queue of the graph index and never contains
// documents that were deleted from the graph on purpose.
//
// Parameters:
//   - ctx: request context.
//   - limit: maximum rows to return; non-positive falls back to 50.
func (r *LightRAGDocumentRepository) ListPending(ctx context.Context, limit int) ([]LightRAGDocument, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.stores.DB.QueryContext(ctx,
		"SELECT namespace, document_id, document_name, config_fingerprint, chunk_count, "+
			"status, attempts, last_error, indexed_at, updated_at "+
			"FROM lightrag_documents WHERE status = ? ORDER BY updated_at DESC LIMIT ?",
		LightRAGStatusFailed, limit)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	pending := make([]LightRAGDocument, 0, limit)
	for rows.Next() {
		var (
			row       LightRAGDocument
			lastError sql.NullString
			indexedAt sql.NullString
		)
		if err := rows.Scan(&row.Namespace, &row.DocumentID, &row.DocumentName,
			&row.ConfigFingerprint, &row.ChunkCount, &row.Status, &row.Attempts,
			&lastError, &indexedAt, &row.UpdatedAt); err != nil {
			return nil, err
		}
		row.LastError = lastError.String
		row.IndexedAt = indexedAt.String
		pending = append(pending, row)
	}
	return pending, rows.Err()
}
