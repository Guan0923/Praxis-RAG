package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// LoopbackHosts are the host names allowed to serve without a service token.
var LoopbackHosts = map[string]struct{}{
	"127.0.0.1": {},
	"localhost": {},
	"::1":       {},
}

// DefaultMCPAllowedHosts mirrors RagHttpSettings defaults for local MCP clients.
var DefaultMCPAllowedHosts = []string{
	"127.0.0.1",
	"127.0.0.1:*",
	"localhost",
	"localhost:*",
	"[::1]",
	"[::1]:*",
}

// DefaultMCPAllowedOrigins mirrors RagHttpSettings defaults for local MCP clients.
var DefaultMCPAllowedOrigins = []string{
	"http://127.0.0.1",
	"http://127.0.0.1:*",
	"http://localhost",
	"http://localhost:*",
	"http://[::1]",
	"http://[::1]:*",
}

// ServiceSettings configures the standalone HTTP service entry point.
type ServiceSettings struct {
	Host              string
	Port              int
	DataRoot          string
	Token             string
	MCPAllowedHosts   []string
	MCPAllowedOrigins []string
	// WebRoot is the directory containing the built Console (index.html and
	// assets). An empty value means "resolve a default location".
	WebRoot string
}

// ServiceSettingsFromEnv loads the HTTP service settings, enforcing the same
// loopback/token rules as rag_mcp.http_service.RagHttpSettings.
func ServiceSettingsFromEnv() (ServiceSettings, error) {
	rawPort := strings.TrimSpace(os.Getenv("RAG_SERVICE_PORT"))
	if rawPort == "" {
		rawPort = "8011"
	}
	port, err := strconv.Atoi(rawPort)
	if err != nil {
		return ServiceSettings{}, fmt.Errorf("RAG_SERVICE_PORT must be an integer")
	}
	if port < 1 || port > 65535 {
		return ServiceSettings{}, fmt.Errorf("RAG_SERVICE_PORT must be between 1 and 65535")
	}

	token := strings.TrimSpace(os.Getenv("RAG_SERVICE_TOKEN"))
	host := strings.TrimSpace(os.Getenv("RAG_SERVICE_HOST"))
	if host == "" {
		host = "127.0.0.1"
	}

	rawHosts, hostsSet := os.LookupEnv("RAG_MCP_ALLOWED_HOSTS")
	rawOrigins, originsSet := os.LookupEnv("RAG_MCP_ALLOWED_ORIGINS")
	if _, loopback := LoopbackHosts[host]; !loopback {
		if token == "" {
			return ServiceSettings{}, fmt.Errorf("RAG_SERVICE_TOKEN is required when RAG_SERVICE_HOST is not loopback")
		}
		if !hostsSet || strings.TrimSpace(rawHosts) == "" {
			return ServiceSettings{}, fmt.Errorf("RAG_MCP_ALLOWED_HOSTS is required for a non-loopback RAG service")
		}
		if !originsSet || strings.TrimSpace(rawOrigins) == "" {
			return ServiceSettings{}, fmt.Errorf("RAG_MCP_ALLOWED_ORIGINS is required for a non-loopback RAG service")
		}
	}

	allowedHosts, err := csvSetting(rawHosts, hostsSet, DefaultMCPAllowedHosts)
	if err != nil {
		return ServiceSettings{}, err
	}
	allowedOrigins, err := csvSetting(rawOrigins, originsSet, DefaultMCPAllowedOrigins)
	if err != nil {
		return ServiceSettings{}, err
	}

	dataRoot := strings.TrimSpace(os.Getenv("RAG_SERVICE_DATA_ROOT"))
	if dataRoot == "" {
		home, homeErr := os.UserHomeDir()
		if homeErr != nil {
			return ServiceSettings{}, fmt.Errorf("Cannot resolve the home directory for RAG_SERVICE_DATA_ROOT")
		}
		dataRoot = filepath.Join(home, ".praxis-rag", "data")
	}
	dataRoot = ExpandPath(dataRoot)

	return ServiceSettings{
		Host:              host,
		Port:              port,
		DataRoot:          dataRoot,
		Token:             emptyToZero(token),
		MCPAllowedHosts:   allowedHosts,
		MCPAllowedOrigins: allowedOrigins,
		WebRoot:           strings.TrimSpace(os.Getenv("RAG_WEB_ROOT")),
	}, nil
}

func csvSetting(raw string, set bool, defaults []string) ([]string, error) {
	if !set {
		return append([]string(nil), defaults...), nil
	}
	values := make([]string, 0, 4)
	for _, item := range strings.Split(raw, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			values = append(values, trimmed)
		}
	}
	if len(values) == 0 {
		return nil, fmt.Errorf("MCP host and origin allowlists must not be empty")
	}
	return values, nil
}

func emptyToZero(value string) string { return value }
