package mcp

import (
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
)

// Compile-time proof that the MCP facade satisfies the HTTP API layer's
// consumer interfaces without internal/mcp importing internal/httpapi in
// production code.
var (
	_ httpapi.ToolRegistry   = (*Endpoint)(nil)
	_ httpapi.ChangeNotifier = (*Endpoint)(nil)
)
