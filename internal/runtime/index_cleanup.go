package runtime

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"github.com/Guan0923/Praxis-RAG/internal/lightrag"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// CleanupIndexBatch also recovers ready jobs left behind by a previous process.
func (s *Service) CleanupIndexBatch(ctx context.Context) (int, error) {
	jobs, err := s.stores.ReadyIndexJobs(ctx)
	if err != nil {
		return 0, err
	}
	var first error
	for _, job := range jobs {
		err := s.processIndexCleanup(ctx, job)
		if err != nil && first == nil {
			first = err
		}
		if ctx.Err() != nil {
			return 0, ctx.Err()
		}
	}
	return len(jobs), first
}

func (s *Service) cleanupIndexTarget(ctx context.Context, j state.IndexCleanup) error {
	if j.Target == "qdrant_scan" {
		return s.reconcileVectors(ctx, j.Namespace, j.Collection)
	}
	if j.Target == "graph_workspace" {
		return s.pruneRetiredWorkspace(ctx, j)
	}
	live, err := s.stores.CleanupProtected(ctx, j)
	if err != nil || live {
		return err
	}
	switch j.Target {
	case "qdrant":
		store, err := s.newDocumentStore(ctx, j.Namespace, j.Collection)
		if err != nil {
			return err
		}
		if j.Version == "" {
			return store.DeleteLegacy(ctx, j.DocumentID)
		}
		return store.DeleteVersion(ctx, j.DocumentID, j.Version)
	case "graph":
		client, err := s.graphClient()
		if err != nil {
			return err
		}
		if client == nil {
			return fmt.Errorf("graph service unavailable for cleanup")
		}
		revision, err := client.DocumentRevision(ctx, j.Namespace, j.DocumentID)
		if err != nil {
			return err
		}
		// The token precedes the authority read: if the document is recreated
		// after this read, a new sidecar registration rejects this old prune.
		live, err = s.stores.CleanupProtected(ctx, j)
		if err != nil || live {
			return err
		}
		var binding *lightrag.WorkspaceConfig
		settings, err := s.stores.KnowledgeBases.Get(ctx, j.Namespace)
		if err != nil {
			return err
		}
		if mirror, _ := s.resolveGraphMirror(settings); mirror != nil {
			cfg := mirror.workspaceConfig(j.Namespace)
			binding = &cfg
		}
		var active bool
		if err := s.stores.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM documents WHERE namespace=? AND document_id=? AND del_flag=0)", j.Namespace, j.DocumentID).Scan(&active); err != nil {
			return err
		}
		if err = client.PruneRevision(ctx, j.Namespace, j.DocumentID, revision, j.Version, !active, binding); err != nil {
			return err
		}
		if active {
			return nil
		}
		return s.stores.LightRAGDocuments.Delete(ctx, j.Namespace, j.DocumentID)
	default:
		return fmt.Errorf("unknown index cleanup target")
	}
}

// ReconcileIndexScope is explicit about storage ownership. It can be run even
// when the original namespace registration is missing from MySQL.
func (s *Service) ReconcileIndexScope(ctx context.Context, namespace, collection string) error {
	if namespace == "" || collection == "" {
		return fmt.Errorf("namespace and collection are required")
	}
	qerr := s.reconcileVectors(ctx, namespace, collection)
	client, err := s.graphClient()
	if err != nil || client == nil {
		return errors.Join(qerr, err)
	}
	after := ""
	for {
		ids, err := client.Inventory(ctx, namespace, after)
		if err != nil {
			return errors.Join(qerr, err)
		}
		if len(ids) == 0 {
			break
		}
		for _, id := range ids {
			if err := s.stores.ObserveIndex(ctx, state.IndexCleanup{Target: "graph", Namespace: namespace, Collection: collection, DocumentID: id.ID, Version: id.Version, Hash: id.Hash}); err != nil {
				return errors.Join(qerr, err)
			}
		}
		after = ids[len(ids)-1].ID
	}
	return qerr
}

func (s *Service) ReconcileIndexes(ctx context.Context) error {
	rows, err := s.stores.DB.QueryContext(ctx, "SELECT namespace,collection FROM knowledge_bases UNION SELECT namespace,collection FROM documents UNION SELECT namespace,collection FROM index_cleanup_jobs ORDER BY namespace,collection")
	if err != nil {
		return err
	}
	defer rows.Close()
	var scopes [][2]string
	for rows.Next() {
		var ns string
		var collection sql.NullString
		if err := rows.Scan(&ns, &collection); err != nil {
			return err
		}
		if collection.String != "" {
			scopes = append(scopes, [2]string{ns, collection.String})
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	var first error
	for _, scope := range scopes {
		if err := s.ReconcileIndexScope(ctx, scope[0], scope[1]); err != nil && first == nil {
			first = err
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	return first
}

func (s *Service) reconcileVectors(ctx context.Context, namespace, collection string) error {
	store, err := s.newDocumentStore(ctx, namespace, collection)
	if err != nil {
		return err
	}
	return store.WalkCleanupTargets(ctx, func(doc, version, hash string) error {
		return s.stores.ObserveIndex(ctx, state.IndexCleanup{Target: "qdrant", Namespace: namespace, Collection: collection, DocumentID: doc, Version: version, Hash: hash})
	})
}

// The namespace lease also covers concurrent uploads. A later KB with the
// same name owns its workspace; individual retired documents remain eligible.
func (s *Service) pruneRetiredWorkspace(ctx context.Context, j state.IndexCleanup) error {
	var live, retained bool
	err := s.stores.DB.QueryRowContext(ctx, "SELECT EXISTS(SELECT 1 FROM knowledge_bases WHERE namespace=?) OR EXISTS(SELECT 1 FROM documents WHERE namespace=? AND del_flag=0),"+
		"EXISTS(SELECT 1 FROM index_cleanup_jobs k JOIN content_objects c ON c.hash=k.source_hash WHERE k.namespace=?)", j.Namespace, j.Namespace, j.Namespace).Scan(&live, &retained)
	if err != nil || live {
		return err
	}
	if retained {
		return fmt.Errorf("workspace still has retained sources")
	}
	client, err := s.graphClient()
	if err != nil {
		return err
	}
	if client == nil {
		return fmt.Errorf("graph service unavailable for cleanup")
	}
	revision, err := client.DocumentRevision(ctx, j.Namespace, "")
	if err != nil {
		return err
	}
	if err = client.PruneRevision(ctx, j.Namespace, "", revision, "", false, nil); err != nil {
		return err
	}
	_, err = s.stores.DB.ExecContext(ctx, "DELETE FROM lightrag_documents WHERE namespace=?", j.Namespace)
	return err
}

func (s *Service) ProcessCleanupNotification(ctx context.Context, locator state.IndexCleanup) error {
	job, err := s.stores.LookupCleanup(ctx, locator)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return s.processIndexCleanup(ctx, job)
}

func (s *Service) processIndexCleanup(ctx context.Context, job state.IndexCleanup) error {
	return s.stores.RunIndexCleanup(ctx, job, func() error {
		resource := &job.DocumentID
		if job.DocumentID == "" {
			resource = nil
		}
		return s.stores.Uploads.WithLock(ctx, job.Namespace, resource, func(*state.LockToken) error {
			return s.cleanupIndexTarget(ctx, job)
		})
	})
}
