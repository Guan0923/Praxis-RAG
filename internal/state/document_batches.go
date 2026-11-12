package state

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"strings"
	"time"
)

// Document batch lifecycle. Only two statuses are stored: "saving" while the
// per-file admissions run and "active" once the batch row has settled. The
// Console-facing projection derives processing/completed/failed/interrupted on
// read, so a batch never needs a background worker to advance its state.
const (
	DocumentBatchSaving = "saving"
	DocumentBatchActive = "active"
)

// Derived document batch statuses (read projection only, never stored).
const (
	DocumentBatchProcessing  = "processing"
	DocumentBatchCompleted   = "completed"
	DocumentBatchFailed      = "failed"
	DocumentBatchInterrupted = "interrupted"
)

// Document batch item statuses.
const (
	DocumentBatchItemSaving   = "saving"
	DocumentBatchItemAccepted = "accepted"
	DocumentBatchItemSkipped  = "skipped"
	DocumentBatchItemFailed   = "failed"
)

// Document batch item reasons. Skipped rows explain why the file was not
// queued; failed rows narrow the safe error category for the Console.
const (
	DocumentBatchReasonExists     = "exists"
	DocumentBatchReasonQueued     = "queued"
	DocumentBatchReasonDuplicate  = "duplicate"
	DocumentBatchReasonValidation = "validation"
	DocumentBatchReasonConfig     = "config"
	DocumentBatchReasonConflict   = "conflict"
	DocumentBatchReasonInternal   = "internal"
)

// documentBatchInterruptSeconds marks a batch that never finished its
// admissions (for example the service restarted mid-batch) as interrupted.
const documentBatchInterruptSeconds = 600

// DocumentBatchItem is one file of a batch upload.
type DocumentBatchItem struct {
	BatchID    string  `json:"batch_id"`
	Position   int     `json:"position"`
	Name       string  `json:"name"`
	Status     string  `json:"status"`
	Reason     *string `json:"reason"`
	ArtifactID *string `json:"artifact_id"`
	Error      *string `json:"error"`
	CreatedAt  float64 `json:"created_at"`
	UpdatedAt  float64 `json:"updated_at"`
	// Job is the asynchronous document job projection for accepted items. It
	// is nil while the admission is still running or after a skipped/failed
	// item.
	Job *DocumentJobStatus `json:"job,omitempty"`
}

// DocumentBatch groups every file of one POST /v1/document-batches request.
type DocumentBatch struct {
	ID             string              `json:"id"`
	Namespace      string              `json:"namespace"`
	Owner          *string             `json:"owner"`
	IdempotencyKey *string             `json:"idempotency_key"`
	Status         string              `json:"status"`
	Total          int                 `json:"total"`
	Accepted       int                 `json:"accepted"`
	Skipped        int                 `json:"skipped"`
	Failed         int                 `json:"failed"`
	CreatedAt      float64             `json:"created_at"`
	UpdatedAt      float64             `json:"updated_at"`
	Items          []DocumentBatchItem `json:"items"`
}

// DocumentBatchItemResult is one admission outcome written by Finish. The zero
// ArtifactID/Error strings mean "not set" and are stored as NULL.
type DocumentBatchItemResult struct {
	Position   int
	Status     string
	Reason     string
	ArtifactID string
	Error      string
}

// DocumentBatches stores the Console batch upload records.
type DocumentBatches struct {
	stores *Stores
	// Clock is overridable in tests; nil means unixSeconds.
	Clock func() float64
}

// NewDocumentBatches builds the repository over an open store.
func NewDocumentBatches(stores *Stores) *DocumentBatches {
	return &DocumentBatches{stores: stores, Clock: unixSeconds}
}

func (d *DocumentBatches) now() float64 {
	if d.Clock == nil {
		return unixSeconds()
	}
	return d.Clock()
}

const documentBatchColumns = "id, namespace, owner, idempotency_key, status, total, " +
	"accepted, skipped, failed, created_at, updated_at"

const documentBatchItemColumns = "batch_id, position, name, status, reason, artifact_id, " +
	"error, created_at, updated_at"

// Create records the batch header and one "saving" item per file. A repeated
// Idempotency-Key returns the original batch (reused=true) instead of a second
// record, so a client retry after a lost response never duplicates the work.
func (d *DocumentBatches) Create(
	ctx context.Context,
	namespace string,
	owner *string,
	idempotencyKey string,
	names []string,
) (*DocumentBatch, bool, error) {
	if len(names) == 0 {
		return nil, false, newValueError("A document batch needs at least one file")
	}
	var key *string
	if idempotencyKey != "" {
		key = &idempotencyKey
	}
	if key != nil {
		existing, err := d.byIdempotencyKey(ctx, namespace, *key)
		if err != nil {
			return nil, false, err
		}
		if existing != "" {
			loaded, loadErr := d.Get(ctx, namespace, existing)
			if loadErr != nil {
				return nil, false, loadErr
			}
			if loaded != nil {
				return loaded, true, nil
			}
		}
	}
	batchID, err := randomHex(16)
	if err != nil {
		return nil, false, err
	}
	now := d.now()
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO document_batches (id, namespace, owner, idempotency_key, status, total, "+
			"accepted, skipped, failed, created_at, updated_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, 0, 0, 0, ?, ?)",
		batchID, namespace, nilIfNil(owner), nilIfNil(key), DocumentBatchSaving,
		len(names), now, now); err != nil {
		if isUniqueViolation(err) && key != nil {
			existing, lookupErr := d.byIdempotencyKey(ctx, namespace, *key)
			if lookupErr != nil {
				return nil, false, lookupErr
			}
			if existing != "" {
				loaded, loadErr := d.Get(ctx, namespace, existing)
				if loadErr != nil {
					return nil, false, loadErr
				}
				if loaded != nil {
					return loaded, true, nil
				}
			}
		}
		return nil, false, err
	}
	for position, name := range names {
		if _, err := transaction.ExecContext(ctx,
			"INSERT INTO document_batch_items (batch_id, position, name, status, reason, "+
				"artifact_id, error, created_at, updated_at) VALUES (?, ?, ?, ?, NULL, NULL, NULL, ?, ?)",
			batchID, position, name, DocumentBatchItemSaving, now, now); err != nil {
			return nil, false, err
		}
	}
	if err := transaction.Commit(); err != nil {
		return nil, false, err
	}
	batch, err := d.Get(ctx, namespace, batchID)
	if err != nil {
		return nil, false, err
	}
	return batch, false, nil
}

// Finish persists every per-file admission outcome and settles the batch
// header in one transaction.
func (d *DocumentBatches) Finish(
	ctx context.Context,
	namespace, batchID string,
	results []DocumentBatchItemResult,
) error {
	now := d.now()
	accepted, skipped, failed := 0, 0, 0
	for _, result := range results {
		switch result.Status {
		case DocumentBatchItemAccepted:
			accepted++
		case DocumentBatchItemSkipped:
			skipped++
		case DocumentBatchItemFailed:
			failed++
		}
	}
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	for _, result := range results {
		if _, err := transaction.ExecContext(ctx,
			"UPDATE document_batch_items SET status = ?, reason = ?, artifact_id = ?, "+
				"error = ?, updated_at = ? WHERE batch_id = ? AND position = ?",
			result.Status,
			nullableText(result.Reason),
			nullableText(result.ArtifactID),
			nullableText(result.Error),
			now, batchID, result.Position); err != nil {
			return err
		}
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE document_batches SET status = ?, accepted = ?, skipped = ?, failed = ?, "+
			"updated_at = ? WHERE id = ? AND namespace = ?",
		DocumentBatchActive, accepted, skipped, failed, now, batchID, namespace); err != nil {
		return err
	}
	return transaction.Commit()
}

// Get loads one batch with its items and the derived Console status. A batch
// in another namespace is reported as absent.
func (d *DocumentBatches) Get(ctx context.Context, namespace, batchID string) (*DocumentBatch, error) {
	batch, err := d.scanBatch(d.stores.DB.QueryRowContext(ctx,
		"SELECT "+documentBatchColumns+" FROM document_batches WHERE id = ? AND namespace = ?",
		batchID, namespace))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	if batch == nil {
		return nil, nil
	}
	if err := d.loadItems(ctx, batch); err != nil {
		return nil, err
	}
	return batch, nil
}

// ListActive returns the unfinished batches of one namespace, newest first.
// A nil owner keeps the legacy API-key visibility (every batch of the
// namespace); a non-nil owner restricts the list to that Console account.
func (d *DocumentBatches) ListActive(
	ctx context.Context,
	namespace string,
	owner *string,
	limit int,
) ([]*DocumentBatch, error) {
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}
	query := "SELECT " + documentBatchColumns + " FROM document_batches " +
		"WHERE namespace = ? AND status IN (?, ?)"
	arguments := []any{namespace, DocumentBatchSaving, DocumentBatchActive}
	if owner != nil {
		query += " AND owner = ?"
		arguments = append(arguments, *owner)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	arguments = append(arguments, limit)
	rows, err := d.stores.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	batches := make([]*DocumentBatch, 0, limit)
	for rows.Next() {
		batch, scanErr := d.scanBatch(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		if batch != nil {
			batches = append(batches, batch)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, batch := range batches {
		if err := d.loadItems(ctx, batch); err != nil {
			return nil, err
		}
	}
	return batches, nil
}

// Prune deletes settled batch metadata older than the cutoff. File bytes were
// never stored here (they live in document_jobs), so nothing durable is lost.
func (d *DocumentBatches) Prune(ctx context.Context, cutoff float64) (int, error) {
	rows, err := d.stores.DB.QueryContext(ctx,
		"SELECT id FROM document_batches WHERE created_at < ? LIMIT 500", cutoff)
	if err != nil {
		return 0, err
	}
	ids := make([]string, 0, 16)
	for rows.Next() {
		var id string
		if scanErr := rows.Scan(&id); scanErr != nil {
			_ = rows.Close()
			return 0, scanErr
		}
		ids = append(ids, id)
	}
	if closeErr := rows.Close(); closeErr != nil {
		return 0, closeErr
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if len(ids) == 0 {
		return 0, nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, 0, len(ids))
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	transaction, err := d.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM document_batch_items WHERE batch_id IN ("+placeholders+")", arguments...); err != nil {
		return 0, err
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM document_batches WHERE id IN ("+placeholders+") AND created_at < ?",
		append(arguments, cutoff)...); err != nil {
		return 0, err
	}
	if err := transaction.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

// RunRetention prunes expired batch metadata until ctx is canceled.
func (d *DocumentBatches) RunRetention(ctx context.Context, olderThan, interval time.Duration) {
	if interval <= 0 {
		interval = time.Hour
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		if _, err := d.Prune(context.Background(), d.now()-olderThan.Seconds()); err != nil {
			log.Printf("Document batch retention failed error_type=%s", errorTypeName(err))
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

// nullableText stores an empty string as SQL NULL.
func nullableText(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func (d *DocumentBatches) byIdempotencyKey(ctx context.Context, namespace, key string) (string, error) {
	var batchID string
	err := d.stores.DB.QueryRowContext(ctx,
		"SELECT id FROM document_batches WHERE namespace = ? AND idempotency_key = ?",
		namespace, key).Scan(&batchID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return batchID, nil
}

func (d *DocumentBatches) scanBatch(scanner interface{ Scan(...any) error }) (*DocumentBatch, error) {
	var (
		batch          DocumentBatch
		owner          sql.NullString
		idempotencyKey sql.NullString
	)
	if err := scanner.Scan(&batch.ID, &batch.Namespace, &owner, &idempotencyKey, &batch.Status,
		&batch.Total, &batch.Accepted, &batch.Skipped, &batch.Failed,
		&batch.CreatedAt, &batch.UpdatedAt); err != nil {
		return nil, err
	}
	batch.Owner = nullStringPointer(owner)
	batch.IdempotencyKey = nullStringPointer(idempotencyKey)
	return &batch, nil
}

func (d *DocumentBatches) loadItems(ctx context.Context, batch *DocumentBatch) error {
	rows, err := d.stores.DB.QueryContext(ctx,
		"SELECT "+documentBatchItemColumns+" FROM document_batch_items "+
			"WHERE batch_id = ? ORDER BY position", batch.ID)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	items := make([]DocumentBatchItem, 0, batch.Total)
	for rows.Next() {
		var (
			item       DocumentBatchItem
			reason     sql.NullString
			artifactID sql.NullString
			errorText  sql.NullString
		)
		if err := rows.Scan(&item.BatchID, &item.Position, &item.Name, &item.Status, &reason,
			&artifactID, &errorText, &item.CreatedAt, &item.UpdatedAt); err != nil {
			return err
		}
		item.Reason = nullStringPointer(reason)
		item.ArtifactID = nullStringPointer(artifactID)
		item.Error = nullStringPointer(errorText)
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	batch.Items = items
	if err := d.attachJobs(ctx, batch.Items); err != nil {
		return err
	}
	batch.Status = d.deriveStatus(batch)
	return nil
}

// attachJobs joins the accepted items with their document job status, so one
// batch read shows both the admission outcome and the processing stage.
func (d *DocumentBatches) attachJobs(ctx context.Context, items []DocumentBatchItem) error {
	ids := make([]string, 0, len(items))
	for index := range items {
		if items[index].ArtifactID != nil && *items[index].ArtifactID != "" {
			ids = append(ids, *items[index].ArtifactID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
	arguments := make([]any, 0, len(ids))
	for _, id := range ids {
		arguments = append(arguments, id)
	}
	rows, err := d.stores.DB.QueryContext(ctx,
		"SELECT artifact_id, status, stage, error FROM document_jobs WHERE artifact_id IN ("+
			placeholders+")", arguments...)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	statuses := make(map[string]*DocumentJobStatus, len(ids))
	for rows.Next() {
		var (
			status DocumentJobStatus
			text   sql.NullString
		)
		if err := rows.Scan(&status.ArtifactID, &status.Status, &status.Stage, &text); err != nil {
			return err
		}
		status.Error = nullStringPointer(text)
		statuses[status.ArtifactID] = &status
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for index := range items {
		if items[index].ArtifactID == nil {
			continue
		}
		if status, found := statuses[*items[index].ArtifactID]; found {
			items[index].Job = status
		}
	}
	return nil
}

// deriveStatus turns the stored lifecycle into the Console-facing status.
func (d *DocumentBatches) deriveStatus(batch *DocumentBatch) string {
	switch batch.Status {
	case DocumentBatchSaving:
		if d.now()-batch.UpdatedAt > documentBatchInterruptSeconds {
			return DocumentBatchInterrupted
		}
		return DocumentBatchSaving
	case DocumentBatchActive:
		if batch.Accepted == 0 {
			// Nothing was queued: an all-skipped batch (existing names) still
			// completed its work, only an actual failure makes it failed.
			if batch.Failed > 0 {
				return DocumentBatchFailed
			}
			return DocumentBatchCompleted
		}
		for _, item := range batch.Items {
			if item.Status != DocumentBatchItemAccepted {
				continue
			}
			if item.Job == nil {
				return DocumentBatchProcessing
			}
			if item.Job.Status != DocumentJobSucceeded && item.Job.Status != DocumentJobFailed {
				return DocumentBatchProcessing
			}
		}
		return DocumentBatchCompleted
	default:
		return batch.Status
	}
}
