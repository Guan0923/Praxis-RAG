# Docling 解析服务（Word / HTML）

Praxis-RAG 的独立 HTTP 解析服务：接收一个 `.docx`、`.html` 或 `.htm` 文件，用固定版本的
[Docling](https://github.com/docling-project/docling) 解析，返回一个 ZIP，其中：

- `manifest.json`：版本化清单，Go 端直接映射为 `document.WordManifest`；
- `document.json`：序列化的 `DoclingDocument`（图片以占位符表示），用于审计/排查；
- `images/picture-N.<ext>`：清单 `assets[].image_file` 引用的图片。

服务无状态、不访问数据库/消息队列，也不下载任何模型权重。`.doc` 不在这里处理：Go 端先用
LibreOffice 转成 `.docx`，再把转换结果上传给本服务，用户看到的来源名仍是原 `.doc`。

## 1. 版本与依赖

| 项目 | 值 |
| --- | --- |
| 服务契约 | `docling-service-v1`（`manifest.parser.contract`） |
| DOCX 清单版本 | `docling-word-v1` |
| HTML 清单版本 | `docling-html-v1` |
| Docling | `2.150.1`（`docling-slim[convert-core,format-docx,format-html]`，见 `requirements.txt`） |
| DoclingDocument schema | `1.11.0`（由 Docling 2.150.1 输出） |
| Python | 3.11 |

`requirements.txt` 固定 Docling 版本；升级 Docling 后必须重新跑本目录的测试，并用一个真实
DOCX、一个真实 HTML 复核 `manifest.json` 的表格行列、图片和题注字段。**不要**改用完整
`docling` 包：本服务只解析 DOCX/HTML，不需要 `torch`、布局模型、OCR 模型或 PDF 依赖。

## 2. HTTP 接口

### `GET /healthz`（别名 `/health`、`/`）

始终返回 200，便于容器健康检查；响应体报告固定版本与配置问题：

```json
{
  "status": "ok",
  "service": "praxis-rag-docling-service",
  "version": "1.0.0",
  "contract": "docling-service-v1",
  "docling_version": "2.150.1",
  "formats": [".docx", ".html", ".htm"],
  "config_error": null
}
```

### `POST /v1/parse`

- `Content-Type: multipart/form-data`
- 文件字段：`file`
- 可选文本字段：`options`（当前保留，不读取；便于以后扩展）
- 成功：`200 application/zip`，`Content-Disposition: attachment; filename="docling-parse.zip"`
- 失败：JSON `{"detail": "<可展示的安全文案>"}`，不回显文档正文、上传字节或上游响应体。

| 状态码 | 含义 |
| --- | --- |
| 400 | 输入无效（空文件、损坏的 DOCX、非 UTF-8 HTML、超过大小限制） |
| 415 | 格式不支持（只接受 `.docx`、`.html`、`.htm`） |
| 500 | Docling 转换或清单映射失败 |
| 503 | 服务部署不完整（Docling 未安装/未正确配置） |
| 504 | 单次转换超过 `RAG_DOCLING_SERVICE_PARSE_TIMEOUT` |

请求头 `X-Request-ID` 可以携带调用方的关联 ID（1–64 位 `[A-Za-z0-9._:-]`），服务会在响应头
原样返回并在结构化日志里使用；非法值会被替换为随机 ID。

## 3. 清单契约

`manifest.json` 与 Go 端 `internal/document/wordmanifest.go` 的 JSON 形状一致：

```json
{
  "version": "docling-word-v1",
  "blocks": [
    {"ref": "#/texts/0", "order": 0, "kind": "heading", "text": "标题",
     "location": {"part": "body", "paragraph": 1, "headings": ["标题"],
                  "scope": "body", "section": 1},
     "asset_id": "", "in_cell": false},
    {"ref": "#/tables/0", "order": 9, "kind": "table", "text": "",
     "location": {"...": "..."}, "asset_id": "table-1", "in_cell": false}
  ],
  "assets": [
    {"asset_id": "table-1", "kind": "table", "image_file": null, "mime_type": null,
     "caption": "表 1 硬件参数表", "caption_source": "document",
     "captions": [{"text": "表 1 硬件参数表", "ref": "#/texts/4", "location": {}}],
     "footnotes": [],
     "table": {"num_rows": 4, "num_cols": 3,
               "cells": [{"row": 0, "col": 1, "row_span": 1, "col_span": 2,
                          "text": "规格", "nested_tables": []}],
               "rows": [["部件", "规格", ""], ["处理器", "8 核", "3.2GHz"]],
               "headers": ["部件", "规格", ""],
               "header_rows": [["部件", "规格", ""]],
               "footnotes": [], "parent_cell": null, "caption": "表 1 硬件参数表"},
     "location": {"...": "..."}, "labels": [], "bookmarks": []},
    {"asset_id": "picture-1", "kind": "picture", "image_file": "images/picture-1.png",
     "mime_type": "image/png", "caption": "图 1 系统架构图", "caption_source": "document",
     "captions": [{"text": "图 1 系统架构图", "ref": "#/texts/6", "location": {}}],
     "table": null, "location": {"...": "..."}}
  ],
  "diagnostics": [],
  "skipped_object_count": 0,
  "parser": {
    "contract": "docling-service-v1",
    "service_version": "1.0.0",
    "docling_version": "2.150.1",
    "docling_schema": "1.11.0",
    "input_format": "docx"
  }
}
```

结构保留规则：

- **阅读顺序**来自 `DoclingDocument` 的 body 树（不是扁平的 `texts` 数组），`order` 从 0 连续递增；
- **标题层级**：DOCX 使用 Word 标题样式映射的 `level`；HTML 把 `<h1>`（Docling `title`）记为
  1 级、`<h2>` 起依次加一。HTML 与 DOCX 一样参与 `structure` 切分的标题分组；
- **表格**：逐单元格保留 `row`/`col`/`row_span`/`col_span`，合并延续格折叠进锚点格
  （锚点保留全部文本，延续位置为空），`header_rows` 取开头所有带 `column_header` 标记的行；
- **图片和题注**：只接受 Docling 明确给出的引用（HTML 的 `<figcaption>`、`<caption>`、
  `alt`，或 DOCX 的显式题注关系）。题注文本写进对应资产的 `caption`/`captions`，
  表格题注同时写进 `table.caption`；其它题注型段落留在 `blocks` 里作为正文，由 Go 端做保守关联，
  绝不强行挂到邻近图片；
- **图片字节**：DOCX 图片从 DoclingDocument 的 data URI 解码；HTML 不依赖 Docling 解码内嵌
  data URI，而是按文档顺序自行读取 `<img>`。外部 URL、协议相对地址、相对路径和 `file:` 一律
  不抓取、不读宿主机文件，资产保留题注文字、`image_file` 为 `null`，并在 `diagnostics` 记录原因。

## 4. 环境变量

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `RAG_DOCLING_SERVICE_HOST` | `0.0.0.0` | 监听地址 |
| `PORT` | `8092` | 监听端口（容器内固定 8092） |
| `RAG_DOCLING_SERVICE_TIMEOUT` | `300` | 单个请求的总预算（秒），需大于 parse timeout |
| `RAG_DOCLING_SERVICE_PARSE_TIMEOUT` | `120` | 单次 Docling 转换预算（秒）；超时返回 504，工作线程继续占槽直到结束 |
| `RAG_DOCLING_SERVICE_CONCURRENCY` | `1` | 每个 worker 进程内并发转换数（信号量） |
| `RAG_DOCLING_SERVICE_WORKERS` | `1` | Uvicorn worker 进程数 |
| `RAG_DOCLING_SERVICE_MAX_BYTES` | `52428800` | 单文件大小上限（字节） |
| `RAG_DOCLING_SERVICE_LOG_LEVEL` | `info` | `debug`/`info`/`warning`/`error` |
| `RAG_DOCLING_SERVICE_LOG_FORMAT` | `text` | `text` 或 `json` |
| `RAG_DEBUG_TRACE` | 未设置 | 未设置 LOG_LEVEL 时的调试开关，与 Go 服务共用 |

日志不记录文档正文、上传内容或查询；只记录请求 ID、格式、字节数、块/资产数量和耗时。

## 5. 本地运行

```powershell
cd python-workers/docling-service
python -m venv .venv
.venv\Scripts\python -m pip install -r requirements-dev.txt
.venv\Scripts\python -m pytest            # 契约/表格/题注/错误分支测试
.venv\Scripts\python -m app.main          # http://127.0.0.1:8092
```

Docker：

```powershell
docker build -t praxis-rag-docling-service python-workers/docling-service
docker run --rm -p 8092:8092 praxis-rag-docling-service
```

Go 服务通过 `RAG_DOCLING_SERVICE_URL`（例如 `http://127.0.0.1:8092`）和
`RAG_DOCLING_SERVICE_TIMEOUT`（默认 300 秒）访问本服务。未配置 URL 时：`.docx`/`.doc`
保持进程内解析，`.html`/`.htm` 上传被拒绝；配置 URL 后新上传、替换和重新入库都走本服务。
服务不可达或超时时任务按既有失败/重试流程处理，**不会**静默回退旧 Word 解析器。

## 6. 已验证范围与已知限制

本目录的 12 个测试用真实 Docling 2.150.1 跑通：DOCX（标题/段落/表格合并、内嵌图片）和
HTML（`rowspan`/`colspan`、`<caption>`、`<figcaption>`、内嵌 data URI 图片、外链图片）的清单
内容、ZIP 布局、错误码与超时分支。

仍会丢失或降级的结构（实现时有意取舍，交付说明中同样列出）：

- **公式**：Docling 对 Word OMML 公式输出文本/公式项，本服务保留其文本，不做 LaTeX 结构化；
- **嵌套表格/单元格内图片**：Docling 把表格内部结构放在 table data 中；本服务只映射表格本身，
  不把单元格里的嵌套对象提升为独立资产；
- **页眉页脚、批注、修订**：Docling 归入 furniture 或直接丢弃，不进入正文块；
- **DOCX 题注样式**：Docling 不保留“题注”段落样式；依靠文本模式（图/表 N）在 Go 端保守关联，
  无法识别纯样式题注；
- **HTML 外部/相对图片**：按安全策略不抓取，只保留题注文字；
- **CSS 布局、脚注编号交叉引用**：不保留。
