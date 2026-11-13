# 后端 Go 重构总纲

## REST 上传接口统一（2026-11-12）

删除 `POST /v1/documents`，REST 上传统一迁移到 `POST /v1/document-batches`，包括只上传一个文件的请求。multipart 的 `files` 字段、认证和 `Idempotency-Key` 请求头不变；响应改为 `202`，通过 `batch_id` 和 `items`（每文件 `accepted/skipped/failed` 与受理成功后的 `artifact_id`）读取结果。轮询整个批次使用 `GET /v1/document-batches/{batch_id}`，单文件任务仍使用 `GET /v1/document-jobs/{artifact_id}`。

旧接口的请求内同步上传模式及专用 `RAG_UPLOAD_MODE` 配置一并删除。`GET /v1/documents` 列表、文档内容下载、删除及 MCP 工具保留。不增加旧 POST 路径的兼容处理；已有文档、任务和批次无需数据库迁移。下文审计记录中的旧上传接口描述仅反映当时实现。

最后更新：2026-11-10

## 1. 目标

把 Praxis-RAG 的后端从 Python（`src/rag_mcp`，约 69 个模块 / 11.5k 行）完整迁移到 Go，
最终删除 Python 后端代码，同时保持以下对外契约不变：

- REST API 路径、认证方式、状态码和响应结构；
- MCP Streamable HTTP 端点 `/mcp` 的工具名、JSON Schema、错误语义；
- 环境变量、`~/.praxis-rag/config.toml` 配置项、数据目录和共享状态数据库结构；
- Web Console 静态资源的访问路径；
- 与 Qdrant、MySQL/SQLite、Redis、RocketMQ 的现有交互格式与既有数据。

前端（`frontend/`，React + Vite）不重写；构建产物仍输出到 Console 静态目录。
DOCX 读取与 Word 图片描述由 Go 进程内完成（图片描述由 Go 直接调用远端 OpenAI 兼容 VLM）；PDF 默认由独立 Docling 解析微服务（`python-workers/pdf-service`，HTTP + zip 清单）处理，Go 端可用 `RAG_PDF_ENGINE=go` 切回内置纯文本引擎；Go 主服务与镜像不依赖 Python。仅 .doc 转换仍使用外部 LibreOffice。
Go 只负责按其现有协议调用并消费解析结果；语义检索和重排本来就是 HTTP 模型 API。

## 2. 目标架构

```text
cmd/
  rag-service/            主 HTTP 服务（REST + MCP + Console 静态资源）
  rag-document-worker/    异步文档入库 worker（RocketMQ / MySQL 租约）
  rag-search-worker/      排队搜索 worker（RocketMQ / Redis 快照）
  rag-dead-letter-worker/ DLQ 终结 worker
  rag-state-migrate/      共享状态迁移
internal/
  version/                版本常量
  config/                 环境变量、config.toml、Rag/Service/State 配置
  contracts/              检索请求/候选/错误等 provider-neutral 契约
  chunking/               切分策略、切分配置与切分器
  state/                  MySQL/SQLite schema、仓储、加密、上传日志、缓存同步
  qdrant/                 Qdrant HTTP REST 客户端（含本地路径模式适配）
  retrieval/              BM25 sparse、dense、hybrid、rerank、orchestrator
  document/               Markdown/TXT/DOCX 读取、PDF 解析进程编排、资产
  modelapi/               Embedding/Reranker HTTP 模型 API 客户端
  redis/                  Redis 状态缓存、搜索任务与结果快照
  mq/                     RocketMQ 客户端封装、outbox relay、DLQ
  mcp/                    MCP JSON-RPC Streamable HTTP 服务、工具/提示词目录
  runtime/                服务运行时（知识库、文档、搜索、租约恢复）
  httpapi/                REST 路由、认证、中间件、Console 静态资源
  webui/                  Console 资源定位（磁盘目录，后续可 go:embed）
assets/                   可选嵌入资源目录（构建时生成，不入库）
```

依赖选型（最小必要集，逐阶段引入）：

| 能力 | 选型 |
| --- | --- |
| HTTP | 标准库 `net/http` + 自研小路由（精确 400/404/405 语义） |
| MySQL | `github.com/go-sql-driver/mysql` |
| SQLite | `modernc.org/sqlite`（纯 Go，无 CGO） |
| Redis | `github.com/redis/go-redis/v9` |
| RocketMQ | 优先官方 `apache/rocketmq-clients/golang/v5`（gRPC）；若不兼容则以 gRPC 直连封装 |
| Qdrant | 直接用 Qdrant HTTP REST（`net/http`），不引入第三方客户端 |
| TOML | `github.com/BurntSushi/toml` |
| 加密 | 标准库 AES-GCM（兼容现有 `enc:v1:` 格式） |
| 密码学随机/UUID | 标准库 `crypto/rand` + 自研 v4 UUID（与现有格式一致） |

## 3. 兼容性硬约束

1. **数据库 schema 与 SQLAlchemy 建表结果一致**：表名、列名、主键、唯一约束、索引语义不变；
   已有 MySQL/SQLite 数据可直接读取，不要求数据迁移。
2. **`enc:v1:` 凭据格式**：AES-GCM，12 字节 nonce，`base64(nonce||ciphertext)`；
   `RAG_STATE_ENCRYPTION_KEY` 的 `base64:` 前缀和 32 字节要求不变。Windows 旧 `dpapi:`/`private-file:` 记录只读兼容（迁移期保留 Python 回退或实现 DPAPI 读）。
3. **MCP 工具契约**：`search`、`get_search_result`、`add_document`、`get_document_status`、
   `update_document`、`delete_document`、`get_document_content`、提示词模板等名称与 schema 不变。
4. **RAG 结果 JSON 字段**：`evidence_status`、`answer_guidance`、`results[].rank/document_id/chunk_id/title/source/text/score/metadata` 等不变。
5. **认证与租户边界**：`Authorization: Bearer <service token|API Key>` + `X-RAG-Namespace`；
   本机管理接口的 loopback/Forwarded/Origin 校验规则不变。
6. **环境变量**：`startup_config.py::_KEYS` 中的全部键名、默认值、校验错误语义不变。
7. **数据目录**：`~/.praxis-rag/config.toml`、`RAG_SERVICE_DATA_ROOT`、SQLite `state.sqlite3` 位置不变。

## 4. 阶段计划

| 阶段 | 内容 | 状态 |
| --- | --- | --- |
| G0 | 迁移总纲、Go 模块骨架、配置/契约、HTTP 路由/认证/Console/健康检查 | 已完成 |
| G1 | 共享状态层：schema、MySQL/SQLite 仓储、加密、知识库/文档/API Key 读模型 | 已完成（part 1/2）；上传事务进行中 |
| G2 | 上传事务：UploadJournal、租约、blob、制品注册、制品/结果快照、缓存 outbox | 进行中（blob/制品/快照/缓存已落地，Journal/Coordinator 进行中） |
| G3 | 读取与切分：Markdown/TXT/DOCX 读取、PDF 解析进程编排、五类切分策略 | 已完成基础；资产/PDF chunks/公式进行中 |
| G4 | 检索：Qdrant 客户端、BM25 sparse、Dense/Hybrid、Reranker、orchestrator | 已完成；runtime 接入进行中 |
| G5 | 运行时：知识库创建/更新/重向量化、文档 CRUD、搜索、恢复线程 | 进行中（Service 已接线：搜索/文档读路径/工件；上传与 KB 管理进行中） |
| G6 | MCP：Streamable HTTP transport、工具/提示词目录、订阅通知 | 已完成（工具/提示词/resources/订阅/双协议） |
| G7 | 异步：Redis 缓存/搜索任务/快照、RocketMQ outbox、文档/搜索/DLQ worker | 进行中（redisstate/searchrefs 先行；MQ worker 待开始） |
| G8 | 迁移 CLI、Docker/脚本/文档切换、等价性回归、删除 Python 后端 | 待开始 |

核心决策补充：

- **MCP 传输采用官方 Go SDK** `github.com/modelcontextprotocol/go-sdk v1.7.0`，不再手写 JSON-RPC /
  Streamable HTTP。SDK 对 `>= 2026-07-28` 现代协议只支持 stateless 传输，对遗留协议
  （`<= 2025-11-25`）使用 stateful 会话；因此对接层需要一个按请求协议版本分发的双 handler，
  这属于实现细节，不改对外 URL 与认证契约。
- 工具/提示词目录（`internal/catalog`）与文档资源 URI/分页（`internal/resources`）已先于 MCP
  传输完成，MCP 层直接复用。

## 5. 验证策略

1. **模块级 Go 测试**：每个移植模块必须带 `_test.go`，把对应 Python 测试用例逐条翻译，
   断言消息/边界值/错误语义与原实现一致。
2. **黑盒等价性测试**：`internal/parity`（后续）用同一组请求分别打 Python 与 Go 服务（或
   以真实依赖 SQLite + 内存 Qdrant stub 启动 Go 服务），比对状态码、JSON 结构、关键字段。
3. **真实依赖冒烟**：MySQL/Redis/RocketMQ/Qdrant 使用 `docker-compose.yml` 起真实依赖，
   执行上传 → 列表 → 检索 → MCP 调用 → 异步任务全链路。
4. **删除门槛**：Python 后端仅在 Go 服务通过全部 parity 用例、压缩包/部署脚本切换完成、
   文档更新完毕后才删除；删除前保留一个可回退的 tag。

## 6. 已知风险与决策

- **Tokenizer 依赖**：Python 用 `transformers` 加载 BGE-M3 tokenizer 做 token 预算切分。
  Go 侧没有等价库，需在 G3 决策：纯 Go SentencePiece/Unigram 实现（推荐，需与
  `AutoTokenizer` 结果对齐验证）或把 tokenizer 作为外部小进程。切分边界必须保持可复现。
- **PDF/Word 解析**：在 Go 进程内完成——PDF 由纯 Go 阅读器抽取正文，DOCX 由内置 OOXML 读取器解析，Word 无图注图片由 Go 直接调用远端 VLM。只有 .doc 转换使用外部 LibreOffice。不保留 Python 解析进程。
- **RocketMQ Go SDK**：官方 Go client 的 gRPC 协议需与 Broker 5.3.3 + Proxy 兼容性实测；
  不通过时降级为自研 gRPC 封装，风险在 G7 处理。
- **Qdrant 本地路径模式**：Python `qdrant-client` 支持 `QDRANT_PATH` 本地文件模式；Go 版
  优先支持 HTTP 模式，本地路径模式在 G4 评估（可提示改用 HTTP，或用 `qdrant` 二进制）。
- **速率/并发语义**：Python 的同步阻塞 + 全局推理锁需在 Go 里以限流/互斥等价实现，
  避免模型调用被并发打爆。

## 7. 进度记录

### 2026-11-08 G0 起步

- 建立 Go module、`cmd/rag-service`、`internal/{version,config,contracts,chunking,runtime,httpapi}`。
- 移植：环境变量配置（Rag/Service/State）、`config.toml` 加载与校验、搜索契约校验、
  切分策略目录与切分配置校验、健康检查状态结构、租户认证/local-admin 校验、
  `/console` 静态资源、`no-store` 中间件。
- 验证：`go vet ./...`、`go test ./...`、本机启动 `/health` 冒烟。
- G0 验收记录：`go vet ./...` 通过，Go 测试 42 个用例全部通过；构建 `rag-service` 后在
  `127.0.0.1:18011` 实测 `/health` 返回 backend/collection/state、无凭证访问租户接口返回 401、
  `/v1/search-results/*` 返回 `Cache-Control: no-store`、Console 未构建时 `/console` 返回 404。

### 2026-11-08 并行迁移启动（G1/G3/G4/G6）

- 新增 `internal/catalog`：工具覆盖设置合并/无变化检测、提示词定义校验与渲染、持久化
  `catalog.Store` 接口与内存实现，对应 `rag_mcp/catalog.py`。
- 新增 `internal/resources`：`rag://knowledge/{namespace}/{name}` 规范化、严格查询参数校验、
  8000 字符段落分页、公式块/行内公式保护、`resource_page` 载荷，对应 `rag_mcp/resources.py`。
- 引入并锁定 MCP Go SDK `v1.7.0`；`/mcp` 传输方案与协议双代分发写入上表决策。
- 共享任务板启动四个互斥写域任务：task-1 状态层（`internal/state`）、task-2 检索栈
  （`internal/{qdrant,retrieval,modelapi,sparse}`）、task-3 文档读取与切分
  （`internal/{document,chunking}`）、task-4 MCP 服务层（`internal/mcp`）。
- 本轮新增测试：catalog 6 项、resources 6 项；`go test ./internal/catalog/... ./internal/resources/...` 通过。

### 2026-11-08 第二轮：状态层/检索/文档/MCP 并行落地

已提交（按提交顺序）：

- `cd3f852`+：MCP 目录、文档资源、`/v1/admin/tools` 与 `/v1/admin/prompts`。
- `a1843c0`：状态层 part 1（schema/crypto/store/四类仓储，61 pass）。
- `099b526`：SQLite 状态接入 `rag-service`、API Key 认证与管理接口（`/v1/admin/api-keys`、mcp-config）。
- `7d699bb`：文档读取与切分（document 31 + chunking 40 用例）与检索栈（qdrant/retrieval/modelapi/sparse，147 用例）。
- `feat(go): implement MCP server layer...`：官方 Go SDK MCP 层（双协议分发、6 工具、提示词、传输安全，57 用例）。
- `feat(go): add search, search-result and artifact endpoints`：`/v1/search`、`/v1/search-results/*`、`/v1/artifacts/*`。
- `feat(go): state part 2 ...`：blob/制品/结果快照/缓存同步/读视图/DPAPI（115 pass）+ `internal/kbconfig`（69 用例）+ `/v1/documents*`、`/v1/knowledge-base`、`/mcp` 挂载。

进行中：`task-6` 文档资产/PDF chunks/公式（go-document）、`task-8` MCP resources/订阅（go-mcp）、`task-9` 上传事务（go-state）、`task-10` runtime 搜索路径（go-retrieval）。

尚未开始的关键路径：`internal/runtime` 文档上传/删除/入库（等待 task-9 上传事务）、Redis 搜索快照/任务与 RocketMQ worker（G7）、管理端知识库创建/更新/重向量化、Console 构建集成与打包切换。

### 2026-11-08 第三轮：运行时与 MCP 全链路接线

新增提交：

- MCP resources/templates/read、artifact URI、subscriptions 与 `Endpoint.DocumentsChanged`（103 用例）。
- `internal/runtime` 搜索路径：`SearchEngine`（租户缓存 + Qdrant orchestrator + 公开 KB + 结果快照读取，20 用例）。
- `internal/runtime.Service`：搜索/公开 KB/结果页/文档列表/文档读取（md/txt/docx）/工件分块与下载/去重检查；上传、删除、异步入库暂时返回明确的不支持错误（等待上传事务与 G7）。
- `internal/wire`：HTTP/MCP 适配器（两个传输层对 `ListDocuments`/`ReadDocument` 的返回类型不同，运行时保持传输无关）+ `Notifier`（含文档变更通知）。
- `cmd/rag-service`：挂载 `/mcp`（legacy stateful / modern stateless 双协议，API Key + namespace 认证）、Catalog Store、ToolRegistry、ChangeNotifier、DocumentChangeNotifier；`/v1/admin/tools` 直接由 MCP Endpoint 提供。

端到端冒烟（真实 SQLite 状态 + 真实 Go 二进制，`127.0.0.1:18015`）：

```text
health=ok backend=qdrant state=sqlite
REST documents=0 supported=.doc,.docx,.md,.pdf,.txt
REST knowledge-base (missing)=404
REST admin prompts PUT changed=true
MCP initialize=200 (Mcp-Session-Id) -> initialized=202 -> tools/list 6 tools
MCP prompts/list has summary; prompts/get renders "Summarize E03"
MCP tools/call search -> isError=true, "该知识库没有配置记录，请先创建知识库"
MCP unauthenticated GET -> 401 + WWW-Authenticate: Bearer
REST /v1/admin/tools -> 6 tools
```

当前已知限制（Go 侧）：上传/删除/异步文档状态尚未接线（需要 task-9 上传事务与 G7）；PDF 文本预览需要外部解析器清单（读取原文件可用）；同步搜索结果快照尚未保存（task-13 完成 SearchReferences 后接入）。

### 2026-11-08 第四轮：上传事务、文档 ingest 与只读状态接口

新增提交：

- `feat(go): durable upload journal and coordinator`：UploadJournal + UploadCoordinator（操作/步骤/对象/head/租约/幂等/恢复/补偿/工作区安全，149 pass / 2 MySQL skip）。
- `feat(go): ingest adapter, Word chunk builder and chunking exports`：`internal/ingest`（md/txt/doc/docx/pdf 统一 Prepare 适配器、Word chunk builder、资产 staging、稳定 chunk id 与文档元数据）。
- `feat(go): upload status, published-version and lease read endpoints`：`/v1/uploads`、`/v1/uploads/{operation_id}`、`/v1/admin/published-versions`、`/v1/admin/upload-leases`（只读展示模型；前两者租户认证，后两者本机管理 + 租户双重校验）。

进行中：

- task-12（go-retrieval）：runtime 知识库 create/update/reembed/delete。
- task-13（go-mcp）：`internal/redisstate` + `internal/searchrefs`（结果快照与引用捕获）。
- task-14（go-state）：DocumentJobs/SearchTasks + outbox dispatcher（G7 状态机）。
- task-15（go-document）：Dockerfile/构建脚本/Makefile/冒烟工作流。

下一步：用 upload journal + ingest 适配器实现 `runtime.Upload/Delete/WriteDocument/DeleteDocument`（含 Qdrant 发布、blob/Artifact 持久化、文档变更通知），随后接线管理端知识库路由与 Redis 快照。

### 2026-11-08 第五轮：知识库管理、G7 状态机与上传路径启动

已提交：

- `feat(go): runtime knowledge-base administration`：`Service.List/Create/Update/DeleteKnowledgeBase`、key 读取、模型列表（kbconfig + 状态仓储 + Qdrant staging reembed + 版本栅栏 + Invalidate），34 runtime 测试全绿。
- `feat(go): document jobs, search tasks and outbox dispatchers`：G7 状态机（异步入库 job + outbox 单事务、SQL 租约/心跳/首终态、relay/debezium 模式；搜索任务 envelope/租约/失败/截止时间 + dispatcher 确认发布），165 state 测试。
- `feat(httpapi): knowledge-base administration endpoints and adapters`：`/v1/admin/knowledge-bases*`（list/create/models/details/key/patch）经 `internal/wire` 适配到 runtime；JSON 校验与 FastAPI 风格 422、错误映射 400/404/409/503；httpapi 测试全绿。

进行中：task-13 (`internal/redisstate` + `internal/searchrefs`)、task-15（Dockerfile/构建脚本）、task-16（runtime 上传/删除/写入路径，unexported entrypoints，Lead 负责替换 stub 委托）。

下一步：替换 `Upload/Delete/WriteDocument/DeleteDocument` stub 为 task-16 实现；接入 redisstate/searchrefs 生成 result_id 快照；构建 `rag-document-worker` / `rag-search-worker` / `rag-dead-letter-worker` 主程序（RocketMQ 客户端）；最后 G8 打包、parity 回归与 Python 后端删除。

### 2026-11-08 第六轮：Redis 状态层与搜索结果快照接线

已提交：

- `feat(go): Redis state layer, search reference capture and result snapshots wiring`
  - `internal/redisstate`：Redis StateCache + TransientStore（go-redis，前缀/修订水位/TTL/JSON/禁用 no-op/可用性错误映射，miniredis 测试）。
  - `internal/searchrefs`：SearchReferences.capture 移植（源文件/PDF·Word side-car/图片·表格资产 → 过期 Artifact 引用 + 不可变结果快照；失败降级不丢检索结果）。
  - `main.go`：`redisstate.NewFromEnv` → `Attach(stores)` → `stores.CacheSync.Start()`；`runtime.SearchEngine.SetReferenceCapture(searchrefs.New(...))`。
  - `SearchEngine.Search` 成功后自动捕获引用并写快照（Redis 不可用时 `snapshot_available=false` 安全降级）。

新增依赖：`github.com/alicebob/miniredis/v2 v2.40.0`（+ `yuin/gopher-lua` 间接，测试用）。

进行中：task-15（Dockerfile/构建脚本/Makefile，文件已出现）、task-16（runtime 上传/删除/写入，`documents_publish.go` 已出现）、task-17（`internal/mq` RocketMQ 传输层）。

### 2026-11-08 第七轮：构建打包

已提交 `build(go): Dockerfile, cross-platform build scripts, Makefile and app compose profile`：

- `Dockerfile`：三段式（node 构建 Console → golang 编译全部 `cmd/*` 静态二进制 → alpine 非 root 运行，`RAG_WEB_ROOT=/app/web`、`/health` HEALTHCHECK、`deploy/config.container.toml` 非敏感默认配置）。
- `.dockerignore`、`Makefile`、`scripts/build-go.ps1`、`scripts/build-go.sh`：构建 `dist/bin/<cmd>` 与 `dist/web`，内置冒烟（临时端口 + 临时 SQLite → `/health`、`/console`、`/`），支持 `-SkipFrontend`/`-SkipGo`/`-SmokeOnly`/`-Test`/`-FreshFrontendDeps`。
- `docker-compose.yml`：新增可选 `app` profile（默认 `docker compose up` 仍只启动基础设施；app 需要 `RAG_SERVICE_TOKEN`）。
- `.env.example`：补充 Go 服务/容器所需变量说明。

本机验证（无 Docker）：`pwsh -File scripts/build-go.ps1 -SmokeOnly` 通过，`/health 200 status=ok`、`/console` 返回构建后的 Console、`/` 200；`scripts/build-go.sh` 语法与 smoke 通过。容器镜像与 compose `app` profile 需在具备 Docker 的主机/CI 验证（受限项已记录）。

### 2026-11-08 第八轮：上传写入路径、RocketMQ 与打包

已提交：

- `feat(go): RocketMQ transport layer for async workers`：`internal/mq`（settings/消息编解码/Publisher 实现两个 state 发布接口/PullConsumer + SDK 封装；Go SDK v5.1.4；无 broker 环境，实机回环未验证）。
- `feat(go): runtime document upload, delete and write path`：`uploadDocuments/storeAndIndex`（写前日志 → 幂等重放 → 去重 → ingest → embedding/sparse → stage_qdrant → 单事务发布 → 补偿）、`writeDocumentContent`、`deleteDocumentByName`，以及 `documentIndexStore`/恢复补偿 seam；Lead 已把 `Service.Upload/Delete/WriteDocument/DeleteDocument` stub 替换为委托。**HTTP `/v1/documents*` 与 MCP 文档工具现已具备真实写入能力。**
- `build(go): Dockerfile, cross-platform build scripts, Makefile and app compose profile`（见上节）。

验证：`go build ./...` 全绿；runtime 46 测试、state 165、mq 8、httpapi/mcp/检索等全部通过。`dist/bin/rag-service.exe` + 真实构建 Console 的冒烟通过。

并行进行中（下一轮收口）：

| 任务 | 负责人 | 内容 |
| --- | --- | --- |
| task-18 | go-retrieval | runtime 异步受理 + 排队搜索 + 后台维护循环 |
| task-19 | go-state | migrate/CLI（rag-service-migrate、rag-state-migrate、ingest/debug） |
| task-20 | go-mcp | search/document/dead-letter worker 处理库 |
| task-21 | go-document | Go vs Python/前端 parity 审计报告 |

剩余主要工作：worker 主程序、异步文档状态（MCP add_document 接线）、queued search 等待语义、G8 文档/README 切换、黑盒 parity 回归、删除 Python 后端。

### 2026-11-08 第九轮：迁移与 CLI 命令

已提交 `feat(go): migration and CLI commands (service/state migrate, ingest, debug)`：

- `internal/migrate`：`NamespaceForLegacyUser`（"user-"+sha256[:16]）、`DiscoverLegacySources`（只读扫描/排序/过滤）、`BuildPlan`（稳定 document id/sha256/size）、`Apply`（blobs+documents 单事务/幂等/Force）、`StateSchemaStatus`/`EnsureStateSchema`（表就绪检查与幂等建表）。
- `cmd/rag-service-migrate`：默认预览，`--apply` 导入，`--legacy-root`/`--user-id`；退出码 0/1/2。
- `cmd/rag-state-migrate`：默认报告 schema 就绪度，`--apply` 幂等建表；退出码 0/1/2。
- `cmd/rag-mcp-debug`：真实 mock orchestrator 调试输出（无外部服务）。
- `cmd/rag-mcp-ingest`：明确限制的 TODO skeleton（退出码 2），不静默成功。

**迁移差异（必须记录）**：`rag-service-migrate --apply` 当前只导入状态（blobs+documents），**不执行向量重建**（Python 版会调用 runtime.upload）；需要在迁移后用文档 worker/重新上传重新索引。`--skip-assets` 与旧版解析资产树仍未纳入发现计划（TODO）。

验证：gofmt/vet clean；`go test -count=1 ./internal/migrate/...` 4/4 通过；`go build ./...` 全绿。

### 2026-11-08 第十轮：异步受理、排队搜索与后台维护接线

已提交 `feat(go): async admission, queued search and background maintenance`：

- `async.go`：`addDocumentAsync`（DocumentDispatcher 单事务受理 + best-effort relay，返回 `{"artifact_id"}`，Python 冲突/校验错误映射）、`documentStatus`（4 键状态投影）。
- `queued_search.go`：`submitSearchTask`（无 broker 时 fail-closed；Submit+DispatchPending 确认发布）、`submitSearch`（wait_seconds 0..30 + 100ms 轮询；完成返回快照、超时只返回 `{"result_id"}`）、`getSearchResultOrdered`（先 SearchTasks 活动视图，再快照）。
- `maintenance.go`：`StartBackground/StopBackground`（mq publisher 注入、cache-sync/document relay、10s 维护循环：上传恢复清理、SearchTasks.Expire、DocumentJobs.Sweep/GC、结果快照与 Artifact 过期清理；无 broker 时降级不阻塞启动）。
- Lead 已完成 5 处 `service.go` 委托与 `main.go` `StartBackground`，`Close` 先 `StopBackground`。

验证：`go build ./...` 全绿；runtime 59 测试通过；二进制冒烟（真实 SQLite）：health/documents/uploads/MCP initialize 均正常，无 broker 时仅输出降级日志。

并行收尾：task-20（worker 处理库，`internal/{searchworker,documentworker}` 已通过测试并有 WIRING.md）、task-21（parity 审计报告）。

### 2026-11-08 第十一轮：parity 审计与 P0/P1 修复启动

- `docs/GO_PARITY_AUDIT.md` 已提交：前端调用的全部端点均存在；P0 = 搜索资产投影缺失、MCP 同步/排队模式、worker 可执行缺失、ReadDocument 未读 document.md；P1 = snapshot_context、documents/check 扩展名校验、KB HTTP 校验漂移、delete_document annotation、md/txt 校验、cached read views、OpenAPI。
- 已修复并提交：`fix(go): parity fixes from audit (queued search flag, check suffix validation, delete annotation)` —— MCP `QueuedSearch` 改为按 `mq.SettingsFromEnv().Configured()` 自动选择（无 broker 时保持同步可用），`/v1/documents/check` 增加 Python 一致的扩展名 400 文案，`delete_document` destructive annotation 修正。
- 进行中：task-22（runtime ProcessDocumentJob/RecoverDocumentUploads）、task-23（三个 worker 主程序）、task-24（P0-1 资产投影 + P0-4 document.md 预览）、task-25（snapshot_context/OpenAPI/KB 校验）。

审计建议顺序：P0-4+P0-1（共享资产/版本对象读取）→ P0-2 queued 标志 + MCP 测试 → P0-3 workers + ProcessDocumentJob → P1 批次 → 内部 parity 黑盒差分 → 删除 Python。

### 2026-11-08 第十二轮：G7 闭环（jobs 执行 + 三个 worker 可执行程序）

已提交：

- `feat(go): runtime document-job execution with publication validation`：`Service.ProcessDocumentJob`（终态幂等、身份/执行栅栏校验、KB 版本预检、progress 阶段、journal task 绑定、**事务内 ValidatePublication**、成功 Succeed）、`Service.RecoverDocumentUploads`；`documents_upload.go`/`documents_publish.go` 仅做 nil-safe 增量编辑（Task 字段/进度/事务内校验），65 runtime 测试通过。
- `feat(go): document, search and dead-letter worker binaries`：`cmd/rag-document-worker`（broker 消费或 `RAG_DOCUMENT_WORKER_LOCAL=1` 本地 outbox 模式 + 30s Recover ticker）、`cmd/rag-search-worker`（消费 schema-2 envelope → `SearchRaw` → `searchrefs.Capture(false)` → `SearchTasks.Complete`）、`cmd/rag-dead-letter-worker`（`--kind search|document`，`%DLQ%<source group>`，仅终态/不存在时 ack）。统一 exit code 0/1/2，worker 不调用 `StartBackground`。

已验证：`go build ./cmd/...`、`go vet ./cmd/...` 全绿；三个二进制实跑：无 broker → 2（明确文案）、错误 backend → 2、`--kind` 非法 → 2、`--help` → 0、document worker 本地模式可启动并创建 SQLite 状态。

**Worker 运行环境要点**（完整表见 CLI 输出/GO_MIGRATION 计划）：共享 `RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`RAG_REDIS_*`、`RAG_BACKEND=qdrant`、`QDRANT_URL`、`RAG_ROCKETMQ_*`；搜索 worker 另有 `RAG_SEARCH_TOPIC/GROUP/MAX_ATTEMPTS`；文档 worker 另有 `RAG_DOCUMENT_TOPIC/GROUP/BROKER_MAX_RETRIES`、`RAG_DOCUMENT_WORKER_LOCAL/POLL_SECONDS/RECOVER_SECONDS`；DLQ worker 另有 `RAG_*_DLQ_GROUP/TOPIC`。

### 2026-11-08 第十三轮：P0/P1 parity 修复完成

已提交：

- `fix(go): P1 parity (snapshot_context, OpenAPI/docs, KB validation)`：搜索 payload 增加 Python 一致的 `snapshot_context`（collection/embedding_model/reranker_model/vector_size/chunk_tokenizer_model/configuration_version，Search 与 SearchRaw 均生效）；`/openapi.json`（OpenAPI 3.1，34 个操作，SearchBody rerank enum）+ `/docs`（自包含页面）；KB 管理 create/PATCH 校验与 Python 422 对齐（embedding 必填、模型名长度、loc 正确）。
- `fix(go): P0 parity (search asset projection, saved document.md preview)`：`pdf_assets.project_results` 移植（image base64/table rows/captions/footnotes/location/page/bbox/metadata 增补、upload_version 可见性过滤、失败安全降级）接入 tenant orchestrator，Search/SearchRaw 均产出 asset；`ReadDocument` 对版本化 PDF/DOC/DOCX 读取已落盘 `document.md`（缺失时 Python 文案 "Saved content unavailable; reimport this document"），md/txt 严格 UTF-8 + `MAX_EXTRACTED_CHARACTERS` 校验。

**全量验证**：`go build ./...` + `go test -count=1 ./...` 全部 23 个包通过（state 165、runtime 79、mq 8、workers 27、httpapi/mcp/searchrefs/检索栈等）。

进行中：task-26（异步 e2e 集成测试）、task-27（P1-6 cached read views / P1-2 / P2 静态服务）、task-28（G8 文档切换）、task-29（真实 rag-mcp-ingest CLI）。

## 当前切换状态（G8）

> 本节是 G8 文档切换时追加的**状态快照**，不修改上方历史记录；后续进度请在本节末尾追加，不要改写历史轮次。快照时间：2026-11-08，以本次文档提交时的仓库状态为准。

### 已切换 / 已验证

- **用户文档已切换到 Go 后端**：[README](../README.md)、[USAGE](USAGE.md)、[DEPLOYMENT](DEPLOYMENT.md) 以 Go 服务 + 三个 worker 为主路径；新增 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md) 作为契约差异基线。本文历史记录保持原样。
- **Go 目标后端已具备完整可运行面**：`rag-service`（REST `/v1/*`、MCP `/mcp`、Console `/console`、`/health`、OpenAPI `/openapi.json` + `/docs`、进程内 outbox relay、后台维护循环）、`rag-document-worker`、`rag-search-worker`、`rag-dead-letter-worker`，以及 `rag-service-migrate`、`rag-state-migrate`、`rag-mcp-debug` 和 `rag-mcp-ingest`（真实本地文件/目录批量导入，走运行时上传管线）。
- **Parity P0 已关闭**：搜索资产投影（`pdf_assets.project_results` 移植）、MCP 排队/同步模式自动选择（按 `RAG_ROCKETMQ_ENDPOINT`）、三个 worker 可执行程序 + `ProcessDocumentJob`/`RecoverDocumentUploads`、已保存 `document.md` 预览；主要 P1（snapshot_context、`/v1/documents/check` 扩展名、KB HTTP 校验、delete annotation、md/txt UTF-8/长度、OpenAPI/docs）已修复并提交。
- **验证边界**：`go build ./...` 与全量 `go test -count=1 ./...` 通过；`scripts/build-go.ps1` 本机构建 + 冒烟通过；worker 的 `--help`/无 broker/错误 backend/非法 `--kind`/本地 outbox 模式退出码行为已验证。**未连接真实 MySQL/Redis/RocketMQ/Qdrant**，未运行 Docker/Compose/systemd，未在 Go 环境实机验证 Docling/LibreOffice/视觉链路，也没有浏览器 E2E；真实 broker SDK 兼容性是已知风险（见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md) 与 [DEPLOYMENT](DEPLOYMENT.md)）。

### Python 后端的定位（已删除）

- 原 Python 后端（`src/rag_mcp/` 的 HTTP/MCP/状态/检索/文档/测试、`tests/`、`pyproject.toml`、`uv.lock`）**已从仓库删除**。
- **Python 解析 worker 已删除（2026-11-10）**：`python-workers/`（PDF Docling worker 与 Word 视觉 worker）与 `benchmarks/` 评估脚本当日一并删除；随后又以**独立 Docling PDF 解析微服务**（`python-workers/pdf-service`）的形式恢复了 PDF 高保真解析——它只在 `RAG_PDF_ENGINE=docling` 时被 HTTP 调用，不进入 Go 构建/镜像/测试依赖，Go 主服务仍无 Python 运行时依赖。PDF 的 Go 内置纯文本引擎（`RAG_PDF_ENGINE=go`）继续可用；DOCX 解析与 Word 图片描述仍由 Go 实现。详见文末「2026-11-10 恢复 Docling 外部解析为独立微服务」。
- `examples/` 保留示例知识库文件。
- 数据层兼容：Go 复用既有 MySQL/SQLite schema、`enc:v1:` 凭据与 `~/.praxis-rag/` 配置/数据目录；`rag-state-migrate` 可检查/幂等建表，`rag-service-migrate --apply` 只导入状态、**不重建向量**（需重新上传或等待 reindex）。
- 已删除 Python 旧路径专用项：`RAG_OUTBOX_BRIDGE=debezium`（Go 未实现，配置 broker 时 Go 总是进程内 relay）、Python worker 命令、`scripts/start-stack.ps1` 的 Python 入口（已改写为 Go 流程）、`scripts/start-debezium-stack.ps1`。

### 尚未完成（G8 关闭条件）

1. **真实依赖 e2e**：异步链路已由 `internal/e2e`（fake Qdrant/模型 + 内存 broker/Redis + 真实 SQLite/runtime）覆盖并通过；MySQL/Redis/RocketMQ/Qdrant 实机回归与 Go RocketMQ SDK 对 Broker 5.3.3 + Proxy 的兼容性仍需在具备环境的主机验证。
2. **parity 收尾**：P0 全部关闭，主要 P1 已修复；剩余 P2（casefold 排序近似、API key 列表 read-view 来源、少量错误形状）与同步上传 notifier 的权威快照细节见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)。Python 对照实现已删除，因此黑盒差分改为“固定预期/契约测试”。
3. **已知迁移差异需持续标注**：Go tokenizer 是确定性近似；只有 .doc 转换依赖外部 LibreOffice；Docker/systemd 实机验证与浏览器 E2E 仍未执行；真实 VLM 端点尚未实机联调。
4. **打包切换**：`scripts/build-go.ps1`（含冒烟）与 Dockerfile/Compose `app` profile 已切到 Go；管理命令与迁移 CLI 全部为 Go 二进制。

### 下一步（按依赖顺序）

1. 在具备 Docker 的 CI/主机验证镜像、Compose `app`/worker 与 systemd 模板。
2. 连接真实 MySQL/Redis/RocketMQ/Qdrant 做一次全链路冒烟（含 `rag-mcp-ingest` 真实导入、MCP queued search、DLQ）。
3. 持续推进 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md) 剩余 P2 项，并把黑盒差分改为固定预期契约测试。

### 2026-11-08 第十四轮：删除 Python 后端（G8 后端切换完成）

- `git mv` 外部解析 worker → `python-workers/rag_mcp/`（仅 `document_readers.py`、`chunk_strategies.py`、`chunking.py`、`pdf_content.py`、`pdf_worker.py`、`word_vision_worker.py` 与新 `__init__.py`），并新增 `python-workers/README.md`、`requirements.txt`。
- 删除 `src/rag_mcp/`、`tests/`、`pyproject.toml`、`uv.lock`、`scripts/start-debezium-stack.ps1`；`scripts/start-stack.ps1` 改写为 Go 流程（构建检查 → compose → `rag-state-migrate --apply` → `rag-service-migrate` → `dist/bin/rag-service.exe`）。
- Console 构建输出从 `src/rag_mcp/web` 迁移到 `build/web`（vite/构建脚本/Dockerfile/.gitignore/`resolveWebRoot` 同步更新），不再保留 Python 包目录。
- 文档同步：README/USAGE/DEPLOYMENT/本文件更新为“Python 后端已删除”，`docs/GO_PARITY_AUDIT.md` 标注为历史审计快照；旧设计文档（api-keys/knowledge-base-settings/result-references/state-cache/state-storage）加注为历史设计参考（Go 实现见 `internal/` 对应包）。
- 验证：`go build ./...`、`go test -count=1 ./...` 全绿；`scripts/build-go.ps1` 全量构建 + 冒烟通过（含迁移后的 `build/web` 路径）。

### 2026-11-08 第十五轮：实现并实机验证 `RAG_OUTBOX_BRIDGE=debezium`

- **实现状态**：`internal/state` 的 bridge 模式移植早已存在（`documents` 使用 `DocumentBridgeModeFromEnv`，debezium 时 `DocumentDispatcher.Relay=nil`，应用不发布），本次补齐运行时可观测性、回归测试、验证脚本与文档，并在真实栈上跑通。
- **实机环境**：MySQL 8.0.46 + Qdrant + Redis + RocketMQ 5.3.3（gRPC proxy 127.0.0.1:18081）+ Debezium Server 3.7.1.Final；`rag-service` 与 `rag-document-worker` 连接同一栈。
- **验证链路**：MCP `add_document` → MySQL `document_job_outbox` 提交（保持 PENDING）→ Debezium 读取 binlog → topic `praxis_document_ingest` → worker 消费 → `ProcessDocumentJob` → Qdrant + documents → `get_document_status=succeeded`；REST `/v1/search` 命中该文档。
- **证据**：`scripts/verify-debezium-e2e.py` 输出 14 PASS / 0 FAIL；发件箱行在投递前后始终 `published_at IS NULL`，服务日志输出 `in-process relay disabled`——证明仅 Debezium 投递。
- **顺带修复两个 MySQL 兼容缺陷**（SQLite 测试无法暴露，gated MySQL 测试与实机暴露）：
  1. `internal/state/store.go`：`Config.Params["charset"]` 被驱动执行为 `SET charset=...` → MySQL 1193；改为依赖驱动默认 utf8mb4。
  2. `internal/state/document_jobs.go`：`LIKE ... ESCAPE '\\'` 在 MySQL 是语法错误；改用 `ESCAPE '!'` 与对应 `escapeLike`，新增 `TestEscapeLikeUsesPortableEscapeCharacter` 与 gated `TestMySQLDocumentTaskLookupSQL`。
- **MySQL 验证**：本地容器上 `TestMySQLStoreRoundTrip`、`TestMySQLG7StateMachines`、`TestMySQLUploadJournalRoundTrip`、`TestMySQLDocumentTaskLookupSQL` 全部通过。
- **文档/工具**：README、USAGE、DEPLOYMENT、`deploy/debezium/README.md` 标注为已实现并验证；`scripts/start-stack.ps1` 在 debezium 模式等待 "Starting streaming"；新增可配置的 `scripts/verify-debezium-e2e.py`。

### 2026-11-09 第十六轮：RocketMQ → Kafka 完整切换

- **代码**：`internal/mq` 从头重写为 `github.com/segmentio/kafka-go`（producer/consumer/settings/message；RocketMQ SDK 与相关依赖移除，`go.mod`/`go.sum` tidy）。`internal/state`/`internal/config` 切换到 `RAG_KAFKA_*`、`RAG_CACHE_*`、`RAG_DOCUMENT_*`、`RAG_SEARCH_*` 与显式 `RAG_*_DLQ_TOPIC` 契约；`internal/runtime` 按 Kafka 发布者接线并保持 `RAG_OUTBOX_BRIDGE=relay|debezium` 语义（debezium 时应用不启动 relay）。
- **Kafka 环境契约**：`RAG_KAFKA_BOOTSTRAP_SERVERS`（逗号列表；宿主机 `127.0.0.1:9092`，compose 容器 `kafka:9092`；空 = brokerless）、`RAG_KAFKA_CLIENT_ID`、`RAG_KAFKA_SECURITY_PROTOCOL`、`RAG_KAFKA_SASL_MECHANISM/USERNAME/PASSWORD`、`RAG_KAFKA_TLS_CA/TLS_INSECURE`、`RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS`；cache/document/search topic+group；文档/搜索 DLQ 使用显式 `RAG_DOCUMENT_DLQ_TOPIC`/`RAG_SEARCH_DLQ_TOPIC` 与独立 `RAG_DOCUMENT_DLQ_GROUP`/`RAG_SEARCH_DLQ_GROUP`，不再使用 `%DLQ%<源组>` 或用 mqadmin 建组。
- **消费语义**：handler 成功 → 提交 offset；nack → 同一进程内重试同一条记录，指数退避 100ms 起、上限 5s，`DeliveryAttempt` 1..`MaxAttempts`；耗尽后把原始消息发布到对应 DLQ topic、执行终态回调、再提交 offset；`MaxAttempts=0` 一直重试到 context 取消；整体为至少一次投递。死信 worker `--kind document|search` 消费显式 DLQ topic，仅在持久终态写入后提交。
- **部署**：`docker-compose.yml` 删除全部 RocketMQ 服务/配置/卷，新增单节点 KRaft `kafka`（`apache/kafka:3.9.0`，INTERNAL `kafka:9092` / EXTERNAL `127.0.0.1:9092`，4 partitions、auto-create、`kafka_data`、`KAFKA_LOG_DIRS=/var/lib/kafka/data`）；Debezium 改为原生 Kafka sink（`debezium.sink.type=kafka`、`${RAG_KAFKA_BOOTSTRAP_SERVERS:kafka:9092}`、String serializers、`acks=all`、ByLogicalTableRouter 到 `RAG_DOCUMENT_TOPIC`）；删除 `deploy/rocketmq-broker.conf`、`deploy/rocketmq-proxy.json`；`deploy/debezium/application.properties` 只保留 MySQL source + Kafka sink + 文件 offset/schema + JSON（`schemas.enable=false`），并关闭 `include.schema.changes` 避免 DDL 进入文档 topic。
- **脚本/配置**：`scripts/start-stack.ps1` 移除 mqadmin/subgroup/DLQ 预建，改为等待 Kafka healthy、显式创建 cache/document/search 与两个 DLQ topic、debezium 模式等待 `Starting streaming`；`scripts/stop-stack.ps1`、`scripts/verify-debezium-e2e.py`（kafka-console-consumer 探测替代 mqadmin）同步更新；`.env.example` 全量替换为 Kafka 契约并记录 host/container bootstrap 差异。
- **实机验证（Windows + Docker Desktop）**：MySQL 8.0 + Debezium Server 3.7.1.Final + Kafka 3.9.0。Kafka healthy、日志 `log.dirs=/var/lib/kafka/data`、`num.partitions=4`；Debezium `Snapshot completed` + `Starting streaming`，原生 sink ProducerConfig 为 `bootstrap.servers=[kafka:9092]`、`acks=-1`、idempotence true、String serializer；`praxis_document_ingest` 自动创建（4 partitions/RF1），消费到与 MySQL `document_job_outbox` 一致的记录，outbox 行保持 `PENDING`（应用未发布）；保留 MySQL/Qdrant/Redis 数据卷，旧 RocketMQ 容器与卷已清理。`go build ./...`、全量 `go test -count=1 ./...` 通过。
- **文档**：README/USAGE/DEPLOYMENT/state-cache/state-storage/result-references/GO_PARITY_AUDIT/VALIDATION 更新为 Kafka 传输与显式 DLQ 语义；GO_PARITY_AUDIT 的 RocketMQ 字样标注为历史；本文件历史轮次保留原样。
- **验证补充（2026-11-09）**：Go worker 消费真实 Kafka 的完整 async 链路已实机跑通（`scripts/verify-debezium-e2e.py`：debezium 模式 14/14、relay 模式 13/0；发件箱分别保持 PENDING / 标记 PUBLISHED），排队搜索（`scripts/verify-kafka-search.py`，含 Redis 快照）与 poison→显式 DLQ topic（`scripts/verify-kafka-dlq.py`）也已验证。仍未验证：Kafka SASL/TLS 集群、Docker `app` profile/镜像、systemd、MySQL 多实例故障切换。

### 2026-11-10 恢复 Docling 外部解析为独立微服务（Go 端不引入 Python 依赖）

- **背景**：2026-11-10 删除 `python-workers/` 后，PDF 只走 Go 内置纯文本引擎；公式被当作普通文本处理，实测出现 Ω/⊗ 等符号乱码，上标、分式、公式编号等结构全部丢失。本轮把 Docling 恢复为**独立 HTTP 微服务**，而不是回到 Go 进程内拉起 Python 子进程：PDF 高保真解析需要 Python/Docling 与模型，但这不应成为 Go 服务的构建或运行依赖。
- **服务端**：新增 `python-workers/pdf-service/`（FastAPI + `docling==2.126.0` + CodeFormulaV2，CPU torch）：
  - `GET /healthz` 健康检查；`POST /v1/parse`（multipart 字段 `file`）返回 `application/zip`，zip 根目录含 `manifest.json` 与 `assets` 引用的图片（如 `pictures/picture-0.png`）；错误返回 `{"detail": <安全消息>}`，不回显文档正文。
  - Docling 惰性导入（`import app.main` 不要求已安装 docling），实测/联调可用 `RAG_PDF_SERVICE_CONVERTER=fake` 返回固定清单。
  - 公式识别：配置了 `RAG_VLM_*` 时使用远端 OpenAI 兼容 VLM，否则使用本地 CodeFormula 模型；模型目录由 `RAG_PDF_MODELS_PATH` 指定（compose 卷 `pdf_parser_models` 挂到 `/models`）。
  - 其余环境变量：`RAG_PDF_SERVICE_HOST`/`RAG_PDF_SERVICE_PORT`（默认 `0.0.0.0:8091`）、`RAG_PDF_SERVICE_CONCURRENCY`（默认 1）、`RAG_PDF_SERVICE_TIMEOUT`（默认 900 秒）。
- **与旧 subprocess worker 的区别**：旧实现由 Go 通过 `exec` 拉起 `python -m rag_mcp.pdf_worker`，依赖本机 Python 环境与 `PYTHONPATH`、用 stdout JSON 清单 + 共享临时目录传资产、stderr 混在进程输出里，缺失模块时只能靠 stderr 文本诊断；新实现是网络服务——Go 只发 HTTP multipart、收 zip 后安全解压，不再需要本机 Python，也不需要解析子进程退出码/stdout/stderr。清单版本从 `docling-formula-remote-v3` 改为 **`docling-service-v1`**；Go 端 `IsSupportedPDFParserVersion` 同时接受 `go-native-v1`、`docling-service-v1` 与历史 `docling-formula-remote-v3`，已存文档不需要迁移。
- **Go 端**：`internal/document/docling_service.go` 的 `DoclingServiceParser` 实现 `ingest.PDFParser` 与 `document.PDFPages`：`ValidatePDFFile` → multipart `POST {BaseURL}/v1/parse` → 校验 200 与 `application/zip`（兼容 `octet-stream`）→ 安全解压到 staging（拒绝绝对路径/`..`/symlink/重复条目，限制条目数与解压大小）→ 解析 `manifest.json` 并按支持版本校验；失败统一返回 `*document.PDFParseError`（安全消息）。`internal/config` 新增 `RAG_PDF_ENGINE`（`go|docling|空`；空 = 配置了 `RAG_PDF_SERVICE_URL` 则 docling，否则 go）、`RAG_PDF_SERVICE_URL`（默认 `http://127.0.0.1:8091`，校验为绝对 `http(s)` URL）、`RAG_PDF_SERVICE_TIMEOUT`（默认 900，1..7200 秒）；运行时按引擎注入解析器，并用 `PDFParserVersionForEngine` 记录 `pdf_parser_version`（docling → `docling-service-v1`，go → `go-native-v1`）。
- **部署**：`docker-compose.yml` 新增默认启动的 `pdf-parser`（独立镜像 `praxis-rag-pdf-parser:local`，容器名 `praxis-rag-pdf-parser`，容器内 8091 / 健康检查 `/healthz` / 卷 `pdf_parser_models`，无 profile）；`app` 增加 `depends_on: pdf-parser: service_started` 与 `RAG_PDF_ENGINE`（默认 `docling`）、`RAG_PDF_SERVICE_URL`（默认 `http://pdf-parser:8091`）、`RAG_PDF_SERVICE_TIMEOUT`（默认 900）。`.env.example` 增加 `RAG_PDF_ENGINE`/`RAG_PDF_SERVICE_URL`/`RAG_PDF_SERVICE_TIMEOUT`/`RAG_PDF_PARSER_IMAGE`/`RAG_PDF_SERVICE_PORT`/`RAG_PDF_MODELS_PATH`/`RAG_PDF_SERVICE_CONCURRENCY` 与 VLM 共用说明。纯 Go 引擎部署可 `docker compose up -d mysql qdrant redis kafka` 跳过 pdf-parser。
- **文档**：README、USAGE、DEPLOYMENT 按「默认 docling + 可切回 go」重写 PDF 解析章节；本文件补充本节；PROGRESS 追加同日记录。
- **限制**：`docling` 引擎需要 pdf-parser 服务与已预置的 Docling/CodeFormula 模型（CPU torch 镜像较大，首次使用前需联网下载模型并放入模型卷）；服务不可用或超时时 docling 上传会失败（安全可读错误），可显式 `RAG_PDF_ENGINE=go` 回退纯文本。公式识别需要远端 VLM（`RAG_VLM_*`）或本地 CodeFormula 模型，两者都没有时 `/v1/parse` 返回 503、解析失败；版面/表格/OCR 仍只有 Docling 提供。pdf-parser 未加认证，只应发布在 loopback/内网。`RAG_PDF_SERVICE_CONVERTER=fake` 仅用于联调测试。真实 Docling 推理、镜像构建与模型下载尚未在本仓库验证环境实机执行。
- 本节之前的轮次记录保持原样；上方 G8 快照中「仓库不再包含 Python 代码」的表述由本节取代。
