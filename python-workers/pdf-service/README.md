# Praxis-RAG PDF 解析微服务（远端视觉模型）

把 PDF 的版面、OCR、表格、公式与图片描述统一交给**远端 OpenAI 兼容视觉模型**，本服务只负责
**PDF 读取 / 渲染 / 裁图 / 结果组装**。Go 端（`internal/document`）通过 HTTP 调用本服务，拿回一个
zip：根目录的 `manifest.json` + 清单引用的图片，然后交给现有的 `document.BuildPDFContent` / 入库流程。

- **本地不加载任何模型权重**：没有 docling、torch、easyocr、tesseract、rapidocr、CodeFormula，
  也没有 `/models` 模型卷；进程内存里只有 PyMuPDF 与 HTTP 客户端。
- **一个视觉模型承担全部 PDF 视觉能力**：每页一次主调用返回该页的文本/OCR、公式 LaTeX、
  表格结构与图片区域；没有本地图注的图片再裁图调用一次生成描述。
- 模型配置（服务地址、模型名、API Key）由 Go 端按知识库选择后随 `options` 下发；服务端
  `RAG_VLM_*` 只是进程级回退。
- **清单版本固定 `docling-service-v1`**：这是与 Go 端共享的**证据格式契约**（与实现无关，
  保留旧名字是为了历史数据兼容），不是“仍在使用 Docling”。

```
python-workers/pdf-service/
├── app/
│   ├── __init__.py        # 服务名与版本
│   ├── main.py            # FastAPI：/healthz、/v1/parse、并发/超时/错误映射
│   ├── vision.py          # OpenAI 兼容视觉客户端（chat/completions、JSON 解析、并发/重试）
│   ├── pdfreader.py       # PyMuPDF 读取/渲染/裁图
│   ├── parser.py          # VisionConverter（组装清单）、fake converter、清单契约校验
│   └── errors.py          # 安全错误类型与中文消息
├── tests/                 # pytest（除视觉调用外全部可离线跑；视觉客户端可注入 fake）
├── Dockerfile
├── requirements.txt       # pymupdf / httpx / fastapi / uvicorn / python-multipart
├── requirements-dev.txt   # 测试依赖（pytest + 运行时依赖）
├── pytest.ini
└── README.md
```

## 1. HTTP 接口

### `GET /healthz`

始终返回 200（便于容器健康检查与排障）。示例：

```json
{
  "status": "ok",
  "service": "praxis-rag-pdf-service",
  "version": "2.0.0",
  "parser_version": "docling-service-v1",
  "converter": "vision",
  "converter_requested": "vision",
  "vision_configured": true,
  "vision_model": "qwen2.5-vl-72b",
  "max_upload_bytes": 104857600,
  "timeout_seconds": 900.0,
  "concurrency": 1,
  "config_error": null
}
```

- `converter`：实际使用的转换器（`vision` / `fake` / `unavailable`）。
- `converter_requested`：请求的转换器（`docling` 旧值会归一化为 `vision`）。
- `vision_configured` / `vision_model`：服务端是否配置了 `RAG_VLM_*` 回退端点（请求级
  `options.vision` 不体现在健康检查中）；未配置时 `vision_model` 为 `null`。
- `status`：`degraded` 表示配置有误（如 `RAG_PDF_SERVICE_CONVERTER` 非法、`RAG_VLM_*` 只配一半），
  `config_error` 给出原因。配置有问题时服务照常启动，健康检查仍 200，但 `/v1/parse` 会返回 503。

`GET /` 返回同样的服务信息（与 `/healthz` 相同，含 `status`）。

### `POST /v1/parse`

- 请求：`multipart/form-data`，字段名 **`file`**，内容为一个 PDF（服务端检查前 1024 字节含 `%PDF-`）；
  可选文本字段 **`options`**（JSON），用于**按请求**提供视觉模型配置（见下一节）。
- 成功：`200`，`Content-Type: application/zip`，响应头
  `X-PDF-Parser-Version: docling-service-v1`、`X-PDF-Converter: vision|fake`。
- 失败：JSON `{"detail": "<安全中文消息>"}`；`detail` 不会包含文档正文、请求里的 API Key 或上游响应正文。

zip 结构：

```
manifest.json
pictures/picture-0.png
pictures/picture-1.png
...
```

`manifest.json` 里 `image_file` 引用的每个文件都在 zip 内，路径为相对路径（`pictures/picture-N.png`）；
**table 资产不带图**，`image_file` 为 `null`，只携带表格结构。

状态码：

| 状态码 | 含义 |
| --- | --- |
| 200 | 解析成功，返回 zip |
| 400 | 缺少文件内容 / 不是 PDF（`%PDF-` 头校验失败）|
| 413 | 文件超过 `RAG_PDF_SERVICE_MAX_BYTES`（默认 100 MiB）|
| 422 | multipart 字段缺失/不是 `file`；或 `options` 非法（不是 JSON 对象、未知字段、类型错误、`ocr` 键已废除、`vision`/`vlm` 同时出现）|
| 500 | 转换失败（视觉调用失败、公式为空、表格结构识别失败等）|
| 503 | 未配置视觉模型，或配置错误（converter 非法等）|
| 504 | 解析超时（`RAG_PDF_SERVICE_TIMEOUT`）|

### `options`：每请求视觉模型覆盖

`options` 是可选文本字段，内容为 JSON 对象。**不传 / 空字符串 / `{}` / `null` 时完全使用进程环境变量
`RAG_VLM_*`**。

```json
{
  "vision": {
    "base_url": "https://vlm.internal/v1",
    "api_key": "sk-...",
    "model": "qwen2.5-vl-72b",
    "timeout": 120,
    "concurrency": 4
  }
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `vision.base_url` | string | OpenAI 兼容视觉接口的 base URL（如 `https://vlm.internal/v1`）|
| `vision.api_key` | string | 可选；只放进请求头 `Authorization: Bearer ...`，不写日志、不写 manifest |
| `vision.model` | string | 视觉模型名 |
| `vision.timeout` | number | 可选，默认 `120` 秒；下限 `30` |
| `vision.concurrency` | integer | 可选，默认 `4`；下限 `1` |

规则：

- 键名规范为 **`vision`**；同时接受旧键 **`vlm`**（同一对象，作为兼容别名）。两者同时出现 → `422`。
- **`ocr` 键已废除**：出现即 `422`，提示
  `options.ocr 已废除：OCR 已并入视觉模型，请使用 options.vision`。
- `vision` 为空对象 `{}` 视为“无覆盖”，走环境回退。
- `base_url` 与 `model` 必须同时非空；`api_key` 可为空；未知字段、类型错误 → `422`，
  消息不回显任何提交值（尤其不回显 API Key）。
- **请求优先于环境变量**：带了完整 `options.vision` 的请求即使 `RAG_VLM_*` 只配置了一半也能解析；
  不带 `options` 的请求按环境变量配置，缺 `RAG_VLM_BASE_URL`/`RAG_VLM_MODEL` 时返回 503。
- 未配置视觉模型（请求与环境变量都没有）→ `503`：
  `PDF 解析服务未配置视觉模型：请设置 RAG_VLM_BASE_URL 与 RAG_VLM_MODEL，或在请求 options.vision 中提供`。

示例：

```bash
curl -sS -F file=@sample.pdf \
  -F 'options={"vision":{"base_url":"https://vlm.internal/v1","api_key":"sk-...","model":"qwen2.5-vl-72b","timeout":180,"concurrency":2}}' \
  http://127.0.0.1:8091/v1/parse -o out.zip
```

`RAG_PDF_SERVICE_CONVERTER=fake` 时 `options` 会被接受并忽略（fake 清单固定），便于 Go 端联调链路。

## 2. 视觉调用约定

- URL：`{base_url}/chat/completions`；`base_url` 以 `/chat/completions` 结尾时直接使用，以 `/v1` 结尾时补
  `/chat/completions`，否则补 `/v1/chat/completions`。
- 请求：`POST`，头 `Authorization: Bearer <api_key>`（key 非空时）、`Content-Type: application/json`；
  body 为 `{"model": ..., "messages": [...], "temperature": 0, "max_tokens": ...}`，user 消息内容为
  `[{"type":"text","text":<提示词>},{"type":"image_url","image_url":{"url":"data:image/jpeg;base64,..."}}]`。
- 响应：OpenAI chat completions；取 `choices[0].message.content`（兼容字符串或
  `[{"type":"text","text":...}]` 数组）。
- 提示词要求模型**只输出 JSON**；解析时容忍 ```json 代码围栏、前后空白与首尾多余文本
  （取第一个平衡的 `{...}`）。
- **每页一次**主调用返回该页所有块（文本/OCR、公式 LaTeX、表格结构、图片区域）；**每张图片一次**
  裁图描述调用（仅当没有本地图注时）。
- 表格若主调用未给出结构：对该表格 bbox 裁图再发一次“表格结构”调用；仍失败 → `500`
  `PDF 解析失败：第 {page} 页表格结构识别失败`。
- 并发：页调用按 `concurrency` 并发，失败请求按“每页最多 2 次尝试”重试一次，仍失败 → 整篇失败：
  `PDF 解析失败：第 {page} 页视觉模型调用失败，请检查服务地址、模型和 API Key`。
- 超时 / 网络错误 → `500`（中文、含页码、不含响应体）；公式 LaTeX 为空 →
  `PDF 解析失败：第 {page} 页公式识别结果为空`。
- 不把 PDF 全文或密钥写日志；debug 级最多记录页码/耗时/模型名。

## 3. 清单契约（与 Go 端 `internal/document/pdf.go` 严格对齐）

顶层：

```json
{
  "version": "docling-service-v1",
  "blocks": [],
  "assets": [],
  "warnings": [],
  "parser": {},
  "metrics": {}
}
```

`blocks[]`（每一项的所有键都会输出，Go 结构体没有 `omitempty`）：

| 字段 | 说明 |
| --- | --- |
| `ref` | 引用，如 `#/texts/3`、`#/pictures/0` |
| `kind` | `text` / `title` / `section_header` / `list_item` / `caption` / `footnote` / `formula` / `picture` / `table` / `code` |
| `page` | 1 起始页码 |
| `order` | 块顺序（0 起连续）|
| `text` | chunk 正文；picture/table 块为空串（由 asset 渲染）|
| `asset_id` | 仅 picture/table 块非空，指向 `assets[].asset_id` |
| `bbox` | `{l,t,r,b,coord_origin}`，PDF 点坐标、原点左上（`TOPLEFT`）|
| `locations` | `[{page,bbox,charspan}]` |
| `latex` | 仅公式块：纯 LaTeX |
| `original_text` | 仅公式块：视觉模型返回的原始文本 |
| `equation_number` | 公式编号字符串或 `null` |
| `association` | 仅 caption/footnote：`document` / `unlinked` / `""` |
| `associated_assets` | caption/footnote 关联的 asset_id 列表 |

公式块规则（Go 下游把 `text` 当 chunk 正文、`latex` 放 metadata）：

```json
{
  "kind": "formula",
  "latex": "G = X_{basis}^{T} X_{basis}",
  "text": "$$\nG = X_{basis}^{T} X_{basis}\n$$\n\n(1)",
  "original_text": "G = X_basis^T X_basis (1)",
  "equation_number": "1"
}
```

- 有编号时 `text` 末尾追加 `\n\n(编号)`（若 LaTeX 本身已含该编号则不重复）；
- **`latex` 与 `text` 都不允许为空**：公式识别失败会让整篇文档失败（500），不会静默索引空公式。

`assets[]`：

| 字段 | 说明 |
| --- | --- |
| `asset_id` / `kind` / `page` / `bbox` | 身份与位置，`kind` 为 `picture` 或 `table` |
| `caption` / `caption_source` / `generated_description` | 图注（`document`/`none`）与视觉模型生成的描述 |
| `image_file` | picture：`pictures/picture-N.png`；table：`null` |
| `table` | table 结构：`num_rows`/`num_cols`/`orientation`/`headers`/`header_rows`/`rows`/`cells`/`footnotes` |
| `captions` / `footnotes` | 注解列表：`{ref,text,page,bbox,locations}` |

`warnings[]`：`{page, kind, count, message}`；空页面可用 `kind="empty_page"`。

`parser`：自由对象，建议含 `version` / `service_version` / `service` / `converter="vision"` /
`backend="openai-compatible"` / `model` / `endpoint`（只含 host）/ `prompt_version` / `capabilities`；
**不得**包含 API Key。

`metrics`：自由对象，建议 `pages`、`vision_requests`、`vision_failures`、`elapsed_seconds`。

服务端在返回前用 `app.parser.validate_manifest` 严格校验以上结构（键集合、类型、公式非空、图片文件存在、
table 的 `image_file` 为 null、`image_file` 是安全相对路径等），避免契约漂移在 Go 端变成空字段。

## 4. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_PDF_SERVICE_HOST` | `0.0.0.0` | `python -m app.main` 的监听地址（uvicorn CLI 以命令行参数为准）|
| `PORT` | `8091` | 服务端口 |
| `RAG_PDF_SERVICE_TIMEOUT` | `900` | 单请求超时（秒）：排队等待与转换各自不超过该值，超时返回 504 |
| `RAG_PDF_SERVICE_WORKERS` | `1` | Uvicorn 服务进程数（正整数）；各进程共享监听端口 |
| `RAG_PDF_SERVICE_CONCURRENCY` | `1` | 进程内并发转换上限（信号量）|
| `RAG_PDF_SERVICE_MAX_BYTES` | `104857600` | 上传大小上限（字节），超限 413 |
| `RAG_PDF_SERVICE_CONVERTER` | `vision` | `vision` 远端视觉解析；旧值 `docling` 作为兼容别名视为 `vision`；`fake` 固定清单（仅测试/联调）；其它值 → 配置错误 |
| `RAG_VLM_BASE_URL` | 空 | OpenAI 兼容视觉接口的 base URL（如 `https://vlm.internal/v1`）|
| `RAG_VLM_API_KEY` | 空 | 视觉模型 API Key（只用于鉴权，不落盘、不进清单）|
| `RAG_VLM_MODEL` | 空 | 视觉模型名 |
| `RAG_VLM_TIMEOUT` | `120` | 视觉请求超时（秒，最低 30）|
| `RAG_VLM_CONCURRENCY` | `4` | 单篇文档的页级视觉并发 |
| `RAG_VLM_MAX_TOKENS` | `4096` | 单次视觉输出上限（可选）|
| `RAG_PDF_PAGE_DPI` | `200` | 页面渲染 DPI（可选，范围 72–300）|
| `RAG_VLM_PROMPT_VERSION` | `vision-v1` | 提示词版本（可选，只用于 `manifest.parser` 记录）|

模型选择规则：

- `options.vision`（或别名 `options.vlm`）优先；未提供时用 `RAG_VLM_*`。
- `RAG_VLM_BASE_URL` + `RAG_VLM_MODEL` **必须同时配置**，只配一半 → `config_error` 指明缺少项，
  请求返回 503（带完整 `options.vision` 的请求不受影响）。
- `RAG_PDF_MODELS_PATH` **已废弃/删除**：不再有本地模型，出现也不会读取。

## 5. Docker

```bash
# 构建（context 必须是 python-workers/pdf-service）
docker build -t praxis-rag-pdf-service python-workers/pdf-service

# 运行：视觉模型由请求 options.vision 提供，或在此配置服务级回退
docker run --rm -p 8091:8091 \
  -e RAG_PDF_SERVICE_TIMEOUT=900 \
  -e RAG_PDF_SERVICE_CONCURRENCY=1 \
  -e RAG_VLM_BASE_URL=https://vlm.internal/v1 \
  -e RAG_VLM_API_KEY=... \
  -e RAG_VLM_MODEL=qwen2.5-vl-72b \
  praxis-rag-pdf-service
```

镜像要点：`python:3.11-slim`；不安装 torch/CUDA/GL 系统库，不挂载 `/models`；非 root（uid 10001）运行；
`HEALTHCHECK` 用 Python 标准库 `urllib` 调 `http://127.0.0.1:$PORT/healthz`（镜像内没有 curl）；
默认入口为 `python -m app.main`，通过 `RAG_PDF_SERVICE_WORKERS` 配置 Uvicorn 服务进程数，默认 1。
各进程共享监听 socket，分别持有 `RAG_PDF_SERVICE_CONCURRENCY` 个解析名额；总解析容量上限为两者乘积，
实际任务分布受连接复用和任务耗时影响。也可通过多个容器副本扩容，此时需另行配置负载分配。

构建参数只保留 `BASE_IMAGE` / `APT_MIRROR` / `PIP_INDEX_URL` / `PIP_RETRIES` / `PIP_TIMEOUT`。

## 6. 本地开发与测试

```powershell
$env:PYTHONIOENCODING='utf-8'
cd python-workers/pdf-service

# 测试依赖（pytest + 运行时依赖；不需要任何模型）
python -m pip install -r requirements-dev.txt

# 单元/契约测试：healthz、/v1/parse zip/manifest 契约、错误码、清单校验、
# options（vision/vlm 别名、ocr 422、422 安全消息）、视觉请求体与超时/错误映射
python -m pytest -q
```

视觉客户端必须支持注入 `httpx.MockTransport` 或等价 fake，**测试不得访问网络**。

fake 模式（无需视觉模型，Go 端 live 测试/联调用）：

```powershell
$env:RAG_PDF_SERVICE_CONVERTER='fake'
$env:RAG_PDF_SERVICE_HOST='127.0.0.1'
$env:RAG_PDF_SERVICE_WORKERS='2'
$env:RAG_PDF_SERVICE_CONCURRENCY='2'
python -m app.main
```

直接使用 `python -m uvicorn app.main:app` 时，应通过 `--workers 2` 指定服务进程数；Uvicorn CLI 不读取
项目自定义的 `RAG_PDF_SERVICE_WORKERS`。每进程解析并发仍由应用读取 `RAG_PDF_SERVICE_CONCURRENCY`。

fake 清单固定为：1 个 `text` 块、1 个 `formula` 块（latex `G = X_{basis}^{T} X_{basis}`）、
1 个 `picture` 资产（真实 8x8 PNG，`pictures/picture-0.png`），版本 `docling-service-v1`。

冒烟：

```bash
curl -s http://127.0.0.1:8091/healthz
curl -sS -F file=@sample.pdf http://127.0.0.1:8091/v1/parse -o out.zip
python - <<'PY'
import json, zipfile
with zipfile.ZipFile("out.zip") as z:
    print(z.namelist())
    m = json.loads(z.read("manifest.json"))
    print(m["version"], [b["kind"] for b in m["blocks"]])
    print([b.get("latex") for b in m["blocks"] if b["kind"] == "formula"])
PY
```

## 7. 与 Go 后端联调

Go 端（`internal/document/pdf_service.go`）：

1. 配置服务地址 `RAG_PDF_SERVICE_URL`（例如 `http://127.0.0.1:8091`，compose 内为 `http://pdf-parser:8091`）；
2. 对每个 PDF 发 `POST {RAG_PDF_SERVICE_URL}/v1/parse`，multipart 字段 `file` + 文本字段 `options`，
   把响应体解压到解析 staging 目录（保持 `manifest.json` 与 `pictures/` 的相对路径）；
3. 读 `manifest.json` 反序列化为 `document.PDFManifest`，走现有 `document.BuildPDFContent(...)` /
   资产入库；
4. 非 200 时读 `{"detail"}` 作为用户可见错误；HTTP 客户端超时建议 ≥ `RAG_PDF_SERVICE_TIMEOUT`。

`options` 只发 `vision`：

```json
{"vision":{"base_url":"https://vlm.internal/v1","api_key":"sk-...","model":"qwen2.5-vl-72b","timeout":120,"concurrency":4}}
```

未配置视觉模型（`VLM.Configured()==false`）时不发 `options` 字段。API Key 不进日志/错误。

联调顺序建议：先以 `RAG_PDF_SERVICE_CONVERTER=fake` 打通 Go→HTTP→zip→staging→manifest 链路，
再用真实视觉端点做文档验收。

## 8. 迁移说明 / 已知限制

- **从 Docling 实现迁移**：旧版的本地 docling/torch/easyocr/CodeFormula 及 `/models` 模型卷已全部移除；
  `RAG_PDF_SERVICE_CONVERTER` 的旧值 `docling` 仍被接受并归一化为 `vision`（不改变行为，只做兼容）。
  `RAG_PDF_MODELS_PATH` 已废弃，不再读取。
- **清单版本 `docling-service-v1` 保持不变**：它标识的是证据格式契约（顶层键、块/资产结构、公式
  `text` 中的 `$$` 等），与生产实现无关；Go 端 `IsSupportedPDFParserVersion` 继续接受
  `go-native-v1`、`docling-service-v1`、`docling-formula-remote-v3`。
- **zip 只含 manifest.json + 图片**：不返回 OCR 明细或 document.json；诊断只保留为 `warnings[]`。
- **超时不能中断上游视觉请求**：504 只是快速把错误返回给调用方，后台线程可能跑完并占用并发槽位；
  遇到重复超时请提高 `RAG_PDF_SERVICE_TIMEOUT` 或扩容副本。
- **质量取决于远端视觉模型**：版面、OCR、表格、公式、图片描述都由同一模型完成；模型能力不足时
  可能出现块顺序/表格结构不准确，但契约仍会强制：空公式/缺失表格结构会让整篇文档失败，
  不会静默写入错误证据。
- **并发与限流**：页级并发受 `options.vision.concurrency`（或 `RAG_VLM_CONCURRENCY`）与进程级
  `RAG_PDF_SERVICE_CONCURRENCY` 两层约束；远端服务自身的限流需要部署侧配合。
- **未验证项**：真实视觉端点、真实 PDF 端到端与 Docker 构建未在本次迁移中验证（见根目录 `VALIDATION.md`）。
