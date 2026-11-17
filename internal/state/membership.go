package state

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/plans"
)

// Membership, quota and order state is always read from MySQL. Redis is a
// read-model cache only: an entitlement, a quota decision or an order status
// never depends on a cached value, so a lost or stale Redis key cannot grant
// paid limits or spend another account's allowance.

// SystemUserID is the account id used for platform-owned work (a knowledge base
// without an owner, a maintenance re-embed). It keeps its own ledger source and
// never consumes or creates a personal membership row.
const SystemUserID = "system"

// Entitlement sources stored in user_memberships.source and
// embedding_quota_periods.source.
const (
	MembershipSourceFree   = "free"
	MembershipSourcePaid   = "paid"
	MembershipSourceSystem = "system"
)

// Membership term kinds.
const (
	MembershipKindPurchase = "purchase"
	MembershipKindRenew    = "renew"
	MembershipKindUpgrade  = "upgrade"
)

// Membership term statuses.
const (
	MembershipTermActive   = "active"
	MembershipTermRefunded = "refunded"
)

// Embedding usage ledger statuses.
const (
	EmbeddingUsageReserved      = "reserved"
	EmbeddingUsageSettled       = "settled"
	EmbeddingUsagePendingReview = "pending_review"
	EmbeddingUsageReleased      = "released"
)

// Embedding purposes the runtime is expected to report. The repository records
// whatever it is given and does not reject an unknown purpose: metering belongs
// to the caller and a failed ledger write must not block an ingest.
const (
	EmbeddingPurposeDocument = "document"
	EmbeddingPurposeReembed  = "reembed"
	EmbeddingPurposeSystem   = "system"
)

// EmbeddingReservationLease is how long one reservation (or one pending-review
// hold) keeps quota before ReleaseStaleEmbedding may release it. The platform
// absorbs a call whose upstream result stays unknown for that long.
const EmbeddingReservationLease = 24 * time.Hour

// systemEmbeddingAllowance is the ceiling of one system ledger period. It is a
// counter that never blocks platform maintenance work; the ledger is kept so
// background usage stays attributable.
const systemEmbeddingAllowance int64 = 1 << 62

// Entitlement is one account's effective membership at a point in time.
type Entitlement struct {
	UserID string
	Plan   plans.Plan
	// Source is free|paid|system.
	Source string
	// StartsAt is the start of the current entitlement (the latest covering
	// paid term start, or the current Shanghai natural month for Free).
	StartsAt time.Time
	// ExpiresAt is the furthest active paid term end (zero when no paid term
	// exists), including a renewal that has not started yet.
	ExpiresAt time.Time
	// AnchorDay is the membership day-of-month the paid period is anchored to.
	AnchorDay int
	// EntitlementVersion is bumped by every entitlement grant and is the token
	// upgrade quotes are validated against.
	EntitlementVersion int64
	// RefreshAt is when the current embedding allowance period ends.
	RefreshAt time.Time
	// Limits is the catalog row of Plan.
	Limits plans.Product
}

// MembershipTerm is one immutable grant of a paid period. An upgrade term is an
// override that raises the plan only until the existing expiry.
type MembershipTerm struct {
	ID            string
	UserID        string
	OrderID       string
	Plan          plans.Plan
	BillingPeriod string
	Kind          string
	StartsAt      time.Time
	EndsAt        time.Time
	AnchorDay     int
	Status        string
	CreatedAt     time.Time
	UpdatedAt     time.Time
}

// QuotaSummary is the embedding allowance window shown to an account owner.
type QuotaSummary struct {
	Plan      plans.Plan
	Source    string
	RefreshAt time.Time
	// PeriodStart and PeriodEnd are the left-closed/right-open window bounds.
	PeriodStart time.Time
	PeriodEnd   time.Time
	// AllowanceChars is the granted characters, UsedChars the settled amount
	// and ReservedChars the amount held by in-flight operations.
	AllowanceChars int64
	UsedChars      int64
	ReservedChars  int64
}

// FileUsage is the owner-scoped file admission state of one knowledge base.
// Every account with access reads the owner's number; sharing never spends the
// collaborator's own allowance.
type FileUsage struct {
	Namespace   string
	OwnerUserID string
	Used        int
	// Limit is the owner's catalog file limit; -1 means unlimited.
	Limit     int
	Unlimited bool
	CanAdd    bool
	// Reason is the Chinese explanation shown when CanAdd is false.
	Reason string
}

// EmbeddingReservationInput is one reservation request. OperationID is the
// idempotent identity of the logical embedding call: an automatic retry must
// reuse it.
type EmbeddingReservationInput struct {
	UserID          string
	OperationID     string
	Namespace       string
	DocumentName    string
	DocumentVersion string
	Model           string
	Purpose         string
	Chars           int64
	Now             time.Time
}

// EmbeddingReservation is the ledger state after a reserve/settle/release.
type EmbeddingReservation struct {
	OperationID    string
	PeriodID       string
	Chars          int64
	AllowanceChars int64
	UsedChars      int64
	ReservedChars  int64
	PeriodStart    time.Time
	PeriodEnd      time.Time
	RefreshAt      time.Time
	// Reused reports that the operation already had a ledger row and was not
	// charged again.
	Reused bool
}

// QuotaExceededError reports an insufficient embedding allowance. Callers use
// errors.As and map it to a public failure reason.
type QuotaExceededError struct {
	Kind      string
	Required  int64
	Available int64
	RefreshAt time.Time
	Message   string
}

// Error implements error.
func (e *QuotaExceededError) Error() string { return e.Message }

// DocumentQuotaExceededError reports that the knowledge-base owner's logical
// file allowance is exhausted. It is returned from the admission transaction
// before a document job row is written.
type DocumentQuotaExceededError struct {
	Used    int
	Limit   int
	Message string
}

// Error implements error.
func (e *DocumentQuotaExceededError) Error() string { return e.Message }

// KnowledgeBaseLimitError reports that the owner already created the maximum
// number of knowledge bases allowed by the plan.
type KnowledgeBaseLimitError struct {
	Current int
	Limit   int
	Message string
}

// Error implements error.
func (e *KnowledgeBaseLimitError) Error() string { return e.Message }

// sqlQueryer is the read/write surface shared by *sql.DB and *sql.Tx so the
// entitlement helpers can serve both the standalone and in-transaction paths.
type sqlQueryer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// MembershipRepository is the MySQL implementation of the personal-membership
// entitlement and the embedding quota ledger.
type MembershipRepository struct {
	db *sql.DB
	// Clock is used by operations that do not take a caller timestamp
	// (PendingReviewEmbedding and ReleaseStaleEmbedding).
	Clock func() time.Time
}

func newMembershipRepository(db *sql.DB) *MembershipRepository {
	return &MembershipRepository{db: db, Clock: func() time.Time { return time.Now().UTC() }}
}

func (r *MembershipRepository) now() time.Time {
	if r.Clock == nil {
		return time.Now().UTC()
	}
	return r.Clock()
}

// normalizeMembershipNow treats the zero time as "now" so a caller can omit the
// instant on read paths.
func normalizeMembershipNow(now time.Time) time.Time {
	if now.IsZero() {
		return time.Now().UTC()
	}
	return now
}

// newStateID generates one identifier for the membership/billing tables: their
// primary keys are VARCHAR(32), so a dashed UUID (36 characters) would not fit.
func newStateID() (string, error) {
	return randomHex(16)
}

// nullableTimestamp renders a timestamp for storage, NULL for the zero value.
func nullableTimestamp(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return plans.FormatTimestamp(value)
}

// shanghaiDay is the Shanghai calendar day of one instant: membership months
// are anchored to the Shanghai day-of-month, matching plans.AddMonths.
func shanghaiDay(value time.Time) int {
	return value.In(plans.Shanghai()).Day()
}

const membershipTermColumns = "id, user_id, order_id, plan, billing_period, kind, " +
	"starts_at, ends_at, anchor_day, status, created_at, updated_at"

func scanMembershipTerm(scanner rowScanner) (MembershipTerm, error) {
	var term MembershipTerm
	var orderID sql.NullString
	var plan, billingPeriod, kind, startsAt, endsAt, status, createdAt, updatedAt string
	var anchorDay int
	if err := scanner.Scan(&term.ID, &term.UserID, &orderID, &plan, &billingPeriod, &kind,
		&startsAt, &endsAt, &anchorDay, &status, &createdAt, &updatedAt); err != nil {
		return MembershipTerm{}, err
	}
	if orderID.Valid {
		term.OrderID = orderID.String
	}
	term.Plan = plans.Plan(plan)
	term.BillingPeriod = billingPeriod
	term.Kind = kind
	term.StartsAt = plans.ParseTimestamp(startsAt)
	term.EndsAt = plans.ParseTimestamp(endsAt)
	term.AnchorDay = anchorDay
	term.Status = status
	term.CreatedAt = plans.ParseTimestamp(createdAt)
	term.UpdatedAt = plans.ParseTimestamp(updatedAt)
	return term, nil
}

// membershipSnapshot is the cached user_memberships row. It is derived state:
// membership_terms is authoritative for what is in force.
type membershipSnapshot struct {
	Plan               plans.Plan
	Source             string
	StartsAt           time.Time
	ExpiresAt          time.Time
	AnchorDay          int
	EntitlementVersion int64
}

// ensureMembershipRow inserts the default Free row for an account that has
// none. INSERT IGNORE keeps it safe under concurrent startup and requests. The
// system account has no personal membership.
func ensureMembershipRow(ctx context.Context, queryer sqlQueryer, userID string, now time.Time) error {
	if userID == "" || userID == SystemUserID {
		return nil
	}
	timestamp := plans.FormatTimestamp(now)
	_, err := queryer.ExecContext(ctx,
		"INSERT IGNORE INTO user_memberships (user_id, plan, source, starts_at, expires_at, "+
			"anchor_day, entitlement_version, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, NULL, ?, 1, ?, ?)",
		userID, string(plans.PlanFree), MembershipSourceFree, timestamp, now.UTC().Day(),
		timestamp, timestamp)
	if err != nil {
		return newStateError("会员权益无法保存")
	}
	return nil
}

// migrateMembershipBaseline backfills one Free membership row per existing
// account. It is additive: rows that already exist are never touched.
func (s *Stores) migrateMembershipBaseline(ctx context.Context) error {
	exists, err := s.hasTable(ctx, "users")
	if err != nil {
		return err
	}
	if !exists {
		return nil
	}
	timestamp := plans.FormatTimestamp(time.Now().UTC())
	if _, err := s.DB.ExecContext(ctx,
		"INSERT IGNORE INTO user_memberships (user_id, plan, source, starts_at, expires_at, "+
			"anchor_day, entitlement_version, created_at, updated_at) "+
			"SELECT id, ?, ?, ?, NULL, ?, 1, ?, ? FROM users",
		string(plans.PlanFree), MembershipSourceFree, timestamp, time.Now().UTC().Day(),
		timestamp, timestamp); err != nil {
		return err
	}
	return nil
}

func loadMembershipSnapshot(ctx context.Context, queryer sqlQueryer, userID string, forUpdate bool) (membershipSnapshot, error) {
	query := "SELECT plan, source, starts_at, expires_at, anchor_day, entitlement_version " +
		"FROM user_memberships WHERE user_id = ?"
	if forUpdate {
		query += " FOR UPDATE"
	}
	var snapshot membershipSnapshot
	var plan, source, startsAt string
	var expiresAt sql.NullString
	var anchorDay int
	var version int64
	err := queryer.QueryRowContext(ctx, query, userID).
		Scan(&plan, &source, &startsAt, &expiresAt, &anchorDay, &version)
	if errors.Is(err, sql.ErrNoRows) {
		return membershipSnapshot{}, nil
	}
	if err != nil {
		return membershipSnapshot{}, newStateError("会员权益读取失败")
	}
	snapshot.Plan = plans.Plan(plan)
	snapshot.Source = source
	snapshot.StartsAt = plans.ParseTimestamp(startsAt)
	if expiresAt.Valid {
		snapshot.ExpiresAt = plans.ParseTimestamp(expiresAt.String)
	}
	snapshot.AnchorDay = anchorDay
	snapshot.EntitlementVersion = version
	return snapshot, nil
}

// loadTerms reads every term of one account, oldest first. Callers filter by
// status and instant in Go so a malformed stored timestamp degrades to "not in
// force" instead of a wrong SQL range match.
func loadTerms(ctx context.Context, queryer sqlQueryer, userID string) ([]MembershipTerm, error) {
	rows, err := queryer.QueryContext(ctx,
		"SELECT "+membershipTermColumns+" FROM membership_terms WHERE user_id = ? "+
			"ORDER BY starts_at, id", userID)
	if err != nil {
		return nil, newStateError("会员权益时段读取失败")
	}
	defer func() { _ = rows.Close() }()
	terms := make([]MembershipTerm, 0, 4)
	for rows.Next() {
		term, err := scanMembershipTerm(rows)
		if err != nil {
			return nil, newStateError("会员权益时段读取失败")
		}
		terms = append(terms, term)
	}
	if err := rows.Err(); err != nil {
		return nil, newStateError("会员权益时段读取失败")
	}
	return terms, nil
}

// membershipState derives the currently effective plan and the furthest paid
// expiry from one account's terms. A term that has ended or was refunded never
// contributes; a term that has not started yet only extends the expiry.
func membershipState(terms []MembershipTerm, now time.Time) (plans.Plan, time.Time) {
	plan := plans.PlanFree
	var expires time.Time
	for _, term := range terms {
		if term.Status != MembershipTermActive || !term.EndsAt.After(now) {
			continue
		}
		if term.EndsAt.After(expires) {
			expires = term.EndsAt
		}
		if term.StartsAt.After(now) {
			continue
		}
		if plans.PlanRank(term.Plan) > plans.PlanRank(plan) {
			plan = term.Plan
		}
	}
	return plan, expires
}

// baseTermForNow returns the purchase/renew term that covers now. Upgrade terms
// are overrides and do not define a membership month.
func baseTermForNow(terms []MembershipTerm, now time.Time) (MembershipTerm, bool) {
	var base MembershipTerm
	found := false
	for _, term := range terms {
		if term.Status != MembershipTermActive || term.Kind == MembershipKindUpgrade {
			continue
		}
		if term.StartsAt.After(now) || !term.EndsAt.After(now) {
			continue
		}
		if !found || term.StartsAt.After(base.StartsAt) {
			base = term
			found = true
		}
	}
	return base, found
}

// membershipPeriod is the embedding allowance window currently in force.
type membershipPeriod struct {
	Source string
	Plan   plans.Plan
	Start  time.Time
	End    time.Time
}

func allowanceForPeriod(period membershipPeriod) int64 {
	if period.Source == MembershipSourceSystem {
		return systemEmbeddingAllowance
	}
	return plans.ProductFor(period.Plan).EmbeddingCharsPerMonth
}

// membershipPeriodFor resolves the allowance window: the membership month of
// the base paid term, or the Shanghai natural month while Free. A paid override
// without a base term falls back to Free.
func membershipPeriodFor(ctx context.Context, queryer sqlQueryer, userID string, now time.Time) (membershipPeriod, error) {
	if userID == SystemUserID {
		start, end := plans.FreeMonthBounds(now)
		return membershipPeriod{Source: MembershipSourceSystem, Plan: plans.PlanPro, Start: start, End: end}, nil
	}
	if err := ensureMembershipRow(ctx, queryer, userID, now); err != nil {
		return membershipPeriod{}, err
	}
	terms, err := loadTerms(ctx, queryer, userID)
	if err != nil {
		return membershipPeriod{}, err
	}
	plan, _ := membershipState(terms, now)
	if plan != plans.PlanFree {
		if base, ok := baseTermForNow(terms, now); ok {
			if index := plans.CurrentPeriodIndex(base.StartsAt, now); index >= 0 {
				start, end := plans.PeriodBounds(base.StartsAt, index)
				return membershipPeriod{Source: MembershipSourcePaid, Plan: plan, Start: start, End: end}, nil
			}
		}
	}
	start, end := plans.FreeMonthBounds(now)
	return membershipPeriod{Source: MembershipSourceFree, Plan: plans.PlanFree, Start: start, End: end}, nil
}

// systemEntitlement is the platform account's effective entitlement: Pro-shaped
// limits plus an effectively unlimited embedding ledger.
func systemEntitlement(now time.Time) Entitlement {
	start, end := plans.FreeMonthBounds(now)
	limits := plans.ProductFor(plans.PlanPro)
	limits.EmbeddingCharsPerMonth = systemEmbeddingAllowance
	return Entitlement{
		UserID:             SystemUserID,
		Plan:               plans.PlanPro,
		Source:             MembershipSourceSystem,
		StartsAt:           start,
		AnchorDay:          now.UTC().Day(),
		EntitlementVersion: 0,
		RefreshAt:          end,
		Limits:             limits,
	}
}

// updateMembershipSnapshot writes the derived snapshot row. It never changes
// starts_at or anchor_day and only advances the entitlement version when the
// caller (billing) performed a grant.
func updateMembershipSnapshot(ctx context.Context, queryer sqlQueryer, userID string, plan plans.Plan,
	source string, expires time.Time, now time.Time, bumpVersion bool) error {
	versionClause := ""
	if bumpVersion {
		versionClause = ", entitlement_version = entitlement_version + 1"
	}
	if _, err := queryer.ExecContext(ctx,
		"UPDATE user_memberships SET plan = ?, source = ?, expires_at = ?, updated_at = ?"+
			versionClause+" WHERE user_id = ?",
		string(plan), source, nullableTimestamp(expires), plans.FormatTimestamp(now), userID); err != nil {
		return newStateError("会员权益无法保存")
	}
	return nil
}

// effectiveFor is the shared entitlement resolver. It reads the terms (the
// authority) and refreshes the cached snapshot when it drifted.
func effectiveFor(ctx context.Context, queryer sqlQueryer, userID string, now time.Time) (Entitlement, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return Entitlement{}, newValueError("账号不能为空")
	}
	if userID == SystemUserID {
		return systemEntitlement(now), nil
	}
	if err := ensureMembershipRow(ctx, queryer, userID, now); err != nil {
		return Entitlement{}, err
	}
	snapshot, err := loadMembershipSnapshot(ctx, queryer, userID, false)
	if err != nil {
		return Entitlement{}, err
	}
	terms, err := loadTerms(ctx, queryer, userID)
	if err != nil {
		return Entitlement{}, err
	}
	plan, expires := membershipState(terms, now)
	entitlement := Entitlement{
		UserID:             userID,
		Plan:               plan,
		ExpiresAt:          expires,
		EntitlementVersion: snapshot.EntitlementVersion,
		Limits:             plans.ProductFor(plan),
	}
	if plan == plans.PlanFree {
		entitlement.Source = MembershipSourceFree
		start, end := plans.FreeMonthBounds(now)
		entitlement.StartsAt = start
		entitlement.RefreshAt = end
		entitlement.AnchorDay = now.UTC().Day()
	} else {
		entitlement.Source = MembershipSourcePaid
		var latestStart time.Time
		for _, term := range terms {
			if term.Status != MembershipTermActive || !term.EndsAt.After(now) {
				continue
			}
			if term.StartsAt.After(now) {
				continue
			}
			if term.StartsAt.After(latestStart) {
				latestStart = term.StartsAt
			}
		}
		entitlement.StartsAt = latestStart
		entitlement.AnchorDay = shanghaiDay(latestStart)
		entitlement.RefreshAt = expires
		if base, ok := baseTermForNow(terms, now); ok {
			if index := plans.CurrentPeriodIndex(base.StartsAt, now); index >= 0 {
				_, periodEnd := plans.PeriodBounds(base.StartsAt, index)
				entitlement.RefreshAt = periodEnd
			}
		}
	}
	if snapshot.Plan != entitlement.Plan || snapshot.Source != entitlement.Source ||
		!snapshot.ExpiresAt.Equal(entitlement.ExpiresAt) {
		if err := updateMembershipSnapshot(ctx, queryer, userID, entitlement.Plan, entitlement.Source,
			entitlement.ExpiresAt, now, false); err != nil {
			return Entitlement{}, err
		}
	}
	return entitlement, nil
}

// Effective resolves one account's membership at now.
func (r *MembershipRepository) Effective(ctx context.Context, userID string, now time.Time) (Entitlement, error) {
	return effectiveFor(ctx, r.db, userID, normalizeMembershipNow(now))
}

// CanShare reports whether the account's own membership may issue share
// invitations. Administrator/VIP role compatibility stays with the caller.
func (r *MembershipRepository) CanShare(ctx context.Context, userID string, now time.Time) (bool, error) {
	entitlement, err := r.Effective(ctx, userID, now)
	if err != nil {
		return false, err
	}
	return entitlement.Limits.CanShareOwnedKnowledgeBases, nil
}

// Terms lists every membership grant of one account, oldest first.
func (r *MembershipRepository) Terms(ctx context.Context, userID string) ([]MembershipTerm, error) {
	if strings.TrimSpace(userID) == "" {
		return []MembershipTerm{}, nil
	}
	if userID == SystemUserID {
		return []MembershipTerm{}, nil
	}
	return loadTerms(ctx, r.db, userID)
}

// ActiveBaseTerms lists the purchase/renew terms that have not ended yet,
// including a renewal that has not started.
func (r *MembershipRepository) ActiveBaseTerms(ctx context.Context, userID string, now time.Time) ([]MembershipTerm, error) {
	now = normalizeMembershipNow(now)
	if strings.TrimSpace(userID) == "" || userID == SystemUserID {
		return []MembershipTerm{}, nil
	}
	terms, err := loadTerms(ctx, r.db, userID)
	if err != nil {
		return nil, err
	}
	active := make([]MembershipTerm, 0, len(terms))
	for _, term := range terms {
		if term.Status != MembershipTermActive || !term.EndsAt.After(now) {
			continue
		}
		if term.Kind != MembershipKindPurchase && term.Kind != MembershipKindRenew {
			continue
		}
		active = append(active, term)
	}
	return active, nil
}

// QuotaSummary returns the embedding allowance window currently in force.
func (r *MembershipRepository) QuotaSummary(ctx context.Context, userID string, now time.Time) (QuotaSummary, error) {
	now = normalizeMembershipNow(now)
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return QuotaSummary{}, newValueError("账号不能为空")
	}
	period, err := membershipPeriodFor(ctx, r.db, userID, now)
	if err != nil {
		return QuotaSummary{}, err
	}
	summary := QuotaSummary{
		Plan:           period.Plan,
		Source:         period.Source,
		RefreshAt:      period.End,
		PeriodStart:    period.Start,
		PeriodEnd:      period.End,
		AllowanceChars: allowanceForPeriod(period),
	}
	var allowance, used, reserved int64
	err = r.db.QueryRowContext(ctx,
		"SELECT allowance_chars, used_chars, reserved_chars FROM embedding_quota_periods "+
			"WHERE user_id = ? AND source = ? AND period_start = ?",
		userID, period.Source, plans.FormatTimestamp(period.Start)).Scan(&allowance, &used, &reserved)
	if errors.Is(err, sql.ErrNoRows) {
		return summary, nil
	}
	if err != nil {
		return QuotaSummary{}, newStateError("额度账本读取失败")
	}
	summary.AllowanceChars = allowance
	summary.UsedChars = used
	summary.ReservedChars = reserved
	return summary, nil
}

// logicalFileCount counts the logical files of one knowledge base: published
// documents plus admitted jobs that have no published document yet. Names are
// compared case-insensitively (the admission path already rejects a collision),
// so the same logical document never counts twice.
func logicalFileCount(ctx context.Context, queryer sqlQueryer, namespace string) (int, error) {
	const query = "SELECT COUNT(*) FROM (" +
		"SELECT LOWER(name) AS logical_name FROM documents WHERE namespace = ? AND del_flag = 0 " +
		"UNION " +
		"SELECT LOWER(name) AS logical_name FROM document_jobs WHERE namespace = ? AND status IN (?, ?)" +
		") AS logical_files"
	var count int
	if err := queryer.QueryRowContext(ctx, query, namespace, namespace,
		DocumentJobQueued, DocumentJobProcessing).Scan(&count); err != nil {
		return 0, newStateError("知识库文件数读取失败")
	}
	return count, nil
}

// logicalFileCountLocked is the admission-side count. At MySQL's default
// REPEATABLE READ isolation a plain SELECT after waiting for the owner row lock
// would still see the pre-lock snapshot, letting two concurrent admissions both
// pass. Locking reads are current reads, so they observe the transaction that
// just committed and the same logical name is still counted once.
func logicalFileCountLocked(ctx context.Context, tx *sql.Tx, namespace string) (int, error) {
	names := map[string]struct{}{}
	rows, err := tx.QueryContext(ctx,
		"SELECT LOWER(name) FROM documents WHERE namespace = ? AND del_flag = 0 FOR UPDATE", namespace)
	if err != nil {
		return 0, newStateError("知识库文件数读取失败")
	}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			return 0, newStateError("知识库文件数读取失败")
		}
		names[name] = struct{}{}
	}
	if err := rows.Close(); err != nil {
		return 0, newStateError("知识库文件数读取失败")
	}
	rows, err = tx.QueryContext(ctx,
		"SELECT LOWER(name) FROM document_jobs WHERE namespace = ? AND status IN (?, ?) FOR UPDATE",
		namespace, DocumentJobQueued, DocumentJobProcessing)
	if err != nil {
		return 0, newStateError("知识库文件数读取失败")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return 0, newStateError("知识库文件数读取失败")
		}
		names[name] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return 0, newStateError("知识库文件数读取失败")
	}
	return len(names), nil
}

// namespaceOwner returns the owner account of one knowledge base, or "" for an
// unowned (system) knowledge base.
func namespaceOwner(ctx context.Context, queryer sqlQueryer, namespace string) (string, error) {
	var owner sql.NullString
	err := queryer.QueryRowContext(ctx,
		"SELECT owner_user_id FROM knowledge_bases WHERE namespace = ?", namespace).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", newStateError("知识库归属读取失败")
	}
	if !owner.Valid {
		return "", nil
	}
	return owner.String, nil
}

// lockUserRow serializes quota and entitlement changes of one account. A
// missing users row is tolerated (the knowledge base outlives a deleted
// account) and keeps the Free limits.
func lockUserRow(ctx context.Context, tx *sql.Tx, userID string) error {
	if userID == "" || userID == SystemUserID {
		return nil
	}
	var locked string
	err := tx.QueryRowContext(ctx, "SELECT id FROM users WHERE id = ? FOR UPDATE", userID).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return newStateError("账号状态读取失败")
	}
	return nil
}

// buildFileUsage assembles the admission projection from the owner's limit and
// the current count.
func buildFileUsage(namespace, ownerUserID string, used, limit int) FileUsage {
	usage := FileUsage{Namespace: namespace, OwnerUserID: ownerUserID, Used: used, Limit: limit}
	if limit < 0 {
		usage.Unlimited = true
		usage.CanAdd = true
		return usage
	}
	usage.CanAdd = used < limit
	if !usage.CanAdd {
		usage.Reason = fmt.Sprintf("知识库文件数已达上限（%d 个）", limit)
	}
	return usage
}

// FileUsage reports whether the knowledge base may admit one more file. The
// owner's plan is used, not the caller's: a collaborator never spends their own
// file quota on a shared knowledge base.
func (r *MembershipRepository) FileUsage(ctx context.Context, namespace string, now time.Time) (FileUsage, error) {
	now = normalizeMembershipNow(now)
	owner, err := namespaceOwner(ctx, r.db, namespace)
	if err != nil {
		return FileUsage{}, err
	}
	if owner == "" {
		return FileUsage{Namespace: namespace, Unlimited: true, CanAdd: true}, nil
	}
	entitlement, err := r.Effective(ctx, owner, now)
	if err != nil {
		return FileUsage{}, err
	}
	used, err := logicalFileCount(ctx, r.db, namespace)
	if err != nil {
		return FileUsage{}, err
	}
	return buildFileUsage(namespace, owner, used, entitlement.Limits.MaxFilesPerKB), nil
}

// documentQuotaTx is the admission-side check. It runs inside the document job
// transaction: the owner row is locked first so concurrent admissions for the
// same owner cannot both pass, then the count is taken from the same
// transaction that is about to write the job.
func (r *MembershipRepository) documentQuotaTx(ctx context.Context, tx *sql.Tx, namespace string, now time.Time) (FileUsage, error) {
	owner, err := namespaceOwner(ctx, tx, namespace)
	if err != nil {
		return FileUsage{}, err
	}
	if owner == "" {
		return FileUsage{Namespace: namespace, Unlimited: true, CanAdd: true}, nil
	}
	if err := lockUserRow(ctx, tx, owner); err != nil {
		return FileUsage{}, err
	}
	entitlement, err := effectiveFor(ctx, tx, owner, now)
	if err != nil {
		return FileUsage{}, err
	}
	used, err := logicalFileCountLocked(ctx, tx, namespace)
	if err != nil {
		return FileUsage{}, err
	}
	return buildFileUsage(namespace, owner, used, entitlement.Limits.MaxFilesPerKB), nil
}

// RaiseQuotaAllowance raises every running paid allowance window of one account
// to at least the given plan's ceiling. It returns the number of windows the
// statement touched; it is safe to call more than once.
func (r *MembershipRepository) RaiseQuotaAllowance(ctx context.Context, userID string, now time.Time, plan plans.Plan) (int, error) {
	now = normalizeMembershipNow(now)
	userID = strings.TrimSpace(userID)
	if userID == "" || userID == SystemUserID {
		return 0, nil
	}
	product := plans.ProductFor(plan)
	result, err := r.db.ExecContext(ctx,
		"UPDATE embedding_quota_periods SET allowance_chars = GREATEST(allowance_chars, ?), "+
			"plan = ?, updated_at = ? WHERE user_id = ? AND source = ? AND period_end > ?",
		product.EmbeddingCharsPerMonth, string(plan), plans.FormatTimestamp(now), userID,
		MembershipSourcePaid, plans.FormatTimestamp(now))
	if err != nil {
		return 0, newStateError("额度账本无法保存")
	}
	changed, err := result.RowsAffected()
	if err != nil {
		return 0, newStateError("额度账本无法保存")
	}
	return int(changed), nil
}

// embeddingUsageRow is one ledger row read under a row lock.
type embeddingUsageRow struct {
	ID            string
	OperationID   string
	UserID        string
	PeriodID      string
	ReservedChars int64
	UsedChars     int64
	Status        string
}

// embeddingPeriodRow is the quota window of one ledger row.
type embeddingPeriodRow struct {
	ID          string
	Source      string
	Plan        plans.Plan
	Allowance   int64
	Used        int64
	Reserved    int64
	PeriodStart time.Time
	PeriodEnd   time.Time
}

func lockUsageByOperation(ctx context.Context, tx *sql.Tx, operationID string) (embeddingUsageRow, error) {
	var usage embeddingUsageRow
	err := tx.QueryRowContext(ctx,
		"SELECT id, operation_id, user_id, quota_period_id, reserved_chars, used_chars, status "+
			"FROM embedding_usage WHERE operation_id = ? FOR UPDATE", operationID).
		Scan(&usage.ID, &usage.OperationID, &usage.UserID, &usage.PeriodID, &usage.ReservedChars,
			&usage.UsedChars, &usage.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingUsageRow{}, newLookupError("额度预留不存在")
	}
	if err != nil {
		return embeddingUsageRow{}, newStateError("额度账本读取失败")
	}
	return usage, nil
}

func lockPeriodByID(ctx context.Context, tx *sql.Tx, periodID string) (embeddingPeriodRow, error) {
	var period embeddingPeriodRow
	var plan, start, end string
	err := tx.QueryRowContext(ctx,
		"SELECT id, source, plan, allowance_chars, used_chars, reserved_chars, period_start, period_end "+
			"FROM embedding_quota_periods WHERE id = ? FOR UPDATE", periodID).
		Scan(&period.ID, &period.Source, &plan, &period.Allowance, &period.Used, &period.Reserved,
			&start, &end)
	if errors.Is(err, sql.ErrNoRows) {
		return embeddingPeriodRow{}, newLookupError("额度周期不存在")
	}
	if err != nil {
		return embeddingPeriodRow{}, newStateError("额度账本读取失败")
	}
	period.Plan = plans.Plan(plan)
	period.PeriodStart = plans.ParseTimestamp(start)
	period.PeriodEnd = plans.ParseTimestamp(end)
	return period, nil
}

func buildEmbeddingReservation(operationID string, chars int64, period embeddingPeriodRow) EmbeddingReservation {
	return EmbeddingReservation{
		OperationID:    operationID,
		PeriodID:       period.ID,
		Chars:          chars,
		AllowanceChars: period.Allowance,
		UsedChars:      period.Used,
		ReservedChars:  period.Reserved,
		PeriodStart:    period.PeriodStart,
		PeriodEnd:      period.PeriodEnd,
		RefreshAt:      period.PeriodEnd,
	}
}

// ReserveEmbedding holds embedding characters for one logical operation. The
// whole decision runs in one transaction: the account snapshot row is locked,
// the allowance window is created or raised, and the operation identity is
// checked before anything is charged. A retry of the same OperationID reuses
// the existing ledger row and is never charged twice.
func (r *MembershipRepository) ReserveEmbedding(ctx context.Context, input EmbeddingReservationInput) (EmbeddingReservation, error) {
	reservation, err := r.reserveEmbedding(ctx, input)
	if err != nil && isUniqueViolation(err) {
		// A concurrent retry of the same operation won the insert; the second
		// pass finds its ledger row and returns it as a reuse.
		return r.reserveEmbedding(ctx, input)
	}
	return reservation, err
}

func (r *MembershipRepository) reserveEmbedding(ctx context.Context, input EmbeddingReservationInput) (EmbeddingReservation, error) {
	now := normalizeMembershipNow(input.Now)
	userID := strings.TrimSpace(input.UserID)
	operationID := strings.TrimSpace(input.OperationID)
	if userID == "" {
		return EmbeddingReservation{}, newValueError("账号不能为空")
	}
	if operationID == "" {
		return EmbeddingReservation{}, newValueError("向量化操作标识不能为空")
	}
	if input.Chars < 0 {
		return EmbeddingReservation{}, newValueError("向量化字符数不能为负数")
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return EmbeddingReservation{}, newStateError("额度预留失败")
	}
	defer func() { _ = transaction.Rollback() }()
	if userID != SystemUserID {
		if err := ensureMembershipRow(ctx, transaction, userID, now); err != nil {
			return EmbeddingReservation{}, err
		}
		if _, err := loadMembershipSnapshot(ctx, transaction, userID, true); err != nil {
			return EmbeddingReservation{}, err
		}
	}
	period, err := membershipPeriodFor(ctx, transaction, userID, now)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	allowance := allowanceForPeriod(period)
	periodStart := plans.FormatTimestamp(period.Start)
	periodEnd := plans.FormatTimestamp(period.End)
	timestamp := plans.FormatTimestamp(now)
	periodID, err := newStateID()
	if err != nil {
		return EmbeddingReservation{}, newStateError("额度预留失败")
	}
	// The catalog allowance is a floor: an upgrade raises it, a renewal of the
	// same tier never lowers a window that already granted more.
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO embedding_quota_periods (id, user_id, source, period_start, period_end, plan, "+
			"allowance_chars, used_chars, reserved_chars, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, 0, 0, ?, ?) "+
			"ON DUPLICATE KEY UPDATE allowance_chars = GREATEST(allowance_chars, VALUES(allowance_chars)), "+
			"plan = IF(FIELD(VALUES(plan), 'pro', 'plus', 'free') <= FIELD(plan, 'pro', 'plus', 'free'), "+
			"VALUES(plan), plan), updated_at = VALUES(updated_at)",
		periodID, userID, period.Source, periodStart, periodEnd, string(period.Plan), allowance,
		timestamp, timestamp); err != nil {
		return EmbeddingReservation{}, newStateError("额度账本无法保存")
	}
	var storedPeriodID string
	var storedPlan, storedEnd string
	var storedAllowance, storedUsed, storedReserved int64
	if err := transaction.QueryRowContext(ctx,
		"SELECT id, plan, allowance_chars, used_chars, reserved_chars, period_end "+
			"FROM embedding_quota_periods WHERE user_id = ? AND source = ? AND period_start = ? FOR UPDATE",
		userID, period.Source, periodStart).
		Scan(&storedPeriodID, &storedPlan, &storedAllowance, &storedUsed, &storedReserved, &storedEnd); err != nil {
		return EmbeddingReservation{}, newStateError("额度账本读取失败")
	}
	var existingID, existingPeriodID, existingStatus string
	var existingReserved, existingUsed int64
	err = transaction.QueryRowContext(ctx,
		"SELECT id, quota_period_id, reserved_chars, used_chars, status FROM embedding_usage "+
			"WHERE operation_id = ?", operationID).
		Scan(&existingID, &existingPeriodID, &existingReserved, &existingUsed, &existingStatus)
	if err == nil {
		// The operation already owns a ledger row. Return that ledger (which
		// may belong to an earlier allowance window) and charge nothing.
		original, err := lockPeriodByID(ctx, transaction, existingPeriodID)
		if err != nil {
			return EmbeddingReservation{}, err
		}
		reservation := buildEmbeddingReservation(operationID, existingReserved, original)
		reservation.Reused = true
		return reservation, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return EmbeddingReservation{}, newStateError("额度账本读取失败")
	}
	available := storedAllowance - storedUsed - storedReserved
	if input.Chars > available {
		refreshAt := plans.ParseTimestamp(storedEnd)
		return EmbeddingReservation{}, &QuotaExceededError{
			Kind:      "embedding",
			Required:  input.Chars,
			Available: available,
			RefreshAt: refreshAt,
			Message:   "所有者的文档向量化额度不足",
		}
	}
	usageID, err := newStateID()
	if err != nil {
		return EmbeddingReservation{}, newStateError("额度预留失败")
	}
	expiresAt := now.Add(EmbeddingReservationLease)
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO embedding_usage (id, operation_id, user_id, quota_period_id, namespace, "+
			"document_name, document_version, model, purpose, reserved_chars, used_chars, status, "+
			"expires_at, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)",
		usageID, operationID, userID, storedPeriodID, input.Namespace, input.DocumentName,
		input.DocumentVersion, input.Model, input.Purpose, input.Chars, EmbeddingUsageReserved,
		plans.FormatTimestamp(expiresAt), timestamp, timestamp); err != nil {
		if isUniqueViolation(err) {
			return EmbeddingReservation{}, err
		}
		return EmbeddingReservation{}, newStateError("额度预留无法保存")
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_quota_periods SET reserved_chars = reserved_chars + ?, updated_at = ? WHERE id = ?",
		input.Chars, timestamp, storedPeriodID); err != nil {
		return EmbeddingReservation{}, newStateError("额度预留无法保存")
	}
	if err := transaction.Commit(); err != nil {
		return EmbeddingReservation{}, newStateError("额度预留失败")
	}
	return EmbeddingReservation{
		OperationID:    operationID,
		PeriodID:       storedPeriodID,
		Chars:          input.Chars,
		AllowanceChars: storedAllowance,
		UsedChars:      storedUsed,
		ReservedChars:  storedReserved + input.Chars,
		PeriodStart:    period.Start,
		PeriodEnd:      plans.ParseTimestamp(storedEnd),
		RefreshAt:      plans.ParseTimestamp(storedEnd),
	}, nil
}

// SettleEmbedding converts a reservation into used characters. It is
// idempotent once settled: a repeated callback never charges again. When the
// operation is in pending_review (an earlier attempt had an unknown result) the
// retry total only adds the increment above what was already counted and
// releases the remainder of the hold.
func (r *MembershipRepository) SettleEmbedding(ctx context.Context, operationID string, usedChars int64) (EmbeddingReservation, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return EmbeddingReservation{}, newValueError("向量化操作标识不能为空")
	}
	if usedChars < 0 {
		usedChars = 0
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	defer func() { _ = transaction.Rollback() }()
	usage, err := lockUsageByOperation(ctx, transaction, operationID)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	period, err := lockPeriodByID(ctx, transaction, usage.PeriodID)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	if usage.Status == EmbeddingUsageSettled || usage.Status == EmbeddingUsageReleased {
		return buildEmbeddingReservation(operationID, usage.ReservedChars, period), nil
	}
	held := usage.ReservedChars
	target := usage.UsedChars
	if usedChars > target {
		target = usedChars
	}
	if limit := usage.UsedChars + held; target > limit {
		target = limit
	}
	delta := target - usage.UsedChars
	now := r.now()
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_quota_periods SET used_chars = used_chars + ?, "+
			"reserved_chars = GREATEST(reserved_chars - ?, 0), updated_at = ? WHERE id = ?",
		delta, held, plans.FormatTimestamp(now), period.ID); err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_usage SET used_chars = ?, status = ?, updated_at = ? WHERE id = ?",
		target, EmbeddingUsageSettled, plans.FormatTimestamp(now), usage.ID); err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	if err := transaction.Commit(); err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	period.Used += delta
	period.Reserved -= held
	if period.Reserved < 0 {
		period.Reserved = 0
	}
	return buildEmbeddingReservation(operationID, held, period), nil
}

// PendingReviewEmbedding keeps an operation whose upstream result is unknown.
// The already confirmed characters become used, the unconfirmed remainder stays
// reserved and the hold gets a fresh 24 hour lease. Repeating the call for a row
// that is already pending/settled/released changes nothing.
func (r *MembershipRepository) PendingReviewEmbedding(ctx context.Context, operationID string, usedChars int64) (EmbeddingReservation, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return EmbeddingReservation{}, newValueError("向量化操作标识不能为空")
	}
	if usedChars < 0 {
		usedChars = 0
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	defer func() { _ = transaction.Rollback() }()
	usage, err := lockUsageByOperation(ctx, transaction, operationID)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	period, err := lockPeriodByID(ctx, transaction, usage.PeriodID)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	if usage.Status != EmbeddingUsageReserved {
		return buildEmbeddingReservation(operationID, usage.ReservedChars, period), nil
	}
	held := usage.ReservedChars
	counted := usedChars
	if counted > held {
		counted = held
	}
	now := r.now()
	timestamp := plans.FormatTimestamp(now)
	delta := counted
	status := EmbeddingUsagePendingReview
	remaining := held - counted
	if remaining <= 0 {
		// The whole hold was confirmed: settle it in one step.
		status = EmbeddingUsageSettled
		remaining = 0
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_quota_periods SET used_chars = used_chars + ?, "+
			"reserved_chars = GREATEST(reserved_chars - ?, 0), updated_at = ? WHERE id = ?",
		delta, delta, timestamp, period.ID); err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	expiresAt := now.Add(EmbeddingReservationLease)
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_usage SET used_chars = ?, reserved_chars = ?, status = ?, "+
			"expires_at = ?, updated_at = ? WHERE id = ?",
		counted, remaining, status, plans.FormatTimestamp(expiresAt), timestamp, usage.ID); err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	if err := transaction.Commit(); err != nil {
		return EmbeddingReservation{}, newStateError("额度结算失败")
	}
	period.Used += delta
	period.Reserved -= delta
	if period.Reserved < 0 {
		period.Reserved = 0
	}
	return buildEmbeddingReservation(operationID, remaining, period), nil
}

// ReleaseEmbedding gives back the still-held characters of one operation. A
// settled or already released operation is left untouched.
func (r *MembershipRepository) ReleaseEmbedding(ctx context.Context, operationID string) (EmbeddingReservation, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return EmbeddingReservation{}, newValueError("向量化操作标识不能为空")
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return EmbeddingReservation{}, newStateError("额度释放失败")
	}
	defer func() { _ = transaction.Rollback() }()
	usage, err := lockUsageByOperation(ctx, transaction, operationID)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	period, err := lockPeriodByID(ctx, transaction, usage.PeriodID)
	if err != nil {
		return EmbeddingReservation{}, err
	}
	if usage.Status != EmbeddingUsageReserved && usage.Status != EmbeddingUsagePendingReview {
		return buildEmbeddingReservation(operationID, usage.ReservedChars, period), nil
	}
	held := usage.ReservedChars
	now := r.now()
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_quota_periods SET reserved_chars = GREATEST(reserved_chars - ?, 0), "+
			"updated_at = ? WHERE id = ?", held, plans.FormatTimestamp(now), period.ID); err != nil {
		return EmbeddingReservation{}, newStateError("额度释放失败")
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE embedding_usage SET status = ?, updated_at = ? WHERE id = ?",
		EmbeddingUsageReleased, plans.FormatTimestamp(now), usage.ID); err != nil {
		return EmbeddingReservation{}, newStateError("额度释放失败")
	}
	if err := transaction.Commit(); err != nil {
		return EmbeddingReservation{}, newStateError("额度释放失败")
	}
	period.Reserved -= held
	if period.Reserved < 0 {
		period.Reserved = 0
	}
	return buildEmbeddingReservation(operationID, held, period), nil
}

// ReleaseStaleEmbedding releases every reservation or pending-review hold whose
// lease ended at or before before, returning the number of released rows. One
// transaction releases the whole batch so a crash cannot leave half a window
// reconciled.
func (r *MembershipRepository) ReleaseStaleEmbedding(ctx context.Context, before time.Time) (int, error) {
	before = normalizeMembershipNow(before)
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, newStateError("额度释放失败")
	}
	defer func() { _ = transaction.Rollback() }()
	rows, err := transaction.QueryContext(ctx,
		"SELECT id, quota_period_id, reserved_chars FROM embedding_usage "+
			"WHERE status IN (?, ?) AND expires_at <= ? ORDER BY expires_at",
		EmbeddingUsageReserved, EmbeddingUsagePendingReview, plans.FormatTimestamp(before))
	if err != nil {
		return 0, newStateError("额度释放失败")
	}
	type staleUsage struct {
		id       string
		periodID string
		reserved int64
	}
	stale := make([]staleUsage, 0, 4)
	for rows.Next() {
		var item staleUsage
		if err := rows.Scan(&item.id, &item.periodID, &item.reserved); err != nil {
			_ = rows.Close()
			return 0, newStateError("额度释放失败")
		}
		stale = append(stale, item)
	}
	if err := rows.Close(); err != nil {
		return 0, newStateError("额度释放失败")
	}
	if len(stale) == 0 {
		return 0, nil
	}
	timestamp := plans.FormatTimestamp(r.now())
	for _, item := range stale {
		if _, err := transaction.ExecContext(ctx,
			"UPDATE embedding_quota_periods SET reserved_chars = GREATEST(reserved_chars - ?, 0), "+
				"updated_at = ? WHERE id = ?", item.reserved, timestamp, item.periodID); err != nil {
			return 0, newStateError("额度释放失败")
		}
		if _, err := transaction.ExecContext(ctx,
			"UPDATE embedding_usage SET status = ?, updated_at = ? WHERE id = ?",
			EmbeddingUsageReleased, timestamp, item.id); err != nil {
			return 0, newStateError("额度释放失败")
		}
	}
	if err := transaction.Commit(); err != nil {
		return 0, newStateError("额度释放失败")
	}
	return len(stale), nil
}
