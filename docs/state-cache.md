# Redis 短期任务状态与持久读模型缓存

## 范围

Redis 有两种不同职责，不能混用其故障语义：

- **短期权威状态**：MCP 异步上传/排队搜索的任务状态、上传分块重组及搜索结果快照只在 Redis 中保存。Redis 是必需依赖，不存在 SQL 回源。任务/重组及搜索快照沿用 `RAG_RESULT_TTL_SECONDS=3600`，与下面展示缓存的 60 秒 TTL 不同。
- **持久数据展示缓存**：MySQL 继续保存已发布文档/源文件/解析产物、KB/API Key/catalog、artifact 快照和 UploadJournal 操作/租约；Qdrant 继续检索。下面第一、二批读模型仍可在 Redis 故障时回源 MySQL，cache outbox 保持原义。

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

租约 owner 原始所有权 token 不放 Redis；holder_id 是 token 的短哈希，仅用于区分执行者。操作详情不缓存 idempotency_key、workspace、fingerprint、result 原文、步骤输入 details、文件内容或密钥。upload_status 的返回值再次验证 namespace，UUID 不授予访问权。API Key 列表仍仅允许本机管理员。

上述**展示读模型**不缓存原始文件、图片、解析产物、查询/重排结果或模型密钥。异步上传重组及搜索结果使用独立 Redis 短期存储（见下节），不套用此限制或 MySQL 回源策略；文档重组会在 Redis 暂存解码后的原始文件分块字节，这不是禁止存放原始字节的规则，也不是永久源文件存储。事务/步骤的展示投影可缓存，MySQL 原始 UploadJournal 始终是恢复依据；不迁移发布/恢复日志或业务租约权威到 Redis。文档列表中的 operation_id 也仅为展示元数据。

公开配置和列表命中 Redis 时直接返回，不额外查 MySQL 版本，也不构建 tenant 或调用模型。上传、去重、覆盖/删除检查、reembedding、鉴权、版本发布与恢复仍使用 MySQL 权威读取。工具列表允许缓存，但真正执行工具时直接校验 MySQL 的 enabled 状态，避免停用被旧缓存绕过。

## MCP 短期任务、重组与搜索快照

API 直接向 RocketMQ 投递 schema 2 完整请求：搜索消息包含查询及参数；文档请求分块传输，每块解码后最多 **512 KiB**，单文件仍最多 **10 MiB**。Redis 保存任务状态和分块重组数据。只有所有消息都获 Broker 确认且 Redis 接收状态写入成功后，API 才返回成功；`wait_seconds=0` 也不能跳过确认。成功响应表示已投递，不表示完成检索或文档发布。

MQ/Redis 未配置、SDK 缺失、连接失败或发送确认失败必须明确报错；故障期间没有 SQL 持久接收和 outbox 补发保证。部分发送后的超时可能不确定，上传重试应保留原 idempotency_key、内容和参数。Redis 丢失、淘汰或 TTL 到期可使任务/重组或搜索结果引用不可用，可能需要重新提交；不读旧 SQL 任务/结果表，也不自动重新搜索来伪造原快照。

文档与搜索由独立 worker、独立 topic/group 消费；缓存刷新使用第三组独立 topic/group。消费者续期 MQ visibility；文档分块未收齐不 ACK，Broker 重投使 Redis 过期/丢失后仍可重组原请求。任一收齐分块的消息可触发执行，已记录终态后其余重复分块才 ACK。临时故障不 ACK，确定性故障先写 Redis 失败终态再 ACK，成功也先写结果/终态再 ACK。Redis 任务状态通过 CAS 条件更新，结果与成功终态原子写入 Redis；重复消息遵循 first-terminal-wins，不能覆盖仍存在的终态。这种 Redis 内部原子性不包含 MySQL 发布。Redis 丢失后不承诺搜索持久去重。

文档 `succeeded` 只能在 MySQL UploadJournal 已持久发布之后写入。两次写入**不是 Redis+MySQL 原子事务**；若发布后 Redis 写入失败，重投通过 MySQL journal 对账并修复状态，而不是重复发布。业务租约、fencing、补偿和恢复仍由 MySQL 执行。日志仅记录任务/操作 ID 与安全错误类别，不记录查询、文件/消息正文或凭据。

升级前停旧入口并排空旧 worker/outbox/消息；API 和 worker 一起部署。schema 2 与旧 UUID-only 消息不兼容，旧任务和结果不迁移，原 SQL 表保留但不在活跃路径。文档 worker 已移除 `--dispatch-only`，dispatcher 兼容入口为空操作，不 relay SQL，也无文件系统 MQ spool；下面的 `cache_outbox` 不受此迁移影响。结果与 MySQL artifact 保留边界见 [UUID 引用](result-references.md)。

## 持久读模型的读写与可靠同步

```text
读取：Redis 命中 → 返回；未命中 → MySQL 一致快照 → 带修订号回填
写入：MySQL 同事务更新业务数据、cache_revisions、cache_outbox
      → 提交成功后同步刷新 Redis
      → 失败投递 RocketMQ（消息只含 event_id）
      → 消费者重新读取最新公开快照，刷新成功才 ACK
```

- 数据和版本用一个 Redis Lua 原子写入；版本使用十进制字符串比较，避免 BIGINT 超出 Lua 浮点精度。
- 每种 kind/scope 独立递增修订号；旧消息和旧回填不能覆盖新值。
- MySQL 修订行是快照锁：写方先获取修订行再改业务数据；缓存未命中和消息刷新读取同一提交快照的数据与修订号。
- Redis payload 默认 TTL 60 秒；另一个持久水位 key 保存版本/删除标记，防止 payload 过期后旧回填复活已删除数据。相同修订号不能覆盖删除标记。
- 同步消息是 at-least-once，重复、乱序消息允许；消费者读最新数据库快照，不回放消息中的陈旧数据。
- Redis、MQ 都故障或进程中断时，MySQL outbox 保留任务；后台每 2 秒处理待同步事件，MQ 已发送但未完成的事件超过 60 秒再次投递。MQ 达到最大重试次数进 DLQ 后，outbox 仍可补发；需监控持续失败。
- 同一 kind/scope 的积压刷新会合并到最新修订。成功刷新后，将所有不大于该修订的事件标记 DONE。
- Redis 不可读时直接回源 MySQL。Redis 可读但刷新失败时可能暂时返回旧值，补偿/TTL 后收敛；这是最终一致性，不是跨库原子提交。
- 新增/更新/删除知识库刷新公开配置、文档展示及全局列表；文档发布/删除刷新文档与全局列表；MCP 修改刷新 catalog。
- 上传未发布时，公开文档和发布指针保持旧版本，但 RUNNING/FAILED/ROLLING_BACK 等进度缓存照常提交。最终发布的 heads 和 COMMITTED/步骤状态事件，与业务记录在同一 MySQL 事务中提交；发布回滚不会留下这些未提交事件。补偿完成则另行提交 ROLLED_BACK 的状态事件。
- API Key 创建、修改、删除/导入刷新管理列表。续租只刷新 leases，不制造无关步骤状态；租约过期是时间事件，展示端每次用 expires_at 重新计算 expired，不必等待缓存 TTL。
- 提交/删除等由外部事务持有的 journal 方法仅写 outbox，不在事务内访问 Redis/MQ；由调用方提交后同步刷新。真实状态查询、恢复扫描和 fencing 不追加缓存事件。

## 第二批状态查询接口

全部使用现有鉴权和 namespace 检查，不增加 MCP 工具，也不改变 UUID 搜索结果协议：

- `GET /v1/admin/api-keys`：已有管理列表，现在使用缓存。
- `GET /v1/uploads`：当前 namespace 最近 100 条操作摘要。
- `GET /v1/uploads/{operation_id}`：当前 namespace 的操作/步骤/回滚状态；跨 namespace 或不存在返回 404。
- `GET /v1/admin/published-versions`：namespace 的已发布版本展示，额外要求本机直连管理权限。
- `GET /v1/admin/upload-leases`：namespace 的租约/执行者展示，额外要求本机直连管理权限。

新增状态响应带 `display_only: true` 和 `Cache-Control: no-store`，明确状态可能最终一致，不用于授权、决定写入或自动回滚。调用方应使用操作 ID 查询进度，而不是依据列表推断所有未完成任务。未提供独立前端进度页面。

## 配置

普通安装包含 redis-py；RocketMQ 重试需要可选 SDK：

```powershell
.\.venv\Scripts\python.exe -m pip install -e ".[cache]"
```

环境变量（可放入服务配置中的非敏感项）：

```text
RAG_REDIS_URL=redis://127.0.0.1:6379/0
RAG_REDIS_ENABLED=true
RAG_REDIS_PREFIX=rag:state:v1
RAG_REDIS_TTL_SECONDS=60
RAG_REDIS_TIMEOUT_SECONDS=0.3
RAG_ROCKETMQ_ENDPOINT=127.0.0.1:8081
RAG_ROCKETMQ_TOPIC=praxis_state_cache_refresh
RAG_ROCKETMQ_GROUP=praxis_state_cache_refresh_workers
RAG_ROCKETMQ_NAMESPACE=
RAG_ROCKETMQ_TLS=false
```

`RAG_REDIS_TTL_SECONDS=60` 只用于展示缓存，搜索快照用 `RAG_RESULT_TTL_SECONDS=3600`。不设置 Redis URL 或显式 `RAG_REDIS_ENABLED=false` 可禁用 Redis，但**不能使用 MCP 异步上传、排队搜索或可复用搜索结果快照**，不可将禁用缓存当作这些路径的降级方式。同步 REST 文档上传、更新/删除仍使用 MySQL UploadJournal，不受 Redis 显式禁用影响；持久数据展示读取可回源 MySQL。多实例共享相同 Redis、前缀、MySQL 和各自 topic/group。不同 MySQL 数据库使用不同 Redis 前缀，防止隔离域串数据。Redis 密码可放 URL（勿打印或提交）；MQ 凭据仅从环境变量 RAG_ROCKETMQ_ACCESS_KEY / RAG_ROCKETMQ_SECRET_KEY 读取。

## Docker 与启动

Compose 新增 Redis 7.4、RocketMQ 5.3.3 NameServer、Broker/Proxy 和存储卷初始化。仅向本机发布 Redis 6379 与 gRPC Proxy 8081，NameServer 不发布端口。Redis 开启 AOF 和 noeviction；Broker 同步刷盘。初始化服务仅修复 RocketMQ 自有卷 UID，不访问宿主机其他目录。

```powershell
.\scripts\start-stack.ps1 -MigrateOnly -SkipMigrate
.\scripts\start-stack.ps1 -SkipMigrate
```

脚本启动依赖、等待 MySQL、创建/更新 MQ topic 和 consumer group（最大重试 16 次），随后运行服务；HTTP lifespan 启停缓存 relay/consumer。只运行 docker-compose up 不会自动创建 topic/group，需脚本或手动执行：

```powershell
docker exec praxis-rag-rocketmq sh mqadmin updatetopic -n rocketmq-namesrv:9876 -c DefaultCluster -t praxis_state_cache_refresh
docker exec praxis-rag-rocketmq sh mqadmin updateSubGroup -n rocketmq-namesrv:9876 -c DefaultCluster -g praxis_state_cache_refresh_workers -r 16
```

对持久读模型刷新，Redis/RocketMQ 失败不回滚已提交的 MySQL 业务操作，`cache_outbox` 保留待处理事件。对 MCP 上传/排队搜索，Redis/MQ 是提交前置依赖；缺 SDK/配置或连接失败直接报错，不通过 SQL task outbox 持久接收。启动前安装 cache extra，确认 Redis、MQ 连接、topic/group 和权限。

## 运维与限制

新增表 cache_revisions 和 cache_outbox；正常自动 schema 会建缺失表，不改既有业务列。禁用自动 schema 的部署需先使用有 DDL 权限的账号建表。

展示缓存水位不独立淘汰；内存用量随缓存 kind/scope 增长。不要删除水位却保留 payload，或在旧 worker 仍回填时 FLUSHDB。清空/换库前先停入口并排空 worker，再停所有实例；新前缀可从 MySQL 重建**展示缓存**，不能恢复短期任务、未重组上传或搜索快照，须告知客户端已有 ID 可能不可用并需重新提交。Redis 持续不可用时 `cache_outbox` 会积压；DONE 事件按审计要求归档清理（不要清理未完成事件或修订水位）。

本批不实现跨实例 MCP 订阅通知总线；配置缓存同步不等于通知可重放。UUID 搜索结果使用独立 Redis TTL 存储，产物快照仍在 MySQL；两者都不复用展示缓存 outbox。见 [UUID 引用](result-references.md)。

## 验证

```powershell
.\.venv\Scripts\python.exe -m pytest tests\test_state_cache.py tests\test_cache_synchronization.py tests\test_cached_read_views.py tests\test_cache_publication.py tests\test_cache_queue.py tests\test_api_key_cache.py tests\test_second_batch_cache.py tests\test_journal_cache_events.py
```

可选真实 Redis 测试设置 RAG_TEST_REDIS_URL；完整 MySQL/Redis/RocketMQ 验证设置 RAG_TEST_CACHE_MYSQL_URL 指向名为 praxis_cache_test_* 的独立测试库，并创建独立 RAG_TEST_ROCKETMQ_GROUP。tests/test_cache_live.py 会清空测试 schema 的表，仅可使用可丢弃数据库；仅删除自身前缀的 Redis key。
