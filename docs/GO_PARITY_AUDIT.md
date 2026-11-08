# Go 后端与 Python/前端契约一致性审计

审计对象：`D:\Project\Praxis-RAG\.agents\worktrees\dsh-d432f525-b2f1`
审计类型：只读（本文件是唯一写入）
审计时间：2026-11-08 21:00 左右（Asia/Shanghai）
代码基线：`a80f831 feat(go): expose SearchRaw worker seam`（包含 `23c53e5` 的 worker processor 库；2026-11-08 21:10 左右），工作区干净（仅本报告未跟踪）。审计过程中并行开发提交了 task-18/20（异步受理、queued search、后台维护、SearchRaw、worker processor 库）；**行号会随并行开发漂移，结论以提交/代码再核对为准。**

方法：逐文件静态对照 `frontend/src`、`internal/httpapi`、`internal/runtime`、`internal/mcp`、`src/rag_mcp` 与 `tests/`；抽查运行了 Go 测试。未启动 Python 服务，未做黑盒差分。范围外的外部依赖（Docker/MySQL/Redis/RocketMQ/Qdrant/Docling/LibreOffice/浏览器）均未运行。

本轮验证命令与结果：

```text
go build ./...                                          # exit 0
go test -count=1 ./internal/httpapi/... ./internal/runtime/... ./internal/mcp/... ./internal/searchrefs/...
ok internal/httpapi 1.676s
ok internal/runtime 13.440s
ok internal/mcp 2.654s
ok internal/searchrefs 2.747s
```

规模：Go 侧 83 个 `_test.go`、671 个 `Test*`（23 个包）；Python 侧 64 个 `test_*.py`（另有 3 个 helper）、593 个 `def test_*`。数量接近，但覆盖域并不等价（见 §5）。

---

## 0. 结论摘要

前端 `/console` 实际调用的 **全部端点都已在 Go 路由表中存在**，`api.ts` 的错误解析、健康检查、认证/local-admin 规则、no-store、KB/API Key/文档 CRUD 的主字段形状基本一致。切换阻断项集中在“Python 行为已存在、Go 尚未接线”的少数路径：

| 优先级 | 编号 | 问题 | 影响 |
| --- | --- | --- | --- |
| **P0** | P0-1 | `/v1/search` 未移植 `pdf_assets.project_results`：搜索结果缺少 `asset`、`metadata.captions/footnotes/location/caption_source/page/bbox`，MCP 也不会产生图片内容块 | PDF/Word 资产命中在 Console 无图/表预览；MCP 图片证据丢失 |
| **P0** | P0-2 | Go 的 MCP `search` 固定同步模式（`QueuedSearch: false`），Python 生产模式暴露 queued + `wait_seconds` schema | MCP 工具契约/调用方式不一致，按 Python 文档传 `wait_seconds` 会报错 |
| **P0** | P0-3 | 没有 `rag-document-worker` / `rag-search-worker` / `rag-dead-letter-worker` 可执行程序，且文档 worker 所需的 `Service.ProcessDocumentJob` seam 未实现（processor 库 + `WIRING.md` 已在 `23c53e5` 提交） | Go-only 部署下 MCP `add_document` 入库永远停在 queued；搜索任务不被消费 |
| **P0** | P0-4 | `runtime.ReadDocument` 不读已落盘的 `document.md`：PDF 直接报错，Word 重新抽取纯文本 | MCP `resources/read`（PDF 报错、Word 内容不同）、Console Word 预览与 Python 不一致 |
| P1 | P1-1 | `/v1/search` 缺少 `snapshot_context` | 保存的搜索快照/debug JSON 与 Python 不同 |
| P1 | P1-2 | `/v1/documents/check` 未校验扩展名 | Python 400 “暂不支持该文件类型”，Go 200 + null |
| P1 | P1-3 | KB 创建/更新 HTTP 校验漂移（`embedding` 必填、`embedding_model` min_length） | Python 422，Go 可能 400/409/503 |
| P1 | P1-4 | MCP `delete_document` destructive annotation 错误（Go false，Python true） | 客户端安全提示不一致 |
| P1 | P1-5 | md/txt 预览未做 UTF-8 与 `MAX_EXTRACTED_CHARACTERS` 校验 | 非法内容 200 vs Python 404/503 |
| P1 | P1-6 | `/v1/documents` 使用权威仓储而非 cached read views | 两次读取的可见性/时序不同 |
| P2 | — | 静态目录列表、裸二进制找不到 Console、casefold、错误 type 文案、OpenAPI 文档等 | 见 §4.3 |

---

## 1. REST 契约审计

### 1.1 端点全景（前端实际调用）

前端调用来自 `frontend/src/api.ts` 与各页面（`SearchPage.tsx`、`DocumentsPage.tsx`、`KnowledgeBasesPage.tsx`、`ApiKeysPage.tsx`、`CreateKnowledgeBase.tsx`、`DocumentPreview.tsx`、`Console.tsx`）。逐项核对结果：

| # | 前端调用 | Go 实现（文件:行） | 结论 |
| --- | --- | --- | --- |
| 1 | `GET /health` | `internal/httpapi/server.go:108,249-255` | ✅ 形状一致（`status/service/backend/collection/state`），无认证；Console 只读 `backend` |
| 2 | `POST /v1/search` | `internal/httpapi/search.go:95-127`；`internal/httpapi/search_body.go` | ⚠️ 外壳一致，资产/`snapshot_context` 缺失（P0-1、P1-1） |
| 3 | `GET /v1/documents` | `internal/httpapi/documents.go:46-68` | ✅ 额外字段 `supported_extensions/max_files_per_batch/max_file_bytes` 与 Python 相同 |
| 4 | `POST /v1/documents/check` | `internal/httpapi/documents.go:284-313` | ⚠️ 字段/顺序一致，缺扩展名校验（P1-2） |
| 5 | `POST /v1/documents`（multipart，`Idempotency-Key`） | `internal/httpapi/documents.go:70-121` → `runtime.documents_upload.go:146-201` | ✅ `imported/skipped/failed/documents`，`skipped[].requested_name` 一致；非文件字段 400 文案一致 |
| 6 | `DELETE /v1/documents/{name}` | `internal/httpapi/documents.go:177-204` | ✅ `{"deleted": record, "documents": [...]}`，404 映射一致 |
| 7 | `GET /v1/documents/{name}/content` | `internal/httpapi/documents.go:229-263` | ⚠️ PDF 返回源文件 ✅；Word 预览内容不同（P0-4）；md/txt 缺 UTF-8/长度校验（P1-5） |
| 8 | `GET /v1/knowledge-base` | `internal/httpapi/mcp_mount.go:26-50` | ✅ `public_settings + strategies`；缺配置 404 一致 |
| 9 | `GET /v1/admin/knowledge-bases` | `internal/httpapi/kb_admin.go:104,120-133` | ✅ `knowledge_bases[]` 含 `configured/document_count`（`internal/state/read_views.go:140-215`） |
| 10 | `GET /v1/admin/knowledge-bases/{namespace}` | `internal/httpapi/kb_admin.go:107,205-221` | ✅ 公开设置；404 文案一致 |
| 11 | `POST /v1/admin/knowledge-bases` | `internal/httpapi/kb_admin.go:105,136-194` | ⚠️ 201 正确；`embedding` 必填校验缺失（P1-3） |
| 12 | `PATCH /v1/admin/knowledge-bases/{namespace}` | `internal/httpapi/kb_admin.go:109,303-329` | ⚠️ `embedding_model` min_length 未在 HTTP 层校验（P1-3） |
| 13 | `POST /v1/admin/knowledge-bases/models/{kind}` | `internal/httpapi/kb_admin.go:106,249-301` | ✅ `{"models":[...]}`、422/400 映射一致 |
| 14 | `GET /v1/admin/knowledge-bases/{namespace}/key/{kind}` | `internal/httpapi/kb_admin.go:108,223-247` | ✅ `{"apikey": ...}`，kind 422 文案一致 |
| 15 | `GET /v1/admin/knowledge-bases/defaults` | `internal/httpapi/server.go:111,261-263` | ✅ `{"default":"recursive","strategies":[...]}`，与 `rag_mcp.chunk_strategies.strategy_catalog()` 字段完全相同（`internal/chunking/chunking.go:19-50`） |
| 16 | `GET /v1/admin/api-keys` | `internal/httpapi/api_keys.go:103-115` | ✅ `{"keys":[...]}`，`exportable/prefix/namespaces` 等一致 |
| 17 | `POST /v1/admin/api-keys` | `internal/httpapi/api_keys.go:117-144` | ✅ 201、`key` 只出现一次 |
| 18 | `PUT /v1/admin/api-keys/{id}` | `internal/httpapi/api_keys.go:146-188` | ✅ 200/404；`name+namespaces+enabled` 必填 |
| 19 | `DELETE /v1/admin/api-keys/{id}` | `internal/httpapi/api_keys.go:190-205` | ✅ 204/404 |
| 20 | `GET /v1/admin/api-keys/{id}/mcp-config` | `internal/httpapi/api_keys.go:207-234` | ✅ `{"url","headers"}`，需要 `X-RAG-Namespace`（前端已带） |
| 21 | `GET /v1/uploads[/{id}]`、`/v1/admin/published-versions`、`/v1/admin/upload-leases` | `internal/httpapi/upload_status.go` | ✅ `operations/limit/display_only`、`operation/display_only`、`versions/leases`；后两者 handler 内再做 local-admin+tenant（路由 policy 为 none，行 61-68） |
| 22 | `GET /`、`/console`、`/console/assets/*` | `internal/httpapi/static.go:9-22` | ✅ 基准路径与 vite `base:"/console/assets/"` 对齐（见 §2） |
| 23 | `GET/POST/DELETE /mcp` | `internal/httpapi/mcp_mount.go:18-24`、`internal/mcp/http.go` | ✅ 鉴权/协议外壳一致；queued 语义见 P0-2 |

Python 参考路由表：`src/rag_mcp/http_service.py:395-744`。Go 路由表见 `internal/httpapi/server.go:107-145` + `internal/httpapi/kb_admin.go:103-110`；**前端/后端路由无缺项**。前端未调用、但 Python 提供的 `/v1/admin/tools`、`/v1/admin/prompts`、`/v1/search-results/*`、`/v1/artifacts/*` 在 Go 中也已实现。

### 1.2 已确认一致的“易错点”

- 错误信封：Python `{"detail": ...}`（`http_service.py:369-389,433-435`）↔ Go `writeDetail`（`internal/httpapi/server.go:358-360`）；前端解析 `detail` 字符串或数组（`frontend/src/api.ts:138-148`）均可工作。
- 校验状态码：`/v1/search-results/`、`/v1/artifacts/` 的 query 校验 Python 返回 **400**（`http_service.py:371-373`），Go `intQuery` 返回 400（`internal/httpapi/search.go:156-228`）一致；KB admin 前缀 Python 422（`http_service.py:374-376`），Go 422 一致。
- `no-store` 路径：Python `_SnapshotNoStoreMiddleware`（`http_service.py:73-88`）覆盖 `/mcp`、`/v1/search`、`/v1/search-results/*`、`/v1/artifacts/*`；Go `isSnapshotPath` 相同（`internal/httpapi/server.go:273-278`，在 `serveHTTP` 中提前设置）。
- 认证：服务 token → API Key 命名空间 → loopback 无凭证 fallback，顺序与文案一致（`internal/httpapi/auth.go:34-82` vs `http_service.py:285-312`）；local-admin 的 loopback/Forwarded/Origin/Sec-Fetch-Site 规则已逐条移植（`internal/httpapi/auth.go:100-150` vs `http_service.py:192-213`）。
- `/v1/documents` 的文档记录：Go `state.DocumentRecord` JSON tag（`internal/state/repositories.go:543-556`）与 Python `_document_record`（`src/rag_mcp/state/repositories.py:297-312`）字段名、`operation_id` 可选性、诊断/跳过计数一致；列表按 name casefold 排序（`internal/state/repositories.go:636-640`）。
- 上传结果：`imported/skipped/failed/documents`、`skipped.requested_name`（Go `internal/runtime/documents_upload.go:184-200,495-498`；Python `src/rag_mcp/service_knowledge.py:848-874,220-221`）一致。
- KB 公开设置：`{namespace, vector_size, embedding_model, reranker_model, collection, tokenizer, strategy, chunk_size, chunk_overlap, created_at, embedding{base_url,api_key_set}, reranker{...}}`（`internal/kbconfig/public.go:11-34` vs `src/rag_mcp/knowledge_bases.py` `public_settings()`；列表另加 `configured/document_count`）。
- 时间戳格式：Go `kbconfig.ISOTimestamp`（`internal/kbconfig/settings.go:24,49-51`）与 Python `datetime.now(UTC).isoformat()` 的 `...+00:00` 微秒格式一致。
- PDF 前端依赖：`PdfPreview` 的 `BASE_URL` 是 `/console/assets/`（vite `base`），`/console/assets/pdfjs/...` 由 Go 静态处理器 strip 前缀后落到 `RAG_WEB_ROOT/pdfjs/...`（`dist/web/pdfjs` 存在），可正常解析。

---

## 2. Console 构建与静态托管

- 构建链：vite `outDir: ../src/rag_mcp/web`、`base: /console/assets/`（`frontend/vite.config.ts:14-18`）→ `scripts/build-go.ps1` 复制到 `dist/web`；Go `RAG_WEB_ROOT` 解析顺序为显式设置 → `src/rag_mcp/web` → `web` → 可执行文件同级 `web`/`../src/rag_mcp/web`（`internal/httpapi/server.go:280-315`）。打包/容器分别设置 `dist/web` 与 `/app/web`，路径假设成立。
- 路由：`/` 与 `/console` 返回 `index.html`（`internal/httpapi/static.go:9-17`），`/console/assets/*` 用 `FileServer(webRoot)` 服务（`:19-22`）；构建产物 `dist/web/index.html` 的 script/css 均为 `/console/assets/assets/...`，`dist/web/pdfjs/{cmaps,standard_fonts,wasm}` 共 198 个文件；抽查 URL 映射（`/console/assets/pdfjs/...` → `webRoot/pdfjs/...`）正确，Python 的 `StaticFiles(directory=_WEB_ROOT)` 行为等价（`http_service.py:500-506`）。
- 前端没有 `/openapi.json`、`/docs`、`/redoc`、Swagger 依赖，也没有 SPA 深链接（hash 路由，`frontend/src/navigation.ts:17-44`），因此 Go 未提供这些路由不影响 Console；但外部 API 用户/Python 测试会用到 `/openapi.json`（`tests/test_http_service.py:91`），见 P2-6。
- 无 CORS 中间件：Python/Go 都没有 REST CORS；MCP 的 host/origin allowlist 两边都由 MCP handler 校验（Go `internal/mcp/http.go`，Python `TransportSecuritySettings` `http_service.py:321-329`）。Console 同源访问不依赖 CORS。
- 静态差异（P2）：Go `http.FileServer` 对没有 `index.html` 的子目录（如 `/console/assets/assets/`）会返回 HTML 目录列表，Python `StaticFiles` 返回 404；且 Go 的 `/console/assets/` 会因 `webRoot/index.html` 存在而返回 index.html。若概率不可接受，可在静态 handler 上加“只允许已知目录/文件”的白名单。

---

## 3. MCP / 异步契约抽查

### 3.1 工具定义

- 工具集合/顺序：`search`、`get_search_result`、`add_document`、`update_document`、`delete_document`、`get_document_status`（Go `internal/mcp/tools.go:21-29,70-186`；Python `src/rag_mcp/server.py:255-289` + `src/rag_mcp/document_tools.py:89-141`）一致。
- 输入 schema：`search`（含 filters 三键、`additionalProperties:false`、默认值）逐字段一致（`internal/contracts/contracts.go:400-437` vs `src/rag_mcp/server.py:80-126`）；文档工具 name/content/content_base64/idempotency_key 与 oneOf 一致（`internal/mcp/tools.go:188-251` vs `document_tools.py:99-125`）。
- 描述文本一致（含 10 MiB、async 语义）。
- annotations：
  - `search`/`get_search_result`/`get_document_status` read-only/idempotent 一致；
  - `add_document`（read_only=false, destructive=false）与 `update_document`（destructive=true）一致；
  - **`delete_document` 不一致**：Python `destructive_hint = name != "add_document"`（`document_tools.py:121-125`）→ delete 为 `true`；Go `destructive := writing && name != addDocumentToolName`（`internal/mcp/tools.go:226-233`）→ delete 为 `false`。见 P1-4。
- `renderPayload`：图片资产转 MCP ImageContent 并回填 `content_block` 与 Python 相同（`internal/mcp/tools.go:435-471` vs `src/rag_mcp/server.py:241-252`）；但前提是 `asset.data` 存在（P0-1）。

### 3.2 同步 / 排队搜索

- Python：`create_registry` 在 runtime 暴露 `search_queued` 时使用 queued 版本，schema 增加 `wait_seconds`，调用链是 `submit_search → 轮询 get_search_result`（`src/rag_mcp/server.py:126-133,255-261`；`src/rag_mcp/mcp_http.py:318-348`）；生产 `RagServiceRuntime` 具备该方法，因此 **Python MCP 搜索实际是 queued 协议**。
- Go：queued 机制已实现——`submitSearch`（`internal/runtime/queued_search.go:48-127`）、有序读取 queued 视图（`:132-155`）、MCP queued 执行/等待（`internal/mcp/tools.go:256-308`）、`SearchDispatcher`/`StartBackground`（`internal/runtime/maintenance.go:75-135`，`cmd/rag-service/main.go:74`）。但 `main.go:81` 硬编码 `QueuedSearch: false`，导致 MCP 永远走同步 schema：客户端传 `wait_seconds` 会被 `SearchRequestFromArguments` 视为 Unknown argument（`internal/contracts/contracts.go:135-...`）。见 P0-2。

### 3.3 异步文档

- Python：`add_document` → `DocumentJobDispatcher.submit` 返回 `{"artifact_id": ...}`（`src/rag_mcp/state/document_queue.py:258-267`）；`get_document_status` 返回 `{artifact_id,status,stage,error}`（`src/rag_mcp/state/document_jobs.py:257-262`）；错误映射和 409 文案在 `mcp_http.py:350-395`。
- Go 运行时已具备相同返回（`internal/runtime/async.go:26-46`、`:86-101`；`internal/state/document_jobs.go:259-272,522-531`），`Service.AddDocument/GetDocumentStatus` 已委托（`internal/runtime/service.go:311-318`）。
- **缺执行侧**：`internal/documentworker/processor.go`（16 个测试）与 `internal/searchworker/processor.go`（13 个测试）只是库；`cmd/` 只有 `rag-mcp-debug`、`rag-mcp-ingest`、`rag-service`、`rag-service-migrate`、`rag-state-migrate`，没有 `rag-document-worker`/`rag-search-worker`/`rag-dead-letter-worker`。两份 `WIRING.md`（`internal/documentworker/WIRING.md`、`internal/searchworker/WIRING.md`）给出了 main 的接线代码，但其中要求的 runtime seam 只完成了一半：`SearchRaw` 已存在（`internal/runtime/service.go:419-421`、`internal/runtime/search_engine.go:193-207`），而文档侧 `ProcessDocumentJob`/`RecoverDocumentUploads` 只有 `documentworker` 接口与 fake，runtime 未实现（grep 全仓库仅出现在 `internal/documentworker/*`）。Python 侧对应 `document_worker.py`、`search_worker.py`、`dead_letter_worker.py`，且 `docs/GO_MIGRATION.md` §10 也把 worker 主程序列为剩余项。见 P0-3。

### 3.4 文档资源（MCP resources/read）

- Python 资源读取走 runtime `read_document`/`read_text`：PDF 用 `read_pdf_content`，Word 用 `read_word_content`（读取已落盘 `document.md`），见 `src/rag_mcp/service_knowledge.py:347-374`、`src/rag_mcp/word_assets.py:183-193`；`tests/test_mcp_http.py:772 test_word_and_pdf_resources_over_http` 明确验证 Word+PDF 资源。
- Go：resources handler 直接调用 runtime `ReadDocument`（`internal/mcp/resources.go:116-127`）；而 `ReadDocument` 对 PDF 直接返回错误，对 Word 重新解析源文件（`internal/runtime/service.go:153-186,215-226`）。见 P0-4。

---

## 4. 具体问题清单（按优先级）

### 4.1 P0（阻断切换）

#### P0-1 搜索结果缺少资产投影（`asset` / captions / footnotes / location）

- 期望（Python）：`pdf_assets.project_results` 把命中里的 `metadata.asset_id` 通过 Qdrant payload 还原为资产，并校验 `upload_version`；图片命中输出 `asset={type:"image",mime_type,data(base64),caption,page,bbox}`（Word 存储时改为 `location`），表格输出 `asset={type:"table",caption,page,bbox,rows,header_rows,cells,num_rows,num_cols,footnotes,...}`；metadata 增加 `captions/footnotes/caption_source/asset_kind/page/bbox`（`src/rag_mcp/pdf_assets.py:120-179`）。该函数被装配为 orchestrator 的 projector（`src/rag_mcp/service_knowledge.py:1074-1076`）。
- 实际（Go）：`buildTenant` 明确传 `nil` projector（`internal/runtime/tenant.go:109-122`，注释“artifact projection is attached by the document/artifact layer”），但全仓库没有任何调用方；`qdrant.Store.GetAssets` 已实现（`internal/qdrant/store.go:627+`）却只被 qdrant 测试引用。结果命中只有 `rank/document_id/chunk_id/title/source/text/score/metadata`（`internal/contracts/contracts.go` `Candidate.ToResult`），**没有 `asset` 键**，metadata 里只有 Qdrant 原始字段（`asset_id/upload_version/...`）。
- 影响：
  - Console：`SearchPage` 用 `item.asset` 决定是否渲染图片/表格（`frontend/src/SearchPage.tsx:73-78`），`AssetView` 依赖 `asset.data`/`rows`/`cells`（`frontend/src/AssetView.tsx:16-71`）→ 资产命中退化为纯文本，图/表不可见。
  - HTTP 契约：Python 明确保证直接 HTTP 客户端拿到内联 base64（`tests/test_http_service.py:207-217`）。
  - MCP：`renderPayload` 只有 `asset.data` 存在才生成 ImageContent（`internal/mcp/tools.go:435-471`）→ Agent 收不到图片证据。
  - `searchrefs` 只对已有 `hit["asset"]` 做 artifact 引用（`internal/searchrefs/searchrefs.go:272,357+`），因此也不会为资产生成 artifact。
- 建议：把 `project_results` 移植为 runtime tenant 的 projector：按 `metadata.asset_id` 调 `qdrant.Store.GetAssets`，按 `upload_version` 过滤，从 `state.Blobs`（WORD_ASSET_BUCKET/PDF_ASSET_BUCKET + `upload_objects` 版本对象）读取图片 bytes 并 base64；保留 Word 的 `location` 与 `caption/footnote/caption_source` 语义；在 `SearchEngine.Search` 的 capture 之前生效，补一个与 Python `tests/test_pdf_assets.py`/`test_pdf_fidelity.py` 对应的 Go 测试。

#### P0-2 MCP 搜索契约：Go 固定同步，Python 生产为 queued

- 期望（Python）：`search` 工具 schema 带 `wait_seconds`（0–30，默认 10）和 queued 描述（`src/rag_mcp/server.py:126-153`）；调用后最多等待 wait 秒，超时只返回 `result_id`，再轮询 `get_search_result`（`src/rag_mcp/mcp_http.py:334-348`）；`test_mcp_http.py` 有多个 queued/取消/超时/隔离用例（452、497、527、549、574 等行）。
- 实际（Go）：`cmd/rag-service/main.go:81` `QueuedSearch: false` → 使用同步 schema（无 `wait_seconds`，`internal/contracts/contracts.go:400-437`）；同步执行会真正跑完检索。runtime 虽然实现了 queued 全部语义（`internal/runtime/queued_search.go:48-155`，`internal/mcp/tools.go:256-308`），但入口不可达。
- 影响：按 Python 部署文档/Agent 提示词传 `wait_seconds` 会得到 MCP tool error（未知参数）；长期运行语义（提交后不阻断、可轮询）不同；`GO_MIGRATION.md §3.3` 要求 MCP schema 不变。
- 建议：按 `SearchDispatcher`/broker 是否可用决定 `QueuedSearch`（或直接默认 true 并保留无 broker 时 fail-closed 的 `submitSearchTask` 行为），补一个 queued 模式下的 MCP 集成测试；在 docs 明确两种模式。

#### P0-3 异步 worker 无可执行程序

- 期望（Python）：`rag_mcp.document_worker`、`rag_mcp.search_worker`、`rag_mcp.dead_letter_worker` 作为独立进程消费 RocketMQ/outbox；文档作业 claim→progress→complete，搜索任务 claim→execute→complete，DLQ 终端记录（`src/rag_mcp/document_worker.py`、`src/rag_mcp/search_worker.py`、`src/rag_mcp/dead_letter_worker.py`；测试 `tests/test_document_tasks*.py`、`test_search_worker.py`、`test_dead_letter*.py`）。
- 实际（Go）：状态机、outbox、MQ transport、processor 库都有（`internal/state/document_jobs.go`、`internal/state/document_queue.go`、`internal/mq`、`internal/documentworker`、`internal/searchworker`，后者两个包在 `23c53e5` 提交并各自带 `WIRING.md`），但 `cmd/` 下没有 worker 命令；文档 worker 还需要 runtime 的 `ProcessDocumentJob`/`RecoverDocumentUploads` seam（`documentworker/processor.go` 接口 + `WIRING.md` 要求），当前 runtime 未提供，因此即使补 main 也编译/运行不起来文档消费。`internal/runtime/async.go:26-46` 会把 MCP `add_document` 成功写成 queued job，而 Go 进程没有任何消费者 → 状态永远 `queued`，`get_document_status` 永远不 succeeded；搜索任务同理（搜索侧 `Service.SearchRaw` seam 已具备）。REST 同步上传不受影响（不经过队列）。
- 建议：先把 runtime 的 `ProcessDocumentJob`/`RecoverDocumentUploads` 接上（Python `process_document_task`/`UploadCoordinator.recover` 语义），再新增 `cmd/rag-document-worker`、`cmd/rag-search-worker`、`cmd/rag-dead-letter-worker`（消费 `internal/mq` + 调用 processor，严格按 `WIRING.md` 的 lifecycle），加入 Dockerfile（当前 Dockerfile 会自动构建所有 `cmd/*`，无需改）、compose、`scripts/build-go.ps1` 的 smoke/文档；或在切换期明确继续运行 Python workers。

#### P0-4 `runtime.ReadDocument` 未读取已落盘 `document.md`

- 期望（Python）：`read_text` 对带 `operation_id` 的 `.pdf/.doc/.docx` 优先从上传版本对象读取 `{document_id}/document.md`；缺失时报 `Saved content unavailable; reimport this document`（404 映射）；无版本时 PDF 用 `read_pdf_content`、Word 用 `read_word_content`；md/txt 用 `utf-8-sig` 解码并限制 `MAX_EXTRACTED_CHARACTERS`（`src/rag_mcp/service_knowledge.py:347-374`）。
- 实际（Go）：`ReadDocument` 只读源 blob：md/txt `stripBOM`；doc/docx `document.ReadText` 重新解析（`.doc` 还需要外部 soffice，失败则报错）；PDF 直接返回 503 文案 `PDF text preview requires the external parser; read the original file instead.`（`internal/runtime/service.go:153-186,215-226`）。
- 影响：Console Word 预览内容与索引视图不一致（缺表格/标题/公式等 markdown 渲染）；MCP `resources/read` 对 PDF 文档直接失败（`internal/mcp/resources.go:116-127`），Python 测试 `test_mcp_http.py:772` 明确覆盖 Word+PDF 资源；`.doc` 预览在没有 LibreOffice 的环境不可用而 Python 可以。
- 建议：实现 `read_text` 的版本对象分支（`state.Uploads.GetObject`/`blobs.Get` + `document.md`），PDF/Word fallback 用 `document.ReadContent`/`ReadWordContent` 解析 manifest；错误文案与 Python 保持一致；补 Go 端 HTTP/MCP resources 测试。

### 4.2 P1（高概率功能/契约回归）

1. **`snapshot_context` 缺失**：Python `/v1/search` 在保存快照前注入 `{collection, embedding_model, reranker_model, vector_size, chunk_tokenizer_model, configuration_version}`（`src/rag_mcp/service_knowledge.py:958-977`），Go `SearchEngine.Search` 直接 capture（`internal/runtime/search_engine.go:68-85`）→ 保存的 result snapshot 与 MCP `get_search_result` debug 上下文少一个字段。新增的 worker seam `SearchEngine.SearchRaw`（`internal/runtime/search_engine.go:193-207`）也未注入该字段，而 `internal/searchworker/WIRING.md` 明确要求 raw 搜索带 `snapshot_context`；Go 的 tenant 也没有 Python 的 `kb_version` 可填 `configuration_version`。
2. **`/v1/documents/check` 扩展名校验缺失**：Python `check_documents` 先 `validate_uploads`（`src/rag_mcp/service_knowledge.py:836-846`），对不支持的扩展名 400 `暂不支持该文件类型...`；Go `CheckDocuments` 只 `SanitizeName`（`internal/runtime/service.go:235-...`）→ 返回 200 + `document:null`。前端在上传前自己限制了扩展名，因此 Console 不触发，但外部 API 契约不同。
3. **KB 管理 HTTP 校验漂移**：Python `CreateKnowledgeBaseBody.embedding` 必填（`http_service.py:255-264`）；Go `createKnowledgeBaseBody.Embedding *providerSettings` 可选（`internal/httpapi/kb_admin.go:61-70`，nil 时只在 172-179 传空 map），随后探测失败返回 400/503 而非 422。Python `UpdateKnowledgeBaseBody.embedding_model` 有 `min_length=1`（`http_service.py:239-245`）；Go 只在运行期 kbconfig 校验（`internal/kbconfig/settings.go:280-292`），返回 409 文案。DTO 轮询/前端正常路径不触发。
4. **MCP `delete_document` destructive hint 错误**：见 §3.1/`internal/mcp/tools.go:226-233` vs `src/rag_mcp/document_tools.py:121-125`。
5. **md/txt 预览缺校验**：Python 对非 UTF-8 返回 `文本文件必须使用 UTF-8 编码：name`（400/404 映射）并限制 `MAX_EXTRACTED_CHARACTERS`（`service_knowledge.py:365-372`）；Go `service.go:168-170` 直接 `string(stripBOM(...))`，非法字节流按 200 返回。
6. **`/v1/documents` 数据源**：Python 用 cached read views（`service_knowledge.py:809-811` → `state/read_views.py:97-98`），Go 用权威仓储 + 手工 overlay（`internal/runtime/service.go:126-148`）。同一数据内容，但缓存/新鲜度/事件顺序语义不同；对 Console 无可见差异。
7. **OpenAPI 文档缺失**：Python FastAPI 自动提供 `/openapi.json`、`/docs`、`/redoc`（`http_service.py:362`；`tests/test_http_service.py:91` 使用 `/openapi.json`），Go 路由表无这些端点（`internal/httpapi/server.go:107-145`）。前端不依赖，但外部 API 消费者、代码生成、既有 parity 测试会失败。
8. **无 `snapshot_context` + queued（P0-2）组合**：Python queued 搜索的 result snapshot 也含 snapshot_context；Go queued 快照字段将不同（修复 P1-1 后自然消失）。

### 4.3 P2（细节/部署）

1. **裸二进制找不到 Console**：Go 默认只探测 cwd/可执行文件邻居（`internal/httpapi/server.go:280-315`），`dist/bin/rag-service.exe` 从任意目录启动且未设 `RAG_WEB_ROOT` 时 `/console` 404；Python 用包内 `web` 目录（`http_service.py:53`）。打包脚本/Docker 已设置 `RAG_WEB_ROOT`，需要在 docs/README 中写明。
2. **静态目录列表**：Go `FileServer` 会为 `dist/web/assets/`、`pdfjs/cmaps/` 等目录返回 HTML 列表（`internal/httpapi/static.go:19-22`），Python `StaticFiles` 返回 404；可加白名单/仅文件。
3. **排序 casefold 近似**：Go 用 `strings.ToLower` 近似 `str.casefold()`（`internal/state/repositories.go:636-640`）；对 `ß`/土耳其语等文件名排序与 Python 不同（文件名为用户输入，概率低）。
4. **API Key 列表数据源**：Python `key_store.list()` 走 `state.views.api_keys`（`http_service.py:395-397` → `state/read_views.py:110-112`），Go `apikeys.Store.List` 读权威仓储（`internal/httpapi/api_keys.go:103-115`）；字段相同。
5. **`Idempotency-Key` 空值**：Python Header min_length=1 → `string_too_short`；Go `idempotencyHeader` 空串返回 `string_too_long`（`internal/httpapi/documents.go:108-121`）。仅错误 type/message 细节。
6. **UUID/工具错误文案**：MCP `get_search_result`/`get_document_status` 的非法 UUID 文案与 Python `validate_uuid`/`validate_artifact_id` 略有差异（`internal/mcp/tools.go:613-627`、`internal/contracts` 的 `ValidateUUID`；Python `server.py:32-45`、`state/document_jobs.py`）。状态/是否 tool error 一致。
7. **`chunk_size` 严格整数**：Python `chunk_size: int | None = Field(strict=True)` 对 `1.0`/`"1"` 返回 422 `int_type`；Go 反序列化到 `*int` 对 `1.0` 报 `json_invalid`（`internal/httpapi/kb_admin.go:61-70`）。仅错误 shape 差异。
8. **`/v1/search` 未知字段**：Python `SearchBody` 默认忽略 extra；Go 同样忽略（`search_body.go` 注释一致）。✅
9. **`/v1/admin/prompts` 等外部管理端点**：前端无页面，Go 已按 Python 语义实现；未做专项黑盒差分。
10. **MCP `get_document_content`**：`docs/GO_MIGRATION.md §3.3` 提到该工具名，但 Python `server.py`/`document_tools.py` 实际没有注册（用 resources 代替）；以 Python 源码为准，不算 Go 缺项。

---

## 5. 测试覆盖清单（Python ↔ Go）

### 5.1 映射表

| 领域 | Python 测试（tests/） | Go 对应（internal/*_test.go） | 覆盖判断 |
| --- | --- | --- | --- |
| HTTP 服务/路由/鉴权/错误 | `test_http_service.py`（13） | `httpapi/server_test.go`(15)、`documents_test.go`(3)、`search_test.go`(4)、`kb_admin_test.go`(5)、`upload_status_test.go`(2) | ✅ 主干覆盖，缺 `/openapi.json`、console asset 全量抓取 |
| MCP HTTP/双协议 | `test_mcp_http.py`（23） | `mcp/http_test.go`(8)、`http_modern_test.go`(4)、`resources_http_test.go`(6)、`resources_test.go`(4)、`tools_test.go`(9) | ⚠️ 部分：queued 轮询/取消屏蔽/订阅过滤/关机释放/部分上传通知/Redis 驱逐无 Go 对应 |
| MCP 工具/文档工具 | `test_document_tools.py`(6)、`test_mcp_http.py` 部分 | `mcp/tools_test.go`(9)、`runtime/documents_upload_test.go`、`runtime/async_test.go`(2) | ✅ 工具校验/写路径覆盖；缺 10 MiB 端到端 base64、并发 add |
| 文档读取/切分 | `test_document_readers.py`、`test_chunk_strategies.py`、`test_real_chunking.py` | `document/*_test.go`(63)、`chunking/*_test.go`(40)、`ingest/*_test.go`(18) | ✅ 内存 fixture 平移；真实文件/真实 tokenizer 未验证（已知 parity 风险） |
| Word/PDF 资产与保真 | `test_word_fidelity.py`、`test_real_word_fidelity.py`、`test_pdf_assets.py`、`test_pdf_fidelity.py`、`test_real_pdf_fidelity.py` | `document/{word,assets,associate,formula,pdf}*_test.go`、`ingest/*`、`document/pdfcontent` | ⚠️ fixture 级覆盖；真实 Docling/LibreOffice/vision 未验证 |
| 检索栈 | `test_phase2_dense.py`、`test_phase3_hybrid.py`、`test_phase4_reranker.py`、`test_orchestrator.py`、`test_qdrant_versions.py` | `retrieval/*_test.go`(39)、`qdrant/store_test.go`(33)、`modelapi/model_api_test.go`(20)、`sparse/sparse_test.go`(9) | ✅ 单位/假服务器覆盖；真实 Qdrant/model 服务未验证 |
| 共享状态层 | `test_state_store.py`、`test_api_keys.py`、`test_cached_read_views.py`、`test_soft_delete_references.py` | `state/*_test.go`（168） | ✅ 大面积平移；MySQL live 见下 |
| 上传事务 | `test_upload_coordinator.py`、`test_upload_journal.py`、`test_upload_recovery.py`、`test_upload_transaction.py`、`test_upload_dedup.py`、`test_upload_entrypoints.py` | `state/upload_coordinator_test.go`(20)、`state/uploads_test.go`(14)、`runtime/documents_upload_test.go`(12) | ✅ SQLite 语义覆盖；HTTP/MCP entrypoint 细节少 |
| MySQL 事务 | `test_mysql_upload_transactions.py` | `state/mysql_test.go`(3，需 `RAG_TEST_MYSQL_URL`，当前 skip) | ❌ 未验证 |
| 缓存/Redis/快照 | `test_cache_*.py`(7)、`test_state_cache.py`、`test_transient.py`、`test_second_batch_cache.py`、`test_journal_cache_events.py`、`test_result_snapshots.py`、`test_artifact_registry.py` | `state/cache_*_test.go`(19)、`state/result_snapshots_test.go`(11)、`state/artifacts_test.go`(16)、`redisstate/client_test.go`(15)、`searchrefs/searchrefs_test.go`(11) | ✅ miniredis/内存级覆盖；真实 Redis 未验证 |
| 异步文档/搜索任务 | `test_document_tasks.py`、`test_document_queue.py`、`test_document_outbox.py`、`test_document_delivery.py`、`test_search_tasks.py`、`test_search_worker.py` | `state/g7_test.go`(16)、`mq/mq_test.go`(11)、`runtime/async_test.go`(2)、`queued_search_test.go`(6)、`maintenance_test.go`(5)、`documentworker/processor_test.go`(16)、`searchworker/processor_test.go`(13) | ⚠️ 状态机/processor 覆盖，无 worker 可执行程序；文档 worker runtime seam 未实现；无 MQ 实机测试 |
| 死信/租约/恢复 | `test_dead_letter_worker.py`、`test_dead_letter_live.py`、`test_upload_recovery.py` 等 | `documentworker`/`searchworker` processor 测试、`state/g7_test.go`、`runtime/maintenance_test.go` | ⚠️ 逻辑覆盖，无 live |
| 迁移 CLI | （Python `test_state_store.py` 部分） | `migrate/migrate_test.go`(4)、`cmd/rag-*-migrate` | ⚠️ 行为差异见 `docs/GO_MIGRATION.md` §9（不重建向量、`--skip-assets` TODO） |
| 浏览器/E2E | `test_browser_chunking.py`、`test_browser_model_options.py`、`test_browser_navigation.py`、`test_browser_word.py`、`test_real_model_api_ui.py` | 无 | ❌ 无 Go/浏览器 E2E；Console 仅打包 smoke |
| Live 集成 | `test_cache_live.py`、`test_document_delivery_live.py`、`test_document_tasks_live.py`、`test_search_tasks_live.py`、`test_uuid_live.py`、`test_dead_letter_live.py` | 无（Docker/外部服务不可用） | ❌ 未验证 |

### 5.2 最高风险测试缺口

1. **异步链路端到端**：processor 库有较充分单测（`documentworker` 16、`searchworker` 13）且带 `WIRING.md`，但没有 worker 主程序、文档侧 runtime seam 未实现、也没有 RocketMQ/MySQL 真实消费测试；`test_document_tasks*.py`/`test_search_worker.py`/`test_dead_letter*.py` 的端到端语义在 Go 侧无法回归（P0-3）。
2. **MCP HTTP 行为**：Python 23 个用例包含 queued 轮询、取消屏蔽、订阅过滤、部分上传通知、broker outage admission、Redis 驱逐不复活等；Go 的 MCP 测试约 12–31 个用例主要覆盖协议外壳，queued 相关完全不可用（P0-2）。
3. **Console 浏览器行为**：`test_browser_*.py` 覆盖导航/切分选择/模型选项/Word 预览，Go 侧只有静态文件 smoke（`/health`、`/console`、`/`），没有浏览器执行；Console 与 Go API 的字段联动只能靠人工/后续 E2E。
4. **真实外部依赖**：Docling/Word 外部解析、LibreOffice、vision worker、BGE-M3 tokenizer（Go 为近似实现）、真实 Qdrant/Redis/MQ/MySQL 均无 Go 等价验证；迁移文档已把 tokenizer 与 broker 列为主要风险。
5. **Word/PDF 预览与 MCP resources**：`test_document_preview.py`（3）与 `test_mcp_http.py:772` 的 Word/PDF 资源读取在 Go 侧没有对应用例，因此 P0-4 没被测试暴露。
6. **MySQL 事务/锁语义**：Go `state/mysql_test.go` 3 个用例默认 skip，Python 有独立 MySQL 上传事务测试。
7. **OpenAPI/schema 漂移**：Python 测试直接断言 `/openapi.json`（`tests/test_http_service.py:91`），Go 没有该端点，未来 parity harness 会直接失败。

---

## 6. 建议下一步（按顺序）

1. **P0-4**：实现 runtime 的“已保存 document.md”读取路径（Word/PDF/MCP resources），这是 Console 预览 + MCP 资源的基础。
2. **P0-1**：移植 `project_results` 资产投影（含图片 base64、表格、captions/footnotes/location、upload_version 过滤），并加 Go 测试。P0-1 与 P0-4 共用 asset bucket/版本对象读取代码，可一起做。
3. **P0-2**：在 `cmd/rag-service` 根据 broker/dispatcher 可用性启用 `QueuedSearch`，补 queued MCP 模式集成测试；文档说明同步/排队两种模式的配置差异。
4. **P0-3**：先实现文档 worker 的 runtime seam（`ProcessDocumentJob`/`RecoverDocumentUploads`），再按 `WIRING.md` 新增三个 worker 命令（document/search/dead-letter），复用 `internal/mq` + `internal/{documentworker,searchworker}` processor；更新 Dockerfile/compose/build 脚本/README。没有 worker 前不要宣称 async MCP 可用。
5. **P1**：补 `snapshot_context`、`documents/check` 扩展名校验、KB 管理 422 校验、delete annotation、md/txt UTF-8/长度校验；这些小改动可一次性完成并加 HTTP 断言。
6. **测试**：优先补 (a) HTTP `/v1/search` 资产内联 base64 用例、(b) MCP resources Word/PDF、(c) queued search MCP 轮询、(d) 异步 document 端到端（SQLite+内存 MQ 或 fake transport）、(e) worker processor 已覆盖的用例保持绿灯。
7. **G8 前**：建立 `internal/parity` 黑盒差分（同请求打 Python 与 Go 服务，比较状态码/JSON 关键字段），至少覆盖本文 §1 的 23 个前端端点与 §3 的 6 个 MCP 工具；在 Docker 环境跑 Qdrant/MySQL/Redis/RocketMQ 冒烟。

---

## 7. 未验证声明（无法在本环境确认）

- **未启动 Python 服务**：所有 Python 行为均来自源码阅读，没有做同一请求的 Python/Go 差分；Python 依赖（torch/FlagEmbedding/rocketmq 等）在本审计环境未安装。
- **未启动 Go 服务做本报告的黑盒验证**：仅运行了 §0 列出的四个包测试与 `go build ./...`；Console 静态路径是从构建产物与 handler 代码推导的（task-15 的 smoke 曾验证 `/health`、`/console`、`/`，但那是在打包验收时，对应的是当时的二进制与 dist/web）。
- **Docker 不可用**：没有构建镜像，也没有 `docker compose --profile app` 验证；`Dockerfile`/compose 的正确性仅人工检查（task-15 已记录）。
- **外部基础设施不可用**：没有 MySQL/Redis/RocketMQ/Qdrant/Docling/LibreOffice/vision 模型，无法验证 P0-3 的真实消费、MySQL 事务、缓存 live 行为、PDF/Word 真实解析与 tokenizer 等价。
- **无浏览器自动化**：没有运行 `test_browser_*.py` 的替代 E2E，Console 的字段/交互（特别是 P0-1 的资产渲染、P0-4 的 Word 预览）是静态对照结论。
- **并行开发中**：审计期间上游提交了 `5283d8d`/`03c7ede`/`23c53e5`/`a80f831`（异步受理、queued search、后台维护、SearchRaw、worker processor 库），本报告按最终 HEAD `a80f831` 重写过相关结论；提交后行号仍会漂移，删除 Python 后端前需再次核对 §4 的 P0/P1 是否关闭。
