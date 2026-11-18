package runtime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/chunking"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/document"
	"github.com/Guan0923/Praxis-RAG/internal/ingest"
	"github.com/Guan0923/Praxis-RAG/internal/kbconfig"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/modelapi"
	"github.com/Guan0923/Praxis-RAG/internal/qdrant"
	"github.com/Guan0923/Praxis-RAG/internal/sparse"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// Upload limits mirror service_knowledge.MAX_FILES_PER_BATCH / MAX_FILE_BYTES.
const (
	maxFilesPerBatch = 10
	maxFileBytes     = 10 * 1024 * 1024
)

// supportedUploadSuffixes is the sorted SUPPORTED_SUFFIXES set used verbatim in
// the Python validation message.
var supportedUploadSuffixes = []string{".doc", ".docx", ".htm", ".html", ".md", ".pdf", ".txt"}

// legacyChunking mirrors chunk_strategies.LEGACY_CONFIG.
var legacyChunking = chunking.Config{Strategy: "character", ChunkSize: 700, ChunkOverlap: 100}

// documentOperationError carries the journal operation id on a failed write
// (Python sets exc.operation_id on the original exception). Unwrap keeps the
// safe error category observable to callers.
type documentOperationError struct {
	Err         error
	OperationID string
}

// remotePDFModelsRequiredMessage is the fixed Chinese rejection shown when a
// PDF is uploaded under the remote vision engine without the VLM configuration
// it needs. It is raised while the upload is accepted, before any journal/queue
// work, so users are sent to 模型配置 instead of seeing a parse failure later.
const remotePDFModelsRequiredMessage = "PDF 解析需要先在「模型配置」页添加视觉模型（VLM），并在知识库配置中选择后再上传。"

// doclingServiceRequiredMessage is the fixed Chinese rejection shown when an
// HTML/HTM file is uploaded before RAG_DOCLING_SERVICE_URL is configured.
// Static HTML has no in-process parser (reading it as text would index raw
// markup), so the upload is rejected at admission instead of failing later in
// the worker.
const doclingServiceRequiredMessage = "HTML 文档需要配置 Docling 解析服务（RAG_DOCLING_SERVICE_URL）。"

// Error implements error.
func (e *documentOperationError) Error() string { return e.Err.Error() }

// Unwrap exposes the original error.
func (e *documentOperationError) Unwrap() error { return e.Err }

// uploadResources is the resolved per-namespace stack used by one upload.
type uploadResources struct {
	settings       map[string]any
	chunking       chunking.Config
	tokenizerModel string
	embeddingModel string
	vlm            document.VLMSettings
	collection     string
	vectorSize     int
	embedder       *modelapi.HTTPEmbedder
	encoder        *sparse.Encoder
	store          *qdrant.Store
	// graph is the knowledge-graph mirror configuration resolved from the
	// knowledge-base settings. Nil means graph indexing is switched off for
	// this knowledge base (or the deployment has no sidecar), which every
	// caller treats as "do nothing".
	graph *graphMirror
}

// resolveUploadResources loads the knowledge-base configuration and builds the
// model/Qdrant stack for an upload. The Python runtime resolves the same values
// through its cached tenant.
func (s *Service) resolveUploadResources(ctx context.Context, namespace string) (*uploadResources, error) {
	settings, err := s.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if settings == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	collection, err := settingString(settings, "collection")
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无效"}
	}
	vectorSize, err := settingInteger(settings, "vector_size")
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "知识库配置无效"}
	}
	strategy, _ := settings["strategy"].(string)
	chunkSize, _ := settingInteger(settings, "chunk_size")
	chunkOverlap, _ := settingInteger(settings, "chunk_overlap")
	configuration := chunking.Config{Strategy: strategy, ChunkSize: chunkSize, ChunkOverlap: chunkOverlap}
	if _, err := chunking.NewConfig(configuration.Strategy, configuration.ChunkSize, configuration.ChunkOverlap); err != nil {
		return nil, &apierrors.KnowledgeError{Message: err.Error()}
	}
	tokenizerModel, _ := settings["tokenizer"].(string)
	if tokenizerModel == "" {
		tokenizerModel = kbconfig.DefaultTokenizerModel
	}
	embeddingModel, _ := settings["embedding_model"].(string)
	embeddingConnection, err := kbconfig.ModelConnectionForKey(settings, "embedding", s.stores.Settings.EncryptionKey)
	if err != nil {
		return nil, adminError(err)
	}
	vlm, err := s.uploadVLM(settings)
	if err != nil {
		return nil, err
	}
	store, err := s.newDocumentStore(ctx, namespace, collection)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "Qdrant 索引不可用，请检查 Qdrant 服务配置"}
	}
	// A knowledge base that switched graph indexing on must have a usable
	// graph model and a configured sidecar before any upload is admitted;
	// otherwise the document would be indexed but silently absent from the
	// graph.
	graph, err := s.resolveGraphMirror(settings)
	if err != nil {
		return nil, err
	}
	return &uploadResources{
		settings:       settings,
		chunking:       configuration,
		tokenizerModel: tokenizerModel,
		embeddingModel: embeddingModel,
		vlm:            vlm,
		collection:     collection,
		vectorSize:     vectorSize,
		embedder:       modelapi.NewHTTPEmbedder(embeddingConnection, modelapi.DefaultTimeout),
		encoder:        sparse.NewDefaultEncoder(),
		store:          store,
		graph:          graph,
	}, nil
}

// validateUploads mirrors KnowledgeDocumentStore.validate_uploads.
func (s *Service) validateUploads(files []UploadFile) error {
	if len(files) == 0 {
		return &apierrors.KnowledgeValidationError{Message: "请选择要上传的 Markdown、TXT、PDF 或 DOCX 文件。"}
	}
	if len(files) > maxFilesPerBatch {
		return &apierrors.KnowledgeValidationError{Message: fmt.Sprintf("单次最多上传 %d 个知识文件。", maxFilesPerBatch)}
	}
	for _, file := range files {
		name, err := SanitizeName(file.Name)
		if err != nil {
			return &apierrors.KnowledgeValidationError{Message: err.Error()}
		}
		if !document.IsSupportedSuffix(filepath.Ext(name)) {
			return &apierrors.KnowledgeValidationError{
				Message: fmt.Sprintf("暂不支持该文件类型：%s；请选择 %s。", name, strings.Join(supportedUploadSuffixes, "、")),
			}
		}
	}
	return nil
}

// validateRemotePDFUpload rejects a PDF upload under the remote vision engine
// before any journal/queue work when the knowledge base has no VLM (Console
// knowledge base setting or RAG_VLM_* environment) with a model configured. The
// message sends the user to 模型配置 instead of failing later at parse time.
func (s *Service) validateRemotePDFUpload(files []UploadFile, resources *uploadResources) error {
	if !config.IsRemotePDFEngine(s.config.PDFEngine) || resources == nil {
		return nil
	}
	hasPDF := false
	for _, file := range files {
		name, err := SanitizeName(file.Name)
		if err != nil {
			continue
		}
		if strings.EqualFold(filepath.Ext(name), document.SuffixPDF) {
			hasPDF = true
			break
		}
	}
	if !hasPDF {
		return nil
	}
	if resources.vlm.Configured() && strings.TrimSpace(resources.vlm.Model) != "" {
		return nil
	}
	return &apierrors.KnowledgeValidationError{Message: remotePDFModelsRequiredMessage}
}

// validateRemotePDFAdmission is the asynchronous counterpart of
// validateRemotePDFUpload: a PDF admitted to the document-job queue under the
// remote vision engine is rejected while the upload is admitted when the
// knowledge base has no VLM configuration. The Console therefore shows the
// actionable Chinese message instead of a generic failed job after parsing.
func (s *Service) validateRemotePDFAdmission(ctx context.Context, namespace, name string) error {
	if !config.IsRemotePDFEngine(s.config.PDFEngine) {
		return nil
	}
	safeName, err := SanitizeName(name)
	if err != nil || !strings.EqualFold(filepath.Ext(safeName), document.SuffixPDF) {
		return nil
	}
	settings, err := s.stores.KnowledgeBases.Get(ctx, namespace)
	if err != nil {
		return &apierrors.KnowledgeError{Message: "知识库配置无法读取"}
	}
	if settings == nil {
		return &apierrors.KnowledgeValidationError{Message: kbconfig.MessageKnowledgeBaseMissing}
	}
	vlm, err := s.uploadVLM(settings)
	if err != nil {
		return err
	}
	return s.validateRemotePDFUpload(
		[]UploadFile{{Name: safeName}}, &uploadResources{vlm: vlm})
}

// doclingEnabled reports whether the Docling HTTP service is configured. It
// selects the structured parser for DOC/DOCX and is the only way static HTML
// can be imported at all.
func (s *Service) doclingEnabled() bool {
	return strings.TrimSpace(s.config.DoclingServiceURL) != ""
}

// validateDoclingHTMLUpload rejects HTML/HTM uploads before any journal/queue
// work when the Docling service is not configured. DOCX keeps the in-process
// parser in that case, so only the HTML family is rejected here.
func (s *Service) validateDoclingHTMLUpload(files []UploadFile) error {
	if s.doclingEnabled() {
		return nil
	}
	for _, file := range files {
		name, err := SanitizeName(file.Name)
		if err != nil {
			continue
		}
		switch strings.ToLower(filepath.Ext(name)) {
		case document.SuffixHTML, document.SuffixHTM:
			return &apierrors.KnowledgeValidationError{Message: doclingServiceRequiredMessage}
		}
	}
	return nil
}

// validateDoclingHTMLAdmission is the asynchronous counterpart of
// validateDoclingHTMLUpload: a queued HTML job is rejected while it is admitted
// rather than failing later in the worker with a generic parse error.
func (s *Service) validateDoclingHTMLAdmission(name string) error {
	return s.validateDoclingHTMLUpload([]UploadFile{{Name: name}})
}

// uploadDocuments is the real implementation behind Service.Upload: a batch of
// synchronous uploads, one journaled transaction per file, with per-file error
// isolation and the post-write document list.
func (s *Service) uploadDocuments(
	ctx context.Context,
	namespace string,
	files []UploadFile,
	idempotencyKey string,
) (map[string]any, error) {
	resources, err := s.resolveUploadResources(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if err := s.validateUploads(files); err != nil {
		return nil, err
	}
	if err := s.validateRemotePDFUpload(files, resources); err != nil {
		return nil, err
	}
	if err := s.validateDoclingHTMLUpload(files); err != nil {
		return nil, err
	}
	imported := make([]map[string]any, 0, len(files))
	skipped := make([]map[string]any, 0)
	failed := make([]map[string]any, 0)
	for position, file := range files {
		requestKey := ""
		if idempotencyKey != "" {
			requestKey = sha256Hex(fmt.Sprintf("%s:%d", idempotencyKey, position))
		}
		record, err := s.storeAndIndex(ctx, namespace, resources, storeIndexOptions{
			Name:                   file.Name,
			Content:                file.Content,
			IdempotencyKey:         requestKey,
			DeduplicateAcrossNames: true,
			EmbeddingModel:         resources.embeddingModel,
		})
		if err != nil {
			name, _ := SanitizeName(file.Name)
			failedRecord := map[string]any{"name": name, "error": safeUploadErrorText(err)}
			var operationError *documentOperationError
			if errors.As(err, &operationError) {
				failedRecord["operation_id"] = operationError.OperationID
			}
			failed = append(failed, failedRecord)
			continue
		}
		if unchanged, _ := record["skipped"].(bool); unchanged {
			delete(record, "skipped")
			skipped = append(skipped, record)
		} else {
			imported = append(imported, record)
		}
	}
	documents, err := s.ListDocuments(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"imported":  imported,
		"skipped":   skipped,
		"failed":    failed,
		"documents": documents,
	}, nil
}

// writeDocumentContent is the real implementation behind Service.WriteDocument:
// synchronous create/update with overwrite and idempotency rules.
func (s *Service) writeDocumentContent(
	ctx context.Context,
	namespace, name string,
	content []byte,
	overwrite bool,
	idempotencyKey string,
) (map[string]any, error) {
	resources, err := s.resolveUploadResources(ctx, namespace)
	if err != nil {
		return nil, err
	}
	safeName, err := SanitizeName(name)
	if err != nil || safeName != name {
		return nil, &apierrors.KnowledgeValidationError{Message: "Invalid document name"}
	}
	records, err := s.stores.Documents.List(ctx, namespace)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "文档记录无法读取"}
	}
	var existing *state.DocumentRecord
	for index := range records {
		if records[index].Name == name {
			existing = &records[index]
			break
		}
	}
	if idempotencyKey == "" && overwrite && existing == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: "Document does not exist: " + name}
	}
	if idempotencyKey == "" && !overwrite {
		duplicate := false
		for _, record := range records {
			if strings.EqualFold(record.Name, name) {
				duplicate = true
				break
			}
		}
		if duplicate {
			return nil, &apierrors.KnowledgeValidationError{Message: "Document already exists: " + name + "; use update_document"}
		}
		if stored, lookupErr := s.stores.Documents.Get(ctx, namespace, name); lookupErr == nil && stored != nil {
			return nil, &apierrors.KnowledgeValidationError{Message: "Document already exists: " + name + "; use update_document"}
		}
	}
	if err := s.validateUploads([]UploadFile{{Name: name, Content: content}}); err != nil {
		return nil, err
	}
	if err := s.validateRemotePDFUpload([]UploadFile{{Name: name, Content: content}}, resources); err != nil {
		return nil, err
	}
	if err := s.validateDoclingHTMLUpload([]UploadFile{{Name: name, Content: content}}); err != nil {
		return nil, err
	}
	record, err := s.storeAndIndex(ctx, namespace, resources, storeIndexOptions{
		Name:                   name,
		Content:                content,
		IdempotencyKey:         idempotencyKey,
		DeduplicateAcrossNames: false,
		Overwrite:              &overwrite,
		EmbeddingModel:         resources.embeddingModel,
	})
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: safeUploadErrorText(err)}
	}
	unchanged, _ := record["skipped"].(bool)
	delete(record, "skipped")
	delete(record, "requested_name")
	status := "added"
	if overwrite {
		status = "updated"
	}
	if unchanged {
		status = "unchanged"
	}
	documents, err := s.ListDocuments(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return map[string]any{"status": status, "document": record, "documents": documents}, nil
}

// deleteDocumentByName is the real implementation behind Service.Delete and
// Service.DeleteDocument: a logical (del_flag) delete under the document lock.
func (s *Service) deleteDocumentByName(ctx context.Context, namespace, name string) (map[string]any, error) {
	safeName, err := SanitizeName(name)
	if err != nil {
		return nil, &apierrors.KnowledgeValidationError{Message: err.Error()}
	}
	record, err := s.stores.Documents.Get(ctx, namespace, safeName)
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "文档记录无法读取"}
	}
	if record == nil {
		return nil, &apierrors.KnowledgeValidationError{Message: "知识文件不存在：" + safeName}
	}
	documentID := record.DocumentID
	if documentID == "" {
		return nil, &apierrors.KnowledgeError{Message: "知识文件索引记录损坏：" + safeName}
	}
	journal := s.stores.Uploads
	if journal == nil {
		return nil, &apierrors.KnowledgeError{Message: "上传事务存储不可用"}
	}
	lockErr := journal.WithLock(ctx, namespace, &documentID, func(token *state.LockToken) error {
		transaction, err := s.stores.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = transaction.Rollback() }()
		if err := journal.AssertLockTx(ctx, transaction, token); err != nil {
			return err
		}
		changed, err := s.stores.Documents.DeleteTx(ctx, transaction, namespace, safeName)
		if err != nil {
			return err
		}
		if !changed {
			return &apierrors.KnowledgeValidationError{Message: "知识文件不存在：" + safeName}
		}
		return transaction.Commit()
	})
	if lockErr != nil {
		var validation *apierrors.KnowledgeValidationError
		if errors.As(lockErr, &validation) {
			return nil, validation
		}
		var conflict *state.UploadConflict
		if errors.As(lockErr, &conflict) {
			return nil, &apierrors.KnowledgeValidationError{Message: "文档正在更新，请稍后再删除"}
		}
		var lockUnavailable *state.LockUnavailable
		if errors.As(lockErr, &lockUnavailable) {
			return nil, &apierrors.KnowledgeError{Message: documentLockUnavailableMessage}
		}
		var knowledge *apierrors.KnowledgeError
		if errors.As(lockErr, &knowledge) {
			return nil, knowledge
		}
		return nil, &apierrors.KnowledgeError{Message: "知识文件删除失败"}
	}
	// The deletion already committed. Removing the graph copy is best effort:
	// a sidecar outage must not make a successful delete look failed, and the
	// ledger row is removed with the graph copy so a stale copy cannot be
	// mistaken for a current one.
	s.removeDocumentGraph(ctx, namespace, documentID)
	documents, err := s.ListDocuments(ctx, namespace)
	if err != nil {
		return nil, err
	}
	return map[string]any{"deleted": recordMap(*record), "documents": documents}, nil
}

// storeIndexOptions are the per-document arguments of storeAndIndex.
type storeIndexOptions struct {
	Name                   string
	Content                []byte
	IdempotencyKey         string
	DeduplicateAcrossNames bool
	Overwrite              *bool
	EmbeddingModel         string
	// Task, when non-nil, binds this publication to one queued document job
	// (document-walker execution): stage progress, journal Bind and
	// DocumentJobs.ValidatePublication before the journal commit. Nil keeps the
	// synchronous upload wording and behavior unchanged.
	Task *state.DocumentJobTask
}

// indexConfiguration mirrors _index_configuration + ChunkingConfig.metadata +
// the embedding model suffix used for deduplication and stored metadata. The
// PDF parser version comes from the configured engine so stored documents
// record the parser that actually produced their content; doclingEnabled does
// the same for DOC/DOCX (Docling service vs in-process OOXML parser). HTML is
// always parsed by Docling, so it records the HTML parser contract.
func indexConfiguration(name string, configuration chunking.Config, tokenizerModel, embeddingModel, pdfParserVersion string, doclingEnabled bool) map[string]any {
	metadata := configuration.Metadata(tokenizerModel)
	suffix := strings.ToLower(filepath.Ext(name))
	switch suffix {
	case document.SuffixPDF:
		metadata["pdf_parser_version"] = pdfParserVersion
	case document.SuffixDOC, document.SuffixDOCX:
		if doclingEnabled {
			metadata["word_parser_version"] = document.DoclingWordParserVersion
		} else {
			metadata["word_parser_version"] = document.WordParserVersion
		}
	case document.SuffixHTML, document.SuffixHTM:
		metadata["html_parser_version"] = document.HTMLParserVersion
	}
	metadata["embedding_model"] = embeddingModel
	return metadata
}

// storeAndIndex ports KnowledgeDocumentStore.store_and_index: write-ahead
// journal, immutable staging, then one atomic publication.
func (s *Service) storeAndIndex(
	ctx context.Context,
	namespace string,
	resources *uploadResources,
	options storeIndexOptions,
) (map[string]any, error) {
	name, err := SanitizeName(options.Name)
	if err != nil {
		return nil, &apierrors.KnowledgeValidationError{Message: err.Error()}
	}
	if options.IdempotencyKey != "" && len(options.IdempotencyKey) > 200 {
		return nil, &apierrors.KnowledgeValidationError{Message: "Invalid idempotency key"}
	}
	configuration := indexConfiguration(name, resources.chunking, resources.tokenizerModel,
		options.EmbeddingModel, document.PDFParserVersionForEngine(s.config.PDFEngine), s.doclingEnabled())
	journal := s.stores.Uploads
	if journal == nil {
		return nil, &apierrors.KnowledgeError{Message: "上传事务存储不可用"}
	}
	coordinator, err := s.uploadCoordinator()
	if err != nil {
		return nil, &apierrors.KnowledgeError{Message: "上传协调器不可用"}
	}
	s.recoverUploads(ctx, namespace)

	documentID := chunking.StableID("doc", sparse.CaseFold(name))
	fingerprint := uploadFingerprint(name, configuration, options.Overwrite)
	var idempotencyKey *string
	if options.IdempotencyKey != "" {
		key := options.IdempotencyKey
		idempotencyKey = &key
	}
	operation, err := journal.Begin(ctx, namespace, name, documentID, resources.collection, state.BeginOptions{
		IdempotencyKey: idempotencyKey,
		Fingerprint:    &fingerprint,
	})
	if err != nil {
		return nil, err
	}
	// Correlate every following log line and persisted reason with the durable
	// operation (and, for a queued job, the artifact). Annotate extends the
	// enclosing HTTP request's field set in place so its access log line can
	// report the ids; it is a no-op outside a request context. Annotate must run
	// on the incoming context: With forks the attribute set, so annotating the
	// derived context would not reach the request.
	annotations := []any{logging.FieldOperationID, operation.ID}
	if options.Task != nil {
		annotations = append(annotations, logging.FieldArtifactID, options.Task.ArtifactID)
	}
	logging.Annotate(ctx, annotations...)
	ctx = logging.WithOperationID(ctx, operation.ID)
	if options.Task != nil {
		ctx = logging.WithArtifactID(ctx, options.Task.ArtifactID)
	}
	if operation.Status == state.UploadCommitted {
		return replayCommittedUpload(operation, options.Content, name)
	}
	if options.Task != nil {
		s.stores.DocumentJobs.Bind(options.Task, operation)
	}

	workspace := filepath.Join(os.TempDir(), "rag-txn-"+operation.ID)
	temporary := filepath.Join(workspace, name)
	if _, err := journal.RegisterWorkspace(ctx, operation, workspace, nil); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation, err)
	}
	currentSettings, settingsErr := s.stores.KnowledgeBases.Get(ctx, namespace)
	if settingsErr != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("configuration", settingsErr))
	}
	if collection, ok := currentSettings["collection"].(string); currentSettings != nil && ok && collection != resources.collection {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("configuration",
				&state.UploadConflict{Message: "Knowledge-base index changed; retry with refreshed settings"}))
	}

	sha256Value := sha256HexBytes(options.Content)
	size := int64(len(options.Content))
	received, err := journal.RunStepContext(ctx, operation, "receive_source", func(actionContext context.Context) (any, error) {
		ctx := actionContext
		if err := os.MkdirAll(workspace, 0o755); err != nil {
			return nil, err
		}
		digest, size, err := writeBoundedDocument(temporary, options.Content, name)
		if err != nil {
			return nil, err
		}
		if err := journal.PutObject(ctx, operation, state.SourceBucket, name, options.Content, nil); err != nil {
			return nil, err
		}
		return map[string]any{"sha256": digest, "size": size}, nil
	}, nil)
	if err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation, err)
	}
	_ = received

	existing, err := s.stores.Documents.Get(ctx, namespace, name)
	if err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation, err)
	}
	if options.Overwrite != nil && *options.Overwrite {
		if existing == nil {
			return s.failDocumentOperation(ctx, journal, coordinator, operation,
				&apierrors.KnowledgeValidationError{Message: "Document does not exist: " + name})
		}
	} else if options.Overwrite != nil && !*options.Overwrite {
		duplicate := existing != nil
		if !duplicate {
			records, listErr := s.stores.Documents.List(ctx, namespace)
			if listErr == nil {
				for _, record := range records {
					if strings.EqualFold(record.Name, name) {
						duplicate = true
						break
					}
				}
			}
		}
		if duplicate {
			return s.failDocumentOperation(ctx, journal, coordinator, operation,
				&apierrors.KnowledgeValidationError{Message: "Document already exists: " + name + "; use update_document"})
		}
	}

	var onlyName *string
	if !options.DeduplicateAcrossNames {
		onlyName = &name
	}
	previousValue, err := journal.RunStepContext(ctx, operation, "deduplicate", func(actionContext context.Context) (any, error) {
		ctx := actionContext
		previous, err := s.findDuplicate(ctx, namespace, sha256Value, configuration, onlyName)
		if err != nil {
			return nil, err
		}
		if previous == nil {
			return nil, nil
		}
		return previous, nil
	}, nil)
	if err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation, err)
	}
	if previousMap, ok := previousValue.(map[string]any); ok && previousMap != nil {
		result := make(map[string]any, len(previousMap)+4)
		for key, value := range previousMap {
			result[key] = value
		}
		result["skipped"] = true
		result["requested_name"] = name
		result["operation_id"] = operation.ID
		result["request_sha256"] = sha256Value
		if err := journal.CommitNoop(ctx, operation, result); err != nil {
			return s.failDocumentOperation(ctx, journal, coordinator, operation, err)
		}
		if _, cleanupErr := coordinator.CleanupWorkspace(ctx, operation); cleanupErr != nil {
			logUploadWarning(ctx, "Committed workspace cleanup pending", operation.ID, cleanupErr)
		}
		return result, nil
	}

	if err := s.documentJobProgress(ctx, options.Task, "chunking"); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("chunking", err))
	}
	indexed, err := s.ingestDocument(ctx, journal, operation, workspace, temporary, name, resources)
	if err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("parsing", err))
	}
	if err := s.documentJobProgress(ctx, options.Task, "staging"); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("staging", err))
	}
	if err := s.stageArtifacts(ctx, journal, operation, indexed); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("staging", err))
	}
	if err := s.documentJobProgress(ctx, options.Task, "embedding"); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("embedding", err))
	}
	// Reserve the whole document's known character count before the first paid
	// embedding call. The reservation is bound to this operation (and therefore
	// to automatic job retries) and to the quota period active right now.
	var embeddingChars int64
	for _, chunk := range indexed.result.Chunks {
		embeddingChars += int64(utf8.RuneCountInString(chunk.Text))
	}
	meterVersion := operation.ID
	if options.Task != nil {
		// Automatic retries of one admitted artifact reuse this identity, so the
		// customer is charged at most once per logical document call.
		meterVersion = options.Task.ArtifactID
	}
	meter, meterErr := s.prepareDocumentEmbeddingMeter(
		ctx, namespace, name, meterVersion, resources.embeddingModel, embeddingChars, "document", "")
	if meterErr != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("embedding", meterErr))
	}
	if meter != nil {
		resources.embedder.OnBatchSucceeded = meter.observe
	}
	vectors, sparseVectors, err := s.encodeDocumentVectors(ctx, journal, operation, indexed, resources)
	if meter != nil {
		resources.embedder.OnBatchSucceeded = nil
	}
	if err != nil {
		if meter != nil {
			_ = meter.finishUnknown(ctx)
		}
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("embedding", err))
	}
	if meter != nil {
		if err := meter.settle(ctx); err != nil {
			return s.failDocumentOperation(ctx, journal, coordinator, operation,
				logging.WrapStage("embedding", err))
		}
	}
	assetPayloads := make([]map[string]any, 0, len(indexed.result.Assets))
	for _, asset := range indexed.result.Assets {
		payload := asset.Payload(indexed.result.Chunks[0].Source)
		payload["document_id"] = indexed.result.Chunks[0].DocumentID
		assetPayloads = append(assetPayloads, payload)
	}
	if err := s.documentJobProgress(ctx, options.Task, "indexing"); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("indexing", err))
	}
	stageValue, err := journal.RunStepContext(ctx, operation, "stage_qdrant", func(actionContext context.Context) (any, error) {
		count, stageErr := resources.store.StageVersion(actionContext, indexed.qdrantChunks, vectors, sparseVectors,
			assetPayloads, operation.ID)
		return count, stageErr
	}, map[string]any{"version": operation.ID})
	if err != nil {
		if current, currentErr := journal.Get(ctx, operation.ID); currentErr == nil && current.Status != state.UploadCommitted {
			if deleteErr := resources.store.DeleteVersion(ctx, documentID, operation.ID); deleteErr != nil {
				logUploadWarning(ctx, "Version cleanup requires recovery", operation.ID, deleteErr)
			}
		}
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("indexing", err))
	}
	chunkCount := len(indexed.result.Chunks)
	if staged, ok := stageValue.(int); ok {
		chunkCount = staged
	}
	indexed.document.ChunkCount = chunkCount
	record := state.DocumentRecord{
		Name:               name,
		Size:               size,
		DocumentID:         indexed.document.DocumentID,
		ChunkCount:         chunkCount,
		Collection:         resources.collection,
		Diagnostics:        indexed.result.Diagnostics,
		SkippedObjectCount: indexed.result.SkippedObjectCount,
		SHA256:             sha256Value,
		Chunking:           configuration,
	}
	metadata := documentMetadata(name, size, sha256Value, indexed.document, configuration, operation.ID)
	if options.Task != nil {
		// Python stores the admitted configuration version with the publication so
		// the worker's reconcile can prove the same request was indexed.
		metadata["task_configuration_version"] = options.Task.ConfigurationVersion
		metadata["task_artifact_id"] = options.Task.ArtifactID
	}
	if err := s.documentJobProgress(ctx, options.Task, "publishing"); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("publishing", err))
	}
	if err := journal.Intent(ctx, operation, "publish", map[string]any{"version": operation.ID}); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("publishing", err))
	}
	if err := s.publishDocument(ctx, operation, record, metadata, options.Content,
		indexed.artifactBucket, indexed.result.StagingDir, options.Task); err != nil {
		return s.failDocumentOperation(ctx, journal, coordinator, operation,
			logging.WrapStage("publishing", err))
	}
	// The publication is durable, so the knowledge-graph mirror is scheduled
	// afterwards and never affects the upload outcome: a sidecar outage leaves
	// a failed ledger row, not a failed document.
	s.scheduleGraphMirror(ctx, namespace, name, documentID, resources.graph, indexed.result.Chunks)
	if _, cleanupErr := coordinator.CleanupWorkspace(ctx, operation); cleanupErr != nil {
		logUploadWarning(ctx, "Committed workspace cleanup pending", operation.ID, cleanupErr)
	}
	// The publication is durable: run the non-fatal SAG extraction stage. Its
	// failures are logged inside and never change the publication outcome.
	s.extractSagIndex(ctx, namespace, indexed.document.DocumentID, operation.ID, indexed.qdrantChunks)
	return metadata, nil
}

// indexedUpload is the prepared ingest output plus the derived index fields.
type indexedUpload struct {
	result         ingest.Result
	document       IndexedDocument
	qdrantChunks   []qdrant.Chunk
	artifactBucket string
}

// IndexedDocument mirrors service_knowledge.IndexedDocument.
type IndexedDocument struct {
	DocumentID         string
	ChunkCount         int
	Collection         string
	Diagnostics        []document.Diagnostic
	SkippedObjectCount int
	StagingDir         string
	ArtifactBucket     string
}

// ingestDocument runs the chunking/parsing step and converts chunks for Qdrant.
func (s *Service) ingestDocument(
	ctx context.Context,
	journal *state.UploadJournal,
	operation *state.UploadOperation,
	workspace, sourcePath, name string,
	resources *uploadResources,
) (*indexedUpload, error) {
	value, err := journal.RunStepContext(ctx, operation, "chunking_tokenizer", func(actionContext context.Context) (any, error) {
		return ingest.Prepare(actionContext, sourcePath, ingest.Options{
			SourceName:     name,
			Chunking:       resources.chunking,
			TokenizerModel: resources.tokenizerModel,
			StagingDir:     filepath.Join(workspace, "parsed"),
			PDF:            s.pdfOptions(resources),
			Docling:        s.doclingOptions(),
			Word: ingest.WordOptions{
				Converter: &document.SofficeConverter{
					Executable: s.config.WordConverterPath,
					Timeout:    time.Duration(s.config.WordConvertTimeout) * time.Second,
				},
				Vision: s.wordVisionWorker(resources.vlm),
			},
		})
	}, nil)
	if err != nil {
		return nil, logging.WrapStage("parsing", err)
	}
	result, ok := value.(ingest.Result)
	if !ok {
		return nil, logging.WrapStage("chunking",
			fmt.Errorf("document ingestion returned an unexpected result"))
	}
	if len(result.Chunks) == 0 {
		return nil, logging.WrapStage("chunking",
			&apierrors.KnowledgeValidationError{Message: "知识文件没有可索引的文本：" + name})
	}
	qdrantChunks := make([]qdrant.Chunk, len(result.Chunks))
	for index, chunk := range result.Chunks {
		qdrantChunks[index] = qdrant.Chunk{
			DocumentID: chunk.DocumentID,
			ChunkID:    chunk.ChunkID,
			Text:       chunk.Text,
			Title:      chunk.Title,
			Source:     chunk.Source,
			Metadata:   chunk.Metadata,
		}
	}
	artifactBucket := ""
	suffix := strings.ToLower(filepath.Ext(name))
	if result.StagingDir != "" {
		if bucket, ok := artifactBucketForSuffix(suffix); ok {
			artifactBucket = bucket
		}
	}
	indexed := IndexedDocument{
		DocumentID:         result.Chunks[0].DocumentID,
		ChunkCount:         len(result.Chunks),
		Collection:         resources.collection,
		Diagnostics:        result.Diagnostics,
		SkippedObjectCount: result.SkippedObjectCount,
		StagingDir:         result.StagingDir,
		ArtifactBucket:     artifactBucket,
	}
	return &indexedUpload{
		result:         result,
		document:       indexed,
		qdrantChunks:   qdrantChunks,
		artifactBucket: artifactBucket,
	}, nil
}

// pdfOptions selects the PDF parser for the ingest step. The in-process pure-Go
// engine stays the default (PDF.Parser nil); RAG_PDF_ENGINE=vision delegates to
// the remote vision parse service, whose base URL and timeout config already
// resolved. The knowledge base's VLM configuration is forwarded with every parse
// request so one service can serve deployments with different models.
func (s *Service) pdfOptions(resources *uploadResources) ingest.PDFOptions {
	if !config.IsRemotePDFEngine(s.config.PDFEngine) {
		return ingest.PDFOptions{}
	}
	parser := &document.PDFServiceParser{
		BaseURL: s.config.PDFServiceURL,
		Client: &http.Client{
			Timeout: time.Duration(s.config.PDFServiceTimeout) * time.Second,
		},
	}
	if resources != nil {
		parser.VLM = resources.vlm
	}
	return ingest.PDFOptions{Parser: parser}
}

// doclingOptions selects the Docling HTTP service for DOC/DOCX and static
// HTML. An empty RAG_DOCLING_SERVICE_URL keeps ingest.Docling.Parser nil, which
// leaves DOCX on the in-process OOXML parser and makes HTML import fail with
// ingest.ErrDoclingNotConfigured (already rejected at admission).
func (s *Service) doclingOptions() ingest.DoclingOptions {
	if !s.doclingEnabled() {
		return ingest.DoclingOptions{}
	}
	return ingest.DoclingOptions{
		Parser: &document.DoclingServiceParser{
			BaseURL: strings.TrimRight(strings.TrimSpace(s.config.DoclingServiceURL), "/"),
			Client: &http.Client{
				Timeout: time.Duration(s.config.DoclingServiceTimeout) * time.Second,
			},
		},
	}
}

// stageArtifacts persists the chunk manifest and parser artifacts in the
// journal object store (Python's stage_mysql step).
func (s *Service) stageArtifacts(
	ctx context.Context,
	journal *state.UploadJournal,
	operation *state.UploadOperation,
	indexed *indexedUpload,
) error {
	_, err := journal.RunStepContext(ctx, operation, "stage_mysql", func(actionContext context.Context) (any, error) {
		ctx := actionContext
		manifest := make([]map[string]any, len(indexed.result.Chunks))
		for index, chunk := range indexed.result.Chunks {
			manifest[index] = map[string]any{
				"chunk_id": chunk.ChunkID, "text": chunk.Text, "metadata": chunk.Metadata,
			}
		}
		encoded, err := json.Marshal(manifest)
		if err != nil {
			return nil, err
		}
		if err := journal.PutObject(ctx, operation, "index", "chunks.json", encoded, nil); err != nil {
			return nil, err
		}
		if indexed.result.StagingDir == "" || indexed.artifactBucket == "" {
			return nil, nil
		}
		files := make([]string, 0, 8)
		if walkErr := filepath.Walk(indexed.result.StagingDir, func(path string, info os.FileInfo, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if !info.IsDir() {
				files = append(files, path)
			}
			return nil
		}); walkErr != nil {
			return nil, walkErr
		}
		sort.Strings(files)
		for _, file := range files {
			relative, relErr := filepath.Rel(indexed.result.StagingDir, file)
			if relErr != nil {
				return nil, relErr
			}
			content, readErr := os.ReadFile(file)
			if readErr != nil {
				return nil, readErr
			}
			key := indexed.document.DocumentID + "/" + filepath.ToSlash(relative)
			if err := journal.PutObject(ctx, operation, indexed.artifactBucket, key, content, nil); err != nil {
				return nil, err
			}
		}
		return nil, nil
	}, nil)
	return logging.WrapStage("staging", err)
}

// encodeDocumentVectors overlaps embedding requests with local BM25 encoding.
// Both journal steps finish before the caller can publish or compensate.
func (s *Service) encodeDocumentVectors(
	ctx context.Context,
	journal *state.UploadJournal,
	operation *state.UploadOperation,
	indexed *indexedUpload,
	resources *uploadResources,
) ([][]float64, []sparse.SparseEmbedding, error) {
	workCtx, cancel := context.WithCancelCause(ctx)
	defer cancel(nil)
	texts := make([]string, len(indexed.result.Chunks))
	for index, chunk := range indexed.result.Chunks {
		texts[index] = chunk.Text
	}
	runStep := func(name string, action func() error) {
		// Keep journal writes on the caller's context so cancelling the sibling's
		// computation does not prevent recording the original failed step.
		_, err := journal.RunStep(ctx, operation, name, func() (any, error) {
			err := action()
			if err != nil {
				// Capture the cause before journal fencing can fail in the sibling.
				cancel(err)
			}
			return nil, err
		}, nil)
		if err != nil {
			cancel(err)
		}
	}
	var vectors [][]float64
	denseDone := make(chan struct{})
	go func() {
		defer close(denseDone)
		runStep("embedding", func() error {
			var err error
			vectors, err = resources.embedder.EmbedDocuments(workCtx, texts)
			return err
		})
	}()
	sparseVectors := make([]sparse.SparseEmbedding, 0, len(texts))
	runStep("sparse", func() error {
		for _, text := range texts {
			if err := workCtx.Err(); err != nil {
				return err
			}
			vector, err := resources.encoder.EncodeDocument(text)
			if err != nil {
				return err
			}
			sparseVectors = append(sparseVectors, vector)
		}
		return nil
	})
	<-denseDone
	if err := context.Cause(workCtx); err != nil {
		return nil, nil, logging.WrapStage("embedding", err)
	}
	return vectors, sparseVectors, nil
}

// failDocumentOperation records the failure, compensates staged vectors and
// returns the error annotated with its operation id. An ambiguous commit is
// treated as success, never rolled back.
func (s *Service) failDocumentOperation(
	ctx context.Context,
	journal *state.UploadJournal,
	coordinator *state.UploadCoordinator,
	operation *state.UploadOperation,
	cause error,
) (map[string]any, error) {
	if current, err := journal.Get(ctx, operation.ID); err == nil && current != nil && current.Status == state.UploadCommitted {
		// A committed journal row is the authority: the caller observes success
		// even when the acknowledgement was lost.
		if _, cleanupErr := coordinator.CleanupWorkspace(ctx, current); cleanupErr != nil {
			logUploadWarning(ctx, "Committed workspace cleanup pending", operation.ID, cleanupErr)
		}
		result := map[string]any{}
		if stored, ok := current.Result.(map[string]any); ok {
			for key, value := range stored {
				result[key] = value
			}
		}
		return result, nil
	}
	s.compensateUpload(ctx, coordinator, operation, cause)
	logging.FromContext(ctx).Warn("document upload failed",
		append([]any{"operation_id", operation.ID}, logging.ErrorAttrs(cause)...)...)
	return nil, &documentOperationError{Err: cause, OperationID: operation.ID}
}

// replayCommittedUpload validates the bytes of an idempotent replay without
// repeating any external side effect.
func replayCommittedUpload(operation *state.UploadOperation, content []byte, name string) (map[string]any, error) {
	result := make(map[string]any)
	if existing, ok := operation.Result.(map[string]any); ok {
		for key, value := range existing {
			result[key] = value
		}
	}
	if len(content) > maxFileBytes {
		return nil, &apierrors.KnowledgeValidationError{Message: "Document exceeds the file size limit"}
	}
	digestValue, _ := result["request_sha256"].(string)
	if digestValue == "" {
		digestValue, _ = result["sha256"].(string)
	}
	if sha256HexBytes(content) != digestValue {
		return nil, &state.UploadConflict{Message: "Idempotency key has different file bytes"}
	}
	_ = name
	result["operation_id"] = operation.ID
	result["replayed"] = true
	return result, nil
}

// findDuplicate ports KnowledgeDocumentStore.find_duplicate.
func (s *Service) findDuplicate(
	ctx context.Context,
	namespace, sha256Value string,
	configuration map[string]any,
	onlyName *string,
) (map[string]any, error) {
	records, err := s.stores.Documents.Candidates(ctx, namespace, sha256Value, onlyName)
	if err != nil {
		return nil, err
	}
	for index := range records {
		candidate := &records[index]
		name := candidate.Name
		if onlyName != nil && name != *onlyName {
			continue
		}
		exists, err := s.stores.Blobs.Exists(ctx, namespace, state.SourceBucket, name)
		if err != nil {
			return nil, err
		}
		if !exists {
			continue
		}
		artifactBucket := ""
		if _, ok := configuration["pdf_parser_version"]; ok {
			artifactBucket = state.PDFAssetBucket
		}
		if _, ok := configuration["word_parser_version"]; ok {
			artifactBucket = state.WordAssetBucket
		}
		if _, ok := configuration["html_parser_version"]; ok {
			artifactBucket = state.HTMLAssetBucket
		}
		if artifactBucket != "" {
			hasDocument, err := s.stores.Blobs.Exists(ctx, namespace, artifactBucket, candidate.DocumentID+"/document.md")
			if err != nil {
				return nil, err
			}
			hasManifest, err := s.stores.Blobs.Exists(ctx, namespace, artifactBucket, candidate.DocumentID+"/manifest.json")
			if err != nil {
				return nil, err
			}
			if !hasDocument || !hasManifest {
				continue
			}
		}
		if candidate.SHA256 == "" {
			content, found, err := s.stores.Blobs.Get(ctx, namespace, state.SourceBucket, name)
			if err != nil {
				return nil, err
			}
			if !found {
				continue
			}
			candidate.SHA256 = sha256HexBytes(content)
			if candidate.Chunking == nil {
				candidate.Chunking = legacyChunking.Metadata(kbconfig.DefaultTokenizerModel)
			}
			if err := s.stores.Documents.Upsert(ctx, namespace, *candidate); err != nil {
				return nil, err
			}
		}
		if candidate.Chunking == nil {
			candidate.Chunking = legacyChunking.Metadata(kbconfig.DefaultTokenizerModel)
		}
		record := recordMap(*candidate)
		if candidate.SHA256 == sha256Value && sameChunking(record["chunking"], configuration) {
			return record, nil
		}
	}
	return nil, nil
}

// documentMetadata builds the metadata map persisted at publication.
func documentMetadata(
	name string,
	size int64,
	sha256Value string,
	indexed IndexedDocument,
	configuration map[string]any,
	operationID string,
) map[string]any {
	timestamp := kbconfig.ISOTimestamp(time.Now())
	return map[string]any{
		"name":                 name,
		"size":                 size,
		"modified_at":          timestamp,
		"indexed_at":           timestamp,
		"document_id":          indexed.DocumentID,
		"chunk_count":          indexed.ChunkCount,
		"collection":           indexed.Collection,
		"diagnostics":          indexed.Diagnostics,
		"skipped_object_count": indexed.SkippedObjectCount,
		"sha256":               sha256Value,
		"chunking":             configuration,
		"operation_id":         operationID,
	}
}

// writeBoundedDocument writes the source bytes and returns the SHA-256/size,
// enforcing MAX_FILE_BYTES like _write_bounded.
func writeBoundedDocument(target string, content []byte, name string) (string, int64, error) {
	if int64(len(content)) > maxFileBytes {
		return "", 0, &apierrors.KnowledgeValidationError{
			Message: fmt.Sprintf("单个知识文件不能超过 %d MiB：%s", maxFileBytes/(1024*1024), name),
		}
	}
	if err := os.WriteFile(target, content, 0o644); err != nil {
		return "", 0, err
	}
	return sha256HexBytes(content), int64(len(content)), nil
}

// uploadFingerprint mirrors hashlib.sha256(json.dumps({...}, sort_keys=True)).
// The canonical JSON uses Python's default ", "/": " separators so a Python
// instance would compute the same fingerprint for the same request.
func uploadFingerprint(name string, configuration map[string]any, overwrite *bool) string {
	payload := map[string]any{
		"name":          name,
		"configuration": configuration,
		"overwrite":     overwrite,
	}
	encoded, err := pythonJSONValue(payload)
	if err != nil {
		return sha256Hex(fmt.Sprintf("%v", payload))
	}
	return sha256Hex(encoded)
}

// pythonJSONValue renders a JSON value with sorted object keys, ", "/": "
// separators and ensure_ascii escaping, i.e. json.dumps(value, sort_keys=True).
func pythonJSONValue(value any) (string, error) {
	var builder strings.Builder
	if err := writePythonJSON(&builder, value); err != nil {
		return "", err
	}
	return builder.String(), nil
}

func writePythonJSON(builder *strings.Builder, value any) error {
	switch typed := value.(type) {
	case nil:
		builder.WriteString("null")
	case string:
		builder.WriteString(pythonJSONString(typed))
	case bool:
		if typed {
			builder.WriteString("true")
		} else {
			builder.WriteString("false")
		}
	case int:
		builder.WriteString(fmt.Sprintf("%d", typed))
	case int64:
		builder.WriteString(fmt.Sprintf("%d", typed))
	case float64:
		if typed == float64(int64(typed)) {
			builder.WriteString(fmt.Sprintf("%d.0", int64(typed)))
		} else {
			builder.WriteString(strconvFormatFloat(typed))
		}
	case map[string]any:
		keys := make([]string, 0, len(typed))
		for key := range typed {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		builder.WriteByte('{')
		for index, key := range keys {
			if index > 0 {
				builder.WriteString(", ")
			}
			builder.WriteString(pythonJSONString(key))
			builder.WriteString(": ")
			if err := writePythonJSON(builder, typed[key]); err != nil {
				return err
			}
		}
		builder.WriteByte('}')
	case []any:
		builder.WriteByte('[')
		for index, item := range typed {
			if index > 0 {
				builder.WriteString(", ")
			}
			if err := writePythonJSON(builder, item); err != nil {
				return err
			}
		}
		builder.WriteByte(']')
	default:
		encoded, err := json.Marshal(typed)
		if err != nil {
			return err
		}
		builder.Write(encoded)
	}
	return nil
}

// pythonJSONString escapes one string with json.dumps(ensure_ascii=True) rules.
func pythonJSONString(value string) string {
	var builder strings.Builder
	builder.WriteByte('"')
	for _, char := range value {
		switch char {
		case '"':
			builder.WriteString(`\"`)
		case '\\':
			builder.WriteString(`\\`)
		case '\b':
			builder.WriteString(`\b`)
		case '\f':
			builder.WriteString(`\f`)
		case '\n':
			builder.WriteString(`\n`)
		case '\r':
			builder.WriteString(`\r`)
		case '\t':
			builder.WriteString(`\t`)
		default:
			if char < 0x20 {
				builder.WriteString(fmt.Sprintf(`\u%04x`, char))
				continue
			}
			if char <= 0x7E {
				builder.WriteRune(char)
				continue
			}
			if char <= 0xFFFF {
				builder.WriteString(fmt.Sprintf(`\u%04x`, char))
				continue
			}
			scalar := char - 0x10000
			builder.WriteString(fmt.Sprintf(`\u%04x\u%04x`,
				0xD800+(scalar>>10), 0xDC00+(scalar&0x3FF)))
		}
	}
	builder.WriteByte('"')
	return builder.String()
}

// safeUploadErrorText mirrors service_knowledge._safe_error: known safe errors
// keep their message (truncated); everything else is reported by category only.
func safeUploadErrorText(err error) string {
	if err == nil {
		return ""
	}
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		return truncateText(validation.Message, 500)
	}
	var lockUnavailable *state.LockUnavailable
	if errors.As(err, &lockUnavailable) {
		return documentLockUnavailableMessage
	}
	var knowledge *apierrors.KnowledgeError
	if errors.As(err, &knowledge) {
		return truncateText(knowledge.Message, 500)
	}
	var modelError *modelapi.ModelAPIError
	if errors.As(err, &modelError) {
		return truncateText(modelError.Message, 500)
	}
	var pdfParse *document.PDFParseError
	if errors.As(err, &pdfParse) {
		return truncateText(pdfParse.Message, 500)
	}
	var operationError *documentOperationError
	if errors.As(err, &operationError) {
		return "知识库操作失败：" + shortTypeName(operationError.Err)
	}
	return "知识库操作失败：" + shortTypeName(err)
}

func truncateText(value string, maximum int) string {
	runes := []rune(value)
	if len(runes) <= maximum {
		return value
	}
	return string(runes[:maximum])
}

// shortTypeName renders the Go type name without its package or pointer,
// matching Python's type(exc).__name__ shape in safe error text.
func shortTypeName(err error) string {
	if err == nil {
		return "nil"
	}
	name := fmt.Sprintf("%T", err)
	name = strings.TrimPrefix(name, "*")
	if index := strings.LastIndex(name, "."); index >= 0 {
		name = name[index+1:]
	}
	// Go has no Python exception classes; errors.New and fmt.Errorf without
	// %w are the port's RuntimeError. Without this mapping callers would see
	// the exported Go identifier ("errorString") instead of an error category.
	switch name {
	case "errorString", "wrapError":
		return "RuntimeError"
	}
	return name
}

// logUploadWarning reports a best-effort upload cleanup/compensation problem.
// The cause is classified and redacted: a raw message may embed credentials,
// URLs or document text and must never reach the log sink verbatim.
func logUploadWarning(ctx context.Context, message, operationID string, err error) {
	logging.FromContext(ctx).Warn(message,
		append([]any{"operation_id", operationID}, logging.ErrorAttrs(err)...)...)
}

func strconvFormatFloat(value float64) string {
	return strconv.FormatFloat(value, 'g', -1, 64)
}

func sha256Hex(value string) string { return sha256HexBytes([]byte(value)) }

func sha256HexBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// vlmSettings maps the service configuration to the remote VLM endpoint used by
// formula recognition and picture descriptions.
func (s *Service) vlmSettings() document.VLMSettings {
	return document.VLMSettings{
		BaseURL:     s.config.VLMBaseURL,
		APIKey:      s.config.VLMAPIKey,
		Model:       s.config.VLMModel,
		Timeout:     time.Duration(s.config.VLMTimeout) * time.Second,
		Concurrency: s.config.VLMConcurrency,
	}
}

// wordVisionWorker builds the Word image describer for one knowledge base. It
// returns nil when no remote VLM is configured so uploads keep working without
// one.
func (s *Service) wordVisionWorker(vlm document.VLMSettings) *document.WordVisionWorker {
	if !vlm.Configured() {
		return nil
	}
	return &document.WordVisionWorker{VLM: vlm}
}

// uploadVLM prefers the knowledge base's own VLM provider and falls back to the
// service environment (RAG_VLM_*) when the knowledge base has none, so the
// Console settings take precedence while existing deployments keep working.
func (s *Service) uploadVLM(settings map[string]any) (document.VLMSettings, error) {
	connection, err := kbconfig.ModelConnectionForKey(settings, "vlm", s.stores.Settings.EncryptionKey)
	if err != nil {
		return document.VLMSettings{}, adminError(err)
	}
	fallback := s.vlmSettings()
	if !connection.Configured() || connection.Model == "" {
		return fallback, nil
	}
	return document.VLMSettings{
		BaseURL:     connection.BaseURL,
		APIKey:      connection.APIKey,
		Model:       connection.Model,
		Timeout:     fallback.Timeout,
		Concurrency: fallback.Concurrency,
	}, nil
}
