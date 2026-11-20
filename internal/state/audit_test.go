package state

import (
	"context"
	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"testing"
)

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
