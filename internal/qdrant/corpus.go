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
