# AGENTS.md

不需要执行全量测试，只需要执行修改范围相对应的测试代码即可。同时对于处理用户的任务，不需要进行反复的校验，只要有测试能证明已完成就可以终止任务了。

## 1. 这是什么

Praxis-RAG：面向 AI 助手与业务应用的**独立知识库与检索服务**。文档切分、向量化并索引后，通过 REST / MCP / Web Console 返回带出处（文档、片段、页码、图片、表格）的**证据**，不生成最终回答。

- 后端：**Go 1.27**（`cmd/` 入口 + `internal/` 实现）；`rag-service` + 3 个 worker（文档/缓存/死信）+ 若干 CLI。
- 前端：`frontend/`（React 19 + TypeScript + Ant Design 6 + Vite），Go 服务在 `/console` 托管。
- 状态：**MySQL 8**（唯一状态库）；向量：**Qdrant HTTP**；缓存：Redis（可选：只读模型展示缓存 + 可选的文档长租约后端，默认长租约仍在 MySQL）；消息：Kafka（文档入库与缓存刷新必需；搜索为同步执行，不经过 Kafka）。搜索一次调用同步返回证据，无任务 ID/轮询/结果快照。
- PDF 高保真：可选独立微服务 `python-workers/pdf-service`（FastAPI + PyMuPDF，HTTP :8091）；本地只做 PDF 读取/渲染/裁图与清单组装，版面/OCR/表格/公式/图片描述统一调用知识库配置的远端 OpenAI 兼容视觉模型（VLM），进程内不加载任何模型权重。
- 知识图谱（可选）：`python-workers/lightrag-service`（FastAPI，HTTP :8093）负责实体关系索引与图谱上下文检索，只返回证据不生成答案，pin 上游 `lightrag-hku` 版本；仅在配置 `RAG_LIGHTRAG_SERVICE_URL` 后由 `retrieval=graph` 调用。除此之外无 Python 依赖。

## 2. 硬性约束（任何改动都适用）

1. **契约稳定**：MCP 工具名与 schema、REST 路径与响应、环境变量名、`enc:v1:` 凭据格式、MySQL 表结构、PDF 清单版本（`go-native-v1` / `docling-service-v1`；后者只是证据格式契约，与是否使用 Docling 无关）。改契约必须同时做迁移并更新文档。
2. **不要重新引入**：Python 主后端、SQLite、RocketMQ、Qdrant 本地文件模式（`QDRANT_PATH`）。当前是 Kafka + MySQL。
3. **Go 不依赖 Python**：`go build ./...` 与 `go test ./...` 不涉及 Python；PDF 版面/OCR/表格/公式/图片描述能力由 pdf-service 调用远端视觉模型（或纯 Go 实现）提供，本地不加载模型，也没有 `RAG_PDF_MODELS_PATH` / 模型卷。`python-workers/` 下只有受控微服务（pdf-service、docling-service、lightrag-service），通过 HTTP 契约与 Go 交互，Go 侧用 fake server 测试契约。
4. **MySQL 是事实来源**：Kafka 只传通知，由两个 Debezium 连接器从 MySQL 表级采集投递（任务连接器 `document_jobs`，缓存连接器 8 张读模型表），应用不写事务 outbox（旧 outbox 已于 2026-11-13 下线）。通知只携带定位标识与 `schedule_version`，参数回 MySQL 读取；上传默认**异步受理**（`document_jobs` 单事务），`artifact_id` 不代表已索引，只有状态 `succeeded` 才可检索。MCP/REST 搜索为**同步执行**，一次调用直接返回证据；`search_tasks`/`search_result_snapshots` 为历史表、保留数据但不再读写，artifact（`artifact_snapshots`）按 ID 独立读取。
5. **测试必须串行**：`RAG_TEST_MYSQL_URL` 指向库名含 `test` 的一次性 MySQL，并用 `go test -p 1 -count=1 -timeout 25m ./...`；并行会抢同一 schema 的 advisory lock。
6. **不提交生成物**：`dist/`、`build/`、`frontend/node_modules/`、`.test-tmp/`、`.env*`（除 `.env.example`）、模型/向量数据、根目录 worker 二进制。
7. **安全默认**：默认只监听 loopback；非 loopback 必须 `RAG_SERVICE_TOKEN` + MCP Host/Origin 白名单；`/v1/admin/*` 仅本机直连或管理员会话；日志不记录查询、正文与凭据。
8. **中文优先**：Console 文案、`README.md`、`docs/` 均为中文；功能/配置变化至少同步 `README.md` 或 `docs/USAGE.md`。

## 3. 常用命令

```powershell
pwsh -File scripts/build-go.ps1            # Console + Go 二进制 + 本机冒烟
make build | make test | make check | make smoke

$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:change-me@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'
go vet ./...; go test -p 1 -count=1 -timeout 25m ./...

cd frontend; npm ci; npm run typecheck; npm test; npm run build
```

产物在 `dist/bin/` 与 `dist/web/`；服务入口 `dist/bin/rag-service`（`/console`、`/mcp`、`/v1/*`、`/docs`、`/health`）。

## 4. 阅读地图

| 需要什么 | 读这里 |
| --- | --- |
| 架构、进程、目录结构、`internal` 包职责 | [`docs/agents/architecture.md`](docs/agents/architecture.md) |
| 功能清单：导入/解析/切分/检索/MCP/REST/Console/权限/队列/CLI | [`docs/agents/features.md`](docs/agents/features.md) |
| 配置项、构建测试、改动约束细项、限制与未验证项 | [`docs/agents/development.md`](docs/agents/development.md) |
| 用户视角、快速开始、部署 | [`README.md`](README.md)、[`docs/USAGE.md`](docs/USAGE.md)、[`docs/DEPLOYMENT.md`](docs/DEPLOYMENT.md) |
| 权限与共享 | [`docs/PERMISSIONS.md`](docs/PERMISSIONS.md) |
| 迁移历史与契约差异 | [`docs/GO_MIGRATION.md`](docs/GO_MIGRATION.md)、[`docs/GO_PARITY_AUDIT.md`](docs/GO_PARITY_AUDIT.md) |
| 状态库 / 缓存 / 产物引用 | [`docs/state-storage.md`](docs/state-storage.md)、[`docs/state-cache.md`](docs/state-cache.md)、[`docs/result-references.md`](docs/result-references.md) |
| 已做过的验证 | [`VALIDATION.md`](VALIDATION.md) |

## 5. 代码速查

- 服务入口与路由：`cmd/rag-service/main.go`、`internal/httpapi/server.go`
- MCP：`internal/mcp/`（工具 `tools.go`、资源 `resources.go`、提示词 `prompts.go`）
- 状态库与 schema：`internal/state/`；入库管线：`internal/ingest/`；解析：`internal/document/`
- 检索：`internal/retrieval/` + `internal/qdrant/` + `internal/sparse/`
- 配置：`internal/config/`；Console：`frontend/src/`
