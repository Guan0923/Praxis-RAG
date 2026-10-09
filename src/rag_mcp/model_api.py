"""HTTP-only embedding and reranking clients."""

from __future__ import annotations

import math
from collections.abc import Sequence
from dataclasses import dataclass, field, replace
from urllib.parse import urlsplit

import httpx

from .contracts import Candidate, RagOperationError


class ModelAPIError(RagOperationError):
    """A safe, user-facing model service error without credentials or payloads."""


@dataclass(frozen=True)
class ModelConnection:
    base_url: str = ""
    model: str = ""
    api_key: str = field(default="", repr=False)
    vector_size: int | None = None

    @property
    def configured(self) -> bool:
        return bool(self.base_url and self.model)

    def public(self) -> dict[str, object]:
        return {"base_url": self.base_url, "model": self.model,
                "api_key_set": bool(self.api_key), "vector_size": self.vector_size,
                "configured": self.configured}


def normalize_base_url(value: str) -> str:
    value = value.strip().rstrip("/")
    try:
        parsed = urlsplit(value)
        port = parsed.port
    except ValueError as exc:
        raise ModelAPIError("base_url 地址无效") from exc
    if (parsed.scheme not in {"http", "https"} or not parsed.hostname
            or parsed.username is not None or parsed.password is not None
            or parsed.query or parsed.fragment or any(char.isspace() for char in value)
            or (port is not None and not 1 <= port <= 65535)):
        raise ModelAPIError("base_url 必须是 HTTP 或 HTTPS 地址，不能包含账号、查询参数或片段")
    return value


class ModelHTTPClient:
    def __init__(self, connection: ModelConnection, *, timeout: float = 60) -> None:
        self.connection = connection
        self.timeout = timeout

    def request(self, endpoint: str, body: dict | None = None) -> dict:
        if not self.connection.base_url:
            raise ModelAPIError("尚未配置模型服务地址")
        headers = {"Accept": "application/json"}
        if self.connection.api_key:
            headers["Authorization"] = f"Bearer {self.connection.api_key}"
        try:
            with httpx.Client(timeout=httpx.Timeout(self.timeout, connect=5),
                              follow_redirects=False, trust_env=False) as client:
                response = client.request(
                    "GET" if body is None else "POST",
                    self.connection.base_url + "/" + endpoint, headers=headers, json=body,
                )
            if not 200 <= response.status_code < 300:
                detail = {401: "认证失败，请检查 apikey", 403: "服务拒绝访问",
                          404: "接口或模型不存在，请检查地址和模型名称",
                          429: "请求过于频繁或额度不足，请稍后重试"}.get(response.status_code, "模型服务请求失败")
                raise ModelAPIError(f"HTTP {response.status_code}：{detail}")
            payload = response.json()
        except httpx.TimeoutException as exc:
            raise ModelAPIError("模型服务请求超时") from exc
        except httpx.HTTPError as exc:
            raise ModelAPIError("无法连接模型服务，请检查地址、端口和服务状态") from exc
        except (ValueError, UnicodeError) as exc:
            raise ModelAPIError("模型服务返回了无效 JSON") from exc
        if not isinstance(payload, dict):
            raise ModelAPIError("模型服务响应必须是 JSON 对象")
        return payload

    def models(self) -> list[str]:
        data = self.request("models").get("data")
        if not isinstance(data, list) or any(
            not isinstance(item, dict) or not isinstance(item.get("id"), str) or not item["id"].strip()
            for item in data
        ):
            raise ModelAPIError("模型列表格式无效")
        return list(dict.fromkeys(item["id"] for item in data))


class HTTPEmbedder(ModelHTTPClient):
    def embed_documents(self, texts: Sequence[str]) -> list[list[float]]:
        if not texts:
            return []
        if not self.connection.configured:
            raise ModelAPIError("Embedding 尚未配置，请配置服务地址并为知识库选择模型")
        vectors: list[list[float]] = []
        dimension = self.connection.vector_size
        for start in range(0, len(texts), 32):
            batch = list(texts[start:start + 32])
            data = self.request("embeddings", {"model": self.connection.model, "input": batch}).get("data")
            if not isinstance(data, list) or len(data) != len(batch):
                raise ModelAPIError("Embedding 返回的向量数量与输入不一致")
            ordered: dict[int, list[float]] = {}
            for item in data:
                if not isinstance(item, dict):
                    raise ModelAPIError("Embedding 返回格式无效")
                index, vector = item.get("index"), item.get("embedding")
                if (type(index) is not int or not 0 <= index < len(batch) or index in ordered
                        or not isinstance(vector, list) or not vector
                        or any(type(value) not in (int, float) or not math.isfinite(value) for value in vector)):
                    raise ModelAPIError("Embedding 返回的序号或向量无效")
                dimension = dimension or len(vector)
                if len(vector) != dimension:
                    raise ModelAPIError("Embedding 向量长度与当前索引不一致")
                ordered[index] = [float(value) for value in vector]
            vectors.extend(ordered[index] for index in range(len(batch)))
        return vectors

    def embed_query(self, text: str) -> list[float]:
        return self.embed_documents([text])[0]


class HTTPReranker(ModelHTTPClient):
    def __init__(self, connection: ModelConnection, *, max_candidates: int = 30,
                 max_text_chars: int = 4000, timeout: float = 60) -> None:
        super().__init__(connection, timeout=timeout)
        self.max_candidates = max_candidates
        self.max_text_chars = max_text_chars

    def scores(self, query: str, documents: list[str]) -> list[tuple[int, float]]:
        if not self.connection.configured:
            raise ModelAPIError("Reranker 尚未配置，请配置服务地址并为知识库选择模型")
        data = self.request("rerank", {"model": self.connection.model, "query": query,
                                       "documents": documents, "top_n": len(documents)}).get("results")
        if not isinstance(data, list) or len(data) != len(documents):
            raise ModelAPIError("Reranker 返回的结果数量不完整")
        scores: dict[int, float] = {}
        for item in data:
            if not isinstance(item, dict):
                raise ModelAPIError("Reranker 返回格式无效")
            index, score = item.get("index"), item.get("relevance_score")
            if (type(index) is not int or not 0 <= index < len(documents) or index in scores
                    or type(score) not in (int, float) or not math.isfinite(score)):
                raise ModelAPIError("Reranker 返回的序号或分数无效")
            scores[index] = float(score)
        return sorted(scores.items(), key=lambda item: (-item[1], item[0]))

    def rerank(self, query: str, candidates: list[Candidate]) -> list[Candidate]:
        if not candidates:
            return []
        selected = candidates[:self.max_candidates]
        documents = [(item.title + "\n" + item.text).strip()[:self.max_text_chars] for item in selected]
        scores = self.scores(query, documents)
        return [replace(selected[index], score=score,
                        metadata={**selected[index].metadata, "retrieval_score": round(selected[index].score, 6)})
                for index, score in scores] + candidates[self.max_candidates:]
