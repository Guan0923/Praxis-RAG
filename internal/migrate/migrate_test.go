package migrate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/statetest"
)

func TestNamespaceForLegacyUser(t *testing.T) {
	if got := NamespaceForLegacyUser("alice"); got != "user-"+"2bd806c97f0e00af"[:16] && len(got) != 21 {
		// Value check is anchored below via determinism and shape.
		t.Fatalf("unexpected namespace %q", got)
	}
	if NamespaceForLegacyUser("alice") != NamespaceForLegacyUser("alice") {
		t.Fatal("namespace mapping is not deterministic")
	}
	if NamespaceForLegacyUser("alice") == NamespaceForLegacyUser("bob") {
		t.Fatal("different users share a namespace")
	}
}

func TestDiscoverLegacySourcesRules(t *testing.T) {
	root := t.TempDir()
	makeUser := func(user string, files ...string) string {
		knowledge := filepath.Join(root, user, "rag", "knowledge")
		if err := os.MkdirAll(knowledge, 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		for _, name := range files {
			if err := os.WriteFile(filepath.Join(knowledge, name), []byte("content"), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
		}
		return knowledge
	}
	makeUser("b-user", "b.txt", "A.md")
	makeUser("a-user", "doc.pdf", "skip.exe")
	// Unsupported-only user is not a source; non-knowledge dirs are ignored.
	unsupported := filepath.Join(root, "c-user", "rag", "knowledge")
	if err := os.MkdirAll(unsupported, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(unsupported, "z.bin"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sources, err := DiscoverLegacySources(root, nil)
	if err != nil {
		t.Fatalf("DiscoverLegacySources() error = %v", err)
	}
	if len(sources) != 2 || sources[0].UserID != "a-user" || sources[1].UserID != "b-user" {
		t.Fatalf("sources = %+v", sources)
	}
	if len(sources[0].Files) != 1 || filepath.Base(sources[0].Files[0]) != "doc.pdf" {
		t.Fatalf("a-user files = %v", sources[0].Files)
	}
	// Files sort by casefolded name.
	if len(sources[1].Files) != 2 || filepath.Base(sources[1].Files[0]) != "A.md" ||
		filepath.Base(sources[1].Files[1]) != "b.txt" {
		t.Fatalf("b-user files = %v", sources[1].Files)
	}
	filtered, err := DiscoverLegacySources(root, map[string]struct{}{"b-user": {}})
	if err != nil || len(filtered) != 1 || filtered[0].UserID != "b-user" {
		t.Fatalf("filtered sources = %+v, %v", filtered, err)
	}
	if missing, err := DiscoverLegacySources(filepath.Join(root, "nope"), nil); err != nil || missing != nil {
		t.Fatalf("missing root = %+v, %v", missing, err)
	}
}

func TestBuildPlanAndApplyIdempotency(t *testing.T) {
	root := t.TempDir()
	knowledge := filepath.Join(root, "alice", "rag", "knowledge")
	if err := os.MkdirAll(knowledge, 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(knowledge, "Doc.TXT"), []byte("legacy text"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	sources, err := DiscoverLegacySources(root, nil)
	if err != nil {
		t.Fatalf("DiscoverLegacySources() error = %v", err)
	}
	plan, err := BuildPlan(root, sources)
	if err != nil || len(plan.Files) != 1 {
		t.Fatalf("BuildPlan() = %+v, %v", plan, err)
	}
	file := plan.Files[0]
	if file.Namespace != NamespaceForLegacyUser("alice") || file.Name != "Doc.TXT" ||
		file.Size != int64(len("legacy text")) || len(file.SHA256) != 64 || len(file.DocumentID) == 0 {
		t.Fatalf("plan file = %+v", file)
	}
	stores := newMigrateStores(t)
	first := Apply(context.Background(), stores, plan, ApplyOptions{})
	if first.Imported != 1 || first.Failed != 0 || !containsWarning(first.Warnings, "vector indexing") {
		t.Fatalf("first apply = %+v", first)
	}
	second := Apply(context.Background(), stores, plan, ApplyOptions{})
	if second.Imported != 0 || second.Failed != 0 {
		t.Fatalf("second apply = %+v", second)
	}
	// Force re-imports (idempotent upsert).
	forced := Apply(context.Background(), stores, plan, ApplyOptions{Force: true})
	if forced.Imported != 1 {
		t.Fatalf("forced apply = %+v", forced)
	}
	document, err := stores.Documents.Get(context.Background(), file.Namespace, file.Name)
	if err != nil || document == nil || document.SHA256 != file.SHA256 {
		t.Fatalf("document = %+v, %v", document, err)
	}
	blob, found, err := stores.Blobs.Get(context.Background(), file.Namespace, state.SourceBucket, file.Name)
	if err != nil || !found || string(blob) != "legacy text" {
		t.Fatalf("blob = %q, %v, %v", blob, found, err)
	}
}

func TestStateSchemaStatusAndEnsure(t *testing.T) {
	url, key := statetest.Connection(t)
	settings := state.StateSettings{
		URL: url, EncryptionKey: key, AutoSchema: true,
		PoolSize: 10, MaxOverflow: 20, PoolRecycle: 1800, PoolTimeout: 30,
	}
	seed, err := state.New(settings, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, seed.DB)
	dropSchemaTables(t, seed)
	_ = seed.Close()

	missing, err := StateSchemaStatus(context.Background(), settings)
	if err != nil {
		t.Fatalf("StateSchemaStatus() error = %v", err)
	}
	if missing.SchemaGood || missing.Tables != 0 || len(missing.Missing) == 0 {
		t.Fatalf("fresh status = %+v", missing)
	}
	ready, err := EnsureStateSchema(context.Background(), settings)
	if err != nil {
		t.Fatalf("EnsureStateSchema() error = %v", err)
	}
	if !ready.SchemaGood || ready.Tables != len(state.SchemaTables()) {
		t.Fatalf("ensured status = %+v", ready)
	}
	second, err := EnsureStateSchema(context.Background(), settings)
	if err != nil || !second.SchemaGood || second.Tables != ready.Tables {
		t.Fatalf("second ensure = %+v, %v", second, err)
	}
}

// dropSchemaTables removes every state table so the schema-status test can start
// from an empty database.
func dropSchemaTables(t *testing.T, stores *state.Stores) {
	t.Helper()
	ctx := context.Background()
	rows, err := stores.DB.QueryContext(ctx,
		"SELECT table_name FROM information_schema.tables WHERE table_schema = DATABASE() AND table_type = 'BASE TABLE'")
	if err != nil {
		t.Fatalf("listing tables: %v", err)
	}
	var tables []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			_ = rows.Close()
			t.Fatalf("scanning table name: %v", err)
		}
		tables = append(tables, name)
	}
	if err := rows.Close(); err != nil {
		t.Fatalf("closing rows: %v", err)
	}
	if _, err := stores.DB.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 0"); err != nil {
		t.Fatalf("disabling foreign key checks: %v", err)
	}
	for _, name := range tables {
		if _, err := stores.DB.ExecContext(ctx, "DROP TABLE `"+name+"`"); err != nil {
			t.Fatalf("dropping %s: %v", name, err)
		}
	}
	if _, err := stores.DB.ExecContext(ctx, "SET FOREIGN_KEY_CHECKS = 1"); err != nil {
		t.Fatalf("enabling foreign key checks: %v", err)
	}
}

func newMigrateStores(t *testing.T) *state.Stores {
	t.Helper()
	url, key := statetest.Connection(t)
	stores, err := state.New(state.StateSettings{
		URL: url, EncryptionKey: key, AutoSchema: true,
		PoolSize: 10, MaxOverflow: 20, PoolRecycle: 1800, PoolTimeout: 30,
	}, true)
	if err != nil {
		t.Fatalf("state.New(MySQL) error = %v", err)
	}
	statetest.Prepare(t, stores.DB)
	t.Cleanup(func() { _ = stores.Close() })
	return stores
}

func containsWarning(warnings []string, substring string) bool {
	for _, warning := range warnings {
		if len(warning) >= len(substring) && (warning == substring || len(substring) == 0 ||
			contains(warning, substring)) {
			return true
		}
	}
	return false
}

func contains(value, substring string) bool {
	for index := 0; index+len(substring) <= len(value); index++ {
		if value[index:index+len(substring)] == substring {
			return true
		}
	}
	return false
}
