# 开发与配置

> 本文件是根 `AGENTS.md` 的拆分细节：配置项、构建/运行/测试、改动约束、限制与未验证项。
> 校对基准：仓库代码 + `docs/`（2026-11-11）。

## 1. 配置

优先级：**进程环境变量 > `~/.praxis-rag/config.toml`**（首次自动创建，`RAG_CONFIG_FILE` 可改路径；文件只有一个 `[environment]` 节，键值必须是字符串且在白名单内，未知键会让启动失败）。凭据只放环境变量，不要写入配置文件。

完整表见 `docs/USAGE.md` §3，这里只列必须知道的：

| 领域 | 变量 |
| --- | --- |
| 服务/监听 | `RAG_SERVICE_HOST`（默认 `127.0.0.1`）、`RAG_SERVICE_PORT`（`8011`）、`RAG_SERVICE_TOKEN`、`RAG_SERVICE_DATA_ROOT`、`RAG_WEB_ROOT`、`RAG_MCP_ALLOWED_HOSTS`、`RAG_MCP_ALLOWED_ORIGINS` |
| 账号/上传 | `RAG_AUTH_ENABLED`（`true`）、`RAG_AUTH_REGISTRATION`（`open`）、`RAG_AUTH_SESSION_TTL_HOURS`（168）、`RAG_INVITATION_TTL_HOURS`（168） |
| 状态库 | `RAG_STATE_URL` 或 `RAG_MYSQL_*`、`RAG_MYSQL_DATABASE`（`praxis_rag`）、`RAG_STATE_ENCRYPTION_KEY`、`RAG_STATE_AUTO_SCHEMA` |
| 向量/检索 | `RAG_BACKEND`（服务必须设 `qdrant`）、`QDRANT_URL`、`RAG_COLLECTION`、`RAG_DENSE_VECTOR_SIZE`（1024）、`RAG_EMBEDDING_MODEL`、`RAG_RERANKER_*` |
| Redis | `RAG_REDIS_URL`、`RAG_REDIS_ENABLED`、`RAG_REDIS_PREFIX`、`RAG_RESULT_TTL_SECONDS`（3600）、`RAG_RESULT_MAX_BYTES`、`RAG_ARTIFACT_MAX_BYTES` |
| Kafka | `RAG_KAFKA_BOOTSTRAP_SERVERS`、`RAG_KAFKA_SECURITY_PROTOCOL`、`RAG_KAFKA_SASL_*`、`RAG_KAFKA_TLS_*`、`RAG_*_TOPIC`/`RAG_*_GROUP`、`RAG_*_DLQ_TOPIC`、`RAG_DOCUMENT_WORKER_CONCURRENCY`/`RAG_SEARCH_WORKER_CONCURRENCY`（默认 `1`） |
| 文档投递/监控 | `RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS`（默认 604800）、`RAG_DOCUMENT_BROKER_MAX_RETRIES`/`RAG_DOCUMENT_MAX_ATTEMPTS`（默认 5）、`RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS`（Worker 心跳间隔，默认 15；监控页 60 秒无心跳判降级）、`RAG_DEBEZIUM_HEALTH_URL`（可选只读健康探测；采集位点不可读，监控显示「未知」）、`RAG_APP_DEBEZIUM_HEALTH_URL`（compose app profile 覆盖用）、`RAG_DEBEZIUM_PORT`（宿主机健康端口，默认 8083） |
| PDF/Word | `RAG_PDF_ENGINE`（`vision`/`go`；旧值 `docling` 是 `vision` 的兼容别名）、`RAG_PDF_SERVICE_URL`、`RAG_PDF_SERVICE_TIMEOUT`（900）、`RAG_WORD_CONVERTER_PATH`、`RAG_WORD_CONVERT_TIMEOUT`；`RAG_PDF_MODELS_PATH` 已废弃（本地无模型） |
| 日志 | `RAG_LOG_LEVEL`（`info`）、`RAG_LOG_FORMAT`（`text`/`json`）、`RAG_DEBUG_TRACE`（兼容旧调试开关；未设级别时为 `debug`）、`RAG_PDF_SERVICE_LOG_LEVEL`/`RAG_PDF_SERVICE_LOG_FORMAT` |
| VLM | `RAG_VLM_BASE_URL`、`RAG_VLM_API_KEY`、`RAG_VLM_MODEL`、`RAG_VLM_TIMEOUT`、`RAG_VLM_CONCURRENCY`（Console 知识库配置优先，环境变量是回退） |
| 默认知识库模板 | `RAG_DEFAULT_KB_TEMPLATE`（默认 `default`，VIP/普通用户建库时克隆其模型配置） |

注意：

- `RAG_BACKEND` 在配置层默认是 `mock`，但 `rag-service` 启动时**只接受 `qdrant`**（`mock` 仅供 `rag-mcp-debug`）。
- `QDRANT_PATH`（Qdrant 本地文件模式）会被拒绝，只用 `QDRANT_URL`。
- 状态库只有 MySQL；未配置 `RAG_STATE_URL` / `RAG_MYSQL_*` 时服务直接启动失败，没有本地文件回退。
- 日志统一走 `internal/logging`（`slog` + 上下文关联 + 脱敏）；入口/worker 安装默认 logger，新代码不要再用 `log.Printf`，也不要只记录 `%T` 错误类型。HTTP 入口统一接受/回显 `X-Request-ID`，上传与后台任务日志带 `operation_id`/`artifact_id`/`batch_id`。

## 2. 构建

```powershell
# Windows（PowerShell 5.1+ / 7+）
pwsh -File scripts/build-go.ps1                # Console + Go 二进制 + 本机冒烟
pwsh -File scripts/build-go.ps1 -SkipFrontend  # 复用已有 dist/web
pwsh -File scripts/build-go.ps1 -SkipGo        # 只重建 Console
pwsh -File scripts/build-go.ps1 -Test          # 构建 + 串行 Go 测试 + 冒烟

# POSIX
make build | make frontend | make go | make test | make vet | make check | make smoke | make clean
```

产物：

```text
dist/bin/rag-service(.exe)             REST + MCP + Console
dist/bin/rag-document-worker(.exe)     异步文档 worker
dist/bin/rag-search-worker(.exe)       排队搜索 worker
dist/bin/rag-dead-letter-worker(.exe)  死信 worker
dist/bin/rag-service-migrate(.exe)     旧知识源迁移（默认预演）
dist/bin/rag-state-migrate(.exe)       状态表结构检查/创建
dist/bin/rag-mcp-debug(.exe)           mock 检索调试
dist/bin/rag-mcp-ingest(.exe)          本地批量导入
dist/web/                              Console 构建产物（RAG_WEB_ROOT 指向它）
```

只使用 API/MCP 时可不构建前端（此时 `/console` 返回 404）。

## 3. 运行

```powershell
# 基础设施（MySQL 3307、Qdrant 6333/6334、Redis 6379、Kafka 9092 + pdf-parser 8091）
Copy-Item .env.example .env   # 必填 RAG_MYSQL_PASSWORD、MYSQL_ROOT_PASSWORD、RAG_STATE_ENCRYPTION_KEY
docker compose up -d

# Go 服务（app profile 构建并启动；容器绑定 0.0.0.0，必须提供 RAG_SERVICE_TOKEN）
docker compose --profile app up -d --build

# 一键：起依赖、建 topic、迁移、启动 rag-service + rag-document-worker
pwsh -File scripts/start-stack.ps1
```

人工启动：

```powershell
$env:RAG_SERVICE_HOST = "127.0.0.1"
$env:RAG_SERVICE_PORT = "8011"
$env:QDRANT_URL = "http://127.0.0.1:6333"
$env:RAG_WEB_ROOT = "$PWD/dist/web"
.\dist\bin\rag-service.exe
```

默认地址：Console `/console`、MCP `/mcp`、API 文档 `/docs`（机器可读 `/openapi.json`）、健康检查 `/health`。

worker 与 `rag-service` 必须共享同一 `RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`QDRANT_URL`、`RAG_STATE_ENCRYPTION_KEY`、Redis/Kafka 配置；它们不继承其他终端里的 `.env`。

## 4. 测试

```powershell
# 后端：一次性 MySQL schema，库名必须含 "test"，必须串行
$env:RAG_TEST_MYSQL_URL = 'mysql+pymysql://rag:change-me@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4'
go vet ./...
go test -p 1 -count=1 -timeout 25m ./...
```

- `-p 1` 不是建议而是**必须**：所有包共用同一个测试 schema，靠 advisory lock 互斥；并行会排队甚至超时。
- `internal/e2e` 是真实 HTTP 全栈测试：真实 MySQL + 真实 `httpapi`/`runtime`/`access`/`auth`，模型、Qdrant、Redis（miniredis）、broker 用进程内 fake。
- 只跑某一部分时仍要保证同一时间只跑一个包：`go test -p 1 -count=1 -timeout 10m ./internal/runtime/`。

```powershell
# 前端
cd frontend
npm ci
npm run typecheck   # 含测试文件
npm test            # Vitest（jsdom，src/**/*.test.ts(x)）
npm run build       # tsc + vite build
```

CI 见 `.github/workflows/test.yml`：`go vet + go build + go test -p 1`（MySQL 8 service，库名 `praxis_unit_test`）与前端 `typecheck + test + build`。

真实基础设施上的验证脚本（需要 Docker + Python 3）：

- `scripts/verify-debezium-e2e.py`（Debezium 文档通知链路）
- `scripts/verify-kafka-dlq.py`（显式 DLQ）
- `scripts/verify-kafka-search.py`（排队搜索 + Redis 快照）

## 5. 改动约束（细项）

1. **对外契约**：MCP 工具名与输入 schema、REST 路径与响应结构、环境变量名、`enc:v1:` 凭据格式、MySQL 表结构、PDF 清单版本（`go-native-v1` / `docling-service-v1`，旧 `docling-formula-remote-v3` 仍可读；`docling-service-v1` 只是证据格式契约，与是否使用 Docling 无关）。改契约必须同时做数据迁移、兼容读取与文档更新。
2. **不要重新引入**：Python 主后端、SQLite 状态库、RocketMQ、Qdrant 本地文件模式。当前传输是 Kafka + MySQL，凭据格式是 `enc:v1:`。
3. **Go 服务不得依赖 Python**：PDF 的版面/OCR/表格/公式/图片描述能力由 `python-workers/pdf-service` 调用远端视觉模型（或纯 Go 实现）提供；`go build ./...` / `go test ./...` 不涉及 Python，也不加载任何本地模型。
4. **测试串行**：新增包测试仍受同一 MySQL schema 与 advisory lock 约束；不要写依赖执行顺序之外并行假设的测试。
5. **异步受理不代表完成**：涉及上传/索引的改动必须维护 `document_jobs` 状态机、发件箱、幂等与恢复语义，并保留 `GET /v1/document-jobs/{artifact_id}` 的可观测性。
6. **不要提交生成物与本地数据**：`build/`、`dist/`、`frontend/node_modules/`、`.test-tmp/`、`.env*`（除 `.env.example`）、模型/向量数据、根目录 worker 二进制（见 `.gitignore`）。
7. **安全默认值**：默认只监听 loopback；非 loopback 必须要求 `RAG_SERVICE_TOKEN` + MCP 白名单；`/v1/admin/*` 默认只允许本机直连或管理员会话；日志只记录任务/操作 ID 与安全错误类别，不记录查询、文件内容、消息正文或凭据。
8. **中文 UI 与文档**：Console 文案、`README.md`、`docs/` 均为中文；新增面向用户的文案保持一致。
9. **行为变化要同步文档**：涉及功能/配置/接口的改动，至少同步 `README.md` 或 `docs/USAGE.md` 的相关段落；做了实机验证的，按现有格式在 `VALIDATION.md` 追加一条带日期的记录。
10. **先看现有实现再动手**：同类能力优先复用 `internal/runtime`、`internal/state`、`internal/kbconfig`，避免引入新的第三方客户端（Qdrant、MySQL、Kafka、MCP 均已有封装）。

## 6. 已知限制

- Token 计数是确定性近似，token 策略的切分边界可能与 Python/BGE-M3 不完全一致。
- vision 引擎的 PDF 必须先在知识库配置 VLM，否则受理阶段直接拒绝；pdf-parser 不可用或超时、远端视觉模型不可用同样会令该次上传失败（可临时切 `RAG_PDF_ENGINE=go`）。OCR 已并入视觉模型，不再有独立 OCR 模型类型；升级迁移会删除旧 `kind='ocr'` 配置并清除知识库中的 `ocr`/`ocr_model` 设置。
- `RAG_PDF_ENGINE=go` 只适合文字型 PDF：无版面分析、表格结构、OCR、公式与图片资产；扫描件需先 OCR。
- 暂不支持 Excel、网页抓取、独立图片上传；`.doc` 依赖外部 LibreOffice。
- 排队搜索依赖 Kafka + Redis；结果快照过期/丢失后无法回放；死信 topic 需要显式创建。
- 上传默认异步，必须存在文档消费者：配置 Kafka + Debezium 并运行独立的 `rag-document-worker`；没有内嵌/进程内选项，未部署时任务会一直 `queued`。
- 受限账号在启用 Kafka 的部署下不支持排队检索（同步检索不受影响）。
- 管理凭证（Service Token / API Key）在请求时不做知识库归属校验，只受 API Key 命名空间白名单约束。

## 7. 未验证项（不要据此宣称生产就绪）

- Docker 镜像构建、compose `app` profile、systemd 部署；Go worker 容器化。
- pdf-parser 镜像构建、真实视觉端点推理与真实扫描 PDF 的解析质量（现有测试用 fake converter 与注入式视觉客户端覆盖协议，不访问网络）。
- 远端视觉模型端点的真实联调（只做了配置/协议单测与 httptest 集成测试）。
- Console 的浏览器级 E2E（只有 Vitest 单元/契约测试与人工截图核对）。
- Kafka SASL/TLS 外部集群、MySQL 多实例并发/故障切换。
- `rag-mcp-ingest` 未连接真实 Qdrant/模型 API 实跑。

详细的验证边界见 `docs/USAGE.md` §10 与 `VALIDATION.md`。

## 8. 文档索引

| 文档 | 内容 |
| --- | --- |
| `README.md` | 产品概览、快速开始、运行时架构、使用边界 |
| `docs/USAGE.md` | 构建/配置/启动、REST/MCP 示例、worker、迁移 CLI、Docker、验证边界 |
| `docs/DEPLOYMENT.md` | Docker/Compose、systemd、worker 重启策略、DLQ 监控、outbox 投递模式 |
| `docs/PERMISSIONS.md` | 角色、知识库归属、读/写与范围、邀请流程与接口 |
| `docs/GO_MIGRATION.md` | Python→Go 迁移总纲、阶段与进度（历史背景） |
| `docs/GO_PARITY_AUDIT.md` | Go 与 Python/前端契约差异、P0/P1/P2 清单 |
| `docs/state-storage.md` | MySQL 表结构、密钥加密、迁移与多实例注意事项 |
| `docs/state-cache.md` | Redis 读模型缓存、修订与 outbox、一致性边界 |
| `docs/result-references.md` | `result_id`、快照 TTL、artifact 引用与容量限制 |
| `docs/api-keys.md` | API Key 归属、命名空间与 MCP 配置 |
| `docs/knowledge-base-settings.md` | 知识库配置、模型配置与 profile 关系（历史设计 + 当前实现说明） |
| `deploy/debezium/README.md` | Debezium CDC 桥接配置 |
| `python-workers/pdf-service/README.md` | PDF 视觉解析微服务的构建、视觉模型配置与接口 |
| `VALIDATION.md` | 已执行的验证记录（按日期） |
| `PROGRESS.md` | 早期开发阶段记录（历史，不作当前功能清单） |
