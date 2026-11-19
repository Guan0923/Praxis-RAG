package runtime

import (
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

func sagExtractionChunkIDs(chunks []qdrant.Chunk) []string {
	chunkIDs := make([]string, 0, min(len(chunks), sagExtractionMaxChunks))
	seen := make(map[string]bool, cap(chunkIDs))
	for _, chunk := range chunks[:min(len(chunks), sagExtractionMaxChunks)] {
		if chunk.ChunkID != "" && strings.TrimSpace(chunk.Text) != "" && !seen[chunk.ChunkID] {
			chunkIDs = append(chunkIDs, chunk.ChunkID)
			seen[chunk.ChunkID] = true
		}
	}
	return chunkIDs
}

// RunSagExtraction drains due chunks after Kafka wakeups and periodically
// recovers work whose notification was missed, retry is due, or lease expired.
func (s *Service) RunSagExtraction(ctx context.Context, wake <-chan struct{}, logger *slog.Logger, beat func()) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	qdrantClient := qdrant.NewClient(s.config.QdrantURL, "", nil)

	for ctx.Err() == nil {
		beat()
		// Re-read the model between batches so Console changes take effect.
		filled, err := s.drainSagExtractions(ctx, qdrantClient, logger)
		if err != nil && ctx.Err() == nil {
			logger.Warn("SAG extraction pass failed", logging.ErrorAttrs(err)...)
		}
		if filled && err == nil {
			continue
		}
		select {
		case <-ctx.Done():
		case <-ticker.C:
		case <-wake:
		}
	}
}

func (s *Service) drainSagExtractions(ctx context.Context, qdrantClient *qdrant.Client, logger *slog.Logger) (bool, error) {
	model, err := s.sagExtractionEnabled(ctx)
	if err != nil || model == nil {
		return false, err
	}
	var active sync.WaitGroup
	var failures [sagExtractionMaxParallel]error
	claimed := 0
	for slot := range failures {
		job, claimErr := s.stores.SagIndex.ClaimExtraction(ctx)
		if claimErr != nil || job == nil {
			failures[slot] = claimErr
			break
		}
		claimed++
		active.Go(func() {
			failures[slot] = s.processSagExtraction(ctx, qdrantClient, model, job, logger)
		})
	}
	active.Wait()
	return claimed == len(failures), errors.Join(failures[:]...)
}

func (s *Service) processSagExtraction(ctx context.Context, qdrantClient *qdrant.Client,
	model *modelapi.ExtractionClient, job *state.SagExtractionJob, logger *slog.Logger) error {
	work, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	repository := s.stores.SagIndex
	collection, err := repository.ExtractionCollection(work, job)
	if err != nil {
		return err
	}
	if collection == "" {
		return repository.FinishExtraction(ctx, job, nil, "")
	}

	store := qdrant.NewStoreWithClient(qdrant.StoreConfig{Namespace: job.Namespace, Collection: collection}, qdrantClient)
	chunk, err := store.ReadVersionChunk(work, job.DocumentID, job.Version, job.ChunkID)
	failure := "qdrant_read_failed"
	if err == nil {
		var result modelapi.ExtractionResult
		result, err = model.Extract(work, modelapi.ExtractionRequest{ChunkID: chunk.ChunkID, Text: chunk.Text, Title: chunk.Title})
		if err == nil {
			return repository.FinishExtraction(ctx, job, &state.SagExtraction{
				ChunkID: result.ChunkID, Event: result.Event, Entities: result.Entities,
			}, "")
		}
		failure = "model_extraction_failed"
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	logger.Warn("SAG chunk extraction failed", "namespace", job.Namespace, "document_id", job.DocumentID,
		"version", job.Version, "chunk_id", job.ChunkID, "category", failure)
	return repository.FinishExtraction(ctx, job, nil, failure)
}
