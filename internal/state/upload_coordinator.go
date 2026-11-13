// Package-level errors and the durable upload coordinator.
package state

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/logging"
)

// IndexPublisher is the compensating index seam the runtime implements with
// Qdrant. DeleteVersion must be idempotent and version-scoped: never delete by
// document id alone.
type IndexPublisher interface {
	DeleteVersion(ctx context.Context, documentID, version string) error
}

// LegacyIndexPublisher is the optional unversioned-points seam used for
// first-version publications that have no staged version to delete.
type LegacyIndexPublisher interface {
	DeleteLegacy(ctx context.Context, documentID string) error
}

// IndexStoreFactory resolves the operation's Qdrant collection lazily. It is
// only called when a durable Qdrant intent exists, so neither pre-vector
// failures nor workspace-only cleanup need an index client.
type IndexStoreFactory func(operation *UploadOperation) (IndexPublisher, error)

// IndexPublication is the runtime-owned publish/restore seam. The state layer
// never calls it directly: the runtime drives these callbacks inside journal step
// actions (RunStep/Intent with names such as "stage_qdrant" and "publish"), which
// is exactly how rag_mcp.service_knowledge publishes vectors. The coordinator
// consumes only the compensating delete seams above.
type IndexPublication interface {
	PublishVersion(ctx context.Context, operation *UploadOperation) error
	RestoreVersion(ctx context.Context, operation *UploadOperation) error
}

var qdrantIntentPattern = regexp.MustCompile(`(?i)^qdrant(?:$|[._:-])`)

// hasQdrantIntent reports whether any recorded step is a vector-write intent.
// Even an unfinished or failed step counts: its external write may land late.
func hasQdrantIntent(operation *UploadOperation) bool {
	for _, step := range operation.Steps {
		if step.Name == uploadQdrantIntentStep || qdrantIntentPattern.MatchString(step.Name) {
			return true
		}
	}
	return false
}

// UploadCoordinator owns compensation, committed cleanup, workspace reclamation
// and journal-driven crash recovery. It is a port of
// rag_mcp.state.upload_coordinator.UploadCoordinator.
type UploadCoordinator struct {
	Journal       *UploadJournal
	WorkspaceRoot string
	// Node identifies this host for workspace ownership (hostname).
	Node string

	removeAll func(string) error
}

// NewUploadCoordinator builds a coordinator over the store's journal. An empty
// workspaceRoot defaults to the system temporary directory.
func NewUploadCoordinator(stores *Stores, workspaceRoot string) (*UploadCoordinator, error) {
	journal := stores.Uploads
	if journal == nil {
		created, err := NewUploadJournal(stores)
		if err != nil {
			return nil, err
		}
		journal = created
	}
	root := workspaceRoot
	if root == "" {
		root = os.TempDir()
	}
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if resolved, resolveErr := filepath.EvalSymlinks(absolute); resolveErr == nil {
		absolute = resolved
	}
	node, _ := os.Hostname()
	return &UploadCoordinator{
		Journal:       journal,
		WorkspaceRoot: absolute,
		Node:          node,
		removeAll:     os.RemoveAll,
	}, nil
}

// Close stops this coordinator's journal lease-renewal threads.
func (c *UploadCoordinator) Close() {
	if c.Journal != nil {
		c.Journal.Close()
	}
}

// validUploadOperationID reports the lowercase 32-hex operation id format the
// journal guarantees.
func validUploadOperationID(operationID string) bool {
	return ValidateCacheEventID(operationID)
}

func hasParentTraversal(path string) bool {
	for _, part := range strings.FieldsFunc(path, func(char rune) bool {
		return char == '/' || char == '\\'
	}) {
		if part == ".." {
			return true
		}
	}
	return false
}

// removeWorkspace removes only this operation's registered local workspace.
// Remote or absent registrations return false; unsafe paths raise.
func (c *UploadCoordinator) removeWorkspace(operation *UploadOperation) (bool, error) {
	if operation.Workspace == nil || operation.WorkspaceNode == nil || *operation.WorkspaceNode != c.Node {
		return false, nil
	}
	operationID := operation.ID
	raw := *operation.Workspace
	prefix := "rag-txn-" + operationID
	if !validUploadOperationID(operationID) || !filepath.IsAbs(raw) || hasParentTraversal(raw) {
		return false, newValueError("Unsafe upload workspace")
	}
	cleaned := filepath.Clean(raw)
	if filepath.Dir(cleaned) != c.WorkspaceRoot {
		return false, newValueError("Unsafe upload workspace")
	}
	base := filepath.Base(cleaned)
	if base != prefix && !strings.HasPrefix(base, prefix+"-") {
		return false, newValueError("Unsafe upload workspace")
	}
	info, err := os.Lstat(raw)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return false, newValueError("Unsafe upload workspace")
	}
	resolved, err := filepath.EvalSymlinks(raw)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if filepath.Dir(resolved) != c.WorkspaceRoot {
		return false, newValueError("Unsafe upload workspace")
	}
	remove := c.removeAll
	if remove == nil {
		remove = os.RemoveAll
	}
	if err := remove(raw); err != nil {
		if _, statErr := os.Lstat(raw); errors.Is(statErr, os.ErrNotExist) {
			return true, nil
		}
		return false, err
	}
	return true, nil
}

// CleanupWorkspace removes only the registered workspace of one operation.
func (c *UploadCoordinator) CleanupWorkspace(ctx context.Context, operation *UploadOperation) (bool, error) {
	current, err := c.Journal.Get(ctx, operation.ID)
	if err != nil {
		return false, err
	}
	if current.Status != UploadCommitted && current.Status != UploadRolledBack {
		if _, err := c.Journal.AssertOwner(ctx, operation); err != nil {
			return false, err
		}
	}
	removed, err := c.removeWorkspace(current)
	if err != nil {
		return false, err
	}
	if removed && current.Status == UploadCommitted {
		if err := c.clearWorkspace(ctx, current); err != nil {
			return false, err
		}
	}
	return removed, nil
}

// clearWorkspace forgets a committed operation's workspace registration.
func (c *UploadCoordinator) clearWorkspace(ctx context.Context, operation *UploadOperation) error {
	if operation.Workspace == nil || operation.WorkspaceNode == nil {
		return nil
	}
	transaction, err := c.Journal.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if err := c.Journal.guard(ctx, transaction, operation.Namespace); err != nil {
		return err
	}
	var currentID string
	err = transaction.QueryRowContext(ctx,
		"SELECT id FROM upload_operations WHERE id = ? AND status = ? AND workspace = ? AND workspace_node = ?"+
			c.Journal.forUpdate(),
		operation.ID, UploadCommitted, *operation.Workspace, *operation.WorkspaceNode).Scan(&currentID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := c.Journal.touchCache(ctx, transaction, operation.Namespace, &operation.ID, false, false); err != nil {
		return err
	}
	if _, err := transaction.ExecContext(ctx,
		"UPDATE upload_operations SET workspace = NULL, workspace_node = NULL WHERE id = ? AND status = ? "+
			"AND workspace = ? AND workspace_node = ?",
		operation.ID, UploadCommitted, *operation.Workspace, *operation.WorkspaceNode); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	c.Journal.flushCache(ctx, operation.Namespace, &operation.ID, false, false)
	return nil
}

// Compensate immediately rolls back an owned unpublished upload; failures
// propagate. No vector deletion happens without a durable Qdrant intent.
func (c *UploadCoordinator) Compensate(ctx context.Context, operation *UploadOperation, store IndexPublisher) (*UploadOperation, error) {
	return c.rollbackOp(ctx, operation, func(*UploadOperation) (IndexPublisher, error) {
		return store, nil
	})
}

// rollbackOp runs the journal rollback with version-scoped vector compensation
// and workspace removal, then reconciles workspace state.
func (c *UploadCoordinator) rollbackOp(ctx context.Context, operation *UploadOperation, factory IndexStoreFactory) (*UploadOperation, error) {
	cleanup := func(current *UploadOperation) error {
		durable, err := c.Journal.Get(ctx, current.ID)
		if err != nil {
			return err
		}
		// Read durable steps, not the possibly stale caller snapshot.
		if hasQdrantIntent(durable) {
			store, err := factory(current)
			if err != nil {
				return err
			}
			if store != nil {
				if err := store.DeleteVersion(ctx, current.DocumentID, current.Version); err != nil {
					return err
				}
			}
		}
		_, err = c.removeWorkspace(current)
		return err
	}
	result, err := c.Journal.Rollback(ctx, operation, cleanup)
	if err != nil {
		return nil, err
	}
	if result != nil {
		if _, cleanupErr := c.CleanupWorkspace(ctx, result); cleanupErr != nil {
			logging.FromContext(ctx).Warn("Upload workspace cleanup failed; retry pending", append([]any{"operation_id", operation.ID}, logging.ErrorAttrs(cleanupErr)...)...)
		}
	}
	return c.Journal.Get(ctx, operation.ID)
}

// Cleanup cleans the workspace and old_version (or unversioned legacy points)
// after the grace period. Cleanup errors are retained as cleanup_pending.
func (c *UploadCoordinator) Cleanup(ctx context.Context, operation *UploadOperation, store IndexPublisher) (*UploadOperation, error) {
	return c.cleanupOp(ctx, operation, func(*UploadOperation) (IndexPublisher, error) {
		return store, nil
	})
}

func (c *UploadCoordinator) cleanupOp(ctx context.Context, operation *UploadOperation, factory IndexStoreFactory) (*UploadOperation, error) {
	current, err := c.Journal.Get(ctx, operation.ID)
	if err != nil {
		return nil, err
	}
	if current.Status != UploadCommitted {
		return nil, newUploadConflict("Committed cleanup requires COMMITTED")
	}
	cleanupOld := func(committed *UploadOperation) error {
		if committed.OldVersion != nil {
			store, err := factory(committed)
			if err != nil {
				return err
			}
			if store == nil {
				return nil
			}
			return store.DeleteVersion(ctx, committed.DocumentID, *committed.OldVersion)
		}
		durable, err := c.Journal.Get(ctx, committed.ID)
		if err != nil {
			return err
		}
		for _, step := range durable.Steps {
			if step.Name != uploadQdrantIntentStep {
				continue
			}
			store, err := factory(committed)
			if err != nil {
				return err
			}
			if legacy, ok := store.(LegacyIndexPublisher); ok {
				return legacy.DeleteLegacy(ctx, committed.DocumentID)
			}
			return nil
		}
		return nil
	}
	// Workspace cleanup is independent of vector cleanup eligibility.
	if _, workspaceErr := c.removeWorkspace(current); workspaceErr != nil {
		return c.recordCleanupFailure(ctx, current, workspaceErr)
	} else {
		now, err := c.Journal.dbNow(ctx, c.Journal.stores.DB)
		if err != nil {
			return nil, err
		}
		if now >= current.UpdatedAt+c.Journal.CleanupDelaySeconds {
			if err := c.clearWorkspaceAfterRemoval(ctx, current); err != nil {
				return c.recordCleanupFailure(ctx, current, err)
			}
		}
	}
	result, cleanupErr := c.Journal.Cleanup(ctx, current, cleanupOld)
	if cleanupErr != nil {
		return c.recordCleanupFailure(ctx, current, cleanupErr)
	}
	return result, nil
}

// clearWorkspaceAfterRemoval clears a committed workspace registration even when
// old-version cleanup is still deferred by the grace period.
func (c *UploadCoordinator) clearWorkspaceAfterRemoval(ctx context.Context, operation *UploadOperation) error {
	removed, err := c.removeWorkspace(operation)
	if err != nil {
		return err
	}
	if removed {
		return c.clearWorkspace(ctx, operation)
	}
	return nil
}

// recordCleanupFailure keeps committed cleanup retryable without rolling back.
func (c *UploadCoordinator) recordCleanupFailure(ctx context.Context, operation *UploadOperation, cause error) (*UploadOperation, error) {
	transaction, err := c.Journal.stores.DB.BeginTx(ctx, nil)
	if err == nil {
		func() {
			defer func() { _ = transaction.Rollback() }()
			if guardErr := c.Journal.guard(ctx, transaction, operation.Namespace); guardErr != nil {
				return
			}
			pending := operation.CleanupPending || operation.OldVersion == nil
			var changedID string
			query := "SELECT id FROM upload_operations WHERE id = ? AND status = ? AND cleanup_pending != ?" +
				c.Journal.forUpdate()
			if queryErr := transaction.QueryRowContext(ctx, query,
				operation.ID, UploadCommitted, pending).Scan(&changedID); queryErr != nil {
				return
			}
			if touchErr := c.Journal.touchCache(ctx, transaction, operation.Namespace, &operation.ID, false, false); touchErr != nil {
				return
			}
			if _, execErr := transaction.ExecContext(ctx,
				"UPDATE upload_operations SET cleanup_pending = ? WHERE id = ? AND status = ? AND cleanup_pending != ?",
				pending, operation.ID, UploadCommitted, pending); execErr != nil {
				return
			}
			if commitErr := transaction.Commit(); commitErr != nil {
				return
			}
			c.Journal.flushCache(ctx, operation.Namespace, &operation.ID, false, false)
		}()
	}
	logging.FromContext(ctx).Warn("Upload cleanup failed; retry pending", append([]any{"operation_id", operation.ID}, logging.ErrorAttrs(cause)...)...)
	return c.Journal.Get(ctx, operation.ID)
}

// Reap rolled-back vector intents once per scheduled recovery pass, containing
// writes that expired workers completed late.
func (c *UploadCoordinator) reapRolledBack(ctx context.Context, operation *UploadOperation, factory IndexStoreFactory) error {
	current, err := c.Journal.Get(ctx, operation.ID)
	if err != nil {
		return err
	}
	if current.Status != UploadRolledBack || !hasQdrantIntent(current) {
		return nil
	}
	head, err := c.Journal.Head(ctx, current.Namespace, current.DocumentID)
	if err != nil {
		return err
	}
	if head != nil && *head == current.ID {
		return newUploadConflict("Cannot reap the active version")
	}
	store, err := factory(current)
	if err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	return store.DeleteVersion(ctx, current.DocumentID, current.Version)
}

// recoveryOperationIDs finds committed/rolled-back operations that still own a
// local workspace plus rolled-back Qdrant tombstones.
func (c *UploadCoordinator) recoveryOperationIDs(ctx context.Context, namespace *string) ([]string, error) {
	arguments := []any{UploadCommitted, UploadRolledBack, c.Node}
	query := "SELECT id FROM upload_operations WHERE status IN (?, ?) AND workspace IS NOT NULL AND workspace_node = ?"
	if namespace != nil {
		query += " AND namespace = ?"
		arguments = append(arguments, *namespace)
	}
	rows, err := c.Journal.stores.DB.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	ids := make([]string, 0)
	seen := map[string]bool{}
	for rows.Next() {
		var operationID string
		if err := rows.Scan(&operationID); err != nil {
			_ = rows.Close()
			return nil, err
		}
		if !seen[operationID] {
			seen[operationID] = true
			ids = append(ids, operationID)
		}
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	tombstoneArguments := []any{UploadRolledBack}
	tombstoneQuery := "SELECT o.id FROM upload_operations o WHERE o.status = ? AND EXISTS (" +
		"SELECT 1 FROM upload_steps s WHERE s.operation_id = o.id AND (s.name = ? OR LOWER(s.name) = ? " +
		"OR LOWER(s.name) LIKE ? OR LOWER(s.name) LIKE ? OR LOWER(s.name) LIKE ? OR LOWER(s.name) LIKE ?))"
	tombstoneArguments = append(tombstoneArguments, uploadQdrantIntentStep, "qdrant",
		"qdrant.%", "qdrant_%", "qdrant-%", "qdrant:%")
	if namespace != nil {
		tombstoneQuery += " AND o.namespace = ?"
		tombstoneArguments = append(tombstoneArguments, *namespace)
	}
	tombstoneRows, err := c.Journal.stores.DB.QueryContext(ctx, tombstoneQuery, tombstoneArguments...)
	if err != nil {
		return nil, err
	}
	for tombstoneRows.Next() {
		var operationID string
		if err := tombstoneRows.Scan(&operationID); err != nil {
			_ = tombstoneRows.Close()
			return nil, err
		}
		if !seen[operationID] {
			seen[operationID] = true
			ids = append(ids, operationID)
		}
	}
	if err := tombstoneRows.Close(); err != nil {
		return nil, err
	}
	return ids, nil
}

// Recover recovers expired unfinished uploads and retries committed cleanup;
// live leases are never stolen. Failures are isolated per operation and
// returned snapshots are the post-recovery journal state.
func (c *UploadCoordinator) Recover(ctx context.Context, factory IndexStoreFactory, namespace *string) []*UploadOperation {
	candidates := make([]*UploadOperation, 0)
	seen := map[string]bool{}
	addCandidate := func(operation *UploadOperation) {
		if operation == nil || seen[operation.ID] {
			return
		}
		seen[operation.ID] = true
		candidates = append(candidates, operation)
	}
	unfinished, err := c.Journal.Unfinished(ctx, namespace)
	if err == nil {
		for _, operation := range unfinished {
			addCandidate(operation)
		}
	}
	operationIDs, err := c.recoveryOperationIDs(ctx, namespace)
	if err == nil {
		for _, operationID := range operationIDs {
			if seen[operationID] {
				continue
			}
			operation, getErr := c.Journal.Get(ctx, operationID)
			if getErr == nil {
				addCandidate(operation)
			}
		}
	}
	recovered := make([]*UploadOperation, 0)
	for _, snapshot := range candidates {
		result, err := c.recoverOne(ctx, snapshot, factory)
		if err != nil {
			logging.FromContext(ctx).Warn("Upload recovery failed; retry pending", append([]any{"operation_id", snapshot.ID}, logging.ErrorAttrs(err)...)...)
			continue
		}
		if result != nil {
			recovered = append(recovered, result)
		}
	}
	return recovered
}

func (c *UploadCoordinator) recoverOne(ctx context.Context, snapshot *UploadOperation, factory IndexStoreFactory) (*UploadOperation, error) {
	// Refresh: another worker may have committed since enumeration.
	current, err := c.Journal.Get(ctx, snapshot.ID)
	if err != nil {
		return nil, err
	}
	switch current.Status {
	case UploadCommitted:
		return c.cleanupOp(ctx, current, factory)
	case UploadRolledBack:
		if _, err := c.CleanupWorkspace(ctx, current); err != nil {
			return nil, err
		}
		if err := c.reapRolledBack(ctx, current, factory); err != nil {
			return nil, err
		}
		return c.Journal.Get(ctx, current.ID)
	default:
		claimed, err := c.Journal.ClaimRecovery(ctx, current.ID)
		if err != nil {
			return nil, err
		}
		if claimed == nil {
			return nil, nil
		}
		return c.rollbackOp(ctx, claimed, factory)
	}
}
