// This file tests the knowledge-graph configuration: the graph extraction
// model connection, the graph_enabled switch and its legacy-row default.

package kbconfig

import (
	"context"
	"testing"
)

func TestCreateSettingsStoresGraphProvider(t *testing.T) {
	prober := &fakeProber{vectorSize: 3}
	input := newTestCreateInput("http://host:8080/v1/")
	input.GraphModel = " graph-llm "
	input.GraphValues = map[string]any{"base_url": "http://graph:8000/v1/", "apikey": "graph-key"}
	input.GraphEnabled = true
	settings, err := CreateSettings(context.Background(), input, prober)
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	// Saving then reading the graph connection must round-trip base_url, model
	// and the decrypted API key, exactly like the other credential kinds.
	connection, err := ModelConnectionForKey(settings, "graph", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(graph) error: %v", err)
	}
	if connection.BaseURL != "http://graph:8000/v1" || connection.Model != "graph-llm" ||
		connection.APIKey != "graph-key" {
		t.Fatalf("graph connection = %+v", connection)
	}
	if settings["graph_model"] != "graph-llm" || settings["graph_enabled"] != true {
		t.Fatalf("stored graph fields = %v/%v", settings["graph_model"], settings["graph_enabled"])
	}
	enabled, err := GraphEnabled(settings)
	if err != nil || !enabled {
		t.Fatalf("GraphEnabled() = %v, %v", enabled, err)
	}
	public := PublicSettings(settings)
	graph, ok := public["graph"].(map[string]any)
	if !ok || graph["base_url"] != "http://graph:8000/v1" || graph["api_key_set"] != true {
		t.Fatalf("public graph = %v", public["graph"])
	}
	if public["graph_model"] != "graph-llm" || public["graph_enabled"] != true {
		t.Fatalf("public graph fields = %v/%v", public["graph_model"], public["graph_enabled"])
	}
}

func TestCreateSettingsGraphDefaultsOff(t *testing.T) {
	input := newTestCreateInput("http://host:8080/v1")
	settings, err := CreateSettings(context.Background(), input, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("CreateSettings() error: %v", err)
	}
	if settings["graph_enabled"] != false {
		t.Fatalf("graph_enabled = %v, want false", settings["graph_enabled"])
	}
	if settings["graph_model"] != "" {
		t.Fatalf("graph_model = %v, want empty", settings["graph_model"])
	}
	enabled, err := GraphEnabled(settings)
	if err != nil || enabled {
		t.Fatalf("GraphEnabled() = %v, %v", enabled, err)
	}
	connection, err := ModelConnectionForKey(settings, "graph", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(graph) error: %v", err)
	}
	if connection.Configured() {
		t.Fatalf("unconfigured graph connection = %+v", connection)
	}
}

func TestCreateSettingsRejectsIncompleteGraph(t *testing.T) {
	cases := map[string]struct {
		mutate  func(*CreateInput)
		message string
	}{
		"model without url": {
			mutate:  func(input *CreateInput) { input.GraphModel = "graph-llm" },
			message: MessageGraphURLRequired,
		},
		"url without model": {
			mutate:  func(input *CreateInput) { input.GraphValues = map[string]any{"base_url": "http://graph:8000/v1"} },
			message: MessageGraphModelRequired,
		},
	}
	for name, testCase := range cases {
		t.Run(name, func(t *testing.T) {
			input := newTestCreateInput("http://host:8080/v1/")
			testCase.mutate(&input)
			_, err := CreateSettings(context.Background(), input, &fakeProber{vectorSize: 3})
			assertValidationMessage(t, err, testCase.message)
		})
	}
}

func TestGraphEnabled(t *testing.T) {
	cases := []struct {
		name     string
		settings map[string]any
		want     bool
		wantErr  string
	}{
		{"legacy row without the key", map[string]any{"namespace": "alpha"}, false, ""},
		{"nil value", map[string]any{"graph_enabled": nil}, false, ""},
		{"explicit false", map[string]any{"graph_enabled": false}, false, ""},
		{"explicit true", map[string]any{"graph_enabled": true}, true, ""},
		{"non-boolean value", map[string]any{"graph_enabled": "true"}, false, MessageGraphEnabledInvalid},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			enabled, err := GraphEnabled(testCase.settings)
			if testCase.wantErr != "" {
				assertValidationMessage(t, err, testCase.wantErr)
				return
			}
			if err != nil || enabled != testCase.want {
				t.Fatalf("GraphEnabled() = %v, %v; want %v", enabled, err, testCase.want)
			}
		})
	}
}

// TestUpdateSettingsGraphSwitch covers the graph_enabled default and backfill
// rule: legacy rows read as disabled, unrelated updates leave the settings shape
// alone, and an explicit request configures the graph connection, its model and
// the switch itself.
func TestUpdateSettingsGraphSwitch(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	// A legacy row carries no graph fields at all and must read as disabled
	// instead of raising a corrupt-settings error.
	enabled, err := GraphEnabled(current)
	if err != nil || enabled {
		t.Fatalf("legacy GraphEnabled() = %v, %v", enabled, err)
	}
	unchanged, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"embedding_model": "embed-a"},
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("UpdateSettings() error: %v", err)
	}
	// The settings shape stays stable: an unrelated update must not backfill
	// graph_enabled into a row written before the switch existed.
	if _, present := unchanged.Settings["graph_enabled"]; present {
		t.Fatalf("graph_enabled was backfilled on a legacy row: %v", unchanged.Settings["graph_enabled"])
	}
	enabled, err = GraphEnabled(unchanged.Settings)
	if err != nil || enabled {
		t.Fatalf("GraphEnabled() after update = %v, %v", enabled, err)
	}

	updated, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values: map[string]any{
			"graph":         map[string]any{"base_url": "http://graph:8000/v1", "apikey": "graph-key"},
			"graph_model":   "graph-llm",
			"graph_enabled": true,
		},
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("UpdateSettings(graph) error: %v", err)
	}
	connection, err := ModelConnectionForKey(updated.Settings, "graph", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(graph) error: %v", err)
	}
	if connection.BaseURL != "http://graph:8000/v1" || connection.Model != "graph-llm" ||
		connection.APIKey != "graph-key" {
		t.Fatalf("graph connection = %+v", connection)
	}
	if updated.Settings["graph_enabled"] != true {
		t.Fatalf("graph_enabled = %v, want true", updated.Settings["graph_enabled"])
	}
	public := PublicSettings(updated.Settings)
	graph, ok := public["graph"].(map[string]any)
	if !ok || graph["base_url"] != "http://graph:8000/v1" || graph["api_key_set"] != true {
		t.Fatalf("public graph = %v", public["graph"])
	}
	if _, leaked := graph["protected_key"]; leaked {
		t.Fatalf("public graph leaked protected_key: %v", graph)
	}

	off, err := UpdateSettings(context.Background(), updated.Settings, UpdateInput{
		Values:        map[string]any{"graph_enabled": false},
		UpdatedAt:     fixedTimestamp(),
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	if err != nil {
		t.Fatalf("UpdateSettings(graph off) error: %v", err)
	}
	if off.Settings["graph_enabled"] != false {
		t.Fatalf("graph_enabled = %v, want false", off.Settings["graph_enabled"])
	}
	// Disabling the switch keeps the configured graph connection.
	kept, err := ModelConnectionForKey(off.Settings, "graph", testEncryptionKey)
	if err != nil {
		t.Fatalf("ModelConnectionForKey(graph) error: %v", err)
	}
	if kept.APIKey != "graph-key" {
		t.Fatalf("graph key = %q after disabling the switch", kept.APIKey)
	}
}

func TestUpdateSettingsRejectsNonBooleanGraphEnabled(t *testing.T) {
	current := testStoredSettings(t, "http://host", "private-key")
	_, err := UpdateSettings(context.Background(), current, UpdateInput{
		Values:        map[string]any{"graph_enabled": "yes"},
		EncryptionKey: testEncryptionKey,
	}, &fakeProber{vectorSize: 3})
	assertValidationMessage(t, err, MessageGraphEnabledInvalid)
}
