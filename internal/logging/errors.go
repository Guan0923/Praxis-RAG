package logging

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// maxSafeDetailRunes bounds one error detail inside a log line or a persisted
// job error message.
const maxSafeDetailRunes = 400

// The optional interfaces domain error types implement so this package can
// classify them without importing the project packages (which would create an
// import cycle through state/runtime).
type (
	categorizer interface{ ErrorCategory() string }
	statusCoder interface{ HTTPStatus() int }
	stageNamer  interface{ FailureStage() string }
)

// stageError annotates a failure with the pipeline stage that produced it.
type stageError struct {
	stage string
	err   error
}

// Error implements error. The stage is reported through FailureStage instead of
// changing the message callers match on.
func (e *stageError) Error() string { return e.err.Error() }

// Unwrap keeps the original error observable to errors.Is/errors.As.
func (e *stageError) Unwrap() error { return e.err }

// FailureStage implements stageNamer.
func (e *stageError) FailureStage() string { return e.stage }

// WrapStage annotates err with the pipeline stage (for example "parsing",
// "embedding", "indexing", "publishing"). The innermost annotated stage wins:
// wrapping an error that already carries a stage returns it unchanged.
func WrapStage(stage string, err error) error {
	if err == nil {
		return nil
	}
	var existing stageNamer
	if errors.As(err, &existing) && existing.FailureStage() != "" {
		return err
	}
	return &stageError{stage: stage, err: err}
}

// ErrorCategory returns a stable, low-cardinality category for logs and job
// error text. Domain errors implement ErrorCategory; unclassified errors fall
// back to their Go type name so nothing is silently lost.
func ErrorCategory(err error) string {
	if err == nil {
		return ""
	}
	var categorized categorizer
	if errors.As(err, &categorized) {
		if category := strings.TrimSpace(categorized.ErrorCategory()); category != "" {
			return category
		}
	}
	return goTypeName(err)
}

// HTTPStatus returns the upstream HTTP status carried by the error, or 0 when
// the failure did not reach a remote service or carries no status.
func HTTPStatus(err error) int {
	if err == nil {
		return 0
	}
	var coder statusCoder
	if errors.As(err, &coder) {
		if status := coder.HTTPStatus(); status > 0 {
			return status
		}
	}
	return 0
}

// ErrorStage returns the pipeline stage annotated by WrapStage, or empty.
func ErrorStage(err error) string {
	if err == nil {
		return ""
	}
	var staged stageNamer
	if errors.As(err, &staged) {
		return strings.TrimSpace(staged.FailureStage())
	}
	return ""
}

// ErrorSummary renders the persisted, operator-facing one-line reason:
// "category at stage (HTTP n): safe detail". It never contains credentials or
// document text because the detail goes through Redact.
func ErrorSummary(err error) string {
	if err == nil {
		return ""
	}
	summary := ErrorCategory(err)
	if summary == "" {
		return ""
	}
	if stage := ErrorStage(err); stage != "" {
		summary += " at " + stage
	}
	if status := HTTPStatus(err); status > 0 {
		summary += fmt.Sprintf(" (HTTP %d)", status)
	}
	detail := SafeDetail(err)
	if detail != "" && detail != summary {
		summary += ": " + detail
	}
	return summary
}

// ErrorAttrs returns slog key/value pairs for a structured warning line:
// error_category, error_stage, error_status and error_detail (only when known).
func ErrorAttrs(err error) []any {
	category := ErrorCategory(err)
	if category == "" {
		return nil
	}
	attrs := []any{"error_category", category}
	if stage := ErrorStage(err); stage != "" {
		attrs = append(attrs, "error_stage", stage)
	}
	if status := HTTPStatus(err); status > 0 {
		attrs = append(attrs, "error_status", status)
	}
	if detail := SafeDetail(err); detail != "" {
		attrs = append(attrs, "error_detail", detail)
	}
	return attrs
}

// SafeDetail returns the redacted, bounded error message. Use it for any log or
// persisted field that must show "what went wrong" without echoing secrets or
// document content.
func SafeDetail(err error) string {
	if err == nil {
		return ""
	}
	return RedactLimit(err.Error(), maxSafeDetailRunes)
}

// Redact removes credential-shaped substrings and collapses the text to one
// line. It is a defense-in-depth filter for diagnostic text produced elsewhere;
// callers must still avoid passing document content, queries or message bodies.
func Redact(text string) string {
	return RedactLimit(text, maxSafeDetailRunes)
}

// RedactLimit is Redact with an explicit rune budget.
func RedactLimit(text string, maximum int) string {
	if text == "" {
		return ""
	}
	sanitized := sanitizeWhitespace(text)
	sanitized = urlUserInfoPattern.ReplaceAllString(sanitized, "${1}***@")
	sanitized = encryptedValuePattern.ReplaceAllString(sanitized, "enc:v1:***")
	sanitized = openAIKeyPattern.ReplaceAllString(sanitized, "sk-***")
	sanitized = authorizationHeaderPattern.ReplaceAllString(sanitized, "${1}=***")
	sanitized = bearerPattern.ReplaceAllString(sanitized, "Bearer ***")
	sanitized = secretAssignmentPattern.ReplaceAllString(sanitized, "${1}=***")
	sanitized = querySecretPattern.ReplaceAllString(sanitized, "${1}***")
	return truncateRunes(sanitized, maximum)
}

var (
	// scheme://user:password@host -> scheme://***@host
	urlUserInfoPattern = regexp.MustCompile(`([A-Za-z][A-Za-z0-9+.\-]*://)[^/\s@]+@`)
	// enc:v1:<ciphertext> -> enc:v1:***
	encryptedValuePattern = regexp.MustCompile(`enc:v1:[A-Za-z0-9+/=_\-]+`)
	// sk-<key> -> sk-***
	openAIKeyPattern = regexp.MustCompile(`\bsk-[A-Za-z0-9_\-]{8,}`)
	// Authorization: Bearer <token> (or Basic/Digest) -> Authorization=***
	authorizationHeaderPattern = regexp.MustCompile(
		`(?i)\b(authorization)\b\s*[:=]\s*(?:(?:bearer|basic|digest)\s+)?[^\s,;]+`)
	// bare Bearer <token> -> Bearer ***
	bearerPattern = regexp.MustCompile(`(?i)\b(Bearer)\s+[A-Za-z0-9._~+/=\-]{4,}`)
	// key=value / key: value for credential-shaped names
	secretAssignmentPattern = regexp.MustCompile(
		`(?i)\b(api[_-]?key|apikey|access[_-]?token|refresh[_-]?token|token|password|passwd|secret|client[_-]?secret|credential)\b\s*[:=]\s*("[^"]*"|'[^']*'|[^\s,;]+)`)
	// ?api_key=value and friends in URLs
	querySecretPattern = regexp.MustCompile(
		`(?i)([?&](?:api[_-]?key|key|token|secret|password|signature|sig)=)[^&\s]+`)
)

// sanitizeWhitespace turns multi-line diagnostics into one bounded line and
// drops control characters that could forge log records.
func sanitizeWhitespace(text string) string {
	var builder strings.Builder
	builder.Grow(len(text))
	for _, character := range text {
		switch {
		case character == '\r' || character == '\n' || character == '\t':
			builder.WriteByte(' ')
		case character < 0x20 || character == 0x7f:
			builder.WriteByte(' ')
		default:
			builder.WriteRune(character)
		}
	}
	return strings.Join(strings.Fields(builder.String()), " ")
}

// truncateRunes bounds the text by runes, marking truncation explicitly.
func truncateRunes(text string, maximum int) string {
	if maximum <= 0 || utf8.RuneCountInString(text) <= maximum {
		return text
	}
	runes := []rune(text)
	return string(runes[:maximum]) + "..."
}

// goTypeName renders an unclassified error's type without the pointer marker,
// for example "apierrors.KnowledgeError". Diagnostic wrappers (stage annotations
// and fmt %w chains) are peeled off so the type name stays meaningful.
func goTypeName(err error) string {
	target := err
	for depth := 0; depth < 16; depth++ {
		var staged *stageError
		if errors.As(target, &staged) && staged.err != nil {
			target = staged.err
			continue
		}
		unwrapped := errors.Unwrap(target)
		if unwrapped == nil {
			break
		}
		target = unwrapped
	}
	name := fmt.Sprintf("%T", target)
	name = strings.TrimPrefix(name, "*")
	if name == "" || name == "<nil>" {
		return "error"
	}
	return name
}
