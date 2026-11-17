package runtime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/modelapi"
	"github.com/Guan0923/xinghe_rag/internal/state"
)

// SystemEmbeddingUserID is the ledger account used for platform-owned
// re-indexing. Its usage is recorded for attribution but never charged to a
// customer.
const SystemEmbeddingUserID = "system"

// embeddingBillingSource returns the connection's billing owner marker. The
// default is platform-provided; only an administrator may set "customer" on a
// knowledge-base connection, and ordinary request bodies are never consulted.
func embeddingBillingSource(settings map[string]any) string {
	source, _ := settings["embedding_billing_source"].(string)
	source = strings.ToLower(strings.TrimSpace(source))
	if source == "customer" {
		return "customer"
	}
	return "platform"
}

// embeddingMeter binds one logical document embedding operation to its MySQL
// reservation. Automatic job retries use the same operation id, so a retried
// provider call cannot charge the customer twice.
type embeddingMeter struct {
	repository  *state.MembershipRepository
	operationID string
	totalChars  int64
	purpose     string

	mu           sync.Mutex
	successChars int64
	settled      bool
}

func (m *embeddingMeter) observe(_ int, texts []string) {
	var chars int64
	for _, text := range texts {
		chars += int64(utf8.RuneCountInString(text))
	}
	m.mu.Lock()
	m.successChars += chars
	m.mu.Unlock()
}

func (m *embeddingMeter) successfulChars() int64 {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.successChars
}

// settle records a fully successful embedding call. It keeps the reservation
// bound to the original quota period even if the call crossed a month boundary.
func (m *embeddingMeter) settle(ctx context.Context) error {
	if m == nil || m.repository == nil || m.settled {
		return nil
	}
	if _, err := m.repository.SettleEmbedding(ctx, m.operationID, m.totalChars); err != nil {
		logging.FromContext(ctx).Warn("文档 Embedding 额度结算失败",
			"operation_id", m.operationID, "error", err.Error())
		return err
	}
	m.settled = true
	return nil
}

// finishUnknown preserves the reservation as pending review when the provider
// outcome is unknown (timeout/crash). A recovery pass releases it after 24
// hours; a successful automatic retry settles the same operation id.
func (m *embeddingMeter) finishUnknown(ctx context.Context) error {
	if m == nil || m.repository == nil || m.settled {
		return nil
	}
	if _, err := m.repository.PendingReviewEmbedding(ctx, m.operationID, m.successfulChars()); err != nil {
		logging.FromContext(ctx).Warn("文档 Embedding 未知结果记录失败",
			"operation_id", m.operationID, "error", err.Error())
		return err
	}
	m.settled = true
	return nil
}

// prepareDocumentEmbeddingMeter reserves the whole document's known character
// count after chunking and before the first paid provider call. It returns nil
// for unowned/system knowledge bases and customer-paid connections, in which
// case no platform quota is touched.
func (s *Service) prepareDocumentEmbeddingMeter(
	ctx context.Context,
	namespace, documentName, documentVersion, model string,
	chars int64,
	purpose, userOverride string,
) (*embeddingMeter, error) {
	if s.stores == nil || s.stores.Membership == nil || chars <= 0 {
		return nil, nil
	}
	settings, err := s.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if embeddingBillingSource(settings) == "customer" {
		return nil, nil
	}
	owner := userOverride
	if owner == "" {
		owner, err = s.stores.KnowledgeBases.OwnerOf(ctx, namespace)
		if err != nil {
			return nil, err
		}
	}
	if owner == "" {
		// Legacy unowned libraries and system templates keep the administrative
		// path; their usage is recorded against the isolated system ledger for
		// attribution and is never charged to a visitor.
		owner = SystemEmbeddingUserID
	}
	operationID := "document:" + documentVersion
	if purpose == "reembed" {
		operationID = "reembed:" + documentVersion
	}
	reservation, err := s.stores.Membership.ReserveEmbedding(ctx, state.EmbeddingReservationInput{
		UserID:          owner,
		OperationID:     operationID,
		Namespace:       namespace,
		DocumentName:    documentName,
		DocumentVersion: documentVersion,
		Model:           model,
		Purpose:         purpose,
		Chars:           chars,
		Now:             s.now(),
	})
	if err != nil {
		return nil, err
	}
	_ = reservation
	return &embeddingMeter{
		repository:  s.stores.Membership,
		operationID: operationID,
		totalChars:  chars,
		purpose:     purpose,
	}, nil
}

// now is the runtime clock seam used by metering; tests may override it.
func (s *Service) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}

// meteredReembedder reserves each re-embedding batch before the provider call.
// Re-embedding is an administrator-triggered platform operation, so its usage
// is booked to the isolated system ledger, never to the knowledge-base owner.
type meteredReembedder struct {
	inner     *modelapi.HTTPEmbedder
	repo      *state.MembershipRepository
	namespace string
	model     string
	version   int
	batch     int
	now       func() time.Time
}

func (e *meteredReembedder) VectorSize() int { return e.inner.VectorSize() }

func (e *meteredReembedder) EmbedDocuments(ctx context.Context, texts []string) ([][]float64, error) {
	if e.repo == nil || len(texts) == 0 {
		return e.inner.EmbedDocuments(ctx, texts)
	}
	var chars int64
	for _, text := range texts {
		chars += int64(utf8.RuneCountInString(text))
	}
	if chars <= 0 {
		return e.inner.EmbedDocuments(ctx, texts)
	}
	operationID := fmt.Sprintf("reembed:%s:%d:%d", e.namespace, e.version, e.batch)
	e.batch++
	now := time.Now()
	if e.now != nil {
		now = e.now()
	}
	if _, err := e.repo.ReserveEmbedding(ctx, state.EmbeddingReservationInput{
		UserID:          SystemEmbeddingUserID,
		OperationID:     operationID,
		Namespace:       e.namespace,
		DocumentVersion: fmt.Sprintf("%d", e.version),
		Model:           e.model,
		Purpose:         "reembed",
		Chars:           chars,
		Now:             now,
	}); err != nil {
		return nil, err
	}
	meter := &embeddingMeter{repository: e.repo, operationID: operationID, totalChars: chars, purpose: "reembed"}
	e.inner.OnBatchSucceeded = meter.observe
	defer func() { e.inner.OnBatchSucceeded = nil }()
	vectors, err := e.inner.EmbedDocuments(ctx, texts)
	if err != nil {
		_ = meter.finishUnknown(ctx)
		return nil, err
	}
	if err := meter.settle(ctx); err != nil {
		return nil, err
	}
	return vectors, nil
}
