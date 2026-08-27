# Standalone RAG MCP

这是一个可独立安装和运行的 Agentic RAG 模组，不依赖 Mini-Agent Backend 或主前端。一个安装包提供两个进程：

- `rag-service`：拥有知识源文件、切分、Embedding、BM25、Qdrant、Reranking、HTTP API 和独立 Web Console。
- `rag-mcp`：标准 stdio MCP 适配器；收到 Agent 的 `search` 调用时才连接 `rag-service`。

```text
浏览器 -> http://127.0.0.1:8011/console
                         |
任意 MCP Host -> rag-mcp |-> rag-service :8011 -> Qdrant :6333
                         |
                 同一个 namespace
```

RAG 只返回证据，不生成最终回答。是否调用、是否改写查询、是否多跳检索以及如何组织答案，仍由 Agent 决定。

## 安装

在本仓库中开发：

```powershell
cd D:\mini_agent-main
uv sync --all-packages --extra dense
```

作为独立工具安装当前目录：

```powershell
cd D:\mini_agent-main
uv tool install --editable ".\rag_mcp[dense]"
```

从独立 Git 仓库直接安装：

```powershell
uv tool install "mini-agent-rag-mcp[dense] @ git+https://github.com/Guan0923/xinghe_rag.git"
```

安装后，`rag-service`、`rag-mcp`、`rag-mcp-debug` 和 `rag-mcp-ingest` 都是独立命令。

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
$env:RAG_COLLECTION = "mini_agent_phase3_hybrid"
$env:RAG_EMBEDDING_DEVICE = "cpu"
$env:RAG_RERANKER_DEVICE = "cpu"
$env:RAG_SERVICE_DATA_ROOT = "$HOME/.mini_agent-rag"

rag-service
```

仓库开发环境中也可以执行：

```powershell
uv run --package mini-agent-rag-mcp rag-service
```

默认地址：

- Web Console：`http://127.0.0.1:8011/console`
- OpenAPI：`http://127.0.0.1:8011/docs`
- 健康检查：`http://127.0.0.1:8011/health`

Console 包含知识上传、文档删除、BM25/Embedding/Hybrid 检索、Reranker 切换、Top-K 结果、score、source、chunk_id 和阶段耗时。它直接访问同源 RAG API，不经过 Mini-Agent。调试页还提供“三模式对比”，固定关闭重排并并排展示三种 Retriever 的原始结果，避免 Cross Encoder 把不同召回重新排成相同顺序。

首次上传或 Dense 检索时会下载并加载 BGE-M3。上传后会自动切分，并把 Dense Vector 与 BM25 Sparse Vector 写入 Qdrant。目前只接受 UTF-8 `.md` 和 `.txt`，每批最多 10 个文件，单文件最大 10 MiB。

## Namespace

namespace 是一个独立知识库标识，例如 `default`、`product-manuals` 或 `team-a`。Console 与 MCP 配置必须使用相同值，否则 Agent 检索不到网页上传的文档。

允许 1～64 个字符，只能包含字母、数字、点、下划线和横线。知识源文件位于：

```text
~/.mini_agent-rag/<namespace>/knowledge/
~/.mini_agent-rag/<namespace>/manifest.json
```

namespace 不在 Agent 可见的工具 Schema 中；它由 MCP 进程配置并通过 Header 发送给服务。

## 接入任意 stdio MCP Host

通用配置：

```toml
[servers.rag]
command = "rag-mcp"
enabled = true

[servers.rag.env]
RAG_SERVICE_URL = "http://127.0.0.1:8011"
RAG_NAMESPACE = "default"
```

Mini-Agent 使用相同的用户级 `mcp/servers.toml` 配置。开启其 `capabilities.mcp=true` 后新建 Agent run，即会通过通用 MCP 加载器发现工具；Mini-Agent 没有 RAG 专用路由或内置 RAG ToolSpec。

MCP 内部工具名为 `search`。若 Host 使用 `rag` 作为 server 名，展示名通常是 `mcp_rag_search`。

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

默认只监听 loopback。绑定非 loopback 地址时必须设置 Token：

```powershell
$env:RAG_SERVICE_HOST = "0.0.0.0"
$env:RAG_SERVICE_TOKEN = "replace-with-a-random-secret"
rag-service
```

MCP 配置不要保存明文密钥：

```toml
[servers.rag.env]
RAG_SERVICE_URL = "https://rag.example.com"
RAG_NAMESPACE = "product-manuals"

[servers.rag.env_refs]
RAG_SERVICE_TOKEN = "env://RAG_SERVICE_TOKEN"
```

Console 的 Token 只保存到浏览器 `sessionStorage`。生产环境还应在 RAG Service 前配置 HTTPS、访问控制和请求大小限制。

## 本地直连模式

不配置 `RAG_SERVICE_URL` 时，`rag-mcp` 仍可在自己的进程中直接加载模型和连接 Qdrant，用于兼容或单进程部署。推荐日常使用远程适配模式，以避免 MCP Host 重复加载重模型：

```text
推荐：rag-mcp --HTTP--> rag-service --模型/Qdrant
兼容：rag-mcp ----------> 模型/Qdrant
```

## 开发与验证

```powershell
uv run --no-sync python -m pytest rag_mcp/tests -q
uv run --no-sync python -m ruff check rag_mcp
uv run --package mini-agent-rag-mcp rag-mcp-debug "RRF 怎么融合 BM25 和 Dense？"
uv run --package mini-agent-rag-mcp rag-mcp-debug --schema
```

`rag-mcp` 是 stdio 服务，手工运行后等待 MCP 客户端输入、终端看起来没有输出是正常现象。浏览器调试应使用独立 Console。

## 当前限制

- 文档解析只支持 Markdown/TXT；PDF、Word、Excel、网页抓取和图片 OCR 尚未实现。
- `embedding` 使用 BGE-M3，默认 1024 维。
- `hybrid` 使用 Qdrant Dense + Sparse 预取和 RRF 融合。
- `cross_encoder` 使用真实 BGE Reranker；`bi_encoder` 仍是确定性 Mock。
- Console 会同时展示 Cross Encoder 最终分数和重排前的 `retrieval_score`；请求失败时会清空旧结果并保留错误面板。
- Cross Encoder 默认延迟加载。若首次调用因模型源网络不可达而失败，可先选择“不重排”或“三模式对比”验证召回，再准备本地模型缓存并重启服务。
- 上传目前是同步请求，大文件索引期间网页会等待。
- Console 用于本地开发调试，不提供账户系统或细粒度多用户权限。

历史阶段和本轮变更记录见 [PROGRESS.md](PROGRESS.md)。
