# Agentic RAG 基础

Agentic RAG 把“是否检索、如何改写查询、是否再次检索”的决策交给 Agent。RAG MCP 本身只执行一次检索并返回可引用证据，不负责生成最终回答。

Mini-Agent 通过 `mcp_rag_search` 调用本地 RAG 服务。Phase 2 使用 BGE-M3 生成 1024 维 dense vector，并把文档 Chunk 存入 Qdrant 的 `dense` named vector。

每条结果包含 `document_id`、`chunk_id`、`source`、原文和相似度分数。Agent 应根据证据回答，并在证据不足时改写查询或停止作答。
