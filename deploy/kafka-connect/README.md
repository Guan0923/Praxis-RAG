# 分布式 Kafka Connect + Debezium MySQL 连接器

本目录是 CDC 采集层的**唯一生产配置源**。两个 Debezium MySQL 连接器以 Kafka
Connect task 的形式运行在分布式 Connect 集群里，替代原来的两个独立 Debezium
Server 进程。

> 旧 `deploy/debezium/` 下的 `application*.properties` 只保留给切换窗口回滚
> 使用，不再是启动路径；见 [deploy/debezium/README.md](../debezium/README.md)。

## 1. 为什么改成 Kafka Connect

旧部署的两个 Debezium Server 各自把 binlog 位点和 schema history 存在本地文件
（`debezium_data` / `debezium_cache_data` 卷）。进程或宿主机故障后，位点不会转移，
只能人工在同一台机器上重启。Kafka Connect 把连接器配置、source offset 与 task
状态放进 Kafka 内部 topic：

| 数据 | Kafka 内部 topic | 说明 |
| --- | --- | --- |
| 连接器配置 | `praxis_connect_configs` | PUT 注册的 connector config |
| source offset（binlog 位点） | `praxis_connect_offsets` | 任务在哪个 worker 重启都从同一保存位点继续 |
| task/connector 状态 | `praxis_connect_status` | 监控与自动重启读取的权威状态 |

每个连接器另有**独立的 schema history topic**（`praxis_cdc_document_schema_history`、
`praxis_cdc_cache_schema_history`），不能共用，否则两路采集会互相污染表结构历史。

集群由两个 worker 组成（`connect-1`、`connect-2`，同一个 `GROUP_ID`）。一个 worker
宕机后，Kafka Connect 的 group rebalance 会把它的 task 分配给存活 worker，task 从
`praxis_connect_offsets` 里的位点继续读取 binlog。

## 2. 目录与固定名称

| 连接器 | 模板 | 采集范围 | 目标 topic | schema history |
| --- | --- | --- | --- | --- |
| `praxis-document-cdc` | `connectors/document-cdc.json` | 仅 `document_jobs`，仅 INSERT 与 `schedule_version` 增长的 UPDATE | `RAG_DOCUMENT_TOPIC`（默认 `praxis_document_ingest`） | `praxis_cdc_document_schema_history` |
| `praxis-cache-cdc` | `connectors/cache-cdc.json` | 8 张读模型表，全部 c/u/d | `RAG_CACHE_TOPIC`（默认 `praxis_state_cache_refresh`） | `praxis_cdc_cache_schema_history` |

连接器名称是**固定契约**：注册、监控、自动重启都按这两个名字寻址。修改名称需要
同时修改 `.env`、监控与脚本。

两个连接器的关键差异：

- **文档连接器**只保留定位列（`artifact_id`、`namespace`、`schedule_version`），
  `skip.messages.without.change=true`，`skipped.operations=d`，所以删除业务文档
  不会产生 Kafka 通知；`schedule_version` 变化的重试才会重新投递。
- **缓存连接器**必须保留删除前镜像（`tombstones.on.delete=false`），因为
  `rag-cache-worker` 要用 `before` 里的定位字段刷新读模型。
- 两连接器使用**不同的 MySQL 账号**（`debezium` / `debezium_cache`）、不同的
  `database.server.id`，一个账号被锁/权限故障不会同时打挂两路。

模板使用 `${VAR}` / `${VAR:-default}` 占位符；未设置的必填变量会让注册失败，而
不是把空值写进连接器配置。

## 3. 启动与注册

Compose 提供：

- `connect-1`：REST `127.0.0.1:${RAG_CONNECT_PORT_1:-8083}`；
- `connect-2`：REST `127.0.0.1:${RAG_CONNECT_PORT_2:-8084}`；
- `connect-init`（`app` profile）：一次性运行 `rag-connect register` 并按固定名称
  等待两个 connector 与 task 都 `RUNNING`，成功退出后 `app` 才启动。

`connect-init` 会先等待**所有**配置的 worker 可访问、并确认每个 worker 上报同一
个 `DEBEZIUM_CONNECT_VERSION`，然后逐个 `PUT /connectors/{name}/config`：

- 连接器不存在 → 创建；
- 配置完全相同 → 不发送更新（避免重启健康的采集 task）；
- 配置不同 → 更新。已保存的 source offset 不受影响。

宿主机开发流程由 `scripts/start-stack.ps1` 完成同样的事（等待 REST →
`rag-connect register` → `rag-connect wait`），并在启动前用幂等 SQL 补齐两个 CDC
账号，兼容已存在的 MySQL 数据卷。

手工执行：

```powershell
# 只看模板展开结果，不访问 Connect
dist/bin/rag-connect.exe register --offline --connector-dir deploy/kafka-connect/connectors

# 注册/更新并等待两个 connector 与 task 都 RUNNING
$env:RAG_CONNECT_URLS = "http://127.0.0.1:8083,http://127.0.0.1:8084"
dist/bin/rag-connect.exe register --wait 180s
dist/bin/rag-connect.exe wait --wait 180s
dist/bin/rag-connect.exe status
```

`register` 在容器内运行时需要 `RAG_MYSQL_HOST=mysql`、`RAG_MYSQL_PORT=3306`、
`RAG_CONNECT_KAFKA_BOOTSTRAP_SERVERS=kafka:9092`；宿主机启动脚本会在调用前临时
覆盖这几个变量，避免把 `127.0.0.1` 写进容器内连接器。

## 4. 生产部署要求

1. **至少两台机器**，每台一个 Connect worker，使用同一个 `RAG_CONNECT_GROUP_ID`。
2. 每个 worker 必须安装**同一版本**的 Debezium MySQL Connector
   （`DEBEZIUM_CONNECT_VERSION`，当前 `3.7.1.Final`）。`rag-connect register` 会在
   版本不一致时拒绝注册。
3. `RAG_CONNECT_TOPIC_REPLICATION_FACTOR` 设为 3（至少 2）；内部 topic 的副本数
   低于 broker 数时，单 broker 故障会丢连接器状态。
4. REST 默认没有认证。只监听 loopback / 内网，前面放反向代理与认证；不要把
   8083 暴露到公网。
5. worker 的 `OFFSET_STORAGE_TOPIC` / `CONFIG_STORAGE_TOPIC` /
   `STATUS_STORAGE_TOPIC` 必须与 `.env` 中的三个内部 topic 一致，且不能与
   其他 Connect 集群共用。
6. 连接器配置中的数据库地址、Kafka bootstrap 必须是**worker 容器/主机可达**的
   地址；不要在宿主机注册时写入 `127.0.0.1`。
7. 每个连接器的 `database.server.id` 全局唯一；schema history topic 每连接器
   独立。

## 5. 自动恢复 FAILED task

Kafka Connect 本身不会自动重启 FAILED task。`rag-service` 内运行的 supervisor
每 `RAG_CONNECT_SUPERVISOR_INTERVAL_SECONDS`（默认 15 秒）读取两个连接器的
`tasks[].state`：

- 只对 `FAILED` 的 task 调用 `POST /connectors/{name}/tasks/{id}/restart`；
- 指数退避（`RAG_CONNECT_RESTART_BASE_BACKOFF_SECONDS` 起，封顶
  `RAG_CONNECT_RESTART_MAX_BACKOFF_SECONDS`）；
- 最多 `RAG_CONNECT_RESTART_MAX_ATTEMPTS` 次（默认 5），耗尽后记录并保留失败原因，
  不再自动重启，Console 显示「自动重启已耗尽（N 次），需人工处理」；
- task 稳定运行 `30s` 后才清零尝试计数，避免 flapping 任务无限重试；
- 有多个 `rag-service` 副本时，每个副本都会尝试重启；重启前会重新读取状态，
  重复调用通常返回 409/无操作。生产建议只让一个副本启用 supervisor
  （`RAG_CONNECT_SUPERVISOR_ENABLED=false` 关闭其余副本），或接受该幂等重试。

不运行 `rag-service` 的站点可以单独运行：

```powershell
dist/bin/rag-connect.exe supervise
```

## 6. 故障演练（与 `scripts/verify-debezium-e2e.py` 对应）

### 6.1 worker 宕机（验收 3）

```powershell
# 找到 document task 所在 worker（connector/task status 的 worker_id）
dist/bin/rag-connect.exe status
docker stop praxis-rag-connect-1        # 换成实际 worker

# 在接管窗口内提交一个文档；存活 worker 应接管 task 并从保存位点继续
dist/bin/rag-connect.exe wait --wait 180s

docker start praxis-rag-connect-1
```

判定标准不是 connector `RUNNING`，而是新 worker 上的 task `RUNNING`，且提交的
文档最终 `succeeded`（`rag-document-worker` 真正领取并处理）。

### 6.2 task FAILED、worker 在线（验收 4）

用一个只影响缓存路径的故障：

```sql
-- 锁住缓存连接器的账号，并 kill 它已有的连接
ALTER USER 'debezium_cache'@'%' ACCOUNT LOCK;
SELECT id FROM information_schema.processlist WHERE user = 'debezium_cache';
KILL <id>;
```

缓存 task 进入 `FAILED`，文档连接器保持 `RUNNING`。解除故障：

```sql
ALTER USER 'debezium_cache'@'%' ACCOUNT UNLOCK;
```

supervisor 应自动把缓存 task 重启到 `RUNNING`，无需在 Dashboard 手工 restart；
Console 的「缓存采集」行显示重启次数与恢复结果。

### 6.3 暂停缓存连接器（验收 2）

```powershell
curl.exe -X PUT http://127.0.0.1:8083/connectors/praxis-cache-cdc/pause
# 产生缓存表变更并提交一个文档；文档必须完成
curl.exe -X PUT http://127.0.0.1:8083/connectors/praxis-cache-cdc/resume
```

缓存连接器暂停期间 document 连接器不受影响；resume 后缓存连接器从保存位点补齐
暂停期间的变更。

## 7. 监控

`GET /v1/admin/monitor/overview` 的 `pipeline.debezium.tasks` / `.cache` 分别对应
两个连接器，新增字段：

| 字段 | 含义 |
| --- | --- |
| `connector` / `connector_state` | 固定连接器名与集群状态 |
| `worker_id` | 当前承载连接器/task 的 worker（故障转移后会变化） |
| `tasks[]` | 每个 task 的 `id`、`state`、`worker_id` |
| `failed_tasks` / `failure_reason` | 失败 task 数量与首个失败原因 |
| `restarting` / `restart_attempts` / `restart_exhausted` | supervisor 当前动作与预算 |
| `recovery` | `restarting` / `exhausted` / `recovered`（空表示未发生） |
| `last_restart_at` / `last_recovered_at` / `next_restart_at` | 时间线 |
| `supervised` | 是否由 `rag-service` supervisor 托管 |

`capture_progress` 始终是 `null`：binlog 位点在 Kafka Connect 的 offset topic 里，
应用侧不读取，Console 显示「未知」而不是编造百分比。缓存消费者滞后与 CDC 采集
状态是两件事，分别显示在「缓存采集」和「Kafka 消费进度」区域。

## 8. 切换、回滚与已知限制

- **旧位点不能导入**：Debezium Server 的 file offset 与 Kafka Connect 的
  `praxis_connect_offsets` 格式不同。首次启动使用 `snapshot.mode=initial` 做一次
  初始快照（会产生 `op=r` 的全量事件），然后从快照位点持续采集；切换前未完成的
  文档任务用 `rag-cdc-cutover --backfill` 补齐。
- **保留旧卷**：`debezium_data` / `debezium_cache_data` 在回滚窗口内不要删除，
  也不要执行 `docker compose down -v`。
- **回滚**：恢复 `deploy/debezium/application*.properties` 对应的两个 Debezium
  Server 服务与旧卷，停掉 `connect-1/connect-2/connect-init`，并回退到仍读取
  `RAG_DEBEZIUM_*_HEALTH_URL` 的旧 `rag-service` 二进制/配置（当前版本已忽略旧健康
  变量）。回滚期间业务消费者必须容忍重复通知（按 `schedule_version` / 幂等键吸收）。
- **重复是正常语义**：worker 故障或 task 重启可能重放少量已投递消息；消费者按
  定位标识 + `schedule_version` 幂等处理。
- **未实机验证项**：本仓库没有 Docker daemon，Connect 集群、真实 failover 与
  账号锁定演练需要在预发环境执行 `scripts/verify-debezium-e2e.py` 并记录到
  `VALIDATION.md`。
