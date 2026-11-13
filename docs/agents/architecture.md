# 架构（Agent 视角）

> 本文件是根 `AGENTS.md` 的拆分细节：架构、进程、目录与包职责。根文件只保留常驻系统提示词的精简索引。
> 校对基准：仓库代码 + `docs/`（2026-11-11）。

## 1. 运行时架构

```text
浏览器 ──/console──────────────┐
MCP 客户端 ──/mcp──────────────┤
REST 客户端 ──/v1/*────────────┴──> rag-service
                                       ├─> Qdrant（dense + sparse 向量）
                                       ├─> MySQL（文档/KB/任务/API Key/模型配置/权限/发件箱）
                                       ├─> Redis（读模型缓存、搜索结果快照；可选）
                                       ├─> Kafka（Debezium 文档通知 / 搜索队列；文档处理必需）
                                       └─> pdf-parser :8091（HTTP；本地 PyMuPDF 读取/渲染/裁图，
                                              版面/OCR/表格/公式/图片描述交远端视觉模型）

网页/REST 上传（默认异步）与 MCP add_document
  → MySQL 单事务写 document_jobs + document_job_outbox，返回 artifact_id
  → 有 Kafka：Debezium → Kafka → rag-document-worker
  → 无 Kafka：没有消费者，文档任务停留在 queued（rag-service 只受理）
  → rag-document-worker 完成 解析/切分/Embedding/Qdrant 发布
  → 客户端用 GET /v1/document-jobs/{artifact_id} 轮询 status/stage

Console 批次上传（POST /v1/document-batches）
  → 先提交 document_batches + document_batch_items（每项 saving）
  → 每文件一个事务并行受理（按文件名 lease 行加锁；不同名不排队，默认 6 路）
  → 结果写回 items 与计数，202 返回 batch_id；GET /v1/document-batches/{batch_id}
    同时给出保存结果与已入队任务的 status/stage

MCP search（配置 Kafka 时）→ search_task_outbox → Kafka → rag-search-worker
  → 检索结果写入 Redis 快照 → get_search_result 分页读取
```

关键点：

- **MySQL 是事实来源**：文档任务、发件箱、知识库配置、API Key、权限、模型配置、排队搜索任务都在 MySQL；Kafka 只负责通知，不是状态存储。
- **上传默认异步受理**：HTTP 提交点是“任务 + 发件箱”事务提交，而不是解析完成；`add_document` 返回 `artifact_id` 不代表已可检索，只有 `succeeded` 才算发布完成。
- **检索默认同步**（REST/Console），只有配置了 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时 MCP `search` 才切换为排队模式。
- **文档解析必须走 Kafka**：`rag-service` 不内嵌文档消费者；未配置 Kafka + Debezium + `rag-document-worker` 时，上传仍会受理，但文档任务停留在 `queued`。
- **PDF 双引擎**：`RAG_PDF_ENGINE=vision`（compose `app` 默认；为空时按是否配置 `RAG_PDF_SERVICE_URL` 决定）走 pdf-parser，由远端 OpenAI 兼容视觉模型完成版面/OCR/表格/公式/图片描述；旧值 `docling` 作为兼容别名等价于 `vision`；`go` 使用 Go 进程内纯文本引擎。pdf-service 本地不加载任何模型权重。

## 2. 进程与可执行文件

| 进程 | 作用 | 主要入口 |
| --- | --- | --- |
| `rag-service` | REST + MCP + Console + 后台维护循环；只受理上传，不解析文档 | `cmd/rag-service/main.go` |
| `rag-document-worker` | 消费文档通知，执行解析/切分/Embedding/Qdrant 发布 | `cmd/rag-document-worker/main.go` |
| `rag-search-worker` | 消费排队搜索请求，执行检索并写 Redis 快照 | `cmd/rag-search-worker/main.go` |
| `rag-dead-letter-worker` | 消费显式 DLQ topic（`--kind document|search`），把可确认的失败写入终态并 ACK | `cmd/rag-dead-letter-worker/main.go` |
| `rag-state-migrate` | 状态库表结构检查/创建 | `cmd/rag-state-migrate/main.go` |
| `rag-service-migrate` | 旧知识源（`~/.mini_agent`）预览/导入（**不重建向量**） | `cmd/rag-service-migrate/main.go` |
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
deploy/                     compose 用容器配置、Debezium 配置、MySQL 初始化脚本
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
| `monitor` | 只读监控快照：状态库聚合（文档任务/检索任务/上传分类、outbox 记录统计、有效文档计数、活动计数器）与 MySQL/Redis/Qdrant/Kafka/pdf-service 依赖探测；另单独汇总 Debezium `/q/health` 运行检查、Kafka 文档消费组进度和文档 Worker 心跳（应用读不到的采集位点/滞后显式为「未知」），并按三态聚合页头总体状态，供 Console「系统监控」页与 `GET /v1/admin/monitor/overview` 使用。outbox 的 PENDING 只是保留期记录，不作为未投递积压 |
| `mq` | Kafka 生产/消费、进程内重试、DLQ 发布 |
| `qdrant` | Qdrant HTTP 客户端（collection、向量、过滤、RRF 查询） |
| `redisstate` | Redis 读模型缓存与搜索任务/快照 |
| `resources` | MCP 文档资源 URI 与分页 |
| `retrieval` | BM25 / Dense / Hybrid 检索、Cross-Encoder 重排、Orchestrator |
| `runtime` | 业务运行时：知识库、文档 CRUD、搜索、上传事务、任务状态、维护恢复 |
| `searchrefs` | 搜索结果投影、artifact 引用与快照写入 |
| `searchworker` | 排队搜索处理器 |
| `sparse` | BM25 sparse 编码（确定性哈希词表） |
| `state` | MySQL schema、仓储、`enc:v1:` 加密、上传日志、任务/发件箱、结果快照 |
| `statetest` | 测试用共享 schema 与 advisory lock |
| `version` | 版本常量 |
| `wire` | 依赖装配（adapters） |
| `e2e` | 真实 HTTP 全栈测试（真实 MySQL + fake 模型/Qdrant/Redis/broker） |
