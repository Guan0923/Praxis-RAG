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

// cacheKinds is the set of read-model kinds owned by the state layer: every
// kind with a registered refresh loader (see NewCachedReadViews). Unused kinds
// (document_status/search_status) were removed together with cache_outbox.
var cacheKinds = map[string]struct{}{
	"kb_public":     {},
	"kb_list":       {},
	"documents":     {},
	"catalog":       {},
	"heads":         {},
	"leases":        {},
	"upload_status": {},
	"upload_list":   {},
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

func catalogKeys(namespace string) []CacheKey {
	return []CacheKey{{Kind: "catalog", Scope: namespace}}
}

// sortedCacheKeys validates, de-duplicates and sorts cache keys by (kind,
// scope), which is the single global acquisition order for cache_revisions
// rows. Sorted acquisition prevents cross-kind lock inversion between writers.
func sortedCacheKeys(keys []CacheKey) ([]CacheKey, error) {
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
	return pending, nil
}

// knownCacheKinds returns the registered kinds in deterministic order. The
// dirty sweep uses it to ignore revision rows for retired kinds, which could
// otherwise occupy every batch forever (no loader can rebuild them).
func knownCacheKinds() []string {
	kinds := make([]string, 0, len(cacheKinds))
	for kind := range cacheKinds {
		kinds = append(kinds, kind)
	}
	sort.Strings(kinds)
	return kinds
}

// bumpRevisionTx bumps one read-model revision inside the caller's transaction.
// cache_revisions is the only durable cache signal: Debezium captures the
// business-table change and the cache consumer rebuilds the affected scope from
// MySQL. No outbox row is written.
func bumpRevisionTx(ctx context.Context, tx *sql.Tx, key CacheKey, updatedAt float64) error {
	_, err := tx.ExecContext(ctx,
		"INSERT INTO cache_revisions (kind, scope, revision, updated_at) VALUES (?, ?, 1, ?) "+
			"ON DUPLICATE KEY UPDATE revision = revision + 1, updated_at = ?",
		key.Kind, key.Scope, updatedAt, updatedAt)
	return err
}

// touch mirrors cache_events.touch: bump every read-model revision inside the
// caller's transaction. The legacy event-id return value is kept for call-site
// compatibility and is always nil; the revision water line replaces the
// transactional outbox.
func touch(ctx context.Context, tx *sql.Tx, keys ...CacheKey) ([]string, error) {
	pending, err := sortedCacheKeys(keys)
	if err != nil {
		return nil, err
	}
	updatedAt := unixSeconds()
	for _, key := range pending {
		if err := bumpRevisionTx(ctx, tx, key, updatedAt); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
