# Praxis-RAG 使用与部署（Go 后端）

本文面向运行 **Go 后端**（`rag-service` + 文档/缓存/死信三个 worker）的用户，介绍构建、配置、启动、REST/MCP 使用、worker、迁移与切换 CLI、Docker 与验证边界。产品概览见 [README](../README.md)；迁移背景与阶段见 [GO_MIGRATION](GO_MIGRATION.md)；契约差异见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)；容器与 systemd 示例见 [DEPLOYMENT](DEPLOYMENT.md)。

```text
浏览器 ------------------------> /console
MCP 客户端 -> /mcp ------------> rag-service ──> Qdrant
REST /v1/* --------------------> rag-service ──> 共享状态库（MySQL：任务/artifact/缓存水位）
                                             ├─> Redis（读模型展示缓存；可选）
                                             └─> Kafka（Debezium 文档/缓存通知；文档处理必需）

网页/REST 上传（默认异步）与 MCP add_document
                 -> 状态库单事务写 document_jobs（schedule_version=1）
                 -> 有 Kafka：Debezium 任务连接器 -> Kafka 通知 -> rag-document-worker
                 -> 无 Kafka：没有消费者，文档任务保持 queued（rag-service 只受理）
                 -> rag-document-worker 完成解析/切分/Embedding/Qdrant 发布，用
                    GET /v1/document-jobs/{artifact_id} 轮询状态
REST 批次上传 POST /v1/document-batches（Console 使用）
                 -> 每文件一个事务并行受理（按文件名加锁，不同名互不排队）
                 -> 202 返回 batch_id + 每个文件 accepted/skipped/failed
                 -> GET /v1/document-batches/{batch_id} 同时给出保存结果与解析阶段
MCP search / REST /v1/search（同步）-> runtime.Service.Search -> 检索 + 重排 + artifact 投影
                 -> 同一次响应返回 results / artifacts / evidence_status / answer_guidance
                 -> 无任务 ID、无轮询、无搜索 topic；总预算 RAG_SEARCH_TIMEOUT_SECONDS
业务表变更（含手工 SQL）-> Debezium 缓存连接器 -> Kafka -> rag-cache-worker
                 -> cache_revisions.revision+1 -> 重建并写 Redis（失败由 RefreshDirty 收敛）
Kafka 死信（显式 DLQ topic）-> rag-dead-letter-worker --kind document|cache
```

RAG 只返回证据，不生成最终回答。后端为 Go 实现：DOCX 读取与 Word 图片描述在 Go 进程内完成（图片描述调用远端视觉模型），PDF 默认交给独立的 PDF 视觉解析微服务（`python-workers/pdf-service`，HTTP + zip 清单，本地只做 PDF 读取/渲染/裁图，版面/OCR/表格/公式/图片描述统一调用远端 OpenAI 兼容视觉模型；可用 `RAG_PDF_ENGINE=go` 切回内置纯文本引擎）。VLM 配置由 Console「模型配置」与知识库设置按知识库下发，缺失时 PDF 上传会在受理阶段提示先去配置。Go 服务本身不依赖 Python，只有 pdf-parser 微服务使用 Python。

## 1. 安装与构建

### 前置条件

| 依赖 | 版本/要求 | 用途 |
| --- | --- | --- |
| Go | 1.27+（`go.mod`） | 构建 `rag-service` 与全部命令 |
| Node.js | 20.19+ 或 22.12+ | 构建 Console（只使用 API/MCP 可跳过） |
| Qdrant | HTTP API（默认 `6333`） | 检索索引；Go **不支持** `QDRANT_PATH` 本地文件模式 |
| 共享状态库 | MySQL 8.0+（`utf8mb4`） | 文档、KB、API Key、任务、artifact 快照、缓存 revision 水位、UploadJournal |
| Redis | 可选，7.x | 读模型展示缓存；搜索为同步直返，artifact 事实在 MySQL，Redis 丢失不影响读取 |
| Kafka | 文档入库与缓存刷新必需，单节点 KRaft（Compose `kafka`）或外部集群 | 两类通知：文档任务、缓存变更（均只含定位标识）；可用 SASL/TLS 环境变量连接外部集群，Compose 本身只提供 PLAINTEXT。需两个 Debezium 连接器 |
| PDF 视觉解析微服务 | 可选（默认 compose 启动） | PDF 版面/OCR/表格/公式/图片描述统一由远端视觉模型完成（`python-workers/pdf-service`，容器端口 8091，本地只做读取/渲染/裁图）；`RAG_PDF_ENGINE=go` 时不需要。vision 引擎还需要在 Console 为知识库配置 VLM 模型 |
| LibreOffice | 可选 | 仅 DOC 转换需要外部程序；PDF（go 引擎）/DOCX 解析不依赖它 |

### 构建命令

```powershell
# Windows PowerShell 5.1+ / 7+
pwsh -File scripts/build-go.ps1                 # 前端 + Go 二进制 + 本机冒烟
pwsh -File scripts/build-go.ps1 -SkipFrontend   # 复用已有 dist/web
pwsh -File scripts/build-go.ps1 -SkipGo         # 只重建 Console
pwsh -File scripts/build-go.ps1 -SmokeOnly      # 只跑本机冒烟
pwsh -File scripts/build-go.ps1 -Test           # 构建后 go test ./...
```

```bash
# POSIX（Makefile 内部调用 scripts/build-go.sh）
make build       # frontend + go
make test        # go test ./...
make vet         # go vet ./...
make smoke       # 启动 dist/bin/rag-service 检查 /health、/console、/
make clean
```

构建产物（`dist/`）：

```text
dist/bin/rag-service(.exe)
dist/bin/rag-document-worker(.exe)
dist/bin/rag-cache-worker(.exe)
dist/bin/rag-dead-letter-worker(.exe)
dist/bin/rag-service-migrate(.exe)
dist/bin/rag-cdc-cutover(.exe)
dist/bin/rag-state-migrate(.exe)
dist/bin/rag-mcp-debug(.exe)
dist/bin/rag-mcp-ingest(.exe)        # 本地文件/目录批量导入，见 §7
dist/web/                            # Console（服务通过 RAG_WEB_ROOT 读取）
```

脚本默认把 Console 复制到 `dist/web`；直接运行 `dist/bin/rag-service` 时请设置 `RAG_WEB_ROOT=<repo>/dist/web`，否则服务只探测当前目录/可执行文件邻居的 `web` 或 `build/web`。构建脚本的 smoke 模式会自动设置临时端口、临时 config 与数据目录。

### 前端构建与开发

Console 使用 React + TypeScript + Ant Design；vite 输出到 `build/web`（`.gitignore` 忽略），Go 构建脚本会再复制到 `dist/web`。手工构建：

```powershell
Push-Location .\frontend
try { npm ci; npm run build } finally { Pop-Location }
```

热更新开发服务器（把 API 代理到本地服务）：

```powershell
Set-Location .\frontend
$env:RAG_DEV_API = "http://127.0.0.1:8011"
npm run dev
```

只使用 API/MCP 时无需构建前端；未构建时 `/console` 返回 404。

## 2. 启动基础设施

### Docker Compose

仓库自带 `docker-compose.yml`，默认 profile 启动基础设施（MySQL、Qdrant、Redis、Kafka）以及 PDF 解析微服务 `pdf-parser`：

```powershell
Copy-Item .env.example .env
# 必填：RAG_MYSQL_PASSWORD、MYSQL_ROOT_PASSWORD、RAG_STATE_ENCRYPTION_KEY
docker compose up -d

# 只用 Go 纯文本引擎（RAG_PDF_ENGINE=go）时，可以跳过 pdf-parser：
docker compose up -d mysql qdrant redis kafka
# 单独构建 / 启动 / 查看 PDF 解析微服务：
docker compose up -d --build pdf-parser
docker compose logs -f pdf-parser
```

MySQL 默认映射 `127.0.0.1:3307`（避免与本地 3306 冲突），Qdrant 为 `6333/6334`，Redis 为 `6379`，Kafka 宿主监听 `127.0.0.1:9092`（容器内 `kafka:9092`），pdf-parser 发布 `127.0.0.1:8091`（容器内固定 `8091`）。停止保留数据：`docker compose down`。

pdf-parser 首次构建只安装 Python/PyMuPDF 依赖，镜像很小，也无需预置任何模型。知识库需要在 Console「模型配置」页添加 VLM（服务地址 + 模型名 + 可选 API Key）并在知识库表单中选择，也可以配置 `RAG_VLM_BASE_URL` + `RAG_VLM_MODEL` 作为服务级回退；两者都没有时 `/v1/parse` 返回 503。宿主机直接运行的 Go 服务默认通过 `http://127.0.0.1:8091` 访问它；compose `app` 容器由 compose 注入 `http://pdf-parser:8091`。vision 引擎要求每个知识库在 Console 选中 VLM，缺失时 PDF 上传会在受理阶段返回中文提示（不进入队列）。

> Kafka 默认自动创建 topic（4 partitions、副本 1）；`scripts/start-stack.ps1` 仍会显式创建 document/search/cache 与三个 DLQ topic、等待 Kafka healthy、执行 `rag-state-migrate --apply`（可选导入 legacy 知识源），然后启动 `dist/bin/rag-service.exe`、`dist/bin/rag-document-worker.exe` 与 `dist/bin/rag-cache-worker.exe`（收到 Ctrl+C 时一并停止）；默认同时启动两个 Debezium 连接器（任务/缓存），并等待任务连接器输出 `Starting streaming`。用 `-MigrateOnly` 可只准备依赖与数据库。该脚本执行默认 `docker compose up -d`，因此也会启动 pdf-parser，并把 `.env` 中的 `RAG_PDF_ENGINE` 等变量加载进宿主进程；只用 Go 引擎时请在 `.env` 设 `RAG_PDF_ENGINE=go` 并自行停掉 pdf-parser。不配置 Kafka 时服务仍可启动（仅记录告警）并受理上传，但没有文档消费者，任务保持 `queued`；完整异步链路必须配置 Kafka + Debezium 并运行 `rag-document-worker`。

### 手动依赖

REST/Console 批次上传与 MCP 新增均异步受理，但都不在 `rag-service` 内解析：文档解析/索引只由独立的 `rag-document-worker` 消费 Kafka 通知完成。除状态库 + Qdrant 外还需要 Kafka（Debezium 投递文档通知）；无 Kafka 时服务仍能启动并受理上传，但任务保持 `queued`。

```powershell
docker run -d --name praxis-qdrant -p 6333:6333 -p 6334:6334 `
  -v praxis_qdrant_data:/qdrant/storage qdrant/qdrant:latest
```

状态库只有 MySQL 一种后端，本地开发也必须先把 MySQL 跑起来（`start-stack.ps1` 会拉起 broker 与数据库容器）；未配置 `RAG_STATE_URL` / `RAG_MYSQL_*` 时服务直接启动失败，不会回退到本地文件。请设置 `RAG_STATE_ENCRYPTION_KEY` 以便多实例解密模型 Key。

## 3. 配置

### config.toml

服务与 worker 启动时读取 `~/.praxis-rag/config.toml`（首次自动创建；`RAG_CONFIG_FILE` 可指定其他路径，指定的文件必须存在）。格式为单个 `[environment]` 节；**进程环境变量优先于文件值**，文件中的值会写入进程环境，因此 worker 也会共享同样的配置文件。

```toml
[environment]
RAG_BACKEND = "qdrant"
RAG_SERVICE_HOST = "127.0.0.1"
RAG_SERVICE_PORT = "8011"
RAG_SERVICE_DATA_ROOT = "~/.praxis-rag/data"
QDRANT_URL = "http://127.0.0.1:6333"
RAG_COLLECTION = "mini_agent_knowledge_pdf_assets_v1"
RAG_WORD_CONVERT_TIMEOUT = "120"
# Kafka（宿主机进程用 127.0.0.1:9092；compose 容器由 compose 注入 kafka:9092）
# RAG_KAFKA_BOOTSTRAP_SERVERS = "127.0.0.1:9092"
# RAG_KAFKA_CLIENT_ID = "praxis-rag"
# RAG_CACHE_TOPIC = "praxis_state_cache_refresh"
# RAG_CACHE_GROUP = "praxis_state_cache_refresh_workers"
# RAG_DOCUMENT_TOPIC = "praxis_document_ingest"
# RAG_DOCUMENT_DLQ_TOPIC = "praxis_document_ingest_dead_letters"
# RAG_CACHE_DLQ_TOPIC = "praxis_state_cache_refresh_dead_letters"
# RAG_DEBEZIUM_HEALTH_URL = "http://127.0.0.1:8083/q/health"
# RAG_DEBEZIUM_CACHE_HEALTH_URL = "http://127.0.0.1:8084/q/health"
# RAG_MYSQL_HOST = "127.0.0.1"
# RAG_MYSQL_PORT = "3306"
# RAG_MYSQL_USER = "rag"
# RAG_MYSQL_DATABASE = "praxis_rag"
# RAG_STATE_URL = "mysql+pymysql://rag:password@127.0.0.1:3306/praxis_rag?charset=utf8mb4"
```

凭据只放环境变量（`RAG_MYSQL_PASSWORD`、`RAG_STATE_ENCRYPTION_KEY`、`RAG_SERVICE_TOKEN`、`RAG_KAFKA_SASL_USERNAME/PASSWORD`、`RAG_KAFKA_TLS_CA`），不要写入配置文件。配置值必须是带引号的字符串；Windows 路径用 `/`。未知键或 TOML 语法错误会让进程明确失败，不会静默忽略。

### 服务入口

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_SERVICE_HOST` | `127.0.0.1` | 绑定地址；非 loopback 时必须提供 token 与 MCP 白名单 |
| `RAG_SERVICE_PORT` | `8011` | HTTP 端口 |
| `RAG_SERVICE_TOKEN` | 空 | Service Token；非 loopback 宿主强制要求，客户端用 `Authorization: Bearer` |
| `RAG_SERVICE_DATA_ROOT` | `~/.praxis-rag/data` | 上传恢复工作区根目录 |
| `RAG_WEB_ROOT` | 空（自动探测） | Console 构建目录；`dist/web` 或 `/app/web` |
| `RAG_MCP_ALLOWED_HOSTS` | `127.0.0.1,127.0.0.1:*,localhost,localhost:*,[::1],[::1]:*` | MCP Host 白名单（逗号分隔） |
| `RAG_MCP_ALLOWED_ORIGINS` | 对应 loopback `http` 来源 | MCP Origin 白名单 |
| `RAG_CONFIG_FILE` | `~/.praxis-rag/config.toml` | 启动配置文件路径 |

### 账号、角色与知识库共享

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_INVITATION_TTL_HOURS` | `168` | 知识库邀请链接有效期（小时，1..8760） |
| `RAG_DEFAULT_KB_TEMPLATE` | `default` | VIP/普通用户创建知识库时克隆模型连接（Embedding/Reranker/VLM）与向量维度的模板知识库；模板不存在时自建返回 409 |

角色分管理员（`admin`）、VIP（`vip`）和普通用户（`user`），只有管理员可以改角色。知识库默认归创建者私有，共享通过邀请链接完成：仅 VIP 与管理员可邀请（VIP 只能邀请自己创建的知识库，管理员可邀请任意知识库），权限分「可读 / 可修改」、范围分「单文件 / 多文件 / 全库」；被邀请人登录后接受邀请才生效。完整流程见 [权限管理](PERMISSIONS.md)。

### 检索、模型与解析配置

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_BACKEND` | `mock`（服务拒绝启动） | 必须为 `qdrant`；`mock` 仅用于 mcp-debug |
| `QDRANT_URL` | 未设置时 `http://localhost:6333` | Qdrant HTTP 地址 |
| `QDRANT_PATH` | 空 | **Go 不支持**；设置会启动失败（改用 `QDRANT_URL`） |
| `RAG_COLLECTION` | `mini_agent_knowledge_pdf_assets_v1` | 默认集合名；实际集合由知识库配置决定 |
| `RAG_DENSE_VECTOR_SIZE` | `1024` | 固定值，其他尺寸报错 |
| `RAG_EMBEDDING_MODEL` | `BAAI/bge-m3` | 默认模型标识（实际连接由知识库配置覆盖） |
| `RAG_EMBEDDING_DEVICE` | 空 | 传给兼容模型 API 的可选设备提示 |
| `RAG_CHUNK_TOKENIZER_MODEL` | `BAAI/bge-m3` | tokenizer 标识；Go 当前使用确定性近似 token 计数（见 §12） |
| `RAG_RERANKER_MODEL` | `BAAI/bge-reranker-base` | 默认 reranker 标识 |
| `RAG_RERANKER_DEVICE` / `RAG_RERANKER_BATCH_SIZE` | 空 / `4` | reranker 设备提示与批大小 |
| `RAG_RERANKER_MAX_CANDIDATES` | `30` | 进入重排的候选上限 |
| `RAG_RERANKER_MAX_LENGTH` | `512` | 重排文本 token 上限 |
| `RAG_RERANKER_MAX_TEXT_CHARS` | `4000` | 重排文本字符上限 |
| `RAG_PRELOAD_MODEL` / `RAG_PRELOAD_RERANKER` | `false` | Go 通过 HTTP 调用模型，不加载本地权重 |
| `RAG_NAMESPACE` | 空 | 可选的 Qdrant 物理 namespace 过滤 |
| `RAG_PDF_ENGINE` | 空（有 URL 则 vision，否则 go） | PDF 解析引擎：`go` 内置纯文本 / `vision` 调用 pdf-parser 微服务（远端视觉模型）；旧值 `docling` 作为兼容别名等价于 `vision`；空值按是否配置 `RAG_PDF_SERVICE_URL` 决定。compose `app` 容器默认 `vision` |
| `RAG_PDF_SERVICE_URL` | `http://127.0.0.1:8091` | pdf-parser 服务地址（绝对 `http(s)` URL，不带 query/fragment）；compose `app` 容器由 compose 注入 `http://pdf-parser:8091` |
| `RAG_PDF_SERVICE_TIMEOUT` | `900` 秒（1..7200） | Go 调用 pdf-parser 的解析超时；pdf-parser 同时把它作为单请求解析超时 |
| `RAG_PDF_SERVICE_PORT` | `8091`（仅 compose） | pdf-parser 的宿主发布端口；容器内端口固定 `8091` |
| `RAG_PDF_PARSER_IMAGE` | `praxis-rag-pdf-parser:local`（仅 compose） | pdf-parser 镜像名 |
| `RAG_PDF_MODELS_PATH` | 空 | **已废弃**：本地不再有模型，pdf-parser 不再读取该变量 |
| `RAG_PDF_SERVICE_WORKERS` | `1` | PDF 容器内启动的 Uvicorn 服务进程数（正整数）。由 `python -m app.main` 读取，同一端口接收请求；每个进程有独立的解析并发名额 |
| `RAG_PDF_SERVICE_CONCURRENCY` | `1`（仅 pdf-parser） | 每个 PDF 服务进程的并发解析数；PDF 总解析容量上限为 `RAG_PDF_SERVICE_WORKERS × 本值`。应与文档 worker 总任务并发一起调整，超出解析容量的请求会排队并消耗超时窗口；调大前先核对容器 CPU/内存 |
| `RAG_VLM_BASE_URL` | 空 | 远端 OpenAI 兼容视觉模型端点的**环境回退值**，用于描述 Word 无图注图片和 vision 引擎的 PDF 版面/OCR/表格/公式/图片描述；Console 知识库设置优先 |
| `RAG_VLM_API_KEY` | 空 | VLM 端点密钥，作为 `Authorization: Bearer` 发送 |
| `RAG_VLM_MODEL` | 空 | 端点上的模型名，如 `qwen2.5-vl-7b`（环境回退值） |
| `RAG_VLM_TIMEOUT` | `120` 秒（1..3600） | 单次 VLM 请求超时 |
| `RAG_VLM_CONCURRENCY` | `4`（1..64） | 并发 VLM 请求数 |
| `RAG_WORD_CONVERTER_PATH` | 空 | LibreOffice `soffice` 路径（仅 DOC 转换） |
| `RAG_WORD_CONVERT_TIMEOUT` | `120` 秒（1..7200） | DOC 转换超时 |

### PDF 解析引擎（vision / go）

`RAG_PDF_ENGINE` 决定 PDF 上传如何解析：

- `vision`（compose `app` 默认；旧值 `docling` 作为兼容别名等价）：Go 服务把 PDF 以 multipart `file` 发给 pdf-parser 的 `POST /v1/parse`，并附带知识库配置的 `options`（`{"vision":{"base_url","api_key","model","timeout","concurrency"}}`）；服务本地用 PyMuPDF 读取/渲染/裁图，版面/OCR/表格/公式/图片描述全部调用远端 OpenAI 兼容视觉模型，返回 `application/zip`（根目录 `manifest.json` + 图片资产，清单版本 `docling-service-v1`，该字符串是证据格式契约），Go 端校验状态码/content-type、安全解压后切分入库。**vision 引擎要求知识库已配置 VLM**（Console「模型配置」页添加，建库/编辑知识库时选择）：缺失时 PDF 上传在受理阶段返回
  `PDF 解析需要先在「模型配置」页添加视觉模型（VLM），并在知识库配置中选择后再上传。`，不进入 journal/队列。VLM 可用 `RAG_VLM_*` 环境变量作为回退（Console 知识库配置优先）。
- `go`：使用 Go 进程内纯文本引擎（按基线分行、按字号与行距分段、去页眉页脚与页码、合并连字符换行），适合文字型 PDF；不含版面分析、表格结构、OCR、公式与图片资产，扫描件会返回“没有可索引的内容”或空页告警，不会静默入库垃圾文本。不依赖 Python 或任何外部服务。
- 空值：配置了 `RAG_PDF_SERVICE_URL` 时按 `vision`，否则按 `go`。

部署与运维要点：

- **服务依赖**：`vision` 引擎强依赖 pdf-parser 可用性与远端视觉模型的连通性。服务未启动、连接失败或超过 `RAG_PDF_SERVICE_TIMEOUT`（默认 900 秒）时，该次 PDF 上传失败并返回安全错误（不泄露文档正文）；Markdown/TXT/DOCX 上传不受影响。需要临时降级时显式设置 `RAG_PDF_ENGINE=go`。
- **无本地模型/模型卷**：`pdf_parser_models` 卷与 `RAG_PDF_MODELS_PATH` 已移除，无需下载或预置任何模型；旧部署的模型卷可以直接删除。视觉模型未配置（知识库/请求/环境变量都没有）时，`/v1/parse` 返回 503，PDF 上传失败。
- **Console 模型要求（vision）**：先在「模型配置」页添加 VLM（服务地址 + 模型名 + 可选 API Key）条目，再在创建/编辑知识库时选中；缺失时 PDF 上传会被拒绝并提示配置。VLM 配置随每次解析请求下发（multipart `options`），因此同一 pdf-parser 可服务不同知识库（不同知识库可用不同视觉模型）。
- **端口与健康检查**：pdf-parser 容器内监听 `0.0.0.0:8091`，compose 只发布 `127.0.0.1:${RAG_PDF_SERVICE_PORT:-8091}`；健康检查是 `GET /healthz`，`POST /v1/parse` 不做认证，因此**不要暴露到公网**。修改宿主端口时要同步 `RAG_PDF_SERVICE_URL`（宿主 Go 进程默认 `http://127.0.0.1:8091`）。
- **并发与超时**：`RAG_PDF_SERVICE_WORKERS` 控制 PDF 服务进程数，`RAG_PDF_SERVICE_CONCURRENCY` 是每个进程内 `asyncio.Semaphore` 的许可数，两者代码默认值均为 `1`。按总解析容量（两者乘积）与文档 worker 总任务并发一起调整，并核对容器 CPU/内存与远端视觉模型限流；多个服务进程不共享信号量。`RAG_PDF_SERVICE_TIMEOUT` 同时是 Go 端调用超时和 pdf-parser 单请求解析超时，**排队等待也计入**（排满返回 503「PDF 解析服务繁忙」）；超大 PDF 会占用该窗口。
- **测试模式**：pdf-parser 的 `RAG_PDF_SERVICE_CONVERTER=fake` 只用于测试/联调（返回固定清单，不调用视觉模型），生产必须保持默认 `vision`。
- **`.doc` 不受影响**：旧版 `.doc` 仍由 Go 服务调用外部 LibreOffice（`RAG_WORD_CONVERTER_PATH`）；DOCX 解析仍在 Go 进程内完成。

**Word 与 VLM**：Word 无图注图片会调用远端 OpenAI 兼容视觉模型（`RAG_VLM_BASE_URL`/`RAG_VLM_API_KEY`/`RAG_VLM_MODEL`，或 Console 知识库设置），未配置时图片保留占位描述、上传不失败；DOC 转换仍需要外部 LibreOffice。

**VLM 优先取知识库配置**：Console 的知识库设置页在 Embedding/Reranker 之外提供 VLM 区块（从「模型配置」列表选择，可开关），保存后该知识库随后的上传与 worker 解析立即使用它。vision 引擎上传 PDF 时，Go 服务把 VLM（含加密存储的 Key）随请求发给 pdf-parser；VLM 未配置或关闭时回退到 `RAG_VLM_*` 环境变量。API Key 与其他 provider 一样加密存储，前端只显示“已保存”；缺失配置时上传会被拒绝并提示去「模型配置」页添加。升级时一次性迁移会删除旧 `kind='ocr'` 模型配置并清除知识库设置中的 `ocr`/`ocr_model` 键。

### 共享状态库

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_STATE_URL` | 空 | 显式 MySQL URL，优先级最高：`mysql+pymysql://...`；非 MySQL 的 scheme 会被拒绝 |
| `RAG_MYSQL_HOST` | 空 | 未设置 `RAG_STATE_URL` 时使用这组变量拼出 MySQL 连接 |
| `RAG_MYSQL_PORT` | `3306` | MySQL 端口 |
| `RAG_MYSQL_USER` / `RAG_MYSQL_PASSWORD` | `rag` / 空 | MySQL 账号；密码只从环境读取 |
| `RAG_MYSQL_DATABASE` | `praxis_rag` | 数据库名 |
| `RAG_MYSQL_SSL_CA` | 空 | 可选 CA PEM 路径 |
| `RAG_STATE_ENCRYPTION_KEY` | 空 | `base64:` 前缀的 32 字节密钥；加密模型 API Key，多实例必须一致 |
| `RAG_STATE_POOL_SIZE` / `RAG_STATE_MAX_OVERFLOW` | `10` / `20` | 连接池 |
| `RAG_STATE_POOL_RECYCLE` / `RAG_STATE_POOL_TIMEOUT` | `1800` / `30` 秒 | 连接回收与等待 |
| `RAG_STATE_ECHO` | `false` | SQL 回显（调试用，勿在生产开启） |
| `RAG_STATE_AUTO_SCHEMA` | `true` | 启动时创建缺失表；生产可关闭并由部署账号预建 |
| `RAG_UPLOAD_LEASE_SECONDS` | `120` | 上传事务业务租约时长 |
| `RAG_UPLOAD_CLEANUP_DELAY_SECONDS` | `3600` | 已完成上传工作区清理延迟 |

`RAG_SERVICE_DATA_ROOT` 只决定上传恢复工作区等本地文件的位置，不再决定数据库。两项都未配置时启动报错；表结构可用 §7 的 `rag-state-migrate` 检查/建表。

### Redis 与 artifact

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_REDIS_ENABLED` | 未设置 = 按 URL 自动启用 | `false/0/off` 显式禁用；禁用时只影响读模型展示缓存（读取回源 MySQL） |
| `RAG_REDIS_URL` | 空 | 例如 `redis://127.0.0.1:6379/0` |
| `RAG_REDIS_PREFIX` | `rag:state:v1` | 读模型缓存键前缀 |
| `RAG_REDIS_TTL_SECONDS` | `60` | 读模型展示缓存 TTL |
| `RAG_REDIS_TIMEOUT_SECONDS` | `0.3` | Redis 连接/命令超时 |
| `RAG_RESULT_TTL_SECONDS` | `3600` | **artifact 保留期**（变量名保留，语义已更正；不再用于搜索结果快照） |
| `RAG_RESULT_MAX_BYTES` | `16777216` | 单次搜索 artifact 累计字节上限 |
| `RAG_ARTIFACT_MAX_BYTES` | `67108864` | 单 artifact 上限 |
| `RAG_RESULT_MAX_ARTIFACTS` | `100` | 每次搜索最多 artifact 数 |

Redis 不可用时，持久读模型会回源状态库；搜索为同步直返，响应不写 Redis，artifact 以 MySQL `artifact_snapshots` 为事实来源并按 ID 独立读取，**清空 Redis 不影响搜索结果或已签发 artifact**。`RAG_RESULT_TTL_SECONDS` 到期后 MySQL artifact 被有界清理，再读取返回 410。

### Kafka 与队列

单节点 Compose Kafka 使用 INTERNAL/EXTERNAL 双 listener：宿主机进程用 `127.0.0.1:9092`，同一 compose 网络的容器用 `kafka:9092`；`RAG_KAFKA_BOOTSTRAP_SERVERS` 为空表示 brokerless：异步受理仍可提交，但没有 Kafka 就没有消费者，文档任务保持 `queued`、缓存刷新不生效（服务仍可启动，监控显示 Kafka 未启用）。搜索为同步执行，不受 Kafka 是否配置影响。两类事件各自独立 topic 与消费组，由两个 Debezium 连接器从 MySQL 表级采集后投递（不写事务 outbox）。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_KAFKA_BOOTSTRAP_SERVERS` | 空 | 逗号分隔 `host:port`；宿主默认 `127.0.0.1:9092`，compose 容器 `kafka:9092`；空 = brokerless（文档任务保持 `queued`，处理文档必须配置；搜索为同步执行，不受影响） |
| `RAG_KAFKA_CLIENT_ID` | `praxis-rag` | Kafka 客户端标识 |
| `RAG_KAFKA_SECURITY_PROTOCOL` | `PLAINTEXT` | `PLAINTEXT` / `SSL` / `SASL_PLAINTEXT` / `SASL_SSL` |
| `RAG_KAFKA_SASL_MECHANISM` | 空 | `PLAIN` / `SCRAM-SHA-256` / `SCRAM-SHA-512`（仅 SASL 协议使用） |
| `RAG_KAFKA_SASL_USERNAME` / `RAG_KAFKA_SASL_PASSWORD` | 空 | SASL 凭据，仅从环境读取 |
| `RAG_KAFKA_TLS_CA` | 空 | 可选 CA PEM 路径 |
| `RAG_KAFKA_TLS_INSECURE` | `false` | 跳过 TLS 证书校验（仅测试） |
| `RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS` | `10` | 发布超时 |
| `RAG_CACHE_TOPIC` / `RAG_CACHE_GROUP` | `praxis_state_cache_refresh` / `praxis_state_cache_refresh_workers` | 缓存变更 topic/group（缓存连接器 → `rag-cache-worker`） |
| `RAG_DOCUMENT_TOPIC` / `RAG_DOCUMENT_GROUP` | `praxis_document_ingest` / `praxis_document_ingest_workers` | 文档任务 topic/group（任务连接器 → `rag-document-worker`） |
| `RAG_DOCUMENT_DLQ_TOPIC` | `praxis_document_ingest_dead_letters` | 文档死信 topic（显式配置，不是 `%DLQ%` 约定） |
| `RAG_CACHE_DLQ_TOPIC` | `praxis_state_cache_refresh_dead_letters` | 缓存死信 topic（显式配置） |
| `RAG_DOCUMENT_DLQ_GROUP` / `RAG_CACHE_DLQ_GROUP` | `praxis_document_dead_letters` / `praxis_state_cache_refresh_dead_letters` | 死信 worker 的独立 group |
| ~~`RAG_SEARCH_TOPIC` / `RAG_SEARCH_GROUP` / `RAG_SEARCH_DLQ_TOPIC` / `RAG_SEARCH_DLQ_GROUP`~~ | — | **已随排队搜索下线**：配置保留为已废弃、启动时忽略且不生效 |
| `RAG_APP_KAFKA_BOOTSTRAP_SERVERS` | 空（compose 内默认 `kafka:9092`） | 仅 compose app/Debezium 容器读取的 bootstrap 覆盖；宿主 Go 进程读 `RAG_KAFKA_BOOTSTRAP_SERVERS` |

**消费语义（至少一次）**：

- **只有 MySQL 已记录领取、安全跳过或终态后才提交 Kafka offset**；DB 不可用/写失败返回 nack；
- handler 失败（nack）→ 在**同一进程内**重试同一条记录，指数退避从 `100ms` 开始、上限 `5s`，`DeliveryAttempt` 从 1 递增；
- 达到 `MaxAttempts`（document worker 默认 5）→ 把原始消息发布到对应 `RAG_*_DLQ_TOPIC`、执行终态回调、再提交 offset；
- `MaxAttempts=0` → 不设上限，一直重试直到进程或 context 取消；
- 死信 worker 的 `--kind document|cache` 实例消费显式 DLQ topic 与独立 `RAG_*_DLQ_GROUP`，不再使用 `%DLQ%<源组>` 或 mqadmin；`MaxAttempts=0` 的死信 worker 自身不会二次死信。原 `--kind search` 分支已随排队搜索删除。
- 任务通知只处理与数据库当前 `schedule_version` 相等的记录；版本不符、任务已终态、租约仍有效一律安全跳过并 ACK（任务类事件）。缓存事件按表→key 映射刷新，Redis 写失败不 nack。
- 消费重试与死信发布由 Go 消费端完成，不依赖 Kafka 的不可见期/重投机制；重复投递由 handler 幂等与 MySQL 业务租约吸收。
- 每个 worker 可并发处理多条消息（`RAG_DOCUMENT_WORKER_CONCURRENCY`，默认 `1`）：拉取端按 `BatchSize`（默认 16）预取到本地队列，多个 goroutine 并发执行 handler。并发时仍按分区提交 offset：只提交"连续完成的前缀"，慢任务不会被后一条提交跳过；进程退出时未提交的记录会重投，由幂等处理吸收。

### 异步文档与 worker 环境

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_DOCUMENT_JOB_LEASE_SECONDS` | `300` | 文档任务 SQL 租约 |
| `RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS` | `20` | 处理期间续租间隔 |
| `RAG_DOCUMENT_JOB_SWEEP_SECONDS` | `120` | 滞留/过期任务回收窗口 |
| `RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS` | `15` | 文档 Worker 写入 MySQL `runtime_heartbeats` 的运行心跳间隔（秒，可小数）；监控页据此判断 Worker 是否真的在运行，超过 60 秒未更新显示降级。非法值回退默认 |
| `RAG_DEBEZIUM_HEALTH_URL` | 空（未知） | 任务连接器的只读健康探测地址（宿主机进程示例 `http://127.0.0.1:8083/q/health`）。只反映连接器存活，binlog 采集位点应用侧读不到，监控显示「未知」 |
| `RAG_DEBEZIUM_CACHE_HEALTH_URL` | 空（未知） | 缓存连接器的只读健康探测地址（宿主机进程示例 `http://127.0.0.1:8084/q/health`） |
| `RAG_DEBEZIUM_PORT` / `RAG_DEBEZIUM_CACHE_PORT` | `8083` / `8084` | Compose 把两个 Debezium 健康端口 8080 分别发布到宿主机 loopback 的端口 |
| `RAG_APP_DEBEZIUM_HEALTH_URL` / `RAG_APP_DEBEZIUM_CACHE_HEALTH_URL` | `http://debezium:8080/q/health` / `http://debezium-cache:8080/q/health` | Compose `app` profile 内部覆盖用的两个连接器健康地址 |
| `RAG_CACHE_WORKER_RECOVER_SECONDS` | `30` | `rag-cache-worker` 周期 `RefreshDirty` 补偿间隔（Redis 写失败/脏 key 收敛） |
| `RAG_DOCUMENT_JOB_RETENTION_SECONDS` | `2592000`（30 天） | 终态任务保留期 |
| `RAG_DOCUMENT_WORKER_RECOVER_SECONDS` | `30` | 文档 worker 周期性 Recover（恢复中断上传/补偿） |
| `RAG_DOCUMENT_WORKERS` | `1` | `scripts/start-stack.ps1` 启动的 `rag-document-worker` 进程数（Kafka 同组分摊 `RAG_DOCUMENT_TOPIC` 分区，能分配到分区的进程数受分区数限制；文档任务总并发为有效进程数 × `RAG_DOCUMENT_WORKER_CONCURRENCY`，PDF 解析并发还受 pdf-parser 限制） |
| `RAG_DOCUMENT_BROKER_MAX_RETRIES` / `RAG_DOCUMENT_MAX_ATTEMPTS` | `5` | 文档 worker 的 Kafka 进程内最大尝试次数（前者优先；名称为兼容旧配置保留） |
| `RAG_DOCUMENT_WORKER_CONCURRENCY` | `1` | 文档 worker 并发处理数（1..64）。`>1` 时同一 worker 可同时解析多篇 PDF；解析段的有效并行度受 pdf-parser 的 `RAG_PDF_SERVICE_CONCURRENCY` 封顶，两侧应同量级调整，并核对 pdf-parser 内存/CPU 与本地解压磁盘峰值 |

对于 16 个逻辑处理器、约 32 GiB 内存的本机部署，`.env.example` 提供以下起始配置（通过 `scripts/start-stack.ps1` 启动，使用单个 pdf-parser 进程）：

```dotenv
RAG_DOCUMENT_WORKERS=1
RAG_DOCUMENT_WORKER_CONCURRENCY=4
RAG_PDF_SERVICE_WORKERS=1
RAG_PDF_SERVICE_CONCURRENCY=4
```

`RAG_DOCUMENT_WORKERS` 只控制启动脚本创建的进程数；`RAG_DOCUMENT_WORKER_CONCURRENCY` 控制 Kafka 模式下每个文档 worker 的任务处理 goroutine 数。上述配置用一个进程中的 4 个处理协程同时推进最多 4 份文档。PDF 服务的 `RAG_PDF_SERVICE_WORKERS` 控制服务进程数，`RAG_PDF_SERVICE_CONCURRENCY` 控制每个进程中 `asyncio.Semaphore` 的许可数，每份转换在线程池中执行，因此后者是进程内并发转换数，不是 Python 线程数或协程总数。

PDF 总解析容量上限为 `RAG_PDF_SERVICE_WORKERS × RAG_PDF_SERVICE_CONCURRENCY`，文档任务总并发为有效文档 worker 进程数 × `RAG_DOCUMENT_WORKER_CONCURRENCY`。例如 PDF 端也可设为 2 个进程、每进程并发 2，总解析容量上限仍为 4，但多进程不会共享模型内存。所有 PDF 服务进程共用容器的 8091 端口，由 Uvicorn 共享监听 socket 接收连接，无需配置多组 URL；实际负载分布受连接复用和任务耗时影响。使用自定义 Uvicorn CLI 启动时，应显式传入 `--workers`，该 CLI 不读取项目自定义的进程数变量。

该值是未经过真实 PDF 压测的起点；实际容量取决于容器可用资源和文档复杂度。未设置环境变量时，代码中的两项并发默认值仍为 1；示例配置显式设置为 4。修改 `.env` 后，需重新运行启动脚本以重启文档 worker，并由 Compose 更新 pdf-parser 容器，已有进程不会热加载这些值。

### 日志与请求关联

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_LOG_LEVEL` | `info` | Go 服务与 3 个 worker 的统一日志级别：`debug`/`info`/`warning`/`error`（`warn` 同义）；未设置时回退 `RAG_DEBUG_TRACE` |
| `RAG_LOG_FORMAT` | `text` | `text`（默认，单行键值）或 `json`（每行一个 JSON 对象，便于采集）|
| `RAG_DEBUG_TRACE` | `false` | 兼容旧调试开关；为真且未设置 `RAG_LOG_LEVEL` 时日志为 `debug`，同时控制模型 API 的调试输出 |
| `RAG_PDF_SERVICE_LOG_LEVEL` | `info` | pdf-parser（Python）日志级别：`debug`/`info`/`warning`/`error` |
| `RAG_PDF_SERVICE_LOG_FORMAT` | `text` | pdf-parser 日志编码：`text`/`json` |

- **一次上传从请求追到任务**：`rag-service` 为每个 HTTP 请求接受或生成 `X-Request-ID`（响应头回写）；上传日志携带 `operation_id`/`artifact_id`/`batch_id`，`rag-document-worker` 的日志携带 `artifact_id`/`operation_id`/`attempt`；上传路径调用 pdf-parser 时也会转发同一个 `X-Request-ID`，两边日志可按该 ID 关联。
- **失败原因可诊断**：文档任务失败时 `document_jobs.error` 写入最近一次失败摘要（`<类别> at <阶段> (HTTP <状态>): <脱敏详情>`）；重试耗尽的终态提示为 `Message retry limit reached; last error: ...`，死信日志同样保留该摘要。
- **统一脱敏**：日志只记录任务/操作 ID、失败阶段与安全错误类别，不记录查询、文件正文、消息正文与凭据；错误详情经 `enc:v1:`、URL 用户信息、Bearer/API Key 与查询参数脱敏后截断。

### 搜索执行环境

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_SEARCH_TIMEOUT_SECONDS` | `120` | **同步检索总预算**（检索 + 重排；已有更早 deadline 不延长），超时映射为既有搜索错误 |
| ~~`RAG_SEARCH_MAX_ATTEMPTS`~~ / ~~`RAG_SEARCH_WORKER_CONCURRENCY`~~ / ~~`RAG_SEARCH_LEASE_SECONDS`~~ / ~~`RAG_SEARCH_RETRY_SECONDS`~~ | — | **已随排队搜索下线**：配置保留为废弃键，启动时忽略且不生效 |

搜索在 `rag-service` 进程内同步执行；客户端断开或请求取消会向下传递到 Qdrant 与模型调用。artifact 按 ID 独立读取，保留期由 `RAG_RESULT_TTL_SECONDS` 控制（见 [产物引用](result-references.md)）。

### Worker 二进制与运行模式

| 命令 | 作用 | 必需 | 退出码 |
| --- | --- | --- | --- |
| `rag-service` | REST + MCP + Console + 后台维护循环 + 同步搜索；只受理上传，不解析文档 | Qdrant、状态库；MCP 需要 token | `0` 正常停止，`1` 运行时失败 |
| `rag-document-worker` | 唯一的文档消费者：消费 Kafka 文档通知，解析/切分/Embedding/发布 | **必需**（异步上传默认开启）；需要 `RAG_KAFKA_BOOTSTRAP_SERVERS`、状态库、Qdrant | `0/1/2` |
| `rag-cache-worker` | 消费缓存变更事件（schema 1），递增 revision 并重建 Redis；周期 `RefreshDirty` 补偿 | `RAG_KAFKA_BOOTSTRAP_SERVERS`、状态库、Redis（可选，禁用时只维护水位） | `0/1/2` |
| `rag-dead-letter-worker --kind document\|cache` | 消费显式 `RAG_*_DLQ_TOPIC`，把可确认的失败写入持久终态并 ACK | `RAG_KAFKA_BOOTSTRAP_SERVERS`、对应 `RAG_*_DLQ_GROUP` | `0/1/2` |

退出码约定（worker 与全部 CLI）：`0` = 收到 SIGINT/SIGTERM 正常停止；`1` = 运行时失败（应重启/告警）；`2` = 配置或用法错误（修配置，不要自动重启）。服务本身配置错误也以 `1` 退出。文档 worker 只有 Kafka 消费模式：必须配置 `RAG_KAFKA_BOOTSTRAP_SERVERS`，消费 Debezium 投递的 `RAG_DOCUMENT_TOPIC`；它始终是独立进程，`rag-service` 只负责受理、维护循环与 API，不解析文档。

## 4. 启动与冒烟

### 启动 rag-service

```powershell
$env:RAG_SERVICE_HOST = "127.0.0.1"
$env:RAG_SERVICE_PORT = "8011"
$env:QDRANT_URL = "http://127.0.0.1:6333"
$env:RAG_WEB_ROOT = "$PWD/dist/web"     # 若 Console 已构建
.\dist\bin\rag-service.exe
```

默认地址：Console `/console`、MCP `/mcp`、API 文档 `/docs`（`/openapi.json`）、健康检查 `/health`。

### 创建 API Key

API Key 管理接口接受两种调用方式：本机直连（loopback、无代理头、同源 Origin），或任意已登录账号的 Console 会话（Cookie + `X-CSRF-Token`，或 Bearer 会话 Token）。密钥归创建它的账号所有，规则见[权限管理](PERMISSIONS.md) §9。

```powershell
# 本机直连：创建无主密钥（owner 为空，之后仅本机管理或管理员可见）
curl.exe -X POST http://127.0.0.1:8011/v1/admin/api-keys `
  -H "Content-Type: application/json" `
  -d '{"name":"cli","namespaces":["default"]}'

# 登录会话：创建归当前账号所有的密钥；非管理员只能使用自己拥有的知识库
curl.exe -X POST http://127.0.0.1:8011/v1/admin/api-keys `
  -H "Content-Type: application/json" -b cookies.txt `
  -H "X-CSRF-Token: <会话 CSRF Token>" `
  -d '{"name":"cli","namespaces":["my-kb"]}'
```

响应中的 `key` 只出现一次；后续请求带 `Authorization: Bearer <key>` 和 `X-RAG-Namespace: <知识库>`。也可直接使用 `RAG_SERVICE_TOKEN`（本地链接可用 Service Token 代替 API Key）。非管理员绑定不是本人创建的知识库会返回 403 `只能为本人创建的知识库创建 API Key`。

### 用户登录与注册（Console）

Console 默认启用账号登录（`RAG_AUTH_ENABLED=true`）：

1. 打开 `/console` 会进入登录页；切到"注册"创建账号，**第一个注册的账号自动成为管理员**。用户名不区分大小写（统一按小写保存）。
2. 管理员在侧边栏"用户管理"页可以：把账号设为管理员 / VIP / 普通用户、启用/禁用、删除。系统始终保留至少一名启用状态的管理员（最后一个管理员不能被降级、禁用或删除）。
3. 普通用户只能使用自己创建的知识库以及被邀请授权的知识库；管理员可以使用全部知识库并管理知识库配置、全部 API Key 和用户；VIP 额外可以邀请他人共享自己创建的知识库。所有登录角色都能打开「API Key」页管理自己的密钥，但非管理员只能绑定本人创建的知识库（详见[权限管理](PERMISSIONS.md) §9）。
4. 共享入口在侧边栏「权限管理」（管理员与 VIP 可见）：选择知识库 → 邀请用户（权限：可读/可修改；范围：全库 / 若干文件）→ 复制邀请链接发给对方。被邀请人会在控制台通知铃铛看到待处理邀请，登录后打开链接即可接受；接受后权限立即生效。详见 [权限管理](PERMISSIONS.md)。
5. 知识库与 API Key 页面支持批量删除：勾选行首复选框，或按住鼠标左键在表格上拖出框选。框选只增不减：拖动或缩小框选区域、开始新一轮框选、上下滚动页面都不会取消已选中的行；要取消某一行请点它的行首复选框（Esc 只取消本次拖动并恢复拖动前的选中）。工具栏「批量删除」按钮会显示已选数量。知识库批量删除会删除所选知识库的全部文档并移除配置记录，**删除后无法恢复**；只要有一个文档删除失败，该知识库就保留并列出失败原因，可重试。API Key 批量删除对已不存在的 id 视为已删除，失败项保留选中。

会话使用 HttpOnly Cookie（`rag_session`）保存，默认有效期 7 天（`RAG_AUTH_SESSION_TTL_HOURS`），所有写操作要求 `X-CSRF-Token`（Console 自动带上）。判定规则：

| 变量 | 默认 | 作用 |
| --- | --- | --- |
| `RAG_AUTH_ENABLED` | `true` | 设为 `false` 关闭登录，Console 回到旧的本机/Token 模式 |
| `RAG_AUTH_REGISTRATION` | `open` | `closed` 关闭自助注册（仍需先存在管理员） |
| `RAG_AUTH_SESSION_TTL_HOURS` | `168` | 会话有效期（小时） |
| `RAG_AUTH_COOKIE_SECURE` | `false` | HTTPS 部署时设为 `true` |

管理员会话可以让远程浏览器访问 `/v1/admin/*`（此前只允许本机）；自动化脚本继续使用 `RAG_SERVICE_TOKEN` 或 API Key，不受影响。

**邮箱验证与个人信息（Console）**：管理员在侧边栏「系统设置」页配置 SMTP 邮件服务器（主机、端口、TLS 模式、用户名、密码、发件人）并可以发送测试邮件，密码与其他 provider 凭据一样按 `enc:v1:` 加密保存，接口只返回 `password_set` 标记（Console 显示为"已保存"）。启用邮件服务器后：

1. 注册页要求填写邮箱并点击「发送验证码」，验证码 10 分钟内有效、最多尝试 5 次、同一邮箱 60 秒内不可重发；验证通过后账号邮箱标记为已验证。未配置/未启用邮件服务器时注册仍按用户名+密码进行（向后兼容），邮箱可在个人信息中补充但不会标记为已验证。
2. 登录后点右上角账号菜单 →「个人信息」：上传/移除头像（PNG/JPEG/WEBP/GIF，≤2MB）、设置年龄（1-150）与性别（男/女/其他/未设置）、修改用户名；「修改邮箱」「修改密码」为两个独立页签，分别修改邮箱（需当前密码 + 新邮箱验证码）与修改密码（需当前密码，至少 8 位）。
3. 对应接口：`POST /v1/auth/register/code`、`PATCH /v1/auth/profile`、`POST /v1/auth/password`、`POST /v1/auth/email/change/code`、`POST /v1/auth/email/change`、`PUT/GET/DELETE /v1/auth/avatar`、管理员的 `GET/PUT /v1/admin/mail-settings` 与 `POST /v1/admin/mail-settings/test`。`GET /v1/auth/status` 的 `email_verification_enabled` 表示注册是否需要邮箱验证码。

### 启动 worker

```powershell
# 必需：消费默认异步上传与 MCP add_document 的 Kafka 通知
# （由 Debezium 任务连接器采集 document_jobs 投递；未运行时任务保持 queued）
.\dist\bin\rag-document-worker.exe

# 缓存变更消费者（Debezium 缓存连接器投递）
.\dist\bin\rag-cache-worker.exe

# 死信处理（两个独立实例）
.\dist\bin\rag-dead-letter-worker.exe --kind document
.\dist\bin\rag-dead-letter-worker.exe --kind cache
```

worker 必须与 `rag-service` 使用**同一** `RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`RAG_BACKEND=qdrant`/`QDRANT_URL`、`RAG_STATE_ENCRYPTION_KEY`、Redis/队列设置。它们不继承其他终端的 `.env`（PowerShell 中请显式设置或使用进程管理器）。

### 冒烟检查

1. `GET /health` 返回 `status=ok`，`backend=qdrant`，`state` 显示 MySQL 后端。
2. `/console` 返回 Console 页面（构建了 `dist/web` 且 `RAG_WEB_ROOT` 正确时）。
3. REST `/v1/documents` 需要认证，未认证返回 401。
4. MCP 初始化成功，`tools/list` 返回 5 个工具。
5. 异步链路：默认异步上传与 MCP `add_document` 得到 `artifact_id`；配置 Kafka/Debezium 并启动 document worker 后，轮询 `get_document_status` 至 `succeeded`；未运行 worker 时任务保持 `queued`。
6. 同步搜索：MCP `search` 与 `POST /v1/search` 一次调用直接返回 `results`/`artifacts`/`evidence_status`；响应无 `result_id`，`GET /v1/search-results/<uuid>` 返回 404；受限账号检索按 scope 过滤。
7. 缓存收敛：直接 `UPDATE`/`DELETE` 一张缓存相关业务表，等待 `rag-cache-worker` 消费后 Redis 读模型与 MySQL 一致；监控页缓存 dirty/failures 归零。
8. PDF 解析（vision 引擎）：`curl.exe http://127.0.0.1:8091/healthz` 返回 200 且 `status=ok`，在 Console 为测试知识库选择 VLM 后上传一个 PDF，任务 `succeeded` 且该文档的 PDF 清单版本为 `docling-service-v1`；`RAG_PDF_ENGINE=go` 时清单版本为 `go-native-v1`。

## 5. REST API 示例

### 认证与命名空间

- 普通 `/v1/*`、`/mcp`：`Authorization: Bearer <RAG_SERVICE_TOKEN 或 API Key>`；未设置 Token 时，本机 loopback 可直接使用 Headers 的 `X-RAG-Namespace` 访问普通租户接口。Console 登录后也接受会话 Cookie（或 Bearer 会话 Token）。
- `/v1/admin/*`：本机直连（loopback/无 Forwarded 代理头/同源 Origin）**或**登录管理员的会话（Cookie 写操作需 `X-CSRF-Token`）。**例外**：`/v1/admin/api-keys` 的五条管理路由接受任意角色的登录会话，但只作用于该会话自己的密钥（见[权限管理](PERMISSIONS.md) §9）。
- 所有租户接口都需要 `X-RAG-Namespace: <知识库>`；MCP 否则无法确定工作区。

```powershell
$headers = @{
  "Authorization"   = "Bearer <token-or-key>"
  "X-RAG-Namespace" = "default"
  "Content-Type"    = "application/json"
}
```

### 健康与 OpenAPI

```powershell
curl.exe http://127.0.0.1:8011/health
curl.exe http://127.0.0.1:8011/openapi.json   # OpenAPI 3.1
curl.exe http://127.0.0.1:8011/docs           # 自包含交互文档
```

### 文档上传与批次进度

文档导入有两个入口：REST 批次上传（单批 ≤10 个文件、单文件 ≤10 MiB）和 MCP 单文件新增。两者都将每个文件的完整任务（`document_jobs`）写入同一个事务，提交即受理：

| 入口 | 行为 |
| --- | --- |
| `POST /v1/document-batches` | **批次受理**：`202` 返回 `batch_id` 与每个文件的结果（`accepted` / `skipped` / `failed`），供 Console 显示进度；每文件独立事务并行提交，不同文件名之间不互相排队（同名仍串行，保证查重与插入的原子性） |
| MCP `add_document` | 单文件异步受理，只返回 `artifact_id` |

切块完成后，同一文档的 Embedding 与 BM25 稀疏向量计算并发执行，两步都成功才写入 Qdrant。任一步失败会取消另一侧的计算，并等待其退出后清理上传；BM25 在 chunk 之间检查取消信号。Embedding 仍每批最多发送 32 个 chunk，同一文档内各批请求按顺序执行。

`POST /v1/document-batches` 的响应结构（Console 气泡直接消费）：

```json
{
  "batch_id": "8f0c…",
  "status": "processing",
  "total": 3, "accepted": 1, "skipped": 1, "failed": 1,
  "items": [
    {"position": 0, "name": "a.pdf", "status": "accepted", "reason": null,
     "artifact_id": "1feb…", "error": null,
     "job": {"status": "processing", "stage": "parsing", "error": null}},
    {"position": 1, "name": "b.pdf", "status": "skipped", "reason": "exists"},
    {"position": 2, "name": "c.pdf", "status": "failed", "reason": "config", "error": "…"}
  ]
}
```

- `skipped.reason`：`exists`（已发布同名文档）、`queued`（同名任务在队列）、`duplicate`（本批重复）。
- `failed.reason`：`validation`（名称/类型/大小）、`config`（知识库缺 VLM 等）、`conflict`、`network`（仅前端产生）、`internal`。
- 同一个 `Idempotency-Key` 重发返回原批次，不会重复入队。
- `GET /v1/document-batches/{batch_id}` 返回同一结构，`items[].job` 给出解析阶段（`parsing` → `chunking` → `staging` → `embedding` → `indexing` → `publishing`），终态为 `succeeded` / `failed`。
- `GET /v1/document-batches?limit=20` 只返回调用方未完成的批次，Console 刷新/切页后据此恢复进度；批次只保存进度元数据（不保存文件字节），默认保留 7 天后清理。
- 批次状态由批次行和对应 `document_jobs` 实时推导：`saving`（受理中）→ `processing` → `completed`（含全部跳过）/ `failed`（有失败且没有任何文件入队）；`interrupted` 表示服务在受理中途重启（文件未入队，可重试）。
- Console 行为：点"上传并索引"后弹窗立即关闭，按钮缩成"保存中 x/y / 处理中 x/y / 已完成 x/y"的小徽标，点开是逐文件进度气泡；失败项可单独重试。上传期间**允许切换 Console 页面**（工作区切换仍被锁住，因为批次绑定知识库）。

### 系统监控（Console 页面与只读接口）

管理员（或本机直连）可在 Console 侧边栏打开「系统监控」（`#/monitor`），页面每 15 秒自动刷新，全部为只读数据。版式按重要性排列：页头总体状态 → 关键指标 → 依赖与投递 → 队列与记录 → 服务信息；窄屏自动单列。

- **页头总体状态**：由状态库聚合与依赖探测共同推导的三态 `status`（`ok`/`degraded`/`error`，`ok` 为兼容字段，等于 `status == "ok"`）。任一依赖异常或状态库不可读 → `error`；任一依赖/Qdrant 目标集合/文档 Worker 心跳/Debezium 运行检查降级，或 Kafka 消费进度读取失败 → `degraded`；未启用或未知不降级。`pipeline.document_jobs` 只反映任务负载，不参与总体状态。Qdrant 已降级时页头不再显示「运行正常」。
- **关键指标**：有效文档数（只统计 `del_flag = 0`，不含软删除）、排队/处理中文档任务、上传进行中、近 24 小时检索次数、artifact 快照数（结果快照指标已随搜索同步化移除）。
- **依赖探测**：MySQL（含连接池 in_use/wait）、Redis（PING）、Qdrant（集合数与目标集合点数）、Kafka（broker 元数据：连通性、TLS/SASL 认证、目标主题是否存在与分区数，不再只测 TCP 端口）、pdf-service（`/healthz` 解析转换器状态、版本、配置错误，不再只看 HTTP 状态码）；未启用的依赖标记为「未启用」，未配置 Kafka 时显示「未配置 Kafka：…」。
- **文档任务与队列**：`document_jobs` 按状态/阶段计数、最老排队时长、租约过期的卡死任务数、24 小时失败数；上传操作拆成互斥分类：进行中（`INTENT`+`RUNNING`）、失败（`FAILED`+`ROLLBACK_FAILED`）、回滚中（`ROLLING_BACK`）、待清理（`cleanup_pending`），并保留并集 `unfinished`。页面注明「未完成操作」是进行中、失败待处理与待清理的合计，不代表当前正在上传或入库的文件数。
- **Debezium 两个连接器**：任务连接器与缓存连接器分别探测 `/q/health` 的 `checks[]`，失败项点名；两个连接器的 binlog 采集位点都显示「未知」（应用侧读不到）。
- **Kafka 两组积压**：文档、缓存两个消费组各自的 topic/group、已提交位点、末尾位点与滞后（读不到/未配置显示「未知/未启用」）；聚合状态按两组综合。
- **缓存水位**：脏 key 数（`refreshed_revision < revision`）与刷新失败数（`last_error` 非空）；统计不可用时为 `null`（未知）。
- **资料库与活动**：有效文档数（排除软删除）、知识库数、artifact 快照数；最近 24 小时入库成功/失败与检索次数。检索次数由活动计数器记录**每次**同步检索调用（失败也计入）。

页面对应的只读接口是 `GET /v1/admin/monitor/overview`（仅本机直连或管理员会话，响应 `Cache-Control: no-store`）。依赖降级时接口仍返回 200，用顶层 `status` 与 `dependencies[].status`（`ok`/`degraded`/`error`/`disabled`/`unknown`）区分；`queues.uploads` 同时提供 `by_status` 与 `in_progress`/`failed`/`rolling_back`/`cleanup_pending`；`pipeline.debezium` 为 `{tasks, cache}` 两个连接器（各含 `status/detail/capture_progress`），`pipeline.kafka.groups` 为 `document`/`cache` 两组位点，`pipeline.cache` 为 `{status, dirty_keys, refresh_failures}`，`service.delivery` 固定为 `debezium`；`library` 只含有效文档/知识库/artifact 快照计数（原 `result_snapshots` 字段已移除）。状态库不可读时 `queues`/`library`/`activity` 为 `null`、`pipeline.cache.dirty_keys/refresh_failures` 为 `null`，`pipeline.document_jobs.status` 为 `unknown`，`pipeline.debezium`/`pipeline.kafka`/`pipeline.worker` 仍各自探测，并附带 `errors` 中文说明。数字 `null` 表示未知，不要当作 0。该接口只做聚合查询，不读取任务/文档正文，也不记录查询内容：

```powershell
curl.exe http://127.0.0.1:8011/v1/admin/monitor/overview
```

### API Key 管理（本机或登录会话）

```powershell
curl.exe http://127.0.0.1:8011/v1/admin/api-keys
curl.exe -X POST http://127.0.0.1:8011/v1/admin/api-keys `
  -H "Content-Type: application/json" `
  -d '{"name":"cli","namespaces":["default","product-manuals"]}'
curl.exe -X PUT http://127.0.0.1:8011/v1/admin/api-keys/<id> `
  -H "Content-Type: application/json" `
  -d '{"name":"cli","namespaces":["default"],"enabled":false}'
curl.exe -X DELETE http://127.0.0.1:8011/v1/admin/api-keys/<id>
curl.exe "http://127.0.0.1:8011/v1/admin/api-keys/<id>/mcp-config" `
  -H "X-RAG-Namespace: default"

# 批量删除（Console「批量删除」走这条接口；1..100 个 id，重复 id 自动去重）
curl.exe -X POST http://127.0.0.1:8011/v1/admin/api-keys/batch-delete `
  -H "Content-Type: application/json" `
  -d '{"ids":["<id-a>","<id-b>","<id-missing>"]}'
```

`batch-delete` 在一个事务里完成整批删除：1 条 `SELECT ... WHERE id IN (...)` 分类（不存在或无权访问的 id 进入 `not_found`，与单条删除同语义、不视为错误），1 条 `DELETE ... WHERE id IN (...)` 删除，整批只提交 1 次、缓存修订与事件也只写 1 次。返回 `{"deleted":["<id>"],"not_found":["<id>"],"failed":[],"deleted_count":N}`：`failed` 恒为空数组（保留字段以兼容旧客户端）；整批数据库错误返回 500 `API Key 删除失败` 并整批回滚，可重试。请求体校验失败（缺失、空数组、空字符串、超过 100 项、未知字段）返回 FastAPI 形状的 422。

本机直连可以看到并管理全部密钥（含历史无主密钥）。改用登录会话（Cookie + 写操作 `X-CSRF-Token`）调用时按账号过滤：管理员看到全部密钥，响应条目带 `owner_user_id` / `owner_username`（历史密钥不带这两个字段，Console 显示「—（未绑定）」）；VIP / 普通用户只看到自己创建的密钥，查看/修改/删除/导出别人的密钥返回 404 `API Key 不存在`。非管理员创建或修改密钥时只能绑定自己拥有的知识库，否则返回 403 `只能为本人创建的知识库创建 API Key`。

`mcp-config` 返回 `{"url":"http://<host>/mcp","headers":{"Authorization":"Bearer <key>","X-RAG-Namespace":"default"}}`，可直接粘贴到 MCP 客户端配置。

### 模型配置（模型列表）

管理员在 Console 侧边栏「模型配置」（`#/model-profiles`）维护可复用的 Embedding / Reranker / VLM / JEV 列表，不再在创建知识库时手工填写服务地址、Key 和模型名。页面按模型类型分页签，表格展示 名称 / 模型 / 服务地址 / Key / 向量维度（仅 Embedding）/ 更新时间 / 操作，支持「添加模型」抽屉、编辑和删除。API Key 只在服务端用 `RAG_STATE_ENCRYPTION_KEY` 加密保存，列表与详情只返回 `api_key_set`；编辑时留空表示沿用已保存的 Key，显式提交空字符串会清除。`(kind, name)` 唯一，重名返回 409。VLM 是唯一的远端 OpenAI 兼容视觉模型，PDF 的版面/OCR/表格/公式/图片描述统一由它完成；OCR 不再作为独立模型类型。

JEV 条目（TypeSafe System One 决策模型）保存名称、服务地址、模型标识和加密 Key；创建与修改都不探测 provider，`/{id}/models` 对 JEV 直接返回空数组。JEV 不是知识库模型，创建/编辑知识库时不会出现该选项，当前仅保存配置，尚未接入检索或决策调用。常见取值：TypeSafe 服务地址 `https://api.typesafe.ai/v1`（模型 `jev-latest` / `jev-preview` / `jev-1.13.0`），OpenRouter 服务地址 `https://openrouter.ai/api/alpha`（模型 `typesafe/jev-1.13`）。

保存时会先探测 provider：

- Embedding：真实执行一次 Embedding 调用，把返回的稠密向量维度记录到 `vector_size`；探测失败则不写入。
- Reranker：执行一次有界的 rerank 调用，只验证服务可达与鉴权。
- VLM：不做探测，因此半配置的 VLM 端点也可以先保存。
- JEV：不做探测，直接保存配置（只保存连接，供后续决策能力使用）。

`PATCH` 只在服务地址、模型或 Key 实际变化时重新探测；只改名称或原样保存不会调用 provider，已记录的 Embedding 维度保持不变。修改服务地址但未提交新 Key 时保存的 Key 会被清除（与知识库连接表单的 `DraftConnection` 语义一致）。

管理员的创建知识库表单现在只从该列表选择模型（Embedding 必填；Reranker、VLM 通过开关启用，**创建页两个开关默认全部开启**，可按需关闭），编辑知识库的抽屉（知识库列表 → 配置）同样选择模型：保存时 Console 读取所选条目的 Key，并把该配置的服务地址、模型名和 Key 复制进原有的知识库请求；**切换 Embedding 到不同连接仍要求「重新向量化并保存」确认**。知识库已有连接不在列表中时，编辑抽屉保留「当前配置」选项，保存时沿用原连接、不触发重新向量化。删除或编辑模型配置不会修改已保存到知识库里的连接。知识库 REST 契约不变（仍接受内联 `embedding`/`reranker`/`vlm` 连接），模型列表只服务于 Console 管理员表单。

以下接口都属于 `/v1/admin/*`，仅本机直连或管理员会话可访问：

```powershell
# 列表（按 kind、名称排序；每个条目含 id/kind/name/base_url/model/api_key_set/vector_size/created_at/updated_at，不含 Key 明文）
curl.exe http://127.0.0.1:8011/v1/admin/model-profiles

# 新建：kind 为 embedding / reranker / vlm / jev；Embedding 会真实探测并记录向量维度
curl.exe -X POST http://127.0.0.1:8011/v1/admin/model-profiles `
  -H "Content-Type: application/json" `
  -d '{"kind":"embedding","name":"生产 Embedding","base_url":"http://127.0.0.1:8032/v1","apikey":"<provider-key>","model":"bge-m3"}'

# 新建 JEV 配置：不探测 provider，保存后即可在列表中编辑；模型名也可填 jev-preview / jev-1.13.0
curl.exe -X POST http://127.0.0.1:8011/v1/admin/model-profiles `
  -H "Content-Type: application/json" `
  -d '{"kind":"jev","name":"生产 JEV","base_url":"https://api.typesafe.ai/v1","apikey":"<jev-key>","model":"jev-latest"}'

# 详情 / 修改（字段均可选，kind 不可修改；省略 apikey 且服务地址不变时沿用已保存的 Key）
curl.exe http://127.0.0.1:8011/v1/admin/model-profiles/<profile_id>
curl.exe -X PATCH http://127.0.0.1:8011/v1/admin/model-profiles/<profile_id> `
  -H "Content-Type: application/json" `
  -d '{"name":"生产 Embedding（主）","model":"bge-m3"}'

# 删除（成功 204；不存在 404）
curl.exe -X DELETE http://127.0.0.1:8011/v1/admin/model-profiles/<profile_id>

# 读取 Key（仅本机/管理员会话；列表、详情接口从不返回明文）
curl.exe http://127.0.0.1:8011/v1/admin/model-profiles/<profile_id>/key

# 通过已保存的连接列出 provider 模型（无请求体；端点没有 /models 时返回空数组，JEV 直接返回空数组）
curl.exe -X POST http://127.0.0.1:8011/v1/admin/model-profiles/<profile_id>/models
```

### 知识库管理（本机管理）

```powershell
# 列表与详情
curl.exe http://127.0.0.1:8011/v1/admin/knowledge-bases
curl.exe http://127.0.0.1:8011/v1/admin/knowledge-bases/default

# 创建：需要可访问的 Embedding API；创建时会真实探测向量维度
curl.exe -X POST http://127.0.0.1:8011/v1/admin/knowledge-bases `
  -H "Content-Type: application/json" `
  -d '{
    "namespace":"default",
    "embedding":{"base_url":"http://127.0.0.1:8032/v1","apikey":"<provider-key>"},
    "embedding_model":"bge-m3",
    "reranker":{"base_url":"http://127.0.0.1:8032/v1","apikey":""},
    "reranker_model":"",
    "strategy":"recursive",
    "chunk_size":512,
    "chunk_overlap":64
  }'

# 修改（修改 embedding URL/模型/维度必须 reembed=true）
curl.exe -X PATCH http://127.0.0.1:8011/v1/admin/knowledge-bases/default `
  -H "Content-Type: application/json" `
  -d '{"reranker_model":"bge-reranker-base","reranker":{"base_url":"http://127.0.0.1:8032/v1"},"reembed":false}'

# 模型列表、切分策略目录、模型 Key 读取（本机管理）
curl.exe -X POST http://127.0.0.1:8011/v1/admin/knowledge-bases/models/embedding `
  -H "Content-Type: application/json" -d '{"base_url":"http://127.0.0.1:8032/v1"}'
curl.exe http://127.0.0.1:8011/v1/admin/knowledge-bases/defaults
curl.exe http://127.0.0.1:8011/v1/admin/knowledge-bases/default/key/embedding

# 删除单个知识库：逐个删除（逻辑删除 del_flag）该命名空间全部文档后移除配置记录
# 成功 204；配置和文档都不存在 404；有文档删除失败时保留配置并返回 409
curl.exe -X DELETE http://127.0.0.1:8011/v1/admin/knowledge-bases/default

# 批量删除（Console 知识库页「批量删除」走这条接口；1..100 个命名空间，重复自动去重）
curl.exe -X POST http://127.0.0.1:8011/v1/admin/knowledge-bases/batch-delete `
  -H "Content-Type: application/json" `
  -d '{"namespaces":["team-kb","old-kb"]}'
```

知识库的列表是「配置记录 ∪ 文档工作区」的合并视图，所以只删配置记录会留下一个「缺少配置记录」的条目。`DELETE /v1/admin/knowledge-bases/{namespace}` 因此先删除该命名空间下的全部文档、再删除配置记录：文档走与单文档删除相同的逻辑删除（`del_flag`，Qdrant 向量按现有语义保留，不删 collection）；任一文档删除失败（例如正在上传/更新）时保留配置记录，返回 409 `{"detail":"部分文档删除失败，知识库未删除","namespace":"...","failed_documents":[{"name":"...","error":"..."}]}`，可修复后重试。

`batch-delete` 逐条执行同样的删除，返回 `{"deleted":["<namespace>"],"missing":["<namespace>"],"failed":[{"namespace":"...","error":"...","failed_documents":[...]}],"documents_deleted":N,"configurations_deleted":N}`：`deleted` 为已删除，`missing` 为配置和文档都不存在的命名空间，`failed` 为部分文档删除失败或运行时报错（该命名空间保留，Console 保持选中以便重试）；请求体校验失败（缺失、空数组、非法命名空间、超过 100 项、未知字段）返回 422。

公开配置：`GET /v1/knowledge-base`（返回 `public_settings + strategies`，缺少知识库时 404）。创建/修改的请求体与 Python 实现一致，`embedding` 必填，`embedding_model` 长度 1..512。Console 管理员表单改为从「模型配置」模型列表选择（见上一节），但 REST 契约不变：仍提交内联的 `embedding`/`reranker`/`vlm` 连接。

### 知识库归属、权限与邀请（Console 会话）

知识库默认归创建者私有：管理员可见全部，其他账号只能看到自己创建的与被邀请授权的知识库。以下接口需要 Console 登录会话（Cookie + `X-CSRF-Token`）；完整语义见 [权限管理](PERMISSIONS.md)。

控制台顶部「选择知识库」在登录后自动加载当前账号可见的知识库，并按账号分别记住当前选择：重新登录或在账号间切换时，会校验原选择是否仍可访问，失效则回退到第一个可用知识库；没有任何可见知识库时显示「暂无可访问的知识库」，检索与文档页面不会发起依赖知识库的请求。使用 API Key 直连（未启用登录）时同样会自动加载该密钥的授权范围，并纠正浏览器里已失效的旧选择。

```powershell
# 当前账号可用的知识库与某个知识库上的有效权限
curl.exe http://127.0.0.1:8011/v1/namespaces
curl.exe http://127.0.0.1:8011/v1/knowledge-bases/team-kb/access

# VIP/普通用户自建知识库（模型配置沿用 RAG_DEFAULT_KB_TEMPLATE 模板）
curl.exe -X POST http://127.0.0.1:8011/v1/knowledge-bases `
  -H "Content-Type: application/json" `
  -d '{"namespace":"my-kb","strategy":"character","chunk_size":800,"chunk_overlap":100}'

# 邀请人（管理员或知识库创建者）创建邀请并拿到一次性链接
curl.exe -X POST http://127.0.0.1:8011/v1/knowledge-bases/team-kb/invitations `
  -H "Content-Type: application/json" `
  -d '{"username":"dave","permission":"read","scope":"files","document_names":["spec.md","guide.pdf"]}'

# 被邀请人：收件箱、预览、接受/拒绝
curl.exe http://127.0.0.1:8011/v1/invitations
curl.exe -X POST http://127.0.0.1:8011/v1/invitations/preview -H "Content-Type: application/json" -d '{"token":"<link-token>"}'
curl.exe -X POST http://127.0.0.1:8011/v1/invitations/accept  -H "Content-Type: application/json" -d '{"token":"<link-token>"}'

# 管理已有授权与未接受的邀请
curl.exe http://127.0.0.1:8011/v1/knowledge-bases/team-kb/permissions
curl.exe -X DELETE http://127.0.0.1:8011/v1/knowledge-bases/team-kb/permissions/<user_id>
curl.exe -X DELETE http://127.0.0.1:8011/v1/invitations/<invitation_id>
```

邀请链接形如 `http://127.0.0.1:8011/console#/invite?token=<一次性token>`；数据库只保存 token 的 SHA-256 摘要，链接只在创建响应中出现一次。邀请只能由被邀请的账号接受。

### 文档管理

```powershell
# 上传前检查（按顺序返回 files，document 为已存在文档或 null）
curl.exe -X POST http://127.0.0.1:8011/v1/documents/check `
  -H $headers -d '{"files":[{"name":"guide.md","sha256":"<64-hex>"}]}'

# 上传（默认异步受理；multipart，字段名必须为 files；可选 Idempotency-Key）
curl.exe -X POST http://127.0.0.1:8011/v1/document-batches `
  -H "Authorization: Bearer <token-or-key>" `
  -H "X-RAG-Namespace: default" `
  -H "Idempotency-Key: batch-20261108-01" `
  -F "files=@guide.md" -F "files=@manual.txt"

# 202 响应中 items[].artifact_id 标识已受理文件，可按 id 轮询状态
curl.exe "http://127.0.0.1:8011/v1/document-jobs/<artifact_id>" -H $headers

# 列表、读取正文、删除
curl.exe http://127.0.0.1:8011/v1/documents -H $headers
curl.exe "http://127.0.0.1:8011/v1/documents/guide.md/content" -H $headers   # 文本
curl.exe -X DELETE "http://127.0.0.1:8011/v1/documents/guide.md" -H $headers
```

批次上传返回 `202`，响应包含 `batch_id`、`total/accepted/skipped/failed` 计数与 `items`。每项包含文件名、`accepted/skipped/failed` 结果、原因，以及受理成功后的 `artifact_id`。使用 `GET /v1/document-batches/{batch_id}` 查询整个批次，或 `GET /v1/document-jobs/{artifact_id}` 查询单文件状态；只有任务 `succeeded` 才表示已索引。重复提交同一 `Idempotency-Key` 会返回原批次。单文件 10 MiB、单批 10 个，切分参数来自知识库配置，不接受请求级覆盖。PDF 正文下载返回原文件字节（`Content-Type: application/pdf`），版本化 PDF/Word 的文本视图读取已保存的 `document.md`。

### 搜索、结果与产物

```powershell
curl.exe -X POST http://127.0.0.1:8011/v1/search `
  -H $headers `
  -d '{"query":"E03 怎么处理？","retrieval":"hybrid","rerank":"cross_encoder","candidate_k":10,"top_k":5,"filters":{}}'

# 同步搜索（一次调用直接返回证据，无任务 ID/轮询）
curl.exe -X POST http://127.0.0.1:8011/v1/search `
  -H $headers `
  -d '{"query":"E03 怎么处理？","retrieval":"hybrid","rerank":"cross_encoder","candidate_k":10,"top_k":5,"filters":{}}'

# artifact（源文件/图片/表格 JSON）
curl.exe "http://127.0.0.1:8011/v1/artifacts/<artifact_id>" -H $headers
curl.exe "http://127.0.0.1:8011/v1/artifacts/<artifact_id>/content" -H $headers
```

搜索**同步执行**：一次请求完成检索 + 重排并直接返回 `retrieval`、`rerank`、`evidence_status`、`results`、`debug`、`snapshot_context`、`artifacts`；资产命中带 `asset`（图片 base64 或表格结构）。响应**不含** `result_id`/`created_at`/`expires_at`/`snapshot_available`/`snapshot_error`，`GET /v1/search-results/{result_id}` 已删除。分页/偏移参数越界返回 400；超过 `RAG_SEARCH_TIMEOUT_SECONDS` 返回搜索错误，重试等于重新检索。

### 上传状态、发布版本与租约

```powershell
curl.exe "http://127.0.0.1:8011/v1/uploads" -H $headers
curl.exe "http://127.0.0.1:8011/v1/uploads/<operation_id>" -H $headers
curl.exe "http://127.0.0.1:8011/v1/admin/published-versions" -H "X-RAG-Namespace: default"
curl.exe "http://127.0.0.1:8011/v1/admin/upload-leases" -H "X-RAG-Namespace: default"
```

后两者属于只读展示模型，除租户认证外也要求本机管理边界。

### 工具与提示词管理（本机管理）

```powershell
curl.exe http://127.0.0.1:8011/v1/admin/tools -H "X-RAG-Namespace: default"
curl.exe -X PATCH http://127.0.0.1:8011/v1/admin/tools/search `
  -H "X-RAG-Namespace: default" -H "Content-Type: application/json" -d '{"enabled":false}'
curl.exe http://127.0.0.1:8011/v1/admin/prompts -H "X-RAG-Namespace: default"
curl.exe -X PUT http://127.0.0.1:8011/v1/admin/prompts/summary `
  -H "X-RAG-Namespace: default" -H "Content-Type: application/json" `
  -d '{"description":"总结资料","arguments":[{"name":"text","description":"正文","required":true}],"messages":[{"role":"user","text":"{{text}}"}]}'
```

模板只替换 `{{argument}}`，不执行表达式；提示词 `PUT` 校验参数名与消息角色。

## 6. MCP 使用

### 连接

```json
{
  "url": "http://127.0.0.1:8011/mcp",
  "headers": {
    "Authorization": "Bearer <RAG_SERVICE_TOKEN 或 API Key>",
    "X-RAG-Namespace": "default"
  }
}
```

MCP **始终**要求 `Authorization`（loopback 也不例外）；本地管理界面可用 `mcp-config` 生成配置。支持的协议：旧版 `2025-11-25`（initialize → initialized → tools/list/call）与新版 `2026-07-28`（每次请求自描述，可不建旧会话）；由官方 Go SDK 按请求分发，客户端不能把新版版本号塞进旧版 initialize。订阅/资源通知只在 API 进程内投递，不跨进程推送、无历史重放。

### 工具

| 工具 | 参数 | 行为 |
| --- | --- | --- |
| `search` | `query`、`retrieval`、`rerank`、`candidate_k`、`top_k`、`filters` | 同步检索并直接返回证据；无 `wait_seconds`/任务 ID/轮询 |
| `add_document` | `name` + `content`/`content_base64`，可选 `idempotency_key` | 异步入库，成功只返回 `artifact_id` |
| `get_document_status` | `artifact_id` | 查询 `status/stage/error`，状态在 MySQL |
| `update_document` | `name` + `content`/`content_base64`，可选 `idempotency_key` | 同步替换并重建索引 |
| `delete_document` | `name` | 同步逻辑删除 |

工具集合固定 **5 个**（`get_search_result` 已于 2026-11-13 删除）；`delete_document` 标注为 destructive。是否启用某个工具可经 `/v1/admin/tools` 管理；执行时会实时读取 catalog，禁用后返回错误。

### 同步搜索语义

- `search` **永远同步**：一次 `tools/call` 完成检索 + 重排并直接返回证据（`results`/`artifacts`/`evidence_status`/`answer_guidance`/`debug`）。没有 `wait_seconds`、没有 `result_id`、没有轮询；`get_search_result` 工具已删除。
- 总预算 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）：在服务内基于父 ctx 派生 deadline，已有更早 deadline 不延长；超时返回搜索错误。客户端断开/取消会向下传递到 Qdrant 与模型调用。
- 权限与文档范围过滤保留：逐请求 scope 头 → `ResolveDocumentIDs` → 检索 `DocumentIDs`；受限 scope 现在会真正执行同步检索（原先在排队模式下被拒绝）。
- 图片资产沿用 ImageContent；artifact 按 ID 独立读取，保留期 `RAG_RESULT_TTL_SECONDS`（默认 3600 秒，语义为 artifact TTL）。
- `search_tasks`/`search_result_snapshots` 为历史表、保留数据但不再读写；旧客户端迁移见 [部署指南](DEPLOYMENT.md) §12。

### 异步文档

`add_document` 在状态库单事务写入完整任务（含文件字节、`schedule_version=1`），提交后返回 `{"artifact_id": "<uuid>"}`，不代表已解析/可检索。`get_document_status` 返回 `status`（`queued/processing/succeeded/failed`）、`stage`、`error`；仅 `succeeded` 表示已持久发布。同名文档已存在或已有排队/执行中的新增会被拒绝；不确定提交结果时用同一 `idempotency_key` 重试，不会重复入队。

投递方式：文档通知由 Debezium **任务连接器**采集 `document_jobs` 表后投递（只投递 INSERT 与 `schedule_version` 变化），应用不写任何 outbox；配置见 [Debezium 指南](../deploy/debezium/README.md) 与 [CDC 迁移契约](cdc-migration.md)。唯一消费者是独立的 `rag-document-worker`；未配置 Kafka/Debezium 或未运行该 worker 时，文档任务保持 `queued`。

### 资源、提示词与订阅

- `resources/list` / `resources/templates/list` / `resources/read`：文档 URI 为 `rag://knowledge/<namespace>/<文件名>`，正文按段落分页（每页最多 8000 字符），返回 JSON 含 `uri/text/page/total_pages/version/truncated/next_uri`。
- `prompts/list` / `prompts/get`：使用 `/v1/admin/prompts` 管理的模板。
- 旧版 `resources/subscribe`、列表变化通知与新版 `subscriptions/listen` 可用；通知仅表示“发生变化”，客户端需重新列出/读取。

## 7. 迁移与 CLI

### rag-state-migrate

```powershell
.\dist\bin\rag-state-migrate.exe                 # 只报告 schema 就绪度
.\dist\bin\rag-state-migrate.exe --apply         # 幂等创建缺失表/增量迁移
.\dist\bin\rag-state-migrate.exe --data-root D:/praxis-rag/data
```

环境同服务（`RAG_STATE_URL`/`RAG_MYSQL_*`/`RAG_SERVICE_DATA_ROOT`）。退出码：`0` schema 就绪，`1` 不完整/失败，`2` 配置错误。

### rag-service-migrate（旧知识源导入 / 旧表下线）

```powershell
# 默认只读预演
.\dist\bin\rag-service-migrate.exe --legacy-root "$env:USERPROFILE/.mini_agent"
# 实际导入（写入状态库：blobs + documents）
.\dist\bin\rag-service-migrate.exe --apply --legacy-root "$env:USERPROFILE/.mini_agent"
# 只迁移指定用户（可重复）
.\dist\bin\rag-service-migrate.exe --apply --user-id <uuid>

# 旧 outbox 表下线（只在 CDC 切换完成、确认无旧进程写入后执行）
.\dist\bin\rag-service-migrate.exe --retire-outbox --yes
```

**重要差异**：`--apply` 只导入状态（源文件 blob 与文档记录），**不执行向量重建**（Python 版会重跑上传）。迁移后必须通过 HTTP/MCP 重新上传（或等待后续 reindex 支持）才会进入 Qdrant 可检索；`--skip-assets` 与旧版解析资产树尚未实现（TODO）。退出码 `0/1/2` 同上；`WARN` 行最多打印 20 条。

`--retire-outbox` 必须同时传 `--yes` 才会执行，逐表 `DROP TABLE IF EXISTS` 删除 `document_job_outbox`、`search_task_outbox`、`cache_outbox`、`document_task_outbox`，只删除不重建、不默认执行；每张表单独打印“已删除/不存在”，任一失败返回 `1`。旧表下线是 CDC 存量切换的最后一步，完整顺序见 [CDC 迁移契约 §8](cdc-migration.md#8-存量切换方案)。

### rag-cdc-cutover（CDC 存量切换）

```powershell
# 1. 只读盘点：未完成文档/搜索任务、缓存 key 数与 revision、脏 key，并打印 SHOW MASTER STATUS 指引
.\dist\bin\rag-cdc-cutover.exe --inventory

# 2. 停写窗口内记录 Debezium 起始 binlog 位点（File/Position 取自 SHOW MASTER STATUS）
.\dist\bin\rag-cdc-cutover.exe --record-position --file=mysql-bin.000123 --pos=4567890

# 3. 为仍未完成的任务递增 schedule_version 到基线之上，保证每条任务都有可消费通知
.\dist\bin\rag-cdc-cutover.exe --backfill --baseline=7

# 4. 缓存基线：把所有读模型 key 标脏，由缓存消费者重建
.\dist\bin\rag-cdc-cutover.exe --mark-cache-baseline

# 5. 核对：断言活动任务 schedule_version >= baseline+1，输出核对清单
.\dist\bin\rag-cdc-cutover.exe --verify --baseline=7

# 或者按顺序执行上述步骤（需要 --file/--pos/--baseline）
.\dist\bin\rag-cdc-cutover.exe --all --file=mysql-bin.000123 --pos=4567890 --baseline=7
```

所有子命令复用服务同款 `RAG_STATE_*`/`RAG_MYSQL_*` 连接配置，只读写 `document_jobs`、缓存相关表与 `cdc_cutover_state`；`search_tasks` 不再读写（历史表保留）。日志只输出计数、状态与 key 名称，不包含任务参数、文档正文或凭据。退出码：`0` 成功 / `1` 核对不通过或运行时失败 / `2` 参数或配置错误。切换与回滚的完整说明见 [CDC 迁移契约](cdc-migration.md) §8（文档/缓存）与 §12（搜索同步化）。

### rag-mcp-ingest（本地批量导入）与 rag-mcp-debug

`rag-mcp-ingest` 是 Python `ingest.py` 的 Go 移植：递归发现支持的文档，逐个走 `Service.WriteDocument` 上传/索引管线，输出 `OK/SKIP/FAIL` 行，最后打印 `SUMMARY discovered=<n> imported=<n> chunks=<n> failed=<n>`。

```powershell
# 需要 RAG_BACKEND=qdrant、QDRANT_URL、可用模型 API 和目标 namespace 的知识库配置
.\dist\bin\rag-mcp-ingest.exe --namespace default .\docs
.\dist\bin\rag-mcp-ingest.exe --dry-run .\docs            # 只列出将导入的文件
.\dist\bin\rag-mcp-ingest.exe --verbose --dir .\docs     # 打印配置/进度
```

- 目录内文档按相对 POSIX 路径命名（与 Python 一致）；单文件按 basename。
- 同名已存在文档默认覆盖；内容未变时输出 `SKIP ... unchanged content`，不重复 embedding（Python 会无条件重跑）。
- 退出码：`0` 全部成功；`1` 至少一个文档失败或运行时不可用；`2` 参数/配置错误（缺 `RAG_BACKEND=qdrant`、`QDRANT_PATH`、namespace 或知识库不存在）。
- PDF/DOCX/DOC 与 HTTP 上传使用同一解析配置（PDF/DOCX 由 Go 进程内解析，DOC 需 LibreOffice）；解析器不可用时按上传同规则失败。`--dry-run` 不打开运行时/状态库。
- 兼容性说明：该命令在本仓库验证环境中未连接真实 Qdrant/模型 API 实跑，上线前先用 `--dry-run` 和一个小目录验证。

`rag-mcp-debug`：无 Qdrant/模型的 mock 检索调试：

```powershell
.\dist\bin\rag-mcp-debug.exe "RRF 怎么融合 BM25 和 Dense？"
.\dist\bin\rag-mcp-debug.exe --query "E03" --limit 3
```

## 8. Docker 与 Compose

```powershell
# 构建镜像：Node 阶段构建 Console，Go 阶段编译全部 cmd/*，alpine 非 root 运行
docker build -t praxis-rag:local .
# 受限网络可覆盖：
# docker build --build-arg NPM_REGISTRY=https://registry.npmmirror.com `
#              --build-arg GOPROXY=https://goproxy.cn,direct -t praxis-rag:local .

# 直接运行（容器绑定 0.0.0.0，RAG_SERVICE_TOKEN 必填；allowlist 已在镜像配置中提供 loopback 默认值）
docker run --rm -p 127.0.0.1:8011:8011 `
  -e RAG_SERVICE_TOKEN="<random-secret>" `
  -e QDRANT_URL="http://host.docker.internal:6333" `
  -v praxis-data:/app/data `
  praxis-rag:local

# Compose 的 app profile（会构建 Go 镜像；默认 compose 还会启动 pdf-parser）
docker compose --profile app up -d --build

# PDF 解析微服务（独立镜像：FastAPI + PyMuPDF + httpx，本地不加载模型）
docker compose up -d --build pdf-parser
curl.exe http://127.0.0.1:8091/healthz
```

镜像内容与约定：

- `/app/bin/` 包含全部命令（含四个 worker 与迁移/切换 CLI）；`ENTRYPOINT` 为 `/app/bin/rag-service`。
- `/app/web` 为 Console，`RAG_WEB_ROOT=/app/web`；`/app/config.toml` 为非敏感默认配置（`RAG_SERVICE_HOST=0.0.0.0`、端口 8011、`/app/data`、Qdrant 服务名、loopback MCP 白名单）。
- 非 root 用户 `rag`，`/app/data` 为数据卷；健康检查调用 `/health`。
- Go 镜像**不含** Python 或 LibreOffice：DOCX 解析与 Word 图片描述内置在 Go 二进制中，PDF 高保真解析由独立的 `pdf-parser` 镜像（`python-workers/pdf-service`，容器端口 8091，健康检查 `/healthz`）通过 HTTP 完成；只有 .doc 转换需要把 LibreOffice 挂载到容器并设置 `RAG_WORD_CONVERTER_PATH`。Word 无图注图片需要 `RAG_VLM_BASE_URL`/`RAG_VLM_MODEL`（或 Console 知识库设置）才会生成描述，未配置时保留占位描述、上传不会失败；vision 引擎的 PDF 视觉解析复用同一个视觉模型端点。
- pdf-parser 镜像与 Go 镜像相互独立：`pdf-parser` 属于默认 profile，compose 的 `app` profile 会通过 `depends_on` 一并拉起它（因此用 app profile 时不能跳过）；只用纯文本引擎时 Go 进程不调用它（`RAG_PDF_ENGINE=go`），也可以不使用 app profile、直接运行 Go 二进制，此时无需构建/启动 pdf-parser。它本地不加载模型，使用前只需在 Console 为知识库选择 VLM 或配置 `RAG_VLM_*`。
- Compose `app` 服务只运行 `rag-service`；worker 没有内置 compose 服务，可用一次性容器（把入口覆盖为 worker 二进制或 `/app/bin/rag-document-worker`、连接到同一网络并复用同一环境）或参考 [DEPLOYMENT](DEPLOYMENT.md) 的 systemd/override 示例。
- **已验证/未验证**：本机 Docker 已实机拉起 Kafka + Debezium 基础设施，并用 host 模式 Go 服务/worker 跑通 Debezium 异步链路与 DLQ（见 §10；脚本 `scripts/verify-debezium-e2e.py`、`scripts/verify-kafka-dlq.py`）；原排队搜索脚本 `verify-kafka-search.py` 已随功能删除。Go 镜像构建、compose `app` profile 与 worker 容器仍未经实机验证；pdf-parser 镜像与真实视觉端点推理同样未在本环境验证（联调使用 `RAG_PDF_SERVICE_CONVERTER=fake`，测试不访问网络）。

## 9. Token 与远程部署

默认只监听 loopback。绑定非 loopback 地址时必须设置 Token 与 MCP 白名单：

```powershell
$env:RAG_SERVICE_HOST = "0.0.0.0"
$env:RAG_SERVICE_TOKEN = "replace-with-a-random-secret"
$env:RAG_MCP_ALLOWED_HOSTS = "rag.example.com"
$env:RAG_MCP_ALLOWED_ORIGINS = "https://app.example.com"
.\dist\bin\rag-service.exe
```

线上 MCP 客户端需同时发送 `Authorization: Bearer <RAG_SERVICE_TOKEN>` 与 `X-RAG-Namespace`。生产环境应在服务前配置 HTTPS、访问控制和请求大小限制；`/v1/admin/*` 仍只允许本机直连，远程管理需自行通过受控跳板或关闭管理入口。Console 的 Token 只保存在浏览器 `sessionStorage`。

## 10. 验证范围与未验证项

已在仓库内验证：

- `go build ./...` 全部通过；`go test -p 1 -count=1 -timeout 25m ./...` 全部通过（串行是必须的：所有包共用同一个 MySQL 测试 schema，靠 advisory lock 互斥）。测试覆盖 state/runtime/workers/httpapi/mcp/searchrefs/检索栈，以及 `internal/e2e` 的真实 HTTP 全栈链路。
- `scripts/build-go.ps1` 本机构建 + 冒烟：`/health 200 status=ok`、`/console` 返回构建后的 Console、`/` 200。POSIX 的 `scripts/build-go.sh`/`make check` 未在当前 Windows 验证环境执行。
- 四个 worker 二进制与切换/迁移 CLI 的 `--help`、缺少 Kafka、错误 backend、非法 `--kind` 的退出码行为。
- Go 端 REST/MCP/检索/文档/资产投影/`document.md` 预览/上传事务的单元与集成测试（真实 MySQL + fake Qdrant/model）。
- Console 角色/知识库归属/邀请共享的**全栈 HTTP E2E**（真实登录会话与 CSRF、真实 access 服务、真实 MySQL）：`internal/e2e/permissions_http_test.go` 覆盖角色变更、归属可见性、邀请创建→收件箱→预览→接受、只读/读写闸门、模板建库、撤销授权。异步上传发布后读模型刷新的回归也由 `internal/e2e` 覆盖。

**未验证**（不要据此宣称生产就绪）：

- Docker 镜像构建、Compose `app` profile、systemd 单元未实际执行（本环境无 Docker/systemd）。
- **历史实机验证（已下线链路）**：2026-11-09 在 Kafka 3.9.0 + Debezium Server 3.7.1 + MySQL 8.0 + Redis + Qdrant 上验证过当时的“事务 outbox + 单 Debezium 连接器”投递、Go worker 异步链路、显式 DLQ topic 与排队 MCP 搜索；该链路已于 2026-11-13 改造下线。仍未验证 SASL/TLS 外部 Kafka、MySQL 多实例锁与 systemd 部署。
- **2026-11-13 CDC 改造未实机验证**：双 Debezium 连接器（任务/缓存）与三 topic、缓存消费者 revision 收敛、搜索 MySQL 结果回源、存量切换与回滚演练均未在真实 Docker/Debezium/Kafka/Redis 上执行；本工作树只做了文档一致性、前端类型自检与 Go 目标构建/单测，边界见 [VALIDATION.md](../VALIDATION.md)。
- PDF 默认引擎（vision）依赖的 pdf-parser 微服务：Python 侧测试覆盖 `GET /healthz`、`POST /v1/parse` 的 zip/manifest 与错误码，视觉客户端用 fake/mock transport 覆盖（不访问网络）；真实视觉端点推理、镜像构建未在本环境实机验证，正式上线前需在预发环境用真实 PDF 与真实视觉端点验证。Go 内置纯文本引擎（`RAG_PDF_ENGINE=go`）与 DOCX 解析有单元测试；.doc 转换依赖外部 LibreOffice，未与真实 LibreOffice 联调。远端视觉模型端点只做了配置/协议单测与 httptest 集成测试，未与真实端点做实机联调。
- Console 有前端单元与契约测试（`frontend/src/*.test.ts`，Vitest：角色/权限文案、邀请过期计算、api 客户端的路径/CSRF/错误契约、路由与邀请 token 解析）与构建检查；但**没有浏览器级 E2E**，页面交互/样式仍由人工核对。
- Tokenizer 是确定性近似，不保证与 Python/BGE-M3 的 token 计数一致（句子/段落/结构边界一致）。
- 异步 e2e（API 受理 → fake Kafka transport → worker → 发布）已由 `internal/e2e` 覆盖。历史脚本 `scripts/verify-debezium-e2e.py`、`verify-kafka-search.py`、`verify-kafka-dlq.py` 对应已下线的 outbox 链路，不再代表当前双连接器架构；新链路验证需按 [CDC 迁移契约 §11](cdc-migration.md#11-验收矩阵) 重跑并记录。

## 11. 开发与测试

### 后端（Go）

```powershell
gofmt -l internal cmd
go vet ./...
go build ./...

# 测试需要一次性 MySQL schema（库名必须含 test），不会回退 SQLite
$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:change-me@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'

# 必须串行：所有包共用同一个 MySQL 测试 schema，靠 GET_LOCK 互斥；
# 并行运行会排队甚至撞上默认 10 分钟包超时。
go test -p 1 -count=1 -timeout 25m ./...

# 只跑某一部分（同一时间仍然只跑一个包，避免抢锁）
go test -p 1 -count=1 -timeout 10m ./internal/runtime/
go test -p 1 -count=1 -timeout 10m ./internal/httpapi/ ./internal/mcp/
```

`internal/e2e` 是真实 HTTP 接口的全栈测试：真实 MySQL 状态库、真实 `httpapi`/`runtime`/`access`/`auth`，模型、Qdrant、Redis（miniredis）与 broker 用进程内 fake。新增的 `permissions_http_test.go` 覆盖角色、知识库归属、邀请链接生命周期与文档读写闸门，约 45 秒。

也可以用脚本/Make 目标（会带上 `-p 1` 和超时）：

```powershell
pwsh -File scripts/build-go.ps1 -Test          # 构建 + 串行 Go 测试 + 冒烟
make test                                      # go test -p 1 ...
```

### 前端（Console）

```powershell
cd frontend
npm ci
npm run typecheck        # 含测试文件
npm test                 # Vitest 单元/契约测试（worker threads + jsdom）
npm run build            # tsc + vite build
```

`npm test` 的用例位于 `frontend/src/*.test.ts`：`api.test.ts` 校验 api 客户端的 URL/请求头/CSRF/错误契约与共享接口的请求体，`navigation.test.ts` 校验 hash 路由、邀请 token 解析与 namespace 规则，`apiKeys.test.ts` 校验 API Key 页的 Namespace 选项与所有者展示规则（管理员/本机模式看到全部知识库，非管理员只看到本人拥有的知识库）。

`make check` 一次执行 `vet + Go 测试 + 前端 typecheck/测试/构建`（需要 `RAG_TEST_MYSQL_URL`）。

### 持续集成

`.github/workflows/test.yml` 提供两条流水线：`go vet + go build + go test -p 1`（GitHub Actions 的 MySQL 8 service，库名 `praxis_unit_test`）和前端 `typecheck + npm test + build`。CI 与本地命令一致，唯一区别是 Linux 上不需要 `-p 1` 以外的额外处理。

PDF 默认由独立的 `pdf-parser` 微服务解析（本地 PyMuPDF 读取/渲染/裁图，版面/OCR/表格/公式/图片描述调用远端 OpenAI 兼容视觉模型，HTTP + zip 清单，不进入 Go 构建/镜像）；`RAG_PDF_ENGINE=go` 时使用内置纯 Go 阅读器。DOCX 由内置 OOXML 读取器解析，Word 无图注图片由 Go 直接调用远端 OpenAI 兼容视觉模型；只有 .doc 转换需要外部 LibreOffice（`RAG_WORD_CONVERTER_PATH`）。

## 12. 当前限制

- **Qdrant**：仅支持 HTTP 模式；`QDRANT_PATH` 会在启动时被拒绝。
- **Tokenizer**：见 §10；token 预算切分位置可能与 Python 不同。
- **解析器**：PDF 默认走独立 `pdf-parser` 微服务（本地只做 PDF 读取/渲染/裁图，版面/OCR/表格/公式 LaTeX/图片描述统一由知识库配置的远端视觉模型完成；需要该服务与可用视觉端点，服务或模型不可用时 vision 上传会失败，`docling` 是 `vision` 的兼容别名）。`RAG_PDF_ENGINE=go` 时使用 Go 内置纯文本引擎（文字型 PDF；扫描件、PDF 表格/公式/图片资产不支持，需要时请先 OCR 或用其他工具预处理）。DOCX 解析在 Go 进程内完成；Word 无图注图片需要视觉模型端点才会生成描述，未配置时保留占位描述、上传不失败；.doc 转换仍需要外部 LibreOffice。
- **异步投递**：Kafka 通知由两个 Debezium 连接器从 MySQL 表级采集后写入 Kafka（任务连接器只采集 `document_jobs` 的 INSERT 与 `schedule_version` 变化；缓存连接器采集 8 张业务表全部 c/u/d）；必须配置 Kafka + 两个 Debezium 连接器并运行 `rag-document-worker`/`rag-cache-worker`，否则文档任务保持 `queued`、缓存刷新不生效。
- **同步搜索**：MCP `search` 与 REST `POST /v1/search` 一次调用完成检索 + 重排并直接返回证据，无任务 ID/轮询/结果快照；总预算 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）。`search_tasks`/`search_result_snapshots` 为历史表、保留数据但不再读写，搜索 topic/消费组/DLQ 与 `rag-search-worker` 已下线；迁移与回滚见 [部署指南](DEPLOYMENT.md) §12。
- **上传受理**：REST/Console 统一使用 `POST /v1/document-batches` 异步受理，通过批次或文档任务状态接口观察进度。`rag-service` 只受理，文档解析只由消费 Kafka 通知的 `rag-document-worker` 完成；未部署时任务保持 `queued`。MCP `update_document`/`delete_document` 仍同步执行，通过同名检查和发布事务内的 `ValidatePublication` 与在途文档任务互斥。
- **权限与共享**：账号分为管理员 / VIP / 普通用户；知识库默认归创建者私有，共享靠邀请链接完成（权限：可读/可修改；范围：单文件/多文件/全库），只有 VIP 与管理员可发起邀请。邀请消息目前是站内通知（控制台铃铛），不发送邮件/IM；链接只展示一次，库内只存摘要。文件级写授权只能替换已授权的同名文档，不能上传新文件；历史无创建者的知识库仅管理员可用/可授权；受限账号在同步检索中按文档 scope 过滤，不再被拒绝。详见 [权限管理](PERMISSIONS.md)。
- **管理边界**：`/v1/admin/*` 仅本机直连或管理员会话；**例外**是 `/v1/admin/api-keys` 的五条管理路由——任意登录角色的会话都可管理自己的 API Key（管理员管理全部，非管理员只能绑定本人拥有的知识库）。API Key 按创建账号归属隔离，历史无主密钥仅管理员/本机管理可见；管理凭证（Service Token / API Key）在请求时仍不做知识库归属校验，API Key 的 namespace 白名单仍然生效。详见 [权限管理](PERMISSIONS.md) §9。
- **数据**：状态表用 `rag-state-migrate`；旧知识源 `rag-service-migrate --apply` 不重建向量，需重新上传。旧任务/旧结果快照不迁移也不读取；artifact 以 MySQL `artifact_snapshots` 为事实来源（保留期 `RAG_RESULT_TTL_SECONDS`），`search_tasks`/`search_result_snapshots` 保留历史数据、不再读写，物理删除另行安排。
- 具体契约差异（cached read views、静态目录列表等剩余的 P1/P2）见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)。





