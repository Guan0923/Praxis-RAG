# 功能清单

> 本文件是根 `AGENTS.md` 的拆分细节：当前实际提供的功能。根文件只保留常驻系统提示词的精简索引。
> 校对基准：仓库代码 + `docs/`（2026-11-14）。

## 1. 知识库（namespace）与租户隔离

- 一个知识库 = 一个 namespace，命名规则 `^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`；文档、切分配置、Embedding/Reranker/VLM 连接、Qdrant collection 与维度都按知识库保存。
- 所有租户请求（REST `/v1/*`、MCP）必须携带 `X-RAG-Namespace`；Qdrant 查询按 payload 中的 `namespace` 过滤，不同知识库互不可见。
- 知识库默认归创建者私有；共享通过「权限管理」页的邀请链接完成（见 §10）。个人会员 Free/Plus/Pro 只影响自有库数、每库文件数和平台文档 Embedding 字符额度，不改 `admin/vip/user` 角色；Plus/Pro 可对自己拥有的知识库发邀请。
- 每个知识库的 Qdrant 集合由知识库配置决定（创建知识库时生成 `knowledge_*` 名称，首次入库时创建）；`RAG_COLLECTION` 只是没有知识库配置时的回退名，系统监控按有效文档实际入库的集合校验，不要求回退集合存在，空知识库没有集合也不算异常；dense 向量维度固定 1024（BGE-M3），其他维度启动即报错。

## 2. 文档导入与异步任务

- 支持格式：`.md`、`.txt`、`.pdf`、`.docx`、`.doc`、`.html`、`.htm`；每批最多 10 个文件，单文件最大 10 MiB。`.docx`/`.doc`（先经 LibreOffice 转 `.docx`）与 `.html`/`.htm` 一律由独立的 Docling 解析服务解析（配置 `RAG_DOCLING_SERVICE_URL`）；未配置时 Word/HTML 上传在受理阶段被拒绝（进程内 Word 解析已删除）。
- **异步受理**：`POST /v1/document-batches` 为每个文件在一个事务内写入 `document_jobs`（含文件字节、sha256、配置版本、`schedule_version=1`），以 `202` 返回批次和逐文件结果；解析/切分/Embedding/发布在 worker 后台完成。任务表由 Kafka Connect 的 `praxis-document-cdc` 连接器采集，不再写事务 outbox。上传 POST 可携带可选的 `X-Upload-ID`，与 `GET /v1/document-batches/upload-events/{upload_id}`（SSE）配对实时推送逐文件字节进度与保存结果；缺省该头时行为与响应完全不变。
- 上传前可用 `POST /v1/documents/check` 做同名/SHA-256 预检；`Idempotency-Key` 支持重试复用原批次及其文件任务。
- **批次上传（Console 使用）**：`POST /v1/document-batches` 先提交 `document_batches` + `document_batch_items`（每项 `saving`），再按输入顺序逐项受理每个文件（每项仍有独立的文件名锁与事务），最后把每项结果与计数写回并返回 `202`。顺序裁决保证最后一个文件/知识库名额归属确定，已受理项仍在后台异步运行。`GET /v1/document-batches/{batch_id}` 同时给出保存结果（`accepted`/`skipped`/`failed` + 原因）与已受理文件的 `job.status/stage`；`GET /v1/document-batches?limit=20` 只返回仍有未完成工作的批次，供 Console 刷新后恢复进度气泡。批次只是进度元数据，不保存文件字节，7 天后清理。Console 上传面板为每个文件一行独立进度条并先订阅 SSE（`ready` 后发 POST）：`progress.received` 是请求体累计接收的内容字节数（前端按本地 `File.size` 换算百分比），每文件 `result`（`saved`/`skipped`/`failed`）落地即结束该行；面板在保存结果处结束，不再展示或轮询解析阶段（解析阶段仍由批次状态接口的 `items[].job.status/stage` 暴露）。
- 任务状态：`queued` → `processing`（带 `stage`）→ `succeeded` / `failed`；`stage` 依次为 `parsing`、`chunking`、`staging`、`embedding`、`indexing`、`publishing`、`completed`/`failed`。只有 `succeeded` 表示已持久发布、可检索。
- 冲突规则：同名文档已存在、或已有同名排队/执行中的新增，会被拒绝；`add_document` 不覆盖同名文档，替换用 `update_document`。Console 的「替换文件」调用同步 `POST /v1/documents/{name}/replace`（multipart 字段 `file`）：校验目标文档写权限（全库或单文件授权）、新文件类型与原文档一致，然后复用 `WriteDocument(overwrite=true)` 的替换与重建索引逻辑，保留原名；普通上传规则不变。
- 失败重试：同进程内指数退避（默认最多 5 次），仍是“至少一次”投递；耗尽后进入显式 DLQ topic，由 `rag-dead-letter-worker` 落终态。崩溃恢复：任务租约 + 心跳 + 上传 journal 对账，重复投递靠幂等吸收。
- 上传/切分参数来自知识库配置，**不接受请求级覆盖**。

### 2.1 从飞书导入（可选）

- **授权边界**：部署方配置 `RAG_FEISHU_APP_ID` / `RAG_FEISHU_APP_SECRET` / `RAG_FEISHU_REDIRECT_URI`（三项必须同时存在，否则启动失败；全空则功能禁用）。每个 Console 账号通过 OAuth 连接自己的飞书账号，浏览、搜索、读取正文均使用该账号的 `user_access_token`，不使用应用身份越权。
- **配置项**（`internal/config/feishu.go`）：`RAG_FEISHU_API_BASE_URL`（默认 `https://open.feishu.cn`）、`RAG_FEISHU_ACCOUNTS_BASE_URL`（默认 `https://accounts.feishu.cn`）、`RAG_FEISHU_TIMEOUT_SECONDS`（默认 20，1..300）。回调地址必须与飞书开放平台登记值完全一致；申请 `drive:drive:readonly`、`docx:document:readonly`、`offline_access`。
- **API**（`internal/httpapi/feishu.go`，`internal/feishu/`）：
  - `GET /v1/feishu/status`（会话）：返回是否启用、是否已连接、账号、scope 缺口、是否需要重新授权。
  - `POST /v1/feishu/oauth/start`（会话，CSRF）：创建一次性 state，返回 `authorize_url`。
  - `GET /v1/feishu/oauth/callback`（无路由鉴权，处理函数自行校验会话）：校验 state 后换取 token、读取用户信息并加密保存；302 回 `#/workspaces/documents`，结果用 `?feishu=connected|denied|error` 表达。
  - `DELETE /v1/feishu/connection`（会话，CSRF）：删除本人授权。
  - `GET /v1/feishu/files`（会话）：列根目录或指定 `folder_token`；`docx`/指向 docx 的 `shortcut` 可导入，文件夹可下钻，其他类型给出明确不支持原因。
  - `GET /v1/feishu/search`（会话）：`POST /open-apis/suite/docs-api/search/object`，只请求 `docs_types:["doc"]`；该共享接口同时覆盖新旧文档，旧版 `doc` 在读取正文时按该篇失败返回（能识别为版本过旧时提示不支持）。
  - `POST /v1/feishu/import`（租户 + 会话 + 写 scope，CSRF）：body `{"documents":[{"document_id","title"}]}`，1..10 篇；并发读取上限 4、单篇上下文上限 30 秒（HTTP 客户端默认 20 秒）；按文件名安全转换后复用 `SubmitDocumentBatch`，逐篇返回 `accepted/skipped/failed` 与 job，读取失败不阻断其他文档。
- **状态表**（Go-only，append 在 schema 末尾）：`feishu_connections`（每账号一行，`access_token`/`refresh_token` 用 `enc:v1:` 加密，App Secret 不入库）、`feishu_oauth_states`（只存 state 的 SHA-256 摘要，10 分钟过期，一次性消费，绑定账号）。
- **刷新**：access token 临期或接口返回鉴权错误时用 refresh token 刷新；每进程按账号分片加锁，避免单次性 refresh token 并发失效。刷新失败或 scope 缺失时 `status.needs_reauth=true`，前端提示重新连接。
- **入库语义**：只取 docx `raw_content` 纯文本（空正文/超过 10 MiB 拒绝），生成安全 `.md` 文件名；SHA-256 去重、同名跳过、写权限校验和异步索引全部走普通上传路径。导入结果把「已受理」与「可检索」分开，前端轮询 `GET /v1/document-batches/{batch_id}` 的 job，`succeeded` 才展示为已索引。
- **可见范围**：副本属于目标知识库，检索可见性由 Praxis-RAG 权限决定，与飞书原文档分享范围无关；弹窗在导入前展示目标知识库名和该说明。

## 3. 文档解析

| 格式 | 引擎 | 产物/说明 |
| --- | --- | --- |
| PDF（vision，默认） | 独立 `pdf-parser` 微服务 + 远端视觉模型 | 本地 PyMuPDF 读取/渲染/裁图，版面分析、表格结构（行列/表头/合并单元格）、OCR、公式 LaTeX、图片描述统一由知识库配置的远端 OpenAI 兼容视觉模型完成；返回 zip：根 `manifest.json`（版本 `docling-service-v1`，证据格式契约）+ 图片资产 |
| PDF（`RAG_PDF_ENGINE=go`） | Go 进程内纯文本引擎 | 按页抽正文、按基线分行、合并上标、去页眉页脚/页码、修复连字符换行；清单版本 `go-native-v1`；不支持版面/表格/OCR/公式/图片资产，扫描件会明确报“没有可索引的内容” |
| DOCX | 独立 `docling-service`（`POST /v1/parse`，multipart 字段 `file`，返回 zip：版本化清单 + 序列化 `DoclingDocument` + 图片资产；`document.md` 由 Go 生成）；**必须配置 `RAG_DOCLING_SERVICE_URL`**，未配置时上传在受理阶段被拒绝 | Go 端只校验状态码/content-type、安全解压后切分；本地仍执行 zip 边界校验（条目数/加密/解压大小）后上传；进程内 OOXML 解析已删除 |
| DOC | 外部 LibreOffice（`RAG_WORD_CONVERTER_PATH`）转成 `.docx` 后交给 `docling-service`（同样要求已配置 Docling） | 超时 `RAG_WORD_CONVERT_TIMEOUT`（默认 120s）；服务只接收 `.docx`/HTML，不直接接收 `.doc` |
| HTML / HTM | 必须配置 `docling-service`（未配置时上传被拒绝/任务报 `ErrDoclingNotConfigured`） | 静态 HTML 上传；外部/相对图片不抓取、只保留文字说明（如替代文本）；Console 预览只展示解析后的纯文本，不在同源页面渲染或执行上传的 HTML；不支持网页抓取 |
| Markdown/TXT | 直接读取 | 直接进入切分 |

- **vision 引擎的强约束**：知识库必须已在 Console「模型配置」中添加并选中 VLM，否则 PDF 上传会在**受理阶段**被拒绝并提示配置，不进入队列；VLM 有 `RAG_VLM_*` 环境回退。同一个视觉模型承担 PDF 版面/OCR/表格/公式/图片描述。OCR 已并入视觉模型，不再有独立 OCR 模型类型；升级迁移会删除旧 `kind='ocr'` 配置并清除知识库中的 `ocr`/`ocr_model` 设置。旧值 `RAG_PDF_ENGINE=docling` 等价于 `vision`。
- 解析上限：单文档最多 1000 页、解压后 50 MiB、最多 10000 个 zip 成员、500 万字符；pdf-parser 请求体默认上限 100 MiB。
- 安全拒绝：非 `%PDF-` 文件、明显加密的 PDF、空抽取结果都会返回明确错误，不会静默入库垃圾文本。
- **Docling 服务**：`.docx`/`.doc`（先经 LibreOffice 转 `.docx`）与 `.html`/`.htm` 一律通过该独立服务解析（配置 `RAG_DOCLING_SERVICE_URL`），单次调用超时 `RAG_DOCLING_SERVICE_TIMEOUT`（默认 300 秒，1..7200）；该 URL 为空时 Word/HTML 上传在受理阶段被拒绝并提示配置（进程内 Word 解析已删除，`.md`/`.txt`/`.pdf` 不受影响）。已配置但服务不可达时任务失败并按既有重试处理，不回退进程内解析。服务自身环境变量：`RAG_DOCLING_SERVICE_HOST`、`PORT`（默认 8092）、`RAG_DOCLING_SERVICE_TIMEOUT`、`RAG_DOCLING_SERVICE_PARSE_TIMEOUT`、`RAG_DOCLING_SERVICE_CONCURRENCY`、`RAG_DOCLING_SERVICE_WORKERS`、`RAG_DOCLING_SERVICE_MAX_BYTES`、`RAG_DOCLING_SERVICE_LOG_LEVEL`、`RAG_DOCLING_SERVICE_LOG_FORMAT`；容器端口 8092，健康检查 `/healthz`；依赖版本与启动方式见 `python-workers/docling-service/README.md`。
- **HTML 预览安全**：Console 以 React 文本节点渲染解析后的纯文本，不使用 `dangerouslySetInnerHTML`，上传的 HTML/JS 不会在 Console 同源页面执行。
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

- 检索模式：`bm25`（关键词）、`embedding`（语义）、`hybrid`（默认，Qdrant 服务端 RRF 融合 dense + sparse）、`sag2`（词法多跳：BM25 初筛 → JEV 必要证据判定 → 词项扩展再召回 → DPP 终排）。
- `sag2` 约束：必须搭配 `rerank: none`（JEV 证据分即最终排序，校验层直接拒绝 `cross_encoder` 组合）；需要管理员在「模型配置」保存 `kind=jev` 条目，否则检索返回可解释的操作错误；语料在进程内按可见版本指纹缓存（发布/覆盖/删除自动失效），首次查询或失效后重建索引需要全量读取活跃 chunk；`candidate_k` 同时是每轮召回预算（SAG2 参考实现 B=100，建议给 50..100），轮数固定 3（1 轮初筛 + 2 轮扩展），扩展词 ≤20、种子 ≤10、终排前 10 名做 DPP；每次查询约 3 轮 JEV 决策调用，延迟与费用显著高于其他模式。
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
| 租户：文档 | `GET /v1/documents`、`POST /v1/documents/check`、`GET /v1/document-jobs/{artifact_id}`、`POST /v1/document-batches`、`GET /v1/document-batches`、`GET /v1/document-batches/{batch_id}`、`GET /v1/document-batches/upload-events/{upload_id}`（SSE）、`GET /v1/documents/{name}/content`、`DELETE /v1/documents/{name}` |
| 租户：知识库/状态 | `GET /v1/knowledge-base`、`GET /v1/namespaces`、`GET /v1/chunking/strategies`、`GET /v1/uploads`、`GET /v1/uploads/{operation_id}` |
| Console 会话：知识库/共享 | `GET /v1/users`、`POST /v1/knowledge-bases`、`GET /v1/knowledge-bases/{namespace}/access`、`GET /v1/knowledge-bases/{namespace}/permissions`、`DELETE /v1/knowledge-bases/{namespace}/permissions/{user_id}`、`GET|POST /v1/knowledge-bases/{namespace}/invitations`、`GET /v1/invitations`、`POST /v1/invitations/preview`、`POST /v1/invitations/accept`、`POST /v1/invitations/decline`、`DELETE /v1/invitations/{invitation_id}` |
| Console 会话：会员/支付 | `GET /v1/membership`、`GET|POST /v1/membership/orders`、`GET /v1/membership/orders/{order_id}`、`POST /v1/membership/orders/{order_id}/close`、`GET /v1/workspaces/{namespace}/entitlements`、`POST /v1/payments/alipay/notify`（无会话，RSA2 验签）；管理员：`GET /v1/admin/billing/orders`、`POST /v1/admin/billing/orders/{order_id}/query|refund|manual` |
| 管理：用户 | `GET /v1/admin/users`、`PATCH /v1/admin/users/{user_id}`、`DELETE /v1/admin/users/{user_id}` |
| 管理：知识库 | `GET|POST /v1/admin/knowledge-bases`、`POST /v1/admin/knowledge-bases/models/{kind}`、`GET /v1/admin/knowledge-bases/defaults`、`GET|PATCH /v1/admin/knowledge-bases/{namespace}`、`GET /v1/admin/knowledge-bases/{namespace}/key/{kind}` |
| 管理：模型配置 | `GET|POST /v1/admin/model-profiles`、`GET|PATCH|DELETE /v1/admin/model-profiles/{profile_id}`、`GET /v1/admin/model-profiles/{profile_id}/key`、`POST /v1/admin/model-profiles/{profile_id}/models` |
| 管理：MCP 配置 | `GET /v1/admin/tools`、`PATCH /v1/admin/tools/{name}`、`GET /v1/admin/prompts`、`PUT|DELETE /v1/admin/prompts/{name}` |
| 管理：展示/只读 | `GET /v1/admin/published-versions`、`GET /v1/admin/upload-leases`（注册为无认证，但 handler 内二次校验管理员+本机边界） |
| 管理：监控 | `GET /v1/admin/monitor/overview`（只读聚合：三态总体状态、服务信息、依赖探测（Kafka 元数据 / PDF 健康负载）、文档任务/上传分类队列、两个 Kafka Connect 采集连接器（connector/task、worker、失败原因、自动重启/恢复结果）、两组 Kafka 消费组积压、缓存 dirty/failures、文档 Worker 心跳、有效文档计数、24 小时检索活动，以及顶层 `system` 主机资源（CPU/内存/网络/进程；采集不可用为 `null` 并追加到 `errors`，不影响健康判定）；不可得的数字显式为 `null`/未知；本机直连或管理员会话）、`GET /v1/admin/monitor/logs`（组件日志查询：`source=memory|file`、`component`、最低 `level`、`query`、`limit`/`offset`，按最新在前分页，返回各组件条数（「全部日志」组件筛选用）与文件元信息；未配置 `RAG_LOG_FILE` 时 `available=false` 仍返回 200，参数非法返回 400；本机直连或管理员会话） |
| 管理：Kubernetes | `GET /v1/admin/k8s/workloads`、`GET /v1/admin/k8s/pods`（`namespace` 精确匹配、`name` 大小写不敏感子串；返回全部命名空间的工作负载/Pod 快照与命名空间列表）、`POST /v1/admin/k8s/workloads/{resource}/{namespace}/{name}/restart`（resource 仅 `deployments`/`statefulsets`/`daemonsets`；patch Pod 模板的 `kubectl.kubernetes.io/restartedAt` 注解）。全部 `Cache-Control: no-store`；只接受已登录管理员会话（Cookie 或 Bearer 会话令牌；无 loopback 兜底；Cookie 写请求需 CSRF） |
| 每账号 API Key | `GET|POST /v1/admin/api-keys`、`PUT|DELETE /v1/admin/api-keys/{key_id}`、`GET /v1/admin/api-keys/{key_id}/mcp-config` |
| MCP | `POST|GET|DELETE /mcp` |

认证边界：

- 租户接口：`Authorization: Bearer <RAG_SERVICE_TOKEN 或 API Key>` + `X-RAG-Namespace`；Console 登录后也可用会话 Cookie（写操作需 `X-CSRF-Token`）。
- `/v1/admin/*`：本机直连（loopback、无代理头、同源 Origin）**或**管理员会话。`/v1/admin/api-keys` 允许任意登录账号管理**自己的** API Key（非管理员只能绑定本人创建的知识库）；`/v1/admin/k8s/*` 只接受已登录管理员会话，没有本机直连兜底（无会话 401、普通用户 403）。
- 会话共享接口（邀请/权限）只接受 Console 会话，没有 loopback 兜底。
- 未设置 Token 且本机直连时，普通租户接口可用 headers 直接访问（开发便利），生产必须配置 Token。

## 9. Web Console（`/console`）

Hash 路由（namespace 不在 URL 中，保存在 `localStorage`，默认 `default`）：

| 路由 | 页面 | 可见角色 | 主要功能 |
| --- | --- | --- | --- |
| `#/workspaces/search` | 检索调试 | 所有登录账号 | BM25/Embedding/Hybrid 检索、Cross-Encoder 重排开关、候选/Top-K 调整、三模式对比、耗时与证据状态、命中详情（分数、来源、页码/章节、图片、脚注） |
| `#/workspaces/documents` | 知识文档 | 所有登录账号 | 文档列表（大小/块数/切分配置）、上传（批次受理：工具栏「保存中/已完成 x/y」徽标 + 逐文件独立进度条气泡，SSE 实时字节进度与每文件保存结果，可失败重试；面板在保存结果处结束、不展示解析阶段；刷新后恢复未完成批次）、「从飞书导入」（连接本人飞书账号 → 浏览文件夹/搜索 → 最多勾选 10 篇 → 目标知识库与可见范围提示 → 逐篇「已受理/已跳过/失败」+ 索引进度）、删除；只读授权时隐藏写操作 |
| `#/workspaces/documents/{name}` | 文档详情 | 所有登录账号 | 正文预览、PDF 预览（pdfjs）、图片/表格资产查看 |
| `#/workspaces/create` | 创建知识库 | 所有登录账号 | namespace + 切分参数 + 从**本人**「模型配置」选择 Embedding/Reranker/VLM；管理员未提交连接时可回退默认模板，普通账号必须选择本人模型 |
| `#/knowledge-bases` | 知识库 | 管理员 | 知识库列表、编辑抽屉（模型/切分/重新向量化确认） |
| `#/model-profiles` | 模型配置 | 管理员 | Embedding/Reranker/VLM/JEV 分页签，新增/编辑/删除、模型探测、Key 只写不读 |
| `#/api-keys` | API Key | 所有登录账号 | 自己的密钥 CRUD/启停、一次性明文、复制 MCP 配置 |
| `#/users` | 用户管理 | 管理员 | 角色（admin/vip/user）、启用/禁用、删除；服务端保证至少保留一名启用的管理员 |
| `#/monitor` | 系统监控 | 管理员 | 只读监控 + 运维操作：顶部标签栏 = `概览` + `MySQL`/`Redis`/`Qdrant`/`Kafka`/`PDF 解析服务`/`文档 Worker`/`Debezium` + `全部日志`，组件标签含该组件既有内容与该组件日志，`全部日志` 的组件筛选显示各组件日志条数。管理员另有 `Kubernetes` 标签：工作负载（Deployment/StatefulSet/DaemonSet/ReplicaSet/ReplicationController/Job/CronJob，7 类）与「全部 Pod」两个视图，覆盖全部命名空间，支持命名空间/名称筛选，展开行按 ownerReferences 显示归属 Pod；仅对可滚动更新的 Deployment/StatefulSet/DaemonSet 提供滚动重启（paused/Recreate/OnDelete/`partition>0` 行内说明不可用原因）。概览：页头三态状态与降级告警、主机资源卡片（CPU 使用率/核数/型号/load、内存与 swap、网络速率与最忙网卡、进程 RSS/VMS/goroutine/heap；首次采样与 Windows load 显示「—」）、关键指标（有效文档/任务/上传/检索）、MySQL/Redis/Qdrant/Kafka/pdf-service 依赖状态（Kafka 校验元数据与主题、PDF 校验转换器）、`document_jobs` 队列与阶段、上传互斥分类、`文档采集`/`缓存采集` 两行（connector/task 状态、所在 worker、失败原因、自动重启次数与恢复结果）、两组 Kafka 消费组滞后、缓存 dirty/failures、文档 Worker 心跳（读不到显示「未知」）、服务信息；全部日志支持内存/文件来源、最低级别、关键字过滤与分页；每秒自动刷新（页头固定「自动刷新 · 每 1 秒」与最近一次成功更新时间，无自动刷新开关/手动刷新/失败重试；失败保留上次数据并自动重试，慢请求不堆叠），窄屏单列 |
| `#/membership` | 会员与用量 | 所有登录账号 | Free/Plus/Pro 目录、月/年卡、到期/刷新日、自有库与文件额度、Embedding 字符总额/已用/预留、下单并跳转支付宝、本人订单与继续支付/关单；管理员额外显示订单对账/查单/整单退款/人工处理 |
| `#/permissions` | 权限管理 | 所有登录账号（仅自有库可管理；新建邀请限管理员/VIP/Plus/Pro） | 选择知识库、按用户聚合授权、创建邀请（可读/可修改 × 全库/若干文件）、复制链接、撤销授权/邀请；到期后仍可查看和撤销已有共享 |
| `#/invitations` | 邀请中心 | 登录账号 | 待处理邀请、粘贴邀请链接、接受/拒绝（顶部铃铛轮询） |
| `#/invite?token=` | 受邀预览 | 登录账号 | 预览邀请并接受/拒绝，随后切换到该知识库 |

- 侧边栏按角色过滤；管理员专属页面在无会话（旧模式）下仍显示，但会话模式下非管理员会被重定向。
- 登录/注册：第一个注册账号自动成为管理员；`RAG_AUTH_REGISTRATION=closed` 可关闭自助注册。

## 10. 账号、角色与共享权限

- 角色只有 `admin`、`vip`、`user`；只有管理员能改角色，且最后一个启用的管理员不能被降级/禁用/删除。
- Console 会话：HttpOnly Cookie `rag_session`（默认 7 天，`RAG_AUTH_SESSION_TTL_HOURS`）；Cookie 会话的写操作要求 `X-CSRF-Token`，Bearer 会话/API Key 不需要。
- 知识库权限：管理员或创建者拥有全部读写；其他人取授权的并集——授权分 `read`/`write`，范围分全库或指定文件；写授权隐含可读。**文件级写授权只能替换已授权的同名文档，不能上传新文件**。
- 邀请：管理员、VIP 以及当前 Plus/Pro 会员可发起（管理员可邀请任意知识库，其余只能邀请自己创建的；手工 VIP 保留原权限）；邀请链接一次性，数据库只存 token 的 SHA-256 摘要；被邀请人必须存在且启用，不能邀请自己；默认 TTL 168 小时。到期后已有共享保留，但停止新发付费邀请；接受过期会员先前发出的未使用邀请会重新校验邀请人资格。
- API Key：`prk_` 前缀，明文只在创建时返回一次；按命名空间白名单生效；密钥归创建账号所有，非管理员看不到别人的密钥（返回 404）。

## 11. 模型配置（model profiles）

- 管理员在「模型配置」页维护可复用列表：`embedding` / `reranker` / `vlm` / `jev`；`(kind, name)` 唯一。
- Embedding/Reranker/VLM/JEV 保存 `base_url`、`model`、加密后的 API Key；Embedding 保存时会真实探测并记录 `vector_size`，Reranker 做一次有界探测，VLM 与 JEV 不探测。VLM 是唯一的远端视觉模型，PDF 版面/OCR/表格/公式/图片描述都由它完成；`ocr` 类型已删除（升级迁移会删除旧条目）。
- 知识库保存的是**连接副本**（保存时把 profile 的 URL/模型/Key 复制进知识库配置），之后编辑/删除 profile 不影响已保存的知识库；知识库 REST 契约仍是内联连接对象。
- 凭据加密格式 `enc:v1:`（AES-GCM，12 字节 nonce，`base64(nonce||ciphertext)`），密钥来自 `RAG_STATE_ENCRYPTION_KEY`（`base64:` 前缀的 32 字节）；旧 `dpapi:`/`private-file:` 记录只读兼容。

## 12. 异步队列、死信与缓存

- Kafka 只在配置 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时启用；空值 = brokerless（通知无法投递、文档任务保持 `queued`、缓存刷新不生效）。搜索为同步执行，不受 Kafka 是否配置影响。
- **两个 Kafka Connect 采集连接器**：`praxis-document-cdc` 只采集 `document_jobs`（仅 INSERT 与 `schedule_version` 变化，`skipped.operations=d`）；`praxis-cache-cdc` 采集 `knowledge_bases`/`documents`/`mcp_catalogs`/`api_keys`/`document_heads`/`document_leases`/`upload_operations`/`upload_steps`（全部 c/u/d）。二者运行在同一分布式 Connect 集群，位点保存在 Kafka 内部 topic；应用不再写任何事务 outbox。
- 重发只发生在 MySQL 事务内：可重试失败先写 `retry_at`，到期由 `Sweep` 递增 `schedule_version`；租约过期也由 `Sweep` 递增版本重排。普通 `queued` 等待不递增版本、不消耗执行次数。消费者只处理与数据库当前版本相等的通知，旧版本/终态/有效租约/未到期重试安全跳过并 ACK。任务表初始快照的 `op=r` 事件用于切换补投。
- topic：`praxis_document_ingest`、`praxis_state_cache_refresh`；DLQ 是显式 topic `praxis_document_ingest_dead_letters` / `praxis_state_cache_refresh_dead_letters`（不是 `%DLQ%` 约定），由 `rag-dead-letter-worker --kind document|cache` 消费。原搜索 topic/消费组/DLQ 已下线。
- 缓存消费者按 `cache_revisions` 的 revision 水位刷新：读取事件 → 事务内 `revision+1` → 重建脱敏视图 → 提交后写 Redis；失败记录 `attempts/last_error` 并照常 ACK，由周期 `RefreshDirty` 补偿，Redis 写失败不丢收敛、不会造成 DLQ 风暴。
- 消费语义至少一次：成功提交 offset；失败同进程指数退避重试（100ms 起、上限 5s），达到 `MaxAttempts`（默认 3）后写 DLQ 再提交；重复投递由幂等与 MySQL 租约吸收。文档执行次数另由 `document_jobs.attempts` 记录（上限 3，跨消息/worker/重启），第 3 次失败写 `failed`。
- 文档 worker 只有 Kafka 消费模式：`rag-service` 不内嵌消费者，`rag-document-worker` 消费 Kafka Connect 的 `praxis-document-cdc` 投递的 `RAG_DOCUMENT_TOPIC`；未配置 Kafka 或未运行该 worker 时，上传仍受理但任务保持 `queued`。
- Redis 用途：默认只承担读模型展示缓存（短 TTL）；搜索响应与 artifact 事实都不在 Redis（artifact 在 MySQL，按 ID 独立读取）。可选把文档长租约后端切到 Redis（`RAG_DOCUMENT_LOCK_MODE=redis`，部署级开关，MySQL 仍保存持有者登记与 `fencing_epoch`）；展示缓存开关不控制文档锁，切换见 [部署指南](../DEPLOYMENT.md) §13。
- 搜索为同步执行：一次调用完成检索 + 重排并返回证据；总预算由 `RAG_SEARCH_TIMEOUT_SECONDS`（默认 120s）约束，客户端取消会终止下游调用。

## 13. 运维与 CLI

- 健康检查 `GET /health`；OpenAPI `GET /openapi.json`（OpenAPI 3.1.0）、交互文档 `GET /docs`。
- `rag-state-migrate [--apply]`：状态库表结构检查/幂等创建；`rag-service-migrate` 默认只读预演，`--apply` 只导入源文件与文档记录，**不重建向量**，需要重新上传才会可检索；`rag-service-migrate --retire-outbox --yes` 显式删除 4 张已下线的 outbox 表。
- `rag-cdc-cutover`：CDC 存量切换 CLI（`--inventory` 盘点、`--record-position` 记录 binlog 位点、`--backfill --baseline=N` 补投、`--mark-cache-baseline` 缓存基线、`--verify --baseline=N` 核对、`--all`），只覆盖 `document_jobs` 与缓存表（搜索任务盘点/回填已随同步化改造删除），详见 [CDC 迁移契约](../cdc-migration.md) §8。
- `rag-connect`：Kafka Connect 运维 CLI（`register` 固定名称幂等注册、`wait` 等待 connector/task RUNNING、`status`、`plugins` 版本校验、`supervise` standalone 自动重启、`decode` 用生产解码器验证真实 Kafka 消息），详见 [Kafka Connect 手册](../../deploy/kafka-connect/README.md)。
- `rag-mcp-ingest`：递归导入本地文件/目录，输出 `OK/SKIP/FAIL` 与 `SUMMARY`；`--dry-run` 不打开运行时。
- `docker compose up -d` 启动基础设施（MySQL 3307、Qdrant 6333、Redis 6379、Kafka 9092）、分布式 Kafka Connect 两个 worker 与 `pdf-parser`（8091）；`--profile app` 通过 `connect-init` 注册连接器后构建并启动 Go 服务。
- `scripts/start-stack.ps1` 一键起依赖、建 topic、迁移、注册并等待两个 Connect 连接器，然后启动 `rag-service` + `rag-document-worker`；`stop-stack.ps1` 停止。
- `scripts/verify-debezium-e2e.py`（Kafka Connect 契约 + 验收 2/3/4）、`verify-kafka-dlq.py` 是真实基础设施上的验证脚本（需要 Docker 与 Python 3）；原 `verify-kafka-search.py`（MCP 排队搜索）已随功能删除。
