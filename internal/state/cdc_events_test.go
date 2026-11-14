package state

import (
	"encoding/json"
	"math"
	"testing"
)

const cdcTestResultID = "d1feec53-1036-4af0-8035-86ef6990fa6a"

func TestNotificationDecodeAcceptsNativeAndDebeziumFormats(t *testing.T) {
	// The application-native schema-4 document envelope.
	document, err := ValidateDocumentNotification(map[string]any{
		"schema": float64(DocumentEnvelopeSchema), "artifact_id": cdcTestResultID,
		"namespace": "manuals", "schedule_version": float64(3),
	})
	if err != nil || document.ArtifactID != cdcTestResultID || document.ScheduleVersion != 3 ||
		document.Schema != 4 || document.Namespace != "manuals" {
		t.Fatalf("document native envelope = %+v, %v", document, err)
	}
	// Raw Debezium JSON change events (schemas.enable=false); op r/c/u are the
	// only task-producing operations. json.Number is what UseNumber produces.
	for _, op := range []string{"c", "r", "u"} {
		payload := map[string]any{"before": nil, "op": op, "ts_ms": json.Number("1700000000000"),
			"after": map[string]any{"artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": json.Number("7")}}
		if notification, err := ValidateDocumentNotification(payload); err != nil || notification.ScheduleVersion != 7 {
			t.Fatalf("document op %q = %+v, %v", op, notification, err)
		}
	}
	// Integer coding variants are normalized to int64.
	for _, value := range []any{float64(5), int(5), int64(5), json.Number("5")} {
		notification, err := ValidateDocumentNotification(map[string]any{
			"schema": float64(DocumentEnvelopeSchema), "artifact_id": cdcTestResultID,
			"namespace": "manuals", "schedule_version": value,
		})
		if err != nil || notification.ScheduleVersion != 5 {
			t.Fatalf("schedule_version %#v = %+v, %v", value, notification, err)
		}
	}
	// The consumer may hand the validator the raw JSON bytes/string instead of a
	// decoded map; UseNumber keeps integer fields exact.
	raw := `{"before":null,"after":{"artifact_id":"` + cdcTestResultID +
		`","namespace":"manuals","schedule_version":11},"op":"u"}`
	for _, carrier := range []any{[]byte(raw), json.RawMessage(raw), raw} {
		notification, err := ValidateDocumentNotification(carrier)
		if err != nil || notification.ScheduleVersion != 11 {
			t.Fatalf("raw carrier %T = %+v, %v", carrier, notification, err)
		}
	}
	if _, err := ValidateDocumentNotification([]byte("null")); !IgnorableCDCChange(err) {
		t.Fatalf("raw tombstone error = %v, want ignorable", err)
	}
	if _, err := ValidateDocumentNotification([]byte(raw + "trailing")); err == nil {
		t.Fatal("raw payload with trailing data was accepted")
	}
	// schemas.enable=true also works: the top-level schema is an object and the
	// change image lives under payload.
	withSchema := map[string]any{
		"schema": map[string]any{"type": "struct", "name": "server.praxis.document_jobs.Envelope"},
		"payload": map[string]any{
			"before": nil, "op": "c",
			"after": map[string]any{"artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": json.Number("12")},
		},
	}
	notification, err := ValidateDocumentNotification(withSchema)
	if err != nil || notification.ScheduleVersion != 12 {
		t.Fatalf("schemas.enable=true payload = %+v, %v", notification, err)
	}
	encodedSchema, err := json.Marshal(withSchema)
	if err != nil {
		t.Fatalf("json.Marshal(withSchema) error = %v", err)
	}
	if notification, err := ValidateDocumentNotification(encodedSchema); err != nil || notification.ScheduleVersion != 12 {
		t.Fatalf("schemas.enable=true raw payload = %+v, %v", notification, err)
	}
}

func TestNotificationDecodeClassifiesIgnorableChanges(t *testing.T) {
	payloads := map[string]any{
		"json null tombstone": nil,
		"delete op": map[string]any{
			"before": map[string]any{"artifact_id": cdcTestResultID}, "after": nil, "op": "d",
		},
		"delete without before image": map[string]any{"after": nil, "op": "d"},
		"before only":                 map[string]any{"before": map[string]any{"result_id": cdcTestResultID}},
		"truncate":                    map[string]any{"op": "t"},
	}
	for name, payload := range payloads {
		if _, err := ValidateDocumentNotification(payload); !IgnorableCDCChange(err) {
			t.Fatalf("%s: document error = %v, want ignorable", name, err)
		}
	}
	if IgnorableCDCChange(nil) {
		t.Fatal("nil is not an ignorable change")
	}
}

func TestNotificationDecodeRejectsMalformedMessages(t *testing.T) {
	documentCases := map[string]any{
		"not an object":    "x",
		"old schema three": map[string]any{"schema": float64(3), "artifact_id": cdcTestResultID, "namespace": "manuals"},
		"bad schema type":  map[string]any{"schema": "4", "artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": float64(1)},
		"bad artifact":     map[string]any{"schema": float64(4), "artifact_id": "not-a-uuid", "namespace": "manuals", "schedule_version": float64(1)},
		"empty namespace":  map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "", "schedule_version": float64(1)},
		"long namespace":   map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": string(make([]byte, 65)), "schedule_version": float64(1)},
		"missing version":  map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "manuals"},
		"zero version":     map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": float64(0)},
		"fraction version": map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": float64(1.5)},
		"NaN version":      map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": math.NaN()},
		"infinite version": map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": math.Inf(1)},
		"string version":   map[string]any{"schema": float64(4), "artifact_id": cdcTestResultID, "namespace": "manuals", "schedule_version": "1"},
		"after not object": map[string]any{"after": "row", "op": "c"},
		"after without op": map[string]any{"after": map[string]any{"artifact_id": cdcTestResultID}},
		"unknown op":       map[string]any{"after": map[string]any{"artifact_id": cdcTestResultID}, "op": "x"},
	}
	for name, payload := range documentCases {
		if _, err := ValidateDocumentNotification(payload); err == nil || IgnorableCDCChange(err) {
			t.Fatalf("%s: document payload accepted (err=%v)", name, err)
		}
	}
}

func TestNotificationEnvelopeBuilders(t *testing.T) {
	job := &DocumentJob{
		ArtifactID: cdcTestResultID, Namespace: "manuals", Name: "a.txt",
		Content: []byte("secret bytes"), ScheduleVersion: 9,
	}
	document, err := DocumentJobEnvelope(job)
	if err != nil || document.Schema != DocumentEnvelopeSchema || document.ScheduleVersion != 9 {
		t.Fatalf("DocumentJobEnvelope() = %+v, %v", document, err)
	}
	encoded, err := marshalJSON(document)
	if err != nil {
		t.Fatalf("marshalJSON() error = %v", err)
	}
	if containsStringText(encoded, "secret bytes") || containsStringText(encoded, "a.txt") {
		t.Fatalf("document notification leaked job content: %s", encoded)
	}
	if _, err := DocumentJobEnvelope(nil); err == nil {
		t.Fatal("nil job produced a notification")
	}
}

// containsStringText reports whether haystack contains needle.
func containsStringText(haystack, needle string) bool {
	if len(needle) == 0 {
		return true
	}
	for index := 0; index+len(needle) <= len(haystack); index++ {
		if haystack[index:index+len(needle)] == needle {
			return true
		}
	}
	return false
}
