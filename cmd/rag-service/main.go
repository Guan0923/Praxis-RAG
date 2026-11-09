// Command rag-service starts the standalone Praxis-RAG HTTP service: REST API,
// MCP Streamable HTTP endpoint, and Console static assets.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/apikeys"
	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/catalog"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/mcp"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
	"github.com/Guan0923/xinghe_rag/internal/wire"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "praxis-rag: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	if _, err := config.LoadStartupConfig(); err != nil {
		return err
	}
	serviceSettings, err := config.ServiceSettingsFromEnv()
	if err != nil {
		return err
	}
	ragConfig, err := config.RagConfigFromEnv()
	if err != nil {
		return err
	}
	stateSettings, err := state.SettingsFromEnv(serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	stores, err := state.New(stateSettings, stateSettings.AutoSchema)
	if err != nil {
		return err
	}
	defer func() { _ = stores.Close() }()
	logger := newLogger()
	mqSettings, err := mq.SettingsFromEnv()
	if err != nil {
		return err
	}
	cache, err := redisstate.NewFromEnv()
	if err != nil {
		return err
	}
	if err := redisstate.Attach(stores, cache); err != nil {
		return err
	}
	stores.CacheSync.Start()
	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	service.SearchEngine().SetReferenceCapture(searchrefs.New(stores, logger))
	if err := service.StartBackground(context.Background()); err != nil {
		return err
	}
	catalogStore := catalog.StateStore{Repository: stores.Catalogs}
	mcpEndpoint, err := mcp.NewEndpoint(mcp.Options{
		Runtime:      wire.MCPRuntime{Service: service},
		Catalog:      catalogStore,
		QueuedSearch: mqSettings.Configured(),
		Logger:       logger,
	})
	if err != nil {
		return err
	}
	mcpHandler, err := mcp.NewHTTPHandler(mcp.HTTPOptions{
		Endpoint: mcpEndpoint,
		NamespaceFromRequest: func(r *http.Request) (string, error) {
			namespace, ok := httpapi.NamespaceFromContext(r.Context())
			if !ok || namespace == "" {
				return "", mcp.ErrUnauthorized
			}
			return namespace, nil
		},
		AllowedHosts:   serviceSettings.MCPAllowedHosts,
		AllowedOrigins: serviceSettings.MCPAllowedOrigins,
		Logger:         logger,
	})
	if err != nil {
		return err
	}
	userAuth := auth.NewService(stores.Users, stores.Sessions, auth.Options{
		RegistrationEnabled: serviceSettings.RegistrationEnabled,
		SessionTTL:          serviceSettings.SessionTTL,
	})
	// Knowledge-base ownership and the invite-based sharing rules are enforced
	// by the access service; httpapi only maps its typed errors to HTTP.
	accessService := access.NewService(stores, access.Options{
		InvitationTTL: serviceSettings.InvitationTTL,
	})
	api := httpapi.New(serviceSettings, service, nil, logger,
		httpapi.WithRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithKnowledgeBaseAdminRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithKnowledgeBaseSelfCreateRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithAccessControl(accessService),
		httpapi.WithAPIKeyStore(apikeys.New(stores.APIKeys)),
		httpapi.WithCatalogStore(catalogStore),
		httpapi.WithToolRegistry(mcpEndpoint),
		httpapi.WithChangeNotifier(wire.Notifier{Endpoint: mcpEndpoint}),
		httpapi.WithDocumentNotifier(wire.Notifier{Endpoint: mcpEndpoint}),
		httpapi.WithMCPHandler(mcpHandler),
		httpapi.WithUserAuth(userAuth),
	)

	address := net.JoinHostPort(serviceSettings.Host, strconv.Itoa(serviceSettings.Port))
	httpServer := &http.Server{
		Addr:              address,
		Handler:           api.Handler(),
		ReadHeaderTimeout: 15 * time.Second,
	}
	logger.Info("Praxis-RAG service starting",
		"version", version.Version,
		"address", address,
		"backend", string(ragConfig.Backend),
		"console", api.WebRoot(),
		"upload_mode", serviceSettings.UploadMode,
	)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	serveError := make(chan error, 1)
	go func() {
		serveError <- httpServer.ListenAndServe()
	}()

	select {
	case err := <-serveError:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
	}

	logger.Info("Praxis-RAG service stopping")
	shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpServer.Shutdown(shutdownContext); err != nil {
		return err
	}
	if err := service.Close(shutdownContext); err != nil {
		return err
	}
	return nil
}

func newLogger() *slog.Logger {
	level := slog.LevelInfo
	if isTruthy(os.Getenv("RAG_DEBUG_TRACE")) {
		level = slog.LevelDebug
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
}

func isTruthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}
