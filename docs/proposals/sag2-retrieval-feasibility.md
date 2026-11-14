# SAG2（BM25 + JEV 查询期关联检索）可行性评估与落地方案

> 状态：**Phase 0 + Phase 1 已实现**（2026-11-12）——`retrieval: sag2` 第四种检索模式已落地（云端 JEV 客户端 + 进程内词法索引 + 三轮状态机 + DPP），Phase 2（MySQL 持久化倒排）未实现；本文的评估、方案对比与风险清单仍有效，实现差异见各源文件头部注释与 `docs/USAGE.md` 的 SAG2 小节。
> 评估对象：[SAG2: Embedding-Free Multi-Hop Retrieval with BM25 and JEV（arXiv:2610.22588）](https://arxiv.org/abs/2610.22588)
> 参考实现：Zleap-AI/SAG-Benchmark 的 `sag2/scripts/sag2.py`、PyPI `zleap-sag2 0.4.0`、SaturnAura/OpenJev-Sag2
> 结论时间：2026-11-12

## 0. 结论速览

**可以实现，但需要先纠正两个概念，并接受一个前提。**

1. **SAG2 不是"图检索"**。它不建文档图/邻居图，不抽实体，也不存图数据库；它在**查询期**用 `段落 → 词项 → 段落` 的词法关联做多轮扩展，JEV 只负责判断"这个候选段落是否为回答链条中的必要证据"。论文只在 Related Work 里把 GAR（预建邻居图）列为对照。
2. **"倒排查找"（inverted index）确实是核心**：BM25 用倒排索引做初筛与扩展召回；JEV 不是倒排的一部分，而是一个外部决策模型（TypeSafe System One，输出 `noul ∈ [0,1]`）。
3. **前提**：SAG2 的收益几乎全部来自 JEV。论文消融显示去掉 JEV 后，词项扩展相对 BM25 几乎没有增益（RM3 甚至更差）；所以"只做 BM25 + 扩展、不上 JEV"没有落地价值。JEV 是外部/自托管模型调用，会带来数据外发、延迟、成本与可用性问题。

**建议：不替换现有 `bm25`/`embedding`/`hybrid`，按三步走新增一个默认关闭的可选模式。**

| 阶段 | 内容 | 预估 | 目的 |
| --- | --- | --- | --- |
| Phase 0 | JEV 客户端 + 知识库级 JEV 配置 + 新增 `rerank: jev`（只重排现有候选） | 5–8 人日 | 用最低成本验证"JEV 在本项目语料上是否值得用" |
| Phase 1 | 新增 `retrieval: sag2`（SAG2-lite：词法初筛 + JEV 两轮扩展 + DPP，进程内倒排） | 3–5 人周 | 验证多轮关联检索相对 hybrid 的召回增益 |
| Phase 2 | MySQL 持久化倒排索引（生产级一致性/删除/回填/缓存/可观测） | 4–6 人周 | 支撑大规模知识库与多实例部署 |

如果只想要一个"更懂多跳关联"的检索能力，**Phase 0 + Phase 1 足够验证价值**；Phase 2 只在效果被真实语料证明后再投入。不建议把 Python 版 `zleap-sag2` 作为 sidecar 接入主路径（违反"Go 不依赖 Python"的硬约束，且会引入第二套状态与可见性）。

## 1. SAG2 到底做什么

简化后的流程（与参考实现逐行核对过）：

```text
初始轮：
  query 词项（≤80 个）OR 组合 → BM25（title/heading/body 字段权重 2/2/1）→ 前 B=100 段
  JEV(Q, 候选, 空上下文) → 每个候选一个必要证据分 p∈[0,1]
  取分数最高的 s=10 个"未扩展"段落作为 expansion seeds

扩展轮（默认 2 轮，共 3 次评分）：
  从 seeds 的句子中选扩展词（≤L=20）：
    优先级 = (1+ln tf) × I(t) × (1+g_q(t)) × w(seed)
    I(t)=ln(1+(N-df+0.5)/(df+0.5))       # 语料稀有度
    g_q(t)=包含 t 的句子里与原始 query 共享词项的 I 之和（取最大）
    w(seed)=0.5+0.5·p(seed)/max(p)        # 低分 seed 降权但不丢弃
  每个扩展词单独检索"未见过的段落"（必须命中该词），按词优先级交织 → 新候选 ≤100
  合并：首轮 top5 + 已扩展 seeds + 新候选；最后一轮可回炉历史候选（新候选 ≤20）
  JEV(Q, 合并池, 当前证据上下文) → 重新评分；末轮上下文带上轮 top10 与旧分

终排：
  按 JEV 分排序取 top10，再做 DPP 多样性重排（只动前 10）
```

关键事实：

- **索引侧**只有段落原文和词法统计（无 embedding、无实体抽取、无离线模型推理）；MuSiQue 的 11,656 段建库 2.36 秒、25.87 MiB。
- **JEV 是 `[0,1]` 打分器**，不是生成式 LLM；请求里放"问题 + 候选段落 + 已检索证据"，响应直接给概率。论文与参考实现都走托管接口（TypeSafe `.../v1/systemone`，或 OpenRouter 的 decisions 路径）。
- **指标**：论文在 MuSiQue / 2WikiMultiHopQA / HotpotQA 的 1,000 题子集上 R@10 分别为 86.62% / 97.43% / 99.00%，EnterpriseRAG-Bench（511,962 文档）文档级 R@10 81.10%。
- **代价**：每题 3 轮 JEV、最多约 340 次候选判定；托管 JEV 单请求 p50 约 0.19–0.53s（第三方实测），论文成本约 $0.0043/题（$0.042/M 输入 token）。本地 9B 决策头在 RTX 3090 上每题 6.5–19s，只能离线评测，不能在线用。
- **局限**：候选必须命中问句词项，语义/同义改写类问题的候选池天然受限（论文 Enterprise 数据里语义类召回 58.40%）。这正是本项目已有 dense/hybrid 的价值所在。

## 2. 本项目现状盘点

### 2.1 检索与存储

- 三种模式：`bm25` / `embedding` / `hybrid`，声明于 `internal/contracts/contracts.go:19-22`，参数校验在 `internal/contracts/contracts.go:155-161`，MCP schema 在 `internal/contracts/contracts.go:410-412`。
- 每个知识库在请求时构造自己的检索栈：`internal/runtime/tenant.go:66-139`（embedding/reranker 连接 → Qdrant store → Orchestrator，`internal/runtime/tenant.go:117-130`）。
- `bm25` 不是 MySQL 倒排：它是 Qdrant 的 sparse named vector。词项来自 `internal/sparse` 的**确定性哈希词表**（BLAKE2s 4 字节，`internal/sparse/sparse.go:232-235`），中文按**单字 + 相邻二元**切分（`internal/sparse/sparse.go:183-218`），IDF 由 Qdrant 集合级 modifier 提供，客户端**拿不到 df**。
- chunk 正文 / 标题 / 来源 / metadata 在 Qdrant payload（`internal/qdrant/store.go:346-355`）；`documents` 表只有 `chunk_count` 等清单字段（`internal/state/schema.go:304-321`），`docs/state-storage.md:20` 也写明 "Chunks, dense/sparse vectors → Qdrant"。
- **重要意外发现**：`stage_mysql` 步骤会把 `{chunk_id, text, metadata}` 写入 MySQL 的 `upload_objects`（bucket=`index`，key=`chunks.json`，`internal/runtime/documents_upload.go:805-824`）；发布后不删，`Cleanup` 只删除**旧版本**对象（`internal/state/uploads.go:1241-1318`，DELETE 在 `:1296`）。也就是说**当前 head 版本的全部 chunk 正文与 page/heading 元数据长期留在 MySQL**，可以作为词法索引的回填来源，不需要复制第二份正文。
- 可见性权威是 `document_heads` + `VisibleVersions`（`internal/state/repositories.go:1155-1180`），Qdrant 查询按 namespace + 活跃 `upload_version` 过滤（`internal/qdrant/filters.go:77-105`）。删除是逻辑删除（`docs/state-storage.md`），但文档/命名空间清理路径不止一处（`internal/state/uploads.go:1074/1156/1296/1399`），新增词法表必须同步这些清理点。

### 2.2 JEV 现状：有配置缝，没有检索链路

- `model_profiles` 表已支持 `kind=jev`（`internal/httpapi/model_profiles.go:241`、`internal/state/schema.go:681`），管理员 Console「模型配置」可保存 base_url / model / 加密 Key，且明确不探测、无 `/models` 列表。
- 但 `jev` **不是**知识库级模型：`internal/kbconfig/connections.go:24` 的 `Kinds` 含 jev，而 `CredentialKinds`（`:30`）只有 embedding/reranker/vlm；`docs/knowledge-base-settings.md:49-54` 写明它是 "Console-only configuration ... kept for later decision-model use"。
- 检索链路没有任何 JEV 调用；`internal/modelapi` 只有 OpenAI 风格的 embedding/reranker 客户端，没有决策接口。

### 2.3 契约与界面

- MCP `search` 工具 schema、REST `/v1/search`（`internal/httpapi/search_body.go` 透传到同一个校验）、OpenAPI、Console 检索调试页（`frontend/src/api.ts:195` 的 `Retrieval` union、`:308` 的 `retrievalLabels`、`SearchPage.tsx:139` 的三模式对比循环）、结果快照与排队搜索都围绕三种模式编写。
- 默认 `retrieval=hybrid`、`rerank=cross_encoder`；`SearchRequestFromArguments` 只接受三个枚举值，其他值返回校验错误。

## 3. 差距分析

| SAG2 需要 | 本项目现状 | 缺口与处理 |
| --- | --- | --- |
| 词项倒排、df、N/avgdl | Qdrant sparse 只返回 top-k 分数；词项是单向哈希，无法还原词形 | **必须新建 Go 侧倒排索引**（MySQL 表或进程内），df/长度/字段 tf 自存 |
| 字段级 tf（title/heading/body） | sparse 文档向量内置 TF，但不暴露、无字段 | 索引侧按字段分别存 tf |
| 位置（短语匹配） | 无 | MVP 可先不做；中文用 bigram 近似，后续再加 positions |
| 句子级统计（扩展词权重） | chunk 文本可取（Qdrant payload / MySQL chunks.json） | 查询时只对 ≤10 个 seeds 现算，无需全量存句子 |
| 按单个词项召回"未见段落" | Qdrant 只能整向量查询，无按词项过滤/反查 | 用"单词项 sparse 查询 + 本地去重"或倒排表 prefix 查询 |
| 排除已见候选 | 现有 `Filter` 不支持 has_id | 多取候选后本地过滤，或扩展 Qdrant Filter |
| JEV 决策客户端 | 无；只有配置存档 | 新增 `internal/modelapi/jev.go`（请求/响应/批处理/重试/脱敏） |
| 多轮状态机 + 预算 | 无 | 新增 ~400–600 LOC（hop 循环、池组装、seen、末轮历史替换） |
| DPP 多样性重排 | 无 | 新增 ~150 LOC 纯 Go |
| 知识库级 JEV 配置 | 只有管理员级 kind=jev 存档 | KB settings 增加可选 `jev`/`jev_model`，两处公开投影同步 |
| 契约/Console | 三模式写死 | 新增枚举值是**可加性契约变更**：MCP schema、REST 文档、前端 union/labels、测试 |

## 4. 方案对比

### A. 完整原生 SAG2（MySQL 持久倒排）—— 生产目标

新增词法表（命名空间前缀，参考 `zleap-sag2 0.3` 的 `sag2_lex_postings`）：

- `kb_lex_chunks`：`(namespace, chunk_id)` → `document_id`、`chunk_len`、字段长度、`text` 可选（若不想回读 Qdrant）；
- `kb_lex_postings`：`(namespace, term_key, chunk_id, field)` → `tf`、`positions`（可选）；
- `kb_lex_meta`：`N`、`avgdl`、词表统计。

写入与发布同生命周期：worker 在 `indexing` 阶段内存构建倒排，`publish` 事务内替换该文档旧行（或先按版本 stage、发布后激活）；清理复用现有 `uploads.go` 的四个删除点。可见性仍以 `document_heads`/`VisibleVersions` 为准，不在词法表里造第二套可见性。

- 优点：精确 BM25、df、位置、短语、可扩展；不依赖 Qdrant 能力；与 MySQL-only 约束一致。
- 缺点：写放大（1 万 chunk 量级可能到百万级 posting 行）、事务/清理/回填复杂、需要迁移与运维；实现量最大。

### B. SAG2-lite（MySQL chunks.json 回填 + 进程内倒排）—— 推荐用于 Phase 1

从 `upload_objects(index/chunks.json)` 读取 head 版本 chunk 文本（或对 legacy 文档用 Qdrant scroll 兜底），在 tenant 级缓存中构建倒排与 df；发布已 bump 的 revision 作为失效信号（跨实例轮询）。召回仍可复用 Qdrant sparse 做初筛/单词项召回，扩展词权重用本地 df 计算。

- 优点：零表结构变更、可快速验证、与现有发布/版本链路不冲突。
- 缺点：进程内索引占内存，KB 大时冷启动/重建慢；多实例重复构建；不是长期方案。

### C. 只加 JEV 重排（`rerank: jev`）—— 成本最低的验证

在现有 `hybrid`/`bm25` 召回后，把前 N（≤30）个候选交给 JEV 打分重排（state=query，questions=候选）。复用 `Reranker` 接口与 `RerankMode` 注册表，不需要倒排索引。

- 优点：5–8 人日；不触碰存储；能直接回答"JEV 在本项目语料上是否比 cross_encoder 好"。
- 缺点：只改善排序、不改善候选发现（多跳需要的正是候选发现），预期收益小于完整 SAG2。

### D. Python sidecar（`zleap-sag2` 独立服务）—— 不推荐

最快获得功能完整度，但违反 AGENTS.md "除可选 pdf-service 外无 Python 依赖"，并引入第二套状态库/可见性/运维面；只适合做离线评测对照，不应进入运行时主路径。

**推荐组合：A 为目标、B 为过渡、C 为前置验证。** 如果 Phase 0 的实测显示 JEV 重排没有超过 cross_encoder，就不值得继续投入 Phase 1/2。

## 5. 分阶段落地

### Phase 0：JEV 客户端 + `rerank: jev`（5–8 人日）

1. `internal/modelapi/jev.go`：`POST {base_url}/systemone`，Bearer Key；请求 `{"model","state","questions"}`（`type=noul`，`criteria.{true,false}`）；响应 `answers[key].noul`；批处理（单请求候选数按 token 预算切分）、超时、429/5xx 重试、`ModelAPIError` 状态映射、日志脱敏。
2. 知识库级 JEV 配置：KB settings 增加可选 `jev{base_url, protected_key}` + `jev_model`；更新 `CredentialKinds`、`kbconfig/public.go`、`state/read_views.go` 两份投影（易漏）、KB 创建/编辑/模板克隆、`kb_admin` 的 key/models 路由、Console 创建页。无 MySQL 迁移（`knowledge_bases.settings` 是 JSON）。
3. `internal/retrieval/reranker.go` 增加 `JevReranker`；`contracts.RerankMode` 增 `jev`；MCP schema/Console 增选项；未配置 JEV 时返回 `RagOperationError`。
4. 测试与文档：`httptest` 覆盖请求/响应/错误；前端契约测试；`docs/agents/features.md`、`docs/USAGE.md`、README。

验收：在真实知识库上对比 `rerank=none|cross_encoder|jev` 的 nDCG@10 / Recall@5、延迟 p50/p95、单次成本；JEV 不可用时明确报错且不影响其他模式。

### Phase 1：`retrieval: sag2`（SAG2-lite，3–5 人周）

1. 新增 `internal/lexindex`：基于 `chunks.json`（回填）+ 发布 revision 失效的进程内倒排；字段 tf、df、N/avgdl、句子边界。
2. 新增 `internal/retrieval/sag2.go`：三轮回合、seeds、扩展词公式、交织去重、末轮历史、DPP；`candidate_k`→B、`top_k`→输出上限；硬超时与失败降级（JEV 不可用 → 回退 hybrid 或返回明确错误，二选一需在提案评审时定）。
3. 契约：`RetrievalSAG2="sag2"`（`contracts.go`）、校验文案、MCP schema enum、OpenAPI 描述、前端 union/labels、Console 默认不参与三模式对比；默认 `rerank=none`（避免 SAG2 后再跑 cross-encoder 双倍延迟），显式传 `cross_encoder` 时才叠加。
4. 灰度：默认关闭；KB 配好 JEV 后才可选；`debug.sag2{rounds, decisions, decision_ms, cache_hit}` 以可加性字段返回。
5. 评测：构造 50–100 条真实 KB 查询（含人工相关性标注），对比 `bm25`/`embedding`/`hybrid`/`hybrid+cross_encoder`/`sag2` 的 Recall@5/10、nDCG@10、延迟、成本。

验收：sag2 在多跳/关联型问题上优于 hybrid，且单跳/语义型问题不明显退化；否则只保留 Phase 0。

### Phase 2：持久化倒排（4–6 人周，效果达标后再做）

1. 新增 `kb_lex_chunks`/`kb_lex_postings`/`kb_lex_meta` 与 schema 迁移、回填 CLI（从 head 版 `chunks.json` 重建，legacy 文档走 Qdrant scroll 兜底）。
2. 发布事务内替换/清理；同步 `uploads.go` 的四个删除点；多实例缓存失效；`maintain` 类后台任务回收退役版本。
3. 位置/短语、字段权重、df 精确化；查询并发、限流、熔断、指标（决策次数、token、缓存命中、降级次数）。
4. 文档：`docs/agents/architecture.md`、`features.md`、`docs/USAGE.md`、`README.md`，以及 `VALIDATION.md` 实机记录。

## 6. 关键设计决定（需评审拍板）

1. **JEV endpoint 约定**：管理员保存的是完整决策 URL（如 `https://api.typesafe.ai/v1/systemone`），还是 `base_url` + 固定 `/systemone`？建议前者，避免厂商路径变化时改代码；文档明确"不再追加路径"。
2. **KB 级 vs 全局 JEV**：推荐 KB 级（与 embedding/reranker/vlm/ocr 一致、可隔离、可关闭）；全局方案改动小但无法按 KB 治理。
3. **默认降级**：`sag2` 在 JEV 失败时回退 hybrid（可用性优先）还是直接报错（可解释性优先）？推荐"本轮内重试 1 次后回退 hybrid，并在 `debug` 标注降级"。
4. **数据外发**：查询与候选正文会发给第三方决策模型；必须由管理员显式配置、按 KB 开关、文档声明，并保持"日志不记录 query/正文"约束。
5. **token 估算**：参考实现用 o200k BPE，Go 侧没有现成编码器；建议按字符数近似 + 安全系数（中文约 1.5 字符/token、英文约 4 字符/token），并留 20% 余量，超限拒绝而不是截断。
6. **超时预算**：MCP 排队搜索默认 120s（`internal/state/search_tasks.go:58`），同步 MCP `wait_seconds ≤30`（`docs/agents/features.md`）；SAG2 需自带单轮 deadline（建议总预算 8–15s），不能依赖外层超时兜底。

## 7. 风险清单

| 风险 | 影响 | 缓解 |
| --- | --- | --- |
| SAG2 只在英文多跳基准上有优势 | 中文企业 KB 收益不确定 | Phase 0 先用 JEV 重排做小样本验证；Phase 1 再评测 |
| 候选必须命中问句词项 | 语义/同义问题召回差（论文语义类 58.40%） | 保留 hybrid 并作为默认；sag2 只作为补充 |
| 数据出境 | 合规/安全 | KB 级显式开关、管理员配置、文档声明、可切换自托管 |
| 延迟与成本 | 每题 3 轮、最多约 340 次判定；托管 p50 0.2–0.5s/请求 | 硬预算、批处理、并发受限、熔断、回退 |
| JEV 不可用 | sag2 失败 | 轮内重试 + 回退 hybrid + debug 标注 |
| 词法索引一致性 | 新增删除/回填路径遗漏 | 复用可见性权威；把清理点写进迁移清单与测试 |
| 写放大/存储 | 百万级 posting 行 | Phase 2 才落表；先 B 方案验证 |
| 契约/前端遗漏 | MCP/Console 不接受新值 | 可加性变更 + 契约测试 + 双份投影检查 |
| 缓存跨实例失效 | 新文档检索不到 | 复用发布 revision；TTL 兜底；Phase 2 改 MySQL |

## 8. 未验证项

- TypeSafe System One 的真实响应细节（`answers` 键集、错误码、并发限制）与 `stateful` 载荷的 JSON 形状；
- 托管 JEV 在本项目网络环境下的端到端延迟与限流；
- 中文语料上的 JEV 判断质量（论文与 OpenJev 基准均为英文）；
- Qdrant 当前镜像版本是否支持只取 sparse 向量的 scroll（本仓库未锁 Qdrant 版本）；
- 真实 KB 的单库文档/chunk 规模（仓库文档无数值），决定 Phase 2 是否必须。

## 9. 与"图检索"的边界（供选型时对齐）

- 如果目标是**多跳证据串联**：SAG2 的"查询期词法关联"就是合适的轻量方案，不需要图数据库。
- 如果目标是**实体/关系图谱、全局摘要、跨文档实体消歧**：那是 GraphRAG/知识图谱范畴，SAG2 不提供；本项目当前也没有实体抽取与图存储，属于另一条更大的技术路线。
- 如果目标是**单纯的排序提升**：Phase 0 的 `rerank: jev` 最划算，不必先做倒排。
