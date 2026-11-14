package state

import (
	"context"
	"database/sql"
)

// ContentObjectRetentionSeconds is how long an unreferenced payload row is kept
// after it loses its last reference. The product requirement is "more than one
// week": reference rows disappear immediately, but content bytes stay for this
// window (8 days > 1 week) so an accidental delete can still be recovered from
// a backup before the only remaining copy is dropped.
const ContentObjectRetentionSeconds = int64(8 * 24 * 60 * 60)

// contentObjectBatch bounds one backfill or GC step so a maintenance pass never
// scans or deletes an unbounded amount of data.
const contentObjectBatch = 200

// insertContentObjectTx stores one payload once and returns its SHA-256 key.
// References live in upload_objects.content_hash and blobs.sha256, so repeated
// uploads of the same bytes add only a reference row.
func insertContentObjectTx(ctx context.Context, tx *sql.Tx, content []byte, createdAt float64) (string, error) {
	hash := sha256Sum(content)
	result, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO content_objects (hash, size, content, created_at) VALUES (?, ?, ?, ?)",
		hash, int64(len(content)), content, createdAt)
	if err != nil {
		return "", err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected == 0 {
		// The payload already exists and may be marked retired after its last
		// reference disappeared; a new reference re-arms it so the GC keeps it.
		if _, err := tx.ExecContext(ctx,
			"UPDATE content_objects SET retired_at = NULL WHERE hash = ? AND retired_at IS NOT NULL",
			hash); err != nil {
			return "", err
		}
	}
	return hash, nil
}

// contentHashesTx runs one SELECT that returns content hashes.
func contentHashesTx(ctx context.Context, tx *sql.Tx, query string, arguments ...any) ([]string, error) {
	rows, err := tx.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var hashes []string
	for rows.Next() {
		var hash sql.NullString
		if err := rows.Scan(&hash); err != nil {
			return nil, err
		}
		if hash.Valid && hash.String != "" {
			hashes = append(hashes, hash.String)
		}
	}
	return hashes, rows.Err()
}

// retireContentHashesTx marks payload rows whose last reference just
// disappeared at now. Rows another reference row still points at stay untouched;
// the maintenance GC deletes retired rows only after the retention window.
func retireContentHashesTx(ctx context.Context, tx *sql.Tx, hashes []string, now float64) error {
	unique := dedupeNonEmpty(hashes)
	const chunk = 500
	for start := 0; start < len(unique); start += chunk {
		end := start + chunk
		if end > len(unique) {
			end = len(unique)
		}
		batch := unique[start:end]
		arguments := append([]any{now}, stringArguments(batch)...)
		if _, err := tx.ExecContext(ctx,
			"UPDATE content_objects SET retired_at = ? WHERE retired_at IS NULL AND hash IN ("+
				inPlaceholders(len(batch))+") "+
				"AND NOT EXISTS (SELECT 1 FROM upload_objects u WHERE u.content_hash = content_objects.hash) "+
				"AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.sha256 = content_objects.hash)",
			arguments...); err != nil {
			return err
		}
	}
	return nil
}

// deleteUploadObjectsTx deletes the staged objects of one operation and retires
// payloads that no longer have any reference row.
func deleteUploadObjectsTx(ctx context.Context, tx *sql.Tx, operationID string) error {
	hashes, err := contentHashesTx(ctx, tx,
		"SELECT DISTINCT content_hash FROM upload_objects WHERE operation_id = ? AND content_hash IS NOT NULL",
		operationID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM upload_objects WHERE operation_id = ?", operationID); err != nil {
		return err
	}
	return retireContentHashesTx(ctx, tx, hashes, unixSeconds())
}

// deleteDocumentContentRefsTx drops every reference row that owns bytes of one
// document and retires the payloads that no reference row names anymore. The
// content_objects rows themselves stay until ContentObjectRetentionSeconds has
// passed, then the maintenance GC removes them.
func deleteDocumentContentRefsTx(ctx context.Context, tx *sql.Tx, namespace, documentID string) error {
	if documentID == "" {
		return nil
	}
	hashes, err := contentHashesTx(ctx, tx,
		"SELECT DISTINCT content_hash FROM upload_objects WHERE content_hash IS NOT NULL "+
			"AND operation_id IN (SELECT id FROM upload_operations WHERE namespace = ? AND document_id = ?) "+
			"UNION SELECT DISTINCT sha256 FROM blobs WHERE namespace = ? AND document_id = ?",
		namespace, documentID, namespace, documentID)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM upload_objects WHERE operation_id IN "+
			"(SELECT id FROM upload_operations WHERE namespace = ? AND document_id = ?)",
		namespace, documentID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM blobs WHERE namespace = ? AND document_id = ?", namespace, documentID); err != nil {
		return err
	}
	return retireContentHashesTx(ctx, tx, hashes, unixSeconds())
}

// PurgeUnreferencedContent deletes at most limit content rows that lost their
// last reference more than retentionSeconds ago. It returns the number of
// removed rows.
func (s *Stores) PurgeUnreferencedContent(ctx context.Context, retentionSeconds int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = contentObjectBatch
	}
	if retentionSeconds < 0 {
		retentionSeconds = 0
	}
	cutoff := unixSeconds() - float64(retentionSeconds)
	result, err := s.DB.ExecContext(ctx,
		"DELETE FROM content_objects WHERE retired_at IS NOT NULL AND retired_at < ?"+
			" AND NOT EXISTS (SELECT 1 FROM upload_objects u WHERE u.content_hash = content_objects.hash)"+
			" AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.sha256 = content_objects.hash)"+
			" LIMIT ?", cutoff, limit)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// BackfillContentObjects moves inline bytes of legacy rows into content_objects
// in bounded batches. A legacy row keeps working through the repository
// fallbacks until it is migrated, and the migration is idempotent: a row is only
// touched while content_hash is still NULL (upload_objects) or content is still
// non-empty (blobs).
func (s *Stores) BackfillContentObjects(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = contentObjectBatch
	}
	moved, err := s.backfillUploadObjects(ctx, limit)
	if err != nil {
		return moved, err
	}
	more, err := s.backfillBlobs(ctx, limit)
	return moved + more, err
}

type legacyContentRow struct {
	namespace  string
	bucket     string
	key        string
	operation  string
	content    []byte
}

func (s *Stores) backfillUploadObjects(ctx context.Context, limit int) (int64, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT operation_id, bucket, `key`, content FROM upload_objects "+
			"WHERE content_hash IS NULL AND OCTET_LENGTH(content) > 0 LIMIT ?", limit)
	if err != nil {
		return 0, err
	}
	items := make([]legacyContentRow, 0, limit)
	for rows.Next() {
		var item legacyContentRow
		if err := rows.Scan(&item.operation, &item.bucket, &item.key, &item.content); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var moved int64
	for _, item := range items {
		if err := s.moveLegacyContent(ctx, item, true); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

func (s *Stores) backfillBlobs(ctx context.Context, limit int) (int64, error) {
	rows, err := s.DB.QueryContext(ctx,
		"SELECT namespace, bucket, `key`, content FROM blobs "+
			"WHERE OCTET_LENGTH(content) > 0 LIMIT ?", limit)
	if err != nil {
		return 0, err
	}
	items := make([]legacyContentRow, 0, limit)
	for rows.Next() {
		var item legacyContentRow
		if err := rows.Scan(&item.namespace, &item.bucket, &item.key, &item.content); err != nil {
			_ = rows.Close()
			return 0, err
		}
		items = append(items, item)
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}
	var moved int64
	for _, item := range items {
		if err := s.moveLegacyContent(ctx, item, false); err != nil {
			return moved, err
		}
		moved++
	}
	return moved, nil
}

// moveLegacyContent copies one legacy inline payload into content_objects and
// replaces it with an empty placeholder plus the reference key.
func (s *Stores) moveLegacyContent(ctx context.Context, item legacyContentRow, uploadObject bool) error {
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	hash, err := insertContentObjectTx(ctx, tx, item.content, unixSeconds())
	if err != nil {
		return err
	}
	if uploadObject {
		_, err = tx.ExecContext(ctx,
			"UPDATE upload_objects SET content = ?, content_hash = ? "+
				"WHERE operation_id = ? AND bucket = ? AND `key` = ? AND content_hash IS NULL",
			[]byte{}, hash, item.operation, item.bucket, item.key)
	} else {
		_, err = tx.ExecContext(ctx,
			"UPDATE blobs SET content = ?, sha256 = ? "+
				"WHERE namespace = ? AND bucket = ? AND `key` = ? AND OCTET_LENGTH(content) > 0",
			[]byte{}, hash, item.namespace, item.bucket, item.key)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}
