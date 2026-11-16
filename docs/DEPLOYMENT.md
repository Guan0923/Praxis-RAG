# Praxis-RAG 部署指南（Go 后端）

本文给出 `rag-service` 与三个 worker（文档/缓存/死信）的生产部署模板：Docker/Compose、systemd、重启策略、DLQ 监控和上线顺序。搜索为同步执行、不部署 worker；**搜索同步化迁移与回滚见 §12**。安装、环境变量与 API 用法见 [使用与部署](USAGE.md)；迁移背景与当前缺口见 [GO_MIGRATION](GO_MIGRATION.md)；契约差异见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)。

> 本文的镜像构建、`app`/worker Compose profile 与 systemd 示例未在本仓库完整实机运行。2026-11-15 的 Kafka Connect 改造（两个 Debezium MySQL Connector 运行在双 worker 的分布式 Connect 集群，连接器配置/位点/task 状态存 Kafka 内部 topic）**尚未在真实 Docker/Kafka Connect/MySQL 链路上验证**；此前 Debezium Server 文件位点的实机记录对应已下线的部署。生产上线前请按 [Kafka Connect 操作手册](../deploy/kafka-connect/README.md)、[CDC 迁移契约](cdc-migration.md) §8、§12 与本文 §6、§12 逐项演练并记录到 [VALIDATION](../VALIDATION.md)。

## 1. 进程拓扑

```text
                  ┌────────────────────────────┐
浏览器/MCP/REST ──>│ rag-service :8011          │──> Qdrant（HTTP :6333）
                  │ REST /v1, MCP /mcp, /console│──> 共享状态库 MySQL
                  └────────────┬───────────────┘──> Redis（可选：读模型展示缓存）
                               │ Kafka（文档/缓存两类通知；文档入库必需）
        MySQL binlog ──> Kafka Connect 集群（connect-1 / connect-2，同一 GROUP_ID）
                         ├─ praxis-document-cdc ──> praxis_document_ingest
                         └─ praxis-cache-cdc    ──> praxis_state_cache_refresh
             ┌─────────────────┴──────────────────────┐
             ▼                                        ▼
  rag-document-worker                     rag-cache-worker
  （文档任务）                             （缓存变更）
             └─────────────────┬──────────────────────┘
                               ▼
  rag-dead-letter-worker --kind {document,cache}（显式 DLQ topic: RAG_*_DLQ_TOPIC）

  rag-service / rag-document-worker ──HTTP POST /v1/parse──> pdf-parser :8091
                                                             （PyMuPDF 读取/渲染/裁图）
                                                                    │
                                                                    ▼
                                                        远端 OpenAI 兼容视觉模型
                                                        （版面/OCR/表格/公式/图片描述）
```

连接器配置、source offset（binlog 位点）与 task 状态保存在 Kafka 内部 topic
`praxis_connect_configs` / `praxis_connect_offsets` / `praxis_connect_status`；一个
Connect worker 宕机后，peer 接管 task 并从保存位点继续，不依赖本地文件。

- **按部署形态选择进程**：`rag-service` 只受理上传、同步搜索与后台维护，不内嵌任何消费者；文档/缓存/死信 worker 全部独立部署。文档解析必须由 `rag-document-worker` 通过 Kafka 消费完成；未部署时上传仍会受理，但文档任务保持 `queued`。缓存变更由 `rag-cache-worker` 消费；未部署时 Redis 读模型最终一致会延后（周期补偿也依赖该进程），但 MySQL 事实不受影响。worker 不监听端口。
- **PDF 解析引擎可选**：默认 `RAG_PDF_ENGINE=vision` 时，`rag-service`/`rag-document-worker` 通过 `RAG_PDF_SERVICE_URL` 调用独立进程 `pdf-parser`（容器端口 8091，健康检查 `/healthz`）；它不连接状态库/Qdrant，本地只做 PDF 读取/渲染/裁图与结果组装，版面/OCR/表格/公式/图片描述统一调用远端视觉模型，最终把 PDF 转成 zip（`manifest.json` + 图片资产）。`RAG_PDF_ENGINE=go` 时 pdf-parser 不需要运行，PDF 由 Go 内置纯文本引擎处理。旧值 `docling` 作为兼容别名等价于 `vision`。详见 §3.5。
- 所有进程必须共享同一份基础设施配置：`RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`RAG_BACKEND=qdrant`/`QDRANT_URL`、`RAG_STATE_ENCRYPTION_KEY`、Redis 与 Kafka（bootstrap、topic/group、DLQ）设置。配置不一致会造成 401/找不到任务/重复发布等难以排查的问题。
- 上传默认异步受理：`rag-service + Qdrant + 状态库` 只能受理；完整的解析/索引还需要 Kafka + **分布式 Kafka Connect 集群（含两个 Debezium MySQL 连接器）** + `rag-document-worker`（`scripts/start-stack.ps1` 在开发环境会自动启动）；缓存收敛需要 cache worker。搜索在 `rag-service` 内同步执行，不需要 search worker、搜索 topic 或 Redis。

## 2. 二进制与目录布局

```text
/opt/praxis-rag/
  bin/rag-service
  bin/rag-document-worker
  bin/rag-cache-worker
  bin/rag-dead-letter-worker
  bin/rag-service-migrate
  bin/rag-cdc-cutover
  bin/rag-connect            # Kafka Connect 注册/等待/状态/FAILED task 监督
  bin/rag-state-migrate
  bin/rag-mcp-debug
  web/                     # Console 构建产物（dist/web）
/etc/praxis-rag/
  env                      # 仅 root 可读的密钥/环境文件
  config.toml              # 非敏感默认配置（[environment]）
/var/lib/praxis-rag/       # RAG_SERVICE_DATA_ROOT（上传恢复工作区）
```

`rag-service` 当前目录或可执行文件邻居中的 `web` 目录可作为 Console 默认位置；生产建议显式设置 `RAG_WEB_ROOT=/opt/praxis-rag/web`。`config.toml` 里不要写密码；密码、Token、加密密钥放 `env` 文件（权限 `0600`）。

## 3. Docker 部署

### 3.1 构建镜像

```bash
docker build -t praxis-rag:local .
# 受限网络：
docker build \
  --build-arg NPM_REGISTRY=https://registry.npmmirror.com \
  --build-arg GOPROXY=https://goproxy.cn,direct \
  -t praxis-rag:local .

# PDF 解析微服务镜像（独立于 app 镜像；纯 Python + PyMuPDF，不安装本地推理依赖）
docker build -t praxis-rag-pdf-parser:local python-workers/pdf-service
```

镜像内容：`/app/bin/`（全部命令，入口默认 `/app/bin/rag-service`）、`/app/web`（Console）、`/app/connect/connectors`（两个连接器模板，供 `connect-init` 注册）、`/app/config.toml`（`0.0.0.0:8011`、`/app/data`、Qdrant 服务名、loopback MCP 白名单）；非 root 用户 `rag`，健康检查 `/health`，数据卷 `/app/data`。`pdf-parser` 是另一个镜像（FastAPI + PyMuPDF + httpx），监听 8091、健康检查 `/healthz`，本地不加载模型，与 Go 镜像无关。

### 3.2 运行 rag-service

容器内绑定 `0.0.0.0`，`RAG_SERVICE_TOKEN` 与 MCP 白名单是**启动硬要求**（镜像配置已提供 loopback 默认 allowlist，但非 loopback 宿主必须设置 token）：

```bash
docker run -d --name praxis-rag-service \
  --restart unless-stopped \
  -p 127.0.0.1:8011:8011 \
  -e RAG_SERVICE_TOKEN="$(openssl rand -hex 32)" \
  -e QDRANT_URL="http://qdrant:6333" \
  -e RAG_STATE_URL="mysql+pymysql://rag:${RAG_MYSQL_PASSWORD}@mysql:3306/praxis_rag?charset=utf8mb4" \
  -e RAG_STATE_ENCRYPTION_KEY="${RAG_STATE_ENCRYPTION_KEY}" \
  -e RAG_KAFKA_BOOTSTRAP_SERVERS="kafka:9092" \
  -e RAG_REDIS_URL="redis://redis:6379/0" \
  -v praxis-rag-data:/app/data \
  praxis-rag:local
```

- 公开到宿主机时加 TLS 反向代理；不要直接把 8011 暴露到公网。
- 使用 MySQL 时先执行一次 schema 检查/建表（见 §6）：`docker run --rm --entrypoint /app/bin/rag-state-migrate ... --apply`。
- 镜像不含 Python 或 LibreOffice：DOCX 解析与 Word 图片描述（HTTP VLM）由 Go 进程内完成；PDF 高保真解析调用独立的 `pdf-parser`（见 §3.5），使用 `RAG_PDF_ENGINE=go` 时不需要它；只有 .doc 转换需要把 LibreOffice bind mount 进容器并设置 `RAG_WORD_CONVERTER_PATH`；转换进程的路径必须在容器内可见。
- PDF 高保真解析：设置 `RAG_PDF_ENGINE=vision` 并把 `RAG_PDF_SERVICE_URL` 指向 pdf-parser（同一 compose 网络用 `http://pdf-parser:8091`；单独 `docker run` 时用从容器内可达的地址，如 `http://host.docker.internal:8091`）；调用总超时用 `RAG_PDF_SERVICE_TIMEOUT`（默认 900 秒），真正开始解析后的单次上限用 `RAG_PDF_SERVICE_PARSE_TIMEOUT`（默认 120 秒，排队等待不计入）。pdf-parser 不可用时 vision 上传会失败，可切回 `RAG_PDF_ENGINE=go`。
- Word 无图注图片的描述调用远端 OpenAI 兼容视觉模型：优先在 Console 的知识库设置页配置（URL/Key/模型），也可用 `RAG_VLM_BASE_URL`、`RAG_VLM_API_KEY`、`RAG_VLM_MODEL`（可选 `RAG_VLM_TIMEOUT`/`RAG_VLM_CONCURRENCY`）作为未配置知识库时的环境回退。图片内容会离开本机到该端点，选型时确认合规；未配置时这些图片保留占位描述，上传不会失败。该视觉模型端点同时用于 vision 引擎的 PDF 版面/OCR/表格/公式/图片描述；vision 引擎要求知识库配置 VLM（Console「模型配置」页添加并在知识库表单选择），否则 PDF 上传会在受理阶段被拒绝并提示配置。
- 数据目录卷保存上传恢复工作区与 artifact 工作区；不要用匿名卷。

### 3.3 Compose：app profile

仓库 `docker-compose.yml` 默认启动基础设施（含 PDF 解析微服务 `pdf-parser`）；`app` profile 额外构建并启动 Go 服务：

```bash
docker compose --profile app up -d --build
# 只用 Go 纯文本引擎时，默认 compose 可跳过 pdf-parser：
docker compose up -d mysql qdrant redis kafka
```

`app` 服务从 `.env` 读取 `RAG_SERVICE_TOKEN`（容器 0.0.0.0 绑定，必填）、`RAG_MYSQL_*`、`RAG_STATE_ENCRYPTION_KEY`、`RAG_APP_QDRANT_URL`、`RAG_APP_PORT`、`RAG_APP_KAFKA_BOOTSTRAP_SERVERS`（默认 `kafka:9092`）、`RAG_AUTH_*`（Console 登录，见下）、`RAG_FEISHU_*`（可选飞书导入，见下）、`RAG_PDF_ENGINE`（默认 `vision`）、`RAG_PDF_SERVICE_URL`（compose 内默认 `http://pdf-parser:8091`）、`RAG_PDF_SERVICE_TIMEOUT`（默认 900）、`RAG_PDF_SERVICE_PARSE_TIMEOUT`（默认 120）等；默认依赖 MySQL healthy + Qdrant started + Kafka healthy + pdf-parser started。`pdf-parser` 没有 profile，默认 `docker compose up -d` 即启动；它只在 `vision` 引擎下被调用。详细的环境变量见 §3.5。

### 3.4 Compose：worker 示例（需自行保存为 override 文件）

仓库镜像包含 worker 二进制，但 `app` profile 只启动 `rag-service`。推荐为 worker 写 override（保存为 `deploy/compose.workers.yml` 一类文件，随部署仓库管理）：

```yaml
services:
  document-worker:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-document-worker"]
    restart: on-failure:5
    env_file: [.env]
    environment:
      RAG_STATE_URL: ${RAG_STATE_URL:-mysql+pymysql://rag:${RAG_MYSQL_PASSWORD}@mysql:3306/praxis_rag?charset=utf8mb4}
      RAG_STATE_ENCRYPTION_KEY: ${RAG_STATE_ENCRYPTION_KEY}
      QDRANT_URL: http://qdrant:6333
      RAG_KAFKA_BOOTSTRAP_SERVERS: ${RAG_APP_KAFKA_BOOTSTRAP_SERVERS:-kafka:9092}
      RAG_REDIS_URL: redis://redis:6379/0
    depends_on:
      mysql: { condition: service_healthy }
      qdrant: { condition: service_started }
      kafka: { condition: service_healthy }

  cache-worker:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-cache-worker"]
    restart: on-failure:5
    env_file: [.env]
    environment:
      RAG_STATE_URL: ${RAG_STATE_URL:-mysql+pymysql://rag:${RAG_MYSQL_PASSWORD}@mysql:3306/praxis_rag?charset=utf8mb4}
      RAG_STATE_ENCRYPTION_KEY: ${RAG_STATE_ENCRYPTION_KEY}
      RAG_KAFKA_BOOTSTRAP_SERVERS: ${RAG_APP_KAFKA_BOOTSTRAP_SERVERS:-kafka:9092}
      RAG_REDIS_URL: redis://redis:6379/0
    depends_on:
      mysql: { condition: service_healthy }
      kafka: { condition: service_healthy }

  dead-letter-document:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-dead-letter-worker", "--kind", "document"]
    restart: on-failure:5
    env_file: [.env]
    depends_on:
      mysql: { condition: service_healthy }
      kafka: { condition: service_healthy }

  dead-letter-cache:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-dead-letter-worker", "--kind", "cache"]
    restart: on-failure:5
    env_file: [.env]
    depends_on:
      mysql: { condition: service_healthy }
      kafka: { condition: service_healthy }
```

```bash
docker compose -f docker-compose.yml -f deploy/compose.workers.yml --profile workers up -d
```

注意：

- Compose 不能按退出码区分重启。`on-failure:5` 会重启所有非 0（包括配置错误 `2`）；生产请同时用监控/日志告警识别 `2`，或改用 systemd（见 §4）精确控制。
- 共用 `.env` 时 `RAG_CONFIG_FILE` 不要指向宿主机路径（容器内文件不存在）；直接使用环境变量即可。
- worker 与 `rag-service` 必须共用同一个 `RAG_STATE_URL`、Qdrant、Redis、Kafka（bootstrap/topic/group）；不要给不同进程建不同状态库或队列。
- `document-worker` 不能省略：它是唯一的文档消费者，只通过 Kafka 消费文档通知。未配置 Kafka 时 `rag-service` 仍可启动并受理上传，但文档任务会一直 `queued`。
- `cache-worker` 负责把缓存变更收敛到 Redis；不部署不会影响 MySQL 事实与任务处理，但手工 SQL/业务写入后的读模型可能长期陈旧。它同样只通过 Kafka 消费，不监听端口。
- 采集层是分布式 Kafka Connect 集群（生产至少两台机器，同一 `RAG_CONNECT_GROUP_ID`、同一 `DEBEZIUM_CONNECT_VERSION`）。Compose 的 `connect-1`/`connect-2` 与一次性 `connect-init` 注册/等待服务必须先于业务 worker 就绪；宿主机开发流程由 `scripts/start-stack.ps1` 注册并等待两个连接器/task `RUNNING`。详细契约见 [deploy/kafka-connect/README.md](../deploy/kafka-connect/README.md)。

### 3.5 PDF 解析微服务（pdf-parser）

`pdf-parser` 是独立容器/进程（`python-workers/pdf-service`：FastAPI + PyMuPDF + httpx），本地只负责
PDF 读取、渲染、裁图与清单组装；版面分析、OCR、表格结构、公式 LaTeX 与图片描述都通过
**远端 OpenAI 兼容视觉模型**完成。Go 服务只通过 HTTP 调用它，Python 不会进入 app 镜像。接口固定：
`GET /healthz`、`POST /v1/parse`（multipart 字段 `file` + 可选文本字段 `options`，成功返回
`application/zip`，根目录含 `manifest.json` 与图片资产）；清单版本固定 `docling-service-v1`
（证据格式契约，与实现无关，保留旧名字是为了历史数据兼容）。使用 `RAG_PDF_ENGINE=go` 时无需部署。

视觉模型按知识库下发：Go 服务在 `POST /v1/parse` 的 multipart 中附带文本字段 `options`
（`{"vision":{"base_url","api_key","model","timeout","concurrency"}}`），值为该知识库在 Console 中选中的
VLM 配置，`api_key` 不会写入日志；旧键 `vlm` 仍被接受为兼容别名，已废除的 `ocr` 键会返回 422。
未带 `options` 的请求回退到下表的 `RAG_VLM_*` 环境变量。vision 引擎下知识库未配置 VLM 时，
Go 服务在受理上传时即返回中文提示并拒绝，不会调用本服务。

Compose 部署（`pdf-parser` 没有 profile，默认 `docker compose up -d` 就会启动）：

```bash
docker compose up -d --build pdf-parser
docker compose logs -f pdf-parser
curl -fsS http://127.0.0.1:8091/healthz    # 200 且 status=ok 表示就绪；degraded 时解析请求会 503
```

不用 compose 时可直接运行镜像（环境变量与 compose 一致）：

```bash
docker run -d --name praxis-rag-pdf-parser \\
  --restart unless-stopped \\
  -p 127.0.0.1:8091:8091 \\
  -e RAG_PDF_SERVICE_CONCURRENCY=1 \\
  -e RAG_PDF_SERVICE_TIMEOUT=900 \\
  -e RAG_VLM_BASE_URL="https://llm.internal/v1" \\
  -e RAG_VLM_API_KEY="<key>" \\
  -e RAG_VLM_MODEL="qwen2.5-vl-7b" \\
  praxis-rag-pdf-parser:local
```

环境变量：

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `RAG_PDF_SERVICE_HOST` | `0.0.0.0` | 服务进程监听地址（uvicorn CLI 参数优先） |
| `PORT` | `8091` | 服务进程监听端口；Docker/Compose 内固定 8091 |
| `RAG_PDF_SERVICE_PORT` | `8091` | 仅用于 compose 的宿主发布端口；容器内固定 8091，改宿主端口后要同步 Go 端 `RAG_PDF_SERVICE_URL` |
| `RAG_PDF_SERVICE_WORKERS` | `1` | 容器内 Uvicorn 服务进程数（正整数）；默认入口 `python -m app.main` 读取。所有进程共用同一端口 |
| `RAG_PDF_SERVICE_CONCURRENCY` | `1` | 每进程并发解析请求数（`asyncio.Semaphore` 许可数）。本地不再加载模型权重，内存主要随渲染 DPI 与并发解析数增长；内存/CPU 紧张时保持 1 靠副本扩容 |
| `RAG_PDF_SERVICE_TIMEOUT` | `900` 秒 | Go 调用 pdf-parser 的总超时；pdf-parser 用它作为总预算，排队等待最多 `本值 - RAG_PDF_SERVICE_PARSE_TIMEOUT`（排满 503） |
| `RAG_PDF_SERVICE_PARSE_TIMEOUT` | `120` 秒 | pdf-parser 真正开始转换后的单次解析上限（超时 504），须小于 `RAG_PDF_SERVICE_TIMEOUT`；同一 PDF 的下一轮解析会等待上一轮线程停止，避免同时占用两个名额（进程内 gate；多 Uvicorn 进程不共享，严格串行时保持 `WORKERS=1`） |
| `RAG_PDF_SERVICE_MAX_BYTES` | `104857600` | 单次上传上限（字节），超限 413 |
| `RAG_PDF_SERVICE_CONVERTER` | `vision` | `vision`：远端视觉解析；旧值 `docling` 作为兼容别名归一化为 `vision`；`fake` 仅在测试/联调时返回固定清单，不要用于生产 |
| `RAG_VLM_BASE_URL` / `RAG_VLM_API_KEY` / `RAG_VLM_MODEL` | 空 | 视觉模型的**环境回退**（请求 `options` 与 Console 知识库配置优先）；`RAG_VLM_BASE_URL` 与 `RAG_VLM_MODEL` 必须**同时**配置，只配一半按配置错误处理 |
| `RAG_VLM_TIMEOUT` / `RAG_VLM_CONCURRENCY` | `120` 秒 / `4` | 视觉请求超时与页级并发 |
| `RAG_VLM_MAX_TOKENS` | `4096` | 单次视觉输出上限（可选） |
| `RAG_PDF_PAGE_DPI` | `200` | 页面渲染 DPI（可选，范围 72–300） |
| `RAG_VLM_PROMPT_VERSION` | `vision-v1` | 提示词版本（可选，仅记录到 `manifest.parser`） |

视觉模型与离线部署：

- **不再有本地模型或模型卷**：`pdf_parser_models` 卷已删除，无需任何模型下载；`RAG_PDF_MODELS_PATH`
  已废弃，服务不再读取。
- 模型选择：请求 `options.vision`（或旧别名 `options.vlm`）优先，否则用 `RAG_VLM_*`；
  `RAG_VLM_BASE_URL` 与 `RAG_VLM_MODEL` 必须同时配置，只配一半时 `/healthz` 显示 `degraded`
  且 `/v1/parse` 返回 503。
- 离线内网部署时，把 `base_url` 指向内网的 OpenAI 兼容视觉端点即可；PDF 页面渲染图与裁图会发送到
  该端点，选型与合规评估时需确认。
- 升级注意：旧部署的 `pdf_parser_models` 卷可以直接删除
  （`docker volume rm <项目名>_pdf_parser_models`）；数据库一次性迁移会删除 `kind='ocr'` 的模型配置，
  并清除知识库设置中的遗留 `ocr`/`ocr_model` 键。

资源、端口与安全：

- CPU 与内存：本地不再加载任何模型权重，单个解析进程的内存主要由页面渲染（`RAG_PDF_PAGE_DPI`）
  与 PDF 页数决定，通常远小于旧的本地推理镜像；远端视觉模型的吞吐与限流由部署侧负责。
  `RAG_PDF_SERVICE_CONCURRENCY=1` 表示每个进程一次只处理一个 PDF；提高 Go worker 总任务并发时，
  要核对 PDF 端的总解析容量（服务进程数 × 每进程解析并发）和容器 CPU/内存。
- 进程数与进程内并发分开配置：`RAG_PDF_SERVICE_WORKERS=1`、`RAG_PDF_SERVICE_CONCURRENCY=4`
  表示一个服务进程最多同时解析 4 份；改为 `2` 和 `2` 时，两个进程总解析容量上限仍为 4。
  进程之间不共享信号量；连接复用可能使任务分布不均。修改后执行
  `docker compose up -d --build pdf-parser`，不需要增加端口或修改 Go 客户端地址。
- 只发布 `127.0.0.1:${RAG_PDF_SERVICE_PORT:-8091}`（容器内固定 8091）。`POST /v1/parse`
  没有认证，只应在本机/内网暴露，不要直接挂公网或反向代理。修改宿主端口后要同步 Go 端
  `RAG_PDF_SERVICE_URL`。
- `GET /healthz` 用于健康检查；compose 用 Python 内置 `urllib` 探测，无模型冷加载，
  `start_period` 30 秒即可。
- 需要 systemd 时可参照 `python-workers/pdf-service/README.md` 的 uvicorn 启动方式
  （`app.main:app`，端口 8091）编写单元，环境变量同上，并在 `rag-service` 之前启动。

与其他解析路径的关系：

- `.doc` 转换仍由 Go 服务调用外部 LibreOffice，不经过 pdf-parser；DOCX 解析仍在 Go 进程内完成。
- `RAG_PDF_ENGINE=go` 时 PDF 由 Go 纯文本引擎处理（无版面/表格/OCR/公式），此时 Go 服务不调用
  pdf-parser，可以 `docker compose stop pdf-parser` 释放资源；但下次 `docker compose up -d`
  仍会按默认 profile 启动它，可用 `docker compose up -d mysql qdrant redis kafka` 跳过。
  使用 compose 的 `app` profile 时，`app` 的 `depends_on` 要求 pdf-parser 已启动（`service_started`）。

## 4. systemd 部署

### 4.1 环境文件

```ini
# /etc/praxis-rag/env   (chmod 600, root:p raxis)
RAG_CONFIG_FILE=/etc/praxis-rag/config.toml
RAG_SERVICE_HOST=127.0.0.1
RAG_SERVICE_PORT=8011
RAG_WEB_ROOT=/opt/praxis-rag/web
RAG_SERVICE_DATA_ROOT=/var/lib/praxis-rag
RAG_STATE_URL=mysql+pymysql://rag:CHANGE_ME@127.0.0.1:3306/praxis_rag?charset=utf8mb4
RAG_STATE_ENCRYPTION_KEY=base64:CHANGE_ME
RAG_BACKEND=qdrant
QDRANT_URL=http://127.0.0.1:6333
RAG_REDIS_URL=redis://127.0.0.1:6379/0
RAG_KAFKA_BOOTSTRAP_SERVERS=127.0.0.1:9092
# 外部 Kafka 可追加 RAG_KAFKA_SECURITY_PROTOCOL / RAG_KAFKA_SASL_* / RAG_KAFKA_TLS_*
# 上传默认异步受理：由独立的 rag-document-worker 消费 Kafka 文档通知
# （Kafka Connect 的 praxis-document-cdc 投递）；未配置 Kafka 时任务保持 queued。
# RAG_DOCUMENT_WORKER_CONCURRENCY=1
RAG_CACHE_TOPIC=praxis_state_cache_refresh
RAG_CACHE_GROUP=praxis_state_cache_refresh_workers
RAG_DOCUMENT_TOPIC=praxis_document_ingest
RAG_DOCUMENT_GROUP=praxis_document_ingest_workers
RAG_DOCUMENT_DLQ_TOPIC=praxis_document_ingest_dead_letters
RAG_CACHE_DLQ_TOPIC=praxis_state_cache_refresh_dead_letters
RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters
RAG_CACHE_DLQ_GROUP=praxis_state_cache_refresh_dead_letters
# 搜索为同步执行：RAG_SEARCH_TIMEOUT_SECONDS=120（默认）为检索总预算；
# 原 RAG_SEARCH_TOPIC/GROUP/DLQ_*/WORKER_CONCURRENCY 已下线，保留为废弃键、启动时忽略。
# 两个 Debezium MySQL 连接器由分布式 Kafka Connect 托管；宿主进程按固定名称
# 读取 task 状态，并对 FAILED task 自动重启（连接器名是契约）。
RAG_CONNECT_URLS=http://127.0.0.1:8083,http://127.0.0.1:8084
RAG_CONNECT_TIMEOUT_SECONDS=3
RAG_CONNECT_DOCUMENT_CONNECTOR=praxis-document-cdc
RAG_CONNECT_CACHE_CONNECTOR=praxis-cache-cdc
RAG_CONNECT_SUPERVISOR_ENABLED=true
RAG_CONNECT_SUPERVISOR_INTERVAL_SECONDS=15
RAG_CONNECT_RESTART_MAX_ATTEMPTS=5
RAG_CONNECT_RESTART_BASE_BACKOFF_SECONDS=2
RAG_CONNECT_RESTART_MAX_BACKOFF_SECONDS=300
# 两个连接器各用一个 MySQL CDC 账号；升级已有数据卷时必须补齐缓存账号密码，
# 否则 Compose/注册会明确失败（start-stack 会用文档账号密码兜底）。
RAG_MYSQL_DEBEZIUM_USER=debezium
RAG_MYSQL_DEBEZIUM_PASSWORD=CHANGE_ME
RAG_MYSQL_DEBEZIUM_CACHE_USER=debezium_cache
RAG_MYSQL_DEBEZIUM_CACHE_PASSWORD=CHANGE_ME_TOO
RAG_SERVICE_TOKEN=CHANGE_ME
RAG_MCP_ALLOWED_HOSTS=rag.example.com
RAG_MCP_ALLOWED_ORIGINS=https://app.example.com
# PDF 解析引擎：vision 需要独立运行的 pdf-parser（见 §3.5），go 为内置纯文本引擎。
RAG_PDF_ENGINE=vision
RAG_PDF_SERVICE_URL=http://127.0.0.1:8091
RAG_PDF_SERVICE_TIMEOUT=900
RAG_PDF_SERVICE_PARSE_TIMEOUT=120
# RAG_VLM_BASE_URL=https://llm.internal/v1
# RAG_VLM_MODEL=qwen2.5-vl-7b
# RAG_VLM_TIMEOUT=120
# RAG_VLM_CONCURRENCY=4
# RAG_WORD_CONVERTER_PATH=/usr/bin/soffice
```

worker 共用同一 env 文件；如果某些 worker 需要独立的最大尝试次数，可再叠加 `Environment=` 覆盖。文档 worker 只有 Kafka 消费模式，必须与 `rag-service` 使用同一组 Kafka bootstrap 与 topic/group 配置。

### 4.2 rag-service

```ini
# /etc/systemd/system/praxis-rag.service
[Unit]
Description=Praxis-RAG Go service
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
User=praxis
Group=praxis
WorkingDirectory=/opt/praxis-rag
EnvironmentFile=/etc/praxis-rag/env
ExecStart=/opt/praxis-rag/bin/rag-service
Restart=on-failure
RestartSec=5
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

服务收到 SIGTERM 后停止后台发布与维护循环、关闭 HTTP 与状态库；`Restart=on-failure` 覆盖意外崩溃。服务配置错误也以 `1` 退出，因此若配置写错会反复重启——上线前先用前台方式验证一次 `ExecStart`。

### 4.3 文档 worker

```ini
# /etc/systemd/system/praxis-rag-document-worker.service
[Unit]
Description=Praxis-RAG document worker
After=network-online.target praxis-rag.service
Wants=network-online.target

[Service]
Type=simple
User=praxis
Group=praxis
WorkingDirectory=/opt/praxis-rag
EnvironmentFile=/etc/praxis-rag/env
ExecStart=/opt/praxis-rag/bin/rag-document-worker
Restart=on-failure
RestartPreventExitStatus=2
RestartSec=5
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

默认异步上传必须运行本单元：它消费 Kafka Connect 的 `praxis-document-cdc` 投递的 Kafka 文档通知（`scripts/start-stack.ps1` 在开发环境会自动启动它），未运行时上传仍会受理但任务保持 `queued`。本单元同时要求 `rag-service` 配置 `RAG_KAFKA_BOOTSTRAP_SERVERS`，且文档连接器已运行。

### 4.4 同步搜索（无独立 worker）

搜索在 `rag-service` 进程内同步执行，**不需要** search worker、搜索 topic/消费组或搜索 DLQ。原 `praxis-rag-search-worker.service` 与 `bin/rag-search-worker` 已随 2026-11-13 搜索同步化改造删除；升级时请停用并删除该单元（见 §12）。同步检索总预算由 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）控制。

### 4.5 缓存 worker

```ini
# /etc/systemd/system/praxis-rag-cache-worker.service
[Unit]
Description=Praxis-RAG cache worker
After=network-online.target praxis-rag.service
Wants=network-online.target

[Service]
Type=simple
User=praxis
Group=praxis
WorkingDirectory=/opt/praxis-rag
EnvironmentFile=/etc/praxis-rag/env
ExecStart=/opt/praxis-rag/bin/rag-cache-worker
Restart=on-failure
RestartPreventExitStatus=2
RestartSec=5
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

缓存 worker 消费 Kafka Connect 的 `praxis-cache-cdc` 投递的 `RAG_CACHE_TOPIC`：事务内递增 `cache_revisions.revision`、重建脱敏视图并在提交后写 Redis；Redis 写失败只记录 `attempts/last_error`，不 nack、不进 DLQ，由周期 `RefreshDirty`（`RAG_CACHE_WORKER_RECOVER_SECONDS`，默认 30 秒）收敛。它需要状态库与 Kafka；不配置 Redis 时仍维护 revision 水位。

### 4.6 死信 worker（两个实例）

模板单元：

```ini
# /etc/systemd/system/praxis-rag-dead-letter-worker@.service
[Unit]
Description=Praxis-RAG dead-letter worker (%i)
After=network-online.target praxis-rag.service
Wants=network-online.target

[Service]
Type=simple
User=praxis
Group=praxis
WorkingDirectory=/opt/praxis-rag
EnvironmentFile=/etc/praxis-rag/env
ExecStart=/opt/praxis-rag/bin/rag-dead-letter-worker --kind %i
Restart=on-failure
RestartPreventExitStatus=2
RestartSec=10
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

```bash
systemctl enable --now praxis-rag-dead-letter-worker@document.service
systemctl enable --now praxis-rag-dead-letter-worker@cache.service
```

`%i` 只接受 `document` 或 `cache`；两个实例必须使用相互独立、且不等于正常消费组的 `RAG_DOCUMENT_DLQ_GROUP` / `RAG_CACHE_DLQ_GROUP`（默认 `praxis_document_dead_letters`、`praxis_state_cache_refresh_dead_letters`）。缓存消费失败一般由周期 `RefreshDirty` 补偿，不需要经 DLQ；`cache` 实例只做持久终态确认与告警。原 `@search` 实例已随排队搜索删除。

### 4.7 启动顺序

1. MySQL/Redis/Kafka/Qdrant 就绪，`binlog_row_image=FULL` 且两个 CDC 账号（`debezium`、`debezium_cache`）都具备 `SELECT/RELOAD/SHOW DATABASES/REPLICATION SLAVE/REPLICATION CLIENT`。
2. `pdf-parser` 就绪（使用 vision 引擎时；compose 默认随基础设施启动，等待 `/healthz` 返回 200）。它不依赖状态库，可与基础设施并行启动。
3. `rag-state-migrate --apply`（见 §6；幂等追加 `schedule_version`/缓存水位列并创建 `cdc_cutover_state`）。
4. 分布式 Kafka Connect 至少两个 worker 就绪（同一 `GROUP_ID`、同一连接器版本），执行 `rag-connect register` 注册两个固定名称连接器，并等待 connector/task 都 `RUNNING`；一个 worker 宕机时 peer 必须能接管。
5. `praxis-rag.service`（API 受理、同步搜索、维护循环、FAILED task supervisor）。
6. `praxis-rag-document-worker.service`（文档解析必需；未启动时上传只受理、任务保持 `queued`）、`praxis-rag-cache-worker.service`。
7. `praxis-rag-dead-letter-worker@document.service` 与 `@cache.service`。
8. 存量库升级按 §6.2 执行 `rag-cdc-cutover` 切换与核对，再放量；最后 `rag-service-migrate --retire-outbox --yes` 下线旧表。从排队搜索版本升级时按 §12 执行搜索同步化切换。
9. 打开 `/health`，创建/确认 API Key，做一次上传与检索冒烟；vision 引擎还要确认 `http://127.0.0.1:8091/healthz`，并为测试知识库选择 VLM 配置。

停止顺序相反：先停 worker/死信（让在途任务完成或回到可重试状态），再停服务。

## 5. 重启策略

所有 worker 与 CLI 的退出码语义：

| 退出码 | 含义 | 建议 |
| --- | --- | --- |
| `0` | 收到 SIGINT/SIGTERM，正常停止 | 不重启（systemd 的 `Restart=on-failure` 不触发） |
| `1` | 运行时失败（依赖暂时不可用、DB/队列错误等） | 重启并告警；连续失败升级 |
| `2` | 配置/用法错误（缺 `RAG_KAFKA_BOOTSTRAP_SERVERS`、非法 `--kind`、错误 backend、非法环境变量） | **不要自动重启**；修复配置后人工启动 |

- systemd：worker 单元用 `Restart=on-failure` + `RestartPreventExitStatus=2`，服务单元用 `Restart=on-failure`。
- Docker：`restart: unless-stopped`（服务）或 `on-failure:5`（worker）。Docker 无法按退出码跳过 `2`，需日志/监控报警；这也是生产优先 systemd 或同时配置健康监控的原因。
- Kafka 消费语义：**只有 MySQL 已记录领取/安全跳过/终态后才提交 offset**；nack → 同一进程内指数退避重试同一条记录（100ms 起，上限 5s），达到 `MaxAttempts` 后把原始消息写入对应 DLQ topic、执行终态回调、再提交 offset；`MaxAttempts=0` 会一直重试到进程/context 取消；整体为至少一次投递。
- 队列型 worker 可以多实例扩容：Kafka consumer group 在实例间分摊分区；同一任务有 `schedule_version` 与 SQL 租约，重复通知/旧版本通知会被安全跳过（至少一次语义）。
- 缓存 worker 的 Redis 写失败不是 nack 理由：失败记录在 `cache_revisions.last_error/attempts`，由周期 `RefreshDirty` 重试；Redis 最终一致，MySQL 事实不回滚。

## 6. 数据库迁移与上线

```bash
# 只报告（默认）
/opt/praxis-rag/bin/rag-state-migrate
# 幂等建表/增量迁移（追加 schedule_version、缓存水位列，创建 cdc_cutover_state）
/opt/praxis-rag/bin/rag-state-migrate --apply
# 旧 Mini-Agent 知识源预览/导入（不重建向量）
/opt/praxis-rag/bin/rag-service-migrate --legacy-root /home/<user>/.mini_agent
/opt/praxis-rag/bin/rag-service-migrate --apply --legacy-root /home/<user>/.mini_agent
# 切换完成、确认无旧进程后：删除 4 张已下线的 outbox 表（必须显式 --yes）
/opt/praxis-rag/bin/rag-service-migrate --retire-outbox --yes
```

注意：

- 表结构与 `enc:v1:` 凭据格式与旧 Python 版本兼容；从旧部署升级时应先停旧入口并备份数据库（Python 后端代码已从仓库删除）。
- `rag-service-migrate --apply` 只写状态（blobs + documents），**不生成 Qdrant 向量**；迁移完成后需通过 HTTP/MCP 重新上传文档或等待后续 reindex，才能被检索到。
- API 与 worker 一起发布：文档任务通知为 schema 4（搜索通知 schema 3 已随排队搜索下线，旧消息不得再消费），旧 schema 消息不兼容，禁止混跑；旧 outbox 链路与当前双连接器架构不能同时消费同一批 topic。
- 升级注意（内嵌文档 worker 已删除）：`rag-service` 不再解析文档，`RAG_DOCUMENT_WORKER_INPROCESS` / `RAG_DOCUMENT_WORKER_POLL_SECONDS` / `RAG_DOCUMENT_WORKER_LOCAL` 不再读取（残留设置不生效，也不会报错）；必须配置 Kafka 并运行 `rag-document-worker`，否则文档任务保持 `queued`。REST/Console 上传仍默认异步受理，MCP `add_document` 始终异步；`update_document`/`delete_document` 仍同步。
- 生产 `RAG_STATE_AUTO_SCHEMA` 可关闭，由迁移步骤预先建表；否则服务启动也会尝试建缺失表。

### 6.2 CDC 存量切换与回滚（升级必读）

前提：应用写入口已升级到“无 outbox 写入 + `schedule_version`”版本，旧 worker 已停止。完整契约见 [CDC 迁移契约 §8](cdc-migration.md#8-存量切换方案)。

1. **备份**：切换前备份 MySQL（含 4 张旧 outbox 表，回滚需要）；保留旧服务/worker 二进制、旧 Debezium Server 配置与 `debezium_data` / `debezium_cache_data` 卷。**旧 Debezium Server 的文件位点不能导入 Kafka Connect**，新连接器第一次启动会按 `snapshot.mode=initial` 做全量快照（`op=r`）。
2. **记录起点**：停写窗口内执行 `SHOW MASTER STATUS` 取得 `File`/`Position`，然后：
   `rag-cdc-cutover --record-position --file=mysql-bin.000123 --pos=4567890`
3. **盘点**：`rag-cdc-cutover --inventory` 输出未完成文档任务、缓存 key 数、脏 key 与最老任务年龄，并核对没有遗漏的旧进程（搜索任务不再盘点；历史 `search_tasks` 行原样保留）。
4. **启动 Connect 集群并注册**：至少两个 worker、同一连接器版本，`rag-connect register` 注册两个固定名称连接器并等待 RUNNING。初始快照把业务表现存行投递一次，缓存消费者按 revision 水位重建读模型。
5. **任务连接器补投**：初始快照为现存任务投递 `op=r`；随后 `rag-cdc-cutover --backfill --baseline=N` 循环递增未完成任务 `schedule_version`，确保每条任务都有可消费通知（幂等，重复执行返回 0）。
6. **缓存基线**：`rag-cdc-cutover --mark-cache-baseline` 把全部读模型 key 标脏。
7. **核对**：`rag-cdc-cutover --verify --baseline=N` 断言活动任务 `schedule_version >= baseline+1`；同时观察监控页两个连接器/task 状态、所在 worker、两组 Kafka lag 与缓存 dirty 归零。
8. **放量**：恢复写入口，继续观察；确认稳定后执行 `rag-service-migrate --retire-outbox --yes` 删除 4 张旧表。

**回滚**：切换窗口内回滚需要同时回退服务/worker 二进制、恢复旧 Debezium Server 服务与两个旧卷（旧实例从文件位点继续），并停掉 `connect-1`/`connect-2`/`connect-init`。Kafka Connect 的 `praxis_connect_*` 内部 topic 与 `praxis_cdc_*_schema_history` 不会被旧部署使用，确认无其他连接器后再清理。消费者按 `schedule_version` 幂等吸收重复。旧 outbox 表在 `--retire-outbox` 之前仍可恢复读取；一旦删除，回滚只能依赖切换前备份。搜索同步化切换的回滚路径见 §12.4（与文档链路相互独立）。

## 7. DLQ 监控

- 死信 topic 是显式配置的 `RAG_DOCUMENT_DLQ_TOPIC`、`RAG_CACHE_DLQ_TOPIC`（默认 `praxis_document_ingest_dead_letters`、`praxis_state_cache_refresh_dead_letters`），不是 `%DLQ%<源消费组>`。消费重试在 worker 进程内完成，耗尽后由消费者发布原始消息到对应 DLQ topic 再提交 offset；请预建两个 DLQ topic 并授权独立 `RAG_*_DLQ_GROUP` 读取。原 `praxis_search_dead_letters` 已下线，可保留历史消息待过期。
- 死信 worker 只做**持久终态确认**：document 失败写 `document_jobs.status=failed`，cache 只记录告警（缓存失败由 `RefreshDirty` 收敛）；只有终态（或任务不存在）才提交 offset，活租约或未提交发布不会提交，Kafka 会重投（至少一次）。搜索为同步执行，不产生死信。
- 监控建议：
  - 两个显式 DLQ topic（document/cache）的堆积量与增长速率；持续增长说明消费重试耗尽。
  - 死信 worker 进程退出码/重启次数（`2` 表示配置错，`1` 表示依赖不可用）。
  - `/v1/uploads` 的失败操作、`document_jobs` 的 `failed` 计数。
  - Kafka consumer group lag：文档、缓存两个消费组分别观察；Console 监控页的 `pipeline.kafka.groups` 会给出两组已提交/末尾位点与滞后，读不到时显示「未知」。
  - 缓存 dirty/failures：`cache_revisions` 中 `refreshed_revision < revision` 或 `last_error` 非空的行；持续不清零说明缓存 worker 未运行或 Redis 不可用。
  - Console「系统监控」页（`#/monitor`）汇总上述队列与依赖状态，对应只读接口 `GET /v1/admin/monitor/overview`（本机直连或管理员会话），适合作值班快速定位入口。
- 告警处理顺序：先看死信 worker 是否在跑且配置正确，再查对应正常组是否有堆积，再按 `stage/error` 定位解析/模型/发布失败；不要直接清空 DLQ topic（会丢失持久终态）。

## 8. 分布式 Kafka Connect、两个 Debezium 连接器与缓存消费者

| 连接器 | 采集范围 | 投递 | 配置 |
| --- | --- | --- | --- |
| `praxis-document-cdc` | 只采集 `document_jobs`；`column.include.list` 只含定位列，`skip.messages.without.change=true`，`skipped.operations=d` | `RAG_DOCUMENT_TOPIC` | `deploy/kafka-connect/connectors/document-cdc.json` |
| `praxis-cache-cdc` | `knowledge_bases`、`documents`、`mcp_catalogs`、`api_keys`、`document_heads`、`document_leases`、`upload_operations`、`upload_steps`；所有 c/u/d | `RAG_CACHE_TOPIC` | `deploy/kafka-connect/connectors/cache-cdc.json` |

- 两个连接器运行在**同一个分布式 Connect 集群**的两个 worker 上，共享
  `praxis_connect_configs` / `praxis_connect_offsets` / `praxis_connect_status`
  内部 topic，但各自使用独立的 schema history topic
  （`praxis_cdc_document_schema_history` / `praxis_cdc_cache_schema_history`）、
  独立 `database.server.id` 与独立 MySQL 账号。Compose 的 REST 端口分别是
  `127.0.0.1:8083` 与 `127.0.0.1:8084`。
- 连接器由 `rag-connect register` 以固定名称幂等注册，并用 `rag-connect wait`
  等待 connector/task `RUNNING`；Compose 的 `connect-init` 在 `app` profile 下依次
  执行这两步。配置未变化时不会更新，避免重启健康 task。
- **worker 故障**：Connect group rebalance 把 task 交给存活 worker，task 从 Kafka
  已保存的 source offset 继续；不需要人工恢复位点。验收必须看到对端 worker 上的
  task `RUNNING` 且文档实际 `succeeded`，不能只看 connector 状态。
- **task FAILED（worker 在线）**：Kafka Connect 不会自动重启 FAILED task。
  `rag-service` 的 supervisor 周期性读取 `tasks[].state`，只重启 FAILED task，
  指数退避并在 `RAG_CONNECT_RESTART_MAX_ATTEMPTS` 次后停止；Console 保留失败原因
  与恢复结果，耗尽后需人工处理。
- 应用不再写事务 outbox，也不发布文档/搜索通知；Debezium 只读 binlog 投递，
  Kafka 只记录消费位点，文档任务结果由 `document_jobs` 记录，artifact 由
  `artifact_snapshots` 记录。缓存消费者只维护 `cache_revisions` revision 水位并
  重建 Redis。`search_tasks`/`search_result_snapshots` 为历史表、不再读写。
- 监控读取 `GET /connectors/{name}/status`，展示每个连接器的 connector/task 状态、
  所在 worker、失败原因、是否正在重启、重启次数与恢复结果；`capture_progress`
  始终为 `null`（binlog 位点在 Connect offset topic，应用读不到），Console 显示
  「未知」。缓存消费者 lag 属于 `pipeline.kafka.groups.cache`，与采集状态分开。
- 端到端脚本 `scripts/verify-debezium-e2e.py` 覆盖验收 2/3/4：暂停缓存连接器后
  文档仍完成、停止承载 document task 的 worker 后对端接管并完成文档、锁定缓存
  CDC 账号使 task FAILED 后由 supervisor 自动恢复，并用实际 Kafka 消息验证 Go
  解码器提取 `artifact_id`/`namespace`/`schedule_version` 与缓存删除的 `before`
  定位字段。演练步骤见 [Kafka Connect 操作手册](../deploy/kafka-connect/README.md)
  §6。

## 9. 安全与网络

- `rag-service` 默认 loopback；远程访问必须设置 `RAG_SERVICE_TOKEN`、`RAG_MCP_ALLOWED_HOSTS`、`RAG_MCP_ALLOWED_ORIGINS`，并在前面加 HTTPS 反向代理。
- Console 登录默认启用：首次访问注册的第一个账号成为管理员，之后可在"用户管理"页分配角色（管理员 `admin` / VIP `vip` / 普通用户 `user`）、启用/禁用、删除；最后一个启用管理员受保护。HTTPS 部署请设置 `RAG_AUTH_COOKIE_SECURE=true`；不需要自助注册时设 `RAG_AUTH_REGISTRATION=closed`，完全关闭登录用 `RAG_AUTH_ENABLED=false`。
- 可选的飞书文档导入由 `RAG_FEISHU_APP_ID`、`RAG_FEISHU_APP_SECRET`、`RAG_FEISHU_REDIRECT_URI` 启用，三项必须同时配置。飞书应用需申请 `drive:drive:readonly`、`docx:document:readonly`、`offline_access`，开启「安全设置 → 刷新 user_access_token」并发布应用，再把回调地址（如 `https://rag.example.com/v1/feishu/oauth/callback`）原样登记到飞书开放平台「安全设置 → 重定向 URL」；生产环境必须使用 HTTPS。App Secret 只通过环境变量/密钥注入，不写入 `config.toml`、MySQL 或日志；用户的 access/refresh token 以 `enc:v1:` 加密存于 `feishu_connections`，回调 state 只存摘要且一次性使用。导入后的文档副本按 Praxis-RAG 知识库权限可见，与飞书原文档分享范围无关。
- 知识库权限：每个知识库归创建者私有，VIP/管理员可通过邀请链接按「可读/可修改」和「单文件/多文件/全库」授权其他账号，被邀请人登录接受后生效；邀请有效期 `RAG_INVITATION_TTL_HOURS`（默认 168 小时）。VIP/普通用户自建知识库时模型连接克隆自 `RAG_DEFAULT_KB_TEMPLATE`（默认 `default`）指向的模板知识库，部署时应先由管理员创建该模板。详见 [权限管理](PERMISSIONS.md)。
- 账号密码以 PBKDF2-SHA256（每账号独立盐）存于状态库；会话只保存 SHA-256 摘要，Cookie 为 HttpOnly + SameSite=Lax，写操作要求 CSRF Token。
- `/v1/admin/*` 允许本机直连（loopback + 无代理头 + 同源 Origin）或管理员登录会话；`/v1/admin/api-keys` 是例外：任意已登录账号都可管理自己的 API Key（非管理员只能绑定本人创建的知识库），无会话时仍仅限本机直连。远程自动化仍建议使用 API Key/Service Token。
- MySQL/Redis/Kafka 只监听内网；Kafka 启用 SASL/TLS 时使用 `RAG_KAFKA_SASL_*` / `RAG_KAFKA_TLS_*`，凭据通过环境文件/密钥管理注入，禁止写入 `config.toml`、镜像或 Git。
- `pdf-parser` 是无认证的内部服务：compose 只把 8091 发布到 `127.0.0.1`，不要改成 `0.0.0.0` 或挂到公网反向代理；只允许 Go 服务所在的容器/主机访问。VLM 端点凭据同样只通过环境变量/密钥注入，且要确认 PDF 图片/公式会被发送到该外部端点。
- 运行用户非 root，数据目录 0700；env 文件 0600；日志不要输出查询/文件内容（Go 实现只记录任务 ID 与错误类别）。
- 定期备份 MySQL；上传恢复工作区同样需要备份。Redis 只保存可丢失的缓存/短期快照，不作为真相。

## 10. 验证与已知未验证项

已在本仓库验证（详见 [USAGE](USAGE.md) 的验证范围与 [VALIDATION](../VALIDATION.md)）：

- Go 全量 `go build ./...` 与 `go test -count=1 ./...`（历史基线，包数/用例数随代码演进）。
- `scripts/build-go.ps1` 本机构建 + 冒烟（健康检查、Console、`/`）。
- worker 的启动/退出码行为（缺少 Kafka → `2`、错误 backend、非法 `--kind`、`--help`）。

本环境**未完整验证**：Docker 镜像构建、`app`/worker Compose profile、`pdf-parser` 镜像与真实视觉端点推理、systemd 单元、真实 MySQL 多实例并发与故障切换、真实 Kafka+SASL/TLS 集群、LibreOffice DOC 转换与真实 VLM 端点实链路。**2026-11-15 Kafka Connect 改造（双 worker、Kafka 内部 topic 保存位点、固定名称注册、FAILED task 自动重启、worker/task 故障演练）与 2026-11-13 CDC/搜索改造尚未实机验证**；历史 Kafka 3.9.0 + Debezium 3.7.1 实机记录对应已下线的 Debezium Server/outbox 链路，不能作为当前架构验收依据。上线前请在预发环境按本文 §6.2、§12、[Kafka Connect 操作手册](../deploy/kafka-connect/README.md) §6 与 [CDC 迁移契约](cdc-migration.md) §8、§12 逐项演练，并参考 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md) 的剩余 P1/P2 与 [GO_MIGRATION](GO_MIGRATION.md) 的进行中项。

## 11. 搜索同步化变更速查（2026-11-13）

| 面 | 变更 |
| --- | --- |
| MCP 工具 | 5 个：`search`、`add_document`、`update_document`、`delete_document`、`get_document_status`；`get_search_result` 删除，`search` 无 `wait_seconds` 参数 |
| REST | `POST /v1/search` 同步返回；`GET /v1/search-results/{result_id}` 删除；保留 `/v1/artifacts/{artifact_id}` 与 `/content`（过期/不可用仍映射 410） |
| 搜索响应 | 去掉 `result_id`、`created_at`、`expires_at`、`snapshot_available`、`snapshot_error`；保留 `results`/`artifacts`/`artifact_limits`/`artifact_warning`/`evidence_status`/`answer_guidance`/`debug`/`snapshot_context` 等既有字段 |
| 执行方式 | 搜索在 `rag-service` 内同步执行，一次调用检索 + 重排；总预算 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒），取消向下传递 |
| 进程/消息 | `rag-search-worker`、`cmd/rag-search-worker`、死信 `--kind search`、搜索 topic/消费组/DLQ、搜索 schema-3 通知全部删除；Kafka 只剩文档/缓存两类通知 |
| 环境变量 | `RAG_SEARCH_TOPIC`/`RAG_SEARCH_GROUP`/`RAG_SEARCH_DLQ_TOPIC`/`RAG_SEARCH_DLQ_GROUP`/`RAG_SEARCH_MAX_ATTEMPTS`/`RAG_SEARCH_LEASE_SECONDS`/`RAG_SEARCH_RETRY_SECONDS`/`RAG_SEARCH_WORKER_CONCURRENCY` 下线（保留为废弃键、启动忽略）；`RAG_SEARCH_TIMEOUT_SECONDS` 保留 |
| 存储 | `search_tasks`、`search_result_snapshots` 表保留历史数据、不再读写（DDL 注释 `retired; no longer read or written`），不 DROP、不清数据；`artifact_snapshots` 与 artifact TTL（`RAG_RESULT_TTL_SECONDS`）不变 |
| 监控 | `pipeline.kafka.groups` 两组（document/cache）；移除 `search_tasks` 队列指标、结果快照计数与搜索 DLQ 指标；Console 监控页同步删除对应卡片（前端由前端负责人处理） |
| 权限 | 受限 scope 不再被拒绝，改为同步检索并按 `DocumentIDs` 过滤 |

## 12. 搜索同步化迁移与切换（升级必读）

本节只覆盖“排队搜索 → 同步搜索”。文档入库与缓存链路不变（`document_jobs` + schema 4、两个 Debezium 连接器、`rag-cache-worker`），其切换见 §6.2。契约入口见 [CDC 迁移契约 §12](cdc-migration.md#12-搜索同步化迁移入口)。

### 12.1 升级前准备

1. 确认所有客户端版本：MCP 客户端不再发送 `wait_seconds`、不再调用 `get_search_result`、不再轮询 `result_id`；REST 客户端不再请求 `/v1/search-results/{id}`。
2. 记录当前在途搜索：`SELECT status, COUNT(*) FROM search_tasks WHERE status IN ('queued','retrying','processing') GROUP BY status;`（仅作观察，升级后这些行不再被读写，也不会自动完成）。
3. 备份 MySQL（含 `search_tasks`、`search_result_snapshots`；保留历史数据与回滚可能）。
4. 保留旧版 `rag-service`/`rag-search-worker` 二进制与旧采集连接器配置，用于回滚。

### 12.2 切换步骤（建议停机窗口）

1. **停搜索入口**：停止旧 `rag-service` 的对外流量（或维护页），避免新旧版本同时受理搜索。
2. **停搜索 worker 与死信 worker**：`systemctl stop praxis-rag-search-worker.service`、`systemctl stop praxis-rag-dead-letter-worker@search.service`（Compose 部署移除 `search-worker`/`dead-letter-search` 服务）。
3. **排空搜索消息**：等待 `praxis_search` 消费组 lag 归零；确认无新搜索任务写入后，可停止并删除搜索 topic `praxis_search` 与 `praxis_search_dead_letters`（先记录存量消息数，必要时备份）。**不要**删除 `search_tasks`/`search_result_snapshots` 表。
4. **处理在途任务**：`search_tasks` 中未完成行不会再有消费者。按业务决定：直接保留为历史（推荐）或在维护窗口内 SQL 收敛（例如 `UPDATE search_tasks SET status='timed_out', updated_at=NOW() WHERE status IN (...)`）；不要 DROP 表、不要清空数据。
5. **升级数据库 schema**：`rag-state-migrate --apply`（幂等；不再给 `search_tasks` 打 `schedule_version` 补丁，表只保留原结构）。
6. **部署新版本**：替换 `rag-service`（新版本不再链接搜索 worker）；从 systemd/Compose/进程管理器中删除 search worker 与 `--kind search` 死信实例；确认启动日志无搜索 topic/group 配置错误（旧配置键被忽略而不是报错）。
7. **更新采集连接器**：`column.include.list` 只保留 `document_jobs`（由 W2/deploy 负责）；更新 `praxis-document-cdc` 并确认其 source offset 未受影响。
8. **验证**：`curl.exe -X POST /v1/search` 一次返回 `results`/`artifacts`；`tools/list` 只有 5 个工具；`GET /v1/search-results/<任意 uuid>` 返回 404；受限账号检索按 scope 过滤；监控页无搜索队列/快照卡片且不报错。
9. **放量并观察**：搜索错误率、P95 延迟（受 `RAG_SEARCH_TIMEOUT_SECONDS` 约束）、artifact 410 比例、Qdrant/模型超时与取消日志。

### 12.3 客户端调整

- MCP：删除 `wait_seconds` 与轮询逻辑；把 `get_search_result` 调用改为直接使用本次 `search` 响应；超时/`isError` 后重试即重新检索（会产生新调用，不再按 `result_id` 复用）。
- REST：把 `GET /v1/search-results/{result_id}?page=` 的读取改为直接消费 `POST /v1/search` 响应；分页在响应内完成，不存在“按 ID 再读”。
- 历史 `result_id` 一律视为不存在（410/404 按实现映射），不要持久化；需要证据请重新检索。
- 若客户端依赖“提交即返回、后台算”的语义，改为提高超时/并发或接受同步阻塞；服务端不提供排队模式。

### 12.4 回滚

- 回滚 = 回退 `rag-service`/`rag-search-worker` 二进制与采集连接器配置（恢复 `search_tasks` 采集与 `praxis_search` topic），并重启 search worker 与 `--kind search` 死信实例。
- 因为 `search_tasks`/`search_result_snapshots` 未删除、未清空，回滚后旧任务行仍可被旧 worker 继续消费（旧 worker 按 `schedule_version`/状态收敛）；回滚前请确认切换窗口内没有新格式数据写入这两张表（新版本完全不写）。
- 若已删除搜索 topic，回滚需重建 topic 并依赖任务连接器初始快照（`op=r`）补投未完成任务；否则需手工重排 `schedule_version`。
- 回滚不涉及 artifact：`artifact_snapshots` 与 TTL 语义前后一致，已签发 artifact 继续可读。
- 回滚后务必再次核对监控页 `pipeline.kafka.groups` 是否恢复三组（旧版本）或保持两组（新版本），避免混跑。
