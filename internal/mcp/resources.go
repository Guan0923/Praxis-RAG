package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/contracts"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/resources"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
)

// Resource behavior ported from rag_mcp/mcp_http.py (NamespaceEndpoint):
// resources/list, resources/templates/list and resources/read, artifact URIs,
// and resource subscriptions/notifications.
const (
	methodResourcesList     = "resources/list"
	methodResourceTemplates = "resources/templates/list"
	methodResourcesRead     = "resources/read"

	// resourceDescription mirrors NamespaceEndpoint.list_resources.
	resourceDescription = "Paragraph-paged document text; follow next_uri to continue."
	// defaultArtifactLength mirrors the length default of _parse_artifact_uri.
	defaultArtifactLength = 65536
)

// listResources ports NamespaceEndpoint.list_resources.
func (e *Endpoint) listResources(ctx context.Context, namespace string, params *sdk.ListResourcesParams) (*sdk.ListResourcesResult, error) {
	documents, err := e.opts.Runtime.ListDocuments(ctx, namespace)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	scope, hasScope := scopeFor(ctx)
	available := make([]*sdk.Resource, 0, len(documents))
	for _, document := range documents {
		name := document.Name()
		if name == "" {
			continue
		}
		// A file-level grant exposes only the granted documents; management
		// credentials (no scope) keep the historical full list.
		if hasScope && !scope.CanReadDocument(name) {
			continue
		}
		available = append(available, &sdk.Resource{
			URI:         resources.DocumentURI(namespace, name),
			Name:        name,
			MIMEType:    "application/json",
			Description: resourceDescription,
		})
	}
	cursor := ""
	if params != nil {
		cursor = params.Cursor
	}
	start, end, next, err := paginate(len(available), cursor)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	return &sdk.ListResourcesResult{Resources: available[start:end], NextCursor: next}, nil
}

// listResourceTemplates ports NamespaceEndpoint.list_templates.
func (e *Endpoint) listResourceTemplates(_ context.Context, namespace string, _ *sdk.ListResourceTemplatesParams) (*sdk.ListResourceTemplatesResult, error) {
	templates := []*sdk.ResourceTemplate{
		{
			URITemplate: fmt.Sprintf("rag://knowledge/%s/{name}{?page,version}", namespace),
			Name:        "Document paragraph pages",
			MIMEType:    "application/json",
		},
		{
			URITemplate: "rag://artifacts/{namespace}/{artifact_id}{?offset,length}",
			Name:        "Artifact byte chunks",
			Description: "Read an expiring artifact UUID as a bounded JSON chunk envelope; follow next_uri.",
			MIMEType:    "application/json",
		},
	}
	return &sdk.ListResourceTemplatesResult{ResourceTemplates: templates}, nil
}

// readResource ports NamespaceEndpoint.read_resource: artifact URIs return a
// bounded byte-chunk envelope, document URIs return one paragraph page.
// Application failures are INVALID_PARAMS protocol errors, matching the Python
// observe middleware.
func (e *Endpoint) readResource(ctx context.Context, namespace string, params *sdk.ReadResourceParams) (*sdk.ReadResourceResult, error) {
	if params == nil {
		return nil, invalidParams("Invalid resource URI")
	}
	uri := params.URI
	scope, hasScope := scopeFor(ctx)
	if parsed, err := url.Parse(uri); err == nil && parsed.Host == "artifacts" {
		artifactID, offset, length, parseErr := parseArtifactURI(namespace, uri)
		if parseErr != nil {
			return nil, invalidParams(parseErr.Error())
		}
		if hasScope && scope.Restricted() {
			// REST and MCP enforce the same document-level artifact visibility:
			// the runtime resolves the artifact's parent document and applies
			// the caller's file-level grants in one transaction.
			ctx = runtime.WithDocumentReadGate(ctx, scope.CanReadDocument)
		}
		payload, runtimeErr := e.opts.Runtime.GetArtifact(ctx, namespace, artifactID, offset, length)
		if runtimeErr != nil {
			return nil, invalidParams(runtimeErr.Error())
		}
		// Copy before decorating so the runtime's payload is never mutated.
		public := make(map[string]any, len(payload)+1)
		for key, value := range payload {
			public[key] = value
		}
		public["next_uri"] = nil
		if rawNext, ok := payload["next_offset"]; ok && rawNext != nil {
			nextOffset, canonical := artifactOffsetString(rawNext)
			if !canonical {
				nextOffset = fmt.Sprint(rawNext)
			}
			public["next_uri"] = artifactURI(namespace, artifactID) +
				"?offset=" + nextOffset + "&length=" + strconv.Itoa(length)
		}
		return jsonResourceResult(uri, public), nil
	}

	name, page, expected, err := resources.ParseURI(namespace, uri)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	// The document resource is part of the namespace's readable surface: an
	// ungranted filename is answered like the REST document routes.
	if hasScope && !scope.CanReadDocument(name) {
		return nil, invalidParams(access.MessageKnowledgeAccess)
	}
	content, err := e.opts.Runtime.ReadDocument(ctx, namespace, name)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	payload, err := resources.ResourcePage(namespace, name, content.Text, content.Version, page, expected)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	return jsonResourceResult(uri, payload), nil
}

// resourceReadHandler serves the SDK registrations used to publish
// resources/list_changed. Reads normally flow through readResource in the
// receiving middleware.
func (e *Endpoint) resourceReadHandler(namespace string) sdk.ResourceHandler {
	return func(ctx context.Context, request *sdk.ReadResourceRequest) (*sdk.ReadResourceResult, error) {
		if request == nil {
			return nil, invalidParams("Invalid resource URI")
		}
		ctx = withRequestScope(ctx, request.GetExtra())
		return e.readResource(ctx, namespace, request.Params)
	}
}

func jsonResourceResult(uri string, payload map[string]any) *sdk.ReadResourceResult {
	return &sdk.ReadResourceResult{Contents: []*sdk.ResourceContents{{
		URI:      uri,
		MIMEType: "application/json",
		Text:     encodeJSON(payload),
	}}}
}

// ---- Artifact URIs -------------------------------------------------------

// artifactURI mirrors mcp_http._artifact_uri.
func artifactURI(namespace, artifactID string) string {
	return "rag://artifacts/" + percentQuote(namespace) + "/" + artifactID
}

// percentQuote mirrors Python urllib.parse.quote(value, safe="").
func percentQuote(value string) string {
	const hex = "0123456789ABCDEF"
	var builder strings.Builder
	for index := 0; index < len(value); index++ {
		character := value[index]
		if (character >= 'A' && character <= 'Z') ||
			(character >= 'a' && character <= 'z') ||
			(character >= '0' && character <= '9') ||
			character == '-' || character == '_' || character == '.' || character == '~' {
			builder.WriteByte(character)
			continue
		}
		builder.WriteByte('%')
		builder.WriteByte(hex[character>>4])
		builder.WriteByte(hex[character&0x0f])
	}
	return builder.String()
}

var (
	errInvalidArtifactURI       = fmt.Errorf("Invalid artifact URI")
	errArtifactOutsideNamespace = fmt.Errorf("Artifact URI is outside this namespace or non-canonical")
	errInvalidArtifactChunk     = fmt.Errorf("Invalid artifact chunk parameters")
	errNonCanonicalArtifact     = fmt.Errorf("Artifact chunk parameters must be canonical integers")
)

// parseArtifactURI ports mcp_http._parse_artifact_uri.
func parseArtifactURI(namespace, uri string) (string, int, int, error) {
	for _, character := range uri {
		if character <= 32 || character == 127 || character == '#' {
			return "", 0, 0, errInvalidArtifactURI
		}
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return "", 0, 0, errInvalidArtifactURI
	}
	escapedPath := parsed.EscapedPath()
	parts := strings.Split(escapedPath, "/")
	if parsed.Scheme != "rag" || parsed.Host != "artifacts" || parsed.Fragment != "" || len(parts) != 3 {
		return "", 0, 0, errInvalidArtifactURI
	}
	uriNamespace, err := url.PathUnescape(parts[1])
	if err != nil {
		return "", 0, 0, errInvalidArtifactURI
	}
	rawID, err := url.PathUnescape(parts[2])
	if err != nil {
		return "", 0, 0, errInvalidArtifactURI
	}
	artifactID, err := contracts.ValidateUUID(rawID)
	if err != nil {
		return "", 0, 0, err
	}
	if uriNamespace != namespace || "rag://artifacts"+escapedPath != artifactURI(namespace, artifactID) {
		return "", 0, 0, errArtifactOutsideNamespace
	}

	// parse_qs(..., strict_parsing=True, keep_blank_values=True): every field
	// needs '=', keys are decoded, duplicates are rejected by the len==1 check.
	if parsed.ForceQuery && parsed.RawQuery == "" {
		return "", 0, 0, errInvalidArtifactChunk
	}
	query, order, err := parseStrictQueryPairs(parsed.RawQuery)
	if err != nil {
		return "", 0, 0, errInvalidArtifactChunk
	}
	for key := range query {
		if key != "offset" && key != "length" {
			return "", 0, 0, errInvalidArtifactChunk
		}
	}
	rejoined := make([]string, 0, len(order))
	for _, key := range order {
		rejoined = append(rejoined, key+"="+query[key])
	}
	if strings.Join(rejoined, "&") != parsed.RawQuery {
		return "", 0, 0, errInvalidArtifactChunk
	}

	offset := 0
	length := defaultArtifactLength
	for _, key := range []string{"offset", "length"} {
		raw, found := query[key]
		if !found {
			continue
		}
		if raw == "" || len(raw) > 20 || !isASCIIDecimal(raw) {
			return "", 0, 0, errInvalidArtifactChunk
		}
		value, convErr := strconv.Atoi(raw)
		if convErr != nil {
			return "", 0, 0, errInvalidArtifactChunk
		}
		if raw != strconv.Itoa(value) {
			return "", 0, 0, errNonCanonicalArtifact
		}
		if key == "offset" {
			offset = value
		} else {
			length = value
		}
	}
	if err := contracts.ValidateArtifactRange(offset, length); err != nil {
		return "", 0, 0, err
	}
	return artifactID, offset, length, nil
}

// parseStrictQueryPairs decodes a raw query preserving field order so the
// caller can reject non-canonical spellings exactly like parse_qs + rejoin.
func parseStrictQueryPairs(rawQuery string) (map[string]string, []string, error) {
	values := make(map[string]string)
	if rawQuery == "" {
		return values, nil, nil
	}
	order := make([]string, 0, strings.Count(rawQuery, "&")+1)
	for _, field := range strings.Split(rawQuery, "&") {
		rawKey, rawValue, found := strings.Cut(field, "=")
		if !found {
			return nil, nil, errInvalidArtifactChunk
		}
		key, err := url.QueryUnescape(rawKey)
		if err != nil {
			return nil, nil, errInvalidArtifactChunk
		}
		value, err := url.QueryUnescape(rawValue)
		if err != nil {
			return nil, nil, errInvalidArtifactChunk
		}
		if _, duplicate := values[key]; duplicate {
			return nil, nil, errInvalidArtifactChunk
		}
		values[key] = value
		order = append(order, key)
	}
	return values, order, nil
}

func isASCIIDecimal(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if value[index] < '0' || value[index] > '9' {
			return false
		}
	}
	return true
}

// artifactOffsetString renders a runtime next_offset the way Python's
// urlencode(str(value)) would for the integer values the runtime returns.
func artifactOffsetString(value any) (string, bool) {
	switch typed := value.(type) {
	case int:
		return strconv.Itoa(typed), true
	case int64:
		return strconv.FormatInt(typed, 10), true
	case float64:
		if math.IsNaN(typed) || math.IsInf(typed, 0) || math.Trunc(typed) != typed {
			return "", false
		}
		return strconv.FormatInt(int64(typed), 10), true
	case json.Number:
		text := string(typed)
		if strings.ContainsAny(text, ".eE") {
			return "", false
		}
		if _, err := strconv.Atoi(text); err != nil {
			return "", false
		}
		return text, true
	default:
		return "", false
	}
}

// ---- Subscriptions and document change notifications ---------------------

// subscribeHandler ports NamespaceEndpoint.subscribe validation.
func (e *Endpoint) subscribeHandler(namespace string) func(context.Context, *sdk.SubscribeRequest) error {
	return func(ctx context.Context, request *sdk.SubscribeRequest) error {
		if request == nil || request.Params == nil {
			return invalidParams("Invalid resource URI")
		}
		ctx = withRequestScope(ctx, request.GetExtra())
		return e.validateSubscription(ctx, namespace, request.Params.URI)
	}
}

// validateSubscription ports NamespaceEndpoint.validate_subscription: only the
// canonical document URI of an existing, readable document may be subscribed.
func (e *Endpoint) validateSubscription(ctx context.Context, namespace, uri string) error {
	name, _, _, err := resources.ParseURI(namespace, uri)
	if err != nil {
		return invalidParams(err.Error())
	}
	if uri != resources.DocumentURI(namespace, name) {
		return invalidParams("Subscribe to the document URI, not a page URI")
	}
	if scope, hasScope := scopeFor(ctx); hasScope && !scope.CanReadDocument(name) {
		return invalidParams(access.MessageKnowledgeAccess)
	}
	documents, listErr := e.opts.Runtime.ListDocuments(ctx, namespace)
	if listErr != nil {
		return invalidParams(listErr.Error())
	}
	for _, document := range documents {
		if document.Name() == name {
			return nil
		}
	}
	return invalidParams("Document does not exist")
}

// DocumentsChanged publishes resources/list_changed and, per changed document,
// notifications/resources/updated. It is a port of
// McpHttpEndpoint.documents_changed and is called by the MCP document tools and
// (later) by the HTTP document upload/delete routes.
func (e *Endpoint) DocumentsChanged(namespace string, before, after []DocumentRecord) {
	e.mu.Lock()
	defer e.mu.Unlock()
	transport, ok := e.transports[namespace]
	if !ok {
		return
	}
	old := recordsByName(before)
	current := recordsByName(after)
	if reflect.DeepEqual(old, current) {
		return
	}
	e.syncResourcesLocked(transport, current)

	ctx := context.Background()
	for name, record := range old {
		if updated, exists := current[name]; exists && reflect.DeepEqual(record, updated) {
			continue
		}
		if err := transport.server.ResourceUpdated(ctx, &sdk.ResourceUpdatedNotificationParams{
			URI: resources.DocumentURI(namespace, name),
		}); err != nil {
			e.logger.Warn("mcp: cannot publish resource update", append([]any{"namespace", namespace, "uri", name}, logging.ErrorAttrs(err)...)...)
		}
	}
}

// syncResourcesLocked keeps the SDK resource registrations in sync with the
// document set; AddResource/RemoveResources are what emit the debounced
// notifications/resources/list_changed.
func (e *Endpoint) syncResourcesLocked(transport *namespaceTransport, documents map[string]DocumentRecord) {
	stale := make([]string, 0)
	for name := range transport.resourceNames {
		if _, exists := documents[name]; !exists {
			stale = append(stale, resources.DocumentURI(transport.namespace, name))
		}
	}
	if len(stale) > 0 {
		transport.server.RemoveResources(stale...)
	}
	handler := e.resourceReadHandler(transport.namespace)
	names := make(map[string]struct{}, len(documents))
	for name := range documents {
		transport.server.AddResource(&sdk.Resource{
			URI:         resources.DocumentURI(transport.namespace, name),
			Name:        name,
			MIMEType:    "application/json",
			Description: resourceDescription,
		}, handler)
		names[name] = struct{}{}
	}
	transport.resourceNames = names
}

// recordsByName mirrors the `{item["name"]: item for item in documents}` maps
// of mcp_http.documents_changed.
func recordsByName(records []DocumentRecord) map[string]DocumentRecord {
	result := make(map[string]DocumentRecord, len(records))
	for _, record := range records {
		name := record.Name()
		if name == "" {
			continue
		}
		result[name] = record
	}
	return result
}
