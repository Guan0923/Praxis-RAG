package chunking

import "testing"

func TestStrategyCatalog(t *testing.T) {
	catalog := StrategyCatalog()
	if catalog.Default != "recursive" {
		t.Fatalf("default = %q", catalog.Default)
	}
	if len(catalog.Strategies) != 5 {
		t.Fatalf("strategies = %d", len(catalog.Strategies))
	}
	ids := []string{"character", "token", "sentence", "recursive", "structure"}
	for index, expected := range ids {
		if catalog.Strategies[index].ID != expected {
			t.Fatalf("strategy[%d] = %q, want %q", index, catalog.Strategies[index].ID, expected)
		}
	}
	if catalog.Strategies[0].ChunkSize != 700 || catalog.Strategies[0].ChunkOverlap != 100 {
		t.Fatalf("character defaults = %+v", catalog.Strategies[0])
	}
	if catalog.Strategies[0].Label != "按字符" || catalog.Strategies[0].Unit != "characters" {
		t.Fatalf("character labels = %+v", catalog.Strategies[0])
	}
}

func TestNewConfigValidation(t *testing.T) {
	if config, err := NewConfig("recursive", 512, 64); err != nil || config.Strategy != "recursive" {
		t.Fatalf("NewConfig() = %+v, %v", config, err)
	}
	cases := []struct {
		strategy string
		size     int
		overlap  int
		message  string
	}{
		{"unknown", 512, 64, "未知切分策略"},
		{"token", 0, 0, "块长度必须是 1 到 100000 的整数"},
		{"token", 100_001, 0, "块长度必须是 1 到 100000 的整数"},
		{"token", 512, 512, "重叠长度必须是非负整数且小于块长度"},
		{"token", 512, -1, "重叠长度必须是非负整数且小于块长度"},
		{"character", 99, 10, "字符切分的块长度至少为 100"},
	}
	for _, testCase := range cases {
		_, err := NewConfig(testCase.strategy, testCase.size, testCase.overlap)
		if err == nil || err.Error() != testCase.message {
			t.Fatalf("NewConfig(%q, %d, %d) error = %v, want %q",
				testCase.strategy, testCase.size, testCase.overlap, err, testCase.message)
		}
	}
}

func TestConfigFromRequestDefaults(t *testing.T) {
	config, err := ConfigFromRequest("recursive", nil, nil)
	if err != nil {
		t.Fatalf("ConfigFromRequest() error = %v", err)
	}
	if config.ChunkSize != 512 || config.ChunkOverlap != 64 {
		t.Fatalf("recursive defaults = %+v", config)
	}
	size := 900
	overlap := 120
	config, err = ConfigFromRequest("character", &size, &overlap)
	if err != nil {
		t.Fatalf("ConfigFromRequest() error = %v", err)
	}
	if config.ChunkSize != 900 || config.ChunkOverlap != 120 {
		t.Fatalf("explicit character config = %+v", config)
	}
	if _, err := ConfigFromRequest("unknown", nil, nil); err == nil {
		t.Fatalf("unknown strategy accepted")
	}
}

func TestConfigMetadata(t *testing.T) {
	config, err := NewConfig("token", 256, 32)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	metadata := config.Metadata("BAAI/bge-m3")
	if metadata["strategy"] != "token" || metadata["chunk_size"] != 256 || metadata["tokenizer"] != "BAAI/bge-m3" {
		t.Fatalf("metadata = %v", metadata)
	}
}
