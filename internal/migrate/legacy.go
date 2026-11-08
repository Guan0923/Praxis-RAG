// Package migrate implements the explicit, non-destructive G8 migrations:
// legacy knowledge-source discovery (preview/apply) and shared-state schema
// reporting. It is a port of rag_mcp/migrate.py and parts of
// rag_mcp/state/migrate.py.
package migrate

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/chunking"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// MaxFilesPerBatch mirrors service_knowledge.MAX_FILES_PER_BATCH.
const MaxFilesPerBatch = 10

// SupportedExtensions mirrors document_readers.SUPPORTED_SUFFIXES.
var SupportedExtensions = map[string]struct{}{
	".doc": {}, ".docx": {}, ".md": {}, ".pdf": {}, ".txt": {},
}

// NamespaceForLegacyUser is the stable legacy-user namespace mapping.
// Python: "user-" + sha256(user_id)[:16].
func NamespaceForLegacyUser(userID string) string {
	digest := sha256.Sum256([]byte(userID))
	return "user-" + hex.EncodeToString(digest[:])[:16]
}

// LegacySource is one preserved legacy user whose knowledge files can migrate.
type LegacySource struct {
	UserID string
	Files  []string
}

// DiscoverLegacySources finds "<root>/<user>/rag/knowledge/<supported files>"
// without modifying either store. Symlinks are skipped; users are sorted by
// name and files by casefolded name.
func DiscoverLegacySources(root string, selectedUsers map[string]struct{}) ([]LegacySource, error) {
	root = filepath.Clean(root)
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return nil, nil
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	sources := make([]LegacySource, 0)
	for _, userID := range names {
		if len(selectedUsers) > 0 {
			if _, selected := selectedUsers[userID]; !selected {
				continue
			}
		}
		knowledge := filepath.Join(root, userID, "rag", "knowledge")
		info, err := os.Lstat(knowledge)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			continue
		}
		files, err := os.ReadDir(knowledge)
		if err != nil {
			continue
		}
		candidates := make([]string, 0)
		for _, file := range files {
			if !file.Type().IsRegular() {
				continue
			}
			extension := strings.ToLower(filepath.Ext(file.Name()))
			if _, supported := SupportedExtensions[extension]; !supported {
				continue
			}
			candidates = append(candidates, filepath.Join(knowledge, file.Name()))
		}
		sort.Slice(candidates, func(left, right int) bool {
			return strings.ToLower(filepath.Base(candidates[left])) < strings.ToLower(filepath.Base(candidates[right]))
		})
		if len(candidates) > 0 {
			sources = append(sources, LegacySource{UserID: userID, Files: candidates})
		}
	}
	return sources, nil
}

// PlanFile is one previewed legacy file.
type PlanFile struct {
	Path       string
	Name       string
	Namespace  string
	DocumentID string
	SHA256     string
	Size       int64
}

// Plan is the read-only migration preview.
type Plan struct {
	Root  string
	Users []string
	Files []PlanFile
}

// BuildPlan reads every source file once to report namespace/name/hash/size.
func BuildPlan(root string, sources []LegacySource) (Plan, error) {
	plan := Plan{Root: filepath.Clean(root), Users: make([]string, 0, len(sources))}
	for _, source := range sources {
		namespace := NamespaceForLegacyUser(source.UserID)
		plan.Users = append(plan.Users, fmt.Sprintf("%s -> %s (%d file(s))", source.UserID, namespace, len(source.Files)))
		for _, path := range source.Files {
			content, err := os.ReadFile(path)
			if err != nil {
				continue
			}
			name := filepath.Base(path)
			digest := sha256.Sum256(content)
			plan.Files = append(plan.Files, PlanFile{
				Path:       path,
				Name:       name,
				Namespace:  namespace,
				DocumentID: chunking.StableID("doc", strings.ToLower(name)),
				SHA256:     hex.EncodeToString(digest[:]),
				Size:       int64(len(content)),
			})
		}
	}
	return plan, nil
}

// ApplyOptions controls the apply phase; Apply is idempotent unless Force.
type ApplyOptions struct {
	Force      bool
	SkipAssets bool
}

// ApplyResult reports one apply run.
type ApplyResult struct {
	Imported int
	Failed   int
	Warnings []string
}

// Apply copies planned sources into the shared state (blobs + documents) in
// batches. Vector indexing is NOT performed here: the document worker pipeline
// owns reindexing, so the caller should enqueue/update afterwards. This is
// recorded in the CLI warning list.
func Apply(ctx context.Context, stores *state.Stores, plan Plan, options ApplyOptions) ApplyResult {
	result := ApplyResult{Warnings: []string{
		"vector indexing is not performed by this migration; reindex affected documents with the document worker pipeline",
	}}
	for _, file := range plan.Files {
		content, err := os.ReadFile(file.Path)
		if err != nil {
			result.Failed++
			result.Warnings = append(result.Warnings, fmt.Sprintf("%s/%s: source unreadable", file.Namespace, file.Name))
			continue
		}
		existing, err := stores.Documents.Get(ctx, file.Namespace, file.Name)
		if err != nil {
			result.Failed++
			continue
		}
		if existing != nil && !options.Force {
			blobExists, err := stores.Blobs.Exists(ctx, file.Namespace, state.SourceBucket, file.Name)
			if err == nil && blobExists {
				continue
			}
		}
		transaction, err := stores.DB.BeginTx(ctx, nil)
		if err != nil {
			result.Failed++
			continue
		}
		documentID := file.DocumentID
		if err := stores.Blobs.PutTx(ctx, transaction, file.Namespace, state.SourceBucket, file.Name, content, &documentID); err != nil {
			_ = transaction.Rollback()
			result.Failed++
			continue
		}
		record := state.DocumentRecord{
			Name: file.Name, DocumentID: documentID, Size: int64(len(content)), SHA256: file.SHA256,
			Collection: file.Namespace, Chunking: map[string]any{}, Diagnostics: []any{},
		}
		if err := stores.Documents.UpsertTx(ctx, transaction, file.Namespace, record); err != nil {
			_ = transaction.Rollback()
			result.Failed++
			continue
		}
		if err := transaction.Commit(); err != nil {
			result.Failed++
			continue
		}
		result.Imported++
	}
	return result
}
