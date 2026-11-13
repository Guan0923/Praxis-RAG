# UUID 搜索结果快照与产物引用
> **历史设计文档**：对应的 Python 后端实现已删除；当前 Go 实现位于 `internal/` 对应包，迁移状态与差异见 [GO_MIGRATION](GO_MIGRATION.md)。本文的 schema/契约描述仍作为共享状态层的设计参考。


## 已确认的规则

- 搜索结果快照**以 MySQL `search_result_snapshots` 为事实来源**，沿用 `RAG_RESULT_TTL_SECONDS`，默认有效期 **3600 秒（1 小时）**；Redis 只是读取缓存，不是事实来源。
- Redis 淘汰、丢数据、未启用或故障时，读取回源 MySQL 并尽力回填，客户端不需要重新提交；TTL 到期后 MySQL 快照被有界批量清理，结果引用才不可用（明确失败），不自动重新搜索、Embedding 或重排。MySQL 的持久保留保证适用于下述 artifact 快照。
- 完成的搜索响应保留完整结果、图片和表格，新增 result_id、expires_at、artifact 引用。服务端 MCP `search` 使用任务队列；等待未完成时仅返回 result_id，客户端须用现有 `get_search_result` 轮询。REST `/v1/search` 与本地同步调试保持同步。
- 已签发的源文件、图片、表格、解析产物引用绑定具体版本。原文档更新、删除后，旧引用继续有效至到期。
- 删除文档只设置 `documents.del_flag=true`：新的列表、按名称读取、去重、覆盖存在检查和搜索过滤掉该记录；不撤销旧 artifact UUID，不物理删除当前源文件、产物或 Qdrant 向量。重新上传该名称成功发布后清除 del_flag。搜索结果仍受 Redis 数据存续与 TTL 约束。
- UUID 不是访问凭证；每次请求仍执行现有 API Key/namespace 鉴权。权限撤销会阻止后续读取。

## 保存与取回

```text
服务端 MCP search → MySQL search_tasks 提交即受理（返回 result_id，不等待 Kafka）
                                             ↓
             Debezium 任务连接器 → Kafka schema 3（仅 result_id/namespace/schedule_version）
                                             ↓
                         rag-search-worker → 原有检索/重排
                                             ↓
          MySQL search_result_snapshots（事实来源）+ Redis 读取缓存
                                             ↓
等待内完成：完整结果 + result_id；否则：仅 result_id → get_search_result 轮询

异步文档新增（MCP add_document）→ MySQL 单事务（document_jobs 完整任务含文件字节）
                                  → 提交即返回 artifact_id
                                             ↓
          Debezium 任务连接器 → Kafka schema 4 纯通知（仅标识 + schedule_version）
                                             ↓
                  rag-document-worker → 从 MySQL 读回任务并解析/索引
                                             ↓
             get_document_status 直接读 MySQL（无 Redis TTL）；succeeded 表示已发布

REST /v1/search / 本地调试 → 同步检索/重排（不经搜索任务队列）
结果按 ID 读取 MySQL（Redis 缓存未命中时回源）；artifact 按 ID 读取 MySQL；不调用搜索或模型
```

每次搜索生成独立 UUIDv4 result_id，不按查询文本匹配。MCP 的任务与最终快照使用同一 UUID。**两条路径都是 MySQL 提交即受理**：排队搜索只写 `search_tasks` 并返回 `result_id`（不再等待 Kafka producer 确认，也不因投递失败回滚任务），参数由 worker 按 `result_id` 回 MySQL 读取；异步文档新增只提交一个 MySQL 事务（完整任务含文件原字节），提交成功即返回 `artifact_id`，Kafka 只传 schema 4 纯通知，Redis 不参与受理、状态或投递。投递失败不影响受理，由 Debezium 任务连接器补投；"发送确认超时可能不确定"已不再适用于搜索受理。

Redis 保存排队搜索的 namespace、短期任务状态、完整搜索响应和绝对过期时间。key 包含 namespace 哈希及 ID，结果 TTL 与展示缓存独立；UUID 不允许覆盖其他快照。快照保留原结果顺序、分数、证据、debug 元数据及产物引用，分页只切片原快照，不再查 Qdrant。读取不延长绝对过期时间。文档任务状态、租约和文件字节保存在 MySQL，不使用该 Redis 快照，也没有 TTL。

2026-11-13 升级/切换：停旧入口并排空旧 worker 及在途消息，再同时部署 API 与 worker；文档通知为 schema 4、搜索通知为 schema 3，不能与旧 schema 混跑。切换完成后旧 outbox 表可由 `rag-service-migrate --retire-outbox --yes` 删除；`document_tasks` 仍为历史表。存量切换（位点、补投、缓存基线、核对）见 [CDC 迁移契约](cdc-migration.md) §8；旧任务和结果不迁移。日志仅记录 ID 和安全错误类别，不记录查询、文件/消息正文或秘密。

相同 namespace、文档版本、产物类型、名称和字节内容复用稳定 UUIDv5 artifact_id。新的引用可单调延长它的有效期，不缩短已有引用；旧响应里的 expires_at 是该次签发时的保留期限。

## 产物保留方式

MySQL `artifact_snapshots` 保存版本固定的产物 ID、元数据、字节和 expires_at；搜索结果保存在 MySQL `search_result_snapshots`（事实来源，Redis 为读取缓存）。产物字节按需复制一份，以便上传替换、文档删除和旧版本清理不会破坏已经发出的 artifact ID。无需修改业务租约/上传事务或长期阻止旧版本清理；此持久保证不承诺 result_id 在 TTL 清理后一直可读。

搜索命中后可签发：

- 源文件 source；
- PDF/Word 的 document.md、manifest.json、document.json、ocr.json（存在时）；
- 命中图片的原字节；
- 命中表格的结构化 JSON。

只注册实际存在且在限制内的内容，不伪造缺失产物。图片/表格在原 asset 上增加 artifact_id、resource_uri、expires_at；每条命中及顶层 artifacts 提供文件/解析产物引用。旧文档无 upload_version 时按 legacy 数据读取，不把旧命中指向新版本源文件。

捕获期间不持有写租约，因此不会因为另一个节点上传就阻止搜索旧的已提交版本。如果原源文件恰好在捕获前被其他节点删除，可能无法签发其文件引用；已经成功注册并返回的 artifact_id 才具有保留保证。命中的完整文本/图片/表格仍保留在结果快照中。

服务后台恢复循环按批清理到期的 MySQL `artifact_snapshots` 与 `search_result_snapshots`；Redis 缓存随 TTL 失效。到期判断不依赖后台清理是否已执行。文档软删除不撤销已签发 artifact；del_flag 记录和源文件、向量不会被这些 TTL 物理删除。旧上传版本仍可按原事务 GC 清理，已签发产物有独立字节副本，不受影响。

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

同步 REST 搜索若 Redis 保存失败或快照超过限制，仍返回原搜索结果并可标记 snapshot_available=false / snapshot_error；正式结果已写入 MySQL（事实来源）。MCP 排队任务只有 MySQL 快照提交成功后才置 `succeeded`，不以未保存的结果冒充完成快照；Redis 写失败只影响读取缓存，可由回源重建。已成功签发的 MySQL artifact 引用仍可用。单个产物保存失败或超过限制时，artifact_warning 提示部分引用不可用，不影响原结果。

### 结果分页

`GET /v1/search-results/{result_id}?page=1&page_size=10`

- page 从 1 开始，page_size 为 1..100。
- 返回原响应元数据及这一页 results、total_results、next_page。
- 结果不存在、跨 namespace、已过期（超过 `expires_at` 且 MySQL 快照已清理）：410；不自动重跑。Redis 丢失或暂不可用不属于不可用：回源 MySQL 并尽力回填。需要重新提交时会生成新的结果，而不是恢复原快照。
- 非法分页/页码超出范围：400 或参数校验 422。

### 下载与分块

- `GET /v1/artifacts/{artifact_id}`：直接下载原字节，带安全 Content-Disposition。
- `GET /v1/artifacts/{artifact_id}/content?offset=0&length=65536`：JSON base64 字节片段。
- offset 为字节位置；length 为 1..1048576，默认 65536。
- envelope 包含 data、encoding=base64、total_bytes、offset、next_offset、mime_type 和 expires_at。
- 文件不存在、过期或跨 namespace：410。不会回源已删除文档或重新解析。

## MCP

不修改 MCP 标准，只扩展本项目工具与资源：

- 服务端 `search`：提交只写 MySQL `search_tasks` 并返回 `result_id`（提交即受理，不等待 Kafka 确认）；新增 `wait_seconds`，必须为有限数值（支持小数），0..30，默认 10，只是客户端等待结果的时间，不是任务 deadline。在等待内完成返回原完整 payload、同一任务 UUID `result_id` 和产物引用；否则**仅**返回 `{"result_id":"<UUID>"}`。REST `/v1/search` 和本地同步调试不变。
- 扩展现有 `get_search_result`（不重复注册新工具）：参数 result_id，以及可选 page/page_size。任务未完成返回 `{"result_id":"<UUID>","status":"queued","stage":"queued"}`；执行中或等待重试仍为 `status="queued"`，stage 分别为 `processing` / `retry_wait`。任务状态直接读取 MySQL `search_tasks`；Redis 仅缓存结果快照，丢失时回源 MySQL，超过 TTL 清理后不可用。
- 成功后 `get_search_result` 返回 Redis 完整快照（可分页），沿用原来的 ImageContent 渲染，不重新检索。执行最终失败、任务 deadline 到达、Redis 数据丢失或结果过期返回 MCP `isError=true`，不能当作成功的空结果。
- Resource 模板：`rag://artifacts/{namespace}/{artifact_id}{?offset,length}`。
- read_resource 返回 JSON 分块 envelope，next_uri 指向下一段；按 next_uri 读取并 base64 解码、拼接可恢复文件。
- 原 rag://knowledge 文档资源和工具启停管理不变。
- 过期/不可用通过明确 MCP 错误返回；调用工具仍遵守管理端启停。

## 配置与限制

```text
RAG_KAFKA_BOOTSTRAP_SERVERS=127.0.0.1:9092
RAG_KAFKA_CLIENT_ID=praxis-rag
RAG_KAFKA_SECURITY_PROTOCOL=PLAINTEXT
RAG_SEARCH_TOPIC=praxis_search
RAG_SEARCH_GROUP=praxis_search_workers
RAG_SEARCH_TIMEOUT_SECONDS=120
RAG_SEARCH_MAX_ATTEMPTS=5
RAG_SEARCH_DLQ_TOPIC=praxis_search_dead_letters
RAG_SEARCH_DLQ_GROUP=praxis_search_dead_letters
RAG_RESULT_TTL_SECONDS=3600
RAG_RESULT_MAX_BYTES=16777216
RAG_ARTIFACT_MAX_BYTES=67108864
RAG_RESULT_MAX_ARTIFACTS=100
```

- MCP 搜索必须运行独立 `rag-search-worker`；没有消费者会排队直至 deadline 并收敛为 `timed_out`。每个消费进程默认顺序执行，文档、搜索、缓存刷新使用独立 topic/group；worker 与 API 共享 MySQL、Kafka、Qdrant 和状态加密设置，Redis 可选（结果读取缓存）。文档异步新增另需 `rag-document-worker`，三类通知均由 Debezium 从 MySQL 表级采集投递（任务连接器/缓存连接器），应用不写 outbox。
- 任务默认总超时 120 秒，包含排队等待，与 `wait_seconds`（默认 10 秒）和完成快照的 3600 秒 TTL 独立。生产检索在子进程执行；Kafka 消费失败在同一进程内指数退避重试（100ms 起、上限 5s），不承诺立即取消正在执行的检索或下游调用。
- Kafka 通知只携带定位标识（`result_id/namespace/schedule_version`），参数回 MySQL 读取；结果快照与成功终态在**同一 MySQL 事务**提交，Redis 只作读取缓存。消费语义：成功提交 offset；临时故障在同一进程内重试同一条记录；达到 `RAG_SEARCH_MAX_ATTEMPTS` 后原始消息写入 `RAG_SEARCH_DLQ_TOPIC` 再提交；至少一次。旧 `schedule_version`/终态消息安全跳过；Redis 丢失不影响持久去重与结果事实。
- 文档消息只含 schema 4 通知标识（`artifact_id/namespace/schedule_version`）；worker 按 ID 从 MySQL 读回任务与文件字节，用 SQL 租约 claim 并心跳续租，只有 MySQL 记录终态才提交 offset；瞬时失败在同一进程内重试，达到 `RAG_DOCUMENT_BROKER_MAX_RETRIES`/`RAG_DOCUMENT_MAX_ATTEMPTS` 后写入 `RAG_DOCUMENT_DLQ_TOPIC` 再提交（至少一次）。旧版本/终态/有效租约通知安全跳过。没有 Redis 任务状态或重组 TTL，也没有 Redis+MySQL 原子事务：文档 `succeeded` 就是 MySQL 持久发布结果。
- 文档重投通过 UploadJournal 对账，不重复发布；MySQL 仍保存业务操作/租约、fencing、补偿与恢复。
- 独立 `rag-dead-letter-worker --kind search|document|cache` 把尚未终结的任务标记失败：搜索写 MySQL `search_tasks`、文档写 MySQL `document_jobs`，成功写入后才提交 offset，不加载模型或执行推理；cache 只记录告警。死信 topic 是显式的 `RAG_*_DLQ_TOPIC`（不再使用 `%DLQ%<源消费组>`），三个实例各自使用独立的 `RAG_*_DLQ_GROUP`。历史 Kafka 3.9.0 + Debezium 3.7.1 的 CDC 实机验证针对已下线的 outbox 链路；当前双连接器架构未实机验证，见 [VALIDATION](../VALIDATION.md)。
- 结果快照默认最大 16 MiB（MySQL 快照受 `RAG_RESULT_MAX_BYTES` 约束）；保持原图片兼容响应。同步 REST 搜索快照过大时仍可返回原结果并标记快照不可用；MCP 排队任务不能以未写入 MySQL 的结果冒充已完成快照。
- 每次搜索默认至多签发 100 个产物，累计字节预算 64 MiB；同一搜索的重复产物不重复计费。
- 单个 artifact 默认也受 64 MiB 上限约束，MySQL max_allowed_packet 必须满足实际产物大小；默认 Compose 为 64 MiB，接近上限的大对象可能因协议开销失败并提示引用不可用。
- Redis 使用已有连接配置和前缀；结果读取缓存不采用展示缓存的 revision 水位（结果事实在 MySQL）。Redis 丢失可由 MySQL 回源重建；TTL 到期后 MySQL 快照被有界清理，不会通过 Kafka 自动重跑已完成结果，客户端需要重新提交。不要将 MySQL artifact 保留保证与 result_id 的 TTL 混淆。
- 此版本不增加前端快照历史/下载入口；现有搜索 UI 继续使用完整结果。

## 验证

`go test -count=1 ./internal/searchrefs/... ./internal/state/...` 覆盖结果快照、artifact 注册/读取、软删除引用与 UUID 协议。

历史 Python 测试已随后端删除。2026-11-13 改造后结果快照改由 MySQL 事实来源 + Redis 读取缓存，相关用例在 `internal/state`/`internal/searchrefs` 定向测试覆盖；双 Debezium 连接器、真实 Redis 回源与切换演练尚未实机验证，详见 [VALIDATION](../VALIDATION.md)。
