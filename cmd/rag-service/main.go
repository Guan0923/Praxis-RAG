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

	"github.com/Guan0923/Praxis-RAG/internal/access"
	"github.com/Guan0923/Praxis-RAG/internal/apikeys"
	"github.com/Guan0923/Praxis-RAG/internal/auth"
	"github.com/Guan0923/Praxis-RAG/internal/catalog"
	"github.com/Guan0923/Praxis-RAG/internal/config"
	"github.com/Guan0923/Praxis-RAG/internal/connect"
	"github.com/Guan0923/Praxis-RAG/internal/feishu"
	"github.com/Guan0923/Praxis-RAG/internal/httpapi"
	"github.com/Guan0923/Praxis-RAG/internal/logging"
	"github.com/Guan0923/Praxis-RAG/internal/mail"
	"github.com/Guan0923/Praxis-RAG/internal/mcp"
	"github.com/Guan0923/Praxis-RAG/internal/membership"
	"github.com/Guan0923/Praxis-RAG/internal/monitor"
	"github.com/Guan0923/Praxis-RAG/internal/mq"
	"github.com/Guan0923/Praxis-RAG/internal/payment"
	"github.com/Guan0923/Praxis-RAG/internal/redisstate"
	"github.com/Guan0923/Praxis-RAG/internal/runtime"
	"github.com/Guan0923/Praxis-RAG/internal/searchrefs"
	"github.com/Guan0923/Praxis-RAG/internal/state"
	"github.com/Guan0923/Praxis-RAG/internal/sysinfo"
	"github.com/Guan0923/Praxis-RAG/internal/version"
	"github.com/Guan0923/Praxis-RAG/internal/wire"
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
	defer func() { _ = logging.CloseLogFile() }()
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
	feishuSettings, err := config.FeishuSettingsFromEnv()
	if err != nil {
		return err
	}
	// Personal membership / Alipay. An incomplete merchant configuration is a
	// valid state: the Console still shows plans and usage, while purchasing is
	// disabled. Invalid scalar values (bad timeout/flag/gateway) fail startup.
	paymentSettings, err := payment.SettingsFromEnv()
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
	// The document lock is independent of the display cache: it has its own
	// TTL, and disabling the cache must not disable it. Redis is the required
	// lease backend, so every process that writes, recovers or cleans document
	// state must resolve the same RAG_REDIS_LOCK_* configuration
	// (rag-service, rag-document-worker, rag-mcp-ingest).
	lockSettings, err := state.DocumentLockSettingsFromEnv()
	if err != nil {
		return err
	}
	documentLock, err := redisstate.NewDocumentLock(lockSettings)
	if err != nil {
		return err
	}
	defer func() { _ = documentLock.Close() }()
	if err := stores.AttachDocumentLocker(documentLock); err != nil {
		return err
	}
	logger.Info("Document lease backend configured",
		"mode", "redis", "ttl_seconds", lockSettings.TTLSeconds)
	var redisProbe monitor.RedisProbe = cache
	if !cache.Enabled() {
		// The Console dependency row reports the lease backend when the display
		// cache is disabled.
		redisProbe = leaseLivenessProbe{lock: documentLock}
	}
	stores.CacheSync.Start()
	service, err := runtime.New(ragConfig, stores, serviceSettings.DataRoot)
	if err != nil {
		return err
	}
	service.SearchEngine().SetReferenceCapture(searchrefs.New(stores, logger))
	// Every retrieval execution (synchronous REST/MCP) passes through the
	// runtime search path, so one counter here covers all of them for the
	// Console activity card.
	service.SetActivityRecorder(stores.Activity)
	if err := service.StartBackground(context.Background()); err != nil {
		return err
	}
	// Distributed Kafka Connect hosts the two Debezium capture connectors. The
	// supervisor repairs one class of failure the platform leaves alone: a task
	// that entered FAILED (for example after a transient MySQL/network error)
	// stays FAILED until something restarts it. Worker failover itself is Kafka
	// Connect's job and needs no application code.
	connectSettings, err := connect.SettingsFromEnv()
	if err != nil {
		return err
	}
	connectRuntime, err := connect.NewRuntime(connectSettings, logger)
	if err != nil {
		return err
	}
	defer connectRuntime.Close()
	connectRuntime.Start(context.Background())
	if !connectRuntime.Enabled() {
		logger.Warn("RAG_CONNECT_URLS is not configured: the Console cannot report Kafka Connect capture state and failed tasks are not restarted automatically")
	}
	// Document parsing happens only in the standalone rag-document-worker, which
	// consumes Debezium-delivered notifications from Kafka. Without Kafka the
	// service still admits uploads, but their jobs stay queued.
	if !mqSettings.Configured() {
		logger.Warn("RAG_KAFKA_BOOTSTRAP_SERVERS is not configured: document jobs stay queued because rag-document-worker consumes them through Kafka")
	}
	// The Console monitoring page is read-only: one state aggregate plus the
	// bounded dependency probes assembled per request by internal/monitor.
	// Task delivery is owned by Debezium, so the pipeline view reports the two
	// capture connectors' health and two independent business consumer groups
	// (documents and cache); search is synchronous and has no queue. The service
	// never fabricates an in-process outbox row.
	progressSettings := mqSettings
	kafkaGroupProgress := func(ctx context.Context, topic, group string) (monitor.KafkaProgress, error) {
		progress, progressErr := mq.ConsumerProgressFor(ctx, progressSettings, topic, group)
		if progressErr != nil {
			return monitor.KafkaProgress{}, progressErr
		}
		return monitor.KafkaProgress{
			CommittedOffset: progress.CommittedOffset,
			EndOffset:       progress.EndOffset,
			Lag:             progress.Lag,
		}, nil
	}
	// The monitoring snapshot needs live side channels the state aggregate
	// cannot provide: broker metadata (authentication + topic) and the
	// document-worker heartbeat. Each failure only makes its own row unknown.
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
	// Host resources are a best-effort extra card on the monitoring page: a
	// failed sampler only leaves "system" null, it never blocks the service.
	// The in-memory log ring and the optional RAG_LOG_FILE path are installed by
	// logging.Setup above, so the Console log view reads the same records.
	var systemSampler monitor.SystemSampler
	sampler, samplerErr := sysinfo.New()
	if samplerErr != nil {
		logger.Warn("系统资源采集初始化失败，Console 监控页的 system 将为 null", "error", samplerErr)
	} else {
		systemSampler = sampler
	}
	var connectorProbe monitor.ConnectorProbe
	if connectRuntime.Enabled() {
		connectorProbe = connectRuntime
	}
	// The Console operator actions (立即重投 / 重启连接器 / worker 扩缩容).
	// Outside a Kubernetes pod the scale card degrades; the requeue and
	// restart actions work wherever MySQL / Kafka Connect are reachable.
	actions, err := monitorOpsBuild(stores, connectRuntime)
	if err != nil {
		return err
	}
	if actions.kubernetes == nil {
		logger.Info("Kubernetes 操作未启用：未挂载 ServiceAccount（Compose/宿主机部署为预期行为），Console 的扩缩容卡片将显示为不可用")
	}
	monitorOptions := monitor.Options{
		Stats:                 stores,
		MySQL:                 stores,
		DBStats:               stores.DB.Stats,
		Redis:                 redisProbe,
		QdrantURL:             ragConfig.QdrantURL,
		FallbackCollection:    ragConfig.Collection,
		IndexedCollections:    stores.MonitorIndexedCollections,
		KafkaBootstrapServers: mqSettings.BootstrapServers,
		KafkaProbe:            kafkaProbe,
		// The knowledge-graph sidecar is optional: an empty URL makes the
		// Console report the capability as disabled instead of degraded.
		LightRAGServiceURL:     ragConfig.LightRAGServiceURL,
		LightRAGServiceToken:   ragConfig.LightRAGServiceToken,
		LightRAGServiceTimeout: ragConfig.LightRAGServiceTimeout,
		// The two Debezium connectors now run as tasks in the distributed Kafka
		// Connect cluster. The probe reads connector/task state (including the
		// worker that owns each task) and the restart supervisor's bookkeeping;
		// the binlog offset itself stays inside Kafka Connect.
		ConnectorProbe:     connectorProbe,
		DocumentConnector:  connectSettings.DocumentConnector,
		CacheConnector:     connectSettings.CacheConnector,
		DocumentTopic:      mqSettings.DocumentTopic,
		DocumentGroup:      mqSettings.DocumentGroup,
		CacheTopic:         mqSettings.CacheTopic,
		CacheGroup:         mqSettings.CacheGroup,
		KafkaGroupProgress: kafkaGroupProgress,
		WorkerHeartbeat:    workerHeartbeat,
		Delivery:           "debezium",
		UploadMode:         "async",
		PDFEngine:          ragConfig.PDFEngine,
		PDFServiceURL:      ragConfig.PDFServiceURL,
		StartedAt:          time.Now(),
		System:             systemSampler,
		Logs:               logging.DefaultBuffer(),
		LogFilePath:        logging.LogFilePath(),
		// Operator-action capability flags for the Console buttons. The
		// actions themselves run through httpapi.MonitorActionsRuntime; these
		// funcs only report whether they are wired.
		RequeueJobs: func(ctx context.Context) (int, error) {
			return actions.RequeueStuckDocumentJobs(ctx)
		},
	}
	// The scale funcs stay nil outside a Kubernetes pod so the Console card
	// reports 未启用 instead of an endpoint that would answer 503.
	if actions.kubernetes != nil {
		monitorOptions.ScaleWorker = func(ctx context.Context, replicas int32) error {
			return actions.ScaleWorker(ctx, replicas)
		}
	}
	monitorService := monitor.New(monitorOptions)
	// The Console Kubernetes tab reads the shared workload/pod cache and the
	// rolling restart writes the pod-template annotation through the same
	// client. A deployment with no cluster credentials stays usable: the
	// backend is simply absent and the tab shows the configuration hint.
	kubernetesCluster, err := kubernetesMonitorBuild(logger)
	if err != nil {
		return err
	}
	if kubernetesCluster != nil {
		defer kubernetesCluster.Close()
	}
	catalogStore := catalog.StateStore{Repository: stores.Catalogs}
	mcpEndpoint, err := mcp.NewEndpoint(mcp.Options{
		Runtime: wire.MCPRuntime{Service: service},
		Catalog: catalogStore,
		Logger:  logger,
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
		Mail:    mail.NewService(stores.AppSettings, stores.Settings.EncryptionKey),
		Codes:   stores.EmailCodes,
		Avatars: stores.Avatars,
	})
	// Knowledge-base ownership and the invite-based sharing rules are enforced
	// by the access service; httpapi only maps its typed errors to HTTP.
	membershipService, err := membership.New(stores, paymentSettings)
	if err != nil {
		return err
	}
	accessService := access.NewService(stores, access.Options{
		InvitationTTL: serviceSettings.InvitationTTL,
		Entitlements:  membershipService,
	})
	// The optional Feishu integration stores one OAuth grant per Console account
	// and calls Feishu with that account's user_access_token. It is disabled
	// unless the deployment configured the app credentials and redirect URI.
	feishuService := feishu.NewService(feishuSettings, stores.FeishuConnections, stores.FeishuOAuthStates)
	if !feishuService.Enabled() {
		logger.Info("Feishu document import is disabled: set RAG_FEISHU_APP_ID, RAG_FEISHU_APP_SECRET and RAG_FEISHU_REDIRECT_URI to enable it")
	}
	httpOptions := []httpapi.Option{
		httpapi.WithRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithKnowledgeBaseAdminRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithKnowledgeBaseSelfCreateRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithModelProfileRuntime(wire.HTTPRuntime{Service: service}),
		httpapi.WithMembership(membershipService),
		httpapi.WithMonitorRuntime(monitorService),
		httpapi.WithMonitorActions(actions),
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
		httpapi.WithFeishuService(feishuService),
	}
	// The Kubernetes monitor endpoints must only exist when a cluster client is
	// configured; without it they answer 503 "未启用" instead of pretending.
	if kubernetesCluster != nil {
		httpOptions = append(httpOptions, httpapi.WithKubernetesRuntime(kubernetesCluster))
	}
	api := httpapi.New(serviceSettings, service, nil, logger, httpOptions...)

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

// leaseLivenessProbe adapts the document-lock backend to the Console Redis
// dependency probe when the display cache itself is disabled.
type leaseLivenessProbe struct{ lock *redisstate.DocumentLock }

// Enabled reports that the lease backend is configured.
func (leaseLivenessProbe) Enabled() bool { return true }

// Ping checks lease-backend connectivity.
func (p leaseLivenessProbe) Ping(ctx context.Context) error { return p.lock.Ping(ctx) }
