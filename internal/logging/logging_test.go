package logging

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

func TestOptionsFromEnvDefaults(t *testing.T) {
	options, err := optionsFromGetenv(func(string) string { return "" })
	if err != nil {
		t.Fatalf("optionsFromGetenv() error = %v", err)
	}
	if options.Level != slog.LevelInfo || options.Format != FormatText {
		t.Fatalf("optionsFromGetenv() = %+v, want info/text", options)
	}
}

func TestOptionsFromEnvDebugTraceCompatibility(t *testing.T) {
	values := map[string]string{DebugTraceEnvVar: "true"}
	options, err := optionsFromGetenv(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("optionsFromGetenv() error = %v", err)
	}
	if options.Level != slog.LevelDebug {
		t.Fatalf("level = %v, want debug from %s", options.Level, DebugTraceEnvVar)
	}
}

func TestOptionsFromEnvExplicitValues(t *testing.T) {
	values := map[string]string{LevelEnvVar: "WARN", FormatEnvVar: "JSON"}
	options, err := optionsFromGetenv(func(name string) string { return values[name] })
	if err != nil {
		t.Fatalf("optionsFromGetenv() error = %v", err)
	}
	if options.Level != slog.LevelWarn || options.Format != FormatJSON {
		t.Fatalf("optionsFromGetenv() = %+v, want warn/json", options)
	}
}

func TestOptionsFromEnvRejectsUnknownValues(t *testing.T) {
	for name, values := range map[string]map[string]string{
		"level":  {LevelEnvVar: "verbose"},
		"format": {FormatEnvVar: "yaml"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := optionsFromGetenv(func(key string) string { return values[key] }); err == nil {
				t.Fatalf("optionsFromGetenv(%v) = nil error, want configuration error", values)
			}
		})
	}
}

func TestNewWithWriterHonoursLevel(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewWithWriter(&buffer, Options{Level: slog.LevelWarn, Format: FormatText})
	logger.Info("hidden")
	logger.Warn("visible")
	output := buffer.String()
	if strings.Contains(output, "hidden") {
		t.Fatalf("info line logged below warn level: %q", output)
	}
	if !strings.Contains(output, "visible") {
		t.Fatalf("warn line missing from %q", output)
	}
}

func TestJSONFormat(t *testing.T) {
	var buffer bytes.Buffer
	logger := NewWithWriter(&buffer, Options{Level: slog.LevelInfo, Format: FormatJSON})
	logger.Info("event", "key", "value")
	output := buffer.String()
	if !strings.HasPrefix(output, "{") || !strings.Contains(output, `"msg":"event"`) {
		t.Fatalf("json handler output = %q", output)
	}
}

func TestContextAttributesFlowIntoLogger(t *testing.T) {
	var buffer bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(NewWithWriter(&buffer, Options{Level: slog.LevelInfo, Format: FormatText}))
	t.Cleanup(func() { slog.SetDefault(previous) })

	ctx := With(context.Background(), "artifact_id", "artifact-1")
	ctx = With(ctx, "operation_id", "operation-1")
	Annotate(ctx, "request_id", "request-1")
	FromContext(ctx).Info("processed")

	output := buffer.String()
	for _, want := range []string{"artifact-1", "operation-1", "request-1"} {
		if !strings.Contains(output, want) {
			t.Fatalf("log line %q missing %q", output, want)
		}
	}
	if got := CorrelationID(ctx); got != "request-1" {
		t.Fatalf("CorrelationID() = %q, want request-1", got)
	}
}

func TestWithDoesNotMutateParent(t *testing.T) {
	parent := With(context.Background(), "artifact_id", "a")
	child := With(parent, "artifact_id", "b")
	if value := Value(parent, FieldArtifactID); value != "a" {
		t.Fatalf("parent artifact_id = %q, want a", value)
	}
	if value := Value(child, FieldArtifactID); value != "b" {
		t.Fatalf("child artifact_id = %q, want b", value)
	}
}

func TestAnnotateReplacesExistingField(t *testing.T) {
	ctx := With(context.Background(), "operation_id", "first")
	Annotate(ctx, "operation_id", "second")
	if value := Value(ctx, FieldOperationID); value != "second" {
		t.Fatalf("operation_id = %q, want second", value)
	}
	if fields := Fields(ctx); len(fields) != 1 {
		t.Fatalf("fields = %v, want one merged field", fields)
	}
}

func TestAnnotateWithoutStateIsNoop(t *testing.T) {
	Annotate(context.Background(), "request_id", "ignored")
	if fields := Fields(context.Background()); len(fields) != 0 {
		t.Fatalf("background context gained fields: %v", fields)
	}
}

type categorizedError struct {
	category string
	status   int
}

func (e *categorizedError) Error() string         { return "upstream failure" }
func (e *categorizedError) ErrorCategory() string { return e.category }
func (e *categorizedError) HTTPStatus() int       { return e.status }

func TestErrorClassification(t *testing.T) {
	err := WrapStage("embedding", &categorizedError{category: "model_api_error", status: 429})
	if got := ErrorCategory(err); got != "model_api_error" {
		t.Fatalf("ErrorCategory() = %q", got)
	}
	if got := ErrorStage(err); got != "embedding" {
		t.Fatalf("ErrorStage() = %q", got)
	}
	if got := HTTPStatus(err); got != 429 {
		t.Fatalf("HTTPStatus() = %d", got)
	}
	summary := ErrorSummary(err)
	for _, want := range []string{"model_api_error at embedding", "HTTP 429", "upstream failure"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("ErrorSummary() = %q, missing %q", summary, want)
		}
	}
	attrs := ErrorAttrs(err)
	joined := fmt.Sprint(attrs...)
	for _, want := range []string{"error_category", "model_api_error", "error_stage", "embedding", "error_status", "429"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("ErrorAttrs() = %v, missing %q", attrs, want)
		}
	}
}

func TestWrapStageKeepsInnermostStage(t *testing.T) {
	inner := WrapStage("parsing", errors.New("parse failed"))
	outer := WrapStage("publishing", inner)
	if got := ErrorStage(outer); got != "parsing" {
		t.Fatalf("ErrorStage() = %q, want parsing", got)
	}
	if !errors.Is(outer, inner) {
		t.Fatal("wrapped error lost its identity")
	}
}

func TestUnknownErrorFallsBackToTypeName(t *testing.T) {
	if got := ErrorCategory(errors.New("plain")); got != "errors.errorString" {
		t.Fatalf("ErrorCategory() = %q, want errors.errorString", got)
	}
	wrapped := WrapStage("indexing", fmt.Errorf("store: %w", errors.New("boom")))
	if got := ErrorCategory(wrapped); got != "errors.errorString" {
		t.Fatalf("ErrorCategory(wrapped) = %q", got)
	}
	summary := ErrorSummary(wrapped)
	if !strings.Contains(summary, "at indexing") {
		t.Fatalf("ErrorSummary() = %q", summary)
	}
}

func TestErrorHelpersTolerateNil(t *testing.T) {
	if ErrorCategory(nil) != "" || ErrorStage(nil) != "" || HTTPStatus(nil) != 0 ||
		ErrorSummary(nil) != "" || SafeDetail(nil) != "" || ErrorAttrs(nil) != nil {
		t.Fatal("nil error helpers must return zero values")
	}
}

func TestRedactCredentialShapes(t *testing.T) {
	cases := []struct {
		name    string
		input   string
		wantNot []string
		wantIn  []string
	}{
		{
			name:    "authorization header",
			input:   "request failed: Authorization: Bearer abcdef1234567890",
			wantNot: []string{"abcdef1234567890"},
			wantIn:  []string{"Authorization=***"},
		},
		{
			name:    "bare bearer token",
			input:   "upstream rejected header Bearer abcdef1234567890",
			wantNot: []string{"abcdef1234567890"},
			wantIn:  []string{"Bearer ***"},
		},
		{
			name:    "api key assignment",
			input:   `upstream said api_key=sk-abcdefghijklmnop`,
			wantNot: []string{"sk-abcdefghijklmnop"},
			wantIn:  []string{"api_key=***"},
		},
		{
			name:    "password query",
			input:   "GET http://host/path?token=supersecret&x=1 failed",
			wantNot: []string{"supersecret"},
			wantIn:  []string{"token=***"},
		},
		{
			name:    "url userinfo",
			input:   "dial http://user:hunter2@db.internal:3306 failed",
			wantNot: []string{"hunter2"},
			wantIn:  []string{"http://***@db.internal"},
		},
		{
			name:    "encrypted value",
			input:   "cipher enc:v1:QUJDREVGR0hJSktMTU5PUA== leaked",
			wantNot: []string{"QUJDREVGR0hJSktMTU5PUA=="},
			wantIn:  []string{"enc:v1:***"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			redacted := Redact(testCase.input)
			for _, forbidden := range testCase.wantNot {
				if strings.Contains(redacted, forbidden) {
					t.Fatalf("Redact(%q) = %q, still contains %q", testCase.input, redacted, forbidden)
				}
			}
			for _, expected := range testCase.wantIn {
				if !strings.Contains(redacted, expected) {
					t.Fatalf("Redact(%q) = %q, missing %q", testCase.input, redacted, expected)
				}
			}
		})
	}
}

func TestRedactCollapsesLinesAndTruncates(t *testing.T) {
	redacted := Redact("first line\r\nsecond\tline")
	if redacted != "first line second line" {
		t.Fatalf("Redact() = %q", redacted)
	}
	truncated := RedactLimit(strings.Repeat("中", 20), 5)
	if truncated != "中中中中中..." {
		t.Fatalf("RedactLimit() = %q", truncated)
	}
}

func TestSafeDetailUsesRedaction(t *testing.T) {
	err := errors.New("connect failed api_key=sk-abcdefghijklmnop")
	detail := SafeDetail(err)
	if strings.Contains(detail, "sk-abcdefghijklmnop") {
		t.Fatalf("SafeDetail() = %q leaked the key", detail)
	}
}
