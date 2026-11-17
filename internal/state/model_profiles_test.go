package state

import (
	"context"
	"errors"
	"regexp"
	"testing"
)

// modelProfileIDPattern is the generated 32-lowercase-hex identifier shape
// (secrets.token_hex(16)).
var modelProfileIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestModelProfileRepositoryLifecycle(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	repository := stores.ModelProfiles
	if repository == nil {
		t.Fatal("Stores.ModelProfiles is nil")
	}

	if list, err := repository.List(ctx); err != nil || len(list) != 0 {
		t.Fatalf("initial List = %v, %v", list, err)
	}

	vectorSize := 1024
	embedding, err := repository.Create(ctx, ModelProfile{
		Kind: "embedding", Name: "bge-m3", BaseURL: "http://model.local/v1",
		Model: "BAAI/bge-m3", ProtectedKey: "enc:v1:key", VectorSize: &vectorSize,
	})
	if err != nil {
		t.Fatalf("Create(embedding) error = %v", err)
	}
	if !modelProfileIDPattern.MatchString(embedding.ID) {
		t.Fatalf("generated id = %q", embedding.ID)
	}
	if embedding.CreatedAt == "" || embedding.UpdatedAt == "" {
		t.Fatalf("timestamps were not generated: %+v", embedding)
	}
	// A reranker profile keeps vector_size NULL.
	reranker, err := repository.Create(ctx, ModelProfile{
		Kind: "reranker", Name: "bge-reranker", BaseURL: "http://model.local/v1",
		Model: "BAAI/bge-reranker-base",
	})
	if err != nil {
		t.Fatalf("Create(reranker) error = %v", err)
	}
	// A second embedding profile sorts after "bge-m3" by name.
	if _, err := repository.Create(ctx, ModelProfile{
		Kind: "embedding", Name: "a-embed", BaseURL: "http://other.local/v1",
		Model: "embed-small",
	}); err != nil {
		t.Fatalf("Create(second embedding) error = %v", err)
	}

	listed, err := repository.List(ctx)
	if err != nil {
		t.Fatalf("List error = %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("List returned %d rows, want 3", len(listed))
	}
	order := []string{listed[0].Kind + "/" + listed[0].Name, listed[1].Kind + "/" + listed[1].Name, listed[2].Kind + "/" + listed[2].Name}
	wantOrder := []string{"embedding/a-embed", "embedding/bge-m3", "reranker/bge-reranker"}
	for index := range wantOrder {
		if order[index] != wantOrder[index] {
			t.Fatalf("List order = %v, want %v", order, wantOrder)
		}
	}

	embeddings, err := repository.ListByKind(ctx, "embedding")
	if err != nil || len(embeddings) != 2 {
		t.Fatalf("ListByKind(embedding) = %v, %v", embeddings, err)
	}
	if embeddings[1].ID != embedding.ID || embeddings[1].VectorSize == nil || *embeddings[1].VectorSize != 1024 {
		t.Fatalf("stored embedding = %+v", embeddings[1])
	}
	if embeddings[1].ProtectedKey != "enc:v1:key" {
		t.Fatalf("protected key = %q", embeddings[1].ProtectedKey)
	}

	stored, err := repository.Get(ctx, reranker.ID)
	if err != nil || stored == nil {
		t.Fatalf("Get(reranker) = %v, %v", stored, err)
	}
	if stored.VectorSize != nil {
		t.Fatalf("reranker vector size = %v, want nil", *stored.VectorSize)
	}
	if missing, err := repository.Get(ctx, "0123456789abcdef0123456789abcdef"); err != nil || missing != nil {
		t.Fatalf("Get(missing) = %v, %v", missing, err)
	}

	// Update rewrites every mutable field and keeps the stored kind.
	updatedVector := 768
	updated, err := repository.Update(ctx, ModelProfile{
		ID: embedding.ID, Kind: "vlm", Name: "renamed", BaseURL: "http://new.local/v1",
		Model: "BAAI/bge-base", ProtectedKey: "enc:v1:new", VectorSize: &updatedVector,
		UpdatedAt: "2026-11-10T00:00:00.000000+00:00",
	})
	if err != nil {
		t.Fatalf("Update error = %v", err)
	}
	if updated.Kind != "embedding" || updated.Name != "renamed" ||
		updated.BaseURL != "http://new.local/v1" || updated.Model != "BAAI/bge-base" ||
		updated.ProtectedKey != "enc:v1:new" || updated.VectorSize == nil || *updated.VectorSize != 768 {
		t.Fatalf("updated profile = %+v", updated)
	}
	if updated.UpdatedAt != "2026-11-10T00:00:00.000000+00:00" {
		t.Fatalf("updated_at = %q", updated.UpdatedAt)
	}
	if updated.CreatedAt != embedding.CreatedAt {
		t.Fatalf("created_at changed: %q -> %q", embedding.CreatedAt, updated.CreatedAt)
	}

	if _, err := repository.Update(ctx, ModelProfile{ID: "missing", Name: "x"}); err == nil {
		t.Fatal("Update(missing) returned no error")
	} else {
		var lookup *LookupError
		if !errors.As(err, &lookup) || lookup.Message != "该模型配置不存在" {
			t.Fatalf("Update(missing) error = %v", err)
		}
	}

	deleted, err := repository.Delete(ctx, reranker.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete(reranker) = %v, %v", deleted, err)
	}
	if deleted, err := repository.Delete(ctx, reranker.ID); err != nil || deleted {
		t.Fatalf("second Delete(reranker) = %v, %v", deleted, err)
	}
}

func TestModelProfileRepositoryDuplicateNameConflict(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	repository := stores.ModelProfiles

	first := ModelProfile{Kind: "embedding", Name: "shared", BaseURL: "http://a.local/v1", Model: "embed-a", OwnerUserID: "user-a"}
	if _, err := repository.Create(ctx, first); err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if _, err := repository.Create(ctx, first); err == nil {
		t.Fatal("duplicate (kind, name) was accepted")
	} else {
		var conflict *StateConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("duplicate error = %v, want StateConflict", err)
		}
	}
	// 隔离的核心保证：两个账号可以各自保存同名同 kind 的配置。
	neighbour := ModelProfile{Kind: "embedding", Name: "shared", BaseURL: "http://c.local/v1", Model: "embed-c", OwnerUserID: "user-b"}
	if _, err := repository.Create(ctx, neighbour); err != nil {
		t.Fatalf("Create(same name, other owner) error = %v", err)
	}

	// The same display name is allowed for a different kind.
	other := ModelProfile{Kind: "reranker", Name: "shared", BaseURL: "http://a.local/v1", Model: "rank-a", OwnerUserID: "user-a"}
	if _, err := repository.Create(ctx, other); err != nil {
		t.Fatalf("Create(other kind) error = %v", err)
	}

	// Renaming a profile onto another profile's (kind, name) is a conflict too.
	second := ModelProfile{Kind: "embedding", Name: "renamed", BaseURL: "http://b.local/v1", Model: "embed-b", OwnerUserID: "user-a"}
	created, err := repository.Create(ctx, second)
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	created.Name = "shared"
	if _, err := repository.Update(ctx, created); err == nil {
		t.Fatal("duplicate rename was accepted")
	} else {
		var conflict *StateConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("duplicate rename error = %v, want StateConflict", err)
		}
	}
}

// TestModelProfileTableShape pins the storage contract: nullable protected key
// and vector size survive a round trip as empty/nil.
func TestModelProfileTableShape(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()

	created, err := stores.ModelProfiles.Create(ctx, ModelProfile{
		Kind: "vlm", Name: "qwen-vl", BaseURL: "http://vlm.local/v1", Model: "qwen2.5-vl-7b",
	})
	if err != nil {
		t.Fatalf("Create error = %v", err)
	}
	stored, err := stores.ModelProfiles.Get(ctx, created.ID)
	if err != nil || stored == nil {
		t.Fatalf("Get = %v, %v", stored, err)
	}
	if stored.ProtectedKey != "" || stored.VectorSize != nil {
		t.Fatalf("nullable columns = %q/%v, want empty/nil", stored.ProtectedKey, stored.VectorSize)
	}
}
