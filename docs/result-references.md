# 产物引用（artifact references）

> **主题变更（2026-11-13 搜索同步化改造）**：搜索结果快照（`result_id`、轮询、`get_search_result`、`GET /v1/search-results/{result_id}`）已随排队搜索下线。MySQL 表 `search_result_snapshots` 与 `search_tasks` **保留历史数据、不再读写**，物理删除另行安排。本文现在只描述 **artifact 引用**（源文件、图片、表格、解析产物）的签发、读取与保留承诺。
> 迁移与切换步骤见 [部署指南](DEPLOYMENT.md#12-搜索同步化迁移与切换) 与 [CDC 迁移契约 §12](cdc-migration.md#12-搜索同步化迁移入口)；历史设计见 [GO_MIGRATION](GO_MIGRATION.md) 补注。

## 1. 已确认的规则

- 搜索**一次调用同步返回**完整结果与 `artifacts`，响应不再包含 `result_id`、`created_at`、`expires_at`、`snapshot_available`、`snapshot_error`；客户端只从本次响应取证据，没有“按 ID 再读结果”的路径。
- 产物引用以 MySQL `artifact_snapshots` 为事实来源；`RAG_RESULT_TTL_SECONDS`（默认 **3600 秒**）现在只表示 **artifact 保留期**（变量名保持不变以兼容既有部署，语义已更正）。
- 已签发的源文件、图片、表格、解析产物引用绑定具体版本；原文档更新、删除后，旧引用继续有效至到期。
- 删除文档只设置 `documents.del_flag=true`：新的列表、按名称读取、去重、覆盖存在检查和搜索过滤掉该记录；不撤销旧 artifact UUID，不物理删除当前源文件、产物或 Qdrant 向量。重新上传该名称成功发布后清除 del_flag。
- artifact UUID 不是访问凭证；每次请求仍执行现有 API Key/namespace 鉴权，权限撤销会阻止后续读取。
- 搜索不再写 MySQL `search_result_snapshots`，也不再写 Redis transient/ZSET；Redis 只保留读模型展示缓存（见 [状态缓存](state-cache.md)）。

## 2. 搜索内联（同步）

```text
POST /v1/search（REST/Console/本地调试）或 MCP search
  → runtime.Service.Search → SearchEngine.Search（检索 + 重排）
  → searchrefs.Capture 只做 artifact 投影（不再保存结果快照）
  → 同一次响应返回 results / artifacts / evidence_status / answer_guidance / debug
```

响应示例（字段与当前实现一致，注意**没有**快照字段）：

```json
{
  "query": "E03 供水不足",
  "retrieval": "hybrid",
  "rerank": "cross_encoder",
  "candidate_k": 10,
  "top_k": 5,
  "evidence_status": "found",
  "answer_guidance": "",
  "results": [],
  "artifacts": [],
  "artifact_limits": {"max_count": 100, "max_bytes": 67108864},
  "debug": {}
}
```

单个产物保存失败或超过限制时，`artifact_warning` 提示部分引用不可用，不影响原结果返回；`artifact_limits` 描述本次上限。

## 3. 产物保留方式

- MySQL `artifact_snapshots` 保存版本固定的产物 ID、元数据、字节和 `expires_at`；产物字节按需复制一份，因此上传替换、文档删除和旧版本清理不会破坏已经发出的 artifact ID。
- 维护循环按批清理到期的 `artifact_snapshots` 与已下线的结果快照表遗留行（只清理 artifact 表；结果表只保留、不读写）。到期判断不依赖后台清理是否已执行。
- 相同 namespace、文档版本、产物类型、名称和字节内容复用稳定 UUIDv5 artifact_id；新的引用可单调延长有效期，不缩短已有引用。
- 搜索命中后可签发：源文件 source；PDF/Word 的 `document.md`、`manifest.json`、`document.json`、`ocr.json`（存在时）；命中图片的原字节；命中表格的结构化 JSON。只注册实际存在且在限制内的内容，不伪造缺失产物。
- 捕获期间不持有写租约，不会因为另一个节点上传就阻止搜索旧的已提交版本；旧文档无 upload_version 时按 legacy 数据读取，不把旧命中指向新版本源文件。

## 4. REST

所有接口沿用 `Authorization` 与 `X-RAG-Namespace`，响应不允许浏览器公共缓存。

### 搜索

`POST /v1/search` 保留原 body 与结果语义，**同步返回** `results`/`artifacts`/`evidence_status`/`answer_guidance`/`debug`；不返回任务 ID 与快照字段。校验失败 422、无读权限 403、运行时/模型错误 503（错误映射保持现状）。

### 产物下载与分块

- `GET /v1/artifacts/{artifact_id}`：直接下载原字节，带安全 `Content-Disposition`。
- `GET /v1/artifacts/{artifact_id}/content?offset=0&length=65536`：JSON base64 字节片段。
- offset 为字节位置；length 为 1..1048576，默认 65536。
- envelope 包含 `data`、`encoding=base64`、`total_bytes`、`offset`、`next_offset`、`mime_type` 和 `expires_at`。
- 文件不存在、过期或跨 namespace：410（`apierrors.SnapshotUnavailable` 常量保留，仅用于 artifact 过期/不可用映射）。不会回源已删除文档或重新解析。

### 已删除的端点

`GET /v1/search-results/{result_id}` 已删除（路由、handler、OpenAPI 声明与 `no-store` 路径前缀一并移除）。旧客户端持有历史 `result_id` 时一律视为不存在，需要证据请重新发起同步搜索。

## 5. MCP

MCP 标准不变，工具集合固定 5 个：`search`、`add_document`、`update_document`、`delete_document`、`get_document_status`。

- 服务端 `search` **永远同步**：一次调用完成检索 + 重排并直接返回证据（`results`/`artifacts`/`evidence_status`/`answer_guidance`/`debug`）。没有 `wait_seconds`、没有 `result_id`、没有轮询；`get_search_result` 工具已删除。
- 权限与文档范围过滤保留：逐请求 scope 头 → `ResolveDocumentIDs` → 检索 `DocumentIDs`；受限 scope 现在会真正执行同步检索（原先在排队模式下被拒绝）。
- 图片资产沿用原有 ImageContent 渲染；`resources/templates/list` 提供 `rag://artifacts/{namespace}/{artifact_id}{?offset,length}`，`resources/read` 返回 JSON 分块 envelope，`next_uri` 指向下一段。
- 原 `rag://knowledge` 文档资源与工具启停管理不变；过期/不可用通过明确 MCP 错误返回。

## 6. 配置与限制

```text
RAG_SEARCH_TIMEOUT_SECONDS=120   # 同步检索总预算（含检索/重排；不再含排队）
RAG_RESULT_TTL_SECONDS=3600      # artifact 保留期（变量名保留）
RAG_RESULT_MAX_BYTES=16777216
RAG_ARTIFACT_MAX_BYTES=67108864
RAG_RESULT_MAX_ARTIFACTS=100
```

- 已移除的搜索配置：`RAG_SEARCH_TOPIC`、`RAG_SEARCH_GROUP`、`RAG_SEARCH_DLQ_TOPIC`、`RAG_SEARCH_DLQ_GROUP`、`RAG_SEARCH_MAX_ATTEMPTS`、`RAG_SEARCH_TASK_LEASE_SECONDS`、`RAG_SEARCH_RETRY_SECONDS`、`RAG_SEARCH_WORKER_CONCURRENCY`。含这些旧键的既有 `config.toml` 不会导致启动失败（键名保留为已废弃、忽略），但不再生效。
- 每次搜索默认至多签发 100 个产物，累计字节预算 64 MiB；同一搜索的重复产物不重复计费。
- 单个 artifact 默认受 64 MiB 上限约束，MySQL `max_allowed_packet` 必须满足实际产物大小；接近上限的大对象可能因协议开销失败并提示引用不可用。
- Redis 仅承担读模型展示缓存；artifact 事实在 MySQL，清空 Redis 不影响已签发引用。
- 此版本不增加前端产物历史/下载入口；现有搜索 UI 继续使用完整结果。

## 7. 历史与迁移

- 排队搜索与结果快照链路（`search_tasks`、`search_result_snapshots`、搜索 topic/消费组/DLQ、`rag-search-worker`、MCP `get_search_result`、`wait_seconds`）已于 2026-11-13 搜索同步化改造下线。
- `search_tasks`、`search_result_snapshots` 两张表保留历史数据、不再读写，物理删除另行安排（与 `document_tasks` 先例一致）。
- 旧客户端调整、在途任务处理、服务切换与回滚路径见 [部署指南 §12](DEPLOYMENT.md#12-搜索同步化迁移与切换)。
- 正常自动 schema 创建缺失的 artifact 表；兼容升级为既有 documents 表添加非空布尔 `del_flag`（默认 0，保留旧行）。禁用自动 schema 的生产部署需先建 artifact 表并执行：

```sql
ALTER TABLE documents ADD COLUMN del_flag BOOLEAN NOT NULL DEFAULT 0;
```

## 8. 验证

```powershell
go test -count=1 ./internal/searchrefs/... ./internal/state/...
```

覆盖 artifact 注册/读取/分块、软删除引用、版本固定与 UUID 复用。结果快照相关用例已随功能删除；历史验证记录见 [VALIDATION](../VALIDATION.md)（标注“已过期”的条目仅作存档）。
