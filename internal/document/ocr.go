package document

import "strings"

// OCRSettings configures the OCR engine passed to the remote Docling parse
// service for one document (the "options.ocr" object of the multipart options
// field). OCR always runs inside the parse service, so this carries an engine id
// only: no endpoint and no credential.
type OCRSettings struct {
	// Engine is the OCR engine id: auto, easyocr, tesseract or rapidocr.
	// Empty means "not configured" and the field is omitted from the request.
	Engine string
}

// Configured reports whether an OCR engine was selected.
func (o OCRSettings) Configured() bool {
	return strings.TrimSpace(o.Engine) != ""
}

// NormalizedEngine returns the engine id trimmed for the parse-service payload.
func (o OCRSettings) NormalizedEngine() string {
	return strings.TrimSpace(o.Engine)
}
