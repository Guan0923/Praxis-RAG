package state

// SagIndex is the offline event-entity index of the SAG retrieval
// architecture (Zleap-AI/SAG): one event row per chunk plus its normalized
// entities and the event-entity association rows. MySQL is authoritative; the
// Qdrant points of the same rows are only the semantic seed-recall copies.
//
// Writes happen on the document publish path (one namespace, one document,
// one version), so every method is scoped by namespace and keyed by chunk.
import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"time"
)

// sagIDNamespace is the fixed UUID namespace for the SAG index rows.
var sagIDNamespace = [16]byte{
	0x9a, 0x6c, 0x43, 0x2e, 0x71, 0x0d, 0x4f, 0xb2,
	0x93, 0x1a, 0xc5, 0x77, 0x2f, 0xe4, 0x88, 0x19,
}

// sagEventID derives the deterministic event id: uuid5 over the namespace and
// chunk id. One chunk produces at most one event, so the chunk id alone is
// stable across re-extractions.
func sagEventID(namespace, chunkID string, position int) string {
	_ = position
	return uuid5(sagIDNamespace, "event:"+namespace+"\x00"+chunkID)
}

// sagEntityID derives the deterministic entity id: uuid5 over the namespace
// and entity key.
func sagEntityID(namespace, entityKey string) string {
	return uuid5(sagIDNamespace, "entity:"+namespace+"\x00"+entityKey)
}

// nowSag renders the sag-table timestamp (the shared isoTimestampFormat).
func nowSag() string {
	return nowISO()
}

// SagEvent is one extracted event: the semantically complete unit of a chunk.
type SagEvent struct {
	ID         string
	Namespace  string
	DocumentID string
	ChunkID    string
	Event      string
	Version    string
	CreatedAt  string
}

// SagEntity is one normalized entity. NormalizedKey is the join key;
// DisplayName is the first-seen original form.
type SagEntity struct {
	ID            string
	Namespace     string
	NormalizedKey string
	EntityKey     string
	DisplayName   string
	MentionCount  int64
	CreatedAt     string
	UpdatedAt     string
}

// SagExtraction is one chunk's extraction result as produced by the model
// client: one event text plus the entity names in first-seen order.
type SagExtraction struct {
	ChunkID  string
	Event    string
	Entities []string
}

// SagRepository persists the event-entity index.
type SagRepository struct{ db *sql.DB }

func newSagRepository(db *sql.DB) *SagRepository {
	return &SagRepository{db: db}
}

const sagRepositoryDoc = "SagRepository persists the event-entity index."

// ReplaceDocument atomically replaces the SAG index rows of one document
// version: the previous events of the same document are deleted first (their
// associations cascade), then the new events, entities and associations are
// written. Entities are shared per namespace: mention_count is incremented for
// reused entity keys and the display name keeps its first-seen form.
//
// An empty extractions slice removes the document's index rows, mirroring a
// document whose chunks produced no extraction.
func (r *SagRepository) ReplaceDocument(
	ctx context.Context,
	namespace, documentID, version string,
	extractions []SagExtraction,
) error {
	return r.replaceDocument(ctx, namespace, documentID, version, extractions, false)
}

// BackfillDocument publishes CLI results only while the document snapshot is
// current and has no fragment jobs managed by the worker.
func (r *SagRepository) BackfillDocument(ctx context.Context, namespace, documentID, version string,
	extractions []SagExtraction) error {
	return r.replaceDocument(ctx, namespace, documentID, version, extractions, true)
}

func (r *SagRepository) replaceDocument(ctx context.Context, namespace, documentID, version string,
	extractions []SagExtraction, backfill bool) error {
	if strings.TrimSpace(namespace) == "" {
		return newStateError("SAG 索引写入失败：namespace 不能为空")
	}
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return newStateError("SAG 索引写入失败")
	}
	defer func() { _ = transaction.Rollback() }()

	if backfill {
		current, active, err := lockSagDocumentVersion(ctx, transaction, namespace, documentID)
		if err != nil || !active || current != version {
			return err
		}
		var managed bool
		err = transaction.QueryRowContext(ctx,
			"SELECT EXISTS (SELECT 1 FROM sag_extraction_jobs WHERE namespace = ? AND document_id = ?)",
			namespace, documentID).Scan(&managed)
		if err != nil || managed {
			return err
		}
	}
	if err := deleteSagEvents(ctx, transaction, namespace, documentID); err != nil {
		return err
	}

	now := nowSag()
	for position, extraction := range extractions {
		if err := insertSagExtraction(ctx, transaction, namespace, documentID, version, now, position, extraction); err != nil {
			return err
		}
	}
	if err := transaction.Commit(); err != nil {
		return sagWriteError(err)
	}
	return nil
}

// insertSagExtraction adds one fragment and its entity links to the caller's transaction.
func insertSagExtraction(ctx context.Context, transaction *sql.Tx, namespace, documentID, version, now string,
	position int, extraction SagExtraction) error {
	if strings.TrimSpace(extraction.ChunkID) == "" || strings.TrimSpace(extraction.Event) == "" {
		return newStateError("SAG 索引写入失败：chunk_id 与事件不能为空")
	}
	eventID := sagEventID(namespace, extraction.ChunkID, position)
	if _, err := transaction.ExecContext(ctx,
		"INSERT INTO sag_events (id, namespace, document_id, chunk_id, event, version, created_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?)",
		eventID, namespace, documentID, extraction.ChunkID, extraction.Event, version, now); err != nil {
		if isUniqueViolation(err) {
			return newStateError("SAG 索引写入失败：chunk 重复")
		}
		return sagWriteError(err)
	}
	for _, name := range extraction.Entities {
		trimmed := strings.TrimSpace(name)
		if trimmed == "" {
			continue
		}
		entity, err := upsertSagEntity(ctx, transaction, namespace, trimmed, now)
		if err != nil {
			return err
		}
		if _, err := transaction.ExecContext(ctx,
			"INSERT INTO sag_event_entities (event_id, entity_id, namespace, created_at) VALUES (?, ?, ?, ?)",
			eventID, entity.ID, namespace, now); err != nil {
			if isUniqueViolation(err) {
				// The same entity mentioned twice in one chunk is stored once.
				continue
			}
			return sagWriteError(err)
		}
	}
	return nil
}

// upsertSagEntity resolves one entity name to its stable id, creating the row
// on first sight. The entity key is the casefolded chunk-independent form of
// the name so every chunk mentioning the same entity joins on the same row.
func upsertSagEntity(ctx context.Context, transaction *sql.Tx, namespace, name, now string) (SagEntity, error) {
	normalized := sagNormalizedKey(name)
	entityKey := normalized
	for {
		row := transaction.QueryRowContext(ctx,
			"SELECT id, namespace, normalized_key, entity_key, display_name, mention_count, created_at, updated_at "+
				"FROM sag_entities WHERE namespace = ? AND entity_key = ? FOR UPDATE",
			namespace, entityKey)
		entity, err := scanSagEntity(row)
		if errors.Is(err, sql.ErrNoRows) {
			id := sagEntityID(namespace, entityKey)
			result, insertErr := transaction.ExecContext(ctx,
				"INSERT INTO sag_entities (id, namespace, normalized_key, entity_key, display_name, mention_count, created_at, updated_at) "+
					"VALUES (?, ?, ?, ?, ?, 1, ?, ?)",
				id, namespace, normalized, entityKey, name, now, now)
			if insertErr != nil {
				if isUniqueViolation(insertErr) {
					// Lost an insert race inside the same transaction family;
					// re-read the winner.
					continue
				}
				return SagEntity{}, sagWriteError(insertErr)
			}
			if affected, affectedErr := result.RowsAffected(); affectedErr == nil && affected == 0 {
				continue
			}
			return SagEntity{
				ID:            id,
				Namespace:     namespace,
				NormalizedKey: normalized,
				EntityKey:     entityKey,
				DisplayName:   name,
				MentionCount:  1,
				CreatedAt:     now,
				UpdatedAt:     now,
			}, nil
		}
		if err != nil {
			return SagEntity{}, sagWriteError(err)
		}
		if _, err := transaction.ExecContext(ctx,
			"UPDATE sag_entities SET mention_count = mention_count + 1, updated_at = ? "+
				"WHERE namespace = ? AND entity_key = ?",
			now, namespace, entityKey); err != nil {
			return SagEntity{}, sagWriteError(err)
		}
		entity.MentionCount++
		entity.UpdatedAt = now
		return entity, nil
	}
}

// EventsByChunkIDs returns the events of the given chunk ids, keyed by
// chunk_id. Chunk ids without an event (extraction not run or failed) are
// absent from the map.
func (r *SagRepository) EventsByChunkIDs(ctx context.Context, namespace string, chunkIDs []string) (map[string]SagEvent, error) {
	result := make(map[string]SagEvent, len(chunkIDs))
	if len(chunkIDs) == 0 {
		return result, nil
	}
	placeholders := strings.Repeat("?,", len(chunkIDs))
	args := make([]any, 0, len(chunkIDs)+1)
	args = append(args, namespace)
	for _, chunkID := range chunkIDs {
		args = append(args, chunkID)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, namespace, document_id, chunk_id, event, version, created_at "+
			"FROM sag_events WHERE namespace = ? AND chunk_id IN ("+placeholders[:len(placeholders)-1]+")",
		args...)
	if err != nil {
		return nil, newStateError("SAG 索引读取失败")
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		event, err := scanSagEvent(rows)
		if err != nil {
			return nil, newStateError("SAG 索引读取失败")
		}
		result[event.ChunkID] = event
	}
	return result, rows.Err()
}

// EventsByEntities expands seed entities into their associated events. This
// is the query-time dynamic hyperedge: only the associations relevant to this
// query's seed entities are instantiated, never a global graph.
func (r *SagRepository) EventsByEntities(ctx context.Context, namespace string, entityIDs []string, limit int) ([]SagEvent, error) {
	return r.EventsByEntityIDs(ctx, namespace, entityIDs, limit)
}

// EntitiesByEvents returns the entities associated with the given event ids.
func (r *SagRepository) EntitiesByEvents(ctx context.Context, namespace string, eventIDs []string) ([]SagEntity, error) {
	if len(eventIDs) == 0 {
		return []SagEntity{}, nil
	}
	placeholders := strings.Repeat("?,", len(eventIDs))
	args := make([]any, 0, len(eventIDs)+1)
	args = append(args, namespace)
	for _, eventID := range eventIDs {
		args = append(args, eventID)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT DISTINCT n.id, n.namespace, n.normalized_key, n.entity_key, n.display_name, n.mention_count, n.created_at, n.updated_at "+
			"FROM sag_entities n JOIN sag_event_entities a ON a.entity_id = n.id AND a.namespace = n.namespace "+
			"WHERE n.namespace = ? AND a.event_id IN ("+placeholders[:len(placeholders)-1]+")",
		args...)
	if err != nil {
		return nil, newStateError("SAG 索引读取失败")
	}
	defer func() { _ = rows.Close() }()
	result := make([]SagEntity, 0)
	for rows.Next() {
		entity, err := scanSagEntity(rows)
		if err != nil {
			return nil, newStateError("SAG 索引读取失败")
		}
		result = append(result, entity)
	}
	return result, rows.Err()
}

// EntitiesByKeys resolves entity ids by normalized keys (the query-side seed
// path after seed-entity recall returns display names).
func (r *SagRepository) EntitiesByKeys(ctx context.Context, namespace string, keys []string) ([]SagEntity, error) {
	if len(keys) == 0 {
		return []SagEntity{}, nil
	}
	placeholders := strings.Repeat("?,", len(keys))
	args := make([]any, 0, len(keys)+1)
	args = append(args, namespace)
	for _, key := range keys {
		args = append(args, key)
	}
	rows, err := r.db.QueryContext(ctx,
		"SELECT id, namespace, normalized_key, entity_key, display_name, mention_count, created_at, updated_at "+
			"FROM sag_entities WHERE namespace = ? AND normalized_key IN ("+placeholders[:len(placeholders)-1]+")",
		args...)
	if err != nil {
		return nil, newStateError("SAG 索引读取失败")
	}
	defer func() { _ = rows.Close() }()
	result := make([]SagEntity, 0)
	for rows.Next() {
		entity, err := scanSagEntity(rows)
		if err != nil {
			return nil, newStateError("SAG 索引读取失败")
		}
		result = append(result, entity)
	}
	return result, rows.Err()
}

// EventsByEntityIDsRaw resolves events joined directly by entity ids.
func (r *SagRepository) EventsByEntityIDs(ctx context.Context, namespace string, entityIDs []string, limit int) ([]SagEvent, error) {
	if len(entityIDs) == 0 || limit <= 0 {
		return []SagEvent{}, nil
	}
	placeholders := strings.Repeat("?,", len(entityIDs))
	args := make([]any, 0, len(entityIDs)+2)
	args = append(args, namespace)
	for _, entityID := range entityIDs {
		args = append(args, entityID)
	}
	args = append(args, limit)
	rows, err := r.db.QueryContext(ctx,
		"SELECT e.id, e.namespace, e.document_id, e.chunk_id, e.event, e.version, e.created_at "+
			"FROM sag_events e "+
			"JOIN sag_event_entities a ON a.event_id = e.id AND a.namespace = e.namespace "+
			"WHERE e.namespace = ? AND a.entity_id IN ("+placeholders[:len(placeholders)-1]+") "+
			"ORDER BY e.created_at DESC LIMIT ?",
		args...)
	if err != nil {
		return nil, newStateError("SAG 索引读取失败")
	}
	defer func() { _ = rows.Close() }()
	result := make([]SagEvent, 0, limit)
	for rows.Next() {
		event, err := scanSagEvent(rows)
		if err != nil {
			return nil, newStateError("SAG 索引读取失败")
		}
		result = append(result, event)
	}
	return result, rows.Err()
}

// DocumentEventCount returns the number of indexed events of one document.
func (r *SagRepository) DocumentEventCount(ctx context.Context, namespace, documentID string) (int, error) {
	var count int
	if err := r.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sag_events WHERE namespace = ? AND document_id = ?",
		namespace, documentID).Scan(&count); err != nil {
		return 0, newStateError("SAG 索引读取失败")
	}
	return count, nil
}

// ListDocumentsWithoutEvents returns document ids of the namespace that have
// no SAG events yet (the backfill CLI's work list).
func (r *SagRepository) ListDocumentsWithoutEvents(ctx context.Context, namespace string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx,
		"SELECT d.document_id FROM documents d LEFT JOIN sag_events e "+
			"ON e.namespace = d.namespace AND e.document_id = d.document_id "+
			"WHERE d.namespace = ? AND d.del_flag = 0 AND e.id IS NULL "+
			"AND NOT EXISTS (SELECT 1 FROM sag_extraction_jobs j "+
			"WHERE j.namespace = d.namespace AND j.document_id = d.document_id)",
		namespace)
	if err != nil {
		return nil, newStateError("SAG 索引读取失败")
	}
	defer func() { _ = rows.Close() }()
	result := make([]string, 0)
	for rows.Next() {
		var documentID string
		if err := rows.Scan(&documentID); err != nil {
			return nil, newStateError("SAG 索引读取失败")
		}
		result = append(result, documentID)
	}
	return result, rows.Err()
}

// DeleteDocument removes every SAG index row of one document. It is called on
// the document delete path so the hyperedges never reference removed chunks.
func (r *SagRepository) DeleteDocument(ctx context.Context, namespace, documentID string) error {
	transaction, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return newStateError("SAG 索引删除失败")
	}
	defer func() { _ = transaction.Rollback() }()
	if err := deleteSagEvents(ctx, transaction, namespace, documentID); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return sagWriteError(err)
	}
	return nil
}

func scanSagEvent(scanner rowScanner) (SagEvent, error) {
	var event SagEvent
	if err := scanner.Scan(&event.ID, &event.Namespace, &event.DocumentID, &event.ChunkID,
		&event.Event, &event.Version, &event.CreatedAt); err != nil {
		return SagEvent{}, err
	}
	return event, nil
}

func scanSagEntity(scanner rowScanner) (SagEntity, error) {
	var entity SagEntity
	if err := scanner.Scan(&entity.ID, &entity.Namespace, &entity.NormalizedKey, &entity.EntityKey,
		&entity.DisplayName, &entity.MentionCount, &entity.CreatedAt, &entity.UpdatedAt); err != nil {
		return SagEntity{}, err
	}
	return entity, nil
}

func sagWriteError(err error) error {
	if err != nil {
		return newStateError("SAG 索引写入失败")
	}
	return nil
}

// sagNormalizedKey produces the casefolded normalization of an entity name
// (its join key). It stays dependency-free (state imports nothing beyond the
// standard library).
func sagNormalizedKey(name string) string {
	normalized := strings.TrimSpace(strings.ToLower(name))
	if normalized == "" {
		return name
	}
	return normalized
}

func deleteSagEvents(ctx context.Context, transaction *sql.Tx, namespace, documentID string) error {
	// Start the retention clock when associations disappear, including replacement.
	if _, err := transaction.ExecContext(ctx, "UPDATE sag_entities n JOIN sag_event_entities a ON a.namespace=n.namespace AND a.entity_id=n.id "+
		"JOIN sag_events e ON e.id=a.event_id AND e.namespace=a.namespace SET n.updated_at=? WHERE e.namespace=? AND e.document_id=?", nowISO(), namespace, documentID); err != nil {
		return sagWriteError(err)
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM sag_event_entities WHERE namespace = ? AND event_id IN "+
			"(SELECT id FROM sag_events WHERE namespace = ? AND document_id = ?)",
		namespace, namespace, documentID); err != nil {
		return sagWriteError(err)
	}
	if _, err := transaction.ExecContext(ctx,
		"DELETE FROM sag_events WHERE namespace = ? AND document_id = ?",
		namespace, documentID); err != nil {
		return sagWriteError(err)
	}
	return nil
}

// PurgeUnusedSagEntities runs after the content-retention window. An entity
// mentioned by any remaining event survives, regardless of mention_count.
func (s *Stores) PurgeUnusedSagEntities(ctx context.Context) error {
	cutoff := time.Now().UTC().Add(-time.Duration(ContentObjectRetentionSeconds) * time.Second).Format(isoTimestampFormat)
	_, err := s.DB.ExecContext(ctx, "DELETE FROM sag_entities WHERE updated_at<? AND NOT EXISTS (SELECT 1 FROM sag_event_entities a WHERE a.namespace=sag_entities.namespace AND a.entity_id=sag_entities.id) LIMIT 200", cutoff)
	return err
}
