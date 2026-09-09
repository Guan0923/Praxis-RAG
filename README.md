# Standalone RAG MCP

这是一个可独立安装和运行的 Agentic RAG 模组，不依赖 Mini-Agent Backend 或主前端。`rag-service` 一个进程同时提供：

- 标准 Streamable HTTP MCP：`/mcp`
- 知识管理和检索 REST API：`/v1/*`
- 独立 Web Console：`/console`
- 文本切分、Embedding、BM25、Qdrant 和 Reranking

```text
浏览器 ----------> http://127.0.0.1:8011/console
MCP HTTP Client -> http://127.0.0.1:8011/mcp -> rag-service -> Qdrant :6333
```

RAG 只返回证据，不生成最终回答。是否调用、是否改写查询、是否多跳检索以及如何组织答案，仍由 Agent 决定。

## 安装

在本仓库中开发：

```powershell
cd xinghe_rag
uv sync --extra dense
```

作为独立工具安装当前目录：

```powershell
cd xinghe_rag
uv tool install --editable ".[dense,pdf]"
```

从独立 Git 仓库直接安装：

```powershell
uv tool install "mini-agent-rag-mcp[dense,pdf] @ git+https://github.com/Guan0923/xinghe_rag.git"
```

安装后，`rag-service`、`rag-mcp-debug`、`rag-mcp-ingest` 和 `rag-service-migrate` 都是独立命令。

## 启动

### 1. 启动 Qdrant

```powershell
docker run -d --name mini-agent-qdrant -p 6333:6333 -p 6334:6334 -v mini_agent_qdrant_data:/qdrant/storage qdrant/qdrant:latest
```

容器已经创建过时执行：

```powershell
docker start mini-agent-qdrant
```

### 2. 启动 RAG Service 和独立前端

```powershell
$env:RAG_BACKEND = "qdrant"
$env:QDRANT_URL = "http://127.0.0.1:6333"
$env:RAG_COLLECTION = "mini_agent_knowledge_pdf_assets_v1"
$env:RAG_EMBEDDING_DEVICE = "cuda:0"
$env:RAG_RERANKER_DEVICE = "cuda:0"
$env:RAG_SERVICE_DATA_ROOT = "$HOME/.mini_agent-rag"
$env:RAG_PDF_PARSER_PYTHON = "C:/path/to/pdf-parser/python.exe"
$env:RAG_PDF_MODELS_PATH = "C:/path/to/docling/models"

rag-service
```

仓库开发环境中也可以执行：

```powershell
uv run --package mini-agent-rag-mcp rag-service
```

默认地址：

- Web Console：`http://127.0.0.1:8011/console`
- MCP：`http://127.0.0.1:8011/mcp`
- OpenAPI：`http://127.0.0.1:8011/docs`
- 健康检查：`http://127.0.0.1:8011/health`

Console 包含知识上传、文档删除、BM25/Embedding/Hybrid 检索、Reranker 切换、Top-K 结果、score、source、chunk_id 和阶段耗时。它直接访问同源 RAG API，不经过 Mini-Agent。调试页还提供“三模式对比”，固定关闭重排并并排展示三种 Retriever 的原始结果，避免 Cross Encoder 把不同召回重新排成相同顺序。

首次上传或 Dense 检索时会下载并加载 BGE-M3。PDF 由独立 Docling 2.126.0 子进程解析，使用 PDFium、CUDA 和本机模型；图片或表格没有有效原文说明时，才使用固定版本的 Granite Vision 3.3 2B 生成检索描述。图片原图保存在租户目录，图表原始数据和检索文字写入 Qdrant。每批最多 10 个文件，单文件最大 10 MiB。

PDF 公式使用本地 CodeFormulaV2 转为 LaTeX，开启生成缓存，并以紧裁剪、单公式推理减少相邻文字干扰。模型错误、空公式输出或生成被截断会导致该文件解析失败，不静默跳过。模型能够输出 LaTeX 不代表数学内容一定正确，复杂公式仍需要用原页验证。

OCR 不再按默认置信度阈值丢弃已识别文字；低置信度内容保留，并在 `ocr.json`、解析清单和相关检索块的 `source_warnings` 中记录。保留文字不等于识别正确，扫描件仍可能有错字。

每份 PDF 的原生 `document.json`、按阅读顺序保存的 `manifest.json`、正文 `document.md`、OCR 记录 `ocr.json` 和图片保存在知识目录的 `.pdf-assets/<document_id>/`。正文、图片、表格与公式保持 Docling 的遍历顺序；图注、表题、脚注保留原文关联，不按长度丢弃，也不猜配未关联注解。生成描述仅用于检索，不充当原文注解。命令行 PDF 导入和 MCP 正文读取使用同一份解析结果。

### 前端开发与打包

Console 使用 React、TypeScript 和 Ant Design，提供浅色与深色主题。主题和 Namespace 保存在当前浏览器；Service Token 仅保存在当前会话，MCP 配置示例不会显示真实 Token。

正常运行仍只需启动 `rag-service`，通过 `/console` 打开页面，不需要 Node.js 或额外前端进程。仓库中的 `src/rag_mcp/web/` 是随 Python 包交付的构建文件，修改界面请编辑 `frontend/src/`，不要手动修改构建文件。

```powershell
conda activate dev
cd frontend
npm ci
npm run build
```

构建会替换 `src/rag_mcp/web/` 中的旧文件；发布或打包 Python 包前，必须一起包含新的构建结果。仅开发时需要 Node.js 22.12+。

需要热更新时，可以单独运行开发服务器：

```powershell
$env:RAG_DEV_API = "http://127.0.0.1:8011"
npm run dev
```

访问终端输出地址下的 `/console/assets/`。开发服务器将 API 请求转发给 `RAG_DEV_API`；生产环境始终使用同源接口。

真实浏览器验收必须指向独立测试服务和独立数据目录，测试会创建自己的 Namespace，并只清理其中的测试文档：

```powershell
conda activate dl
$env:PYTHONPATH = "$PWD/src"
$env:RAG_BROWSER_URL = "http://127.0.0.1:8021"
python -m pytest tests/test_browser_chunking.py -v
```

测试需要 Python Playwright 和本机 Edge。截图写入 `.test-tmp/console-browser/`；真实上传、检索与重排使用实际模型，额外的失败和延迟响应测试使用浏览器拦截。

### 切分策略与上传去重

Web Console 的“知识文档”页面可选择五种切分策略。网页及上传 API 默认使用 `recursive`，块长度为 512 token，重叠上限为 64 token；参数可调整。命令行导入仍使用原字符策略，不增加新参数。

| 策略值 | 行为 |
| --- | --- |
| `character` | 原字符切分，默认 700 字符、100 字符重叠，优先在换行或标点处结束。 |
| `token` | 使用 Embedding 模型的 tokenizer，按原文位置截取 token 窗口。 |
| `sentence` | 完整句子合块，超长单句按 token 拆分。支持常见中英文断句和英文缩写，不做语言模型语义判断。 |
| `recursive` | 段落、句子、token 逐级细分，再合并完整内容单元。 |
| `structure` | Markdown 按标题；TXT 按空行段落；PDF 按页、段落、句子；DOCX 按标题样式及段落，表格过长按行处理。 |

短内容按原文顺序合并，允许跨章节和 PDF 页面。重叠计入长度上限，句子重叠可能少于设置值或为零；超长句子使用 token 重叠。chunk 元数据记录字符位置、实际页码、标题路径和段落位置。PDF 正式上传按 Docling 的页码和版面顺序生成正文块；图片按原图注检索并返回原图，表格按表题、表头和行内容检索并返回行列及合并信息。扫描文本使用 Docling OCR；无法稳定恢复结构的表格会终止该文件入库。

PDF 正文实际执行所选切分策略；公式整体保留，表格按完整行附带全部表头层级入库，不跨图表或公式边界拼接正文。表格的 `header_rows` 保存多层表头，`cells` 保存合并范围，空列不会被删除。不可拆分的公式、图注或表格行可以超过目标块长度，但不得超过 Embedding 模型输入上限，超过时明确报错。PDF 块记录原始引用、页码及位置；跨页原文保留全部 `locations`，`page` 是起始页。普通正文的字符偏移对应当前连续正文段，不是 PDF 文件字节位置；来源的 `charspan` 对应 Docling 原始元素。

- `GET /v1/chunking/strategies` 返回策略选项、单位和默认参数。
- `POST /v1/documents` 在原 `files` multipart 字段外接收 `strategy`、`chunk_size`、`chunk_overlap`。
- `POST /v1/documents/check` 接收 JSON：`{"files":[{"name":"guide.md","sha256":"64位小写十六进制哈希"}],"strategy":"recursive","chunk_size":512,"chunk_overlap":64}`，返回同序的 `files`，每项 `document` 为已有文档或 `null`。
- 三个接口均沿用 `X-RAG-Namespace` 和服务认证。去重仅在同一 namespace 内比较文件 SHA-256、策略、长度、重叠及 tokenizer 标识；文件名不参与比较。
- PDF 去重还比较 `pdf_parser_version`，解析器更新后重新上传会重新索引。旧 PDF 不自动迁移，缺少当前版本解析正文时 MCP 会要求显式重新导入，不回退到旧的纯文本提取。
- 浏览器先计算哈希并检查，命中则不发送文件正文。需要 HTTPS 或 localhost 安全连接。后端接收正文时重新计算哈希，并在租户锁内再次去重。
- 上传结果分别包含 `imported`、`skipped`、`failed` 和 `documents`。更改参数会重新索引；改名但配置相同则返回已有文档，不新建别名。已有文档不会自动重建。
- 旧清单首次检查时按需补齐哈希，缺失配置按原 700/100 字符策略解释。tokenizer 下载或加载失败会明确报错，不退回字符计数。

真实验收（在已安装 `.[dense]` 和 `pytest` 的 `dl` 环境运行）：

```powershell
conda activate dl
$env:RAG_REAL_SMOKE = "1"
python -m pytest tests/test_real_chunking.py -q -s
```

该测试用真实 BGE-M3、BM25 和本地 Qdrant，在临时目录中验证五种策略及四种文件。浏览器验收另需 `playwright` 和本机 Edge：设置 `RAG_BROWSER_URL` 为独立测试服务地址后，运行 `tests/test_browser_chunking.py`；截图保存在 `.mini_agent-rag/chunking-ui/`。

PDF 最多解析 1000 页；DOCX 解压后的内部文件总量最多 50 MiB；单个文档最多提取 500 万字符。扫描版 PDF 使用本地 OCR；旧版二进制 `.doc` 需要先另存为 `.docx`。

## Namespace

namespace 是一个独立知识库标识，例如 `default`、`product-manuals` 或 `team-a`。Console 与 MCP 配置必须使用相同值，否则 Agent 检索不到网页上传的文档。

允许 1～64 个字符，只能包含字母、数字、点、下划线和横线。知识源文件位于：

```text
~/.mini_agent-rag/<namespace>/knowledge/
~/.mini_agent-rag/<namespace>/manifest.json
```

namespace 不在 Agent 可见的工具 Schema 中；MCP HTTP 客户端必须在每个请求中发送 `X-RAG-Namespace`。同一个 MCP 会话不能切换 namespace，需要切换时应重新初始化会话。

## 接入 Streamable HTTP MCP Client

MCP 客户端使用以下连接信息：

```json
{
  "url": "http://127.0.0.1:8011/mcp",
  "headers": {
    "X-RAG-Namespace": "default"
  }
}
```

启用 Token 后还需要发送：

```text
Authorization: Bearer <RAG_SERVICE_TOKEN>
```

客户端可使用 MCP `2025-11-25` 或 `2026-07-28` 的 Streamable HTTP 传输。旧版先初始化，新版可发现服务或直接调用；具体流程见后面的“双版本”章节。取消调用只结束等待，不会强制终止已经进入模型或 Qdrant 的同步计算。

MCP 提供 `search`、`add_document`、`update_document`、`delete_document` 四个工具。若 Host 使用 `rag` 作为 server 名，搜索工具展示名通常是 `mcp_rag_search`。

### MCP 文件管理

三个文件工具只操作连接时 `X-RAG-Namespace` 指定的知识库，使用普通 Service Token，不需要管理 Token。请仅向可信客户端提供具有写入权限的连接；工具提示不能替代权限控制。管理员可通过工具管理接口分别禁用这些工具。

| 工具 | 参数 | 行为 |
|---|---|---|
| `add_document` | `name` 和 `content` 或 `content_base64` | 添加并建立索引；同名文件报错，不覆盖 |
| `update_document` | `name` 和 `content` 或 `content_base64` | 完整替换已存在文件并重建索引，保留切分设置；不存在时报错 |
| `delete_document` | `name` | 删除文件和索引；不存在时报错 |

`name` 必须是含扩展名的准确文件名，不能传路径。支持 TXT、Markdown（`.md`）、PDF、DOCX，单文件最大 10 MiB。TXT/Markdown 可使用 UTF-8 文本 `content`；PDF/DOCX 必须由客户端读取文件后传标准 Base64 到 `content_base64`。两种内容参数只能传一种，不支持服务器路径或 URL。

```json
{"name": "manual.txt", "content": "E03 表示温度传感器故障。"}
```

新增使用默认递归切分。同内容不同文件名会分别保存，确保更新始终针对指定文件。同名、同内容、同切分设置的更新返回 `unchanged`。成功返回 `status`、`document` 和可读取的 `uri`；工具参数错误或操作失败返回 MCP `isError=true`。整个请求另有大小上限，超过后返回 HTTP 413；既有文档解析安全上限仍然适用。文件变更会发送资源更新通知。删除无法通过 MCP 撤销，更新前需要保留旧版本时应先读取或备份。

工具输入：

```json
{
  "query": "星河 X7 出现 E03 应该怎么处理？",
  "retrieval": "hybrid",
  "rerank": "cross_encoder",
  "candidate_k": 10,
  "top_k": 5,
  "filters": {}
}
```

允许的 filter 只有 `document_id`、`source` 和 `title`，不接受原始 Qdrant Filter。

## Token 与远程部署

默认只监听 loopback。绑定非 loopback 地址时必须设置 Token，以及 MCP Host、Origin 白名单：

```powershell
$env:RAG_SERVICE_HOST = "0.0.0.0"
$env:RAG_SERVICE_TOKEN = "replace-with-a-random-secret"
$env:RAG_MCP_ALLOWED_HOSTS = "rag.example.com"
$env:RAG_MCP_ALLOWED_ORIGINS = "https://app.example.com"
rag-service
```

线上 MCP 客户端连接：

```json
{
  "url": "https://rag.example.com/mcp",
  "headers": {
    "X-RAG-Namespace": "product-manuals",
    "Authorization": "Bearer <RAG_SERVICE_TOKEN>"
  }
}
```

Console 的 Token 只保存到浏览器 `sessionStorage`。生产环境还应在 RAG Service 前配置 HTTPS、访问控制和请求大小限制。

## 开发与验证

```powershell
uv run --no-sync python -m pytest tests -q
uv run --no-sync python -m ruff check src tests
uv run --package mini-agent-rag-mcp rag-mcp-debug "RRF 怎么融合 BM25 和 Dense？"
uv run --package mini-agent-rag-mcp rag-mcp-debug --schema
```

协议测试会启动真实本机 HTTP 服务，使用官方客户端和真实文件存储，但以可控检索器和 PDF 解析结果替代模型，不下载模型或连接 Qdrant。这不等于真实模型效果验证。

本地 GPU 的 PDF 完整链路验收使用 `tests/test_real_pdf_fidelity.py`。先激活 `dl`，指定已安装依赖的解析器 Python、本地模型目录和样本目录，再执行：

```powershell
conda activate dl
$env:PYTHONPATH = "$PWD/src"
$env:HF_HUB_OFFLINE = "1"
$env:TRANSFORMERS_OFFLINE = "1"
$env:RAG_REAL_PDF = "1"
$env:RAG_PDF_PARSER_PYTHON = "C:/path/to/pdf-parser/python.exe"
$env:RAG_PDF_MODELS_PATH = "C:/path/to/docling/models"
$env:RAG_PDF_SAMPLES = "C:/path/to/pdf/samples"
$env:RAG_PDF_SAMPLE_FILES = "aviation.pdf,doclaynet.pdf,mineru_demo2.pdf,scan.pdf"
& $env:RAG_PDF_PARSER_PYTHON -m pytest tests/test_real_pdf_fidelity.py -q -s
```

此测试在独立临时目录启动 HTTP 服务和本地 Qdrant，调用真实 Docling、CodeFormulaV2、OCR、BGE-M3；检查上传、重复检测、检索和 MCP 全文读取。耗时、显存分配峰值、公式与图表数量、检索结果写入 `.test-tmp/pdf-fidelity-real/<run>/report.json`。这些检查证明链路保留情况，不等于公式语义或版面识别准确率；仍须对照原页和原生结果检查。

本轮实测结果及未解决样例见 [PDF 修复验证记录](benchmarks/PDF_VALIDATION.md)。

### Conda 开发环境

其他项目可能仍依赖 MCP 1.x。为避免改动共享环境，在激活 `dev` 后给本项目创建独立虚拟环境：

```powershell
conda activate dev
python -m venv --system-site-packages .venv
.venv\Scripts\python -m pip install -e .
.venv\Scripts\python -m pytest tests -q
.venv\Scripts\python -m rag_mcp.http_service
```

完整模型检索还需要安装 `.[dense]` 并准备模型及 Qdrant；上述协议测试不要求这些依赖。

## MCP 双版本

依赖固定为 `mcp==2.1.1`，同一个 `/mcp` 支持以下两种流程，客户端不能把新版版本号放进旧版 `initialize` 来切换流程：

```text
2025-11-25: initialize → notifications/initialized → tools/list / tools/call
2026-07-28: server/discover（可选）→ tools/list / tools/call → subscriptions/listen（按需）
```

新版每次请求携带自己的协议信息，不要求先建立旧版会话。协议头和请求格式应由 SDK 处理。Python SDK `Client` 的 `mode="legacy"` 使用旧版，默认 `mode="auto"` 发现新版，`mode="2026-07-28"` 跳过发现直接使用新版。

请求均携带 `X-RAG-Namespace` 和配置要求的 Service Token。旧会话不能跨 namespace 使用；订阅固定建立时的 namespace。工具调用取消只停止等待和发送结果，不强停后台模型线程。

HTTP MCP 注册搜索和三个文件管理工具；本地搜索调试入口仍只提供搜索。新增工具要在项目中登记定义、参数校验和执行函数；配置接口不能上传代码，也不能注册外部 HTTP 转发。

## 工具与提示词管理

设置独立的 `RAG_ADMIN_TOKEN` 开启管理接口；未设置时管理接口返回 403。它必须与 `RAG_SERVICE_TOKEN` 不同，二者不能互换。管理接口使用 `Authorization: Bearer <RAG_ADMIN_TOKEN>` 和 `X-RAG-Namespace`。普通 MCP/REST 请求使用 Service Token，Console 不保存或展示管理凭证。

| 接口 | 行为 |
|---|---|
| `GET /v1/admin/tools` | 返回本 namespace 的已注册工具和启用状态 |
| `PATCH /v1/admin/tools/{name}` | 仅支持 `enabled`、`description`；未知工具返回 404 |
| `GET /v1/admin/prompts` | 返回提示词模板及正文 |
| `PUT /v1/admin/prompts/{name}` | 新建或完整替换模板 |
| `DELETE /v1/admin/prompts/{name}` | 删除模板，不存在返回 404 |

工具修改示例：`{"enabled": false}`。提示词 PUT 请求示例：

```json
{
  "description": "总结已读取的资料",
  "arguments": [{"name": "text", "description": "资料正文", "required": true}],
  "messages": [{"role": "user", "text": "请总结以下资料：\n{{text}}"}]
}
```

模板仅替换 `{{argument}}`；参数必须是字符串，消息角色仅为 `user` 或 `assistant`，不执行表达式或代码。可选参数未提供时替换为空字符串。MCP 客户端通过 `prompts/list`、`prompts/get` 使用这些模板。

配置保存在 `<RAG_SERVICE_DATA_ROOT>/<namespace>/mcp_catalog.json`，重启恢复，管理配置不会加载检索模型。管理 Token 仍可管理所有 namespace，不提供用户级权限隔离。配置修改请走管理接口，不手工编辑正在运行的配置文件。

## 文档资源与订阅

- `resources/list` 返回当前知识库已导入文档，列表每页最多 50 项，按 `nextCursor` 继续；`resources/templates/list` 返回分页地址规则。
- 文档地址为 `rag://knowledge/<namespace>/<编码后的文件名>`，`resources/read` 默认读取第一页，不返回本地路径。
- 正文按段落分页，通常每页最多 8000 字符；长公式独占一页且不截断，可以超过此值。单个公式已在入库时受模型输入上限约束。这不是 PDF 原始页码，也不是自动识别的章节。
- 返回标准文本资源，其中 `text` 是 JSON：`uri`、`text`、`page`、`total_pages`、`version`、`has_more`、`truncated` 和 `next_uri`。`truncated=true` 表示当前页未包含剩余正文，可继续读取 `next_uri`。
- 下一页地址携带文档版本。文档替换后旧的下一页地址会报错，客户端应从文档 URI 重读。解析安全上限与上传规则不变，超限或解析失败明确报错，不静默截断整份文档。
- 指定资源更新订阅使用稳定的文档 URI，不使用分页 URI；删除后通知订阅者，后续读取返回不存在。

新版 `subscriptions/listen` 可选择工具列表、提示词列表、资源列表及指定文档更新。旧版仍使用列表变化通知与 `resources/subscribe`、`resources/unsubscribe`。通知只表示“发生变化”，客户端需要重新列出或读取内容。

启停工具或修改描述、增删改提示词、上传替换删除文档都会在对应操作成功后通知。失败项和没有变化的重复操作不通知；同名相同内容上传跳过重复索引。通知不会跨 namespace。

本轮仅支持**单进程、单 worker**。不提供跨进程通知、历史事件重放或断线补发。订阅断线后客户端重新订阅并读取最新状态。服务退出时释放会话与订阅，不部署 OAuth、账户系统或云服务。

## 当前限制

- 文档解析支持 Markdown/TXT、电子版和扫描版 PDF、DOCX；旧版 `.doc`、Excel、网页抓取和独立图片上传尚未实现。
- PDF 使用本机 Docling、OCR 和公式模型；不能保证任意扫描件、旋转表格或复杂公式都准确。公式处理针对 Docling 检出的公式块，不保证所有行内数学符号转换为 LaTeX。明确的解析错误会失败，但不能自动识别所有内容错误。
- `embedding` 使用 BGE-M3，默认 1024 维。
- `hybrid` 使用 Qdrant Dense + Sparse 预取和 RRF 融合。
- `cross_encoder` 使用真实 BGE Reranker；`bi_encoder` 仍是确定性 Mock。
- Console 会同时展示 Cross Encoder 最终分数和重排前的 `retrieval_score`；请求失败时会清空旧结果并保留错误面板。
- Cross Encoder 默认延迟加载。若首次调用因模型源网络不可达而失败，可先选择“不重排”或“三模式对比”验证召回，再准备本地模型缓存并重启服务。
- 上传目前是同步请求，大文件索引期间网页会等待。
- Console 用于本地开发调试，不提供账户系统或细粒度多用户权限。

历史阶段和本轮变更记录见 [PROGRESS.md](PROGRESS.md)。
