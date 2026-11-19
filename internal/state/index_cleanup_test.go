package state

import (
	"context"
	"errors"
	"testing"
)

func TestIndexCleanupRetentionAndIndependentRetries(t *testing.T) {
	s := newTestStores(t)
	ctx := context.Background()
	const ns = "cleanup-kb"
	id := "x7"
	exec := func(query string, args ...any) {
		t.Helper()
		if _, err := s.DB.ExecContext(ctx, query, args...); err != nil {
			t.Fatal(err)
		}
	}
	content := []byte("X7 manual")
	exec("INSERT INTO documents (namespace,name,document_id,size,sha256,chunking,collection,chunk_count,modified_at,indexed_at,diagnostics,skipped_object_count) VALUES (?,?,?,8,?,'{}','manuals',1,'','','{}',0)", ns, "x7.md", id, sha256Sum(content))
	exec("INSERT INTO knowledge_bases (namespace,collection,settings,version,created_at,updated_at) VALUES (?,'manuals','{\"graph_enabled\":true}',1,'','')", ns)
	if err := s.Blobs.Put(ctx, ns, SourceBucket, "x7.md", content, &id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Documents.Delete(ctx, ns, "x7.md"); err != nil {
		t.Fatal(err)
	}
	exec("UPDATE index_cleanup_jobs SET due_at=0")
	ready := func() []IndexCleanup {
		t.Helper()
		if err := s.ScheduleIndexCleanup(ctx); err != nil {
			t.Fatal(err)
		}
		jobs, err := s.ReadyIndexJobs(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return jobs
	}
	if got := len(ready()); got != 0 {
		t.Fatalf("scheduled during retention: %d", got)
	}
	exec("UPDATE content_objects SET retired_at=1")
	if n, err := s.PurgeUnreferencedContent(ctx, ContentObjectRetentionSeconds, 50); err != nil || n != 1 {
		t.Fatalf("GC %d %v", n, err)
	}
	jobs := ready()
	if len(jobs) != 2 {
		t.Fatalf("expected two targets, got %+v", jobs)
	}
	failure := errors.New("sidecar offline")
	for _, j := range jobs {
		err := s.RunIndexCleanup(ctx, j, func() error {
			if j.Target == "graph" {
				return failure
			}
			return nil
		})
		if (j.Target == "graph") != errors.Is(err, failure) {
			t.Fatal(j, err)
		}
	}
	exec("UPDATE index_cleanup_jobs SET due_at=0 WHERE status='waiting'")
	retry := ready()
	if len(retry) != 1 || retry[0].Target != "graph" {
		t.Fatalf("retry targets: %+v", retry)
	}
	if err := s.RunIndexCleanup(ctx, retry[0], func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := s.RunIndexCleanup(ctx, retry[0], func() error { t.Fatal("duplicate executed"); return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestIndexCleanupReconcileGrace(t *testing.T) {
	s := newTestStores(t)
	ctx := context.Background()
	j := IndexCleanup{Target: "qdrant", Namespace: "manuals", DocumentID: "orphan", Version: "old", Collection: "owned"}
	if err := s.ObserveIndex(ctx, j); err != nil {
		t.Fatal(err)
	}
	var first float64
	if err := s.DB.QueryRow("SELECT due_at FROM index_cleanup_jobs").Scan(&first); err != nil {
		t.Fatal(err)
	}
	if err := s.ObserveIndex(ctx, j); err != nil {
		t.Fatal(err)
	}
	var count int
	var due float64
	if err := s.DB.QueryRow("SELECT COUNT(*),MIN(due_at) FROM index_cleanup_jobs").Scan(&count, &due); err != nil {
		t.Fatal(err)
	}
	if count != 1 || due != first || due < unixSeconds()+float64(ContentObjectRetentionSeconds)-5 {
		t.Fatal(count, due, first)
	}
}

func TestIndexCleanupCDCAndGraphVisibility(t *testing.T) {
	const job = "12345678-1234-4123-8123-123456789abc"
	event := func(gen string) []byte {
		return []byte(`{"op":"u","after":{"namespace":"kb","job_id":"` + job + `","schedule_version":` + gen + `}}`)
	}
	if _, err := DecodeCleanupNotification(event("0")); !IgnorableCDCChange(err) {
		t.Fatal(err)
	}
	loc, err := DecodeCleanupNotification(event("2"))
	if err != nil || loc.Generation != 2 || loc.ID != job {
		t.Fatal(loc, err)
	}
	s := newTestStores(t)
	ctx := context.Background()
	record := DocumentRecord{Name: "X7.md", DocumentID: "x7", Collection: "kb", Chunking: map[string]any{}}
	if err := s.Documents.Upsert(ctx, "kb", record); err != nil {
		t.Fatal(err)
	}
	heads, legacy, err := s.LightRAGDocuments.VisibleHeads(ctx, "kb", []string{"x7"})
	if _, ok := heads["x7"]; err != nil || !ok || !legacy["x7"] {
		t.Fatal(heads, legacy, err)
	}
	if _, err := s.Documents.Delete(ctx, "kb", record.Name); err != nil {
		t.Fatal(err)
	}
	heads, _, err = s.LightRAGDocuments.VisibleHeads(ctx, "kb", []string{"x7"})
	if err != nil || len(heads) != 0 {
		t.Fatal("deleted graph still visible", heads, err)
	}
	if err := s.Documents.Upsert(ctx, "kb", record); err != nil {
		t.Fatal(err)
	}
	_, legacy, err = s.LightRAGDocuments.VisibleHeads(ctx, "kb", []string{"x7"})
	if err != nil || legacy["x7"] {
		t.Fatal("legacy graph visible after re-upload", legacy, err)
	}
}

func TestIndexCleanupOrphanSourceAndSag(t *testing.T) {
	ctx := context.Background()
	s := newTestStores(t)
	content := []byte("X7 source")
	if err := s.Blobs.Put(ctx, "kb", SourceBucket, "x7.md", content, nil); err != nil {
		t.Fatal(err)
	}
	j := IndexCleanup{Target: "qdrant", Namespace: "kb", DocumentID: "missing", Collection: "kb", Version: "v1", Hash: sha256Sum(content)}
	if err := s.ObserveIndex(ctx, j); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE index_cleanup_jobs SET due_at=0"); err != nil {
		t.Fatal(err)
	}
	if err := s.ScheduleIndexCleanup(ctx); err != nil {
		t.Fatal(err)
	}
	jobs, err := s.ReadyIndexJobs(ctx)
	if err != nil || len(jobs) != 0 {
		t.Fatalf("referenced orphan was released %v %v", jobs, err)
	}
	for _, id := range []string{"manual", "checklist"} {
		if err := s.SagIndex.ReplaceDocument(ctx, "kb", id, "v1", []SagExtraction{{ChunkID: id, Event: "E03", Entities: []string{"X7", id}}}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SagIndex.DeleteDocument(ctx, "kb", "manual"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE sag_entities SET updated_at='2000-01-01'"); err != nil {
		t.Fatal(err)
	}
	if err := s.PurgeUnusedSagEntities(ctx); err != nil {
		t.Fatal(err)
	}
	if n := countTable(t, s, "sag_entities"); n != 2 {
		t.Fatalf("shared entity or orphan cleanup: got %d rows", n)
	}
}
