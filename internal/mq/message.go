package mq

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// lookupEnv is a test seam for environment reads.
var lookupEnv = os.LookupEnv

// cacheChangeSchema is the logical schema version of the cache change event
// (docs/cdc-migration.md §2.3). It only exists in hand-published/test messages;
// the physical Debezium event carries before/after/op/source without a schema key.
const cacheChangeSchema = 1

// DecodeDocumentNotification decodes one document-task topic payload.
//
// It accepts both message shapes a deployment can contain:
//   - the application-native schema-4 envelope:
//     {"schema":4,"artifact_id":...,"namespace":...,"schedule_version":...}
//   - the raw Debezium JSON change event of document_jobs
//     (schemas.enable=false, so before/after/op/source are top-level).
//
// Debezium only publishes INSERTs and schedule_version bumps for that table;
// op r/c/u are accepted. A delete (op=d), a tombstone (null/empty Kafka value)
// and a before-image-only record are not job work: they return
// state.ErrIgnorableCDCChange and callers ACK them without retry or dead letter.
// A genuinely malformed payload returns an ErrPayloadInvalid-wrapped error, so
// the broker retry/dead-letter policy keeps applying to real poison messages.
func DecodeDocumentNotification(body []byte) (state.DocumentNotification, error) {
	payload, err := decodeCDCPayload(body)
	if err != nil {
		return state.DocumentNotification{}, newSafeError(ErrPayloadInvalid, "decode document notification", err)
	}
	notification, err := state.ValidateDocumentNotification(payload)
	if err != nil {
		if state.IgnorableCDCChange(err) {
			return state.DocumentNotification{}, err
		}
		return state.DocumentNotification{}, newSafeError(ErrPayloadInvalid, "decode document notification", err)
	}
	return notification, nil
}

// CacheChange is one cache read-model change derived from a Debezium event.
//
// Before/After only ever contain the locator columns declared in
// deploy/debezium/application-cache.properties (never file bytes, query text,
// KB settings, catalog data or credentials). Op is the Debezium operation:
// c (create), u (update), r (initial-snapshot read) or d (delete).
type CacheChange struct {
	Table  string         `json:"table"`
	Op     string         `json:"op"`
	Before map[string]any `json:"before,omitempty"`
	After  map[string]any `json:"after,omitempty"`
}

// cacheLocator is one captured locator column of a cache-relevant table.
// nullable columns may legitimately be JSON null / absent.
type cacheLocator struct {
	column   string
	nullable bool
}

// cacheChangeLocators is the whitelist of tables and locator columns the cache
// connector captures (docs/cdc-migration.md §7.1). Any table or column outside
// this map is ignored by DecodeCacheChange, so a misconfigured connector cannot
// leak unrelated data into the cache consumer.
var cacheChangeLocators = map[string][]cacheLocator{
	"knowledge_bases":   {{column: "namespace"}, {column: "version"}},
	"documents":         {{column: "namespace"}, {column: "name"}, {column: "document_id"}, {column: "del_flag"}},
	"mcp_catalogs":      {{column: "namespace"}},
	"api_keys":          {{column: "id"}, {column: "owner_user_id", nullable: true}, {column: "enabled"}},
	"document_heads":    {{column: "namespace"}, {column: "document_id"}, {column: "version"}},
	"document_leases":   {{column: "namespace"}, {column: "resource"}, {column: "owner"}, {column: "operation_id", nullable: true}},
	"upload_operations": {{column: "id"}, {column: "namespace"}, {column: "name"}, {column: "status"}},
	"upload_steps":      {{column: "operation_id"}, {column: "name"}, {column: "status"}},
}

// DecodeCacheChange decodes one cache-topic payload.
//
// It accepts the raw Debezium JSON change event of any table captured by the
// cache connector, and the application-native schema-1 envelope
// {"schema":1,"table":...,"op":...,"before":...,"after":...} (tests and manual
// replay). Tombstones, delete-only shapes that carry no before image, DDL
// synthetic records and tables outside the whitelist return
// state.ErrIgnorableCDCChange (ACK without retry). Malformed payloads and known
// tables whose locator columns are missing return ErrPayloadInvalid (nack).
func DecodeCacheChange(body []byte) (CacheChange, error) {
	payload, err := decodeCDCPayload(body)
	if err != nil {
		return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change", err)
	}
	if payload == nil {
		// JSON null value: a Kafka tombstone.
		return CacheChange{}, state.ErrIgnorableCDCChange
	}
	object, ok := payload.(map[string]any)
	if !ok {
		return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change",
			errors.New("cache change must be a JSON object"))
	}

	var (
		table     string
		op        string
		beforeRaw any
		afterRaw  any
	)
	if _, native := object["schema"]; native {
		schema, ok := integerField(object["schema"])
		if !ok || schema != cacheChangeSchema {
			return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change",
				errors.New("invalid cache change schema"))
		}
		table, _ = object["table"].(string)
		op, _ = object["op"].(string)
		beforeRaw = object["before"]
		afterRaw = object["after"]
	} else {
		op, _ = object["op"].(string)
		beforeRaw = object["before"]
		afterRaw = object["after"]
		if source, ok := object["source"].(map[string]any); ok {
			table, _ = source["table"].(string)
		}
	}

	table = bareTableName(table)
	if table == "" || op == "" {
		// Debezium schema-change/DDL synthetic records carry no per-row image.
		return CacheChange{}, state.ErrIgnorableCDCChange
	}
	locators, known := cacheChangeLocators[table]
	if !known {
		return CacheChange{}, state.ErrIgnorableCDCChange
	}

	switch op {
	case "d":
		before, ok := beforeRaw.(map[string]any)
		if !ok {
			// A delete without a before image cannot locate the cache key.
			return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change",
				errors.New("delete change is missing the before image"))
		}
		change := CacheChange{Table: table, Op: op, Before: filterLocators(before, locators)}
		if err := requireLocators(change.Before, locators); err != nil {
			return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change", err)
		}
		return change, nil
	case "c", "u", "r":
		after, ok := afterRaw.(map[string]any)
		if !ok {
			return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change",
				errors.New("change is missing the after image"))
		}
		change := CacheChange{Table: table, Op: op, After: filterLocators(after, locators)}
		if before, ok := beforeRaw.(map[string]any); ok {
			change.Before = filterLocators(before, locators)
		}
		if err := requireLocators(change.After, locators); err != nil {
			return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change", err)
		}
		return change, nil
	case "t":
		// TRUNCATE: the consumer cannot map it to a concrete cache key.
		return CacheChange{}, state.ErrIgnorableCDCChange
	default:
		return CacheChange{}, newSafeError(ErrPayloadInvalid, "decode cache change",
			errors.New("unsupported CDC operation"))
	}
}

// decodeCDCPayload decodes exactly one JSON value, keeping numbers as
// json.Number (the state validators accept that form and range-check it).
//
// An empty/whitespace body and the JSON literal null both mean "tombstone" and
// return (nil, nil); callers turn that into a state.ErrIgnorableCDCChange.
func decodeCDCPayload(body []byte) (any, error) {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
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

// bareTableName strips an optional schema prefix ("praxis_rag.documents") from
// a Debezium source table name so the whitelist matches the bare table.
func bareTableName(table string) string {
	table = strings.TrimSpace(table)
	if index := strings.LastIndex(table, "."); index >= 0 {
		table = table[index+1:]
	}
	return table
}

// filterLocators keeps only the whitelisted locator columns of one row image.
func filterLocators(image map[string]any, locators []cacheLocator) map[string]any {
	filtered := make(map[string]any, len(locators))
	for _, locator := range locators {
		if value, present := image[locator.column]; present {
			filtered[locator.column] = value
		}
	}
	return filtered
}

// requireLocators rejects a change whose non-nullable locator columns are
// missing: the consumer could not address the affected cache key.
func requireLocators(image map[string]any, locators []cacheLocator) error {
	for _, locator := range locators {
		if locator.nullable {
			continue
		}
		if _, present := image[locator.column]; !present {
			return errors.New("cache change is missing locator column " + locator.column)
		}
	}
	return nil
}

// integerField normalizes a JSON-decoded integer field (float64, json.Number or
// an integral Go int) for the logical schema check.
func integerField(value any) (int, bool) {
	switch typed := value.(type) {
	case json.Number:
		parsed, err := typed.Int64()
		if err != nil {
			return 0, false
		}
		return int(parsed), true
	case float64:
		if typed != float64(int(typed)) {
			return 0, false
		}
		return int(typed), true
	case int:
		return typed, true
	case int64:
		return int(typed), true
	default:
		return 0, false
	}
}
