package logging

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
)

// Well-known correlation field names. They are stable so one request can be
// followed from the HTTP access log to the durable job row and the worker log.
const (
	FieldRequestID   = "request_id"
	FieldNamespace   = "namespace"
	FieldOperationID = "operation_id"
	FieldArtifactID  = "artifact_id"
	FieldBatchID     = "batch_id"
)

// contextKey is the private context key holding *contextState.
type contextKey struct{}

// contextState is the mutable, mutex-guarded attribute set carried by a
// context. Annotate can extend it in place (HTTP handlers attach the ids they
// create), while With forks it so a child context never rewrites its parent.
type contextState struct {
	mu     sync.Mutex
	fields []slog.Attr
}

func stateFrom(ctx context.Context) *contextState {
	if ctx == nil {
		return nil
	}
	state, _ := ctx.Value(contextKey{}).(*contextState)
	return state
}

// With returns a context carrying the given slog key/value pairs in addition to
// whatever the parent already carried. Later values for the same key win.
func With(ctx context.Context, args ...any) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	added := toAttrs(args)
	if len(added) == 0 {
		return ctx
	}
	parent := stateFrom(ctx)
	fields := make([]slog.Attr, 0, len(added))
	if parent != nil {
		parent.mu.Lock()
		fields = append(fields, parent.fields...)
		parent.mu.Unlock()
	}
	fields = mergeAttrs(fields, added)
	return context.WithValue(ctx, contextKey{}, &contextState{fields: fields})
}

// Annotate extends the attribute set in place so ids discovered after the
// context was created appear on the enclosing request's log line. It is a
// no-op for contexts that carry no logging state, and is safe to call from
// concurrent goroutines sharing the context.
func Annotate(ctx context.Context, args ...any) {
	added := toAttrs(args)
	if len(added) == 0 {
		return
	}
	state := stateFrom(ctx)
	if state == nil {
		return
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	state.fields = mergeAttrs(state.fields, added)
}

// Fields returns a copy of the context's attributes in slog form.
func Fields(ctx context.Context) []slog.Attr {
	state := stateFrom(ctx)
	if state == nil {
		return nil
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	return append([]slog.Attr(nil), state.fields...)
}

// Attrs returns a copy of the context's attributes as slog key/value pairs.
func Attrs(ctx context.Context) []any {
	fields := Fields(ctx)
	if len(fields) == 0 {
		return nil
	}
	args := make([]any, 0, len(fields))
	for _, field := range fields {
		args = append(args, field.Key, field.Value.Any())
	}
	return args
}

// FromContext returns the default logger enriched with the context's
// correlation fields. Use it instead of log.Printf / slog.Default() in code
// that has a context.
func FromContext(ctx context.Context) *slog.Logger {
	fields := Fields(ctx)
	if len(fields) == 0 {
		return slog.Default()
	}
	return slog.Default().With(attrValues(fields)...)
}

func attrValues(fields []slog.Attr) []any {
	args := make([]any, 0, len(fields))
	for _, field := range fields {
		args = append(args, field)
	}
	return args
}

// Value returns one context correlation field.
func Value(ctx context.Context, key string) string {
	state := stateFrom(ctx)
	if state == nil {
		return ""
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, field := range state.fields {
		if field.Key == key {
			return field.Value.String()
		}
	}
	return ""
}

// RequestID returns the request correlation id, empty outside an HTTP request.
func RequestID(ctx context.Context) string { return Value(ctx, FieldRequestID) }

// CorrelationID returns the best available id for cross-service correlation:
// the request id first, then the durable document ids a background task carries.
func CorrelationID(ctx context.Context) string {
	for _, key := range []string{FieldRequestID, FieldOperationID, FieldArtifactID} {
		if value := Value(ctx, key); value != "" {
			return value
		}
	}
	return ""
}

// WithRequestID attaches a request correlation id.
func WithRequestID(ctx context.Context, requestID string) context.Context {
	return With(ctx, FieldRequestID, requestID)
}

// WithNamespace attaches the authenticated tenant namespace.
func WithNamespace(ctx context.Context, namespace string) context.Context {
	return With(ctx, FieldNamespace, namespace)
}

// WithOperationID attaches the durable upload operation id.
func WithOperationID(ctx context.Context, operationID string) context.Context {
	return With(ctx, FieldOperationID, operationID)
}

// WithArtifactID attaches the durable document artifact id.
func WithArtifactID(ctx context.Context, artifactID string) context.Context {
	return With(ctx, FieldArtifactID, artifactID)
}

// WithBatchID attaches the document-batch id.
func WithBatchID(ctx context.Context, batchID string) context.Context {
	return With(ctx, FieldBatchID, batchID)
}

// toAttrs normalises slog-style key/value arguments into attributes. Non-string
// keys and odd argument counts are preserved as slog's "!BADKEY" marker instead
// of panicking.
func toAttrs(args []any) []slog.Attr {
	if len(args) == 0 {
		return nil
	}
	attrs := make([]slog.Attr, 0, (len(args)+1)/2)
	for index := 0; index < len(args); {
		first := args[index]
		if attr, ok := first.(slog.Attr); ok {
			attrs = append(attrs, attr)
			index++
			continue
		}
		key, ok := first.(string)
		if !ok {
			key = fmt.Sprint("!BADKEY=", first)
			attrs = append(attrs, slog.String(key, ""))
			index++
			continue
		}
		if index+1 >= len(args) {
			attrs = append(attrs, slog.Any(key, "!MISSINGVALUE"))
			break
		}
		attrs = append(attrs, slog.Any(key, args[index+1]))
		index += 2
	}
	return attrs
}

// mergeAttrs appends additions, replacing an existing value with the same key
// so a late annotation wins without duplicating the field in the log line.
func mergeAttrs(base, additions []slog.Attr) []slog.Attr {
	merged := append([]slog.Attr(nil), base...)
	for _, addition := range additions {
		replaced := false
		for index := range merged {
			if merged[index].Key == addition.Key {
				merged[index] = addition
				replaced = true
				break
			}
		}
		if !replaced {
			merged = append(merged, addition)
		}
	}
	return merged
}
