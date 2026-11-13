# Redis 短期搜索状态与持久读模型缓存
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。

> **传输层更新（2026-11-09）**：旧 RocketMQ 传输已全部替换为 Kafka；本文的队列/topic 示例按当前 `RAG_KAFKA_*` 与显式 `RAG_*_DLQ_TOPIC` 契约阅读，历史验证命令仅作记录。


## 范围

Redis 有两种不同职责，不能混用其故障语义：

- **短期搜索状态（2026-11-13 起以 MySQL 为事实来源）**：排队搜索的任务状态与结果快照保存在 MySQL（`search_tasks`、`search_result_snapshots`）。Redis 只是读取缓存：未命中/不可用时回源 MySQL 并尽力回填；超过 `RAG_RESULT_TTL_SECONDS=3600` 后 MySQL 快照会被有界清理。**异步文档入库的任务状态与文件字节也保存在 MySQL**：没有 Redis TTL，不会因 Redis 丢失、过期或不可用而失效。
- **持久数据展示缓存**：MySQL 继续保存异步文档任务、已发布文档/源文件/解析产物、KB/API Key/catalog、artifact 快照和 UploadJournal 操作/租约；Qdrant 继续检索。下面第一、二批读模型仍可在 Redis 故障时回源 MySQL；缓存刷新由 Debezium 表级事件驱动 revision 水位，`cache_outbox` 已于 2026-11-13 下线（下文的 outbox 描述仅作历史）。

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
| `api_keys` | 管理端全局空 scope | Key 名称、前缀、授权 namespace、enabled、exportable；无 key/digest/密文 |
| `heads` | namespace | 已发布 document_id → version 的展示快照 |
| `leases` | namespace | 资源、holder_id、operation_id、expires_at；读取时重新计算 expired |
| `upload_list` | namespace | 最近 100 条操作状态摘要，不用于恢复扫描 |
| `upload_status` | operation_id | namespace、操作阶段、错误类型、步骤/回滚状态；最多 256 个步骤 |

**严格边界**：真实 API Key 鉴权、密钥导出、检索可见版本、租约申请/续期/认领、fencing、提交与回滚仍直接访问 MySQL。缓存中的 enabled、版本、owner 展示值均不能授予执行权。即使缓存故意置旧，也不能导致已停用 Key 被放行或过期节点成功提交。

租约 owner 原始所有权 token 不放 Redis；holder_id 是 token 的短哈希，仅用于区分执行者。操作详情不缓存 idempotency_key、workspace、fingerprint、result 原文、步骤输入 details、文件内容或密钥。upload_status 的返回值再次验证 namespace，UUID 不授予访问权。API Key 列表按账号可见（管理员/本机管理为全部，普通账号仅本人；见 [权限管理](PERMISSIONS.md) §9）。

上述**展示读模型**不缓存原始文件、图片、解析产物、查询/重排结果或模型密钥。排队搜索任务与结果快照以 MySQL 为事实来源（见下节），Redis 只作读取缓存：未命中/不可用时回源 MySQL，清空 Redis 不需要客户端重新提交；文档原本文件不写入 Redis，只保留在 MySQL 的 `document_jobs` 与 `blobs` 中。事务/步骤的展示投影可缓存，MySQL 原始 UploadJournal 始终是恢复依据；不迁移发布/恢复日志或业务租约权威到 Redis。文档列表中的 operation_id 也仅为展示元数据。

公开配置和列表命中 Redis 时直接返回，不额外查 MySQL 版本，也不构建 tenant 或调用模型。上传、去重、覆盖/删除检查、reembedding、鉴权、版本发布与恢复仍使用 MySQL 权威读取。工具列表允许缓存，但真正执行工具时直接校验 MySQL 的 enabled 状态，避免停用被旧缓存绕过。

## MCP 短期搜索任务与结果快照

搜索受理已改为**提交即受理**：`SearchTasks.Submit` 只向 MySQL `search_tasks` 写一个事务并返回 `result_id`，不再等待 Kafka 确认；通知由 Debezium 任务连接器采集任务表后投递。`wait_seconds` 只是客户端等待结果的时间。**文档入库同样不依赖 Redis**：`add_document` 只提交一个 MySQL 事务（`document_jobs` 完整任务），Kafka 只传 schema 4 纯通知。

搜索不再有“Kafka/Redis 发送确认”前置依赖：MySQL 提交即受理，Kafka/Debezium 暂停时任务保持 `queued`，恢复后补投；逾期由 `Expire`/`Claim`/`Get` 收敛为 `timed_out`。重试应保留原 idempotency_key、内容和参数。Redis 丢失/淘汰只影响读取性能，结果可从 MySQL `search_result_snapshots` 回源重建；超过 `RAG_RESULT_TTL_SECONDS` 后 MySQL 快照被有界清理，`result_id` 才不可用，不自动重新搜索来伪造原快照。

结果快照以 MySQL `search_result_snapshots` 为事实来源，Console 监控页的「当前结果快照」直接统计 MySQL 未过期快照数（`MonitorStats.SearchResultSnapshotsActive`），统计失败时整个 `library` 为 `null`、页面显示「未知」。Redis 只是读取缓存：未命中或不可用时按 `result_id` 回源 MySQL 并尽力回填，不影响该计数；旧的 Redis ZSET 索引已随本次改造下线。

搜索由独立 worker、独立 topic/group 消费；缓存刷新使用第三组独立 topic/group，文档任务使用文档 topic/group；三类事件均由 Debezium 从 MySQL 表级采集（应用不写 outbox）。Kafka 消费语义为 ack=提交 offset、nack=同一进程内指数退避重试同一条记录、达到 `MaxAttempts` 后写入显式 `RAG_*_DLQ_TOPIC` 并提交、`MaxAttempts=0` 无限重试（至少一次）。**搜索临时故障不提交；成功时在同一 MySQL 事务写 `search_result_snapshots` 并把任务置 `succeeded`，提交后才提交 offset**；重复消息按 `schedule_version` 校验，旧版本/终态安全跳过，不覆盖仍存在的终态。Redis 丢失后可从 MySQL 重建，不依赖 Redis 去重。

文档侧没有 Redis 参与：任务状态、租约与终态全部由 MySQL 提供，worker 用 SQL 租约 claim 并心跳续租，只有 MySQL 记录领取/终态后才 ACK；`get_document_status` 直接读 MySQL，无 TTL。UploadJournal 继续保障文档发布版本一致性，不存在 Redis+MySQL 原子事务问题，也没有 Redis 回源。事务 outbox 已于 2026-11-13 下线：重发只通过 `schedule_version + 1`，不再有 outbox 行或保留期清理。日志仅记录任务/操作 ID 与安全错误类别，不记录查询、文件/消息正文或凭据。

2026-11-13 切换：停旧入口并排空旧 worker/消息，API 与 worker 一起部署；文档通知为 schema 4、搜索通知为 schema 3，旧 schema 不兼容。旧 outbox 表在切换完成前保留，确认无需回滚后执行 `rag-service-migrate --retire-outbox --yes` 删除；`document_tasks` 作为历史表保留但不再读写。结果与 MySQL artifact 保留边界见 [UUID 引用](result-references.md)，切换步骤见 [CDC 迁移契约](cdc-migration.md) §8。

## 持久读模型的读写与可靠同步

```text
读取：Redis 命中 → 返回；未命中 → MySQL 一致快照 → 带修订号回填
写入：MySQL 同事务更新业务数据 + cache_revisions（revision+1、updated_at）
      → Debezium 缓存连接器采集 binlog 投递 Kafka（只含定位列）
      → rag-cache-worker 事务内 revision+1、重读脱敏视图、提交后写 Redis
      → 失败记录 attempts/last_error 并 ACK；周期 RefreshDirty 继续重试
```

- 数据和版本用一个 Redis Lua 原子写入；版本使用十进制字符串比较，避免 BIGINT 超出 Lua 浮点精度。
- 每种 kind/scope 独立递增修订号；旧消息和旧回填不能覆盖新值。
- MySQL 修订行是快照锁：写方先获取修订行再改业务数据；缓存未命中和消息刷新读取同一提交快照的数据与修订号。
- Redis payload 默认 TTL 60 秒；另一个持久水位 key 保存版本/删除标记，防止 payload 过期后旧回填复活已删除数据。相同修订号不能覆盖删除标记。
- 同步消息是 at-least-once，重复、乱序消息允许；消费者读最新数据库快照，不回放消息中的陈旧数据。
- Redis、Kafka 都故障或进程中断时，脏状态仍保留在 MySQL `cache_revisions`（revision/refreshed_revision/last_error）；`rag-cache-worker` 周期 `RefreshDirty` 会继续重试直到成功。缓存失败不进入 DLQ，也不会回滚已提交的业务写入；需监控 dirty/failures 直到归零。
- 同一 kind/scope 的积压刷新会合并到最新修订；成功刷新后 `refreshed_revision = revision`、`last_error = NULL`。
- Redis 不可读时直接回源 MySQL。Redis 可读但刷新失败时可能暂时返回旧值，补偿/TTL 后收敛；这是最终一致性，不是跨库原子提交。
- 新增/更新/删除知识库刷新公开配置、文档展示及全局列表；文档发布/删除刷新文档与全局列表；MCP 修改刷新 catalog。
- 上传未发布时，公开文档和发布指针保持旧版本，但 RUNNING/FAILED/ROLLING_BACK 等进度缓存照常提交。最终发布的 heads 和 COMMITTED/步骤状态事件，与业务记录在同一 MySQL 事务中提交；发布回滚不会留下这些未提交事件。补偿完成则另行提交 ROLLED_BACK 的状态事件。
- API Key 创建、修改、删除/导入刷新管理列表。续租只刷新 leases，不制造无关步骤状态；租约过期是时间事件，展示端每次用 expires_at 重新计算 expired，不必等待缓存 TTL。
- 提交/删除等由外部事务持有的 journal 方法只递增 revision，不在事务内访问 Redis/Kafka；由调用方提交后刷新。真实状态查询、恢复扫描和 fencing 不追加缓存事件。直接用 SQL 修改业务表（含删除与同名重建）也会产生 CDC 事件并被同一条映射收敛。

## 第二批状态查询接口

全部使用现有鉴权和 namespace 检查，不增加 MCP 工具，也不改变 UUID 搜索结果协议：

- `GET /v1/admin/api-keys`：已有管理列表，现在使用缓存。
- `GET /v1/uploads`：当前 namespace 最近 100 条操作摘要。
- `GET /v1/uploads/{operation_id}`：当前 namespace 的操作/步骤/回滚状态；跨 namespace 或不存在返回 404。
- `GET /v1/admin/published-versions`：namespace 的已发布版本展示，额外要求本机直连管理权限。
- `GET /v1/admin/upload-leases`：namespace 的租约/执行者展示，额外要求本机直连管理权限。

新增状态响应带 `display_only: true` 和 `Cache-Control: no-store`，明确状态可能最终一致，不用于授权、决定写入或自动回滚。调用方应使用操作 ID 查询进度，而不是依据列表推断所有未完成任务。未提供独立前端进度页面。

## 配置

当前 Go 实现使用 kafka-go 客户端；宿主进程连接宿主机 listener，compose 容器使用内部 listener。

环境变量（可放入服务配置中的非敏感项）：

```text
RAG_REDIS_URL=redis://127.0.0.1:6379/0
RAG_REDIS_ENABLED=true
RAG_REDIS_PREFIX=rag:state:v1
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
RAG_SEARCH_TOPIC=praxis_search
RAG_SEARCH_GROUP=praxis_search_workers
RAG_DOCUMENT_DLQ_TOPIC=praxis_document_ingest_dead_letters
RAG_SEARCH_DLQ_TOPIC=praxis_search_dead_letters
RAG_CACHE_DLQ_TOPIC=praxis_state_cache_refresh_dead_letters
RAG_DOCUMENT_DLQ_GROUP=praxis_document_dead_letters
RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters
RAG_CACHE_DLQ_GROUP=praxis_state_cache_refresh_dead_letters
RAG_CACHE_WORKER_RECOVER_SECONDS=30
# RAG_DEBEZIUM_HEALTH_URL / RAG_DEBEZIUM_CACHE_HEALTH_URL 为可选只读健康探测
```

`RAG_REDIS_TTL_SECONDS=60` 只用于展示缓存，结果快照用 `RAG_RESULT_TTL_SECONDS=3600`。不设置 Redis URL 或显式 `RAG_REDIS_ENABLED=false` 可禁用 Redis：**排队搜索仍可受理并从 MySQL 读取结果**（只是没有读取缓存），结果快照 TTL 与 MySQL 有界清理不受影响。文档异步新增不依赖 Redis：禁用后仍可提交、查询状态并完成 MySQL 持久发布（Kafka 部署需 Debezium 完成通知投递，但不影响受理与状态查询）。同步 REST 文档上传、更新/删除仍使用 MySQL UploadJournal，不受 Redis 显式禁用影响；持久数据展示读取可回源 MySQL。多实例共享相同 Redis、前缀、MySQL 和各自的 cache/document/search topic/group（以及三个显式 DLQ topic）。不同 MySQL 数据库使用不同 Redis 前缀，防止隔离域串数据。Redis 密码可放 URL（勿打印或提交）；Kafka SASL/TLS 凭据仅从 `RAG_KAFKA_SASL_*` / `RAG_KAFKA_TLS_*` 读取。

## Docker 与启动

Compose 提供 Redis 7.4、单节点 KRaft Kafka（`apache/kafka:3.9.0`，宿主 `127.0.0.1:9092` / 容器 `kafka:9092`，4 partitions、自动创建 topic、`kafka_data` 持久卷）以及默认启动的**两个 Debezium 连接器**（`debezium` 任务连接器与 `debezium-cache` 缓存连接器，独立 offset/schema-history 与健康端口）。Redis 开启 AOF 和 noeviction；Kafka 数据写入 `KAFKA_LOG_DIRS=/var/lib/kafka/data`。不再有旧 RocketMQ 组件或存储卷 UID 初始化。

```powershell
.\scripts\start-stack.ps1 -MigrateOnly -SkipMigrate
.\scripts\start-stack.ps1 -SkipMigrate
```

脚本启动 MySQL、Kafka、两个 Debezium 连接器等依赖，创建 document/search/cache 与三个 DLQ topic（4 partitions、RF=1），然后运行服务。三类通知的 Kafka 投递由两个 Debezium 连接器承担；应用不写事务 outbox。

```powershell
docker exec praxis-rag-kafka /opt/kafka/bin/kafka-topics.sh --bootstrap-server kafka:9092 `
  --create --if-not-exists --topic praxis_state_cache_refresh --partitions 4 --replication-factor 1
```

对持久读模型刷新，Redis/Kafka 失败不回滚已提交的 MySQL 业务操作：revision 已递增，脏状态保留在 `cache_revisions`，由 `rag-cache-worker` 周期重试。对排队搜索，MySQL 提交即受理（不等待 Kafka/Redis），投递由 Debezium 任务连接器补足；结果以 MySQL 快照为准。对异步文档新增，MySQL 提交即受理，Kafka 故障既不回滚也不阻塞受理。启动前确认 MySQL、Redis（可选）、Kafka 连接、topic/group 与两个连接器权限。

## 运维与限制

`cache_revisions` 已扩展为 revision 水位（`revision`、`refreshed_revision`、`attempts`、`last_error`、`updated_at`），正常自动 schema 会幂等追加缺失列；`cache_outbox` 已下线并由迁移 CLI 删除。禁用自动 schema 的部署需先使用有 DDL 权限的账号执行同样的增量迁移。

展示缓存水位不独立淘汰；内存用量随缓存 kind/scope 增长。不要删除水位却保留 payload，或在旧 worker 仍回填时 FLUSHDB。清空/换库前先停入口并排空 worker，再停所有实例；新前缀可从 MySQL 重建**展示缓存**，排队搜索任务与结果也能从 MySQL 回源/重建（超过 TTL 的快照已清理则不可恢复，客户端需重新提交）；异步文档任务与文件字节在 MySQL，仍可查询并继续投递。Redis 持续不可用时脏状态保留在 `cache_revisions`，`RefreshDirty` 会持续重试；不要清理 revision 水位行。

本批不实现跨实例 MCP 订阅通知总线；配置缓存同步不等于通知可重放。UUID 搜索结果以 MySQL `search_result_snapshots` 为事实来源、Redis 为读取缓存；产物快照仍在 MySQL。文档任务状态使用 MySQL `document_jobs`，不进入 Redis 短期存储。见 [UUID 引用](result-references.md)。

## 验证

```powershell
go test -count=1 ./internal/state/ ./internal/redisstate/...
```

历史 Python 测试已随 Python 后端删除；当前 Go 状态层的缓存/Redis 测试使用上述命令。2026-11-13 的缓存消费者改造（Debezium 缓存连接器 + revision 水位 + MarkAllDirty/RefreshDirty）以 `internal/state` 与 `internal/cacheconsumer` 单测覆盖；真实 Redis/SASL/TLS 集群与双连接器实链路未验证，见 [VALIDATION](../VALIDATION.md)。
