package catalog

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func boolPtr(value bool) *bool       { return &value }
func stringPtr(value string) *string { return &value }

func defaultToolSettings() ToolSettings {
	return ToolSettings{Enabled: boolPtr(true), Description: stringPtr("Search the knowledge base.")}
}

func newCatalog(t *testing.T) *NamespaceCatalog {
	t.Helper()
	catalog, err := NewNamespaceCatalog(NewMemoryStore(), "user-1")
	if err != nil {
		t.Fatalf("NewNamespaceCatalog() error = %v", err)
	}
	return catalog
}

func TestSnapshotIsEmptyByDefault(t *testing.T) {
	catalog := newCatalog(t)
	data, err := catalog.Snapshot(context.Background(), false)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	if len(data.Tools) != 0 || len(data.Prompts) != 0 {
		t.Fatalf("data = %+v", data)
	}
	encoded, err := Encode(data)
	if err != nil {
		t.Fatalf("Encode() error = %v", err)
	}
	if string(encoded) != "{\n  \"tools\": {},\n  \"prompts\": {}\n}" {
		t.Fatalf("encoded = %q", string(encoded))
	}
}

func TestPatchToolMergesDefaultsAndDetectsNoOp(t *testing.T) {
	catalog := newCatalog(t)
	ctx := context.Background()

	changed, err := catalog.PatchTool(ctx, "search", ToolPatch{Description: stringPtr("Custom description")}, defaultToolSettings())
	if err != nil {
		t.Fatalf("PatchTool() error = %v", err)
	}
	if !changed {
		t.Fatalf("changed = false, want true")
	}
	data, err := catalog.Snapshot(ctx, true)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	settings := data.Tools["search"]
	if settings.Enabled == nil || !*settings.Enabled {
		t.Fatalf("enabled = %v", settings.Enabled)
	}
	if settings.Description == nil || *settings.Description != "Custom description" {
		t.Fatalf("description = %v", settings.Description)
	}

	changed, err = catalog.PatchTool(ctx, "search", ToolPatch{Description: stringPtr("Custom description")}, defaultToolSettings())
	if err != nil {
		t.Fatalf("PatchTool() error = %v", err)
	}
	if changed {
		t.Fatalf("second identical patch reported a change")
	}

	changed, err = catalog.PatchTool(ctx, "search", ToolPatch{Enabled: boolPtr(false)}, defaultToolSettings())
	if err != nil {
		t.Fatalf("PatchTool() error = %v", err)
	}
	if !changed {
		t.Fatalf("disable patch reported no change")
	}
}

func TestPatchToolValidation(t *testing.T) {
	catalog := newCatalog(t)
	if _, err := catalog.PatchTool(context.Background(), "search", ToolPatch{}, defaultToolSettings()); err == nil {
		t.Fatalf("empty patch accepted")
	}
	longDescription := strings.Repeat("x", 8001)
	if _, err := catalog.PatchTool(context.Background(), "search", ToolPatch{Description: &longDescription}, defaultToolSettings()); err == nil {
		t.Fatalf("oversized description accepted")
	}
}

func promptTemplate() PromptDefinition {
	return PromptDefinition{
		Description: "Summary prompt",
		Arguments:   []PromptArgument{{Name: "topic", Required: true}},
		Messages:    []PromptMessage{{Role: "user", Text: "Summarize {{topic}}"}},
	}
}

func TestPromptLifecycleAndRender(t *testing.T) {
	catalog := newCatalog(t)
	ctx := context.Background()

	changed, err := catalog.PutPrompt(ctx, "summary", promptTemplate())
	if err != nil {
		t.Fatalf("PutPrompt() error = %v", err)
	}
	if !changed {
		t.Fatalf("PutPrompt() changed = false")
	}
	changed, err = catalog.PutPrompt(ctx, "summary", promptTemplate())
	if err != nil || changed {
		t.Fatalf("second PutPrompt() changed = %v, err = %v", changed, err)
	}

	data, err := catalog.Snapshot(ctx, false)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	rendered, err := data.Prompts["summary"].Render(map[string]string{"topic": "{{do_not_execute}}"})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if len(rendered) != 1 || rendered[0].Text != "Summarize {{do_not_execute}}" {
		t.Fatalf("rendered = %+v", rendered)
	}
	if _, err := data.Prompts["summary"].Render(nil); err == nil {
		t.Fatalf("missing required argument accepted")
	}
	if _, err := data.Prompts["summary"].Render(map[string]string{"other": "x"}); err == nil {
		t.Fatalf("unknown argument accepted")
	}

	if err := catalog.DeletePrompt(ctx, "summary"); err != nil {
		t.Fatalf("DeletePrompt() error = %v", err)
	}
	var missing *MissingPromptError
	if err := catalog.DeletePrompt(ctx, "summary"); err == nil || !strings.Contains(err.Error(), "missing prompt") {
		t.Fatalf("expected MissingPromptError, got %v", err)
	} else if !asMissing(err, &missing) {
		t.Fatalf("error type = %T", err)
	}
}

func asMissing(err error, target **MissingPromptError) bool {
	missing, ok := err.(*MissingPromptError)
	if ok {
		*target = missing
	}
	return ok
}

func TestPromptValidation(t *testing.T) {
	cases := []PromptDefinition{
		{Messages: nil},
		{Messages: []PromptMessage{{Role: "system", Text: "x"}}},
		{Messages: []PromptMessage{{Role: "user", Text: "{{undeclared}}"}}},
		{Messages: []PromptMessage{{Role: "user", Text: "{{broken"}}},
		{Messages: []PromptMessage{{Role: "user", Text: "x"}}, Arguments: []PromptArgument{{Name: "a"}, {Name: "a"}}},
		{Messages: []PromptMessage{{Role: "user", Text: "x"}}, Arguments: []PromptArgument{{Name: "bad name"}}},
	}
	for index, definition := range cases {
		if err := definition.Validate(); err == nil {
			t.Fatalf("case %d accepted invalid prompt", index)
		}
	}
	if err := promptTemplate().Validate(); err != nil {
		t.Fatalf("valid prompt rejected: %v", err)
	}
}

func TestDecodeRejectsInvalidPersistedCatalog(t *testing.T) {
	for _, raw := range []string{
		`not-json`,
		`{"tools":{"bad name":{"enabled":true}}}`,
		`{"tools":{"search":{}}}`,
	} {
		if _, err := Decode([]byte(raw)); err == nil {
			t.Fatalf("Decode(%q) accepted invalid catalog", raw)
		}
	}
	decoded, err := Decode([]byte(`{"tools":{"search":{"enabled":false}},"prompts":{}}`))
	if err != nil {
		t.Fatalf("Decode() error = %v", err)
	}
	settings := decoded.Tools["search"]
	if settings.Enabled == nil || *settings.Enabled {
		t.Fatalf("enabled = %v", settings.Enabled)
	}
	if _, err := json.Marshal(decoded); err != nil {
		t.Fatalf("marshal: %v", err)
	}
}
