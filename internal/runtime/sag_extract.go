package runtime

// This file wires the offline SAG extraction stage into the document publish
// path. It runs after the upload journal commits: extraction is deliberately
// non-fatal, because a chunk without an event simply falls back to direct
// chunk candidates during sag retrieval, so a missing or failing extraction
// model must never block or fail a published document.
import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"

	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// sagExtractionEnabled resolves the extraction connection from the
// admin-managed model profiles (kind=extraction), mirroring the JEV resolver.
// The most recently updated profile wins; no profile means the stage is off.
func (s *Service) sagExtractionEnabled(ctx context.Context) (*modelapi.ExtractionClient, error) {
	if s.stores == nil || s.stores.ModelProfiles == nil {
		return nil, nil
	}
	profiles, err := s.stores.ModelProfiles.ListByKind(ctx, "extraction")
	if err != nil {
		return nil, err
	}
	if len(profiles) == 0 {
		return nil, nil
	}
	profile := profiles[0]
	for _, candidate := range profiles[1:] {
		if candidate.UpdatedAt > profile.UpdatedAt {
			profile = candidate
		}
	}
	if strings.TrimSpace(profile.BaseURL) == "" || strings.TrimSpace(profile.Model) == "" {
		return nil, nil
	}
	apiKey, err := state.Unprotect(profile.ProtectedKey, s.stores.Settings.EncryptionKey)
	if err != nil {
		return nil, err
	}
	client := modelapi.NewExtractionClient(modelapi.ModelConnection{
		BaseURL: profile.BaseURL,
		Model:   profile.Model,
		APIKey:  apiKey,
	}, modelapi.DefaultTimeout)
	return client, nil
}

// The cap applies to queued extraction and to the legacy backfill CLI.
const (
	sagExtractionMaxChunks   = 512
	sagExtractionMaxParallel = 4
)

// extractSagBackfill builds the index for one published document from the CLI.
// A failed fragment is logged while other documents can still be processed.
func (s *Service) extractSagBackfill(
	ctx context.Context,
	namespace, documentID, version string,
	chunks []qdrant.Chunk,
) {
	client, err := s.sagExtractionEnabled(ctx)
	if err != nil || client == nil {
		if err != nil {
			s.sagExtractionLog(namespace, documentID, "extraction model resolution failed", err)
		}
		return
	}
	if s.stores == nil || s.stores.SagIndex == nil {
		return
	}
	bounded := chunks
	if len(bounded) > sagExtractionMaxChunks {
		bounded = bounded[:sagExtractionMaxChunks]
	}
	if len(bounded) == 0 {
		return
	}
	// Run the model calls with bounded parallelism; each result is independent.
	results := make([]state.SagExtraction, len(bounded))
	failures := make([]error, len(bounded))
	semaphore := make(chan struct{}, sagExtractionMaxParallel)
	group := &sync.WaitGroup{}
	for index, chunk := range bounded {
		group.Add(1)
		go func(index int, chunk qdrant.Chunk) {
			defer group.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			result, err := client.Extract(ctx, modelapi.ExtractionRequest{
				ChunkID: chunk.ChunkID,
				Text:    chunk.Text,
				Title:   chunk.Title,
			})
			if err != nil {
				failures[index] = err
				return
			}
			results[index] = state.SagExtraction{
				ChunkID:  result.ChunkID,
				Event:    result.Event,
				Entities: result.Entities,
			}
		}(index, chunk)
	}
	group.Wait()
	extractions := make([]state.SagExtraction, 0, len(results))
	failed := 0
	for _, result := range results {
		if result.Event == "" {
			continue
		}
		extractions = append(extractions, result)
	}
	for _, err := range failures {
		if err != nil {
			failed++
		}
	}
	if err := s.stores.SagIndex.BackfillDocument(ctx, namespace, documentID, version, extractions); err != nil {
		s.sagExtractionLog(namespace, documentID, "SAG index write failed", err)
		return
	}
	if failed > 0 {
		s.sagExtractionLog(namespace, documentID,
			fmt.Sprintf("SAG extraction: %d of %d chunks failed", failed, len(bounded)), nil)
	}
}

// BackfillSagIndex extracts and persists the SAG index rows for the
// documents of one namespace that have none yet (the backfill CLI entry).
// It returns the processed document count; a document whose chunks produce
// no extractions is still "processed" (its row state is simply empty).
//
// Chunks are read from the Qdrant active-version points (the same authority
// search uses), so the backfill never resurrects deleted documents.
func (s *Service) BackfillSagIndex(ctx context.Context, namespace string) (int, error) {
	if s == nil || s.stores == nil || s.stores.SagIndex == nil {
		return 0, fmt.Errorf("SAG 回填需要状态库与事件-实体索引")
	}
	documentIDs, err := s.stores.SagIndex.ListDocumentsWithoutEvents(ctx, namespace)
	if err != nil {
		return 0, err
	}
	if len(documentIDs) == 0 {
		return 0, nil
	}
	client, err := s.sagExtractionEnabled(ctx)
	if err != nil {
		return 0, err
	}
	if client == nil {
		return 0, fmt.Errorf("SAG 回填需要 kind=extraction 的模型配置（管理员在 Console「模型配置」中添加）")
	}
	// The tenant store resolves the collection and the active-version filter
	// for this namespace.
	tenant, err := s.search.tenant(ctx, namespace)
	if err != nil {
		return 0, err
	}
	versions, err := s.stores.Documents.VisibleVersions(ctx, namespace)
	if err != nil {
		return 0, err
	}
	processed := 0
	for _, documentID := range documentIDs {
		chunks, err := s.scrollDocumentChunks(ctx, namespace, tenant.store, documentID)
		if err != nil {
			return processed, err
		}
		if len(chunks) == 0 {
			// No active points: nothing to extract. Mark processed without
			// writing rows so the document is not re-listed forever.
			processed++
			continue
		}
		s.extractSagBackfill(ctx, namespace, documentID, versions[documentID], chunks)
		processed++
	}
	return processed, nil
}

// scrollDocumentChunks reads one document's active chunk points as the
// extraction inputs (chunk id + text + title only).
func (s *Service) scrollDocumentChunks(
	ctx context.Context,
	namespace string,
	store *qdrant.Store,
	documentID string,
) ([]qdrant.Chunk, error) {
	chunks := make([]qdrant.Chunk, 0)
	err := store.ScrollActiveChunks(ctx, func(points []qdrant.Point) error {
		for _, point := range points {
			payload := point.Payload
			if payload == nil {
				continue
			}
			if id, _ := payload["document_id"].(string); id != documentID {
				continue
			}
			chunkID, _ := payload["chunk_id"].(string)
			text, _ := payload["text"].(string)
			if chunkID == "" || strings.TrimSpace(text) == "" {
				continue
			}
			title, _ := payload["title"].(string)
			chunks = append(chunks, qdrant.Chunk{
				DocumentID: documentID,
				ChunkID:    chunkID,
				Text:       text,
				Title:      title,
			})
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return chunks, nil
}

// sagExtractionLog records one non-fatal extraction-stage failure. It never
// contains chunk text or model payloads, only the stage and a safe reason.
func (s *Service) sagExtractionLog(namespace, documentID, stage string, err error) {
	logger := slog.Default()
	attrs := []any{"namespace", namespace, "document_id", documentID}
	if err != nil {
		attrs = append(attrs, "error_type", fmt.Sprintf("%T", err))
	}
	logger.Warn("SAG extraction stage skipped: "+stage, attrs...)
}
