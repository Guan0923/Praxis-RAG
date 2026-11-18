package redisstate

import (
	"context"
	"errors"
	"testing"

	"github.com/redis/go-redis/v9"

	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// TestDocumentLockTwoInstances drives the real journal against one real MySQL
// schema and real Redis with two independent Redis clients, one per simulated
// service instance. It proves the conflict matrix the persisted lease model has
// to keep after the backend migration: same document, same name, namespace
// exclusivity in both directions, and unaffected parallel uploads.
//
// It needs both RAG_TEST_MYSQL_URL (disposable schema) and RAG_TEST_REDIS_URL
// (disposable database; the test flushes it).
func TestDocumentLockTwoInstances(t *testing.T) {
	redisURL := redisIntegrationURL(t)
	mysqlURL, encryptionKey := statetest.Connection(t)
	ctx := context.Background()

	options, err := redisOptionsFromURL(redisURL, 2)
	if err != nil {
		t.Fatalf("redisOptionsFromURL: %v", err)
	}
	clientA := redis.NewClient(options)
	clientB := redis.NewClient(options)
	if err := clientA.FlushDB(ctx).Err(); err != nil {
		t.Fatalf("flushing the test Redis database: %v", err)
	}
	lockA, err := NewDocumentLock(lockSettings(t, redisURL), WithRedisClient(clientA))
	if err != nil {
		t.Fatalf("NewDocumentLock(A): %v", err)
	}
	lockB, err := NewDocumentLock(lockSettings(t, redisURL), WithRedisClient(clientB))
	if err != nil {
		t.Fatalf("NewDocumentLock(B): %v", err)
	}
	t.Cleanup(func() {
		_ = lockA.Close()
		_ = lockB.Close()
	})

	sts, err := state.New(state.StateSettings{
		URL:           mysqlURL,
		EncryptionKey: encryptionKey,
		AutoSchema:    true,
		PoolSize:      10,
		MaxOverflow:   20,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	t.Cleanup(func() { _ = sts.Close() })
	statetest.Prepare(t, sts.DB)

	newJournal := func(lock state.DocumentLocker) *state.UploadJournal {
		journal, err := state.NewUploadJournal(sts,
			state.WithDocumentLocker(lock),
			state.WithLeaseSeconds(60),
			state.WithAutoHeartbeat(false),
			state.WithCleanupDelaySeconds(0))
		if err != nil {
			t.Fatalf("NewUploadJournal() error = %v", err)
		}
		t.Cleanup(func() { journal.Close() })
		return journal
	}
	journalA := newJournal(lockA)
	journalB := newJournal(lockB)

	operationA, err := journalA.Begin(ctx, "ns", "file.txt", "doc-a", "collection", state.BeginOptions{})
	if err != nil {
		t.Fatalf("Begin(A) error = %v", err)
	}
	if _, err := journalB.Begin(ctx, "ns", "file.txt", "doc-a", "collection", state.BeginOptions{}); err == nil {
		t.Fatal("Begin(B, same document) succeeded")
	} else {
		var conflict *state.UploadConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("Begin(B, same document) error = %v, want UploadConflict", err)
		}
	}
	if _, err := journalB.Begin(ctx, "ns", "file.txt", "doc-b", "collection", state.BeginOptions{}); err == nil {
		t.Fatal("Begin(B, same name) succeeded")
	}
	// A different document still runs in parallel across the two instances.
	operationB, err := journalB.Begin(ctx, "ns", "other.txt", "doc-c", "collection", state.BeginOptions{})
	if err != nil {
		t.Fatalf("Begin(B, other document) error = %v", err)
	}
	// The namespace-exclusive operation conflicts with live document leases.
	if _, err := journalB.AcquireLock(ctx, "ns", nil); err == nil {
		t.Fatal("AcquireLock(exclusive) succeeded while document leases are live")
	}
	for _, current := range []struct {
		journal *state.UploadJournal
		op      *state.UploadOperation
	}{{journalA, operationA}, {journalB, operationB}} {
		if _, err := current.journal.Rollback(ctx, current.op, func(*state.UploadOperation) error { return nil }); err != nil {
			t.Fatalf("Rollback(%s) error = %v", current.op.ID, err)
		}
	}
	// With every document lease released the exclusive lock works and blocks a
	// new document write from the other instance.
	if err := journalA.WithLock(ctx, "ns", nil, func(token *state.LockToken) error {
		if _, err := journalB.Begin(ctx, "ns", "third.txt", "doc-d", "collection", state.BeginOptions{}); err == nil {
			t.Fatal("Begin(B) succeeded while A holds the namespace-exclusive lease")
		}
		transaction, err := sts.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = transaction.Rollback() }()
		if err := journalA.AssertLockTx(ctx, transaction, token); err != nil {
			return err
		}
		return transaction.Commit()
	}); err != nil {
		t.Fatalf("WithLock(exclusive) error = %v", err)
	}
}
