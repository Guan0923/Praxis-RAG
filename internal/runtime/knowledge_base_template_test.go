package runtime

import (
	"context"
	"errors"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/apierrors"
	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/kbconfig"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// newKBServiceWithConfig builds a Service over a fresh MySQL test store with an
// explicit retrieval configuration (for the RAG_DEFAULT_KB_TEMPLATE override).
func newKBServiceWithConfig(t *testing.T, cfg config.RagConfig) (*Service, *state.Stores) {
	t.Helper()
	stores := newSearchTestStores(t)
	if cfg.Backend == "" {
		cfg.Backend = config.BackendQdrant
	}
	if cfg.QdrantURL == "" {
		cfg.QdrantURL = "http://127.0.0.1:1"
	}
	service, err := New(cfg, stores, t.TempDir())
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = service.Close(context.Background()) })
	return service, stores
}

// TestCreateKnowledgeBaseForUserClonesDefaultTemplate pins the tenant
// self-create contract: the caller's provider fields are ignored, the default
// template's decrypted connections/models/vector size are cloned, the requested
// namespace and chunking are kept, and the new row is owned by the creator.
func TestCreateKnowledgeBaseForUserClonesDefaultTemplate(t *testing.T) {
	// An unreachable provider proves the clone replays the template's validated
	// configuration instead of probing the model APIs again.
	const templateURL = "http://127.0.0.1:1"
	service, stores := newKBService(t, templateURL)
	ctx := context.Background()
	template := testKnowledgeBaseSettings(t, "default", templateURL, "knowledge_template")
	if err := stores.KnowledgeBases.Create(ctx, template); err != nil {
		t.Fatalf("KnowledgeBases.Create(template) error = %v", err)
	}

	input := kbconfig.CreateInput{
		Namespace:      "mine",
		Chunking:       chunking.Config{Strategy: "character", ChunkSize: 200, ChunkOverlap: 25},
		EmbeddingModel: " ignore-me ",
		EmbeddingValues: map[string]any{
			"base_url": "http://evil.invalid/v1", "apikey": "evil-key",
		},
		RerankerModel:  "ignore-me",
		RerankerValues: map[string]any{"base_url": "http://evil.invalid/v1", "apikey": "evil-key"},
		VLMModel:       "ignore-me",
		VLMValues:      map[string]any{"base_url": "http://evil.invalid/v1", "apikey": "evil-key"},
	}
	created, err := service.CreateKnowledgeBaseForUser(ctx, "user-1", false, input)
	if err != nil {
		t.Fatalf("CreateKnowledgeBaseForUser() error = %v", err)
	}
	if created["namespace"] != "mine" {
		t.Errorf("namespace = %v", created["namespace"])
	}
	if created["embedding_model"] != "embed-a" || created["reranker_model"] != "rank-a" {
		t.Errorf("cloned models = %v/%v", created["embedding_model"], created["reranker_model"])
	}
	if created["vector_size"] != 3 {
		t.Errorf("vector_size = %v (%T), want template size 3", created["vector_size"], created["vector_size"])
	}
	if created["strategy"] != "character" || created["chunk_size"] != 200 || created["chunk_overlap"] != 25 {
		t.Errorf("request chunking was not kept: %v", created)
	}
	embedding, _ := created["embedding"].(map[string]any)
	if embedding["base_url"] != templateURL || embedding["api_key_set"] != true {
		t.Errorf("cloned embedding = %v", embedding)
	}
	if collection, _ := created["collection"].(string); collection == "knowledge_template" || !knowledgeCollectionPattern.MatchString(collection) {
		t.Errorf("collection = %q", created["collection"])
	}

	owner, err := stores.KnowledgeBases.OwnerOf(ctx, "mine")
	if err != nil || owner != "user-1" {
		t.Fatalf("OwnerOf(mine) = %q, %v", owner, err)
	}
	stored, err := stores.KnowledgeBases.Get(ctx, "mine")
	if err != nil || stored == nil {
		t.Fatalf("KnowledgeBases.Get(mine) = %v, %v", stored, err)
	}
	connection, err := kbconfig.ModelConnectionForKey(stored, "embedding", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey() error = %v", err)
	}
	if connection.APIKey != "embedding-key" || connection.Model != "embed-a" ||
		connection.BaseURL != templateURL || connection.VectorSize == nil || *connection.VectorSize != 3 {
		t.Errorf("stored embedding connection = %+v", connection)
	}
	if stored["tokenizer"] != "BAAI/bge-m3" {
		t.Errorf("tokenizer = %v", stored["tokenizer"])
	}
	// The template row itself keeps a NULL owner (administrator row).
	if owner, err := stores.KnowledgeBases.OwnerOf(ctx, "default"); err != nil || owner != "" {
		t.Errorf("template owner = %q, %v", owner, err)
	}
}

// TestCreateKnowledgeBaseForUserMissingTemplate pins the 409 conflict.
func TestCreateKnowledgeBaseForUserMissingTemplate(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	input := kbconfig.CreateInput{
		Namespace: "mine",
		Chunking:  chunking.Config{Strategy: "character", ChunkSize: 120, ChunkOverlap: 20},
	}
	_, err := service.CreateKnowledgeBaseForUser(ctx, "user-1", false, input)
	var conflict *apierrors.KnowledgeConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("CreateKnowledgeBaseForUser() error = %v, want KnowledgeConflictError", err)
	}
	if conflict.Message != messageDefaultKBTemplateMissing {
		t.Errorf("message = %q", conflict.Message)
	}
	if conflict.Error() != messageDefaultKBTemplateMissing {
		t.Errorf("Error() = %q", conflict.Error())
	}
	if stored, _ := stores.KnowledgeBases.Get(ctx, "mine"); stored != nil {
		t.Fatalf("knowledge base was created without a template")
	}

	// Request validation runs before the template lookup.
	_, err = service.CreateKnowledgeBaseForUser(ctx, "user-1", false, kbconfig.CreateInput{
		Namespace: "bad namespace",
		Chunking:  chunking.Config{Strategy: "character", ChunkSize: 120, ChunkOverlap: 20},
	})
	var validation *apierrors.KnowledgeValidationError
	if !errors.As(err, &validation) || validation.Message != kbconfig.MessageNamespaceInvalid {
		t.Fatalf("invalid namespace error = %v", err)
	}
}

// TestCreateKnowledgeBaseForUserCustomTemplateConfig pins RAG_DEFAULT_KB_TEMPLATE
// selecting the template namespace.
func TestCreateKnowledgeBaseForUserCustomTemplateConfig(t *testing.T) {
	service, stores := newKBServiceWithConfig(t, config.RagConfig{
		Backend:           config.BackendQdrant,
		QdrantURL:         "http://127.0.0.1:1",
		DefaultKBTemplate: "kb-template",
	})
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "kb-template", "http://127.0.0.1:1", "knowledge_template")); err != nil {
		t.Fatalf("KnowledgeBases.Create(template) error = %v", err)
	}
	created, err := service.CreateKnowledgeBaseForUser(ctx, "user-2", false, kbconfig.CreateInput{
		Namespace: "custom",
		Chunking:  chunking.Config{Strategy: "character", ChunkSize: 120, ChunkOverlap: 20},
	})
	if err != nil {
		t.Fatalf("CreateKnowledgeBaseForUser() error = %v", err)
	}
	if created["embedding_model"] != "embed-a" || created["vector_size"] != 3 {
		t.Errorf("created = %v", created)
	}
	if owner, _ := stores.KnowledgeBases.OwnerOf(ctx, "custom"); owner != "user-2" {
		t.Errorf("owner = %q", owner)
	}
}

// TestCreateKnowledgeBaseForUserAdminKeepsFullConfiguration pins the admin
// branch: the admin route semantics are unchanged and ownership is recorded.
// The legacy CreateKnowledgeBase keeps a NULL owner for compatibility.
func TestCreateKnowledgeBaseForUserAdminKeepsFullConfiguration(t *testing.T) {
	modelURL, _ := newFakeModelAPI(t, kbModelHandler(nil))
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()

	created, err := service.CreateKnowledgeBaseForUser(ctx, "admin-1", true, testCreateInput("admin-kb", modelURL))
	if err != nil {
		t.Fatalf("CreateKnowledgeBaseForUser(admin) error = %v", err)
	}
	if created["embedding_model"] != "embed-a" || created["vector_size"] != 3 {
		t.Errorf("admin create = %v", created)
	}
	if owner, _ := stores.KnowledgeBases.OwnerOf(ctx, "admin-kb"); owner != "admin-1" {
		t.Errorf("admin owner = %q", owner)
	}

	if _, err := service.CreateKnowledgeBase(ctx, testCreateInput("legacy", modelURL)); err != nil {
		t.Fatalf("CreateKnowledgeBase() error = %v", err)
	}
	if owner, _ := stores.KnowledgeBases.OwnerOf(ctx, "legacy"); owner != "" {
		t.Errorf("legacy row owner = %q, want NULL-owner compatibility", owner)
	}
}

// TestProviderInputConfigured pins the provider-completeness check that decides
// whether a create input can skip the default template.
func TestProviderInputConfigured(t *testing.T) {
	cases := []struct {
		name  string
		input kbconfig.CreateInput
		want  bool
	}{
		{name: "empty", input: kbconfig.CreateInput{}, want: false},
		{
			name:  "model only",
			input: kbconfig.CreateInput{EmbeddingModel: "embed-a"},
			want:  false,
		},
		{
			name: "provider draft without base_url",
			input: kbconfig.CreateInput{
				EmbeddingModel:  "embed-a",
				EmbeddingValues: map[string]any{"apikey": "k"},
			},
			want: false,
		},
		{
			name: "blank base_url",
			input: kbconfig.CreateInput{
				EmbeddingModel:  "embed-a",
				EmbeddingValues: map[string]any{"base_url": "   "},
			},
			want: false,
		},
		{
			name: "complete",
			input: kbconfig.CreateInput{
				EmbeddingModel:  "embed-a",
				EmbeddingValues: map[string]any{"base_url": "http://models.local/v1", "apikey": "k"},
			},
			want: true,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			if got := providerInputConfigured(testCase.input); got != testCase.want {
				t.Fatalf("providerInputConfigured() = %v, want %v", got, testCase.want)
			}
		})
	}
}

// TestCreateKnowledgeBaseForUserAdminUsesTemplateWhenProviderMissing pins the
// unified tenant self-create decision: an administrator calling
// POST /v1/knowledge-bases without provider fields is cloned from the template
// exactly like a normal user, and still becomes the owner. The admin route
// (full configuration) is covered by
// TestCreateKnowledgeBaseForUserAdminKeepsFullConfiguration.
func TestCreateKnowledgeBaseForUserAdminUsesTemplateWhenProviderMissing(t *testing.T) {
	const templateURL = "http://127.0.0.1:1"
	service, stores := newKBService(t, templateURL)
	ctx := context.Background()
	if err := stores.KnowledgeBases.Create(ctx,
		testKnowledgeBaseSettings(t, "default", templateURL, "knowledge_template")); err != nil {
		t.Fatalf("KnowledgeBases.Create(template) error = %v", err)
	}
	created, err := service.CreateKnowledgeBaseForUser(ctx, "admin-1", true, kbconfig.CreateInput{
		Namespace: "admin-self",
		Chunking:  chunking.Config{Strategy: "character", ChunkSize: 120, ChunkOverlap: 20},
	})
	if err != nil {
		t.Fatalf("CreateKnowledgeBaseForUser(admin, template) error = %v", err)
	}
	if created["embedding_model"] != "embed-a" || created["vector_size"] != 3 {
		t.Errorf("admin template create = %v", created)
	}
	embedding, _ := created["embedding"].(map[string]any)
	if embedding["base_url"] != templateURL || embedding["api_key_set"] != true {
		t.Errorf("admin template embedding = %v", embedding)
	}
	if owner, _ := stores.KnowledgeBases.OwnerOf(ctx, "admin-self"); owner != "admin-1" {
		t.Errorf("owner = %q", owner)
	}
}

// TestResolveDocumentIDs pins the name -> id mapping used by MCP visibility:
// missing names and empty entries are skipped and duplicate ids collapsed.
func TestResolveDocumentIDs(t *testing.T) {
	service, stores := newKBService(t, "http://127.0.0.1:1")
	ctx := context.Background()
	for _, record := range []state.DocumentRecord{
		{Name: "a.txt", DocumentID: "doc-a", ChunkCount: 1, Chunking: map[string]any{}},
		{Name: "b.txt", DocumentID: "doc-b", ChunkCount: 1, Chunking: map[string]any{}},
	} {
		if err := stores.Documents.Upsert(ctx, "alpha", record); err != nil {
			t.Fatalf("Documents.Upsert(%s) error = %v", record.Name, err)
		}
	}
	ids, err := service.ResolveDocumentIDs(ctx, "alpha", []string{"a.txt", "missing.txt", "b.txt", "a.txt", ""})
	if err != nil {
		t.Fatalf("ResolveDocumentIDs() error = %v", err)
	}
	if len(ids) != 2 || ids[0] != "doc-a" || ids[1] != "doc-b" {
		t.Fatalf("ids = %v", ids)
	}
	empty, err := service.ResolveDocumentIDs(ctx, "alpha", nil)
	if err != nil {
		t.Fatalf("ResolveDocumentIDs(nil) error = %v", err)
	}
	if empty == nil || len(empty) != 0 {
		t.Fatalf("empty ids = %#v, want non-nil empty slice", empty)
	}
	other, err := service.ResolveDocumentIDs(ctx, "other", []string{"a.txt"})
	if err != nil {
		t.Fatalf("ResolveDocumentIDs(other) error = %v", err)
	}
	if len(other) != 0 {
		t.Fatalf("other namespace ids = %v", other)
	}
}
