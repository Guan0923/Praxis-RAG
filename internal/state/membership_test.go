package state

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/plans"
)

// membershipTestNow is one fixed instant inside a Shanghai natural month so the
// membership/quota assertions never depend on the wall clock.
func membershipTestNow() time.Time {
	return time.Date(2026, 11, 17, 3, 0, 0, 0, time.UTC)
}

// newMembershipTestStores skips when the disposable MySQL schema is not
// configured and truncates it otherwise.
func newMembershipTestStores(t *testing.T) *Stores {
	t.Helper()
	mysqlTestURL(t)
	return newTestStores(t)
}

// createMembershipTestUser inserts one Console account.
func createMembershipTestUser(t *testing.T, stores *Stores, username string) User {
	t.Helper()
	user, err := stores.Users.Create(context.Background(), username, "pbkdf2-test-hash")
	if err != nil {
		t.Fatalf("Users.Create(%q) error = %v", username, err)
	}
	return user
}

// createTestKnowledgeBase inserts one knowledge base, optionally owned.
func createTestKnowledgeBase(t *testing.T, stores *Stores, namespace, ownerUserID string) {
	t.Helper()
	err := stores.KnowledgeBases.CreateWithOwner(context.Background(), map[string]any{
		"namespace":  namespace,
		"collection": namespace + "-collection",
	}, ownerUserID)
	if err != nil {
		t.Fatalf("KnowledgeBases.CreateWithOwner(%q) error = %v", namespace, err)
	}
}

// publishTestDocument writes one published document row.
func publishTestDocument(t *testing.T, stores *Stores, namespace, name string) {
	t.Helper()
	documentID, err := randomHex(16)
	if err != nil {
		t.Fatalf("randomHex error = %v", err)
	}
	err = stores.Documents.Upsert(context.Background(), namespace, DocumentRecord{
		Name:       name,
		DocumentID: documentID,
		Size:       4,
		SHA256:     sha256Hex(name),
		Collection: namespace + "-collection",
	})
	if err != nil {
		t.Fatalf("Documents.Upsert(%q) error = %v", name, err)
	}
}

// submitTestDocument admits one small file.
func submitMembershipTestDocument(t *testing.T, stores *Stores, namespace, name, content string, idempotencyKey *string) (DocumentAdmission, error) {
	t.Helper()
	return stores.DocumentJobs.Submit(context.Background(), namespace, name, []byte(content), idempotencyKey)
}

func TestMembershipEffectiveFreeBaseline(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "free-user")

	entitlement, err := stores.Membership.Effective(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	if entitlement.Plan != plans.PlanFree || entitlement.Source != MembershipSourceFree {
		t.Fatalf("Effective() = %+v, want the Free plan", entitlement)
	}
	if entitlement.Limits.MaxOwnedKnowledgeBases != 5 || entitlement.Limits.MaxFilesPerKB != 10 {
		t.Fatalf("Free limits = %+v", entitlement.Limits)
	}
	start, end := plans.FreeMonthBounds(now)
	if !entitlement.StartsAt.Equal(start) || !entitlement.RefreshAt.Equal(end) {
		t.Fatalf("Free Start/RefreshAt = %v/%v, want %v/%v",
			entitlement.StartsAt, entitlement.RefreshAt, start, end)
	}
	canShare, err := stores.Membership.CanShare(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("CanShare() error = %v", err)
	}
	if canShare {
		t.Fatal("Free CanShare() = true, want false")
	}
	var plan string
	var version int64
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT plan, entitlement_version FROM user_memberships WHERE user_id = ?", user.ID).
		Scan(&plan, &version); err != nil {
		t.Fatalf("reading the membership row: %v", err)
	}
	if plan != string(plans.PlanFree) || version != 1 {
		t.Fatalf("membership row = %q/%d, want free/1", plan, version)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.Source != MembershipSourceFree || summary.AllowanceChars != 100_000 ||
		summary.UsedChars != 0 || summary.ReservedChars != 0 {
		t.Fatalf("QuotaSummary() = %+v", summary)
	}
	if !summary.RefreshAt.Equal(end) {
		t.Fatalf("QuotaSummary RefreshAt = %v, want %v", summary.RefreshAt, end)
	}
}

func TestMembershipBackfillIsIdempotent(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	user := createMembershipTestUser(t, stores, "backfill-user")
	var count int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_memberships WHERE user_id = ?", user.ID).Scan(&count); err != nil {
		t.Fatalf("counting membership rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("membership rows before the migration = %d, want 0", count)
	}
	for attempt := 0; attempt < 2; attempt++ {
		if err := stores.ensureSchema(ctx); err != nil {
			t.Fatalf("ensureSchema() attempt %d error = %v", attempt, err)
		}
	}
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_memberships WHERE user_id = ? AND plan = ?", user.ID, "free").
		Scan(&count); err != nil {
		t.Fatalf("counting backfilled rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("backfilled membership rows = %d, want 1", count)
	}
}

func TestKnowledgeBaseOwnerLimit(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	user := createMembershipTestUser(t, stores, "kb-limit-user")
	for index := 0; index < 5; index++ {
		namespace := "owned-" + string(rune('a'+index))
		if err := stores.KnowledgeBases.CreateWithOwnerLimit(ctx, map[string]any{
			"namespace": namespace, "collection": namespace + "-c",
		}, user.ID, 5); err != nil {
			t.Fatalf("CreateWithOwnerLimit #%d error = %v", index, err)
		}
	}
	err := stores.KnowledgeBases.CreateWithOwnerLimit(ctx, map[string]any{
		"namespace": "owned-f", "collection": "owned-f-c",
	}, user.ID, 5)
	limitErr := mustBeError[*KnowledgeBaseLimitError](t, err)
	if limitErr.Current != 5 || limitErr.Limit != 5 {
		t.Fatalf("KnowledgeBaseLimitError = %+v, want current=5 limit=5", limitErr)
	}
	if !strings.Contains(limitErr.Error(), "5") {
		t.Fatalf("KnowledgeBaseLimitError message = %q", limitErr.Error())
	}
	if existing, err := stores.KnowledgeBases.Get(ctx, "owned-f"); err != nil || existing != nil {
		t.Fatalf("rejected knowledge base exists: %v / %v", existing, err)
	}
	// The legacy unlimited method keeps its old behaviour.
	if err := stores.KnowledgeBases.CreateWithOwner(ctx, map[string]any{
		"namespace": "owned-g", "collection": "owned-g-c",
	}, user.ID); err != nil {
		t.Fatalf("CreateWithOwner after the limit error = %v", err)
	}
}

func TestKnowledgeBaseOwnerLimitConcurrent(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	user := createMembershipTestUser(t, stores, "kb-concurrent-user")
	for index := 0; index < 4; index++ {
		namespace := "concurrent-" + string(rune('a'+index))
		if err := stores.KnowledgeBases.CreateWithOwnerLimit(ctx, map[string]any{
			"namespace": namespace, "collection": namespace + "-c",
		}, user.ID, 5); err != nil {
			t.Fatalf("CreateWithOwnerLimit #%d error = %v", index, err)
		}
	}
	var wait sync.WaitGroup
	results := make(chan error, 2)
	for index := 0; index < 2; index++ {
		namespace := "race-" + string(rune('a'+index))
		wait.Add(1)
		go func(namespace string) {
			defer wait.Done()
			results <- stores.KnowledgeBases.CreateWithOwnerLimit(ctx, map[string]any{
				"namespace": namespace, "collection": namespace + "-c",
			}, user.ID, 5)
		}(namespace)
	}
	wait.Wait()
	close(results)
	succeeded, limited := 0, 0
	for err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.As(err, new(*KnowledgeBaseLimitError)):
			limited++
		default:
			t.Fatalf("unexpected concurrent create error: %v", err)
		}
	}
	if succeeded != 1 || limited != 1 {
		t.Fatalf("concurrent creates: %d succeeded / %d limited, want 1/1", succeeded, limited)
	}
}

func TestDocumentFileQuotaAndIdempotentReplay(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "file-quota-user")
	namespace := "file-quota-kb"
	createTestKnowledgeBase(t, stores, namespace, user.ID)
	for index := 0; index < 9; index++ {
		publishTestDocument(t, stores, namespace, "doc-"+string(rune('a'+index))+".md")
	}
	usage, err := stores.Membership.FileUsage(ctx, namespace, now)
	if err != nil {
		t.Fatalf("FileUsage() error = %v", err)
	}
	if usage.Used != 9 || usage.Limit != 10 || !usage.CanAdd || usage.Unlimited {
		t.Fatalf("FileUsage before the last slot = %+v", usage)
	}
	if usage.OwnerUserID != user.ID {
		t.Fatalf("FileUsage owner = %q, want %q", usage.OwnerUserID, user.ID)
	}

	idempotencyKey := "idem-first-admission"
	first, err := submitMembershipTestDocument(t, stores, namespace, "first.md", "first-content", &idempotencyKey)
	if err != nil {
		t.Fatalf("Submit(first.md) error = %v", err)
	}
	if first.Reused {
		t.Fatal("first Submit was reported as reused")
	}
	usage, err = stores.Membership.FileUsage(ctx, namespace, now)
	if err != nil {
		t.Fatalf("FileUsage() after admission error = %v", err)
	}
	if usage.Used != 10 || usage.CanAdd {
		t.Fatalf("FileUsage at the limit = %+v", usage)
	}
	if usage.Reason != "知识库文件数已达上限（10 个）" {
		t.Fatalf("FileUsage Reason = %q", usage.Reason)
	}

	// An idempotent replay returns the admitted artifact even though the quota
	// is exhausted: replay never re-checks or re-counts.
	replay, err := submitMembershipTestDocument(t, stores, namespace, "first.md", "first-content", &idempotencyKey)
	if err != nil {
		t.Fatalf("replayed Submit error = %v", err)
	}
	if !replay.Reused || replay.ArtifactID != first.ArtifactID {
		t.Fatalf("replayed Submit = %+v, want reused %q", replay, first.ArtifactID)
	}

	_, err = submitMembershipTestDocument(t, stores, namespace, "second.md", "second-content", nil)
	quotaErr := mustBeError[*DocumentQuotaExceededError](t, err)
	if quotaErr.Used != 10 || quotaErr.Limit != 10 {
		t.Fatalf("DocumentQuotaExceededError = %+v", quotaErr)
	}
	if !strings.Contains(quotaErr.Error(), "所有者的知识库文件数已达上限（10 个）") {
		t.Fatalf("DocumentQuotaExceededError message = %q", quotaErr.Error())
	}
}

func TestFileUsageUnownedKnowledgeBase(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	if err := stores.KnowledgeBases.Create(ctx, map[string]any{
		"namespace": "system-kb", "collection": "system-kb-c",
	}); err != nil {
		t.Fatalf("KnowledgeBases.Create() error = %v", err)
	}
	usage, err := stores.Membership.FileUsage(ctx, "system-kb", now)
	if err != nil {
		t.Fatalf("FileUsage() error = %v", err)
	}
	if !usage.Unlimited || !usage.CanAdd || usage.OwnerUserID != "" || usage.Reason != "" {
		t.Fatalf("unowned FileUsage() = %+v", usage)
	}
}

func TestEmbeddingReservationConcurrent(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "embedding-race-user")
	if _, err := stores.Membership.Effective(ctx, user.ID, now); err != nil {
		t.Fatalf("Effective() error = %v", err)
	}
	type result struct {
		reservation EmbeddingReservation
		err         error
	}
	results := make(chan result, 2)
	var wait sync.WaitGroup
	for index := 0; index < 2; index++ {
		operationID := "op-race-" + string(rune('a'+index))
		wait.Add(1)
		go func(operationID string) {
			defer wait.Done()
			reservation, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
				UserID:      user.ID,
				OperationID: operationID,
				Namespace:   "kb",
				Chars:       60_000,
				Now:         now,
			})
			results <- result{reservation: reservation, err: err}
		}(operationID)
	}
	wait.Wait()
	close(results)
	succeeded := 0
	exceeded := 0
	for item := range results {
		if item.err == nil {
			succeeded++
			continue
		}
		var quotaErr *QuotaExceededError
		if errors.As(item.err, &quotaErr) {
			exceeded++
			if quotaErr.Kind != "embedding" || quotaErr.Required != 60_000 ||
				quotaErr.Available != 40_000 {
				t.Fatalf("QuotaExceededError = %+v", quotaErr)
			}
			continue
		}
		t.Fatalf("unexpected concurrent reserve error: %v", item.err)
	}
	if succeeded != 1 || exceeded != 1 {
		t.Fatalf("concurrent reserves: %d succeeded / %d exceeded, want 1/1", succeeded, exceeded)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.ReservedChars != 60_000 || summary.UsedChars != 0 {
		t.Fatalf("QuotaSummary after the race = %+v", summary)
	}
}

func TestEmbeddingSettleAndReleaseIdempotent(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "settle-user")

	reservation, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-settle", Namespace: "kb", Chars: 1_000, Now: now,
	})
	if err != nil {
		t.Fatalf("ReserveEmbedding() error = %v", err)
	}
	if reservation.UsedChars != 0 || reservation.ReservedChars != 1_000 {
		t.Fatalf("reservation = %+v", reservation)
	}
	settled, err := stores.Membership.SettleEmbedding(ctx, "op-settle", 700)
	if err != nil {
		t.Fatalf("SettleEmbedding() error = %v", err)
	}
	if settled.UsedChars != 700 || settled.ReservedChars != 0 {
		t.Fatalf("settled = %+v, want used=700 reserved=0", settled)
	}
	// A repeated settlement callback never charges again.
	again, err := stores.Membership.SettleEmbedding(ctx, "op-settle", 900)
	if err != nil {
		t.Fatalf("repeated SettleEmbedding() error = %v", err)
	}
	if again.UsedChars != 700 {
		t.Fatalf("repeated settlement used = %d, want 700", again.UsedChars)
	}
	// Releasing a settled operation changes nothing.
	released, err := stores.Membership.ReleaseEmbedding(ctx, "op-settle")
	if err != nil {
		t.Fatalf("ReleaseEmbedding(settled) error = %v", err)
	}
	if released.UsedChars != 700 || released.ReservedChars != 0 {
		t.Fatalf("released settled = %+v", released)
	}

	if _, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-release", Namespace: "kb", Chars: 500, Now: now,
	}); err != nil {
		t.Fatalf("ReserveEmbedding(op-release) error = %v", err)
	}
	released, err = stores.Membership.ReleaseEmbedding(ctx, "op-release")
	if err != nil {
		t.Fatalf("ReleaseEmbedding() error = %v", err)
	}
	if released.UsedChars != 700 || released.ReservedChars != 0 {
		t.Fatalf("released = %+v, want used=700 reserved=0", released)
	}
	if _, err := stores.Membership.ReleaseEmbedding(ctx, "op-release"); err != nil {
		t.Fatalf("repeated ReleaseEmbedding() error = %v", err)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.UsedChars != 700 || summary.ReservedChars != 0 {
		t.Fatalf("QuotaSummary() = %+v, want used=700 reserved=0", summary)
	}
	if _, err := stores.Membership.SettleEmbedding(ctx, "missing-operation", 10); err == nil {
		t.Fatal("SettleEmbedding(unknown operation) did not fail")
	}
}

func TestEmbeddingPendingReviewRetrySettlement(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "pending-review-user")

	if _, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-pending", Namespace: "kb", Chars: 1_000, Now: now,
	}); err != nil {
		t.Fatalf("ReserveEmbedding() error = %v", err)
	}
	pending, err := stores.Membership.PendingReviewEmbedding(ctx, "op-pending", 400)
	if err != nil {
		t.Fatalf("PendingReviewEmbedding() error = %v", err)
	}
	if pending.UsedChars != 400 || pending.ReservedChars != 600 {
		t.Fatalf("pending = %+v, want used=400 reserved=600", pending)
	}
	// Repeating the pending-review call does not charge twice.
	repeat, err := stores.Membership.PendingReviewEmbedding(ctx, "op-pending", 400)
	if err != nil {
		t.Fatalf("repeated PendingReviewEmbedding() error = %v", err)
	}
	if repeat.UsedChars != 400 || repeat.ReservedChars != 600 {
		t.Fatalf("repeated pending = %+v", repeat)
	}
	// The automatic retry reuses the same operation identity and is not
	// charged again.
	reused, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-pending", Namespace: "kb", Chars: 1_000, Now: now,
	})
	if err != nil {
		t.Fatalf("replayed ReserveEmbedding() error = %v", err)
	}
	if !reused.Reused || reused.UsedChars != 400 || reused.ReservedChars != 600 {
		t.Fatalf("replayed reservation = %+v", reused)
	}
	// The retry succeeded with 900 characters in total: only the 500 character
	// increment is added and the remaining hold is released.
	settled, err := stores.Membership.SettleEmbedding(ctx, "op-pending", 900)
	if err != nil {
		t.Fatalf("SettleEmbedding(after pending) error = %v", err)
	}
	if settled.UsedChars != 900 || settled.ReservedChars != 0 {
		t.Fatalf("settled = %+v, want used=900 reserved=0", settled)
	}
	again, err := stores.Membership.SettleEmbedding(ctx, "op-pending", 900)
	if err != nil {
		t.Fatalf("repeated SettleEmbedding() error = %v", err)
	}
	if again.UsedChars != 900 {
		t.Fatalf("repeated settlement used = %d, want 900", again.UsedChars)
	}

	// A retry that consumed no more than the confirmed batch only releases the
	// remainder.
	if _, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-pending-small", Namespace: "kb", Chars: 1_000, Now: now,
	}); err != nil {
		t.Fatalf("ReserveEmbedding(op-pending-small) error = %v", err)
	}
	if _, err := stores.Membership.PendingReviewEmbedding(ctx, "op-pending-small", 600); err != nil {
		t.Fatalf("PendingReviewEmbedding(op-pending-small) error = %v", err)
	}
	small, err := stores.Membership.SettleEmbedding(ctx, "op-pending-small", 500)
	if err != nil {
		t.Fatalf("SettleEmbedding(op-pending-small) error = %v", err)
	}
	if small.UsedChars != 1_500 || small.ReservedChars != 0 {
		t.Fatalf("small settlement = %+v, want used=1500 reserved=0", small)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.UsedChars != 1_500 || summary.ReservedChars != 0 {
		t.Fatalf("QuotaSummary() = %+v, want used=1500 reserved=0", summary)
	}
}

func TestEmbeddingStaleRelease(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	user := createMembershipTestUser(t, stores, "stale-user")
	if _, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: user.ID, OperationID: "op-stale", Namespace: "kb", Chars: 2_000, Now: now,
	}); err != nil {
		t.Fatalf("ReserveEmbedding() error = %v", err)
	}
	released, err := stores.Membership.ReleaseStaleEmbedding(ctx, now.Add(23*time.Hour))
	if err != nil {
		t.Fatalf("ReleaseStaleEmbedding(early) error = %v", err)
	}
	if released != 0 {
		t.Fatalf("early stale release = %d, want 0", released)
	}
	released, err = stores.Membership.ReleaseStaleEmbedding(ctx, now.Add(25*time.Hour))
	if err != nil {
		t.Fatalf("ReleaseStaleEmbedding() error = %v", err)
	}
	if released != 1 {
		t.Fatalf("stale release = %d, want 1", released)
	}
	summary, err := stores.Membership.QuotaSummary(ctx, user.ID, now)
	if err != nil {
		t.Fatalf("QuotaSummary() error = %v", err)
	}
	if summary.UsedChars != 0 || summary.ReservedChars != 0 {
		t.Fatalf("QuotaSummary after stale release = %+v", summary)
	}
}

func TestSystemEmbeddingLedger(t *testing.T) {
	stores := newMembershipTestStores(t)
	ctx := context.Background()
	now := membershipTestNow()
	reservation, err := stores.Membership.ReserveEmbedding(ctx, EmbeddingReservationInput{
		UserID: SystemUserID, OperationID: "op-system", Namespace: "system-kb",
		Purpose: EmbeddingPurposeSystem, Chars: 1_000_000, Now: now,
	})
	if err != nil {
		t.Fatalf("ReserveEmbedding(system) error = %v", err)
	}
	if reservation.AllowanceChars != systemEmbeddingAllowance ||
		reservation.ReservedChars != 1_000_000 {
		t.Fatalf("system reservation = %+v", reservation)
	}
	settled, err := stores.Membership.SettleEmbedding(ctx, "op-system", 800_000)
	if err != nil {
		t.Fatalf("SettleEmbedding(system) error = %v", err)
	}
	if settled.UsedChars != 800_000 || settled.ReservedChars != 0 {
		t.Fatalf("system settled = %+v", settled)
	}
	var count int
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM user_memberships WHERE user_id = ?", SystemUserID).Scan(&count); err != nil {
		t.Fatalf("counting system membership rows: %v", err)
	}
	if count != 0 {
		t.Fatalf("system membership rows = %d, want 0", count)
	}
	var source string
	if err := stores.DB.QueryRowContext(ctx,
		"SELECT source FROM embedding_quota_periods WHERE user_id = ?", SystemUserID).
		Scan(&source); err != nil {
		t.Fatalf("reading the system quota period: %v", err)
	}
	if source != MembershipSourceSystem {
		t.Fatalf("system quota source = %q", source)
	}
}
