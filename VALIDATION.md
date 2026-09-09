# Chunking Integration Validation

Date: 2026-09-07

Validated the existing five chunking strategies and upload preflight before
integrating the branch. Removed two unused imports and replaced one test lambda
with a named function; no chunking behavior was changed during this review.

## Results

- Ruff: passed using the existing `dev` environment.
- Full tests: 90 passed, 2 opt-in tests skipped, 4 MCP deprecation warnings.
- Real ingestion: 1 passed; BGE-M3 on CUDA, BM25, and isolated local Qdrant.
  All five strategies processed Markdown, TXT, PDF, and DOCX, retrieved evidence
  through embedding/BM25/hybrid, and skipped renamed duplicates.
- Browser: 1 passed against a temporary real service with CUDA embeddings.
  Verified preflight avoids duplicate uploads, changed parameters reindex,
  all five selections, and 1440/390/320-pixel layouts. No page errors.
- The temporary browser service and its child processes were stopped afterward.

## Reproduction

Run from this checkout with its `src` directory on `PYTHONPATH`.
Use `conda activate dl` for model tests and a project-local `.venv` created
with `--system-site-packages`; no shared environment packages were modified.

```powershell
conda activate dev
python -m ruff check src tests
conda activate dl
$env:PYTHONPATH = Join-Path $PWD 'src'
$env:HF_HUB_OFFLINE = '1'
.venv\Scripts\python.exe -m pytest tests -q
$env:RAG_REAL_SMOKE = '1'
.venv\Scripts\python.exe -m pytest tests/test_real_chunking.py -q -s
$env:RAG_BROWSER_URL = '<temporary local test service URL>'
.venv\Scripts\python.exe -m pytest tests/test_browser_chunking.py -q -s
```

The initial sandboxed test run failed on Windows temporary-directory permissions.
The results above are from a rerun with appropriate local execution permissions,
using separate test directories rather than the existing knowledge store.
Browser screenshots remain in ignored `.mini_agent-rag/chunking-ui/`.

# Ant Design Console Validation

Date: 2026-09-09

Implemented on `codex/ant-design-console`, in `.worktrees/ant-design-console`,
created from local `main` at `f6e8515`. No commit, merge, push, or restart of
the existing port-8011 service was performed.

## Console Results

- TypeScript check and Vite production build: passed in `conda activate dev`.
- Focused HTTP and browser suite: **15 passed** in 31.34 seconds.
- Two browser cases used actual uploads and searches against local Qdrant,
  BGE-M3 and BGE Reranker on CUDA in the `dl` environment. Verified all five
  chunking strategies, renamed duplicate skipping without a second upload,
  changed-parameter reindexing, original text, recall and rerank scores,
  sequential three-mode comparison with reranking disabled, deletion and
  cancellation, and a mixed batch with one indexed TXT and one invalid DOCX.
- Three supplementary browser cases used controlled transport responses to
  verify failed-search clearing, repeat-request prevention, stale search and
  document response isolation after namespace changes, invalid parameters,
  and partially failed comparison. The batch case also paused preflight to
  check the upload lock before continuing to the real API.
- Both themes checked at 1440, 390 and 320 pixels across all three pages,
  upload dialogs and strategy menus. Screenshots were visually reviewed;
  no page overflow or unhandled JavaScript errors were detected. Theme
  persistence and session-only Token storage were verified.
- Wheel built in a fresh build directory and extracted into a separate import
  directory. The packaged app served `/`, `/console` and both referenced
  compiled assets successfully; old `app.js` and `styles.css` were absent.
- Vite reports one non-blocking bundle-size warning: the main script is about
  1.02 MB before compression, 326 KB gzip. No runtime CDN is required.

## Isolated Preview

- Address: `http://127.0.0.1:8021/console` (left running for inspection).
- Data and local Qdrant: `.test-tmp/ant-console-runtime/` in this worktree.
- Collection: `ant_console_acceptance`; browser cases create unique `ui-*`
  namespaces and clean up only their own uploaded documents.
- Screenshots: `.test-tmp/console-browser/`; clean wheel:
  `.test-tmp/console-wheel/`.
- No existing knowledge documents or model settings were changed. The full
  unrelated backend suite and PDF parsing benchmarks were not rerun for this
  frontend-only change.
