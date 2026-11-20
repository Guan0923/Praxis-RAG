package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Guan0923/Praxis-RAG/internal/apierrors"
	"github.com/Guan0923/Praxis-RAG/internal/feishu"
)

// feishuImportReadConcurrency bounds the number of simultaneous document reads
// for one import request.
const feishuImportReadConcurrency = 4

// feishuImportReadTimeout is a context backstop for one document read. The
// configured Feishu client timeout normally fires first; either way a slow
// document is reported as its own failure instead of blocking the selection.
const feishuImportReadTimeout = 30 * time.Second

// FeishuService is the Console-facing Feishu integration. The concrete
// implementation is *feishu.Service.
type FeishuService interface {
	Enabled() bool
	Status(ctx context.Context, userID string) (feishu.Status, error)
	AuthorizeURL(ctx context.Context, userID string) (string, error)
	CompleteOAuth(ctx context.Context, userID, code, state string) error
	Disconnect(ctx context.Context, userID string) (bool, error)
	ListFiles(ctx context.Context, userID, folderToken, pageToken string, pageSize int) (feishu.FilePageView, error)
	Search(ctx context.Context, userID, query string, offset, count int) (feishu.SearchPageView, error)
	ReadDocument(ctx context.Context, userID, documentID string) (feishu.DocumentContent, error)
}

// WithFeishuService wires the Feishu document import integration.
func WithFeishuService(service FeishuService) Option {
	return func(s *Server) { s.feishu = service }
}

// registerFeishuRoutes mounts the per-account Feishu connection, browsing,
// search and import endpoints.
//
// status/authorize/files/search are session-only because the grant belongs to
// one Console account. The import endpoint also needs a tenant namespace and
// the knowledge-base write scope, exactly like the ordinary upload path. The
// callback is reached by a browser redirect from Feishu and authenticates the
// session cookie itself.
func (s *Server) registerFeishuRoutes() {
	s.router.add("GET", "/v1/feishu/status", authSession, s.handleFeishuStatus)
	s.router.add("POST", "/v1/feishu/oauth/start", authSession, s.handleFeishuOAuthStart)
	s.router.add("GET", "/v1/feishu/oauth/callback", authNone, s.handleFeishuOAuthCallback)
	s.router.add("DELETE", "/v1/feishu/connection", authSession, s.handleFeishuDisconnect)
	s.router.add("GET", "/v1/feishu/files", authSession, s.handleFeishuFiles)
	s.router.add("GET", "/v1/feishu/search", authSession, s.handleFeishuSearch)
	s.router.add("POST", "/v1/feishu/import", authTenant, s.handleFeishuImport)
}

// handleFeishuStatus reports whether the deployment configured Feishu and
// whether the signed-in account connected it.
func (s *Server) handleFeishuStatus(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if s.feishu == nil {
		writeJSON(w, http.StatusOK, feishu.Status{
			Enabled: false, Scopes: []string{}, MissingScopes: []string{},
			RequiredScopes: append([]string(nil), feishu.RequiredScopes...),
		})
		return
	}
	status, err := s.feishu.Status(r.Context(), session.User.ID)
	if err != nil {
		writeFeishuError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, status)
}

// handleFeishuOAuthStart creates one OAuth state and returns the browser URL.
func (s *Server) handleFeishuOAuthStart(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireFeishu(w) {
		return
	}
	authorizeURL, err := s.feishu.AuthorizeURL(r.Context(), session.User.ID)
	if err != nil {
		writeFeishuError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]string{"authorize_url": authorizeURL})
}

// handleFeishuOAuthCallback completes one browser authorization. It redirects
// back into the Console instead of rendering JSON, so the browser lands on the
// same workspace it started from.
func (s *Server) handleFeishuOAuthCallback(w http.ResponseWriter, r *http.Request) {
	redirect := func(reason string) {
		target := "/console?"
		switch reason {
		case "connected":
			target += "feishu=connected"
		case "denied":
			target += "feishu=denied"
		case "login_required":
			target += "feishu=login_required"
		default:
			target += "feishu=error&feishu_reason=" + reason
		}
		target += "#/workspaces/documents"
		w.Header().Set("Cache-Control", "no-store")
		http.Redirect(w, r, target, http.StatusFound)
	}
	session, ok := s.requestSessionUser(r)
	if !ok || session == nil {
		redirect("login_required")
		return
	}
	if !s.requireFeishuBool() {
		redirect("disabled")
		return
	}
	if r.URL.Query().Get("error") != "" {
		redirect("denied")
		return
	}
	code := strings.TrimSpace(r.URL.Query().Get("code"))
	state := strings.TrimSpace(r.URL.Query().Get("state"))
	if code == "" || state == "" {
		redirect("state")
		return
	}
	if err := s.feishu.CompleteOAuth(r.Context(), session.User.ID, code, state); err != nil {
		redirect(feishuRedirectReason(err))
		return
	}
	redirect("connected")
}

// handleFeishuFiles lists one folder level, or the user's root folder.
func (s *Server) handleFeishuFiles(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireFeishu(w) {
		return
	}
	query := r.URL.Query()
	pageSize := 50
	if raw := strings.TrimSpace(query.Get("page_size")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 200 {
			writeDetail(w, http.StatusUnprocessableEntity, "page_size 必须是 1 到 200 之间的整数")
			return
		}
		pageSize = parsed
	}
	page, err := s.feishu.ListFiles(r.Context(), session.User.ID,
		strings.TrimSpace(query.Get("folder_token")),
		strings.TrimSpace(query.Get("page_token")), pageSize)
	if err != nil {
		writeFeishuError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

// handleFeishuSearch searches the documents visible to the account.
func (s *Server) handleFeishuSearch(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireFeishu(w) {
		return
	}
	query := r.URL.Query()
	offset := 0
	if raw := strings.TrimSpace(query.Get("offset")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed >= 200 {
			writeDetail(w, http.StatusUnprocessableEntity, "offset 必须是 0 到 199 之间的整数")
			return
		}
		offset = parsed
	}
	count := 20
	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 50 {
			writeDetail(w, http.StatusUnprocessableEntity, "limit 必须是 1 到 50 之间的整数")
			return
		}
		count = parsed
	}
	page, err := s.feishu.Search(r.Context(), session.User.ID,
		query.Get("q"), offset, count)
	if err != nil {
		writeFeishuError(w, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

// handleFeishuDisconnect removes the account's stored Feishu grant.
func (s *Server) handleFeishuDisconnect(w http.ResponseWriter, r *http.Request) {
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireFeishu(w) {
		return
	}
	if _, err := s.feishu.Disconnect(r.Context(), session.User.ID); err != nil {
		writeFeishuError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// feishuImportBody is one multi-document import request.
type feishuImportBody struct {
	Documents []feishuImportDocument `json:"documents"`
}

// feishuImportDocument is one selected Feishu document.
type feishuImportDocument struct {
	DocumentID string `json:"document_id"`
	Title      string `json:"title"`
}

// feishuImportItem is one per-document outcome. Status is accepted / skipped /
// failed, mirroring the ordinary upload batch vocabulary.
type feishuImportItem struct {
	DocumentID string          `json:"document_id"`
	Title      string          `json:"title"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Reason     string          `json:"reason,omitempty"`
	Error      string          `json:"error,omitempty"`
	ArtifactID *string         `json:"artifact_id,omitempty"`
	Job        *feishuBatchJob `json:"job,omitempty"`
}

// feishuImportResponse is one import result set.
type feishuImportResponse struct {
	BatchID   string             `json:"batch_id,omitempty"`
	Namespace string             `json:"namespace"`
	Total     int                `json:"total"`
	Accepted  int                `json:"accepted"`
	Skipped   int                `json:"skipped"`
	Failed    int                `json:"failed"`
	Items     []feishuImportItem `json:"items"`
}

// feishuBatchPayload mirrors the runtime's batch admission projection.
type feishuBatchPayload struct {
	BatchID  string            `json:"batch_id"`
	Status   string            `json:"status"`
	Total    int               `json:"total"`
	Accepted int               `json:"accepted"`
	Skipped  int               `json:"skipped"`
	Failed   int               `json:"failed"`
	Items    []feishuBatchItem `json:"items"`
}

// feishuBatchItem mirrors one runtime batch item.
type feishuBatchItem struct {
	Position   int             `json:"position"`
	Name       string          `json:"name"`
	Status     string          `json:"status"`
	Reason     *string         `json:"reason"`
	ArtifactID *string         `json:"artifact_id"`
	Error      *string         `json:"error"`
	Job        *feishuBatchJob `json:"job"`
}

// feishuBatchJob mirrors one document job's status/stage.
type feishuBatchJob struct {
	Status string  `json:"status"`
	Stage  string  `json:"stage"`
	Error  *string `json:"error"`
}

// handleFeishuImport reads every selected document and admits the readable ones
// as one ordinary document batch. A failed read is isolated to its own item so
// the remaining documents continue through the existing permission/dedupe/
// asynchronous-indexing path.
func (s *Server) handleFeishuImport(w http.ResponseWriter, r *http.Request) {
	namespace, ok := s.tenantNamespace(w, r)
	if !ok {
		return
	}
	session, ok := requestSessionUserContext(w, r)
	if !ok {
		return
	}
	if !s.requireFeishu(w) {
		return
	}
	if s.runtime == nil {
		writeDetail(w, http.StatusServiceUnavailable, "RAG service runtime is not available")
		return
	}
	var body feishuImportBody
	if details := decodeJSONBody(r, &body); details != nil {
		writeValidation(w, http.StatusUnprocessableEntity, details)
		return
	}
	if len(body.Documents) == 0 {
		writeDetail(w, http.StatusUnprocessableEntity, "请至少选择一篇飞书文档。")
		return
	}
	if len(body.Documents) > feishu.MaxImportDocuments {
		writeDetail(w, http.StatusUnprocessableEntity, "单次最多导入 10 篇飞书文档，请分批选择。")
		return
	}
	documents := make([]feishuImportDocument, 0, len(body.Documents))
	for _, item := range body.Documents {
		documentID := strings.TrimSpace(item.DocumentID)
		title := strings.TrimSpace(item.Title)
		if documentID == "" {
			writeDetail(w, http.StatusUnprocessableEntity, "飞书文档标识不能为空。")
			return
		}
		if title == "" {
			title = "未命名文档"
		}
		if len([]rune(title)) > 200 {
			title = string([]rune(title)[:200])
		}
		documents = append(documents, feishuImportDocument{DocumentID: documentID, Title: title})
	}
	items := make([]feishuImportItem, len(documents))
	names := make([]string, len(documents))
	unique := make([]bool, len(documents))
	seen := map[string]struct{}{}
	for index, document := range documents {
		names[index] = feishu.DocumentFilename(document.Title)
		items[index] = feishuImportItem{
			DocumentID: document.DocumentID, Title: document.Title, Name: names[index],
		}
		if _, duplicate := seen[document.DocumentID]; duplicate {
			items[index].Status = "skipped"
			items[index].Reason = "duplicate"
			items[index].Error = "本次导入中重复选择，已跳过。"
			continue
		}
		seen[document.DocumentID] = struct{}{}
		unique[index] = true
	}
	// Apply the ordinary knowledge-base admission check before reading any
	// content, so a caller without the required operations learns about it
	// immediately and consistently: new names need the library create bit,
	// names that already exist need that document's update bit.
	candidates := make([]UploadFile, 0, len(documents))
	for index, isUnique := range unique {
		if isUnique {
			candidates = append(candidates, UploadFile{Name: names[index]})
		}
	}
	if blocked, _ := s.scopeUploadBatchBlocked(w, r, namespace, candidates); blocked {
		return
	}
	contents := make([]string, len(documents))
	readErrors := make([]error, len(documents))
	semaphore := make(chan struct{}, feishuImportReadConcurrency)
	var waitGroup sync.WaitGroup
	for index := range documents {
		if !unique[index] {
			continue
		}
		waitGroup.Add(1)
		go func(position int) {
			defer waitGroup.Done()
			semaphore <- struct{}{}
			defer func() { <-semaphore }()
			readContext, cancel := context.WithTimeout(r.Context(), feishuImportReadTimeout)
			defer cancel()
			content, err := s.feishu.ReadDocument(readContext, session.User.ID, documents[position].DocumentID)
			if err != nil {
				readErrors[position] = err
				return
			}
			contents[position] = content.Text
		}(index)
	}
	waitGroup.Wait()

	submitFiles := make([]UploadFile, 0, len(documents))
	submitPositions := make([]int, 0, len(documents))
	for index := range documents {
		if !unique[index] {
			continue
		}
		if readErrors[index] != nil {
			reason, text := feishuImportFailure(readErrors[index])
			items[index].Status = "failed"
			items[index].Reason = reason
			items[index].Error = text
			continue
		}
		content := contents[index]
		if strings.TrimSpace(content) == "" {
			items[index].Status = "failed"
			items[index].Reason = "empty"
			items[index].Error = "飞书文档没有可导入的正文。"
			continue
		}
		if len(content) > feishu.MaxDocumentBytes {
			items[index].Status = "failed"
			items[index].Reason = "too_large"
			items[index].Error = "飞书文档正文超过 10 MiB，请拆分后重试。"
			continue
		}
		submitFiles = append(submitFiles, UploadFile{Name: names[index], Content: []byte(content)})
		submitPositions = append(submitPositions, index)
	}

	batchID := ""
	if len(submitFiles) > 0 {
		payload, err := s.runtime.SubmitDocumentBatch(r.Context(), namespace, submitFiles, "", session.User.ID)
		if err != nil {
			status, detail := runtimeErrorStatus(err)
			reason := "internal"
			switch status {
			case http.StatusBadRequest, http.StatusUnprocessableEntity:
				reason = "validation"
			case http.StatusForbidden:
				reason = "permission"
			case http.StatusConflict:
				reason = "conflict"
			case http.StatusNotFound, http.StatusServiceUnavailable:
				reason = "config"
			}
			for _, position := range submitPositions {
				items[position].Status = "failed"
				items[position].Reason = reason
				items[position].Error = detail
			}
		} else {
			var batch feishuBatchPayload
			if encoded, marshalErr := json.Marshal(payload); marshalErr == nil {
				if decodeErr := json.Unmarshal(encoded, &batch); decodeErr == nil {
					batchID = batch.BatchID
					byPosition := make(map[int]feishuBatchItem, len(batch.Items))
					for _, batchItem := range batch.Items {
						byPosition[batchItem.Position] = batchItem
					}
					for offset, position := range submitPositions {
						batchItem, present := byPosition[offset]
						if !present {
							items[position].Status = "failed"
							items[position].Reason = "internal"
							items[position].Error = "未收到该文件的受理结果。"
							continue
						}
						items[position].Status = batchItem.Status
						if items[position].Status != "accepted" && items[position].Status != "skipped" &&
							items[position].Status != "failed" {
							items[position].Status = "failed"
							items[position].Reason = "internal"
							items[position].Error = "批次受理结果状态无效。"
							continue
						}
						if batchItem.Reason != nil {
							items[position].Reason = *batchItem.Reason
						}
						if batchItem.Error != nil {
							items[position].Error = *batchItem.Error
						}
						items[position].ArtifactID = batchItem.ArtifactID
						items[position].Job = batchItem.Job
					}
				}
			}
			if batchID == "" {
				for _, position := range submitPositions {
					items[position].Status = "failed"
					items[position].Reason = "internal"
					items[position].Error = "批次受理结果无法解析。"
				}
			}
		}
	}
	response := feishuImportResponse{
		BatchID: batchID, Namespace: namespace, Total: len(items),
		Items: items,
	}
	targets := make([]audit.Target, 0, len(items))
	for _, item := range items {
		targets = append(targets, audit.Target{Name: item.Name, Outcome: item.Status})
		switch item.Status {
		case "accepted":
			response.Accepted++
		case "skipped":
			response.Skipped++
		default:
			response.Failed++
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	audit.Record(r.Context(), s.audit, s.logger, "REST", "import_document", namespace, targets...)
	writeJSON(w, http.StatusAccepted, response)
}

// requireFeishu writes the stable 503 message when the integration is absent.
func (s *Server) requireFeishu(w http.ResponseWriter) bool {
	if s.feishu != nil && s.feishu.Enabled() {
		return true
	}
	writeDetail(w, http.StatusServiceUnavailable,
		"飞书导入未启用：部署方尚未配置 RAG_FEISHU_APP_ID、RAG_FEISHU_APP_SECRET 和 RAG_FEISHU_REDIRECT_URI。")
	return false
}

// requireFeishuBool is the redirect-friendly variant used by the callback.
func (s *Server) requireFeishuBool() bool {
	return s.feishu != nil && s.feishu.Enabled()
}

// feishuImportFailure maps one read failure to the per-item vocabulary.
func feishuImportFailure(err error) (string, string) {
	switch {
	case errors.Is(err, feishu.ErrEmptyDocument):
		return "empty", "飞书文档没有可导入的正文。"
	case errors.Is(err, feishu.ErrDocumentTooLarge):
		return "too_large", "飞书文档正文超过 10 MiB，请拆分后重试。"
	case errors.Is(err, feishu.ErrReauthRequired), feishu.IsAuthError(err):
		return "reauth", "飞书授权已失效，请重新连接飞书账号。"
	case errors.Is(err, feishu.ErrNotConnected):
		return "reauth", "尚未连接飞书账号。"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", "读取飞书文档超时，请单独重试该篇。"
	}
	var apiError *feishu.APIError
	if errors.As(err, &apiError) {
		switch apiError.Code {
		case 1770004, 1770008, 1770033:
			return "too_large", "飞书文档内容超过接口限制，请拆分后重试。"
		case 1770021:
			return "unsupported", "飞书文档版本过旧，暂不支持导入。"
		}
	}
	switch feishu.KindOf(err) {
	case feishu.KindPermission:
		return "permission", err.Error()
	case feishu.KindNotFound:
		return "not_found", "飞书文档不存在或已被删除。"
	case feishu.KindRateLimited:
		return "rate_limited", "飞书接口调用过于频繁，请稍后重试。"
	case feishu.KindConfig:
		return "config", err.Error()
	case feishu.KindValidation:
		return "validation", err.Error()
	case feishu.KindNetwork:
		return "network", err.Error()
	}
	return "read_failed", "读取飞书文档失败，请稍后重试。"
}

// writeFeishuError maps integration failures to stable HTTP responses. It does
// not use 401 for a Feishu grant problem: that status would also invalidate the
// Console session in the web client.
func writeFeishuError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, feishu.ErrNotConfigured):
		writeDetail(w, http.StatusServiceUnavailable,
			"飞书导入未启用：部署方尚未配置飞书应用。")
		return
	case errors.Is(err, feishu.ErrNotConnected):
		writeDetail(w, http.StatusConflict, "尚未连接飞书账号，请先完成飞书授权。")
		return
	case errors.Is(err, feishu.ErrReauthRequired):
		writeDetail(w, http.StatusConflict, "飞书授权已失效，请重新连接飞书账号。")
		return
	case errors.Is(err, feishu.ErrInvalidState):
		writeDetail(w, http.StatusConflict, "飞书授权校验失败，请重新发起连接。")
		return
	case errors.Is(err, feishu.ErrEmptyDocument):
		writeDetail(w, http.StatusUnprocessableEntity, "飞书文档没有可导入的正文。")
		return
	case errors.Is(err, feishu.ErrDocumentTooLarge):
		writeDetail(w, http.StatusRequestEntityTooLarge, "飞书文档正文超过 10 MiB。")
		return
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		writeDetail(w, http.StatusGatewayTimeout, "飞书接口调用超时，请稍后重试。")
		return
	}
	var apiError *feishu.APIError
	if errors.As(err, &apiError) {
		switch apiError.Kind {
		case feishu.KindConfig:
			writeDetail(w, http.StatusServiceUnavailable, apiError.Message)
		case feishu.KindValidation:
			writeDetail(w, http.StatusUnprocessableEntity, apiError.Message)
		case feishu.KindAuth, feishu.KindPermission:
			writeDetail(w, http.StatusConflict, apiError.Message)
		case feishu.KindNotFound:
			writeDetail(w, http.StatusNotFound, apiError.Message)
		case feishu.KindRateLimited:
			writeDetail(w, http.StatusTooManyRequests, apiError.Message)
		case feishu.KindNetwork:
			writeDetail(w, http.StatusBadGateway, apiError.Message)
		default:
			writeDetail(w, http.StatusBadGateway, apiError.Message)
		}
		return
	}
	var validation *apierrors.KnowledgeValidationError
	if errors.As(err, &validation) {
		writeDetail(w, http.StatusUnprocessableEntity, validation.Message)
		return
	}
	writeDetail(w, http.StatusInternalServerError, "飞书服务异常，请稍后重试。")
}

// feishuRedirectReason converts a callback failure into a stable short code.
func feishuRedirectReason(err error) string {
	switch {
	case errors.Is(err, feishu.ErrInvalidState):
		return "state"
	case errors.Is(err, feishu.ErrReauthRequired), feishu.IsAuthError(err):
		return "reauth"
	}
	switch feishu.KindOf(err) {
	case feishu.KindPermission:
		return "permission"
	case feishu.KindConfig:
		return "config"
	case feishu.KindNetwork:
		return "network"
	}
	return "error"
}
