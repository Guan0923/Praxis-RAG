"""OpenAI-compatible HTTP bindings handed to LightRAG.

LightRAG calls back into the embedding and chat endpoints of each workspace
binding. This module builds those callbacks and performs the HTTP calls; keeping
them out of the engine adapter makes both files readable and keeps the
credential-bearing closures in one place.

Error messages are fixed strings: transport errors can include the endpoint URL
and upstream errors can include prompt/response bodies, so neither is ever
propagated to the caller or written to the log. The callbacks themselves never
log their inputs.
"""

from __future__ import annotations

import logging
from typing import Any, Awaitable, Callable

from . import SERVICE_NAME
from .config import EmbeddingConfig, LLMConfig
from .errors import EngineUnavailableError

LOGGER = logging.getLogger(SERVICE_NAME)


async def post_json(
    url: str,
    api_key: str,
    payload: dict[str, Any],
    timeout: float,
) -> dict[str, Any]:
    """POST a JSON body to an OpenAI-compatible endpoint.

    Args:
        url: Absolute endpoint URL.
        api_key: Bearer credential; an empty string sends no Authorization
            header (local model servers often ignore auth).
        payload: JSON request body.
        timeout: HTTP deadline in seconds.

    Returns:
        Parsed JSON object.

    Raises:
        EngineUnavailableError: Transport failure, non-2xx status or a body
            that is not a JSON object. The message never contains the URL,
            credentials or response body.
    """

    import httpx

    headers = {"Content-Type": "application/json"}
    if api_key:
        headers["Authorization"] = f"Bearer {api_key}"
    try:
        async with httpx.AsyncClient(timeout=timeout) as client:
            response = await client.post(url, json=payload, headers=headers)
    except Exception as error:
        raise EngineUnavailableError("上游模型服务不可用") from error
    if response.status_code >= 400:
        raise EngineUnavailableError("上游模型服务返回错误")
    try:
        body = response.json()
    except Exception as error:
        raise EngineUnavailableError("上游模型服务返回了无法解析的响应") from error
    if not isinstance(body, dict):
        raise EngineUnavailableError("上游模型服务返回了无法解析的响应")
    return body


def completion_text(body: dict[str, Any]) -> str:
    """Extract assistant message text from an OpenAI-compatible response.

    Args:
        body: Parsed chat-completion response.

    Returns:
        Message content, or an empty string when the shape is unexpected
        (LightRAG tolerates an empty extraction result and retries/warns).
    """

    choices = body.get("choices")
    if not isinstance(choices, list) or not choices:
        return ""
    message = choices[0].get("message") if isinstance(choices[0], dict) else None
    if not isinstance(message, dict):
        return ""
    content = message.get("content")
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts = [
            item.get("text")
            for item in content
            if isinstance(item, dict) and isinstance(item.get("text"), str)
        ]
        return "".join(parts)
    return ""


def build_completion(config: LLMConfig) -> Callable[..., Awaitable[str]]:
    """Build the async chat-completion callable LightRAG invokes.

    LightRAG's convention is
    ``func(prompt, system_prompt=None, history_messages=None, **kwargs)`` and the
    return value must be a ``str``. The sidecar never streams, so a
    ``stream=True`` hint is ignored; ``response_format`` and the deprecated
    ``keyword_extraction`` flag are forwarded so JSON-mode extraction keeps
    working.

    Args:
        config: Endpoint binding for one role (extract or keyword).

    Returns:
        Async callable returning the assistant message text.
    """

    async def complete(
        prompt: str,
        system_prompt: str | None = None,
        history_messages: list[dict[str, Any]] | None = None,
        **kwargs: Any,
    ) -> str:
        """Call the configured chat endpoint once.

        Args:
            prompt: User prompt built by LightRAG; never logged.
            system_prompt: Optional system prompt.
            history_messages: Optional prior messages.
            **kwargs: LightRAG flags such as ``response_format``.

        Returns:
            Assistant message content.

        Raises:
            EngineUnavailableError: The endpoint failed.
        """

        messages: list[dict[str, str]] = []
        if system_prompt:
            messages.append({"role": "system", "content": str(system_prompt)})
        for message in history_messages or []:
            if not isinstance(message, dict):
                continue
            role = message.get("role")
            content = message.get("content")
            if isinstance(role, str) and isinstance(content, str):
                messages.append({"role": role, "content": content})
        messages.append({"role": "user", "content": str(prompt)})

        payload: dict[str, Any] = {"model": config.model, "messages": messages}
        if config.max_output_tokens:
            payload["max_tokens"] = config.max_output_tokens
        response_format = kwargs.get("response_format")
        if isinstance(response_format, dict):
            payload["response_format"] = response_format
        elif kwargs.get("keyword_extraction"):
            # Deprecated upstream flag; JSON object mode replaces it.
            payload["response_format"] = {"type": "json_object"}
        body = await post_json(
            f"{config.base_url.rstrip('/')}/chat/completions",
            config.api_key,
            payload,
            config.timeout_seconds,
        )
        return completion_text(body)

    return complete


def build_embedding(
    config: EmbeddingConfig,
    timeout: float,
) -> Callable[..., Awaitable[Any]]:
    """Build the async embedding callable LightRAG validates and batches.

    Args:
        config: Embedding endpoint binding, including the expected dimension.
        timeout: HTTP deadline for one embedding call.

    Returns:
        Async callable returning a ``float32`` matrix of shape
        ``(len(texts), config.dimension)``; LightRAG requires a NumPy array.
    """

    async def embed(texts: list[str], **kwargs: Any) -> Any:
        """Embed one batch of texts.

        Args:
            texts: Texts to embed.
            **kwargs: Ignored LightRAG hints (``context``, ``max_token_size``).

        Returns:
            NumPy matrix with one row per input text.

        Raises:
            EngineUnavailableError: The endpoint failed or the returned vectors
                do not match the configured dimension/count.
        """

        import numpy as np

        inputs = [str(text) for text in texts]
        if not inputs:
            return np.zeros((0, config.dimension), dtype=np.float32)
        body = await post_json(
            f"{config.base_url.rstrip('/')}/embeddings",
            config.api_key,
            {"model": config.model, "input": inputs},
            timeout,
        )
        data = body.get("data")
        if not isinstance(data, list) or len(data) != len(inputs):
            raise EngineUnavailableError("Embedding 服务返回的向量数量与请求不一致")
        ordered = sorted(
            (entry for entry in data if isinstance(entry, dict)),
            key=lambda entry: entry.get("index", 0),
        )
        vectors: list[list[float]] = []
        for entry in ordered:
            vector = entry.get("embedding")
            if not isinstance(vector, list) or len(vector) != config.dimension:
                raise EngineUnavailableError("Embedding 维度与配置不一致")
            try:
                vectors.append([float(value) for value in vector])
            except (TypeError, ValueError) as error:
                raise EngineUnavailableError("Embedding 向量格式无法识别") from error
        matrix = np.asarray(vectors, dtype=np.float32)
        if matrix.shape != (len(inputs), config.dimension):
            raise EngineUnavailableError("Embedding 维度与配置不一致")
        return matrix

    return embed
