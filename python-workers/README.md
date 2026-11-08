# Python parser workers (external processes)

The Go backend (`rag-service`, workers, CLIs) never imports Python. Two optional
external parser workers remain Python because their native dependencies
(Docling / PyTorch / vision models) have no Go equivalent here:

| Worker | Invocation | Purpose |
| --- | --- | --- |
| PDF worker | `python -m rag_mcp.pdf_worker --input <pdf> --output <staging> --manifest ...` | Docling PDF parsing, formula/table extraction, manifest v2 (`docling-formula-ordered-v2`) |
| Word vision worker | `python -m rag_mcp.word_vision_worker --manifest ...` | Optional picture descriptions for Word documents that have no captions |

They are configured from the Go service:

| Env var | Meaning |
| --- | --- |
| `RAG_PDF_PARSER_PYTHON` | Python interpreter (default `python`). Set to an absolute path for the worker venv. |
| `RAG_PDF_PARSE_TIMEOUT` | PDF parse timeout in seconds (default 900). |
| `RAG_PDF_MODELS_PATH` | Optional Docling models directory. |

The Go runtime sets `PYTHONUTF8=1` and prepends `PYTHONPATH` before spawning the
workers, so point `PYTHONPATH` at this directory (i.e. the parent of the
`rag_mcp` package) when it is not installed into the interpreter's environment:

```powershell
$env:PYTHONPATH = (Resolve-Path .\python-workers).Path
$env:RAG_PDF_PARSER_PYTHON = "C:\path\to\python.exe"
python -m rag_mcp.pdf_worker --help
```

## Install dependencies

The PDF worker needs Docling and its runtime dependencies (the Go service itself
does not):

```bash
python -m pip install -r python-workers/requirements.txt
```

CPU-only deployments can install the CPU build of PyTorch per the official
instructions. The Word vision worker only needs the standard library, but the
model invocation is performed by the caller-supplied converter/vision commands.

## Behaviour contract

* Exact command line: `--input`, `--output`, optional `--manifest`, optional
  `--models-path` (see `internal/document/pdf.go`).
* Manifest schema: `docling-formula-ordered-v2`; assets are staged next to
  `document.md` and consumed by the Go ingest pipeline.
* Exit code 0 on success; non-zero with a safe error on stderr otherwise. The Go
  service maps failures to the Python-compatible validation messages.

Nothing here serves HTTP, MCP, state, retrieval or chunking: that backend is Go
(`cmd/rag-service`, `cmd/rag-*-worker`). Only the two parser entry points above
are supported external-process interfaces.
