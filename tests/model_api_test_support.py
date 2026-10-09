"""Shared setup for opt-in acceptance against a user-specified model provider."""

import os
from dataclasses import replace

import pytest


def configure_api_runtime(runtime):
    url = os.getenv("RAG_MODEL_PROVIDER_URL")
    if not url:
        pytest.skip("Set RAG_MODEL_PROVIDER_URL to an isolated real inference provider")
    runtime.provider_settings = {kind: {"base_url": url} for kind in ("embedding", "reranker")}
    for kind, default in (("embedding", "BAAI/bge-m3"), ("reranker", "BAAI/bge-reranker-base")):
        runtime.config = replace(runtime.config, **{f"{kind}_model": os.getenv(f"RAG_TEST_{kind.upper()}_MODEL", default)})
