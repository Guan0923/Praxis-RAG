# 缓存读模型刷新链路接线说明（internal/cacheconsumer + cmd/rag-cache-worker）

本文说明 Debezium 表级事件 → Kafka → `rag-cache-worker` → MySQL `cache_revisions`
修订水位 → Redis 读模型快照的完整接线，是 `docs/cdc-migration.md` §7（缓存消费者）
在代码层的落地索引。契约以 `docs/cdc-migration.md` 为准；本文描述的是当前实现。

涉及代码：

| 位置 | 职责 |
| --- | --- |
| `internal/cacheconsumer/cacheconsumer.go` | 传输无关的 `Processor`：解码、映射 key、调用状态层、周期补偿 |
| `cmd/rag-cache-worker/main.go` | 生产接线：配置解析、`state.New`、Redis 装配、Kafka 消费循环、退出码 |
| `internal/mq/message.go`（`DecodeCacheChange`） | 事件解码与表/定位列白名单（ignorable 与 nack 的分类） |
| `internal/mq/consumer.go` | `mq.Handler`（true=ACK、false=nack 重试）、DLQ 发布、offset 提交 |
| `internal/state/cache_sync.go` | `Invalidate/RefreshKey/Revalidate/DirtyKeys/RefreshDirty/MarkAllDirty` 与 Redis watermark |
| `internal/state/cache_events.go` | 写路径 `touch()`：`cache_revisions.revision+1`、`updated_at`，不再写 outbox |
| `deploy/debezium/application-cache.properties` | 缓存连接器：7 张表、定位列、单 topic 路由 |

## 1. 数据流

```text
业务写事务（MySQL）
  └─ touch(keys) 只更新 cache_revisions（revision+1、updated_at）
        └─ binlog
             └─ Debezium 缓存连接器（7 张表、只采集定位列）
                  └─ Kafka RAG_CACHE_TOPIC（单 topic，按表路由）
                       └─ rag-cache-worker（mq.PullConsumer + cacheconsumer.Processor）
                            ├─ 解码：表 + op + before/after 定位列
                            ├─ KeysForChange：去重后的 (kind, scope)
                            ├─ state.Revalidate：Invalidate（事务内 revision+1）
                            │                      + RefreshKey（事务内读脱敏视图、提交后 Put）
                            ├─ Redis Put 失败：记录 attempts/last_error，事件照常 ACK
                            └─ RunRecovery：每 30s 扫脏 key 继续 RefreshKey（补偿）
```

读路径 `GetOrLoad` 未命中时走同一把 `cache_revisions` 行锁的一致性快照并带修订号回填，
旧回填会被 Redis watermark 拒绝；因此事件重复、乱序、迟到都不会把旧值写回。

## 2. 缓存连接器（Debezium）

配置文件 `deploy/debezium/application-cache.properties`，与任务连接器完全独立
（独立 `database.server.id`、独立 offset/schema-history 文件、独立健康端口）：

- 7 张表全量行变更（c/u/d，含 DELETE）投递到 `RAG_CACHE_TOPIC`；
- `skip.messages.without.change=false`：即使本次只改了非定位列（正文摘要、settings 等）
  也投递一条“这一行变了”，消费端据此递增 revision；
- `include.schema.changes=false`：DDL/DDL 合成记录不进入 topic；
- `tombstones.on.delete=false`：不需要 value=null 的 tombstone，delete 事件的 before
  镜像已含定位列；
- `snapshot.mode=initial`：首次启动（无 offset）对现存行投递 `op=r`，用于切换后的
  新版本基线；
- `column.include.list` 只含定位列，不含文件内容、查询正文、凭据、`settings`、
  `mcp_catalogs.data`、`upload_operations.result/error` 等；
- 单 topic 路由由 `ByLogicalTableRouter` 完成；消费者按 `source.table` 映射缓存 key；
- MySQL 侧要求 `binlog_row_image=FULL`（`docker-compose.yml` 已配置），否则 delete
  事件缺少非主键定位列，会被解码器判为 malformed 而重试/DLQ。

### 2.1 表与定位列（`mq.cacheChangeLocators` 白名单）

| 业务表 | 采集的定位列 | 说明 |
| --- | --- | --- |
| `knowledge_bases` | `namespace`, `version` | `namespace` 是缓存 scope |
| `documents` | `namespace`, `name`, `document_id`, `del_flag` | `del_flag` 用于软删事件 |
| `mcp_catalogs` | `namespace` | |
| `document_heads` | `namespace`, `document_id`, `version` | |
| `document_leases` | `namespace`, `resource`, `owner`, `operation_id`(可空) | |
| `upload_operations` | `id`, `namespace`, `name`, `status` | |
| `upload_steps` | `operation_id`, `name`, `status` | |

解码器把表名去库名后与白名单匹配；`before/after` 只保留上表列，因此即使连接器
列配置漂移，也不会有额外数据进入消费者。

### 2.2 事件形态与 `op`

生产事件是 Debezium 原始 JSON（`debezium.format.value.schemas.enable=false`）：

```json
{"before":{...},"after":{"namespace":"manuals","name":"a.pdf"},"source":{"table":"documents"},"op":"u"}
```

测试/手工重放接受应用原生信封（schema 1）：

```json
{"schema":1,"table":"documents","op":"u","before":{"namespace":"manuals","name":"a.pdf"},"after":{"namespace":"manuals","name":"a.pdf"}}
```

`op` 取值：`c`（insert）、`u`（update，含主键更新）、`r`（初始快照）、`d`（delete）。
定位列从 `after` 取（c/u/r），delete 从 `before` 取。

| 解码结果 | 判定 | 消费行为 |
| --- | --- | --- |
| 空 body / JSON `null`（tombstone） | ignorable | ACK，不做任何状态变更 |
| `op=t`（TRUNCATE） | ignorable | ACK |
| 表不在 7 张白名单内 | ignorable | ACK |
| 无 `table`/`op` 的 DDL 合成记录 | ignorable | ACK |
| 合法 JSON 但不是对象 / 信封 schema 非法 | `ErrPayloadInvalid` | nack（重试→DLQ） |
| 已知表但 c/u/r 缺 after 镜像 | `ErrPayloadInvalid` | nack |
| delete 缺 before 镜像 | `ErrPayloadInvalid` | nack |
| 已知表缺非空定位列 | `ErrPayloadInvalid` | nack |
| 不支持的 `op` | `ErrPayloadInvalid` | nack |

“nack”在 `mq.PullConsumer` 内是同一进程内指数退避重试同一条记录；达到
`MaxAttempts` 后把原始 payload 发布到 `RAG_CACHE_DLQ_TOPIC` 并提交 offset。

## 3. Topic / 消费组 / DLQ

| 配置项 | 默认值 | 用途 |
| --- | --- | --- |
| `RAG_CACHE_TOPIC` | `praxis_state_cache_refresh` | 缓存连接器 → 缓存 worker |
| `RAG_CACHE_GROUP` | `praxis_state_cache_refresh_workers` | 缓存 worker 消费组 |
| `RAG_CACHE_DLQ_TOPIC` | `praxis_state_cache_refresh_dead_letters` | 重试耗尽后的原始 payload |
| `RAG_CACHE_DLQ_GROUP` | `praxis_state_cache_refresh_dead_letters` | `rag-dead-letter-worker --kind cache` 消费组 |

约束：三个业务 topic/group 与三个 DLQ topic/group 互不复用（`mq.Settings.Validate`
会拒绝冲突）。DLQ 由 `cmd/rag-dead-letter-worker --kind cache` 消费：它只按
`table/op` 记录告警并 ACK，不重放、不修改业务状态；缓存收敛由 `RefreshDirty`、
TTL（默认 60s）和下一次业务写入兜底。

### 3.1 缓存 worker 环境变量

| 变量 | 默认 | 说明 |
| --- | --- | --- |
| `RAG_CACHE_MAX_ATTEMPTS` | `5` | 单条事件进程内重试上限；`0` = 无限重试（不 DLQ，需谨慎） |
| `RAG_CACHE_WORKER_CONCURRENCY` | `1` | 处理器 goroutine 数，范围 1..64 |
| `RAG_KAFKA_BOOTSTRAP_SERVERS` | — | 必填；未配置时 worker 以退出码 2 退出 |
| `RAG_REDIS_*` | — | Redis 可选；未启用时只维护 MySQL 修订水位，读路径回源 MySQL |

`RAG_CACHE_MAX_ATTEMPTS` 的 `0` 与 `mq.ConsumerConfig.MaxAttempts=0` 语义一致
（无限重试，永不进 DLQ），仅建议在排障时临时使用。

## 4. 事件 → 缓存 key 映射（`cacheconsumer.KeysForChange`）

`kind` 与 scope 的语义见 `docs/state-cache.md`；映射与写路径 `internal/state` 的
`touch()` 保持同一套 key，因此“应用写入”和“直接 SQL UPDATE/DELETE/同名重建”
走同一条收敛路径。

| 业务表 | 变更后需要失效/重建的 key |
| --- | --- |
| `knowledge_bases` | `kb_list("")`、`kb_public(ns)`、`documents(ns)` |
| `documents` | `kb_list("")`、`documents(ns)` |
| `mcp_catalogs` | `catalog(ns)` |
| `document_heads` | `heads(ns)`、`documents(ns)`* |
| `document_leases` | `leases(ns)` |
| `upload_operations` | `upload_list(ns)`、`upload_status(id)` |
| `upload_steps` | `upload_status(operation_id)` |

\* `documents` 视图的查询会 `LEFT JOIN document_heads` 取发布版本，所以
`document_heads` 事件额外刷新 `documents(ns)`（`docs/cdc-migration.md` §7.1 的表格
未列出这一项，属于实现补充，见 §9）。

细节：

- `ns` 取事件行镜像的 `namespace`；`id/operation_id` 同理。缺失时该 key 被跳过，
  整条事件若没有可用 key 则安全 ACK（解码器已先保证非空定位列存在）。
- 表名允许带库名前缀，比较前会去掉并转小写。
- 同一事件内 key 去重；状态层还会按 `(kind, scope)` 排序后再加锁，避免交叉锁序。
- `upload_steps` 不刷新 `upload_list`：列表摘要不包含步骤。

### 4.1 与 `MarkAllDirty` 基线覆盖集合的一致性

`CacheSynchronizer.MarkAllDirty`（`rag-cdc-cutover --mark-cache-baseline`）按业务表现有
行推导全部 key，不依赖事件：

| kind | scope 来源 |
| --- | --- |
| `kb_list` | 固定 `""` |
| `kb_public`、`documents` | `SELECT DISTINCT namespace FROM knowledge_bases` |
| `catalog` | `SELECT DISTINCT namespace FROM mcp_catalogs` |
| `heads` | `SELECT DISTINCT namespace FROM document_heads` |
| `leases` | `SELECT DISTINCT namespace FROM document_leases` |
| `upload_list` | `SELECT DISTINCT namespace FROM upload_operations` |
| `upload_status` | `SELECT DISTINCT id FROM upload_operations` |

与事件映射一致；差异仅在“推导源”：事件按行映射，基线按现存行集合映射。因此
`MarkAllDirty` 不会为“已没有任何业务行的孤儿 scope”建 key，这类旧缓存行靠 TTL
过期（读路径未命中后回源）。空库上 `MarkAllDirty` 只返回 2 个固定 key，可安全重复执行
（每次继续递增 revision）。

## 5. `Revalidate` 语义（状态层）

`Refresher` 是缓存消费者需要的最小接口，`*state.CacheSynchronizer` 直接实现：

```go
type Refresher interface {
    Revalidate(ctx context.Context, keys []state.CacheKey) error
    RefreshDirty(ctx context.Context, limit int) error
}
```

`Revalidate(keys)` = `Invalidate(keys)` + 对每个 key 调用 `RefreshKey`：

1. **Invalidate**：单事务内对去重、排序后的 key 执行
   `INSERT INTO cache_revisions (kind, scope, revision, updated_at) VALUES (?,?,1,?)
    ON DUPLICATE KEY UPDATE revision = revision + 1, updated_at = ?`。
   提交后这些 revision 就是“脏状态”；不写任何 outbox 行。未知 kind 直接报错
   （不会产生半开事务）。
2. **RefreshKey(kind, scope)**：
   - 无注册 loader → `(false, nil)`（不知如何重建，留给下次注册/忽略）；
   - 打开事务，`SELECT revision FROM cache_revisions WHERE kind=? AND scope=? FOR UPDATE`
     与读路径 `GetOrLoad` 未命中快照使用同一把行锁；
   - 在事务内用已注册 loader 读取脱敏视图（与读路径同一份 SQL），提交；
   - 提交后用**该 revision** 调 `Backend.Put(kind, scope, revision, value, value==nil)`；
     Redis watermark 按十进制字符串比较版本，旧 revision 不能覆盖新值，删除标记
     （`value==nil`）同样受保护；
   - 成功：`UPDATE cache_revisions SET refreshed_revision = GREATEST(refreshed_revision, ?),
     last_error = NULL`；
   - Redis 写失败：`UPDATE ... attempts = attempts + 1, last_error = <安全错误类别>`
     （只记录类别名并截断，绝不写原始错误/连接串），返回 `(false, nil)`；
   - 只有 MySQL 错误（事务/loader/水位更新）才返回 `error`。
3. Redis 未启用时使用 `DisabledStateCache`：`Put` 不落盘但仍推进
   `refreshed_revision`，即“只维护水位、按需回源”；之后启用 Redis 时读路径会自然回填。

因此：**Redis 故障不是消息失败**。事件照常 ACK，脏 revision 留在 MySQL，
`RefreshDirty` 与 TTL 兜底。

## 6. ACK 规则

`Processor.MessageHandler` 返回 `true` 表示 ACK（提交 offset），`false` 表示 nack
（同进程重试，耗尽后 DLQ 再提交）：

| 场景 | 返回值 | 说明 |
| --- | --- | --- |
| 事件解码为 ignorable（tombstone/DDL/未知表/TRUNCATE） | `true` | 例行 ACK，不产生状态变更 |
| `KeysForChange` 得到空集合 | `true` | 定位不了 key，安全 ACK |
| `Revalidate` 成功（含 Redis 写失败已记 dirty） | `true` | 只有 MySQL 状态层失败才 nack |
| `Revalidate` 返回 MySQL 错误 | `false` | 重试；`Invalidate` 已提交的脏行仍由 `RefreshDirty` 收敛 |
| 解码为 `ErrPayloadInvalid`（坏 JSON/缺镜像/缺定位列） | `false` | 重试 `MaxAttempts` 次后进 DLQ |
| 达到 `MaxAttempts` | 提交 offset | 原始 payload 先发布到 `RAG_CACHE_DLQ_TOPIC`；缓存 worker 的 `OnExhausted` 只记日志并返回 nil |

要点：

- **只有 MySQL 状态层失败才 nack**；Redis/Put 失败记录 dirty 并 ACK，不反复进 DLQ
  （契约 §5.3、§7.3）。
- `Revalidate` 先提交 `Invalidate` 再逐个 `RefreshKey`；如果中途 MySQL 失败，已提交的
  revision 让该 key 保持 dirty，补偿循环会继续重试，不会因为消息被 DLQ 而丢失收敛。
  只有 `Invalidate` 自身失败（DB 全不可用）时本次失效才没有丢进 MySQL，此时依赖
  DLQ 告警 + TTL + 下一次写入。
- DLQ 发布失败时 `mq.PullConsumer` 仍会提交 offset（既有语义，只记录 lastError）；
  因此“DB 长时间不可用 + DLQ 也不可写”是唯一可能真正丢无效化事件的窗口，需要
  监控 `pipeline.cache` 与 DLQ topic 积压。
- handler 可能并发执行（`RAG_CACHE_WORKER_CONCURRENCY>1`）；`Handle` 不使用共享
  可变状态，状态层用 MySQL 行锁/事务保证幂等。

## 7. `RefreshDirty` 补偿循环

`RunRecovery(ctx)` 在 worker 启动时立即执行一轮，之后按固定间隔循环；`Stop()`/ctx
取消时退出（`Recover` 用 `recoverMu` 串行，避免多轮叠加）：

```go
const (
    DefaultRecoveryInterval = 30 * time.Second // 补偿周期
    DefaultRecoveryBatch    = 200              // 每轮最多处理的 key 数
)
```

`RefreshDirty(limit)` 的逻辑：

1. `DirtyKeys(limit)` 查询
   `SELECT kind, scope FROM cache_revisions
    WHERE (refreshed_revision < revision OR last_error IS NOT NULL)
      AND kind IN (<9 个已注册 kind>)
    ORDER BY kind, scope LIMIT ?`
   —— `last_error IS NOT NULL` 保证“Redis 写失败但 revision 已是最新”的 key 也会重试；
   kind 过滤避免退役 kind 的历史行永久占满批次造成饥饿；按 `(kind, scope)` 排序让
   多轮扫描有确定顺序。
2. 对每个 key 调 `RefreshKey`；Redis 失败会重新记录 dirty，下一轮继续；MySQL 错误
   只返回第一个错误给调用方记日志（不会中断整轮）。

因此收敛条件：MySQL 可写 + Redis 可用（或禁用）+ worker 运行。Redis 长时间故障时
脏 key 数会持续累积，恢复后按批清空，Console 的
`pipeline.cache.dirty_keys` / `pipeline.cache.refresh_failures` 可观察。

## 8. 运维指标与排障

| 指标（`/v1/admin/monitor/overview`） | 含义 | 期望 |
| --- | --- | --- |
| `pipeline.cache.status` / `dirty_keys` / `refresh_failures` | `cache_revisions` 中 `refreshed_revision < revision` / `last_error IS NOT NULL` 的行数 | 稳态为 0；持续增长说明 Redis 写失败或 loader 失败 |
| `pipeline.debezium.cache.status` | 缓存连接器健康 | `ok`；`degraded` 时检查 connector 与 binlog 权限 |
| `pipeline.kafka.groups.cache`（`committed_offset/end_offset/consumer_lag`） | 缓存消费组进度 | lag 持续增长说明 worker 停摆或处理过慢 |
| 日志 `cache revalidated` / `cache revalidation failed` | 每条事件处理的 table/op/key 数与安全错误类别 | 失败日志按类别定位 MySQL/Redis |
| 日志 `cache dirty-key recovery failed` | `RefreshDirty` 的 MySQL 错误 | 检查 DB 可用性 |
| 日志 `cache change exhausted; recorded on the dead-letter topic` | 事件重试耗尽 | 配合 DLQ topic 与 `rag-dead-letter-worker --kind cache` 排查 |

注意：`internal/state` 的监控 SQL 统计 `cache_revisions` 全表（不按 kind 过滤），
而 `DirtyKeys` 只处理已注册的 9 个 kind；正常库没有退役 kind 行，两者应一致。
另外 `RefreshDirty` 只把第一个 MySQL 错误返回给调用方记日志，不影响其余 key 的处理和下一轮补偿。

## 9. 已知限制与实现差异（供评审/后续任务）

1. **补偿周期可配置**：`DefaultRecoveryInterval=30s`、`DefaultRecoveryBatch=200` 是
   默认值；`rag-cache-worker` 读取 `RAG_CACHE_WORKER_RECOVER_SECONDS`（秒）覆盖周期，
   `RAG_CACHE_WORKER_CONCURRENCY` 控制处理并发。
2. **`document_heads` 的额外 key**：实现会给 `document_heads` 事件同时刷新
   `documents(ns)`（文档视图 join 了 heads），`docs/cdc-migration.md` §7.1 的表格已同步。
3. **namespace 变更的 UPDATE**：`KeysForChange` 对 `op=u` 取 `before` 与 `after` 的
   namespace 并集，因此手工 SQL 跨 namespace 移动行时新旧 scope 都会立即收敛；`c/r`
   只取 `after`，`d` 只取 `before`。
4. **`documents` 软删与 `heads` 视图**：`heads` loader 通过 `documents.del_flag`
   过滤，但 `documents` 事件只刷新 `kb_list`/`documents`，与旧写路径的 key 集合一致；
   `heads` 在软删/恢复后最多滞后一个 Redis TTL（默认 60s）。如需严格立即收敛，可在
   `documents` 映射中追加 `heads(ns)`（同时评估事件量与 loader 依赖）。
5. **基线只覆盖现存业务行**：`MarkAllDirty` 不会为已无业务行的孤儿 scope 建 key
   （见 §4.1），旧缓存行靠 TTL 过期。
6. **DLQ 不重放**：cache DLQ 只记录告警；若某条无效化事件在 DB 完全不可用期间被
   耗尽并提交 offset，该次失效没有进入 MySQL 脏水位，只能靠 TTL/下一次写入收敛。
   监控 `pipeline.cache.dirty_keys` 与 DLQ 积压即可发现。
7. **`RAG_CACHE_MAX_ATTEMPTS=0` 无限重试**：与 mq 语义一致，不建议生产使用。

## 10. 测试与手工验证

```bash
# 单元测试（不依赖 MySQL/Redis/Kafka；Processor 通过 fake Refresher/Decoder 驱动）
go test -count=1 ./internal/cacheconsumer/

# 编译与静态检查
go vet ./internal/cacheconsumer/ ./cmd/rag-cache-worker/

# 状态层集成测试（需要 RAG_TEST_MYSQL_URL 指向库名含 test 的一次性 MySQL）
RAG_TEST_MYSQL_URL='mysql://rag:...@127.0.0.1:3307/praxis_unit_test?charset=utf8mb4' \
  go test -count=1 ./internal/state/ -run 'Cache|ReadViews'
```

`internal/cacheconsumer` 的测试覆盖：7 张表的 c/u/d 映射、带库名前缀、未知表/缺定位
列/缺 before 镜像的忽略、`Handle` 成功与状态层失败（nack）、decoder 的
ignorable/malformed 契约、`RunRecovery` 立即执行并可按 `Stop()` 退出、以及 JSON
行镜像解码。状态层的 Invalidate/RefreshKey/watermark/脏扫描/Redis 故障记录/
`MarkAllDirty` 由 `internal/state` 的缓存测试覆盖（MySQL 集成）。

手工联调建议顺序：启动 MySQL/Redis/Kafka 与两个 Debezium 连接器 →
`rag-cdc-cutover --mark-cache-baseline` → 启动 `rag-cache-worker` → 对业务表执行一条
`UPDATE`/`DELETE` → 观察 worker 日志出现 `cache revalidated`、Redis 值更新、
`pipeline.cache.dirty_keys` 归零；再停 Redis 执行一次 UPDATE，确认事件仍被 ACK、
`dirty_keys/refresh_failures` 增长，恢复 Redis 后自动归零。
