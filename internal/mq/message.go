package mq

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
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

// DocumentMessageKind classifies one payload from the document topic.
//
// Debezium tails document_job_outbox, not only document_jobs: routine outbox
// retention cleanup (state.DocumentJobs.GCOutbox) deletes expired rows from that
// table too, and binlog row-image FULL publishes those DELETEs on the same
// topic. Such records carry no job work, so consumers must acknowledge them
// instead of retrying and dead-lettering them like a poisoned notification.
type DocumentMessageKind int

const (
	// DocumentMessageJob is a document job notification: the native schema-3
	// envelope or a Debezium insert/read/update (op c/r/u) envelope. It must be
	// processed by the document worker's normal Claim/ingest path.
	DocumentMessageJob DocumentMessageKind = iota
	// DocumentMessageOutboxDeletion is a routine outbox-retention deletion:
	// a Debezium delete event (op "d", or a before-image-only envelope), the
	// Debezium Server tombstone (null/empty Kafka value), or a JSON null body.
	// It must be acknowledged and must never change a document job.
	DocumentMessageOutboxDeletion
)

// DecodeDocumentMessage classifies one document-topic payload and decodes it
// when it is a job notification.
//
// Deletion records return the zero state.DocumentNotification, a nil error and
// DocumentMessageOutboxDeletion; callers acknowledge them without retries or
// dead letters. Genuinely malformed payloads stay DocumentMessageJob with an
// ErrPayloadInvalid-wrapped error so the broker retry/dead-letter policy keeps
// applying to real poison messages.
func DecodeDocumentMessage(body []byte) (state.DocumentNotification, DocumentMessageKind, error) {
	if isOutboxDeletionRecord(body) {
		return state.DocumentNotification{}, DocumentMessageOutboxDeletion, nil
	}
	notification, err := DecodeDocumentNotification(body)
	if err != nil {
		return state.DocumentNotification{}, DocumentMessageJob, err
	}
	return notification, DocumentMessageJob, nil
}

// isOutboxDeletionRecord reports whether one payload is a routine outbox
// deletion rather than a job notification. It accepts:
//   - an empty/whitespace body, i.e. Debezium's null-value tombstone;
//   - the JSON literal null (tombstone with a non-empty value);
//   - a Debezium delete envelope carrying "op":"d" (with or without before);
//   - a before-image-only envelope (after null or absent, before an object),
//     which covers sinks or transformations that drop the op field.
//
// Malformed JSON and non-object JSON values are not deletions: they must keep
// the existing invalid-payload behaviour (nack, broker retry, dead letter).
func isOutboxDeletionRecord(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return true
	}
	var payload any
	if err := json.Unmarshal(trimmed, &payload); err != nil {
		return false
	}
	if payload == nil {
		// json.Unmarshal maps the JSON literal null to a nil interface value.
		return true
	}
	envelope, ok := payload.(map[string]any)
	if !ok {
		return false
	}
	if op, present := envelope["op"].(string); present && op == "d" {
		return true
	}
	// A non-null object before image with no after image means the row was
	// removed, regardless of whether the sink preserved the op field.
	if _, hasBefore := envelope["before"].(map[string]any); !hasBefore {
		return false
	}
	after, hasAfter := envelope["after"]
	return !hasAfter || after == nil
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
	payload, err := decodePayloadUseNumber(body)
	if err != nil {
		return state.SearchEnvelope{}, newSafeError(ErrPayloadInvalid, "decode search envelope", err)
	}
	envelope, err := state.ValidateSearchEnvelope(payload)
	if err != nil {
		return state.SearchEnvelope{}, newSafeError(ErrPayloadInvalid, "decode search envelope", err)
	}
	return envelope, nil
}

// decodePayloadUseNumber decodes exactly one JSON value while keeping every
// number as json.Number. Python's json module preserves ints, and the queued
// search contract validates candidate_k/top_k as integers; plain json.Unmarshal
// would yield float64 and break broker round-trips (the MCP/httpapi decoders use
// json.Decoder.UseNumber for the same reason).
func decodePayloadUseNumber(body []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var payload any
	if err := decoder.Decode(&payload); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("decode payload: trailing data")
		}
		return nil, err
	}
	return payload, nil
}
