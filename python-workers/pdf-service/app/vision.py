"""Remote OpenAI-compatible vision client and prompt text.

The service keeps **no** local model weights: every layout / OCR / table /
formula / picture-description inference is one HTTP chat-completions call to a
remote OpenAI-compatible endpoint. This module owns

* :class:`VisionSettings` — the endpoint/model/timeout/concurrency tuple,
  resolved either from ``options.vision`` (per request, sent by the Go backend)
  or from the ``RAG_VLM_*`` environment fallback;
* :class:`VisionClient` — a thin ``httpx`` wrapper that sends one image plus a
  prompt and returns the message text. A ``httpx.MockTransport`` can be injected
  so the whole test suite runs without network access;
* the prompt texts (page layout / table structure / picture description) and
  :func:`extract_json_object`, which tolerates code fences and surrounding text.

Security: the API key only travels in the ``Authorization`` header of the
request. It is never logged, never stored in the manifest and never echoed in an
error message — callers only get :class:`app.errors.VisionError` text such as
``"vision request failed (HTTP 500)"`` without the upstream body.
"""

from __future__ import annotations

import base64
import json
import os
import re
from dataclasses import dataclass
from typing import Any
from urllib.parse import urlsplit

import httpx

from .errors import ParserConfigError, VisionError

#: Defaults for a per-request ``options.vision`` block. The Go backend sends the
#: same values through ``RAG_VLM_TIMEOUT`` / ``RAG_VLM_CONCURRENCY``.
DEFAULT_TIMEOUT = 120.0
DEFAULT_CONCURRENCY = 4
DEFAULT_MAX_TOKENS = 4096

#: Floor applied to every vision call; a very short timeout would turn a slow
#: vision model into spurious document failures.
TIMEOUT_FLOOR = 30.0

#: Provenance string recorded in ``manifest.parser.prompt_version``.
DEFAULT_PROMPT_VERSION = "vision-v1"

#: Recorded in ``manifest.parser.backend``; the client speaks the OpenAI chat
#: completions shape (see ``_request_payload``).
BACKEND = "openai-compatible"

#: One page, one call: the model returns every block of that page, including
#: OCR text, formula LaTeX, table structure and picture regions.
PAGE_PROMPT = (
    "You are a document layout analysis engine. The input is the image of one PDF page.\n"
    "Return ONLY one JSON object (no markdown, no code fence, no commentary):\n"
    '{"blocks":[{"kind":"text|title|section_header|list_item|caption|footnote|formula|table|picture|code",'
    '"text":"readable text of the block",'
    '"latex":"formula only: LaTeX without the $$ delimiters",'
    '"original_text":"formula only: the raw formula text as printed",'
    '"equation_number":"formula only: number such as 12, otherwise null",'
    '"caption":"picture/table only: printed caption or table title",'
    '"table":{"num_rows":2,"num_cols":2,"headers":["column title"],'
    '"header_rows":[["column title"]],"rows":[["cell text"]]},'
    '"bbox":{"l":0.0,"t":0.0,"r":1.0,"b":1.0}}]}\n'
    "Rules:\n"
    "- Order blocks in reading order (top to bottom, left to right).\n"
    "- bbox uses normalized coordinates in [0,1], origin at the TOP-LEFT corner of the page; "
    "it must frame the whole block.\n"
    "- Transcribe ALL visible text, including scanned pages; keep paragraph line breaks. "
    "Never translate, rewrite, summarise or invent content.\n"
    "- Formulae use kind \"formula\" with a non-empty LaTeX value; tables use kind \"table\" "
    "with the complete row/column structure; figures, charts and photos use kind \"picture\".\n"
    "- Do not output page headers or page footers. If the page has no content return {\"blocks\":[]}."
)

#: Second call for a table whose structure the page call did not return.
TABLE_PROMPT = (
    "You are a table structure recognition engine. The input is the image of one table.\n"
    "Return ONLY one JSON object (no markdown, no code fence, no commentary):\n"
    '{"num_rows":2,"num_cols":2,"headers":["column title"],'
    '"header_rows":[["column title"]],"rows":[["cell text"]]}\n'
    "Rules: len(rows) == num_rows and every row has exactly num_cols cells; use \"\" for an "
    "empty cell; expand merged cells by repeating their text over the covered grid positions; "
    "never invent cell values."
)

#: Picture description prompt (kept semantically identical to the former worker).
DESCRIPTION_PROMPT = (
    "Describe the visible content for document retrieval in 2 to 4 concise sentences. "
    "Preserve visible names, labels and relationships. For a table, summarize its subject only; "
    "never invent cell values. End with a complete sentence."
)


@dataclass(frozen=True)
class VisionSettings:
    """One remote OpenAI-compatible vision endpoint.

    ``timeout``/``concurrency``/``max_tokens`` are normalized on construction:
    the timeout never drops below :data:`TIMEOUT_FLOOR`, the concurrency and
    ``max_tokens`` are at least 1.
    """

    base_url: str
    api_key: str = ""
    model: str = ""
    timeout: float = DEFAULT_TIMEOUT
    concurrency: int = DEFAULT_CONCURRENCY
    max_tokens: int = DEFAULT_MAX_TOKENS

    def __post_init__(self) -> None:
        object.__setattr__(self, "base_url", (self.base_url or "").strip().rstrip("/"))
        object.__setattr__(self, "api_key", (self.api_key or "").strip())
        object.__setattr__(self, "model", (self.model or "").strip())
        object.__setattr__(
            self, "timeout", max(float(self.timeout or DEFAULT_TIMEOUT), TIMEOUT_FLOOR)
        )
        object.__setattr__(self, "concurrency", max(int(self.concurrency or 1), 1))
        object.__setattr__(self, "max_tokens", max(int(self.max_tokens or 1), 1))

    @property
    def configured(self) -> bool:
        """A callable endpoint needs both a base URL and a model name."""
        return bool(self.base_url and self.model)

    @property
    def chat_url(self) -> str:
        """``{base_url}/chat/completions`` with the documented ``/v1`` rules."""
        if self.base_url.endswith("/chat/completions"):
            return self.base_url
        if self.base_url.endswith("/v1"):
            return self.base_url + "/chat/completions"
        return self.base_url + "/v1/chat/completions"

    @property
    def endpoint_host(self) -> str:
        """Host (and port) of the endpoint — safe to record in the manifest."""
        return urlsplit(self.base_url).netloc

    def headers(self) -> dict[str, str]:
        """Request headers; the API key is only present when one is configured."""
        if not self.api_key:
            return {}
        return {"Authorization": f"Bearer {self.api_key}"}

    def describe(self) -> dict[str, object]:
        """Provenance for ``manifest.parser``: model and host, never the key."""
        return {
            "backend": BACKEND,
            "model": self.model,
            "endpoint": self.endpoint_host,
        }


def vision_settings_from_env() -> VisionSettings | None:
    """Read the ``RAG_VLM_*`` service-level fallback.

    Returns ``None`` when neither ``RAG_VLM_BASE_URL`` nor ``RAG_VLM_MODEL`` is
    set. A partial configuration is a :class:`ParserConfigError` instead of a
    silent fallback: the operator clearly intended to configure a remote endpoint.
    """
    base_url = os.environ.get("RAG_VLM_BASE_URL", "").strip()
    model = os.environ.get("RAG_VLM_MODEL", "").strip()
    if not base_url and not model:
        return None
    if not base_url:
        raise ParserConfigError(
            "RAG_VLM_BASE_URL is not configured: set both RAG_VLM_BASE_URL and RAG_VLM_MODEL "
            "for the remote vision endpoint"
        )
    if not model:
        raise ParserConfigError(
            "RAG_VLM_MODEL is not configured: set both RAG_VLM_BASE_URL and RAG_VLM_MODEL "
            "for the remote vision endpoint"
        )
    timeout = _env_number("RAG_VLM_TIMEOUT", DEFAULT_TIMEOUT)
    concurrency = _env_integer("RAG_VLM_CONCURRENCY", DEFAULT_CONCURRENCY)
    max_tokens = _env_integer("RAG_VLM_MAX_TOKENS", DEFAULT_MAX_TOKENS)
    return VisionSettings(
        base_url=base_url,
        api_key=os.environ.get("RAG_VLM_API_KEY", "").strip(),
        model=model,
        timeout=timeout,
        concurrency=concurrency,
        max_tokens=max_tokens,
    )


def _env_number(name: str, default: float) -> float:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return float(raw)
    except ValueError as error:
        raise ParserConfigError(f"{name} must be a number of seconds") from error


def _env_integer(name: str, default: int) -> int:
    raw = os.environ.get(name, "").strip()
    if not raw:
        return default
    try:
        return int(raw)
    except ValueError as error:
        raise ParserConfigError(f"{name} must be an integer") from error


def _request_payload(
    settings: VisionSettings, prompt: str, image_jpeg: bytes | None, max_tokens: int | None
) -> dict[str, Any]:
    """Build the OpenAI chat-completions body (text first, then the image)."""
    content: list[dict[str, Any]] = [{"type": "text", "text": prompt}]
    if image_jpeg:
        encoded = base64.b64encode(image_jpeg).decode("ascii")
        content.append(
            {"type": "image_url", "image_url": {"url": "data:image/jpeg;base64," + encoded}}
        )
    return {
        "model": settings.model,
        "messages": [{"role": "user", "content": content}],
        "temperature": 0,
        "max_tokens": int(max_tokens or settings.max_tokens),
    }


def message_text(data: Any) -> str:
    """Extract ``choices[0].message.content`` as text.

    OpenAI-compatible servers return either a plain string or a list of parts
    such as ``[{"type": "text", "text": "..."}]``; both are accepted.
    """
    try:
        content = data["choices"][0]["message"]["content"]
    except (KeyError, IndexError, TypeError) as error:
        raise VisionError("vision response has no choices[0].message.content") from error
    if isinstance(content, str):
        return content
    if isinstance(content, list):
        parts: list[str] = []
        for part in content:
            if isinstance(part, str):
                parts.append(part)
            elif isinstance(part, dict):
                value = part.get("text")
                if isinstance(value, str):
                    parts.append(value)
        return "".join(parts)
    if isinstance(content, dict) and isinstance(content.get("text"), str):
        return str(content["text"])
    raise VisionError("vision response content is not text")


class VisionClient:
    """One HTTP client for one resolved :class:`VisionSettings`.

    A ``httpx.MockTransport`` (or a pre-built ``httpx.Client``) can be injected,
    which is how the offline test suite exercises the request/response shape.
    The client is safe to share between the page-processing worker threads:
    ``httpx.Client`` is documented as thread-safe.
    """

    def __init__(
        self,
        settings: VisionSettings,
        *,
        transport: httpx.BaseTransport | None = None,
        client: httpx.Client | None = None,
    ) -> None:
        self.settings = settings
        self._owns_client = client is None
        self._client = client or httpx.Client(
            transport=transport,
            timeout=settings.timeout,
            trust_env=False,
        )

    def close(self) -> None:
        if self._owns_client:
            self._client.close()

    def __enter__(self) -> "VisionClient":
        return self

    def __exit__(self, *exc_info: object) -> None:
        self.close()

    def complete(
        self,
        prompt: str,
        image_jpeg: bytes | None = None,
        *,
        max_tokens: int | None = None,
    ) -> str:
        """Run exactly one chat-completions call and return the message text.

        Network failures, non-200 responses and malformed bodies raise
        :class:`VisionError` with a message that never quotes the response body
        or the API key. Retrying is the caller's business
        (:class:`app.parser.VisionSession` retries a page once).
        """
        payload = _request_payload(self.settings, prompt, image_jpeg, max_tokens)
        headers = {"Content-Type": "application/json"}
        headers.update(self.settings.headers())
        try:
            response = self._client.post(self.settings.chat_url, headers=headers, json=payload)
        except httpx.HTTPError as error:
            raise VisionError(f"vision request failed ({type(error).__name__})") from error
        if response.status_code != 200:
            raise VisionError(f"vision request failed (HTTP {response.status_code})")
        try:
            data = response.json()
        except ValueError as error:
            raise VisionError("vision response is not valid JSON") from error
        text = message_text(data).strip()
        if not text:
            raise VisionError("vision response is empty")
        return text

    def complete_json(
        self,
        prompt: str,
        image_jpeg: bytes | None = None,
        *,
        max_tokens: int | None = None,
    ) -> dict[str, Any]:
        """One call whose answer must be a JSON object (see :func:`extract_json_object`)."""
        return extract_json_object(self.complete(prompt, image_jpeg, max_tokens=max_tokens))


_FENCE = re.compile(r"```(?:json)?\s*(.*?)```", re.DOTALL | re.IGNORECASE)


def extract_json_object(text: str) -> dict[str, Any]:
    """Parse the first JSON object out of a model answer.

    Tolerates ```json fences, leading/trailing whitespace and prose around the
    object (the first balanced ``{...}`` wins). Raises :class:`VisionError`
    without echoing the model output.
    """
    candidates: list[str] = []
    for match in _FENCE.finditer(text or ""):
        candidates.append(match.group(1))
    candidates.append(text or "")
    for candidate in candidates:
        fragment = _first_balanced_object(candidate)
        if fragment is None:
            continue
        try:
            value = json.loads(fragment)
        except ValueError:
            continue
        if isinstance(value, dict):
            return value
    raise VisionError("vision response did not contain a JSON object")


def _first_balanced_object(text: str) -> str | None:
    """Return the first balanced ``{...}`` slice of ``text`` (quote aware)."""
    start = text.find("{")
    if start < 0:
        return None
    depth = 0
    in_string = False
    escaped = False
    for index in range(start, len(text)):
        char = text[index]
        if in_string:
            if escaped:
                escaped = False
            elif char == "\\":
                escaped = True
            elif char == '"':
                in_string = False
            continue
        if char == '"':
            in_string = True
        elif char == "{":
            depth += 1
        elif char == "}":
            depth -= 1
            if depth == 0:
                return text[start : index + 1]
    return None
