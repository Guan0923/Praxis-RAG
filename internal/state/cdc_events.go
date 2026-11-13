package state

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math"
)

// Notification schema versions (docs/cdc-migration.md §2). They are logical
// envelope versions: the physical Debezium JSON event carries no "schema" key
// because schemas.enable=false, but hand-published/test messages do.
const (
	// DocumentEnvelopeSchema is the identifier-only document task notification.
	DocumentEnvelopeSchema = 4
	// SearchEnvelopeSchema is the identifier-only queued-search notification.
	SearchEnvelopeSchema = 3
)

// ErrIgnorableCDCChange marks a Debezium change that is not a task record:
// a delete (op=d), a tombstone (null value), or a before-only row image.
// Consumers must ACK it without retry or DLQ. Use IgnorableCDCChange to test.
var ErrIgnorableCDCChange = errors.New("ignorable CDC change")

// DocumentNotification is the schema-4 document task notification. It carries
// locating identifiers only; the worker reads the complete job back from MySQL
// and only executes it when schedule_version equals the stored value.
type DocumentNotification struct {
	Schema          int    `json:"schema"`
	ArtifactID      string `json:"artifact_id"`
	Namespace       string `json:"namespace"`
	ScheduleVersion int64  `json:"schedule_version"`
}

// SearchNotification is the schema-3 queued-search notification. It carries
// locating identifiers only; the worker reads the arguments back from MySQL.
type SearchNotification struct {
	Schema          int    `json:"schema"`
	ResultID        string `json:"result_id"`
	Namespace       string `json:"namespace"`
	ScheduleVersion int64  `json:"schedule_version"`
}

// IgnorableCDCChange reports whether a decode error means "not a task record"
// rather than a malformed message.
func IgnorableCDCChange(err error) bool {
	return errors.Is(err, ErrIgnorableCDCChange)
}

// ValidateDocumentNotification accepts the application-native schema-4 envelope
// or a raw Debezium change event for document_jobs and returns the canonical
// notification. Deletes, tombstones and before-only changes return
// ErrIgnorableCDCChange; malformed messages return a *ValueError.
func ValidateDocumentNotification(payload any) (DocumentNotification, error) {
	fields, err := notificationFields(payload, DocumentEnvelopeSchema)
	if err != nil {
		return DocumentNotification{}, err
	}
	artifactID, err := canonicalNotificationID(fields, "artifact_id")
	if err != nil {
		return DocumentNotification{}, err
	}
	namespace, err := notificationNamespace(fields)
	if err != nil {
		return DocumentNotification{}, err
	}
	scheduleVersion, err := notificationScheduleVersion(fields)
	if err != nil {
		return DocumentNotification{}, err
	}
	return DocumentNotification{
		Schema:          DocumentEnvelopeSchema,
		ArtifactID:      artifactID,
		Namespace:       namespace,
		ScheduleVersion: scheduleVersion,
	}, nil
}

// ValidateSearchNotification accepts the application-native schema-3 envelope
// or a raw Debezium change event for search_tasks and returns the canonical
// notification. Deletes, tombstones and before-only changes return
// ErrIgnorableCDCChange; malformed messages return a *ValueError.
func ValidateSearchNotification(payload any) (SearchNotification, error) {
	fields, err := notificationFields(payload, SearchEnvelopeSchema)
	if err != nil {
		return SearchNotification{}, err
	}
	resultID, err := canonicalNotificationID(fields, "result_id")
	if err != nil {
		return SearchNotification{}, err
	}
	namespace, err := notificationNamespace(fields)
	if err != nil {
		return SearchNotification{}, err
	}
	scheduleVersion, err := notificationScheduleVersion(fields)
	if err != nil {
		return SearchNotification{}, err
	}
	return SearchNotification{
		Schema:          SearchEnvelopeSchema,
		ResultID:        resultID,
		Namespace:       namespace,
		ScheduleVersion: scheduleVersion,
	}, nil
}

// DocumentJobEnvelope builds the content-free schema-4 notification of one job.
func DocumentJobEnvelope(job *DocumentJob) (DocumentNotification, error) {
	if job == nil {
		return DocumentNotification{}, newValueError("Invalid document message")
	}
	return ValidateDocumentNotification(map[string]any{
		"schema":           float64(DocumentEnvelopeSchema),
		"artifact_id":      job.ArtifactID,
		"namespace":        job.Namespace,
		"schedule_version": job.ScheduleVersion,
	})
}

// SearchTaskEnvelope builds the content-free schema-3 notification of one task.
func SearchTaskEnvelope(task *SearchTask) (SearchNotification, error) {
	if task == nil {
		return SearchNotification{}, newValueError("Invalid search message envelope")
	}
	return ValidateSearchNotification(map[string]any{
		"schema":           float64(SearchEnvelopeSchema),
		"result_id":        task.ResultID,
		"namespace":        task.Namespace,
		"schedule_version": task.ScheduleVersion,
	})
}

// notificationFields normalizes either the native envelope or a Debezium
// change event into the locating-field map. Accepted carriers are the decoded
// map[string]any, the raw JSON bytes/string (decoded with UseNumber so integer
// ids and schedule_version survive), and json.RawMessage.
func notificationFields(payload any, schemaVersion int) (map[string]any, error) {
	var object map[string]any
	switch typed := payload.(type) {
	case nil:
		// JSON null value: a Kafka tombstone.
		return nil, ErrIgnorableCDCChange
	case map[string]any:
		object = typed
	case json.RawMessage:
		return notificationFields([]byte(typed), schemaVersion)
	case []byte:
		decoded, err := decodeCDCJSON(typed)
		if err != nil {
			return nil, newValueError("Invalid CDC message")
		}
		return notificationFields(decoded, schemaVersion)
	case string:
		decoded, err := decodeCDCJSON([]byte(typed))
		if err != nil {
			return nil, newValueError("Invalid CDC message")
		}
		return notificationFields(decoded, schemaVersion)
	default:
		return nil, newValueError("Invalid CDC message")
	}
	if object == nil {
		return nil, ErrIgnorableCDCChange
	}
	if rawSchema, native := object["schema"]; native {
		// A scalar schema is the application-native envelope. A schema object is
		// a Debezium event built with schemas.enable=true: unwrap its payload and
		// fall through to the after/op handling below.
		if _, schemaObject := rawSchema.(map[string]any); !schemaObject {
			schema, ok := integerValue(rawSchema)
			if !ok || schema != int64(schemaVersion) {
				return nil, newValueError("Invalid message schema")
			}
			return object, nil
		}
		if payload, ok := object["payload"].(map[string]any); ok {
			object = payload
		}
	}
	// Raw Debezium JSON change event (schemas.enable=false):
	// {"before":..., "after":..., "source":..., "op":"c", "ts_ms":...}
	op, _ := object["op"].(string)
	if op == "d" {
		return nil, ErrIgnorableCDCChange
	}
	afterRaw, hasAfter := object["after"]
	if !hasAfter || afterRaw == nil {
		// Tombstone (no after at all) or before-only delete image.
		return nil, ErrIgnorableCDCChange
	}
	after, ok := afterRaw.(map[string]any)
	if !ok {
		return nil, newValueError("Invalid CDC message")
	}
	switch op {
	case "c", "r", "u":
		return after, nil
	case "":
		return nil, newValueError("Invalid CDC message op")
	default:
		return nil, newValueError("Invalid CDC message op")
	}
}

// decodeCDCJSON decodes one complete JSON value with UseNumber. Trailing data is
// rejected so a concatenated or truncated message cannot be silently accepted.
func decodeCDCJSON(raw []byte) (any, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, newValueError("Invalid CDC message")
	}
	return value, nil
}

// canonicalNotificationID validates one canonical-UUID locating identifier.
func canonicalNotificationID(fields map[string]any, key string) (string, error) {
	value, ok := fields[key].(string)
	if !ok || !canonicalUUID(value) {
		return "", newValueError(key + " must be a canonical UUID")
	}
	return value, nil
}

// notificationNamespace validates the namespace locator. It is deliberately
// limited to the persisted column contract (1..64 characters): the Debezium row
// image is authoritative and the repository re-checks the namespace against the
// locked task row.
func notificationNamespace(fields map[string]any) (string, error) {
	namespace, ok := fields["namespace"].(string)
	if !ok || len(namespace) < 1 || len(namespace) > 64 {
		return "", newValueError("Invalid message namespace")
	}
	return namespace, nil
}

// notificationScheduleVersion validates the scheduling generation locator.
// Version 0/absent values are rejected: admitted tasks start at 1.
func notificationScheduleVersion(fields map[string]any) (int64, error) {
	raw, present := fields["schedule_version"]
	if !present || raw == nil {
		return 0, newValueError("schedule_version is required")
	}
	version, ok := integerValue(raw)
	if !ok || version < 1 {
		return 0, newValueError("Invalid schedule_version")
	}
	return version, nil
}

// integerValue normalizes a JSON-decoded number (float64 by default, json.Number
// with UseNumber) into an int64. Non-integral and out-of-range values fail.
func integerValue(value any) (int64, bool) {
	switch typed := value.(type) {
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || typed != math.Trunc(typed) ||
			typed < 1 || typed >= 9.223372036854776e18 {
			return 0, false
		}
		return int64(typed), true
	case float32:
		return integerValue(float64(typed))
	case int:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return parsed, true
	default:
		return 0, false
	}
}
