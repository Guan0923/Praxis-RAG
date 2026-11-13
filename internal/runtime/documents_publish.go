package runtime

import (
	"context"
	"net/http"
	"os"

	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/qdrant"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// documentIndexStore adapts one Qdrant store to the upload coordinator's
// compensation seams. DeleteVersion removes only one staged version (never a
// published head); DeleteLegacy removes unversioned points from a pre-G4 index.
type documentIndexStore struct {
	store *qdrant.Store
}

// DeleteVersion implements state.IndexPublisher.
func (p documentIndexStore) DeleteVersion(ctx context.Context, documentID, version string) error {
	return p.store.DeleteVersion(ctx, documentID, version)
}

// DeleteLegacy implements state.LegacyIndexPublisher.
func (p documentIndexStore) DeleteLegacy(ctx context.Context, documentID string) error {
	return p.store.DeleteLegacy(ctx, documentID)
}

// newDocumentStore builds a namespace-scoped Qdrant store for the current
// knowledge-base configuration. It is used for publishing (stage_version) and
// for coordinator compensation.
func (s *Service) newDocumentStore(ctx context.Context, namespace, collection string) (*qdrant.Store, error) {
	vectorSize := s.config.VectorSize
	if settings, settingsErr := s.stores.KnowledgeBases.Get(ctx, namespace); settingsErr == nil && settings != nil {
		if configured, valueErr := settingInteger(settings, "vector_size"); valueErr == nil {
			vectorSize = configured
		}
	}
	httpClient := &http.Client{Timeout: qdrant.DefaultTimeout}
	return qdrant.NewStore(qdrant.StoreConfig{
		Collection: collection,
		URL:        s.config.QdrantURL,
		Path:       s.config.QdrantPath,
		VectorSize: vectorSize,
		Namespace:  namespace,
		ActiveVersions: func() map[string]string {
			versions, err := s.stores.Documents.VisibleVersions(context.Background(), namespace)
			if err != nil {
				return nil
			}
			return versions
		},
		HTTPClient: httpClient,
	})
}

// indexStoreFactory resolves the compensation publisher for one journal
// operation ("version_store(op.collection)" in Python).
func (s *Service) indexStoreFactory(operation *state.UploadOperation) (state.IndexPublisher, error) {
	store, err := s.newDocumentStore(context.Background(), operation.Namespace, operation.Collection)
	if err != nil {
		return nil, err
	}
	return documentIndexStore{store: store}, nil
}

// uploadCoordinator builds the shared upload coordinator rooted at the system
// temporary directory (Python tempfile.gettempdir()), matching the workspace
// location of "rag-txn-<operation>" directories.
func (s *Service) uploadCoordinator() (*state.UploadCoordinator, error) {
	return state.NewUploadCoordinator(s.stores, os.TempDir())
}

// recoverUploads completes or compensates interrupted operations of one
// namespace before a new upload starts (Python coordinator.recover(...)).
func (s *Service) recoverUploads(ctx context.Context, namespace string) {
	coordinator, err := s.uploadCoordinator()
	if err != nil {
		logging.FromContext(ctx).Warn("Upload coordinator unavailable for recovery", logging.ErrorAttrs(err)...)
		return
	}
	coordinator.Recover(ctx, s.indexStoreFactory, &namespace)
}

// compensateUpload marks one failed operation and removes its staged vectors,
// workspace and journal state. Recovery failures are logged, never allowed to
// replace the original upload error.
func (s *Service) compensateUpload(ctx context.Context, coordinator *state.UploadCoordinator, operation *state.UploadOperation, cause error) {
	if err := s.stores.Uploads.Fail(ctx, operation, cause, nil); err != nil {
		logging.FromContext(ctx).Warn("Upload failure record pending", append([]any{"operation_id", operation.ID}, logging.ErrorAttrs(err)...)...)
	}
	var publisher state.IndexPublisher
	if store, err := s.indexStoreFactory(operation); err == nil {
		publisher = store
	} else {
		logging.FromContext(ctx).Warn("Upload compensation store unavailable", append([]any{"operation_id", operation.ID}, logging.ErrorAttrs(err)...)...)
	}
	if _, err := coordinator.Compensate(ctx, operation, publisher); err != nil {
		logging.FromContext(ctx).Warn("Upload recovery pending", append([]any{"operation_id", operation.ID}, logging.ErrorAttrs(err)...)...)
	}
}

// publishDocument writes the document metadata, source blob, parser artifacts
// and the journal COMMITTED status in one database transaction (Python's
// store_and_index publish callback). Vectors are already staged under the
// operation id and become visible through document_heads after this commit.
func (s *Service) publishDocument(
	ctx context.Context,
	operation *state.UploadOperation,
	record state.DocumentRecord,
	metadata map[string]any,
	content []byte,
	artifactBucket string,
	artifactDir string,
	task *state.DocumentJobTask,
) error {
	transaction, err := s.stores.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = transaction.Rollback() }()
	if _, err := s.stores.Uploads.AssertOwnerTx(ctx, transaction, operation); err != nil {
		return err
	}
	if err := s.stores.Documents.UpsertTx(ctx, transaction, operation.Namespace, record); err != nil {
		return err
	}
	documentID := operation.DocumentID
	if err := s.stores.Blobs.PutTx(ctx, transaction, operation.Namespace, state.SourceBucket,
		operation.Name, content, &documentID); err != nil {
		return err
	}
	if artifactBucket != "" && artifactDir != "" {
		if err := s.stores.Blobs.DeleteDocumentTx(ctx, transaction, operation.Namespace,
			artifactBucket, documentID); err != nil {
			return err
		}
		if _, err := s.stores.Blobs.PutTreeTx(ctx, transaction, operation.Namespace,
			artifactBucket, documentID, artifactDir); err != nil {
			return err
		}
	}
	// A queued document job may only publish while its admitted configuration
	// version is still current (Python process_document_task validate_publication).
	if task != nil {
		if err := s.stores.DocumentJobs.ValidatePublication(ctx, transaction, task); err != nil {
			return err
		}
	}
	if _, err := s.stores.Uploads.Commit(ctx, operation, metadata, transaction); err != nil {
		return err
	}
	if err := transaction.Commit(); err != nil {
		return err
	}
	// The publish transaction touched the documents/kb_list read models through
	// UpsertTx (which never flushes on its own). Refresh them now so cached
	// document lists and search snapshots see the published head immediately.
	s.stores.Documents.FlushViews(ctx, operation.Namespace)
	return nil
}
