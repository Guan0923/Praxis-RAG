# Praxis-RAG 接入 Kubernetes 实施任务书

执行对象为可读取本仓库的 DeepSeek V4.1 Flash。请直接提交到工作区的文件改动、相应验证结果和可复制的部署命令。先读根目录 `AGENTS.md`，保留用户已有改动。本文约定第一版的交付范围，实际参数以当前 checkout 的代码为准。

## 1. 第一版要交付什么

在已有 Kubernetes 集群上，以 Kustomize 部署 Praxis-RAG 应用层。使用方提供集群可达的 MySQL 8、Qdrant HTTP、Kafka、装有匹配版本 Debezium MySQL 插件的分布式 Kafka Connect，以及启用展示缓存时的 Redis。保留本地 Compose 开发入口。首版使用固定副本数和集群内访问，方便单独扩文档处理能力，再决定是否引入自动扩缩容。

完成条件是操作者能从空的应用命名空间启动组件，上传一份文档并同步检索到带出处的证据，也能把文档 worker 从 1 个扩为 2 个。环境缺少真实集群或依赖时，交付可执行的验收步骤，并明确哪些结果仅通过静态验证。

## 2. 实现前核对的代码

| 位置 | 核对内容 |
| --- | --- |
| `Dockerfile`、`deploy/config.container.toml`、`docker-compose.yml` | Go 镜像已有全部 CLI、Console 和 connector 模板。核对运行用户、启动参数、真实环境变量，以及容器地址和宿主机地址的差异。 |
| `cmd/rag-state-migrate/main.go`、`cmd/rag-connect/main.go`、`deploy/kafka-connect/README.md` | schema 检查与初始化、注册两个 connector 所需权限、插件版本、topic 名和 server ID。 |
| `cmd/rag-document-worker/main.go`、`internal/runtime/document_job.go`、`internal/state/upload_coordinator.go` | 实例身份、消费者并发、租约续期、重投、提交与临时目录清理。 |
| `internal/mcp/endpoint.go`、`cmd/rag-service/main.go`、`internal/httpapi/server.go` | MCP 有状态会话、服务进程内的 Connect 监督任务、健康接口的真实返回值。 |
| `python-workers/pdf-service/Dockerfile`、`python-workers/pdf-service/README.md` | PDF 服务独立镜像、非 root 用户、临时目录、并发与远端 VLM 配置。 |

## 3. 文件与工作负载

新增 `deploy/k8s/`，公共工作负载放在 `base/`，提供可直接渲染的 `overlays/go/` 和 `overlays/vision/`。两者都是完整应用安装入口。独立初始化资源放在 `bootstrap/`，缓存组件可通过独立组件或 overlay 启用。在 `docs/USAGE.md` 增加 Kubernetes 部署入口，详细操作集中到 `deploy/k8s/README.md`。镜像仓库、版本、外部地址及命名空间允许按环境替换。

| 组件 | Kubernetes 对象及首版默认行为 |
| --- | --- |
| API、MCP、Console | `rag-service` Deployment，1 副本，ClusterIP 8011；镜像已有 `/app/web`。单实例首版采用避免新旧实例重叠的更新策略，文档说明更新期间可能短暂中断。 |
| 文档入库 | `rag-document-worker` Deployment，起始 1 副本，复用 Go 镜像，显式指定 `command: ["/app/bin/rag-document-worker"]`。每个副本使用同一文档 consumer group，保留不同实例身份。 |
| 死信、缓存 | 文档死信独立运行 `rag-dead-letter-worker --kind document`。启用缓存时，增加 `rag-cache-worker` 和 `rag-dead-letter-worker --kind cache`，按实际代码配置 Redis 与 topic/group。三个消费者均无需 Service。 |
| PDF 高保真 | vision 入口部署 `pdf-parser` 和 ClusterIP 8091，应用与文档 worker 指向 `http://pdf-parser:8091`。go 入口设置 `RAG_PDF_ENGINE=go`，可独立启动。vision 使用知识库配置的远端 VLM，并保持现有超时和清单契约。 |
| 初始化 | schema 与 connector 注册使用分别执行的一次性 Job。干净安装时先完成 schema，再注册并等待 CDC 就绪，随后启动应用。已有基础设施可单独检查并复用。 |

### 配置、网络和存储

- 用 ConfigMap 配置普通参数。用已有 Secret 的键引用注入服务 token、数据库凭据、状态加密密钥及启用功能所需的凭据。为用户列出必填键、适用组件和示例创建步骤。CDC 账号凭据只给注册 Job；运行中的 API 和文档 worker 复用已有的状态加密密钥。使用独立迁移账号时，迁移凭据只给迁移 Job。
- 应用绑定 `0.0.0.0:8011`，明确设置 `RAG_SERVICE_TOKEN`、`RAG_MCP_ALLOWED_HOSTS`、`RAG_MCP_ALLOWED_ORIGINS`。初次验证用 `kubectl port-forward` 和与之匹配的精确白名单。PDF 与 Connect REST 仅供内部受信调用。记录 Kafka 的 advertised listeners、Connect、模型端点都需要从实际调用方可达。
- 区分 `RAG_SERVICE_DATA_ROOT=/app/data` 和 Go 的 `os.TempDir()`。梳理前者的运行时写入，为需跨重建保留的数据提供 PVC 及挂载说明。为各 Pod 的解析暂存目录提供可写临时空间，按镜像实际 UID/GID 配置权限。文档写清卷的访问模式与扩容限制：单节点读写卷不能作为跨节点多副本共享方案。
- 恢复代码按 `workspace_node` 检查临时目录归属。即使挂载共享存储，新 Pod 的 hostname 也可能不同。验证时分别观察 MySQL 任务终态、Qdrant 发布结果以及旧 workspace 登记的清理情况。若只剩历史清理项，记录其实际影响；如果发现数据丢失或重复可见，先修复并用针对性回归验证，再宣称支持该故障场景。
- 设置可覆盖的 CPU、内存 requests/limits 与终止宽限时间。按 PDF 单份大小、解析超时和 worker 退出行为说明调参依据。先给出保守的 PDF 进程数和并发数，避免多个 Pod 一起耗尽远端 API 配额。
- API 使用已有 `/health`，PDF 使用 `/healthz`。当前 Go 的 `handleHealth` 固定写 HTTP 200，HTTP 探针只能说明服务能响应，依赖就绪通过独立验收检查。worker 没有 HTTP 端口，用进程退出重启、消费者状态及现有心跳观测。保留集群中实际可运行的非 root 配置，尤其要核对 Go 镜像使用名字 `rag` 的情况。

## 4. 安装、扩容与回退顺序

1. **准备环境。** 给出构建并向集群提供两个镜像的命令，替换镜像版本、外部地址，创建命名空间和 Secret。开发者使用本地集群时，写明如何导入本地镜像。核对 MySQL binlog、CDC 用户权限、Kafka topic 与实际分区数。初次演练使用隔离的测试库、Kafka topic/group、Connect 注册及 Qdrant collection。
2. **初始化状态。** 复用 `/app/bin/rag-state-migrate` 检查，首次安装按需运行 `/app/bin/rag-state-migrate --apply`。注册 Job 复用镜像中 `/app/connect/connectors`，顺序运行 `rag-connect register --wait 180` 与 `rag-connect wait --wait 180`，传入现有配置所需的 Connect 地址、数据库/CDC 凭据、Kafka 地址、connector 名与历史 topic。已有生产 connector 时先检查状态和配置，避免同一 binlog 来源被重复采集。文档给出等待 Job 完成、查看失败原因、按版本重跑的确切命令；不要依赖 YAML 书写顺序，也不要让每个应用副本执行迁移。
3. **启动应用。** 根据需要 apply go 或 vision 入口，等待 Deployment 就绪；通过 port-forward 访问 Console、REST 与 MCP。使用现有接口及权限配置完成首次上传。给出按组件查看日志和 worker 状态的命令。
4. **验证独立扩容。** 在测试环境把文档 worker 从 1 扩到 2，确认加入同一 group 并分到分区。用同一批文档比较排队时间、单位时间完成数和下游限流。检查实际分区数与 `RAG_DOCUMENT_WORKER_CONCURRENCY`；仓库启动脚本默认建 4 个分区。首版保留 API 单副本，另列多副本的前置验证，包括 MCP 会话路由、断线重连与 Connect 监督任务的并发行为。
5. **升级与回退。** 给出更新镜像和恢复上一版 overlay 的命令，注明 Job、ConfigMap、Secret 和镜像的版本关系。回退应用前检查旧版与当前 schema 的兼容性，保留 MySQL、Qdrant、Kafka offsets 和 PVC。若从现有 Compose 实例切流，先结束或排空旧 worker，再按计划接入同一消费组；记录切回步骤。

## 5. 最小验收与交付报告

| 验收项 | 通过标准 |
| --- | --- |
| 清单与配置 | 使用 `kubectl kustomize` 成功渲染 go、vision 和提供的缓存组合，确认命令、镜像、Service selector、Secret 引用及 DNS 一致。有可用测试集群时，再用 server-side dry-run 验证资源。 |
| 上传到检索 | 在隔离环境上传含唯一短语的 TXT，通过既有任务状态接口或 Console 观察到 `succeeded`，再经同步搜索得到该文档的片段和出处。记录一次 MCP 调用结果。 |
| vision 模式 | 上传一页 PDF，确认经 PDF 服务和已配置 VLM 完成解析，并能取得页码与关联证据。若使用 fake converter，只标记为协议冒烟，记录真实 VLM 验收待执行。 |
| 重建与重投 | 在隔离环境处理一份耗时文档时删除正在处理它的 worker Pod，观察重新调度、租约或重投后的任务结果，检查可检索版本唯一性及恢复登记。区别正常终止和强制失联，报告实际覆盖的场景。 |
| 变更范围 | 只运行部署清单及实际改动包的针对性验证。若修改 Go，用仓库规定的串行参数运行对应包；涉及 MySQL 的测试使用库名含 test 的一次性数据库。 |

最终回复列出改动文件、复制即可运行的首次部署/扩容/回退命令、每项实际验证结果，以及因缺集群或外部服务而待执行的步骤。本轮交付应用层 Kubernetes 支持；基础设施 Operator、Helm、HPA/KEDA 和公网入口留给后续有明确需求的版本。
