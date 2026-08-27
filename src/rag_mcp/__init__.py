"""Agentic RAG MCP service.

The stable MCP contract supports a deterministic Mock backend and a Qdrant
backend with Dense, BM25 Sparse, RRF Hybrid, and Cross Encoder stages without
changing the public ``search`` tool.
"""

from .orchestrator import RagOrchestrator, build_default_orchestrator

__all__ = ["RagOrchestrator", "build_default_orchestrator"]
