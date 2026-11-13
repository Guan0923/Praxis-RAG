package mq

import (
	"context"
	"errors"
	"testing"
)

// fakeProgressSource is the documentProgressSource seam: it returns fixed
// per-partition offsets so no live broker is needed.
type fakeProgressSource struct {
	ends      map[int]int64
	committed map[int]int64
	err       error
	calls     int
}

func (f *fakeProgressSource) offsets(context.Context, string, string) (map[int]int64, map[int]int64, error) {
	f.calls++
	if f.err != nil {
		return nil, nil, f.err
	}
	return f.ends, f.committed, nil
}

// TestDocumentProgressFromSourceAggregates checks the healthy path: per-partition
// end and committed offsets become totals and a non-negative lag.
func TestDocumentProgressFromSourceAggregates(t *testing.T) {
	source := &fakeProgressSource{
		ends:      map[int]int64{0: 10, 1: 20},
		committed: map[int]int64{0: 8, 1: 20},
	}
	progress, err := documentProgressFromSource(context.Background(), source, "docs", "docs-workers")
	if err != nil {
		t.Fatalf("documentProgressFromSource() error = %v", err)
	}
	if source.calls != 1 {
		t.Fatalf("source calls = %d, want 1", source.calls)
	}
	if progress.Topic != "docs" || progress.Group != "docs-workers" {
		t.Fatalf("identity = %q/%q", progress.Topic, progress.Group)
	}
	if progress.CommittedOffset == nil || *progress.CommittedOffset != 28 {
		t.Fatalf("CommittedOffset = %v, want 28", progress.CommittedOffset)
	}
	if progress.EndOffset == nil || *progress.EndOffset != 30 {
		t.Fatalf("EndOffset = %v, want 30", progress.EndOffset)
	}
	if progress.Lag == nil || *progress.Lag != 2 {
		t.Fatalf("Lag = %v, want 2", progress.Lag)
	}
}

// TestDocumentProgressFromSourceNoCommit asserts an uncommitted group leaves
// every number nil so the monitor renders 未知 instead of a fabricated zero.
func TestDocumentProgressFromSourceNoCommit(t *testing.T) {
	source := &fakeProgressSource{
		ends:      map[int]int64{0: 10, 1: 20},
		committed: map[int]int64{0: -1, 1: -1},
	}
	progress, err := documentProgressFromSource(context.Background(), source, "docs", "docs-workers")
	if err != nil {
		t.Fatalf("documentProgressFromSource() error = %v", err)
	}
	if progress.CommittedOffset != nil || progress.EndOffset != nil || progress.Lag != nil {
		t.Fatalf("progress = %+v, want nil numbers", progress)
	}
	if progress.Topic != "docs" || progress.Group != "docs-workers" {
		t.Fatalf("identity = %q/%q", progress.Topic, progress.Group)
	}
}

// TestDocumentProgressFromSourceErrors asserts a broker read failure surfaces as
// an error with no partial numbers.
func TestDocumentProgressFromSourceErrors(t *testing.T) {
	failure := errors.New("broker unavailable")
	source := &fakeProgressSource{err: failure}
	progress, err := documentProgressFromSource(context.Background(), source, "docs", "docs-workers")
	if !errors.Is(err, failure) {
		t.Fatalf("error = %v, want %v", err, failure)
	}
	if progress.CommittedOffset != nil || progress.EndOffset != nil || progress.Lag != nil {
		t.Fatalf("progress = %+v, want no numbers on error", progress)
	}

	if _, err := documentProgressFromSource(context.Background(), nil, "docs", "docs-workers"); err == nil {
		t.Fatal("nil source error = nil, want a failure")
	}
}

// TestAggregateConsumerProgressClampsLag asserts an ahead-of-end commit (for
// example after a topic truncation) never reports a negative lag.
func TestAggregateConsumerProgressClampsLag(t *testing.T) {
	progress := aggregateConsumerProgress("docs", "docs-workers",
		map[int]int64{0: 5}, map[int]int64{0: 9})
	if progress.Lag == nil || *progress.Lag != 0 {
		t.Fatalf("Lag = %v, want 0", progress.Lag)
	}
	if progress.CommittedOffset == nil || *progress.CommittedOffset != 9 {
		t.Fatalf("CommittedOffset = %v, want 9", progress.CommittedOffset)
	}
	if progress.EndOffset == nil || *progress.EndOffset != 5 {
		t.Fatalf("EndOffset = %v, want 5", progress.EndOffset)
	}

	empty := aggregateConsumerProgress("docs", "docs-workers", nil, nil)
	if empty.CommittedOffset != nil || empty.EndOffset != nil || empty.Lag != nil {
		t.Fatalf("empty progress = %+v, want nil numbers", empty)
	}
}

// TestDocumentConsumerProgressNotConfigured asserts brokerless deployments fail
// closed with ErrNotConfigured before any dial.
func TestDocumentConsumerProgressNotConfigured(t *testing.T) {
	progress, err := DocumentConsumerProgress(context.Background(), Settings{})
	if !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("error = %v, want ErrNotConfigured", err)
	}
	if progress.CommittedOffset != nil || progress.EndOffset != nil || progress.Lag != nil {
		t.Fatalf("progress = %+v, want no numbers", progress)
	}
}
