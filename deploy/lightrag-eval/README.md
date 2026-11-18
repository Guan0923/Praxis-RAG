# LightRAG 会话评估（M0）

> 目的：在写任何集成代码之前，用真实语料判断「知识图谱检索」相对现有 `hybrid` / `sag2` 是否带来可感知的增益，并把结论固化成工程默认值（默认 query mode、是否启用、成本上限）。
>
> 本目录只放评估用的操作说明，不参与构建，也不进入 CI。

## 1. 为什么单独评估

Praxis-RAG 的检索契约是「只出证据、不出答案」，四种既有模式各自解决不同问题：

| 模式 | 擅长 | 短板 |
| --- | --- | --- |
| `bm25` | 精确关键词、专有名词 | 同义改写、跨表述 |
| `embedding` | 语义相近片段 | 精确编号、条款号 |
| `hybrid` | 大多数单文档问答 | 事实分散在多份文档时的关联 |
| `sag2` | 词法多跳、术语扩展 | 需要每个查询多次 JEV 调用，成本高 |
| `graph`（待接入） | 跨文档主题、实体关系、全局归纳 | 入库要按块做实体抽取，成本与时长显著上升 |

如果这 30 份法规里，「用户画像」相关的义务其实散在 6 份文档、共 11 个条款，`hybrid` 只会返回其中语义最像的 5 段；`graph` 的 `global`/`mix` 模式能把它们连起来。评估要回答的就是：**这种场景在你的真实语料里占多大比例，值不值得为它付索引成本。**

## 2. 前置条件

- Docker Desktop（或等价的 compose 运行时）
- 一个 OpenAI 兼容的对话模型端点（实体关系抽取 + 关键词抽取）
- 一个 OpenAI 兼容的 Embedding 端点；推荐与本项目一致：`BAAI/bge-m3`，维度 1024
- 本机可用端口 `9621`（LightRAG 默认）与 `8093`（本项目侧车，评估阶段不用）

## 3. 启动官方实例

### 方式一：PyPI 包（最快，自带 WebUI）

```bash
uv tool install "lightrag-hku[api]"
curl -O https://raw.githubusercontent.com/HKUDS/LightRAG/main/env.example
cp env.example .env
# 按下一节修改 .env
lightrag-server
```

### 方式二：Docker Compose

```bash
git clone https://github.com/HKUDS/LightRAG.git
cd LightRAG
cp env.example .env
# 按下一节修改 .env
docker compose up
```

打开 `http://127.0.0.1:9621/webui`。

## 4. 安全基线（必须做，否则等于把 LLM 额度对公网敞开）

官方服务默认监听 `0.0.0.0` 且**默认不开启认证**。评估实例也必须收紧：

```dotenv
HOST=127.0.0.1
LIGHTRAG_API_KEY=<生成一个随机串>
```

生成密钥：`openssl rand -hex 32`（Windows 可用 `pwsh -c "(New-Guid).Guid"` 之类的替代）。

设了 `LIGHTRAG_API_KEY` 之后，所有 `/api/*` 与查询接口都要带 `Authorization: Bearer <key>`。

## 5. 模型与语言配置

```dotenv
# 抽取模型：每个文本块一次调用，必须是「非思考」快模型
LLM_BINDING=openai
LLM_BINDING_HOST=<你的 OpenAI 兼容网关>
LLM_MODEL=<非思考模型，例如某家的 flash / lite / haiku 档>
LLM_BINDING_API_KEY=<key>

# 实体关系摘要语言：跟随语料语言，中文语料必须设
SUMMARY_LANGUAGE=Chinese

# Embedding：与本项目一致，维度必须对上
EMBEDDING_BINDING=openai
EMBEDDING_BINDING_HOST=<你的 embedding 网关>
EMBEDDING_MODEL=BAAI/bge-m3
EMBEDDING_DIM=1024
EMBEDDING_BINDING_API_KEY=<key>
```

要点：

- **抽取模型不要用思考模型**。官方明确说明每块一次抽取调用，思考模式会让索引慢一个数量级并且更贵。
- `EMBEDDING_DIM` 必须等于模型实际输出维度，否则启动或入库直接报错。
- 第一次灌入后**不要再改 embedding 模型**；改了要整体重建向量。
- 先用默认的本地文件存储跑评估即可，不要为评估搭 PostgreSQL/Neo4j。

## 6. 灌入评估语料

建议直接用仓库里的 `examples/knowledge/` 三篇文档（`agentic-rag.md`、`rag-upload-test.md`、`x7-maintenance-manual.md`），再加上你自己的 20~30 份真实业务文档——**增益只在多文档语义关联的场景里出现，单文档语料评估不出差异**。

在 WebUI 的文档管理页上传即可。灌入时观察并记录：

- 单篇文档的处理耗时
- 抽取出的实体数、关系数
- 有没有出现长时间的抽取超时

## 7. 对比问题清单

准备 8 个问题，每类 2 个，同时用本项目的 `hybrid` 和 LightRAG 的 `naive`/`local`/`global`/`mix` 各跑一遍，记录返回内容与耗时。

| 类别 | 问题形态 | 期望胜出者 |
| --- | --- | --- |
| 单点事实 | 「X7 设备的滤芯更换周期是多少」 | hybrid |
| 精确编号 | 「第 3.2.1 条要求什么」 | hybrid / bm25 |
| 跨文档关联 | 「所有涉及用户画像的条款之间是什么关系，分别出自哪些文档」 | graph |
| 主题归纳 | 「这几份文档对数据留存期限的整体立场是什么」 | graph |
| 实体网络 | 「A 系统的上游调用方有哪些，各自又依赖什么」 | graph |
| 否定/例外 | 「哪些情况下不适用上述豁免」 | hybrid + rerank |
| 多跳 | 「触发告警的阈值来自哪份规范，那份规范又被谁引用」 | graph / sag2 |
| 口语化改写 | 「滤芯多久换一次」 | embedding |

每行记录：命中片段数、是否包含全部相关文档、耗时、主观相关性 1~5。

判定标准（建议事先约定，避免事后自我说服）：

- 跨文档关联类问题里，`graph` 有 **≥3/4** 的明显胜出，且 `hybrid` 明确漏掉多份文档 → 值得接入。
- 只有 1~2 题胜出 → 先不接入，把精力放在现有模式的重排上。
- 索引成本超预算 → 只对「开启图谱」的知识库做抽取，其余知识库保持关闭。

## 8. 记录结论

评估结束后把结论写进 `VALIDATION.md` 的对应小节，至少包含：

- 语料规模（文档数、块数）
- `graph` 相对 `hybrid` 的胜负分数
- 索引成本（每千块的抽取调用数与大致时长）
- 最终选定的默认 query mode 与启用策略

这些数字直接决定集成时的默认值；没有它们，默认 mode 只能靠猜。
