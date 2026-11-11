# Praxis-RAG

**面向 AI 助手和业务应用的独立知识库与检索服务。Go 后端 + Web Console。**

Praxis-RAG 把分散的文档整理为可搜索、可追溯的知识库。你可以在网页中上传和管理资料，比较检索结果，也可以让 AI 助手通过 MCP 连接知识库，查找资料、读取正文和维护文档。

当前对外运行的后端是 **Go 实现**（`internal/`、`cmd/`）：一个 `rag-service` 进程提供 REST、MCP 和 Console，另提供文档、搜索、死信三个独立 worker。Python 主后端与旧 Python 解析 worker 已从仓库删除；PDF 高保真解析恢复为独立的 **Docling 解析微服务**（`python-workers/pdf-service`，Go 通过 HTTP 调用，不进入 Go 进程），Go 服务本身仍无 Python 依赖，也可用 `RAG_PDF_ENGINE=go` 切回内置纯文本引擎；Word/DOCX 解析与 Word 图片描述仍在 Go 进程内完成（图片描述调用远端 VLM）。迁移历史见 [迁移状态](#迁移状态) 与 [docs/GO_MIGRATION.md](docs/GO_MIGRATION.md)。

它负责提供资料和出处，**不直接生成最终回答**。如何提问、是否继续查找、如何组织答案，由接入的 AI 助手或业务应用决定。

## 适用场景

- **产品与售后知识库**：从手册、操作说明和故障记录中查找处理步骤，并返回对应资料。
- **团队文档检索**：按工作区整理项目资料，让网页、AI 助手和业务系统访问同一份知识库。
- **研究资料查阅**：检索 PDF/Word 正文，再对照原文核查。
- **检索方案调试**：比较关键词检索、语义检索和混合检索，调整切分方式与结果排序。

## 主要功能

### 文档导入与管理

支持 Markdown、TXT、PDF、DOCX，以及安装 LibreOffice 后的 DOC。网页/REST 上传默认异步受理：每个文件在共享状态数据库（MySQL）的一个事务内写入完整任务与发件箱通知，**提交成功即返回 `artifact_id`**，解析与索引由文档 worker 在后台完成（无 Kafka 的单进程部署由 `rag-service` 内嵌的 worker 完成），前端用 `GET /v1/document-jobs/{artifact_id}` 轮询 `status`/`stage`，Console 上传对话框同样轮询并显示每个文件的进度。设置 `RAG_UPLOAD_MODE=sync` 可回退为请求内解析/切分/Embedding/发布的同步模式。MCP `add_document` 始终异步；MCP `update_document`、`delete_document` 仍为同步请求。每批最多 10 个文件，单文件最大 10 MiB。

相同内容与切分配置的重复上传会跳过；`add_document` 与异步 REST 上传不覆盖同名文档，替换请用 `update_document`。状态用 MCP `get_document_status` 或 REST `GET /v1/document-jobs/{artifact_id}` 轮询，终态为 `succeeded` / `failed`，状态在 MySQL，不依赖 Redis TTL。

### PDF 与 Word 解析

PDF 默认交给 **Docling 解析微服务**（`python-workers/pdf-service`，默认 `RAG_PDF_ENGINE=docling`）：版面分析、表格结构（行列/表头/合并单元格）、OCR 与公式 LaTeX 都在该服务内完成，Go 服务通过 HTTP 取回 zip（`manifest.json` + 图片资产）后再切分入库。该服务是独立镜像，需要单独构建/启动（`docker compose up -d pdf-parser`），并把版面/表格/OCR 模型准备到模型卷。

docling 引擎使用**知识库在 Console 中配置的 VLM 与 OCR 模型**：在「模型配置」页添加 VLM（OpenAI 兼容端点）和 OCR（引擎 `auto`/`easyocr`/`tesseract`/`rapidocr`，只需选择引擎），创建或编辑知识库时选择它们；上传 PDF 时 Go 服务把这两个配置随解析请求一并发给 pdf-parser（multipart 的 `options` 字段），因此不同知识库可以使用不同模型。知识库未配置 VLM 或 OCR 时，docling 引擎下的 PDF 上传会在**受理阶段**被拒绝，并提示先到「模型配置」页添加模型、在知识库配置中选择；VLM 仍支持 `RAG_VLM_*` 环境变量作为回退，OCR 没有环境回退。公式识别的本地 CodeFormula 权重只在知识库/环境都未配置 VLM 时才用到。

Go 服务保留**内置纯文本引擎**（`RAG_PDF_ENGINE=go`）：按页抽取正文、按基线分行、合并上标、去页眉页脚与页码、修复连字符换行，适合不需要版面/表格/公式的文字型 PDF，扫描件会明确报“没有可索引的内容”；切回纯文本不需要 Python 或任何外部服务。

切换引擎只需要环境变量（`docker compose` 场景写在 `.env`，宿主机直接运行 Go 服务时写在进程环境里）：

```ini
# 默认：调用 pdf-parser 微服务
RAG_PDF_ENGINE=docling
# 回退：使用 Go 内置纯文本引擎，不需要 Python/pdf-parser
RAG_PDF_ENGINE=go
```

Word 通过内置 DOCX 读取（DOC 可用 LibreOffice 转换），无图注图片由远端 VLM 端点生成描述（Go 直接调用 OpenAI 兼容接口）。解析产物（正文 Markdown、清单、图片、表格）保存在共享状态数据库，检索命中会返回图片 Base64、表格行列/表头/合并信息、图注与脚注，以及页码/版面或 Word 章节位置。

### 多种检索方式

| 方式 | 用途 |
| --- | --- |
| 关键词检索（BM25） | 查找型号、术语、故障码等明确词语。 |
| 语义检索（Embedding） | 按含义查找相关内容，不要求提问与原文用词完全相同。 |
| 混合检索（Hybrid） | 合并关键词与语义检索的候选结果。 |
| 结果重排（Reranker，可选） | 对候选内容再次排序，优先展示与问题更相关的片段。 |

REST `/v1/search`、Console 与本地调试同步执行。服务端 MCP `search` 在配置了 Kafka（`RAG_KAFKA_BOOTSTRAP_SERVERS`）时自动使用**排队模式**（schema 2 完整查询请求；`wait_seconds` 0..30，超时只返回 `result_id`，用 `get_search_result` 轮询），需要 Redis 与 `rag-search-worker`；未配置 Kafka 时 MCP 搜索退化为同步执行，便于本地开发。

### 可调整的文档切分

提供按字符、token、句子、递归和文档结构五种策略，可调整片段长度与重叠量。表格行按内容边界处理，超过模型输入上限时明确报错。

> Go 后端当前使用确定性 token 近似计数，而不是 BGE-M3 SentencePiece；句子/段落/结构边界与 Python 一致，但 token 预算下的切分位置可能与 Python 不完全相同。这是已知迁移差异，见 [docs/GO_PARITY_AUDIT.md](docs/GO_PARITY_AUDIT.md) 与 [docs/GO_MIGRATION.md](docs/GO_MIGRATION.md)。

### 接入 AI 助手与业务系统

同一个服务提供三种入口，无需另外部署前端服务：

| 入口 | 能力 |
| --- | --- |
| Web Console：`/console` | 账号登录/注册、角色与知识库权限管理、文档管理、检索调试、工作区切换、管理员「系统监控」只读页面，「模型配置」模型列表（Embedding / Reranker / VLM / OCR / JEV）与知识库配置中的模型快速选择，以及知识库 / API Key 的批量删除（复选或鼠标框选）。 |
| MCP：`/mcp` | 搜索、添加/更新/删除文档、分页读取正文、提示词模板及订阅变更。 |
| REST API：`/v1/*` | 知识管理与检索接入现有应用；`/openapi.json` 与 `/docs` 提供 OpenAPI 3.1 契约。 |

MCP 需要 `Authorization: Bearer <service token 或 API Key>` 与 `X-RAG-Namespace`；`/v1/admin/*` 管理接口仅允许本机直连或管理员会话，其中 `/v1/admin/api-keys` 是例外：任意已登录账号都可以管理**自己的** API Key（非管理员只能绑定本人创建的知识库，历史无主密钥仅管理员/本机管理可见）。管理员可以启停已注册的 MCP 工具、调整工具说明和管理提示词模板。

### 模型配置与知识库模型选择

管理员在侧边栏「模型配置」页（`#/model-profiles`）维护可复用的 Embedding / Reranker / VLM / OCR / JEV 模型列表：Embedding / Reranker / VLM 条目保存名称、服务地址、模型标识和加密后的 API Key，Embedding 条目还记录保存时探测到的向量维度；OCR 条目只选择引擎（`auto` 自动、`easyocr`、`tesseract`、`rapidocr`），没有服务地址与 Key，推理由 pdf-parser 在本地执行；JEV 条目（TypeSafe System One 决策模型）同样保存名称、服务地址、模型标识和加密后的 API Key，但保存时不做连接探测，也不出现在知识库表单中，当前仅做配置保存、供后续决策能力读取（常见模板：TypeSafe `https://api.typesafe.ai/v1`，模型 `jev-latest` / `jev-preview` / `jev-1.13.0`；OpenRouter `https://openrouter.ai/api/alpha`，模型 `typesafe/jev-1.13`）。创建或修改知识库时，管理员表单只从这个列表快速选择 Embedding / Reranker / VLM / OCR 模型，不再手工输入 Base URL / API Key / 模型名；切换 Embedding 到不同连接仍要求重新向量化。docling 引擎上传 PDF 要求知识库同时选中 VLM 与 OCR，否则上传会被拒绝并提示配置。模型配置与知识库之间是保存时复制的引用关系，之后编辑或删除模型配置不会改动已保存连接的知识库。VIP/普通用户建库表单不变，仍从 `RAG_DEFAULT_KB_TEMPLATE` 克隆模型配置（含 OCR 引擎）。

### 独立部署与模型状态

Qdrant 保存检索索引；源文件、PDF/Word 解析产物、异步文档任务与发件箱、UploadJournal、文档清单、知识库配置、Console 模型列表（`model_profiles`）、MCP 配置和 API Key 保存在共享状态数据库（MySQL）；排队搜索任务状态也在状态库，结果快照与展示缓存放 Redis；Kafka 传递异步通知和排队搜索请求。Embedding / Reranker 通过模型 API 调用，服务本身不加载模型权重。PDF 高保真解析由独立的 `pdf-parser` 容器加载 Docling/CodeFormula 模型（模型放在 `pdf_parser_models` 卷），Go 服务本身只加载纯文本引擎。

## 快速开始（Go 后端）

### 1. 准备

- **Go 1.27+**（`go.mod` 要求；构建服务与 worker 必需）
- **Node.js 20.19+ / 22.12+**（仅构建 Console；不构建前端时可跳过）
- **Qdrant**（HTTP 模式；Go 不支持 `QDRANT_PATH` 本地文件模式）
- **共享状态库**：MySQL 8（`utf8mb4`），必须配置 `RAG_STATE_URL` 或 `RAG_MYSQL_*`，不再提供本地文件回退
- **可选**：Redis（搜索快照/缓存）、Kafka（异步文档通知、排队搜索）、LibreOffice（仅 .doc 转换）
- **可选（PDF 高保真解析）**：Docling 解析微服务（`python-workers/pdf-service`，独立镜像，容器端口 8091）。默认 `docker compose up -d` 会随基础设施一起启动；首次使用前需要把 Docling/CodeFormula 模型准备到 `pdf_parser_models` 卷（先 `docling-tools models download --output-dir /models`，CodeFormulaV2 权重按 `python-workers/pdf-service/README.md` 单独准备），或配置 `RAG_VLM_BASE_URL` + `RAG_VLM_MODEL` 使用远端 VLM 做公式识别。docling 引擎下每个知识库还需在 Console「模型配置」页添加 VLM 与 OCR 模型，并在建库/编辑知识库时选中，否则 PDF 上传会被拒绝并提示配置。只用纯文本引擎时可以不启动它（`RAG_PDF_ENGINE=go`）
- **可选**：远端 OpenAI 兼容 VLM（`RAG_VLM_BASE_URL`/`RAG_VLM_API_KEY`/`RAG_VLM_MODEL`）用于描述 Word 无图注图片，并在 `RAG_PDF_ENGINE=docling` 时作为公式识别的环境回退（Console 知识库 VLM 设置优先）；未配置 VLM 时 Word 图片保留占位描述（上传不会失败），docling 引擎则因知识库/环境都没有 VLM 而在上传受理阶段被拒绝；配置了远端 VLM 后无需本地 CodeFormula 权重

本地依赖可直接用仓库的 `docker-compose.yml`：

```powershell
Copy-Item .env.example .env
# 编辑 .env：RAG_MYSQL_PASSWORD、MYSQL_ROOT_PASSWORD、RAG_STATE_ENCRYPTION_KEY 必填
docker compose up -d
```

默认启动基础设施（MySQL/Qdrant/Redis/Kafka）以及 PDF 解析微服务 `pdf-parser`，但不启动 Go 应用；Compose 的 `app` profile 构建并启动 Go 服务，见下文。只用 Go 纯文本引擎时，可以 `docker compose up -d mysql qdrant redis kafka` 跳过 pdf-parser，并在 `.env` 设 `RAG_PDF_ENGINE=go`。使用默认 docling 引擎上传 PDF 前，需要先把模型准备到 `pdf_parser_models` 卷或配置 `RAG_VLM_*`（否则 pdf-parser 对解析请求返回 503）；两者都不准备时请显式把引擎设为 `go`。

### 2. 构建

```powershell
# Windows（PowerShell 5.1+ / 7+）：前端 + Go 二进制 + 本机冒烟
pwsh -File scripts/build-go.ps1

# POSIX（需要 make 或 bash/curl）
make build
```

产物：

```text
dist/bin/rag-service.exe            主服务（REST + MCP + Console）
dist/bin/rag-document-worker.exe    异步文档 worker
dist/bin/rag-search-worker.exe      排队搜索 worker
dist/bin/rag-dead-letter-worker.exe 死信处理 worker
dist/bin/rag-service-migrate.exe    旧知识源数据迁移（默认预演）
dist/bin/rag-state-migrate.exe      共享状态表结构检查/创建
dist/bin/rag-mcp-debug.exe          无依赖的 mock 检索调试
dist/bin/rag-mcp-ingest.exe         本地文件/目录批量导入（走运行时上传管线）
dist/web/                           Console 构建产物（由服务通过 RAG_WEB_ROOT 读取）
```

POSIX 构建产物没有 `.exe` 后缀；下文的命令在 Windows 使用 `.exe`，在 Linux/macOS 去掉后缀即可。

常用开关：`-SkipFrontend`、`-SkipGo`、`-SmokeOnly`、`-Test`、`-FreshFrontendDeps`。`make` 提供 `frontend`、`go`、`build`、`test`、`vet`、`smoke`、`clean`。若只想用 API/MCP，可 `-SkipFrontend`，服务不提供 `/console`。

### 3. 启动服务

```powershell
# 可选：用环境变量覆盖默认配置（默认 Qdrant http://127.0.0.1:6333）
$env:QDRANT_URL = "http://127.0.0.1:6333"
$env:RAG_SERVICE_PORT = "8011"
# 生产 MySQL 示例（也可写入 ~/.praxis-rag/config.toml 的 [environment]）
$env:RAG_MYSQL_HOST = "127.0.0.1"
$env:RAG_MYSQL_PORT = "3306"
$env:RAG_MYSQL_USER = "rag"
$env:RAG_MYSQL_DATABASE = "praxis_rag"
$env:RAG_MYSQL_PASSWORD = "<password>"
$env:RAG_STATE_ENCRYPTION_KEY = "base64:<32-byte-key>"

.\dist\bin\rag-service.exe
```

首次启动会创建 `~/.praxis-rag/config.toml`（默认 `RAG_BACKEND=qdrant`、`127.0.0.1:8011`）；可用 `RAG_CONFIG_FILE` 指定其他 TOML。服务只接受 `RAG_BACKEND=qdrant`，`QDRANT_PATH` 本地文件模式不支持。

打开：

- Console：`http://127.0.0.1:8011/console`
- MCP：`http://127.0.0.1:8011/mcp`
- API 文档：`http://127.0.0.1:8011/docs`（机器可读：`/openapi.json`）
- 健康检查：`http://127.0.0.1:8011/health`

### 创建管理员账号

首次打开 Console 会显示登录页，切到"注册"创建账号，第一个账号自动成为管理员；之后可在"用户管理"页分配角色（管理员 `admin` / VIP `vip` / 普通用户 `user`）、启用/禁用或删除账号（始终保留至少一名启用状态的管理员）。自动化脚本继续使用 `RAG_SERVICE_TOKEN` 或 API Key，不受登录影响；也可以用 `RAG_AUTH_ENABLED=false` 关闭登录。

知识库默认归创建者私有：VIP 和管理员可以在「权限管理」页通过邀请链接把知识库按「可读 / 可修改」和「单文件 / 多文件 / 全库」范围共享给其他账号，被邀请人登录后接受邀请才生效。详见 [知识库权限管理](docs/PERMISSIONS.md)。详见 [docs/USAGE.md](docs/USAGE.md#用户登录与注册console)。

创建第一批 API Key（本机直连或管理员登录会话可为任意知识库创建；普通/VIP 账号登录后也可在「API Key」页创建自己的密钥，但只能绑定本人创建的知识库）：

```powershell
curl.exe -X POST http://127.0.0.1:8011/v1/admin/api-keys `
  -H "Content-Type: application/json" `
  -d '{"name":"console","namespaces":["default"]}'
```

响应中的 `key` 只出现一次；随后请求携带 `Authorization: Bearer <key>` 与 `X-RAG-Namespace: default`。

### 4. 启动 worker（按需）

```powershell
# Kafka 部署的异步上传（默认上传方式）与 MCP add_document 必需；
# 无 Kafka 的单进程部署由 rag-service 内嵌 worker 消费，无需单独启动。
# 需要 MySQL + Kafka（或 RAG_DOCUMENT_WORKER_LOCAL=1 本地 outbox 模式）
.\dist\bin\rag-document-worker.exe

# 服务端 MCP 排队搜索必需；需要 Redis + Kafka + MySQL
.\dist\bin\rag-search-worker.exe

# Kafka 死信：两个实例，分别消费显式 DLQ topic（document / search）
.\dist\bin\rag-dead-letter-worker.exe --kind document
.\dist\bin\rag-dead-letter-worker.exe --kind search
```

worker 消费语义：handler 返回成功即提交 Kafka offset；失败在同一进程内指数重试（默认 100ms 起、上限 5s），达到 `MaxAttempts` 后把原始消息写入对应 DLQ topic、执行终态回调再提交 offset，因此是至少一次投递。无 Kafka 时 `rag-service` 默认内嵌文档 worker 直接轮询 MySQL 发件箱（`RAG_DOCUMENT_WORKER_INPROCESS`，通常无需单独启动 worker）；如关闭内嵌 worker，独立文档 worker 可用 `RAG_DOCUMENT_WORKER_LOCAL=1` 轮询发件箱（开发/单进程部署；有 Kafka 时该变量被忽略）。详细说明见[使用与部署](docs/USAGE.md)与[部署指南](docs/DEPLOYMENT.md)。

Kafka 连接用 `RAG_KAFKA_BOOTSTRAP_SERVERS`（宿主 `127.0.0.1:9092`，compose 容器 `kafka:9092`），可选 `RAG_KAFKA_SECURITY_PROTOCOL` / `RAG_KAFKA_SASL_*` / `RAG_KAFKA_TLS_*`；队列默认 `praxis_state_cache_refresh`、`praxis_document_ingest`、`praxis_search`，死信用显式 `RAG_DOCUMENT_DLQ_TOPIC`（`praxis_document_ingest_dead_letters`）与 `RAG_SEARCH_DLQ_TOPIC`（`praxis_search_dead_letters`）及独立 `RAG_*_DLQ_GROUP`。`rag-service`、三个 worker 必须共享同一组 bootstrap 与 topic/group 配置。

### 5. Docker / Compose（可选）

```powershell
# 构建 Go 服务镜像（可加 --build-arg NPM_REGISTRY=... / GOPROXY=...）
docker build -t praxis-rag:local .

# 默认 compose：基础设施 + pdf-parser（Docling 解析微服务）
docker compose up -d

# 只启动 / 重新构建 PDF 解析微服务（首次构建含 CPU torch；模型需预置到模型卷）
docker compose up -d --build pdf-parser

# 启动 Go 服务（app profile；容器绑定 0.0.0.0，因此必须提供 RAG_SERVICE_TOKEN）
docker compose --profile app up -d --build
```

Go 镜像内包含 `/app/bin/` 下全部命令、`/app/web` Console 和 `/app/config.toml`，以非 root 用户运行，健康检查 `/health`。Go 镜像**不包含** Python、Docling 或 LibreOffice：Word/DOCX 解析和 VLM 调用（HTTP）内置于 Go 二进制，PDF 高保真解析由独立的 `pdf-parser` 镜像（`python-workers/pdf-service`）通过 HTTP 完成，只有 .doc 转换需要把 LibreOffice 挂载进容器并用 `RAG_WORD_CONVERTER_PATH` 指向。用纯文本引擎（`RAG_PDF_ENGINE=go`）时不需要 pdf-parser 镜像或模型（compose 的 `app` profile 因 `depends_on` 仍会拉起 pdf-parser，直接运行 Go 二进制或跳过 app profile 时不需要）。Docker/systemd 示例及未验证说明见[部署指南](docs/DEPLOYMENT.md)。

宿主机直接运行 Go 服务（不经 compose 的 app）时，用 compose 把 pdf-parser 发布到本机即可：设置 `RAG_PDF_SERVICE_URL=http://127.0.0.1:8091`（未设置时默认就是该地址），`RAG_PDF_ENGINE=docling`；需要切回纯文本时设 `RAG_PDF_ENGINE=go` 并停掉 pdf-parser。

## 运行时架构

```text
浏览器  --------------------------> http://127.0.0.1:8011/console
MCP 客户端 -> /mcp（Streamable HTTP）-> rag-service
REST /v1/* ------------------------> rag-service（同步搜索/管理/状态；上传默认异步受理）
rag-service -> Qdrant（检索索引）、共享状态库（文档/KB/Key/任务/发件箱）、
               Redis（读模型缓存 + 搜索快照；可选）、Kafka（outbox relay/搜索队列；可选）
网页/REST 上传（默认）与 MCP add_document
                 -> 状态库单事务写入 document_jobs + document_job_outbox，返回 artifact_id
                 -> 有 Kafka：relay（rag-service 内置）-> Kafka 通知 -> rag-document-worker
                 -> 无 Kafka：rag-service 内嵌 worker 直接消费发件箱
                 -> 两种方式都完成解析/切分/Embedding/Qdrant 发布，用
                    GET /v1/document-jobs/{artifact_id} 轮询 status/stage
REST 上传（RAG_UPLOAD_MODE=sync）-> 请求内解析/切分/Embedding/发布，返回 imported/skipped/failed/documents
PDF 解析（默认 RAG_PDF_ENGINE=docling）-> pdf-parser :8091（HTTP，返回 zip：manifest.json + 图片资产）
MCP search（配置 Kafka 时）-> search_task_outbox -> Kafka -> rag-search-worker -> SearchRaw + Redis 快照
```

- PDF 解析引擎可切换：默认 `docling` 走独立的 `pdf-parser` 微服务（版面/表格/OCR/公式），`go` 使用 Go 进程内纯文本引擎；`docling` 引擎下知识库未配置 VLM/OCR 时，PDF 上传会在受理阶段被拒绝并提示去「模型配置」页面配置（不进入队列）；pdf-parser 不可用或超时同样会让该次上传失败，可显式设 `RAG_PDF_ENGINE=go` 回退。
- 搜索 worker、死信 worker 与 Kafka 模式的文档 worker 是**独立进程**；`rag-service` 只在无 Kafka 时内嵌文档 worker（`RAG_DOCUMENT_WORKER_INPROCESS`，默认 auto），Kafka 模式不内嵌消费者。
- `rag-service` 在配置 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时启用 in-process outbox relay 和排队搜索；未配置时 MCP 搜索同步，默认由内嵌文档 worker 直接消费发件箱（`RAG_DOCUMENT_WORKER_INPROCESS=0` 可关闭并改跑独立 worker），没有消费者时任务保持 `queued`。
- **`RAG_OUTBOX_BRIDGE` 支持 `relay`（默认）与 `debezium`**：`relay` 由服务进程内发布发件箱；`debezium` 时服务**不发布**，由外部 Debezium Server 读取 MySQL binlog 投递。Debezium 部署链路已在真实 MySQL 8.0 + Debezium 3.7.1 + Kafka 3.9.0 上验证；Go 服务/worker 的完整异步链路也已实机跑通（debezium 与 relay 两种模式；脚本 `scripts/verify-debezium-e2e.py`），死信与排队搜索分别由 `scripts/verify-kafka-dlq.py`、`scripts/verify-kafka-search.py` 实机验证。两种模式不能同时启用，否则会重复投递。

## 迁移状态

- **Go 是目标后端**：`rag-service`、三个 worker、Console、OpenAPI、迁移/状态 CLI 与本地批量导入 CLI 均已落地；parity 审计的 P0（搜索资产投影、MCP 排队/同步选择、worker 可执行程序、已保存 `document.md` 预览）与主要 P1 已修复，`go build ./...` 与全量 `go test ./...` 通过。
- **Python 后端已删除**：原 `src/rag_mcp`、`tests`、`pyproject.toml`、`uv.lock` 已移除；`python-workers/` 曾整体删除，2026-11-10 起以**独立 Docling PDF 解析微服务**（`python-workers/pdf-service`，仅 PDF、HTTP + zip 清单，清单版本 `docling-service-v1`）的形式恢复，不进入 Go 构建/镜像/测试依赖。PDF 默认调用该服务，也可切回 Go 内置纯文本引擎；DOCX 读取与 Word 图片描述在 Go 进程内完成；只有 .doc 转换需要外部 LibreOffice，VLM 通过 HTTP 调用。
- **数据兼容**：Go 复用既有 MySQL 表结构、`enc:v1:` 凭据格式和配置文件位置（`~/.praxis-rag/config.toml`），不需要数据迁移；状态表用 `rag-state-migrate` 检查/建表，旧知识源用 `rag-service-migrate` 预览/导入（见已知差异）。
- **已实机验证**：Kafka 3.9.0 + Debezium 3.7.1 + MySQL 8.0 + Redis + Qdrant 上的完整异步链路（debezium 与 relay 两种模式）、显式 DLQ topic、排队 MCP 搜索（search worker + Redis 快照）；脚本见 `scripts/verify-debezium-e2e.py`、`scripts/verify-kafka-dlq.py`、`scripts/verify-kafka-search.py`。
- **仍待验证**（以 [GO_MIGRATION](docs/GO_MIGRATION.md) 为准）：Kafka SASL/TLS 外部集群、Docker 镜像/Compose `app` profile、pdf-parser 镜像与真实 Docling/CodeFormula 推理、systemd、真实 MySQL 多实例并发/故障切换、浏览器 E2E；黑盒 parity 差分改为固定预期（Python 对照已删除）。真实环境冒烟与未验证项见[使用与部署](docs/USAGE.md) 的验证边界。

## 测试

```powershell
# 后端：一次性 MySQL schema（库名必须含 test），所有包共用 schema，必须串行
$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:change-me@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'
go vet ./...
go test -p 1 -count=1 -timeout 25m ./...

# 前端：类型检查 + 单元/契约测试 + 构建
cd frontend
npm ci
npm test
npm run build
```

也可以 `pwsh -File scripts/build-go.ps1 -Test`（构建 + 串行测试 + 冒烟）或 `make check`（vet + Go 测试 + 前端 typecheck/测试/构建）。CI 工作流见 `.github/workflows/test.yml`，详细说明见[使用与部署](docs/USAGE.md)的「开发与测试」。

## 使用边界

- 异步受理（网页/REST 上传默认方式与 MCP `add_document`）的受理点是状态库单事务提交（任务 + 发件箱），不是 Kafka 确认；解析/索引在后台进行，用 `GET /v1/document-jobs/{artifact_id}` 观察进度。MCP 更新/删除与 `RAG_UPLOAD_MODE=sync` 的上传仍在请求内完成，索引期间会等待。
- `RAG_UPLOAD_MODE` 默认 `async`，必须有一个文档消费者：无 Kafka 的单进程部署由 `rag-service` 内嵌 worker 覆盖（`RAG_DOCUMENT_WORKER_INPROCESS` 默认 auto）；Kafka 部署运行 `rag-document-worker`（`scripts/start-stack.ps1` 会自动启动），或显式设置 `RAG_UPLOAD_MODE=sync` 保留请求内完成的旧行为。
- 每批最多 10 个文件、单文件 10 MiB；暂不支持 Excel、网页抓取和独立图片上传。
- Word 保留内容控件、嵌套表格、合并范围与原生公式；未知公式结构、原生图表等无法可靠读取的内容会明确拒绝入库；图注关联有歧义时跳过并报告原因。
- 知识库（namespace）默认归创建者私有，只有创建者和管理员可以使用；共享通过「权限管理」页的邀请链接完成，权限分为可读/可修改、范围分为单文件/多文件/全库，仅 VIP 与管理员可邀请。管理凭证（Service Token / API Key / 本机无会话直连）沿用原有语义，不做知识库归属校验；`/v1/admin/*` 默认只允许本机直连或管理员会话，其中 `/v1/admin/api-keys` 例外：任意已登录账号都可以管理自己的 API Key（非管理员只能绑定本人知识库，历史无主密钥仅管理员/本机可见）。MCP/普通 API 使用 Service Token 或 API Key。详见 [知识库权限管理](docs/PERMISSIONS.md)。
- 默认只监听 `127.0.0.1`。远程部署必须设置 `RAG_SERVICE_TOKEN` 与 MCP Host/Origin 白名单，并在前面配置 HTTPS 与访问控制。
- PDF 解析引擎默认是 `docling`：Go 服务调用独立的 `pdf-parser` 微服务（版面分析、表格结构、OCR、公式 LaTeX；公式识别依赖 `RAG_VLM_*` 远端 VLM 或模型卷内的本地 CodeFormula）。该服务未启动或超时（默认 900 秒）时 docling 上传会失败，可显式 `RAG_PDF_ENGINE=go` 切回 Go 内置纯文本引擎（文字型 PDF；不含版面/表格/OCR/公式与图片资产，扫描件请先 OCR）。Word 无图注图片可调用远端 VLM（`RAG_VLM_*`），文档裁图会发送到该端点；未配置 VLM 时图片保留占位描述。.doc 转换依赖外部 LibreOffice。
- Redis 搜索快照有 TTL（`RAG_RESULT_TTL_SECONDS`，默认 3600 秒），丢失或到期后 `result_id` 不可用，无 SQL 回放。异步文档任务状态在 MySQL，不受 Redis TTL 影响。
- 日志仅记录任务/操作 ID 和安全错误类别，不记录查询、文件内容、消息正文或凭据。

## 项目文档

| 文档 | 内容 |
| --- | --- |
| [使用与部署](docs/USAGE.md) | Go 后端的构建、运行、环境变量、REST/MCP 示例、worker、迁移 CLI。 |
| [知识库权限管理](docs/PERMISSIONS.md) | 角色、知识库归属、可读/可修改与单文件/多文件/全库权限、邀请链接流程与接口。 |
| [部署指南](docs/DEPLOYMENT.md) | Docker/Compose 与 systemd 示例、worker 重启策略、DLQ 监控、outbox 模式。 |
| [Go 迁移总纲](docs/GO_MIGRATION.md) | 目标架构、兼容性硬约束、阶段与进度记录、当前切换状态。 |
| [Go Parity 审计](docs/GO_PARITY_AUDIT.md) | Go 与 Python/前端契约差异、P0/P1/P2 清单与验证限制。 |
| [共享状态存储](docs/state-storage.md) | MySQL 表结构、密钥加密、迁移与多实例注意事项。 |
| [状态缓存](docs/state-cache.md) | Redis 读模型缓存、修订与 outbox、缓存一致性边界。 |
| [UUID 结果与产物引用](docs/result-references.md) | result_id、快照 TTL、artifact 引用与容量限制。 |
| [验证记录](VALIDATION.md) | 已执行的验证及其覆盖范围。 |
| [历史开发记录](PROGRESS.md) | 项目早期开发阶段记录，不作为当前功能清单。 |

产品名为 **Praxis-RAG**，Go module 为 `github.com/Guan0923/xinghe_rag`，Python 安装包名为 `praxis-rag`、内部模块仍为 `rag_mcp`。REST 路径、MCP 工具名、环境变量与数据位置尽量保持与 Python 实现一致；差异以 [Go Parity 审计](docs/GO_PARITY_AUDIT.md) 为准。

