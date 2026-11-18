package runtime

import (
	"context"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
)

// UploadOperations returns the display-only recent upload operations.
func (s *Service) UploadOperations(ctx context.Context, namespace string) ([]map[string]any, error) {
	operations, err := s.stores.Views.UploadOperations(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Upload operations cannot be read."}
	}
	return operations, nil
}

// UploadOperation returns one display-only operation, or nil when it does not
// exist or belongs to another namespace.
func (s *Service) UploadOperation(ctx context.Context, namespace, operationID string) (map[string]any, bool, error) {
	operation, err := s.stores.Views.UploadOperation(ctx, namespace, operationID)
	if err != nil {
		return nil, false, &apierrors.KnowledgeError{Message: "Upload operation cannot be read."}
	}
	if operation == nil {
		return nil, false, nil
	}
	return operation, true, nil
}

// PublishedVersions returns the committed document version per name.
func (s *Service) PublishedVersions(ctx context.Context, namespace string) (map[string]string, error) {
	versions, err := s.stores.Views.PublishedVersions(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Published versions cannot be read."}
	}
	return versions, nil
}

// UploadLeases returns the namespace's current resource leases (display only).
func (s *Service) UploadLeases(ctx context.Context, namespace string) ([]map[string]any, error) {
	leases, err := s.stores.Views.Leases(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Upload leases cannot be read."}
	}
	return leases, nil
}
