# Durable upload validation — 2026-10-21

- HTTP/MCP uploads now share write-ahead operation/step logs, renewable MySQL
  leases, immutable version staging, atomic head publication and recovery.
- Python regression (excluding three unavailable browser modules): 329 passed,
  18 skipped, four existing MCP deprecation warnings. After the final version-byte
  deletion check, focused transaction/protocol regression: 143 passed, 2 skipped.
- Real MySQL 8 / Qdrant integration: 4 passed in a disposable
  `praxis_txn_test_*` schema and dedicated `txn_test_*` collections. Tests cover
  replay, partial Qdrant write failure, MySQL publication rollback and abrupt
  child-process exit before publication. Test schema/collections were removed.
- Fault injection covers source reception, parsing, JSON/Markdown rendering,
  chunking/tokenization, embedding, sparse encoding and both storage phases.
- Ruff F/E9 checks passed for changed source and transaction tests.
- Production console assets rebuilt successfully with Vite. `npm run build`
  typecheck remains blocked by a missing platform TypeScript `tsc.exe`, including
  after restoring declared optional dependencies; no compiler success is claimed.
- No production document/index migration was performed by this validation.

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

## Local Main Integration

Date: 2026-09-09

- UI commit: `5566207`; merged with the PDF and MCP changes on main at
  `c0f0630`. The README insertion conflict was resolved by retaining both
  the PDF fidelity notes and frontend development instructions.
- On the combined main checkout, `npm ci` and `npm run build` passed.
- HTTP, document tool, and MCP tests: 36 passed, 1 skipped, with four
  existing legacy MCP deprecation warnings. The Vite bundle-size warning
  remains non-blocking.
- Local merge only; no push or existing-service restart. The independent
  frontend worktree and preview service were retained.

# Model API Settings Validation

Date: 2026-09-09

Implemented in `.worktrees/model-api-settings` on `codex/model-api-settings`,
created from local `main` at `f07df00`. No commit, merge, or push was performed.
The original checkout and port-8011 service were not changed.

## Behavior

- Embedding and Reranker now use independently configured HTTP endpoints in
  the standalone HTTP/MCP runtime. No local inference model is loaded there.
- Console supports editable model lists, optional API keys, real connection
  tests, and persistent immediate application. API keys are never returned
  or stored in browser storage; Windows persistence uses DPAPI.
- Configuration mutations and proxy/test requests require a direct local
  request and existing service authentication. Remote configuration is read-only.
- All runtime operations resolve their model dependencies under the same
  configuration lock. Existing namespaces are refreshed together after saving.
- Vector sizes are detected from responses. Occupied namespaces and stored
  Qdrant points prevent changing embedding identity/dimension; changing empty
  collection dimensions creates a new collection without deleting the old one.
- Chunk tokenizer identity is separate from API model identity; both remain
  part of upload deduplication metadata.

## Verified Results

- Frontend TypeScript check and production build passed. Existing Vite warning
  for the approximately 1.03 MB main bundle remains non-blocking.
- Focused HTTP, settings, MCP, document operations and dedup suite:
  **64 passed, 1 opt-in skipped**, four existing legacy MCP warnings.
- Settings boundary suite plus real browser acceptance: **21 passed**.
  Includes a fresh Python process restoring persisted configuration and
  decrypting its key, then successfully calling the configured API.
- Real MCP acceptance: **1 passed**. Added, retrieved, updated, read, and
  deleted a document using actual BGE embeddings through HTTP and real Qdrant.
- Real browser acceptance used actual CUDA BGE-M3 and BGE Reranker behind an
  independent HTTP test provider. Verified empty-key configuration, model
  lists and manual entry, real testing/saving, upload with recursive token
  chunking using a remote model alias, embedding retrieval with reranking,
  occupied-model-change rejection, key non-disclosure/preservation/clearing,
  remote read-only controls and browser reload persistence.
- Screenshots at 1440, 390 and 320 pixels in both themes were inspected.
  No horizontal overflow or unhandled page errors were detected.
- Ruff on changed Python modules/tests and `git diff --check` passed.

## Full-Suite Boundary

The broader suite returned **145 passed, 12 skipped, 5 failed** before the
additional fresh-process case was added. All five failures were reproduced
against the original main checkout using the same environment: four require
missing `docling_core`, and one expects an OCR error but receives a missing
`docling` error. They are not counted as passing or changed in this work.
Full real PDF parsing was not rerun for this model-API change.

## Local Preview and Reproduction

- Preview: `http://127.0.0.1:8031/console`.
- Independent real test provider: `http://127.0.0.1:8032/v1`, with aliases
  `served-embedding` and `served-reranker`. It uses already cached weights
  in the `dl` environment and is not launched by production application code.
- Both preview helpers are left running for inspection. The provider occupies
  GPU memory; original service processes and data paths were not restarted.
- RAG data/Qdrant and logs: `.test-tmp/model-api-acceptance/`;
  provider logs: `.test-tmp/model-api-live/`;
  screenshots: `.test-tmp/model-api-browser/`.
- Python tests use the worktree-local `.venv` created from `conda activate dev`,
  with `pip install -e ".[api]" playwright`. Shared environments were unchanged.

```powershell
conda activate dev
$env:PYTHONPATH = "$PWD/src"
.venv\Scripts\python -m pytest tests/test_model_api_settings.py tests/test_http_service.py tests/test_mcp_http.py tests/test_document_tools.py tests/test_upload_dedup.py -q
$env:RAG_MODEL_API_TEST_URL = "http://127.0.0.1:8031"
$env:RAG_MODEL_PROVIDER_URL = "http://127.0.0.1:8032/v1"
.venv\Scripts\python -m pytest tests/test_real_model_api_ui.py -q -s
$env:RAG_REAL_SMOKE = "1"
.venv\Scripts\python -m pytest tests/test_document_tools.py -k real_mcp -q -s
```
