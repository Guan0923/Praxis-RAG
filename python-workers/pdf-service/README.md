# Praxis-RAG PDF 解析微服务（Docling）

把 PDF 解析从 Go 进程内抽取改回 Python Docling 的独立 HTTP 微服务。Go 端（`internal/document`）通过 HTTP
调用本服务，拿回一个 zip：根目录的 `manifest.json` + 清单引用的图片，然后交给现有的
`document.BuildPDFContent` / 入库流程。

本服务是提交 `f778504` 删除的旧 worker（`python-workers/rag_mcp/pdf_worker.py`）的服务端移植：

- 文档转换逻辑（Docling 2.126.0、CodeFormulaV2 公式、EasyOCR、TableFormer）与原 worker 一致；
- 去掉了 argparse/CLI，改为常驻服务，支持并发上限与单请求超时；
- 清单版本从 `docling-formula-remote-v3` 改为 **`docling-service-v1`**；
- Docling/torch **惰性导入**：模块 import、pytest、`RAG_PDF_SERVICE_CONVERTER=fake` 都不需要安装它们。

```
python-workers/pdf-service/
├── app/
│   ├── __init__.py        # 服务名与版本
│   ├── main.py            # FastAPI：/healthz、/v1/parse、并发/超时/错误映射
│   └── parser.py          # Docling 转换、清单导出、fake converter、清单契约校验
├── tests/                 # pytest（不需要 docling/torch）
├── Dockerfile
├── requirements.txt       # 运行时（docling==2.126.0 等）
├── requirements-dev.txt   # 测试/fake 模式（无 docling/torch）
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
  "version": "1.0.0",
  "parser_version": "docling-service-v1",
  "converter": "docling",
  "converter_requested": "docling",
  "docling_installed": true,
  "models_configured": true,
  "vlm_configured": false,
  "max_upload_bytes": 104857600,
  "timeout_seconds": 900.0,
  "concurrency": 1,
  "config_error": null
}
```

- `converter`：实际使用的转换器（`docling` / `fake` / `unavailable`）。
- `status`：`degraded` 表示配置有误（如 `RAG_PDF_SERVICE_CONVERTER` 非法），`config_error` 给出原因。
- 配置有问题时服务照常启动，健康检查仍 200，但 `/v1/parse` 会返回 503 并给出可读原因。

`GET /` 返回同样的服务信息（不含 `status`/`docling_installed`）。

### `POST /v1/parse`

- 请求：`multipart/form-data`，字段名 **`file`**，内容为一个 PDF（服务端检查前 1024 字节含 `%PDF-`）；
  可选文本字段 **`options`**（JSON），用于**按请求**覆盖 VLM 与 OCR 引擎配置（见下一节）。
- 成功：`200`，`Content-Type: application/zip`，响应头
  `X-PDF-Parser-Version: docling-service-v1`、`X-PDF-Converter: docling|fake`。
- 失败：JSON `{"detail": "<安全消息>"}`，`detail` 不会包含文档正文；`options` 中的 API Key 不会被回显。

zip 结构：

```
manifest.json
pictures/picture-0.png
pictures/picture-1.png
...
```

`manifest.json` 里 `image_file` 引用的每个文件都在 zip 内，路径为相对路径（`pictures/picture-N.png`）；
**table 资产不带图**，`image_file` 为 `null`，只携带表格结构。图片之外的中间产物（表格裁剪图、OCR 明细）
不会进入 zip。

状态码：

| 状态码 | 含义 |
| --- | --- |
| 200 | 解析成功，返回 zip |
| 400 | 缺少文件内容 / 不是 PDF（`%PDF-` 头校验失败）|
| 413 | 文件超过 `RAG_PDF_SERVICE_MAX_BYTES`（默认 100 MiB）|
| 422 | multipart 字段缺失/不是 `file`（FastAPI 校验），或 `options` 不是合法 JSON / 字段类型错误 / OCR 引擎不在白名单 |
| 500 | 转换失败（Docling 报错、公式为空、表格结构不可靠等）|
| 503 | 配置错误（converter/模型/VLM 未配置好）或队列等待超时 |
| 504 | 解析超时（`RAG_PDF_SERVICE_TIMEOUT`）|

### `options`：每请求 VLM / OCR 覆盖

`options` 是可选文本字段，内容为 JSON 对象。**不传 / 空字符串 / `{}` / `null` 时完全保持原有行为**
（使用进程环境变量 `RAG_VLM_*` + 默认 EasyOCR），保证向后兼容。

```json
{
  "vlm": {
    "base_url": "http://vlm-host:8000/v1",
    "api_key": "sk-...",
    "model": "qwen2.5-vl-72b",
    "timeout": 120,
    "concurrency": 4
  },
  "ocr": {"engine": "easyocr"}
}
```

| 字段 | 类型 | 说明 |
| --- | --- | --- |
| `vlm.base_url` | string | OpenAI 兼容 VLM 的 base URL，可带自定义端口（如 `https://vlm.internal:8443/v1`）|
| `vlm.api_key` | string | 可选；只放进请求头 `Authorization: Bearer ...`，不写日志、不写 manifest |
| `vlm.model` | string | VLM 模型名 |
| `vlm.timeout` | number | 可选，默认 `120` 秒；下限 `30`（与 `RAG_VLM_TIMEOUT` 一致）|
| `vlm.concurrency` | integer | 可选，默认 `4`；下限 `1` |
| `ocr.engine` | string | 可选，默认 `easyocr`；可选值 `auto` / `easyocr` / `tesseract` / `rapidocr` |

规则：

- **`options` 优先于环境变量**：给了 `options.vlm` 就用它（整篇文档），否则用 `RAG_VLM_*`；
  因此部署侧 `RAG_VLM_*` 只配置了一半（缺 `RAG_VLM_MODEL`）时，**带上完整 `options.vlm` 的请求仍可正常解析**，
  不带 `options.vlm` 的请求仍按原行为返回 503。
- `options.vlm` 必须同时给出非空的 `base_url` 与 `model`；`api_key` 可省略（不带鉴权头）。
  空的 `{"vlm": {}}` 视为“不覆盖”，回落到环境变量。
- OCR 引擎映射（Docling 2.126.0）：
  - `easyocr`（默认）：`EasyOcrOptions(lang=["ch_sim","en"], download_enabled=False, confidence_threshold=0.0)`，
    有 `RAG_PDF_MODELS_PATH` 时模型目录为 `<models>/EasyOcr`；
  - `auto`：与 `easyocr` 相同（契约固定为 EasyOCR，避免 Docling 自动选择到未安装的引擎）；
  - `tesseract`：`TesseractOcrOptions(lang=["chi_sim","eng"])`；
  - `rapidocr`：`RapidOcrOptions()`。
  `TesseractOcrOptions`/`RapidOcrOptions` 在 Docling 惰性导入时才导入，不影响无 docling 的测试环境。
- 实际使用的 OCR 引擎写入 `manifest.parser.ocr_engine`（`auto` 记录为 `easyocr`）；VLM 只记录 host+model。
- 非法 `options` 返回 `422`，`detail` 为安全中文消息（指明字段与可选值，**不回显提交值，尤其不回显 API key**）。

示例（自定义端口 VLM + Tesseract）：

```bash
curl -sS -F file=@sample.pdf \
  -F 'options={"vlm":{"base_url":"https://vlm.internal:8443/v1","api_key":"sk-...","model":"qwen2.5-vl-72b","timeout":180,"concurrency":2},"ocr":{"engine":"tesseract"}}' \
  http://127.0.0.1:8091/v1/parse -o out.zip
```

示例（仅切换 OCR 引擎，VLM 继续用 `RAG_VLM_*`）：

```bash
curl -sS -F file=@sample.pdf -F 'options={"ocr":{"engine":"rapidocr"}}' \
  http://127.0.0.1:8091/v1/parse -o out.zip
```

`RAG_PDF_SERVICE_CONVERTER=fake` 时 `options` 会被接受并忽略（fake 清单固定），便于 Go 端联调链路。

## 2. 清单契约（与 Go 端 `internal/document/pdf.go` 严格对齐）

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
| `ref` | Docling 引用，如 `#/texts/3`、`#/pictures/0` |
| `kind` | `text` / `section_header` / `list_item` / `caption` / `footnote` / `formula` / `picture` / `table` …（Docling label）|
| `page` | 1 起始页码 |
| `order` | 块顺序（0 起连续）|
| `text` | chunk 正文；picture/table 块为空串（由 asset 渲染）|
| `asset_id` | 仅 picture/table 块非空，指向 `assets[].asset_id` |
| `bbox` | `{l,t,r,b,coord_origin}` |
| `locations` | `[{page,bbox,charspan}]` |
| `latex` | 仅公式块：纯 LaTeX |
| `original_text` | 仅公式块：Docling 原始抽取文本 |
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
- **`text` 与 `latex` 都不允许为空**：公式识别失败会让整篇文档失败（500），不会静默索引空公式。

`assets[]`：

| 字段 | 说明 |
| --- | --- |
| `asset_id` / `kind` / `page` / `bbox` | 身份与位置，`kind` 为 `picture` 或 `table` |
| `caption` / `caption_source` / `generated_description` | 图注（`document`/`none`）与 VLM 生成描述 |
| `image_file` | picture：`pictures/picture-N.png`；table：`null` |
| `table` | table 结构：`num_rows`/`num_cols`/`orientation`/`headers`/`header_rows`/`rows`/`cells`/`footnotes` |
| `captions` / `footnotes` | 注解列表：`{ref,text,page,bbox,locations}` |

`warnings[]`：`{page, kind, count, message}`，目前用于低置信度 OCR（`low_confidence_ocr`），只给告警、
不丢文本。

`parser`：`{version, service_version, service, converter, docling, formula_model, formula_revision, vlm,
formula_crop_scale, formula_crop_expansion, formula_batch_size, ocr_confidence_threshold, ocr_engine}`。

- `vlm`：实际使用的远端 VLM 描述 `{backend, model, endpoint}`（`endpoint` 只含 host:port，不含 API Key）；
  本地 CodeFormulaV2 模式为 `null`。
- `ocr_engine`：实际运行的 OCR 引擎（`auto` 记录为 `easyocr`）；`fake` 模式为 `null`。

`metrics`：`{seconds, pages, peak_cuda_allocated_bytes}`。

服务端在返回前用 `app.parser.validate_manifest` 严格校验以上结构（键集合、类型、公式非空、图片文件存在、
table 的 `image_file` 为 null、`image_file` 是安全相对路径等），避免契约漂移在 Go 端变成空字段。

## 3. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_PDF_SERVICE_HOST` | `0.0.0.0` | `python -m app.main` 的监听地址（uvicorn CLI 以命令行参数为准）|
| `PORT` | `8091` | 服务端口 |
| `RAG_PDF_SERVICE_TIMEOUT` | `900` | 单请求超时（秒）：排队等待与转换各自不超过该值，超时返回 504 |
| `RAG_PDF_SERVICE_WORKERS` | `1` | `python -m app.main` 启动的 Uvicorn 服务进程数（正整数）；容器默认入口同样读取此变量，各进程共用监听端口 |
| `RAG_PDF_SERVICE_CONCURRENCY` | `1` | 进程内并发转换上限（信号量）；模型权重常驻共享，转换工作内存与 CPU 随并发线性增长，内存紧张保持 1 靠副本扩容 |
| `RAG_PDF_SERVICE_CONVERTER` | `docling` | `docling` 真实解析；`fake` 固定清单（仅测试/联调）|
| `RAG_PDF_SERVICE_LOG_LEVEL` | `info` | 日志级别：`debug`/`info`/`warning`/`error`；未设置时回退共享调试开关 `RAG_DEBUG_TRACE` |
| `RAG_PDF_SERVICE_LOG_FORMAT` | `text` | `text`（默认，`时间 级别 名称 消息`）或 `json`（每行一个 JSON 对象）|
| `RAG_PDF_MODELS_PATH` | 空 | Docling 模型目录（`artifacts_path`），容器内为 `/models` |
| `RAG_PDF_SERVICE_MAX_BYTES` | `104857600` | 上传大小上限（字节），超限 413 |
| `RAG_VLM_BASE_URL` | 空 | OpenAI 兼容 VLM 的 base URL（如 `http://host:8000/v1`）|
| `RAG_VLM_API_KEY` | 空 | VLM API Key（只用于鉴权，不落盘、不进清单）|
| `RAG_VLM_MODEL` | 空 | VLM 模型名 |
| `RAG_VLM_TIMEOUT` | `120` | VLM 请求超时（秒，最低 30）|
| `RAG_VLM_CONCURRENCY` | `4` | VLM 并发（Docling API engine 的 concurrency）|

模型选择规则：

- **结构化日志与请求关联**：`python -m app.main` 安装统一 root handler（`RAG_PDF_SERVICE_LOG_LEVEL`/`RAG_PDF_SERVICE_LOG_FORMAT`，`log_config=None` 让 Uvicorn 复用该配置）；每个请求接受或生成 `X-Request-ID` 并在响应头回写，Go 后端上传时会把同一个 ID 透传过来。422 校验失败只记录字段位置、错误数量与错误类型，500 只记录异常类名，均不记录请求值、文档正文或凭据。

- `RAG_VLM_BASE_URL` + `RAG_VLM_MODEL` **同时配置** → 公式识别使用远端 VLM
  （`ApiVlmEngineOptions`，temperature 0、max_tokens 1024）；无 caption 的图片/表格也会调用该 VLM 生成
  `generated_description`（失败重试 3 次，429/5xx 重试）。
- 否则 → 使用本地 CodeFormulaV2 模型（`artifacts_path=RAG_PDF_MODELS_PATH`，revision 固定
  `ecedbe111d15c2dc60bfd4a823cbe80127b58af4`），图片描述跳过（保持空串）。
- **两者都没有** → 转换前给出可读配置错误（503）：“Formula recognition is not configured …”，
  不会静默输出空公式。
- 只配置一半（例如只有 `RAG_VLM_BASE_URL`）→ `config_error` 明确指出缺少 `RAG_VLM_MODEL`，
  不带 `options.vlm` 的 `/v1/parse` 请求返回 503（带完整 `options.vlm` 的请求不受影响，见第 1 节）。
- 以上环境变量是**进程级默认值**；`/v1/parse` 的 `options` 字段可按请求覆盖 VLM 与 OCR（优先于环境变量）。

## 4. 模型下载（本地 CodeFormula 模式）

容器/主机需要一份 Docling 模型目录（容器内挂到 `/models`）：

```bash
# 方式一：Docling 官方下载器（layout、TableFormer、EasyOCR 等默认模型）
docling-tools models download --output-dir /models

# 方式二：单独补 CodeFormulaV2 公式模型（默认下载命令可能不含它）
python - <<'PY'
from huggingface_hub import snapshot_download
snapshot_download(
    repo_id="docling-project/CodeFormulaV2",
    revision="ecedbe111d15c2dc60bfd4a823cbe80127b58af4",
    local_dir="/models/docling-project--CodeFormulaV2",
)
PY
```

目录约定（Docling 的 `artifacts_path` 命名）：

```
/models/
├── docling-project--CodeFormulaV2/   # 公式模型（本地模式必须有）
├── ds4sd--docling-layout-heron/      # layout（docling-tools 下载）
├── ds4sd--docling-models/            # TableFormer（docling-tools 下载）
└── EasyOcr/                          # OCR 模型（EasyOcrOptions.model_storage_directory）
```

服务在本地模式启动转换时会检查 `/models` 存在且包含名字带 `CodeFormula` 的目录，否则返回 503 提示
`docling-tools models download`。

## 5. Docker

```bash
# 构建（context 必须是 python-workers/pdf-service）
docker build -t praxis-rag-pdf-service python-workers/pdf-service

# 运行（本地 CodeFormula 模式）
docker run --rm -p 8091:8091 \
  -v /srv/praxis-rag/models:/models:ro \
  -e RAG_PDF_SERVICE_TIMEOUT=900 \
  -e RAG_PDF_SERVICE_CONCURRENCY=1 \
  praxis-rag-pdf-service

# 运行（远端 VLM 模式：公式识别走远端，但 layout/table/OCR 仍需本地权重或联网下载）
docker run --rm -p 8091:8091 \
  -v /srv/praxis-rag/models:/models:ro \
  -e RAG_VLM_BASE_URL=http://vlm-host:8000/v1 \
  -e RAG_VLM_API_KEY=... \
  -e RAG_VLM_MODEL=qwen2.5-vl-7b \
  praxis-rag-pdf-service
```

VLM 模式只是把公式识别（以及无 caption 图片描述）交给远端；Docling 的 layout/TableFormer/EasyOCR
仍然需要模型，建议同样挂载 `/models`。不挂载时 Docling 会尝试联网下载到默认缓存（需要容器能访问
Hugging Face）。

镜像要点：`python:3.11-slim`；`torch>=2.0` 从 `https://download.pytorch.org/whl/cpu` 装 CPU 轮子；
模型不打进镜像，挂载 `/models`；非 root（uid 10001）运行；`HEALTHCHECK` 用 Python 标准库
`urllib` 调 `http://127.0.0.1:$PORT/healthz`（镜像内没有 curl）。默认入口为 `python -m app.main`，通过 `RAG_PDF_SERVICE_WORKERS` 配置 Uvicorn 服务进程数，默认 1。各进程共享监听 socket，分别持有 `RAG_PDF_SERVICE_CONCURRENCY` 个解析名额；总解析容量上限为两者乘积，实际任务分布受连接复用和任务耗时影响。进程之间不共享模型内存；例如 `2 × 2` 和 `1 × 4` 都有最多 4 个解析名额，但前者多一个服务进程的模型内存开销。也可以通过多个容器副本扩容，此时需另行配置负载分配。

## 6. 本地开发与测试

```powershell
$env:PYTHONIOENCODING='utf-8'
cd python-workers/pdf-service

# 只装测试依赖（不需要 docling/torch）
..\..\.venv\Scripts\python.exe -m pip install -r requirements-dev.txt

# 140 个测试：healthz、/v1/parse zip/manifest 契约、公式 latex、错误码、清单校验、
# options（VLM/OCR 覆盖、引擎映射、422 安全消息）
..\..\.venv\Scripts\python.exe -m pytest tests -q
```

fake 模式（无需任何模型，Go 端 live 测试/联调用）：

```powershell
$env:RAG_PDF_SERVICE_CONVERTER='fake'
$env:RAG_PDF_SERVICE_HOST='127.0.0.1'
$env:RAG_PDF_SERVICE_WORKERS='2'
$env:RAG_PDF_SERVICE_CONCURRENCY='2'
..\..\.venv\Scripts\python.exe -m app.main
```

直接使用 `python -m uvicorn app.main:app` 时，应通过 `--workers 2` 指定服务进程数；Uvicorn CLI 不读取项目自定义的 `RAG_PDF_SERVICE_WORKERS`。每进程解析并发仍由应用读取 `RAG_PDF_SERVICE_CONCURRENCY`。

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

Go 端（由后端同事实现）只需：

1. 配置服务地址（建议环境变量 `RAG_PDF_SERVICE_URL`，例如 `http://127.0.0.1:8091`，compose 内为
   `http://pdf-service:8091`）；
2. 对每个 PDF 发 `POST {RAG_PDF_SERVICE_URL}/v1/parse`，multipart 字段 `file`，把响应体解压到解析
   staging 目录（保持 `manifest.json` 与 `pictures/` 的相对路径）；
3. 读 `manifest.json` 反序列化为 `document.PDFManifest`，走现有 `document.BuildPDFContent(...)` /
   资产入库；
4. 非 200 时读 `{"detail"}` 作为用户可见错误；HTTP 客户端超时建议 ≥ `RAG_PDF_SERVICE_TIMEOUT`。

联调顺序建议：先以 `RAG_PDF_SERVICE_CONVERTER=fake` 打通 Go→HTTP→zip→staging→manifest 链路，
再切回 `docling` 做真实文档验收。

## 8. 与旧 worker 的差异 / 已知限制

- **清单版本**：`docling-service-v1`（旧 `docling-formula-remote-v3`）。Go 端需要把 PDF 解析器版本常量与
  校验登记为新版本，否则 `ValidatePDFParserVersion` 会拒绝。
- **zip 只含 manifest.json + 图片**：旧 worker 还会写 `document.json`（内嵌图片、体积大）和 `ocr.json`
  （OCR 明细）。本服务不返回它们；OCR 诊断只保留为 `warnings[]`，明细写入临时目录后丢弃。
- **超时不能中断 Docling**：504 只是快速把错误返回给调用方，后台线程会跑完并一直占用并发槽位；遇到
  重复超时请提高 `RAG_PDF_SERVICE_TIMEOUT` 或拆分文档/扩容副本。
- **内存**：单进程加载 Docling 模型约数 GB（CPU/GPU 视模型而定），上传文件先落临时文件（默认上限
  100 MiB），zip 在内存中组装；并发>1 时每次转换的工作内存线性叠加（权重共享），内存紧张时保持 1。
- **VLM 描述**仅在配置了 VLM（`RAG_VLM_*` 或请求 `options.vlm`）时启用；本地 CodeFormula 模式下无 caption
  的图片/表格保持空描述。
- **OCR 引擎**：`tesseract` 需要镜像内安装系统 Tesseract 与 `chi_sim`/`eng` 语言包（当前 Dockerfile 未装，
  选用前需自行扩展镜像）；`rapidocr` 使用 Docling 自带的 RapidOCR 依赖。`auto` 固定映射为 `easyocr`，
  不使用 Docling 的 `OcrAutoOptions`（避免自动选中未安装引擎导致运行期失败）。
- **每请求 `options` 只影响当前文档**：VLM 客户端按请求构造，不缓存、不复用；并发仍受
  `RAG_PDF_SERVICE_CONCURRENCY`（进程内）与 `options.vlm.concurrency`（单个 VLM 引擎内部并发）两层约束。
- **公式质量**依赖 Docling + CodeFormulaV2/VLM；`_CheckedFormulaEngine` 会把空输出、被截断（缺少
  `</formula>`）的输出当作硬失败，整篇文档 500，避免把错误公式写进索引。
- 本地模式下 EasyOCR 关闭了自动下载（`download_enabled=False`），OCR 模型必须在 `RAG_PDF_MODELS_PATH/EasyOcr`。
