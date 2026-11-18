# Redis 持久读模型缓存
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。

> **传输层更新（2026-11-09）**：旧 RocketMQ 传输已全部替换为 Kafka；本文的队列/topic 示例按当前 `RAG_KAFKA_*` 与显式 `RAG_*_DLQ_TOPIC` 契约阅读，历史验证命令仅作记录。
> **搜索同步化（2026-11-13）**：搜索已改为一次调用同步返回证据，不再有搜索任务、结果快照、搜索 topic/worker/DLQ，也不再使用 Redis 短期搜索状态。`search_tasks`/`search_result_snapshots` 表保留历史数据、不再读写。本文只描述**持久读模型展示缓存**；artifact 引用见 [UUID 引用](result-references.md)。


## 范围

Redis 主要承担**持久数据展示缓存**与**文档长租约**（Redis 是唯一锁后端，旧 `RAG_DOCUMENT_LOCK_MODE` 开关已移除、残留值被忽略），两者共用同一 Redis 实例，但键空间、开关与 TTL 完全独立：

- **异步文档入库的任务状态与文件字节保存在 MySQL**（`document_jobs`、`blobs`）：没有 Redis TTL，不会因 Redis 丢失、过期或不可用而失效；进度只由 Debezium 任务连接器 → `rag-document-worker` 推进。
- **持久数据展示缓存**：MySQL 继续保存异步文档任务、已发布文档/源文件/解析产物、KB/API Key/catalog、artifact 快照和 UploadJournal 操作/租约；Qdrant 继续检索。下面第一、二批读模型在 Redis 故障时按「持久读模型的读写与可靠同步」的故障行为处理（熔断 + 回源限流 + 跨实例回源锁，均回源 MySQL）；缓存刷新由 Debezium 表级事件驱动 revision 水位，`cache_outbox` 已于 2026-11-13 下线（下文的 outbox 描述仅作历史）。

第一批缓存四种读模型（MCP 工具和提示词共用一份 catalog）：

| 缓存 kind | scope | 内容 |
| --- | --- | --- |
| `kb_public` | namespace | 知识库公开配置；provider 仅 URL 和 api_key_set |
| `catalog` | namespace | MCP 工具启停/说明、提示词模板/参数 |
| `documents` | namespace | 文档列表、大小、片段数、索引时间、诊断元数据 |
| `kb_list` | 全局空 scope | 管理端知识库列表、配置标志、文档数量 |

第二批按确认的严格模式，增加以下**状态查询缓存**。正常命中不查 MySQL；不改变权威判定：

| 缓存 kind | scope | 内容 |
| --- | --- | --- |
| `heads` | namespace | 已发布 document_id → version 的展示快照 |
| `leases` | namespace | 资源、holder_id、operation_id、expires_at；读取时重新计算 expired |
| `upload_list` | namespace | 最近 100 条操作状态摘要，不用于恢复扫描 |
| `upload_status` | operation_id | namespace、操作阶段、错误类型、步骤/回滚状态；最多 256 个步骤 |

**严格边界**：真实 API Key 鉴权、密钥导出、检索可见版本与提交/回滚校验仍直接访问 MySQL。Redis 是文档长租约的唯一后端：长租约的申请/续期/认领在 Redis 原子完成，但 **fencing、持有者登记与提交/回滚校验仍以 MySQL 为准**，锁键不进入任何读模型，缓存中的 enabled、版本、owner 展示值均不能授予执行权。即使缓存故意置旧，也不能导致已停用 Key 被放行或过期节点成功提交。

租约 owner 原始所有权 token 不放 Redis；holder_id 是 token 的短哈希，仅用于区分执行者。操作详情不缓存 idempotency_key、workspace、fingerprint、result 原文、步骤输入 details、文件内容或密钥。upload_status 的返回值再次验证 namespace，UUID 不授予访问权。API Key 列表直接读取 MySQL，按账号可见（管理员/本机管理为全部，普通账号仅本人；见 [权限管理](PERMISSIONS.md) §9）。

上述**展示读模型**不缓存原始文件、图片、解析产物、查询/重排结果或模型密钥。搜索响应不写入 Redis：检索结果在请求内同步返回，artifact 事实在 MySQL（见 [产物引用](result-references.md)）；文档原本文件不写入 Redis，只保留在 MySQL 的 `document_jobs` 与 `blobs` 中。事务/步骤的展示投影可缓存，MySQL 原始 UploadJournal 始终是恢复依据；不迁移发布/恢复日志或租约的最终判定权威到 Redis（启用文档锁 Redis 后端时只有租约本身在 Redis，fencing 与提交校验仍在 MySQL）。文档列表中的 operation_id 也仅为展示元数据。

公开配置和列表命中 Redis 时直接返回，不额外查 MySQL 版本，也不构建 tenant 或调用模型。上传、去重、覆盖/删除检查、reembedding、鉴权、版本发布与恢复仍使用 MySQL 权威读取。工具列表允许缓存，但真正执行工具时直接校验 MySQL 的 enabled 状态，避免停用被旧缓存绕过。

## 搜索不再使用 Redis 短期状态

搜索已改为**同步直返**：`POST /v1/search` 与 MCP `search` 一次调用完成检索 + 重排并直接返回证据，没有任务 ID、`wait_seconds`、轮询、结果快照或 Redis 短期缓存。原 `search_tasks`/`search_result_snapshots` 表保留历史数据、不再读写；搜索 topic/消费组/DLQ 与 `rag-search-worker` 已下线（见 [部署指南 §12](DEPLOYMENT.md#12-搜索同步化迁移与切换)）。**文档入库的受理与展示不依赖 Redis**：`add_document` 只提交一个 MySQL 事务（`document_jobs` 完整任务），Kafka 只传 schema 4 纯通知；后续解析/发布阶段才依赖锁后端（Redis 是必需依赖）。

文档侧的**入库任务**状态、领取、心跳、重试与终态全部由 MySQL 提供（`document_jobs`），worker 用 SQL 租约 claim 并心跳续租，只有 MySQL 记录领取/终态后才 ACK；`get_document_status` 直接读 MySQL，无 TTL。**文档长租约**由 Redis 原子获取/续租（Redis 是唯一后端，`RAG_DOCUMENT_LOCK_MODE` 已移除），MySQL 只保留持有者登记与单调递增的 `fencing_epoch`（见下文「文档锁」）。UploadJournal 继续保障文档发布版本一致性，不存在 Redis+MySQL 原子事务问题，也没有 Redis 回源。事务 outbox 已于 2026-11-13 下线：重发只通过 `schedule_version + 1`（到期重试或文档任务租约过期时由 `Sweep` 执行），不再有 outbox 行或保留期清理。日志仅记录任务/操作 ID 与安全错误类别，不记录查询、文件/消息正文或凭据。

2026-11-13 搜索同步化：搜索不再有 Kafka 通知（原 schema 3 搜索消息、`praxis_search` topic、搜索消费组与搜索 DLQ 一并下线），文档通知仍为 schema 4。切换步骤见 [部署指南 §12](DEPLOYMENT.md#12-搜索同步化迁移与切换)；`search_tasks`/`search_result_snapshots` 保留历史数据、不再读写，artifact 保留边界见 [产物引用](result-references.md)。旧 outbox 表仍可由 `rag-service-migrate --retire-outbox --yes` 删除；`document_tasks` 作为历史表保留但不再读写。

## 文档锁（Redis 唯一后端）

文档写入的长时互斥（上传 INTENT、覆盖发布、删除、恢复、清理与知识库独占操作）由 Redis 租约控制（Redis 是唯一后端，旧 `RAG_DOCUMENT_LOCK_MODE` 开关已移除、残留值被忽略）；MySQL 只保存持有者登记与单调递增的 `fencing_epoch`，提交/回滚仍由 MySQL 事务校验持有者。入库任务领取、任务心跳、重试、终态与 Kafka 去重不受影响，仍在 `document_jobs`。

- **与展示缓存解耦**：`RAG_REDIS_ENABLED=false` 只关闭读模型展示缓存，文档锁仍用 Redis；缓存 TTL（`RAG_REDIS_TTL_SECONDS`）与锁 TTL（`RAG_REDIS_LOCK_TTL_SECONDS`，默认 120，未显式设置时跟随 `RAG_UPLOAD_LEASE_SECONDS`）是两套配置。锁配置是部署级的：`rag-service`、`rag-document-worker`、`rag-mcp-ingest` 必须使用同一 Redis 实例、逻辑库、`RAG_REDIS_LOCK_URL`（为空回退 `RAG_REDIS_URL`）、前缀与 TTL；缺失锁地址时这些进程启动即报配置错误。
- **键空间**：每个 namespace 一个 Redis Hash `<prefix>:{<sha256(namespace) 前 32 位>}:leases`（`RAG_REDIS_LOCK_PREFIX` 默认 `rag:lock:v1`），field 为资源（`document:<hash>`、`name:<hash>`、`namespace:exclusive`），单键 + hash tag，天然兼容 Redis Cluster。每次获取是一条 Lua 脚本，原子且全有或全无；时间基准用 Redis `TIME`；释放只删除本次 owner token 的 field；owner token 不出现在接口、日志或展示里（展示用 sha256 前 16 位）。
- **失败语义**：Redis 冲突保持原有 409「资源被占用」；Redis 暂时不可用（超时/连接失败/未配置后端）返回**可重试**错误（REST 文案「文档锁暂不可用，请稍后使用相同的幂等键重试」，异步受理为 5xx），**禁止单实例回退到 MySQL 锁**；续租失去所有权或无法确认时停止启动新步骤，并通过可取消的 context 通知在途解析/嵌入/Qdrant 调用；提交成功但释放锁失败时不重复执行已提交操作，最多等到租约 TTL 过期；Redis 重启丢失租约后该实例在下一个步骤边界停止推进，由恢复程序接管。
- **恢复扫描**：只有 Redis 租约已释放（过期）才允许接管，MySQL 登记行的 `expires_at` 只是登记/展示兜底，不再随心跳续写，也不会仅凭它接管仍在运行的上传。
- **展示**：`GET /v1/admin/upload-leases` 字段不变；`redis` 模式下 `expires_at`/`expired` 按锁后端实时 TTL 计算，Redis 不可用时回退 MySQL 登记值。释放后的行保留代次但 `owner` 为空，展示只列有持有者的行；`leases` 读模型与 CDC 不需要新字段（`fencing_epoch` 只参与 MySQL 校验）。切换/回退见 [部署指南](DEPLOYMENT.md) §13。

## 持久读模型的读写与可靠同步

```text
读取：Redis 命中 → 返回；未命中 → 按 kind+scope 争 Redis 回源锁（SET NX PX + 唯一 token）
      → 拿到锁先复读 Redis（命中即返回）→ 仍未命中查 MySQL 一致快照 → 带修订号回填 → 释放锁
      → 未拿到锁的请求短暂退避，每次醒来先复读 Redis；命中直接返回，仍不命中再争锁
写入：MySQL 同事务更新业务数据 + cache_revisions（revision+1、updated_at）
      → Debezium 缓存连接器采集 binlog 投递 Kafka（只含定位列）
      → rag-cache-worker 与读取共用同一把回源锁，事务内 revision+1、重读脱敏视图、提交后写 Redis
      → 失败记录 attempts/last_error 并 ACK；周期 RefreshDirty 继续重试
```

- 数据和版本用一个 Redis Lua 原子写入；版本使用十进制字符串比较，避免 BIGINT 超出 Lua 浮点精度。
- 每种 kind/scope 独立递增修订号；旧消息和旧回填不能覆盖新值。
- **Redis payload TTL 随机化**：每次写入在 `[T - floor(T/5), T]` 内随机取整秒（`RAG_REDIS_TTL_SECONDS` 为上限 T，默认 60 → **48～60 秒**），错开同一 key 的同步过期，配合回源锁抑制惊群；`revision` 水位 key 保持无过期时间，防止 payload 过期后旧回填复活已删除数据。相同修订号不能覆盖删除标记。
- **按缓存键加 Redis 回源锁**：锁粒度为 `kind + scope`（同一知识库的文档列表共用一把锁，不同知识库可并行回源），`SET NX PX` 原子加锁；每次持锁使用唯一随机 token，续租与释放都校验 token，旧持有者无法管理新持有者的锁。持有者查询期间按 TTL/2 续租；失去租约（过期或被接管）即取消查询并放弃回填。
- **读取流程**：请求先读 Redis，未命中才争锁；拿到锁后**再次读 Redis**（双重检查），仍未命中才查 MySQL，随后按现有版本规则回填并释放锁。未拿到锁的请求短暂退避（默认 25ms）并总先复读 Redis；等待有界（默认合计一个 TTL，5 秒），超时返回**可重试**错误，不绕过锁再查一次。
- **故障行为（缓存启用时）**：Redis 故障沿既有契约**直接回源 MySQL**——展示缓存熔断器（`RAG_REDIS_BREAKER_THRESHOLD` 次连续可用性故障后跳过 Redis 读写，冷却 `RAG_REDIS_BREAKER_COOLDOWN_SECONDS` 后放单个探测）限制每个请求的超时代价；回源并发受 `RAG_CACHE_FALLBACK_CONCURRENCY` 限额（满额立即 503「服务繁忙，请稍后重试」）。**锁后端故障同样回源**（锁不可用时保护已失效，直接查 MySQL 并跳过回填，发布由脏键扫描补偿）；只有「锁被别的实例长期占用且等待超时/租约丢失」返回可重试错误。Redis Put 失败不失败读取（读取已从 MySQL 返回，发布由脏键扫描补偿）。缓存禁用（`RAG_REDIS_ENABLED=false`）时行为不变：读取直接回源 MySQL。
- **后台刷新共用回源锁**：`RefreshKey` 与 `GetOrLoad` 使用同一把 `kind + scope` 锁。后台按目标 `revision` 判断刷新是否完成：锁被占用且目标版本尚未发布时，保留待刷新状态（`refreshed_revision` 仍落后），由 `RefreshDirty` 补偿完成目标版本发布。
- MySQL 修订行是快照锁：写方先获取修订行再改业务数据；缓存未命中和消息刷新读取同一提交快照的数据与修订号。
- 同步消息是 at-least-once，重复、乱序消息允许；消费者读最新数据库快照，不回放消息中的陈旧数据。
- Redis、Kafka 都故障或进程中断时，脏状态仍保留在 MySQL `cache_revisions`（revision/refreshed_revision/last_error）；`rag-cache-worker` 周期 `RefreshDirty` 会继续重试直到成功。缓存失败不进入 DLQ，也不会回滚已提交的业务写入；需监控 dirty/failures 直到归零。
- 同一 kind/scope 的积压刷新会合并到最新修订；成功刷新后 `refreshed_revision = revision`、`last_error = NULL`。
- **一致性边界**：Redis 可读但刷新失败时可能暂时返回旧值，补偿/TTL 后收敛；这是最终一致性，不是跨库原子提交。回源锁防止的是“同一 key 的重复 MySQL 回填与旧持有者回填”，不是跨库原子提交。
- 新增/更新/删除知识库刷新公开配置、文档展示及全局列表；文档发布/删除刷新文档与全局列表；MCP 修改刷新 catalog。
- 上传未发布时，公开文档和发布指针保持旧版本，但 RUNNING/FAILED/ROLLING_BACK 等进度缓存照常提交。最终发布的 heads 和 COMMITTED/步骤状态事件，与业务记录在同一 MySQL 事务中提交；发布回滚不会留下这些未提交事件。补偿完成则另行提交 ROLLED_BACK 的状态事件。
- 续租只刷新 leases，不制造无关步骤状态；租约过期是时间事件，展示端每次用 `expires_at`（`redis` 模式下按锁后端实时 TTL）重新计算 `expired`，不必等待缓存 TTL。
- 提交/删除等由外部事务持有的 journal 方法只递增 revision，不在事务内访问 Redis/Kafka；由调用方提交后刷新。真实状态查询、恢复扫描和 fencing 不追加缓存事件。直接用 SQL 修改业务表（含删除与同名重建）也会产生 CDC 事件并被同一条映射收敛。

## 第二批状态查询接口

全部使用现有鉴权和 namespace 检查，不增加 MCP 工具，也不改变 UUID 搜索结果协议：

- `GET /v1/admin/api-keys`：管理列表从 MySQL 直接读取。
- `GET /v1/uploads`：当前 namespace 最近 100 条操作摘要。
- `GET /v1/uploads/{operation_id}`：当前 namespace 的操作/步骤/回滚状态；跨 namespace 或不存在返回 404。
- `GET /v1/admin/published-versions`：namespace 的已发布版本展示，额外要求本机直连管理权限。
- `GET /v1/admin/upload-leases`：namespace 的租约/执行者展示，额外要求本机直连管理权限；字段不变，`redis` 模式下 `expires_at`/`expired` 取锁后端实时 TTL，锁后端不可用时回退 MySQL 登记值，`holder_id` 仍是 owner token 的短哈希。

新增状态响应带 `display_only: true` 和 `Cache-Control: no-store`，明确状态可能最终一致，不用于授权、决定写入或自动回滚。调用方应使用操作 ID 查询进度，而不是依据列表推断所有未完成任务。未提供独立前端进度页面。

## 配置

当前 Go 实现使用 kafka-go 客户端；宿主进程连接宿主机 listener，compose 容器使用内部 listener。

环境变量（可放入服务配置中的非敏感项）：

```text
RAG_REDIS_URL=redis://127.0.0.1:6379/0
RAG_REDIS_ENABLED=true
RAG_REDIS_PREFIX=rag:state:v1
# 随机 TTL 上限 T：每次写入取 [T-floor(T/5), T] 内随机整秒（默认 60 → 48~60 秒）
RAG_REDIS_TTL_SECONDS=60
RAG_REDIS_TIMEOUT_SECONDS=0.3
RAG_KAFKA_BOOTSTRAP_SERVERS=127.0.0.1:9092
RAG_KAFKA_CLIENT_ID=praxis-rag
RAG_KAFKA_SECURITY_PROTOCOL=PLAINTEXT
# RAG_KAFKA_SASL_MECHANISM / RAG_KAFKA_SASL_USERNAME / RAG_KAFKA_SASL_PASSWORD
# RAG_KAFKA_TLS_CA / RAG_KAFKA_TLS_INSECURE
RAG_KAFKA_PUBLISH_TIMEOUT_SECONDS=10
RAG_CACHE_TOPIC=praxis_state_cache_refresh
RAG_CACHE_GROUP=praxis_state_cache_refresh_workers
RAG_DOCUMENT_TOPIC=praxis_document_ingest
RAG_DOCUMENT_GROUP=praxis_document_ingest_workers
RAG_DOCUMENT_DLQ_TOPIC=praxis_document_ingest_dead_letters
RAG_CACHE_DLQ_TOPIC=praxis_state_cache_refresh_dead_letters
RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters
RAG_CACHE_DLQ_GROUP=praxis_state_cache_refresh_dead_letters
RAG_CACHE_WORKER_RECOVER_SECONDS=30
# 分布式 Kafka Connect（宿主机进程读取 connector/task 状态并自动重启 FAILED task）
RAG_CONNECT_URLS=http://127.0.0.1:8083,http://127.0.0.1:8084
RAG_CONNECT_DOCUMENT_CONNECTOR=praxis-document-cdc
RAG_CONNECT_CACHE_CONNECTOR=praxis-cache-cdc
```

`RAG_REDIS_TTL_SECONDS=60` 只用于展示缓存，且为**随机 TTL 的上限**：每次写入取 `[T - floor(T/5), T]`（默认 48～60 秒）内的随机整秒，错开同步过期；artifact 保留期用 `RAG_RESULT_TTL_SECONDS=3600`（变量名保留，语义已更正为 artifact TTL）。`RAG_REDIS_ENABLED=false` 只禁用读模型展示缓存（读取回源 MySQL，不涉及回源锁），**不禁用文档锁**：**文档锁与展示缓存解耦，Redis 是文档锁的必需依赖**——`RAG_REDIS_LOCK_URL`（为空回退 `RAG_REDIS_URL`）必须能解析，且所有参与文档写入/恢复/清理的进程必须使用同一 Redis 实例、逻辑库与前缀；缺失锁地址时进程启动报配置错误。文档异步受理与状态查询不依赖 Redis 锁（受理只写 MySQL，解析/发布阶段才取锁）。多实例共享相同 Redis、前缀、MySQL 和各自的 cache/document topic/group（以及两个显式 DLQ topic）。不同 MySQL 数据库使用不同 Redis 前缀，防止隔离域串数据。Redis 密码可放 URL（勿打印或提交）；Kafka SASL/TLS 凭据仅从 `RAG_KAFKA_SASL_*` / `RAG_KAFKA_TLS_*` 读取。

## Docker 与启动

Compose 提供 Redis 7.4、单节点 KRaft Kafka（`apache/kafka:3.9.0`，宿主 `127.0.0.1:9092` / 容器 `kafka:9092`，4 partitions、自动创建 topic、`kafka_data` 持久卷）以及默认启动的**分布式 Kafka Connect 集群**（`connect-1`/`connect-2`；`connect-init` 一次性注册 `praxis-document-cdc` 与 `praxis-cache-cdc`）。连接器配置、binlog 位点与 task 状态保存在 Kafka 内部 topic，schema history 每连接器独立。Redis 开启 AOF 和 noeviction；Kafka 数据写入 `KAFKA_LOG_DIRS=/var/lib/kafka/data`。不再有旧 RocketMQ 组件或存储卷 UID 初始化。

```powershell
.\scripts\start-stack.ps1 -MigrateOnly -SkipMigrate
.\scripts\start-stack.ps1 -SkipMigrate
```

脚本启动 MySQL、Kafka、两个 Debezium 连接器等依赖，创建 document/cache 与两个 DLQ topic（4 partitions、RF=1），然后运行服务。两类通知的 Kafka 投递由两个 Debezium 连接器承担；应用不写事务 outbox。搜索不经过 Kafka。

```powershell
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 `
  --create --if-not-exists --topic praxis_state_cache_refresh --partitions 4 --replication-factor 1
```

对持久读模型刷新，Redis/Kafka 失败不回滚已提交的 MySQL 业务操作：revision 已递增，脏状态保留在 `cache_revisions`，由 `rag-cache-worker` 周期重试。搜索为同步执行，不写 Kafka/Redis；对异步文档新增，MySQL 提交即受理，Kafka 故障既不回滚也不阻塞受理。启动前确认 MySQL、Redis（可选）、Kafka 连接、topic/group 与两个连接器权限。

## 运维与限制

`cache_revisions` 已扩展为 revision 水位（`revision`、`refreshed_revision`、`attempts`、`last_error`、`updated_at`），正常自动 schema 会幂等追加缺失列；`cache_outbox` 已下线并由迁移 CLI 删除。禁用自动 schema 的部署需先使用有 DDL 权限的账号执行同样的增量迁移。

展示缓存水位不独立淘汰；内存用量随缓存 kind/scope 增长。不要删除水位却保留 payload，或在旧 worker 仍回填时 FLUSHDB。清空/换库前先停入口并排空 worker，再停所有实例；新前缀可从 MySQL 重建**展示缓存**；异步文档任务与文件字节在 MySQL，仍可查询并继续投递；搜索无持久缓存，重新检索即可。Redis 持续不可用时脏状态保留在 `cache_revisions`，`RefreshDirty` 会持续重试；不要清理 revision 水位行。

本批不实现跨实例 MCP 订阅通知总线；配置缓存同步不等于通知可重放。artifact 快照保存在 MySQL，按 ID 独立读取（见 [产物引用](result-references.md)）；`search_tasks`/`search_result_snapshots` 为历史表、不再读写。文档任务状态使用 MySQL `document_jobs`，不进入 Redis 短期存储。

## 验证

```powershell
go test -count=1 ./internal/state/ ./internal/redisstate/...
```

历史 Python 测试已随 Python 后端删除；当前 Go 状态层的缓存/Redis 测试使用上述命令。2026-11-13 的缓存消费者改造（Debezium 缓存连接器 + revision 水位 + MarkAllDirty/RefreshDirty）以 `internal/state` 与 `internal/cacheconsumer` 单测覆盖；搜索结果快照/搜索任务相关用例已随同步化改造删除。2026-11-18 的「展示缓存熔断与回源限流」（`cache_guard.go`）以 `internal/state` 的 `cache_backpressure_test.go`/`cache_guard_test.go` 与 `internal/e2e` 回归覆盖；同日的**随机 TTL + 跨实例回源锁**改造（随机 TTL 48～60 秒、按 `kind+scope` 加锁、双重检查读取、等待者退避复读/超时可重试、锁故障回源 MySQL、`RefreshKey` 共用锁并按目标 revision 收敛）由 `internal/state` 的回源锁行为测试与 `internal/redisstate` 的锁/TTL 测试覆盖。文档锁的 Redis 后端另由 `internal/state`（冲突矩阵、迟到登记、接管围栏、Redis 丢失/不可用/释放失败、租约展示）、`internal/runtime`/`internal/documentworker` 回归与 `internal/redisstate`（两个独立客户端对真实 Redis 的 Lua/续租/TTL 集成测试）覆盖；Redis Cluster/Sentinel 与 `-race` 压测未验证。真实 Redis/SASL/TLS 集群与双连接器实链路未验证，见 [VALIDATION](../VALIDATION.md)。
