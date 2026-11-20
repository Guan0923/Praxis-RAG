package mcp

import (
	"context"
	"fmt"
	"github.com/Guan0923/Praxis-RAG/internal/audit"
	"log/slog"
	"net/http"
	"regexp"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/jsonrpc"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/Guan0923/Praxis-RAG/internal/catalog"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/version"
)

// namespacePattern is the shared namespace identifier format (catalog.NAME_PATTERN).
var namespacePattern = regexp.MustCompile(catalog.NamePattern)

// namespaceTransport owns the lazily built SDK server plus the dual-era HTTP
// transports of exactly one namespace. Sessions can never cross namespaces
// because every namespace has its own stateful handler.
type namespaceTransport struct {
	namespace     string
	server        *sdk.Server
	stateful      *sdk.StreamableHTTPHandler
	stateless     *sdk.StreamableHTTPHandler
	promptNames   map[string]struct{}
	resourceNames map[string]struct{}
}

// Endpoint is the per-service MCP facade: it lazily creates one SDK server and
// one pair of Streamable HTTP transports per namespace, serves the catalog
// administration endpoints, and publishes list-changed notifications.
//
// It implements the internal/httpapi.ToolRegistry and
// internal/httpapi.ChangeNotifier interfaces.
type Endpoint struct {
	opts   Options
	logger *slog.Logger
	// entries is the static ordered tool registry (catalog overrides are
	// applied when listing and when checking execution permission).
	entries []toolEntry

	mu         sync.Mutex
	transports map[string]*namespaceTransport
}

// NewEndpoint builds the MCP endpoint facade. Runtime and Catalog are required.
func NewEndpoint(opts Options) (*Endpoint, error) {
	if opts.Runtime == nil {
		return nil, fmt.Errorf("mcp: Runtime is required")
	}
	if opts.Catalog == nil {
		return nil, fmt.Errorf("mcp: catalog.Store is required")
	}
	if opts.ServerName == "" {
		opts.ServerName = "Praxis-RAG"
	}
	if opts.Version == "" {
		opts.Version = version.Version
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	endpoint := &Endpoint{
		opts:       opts,
		logger:     logger,
		transports: map[string]*namespaceTransport{},
	}
	endpoint.entries = endpoint.buildToolEntries()
	return endpoint, nil
}

// Runtime exposes the configured runtime (tests and wiring helpers).
func (e *Endpoint) Runtime() Runtime { return e.opts.Runtime }

// ServerFor returns the per-namespace SDK server, building it lazily. The same
// server instance is reused for every session of that namespace so catalog
// change notifications reach all of them.
func (e *Endpoint) ServerFor(namespace string) (*sdk.Server, error) {
	transport, err := e.transportFor(namespace)
	if err != nil {
		return nil, err
	}
	return transport.server, nil
}

// transportFor returns (creating on first use) the namespace transport pair.
func (e *Endpoint) transportFor(namespace string) (*namespaceTransport, error) {
	if !namespacePattern.MatchString(namespace) {
		return nil, fmt.Errorf("Invalid namespace")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if transport, ok := e.transports[namespace]; ok {
		return transport, nil
	}
	transport, err := e.newNamespaceTransport(namespace)
	if err != nil {
		return nil, err
	}
	e.transports[namespace] = transport
	if err := e.syncPrompts(context.Background(), transport); err != nil {
		e.logger.Warn("mcp: cannot load prompts", append([]any{"namespace", namespace}, logging.ErrorAttrs(err)...)...)
	}
	return transport, nil
}

// existingTransport returns the namespace transport without creating it.
func (e *Endpoint) existingTransport(namespace string) (*namespaceTransport, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	transport, ok := e.transports[namespace]
	return transport, ok
}

// newNamespaceTransport builds the SDK server and both HTTP transports.
func (e *Endpoint) newNamespaceTransport(namespace string) (*namespaceTransport, error) {
	server := sdk.NewServer(&sdk.Implementation{
		Name:    e.opts.ServerName,
		Version: e.opts.Version,
	}, &sdk.ServerOptions{
		Logger: e.logger,
		Capabilities: &sdk.ServerCapabilities{
			Tools:   &sdk.ToolCapabilities{ListChanged: true},
			Prompts: &sdk.PromptCapabilities{ListChanged: true},
			Resources: &sdk.ResourceCapabilities{
				ListChanged: true,
				Subscribe:   true,
			},
		},
		SubscribeHandler: e.subscribeHandler(namespace),
		// Python's unsubscribe validates nothing; it only drops bookkeeping.
		UnsubscribeHandler: func(context.Context, *sdk.UnsubscribeRequest) error { return nil },
	})

	handlers := make(map[string]sdk.ToolHandler, len(e.entries))
	for _, entry := range e.entries {
		handler := e.toolHandler(namespace, entry)
		handlers[entry.name] = handler
		server.AddTool(entry.definition, handler)
	}
	server.AddReceivingMiddleware(e.receivingMiddleware(namespace, handlers))

	transport := &namespaceTransport{
		namespace:     namespace,
		server:        server,
		promptNames:   map[string]struct{}{},
		resourceNames: map[string]struct{}{},
	}
	transport.stateful = sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{
			Logger:              e.logger,
			MaxRequestBodyBytes: maxRequestBodyBytes,
			// Search runs synchronously inside the POST, so a client disconnect
			// (or its request deadline) must cancel retrieval instead of
			// leaving Qdrant/model work running for a gone caller.
			PropagateRequestCancellation: true,
			// The Python service enforces its own Host/Origin allowlist in
			// mcp_http.py; internal/mcp's HTTP handler ports that allowlist, so
			// the SDK's localhost-only heuristic must not preempt it.
			DisableLocalhostProtection: true,
		})
	transport.stateless = sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return server },
		&sdk.StreamableHTTPOptions{
			Stateless:                    true,
			Logger:                       e.logger,
			MaxRequestBodyBytes:          maxRequestBodyBytes,
			PropagateRequestCancellation: true,
			DisableLocalhostProtection:   true,
		})
	return transport, nil
}

// toolHandler wraps one registry entry as an SDK tool handler. Validation and
// runtime failures are surfaced as tool errors, never protocol errors.
func (e *Endpoint) toolHandler(namespace string, entry toolEntry) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var extra *sdk.RequestExtra
		if req != nil {
			extra = req.Extra
		}
		ctx = withRequestScope(ctx, extra)
		var raw []byte
		if req != nil && req.Params != nil {
			raw = req.Params.Arguments
		}
		arguments, err := decodeArguments(raw)
		if err != nil {
			return toolError(err.Error()), nil
		}
		enabled, err := e.toolEnabled(ctx, namespace, entry.name)
		if err != nil {
			return toolError(err.Error()), nil
		}
		if !enabled {
			return toolError(fmt.Sprintf("Unknown or disabled RAG tool: %s", entry.name)), nil
		}
		validated, err := entry.validate(arguments)
		if err != nil {
			return toolError(err.Error()), nil
		}
		// Document tools carry their own per-operation permission checks in
		// tools.go (create/update/delete differ); the shared check here only
		// covers read-oriented payloads.
		payload, err := entry.execute(ctx, namespace, validated)
		if err != nil {
			return toolError(err.Error()), nil
		}
		switch input := validated.(type) {
		case documentRequest:
			outcome := "success"
			if entry.name == addDocumentToolName {
				outcome = "accepted"
			}
			audit.Record(ctx, e.opts.Audit, e.logger, "MCP", entry.name, namespace, audit.Target{Name: input.name, Outcome: outcome})
		case documentStatusRequest:
			audit.Record(ctx, e.opts.Audit, e.logger, "MCP", "read_job", namespace, audit.Target{Name: input.artifactID, Outcome: "success"})
		default:
			if entry.name == searchToolName {
				audit.Record(ctx, e.opts.Audit, e.logger, "MCP", "search", namespace, audit.SearchTargets(payload)...)
			}
		}
		return renderPayload(payload), nil
	}
}

// receivingMiddleware serves tools/list and prompts/list itself so pagination
// and ordering match the Python implementation exactly (the SDK lists its
// registrations sorted by name and paginates with opaque gob cursors), and
// turns calls to unregistered or disabled tools into MCP tool errors.
func (e *Endpoint) receivingMiddleware(namespace string, handlers map[string]sdk.ToolHandler) sdk.Middleware {
	return func(next sdk.MethodHandler) sdk.MethodHandler {
		return func(ctx context.Context, method string, req sdk.Request) (sdk.Result, error) {
			// Resolve this message's visibility once so the resource handlers
			// below never inherit a stale session context.
			if req != nil {
				ctx = withRequestScope(ctx, req.GetExtra())
			}
			switch method {
			case methodToolsList:
				params, _ := req.GetParams().(*sdk.ListToolsParams)
				return e.listTools(ctx, namespace, params)
			case methodPromptsList:
				params, _ := req.GetParams().(*sdk.ListPromptsParams)
				return e.listPrompts(ctx, namespace, params)
			case methodResourcesList:
				params, _ := req.GetParams().(*sdk.ListResourcesParams)
				return e.listResources(ctx, namespace, params)
			case methodResourceTemplates:
				params, _ := req.GetParams().(*sdk.ListResourceTemplatesParams)
				return e.listResourceTemplates(ctx, namespace, params)
			case methodResourcesRead:
				params, ok := req.GetParams().(*sdk.ReadResourceParams)
				if !ok || params == nil {
					return nil, invalidParams("Invalid resource URI")
				}
				return e.readResource(ctx, namespace, params)
			case methodToolsCall:
				params, ok := req.GetParams().(*sdk.CallToolParamsRaw)
				if !ok || params == nil {
					return nil, invalidParams("Invalid tool call parameters")
				}
				if _, known := handlers[params.Name]; !known {
					return toolError(fmt.Sprintf("Unknown or disabled RAG tool: %s", params.Name)), nil
				}
				enabled, err := e.toolEnabled(ctx, namespace, params.Name)
				if err != nil {
					return toolError(err.Error()), nil
				}
				if !enabled {
					return toolError(fmt.Sprintf("Unknown or disabled RAG tool: %s", params.Name)), nil
				}
				return next(ctx, method, req)
			}
			return next(ctx, method, req)
		}
	}
}

// listTools ports ToolRegistry.definitions: disabled tools are hidden and
// persisted description overrides win. Python does not paginate tools/list.
func (e *Endpoint) listTools(ctx context.Context, namespace string, _ *sdk.ListToolsParams) (*sdk.ListToolsResult, error) {
	data, err := e.loadCatalog(ctx, namespace)
	if err != nil {
		return nil, invalidParams(err.Error())
	}
	tools := make([]*sdk.Tool, 0, len(e.entries))
	for _, entry := range e.entries {
		description := entry.definition.Description
		enabled := true
		if settings, ok := data.Tools[entry.name]; ok {
			if settings.Enabled != nil {
				enabled = *settings.Enabled
			}
			if settings.Description != nil {
				description = *settings.Description
			}
		}
		if !enabled {
			continue
		}
		definition := *entry.definition
		definition.Description = description
		tools = append(tools, &definition)
	}
	return &sdk.ListToolsResult{Tools: tools}, nil
}

// syncPrompts registers the namespace's stored prompts on the SDK server.
func (e *Endpoint) syncPrompts(ctx context.Context, transport *namespaceTransport) error {
	data, err := e.loadCatalog(ctx, transport.namespace)
	if err != nil {
		return err
	}
	stale := make([]string, 0)
	for name := range transport.promptNames {
		if _, ok := data.Prompts[name]; !ok {
			stale = append(stale, name)
		}
	}
	if len(stale) > 0 {
		transport.server.RemovePrompts(stale...)
	}
	handler := e.promptHandler(transport.namespace)
	names := make(map[string]struct{}, len(data.Prompts))
	for name, prompt := range data.Prompts {
		transport.server.AddPrompt(sdkPrompt(name, prompt), handler)
		names[name] = struct{}{}
	}
	transport.promptNames = names
	return nil
}

// refreshTools re-registers every tool with the current catalog description.
// SDK AddTool always schedules a notifications/tools/list_changed, so callers
// must invoke this only after an actual catalog change.
func (e *Endpoint) refreshTools(namespace string) error {
	transport, ok := e.existingTransport(namespace)
	if !ok {
		return nil
	}
	data, err := e.loadCatalog(context.Background(), namespace)
	if err != nil {
		return err
	}
	for _, entry := range e.entries {
		definition := *entry.definition
		if settings, ok := data.Tools[entry.name]; ok && settings.Description != nil {
			definition.Description = *settings.Description
		}
		transport.server.AddTool(&definition, e.toolHandler(namespace, entry))
	}
	return nil
}

// ToolsChanged implements internal/httpapi.ChangeNotifier.
func (e *Endpoint) ToolsChanged(namespace string) {
	if err := e.refreshTools(namespace); err != nil {
		e.logger.Warn("mcp: cannot refresh tools", append([]any{"namespace", namespace}, logging.ErrorAttrs(err)...)...)
	}
}

// PromptsChanged implements internal/httpapi.ChangeNotifier.
func (e *Endpoint) PromptsChanged(namespace string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	transport, ok := e.transports[namespace]
	if !ok {
		return
	}
	if err := e.syncPrompts(context.Background(), transport); err != nil {
		e.logger.Warn("mcp: cannot refresh prompts", append([]any{"namespace", namespace}, logging.ErrorAttrs(err)...)...)
	}
}

// Configured implements internal/httpapi.ToolRegistry: it returns the
// admin-visible tool list with persisted overrides applied.
func (e *Endpoint) Configured(namespace string) ([]map[string]any, error) {
	data, err := e.loadCatalog(context.Background(), namespace)
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(e.entries))
	for _, entry := range e.entries {
		description := entry.definition.Description
		enabled := true
		if settings, ok := data.Tools[entry.name]; ok {
			if settings.Description != nil {
				description = *settings.Description
			}
			if settings.Enabled != nil {
				enabled = *settings.Enabled
			}
		}
		items = append(items, map[string]any{
			"name":        entry.name,
			"enabled":     enabled,
			"description": description,
		})
	}
	return items, nil
}

// Defaults implements internal/httpapi.ToolRegistry: the built-in settings of
// one registered tool.
func (e *Endpoint) Defaults(_ string, name string) (catalog.ToolSettings, bool) {
	for _, entry := range e.entries {
		if entry.name != name {
			continue
		}
		enabled := true
		description := entry.definition.Description
		return catalog.ToolSettings{Enabled: &enabled, Description: &description}, true
	}
	return catalog.ToolSettings{}, false
}

// invalidParams builds an MCP protocol error for malformed prompt/resource
// requests, matching the Python observe middleware that maps ValueError,
// KeyError and KnowledgeError to INVALID_PARAMS.
func invalidParams(message string) error {
	return &jsonrpc.Error{Code: jsonrpc.CodeInvalidParams, Message: message}
}

// SDK method names intercepted by the receiving middleware.
const (
	methodToolsList   = "tools/list"
	methodToolsCall   = "tools/call"
	methodPromptsList = "prompts/list"
)
