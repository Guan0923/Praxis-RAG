package httpapi

import (
	"net/http"
	"strings"

	"github.com/Guan0923/xinghe_rag/internal/version"
)

// openAPIRoute is one documented REST operation. Routes are generated from this
// single table so new handlers only need one row here; the document is rebuilt
// deterministically on every request.
type openAPIRoute struct {
	Method  string
	Path    string
	Summary string
	Tag     string
	Request string   // component schema name for the JSON body
	Query   []string // query parameter names (rendered as integer)
	// QueryBoolean lists boolean query parameters; the generic Query list keeps
	// its integer rendering for the existing limit/offset-style parameters.
	QueryBoolean []string
	Response     string // success status code
	Multipart    bool   // request body is multipart/form-data
	// MultipartField is the single binary field of a multipart request. When
	// empty, the legacy multi-file "files" array schema is generated.
	MultipartField string
	RequestBody    bool
}

var openAPIRoutes = []openAPIRoute{
	{Method: "GET", Path: "/health", Summary: "Service health", Tag: "health", Response: "200"},
	{Method: "GET", Path: "/v1/chunking/strategies", Summary: "List chunking strategies", Tag: "chunking", Response: "200"},
	{Method: "GET", Path: "/v1/admin/knowledge-bases/defaults", Summary: "Knowledge-base defaults", Tag: "knowledge-bases", Response: "200"},
	{Method: "GET", Path: "/v1/admin/knowledge-bases", Summary: "List knowledge bases", Tag: "knowledge-bases", Response: "200"},
	{Method: "POST", Path: "/v1/admin/knowledge-bases", Summary: "Create a knowledge base", Tag: "knowledge-bases", Request: "CreateKnowledgeBaseBody", Response: "201"},
	{Method: "POST", Path: "/v1/admin/knowledge-bases/models/{kind}", Summary: "List provider models", Tag: "knowledge-bases", Response: "200"},
	{Method: "GET", Path: "/v1/admin/knowledge-bases/{namespace}", Summary: "Knowledge-base details", Tag: "knowledge-bases", Response: "200"},
	{Method: "PATCH", Path: "/v1/admin/knowledge-bases/{namespace}", Summary: "Update a knowledge base", Tag: "knowledge-bases", Request: "UpdateKnowledgeBaseBody", Response: "200"},
	{Method: "DELETE", Path: "/v1/admin/knowledge-bases/{namespace}", Summary: "Delete a knowledge base and its documents", Tag: "knowledge-bases", Response: "204"},
	{Method: "POST", Path: "/v1/admin/knowledge-bases/batch-delete", Summary: "Delete knowledge bases in bulk", Tag: "knowledge-bases", Request: "BatchDeleteKnowledgeBasesBody", Response: "200"},
	{Method: "GET", Path: "/v1/admin/knowledge-bases/{namespace}/key/{kind}", Summary: "Reveal a provider key", Tag: "knowledge-bases", Response: "200"},
	{Method: "GET", Path: "/v1/admin/model-profiles", Summary: "List saved model configurations", Tag: "model-profiles", Response: "200"},
	{Method: "POST", Path: "/v1/admin/model-profiles", Summary: "Create a saved model configuration", Tag: "model-profiles", Request: "CreateModelProfileBody", Response: "201"},
	{Method: "GET", Path: "/v1/admin/model-profiles/{profile_id}", Summary: "Saved model configuration details", Tag: "model-profiles", Response: "200"},
	{Method: "PATCH", Path: "/v1/admin/model-profiles/{profile_id}", Summary: "Update a saved model configuration", Tag: "model-profiles", Request: "UpdateModelProfileBody", Response: "200"},
	{Method: "DELETE", Path: "/v1/admin/model-profiles/{profile_id}", Summary: "Delete a saved model configuration", Tag: "model-profiles", Response: "204"},
	{Method: "GET", Path: "/v1/admin/model-profiles/{profile_id}/key", Summary: "Reveal a saved model configuration key", Tag: "model-profiles", Response: "200"},
	{Method: "POST", Path: "/v1/admin/model-profiles/{profile_id}/models", Summary: "List a saved model configuration's provider models", Tag: "model-profiles", Response: "200"},
	{Method: "GET", Path: "/v1/admin/prompts", Summary: "List prompts", Tag: "catalog", Response: "200"},
	{Method: "PUT", Path: "/v1/admin/prompts/{name}", Summary: "Upsert a prompt", Tag: "catalog", Response: "200"},
	{Method: "DELETE", Path: "/v1/admin/prompts/{name}", Summary: "Delete a prompt", Tag: "catalog", Response: "200"},
	{Method: "GET", Path: "/v1/admin/tools", Summary: "List tools", Tag: "catalog", Response: "200"},
	{Method: "PATCH", Path: "/v1/admin/tools/{name}", Summary: "Update a tool", Tag: "catalog", Response: "200"},
	{Method: "GET", Path: "/v1/admin/api-keys", Summary: "List API keys", Tag: "api-keys", Response: "200"},
	{Method: "POST", Path: "/v1/admin/api-keys", Summary: "Create an API key", Tag: "api-keys", Response: "200"},
	{Method: "PUT", Path: "/v1/admin/api-keys/{key_id}", Summary: "Update an API key", Tag: "api-keys", Response: "200"},
	{Method: "DELETE", Path: "/v1/admin/api-keys/{key_id}", Summary: "Delete an API key", Tag: "api-keys", Response: "200"},
	{Method: "POST", Path: "/v1/admin/api-keys/batch-delete", Summary: "Delete API keys in bulk", Tag: "api-keys", Request: "BatchDeleteAPIKeysBody", Response: "200"},
	{Method: "GET", Path: "/v1/admin/api-keys/{key_id}/mcp-config", Summary: "MCP client configuration", Tag: "api-keys", Response: "200"},
	{Method: "GET", Path: "/v1/auth/status", Summary: "Console authentication status", Tag: "auth", Response: "200"},
	{Method: "POST", Path: "/v1/auth/register", Summary: "Register an account", Tag: "auth", Request: "AuthBody", Response: "201"},
	{Method: "POST", Path: "/v1/auth/register/code", Summary: "Send a registration verification code", Tag: "auth", Request: "RegisterCodeBody", Response: "204"},
	{Method: "POST", Path: "/v1/auth/login", Summary: "Log in", Tag: "auth", Request: "AuthBody", Response: "200"},
	{Method: "POST", Path: "/v1/auth/logout", Summary: "Log out", Tag: "auth", Response: "204"},
	{Method: "GET", Path: "/v1/auth/me", Summary: "Current account and CSRF token", Tag: "auth", Response: "200"},
	{Method: "PATCH", Path: "/v1/auth/profile", Summary: "Update the current account profile", Tag: "auth", Request: "AccountProfileBody", Response: "200"},
	{Method: "POST", Path: "/v1/auth/password", Summary: "Change the current account password", Tag: "auth", Request: "PasswordBody", Response: "204"},
	{Method: "POST", Path: "/v1/auth/email/change/code", Summary: "Send an e-mail change verification code", Tag: "auth", Request: "EmailChangeCodeBody", Response: "204"},
	{Method: "POST", Path: "/v1/auth/email/change", Summary: "Apply a verified e-mail change", Tag: "auth", Request: "EmailChangeBody", Response: "200"},
	{Method: "PUT", Path: "/v1/auth/avatar", Summary: "Upload the account avatar", Tag: "auth", Response: "200", Multipart: true, MultipartField: "avatar"},
	{Method: "GET", Path: "/v1/auth/avatar", Summary: "Read the account avatar", Tag: "auth", Response: "200"},
	{Method: "DELETE", Path: "/v1/auth/avatar", Summary: "Remove the account avatar", Tag: "auth", Response: "204"},
	{Method: "GET", Path: "/v1/admin/mail-settings", Summary: "Read the mail server settings", Tag: "admin", Response: "200"},
	{Method: "PUT", Path: "/v1/admin/mail-settings", Summary: "Update the mail server settings", Tag: "admin", Request: "MailSettingsBody", Response: "200"},
	{Method: "POST", Path: "/v1/admin/mail-settings/test", Summary: "Send a test mail", Tag: "admin", Request: "MailTestBody", Response: "204"},
	{Method: "GET", Path: "/v1/admin/users", Summary: "List accounts", Tag: "users", Response: "200"},
	{Method: "PATCH", Path: "/v1/admin/users/{user_id}", Summary: "Update an account", Tag: "users", Request: "UserUpdateBody", Response: "200"},
	{Method: "DELETE", Path: "/v1/admin/users/{user_id}", Summary: "Delete an account", Tag: "users", Response: "204"},
	{Method: "POST", Path: "/v1/search", Summary: "Search a knowledge base", Tag: "search", Request: "SearchBody", Response: "200"},
	{Method: "GET", Path: "/v1/artifacts/{artifact_id}", Summary: "Read an artifact", Tag: "search", Response: "200"},
	{Method: "GET", Path: "/v1/artifacts/{artifact_id}/content", Summary: "Download artifact content", Tag: "search", Response: "200"},
	{Method: "GET", Path: "/v1/documents", Summary: "List documents", Tag: "documents", Response: "200"},
	{Method: "POST", Path: "/v1/documents/check", Summary: "Check document names", Tag: "documents", Request: "DocumentsCheckBody", Response: "200"},
	{Method: "GET", Path: "/v1/document-jobs/{artifact_id}", Summary: "Read an asynchronous document job status", Tag: "documents", Response: "200"},
	{Method: "POST", Path: "/v1/document-batches", Summary: "Upload documents as one durable batch (202 with per-file outcomes)", Tag: "documents", Response: "202", Multipart: true, RequestBody: true},
	{Method: "GET", Path: "/v1/document-batches", Summary: "List the caller's unfinished document batches", Tag: "documents", Query: []string{"limit"}, Response: "200"},
	{Method: "GET", Path: "/v1/document-batches/{batch_id}", Summary: "Read one document batch with per-file progress", Tag: "documents", Response: "200"},
	{Method: "GET", Path: "/v1/document-batches/upload-events/{upload_id}", Summary: "Stream one upload's file progress and per-file save results (SSE)", Tag: "documents", Response: "200"},
	{Method: "GET", Path: "/v1/documents/{name}", Summary: "Read document metadata", Tag: "documents", Response: "200"},
	{Method: "POST", Path: "/v1/documents/{name}/replace", Summary: "Replace one document file and rebuild its index", Tag: "documents", Response: "200", Multipart: true, MultipartField: "file", RequestBody: true},
	{Method: "DELETE", Path: "/v1/documents/{name}", Summary: "Delete a document", Tag: "documents", Response: "200"},
	{Method: "GET", Path: "/v1/documents/{name}/content", Summary: "Download document content", Tag: "documents", Response: "200"},
	{Method: "GET", Path: "/v1/feishu/status", Summary: "Read the account's Feishu connection status", Tag: "feishu", Response: "200"},
	{Method: "POST", Path: "/v1/feishu/oauth/start", Summary: "Start one Feishu user authorization", Tag: "feishu", Response: "200"},
	{Method: "GET", Path: "/v1/feishu/oauth/callback", Summary: "Complete the Feishu OAuth callback", Tag: "feishu", Query: []string{"code", "state", "error"}, Response: "302"},
	{Method: "DELETE", Path: "/v1/feishu/connection", Summary: "Disconnect the account's Feishu authorization", Tag: "feishu", Response: "204"},
	{Method: "GET", Path: "/v1/feishu/files", Summary: "List one Feishu folder level", Tag: "feishu", Query: []string{"folder_token", "page_token", "page_size"}, Response: "200"},
	{Method: "GET", Path: "/v1/feishu/search", Summary: "Search the account's Feishu documents", Tag: "feishu", Query: []string{"q", "offset", "limit"}, Response: "200"},
	{Method: "POST", Path: "/v1/feishu/import", Summary: "Import selected Feishu documents into the current knowledge base", Tag: "feishu", Request: "FeishuImportBody", Response: "202"},
	{Method: "GET", Path: "/v1/knowledge-base", Summary: "Public knowledge-base settings", Tag: "knowledge-bases", Response: "200"},
	{Method: "GET", Path: "/v1/namespaces", Summary: "List knowledge-base namespaces", Tag: "knowledge-bases", Response: "200"},
	{Method: "GET", Path: "/v1/membership", Summary: "Read the current account's membership, quota and recent orders", Tag: "membership", Response: "200"},
	{Method: "GET", Path: "/v1/membership/orders", Summary: "List the current account's membership orders", Tag: "membership", Response: "200"},
	{Method: "POST", Path: "/v1/membership/orders", Summary: "Create or resume one server-priced membership order", Tag: "membership", Request: "CreateMembershipOrderBody", Response: "201"},
	{Method: "GET", Path: "/v1/membership/orders/{order_id}", Summary: "Read one own order; refresh=1 queries Alipay", Tag: "membership", QueryBoolean: []string{"refresh"}, Response: "200"},
	{Method: "POST", Path: "/v1/membership/orders/{order_id}/close", Summary: "Close one pending membership order", Tag: "membership", Response: "200"},
	{Method: "GET", Path: "/v1/workspaces/{namespace}/entitlements", Summary: "Read the knowledge-base owner's file and Embedding entitlement", Tag: "membership", Response: "200"},
	{Method: "GET", Path: "/v1/admin/billing/orders", Summary: "List membership orders for reconciliation", Tag: "membership", Query: []string{"status", "limit"}, Response: "200"},
	{Method: "POST", Path: "/v1/admin/billing/orders/{order_id}/query", Summary: "Actively query Alipay for one order", Tag: "membership", Response: "200"},
	{Method: "POST", Path: "/v1/admin/billing/orders/{order_id}/refund", Summary: "Refund one whole paid order", Tag: "membership", Request: "BillingRefundBody", Response: "200"},
	{Method: "POST", Path: "/v1/admin/billing/orders/{order_id}/manual", Summary: "Mark one order for manual reconciliation", Tag: "membership", Request: "BillingManualBody", Response: "200"},
	{Method: "POST", Path: "/v1/payments/alipay/notify", Summary: "Alipay asynchronous payment notification (RSA2 verified)", Tag: "membership", Response: "200"},
	{Method: "GET", Path: "/v1/uploads", Summary: "List upload operations", Tag: "uploads", Response: "200"},
	{Method: "GET", Path: "/v1/uploads/{operation_id}", Summary: "Upload operation details", Tag: "uploads", Response: "200"},
	{Method: "GET", Path: "/v1/admin/published-versions", Summary: "List published versions", Tag: "admin", Response: "200"},
	{Method: "GET", Path: "/v1/admin/upload-leases", Summary: "List upload leases", Tag: "admin", Response: "200"},
	{Method: "GET", Path: "/v1/admin/monitor/overview", Summary: "Read-only monitoring snapshot", Tag: "admin", Response: "200"},
	{Method: "GET", Path: "/v1/admin/monitor/logs", Summary: "Read-only monitor log page (memory ring or RAG_LOG_FILE)", Tag: "admin", Query: []string{"source", "component", "level", "query", "limit", "offset"}, Response: "200"},
	{Method: "POST", Path: "/v1/admin/monitor/actions/requeue-jobs", Summary: "Requeue due retries and expired document-job leases now (sweep semantics)", Tag: "admin", Response: "200"},
	{Method: "POST", Path: "/v1/admin/monitor/actions/connectors/{connector}/restart", Summary: "Restart one capture connector (fixed names only) and reset its restart budget", Tag: "admin", QueryBoolean: []string{"includeTasks"}, Response: "200"},
	{Method: "GET", Path: "/v1/admin/monitor/actions/worker-deployment", Summary: "Read the document worker Deployment replica state (Kubernetes only)", Tag: "admin", Response: "200"},
	{Method: "POST", Path: "/v1/admin/monitor/actions/worker-deployment/scale", Summary: "Scale the document worker Deployment (kubectl scale equivalent)", Tag: "admin", Request: "WorkerScaleBody", Response: "200"},
	{Method: "GET", Path: "/v1/users", Summary: "List accounts available for an invitation", Tag: "sharing", Response: "200"},
	{Method: "POST", Path: "/v1/knowledge-bases", Summary: "Create a knowledge base owned by the current account", Tag: "knowledge-bases", Request: "SelfCreateKnowledgeBaseBody", Response: "201"},
	{Method: "GET", Path: "/v1/knowledge-bases/{namespace}/access", Summary: "Read the current account's knowledge-base access", Tag: "sharing", Response: "200"},
	{Method: "GET", Path: "/v1/knowledge-bases/{namespace}/permissions", Summary: "List knowledge-base permissions", Tag: "sharing", Response: "200"},
	{Method: "DELETE", Path: "/v1/knowledge-bases/{namespace}/permissions/{user_id}", Summary: "Revoke a knowledge-base permission", Tag: "sharing", Response: "204"},
	{Method: "GET", Path: "/v1/knowledge-bases/{namespace}/invitations", Summary: "List knowledge-base invitations", Tag: "sharing", Response: "200"},
	{Method: "POST", Path: "/v1/knowledge-bases/{namespace}/invitations", Summary: "Create a share invitation", Tag: "sharing", Request: "CreateInvitationBody", Response: "201"},
	{Method: "GET", Path: "/v1/invitations", Summary: "List the invitations addressed to the current account", Tag: "sharing", Response: "200"},
	{Method: "POST", Path: "/v1/invitations/preview", Summary: "Preview a share invitation link", Tag: "sharing", Request: "InvitationTokenBody", Response: "200"},
	{Method: "POST", Path: "/v1/invitations/accept", Summary: "Accept a share invitation", Tag: "sharing", Request: "InvitationTokenBody", Response: "200"},
	{Method: "POST", Path: "/v1/invitations/decline", Summary: "Decline a share invitation", Tag: "sharing", Request: "InvitationTokenBody", Response: "200"},
	{Method: "DELETE", Path: "/v1/invitations/{invitation_id}", Summary: "Revoke a pending invitation", Tag: "sharing", Response: "204"},
}

// openAPISchemas returns the component schemas referenced by the route table.
func openAPISchemas() map[string]any {
	return map[string]any{
		"CreateMembershipOrderBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"plan":   map[string]any{"type": "string", "enum": []any{"plus", "pro"}, "title": "Plan"},
				"period": map[string]any{"type": "string", "enum": []any{"month", "year"}, "title": "Period"},
				"intent": map[string]any{"type": "string", "enum": []any{"purchase", "upgrade"}, "title": "Intent"},
			},
		},
		"BillingRefundBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"reason": map[string]any{"type": "string", "maxLength": 500, "title": "Reason"},
			},
		},
		"BillingManualBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"note": map[string]any{"type": "string", "maxLength": 1000, "title": "Note"},
			},
		},
		"SearchBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"query"},
			"properties": map[string]any{
				"query":       map[string]any{"type": "string", "title": "Query"},
				"retrieval":   map[string]any{"type": "string", "title": "Retrieval"},
				"rerank":      map[string]any{"type": "string", "enum": []any{"none", "cross_encoder"}, "title": "Rerank"},
				"candidate_k": map[string]any{"type": "integer", "title": "Candidate K"},
				"top_k":       map[string]any{"type": "integer", "title": "Top K"},
				"filters":     map[string]any{"type": "object", "title": "Filters"},
			},
		},
		"BatchDeleteKnowledgeBasesBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"namespaces"},
			"properties": map[string]any{
				"namespaces": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"title": "Namespaces",
				},
			},
		},
		"BatchDeleteAPIKeysBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"ids"},
			"properties": map[string]any{
				"ids": map[string]any{
					"type": "array", "items": map[string]any{"type": "string"},
					"title": "Ids",
				},
			},
		},
		"DocumentsCheckBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"names"},
			"properties": map[string]any{
				"names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "title": "Names"},
			},
		},
		"FeishuImportBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"documents"},
			"properties": map[string]any{
				"documents": map[string]any{
					"type": "array", "minItems": 1, "maxItems": 10, "title": "Documents",
					"items": map[string]any{
						"type":                 "object",
						"additionalProperties": false,
						"required":             []any{"document_id", "title"},
						"properties": map[string]any{
							"document_id": map[string]any{"type": "string", "minLength": 1, "maxLength": 128, "title": "Document Id"},
							"title":       map[string]any{"type": "string", "minLength": 1, "maxLength": 200, "title": "Title"},
						},
					},
				},
			},
		},
		"AuthBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"username", "password"},
			"properties": map[string]any{
				"username": map[string]any{"type": "string", "minLength": 3, "maxLength": 64, "title": "Username"},
				"password": map[string]any{"type": "string", "minLength": 8, "maxLength": 128, "title": "Password"},
				"email":    map[string]any{"type": "string", "maxLength": 254, "title": "Email"},
				"code":     map[string]any{"type": "string", "maxLength": 16, "title": "Code"},
			},
		},
		"RegisterCodeBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"email"},
			"properties": map[string]any{
				"email": map[string]any{"type": "string", "maxLength": 254, "title": "Email"},
			},
		},
		"AccountProfileBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"username": map[string]any{"type": "string", "minLength": 3, "maxLength": 64, "title": "Username"},
				"age":      map[string]any{"type": "integer", "minimum": 1, "maximum": 150, "nullable": true, "title": "Age"},
				"gender":   map[string]any{"type": "string", "enum": []any{"", "male", "female", "other"}, "title": "Gender"},
			},
		},
		"PasswordBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"current_password", "new_password"},
			"properties": map[string]any{
				"current_password": map[string]any{"type": "string", "title": "Current Password"},
				"new_password":     map[string]any{"type": "string", "minLength": 8, "maxLength": 128, "title": "New Password"},
			},
		},
		"EmailChangeCodeBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"email", "password"},
			"properties": map[string]any{
				"email":    map[string]any{"type": "string", "maxLength": 254, "title": "Email"},
				"password": map[string]any{"type": "string", "title": "Password"},
			},
		},
		"EmailChangeBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"email", "code"},
			"properties": map[string]any{
				"email": map[string]any{"type": "string", "maxLength": 254, "title": "Email"},
				"code":  map[string]any{"type": "string", "maxLength": 16, "title": "Code"},
			},
		},
		"MailSettingsBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"enabled":  map[string]any{"type": "boolean", "title": "Enabled"},
				"host":     map[string]any{"type": "string", "maxLength": 255, "title": "Host"},
				"port":     map[string]any{"type": "integer", "minimum": 1, "maximum": 65535, "default": 587, "title": "Port"},
				"username": map[string]any{"type": "string", "title": "Username"},
				"password": map[string]any{"type": "string", "title": "Password"},
				"from":     map[string]any{"type": "string", "maxLength": 254, "title": "From"},
				"tls":      map[string]any{"type": "string", "enum": []any{"none", "starttls", "tls"}, "default": "starttls", "title": "Tls"},
			},
		},
		"MailTestBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"to"},
			"properties": map[string]any{
				"to": map[string]any{"type": "string", "maxLength": 254, "title": "To"},
			},
		},
		"UserUpdateBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"role"},
			"properties": map[string]any{
				"role":    map[string]any{"type": "string", "enum": []any{"admin", "vip", "user"}, "title": "Role"},
				"enabled": map[string]any{"type": "boolean", "default": true, "title": "Enabled"},
			},
		},
		"SelfCreateKnowledgeBaseBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"namespace"},
			"properties": map[string]any{
				"namespace":     map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$", "title": "Namespace"},
				"strategy":      map[string]any{"type": "string", "default": "recursive", "title": "Strategy"},
				"chunk_size":    map[string]any{"type": "integer", "nullable": true, "title": "Chunk Size"},
				"chunk_overlap": map[string]any{"type": "integer", "nullable": true, "title": "Chunk Overlap"},
			},
		},
		"CreateInvitationBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"username", "permission", "scope"},
			"properties": map[string]any{
				"username":       map[string]any{"type": "string", "title": "Username"},
				"permission":     map[string]any{"type": "string", "enum": []any{"read", "write"}, "title": "Permission"},
				"scope":          map[string]any{"type": "string", "enum": []any{"all", "files"}, "title": "Scope"},
				"document_names": map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "default": []any{}, "title": "Document Names"},
			},
		},
		"InvitationTokenBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"token"},
			"properties": map[string]any{
				"token": map[string]any{"type": "string", "title": "Token"},
			},
		},
		"ProviderSettingsBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"base_url": map[string]any{"type": "string", "title": "Base Url"},
				"apikey":   map[string]any{"type": "string", "title": "Apikey"},
			},
		},
		"WorkerScaleBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"replicas"},
			"properties": map[string]any{
				"replicas": map[string]any{"type": "integer", "minimum": 0, "maximum": 64, "title": "Replicas"},
			},
		},
		"CreateKnowledgeBaseBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"namespace", "embedding", "embedding_model"},
			"properties": map[string]any{
				"namespace":       map[string]any{"type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$", "title": "Namespace"},
				"embedding":       map[string]any{"$ref": "#/components/schemas/ProviderSettingsBody"},
				"reranker":        map[string]any{"$ref": "#/components/schemas/ProviderSettingsBody"},
				"embedding_model": map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "title": "Embedding Model"},
				"reranker_model":  map[string]any{"type": "string", "maxLength": 512, "default": "", "title": "Reranker Model"},
				"vlm":             map[string]any{"$ref": "#/components/schemas/ProviderSettingsBody"},
				"vlm_model":       map[string]any{"type": "string", "maxLength": 512, "default": "", "title": "VLM Model"},
				"strategy":        map[string]any{"type": "string", "default": "recursive", "title": "Strategy"},
				"chunk_size":      map[string]any{"type": "integer", "nullable": true, "title": "Chunk Size"},
				"chunk_overlap":   map[string]any{"type": "integer", "nullable": true, "title": "Chunk Overlap"},
			},
		},
		"UpdateKnowledgeBaseBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"embedding":       map[string]any{"$ref": "#/components/schemas/ProviderSettingsBody"},
				"reranker":        map[string]any{"$ref": "#/components/schemas/ProviderSettingsBody"},
				"embedding_model": map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "title": "Embedding Model"},
				"reranker_model":  map[string]any{"type": "string", "maxLength": 512, "title": "Reranker Model"},
				"vlm":             map[string]any{"$ref": "#/components/schemas/ProviderSettingsBody"},
				"vlm_model":       map[string]any{"type": "string", "maxLength": 512, "title": "VLM Model"},
				"reembed":         map[string]any{"type": "boolean", "default": false, "title": "Reembed"},
			},
		},
		"CreateModelProfileBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"required":             []any{"kind", "name", "base_url", "model"},
			"properties": map[string]any{
				"kind":     map[string]any{"type": "string", "enum": []any{"embedding", "reranker", "vlm", "jev"}, "title": "Kind"},
				"name":     map[string]any{"type": "string", "minLength": 1, "maxLength": 80, "title": "Name"},
				"base_url": map[string]any{"type": "string", "maxLength": 2048, "title": "Base Url"},
				"apikey":   map[string]any{"type": "string", "title": "Apikey"},
				"model":    map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "title": "Model"},
			},
		},
		"UpdateModelProfileBody": map[string]any{
			"type":                 "object",
			"additionalProperties": false,
			"properties": map[string]any{
				"name":     map[string]any{"type": "string", "minLength": 1, "maxLength": 80, "title": "Name"},
				"base_url": map[string]any{"type": "string", "maxLength": 2048, "title": "Base Url"},
				"apikey":   map[string]any{"type": "string", "title": "Apikey"},
				"model":    map[string]any{"type": "string", "minLength": 1, "maxLength": 512, "title": "Model"},
			},
		},
	}
}

// registerDocumentationRoutes serves the unauthenticated OpenAPI document and a
// self-contained docs page, matching FastAPI's /openapi.json and /docs.
func (s *Server) registerDocumentationRoutes() {
	s.router.add("GET", "/openapi.json", authNone, s.handleOpenAPI)
	s.router.add("GET", "/docs", authNone, s.handleDocs)
}

func (s *Server) handleOpenAPI(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, buildOpenAPIDocument())
}

func (s *Server) handleDocs(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(docsHTML))
}

// buildOpenAPIDocument generates a deterministic OpenAPI 3 document from the
// route table. Map keys are sorted by encoding/json, so repeated requests are
// byte-identical.
func buildOpenAPIDocument() map[string]any {
	paths := make(map[string]any, len(openAPIRoutes))
	for _, route := range openAPIRoutes {
		pathItem, _ := paths[route.Path].(map[string]any)
		if pathItem == nil {
			pathItem = map[string]any{}
			paths[route.Path] = pathItem
		}
		operation := map[string]any{
			"summary":     route.Summary,
			"tags":        []any{route.Tag},
			"operationId": operationID(route),
			"responses": map[string]any{
				route.Response: map[string]any{"description": "Successful Response"},
			},
		}
		parameters := make([]any, 0, len(route.Query)+2)
		for _, name := range pathParameters(route.Path) {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "path", "required": true,
				"schema": map[string]any{"type": "string"},
			})
		}
		for _, name := range route.Query {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "query", "required": false,
				"schema": map[string]any{"type": "integer"},
			})
		}
		for _, name := range route.QueryBoolean {
			parameters = append(parameters, map[string]any{
				"name": name, "in": "query", "required": false,
				"schema": map[string]any{"type": "boolean", "default": true},
			})
		}
		if len(parameters) > 0 {
			operation["parameters"] = parameters
		}
		if route.RequestBody || route.Request != "" {
			contentType := "application/json"
			schema := map[string]any{"type": "object"}
			if route.Request != "" {
				schema = map[string]any{"$ref": "#/components/schemas/" + route.Request}
			}
			if route.Multipart {
				contentType = "multipart/form-data"
				if route.MultipartField != "" {
					schema = map[string]any{
						"type":     "object",
						"required": []any{route.MultipartField},
						"properties": map[string]any{
							route.MultipartField: map[string]any{"type": "string", "format": "binary"},
						},
					}
				} else {
					schema = map[string]any{
						"type":     "object",
						"required": []any{"files"},
						"properties": map[string]any{
							"files": map[string]any{
								"type":  "array",
								"items": map[string]any{"type": "string", "format": "binary"},
							},
						},
					}
				}
			}
			operation["requestBody"] = map[string]any{
				"required": true,
				"content":  map[string]any{contentType: map[string]any{"schema": schema}},
			}
			operation["responses"].(map[string]any)["422"] = map[string]any{
				"description": "Validation Error",
				"content": map[string]any{
					"application/json": map[string]any{
						"schema": map[string]any{"$ref": "#/components/schemas/HTTPValidationError"},
					},
				},
			}
		}
		pathItem[strings.ToLower(route.Method)] = operation
	}
	return map[string]any{
		"openapi": "3.1.0",
		"info": map[string]any{
			"title":   "Praxis-RAG",
			"version": version.Version,
		},
		"paths": paths,
		"components": map[string]any{
			"schemas": openAPISchemas(),
		},
	}
}

// operationID produces a stable id from the method and path, e.g.
// get_v1_admin_knowledge_bases__namespace_.
func operationID(route openAPIRoute) string {
	builder := strings.Builder{}
	builder.WriteString(strings.ToLower(route.Method))
	for _, character := range route.Path {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') {
			builder.WriteRune(character)
			continue
		}
		builder.WriteByte('_')
	}
	return builder.String()
}

func pathParameters(path string) []string {
	parameters := make([]string, 0)
	for start := 0; ; {
		openIndex := strings.IndexByte(path[start:], '{')
		if openIndex < 0 {
			return parameters
		}
		start += openIndex + 1
		closeIndex := strings.IndexByte(path[start:], '}')
		if closeIndex < 0 {
			return parameters
		}
		parameters = append(parameters, path[start:start+closeIndex])
		start += closeIndex + 1
	}
}

const docsHTML = `<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8">
<title>Praxis-RAG API docs</title>
<style>
body { font-family: system-ui, sans-serif; margin: 2rem auto; max-width: 960px; line-height: 1.5; }
code { background: #f2f2f2; padding: .1rem .3rem; border-radius: 4px; }
.route { border-bottom: 1px solid #eee; padding: .4rem 0; }
.method { display: inline-block; width: 4.5rem; font-weight: 600; }
</style>
</head>
<body>
<h1>Praxis-RAG API</h1>
<p>Machine-readable contract: <a href="/openapi.json"><code>/openapi.json</code></a></p>
<div id="routes">Loading routes&hellip;</div>
<script>
fetch('/openapi.json').then(function (response) { return response.json(); }).then(function (schema) {
  var container = document.getElementById('routes');
  container.textContent = '';
  Object.keys(schema.paths).sort().forEach(function (path) {
    Object.keys(schema.paths[path]).forEach(function (method) {
      var operation = schema.paths[path][method];
      var row = document.createElement('div');
      row.className = 'route';
      var verb = document.createElement('span');
      verb.className = 'method';
      verb.textContent = method.toUpperCase();
      row.appendChild(verb);
      var code = document.createElement('code');
      code.textContent = path;
      row.appendChild(code);
      row.appendChild(document.createTextNode(' — ' + (operation.summary || operation.operationId)));
      container.appendChild(row);
    });
  });
});
</script>
</body>
</html>
`
