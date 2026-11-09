# Debezium 桥接：MySQL binlog → RocketMQ

这个目录让 MySQL 里已经提交的事务**必然**产生一条 RocketMQ 通知，中间不依赖应用进程是否存活。

> **本机实测记录（2026-11-08，Windows + Docker Desktop）**：下表所有配置都已在真实
> MySQL 8.0.46 + RocketMQ 5.3.3 + Debezium Server 3.7.1.Final 上跑通，端到端链路为
> `MySQL 事务提交 → binlog → Debezium → topic praxis_document_ingest → 宿主消费 → worker → succeeded`。
> 排查过程中踩到的坑都写在下面的"实测踩坑"一节。

## 为什么需要它

异步新增文档的投递有两条**互斥**的路径，由 `RAG_OUTBOX_BRIDGE` 选择：

| 模式 | 谁把 `document_job_outbox` 的行变成 MQ 消息 | 适用场景 |
| --- | --- | --- |
| `relay`（默认） | `rag-service` 进程内的 `DocumentOutboxRelay` 线程轮询 PENDING 行并发送 | 没有 CDC 基础设施的部署 |
| `debezium` | Debezium Server 读取 MySQL binlog 中该表的 INSERT 事件并投递 | 生产环境：投递不依赖应用进程，事务提交即保证有投递 |

两种模式**不能同时开启**，否则同一行会被投递两次。重复投递本身安全（worker 幂等），但会浪费 broker 容量并让排障变复杂。

## 启动

1. 在 `.env` 里填好：

```dotenv
RAG_OUTBOX_BRIDGE=debezium
# 必须与 Debezium 的 SMT 目标 topic 一致（RocketMQ topic 名不允许点号）
RAG_DOCUMENT_TOPIC=praxis_document_ingest
# gRPC proxy 端口；8081 在部分机器上被 McAfee 等服务占用
RAG_ROCKETMQ_PROXY_PORT=18081
RAG_ROCKETMQ_ENDPOINT=127.0.0.1:18081
# CDC 专用账号（首次初始化 MySQL 数据目录时创建，脚本也会幂等补建）
RAG_MYSQL_DEBEZIUM_USER=debezium
RAG_MYSQL_DEBEZIUM_PASSWORD=change-me
```

2. 一条命令启动并自检（**必须在普通 PowerShell 窗口**运行，agent 沙箱打不开 Docker 的 named pipe）：

```powershell
.\scripts\start-debezium-stack.ps1
```

脚本会依次：起依赖 → 等 MySQL healthy → 建 CDC 账号 → 建表 → 预建 topic/group → 起 Debezium → 等 streaming → 打印状态。
失败会明确抛错，不会假装就绪。

3. 用 `RAG_OUTBOX_BRIDGE=debezium` 启动 `rag-service` 和 `rag-document-worker`。

## 消息长什么样（实测抓取）

topic `praxis_document_ingest` 上的真实消息：

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

worker 的 `validate_document_envelope()` 直接读 `after` 取标识符，**不需要额外的 SMT/脚本转换**；
文件字节始终从 MySQL 的 `document_jobs` 读回，不经过 MQ。

## 实测踩坑（都已在配置里修正）

| 现象 | 原因 | 修正 |
| --- | --- | --- |
| `Failed to load mandatory config value 'debezium.sink.type'` | 配置目录是 `/debezium/config`，不是 `/debezium/conf` | compose 挂载改为 `/debezium/config/application.properties` |
| `Failed to establish connection with the database:`（消息为空） | CDC 账号是 `caching_sha2_password`，而配置写了 `ssl.mode=disabled`，JDBC 取不到服务端公钥 | `database.ssl.mode=preferred`（镜像自带自签证书） |
| `contains illegal characters, allowing only ^[%\|a-zA-Z0-9_-]+$` | 连接器默认 destination 是 `<prefix>.<db>.<table>`，**RocketMQ topic 不允许点号** | 用 `ByLogicalTableRouter` SMT 把所有记录路由到单一合法 topic |
| `No route info for this topic` / 端口冲突 | gRPC proxy 端口写死 8081，配置里放 broker.properties 无效；端口被 McAfee `macmnsvc` 占用 | proxy 用 **JSON** 配置 `conf/rmq-proxy.json` 的 `grpcServerPort`；compose 用同一变量映射宿主端口 |
| `Port 8080 seems to be in use` | Debezium(Quarkus) 与 broker 共享网络命名空间时 8080 冲突 | `quarkus.http.port=0` |
| `errors resolving rocketmq:8081` | broker 通告容器名，宿主机解析不了；Remoting 需要 broker 地址 | Debezium 容器 `network_mode: service:rocketmq` 共享 broker 网络命名空间，`brokerIP1` 保持 `127.0.0.1` |

## 冒烟验证

```powershell
# 1. Debezium 是否在 streaming（不要只看容器 running）
docker logs praxis-rag-debezium --tail 30 2>&1 | Select-String "Starting streaming|Snapshot completed"

# 2. 发件箱与 topic 是否一致
docker exec praxis-rag-mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -e "SELECT artifact_id FROM praxis_rag.document_job_outbox ORDER BY created_at DESC LIMIT 3;"'
docker exec praxis-rag-rocketmq sh mqadmin consumeMessage -n rocketmq-namesrv:9876 -t praxis_document_ingest -g praxis_verify_probe -c 1

# 3. 端到端：提交一次上传，get_document_status 应最终变成 succeeded
```

## 回退到 relay

```powershell
$env:RAG_OUTBOX_BRIDGE="relay"
docker compose --profile debezium stop debezium
# 重启 rag-service 与 rag-document-worker
```

应用内 relay 会在下一轮轮询（默认 2 秒）接手所有 PENDING 行；历史未标记行会被重新投递，worker 幂等。

## 已知限制

- **宿主机 8081 被占用时**必须改 `RAG_ROCKETMQ_PROXY_PORT`：proxy 会把**自身端口**通告给客户端，所以容器内监听端口与宿主发布端口必须一致（compose 已用同一变量绑定两端）。
- **`brokerIP1` 是单值**：它同时被 Remoting（Debezium）和 gRPC proxy 用于通告。本仓库让 Debezium 共享 broker 网络命名空间，从而保持 `127.0.0.1` 对宿主客户端和 Debezium 都成立。若把 Debezium 放到独立容器且不共享命名空间，需要改成两者都能解析的地址。
- **首次启动会做一次初始快照**（默认 `snapshot.mode=initial`），把现存发件箱行作为 `op: r` 事件重放；worker 对已完成任务直接 ACK，不会重复索引。
- **发件箱保留期**：应用按 `RAG_DOCUMENT_OUTBOX_RETENTION_SECONDS`（默认 7 天）删除旧行。Debezium 停机超过该值可能错过事件；建议保留期大于最长停机时间。
- 单实例部署即可；多实例并发读同一 binlog 需要额外做主备/分区规划，本仓库未提供。
- 镜像 tag 固定为 `quay.io/debezium/server:3.7.1.Final`（已确认该 tag 与 `io.debezium:debezium-server-rocketmq` 3.7.1.Final 均存在）。升级前请重新核对 RocketMQ sink 的属性名。
