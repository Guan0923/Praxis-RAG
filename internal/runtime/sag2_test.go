package runtime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func TestSag2CorpusRevisionChangesWithEveryMutation(t *testing.T) {
	base := map[string]string{"doc-a": "v1", "doc-b": "v2"}
	revision := sag2CorpusRevision(base)
	if revision == "" {
		t.Fatal("revision is empty")
	}
	if second := sag2CorpusRevision(map[string]string{"doc-a": "v1", "doc-b": "v2"}); second != revision {
		t.Fatalf("equal maps produced %q and %q", revision, second)
	}
	if changed := sag2CorpusRevision(map[string]string{"doc-a": "v1"}); changed == revision {
		t.Fatal("removing a document kept the revision")
	}
	if changed := sag2CorpusRevision(map[string]string{"doc-a": "v1", "doc-b": "v3"}); changed == revision {
		t.Fatal("republishing a document kept the revision")
	}
	if changed := sag2CorpusRevision(map[string]string{"doc-a": "", "doc-b": "v2"}); changed == revision {
		t.Fatal("a soft-delete tombstone kept the revision")
	}
}

func TestSag2DocumentProjection(t *testing.T) {
	point := qdrant.Point{Payload: map[string]any{
		"record_type": "chunk",
		"chunk_id":    "doc-1:0",
		"document_id": "doc-1",
		"text":        "索引包含文档片段。",
		"title":        "维护手册",
		"source":       "manual.md",
		"metadata":    map[string]any{"pages": []any{float64(3)}},
	}}
	document, ok := sag2Document(point)
	if !ok {
		t.Fatal("chunk point was rejected")
	}
	if document.ChunkID != "doc-1:0" || document.DocumentID != "doc-1" ||
		document.Title != "维护手册" || document.Source != "manual.md" {
		t.Fatalf("document = %+v", document)
	}
	if document.Text != "索引包含文档片段。" {
		t.Fatalf("text = %q", document.Text)
	}

	// Assets and blank-text points never enter the lexical corpus.
	asset := qdrant.Point{Payload: map[string]any{"record_type": "asset", "asset_id": "a1"}}
	if _, ok := sag2Document(asset); ok {
		t.Fatal("asset point was projected as a document")
	}
	blank := qdrant.Point{Payload: map[string]any{"record_type": "chunk", "chunk_id": "c1", "text": "   "}}
	if _, ok := sag2Document(blank); ok {
		t.Fatal("blank-text chunk was projected as a document")
	}
}

// newSag2ResolverEngine builds an engine over the given shared MySQL test
// store with a placeholder Qdrant URL; the resolver itself never touches
// Qdrant. Callers keep the store to seed model profiles before resolving.
func newSag2ResolverEngine(t *testing.T, stores *state.Stores) *SearchEngine {
	t.Helper()
	engine, err := NewSearchEngine(config.RagConfig{
		Backend:    config.BackendQdrant,
		QdrantURL:  "http://127.0.0.1:1",
		Collection: "sag2_resolver_test",
		VectorSize: 1024,
	}, stores)
	if err != nil {
		t.Fatalf("NewSearchEngine() error = %v", err)
	}
	t.Cleanup(func() { _ = engine.Close(context.Background()) })
	return engine
}
// TestSag2ScorerResolverRequiresJeVProfile pins the resolver contract: without
// any jev profile the search fails with the safe operational error instead of
// falling back to a different scorer.
func TestSag2ScorerResolverRequiresJeVProfile(t *testing.T) {
	engine := newSag2ResolverEngine(t, newSearchTestStores(t))

	scorer, err := engine.sag2ScorerResolver()(context.Background())
	if scorer != nil || err == nil {
		t.Fatalf("resolver = %v, %v, want nil scorer and an error", scorer, err)
	}
	var operation *contracts.RagOperationError
	if !errors.As(err, &operation) {
		t.Fatalf("resolver error = %v, want RagOperationError", err)
	}
	if !strings.Contains(err.Error(), "SAG2 检索需要 JEV 决策模型") {
		t.Fatalf("resolver error = %v, want the missing-jev guidance", err)
	}
}

// TestSag2ScorerResolverRequiresCompleteProfile pins the incomplete-connection
// branch: a jev profile without a base URL or model name is rejected with the
// completion guidance instead of building a client.
func TestSag2ScorerResolverRequiresCompleteProfile(t *testing.T) {
	stores := newSearchTestStores(t)
	ctx := context.Background()
	if _, err := stores.ModelProfiles.Create(ctx, state.ModelProfile{
		Kind: "jev", Name: "半配置", BaseURL: "", Model: "jev-latest",
	}); err != nil {
		t.Fatalf("Create(half jev) error = %v", err)
	}
	engine := newSag2ResolverEngine(t, stores)

	scorer, err := engine.sag2ScorerResolver()(ctx)
	if scorer != nil || err == nil {
		t.Fatalf("resolver = %v, %v, want nil scorer and an error", scorer, err)
	}
	if !strings.Contains(err.Error(), "请补全 jev 模型配置的服务地址和模型名称") {
		t.Fatalf("resolver error = %v, want the completion guidance", err)
	}
}

// TestSag2ScorerResolverPicksMostRecentProfileAndBuildsClient pins the winner
// rule and the happy path: with several jev profiles the most recently updated
// one wins, its protected key decrypts and a JEV client comes back.
func TestSag2ScorerResolverPicksMostRecentProfileAndBuildsClient(t *testing.T) {
	stores := newSearchTestStores(t)
	engine := newSag2ResolverEngine(t, stores)
	ctx := context.Background()

	protected, err := state.Protect("jev-secret-key", stores.Settings.EncryptionKey)
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	older, err := stores.ModelProfiles.Create(ctx, state.ModelProfile{
		Kind: "jev", Name: "旧配置", BaseURL: "http://older.local/v1", Model: "jev-old",
		ProtectedKey: protected,
	})
	if err != nil {
		t.Fatalf("Create(older jev) error = %v", err)
	}
	recent, err := stores.ModelProfiles.Create(ctx, state.ModelProfile{
		Kind: "jev", Name: "新配置", BaseURL: "http://recent.local/v1", Model: "jev-latest",
		ProtectedKey: protected,
	})
	if err != nil {
		t.Fatalf("Create(recent jev) error = %v", err)
	}
	// CreatedAt/UpdatedAt are second-resolution timestamps: when both profiles
	// land in the same second, deterministically bump the newer row's
	// updated_at past the older one so the winner rule is observable.
	if recent.UpdatedAt <= older.UpdatedAt {
		bumped := time.Now().UTC().Add(time.Hour).Format("2006-01-02T15:04:05.000000+00:00")
		if _, err := stores.DB.ExecContext(ctx,
			"UPDATE model_profiles SET updated_at = ? WHERE id = ?", bumped, recent.ID); err != nil {
			t.Fatalf("bump updated_at error = %v", err)
		}
	}

	scorer, err := engine.sag2ScorerResolver()(ctx)
	if err != nil || scorer == nil {
		t.Fatalf("resolver = %v, %v, want a JEV scorer", scorer, err)
	}
}

// TestSag2ScorerResolverRejectsUndecryptableKey pins the decryption-failure
// branch: a protected key encrypted with another key fails closed with the
// decryption error and never builds a client.
func TestSag2ScorerResolverRejectsUndecryptableKey(t *testing.T) {
	stores := newSearchTestStores(t)
	ctx := context.Background()
	// A different well-formed 32-byte key: encryption succeeds, decryption
	// with the store's key fails.
	foreign, err := state.Protect("jev-secret-key", "MDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDAwMDA=")
	if err != nil {
		t.Fatalf("Protect() error = %v", err)
	}
	if _, err := stores.ModelProfiles.Create(ctx, state.ModelProfile{
		Kind: "jev", Name: "坏密钥", BaseURL: "http://jev.local/v1", Model: "jev-latest",
		ProtectedKey: foreign,
	}); err != nil {
		t.Fatalf("Create(bad-key jev) error = %v", err)
	}
	engine := newSag2ResolverEngine(t, stores)

	scorer, err := engine.sag2ScorerResolver()(ctx)
	if scorer != nil || err == nil {
		t.Fatalf("resolver = %v, %v, want nil scorer and an error", scorer, err)
	}
	if !strings.Contains(err.Error(), "JEV 模型配置密钥解密失败") {
		t.Fatalf("resolver error = %v, want the decryption failure", err)
	}
}
