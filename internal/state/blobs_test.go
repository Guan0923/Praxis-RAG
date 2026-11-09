package state

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestBlobRoundTrip ports the blob half of
// tests/test_state_store.py::test_documents_and_blobs_round_trip.
func TestBlobRoundTrip(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, "a.txt", []byte("hello"), stringPointer("doc-a")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	if err := stores.Blobs.Put(ctx, "alpha", PDFAssetBucket, "doc-a/document.md", []byte("saved"), stringPointer("doc-a")); err != nil {
		t.Fatalf("Put(pdf) error = %v", err)
	}
	content, found, err := stores.Blobs.Get(ctx, "alpha", SourceBucket, "a.txt")
	if err != nil || !found || string(content) != "hello" {
		t.Fatalf("Get() = %q, %v, %v", content, found, err)
	}
	text, found, err := stores.Blobs.GetText(ctx, "alpha", PDFAssetBucket, "doc-a/document.md")
	if err != nil || !found || text != "saved" {
		t.Fatalf("GetText() = %q, %v, %v", text, found, err)
	}
	keys, err := stores.Blobs.ListKeys(ctx, "alpha", PDFAssetBucket, stringPointer("doc-a"))
	if err != nil || len(keys) != 1 || keys[0] != "doc-a/document.md" {
		t.Fatalf("ListKeys() = %v, %v", keys, err)
	}
	exists, err := stores.Blobs.Exists(ctx, "alpha", SourceBucket, "a.txt")
	if err != nil || !exists {
		t.Fatalf("Exists() = %v, %v", exists, err)
	}
	if err := stores.Blobs.DeleteKey(ctx, "alpha", SourceBucket, "a.txt"); err != nil {
		t.Fatalf("DeleteKey() error = %v", err)
	}
	if _, found, err := stores.Blobs.Get(ctx, "alpha", SourceBucket, "a.txt"); err != nil || found {
		t.Fatalf("Get() after DeleteKey = found=%v err=%v", found, err)
	}
	if err := stores.Blobs.DeleteDocument(ctx, "alpha", PDFAssetBucket, "doc-a"); err != nil {
		t.Fatalf("DeleteDocument() error = %v", err)
	}
	if content, found, err := stores.Blobs.Get(ctx, "alpha", PDFAssetBucket, "doc-a/document.md"); err != nil || found {
		t.Fatalf("Get() after DeleteDocument = %q, found=%v err=%v", content, found, err)
	}
}

func TestBlobMetadataAndOverwrite(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, "file.bin", []byte("first"), stringPointer("doc-1")); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	var size int64
	var sha, documentID string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT size, sha256, document_id FROM blobs WHERE namespace = ? AND bucket = ? AND `key` = ?",
		"alpha", SourceBucket, "file.bin").Scan(&size, &sha, &documentID); err != nil {
		t.Fatalf("metadata query error = %v", err)
	}
	if size != 5 || sha != sha256Sum([]byte("first")) || documentID != "doc-1" {
		t.Fatalf("metadata = size=%d sha=%q document_id=%q", size, sha, documentID)
	}
	// Overwrite updates bytes, size, digest, owner and created_at in place.
	if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, "file.bin", []byte("second!),"), stringPointer("doc-2")); err != nil {
		t.Fatalf("overwrite Put() error = %v", err)
	}
	content, found, err := stores.Blobs.Get(ctx, "alpha", SourceBucket, "file.bin")
	if err != nil || !found || string(content) != "second!)," {
		t.Fatalf("Get() = %q, %v, %v", content, found, err)
	}
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT size, sha256, document_id FROM blobs WHERE namespace = ? AND bucket = ? AND `key` = ?",
		"alpha", SourceBucket, "file.bin").Scan(&size, &sha, &documentID); err != nil {
		t.Fatalf("metadata query error = %v", err)
	}
	if size != int64(len("second!),")) || sha != sha256Sum([]byte("second!),")) || documentID != "doc-2" {
		t.Fatalf("metadata after overwrite = size=%d sha=%q document_id=%q", size, sha, documentID)
	}
	var count int
	if err := stores.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM blobs").Scan(&count); err != nil {
		t.Fatalf("count error = %v", err)
	}
	if count != 1 {
		t.Fatalf("blobs rows = %d, want 1", count)
	}
}

func TestBlobNamespaceAndKeyAreCaseSensitive(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Blobs.Put(ctx, "Alpha", SourceBucket, "Doc.txt", []byte("upper"), nil); err != nil {
		t.Fatalf("Put(Alpha) error = %v", err)
	}
	if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, "Doc.txt", []byte("lower"), nil); err != nil {
		t.Fatalf("Put(alpha) error = %v", err)
	}
	upper, found, err := stores.Blobs.Get(ctx, "Alpha", SourceBucket, "Doc.txt")
	if err != nil || !found || string(upper) != "upper" {
		t.Fatalf("Get(Alpha) = %q, %v, %v", upper, found, err)
	}
	lower, found, err := stores.Blobs.Get(ctx, "alpha", SourceBucket, "Doc.txt")
	if err != nil || !found || string(lower) != "lower" {
		t.Fatalf("Get(alpha) = %q, %v, %v", lower, found, err)
	}
	if _, found, err := stores.Blobs.Get(ctx, "Alpha", SourceBucket, "doc.txt"); err != nil || found {
		t.Fatalf("Get(Alpha, doc.txt) found=%v err=%v; keys are not case-sensitive", found, err)
	}
}

func TestBlobListKeysOrderingAndOwnerFilter(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	for _, item := range []struct {
		key     string
		owner   *string
		content string
	}{
		{"z.txt", stringPointer("doc-1"), "z"},
		{"a.txt", stringPointer("doc-1"), "a"},
		{"m.txt", stringPointer("doc-2"), "m"},
		{"none.txt", nil, "n"},
	} {
		if err := stores.Blobs.Put(ctx, "alpha", SourceBucket, item.key, []byte(item.content), item.owner); err != nil {
			t.Fatalf("Put(%q) error = %v", item.key, err)
		}
	}
	keys, err := stores.Blobs.ListKeys(ctx, "alpha", SourceBucket, nil)
	if err != nil {
		t.Fatalf("ListKeys() error = %v", err)
	}
	expected := []string{"a.txt", "m.txt", "none.txt", "z.txt"}
	if len(keys) != len(expected) {
		t.Fatalf("ListKeys() = %v", keys)
	}
	for index, key := range expected {
		if keys[index] != key {
			t.Fatalf("ListKeys()[%d] = %q, want %q", index, keys[index], key)
		}
	}
	keys, err = stores.Blobs.ListKeys(ctx, "alpha", SourceBucket, stringPointer("doc-1"))
	if err != nil || len(keys) != 2 || keys[0] != "a.txt" || keys[1] != "z.txt" {
		t.Fatalf("ListKeys(doc-1) = %v, %v", keys, err)
	}
}

func TestBlobPutManyFileAndTree(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Blobs.PutMany(ctx, "alpha", PDFAssetBucket, []BlobItem{
		{Key: "doc-a/one.md", Content: []byte("one")},
		{Key: "doc-a/two.md", Content: []byte("two")},
	}, stringPointer("doc-a")); err != nil {
		t.Fatalf("PutMany() error = %v", err)
	}
	for _, key := range []string{"doc-a/one.md", "doc-a/two.md"} {
		if content, found, err := stores.Blobs.Get(ctx, "alpha", PDFAssetBucket, key); err != nil || !found || len(content) == 0 {
			t.Fatalf("Get(%q) = found=%v err=%v", key, found, err)
		}
	}

	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "pages"), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "document.md"), []byte("saved"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(root, "pages", "page-1.png"), []byte("png"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	count, err := stores.Blobs.PutTree(ctx, "alpha", PDFAssetBucket, "doc-b", root)
	if err != nil || count != 2 {
		t.Fatalf("PutTree() = %d, %v; want 2", count, err)
	}
	keys, err := stores.Blobs.ListKeys(ctx, "alpha", PDFAssetBucket, stringPointer("doc-b"))
	if err != nil {
		t.Fatalf("ListKeys() error = %v", err)
	}
	expected := []string{"doc-b/document.md", "doc-b/pages/page-1.png"}
	if len(keys) != len(expected) {
		t.Fatalf("tree keys = %v", keys)
	}
	for index, key := range expected {
		if keys[index] != key {
			t.Fatalf("tree keys[%d] = %q, want %q", index, keys[index], key)
		}
	}

	filePath := filepath.Join(t.TempDir(), "outside.txt")
	if err := os.WriteFile(filePath, []byte("file"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := stores.Blobs.PutFile(ctx, "alpha", WordAssetBucket, "doc-c/document.md", filePath, stringPointer("doc-c")); err != nil {
		t.Fatalf("PutFile() error = %v", err)
	}
	if content, found, err := stores.Blobs.Get(ctx, "alpha", WordAssetBucket, "doc-c/document.md"); err != nil || !found || string(content) != "file" {
		t.Fatalf("Get(PutFile) = %q, %v, %v", content, found, err)
	}
}

// TestBlobUpdateRollsBackWithTransaction ports
// tests/test_state_store.py::test_blob_update_rolls_back_with_transaction.
func TestBlobUpdateRollsBackWithTransaction(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	if err := stores.Blobs.Put(ctx, "alpha", PDFAssetBucket, "doc/document.md", []byte("previous"), nil); err != nil {
		t.Fatalf("Put() error = %v", err)
	}
	transaction, err := stores.DB.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx() error = %v", err)
	}
	if err := stores.Blobs.PutTx(ctx, transaction, "alpha", PDFAssetBucket, "doc/document.md", []byte("next"), nil); err != nil {
		t.Fatalf("PutTx() error = %v", err)
	}
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("Rollback() error = %v", err)
	}
	content, found, err := stores.Blobs.Get(ctx, "alpha", PDFAssetBucket, "doc/document.md")
	if err != nil || !found || !bytes.Equal(content, []byte("previous")) {
		t.Fatalf("Get() after rollback = %q, %v, %v", content, found, err)
	}
}

func stringPointer(value string) *string { return &value }
