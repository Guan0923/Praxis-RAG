package qdrant

import (
	"context"
	"fmt"
)

// ReadVersionChunk reads one published or staged chunk by its deterministic
// point ID. The SAG worker must still fence the version before saving results.
func (s *Store) ReadVersionChunk(ctx context.Context, documentID, version, chunkID string) (Chunk, error) {
	pointID := s.versionPointID("chunk", chunkID, version, documentID)
	request := map[string]any{
		"ids": []string{pointID}, "with_payload": true, "with_vector": false,
	}
	var points []Point
	if err := s.client.post(ctx, s.collectionPath("/points"), nil, request, &points); err != nil {
		return Chunk{}, err
	}
	if len(points) != 1 || points[0].ID.String() != pointID {
		return Chunk{}, fmt.Errorf("Qdrant did not return the requested versioned chunk")
	}
	payload := points[0].Payload
	if payloadString(payload, "record_type") != "chunk" ||
		payloadString(payload, "document_id") != documentID ||
		payloadString(payload, "chunk_id") != chunkID ||
		payloadString(payload, "upload_version") != version ||
		!s.matchesNamespace(payload) {
		return Chunk{}, fmt.Errorf("Qdrant returned a different chunk version")
	}
	return Chunk{
		DocumentID: documentID,
		ChunkID:    chunkID,
		Text:       payloadString(payload, "text"),
		Title:      payloadString(payload, "title"),
	}, nil
}
