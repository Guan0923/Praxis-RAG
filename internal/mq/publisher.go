package mq

import "errors"

// Safe error kinds. Error() strings never include broker details, payloads, or
// credentials; the wrapped cause is available through Cause() for diagnostics
// that must not be logged verbatim.
var (
	ErrNotConfigured   = errors.New("kafka bootstrap servers are not configured")
	ErrPublishFailed   = errors.New("kafka publish failed")
	ErrPublishTimeout  = errors.New("kafka publish timed out")
	ErrPayloadInvalid  = errors.New("kafka payload is invalid")
	ErrPublisherClosed = errors.New("kafka publisher is closed")
	ErrConsumerFailed  = errors.New("kafka consumer failed")
	ErrReceiveFailed   = errors.New("kafka receive failed")
)

// SafeError maps a transport failure to a sanitized category while keeping the
// underlying error reachable only through Cause (never printed by Error).
type SafeError struct {
	Kind  error
	Op    string
	cause error
}

// Error renders only the sanitized operation/category.
func (e *SafeError) Error() string {
	if e.Op == "" {
		return e.Kind.Error()
	}
	return e.Op + ": " + e.Kind.Error()
}

// Unwrap exposes only the safe sentinel so errors.Is works without leaking.
func (e *SafeError) Unwrap() error { return e.Kind }

// Cause returns the underlying transport error. It may contain broker text and
// must not be logged or returned to clients.
func (e *SafeError) Cause() error { return e.cause }

// ErrorCategory implements the optional interface internal/logging uses to
// classify a failure without importing this package. It is deliberately a
// single stable category: Kind and Cause may carry broker text or credentials,
// so they are never rendered into log lines through this method.
func (e *SafeError) ErrorCategory() string { return "kafka_transport_error" }

func newSafeError(kind error, op string, cause error) *SafeError {
	return &SafeError{Kind: kind, Op: op, cause: cause}
}
