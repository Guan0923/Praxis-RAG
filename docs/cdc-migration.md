# CDC 迁移：从事务 outbox 到 Debezium 表级采集

- 状态：实施中。本文件是本次迁移的**唯一契约来源**；代码、配置、脚本与本文冲突时以本文为准，变更需同时更新本文。
- 迁移目标：删除 `document_job_outbox`、`search_task_outbox`、`cache_outbox`、历史 `document_task_outbox` 的写入/轮询/补发/清理；改由 Debezium 直接采集**任务表**和**影响缓存的业务表**，按表与事件用途路由到独立 topic；MySQL 仍是唯一事实来源，Kafka 只传通知。
- **2026-11-13 二次调整（搜索同步化）**：搜索改为一次调用同步返回，搜索任务/结果快照/搜索 topic/消费者/死信全部下线；`search_tasks`、`search_result_snapshots` 保留历史数据、不再读写。因此本文的搜索链路（schema 3 通知、搜索 worker、提交即受理、结果快照）均标为**历史/已下线**，迁移入口见 §12。文档任务（schema 4）与缓存链路不变。

## 1. 目标架构

```text
                    ┌──────────────────── Debezium（任务连接器，只采集 INSERT / schedule_version 变化）────────────────────┐
document_jobs ──────┤ skipped.operations=d；column.include.list 只含 artifact_id/namespace/schedule_version；               ├─► praxis_document_ingest
                    │ skip.messages.without.change=true；ByLogicalTableRouter 按表路由                                      │
                    └──────────────────────────────────────────────────────────────────────────────────────────────────────┘
                    ┌──────────── Debezium（缓存连接器，采集影响缓存读模型的业务表，全量 c/u/d）───────────────┐
knowledge_bases ────┤ column.include.list 只含定位标识；ByLogicalTableRouter 路由到单一缓存 topic                ├─► praxis_state_cache_refresh
documents / mcp_catalogs / api_keys / document_heads / document_leases / upload_operations / upload_steps ─┘
```

两个消费者完全独立，各自一个 topic + 消费组（搜索为同步执行，不再有消费者）：

| 事件类 | Topic（环境变量） | 消费组（环境变量） | 消费者 |
| --- | --- | --- | --- |
| 文档任务 | `RAG_DOCUMENT_TOPIC`（默认 `praxis_document_ingest`） | `RAG_DOCUMENT_GROUP`（默认 `praxis_document_ingest_workers`） | `cmd/rag-document-worker` |
| 缓存变更 | `RAG_CACHE_TOPIC`（默认 `praxis_state_cache_refresh`） | `RAG_CACHE_GROUP`（默认 `praxis_state_cache_refresh_workers`） | `cmd/rag-cache-worker`（新增） |

约束：topic 与消费组不得复用；DLQ topic 各自独立（`RAG_DOCUMENT_DLQ_TOPIC` / `RAG_CACHE_DLQ_TOPIC`）。DLQ 消费组同样独立。原搜索 topic/消费组/DLQ（`RAG_SEARCH_*`、`praxis_search`）已随搜索同步化下线。

## 2. 事件契约

物理消息即 Debezium 原生 JSON 变更事件（`debezium.format.value.schemas.enable=false`）。消费者同时兼容"应用原生 envelope"（便于测试与手工补发）。**逻辑通知只携带定位标识**，任务参数/检索参数一律由消费者按 ID 回 MySQL 读取。

### 2.1 文档任务通知（schema 4）

原 schema 3 携带 `name`、`created_at`，新契约删除；新增 `schedule_version`。

```json
{"schema":4,"artifact_id":"<uuid>","namespace":"<ns>","schedule_version":3}
```

Debezium 物理事件形如（`column.include.list` 已保证 `content` 等字段不出现）：

```json
{"before":null,"after":{"artifact_id":"<uuid>","namespace":"<ns>","schedule_version":3},"source":{"table":"document_jobs",...},"op":"c","ts_ms":1700000000000}
```

- `op`：首次快照为 `r`，事务内新受理为 `c`，重发（调度版本递增）为 `u`。
- 删除事件（`op=d`、tombstone、`before`-only）不是任务：直接 ACK，不重试、不进 DLQ。
- 调度版本不匹配、任务已终态、租约仍有效的记录一律安全跳过并 ACK（至少一次投递下的幂等要求）。

### 2.2 搜索任务通知（schema 3，已下线）

> **历史（2026-11-13 已删除）**：搜索改为同步执行，不再有 `search_tasks` 通知、schema 3 envelope、搜索 topic/group 或 `rag-search-worker`。以下仅存档，不再实现；`search_tasks` 表保留历史数据、不再读写。

原 schema 2 携带完整 `arguments`/`deadline`/`expires_at`；schema 3 只携带定位标识，参数回 MySQL 读取。

```json
{"schema":3,"result_id":"<uuid>","namespace":"<ns>","schedule_version":2}
```

物理事件同上（`source.table=search_tasks`）。旧 schema 2 消息不兼容，切换时排空。

### 2.3 缓存变更事件（schema 1）

缓存消费者从 Debezium 事件推导出"表 + 操作 + 行标识"，不需要业务正文：

```json
{"schema":1,"table":"documents","op":"u","before":{"namespace":"<ns>","name":"a.pdf"},"after":{"namespace":"<ns>","name":"a.pdf"}}
```

- `op`：`c`/`u`/`d`；tombstone（null value）忽略。
- `before`/`after` 只包含该表在 `column.include.list` 中列出的定位列，永远不含文件内容、查询正文、凭据、KB settings、catalog data 等。
- 消费者只使用 `table` + `op` + 需要的标识列，然后回 MySQL 重建脱敏视图或使缓存失效。

## 3. 表结构变更

### 3.1 新增列（追加在既有列之后，兼容旧库）

```sql
ALTER TABLE document_jobs ADD COLUMN schedule_version BIGINT NOT NULL DEFAULT 1;
```

搜索任务已下线：迁移不再给 `search_tasks` 追加 `schedule_version`（历史列保留原样）。

`cache_revisions` 从"只有版本号"扩展为"缓存脏状态 + 刷新水位"：

```sql
ALTER TABLE cache_revisions ADD COLUMN refreshed_revision BIGINT NOT NULL DEFAULT 0;
ALTER TABLE cache_revisions ADD COLUMN attempts INTEGER NOT NULL DEFAULT 0;
ALTER TABLE cache_revisions ADD COLUMN last_error VARCHAR(128) NULL;
ALTER TABLE cache_revisions ADD COLUMN updated_at DOUBLE NOT NULL DEFAULT 0;
```

迁移幂等（已存在列则跳过，逻辑与 `migrateSoftDelete` 相同）。新库由 `schemaTables` 直接创建新结构。

### 3.2 复用既有表

- `search_result_snapshots`（已存在）**保留为历史表**：搜索同步化后不再写入、不再读取，也不 DROP；artifact 事实来源改为 `artifact_snapshots`（见 [产物引用](result-references.md)）。
- `cache_revisions` 保持 `(kind, scope)` 主键和 `revision` 单调递增语义。

### 3.3 退役表

从 `schemaTables` 移除：`document_job_outbox`、`search_task_outbox`、`cache_outbox`、`document_task_outbox`。已有库通过迁移 CLI 显式删除（见 §8）。`document_tasks` 作为历史表保留但不再读写。

### 3.4 新增工具表（切换用）

```sql
cdc_cutover_state (k VARCHAR(64) PRIMARY KEY, v LONGTEXT NOT NULL)
```

键：`cdc_start_file`、`cdc_start_pos`、`cutover_at`、`schedule_baseline`、`phase`。

## 4. 调度版本与重发语义

### 4.1 版本规则

- 新任务在受理事务内 `schedule_version = 1` 写入。
- **可重试失败不立即发通知**：`Fail(retry=true)` 写入失败原因、`status='queued'`、`stage='retry_wait'` 和 `retry_at`，不递增 `schedule_version`。到期后由周期 `Sweep` 在同一事务内递增版本并重排，发出下一轮通知；这是唯一的自动重试路径。
- **租约过期**：`processing` 且 `lease_until <= now` 的任务由 `Sweep` 立即重排；`attempts` 已达上限时直接写 `failed`，不再启动执行。
- **普通排队不算重试**：仅因 `queued` 超过 120 秒（等 worker 或解析名额）不会被补发，不递增 `schedule_version`，也不消耗执行次数。
- `document_jobs.attempts` 是执行次数（MySQL 持久化，跨 Kafka 消息、worker 进程和重启），上限 3。第 3 次失败写入 `failed` 和最后失败原因；重复通知、`Sweep` 和租约恢复都不得启动第 4 次执行。
- 同一任务可多次递增；消费者只处理与数据库当前 `schedule_version` 相等的通知。
- 心跳、进度、完成/失败终态以及 `retry_wait` 等待都是普通 UPDATE，**不递增版本**。

### 4.2 Debezium 如何识别"同一任务需要再次执行"并排除噪声

任务连接器同时使用三个连接器级配置（无需脚本 SMT，库存镜像即可运行）：

1. `column.include.list`：只包含 `artifact_id/namespace/schedule_version`（`document_jobs`）。心跳列（`lease_until`）、进度列（`stage/operation_id`）、终态列（`status/error/owner/content` 等）不在采集列内。搜索表不采集。
2. `skip.messages.without.change=true`：UPDATE 事件只有在**被采集列**发生变化时才投递。因此心跳/进度/终态 UPDATE 被连接器丢弃；只有 `schedule_version` 递增的 UPDATE 会被投递。这是"同一任务需要再次执行"的唯一 UPDATE 信号。
3. `skipped.operations=d`：`GCOutbox`/`GCJobs` 一类清理 DELETE 不进入业务 topic（也不再产生 tombstone）。

补充：首次启动的初始快照会为 `document_jobs` 的**现存行**投递 `op=r` 事件（快照不受 `skipped.operations` 影响）。消费者对终态/已领取/版本不符的行安全跳过；对遗留的未完成任务则天然完成一次补投——切换阶段依赖这一特性（见 §8）。

### 4.3 为什么不用 Filter SMT

Debezium 的行级过滤 `io.debezium.transforms.Filter` 依赖 JSR-223 脚本引擎，且 `debezium-scripting` 与脚本引擎都不在库存镜像里；引入它们需要自定义镜像并扩大脚本执行面。使用"采集列收敛 + `skip.messages.without.change`"可以把过滤放在采集阶段，配置可审计、无需脚本，**且即使过滤配置失效，消费者按版本条件领取仍保证不重复执行**（纵深防御）。

### 4.4 为什么是两个 Debezium 连接器

任务表需要"只投递 INSERT 与调度版本变化"，缓存表需要"每次行变更都投递（含删除）"。`skipped.operations` / `skip.messages.without.change` 是**连接器级**配置，无法按表差异化，因此拆成两个 Debezium Server 实例（同一 MySQL，不同 `database.server.id`、不同 offset/schema-history 文件、不同健康端口）。

## 5. Worker 领取、续租与提交

### 5.1 文档任务

1. 解码通知，得到 `artifact_id/namespace/schedule_version`。
2. `Claim`：单事务 `SELECT ... FOR UPDATE`，条件为 `artifact_id + namespace`，校验 `schedule_version` 相等、`status IN (queued, processing)`、`lease_until <= now`，`retry_wait` 还要求 `retry_at <= now`；然后置 `processing/owner/execution_id/lease_until/attempts+1`。版本不等、终态、租约有效、重试未到期、行不存在 → 返回 nil；`attempts` 已达 3 时直接终态化并返回 nil。
3. 心跳/进度只更新本执行（`execution_id`  fencing）。
4. 成功：`Reconcile` 确认 journal 发布已提交后写 `succeeded`；失败：可重试则按 §4.1 写 `retry_at` 等待 `Sweep` 重排，不可重试或第 3 次失败写 `failed`。
5. **只有 DB 已记录领取、跳过或终态后才 ACK**；DB 不可用/写失败返回 nack，交给消费重试与 DLQ。

### 5.2 搜索任务（已下线）

> **历史（2026-11-13 已删除）**：搜索不再有 `Claim`/`Complete`/`Expire` 或 `search_result_snapshots` 写入。同步搜索在请求内完成检索 + 重排并直接返回；artifact 写入 `artifact_snapshots`，超时/取消由 `RAG_SEARCH_TIMEOUT_SECONDS` 与请求 ctx 约束。下文仅存档原排队实现。

原排队实现：`Claim` 条件增加 `deadline > now`；逾期在 `Claim`/`Get`/`Expire` 三处收敛为 `timed_out`。成功时在**同一 MySQL 事务**内写 `search_result_snapshots` 并把任务置 `succeeded`，提交后才 ACK。

### 5.3 提交位点原则

ACK（提交 Kafka 位点）当且仅当：
- 本轮通知已被判定为可安全跳过；或
- 任务已被领取（DB 已记录 processing）并完成本轮处理且终态/重排已落库；或
- 死信终态已落库。

Redis 写入失败、缓存刷新失败不是 nack 理由：缓存有独立的重试水位（§7.3），任务事实在 MySQL。

## 6. 搜索同步化后的事实来源（替代原“提交即受理/结果快照”）

### 6.1 同步直返

`runtime.Service.Search` 在一次请求内完成检索 + 重排并返回证据；`RAG_SEARCH_TIMEOUT_SECONDS`（默认 120 秒）在服务内派生 deadline，客户端断开/取消向下传递到 Qdrant 与模型调用。没有任务行、没有提交即受理、没有 `result_id`、没有轮询；旧客户端迁移见 [部署指南 §12](DEPLOYMENT.md#12-搜索同步化迁移与切换)。

### 6.2 artifact 事实来源

- 搜索命中可签发 artifact（源文件、图片、表格 JSON、解析产物），写入 MySQL `artifact_snapshots`，按 ID 独立读取（`GET /v1/artifacts/{artifact_id}` 与 `/content`）。
- 保留期沿用 `RAG_RESULT_TTL_SECONDS`（默认 3600 秒，语义为 artifact TTL）；到期后读取映射 410，客户端重新检索即可。
- `search_tasks`、`search_result_snapshots` 保留历史数据、不再读写；迁移不清空、不 DROP（与 `document_tasks` 先例一致）。

## 7. 缓存消费者

### 7.1 表 → 缓存类型/范围映射

| 业务表 | 变更后需要失效/重建的缓存 key |
| --- | --- |
| `knowledge_bases` | `kb_list("")`、`kb_public(ns)`、`documents(ns)` |
| `documents` | `kb_list("")`、`documents(ns)` |
| `mcp_catalogs` | `catalog(ns)` |
| `api_keys` | `api_keys("")` |
| `document_heads` | `heads(ns)`、`documents(ns)`（docs 视图 join heads 版本） |
| `document_leases` | `leases(ns)` |
| `upload_operations` | `upload_list(ns)`、`upload_status(id)` |
| `upload_steps` | `upload_status(operation_id)` |

`ns`/`id` 从 `after`（c/u）或 `before`（d）的定位列取。删除与同名重建都按同一映射处理；直接 SQL 修改也会产生 CDC 事件，因此同样收敛。

### 7.2 刷新流程（版本保护）

1. 解析事件 → 去重后的 `(kind, scope)` 集合。
2. 每个 key 在 MySQL 事务内 `revision = revision + 1` 并写入 `updated_at`（不建 outbox 行）。
3. 同一事务内用已注册 loader 读取脱敏视图（与读路径 `GetOrLoad` 的 snapshot 使用同一把 `revision` 行锁）。
4. 提交后用**新 revision** 写 Redis。Redis `Put` 现有 watermark 语义保证旧 revision 不会覆盖新值（旧请求回填、并发刷新都安全）。
5. 成功后 `refreshed_revision = revision`、`last_error = NULL`；失败则 `attempts+1`、记录 `last_error`，事件照常 ACK。

### 7.3 失败重试

缓存消费者与周期任务扫描 `cache_revisions WHERE refreshed_revision < revision OR last_error IS NOT NULL`，按当前 revision 重新加载并写 Redis，直到成功；因此 **Redis/Put 失败不会丢缓存收敛**，也不会把事件反复送进 DLQ。`revision` 只增不减，重启后 dirty 状态仍在 MySQL。

## 8. 存量切换方案

前提：应用写入口已升级到"无 outbox 写入 + schedule_version"版本，旧 worker 已停止。

1. **记录 CDC 起始位置**：停写窗口内执行 `SHOW MASTER STATUS`，把 `File/Position` 写入 `cdc_cutover_state`（CLI `--record-position`）。
2. **盘点**：`rag-cdc-cutover --inventory` 输出未完成文档任务（按状态/最老年龄）、缓存 key 数、各 key 当前 revision（搜索任务不再盘点；历史 `search_tasks` 行原样保留）。
3. **启动缓存连接器**：初始快照会把业务表现存行全部投递一次 → 缓存消费者递增 revision 并重建，形成"新版本基线"。
4. **启动任务连接器**：初始快照为 `document_jobs` 现存行投递 `op=r`；`rag-cdc-cutover --backfill` 对仍未完成的任务在事务内把 `schedule_version` 递增到基线（例如 `schedule_version = GREATEST(schedule_version, baseline) + 1`），确保每个未完成任务都有一条可消费通知，避免"快照完成前的 UPDATE 丢失"。
5. **核对**：`--verify` 断言所有活动任务 `schedule_version >= baseline+1`；脚本消费两个 topic 统计 ID 集合，与 MySQL 未完成任务集合比对（允许消费侧安全跳过的终态/重复）。
6. **放量**：恢复写入口，继续观察 Debezium 延迟、Kafka 积压、脏缓存数归零。
7. **下线旧表**：确认无旧进程写入后执行 `rag-service-migrate --retire-outbox`（删除 4 张 outbox 表），并更新 `docs/USAGE.md` 的升级步骤。

回滚：保留旧表迁移前的备份；切换窗口内回滚需要同时回退服务/worker 二进制与 Debezium 连接器（offset 文件保留，重新启动会从旧位点继续，消费者幂等）。

## 9. 下线旧机制清单

- 删除写入：`document_job_outbox`、`search_task_outbox`、`cache_outbox`（及历史 `document_task_outbox`）的 INSERT/UPDATE/DELETE。
- 删除轮询/补发：`DocumentJobs.PendingOutbox/MarkPublished/GCOutbox`、`SearchTasks.DispatchPending/RunOnce`、`CacheSynchronizer` 的 relay/flush-pending 队列。
- 删除"Kafka 受理确认"路径与失败回滚（搜索已改为同步直返，不再有提交即受理）。
- 搜索同步化额外删除：`internal/state/search_tasks.go`、`internal/searchworker/`、`cmd/rag-search-worker/`、死信 `--kind search`、搜索 schema-3 envelope、`SearchEngine.GetSearchResult`、`Runtime.SubmitSearch` 与 `/v1/search-results` 路由。
- 保留：任务租约恢复（`Sweep`）、超时处理（`Expire`）、重排（到期重试或租约过期时 `schedule_version+1`）、死信终态落库、DLQ 消费（仅文档/缓存）。`search_tasks`/`search_result_snapshots` 两表保留历史数据、不再读写。

## 10. 监控替换

| 旧指标 | 新指标 |
| --- | --- |
| `pipeline.outbox`（`document_job_outbox` 状态/最老记录） | `pipeline.debezium`（任务/缓存两个连接器健康）+ `pipeline.kafka`（文档/缓存两个业务 topic 的消费组积压） |
| 搜索 outbox 状态 | 已随搜索同步化移除（`queues.search_tasks` 不再存在） |
| 文档 outbox 积压 | `queues.document_jobs`：按状态/阶段计数、最老排队、租约过期数、近期失败数 |
| — | `cache`：脏 key 数（`refreshed_revision < revision`）、刷新失败数（`last_error` 非空）、最近刷新时间 |

Console 监控页字段、`/v1/admin/monitor/overview` 响应、`internal/monitor` 测试同步调整；README/USAGE/DEPLOYMENT 的 outbox 说明全部替换。结果快照计数与搜索 DLQ 指标随同步化一并删除。

## 11. 验收矩阵

| 链路 | 用例 | 断言 |
| --- | --- | --- |
| 文档受理与补发 | 受理后无 outbox 写入；Sweep 对滞留 queued / 租约过期 processing 递增版本；重复投递旧版本 | 每个未完成任务恰有一次可消费通知；旧版本/终态/有效租约被安全跳过；DB 落库后才 ACK |
| 排队搜索与结果读取（已下线） | ~~Submit 返回 result_id / 超时收敛 / Redis 回源~~ | **已随 2026-11-13 搜索同步化删除**；新断言见 §12：`search` 一次返回、无 `result_id`、artifact 独立读取 |
| 手工 SQL → Redis 收敛 | 直接 `UPDATE/DELETE/同名重建` 业务表 → Debezium 事件 → 消费者 bump revision + 重建 | Redis 最终等于 MySQL 脱敏视图；旧回填被 watermark 拒绝 |
| 故障演练 | Debezium 暂停/恢复、Kafka 重复投递、worker 领取后崩溃、Redis 写入失败 | 任务不丢、终态一致、缓存最终恢复、无死信风暴 |

测试以 Go 单测/集成测试（有 `RAG_TEST_MYSQL_URL` 时）覆盖；实机链路用 `scripts/verify-*.py` 与 Compose 验证，未实机验证项明确记录在 `VALIDATION.md`。

- `internal/cdcacceptance/`：按本表逐条实现的验收/混沌套件（文档受理与补发、手工 SQL 后缓存收敛、Debezium 暂停/重复投递/领取后崩溃/Redis 写失败）。原“搜索提交即受理与 MySQL 回源”用例已随排队搜索删除，改为 `internal/runtime` 与 `internal/httpapi` 的同步搜索/超时/取消用例。
- `internal/{documentworker,cacheconsumer}`：消费者级幂等、版本、租约、ACK/DLQ 规则（`searchworker` 已删除）。
- `internal/{state,mq,monitor,migrate,runtime,e2e}`：表结构/迁移、事件解码、监控契约、切换 CLI 端到端。
- `internal/searchrefs`：artifact 注册、版本固定、分块读取与到期语义（原结果快照用例已删除）。

## 12. 搜索同步化迁移入口

本节是搜索同步化（2026-11-13）的契约入口，取代原先的“提交即受理 + 结果快照”链路。

### 12.1 契约变化

- MCP：工具固定 5 个，`search` 无 `wait_seconds`；`get_search_result` 删除；受限 scope 改为同步检索 + `DocumentIDs` 过滤。
- REST：`POST /v1/search` 同步返回；`GET /v1/search-results/{result_id}` 删除；响应去掉 `result_id`/`created_at`/`expires_at`/`snapshot_available`/`snapshot_error`。
- 存储：`search_tasks`、`search_result_snapshots` 保留历史数据、不再读写、不 DROP；artifact 以 `artifact_snapshots` 为事实来源，TTL 沿用 `RAG_RESULT_TTL_SECONDS`。
- 消息：搜索 schema-3 通知、`praxis_search` topic/group、搜索 DLQ、`rag-search-worker`、死信 `--kind search` 全部删除；环境变量 `RAG_SEARCH_*`（除 `RAG_SEARCH_TIMEOUT_SECONDS`）保留为废弃键、启动忽略。

### 12.2 在途任务与切换

1. 升级前 `SELECT status, COUNT(*) FROM search_tasks GROUP BY status` 记录存量；未完成任务不会再被消费，直接作为历史保留（必要时维护窗口内 SQL 收敛为 `timed_out`，不要 DROP/清空）。
2. 停搜索入口 → 停 `rag-search-worker` 与 `--kind search` 死信 → 等 `praxis_search` lag 归零 → 可删除搜索 topic（先备份消息）；**不要**删除两张历史表。
3. `rag-state-migrate --apply`（不再给 `search_tasks` 追加 `schedule_version`）；部署新版 `rag-service`，从进程管理器与 Debezium 任务连接器 `column.include.list` 中移除搜索表。
4. 验证：`tools/list` 5 个工具、`POST /v1/search` 一次返回、`/v1/search-results/<uuid>` 返回 404、受限账号按 scope 过滤、监控页无搜索队列/快照指标。
5. 完整步骤、客户端调整与回滚见 [部署指南 §12](DEPLOYMENT.md#12-搜索同步化迁移与切换)。

### 12.3 回滚

回退 `rag-service`/`rag-search-worker` 二进制与任务连接器配置（恢复 `search_tasks` 采集与 `praxis_search`），重启搜索 worker 与 `--kind search` 死信。两张历史表未删除、未清空，旧行仍可被旧 worker 按 `schedule_version`/状态收敛；若已删除搜索 topic，需重建并依赖初始快照 `op=r` 补投。artifact 与 TTL 语义前后一致，回滚不影响已签发引用。回滚后在监控页核对 `pipeline.kafka.groups` 组数，避免新旧混跑。
