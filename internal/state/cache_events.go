package state

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
)

// CacheKey identifies one read-model scope whose revision is bumped inside the
// writer's transaction.
type CacheKey struct {
	Kind  string
	Scope string
}

// cacheKinds mirrors cache_events.KINDS.
var cacheKinds = map[string]struct{}{
	"kb_public":       {},
	"kb_list":         {},
	"documents":       {},
	"catalog":         {},
	"api_keys":        {},
	"heads":           {},
	"leases":          {},
	"upload_status":   {},
	"upload_list":     {},
	"document_status": {},
	"search_status":   {},
}

// knowledgeKeys mirrors cache_events.knowledge_keys.
func knowledgeKeys(namespace string) []CacheKey {
	return []CacheKey{
		{Kind: "kb_list", Scope: ""},
		{Kind: "kb_public", Scope: namespace},
		{Kind: "documents", Scope: namespace},
	}
}

// documentKeys mirrors cache_events.document_keys.
func documentKeys(namespace string) []CacheKey {
	return []CacheKey{
		{Kind: "kb_list", Scope: ""},
		{Kind: "documents", Scope: namespace},
	}
}

func apiKeyKeys() []CacheKey { return []CacheKey{{Kind: "api_keys", Scope: ""}} }
func catalogKeys(namespace string) []CacheKey {
	return []CacheKey{{Kind: "catalog", Scope: namespace}}
}

// touch mirrors cache_events.touch: bump every read-model revision and append
// one PENDING outbox event per key inside the caller's transaction. Sorted,
// de-duplicated acquisition order prevents cross-kind lock inversion. The Redis
// publication side (CacheSynchronizer.flush_pending) is deferred to a later
// round, but the transactional rows are written now so revisions stay correct.
func touch(ctx context.Context, tx *sql.Tx, dialect Dialect, keys ...CacheKey) ([]string, error) {
	pending := make([]CacheKey, 0, len(keys))
	seen := make(map[CacheKey]struct{}, len(keys))
	for _, key := range keys {
		if _, known := cacheKinds[key.Kind]; !known {
			return nil, fmt.Errorf("Unknown state cache kind")
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		pending = append(pending, key)
	}
	sort.Slice(pending, func(left, right int) bool {
		if pending[left].Kind != pending[right].Kind {
			return pending[left].Kind < pending[right].Kind
		}
		return pending[left].Scope < pending[right].Scope
	})

	insertRevision := "INSERT OR IGNORE INTO cache_revisions (kind, scope, revision) VALUES (?, ?, 0)"
	if dialect == DialectMySQL {
		insertRevision = "INSERT INTO cache_revisions (kind, scope, revision) VALUES (?, ?, 0) " +
			"ON DUPLICATE KEY UPDATE revision = revision"
	}
	eventIDs := make([]string, 0, len(pending))
	for _, key := range pending {
		if _, err := tx.ExecContext(ctx, insertRevision, key.Kind, key.Scope); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			"UPDATE cache_revisions SET revision = revision + 1 WHERE kind = ? AND scope = ?",
			key.Kind, key.Scope); err != nil {
			return nil, err
		}
		var revision int64
		if err := tx.QueryRowContext(ctx,
			"SELECT revision FROM cache_revisions WHERE kind = ? AND scope = ?",
			key.Kind, key.Scope).Scan(&revision); err != nil {
			return nil, err
		}
		eventID, err := uuid4Hex()
		if err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			"INSERT INTO cache_outbox (id, kind, scope, revision, status, attempts, retry_at, created_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
			eventID, key.Kind, key.Scope, revision, "PENDING", 0, 0.0, unixSeconds()); err != nil {
			return nil, err
		}
		eventIDs = append(eventIDs, eventID)
	}
	return eventIDs, nil
}
