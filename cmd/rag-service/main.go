// Command rag-service starts the standalone Praxis-RAG HTTP service: REST API,
// MCP Streamable HTTP endpoint, and Console static assets.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/Guan0923/xinghe_rag/internal/access"
	"github.com/Guan0923/xinghe_rag/internal/apikeys"
	"github.com/Guan0923/xinghe_rag/internal/auth"
	"github.com/Guan0923/xinghe_rag/internal/catalog"
	"github.com/Guan0923/xinghe_rag/internal/config"
	"github.com/Guan0923/xinghe_rag/internal/httpapi"
	"github.com/Guan0923/xinghe_rag/internal/logging"
	"github.com/Guan0923/xinghe_rag/internal/mail"
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
	logger, err := logging.Setup()
	if err != nil {
		return err
	}
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
	// Every retrieval execution (synchronous REST/MCP and queued search worker)
	// passes through the runtime search path, so one counter here covers all of
	// them for the Console activity card.
	service.SetActivityRecorder(stores.Activity)
	if err := service.StartBackground(context.Background()); err != nil {
		return err
	}
	// Document parsing happens only in the standalone rag-document-worker, which
	// consumes Debezium-delivered notifications from Kafka. Without Kafka the
	// service still admits uploads, but their jobs stay queued.
	if !mqSettings.Configured() {
		logger.Warn("RAG_KAFKA_BOOTSTRAP_SERVERS is not configured: document jobs stay queued because rag-document-worker consumes them through Kafka")
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
	// The monitoring snapshot needs three live side channels the state aggregate
	// cannot provide: broker metadata (authentication + topic), the Redis
	// result-snapshot index, and the document-worker heartbeat. Each failure only
	// makes its own row unknown.
	var kafkaProbe func(context.Context) (monitor.KafkaStatus, error)
	if mqSettings.Configured() {
		kafkaProbeSettings := mqSettings
		kafkaProbe = func(ctx context.Context) (monitor.KafkaStatus, error) {
			status, probeErr := mq.BrokerHealth(ctx, kafkaProbeSettings)
			if probeErr != nil {
				return monitor.KafkaStatus{}, probeErr
			}
			return monitor.KafkaStatus{
				Brokers:     status.Brokers,
				Topic:       status.Topic,
				TopicExists: status.TopicExists,
				Partitions:  status.Partitions,
			}, nil
		}
	}
	var resultSnapshots func(context.Context) (int, error)
	if cache != nil {
		resultSnapshots = func(ctx context.Context) (int, error) {
			return cache.ResultSnapshotIndex().CountActive(ctx, time.Now())
		}
	}
	workerHeartbeat := func(ctx context.Context) (*monitor.WorkerHeartbeat, error) {
		snapshot, beatErr := stores.Heartbeats.Latest(ctx, state.HeartbeatRoleDocumentWorker, 60*time.Second)
		if beatErr != nil {
			return nil, beatErr
		}
		if !snapshot.Found {
			return nil, nil
		}
		return &monitor.WorkerHeartbeat{UpdatedAt: snapshot.UpdatedAt, Instances: snapshot.Instances}, nil
	}
	monitorService := monitor.New(monitor.Options{
		Stats:                 stores,
		MySQL:                 stores,
		DBStats:               stores.DB.Stats,
		Redis:                 cache,
		QdrantURL:             ragConfig.QdrantURL,
		Collection:            ragConfig.Collection,
		KafkaBootstrapServers: mqSettings.BootstrapServers,
		KafkaProbe:            kafkaProbe,
		DebeziumHealthURL:     os.Getenv("RAG_DEBEZIUM_HEALTH_URL"),
		DocumentTopic:         mqSettings.DocumentTopic,
		DocumentGroup:         mqSettings.DocumentGroup,
		KafkaProgress:         kafkaProgress,
		ResultSnapshots:       resultSnapshots,
		WorkerHeartbeat:       workerHeartbeat,
		OutboxBridge:          "debezium",
		UploadMode:            "async",
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
		// The mail server is configured from the Console and stored in
		// app_settings; without it e-mail verification stays disabled.
		Mail:    mail.NewService(stores.AppSettings),
		Codes:   stores.EmailCodes,
		Avatars: stores.Avatars,
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
		httpapi.WithAccountProfile(userAuth),
		httpapi.WithMailAdmin(userAuth),
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
