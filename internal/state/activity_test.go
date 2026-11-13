package state

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestActivityCountersRecordsHourlyBuckets verifies the hourly upsert, the
// window boundary and metric isolation of the activity counters.
func TestActivityCountersRecordsHourlyBuckets(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	now := time.Now()

	if got, err := stores.Activity.Count(ctx, ActivitySearch, now.Add(-time.Hour)); err != nil {
		t.Fatalf("Count() error = %v", err)
	} else if got != 0 {
		t.Fatalf("Count() = %d before any record, want 0", got)
	}

	for attempt := 0; attempt < 3; attempt++ {
		if err := stores.Activity.Record(ctx, ActivitySearch, now); err != nil {
			t.Fatalf("Record() error = %v", err)
		}
	}
	var bucketRows int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM activity_counters WHERE metric = ?", ActivitySearch).Scan(&bucketRows); err != nil {
		t.Fatalf("counting activity buckets: %v", err)
	}
	if bucketRows != 1 {
		t.Fatalf("bucket rows = %d, want 1 (records in the same hour share one row)", bucketRows)
	}
	if got, err := stores.Activity.Count(ctx, ActivitySearch, now.Add(-time.Hour)); err != nil || got != 3 {
		t.Fatalf("Count() = %d, %v; want 3, nil", got, err)
	}

	// A record outside the window must stay out of the sum.
	if err := stores.Activity.Record(ctx, ActivitySearch, now.Add(-48*time.Hour)); err != nil {
		t.Fatalf("Record(old) error = %v", err)
	}
	if got, err := stores.Activity.Count(ctx, ActivitySearch, now.Add(-time.Hour)); err != nil || got != 3 {
		t.Fatalf("Count() = %d, %v after an old record; want 3, nil", got, err)
	}
	if got, err := stores.Activity.Count(ctx, ActivitySearch, now.Add(-72*time.Hour)); err != nil || got != 4 {
		t.Fatalf("Count(wide window) = %d, %v; want 4, nil", got, err)
	}

	// Another metric must not leak into ActivitySearch.
	if err := stores.Activity.Record(ctx, "upload", now); err != nil {
		t.Fatalf("Record(upload) error = %v", err)
	}
	if got, err := stores.Activity.Count(ctx, ActivitySearch, now.Add(-time.Hour)); err != nil || got != 3 {
		t.Fatalf("Count() = %d, %v after another metric; want 3, nil", got, err)
	}
	if got, err := stores.Activity.Count(ctx, "upload", now.Add(-time.Hour)); err != nil || got != 1 {
		t.Fatalf("Count(upload) = %d, %v; want 1, nil", got, err)
	}
}

// TestActivityCountersValidation asserts invalid metric names and an
// unassembled store fail explicitly instead of writing a bogus row.
func TestActivityCountersValidation(t *testing.T) {
	stores := newTestStores(t)
	ctx := context.Background()
	for _, metric := range []string{"   ", strings.Repeat("x", activityMetricMaxLength+1)} {
		if err := stores.Activity.Record(ctx, metric, time.Now()); err == nil {
			t.Fatalf("Record(%q) error = nil, want a validation error", metric)
		}
	}
	var detached *ActivityCounters
	if _, err := detached.Count(ctx, ActivitySearch, time.Now()); err == nil {
		t.Fatal("Count(detached) error = nil, want a state error")
	}
	if err := detached.Record(ctx, ActivitySearch, time.Now()); err == nil {
		t.Fatal("Record(detached) error = nil, want a state error")
	}
}
