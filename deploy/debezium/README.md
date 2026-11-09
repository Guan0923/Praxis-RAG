# Debezium 桥接：MySQL binlog → Kafka

这个目录让 MySQL 里已经提交的事务**必然**产生一条 Kafka 通知，中间不依赖应用进程是否存活。

> **实测记录（2026-11-09，Windows + Docker Desktop）**：仓库的 `docker-compose.yml`
> 使用单节点 KRaft Kafka（`apache/kafka:3.9.0`）与 Debezium Server 3.7.1.Final。
> 已在保留原 MySQL/Qdrant/Redis 数据卷的本地栈上验证：
> Kafka 容器 `healthy`，`log.dirs=/var/lib/kafka/data`（持久卷生效），
> `num.partitions=4`、`auto.create.topics.enable=true`；
> topic `praxis_document_ingest` 自动创建（4 partitions，RF=1）；
> Debezium 完成 `Snapshot completed` 并输出 `Starting streaming`；
> 从 topic 读到的真实变更事件与 MySQL `document_job_outbox` 行一致
> （`artifact_id` 匹配，`op=r`，仅 outbox 行、无 DDL 污染）。
> 完整 Go 端到端（`rag-document-worker` 消费 Kafka → 文档 `succeeded`）依赖并行的
> Kafka 传输层改造，本目录只负责部署与 CDC 投递链路。

## 为什么需要它

异步新增文档的投递有两条**互斥**的路径，由 `RAG_OUTBOX_BRIDGE` 选择：

| 模式 | 谁把 `document_job_outbox` 的行变成 Kafka 消息 | 适用场景 |
| --- | --- | --- |
| `relay`（默认） | `rag-service` 进程内的 outbox relay 轮询 PENDING 行并发送 | SQLite、本地开发、没有 CDC 基础设施的部署 |
| `debezium` | Debezium Server 读取 MySQL binlog 中该表的 INSERT 事件并写入 Kafka | 生产环境：投递不依赖应用进程，事务提交即保证有投递 |

两种模式**不能同时开启**，否则同一行会被投递两次。重复投递本身安全（worker 幂等），但会浪费 Kafka 容量并让排障变复杂。使用 `debezium` 时应用**不会**启动进程内 relay。

## 启动

1. 在 `.env` 里填好：

```dotenv
RAG_OUTBOX_BRIDGE=debezium
# 必须与 Debezium SMT 的 topic.replacement 一致
RAG_DOCUMENT_TOPIC=praxis_document_ingest
# 宿主机进程（rag-service / worker / kafka 工具）：宿主机映射端口
RAG_KAFKA_BOOTSTRAP_SERVERS=127.0.0.1:9092
# CDC 专用账号（首次初始化 MySQL 数据目录时创建，脚本也会幂等补建）
RAG_MYSQL_DEBEZIUM_USER=debezium
RAG_MYSQL_DEBEZIUM_PASSWORD=change-me
```

2. 一条命令启动并自检（需要 Docker；本仓库已在 Windows + Docker Desktop 实测）：

```powershell
.\scripts\start-stack.ps1
```

脚本会依次：起依赖（`RAG_OUTBOX_BRIDGE=debezium` 时带 `--profile debezium`）→ 等 MySQL healthy →
等 Kafka healthy → 可创建 cache/document/search/DLQ topic → 执行 `rag-state-migrate --apply` →
等待 Debezium "Starting streaming" → 启动 `dist/bin/rag-service.exe`。
只想准备依赖与数据库时加 `-MigrateOnly`。

### Go 后端使用

```powershell
# 构建（一次即可）
pwsh -File scripts/build-go.ps1 -SkipFrontend

# 终端 1：API（RAG_OUTBOX_BRIDGE=debezium 时不会启动进程内 relay）
dist\bin\rag-service.exe

# 终端 2：文档 worker（消费 Debezium 投递到 Kafka 的 schema-3 事件）
dist\bin\rag-document-worker.exe
```

两者的 `RAG_*` / `RAG_MYSQL_*` / `RAG_KAFKA_*` 环境必须一致；状态库必须是 MySQL。
端到端验证脚本：`python scripts/verify-debezium-e2e.py`（需要本目录的 compose
栈与 `dist/bin` 已就绪，脚本使用临时 fake 模型 API，不会写入真实知识库配置）。

3. 用 `RAG_OUTBOX_BRIDGE=debezium` 启动 `rag-service` 和 `rag-document-worker`。

## Kafka 拓扑与地址

| 使用方 | bootstrap | 说明 |
| --- | --- | --- |
| 宿主机进程（`start-stack.ps1`、`dist/bin/*`、本机 kafka CLI） | `127.0.0.1:9092` | compose 把容器内 `EXTERNAL` listener（29092）发布到宿主 9092，并对外通告 `127.0.0.1:9092` |
| compose 容器（app profile、Debezium、worker 容器） | `kafka:9092` | `INTERNAL` listener；compose 的 app/debezium 已自动使用，可用 `RAG_APP_KAFKA_BOOTSTRAP_SERVERS` 覆盖 |
| Debezium sink | `${RAG_KAFKA_BOOTSTRAP_SERVERS:kafka:9092}`（application.properties） | 容器的环境变量被 compose 设为 `kafka:9092`；**不要**把宿主值传进容器 |

- 单节点 KRaft（broker + controller 同进程），`kafka_data` 持久化。
- 自动创建 topic 已开启，默认 4 partitions、副本数 1；已知 topic 也可显式创建。
- 开发 compose 使用 PLAINTEXT；生产请配置 SASL/TLS，并同步更新 Debezium 的
  `debezium.sink.kafka.producer.*` 安全属性（本仓库未提供生产安全示例）。

## 消息长什么样（schema 3 / Debezium 事件）

topic `praxis_document_ingest` 上的真实变更事件（Debezium JSON，`value.schemas.enable=false`）：

```json
{
  "before": null,
  "after": {
    "id": "f2c005b83e1e49e895a5e9063858b3c8",
    "artifact_id": "4555932a-1a17-46c9-858a-127349e001ed",
    "namespace": "manuals",
    "name": "cdc-probe.txt",
    "status": "PENDING",
    "attempts": 0,
    "retry_at": 1.7941354837376814E9,
    "created_at": 1.7941354837376814E9,
    "published_at": null,
    "last_error": null
  },
  "source": {"version": "3.7.1.Final", "connector": "mysql",
             "db": "praxis_rag", "table": "document_job_outbox", "file": "mysql-bin.000002"},
  "op": "c",
  "ts_ms": 1794135700187
}
```

worker 直接读 `after` 取标识符，**不需要额外的 SMT/脚本转换**；文件字节始终从
MySQL 的 `document_jobs` 读回，不经过 Kafka。`ByLogicalTableRouter` 只负责把每条
记录路由到单一合法 topic（`RAG_DOCUMENT_TOPIC`）。

## 冒烟验证

```powershell
# 1. Kafka 是否 healthy（不要只看容器 running）
docker inspect --format "{{.State.Health.Status}}" praxis-rag-kafka
# 期望：healthy

# 2. broker API 可达 + 查看 topic（容器内使用内部 listener kafka:9092）
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-broker-api-versions.sh --bootstrap-server kafka:9092 | Select-Object -First 3
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --list

# 3. Debezium 是否在 streaming（不要只看容器 running）
docker logs praxis-rag-debezium --tail 40 2>&1 | Select-String "Starting streaming|Snapshot completed"

# 4. 发件箱与 topic 是否一致（只读探测；有已提交行时能看到事件）
docker exec praxis-rag-mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -e "SELECT artifact_id FROM praxis_rag.document_job_outbox ORDER BY created_at DESC LIMIT 3;"'
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-console-consumer.sh `
  --bootstrap-server kafka:9092 --topic praxis_document_ingest `
  --from-beginning --timeout-ms 5000 2>$null | Select-String artifact_id

# 5. 端到端：提交一次上传，get_document_status 应最终变成 succeeded
```

`kafka-console-consumer.sh` 的探测是**信息性**的：没有历史消息或 offset 已提交时可能
读不到内容；发件箱 PENDING 行 + worker 状态终态才是结论性证据。

## 回退到 relay

```powershell
$env:RAG_OUTBOX_BRIDGE="relay"
docker compose --profile debezium stop debezium
# 重启 rag-service 与 rag-document-worker
```

应用内 relay 会在下一轮轮询（默认 2 秒）接手所有 PENDING 行；历史未标记行会被重新
投递，worker 幂等。

## 实测踩坑（Kafka 版）

| 现象 | 原因 | 修正 |
| --- | --- | --- |
| 文档 topic 出现 `databaseName`/`tableChanges` 的 DDL 事件 | SMT 会重写**所有**连接器记录（包括 schema change），worker 会把 DDL 当无效通知 | `debezium.source.include.schema.changes=false`；本目录配置已设 false，实测 topic 只保留 outbox 行 |
| `Failed to load mandatory config value 'debezium.sink.type'` | 配置目录是 `/debezium/config`，不是 `/debezium/conf` | compose 挂载 `/debezium/config/application.properties` |
| `Failed to establish connection with the database:`（消息为空） | CDC 账号是 `caching_sha2_password`，而配置写了 `ssl.mode=disabled`，JDBC 取不到服务端公钥 | `debezium.source.database.ssl.mode=preferred`（镜像自带自签证书） |
| 宿主机连不上 `kafka:9092`，或容器连不上 `127.0.0.1:9092` | Kafka 通告地址区分 internal/external listener | 宿主机用 `127.0.0.1:9092`，compose 容器用 `kafka:9092`；compose 已为 app/debezium 分别设置 |
| 容器重建后 topic/offset 丢失 | `apache/kafka` 默认 `log.dirs=/tmp/kafka-logs`，未落到挂载卷 | compose 设置 `KAFKA_LOG_DIRS=/var/lib/kafka/data` 并挂载 `kafka_data`（已实测） |
| worker 收不到通知 | 应用仍以 `RAG_OUTBOX_BRIDGE=relay` 启动了进程内 relay，或 topic 不一致 | `RAG_OUTBOX_BRIDGE=debezium`；SMT 的 `topic.replacement` 与 `RAG_DOCUMENT_TOPIC` 必须相同 |

## 已知限制

- **单 broker 开发栈**：副本数 1，不提供高可用；生产请使用多 broker KRaft 集群并调整
  `RAG_KAFKA_BOOTSTRAP_SERVERS` 为多个地址。
- **地址双份**：Kafka 通告地址区分宿主机与容器；把 `127.0.0.1:9092` 写进容器或把
  `kafka:9092` 写到宿主机都会连接失败。compose 已分别为 app/debezium 设置内部地址。
- **首次启动会做一次初始快照**（默认 `snapshot.mode=initial`），把现存发件箱行作为
  `op: r` 事件重放；worker 对已完成任务直接 ACK，不会重复索引。
- **发件箱保留期**：应用按 `RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS`（默认 7 天）删除旧行。
  Debezium 停机超过该值可能错过事件；建议保留期大于最长停机时间。
- **Debezium 配置目录**是 `/debezium/config`（不是 `/debezium/conf`）；compose 已按此挂载。
- **MySQL CDC 认证**：`caching_sha2_password` 在未加密连接上需要
  `debezium.source.database.ssl.mode=preferred`（镜像自带自签证书）。
- 单实例 Debezium 即可；多实例并发读同一 binlog 需要额外做主备/分区规划，本仓库未提供。
- 镜像 tag 固定为 `quay.io/debezium/server:3.7.1.Final`；升级前请重新核对 Kafka sink 属性名。
