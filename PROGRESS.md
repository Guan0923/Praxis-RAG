# RAG MCP 开发进展

最后更新：2026-11-11

## 总体目标

```text
Agent 决定检索动作
  -> MCP search
  -> Retriever Registry
  -> Qdrant Dense/Sparse/Hybrid
  -> Reranker Registry
  -> Top-K 可引用证据
```

## Phase 1：Mock MCP 闭环

- [x] 建立独立 `mini-agent-rag-mcp` 包。
- [x] 定义稳定的 `search` MCP Schema。
- [x] 只允许业务参数，不暴露 Qdrant 和模型实现参数。
- [x] 建立 `RagOrchestrator`、Retriever 和 Reranker 接口。
- [x] 实现确定性的内置知识检索。
- [x] 返回 `document_id`、`chunk_id`、`source` 和原文证据。
- [x] 增加参数边界和 filter 白名单。
- [x] 增加 `rag-mcp-debug` 调试入口。
- [x] 增加单元测试和 MCP 边界测试。
- [x] 完成真实 MCP `initialize -> list_tools -> call_tool` 握手验证。
- [x] 写入用户级 `servers.toml`，Mini-Agent 导入名为 `mcp_rag_search`。

## Phase 2：Qdrant Dense Retrieval

- [x] 同时支持 Qdrant HTTP 服务和本地持久化模式。
- [x] 定义 collection 和 `dense` named vector，维度固定为 1024。
- [x] 支持 Markdown/TXT 文档发现、UTF-8 读取和确定性切分。
- [x] 实现稳定 `document_id`、内容相关 `chunk_id` 和 100 字符 overlap。
- [x] 封装 BGE-M3 dense embedding，并使用 lazy import/lazy load。
- [x] 实现重复导入时按 `document_id` 替换旧 Chunk。
- [x] 实现 Qdrant `DenseRetriever`、filter 白名单和证据 payload 投影。
- [x] 增加 `rag-mcp-ingest`，输出文件数、Chunk 数和失败信息。
- [x] 根据 `RAG_BACKEND` 在 Mock 与 Qdrant backend 之间切换。
- [x] 保持 MCP `search` Schema 不变，并把运行错误转换成 MCP tool error。
- [x] 使用 Fake Embedder + 内存 Qdrant 验证维度、filter、幂等导入和引用字段。
- [x] 启动 HTTP Qdrant 并下载 BGE-M3，完成本机真实模型端到端检索。
- [x] 增加可选模型预热和 secret-free 阶段追踪，解决 Windows CPU 下 MCP 首次调用超时。
- [x] 使用 Mini-Agent 宿主加载真实 `mcp_rag_search` 并验证引用结果。
- [x] 增加认证 Web 知识库 API 和“用户设置 -> RAG 知识库”管理页面。
- [x] 支持网页批量上传 Markdown/TXT 后自动切分、Embedding 和 Qdrant 入库。
- [x] 支持同名文件原子替换、入库失败回滚，以及源文件和对应向量一起删除。
- [x] 为 Qdrant Point、检索、替换和删除增加用户 namespace 隔离。
- [x] 增加上传大小/数量/类型、路径 confinement、用户隔离和 Windows 回滚测试。
- [ ] 建立带相关性标注的 Dense Recall@K 测试集。

## Phase 3：BM25 与 Hybrid

- [x] 增加 `sparse` named vector，并启用 Qdrant collection-level IDF。
- [x] 实现确定性的中英文 BM25 sparse 文档和查询编码。
- [x] 实现 `BM25Retriever`，保留 payload filter 和用户 namespace 边界。
- [x] 使用 Qdrant Query API 和服务端 RRF 实现 `HybridRetriever`。
- [x] Web 上传和 CLI 入库同时生成 Dense + Sparse 双向量。
- [x] 为 Dense-only Collection 增加安全迁移错误，不自动删除旧数据。
- [x] 使用 E03 测试文档对比 BM25、Dense 和 Hybrid 的真实结果。
- [ ] 建立带人工相关性标注的 Recall@K 回归数据集。

## Phase 4：Reranking

- [x] 实现懒加载、线程安全的真实 BGE Cross Encoder Reranker。
- [x] 评估 Bi-Encoder：缺少收益证据，暂时保留 Mock，避免无依据增加第二套模型。
- [x] 限制候选数量、文本字符、模型 token 长度和批量大小。
- [x] 校验模型分数数量、类型和有限值，失败时保持统一 RAG tool error。
- [x] 记录检索与重排耗时，但不记录敏感原始输入。
- [x] 使用真实 CPU 模型完成 Hybrid + Cross Encoder MCP 宿主验证。

## Phase 5：Agentic Retrieval

- [x] 编写明确的工具描述和 NoSearch、单次检索、查询改写、多跳检索策略。
- [x] 仅在当前 Agent 请求实际包含 `mcp_rag_search` 时注入策略，避免错误宣称 RAG 可用。
- [x] 返回 `evidence_status` 和 `answer_guidance`，空结果明确要求不得猜测。
- [x] 要求知识回答引用 `source` 和 `chunk_id`，并把工具返回内容视为不可信证据而非指令。
- [x] 在知识库设置页显示 MCP 总开关、工具、策略、Collection 和 namespace 配置状态。
- [x] 为策略条件注入、只读工具注解、空证据和状态 API/页面增加自动测试。
- [x] 建立五类固定 Agent 行为用例，并按工具选择、查询策略、证据覆盖和依据性回答四个维度评分。
- [x] 提供 Fake Model 自动回归、显式确认的真实模型 CLI/API，以及 Web 单项/批量调试入口。
- [ ] 使用真实模型行为集评测 NoSearch、单次检索、查询改写和多跳检索。
- [ ] 建立答案引用准确率和无证据拒答率回归数据集。
- [ ] 设计可信只读 MCP 的免审批机制；默认继续审批。

## Phase 6：独立 RAG Service

- [x] 新增 `rag-service`，默认独立监听 `127.0.0.1:8011`。
- [x] 将源文件、manifest、切分、Embedding、Sparse、Qdrant、Reranking 和搜索迁入服务。
- [x] 按 `RAG_SERVICE_DATA_ROOT/<namespace>/` 隔离租户文件，并对全部 Qdrant 操作附加相同 namespace。
- [x] Backend 知识库 API 改为认证代理，不再保存、解析或索引知识源文件。
- [x] Backend 本地注册稳定 `mcp_rag_search` ToolSpec，实际调用时才连接 RAG Service。
- [x] namespace 只由 Backend 注入 Header，不进入浏览器或 Agent 可见参数。
- [x] 配置远程服务后自动屏蔽旧 `rag` stdio server，避免重复工具和模型重复加载。
- [x] 增加可选 Bearer token；非 loopback 部署强制要求 token。
- [x] 增加只读预览、显式 `--apply` 的旧知识源迁移命令。
- [ ] 将同步上传/删除改成可观察、可取消、可重试的后台 Job。

## 当前已知限制

1. 默认 `RAG_BACKEND=mock`，三个检索模式仍共用确定性 Mock；需显式设置 `qdrant` 才使用 Dense。
2. Qdrant backend 的 `embedding`、`bm25` 和 `hybrid` 均已真实实现；当前中文 BM25 使用单字和双字词切分，尚未接入领域词典。
3. `cross_encoder` 已使用真实 BGE Reranker；`bi_encoder` 仍是 Mock，需由后续评测决定是否实现。
4. 当前 Mini-Agent 会把全部外部 MCP 工具标记为需要确认，并排除在 Plan mode 之外。
5. 当前返回的 `debug` 字段用于开发，生产阶段需要由配置控制是否暴露。
6. Web 上传当前只接受 UTF-8 `.md`/`.txt`，每批最多 10 个、单文件最大 10 MiB；PDF、Word、网页抓取和 OCR 尚未实现。
7. 上传和删除目前是同步请求；大文件或 CPU Embedding 会占用较长时间，后续应改成可观察、可重试的后台任务。
8. Agentic Retrieval 当前由系统策略引导，不是确定性路由器；固定行为评测框架已完成，但真实模型的多轮稳定通过率尚未评测。
9. Agentic RAG 行为集使用确定性测试证据，专门评估 Agent 决策；它不衡量真实 Qdrant 的 Recall@K，真实模型评测也必须由用户明确确认后才运行。
10. 旧版 `~/.mini_agent/<user>/rag/knowledge` 不自动迁移；需先运行 `rag-service-migrate` 预览，再显式加 `--apply`。旧文件不会被删除。
11. 远程搜索目前使用稳定 Agent ToolSpec + 受控 HTTP 服务边界，工具名保持 `mcp_rag_search`；尚未切换为 MCP Streamable HTTP transport。

## 2026-08-26 Phase 2 验证记录

```text
uv run --isolated --package mini-agent-rag-mcp --with pytest pytest rag_mcp/tests -q
14 passed

uv run --isolated --package mini-agent-rag-mcp --with ruff ruff check rag_mcp
All checks passed!
```

本轮开始时 Docker Desktop daemon 未运行，且项目 `.venv` 被本地后端占用，因此先在 uv 隔离环境完成自动验证，没有中断正在运行的服务。

随后已完成真实环境联调：

```text
Qdrant: mini-agent-qdrant, http://localhost:6333
Collection: mini_agent_phase2_demo
Model: BAAI/bge-m3, CPU, 1024 dimensions
Ingest: 1 file / 1 chunk / 0 failures
Dense score: 0.599715（样本问题）
MCP preload initialization: 9.81s
MCP search after preload: 0.36s
Host tool: mcp_rag_search, has_source=True
```

最终回归：RAG 测试 14 passed，Ruff 通过，宿主 MCP/安全边界测试 29 passed（存在一个原有 coroutine warning）。用户级 MCP 已切换到 Qdrant backend，MCP capability 已启用。

## 2026-08-26 Web 知识库与用户隔离

已完成的调用链：

```text
KnowledgeSettings.tsx
  -> POST /api/knowledge/documents
  -> KnowledgeDocumentStore
  -> Markdown/TXT chunking
  -> BGE-M3 embedding
  -> Qdrant named vector: dense
  -> manifest.json
```

用户源文件位于 `~/.mini_agent/<user_id>/rag/knowledge/`。Web 后端从该用户的 `mcp/servers.toml` 读取 RAG 配置，并校验 `RAG_NAMESPACE` 必须等于当前用户的稳定哈希；namespace 不暴露给 Agent，但会自动约束 Qdrant 的全部写入、查询、替换和删除。

本轮验证结果：

```text
Knowledge API + Phase 2 tests: 13 passed
Ruff: All checks passed
Qdrant migration: 1 namespaced point, 0 legacy unnamespaced points
Backend health: ok; list/upload/delete routes present in OpenAPI
Real MCP: initialize -> list_tools -> call_tool
Implementation: qdrant-dense-v1
Result: 1 namespaced agentic-rag.md evidence
Focused backend regression: 52 passed
Frontend typecheck: passed
Frontend production build: passed
Knowledge/User Settings components: 15 passed
```

前端入口为“用户设置 -> RAG 知识库”。上传会自动生成 Embedding；删除会先删 Qdrant 向量，再删源文件。已登录浏览器的最终点击联调仍需在用户会话中完成；自动化组件测试覆盖上传、显示、错误提示和删除交互。

验证中仍可见项目原有的 FastAPI lifespan 弃用提示、Ant Design/jsdom 渲染提示，以及 MCP shutdown timeout 测试中的 coroutine warning；它们不影响本轮测试通过，后续应单独清理。

## 2026-08-26 Phase 3 Hybrid 验证记录

实现后的真实存储结构：

```text
Collection: mini_agent_phase3_hybrid
Status: green
Dense vector: dense / 1024 dimensions / cosine
Sparse vector: sparse / Qdrant IDF modifier
Points: 3（两个用户 namespace，共两个源文档）
Legacy collection: mini_agent_phase2_demo（保留，未删除）
```

同一个问题“星河 X7 出现 E03 应该怎么处理？”的真实检索结果：

```text
BM25: 2 results; top score=4.034924; E03=True
Dense: 2 results; top score=0.626848; E03=True
Hybrid RRF: 2 results; top score=0.833333; E03=True
Implementation: qdrant-hybrid-v1
```

真实 Mini-Agent MCP 宿主验证：

```text
Loaded tool: mcp_rag_search
Server health: rag=healthy
Call mode: hybrid / rerank=none
First source: rag-upload-test.md
First evidence contains E03: true
Focused backend/RAG regression: 57 passed
Ruff: All checks passed
Frontend typecheck/build: passed
KnowledgeSettings component: 1 passed
```

Phase 2 的 Dense-only Collection 无法原地增加 Sparse Vector。迁移采用新建 `mini_agent_phase3_hybrid` 并从每用户 `rag/knowledge/` 重新入库的安全策略，旧 Collection 保留用于回退。

## 2026-08-26 Phase 4 Cross Encoder 验证记录

本机资源约束下选择 `BAAI/bge-reranker-base`：单份权重约 1.06 GiB，支持中英文；未默认使用约 2.17 GiB 的 `bge-reranker-v2-m3`。模型可通过 `RAG_RERANKER_MODEL` 替换。

```text
Fake-model RAG suite: 27 passed
Real standalone rerank:
  first download/load: 123.56s
  E03 evidence score: 0.995823
  irrelevant filter evidence score: 0.001262

Real Hybrid + Cross Encoder:
  implementation: qdrant-hybrid-rerank-v1
  retrieval_ms: 13658.938（冷加载）
  rerank_ms: 7699.693（冷加载）
  first source: rag-upload-test.md
  first score: 0.998717

Real Mini-Agent MCP host with cached models:
  tool: mcp_rag_search
  health: healthy
  initialization: 12.06s
  first call: 6.39s
  retrieval_ms: 1377.322
  rerank_ms: 5005.462

Focused backend/RAG regression: 63 passed
Ruff: All checks passed
```

当前配置预热 BGE-M3、懒加载 Reranker，初始化和首次调用均低于 60 秒 MCP 超时。Cross Encoder 会把原始检索分数写入结果 `metadata.retrieval_score`，最终 `score` 表示归一化重排相关性。

## 2026-08-26 Phase 5 Agentic Retrieval 与配置可观察性

已完成的 Agent 控制边界：

```text
allowed_tools 包含 mcp_rag_search
  -> 注入 Agentic Retrieval 系统策略
  -> 模型选择 NoSearch / Single / Rewrite / Multi-hop
  -> MCP 返回 evidence_status + answer_guidance + 可引用证据
  -> Agent 基于 source + chunk_id 回答，或在无证据时拒绝猜测
```

知识库列表接口新增无模型加载的 `rag_status`。它只读取当前用户的 `config.toml` 和 `mcp/servers.toml`，检查 MCP capability、`rag` server、Qdrant endpoint、Collection 和用户 namespace，不连接 Qdrant、不加载 BGE 模型。前端据此显示“已配置/未就绪”以及具体原因，并在配置无效时禁用上传。

本轮聚焦验证：

```text
Knowledge API + system prompt + orchestrator: 35 passed
KnowledgeSettings: 2 passed
Frontend typecheck: passed
Focused Ruff: All checks passed
```

这些测试证明策略只在工具存在时注入、MCP 空证据返回契约正确、配置状态可见；它们不等价于真实模型已经稳定做到正确 NoSearch、改写和多跳。固定行为集及轨迹评分框架已在后续小节完成，下一步是在用户明确确认后执行真实 Agent run 并统计多轮稳定性。

本地服务重启后的只读检查：

```text
Backend health: ok
Existing user configurations: 2 / 2 configured
MCP capability: enabled
Tool: mcp_rag_search
Strategy: agentic_auto
Collection: mini_agent_phase3_hybrid
Namespace validation: 2 / 2 valid
Qdrant: green, 3 points, 3 indexed vectors
```

前端全量测试在与 Python 全量测试和构建并行运行时有 3 个无关 UI 用例超过 5 秒；将对应两个测试文件串行重跑后 28/28 通过。Python 全量套件在约 7 分钟仍未退出，已停止该挂起进程；本轮相关的 35 项 Python 聚焦回归、全量 Ruff、前端生产构建和相关组件测试均通过。后续需要单独定位全量 Python 套件中的既有挂起测试。

## 2026-08-26 Phase 5 Agentic RAG 行为评测框架

新增独立评测链路：

```text
固定行为用例
  -> Mini-Agent 真实运行循环
  -> 确定性只读 mcp_rag_search fixture
  -> 记录模型调用、查询和已返回证据
  -> 四维自动评分
```

五类用例分别验证 NoSearch、单次检索、查询改写、多跳检索和无证据拒答。四个评分维度是 `tool_selection`、`query_strategy`、`evidence_coverage` 和 `grounded_answer`。fixture 不连接 Qdrant、不加载 Embedding，使 Agent 工具决策问题不会与检索召回问题混在一起。

已提供三个调试入口：

```text
CLI: python -m benchmarks.rag_agentic.run --list / --case / --all
API: GET /benchmark/rag/tasks, POST /benchmark/rag/run, POST /benchmark/rag/run-all
Web: Benchmark 成绩单 -> Agentic RAG 行为评测（Phase 5）
```

CLI 和 API 都要求显式确认真实模型调用，Web 使用确认弹窗；未确认时 API 返回 HTTP 400，且不会读取模型配置或调用模型。自动测试使用 Fake Model，不产生模型费用。

本轮验证结果：

```text
Agentic RAG Fake Model/API tests: 8 passed
System prompt + orchestrator + RAG evaluation regression: 35 passed
BenchmarkPage: 4 passed
Frontend typecheck: passed
Frontend production build: passed
Ruff check: All checks passed
Focused Ruff format: 9 files already formatted
```

全仓库 `ruff format --check .` 还报告 17 个既有文件需要格式化；为避免改写无关用户内容，本轮只验证了新增和本次修改的 Python 文件。真实付费模型行为评测尚未执行，需用户在页面或 CLI 明确确认后进行。

## 2026-08-27 Phase 6 独立 RAG Service

完成后的所有权边界：

```text
Frontend :5173
  -> Mini-Agent Backend :8000（认证、namespace、Agent ToolSpec、HTTP 代理）
       -> RAG Service :8011（源文件、manifest、切分、Embedding、检索、重排）
            -> Qdrant :6333（Dense + Sparse vectors）
```

Backend 不再导入 BGE/Chunking/Qdrant 入库实现，也不在用户目录保存新上传的知识源文件。`RAG_SERVICE_URL` 配置后，Backend 注册稳定的 `mcp_rag_search`，并屏蔽旧 `servers.rag` stdio 工具；只有上传、列表、删除或 Agent 实际调用搜索时才建立服务连接。namespace 由认证用户 ID 稳定派生并写入 Header，不出现在 Agent Schema。

安全和迁移边界：

- 服务默认只监听 `127.0.0.1:8011`。
- 非 loopback 地址强制要求 `RAG_SERVICE_TOKEN`，Backend 与服务端必须使用同一值。
- 新源文件位于 `~/.mini_agent-rag/<namespace>/`。
- `rag-service-migrate` 默认只读预览；`--apply` 复制并重建索引，保留旧文件。
- 迁移写入前应停止 RAG Service，避免两个进程同时更新 manifest。

本轮验证：

```text
Backend proxy + RAG Service API + MCP/tool boundaries + prompt/orchestrator: 58 passed
KnowledgeSettings: 2 passed
Frontend typecheck: passed
Frontend production build: passed
Focused Ruff format: passed
Full Ruff format check: 16 pre-existing files still require formatting
Real RAG Service smoke on :18011:
  health=ok
  collection=mini_agent_phase3_hybrid
  implementation=rag-service-qdrant-hybrid-rerank-v1
  isolated test namespace: no_evidence / 0 results
Python environment: 128 packages compatible; torch + FlagEmbedding present
```

真实联调只执行查询，不上传、删除或改写 Qdrant 数据；测试 namespace 没有读取到其他用户已有向量。临时服务和测试目录均已停止/删除。

## 2026-08-27 Phase 7 可下载 MCP 模组与独立 Console

本轮把 Phase 6 的“独立后端服务”继续收敛为宿主无关的完整模组：

```text
RAG Console :8011
       -> RAG Service API :8011
              -> Qdrant :6333

任意 Agent/MCP Host
       -> 标准 stdio rag-mcp
              -> 按需 HTTP 调用 RAG Service :8011
```

已完成：

- [x] `rag-service` 自带独立 Web Console，不再依赖 Mini-Agent Frontend。
- [x] Console 支持上传、列表、删除以及 BM25/Embedding/Hybrid + Reranker 检索调试。
- [x] Console 显示 evidence status、候选数、retrieval/rerank 耗时、score、source 和 chunk_id。
- [x] `rag-mcp` 在配置 `RAG_SERVICE_URL` 后成为薄 stdio MCP 适配器，实际调用时才连接服务。
- [x] namespace 改为宿主无关的安全标识，默认 `default`，不进入 Agent 工具参数。
- [x] Python 包包含 Console 静态资源，可通过本地路径或 Git `subdirectory=rag_mcp` 安装。
- [x] 删除 Mini-Agent Backend 的 `/api/knowledge`、专用 RAG HTTP 客户端和内置 ToolSpec。
- [x] 删除 Mini-Agent 主前端的知识库设置页；Mini-Agent 只保留通用 MCP Host 能力。
- [x] 保留旧数据迁移工具；没有自动移动、删除或重建真实用户知识数据。

兼容边界：不配置 `RAG_SERVICE_URL` 时，`rag-mcp` 仍保留原有进程内直连 Qdrant 模式。Phase 6 的历史记录保留用于解释迁移过程，但当前部署方式以 README 的 Phase 7 说明为准。

本轮验证：

```text
RAG Service / remote MCP / Mini-Agent generic MCP focused regression: 60 passed
Frontend: typecheck passed; 153 tests passed; production build passed
Ruff lint: passed
Focused Ruff format: passed
Wheel 0.2.0: built; Console HTML/CSS/JS package data verified
Browser smoke: search and connection pages rendered; health status and generated MCP config verified
Full Python suite: 749 tests collected, but did not finish within the 420-second command limit
```

全量 pytest 在两次执行中均只表现为长时间无输出并达到命令上限，没有产生失败用例或完成汇总，因此不能记录为“全量通过”。本次变更相关的聚焦回归、全部前端测试、包构建和真实浏览器渲染均已完成。浏览器联调使用独立临时目录和 `18011` 端口，结束后已停止服务并删除临时文件，没有上传、删除或迁移真实知识数据。

## 2026-08-27 Phase 8 检索调试可观测性

真实排查发现：三种 Retriever 的分派正确，但测试知识库只有 14 个 Chunk，而默认 `candidate_k=30`，因此各模式会把相同候选全集交给 Cross Encoder。与此同时，Cross Encoder 因模型源连接超时返回 HTTP 503，旧 Console 只弹出短暂 Toast 并继续显示上一次成功结果，造成“切换模式没有生效”的错觉。

本轮改进：

- [x] 搜索开始和失败时清空旧结果；HTTP 错误改为持久页面错误，不再伪装成成功结果。
- [x] 明确显示服务实际返回的 retrieval、rerank、候选数与阶段耗时。
- [x] Cross Encoder 结果同时显示最终重排分数和 `metadata.retrieval_score`。
- [x] 增加“三模式对比”，固定 `rerank=none` 并排展示 BM25、Embedding 和 Hybrid。
- [x] 默认 `candidate_k` 从 30 调整为 10，并同步 MCP Schema、HTTP、CLI 和 Console。
- [x] `Bi Encoder` 在 Console 明确标记为 Mock。
- [x] `/health` 返回 Embedding/Reranker 模型名、设备和进程内加载状态，不触发模型下载。
- [x] Cross Encoder 下载或网络失败时显示可操作提示：先关闭重排验证召回，再准备模型缓存并重启服务。

本轮验证：

```text
RAG / MCP / decoupling focused regression: 62 passed
Ruff lint: passed
Console JavaScript syntax: passed
Wheel 0.3.0: built; HTML/CSS/JS package data verified
Real browser compare on :8011: BM25 / Embedding / Hybrid all succeeded
Real browser error-state check: stale comparison cleared and persistent error displayed
Responsive visual check: six metrics and three compare columns fit without horizontal clipping
```

真实对比仍只执行只读检索，没有上传、删除或改写知识数据。当前正在运行的旧 RAG Service 进程需要重启后，`/health` 才会包含新增模型状态字段；Console 静态文件刷新后即可看到其他界面改进。

## 2026-08-27 Phase 9 独立仓库发布准备

本轮将 RAG MCP 模组整理为可直接克隆和安装的独立仓库：

- [x] Git 安装地址切换为 `Guan0923/xinghe_rag`，不再依赖 Mini-Agent 仓库的 `subdirectory`。
- [x] 包元数据补充 README、Repository 和 Issues 地址。
- [x] 增加 `.gitignore`，排除 Python 缓存、构建产物、本地环境、模型缓存和 Qdrant 数据。
- [x] 从版本控制中移除误加入的 `__pycache__`、`.pyc` 和 `egg-info` 生成文件。
- [x] 发布前扫描敏感文件和常见凭据格式，不包含用户 API Key、知识库数据或本地模型缓存。

发布前验证：Ruff lint 通过，模块测试 39 项通过，`0.3.0` sdist/wheel 构建成功。独立仓库只包含源码、测试、调试 Console、文档和示例知识文件。

## 2026-08-27 Phase 10 PDF 与 Word 文档读取

本轮在现有“落盘 -> 文本切分 -> Embedding/BM25 -> Qdrant”链路前增加独立文档读取层，索引和检索协议保持不变：

- [x] 使用 `pypdf` 提取带文本层的 `.pdf`，保留页间换行后进入现有切块器。
- [x] 使用 `python-docx` 按正文顺序提取 `.docx` 段落和表格单元格。
- [x] Console 文件选择器、上传提示和文档 API 支持 `.pdf`、`.docx`。
- [x] PDF 加密、损坏、超页数和无文本层时返回可操作错误；扫描件明确提示先执行 OCR。
- [x] DOCX 在解析前校验 ZIP 条目数、加密标记和解压总大小，避免压缩炸弹耗尽资源。
- [x] 文档提取文本统一限制为 500 万字符；原有单文件 10 MiB 上传限制保持不变。
- [x] 旧版二进制 `.doc` 仍不支持，需先转换为 `.docx`。

新增解析依赖只运行在 RAG Service 内，不改变 MCP `search` 工具 Schema，也不会让 Agent 直接接触原始文件。验证覆盖真实最小 PDF、真实 DOCX 段落/表格、扫描件提示、损坏文档和扩展名发现。

本轮验证：Ruff lint/format 通过，独立模块测试 47 项通过，Console JavaScript 语法通过；`0.4.0` sdist/wheel 构建成功，PDF/Word 依赖和 Console HTML/CSS/JS 均已包含在包元数据或 wheel 中。

## 2026-11-09 角色、知识库归属与权限邀请（Go 后端）

本轮为 Console 账号增加三级角色与知识库级权限系统，全部在 Go 后端与 React 控制台内实现：

- [x] 角色扩展为管理员（`admin`）、VIP（`vip`）、普通用户（`user`），仅管理员可改角色；保留“至少一名启用管理员”保护。
- [x] `knowledge_bases` 增加 `owner_user_id`（含老库补列迁移）；知识库默认归创建者私有，历史无主知识库仅管理员可用。
- [x] 所有已登录用户可创建自己的知识库；VIP/普通用户从 `RAG_DEFAULT_KB_TEMPLATE`（默认 `default`）模板克隆模型连接，管理员保留完整 provider 表单。
- [x] 新增 `kb_permissions`（可读/可修改 × 单文件/多文件/全库）与 `kb_invitations`（只存 token 摘要）两张表及仓储、访问解析 `internal/access`。
- [x] 邀请流程：管理员/VIP 创建邀请 → 被邀请人收到站内通知与一次性链接 → 登录并接受后写入授权；拒绝/过期/撤销/重复邀请语义完整。
- [x] 归属与权限在 REST 层强制执行：文档列表过滤、上传/替换/删除逐文件写校验、检索结果与快照按可见文档过滤、附件与上传状态读校验、`/v1/namespaces` 过滤。
- [x] MCP 同套可见性：请求上下文解析 scope（stateful 会话经 `X-Rag-Mcp-Scope` 内部头逐请求传递），search/结果/资源按文档过滤，写工具逐文件校验，受限账号拒绝排队检索。
- [x] Console：角色导航、权限管理页、邀请中心与 `#/invite?token=` 详情页、顶部待处理邀请铃铛、只读文档页、VIP/普通用户简化建库表单。
- [x] 文档与验证：新增 `docs/PERMISSIONS.md`，更新 README/USAGE/DEPLOYMENT/.env.example；Go 全包测试（除既有 e2e 失败）、前端 typecheck/build、35 项真实 HTTP 冒烟通过，详见 VALIDATION.md。

已知限制：邀请消息仅站内通知（无邮件/IM）；文件级写授权只能替换已授权文件，不能新建；受限账号在 Kafka 排队搜索部署下会被拒绝异步检索；历史无主知识库需管理员显式授权。

## 2026-11-10 测试工作流完善

本轮把"能不能放心跑测试"补成可复用的工作流，而不只是散落在会话里的命令：

- [x] 修复被红灯掩盖的真实缺陷：异步发布提交 documents/upload journal 后只刷新上传读模型，`documents`/`kb_list` 缓存停在旧版本；`runtime.publishDocument` 在提交后新增 `DocumentRepository.FlushViews`，`TestRESTAsyncUploadPipeline` 由红转绿。
- [x] 新增 `internal/e2e/permissions_http_test.go`：真实会话 Cookie + CSRF、真实 auth/access/MySQL + 假 model/Qdrant 的全栈 HTTP E2E，覆盖角色变更、归属可见性、邀请创建→收件箱→预览→接受、只读/读写文档闸门、模板自建库、授权列表与撤销。
- [x] 前端引入 Vitest + jsdom（threads pool）：`api.test.ts` 固定 API 客户端契约（路径、请求头、CSRF 仅写请求、401 处理、错误格式、共享接口请求体与 null 归一化），`navigation.test.ts` 固定 hash 路由、邀请 token 解析与 namespace 规则；`npm test` 27/27 通过，typecheck/build 保持绿色。
- [x] 测试命令可靠化：`-p 1 -count=1 -timeout 25m` 写入 `scripts/build-go.ps1 -Test` 与 `scripts/build-go.sh test`，README/USAGE 同步；新增 `make test` / `make test-frontend` / `make check`。
- [x] 新增 `.github/workflows/test.yml`：MySQL 8 service + `go vet/build/test -p 1`，以及前端 typecheck/test/build 两条 CI。
- [x] 文档与验证记录：README、docs/USAGE.md（§10/§11）、VALIDATION.md 更新；全量 `go test ./... -p 1 -count=1 -timeout 25m` 全绿（约 16 分钟，e2e 205s、runtime 462s、state 400s）。

已知边界：仍无浏览器级 E2E（Playwright/Cypress），Console 的点击级交互继续靠前端契约测试与人工核对；CI 依赖 Go 1.27 工具链与 GitHub Actions MySQL service，本机 Windows 未实际执行 CI。

## 2026-11-10 API Key 归属隔离（管理凭证按用户隔离）

本轮让管理凭证（API Key）跟随 Console 账号归属，密钥的可见性与可绑定范围按账号隔离：

- [x] `api_keys` 增加 `owner_user_id` 列（老表补列迁移 + 索引）；创建密钥时记录调用账号，历史密钥保持 `NULL`（无主）。
- [x] 可见性与管理规则：管理员看到/管理全部密钥（含历史无主），VIP/普通用户只看到自己创建的密钥，触碰他人密钥返回 404 `API Key 不存在`；列表条目新增可选 `owner_user_id` / `owner_username`（`owner_username` 仅展示）。
- [x] 非管理员创建密钥只能绑定本人拥有的知识库（`knowledge_bases.owner_user_id`），绑定他人知识库返回 403 `只能为本人创建的知识库创建 API Key`；管理员可绑定任意知识库。
- [x] `/v1/namespaces` 增加附加字段 `owned_namespaces`（会话账号为本人拥有的；管理员为管理员自己拥有的；本机无会话等于全部）；五条管理路由路径保持 `/v1/admin/api-keys` 不变，鉴权改为「任意角色登录会话管理自己的密钥 + 无会话可信本机管理」双通道。
- [x] 删除账号时级联删除其拥有的 API Key；租户认证（`Authorization: Bearer` + `X-RAG-Namespace`）语义不变。
- [x] Console 与文档：API Key 页对所有登录角色开放（管理员显示「所有者」列、历史密钥显示「—（未绑定）」；非管理员显示隔离提示并只列出本人知识库）；同步更新 `docs/PERMISSIONS.md`（新增「API Key 归属隔离」）、`docs/api-keys.md`、`docs/USAGE.md`、README；新增前端 `apiKeys.test.ts` 契约测试，前端 typecheck、`npm test` 与生产构建通过。

验证与已知限制：

- [x] Go 回归测试：`internal/state/api_keys_owner_test.go`（归属投影、`ListByOwner` 作用域、历史 NULL 行、更新保留所有者、删除账号级联）、`internal/httpapi/api_keys_owner_test.go`（列表/读取/更新/删除/mcp-config 隔离、404 不可区分、CSRF/Bearer/本机策略、创建与更新绑定规则、no-store、管理员可见性）、`internal/e2e/api_keys_http_test.go`（真实路由 + 会话 + CSRF + auth/access/MySQL 端到端）。
- [x] `go build ./...`、`go vet ./...` 干净；全量 `go test ./... -p 1 -count=1 -timeout 25m` 全绿（state 421s、httpapi 183s、e2e 253s、runtime 465s，约 24 分钟）；前端 typecheck、32/32 vitest、生产构建通过。详见 VALIDATION.md。
- [x] 评审修复：更新密钥时只校验新增 Namespace（避免降级管理员无法改名/停用）；AccessControl 缺失时非管理员绑定规则 fail-closed；索引迁移先查 `information_schema` 避免每次启动 DDL；`ListByOwner` 空账号返回空；账号删除级联同时刷新 `api_keys` 读模型版本。
- 已知限制：禁用账号不会自动停用已签发密钥（需删除账号或手动停用/删除密钥）；密钥按 namespace 名称绑定，删除并重建同名知识库后旧密钥仍生效（删除/重建时应同步清理密钥）。
## 2026-11-10 模型配置页面与模型列表（Console）

本轮把 Console 里的 Embedding / Reranker / VLM 连接从「创建知识库」表单中拆出来，改为管理员独立维护、可复用的模型列表：

- [x] 新增侧边栏「模型配置」页（管理员可见，`#/model-profiles`）：Embedding / Reranker / VLM 三个页签，表格展示 名称 / 模型 / 服务地址 / Key / 向量维度（仅 Embedding）/ 更新时间 / 操作，支持「添加模型」抽屉、编辑和删除；空列表时提示先添加模型并可直接跳转。
- [x] 新增 MySQL 表 `model_profiles`（`id`、`kind`、`name`、`base_url`、`model`、`protected_key`、`vector_size`、`created_at`、`updated_at`，`(kind, name)` 唯一，`kind` 建索引）；API Key 用 `RAG_STATE_ENCRYPTION_KEY` 加密，列表与详情只返回 `api_key_set`，明文仅由本机/管理员会话的 `/key` 接口读取。
- [x] 新增 `/v1/admin/model-profiles` 管理接口：列表、创建、详情、PATCH、删除、`/{id}/key`、`/{id}/models`（共 7 条，OpenAPI 打上 `model-profiles` 标签），仅本机直连或管理员会话；创建时探测 provider（Embedding 真实调用一次并记录向量维度、Reranker 验证一次有界重排、VLM 不探测），PATCH 仅在服务地址/模型/Key 实际变化时重新探测，重名返回 409。
- [x] 创建知识库（管理员表单）改为快速选择已保存的 Embedding（必填）/ Reranker（开关，可选）/ VLM（开关，可选）模型；编辑抽屉（知识库列表 → 配置）同样选择模型，并为不在列表中的旧连接保留「当前配置」兜底选项，保存时不触发重新向量化。
- [x] 切换 Embedding 到不同连接仍走「重新向量化并保存」确认；编辑或删除模型配置不会修改已保存连接的知识库；VIP/普通用户建库表单保持不变，仍从 `RAG_DEFAULT_KB_TEMPLATE` 克隆模型配置。
- [x] 测试：新增 `internal/state`、`internal/kbconfig`、`internal/runtime`、`internal/httpapi` 四个包的 model profile 覆盖（仓储生命周期与重名冲突、表结构、草稿校验与 Key 保留/清除、探测路由、公开投影不泄露 Key、错误映射、HTTP 增删改查与 422/404/409/权限），前端新增 `ModelProfilesPage.test.tsx`、`CreateKnowledgeBase.test.ts` 并扩展 `api.test.ts`；完整结果见 VALIDATION.md。
- [x] 文档：README（Console 能力、主要功能、状态库）、docs/USAGE.md（新增「模型配置（模型列表）」与 7 个接口示例，VLM/知识库管理说明同步）、docs/knowledge-base-settings.md（模型配置与知识库连接的关系）、VALIDATION.md 更新。

已知边界：模型配置与知识库是保存时复制的引用关系（无外键），因此重命名/删除配置不会回填历史知识库；模型列表只服务 Console 管理员表单，知识库 REST 契约不变；保存探测会真实调用 provider（VLM 除外），离线环境无法新增 Embedding/Reranker 配置。

## 2026-11-10 PDF 解析错误信息（上传失败可读化）

本轮修复 PDF 上传失败时只显示 Go 类型名（`知识库操作失败：errorString`）的问题，并让缺失 Docling 环境的情况返回可操作的提示：

- [x] `internal/document` 新增 `PDFParseError`（消息只由本仓库代码生成，可安全展示）；外部解析进程失败不再返回裸 `fmt.Errorf`，避免被安全错误文本归类成 Go 类型名。
- [x] `execCommandRunner` 保留子进程 stderr 并识别 Python `ModuleNotFoundError: No module named '…'`，返回「PDF 解析不可用：Python 解析环境缺少模块 …；请安装 python-workers/requirements.txt 的依赖，并用 RAG_PDF_PARSER_PYTHON 与 PYTHONPATH 指向该环境」；stderr 尾部（≤2000 字节）只写入服务端日志。
- [x] `internal/runtime`：`safeUploadErrorText` 对 `*document.PDFParseError` 返回其安全消息；其余未知错误保持「按类别」输出，并把 Go 的 `errorString`/`wrapError` 映射成 Python 形状的 `RuntimeError`（不再泄露 Go 标识符）。上传失败时服务端日志新增 `Upload <id> failed category=… detail=…`（含截断后的错误消息，便于排查）。
- [x] 测试：`internal/document` 覆盖缺失 Python 模块的 stderr 分类与既有失败消息不变（`TestExternalPDFParserErrors/missing_python_module`、`TestMissingPythonModule`）；`internal/runtime` 覆盖 PDF 解析消息透出与 `RuntimeError` 兜底（`TestSafeUploadErrorTextPdfParseMessage`）。详见 VALIDATION.md。
- 已知限制：错误信息只解决「看不懂」。PDF 上传仍需要外部 Docling/PyTorch 解析环境（本机未安装，`python -m rag_mcp.pdf_worker` 报 `No module named 'rag_mcp'`）以及知识库配置的 VLM 端点；文本/Markdown 上传不受影响。

## 2026-11-10 PDF 解析改为 Go 内置引擎（Docling 改为可选）

本轮把 PDF 上传从「必须安装 Python/Docling」改为默认由 Go 进程内解析，消除用户上传 `2404.16130v2.pdf` 时 `ModuleNotFoundError: No module named 'rag_mcp'` 的根因，同时保留 Docling 作为可选高保真引擎：

- [x] `internal/document/pdf_native.go`：新增 `NativePDFParser`（实现 `ingest.PDFParser` 与 `document.PDFPages`），用纯 Go PDF 阅读器（`github.com/ledongthuc/pdf`）逐页读取内容流：按基线聚类行并合并上标/脚注标记、按字号与行距分段、识别标题、去除页眉页脚与页码、清理目录点引导符、归一化连字（ﬁ/ﬂ/ﬃ）、修复行尾连字符换行、CJK 不插多余空格；按「字宽是否可用」决定按 X 排序还是保留内容流顺序，避免中文 PDF/零宽度字形被排乱。输出仍为 `docling-formula-remote-v3` 清单契约（`parser.engine=go-native`，metrics 记录 pages/blocks/characters/elapsed_ms），切分、元数据与已存文档无需迁移。
- [x] `RAG_PDF_ENGINE`：`go`（默认）或 `docling`；未设置时若配置了 `RAG_PDF_PARSER_PYTHON` 仍走 docling（兼容既有部署），否则走 go。`internal/runtime` 上传路径按引擎注入 parser，`docling` 分支行为与之前完全一致。
- [x] 版面/表格/OCR/公式/图片描述仍是 Docling 独有能力；Go 引擎只提取文字，扫描件会返回「PDF 没有可索引的内容」或空页告警，不会静默入库空文本。README、`docs/USAGE.md`、`docs/DEPLOYMENT.md`、`docs/GO_MIGRATION.md`、`python-workers/README.md` 已按引擎拆分更新；`document.PDFPages` 注释不再声称「Docling is never reimplemented in Go」。
- [x] 测试：`internal/document/pdf_native_test.go`（程序生成的合法 PDF：清单/块顺序/Pages/损坏与空文本错误、连字/页码/连字符/CJK/标点空格启发式）、`internal/config/pdf_engine_test.go`（引擎解析与默认值）、`internal/runtime` 上传引擎选择测试；`go build ./...`、`go vet` 与相关包测试通过。
- 实机验证：重启 8011 服务后，用默认 Go 引擎通过 `POST /v1/documents` 上传 arXiv `2404.16130v2.pdf`（6.89 MiB）→ 200、35 个 chunk 入库；保存的 manifest 为 26 页 / 349 blocks / 89856 字符 / 解析 2.08s，`parser.engine=go-native`；UploadJournal 步骤全部 DONE、操作 COMMITTED；检索命中该 PDF 正文。详见 VALIDATION.md。
- 已知限制：Go 引擎不含版面分析（双栏 PDF 可能列间串行）、表格结构、图片/公式资产与 OCR；需要这些能力时设置 `RAG_PDF_ENGINE=docling` 并安装 python-workers 依赖。另：旧调试上传 `2c06005970…`（namespace `debug222146`）仍每 10 秒打印 `recovery failed (ValueError); retry pending`，与本改动无关，待清理。

## 2026-11-10 解析 worker 全部改为 Go（删除 Python/Docling 兼容层）

按「不再兼容老项目、全部用 Go 实现」的要求，删除了上一节保留的 Docling 回退路径和残留 Python worker：

- [x] 删除 PDF 外部引擎：`document.ExternalPDFParser`、`CommandRunner`/`execCommandRunner`、缺失 Python 模块诊断，以及 `RAG_PDF_ENGINE`、`RAG_PDF_PARSER_PYTHON`、`RAG_PDF_MODELS_PATH`、`RAG_PDF_PARSE_TIMEOUT` 配置、启动模板、`deploy/config.container.toml` 与 `.env.example` 条目；`ingest.PDFOptions` 只保留可选的 `Parser`（默认 `NativePDFParser`），运行时不启动任何解析子进程。新导入的清单版本改为 `go-native-v1`；旧清单不校验版本，所以已存文档仍可读取。
- [x] Word 视觉 worker 从 Python 迁到 Go：`document.WordVisionWorker` 在进程内解码图片（png/jpeg/gif/bmp/tiff，缩放到最长边 384 后转 PNG），直接调用知识库的 OpenAI 兼容 `/chat/completions`（图片 data URL + 固定描述 prompt、max_tokens 256、429/5xx 重试 3 次、拒绝/空回答过滤、`<end_of_utterance>` 清理与句末标点）；未配置 VLM 时图片保留占位描述、上传不失败。`internal/runtime` 的 `wordVisionWorker` 不再传 Python/模型路径。
- [x] 删除 `python-workers/`（Docling PDF worker、Word 视觉 worker 及其 Python 包）与 `benchmarks/`（Docling 评估脚本）；README、`docs/USAGE.md`、`docs/DEPLOYMENT.md`、`docs/GO_MIGRATION.md`、`.env.example`、启动配置模板同步改为 Go-only。`scripts/verify-*.py` 是可选运维测试脚本，不属于运行时。
- [x] 测试：新增 `internal/document/vision_test.go`（httptest VLM 成功路径/图片缩放/鉴权/prompt、跳过有图注图片、未配置端点/模型、拒绝回答、503 重试、HTTP 400、超时、坏图片、清单缺失），ingest 集成测试改用 httptest VLM 与真实 JPEG fixture；删除 `ExternalPDFParser`/引擎选择测试，更新 `PDFParserVersion` 断言。`go build ./...`、`go vet ./...` 干净，`internal/document`、`internal/ingest`、`internal/config`、`pdfcontent`、`chunking` 与 runtime 上传/PDF/Word 子集测试通过。
- 实机验证（新二进制重启 8011）：上传中文 PDF `it-manual-cn.pdf` → 200、9 chunk、`pdf_parser_version=go-native-v1`、`failed: []`；删除该文档成功（404 + 列表消失）；重复上传 `2404.16130v2.pdf` 重新入库为 `go-native-v1`（35 chunks），manifest 存放在 `pdf-assets`，UploadJournal `COMMITTED`、`error NULL`；Console 200。合并 main 的默认异步上传后，同一 PDF 走默认异步链路也验证通过：`mode:"async"`、job 状态 `queued → parsing → chunking → staging → embedding → indexing → publishing → succeeded`，删除成功；合并后的 runtime 上传/PDF/Word/Async 测试与 `go build`/`go vet` 全绿。详见 VALIDATION.md。
- 已知限制：PDF 不支持版面分析、表格结构、OCR、公式/图片资产（双栏可能串行），扫描件需先 OCR；Word 图片描述依赖远端 VLM，仅用 httptest 验证；`.doc` 转换仍需要外部 LibreOffice。旧调试上传 `2c06005970…` 的 recovery 日志仍未清理。

## 2026-11-10 异步上传默认化与内嵌文档 worker

本轮把文档上传的默认路径改为异步受理，并让无 Kafka 的单进程部署无需再单独运行文档 worker：

- [x] 默认值翻转：`RAG_UPLOAD_MODE` 缺省为 `async`（`internal/config` 的 `DefaultUploadMode` 与注释、`internal/httpapi` 的 OpenAPI 摘要同步更新）；`POST /v1/documents` 每文件在一个 MySQL 事务内写入 `document_jobs` + `document_job_outbox` 后立即返回 `{"mode":"async","jobs":[{"name","artifact_id","status","reused"}],"failed":[{"name","error"}]}`，解析/切分/Embedding/Qdrant 发布由文档 worker 后台完成。`sync` 保留为显式 opt-in，响应仍是请求内完成的 `imported`/`skipped`/`failed`/`documents` + `mode:"sync"`。
- [x] 内嵌文档 worker：`rag-service` 在无 Kafka 时进程内消费已提交的 outbox 行；新增 `RAG_DOCUMENT_WORKER_INPROCESS`（未设置/`auto` = 仅未配置 `RAG_KAFKA_BOOTSTRAP_SERVERS` 时启用；`1/true/on` = 强制启用，此时配置 Kafka 会拒绝启动；`0/false/off` = 关闭并改跑独立 worker）与轮询间隔 `RAG_DOCUMENT_WORKER_POLL_SECONDS`（默认 `0.5` 秒）。
- [x] 接收器接线复用：`documentworker.WithOutboxReceiver` 让 worker 用 OutboxReceiver 直接排空已提交的 PENDING `document_job_outbox` 行（broker 模式仍消费 Kafka 消息），`rag-service` 的内嵌 worker、`cmd/rag-document-worker` 的本地 outbox 模式与 e2e 共用该接线；Kafka 部署仍由 `rag-document-worker` 消费同一 consumer group。`scripts/start-stack.ps1` 现在与 `rag-service` 一起启动 `dist\bin\rag-document-worker.exe`，退出时一并停止。
- [x] Console：上传受理后按 排队中/处理中/已完成/失败 展示每个文件的 `GET /v1/document-jobs/{artifact_id}` 轮询状态，并显示「已受理 N 个文件，正在后台处理」；异步轮询逻辑原本已存在，本轮成为默认路径，上传对话框不再等待整个解析过程。
- [x] 文档：更新 `.env.example`、`README.md`、`docs/USAGE.md`、`docs/DEPLOYMENT.md`，说明默认异步、内嵌 worker 的三种取值、Kafka 部署仍需 worker，并加入升级提示（默认异步必须有消费者：单进程内嵌、Kafka worker，或显式 `RAG_UPLOAD_MODE=sync`）。
- [x] 测试：默认值/响应契约/内嵌 worker 接线与 REST 异步上传链路由定向与全量 Go 测试套件覆盖；实际结果由 Lead 复核后记录在 `VALIDATION.md`，本节不预写通过结论。

已知边界：`update_document`/`delete_document` 仍同步执行；MCP `add_document` 始终异步；关闭内嵌 worker 的无 Kafka 部署必须自行运行 `RAG_DOCUMENT_WORKER_LOCAL=1` 的独立文档 worker，否则任务停在 `queued`。

## 2026-11-10 恢复 Docling PDF 解析为独立微服务

本轮把上一轮删除的 Docling 解析以**独立 HTTP 微服务**形式恢复：Go 主服务/镜像仍无 Python 依赖，PDF 默认走 docling 引擎，需要时可切回 Go 内置纯文本引擎。

- [x] 服务：`python-workers/pdf-service/`（FastAPI + `docling==2.126.0` + CodeFormulaV2，CPU torch）；`GET /healthz`、`POST /v1/parse`（multipart 字段 `file` → `application/zip`，含 `manifest.json` 与 `pictures/*` 资产），清单版本 `docling-service-v1`；Docling 惰性导入，`RAG_PDF_SERVICE_CONVERTER=fake` 仅用于测试/联调。
- [x] Go 客户端：`internal/document/docling_service.go`（`DoclingServiceParser` 实现 `ingest.PDFParser` 与 `document.PDFPages`：multipart 调用、状态码/content-type 校验、拒绝路径穿越/符号链接/重复条目的安全解压、条目与解压大小上限、失败返回 `*PDFParseError` 安全消息）；新增 `PDFParserVersionDocling`、`IsSupportedPDFParserVersion`、`PDFParserVersionForEngine`。
- [x] 配置：`RAG_PDF_ENGINE`（`go|docling|空`，空 = 有 URL 则 docling 否则 go）、`RAG_PDF_SERVICE_URL`（宿主默认 `http://127.0.0.1:8091`）、`RAG_PDF_SERVICE_TIMEOUT`（默认 900，1..7200 秒）；服务端 `RAG_PDF_MODELS_PATH`、`RAG_PDF_SERVICE_CONCURRENCY`、`RAG_PDF_SERVICE_PORT`，VLM 复用现有 `RAG_VLM_*`。
- [x] 运行时接线（由 Lead 完成）：上传路径按引擎注入 `DoclingServiceParser`，PDF 元数据记录实际清单版本。
- [x] 部署：compose 新增默认启动的 `pdf-parser`（独立镜像 `praxis-rag-pdf-parser:local`、`pdf_parser_models` 卷、`127.0.0.1:8091`、`/healthz` 健康检查），`app` 增加 `depends_on` 与三个 `RAG_PDF_*` 默认值（compose 内默认 `http://pdf-parser:8091`）；`.env.example` 增补 PDF 引擎、模型卷与 VLM 共用说明。
- [x] 文档：README、`docs/USAGE.md`、`docs/DEPLOYMENT.md`（新增 §3.5 pdf-parser 部署章节）、`docs/GO_MIGRATION.md` 同步为「默认 docling + 可切回 go」；修正此前「PDF 解析不需要 Python/Docling」「仓库不再包含 Python 代码」等已过期表述。
- 已知限制：docling 引擎需要 pdf-parser 服务与已预置的 Docling/CodeFormula 模型（CPU torch 镜像较大，首次使用前需联网下载模型到卷）；pdf-parser 不可用或超时时 docling 上传会失败（错误安全可读），显式 `RAG_PDF_ENGINE=go` 可回退纯文本；公式识别依赖 `RAG_VLM_*` 远端 VLM 或本地 CodeFormula 模型，两者都缺失时 `/v1/parse` 返回 503、解析失败；版面/表格/OCR 只有 Docling 提供；pdf-parser 无认证，只发布 loopback。真实 Docling 推理、镜像构建与模型下载尚未在本环境实机验证（联调使用 fake converter），上线前需在预发环境验证。

## 2026-11-10 创建知识库页面改为多列布局（Console）

本轮把 Console「创建知识库」页面从纵向单列表单改为横向多列布局，减少右侧空白、一屏内完成配置：

- [x] 管理员创建表单：知识库标识（Namespace）半宽（≤460px）；Embedding / Reranker / VLM 三张模型卡片并排成三列（≥1180px 三列，768–1179px 两列，≤767px 单列），模型区顶部共用一条分隔线；文本切分（切分方式 / 块长度 / 重叠长度）排成一行（≥1024px 三列，768–1023px 两列，≤767px 单列）。表单宽度由 720px 放宽到 1080px。
- [x] VIP / 普通用户创建表单：知识库标识、切分方式、块长度、重叠长度按 2×2 两列排布（≤767px 单列）。
- [x] 编辑知识库抽屉（知识库列表 → 配置，720px 宽）保持原有纵向布局，避免在窄抽屉里挤压模型下拉框；表单字段、校验与提交逻辑均未改，仅把模型卡片与切分字段包进 grid 容器。
- [x] 验证：`npm run typecheck` 通过；`npm test` 54/54；`npm run build` 通过；无头 Chrome 在 1440 / 1024 / 390 宽度截图核对管理员三列/两列/单列、租户两列表单与编辑抽屉布局（截图保存在未入库的 `.test-tmp/create-kb-layout/`）。详见 VALIDATION.md。
- 已知边界：多列布局只作用于「创建知识库」页面；编辑抽屉仍为单列；仓库无 Playwright/Cypress，布局用无头 Chrome 截图人工核对，没有自动化浏览器回归。

## 2026-11-11 Console OCR 模型配置与 Docling 按知识库下发 VLM+OCR

本轮把 Docling 的 OCR 与 VLM 从服务级环境变量改为**按知识库的用户配置**：Console「模型配置」新增 OCR 类型，知识库设置新增 `ocr_model`，上传时 Go 服务把 VLM/OCR 随解析请求下发给 pdf-parser；缺失配置在受理阶段提示用户去配置。

- [x] 模型配置：新增 `kind=ocr`（与 embedding/reranker/vlm 并列），条目只保存 OCR 引擎 `auto|easyocr|tesseract|rapidocr`（默认 easyocr），无服务地址/API Key/探测；`DraftModelProfile`/`ProbeModelProfile`/HTTP 校验/openapi kind 枚举同步；不做 DB schema 迁移。
- [x] 知识库设置：创建/更新 API 接受 `ocr_model`（更新缺省保留、空串清除），存入 KB settings 并出现在知识库公开投影；`RAG_DEFAULT_KB_TEMPLATE` 克隆保留 OCR 引擎。
- [x] 运行时：`uploadResources` 解析 VLM+OCR；docling 引擎下 PDF 上传在受理阶段（同步 `Upload` 与异步 `AdmitDocument`）校验 VLM 已配置且 model 非空（KB 优先，`RAG_VLM_*` 回退）+ OCR 已配置，缺失返回中文 `KnowledgeValidationError` 且不写 journal/`document_jobs`；`DoclingServiceParser` 新增 `VLM`/`OCR` 字段，multipart 新增 `options`（`{"vlm":{"base_url","api_key","model","timeout","concurrency"},"ocr":{"engine"}}`，api_key 不写日志），timeout/concurrency 取服务级 `RAG_VLM_*`。
- [x] Python 服务：`/v1/parse` 接受可选 `options` 文本字段，按请求覆盖 VLM 与 OCR 引擎（`easyocr`/`auto`→EasyOCR 默认 `ch_sim,en`、`tesseract`→`chi_sim,eng`、`rapidocr`→RapidOCR），非法 options 返回 422 安全消息，`manifest.parser.ocr_engine` 记录实际引擎；无 options 时保持环境变量行为。
- [x] Console：模型配置页新增 OCR 页签（只显示「OCR 引擎」下拉、隐藏地址/Key/探测，表格按类型调整列）；创建/编辑知识库表单新增 OCR 卡片（启用开关 + 必选，无 OCR 模型时引导去「模型配置」），`ocr_model` 随创建/更新提交并在编辑时回填；创建页模型区改为 2×2。
- [x] 测试：Go `go build ./...`/`go vet ./...` 干净，`go test -p 1 ./internal/...` 全绿（`RAG_TEST_MYSQL_URL=praxis_gotest`）；Python `pytest` 140 passed；Console `npx vitest run` 63 passed + `npm run typecheck`；Go 客户端→fake pdf-parser 真实 HTTP 的 `options` E2E（带配置 200、非法 options 422）通过。详见 `VALIDATION.md`。
- 已知边界：真实 Docling pipeline 仍未在本机运行（未装 docling/torch，tesseract 需要镜像/系统语言包，`auto` 固定映射 EasyOCR）；历史知识库没有 `ocr_model`，在 docling 引擎下需先在 Console 补配 OCR 才能上传 PDF；校验只在受理路径，重新解析既有文档仍走原队列路径；OCR 引擎/语言未逐知识库开放语言列表配置（当前按引擎默认语言）。
