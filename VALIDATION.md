# Historical UUID results/artifacts and logical deletion — 2026-10-23

> **Transport note (2026-11-09):** The dated records in this file were produced
> while the repository still used RocketMQ. The current transport is Kafka
> (`RAG_KAFKA_*`, explicit `RAG_*_DLQ_TOPIC`); the Kafka/MySQL/Debezium live check
> is recorded immediately below. RocketMQ wording in the historical sections is
> retained as history.

## In-process Go PDF engine — 2026-11-10

Delivered: PDF upload no longer requires a Python/Docling installation. The new
`document.NativePDFParser` (`internal/document/pdf_native.go`, reader
`github.com/ledongthuc/pdf`) implements both `ingest.PDFParser` and
`document.PDFPages`, walks each page content stream, groups runs into visual
lines, merges superscripts, removes headers/footers/page numbers and TOC dot
leaders, normalizes ligatures, repairs hyphenated line breaks, keeps CJK text
unspaced and only sorts runs by X when the reader exposes glyph widths (so
zero-width CJK CID fonts keep the content-stream order). It writes the same
`docling-formula-remote-v3` manifest (no chunk/metadata migration) with
`parser.engine=go-native` and page/block/character/elapsed metrics.

`RAG_PDF_ENGINE` (`go` default, `docling` optional) selects the engine; when
unset, an existing `RAG_PDF_PARSER_PYTHON` keeps Docling, so old deployments are
unchanged. Docling remains the only engine with layout analysis, table
structure, OCR, formula recognition and picture descriptions.

Automated checks (worktree `go-pdf-parser`, commit `27b1659`):

- `go build ./...`, `go vet ./internal/document ./internal/config ./internal/runtime ./internal/ingest` — clean.
- `go test ./internal/document ./internal/config ./internal/ingest -count=1` — pass. New cases: generated valid PDF (manifest version/blocks/order/`Pages`/`manifest.json`), broken input → `*PDFParseError`, textless PDF → “没有可索引的内容”, ligature/page-number/dehyphenation/CJK/spacing heuristics, `RAG_PDF_ENGINE` parsing/defaults, and runtime engine selection (`TestUploadPDFOptionsSelectsEngine`).
- `go test ./internal/runtime -run 'Upload|Pdf|PDF' -count=1` — pass (upload/PDF-related runtime tests, including `TestSafeUploadErrorTextPdfParseMessage`).

Live end-to-end check (service on `127.0.0.1:8011`, MySQL `praxis_rag`, KB `asdjasdajbsc`, embedding `http://aihub-api.goertek.com:30080/v1` + `bge-m3`):

- `POST /v1/documents` (multipart `files=2404.16130v2.pdf`, header `X-RAG-Namespace: asdjasdajbsc`) → `200` in 48.5 s, `failed: []`, `imported[0].chunk_count = 35`, no `pdf_parser` error in the server log.
- Stored manifest (`blobs` `pdf-assets/doc-850325c2ab92aea47056c511dd37528b/manifest.json`): `version=docling-formula-remote-v3`, `parser={"engine":"go-native","library":"github.com/ledongthuc/pdf"}`, `metrics={pages:26, blocks:349, characters:89856, elapsed_ms:2082}`, `assets=0`, `warnings=0`; first blocks are the paper title/author lines.
- `upload_operations` row `2c37cfc8ce1846ef67a1b844d0330905` = `COMMITTED`, `error NULL`; steps `receive_source → deduplicate → chunking_tokenizer → stage_mysql → embedding → sparse → stage_qdrant` all `DONE`.
- `POST /v1/search` (`rerank:"none"`, KB has no reranker configured) returns the PDF chunks (top hit score 0.75, GraphRAG title text), and `GET /v1/documents/2404.16130v2.pdf/content` serves the original PDF bytes. `GET /console` and the Vite dev server (`:5173`) return 200.

Parser quality spot-checks (before the live run): arXiv `2404.16130v2.pdf` → 26 pages / 349 blocks / 89,856 chars, correct title/authors/headings/paragraphs; Chinese `新员工IT手册.pdf` → 23 pages / 28 blocks / 15,718 chars with correct CJK reading order after the width-availability fix. Known gaps: two-column PDFs can interleave columns, tables/figures/formulas become plain text (no assets), scanned pages need OCR and surface as empty-content/no-text warnings — use `RAG_PDF_ENGINE=docling` for those.

## 异步上传默认化与内嵌文档 worker — 2026-11-10

Delivered: `POST /v1/documents`（网页 Console 与 REST）默认改为异步受理：每个文件在状态库单事务写入 `document_jobs` + `document_job_outbox` 后立即返回 `{"mode":"async","jobs":[...],"failed":[...]}`，解析/切分/Embedding/Qdrant 发布由文档 worker 在后台完成；`RAG_UPLOAD_MODE=sync` 保留为显式 opt-in 的旧同步行为。无 Kafka 的单进程部署由 `rag-service` 内嵌文档 worker 直接排空已提交的发件箱（`RAG_DOCUMENT_WORKER_INPROCESS`：未设置/`auto` = 未配置 Kafka 时启用；`1/true/on` = 强制启用，配置 Kafka 时拒绝启动；`0/false/off` = 关闭），轮询间隔 `RAG_DOCUMENT_WORKER_POLL_SECONDS`（默认 0.5 秒）。`documentworker.WithOutboxReceiver` 统一 service 内嵌 worker、独立 worker 本地模式与 REST 异步 e2e 的接线；`scripts/start-stack.ps1` 现在随 `rag-service` 启动/停止 `dist/bin/rag-document-worker.exe`。OpenAPI、`.env.example`、README、USAGE、DEPLOYMENT、PROGRESS 同步更新。

Verification (Windows, Go 1.27.3, MySQL 8 on 127.0.0.1:3307 with the isolated `praxis_cache_test_smoke` schema, Qdrant/Redis from the local stack; `GOCACHE`/`GOTMPDIR` redirected into the worktree because the machine caches are outside the sandbox):

- `go build ./...` → exit 0；`go vet ./internal/... ./cmd/...` → exit 0。
- 定向测试：`go test ./internal/config ./internal/httpapi ./internal/documentworker ./cmd/rag-service -count=1` → 全部 `ok`（config 0.4 s、httpapi 145.8 s、documentworker 270.2 s、cmd/rag-service 0.3 s；本轮机器负载较高，耗时高于包内基线）。
- 全量串行套件：`go test ./... -p 1 -count=1 -timeout 40m` → exit 0，25 分 05 秒，所有含测试的包 `ok`，无 `--- FAIL`/`--- SKIP`；重点包 `internal/state` 429.8 s、`internal/runtime` 471.1 s、`internal/e2e` 319.9 s（含 `TestRESTAsyncUploadPipeline`）、`internal/httpapi` 131.1 s、`internal/documentworker` 55.2 s、`internal/access` 20.3 s、`cmd/rag-service` 0.3 s。
- 前端（本次未改前端，仅回归）：`npm run typecheck` 通过；`npm test` 54/54（5 个文件）；`npm run build` 通过（仅既有 >500 kB chunk 提示）。
- 实机冒烟（`cmd/rag-service` 新二进制监听 127.0.0.1:18099，`praxis_cache_test_smoke` 状态库，模拟 OpenAI 兼容服务每次 embedding 延迟 2000 ms）：8/8 检查通过 —— `POST /v1/documents` 在 380 ms 内返回 `mode:"async"` 且 job `status:"queued"`（远小于 2000 ms 的解析延迟）；受理后立即读取 job 为 `queued`/`processing`，此时 `GET /v1/documents` 尚未包含该文件、`/health` 与文档列表均在 1 s 内响应；服务日志显示 `document_worker=in-process`、`poll_seconds=0.5`；后台任务最终 `status:"succeeded"`、`stage:"completed"`，随后文档出现在列表中且正文可读。该冒烟同时证明默认值（不设置 `RAG_UPLOAD_MODE` / `RAG_DOCUMENT_WORKER_INPROCESS`，不配置 Kafka）即可得到期望行为。
- 与 main 的 PDF 内嵌引擎改动（`27b1659`/`bffdcf4`）合并后复验（rebase 到 `bffdcf4`，工作区模块缓存 `GOMODCACHE` 指到 worktree 以便拉取新 PDF 依赖）：`go build ./...` 通过、`go vet ./internal/... ./cmd/...` 通过；`go test ./internal/config ./internal/httpapi ./internal/document ./internal/documentworker ./cmd/rag-service -count=1` 全部 `ok`（config 3.2 s、httpapi 37.0 s、document 1.9 s、documentworker 67.1 s、cmd/rag-service 0.2 s）；`go test ./internal/e2e -run TestRESTAsyncUploadPipeline -count=1` ok（86.6 s）；`go test ./internal/runtime -run 'Upload|Pdf|PDF' -count=1` ok（222.6 s）；用合并后的二进制重跑上节实机冒烟 8/8 通过（上传 389 ms 返回）。

Known limits: `scripts/start-stack.ps1` 的子进程生命周期只做了 PowerShell 语法解析（`[Parser]::ParseFile`），沙箱内无法启动 Docker/Kafka 前台子进程做端到端验证；真实浏览器级 Console 交互仍无自动化（仓库无 Playwright/Cypress），异步轮询 UI 由 Vitest 契约测试与 REST 异步 e2e 覆盖。`rag` MySQL 用户无 `CREATE DATABASE` 权限，新 schema 无法创建，冒烟复用了由测试套件迁移过的 `praxis_cache_test_smoke` 并使用唯一 namespace。行为变更：不再设置 `RAG_UPLOAD_MODE` 的部署从同步变为异步，必须有文档消费者（无 Kafka 自动内嵌；Kafka 需 `rag-document-worker` 或显式 `RAG_UPLOAD_MODE=sync`）。

## Per-account API-key ownership and isolation — 2026-11-10

Delivered: every API key now records the Console account that issued it
(`api_keys.owner_user_id`, nullable for legacy rows; additive migration +
`ix_api_keys_owner`), and the five existing `/v1/admin/api-keys` routes move to
the new `authSessionOrLocal` policy: any signed-in role manages only its own
keys (cookie writes need `X-CSRF-Token`, Bearer sessions are exempt), trusted
loopback keeps the unowned/break-glass behavior, and foreign keys are
indistinguishable from missing (404 `API Key 不存在`). Non-administrator
sessions may bind only knowledge bases they own (`access.Service.OwnedNamespaces`;
403 `只能为本人创建的知识库创建 API Key`) because API keys bypass
knowledge-base/document authorization; updates validate only newly added
namespaces so a demoted administrator can still rename, disable or delete keys.
`/v1/namespaces` gains the additive `owned_namespaces` field, administrator
listings join `users` for `owner_username`, account deletion cascades owned keys
in the same transaction (and bumps the `api_keys` read-model revision). The
Console API-key page is open to all roles, shows the owner column for
administrators and limits the namespace picker to owned knowledge bases. Docs
updated: [PERMISSIONS](docs/PERMISSIONS.md) §9/§10, [api-keys](docs/api-keys.md),
[USAGE](docs/USAGE.md), README, [DEPLOYMENT](docs/DEPLOYMENT.md),
[state-cache](docs/state-cache.md), PROGRESS.

Verification (Windows, Go 1.27.3, MySQL 8 + Redis + Qdrant from the local compose
stack; `GOCACHE` redirected into the worktree because the machine cache is
outside the sandbox):

- `go build ./...` / `go vet ./...` — clean.
- New regression tests: `internal/state/api_keys_owner_test.go` (owner
  projection, `ListByOwner` scoping, legacy NULL rows, update keeps owner,
  account-delete cascade), `internal/httpapi/api_keys_owner_test.go`
  (list/read/update/delete/mcp-config isolation, 404 indistinguishability,
  CSRF/bearer/loopback policy, create/update binding rules, no-store, admin
  visibility), `internal/e2e/api_keys_http_test.go` (real router + sessions +
  CSRF + auth/access/MySQL end to end, admin ownership, `owned_namespaces`).
- Targeted rerun after the review fixes: `go test -p 1 -count=1 -timeout 900s
  -run 'TestAPIKey|TestUserDeleteCascadesAPIKeys|TestSchemaMatchesPythonDefinition|TestAutoSchemaCanBeDisabled|TestSchemaCreationCanBeForcedOff|TestStoresWiresG7StateMachines|TestDocumentJobReconcileCommittedPublication'
  ./internal/state ./internal/httpapi ./internal/e2e` → `ok` state 106.2 s,
  httpapi 23.5 s, e2e 9.6 s.
- Frontend: `npm run typecheck` clean; `npm test` 32/32 (5 new API-key helper
  contract tests); `npm run build` green.
- Full serial suite: `go test ./... -p 1 -count=1 -timeout 25m` → exit 0, every
  package passes (state 421.0 s, httpapi 182.9 s, e2e 252.9 s, runtime 464.7 s,
  documentworker 54.5 s; ≈24 minutes wall time on this loaded machine).

Environment note: an earlier full-suite attempt reported only two G7 tests
failing with `Table 'praxis_gotest.document_task_outbox' doesn't exist` while
another worktree's Go test process was also using the shared `praxis_gotest`
schema (observed `state.test` from
`.agents/worktrees/dsh-200abc8a-f229`). `newTestStores` runs schema
creation/migration before `statetest.Prepare` takes its `GET_LOCK`, so a
concurrent process that drops/recreates the schema is visible in that window —
a pre-existing harness race, not a regression from this change. The green run
above was executed with no other test process on the schema; a private schema
could not be created because the `rag` MySQL user has no `CREATE DATABASE`
grant.

Known limits (also in [PERMISSIONS](docs/PERMISSIONS.md) §10): disabling an
account revokes its sessions but not its already-issued keys (delete the account
or disable/delete the keys); the namespace binding stores the knowledge-base
name, so a deleted-then-recreated knowledge base remains reachable by keys
previously issued for that namespace.

## Model configuration page and model list — 2026-11-10

Delivered: a Console-only 模型配置 page (`#/model-profiles`, administrators
only) that maintains reusable Embedding / Reranker / VLM connections; the MySQL
`model_profiles` table (`kind` + `name` unique, `base_url`, `model`, encrypted
`protected_key`, probed `vector_size`) with keys protected by
`RAG_STATE_ENCRYPTION_KEY`; the seven `/v1/admin/model-profiles` routes (list,
create, details, PATCH, delete, `/key`, `/models`, local admin or administrator
session only); and a create/edit knowledge-base form that quick-selects a saved
model instead of typing the base URL, key and model id. The knowledge-base REST
contract is unchanged — the Console copies the selected profile's connection
into the existing inline provider objects. Profiles and knowledge bases are
decoupled: editing or deleting a profile never mutates a knowledge base that
already copied its connection, and an Embedding selection change still requires
the re-embedding confirmation.

Environment: Windows 11 host, Go 1.27.3 windows/amd64, Node v26.13.0 / npm
12.2.2, MySQL 8 on 127.0.0.1:3307 (isolated schema `praxis_cache_test_smoke`),
Qdrant/Redis from the local compose stack. Two workspace-sandbox accommodations:
`GOCACHE`/`GOTMPDIR` redirected inside the worktree, and Vite/Vitest run with a
`child_process.exec` preload shim because the sandbox refuses Vite's piped
`net use` probe. All Go test output below is from the isolated schema; a
concurrent DSH session on the same machine was running its own Go tests against
the shared `praxis_gotest` schema (leftover `state.test.exe` processes), which
caused DDL races in unrelated schema tests — see the note at the end.

Go verification (cwd worktree):

```
$ go build ./...
exit 0 (only the pre-existing sandbox warning "writing stat cache: ... D:\go-work\pkg\mod\cache ... Access is denied")

$ go vet ./internal/...
exit 0, no output

$ go test ./internal/kbconfig ./internal/httpapi -count=1 -timeout 600s
ok   github.com/Guan0923/xinghe_rag/internal/kbconfig   0.343s
ok   github.com/Guan0923/xinghe_rag/internal/httpapi    11.340s

$ go test ./internal/state -run 'TestModelProfile' -count=1 -timeout 600s
ok   github.com/Guan0923/xinghe_rag/internal/state      35.485s

$ go test ./internal/runtime -run 'TestModelProfile' -count=1 -timeout 600s
ok   github.com/Guan0923/xinghe_rag/internal/runtime    79.162s

$ go test ./internal/e2e -count=1 -timeout 1200s
ok   github.com/Guan0923/xinghe_rag/internal/e2e        306.471s

$ go test ./internal/state -count=1 -timeout 1800s        # full package, isolated schema
ok   github.com/Guan0923/xinghe_rag/internal/state      426.978s
```

Console verification (cwd frontend, NODE_OPTIONS shim set):

```
$ npm run typecheck
exit 0, no diagnostics

$ npm test
Test Files  4 passed (4)
Tests  49 passed (49)
     src/navigation.test.ts (11 tests)
     src/api.test.ts (24 tests)
     src/CreateKnowledgeBase.test.ts (7 tests)
     src/ModelProfilesPage.test.tsx (7 tests)

$ npm run build
../build/web/assets/index-CW9lPsz8.js   1,250.64 kB │ gzip: 395.07 kB
../build/web/assets/index-CsXgnAdA.css     13.51 kB │ gzip:   3.38 kB
✓ built in 808ms
exit 0 (only the pre-existing >500 kB chunk advisory)
```

Live HTTP smoke (built `cmd/rag-service` from this worktree on 127.0.0.1:18099,
isolated MySQL schema, loopback admin calls without a session, mock
OpenAI-compatible provider on 127.0.0.1:18077; 14 checks, all PASS):

```
GET  /v1/admin/model-profiles empty list
POST embedding profile -> probes provider, records vector_size=4, api_key_set=true, key absent from payload
GET  list -> no key leaked;  GET /{id}/key -> returns sk-smoke
POST /{id}/models with no request body -> provider models (3)
PATCH rename -> 200, no re-probe, vector_size kept
duplicate (kind,name) -> 409;  missing base_url -> 400;  unknown kind -> 422 literal_error
POST /v1/admin/knowledge-bases using the saved connection -> 201, vector_size=4, embedding_model=mock-embedding
PATCH knowledge base with the same connection, reembed=false -> 200 (no re-embedding required)
DELETE -> 204;  DELETE unknown id -> 404
GET /openapi.json exposes /v1/admin/model-profiles, /{profile_id}, /{profile_id}/key, /{profile_id}/models
SMOKE OK (14 checks)
```

Coverage added:

- `internal/state`: repository lifecycle (create / list / list-by-kind / get /
  update / delete, generated id and timestamps), duplicate `(kind, name)` →
  conflict, table shape.
- `internal/kbconfig`: draft validation (kind, name/model length limits, service
  URL required, trimming), key preservation (an omitted key keeps the stored one
  while the base URL is unchanged; changing the URL without a key or an explicit
  empty key clears it), probe routing (embedding records the dimension, reranker
  gets one bounded call, VLM is not probed), credential-free public projection,
  encryption round-trip.
- `internal/runtime`: profile lifecycle through the service, error mapping
  (409 conflict, 404-shaped missing profile, 503 state failure).
- `internal/httpapi`: list/details/key/models, create (201), PATCH/delete
  (200/204), FastAPI-shaped 422 validation bodies, 404/409 mapping, admin-only
  and runtime-unavailable guards, OpenAPI route table.
- Console: 49 tests total. `ModelProfilesPage.test.tsx` (7 tests: per-kind
  counts/rows/saved-key state, kind filtering and per-kind empty hints,
  informative empty state, add-drawer required-field validation, create with
  the default kind, explicit empty `apikey` on PATCH clears the saved key,
  untouched key field omits `apikey` on PATCH), `CreateKnowledgeBase.test.ts`
  (7 tests for `matchingProfile` / `profileNeedsReembed`), `api.test.ts` (24
  tests; model-profile client contract: frozen kind vocabulary, list
  normalization, create body, PATCH by encoded id, DELETE empty-body path,
  key/model routes, duplicate conflict detail) and `navigation.test.ts` (11
  tests; `#/model-profiles` parse and hash round-trip).

Not verified live: browser E2E of the new page was not run (no
Playwright/Cypress in this repository); provider probes were exercised against
a mock OpenAI-compatible endpoint plus the unit-test fakes, not a real model
service.

Shared-schema caveat: the first full `go test ./internal/state` attempts on the
shared `praxis_gotest` schema failed in `TestAutoSchemaCanBeDisabled`,
`TestSchemaCreationCanBeForcedOff` and `TestMigrationsAddMissingLegacyColumns`
because a concurrent DSH session on the same machine was running its own Go
tests against the same schema (observed leftover `state.test.exe` processes
performing DROP/ALTER). The same three tests pass when run alone, and the full
package is green on the isolated schema above; none of them exercise
model-profile code.

## Test workflow hardening — 2026-11-10

Goal: make the repository's test workflow actually reliable, and cover the
Console-facing roles/ownership/sharing feature end to end.

- **Fixed a real bug the suite had been hiding.** `TestRESTAsyncUploadPipeline`
  was already red on the untouched `8df52da` baseline: the asynchronous publish
  path committed the document row plus the upload journal in one transaction but
  refreshed only the upload read models. With the Redis cache attached the
  `documents`/`kb_list` revisions stayed stale, so `GET /v1/documents` answered
  from an empty cached snapshot right after the worker published. The publish
  path now calls the new `state.DocumentRepository.FlushViews` after commit
  (`internal/runtime/documents_publish.go`); the e2e test and the full suite are
  green.
- **New full-stack HTTP e2e**: `internal/e2e/permissions_http_test.go` (≈46 s)
  runs the real `httpapi` router with real cookie sessions + CSRF, the real
  `auth` and `access` services and the real MySQL state over the harness' fake
  model/Qdrant servers. It covers: role management (only admins; normal users
  cannot open the picker), ownership visibility before/after a grant, invitation
  create → inbox → preview (wrong account 403) → accept, read-only list/delete/
  upload gates, non-owner and non-VIP invite refusals, the write grant +
  upload + document-list path, template-based self-creation with recorded
  ownership, grant listing and revocation.
- **Console unit/contract tests**: Vitest + jsdom with a worker-thread pool
  (`pool: "threads"`; forked workers are unavailable in this sandbox and are not
  needed). `frontend/src/api.test.ts` (17 tests) pins the api client contract —
  namespace/bearer headers, CSRF on writes only, the 401 handler scoping, error
  detail formatting, sharing endpoint URLs/bodies and null-list normalization.
  `frontend/src/navigation.test.ts` (10 tests) pins hash-route parsing, invite
  token parsing and namespace validation. `npm test` → **27/27 pass**;
  `npm run typecheck` and `npm run build` stay green.
- **Runner reliability**: `-p 1 -count=1 -timeout 25m` is now baked into
  `scripts/build-go.ps1 -Test` and `scripts/build-go.sh test` and documented in
  README/USAGE; `make test`, `make test-frontend` and `make check` added.
  Every package truncates the same MySQL schema behind `GET_LOCK(...,600)`, so
  parallel packages queue past the default 10 m package timeout — serial is not
  a preference, it is required.
- **CI**: `.github/workflows/test.yml` runs `go vet`/`go build`/`go test -p 1`
  with a MySQL 8 service container and a separate frontend
  `typecheck + npm test + build` job.
- **Full serial suite**: `go test ./... -p 1 -count=1 -timeout 25m` → every
  package passes, exit 0, ≈16 minutes total (`internal/e2e` 205.4 s,
  `internal/runtime` 462.5 s, `internal/state` 400.6 s, `documentworker` 53.3 s).
  The previously failing e2e case is part of that green run.

## Roles, knowledge-base ownership and invitation permissions — 2026-11-09

Delivered: `admin` / `vip` / `user` roles (only administrators change roles),
knowledge-base ownership (`knowledge_bases.owner_user_id`), invite-link sharing
with read/write permissions over single-file / multi-file / whole-knowledge-base
scopes, and enforcement across REST, MCP, document lists, search results and
artifacts. Design/spec: `.dev/DESIGN.md` (local scratch); user docs:
[docs/PERMISSIONS.md](docs/PERMISSIONS.md).

Go verification (Windows, Go 1.27.3, MySQL 8 + Redis + Qdrant from the local
compose stack; `GOCACHE` redirected into the worktree because the machine cache
is outside the sandbox):

- `go build ./...` and `go vet ./internal/...` — clean.
- `go test ./... -p 1 -count=1 -timeout 25m` — every package passes except
  `internal/e2e TestRESTAsyncUploadPipeline`, which also fails on the pristine
  `8df52da` baseline in a separate worktree (pre-existing, unrelated to this
  iteration: `documents after publish = []`).
- New coverage: `internal/access` (scope resolution, invitation lifecycle,
  expiry/revoke/idempotent accept, validation, 20s), `internal/state`
  (permission/invitation repositories, ownership, delete cascades),
  `internal/httpapi` (role checks, `authSession`/CSRF, ownership 403s, document
  filtering, search `DocumentIDs`, invitation endpoints, 409 template mapping),
  `internal/runtime` (template clone, restricted-search visibility filter,
  `ResolveDocumentIDs`), `internal/mcp` (per-request scope through the internal
  `X-Rag-Mcp-Scope` header, resource/tool/write filtering, queued-search 403).
- Full `internal/state` suite: 403.9s green; `internal/runtime`: 458.4s green.

Console verification:

- `npm run typecheck` — clean.
- `npm run build` — clean (1,239.66 kB JS / 13.41 kB CSS). Vite 8 aborts in this
  sandbox because it probes Windows network drives through `child_process.exec`
  (pipe EPERM); the build was verified with an uncommitted `child_process.exec`
  shim via `NODE_OPTIONS`, which only affects the probe.

Live HTTP smoke (`dist/bin/rag-service.exe` on the disposable MySQL schema,
Qdrant/Redis from the stack; 35 assertions): roles and role updates, admin-only
user picker, 409 for self-create without a template, owned/unowned namespace
visibility, invitation create → inbox → preview (wrong account 403) → accept,
whole-KB read vs. single-file write effective access, read-only upload/delete
403, grant listing/revoke, non-owner VIP and normal-user invite 403, MCP 401,
Console assets and OpenAPI entries.

Not verified live: end-to-end document upload/search inside a shared knowledge
base (needs a reachable embedding/reranker provider), queued-search behaviour for
restricted grantees under a Kafka deployment, and browser E2E of the new pages.

## Kafka migration validation — 2026-11-09

- Live Docker Desktop stack with existing MySQL/Qdrant/Redis data volumes:
  Apache Kafka 3.9.0 KRaft container `healthy`; Kafka 3.9.0 started; topics
  auto-created with 4 partitions and the start-stack provisioning created
  cache/document/search plus both explicit DLQ topics.
- Debezium Server 3.7.1.Final on MySQL 8.0: `Snapshot completed` and
  `Starting streaming`; native Kafka sink producer logged
  `bootstrap.servers=[kafka:9092]`, `acks=-1`, idempotence enabled and String
  serializers.
- Consumed a real Debezium record from `praxis_document_ingest` whose
  `artifact_id` matched the MySQL `document_job_outbox` row; the row remained
  `PENDING` (the application did not publish), proving the CDC bridge delivered it.
- Verified on 2026-11-09 with the Go backend: the document async pipeline
  end-to-end in both bridge modes (`scripts/verify-debezium-e2e.py`: 14/14
  debezium, 13/0 relay, outbox PENDING vs PUBLISHED), live DLQ delivery for a
  poison payload (`scripts/verify-kafka-dlq.py`) and queued MCP search through
  the Kafka search topic with Redis snapshots (`scripts/verify-kafka-search.py`).
- Not yet verified: SASL/TLS Kafka clusters, Docker app image/Compose profiles,
  systemd units and live multi-instance MySQL failover.

The dated results below describe their tested revisions, not verification of the
current architecture migration. The approved active design used schema 2 complete
Kafka requests (document chunks: at most 512 KiB decoded; files: at most 10 MiB),
all-message broker confirmation before API success, and required Redis transient
task/reassembly/result storage. Search snapshots use `RAG_RESULT_TTL_SECONDS=3600`
with no SQL fallback. Task CAS and atomic result/success writes are Redis-local,
not an atomic Redis+MySQL transaction. MySQL retains published documents/blobs,
KB/API keys/catalog, artifact snapshots and UploadJournal operations/leases;
document success follows durable publication, reconciled via journal on retry.
Old task/outbox/result tables are retained but inactive, with no job/result
migration. Drain old workers before deploying API/workers together: schema 2 is
incompatible with UUID-only messages. See [deployment](docs/USAGE.md) and
[storage boundaries](docs/state-storage.md).

Current migration validation — 2026-10-25:

- Non-browser suite: **841 passed, 37 skipped, 1 deselected** in 141.35 seconds.
- Three browser files (`test_browser_model_options.py`,
  `test_browser_navigation.py`, `test_browser_word.py`) excluded because collection
  requires missing Playwright; it was not installed for these unrelated tests.
- Deselected `tests/test_model_api_settings.py::test_failed_reembedding_preserves_old_configuration_and_index[publish]`:
  the same failure was separately reproduced from pristine HEAD in an isolated
  extracted checkout. Known pre-existing failure; not fixed by this migration.
- Changed/added Python files have zero **new** Ruff findings versus HEAD;
  existing baseline findings remain. `git diff --check` passed.
- No live Kafka/Redis validation was performed for that historical revision; the later Kafka/MySQL/Debezium migration check is recorded at the top of this file.

- At this revision, confirmed one-hour expiry, full backward-compatible search
  responses, and del_flag soft deletion hiding new access while preserving UUIDs.
  Current durable retention guarantees apply to MySQL artifacts; Redis search
  result IDs can become unavailable after data loss/TTL expiry.
- This historical revision used immutable SQL result snapshots with optional
  Redis acceleration/fallback; that design is superseded, not the active contract.
  Artifact bytes had their own durable version-bound snapshots. No UUID read
  called Embedding, retrieval, reranking or parsing.
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

# Historical UUID results/artifacts validation — 2026-10-22

These are historical checks of the then-current snapshot implementation, not
schema 2 queued-request, chunk replay/ACK or migration acceptance verification.

- Policy confirmed at that revision: Redis-only search result snapshots, 3600-second default TTL,
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
- At that revision, Redis outage/oversized snapshot returned synchronous search
  results with explicit snapshot_available=false; unavailable-result lookup errored.
  This is not queued MCP acceptance during an outage: queued operations require
  Redis/Kafka and all producer acknowledgements, with no durable SQL fallback.

# Historical second-batch state-cache validation — 2026-10-22

These checks concern MySQL-backed display read models, not the new Redis-only
MCP task/reassembly/result stores. Raw-input exclusion for these display views
does not forbid temporary raw document chunks in Redis reassembly.

- Confirmed strict mode: API-key authentication, Qdrant version selection, lease
  claims/renewal/fencing, commit and rollback remain authoritative SQL reads.
- Redis caches API-key administration, published-head display, lease/holder
  display, latest operation list and redacted step/rollback progress.
- Default Python regression: 434 passed, 27 skipped, four existing MCP warnings;
  three optional browser modules excluded.
- Live MySQL/Redis/Kafka (historical RocketMQ test stack)/Qdrant plus Redis Lua backend suite: 45 passed using
  isolated praxis_cache_test_* schemas, Redis prefixes and test collections.
  Includes real upload commit/aborted publication/delete refreshing state views,
  stale enabled display not authorizing a disabled key, and expired ownership
  failing fencing despite a live cached snapshot.
- Focused second-batch cache tests prove zero SQL on status cache hits, no raw
  secret/owner/workspace/step input leakage, namespace isolation, rollback events,
  affected-operation refresh after deletion, and no cache events on authority reads.
- Ruff F/E9 passed; editor reports no errors on changed integration files.
- No production document or API-key data was modified by the live test suite.

# Historical first-batch state-cache validation — 2026-10-22

Redis-outage fallback and SQL outbox statements in this section apply only to
persistent MySQL read-model refresh. They do not provide SQL acceptance or result
fallback for queued MCP uploads/search in the current architecture.

- Cached public knowledge-base configuration, MCP catalog, document lists and
  knowledge-base list/statistics; actual authorization/upload fencing remains
  authoritative. Cache-hit tests assert zero SQL and no tenant/model startup.
- Default Python regression: 399 passed, 24 skipped, four existing MCP warnings
  (three browser modules excluded because their optional environment is absent).
- Isolated MySQL/Redis/Kafka (historical RocketMQ test stack) and actual Redis Lua: 42 passed, including the
  live MySQL rollback assertion. The live suite uses disposable
  praxis_cache_test_* schemas, unique Redis prefixes and a test consumer group.
- Live coverage includes synchronous refresh, Redis outage fallback, Kafka delivery
  after failed cache writes, unacknowledged message redelivery, BIGINT ordering,
  persistent deletion watermark and TTL expiry.
- Versioned delayed-fill/duplicate/out-of-order events, transactional outbox
  rollback and simultaneous Redis/Kafka outage are covered by unit/integration tests.
- Docker startup/provisioning script validated with -MigrateOnly -SkipMigrate;
  production documents/credentials were not migrated or modified by these tests.
- Ruff F/E9 passed; no editor diagnostics on new cache code and integration paths.

# Historical durable upload validation — 2026-10-21

- HTTP/MCP uploads at this revision shared write-ahead operation/step logs,
  renewable MySQL leases, immutable version staging, atomic head publication and
  recovery. These MySQL publication/recovery guarantees remain; they do not mean
  the new queued MCP request or Redis success state is atomically committed with
  MySQL, nor verify the new broker-confirmed submission/chunk protocol.
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

## PDF 解析错误信息 — 2026-11-10

现场：Console 批量上传 `2404.16130v2.pdf` 返回「知识库操作失败：errorString」，服务端日志无错误详情。

排查结论（MySQL `upload_operations` / `upload_steps`）：

- 失败操作 `cfbb8f8f5058fdb869a58a2deb2a7fec`（namespace `asdjasdajbsc`）状态 `ROLLED_BACK`，失败步骤 `chunking_tokenizer`，`error` 列只存了脱敏类别 `errorString`；`receive_source`、`deduplicate` 已完成，`rollback` 完成，没有残留文档。
- 根因是 PDF 解析依赖外部 Docling Python worker 而本机没有该环境：`python -m rag_mcp.pdf_worker --help` 直接报 `ModuleNotFoundError: No module named 'rag_mcp'`（系统 Python 3.11；`.venv` 里也没有 `docling`），`.env` 未设置 `RAG_PDF_PARSER_PYTHON`/`PYTHONPATH`；同库的 `.txt` 上传在 13:51 成功。
- `errorString` 来自 `safeUploadErrorText` 的兜底分支：`shortTypeName` 输出 Go 类型名，而 `fmt.Errorf`（无 `%w`）产生的就是 `*errors.errorString`。Python 原实现 `_safe_error` 用的是 `type(exc).__name__`，所以这是移植后丢掉可读性的缺陷。

修复与验证：

```powershell
$env:GOCACHE = ".test-tmp\gocache"
$env:RAG_TEST_MYSQL_URL = "mysql://rag:praxis-dev-pw@127.0.0.1:3307/praxis_gotest"
go vet ./internal/document ./internal/runtime ./internal/ingest
go test ./internal/document ./internal/ingest -count=1
go test ./internal/runtime -run "TestSafeUploadErrorTextPdfParseMessage" -count=1 -v
go test ./internal/runtime -run "Upload|Pdf|PDF|StopIndex|Recover" -count=1 -timeout 20m
```

- `internal/document`：`TestExternalPDFParserErrors/missing_python_module`（stderr 含 `No module named 'rag_mcp'` 时返回 `*PDFParseError`，消息含模块名与 `RAG_PDF_PARSER_PYTHON`）、`TestMissingPythonModule`（带引号/不带引号/无标记三种 stderr）通过；原有失败/超时/manifest 错误消息保持不变。
- `internal/runtime`：`TestSafeUploadErrorTextPdfParseMessage` 通过；上传相关子集（`Upload|Pdf|PDF|StopIndex|Recover`）223.4s 全绿。
- 重建 `dist/bin/rag-service.exe` 并以同样生产配置重启 8011，`/health` 200；再次上传 PDF 时响应应为「PDF 解析不可用：Python 解析环境缺少模块 rag_mcp…」而不是 `errorString`。
- 已知未处理：旧 debug 上传 `2c060059701b999a604c3d42863e3982`（namespace `debug222146`）的历史工作区不在当前数据根下，回收循环每 10 秒打一条 `Unsafe upload workspace`（ValueError）日志，与本问题无关，待单独清理。
