# Praxis-RAG 使用与部署

本文介绍 Praxis-RAG 的安装、部署、接口和开发方式。产品概览见 [README](../README.md)。`rag-service` 一个进程同时提供：

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
uv sync --extra api
```

作为独立工具安装当前目录：

```powershell
uv tool install --editable ".[api,pdf]"
```

从独立 Git 仓库直接安装：

```powershell
uv tool install "praxis-rag[api,pdf] @ git+https://github.com/Guan0923/xinghe_rag.git"
```

安装后，`rag-service`、`rag-document-worker`、`rag-mcp-debug`、`rag-mcp-ingest` 和 `rag-service-migrate` 都是独立命令。异步 MCP 文档队列还需要安装现有可选依赖 `[cache]`（包含 RocketMQ SDK），例如 `uv sync --extra api --extra cache`；独立工具安装时在 extras 中加入 `cache`。

以上本地安装命令在仓库根目录运行，需要先安装 uv。项目产品名为 Praxis-RAG，Python 分发包名为 `praxis-rag`；远程仓库地址以当前 Git 配置为准。内部 Python 模块仍为 `rag_mcp`，集合名和命令保留原值。服务配置与新数据使用下文的 `~/.praxis-rag/` 目录，已有知识库不自动迁移。下文开发和测试命令也默认从仓库根目录运行，前端命令另有说明。

## 启动

### 1. 启动 MySQL、Qdrant、Redis 和 RocketMQ（本地 Docker）

仓库自带 `docker-compose.yml`，会同时启动 MySQL 8、Qdrant、Redis 和 RocketMQ：

```powershell
Copy-Item .env.example .env
# 编辑 .env，填入 RAG_MYSQL_PASSWORD、MYSQL_ROOT_PASSWORD、RAG_STATE_ENCRYPTION_KEY
.\scripts\start-stack.ps1 -MigrateOnly
```

MySQL 默认映射到 `127.0.0.1:3307`，避免和已有的 3306 冲突；Qdrant 使用 `6333/6334`。停止但保留数据：`.\scripts\stop-stack.ps1`。

也可以手动启动 Qdrant：

```powershell
docker run -d --name mini-agent-qdrant -p 6333:6333 -p 6334:6334 -v mini_agent_qdrant_data:/qdrant/storage qdrant/qdrant:latest
```

容器已经创建过时执行：

```powershell
docker start mini-agent-qdrant
```

### 2. 启动共享状态数据库

生产环境使用 MySQL 8.0+（`utf8mb4`，InnoDB）。创建数据库和账号后，通过环境变量或 `config.toml` 配置：

```powershell
$env:RAG_MYSQL_HOST = "127.0.0.1"
$env:RAG_MYSQL_PORT = "3307"
$env:RAG_MYSQL_USER = "rag"
$env:RAG_MYSQL_DATABASE = "praxis_rag"
$env:RAG_MYSQL_PASSWORD = "<password>"
$env:RAG_STATE_ENCRYPTION_KEY = "base64:<32-byte-key>"
```

也可以直接设置完整的 SQLAlchemy URL（包含用户名、密码、主机和数据库，字符需 URL 编码）：`RAG_STATE_URL = "<SQLAlchemy MySQL URL>"`。未配置 MySQL 时，服务回退到 `RAG_SERVICE_DATA_ROOT/state.sqlite3`，仅适合本地开发和测试。表结构、迁移与多实例说明见 [共享状态存储](state-storage.md)。

### 3. 启动 RAG Service 和独立前端

本地使用 Compose 时，直接执行 `.\scripts\start-stack.ps1`（不带 `-MigrateOnly`）会加载 `.env`、启动依赖、显式创建缓存刷新与文档入库的两组 RocketMQ topic/group、迁移旧状态并运行服务。脚本不会启动文档消费者；MCP 异步新增还需下面的独立 worker。手动配置环境变量时也可以直接执行：

```powershell
rag-service
```

`rag-service` 自动读取 `~/.praxis-rag/config.toml`，首次启动会创建默认文件。主目录已经安装 `.venv` 时，可以直接执行 `.\.venv\Scripts\rag-service.exe`，不需要每次设置 `$env:RAG_BACKEND`。

```toml
[environment]
RAG_BACKEND = "qdrant"
RAG_SERVICE_HOST = "127.0.0.1"
RAG_SERVICE_PORT = "8011"
RAG_SERVICE_DATA_ROOT = "~/.praxis-rag/data"
QDRANT_URL = "http://127.0.0.1:6333"
RAG_COLLECTION = "mini_agent_knowledge_pdf_assets_v1"
RAG_WORD_CONVERT_TIMEOUT = "120"
RAG_MYSQL_HOST = "127.0.0.1"
RAG_MYSQL_PORT = "3306"
RAG_MYSQL_USER = "rag"
RAG_MYSQL_DATABASE = "praxis_rag"
# RAG_STATE_URL = "<SQLAlchemy MySQL URL>"
```

配置值使用带引号的字符串。Windows 路径建议使用 `/`，例如 `C:/models`。DOC 转换器和 PDF/Word 图片模型路径也可写入该节，键名与下文环境变量相同。修改后重启生效；配置格式错误会明确报错，不忽略文件继续运行。`RAG_MYSQL_PASSWORD` 和 `RAG_STATE_ENCRYPTION_KEY` 只从环境变量读取，不要写入配置文件。

环境变量优先于文件；`RAG_CONFIG_FILE` 可指定其他配置文件（指定的文件必须存在）。设置 `QDRANT_PATH` 环境变量时不会再套用文件中的 `QDRANT_URL`，反之亦然。若使用本地数据库文件，请删除配置中的 `QDRANT_URL`，改为 `QDRANT_PATH = "~/.praxis-rag/qdrant"`，不要同时配置两者。

源文件、PDF/Word 解析产物、文档清单、知识库配置、MCP 配置和 API Key 默认保存在共享状态数据库 `state.sqlite3`（仅本地开发）；生产环境使用 MySQL。Qdrant 仍需按所选连接方式准备；配置文件不会代替数据库服务。已有 SQLite/本地文件数据请先执行 `rag-state-migrate`，详见 [共享状态存储](state-storage.md)。

仓库开发环境中也可以执行：

```powershell
uv run --package praxis-rag rag-service
```

默认地址：

- Web Console：`http://127.0.0.1:8011/console`
- MCP：`http://127.0.0.1:8011/mcp`
- OpenAPI：`http://127.0.0.1:8011/docs`
- 健康检查：`http://127.0.0.1:8011/health`

Console 包含知识上传、文档删除、BM25/Embedding/Hybrid 检索、Reranker 切换、Top-K 结果、score、source、chunk_id 和阶段耗时。它直接访问同源 RAG API，不经过 Mini-Agent。调试页还提供“三模式对比”，固定关闭重排并并排展示三种 Retriever 的原始结果，避免 Cross Encoder 把不同召回重新排成相同顺序。

启动后在“模型配置”中分别设置 Embedding 和 Reranker 的 URL 和 Key；模型在创建知识库时选择。RAG HTTP 服务不再加载本地 Embedding/Reranker 权重，也不控制推理框架的 CPU/GPU；本地推理框架需自行运行，再填写其地址与端口。未配置时，相应操作明确报错，不回退到本地模型。

PDF 仍由独立 Docling 2.126.0 子进程解析，使用 PDFium、CUDA 和本机解析模型；图片或表格没有有效原文说明时，才使用固定版本的 Granite Vision 3.3 2B 生成检索描述。解析产物（含图片原图）保存在共享状态数据库，图表原始数据和检索文字写入 Qdrant。每批最多 10 个文件，单文件最大 10 MiB。

PDF 公式使用本地 CodeFormulaV2 转为 LaTeX，开启生成缓存，并以紧裁剪、单公式推理减少相邻文字干扰。模型错误、空公式输出或生成被截断会导致该文件解析失败，不静默跳过。模型能够输出 LaTeX 不代表数学内容一定正确，复杂公式仍需要用原页验证。

OCR 不再按默认置信度阈值丢弃已识别文字；低置信度内容保留，并在 `ocr.json`、解析清单和相关检索块的 `source_warnings` 中记录。保留文字不等于识别正确，扫描件仍可能有错字。

每份 PDF 的原生 `document.json`、按阅读顺序保存的 `manifest.json`、正文 `document.md`、OCR 记录 `ocr.json` 和图片保存在共享数据库 `blobs` 表的 `pdf-assets` 桶中，键为 `<document_id>/...`。正文、图片、表格与公式保持 Docling 的遍历顺序；图注、表题、脚注保留原文关联，不按长度丢弃，也不猜配未关联注解。生成描述仅用于检索，不充当原文注解。命令行 PDF 导入和 MCP 正文读取使用同一份解析结果。

### 4. 启动独立文档 worker（MCP 异步新增必需）

在另一个终端或进程管理器中启动消费者；先为该进程配置与 API 相同的 MySQL（或 `RAG_STATE_URL`）、Qdrant、`RAG_STATE_ENCRYPTION_KEY` 和队列连接环境。需要处理 PDF/Word 时，也要准备对应解析依赖和模型。不要假设另一个终端会继承启动脚本加载的 `.env`。

```powershell
rag-document-worker
```

每个 worker 默认同一时间只执行一个消费任务；通过启动多个独立 `rag-document-worker` 进程扩容，它们使用相同文档 topic/group。API 自带轻量 outbox relay，但不内嵌文档消费者。仅需在 API 之外补发 outbox、无需加载模型时可独立运行：

```powershell
rag-document-worker --dispatch-only
```

`--dispatch-only` 只运行 relay，不解析或索引文档，不能替代消费者。API/relay/worker 应共享以下配置；缓存队列环境变量保留原义，不要把文档队列配置写入缓存 topic/group：

| 环境变量 | 默认值 / 作用 |
| --- | --- |
| `RAG_ROCKETMQ_ENDPOINT` | Compose 为 `127.0.0.1:8081`；缓存和文档共用连接 |
| `RAG_ROCKETMQ_NAMESPACE` | 可选 RocketMQ namespace，与知识库 namespace 不同 |
| `RAG_ROCKETMQ_ACCESS_KEY` / `RAG_ROCKETMQ_SECRET_KEY` | 可选凭据，仅从环境提供 |
| `RAG_ROCKETMQ_TLS` | `false`；共享 TLS 开关 |
| `RAG_ROCKETMQ_TOPIC` / `RAG_ROCKETMQ_GROUP` | 原缓存刷新队列，保持不变 |
| `RAG_DOCUMENT_TOPIC` | `praxis_document_ingest` |
| `RAG_DOCUMENT_GROUP` | `praxis_document_ingest_workers` |
| `RAG_DOCUMENT_MAX_ATTEMPTS` | `3`；文档任务最大尝试次数 |
| `RAG_DOCUMENT_LEASE_SECONDS` | `120`；文档任务租约秒数 |
| `RAG_DOCUMENT_RETRY_SECONDS` | `10`；文档任务重试退避基数，乘以已尝试次数 |

升级时新增 `document_tasks` 和 `document_task_outbox` 两张表。默认自动建表；若关闭 `RAG_STATE_AUTO_SCHEMA`，先由部署账号调用共享 schema 的 `metadata.create_all(engine)` 创建缺失表，再启动服务。排队任务记录知识库配置版本，消费前或发布时配置已经改变会拒绝发布，需要按新配置重新提交。

本地 Broker 的 `autoCreateTopicEnable` 和 `autoCreateSubscriptionGroup` 都为 `false`。启动脚本沿用 `mqadmin updatetopic` 和 `mqadmin updateSubGroup`，显式创建缓存与文档两组资源，全部成功才继续启动服务。生产部署或手动启动时也必须预先创建两组 topic/group，并确保连接凭据具有相应权限。

### 知识库模型配置

创建知识库时填写 Embedding 的 URL、Key 并从下拉列表选择模型；Reranker 可选，每个知识库独立保存配置。侧边栏“知识库”可以查看和修改配置，Key 默认隐藏，点击眼睛后按需读取。

- 修改 Embedding URL、模型或向量长度必须重新向量化：使用已有片段建立新索引，全部成功后才切换配置；失败保留原索引。原文件、附件和旧集合不删除。
- 仅修改 Key 或 Reranker 不需要重新向量化。模型列表使用当前表单中的 URL/Key 获取。
- URL 保留路径前缀，例如服务要求 /v1 时应填写完整地址；不自动补 /v1。创建和保存会验证实际模型调用。
- 配置保存在共享状态数据库的 `knowledge_bases` 表；Windows Key 使用 DPAPI 加密，其他系统使用状态加密密钥，多实例部署必须设置 `RAG_STATE_ENCRYPTION_KEY`。Key 不进入浏览器存储，修改地址不会复用旧 Key。
- 管理接口仅允许本机直连；普通文档和检索接口仍使用服务 API Key，不能与模型提供方 Key 混用。
- POST /v1/admin/knowledge-bases 创建；PATCH /v1/admin/knowledge-bases/{namespace} 修改，reembed=true 执行重新向量化。旧全局 /v1/model-settings 接口已移除。
- 分词器由 RAG_CHUNK_TOKENIZER_MODEL 指定，与远端模型别名分开配置。重新向量化不重新切分文档。

真实 API 浏览器验收需要独立 RAG 服务、独立数据目录和真正运行模型的推理服务：

```powershell
conda activate dev
$env:RAG_MODEL_API_TEST_URL = "http://127.0.0.1:8031"
$env:RAG_MODEL_PROVIDER_URL = "http://127.0.0.1:8032/v1"
.venv\Scripts\python -m pytest tests/test_real_model_api_ui.py -q -s
```

本仓库的测试辅助服务 `tests/model_api_inference_server.py --port 8032` 在 `dl` 环境中使用真实缓存 BGE 模型提供验收接口（模型别名 `served-embedding`、`served-reranker`），不属于生产 RAG 服务。测试截图在 `.test-tmp/model-api-browser/`。

### 前端开发与打包

Console 使用 React、TypeScript 和 Ant Design，提供浅色与深色主题。主题和 Namespace 保存在当前浏览器；Service Token 仅保存在当前会话，MCP 配置示例不会显示真实 Token。

Console 正常运行只需启动 `rag-service`，通过 `/console` 打开页面，不需要 Node.js 或额外前端进程；MCP 异步新增另需独立 `rag-document-worker`。仓库中的 `src/rag_mcp/web/` 是随 Python 包交付的构建文件，修改界面请编辑 `frontend/src/`，不要手动修改构建文件。

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
| `token` | 使用服务配置的 tokenizer，按原文位置截取 token 窗口。 |
| `sentence` | 完整句子合块，超长单句按 token 拆分。支持常见中英文断句和英文缩写，不做语言模型语义判断。 |
| `recursive` | 段落、句子、token 逐级细分，再合并完整内容单元。 |
| `structure` | Markdown 按标题；TXT 按空行段落；PDF 按页、段落、句子；DOCX 按标题样式及段落，表格过长按行处理。 |

短内容按原文顺序合并，允许跨章节和 PDF 页面。重叠计入长度上限，句子重叠可能少于设置值或为零；超长句子使用 token 重叠。chunk 元数据记录字符位置、实际页码、标题路径和段落位置。PDF 正式上传按 Docling 的页码和版面顺序生成正文块；图片按原图注检索并返回原图，表格按表题、表头和行内容检索并返回行列及合并信息。扫描文本使用 Docling OCR；无法稳定恢复结构的表格会终止该文件入库。

PDF 正文实际执行所选切分策略；公式整体保留，表格按完整行附带全部表头层级入库，不跨图表或公式边界拼接正文。表格的 `header_rows` 保存多层表头，`cells` 保存合并范围，空列不会被删除。不可拆分的公式、图注或表格行可以超过目标块长度，但不得超过 Embedding 模型输入上限，超过时明确报错。PDF 块记录原始引用、页码及位置；跨页原文保留全部 `locations`，`page` 是起始页。普通正文的字符偏移对应当前连续正文段，不是 PDF 文件字节位置；来源的 `charspan` 对应 Docling 原始元素。

- `GET /v1/chunking/strategies` 返回策略选项、单位和默认参数。
- `POST /v1/documents` 在原 `files` multipart 字段外接收 `strategy`、`chunk_size`、`chunk_overlap`。
- `POST /v1/documents/check` 接收 JSON：`{"files":[{"name":"guide.md","sha256":"64位小写十六进制哈希"}],"strategy":"recursive","chunk_size":512,"chunk_overlap":64}`，返回同序的 `files`，每项 `document` 为已有文档或 `null`。
- 三个接口均沿用 `X-RAG-Namespace` 和服务认证。去重仅在同一 namespace 内比较文件 SHA-256、策略、长度、重叠、Embedding 模型及 tokenizer 标识；文件名不参与比较。
- PDF 去重还比较 `pdf_parser_version`，解析器更新后重新上传会重新索引。旧 PDF 不自动迁移，缺少当前版本解析正文时 MCP 会要求显式重新导入，不回退到旧的纯文本提取。
- 浏览器先计算哈希并检查，命中则不发送文件正文。需要 HTTPS 或 localhost 安全连接。后端接收正文时重新计算哈希，并在租户锁内再次去重。
- 上传结果分别包含 `imported`、`skipped`、`failed` 和 `documents`。更改参数会重新索引；改名但配置相同则返回已有文档，不新建别名。已有文档不会自动重建。
- 旧清单首次检查时按需补齐哈希，缺失配置按原 700/100 字符策略解释。tokenizer 下载或加载失败会明确报错，不退回字符计数。

真实验收（在已安装 `.[dense]` 和 `pytest` 的 `dl` 环境运行）：

```powershell
conda activate dl
$env:RAG_REAL_SMOKE = "1"
$env:RAG_MODEL_PROVIDER_URL = "http://127.0.0.1:8032/v1"
python -m pytest tests/test_real_chunking.py -q -s
```

该测试用真实 BGE-M3、BM25 和本地 Qdrant，在临时目录中验证五种策略及四种文件。浏览器验收另需 `playwright` 和本机 Edge：设置 `RAG_BROWSER_URL` 为独立测试服务地址后，运行 `tests/test_browser_chunking.py`；截图保存在 `.mini_agent-rag/chunking-ui/`。

PDF 最多解析 1000 页；DOCX 解压后的内部文件总量最多 50 MiB；单个文档最多提取 500 万字符。扫描版 PDF 使用本地 OCR；旧版二进制 `.doc` 由独立 LibreOffice 进程转换后解析。

### Word 解析和图片检索

```powershell
$env:RAG_WORD_CONVERTER_PATH = "C:/Program Files/LibreOffice/program/soffice.exe"
$env:RAG_WORD_CONVERT_TIMEOUT = "120"
# 无图注的图片使用已有 Docling + Granite Vision 环境和模型目录。
$env:RAG_PDF_PARSER_PYTHON = "C:/path/to/pdf-parser/python.exe"
$env:RAG_PDF_MODELS_PATH = "C:/path/to/docling/models"
```

- DOCX 读取正文、内容控件、文本框、页眉页脚、脚注尾注及嵌套表格。只保留最终正文，忽略删除和移出修订。
- 表格保留空列、合并范围、嵌套单元格、表题及表注。每行检索文字携带表头，命中后返回完整结构。
- 原生公式通过 `docxlatex` 转为 LaTeX；不支持的公式结构、损坏内容、外链图片及未能提取的原生图表会明确拒绝入库。
- 图片保存原始字节。真实图注用于检索；没有图注时生成简短视觉描述，仅用于索引，不冒充图注。Word 视觉推理缩放输入到最长边 384 像素，不改变保存的原图；拒答或无效描述导致入库失败。
- 图注、表注按编号、明确引用和相邻关系关联。仍有歧义的对象不保存，注释文字保留，上传结果与文档详情显示跳过数量、位置和原因。
- Word 资产保存在共享数据库 `blobs` 表的 `word-assets` 桶中，与源文件和索引一同更新、回滚和删除。旧文档需重新上传才能使用新解析结果，不会自动重建已有知识库。
- REST 图片结果的 `asset.data` 是 Base64，`asset.mime_type` 是实际类型；MCP 返回图片内容块。Word 使用章节、段落和对象位置，不提供虚构页码或坐标。控制台支持图片下载和表格展示。
- DOC 转换使用独立临时目录及 LibreOffice 配置，禁用宏和链接更新。DOC 原文件仍保留；其图片字节以转换后的 DOCX 为准。转换超时、工具缺失或加密文档会报错，不降级成不完整文本。

真实验收：在 `dev` 的隔离环境运行 `tests/test_real_word_fidelity.py`，设置 `RAG_REAL_WORD=1`、上述转换/模型配置，以及指向测试模型服务的 `RAG_MODEL_PROVIDER_URL`、`RAG_TEST_EMBEDDING_MODEL`。涉及 PyTorch 的进程使用 `dl` 或基于 `dl` 的模型环境。测试使用 `.test-tmp/word-real` 下的独立数据，不访问正式知识库。控制台验收使用 `RAG_WORD_PREVIEW_URL` 和 `tests/test_browser_word.py`。

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

MCP 提供 `search`、文件管理、`get_document_status` 等工具。若 Host 使用 `rag` 作为 server 名，搜索工具展示名通常是 `mcp_rag_search`。

### MCP 文件管理

文件管理和状态工具只操作连接时 `X-RAG-Namespace` 指定的知识库，使用普通 Service Token，不需要管理 Token。请仅向可信客户端提供具有写入权限的连接；工具提示不能替代权限控制。管理员可通过工具管理接口分别禁用这些工具。

| 工具 | 参数 | 行为 |
|---|---|---|
| `add_document` | `name` 和 `content` 或 `content_base64`；可选 `idempotency_key` | 异步新增，不覆盖同名文档；成功仅返回 `artifact_id`，不代表索引完成 |
| `get_document_status` | `artifact_id` | 查询该新增任务的 `artifact_id`、`status`、`stage`、`error` |
| `update_document` | `name` 和 `content` 或 `content_base64` | 仍为同步：完整替换已存在文件并重建索引，保留切分设置；不存在时报错 |
| `delete_document` | `name` | 仍为同步：删除文件和索引；不存在时报错 |

`name` 必须是含扩展名的准确文件名，不能传路径。支持 TXT、Markdown（`.md`）、PDF、DOCX、DOC，单文件最大 10 MiB。TXT/Markdown 可使用 UTF-8 文本 `content`；PDF/DOCX/DOC 必须由客户端读取文件后传标准 Base64 到 `content_base64`。两种内容参数只能传一种，不支持服务器路径或 URL。

```json
{"name": "manual.txt", "content": "E03 表示温度传感器故障。", "idempotency_key": "manual-import-20261023-001"}
```

#### 异步新增与状态轮询

`add_document` 先将不可变原始字节、任务及 outbox 持久化到 MySQL，再等待 RocketMQ 发送确认；成功的工具结果**只有**以下对象，不含 `status`、文档或 URI：

```json
{"artifact_id": "123e4567-e89b-42d3-a456-426614174000"}
```

此 UUID 表示已接收（accepted），不是已经解析、索引或可检索（indexed）。独立 worker 才负责解析、切分、索引与发布；尚未启动消费者时任务可以保持 `queued`。客户端在同一 namespace 调用 `get_document_status`，按需间隔轮询，直到终态：

```json
{"artifact_id": "123e4567-e89b-42d3-a456-426614174000"}
```

状态结果包含以下字段（`stage` 为当前阶段，以下阶段文字仅为示意，不是固定枚举；`error` 为失败信息）：

```json
{
  "artifact_id": "123e4567-e89b-42d3-a456-426614174000",
  "status": "processing",
  "stage": "<当前处理阶段>",
  "error": null
}
```

`status` 为 `queued`、`processing`、`succeeded` 或 `failed`；仅 `succeeded` 表示完成索引发布，此时刷新文档列表/资源列表再读取正文或检索。`failed` 时查看 `stage`、`error` 排查，不要把收到 UUID 当成索引成功。状态展示使用 Redis 缓存，未命中、未配置或不可用时回源 MySQL；缓存可能短暂滞后，MySQL 才是任务、租约和发布权威。

队列故障、发送确认超时或客户端断线时，调用失败不一定意味着未提交：MySQL 中可能已有字节、任务和待补发 outbox，API 自带 relay 或 `--dispatch-only` 可继续补发。不要换 key 盲目重复新增；从首次调用起提供 `idempotency_key`，对同一次提交使用相同 key 和完全相同的参数/内容重试，以找回同一任务。新内容使用新 key。

这里的上传 `artifact_id` 用于 `get_document_status`，不是搜索快照的 `artifact_id`，不能直接用于 `rag://artifacts` 或 `/v1/artifacts` 读取。上传原始字节持久保留在 MySQL，**无上传 TTL**，不随 Redis 状态缓存或搜索结果引用过期自动清理；部署时为持续增长的数据安排容量、备份和保留策略。worker 在独立进程提交后，API **不会自动推送 MCP 资源变更通知**；客户端必须轮询状态并主动刷新 `resources/list` 或文档列表，不能依赖订阅确认入库。

新增使用提交时对应版本的知识库切分配置（新知识库默认递归切分）。同内容不同文件名会分别保存，确保更新始终针对指定文件。同名、同内容、同切分设置的同步更新返回 `unchanged`；同步更新/删除的成功结果仍使用原有 `status`、`document`、`uri` 协议，不受异步新增影响。HTTP 上传也仍为同步请求，完成索引后返回原有上传结果。工具参数错误或调用失败返回 MCP `isError=true`；异步执行失败通过状态查询报告。整个请求另有大小上限，超过后返回 HTTP 413；既有文档解析安全上限仍然适用。删除无法通过 MCP 撤销，更新前需要保留旧版本时应先读取或备份。

搜索工具输入：

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
uv run --package praxis-rag rag-mcp-debug "RRF 怎么融合 BM25 和 Dense？"
uv run --package praxis-rag rag-mcp-debug --schema
```

协议测试会启动真实本机 HTTP 服务，使用官方客户端和真实文件存储，但以可控检索器和 PDF 解析结果替代模型，不下载模型或连接 Qdrant。这不等于真实模型效果验证。

本地 GPU 的 PDF 完整链路验收使用 `tests/test_real_pdf_fidelity.py`。先激活 `dl`，指定已安装依赖的解析器 Python、本地模型目录和样本目录，再执行：

```powershell
conda activate dl
$env:PYTHONPATH = "$PWD/src"
$env:HF_HUB_OFFLINE = "1"
$env:TRANSFORMERS_OFFLINE = "1"
$env:RAG_REAL_PDF = "1"
$env:RAG_MODEL_PROVIDER_URL = "http://127.0.0.1:8032/v1"
$env:RAG_PDF_PARSER_PYTHON = "C:/path/to/pdf-parser/python.exe"
$env:RAG_PDF_MODELS_PATH = "C:/path/to/docling/models"
$env:RAG_PDF_SAMPLES = "C:/path/to/pdf/samples"
$env:RAG_PDF_SAMPLE_FILES = "aviation.pdf,doclaynet.pdf,mineru_demo2.pdf,scan.pdf"
& $env:RAG_PDF_PARSER_PYTHON -m pytest tests/test_real_pdf_fidelity.py -q -s
```

此测试在独立临时目录启动 HTTP 服务和本地 Qdrant，调用真实 Docling、CodeFormulaV2、OCR、BGE-M3；检查上传、重复检测、检索和 MCP 全文读取。耗时、显存分配峰值、公式与图表数量、检索结果写入 `.test-tmp/pdf-fidelity-real/<run>/report.json`。这些检查证明链路保留情况，不等于公式语义或版面识别准确率；仍须对照原页和原生结果检查。

实测结果及未解决样例见 [PDF 修复验证记录](../benchmarks/PDF_VALIDATION.md)。

### Conda 开发环境

其他项目可能仍依赖 MCP 1.x。为避免改动共享环境，在激活 `dev` 后给本项目创建独立虚拟环境：

```powershell
conda activate dev
python -m venv --system-site-packages .venv
.venv\Scripts\python -m pip install -e ".[api]"
.venv\Scripts\python -m pytest tests -q
.venv\Scripts\python -m rag_mcp.http_service
```

HTTP 模型检索需要在控制台设置可用的模型 API，并准备 Qdrant；上述协议测试不调用模型。独立命令行的本地模型流程未改动，仍需要 `.[dense]`。

## MCP 双版本

依赖固定为 `mcp==2.1.1`，同一个 `/mcp` 支持以下两种流程，客户端不能把新版版本号放进旧版 `initialize` 来切换流程：

```text
2025-11-25: initialize → notifications/initialized → tools/list / tools/call
2026-07-28: server/discover（可选）→ tools/list / tools/call → subscriptions/listen（按需）
```

新版每次请求携带自己的协议信息，不要求先建立旧版会话。协议头和请求格式应由 SDK 处理。Python SDK `Client` 的 `mode="legacy"` 使用旧版，默认 `mode="auto"` 发现新版，`mode="2026-07-28"` 跳过发现直接使用新版。

请求均携带 `X-RAG-Namespace` 和配置要求的 Service Token。旧会话不能跨 namespace 使用；订阅固定建立时的 namespace。工具调用取消只停止等待和发送结果，不强停后台模型线程。

HTTP MCP 注册搜索、文件管理及 `get_document_status` 等工具；本地搜索调试入口仍只提供搜索。新增工具要在项目中登记定义、参数校验和执行函数；配置接口不能上传代码，也不能注册外部 HTTP 转发。

## 工具与提示词管理

管理接口仅允许本机直接访问，不使用管理员凭证。MCP 管理接口仍需 `X-RAG-Namespace` 指定知识库。普通 MCP/REST 请求继续使用访问 API Key。

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

配置保存在共享状态数据库的 `mcp_catalogs` 表，重启恢复，管理配置不会加载检索模型。管理 Token 仍可管理所有 namespace，不提供用户级权限隔离。配置修改请走管理接口，不手工编辑数据库记录。

## 文档资源与订阅

- `resources/list` 返回当前知识库已导入文档，列表每页最多 50 项，按 `nextCursor` 继续；`resources/templates/list` 返回分页地址规则。
- 文档地址为 `rag://knowledge/<namespace>/<编码后的文件名>`，`resources/read` 默认读取第一页，不返回本地路径。
- 正文按段落分页，通常每页最多 8000 字符；长公式独占一页且不截断，可以超过此值。单个公式已在入库时受模型输入上限约束。这不是 PDF 原始页码，也不是自动识别的章节。
- 返回标准文本资源，其中 `text` 是 JSON：`uri`、`text`、`page`、`total_pages`、`version`、`has_more`、`truncated` 和 `next_uri`。`truncated=true` 表示当前页未包含剩余正文，可继续读取 `next_uri`。
- 下一页地址携带文档版本。文档替换后旧的下一页地址会报错，客户端应从文档 URI 重读。解析安全上限与上传规则不变，超限或解析失败明确报错，不静默截断整份文档。
- 指定资源更新订阅使用稳定的文档 URI，不使用分页 URI；删除后通知订阅者，后续读取返回不存在。

新版 `subscriptions/listen` 可选择工具列表、提示词列表、资源列表及指定文档更新。旧版仍使用列表变化通知与 `resources/subscribe`、`resources/unsubscribe`。通知只表示“发生变化”，客户端需要重新列出或读取内容。

启停工具或修改描述、增删改提示词，以及 API 进程内完成的同步上传/替换/删除，会在对应操作成功后通知。失败项和没有变化的重复操作不通知；同名相同内容同步上传跳过重复索引。通知不会跨 namespace。MCP `add_document` 返回 UUID 时尚未发布文档；独立 worker 的提交不会自动触发 API 的 MCP 资源推送，客户端须轮询 `get_document_status` 并刷新列表。

订阅通知仍仅在 API 进程内投递，不提供跨进程通知、历史事件重放或断线补发；这不限制独立文档 worker 进程的横向扩容。订阅断线后客户端重新订阅并读取最新状态。服务退出时释放会话与订阅，不部署 OAuth、账户系统或云服务。

## 当前限制

- 文档解析支持 Markdown/TXT、电子版和扫描版 PDF、DOCX，以及安装 LibreOffice 后的 DOC；Excel、网页抓取和独立图片上传尚未实现。
- PDF 使用本机 Docling、OCR 和公式模型；不能保证任意扫描件、旋转表格或复杂公式都准确。公式处理针对 Docling 检出的公式块，不保证所有行内数学符号转换为 LaTeX。明确的解析错误会失败，但不能自动识别所有内容错误。
- HTTP `embedding` 使用配置的兼容模型 API，向量长度由实际响应检测。
- `hybrid` 使用 Qdrant Dense + Sparse 预取和 RRF 融合。
- 重排仅支持 `none`（不重排）与 `cross_encoder`（调用配置的 Reranker API）。
- Console 会同时展示 Cross Encoder 最终分数和重排前的 `retrieval_score`；请求失败时会清空旧结果并保留错误面板。
- 推理服务离线、认证失败或返回格式不兼容时明确报错；可选择“不重排”验证不依赖 Reranker 的召回流程。
- HTTP/Console 上传及 MCP 更新、删除仍为同步请求，大文件索引期间网页会等待；仅 MCP `add_document` 为异步，需独立 worker 和状态轮询。
- Console 用于本地开发调试，不提供账户系统或细粒度多用户权限。

历史阶段和变更记录见 [PROGRESS.md](../PROGRESS.md)。

## 第一批 Redis 缓存

Compose 包含 Redis 和 RocketMQ。安装可选队列 SDK：`pip install -e ".[cache]"`，使用启动脚本会创建缓存刷新和文档入库的两组 topic/group，并加载本地环境变量（不启动文档消费者）。直接启动 rag-service 时自行设置 RAG_REDIS_URL 和 RAG_ROCKETMQ_ENDPOINT；不配置 Redis 默认保持原有 MySQL 读取。命中缓存不查 MySQL；执行鉴权/租约和上传发布校验不使用缓存。详见 [状态缓存与可靠刷新](state-cache.md)。

## 第二批状态缓存（严格模式）

API Key 管理列表、发布版本展示、租约执行者展示和事务/步骤/回滚进度已接入 Redis。新增 `/v1/uploads`、`/v1/uploads/{operation_id}` 查询当前 namespace 的状态；本机管理员可查询 `/v1/admin/published-versions` 和 `/v1/admin/upload-leases`。缓存快照只用于查询展示；实际鉴权、版本选择、租约认领、提交和回滚仍查 MySQL。无需新增环境变量，沿用第一批 outbox、修订号和 RocketMQ 重试。详见 [第二批状态接口及安全边界](state-cache.md#第二批状态查询接口)。

## 按 UUID 复用搜索与产物

search 响应保留原 results，并新增 result_id、expires_at 和 artifacts。MCP get_search_result 或 GET /v1/search-results/{result_id} 可在 1 小时内读取原快照；rag://artifacts 资源或 /v1/artifacts/{artifact_id} 获取源文件、图片、JSON、Markdown 等。不重新执行模型或检索。文档删除为 del_flag 软删除，不影响已签发 UUID；鉴权仍检查当前 Key 权限。详见 [UUID 结果与产物引用](result-references.md)。
