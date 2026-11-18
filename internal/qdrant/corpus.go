package qdrant

import "context"

// ScrollActiveChunks visits every active-version chunk point of this namespace
// in pages, including legacy unversioned points that pass the publication
// filter. It is the corpus source for lexical retrieval modes that need the
// full chunk text and statistics (e.g. SAG2); vector search never needs it.
// The visit callback receives a freshly filtered page and must not retain it.
func (s *Store) ScrollActiveChunks(ctx context.Context, visit func([]Point) error) error {
	if err := s.EnsureCollection(ctx); err != nil {
		return err
	}
	query := s.queryFilter(map[string]any{"record_type": "chunk"})
	return s.scrollAll(ctx, query, 256, false, func(points []Point) error {
		chunks := make([]Point, 0, len(points))
		for _, point := range points {
			payload := point.Payload
			if payload == nil || payloadString(payload, "record_type") != "chunk" {
				continue
			}
			if !s.matchesNamespace(payload) {
				continue
			}
			chunks = append(chunks, point)
		}
		if len(chunks) == 0 {
			return nil
		}
		return visit(chunks)
	})
}

// HydrateChunks returns the active chunk points whose chunk_id is in the
// given set. The lookup scrolls the active corpus once and keeps the first
// match per id; it is the evidence-text source for candidates produced
// outside the vector recall (e.g. the SAG hyperedge expansion), which must
// still return the original chunk text as their evidence boundary.
func (s *Store) HydrateChunks(ctx context.Context, chunkIDs []string) ([]Point, error) {
	if len(chunkIDs) == 0 {
		return []Point{}, nil
	}
	wanted := make(map[string]struct{}, len(chunkIDs))
	for _, chunkID := range chunkIDs {
		wanted[chunkID] = struct{}{}
	}
	matches := make([]Point, 0, len(chunkIDs))
	err := s.ScrollActiveChunks(ctx, func(points []Point) error {
		for _, point := range points {
			payload := point.Payload
			if payload == nil {
				continue
			}
			chunkID, _ := payload["chunk_id"].(string)
			if _, ok := wanted[chunkID]; !ok {
				continue
			}
			delete(wanted, chunkID)
			matches = append(matches, point)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return matches, nil
}
