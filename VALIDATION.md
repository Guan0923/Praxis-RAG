# UUID results/artifacts and logical deletion — 2026-10-23

- Confirmed one-hour expiry, full backward-compatible search responses, and
  del_flag soft deletion that hides new access but preserves existing UUIDs.
- Results are immutable SQL snapshots with optional Redis acceleration/fallback;
  artifact bytes have their own durable version-bound snapshots. No UUID read
  calls Embedding, retrieval, reranking or parsing.
- Available Python regression: 618 passed, 31 skipped; three optional browser
  modules excluded. Existing four MCP deprecation warnings remain.
- Real MySQL/Redis/Qdrant UUID, cache and transaction suites: 16 passed using
  disposable praxis_uuid_test_*, praxis_cache_test_*, praxis_txn_test_* schemas
  and test-owned prefixes/collections; includes additive legacy-column upgrade.
- Focused core: 110 passed across UUID protocols, durable results and soft delete.
  Real TCP MCP tested in legacy, auto and 2026-07-28 modes.
- Tested exact bytes after update/delete/old-version GC, cross-namespace denial,
  revoked keys, paged byte reads, strict expiry, Redis loss/disabled, SQL/cache
  failures, artifact budgets, duplicate UUID rejection and concurrent registration.
- Ruff F/E9 and editor diagnostics passed. Frontend full typecheck remains
  blocked by the pre-existing missing platform TypeScript tsc.exe; only additive
  response types changed and no new frontend interaction is claimed.
- No production data migration or deletion was performed during validation.

# UUID results/artifacts validation — 2026-10-22

- Confirmed policy: Redis-only search result snapshots, 3600-second default TTL,
  full search response preserved. Missing/expired/evicted results never re-run
  inference. Issued version-bound artifacts remain readable after source deletion
  until their expiry; each request still checks current namespace authorization.
- Default Python regression: 529 passed, 30 skipped, four existing MCP warnings;
  three optional browser modules excluded.
- Focused UUID protocol/result/artifact suite: 95 passed.
- Real MySQL/Redis/Qdrant isolated UUID suite: 3 passed. Covers concurrent stable
  artifact registration, real Redis TTL/NX/eviction and full upload→search→replace
  →delete→old-reference read without invoking search again. Disposable schemas,
  prefixes and Qdrant collections were cleaned; no production data modified.
- Ruff F/E9 and diff checks passed; editor diagnostics clean on UUID storage,
  runtime and MCP/REST modules.
- Redis outage/oversized snapshot returns original search results with explicit
  snapshot_available=false; lookup of an unavailable result returns an error.

# Second-batch state-cache validation — 2026-10-22

- Confirmed strict mode: API-key authentication, Qdrant version selection, lease
  claims/renewal/fencing, commit and rollback remain authoritative SQL reads.
- Redis caches API-key administration, published-head display, lease/holder
  display, latest operation list and redacted step/rollback progress.
- Default Python regression: 434 passed, 27 skipped, four existing MCP warnings;
  three optional browser modules excluded.
- Live MySQL/Redis/RocketMQ/Qdrant plus Redis Lua backend suite: 45 passed using
  isolated praxis_cache_test_* schemas, Redis prefixes and test collections.
  Includes real upload commit/aborted publication/delete refreshing state views,
  stale enabled display not authorizing a disabled key, and expired ownership
  failing fencing despite a live cached snapshot.
- Focused second-batch cache tests prove zero SQL on status cache hits, no raw
  secret/owner/workspace/step input leakage, namespace isolation, rollback events,
  affected-operation refresh after deletion, and no cache events on authority reads.
- Ruff F/E9 passed; editor reports no errors on changed integration files.
- No production document or API-key data was modified by the live test suite.

# First-batch state-cache validation — 2026-10-22

- Cached public knowledge-base configuration, MCP catalog, document lists and
  knowledge-base list/statistics; actual authorization/upload fencing remains
  authoritative. Cache-hit tests assert zero SQL and no tenant/model startup.
- Default Python regression: 399 passed, 24 skipped, four existing MCP warnings
  (three browser modules excluded because their optional environment is absent).
- Isolated MySQL/Redis/RocketMQ and actual Redis Lua: 42 passed, including the
  live MySQL rollback assertion. The live suite uses disposable
  praxis_cache_test_* schemas, unique Redis prefixes and a test consumer group.
- Live coverage includes synchronous refresh, Redis outage fallback, MQ delivery
  after failed cache writes, unacknowledged message redelivery, BIGINT ordering,
  persistent deletion watermark and TTL expiry.
- Versioned delayed-fill/duplicate/out-of-order events, transactional outbox
  rollback and simultaneous Redis/MQ outage are covered by unit/integration tests.
- Docker startup/provisioning script validated with -MigrateOnly -SkipMigrate;
  production documents/credentials were not migrated or modified by these tests.
- Ruff F/E9 passed; no editor diagnostics on new cache code and integration paths.

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
