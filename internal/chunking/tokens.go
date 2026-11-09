package chunking

import (
	"unicode"
	"unicode/utf8"
)

// TokenCounter replaces transformers AutoTokenizer in the Go port.
//
// Python loaded BAAI/bge-m3 through AutoTokenizer and split text with real
// SentencePiece token offsets. The Go port keeps the same splitting algorithms
// but makes the counter injectable:
//
//   - TokenCounter is the only required method.
//   - TokenOffsetter optionally exposes token start offsets and is used by the
//     overlap/window logic to reproduce Python's offset_mapping behaviour. When
//     absent, offsets are derived by scanning prefix counts (slower but with the
//     same split semantics).
//   - ModelLimits optionally exposes model_max_length and the number of special
//     tokens, used to bound chunk_size exactly like TokenBudget's constructor.
//
// Parity gap: DefaultCounter is a deterministic approximation, not a
// SentencePiece implementation, so token counts and therefore chunk boundaries
// differ from Python's for the same text. Sentence, paragraph and structure
// boundaries are character based and remain identical; only token-budget
// decisions differ. A future round should either port a BGE-M3 tokenizer to Go
// or run the tokenizer as an external process and inject it here.
type TokenCounter interface {
	// Count returns the token count of text excluding special tokens.
	Count(text string) int
}

// TokenOffsetter reports, for each token of text, the rune offset at which the
// token starts (tokenizer offset_mapping[i][0]). The returned slice must have
// exactly Count(text) entries.
type TokenOffsetter interface {
	TokenStartOffsets(text string) []int
}

// ModelLimits reports tokenizer limits used by TokenBudget.
type ModelLimits interface {
	// ModelMaxLength is tokenizer.model_max_length.
	ModelMaxLength() int
	// SpecialTokensToAdd is tokenizer.num_special_tokens_to_add(pair=False).
	SpecialTokensToAdd() int
}

const (
	// defaultModelMaxLength and defaultSpecialTokens are BGE-M3 defaults used
	// for counters that do not expose ModelLimits.
	defaultModelMaxLength = 8192
	defaultSpecialTokens  = 2
	// bgeMaxInput mirrors TokenBudget's min(model_max_length, 8192) cap.
	bgeMaxInput = 8192
	// subwordRunes approximates SentencePiece subword granularity for the
	// default counter.
	subwordRunes = 4
)

// DefaultCounter is the deterministic default TokenCounter. It approximates
// BGE-M3 SentencePiece counting without any model files:
//
//   - each CJK ideograph, kana or hangul rune counts as one token;
//   - a word (letters/digits/underscore/combining marks) is split into
//     subwordRunes-sized pieces, at least one token per word;
//   - every other rune (punctuation, symbols, emoji) counts as one token;
//   - whitespace separates tokens and is not counted.
//
// It implements TokenOffsetter and ModelLimits. See the TokenCounter parity gap
// note: counts are deterministic but deliberately not equal to BAAI/bge-m3.
var DefaultCounter TokenCounter = defaultTokenCounter{}

type defaultTokenCounter struct{}

// Count implements TokenCounter.
func (defaultTokenCounter) Count(text string) int {
	count, _ := countDefaultTokens(text)
	return count
}

// TokenStartOffsets implements TokenOffsetter.
func (defaultTokenCounter) TokenStartOffsets(text string) []int {
	_, offsets := countDefaultTokens(text)
	return offsets
}

// ModelMaxLength implements ModelLimits.
func (defaultTokenCounter) ModelMaxLength() int { return bgeMaxInput }

// SpecialTokensToAdd implements ModelLimits.
func (defaultTokenCounter) SpecialTokensToAdd() int { return defaultSpecialTokens }

// countDefaultTokens returns the token count and the rune offset of each token
// start.
func countDefaultTokens(text string) (int, []int) {
	if text == "" {
		return 0, nil
	}
	offsets := make([]int, 0, utf8.RuneCountInString(text)/2+1)
	position := 0
	wordStart := -1
	wordLength := 0
	flushWord := func() {
		if wordLength == 0 {
			return
		}
		for piece := 0; piece < wordLength; piece += subwordRunes {
			offsets = append(offsets, wordStart+piece)
		}
		wordStart = -1
		wordLength = 0
	}
	for _, value := range text {
		switch {
		case unicode.IsSpace(value) || value == '\u200b' || value == '\ufeff':
			flushWord()
		case isCJKRune(value):
			flushWord()
			offsets = append(offsets, position)
		case isWordRune(value):
			if wordStart < 0 {
				wordStart = position
			}
			wordLength++
		default:
			flushWord()
			offsets = append(offsets, position)
		}
		position++
	}
	flushWord()
	return len(offsets), offsets
}

// isCJKRune reports whether a rune counts as one token per character, matching
// the granularity of BGE-M3's SentencePiece vocabulary for CJK scripts.
func isCJKRune(value rune) bool {
	switch {
	case value >= 0x3400 && value <= 0x4DBF: // CJK unified ideographs extension A
		return true
	case value >= 0x4E00 && value <= 0x9FFF: // CJK unified ideographs
		return true
	case value >= 0xF900 && value <= 0xFAFF: // CJK compatibility ideographs
		return true
	case value >= 0x3040 && value <= 0x309F: // hiragana
		return true
	case value >= 0x30A0 && value <= 0x30FF: // katakana
		return true
	case value >= 0xAC00 && value <= 0xD7AF: // hangul syllables
		return true
	case value >= 0x1100 && value <= 0x11FF: // hangul jamo
		return true
	case value >= 0x20000 && value <= 0x2FA1F: // CJK extensions B-F and compatibility supplement
		return true
	case value >= 0xFF66 && value <= 0xFF9D: // halfwidth katakana
		return true
	}
	return false
}

// isWordRune reports whether a rune continues a word for the default counter.
func isWordRune(value rune) bool {
	return value == '_' || unicode.IsLetter(value) || unicode.IsDigit(value) ||
		unicode.Is(unicode.Nl, value) || unicode.Is(unicode.No, value) ||
		unicode.Is(unicode.Mn, value) || unicode.Is(unicode.Mc, value)
}

// tokenMaximum mirrors TokenBudget.__init__'s
// min(model_max_length, 8192) - num_special_tokens_to_add.
func tokenMaximum(counter TokenCounter) int {
	maxLength := defaultModelMaxLength
	special := defaultSpecialTokens
	if limits, ok := counter.(ModelLimits); ok {
		if value := limits.ModelMaxLength(); value > 0 {
			maxLength = value
		}
		if value := limits.SpecialTokensToAdd(); value >= 0 {
			special = value
		}
	}
	if maxLength > bgeMaxInput {
		maxLength = bgeMaxInput
	}
	return maxLength - special
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}
