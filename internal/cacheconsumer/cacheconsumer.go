// Package cacheconsumer consumes the cache-change topic and keeps the Redis
// read-model cache convergent with MySQL.
//
// Debezium captures the business tables that back the cached read models and
// routes their row changes to one dedicated topic. The broker payload carries
// only the locating identifiers (never row content), so this consumer maps a
// change to the affected cache kinds and scopes and asks the state layer to
// bump their revisions and rebuild the redacted views from MySQL.
//
// It is deliberately transport neutral: production wires MessageHandler into
// internal/mq.PullConsumer, while tests drive Handle and the recovery pass
// directly. Redis write failures never fail a message: the state layer records
// a dirty revision, and RunRecovery retries every unrecovered key.
package cacheconsumer

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// DefaultRecoveryInterval is the cadence of the dirty-key compensation pass.
// It matches the documented RAG_CACHE_WORKER_RECOVER_SECONDS default (30s).
const DefaultRecoveryInterval = 30 * time.Second

// DefaultRecoveryBatch bounds one compensation pass.
const DefaultRecoveryBatch = 200

// Change is one decoded row change from the cache-change topic. Before/After
// contain only the locating columns Debezium was configured to capture; a
// delete carries Before, an insert/update carries After.
type Change struct {
	Table  string
	Op     string
	Before map[string]any
	After  map[string]any
}

// Decoder decodes one broker payload. A record that is not a business change
// (tombstone, DDL, table outside the captured cache set) must return
// state.ErrIgnorableCDCChange so it is acknowledged without retries; any other
// error marks a malformed payload and the broker retry/dead-letter policy
// applies.
type Decoder func(body []byte) (Change, error)

// Refresher is the narrow cache-synchronizer seam: *state.CacheSynchronizer
// implements it. Revalidate bumps revisions and rebuilds the affected views;
// RefreshDirty is the bounded compensation pass for keys whose Redis write
// failed.
type Refresher interface {
	Revalidate(ctx context.Context, keys []state.CacheKey) error
	RefreshDirty(ctx context.Context, limit int) error
}

// Options configures a Processor. Refresher and Decoder are required.
type Options struct {
	Refresher Refresher
	Decoder   Decoder
	Logger    *slog.Logger

	// RecoveryInterval overrides the dirty-key compensation cadence.
	RecoveryInterval time.Duration
	// RecoveryBatch bounds one compensation pass (default 200).
	RecoveryBatch int
}

// Processor consumes cache-change events. Handle is safe for concurrent calls.
type Processor struct {
	refresher        Refresher
	decoder          Decoder
	logger           *slog.Logger
	recoveryInterval time.Duration
	recoveryBatch    int

	stopOnce sync.Once
	stop     chan struct{}

	recoverMu sync.Mutex
}

// New validates options and builds the processor.
func New(options Options) (*Processor, error) {
	if options.Refresher == nil {
		return nil, fmt.Errorf("cacheconsumer: Refresher is required")
	}
	if options.Decoder == nil {
		return nil, fmt.Errorf("cacheconsumer: Decoder is required")
	}
	logger := options.Logger
	if logger == nil {
		logger = slog.Default()
	}
	interval := options.RecoveryInterval
	if interval <= 0 {
		interval = DefaultRecoveryInterval
	}
	batch := options.RecoveryBatch
	if batch <= 0 {
		batch = DefaultRecoveryBatch
	}
	return &Processor{
		refresher:        options.Refresher,
		decoder:          options.Decoder,
		logger:           logger,
		recoveryInterval: interval,
		recoveryBatch:    batch,
		stop:             make(chan struct{}),
	}, nil
}

// Handle applies one change: map it to the affected cache keys, then invalidate
// and rebuild them. A change whose row identifiers are missing or whose table is
// not cached is acknowledged without work. A state-layer error is returned as
// nack so the broker retry/dead-letter policy applies; the recovery pass is the
// safety net for cache-backend failures, which are not errors here.
func (p *Processor) Handle(ctx context.Context, change Change) bool {
	keys := KeysForChange(change)
	if len(keys) == 0 {
		return true
	}
	if err := p.refresher.Revalidate(ctx, keys); err != nil {
		p.logWarn(ctx, "cache revalidation failed", append([]any{
			"table", change.Table, "op", change.Op, "keys", len(keys),
		}, logging.ErrorAttrs(err)...)...)
		return false
	}
	p.logInfo(ctx, "cache revalidated",
		"table", change.Table, "op", change.Op, "keys", len(keys))
	return true
}

// MessageHandler adapts the processor to mq.PullConsumer.
func (p *Processor) MessageHandler() mq.Handler {
	return func(ctx context.Context, message mq.ReceivedMessage) bool {
		change, err := p.decoder(message.Body)
		if err != nil {
			if state.IgnorableCDCChange(err) {
				// Tombstones, DDL and tables outside the captured cache set are
				// routine and must be acknowledged, never retried or dead-lettered.
				return true
			}
			p.logWarn(ctx, "malformed cache change message ignored", logging.ErrorAttrs(err)...)
			return false
		}
		return p.Handle(ctx, change)
	}
}

// Recover runs one bounded dirty-key compensation pass. It is safe to call while
// Handle runs; the state layer serializes revision work in MySQL.
func (p *Processor) Recover(ctx context.Context) {
	p.recoverMu.Lock()
	defer p.recoverMu.Unlock()
	if err := p.refresher.RefreshDirty(ctx, p.recoveryBatch); err != nil {
		p.logWarn(ctx, "cache dirty-key recovery failed", logging.ErrorAttrs(err)...)
	}
}

// RunRecovery runs the periodic compensation loop until ctx is done or Stop is
// called. It performs one pass immediately so a restart republishes unfinished
// work without waiting for the first tick.
func (p *Processor) RunRecovery(ctx context.Context) error {
	p.Recover(ctx)
	ticker := time.NewTicker(p.recoveryInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-p.stop:
			return nil
		case <-ticker.C:
			p.Recover(ctx)
		}
	}
}

// Stop ends RunRecovery. It is idempotent.
func (p *Processor) Stop() {
	p.stopOnce.Do(func() { close(p.stop) })
}

// RunConsumer starts an mq consumer whose handler is the cache-change handler
// and blocks until ctx is done, then stops the consumer. It is the production
// lifecycle.
func (p *Processor) RunConsumer(ctx context.Context, consumer mq.Consumer) error {
	if consumer == nil {
		return fmt.Errorf("cacheconsumer: consumer is required")
	}
	if err := consumer.Start(ctx); err != nil {
		return err
	}
	defer func() { _ = consumer.Stop() }()
	<-ctx.Done()
	return nil
}

// KeysForChange maps one business-table change to the cached read-model keys it
// invalidates. The mapping mirrors the write-path cache keys of internal/state
// so a directly executed UPDATE/DELETE/rename converges exactly like an
// application write.
//
// Unknown tables and deletes/updates whose locating identifiers are absent
// return no keys; callers acknowledge them.
func KeysForChange(change Change) []state.CacheKey {
	table := normalizeTable(change.Table)
	op := strings.ToLower(strings.TrimSpace(change.Op))
	if table == "" {
		return nil
	}
	var keys []state.CacheKey
	switch op {
	case "d":
		keys = append(keys, keysForRow(table, change.Before)...)
	case "u":
		keys = append(keys, keysForRow(table, change.After)...)
		// A directly executed UPDATE may move a row between scopes (for
		// example a cross-namespace UPDATE): refresh the old scope as well so
		// it converges immediately instead of waiting for the cache TTL.
		keys = append(keys, keysForRow(table, change.Before)...)
	default:
		keys = append(keys, keysForRow(table, change.After)...)
	}
	return deduplicateKeys(keys)
}

// keysForRow maps one row image to the cache keys it invalidates.
func keysForRow(table string, row map[string]any) []state.CacheKey {
	if row == nil {
		return nil
	}
	namespace, _ := textValue(row, "namespace")
	keys := make([]state.CacheKey, 0, 4)
	switch table {
	case "api_keys":
		keys = append(keys, state.CacheKey{Kind: "api_keys", Scope: ""})
	case "knowledge_bases":
		if namespace == "" {
			return nil
		}
		keys = append(keys,
			state.CacheKey{Kind: "kb_list", Scope: ""},
			state.CacheKey{Kind: "kb_public", Scope: namespace},
			state.CacheKey{Kind: "documents", Scope: namespace},
		)
	case "documents":
		if namespace == "" {
			return nil
		}
		keys = append(keys,
			state.CacheKey{Kind: "kb_list", Scope: ""},
			state.CacheKey{Kind: "documents", Scope: namespace},
		)
	case "mcp_catalogs":
		if namespace == "" {
			return nil
		}
		keys = append(keys, state.CacheKey{Kind: "catalog", Scope: namespace})
	case "document_heads":
		if namespace == "" {
			return nil
		}
		// The documents view joins document_heads for the published version.
		keys = append(keys,
			state.CacheKey{Kind: "heads", Scope: namespace},
			state.CacheKey{Kind: "documents", Scope: namespace},
		)
	case "document_leases":
		if namespace == "" {
			return nil
		}
		keys = append(keys, state.CacheKey{Kind: "leases", Scope: namespace})
	case "upload_operations":
		if namespace == "" {
			return nil
		}
		keys = append(keys, state.CacheKey{Kind: "upload_list", Scope: namespace})
		if operationID, ok := textValue(row, "id"); ok {
			keys = append(keys, state.CacheKey{Kind: "upload_status", Scope: operationID})
		}
	case "upload_steps":
		if operationID, ok := textValue(row, "operation_id"); ok {
			keys = append(keys, state.CacheKey{Kind: "upload_status", Scope: operationID})
		}
	}
	return keys
}

// normalizeTable strips an optional database qualifier so both
// "praxis_rag.documents" and "documents" map to the same cache keys.
func normalizeTable(table string) string {
	table = strings.TrimSpace(table)
	if index := strings.LastIndex(table, "."); index >= 0 {
		table = table[index+1:]
	}
	return strings.ToLower(table)
}

// textValue reads one locating string from a decoded row image. Debezium renders
// VARCHAR columns as strings; absent, null or non-string values yield ok=false.
func textValue(row map[string]any, key string) (string, bool) {
	value, present := row[key]
	if !present || value == nil {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	return text, true
}

func deduplicateKeys(keys []state.CacheKey) []state.CacheKey {
	seen := make(map[state.CacheKey]struct{}, len(keys))
	unique := make([]state.CacheKey, 0, len(keys))
	for _, key := range keys {
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, key)
	}
	return unique
}

// logWarn writes a warning enriched with the context correlation fields.
func (p *Processor) logWarn(ctx context.Context, message string, args ...any) {
	p.logger.Warn(message, append(logging.Attrs(ctx), args...)...)
}

// logInfo writes an informational line enriched with the context correlation
// fields.
func (p *Processor) logInfo(ctx context.Context, message string, args ...any) {
	p.logger.Info(message, append(logging.Attrs(ctx), args...)...)
}
