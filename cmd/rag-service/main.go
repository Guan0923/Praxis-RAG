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
	"github.com/Guan0923/xinghe_rag/internal/monitor"
	"github.com/Guan0923/xinghe_rag/internal/mq"
	"github.com/Guan0923/xinghe_rag/internal/redisstate"
	"github.com/Guan0923/xinghe_rag/internal/runtime"
	"github.com/Guan0923/xinghe_rag/internal/searchrefs"
	"github.com/Guan0923/xinghe_rag/internal/state"
	"github.com/Guan0923/xinghe_rag/internal/version"
	"github.com/Guan0923/xinghe_rag/internal/wire"
)

// documentBatchRetention is how long Console batch progress records are kept.
const documentBatchRetention = 7 * 24 * time.Hour

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
	// Batch records are Console progress metadata, not document state: prune
	// settled rows after a week so the table stays bounded.
	retentionContext, stopRetention := context.WithCancel(context.Background())
	defer stopRetention()
	if stores.DocumentBatches != nil {
		go stores.DocumentBatches.RunRetention(retentionContext, documentBatchRetention, time.Hour)
	}
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
	// Broker-less deployments parse admitted uploads in-process; Kafka
	// deployments keep the external rag-document-worker binary.
	inProcessWorker, err := resolveDocumentWorkerMode(
		os.Getenv("RAG_DOCUMENT_WORKER_INPROCESS"), mqSettings.Configured())
	if err != nil {
		return err
	}
	documentWorkerMode := "external"
	var embeddedWorker *embeddedDocumentWorker
	if inProcessWorker {
		pollInterval, pollErr := documentWorkerPollInterval(os.Getenv("RAG_DOCUMENT_WORKER_POLL_SECONDS"))
		if pollErr != nil {
			return pollErr
		}
		embeddedWorker, err = startEmbeddedDocumentWorker(stores, service, logger, pollInterval)
		if err != nil {
			return err
		}
		if embeddedWorker != nil {
			documentWorkerMode = "in-process"
		}
	}
	if documentWorkerMode == "external" {
		logger.Warn("Without an in-process document worker: REST/Console uploads stay queued until rag-document-worker runs",
			"hint", "start rag-document-worker, or leave RAG_KAFKA_BOOTSTRAP_SERVERS empty to parse in-process")
	}
	// The Console monitoring page is read-only: one state aggregate plus five
	// bounded dependency probes, assembled per request by internal/monitor.
	// Kafka progress is wired only when a broker is configured, so a brokerless
	// deployment reports the in-process outbox instead of fabricating offsets.
	var kafkaProgress func(context.Context) (monitor.KafkaProgress, error)
	if mqSettings.Configured() {
		kafkaSettings := mqSettings
		kafkaProgress = func(ctx context.Context) (monitor.KafkaProgress, error) {
			progress, progressErr := mq.DocumentConsumerProgress(ctx, kafkaSettings)
			if progressErr != nil {
				return monitor.KafkaProgress{}, progressErr
			}
			return monitor.KafkaProgress{
				CommittedOffset: progress.CommittedOffset,
				EndOffset:       progress.EndOffset,
				Lag:             progress.Lag,
			}, nil
		}
	}
	monitorService := monitor.New(monitor.Options{
		Stats:                 stores,
		MySQL:                 stores,
		DBStats:               stores.DB.Stats,
		Redis:                 cache,
		QdrantURL:             ragConfig.QdrantURL,
		Collection:            ragConfig.Collection,
		KafkaBootstrapServers: mqSettings.BootstrapServers,
		DebeziumHealthURL:     os.Getenv("RAG_DEBEZIUM_HEALTH_URL"),
		DocumentTopic:         mqSettings.DocumentTopic,
		DocumentGroup:         mqSettings.DocumentGroup,
		KafkaProgress:         kafkaProgress,
		OutboxBridge:          "debezium",
		UploadMode:            "async",
		DocumentWorker:        documentWorkerMode,
		PDFEngine:             ragConfig.PDFEngine,
		PDFServiceURL:         ragConfig.PDFServiceURL,
		StartedAt:             time.Now(),
	})
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
		httpapi.WithModelProfileRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithMonitorRuntime(monitorService),
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
		"upload_mode", "async",
		"document_worker", documentWorkerMode,
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
	// Stop parsing before the runtime and stores close; a bounded context keeps a
	// stuck ingestion from delaying shutdown indefinitely.
	workerStopContext, cancelWorkerStop := context.WithTimeout(context.Background(), documentWorkerShutdownTimeout)
	embeddedWorker.stop(workerStopContext)
	cancelWorkerStop()
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
