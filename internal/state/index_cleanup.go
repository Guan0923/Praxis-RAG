package state

import (
	"context"
	"database/sql"
	"errors"
)

// IndexCleanup describes one external operation. Completing one backend never acknowledges another.
type IndexCleanup struct {
	ID, Target, Namespace, DocumentID, Collection, Version, Hash string
	Generation                                                   int64
}

// Capture the document head and the source hash before namespace purge can remove them.
// A graph may have been written even when the graph ledger failed to commit.
func queueDeletedIndexes(ctx context.Context, tx *sql.Tx, predicate string, args ...any) error {
	for _, target := range []string{"qdrant", "graph"} {
		query := "INSERT INTO index_cleanup_jobs (job_id,target,namespace,document_id,collection,version,source_hash,status,due_at) " +
			"SELECT UUID(),?,d.namespace,d.document_id,d.collection,COALESCE(h.version,''),d.sha256,'waiting',? " +
			"FROM documents d LEFT JOIN document_heads h ON h.namespace=d.namespace AND h.document_id=d.document_id " +
			"LEFT JOIN knowledge_bases k ON k.namespace=d.namespace WHERE " + predicate
		if target == "graph" {
			query += " AND (JSON_UNQUOTE(JSON_EXTRACT(k.settings,'$.graph_enabled'))='true' OR EXISTS (SELECT 1 FROM lightrag_documents g WHERE g.namespace=d.namespace AND g.document_id=d.document_id))"
		}
		query += " ON DUPLICATE KEY UPDATE source_hash=VALUES(source_hash),due_at=VALUES(due_at),status='waiting',schedule_version=index_cleanup_jobs.schedule_version+1"
		params := append([]any{target, unixSeconds() + float64(ContentObjectRetentionSeconds)}, args...)
		if _, err := tx.ExecContext(ctx, query, params...); err != nil {
			return err
		}
	}
	// A re-embedding publishes a new active collection without rewriting each
	// document row. Both collections can still contain the same upload version.
	_, err := tx.ExecContext(ctx, "INSERT INTO index_cleanup_jobs (job_id,target,namespace,document_id,collection,version,source_hash,status,due_at) "+
		"SELECT UUID(),'qdrant',d.namespace,d.document_id,k.collection,COALESCE(h.version,''),d.sha256,'waiting',? "+
		"FROM documents d JOIN knowledge_bases k ON d.namespace=k.namespace LEFT JOIN document_heads h ON h.namespace=d.namespace AND h.document_id=d.document_id "+
		"WHERE k.collection<>d.collection AND "+predicate+" ON DUPLICATE KEY UPDATE source_hash=VALUES(source_hash),due_at=VALUES(due_at),status='waiting',schedule_version=index_cleanup_jobs.schedule_version+1", append([]any{unixSeconds() + float64(ContentObjectRetentionSeconds)}, args...)...)
	return err
}

func (s *Stores) ScheduleIndexCleanup(ctx context.Context) error {
	_, err := s.DB.ExecContext(ctx, "UPDATE index_cleanup_jobs j SET status='ready',schedule_version=schedule_version+1 "+
		"WHERE status IN ('waiting','running') AND due_at<=? AND NOT EXISTS (SELECT 1 FROM content_objects c WHERE c.hash=j.source_hash) ORDER BY due_at LIMIT 200", unixSeconds())
	return err
}

// CleanupProtected is checked again while holding the document lease. A migrated or
// re-created collection can contain a live copy even when the original collection changed.
func (s *Stores) CleanupProtected(ctx context.Context, j IndexCleanup) (bool, error) {
	var live, busy bool
	err := s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM documents d LEFT JOIN document_heads h "+
		"ON h.namespace=d.namespace AND h.document_id=d.document_id WHERE d.namespace=? AND d.document_id=? AND d.del_flag=0 "+
		"AND ((?='graph' AND ?='') OR COALESCE(h.version,'')=?)), EXISTS(SELECT 1 FROM upload_operations WHERE namespace=? AND document_id=? "+
		"AND status NOT IN ('COMMITTED','ROLLED_BACK'))", j.Namespace, j.DocumentID, j.Target, j.Version, j.Version, j.Namespace, j.DocumentID).Scan(&live, &busy)
	if err == nil && busy {
		return true, ErrCleanupBusy
	}
	return live, err
}

var ErrCleanupBusy = errors.New("upload is still active")

// ObserveIndex starts the full retention period on first observation of an orphan.
// Repeated inventory passes preserve that first observation and any captured source hash.
func (s *Stores) ObserveIndex(ctx context.Context, j IndexCleanup) error {
	protected, err := s.CleanupProtected(ctx, j)
	if errors.Is(err, ErrCleanupBusy) {
		return nil
	}
	if err != nil || protected {
		return err
	}
	_, err = s.DB.ExecContext(ctx, "INSERT INTO index_cleanup_jobs (job_id,target,namespace,document_id,collection,version,source_hash,status,due_at) "+
		"SELECT UUID(),?,?,?,?,?,COALESCE(NULLIF(?,''),(SELECT sha256 FROM documents WHERE namespace=? AND document_id=? AND del_flag=1),''),'waiting',? "+
		"ON DUPLICATE KEY UPDATE source_hash=IF(index_cleanup_jobs.source_hash='',VALUES(source_hash),index_cleanup_jobs.source_hash),due_at=IF(index_cleanup_jobs.status='done',VALUES(due_at),index_cleanup_jobs.due_at),status=IF(index_cleanup_jobs.status='done','waiting',index_cleanup_jobs.status)",
		j.Target, j.Namespace, j.DocumentID, j.Collection, j.Version, j.Hash, j.Namespace, j.DocumentID, unixSeconds()+float64(ContentObjectRetentionSeconds))
	return err
}

const cleanupSelect = "SELECT job_id,target,namespace,document_id,collection,version,source_hash,schedule_version FROM index_cleanup_jobs "

func scanCleanup(row rowScanner) (IndexCleanup, error) {
	var j IndexCleanup
	err := row.Scan(&j.ID, &j.Target, &j.Namespace, &j.DocumentID, &j.Collection, &j.Version, &j.Hash, &j.Generation)
	return j, err
}

func (s *Stores) LookupCleanup(ctx context.Context, locator IndexCleanup) (IndexCleanup, error) {
	return scanCleanup(s.DB.QueryRowContext(ctx, cleanupSelect+"WHERE job_id=? AND namespace=? AND schedule_version=? AND status='ready'", locator.ID, locator.Namespace, locator.Generation))
}

// ReadyIndexJobs recovers work after a process or broker interruption.
func (s *Stores) ReadyIndexJobs(ctx context.Context) ([]IndexCleanup, error) {
	rows, err := s.DB.QueryContext(ctx, cleanupSelect+
		"WHERE status='ready' AND due_at<=? ORDER BY due_at,job_id LIMIT 100", unixSeconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []IndexCleanup
	for rows.Next() {
		j, err := scanCleanup(rows)
		if err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// RunIndexCleanup leases one operation without keeping a database connection
// open during HTTP. A stale claimant cannot acknowledge a later generation.
func (s *Stores) RunIndexCleanup(ctx context.Context, j IndexCleanup, execute func() error) error {
	claim, err := s.DB.ExecContext(ctx, "UPDATE index_cleanup_jobs SET status='running',due_at=? WHERE job_id=? AND schedule_version=? AND status='ready'", unixSeconds()+600, j.ID, j.Generation)
	if err != nil {
		return err
	}
	n, err := claim.RowsAffected()
	if err != nil || n == 0 {
		return err
	}
	var retained bool
	err = s.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM content_objects WHERE hash=?) OR EXISTS("+
		"SELECT 1 FROM index_cleanup_jobs k JOIN content_objects c ON c.hash=k.source_hash WHERE k.namespace=? AND k.document_id=? "+
		"AND (?='graph' OR k.version=?))", j.Hash, j.Namespace, j.DocumentID, j.Target, j.Version).Scan(&retained)
	if err != nil {
		return err
	}
	fence := " WHERE job_id=? AND schedule_version=? AND status='running'"
	if retained {
		_, err = s.DB.ExecContext(ctx, "UPDATE index_cleanup_jobs SET status='waiting',due_at=?"+fence, unixSeconds()+3600, j.ID, j.Generation)
		return err
	}
	workErr := execute()
	if workErr != nil {
		_, err = s.DB.ExecContext(ctx, "UPDATE index_cleanup_jobs SET status='waiting',due_at=?+LEAST(3600,30*POW(2,LEAST(failures,7))),failures=failures+1"+fence, unixSeconds(), j.ID, j.Generation)
		return errors.Join(workErr, err)
	}
	_, err = s.DB.ExecContext(ctx, "UPDATE index_cleanup_jobs SET status='done',due_at=?"+fence, unixSeconds(), j.ID, j.Generation)
	return err
}
