# Historical UUID results/artifacts and logical deletion — 2026-10-23

> **Transport note (2026-11-09):** The dated records in this file were produced
> while the repository still used RocketMQ. The current transport is Kafka
> (`RAG_KAFKA_*`, explicit `RAG_*_DLQ_TOPIC`); the Kafka/MySQL/Debezium live check
> is recorded immediately below. RocketMQ wording in the historical sections is
> retained as history.
>
> **Superseded (2026-11-13):** 事务 outbox 与单 Debezium 连接器已下线，当前为
> 两个 Debezium 连接器表级采集；下方 outbox/relay 记录均为历史。
>
> **Superseded (2026-11-13, 搜索同步化):** 排队搜索、结果快照、`get_search_result`、
> 搜索 topic/worker/DLQ 已下线。下方涉及 `search_result_snapshots`、
> `internal/redisstate/snapshot_index.go`（ZSET）、结果快照计数/ZADD、排队搜索与
> `verify-kafka-search.py` 的记录均为历史存档，不代表当前实现；`search_tasks`/
> `search_result_snapshots` 表保留数据但不再读写。当前搜索验证以同步检索、超时/取消与
> artifact 独立读取为准（见 [产物引用](docs/result-references.md)）。

## Kubernetes 应用层部署（Kustomize）— 2026-11-17

工作树 `dsh-0a7946b7-479b`（Windows 11，kubectl v1.36.1 / kustomize v5.8.1）。
本轮交付的是**应用层 Kubernetes 清单**（`deploy/k8s/`）：MySQL / Qdrant / Kafka /
Kafka Connect 与可选 Redis 仍由使用方提供，清单只部署 API/MCP/Console、三个 worker
和可选的 pdf-parser。

离线部分（渲染 + 534 项契约断言）见下；真实集群实测见「真实集群补测」。

### 已验证（可复核，全部离线）

```powershell
# 1) 渲染全部入口（四个完整 overlay + 四个 bootstrap + 公共 ConfigMap）
foreach ($o in 'go','vision','go-cache','vision-cache') { kubectl kustomize "deploy/k8s/overlays/$o" > $null }
foreach ($b in 'state-check','state-migrate','connect-precheck','connect-register') { kubectl kustomize "deploy/k8s/bootstrap/$b" > $null }
kubectl kustomize deploy/k8s/common > $null
# 全部 exit 0；bootstrap 各只产出 1 个 Job，common 只产出 1 个 ConfigMap

# 2) 清单契约断言：命令、镜像、Service selector、Secret 键、DNS、探针、资源、安全上下文
python deploy/k8s/tests/verify_manifests.py        # checks=534 failures=0

# 3) 连接器模板用清单实际注入的变量展开（环境取自渲染出的 ConfigMap + Secret 键）
go run ./cmd/rag-connect register --offline --connector-dir deploy/kafka-connect/connectors --wait 180s
# → praxis-cache-cdc / praxis-document-cdc 各 31 项配置已展开，exit 0

# 4) --wait 的单位：裸数字不可用（Go duration 标志）
go run ./cmd/rag-connect register --offline --wait 180
# → invalid value "180" for flag -wait: parse error（exit 2）
```

第 4 项是本轮发现的既有缺陷：`docker-compose.yml` 的 `connect-init`、
`scripts/start-stack.ps1`、`deploy/kafka-connect/README.md` 与 `cmd/rag-connect/main.go`
的使用注释都写成 `--wait 180`，会让注册流程直接退出 2（start-stack 随后抛
「Connector registration failed」）。已统一改为 `180s`，Kubernetes 的注册 Job 也用 `180s`。

```powershell
go build ./cmd/rag-connect     # exit 0
go vet ./cmd/rag-connect       # exit 0
```

镜像运行时用户（决定 securityContext 的写法；按 Dockerfile 运行时阶段的同样步骤实测）：

```powershell
docker run --rm alpine:3.21 sh -c 'apk add --no-cache ca-certificates tzdata wget >/dev/null 2>&1; addgroup -S rag; adduser -S -D -G rag rag; id rag'
# → uid=100(rag) gid=101(rag)
```

`USER rag` 是**名字**用户：kubelet 无法校验非 root，设 `runAsNonRoot: true` 会以
`image has non-numeric user (rag)` 拒绝启动；写死 `runAsUser` 又会在基础镜像换 UID 时
丢掉 `/app` 写权限。因此 Go 工作负载不设置这两个字段，改用
`allowPrivilegeEscalation: false` + `capabilities.drop: [ALL]` + `seccompProfile: RuntimeDefault`；
pdf-service 的 UID 在构建期固定为 10001，可以设置 `runAsNonRoot: true` + `runAsUser: 10001`。

### 实测环境（2026-11-17 下午补测，见下方「真实集群补测」）

kind v0.27.0 单节点集群（`praxis-accept`，node 镜像 `kindest/node:v1.32.2` 经 daocloud 镜像源拉取）+ 本机 Docker 栈（MySQL 8 / Kafka / Qdrant / Redis / pdf-parser / 两个 Kafka Connect worker）。应用镜像为本仓库 `Dockerfile` 的等价最小镜像（同一 `/app/bin` 契约、`rag` 100:101 用户），kind 加载后由 kubelet 调度。

### 真实集群补测（2026-11-17 下午，逐项实测）

1. **server-side dry-run**：8 个入口（4 overlay + 4 bootstrap Job）`kubectl apply --server-side --dry-run=server -k` 全部 `serverside-applied (server dry run)`，exit 0。
2. **真实 apply**：`go+persistence` 变体真实落盘——ConfigMap / Service / PVC（`standard` StorageClass，Bound 10Gi）/ 3 个 Deployment 全部创建；3 个 Pod `Running`，资源 requests/limits、`/health` 探针、Recreate 策略、`data`+`tmp` 卷均按清单生效；无 Warning 事件。
3. **bootstrap Job**：`state-check`（3s Complete，`schema=ready tables=32`）、`state-migrate`（4s Complete，`mode=apply ... schema=ready`）集群内实测；`connect-precheck`/`connect-register` 亦在集群内跑通（见 4）。
4. **CDC 注册与 binlog 位点**：两连接器 `RUNNING`（task0 RUNNING，分布在两个 Connect worker）；上传后 `mysql-bin.000040 pos 29012747 → 29403665`（document 连接器位点实测推进）。Kafka 重建后以全新内部 topic 重新注册，`register --wait 180s` + `wait --wait 180s` 全通过（0 退出）。
5. **上传 → succeeded → 同步检索带出处**：宿主机与集群内（port-forward → Service → rag-service Pod → CDC → 集群内 document-worker Pod 消费）双路径实测；`e2e-k8s.txt` `succeeded/completed`，检索 `evidence_status=found`，命中含 `source/chunk_id/page/artifact resource_uri`。
6. **一次 MCP 调用**：`initialize`（拿 `Mcp-Session-Id`）→ `notifications/initialized`（202）→ `tools/call search`（200，SSE data 行）→ 内层 JSON `evidence_status=found`，宿主机与集群内各跑一遍。
7. **vision 真实 VLM**：pdf-parser 容器日志见 `POST aihub-api…/v1/chat/completions 200`；1 页 PDF 证据 `page=1`、`pdf_parser_version=docling-service-v1`、source/markdown/manifest 3 个 artifact。集群内 pdf-parser Pod 同样跑通（含一次 VLM 网关偶发 500 后 job 自愈重试成功）。
8. **worker 1→2**：消费组 `praxis_accept_document_workers` 从 1 成员（4 分区全占）到 2 成员各占 `(0,2)`/`(1,3)`；心跳实例=Pod 名（`rag-document-worker-<pod>-1`）。
9. **§9 终止演练**：SIGTERM（容器级，`Upload failure record pending → context canceled` 后优雅停止）与 `--grace-period=0 --force` 等价 SIGKILL 两种；默认参数（租约 300s + 扫描 30s）下删除唯一 worker 并立即起新副本，任务在 **+301s** 由新副本重投并 `succeeded`；残留核对：旧 Pod 的 `upload_operations.workspace_node` 行为 `ROLLED_BACK`（旧执行）+ 新执行 `COMMITTED`，`workspace_node=NULL`，`cleanup_pending=1`，唯一可检索版本无重复。容器级对照（60s 短租约）同样验证了 skip→re-emit→接管全链路。
10. **文档 worker Pod 删除（集群内）**：`kubectl delete pod` 处理中文档，+301s 后由存活副本完成，`podkill.pdf` 证据 `page=1`、provenance `#/texts/0`。

补测期间的环境事实（对复现重要）：本机 Kafka 通告 `127.0.0.1:9092`，kind Pod 内回连不可达——实测中给 broker 临时加了 `EXTERNAL_KIND` listener 并经 Service+Endpoints 暴露；Kafka 容器重建导致内部 topic 与 accept 业务 topic 重建，连接器以 `snapshot=initial` 重新采集。这些是**本机验收环境的补丁**，不属于交付清单内容；生产集群按 README §5 提供可达的 `kafka.infra.svc.cluster.local:9092` 即无此问题。

### 未验证（缺集群与外部依赖）

- 无。六项原「未验证」条目已在 2026-11-17 下午的真实集群补测中全部实测（见上方「真实集群补测」）。
- 以下为补测中的已知边界，非清单缺陷：
  - 本机 VLM 网关偶发 500（一次 `/v1/parse 500`），job 3 次预算内自愈；8 页 PDF 曾 3 次耗尽转 `failed`（属远端模型稳定性）。
  - 本机 Kafka advertised listener 限制（见上）；生产按 README §5 配置无此问题。
  - `scale` 下游「限流」项：实测了分区再平衡与心跳；限流（QPS/带宽）在本机栈无对应指标，未测。

验收步骤、判定标准与失败排查见 [deploy/k8s/README.md](deploy/k8s/README.md) §6/§8/§9/§15。

## Console 监控页运维操作（立即重投 / 重启连接器 / worker 扩缩容）— 2026-11-17

工作树 `dsh-0a7946b7-479b`。本轮把监控页从「只读」升级为「按能力渲染操作按钮」：
`overview` 顶层新增 `actions` 能力块（4 个布尔），三个操作端点全部在
`/v1/admin/*` 边界（管理员会话 + CSRF，或本机直连破窗）。

### 已验证（可复核）

```powershell
# Go（串行，MySQL 容器 praxis-rag-mysql 在 127.0.0.1:3307）
$env:RAG_TEST_MYSQL_URL = 'mysql://rag:<密码>@127.0.0.1:3307/praxis_unit_test'
go test -p 1 -count=1 ./internal/k8sops ./internal/state ./internal/connect ./internal/httpapi ./internal/monitor
# ok × 5；新增：k8sops 6 例（GET/PATCH 契约、403/404 哨兵、范围校验、非集群降级）
#   state 1 例（RequeueDueNow 到期重试+租约过期、不动 queued/活租约）
#   connect 2 例（ResetRestartBudget 清空耗尽预算、未知连接器无操作）
#   httpapi 7 例（重投/重启/扩缩容的传输契约、管理边界、参数校验、未装配 503）
#   monitor 既有用例回归（actions 能力块不破坏现有快照契约）

# 前端
cd frontend; npx tsc --noEmit          # exit 0
npx vitest run src/MonitorPage.test.tsx   # 33/33（新增 5 例：能力开关隐藏、
#   重投 POST+CSRF+toast、失败行重启按钮、扩缩容读+改副本、未启用卡片说明）
npx vite build                         # ✓（build/ 产物不提交）

# 清单
python deploy/k8s/tests/verify_manifests.py   # checks=546 failures=0
#   （新增 12 项断言覆盖 rbac-monitor-ops.yaml 的 SA/Role/RoleBinding 与
#    rag-service 的 serviceAccountName 接线）
```

### 宿主机真实栈实测（2026-11-17 傍晚，MySQL/Kafka/遗留 Debezium Server 栈）

- `actions` 能力块：宿主机（未挂 ServiceAccount）返回
  `requeue_jobs:true, restart_connectors:true, scale_worker:false, worker_deployment:false`——
  与「未启用降级」设计一致。
- **立即重投**：手工把一个 `failed` 终态任务改成 `processing + lease_until=0` 制造卡死行
  → overview `stuck_processing:1` → `POST /actions/requeue-jobs` 返回 `{"requeued":1}`
  → 数据库回到 `queued/retry_wait` 且 `schedule_version` 1226→1227（Debezium 的替代通知信号），
  `attempts` 保持 1（预算未额外消耗）。测试后已把该行恢复为 `failed/failed`。
- **K8s 未启用降级**：`GET /actions/worker-deployment` 与 `POST .../scale`
  均返回 503 `Kubernetes 操作未启用（未挂载 ServiceAccount）`；宿主机启动日志输出对应 INFO。
- **鉴权边界**：`Host: example.com` 的管理调用 403 `仅允许本机直接访问管理接口`。
- **参数校验**：未知连接器名 400 `未知连接器`；非 JSON body 422（FastAPI 形状）；
  `replicas` 越界（-1/65）400。
- **重启连接器**（REST 交互）：宿主机遗留部署是 Debezium Server 而非 Kafka Connect
  （8083/8084 无 Connect REST），实测返回 502 `连接器重启请求失败`（正确错误映射）；
  Connect REST 的 `includeTasks=true` 请求形状、403/404 映射与 supervisor 预算清空
  由 Go 单测覆盖（fakeStatusClient + RestartConnector patch 断言）。真实 Connect 集群
  上的端到端重启在下次集群验收时补测。

### 未验证

- kind 集群内的扩缩容按钮端到端（需要重建集群；RBAC/patch 形状已由 verify_manifests.py
  与 k8sops 单测覆盖，kubectl scale 等价性见 README §8.1）。

### 追加实测（2026-11-17 晚，合并到 main 并重启全栈后）

合并到 main（`5b82b77`）后重建 `dist/`（build-go.ps1 冒烟通过）并重启全栈，
补上了此前「未验证」的最后一项：

- **重启连接器（真实 Kafka Connect 集群）**：`POST /actions/connectors/praxis-cache-cdc/restart`
  返回 200 `{"restarted":true}`；4~10 秒后 connector 与 task 均回到 `RUNNING`
  （重启期间与之后 `praxis-document-cdc` 另一路保持 RUNNING 不受影响）。
- **立即重投（重启后回归）**：`{"requeued":0}`（当前无卡死任务，符合预期）。
- 清理了验收残留：删除了 Connect 集群里遗留的 `praxis-accept-*` 两个连接器
  （会重复采集 binlog 的「旧版本内容」）与两个遗留 Debezium Server 容器
  （占用 8083/8084 端口，阻塞真正的 Connect worker 启动）。
- openapi 契约表补登四个操作端点（`227534e`），`/docs` 与 `/openapi.json`
  现已包含完整监控操作面。

## 当前验证范围：搜索同步化（MCP/REST 一次调用直返证据）— 2026-11-13

工作树 `dsh-07b26c03-826a`（Windows 11，Go 1.27.3）。本轮验证目标是**搜索同步化改造**：MCP `search` 与 REST `POST /v1/search` 一次调用返回证据；删除搜索任务、结果快照、轮询、SQL 任务逻辑、搜索队列/worker/DLQ 与 Debezium 搜索采集；artifact 独立读取与缓存同步链路保留。代码/文档/Console 变更范围见 [PROGRESS.md](PROGRESS.md) 最后一轮。

### 已验证（可复核）

```powershell
# 全体 Go 包（生产代码）构建
$env:GOCACHE = '.test-tmp\gocache'; go build ./...                       # exit 0

# 全部包（含测试文件）编译检查
go vet ./...                                                             # exit 0（12 个此前编译阻塞的包已清理）

# 不依赖外部服务的单元测试
go test -p 1 -count=1 ./internal/mcp ./internal/redisstate ./internal/mq `
  ./internal/monitor ./internal/contracts ./internal/cacheconsumer       # 全部 ok

# 同步搜索专项
go test -p 1 -count=1 ./internal/mcp -run 'TestTool|TestSearch|TestRestricted|TestScope'   # ok
go test -p 1 -count=1 ./internal/httpapi -run 'TestSearch|TestArtifact|TestOpenAPI|TestServerNoStore'  # ok（含 TestSearchResultEndpointRemoved）
go test -p 1 -count=1 ./internal/runtime -run 'TestSearchTimeout|TestServiceSearchPropagatesClientCancellation'  # ok
go test -p 1 -count=1 ./internal/e2e ./internal/cdcacceptance -run '^$'    # 测试二进制编译 ok

# 前端
cd frontend; npm run typecheck                                            # 通过
npx vitest run                                                           # 17 files / 224 tests 全通过

# 缓存同步链路（与搜索快照无关的独立专项）
go test -p 1 -count=1 ./internal/cacheconsumer                           # 12/12 PASS（含新增 4 例：8 表 DELETE 映射、跨 scope、重放幂等、生产 decoder）
```

覆盖点：MCP 恰好 5 个工具且顺序固定（`search`、`add_document`、`update_document`、`delete_document`、`get_document_status`）；`search` 一次调用返回 `results`/`artifacts`/`evidence_status`；`wait_seconds` 报未知参数；受限 scope 同步检索仍按文档过滤；REST `POST /v1/search` 响应无 `result_id`/`created_at`/`expires_at`/`snapshot_available`/`snapshot_error`；`GET /v1/search-results/{id}` 返回 404；artifact 独立读取保留；`RAG_SEARCH_TIMEOUT_SECONDS` 默认 120s、非法值启动失败、超时映射 `Search timed out`、客户端取消可向下传播。

### 环境限制与未验证项

- 本机无 `RAG_TEST_MYSQL_URL`、Redis、Kafka 与 docker：`internal/state`、`internal/runtime`、`internal/httpapi`（api_keys）、`internal/searchrefs`、`internal/e2e`、`internal/cdcacceptance` 中依赖 MySQL 的用例由 `internal/statetest` **Fatalf（环境失败，不是 skip）**；未在本机实跑。
- 未执行真实 Docker/Debezium/Kafka/Redis 端到端切换演练；未验证真实 Qdrant + 模型的同步搜索时延分布。
- Cache 链路四类场景（接口写、手工 SQL 写、删除、故障恢复）由 `internal/cacheconsumer` 与 `internal/cdcacceptance/cache_chain_test.go`（新增 DELETE 后 `kb_list.document_count` 递减、跨 namespace 收敛）覆盖；MySQL/Redis/Kafka 实跑仍需在具备依赖的环境执行。
- 历史表 `search_tasks`/`search_result_snapshots` 数据保留、未 DROP；物理删除按单独运维安排执行。

## 当前验证范围：Debezium 双连接器（事务 outbox 已下线）— 2026-11-13

> **取代关系**：本文件下方所有提到 outbox / relay / 单个 Debezium 连接器 / outbox
> PENDING 记录 / `capture_progress` 的验证记录，验证的是**已下线的旧链路**，仅作历史
> 保留；当前投递为两个 Debezium 连接器直接从 MySQL 表级采集，契约见
> [docs/cdc-migration.md](docs/cdc-migration.md)。本轮改造的代码/文档/Console 变更范围
> 见 [PROGRESS.md](PROGRESS.md) 与 [GO_MIGRATION.md](docs/GO_MIGRATION.md) 最后一轮。

工作树 `dsh-b0dc9fd1-4cde`（Windows 11，Go 1.27.3）。本轮验证目标是**文档、Console
监控页、存量切换 CLI 与旧表退役 CLI**；Go worker/监控后端由并行任务实现并各自定向测试，
本记录不重复声明其覆盖。

### 已验证（可复核）

```powershell
# 仅构建本轮新增/修改的两个命令（避免与并行任务的全仓构建相互影响）
go build ./cmd/rag-cdc-cutover/ ./cmd/rag-service-migrate/   # exit 0
go vet   ./cmd/rag-cdc-cutover/ ./cmd/rag-service-migrate/   # exit 0
gofmt -l cmd/rag-cdc-cutover/main.go cmd/rag-service-migrate/main.go  # 无输出

# CLI 退出码冒烟（go run 包装层会把内层 exit 2 显示为 exit status 2）
go run ./cmd/rag-cdc-cutover                      # 用法错误 → 2（未连接数据库）
go run ./cmd/rag-cdc-cutover --help               # → 0
go run ./cmd/rag-cdc-cutover --backfill           # 缺 --baseline → 2
go run ./cmd/rag-cdc-cutover --record-position    # 缺 --file/--pos → 2

# 旧表退役：无 --yes 时必须拒绝执行（需真实 MySQL 才能观察，本轮未连库执行）
# rag-service-migrate --retire-outbox            → 2，只读列出仍存在的旧表，不执行 DROP
# rag-service-migrate --retire-outbox --yes      → 0，逐表打印 DROP 结果（幂等）
```

- 依赖的状态库 API 由并行任务落地并测试：`internal/state` 的
  `CutoverStateGet/Set/All`、`CutoverInventory`、`CutoverCacheRevisions`、
  `DocumentJobs.RequeueActive`、`SearchTasks.RequeueActive`、`CacheSync.MarkAllDirty`；
  `internal/migrate` 的 `RetireOutbox`/`OutboxTablesPresent`。对方报告
  `go test -count=1 ./internal/state/ ./internal/migrate/` 全绿；本轮 CLI 只调用这些
  已测试入口，不重复实现 DDL/重排逻辑。
- 文档一致性：在 `README.md`、`AGENTS.md`、`docs/**`、`PROGRESS.md` 中检索
  `document_job_outbox`/`search_task_outbox`/`cache_outbox`/`outbox_bridge`/
  `RAG_OUTBOX_BRIDGE`/`进程内 relay`，剩余命中均为明确标注「已下线 / 历史实现」或
  契约对比表；`docs/GO_PARITY_AUDIT.md` 与历史审计记录按要求保留原文。
- 前端搜索文案：`frontend/src` 中未找到「等待 Kafka 确认 / 受理失败 / 提交失败」等
  旧文案；Console 检索页只走同步 REST `/v1/search`，排队搜索的受理语义只存在于 MCP
  与文档（已在 USAGE/README/result-references 改为「提交即受理，稍后按 result_id 读取」），
  因此本项无需改前端组件。

### 未验证（需要真实环境）

- **后端链路**：未启动 Docker/Compose，未运行真实 MySQL 8 + 两个 Debezium 连接器 +
  Kafka 3.9 + Redis，未观察任务连接器快照/流式切换、缓存连接器 8 表事件、三个消费组
  积压与 DLQ；未验证缓存 revision 收敛与 `RefreshDirty`。
- **切换与回滚演练**：未在真实库上执行 `--record-position`/`--backfill`/
  `--mark-cache-baseline`/`--verify`/`--retire-outbox` 全序列，也未演练失败中断后按单个
  模式续跑；未验证 `--verify` 在真实活动任务下的失败/通过分支。
- **搜索与结果读缓存**：未验证 MySQL `search_result_snapshots` 回源、Redis 未命中
  回填、TTL 清理后 410 的真实行为。
- **全仓构建**：只构建了本轮的两个 CLI；`go build ./...` 未作为验收命令执行（并行任务
  正在接线 `cmd/rag-service` 的 `monitor.Options`，全仓构建在其完成前可能失败）。
- **Console 前端**：`frontend/node_modules` 不存在，按要求未执行 `npm install`；
  因此**未运行** `npm run typecheck`、`npm test`、`npm run build`。`api.ts` 的
  monitor 类型、`MonitorPage.tsx` 的卡片/行、`MonitorPage.test.tsx` 的 fixture/断言
  仅按最终字段契约静态对齐，需在有依赖的环境补跑 typecheck 与用例。
- 没有浏览器 E2E；监控页的自动刷新、降级告警与窄屏布局未人工点检。

### 切换/演练/回滚步骤（供真实环境执行）

演练（建议先在一次性 MySQL schema + 测试 topic 上）：

1. 停旧入口与旧 worker，确认无 outbox 写入进程；保留数据库备份。
2. `rag-cdc-cutover --inventory`：核对未完成任务数、最老年龄、最小
   `schedule_version`、缓存 key 数与各 key revision（只读）。
3. `SHOW MASTER STATUS` 取 File/Position →
   `rag-cdc-cutover --record-position --file=<File> --pos=<Position>`。
4. 启动缓存连接器与任务连接器，确认初始快照完成。
5. `rag-cdc-cutover --backfill --baseline=<N>`：循环到 0；重复执行应再返回 0（幂等）。
6. `rag-cdc-cutover --mark-cache-baseline`：记录标脏 key 数（无 Redis 也可执行）。
7. `rag-cdc-cutover --verify --baseline=<N>`：活动任务必须全部
   `schedule_version >= N+1`；脏缓存数等待收敛（警告不阻塞）。
8. 也可用 `--all --file=... --pos=... --baseline=N` 顺序执行 2–7，任一步失败即停止，
   之后用单模式续跑。

回滚：

- **`--retire-outbox` 之前**：停新连接器/新 worker，回退服务与 worker 二进制并重新启用
  旧 outbox 写入链路即可；MySQL 数据与位点文件保留，消费者幂等去重。
- **`--retire-outbox` 之后**：四张 outbox 表已 DROP，回滚必须从迁移前备份恢复这四张表；
  因此只在确认 CDC 切换稳定、无需回滚后执行，且执行前做完整备份。
- 切换窗口内不要同时运行旧 relay 与新连接器消费同一批 topic；回滚后需按旧版本的
  topic/group 配置重新部署。


### Lead 最终集成验证（2026-11-13，在文档/Console 任务之后）

工作树 `dsh-b0dc9fd1-4cde`，Go 1.27.3，MySQL 8（`praxis_unit_test`，127.0.0.1:3307），测试串行 `-p 1`。

```powershell
$env:RAG_TEST_MYSQL_URL='mysql+pymysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'
go build ./...                                   # exit 0
go vet ./...                                     # exit 0
go test -p 1 -count=1 ./internal/state/ ./internal/migrate/ ./internal/mq/ ./internal/monitor/ `
  ./internal/runtime/ ./internal/documentworker/ ./internal/searchworker/ ./internal/cacheconsumer/ `
  ./internal/e2e/ ./internal/cdcacceptance/       # 全部 ok
go test -p 1 -count=1 ./internal/httpapi/ ./internal/mcp/ ./internal/config/ ./internal/redisstate/  # 全部 ok
python -m py_compile scripts/verify-debezium-e2e.py scripts/verify-kafka-search.py                    # exit 0
# .env 必填变量用占位值验证 Compose 解析
docker compose config --quiet; docker compose --profile app config --quiet                            # exit 0 / exit 0
```

- 验收套件 `internal/cdcacceptance/`：14 个用例覆盖 §11 三条链路 + 混沌（Debezium 暂停、Kafka 重复投递、worker 领取后崩溃、Redis 写失败），通过。
- `internal/e2e/` 重写后覆盖异步上传受理、搜索提交即受理与 MySQL 结果读取、DLQ 终态；写路径不再同步刷 Redis，e2e 用 `CacheSync.MarkAllDirty`/`Revalidate` 复现缓存消费者收敛。
- 集成阶段修复审查发现的小缺陷：cache worker 错误串字面量换行、`KeysForChange` 对 `op=u` 取 before+after namespace 并集、`RAG_CACHE_WORKER_RECOVER_SECONDS` 真正接入（默认 30s）、文档 DLQ group 默认值与代码一致、`docs/cdc-migration.md` §7.1 的 `document_heads` 行补全。
- 仍未验证：真实 Docker/双 Debezium 连接器/Kafka/Redis 物理链路与缓存收敛、切换/回滚演练、`--retire-outbox` 实库执行。

合并到 `main`（merge commit `bc76e6f`）后在主检出补跑：`go build ./...`、`go vet ./...` → 0；上述 14 个 Go 包测试全部 ok；前端 `npm run typecheck` → 0，`npm test` → 17 文件 225 用例全通过（Node 26.13.0 / npm 12.2.2）。前端 `npm run build` 未执行（会写 `dist/`）。

### 运行态重启验收（2026-11-13 晚间，合并后）

在 `main`（`bc76e6f` 及后续修复）上完整重建运行栈并跑通文档链路：

- Docker：`docker compose down` 后重建 mysql/kafka/redis/qdrant/pdf-parser/debezium/debezium-cache，
  删除两个 Debezium offset 卷让连接器重新首次快照（任务表 5/3 行、缓存表 354 行 `op=r`）；重放
  topic 后任务/缓存事件均为新 schema（`schedule_version`、定位列）。`pdf-parser` 用当前源码重建，
  健康检查 `converter=vision`、`version=2.0.0`。
- Go 二进制：`scripts/build-go.ps1` 重建 dist/bin 与 dist/web；`rag-service` 监听
  `127.0.0.1:8011`（`/console`、`/v1/*`、`/mcp`），`rag-document-worker`、`rag-search-worker`、
  `rag-cache-worker` 与 document/search/cache 三个 dead-letter worker 同时运行。
- 迁移：`rag-state-migrate --apply` 为 `document_jobs`/`search_tasks` 增加 `schedule_version`、
  为 `cache_revisions` 增加水位列并创建 `cdc_cutover_state`（`tables=29 schema=ready`）。
- 端到端（REST 异步受理）：`POST /v1/document-batches`（namespace=default，`cdc-demo.md`）→ 202
  accepted（`artifact_id=2e87ead1…`）→ `document_jobs` 经 Debezium 通知被 worker 领取 → `succeeded`
  （`chunk_count=1`，collection `knowledge_e0fd…`）；`GET /v1/documents` 立即可见，证明缓存连接器 +
  `rag-cache-worker` 已重建读模型。
- 手工 SQL：对同一 job 执行 `UPDATE … SET schedule_version=schedule_version+1, status='queued',
  owner=NULL, execution_id=NULL, lease_until=0, retry_at=0, attempts=0` → Debezium 捕获该 UPDATE →
  worker 重新领取并成功，验证“手工 SQL 修改也会驱动任务链路”。
- 监控：`pipeline.debezium.tasks/cache=ok`，三个 Kafka 组 `lag=0`（document 284/284、search 3/3、
  cache 4544/4544），`pipeline.cache.dirty_keys=0`、`refresh_failures=0`，`service.delivery=debezium`。
- 本轮修复：`aggregateConsumerProgress` 不再因“从未收到消息的空分区没有提交位点”把整组滞后降级为
  未知（`internal/mq/progress.go` + `progress_test.go`）；Console 上文档/搜索消费组此前一直显示
  未知，修复后均为 `ok`。
- 已知非本次改造问题：`dependencies.qdrant` 仍降级，因为回退集合
  `mini_agent_knowledge_pdf_assets_v1` 不存在（默认知识库实际使用 `knowledge_e0fd…`，全部 26 个
  `knowledge_*` 集合正常）；旧 `upload_operations` 行的工作区路径已失效，恢复协程每 30 秒打印
  `Unsafe upload workspace` 告警，不影响新上传。
- 启动方式说明：本次由 agent 会话启动本地进程，会话进程对用户目录写入受限，因此以
  `TMP/TEMP=dist\tmp`、`RAG_SERVICE_DATA_ROOT=dist\data` 启动；在普通 PowerShell 中按
  `scripts/start-stack.ps1` 启动不需要这两个覆盖。

## Debezium outbox 删除事件过滤与监控语义 — 2026-11-13

工作树 `dsh-1c1bab83-a90b`（Windows 11，Go 1.27.3，Node v26.13.0 / npm 12.2.2）。
目标：outbox 保留期清理产生的 CDC 删除事件不进入文档任务；监控不再把 outbox 的
PENDING 数量/等待时间当作未投递积压；Debezium/Kafka/文档任务/outbox 记录分开展示，
读不到的进度显示「未知」；Debezium 不回写 `PUBLISHED`。

**代码改动**

- `internal/mq/message.go`：新增 `DocumentMessageKind`（`DocumentMessageJob` /
  `DocumentMessageOutboxDeletion`）与 `DecodeDocumentMessage`；空 body（tombstone）、
  JSON null、`op=d`、仅 before-image 的事件分类为 outbox 清理，正常 schema-3 与
  c/r/u 事件不变。
- `internal/documentworker/processor.go`：`MessageHandler` 对清理事件记
  `msg="outbox cleanup event ignored"` INFO 后直接 ACK（不调用 Handle/ingester）；
  `DeadLetterMessage`/`DeadLetterHandler`/`ExhaustedHandler` 对清理事件返回已完成，
  DLQ 重放不会改任何任务。`GCOutbox` 与业务文档删除未改。
- `internal/state/monitor.go` + `internal/monitor/monitor.go`：`queues` 移除
  `document_outbox`；新增恒在 `pipeline`（`outbox` 记录统计 / `document_jobs` /
  `debezium` / `kafka`）；`oldest_pending_seconds` 删除，改为全状态
  `oldest_record_seconds`；`capture_progress` 恒为 null（应用读不到 binlog 位点）。
- `internal/mq/progress.go`：`DocumentConsumerProgress` 用 kafka-go 读取主题
  Metadata→ListOffsets→OffsetFetch，得到已提交/末尾位点与滞后；无提交位点时全部
  null（未知）。
- `cmd/rag-service/main.go`：注入 `RAG_DEBEZIUM_HEALTH_URL`、文档 topic/group 与
  Kafka 进度闭包。
- `frontend/src/{MonitorPage.tsx,MonitorPage.test.tsx,api.ts}`：监控页拆成
  「文档任务」「Outbox 记录（明确注明不代表未投递积压）」「投递与消费进度
  （Debezium 采集 / Kafka 消费 / 文档任务状态）」。
- 部署：`docker-compose.yml` 发布 Debezium 健康端口
  `127.0.0.1:${RAG_DEBEZIUM_PORT:-8083}`，app profile 注入
  `RAG_DEBEZIUM_HEALTH_URL=http://debezium:8080/q/health`；`.env.example`、
  `docs/USAGE.md`、`docs/DEPLOYMENT.md`、`deploy/debezium/README.md`、
  `docs/agents/*`、`README.md` 同步说明。

**定向测试**（仅相关包，未跑全量）

```
$ go build ./...                                  # exit 0
$ go vet ./internal/mq ./internal/documentworker ./internal/monitor ./internal/state ./cmd/rag-service
                                                  # exit 0
$ RAG_TEST_MYSQL_URL=mysql://rag:<env>@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4 \
  go test -p 1 -count=1 -timeout 15m ./internal/state ./internal/monitor ./internal/mq ./internal/documentworker
ok  internal/state 20.200s   ok  internal/monitor 2.451s
ok  internal/mq 0.681s       ok  internal/documentworker 2.468s

$ cd frontend && npx tsc --noEmit                # exit 0
$ npx vitest run src/MonitorPage.test.tsx src/api.test.ts   # 38 passed
```

新增/更新的关键用例：CDC 删除/tombstone 分类、清理事件 ACK 且不触发
ingester、消费者对清理事件只提交一次且不重试/不写 DLQ、已完成任务在 outbox 被
`GCOutbox` 删除后重放删除事件仍为 succeeded、正常通知仍入库、监控四视图
（succeeded + 仍 PENDING 的 outbox 仍为 ok；Debezium/Kafka 未知为 null；stats 失败时
`queues`/`pipeline.outbox` 为 null 而进度探针仍在）、JSON wire-shape、前端
`不代表未投递积压`/`采集进度：未知`/`消费滞后：未知` 文案。

**实机验收**（Compose 基础设施 + 本次构建的 `dist/bin/rag-service.exe` /
`rag-document-worker.exe`，脚本 `scripts/verify-debezium-e2e.py`，2026-11-13）

```
SUMMARY passed=37 failed=0
PASS document job reached succeeded via debezium delivery (stage=completed)
PASS outbox row remained PENDING after processing (only Debezium published it)
PASS outbox retention cleanup deleted the expired record (rows=1)
PASS outbox record is gone from MySQL after cleanup
PASS worker classified the CDC delete event as outbox cleanup and ACKed it
PASS delete event did not take the malformed/retry path (no invalid retry)
PASS worker did not exhaust retries for the cleanup event
PASS document DLQ has no record for the cleaned-up outbox event
PASS consumer kept working after the cleanup event (stage=completed)
PASS original document job still succeeded after its outbox record was removed
PASS document list / REST search still contains the original document
PASS business document delete still works (HTTP 200)
PASS business delete removed only the requested document; the cleaned-up outbox document remains
PASS queues no longer presents document_outbox as a backlog queue
PASS monitor response no longer reports oldest_pending_seconds
PASS outbox record stats are self-consistent (total=1, by_status={'PENDING': 1})
PASS document job status counts successes only (by_status={'failed': 0, 'processing': 0, 'queued': 0, 'succeeded': 2})
PASS succeeded jobs stay ok while retained outbox rows are PENDING (not misreported)
PASS Debezium capture progress is reported as unknown (null), never fabricated
PASS Debezium health probe reports ok
PASS Kafka progress names the document topic/group; lag=0 (status=ok)
```

流程：MCP 受理 → Debezium 投递 → 文档 `succeeded` → 直接删除 MySQL 中对应
outbox 行（模拟保留期清理）→ Debezium 投递 `op=d` 事件 → worker 记录固定 INFO
并 ACK → 再提交第二篇文档仍成功 → 原文档仍在列表/检索结果中 → REST 删除第二篇
文档仍返回 200 且只删除目标文档（业务删除路径未受影响）→
`/v1/admin/monitor/overview` 的 `pipeline` 语义符合上述契约。

说明：为排除同组旧消费者干扰，验收期间临时停止了宿主开发栈中旧的
`rag-document-worker.exe` 与文档死信 worker（它们是修复前二进制，会先消费同一
topic/group），完成后已用原 `dist/bin` 二进制 detached 恢复；父检出 `.env` 早于
Kafka 改造，恢复时显式补了 `RAG_KAFKA_BOOTSTRAP_SERVERS=127.0.0.1:9092`。合并后
请重建并重启这两个进程，才能让运行中的开发栈获得删除事件过滤能力。

## Console create-knowledge-base multi-column layout — 2026-11-10

Delivered: the Console「创建知识库」page no longer stacks every field vertically.
The widest form grows from 720 px to 1080 px and arranges fields in rows:

- Admin form: namespace input capped at 460 px; Embedding / Reranker / VLM cards
  share a three-column grid (three columns ≥1180 px, two 768–1179 px, one
  ≤767 px) under a single top rule; the text-chunking fields
  (切分方式 / 块长度 / 重叠长度) share a three-column row (three ≥1024 px, two
  768–1023 px, one ≤767 px).
- VIP/user form: namespace, strategy, chunk size and chunk overlap form a 2×2
  two-column grid (single column ≤767 px).
- The edit-knowledge-base drawer (720 px, 知识库列表 → 配置) keeps the original
  vertical layout so selects are not squeezed; field set, validation and submit
  payloads are unchanged (only layout wrappers were added).

Frontend verification (worktree `dsh-f2edde66-add5`, Windows 11, Node v26.13.0 /
npm 12.2.2):

```
$ npm run typecheck
exit 0, no diagnostics

$ npm test
Test Files  5 passed (5)
Tests       54 passed (54)

$ npm run build
../build/web/assets/index-BTOMzEre.css      14.84 kB │ gzip:   3.54 kB
../build/web/assets/index-DBnJ_9e6.js    1,251.47 kB │ gzip: 395.34 kB
✓ built in 2.67s   (only the pre-existing >500 kB chunk advisory)
```

Vite 8's Windows mapped-drive probe (`child_process.exec("net use")`) is rejected
by this shell sandbox (pipe EPERM), so the checks above ran with a temporary
`NODE_OPTIONS` shim that only makes that probe fail cleanly; the shim and the
local preview scripts were deleted afterwards and are not part of the change.

Visual check with headless Chrome 145 (CDP `Emulation.setDeviceMetricsOverride`
for exact viewports; the production bundle was served by a temporary local proxy
in front of the running `127.0.0.1:8011` service):

- 1440 px, admin form, draft pre-selected with the saved `default` profiles:
  provider grid = 3 × 344 px, chunk row = 3 × 344 px, form = 1080 px,
  `documentElement.scrollWidth` = 1440 (no overflow). All three model cards keep
  the same height although the selected option labels wrap to two lines.
- 1024 px, admin form: provider grid = 2 × 374 px (VLM wraps to the second row),
  chunk row stays 3 columns.
- 390 px, admin form: single column, form = 358 px,
  `documentElement.scrollWidth` = 390 (no horizontal overflow).
- 1440 px, VIP/user form (temporary registered `user` account): four fields in a
  2×2 grid, model-template alert spanning the form width.
- 720 px drawer with the existing knowledge base `asdjasdajbsc`: unchanged
  vertical Embedding / Reranker / VLM sections and 2-column chunk controls.

Screenshots: `.test-tmp/create-kb-layout/` (git-ignored, not part of the change).
The temporary preview account used for the VIP/user screenshot was removed after
the check (`DELETE /v1/admin/users/{id}` → 204); no persistent data was changed.

Known limits: the repository has no Playwright/Cypress, so this is a manual
headless-Chrome review rather than an automated browser regression; only the
create page was changed, the edit drawer is intentionally still single-column.

## Go-only parser workers (Python/Docling removed) — 2026-11-10

Delivered: the compatibility layer is gone. PDF text extraction, DOCX reading
and Word picture descriptions all run in the Go process; `RAG_PDF_ENGINE`,
`RAG_PDF_PARSER_PYTHON`, `RAG_PDF_MODELS_PATH` and `RAG_PDF_PARSE_TIMEOUT` were
removed, `ExternalPDFParser`/`CommandRunner` were deleted, `ingest.PDFOptions`
now only carries an optional `Parser` (default `document.NativePDFParser`), and
the manifest version written by new imports is `go-native-v1` (stored manifests
are still readable because readers do not re-check the version).

The Word vision worker became a Go HTTP client: `document.WordVisionWorker`
decodes the picture (png/jpeg/gif/bmp/tiff via `golang.org/x/image`), scales it
to a 384-pixel edge cap, re-encodes PNG and calls the knowledge base's
OpenAI-compatible `/chat/completions` endpoint with the fixed description
prompt (max_tokens 256). Transient 429/5xx responses are retried three times,
refusal/generic answers are rejected by the existing filter, and descriptions
are cleaned and persisted into the manifest. Without a configured VLM the
pictures keep placeholder descriptions and the upload still succeeds; request
timeout comes from the VLM settings (120 s default, 30 s floor).

Also deleted: `python-workers/` (Docling PDF worker, Word vision worker, Python
package) and `benchmarks/` (Docling evaluation scripts). README, USAGE,
DEPLOYMENT, GO_MIGRATION, `.env.example`, `deploy/config.container.toml` and the
startup config template were updated to the Go-only pipeline. `scripts/verify-*.py`
remain as optional infrastructure test tooling, not runtime code.

Automated checks (worktree `go-only-parsers`, uncommitted at verification time):

- `go build ./...` and `go vet ./...` — clean.
- `go test ./internal/document ./internal/ingest ./internal/config ./internal/document/pdfcontent ./internal/chunking -count=1` — pass. New `internal/document/vision_test.go` covers the httptest VLM success path (image data URL, bearer auth, prompt, 384-px scaling), captioned-picture skip, missing endpoint/model, refusal rejection, 503 retry (3 attempts), HTTP 400 failure, request timeout, undecodable image and manifest handling. The ingest Word-vision integration test now uses an httptest VLM server and a real JPEG fixture.
- `go test ./internal/runtime -run 'Upload|Pdf|PDF|Word|Vision' -count=1` — pass (231 s), including the byte-identical replay and PDF chunking paths.

Live end-to-end check (service on `127.0.0.1:8011`, MySQL `praxis_rag`, KB `asdjasdajbsc`, real embedding endpoint `http://aihub-api.goertek.com:30080/v1` + `bge-m3`):

- `POST /v1/documents` with Chinese `it-manual-cn.pdf` → `200` in 70.6 s, `failed: []`, 9 chunks, `pdf_parser_version = go-native-v1`; no Python/Docling error in the log.
- `DELETE /v1/documents/it-manual-cn.pdf` → `200` (9 chunks deleted); follow-up `GET /v1/documents` no longer lists it and `GET .../content` returns `404 Document does not exist`.
- Re-uploading `2404.16130v2.pdf` → `200`, `COMMITTED`, 35 chunks, stored manifest `pdf-assets/doc-850325c2ab92aea47056c511dd37528b/manifest.json` now contains `go-native` (26 pages / 349 blocks / 89,856 characters), `upload_operations.error = NULL`; Console `:8011/console` still returns 200.
- After merging the concurrent async-upload change from `main` (default `RAG_UPLOAD_MODE=async`, in-process document worker), the same Chinese PDF was uploaded again through the default async flow: `POST /v1/documents` → `200` with `mode:"async"` and `jobs:[{artifact_id:"1feb3339-3f1e-4ba1-a5af-cde957e2a0b7",status:"queued"}]`; polling `GET /v1/document-jobs/{id}` walked `queued → parsing → chunking → staging → embedding → indexing → publishing → succeeded`; `DELETE /v1/documents/it-manual-cn.pdf` → `200`, and the document list no longer contains it (the read view refreshes within its 60-second TTL). `go test ./internal/runtime -run 'Upload|Pdf|PDF|Word|Vision|Async'` passes on the merged tree (227.8 s), as do `go build ./...`, `go vet ./...`, and `document`/`ingest`/`config`/`pdfcontent` tests.

Known gaps: the Go PDF engine does not produce layout analysis, table structure,
OCR, formula or picture assets (two-column PDFs can interleave columns; scans
need OCR first). Word picture descriptions depend on the remote VLM and were only
tested against httptest servers, not a real endpoint. `.doc` conversion still
needs external LibreOffice.

> **Superseded design note:** the section below (“In-process Go PDF engine”) was
> written earlier the same day when `RAG_PDF_ENGINE=docling` was still an
> optional fallback; the compatibility layer described there was removed by this
> change.

## In-process Go PDF engine — 2026-11-10

Delivered: PDF upload no longer requires a Python/Docling installation. The new
`document.NativePDFParser` (`internal/document/pdf_native.go`, reader
`github.com/ledongthuc/pdf`) implements both `ingest.PDFParser` and
`document.PDFPages`, walks each page content stream, groups runs into visual
lines, merges superscripts, removes headers/footers/page numbers and TOC dot
leaders, normalizes ligatures, repairs hyphenated line breaks, keeps CJK text
unspaced and only sorts runs by X when the reader exposes glyph widths (so
zero-width CJK CID fonts keep the content-stream order). It writes the same
`docling-formula-remote-v3` manifest (no chunk/metadata migration) with
`parser.engine=go-native` and page/block/character/elapsed metrics.

`RAG_PDF_ENGINE` (`go` default, `docling` optional) selects the engine; when
unset, an existing `RAG_PDF_PARSER_PYTHON` keeps Docling, so old deployments are
unchanged. Docling remains the only engine with layout analysis, table
structure, OCR, formula recognition and picture descriptions.

Automated checks (worktree `go-pdf-parser`, commit `27b1659`):

- `go build ./...`, `go vet ./internal/document ./internal/config ./internal/runtime ./internal/ingest` — clean.
- `go test ./internal/document ./internal/config ./internal/ingest -count=1` — pass. New cases: generated valid PDF (manifest version/blocks/order/`Pages`/`manifest.json`), broken input → `*PDFParseError`, textless PDF → “没有可索引的内容”, ligature/page-number/dehyphenation/CJK/spacing heuristics, `RAG_PDF_ENGINE` parsing/defaults, and runtime engine selection (`TestUploadPDFOptionsSelectsEngine`).
- `go test ./internal/runtime -run 'Upload|Pdf|PDF' -count=1` — pass (upload/PDF-related runtime tests, including `TestSafeUploadErrorTextPdfParseMessage`).

Live end-to-end check (service on `127.0.0.1:8011`, MySQL `praxis_rag`, KB `asdjasdajbsc`, embedding `http://aihub-api.goertek.com:30080/v1` + `bge-m3`):

- `POST /v1/documents` (multipart `files=2404.16130v2.pdf`, header `X-RAG-Namespace: asdjasdajbsc`) → `200` in 48.5 s, `failed: []`, `imported[0].chunk_count = 35`, no `pdf_parser` error in the server log.
- Stored manifest (`blobs` `pdf-assets/doc-850325c2ab92aea47056c511dd37528b/manifest.json`): `version=docling-formula-remote-v3`, `parser={"engine":"go-native","library":"github.com/ledongthuc/pdf"}`, `metrics={pages:26, blocks:349, characters:89856, elapsed_ms:2082}`, `assets=0`, `warnings=0`; first blocks are the paper title/author lines.
- `upload_operations` row `2c37cfc8ce1846ef67a1b844d0330905` = `COMMITTED`, `error NULL`; steps `receive_source → deduplicate → chunking_tokenizer → stage_mysql → embedding → sparse → stage_qdrant` all `DONE`.
- `POST /v1/search` (`rerank:"none"`, KB has no reranker configured) returns the PDF chunks (top hit score 0.75, GraphRAG title text), and `GET /v1/documents/2404.16130v2.pdf/content` serves the original PDF bytes. `GET /console` and the Vite dev server (`:5173`) return 200.

Parser quality spot-checks (before the live run): arXiv `2404.16130v2.pdf` → 26 pages / 349 blocks / 89,856 chars, correct title/authors/headings/paragraphs; Chinese `新员工IT手册.pdf` → 23 pages / 28 blocks / 15,718 chars with correct CJK reading order after the width-availability fix. Known gaps: two-column PDFs can interleave columns, tables/figures/formulas become plain text (no assets), scanned pages need OCR and surface as empty-content/no-text warnings — use `RAG_PDF_ENGINE=docling` for those.

## 异步上传默认化与内嵌文档 worker — 2026-11-10

Delivered: `POST /v1/documents`（网页 Console 与 REST）默认改为异步受理：每个文件在状态库单事务写入 `document_jobs` + `document_job_outbox` 后立即返回 `{"mode":"async","jobs":[...],"failed":[...]}`，解析/切分/Embedding/Qdrant 发布由文档 worker 在后台完成；`RAG_UPLOAD_MODE=sync` 保留为显式 opt-in 的旧同步行为。无 Kafka 的单进程部署由 `rag-service` 内嵌文档 worker 直接排空已提交的发件箱（`RAG_DOCUMENT_WORKER_INPROCESS`：未设置/`auto` = 未配置 Kafka 时启用；`1/true/on` = 强制启用，配置 Kafka 时拒绝启动；`0/false/off` = 关闭），轮询间隔 `RAG_DOCUMENT_WORKER_POLL_SECONDS`（默认 0.5 秒）。`documentworker.WithOutboxReceiver` 统一 service 内嵌 worker、独立 worker 本地模式与 REST 异步 e2e 的接线；`scripts/start-stack.ps1` 现在随 `rag-service` 启动/停止 `dist/bin/rag-document-worker.exe`。OpenAPI、`.env.example`、README、USAGE、DEPLOYMENT、PROGRESS 同步更新。

Verification (Windows, Go 1.27.3, MySQL 8 on 127.0.0.1:3307 with the isolated `praxis_cache_test_smoke` schema, Qdrant/Redis from the local stack; `GOCACHE`/`GOTMPDIR` redirected into the worktree because the machine caches are outside the sandbox):

- `go build ./...` → exit 0；`go vet ./internal/... ./cmd/...` → exit 0。
- 定向测试：`go test ./internal/config ./internal/httpapi ./internal/documentworker ./cmd/rag-service -count=1` → 全部 `ok`（config 0.4 s、httpapi 145.8 s、documentworker 270.2 s、cmd/rag-service 0.3 s；本轮机器负载较高，耗时高于包内基线）。
- 全量串行套件：`go test ./... -p 1 -count=1 -timeout 40m` → exit 0，25 分 05 秒，所有含测试的包 `ok`，无 `--- FAIL`/`--- SKIP`；重点包 `internal/state` 429.8 s、`internal/runtime` 471.1 s、`internal/e2e` 319.9 s（含 `TestRESTAsyncUploadPipeline`）、`internal/httpapi` 131.1 s、`internal/documentworker` 55.2 s、`internal/access` 20.3 s、`cmd/rag-service` 0.3 s。
- 前端（本次未改前端，仅回归）：`npm run typecheck` 通过；`npm test` 54/54（5 个文件）；`npm run build` 通过（仅既有 >500 kB chunk 提示）。
- 实机冒烟（`cmd/rag-service` 新二进制监听 127.0.0.1:18099，`praxis_cache_test_smoke` 状态库，模拟 OpenAI 兼容服务每次 embedding 延迟 2000 ms）：8/8 检查通过 —— `POST /v1/documents` 在 380 ms 内返回 `mode:"async"` 且 job `status:"queued"`（远小于 2000 ms 的解析延迟）；受理后立即读取 job 为 `queued`/`processing`，此时 `GET /v1/documents` 尚未包含该文件、`/health` 与文档列表均在 1 s 内响应；服务日志显示 `document_worker=in-process`、`poll_seconds=0.5`；后台任务最终 `status:"succeeded"`、`stage:"completed"`，随后文档出现在列表中且正文可读。该冒烟同时证明默认值（不设置 `RAG_UPLOAD_MODE` / `RAG_DOCUMENT_WORKER_INPROCESS`，不配置 Kafka）即可得到期望行为。
- 与 main 的 PDF 内嵌引擎改动（`27b1659`/`bffdcf4`）合并后复验（rebase 到 `bffdcf4`，工作区模块缓存 `GOMODCACHE` 指到 worktree 以便拉取新 PDF 依赖）：`go build ./...` 通过、`go vet ./internal/... ./cmd/...` 通过；`go test ./internal/config ./internal/httpapi ./internal/document ./internal/documentworker ./cmd/rag-service -count=1` 全部 `ok`（config 3.2 s、httpapi 37.0 s、document 1.9 s、documentworker 67.1 s、cmd/rag-service 0.2 s）；`go test ./internal/e2e -run TestRESTAsyncUploadPipeline -count=1` ok（86.6 s）；`go test ./internal/runtime -run 'Upload|Pdf|PDF' -count=1` ok（222.6 s）；用合并后的二进制重跑上节实机冒烟 8/8 通过（上传 389 ms 返回）。

Known limits: `scripts/start-stack.ps1` 的子进程生命周期只做了 PowerShell 语法解析（`[Parser]::ParseFile`），沙箱内无法启动 Docker/Kafka 前台子进程做端到端验证；真实浏览器级 Console 交互仍无自动化（仓库无 Playwright/Cypress），异步轮询 UI 由 Vitest 契约测试与 REST 异步 e2e 覆盖。`rag` MySQL 用户无 `CREATE DATABASE` 权限，新 schema 无法创建，冒烟复用了由测试套件迁移过的 `praxis_cache_test_smoke` 并使用唯一 namespace。行为变更：不再设置 `RAG_UPLOAD_MODE` 的部署从同步变为异步，必须有文档消费者（无 Kafka 自动内嵌；Kafka 需 `rag-document-worker` 或显式 `RAG_UPLOAD_MODE=sync`）。

## Per-account API-key ownership and isolation — 2026-11-10

Delivered: every API key now records the Console account that issued it
(`api_keys.owner_user_id`, nullable for legacy rows; additive migration +
`ix_api_keys_owner`), and the five existing `/v1/admin/api-keys` routes move to
the new `authSessionOrLocal` policy: any signed-in role manages only its own
keys (cookie writes need `X-CSRF-Token`, Bearer sessions are exempt), trusted
loopback keeps the unowned/break-glass behavior, and foreign keys are
indistinguishable from missing (404 `API Key 不存在`). Non-administrator
sessions may bind only knowledge bases they own (`access.Service.OwnedNamespaces`;
403 `只能为本人创建的知识库创建 API Key`) because API keys bypass
knowledge-base/document authorization; updates validate only newly added
namespaces so a demoted administrator can still rename, disable or delete keys.
`/v1/namespaces` gains the additive `owned_namespaces` field, administrator
listings join `users` for `owner_username`, account deletion cascades owned keys
in the same transaction (and bumps the `api_keys` read-model revision). The
Console API-key page is open to all roles, shows the owner column for
administrators and limits the namespace picker to owned knowledge bases. Docs
updated: [PERMISSIONS](docs/PERMISSIONS.md) §9/§10, [api-keys](docs/api-keys.md),
[USAGE](docs/USAGE.md), README, [DEPLOYMENT](docs/DEPLOYMENT.md),
[state-cache](docs/state-cache.md), PROGRESS.

Verification (Windows, Go 1.27.3, MySQL 8 + Redis + Qdrant from the local compose
stack; `GOCACHE` redirected into the worktree because the machine cache is
outside the sandbox):

- `go build ./...` / `go vet ./...` — clean.
- New regression tests: `internal/state/api_keys_owner_test.go` (owner
  projection, `ListByOwner` scoping, legacy NULL rows, update keeps owner,
  account-delete cascade), `internal/httpapi/api_keys_owner_test.go`
  (list/read/update/delete/mcp-config isolation, 404 indistinguishability,
  CSRF/bearer/loopback policy, create/update binding rules, no-store, admin
  visibility), `internal/e2e/api_keys_http_test.go` (real router + sessions +
  CSRF + auth/access/MySQL end to end, admin ownership, `owned_namespaces`).
- Targeted rerun after the review fixes: `go test -p 1 -count=1 -timeout 900s
  -run 'TestAPIKey|TestUserDeleteCascadesAPIKeys|TestSchemaMatchesPythonDefinition|TestAutoSchemaCanBeDisabled|TestSchemaCreationCanBeForcedOff|TestStoresWiresG7StateMachines|TestDocumentJobReconcileCommittedPublication'
  ./internal/state ./internal/httpapi ./internal/e2e` → `ok` state 106.2 s,
  httpapi 23.5 s, e2e 9.6 s.
- Frontend: `npm run typecheck` clean; `npm test` 32/32 (5 new API-key helper
  contract tests); `npm run build` green.
- Full serial suite: `go test ./... -p 1 -count=1 -timeout 25m` → exit 0, every
  package passes (state 421.0 s, httpapi 182.9 s, e2e 252.9 s, runtime 464.7 s,
  documentworker 54.5 s; ≈24 minutes wall time on this loaded machine).

Environment note: an earlier full-suite attempt reported only two G7 tests
failing with `Table 'praxis_gotest.document_task_outbox' doesn't exist` while
another worktree's Go test process was also using the shared `praxis_gotest`
schema (observed `state.test` from
`.agents/worktrees/dsh-200abc8a-f229`). `newTestStores` runs schema
creation/migration before `statetest.Prepare` takes its `GET_LOCK`, so a
concurrent process that drops/recreates the schema is visible in that window —
a pre-existing harness race, not a regression from this change. The green run
above was executed with no other test process on the schema; a private schema
could not be created because the `rag` MySQL user has no `CREATE DATABASE`
grant.

Known limits (also in [PERMISSIONS](docs/PERMISSIONS.md) §10): disabling an
account revokes its sessions but not its already-issued keys (delete the account
or disable/delete the keys); the namespace binding stores the knowledge-base
name, so a deleted-then-recreated knowledge base remains reachable by keys
previously issued for that namespace.

## Model configuration page and model list — 2026-11-10

Delivered: a Console-only 模型配置 page (`#/model-profiles`, administrators
only) that maintains reusable Embedding / Reranker / VLM connections; the MySQL
`model_profiles` table (`kind` + `name` unique, `base_url`, `model`, encrypted
`protected_key`, probed `vector_size`) with keys protected by
`RAG_STATE_ENCRYPTION_KEY`; the seven `/v1/admin/model-profiles` routes (list,
create, details, PATCH, delete, `/key`, `/models`, local admin or administrator
session only); and a create/edit knowledge-base form that quick-selects a saved
model instead of typing the base URL, key and model id. The knowledge-base REST
contract is unchanged — the Console copies the selected profile's connection
into the existing inline provider objects. Profiles and knowledge bases are
decoupled: editing or deleting a profile never mutates a knowledge base that
already copied its connection, and an Embedding selection change still requires
the re-embedding confirmation.

Environment: Windows 11 host, Go 1.27.3 windows/amd64, Node v26.13.0 / npm
12.2.2, MySQL 8 on 127.0.0.1:3307 (isolated schema `praxis_cache_test_smoke`),
Qdrant/Redis from the local compose stack. Two workspace-sandbox accommodations:
`GOCACHE`/`GOTMPDIR` redirected inside the worktree, and Vite/Vitest run with a
`child_process.exec` preload shim because the sandbox refuses Vite's piped
`net use` probe. All Go test output below is from the isolated schema; a
concurrent DSH session on the same machine was running its own Go tests against
the shared `praxis_gotest` schema (leftover `state.test.exe` processes), which
caused DDL races in unrelated schema tests — see the note at the end.

Go verification (cwd worktree):

```
$ go build ./...
exit 0 (only the pre-existing sandbox warning "writing stat cache: ... D:\go-work\pkg\mod\cache ... Access is denied")

$ go vet ./internal/...
exit 0, no output

$ go test ./internal/kbconfig ./internal/httpapi -count=1 -timeout 600s
ok   github.com/Guan0923/xinghe_rag/internal/kbconfig   0.343s
ok   github.com/Guan0923/xinghe_rag/internal/httpapi    11.340s

$ go test ./internal/state -run 'TestModelProfile' -count=1 -timeout 600s
ok   github.com/Guan0923/xinghe_rag/internal/state      35.485s

$ go test ./internal/runtime -run 'TestModelProfile' -count=1 -timeout 600s
ok   github.com/Guan0923/xinghe_rag/internal/runtime    79.162s

$ go test ./internal/e2e -count=1 -timeout 1200s
ok   github.com/Guan0923/xinghe_rag/internal/e2e        306.471s

$ go test ./internal/state -count=1 -timeout 1800s        # full package, isolated schema
ok   github.com/Guan0923/xinghe_rag/internal/state      426.978s
```

Console verification (cwd frontend, NODE_OPTIONS shim set):

```
$ npm run typecheck
exit 0, no diagnostics

$ npm test
Test Files  4 passed (4)
Tests  49 passed (49)
     src/navigation.test.ts (11 tests)
     src/api.test.ts (24 tests)
     src/CreateKnowledgeBase.test.ts (7 tests)
     src/ModelProfilesPage.test.tsx (7 tests)

$ npm run build
../build/web/assets/index-CW9lPsz8.js   1,250.64 kB │ gzip: 395.07 kB
../build/web/assets/index-CsXgnAdA.css     13.51 kB │ gzip:   3.38 kB
✓ built in 808ms
exit 0 (only the pre-existing >500 kB chunk advisory)
```

Live HTTP smoke (built `cmd/rag-service` from this worktree on 127.0.0.1:18099,
isolated MySQL schema, loopback admin calls without a session, mock
OpenAI-compatible provider on 127.0.0.1:18077; 14 checks, all PASS):

```
GET  /v1/admin/model-profiles empty list
POST embedding profile -> probes provider, records vector_size=4, api_key_set=true, key absent from payload
GET  list -> no key leaked;  GET /{id}/key -> returns sk-smoke
POST /{id}/models with no request body -> provider models (3)
PATCH rename -> 200, no re-probe, vector_size kept
duplicate (kind,name) -> 409;  missing base_url -> 400;  unknown kind -> 422 literal_error
POST /v1/admin/knowledge-bases using the saved connection -> 201, vector_size=4, embedding_model=mock-embedding
PATCH knowledge base with the same connection, reembed=false -> 200 (no re-embedding required)
DELETE -> 204;  DELETE unknown id -> 404
GET /openapi.json exposes /v1/admin/model-profiles, /{profile_id}, /{profile_id}/key, /{profile_id}/models
SMOKE OK (14 checks)
```

Coverage added:

- `internal/state`: repository lifecycle (create / list / list-by-kind / get /
  update / delete, generated id and timestamps), duplicate `(kind, name)` →
  conflict, table shape.
- `internal/kbconfig`: draft validation (kind, name/model length limits, service
  URL required, trimming), key preservation (an omitted key keeps the stored one
  while the base URL is unchanged; changing the URL without a key or an explicit
  empty key clears it), probe routing (embedding records the dimension, reranker
  gets one bounded call, VLM is not probed), credential-free public projection,
  encryption round-trip.
- `internal/runtime`: profile lifecycle through the service, error mapping
  (409 conflict, 404-shaped missing profile, 503 state failure).
- `internal/httpapi`: list/details/key/models, create (201), PATCH/delete
  (200/204), FastAPI-shaped 422 validation bodies, 404/409 mapping, admin-only
  and runtime-unavailable guards, OpenAPI route table.
- Console: 49 tests total. `ModelProfilesPage.test.tsx` (7 tests: per-kind
  counts/rows/saved-key state, kind filtering and per-kind empty hints,
  informative empty state, add-drawer required-field validation, create with
  the default kind, explicit empty `apikey` on PATCH clears the saved key,
  untouched key field omits `apikey` on PATCH), `CreateKnowledgeBase.test.ts`
  (7 tests for `matchingProfile` / `profileNeedsReembed`), `api.test.ts` (24
  tests; model-profile client contract: frozen kind vocabulary, list
  normalization, create body, PATCH by encoded id, DELETE empty-body path,
  key/model routes, duplicate conflict detail) and `navigation.test.ts` (11
  tests; `#/model-profiles` parse and hash round-trip).

Not verified live: browser E2E of the new page was not run (no
Playwright/Cypress in this repository); provider probes were exercised against
a mock OpenAI-compatible endpoint plus the unit-test fakes, not a real model
service.

Shared-schema caveat: the first full `go test ./internal/state` attempts on the
shared `praxis_gotest` schema failed in `TestAutoSchemaCanBeDisabled`,
`TestSchemaCreationCanBeForcedOff` and `TestMigrationsAddMissingLegacyColumns`
because a concurrent DSH session on the same machine was running its own Go
tests against the same schema (observed leftover `state.test.exe` processes
performing DROP/ALTER). The same three tests pass when run alone, and the full
package is green on the isolated schema above; none of them exercise
model-profile code.

## Test workflow hardening — 2026-11-10

Goal: make the repository's test workflow actually reliable, and cover the
Console-facing roles/ownership/sharing feature end to end.

- **Fixed a real bug the suite had been hiding.** `TestRESTAsyncUploadPipeline`
  was already red on the untouched `8df52da` baseline: the asynchronous publish
  path committed the document row plus the upload journal in one transaction but
  refreshed only the upload read models. With the Redis cache attached the
  `documents`/`kb_list` revisions stayed stale, so `GET /v1/documents` answered
  from an empty cached snapshot right after the worker published. The publish
  path now calls the new `state.DocumentRepository.FlushViews` after commit
  (`internal/runtime/documents_publish.go`); the e2e test and the full suite are
  green.
- **New full-stack HTTP e2e**: `internal/e2e/permissions_http_test.go` (≈46 s)
  runs the real `httpapi` router with real cookie sessions + CSRF, the real
  `auth` and `access` services and the real MySQL state over the harness' fake
  model/Qdrant servers. It covers: role management (only admins; normal users
  cannot open the picker), ownership visibility before/after a grant, invitation
  create → inbox → preview (wrong account 403) → accept, read-only list/delete/
  upload gates, non-owner and non-VIP invite refusals, the write grant +
  upload + document-list path, template-based self-creation with recorded
  ownership, grant listing and revocation.
- **Console unit/contract tests**: Vitest + jsdom with a worker-thread pool
  (`pool: "threads"`; forked workers are unavailable in this sandbox and are not
  needed). `frontend/src/api.test.ts` (17 tests) pins the api client contract —
  namespace/bearer headers, CSRF on writes only, the 401 handler scoping, error
  detail formatting, sharing endpoint URLs/bodies and null-list normalization.
  `frontend/src/navigation.test.ts` (10 tests) pins hash-route parsing, invite
  token parsing and namespace validation. `npm test` → **27/27 pass**;
  `npm run typecheck` and `npm run build` stay green.
- **Runner reliability**: `-p 1 -count=1 -timeout 25m` is now baked into
  `scripts/build-go.ps1 -Test` and `scripts/build-go.sh test` and documented in
  README/USAGE; `make test`, `make test-frontend` and `make check` added.
  Every package truncates the same MySQL schema behind `GET_LOCK(...,600)`, so
  parallel packages queue past the default 10 m package timeout — serial is not
  a preference, it is required.
- **CI**: `.github/workflows/test.yml` runs `go vet`/`go build`/`go test -p 1`
  with a MySQL 8 service container and a separate frontend
  `typecheck + npm test + build` job.
- **Full serial suite**: `go test ./... -p 1 -count=1 -timeout 25m` → every
  package passes, exit 0, ≈16 minutes total (`internal/e2e` 205.4 s,
  `internal/runtime` 462.5 s, `internal/state` 400.6 s, `documentworker` 53.3 s).
  The previously failing e2e case is part of that green run.

## Roles, knowledge-base ownership and invitation permissions — 2026-11-09

Delivered: `admin` / `vip` / `user` roles (only administrators change roles),
knowledge-base ownership (`knowledge_bases.owner_user_id`), invite-link sharing
with read/write permissions over single-file / multi-file / whole-knowledge-base
scopes, and enforcement across REST, MCP, document lists, search results and
artifacts. Design/spec: `.dev/DESIGN.md` (local scratch); user docs:
[docs/PERMISSIONS.md](docs/PERMISSIONS.md).

Go verification (Windows, Go 1.27.3, MySQL 8 + Redis + Qdrant from the local
compose stack; `GOCACHE` redirected into the worktree because the machine cache
is outside the sandbox):

- `go build ./...` and `go vet ./internal/...` — clean.
- `go test ./... -p 1 -count=1 -timeout 25m` — every package passes except
  `internal/e2e TestRESTAsyncUploadPipeline`, which also fails on the pristine
  `8df52da` baseline in a separate worktree (pre-existing, unrelated to this
  iteration: `documents after publish = []`).
- New coverage: `internal/access` (scope resolution, invitation lifecycle,
  expiry/revoke/idempotent accept, validation, 20s), `internal/state`
  (permission/invitation repositories, ownership, delete cascades),
  `internal/httpapi` (role checks, `authSession`/CSRF, ownership 403s, document
  filtering, search `DocumentIDs`, invitation endpoints, 409 template mapping),
  `internal/runtime` (template clone, restricted-search visibility filter,
  `ResolveDocumentIDs`), `internal/mcp` (per-request scope through the internal
  `X-Rag-Mcp-Scope` header, resource/tool/write filtering, queued-search 403).
- Full `internal/state` suite: 403.9s green; `internal/runtime`: 458.4s green.

Console verification:

- `npm run typecheck` — clean.
- `npm run build` — clean (1,239.66 kB JS / 13.41 kB CSS). Vite 8 aborts in this
  sandbox because it probes Windows network drives through `child_process.exec`
  (pipe EPERM); the build was verified with an uncommitted `child_process.exec`
  shim via `NODE_OPTIONS`, which only affects the probe.

Live HTTP smoke (`dist/bin/rag-service.exe` on the disposable MySQL schema,
Qdrant/Redis from the stack; 35 assertions): roles and role updates, admin-only
user picker, 409 for self-create without a template, owned/unowned namespace
visibility, invitation create → inbox → preview (wrong account 403) → accept,
whole-KB read vs. single-file write effective access, read-only upload/delete
403, grant listing/revoke, non-owner VIP and normal-user invite 403, MCP 401,
Console assets and OpenAPI entries.

Not verified live: end-to-end document upload/search inside a shared knowledge
base (needs a reachable embedding/reranker provider), queued-search behaviour for
restricted grantees under a Kafka deployment, and browser E2E of the new pages.

## Kafka migration validation — 2026-11-09

- Live Docker Desktop stack with existing MySQL/Qdrant/Redis data volumes:
  Apache Kafka 3.9.0 KRaft container `healthy`; Kafka 3.9.0 started; topics
  auto-created with 4 partitions and the start-stack provisioning created
  cache/document/search plus both explicit DLQ topics.
- Debezium Server 3.7.1.Final on MySQL 8.0: `Snapshot completed` and
  `Starting streaming`; native Kafka sink producer logged
  `bootstrap.servers=[kafka:9092]`, `acks=-1`, idempotence enabled and String
  serializers.
- Consumed a real Debezium record from `praxis_document_ingest` whose
  `artifact_id` matched the MySQL `document_job_outbox` row; the row remained
  `PENDING` (the application did not publish), proving the CDC bridge delivered it.
- Verified on 2026-11-09 with the Go backend: the document async pipeline
  end-to-end in both bridge modes (`scripts/verify-debezium-e2e.py`: 14/14
  debezium, 13/0 relay, outbox PENDING vs PUBLISHED), live DLQ delivery for a
  poison payload (`scripts/verify-kafka-dlq.py`) and queued MCP search through
  the Kafka search topic with Redis snapshots (`scripts/verify-kafka-search.py`).
- Not yet verified: SASL/TLS Kafka clusters, Docker app image/Compose profiles,
  systemd units and live multi-instance MySQL failover.

The dated results below describe their tested revisions, not verification of the
current architecture migration. The approved active design used schema 2 complete
Kafka requests (document chunks: at most 512 KiB decoded; files: at most 10 MiB),
all-message broker confirmation before API success, and required Redis transient
task/reassembly/result storage. Search snapshots use `RAG_RESULT_TTL_SECONDS=3600`
with no SQL fallback. Task CAS and atomic result/success writes are Redis-local,
not an atomic Redis+MySQL transaction. MySQL retains published documents/blobs,
KB/API keys/catalog, artifact snapshots and UploadJournal operations/leases;
document success follows durable publication, reconciled via journal on retry.
Old task/outbox/result tables are retained but inactive, with no job/result
migration. Drain old workers before deploying API/workers together: schema 2 is
incompatible with UUID-only messages. See [deployment](docs/USAGE.md) and
[storage boundaries](docs/state-storage.md).

Current migration validation — 2026-10-25:

- Non-browser suite: **841 passed, 37 skipped, 1 deselected** in 141.35 seconds.
- Three browser files (`test_browser_model_options.py`,
  `test_browser_navigation.py`, `test_browser_word.py`) excluded because collection
  requires missing Playwright; it was not installed for these unrelated tests.
- Deselected `tests/test_model_api_settings.py::test_failed_reembedding_preserves_old_configuration_and_index[publish]`:
  the same failure was separately reproduced from pristine HEAD in an isolated
  extracted checkout. Known pre-existing failure; not fixed by this migration.
- Changed/added Python files have zero **new** Ruff findings versus HEAD;
  existing baseline findings remain. `git diff --check` passed.
- No live Kafka/Redis validation was performed for that historical revision; the later Kafka/MySQL/Debezium migration check is recorded at the top of this file.

- At this revision, confirmed one-hour expiry, full backward-compatible search
  responses, and del_flag soft deletion hiding new access while preserving UUIDs.
  Current durable retention guarantees apply to MySQL artifacts; Redis search
  result IDs can become unavailable after data loss/TTL expiry.
- This historical revision used immutable SQL result snapshots with optional
  Redis acceleration/fallback; that design is superseded, not the active contract.
  Artifact bytes had their own durable version-bound snapshots. No UUID read
  called Embedding, retrieval, reranking or parsing.
- Available Python regression: 618 passed, 31 skipped; three optional browser
  modules excluded. Existing four MCP deprecation warnings remain.
- Real MySQL/Redis/Qdrant UUID, cache and transaction suites: 16 passed using
  disposable praxis_uuid_test_*, praxis_cache_test_*, praxis_txn_test_* schemas
  and test-owned prefixes/collections; includes additive legacy-column upgrade.
- Focused core: 110 passed across UUID protocols, durable results and soft delete.
  Real TCP MCP tested in legacy, auto and 2026-07-28 modes.
- Tested exact bytes after update/delete/old-version GC, cross-namespace denial,
  revoked keys, paged byte reads, strict expiry, Redis loss/disabled, SQL/cache
  failures, artifact budgets, duplicate UUID rejection and concurrent registration.
- Ruff F/E9 and editor diagnostics passed. Frontend full typecheck remains
  blocked by the pre-existing missing platform TypeScript tsc.exe; only additive
  response types changed and no new frontend interaction is claimed.
- No production data migration or deletion was performed during validation.

# Historical UUID results/artifacts validation — 2026-10-22

These are historical checks of the then-current snapshot implementation, not
schema 2 queued-request, chunk replay/ACK or migration acceptance verification.

- Policy confirmed at that revision: Redis-only search result snapshots, 3600-second default TTL,
  full search response preserved. Missing/expired/evicted results never re-run
  inference. Issued version-bound artifacts remain readable after source deletion
  until their expiry; each request still checks current namespace authorization.
- Default Python regression: 529 passed, 30 skipped, four existing MCP warnings;
  three optional browser modules excluded.
- Focused UUID protocol/result/artifact suite: 95 passed.
- Real MySQL/Redis/Qdrant isolated UUID suite: 3 passed. Covers concurrent stable
  artifact registration, real Redis TTL/NX/eviction and full upload→search→replace
  →delete→old-reference read without invoking search again. Disposable schemas,
  prefixes and Qdrant collections were cleaned; no production data modified.
- Ruff F/E9 and diff checks passed; editor diagnostics clean on UUID storage,
  runtime and MCP/REST modules.
- At that revision, Redis outage/oversized snapshot returned synchronous search
  results with explicit snapshot_available=false; unavailable-result lookup errored.
  This is not queued MCP acceptance during an outage: queued operations require
  Redis/Kafka and all producer acknowledgements, with no durable SQL fallback.

# Historical second-batch state-cache validation — 2026-10-22

These checks concern MySQL-backed display read models, not the new Redis-only
MCP task/reassembly/result stores. Raw-input exclusion for these display views
does not forbid temporary raw document chunks in Redis reassembly.

- Confirmed strict mode: API-key authentication, Qdrant version selection, lease
  claims/renewal/fencing, commit and rollback remain authoritative SQL reads.
- Redis caches API-key administration, published-head display, lease/holder
  display, latest operation list and redacted step/rollback progress.
- Default Python regression: 434 passed, 27 skipped, four existing MCP warnings;
  three optional browser modules excluded.
- Live MySQL/Redis/Kafka (historical RocketMQ test stack)/Qdrant plus Redis Lua backend suite: 45 passed using
  isolated praxis_cache_test_* schemas, Redis prefixes and test collections.
  Includes real upload commit/aborted publication/delete refreshing state views,
  stale enabled display not authorizing a disabled key, and expired ownership
  failing fencing despite a live cached snapshot.
- Focused second-batch cache tests prove zero SQL on status cache hits, no raw
  secret/owner/workspace/step input leakage, namespace isolation, rollback events,
  affected-operation refresh after deletion, and no cache events on authority reads.
- Ruff F/E9 passed; editor reports no errors on changed integration files.
- No production document or API-key data was modified by the live test suite.

# Historical first-batch state-cache validation — 2026-10-22

Redis-outage fallback and SQL outbox statements in this section apply only to
persistent MySQL read-model refresh. They do not provide SQL acceptance or result
fallback for queued MCP uploads/search in the current architecture.

- Cached public knowledge-base configuration, MCP catalog, document lists and
  knowledge-base list/statistics; actual authorization/upload fencing remains
  authoritative. Cache-hit tests assert zero SQL and no tenant/model startup.
- Default Python regression: 399 passed, 24 skipped, four existing MCP warnings
  (three browser modules excluded because their optional environment is absent).
- Isolated MySQL/Redis/Kafka (historical RocketMQ test stack) and actual Redis Lua: 42 passed, including the
  live MySQL rollback assertion. The live suite uses disposable
  praxis_cache_test_* schemas, unique Redis prefixes and a test consumer group.
- Live coverage includes synchronous refresh, Redis outage fallback, Kafka delivery
  after failed cache writes, unacknowledged message redelivery, BIGINT ordering,
  persistent deletion watermark and TTL expiry.
- Versioned delayed-fill/duplicate/out-of-order events, transactional outbox
  rollback and simultaneous Redis/Kafka outage are covered by unit/integration tests.
- Docker startup/provisioning script validated with -MigrateOnly -SkipMigrate;
  production documents/credentials were not migrated or modified by these tests.
- Ruff F/E9 passed; no editor diagnostics on new cache code and integration paths.

# Historical durable upload validation — 2026-10-21

- HTTP/MCP uploads at this revision shared write-ahead operation/step logs,
  renewable MySQL leases, immutable version staging, atomic head publication and
  recovery. These MySQL publication/recovery guarantees remain; they do not mean
  the new queued MCP request or Redis success state is atomically committed with
  MySQL, nor verify the new broker-confirmed submission/chunk protocol.
- Python regression (excluding three unavailable browser modules): 329 passed,
  18 skipped, four existing MCP deprecation warnings. After the final version-byte
  deletion check, focused transaction/protocol regression: 143 passed, 2 skipped.
- Real MySQL 8 / Qdrant integration: 4 passed in a disposable
  `praxis_txn_test_*` schema and dedicated `txn_test_*` collections. Tests cover
  replay, partial Qdrant write failure, MySQL publication rollback and abrupt
  child-process exit before publication. Test schema/collections were removed.
- Fault injection covers source reception, parsing, JSON/Markdown rendering,
  chunking/tokenization, embedding, sparse encoding and both storage phases.
- Ruff F/E9 checks passed for changed source and transaction tests.
- Production console assets rebuilt successfully with Vite. `npm run build`
  typecheck remains blocked by a missing platform TypeScript `tsc.exe`, including
  after restoring declared optional dependencies; no compiler success is claimed.
- No production document/index migration was performed by this validation.

# Chunking Integration Validation

Date: 2026-09-07

Validated the existing five chunking strategies and upload preflight before
integrating the branch. Removed two unused imports and replaced one test lambda
with a named function; no chunking behavior was changed during this review.

## Results

- Ruff: passed using the existing `dev` environment.
- Full tests: 90 passed, 2 opt-in tests skipped, 4 MCP deprecation warnings.
- Real ingestion: 1 passed; BGE-M3 on CUDA, BM25, and isolated local Qdrant.
  All five strategies processed Markdown, TXT, PDF, and DOCX, retrieved evidence
  through embedding/BM25/hybrid, and skipped renamed duplicates.
- Browser: 1 passed against a temporary real service with CUDA embeddings.
  Verified preflight avoids duplicate uploads, changed parameters reindex,
  all five selections, and 1440/390/320-pixel layouts. No page errors.
- The temporary browser service and its child processes were stopped afterward.

## Reproduction

Run from this checkout with its `src` directory on `PYTHONPATH`.
Use `conda activate dl` for model tests and a project-local `.venv` created
with `--system-site-packages`; no shared environment packages were modified.

```powershell
conda activate dev
python -m ruff check src tests
conda activate dl
$env:PYTHONPATH = Join-Path $PWD 'src'
$env:HF_HUB_OFFLINE = '1'
.venv\Scripts\python.exe -m pytest tests -q
$env:RAG_REAL_SMOKE = '1'
.venv\Scripts\python.exe -m pytest tests/test_real_chunking.py -q -s
$env:RAG_BROWSER_URL = '<temporary local test service URL>'
.venv\Scripts\python.exe -m pytest tests/test_browser_chunking.py -q -s
```

The initial sandboxed test run failed on Windows temporary-directory permissions.
The results above are from a rerun with appropriate local execution permissions,
using separate test directories rather than the existing knowledge store.
Browser screenshots remain in ignored `.mini_agent-rag/chunking-ui/`.

# Ant Design Console Validation

Date: 2026-09-09

Implemented on `codex/ant-design-console`, in `.worktrees/ant-design-console`,
created from local `main` at `f6e8515`. No commit, merge, push, or restart of
the existing port-8011 service was performed.

## Console Results

- TypeScript check and Vite production build: passed in `conda activate dev`.
- Focused HTTP and browser suite: **15 passed** in 31.34 seconds.
- Two browser cases used actual uploads and searches against local Qdrant,
  BGE-M3 and BGE Reranker on CUDA in the `dl` environment. Verified all five
  chunking strategies, renamed duplicate skipping without a second upload,
  changed-parameter reindexing, original text, recall and rerank scores,
  sequential three-mode comparison with reranking disabled, deletion and
  cancellation, and a mixed batch with one indexed TXT and one invalid DOCX.
- Three supplementary browser cases used controlled transport responses to
  verify failed-search clearing, repeat-request prevention, stale search and
  document response isolation after namespace changes, invalid parameters,
  and partially failed comparison. The batch case also paused preflight to
  check the upload lock before continuing to the real API.
- Both themes checked at 1440, 390 and 320 pixels across all three pages,
  upload dialogs and strategy menus. Screenshots were visually reviewed;
  no page overflow or unhandled JavaScript errors were detected. Theme
  persistence and session-only Token storage were verified.
- Wheel built in a fresh build directory and extracted into a separate import
  directory. The packaged app served `/`, `/console` and both referenced
  compiled assets successfully; old `app.js` and `styles.css` were absent.
- Vite reports one non-blocking bundle-size warning: the main script is about
  1.02 MB before compression, 326 KB gzip. No runtime CDN is required.

## Isolated Preview

- Address: `http://127.0.0.1:8021/console` (left running for inspection).
- Data and local Qdrant: `.test-tmp/ant-console-runtime/` in this worktree.
- Collection: `ant_console_acceptance`; browser cases create unique `ui-*`
  namespaces and clean up only their own uploaded documents.
- Screenshots: `.test-tmp/console-browser/`; clean wheel:
  `.test-tmp/console-wheel/`.
- No existing knowledge documents or model settings were changed. The full
  unrelated backend suite and PDF parsing benchmarks were not rerun for this
  frontend-only change.

## Local Main Integration

Date: 2026-09-09

- UI commit: `5566207`; merged with the PDF and MCP changes on main at
  `c0f0630`. The README insertion conflict was resolved by retaining both
  the PDF fidelity notes and frontend development instructions.
- On the combined main checkout, `npm ci` and `npm run build` passed.
- HTTP, document tool, and MCP tests: 36 passed, 1 skipped, with four
  existing legacy MCP deprecation warnings. The Vite bundle-size warning
  remains non-blocking.
- Local merge only; no push or existing-service restart. The independent
  frontend worktree and preview service were retained.

# Model API Settings Validation

Date: 2026-09-09

Implemented in `.worktrees/model-api-settings` on `codex/model-api-settings`,
created from local `main` at `f07df00`. No commit, merge, or push was performed.
The original checkout and port-8011 service were not changed.

## Behavior

- Embedding and Reranker now use independently configured HTTP endpoints in
  the standalone HTTP/MCP runtime. No local inference model is loaded there.
- Console supports editable model lists, optional API keys, real connection
  tests, and persistent immediate application. API keys are never returned
  or stored in browser storage; Windows persistence uses DPAPI.
- Configuration mutations and proxy/test requests require a direct local
  request and existing service authentication. Remote configuration is read-only.
- All runtime operations resolve their model dependencies under the same
  configuration lock. Existing namespaces are refreshed together after saving.
- Vector sizes are detected from responses. Occupied namespaces and stored
  Qdrant points prevent changing embedding identity/dimension; changing empty
  collection dimensions creates a new collection without deleting the old one.
- Chunk tokenizer identity is separate from API model identity; both remain
  part of upload deduplication metadata.

## Verified Results

- Frontend TypeScript check and production build passed. Existing Vite warning
  for the approximately 1.03 MB main bundle remains non-blocking.
- Focused HTTP, settings, MCP, document operations and dedup suite:
  **64 passed, 1 opt-in skipped**, four existing legacy MCP warnings.
- Settings boundary suite plus real browser acceptance: **21 passed**.
  Includes a fresh Python process restoring persisted configuration and
  decrypting its key, then successfully calling the configured API.
- Real MCP acceptance: **1 passed**. Added, retrieved, updated, read, and
  deleted a document using actual BGE embeddings through HTTP and real Qdrant.
- Real browser acceptance used actual CUDA BGE-M3 and BGE Reranker behind an
  independent HTTP test provider. Verified empty-key configuration, model
  lists and manual entry, real testing/saving, upload with recursive token
  chunking using a remote model alias, embedding retrieval with reranking,
  occupied-model-change rejection, key non-disclosure/preservation/clearing,
  remote read-only controls and browser reload persistence.
- Screenshots at 1440, 390 and 320 pixels in both themes were inspected.
  No horizontal overflow or unhandled page errors were detected.
- Ruff on changed Python modules/tests and `git diff --check` passed.

## Full-Suite Boundary

The broader suite returned **145 passed, 12 skipped, 5 failed** before the
additional fresh-process case was added. All five failures were reproduced
against the original main checkout using the same environment: four require
missing `docling_core`, and one expects an OCR error but receives a missing
`docling` error. They are not counted as passing or changed in this work.
Full real PDF parsing was not rerun for this model-API change.

## Local Preview and Reproduction

- Preview: `http://127.0.0.1:8031/console`.
- Independent real test provider: `http://127.0.0.1:8032/v1`, with aliases
  `served-embedding` and `served-reranker`. It uses already cached weights
  in the `dl` environment and is not launched by production application code.
- Both preview helpers are left running for inspection. The provider occupies
  GPU memory; original service processes and data paths were not restarted.
- RAG data/Qdrant and logs: `.test-tmp/model-api-acceptance/`;
  provider logs: `.test-tmp/model-api-live/`;
  screenshots: `.test-tmp/model-api-browser/`.
- Python tests use the worktree-local `.venv` created from `conda activate dev`,
  with `pip install -e ".[api]" playwright`. Shared environments were unchanged.

```powershell
conda activate dev
$env:PYTHONPATH = "$PWD/src"
.venv\Scripts\python -m pytest tests/test_model_api_settings.py tests/test_http_service.py tests/test_mcp_http.py tests/test_document_tools.py tests/test_upload_dedup.py -q
$env:RAG_MODEL_API_TEST_URL = "http://127.0.0.1:8031"
$env:RAG_MODEL_PROVIDER_URL = "http://127.0.0.1:8032/v1"
.venv\Scripts\python -m pytest tests/test_real_model_api_ui.py -q -s
$env:RAG_REAL_SMOKE = "1"
.venv\Scripts\python -m pytest tests/test_document_tools.py -k real_mcp -q -s
```

## PDF 解析错误信息 — 2026-11-10

现场：Console 批量上传 `2404.16130v2.pdf` 返回「知识库操作失败：errorString」，服务端日志无错误详情。

排查结论（MySQL `upload_operations` / `upload_steps`）：

- 失败操作 `cfbb8f8f5058fdb869a58a2deb2a7fec`（namespace `asdjasdajbsc`）状态 `ROLLED_BACK`，失败步骤 `chunking_tokenizer`，`error` 列只存了脱敏类别 `errorString`；`receive_source`、`deduplicate` 已完成，`rollback` 完成，没有残留文档。
- 根因是 PDF 解析依赖外部 Docling Python worker 而本机没有该环境：`python -m rag_mcp.pdf_worker --help` 直接报 `ModuleNotFoundError: No module named 'rag_mcp'`（系统 Python 3.11；`.venv` 里也没有 `docling`），`.env` 未设置 `RAG_PDF_PARSER_PYTHON`/`PYTHONPATH`；同库的 `.txt` 上传在 13:51 成功。
- `errorString` 来自 `safeUploadErrorText` 的兜底分支：`shortTypeName` 输出 Go 类型名，而 `fmt.Errorf`（无 `%w`）产生的就是 `*errors.errorString`。Python 原实现 `_safe_error` 用的是 `type(exc).__name__`，所以这是移植后丢掉可读性的缺陷。

修复与验证：

```powershell
$env:GOCACHE = ".test-tmp\gocache"
$env:RAG_TEST_MYSQL_URL = "mysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_gotest"
go vet ./internal/document ./internal/runtime ./internal/ingest
go test ./internal/document ./internal/ingest -count=1
go test ./internal/runtime -run "TestSafeUploadErrorTextPdfParseMessage" -count=1 -v
go test ./internal/runtime -run "Upload|Pdf|PDF|StopIndex|Recover" -count=1 -timeout 20m
```

- `internal/document`：`TestExternalPDFParserErrors/missing_python_module`（stderr 含 `No module named 'rag_mcp'` 时返回 `*PDFParseError`，消息含模块名与 `RAG_PDF_PARSER_PYTHON`）、`TestMissingPythonModule`（带引号/不带引号/无标记三种 stderr）通过；原有失败/超时/manifest 错误消息保持不变。
- `internal/runtime`：`TestSafeUploadErrorTextPdfParseMessage` 通过；上传相关子集（`Upload|Pdf|PDF|StopIndex|Recover`）223.4s 全绿。
- 重建 `dist/bin/rag-service.exe` 并以同样生产配置重启 8011，`/health` 200；再次上传 PDF 时响应应为「PDF 解析不可用：Python 解析环境缺少模块 rag_mcp…」而不是 `errorString`。
- 已知未处理：旧 debug 上传 `2c060059701b999a604c3d42863e3982`（namespace `debug222146`）的历史工作区不在当前数据根下，回收循环每 10 秒打一条 `Unsafe upload workspace`（ValueError）日志，与本问题无关，待单独清理。

## PDF 解析恢复为 Docling 微服务 — 2026-11-10

背景：commit `f778504` 删除 Python worker 后，PDF 改由纯 Go 引擎抽取文本，版面/表格/公式全部丢失；公式符号还被 TJ 换行注入与 TeX CM 字体解码成 `Ω`/`⊗`（见上一节）。本轮把 Docling 解析恢复为**独立 Python HTTP 微服务**，Go 主服务不引入 Python 依赖。

交付：

- 服务 `python-workers/pdf-service`（FastAPI + `docling==2.126.0`）：`GET /healthz`、`POST /v1/parse`（multipart 字段 `file` → `application/zip`：`manifest.json` + `pictures/*.png`）；清单版本 `docling-service-v1`；公式 block 带 `latex`/`original_text`/`equation_number`；进程内并发上限与单请求超时；docling/torch 惰性导入（测试与 fake 模式无需安装）；`RAG_PDF_SERVICE_CONVERTER=fake` 仅用于测试联调。
- Go 端：`internal/document/docling_service.go`（`DoclingServiceParser`：流式 multipart → 512 MiB 响应上限 → 安全解压（拒绝绝对路径/`..`/symlink/重复条目，条目/单文件/总大小上限）→ manifest 版本与 `image_file` 存在性校验 → `*PDFParseError` 安全消息）；版本兼容 `go-native-v1`/`docling-service-v1`/历史 `docling-formula-remote-v3`；config 新增 `RAG_PDF_ENGINE`/`RAG_PDF_SERVICE_URL`/`RAG_PDF_SERVICE_TIMEOUT`；runtime 按引擎注入解析器，并把实际清单版本写入文档与 chunk 元数据；`pdfcontent` 的 `pdf_parser_version` 改为 `manifest.Version`。
- 部署/文档：docker compose 新增默认启动的 `pdf-parser`（8091、`pdf_parser_models` 卷、`/healthz` healthcheck）；`.env.example`、README/USAGE/DEPLOYMENT/GO_MIGRATION/PROGRESS 同步；`scripts/start-stack.ps1` 默认拉起并软等待 pdf-parser。

验证（Windows 11，Go 1.x + `.venv` Python 3.11.7）：

```powershell
cd python-workers/pdf-service
..\..\.venv\Scripts\python.exe -m pytest tests -q -p no:cacheprovider
# 57 passed in 0.88s（无 docling/torch）

# fake 模式启动服务
$env:RAG_PDF_SERVICE_CONVERTER = "fake"
..\..\.venv\Scripts\python.exe -m uvicorn app.main:app --host 127.0.0.1 --port 8091
# GET /healthz -> 200 {"status":"ok","converter":"fake","parser_version":"docling-service-v1"}
# POST /v1/parse（10 MB 真实 PDF）-> 200，zip 1208 B；
# manifest.json version=docling-service-v1，blocks=[text, formula($$\nG = X_{basis}^{T} X_{basis}\n$$\n\n(1)), picture]，
# pictures/picture-0.png 存在且 PNG 魔数正确

$env:RAG_PDF_SERVICE_URL = "http://127.0.0.1:8091"
go test ./internal/document -run TestDoclingServiceParserLiveService -count=1 -v
# PASS：真实 HTTP -> zip -> staging -> manifest 校验
go test ./internal/ingest -run TestDoclingServiceLivePrepare -count=1 -v
# chunks=3 formula=1（临时 E2E 测试，验证后已删除）：
# Result.Metadata["parser_version"]=docling-service-v1；每个 chunk 的 pdf_parser_version=docling-service-v1；
# 公式 chunk 的 latex 非空；staging 内含 manifest.json、pictures/picture-0.png、document.md（含 $$ 公式）

go build ./...                        # OK
go vet ./...                          # OK
go test ./internal/runtime -count=1   # 469s 全绿（RAG_TEST_MYSQL_URL=praxis_gotest）
go test ./internal/runtime -run 'TestPDFOptionsSelectsConfiguredEngine|TestIndexConfigurationRecordsPDFParserEngine' -count=1
# PASS：默认引擎 Parser=nil；docling 引擎按 URL/超时构造 DoclingServiceParser；元数据版本随引擎变化
go test ./internal/document ./internal/config ./internal/ingest -count=1   # 全绿
go test -p 1 ./internal/... -count=1   # 全绿（runtime 469s / state 426s / e2e 216s 等；RAG_TEST_MYSQL_URL=praxis_gotest）
# 注意必须 -p 1：多个包并发时 statetest 的 MySQL advisory lock 会让后到的包等 600s 超时。
docker compose --env-file .env.example config --quiet                       # exit 0
```

已知限制/未验证：

- 本机未安装 docling/torch，也未构建 Docker 镜像：真实 Docling 转换（CodeFormulaV2/VLM、EasyOCR、TableFormer）未在本机实测，只有移植代码与 fake 契约测试覆盖；真机首次部署需预置 Docling 模型或配置 `RAG_VLM_*`。
- 服务端超时只能快速返回 504，无法中断已启动的 Docling 线程；并发槽在后台任务结束后才释放。
- `/healthz` 恒 200（配置 degraded 时 `/v1/parse` 返回 503），compose healthcheck 只反映进程可用性。
- zip 不再包含旧 worker 的 `document.json`/`ocr.json`，OCR 明细只保留在 manifest 的 `warnings` 中。

## Console OCR 模型配置 + Docling 按知识库下发 VLM/OCR — 2026-11-11

在独立 worktree/branch `docling-kb-ocr-vlm`（基线 commit `2e17c0f`，已含 Docling PDF 微服务）实现：Console「模型配置」新增 OCR 类型，知识库设置新增 `ocr_model`；docling 引擎上传 PDF 时按知识库把 VLM/OCR 配置随 `POST /v1/parse` 的 multipart `options` 下发给 pdf-parser；未配置时在受理阶段提示用户去配置，不进入队列。

接口/数据契约：

```
POST /v1/parse  multipart: file=<PDF>  options=<JSON>
options = {"vlm":{"base_url":"https://.../v1","api_key":"sk-...","model":"qwen2.5-vl-7b",
                  "timeout":120,"concurrency":4},
           "ocr":{"engine":"easyocr"}}        # engine: auto|easyocr|tesseract|rapidocr
```

- 模型配置：`kind=ocr` 复用 `model_profiles` 列，`model` 为引擎 id、`base_url` 可空、apikey 不存、不探测；`POST/PATCH /v1/admin/model-profiles` 与 openapi kind enum 同步。
- 知识库：创建/更新 API 新增 `ocr_model`（更新缺省保留、空串清除，白名单校验 422）；列表/详情投影与 `RAG_DEFAULT_KB_TEMPLATE` 克隆保留该字段。
- 上传：`RAG_PDF_ENGINE=docling` 且有 `.pdf` 时，同步 `Upload` 与异步受理都要求 VLM（KB 优先，`RAG_VLM_*` 回退，model 非空）与 OCR 均已配置，否则返回 `PDF 解析需要先在「模型配置」页添加 VLM 和 OCR 模型，并在知识库配置中选择后再上传。`，不写 `upload_operations`/`document_jobs`；非 PDF、`go` 引擎不受影响。
- 传输：`DoclingServiceParser` 增加 `VLM`/`OCR`；`options` 仅在已配置时发送，api_key 不落日志/错误文案；python 侧按请求覆盖环境变量，`manifest.parser.ocr_engine` 记录实际引擎。

验证命令与结果（worktree 根目录，MySQL `praxis_gotest`）：

```
$ go build ./...                                      # exit 0
$ go vet ./...                                        # exit 0
$ go test -p 1 ./internal/... -count=1                # exit 0，全绿
  ok internal/document 0.891s   ok internal/kbconfig 0.278s   ok internal/httpapi 17.514s
  ok internal/ingest 0.284s     ok internal/runtime 474.244s  ok internal/state 423.301s
  ok internal/e2e 215.467s      ...（其余包全 ok）

$ cd python-workers/pdf-service
$ D:\Project\Praxis-RAG\.venv\Scripts\python.exe -m pytest tests -q -p no:cacheprovider
140 passed in 0.85s                                   # 基线 57 + 新增 83；无需 docling/torch

$ cd frontend
$ npx vitest run     # 5 files / 63 tests passed（基线 54 + 新增 9，OCR 用例含请求体与 /key、/models 不调用断言）
$ npm run typecheck  # exit 0
# 注：本沙箱 Vite 8 的 `net use` 管道探测报 spawn EPERM（基线同样），按既有做法用临时 NODE_OPTIONS
#     child_process preload shim 运行；shim 在 %TEMP%，未入库。

$ docker compose --env-file .env.example config --quiet   # exit 0（仅注释变更）
```

Go 客户端 → 真实 HTTP fake pdf-parser 的 `options` 端到端（服务用 `RAG_PDF_SERVICE_CONVERTER=fake` 起在 127.0.0.1:8097）：

```
Go: DoclingServiceParser{VLM:{BaseURL:vlm.internal:8443, Model:qwen2.5-vl-7b, Timeout:120s, Concurrency:2},
                         OCR:{Engine:easyocr}, BaseURL:http://127.0.0.1:8097}
    -> ParseInto 成功，staging 含 manifest.json + pictures/picture-0.png（PASS，临时 E2E 测试已删除）
HTTP: 带 options（vlm+ocr=tesseract）                 -> 200 application/zip 1216 B
      options 非法 JSON                                -> 422 "options 不是合法的 JSON 对象"
      options.ocr.engine=nope                          -> 422 "options.ocr.engine 不受支持；可选值：auto、easyocr、tesseract、rapidocr"
      不带 options                                     -> 200 application/zip 1215 B（向后兼容）
```

已知限制/未验证：

- 本机仍未安装 docling/torch：真实 Docling pipeline 未运行，OCR 引擎映射（EasyOCR/Tesseract/RapidOCR）只有不依赖 docling 的单元测试与 fake 契约测试；tesseract 还需要镜像/系统安装 tesseract 与 `chi_sim`/`eng` 语言包，`auto` 当前固定映射 EasyOCR。
- 历史知识库没有 `ocr_model`：在 `RAG_PDF_ENGINE=docling` 下上传 PDF 会被提示先配置（符合需求），需要管理员在 Console 补配 OCR 后才可继续；`go` 引擎不受影响。
- 上传校验只作用于“受理上传”路径；重新解析既有文档仍走原 queue/reparse 路径，不改其失败语义。
- OCR 语言按引擎默认（EasyOCR `ch_sim,en`、Tesseract `chi_sim,eng`），暂未提供按知识库自定义语言列表的入口；模型配置页与知识库表单只做 jsdom 组件测试，未做浏览器视觉回归。

## Console 批量删除（知识库 / API Key）— 2026-11-11

在 worktree/branch `dsh-3f6b8955-e922`（基线 commit `13befbe`）实现 Console 知识库页与 API Key 页的批量删除：行首复选框多选 + 鼠标左键框选（框选只增不减：拖动/缩小框、开始新一轮框选或滚动页面都不会取消已选中的行，取消选中点击行首复选框，Esc 取消本次拖动并恢复拖动前的选中），工具栏「批量删除」按钮显示已选数量；部分失败的行保持选中并显示告警，直到下一次刷新。

接口/数据契约：

```
DELETE /v1/admin/knowledge-bases/{namespace}     # 204 删除成功；404 配置和文档都不存在；409 部分文档删除失败（保留配置）
POST   /v1/admin/knowledge-bases/batch-delete    # {"namespaces":[...]} 1..100
     -> {"deleted":[ns],"missing":[ns],"failed":[{"namespace","error","failed_documents":[{"name","error"}]}],
         "documents_deleted":N,"configurations_deleted":N}
POST   /v1/admin/api-keys/batch-delete           # {"ids":[...]} 1..100
     -> {"deleted":[id],"not_found":[id],"failed":[{"id","error"}],"deleted_count":N}
```

- 知识库删除语义：Console 的知识库列表是「配置记录 ∪ 文档工作区」的合并视图，只删配置记录会留下「缺少配置记录」的条目。因此删除先逐个走既有单文档逻辑删除（`Service.deleteDocumentByName`，`del_flag`；Qdrant 向量按现有语义保留，不删除 collection），再删除配置记录并失效租户缓存；任一文档删除失败（例如上传/更新占用锁）时保留配置并返回 409 + `failed_documents`，可修复后重试。批量删除逐条执行同样的流程，`missing` 表示配置和文档都不存在，`failed` 保留该命名空间。
- API Key 删除语义：与单条 `DELETE /v1/admin/api-keys/{id}` 完全一致（`Get` → `apiKeyCallerOwns` → `Delete`），非管理员访问他人/不存在的 id 归入 `not_found`，存储层错误进入 `failed`；单个 id 失败不中断整批。空 ID、空数组、超过 100 项、未知字段返回 FastAPI 形状 422；未配置 keyStore 返回 503。
- 路由顺序：两条 batch 路由按字面路径注册在 `{namespace}` / `{key_id}` 参数路由之前（router 先匹配者生效），`DELETE /v1/admin/knowledge-bases/{namespace}` 注册在 KB 管理路由表末尾。
- Console：新增 `dragSelect.ts`（`bandRect`/`intersects`/`keysInBand`/`mergeSelection` 纯函数，正面积相交、贴边不选中）与 `useDragSelection`（左键拖拽超过 4px 才接管手势；忽略 button/input/textarea/select/label/th/分页/复选框等交互元素；window 级 move/up/Escape；`document.body` 的 `drag-selecting` 类与内联样式在结束/卸载时清理；不自行插入 DOM）。两个页面渲染返回的 `{dragging, band}` 为 `.drag-band` 固定覆盖层；失败行保持选中并显示告警。框选选择集只增不减：`mergeSelection` 只做并集，拖动/缩小框、开始新一轮框选或滚动页面都不会取消已选中的行，取消选中依靠行首复选框（Esc 只撤销本次拖动）。
- OpenAPI 3.1：`/openapi.json` 新增上述 3 条路由与 `BatchDeleteKnowledgeBasesBody` / `BatchDeleteAPIKeysBody` schema；`openapi_test.go` 断言路由存在、DELETE 方法与 required 字段。

验证命令与结果（worktree 根目录，MySQL `praxis_gotest`）：

```powershell
$env:RAG_TEST_MYSQL_URL = "mysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_gotest"

go build ./...                 # exit 0
go vet ./...                   # exit 0
go test -p 1 ./internal/... -count=1 -timeout 3600s
# exit 0，全绿；关键包：
#   ok internal/httpapi   20.172s
#   ok internal/runtime  485.411s
#   ok internal/state    422.598s
#   ok internal/e2e      215.808s
#   ...（access/auth/catalog/chunking/config/contracts/document/documentworker/ingest/kbconfig/
#        mcp/migrate/modelapi/mq/qdrant/redisstate/resources/retrieval/searchrefs/searchworker/
#        sparse 等其余包全部 ok）

cd frontend
.\node_modules\.bin\tsc.cmd --noEmit -p tsconfig.json
# exit 0，无诊断
.\node_modules\.bin\vitest.cmd run
# Test Files 9 passed (9) / Tests 129 passed (129)，exit 0
npm run build
# exit 0：vite v8.2.2，3112 modules transformed，build/web 资源生成（chunk >500 kB 为既有告警）
```

新增/扩展测试：

- `internal/httpapi/kb_purge_test.go`（新增，7 个用例）：单条删除 204/404/409、运行时报 503、批量 deleted/missing/failed 分桶与计数、422 校验集合（缺失/空数组/非法命名空间 loc/超长/未知字段）、非本机 403。
- `internal/runtime/knowledge_base_test.go`（扩展，3 个用例）：文档逐个逻辑删除 + 配置删除 + 缓存失效 + 合并列表消失；文档删除失败时保留配置并报告；未配置工作区删除文档、以及不存在的命名空间返回零结果。
- `internal/httpapi/api_keys_batch_test.go`（新增）：批量删除已存在/未知 id、删除后旧 key 鉴权失效而剩余 key 仍可用、重复 id 去重、重复删除归入 not_found、422 校验、503。
- `frontend/src/dragSelect.test.ts`（34）、`useDragSelection.test.tsx`（13）、`KnowledgeBasesPage.test.tsx`（7）、`ApiKeysPage.test.tsx`（7）、`api.test.ts` 批量删除契约（新增 5，文件共 30）。其中框选用例覆盖：框移开/缩小后已选中行保留、滚动改变行位置后不取消选中、Ctrl/Shift/Meta 不再改变只增不减语义、复选框选中行在后续框选中保留。
- `internal/httpapi/openapi_test.go`：新增 3 条路由与两个 batch schema 的断言。

环境说明：本会话在 DSH `workspace-write` 下 `ops_bash` 的 Niubash 私有副本持续 `EXECUTOR_UNAVAILABLE`（link UNKNOWN），改在 `danger-full-access` 下用 `pwsh` 运行全部命令；这属于执行器环境问题，与代码无关。

已知限制/未验证：

- 未做浏览器级 E2E：框选与确认弹窗在 jsdom 中以桩 `getBoundingClientRect` 验证，真实浏览器里的指针手感与覆盖层层级只做人工核对。
- 框选采用「正面积相交」：恰好零高度/零宽度的拖拽，或只贴边不重叠的行不会被选中（避免相邻行同时命中）；真实指针抖动会产生非零面积。
- 框选只增不减：已选中行只能通过行首复选框取消（Esc 取消当前这次拖动）；拖动、缩小框选、开始新一轮框选与页面滚动都不会清空已有选择，这是按使用习惯刻意选择的语义。
- 文档删除仍是逻辑删除：Qdrant 向量与 collection 按现有单文档删除语义保留，不随批量删除清理；需要物理清理时要另行走既有 Qdrant 删除路径。
- 知识库批量删除是同步逐文档删除：超大知识库（数千文档）会拉长请求时间，接口上限 100 个命名空间，未做后台任务化。
- 批量删除沿用现有删除路径，没有新增独立的审计日志事件。

## API Key 批量删除性能修复（单事务 + 文本协议）— 2026-11-11

线上手感「批量删除很慢」的根因定位与修复。测量环境：Windows + Docker Desktop，MySQL 8.0.46（`innodb_flush_log_at_trx_commit=1`、`sync_binlog=1`、binlog 开启），宿主通过 `127.0.0.1:3307` 访问。

根因（测量结论，非猜测）：

- 旧实现每个 key 一次 HTTP 循环内处理：`Get`（1 条）+ `Delete`（独立事务：缓存修订 INSERT/UPDATE/SELECT、`cache_outbox` INSERT、`DELETE`、COMMIT），即约 6 条带参数 SQL + 1 次 commit/key，且串行。
- 同一 SQL 用 Python 直连执行整套 touch+delete+commit 只需 **7.4ms/key**，MySQL 执行本身不是瓶颈。
- 隔离实验：go-sql-driver 带参数语句默认走**服务端预处理**（`COM_STMT_PREPARE/EXECUTE`），在该链路上每条约 **43–48ms**；文本协议（`interpolateParams=true`，`COM_QUERY`）同一语句约 **0.3–1ms**；复用同一个 prepared stmt 约 0.7ms。于是旧实现 ≈ 6×44ms ≈ 260ms/key（与实测 272ms/key 吻合）。
- 因此慢在「每条语句一次往返、每次往返固定多约 40ms」，不在 SQL、不在 MySQL、也不在删除逻辑。

交付（原子批量删除 + 连接器协议）：

1. `APIKeyRepository.DeleteMany(ctx, keyIDs, ownerUserID)`：一个事务内 1 条 `SELECT id FROM api_keys WHERE id IN (...) [AND owner_user_id = ?]` 分类、1 条 `DELETE ... WHERE id IN (...)` 删除、整批只调用 1 次 `touch(apiKeyKeys())`（1 条修订 + 1 条 outbox 事件）、1 次 commit；重复 id 去重，空 id 忽略；`RowsAffected` 与已存在数量不一致时整批回滚并返回 `API Key 批量删除冲突，请重试`。
2. `httpapi.handleBatchDeleteAPIKeys`：不再逐 id 调用 `Get`/`Delete`；非管理员会话把 `ownerUserID` 传给仓储（他人/不存在的 id 归入 `not_found`）；响应结构不变（`deleted`/`not_found`/`failed`/`deleted_count`，`failed` 恒为空数组保留兼容）；整批数据库错误返回 500 `API Key 删除失败`（整批回滚）。
3. `internal/state/store.go`：`config.InterpolateParams = true`，带参数语句改走文本协议，全站所有查询受益（含登录会话每次请求的认证查询）。

实测对比（同一台机器、同一 MySQL 容器）：

```text
旧实现（2026-11-11 20:49 测量，praxis_gotest 临时实例）：
  batch-delete 60 个真实 key : 16,349ms  (272ms/key)
  batch-delete 60 个不存在 id: 16,406ms  (Delete 事务照样执行)
  单个 DELETE                : 275ms
  单个 Create                : 230ms

新实现（2026-11-11 22:47 测量）：
  GET  /v1/admin/api-keys        : 2–4ms（预热后）
  batch-delete 60 个不存在 id    : 8ms   （正式实例 8011）
  batch-delete 60 个真实 key     : 15ms  （praxis_gotest 临时实例，deleted=60 not_found=0 failed=0）
  重复删除同一批 60 个 id        : 6ms   （deleted=0 not_found=60）
  单个 DELETE                    : 12ms
  单个 Create                    : 23ms/key（含 PowerShell 客户端每次请求开销）
```

验证命令与结果（worktree 根目录，MySQL `praxis_gotest`）：

```powershell
$env:RAG_TEST_MYSQL_URL = "mysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_gotest"

go build ./...                                   # exit 0
go vet ./...                                     # exit 0
go test -p 1 ./internal/... -count=1 -timeout 3600s
# exit 0，全绿；关键包（同一台机器，修复前 → 修复后）：
#   internal/state        422.598s → 29.810s
#   internal/e2e          215.808s →  8.821s
#   internal/runtime      485.411s → 43.675s
#   internal/httpapi       20.172s →  2.029s
#   internal/documentworker 54.741s → 7.446s
#   internal/access        20.102s → 1.754s

cd frontend
.\node_modules\.bin\tsc.cmd --noEmit -p tsconfig.json   # exit 0
.\node_modules\.bin\vitest.cmd run                      # 9 files / 137 tests passed, exit 0
```

覆盖新增/更新：

- `internal/state/api_keys_batch_test.go`（新增）：混合存在/不存在/重复/空 id 的一次事务批量删除；校验 `deleted` 顺序、`missing` 分桶、整批只 bump 1 次 revision、只写 1 条 outbox 事件、已删除 id 再次提交不写修订/事件；owner 限定批删只删本人 key，他人 key 归入 missing 且保留。
- `internal/httpapi/api_keys_batch_owner_test.go`（新增）：普通账号会话批量删除「本人 + 他人 + 不存在」三个 id，`deleted` 只有本人 key、`not_found` 含他人与不存在 id，他人 key 仍存在；管理员会话可删剩余 key。
- 既有 `internal/httpapi/api_keys_batch_test.go`（生命周期/去重/not_found/422/503）在原子实现下保持通过。

环境 / 运维：修复后已停止并按新二进制重启正式实例（`rag-service` + `rag-document-worker`，`http://127.0.0.1:8011/console`），测量后临时实例与临时探针目录均已清理。

已知限制/未验证：

- 批量删除语义从「逐条尽力而为、部分失败」改为「整批原子」：并发冲突或数据库错误时整批回滚并返回 500，客户端重试即可；`failed` 字段保留但恒为空数组。
- 未做浏览器级性能回归：数值来自服务端日志/HTTP 计时与 Go/MySQL 隔离测量，Console 只验证了响应结构不变。
- 未做并发压测（同时多个管理员对同一批 id 操作时，`RowsAffected` 不一致会整批回滚，预期行为但未专门压测）。
- 知识库批量删除仍是逐文档逻辑删除（`purge` 循环），本次只优化了 API Key 批量删除；如同样慢可按相同思路改造。（后续已于 2026-11-12 按同一思路完成：见文末「内容去重与批量删除」。）

## JEV model configuration — 2026-11-11

Delivered: a Console-only `jev` saved-model kind (TypeSafe JEV / System One
decision endpoint) on the 模型配置 page. It stores name / base_url / model /
encrypted API key like VLM, is never probed, has no provider `/models` route
(the listing endpoint returns `[]`), and is intentionally excluded from the
knowledge-base model vocabulary (`knowledgeBaseKinds` stays
embedding/reranker/vlm/ocr; the Console create/edit knowledge-base form uses
`knowledgeBaseModelKinds`).

Verification performed (2026-11-11, this worktree, Go 1.27.3 / Node v26.13.0 /
npm 12.2.2). The session's `ops_bash` executor is intermittently unavailable
(dsh-ops 0.5.1 exec-tree hardlink/link-count concurrency bug:
`EXECUTOR_UNAVAILABLE: Niubash preparation failed`), so commands were retried
across steps; everything below actually ran:

```
$ git status --short
                                                    # 23 modified files, exactly the
                                                    # intended scopes; no stray files
$ go build ./...
                                                    # exit 0
$ go vet ./internal/kbconfig ./internal/httpapi ./internal/runtime ./internal/state
                                                    # exit 0 (test files compile too)
$ go test ./internal/kbconfig
                                                    # ok
$ go test -p 1 -timeout 480s ./internal/httpapi
                                                    # ok, 17.5 s
$ go test -p 1 -timeout 480s ./internal/runtime ./internal/state
                                                    # ok: runtime 475.0 s, state 422.5 s
$ RAG_TEST_MYSQL_URL=... go test ./internal/runtime -run 'TestModelProfile' -v
                                                    # PASS: lifecycle, error mapping,
                                                    # OCR engine, and the new
                                                    # TestModelProfileJEVConfiguration
$ go test ./internal/httpapi -run 'TestModelProfile' -v
                                                    # PASS: list/details/key/models,
                                                    # create (incl. new kind=jev -> 201),
                                                    # patch/delete, validation parity,
                                                    # runtime/admin guards
$ cd frontend && npm run typecheck
                                                    # exit 0, no diagnostics
$ cd frontend && npm test
                                                    # Test Files 5 passed (5)
                                                    # Tests 64 passed (64) — was 63,
                                                    # +1 JEV console test
```

Notes: the MySQL suite ran against the local `praxis-rag-mysql` container on
127.0.0.1:3307 with the disposable `praxis_unit_test` schema
(`RAG_TEST_MYSQL_URL=mysql://rag:<password>@127.0.0.1:3307/praxis_unit_test`; the
`rag` user lacks CREATE DATABASE, so the schema and grant were created with the
container's root password). Packages must stay serial (`-p 1`): every package
truncates the same schema behind `GET_LOCK(...,600)`, so parallel packages queue
past the 10-minute package timeout — an initial parallel run reproduced exactly
that (`internal/state` FAIL after 600.458 s), and the serial rerun passed.
Frontend commands used a temporary `frontend/node_modules` junction to the main
checkout because this worktree has no installed dependencies; the junction was
removed afterwards and the main checkout stays intact. `go test ./...` for
unrelated packages (e2e, documentworker, cmd) was not rerun.

Behavior/limits confirmed by the run: `Kinds`/`IsKind` include `jev`;
`ProbeModelProfile("jev")` issues no provider call; `POST
/v1/admin/model-profiles` with kind `jev` returns 201 and never probes; `POST
/v1/admin/knowledge-bases/models/jev` still returns 422; JEV is absent from
`knowledgeBaseModelKinds`; the Console JEV test posts
`{kind:"jev",name,base_url,model,apikey}` and never calls `/models`.


## Console 系统监控 page (admin-only) and read-only overview API — 2026-11-11

Delivered in worktree `add-console-monitor-page` (feature commit `b7d9a34`,
rebased onto `main`@`9d55b6e` after the batch-delete, JEV and compact-admin
commits):

- `GET /v1/admin/monitor/overview` (`authAdmin`, `Cache-Control: no-store`):
  service identity, five parallel dependency probes (MySQL / Redis / Qdrant /
  Kafka / pdf-service, 1.5 s each), queue aggregates (`document_jobs` by status
  and stage, `document_job_outbox`, `search_tasks`, unfinished uploads),
  library counts and 24 h activity. Only COUNT/MIN/GROUP BY queries; no
  document content, query text or credentials are read or logged.
- Console「系统监控」page (`#/monitor`): service card, dependency status table,
  queue/backlog blocks, library counters, 24 h activity; 15 s auto-refresh with
  a switch, manual refresh, AbortController for stale responses, degraded cards
  when the state store is unavailable.
- Admin-only on all three layers: the shared `canViewConsolePage` rule hides the
  sidebar entry and redirects a direct `#/monitor` link for non-admins, and the
  REST route rejects a non-admin login session with 403「需要管理员权限」even
  from loopback. Legacy no-session local mode keeps management pages visible,
  matching the existing model-profiles / knowledge-bases / users behaviour.
- `internal/state/monitor.go` (+ test), `internal/monitor/` (+ test),
  `internal/httpapi/monitor.go` (+ test), one OpenAPI row, and doc updates in
  `README.md`, `docs/USAGE.md`, `docs/DEPLOYMENT.md`, `docs/agents/features.md`,
  `docs/agents/architecture.md`.

Verification (Windows 11, Go 1.27.3, Node v26.13.0 / npm 12.2.2; MySQL container
on 127.0.0.1:3307 with the disposable `praxis_gotest` schema):

```
$ go build ./... ; go vet ./...                              # exit 0
$ go test -p 1 -count=1 -timeout 20m ./internal/monitor/
  # ok 2.0 s — healthy, degraded/disabled, dependency error, Stats failure, timeout
$ go test -p 1 -count=5 -timeout 10m ./internal/state/ -run 'Monitor'
  # ok 1.5 s — aggregates / empty / query-failure, repeated to prove row ids unique
$ go test -p 1 -count=1 -timeout 20m ./internal/httpapi/
  # ok — 200 + no-store, 503 without runtime, non-loopback 403,
  # non-admin session 403 (runtime not called), admin 200
$ cd frontend && npm run typecheck                            # exit 0
$ cd frontend && npm test                                     # 10 files / 145 tests passed
$ cd frontend && npm run build                                # exit 0, >500 kB chunk advisory only
```

Isolated smoke (worktree binary on 127.0.0.1:18012 against `praxis_gotest`; the
production `praxis_rag` schema was not touched and no second long-running
service was started): `GET /v1/admin/monitor/overview` returned 200 with
`Cache-Control: no-store` and `ok:true`; mysql / redis / pdf-service reported
`ok`, qdrant `degraded` (collection absent in the empty test schema), kafka
`disabled` (unconfigured); `GET /console` returned 200.

Notes: rebasing onto `main` conflicted only in `README.md` and
`frontend/src/styles.css`, both resolved additively. The state test originally
derived every row id from one shared suffix, which could collide on the
`document_jobs` primary key (~1/3 of runs); it now uses a fresh random id per row
and passed `-count=5`. `go test ./...` for unrelated packages (e2e, cmd, workers)
was not rerun.

## Console 批次上传与逐文件进度气泡 — 2026-11-12

Changed (backend, Go):

- New tables `document_batches` (id, namespace, owner, idempotency_key,
  status, total/accepted/skipped/failed, timestamps) and `document_batch_items`
  (batch_id, position, name, status, reason, artifact_id, error, timestamps),
  appended after the ported tables in `internal/state/schema.go` and covered by
  `schema_test.go`. They store progress metadata only; every admitted file is
  still a `document_jobs` row with its bytes.
- `internal/state/document_batches.go`: Create / Finish / Get / ListActive /
  Prune / RunRetention; stored lifecycle is `saving` → `active`, while
  `processing`/`completed`/`failed`/`interrupted` are derived from the items and
  their jobs. An all-skipped batch is `completed`; an all-failed one `failed`.
- `internal/state/document_jobs.go`: `insertJob` now serializes on the per-file
  lease row (`guardName` in `internal/state/uploads.go`) instead of the
  namespace-wide guard, and reads `knowledge_bases.version` with `FOR SHARE`
  instead of `FOR UPDATE`, so different file names admit in parallel while the
  same name keeps its check-then-insert atomic. The idempotency probe no longer
  takes a row lock (see bug below). Typed `DocumentExistsConflict` /
  `DocumentQueuedConflict` keep the existing messages while letting the batch
  path classify skips.
- `internal/runtime/documents_batch.go`: `SubmitDocumentBatch` commits the batch
  record, fans out admissions with a 6-way semaphore on a disconnect-proof
  context, settles every item and returns the Console payload;
  `GetDocumentBatch` / `ListDocumentBatches` (restore list only contains
  unfinished work, owner-scoped for sessions). `internal/httpapi/documents_batch.go`
  + routes + OpenAPI rows: `POST /v1/document-batches` (202),
  `GET /v1/document-batches/{batch_id}`, `GET /v1/document-batches?limit=20`;
  `/v1/documents` and `/v1/documents/check` are unchanged.
- Retention: `RunRetention` prunes settled batch rows after seven days from
  `cmd/rag-service/main.go` (hourly + at startup).

Bug found during real-service verification: the old
`SELECT ... WHERE namespace = ? AND idempotency_key = ? FOR UPDATE` inside
`insertJob` takes a gap lock for a missing key; once admissions for different
names run concurrently, two such gap locks plus their inserts deadlock and one
file failed with the generic "Document submission failed" message. Removing the
row lock (the unique index plus the existing unique-violation replay fallback
already make retries correct) made 4/4 repeated 3-file batches report
`accepted 2 / skipped 1 / failed 0`.

Changed (frontend, `frontend/`):

- `api.ts`: document-batch types plus `submitDocumentBatch` (XHR multipart with
  `files`, Idempotency-Key, CSRF/Authorization headers, upload progress, 401
  handling, AbortSignal), `getDocumentBatch`, `listActiveDocumentBatches`.
- `uploadBatches.ts`: module store + `useSyncExternalStore`; splits a selection
  into ≤10-file batches (two concurrent uploads), polls every 1500 ms, stops on
  terminal states, `setOnSettled` once, `restore` (saving/processing/interrupted
  only), per-file retry, `clearFinished`, `bindNamespace` (drops the previous
  workspace's finished rows on a workspace switch), injectable timers.
- `DocumentsPage.tsx`: modal is selection-only and closes immediately after
  handing files to the store; the toolbar upload button becomes a small chip
  (`保存中/处理中/已完成 x/y`) that opens an Ant Design Popover bubble with
  per-file state, stage text, retry and clear actions. Navigation is not locked
  (workspace switching stays locked in `Console.tsx`).
- `styles.css` bubble/chip styles; `uploadBatches.test.ts` 10 cases.

Verification (Windows 11, Go 1.27, Node 26; MySQL `praxis_gotest` test schema;
`praxis_rag` used only for the transient end-to-end uploads, whose rows were
deleted afterwards):

```
$ go build ./... ; go vet ./internal/state ./internal/httpapi ./internal/runtime ./internal/wire ./cmd/rag-service   # exit 0
$ go test -p 1 -count=1 ./internal/state ./internal/migrate ./internal/httpapi     # ok
$ go test -p 1 -count=1 ./internal/runtime ./cmd/rag-service                       # ok
$ go test -p 1 -count=15 ./internal/state -run 'TestConcurrent'                    # ok
$ cd frontend && npm run typecheck                                                 # exit 0
$ cd frontend && npm test                                                          # 11 files / 155 tests
$ pwsh -File scripts/build-go.ps1 -SkipGo                                          # vite build + copy to dist/web
  # [ok] frontend -> dist/web (203 files); [ok] smoke: /health 200, /console serves index.html
```

Real service (rebuilt `dist/bin/rag-service.exe`, restarted on 127.0.0.1:8011):

- `POST /v1/document-batches` with `it2-…-a.md`, `it2-…-b.md`, `it2-…-a.md` →
  `202 {status:"processing", total:3, accepted:2, skipped:1, failed:0}`; the
  duplicate name was classified `skipped/queued`. Four consecutive batches
  produced the same 2/1/0 split (no internal failures).
- Replaying the same `Idempotency-Key` returned the same `batch_id` with the
  same counters and did not create new `document_jobs` rows.
- A batch whose only file already exists (`cdc-probe-…txt`) → `skipped/exists`
  and `status:"completed"` (all-skipped is not a failure).
- `GET /v1/document-batches/{batch_id}` returned the per-file `job.stage`
  (`retry_wait` while the worker backs off) and `GET /v1/document-batches`
  excluded the completed batch while still returning the processing one, so a
  reload only restores pending work.
- The rebuilt Console bundle served at `/console/assets/assets/index-BIf5ugcH.js`
  contains the bubble strings; the Go service serves it via `dist/web`.

Not verified: manual browser click-through of the new bubble (unit tests,
typecheck, production build and the served bundle were verified); end-to-end
parse/embedding of batch-uploaded files depends on the target knowledge base
having embedding/VLM configuration — the `manuals` KB used for the smoke has an
empty embedding base URL, so its jobs stayed in worker backoff while the batch
progress itself was correct.

## 系统监控页/API 六项语义修复 — 2026-11-13

工作树 `dsh-2c70f4b2-96c9`（Windows 11，Go 1.27.3，Node v26.13.0 / npm 12.2.2）。
目标：修复监控页六个问题——首屏与布局、总体状态与依赖探测、软删除文档计数、
Outbox 待投递语义、上传操作混合计数、检索次数与结果快照数据源。

**代码改动**

- `internal/monitor/monitor.go`：顶层新增三态 `status`（`ok`/`degraded`/`error`，
  `ok` 兼容字段 = `status == "ok"`）；`overallStatus` 聚合依赖与流水线视图
  （状态库不可读/依赖 error/Debezium 检查失败 → error；依赖降级/Worker 心跳过期/
  Kafka 进度读取失败 → degraded；未启用/未知中性；`document_jobs` 负载不参与）。
  `queues.uploads` 增加 `by_status`/`in_progress`/`failed`/`rolling_back`/
  `cleanup_pending`（保留 `unfinished` 并集）；`pipeline` 增加 `worker`
  `{status, detail}`；`library.result_snapshots` 改为可空（Redis 计数）；
  Kafka 依赖改为 broker 元数据探测（连通性/TLS-SASL/主题/分区），PDF 依赖解析
  `/healthz`（转换器、版本、配置错误），Debezium 解析 `/q/health` 的 `checks[]`
  并点名失败项。
- `internal/state/monitor.go`：`documents` 只统计 `del_flag = 0`；删除
  `ResultSnapshots`（改由 Redis 计数）；`upload_operations` 单条 `GROUP BY
  status, cleanup_pending` 推导互斥分类与并集 `unfinished`；`SearchesRecent`
  改读活动计数器（覆盖同步 REST/MCP 与排队检索，失败也计数）。
- 新增 `internal/state/activity.go`（小时桶 `activity_counters`）、
  `internal/state/heartbeat.go`（`runtime_heartbeats` + 24h 清理 +
  `HeartbeatSnapshot`）、`internal/documentworker/heartbeat.go`
  （`StartHeartbeat`、默认 15s、`RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS` 覆盖）。
- 新增 `internal/mq/probe.go`（`BrokerHealth`：Metadata 请求，证明认证与主题）、
  `internal/redisstate/snapshot_index.go`（`<prefix>:index:search-results` ZSET，
  `Add`/`CountActive` 清过期后计数），`internal/state/result_snapshots.go`
  增加可选 `SnapshotIndex`，`redisstate.Attach` 自动装配。
- `internal/runtime/service.go`：`SetActivityRecorder`，`Service.Search` 每次调用
  静默记一次活动计数。
- `cmd/rag-service`：接线活动计数器、Kafka 元数据探测、Redis 快照计数、Worker
  心跳读取。`cmd/rag-document-worker`：独立 worker 启动心跳。（合并到 main 时
  文档 worker 已固定为独立进程、进程内模式移除，因此心跳只由
  `rag-document-worker` 写入，监控的 `pipeline.worker` 也固定读该角色。）
- `frontend/src/MonitorPage.tsx`、`api.ts`、`styles.css`：按重要性重排（页头状态 →
  关键指标首卡 → 依赖与投递 → 队列与记录 → 服务信息），阶段分布为空不再渲染空
  表格，上传拆成四个互斥分类并说明「未完成操作」含义，未知显示「未知」；窄屏
  单列、表格横向滚动与长文本折行。
- 文档：`docs/USAGE.md`（监控语义与新增环境变量）、`docs/agents/architecture.md`、
  `docs/agents/features.md`、`docs/agents/development.md`、`docs/state-storage.md`
  （新表）、`docs/state-cache.md`（快照索引）、`.env.example` 同步。

**定向测试**（仅相关包，未跑全量）

```
$ go build ./...                                                    # exit 0
$ go vet ./internal/monitor ./internal/state ./internal/runtime \
         ./internal/documentworker ./internal/mq ./internal/redisstate \
         ./cmd/rag-service ./cmd/rag-document-worker                # exit 0
$ RAG_TEST_MYSQL_URL=mysql://rag:<env>@127.0.0.1:3307/praxis_unit_test \
  go test -p 1 -count=1 -timeout 20m ./internal/state ./internal/monitor \
    ./internal/mq ./internal/redisstate ./internal/documentworker \
    ./internal/runtime ./internal/httpapi ./internal/searchrefs
ok  internal/state 21.995s        ok  internal/monitor 2.777s
ok  internal/mq 0.672s           ok  internal/redisstate 4.767s
ok  internal/documentworker 2.672s  ok  internal/runtime 50.533s
ok  internal/httpapi 2.218s      ok  internal/searchrefs 1.276s

$ cd frontend && npm run typecheck                                  # exit 0
$ cd frontend && npx vitest run src/MonitorPage.test.tsx src/api.test.ts
  # 2 files / 44 tests passed
```

**实机验证**（复用 Compose 基础设施：MySQL 3307 / Redis 6379 / Qdrant 6333 /
Kafka 9092 / Debezium 8083 / pdf-service 8091；本次构建的 `rag-service.exe`
运行在 127.0.0.1:8012，状态库指向一次性 `praxis_unit_test`、Redis 使用独立
`rag:monitorcheck` 前缀，避免干扰 8011 上的旧实例与生产数据）

- 正常态：`status:"ok"`、`ok:true`；Kafka 显示「元数据正常（1 个 broker；主题
  praxis_document_ingest 共 4 个分区）」；PDF 显示「转换器 docling，版本 1.0.0」；
  Debezium 显示「健康检查正常（4 项运行检查通过）」；`queues.uploads` 输出五个
  状态与四个分类；`pipeline.worker` 在无心跳时显示「未知」。
- Worker 心跳：向 `runtime_heartbeats` 写入当前时间 → `pipeline.worker` 正常
  （「最近心跳 2.3 秒前，1 个实例运行中」）；改成 120 秒前 → `degraded`
  （「worker 可能已停止」）且顶层 `status` 变 `degraded`；删除行 → 未知。
- 结果快照：向 `rag:monitorcheck:index:search-results` ZADD 一个未来过期成员 →
  `library.result_snapshots: 1`；改成过去时间 → 清理后回到 `0`。
- 检索次数：用本机管理员创建 API Key 后调用 `POST /v1/search`（测试库无该知识库，
  请求返回 400）→ `activity.searches` 从 0 变 1，`activity_counters` 写入
  `metric=search`，证明失败检索也计数。
- 降级一致性：把 Qdrant 目标集合指向不存在的集合 → 依赖行 `degraded`
  （「集合 … 不存在」），顶层 `status:"degraded"`、`ok:false`；此前旧构建在同样
  场景下仍返回 `ok:true`。

未验证/限制：未在浏览器里人工点检窄屏断点（单列、横向滚动与折行由 CSS 与
jsdom 测试覆盖）；结果快照索引只覆盖上线后保存且在 TTL 内的快照，索引前保存的
快照会短暂少计。

随后用 `scripts/build-go.ps1` 重建了本工作树的 `dist/bin` + `dist/web`（冒烟通过），
并把它部署到本机 127.0.0.1:8011（替换旧进程，`rag-document-worker` 一并启动）：
`/health` 200；`/v1/admin/monitor/overview` 返回 `status:"degraded"`、`ok:false`
（Qdrant 目标集合 `mini_agent_knowledge_pdf_assets_v1` 不存在），Kafka/PDF 显示
元数据与转换器检查结果，`pipeline.worker` 正常，`library.documents=8`（旧构建为
24，已排除软删除），`library.result_snapshots` 来自 Redis；`/console` 提供的
`index-sut3qI55.js` 包含「关键指标」「存在降级」「未完成操作」等新文案。

该分支随后合并到 `main`（合并提交 `5b4c360`），并与 main 上「Kafka 必需、移除
进程内文档 worker」「账号邮箱验证/系统设置」等改动对齐：文档 worker 固定为独立
进程，心跳只由 `rag-document-worker` 写入，监控响应不再包含
`service.document_worker`，`pipeline.worker` 固定读 `document-worker` 角色；
`activity_counters`/`runtime_heartbeats` 与 main 新增的 `user_avatars`/
`email_verification_codes`/`app_settings` 表共存。合并后在 main checkout 重新执行
`scripts/build-go.ps1`（冒烟通过）并重启 8011 的 `rag-service` +
`rag-document-worker` + `rag-dead-letter-worker`：`/health` 200，监控接口仍为
`status:"degraded"`（同一缺失集合）、`pipeline.worker` 正常、`documents=8`、
Kafka 元数据与 PDF 转换器检查正常；`/console` 提供的新 bundle
`index-Dsbwy_6w.js` 包含「关键指标」「存在降级」「未完成操作」文案。

## PDF 解析统一为远端视觉模型 — 2026-11-13

工作树 `dsh-c1c4018e-d12f`（Windows 11，Go 1.27.3，Node v26.13.0 / npm 12.2.2，Python 3.11）。
目标：移除 PDF 本地推理（docling / torch / easyocr / CodeFormula / 模型卷），改为**一个按知识库
选择的远端 OpenAI 兼容视觉模型**统一承担版面、OCR、表格、公式与图片描述；本地只做 PDF 读取、
渲染、裁图与清单组装；证据格式与接口保持稳定。

**改动范围**

- `python-workers/pdf-service`：重写为 PyMuPDF 读取/渲染/裁图 + 视觉客户端 + 清单组装；删除
  docling/torch/easyocr/CodeFormula 依赖与 `RAG_PDF_MODELS_PATH`/`pdf_parser_models` 模型卷；
  `RAG_PDF_SERVICE_CONVERTER` 默认 `vision`（旧值 `docling` 作为兼容别名）；`options` 键名规范为
  `vision`（`vlm` 兼容别名，`ocr` 键废除 → 422）；`manifest.version` 仍是 `docling-service-v1`
  （证据格式契约）。
- Go：新增 `internal/document/pdf_service.go`（替换 `docling_service.go`），`RAG_PDF_ENGINE=vision`
  默认（`docling` 兼容别名）/`go`；vision 引擎下知识库未配置 VLM 时受理阶段返回固定中文提示；
  配置迁移删除 `kind='ocr'` 模型配置并清除知识库遗留 `ocr`/`ocr_model` 设置。
- 前端：模型类型收敛为 `embedding` / `reranker` / `vlm` / `jev`；「模型配置」页删除 OCR 页签与
  引擎下拉，创建知识库删除 OCR 开关/字段（可选模型为 Reranker / VLM），模型卡片改为自适应三列；
  `api.test.ts`、`CreateKnowledgeBase.test.ts`、`ModelProfilesPage.test.tsx`、`MonitorPage.test.tsx`
  同步更新。
- 部署与文档：`docker-compose.yml` 的 `pdf-parser` 删除 torch 构建参数与模型卷、默认 converter 改
  `vision`；`scripts/start-stack.ps1`、`.env.example` 同步；README、USAGE、DEPLOYMENT、
  pdf-service README、agents 文档、`AGENTS.md`、GO_MIGRATION/GO_PARITY_AUDIT 的迁移说明更新。

**验证（本工作树实际执行）**

- `cd python-workers/pdf-service && python -m pytest -q`：**176 passed**（离线；视觉客户端注入
  fake / mock transport，不访问网络）。
- `go build ./...`：通过。
- 改动范围定向测试：
  `go test -count=1 ./internal/config/... ./internal/document/... ./internal/kbconfig/... ./internal/monitor/... ./internal/ingest/...`
  → `config` / `document` / `document/pdfcontent` / `kbconfig` / `monitor` / `ingest` 全部 **ok**。
- `internal/runtime`、`internal/httpapi`、`internal/state`、`internal/documentworker` 含依赖真实
  MySQL 的用例（需 `RAG_TEST_MYSQL_URL`），本次未在该环境执行完整包；其中的纯逻辑用例由 Go 实现
  方单独确认通过。
- 前端：`cd frontend && npm run typecheck` 通过；`npm test` → **14 个测试文件 / 188 个用例全部通过**
  （含本次更新的 api / CreateKnowledgeBase / ModelProfilesPage / MonitorPage 用例）。
- 文档一致性：在 `README.md`、`docs/**`、`AGENTS.md`、`docker-compose.yml`、`scripts/**`、
  `.env.example` 中检索 `docling|torch|easyocr|CodeFormula|pdf_parser_models`，剩余命中仅为
  「兼容别名」「证据契约名（docling-service-v1）」或明确标注「已移除/已废弃」的历史说明。

**未验证 / 限制**

- **真实视觉端点端到端**：未用真实 OpenAI 兼容端点 + 真实 API Key 跑通上传→解析→检索（含限流、
  超时、失败重试行为）。
- **真实 PDF 解析质量**：未验证真实扫描件/复杂排版 PDF 的版面、OCR、表格与公式准确率。
- **Docker**：未构建 `pdf-parser` 镜像，未执行 `docker compose up` / `app` profile。
- **MySQL 相关测试与迁移**：未在真实 MySQL 上执行依赖 `RAG_TEST_MYSQL_URL` 的测试，也未实机执行
  升级迁移（`kind='ocr'` 删除、`ocr`/`ocr_model` 清理）的验证与回滚演练。
- 前端仅有 jsdom/Vitest，没有浏览器 E2E；三张模型卡片的自适应网格窄屏断点未人工点检。
## 内容去重与批量删除 — 2026-11-12

目标（用户原话）：MySQL 中同一份文件只保留一份内容，Qdrant 保持现状（每库独立
collection、独立 point）；删除逻辑改为「没有引用才删除内容」，批量删除要组合成
一次 SQL/Qdrant 操作，而不是「批量数量 = 调用次数」。

### 已交付

- 新表 `content_objects(hash CHAR(64) PK, size, content LONGBLOB, created_at)`：
  按 SHA-256 内容寻址，一份文件内容只存一行。
- 引用列：`upload_objects.content_hash`（新增，含 `ix_upload_objects_content`）与
  `blobs.sha256`（复用，新增 `ix_blobs_sha256`）。引用行保存空 `content` 占位，
  存储开销只与「租户/文档 × 对象」数量相关，与文件大小无关。
- 读写：`UploadJournal.PutObjectTx` / `BlobStore` 写入前先 `INSERT IGNORE`
  内容行再写引用；`GetObject` / `BlobStore.Get` 走 `LEFT JOIN content_objects`，
  对历史行自动回退读取内联 `content`。
- 迁移：`ensureSchema` 幂等新增表/列/索引；维护循环 `BackfillContentObjects`
  每轮最多迁移 100 行历史内联内容（上传对象 + blobs），旧进程/旧行始终可读。
- 回收：所有删除引用行的路径（文档删除、上传回滚/空提交/旧版本清理、blob
  覆盖、知识库批删）都会把失去最后一个引用的内容行 `retired_at` 置为当前时间；
  `PurgeUnreferencedContent` 只删除 `retired_at` 已超过保留期（8 天，>1 周）且
  没有 `upload_objects.content_hash` / `blobs.sha256` 引用的内容行。期间若有新
  引用写入，插入路径会清除 `retired_at`，保证活跃内容不会被误删。
- 批量删除：`KnowledgeBaseRepository.Purge(namespaces...)` 在**一个事务**内完成
  全部 namespace 的清理，语句数固定（约 30 条，与文档数/namespace 数无关）：
  先把 upload_operations / content_hash / search result_id 物化到临时表，再逐表
  `DELETE ... IN (SELECT ...)`；缓存修订与 outbox 用 4 条集合语句批量写入；
  `content_objects` 用 `JOIN 临时 hash 表 + NOT EXISTS` 标记 `retired_at`（不立即
  删除字节）。
- Qdrant：每个被删知识库只发一次 `DELETE /collections/{name}`，且仅当没有其他
  知识库引用该 collection；删除失败尽力而为，不影响 MySQL 结果。
- HTTP/接口：`KnowledgeBaseAdminRuntime` 改为
  `PurgeKnowledgeBases(ctx, []string)`；单删与批删共用同一批处理入口，响应结构
  （`deleted` / `missing` / `failed` / `documents_deleted` /
  `configurations_deleted`）保持不变，前端无需改动。
- 冲突语义：namespace 仍有活跃上传租约（`document_leases.expires_at > now`）时，
  该 namespace 进入 `failed` 且数据完全不动（同等旧版 409 路径）。

### 验证（2026-11-12，本 worktree，Go 1.27.3，MySQL 容器 127.0.0.1:3307）

```
$env:RAG_TEST_MYSQL_URL = "mysql+pymysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4"
go build ./...                                                # exit 0
go vet ./...                                                  # exit 0
go test -p 1 -count=1 -timeout 25m ./...                      # exit 0，全绿
# internal/state 30.3s / internal/runtime 38.6s / internal/e2e 8.9s /
# internal/httpapi 1.9s / internal/migrate 3.1s
```

新增/更新测试：

- `internal/state/content_test.go`（新增）：
  - 跨 namespace 相同内容只产生 1 行 `content_objects`，两条引用都能读回原文；
  - blobs 与 upload_objects 共享同一内容行；
  - 删除全部引用后 GC 删除内容行；仍有引用时 GC 不删；
  - 历史内联行 backfill 后 `content_hash`/`sha256` 生效、内联字节清空、读取结果
    不变，且重复 backfill 幂等；
  - 一次 `Purge` 删除 2 个 namespace 的全部行（documents/heads/permissions/
    mcp_catalogs/blobs/upload_operations），其他 namespace 与共享内容保留，
    `kb_list` 修订被批量 bump；
  - 活跃租约 namespace 返回 `Busy` 且一行未动；
  - 300 文档 + 300 上传引用的批处理删除：**55.9ms**（单个事务）。
- `internal/runtime/knowledge_base_test.go`：批处理 purge 正常删除 2 个文档并
  清空列表；活跃上传租约时保留配置行以便重试。
- `internal/httpapi/kb_purge_test.go` / `kb_admin_test.go`：单删 204/404/409、
  批删 `deleted`/`missing`/`failed` 分桶、非 loopback 403、运行时错误 503 不变。
- `internal/state/schema_test.go`：`content_objects`、`upload_objects.content_hash`、
  `ix_upload_objects_content`、`ix_blobs_sha256` 纳入 schema 断言。

正式实例（`dist/bin` 新二进制 + `praxis_rag` 库，2026-11-12 08:52 重启）：

```
information_schema: content_objects 表、upload_objects.content_hash、
                    ix_upload_objects_content、ix_blobs_sha256 均已自动迁移
POST /v1/admin/knowledge-bases/batch-delete  [a(150 文档), b(1), missing]
  -> 200 {"deleted":[a,b],"missing":[missing],"documents_deleted":151,
          "configurations_deleted":2,"failed":[]}，服务端 120ms
POST /v1/admin/knowledge-bases/batch-delete  [d(300 文档)]
  -> 200 {"deleted":[d],"documents_deleted":300,...}，服务端 79ms
DELETE /v1/admin/knowledge-bases/{c}         -> 204，70ms
DELETE /v1/admin/knowledge-bases/{missing}   -> 404
事后校验：documents/upload_operations/upload_objects/blobs/kb_permissions/
          knowledge_bases/document_heads 全部为 0；共享 payload 的
          content_objects 行在最后一个引用删除后只剩 1 行（2026-11-12 调整后为
          retired_at 置位、8 天后才由 GC 删除，见下方「保留期调整」）
```

对照旧实现：同一批 300 文档要走 300 次 `deleteDocumentByName`（每次一个
`journal.WithLock` + 一个事务），这正是「删除时间太久」的原因；现在是 1 个事务
+ 每库 1 次 Qdrant 集合删除。

已知限制/未验证：

- `content_objects` 与引用行的写入在同一事务内，但内容行采用「最后引用消失时
  写 `retired_at` + 保留 8 天」的延迟回收，且维护循环每轮最多删 100 行，因此删除
  后存储空间不会立即释放；保留期由 `state.ContentObjectRetentionSeconds` 控制。
- Qdrant 仍然每库一份向量、按 collection 删除；跨租户向量共享没有实现（用户已
  确认保持现状）。
- 历史遗留的 5 条 `asdjasdajbsc` 命名空间上传操作（workspace 记录在旧的
  `...\Temp\dsh-*\rag-txn-*` 下）仍会每 10s 记录一次
  `cleanup failed (ValueError)`：这是本次改动之前就存在的数据（workspace 根目录
  不匹配），与内容去重/批量删除无关；用新批量删除清掉该 namespace 时会一并清理。

### 保留期调整（2026-11-12 追加）

用户要求：内容字节在「没有引用」后要保留超过 1 周再删除，而不是 600 秒。

- 新列 `content_objects.retired_at DOUBLE NULL`（+ `ix_content_objects_retired`），
  `ensureSchema` 幂等迁移旧库；DDL 禁用部署需手工补该列/索引（见
  `docs/state-storage.md` 的 DDL 片段）。
- 所有删除引用行的路径统一标记 `retired_at`：文档删除
  (`deleteDocumentContentRefsTx`)、上传回滚/空提交/旧版本清理
  (`deleteUploadObjectsTx`)、blob 删除与覆盖 (`blobUpsert` 等)、知识库批删
  （临时 hash 表 + `NOT EXISTS`）。标记与引用删除在同一事务内，回滚即撤销。
- 插入路径 `INSERT IGNORE` 命中已有内容行时清除 `retired_at`，即使内容先被
  标记、之后又有新引用，也不会被 GC 误删。
- GC 条件：`retired_at IS NOT NULL` 且 `retired_at < now - 8 天` 且无任何引用；
  保留期常量 `state.ContentObjectRetentionSeconds = 8 * 24 * 60 * 60`（>1 周）。
- 相关测试：`internal/state` 的 `TestContentObjectsDedupAcrossNamespaces`、
  `TestContentRetirementRearmsOnNewReference`、
  `TestContentObjectsGCKeepsReferencedRows`、`TestContentBackfillMovesLegacyRows`、
  `TestKnowledgeBasePurgeBatchDeletesNamespaceRows`、
  `TestKnowledgeBasePurgeBatchScalesWithDocuments`、schema 断言；全部通过。
- 生产实例重启后 `information_schema` 已出现 `retired_at` 列与
  `ix_content_objects_retired` 索引；批量删除返回结构不变。

## 合并到 main 时的 CDC 适配 — 2026-11-14

内容去重与批量清理分支（`b602bb7` + `d3b87bf`）在 `main`@`9690452` 上重放，适配了
11-13 Debezium CDC 迁移与 11-14 搜索同步化之后的 state 结构：

- `cache_events.touch` 已不再写 `cache_outbox`（事务 outbox 已下线）；批量清理改为
  删除 `cache_revisions` 行后调用 revision-only 的 `touch`，不再插入 outbox。
- 批量清理按当前 schema 调整表集合：去掉已不存在的 `cache_outbox` /
  `document_job_outbox` / `search_task_outbox`，新增 `document_batches` /
  `document_batch_items`（先物化到 `tmp_purge_batches`，再按 batch_id 删除）。
- 维护循环的 GC 调用改为 `PurgeUnreferencedContent(ctx, state.ContentObjectRetentionSeconds, 100)`，
  即 `retired_at` + 8 天保留策略；`logBackgroundFailure` 按当前签名补 `ctx`。
- `schema.go` 末尾追加 `content_objects(hash,size,content,created_at,retired_at)`，并为
  `upload_objects.content_hash` / `ix_upload_objects_content` / `ix_blobs_sha256` /
  `ix_content_objects_retired` 补迁移与 schema 断言。
- 文档：`docs/state-storage.md` 的删除与保留语义改为 `retired_at` + 8 天。

验证（2026-11-14，Windows 11，Go 1.27，MySQL `praxis_gotest` 一次性测试库）：

```
go build ./...                                                                                                         # exit 0
go vet ./internal/state ./internal/runtime ./internal/httpapi ./internal/wire ./internal/migrate ./cmd/rag-service     # exit 0
go test -p 1 -count=1 ./internal/state -skip TestMigrationsAddMissingLegacyColumns                                      # ok
go test -p 1 -count=1 ./internal/runtime ./internal/httpapi                                                             # ok
go test -p 1 -count=1 ./internal/migrate                                                                                # ok
```

说明：`internal/state` 的 `TestMigrationsAddMissingLegacyColumns` 在合并前的主干
（`9690452`）上已经失败（退役的 `search_tasks` 仍声明 `schedule_version`，与“已退役表
不应再迁移回该列”的测试期望不符），与本次去重/批量清理无关，因此验证时用 `-skip`
排除该基线失败。

## Console 逐文件上传进度（SSE）与每文件保存结果 — 2026-11-14

范围：`GET /v1/document-batches/upload-events/{upload_id}` SSE 端点、`POST /v1/document-batches` 的可选 `X-Upload-ID` 头、Console 逐文件进度条与每文件保存结果，以及 README / `docs/USAGE.md` / `docs/agents/features.md` / `PROGRESS.md` 的同步。工作目录 `D:\Project\Praxis-RAG\.agents\worktrees\dsh-074b99dd-e98a`（Windows + MSYS bash，Go 1.27.3，Node v26.13.0，vitest 5.1.0）。

后端：

```
go build ./...                                                                # exit 0
go vet ./internal/httpapi                                                     # exit 0
go test ./internal/httpapi -run 'TestDocumentBatch|TestUpload' -count=1       # ok 1.196s
go test -race ./internal/httpapi -run 'TestDocumentBatch|TestUpload' -count=1 # ok 2.327s
```

说明：`TestUploadEventStreamReportsPerFileProgressAndResults` 用 `httptest.NewServer` 起真实 HTTP 服务，先订阅并读到 `ready` 再异步发 POST，校验 `Content-Type: text/event-stream`、`Cache-Control: no-store`、5 MiB 大文件的逐次 `progress.received` 累计递增、两个文件各自的 `result`（`saved` / `failed`）与末尾 `done`；同组用例还覆盖不带 `X-Upload-ID` 的旧行为、非法 upload id 返回 400、缺租户凭据 401、上传校验失败仍发 `done`（`TestUploadValidationErrorEmitsDone`）。

前端：

```
cd frontend
npm run typecheck                                                      # exit 0（tsc --noEmit）
npx vitest run src/uploadBatches.test.ts src/api.test.ts               # 2 files, 60 tests passed
npx vitest run                                                         # 17 files, 235 tests passed
npm run build                                                          # exit 0（vite 3117 modules；build/web/index.html + assets）
```

说明：定向用例覆盖"先订阅后 POST"、逐文件进度与同名文件不串行、单文件失败不影响其他行、响应核对补齐、恢复/重试与徽标文案，以及 SSE 跨 chunk / CRLF 分帧、HTTP 错误与 `close()` 中断；契约补充的 XMLHttpRequest 断言（`submitDocumentBatch` 发送相同的 `X-Upload-ID`、无 `uploadId` 时不同）也在 60 例内。全量运行 17 文件 235 用例全部通过；单独运行 `src/LoginPage.test.tsx` 仍可见 4 条既有 jsdom teardown 的 `window is not defined` 未捕获异常（该文件 6 个用例通过、退出码 0），与本次改动无关。前端生产构建通过：`npm run build` exit 0，vite 转换 3117 个模块、复制 198 项静态资源，产物 `build/web/index.html` 与 assets（`index-*.js` 1,322.16 kB、`index-*.css` 18.68 kB、`PdfPreview-*.js` 432.76 kB、`pdf.worker.min-*.mjs` 1,265.41 kB），仅提示部分 chunk 超过 500 kB，无错误。

未实机项：

- 未在真实浏览器中打开 Console 观察进度条动画与真实 SSE 连接（仅 jsdom 单测 + `typecheck` + 生产构建覆盖）。
- 未用真实大文件（如 8 MiB PDF）在真实 `rag-service` + MySQL/Kafka 环境实测上传；SSE 端到端仅在 httptest + fake runtime 下验证，未接真实文档 worker 观察解析阶段。
- 多实例 `rag-service` 下的进度流失效属于设计边界（进程内 hub），未做负载均衡实测；需要会话粘滞或单实例部署。

## SAG2 合并到 main 的适配 — 2026-11-14

`sag2-lexical-retrieval`（`3e39327f`）在 `main`@`3447f6b7`（已含内容去重/批量清理
合并与逐文件上传进度 SSE）上重放，适配搜索同步化之后的契约与运行时：

- `SearchToolSchema` 在搜索同步化中改为无参函数且下线 `queued` 搜索；SAG2 分支的
  `TestQueuedSearchRequestValidation` 已删除，SAG2 契约测试改用 `SearchToolSchema()`。
- `README.md` 模型配置段落取主干最新措辞（VLM 为唯一远端视觉模型、OCR 本地引擎
  已移除），并把「JEV 仅保存配置」改为 `sag2` 每次查询读取最近更新的 JEV 配置。
- `internal/runtime` 的 `errorTypeName` 辅助函数随主干重构被删除，SAG2 的 JEV
  解析器需要它做安全错误分类，已在 `internal/runtime/sag2.go` 恢复该函数。
- 其余 SAG2 文件（`internal/retrieval/lexindex.go`、`sag2.go`、`internal/modelapi/jev.go`、
  `internal/qdrant/corpus.go`、`internal/runtime/sag2.go`、运行时装配与文档）自动合并。

验证（2026-11-14，Windows 11，Go 1.27，MySQL `praxis_gotest`）：

```
go build ./...                                    # exit 0
go vet ./internal/contracts ./internal/retrieval ./internal/modelapi ./internal/qdrant ./internal/runtime ./internal/httpapi ./internal/mcp   # exit 0
go test -p 1 -count=1 ./internal/contracts ./internal/retrieval ./internal/modelapi ./internal/qdrant ./internal/runtime ./internal/httpapi ./internal/mcp   # ok
cd frontend && npm run typecheck                  # exit 0
```

## 系统监控 Qdrant 集合判定对齐知识库实际入库集合 — 2026-11-14

工作树 `dsh-43becfb6-ff65`（Windows 11，Go 1.27.3）。目标：修复监控页把
`RAG_COLLECTION` 回退集合当作健康条件导致的误报——知识库创建时在配置中生成
`knowledge_*` 集合名，入库代码按配置写入并在首次入库时创建 Qdrant 集合；服务启动时却把
全局回退名传给监控，监控要求它存在。真实实例有 26 个 `knowledge_*` 集合、默认知识库使用
`knowledge_e0fd7ba996b0e4d3adeb72b1bde6e853` 且仅 1 条有效文档，页面仍报
「集合 mini_agent_knowledge_pdf_assets_v1 不存在」并降级。

**代码改动**

- `internal/state/monitor.go`：新增 `MonitorIndexedCollections`，返回 `documents` 中
  `del_flag = 0` 且 `collection <> ''` 的去重集合名（`ORDER BY collection`）；软删除文档
  不再要求集合存在。
- `internal/monitor/monitor.go`：`Options.Collection` 改为 `FallbackCollection`（只用于
  `service.collection` 展示）；新增 `IndexedCollections` 回调；`probeQdrant` 只校验真实入库
  集合：连接失败 → `error`；回调失败 → `unknown`（不误报 Qdrant）；缺失 → `degraded` 并点名；
  0 个入库集合（空库/空知识库）→ `ok`。`dependencies[].points` 保留字段但不再赋值（多集合
  没有单一聚合点数）。
- `cmd/rag-service/main.go`：注入 `IndexedCollections: stores.MonitorIndexedCollections`。
- 文档：`docs/USAGE.md`（`RAG_COLLECTION` 说明、监控语义、`points` 兼容字段）、
  `docs/agents/features.md`、`PROGRESS.md`。

**验证**

- 基线（旧服务 8011，同一真实栈）：`/v1/admin/monitor/overview` 返回
  `status:"degraded"`、`service.collection:"mini_agent_knowledge_pdf_assets_v1"`，qdrant 行
  `status:"degraded"`、detail「集合 mini_agent_knowledge_pdf_assets_v1 不存在」；Qdrant
  `/collections` 返回 26 个集合且确实不含该回退名。
- 单元/集成测试：
  - `go build ./...` → exit 0；`go vet ./internal/monitor ./internal/state ./cmd/rag-service` → exit 0。
  - `go test ./internal/monitor -count=1` → ok（新增
    `TestMonitorQdrantUsesIndexedCollections`：回退集合未创建不降级、入库集合缺失降级、
    多缺失点名、无入库集合、状态读取失败仅 unknown）。
  - `go test ./internal/state -run 'TestMonitor' -count=1 -p 1`（`RAG_TEST_MYSQL_URL` 指向
    `praxis_unit_test`）→ ok（`TestMonitorIndexedCollections` 覆盖去重、排序、软删除与空集合名）。
- 新构建实机 E2E（`go build -o .test-tmp/bin/rag-service.exe ./cmd/rag-service`，同一二进制与
  装配；`RAG_SERVICE_PORT=18011`，Qdrant 为真实实例）：由于 `rag-service` 启动会执行维护写操作，
  本次按任务允许的安全回退把状态库指向一次性 `praxis_gotest`（库名含 test，先插入 1 行
  `documents`：`del_flag=0`、`collection=knowledge_e0fd7ba996b0e4d3adeb72b1bde6e853`），Redis
  关闭、数据根指向 `.test-tmp`，未写生产库。`/health` 200；`GET /v1/admin/monitor/overview`
  返回 `status:"ok"`，qdrant 行为
  `{"status":"ok","detail":"集合 26 个；knowledge_e0fd7ba996b0e4d3adeb72b1bde6e853 已就绪"}`，
  `service.collection` 仍为 `mini_agent_knowledge_pdf_assets_v1`（兼容），`dependencies[]` 无
  `points` 键，旧回退集合不再被检查。随后杀进程、确认 18011 无监听并清理 `.test-tmp` 与 seed 行。
- 真实库只读核对：`praxis_rag.documents` 共 26 行（1 行有效、25 行软删除墓碑），唯一有效文档的
  collection 为 `knowledge_e0fd7ba996b0e4d3adeb72b1bde6e853`，与修复后监控期望集合一致；
  Qdrant 的 26 个集合包含该集合、不含 `mini_agent_knowledge_pdf_assets_v1`。

**未验证/限制**：未在真实浏览器中人工点检 Console 监控页（只验证 HTTP JSON）；`documents` 表由
入库发布同步写入，若集合被外部删除，监控在下一轮 15s 刷新会发现；不再展示单一"向量点"聚合数；
`documents.collection` 无索引，`DISTINCT` 查询在文档量很大时是全表扫描（当前 26 行无影响，本轮
不改 MySQL 结构；量大时可考虑加 `(del_flag, collection)` 索引）。

## 分布式 Kafka Connect 迁移（2026-11-15）

**改动范围**：新增 `internal/connect`（REST 客户端、fixed-name 幂等注册、FAILED task
supervisor）、`cmd/rag-connect`（register/wait/status/plugins/supervise/decode）、
`deploy/kafka-connect/connectors/*.json`；Compose 用 `connect-1`/`connect-2` +
`connect-init` 替代两个 Debezium Server；监控改为读取 connector/task 状态并展示
worker、失败原因、自动重启与恢复结果；启动脚本与 e2e 脚本覆盖注册、worker failover、
task 自动恢复与真实 Kafka 消息解码。

**本仓库已执行**

- `go build ./...` → exit 0。
- `go test ./internal/connect ./internal/monitor ./internal/mq -count=1` → ok（含新增的
  Connect follower 对写请求返回 409 时向 peer 重试的用例）。
- `go vet ./internal/connect ./internal/monitor ./internal/mq ./cmd/rag-connect ./cmd/rag-service`
  → exit 0。
- `go run ./cmd/rag-connect register --offline --connector-dir deploy/kafka-connect/connectors`
  → 两个模板各展开 31 项配置，固定名称正确。
- `go run ./cmd/rag-connect decode --kind document|cache` 用真实 Connect JSON 形状的
  文档通知与缓存删除事件 → 分别输出 `artifact_id/namespace/schedule_version` 和
  `table/op/before` 定位字段，exit 0。
- 前端 `npm run typecheck` → exit 0；`npx vitest run src/MonitorPage.test.tsx src/api.test.ts`
  → 77 tests passed（新增文档采集/缓存采集的 worker、task、重启耗尽、恢复结果断言）。
- `python -m py_compile scripts/verify-debezium-e2e.py` → exit 0（脚本含验收 2/3/4 与
  `rag-connect decode` 契约检查，但未连接真实栈执行）。

**未实机验证**

- 双 worker Connect 集群启动、注册与 rebalance；`docker stop` 承载 document task 的
  worker 后 peer 接管并从 `praxis_connect_offsets` 恢复。
- 锁定 `debezium_cache` 账号并 kill 连接后 task 进入 FAILED、解除后 supervisor 自动
  重启恢复；该演练依赖真实 MySQL/Debezium 错误分类。
- Compose `app` profile、`connect-init` 一次性容器与镜像内 `deploy/kafka-connect`
  路径。
- `scripts/verify-debezium-e2e.py` 的验收 2/3/4 全链路（本环境无 Docker daemon）。
- 生产多机部署、内部 topic 副本 3、REST 反向代理与认证。

## 飞书文档导入（2026-11-16）

**改动范围**：新增 `internal/feishu`（OAuth/API 客户端、service、文件名安全转换）、
`internal/config/feishu.go`、`internal/state/feishu.go`（`feishu_connections` /
`feishu_oauth_states` 与加密存储）、`internal/httpapi/feishu.go`（status/OAuth/files/
search/import 路由）、Console「从飞书导入」弹窗与 OAuth 回跳提示；导入复用
`SubmitDocumentBatch`，因此权限、SHA-256 去重、同名跳过和异步索引沿用普通上传。

**本仓库已执行**

- `go build ./cmd/rag-service ./internal/...` → exit 0。
- `go test ./internal/feishu ./internal/config -count=1` → ok（客户端请求形状、错误分类、
  OAuth state 单次消费、token 刷新与鉴权失败重试、文件夹/搜索/正文、文件名转换）。
- `go test ./internal/httpapi -run 'TestFeishu|TestOpenAPI' -count=1` → ok（逐篇失败隔离、
  重复选择跳过、写 scope 拦截、CSRF、回调 302 与 OpenAPI 文档）。
- `go test -p 1 -count=1 ./internal/state -run TestFeishu`（本地 `RAG_TEST_MYSQL_URL`）→ ok
  （token 明文不落库、刷新覆盖、OAuth state 绑定账号且单次使用、schema 顺序）。
- 前端 `npm run typecheck` → exit 0；`npm test -- src/FeishuImportModal.test.tsx src/feishu.test.ts`
  → 7 tests passed（目标知识库与可见范围提示、未授权/只读/未启用状态、浏览勾选与导入结果、
  搜索、索引进度文案）。

**未实机验证**

- 未使用真实飞书企业/账号联调：浏览文件夹的实际清单、搜索结果覆盖范围、应用可用范围、
  新增权限后重新授权、refresh token 轮换和飞书限流重试都只在 fake HTTP 服务上验证。
- 回调地址经反向代理/HTTPS 的实际可达性，以及飞书开放平台「重定向 URL」与
  `RAG_FEISHU_REDIRECT_URI` 完全一致的部署核对。
- 真实 docx 正文的排版损失（只导入纯文本，不含图片/表格结构）与 10 MiB 正文上限的实际体验。
- 多副本 `rag-service` 共享同一账号时的 refresh token 竞争：当前锁为进程内，部署需粘滞或单副本；未做多实例压测。

## 文档分布式锁：Redis 长租约 + MySQL fencing（2026-11-17）

工作树 `dsh-66abacb8-5df1`（Windows 11，Go 1.27.3，本地 MySQL 3307 / Redis 6379）。

**改动范围**：新增 `internal/state/document_lock.go`（开关/配置/错误/Locker 契约）、
`internal/state/upload_locks.go`（Redis 模式获取、代次登记、接管、续租）、
`internal/redisstate/locks.go`（单 namespace 单 Hash + Lua 原子多资源脚本）；
`internal/state/uploads.go` 把 `Begin`/`AcquireLock`/`ClaimRecovery`/`WithLock`/
`owned`/`releaseLeases`/`RenewLock`/`ReleaseLock` 拆成 mysql/redis 两条路径；
`document_leases` 追加 `fencing_epoch`（启动时幂等 `ALTER TABLE`）；`read_views` 的
`leases` 展示改用锁后端实时 TTL；`purge` 在 redis 模式先取独占租约并在事务内 fencing；
`run_step` 新增可取消 context（续租失败即取消在途解析/嵌入/Qdrant 调用）；
`rag-service`/`rag-document-worker`/`rag-mcp-ingest` 接线同一套配置。

**本仓库已执行**

```powershell
$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'
$env:RAG_TEST_REDIS_URL = 'redis://127.0.0.1:6379/15'

go vet ./...                                                            # exit 0
go test ./internal/state/ ./internal/runtime/ ./internal/documentworker/ ./internal/redisstate/ ./internal/config/ `
        ./internal/cacheconsumer/ ./internal/httpapi/ ./internal/cdcacceptance/ ./internal/e2e/ `
        -count=1 -p 1 -timeout 25m                                      # 9 包全 ok
                                                                        # state 29.1s / runtime 28.5s / documentworker 3.0s / redisstate 0.6s /
                                                                        # config 0.4s / cacheconsumer 0.4s / httpapi 1.9s / cdcacceptance 1.4s / e2e 2.8s
go test ./internal/redisstate/ -run 'TestDocumentLock' -count=2 -p 1 -timeout 10m  # ok（子代理执行）
```

新增的定向用例（`internal/state/upload_redis_test.go`，MySQL 真实库 + 受控内存锁后端，
不靠 sleep 构造时序）：

- `TestDocumentLockRedisConflictMatrix`：同一文档、同名上传、KB 独占双向互斥与冲突响应；
  不同文档并行；多资源获取失败不残留部分锁。
- `TestDocumentLockRedisPausedRegistrationRejected`：A 取锁后停在 MySQL 登记前，租约消失，
  B 登记并保持运行，A 恢复后迟到登记被拒（未产生第二个未完成操作，A 重新走了获取流程）。
- `TestDocumentLockRedisStaleEpochRegistration`：代次 CAS 直接拒绝过期登记。
- `TestDocumentLockRedisTakeoverFencesOldWorker`：A 失去租约、B 接管后，A 的续租/释放/
  `AssertOwner`/`RunStep`/`Commit`/`Rollback` 全部失败，且不会执行清理回调；B 只补偿自己的版本。
- `TestDocumentLockRedisLostLeaseStopsNextStep`：Redis 重启丢锁后步骤在边界停止并落 FAILED，
  恢复程序可接管至终态。
- `TestDocumentLockRedisHeartbeatCancelsInFlightWork`：续租失败取消在途 action context。
- `TestDocumentLockRedisUnavailableIsRetryable`：Redis 不可用返回可重试错误，不落库、不回退。
- `TestDocumentLockRedisReleaseFailureAfterCommit`：提交后释放失败不重复发布；幂等重放仍可用。
- `TestDocumentLockRedisLeaseDisplayUsesLiveTTL`：管理端租约展示按实时 TTL 判定存活/过期，
  `holder_id` 为短哈希且不暴露 owner token。

`internal/redisstate/document_lock_state_test.go` 用两个独立 `redisstate.DocumentLock`（各自独立 go-redis 客户端）驱动**真实 MySQL + 真实 Redis** 的两个 `UploadJournal`，覆盖同一文档/同名冲突、不同文档并行、namespace 独占双向互斥与释放后再独占。`internal/redisstate/locks_test.go` 用**两个独立 go-redis 客户端**对真实 Redis 验证：
原子多资源获取与冲突不残留、namespace 独占双向冲突、异主续租/释放无效、TTL 过期后
续租返回 `LeaseLost`、`OwnerDigest` 投影、释放幂等；每组测试先 `FLUSHDB`（仅 /15 测试库）。

**未实机验证**

- Redis Cluster / Sentinel / `rediss://` TLS / 认证失败：单 key + hash tag 设计上同 slot，
  但只在 standalone 7.4 上实测。
- `-race` 与多进程高并发抢占压测；正确性依赖 Redis 单线程执行 Lua 与 MySQL 行锁。
- Compose `app` profile 与多主机部署下的切换演练（`docs/DEPLOYMENT.md` §13 给了步骤）。
- 旧版 `rag-service-migrate` 遗留导入是离线操作，不走 journal 锁；不是本次覆盖范围。




## Word / HTML 独立 Docling 解析服务 — 2026-11-17

工作树 `dsh-9769f458-39e5`（Windows 11，Go 1.27.3，Python 3.11.7，Docling 2.150.1，
本地 MySQL 3307）。新增独立 `python-workers/docling-service`（FastAPI，:8092，
`POST /v1/parse` 返回带 `manifest.json`/`document.json`/图片的 ZIP），Go 侧在
`ingest.Prepare` 中按格式分发：`.docx`/`.html`/`.htm` 走 Docling，`.doc` 先经
LibreOffice 转 `.docx` 再交给同一服务；`RAG_DOCLING_SERVICE_URL` 为空时保持旧行为
（`.docx`/`.doc` 进程内解析，HTML 在受理层拒绝）。

### 已验证（可复核）

```powershell
# 1) 真实 Docling 服务（本机 venv，固定 docling-slim==2.150.1）
cd python-workers/docling-service
python -m pytest                                                    # 12 passed
# 覆盖：DOCX/HTML 清单、表格 row/col/row_span/col_span、<caption>/<figcaption>、
# DOCX 内嵌图片、HTML data URI 图片、外链图片不抓取、ZIP 布局、400/415/500/504。

# 2) 真实 Docling + 现有 Go 入库入口（RAG_DOCLING_SERVICE_URL 指向真实服务）
$env:RAG_DOCLING_SERVICE_URL = 'http://127.0.0.1:8092'
cd <repo>
go test -count=1 -run TestDoclingIntegration -v ./internal/ingest/
# TestDoclingIntegrationDOCX/HTML/LegacyDOC/ServiceUnavailable 全 PASS。
# 断言：表格行块包含表头与对应行值、题注进入资产块、图片资产有真实字节、
# .doc 来源名仍为 .doc、失败尝试不写 staging、HTML chunk 只带 html_parser_version。

# 3) 修改范围的 Go 回归（MySQL 串行；RAG_TEST_MYSQL_URL 指向 praxis_gotest）
$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_gotest?charset=utf8mb4'
go test -p 1 -count=1 -timeout 20m ./internal/document/ ./internal/ingest/ ./internal/chunking/ `
        ./internal/config/ ./internal/state/ ./internal/httpapi/ ./internal/searchrefs/ ./internal/runtime/
# 全部 ok（state 37.2s / runtime 33.9s，其余 <2s）。

# 4) Console（worktree 复用主仓 node_modules junction）
cd frontend
npx vitest run --configLoader native src/documentReplace.test.ts   # 11 passed
npm run typecheck                                                  # exit 0

# 5) Compose 渲染
docker compose --env-file .env.example config --quiet              # exit 0
```

### 未实机验证

- `python-workers/docling-service` 镜像构建与容器运行：本机 Docker engine 不可用
  （npipe 权限拒绝），只验证了本机 venv 启动与 `docker compose config`。
- 完整「上传 → Kafka/debezium → rag-document-worker → Qdrant → 检索」链路：未在隔离
  MySQL/Kafka/Qdrant 栈上运行；`ingest.Prepare` 真实解析、切分、资产与来源字段已验证，
  检索层回归测试通过。
- K8s 清单与 `scripts/start-stack.ps1` 尚未接入 Docling（仅 Compose / `.env.example` /
  `deploy/config.container.toml`）。
- HTML 预览的浏览器级同源安全验证（代码保持纯文本渲染、无 `dangerouslySetInnerHTML`）。
