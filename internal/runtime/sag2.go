package runtime

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
	"github.com/Guan0923/xinghe_rag/internal/retrieval"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// This file wires the sag2 retrieval mode into the service runtime. sag2 is
// the only mode whose corpus is materialized in memory: the lexical index
// needs document frequencies, per-field term frequencies and per-term
// candidate access that Qdrant's sparse vector search cannot provide.
//
// The corpus cache is keyed by the namespace's visible document versions
// (the same publication authority the Qdrant query filter uses), so a new
// published version invalidates the cache on the next sag2 search without
// touching the tenant itself. The JEV decision connection is resolved from
// the admin-managed model_profiles (kind=jev) on every search so Console
// edits take effect immediately.

// sag2CorpusCache memoizes one namespace's chunk corpus per visible-version
// fingerprint. It is owned by the loader closure inside one tenant, so tenant
// invalidation (SearchEngine.Invalidate) drops it with the tenant.
type sag2CorpusCache struct {
	mu        sync.Mutex
	revision  string
	documents []retrieval.LexicalDoc
}

// sag2CorpusLoader builds the loader for one tenant: it fingerprints the
// visible document versions and re-scrolls the Qdrant chunk corpus only when
// the fingerprint changed.
func (e *SearchEngine) sag2CorpusLoader(namespace string, store *qdrant.Store) retrieval.CorpusLoader {
	cache := &sag2CorpusCache{}
	return func(ctx context.Context) (retrieval.Sag2Corpus, error) {
		versions, err := e.stores.Documents.VisibleVersions(ctx, namespace)
		if err != nil {
			return retrieval.Sag2Corpus{}, err
		}
		revision := sag2CorpusRevision(versions)
		cache.mu.Lock()
		defer cache.mu.Unlock()
		if cache.documents != nil && cache.revision == revision {
			return retrieval.Sag2Corpus{Revision: revision, Documents: cache.documents}, nil
		}
		documents := make([]retrieval.LexicalDoc, 0, 256)
		err = store.ScrollActiveChunks(ctx, func(points []qdrant.Point) error {
			for _, point := range points {
				if document, ok := sag2Document(point); ok {
					documents = append(documents, document)
				}
			}
			return nil
		})
		if err != nil {
			return retrieval.Sag2Corpus{}, err
		}
		cache.revision, cache.documents = revision, documents
		return retrieval.Sag2Corpus{Revision: revision, Documents: documents}, nil
	}
}

// sag2CorpusRevision fingerprints the visible-version map. Any publish,
// overwrite or (soft-)delete changes the map and therefore the fingerprint.
func sag2CorpusRevision(versions map[string]string) string {
	identifiers := make([]string, 0, len(versions))
	for documentID, version := range versions {
		identifiers = append(identifiers, documentID+"\x00"+version)
	}
	sort.Strings(identifiers)
	digest := fnv.New64a()
	for _, identifier := range identifiers {
		_, _ = digest.Write([]byte(identifier))
		_, _ = digest.Write([]byte{'\n'})
	}
	return fmt.Sprintf("%016x", digest.Sum64())
}

// sag2Document projects one Qdrant chunk point into the lexical corpus.
// Points without indexable text are skipped, mirroring the ingestion rule
// that a published chunk always carries text.
func sag2Document(point qdrant.Point) (retrieval.LexicalDoc, bool) {
	payload := point.Payload
	if payload == nil {
		return retrieval.LexicalDoc{}, false
	}
	chunkID, _ := payload["chunk_id"].(string)
	text, _ := payload["text"].(string)
	if chunkID == "" || strings.TrimSpace(text) == "" {
		return retrieval.LexicalDoc{}, false
	}
	documentID, _ := payload["document_id"].(string)
	title, _ := payload["title"].(string)
	source, _ := payload["source"].(string)
	metadata := map[string]any{}
	if raw, ok := payload["metadata"].(map[string]any); ok {
		metadata = raw
	}
	return retrieval.LexicalDoc{
		ChunkID:    chunkID,
		DocumentID: documentID,
		Title:      title,
		Source:     source,
		Text:       text,
		Metadata:   metadata,
	}, true
}

// sag2ScorerResolver resolves the JEV decision connection on every sag2
// search from the admin-managed model profiles. When several jev profiles
// exist the most recently updated one wins; when none exists the search
// fails with a safe operational error instead of falling back.
func (e *SearchEngine) sag2ScorerResolver() func(context.Context) (retrieval.JevScorer, error) {
	return func(ctx context.Context) (retrieval.JevScorer, error) {
		profiles, err := e.stores.ModelProfiles.ListByKind(ctx, "jev")
		if err != nil {
			return nil, contracts.NewRagOperationError(fmt.Sprintf("SAG2 retrieval failed: JEV 模型配置读取失败: %s", errorTypeName(err)))
		}
		if len(profiles) == 0 {
			return nil, contracts.NewRagOperationError(
				"SAG2 检索需要 JEV 决策模型：请管理员在 Console「模型配置」中添加 kind=jev 的模型配置",
			)
		}
		profile := profiles[0]
		for _, candidate := range profiles[1:] {
			if candidate.UpdatedAt > profile.UpdatedAt {
				profile = candidate
			}
		}
		if strings.TrimSpace(profile.BaseURL) == "" || strings.TrimSpace(profile.Model) == "" {
			return nil, contracts.NewRagOperationError(
				"SAG2 检索需要 JEV 决策模型：请补全 jev 模型配置的服务地址和模型名称",
			)
		}
		apiKey, err := state.Unprotect(profile.ProtectedKey, e.stores.Settings.EncryptionKey)
		if err != nil {
			return nil, contracts.NewRagOperationError("SAG2 retrieval failed: JEV 模型配置密钥解密失败")
		}
		client := modelapi.NewJevClient(modelapi.ModelConnection{
			BaseURL: profile.BaseURL,
			Model:   profile.Model,
			APIKey:  apiKey,
		}, modelapi.DefaultTimeout)
		return client, nil
	}
}

// errorTypeName renders a safe error category for logs and user-facing failure
// summaries, mirroring Python's type(exc).__name__ based diagnostics (never
// message contents). The helper used to live in documents_publish.go; it is
// kept here for the SAG2 resolver after that file was retired.
func errorTypeName(err error) string {
	if err == nil {
		return "nil"
	}
	return fmt.Sprintf("%T", err)
}
