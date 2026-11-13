package migrate

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/Guan0923/xinghe_rag/internal/state"
)

// RetiredOutboxTables are the transactional outbox tables replaced by Debezium
// table-level capture (docs/cdc-migration.md §9). They are dropped only by the
// explicit --retire-outbox step, never by the automatic schema bootstrap.
var RetiredOutboxTables = []string{
	"document_job_outbox",
	"search_task_outbox",
	"cache_outbox",
	"document_task_outbox",
}

// OutboxRetirement reports the retired outbox tables seen by one RetireOutbox
// call.
type OutboxRetirement struct {
	// Dropped lists the tables that existed and were removed by this call.
	Dropped []string
	// Missing lists the tables that were already absent (idempotent rerun).
	Missing []string
}

// RetireOutbox drops the retired transactional outbox tables with
// DROP TABLE IF EXISTS and verifies they are gone. It is idempotent and only
// touches RetiredOutboxTables; document_tasks, document_jobs, search_tasks,
// search_result_snapshots, cache_revisions and every other current table are
// left untouched.
func RetireOutbox(ctx context.Context, stores *state.Stores) (OutboxRetirement, error) {
	retirement := OutboxRetirement{Dropped: []string{}, Missing: []string{}}
	for _, table := range RetiredOutboxTables {
		exists, err := retireTableExists(ctx, stores.DB, table)
		if err != nil {
			return retirement, err
		}
		if !exists {
			retirement.Missing = append(retirement.Missing, table)
			continue
		}
		if _, err := stores.DB.ExecContext(ctx, "DROP TABLE IF EXISTS `"+table+"`"); err != nil {
			return retirement, fmt.Errorf("删除旧 outbox 表 %s 失败：%w", table, err)
		}
		confirmed, err := retireTableExists(ctx, stores.DB, table)
		if err != nil {
			return retirement, err
		}
		if confirmed {
			return retirement, fmt.Errorf("旧 outbox 表 %s 仍然存在", table)
		}
		retirement.Dropped = append(retirement.Dropped, table)
	}
	return retirement, nil
}

// OutboxTablesPresent lists the retired outbox tables that still exist, without
// changing anything. It is the dry-run counterpart of RetireOutbox.
func OutboxTablesPresent(ctx context.Context, stores *state.Stores) ([]string, error) {
	present := make([]string, 0, len(RetiredOutboxTables))
	for _, table := range RetiredOutboxTables {
		exists, err := retireTableExists(ctx, stores.DB, table)
		if err != nil {
			return nil, err
		}
		if exists {
			present = append(present, table)
		}
	}
	return present, nil
}

// retireTableExists reports whether one table exists in the current database.
func retireTableExists(ctx context.Context, database *sql.DB, table string) (bool, error) {
	const query = "SELECT 1 FROM information_schema.tables " +
		"WHERE table_schema = DATABASE() AND table_name = ? LIMIT 1"
	var found int
	err := database.QueryRowContext(ctx, query, table).Scan(&found)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
