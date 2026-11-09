// Package version holds build identity shared by every Go entry point.
package version

// Version is the Praxis-RAG backend version. It replaces rag_mcp.__version__.
const Version = "0.4.0"

// ServiceName is the public service name reported by /health and MCP.
const ServiceName = "praxis-rag"
