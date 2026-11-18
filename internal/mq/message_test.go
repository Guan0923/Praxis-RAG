package mq

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/state"
)

const (
	testDocumentID = "d1feec53-1036-4af0-8035-86ef6990fa6a"
)

// TestDecodeDocumentNotification covers the schema-4 contract: the native
// envelope and the raw Debezium document_jobs event both decode, deletes and
// tombstones are ignorable, malformed payloads are protocol errors.
func TestDecodeDocumentNotification(t *testing.T) {
	native := []byte(`{"schema":4,"artifact_id":"` + testDocumentID +
		`","namespace":"manuals","schedule_version":3}`)
	notification, err := DecodeDocumentNotification(native)
	if err != nil {
		t.Fatalf("native decode error = %v", err)
	}
	if notification.Schema != state.DocumentEnvelopeSchema ||
		notification.ArtifactID != testDocumentID || notification.Namespace != "manuals" ||
		notification.ScheduleVersion != 3 {
		t.Fatalf("native notification = %+v", notification)
	}

	for _, op := range []string{"r", "c", "u"} {
		body := []byte(`{"before":null,"after":{"artifact_id":"` + testDocumentID +
			`","namespace":"manuals","schedule_version":2},"source":{"table":"document_jobs"},` +
			`"op":"` + op + `","ts_ms":1700000000000}`)
		decoded, err := DecodeDocumentNotification(body)
		if err != nil {
			t.Fatalf("Debezium op=%s decode error = %v", op, err)
		}
		if decoded.ArtifactID != testDocumentID || decoded.ScheduleVersion != 2 ||
			decoded.Schema != state.DocumentEnvelopeSchema {
			t.Fatalf("Debezium op=%s notification = %+v", op, decoded)
		}
	}

	ignorable := []struct {
		name string
		body []byte
	}{
		{name: "delete", body: []byte(`{"before":{"artifact_id":"` + testDocumentID +
			`"},"after":null,"source":{"table":"document_jobs"},"op":"d"}`)},
		{name: "tombstone empty", body: nil},
		{name: "tombstone json null", body: []byte(`null`)},
		{name: "before-only", body: []byte(`{"before":{"artifact_id":"` + testDocumentID +
			`"},"after":null}`)},
	}
	for _, item := range ignorable {
		if _, err := DecodeDocumentNotification(item.body); !state.IgnorableCDCChange(err) {
			t.Fatalf("%s: error = %v, want ignorable", item.name, err)
		}
	}

	invalid := []struct {
		name string
		body []byte
	}{
		{name: "old schema 3", body: []byte(`{"schema":3,"artifact_id":"` + testDocumentID +
			`","namespace":"manuals","schedule_version":1}`)},
		{name: "missing schedule_version", body: []byte(`{"schema":4,"artifact_id":"` +
			testDocumentID + `","namespace":"manuals"}`)},
		{name: "bad uuid", body: []byte(`{"schema":4,"artifact_id":"nope","namespace":"manuals","schedule_version":1}`)},
		{name: "truncated json", body: []byte(`{"schema":4`)},
		{name: "trailing data", body: []byte(`{"schema":4} trailing`)},
		{name: "unsupported op", body: []byte(`{"before":{},"after":{},"op":"x"}`)},
	}
	for _, item := range invalid {
		if _, err := DecodeDocumentNotification(item.body); !errors.Is(err, ErrPayloadInvalid) {
			t.Fatalf("%s: error = %v, want ErrPayloadInvalid", item.name, err)
		}
	}
}

func locatorJSON(fields map[string]any) string {
	encoded, err := json.Marshal(fields)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// TestDecodeCacheChange covers the locator-only cache contract: whitelisted
// tables decode with only locator columns, everything else is ignorable.
func TestDecodeCacheChange(t *testing.T) {
	t.Run("documents update strips non-locator columns", func(t *testing.T) {
		body := []byte(`{"before":` + locatorJSON(map[string]any{
			"namespace": "manuals", "name": "a.pdf", "document_id": "doc-1",
			"del_flag": 0, "sha256": "secret-digest", "content": "body-bytes",
		}) + `,"after":` + locatorJSON(map[string]any{
			"namespace": "manuals", "name": "a.pdf", "document_id": "doc-1",
			"del_flag": 0, "sha256": "secret-digest", "content": "body-bytes",
		}) + `,"source":{"db":"praxis_rag","table":"documents"},"op":"u","ts_ms":1}`)
		change, err := DecodeCacheChange(body)
		if err != nil {
			t.Fatalf("decode error = %v", err)
		}
		if change.Table != "documents" || change.Op != "u" {
			t.Fatalf("change = %+v", change)
		}
		expected := map[string]any{"namespace": "manuals", "name": "a.pdf", "document_id": "doc-1", "del_flag": json.Number("0")}
		if !reflect.DeepEqual(change.After, expected) {
			t.Fatalf("after = %v, want %v", change.After, expected)
		}
		if !reflect.DeepEqual(change.Before, expected) {
			t.Fatalf("before = %v, want %v", change.Before, expected)
		}
	})

	t.Run("schema-qualified source table is normalized", func(t *testing.T) {
		body := []byte(`{"after":{"namespace":"manuals","version":2},"source":{"table":"praxis_rag.knowledge_bases"},"op":"u"}`)
		change, err := DecodeCacheChange(body)
		if err != nil || change.Table != "knowledge_bases" || change.Op != "u" {
			t.Fatalf("change = %+v, %v", change, err)
		}
		if change.After["version"] != json.Number("2") {
			t.Fatalf("version = %#v", change.After["version"])
		}
	})

	t.Run("insert without before image", func(t *testing.T) {
		body := []byte(`{"before":null,"after":{"namespace":"manuals","name":"a.pdf","document_id":"doc-1","del_flag":0},"source":{"table":"documents"},"op":"c"}`)
		change, err := DecodeCacheChange(body)
		if err != nil || change.Before != nil || change.Op != "c" {
			t.Fatalf("change = %+v, %v", change, err)
		}
	})

	t.Run("snapshot read op r is an upsert", func(t *testing.T) {
		body := []byte(`{"after":{"operation_id":"op-1","name":"extract","status":"DONE"},"source":{"table":"upload_steps"},"op":"r"}`)
		change, err := DecodeCacheChange(body)
		if err != nil || change.Op != "r" || change.After["status"] != "DONE" {
			t.Fatalf("change = %+v, %v", change, err)
		}
	})

	t.Run("delete keeps the before locators", func(t *testing.T) {
		body := []byte(`{"before":{"namespace":"manuals","resource":"doc:a.pdf","owner":"w1","operation_id":null},"after":null,"source":{"table":"document_leases"},"op":"d"}`)
		change, err := DecodeCacheChange(body)
		if err != nil || change.Op != "d" || change.After != nil {
			t.Fatalf("change = %+v, %v", change, err)
		}
		if change.Before["resource"] != "doc:a.pdf" || change.Before["operation_id"] != nil {
			t.Fatalf("before = %v", change.Before)
		}
	})

	t.Run("nullable locator column may be null", func(t *testing.T) {
		body := []byte(`{"after":{"id":"key-1","owner_user_id":null,"enabled":true},"source":{"table":"api_keys"},"op":"u"}`)
		change, err := DecodeCacheChange(body)
		if err != nil || change.Table != "api_keys" || change.After["enabled"] != true {
			t.Fatalf("change = %+v, %v", change, err)
		}
	})

	t.Run("native schema-1 envelope", func(t *testing.T) {
		body := []byte(`{"schema":1,"table":"mcp_catalogs","op":"u","before":null,"after":{"namespace":"manuals"}}`)
		change, err := DecodeCacheChange(body)
		if err != nil || change.Table != "mcp_catalogs" || change.After["namespace"] != "manuals" {
			t.Fatalf("change = %+v, %v", change, err)
		}
	})

	ignorable := []struct {
		name string
		body []byte
	}{
		{name: "tombstone empty", body: nil},
		{name: "tombstone json null", body: []byte(`null`)},
		{name: "unknown table", body: []byte(`{"after":{"id":"u1"},"source":{"table":"users"},"op":"c"}`)},
		{name: "ddl synthetic record", body: []byte(`{"databaseName":"praxis_rag","tableChanges":[]}`)},
		{name: "truncate", body: []byte(`{"source":{"table":"documents"},"op":"t"}`)},
	}
	for _, item := range ignorable {
		if _, err := DecodeCacheChange(item.body); !state.IgnorableCDCChange(err) {
			t.Fatalf("%s: error = %v, want ignorable", item.name, err)
		}
	}

	invalid := []struct {
		name string
		body []byte
	}{
		{name: "missing documents locator", body: []byte(`{"after":{"namespace":"manuals"},"source":{"table":"documents"},"op":"u"}`)},
		{name: "delete without before image", body: []byte(`{"before":null,"after":null,"source":{"table":"documents"},"op":"d"}`)},
		{name: "unsupported op", body: []byte(`{"after":{"namespace":"manuals","version":1},"source":{"table":"knowledge_bases"},"op":"x"}`)},
		{name: "bad native schema", body: []byte(`{"schema":2,"table":"documents","op":"u","after":{"namespace":"manuals","name":"a.pdf","document_id":"d","del_flag":0}}`)},
		{name: "malformed json", body: []byte(`{"after":`)},
		{name: "json array", body: []byte(`[]`)},
	}
	for _, item := range invalid {
		if _, err := DecodeCacheChange(item.body); !errors.Is(err, ErrPayloadInvalid) {
			t.Fatalf("%s: error = %v, want ErrPayloadInvalid", item.name, err)
		}
	}
}

// TestCacheChangeWhitelistMatchesConnectorProperties keeps the Go whitelist and
// deploy/debezium/application-cache.properties in sync: every captured table and
// column of the connector must be accepted here, and nothing else.
func TestCacheChangeWhitelistMatchesConnectorProperties(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "debezium", "application-cache.properties"))
	if err != nil {
		t.Fatalf("read connector properties: %v", err)
	}
	properties := parseJavaProperties(string(raw))
	tables := splitPropertyList(properties["debezium.source.table.include.list"])
	connectorTables := make([]string, 0, len(tables))
	for _, qualified := range tables {
		connectorTables = append(connectorTables, qualified[strings.LastIndex(qualified, ".")+1:])
	}
	sort.Strings(connectorTables)
	codeTables := make([]string, 0, len(cacheChangeLocators))
	for table := range cacheChangeLocators {
		codeTables = append(codeTables, table)
	}
	sort.Strings(codeTables)
	if !reflect.DeepEqual(connectorTables, codeTables) {
		t.Fatalf("cache tables: connector=%v code=%v", connectorTables, codeTables)
	}

	tableColumns := map[string]map[string]bool{}
	for _, qualified := range splitPropertyList(properties["debezium.source.column.include.list"]) {
		segments := strings.Split(qualified, ".")
		if len(segments) < 3 {
			t.Fatalf("unexpected column entry %q", qualified)
		}
		table := segments[len(segments)-2]
		column := segments[len(segments)-1]
		if tableColumns[table] == nil {
			tableColumns[table] = map[string]bool{}
		}
		tableColumns[table][column] = true
	}
	for table, locators := range cacheChangeLocators {
		captured := tableColumns[table]
		if captured == nil {
			t.Fatalf("table %s has no captured columns", table)
		}
		codeColumns := map[string]bool{}
		for _, locator := range locators {
			codeColumns[locator.column] = true
		}
		if !reflect.DeepEqual(captured, codeColumns) {
			t.Fatalf("table %s columns: connector=%v code=%v", table, captured, codeColumns)
		}
	}
}

// TestTaskConnectorPropertiesCaptureOnlyLocators pins the task connector
// filter: only the locator columns are captured and every noisy UPDATE is
// dropped by skip.messages.without.change.
func TestTaskConnectorPropertiesCaptureOnlyLocators(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "debezium", "application.properties"))
	if err != nil {
		t.Fatalf("read connector properties: %v", err)
	}
	properties := parseJavaProperties(string(raw))
	if properties["debezium.source.skip.messages.without.change"] != "true" {
		t.Fatalf("skip.messages.without.change = %q",
			properties["debezium.source.skip.messages.without.change"])
	}
	if properties["debezium.source.skipped.operations"] != "d" {
		t.Fatalf("skipped.operations = %q", properties["debezium.source.skipped.operations"])
	}
	expected := map[string]map[string]bool{
		"document_jobs": {"artifact_id": true, "namespace": true, "schedule_version": true},
	}
	captured := map[string]map[string]bool{}
	for _, qualified := range splitPropertyList(properties["debezium.source.column.include.list"]) {
		segments := strings.Split(qualified, ".")
		if len(segments) < 3 {
			t.Fatalf("unexpected column entry %q", qualified)
		}
		table := segments[len(segments)-2]
		if captured[table] == nil {
			captured[table] = map[string]bool{}
		}
		captured[table][segments[len(segments)-1]] = true
	}
	if !reflect.DeepEqual(captured, expected) {
		t.Fatalf("task connector columns = %v, want %v", captured, expected)
	}
	for _, key := range []string{
		"debezium.source.offset.storage.file.filename",
		"debezium.source.schema.history.internal.file.filename",
		"debezium.source.database.server.id",
	} {
		if strings.TrimSpace(properties[key]) == "" {
			t.Fatalf("%s is not configured", key)
		}
	}
	transformNames := splitPropertyList(properties["debezium.source.transforms"])
	if len(transformNames) != 1 || transformNames[0] != "RouteDocumentJobs" {
		t.Fatalf("transforms = %v, want only RouteDocumentJobs", transformNames)
	}
	replacements := make([]string, 0, len(transformNames))
	for _, name := range transformNames {
		prefix := "debezium.source.transforms." + name + "."
		if properties[prefix+"type"] != "io.debezium.transforms.ByLogicalTableRouter" {
			t.Fatalf("%s is not ByLogicalTableRouter", name)
		}
		if properties[prefix+"key.enforce.uniqueness"] != "false" {
			t.Fatalf("%s key.enforce.uniqueness is not false", name)
		}
		replacements = append(replacements, properties[prefix+"topic.replacement"])
	}
	if !strings.Contains(strings.Join(replacements, ","), "RAG_DOCUMENT_TOPIC") ||
		strings.Contains(strings.Join(replacements, ","), "RAG_SEARCH_TOPIC") {
		t.Fatalf("topic replacements = %v", replacements)
	}
}

// parseJavaProperties parses the simple key=value/comment format used by the
// connector files (no continuations or escapes are needed there).
func parseJavaProperties(content string) map[string]string {
	properties := map[string]string{}
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "!") {
			continue
		}
		index := strings.Index(line, "=")
		if index <= 0 {
			continue
		}
		properties[strings.TrimSpace(line[:index])] = strings.TrimSpace(line[index+1:])
	}
	return properties
}

// splitPropertyList splits one comma-separated connector list value.
func splitPropertyList(value string) []string {
	items := make([]string, 0, 4)
	for _, item := range strings.Split(value, ",") {
		item = strings.TrimSpace(item)
		if item != "" {
			items = append(items, item)
		}
	}
	return items
}

// readConnectorTemplate parses one Kafka Connect connector template. Placeholder
// values stay literal; the tests only inspect lists and fixed settings.
func readConnectorTemplate(t *testing.T, name string) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "deploy", "kafka-connect", "connectors", name))
	if err != nil {
		t.Fatalf("read connector template %s: %v", name, err)
	}
	var document struct {
		Name   string            `json:"name"`
		Config map[string]string `json:"config"`
	}
	if err := json.Unmarshal(raw, &document); err != nil {
		t.Fatalf("parse connector template %s: %v", name, err)
	}
	return document.Config
}

// TestCacheChangeWhitelistMatchesConnectorTemplate pins the cache connector's
// Kafka Connect template to the Go whitelist, exactly like the old Debezium
// Server properties test did.
func TestCacheChangeWhitelistMatchesConnectorTemplate(t *testing.T) {
	config := readConnectorTemplate(t, "cache-cdc.json")
	tables := splitPropertyList(config["table.include.list"])
	connectorTables := make([]string, 0, len(tables))
	for _, qualified := range tables {
		connectorTables = append(connectorTables, qualified[strings.LastIndex(qualified, ".")+1:])
	}
	sort.Strings(connectorTables)
	codeTables := make([]string, 0, len(cacheChangeLocators))
	for table := range cacheChangeLocators {
		codeTables = append(codeTables, table)
	}
	sort.Strings(codeTables)
	if !reflect.DeepEqual(connectorTables, codeTables) {
		t.Fatalf("cache tables: connector=%v code=%v", connectorTables, codeTables)
	}

	tableColumns := map[string]map[string]bool{}
	for _, qualified := range splitPropertyList(config["column.include.list"]) {
		segments := strings.Split(qualified, ".")
		if len(segments) < 3 {
			t.Fatalf("unexpected column entry %q", qualified)
		}
		table := segments[len(segments)-2]
		if tableColumns[table] == nil {
			tableColumns[table] = map[string]bool{}
		}
		tableColumns[table][segments[len(segments)-1]] = true
	}
	for table, locators := range cacheChangeLocators {
		captured := tableColumns[table]
		if captured == nil {
			t.Fatalf("table %s has no captured columns", table)
		}
		codeColumns := map[string]bool{}
		for _, locator := range locators {
			codeColumns[locator.column] = true
		}
		if !reflect.DeepEqual(captured, codeColumns) {
			t.Fatalf("table %s columns: connector=%v code=%v", table, captured, codeColumns)
		}
	}
	if config["skip.messages.without.change"] != "false" {
		t.Fatalf("cache skip.messages.without.change = %q, want false", config["skip.messages.without.change"])
	}
	if config["skipped.operations"] != "" {
		t.Fatalf("cache must not skip delete operations: skipped.operations = %q", config["skipped.operations"])
	}
	if config["tombstones.on.delete"] != "false" {
		t.Fatalf("cache tombstones.on.delete = %q, want false", config["tombstones.on.delete"])
	}
}

// TestDocumentConnectorTemplateCaptureOnlyLocators pins the document connector
// template: only locator columns are captured, only schedule_version UPDATEs are
// published, deletes are skipped and the Kafka Connect JSON converter keeps the
// envelope flat for the existing Go decoder.
func TestDocumentConnectorTemplateCaptureOnlyLocators(t *testing.T) {
	config := readConnectorTemplate(t, "document-cdc.json")
	if config["skip.messages.without.change"] != "true" {
		t.Fatalf("skip.messages.without.change = %q", config["skip.messages.without.change"])
	}
	if config["skipped.operations"] != "d" {
		t.Fatalf("skipped.operations = %q", config["skipped.operations"])
	}
	expected := map[string]map[string]bool{
		"document_jobs": {"artifact_id": true, "namespace": true, "schedule_version": true},
	}
	captured := map[string]map[string]bool{}
	for _, qualified := range splitPropertyList(config["column.include.list"]) {
		segments := strings.Split(qualified, ".")
		if len(segments) < 3 {
			t.Fatalf("unexpected column entry %q", qualified)
		}
		table := segments[len(segments)-2]
		if captured[table] == nil {
			captured[table] = map[string]bool{}
		}
		captured[table][segments[len(segments)-1]] = true
	}
	if !reflect.DeepEqual(captured, expected) {
		t.Fatalf("document connector columns = %v, want %v", captured, expected)
	}
	if config["transforms.RouteDocumentJobs.type"] != "io.debezium.transforms.ByLogicalTableRouter" ||
		config["transforms.RouteDocumentJobs.key.enforce.uniqueness"] != "false" ||
		!strings.Contains(config["transforms.RouteDocumentJobs.topic.replacement"], "RAG_DOCUMENT_TOPIC") {
		t.Fatalf("document routing = %v", config)
	}
}

// TestConnectorTemplatesUseKafkaConnectJSONAndSeparateHistory pins the settings
// shared by both templates: flat JSON converter (the Go decoder reads
// top-level before/after/op/source), Kafka-backed schema history, distinct
// schema history topics/server ids, and no file-based offsets (Kafka Connect's
// offset topic owns the binlog position so a worker failover resumes there).
func TestConnectorTemplatesUseKafkaConnectJSONAndSeparateHistory(t *testing.T) {
	document := readConnectorTemplate(t, "document-cdc.json")
	cache := readConnectorTemplate(t, "cache-cdc.json")
	for name, config := range map[string]map[string]string{"document": document, "cache": cache} {
		if config["connector.class"] != "io.debezium.connector.mysql.MySqlConnector" {
			t.Fatalf("%s connector.class = %q", name, config["connector.class"])
		}
		if config["tasks.max"] != "1" {
			t.Fatalf("%s tasks.max = %q, want 1 (one binlog reader per connector)", name, config["tasks.max"])
		}
		if config["value.converter"] != "org.apache.kafka.connect.json.JsonConverter" ||
			config["key.converter"] != "org.apache.kafka.connect.json.JsonConverter" {
			t.Fatalf("%s converters = %q/%q", name, config["key.converter"], config["value.converter"])
		}
		if config["value.converter.schemas.enable"] != "false" ||
			config["key.converter.schemas.enable"] != "false" {
			t.Fatalf("%s schemas.enable must stay false", name)
		}
		if config["snapshot.mode"] != "initial" {
			t.Fatalf("%s snapshot.mode = %q", name, config["snapshot.mode"])
		}
		if config["schema.history.internal.kafka.topic"] == "" ||
			config["schema.history.internal.kafka.bootstrap.servers"] == "" {
			t.Fatalf("%s schema history must use Kafka", name)
		}
		for key := range config {
			if strings.Contains(key, "offset.storage.file") || strings.HasPrefix(key, "offset.storage") {
				t.Fatalf("%s still configures file-based offsets: %s", name, key)
			}
		}
	}
	if document["schema.history.internal.kafka.topic"] == cache["schema.history.internal.kafka.topic"] {
		t.Fatal("the two connectors must use separate schema history topics")
	}
	if document["database.server.id"] == cache["database.server.id"] {
		t.Fatal("the two connectors must use distinct database.server.id values")
	}
	if document["topic.prefix"] == cache["topic.prefix"] {
		t.Fatal("the two connectors must use distinct topic prefixes")
	}
}

// TestDecodeConnectJSONConverterEnvelope feeds the physical shape the Kafka
// Connect JsonConverter writes (schemas disabled) through the production
// decoders: a document insert and a cache delete with before-image locators.
func TestDecodeConnectJSONConverterEnvelope(t *testing.T) {
	documentBody := []byte(`{"before":null,` +
		`"after":{"artifact_id":"` + testDocumentID + `","namespace":"manuals","schedule_version":4},` +
		`"source":{"version":"3.7.1.Final","connector":"mysql","name":"praxis_rag","ts_ms":1794135700187,` +
		`"snapshot":"false","db":"praxis_rag","table":"document_jobs","server_id":184054,"file":"mysql-bin.000002","pos":120,"row":0},` +
		`"op":"c","ts_ms":1794135700187}`)
	notification, err := DecodeDocumentNotification(documentBody)
	if err != nil {
		t.Fatalf("decode document envelope: %v", err)
	}
	if notification.ArtifactID != testDocumentID || notification.Namespace != "manuals" ||
		notification.ScheduleVersion != 4 {
		t.Fatalf("notification = %+v", notification)
	}

	cacheBody := []byte(`{"before":{"namespace":"manuals","name":"a.pdf","document_id":"doc-1","del_flag":1},` +
		`"after":null,` +
		`"source":{"version":"3.7.1.Final","connector":"mysql","name":"praxis_rag_cache","ts_ms":1794135700187,` +
		`"snapshot":"false","db":"praxis_rag","table":"documents","server_id":184055,"file":"mysql-bin.000002","pos":220,"row":0},` +
		`"op":"d","ts_ms":1794135700187}`)
	change, err := DecodeCacheChange(cacheBody)
	if err != nil {
		t.Fatalf("decode cache delete envelope: %v", err)
	}
	wantAfterDelete := map[string]any{"namespace": "manuals", "name": "a.pdf", "document_id": "doc-1", "del_flag": json.Number("1")}
	if change.Table != "documents" || change.Op != "d" || !reflect.DeepEqual(change.Before, wantAfterDelete) ||
		change.After != nil {
		t.Fatalf("change = %+v", change)
	}
}
