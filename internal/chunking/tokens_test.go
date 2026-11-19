package chunking

import (
	"reflect"
	"testing"
	"unicode/utf8"
)

// runeCounter is the fixed counter used by the translated chunking tests: one
// token per rune. It makes token-budget assertions predictable without a
// tokenizer model, exactly like the fake/fixed tokenizer the task asks for.
type runeCounter struct{}

func (runeCounter) Count(text string) int { return utf8.RuneCountInString(text) }

func (runeCounter) TokenStartOffsets(text string) []int {
	runes := []rune(text)
	offsets := make([]int, len(runes))
	for index := range offsets {
		offsets[index] = index
	}
	return offsets
}

func (runeCounter) ModelMaxLength() int     { return 8192 }
func (runeCounter) SpecialTokensToAdd() int { return 0 }

// plainRuneCounter counts runes but does not expose token offsets, exercising
// TokenBudget's scanning fallback.
type plainRuneCounter struct{}

func (plainRuneCounter) Count(text string) int { return utf8.RuneCountInString(text) }
func (plainRuneCounter) ModelMaxLength() int   { return 8192 }
func (plainRuneCounter) SpecialTokensToAdd() int {
	return 0
}

// fixedCounter returns the same count for every text.
type fixedCounter struct{ value int }

func (f fixedCounter) Count(string) int { return f.value }

// limitedCounter exposes explicit model limits.
type limitedCounter struct {
	maxLength int
	special   int
}

func (limitedCounter) Count(text string) int { return utf8.RuneCountInString(text) }
func (c limitedCounter) ModelMaxLength() int { return c.maxLength }
func (c limitedCounter) SpecialTokensToAdd() int {
	return c.special
}

func TestDefaultCounterDeterministicApproximation(t *testing.T) {
	counter := DefaultCounter
	cases := []struct {
		text string
		want int
	}{
		{"", 0},
		{"中文句子", 4},
		{"hello world", 4}, // two 5-rune words -> 4 subword pieces
		{"a b c", 3},       // whitespace is not counted
		{"索引 v2", 3},       // 2 CJK runes + one 2-rune word piece
		{"。！？", 3},         // punctuation runes are separate tokens
		{"\n\n\t ", 0},     // whitespace only
	}
	for _, testCase := range cases {
		if got := counter.Count(testCase.text); got != testCase.want {
			t.Fatalf("Count(%q) = %d, want %d", testCase.text, got, testCase.want)
		}
	}
	first := counter.Count("混合 Mixed 文本 👩‍💻")
	second := counter.Count("混合 Mixed 文本 👩‍💻")
	if first != second {
		t.Fatalf("counter not deterministic: %d != %d", first, second)
	}
}

func TestDefaultCounterOffsetsInvariants(t *testing.T) {
	offsetter, ok := DefaultCounter.(TokenOffsetter)
	if !ok {
		t.Fatalf("DefaultCounter does not implement TokenOffsetter")
	}
	text := "索引 v2 版本 code"
	offsets := offsetter.TokenStartOffsets(text)
	if len(offsets) != DefaultCounter.Count(text) {
		t.Fatalf("offsets = %d, count = %d", len(offsets), DefaultCounter.Count(text))
	}
	if len(offsets) == 0 || offsets[0] != 0 {
		t.Fatalf("offsets = %v", offsets)
	}
	for index := 1; index < len(offsets); index++ {
		if offsets[index] <= offsets[index-1] {
			t.Fatalf("offsets not increasing: %v", offsets)
		}
		if offsets[index] >= utf8.RuneCountInString(text) {
			t.Fatalf("offset out of range: %v", offsets)
		}
	}
}

func TestDefaultCounterModelLimits(t *testing.T) {
	limits, ok := DefaultCounter.(ModelLimits)
	if !ok {
		t.Fatalf("DefaultCounter does not implement ModelLimits")
	}
	if limits.ModelMaxLength() != 8192 || limits.SpecialTokensToAdd() != 2 {
		t.Fatalf("limits = %d/%d", limits.ModelMaxLength(), limits.SpecialTokensToAdd())
	}
	config, err := NewConfig("recursive", 512, 64)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	budget, err := NewTokenBudget(DefaultCounter, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	if budget.Maximum() != 8190 {
		t.Fatalf("maximum = %d, want 8190", budget.Maximum())
	}
}

func TestTokenBudgetModelLimitError(t *testing.T) {
	config, err := NewConfig("recursive", 8192, 64)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	_, err = NewTokenBudget(DefaultCounter, config)
	if err == nil || err.Error() != "块长度不能超过模型允许的 8190 token" {
		t.Fatalf("error = %v", err)
	}
	limited := limitedCounter{maxLength: 1024, special: 1}
	config, err = NewConfig("token", 1024, 64)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	_, err = NewTokenBudget(limited, config)
	if err == nil || err.Error() != "块长度不能超过模型允许的 1023 token" {
		t.Fatalf("error = %v", err)
	}
	config, err = NewConfig("token", 1023, 64)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	if _, err := NewTokenBudget(limited, config); err != nil {
		t.Fatalf("boundary size rejected: %v", err)
	}
}

func TestScannedOffsetsMatchOffsetter(t *testing.T) {
	text := "测试设备工作正常。温度超过上限时"
	expected := runeCounter{}.TokenStartOffsets(text)
	for _, counter := range []TokenCounter{runeCounter{}, plainRuneCounter{}, DefaultCounter} {
		if got := scannedTokenOffsets(counter, text); !reflect.DeepEqual(got, expected) {
			t.Fatalf("%T scanned offsets = %v, want %v", counter, got, expected)
		}
	}
}

func TestWindowsWithAndWithoutOffsetsAgree(t *testing.T) {
	text := "abcdefghijklmnopqrstuvwxyz"
	config, err := NewConfig("token", 5, 1)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	withOffsets, err := NewTokenBudget(runeCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	withoutOffsets, err := NewTokenBudget(plainRuneCounter{}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	runes := []rune(text)
	expected, err := withOffsets.windows(runes, 0, len(runes))
	if err != nil {
		t.Fatalf("windows() error = %v", err)
	}
	got, err := withoutOffsets.windows(runes, 0, len(runes))
	if err != nil {
		t.Fatalf("windows() error = %v", err)
	}
	if !reflect.DeepEqual(got, expected) {
		t.Fatalf("fallback windows = %v, want %v", got, expected)
	}
}

func TestWindowsFailsWhenChunkSizeCannotHoldOneCharacter(t *testing.T) {
	config, err := NewConfig("token", 1, 0)
	if err != nil {
		t.Fatalf("NewConfig() error = %v", err)
	}
	budget, err := NewTokenBudget(fixedCounter{value: 100}, config)
	if err != nil {
		t.Fatalf("NewTokenBudget() error = %v", err)
	}
	_, err = budget.windows([]rune("abc"), 0, 3)
	if err == nil || err.Error() != "块长度过小，无法容纳一个完整字符" {
		t.Fatalf("error = %v", err)
	}
}
