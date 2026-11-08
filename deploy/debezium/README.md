# Debezium 桥接：MySQL binlog → RocketMQ

这个目录让 MySQL 里已经提交的事务**必然**产生一条 RocketMQ 通知，中间不依赖应用进程是否存活。

## 为什么需要它

异步新增文档的投递有两条**互斥**的路径，由 `RAG_OUTBOX_BRIDGE` 选择：

| 模式 | 谁把 `document_job_outbox` 的行变成 MQ 消息 | 适用场景 |
| --- | --- | --- |
| `relay`（默认） | `rag-service` 进程内的 `DocumentOutboxRelay` 线程轮询 PENDING 行并发送，成功后标记 PUBLISHED | SQLite、本地开发、没有 CDC 基础设施的部署 |
| `debezium` | Debezium Server 读取 MySQL binlog 中该表的 INSERT 事件并投递 | 生产环境：投递不依赖应用进程，事务提交即保证有投递 |

两种模式**不能同时开启**，否则同一行会被投递两次。重复投递本身是安全的（worker 幂等），但会浪费 broker 容量并让排障变复杂。

## 启动

1. 在 `.env` 里填好 Debezium 需要的值：

```dotenv
# 应用与 worker 都设为 debezium，关闭进程内 relay
RAG_OUTBOX_BRIDGE=debezium
# 消息主题必须与 Debezium 的 <topic.prefix>.<database>.<table> 一致
RAG_DOCUMENT_TOPIC=praxis.praxis_rag.document_job_outbox
# CDC 专用账号（首次初始化 MySQL 数据目录时创建）
RAG_MYSQL_DEBEZIUM_USER=debezium
RAG_MYSQL_DEBEZIUM_PASSWORD=change-me
# 容器内的 Debezium 必须能连到 broker；brokerIP1 默认 127.0.0.1 只适合宿主机客户端
RAG_ROCKETMQ_BROKER_IP=rocketmq
```

2. 启动依赖并带上 `debezium` profile：

```powershell
docker compose --profile debezium up -d
# 或使用脚本：$env:RAG_OUTBOX_BRIDGE="debezium"; .\scripts\start-stack.ps1
```

3. 预建主题（broker 关闭了自动建 topic）：

```powershell
docker exec praxis-rag-rocketmq sh mqadmin updatetopic `
  -n rocketmq-namesrv:9876 -c DefaultCluster -t praxis.praxis_rag.document_job_outbox
```

4. 用 `RAG_OUTBOX_BRIDGE=debezium` 启动 `rag-service` 和 `rag-document-worker`。

## 消息长什么样

Debezium 的 JSON value（`schemas.enable=false`）：

```json
{
  "before": null,
  "after": {
    "id": "9f0c…",
    "artifact_id": "2d1b9f5e-…",
    "namespace": "manuals",
    "name": "manual.pdf",
    "status": "PENDING",
    "attempts": 0,
    "retry_at": 1794058163.19,
    "created_at": 1794058163.19,
    "published_at": null,
    "last_error": null
  },
  "source": {"db": "praxis_rag", "table": "document_job_outbox", "…": "…"},
  "op": "c",
  "ts_ms": 1794058163192
}
```

worker 的 `validate_document_envelope()` 同时接受这种 Debezium 形态和 relay 形态
`{"schema":3,"artifact_id":…,"namespace":…,"name":…,"created_at":…}`，都只取标识符；
**文件字节始终从 MySQL 的 `document_jobs` 读回**，不经过 MQ。

## 冒烟验证

```powershell
# 1. Debezium 连接器是否启动
docker compose logs debezium | Select-String "Snapshot|streaming|ERROR"

# 2. 提交一个上传后，观察 binlog 事件是否被捕获
docker compose exec mysql mysql -uroot -p -e "SELECT id, artifact_id, status FROM praxis_rag.document_job_outbox ORDER BY created_at DESC LIMIT 5;"

# 3. 观察 worker 是否消费到（worker 日志）
#    "Document job accepted artifact_id=..." 由 API 打印
#    "Document job terminal artifact_id=... status=succeeded" 由 worker 打印

# 4. 直接看 topic 是否有积压/消息
docker exec praxis-rag-rocketmq sh mqadmin topicStatus -n rocketmq-namesrv:9876 -t praxis.praxis_rag.document_job_outbox
```

`get_document_status` 最终应返回 `succeeded`。若长期停在 `queued`，按顺序检查：
broker 是否可达、topic 是否预建、worker 是否以 `RAG_OUTBOX_BRIDGE=debezium` 且 **相同**
`RAG_DOCUMENT_TOPIC` 启动。

## 回退到 relay

```powershell
$env:RAG_OUTBOX_BRIDGE="relay"
$env:RAG_DOCUMENT_TOPIC="praxis_document_ingest"
docker compose --profile debezium stop debezium
# 重启 rag-service 与 rag-document-worker
```

应用内 relay 会在下一轮轮询（默认 2 秒）接手所有 PENDING 行。发件箱里未标记
PUBLISHED 的历史行会被重新投递，worker 幂等，不会重复入库。

## 已知限制

- **未在本仓库的开发环境里做过运行时验证。** 上面的属性名已对照 Debezium 官方发布说明核实，
  镜像 tag、broker 可达性、权限组合仍须在你的环境里按冒烟步骤确认。
- **broker 通告地址**：`deploy/rocketmq-broker.conf` 里 `brokerIP1=127.0.0.1`，只适合宿主机客户端。
  compose 已把该值参数化为 `${RAG_ROCKETMQ_BROKER_IP:-127.0.0.1}`；容器内 Debezium 需要把它设成
  compose 网络内可达的名字（如 `rocketmq`）或固定 IP/主机局域网 IP。改动该值后宿主机上的
  应用仍通过 gRPC proxy（`127.0.0.1:8081`）访问，不受影响。
- **主题名依赖库名与前缀**：`debezium.source.topic.prefix`（默认 `praxis`）+ 库名 +
  `document_job_outbox`。改 `RAG_MYSQL_DATABASE` 或前缀时，`RAG_DOCUMENT_TOPIC` 必须同步改，
  否则 worker 会订阅一个永远没有消息的主题。
- **发件箱保留期**：应用按 `RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS`（默认 7 天）删除旧行。
  若 Debezium 停机时间超过该值，binlog 里的事件仍可能被追上，但表行已被删除；建议保留期
  大于最长计划停机时间。
- **首次启动会做一次初始快照**（默认 `snapshot.mode=initial`），把表中现存的行作为 `op: r`
  事件重放。worker 对已完成任务直接 ACK，不会重复索引。
- 单实例部署即可；多实例并发读同一 binlog 需要额外做主备/分区规划，本仓库未提供。
