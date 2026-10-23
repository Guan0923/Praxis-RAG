# Redis 状态缓存（第一批 + 第二批严格模式）

## 范围

MySQL 继续持久化，Qdrant 继续检索。第一批缓存四种读模型（MCP 工具和提示词共用一份 catalog）：

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

不缓存原始文件、图片、解析产物、查询/重排结果、明文/加密模型密钥。事务/步骤的**展示投影**可缓存，MySQL 原始日志始终是恢复依据；本次不是迁移日志或租约权威到 Redis。文档列表中的 operation_id 也仅为展示元数据。

公开配置和列表命中 Redis 时直接返回，不额外查 MySQL 版本，也不构建 tenant 或调用模型。上传、去重、覆盖/删除检查、reembedding、鉴权、版本发布与恢复仍使用 MySQL 权威读取。工具列表允许缓存，但真正执行工具时直接校验 MySQL 的 enabled 状态，避免停用被旧缓存绕过。

## 读写与可靠同步

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

不设置 Redis URL 默认禁用缓存，不影响原有部署。多实例共享相同 Redis、前缀、MySQL、topic/group。不同 MySQL 数据库必须使用不同 Redis 前缀，防止隔离域串数据。Redis 密码可放 URL（勿打印或提交）；MQ 凭据仅从环境变量 RAG_ROCKETMQ_ACCESS_KEY / RAG_ROCKETMQ_SECRET_KEY 读取。

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

Redis/RocketMQ 启动失败不应回滚已提交的业务操作。部署缺少 SDK 时运行日志提示配置错误，outbox 保持待处理。应在启动前安装 cache extra 并确认连接。

## 运维与限制

新增表 cache_revisions 和 cache_outbox；正常自动 schema 会建缺失表，不改既有业务列。禁用自动 schema 的部署需先使用有 DDL 权限的账号建表。

Redis 水位不独立淘汰；内存用量随缓存 kind/scope 增长。不要删除水位却保留 payload，或在旧 worker 仍回填时 FLUSHDB。全量清空/换库应先停所有实例并重启，或在所有实例使用新 RAG_REDIS_PREFIX 后回源重建。Redis 持续不可用时 outbox 会积压；DONE 事件当前保留用于排查，生产需根据审计要求定期归档清理 DONE 行（不要清理未完成事件或修订水位）。

本批不实现跨实例 MCP 订阅通知总线；配置缓存同步不等于通知可重放。UUID 检索结果/产物读取属于独立第三批，见 [UUID 引用](result-references.md)，不复用状态 outbox。

## 验证

```powershell
.\.venv\Scripts\python.exe -m pytest tests\test_state_cache.py tests\test_cache_synchronization.py tests\test_cached_read_views.py tests\test_cache_publication.py tests\test_cache_queue.py tests\test_api_key_cache.py tests\test_second_batch_cache.py tests\test_journal_cache_events.py
```

可选真实 Redis 测试设置 RAG_TEST_REDIS_URL；完整 MySQL/Redis/RocketMQ 验证设置 RAG_TEST_CACHE_MYSQL_URL 指向名为 praxis_cache_test_* 的独立测试库，并创建独立 RAG_TEST_ROCKETMQ_GROUP。tests/test_cache_live.py 会清空测试 schema 的表，仅可使用可丢弃数据库；仅删除自身前缀的 Redis key。
