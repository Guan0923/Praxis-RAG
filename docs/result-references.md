# UUID 搜索结果快照与产物引用

## 已确认的规则

- 搜索结果快照持久化到 MySQL，Redis 负责加速；默认有效期 **3600 秒（1 小时）**。
- Redis 淘汰、丢数据、未启用或故障时，从 MySQL 读取同一份快照，不重新搜索、Embedding 或重排；MySQL 中已过期或不存在则明确失败。
- 完成的搜索响应保留完整结果、图片和表格，新增 result_id、expires_at、artifact 引用。服务端 MCP `search` 使用任务队列；等待未完成时仅返回 result_id，客户端须用现有 `get_search_result` 轮询。REST `/v1/search` 与本地同步调试保持同步。
- 已签发的源文件、图片、表格、解析产物引用绑定具体版本。原文档更新、删除后，旧引用继续有效至到期。
- 删除文档只设置 `documents.del_flag=true`：新的列表、按名称读取、去重、覆盖存在检查和搜索过滤掉该记录；不撤销旧 UUID，不物理删除当前源文件、产物或 Qdrant 向量。重新上传该名称成功发布后清除 del_flag。
- UUID 不是访问凭证；每次请求仍执行现有 API Key/namespace 鉴权。权限撤销会阻止后续读取。

## 保存与取回

```text
服务端 MCP search → MySQL 任务/参数/outbox → RocketMQ（仅 result_id）
                                             ↓
                            rag-search-worker → 原有检索/重排
                                             ↓
                  复制返回产物 → MySQL 结果快照 → Redis 加速
                                             ↓
等待内完成：完整结果 + result_id；否则：仅 result_id → get_search_result 轮询

REST /v1/search / 本地调试 → 同步检索/重排（不经搜索任务队列）
完成快照 / artifact → 按 ID 直接读取，不调用搜索或模型
```

每次搜索生成独立 UUIDv4 result_id，不按查询文本匹配。服务端 MCP 的 result_id 在提交时生成，任务与最终快照使用同一 UUID；搜索参数只进入 MySQL，不进入 Redis 或 MQ。与 `add_document` 不同，`search` 只需 MySQL 任务及 outbox 持久化接收即可返回 UUID，不等待 Broker 确认；API relay 异步投递，MQ 启动或故障不阻塞 `wait_seconds` 的完成等待窗口，队列不可用时任务保持排队直至截止时间。MySQL `search_result_snapshots` 保存独立 UUID、namespace、完整响应及绝对过期时间。Redis key 包含 namespace 哈希及 ID，TTL 与状态缓存独立；UUID 全局唯一，不允许用同一个 UUID 覆盖其他快照。快照保留原结果顺序、分数、证据、debug 元数据及产物引用。分页只切片原快照，不再查询 Qdrant。

相同 namespace、文档版本、产物类型、名称和字节内容复用稳定 UUIDv5 artifact_id。新的引用可单调延长它的有效期，不缩短已有引用；旧响应里的 expires_at 是该次签发时的保留期限。

## 产物保留方式

新增 MySQL `artifact_snapshots` 表，保存版本固定的产物 ID、元数据、字节和 expires_at；结果另存于 `search_result_snapshots`。产物字节按需复制一份，以便上传替换、文档删除和旧版本清理不会破坏已经发出的 ID。无需修改租约/上传事务或长期阻止旧版本清理。

搜索命中后可签发：

- 源文件 source；
- PDF/Word 的 document.md、manifest.json、document.json、ocr.json（存在时）；
- 命中图片的原字节；
- 命中表格的结构化 JSON。

只注册实际存在且在限制内的内容，不伪造缺失产物。图片/表格在原 asset 上增加 artifact_id、resource_uri、expires_at；每条命中及顶层 artifacts 提供文件/解析产物引用。旧文档无 upload_version 时按 legacy 数据读取，不把旧命中指向新版本源文件。

捕获期间不持有写租约，因此不会因为另一个节点上传就阻止搜索旧的已提交版本。如果原源文件恰好在捕获前被其他节点删除，可能无法签发其文件引用；已经成功注册并返回的 artifact_id 才具有保留保证。命中的完整文本/图片/表格仍保留在结果快照中。

服务后台恢复循环按批清理到期的 artifact_snapshots 和 search_result_snapshots；到期判断不依赖清理是否已执行。文档软删除不撤销快照；del_flag 记录和源文件、向量不会被 TTL 任务物理删除。旧上传版本仍可按原事务 GC 机制清理，已签发产物有独立字节副本，不受影响。

正常自动 schema 创建两张快照表；新增兼容升级为既有 documents 表添加非空布尔 del_flag（默认 0，保留旧行），支持重复执行及并发启动重复添加的检查。禁用自动 schema 的生产部署需先建快照表，并执行：

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

MySQL 保存失败或快照过大时，仍返回原搜索结果，并设置 snapshot_available=false / snapshot_error，不返回无法读取的 result_id。Redis 故障不影响快照签发和回源读取。已成功签发的 artifact 引用仍可用。单个产物保存失败或超过限制时，artifact_warning 提示部分引用不可用，不影响原结果。

### 结果分页

`GET /v1/search-results/{result_id}?page=1&page_size=10`

- page 从 1 开始，page_size 为 1..100。
- 返回原响应元数据及这一页 results、total_results、next_page。
- 结果不存在、跨 namespace、过期或持久化存储暂不可用：410；不自动重跑。Redis 淘汰或不可用会回源同一 MySQL 快照。
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
- 扩展现有 `get_search_result`（不重复注册新工具）：参数 result_id，以及可选 page/page_size。任务未完成返回 `{"result_id":"<UUID>","status":"queued","stage":"queued"}`；执行中或等待重试仍为 `status="queued"`，stage 分别为 `processing` / `retry_wait`。任务状态直接读取 SQL，不依赖 Redis 状态缓存。
- 成功后 `get_search_result` 返回完整持久化快照（可分页），沿用原来的 ImageContent 渲染，不重新检索。执行最终失败、任务 deadline 到达或结果过期返回 MCP `isError=true`，不能当作成功的空结果。
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

- 服务端 MCP 搜索必须运行独立 `rag-search-worker` 消费者；已有部署若未启动，搜索会排队直至 deadline。`rag-search-worker --dispatch-only` 只补发 outbox，不执行检索。每个消费进程默认顺序执行，文档与搜索使用独立 topic/group；worker 与 API 共享 MySQL、Redis、MQ、Qdrant 和状态加密设置。
- 任务默认总超时 120 秒，包含排队等待，与 `wait_seconds`（默认 10 秒）和完成快照的 3600 秒 TTL 独立。生产检索在子进程执行；MQ 可见性续期失败有界重试，不承诺立即取消正在执行的检索或下游调用。
- MySQL 保存查询参数、业务状态和首次投递 outbox；消费重试、invisible 与 DLQ 由 Broker 管理。临时故障不 ACK，交由 Broker 重投；确定性失败先提交 SQL 终态再 ACK。终态 first-terminal-wins；即使续期失败，已提交终态仍尝试 ACK，重复消息不得覆盖终态或重新执行已完成搜索。
- 计划提供独立 `rag-dead-letter-worker --kind search`（也支持 `--kind document`），只读任务并提交失败终态，不加载模型或执行推理。正常 Broker 死信 topic 为 `%DLQ%<源消费组>`，处理组使用独立 `RAG_SEARCH_DLQ_GROUP`；不能把处理组名当死信 topic。传输层已验证 Python SDK 5.1.2 / Apache Broker 5.3.3 的正常 DLQ 接收及 ACK；其他版本上线前需重新验证，并预建可读 topic、处理组及权限，详见 [Broker 消费与死信处理](USAGE.md#broker-消费与死信处理)。
- 结果快照默认最大 16 MiB；保持原图片兼容响应。同步 REST 搜索快照过大时仍可返回原结果并标记不可持久化；MCP 排队任务不能以未持久化的结果冒充已完成快照。
- 每次搜索默认至多签发 100 个产物，累计字节预算 64 MiB；同一搜索的重复产物不重复计费。
- 单个 artifact 默认也受 64 MiB 上限约束，MySQL max_allowed_packet 必须满足实际产物大小；默认 Compose 为 64 MiB，接近上限的大对象可能因协议开销失败并提示引用不可用。
- Redis 使用已有连接配置和前缀；不可变结果快照缓存不采用状态修订水位/outbox，缓存丢失直接回源 MySQL，不延长绝对过期时间，不通过 MQ 重跑已完成结果。服务端 MCP 搜索执行任务另有 MySQL outbox 和 RocketMQ 队列，不应与结果缓存混淆。
- 此版本不增加前端快照历史/下载入口；现有搜索 UI 继续使用完整结果。

## 验证

```powershell
.\.venv\Scripts\python.exe -m pytest tests\test_result_snapshots.py tests\test_artifact_registry.py tests\test_uuid_protocol.py tests\test_soft_delete_references.py
```

真实服务测试：RAG_TEST_UUID_MYSQL_URL 指向可丢弃的 praxis_uuid_test_* 库，RAG_TEST_REDIS_URL 指向 Redis，可选 RAG_TEST_QDRANT_URL。运行 tests/test_uuid_live.py；测试会清空独立 schema、删除自己前缀的 Redis key 及独立 uuid_test_* 集合，不可指向生产库。
