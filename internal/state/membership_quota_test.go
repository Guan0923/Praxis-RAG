package state

import (
	"context"
	"errors"
	"sync"
	"testing"
)

// TestMembershipEmbeddingReservationIsAtomic pins the acceptance case: with
// 1,000 characters left, two concurrent 800-character document reservations may
// admit at most one. The loser must not make an upstream call and the period
// totals must stay consistent.
func TestMembershipEmbeddingReservationIsAtomic(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "quota-atomic")

	base := EmbeddingReservationInput{
		UserID: user.ID, OperationID: "quota:base", Namespace: "kb",
		DocumentName: "base.md", DocumentVersion: "v0", Model: "embed",
		Purpose: "document", Chars: 99_000, Now: now,
	}
	if _, err := stores.Membership.ReserveEmbedding(ctx, base); err != nil {
		t.Fatalf("ReserveEmbedding(base) error = %v", err)
	}

	type outcome struct {
		operationID string
		reservation EmbeddingReservation
		err         error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, operation := range []string{"quota:a", "quota:b"} {
		wait.Add(1)
		go func(operationID string) {
			defer wait.Done()
			<-start
			reservation, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
				UserID: user.ID, OperationID: operationID, Namespace: "kb",
				DocumentName: operationID + ".md", DocumentVersion: "v1", Model: "embed",
				Purpose: "document", Chars: 800, Now: now,
			})
			results <- outcome{operationID: operationID, reservation: reservation, err: err}
		}(operation)
	}
	close(start)
	wait.Wait()
	close(results)

	accepted, rejected := 0, 0
	winner := ""
	for result := range results {
		if result.err == nil {
			accepted++
			winner = result.operationID
			continue
		}
		var exceeded *QuotaExceededError
		if !errors.As(result.err, &exceeded) {
			t.Fatalf("loser error = %v (%T), want QuotaExceededError", result.err, result.err)
		}
		if exceeded.Kind != "embedding" || exceeded.Message == "" {
			t.Fatalf("quota error = %+v", exceeded)
		}
		rejected++
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted/rejected = %d/%d, want 1/1", accepted, rejected)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.ReservedChars != 99_800 || summary.UsedChars != 0 {
		t.Fatalf("summary = %+v, want reserved 99800", summary)
	}

	// Settling the winner releases only its reservation; the loser never became
	// a ledger row.
	if _, err := stores.Membership.SettleEmbedding(ctx, winner, 800); err != nil {
		t.Fatalf("SettleEmbedding() error = %v", err)
	}
	summary, _ = stores.Membership.QuotaSummary(ctx, user.ID, now)
	if summary.UsedChars != 800 || summary.ReservedChars != 99_000 {
		t.Fatalf("summary after settle = %+v", summary)
	}
}

// TestMembershipEmbeddingSettlementIsIdempotent covers duplicate callbacks and
// pending-review retries: the same logical call is charged at most once.
func TestMembershipEmbeddingSettlementIsIdempotent(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "quota-settle")
	reservation := EmbeddingReservationInput{
		UserID: user.ID, OperationID: "quota:retry", Namespace: "kb",
		DocumentName: "retry.md", DocumentVersion: "v1", Model: "embed",
		Purpose: "document", Chars: 1_000, Now: now,
	}
	if _, err := stores.Membership.ReserveEmbedding(ctx, reservation); err != nil {
		t.Fatalf("ReserveEmbedding() error = %v", err)
	}
	if _, err := stores.Membership.PendingReviewEmbedding(ctx, reservation.OperationID, 400); err != nil {
		t.Fatalf("PendingReviewEmbedding() error = %v", err)
	}
	summary, _ := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if summary.UsedChars != 400 || summary.ReservedChars != 600 {
		t.Fatalf("pending summary = %+v", summary)
	}
	// Automatic retry succeeds with the full 1,000 characters: only the extra
	// 600 are charged and the remaining hold is released.
	if _, err := stores.Membership.SettleEmbedding(ctx, reservation.OperationID, 1_000); err != nil {
		t.Fatalf("SettleEmbedding(retry) error = %v", err)
	}
	summary, _ = stores.Membership.QuotaSummary(ctx, user.ID, now)
	if summary.UsedChars != 1_000 || summary.ReservedChars != 0 {
		t.Fatalf("settled summary = %+v", summary)
	}
	// A duplicate callback and a late second settle do not add usage.
	if _, err := stores.Membership.SettleEmbedding(ctx, reservation.OperationID, 1_000); err != nil {
		t.Fatalf("SettleEmbedding(duplicate) error = %v", err)
	}
	summary, _ = stores.Membership.QuotaSummary(ctx, user.ID, now)
	if summary.UsedChars != 1_000 || summary.ReservedChars != 0 {
		t.Fatalf("duplicate settle changed summary: %+v", summary)
	}
}

// TestFileAdmissionQuotaIsAtomic reserves the last Free file slot exactly once
// when two different document names are submitted concurrently.
func TestFileAdmissionQuotaIsAtomic(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "quota-files")
	createTestKnowledgeBase(t, stores, "quota-files-kb", user.ID)
	for index := 0; index < 9; index++ {
		publishTestDocument(t, stores, "quota-files-kb", "existing-"+string(rune('a'+index))+".md")
	}

	type outcome struct {
		admission DocumentAdmission
		err       error
	}
	start := make(chan struct{})
	results := make(chan outcome, 2)
	var wait sync.WaitGroup
	for _, name := range []string{"tenth-a.md", "tenth-b.md"} {
		wait.Add(1)
		go func(name string) {
			defer wait.Done()
			<-start
			admission, err := stores.DocumentJobs.Submit(ctx, "quota-files-kb", name,
				[]byte("# tenth\n"), nil)
			results <- outcome{admission: admission, err: err}
		}(name)
	}
	close(start)
	wait.Wait()
	close(results)

	accepted, rejected := 0, 0
	for result := range results {
		if result.err == nil {
			accepted++
			continue
		}
		var exceeded *DocumentQuotaExceededError
		if !errors.As(result.err, &exceeded) {
			t.Fatalf("loser error = %v (%T), want DocumentQuotaExceededError", result.err, result.err)
		}
		if exceeded.Limit != 10 || exceeded.Used != 10 {
			t.Fatalf("file quota error = %+v", exceeded)
		}
		rejected++
	}
	if accepted != 1 || rejected != 1 {
		t.Fatalf("accepted/rejected = %d/%d, want 1/1", accepted, rejected)
	}
	usage, err := stores.Membership.FileUsage(ctx, "quota-files-kb", now)
	if err != nil {
		t.Fatalf("FileUsage() error = %v", err)
	}
	if usage.Used != 10 || usage.CanAdd {
		t.Fatalf("usage = %+v, want used 10 and full", usage)
	}

	// A failed active job releases its logical slot; the next admission is
	// accepted again.
	if _, err := stores.DB.ExecContext(ctx,
		"UPDATE document_jobs SET status = ? WHERE namespace = ? AND status IN (?, ?)",
		DocumentJobFailed, "quota-files-kb", DocumentJobQueued, DocumentJobProcessing); err != nil {
		t.Fatalf("marking active jobs failed: %v", err)
	}
	if _, err := stores.DocumentJobs.Submit(ctx, "quota-files-kb", "tenth-c.md",
		[]byte("# tenth c\n"), nil); err != nil {
		t.Fatalf("Submit after release error = %v", err)
	}
}

// TestOwnedKnowledgeBaseLimitIsAtomic covers the Free 5-library boundary and
// verifies a shared library does not consume the owner's count.
func TestOwnedKnowledgeBaseLimitIsAtomic(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	user := createMembershipTestUser(t, stores, "quota-libraries")
	// Another owner's library shared with this user must not count.
	other := createMembershipTestUser(t, stores, "quota-libraries-other")
	createTestKnowledgeBase(t, stores, "shared-library", other.ID)
	if err := stores.Permissions.Upsert(ctx, Permission{
		Namespace: "shared-library", GranteeUserID: user.ID,
		Permission: PermissionRead, GrantedBy: other.ID,
	}); err != nil {
		t.Fatalf("Permissions.Upsert() error = %v", err)
	}
	for index := 0; index < 5; index++ {
		namespace := "owned-" + string(rune('a'+index))
		if err := stores.KnowledgeBases.CreateWithOwnerLimit(ctx, testKnowledgeBaseSettings(namespace),
			user.ID, 5); err != nil {
			t.Fatalf("create owned library %d error = %v", index, err)
		}
	}
	// Concurrent sixth/seventh attempts: exactly one must lose, and the winner
	// still cannot produce two rows.
	errs := make(chan error, 2)
	for _, namespace := range []string{"owned-sixth", "owned-seventh"} {
		go func(namespace string) {
			errs <- stores.KnowledgeBases.CreateWithOwnerLimit(ctx,
				testKnowledgeBaseSettings(namespace), user.ID, 5)
		}(namespace)
	}
	first, second := <-errs, <-errs
	if first == nil && second == nil {
		t.Fatal("two libraries exceeded the 5-library limit")
	}
	if first != nil {
		var limit *KnowledgeBaseLimitError
		if !errors.As(first, &limit) {
			t.Fatalf("first error = %v (%T)", first, first)
		}
	}
	if second != nil {
		var limit *KnowledgeBaseLimitError
		if !errors.As(second, &limit) {
			t.Fatalf("second error = %v (%T)", second, second)
		}
	}
	owned, err := stores.KnowledgeBases.OwnedNamespaces(ctx, user.ID)
	if err != nil {
		t.Fatalf("OwnedNamespaces() error = %v", err)
	}
	if len(owned) != 5 {
		t.Fatalf("owned libraries = %d, want 5", len(owned))
	}
}

func testKnowledgeBaseSettings(namespace string) map[string]any {
	return map[string]any{"namespace": namespace, "collection": namespace + "-collection"}
}
