# 后端 Go 重构总纲

最后更新：2026-11-08

## 1. 目标

把 Praxis-RAG 的后端从 Python（`src/rag_mcp`，约 69 个模块 / 11.5k 行）完整迁移到 Go，
最终删除 Python 后端代码，同时保持以下对外契约不变：

- REST API 路径、认证方式、状态码和响应结构；
- MCP Streamable HTTP 端点 `/mcp` 的工具名、JSON Schema、错误语义；
- 环境变量、`~/.praxis-rag/config.toml` 配置项、数据目录和共享状态数据库结构；
- Web Console 静态资源的访问路径；
- 与 Qdrant、MySQL/SQLite、Redis、RocketMQ 的现有交互格式与既有数据。

前端（`frontend/`，React + Vite）不重写；构建产物仍输出到 Console 静态目录。
PDF（Docling）与 Word/PDF 视觉解析属于外部 Python 解析进程，不在 Go 服务内重新实现，
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
- **PDF/Word 解析**：Docling/视觉模型保持外部 Python 解析进程，Go 通过文件协议调用；
  这属于「外部解析器」而非后端业务逻辑，需在 README 中明确。
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
