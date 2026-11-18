package state

import (
	"context"
	"errors"
	"testing"

	"github.com/Guan0923/Praxis-RAG/internal/statetest"
)

// newTestUserStores opens the disposable MySQL schema and truncates it.
func newTestUserStores(t *testing.T) *Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := New(StateSettings{
		URL:           url,
		EncryptionKey: key,
		AutoSchema:    true,
		PoolSize:      5,
		MaxOverflow:   10,
		PoolRecycle:   1800,
		PoolTimeout:   30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

func TestUserRepositoryFirstUserBecomesAdmin(t *testing.T) {
	stores := newTestUserStores(t)
	ctx := context.Background()

	first, err := stores.Users.Create(ctx, "alice", "hash-a")
	if err != nil {
		t.Fatalf("Create(first) error = %v", err)
	}
	if !first.IsAdmin() || first.Role != RoleAdmin || !first.Enabled {
		t.Fatalf("first user = %+v, want enabled admin", first)
	}
	if len(first.ID) != 32 || first.CreatedAt == "" || first.UpdatedAt == "" {
		t.Fatalf("first user fields = %+v", first)
	}
	second, err := stores.Users.Create(ctx, "bob", "hash-b")
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	if second.IsAdmin() || second.Role != RoleUser {
		t.Fatalf("second user = %+v, want normal user", second)
	}

	if _, err := stores.Users.Create(ctx, "alice", "hash-c"); err == nil {
		t.Fatalf("duplicate username accepted")
	} else {
		var conflict *StateConflict
		if !errors.As(err, &conflict) {
			t.Fatalf("duplicate error = %v, want *StateConflict", err)
		}
	}

	count, err := stores.Users.Count(ctx)
	if err != nil || count != 2 {
		t.Fatalf("Count() = %d, %v", count, err)
	}
	admins, err := stores.Users.CountEnabledAdmins(ctx)
	if err != nil || admins != 1 {
		t.Fatalf("CountEnabledAdmins() = %d, %v", admins, err)
	}
}

func TestUserRepositoryReadAndUpdate(t *testing.T) {
	stores := newTestUserStores(t)
	ctx := context.Background()
	created, err := stores.Users.Create(ctx, "alice", "hash-a")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	byName, hash, err := stores.Users.ByUsername(ctx, "alice")
	if err != nil || byName == nil || hash != "hash-a" || byName.ID != created.ID {
		t.Fatalf("ByUsername() = %+v, %q, %v", byName, hash, err)
	}
	if _, _, err := stores.Users.ByUsername(ctx, "missing"); err != nil {
		t.Fatalf("ByUsername(missing) error = %v", err)
	}
	missing, _, err := stores.Users.ByUsername(ctx, "missing")
	if err != nil || missing != nil {
		t.Fatalf("ByUsername(missing) = %+v, %v", missing, err)
	}
	byID, err := stores.Users.ByID(ctx, created.ID)
	if err != nil || byID == nil || byID.Username != "alice" {
		t.Fatalf("ByID() = %+v, %v", byID, err)
	}

	updated, err := stores.Users.Update(ctx, created.ID, RoleUser, false)
	if err != nil || updated == nil {
		t.Fatalf("Update() = %+v, %v", updated, err)
	}
	if updated.Role != RoleUser || updated.Enabled {
		t.Fatalf("updated user = %+v", updated)
	}
	missingUpdate, err := stores.Users.Update(ctx, "no-such-id", RoleUser, true)
	if err != nil || missingUpdate != nil {
		t.Fatalf("Update(missing) = %+v, %v", missingUpdate, err)
	}

	users, err := stores.Users.List(ctx)
	if err != nil || len(users) != 1 || users[0].Username != "alice" {
		t.Fatalf("List() = %+v, %v", users, err)
	}

	if err := stores.Users.TouchLogin(ctx, created.ID); err != nil {
		t.Fatalf("TouchLogin() error = %v", err)
	}
	touched, err := stores.Users.ByID(ctx, created.ID)
	if err != nil || touched == nil || touched.LastLoginAt == nil {
		t.Fatalf("LastLoginAt = %+v, %v", touched, err)
	}

	deleted, err := stores.Users.Delete(ctx, created.ID)
	if err != nil || !deleted {
		t.Fatalf("Delete() = %v, %v", deleted, err)
	}
	deletedAgain, err := stores.Users.Delete(ctx, created.ID)
	if err != nil || deletedAgain {
		t.Fatalf("Delete(again) = %v, %v", deletedAgain, err)
	}
}

func TestUserSessionLifecycle(t *testing.T) {
	stores := newTestUserStores(t)
	ctx := context.Background()
	user, err := stores.Users.Create(ctx, "alice", "hash-a")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}

	session, err := stores.Sessions.Create(ctx, user.ID, "digest-a", "csrf-a", "2099-01-01T00:00:00.000000+00:00")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	if session.ID == "" || session.CSRF != "csrf-a" || session.Revoked {
		t.Fatalf("session = %+v", session)
	}
	found, err := stores.Sessions.ByDigest(ctx, "digest-a")
	if err != nil || found == nil || found.UserID != user.ID || found.CSRF != "csrf-a" {
		t.Fatalf("ByDigest() = %+v, %v", found, err)
	}
	missing, err := stores.Sessions.ByDigest(ctx, "missing")
	if err != nil || missing != nil {
		t.Fatalf("ByDigest(missing) = %+v, %v", missing, err)
	}

	if err := stores.Sessions.Touch(ctx, session.ID); err != nil {
		t.Fatalf("Touch() error = %v", err)
	}
	if err := stores.Sessions.Revoke(ctx, session.ID); err != nil {
		t.Fatalf("Revoke() error = %v", err)
	}
	revoked, err := stores.Sessions.ByDigest(ctx, "digest-a")
	if err != nil || revoked == nil || !revoked.Revoked {
		t.Fatalf("revoked session = %+v, %v", revoked, err)
	}

	second, err := stores.Sessions.Create(ctx, user.ID, "digest-b", "csrf-b", "2099-01-01T00:00:00.000000+00:00")
	if err != nil {
		t.Fatalf("Create(second) error = %v", err)
	}
	_ = second
	if err := stores.Sessions.RevokeUserSessions(ctx, user.ID); err != nil {
		t.Fatalf("RevokeUserSessions() error = %v", err)
	}
	for _, digest := range []string{"digest-a", "digest-b"} {
		session, err := stores.Sessions.ByDigest(ctx, digest)
		if err != nil || session == nil || !session.Revoked {
			t.Fatalf("%s revoked = %+v, %v", digest, session, err)
		}
	}

	// Expired sessions are deleted by the sweep.
	expired, err := stores.Sessions.Create(ctx, user.ID, "digest-expired", "csrf", "2000-01-01T00:00:00.000000+00:00")
	if err != nil {
		t.Fatalf("Create(expired) error = %v", err)
	}
	_ = expired
	removed, err := stores.Sessions.DeleteExpired(ctx, "2001-01-01T00:00:00.000000+00:00")
	if err != nil || removed < 1 {
		t.Fatalf("DeleteExpired() = %d, %v", removed, err)
	}
	deleted, err := stores.Sessions.ByDigest(ctx, "digest-expired")
	if err != nil || deleted != nil {
		t.Fatalf("expired session still present: %+v, %v", deleted, err)
	}
}
