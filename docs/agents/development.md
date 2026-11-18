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
| 会员/支付宝 | `RAG_MEMBERSHIP_PURCHASE_ENABLED`（`false`）、`RAG_ALIPAY_ENV`（`production`）、`RAG_ALIPAY_APP_ID`、`RAG_ALIPAY_PRIVATE_KEY`、`RAG_ALIPAY_PUBLIC_KEY`、`RAG_ALIPAY_SELLER_ID`、`RAG_ALIPAY_NOTIFY_URL`、`RAG_ALIPAY_RETURN_URL`、`RAG_ALIPAY_GATEWAY`、`RAG_ALIPAY_TIMEOUT_MINUTES`（30） |
| 飞书导入（可选） | `RAG_FEISHU_APP_ID`、`RAG_FEISHU_APP_SECRET`、`RAG_FEISHU_REDIRECT_URI`（三项必须同时配置或同时为空）、`RAG_FEISHU_API_BASE_URL`（`https://open.feishu.cn`）、`RAG_FEISHU_ACCOUNTS_BASE_URL`（`https://accounts.feishu.cn`）、`RAG_FEISHU_TIMEOUT_SECONDS`（20，1..300）；App Secret 只放环境变量，用户 token 加密入库，只导入 docx 纯文本 |
| 状态库 | `RAG_STATE_URL` 或 `RAG_MYSQL_*`、`RAG_MYSQL_DATABASE`（`praxis_rag`）、`RAG_STATE_ENCRYPTION_KEY`、`RAG_STATE_AUTO_SCHEMA` |
| 向量/检索 | `RAG_BACKEND`（服务必须设 `qdrant`）、`QDRANT_URL`、`RAG_COLLECTION`、`RAG_DENSE_VECTOR_SIZE`（1024）、`RAG_EMBEDDING_MODEL`、`RAG_RERANKER_*` |
| Redis | `RAG_REDIS_URL`、`RAG_REDIS_ENABLED`、`RAG_REDIS_PREFIX`、`RAG_REDIS_TTL_SECONDS`（60，展示缓存**随机 TTL 上限**，每次写入取 48～60 秒）、`RAG_REDIS_BREAKER_THRESHOLD`（5，熔断阈值）、`RAG_REDIS_BREAKER_COOLDOWN_SECONDS`（10）、`RAG_CACHE_FALLBACK_CONCURRENCY`（4，回源并发名额）、`RAG_RESULT_TTL_SECONDS`（3600，**artifact 保留期**）、`RAG_RESULT_MAX_BYTES`、`RAG_ARTIFACT_MAX_BYTES` |
| 文档锁 | `RAG_REDIS_LOCK_URL`（空则回退 `RAG_REDIS_URL`；**Redis 是唯一锁后端**，解析不出 URL 时启动报配置错误，旧 `RAG_DOCUMENT_LOCK_MODE` 已移除、残留值被忽略）、`RAG_REDIS_LOCK_PREFIX`（`rag:lock:v1`）、`RAG_REDIS_LOCK_TTL_SECONDS`（120；心跳按 1/3 续租）、`RAG_REDIS_LOCK_TIMEOUT_SECONDS`（1）、`RAG_REDIS_LOCK_WAIT_SECONDS`（2）；与展示缓存开关解耦，所有文档写入/恢复/清理进程必须使用同一 Redis 实例、逻辑库与前缀 |
| Kafka | `RAG_KAFKA_BOOTSTRAP_SERVERS`、`RAG_KAFKA_SECURITY_PROTOCOL`、`RAG_KAFKA_SASL_*`、`RAG_KAFKA_TLS_*`、`RAG_DOCUMENT_TOPIC`/`RAG_DOCUMENT_GROUP`、`RAG_DOCUMENT_DLQ_TOPIC`/`RAG_DOCUMENT_DLQ_GROUP`、`RAG_CACHE_TOPIC`/`RAG_CACHE_GROUP`、`RAG_CACHE_DLQ_TOPIC`/`RAG_CACHE_DLQ_GROUP`、`RAG_DOCUMENT_WORKER_CONCURRENCY`（默认 `1`）；搜索相关的 `RAG_SEARCH_*` topic/group/worker 配置已删除，仅保留 `RAG_SEARCH_TIMEOUT_SECONDS`（120，同步检索总预算） |
| 文档投递/监控 | `RAG_DOCUMENT_BROKER_MAX_RETRIES`/`RAG_DOCUMENT_MAX_ATTEMPTS`（默认 3；与 MySQL `document_jobs.attempts` 的 3 次执行上限相互独立）、`RAG_DOCUMENT_WORKER_HEARTBEAT_SECONDS`（Worker 心跳间隔，默认 15；监控页 60 秒无心跳判降级）、`RAG_CONNECT_URLS`（分布式 Kafka Connect REST 端点）、`RAG_CONNECT_DOCUMENT_CONNECTOR`/`RAG_CONNECT_CACHE_CONNECTOR`（固定连接器名）、`RAG_CONNECT_SUPERVISOR_*`（FAILED task 自动重启间隔/次数/退避）；采集位点在 Connect 内部 topic，应用读不到，监控显示「未知」 |
| 缓存/CDC | `RAG_CACHE_TOPIC`/`RAG_CACHE_GROUP`、`RAG_CACHE_DLQ_TOPIC`/`RAG_CACHE_DLQ_GROUP`；`cache_revisions` 的 revision/refreshed_revision/attempts/last_error/updated_at；任务表 `schedule_version`；`rag-cdc-cutover` 切换工具 |
| PDF | `RAG_PDF_ENGINE`（`vision`/`go`；旧值 `docling` 是 `vision` 的兼容别名）、`RAG_PDF_SERVICE_URL`、`RAG_PDF_SERVICE_TIMEOUT`（900，Go 调用与 pdf-parser 请求总预算）、`RAG_PDF_SERVICE_PARSE_TIMEOUT`（120，真正开始解析后的单次上限，排队等待不计入，须小于总预算）；`RAG_PDF_MODELS_PATH` 已废弃（本地无模型） |
| Word/HTML | `RAG_DOCLING_SERVICE_URL`（必配：Word `.docx`/`.doc` 与 `.html`/`.htm` 只由 Docling 服务解析，进程内 OOXML 解析已删除；未配置时这些格式上传在受理阶段被拒绝）、`RAG_DOCLING_SERVICE_TIMEOUT`（300，1..7200，Go 调用与 docling-service 请求预算）、`RAG_WORD_CONVERTER_PATH` 与 `RAG_WORD_CONVERT_TIMEOUT`（120，`.doc` 先经 LibreOffice 转成 `.docx` 再上传给服务，服务只接收 `.docx`/HTML）；服务自身变量：`RAG_DOCLING_SERVICE_HOST`、`PORT`（默认 8092）、`RAG_DOCLING_SERVICE_TIMEOUT`、`RAG_DOCLING_SERVICE_PARSE_TIMEOUT`、`RAG_DOCLING_SERVICE_CONCURRENCY`、`RAG_DOCLING_SERVICE_WORKERS`、`RAG_DOCLING_SERVICE_MAX_BYTES`、`RAG_DOCLING_SERVICE_LOG_LEVEL`/`RAG_DOCLING_SERVICE_LOG_FORMAT`；容器端口 8092 |
| 知识图谱（可选） | `RAG_LIGHTRAG_SERVICE_URL`（空=禁用 `retrieval=graph`）、`RAG_LIGHTRAG_SERVICE_TOKEN`（侧车 Bearer token；侧车绑定非 loopback 时必填）、`RAG_LIGHTRAG_SERVICE_TIMEOUT`（60，1..600，单次侧车调用预算）；侧车自身变量前缀 `RAG_LIGHTRAG_SERVICE_`（HOST 默认 `127.0.0.1`、PORT 默认 8093、WORKING_DIR、CONCURRENCY、LOG_LEVEL/LOG_FORMAT），上游 `lightrag-hku` 版本 pin 在该目录 `requirements.txt` |
| 日志 | `RAG_LOG_LEVEL`（`info`）、`RAG_LOG_FORMAT`（`text`/`json`）、`RAG_DEBUG_TRACE`（兼容旧调试开关；未设级别时为 `debug`）、`RAG_LOG_FILE`（可选，所有 Go 进程追加同一个 JSON Lines 文件供监控页文件日志源读取；打开失败 fail fast）、`RAG_LOG_BUFFER_SIZE`（`1000`，范围 `100..20000`，进程内环形缓冲）、`RAG_PDF_SERVICE_LOG_LEVEL`/`RAG_PDF_SERVICE_LOG_FORMAT` |
| VLM | `RAG_VLM_BASE_URL`、`RAG_VLM_API_KEY`、`RAG_VLM_MODEL`、`RAG_VLM_TIMEOUT`、`RAG_VLM_CONCURRENCY`（Console 知识库配置优先，环境变量是回退） |
| 默认知识库模板 | `RAG_DEFAULT_KB_TEMPLATE`（默认 `default`，VIP/普通用户建库时克隆其模型配置） |

注意：

- `RAG_BACKEND` 在配置层默认是 `mock`，但 `rag-service` 启动时**只接受 `qdrant`**（`mock` 仅供 `rag-mcp-debug`）。
- `QDRANT_PATH`（Qdrant 本地文件模式）会被拒绝，只用 `QDRANT_URL`。
- 状态库只有 MySQL；未配置 `RAG_STATE_URL` / `RAG_MYSQL_*` 时服务直接启动失败，没有本地文件回退。
- 日志统一走 `internal/logging`（`slog` + 上下文关联 + 脱敏）；入口/worker 安装默认 logger，新代码不要再用 `log.Printf`，也不要只记录 `%T` 错误类型。HTTP 入口统一接受/回显 `X-Request-ID`，上传与后台任务日志带 `operation_id`/`artifact_id`/`batch_id`。`logging.Setup()` 同时装配 Console 监控页的日志来源：进程内环形缓冲（`logging.DefaultBuffer()`，默认 1000 条，`RAG_LOG_BUFFER_SIZE` 可调）与可选 `RAG_LOG_FILE` JSON Lines 文件（`logging.LogFilePath()`）；多进程共写同一文件时依赖 `O_APPEND`，读取端只读最近 512 KiB。日志组件归类优先取显式 `component` 属性，否则按调用方源码包路径映射（见 `internal/logging/logs.go` 的组件表）。
- 主机资源采集在 `internal/sysinfo`（gopsutil/v4 差值采样）：首次采样只建立基线，CPU 使用率与网络速率为 `null`；Windows 的 load 为 `null` 并在 `notes` 说明。`internal/monitor` 的 `Options.System` 是可注入的窄接口（`Sample() (*sysinfo.Snapshot, error)`），采样失败只把 `system` 置为 `null` 并追加 `errors`，不参与总体健康判定。

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
dist/bin/rag-dead-letter-worker(.exe)  死信 worker（--kind document|cache）
dist/bin/rag-service-migrate(.exe)     旧知识源迁移（默认预演）/ --retire-outbox 旧表下线
dist/bin/rag-cdc-cutover(.exe)         CDC 存量切换（盘点/位点/补投/缓存基线/核对）
dist/bin/rag-cache-worker(.exe)        缓存变更消费者
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

worker 与 `rag-service` 必须共享同一 `RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`QDRANT_URL`、`RAG_STATE_ENCRYPTION_KEY`、Redis/Kafka 配置（还必须共享同一组 `RAG_REDIS_LOCK_*`：同一 Redis 实例、逻辑库与前缀——Redis 是文档锁的唯一后端）；它们不继承其他终端里的 `.env`。

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

- `scripts/verify-debezium-e2e.py`（Kafka Connect 注册/监控契约 + 验收 2/3/4 + 真实 Kafka 消息解码）
- `scripts/verify-kafka-dlq.py`（显式 DLQ）

## 5. 改动约束（细项）

1. **对外契约**：MCP 工具名与输入 schema、REST 路径与响应结构、环境变量名、`enc:v1:` 凭据格式、MySQL 表结构、PDF 清单版本（`go-native-v1` / `docling-service-v1`，旧 `docling-formula-remote-v3` 仍可读；`docling-service-v1` 只是证据格式契约，与是否使用 Docling 无关）。改契约必须同时做数据迁移、兼容读取与文档更新。
2. **不要重新引入**：Python 主后端、SQLite 状态库、RocketMQ、Qdrant 本地文件模式。当前传输是 Kafka + MySQL，凭据格式是 `enc:v1:`。
3. **Go 服务不得依赖 Python**：PDF 的版面/OCR/表格/公式/图片描述能力由 `python-workers/pdf-service` 调用远端视觉模型（或纯 Go 实现）提供，Word/HTML 解析由 `python-workers/docling-service` 提供；`go build ./...` / `go test ./...` 不涉及 Python，也不加载任何本地模型。
4. **测试串行**：新增包测试仍受同一 MySQL schema 与 advisory lock 约束；不要写依赖执行顺序之外并行假设的测试。
5. **异步受理不代表完成**：涉及上传/索引的改动必须维护 `document_jobs` 状态机、`schedule_version` 重发语义、幂等与恢复语义，并保留 `GET /v1/document-jobs/{artifact_id}` 的可观测性；不要重新引入任何事务 outbox（Debezium 直接采集任务表）。
6. **不要提交生成物与本地数据**：`build/`、`dist/`、`frontend/node_modules/`、`.test-tmp/`、`.env*`（除 `.env.example`）、模型/向量数据、根目录 worker 二进制（见 `.gitignore`）。
7. **安全默认值**：默认只监听 loopback；非 loopback 必须要求 `RAG_SERVICE_TOKEN` + MCP 白名单；`/v1/admin/*` 默认只允许本机直连或管理员会话；日志只记录任务/操作 ID 与安全错误类别，不记录查询、文件内容、消息正文或凭据。
8. **中文 UI 与文档**：Console 文案、`README.md`、`docs/` 均为中文；新增面向用户的文案保持一致。
9. **行为变化要同步文档**：涉及功能/配置/接口的改动，至少同步 `README.md` 或 `docs/USAGE.md` 的相关段落；做了实机验证的，按现有格式在 `VALIDATION.md` 追加一条带日期的记录。
10. **先看现有实现再动手**：同类能力优先复用 `internal/runtime`、`internal/state`、`internal/kbconfig`，避免引入新的第三方客户端（Qdrant、MySQL、Kafka、MCP 均已有封装）。

## 6. 已知限制

- Token 计数是确定性近似，token 策略的切分边界可能与 Python/BGE-M3 不完全一致。
- vision 引擎的 PDF 必须先在知识库配置 VLM，否则受理阶段直接拒绝；pdf-parser 不可用或超时、远端视觉模型不可用同样会令该次上传失败（可临时切 `RAG_PDF_ENGINE=go`）。OCR 已并入视觉模型，不再有独立 OCR 模型类型；升级迁移会删除旧 `kind='ocr'` 配置并清除知识库中的 `ocr`/`ocr_model` 设置。
- `RAG_PDF_ENGINE=go` 只适合文字型 PDF：无版面分析、表格结构、OCR、公式与图片资产；扫描件需先 OCR。
- 暂不支持 Excel、网页抓取（只支持静态 `.html`/`.htm` 上传，且 HTML 的外部/相对图片不抓取）、独立图片上传；`.doc` 依赖外部 LibreOffice。`.docx`/`.doc`/`.html`/`.htm` 一律由 Docling 服务解析：必须配置 `RAG_DOCLING_SERVICE_URL`，未配置时这些格式的上传在受理阶段被拒绝（进程内 OOXML 解析已删除）。
- 搜索为同步执行，受 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120s）总预算约束；超时映射为既有搜索错误，客户端取消会传递到 Qdrant/模型调用。`search_tasks`/`search_result_snapshots` 为历史表、不再读写；搜索 topic/worker/DLQ 已下线。
- 上传默认异步，必须存在文档消费者：配置 Kafka + 分布式 Kafka Connect 的两个 Debezium 连接器并运行独立的 `rag-document-worker`；没有内嵌/进程内选项，未部署时任务会一直 `queued`。
- 受限账号可在同步检索中正常工作：服务端按 scope 追加 `document_ids` 过滤，不再有“排队模式下拒绝受限账号”的限制。
- 管理凭证（Service Token / API Key）在请求时不做知识库归属校验，只受 API Key 命名空间白名单约束。
- `sag` 检索的事件-实体抽取每文档上限 512 chunk、实体不做全局消歧（casefold 归一化即 join key）；抽取阶段失败不重试（文档发布不受影响，缺口留给 `rag-sag-backfill` CLI）；`SagRepository` 的 MySQL round-trip 测试需要 `RAG_TEST_MYSQL_URL`（未设则跳过，与包内其他 MySQL 测试一致）。

## 7. 未验证项（不要据此宣称生产就绪）

- Docker 镜像构建、compose `app` profile、systemd 部署；Go worker 容器化。
- pdf-parser 镜像构建、真实视觉端点推理与真实扫描 PDF 的解析质量（现有测试用 fake converter 与注入式视觉客户端覆盖协议，不访问网络）。
- docling-service 在本环境未构建镜像、未跑端到端链路；服务自身测试用真实 Docling `2.150.1` 与 DOCX/HTML fixture 覆盖清单契约（见服务 README §6），「LibreOffice → Docling」的 `.doc` 链路与真实业务文档质量仍需预发验收；依赖版本、启动方式与验收记录见 `python-workers/docling-service/README.md`。
- 远端视觉模型端点的真实联调（只做了配置/协议单测与 httptest 集成测试）。
- Console 的浏览器级 E2E（只有 Vitest 单元/契约测试与人工截图核对）。
- Kafka SASL/TLS 外部集群、MySQL 多实例并发/故障切换。
- 分布式 Kafka Connect 双 worker/failover、FAILED task 自动重启、缓存消费者实链路、artifact 独立读取与同步搜索超时/取消、CDC 存量切换/回滚演练（见 `VALIDATION.md`）。
- `rag-mcp-ingest` 未连接真实 Qdrant/模型 API 实跑。
- 飞书导入未用真实飞书企业/账号联调：现有测试用 fake HTTP 服务覆盖 OAuth 换 token、刷新、文件夹/搜索/正文和错误映射；真实环境还需核对应用可用范围、用户可见的文件夹/搜索结果、权限变更后的重新授权，以及回调地址在反向代理后的 HTTPS 可达性（见 `VALIDATION.md`）。

详细的验证边界见 `docs/USAGE.md` §10 与 `VALIDATION.md`。

## 8. 文档索引

| 文档 | 内容 |
| --- | --- |
| `README.md` | 产品概览、快速开始、运行时架构、使用边界 |
| `docs/USAGE.md` | 构建/配置/启动、REST/MCP 示例、worker、迁移 CLI、Docker、验证边界 |
| `docs/DEPLOYMENT.md` | Docker/Compose、systemd、worker 重启策略、DLQ 监控、分布式 Kafka Connect 与升级/回滚 |
| `docs/PERMISSIONS.md` | 角色、知识库归属、读/写与范围、邀请流程与接口 |
| `docs/GO_MIGRATION.md` | Python→Go 迁移总纲、阶段与进度（历史背景） |
| `docs/GO_PARITY_AUDIT.md` | Go 与 Python/前端契约差异、P0/P1/P2 清单 |
| `docs/state-storage.md` | MySQL 表结构、密钥加密、迁移与多实例注意事项 |
| `docs/state-cache.md` | Redis 读模型缓存、revision 水位、缓存消费者与一致性边界 |
| `docs/result-references.md` | artifact 引用、artifact TTL 与容量限制（搜索结果快照/result_id 已下线） |
| `docs/api-keys.md` | API Key 归属、命名空间与 MCP 配置 |
| `docs/knowledge-base-settings.md` | 知识库配置、模型配置与 profile 关系（历史设计 + 当前实现说明） |
| `deploy/kafka-connect/` | 分布式 Kafka Connect 与两个 Debezium 连接器模板、注册/故障演练手册 |
| `deploy/debezium/` | 旧 Debezium Server 配置（仅回滚保留，已下线） |
| `python-workers/pdf-service/README.md` | PDF 视觉解析微服务的构建、视觉模型配置与接口 |
| `docs/cdc-migration.md` | 事务 outbox → Debezium 表级采集的迁移契约：分布式 Kafka Connect、双连接器、事件、调度版本、缓存消费者、存量切换、监控替换 |
| `VALIDATION.md` | 已执行的验证记录（按日期） |
| [docs/PROGRESS.md](../PROGRESS.md) | 早期开发阶段记录（历史，不作当前功能清单） |
| [docs/事务-1057.md](../事务-1057.md) | 事务与一致性审计记录（历史） |
