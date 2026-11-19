package state

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

// KnowledgeBasePurgeReport is the outcome of one batched namespace purge. The
// purge runs as a single transaction: every namespace-scoped row is deleted with
// constant SQL statement count, independent of the number of documents or
// namespaces, and content_objects rows disappear only when no reference row
// points at them anymore.
type KnowledgeBasePurgeReport struct {
	// Present marks namespaces whose knowledge_bases row existed.
	Present map[string]bool
	// Busy marks namespaces skipped because an upload still holds an active
	// lease; their data is untouched.
	Busy map[string]bool
	// Collections maps every present namespace to its Qdrant collection name,
	// captured before the configuration rows are deleted.
	Collections map[string]string
	// DocumentsByNamespace counts the documents that were still visible,
	// gathered before they are soft-deleted.
	DocumentsByNamespace  map[string]int
	DocumentsDeleted      int
	ConfigurationsDeleted int
}

// Purge removes every namespace-scoped row of the given knowledge bases in one
// transaction. The statement count is fixed (~30) no matter how many documents,
// operations or namespaces are involved, so a batch-delete request costs one
// round-trip set instead of one per document.
//
// A namespace is only purged after its exclusive lease was taken in Redis and
// fenced in MySQL; a live document write rejects the purge with the
// pre-migration "busy" outcome. The MySQL expires_at alone never proves that a
// running upload is gone.
func (r *KnowledgeBaseRepository) Purge(ctx context.Context, namespaces []string) (KnowledgeBasePurgeReport, error) {
	report := KnowledgeBasePurgeReport{
		Present:              map[string]bool{},
		Busy:                 map[string]bool{},
		Collections:          map[string]string{},
		DocumentsByNamespace: map[string]int{},
	}
	names := dedupeNonEmpty(namespaces)
	if len(names) == 0 {
		return report, nil
	}
	journal := r.purgeJournal()
	if journal == nil {
		return report, newValueError("document lease backend is not available")
	}
	held := map[string]*LockToken{}
	for _, namespace := range names {
		token, err := journal.AcquireLock(ctx, namespace, nil)
		if err != nil {
			var conflict *UploadConflict
			if errors.As(err, &conflict) {
				report.Busy[namespace] = true
				continue
			}
			releasePurgeLocks(journal, held)
			return report, err
		}
		held[namespace] = token
	}
	if len(held) == 0 {
		return report, nil
	}
	defer releasePurgeLocks(journal, held)
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return report, err
	}
	defer func() { _ = tx.Rollback() }()

	var active []string
	active = make([]string, 0, len(held))
	for _, namespace := range names {
		if token, ok := held[namespace]; ok {
			if err := journal.AssertLockTx(ctx, tx, token); err != nil {
				return report, err
			}
			active = append(active, namespace)
		}
	}
	if len(active) == 0 {
		if err := tx.Rollback(); err != nil {
			return report, err
		}
		return report, nil
	}
	arguments := stringArguments(active)
	inClause := inPlaceholders(len(active))

	rows, err := tx.QueryContext(ctx,
		"SELECT namespace, collection FROM knowledge_bases WHERE namespace IN ("+inClause+")", arguments...)
	if err != nil {
		return report, err
	}
	for rows.Next() {
		var namespace, collection string
		if err := rows.Scan(&namespace, &collection); err != nil {
			_ = rows.Close()
			return report, err
		}
		report.Present[namespace] = true
		if collection != "" {
			report.Collections[namespace] = collection
		}
	}
	if err := rows.Close(); err != nil {
		return report, err
	}

	countRows, err := tx.QueryContext(ctx,
		"SELECT namespace, COUNT(*) FROM documents WHERE namespace IN ("+inClause+") AND del_flag = 0 GROUP BY namespace",
		arguments...)
	if err != nil {
		return report, err
	}
	for countRows.Next() {
		var namespace string
		var count int
		if err := countRows.Scan(&namespace, &count); err != nil {
			_ = countRows.Close()
			return report, err
		}
		report.DocumentsByNamespace[namespace] = count
	}
	if err := countRows.Close(); err != nil {
		return report, err
	}

	result, err := tx.ExecContext(ctx,
		"UPDATE documents SET del_flag = 1 WHERE namespace IN ("+inClause+") AND del_flag = 0", arguments...)
	if err != nil {
		return report, err
	}
	deleted, err := result.RowsAffected()
	if err != nil {
		return report, err
	}
	report.DocumentsDeleted = int(deleted)

	if err := r.purgeNamespaceRows(ctx, tx, active, arguments, inClause); err != nil {
		return report, err
	}

	result, err = tx.ExecContext(ctx,
		"DELETE FROM knowledge_bases WHERE namespace IN ("+inClause+")", arguments...)
	if err != nil {
		return report, err
	}
	configurations, err := result.RowsAffected()
	if err != nil {
		return report, err
	}
	report.ConfigurationsDeleted = int(configurations)

	if err := r.purgeContentAndCache(ctx, tx, active, arguments, inClause); err != nil {
		return report, err
	}
	if err := tx.Commit(); err != nil {
		return report, err
	}
	return report, nil
}

// purgeJournal returns the upload journal when the store attached one.
func (r *KnowledgeBaseRepository) purgeJournal() *UploadJournal {
	if r.cacheSync == nil || r.cacheSync.stores == nil {
		return nil
	}
	return r.cacheSync.stores.Uploads
}

// releasePurgeLocks releases every exclusive lease a purge acquired. It uses a
// detached context because the request context may already be cancelled.
func releasePurgeLocks(journal *UploadJournal, held map[string]*LockToken) {
	if journal == nil {
		return
	}
	ctx := context.WithoutCancel(context.Background())
	for _, token := range held {
		_ = journal.ReleaseLock(ctx, token)
	}
}

// purgeNamespaceRows deletes every non-configuration row that belongs to the
// active namespaces. Lookup values are materialised into temporary tables first
// so each following DELETE is a single set operation instead of a per-row call.
func (r *KnowledgeBaseRepository) purgeNamespaceRows(ctx context.Context, tx *sql.Tx, active []string, arguments []any, inClause string) error {
	temporary := []struct {
		statement string
		withArgs  bool
	}{
		{"DROP TEMPORARY TABLE IF EXISTS tmp_purge_ops", false},
		{"CREATE TEMPORARY TABLE tmp_purge_ops (id VARCHAR(32) NOT NULL PRIMARY KEY)", false},
		{"INSERT IGNORE INTO tmp_purge_ops SELECT id FROM upload_operations WHERE namespace IN (" + inClause + ")", true},
		{"DROP TEMPORARY TABLE IF EXISTS tmp_purge_hashes", false},
		{"CREATE TEMPORARY TABLE tmp_purge_hashes (hash VARCHAR(64) NOT NULL PRIMARY KEY)", false},
		{"INSERT IGNORE INTO tmp_purge_hashes SELECT DISTINCT content_hash FROM upload_objects " +
			"WHERE content_hash IS NOT NULL AND operation_id IN (SELECT id FROM tmp_purge_ops)", false},
		{"INSERT IGNORE INTO tmp_purge_hashes SELECT DISTINCT sha256 FROM blobs WHERE namespace IN (" + inClause + ")", true},
		{"DROP TEMPORARY TABLE IF EXISTS tmp_purge_batches", false},
		{"CREATE TEMPORARY TABLE tmp_purge_batches (id VARCHAR(32) NOT NULL PRIMARY KEY)", false},
		{"INSERT IGNORE INTO tmp_purge_batches SELECT id FROM document_batches WHERE namespace IN (" + inClause + ")", true},
	}
	for _, statement := range temporary {
		if statement.withArgs {
			if _, err := tx.ExecContext(ctx, statement.statement, arguments...); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, statement.statement); err != nil {
			return err
		}
	}

	deletes := []struct {
		statement string
		withArgs  bool
	}{
		{"DELETE FROM upload_objects WHERE operation_id IN (SELECT id FROM tmp_purge_ops)", false},
		{"DELETE FROM upload_steps WHERE operation_id IN (SELECT id FROM tmp_purge_ops)", false},
		{"DELETE FROM upload_operations WHERE id IN (SELECT id FROM tmp_purge_ops)", false},
		{"DELETE FROM blobs WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM document_jobs WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM document_batch_items WHERE batch_id IN (SELECT id FROM tmp_purge_batches)", false},
		{"DELETE FROM document_batches WHERE id IN (SELECT id FROM tmp_purge_batches)", false},
		{"DELETE FROM search_tasks WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM mcp_catalogs WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM artifact_snapshots WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM document_heads WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM sag_extraction_jobs WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM sag_event_entities WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM sag_events WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM sag_entities WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM document_leases WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM kb_permissions WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM kb_invitations WHERE namespace IN (" + inClause + ")", true},
		{"DELETE FROM kb_group_permissions WHERE namespace IN (" + inClause + ")", true},
	}
	for _, delete := range deletes {
		if delete.withArgs {
			if _, err := tx.ExecContext(ctx, delete.statement, arguments...); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(ctx, delete.statement); err != nil {
			return err
		}
	}
	return nil
}

// purgeContentAndCache retires unreferenced payload rows, drops the tenant cache
// rows and bumps every affected read-model revision in batch. Payload bytes are
// not deleted here: retirement starts the retention window (>1 week) after which
// the maintenance GC removes them, keeping an accidental delete recoverable.
func (r *KnowledgeBaseRepository) purgeContentAndCache(ctx context.Context, tx *sql.Tx, active []string, arguments []any, inClause string) error {
	if _, err := tx.ExecContext(ctx,
		"UPDATE content_objects co JOIN tmp_purge_hashes h ON h.hash = co.hash SET co.retired_at = ? "+
			"WHERE co.retired_at IS NULL "+
			"AND NOT EXISTS (SELECT 1 FROM upload_objects u WHERE u.content_hash = co.hash) "+
			"AND NOT EXISTS (SELECT 1 FROM blobs b WHERE b.sha256 = co.hash)", unixSeconds()); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx,
		"DELETE FROM cache_revisions WHERE scope IN ("+inClause+")", arguments...); err != nil {
		return err
	}
	keys := purgeCacheKeys(active)
	if _, err := touch(ctx, tx, keys...); err != nil {
		return err
	}
	return nil
}

// purgeCacheKeys lists the read-model scopes a namespace purge invalidates.
func purgeCacheKeys(namespaces []string) []CacheKey {
	keys := make([]CacheKey, 0, 1+6*len(namespaces))
	keys = append(keys, CacheKey{Kind: "kb_list", Scope: ""})
	for _, namespace := range namespaces {
		keys = append(keys,
			CacheKey{Kind: "kb_public", Scope: namespace},
			CacheKey{Kind: "documents", Scope: namespace},
			CacheKey{Kind: "catalog", Scope: namespace},
			CacheKey{Kind: "heads", Scope: namespace},
			CacheKey{Kind: "leases", Scope: namespace},
			CacheKey{Kind: "upload_list", Scope: namespace},
		)
	}
	return keys
}

// CollectionsInUse reports which of the given collection names still belong to a
// knowledge base. A collection is safe to delete when it is not in this set.
func (r *KnowledgeBaseRepository) CollectionsInUse(ctx context.Context, collections []string) (map[string]bool, error) {
	inUse := make(map[string]bool, len(collections))
	names := dedupeNonEmpty(collections)
	if len(names) == 0 {
		return inUse, nil
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT DISTINCT collection FROM knowledge_bases WHERE collection IN ("+inPlaceholders(len(names))+")",
		stringArguments(names)...)
	if err != nil {
		return inUse, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var collection string
		if err := rows.Scan(&collection); err != nil {
			return inUse, err
		}
		inUse[collection] = true
	}
	return inUse, rows.Err()
}

// dedupeNonEmpty removes empty and duplicate values while keeping first-seen
// order.
func dedupeNonEmpty(values []string) []string {
	cleaned := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		cleaned = append(cleaned, value)
	}
	return cleaned
}

// inPlaceholders renders "?, ?, ?" for one IN clause.
func inPlaceholders(count int) string {
	if count <= 0 {
		return "NULL"
	}
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}

// stringArguments converts a string slice into Exec/Query arguments.
func stringArguments(values []string) []any {
	arguments := make([]any, len(values))
	for index, value := range values {
		arguments[index] = value
	}
	return arguments
}
