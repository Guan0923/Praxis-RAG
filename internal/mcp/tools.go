package mcp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf8"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/xinghe_rag/internal/contracts"
	"github.com/Guan0923/xinghe_rag/internal/resources"
)

// Tool names are part of the stable MCP contract; never rename them.
const (
	searchToolName          = "search"
	getSearchResultToolName = "get_search_result"
	addDocumentToolName     = "add_document"
	updateDocumentToolName  = "update_document"
	deleteDocumentToolName  = "delete_document"
	documentStatusToolName  = "get_document_status"
)

// supportedExtensions mirrors service_knowledge.SUPPORTED_EXTENSIONS.
var supportedExtensions = map[string]struct{}{
	".txt": {}, ".md": {}, ".pdf": {}, ".docx": {}, ".doc": {},
}

// windowsReservedNames mirrors pathlib._WindowsFlavour reserved device names.
var windowsReservedNames = map[string]struct{}{
	"CON": {}, "PRN": {}, "AUX": {}, "NUL": {}, "CONIN$": {}, "CONOUT$": {},
	"COM1": {}, "COM2": {}, "COM3": {}, "COM4": {}, "COM5": {}, "COM6": {}, "COM7": {}, "COM8": {}, "COM9": {},
	"LPT1": {}, "LPT2": {}, "LPT3": {}, "LPT4": {}, "LPT5": {}, "LPT6": {}, "LPT7": {}, "LPT8": {}, "LPT9": {},
	// Superscript digits are accepted by Windows as COM¹/COM²/COM³/LPT¹/LPT²/LPT³.
	"COM\u00b9": {}, "COM\u00b2": {}, "COM\u00b3": {},
	"LPT\u00b9": {}, "LPT\u00b2": {}, "LPT\u00b3": {},
}

// toolEntry is one registered MCP tool: its definition, its argument validator
// and its execution boundary. validate has to be deterministic and safe to run
// on untrusted agent input.
type toolEntry struct {
	name       string
	definition *sdk.Tool
	validate   func(arguments map[string]any) (any, error)
	execute    func(ctx context.Context, namespace string, input any) (map[string]any, error)
}

// readOnlyAnnotations mirrors the Python search/get_search_result annotations.
func readOnlyAnnotations() *sdk.ToolAnnotations {
	destructive := false
	openWorld := false
	return &sdk.ToolAnnotations{
		ReadOnlyHint:    true,
		DestructiveHint: &destructive,
		IdempotentHint:  true,
		OpenWorldHint:   &openWorld,
	}
}

// buildToolEntries assembles the ordered tool registry. The order is part of
// the legacy tools/list contract observed by existing clients.
func (e *Endpoint) buildToolEntries() []toolEntry {
	entries := make([]toolEntry, 0, 6)
	queued := e.opts.QueuedSearch

	searchDescription := contracts.SearchToolDescription
	if queued {
		searchDescription += contracts.QueuedSearchToolDescriptionSuffix
	}
	entries = append(entries, toolEntry{
		name: searchToolName,
		definition: &sdk.Tool{
			Name:        searchToolName,
			Description: searchDescription,
			InputSchema: contracts.SearchToolSchema(queued),
			Annotations: readOnlyAnnotations(),
		},
		validate: func(arguments map[string]any) (any, error) {
			if queued {
				return contracts.QueuedSearchRequestFromArguments(arguments)
			}
			return contracts.SearchRequestFromArguments(arguments)
		},
		execute: func(ctx context.Context, namespace string, input any) (map[string]any, error) {
			if queued {
				return e.executeQueuedSearch(ctx, namespace, input.(contracts.QueuedSearchRequest))
			}
			return e.opts.Runtime.Search(ctx, namespace, input.(contracts.SearchRequest))
		},
	})

	entries = append(entries, toolEntry{
		name: getSearchResultToolName,
		definition: &sdk.Tool{
			Name: getSearchResultToolName,
			Description: "Read a search by result_id UUID without running retrieval again. Pending queued " +
				"searches return status queued and their stage; completed searches return frozen results. " +
				"Failed, timed-out or expired searches return errors.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"result_id": map[string]any{
						"type": "string", "format": "uuid", "minLength": 36, "maxLength": 36,
					},
					"page": map[string]any{
						"type": "integer", "minimum": 1, "maximum": contracts.MaxResultPage, "default": 1,
					},
					"page_size": map[string]any{
						"type": "integer", "minimum": 1, "maximum": 100, "default": 10,
					},
				},
				"required":             []string{"result_id"},
				"additionalProperties": false,
			},
			Annotations: readOnlyAnnotations(),
		},
		validate: func(arguments map[string]any) (any, error) {
			return contracts.ValidateSearchResultArguments(arguments)
		},
		execute: func(ctx context.Context, namespace string, input any) (map[string]any, error) {
			request := input.(contracts.SearchResultRequest)
			return e.opts.Runtime.GetSearchResult(ctx, namespace, request.ResultID, request.Page, request.PageSize)
		},
	})

	entries = append(entries,
		e.documentEntry(addDocumentToolName,
			"Upload one new file for asynchronous indexing. Returns artifact_id once the request is durably "+
				"committed to the service database, NOT after indexing. The message broker is not on the "+
				"submission path: a committed upload is always delivered later. Poll get_document_status until "+
				"succeeded before searching. Rejects existing or queued filenames. Reuse idempotency_key after "+
				"uncertain submissions. Duplicate content under another name is allowed."),
		e.documentEntry(updateDocumentToolName,
			"Replace the complete content of an existing knowledge file and rebuild its index. Requires the "+
				"exact existing filename and preserves its chunking settings. This overwrites content; use only "+
				"when the user requests an update."),
		e.documentEntry(deleteDocumentToolName,
			"Delete an existing knowledge file and its search index by exact filename. Use only when the user "+
				"requests deletion. This cannot be undone through MCP."),
	)

	entries = append(entries, toolEntry{
		name: documentStatusToolName,
		definition: &sdk.Tool{
			Name: documentStatusToolName,
			Description: "Query an uploaded file by artifact_id in the connected knowledge base. Returns status " +
				"(queued, processing, succeeded, failed), current stage and a safe error. Only succeeded means " +
				"indexed and searchable.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"artifact_id": map[string]any{
						"type": "string", "format": "uuid", "minLength": 36, "maxLength": 36,
					},
				},
				"required":             []string{"artifact_id"},
				"additionalProperties": false,
			},
			Annotations: readOnlyAnnotations(),
		},
		validate: func(arguments map[string]any) (any, error) {
			return parseDocumentStatusArguments(arguments)
		},
		execute: func(ctx context.Context, namespace string, input any) (map[string]any, error) {
			request := input.(documentStatusRequest)
			payload, err := e.opts.Runtime.GetDocumentStatus(ctx, namespace, request.artifactID)
			if err != nil {
				if errors.Is(err, ErrUploadConflict) {
					return nil, fmt.Errorf("Document conflicts with an existing filename, queued upload or idempotency key.")
				}
				return nil, err
			}
			return payload, nil
		},
	})

	return entries
}

// documentEntry builds one of add_document/update_document/delete_document.
func (e *Endpoint) documentEntry(name, description string) toolEntry {
	writing := name != deleteDocumentToolName
	if writing {
		description += " Supply content for UTF-8 TXT/Markdown, or content_base64 for file bytes (required " +
			"for PDF/DOCX/DOC). Maximum decoded size: 10 MiB. No server paths or URLs."
	}
	properties := map[string]any{
		"name": map[string]any{
			"type": "string", "minLength": 1, "maxLength": 200,
			"description": "Exact filename including extension; no directory path.",
		},
	}
	schema := map[string]any{
		"type":                 "object",
		"properties":           properties,
		"required":             []string{"name"},
		"additionalProperties": false,
	}
	if writing {
		idempotencyDefault := any(nil)
		properties["content"] = map[string]any{
			"type": "string", "minLength": 1, "maxLength": MaxFileBytes,
		}
		properties["content_base64"] = map[string]any{
			"type": "string", "minLength": 1, "maxLength": 4 * ((MaxFileBytes + 2) / 3),
		}
		properties["idempotency_key"] = map[string]any{
			"type": []string{"string", "null"}, "minLength": 1, "maxLength": 128,
			"default":     idempotencyDefault,
			"description": "Optional non-secret retry key; reuse only for the same write. Not a credential.",
		}
		schema["oneOf"] = []any{
			map[string]any{"required": []string{"content"}},
			map[string]any{"required": []string{"content_base64"}},
		}
	}

	destructive := writing && name != addDocumentToolName
	openWorld := false
	annotations := &sdk.ToolAnnotations{
		ReadOnlyHint:    false,
		DestructiveHint: &destructive,
		IdempotentHint:  false,
		OpenWorldHint:   &openWorld,
	}

	return toolEntry{
		name: name,
		definition: &sdk.Tool{
			Name:        name,
			Description: description,
			InputSchema: schema,
			Annotations: annotations,
		},
		validate: func(arguments map[string]any) (any, error) {
			return parseDocumentArguments(arguments, writing)
		},
		execute: func(ctx context.Context, namespace string, input any) (map[string]any, error) {
			request := input.(documentRequest)
			return e.executeDocumentOperation(ctx, namespace, name, request)
		},
	}
}

// executeQueuedSearch ports McpHttpEndpoint.search_queued: the submission is
// shielded from client cancellation, then the result is polled for at most
// wait_seconds. A wait timeout is a success that only carries result_id.
func (e *Endpoint) executeQueuedSearch(ctx context.Context, namespace string, request contracts.QueuedSearchRequest) (map[string]any, error) {
	resultID, err := e.opts.Runtime.SubmitSearch(context.WithoutCancel(ctx), namespace, request.Search)
	if err != nil {
		return nil, err
	}
	response := map[string]any{"result_id": resultID}
	if request.WaitSeconds <= 0 {
		return response, nil
	}
	deadline := time.Now().Add(time.Duration(request.WaitSeconds * float64(time.Second)))
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return response, nil
		}
		type readResult struct {
			payload map[string]any
			err     error
		}
		reads := make(chan readResult, 1)
		go func() {
			payload, readErr := e.opts.Runtime.GetSearchResult(ctx, namespace, resultID, 1, 10)
			reads <- readResult{payload: payload, err: readErr}
		}()
		select {
		case read := <-reads:
			if read.err != nil {
				return nil, read.err
			}
			status, _ := read.payload["status"].(string)
			if status != "queued" {
				return read.payload, nil
			}
			pause := 100 * time.Millisecond
			if left := time.Until(deadline); left < pause {
				pause = left
			}
			if pause > 0 {
				sleep := time.NewTimer(pause)
				select {
				case <-sleep.C:
				case <-ctx.Done():
					sleep.Stop()
					return response, nil
				}
			}
		case <-time.After(remaining):
			return response, nil
		case <-ctx.Done():
			return response, nil
		}
	}
}

// executeDocumentOperation ports McpHttpEndpoint.mutate_document for the
// synchronous document operations (add/status go straight to the runtime).
func (e *Endpoint) executeDocumentOperation(ctx context.Context, namespace, operation string, request documentRequest) (map[string]any, error) {
	switch operation {
	case addDocumentToolName:
		payload, err := e.opts.Runtime.AddDocument(ctx, namespace, request.name, request.content, request.idempotencyKey)
		if err != nil {
			if errors.Is(err, ErrUploadConflict) {
				return nil, fmt.Errorf("Document conflicts with an existing filename, queued upload or idempotency key.")
			}
			return nil, err
		}
		return payload, nil

	case deleteDocumentToolName:
		documents, err := e.opts.Runtime.ListDocuments(ctx, namespace)
		if err != nil {
			return nil, err
		}
		found := false
		for _, document := range documents {
			if document.Name == request.name {
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("Document does not exist: %s", request.name)
		}
		payload, err := e.opts.Runtime.DeleteDocument(ctx, namespace, request.name)
		if err != nil {
			if errors.Is(err, ErrUploadConflict) {
				return nil, fmt.Errorf("Upload conflicts with another operation or idempotency key.")
			}
			return nil, err
		}
		deleted, ok := payload["deleted"]
		if !ok {
			return nil, fmt.Errorf("Document operation failed: missing delete result")
		}
		delete(payload, "deleted")
		payload["document"] = deleted
		payload["status"] = "deleted"
		// TODO(G6 resources/subscriptions): publish resources/list_changed and
		// notifications/resources/updated from the post-delete document list.
		delete(payload, "documents")
		payload["uri"] = resources.DocumentURI(namespace, request.name)
		return payload, nil

	case updateDocumentToolName:
		payload, err := e.opts.Runtime.WriteDocument(
			ctx, namespace, request.name, request.content, true, request.idempotencyKey)
		if err != nil {
			if errors.Is(err, ErrUploadConflict) {
				return nil, fmt.Errorf("Upload conflicts with another operation or idempotency key.")
			}
			return nil, err
		}
		// TODO(G6 resources/subscriptions): publish resources/list_changed and
		// notifications/resources/updated from the post-write document list.
		delete(payload, "documents")
		payload["uri"] = resources.DocumentURI(namespace, request.name)
		return payload, nil
	}
	return nil, fmt.Errorf("Unknown document operation")
}

// toolEnabled reports whether one registered tool may execute for this
// namespace. Execution always bypasses any read-model cache, matching
// ToolRegistry.enabled_for_execution.
func (e *Endpoint) toolEnabled(ctx context.Context, namespace, name string) (bool, error) {
	data, err := e.loadCatalog(ctx, namespace)
	if err != nil {
		return false, err
	}
	if settings, ok := data.Tools[name]; ok && settings.Enabled != nil {
		return *settings.Enabled, nil
	}
	return true, nil
}

// renderPayload ports ToolRegistry.render: the payload is deep-copied, image
// assets become MCP ImageContent blocks and are replaced by a content_block
// reference, and everything else stays in the JSON text block.
func renderPayload(payload map[string]any) *sdk.CallToolResult {
	public := deepCopyJSONObject(payload)
	var images []sdk.Content
	if results, ok := public["results"].([]any); ok {
		for _, raw := range results {
			result, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			asset, ok := result["asset"].(map[string]any)
			if !ok || asset["type"] != "image" {
				continue
			}
			data, ok := asset["data"].(string)
			if !ok {
				continue
			}
			mimeType, _ := asset["mime_type"].(string)
			delete(asset, "data")
			asset["content_block"] = len(images) + 1
			decoded, decodeErr := base64.StdEncoding.DecodeString(data)
			if decodeErr != nil {
				// Keep the wire payload rather than dropping the image; Python
				// would forward whatever base64 text the runtime produced.
				decoded = []byte(data)
			}
			images = append(images, &sdk.ImageContent{Data: decoded, MIMEType: mimeType})
		}
	}
	content := make([]sdk.Content, 0, len(images)+1)
	content = append(content, &sdk.TextContent{Text: encodeJSON(public)})
	content = append(content, images...)
	return &sdk.CallToolResult{Content: content}
}

// toolError renders one application error as an MCP tool error, never as a
// protocol-level error, so the agent can read and recover from the message.
func toolError(message string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Content: []sdk.Content{&sdk.TextContent{Text: message}},
		IsError: true,
	}
}

// decodeArguments decodes raw tools/call arguments. json.Number is preserved so
// contracts integer checks keep Python's int/float distinction.
func decodeArguments(raw json.RawMessage) (map[string]any, error) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return map[string]any{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var arguments map[string]any
	if err := decoder.Decode(&arguments); err != nil {
		return nil, fmt.Errorf("Invalid tool arguments")
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return arguments, nil
}

// deepCopyJSONObject mimics json.loads(json.dumps(payload)) so rendering never
// mutates the runtime's payload.
func deepCopyJSONObject(payload map[string]any) map[string]any {
	encoded, err := json.Marshal(payload)
	if err != nil {
		return map[string]any{}
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.UseNumber()
	var copied map[string]any
	if err := decoder.Decode(&copied); err != nil || copied == nil {
		return map[string]any{}
	}
	return copied
}

// encodeJSON serializes a payload the way json.dumps(ensure_ascii=False) would
// keep it: UTF-8 preserved and no HTML escaping. Key order is not contractual.
func encodeJSON(payload map[string]any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(payload); err != nil {
		return "{}"
	}
	return strings.TrimRight(buffer.String(), "\n")
}

// documentRequest is the validated input of add/update/delete_document.
type documentRequest struct {
	name           string
	content        []byte
	idempotencyKey string
}

// documentStatusRequest is the validated input of get_document_status.
type documentStatusRequest struct {
	artifactID string
}

// parseDocumentArguments ports DocumentRequest.from_arguments.
func parseDocumentArguments(arguments map[string]any, writing bool) (documentRequest, error) {
	allowed := map[string]struct{}{"name": {}}
	if writing {
		allowed["content"] = struct{}{}
		allowed["content_base64"] = struct{}{}
		allowed["idempotency_key"] = struct{}{}
	}
	for key := range arguments {
		if _, ok := allowed[key]; !ok {
			return documentRequest{}, fmt.Errorf("Unknown document arguments")
		}
	}

	idempotencyKey := ""
	if raw, exists := arguments["idempotency_key"]; exists && raw != nil {
		value, ok := raw.(string)
		if !ok || len(value) < 1 || len(value) > 128 {
			return documentRequest{}, fmt.Errorf("idempotency_key must be a string of 1 to 128 characters")
		}
		idempotencyKey = value
	}

	rawName, _ := arguments["name"]
	name, ok := rawName.(string)
	if !ok || sanitizeDocumentName(name) != name || windowsReservedPath(name) {
		return documentRequest{}, fmt.Errorf("name must be an exact filename, not a path")
	}
	if !writing {
		return documentRequest{name: name}, nil
	}

	suffix := strings.ToLower(filepath.Ext(name))
	if _, ok := supportedExtensions[suffix]; !ok {
		return documentRequest{}, fmt.Errorf("Supported files: .txt, .md, .pdf, .docx, .doc")
	}

	_, hasContent := arguments["content"]
	_, hasBase64 := arguments["content_base64"]
	if hasContent == hasBase64 {
		return documentRequest{}, fmt.Errorf("Provide exactly one of content or content_base64")
	}

	var content []byte
	if hasContent {
		text, isText := arguments["content"].(string)
		if !isText || utf8.RuneCountInString(text) > MaxFileBytes {
			return documentRequest{}, fmt.Errorf("content must be text no larger than 10 MiB")
		}
		if suffix != ".txt" && suffix != ".md" {
			return documentRequest{}, fmt.Errorf("PDF, DOCX and DOC require content_base64")
		}
		if !utf8.ValidString(text) {
			return documentRequest{}, fmt.Errorf("content must be valid UTF-8 text")
		}
		content = []byte(text)
	} else {
		encoded, isString := arguments["content_base64"].(string)
		if !isString || len(encoded) > 4*((MaxFileBytes+2)/3) {
			return documentRequest{}, fmt.Errorf("content_base64 must encode a file no larger than 10 MiB")
		}
		decoded, decodeErr := base64.StdEncoding.DecodeString(encoded)
		if decodeErr != nil {
			return documentRequest{}, fmt.Errorf("content_base64 is not valid Base64")
		}
		content = decoded
	}
	if len(content) == 0 || len(content) > MaxFileBytes {
		return documentRequest{}, fmt.Errorf("File must be non-empty and no larger than 10 MiB")
	}
	return documentRequest{name: name, content: content, idempotencyKey: idempotencyKey}, nil
}

// parseDocumentStatusArguments ports DocumentStatusRequest.from_arguments.
func parseDocumentStatusArguments(arguments map[string]any) (documentStatusRequest, error) {
	if len(arguments) != 1 {
		return documentStatusRequest{}, fmt.Errorf("Provide only artifact_id")
	}
	raw, ok := arguments["artifact_id"]
	if !ok {
		return documentStatusRequest{}, fmt.Errorf("Provide only artifact_id")
	}
	artifactID, err := contracts.ValidateUUID(raw)
	if err != nil {
		return documentStatusRequest{}, fmt.Errorf("artifact_id must be a canonical UUID")
	}
	return documentStatusRequest{artifactID: artifactID}, nil
}

// sanitizeDocumentName ports KnowledgeDocumentStore.sanitize_name. A name is
// exact only when sanitizing it is a no-op.
func sanitizeDocumentName(rawName string) string {
	var builder strings.Builder
	builder.Grow(len(rawName))
	for _, character := range rawName {
		switch {
		case character == '<' || character == '>' || character == ':' || character == '"',
			character == '/' || character == '\\' || character == '|' || character == '?',
			character == '*' || character < 0x20:
			builder.WriteByte('_')
		default:
			builder.WriteRune(character)
		}
	}
	name := strings.Trim(builder.String(), " .")
	if runes := []rune(name); len(runes) > 200 {
		name = string(runes[:200])
	}
	return name
}

// windowsReservedPath ports PureWindowsPath(name).is_reserved().
func windowsReservedPath(name string) bool {
	// Names starting with '.' or ' ' are never reserved.
	if name == "" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, " ") {
		return false
	}
	part := name
	if index := strings.IndexByte(part, ':'); index >= 0 {
		part = part[:index]
	}
	if index := strings.IndexByte(part, '.'); index >= 0 {
		part = part[:index]
	}
	_, reserved := windowsReservedNames[strings.ToUpper(part)]
	return reserved
}
