# 功能清单

> 本文件是根 `AGENTS.md` 的拆分细节：当前实际提供的功能。根文件只保留常驻系统提示词的精简索引。
> 校对基准：仓库代码 + `docs/`（2026-11-11）。

## 1. 知识库（namespace）与租户隔离

- 一个知识库 = 一个 namespace，命名规则 `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`；文档、切分配置、Embedding/Reranker/VLM 连接、Qdrant collection 与维度都按知识库保存。
- 所有租户请求（REST `/v1/*`、MCP）必须携带 `X-RAG-Namespace`；Qdrant 查询按 payload 中的 `namespace` 过滤，不同知识库互不可见。
- 知识库默认归创建者私有；共享通过「权限管理」页的邀请链接完成（见 §10）。
- 每个知识库的 Qdrant 集合由配置决定，默认 `RAG_COLLECTION=mini_agent_knowledge_pdf_assets_v1`；dense 向量维度固定 1024（BGE-M3），其他维度启动即报错。

## 2. 文档导入与异步任务

- 支持格式：`.md`、`.txt`、`.pdf`、`.docx`、`.doc`（DOC 需外部 LibreOffice）；每批最多 10 个文件，单文件最大 10 MiB。
- **异步受理**：`POST /v1/document-batches` 为每个文件在一个事务内写入 `document_jobs`（含文件字节、sha256、配置版本、`schedule_version=1`），以 `202` 返回批次和逐文件结果；解析/切分/Embedding/发布在 worker 后台完成。任务表由 Debezium 任务连接器采集，不再写事务 outbox。
- 上传前可用 `POST /v1/documents/check` 做同名/SHA-256 预检；`Idempotency-Key` 支持重试复用原批次及其文件任务。
- **批次上传（Console 使用）**：`POST /v1/document-batches` 先提交 `document_batches` + `document_batch_items`（每项 `saving`），再按文件名加锁并行受理每个文件（不同名互不排队，默认 6 路），最后把每项结果与计数写回并返回 `202`。`GET /v1/document-batches/{batch_id}` 同时给出保存结果（`accepted`/`skipped`/`failed` + 原因）与已受理文件的 `job.status/stage`；`GET /v1/document-batches?limit=20` 只返回仍有未完成工作的批次，供 Console 刷新后恢复进度气泡。批次只是进度元数据，不保存文件字节，7 天后清理。
- 任务状态：`queued` → `processing`（带 `stage`）→ `succeeded` / `failed`；`stage` 依次为 `parsing`、`chunking`、`staging`、`embedding`、`indexing`、`publishing`、`completed`/`failed`。只有 `succeeded` 表示已持久发布、可检索。
- 冲突规则：同名文档已存在、或已有同名排队/执行中的新增，会被拒绝；`add_document` 不覆盖同名文档，替换用 `update_document`。
- 失败重试：同进程内指数退避（默认最多 5 次），仍是“至少一次”投递；耗尽后进入显式 DLQ topic，由 `rag-dead-letter-worker` 落终态。崩溃恢复：任务租约 + 心跳 + 上传 journal 对账，重复投递靠幂等吸收。
- 上传/切分参数来自知识库配置，**不接受请求级覆盖**。

## 3. 文档解析

| 格式 | 引擎 | 产物/说明 |
| --- | --- | --- |
| PDF（vision，默认） | 独立 `pdf-parser` 微服务 + 远端视觉模型 | 本地 PyMuPDF 读取/渲染/裁图，版面分析、表格结构（行列/表头/合并单元格）、OCR、公式 LaTeX、图片描述统一由知识库配置的远端 OpenAI 兼容视觉模型完成；返回 zip：根 `manifest.json`（版本 `docling-service-v1`，证据格式契约）+ 图片资产 |
| PDF（`RAG_PDF_ENGINE=go`） | Go 进程内纯文本引擎 | 按页抽正文、按基线分行、合并上标、去页眉页脚/页码、修复连字符换行；清单版本 `go-native-v1`；不支持版面/表格/OCR/公式/图片资产，扫描件会明确报“没有可索引的内容” |
| DOCX | Go 进程内 OOXML 读取 | 保留内容控件、嵌套表格、合并单元格、原生公式（OMML→LaTeX）、图注/脚注关联；无法可靠读取的结构会明确拒绝入库 |
| DOC | 外部 LibreOffice（`RAG_WORD_CONVERTER_PATH`）转换后再按 DOCX 处理 | 超时 `RAG_WORD_CONVERT_TIMEOUT`（默认 120s） |
| Markdown/TXT | 直接读取 | 直接进入切分 |

- **vision 引擎的强约束**：知识库必须已在 Console「模型配置」中添加并选中 VLM，否则 PDF 上传会在**受理阶段**被拒绝并提示配置，不进入队列；VLM 有 `RAG_VLM_*` 环境回退。同一个视觉模型同时承担 PDF 版面/OCR/表格/公式/图片描述与 Word 无图注图片描述。OCR 已并入视觉模型，不再有独立 OCR 模型类型；升级迁移会删除旧 `kind='ocr'` 配置并清除知识库中的 `ocr`/`ocr_model` 设置。旧值 `RAG_PDF_ENGINE=docling` 等价于 `vision`。
- 解析上限：单文档最多 1000 页、解压后 50 MiB、最多 10000 个 zip 成员、500 万字符；pdf-parser 请求体默认上限 100 MiB。
- 安全拒绝：非 `%PDF-` 文件、明显加密的 PDF、空抽取结果都会返回明确错误，不会静默入库垃圾文本。
- 解析产物（`manifest.json`、`document.md`、图片、表格、图注/脚注）保存在共享状态库；检索命中时返回图片 Base64、表格行列/表头/合并信息与页码/版面或 Word 章节位置。
- 图片/表格可经 `/v1/artifacts/{artifact_id}`、`/v1/artifacts/{artifact_id}/content` 读取。

## 4. 切分

| 策略 ID | 名称 | 单位 | 默认块长/重叠 |
| --- | --- | --- | --- |
| `character` | 按字符 | characters | 700 / 100 |
| `token` | 按 token | tokens | 512 / 64 |
| `sentence` | 按句子 | tokens | 512 / 64 |
| `recursive` | 递归切分（默认） | tokens | 512 / 64 |
| `structure` | 按文档结构 | tokens | 512 / 64 |

- 校验：块长 1..100000、重叠 ≥0 且小于块长、字符策略块长 ≥100；未知策略报错。
- 表格行按内容边界处理，超模型输入上限时明确报错；表格同时作为表格资产保留。
- **已知差异**：Go 使用确定性近似 token 计数（不是 BGE-M3 SentencePiece），句子/段落/结构边界与 Python 一致，但 token 预算下的切分位置可能不同。

## 5. 检索与重排

- 检索模式：`bm25`（关键词）、`embedding`（语义）、`hybrid`（默认，Qdrant 服务端 RRF 融合 dense + sparse）。
- 重排模式：`none`、`cross_encoder`（默认）；Cross-Encoder 通过 HTTP 调用：进入重排的候选上限 30、批大小 4、重排文本上限 4000 字符、模型 token 上限 512。
- 参数：`query` ≤2000 字符；`candidate_k` 1..100（默认 10）；`top_k` 1..10（默认 5）且 ≤`candidate_k`；`filters` 仅允许 `document_id`、`source`、`title`（闭集白名单）。
- 名词解释：`dense` 命名向量（Cosine，1024 维）+ `sparse` 命名向量（BM25，确定性哈希词表，IDF 由 Qdrant 提供）；hybrid 先各自 prefetch，再服务端 RRF。
- 检索结果只返回**活跃版本**的 chunk（按 `upload_version` 过滤），并按 namespace 隔离；受限调用方会在服务端追加 `document_ids` 过滤，不能由客户端伪造。
- 结果信封：`query`、`retrieval`、`rerank`、`candidate_k`、`top_k`、`evidence_status`（`found` / `no_evidence`）、`answer_guidance`、`results[]`、`debug{implementation,candidate_count,returned_count,retrieval_ms,rerank_ms,limitations}`。
- 每条结果：`rank`、`document_id`、`chunk_id`、`title`、`source`、`text`、`score`、`metadata`；命中资产时附 `asset`（图片 base64 或表格结构）与 `artifacts` 引用。
- MCP `search` **永远同步**：一次 `tools/call` 完成检索 + 重排并直接返回证据；没有 `wait_seconds`、没有 `result_id`、没有轮询，`get_search_result` 工具已删除。REST `/v1/search` 与 MCP 同语义。
- 搜索总预算复用 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120s）：在 `Service.Search` 内基于父 ctx 派生 deadline（已有更早 deadline 不延长）；客户端断开/请求取消会向下传递到 Qdrant 与模型调用。

## 6. artifact 与引用

- artifact 用于引用源文件、图片、表格 JSON 与解析产物：单 artifact 上限 64 MiB，单次检索最多 100 个，超限时结果带 `artifact_warning`。
- artifact 以 MySQL `artifact_snapshots` 为事实来源，按 ID 独立读取（`GET /v1/artifacts/{artifact_id}` 等），保留期由 `RAG_RESULT_TTL_SECONDS`（默认 3600 秒，语义为 artifact TTL）决定。
- 搜索结果快照已下线：响应不再有 `snapshot_available`、`result_id`、`created_at`、`expires_at`；`search_tasks`/`search_result_snapshots` 为历史表、保留数据但不再读写。
- 删除文档是**逻辑删除**（`del_flag`），不会立即物理删除源文件与 Qdrant 向量；已签发 artifact 继续有效至到期。

## 7. MCP 服务（`/mcp`）

- 端点：`/mcp`（Streamable HTTP，POST/GET/DELETE）；**始终要求** `Authorization: Bearer <service token 或 API Key>`（loopback 也不例外），另需 `X-RAG-Namespace`。
- 协议：旧版 `2025-11-25`（initialize → initialized → tools/list/call）与新版 `2026-07-28`（请求自描述、无状态）由 SDK 按请求分发；两类客户端不能混用版本号。
- 工具（名称是稳定契约，不得改名）：

| 工具 | 参数要点 | 行为 |
| --- | --- | --- |
| `search` | `query`、`retrieval`、`rerank`、`candidate_k`、`top_k`、`filters` | 同步检索并直接返回证据；无 `wait_seconds`/任务 ID |
| `add_document` | `name` + `content`/`content_base64`，可选 `idempotency_key` | 异步入库，成功只返回 `artifact_id` |
| `get_document_status` | `artifact_id` | 查询 `status/stage/error` |
| `update_document` | `name` + `content`/`content_base64` | 同步替换并重建索引 |
| `delete_document` | `name` | 同步逻辑删除（destructive） |

- 资源：`resources/list` 列出文档（`rag://knowledge/<namespace>/<文件名>`），`resources/read` 按段落分页返回 JSON（每页最多 8000 字符，含 `page/total_pages/version/truncated/next_uri`）；另有 artifact 资源模板。
- 提示词：没有内置模板，全部来自命名空间级 catalog（`{{argument}}` 占位符，仅替换不执行表达式），可经 `/v1/admin/prompts` 管理；`prompts/list` / `prompts/get` 使用该数据。
- 订阅：旧版 `resources/subscribe` 与新版 `subscriptions/listen`；变更通知只表示“发生了变化”，客户端需重新 list/read，通知不跨进程重放。
- 工具开关：管理员可经 `/v1/admin/tools` 启停工具、覆盖描述；禁用后 `tools/list` 隐藏、调用返回 `Unknown or disabled RAG tool: <name>`。
- 错误语义：工具失败是 `CallToolResult.isError` 文本，不是 JSON-RPC error；受限权限会真正执行同步检索（服务端按 scope 追加 `document_ids` 过滤）。
- 安全：Host 白名单（不匹配 421）、Origin 校验（不匹配 403，缺省允许）、Content-Type 校验、请求体上限（约 60 MiB + 64 KiB）。

## 8. REST API（`/v1/*`）

按用途分组（完整参数示例见 `docs/USAGE.md`；`/openapi.json` 与 `/docs` 提供 OpenAPI 3.1 契约）：

| 分组 | 路由（方法 + 路径） |
| --- | --- |
| 基础设施 | `GET /health`、`GET /openapi.json`、`GET /docs`、`GET /`、`GET /console`、`GET /console/assets/*` |
| 认证 | `GET /v1/auth/status`、`POST /v1/auth/register`、`POST /v1/auth/login`、`POST /v1/auth/logout`、`GET /v1/auth/me` |
| 租户：检索/结果 | `POST /v1/search`、`GET /v1/artifacts/{artifact_id}`、`GET /v1/artifacts/{artifact_id}/content` |
| 租户：文档 | `GET /v1/documents`、`POST /v1/documents/check`、`GET /v1/document-jobs/{artifact_id}`、`POST /v1/document-batches`、`GET /v1/document-batches`、`GET /v1/document-batches/{batch_id}`、`GET /v1/documents/{name}/content`、`DELETE /v1/documents/{name}` |
| 租户：知识库/状态 | `GET /v1/knowledge-base`、`GET /v1/namespaces`、`GET /v1/chunking/strategies`、`GET /v1/uploads`、`GET /v1/uploads/{operation_id}` |
| Console 会话：知识库/共享 | `GET /v1/users`、`POST /v1/knowledge-bases`、`GET /v1/knowledge-bases/{namespace}/access`、`GET /v1/knowledge-bases/{namespace}/permissions`、`DELETE /v1/knowledge-bases/{namespace}/permissions/{user_id}`、`GET|POST /v1/knowledge-bases/{namespace}/invitations`、`GET /v1/invitations`、`POST /v1/invitations/preview`、`POST /v1/invitations/accept`、`POST /v1/invitations/decline`、`DELETE /v1/invitations/{invitation_id}` |
| 管理：用户 | `GET /v1/admin/users`、`PATCH /v1/admin/users/{user_id}`、`DELETE /v1/admin/users/{user_id}` |
| 管理：知识库 | `GET|POST /v1/admin/knowledge-bases`、`POST /v1/admin/knowledge-bases/models/{kind}`、`GET /v1/admin/knowledge-bases/defaults`、`GET|PATCH /v1/admin/knowledge-bases/{namespace}`、`GET /v1/admin/knowledge-bases/{namespace}/key/{kind}` |
| 管理：模型配置 | `GET|POST /v1/admin/model-profiles`、`GET|PATCH|DELETE /v1/admin/model-profiles/{profile_id}`、`GET /v1/admin/model-profiles/{profile_id}/key`、`POST /v1/admin/model-profiles/{profile_id}/models` |
| 管理：MCP 配置 | `GET /v1/admin/tools`、`PATCH /v1/admin/tools/{name}`、`GET /v1/admin/prompts`、`PUT|DELETE /v1/admin/prompts/{name}` |
| 管理：展示/只读 | `GET /v1/admin/published-versions`、`GET /v1/admin/upload-leases`（注册为无认证，但 handler 内二次校验管理员+本机边界） |
| 管理：监控 | `GET /v1/admin/monitor/overview`（只读聚合：三态总体状态、服务信息、依赖探测（Kafka 元数据 / PDF 健康负载）、文档任务/上传分类队列、两个 Debezium 连接器健康、两组 Kafka 消费组积压、缓存 dirty/failures、文档 Worker 心跳、有效文档计数、24 小时检索活动；不可得的数字显式为 `null`/未知；本机直连或管理员会话） |
| 每账号 API Key | `GET|POST /v1/admin/api-keys`、`PUT|DELETE /v1/admin/api-keys/{key_id}`、`GET /v1/admin/api-keys/{key_id}/mcp-config` |
| MCP | `POST|GET|DELETE /mcp` |

认证边界：

- 租户接口：`Authorization: Bearer <RAG_SERVICE_TOKEN 或 API Key>` + `X-RAG-Namespace`；Console 登录后也可用会话 Cookie（写操作需 `X-CSRF-Token`）。
- `/v1/admin/*`：本机直连（loopback、无代理头、同源 Origin）**或**管理员会话；唯一例外是 `/v1/admin/api-keys`——任意登录账号都可管理**自己的** API Key（非管理员只能绑定本人创建的知识库）。
- 会话共享接口（邀请/权限）只接受 Console 会话，没有 loopback 兜底。
- 未设置 Token 且本机直连时，普通租户接口可用 headers 直接访问（开发便利），生产必须配置 Token。

## 9. Web Console（`/console`）

Hash 路由（namespace 不在 URL 中，保存在 `localStorage`，默认 `default`）：

| 路由 | 页面 | 可见角色 | 主要功能 |
| --- | --- | --- | --- |
| `#/workspaces/search` | 检索调试 | 所有登录账号 | BM25/Embedding/Hybrid 检索、Cross-Encoder 重排开关、候选/Top-K 调整、三模式对比、耗时与证据状态、命中详情（分数、来源、页码/章节、图片、脚注） |
| `#/workspaces/documents` | 知识文档 | 所有登录账号 | 文档列表（大小/块数/切分配置）、上传（批次受理：工具栏徽标 + 逐文件进度气泡，可失败重试；刷新后恢复未完成批次）、删除；只读授权时隐藏写操作 |
| `#/workspaces/documents/{name}` | 文档详情 | 所有登录账号 | 正文预览、PDF 预览（pdfjs）、图片/表格资产查看 |
| `#/workspaces/create` | 创建知识库 | 所有登录账号 | 管理员：namespace + 切分参数 + 从「模型配置」选择 Embedding/Reranker/VLM；VIP/普通用户：简化表单（从默认模板克隆模型配置） |
| `#/knowledge-bases` | 知识库 | 管理员 | 知识库列表、编辑抽屉（模型/切分/重新向量化确认） |
| `#/model-profiles` | 模型配置 | 管理员 | Embedding/Reranker/VLM/JEV 分页签，新增/编辑/删除、模型探测、Key 只写不读 |
| `#/api-keys` | API Key | 所有登录账号 | 自己的密钥 CRUD/启停、一次性明文、复制 MCP 配置 |
| `#/users` | 用户管理 | 管理员 | 角色（admin/vip/user）、启用/禁用、删除；服务端保证至少保留一名启用的管理员 |
| `#/monitor` | 系统监控 | 管理员 | 只读监控快照：页头三态状态与降级告警、关键指标（有效文档/任务/上传/检索）、MySQL/Redis/Qdrant/Kafka/pdf-service 依赖状态（Kafka 校验元数据与主题、PDF 校验转换器）、`document_jobs` 队列与阶段、上传互斥分类、Debezium 任务/缓存两个连接器健康、两组 Kafka 消费组滞后、缓存 dirty/failures、文档 Worker 心跳（读不到显示「未知」）、服务信息；15 秒自动刷新，窄屏单列 |
| `#/permissions` | 权限管理 | 管理员 / VIP | 选择知识库、按用户聚合授权、创建邀请（可读/可修改 × 全库/若干文件）、复制链接、撤销授权/邀请 |
| `#/invitations` | 邀请中心 | 登录账号 | 待处理邀请、粘贴邀请链接、接受/拒绝（顶部铃铛轮询） |
| `#/invite?token=` | 受邀预览 | 登录账号 | 预览邀请并接受/拒绝，随后切换到该知识库 |

- 侧边栏按角色过滤；管理员专属页面在无会话（旧模式）下仍显示，但会话模式下非管理员会被重定向。
- 登录/注册：第一个注册账号自动成为管理员；`RAG_AUTH_REGISTRATION=closed` 可关闭自助注册。

## 10. 账号、角色与共享权限

- 角色只有 `admin`、`vip`、`user`；只有管理员能改角色，且最后一个启用的管理员不能被降级/禁用/删除。
- Console 会话：HttpOnly Cookie `rag_session`（默认 7 天，`RAG_AUTH_SESSION_TTL_HOURS`）；Cookie 会话的写操作要求 `X-CSRF-Token`，Bearer 会话/API Key 不需要。
- 知识库权限：管理员或创建者拥有全部读写；其他人取授权的并集——授权分 `read`/`write`，范围分全库或指定文件；写授权隐含可读。**文件级写授权只能替换已授权的同名文档，不能上传新文件**。
- 邀请：只有管理员与 VIP 可发起（管理员可邀请任意知识库，VIP 只能邀请自己创建的）；邀请链接一次性，数据库只存 token 的 SHA-256 摘要；被邀请人必须存在且启用，不能邀请自己；默认 TTL 168 小时。
- API Key：`prk_` 前缀，明文只在创建时返回一次；按命名空间白名单生效；密钥归创建账号所有，非管理员看不到别人的密钥（返回 404）。

## 11. 模型配置（model profiles）

- 管理员在「模型配置」页维护可复用列表：`embedding` / `reranker` / `vlm` / `jev`；`(kind, name)` 唯一。
- Embedding/Reranker/VLM/JEV 保存 `base_url`、`model`、加密后的 API Key；Embedding 保存时会真实探测并记录 `vector_size`，Reranker 做一次有界探测，VLM 与 JEV 不探测。VLM 是唯一的远端视觉模型，PDF 版面/OCR/表格/公式/图片描述都由它完成；`ocr` 类型已删除（升级迁移会删除旧条目）。
- 知识库保存的是**连接副本**（保存时把 profile 的 URL/模型/Key 复制进知识库配置），之后编辑/删除 profile 不影响已保存的知识库；知识库 REST 契约仍是内联连接对象。
- 凭据加密格式 `enc:v1:`（AES-GCM，12 字节 nonce，`base64(nonce||ciphertext)`），密钥来自 `RAG_STATE_ENCRYPTION_KEY`（`base64:` 前缀的 32 字节）；旧 `dpapi:`/`private-file:` 记录只读兼容。

## 12. 异步队列、死信与缓存

- Kafka 只在配置 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时启用；空值 = brokerless（通知无法投递、文档任务保持 `queued`、缓存刷新不生效）。搜索为同步执行，不受 Kafka 是否配置影响。
- **两个 Debezium 连接器**：任务连接器只采集 `document_jobs`（仅 INSERT 与 `schedule_version` 变化，`skipped.operations=d`）；缓存连接器采集 `knowledge_bases`/`documents`/`mcp_catalogs`/`api_keys`/`document_heads`/`document_leases`/`upload_operations`/`upload_steps`（全部 c/u/d）。应用不再写任何事务 outbox。
- 重发只发生在 MySQL 事务内：`schedule_version = schedule_version + 1`；消费者只处理与数据库当前版本相等的通知，旧版本/终态/有效租约安全跳过并 ACK。任务表初始快照的 `op=r` 事件用于切换补投。
- topic：`praxis_document_ingest`、`praxis_state_cache_refresh`；DLQ 是显式 topic `praxis_document_ingest_dead_letters` / `praxis_state_cache_refresh_dead_letters`（不是 `%DLQ%` 约定），由 `rag-dead-letter-worker --kind document|cache` 消费。原搜索 topic/消费组/DLQ 已下线。
- 缓存消费者按 `cache_revisions` 的 revision 水位刷新：读取事件 → 事务内 `revision+1` → 重建脱敏视图 → 提交后写 Redis；失败记录 `attempts/last_error` 并照常 ACK，由周期 `RefreshDirty` 补偿，Redis 写失败不丢收敛、不会造成 DLQ 风暴。
- 消费语义至少一次：成功提交 offset；失败同进程指数退避重试（100ms 起、上限 5s），达到 `MaxAttempts`（默认 5）后写 DLQ 再提交；重复投递由幂等与 MySQL 租约吸收。
- 文档 worker 只有 Kafka 消费模式：`rag-service` 不内嵌消费者，`rag-document-worker` 消费 Debezium 投递的 `RAG_DOCUMENT_TOPIC`；未配置 Kafka 或未运行该 worker 时，上传仍受理但任务保持 `queued`。
- Redis 用途：只承担读模型展示缓存（短 TTL）；搜索响应与 artifact 事实都不在 Redis（artifact 在 MySQL，按 ID 独立读取）。
- 搜索为同步执行：一次调用完成检索 + 重排并返回证据；总预算由 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120s）约束，客户端取消会终止下游调用。

## 13. 运维与 CLI

- 健康检查 `GET /health`；OpenAPI `GET /openapi.json`（OpenAPI 3.1.0）、交互文档 `GET /docs`。
- `rag-state-migrate [--apply]`：状态库表结构检查/幂等创建；`rag-service-migrate` 默认只读预演，`--apply` 只导入源文件与文档记录，**不重建向量**，需要重新上传才会可检索；`rag-service-migrate --retire-outbox --yes` 显式删除 4 张已下线的 outbox 表。
- `rag-cdc-cutover`：CDC 存量切换 CLI（`--inventory` 盘点、`--record-position` 记录 binlog 位点、`--backfill --baseline=N` 补投、`--mark-cache-baseline` 缓存基线、`--verify --baseline=N` 核对、`--all`），只覆盖 `document_jobs` 与缓存表（搜索任务盘点/回填已随同步化改造删除），详见 [CDC 迁移契约](../cdc-migration.md) §8。
- `rag-mcp-ingest`：递归导入本地文件/目录，输出 `OK/SKIP/FAIL` 与 `SUMMARY`；`--dry-run` 不打开运行时。
- `docker compose up -d` 启动基础设施（MySQL 3307、Qdrant 6333、Redis 6379、Kafka 9092）与 `pdf-parser`（8091）；`--profile app` 构建并启动 Go 服务；默认启动 Debezium CDC。
- `scripts/start-stack.ps1` 一键起依赖、建 topic、迁移并启动 `rag-service` + `rag-document-worker`；`stop-stack.ps1` 停止。
- `scripts/verify-debezium-e2e.py`、`verify-kafka-dlq.py` 是真实基础设施上的验证脚本（需要 Docker 与 Python 3）；原 `verify-kafka-search.py`（MCP 排队搜索）已随功能删除。
