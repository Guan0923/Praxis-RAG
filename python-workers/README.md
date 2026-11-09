# Python parser workers (external processes)

The Go backend (`rag-service`, workers, CLIs) never imports Python. Two optional
external parser workers remain Python because their native dependencies
(Docling / PyTorch) have no Go equivalent here:

| Worker | Invocation | Purpose |
| --- | --- | --- |
| PDF worker | `python -m rag_mcp.pdf_worker --input <pdf> --output <staging> --manifest ...` | Docling PDF parsing, formula/table extraction, manifest v2 (`docling-formula-remote-v3`) |
| Word vision worker | `python -m rag_mcp.word_vision_worker --manifest ...` | Picture descriptions for Word documents that have no captions |

## VLM: remote, not local

Formula recognition and picture descriptions do **not** load a local VLM any
more. Docling's own VLM stages are pointed at an OpenAI-compatible
`/chat/completions` endpoint (`ApiVlmEngineOptions` + `enable_remote_services`),
and uncaptioned figures/tables are described with plain HTTP calls using the
same endpoint. Layout, table structure and OCR still run locally through
Docling, so the Python environment and the model directory are still required.

| Env var | Meaning |
| --- | --- |
| `RAG_VLM_BASE_URL` | Remote endpoint, e.g. `https://llm.internal/v1` (the worker appends `/chat/completions` when needed). Required to parse formulas or describe images. |
| `RAG_VLM_API_KEY` | Sent as `Authorization: Bearer <key>`. Optional for unauthenticated local servers. |
| `RAG_VLM_MODEL` | Model name the endpoint expects, e.g. `qwen2.5-vl-7b`. Required with the base URL. |
| `RAG_VLM_TIMEOUT` | Per-request timeout in seconds (default 120). |
| `RAG_VLM_CONCURRENCY` | Concurrent VLM requests Docling may issue (default 4). |
| `RAG_PDF_PARSER_PYTHON` | Python interpreter (default `python`). Set to an absolute path for the worker venv. |
| `RAG_PDF_PARSE_TIMEOUT` | PDF parse timeout in seconds (default 900). |
| `RAG_PDF_MODELS_PATH` | Docling local layout/table/OCR models directory. |

The Go runtime sets `PYTHONUTF8=1`, prepends `PYTHONPATH` and injects the
`RAG_VLM_*` variables before spawning the workers, so the API key never appears
on a command line. Point `PYTHONPATH` at this directory (i.e. the parent of the
`rag_mcp` package) when it is not installed into the interpreter's environment:

```powershell
$env:PYTHONPATH = (Resolve-Path .\python-workers).Path
$env:RAG_PDF_PARSER_PYTHON = "C:\path\to\python.exe"
$env:RAG_VLM_BASE_URL = "https://llm.internal/v1"
$env:RAG_VLM_MODEL = "qwen2.5-vl-7b"
python -m rag_mcp.pdf_worker --help
```

## Install dependencies

The PDF worker needs Docling and its runtime dependencies (the Go service itself
does not):

```bash
python -m pip install -r python-workers/requirements.txt
```

CPU-only deployments can install the CPU build of PyTorch per the official
instructions; local layout/table/OCR stages then run on CPU (slower). No GPU is
needed for the VLM stages because they run on the remote endpoint.

## Behaviour contract

* Exact command line: `--input`, `--output`, `--manifest`, optional `--models`
  (see `internal/document/pdf.go`).
* Manifest schema: `docling-formula-remote-v3`; assets are staged next to
  `document.md` and consumed by the Go ingest pipeline. Re-parsing is required
  after upgrading from `docling-formula-ordered-v2` because the VLM changed.
* Exit code 0 on success; non-zero with a safe error on stderr otherwise. The Go
  service maps failures to the Python-compatible validation messages. A failed
  or empty VLM answer fails the document instead of indexing partial output.

Nothing here serves HTTP, MCP, state, retrieval or chunking: that backend is Go
(`cmd/rag-service`, `cmd/rag-*-worker`). Only the two parser entry points above
are supported external-process interfaces.
