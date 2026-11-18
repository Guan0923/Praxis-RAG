"""Unit tests for the pinned-upstream mapping helpers.

These tests exercise the code paths that production uses against real LightRAG
payloads, but they feed hand-written dictionaries in the pinned release's shape,
so no upstream import happens here. They also pin the chunk-id derivation the
whole Praxis mapping depends on.
"""

from __future__ import annotations

import asyncio
import hashlib
import importlib.util
from pathlib import Path

import pytest

from app.engine import custom_chunk_key
from app.errors import NotFoundError, ProcessingError
from app.lightrag_engine import LightRAGEngine
from app.upstream_http import completion_text
from app.upstream_query import (
    extract_score,
    iter_entries,
    parse_query_payload,
    split_sources,
)


def test_custom_chunk_key_pins_the_upstream_derivation():
    document_id = "doc-1"
    text = "用户画像包含用户的偏好与历史行为。"
    expected = "chunk-" + hashlib.md5(
        f"{len(document_id)}:{document_id}:{text}".encode("utf-8")
    ).hexdigest()
    assert custom_chunk_key(document_id, text) == expected


def test_parse_query_payload_maps_context_and_evidence():
    payload = {
        "status": "success",
        "data": {
            "chunks": [
                {"chunk_id": "chunk-1", "content": "第一块"},
                {"chunk_id": "chunk-2", "content": "第二块"},
                {"chunk_id": "chunk-1", "content": "重复块被去重"},
                {"chunk_id": "", "content": "缺少 id"},
                {"chunk_id": "chunk-3"},
            ],
            "entities": [
                {
                    "entity_name": "用户画像",
                    "source_id": "chunk-1<SEP>chunk-2",
                }
            ],
            "relationships": [
                {
                    "src_id": "用户画像",
                    "tgt_id": "历史行为",
                    "description": "包含",
                    "source_id": "chunk-1",
                }
            ],
        },
        "metadata": {"query_mode": "mix"},
    }
    result = parse_query_payload(payload, "<SEP>")
    assert [chunk.chunk_id for chunk in result.chunks] == ["chunk-1", "chunk-2"]
    assert result.entities[0].name == "用户画像"
    assert result.entities[0].chunk_ids == ("chunk-1", "chunk-2")
    assert result.relations[0].chunk_ids == ("chunk-1",)
    assert result.relations[0].description == "包含"


def test_parse_query_payload_handles_failure_and_empty_shapes():
    assert parse_query_payload({"status": "failure", "data": {}}, "<SEP>").chunks == ()
    assert parse_query_payload({"status": "success"}, "<SEP>").chunks == ()
    assert parse_query_payload({"status": "success", "data": None}, "<SEP>").chunks == ()


def test_parse_query_payload_rejects_unknown_shapes():
    with pytest.raises(ProcessingError):
        parse_query_payload(["not", "a", "dict"], "<SEP>")
    with pytest.raises(ProcessingError):
        parse_query_payload({"status": "success", "data": []}, "<SEP>")


def test_parse_query_payload_accepts_mapping_collections():
    payload = {
        "status": "success",
        "data": {
            "chunks": [],
            "entities": {"a": {"entity_name": "A"}},
            "relationships": {"b": {"src_id": "A", "tgt_id": "B"}},
        },
    }
    result = parse_query_payload(payload, "<SEP>")
    assert [entity.name for entity in result.entities] == ["A"]
    assert [(relation.source, relation.target) for relation in result.relations] == [("A", "B")]


def test_iter_entries_normalizes_shapes():
    assert iter_entries(None) == []
    assert iter_entries([{"a": 1}, "x"]) == [{"a": 1}]
    assert iter_entries({"k": {"a": 1}}) == [{"a": 1}]
    assert iter_entries("nope") == []


def test_split_sources_uses_separator_and_deduplicates():
    assert split_sources("c-1<SEP>c-2<SEP>c-1", "<SEP>") == ("c-1", "c-2")
    assert split_sources(["c-1", " c-2 "], "<SEP>") == ("c-1", "c-2")
    assert split_sources(None, "<SEP>") == ()
    assert split_sources("c-1, c-2", "<SEP>") == ("c-1, c-2",)


def test_extract_score_prefers_similarity_and_falls_back_to_distance():
    assert extract_score({"score": 0.87}) == 0.87
    assert extract_score({"similarity": 2.5}) == 1.0
    assert extract_score({"distance": 1.0}) == 0.5
    assert extract_score({"score": True}) is None
    assert extract_score({}) is None


def test_completion_text_reads_string_and_multimodal_content():
    assert completion_text(
        {"choices": [{"message": {"content": "ok"}}]}
    ) == "ok"
    assert completion_text(
        {"choices": [{"message": {"content": [{"type": "text", "text": "a"}, {"text": "b"}]}}]}
    ) == "ab"
    assert completion_text({}) == ""
    assert completion_text({"choices": []}) == ""


def test_real_engine_reports_missing_lightrag(tmp_path: Path):
    engine = LightRAGEngine(tmp_path)
    if importlib.util.find_spec("lightrag") is not None:
        pytest.skip("lightrag is installed in this environment")
    assert engine.installed_version() is None
    assert engine.availability_error() == "lightrag-hku 未正确安装：无法导入 lightrag"


def test_engine_query_refuses_unbound_workspace(tmp_path: Path):
    engine = LightRAGEngine(tmp_path)
    with pytest.raises(NotFoundError):
        asyncio.run(engine.query("praxis_legal", "用户画像如何生成", "mix", 10, ()))
