# Praxis-RAG 部署指南（Go 后端）

本文给出 `rag-service` 与三个 worker 的生产部署模板：Docker/Compose、systemd、重启策略、DLQ 监控和上线顺序。安装、环境变量与 API 用法见 [使用与部署](USAGE.md)；迁移背景与当前缺口见 [GO_MIGRATION](GO_MIGRATION.md)；契约差异见 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md)。

> 本文的镜像构建、`app`/worker Compose profile 与 systemd 示例未在本仓库完整实机运行；但 Kafka 3.9.0 + Debezium 3.7.1 + MySQL 8.0 的 compose 基础设施已实机验证（Kafka healthy、Debezium streaming、记录进入 topic 且 outbox 保持 PENDING）。生产上线前仍请在目标主机逐项验证。

## 1. 进程拓扑

```text
                  ┌────────────────────────────┐
浏览器/MCP/REST ──>│ rag-service :8011          │──> Qdrant（HTTP :6333）
                  │ REST /v1, MCP /mcp, /console│──> 共享状态库 MySQL/SQLite
                  └────────────┬───────────────┘──> Redis（可选：缓存/快照）
                               │ Kafka（可选：outbox relay + 排队搜索）
             ┌─────────────────┼──────────────────────┐
             ▼                 ▼                      ▼
  rag-document-worker   rag-search-worker   rag-dead-letter-worker --kind {document,search}
  （异步文档队列）        （排队搜索）          （显式 DLQ topic: RAG_*_DLQ_TOPIC）
```

- **每个进程独立部署**：`rag-service` 不内嵌消费者；worker 不监听端口。
- 所有进程必须共享同一份基础设施配置：`RAG_CONFIG_FILE`、`RAG_SERVICE_DATA_ROOT`、`RAG_STATE_*`、`RAG_BACKEND=qdrant`/`QDRANT_URL`、`RAG_STATE_ENCRYPTION_KEY`、Redis 与 Kafka（bootstrap、topic/group、DLQ）设置。配置不一致会造成 401/找不到任务/重复发布等难以排查的问题。
- 仅同步 REST/Console 的最小部署是 `rag-service + Qdrant + 状态库`；MCP 异步新增需要文档 worker；服务端 MCP 排队搜索需要 search worker + Redis + Kafka。

## 2. 二进制与目录布局

```text
/opt/praxis-rag/
  bin/rag-service
  bin/rag-document-worker
  bin/rag-search-worker
  bin/rag-dead-letter-worker
  bin/rag-service-migrate
  bin/rag-state-migrate
  bin/rag-mcp-debug
  web/                     # Console 构建产物（dist/web）
/etc/praxis-rag/
  env                      # 仅 root 可读的密钥/环境文件
  config.toml              # 非敏感默认配置（[environment]）
/var/lib/praxis-rag/       # RAG_SERVICE_DATA_ROOT（SQLite、上传恢复工作区）
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
```

镜像内容：`/app/bin/`（全部命令，入口默认 `/app/bin/rag-service`）、`/app/web`（Console）、`/app/config.toml`（`0.0.0.0:8011`、`/app/data`、Qdrant 服务名、loopback MCP 白名单）；非 root 用户 `rag`，健康检查 `/health`，数据卷 `/app/data`。

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
- 镜像不含 Docling/LibreOffice/Python 视觉环境：需要 PDF/DOC/Word 视觉解析时，把外部环境 bind mount 进容器，并设置 `RAG_PDF_PARSER_PYTHON`、`RAG_PDF_MODELS_PATH`、`RAG_WORD_CONVERTER_PATH`；解析进程的路径必须在容器内可见。
- 数据目录卷保存 SQLite（若不用 MySQL）、上传恢复工作区与 artifact 工作区；不要用匿名卷。

### 3.3 Compose：app profile

仓库 `docker-compose.yml` 默认只起基础设施；`app` profile 构建并启动 Go 服务：

```bash
docker compose --profile app up -d --build
```

`app` 服务从 `.env` 读取 `RAG_SERVICE_TOKEN`（容器 0.0.0.0 绑定，必填）、`RAG_MYSQL_*`、`RAG_STATE_ENCRYPTION_KEY`、`RAG_APP_QDRANT_URL`、`RAG_APP_PORT`、`RAG_APP_KAFKA_BOOTSTRAP_SERVERS`（默认 `kafka:9092`）等；默认依赖 MySQL healthy + Qdrant started + Kafka healthy。默认 profile 的 `docker compose up` 行为不变。

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

  search-worker:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-search-worker"]
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

  dead-letter-document:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-dead-letter-worker", "--kind", "document"]
    restart: on-failure:5
    env_file: [.env]
    depends_on:
      mysql: { condition: service_healthy }
      kafka: { condition: service_healthy }

  dead-letter-search:
    image: ${RAG_APP_IMAGE:-praxis-rag:local}
    profiles: ["workers"]
    entrypoint: ["/app/bin/rag-dead-letter-worker", "--kind", "search"]
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
RAG_CACHE_TOPIC=praxis_state_cache_refresh
RAG_CACHE_GROUP=praxis_state_cache_refresh_workers
RAG_DOCUMENT_TOPIC=praxis_document_ingest
RAG_DOCUMENT_GROUP=praxis_document_ingest_workers
RAG_SEARCH_TOPIC=praxis_search
RAG_SEARCH_GROUP=praxis_search_workers
RAG_DOCUMENT_DLQ_TOPIC=praxis_document_ingest_dead_letters
RAG_SEARCH_DLQ_TOPIC=praxis_search_dead_letters
RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters
RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters
RAG_SERVICE_TOKEN=CHANGE_ME
RAG_MCP_ALLOWED_HOSTS=rag.example.com
RAG_MCP_ALLOWED_ORIGINS=https://app.example.com
# RAG_PDF_PARSER_PYTHON=/opt/praxis-rag/pdf-parser/bin/python
# RAG_PDF_MODELS_PATH=/opt/praxis-rag/models
# RAG_WORD_CONVERTER_PATH=/usr/bin/soffice
```

worker 共用同一 env 文件；如果某些 worker 需要独立的最大尝试次数或本地模式，可再叠加 `Environment=` 覆盖。`RAG_DOCUMENT_WORKER_LOCAL=1` 只用于无 Kafka 的单进程部署，生产 Kafka 模式保留默认。

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

服务收到 SIGTERM 后停止后台 relay/维护循环、关闭 HTTP 与状态库；`Restart=on-failure` 覆盖意外崩溃。服务配置错误也以 `1` 退出，因此若配置写错会反复重启——上线前先用前台方式验证一次 `ExecStart`。

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

无 Kafka 的单进程环境：在 env 中加 `RAG_DOCUMENT_WORKER_LOCAL=1`，并确保 `rag-service` 不配置 `RAG_KAFKA_BOOTSTRAP_SERVERS`（否则 worker 会切换 Kafka 消费模式并报配置错误）。

### 4.4 搜索 worker

```ini
# /etc/systemd/system/praxis-rag-search-worker.service
[Unit]
Description=Praxis-RAG search worker
After=network-online.target praxis-rag.service
Wants=network-online.target

[Service]
Type=simple
User=praxis
Group=praxis
WorkingDirectory=/opt/praxis-rag
EnvironmentFile=/etc/praxis-rag/env
ExecStart=/opt/praxis-rag/bin/rag-search-worker
Restart=on-failure
RestartPreventExitStatus=2
RestartSec=5
TimeoutStopSec=20

[Install]
WantedBy=multi-user.target
```

搜索 worker 必须有 `RAG_KAFKA_BOOTSTRAP_SERVERS` 与可用 Redis（结果快照）；若只部署同步服务，不要启用排队搜索（不设置 Kafka，或接受 MCP 搜索退化为同步）。

### 4.5 死信 worker（两个实例）

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
systemctl enable --now praxis-rag-dead-letter-worker@search.service
```

`%i` 只接受 `document` 或 `search`；两个实例必须使用相互独立、且不等于正常消费组的 `RAG_DOCUMENT_DLQ_GROUP` / `RAG_SEARCH_DLQ_GROUP`（默认 `praxis_document_dead_letters`、`praxis_search_dead_letters`）。

### 4.6 启动顺序

1. MySQL/Redis/Kafka/Qdrant 就绪。
2. `rag-state-migrate --apply`（见 §6）。
3. `praxis-rag.service`（API 受理、relay、维护循环）。
4. `praxis-rag-document-worker.service`、`praxis-rag-search-worker.service`。
5. `praxis-rag-dead-letter-worker@document/search`。
6. 打开 `/health`，创建/确认 API Key，做一次上传与检索冒烟。

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
- Kafka 消费语义：handler 成功 → 提交 offset；nack → 同一进程内指数退避重试同一条记录（100ms 起，上限 5s），达到 `MaxAttempts` 后把原始消息写入对应 DLQ topic、执行终态回调、再提交 offset；`MaxAttempts=0` 会一直重试到进程/context 取消；整体为至少一次投递。
- 队列型 worker 可以多实例扩容：Kafka consumer group 在实例间分摊分区；同一文档任务有 SQL 租约，重复通知不会重复发布（至少一次语义）。

## 6. 数据库迁移与上线

```bash
# 只报告（默认）
/opt/praxis-rag/bin/rag-state-migrate
# 幂等建表/增量迁移
/opt/praxis-rag/bin/rag-state-migrate --apply
# 旧 Mini-Agent 知识源预览/导入（不重建向量）
/opt/praxis-rag/bin/rag-service-migrate --legacy-root /home/<user>/.mini_agent
/opt/praxis-rag/bin/rag-service-migrate --apply --legacy-root /home/<user>/.mini_agent
```

注意：

- 表结构与 `enc:v1:` 凭据格式与旧 Python 版本兼容；从旧部署升级时应先停旧入口并备份数据库（Python 后端代码已从仓库删除）。
- `rag-service-migrate --apply` 只写状态（blobs + documents），**不生成 Qdrant 向量**；迁移完成后需通过 HTTP/MCP 重新上传文档或等待后续 reindex，才能被检索到。
- API 与 worker 一起发布：Go 文档通知为 schema 3，旧 Python worker 的 schema 2 不兼容，禁止混跑。
- 生产 `RAG_STATE_AUTO_SCHEMA` 可关闭，由迁移步骤预先建表；否则服务启动也会尝试建缺失表。

## 7. DLQ 监控

- 死信 topic 是显式配置的 `RAG_DOCUMENT_DLQ_TOPIC`（默认 `praxis_document_ingest_dead_letters`）与 `RAG_SEARCH_DLQ_TOPIC`（默认 `praxis_search_dead_letters`），不是 `%DLQ%<源消费组>`。消费重试在 worker 进程内完成，耗尽后由消费者发布原始消息到对应 DLQ topic 再提交 offset；请预建两个 DLQ topic 并授权独立 `RAG_DOCUMENT_DLQ_GROUP` / `RAG_SEARCH_DLQ_GROUP` 读取。
- 死信 worker 只做**持久终态确认**：document 失败写 `document_jobs.status=failed`，search 失败写 `search_tasks.status=failed/timed_out`；只有终态（或任务不存在）才提交 offset，活租约或未提交发布不会提交，Kafka 会重投（至少一次）。
- 监控建议：
  - 两个显式 DLQ topic 的堆积量与增长速率；持续增长说明消费重试耗尽。
  - 死信 worker 进程退出码/重启次数（`2` 表示配置错，`1` 表示依赖不可用）。
  - `/v1/uploads` 的失败操作、`document_jobs`/`search_tasks` 的 `failed` 计数。
  - Kafka consumer group lag：文档 worker、搜索 worker 分别观察各自组。
- 告警处理顺序：先看死信 worker 是否在跑且配置正确，再查对应正常组是否有堆积，再按 `stage/error` 定位解析/模型/发布失败；不要直接清空 DLQ topic（会丢失持久终态）。

## 8. outbox 与投递模式

| 模式 | 配置 | 行为 |
| --- | --- | --- |
| relay（Go 默认且唯一内建） | `RAG_KAFKA_BOOTSTRAP_SERVERS` 设置 | `rag-service` 进程内轮询 `document_job_outbox` 投递纯通知；搜索请求也由进程内 relay 投递 |
| 本地单进程 | 不设置 Kafka + `RAG_DOCUMENT_WORKER_LOCAL=1` | 文档 worker 直接轮询 outbox；搜索只能是同步（MCP `search` 无 `wait_seconds`） |
| Debezium | `RAG_OUTBOX_BRIDGE=debezium` + 外部 Debezium | 已实现并验证；应用不发布，由 CDC 投递。不要与 relay 同时启用 |

Debezium 模式（`RAG_OUTBOX_BRIDGE=debezium`）要求状态库为 MySQL：启动 compose `debezium` profile（或外部 Debezium Server），应用侧保持该环境变量；应用不会启动进程内 relay，避免双投递。已验证 CDC 部署链路：MySQL 8.0 + Debezium Server 3.7.1.Final + 原生 Kafka sink + Kafka 3.9.0（topic 记录与 outbox PENDING 一致）；Go 服务/worker 消费真实 Kafka 的完整链路也已实机验证（debezium 与 relay 两种模式，脚本 `scripts/verify-debezium-e2e.py`；DLQ 与排队搜索见 `scripts/verify-kafka-dlq.py` / `scripts/verify-kafka-search.py`）。

## 9. 安全与网络

- `rag-service` 默认 loopback；远程访问必须设置 `RAG_SERVICE_TOKEN`、`RAG_MCP_ALLOWED_HOSTS`、`RAG_MCP_ALLOWED_ORIGINS`，并在前面加 HTTPS 反向代理。
- `/v1/admin/*` 只允许本机直连（loopback + 无代理头 + 同源 Origin）；远程管理请通过 SSH 隧道或受控跳板。
- MySQL/Redis/Kafka 只监听内网；Kafka 启用 SASL/TLS 时使用 `RAG_KAFKA_SASL_*` / `RAG_KAFKA_TLS_*`，凭据通过环境文件/密钥管理注入，禁止写入 `config.toml`、镜像或 Git。
- 运行用户非 root，数据目录 0700；env 文件 0600；日志不要输出查询/文件内容（Go 实现只记录任务 ID 与错误类别）。
- 定期备份 MySQL；上传恢复工作区与 SQLite（若使用）同样需要备份。Redis 只保存可丢失的缓存/短期快照，不作为真相。

## 10. 验证与已知未验证项

已在本仓库验证（详见 [USAGE](USAGE.md) 的验证范围）：

- Go 全量 `go build ./...` 与 `go test -count=1 ./...` 通过（包数/用例数随代码演进）。
- `scripts/build-go.ps1` 本机构建 + 冒烟（健康检查、Console、`/`）。
- 三个 worker 的启动/退出码行为（无 Kafka、错误 backend、非法 `--kind`、`--help`）。

本环境**未完整验证**：Docker 镜像构建、`app`/worker Compose profile、systemd 单元、真实 MySQL 多实例并发与故障切换、真实 Kafka+SASL/TLS 集群、Docling/LibreOffice/视觉模型实链路；已实机验证：Kafka 3.9.0 + Debezium 3.7.1 + MySQL 8.0 + Redis + Qdrant 基础设施、CDC 投递、Go 服务/worker 的 debezium/relay 异步链路、排队搜索与显式 DLQ（脚本 `scripts/verify-*.py`，见 [USAGE](USAGE.md)）。上线前请在预发环境按本指南逐项演练，并参考 [GO_PARITY_AUDIT](GO_PARITY_AUDIT.md) 的剩余 P1/P2 与 [GO_MIGRATION](GO_MIGRATION.md) 的进行中项。




