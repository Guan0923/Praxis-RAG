# UUID 搜索结果快照与产物引用
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。


## 已确认的规则

- 搜索结果快照**只存 Redis**，沿用 `RAG_RESULT_TTL_SECONDS`，默认有效期 **3600 秒（1 小时）**；Redis 是必需依赖，不是可选加速层。
- Redis 淘汰、丢数据、未启用、故障或 TTL 到期时，结果引用不可用，明确失败；**无 SQL 回源**，不自动重新搜索、Embedding 或重排，客户端可能需要重新提交。MySQL 的持久保留保证仅适用于下述 artifact 快照。
- 完成的搜索响应保留完整结果、图片和表格，新增 result_id、expires_at、artifact 引用。服务端 MCP `search` 使用任务队列；等待未完成时仅返回 result_id，客户端须用现有 `get_search_result` 轮询。REST `/v1/search` 与本地同步调试保持同步。
- 已签发的源文件、图片、表格、解析产物引用绑定具体版本。原文档更新、删除后，旧引用继续有效至到期。
- 删除文档只设置 `documents.del_flag=true`：新的列表、按名称读取、去重、覆盖存在检查和搜索过滤掉该记录；不撤销旧 artifact UUID，不物理删除当前源文件、产物或 Qdrant 向量。重新上传该名称成功发布后清除 del_flag。搜索结果仍受 Redis 数据存续与 TTL 约束。
- UUID 不是访问凭证；每次请求仍执行现有 API Key/namespace 鉴权。权限撤销会阻止后续读取。

## 保存与取回

```text
服务端 MCP search → RocketMQ schema 2（完整查询/参数）→ Broker 确认
                                             ↓
                         rag-search-worker → 原有检索/重排
                                             ↓
                   MySQL artifact 快照 + Redis 任务/结果快照
                                             ↓
等待内完成：完整结果 + result_id；否则：仅 result_id → get_search_result 轮询

异步文档新增（MCP add_document）→ MySQL 单事务（document_jobs 完整任务含文件字节
                                  + document_job_outbox 通知）→ 提交即返回 artifact_id
                                             ↓
                  relay / Debezium → RocketMQ schema 3 纯通知（仅标识）
                                             ↓
                  rag-document-worker → 从 MySQL 读回任务并解析/索引
                                             ↓
             get_document_status 直接读 MySQL（无 Redis TTL）；succeeded 表示已发布

REST /v1/search / 本地调试 → 同步检索/重排（不经搜索任务队列）
结果按 ID 读取 Redis；artifact 按 ID 读取 MySQL；不调用搜索或模型
```

每次搜索生成独立 UUIDv4 result_id，不按查询文本匹配。MCP 的任务与最终快照使用同一 UUID。**两条路径的受理语义不同**：搜索消息仍携带完整查询与参数直投 RocketMQ，必须在 Broker 确认、Redis 快照就绪后才返回成功；异步文档新增只提交一个 MySQL 事务（完整任务含文件原字节 + 发件箱通知），提交成功即返回 `artifact_id`，MQ 只传 schema 3 纯通知，Redis 不参与受理、状态或投递。文档投递失败不影响受理，由发件箱 relay/Debezium 重试；"发送确认超时可能不确定"只适用于搜索。

Redis 保存排队搜索的 namespace、短期任务状态、完整搜索响应和绝对过期时间。key 包含 namespace 哈希及 ID，结果 TTL 与展示缓存独立；UUID 不允许覆盖其他快照。快照保留原结果顺序、分数、证据、debug 元数据及产物引用，分页只切片原快照，不再查 Qdrant。读取不延长绝对过期时间。文档任务状态、租约和文件字节保存在 MySQL，不使用该 Redis 快照，也没有 TTL。

升级必须先停旧入口并排空旧 worker 及在途消息，再同时部署 API 与 worker；文档消息已从 schema 2 完整请求改为 schema 3 纯通知，不能混跑；搜索仍为 schema 2，与更早的 UUID-only 消息不兼容。`document_tasks`、`document_task_outbox`、`search_tasks`、`search_task_outbox`、`search_result_snapshots` 保留为历史表，不在活跃路径；旧任务和结果不迁移。日志仅记录 ID 和安全错误类别，不记录查询、文件/消息正文或秘密。

相同 namespace、文档版本、产物类型、名称和字节内容复用稳定 UUIDv5 artifact_id。新的引用可单调延长它的有效期，不缩短已有引用；旧响应里的 expires_at 是该次签发时的保留期限。

## 产物保留方式

MySQL `artifact_snapshots` 保存版本固定的产物 ID、元数据、字节和 expires_at；搜索结果本身只存 Redis，历史 `search_result_snapshots` 不再读写。产物字节按需复制一份，以便上传替换、文档删除和旧版本清理不会破坏已经发出的 artifact ID。无需修改业务租约/上传事务或长期阻止旧版本清理；此持久保证不承诺 Redis result_id 一直可读。

搜索命中后可签发：

- 源文件 source；
- PDF/Word 的 document.md、manifest.json、document.json、ocr.json（存在时）；
- 命中图片的原字节；
- 命中表格的结构化 JSON。

只注册实际存在且在限制内的内容，不伪造缺失产物。图片/表格在原 asset 上增加 artifact_id、resource_uri、expires_at；每条命中及顶层 artifacts 提供文件/解析产物引用。旧文档无 upload_version 时按 legacy 数据读取，不把旧命中指向新版本源文件。

捕获期间不持有写租约，因此不会因为另一个节点上传就阻止搜索旧的已提交版本。如果原源文件恰好在捕获前被其他节点删除，可能无法签发其文件引用；已经成功注册并返回的 artifact_id 才具有保留保证。命中的完整文本/图片/表格仍保留在结果快照中。

服务后台恢复循环按批清理到期的 MySQL `artifact_snapshots`；搜索快照由 Redis TTL 到期。到期判断不依赖后台清理是否已执行。文档软删除不撤销已签发 artifact；del_flag 记录和源文件、向量不会被这些 TTL 物理删除。旧上传版本仍可按原事务 GC 清理，已签发产物有独立字节副本，不受影响。

正常自动 schema 创建缺失的 artifact 表；历史 SQL 结果表保留、不删除、不迁移。兼容升级为既有 documents 表添加非空布尔 del_flag（默认 0，保留旧行），支持重复执行及并发启动重复添加检查。禁用自动 schema 的生产部署需先建 artifact 表，并执行：

```sql
ALTER TABLE documents ADD COLUMN del_flag BOOLEAN NOT NULL DEFAULT 0;
```

没有新增后台物理删除已软删文档的策略；保留数据增长需按业务审计要求另行设计。

## REST

所有接口沿用 Authorization 与 X-RAG-Namespace，响应不允许浏览器公共缓存。

### 搜索

`POST /v1/search` 保留原 body 与结果，新增：

```json
{
  "result_id": "6cec23f3-0452-4289-a1b5-b18c018d0634",
  "created_at": 1792670000.0,
  "expires_at": 1792673600.0,
  "snapshot_available": true,
  "results": [],
  "artifacts": []
}
```

同步 REST 搜索若 Redis 保存失败或快照过大，仍返回原搜索结果，设置 snapshot_available=false / snapshot_error，不签发无法读取的 result_id；它不享有 SQL 结果回源。MCP 排队提交则要求 Redis/MQ 可用，不能以未保存的结果冒充完成快照。已成功签发的 MySQL artifact 引用仍可用。单个产物保存失败或超过限制时，artifact_warning 提示部分引用不可用，不影响原结果。

### 结果分页

`GET /v1/search-results/{result_id}?page=1&page_size=10`

- page 从 1 开始，page_size 为 1..100。
- 返回原响应元数据及这一页 results、total_results、next_page。
- 结果不存在、跨 namespace、过期、Redis 丢失或暂不可用：410；无 SQL 回源，不自动重跑。需要重新提交时会生成新的结果，而不是恢复原快照。
- 非法分页/页码超出范围：400 或参数校验 422。

### 下载与分块

- `GET /v1/artifacts/{artifact_id}`：直接下载原字节，带安全 Content-Disposition。
- `GET /v1/artifacts/{artifact_id}/content?offset=0&length=65536`：JSON base64 字节片段。
- offset 为字节位置；length 为 1..1048576，默认 65536。
- envelope 包含 data、encoding=base64、total_bytes、offset、next_offset、mime_type 和 expires_at。
- 文件不存在、过期或跨 namespace：410。不会回源已删除文档或重新解析。

## MCP

不修改 MCP 标准，只扩展本项目工具与资源：

- 服务端 `search`：新增 `wait_seconds`，必须为有限数值（支持小数），0..30，默认 10；不是任务 deadline。在等待内完成返回原完整 payload、同一任务 UUID `result_id` 和产物引用；否则**仅**返回 `{"result_id":"<UUID>"}`。REST `/v1/search` 和本地同步调试不变。
- 扩展现有 `get_search_result`（不重复注册新工具）：参数 result_id，以及可选 page/page_size。任务未完成返回 `{"result_id":"<UUID>","status":"queued","stage":"queued"}`；执行中或等待重试仍为 `status="queued"`，stage 分别为 `processing` / `retry_wait`。任务状态直接读取 Redis；丢失/过期即不可用，不回源旧 SQL 任务表。
- 成功后 `get_search_result` 返回 Redis 完整快照（可分页），沿用原来的 ImageContent 渲染，不重新检索。执行最终失败、任务 deadline 到达、Redis 数据丢失或结果过期返回 MCP `isError=true`，不能当作成功的空结果。
- Resource 模板：`rag://artifacts/{namespace}/{artifact_id}{?offset,length}`。
- read_resource 返回 JSON 分块 envelope，next_uri 指向下一段；按 next_uri 读取并 base64 解码、拼接可恢复文件。
- 原 rag://knowledge 文档资源和工具启停管理不变。
- 过期/不可用通过明确 MCP 错误返回；调用工具仍遵守管理端启停。

## 配置与限制

```text
RAG_SEARCH_TOPIC=praxis_search
RAG_SEARCH_GROUP=praxis_search_workers
RAG_SEARCH_TIMEOUT_SECONDS=120
# Provisioning only; Broker owns consumer retries (not runtime Python settings).
RAG_SEARCH_BROKER_MAX_RETRIES=16
RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters
RAG_RESULT_TTL_SECONDS=3600
RAG_RESULT_MAX_BYTES=16777216
RAG_ARTIFACT_MAX_BYTES=67108864
RAG_RESULT_MAX_ARTIFACTS=100
```

- MCP 搜索必须运行独立 `rag-search-worker`；没有消费者会排队直至 deadline。每个消费进程默认顺序执行，文档、搜索、缓存刷新使用独立 topic/group；worker 与 API 共享 MySQL、必需的 Redis/MQ、Qdrant 和状态加密设置。文档异步新增另需 `rag-document-worker`（不依赖 Redis），投递模式由 `RAG_OUTBOX_BRIDGE` 选择。
- 任务默认总超时 120 秒，包含排队等待，与 `wait_seconds`（默认 10 秒）和完成快照的 3600 秒 TTL 独立。生产检索在子进程执行；MQ 可见性续期失败有界重试，不承诺立即取消正在执行的检索或下游调用。
- MQ 携带完整查询参数，Redis 保存短期搜索任务/结果；任务状态采用 CAS，结果快照与成功终态在 Redis 内原子保存（不含 MySQL 事务）；消费重试、invisible 与 DLQ 由 Broker 管理。临时故障不 ACK；确定性失败先写 Redis 失败终态再 ACK，成功先存结果/终态再 ACK。终态 first-terminal-wins；续期失败后已记录终态仍尝试 ACK，重复消息不覆盖仍存在的终态；Redis 丢失后不保证搜索持久去重。
- 文档消息只含 schema 3 通知标识；worker 按 `artifact_id` 从 MySQL 读回任务与文件字节，用 SQL 租约 claim 并心跳续租，只有 MySQL 记录终态才 ACK，瞬时失败不 ACK 由 Broker 重投。没有 Redis 任务状态或重组 TTL，也没有 Redis+MySQL 原子事务：文档 `succeeded` 就是 MySQL 持久发布结果。
- 文档重投通过 UploadJournal 对账，不重复发布；MySQL 仍保存业务操作/租约、fencing、补偿与恢复。
- 独立 `rag-dead-letter-worker --kind search`（也支持 `--kind document`）把尚未终结的任务标记失败：搜索写 Redis、文档写 MySQL，成功写入后才 ACK，不加载模型或执行推理。正常 Broker 死信 topic 为 `%DLQ%<源消费组>`，处理组使用独立 `RAG_SEARCH_DLQ_GROUP`；不能把处理组名当死信 topic。传输层已验证 Python SDK 5.1.2 / Apache Broker 5.3.3 的正常 DLQ 接收及 ACK；其他版本上线前需重新验证，并预建可读 topic、处理组及权限，详见 [Broker 消费与死信处理](USAGE.md#broker-消费与死信处理)。
- 结果快照默认最大 16 MiB；保持原图片兼容响应。同步 REST 搜索快照过大时仍可返回原结果并标记快照不可用；MCP 排队任务不能以未写入 Redis 的结果冒充已完成快照。
- 每次搜索默认至多签发 100 个产物，累计字节预算 64 MiB；同一搜索的重复产物不重复计费。
- 单个 artifact 默认也受 64 MiB 上限约束，MySQL max_allowed_packet 必须满足实际产物大小；默认 Compose 为 64 MiB，接近上限的大对象可能因协议开销失败并提示引用不可用。
- Redis 使用已有连接配置和前缀；结果存储不采用展示缓存修订水位/cache outbox，也没有 SQL fallback。Redis 丢失或 TTL 到期不会通过 MQ 自动重跑已完成结果；客户端可能需要重新提交。不要将 MySQL artifact 保留保证与短期搜索结果混淆。
- 此版本不增加前端快照历史/下载入口；现有搜索 UI 继续使用完整结果。

## 验证

```powershell
.\.venv\Scripts\python.exe -m pytest tests\test_result_snapshots.py tests\test_artifact_registry.py tests\test_uuid_protocol.py tests\test_soft_delete_references.py
```

真实服务测试：RAG_TEST_UUID_MYSQL_URL 指向可丢弃的 praxis_uuid_test_* 库，RAG_TEST_REDIS_URL 指向 Redis，可选 RAG_TEST_QDRANT_URL。运行 tests/test_uuid_live.py；测试会清空独立 schema、删除自己前缀的 Redis key 及独立 uuid_test_* 集合，不可指向生产库。
