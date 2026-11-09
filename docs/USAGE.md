# Praxis-RAG 使用与部署（Go 后端）

本文面向运行 **Go 后端**（`rag-service` + 三个 worker）的用户，介绍构建、配置、启动、REST/MCP 使用、worker、迁移 CLI、Docker 与验证边界。产品概览见 [README](../README.md)；迁移背景与阶段见 [GO_MIGRATION](GO_MIGRATION.md)；契约差异见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)；容器与 systemd 示例见 [DEPLOYMENT](DEPLOYMENT.md)。

```text
浏览器 ------------------------> /console
MCP 客户端 -> /mcp ------------> rag-service ──> Qdrant
REST /v1/* --------------------> rag-service ──> 共享状态库（MySQL）
                                             ├─> Redis（读模型缓存、搜索结果快照；可选，排队搜索快照需要）
                                             └─> Kafka（outbox relay / 搜索队列；可选）

MCP add_document -> 状态库单事务（document_jobs + document_job_outbox）
                 -> relay（rag-service 配置 Kafka 时）或本地 outbox 轮询
                 -> Kafka 通知 -> rag-document-worker -> 解析/切分/Embedding/Qdrant 发布
MCP search（配置 Kafka 时）-> search_task_outbox -> Kafka -> rag-search-worker -> SearchRaw + 快照
Kafka 死信（显式 DLQ topic）-> rag-dead-letter-worker --kind document|search
```

RAG 只返回证据，不生成最终回答。后端为 Go 实现；Python 仅作为 PDF（Docling）与 Word 视觉解析的可选外部子进程存在于 `python-workers/`，不是运行 Go 后端的前置条件。

## 1. 安装与构建

### 前置条件

| 依赖 | 版本/要求 | 用途 |
| --- | --- | --- |
| Go | 1.27+（`go.mod`） | 构建 `rag-service` 与全部命令 |
| Node.js | 20.19+ 或 22.12+ | 构建 Console（只使用 API/MCP 可跳过） |
| Qdrant | HTTP API（默认 `6333`） | 检索索引；Go **不支持** `QDRANT_PATH` 本地文件模式 |
| 共享状态库 | MySQL 8.0+（`utf8mb4`） | 文档、KB、API Key、任务、发件箱、UploadJournal |
| Redis | 可选，7.x | 搜索结果快照、读模型缓存；排队 MCP 搜索的快照读取必需（搜索任务状态在共享状态库） |
| Kafka | 可选，单节点 KRaft（Compose `kafka`）或外部集群 | 异步文档通知、排队搜索；可用 SASL/TLS 环境变量连接外部集群，Compose 本身只提供 PLAINTEXT |
| 外部解析器 | 可选 | PDF（Docling Python 环境/模型）、DOC（LibreOffice）；**不包含在 Go 二进制或镜像中** |

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
dist/bin/rag-search-worker(.exe)
dist/bin/rag-dead-letter-worker(.exe)
dist/bin/rag-service-migrate(.exe)
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

仓库自带 `docker-compose.yml`，默认 profile 只启动基础设施（MySQL、Qdrant、Redis、Kafka）：

```powershell
Copy-Item .env.example .env
# 必填：RAG_MYSQL_PASSWORD、MYSQL_ROOT_PASSWORD、RAG_STATE_ENCRYPTION_KEY
docker compose up -d
```

MySQL 默认映射 `127.0.0.1:3307`（避免与本地 3306 冲突），Qdrant 为 `6333/6334`，Redis 为 `6379`，Kafka 宿主监听 `127.0.0.1:9092`（容器内 `kafka:9092`）。停止保留数据：`docker compose down`。

> Kafka 默认自动创建 topic（4 partitions、副本 1）；`scripts/start-stack.ps1` 仍会显式创建 cache/document/search 与两个 DLQ topic、等待 Kafka healthy、执行 `rag-state-migrate --apply`（可选导入 legacy 知识源），然后启动 `dist/bin/rag-service.exe`；`RAG_OUTBOX_BRIDGE=debezium` 时带 `--profile debezium` 并等待 Debezium 输出 `Starting streaming`。用 `-MigrateOnly` 可只准备依赖与数据库。使用 `RAG_DOCUMENT_WORKER_LOCAL=1` 的单进程文档模式或纯同步 REST 场景可以不配置 Kafka（保持 `RAG_KAFKA_BOOTSTRAP_SERVERS` 为空）。

### 手动依赖

只使用同步 REST/Console 时，最小依赖是 Qdrant + 状态库：

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
# RAG_DOCUMENT_TOPIC = "praxis_document_ingest"
# RAG_SEARCH_TOPIC = "praxis_search"
# RAG_DOCUMENT_DLQ_TOPIC = "praxis_document_ingest_dead_letters"
# RAG_SEARCH_DLQ_TOPIC = "praxis_search_dead_letters"
# RAG_OUTBOX_BRIDGE = "relay"
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

### 检索、模型与外部解析器

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
| `RAG_PDF_PARSER_PYTHON` | 空 | Docling 解析进程的 Python 可执行文件 |
| `RAG_PDF_MODELS_PATH` | 空 | Docling 本地版面/表格/OCR 模型目录 |
| `RAG_PDF_PARSE_TIMEOUT` | `900` 秒（30..7200） | PDF 解析外部进程超时 |
| `RAG_VLM_BASE_URL` | 空 | 远端 OpenAI 兼容 VLM 端点的**环境回退值**；Console 知识库设置优先 |
| `RAG_VLM_API_KEY` | 空 | VLM 端点密钥，作为 `Authorization: Bearer` 发送 |
| `RAG_VLM_MODEL` | 空 | 端点上的模型名，如 `qwen2.5-vl-7b`（环境回退值） |
| `RAG_VLM_TIMEOUT` | `120` 秒（1..3600） | 单次 VLM 请求超时 |
| `RAG_VLM_CONCURRENCY` | `4`（1..64） | 并发 VLM 请求数 |
| `RAG_WORD_CONVERTER_PATH` | 空 | LibreOffice `soffice` 路径（DOC 转换） |
| `RAG_WORD_CONVERT_TIMEOUT` | `120` 秒（1..7200） | DOC 转换超时 |

外部解析器的实际调用发生在 ingest 阶段：Go 进程启动外部 Python/soffice 子进程，通过文件协议交换解析产物。PDF 解析仍需要 Python/Docling 环境与本地模型目录（版面、表格、OCR 在本地跑）；**公式识别与图片描述改为调用远端 VLM**，由 `RAG_VLM_BASE_URL`/`RAG_VLM_API_KEY`/`RAG_VLM_MODEL` 配置，未配置时 PDF 解析会明确失败而不是回退到本地模型。Word 无图注图片同样走该远端端点；配置了 VLM 后 Go 会自动启用 `word_vision_worker`。这些外部依赖未在本仓库的 Go 测试环境中实机验证。`r`n`r`n**VLM 优先取知识库配置**：Console 的知识库设置页在 Embedding/Reranker 之外提供 VLM 区块（URL / Key / 模型，可开关），保存后该知识库随后的上传与 worker 解析立即使用它；未配置或关闭时回退到 `RAG_VLM_*` 环境变量。API Key 与其他 provider 一样加密存储，前端只显示"已保存"。

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

### Redis、搜索结果快照

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_REDIS_ENABLED` | 未设置 = 按 URL 自动启用 | `false/0/off` 显式禁用；禁用时排队搜索快照不可读 |
| `RAG_REDIS_URL` | 空 | 例如 `redis://127.0.0.1:6379/0` |
| `RAG_REDIS_PREFIX` | `rag:state:v1` | 缓存/任务键前缀 |
| `RAG_REDIS_TTL_SECONDS` | `60` | 读模型展示缓存 TTL |
| `RAG_REDIS_TIMEOUT_SECONDS` | `0.3` | Redis 连接/命令超时 |
| `RAG_RESULT_TTL_SECONDS` | `3600` | 搜索结果快照 TTL（与搜索任务截止时间独立） |
| `RAG_RESULT_MAX_BYTES` | `16777216` | 单结果快照上限 |
| `RAG_ARTIFACT_MAX_BYTES` | `67108864` | 单 artifact 上限 |
| `RAG_RESULT_MAX_ARTIFACTS` | `100` | 每次搜索最多 artifact 数 |

Redis 不可用时，持久读模型会回源状态库；**排队 MCP 搜索的结果快照必须用 Redis**，没有 SQL 回放。REST 同步搜索、Console 与本地调试不依赖 Redis（快照可降级为 `snapshot_available=false`）。

### Kafka 与队列

单节点 Compose Kafka 使用 INTERNAL/EXTERNAL 双 listener：宿主机进程用 `127.0.0.1:9092`，同一 compose 网络的容器用 `kafka:9092`；`RAG_KAFKA_BOOTSTRAP_SERVERS` 为空表示 brokerless（异步受理仍可提交，但投递/排队搜索不可用）。

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_KAFKA_BOOTSTRAP_SERVERS` | 空 | 逗号分隔 `host:port`；宿主默认 `127.0.0.1:9092`，compose 容器 `kafka:9092`；空 = brokerless |
| `RAG_KAFKA_CLIENT_ID` | `praxis-rag` | Kafka 客户端标识 |
| `RAG_KAFKA_SECURITY_PROTOCOL` | `PLAINTEXT` | `PLAINTEXT` / `SSL` / `SASL_PLAINTEXT` / `SASL_SSL` |
| `RAG_KAFKA_SASL_MECHANISM` | 空 | `PLAIN` / `SCRAM-SHA-256` / `SCRAM-SHA-512`（仅 SASL 协议使用） |
| `RAG_KAFKA_SASL_USERNAME` / `RAG_KAFKA_SASL_PASSWORD` | 空 | SASL 凭据，仅从环境读取 |
| `RAG_KAFKA_TLS_CA` | 空 | 可选 CA PEM 路径 |
| `RAG_KAFKA_TLS_INSECURE` | `false` | 跳过 TLS 证书校验（仅测试） |
| `RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS` | `10` | 发布超时 |
| `RAG_CACHE_TOPIC` / `RAG_CACHE_GROUP` | `praxis_state_cache_refresh` / `praxis_state_cache_refresh_workers` | 缓存刷新 topic/group |
| `RAG_DOCUMENT_TOPIC` / `RAG_DOCUMENT_GROUP` | `praxis_document_ingest` / `praxis_document_ingest_workers` | 文档通知 topic/group |
| `RAG_SEARCH_TOPIC` / `RAG_SEARCH_GROUP` | `praxis_search` / `praxis_search_workers` | 排队搜索 topic/group |
| `RAG_DOCUMENT_DLQ_TOPIC` | `praxis_document_ingest_dead_letters` | 文档死信 topic（显式配置，不是 `%DLQ%` 约定） |
| `RAG_SEARCH_DLQ_TOPIC` | `praxis_search_dead_letters` | 搜索死信 topic（显式配置） |
| `RAG_DOCUMENT_DLQ_GROUP` / `RAG_SEARCH_DLQ_GROUP` | `praxis_document_dead_letters` / `praxis_search_dead_letters` | 死信 worker 的独立 group |
| `RAG_OUTBOX_BRIDGE` | `relay` | `relay`：进程内发布发件箱到 Kafka；`debezium`：应用不发布，由 Debezium Server 读取 MySQL binlog 并通过原生 Kafka sink 投递（需 compose `debezium` profile / 外部 Debezium；两者不可同时启用） |
| `RAG_APP_KAFKA_BOOTSTRAP_SERVERS` | 空（compose 内默认 `kafka:9092`） | 仅 compose app/Debezium 容器读取的 bootstrap 覆盖；宿主 Go 进程读 `RAG_KAFKA_BOOTSTRAP_SERVERS` |

**消费语义（至少一次）**：

- handler 成功 → 提交 Kafka offset；
- handler 失败（nack）→ 在**同一进程内**重试同一条记录，指数退避从 `100ms` 开始、上限 `5s`，`DeliveryAttempt` 从 1 递增；
- 达到 `MaxAttempts`（文档/搜索 worker 默认 5）→ 把原始消息发布到对应 `RAG_*_DLQ_TOPIC`、执行终态回调、再提交 offset；
- `MaxAttempts=0` → 不设上限，一直重试直到进程或 context 取消；
- 死信 worker 的 `--kind document|search` 实例消费显式 DLQ topic 与独立 `RAG_*_DLQ_GROUP`，不再使用 `%DLQ%<源组>` 或 mqadmin；`MaxAttempts=0` 的死信 worker 自身不会二次死信。
- 消费重试与死信发布由 Go 消费端完成，不依赖 Kafka 的不可见期/重投机制；重复投递由 handler 幂等与 MySQL 业务租约吸收。

### 异步文档与 worker 环境

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_DOCUMENT_JOB_LEASE_SECONDS` | `300` | 文档任务 SQL 租约 |
| `RAG_DOCUMENT_JOB_HEARTBEAT_SECONDS` | `20` | 处理期间续租间隔 |
| `RAG_DOCUMENT_JOB_SWEEP_SECONDS` | `120` | 滞留/过期任务回收窗口 |
| `RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS` | `604800`（7 天） | 发件箱行保留期 |
| `RAG_DOCUMENT_JOB_RETENTION_SECONDS` | `2592000`（30 天） | 终态任务保留期 |
| `RAG_DOCUMENT_WORKER_LOCAL` | `false` | 文档 worker 单进程 outbox 模式；仅在未配置 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时生效 |
| `RAG_DOCUMENT_WORKER_POLL_SECONDS` | `0.5` | 本地 outbox / 消费轮询间隔 |
| `RAG_DOCUMENT_WORKER_RECOVER_SECONDS` | `30` | 文档 worker 周期性 Recover（恢复中断上传/补偿） |
| `RAG_DOCUMENT_BROKER_MAX_RETRIES` / `RAG_DOCUMENT_MAX_ATTEMPTS` | `5` | 文档 worker 的 Kafka 进程内最大尝试次数（前者优先；名称为兼容旧配置保留） |

### 排队搜索任务环境

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_SEARCH_TIMEOUT_SECONDS` | `120` | 搜索任务总截止时间（包含排队），与调用方 `wait_seconds` 不同 |
| `RAG_SEARCH_MAX_ATTEMPTS` | `5` | 搜索 worker 的 Kafka 进程内重试上限 |
| `RAG_SEARCH_LEASE_SECONDS` | `120` | 任务执行租约（Go 状态机扩展） |
| `RAG_SEARCH_RETRY_SECONDS` | `5` | 失败重投或重试等待 |

### Worker 二进制与运行模式

| 命令 | 作用 | 必需 | 退出码 |
| --- | --- | --- | --- |
| `rag-service` | REST + MCP + Console + 进程内 relay/维护循环 | Qdrant、状态库；MCP 需要 token；异步/排队需要 Kafka | `0` 正常停止，`1` 运行时失败 |
| `rag-document-worker` | 消费文档通知，解析/切分/Embedding/发布 | 状态库、Qdrant；Kafka 模式需 `RAG_KAFKA_BOOTSTRAP_SERVERS`，或 `RAG_DOCUMENT_WORKER_LOCAL=1` | `0/1/2` |
| `rag-search-worker` | 消费 schema 2 搜索请求，执行 `SearchRaw` + 快照 + 终态 | `RAG_KAFKA_BOOTSTRAP_SERVERS`、状态库、Qdrant、Redis（快照） | `0/1/2` |
| `rag-dead-letter-worker --kind document\|search` | 消费显式 `RAG_DOCUMENT_DLQ_TOPIC` / `RAG_SEARCH_DLQ_TOPIC`，把可确认的失败写入持久终态并 ACK | `RAG_KAFKA_BOOTSTRAP_SERVERS`、对应 `RAG_*_DLQ_GROUP` | `0/1/2` |

退出码约定（worker 与全部 CLI）：`0` = 收到 SIGINT/SIGTERM 正常停止；`1` = 运行时失败（应重启/告警）；`2` = 配置或用法错误（修配置，不要自动重启）。服务本身配置错误也以 `1` 退出。文档 worker 的本地模式适合单进程开发：未配置 Kafka 时它直接轮询 `document_job_outbox`，有 Kafka 时变量被忽略并切换为 Kafka 消费模式。**worker 不在 `rag-service` 进程内运行**；服务只负责受理、relay 和维护循环。

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

管理接口仅允许本机直连（loopback、无代理头、同源 Origin）：

```powershell
curl.exe -X POST http://127.0.0.1:8011/v1/admin/api-keys `
  -H "Content-Type: application/json" `
  -d '{"name":"cli","namespaces":["default"]}'
```

响应中的 `key` 只出现一次；后续请求带 `Authorization: Bearer <key>` 和 `X-RAG-Namespace: default`。也可直接使用 `RAG_SERVICE_TOKEN`（本地链接可用 Service Token 代替 API Key）。

### 用户登录与注册（Console）

Console 默认启用账号登录（`RAG_AUTH_ENABLED=true`）：

1. 打开 `/console` 会进入登录页；切到"注册"创建账号，**第一个注册的账号自动成为管理员**。用户名不区分大小写（统一按小写保存）。
2. 管理员在侧边栏"用户管理"页可以：把账号设为管理员/普通用户、启用/禁用、删除。系统始终保留至少一名启用状态的管理员（最后一个管理员不能被降级、禁用或删除）。
3. 普通用户可检索、上传/删除文档、切换知识库；管理员额外可以管理知识库、API Key 和用户。

会话使用 HttpOnly Cookie（`rag_session`）保存，默认有效期 7 天（`RAG_AUTH_SESSION_TTL_HOURS`），所有写操作要求 `X-CSRF-Token`（Console 自动带上）。判定规则：

| 变量 | 默认 | 作用 |
| --- | --- | --- |
| `RAG_AUTH_ENABLED` | `true` | 设为 `false` 关闭登录，Console 回到旧的本机/Token 模式 |
| `RAG_AUTH_REGISTRATION` | `open` | `closed` 关闭自助注册（仍需先存在管理员） |
| `RAG_AUTH_SESSION_TTL_HOURS` | `168` | 会话有效期（小时） |
| `RAG_AUTH_COOKIE_SECURE` | `false` | HTTPS 部署时设为 `true` |

管理员会话可以让远程浏览器访问 `/v1/admin/*`（此前只允许本机）；自动化脚本继续使用 `RAG_SERVICE_TOKEN` 或 API Key，不受影响。

### 启动 worker

```powershell
# 异步 MCP add_document（Kafka 模式；无 Kafka 可加 $env:RAG_DOCUMENT_WORKER_LOCAL="1"）
.\dist\bin\rag-document-worker.exe

# 服务端 MCP 排队搜索
.\dist\bin\rag-search-worker.exe

# 死信处理（两个独立实例）
.\dist\bin\rag-dead-letter-worker.exe --kind document
.\dist\bin\rag-dead-letter-worker.exe --kind search
```

worker 必须与 `rag-service` 使用**同一** `RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`RAG_BACKEND=qdrant`/`QDRANT_URL`、`RAG_STATE_ENCRYPTION_KEY`、Redis/队列设置。它们不继承其他终端的 `.env`（PowerShell 中请显式设置或使用进程管理器）。

### 冒烟检查

1. `GET /health` 返回 `status=ok`，`backend=qdrant`，`state` 显示 MySQL 后端。
2. `/console` 返回 Console 页面（构建了 `dist/web` 且 `RAG_WEB_ROOT` 正确时）。
3. REST `/v1/documents` 需要认证，未认证返回 401。
4. MCP 初始化成功，`tools/list` 返回 6 个工具。
5. 异步链路：配置 Kafka 并启动 document worker 后，MCP `add_document` 得到 `artifact_id`，轮询 `get_document_status` 至 `succeeded`。
6. 排队搜索：配置 Kafka/Redis 并启动 search worker 后，MCP `search` 返回结果或 `result_id`，`get_search_result` 可轮询到完成。

## 5. REST API 示例

### 认证与命名空间

- 普通 `/v1/*`、`/mcp`：`Authorization: Bearer <RAG_SERVICE_TOKEN 或 API Key>`；未设置 Token 时，本机 loopback 可直接使用 Headers 的 `X-RAG-Namespace` 访问普通租户接口。Console 登录后也接受会话 Cookie（或 Bearer 会话 Token）。
- `/v1/admin/*`：本机直连（loopback/无 Forwarded 代理头/同源 Origin）**或**登录管理员的会话（Cookie 写操作需 `X-CSRF-Token`）。
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

### API Key 管理（本机管理）

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
```

`mcp-config` 返回 `{"url":"http://<host>/mcp","headers":{"Authorization":"Bearer <key>","X-RAG-Namespace":"default"}}`，可直接粘贴到 MCP 客户端配置。

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
```

公开配置：`GET /v1/knowledge-base`（返回 `public_settings + strategies`，缺少知识库时 404）。创建/修改的请求体与 Python 实现一致，`embedding` 必填，`embedding_model` 长度 1..512。

### 文档管理

```powershell
# 上传前检查（按顺序返回 files，document 为已存在文档或 null）
curl.exe -X POST http://127.0.0.1:8011/v1/documents/check `
  -H $headers -d '{"files":[{"name":"guide.md","sha256":"<64-hex>"}]}'

# 同步上传（multipart，字段名必须为 files；可选 Idempotency-Key）
curl.exe -X POST http://127.0.0.1:8011/v1/documents `
  -H "Authorization: Bearer <token-or-key>" `
  -H "X-RAG-Namespace: default" `
  -H "Idempotency-Key: batch-20261108-01" `
  -F "files=@guide.md" -F "files=@manual.txt"

# 列表、读取正文、删除
curl.exe http://127.0.0.1:8011/v1/documents -H $headers
curl.exe "http://127.0.0.1:8011/v1/documents/guide.md/content" -H $headers   # 文本
curl.exe -X DELETE "http://127.0.0.1:8011/v1/documents/guide.md" -H $headers
```

上传响应包含 `imported`、`skipped`、`failed`、`documents`；`skipped` 条目带 `requested_name`。单文件 10 MiB、单批 10 个；上传切分参数来自知识库配置，不接受请求级覆盖。PDF 返回原始文件字节（`Content-Type: application/pdf`）；版本化 PDF/Word 的正文读取已保存的 `document.md`。

### 搜索、结果与产物

```powershell
curl.exe -X POST http://127.0.0.1:8011/v1/search `
  -H $headers `
  -d '{"query":"E03 怎么处理？","retrieval":"hybrid","rerank":"cross_encoder","candidate_k":10,"top_k":5,"filters":{}}'

# 排队/已完成结果（REST 也读取任务状态与快照）
curl.exe "http://127.0.0.1:8011/v1/search-results/<result_id>?page=1&page_size=10" -H $headers

# artifact（源文件/图片/表格 JSON）
curl.exe "http://127.0.0.1:8011/v1/artifacts/<artifact_id>" -H $headers
curl.exe "http://127.0.0.1:8011/v1/artifacts/<artifact_id>/content" -H $headers
```

搜索同步执行（REST 不经队列），结果包含 `retrieval`、`rerank`、`evidence_status`、`results`、`debug`、`snapshot_context`；资产命中带 `asset`（图片 base64 或表格结构）和 `artifacts`。分页/偏移参数越界返回 400。

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
| `search` | `query`、`retrieval`、`rerank`、`candidate_k`、`top_k`、`filters`；配置 Kafka 时还有 `wait_seconds` | 检索；排队模式下超时只返回 `result_id` |
| `get_search_result` | `result_id`、`page`、`page_size` | 读取排队状态或结果分页；不重复检索 |
| `add_document` | `name` + `content`/`content_base64`，可选 `idempotency_key` | 异步入库，成功只返回 `artifact_id` |
| `get_document_status` | `artifact_id` | 查询 `status/stage/error`，状态在 MySQL |
| `update_document` | `name` + `content`/`content_base64`，可选 `idempotency_key` | 同步替换并重建索引 |
| `delete_document` | `name` | 同步逻辑删除 |

工具名与 schema 与 Python 实现一致；`delete_document` 标注为 destructive。是否启用某个工具可经 `/v1/admin/tools` 管理；执行时会实时读取 catalog，禁用后返回错误。

### 排队搜索（配置 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时）

- `search` 增加 `wait_seconds`（0..30，默认 10）。等待内完成返回完整结果；否则只返回 `{"result_id":"<uuid>"}`。
- `get_search_result`：未完成返回 `{"result_id","status":"queued","stage":"queued|processing|retry_wait"}`；成功返回快照分页；失败/超时/快照过期是 `isError=true`。
- 必需：`rag-search-worker`、Redis（结果快照）、状态库（任务/发件箱）。任务截止 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）包含排队时间。
- 未配置 Kafka 时 MCP `search` 退化为同步执行，schema 不包含 `wait_seconds`；客户端应按实际工具 schema 调用。
- `RAG_RESULT_TTL_SECONDS`（默认 3600 秒）到期或 Redis 丢失后 result_id 不可用，无 SQL 回放。

### 异步文档

`add_document` 在状态库单事务写入完整任务（含文件字节）与发件箱，提交后返回 `{"artifact_id": "<uuid>"}`，不代表已解析/可检索。`get_document_status` 返回 `status`（`queued/processing/succeeded/failed`）、`stage`、`error`；仅 `succeeded` 表示已持久发布。同名文档已存在或已有排队/执行中的新增会被拒绝；不确定提交结果时用同一 `idempotency_key` 重试，不会重复入队。

投递方式：默认 `RAG_OUTBOX_BRIDGE=relay`，服务进程内发布发件箱；设为 `debezium` 时服务不发布，由外部 Debezium 读取 MySQL binlog 投递（`docker compose --profile debezium up -d`，配置见 [deploy/debezium/README.md](../deploy/debezium/README.md)）。未配置 Kafka 时可用 `RAG_DOCUMENT_WORKER_LOCAL=1` 让文档 worker 直接轮询发件箱。**不要把 Debezium 与 relay 同时启用**（重复投递）。

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

### rag-service-migrate（旧知识源导入）

```powershell
# 默认只读预演
.\dist\bin\rag-service-migrate.exe --legacy-root "$env:USERPROFILE/.mini_agent"
# 实际导入（写入状态库：blobs + documents）
.\dist\bin\rag-service-migrate.exe --apply --legacy-root "$env:USERPROFILE/.mini_agent"
# 只迁移指定用户（可重复）
.\dist\bin\rag-service-migrate.exe --apply --user-id <uuid>
```

**重要差异**：`--apply` 只导入状态（源文件 blob 与文档记录），**不执行向量重建**（Python 版会重跑上传）。迁移后必须通过 HTTP/MCP 重新上传（或等待后续 reindex 支持）才会进入 Qdrant 可检索；`--skip-assets` 与旧版解析资产树尚未实现（TODO）。退出码 `0/1/2` 同上；`WARN` 行最多打印 20 条。

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
- PDF/DOCX/DOC 与 HTTP 上传使用同一外部解析器配置；未配置解析器时按上传同规则失败。`--dry-run` 不打开运行时/状态库。
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

# Compose 的 app profile（会构建镜像；默认 compose 仍只启动基础设施）
docker compose --profile app up -d --build
```

镜像内容与约定：

- `/app/bin/` 包含全部命令（含三个 worker）；`ENTRYPOINT` 为 `/app/bin/rag-service`。
- `/app/web` 为 Console，`RAG_WEB_ROOT=/app/web`；`/app/config.toml` 为非敏感默认配置（`RAG_SERVICE_HOST=0.0.0.0`、端口 8011、`/app/data`、Qdrant 服务名、loopback MCP 白名单）。
- 非 root 用户 `rag`，`/app/data` 为数据卷；健康检查调用 `/health`。
- 镜像**不含** Python、Docling、LibreOffice 或本地模型：PDF/DOC/Word 解析需要把外部环境挂载到容器并设置 `RAG_PDF_PARSER_PYTHON`、`RAG_PDF_MODELS_PATH`、`RAG_WORD_CONVERTER_PATH` 等变量；公式与图片描述改用远端 VLM，另外需要 `RAG_VLM_BASE_URL`/`RAG_VLM_MODEL`。否则相应上传会明确失败。
- Compose `app` 服务只运行 `rag-service`；worker 没有内置 compose 服务，可用一次性容器（把入口覆盖为 worker 二进制、连接到同一网络并复用同一环境）或参考 [DEPLOYMENT](DEPLOYMENT.md) 的 systemd/override 示例。
- **已验证/未验证**：本机 Docker 已实机拉起 Kafka + Debezium 基础设施，并用 host 模式 Go 服务/worker 跑通 debezium/relay 两种异步链路、排队搜索与 DLQ（见 §10；脚本 `scripts/verify-debezium-e2e.py`、`scripts/verify-kafka-search.py`、`scripts/verify-kafka-dlq.py`）；镜像构建、compose `app` profile 与 worker 容器仍未经实机验证。

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

- `go build ./...` 全部通过；`go test -count=1 ./...` 全部通过（包数与用例数随代码演进：迁移闭环时为 24 个有测试的包，含 state/runtime/workers/httpapi/mcp/searchrefs/检索栈等）。
- `scripts/build-go.ps1` 本机构建 + 冒烟：`/health 200 status=ok`、`/console` 返回构建后的 Console、`/` 200。POSIX 的 `scripts/build-go.sh`/`make` 未在当前 Windows 验证环境执行。
- 三个 worker 二进制 `--help`、无 Kafka、错误 backend、非法 `--kind` 的退出码行为；文档 worker 本地模式可启动并连接 MySQL 状态。
- Go 端 REST/MCP/检索/文档/资产投影/`document.md` 预览/上传事务的单元与集成测试（真实 MySQL + fake Qdrant/model）。

**未验证**（不要据此宣称生产就绪）：

- Docker 镜像构建、Compose `app` profile、systemd 单元未实际执行（本环境无 Docker/systemd）。
- 已实机验证 Kafka 3.9.0 + Debezium Server 3.7.1 + MySQL 8.0 + Redis + Qdrant：CDC 投递（Kafka healthy、`Starting streaming`、topic 记录与 outbox 一致），以及 Go 服务/worker 的完整异步链路（debezium 与 relay 两种模式）、显式 DLQ topic、排队 MCP 搜索与 Redis 快照。仍未验证 SASL/TLS 外部 Kafka、MySQL 多实例锁与 systemd 部署。
- Docling/LibreOffice 链路未在 Go 环境实机验证；PDF/DOC/Word 解析依赖外部 Python 环境，远端 VLM 端点只做了配置/协议单测，未做实机联调。
- 没有浏览器 E2E：Console 只验证构建与静态冒烟，交互/字段联动由人工核对与前端源码检查。
- Tokenizer 是确定性近似，不保证与 Python/BGE-M3 的 token 计数一致（句子/段落/结构边界一致）。
- 异步 e2e（API 受理 → fake Kafka transport → worker → 发布）已由 `internal/e2e` 覆盖；真实 Kafka/MySQL/Redis/Qdrant 上由 Go worker 完成整条链路的黑盒回归已执行并通过：`scripts/verify-debezium-e2e.py`（debezium/relay）、`scripts/verify-kafka-search.py`（排队搜索）、`scripts/verify-kafka-dlq.py`（DLQ）。

## 11. 开发与测试

```powershell
gofmt -l internal cmd
go vet ./...
go build ./...

# 测试需要一次性 MySQL schema（库名必须含 test），不会回退 SQLite
$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:change-me@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'
go test -count=1 ./...

# 只跑某一部分
go test -count=1 ./internal/runtime/...
go test -count=1 ./internal/httpapi/... ./internal/mcp/...
```

外部解析器 worker（可选，Python 仅用于 PDF/Word 视觉解析）：

```powershell
# 安装 Docling 等依赖（只影响 pdf_worker，不影响 Go 服务）
python -m pip install -r python-workers/requirements.txt
$env:PYTHONPATH = (Resolve-Path .\python-workers).Path
python -m rag_mcp.pdf_worker --help
```

Go 服务会在 ingest 阶段以子进程方式调用 `rag_mcp.pdf_worker` / `rag_mcp.word_vision_worker`，其余后端能力全部在 Go 二进制内；详见 [python-workers/README.md](../python-workers/README.md)。

## 12. 当前限制

- **Qdrant**：仅支持 HTTP 模式；`QDRANT_PATH` 会在启动时被拒绝。
- **Tokenizer**：见 §10；token 预算切分位置可能与 Python 不同。
- **外部解析器**：PDF（Docling）、DOC（LibreOffice）、Word 图片描述都不是 Go 内置能力；未配置时相应功能报错。配置了 `RAG_VLM_BASE_URL` 后 Go 会自动启用 `word_vision_worker`，无图注图片由远端 VLM 生成描述。
- **异步投递**：支持 relay（默认，进程内发件箱发布到 Kafka）、debezium（Debezium Server 读 MySQL binlog，经原生 Kafka sink 写 topic，应用不发布）与本地 worker 模式；Debezium 模式需要 MySQL 状态库。
- **排队搜索**：`RAG_KAFKA_BOOTSTRAP_SERVERS` 一旦设置，MCP `search` 自动改为排队模式；若没有部署 `rag-search-worker`/Redis，搜索会保持排队直至截止。纯同步部署不要配置 Kafka。
- **REST 上传/更新/删除**仍同步执行，大文件索引期间请求会等待；异步只用于 MCP `add_document`。
- **管理边界**：`/v1/admin/*` 仅本机直连；没有账户体系或细粒度多用户权限；工作区不是权限隔离。
- **数据**：状态表用 `rag-state-migrate`；旧知识源 `rag-service-migrate --apply` 不重建向量，需重新上传。旧 Redis 搜索结果不迁移。
- 具体契约差异（cached read views、静态目录列表等剩余的 P1/P2）见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)。





