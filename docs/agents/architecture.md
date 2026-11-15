# 架构（Agent 视角）

> 本文件是根 `AGENTS.md` 的拆分细节：架构、进程、目录与包职责。根文件只保留常驻系统提示词的精简索引。
> 校对基准：仓库代码 + `docs/`（2026-11-11）。

## 1. 运行时架构

```text
浏览器 ──/console──────────────┐
MCP 客户端 ──/mcp──────────────┤
REST 客户端 ──/v1/*────────────┴──> rag-service
                                       ├─> Qdrant（dense + sparse 向量）
                                       ├─> MySQL（文档/KB/任务/artifact/API Key/模型配置/权限/缓存水位）
                                       ├─> Redis（读模型展示缓存；可选）
                                       ├─> Kafka（Kafka Connect 的 Debezium 文档通知 / 缓存变更；文档入库必需）
                                       └─> pdf-parser :8091（HTTP；本地 PyMuPDF 读取/渲染/裁图，
                                              版面/OCR/表格/公式/图片描述交远端视觉模型）

网页/REST 上传（默认异步）与 MCP add_document
  → MySQL 单事务写 document_jobs，返回 artifact_id
  → 有 Kafka：Kafka Connect 文档连接器（praxis-document-cdc，表级采集）→ Kafka → rag-document-worker
  → 无 Kafka：没有消费者，文档任务停留在 queued（rag-service 只受理）
  → rag-document-worker 完成 解析/切分/Embedding/Qdrant 发布
  → 客户端用 GET /v1/document-jobs/{artifact_id} 轮询 status/stage

Console 批次上传（POST /v1/document-batches）
  → 先提交 document_batches + document_batch_items（每项 saving）
  → 每文件一个事务并行受理（按文件名 lease 行加锁；不同名不排队，默认 6 路）
  → 结果写回 items 与计数，202 返回 batch_id；GET /v1/document-batches/{batch_id}
    同时给出保存结果与已入队任务的 status/stage

MCP search / REST /v1/search（同步）→ runtime.Service.Search → SearchEngine
  → 检索 + 重排 + artifact 投影（不落结果快照）
  → 同一次响应返回 results / artifacts / evidence_status / answer_guidance
  → 无任务 ID、无轮询、无搜索 topic；总预算 RAG_SEARCH_TIMEOUT_SECONDS

业务表变更（含手工 SQL）→ Kafka Connect 缓存连接器（praxis-cache-cdc）→ praxis_state_cache_refresh
  → rag-cache-worker 递增 cache_revisions.revision 并重建 Redis（失败由 RefreshDirty 补偿）
```

关键点：

- **MySQL 是事实来源**：文档任务、知识库配置、API Key、权限、模型配置、artifact 快照都在 MySQL；Kafka 只传通知（两类事件，只含定位标识），不是状态存储。`search_tasks`/`search_result_snapshots` 为历史表、不再读写。
- **上传默认异步受理**：HTTP 提交点是 `document_jobs` 任务事务提交（不再有发件箱），而不是解析完成；`add_document` 返回 `artifact_id` 不代表已可检索，只有 `succeeded` 才算发布完成。
- **搜索永远同步**：MCP `search` 与 REST `POST /v1/search` 一次调用完成检索 + 重排并直接返回证据，没有任务 ID、`wait_seconds` 或轮询；artifact 按 ID 独立读取（保留期 `RAG_RESULT_TTL_SECONDS`）。
- **文档解析必须走 Kafka**：`rag-service` 不内嵌文档消费者；未配置 Kafka + Kafka Connect 的两个 Debezium 连接器 + `rag-document-worker` 时，上传仍会受理，但文档任务停留在 `queued`。
- **PDF 双引擎**：`RAG_PDF_ENGINE=vision`（compose `app` 默认；为空时按是否配置 `RAG_PDF_SERVICE_URL` 决定）走 pdf-parser，由远端 OpenAI 兼容视觉模型完成版面/OCR/表格/公式/图片描述；旧值 `docling` 作为兼容别名等价于 `vision`；`go` 使用 Go 进程内纯文本引擎。pdf-service 本地不加载任何模型权重。

## 2. 进程与可执行文件

| 进程 | 作用 | 主要入口 |
| --- | --- | --- |
| `rag-service` | REST + MCP + Console + 后台维护循环；只受理上传，不解析文档 | `cmd/rag-service/main.go` |
| `rag-document-worker` | 消费文档通知，执行解析/切分/Embedding/Qdrant 发布 | `cmd/rag-document-worker/main.go` |
| `rag-cache-worker` | 消费 Debezium 缓存连接器投递的业务表变更，按 revision 水位重建 Redis；周期性 `RefreshDirty` 补偿 | `cmd/rag-cache-worker/main.go` |
| `rag-dead-letter-worker` | 消费显式 DLQ topic（`--kind document|cache`），把可确认的失败写入终态并 ACK | `cmd/rag-dead-letter-worker/main.go` |
| `rag-state-migrate` | 状态库表结构检查/创建 | `cmd/rag-state-migrate/main.go` |
| `rag-service-migrate` | 旧知识源（`~/.mini_agent`）预览/导入（**不重建向量**）；`--retire-outbox` 删除已下线的 outbox 表 | `cmd/rag-service-migrate/main.go` |
| `rag-cdc-cutover` | CDC 存量切换：盘点、记录 binlog 位点、补投、缓存基线、核对 | `cmd/rag-cdc-cutover/main.go` |
| `rag-connect` | Kafka Connect 注册/等待/状态/插件版本校验/FAILED task 监督/真实消息解码 | `cmd/rag-connect/main.go` |
| `rag-mcp-ingest` | 本地文件/目录批量导入（走运行时上传管线） | `cmd/rag-mcp-ingest/main.go` |
| `rag-mcp-debug` | 无 Qdrant/模型的 mock 检索调试 | `cmd/rag-mcp-debug/main.go` |
| `pdf-parser`（容器） | PDF 视觉解析微服务：本地 PyMuPDF 读取/渲染/裁图，远端视觉模型完成版面/OCR/表格/公式/图片描述，`POST /v1/parse` 返回 zip（`manifest.json` + 图片资产） | `python-workers/pdf-service/app/main.py` |

退出码约定：`0` 正常停止 / `1` 运行时失败 / `2` 配置或用法错误。

## 3. 仓库结构

```text
cmd/                        各可执行入口（见上表）
internal/                   Go 业务实现（按包见下表）
frontend/                   Console（React + Vite，测试与源文件同目录）
python-workers/pdf-service/  PDF 视觉解析微服务（独立镜像，本地无模型，不在 Go 构建内）
deploy/                     compose 用容器配置、Kafka Connect 连接器模板、旧 Debezium 回滚配置、MySQL 初始化脚本
scripts/                    build-go.ps1/.sh、start-stack、stop-stack、verify-*.py
docs/                       使用、部署、权限、迁移、状态存储、缓存等文档
docs/agents/                本目录：AGENTS.md 的拆分细节（按需读取）
examples/knowledge/         示例知识文档（可直接上传试用）
.github/workflows/test.yml  CI：go vet/build/test + 前端 typecheck/test/build
```

## 4. internal 包职责

| 包 | 职责 |
| --- | --- |
| `access` | 知识库归属、读/写授权、邀请链接 |
| `apierrors` | 安全错误到 HTTP 状态码的映射 |
| `apikeys` | API Key 存储与校验（`prk_` 前缀，摘要入库） |
| `auth` | 账号、角色、密码、Console 会话与 CSRF |
| `catalog` | 命名空间级 MCP 工具开关与提示词模板持久化 |
| `chunking` | 五类切分策略、参数校验与切分实现 |
| `config` | 环境变量、`~/.praxis-rag/config.toml`、启动校验 |
| `contracts` | 检索/MCP 请求契约、JSON Schema、错误语义 |
| `document` | PDF 原生解析、PDF 视觉服务客户端、DOCX、DOC 转换、公式、VLM、图片/表格资产 |
| `documentworker` | 文档任务处理器（仅由 Kafka 消费者 `rag-document-worker` 调用） |
| `httpapi` | REST 路由、认证中间件、OpenAPI、Console 静态资源 |
| `ingest` | “解析 → 切分 → Embedding → Qdrant 发布”管线 |
| `kbconfig` | 知识库模型连接、模型配置（profiles；kind 为 embedding/reranker/vlm/jev，无 ocr） |
| `mcp` | MCP 端点、工具、资源、提示词、权限 scope |
| `migrate` | 旧知识源导入与状态迁移 |
| `modelapi` | Embedding / Reranker HTTP 客户端 |
| `monitor` | 只读监控快照：状态库聚合（文档任务/上传分类、缓存 dirty/failures、有效文档计数、活动计数器）与 MySQL/Redis/Qdrant/Kafka/pdf-service 依赖探测；另单独汇总两个 Kafka Connect 采集连接器（connector/task 状态、所在 worker、失败原因、自动重启与恢复结果）和两组 Kafka 消费组进度、文档 Worker 心跳（应用读不到的采集位点产生显式「未知」），并按三态聚合页头总体状态，供 Console「系统监控」页与 `GET /v1/admin/monitor/overview` 使用 |
| `connect` | 分布式 Kafka Connect 集成：固定名称幂等注册（PUT 前比较配置）、REST 客户端（worker failover、follower 409 重试）、FAILED task supervisor（只重启失败 task、指数退避、有限次数）；worker 宕机由 Connect rebalance 接管，位点在 Kafka offset topic |
| `mq` | Kafka 生产/消费、两类 Debezium 事件解码（文档/缓存）、进程内重试、DLQ 发布 |
| `qdrant` | Qdrant HTTP 客户端（collection、向量、过滤、RRF 查询） |
| `redisstate` | Redis 读模型展示缓存与水位 |
| `resources` | MCP 文档资源 URI 与分页 |
| `retrieval` | BM25 / Dense / Hybrid 检索、Cross-Encoder 重排、Orchestrator |
| `runtime` | 业务运行时：知识库、文档 CRUD、同步搜索（含总预算/取消）、上传事务、任务状态、维护恢复 |
| `searchrefs` | 搜索结果投影与 artifact 引用（不再写结果快照） |
| `sparse` | BM25 sparse 编码（确定性哈希词表） |
| `state` | MySQL schema、仓储、`enc:v1:` 加密、上传日志、任务（`schedule_version`）、缓存 revision 水位、artifact 快照、CDC 切换状态 |
| `statetest` | 测试用共享 schema 与 advisory lock |
| `version` | 版本常量 |
| `wire` | 依赖装配（adapters） |
| `e2e` | 真实 HTTP 全栈测试（真实 MySQL + fake 模型/Qdrant/Redis/broker） |
