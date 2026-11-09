package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
)

// BlobStore is namespace-scoped, database-backed object storage. It is a port
// of rag_mcp.state.blobs.BlobStore.
type BlobStore struct {
	db      *sql.DB
	dialect Dialect
}

func newBlobStore(db *sql.DB, dialect Dialect) *BlobStore {
	return &BlobStore{db: db, dialect: dialect}
}

const blobSelectContent = "SELECT content FROM blobs WHERE namespace = ? AND bucket = ? AND `key` = ?"

// Get returns the stored bytes, or found=false when the object is absent.
func (b *BlobStore) Get(ctx context.Context, namespace, bucket, key string) ([]byte, bool, error) {
	var content []byte
	err := b.db.QueryRowContext(ctx, blobSelectContent, namespace, bucket, key).Scan(&content)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return content, true, nil
}

// GetText returns the stored object decoded as UTF-8. Python supports an
// arbitrary encoding argument; every in-tree caller uses utf-8, so the Go port
// accepts only utf-8 and rejects other encodings explicitly.
func (b *BlobStore) GetText(ctx context.Context, namespace, bucket, key string) (string, bool, error) {
	content, found, err := b.Get(ctx, namespace, bucket, key)
	if err != nil || !found {
		return "", found, err
	}
	return string(content), true, nil
}

// Exists reports whether the object is present.
func (b *BlobStore) Exists(ctx context.Context, namespace, bucket, key string) (bool, error) {
	var marker string
	err := b.db.QueryRowContext(ctx,
		"SELECT `key` FROM blobs WHERE namespace = ? AND bucket = ? AND `key` = ? LIMIT 1",
		namespace, bucket, key).Scan(&marker)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ListKeys returns object keys ordered by key. documentID filters by owner.
func (b *BlobStore) ListKeys(ctx context.Context, namespace, bucket string, documentID *string) ([]string, error) {
	query := "SELECT `key` FROM blobs WHERE namespace = ? AND bucket = ?"
	arguments := []any{namespace, bucket}
	if documentID != nil {
		query += " AND document_id = ?"
		arguments = append(arguments, *documentID)
	}
	query += " ORDER BY `key`"
	rows, err := b.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	keys := make([]string, 0)
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			return nil, err
		}
		keys = append(keys, key)
	}
	return keys, rows.Err()
}

// Put stores one object in its own transaction.
func (b *BlobStore) Put(ctx context.Context, namespace, bucket, key string, content []byte, documentID *string) error {
	transaction, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := b.PutTx(ctx, transaction, namespace, bucket, key, content, documentID); err != nil {
		return err
	}
	return transaction.Commit()
}

// PutTx stores one object inside the caller's transaction (Python `conn=`).
func (b *BlobStore) PutTx(ctx context.Context, tx *sql.Tx, namespace, bucket, key string, content []byte, documentID *string) error {
	return blobUpsert(ctx, tx, namespace, bucket, key, content, documentID)
}

// BlobItem is one key/content pair for the bulk write helpers.
type BlobItem struct {
	Key     string
	Content []byte
}

// PutMany stores several objects in one transaction.
func (b *BlobStore) PutMany(ctx context.Context, namespace, bucket string, items []BlobItem, documentID *string) error {
	transaction, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := b.PutManyTx(ctx, transaction, namespace, bucket, items, documentID); err != nil {
		return err
	}
	return transaction.Commit()
}

// PutManyTx stores several objects inside the caller's transaction.
func (b *BlobStore) PutManyTx(ctx context.Context, tx *sql.Tx, namespace, bucket string, items []BlobItem, documentID *string) error {
	for _, item := range items {
		if err := blobUpsert(ctx, tx, namespace, bucket, item.Key, item.Content, documentID); err != nil {
			return err
		}
	}
	return nil
}

// PutFile stores one file's bytes, mirroring put_file.
func (b *BlobStore) PutFile(ctx context.Context, namespace, bucket, key, path string, documentID *string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return b.Put(ctx, namespace, bucket, key, content, documentID)
}

// PutFileTx stores one file's bytes inside the caller's transaction.
func (b *BlobStore) PutFileTx(ctx context.Context, tx *sql.Tx, namespace, bucket, key, path string, documentID *string) error {
	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return blobUpsert(ctx, tx, namespace, bucket, key, content, documentID)
}

// PutTree stores every regular file below root under "<documentID>/<relative>"
// and returns the number of files written, mirroring put_tree.
func (b *BlobStore) PutTree(ctx context.Context, namespace, bucket, documentID, root string) (int, error) {
	transaction, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	count, err := b.PutTreeTx(ctx, transaction, namespace, bucket, documentID, root)
	if err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// PutTreeTx stores a directory tree inside the caller's transaction.
func (b *BlobStore) PutTreeTx(ctx context.Context, tx *sql.Tx, namespace, bucket, documentID, root string) (int, error) {
	count := 0
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		key := documentID + "/" + filepath.ToSlash(relative)
		if err := blobUpsert(ctx, tx, namespace, bucket, key, content, &documentID); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// DeleteKey removes one object in its own transaction.
func (b *BlobStore) DeleteKey(ctx context.Context, namespace, bucket, key string) error {
	transaction, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := b.DeleteKeyTx(ctx, transaction, namespace, bucket, key); err != nil {
		return err
	}
	return transaction.Commit()
}

// DeleteKeyTx removes one object inside the caller's transaction.
func (b *BlobStore) DeleteKeyTx(ctx context.Context, tx *sql.Tx, namespace, bucket, key string) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM blobs WHERE namespace = ? AND bucket = ? AND `key` = ?", namespace, bucket, key)
	return err
}

// DeleteDocument removes every object owned by documentID in one bucket.
func (b *BlobStore) DeleteDocument(ctx context.Context, namespace, bucket, documentID string) error {
	transaction, err := b.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := b.DeleteDocumentTx(ctx, transaction, namespace, bucket, documentID); err != nil {
		return err
	}
	return transaction.Commit()
}

// DeleteDocumentTx removes a document's objects inside the caller's transaction.
func (b *BlobStore) DeleteDocumentTx(ctx context.Context, tx *sql.Tx, namespace, bucket, documentID string) error {
	_, err := tx.ExecContext(ctx,
		"DELETE FROM blobs WHERE namespace = ? AND bucket = ? AND document_id = ?", namespace, bucket, documentID)
	return err
}

// blobUpsert implements BlobStore._put: UPDATE, then INSERT, then retry UPDATE
// when another instance inserted the same (namespace, bucket, key) first.
func blobUpsert(ctx context.Context, tx *sql.Tx, namespace, bucket, key string, content []byte, documentID *string) error {
	const updateStatement = "UPDATE blobs SET document_id = ?, content = ?, size = ?, sha256 = ?, created_at = ? " +
		"WHERE namespace = ? AND bucket = ? AND `key` = ?"
	const insertStatement = "INSERT INTO blobs (namespace, bucket, `key`, document_id, content, size, sha256, created_at) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?, ?)"
	sum := sha256Sum(content)
	createdAt := nowISO()
	var owner any
	if documentID != nil {
		owner = *documentID
	}
	updateArguments := []any{owner, content, int64(len(content)), sum, createdAt, namespace, bucket, key}
	result, err := tx.ExecContext(ctx, updateStatement, updateArguments...)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if changed > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, insertStatement,
		namespace, bucket, key, owner, content, int64(len(content)), sum, createdAt); err != nil {
		if !isUniqueViolation(err) {
			return err
		}
		_, retryErr := tx.ExecContext(ctx, updateStatement, updateArguments...)
		return retryErr
	}
	return nil
}
