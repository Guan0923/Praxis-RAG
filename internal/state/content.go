package state

import (
	"context"
	"database/sql"
)

// ContentObjectGraceSeconds is the minimum age of an unreferenced content row
// before the maintenance sweep deletes it. The window covers a writer that has
// inserted the content row but not yet committed the reference row, so a
// concurrent upload can never observe a half-deleted content object.
const ContentObjectGraceSeconds = int64(600)

// contentObjectBatch bounds one backfill or GC step so a maintenance pass never
// scans or deletes an unbounded amount of data.
const contentObjectBatch = 200

// insertContentObjectTx stores one payload once and returns its SHA-256 key.
// References live in upload_objects.content_hash and blobs.sha256, so repeated
// uploads of the same bytes add only a reference row.
func insertContentObjectTx(ctx context.Context, tx *sql.Tx, content []byte, createdAt float64) (string, error) {
	hash := sha256Sum(content)
	if _, err := tx.ExecContext(ctx,
		"INSERT IGNORE INTO content_objects (hash, size, content, created_at) VALUES (?, ?, ?, ?)",
		hash, int64(len(content)), content, createdAt); err != nil {
		return "", err
	}
	return hash, nil
}

// deleteDocumentContentRefsTx drops every reference row that owns bytes of one
// document. The corresponding content_objects rows are removed later by
// PurgeUnreferencedContent once no reference row names them anymore.
func deleteDocumentContentRefsTx(ctx context.Context, tx *sql.Tx, namespace, documentID string) error {
	if documentID == "" {
		return nil
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
	return nil
}

// PurgeUnreferencedContent deletes at most limit content rows that no reference
// row points at and that are older than graceSeconds. It returns the number of
// removed rows.
func (s *Stores) PurgeUnreferencedContent(ctx context.Context, graceSeconds int64, limit int) (int64, error) {
	if limit <= 0 {
		limit = contentObjectBatch
	}
	if graceSeconds < 0 {
		graceSeconds = 0
	}
	cutoff := unixSeconds() - float64(graceSeconds)
	result, err := s.DB.ExecContext(ctx,
		"DELETE FROM content_objects WHERE created_at < ?"+
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
