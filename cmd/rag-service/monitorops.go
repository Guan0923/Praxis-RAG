package main

import (
	"context"

	"github.com/Guan0923/Praxis-RAG/internal/connect"
	"github.com/Guan0923/Praxis-RAG/internal/httpapi"
	"github.com/Guan0923/Praxis-RAG/internal/k8sops"
	"github.com/Guan0923/Praxis-RAG/internal/state"
)

// monitorOps is the concrete httpapi.MonitorActionsRuntime wired in
// cmd/rag-service. It binds the three Console operator actions to their
// owning components:
//
//   - the document-job requeue lives in the state store (MySQL is the single
//     source of truth; Debezium emits the replacement notification from the
//     schedule_version bump, exactly like the worker's periodic sweep);
//   - the connector restart goes through the Kafka Connect REST client and
//     resets the supervisor's exhausted budget so the manual restart is not
//     re-flagged by stale bookkeeping;
//   - the worker scale goes through the Kubernetes API with the mounted
//     service account; outside a cluster (or without RBAC) the endpoints
//     degrade instead of failing startup.
type monitorOps struct {
	stores     *state.Stores
	connect    *connect.Runtime
	kubernetes *k8sops.Client
}

// monitorOpsBuild resolves the Kubernetes side channel. It returns nil (and
// no error) when no service account is mounted, so Compose / host deployments
// keep working and the Console hides the scale card.
func monitorOpsBuild(stores *state.Stores, connectRuntime *connect.Runtime) (*monitorOps, error) {
	ops := &monitorOps{stores: stores, connect: connectRuntime}
	settings, err := k8sops.SettingsFromEnv()
	if err != nil {
		return nil, err
	}
	if settings.Enabled() {
		client, err := k8sops.NewClient(settings)
		if err != nil {
			return nil, err
		}
		ops.kubernetes = client
	}
	return ops, nil
}

// RequeueStuckDocumentJobs implements the "立即重投" button: due retries and
// expired leases are requeued immediately. The semantics are Sweep's: the
// attempt budget and terminalization rules are shared, so this can never
// produce a fourth execution or a duplicate visible version.
func (m *monitorOps) RequeueStuckDocumentJobs(ctx context.Context) (int, error) {
	return m.stores.DocumentJobs.RequeueDueNow(ctx, 200)
}

// RestartConnector implements the "重启连接器" button: a Kafka Connect restart
// (connector and tasks by default) plus a supervisor budget reset for that
// connector, so the auto-restart loop gets a fresh budget.
func (m *monitorOps) RestartConnector(ctx context.Context, connector string, includeTasks bool) error {
	if m.connect == nil || !m.connect.Enabled() {
		return connect.ErrNotConfigured
	}
	if err := m.connect.Client().RestartConnector(ctx, connector, includeTasks); err != nil {
		return err
	}
	if supervisor := m.connect.Supervisor(); supervisor != nil {
		supervisor.ResetRestartBudget(connector)
	}
	return nil
}

// ConnectorNames returns the two fixed capture connector names. The transport
// layer uses them to reject any other name before it reaches Kafka Connect.
func (m *monitorOps) ConnectorNames() []string {
	if m.connect == nil {
		return nil
	}
	return m.connect.ConnectorNames()
}

// WorkerDeployment reads the document worker Deployment replica state.
func (m *monitorOps) WorkerDeployment(ctx context.Context) (*k8sops.Deployment, error) {
	if m.kubernetes == nil {
		return nil, k8sops.ErrNotInCluster
	}
	return m.kubernetes.DocumentWorker(ctx)
}

// ScaleWorker sets the document worker replica count.
func (m *monitorOps) ScaleWorker(ctx context.Context, replicas int32) error {
	if m.kubernetes == nil {
		return k8sops.ErrNotInCluster
	}
	return m.kubernetes.ScaleDocumentWorker(ctx, replicas)
}

// interfaces guard: monitorOps must satisfy the transport contract.
var _ httpapi.MonitorActionsRuntime = (*monitorOps)(nil)
