package state

import (
	"context"
	"encoding/json"
	"testing"
)

// TestStripLegacyOCRKeys pins the pure rewrite step of migrateVisionModelConfig:
// legacy "ocr"/"ocr_model" keys are removed, everything else is preserved, and
// invalid or already-clean documents are returned unchanged.
func TestStripLegacyOCRKeys(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		want    string
		changed bool
	}{
		{
			name:    "both legacy keys",
			input:   `{"namespace":"alpha","ocr":{"base_url":"http://legacy"},"ocr_model":"easyocr","vlm_model":"qwen"}`,
			want:    `{"namespace":"alpha","vlm_model":"qwen"}`,
			changed: true,
		},
		{
			name:    "ocr model only",
			input:   `{"namespace":"alpha","ocr_model":"tesseract","chunk_size":120}`,
			want:    `{"chunk_size":120,"namespace":"alpha"}`,
			changed: true,
		},
		{
			name:    "already clean",
			input:   `{"namespace":"alpha","vlm_model":"qwen"}`,
			changed: false,
		},
		{
			name:    "invalid json is untouched",
			input:   `{"namespace":`,
			changed: false,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got, changed := stripLegacyOCRKeys(testCase.input)
			if changed != testCase.changed {
				t.Fatalf("changed = %v, want %v", changed, testCase.changed)
			}
			if !changed {
				if got != testCase.input {
					t.Fatalf("unchanged result = %q, want %q", got, testCase.input)
				}
				return
			}
			var decoded map[string]any
			if err := json.Unmarshal([]byte(got), &decoded); err != nil {
				t.Fatalf("rewritten settings are not valid JSON: %v (%q)", err, got)
			}
			if _, present := decoded["ocr"]; present {
				t.Fatalf("ocr key survived: %q", got)
			}
			if _, present := decoded["ocr_model"]; present {
				t.Fatalf("ocr_model key survived: %q", got)
			}
			var want map[string]any
			if err := json.Unmarshal([]byte(testCase.want), &want); err != nil {
				t.Fatalf("invalid want JSON: %v", err)
			}
			gotEncoded, _ := json.Marshal(decoded)
			wantEncoded, _ := json.Marshal(want)
			if string(gotEncoded) != string(wantEncoded) {
				t.Fatalf("rewritten settings = %s, want %s", gotEncoded, wantEncoded)
			}
			// Idempotent: a rewritten document is already clean.
			if _, again := stripLegacyOCRKeys(got); again {
				t.Fatalf("second rewrite reported a change")
			}
		})
	}
}

// TestMigrateVisionModelConfigRemovesLegacyOCRConfiguration pins the startup
// migration: saved "ocr" model profiles are deleted, legacy ocr/ocr_model
// knowledge-base settings keys are stripped while every other key survives, and
// a second run is a no-op. It needs the opt-in MySQL test database.
func TestMigrateVisionModelConfigRemovesLegacyOCRConfiguration(t *testing.T) {
	_ = mysqlTestURL(t)
	stores := openMySQLTestStores(t)
	ctx := context.Background()

	if _, err := stores.ModelProfiles.Create(ctx, ModelProfile{
		ID: "ocr-profile-1", Kind: "ocr", Name: "easyocr", Model: "easyocr",
	}); err != nil {
		t.Fatalf("ModelProfiles.Create(ocr) error = %v", err)
	}
	if _, err := stores.ModelProfiles.Create(ctx, ModelProfile{
		ID: "vlm-profile-1", Kind: "vlm", Name: "qwen", BaseURL: "http://vlm:8000/v1",
		Model: "qwen2.5-vl-7b",
	}); err != nil {
		t.Fatalf("ModelProfiles.Create(vlm) error = %v", err)
	}
	legacy := map[string]any{
		"namespace": "vision-legacy", "collection": "knowledge_legacy", "vector_size": 3,
		"embedding_model": "embed-a", "vlm_model": "qwen2.5-vl-7b",
		"ocr": map[string]any{"base_url": "http://legacy.local"}, "ocr_model": "easyocr",
	}
	if err := stores.KnowledgeBases.Create(ctx, legacy); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}

	if err := stores.migrateVisionModelConfig(ctx); err != nil {
		t.Fatalf("migrateVisionModelConfig() error = %v", err)
	}

	profiles, err := stores.ModelProfiles.List(ctx)
	if err != nil {
		t.Fatalf("ModelProfiles.List() error = %v", err)
	}
	if len(profiles) != 1 || profiles[0].ID != "vlm-profile-1" {
		t.Fatalf("profiles after migration = %+v", profiles)
	}
	settings, err := stores.KnowledgeBases.Get(ctx, "vision-legacy")
	if err != nil {
		t.Fatalf("KnowledgeBases.Get() error = %v", err)
	}
	if settings == nil {
		t.Fatal("knowledge base disappeared during migration")
	}
	for _, key := range []string{"ocr", "ocr_model"} {
		if _, present := settings[key]; present {
			t.Fatalf("%s survived the migration: %v", key, settings[key])
		}
	}
	if settings["vlm_model"] != "qwen2.5-vl-7b" || settings["embedding_model"] != "embed-a" {
		t.Fatalf("unrelated settings were changed: %v", settings)
	}

	// The migration is wired into ensureSchema, so a second startup is a no-op.
	if err := stores.migrateVisionModelConfig(ctx); err != nil {
		t.Fatalf("second migrateVisionModelConfig() error = %v", err)
	}
	profiles, err = stores.ModelProfiles.List(ctx)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("profiles after the second run = %+v (%v)", profiles, err)
	}
}
