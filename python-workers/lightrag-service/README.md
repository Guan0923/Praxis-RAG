# LightRAG 图谱索引与上下文检索侧车

Praxis-RAG 的独立 HTTP 微服务：把 Go 端**已经解析、已经切分**的文本块灌入
[LightRAG](https://github.com/HKUDS/LightRAG)（PyPI 包名 `lightrag-hku`）构建知识图谱，并在查询时**只返回上下文**
（文本块、实体、关系），绝不返回 LLM 生成的答案文本。

它解决的是一件事：Praxis 的证据链路由 Go 端负责，LightRAG 只负责图谱索引与双层检索；
两者通过本目录定义的固定 HTTP 契约对接。

## 1. 边界（设计约束）

1. **只接收切好的块**。请求里的 `chunks[].text` 原样灌入 LightRAG 的 custom chunks 接口，
   服务不解析文件、不重新切分、不改写文本（仅应用 LightRAG 自身的文本清洗以保证块 id 一致）。
2. **查询只取上下文**。生产适配器调用 `aquery_data`（内部 `only_need_context=True`），在 LLM 生成前停止；
   响应里不存在 `answer`/`response`/`llm_response` 之类的字段。
3. **一个 Praxis namespace 一个 LightRAG workspace**，名称为 `praxis_<namespace>`，
   存储目录也互相独立（`<WORKING_DIR>/workspaces/praxis_<ns>/lightrag`），知识库之间不可能串数据。
4. **引擎可注入**。`app/engine.py` 定义抽象 `GraphEngine`；真实适配器 `app/lightrag_engine.py` 在方法内部
   延迟导入 `lightrag`；测试注入内存 fake，所以**不安装 `lightrag-hku` 也能跑全部测试**。
5. **默认只监听 loopback**（`127.0.0.1:8093`）。绑定非 loopback 地址时若没有配置
   `RAG_LIGHTRAG_SERVICE_TOKEN`，进程**拒绝启动**；配置了 token 后所有 `/v1/*` 都要求
   `Authorization: Bearer <token>`（`/healthz` 除外）。

## 2. 版本与依赖

| 项目 | 值 |
| --- | --- |
| 服务名 | `praxis-lightrag-service` |
| 服务版本 | `0.1.0` |
| 上游包 | `lightrag-hku` |
| 固定版本 | **`1.5.7`**（`requirements.txt`，代码里同样记录为 `LIGHTRAG_PIN`） |
| Python | 3.11（`lightrag-hku` 要求 >= 3.10） |
| 测试依赖 | 见 `requirements-dev.txt`（**不含** `lightrag-hku`） |

### 为什么必须 pin 精确版本

本服务映射的是 LightRAG 的**内部行为**，不是稳定 API：

- custom chunk 的块 id 由 `(doc_id, 文本)` 派生（`make_custom_chunk_id`），Praxis 的 `chunk_id`
  能否在查询结果里回填完全依赖这一派生规则；
- `aquery_data` 的返回结构（`data.entities` / `data.relationships` / `data.chunks`、
  `source_id` 用 `<SEP>` 连接）决定了上下文能否映射回 Praxis 证据；
- 删除接口的返回状态（`success` / `not_found` / `not_allowed`）决定 404/503 的语义；
- per-role LLM 配置（`role_llm_configs={"extract":..., "keyword":...}`，1.5.x 引入）决定
  抽取模型与关键词模型能否分开绑定。

任何一次升级都必须重新核对 [第 8 节](#8-调用-lightrag-sdk-的假设升级核对清单) 的假设，并跑本目录测试。
不要用 `lightrag-hku>=...` 的浮动范围：上游把 TS 代码迁移到 Python、重命名 role 配置或调整
`aquery_data` 形状时，契约会静默错位。

## 3. HTTP 接口

基础地址：`http://127.0.0.1:8093`。除 `/healthz` 外，配置了 token 时所有 `/v1/*` 都要求
`Authorization: Bearer <token>`。错误统一为 `{"detail":"<安全文案>"}`：
参数非法 400、未授权 401、namespace 不存在 404、上游/引擎故障 503、处理失败 500。
错误文案不会回显文档正文、查询原文或密钥。

### `GET /healthz`（根路径 `/` 同义）

始终 200，即使配置或依赖有问题（便于容器健康检查与运维发现）：

```json
{
  "status": "ok",
  "service": "praxis-lightrag-service",
  "version": "0.1.0",
  "upstream": {
    "package": "lightrag-hku",
    "pinned": "1.5.7",
    "installed": null
  },
  "workspaces": 2,
  "config_error": null
}
```

- `upstream.installed`：运行时 `importlib.metadata` 读到的实际版本，未安装为 `null`；
- `workspaces`：已持久化绑定的 namespace 数；
- `config_error`：安全的配置问题说明（非 loopback 无 token、端口非法、LightRAG 未安装等）。

`namespace` 与 Go 端知识库一致：字母/数字/`_` `.` `-`，1–64 位，首尾必须是字母或数字，
且不能是 Windows 保留设备名（`con`、`prn`、`aux`、`nul`、`com1`…）。`config_fingerprint`
按不透明 token 处理（Go 端实际发送纯 SHA-256 十六进制），只要 1–256 位可打印字符即可。
`language` 接受 `zh`/`en` 等短代码并归一化为 LightRAG 的 `Chinese`/`English`，其它值原样透传。

### `POST /v1/workspaces`（幂等）

确保 workspace 存在并绑定模型连接：

```json
{
  "namespace": "legal",
  "config_fingerprint": "sha256:...",
  "language": "Chinese",
  "embedding": {"base_url": "http://127.0.0.1:11434/v1", "model": "bge-m3",
                "api_key": "sk-...", "dimension": 1024},
  "extract_llm": {"base_url": "http://127.0.0.1:11434/v1", "model": "qwen2.5:7b",
                  "api_key": "sk-...", "max_output_tokens": 9000, "timeout_seconds": 300},
  "keyword_llm": {"base_url": "http://127.0.0.1:11434/v1", "model": "qwen2.5:7b",
                  "api_key": "sk-...", "timeout_seconds": 60}
}
```

响应：

```json
{"namespace": "legal", "workspace": "praxis_legal", "created": true, "rebuilt": false}
```

语义：

| 场景 | created | rebuilt | 行为 |
| --- | --- | --- | --- |
| 首次创建 | `true` | `false` | 创建 LightRAG workspace 与绑定文件 |
| 已存在且 `config_fingerprint` 相同 | `false` | `false` | 只确保当前进程有绑定，**不动数据** |
| 指纹不同 | `false` | `true` | 清空该 workspace 的 LightRAG 存储与 Praxis 块元数据后重建 |

绑定（指纹、language）持久化在 `<WORKING_DIR>/state/<namespace>.json`，**不落盘任何密钥**；
密钥只存在于进程内存的引擎绑定中。因此进程重启后 Go 端仍需先调用一次本接口，
服务会用本次请求携带的连接信息重新绑定（已存在的数据不受影响）。

### `POST /v1/workspaces/{namespace}/chunks`

灌入一个文档的已切分块：

```json
{
  "document_id": "doc-1",
  "document_name": "民法典.pdf",
  "chunks": [
    {"chunk_id": "c-1", "text": "...", "page": 3, "heading": "第一编", "upload_version": 2}
  ]
}
```

响应：

```json
{"document_id": "doc-1", "accepted": 400, "skipped": 0, "entities": 123, "relations": 456}
```

- `accepted`：真正交给 LightRAG 的块数（空白文本块被跳过，计入 `skipped`）；
- `entities`/`relations`：LightRAG 为该文档记录的实体/关系数量（尽力而为，取 `full_entities`
  / `full_relations` 行；行缺失时为 0，不影响灌入成功）；
- **替换语义**：同一 `document_id` 重复灌入 = 先删后插。每次灌入前都会先删旧文档，
  避免 LightRAG custom chunks 的"补丁"模式把上一版的块残留下来；
- `chunk_id` 会作为可追溯元数据记录在侧车的块登记表里，查询时原样回填，并且
  `document_name`/`page`/`heading` 一并回填（见 query 接口）；
- 一个文档必须**一次请求发完**（默认上限 10000 块、请求体 32 MiB）。超过时请调大
  `RAG_LIGHTRAG_SERVICE_MAX_BYTES`/`RAG_LIGHTRAG_SERVICE_TIMEOUT`，不能拆成多次调用
  （第二次会被替换语义当成新版本）；
- `chunks` 为 JSON `null` 时按空列表处理（Go 端零值）；`document_name` 可以为空字符串，
  此时查询上下文里的 `document_name` 也是 `""`，由 Go 端回退为 `document_id` 渲染引用。

### `DELETE /v1/workspaces/{namespace}/documents/{document_id}`

```json
{"document_id": "doc-1", "deleted": true}
```

删除文档及其派生的图谱数据。`deleted=false` 表示引擎与登记表都不认识该文档（幂等删除）。

### `POST /v1/workspaces/{namespace}/query`（只取上下文）

```json
{
  "query": "用户画像如何生成",
  "mode": "mix",
  "top_k": 10,
  "filters": {"document_ids": ["doc-1"]},
  "include_entities": true
}
```

响应：

```json
{
  "contexts": [
    {
      "chunk_id": "c-1",
      "document_id": "doc-1",
      "document_name": "民法典.pdf",
      "page": 3,
      "heading": "第一编",
      "text": "...",
      "score": 0.87,
      "entities": ["用户画像"],
      "relations": [{"source": "A", "target": "B", "description": "..."}]
    }
  ],
  "debug": {"mode": "mix", "entities": 12, "relations": 30, "chunks": 10}
}
```

- `mode`：`local` | `global` | `hybrid` | `naive` | `mix`，默认 `mix`（`bypass` 被拒绝：
  它跳过检索，不会产生上下文）；
- `top_k`：上下文条数上限，同时作为检索宽度（1–100）；`0` 或 `null` 取默认 10（Go 端零值）；
- `filters.document_ids`：按 Praxis 文档过滤；侧车会在映射块 id 后过滤，内部会适当多取；
  为 `null` 或省略时表示不限制（Go 端 nil 切片）；
- `mode` 为空字符串时取默认 `mix`（Go 端未指定模式时发送空串）；
- `include_entities=false`：每个 context 的 `entities`/`relations` 返回空数组（`debug` 仍是上游总数）；
- `document_name`/`page`/`heading` 来自灌入时登记、持久化在
  `<WORKING_DIR>/registry/<namespace>/` 的块元数据；`page` 未知时为 `0`，`heading` 未知时为 `""`；
- `score`：优先使用上游返回的相似度；**1.5.7 的 `aquery_data` 不返回逐块分数**，此时使用
  确定性的排名分 `1/(1+rank)`（`1.0, 0.5, 0.3333, ...`）。它只表示同一次响应的相对顺序，
  不要和 Qdrant 的 cosine 分数直接比较；
- `debug.entities`/`debug.relations`：本次上游返回的实体/关系行总数；`debug.chunks`：实际返回的上下文数。

### `DELETE /v1/workspaces/{namespace}`

```json
{"namespace": "legal", "cleared": true}
```

清空该 namespace 的 LightRAG 存储、块登记表与绑定文件。之后再访问该 namespace 返回 404，
直到重新调用 `POST /v1/workspaces`。

## 4. 数据与目录布局

```
<WORKING_DIR>/
├── state/<namespace>.json                  # 工作区绑定：指纹 / language / 时间戳（无密钥）
├── registry/<namespace>/<sha256>.json      # 块元数据：chunk_id / lightrag_chunk_id / page / heading（无正文）
└── workspaces/praxis_<namespace>/lightrag/ # LightRAG 自己的 KV/向量/图存储
```

- 侧车登记表**不保存块正文**：正文只存在于 LightRAG 存储，避免重复落盘与扩大泄漏面；
- 重启后登记表仍在，所以 `document_name`/`page`/`heading` 无需 Go 端重新发送；
- 清空 workspace 直接删除该 workspace 独立目录（LightRAG 没有"drop workspace" API，见第 8 节）。

## 5. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_LIGHTRAG_SERVICE_HOST` | `127.0.0.1` | 监听地址；非 loopback 且无 token 时启动失败 |
| `RAG_LIGHTRAG_SERVICE_PORT` | `8093` | 监听端口 |
| `RAG_LIGHTRAG_SERVICE_TOKEN` | 空 | `/v1/*` 的 Bearer token；空 = 不鉴权（仅 loopback 安全） |
| `RAG_LIGHTRAG_SERVICE_WORKING_DIR` | `rag_storage` | 工作目录（绑定、块元数据、LightRAG 存储） |
| `RAG_LIGHTRAG_SERVICE_LOG_LEVEL` | `info` | `debug` / `info` / `warning` / `error` |
| `RAG_LIGHTRAG_SERVICE_LOG_FORMAT` | `text` | `text` 或 `json` |
| `RAG_LIGHTRAG_SERVICE_CONCURRENCY` | `2` | 进程内同时执行的引擎操作上限（信号量） |
| `RAG_LIGHTRAG_SERVICE_MAX_BYTES` | `33554432` | 请求体大小上限（32 MiB） |
| `RAG_LIGHTRAG_SERVICE_TIMEOUT` | `900` | 单请求引擎操作总预算（秒），超时返回 503 |

日志只记录 request id、namespace、document_id、模式、块/实体/关系数量和耗时；
**不记录查询原文、块正文、文档名与密钥**。未处理异常只记录异常类型，避免上游异常信息夹带正文。

## 6. 测试（不安装 lightrag-hku）

```powershell
cd python-workers/lightrag-service
python -m pip install -r requirements-dev.txt
python -m pytest
```

测试全部通过 `create_app(settings, engine=FakeEngine())` 注入内存引擎，覆盖：
`/healthz` 契约与配置错误、workspace 创建/幂等/指纹重建/重启持久化、chunks 灌入与替换、
query 上下文契约（含 `document_name`/`page`/`heading`）、文档删除、workspace 清空、
参数非法 400、缺 token 401、上游故障 503、异常 500、请求体上限、并发闸门。
`tests/conftest.py` 在导入应用前把默认工作目录指到临时目录，测试不会污染仓库。

生产镜像/真实引擎才需要：

```powershell
python -m pip install -r requirements.txt   # 含 lightrag-hku==1.5.7
```

## 7. 本地运行与 Docker

```powershell
cd python-workers/lightrag-service
python -m app.main          # http://127.0.0.1:8093
```

本机 loopback 开发可以不配 token；容器内绑定 `0.0.0.0`，必须给 token：

```powershell
docker build -t praxis-rag-lightrag-service python-workers/lightrag-service
docker run --rm -p 8093:8093 `
  -e RAG_LIGHTRAG_SERVICE_TOKEN=change-me `
  -v lightrag-data:/data `
  praxis-rag-lightrag-service
```

进程固定单 worker：每个 workspace 是文件型 LightRAG 存储，多进程不会共享；
并发由 `RAG_LIGHTRAG_SERVICE_CONCURRENCY`（外层信号量）和 LightRAG 内部异步队列共同限制。

Go 端通过 `RAG_LIGHTRAG_SERVICE_URL`（例如 `http://127.0.0.1:8093`）与
`RAG_LIGHTRAG_SERVICE_TOKEN` 访问本服务；本目录不修改任何 Go 代码。

注意两个同名但独立的超时：Go 侧的 `RAG_LIGHTRAG_SERVICE_TIMEOUT`（默认 60 秒）是 HTTP
客户端超时，本服务的 `RAG_LIGHTRAG_SERVICE_TIMEOUT`（默认 900 秒）是服务端单请求预算。
大文档灌入时 Go 侧客户端超时必须一起调大，否则请求会在侧车完成前被客户端取消。

## 8. 调用 LightRAG SDK 的假设（升级核对清单）

以下是 `app/lightrag_engine.py` 依赖的上游行为，均已对着 `lightrag-hku==1.5.7` 源码核对过。
升级时必须逐项复核（括号内是 1.5.7 的实际位置）：

1. **构造参数**：`LightRAG(working_dir=, workspace=, llm_model_func=, role_llm_configs=,
   embedding_func=, llm_model_name=, default_llm_timeout=, addon_params=,
   llm_model_max_async=, embedding_func_max_async=)`（`lightrag/lightrag.py` 的 dataclass 字段）。
   1.4.x 及更早没有 `role_llm_configs`，需要改用单独的 LLM 函数或升级策略。
2. **初始化**：`await rag.initialize_storages()` 会顺带初始化 `pipeline_status`；
   1.5.7 不需要再手动调用 `initialize_pipeline_status()`（`initialize_storages` 内部已做）。
3. **预切分灌入**：`await rag.ainsert_custom_chunks(full_text=..., text_chunks=[...], doc_id=...)`；
   文本块会被 `sanitize_text_for_encoding` 清洗后再计算 id。
4. **块 id 派生**：`lightrag.utils_pipeline.make_custom_chunk_id(doc_id, text)` =
   `"chunk-" + md5(f"{len(doc_id)}:{doc_id}:{text}")`。侧车优先导入该 helper，导入失败才回退到
   `app.engine.custom_chunk_key`（同公式、未清洗文本）。**若上游改动派生方式，块映射会全部失效**，
   次级兜底是按返回文本的 md5 匹配登记表。
5. **结构化查询**：`await rag.aquery_data(query, param=QueryParam(mode=..., only_need_context=True,
   top_k=..., chunk_top_k=...))` 返回
   `{"status":"success|failure","data":{"entities":[...],"relationships":[...],"chunks":[...],
   "references":[...]},"metadata":{...}}`；`chunks` 含 `chunk_id`/`content`/`file_path`/
   `reference_id`，`entities` 用 `entity_name`，`relationships` 用 `src_id`/`tgt_id`/`description`，
   `source_id` 是 `<SEP>` 连接的块 id（`lightrag.constants.GRAPH_FIELD_SEP`）。
6. **不返回逐块分数**：1.5.7 的 `chunks` 没有 `score`/`distance`；侧车用排名分兜底。
   若上游未来加了 `score`/`similarity`/`relevance`/`distance` 字段，适配器会自动采用。
7. **删除**：`await rag.adelete_by_doc_id(doc_id)` 返回带 `status` 的对象（`success` /
   `not_found` / `not_allowed` / `fail`）；`not_found` 映射为 `deleted=false`，其它非 success 映射 503。
8. **收尾与清空**：`await rag.finalize_storages()` 存在；上游**没有** drop/clear workspace 的公开 API，
   所以清空 = finalize + 删除本服务独占的 workspace 目录（`<WORKING_DIR>/workspaces/<workspace>/lightrag`）。
9. **每文档计数**：`rag.full_entities.get_by_id(doc_id)` 返回 `{"entity_names": [...], "count": n}`，
   `full_relations` 返回 `{"relation_pairs": [...], "count": n}`；custom chunks 插入本身不返回计数。
10. **LLM 调用约定**：函数签名为
    `func(prompt, system_prompt=None, history_messages=None, **kwargs)`，可能收到
    `response_format` / `keyword_extraction` / `stream` / `_priority` 等参数；**必须返回 str**。
    角色名 `extract` / `keyword` 由 `role_llm_configs` 绑定。
11. **Embedding 约定**：`EmbeddingFunc(embedding_dim=..., func=...)`，`func` 是异步函数且返回
    `numpy.ndarray`，可能收到 `context` / `max_token_size` / `embedding_dim` 参数；
    侧车忽略这些提示，仅校验批大小与维度。
12. **查询校验**：1.5.7 要求查询文本达到 3 个"英文等权字符"（CJK 字符算 2），
    过短会抛 `lightrag.query_validation` 的异常；侧车在 HTTP 层先做同样校验，避免把 500 暴露给 Go。
13. **多 workspace 单进程**：每个 workspace 使用独立 `working_dir` 与 workspace 名；
    首次 `initialize_storages` 会设置进程级默认 workspace（上游仅打日志），并发使用需验证。
14. **Python 版本**：`lightrag-hku` 1.5.7 要求 Python >= 3.10；镜像用 3.11。

## 9. 已知限制

- **替换代价**：重复灌入同一文档是"删图 + 重新抽取"，大文档会触发实体/关系重建，耗时与 LLM 调用量上升；
  Go 端应避免无版本变化的重复灌入。
- **计数是尽力而为**：`entities`/`relations` 取 LightRAG 的 per-doc 行；行缺失或存储异常时为 0，
  不影响写入成功与查询。
- **score 语义**：见第 3 节；跨服务不可比。若 Go 端需要真实相似度，需要上游 `aquery_data` 暴露分数或
  改用向量库直查（超出本侧车范围）。
- **同文档重复文本块**：LightRAG 的 custom chunk id 只由 `(doc_id, text)` 决定；同一文档内文本完全相同的两块
  会共享一行，登记表只保留其中一个 Praxis `chunk_id`。Praxis 切分通常不会产生完全相同的块，接入时需确认。
- **单请求灌入**：一个文档必须一次请求发完，不能分批（替换语义会视为新版本）。
- **重启后需重新绑定**：`POST /v1/workspaces` 是每个 namespace 在当前进程的绑定入口；
  直接查询/灌入未绑定的 namespace 返回 404。
- **不鉴权 `/healthz`**：与仓库其他服务一致，健康检查不要求 token；它不返回任何数据内容。
