"""Praxis-RAG LightRAG sidecar service.

A standalone FastAPI microservice that owns exactly two jobs for a Praxis
knowledge base: build a LightRAG knowledge graph from chunks the Go backend has
already parsed and split, and answer context-only retrieval queries. It never
parses documents, never splits text and never returns generated answer text.

Every Praxis namespace is bound to its own LightRAG workspace (``praxis_<ns>``)
so two knowledge bases can never leak graph data into each other.
"""

from __future__ import annotations

SERVICE_NAME = "praxis-lightrag-service"
__version__ = "0.1.0"

#: PyPI distribution name of the upstream graph/retrieval engine.
LIGHTRAG_PACKAGE = "lightrag-hku"
#: Exact upstream release this sidecar was written against. Changing it requires
#: re-checking the assumptions listed in README.md (SDK method names, chunk-id
#: derivation and the ``aquery_data`` result shape).
LIGHTRAG_PIN = "1.5.7"
#: Prefix of the LightRAG workspace name derived from a Praxis namespace.
WORKSPACE_PREFIX = "praxis_"
