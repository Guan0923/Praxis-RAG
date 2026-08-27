# RAG MCP 开发进展

最后更新：2026-08-26

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
