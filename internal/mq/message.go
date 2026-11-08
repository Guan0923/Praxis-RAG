package mq

import (
	"encoding/json"
	"os"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// lookupEnv is a test seam for environment reads.
var lookupEnv = os.LookupEnv

// EncodeDocumentNotification renders the exact schema-3 identifiers-only JSON
// payload: {"schema":3,"artifact_id":...,"namespace":...,"name":...,"created_at":...}.
// The payload is validated through internal/state so producer and state agree.
func EncodeDocumentNotification(notification state.DocumentNotification) ([]byte, string, error) {
	validated, err := state.ValidateDocumentNotification(map[string]any{
		"schema":      float64(notification.Schema),
		"artifact_id": notification.ArtifactID,
		"namespace":   notification.Namespace,
		"name":        notification.Name,
		"created_at":  notification.CreatedAt,
	})
	if err != nil {
		return nil, "", newSafeError(ErrPayloadInvalid, "encode document notification", err)
	}
	body, err := json.Marshal(validated)
	if err != nil {
		return nil, "", newSafeError(ErrPayloadInvalid, "encode document notification", err)
	}
	return body, validated.ArtifactID, nil
}

// DecodeDocumentNotification parses a native schema-3 notification or a raw
// Debezium change event (consumers tolerate both, exactly like the Python worker).
func DecodeDocumentNotification(body []byte) (state.DocumentNotification, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return state.DocumentNotification{}, newSafeError(ErrPayloadInvalid, "decode document notification", err)
	}
	notification, err := state.ValidateDocumentNotification(payload)
	if err != nil {
		return state.DocumentNotification{}, newSafeError(ErrPayloadInvalid, "decode document notification", err)
	}
	return notification, nil
}

// EncodeSearchEnvelope renders the exact schema-2 full-request JSON payload.
func EncodeSearchEnvelope(envelope state.SearchEnvelope) ([]byte, string, error) {
	validated, err := state.ValidateSearchEnvelope(map[string]any{
		"schema":     float64(envelope.Schema),
		"result_id":  envelope.ResultID,
		"namespace":  envelope.Namespace,
		"arguments":  envelope.Arguments,
		"created_at": envelope.CreatedAt,
		"expires_at": envelope.ExpiresAt,
		"deadline":   envelope.Deadline,
	})
	if err != nil {
		return nil, "", newSafeError(ErrPayloadInvalid, "encode search envelope", err)
	}
	body, err := json.Marshal(validated)
	if err != nil {
		return nil, "", newSafeError(ErrPayloadInvalid, "encode search envelope", err)
	}
	return body, validated.ResultID, nil
}

// DecodeSearchEnvelope parses and re-validates a schema-2 request envelope.
func DecodeSearchEnvelope(body []byte) (state.SearchEnvelope, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return state.SearchEnvelope{}, newSafeError(ErrPayloadInvalid, "decode search envelope", err)
	}
	envelope, err := state.ValidateSearchEnvelope(payload)
	if err != nil {
		return state.SearchEnvelope{}, newSafeError(ErrPayloadInvalid, "decode search envelope", err)
	}
	return envelope, nil
}
