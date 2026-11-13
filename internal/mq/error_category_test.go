package mq

import (
	"errors"
	"strings"
	"testing"
)

// TestSafeErrorCategoryDoesNotExposeCause pins the Kafka transport category and
// verifies the safe surface never leaks the broker cause, which may carry
// addresses or credentials. Cause() stays available for explicit diagnostics.
func TestSafeErrorCategoryDoesNotExposeCause(t *testing.T) {
	cause := errors.New("dial tcp 10.0.0.7:9092: password=broker-secret")
	err := newSafeError(ErrPublishFailed, "publish", cause)

	if got := err.ErrorCategory(); got != "kafka_transport_error" {
		t.Fatalf("ErrorCategory() = %q, want kafka_transport_error", got)
	}
	message := err.Error()
	if strings.Contains(message, "broker-secret") || strings.Contains(message, "10.0.0.7") {
		t.Fatalf("Error() = %q, leaks the cause", message)
	}
	if !errors.Is(err, ErrPublishFailed) {
		t.Fatalf("errors.Is(err, ErrPublishFailed) = false")
	}
	if err.Cause() != cause {
		t.Fatalf("Cause() = %v, want the original transport error", err.Cause())
	}
}
