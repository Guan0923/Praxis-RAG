# Debezium 桥接：MySQL binlog → Kafka（双连接器）

本目录定义 **两个** Debezium Server 实例，共同替代已下线的三张事务 outbox 表：

| 实例 | Compose 服务 / 容器 | 采集表 | 投递 topic（env） | 消费组（env） | 健康端口（loopback） | 状态卷 |
| --- | --- | --- | --- | --- | --- | --- |
| 任务连接器 | `debezium` / `praxis-rag-debezium` | `document_jobs` | `RAG_DOCUMENT_TOPIC` | `RAG_DOCUMENT_GROUP` | `RAG_DEBEZIUM_PORT`（8083） | `debezium_data` |
| 缓存连接器 | `debezium-cache` / `praxis-rag-debezium-cache` | `knowledge_bases`、`documents`、`mcp_catalogs`、`api_keys`、`document_heads`、`document_leases`、`upload_operations`、`upload_steps` | `RAG_CACHE_TOPIC` | `RAG_CACHE_GROUP` | `RAG_DEBEZIUM_CACHE_PORT`（8084） | `debezium_cache_data` |

配置分别在 `application.properties`（任务）与 `application-cache.properties`（缓存），
契约来源是 [`docs/cdc-migration.md`](../../docs/cdc-migration.md) §1–§5、§10。

```text
document_jobs ─────► 任务连接器：只投递 INSERT 与 schedule_version 变化 ──► RAG_DOCUMENT_TOPIC ──► rag-document-worker

knowledge_bases、documents、mcp_catalogs、api_keys、document_heads、document_leases、
upload_operations、upload_steps ──► 缓存连接器：全量 c/u/d ──► RAG_CACHE_TOPIC ──► rag-cache-worker
```

MySQL 仍是唯一事实来源，Kafka 只传通知；消息只带定位标识（`artifact_id`/`namespace`/
`schedule_version`、缓存表定位列），任务参数、文件字节、KB settings、catalog data、
凭据等一律不经过 Kafka，由消费者按 ID 回 MySQL 读取。搜索为同步执行，不经过 Kafka，
也没有搜索任务表/通知（`search_tasks` 为历史表，保留数据但不再读写）。

## 为什么必须拆成两个连接器

`skipped.operations` / `skip.messages.without.change` 是**连接器级**配置，无法按表差异化：

- 任务表需要“**只**投递 INSERT 与 `schedule_version` 变化”：心跳、进度、终态 UPDATE 是噪声，
  清理 DELETE 不是任务信号。
- 缓存表需要“**每次**行变更都投递（含 DELETE）”：只改 `knowledge_bases.settings` 这类
  不在定位列白名单内的 UPDATE 也必须让缓存 revision 递增；删除也必须让缓存失效。

因此同一 MySQL 上跑两个连接器，使用不同的 `database.server.id`、不同的 offset/schema-history
卷和不同的健康端口，互不影响。过滤放在采集阶段（采集列收敛 + `skip.messages.without.change`），
不使用 `io.debezium.transforms.Filter`（它依赖库存镜像里没有的 JSR-223 脚本引擎）。

## 启动

1. 在 `.env` 里填好（`.env.example` 有完整注释）：

```dotenv
# 宿主机进程（rag-service / worker / kafka 工具）
RAG_KAFKA_BOOTSTRAP_SERVERS=127.0.0.1:9092
# CDC 专用账号（首次初始化 MySQL 数据目录时创建，脚本也会幂等补建）
RAG_MYSQL_DEBEZIUM_USER=debezium
RAG_MYSQL_DEBEZIUM_PASSWORD=change-me
# 两个连接器的健康端口（Console 监控页只读探测）
RAG_DEBEZIUM_PORT=8083
RAG_DEBEZIUM_CACHE_PORT=8084
# 宿主机进程使用；compose 的 app profile 用容器内地址覆盖
RAG_DEBEZIUM_HEALTH_URL=http://127.0.0.1:8083/q/health
RAG_DEBEZIUM_CACHE_HEALTH_URL=http://127.0.0.1:8084/q/health
```

2. 一条命令启动并自检（需要 Docker；Windows + Docker Desktop 已实测）：

```powershell
.\scripts\start-stack.ps1
```

脚本会依次：起依赖（含两个 Debezium 连接器）→ 等 MySQL healthy → 等 Kafka healthy →
显式创建/校验 2 个业务 topic 与 2 个 DLQ topic（文档/缓存）→ 执行 `rag-state-migrate --apply` →
等待两个连接器 `/q/health` 为 UP 且日志出现 `Starting streaming` → 启动 `rag-service`、
`rag-document-worker` 与 `rag-cache-worker`。搜索为同步执行，不需要额外 worker。
只想准备依赖与数据库时加 `-MigrateOnly`。

### Go 后端使用

```powershell
pwsh -File scripts/build-go.ps1 -SkipFrontend   # 构建 dist/bin
dist\bin\rag-service.exe                        # API（提交即受理，不再发布通知）
dist\bin\rag-document-worker.exe                # 消费 RAG_DOCUMENT_TOPIC
dist\bin\rag-cache-worker.exe                   # 消费 RAG_CACHE_TOPIC（缓存读模型）
```

三者的 `RAG_*` / `RAG_MYSQL_*` / `RAG_KAFKA_*` 环境必须一致；状态库必须是 MySQL。
端到端验证脚本：`python scripts/verify-debezium-e2e.py`（需要本目录的 compose 栈与
`dist/bin` 已就绪，脚本使用临时 fake 模型 API，不会写入真实知识库配置）。

## Kafka 拓扑与地址

| 使用方 | bootstrap | 说明 |
| --- | --- | --- |
| 宿主机进程（`start-stack.ps1`、`dist/bin/*`、本机 kafka CLI） | `127.0.0.1:9092` | compose 把容器内 `EXTERNAL` listener（29092）发布到宿主 9092，并对外通告 `127.0.0.1:9092` |
| compose 容器（app profile、两个 Debezium、worker 容器） | `kafka:9092` | `INTERNAL` listener；compose 已自动使用，可用 `RAG_APP_KAFKA_BOOTSTRAP_SERVERS` 覆盖 |
| Debezium sink（两个连接器） | `${RAG_KAFKA_BOOTSTRAP_SERVERS:kafka:9092}` | 容器的环境变量被 compose 设为 `kafka:9092`；**不要**把宿主值传进容器 |

- 单节点 KRaft（broker + controller 同进程），`kafka_data` 持久化。
- 自动创建 topic 已开启，默认 4 partitions、副本数 1；已知 topic 由 `start-stack.ps1` 显式创建。
- 开发 compose 使用 PLAINTEXT；生产请配置 SASL/TLS，并同步更新两个连接器的
  `debezium.sink.kafka.producer.*` 安全属性（本仓库未提供生产安全示例）。

## 消息长什么样

### 任务连接器（document_jobs）

`application.properties` 的 `column.include.list` 只保留
`artifact_id` / `namespace` / `schedule_version`，因此 topic 上的物理事件形如
（`value.schemas.enable=false`）：

```json
{"before":null,
 "after":{"artifact_id":"4555932a-...","namespace":"manuals","schedule_version":1},
 "source":{"version":"3.7.1.Final","connector":"mysql","db":"praxis_rag",
           "table":"document_jobs","file":"mysql-bin.000002"},
 "op":"c","ts_ms":1794135700187}
```

- `op`：首次快照为 `r`，事务内新受理为 `c`，重发（调度版本递增）为 `u`。
- 一个 `ByLogicalTableRouter` 把 `document_jobs` 路由到 `RAG_DOCUMENT_TOPIC`；
  `key.enforce.uniqueness=false` 保留原始行 key。
- 消费者兼容“应用原生 envelope”（便于单测和手工补发）：
  `{"schema":4,"artifact_id":"...","namespace":"...","schedule_version":3}`。
- **搜索已改为同步执行**：`search_tasks` 是历史表（保留历史数据、不再写入也不再读取），
  不再采集、不再路由到 `RAG_SEARCH_TOPIC`，也不再有搜索 worker/DLQ。

### 缓存连接器（定位列白名单）

```json
{"before":{"namespace":"manuals","name":"a.pdf"},
 "after":{"namespace":"manuals","name":"a.pdf"},
 "source":{"db":"praxis_rag","table":"documents"},
 "op":"u","ts_ms":1794135700187}
```

`before`/`after` 只含该表在 `column.include.list` 里的定位列，永远没有文件内容、查询正文、
凭据、KB settings、catalog data；删除事件 `op=d` 的 `before` 镜像同样只含定位列。
消费者按 `docs/cdc-migration.md` §7.1 的映射把 `table + op + 定位列` 转成缓存 key，
再回 MySQL 重建脱敏视图或使缓存失效。

## 投递语义与状态职责

### 任务连接器为什么只放行 INSERT / schedule_version 变化

1. `column.include.list` 只含定位列：心跳列（`lease_until`）、进度列（`stage/operation_id`）、
   终态列（`status/error/owner/content` 等）不在采集列内，永远不会出现在事件里。
2. `skip.messages.without.change=true`：UPDATE 只有在**被采集列**发生变化时才投递，
   因此心跳/进度/终态 UPDATE 被连接器丢弃，唯一被投递的 UPDATE 是 `schedule_version` 递增。
3. `skipped.operations=d`：`GCJobs`/切换脚本的清理 DELETE 不进入业务 topic，也不产生 tombstone。

即使过滤配置失效，消费者按“`artifact_id` + `namespace` + `schedule_version`
相等 + 租约/状态条件”领取，重复或过期通知会被安全跳过并 ACK（至少一次投递的幂等要求）。

### 首次快照、断点恢复、删除与重复投递

- **首次快照**（`snapshot.mode=initial`）：为任务表现存行投递 `op=r`，为缓存业务表现存行
  各投递一次。任务消费者跳过终态/已领取/版本不符的行，遗留未完成任务因此天然补投一次；
  切换阶段依赖这一特性，再用 `rag-cdc-cutover --backfill` 把未完成任务 `schedule_version`
  递增到基线，避免“快照完成前的 UPDATE 丢失”。缓存侧则因此重建“新版本基线”。
- **断点恢复**：offset 与 schema history 分别写在 `debezium_data` / `debezium_cache_data`
  卷上，容器重启从上次提交的 binlog 位点继续，不会重新快照。只有明确要做全量重放时才删除
  对应卷。
- **删除**：任务连接器不投递 DELETE；缓存连接器投递 DELETE（`before` 只含定位列），
  且 `tombstones.on.delete=false` 不产生 value 为 null 的 tombstone。
- **重复投递**：Kafka 至少一次。任务消费者在 MySQL 事务内条件领取，缓存消费者按 revision
  水位幂等重建，重复事件不会导致重复执行或旧值覆盖新值。
- **binlog 保留期**：MySQL 的 `binlog-expire-logs-seconds`（compose 默认 7 天）必须大于
  Debezium 最长停机时间；停机超期需要重新快照（删除对应状态卷）并重新切换。

状态职责保持分离：

| 组件 | 职责 |
| --- | --- |
| MySQL `document_jobs` | 任务事实与 `schedule_version`；所有状态变更都在事务内 |
| Debezium（两个连接器） | 读取 binlog 并投递 Kafka；**不回写** MySQL 状态 |
| Kafka 消费组 | 记录消费位点与滞后 |
| `rag-cache-worker` | 消费缓存事件，递增 `cache_revisions` 并重建 Redis 读模型（失败由 `refreshed_revision < revision` 兜底重试） |

## 监控联调

Compose 把两个连接器的 Quarkus 健康端口发布到宿主机 loopback：

- 任务连接器：`127.0.0.1:${RAG_DEBEZIUM_PORT:-8083}/q/health`；
- 缓存连接器：`127.0.0.1:${RAG_DEBEZIUM_CACHE_PORT:-8084}/q/health`。

宿主机进程设置 `RAG_DEBEZIUM_HEALTH_URL` / `RAG_DEBEZIUM_CACHE_HEALTH_URL`；compose 的
`app` profile 默认使用 `http://debezium:8080/q/health` 与
`http://debezium-cache:8080/q/health`。健康探测只说明连接器存活与运行检查通过；
**binlog 采集位点只在 Debezium 内部**，应用读不到，监控页的 `capture_progress` 恒为 `null`，
不会用任务数量推算积压。

## 冒烟验证

```powershell
# 1. 两个容器是否都 healthy/running（不要只看一个）
docker inspect --format "{{.State.Status}}" praxis-rag-debezium praxis-rag-debezium-cache

# 2. broker API 可达 + 两个业务 topic 与两个 DLQ topic 存在
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 --list

# 3. 两个连接器是否都在 streaming
docker logs praxis-rag-debezium --tail 40 2>&1 | Select-String "Starting streaming|Snapshot completed"
docker logs praxis-rag-debezium-cache --tail 40 2>&1 | Select-String "Starting streaming|Snapshot completed"

# 4. 任务表与 topic 是否一致（只读探测；有已受理任务时能看到事件）
docker exec praxis-rag-mysql sh -c 'mysql -uroot -p"$MYSQL_ROOT_PASSWORD" -N -e "SELECT artifact_id, schedule_version FROM praxis_rag.document_jobs ORDER BY created_at DESC LIMIT 3;"'
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-console-consumer.sh `
  --bootstrap-server kafka:9092 --topic praxis_document_ingest `
  --from-beginning --timeout-ms 5000 2>$null | Select-String "schedule_version"

# 5. 缓存连接器是否覆盖业务表（documents 事件应只含定位列）
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-console-consumer.sh `
  --bootstrap-server kafka:9092 --topic praxis_state_cache_refresh `
  --from-beginning --timeout-ms 5000 2>$null | Select-String '"table":"documents"'

# 6. 端到端：提交一次上传，get_document_status 应最终变成 succeeded
python scripts/verify-debezium-e2e.py
```

`kafka-console-consumer.sh` 的探测是**信息性**的：没有历史消息或 offset 已提交时可能
读不到内容；MySQL 任务状态、缓存 revision 与 worker 终态才是结论性证据。

## 实测踩坑

| 现象 | 原因 | 修正 |
| --- | --- | --- |
| topic 出现 `databaseName`/`tableChanges` 的 DDL 事件 | SMT 会重写**所有**连接器记录（包括 schema change），消费者会把它当无效通知 | `debezium.source.include.schema.changes=false`；本目录两个配置都已设 false |
| `Failed to load mandatory config value 'debezium.sink.type'` | 配置目录是 `/debezium/config`，不是 `/debezium/conf` | compose 挂载 `/debezium/config/application.properties` |
| `Failed to establish connection with the database:`（消息为空） | CDC 账号是 `caching_sha2_password`，而配置写了 `ssl.mode=disabled`，JDBC 取不到服务端公钥 | `debezium.source.database.ssl.mode=preferred`（镜像自带自签证书） |
| 第二个连接器启动后第一个断开/反复重连 | 两个连接器用了同一个 `database.server.id` | 用 `DEBEZIUM_SERVER_ID` / `DEBEZIUM_CACHE_SERVER_ID` 区分（默认 184054 / 184055） |
| 两个连接器抢同一个 offset 文件 | 共享了状态卷 | 各自独立 volume：`debezium_data` / `debezium_cache_data` |
| 宿主机连不上 `kafka:9092`，或容器连不上 `127.0.0.1:9092` | Kafka 通告地址区分 internal/external listener | 宿主机用 `127.0.0.1:9092`，compose 容器用 `kafka:9092`；compose 已分别设置 |
| 容器重建后 topic/offset 丢失 | `apache/kafka` 默认 `log.dirs=/tmp/kafka-logs`，未落到挂载卷 | compose 设置 `KAFKA_LOG_DIRS=/var/lib/kafka/data` 并挂载 `kafka_data`（已实测） |
| 任务 topic 出现进度/终态 UPDATE 噪音 | 采集列包含 `status/stage/lease_until`，或 `skip.messages.without.change=false` | 按 `application.properties` 收敛 `column.include.list` 并保持 `skip.messages.without.change=true` |
| 缓存 topic 出现文件内容/凭据 | `column.include.list` 写成了通配或漏了表 | 按 `application-cache.properties` 的定位列白名单；`grep 'content\|sha256\|protected_key'` 自检 |
| 删除业务行后缓存不失效 | 缓存连接器也配置了 `skipped.operations=d` / `tombstones.on.delete=true` | 缓存连接器不要 skip DELETE；`tombstones.on.delete=false` 只是关闭 tombstone |

## 已知限制

- **单 broker 开发栈**：副本数 1，不提供高可用；生产请使用多 broker KRaft 集群并调整
  `RAG_KAFKA_BOOTSTRAP_SERVERS` 为多个地址。
- **地址双份**：Kafka 通告地址区分宿主机与容器；把 `127.0.0.1:9092` 写进容器或把
  `kafka:9092` 写到宿主机都会连接失败。compose 已分别为 app/两个 Debezium 设置内部地址。
- **首次启动会做一次初始快照**（`snapshot.mode=initial`）：任务行以 `op=r` 重放、业务行全量
  投递一次。消费者幂等，切换窗口内重复投递是预期行为。
- **binlog 保留期**：Debezium 停机超过 `binlog-expire-logs-seconds` 会错过事件；生产需按
  最长停机时间调大，并接受重新快照的切换流程。
- **Debezium 配置目录**是 `/debezium/config`（不是 `/debezium/conf`）；compose 已按此挂载。
- **MySQL CDC 认证**：`caching_sha2_password` 在未加密连接上需要
  `debezium.source.database.ssl.mode=preferred`（镜像自带自签证书）。
- **两个实例是固定数量**：任务表与缓存表的过滤语义不同才拆成两个；进一步按表拆分需要
  同步评估 `database.server.id`、offset 卷、topic 与消费者。
- 镜像 tag 固定为 `quay.io/debezium/server:3.7.1.Final`；升级前请重新核对 Kafka sink 属性名。
