package kbconfig

import "strings"

// OCREngines lists the OCR engine ids accepted by the Docling parse service
// (options.ocr.engine). "auto" resolves to the service default engine.
var OCREngines = []string{"auto", "easyocr", "tesseract", "rapidocr"}

// DefaultOCREngine is the engine id the Console preselects for a new OCR model
// configuration.
const DefaultOCREngine = "easyocr"

// IsOCREngine reports whether engine is exactly one of OCREngines.
func IsOCREngine(engine string) bool {
	for _, candidate := range OCREngines {
		if engine == candidate {
			return true
		}
	}
	return false
}

// NormalizeOCREngine trims and lowercases one OCR engine id. An empty value is
// accepted and means "not configured"; any other value must be a supported
// engine (an unsupported value is a Chinese validation error so the Console can
// display it directly).
func NormalizeOCREngine(engine string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(engine))
	if normalized == "" {
		return "", nil
	}
	if !IsOCREngine(normalized) {
		return "", NewValidationError(MessageOCREngineInvalid)
	}
	return normalized, nil
}
