# Praxis-RAG

**面向 AI 助手和业务应用的独立知识库与检索服务。Go 后端 + Web Console。**

Praxis-RAG 把分散的文档整理为可搜索、可追溯的知识库。你可以在网页中上传和管理资料，比较检索结果，也可以让 AI 助手通过 MCP 连接知识库，查找资料、读取正文和维护文档。

当前对外运行的后端是 **Go 实现**（`internal/`、`cmd/`）：一个 `rag-service` 进程提供 REST、MCP 和 Console，另提供文档、搜索、缓存、死信四个独立 worker。Python 主后端与旧 Python 解析 worker 已从仓库删除；PDF 高保真解析恢复为独立的 **PDF 视觉解析微服务**（`python-workers/pdf-service`，本地只做 PDF 读取/渲染/裁图与结果组装，版面/OCR/表格/公式/图片描述统一调用远端 OpenAI 兼容视觉模型；Go 通过 HTTP 调用，不进入 Go 进程），Go 服务本身仍无 Python 依赖，也可用 `RAG_PDF_ENGINE=go` 切回内置纯文本引擎；Word/DOCX 解析与 Word 图片描述仍在 Go 进程内完成（图片描述调用同一个远端 VLM）。迁移历史见 [迁移状态](#迁移状态) 与 [docs/GO_MIGRATION.md](docs/GO_MIGRATION.md)。

它负责提供资料和出处，**不直接生成最终回答**。如何提问、是否继续查找、如何组织答案，由接入的 AI 助手或业务应用决定。

## 适用场景

- **产品与售后知识库**：从手册、操作说明和故障记录中查找处理步骤，并返回对应资料。
- **团队文档检索**：按工作区整理项目资料，让网页、AI 助手和业务系统访问同一份知识库。
- **研究资料查阅**：检索 PDF/Word 正文，再对照原文核查。
- **检索方案调试**：比较关键词检索、语义检索和混合检索，调整切分方式与结果排序。

## 主要功能

### 文档导入与管理

支持 Markdown、TXT、PDF、DOCX，以及安装 LibreOffice 后的 DOC。网页/REST 上传默认异步受理：每个文件在共享状态数据库（MySQL）的一个事务内写入完整任务（`document_jobs`），**提交成功即返回 `artifact_id`**，解析与索引由独立的 `rag-document-worker` 在后台完成（任务表由 Debezium 采集，需要 Kafka + Debezium；否则任务保持 `queued`），前端用 `GET /v1/document-jobs/{artifact_id}` 轮询 `status`/`stage`。Console 使用批次接口 `POST /v1/document-batches`：每个文件一行独立进度条，上传时通过 SSE（`GET /v1/document-batches/upload-events/{upload_id}`，POST 同时带可选的 `X-Upload-ID` 头）实时推送该文件累计接收字节数（前端按本地文件大小换算百分比），每个文件保存成功/失败/跳过时推送该文件的保存结果（已保存/保存失败/已跳过）；上传面板在保存结果处结束，**不再展示或轮询解析进度**。点上传后弹窗立即关闭，工具栏按钮缩成"保存中/已完成 x/y"的徽标，点开是逐文件进度气泡（每文件进度条 + 保存结果 + 失败重试），刷新或切页后从 `GET /v1/document-batches?limit=20` 恢复；上传期间允许切换 Console 页面，工作区切换仍锁定。`X-Upload-ID` 可省略：省略时不推送事件，POST 响应与旧行为完全一致。MCP `add_document` 始终异步；MCP `update_document`、`delete_document` 仍为同步请求。每批最多 10 个文件，单文件最大 10 MiB。

相同内容与切分配置的重复上传会跳过；`add_document` 与异步 REST 上传不覆盖同名文档，替换请用 `update_document`。状态用 MCP `get_document_status` 或 REST `GET /v1/document-jobs/{artifact_id}` 轮询，终态为 `succeeded` / `failed`，状态在 MySQL，不依赖 Redis TTL。

知识文档页还提供「从飞书导入」：用户先用自己的飞书账号完成 OAuth 授权，然后按文件夹浏览或搜索自己有权限访问的飞书文档，勾选后一次导入当前知识库（单次最多 10 篇）。浏览、搜索和读取正文都使用该用户的 `user_access_token`，不会越权读取；导入只取新版飞书文档（docx）的纯文本，复用普通上传的知识库写权限校验、SHA-256 去重、同名跳过和异步索引，逐篇返回「已受理 / 已跳过 / 失败」并在后台轮询索引状态，只有索引成功后才可检索。导入后的副本属于目标知识库，可见范围由 Praxis-RAG 的知识库权限决定，与飞书原文档的分享范围无关。部署方需要配置 `RAG_FEISHU_APP_ID`、`RAG_FEISHU_APP_SECRET` 和 `RAG_FEISHU_REDIRECT_URI`，并在飞书应用申请 `drive:drive:readonly`、`docx:document:readonly`、`offline_access` 权限；未配置时入口会说明原因。文件列表中的旧版飞书文档（doc）、表格和附件会标记为不支持；搜索命中的旧版或不支持文档会作为该篇的失败原因返回，不影响同批其他文档。

### PDF 与 Word 解析

PDF 默认交给 **PDF 视觉解析微服务**（`python-workers/pdf-service`，默认 `RAG_PDF_ENGINE=vision`，旧值 `docling` 作为兼容别名）：版面分析、表格结构（行列/表头/合并单元格）、OCR、公式 LaTeX 与图片描述都在该服务内通过**远端 OpenAI 兼容视觉模型**完成；服务本地只用 PyMuPDF 读取/渲染/裁图并组装清单，不加载任何模型权重。Go 服务通过 HTTP 取回 zip（`manifest.json` + 图片资产）后再切分入库。该服务是独立镜像，需要单独构建/启动（`docker compose up -d pdf-parser`），只需能访问配置好的视觉模型端点。

vision 引擎使用**知识库在 Console 中配置的 VLM（视觉模型）**：在「模型配置」页添加 VLM（OpenAI 兼容端点的服务地址、模型名与 API Key），创建或编辑知识库时选择它；上传 PDF 时 Go 服务把这个配置随解析请求一并发给 pdf-parser（multipart 的 `options` 字段，键名 `vision`），因此不同知识库可以使用不同视觉模型。知识库未配置 VLM 时，vision 引擎下的 PDF 上传会在**受理阶段**被拒绝，并提示
`PDF 解析需要先在「模型配置」页添加视觉模型（VLM），并在知识库配置中选择后再上传。`；VLM 仍支持 `RAG_VLM_*` 环境变量作为回退。OCR 已并入视觉模型，不再有独立的 OCR 引擎配置。

Go 服务保留**内置纯文本引擎**（`RAG_PDF_ENGINE=go`）：按页抽取正文、按基线分行、合并上标、去页眉页脚与页码、修复连字符换行，适合不需要版面/表格/公式的文字型 PDF，扫描件会明确报“没有可索引的内容”；切回纯文本不需要 Python 或任何外部服务。

切换引擎只需要环境变量（`docker compose` 场景写在 `.env`，宿主机直接运行 Go 服务时写在进程环境里）：

```ini
# 默认：调用 pdf-parser 微服务（远端视觉模型）
RAG_PDF_ENGINE=vision
# 兼容别名：旧部署里的 docling 等价于 vision
# RAG_PDF_ENGINE=docling
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

REST `/v1/search`、MCP `search`、Console 与本地调试**全部同步执行**：一次调用完成检索 + 重排并直接返回证据（`results`/`artifacts`/`evidence_status`/`answer_guidance`）。没有任务 ID、没有 `wait_seconds`、没有轮询——`result_id`、`get_search_result` 与搜索结果快照已随排队搜索删除。总预算由 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）约束，客户端断开即取消下游检索/模型调用。受限账号同样走同步检索，服务端按文档 scope 追加过滤。

### 可调整的文档切分

提供按字符、token、句子、递归和文档结构五种策略，可调整片段长度与重叠量。表格行按内容边界处理，超过模型输入上限时明确报错。

> Go 后端当前使用确定性 token 近似计数，而不是 BGE-M3 SentencePiece；句子/段落/结构边界与 Python 一致，但 token 预算下的切分位置可能与 Python 不完全相同。这是已知迁移差异，见 [docs/GO_PARITY_AUDIT.md](docs/GO_PARITY_AUDIT.md) 与 [docs/GO_MIGRATION.md](docs/GO_MIGRATION.md)。

### 接入 AI 助手与业务系统

同一个服务提供三种入口，无需另外部署前端服务：

| 入口 | 能力 |
| --- | --- |
| Web Console：`/console` | 账号登录/注册（可配置邮箱验证码）、个人信息（头像 / 年龄 / 性别 / 用户名 / 邮箱 / 密码）、角色与知识库权限管理、文档管理、检索调试、工作区切换、管理员「系统设置」邮件服务器（SMTP）与「系统监控」只读页面，「模型配置」模型列表（Embedding / Reranker / VLM / JEV）与知识库配置中的模型快速选择，以及知识库 / API Key 的批量删除（复选或鼠标框选）。 |
| MCP：`/mcp` | 搜索、添加/更新/删除文档、分页读取正文、提示词模板及订阅变更。 |
| REST API：`/v1/*` | 知识管理与检索接入现有应用；`/openapi.json` 与 `/docs` 提供 OpenAPI 3.1 契约。 |

MCP 需要 `Authorization: Bearer <service token 或 API Key>` 与 `X-RAG-Namespace`；`/v1/admin/*` 管理接口仅允许本机直连或管理员会话，其中 `/v1/admin/api-keys` 是例外：任意已登录账号都可以管理**自己的** API Key（非管理员只能绑定本人创建的知识库，历史无主密钥仅管理员/本机管理可见）。管理员可以启停已注册的 MCP 工具、调整工具说明和管理提示词模板。

### 模型配置与知识库模型选择

管理员在侧边栏「模型配置」页（`#/model-profiles`）维护可复用的 Embedding / Reranker / VLM / JEV 模型列表：Embedding / Reranker / VLM 条目保存名称、服务地址、模型标识和加密后的 API Key，Embedding 条目还记录保存时探测到的向量维度；VLM 是唯一的远端 OpenAI 兼容视觉模型，PDF 的版面/OCR/表格/公式/图片描述统一由它完成；JEV 条目（TypeSafe System One 决策模型）同样保存名称、服务地址、模型标识和加密后的 API Key，但保存时不做连接探测，也不出现在知识库表单中；检索模式 `sag2`（词法多跳：BM25 + JEV 必要证据判定 + 词项扩展）会在每次查询时读取**最近更新**的 JEV 配置调用云端决策接口（常见模板：TypeSafe `https://api.typesafe.ai/v1`，模型 `jev-latest` / `jev-preview` / `jev-1.13.0`；OpenRouter `https://openrouter.ai/api/alpha`，模型 `typesafe/jev-1.13`）。创建或修改知识库时，管理员表单只从这个列表快速选择 Embedding / Reranker / VLM 模型，不再手工输入 Base URL / API Key / 模型名；切换 Embedding 到不同连接仍要求重新向量化。vision 引擎上传 PDF 要求知识库选中 VLM，否则上传会被拒绝并提示配置。模型配置与知识库之间是保存时复制的引用关系，之后编辑或删除模型配置不会改动已保存连接的知识库。升级时一次性迁移会删除旧的 `kind='ocr'` 模型配置并清除知识库中的遗留 `ocr`/`ocr_model` 设置。VIP/普通用户建库表单不变，仍从 `RAG_DEFAULT_KB_TEMPLATE` 克隆模型配置。

### 独立部署与模型状态

Qdrant 保存检索索引；源文件、PDF/Word 解析产物、异步文档任务、UploadJournal、文档清单、知识库配置、Console 模型列表（`model_profiles`）、MCP 配置、API Key 与 artifact 快照保存在共享状态数据库（MySQL）；读模型展示缓存放 Redis（可选）。Kafka 只传递通知（文档任务、缓存变更两类事件，均只含定位标识），由运行在分布式 Kafka Connect 上的两个 Debezium 连接器从 MySQL 表级采集；搜索为同步执行，不经过 Kafka。Embedding / Reranker 通过模型 API 调用，服务本身不加载模型权重。PDF 高保真解析由独立的 `pdf-parser` 容器通过 HTTP 调用远端视觉模型完成（本地不加载任何模型权重），Go 服务本身只加载纯文本引擎。

## 快速开始（Go 后端）

### 1. 准备

- **Go 1.27+**（`go.mod` 要求；构建服务与 worker 必需）
- **Node.js 20.19+ / 22.12+**（仅构建 Console；不构建前端时可跳过）
- **Qdrant**（HTTP 模式；Go 不支持 `QDRANT_PATH` 本地文件模式）
- **共享状态库**：MySQL 8（`utf8mb4`），必须配置 `RAG_STATE_URL` 或 `RAG_MYSQL_*`，不再提供本地文件回退
- **文档处理必需**：Kafka + 分布式 Kafka Connect（其中注册两个 Debezium MySQL 连接器：文档连接器只采集 `document_jobs`，缓存连接器采集业务表）；未配置时上传仍可受理，但文档任务保持 `queued`、缓存刷新不生效。搜索为同步执行，不需要 Kafka。
- **可选**：Redis（读模型展示缓存，可禁用）、LibreOffice（仅 .doc 转换）
- **可选（PDF 高保真解析）**：PDF 视觉解析微服务（`python-workers/pdf-service`，独立镜像，容器端口 8091）。默认 `docker compose up -d` 会随基础设施一起启动；它本地不加载模型，只需能访问远端 OpenAI 兼容视觉模型。vision 引擎下每个知识库还需在 Console「模型配置」页添加 VLM 模型，并在建库/编辑知识库时选中，否则 PDF 上传会被拒绝并提示配置；也可用 `RAG_VLM_*` 作为服务级回退。只用纯文本引擎时可以不启动它（`RAG_PDF_ENGINE=go`）
- **可选**：远端 OpenAI 兼容视觉模型（`RAG_VLM_BASE_URL`/`RAG_VLM_API_KEY`/`RAG_VLM_MODEL`）同时用于 PDF 视觉解析与 Word 无图注图片描述，`RAG_PDF_ENGINE=vision` 时作为知识库 VLM 设置的环境回退（Console 知识库 VLM 设置优先）；未配置 VLM 时 Word 图片保留占位描述（上传不会失败），vision 引擎则因知识库/环境都没有 VLM 而在上传受理阶段被拒绝

本地依赖可直接用仓库的 `docker-compose.yml`：

```powershell
Copy-Item .env.example .env
# 编辑 .env：RAG_MYSQL_PASSWORD、MYSQL_ROOT_PASSWORD、RAG_STATE_ENCRYPTION_KEY 必填
docker compose up -d
```

默认启动基础设施（MySQL/Qdrant/Redis/Kafka）以及 PDF 解析微服务 `pdf-parser`，但不启动 Go 应用；Compose 的 `app` profile 构建并启动 Go 服务，见下文。只用 Go 纯文本引擎时，可以 `docker compose up -d mysql qdrant redis kafka` 跳过 pdf-parser，并在 `.env` 设 `RAG_PDF_ENGINE=go`。使用默认 vision 引擎上传 PDF 前，需要在 Console 为知识库选择视觉模型（VLM）或配置 `RAG_VLM_*`（否则 pdf-parser 对解析请求返回 503，Go 端也会在受理阶段拒绝未配置 VLM 的知识库上传）；两者都不准备时请显式把引擎设为 `go`。

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
dist/bin/rag-cache-worker.exe       缓存变更消费者（praxis-cache-cdc 投递）
dist/bin/rag-dead-letter-worker.exe 死信处理 worker
dist/bin/rag-service-migrate.exe    旧知识源数据迁移（默认预演）/ 旧表下线
dist/bin/rag-cdc-cutover.exe        CDC 存量切换（盘点/记录位点/补投/缓存基线/核对）
dist/bin/rag-connect.exe            Kafka Connect 注册/等待/状态/插件/FAILED task 监督（standalone）
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

管理员可在「系统设置」页配置 SMTP 邮件服务器（主机、端口、TLS 模式、用户名/密码、发件人）并发送测试邮件。配置并启用后，注册页要求填写邮箱并通过邮件验证码注册，修改邮箱同样走验证码；未配置邮件服务器时注册仍按用户名+密码进行，邮箱可以在登录后的「个人信息」中补充（不会标记为已验证）。登录后点右上角账号菜单的「个人信息」可上传头像（PNG/JPEG/WEBP/GIF，≤2MB）、设置年龄与性别、修改用户名、邮箱或密码。详见 [docs/USAGE.md](docs/USAGE.md#用户登录与注册console)。

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
# 异步上传（默认上传方式）与 MCP add_document 必需；
# 本进程是唯一的文档消费者，Kafka 通知由 Kafka Connect 的 praxis-document-cdc 采集 document_jobs 表投递。
# 需要 MySQL + Kafka；未运行时上传仍受理，但文档任务保持 queued。
.\dist\bin\rag-document-worker.exe

# 缓存变更消费者：消费 praxis-cache-cdc 投递的业务表变更，按 revision 水位刷新 Redis；
# 未运行时缓存最终一致会延后，由周期 RefreshDirty 补偿
.\dist\bin\rag-cache-worker.exe

# Kafka 死信：两个实例，分别消费显式 DLQ topic（document / cache）
.\dist\bin\rag-dead-letter-worker.exe --kind document
.\dist\bin\rag-dead-letter-worker.exe --kind cache
```

worker 消费语义：handler 返回成功即提交 Kafka offset；失败在同一进程内指数重试（默认 100ms 起、上限 5s），达到 `MaxAttempts` 后把原始消息写入对应 DLQ topic、执行终态回调再提交 offset，因此是至少一次投递。每个 worker 默认单条串行处理，`RAG_DOCUMENT_WORKER_CONCURRENCY` 可让单个 worker 进程并发处理多条消息；并发时仍按分区"连续完成前缀"提交 offset，未提交记录由幂等处理吸收重投。文档执行次数另由 MySQL `document_jobs.attempts` 记录（跨消息/worker/重启，上限 3）：第 1、2 次可重试失败保存原因与 `retry_at`，到期由后台扫描发出下一轮通知；第 3 次失败直接写 `failed`，不会启动第 4 次。文档 worker 只通过 Kafka 消费：`rag-service` 只受理上传、从不解析文档；未配置 Kafka 或未运行 `rag-document-worker` 时任务保持 `queued`。详细说明见[使用与部署](docs/USAGE.md)与[部署指南](docs/DEPLOYMENT.md)。

Kafka 连接用 `RAG_KAFKA_BOOTSTRAP_SERVERS`（宿主 `127.0.0.1:9092`，compose 容器 `kafka:9092`），可选 `RAG_KAFKA_SECURITY_PROTOCOL` / `RAG_KAFKA_SASL_*` / `RAG_KAFKA_TLS_*`；两类事件各自独立 topic/消费组：文档 `praxis_document_ingest`、缓存 `praxis_state_cache_refresh`；死信用显式 `RAG_DOCUMENT_DLQ_TOPIC`、`RAG_CACHE_DLQ_TOPIC` 及独立 `RAG_*_DLQ_GROUP`。原搜索 topic/消费组/DLQ（`praxis_search`、`RAG_SEARCH_DLQ_*`）已随排队搜索下线。`rag-service`、三个 worker 必须共享同一组 bootstrap 与 topic/group 配置。

### 5. Docker / Compose（可选）

```powershell
# 构建 Go 服务镜像（可加 --build-arg NPM_REGISTRY=... / GOPROXY=...）
docker build -t praxis-rag:local .

# 默认 compose：基础设施 + pdf-parser（PDF 视觉解析微服务）
docker compose up -d

# 只启动 / 重新构建 PDF 解析微服务（纯 Python + PyMuPDF，不再安装本地推理依赖）
docker compose up -d --build pdf-parser

# 启动 Go 服务（app profile；容器绑定 0.0.0.0，因此必须提供 RAG_SERVICE_TOKEN）
docker compose --profile app up -d --build
```

Go 镜像内包含 `/app/bin/` 下全部命令、`/app/web` Console 和 `/app/config.toml`，以非 root 用户运行，健康检查 `/health`。Go 镜像**不包含** Python 或 LibreOffice：Word/DOCX 解析和 VLM 调用（HTTP）内置于 Go 二进制，PDF 高保真解析由独立的 `pdf-parser` 镜像（`python-workers/pdf-service`）通过 HTTP 完成，只有 .doc 转换需要把 LibreOffice 挂载进容器并用 `RAG_WORD_CONVERTER_PATH` 指向。用纯文本引擎（`RAG_PDF_ENGINE=go`）时不需要 pdf-parser 镜像（compose 的 `app` profile 因 `depends_on` 仍会拉起 pdf-parser，直接运行 Go 二进制或跳过 app profile 时不需要）。Docker/systemd 示例及未验证说明见[部署指南](docs/DEPLOYMENT.md)。

宿主机直接运行 Go 服务（不经 compose 的 app）时，用 compose 把 pdf-parser 发布到本机即可：设置 `RAG_PDF_SERVICE_URL=http://127.0.0.1:8091`（未设置时默认就是该地址），`RAG_PDF_ENGINE=vision`；需要切回纯文本时设 `RAG_PDF_ENGINE=go` 并停掉 pdf-parser。

## 运行时架构

```text
浏览器  --------------------------> http://127.0.0.1:8011/console
MCP 客户端 -> /mcp（Streamable HTTP）-> rag-service
REST /v1/* ------------------------> rag-service（同步搜索/管理/状态；上传默认异步受理）
rag-service -> Qdrant（检索索引）、共享状态库（文档/KB/Key/任务/artifact/缓存水位）、
               Redis（读模型展示缓存；可选）、Kafka（Debezium 文档/缓存通知；文档处理必需）
网页/REST 上传（默认）与 MCP add_document
                 -> 状态库单事务写入 document_jobs（schedule_version=1），返回 artifact_id
                 -> 有 Kafka：Kafka Connect 文档连接器（表级采集）-> Kafka 通知 -> rag-document-worker
                 -> 无 Kafka：没有消费者，文档任务保持 queued（rag-service 只受理）
                 -> rag-document-worker 完成解析/切分/Embedding/Qdrant 发布，用
                    GET /v1/document-jobs/{artifact_id} 轮询 status/stage
PDF 解析（默认 RAG_PDF_ENGINE=vision）-> pdf-parser :8091（HTTP，返回 zip：manifest.json + 图片资产）-> 远端视觉模型（版面/OCR/表格/公式/图片描述）
MCP search / REST /v1/search（同步）-> runtime.Service.Search -> 检索 + 重排 + artifact 投影
                 -> 同一次响应返回 results / artifacts / evidence_status / answer_guidance
                 -> 无任务 ID、无轮询、无搜索 topic；总预算 RAG_SEARCH_TIMEOUT_SECONDS
业务表变更（含手工 SQL）-> Kafka Connect 缓存连接器 -> Kafka -> rag-cache-worker
                 -> cache_revisions.revision+1 -> 重建并写 Redis（失败由 RefreshDirty 收敛）
```

- PDF 解析引擎可切换：默认 `vision` 走独立的 `pdf-parser` 微服务（版面/OCR/表格/公式/图片描述统一由远端视觉模型完成），`go` 使用 Go 进程内纯文本引擎；`vision` 引擎下知识库未配置 VLM 时，PDF 上传会在受理阶段被拒绝并提示去「模型配置」页面配置（不进入队列）；pdf-parser 不可用或超时同样会让该次上传失败，可显式设 `RAG_PDF_ENGINE=go` 回退。旧值 `docling` 作为兼容别名等价于 `vision`。
- 文档、缓存两个消费者与死信 worker 都是**独立进程**；`rag-service` 只受理上传与后台维护，不内嵌任何消费者。搜索在 `rag-service` 内同步执行，不需要 worker。
- **两类事件、两个 Debezium 连接器**：`praxis-document-cdc` 只采集 `document_jobs`，且只投递 INSERT 与 `schedule_version` 递增的 UPDATE（`skip.messages.without.change`）；`praxis-cache-cdc` 采集 8 张影响读模型的业务表并投递全部 c/u/d。二者运行在同一个分布式 Kafka Connect 集群（生产至少两个 worker），使用独立 `database.server.id`、独立 schema history topic 与独立 MySQL 账号；connector 配置、binlog 位点和 task 状态保存在 Kafka 内部 topic，worker 故障由 peer 接管并从保存位点恢复。topic/消费组与 DLQ 各自独立。
- **重发只在 MySQL 事务内发生**：可重试失败先记录原因与 `retry_at`，到期由 `Sweep` 递增 `schedule_version`；租约过期由 `Sweep` 递增版本重排。普通 `queued` 等待（worker/解析名额）不会递增版本，也不消耗执行次数。消费者只处理与数据库当前版本相等的通知；任务表初始快照的 `op=r` 事件用于存量切换补投。
- **搜索永远同步**：MCP `search` 与 REST `POST /v1/search` 一次调用完成检索 + 重排并直接返回证据，没有 `result_id`/`wait_seconds`/轮询；artifact 按 ID 独立读取，保留期由 `RAG_RESULT_TTL_SECONDS`（默认 3600 秒）控制。原排队搜索链路（`search_tasks`、`search_result_snapshots`、`praxis_search` topic、`rag-search-worker`、搜索 DLQ）已于 2026-11-13 下线，两张历史表保留数据但不再读写。
- **缓存消费者 revision 水位**：业务表变更（包括手工 SQL）由缓存连接器投递，消费者递增 `cache_revisions.revision` 后重建并写 Redis；失败记录 `attempts/last_error`，由周期 `RefreshDirty` 收敛，不再有 `cache_outbox` 补发；因此 Redis 写失败不会把事件反复送进 DLQ。
- Console「系统监控」采用顶部标签栏：`概览` 展示总体状态、关键指标、依赖探测、文档任务/上传、两个 Kafka Connect 采集连接器（`文档采集`/`缓存采集`，含 connector/task 状态、所在 worker、失败原因、自动重启与恢复结果）、两组 Kafka 消费组、缓存水位与 Worker 心跳，并新增主机资源卡片（CPU 使用率/核数/型号/load、内存与 swap、网络总速率与最忙网卡、进程 RSS/VMS/goroutine/heap）；`MySQL`/`Redis`/`Qdrant`/`Kafka`/`PDF 解析服务`/`文档 Worker`/`Debezium` 标签展示该组件既有内容与该组件日志；`全部日志` 提供来源（内存/文件）、级别、关键字过滤与分页。读不到的进度显示「未知」。搜索同步化迁移与切换步骤见 [CDC 迁移契约](docs/cdc-migration.md) §12 与 [部署指南](docs/DEPLOYMENT.md) §12；采集层操作见 [Kafka Connect 手册](deploy/kafka-connect/README.md)。

## 迁移状态

- **Go 是目标后端**：`rag-service`、文档/缓存/死信三个 worker、Console、OpenAPI、迁移/切换/状态 CLI 与本地批量导入 CLI 均已落地；搜索已于 2026-11-13 改为同步直返（MCP 5 个工具、REST 无 `/v1/search-results`）。parity 审计的 P0（搜索资产投影、worker 可执行程序、已保存 `document.md` 预览）与主要 P1 已修复，`go build ./...` 与全量 `go test ./...` 通过。
- **Python 后端已删除**：原 `src/rag_mcp`、`tests`、`pyproject.toml`、`uv.lock` 已移除；`python-workers/` 曾整体删除，2026-11-10 起以**独立 PDF 解析微服务**（`python-workers/pdf-service`，仅 PDF、HTTP + zip 清单，清单版本 `docling-service-v1`，该字符串是证据格式契约）的形式恢复，不进入 Go 构建/镜像/测试依赖；2026-11-13 起其本地 docling/torch/easyocr/CodeFormula 与模型卷全部移除，改为调用远端 OpenAI 兼容视觉模型。PDF 默认调用该服务，也可切回 Go 内置纯文本引擎；DOCX 读取与 Word 图片描述在 Go 进程内完成；只有 .doc 转换需要外部 LibreOffice，VLM 通过 HTTP 调用。
- **数据兼容**：Go 复用既有 MySQL 表结构、`enc:v1:` 凭据格式和配置文件位置（`~/.praxis-rag/config.toml`），不需要数据迁移；状态表用 `rag-state-migrate` 检查/建表，旧知识源用 `rag-service-migrate` 预览/导入（见已知差异）。
- **历史验证（已下线链路）**：2026-11-09 的 Kafka 3.9.0 + Debezium 3.7.1 + MySQL 8.0 + Redis + Qdrant 实机验证针对当时的“事务 outbox + 单 Debezium 连接器”实现；该投递链路已于 2026-11-13 改造下线，旧验证脚本不再代表当前架构。
- **本次改造未实机验证**：分布式 Kafka Connect 双 worker、Kafka 内部 topic 保存连接器位点、固定名称幂等注册、FAILED task 自动重启、worker/task 故障实战、双 topic 实链路、缓存消费者 revision 收敛、artifact 独立读取与同步搜索超时/取消、存量切换/回滚演练均未在真实 Docker/Kafka Connect/Kafka/Redis 上执行；生产上线前必须按 [Kafka Connect 操作手册](deploy/kafka-connect/README.md) §6、[CDC 迁移契约](docs/cdc-migration.md) §8 与 [部署指南](docs/DEPLOYMENT.md) §12 演练，验证边界见 [VALIDATION.md](VALIDATION.md)。
- **仍待验证**（以 [GO_MIGRATION](docs/GO_MIGRATION.md) 为准）：Kafka SASL/TLS 外部集群、Docker 镜像/Compose `app` profile、pdf-parser 镜像与真实视觉端点推理、systemd、真实 MySQL 多实例并发/故障切换、浏览器 E2E；黑盒 parity 差分改为固定预期（Python 对照已删除）。真实环境冒烟与未验证项见[使用与部署](docs/USAGE.md) 的验证边界。

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

- 异步受理（网页/REST 上传默认方式与 MCP `add_document`）的受理点是状态库单事务提交（`document_jobs` 任务行，不再有发件箱），不是 Kafka 确认；任务表由 Debezium 采集后投递；解析/索引在后台进行，用 `GET /v1/document-jobs/{artifact_id}` 观察进度；批次上传额外提供 `POST /v1/document-batches` + `GET /v1/document-batches/{batch_id}`（Console 使用），每文件独立事务并行受理，不同文件名之间不排队，同名仍串行。MCP 更新/删除仍在请求内完成，索引期间会等待。
- REST/Console 上传统一使用 `POST /v1/document-batches` 异步受理，必须有文档消费者：必须配置 Kafka + Debezium 并运行 `rag-document-worker`（启动脚本自动启动），否则任务保持 `queued`。Worker 的进程数与每进程任务并发分别由 `RAG_DOCUMENT_WORKERS`、`RAG_DOCUMENT_WORKER_CONCURRENCY` 配置，PDF 解析还受解析服务的进程数与每进程并发限制。
- 每批最多 10 个文件、单文件 10 MiB；暂不支持 Excel、网页抓取和独立图片上传。
- Word 保留内容控件、嵌套表格、合并范围与原生公式；未知公式结构、原生图表等无法可靠读取的内容会明确拒绝入库；图注关联有歧义时跳过并报告原因。
- 知识库（namespace）默认归创建者私有，只有创建者和管理员可以使用；共享通过「权限管理」页的邀请链接完成，权限分为可读/可修改、范围分为单文件/多文件/全库，仅 VIP 与管理员可邀请。管理凭证（Service Token / API Key / 本机无会话直连）沿用原有语义，不做知识库归属校验；`/v1/admin/*` 默认只允许本机直连或管理员会话，其中 `/v1/admin/api-keys` 例外：任意已登录账号都可以管理自己的 API Key（非管理员只能绑定本人知识库，历史无主密钥仅管理员/本机可见）。MCP/普通 API 使用 Service Token 或 API Key。详见 [知识库权限管理](docs/PERMISSIONS.md)。
- 默认只监听 `127.0.0.1`。远程部署必须设置 `RAG_SERVICE_TOKEN` 与 MCP Host/Origin 白名单，并在前面配置 HTTPS 与访问控制。
- PDF 解析引擎默认是 `vision`：Go 服务调用独立的 `pdf-parser` 微服务，后者把版面分析、表格结构、OCR 与公式 LaTeX（以及图片描述）统一交给知识库配置的远端视觉模型（VLM）。该服务未启动或超时（默认 900 秒）时 vision 上传会失败，可显式 `RAG_PDF_ENGINE=go` 切回 Go 内置纯文本引擎（文字型 PDF；不含版面/表格/OCR/公式与图片资产，扫描件请先 OCR）。旧值 `docling` 等价于 `vision`。Word 无图注图片可调用同一个远端 VLM（`RAG_VLM_*`），文档裁图会发送到该端点；未配置 VLM 时图片保留占位描述。.doc 转换依赖外部 LibreOffice。
- 搜索**同步直返**：MCP `search` 与 REST `POST /v1/search` 一次调用完成检索 + 重排并返回证据（`results`/`artifacts`/`evidence_status`/`answer_guidance`），没有 `result_id`、`wait_seconds`、轮询或搜索结果快照；`GET /v1/search-results/{result_id}` 与 MCP `get_search_result` 已删除。总预算由 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）约束，超过返回错误，客户端重试等于重新检索。
- artifact 引用独立保留：以 MySQL `artifact_snapshots` 为事实来源，按 ID 读取（`GET /v1/artifacts/{artifact_id}` 与 `/content`），保留期由 `RAG_RESULT_TTL_SECONDS`（默认 3600 秒，语义为 artifact TTL）控制；`search_tasks`/`search_result_snapshots` 两张历史表保留数据但不再读写。
- 日志统一为结构化日志（Go 服务与 3 个 worker 共用 `slog`，`RAG_LOG_LEVEL`/`RAG_LOG_FORMAT` 控制级别与 `text`/`json` 编码，`RAG_DEBUG_TRACE` 保持旧开关兼容）；只记录任务/操作 ID、失败阶段与安全错误类别，不记录查询、文件内容、消息正文或凭据。Console「系统监控」的日志视图由 `RAG_LOG_BUFFER_SIZE`（进程内环形缓冲，默认 1000 条）与可选 `RAG_LOG_FILE`（所有 Go 进程追加同一个 JSON Lines 文件，供多进程日志汇总）提供；两者均为只读展示，日志正文与属性在输出前经统一脱敏。
- 每个 HTTP 请求接受或回写 `X-Request-ID`，上传/后台任务日志带 `operation_id`、`artifact_id`、`batch_id`：一次上传可从请求日志追到入队、重试与最终失败。
- 文档任务失败时 `document_jobs.error` 保留最近一次失败摘要（错误类别、失败阶段、上游 HTTP 状态与脱敏详情）；重试耗尽进入终态或死信时提示会附带该摘要，不再只有统一提示。

## 项目文档

| 文档 | 内容 |
| --- | --- |
| [使用与部署](docs/USAGE.md) | Go 后端的构建、运行、环境变量、REST/MCP 示例、worker、迁移 CLI。 |
| [知识库权限管理](docs/PERMISSIONS.md) | 角色、知识库归属、可读/可修改与单文件/多文件/全库权限、邀请链接流程与接口。 |
| [部署指南](docs/DEPLOYMENT.md) | Docker/Compose 与 systemd 示例、worker 重启策略、DLQ 监控、分布式 Kafka Connect 与升级/回滚。 |
| [Kafka Connect 操作手册](deploy/kafka-connect/README.md) | 两个 Debezium 连接器的模板、注册、生产要求、FAILED task 自动重启与故障演练。 |
| [Go 迁移总纲](docs/GO_MIGRATION.md) | 目标架构、兼容性硬约束、阶段与进度记录、当前切换状态。 |
| [Go Parity 审计](docs/GO_PARITY_AUDIT.md) | Go 与 Python/前端契约差异、P0/P1/P2 清单与验证限制。 |
| [共享状态存储](docs/state-storage.md) | MySQL 表结构、密钥加密、迁移与多实例注意事项。 |
| [状态缓存](docs/state-cache.md) | Redis 读模型缓存、revision 水位、缓存消费者与一致性边界。 |
| [CDC 迁移契约](docs/cdc-migration.md) | 双 Debezium 连接器、两类事件（文档/缓存）、调度版本、缓存消费者、存量切换与监控替换；搜索同步化迁移入口见 §12。 |
| [产物引用](docs/result-references.md) | artifact 引用、artifact TTL 与容量限制（搜索结果快照/result_id 已下线）。 |
| [验证记录](VALIDATION.md) | 已执行的验证及其覆盖范围。 |
| [历史开发记录](PROGRESS.md) | 项目早期开发阶段记录，不作为当前功能清单。 |

产品名为 **Praxis-RAG**，Go module 为 `github.com/Guan0923/xinghe_rag`，Python 安装包名为 `praxis-rag`、内部模块仍为 `rag_mcp`。REST 路径、MCP 工具名、环境变量与数据位置尽量保持与 Python 实现一致；差异以 [Go Parity 审计](docs/GO_PARITY_AUDIT.md) 为准。

