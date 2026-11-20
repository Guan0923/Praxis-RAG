package state

import (
	"context"
	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"testing"
	"time"
)

func TestSessionPresenceAndForceLogout(t *testing.T) {
	stores := newTestUserStores(t)
	ctx := audit.WithActor(context.Background(), "用户 admin")
	user, err := stores.Users.Create(ctx, "reader", "hash")
	if err != nil {
		t.Fatal(err)
	}
	check := func(now time.Time, presence string, count int) {
		t.Helper()
		users, err := stores.Sessions.ListSessionAccounts(ctx, now)
		if err != nil || len(users) != 1 || users[0].Presence != presence || users[0].Sessions != count {
			t.Fatalf("users=%+v err=%v", users, err)
		}
	}
	check(time.Now(), "offline", 0)
	expires := time.Now().Add(time.Hour).UTC().Format(isoTimestampFormat)
	for _, digest := range []string{"first", "second"} {
		if _, err := stores.Sessions.Create(ctx, user.ID, digest, "csrf", expires); err != nil {
			t.Fatal(err)
		}
	}
	check(time.Now(), "online", 2)
	check(time.Now().Add(6*time.Minute), "idle", 2)
	check(time.Now().Add(2*time.Hour), "offline", 0)
	for _, want := range []int64{2, 0} {
		count, err := stores.Sessions.ForceLogout(ctx, user.ID)
		if err != nil || count != want {
			t.Fatalf("revoked=%d err=%v", count, err)
		}
	}
	check(time.Now(), "offline", 0)
	for _, digest := range []string{"first", "second"} {
		session, err := stores.Sessions.ByDigest(ctx, digest)
		if err != nil || session == nil || !session.Revoked {
			t.Fatalf("session=%v err=%v", session, err)
		}
	}
	events, err := (audit.Store{DB: stores.DB}).List(ctx, audit.Filter{Action: "force_logout", Outcome: "success"}, 5)
	if err != nil || len(events) != 1 || events[0].Target != "reader（"+user.ID+"）" || events[0].Actor != "用户 admin" {
		t.Fatalf("events=%v err=%v", events, err)
	}
}

func TestAuditPersistenceAndCursor(t *testing.T) {
	stores := newTestStores(t)
	repo := audit.Store{DB: stores.DB}
	ctx := context.Background()
	entries := []audit.Entry{
		{ID: "01", Actor: "用户 bob（u-1）", Channel: "REST", Namespace: "manuals", Action: "read_document", Target: "a.md", Outcome: "success"},
		{ID: "02", Actor: "用户 bob（u-1）", Channel: "MCP", Namespace: "manuals", Action: "read_document", Target: "a.md", Outcome: "success"},
		{ID: "03", Actor: "用户 bob（u-1）", Channel: "REST", Namespace: "other", Action: "read_document", Target: "a.md", Outcome: "success"},
	}
	if err := repo.Append(ctx, entries); err != nil {
		t.Fatal(err)
	}
	filter := audit.Filter{Actor: "bob", Namespace: "manuals", Action: "read_document", Target: "a.md"}
	for _, want := range []string{"02", "01"} {
		items, err := repo.List(ctx, filter, 1)
		if err != nil || len(items) != 1 || items[0].ID != want {
			t.Fatalf("list: %v %v", items, err)
		}
		filter.Before = items[0].ID
	}
	items, err := repo.List(ctx, filter, 1)
	if err != nil || len(items) != 0 {
		t.Fatalf("last page: %v %v", items, err)
	}
}
